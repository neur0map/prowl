/**
 * Full vector-embedding workflow: pull eligible nodes from KuzuDB,
 * convert to text, embed in batches, store vectors, and build
 * the HNSW index used by semantic search.
 */

import { initEmbedder, embedBatch, embedText, embeddingToArray, isEmbedderReady } from './embedder';
import { prepareBatchTexts, generateEmbeddingText } from './text-generator';
import {
  type EmbeddingProgress,
  type EmbeddingConfig,
  type EmbeddableNode,
  type SemanticSearchResult,
  type ModelProgress,
  DEFAULT_EMBEDDING_CONFIG,
  VECTORIZABLE_TYPES,
} from './types';

/* ── Types ───────────────────────────────────────────── */

export type EmbeddingProgressCallback = (progress: EmbeddingProgress) => void;

/* ── Query generation ────────────────────────────────── */

/* Cypher statement that fetches all nodes of a given label */
function cypherForLabel(nodeLabel: string): string {
  if (nodeLabel === 'File') {
    return `
      MATCH (n:File)
      RETURN n.id AS id, n.name AS name, 'File' AS label,
             n.filePath AS filePath, n.content AS content
    `;
  }
  return `
    MATCH (n:${nodeLabel})
    RETURN n.id AS id, n.name AS name, '${nodeLabel}' AS label,
           n.filePath AS filePath, n.content AS content,
           n.startLine AS startLine, n.endLine AS endLine
  `;
}

/* Convert a row (named or positional) into a typed node */
function parseRow(row: any): EmbeddableNode {
  return {
    id: row.id ?? row[0],
    name: row.name ?? row[1],
    label: row.label ?? row[2],
    filePath: row.filePath ?? row[3],
    content: row.content ?? row[4] ?? '',
    startLine: row.startLine ?? row[5],
    endLine: row.endLine ?? row[6],
  };
}

/* Gather every node that qualifies for vector embedding */
async function gatherEmbeddableNodes(
  executeQuery: (cypher: string) => Promise<any[]>,
): Promise<EmbeddableNode[]> {
  const collected: EmbeddableNode[] = [];

  await Promise.all(
    VECTORIZABLE_TYPES.map(async (label) => {
      try {
        const rows = await executeQuery(cypherForLabel(label));
        for (const row of rows) collected.push(parseRow(row));
      } catch (err) {
        if (import.meta.env.DEV) {
          console.warn(`Fetch for ${label} nodes failed:`, err);
        }
      }
    }),
  );

  return collected;
}

/* ── Persistence helpers ─────────────────────────────── */

/* Write embedding vectors into the CodeEmbedding table */
async function storeVectors(
  executeWithReusedStatement: (
    cypher: string,
    paramsList: Array<Record<string, any>>,
  ) => Promise<void>,
  entries: Array<{ id: string; embedding: number[] }>,
): Promise<void> {
  const stmt = `CREATE (e:CodeEmbedding {nodeId: $nodeId, embedding: $embedding})`;
  const params = entries.map((entry) => ({
    nodeId: entry.id,
    embedding: entry.embedding,
  }));
  await executeWithReusedStatement(stmt, params);
}

/* Build the cosine HNSW index if it does not yet exist */
async function guaranteeHNSWIndex(
  executeQuery: (cypher: string) => Promise<any[]>,
): Promise<void> {
  try {
    await executeQuery(
      `CALL CREATE_VECTOR_INDEX('CodeEmbedding', 'code_embedding_idx', 'embedding', metric := 'cosine')`,
    );
  } catch (err) {
    if (import.meta.env.DEV) {
      console.warn('Vector index creation skipped (may already exist):', err);
    }
  }
}

/* Forward a progress event to the caller */
function notify(
  cb: EmbeddingProgressCallback,
  payload: EmbeddingProgress,
): void {
  cb(payload);
}

/* ── Main pipeline ───────────────────────────────────── */

export async function runEmbeddingPipeline(
  executeQuery: (cypher: string) => Promise<any[]>,
  executeWithReusedStatement: (cypher: string, paramsList: Array<Record<string, any>>) => Promise<void>,
  onProgress: EmbeddingProgressCallback,
  config: Partial<EmbeddingConfig> = {},
): Promise<void> {
  const cfg = { ...DEFAULT_EMBEDDING_CONFIG, ...config };

  try {
    /* Step 1 — load the transformer model */
    notify(onProgress, {
      phase: 'loading-model',
      percent: 0,
      modelDownloadPercent: 0,
    });

    await initEmbedder((mp: ModelProgress) => {
      const pct = mp.progress ?? 0;
      notify(onProgress, {
        phase: 'loading-model',
        percent: Math.round(pct * 0.2),
        modelDownloadPercent: pct,
      });
    }, cfg);

    notify(onProgress, {
      phase: 'loading-model',
      percent: 15,
      modelDownloadPercent: 100,
    });

    /* Warm-up: run a single short inference to trigger WebGPU shader
       compilation *before* entering the batch loop. Capped at 30s —
       if shader compilation hangs (some GPU/driver combos), skip it
       and let the first real batch handle compilation with progress. */
    if (import.meta.env.DEV) {
      console.log('[prowl:embedder] running warm-up inference...');
    }
    try {
      await Promise.race([
        embedText('warm-up'),
        new Promise<never>((_, reject) =>
          setTimeout(() => reject(new Error('warm-up timeout')), 30_000),
        ),
      ]);
      if (import.meta.env.DEV) {
        console.log('[prowl:embedder] warm-up complete');
      }
    } catch (warmUpErr) {
      if (import.meta.env.DEV) {
        console.warn('[prowl:embedder] warm-up skipped:', warmUpErr instanceof Error ? warmUpErr.message : warmUpErr);
      }
    }

    notify(onProgress, {
      phase: 'loading-model',
      percent: 20,
      modelDownloadPercent: 100,
    });

    if (import.meta.env.DEV) {
      console.log('Fetching embeddable nodes...');
    }

    /* Step 2 — pull nodes from graph DB */
    const nodes = await gatherEmbeddableNodes(executeQuery);
    const totalCount = nodes.length;

    if (import.meta.env.DEV) {
      console.log(`Collected ${totalCount} nodes for embedding`);
    }

    if (totalCount === 0) {
      notify(onProgress, {
        phase: 'ready',
        percent: 100,
        nodesProcessed: 0,
        totalNodes: 0,
      });
      return;
    }

    /* Wipe stale vectors before re-persisting */
    try {
      await executeQuery('MATCH (e:CodeEmbedding) DELETE e');
    } catch { /* table may not exist yet */ }

    /* Remove duplicate node IDs (upstream graph can contain repeats) */
    const seen = new Map<string, typeof nodes[0]>();
    for (const nd of nodes) {
      if (!seen.has(nd.id)) seen.set(nd.id, nd);
    }
    const unique = Array.from(seen.values());
    const uniqueCount = unique.length;

    /* Step 3 — batch vectorisation */
    const batchSz = cfg.batchSize;
    const batchCount = Math.ceil(uniqueCount / batchSz);
    let done = 0;

    notify(onProgress, {
      phase: 'embedding',
      percent: 20,
      nodesProcessed: 0,
      totalNodes: uniqueCount,
      currentBatch: 0,
      totalBatches: batchCount,
    });

    for (let b = 0; b < batchCount; b++) {
      const lo = b * batchSz;
      const hi = Math.min(lo + batchSz, uniqueCount);
      const chunk = unique.slice(lo, hi);

      if (import.meta.env.DEV) {
        console.log(`[prowl:embedder] batch ${b + 1}/${batchCount} (${chunk.length} nodes)`);
      }

      const texts = prepareBatchTexts(chunk, cfg);
      const vectors = await embedBatch(texts);

      const entries = chunk.map((nd, idx) => ({
        id: nd.id,
        embedding: embeddingToArray(vectors[idx]),
      }));

      await storeVectors(executeWithReusedStatement, entries);

      done += chunk.length;
      const phasePct = 20 + (done / uniqueCount) * 70;

      notify(onProgress, {
        phase: 'embedding',
        percent: Math.round(phasePct),
        nodesProcessed: done,
        totalNodes: uniqueCount,
        currentBatch: b + 1,
        totalBatches: batchCount,
      });
    }

    /* Step 4 — create HNSW vector index */
    notify(onProgress, {
      phase: 'indexing',
      percent: 90,
      nodesProcessed: uniqueCount,
      totalNodes: uniqueCount,
    });

    if (import.meta.env.DEV) {
      console.log('Building vector index...');
    }

    await guaranteeHNSWIndex(executeQuery);

    notify(onProgress, {
      phase: 'ready',
      percent: 100,
      nodesProcessed: uniqueCount,
      totalNodes: uniqueCount,
    });

    if (import.meta.env.DEV) {
      console.log('Vector pipeline finished');
    }
  } catch (err) {
    const msg = err instanceof Error ? err.message : 'Unknown error';

    if (import.meta.env.DEV) {
      console.error('Vector pipeline failed:', err);
    }

    notify(onProgress, {
      phase: 'error',
      percent: 0,
      error: msg,
    });

    throw err;
  }
}

/* ── Semantic search ─────────────────────────────────── */

/* Query the HNSW index for nearest-neighbour matches */
export async function semanticSearch(
  executeQuery: (cypher: string) => Promise<any[]>,
  query: string,
  k: number = 10,
  maxDistance: number = 0.5,
): Promise<SemanticSearchResult[]> {
  if (!isEmbedderReady()) {
    throw new Error('Vector model unavailable. Execute the embedding pipeline before searching.');
  }

  const qVec = embeddingToArray(await embedText(query));
  const vecLiteral = `[${qVec.join(',')}]`;

  const vectorCypher = `
    CALL QUERY_VECTOR_INDEX('CodeEmbedding', 'code_embedding_idx',
      CAST(${vecLiteral} AS FLOAT[384]), ${k})
    YIELD node AS emb, distance
    WITH emb, distance
    WHERE distance < ${maxDistance}
    RETURN emb.nodeId AS nodeId, distance
    ORDER BY distance
  `;

  const embHits = await executeQuery(vectorCypher);

  if (embHits.length === 0) {
    return [];
  }

  /* Resolve metadata for each matched node */
  const output: SemanticSearchResult[] = [];

  await Promise.all(
    embHits.map(async (hit) => {
      const nid: string = hit.nodeId ?? hit[0];
      const dist: number = hit.distance ?? hit[1];

      const colonPos = nid.indexOf(':');
      const nodeLabel = colonPos > 0 ? nid.substring(0, colonPos) : 'Unknown';
      const escapedId = nid.replace(/'/g, "''");

      try {
        const metaCypher =
          nodeLabel === 'File'
            ? `MATCH (n:File {id: '${escapedId}'}) RETURN n.name AS name, n.filePath AS filePath`
            : `MATCH (n:${nodeLabel} {id: '${escapedId}'}) RETURN n.name AS name, n.filePath AS filePath, n.startLine AS startLine, n.endLine AS endLine`;

        const metaRows = await executeQuery(metaCypher);
        if (metaRows.length > 0) {
          const mr = metaRows[0];
          output.push({
            nodeId: nid,
            name: mr.name ?? mr[0] ?? '',
            label: nodeLabel,
            filePath: mr.filePath ?? mr[1] ?? '',
            distance: dist,
            startLine: nodeLabel !== 'File' ? (mr.startLine ?? mr[2]) : undefined,
            endLine: nodeLabel !== 'File' ? (mr.endLine ?? mr[3]) : undefined,
          });
        }
      } catch {
        /* node table may not exist — skip */
      }
    }),
  );

  /* Re-sort after parallel metadata resolution */
  output.sort((a, b) => a.distance - b.distance);

  return output;
}

/* Flat-format semantic search returning metadata alongside each hit */
export async function semanticSearchWithContext(
  executeQuery: (cypher: string) => Promise<any[]>,
  query: string,
  k: number = 5,
  _hops: number = 1,
): Promise<any[]> {
  const hits = await semanticSearch(executeQuery, query, k, 0.5);

  return hits.map((h) => ({
    matchId: h.nodeId,
    matchName: h.name,
    matchLabel: h.label,
    matchPath: h.filePath,
    distance: h.distance,
    connectedId: null,
    connectedName: null,
    connectedLabel: null,
    relationType: null,
  }));
}
