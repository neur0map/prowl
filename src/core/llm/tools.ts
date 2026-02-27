import { tool } from '@langchain/core/tools';
import { z } from 'zod';
import { WebGPUNotAvailableError, embedText, embeddingToArray, initEmbedder, isEmbedderReady } from '../embeddings/embedder';

/* ── Shared utilities ────────────────────────────────── */

function escapeCypher(raw: string): string {
  return raw.replace(/'/g, "''");
}

function pickField(row: any, arrayIdx: number, objectKey: string): any {
  return Array.isArray(row) ? row[arrayIdx] : row[objectKey];
}

function confPercent(rawConf: number | null | undefined): number {
  return rawConf != null ? Math.round(rawConf * 100) : 100;
}

function formatEdgeArrow(
  entry: { name: string; type: string; confidence?: number },
  dir: 'outbound' | 'inbound',
): string {
  const pct = confPercent(entry.confidence);
  if (dir === 'outbound') {
    return `-[${entry.type} ${pct}%]-> ${entry.name}`;
  }
  return `<-[${entry.type} ${pct}%]- ${entry.name}`;
}

function isTestPath(fp: string): boolean {
  if (!fp) return false;
  const lower = fp.toLowerCase();
  return (
    lower.includes('.test.') ||
    lower.includes('.spec.') ||
    lower.includes('__tests__') ||
    lower.includes('__mocks__') ||
    lower.endsWith('.test.ts') ||
    lower.endsWith('.test.tsx') ||
    lower.endsWith('.spec.ts') ||
    lower.endsWith('.spec.tsx')
  );
}

function regexSafe(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

async function queryConnections(
  nodeIdentifier: string,
  runQuery: (cypher: string) => Promise<any[]>,
): Promise<{ outbound: any[]; inbound: any[] }> {
  const nodeKind = nodeIdentifier.split(':')[0];
  const escaped = escapeCypher(nodeIdentifier);
  const stmt = `
    MATCH (n:${nodeKind} {id: '${escaped}'})
    OPTIONAL MATCH (n)-[r1:CodeEdge]->(dst)
    OPTIONAL MATCH (src)-[r2:CodeEdge]->(n)
    RETURN
      collect(DISTINCT {name: dst.name, type: r1.type, confidence: r1.confidence}) AS outgoing,
      collect(DISTINCT {name: src.name, type: r2.type, confidence: r2.confidence}) AS incoming
    LIMIT 1
  `;
  try {
    const rows = await runQuery(stmt);
    if (rows.length === 0) return { outbound: [], inbound: [] };
    const first = rows[0];
    const rawOut = Array.isArray(first) ? first[0] : (first.outgoing || []);
    const rawIn = Array.isArray(first) ? first[1] : (first.incoming || []);
    return {
      outbound: (rawOut || []).filter((c: any) => c && c.name),
      inbound: (rawIn || []).filter((c: any) => c && c.name),
    };
  } catch {
    return { outbound: [], inbound: [] };
  }
}

async function queryClusterName(
  nodeIdentifier: string,
  runQuery: (cypher: string) => Promise<any[]>,
): Promise<string> {
  const nodeKind = nodeIdentifier.split(':')[0];
  const escaped = escapeCypher(nodeIdentifier);
  const stmt = `
    MATCH (n:${nodeKind} {id: '${escaped}'})
    MATCH (n)-[:CodeEdge {type: 'MEMBER_OF'}]->(c:Community)
    RETURN c.label AS label
    LIMIT 1
  `;
  try {
    const rows = await runQuery(stmt);
    if (rows.length > 0) {
      const val = pickField(rows[0], 0, 'label');
      if (val) return val;
    }
  } catch {
    /* cluster lookup is best-effort */
  }
  return 'Unclustered';
}

async function queryProcessMembership(
  nodeIdentifier: string,
  runQuery: (cypher: string) => Promise<any[]>,
): Promise<Array<{ id: string; label: string; step?: number; stepCount?: number }>> {
  const nodeKind = nodeIdentifier.split(':')[0];
  const escaped = escapeCypher(nodeIdentifier);
  const stmt = `
    MATCH (n:${nodeKind} {id: '${escaped}'})
    MATCH (n)-[r:CodeEdge {type: 'STEP_IN_PROCESS'}]->(p:Process)
    RETURN p.id AS id, p.label AS label, r.step AS step, p.stepCount AS stepCount
    ORDER BY r.step
  `;
  const output: Array<{ id: string; label: string; step?: number; stepCount?: number }> = [];
  try {
    const rows = await runQuery(stmt);
    for (const row of rows) {
      const pid = pickField(row, 0, 'id');
      const pLabel = pickField(row, 1, 'label');
      const pStep = pickField(row, 2, 'step');
      const pStepCount = pickField(row, 3, 'stepCount');
      if (pid && pLabel) {
        output.push({ id: pid, label: pLabel, step: pStep, stepCount: pStepCount });
      }
    }
  } catch {
    /* process lookup is best-effort */
  }
  return output;
}

/* ── Tool factories ──────────────────────────────────── */

function createSearchTool(
  runQuery: (cypher: string) => Promise<any[]>,
  doSemanticSearch: (query: string, k?: number, maxDistance?: number) => Promise<any[]>,
  doHybridSearch: (query: string, k?: number) => Promise<any[]>,
  embeddingAvailable: () => boolean,
  bm25Available: () => boolean,
) {
  type ProcRef = { id: string; label: string; step?: number; stepCount?: number };
  type ResultEntry = {
    rank: number;
    nid: string;
    symbolName: string;
    nodeLabel: string;
    path: string;
    lineRange: string;
    foundVia: string;
    relevance: string;
    connectionText: string;
    cluster: string;
    procs: ProcRef[];
  };

  return tool(
    async ({ query, limit, groupByProcess }: { query: string; limit?: number; groupByProcess?: boolean }) => {
      const maxItems = limit ?? 10;
      const grouped = groupByProcess ?? true;

      let results: any[] = [];

      if (bm25Available()) {
        try {
          results = await doHybridSearch(query, maxItems);
        } catch {
          if (embeddingAvailable()) {
            results = await doSemanticSearch(query, maxItems);
          }
        }
      } else if (embeddingAvailable()) {
        results = await doSemanticSearch(query, maxItems);
      } else {
        return 'Search is not available. Please load a repository first.';
      }

      if (results.length === 0) {
        return `No code found matching "${query}". Try different terms or use grep for exact patterns.`;
      }

      const limited = results.slice(0, maxItems);
      const entries: ResultEntry[] = [];

      for (let pos = 0; pos < limited.length; pos++) {
        const hit = limited[pos];
        const nid = hit.nodeId || hit.id || '';
        const symbolName = hit.name || hit.filePath?.split('/').pop() || 'Unknown';
        const nodeLabel = hit.label || 'File';
        const path = hit.filePath || '';
        const lineRange = hit.startLine ? ` (lines ${hit.startLine}-${hit.endLine})` : '';
        const foundVia = hit.sources?.join('+') || 'hybrid';
        const relevance = hit.score ? ` [score: ${hit.score.toFixed(2)}]` : '';

        let connectionText = '';
        if (nid) {
          const { outbound, inbound } = await queryConnections(nid, runQuery);
          const outSlice = outbound.slice(0, 3).map((c: any) => formatEdgeArrow(c, 'outbound'));
          const inSlice = inbound.slice(0, 3).map((c: any) => formatEdgeArrow(c, 'inbound'));
          const merged = [...outSlice, ...inSlice];
          if (merged.length > 0) {
            connectionText = `\n    Connections: ${merged.join(', ')}`;
          }
        }

        const cluster = nid ? await queryClusterName(nid, runQuery) : 'Unclustered';
        const procs = nid ? await queryProcessMembership(nid, runQuery) : [];

        entries.push({
          rank: pos + 1,
          nid,
          symbolName,
          nodeLabel,
          path,
          lineRange,
          foundVia,
          relevance,
          connectionText,
          cluster,
          procs,
        });
      }

      const renderSingle = (rec: ResultEntry, stepCtx?: ProcRef) => {
        const stepSuffix = stepCtx?.step ? ` (step ${stepCtx.step}/${stepCtx.stepCount ?? '?'})` : '';
        return `[${rec.rank}] ${rec.nodeLabel}: ${rec.symbolName}${rec.relevance}${stepSuffix}\n    ID: ${rec.nid}\n    File: ${rec.path}${rec.lineRange}\n    Cluster: ${rec.cluster}\n    Found by: ${rec.foundVia}${rec.connectionText}`;
      };

      if (!grouped) {
        return `Found ${results.length} matches:\n\n${entries.map(r => renderSingle(r)).join('\n\n')}`;
      }

      const groups = new Map<string, { title: string; totalSteps?: number; items: { rec: ResultEntry; step?: number; stepCount?: number }[] }>();
      const ungroupedKey = '__ungrouped__';

      for (const rec of entries) {
        if (rec.procs.length === 0) {
          if (!groups.has(ungroupedKey)) {
            groups.set(ungroupedKey, { title: 'No process', items: [] });
          }
          groups.get(ungroupedKey)!.items.push({ rec });
        } else {
          for (const p of rec.procs) {
            if (!groups.has(p.id)) {
              groups.set(p.id, { title: p.label, totalSteps: p.stepCount, items: [] });
            }
            groups.get(p.id)!.items.push({ rec, step: p.step, stepCount: p.stepCount });
          }
        }
      }

      const ordered = Array.from(groups.entries())
        .sort((x, y) => y[1].items.length - x[1].items.length);

      const outputParts: string[] = [];
      outputParts.push(`Found ${results.length} matches grouped by process:`);
      outputParts.push('');

      for (const [gid, group] of ordered) {
        const stepNote = group.totalSteps ? `, ${group.totalSteps} steps` : '';
        const heading = gid === ungroupedKey
          ? `NO PROCESS (${group.items.length} matches)`
          : `PROCESS: ${group.title} (${group.items.length} matches${stepNote})`;
        outputParts.push(heading);
        for (const entry of group.items) {
          const ctx = entry.step
            ? { id: gid, label: group.title, step: entry.step, stepCount: entry.stepCount }
            : undefined;
          outputParts.push(renderSingle(entry.rec, ctx));
        }
        outputParts.push('');
      }

      return outputParts.join('\n').trim();
    },
    {
      name: 'search',
      description: 'Keyword + semantic code search. Matches are grouped by process and enriched with cluster context.',
      schema: z.object({
        query: z.string().describe('The concept or keyword to search for (e.g. "authentication middleware", "database pool")'),
        groupByProcess: z.boolean().optional().describe('Organize results by process (default: true)'),
        limit: z.number().optional().describe('Maximum number of results (default: 10)'),
      }),
    },
  );
}

function createCypherTool(
  runQuery: (cypher: string) => Promise<any[]>,
  embeddingAvailable: () => boolean,
) {
  return tool(
    async ({ query, cypher }: { query?: string; cypher: string }) => {
      try {
        let processedCypher = cypher;

        if (cypher.includes('{{QUERY_VECTOR}}')) {
          if (!query) {
            return "Error: Your Cypher contains {{QUERY_VECTOR}} but you didn't provide a 'query' to embed. Add a natural language query.";
          }

          if (!embeddingAvailable()) {
            try {
              await initEmbedder();
            } catch (initErr) {
              if (initErr instanceof WebGPUNotAvailableError) {
                await initEmbedder(undefined, {}, 'wasm');
              } else {
                return 'Embeddings not available. Remove {{QUERY_VECTOR}} and use a non-vector query.';
              }
            }
          }

          const vec = await embedText(query);
          const numArr = embeddingToArray(vec);
          const castExpr = `CAST([${numArr.join(',')}] AS FLOAT[384])`;
          processedCypher = cypher.replace(/\{\{\s*QUERY_VECTOR\s*\}\}/g, castExpr);
        }

        const queryResult = await runQuery(processedCypher);

        if (queryResult.length === 0) {
          return 'Query returned no results.';
        }

        const sampleRow = queryResult[0];
        const cols = (typeof sampleRow === 'object' && !Array.isArray(sampleRow))
          ? Object.keys(sampleRow)
          : [];

        if (cols.length > 0) {
          const headerLine = `| ${cols.join(' | ')} |`;
          const divider = `|${cols.map(() => '---').join('|')}|`;

          const dataRows = queryResult.slice(0, 50).map(row => {
            const cells = cols.map(col => {
              const cellVal = row[col];
              if (cellVal == null) return '';
              if (typeof cellVal === 'object') return JSON.stringify(cellVal);
              const asStr = String(cellVal).replace(/\|/g, '\\|');
              return asStr.length > 60 ? asStr.slice(0, 57) + '...' : asStr;
            });
            return `| ${cells.join(' | ')} |`;
          }).join('\n');

          const overflow = queryResult.length > 50 ? `\n\n_(${queryResult.length - 50} more rows)_` : '';
          return `**${queryResult.length} results:**\n\n${headerLine}\n${divider}\n${dataRows}${overflow}`;
        }

        const fallbackLines = queryResult.slice(0, 50).map((row, idx) =>
          `[${idx + 1}] ${JSON.stringify(row)}`,
        );
        const overflow = queryResult.length > 50 ? `\n... (${queryResult.length - 50} more)` : '';
        return `${queryResult.length} results:\n${fallbackLines.join('\n')}${overflow}`;
      } catch (err) {
        const errText = err instanceof Error ? err.message : String(err);
        return `Cypher error: ${errText}\n\nFix your query. Rules: (1) NEVER return whole nodes — use RETURN f.name, f.filePath (2) WITH + ORDER BY MUST have LIMIT (3) Properties: name, filePath, startLine, endLine, content, isExported. Use LABEL(n) for node type.\n\nExample: MATCH (f:File)-[:CodeEdge {type: 'IMPORTS'}]->(g:File) RETURN f.name, f.filePath, g.name, g.filePath LIMIT 20`;
      }
    },
    {
      name: 'cypher',
      description: `Run a Cypher query directly against the KuzuDB code graph. Use for structural traversals: callers, imports, class hierarchies, custom patterns.

Node tables: File, Folder, Function, Class, Interface, Method, CodeElement
Edge table: CodeEdge (single table, 'type' prop: CONTAINS, DEFINES, IMPORTS, CALLS, EXTENDS, IMPLEMENTS)
Props: name, filePath, startLine, endLine, content, isExported. Use LABEL(n) for node type.

RULES: Never return whole nodes (RETURN f) — always project properties (RETURN f.name, f.filePath).
WITH + ORDER BY must include LIMIT. Example: WITH n, cnt ORDER BY cnt DESC LIMIT 20 RETURN n, cnt

Sample queries:
- Most-connected files: MATCH (f:File)-[r:CodeEdge]-(m) WITH f.name AS name, f.filePath AS fp, COUNT(r) AS c ORDER BY c DESC LIMIT 10 RETURN name, fp, c
- Who calls a function: MATCH (caller:Function)-[:CodeEdge {type: 'CALLS'}]->(fn:Function {name: 'validate'}) RETURN caller.name, caller.filePath
- Inheritance: MATCH (child:Class)-[:CodeEdge {type: 'EXTENDS'}]->(parent:Class) RETURN child.name, parent.name
- Importers of a file: MATCH (f:File)-[:CodeEdge {type: 'IMPORTS'}]->(target:File) WHERE target.name = 'utils.ts' RETURN f.name, f.filePath
- Neighbours of a node: MATCH (n)-[r:CodeEdge]-(m) WHERE n.name = 'MyClass' RETURN m.name, LABEL(m) AS type, r.type LIMIT 20

For combined semantic + graph queries, put {{QUERY_VECTOR}} in the Cypher and supply a 'query' param:
CALL QUERY_VECTOR_INDEX('CodeEmbedding', 'code_embedding_idx', {{QUERY_VECTOR}}, 10) YIELD node AS emb, distance
WITH emb, distance WHERE distance < 0.5
MATCH (n:Function {id: emb.nodeId}) RETURN n.name, n.filePath, distance`,
      schema: z.object({
        cypher: z.string().describe('Cypher statement to run'),
        query: z.string().optional().describe('Natural-language query to embed (needed when cypher includes {{QUERY_VECTOR}})'),
      }),
    },
  );
}

function createGrepTool(
  fileStore: Map<string, string>,
) {
  return tool(
    async ({ pattern, fileFilter, caseSensitive, maxResults }: {
      pattern: string;
      fileFilter?: string;
      caseSensitive?: boolean;
      maxResults?: number;
    }) => {
      try {
        const regexFlags = caseSensitive ? 'g' : 'gi';
        let compiledPattern: RegExp;
        try {
          compiledPattern = new RegExp(pattern, regexFlags);
        } catch (compileErr) {
          return `Invalid regex: ${pattern}. Error: ${compileErr instanceof Error ? compileErr.message : String(compileErr)}`;
        }

        const cap = maxResults ?? 100;
        const hits: Array<{ file: string; line: number; content: string }> = [];

        for (const [fp, body] of fileStore.entries()) {
          if (fileFilter && !fp.toLowerCase().includes(fileFilter.toLowerCase())) {
            continue;
          }

          const sourceLines = body.split('\n');
          for (let lineIdx = 0; lineIdx < sourceLines.length; lineIdx++) {
            if (compiledPattern.test(sourceLines[lineIdx])) {
              hits.push({
                file: fp,
                line: lineIdx + 1,
                content: sourceLines[lineIdx].trim().slice(0, 150),
              });
              if (hits.length >= cap) break;
            }
            compiledPattern.lastIndex = 0;
          }
          if (hits.length >= cap) break;
        }

        if (hits.length === 0) {
          return `No matches for "${pattern}"${fileFilter ? ` in files matching "${fileFilter}"` : ''}`;
        }

        const rendered = hits.map(h => `${h.file}:${h.line}: ${h.content}`).join('\n');
        const capMsg = hits.length >= cap ? `\n\n(Showing first ${cap} results)` : '';

        return `Found ${hits.length} matches:\n\n${rendered}${capMsg}`;
      } catch (ex) {
        return `Grep error: ${ex instanceof Error ? ex.message : String(ex)}`;
      }
    },
    {
      name: 'grep',
      description: 'Regex search across all indexed files. Best for finding literal strings, error messages, TODOs, or specific identifiers.',
      schema: z.object({
        pattern: z.string().describe('Regex to match (e.g. "TODO", "console\\.log", "API_KEY")'),
        fileFilter: z.string().optional().describe('Restrict to files whose path contains this substring (e.g. ".ts", "src/api")'),
        caseSensitive: z.boolean().optional().describe('Match case exactly (default: false)'),
        maxResults: z.number().optional().describe('Result cap (default: 100)'),
      }),
    },
  );
}

function createReadTool(
  fileStore: Map<string, string>,
) {
  return tool(
    async ({ filePath }: { filePath: string }) => {
      const normalizedInput = filePath.replace(/\\/g, '/').toLowerCase();

      let resolvedContent = fileStore.get(filePath);
      let resolvedPath = filePath;

      if (!resolvedContent) {
        const scored: Array<{ p: string; s: number }> = [];

        for (const [candidate] of fileStore.entries()) {
          const normalizedCandidate = candidate.toLowerCase();

          if (normalizedCandidate === normalizedInput) {
            scored.push({ p: candidate, s: 1000 });
          } else if (normalizedCandidate.endsWith(normalizedInput)) {
            scored.push({ p: candidate, s: 100 + (200 - candidate.length) });
          } else {
            const inputParts = normalizedInput.split('/').filter(Boolean);
            const candidateParts = normalizedCandidate.split('/');
            let matchVal = 0;
            let prevIdx = -1;

            for (const seg of inputParts) {
              const found = candidateParts.findIndex((s, i) => i > prevIdx && s.includes(seg));
              if (found > prevIdx) {
                matchVal += 10;
                prevIdx = found;
              }
            }

            if (matchVal >= inputParts.length * 5) {
              scored.push({ p: candidate, s: matchVal });
            }
          }
        }

        scored.sort((a, b) => b.s - a.s);
        if (scored.length > 0) {
          resolvedPath = scored[0].p;
          resolvedContent = fileStore.get(resolvedPath);
        }
      }

      if (!resolvedContent) {
        const baseName = filePath.split('/').pop()?.toLowerCase() || '';
        const suggestions = Array.from(fileStore.keys())
          .filter(k => k.toLowerCase().includes(baseName))
          .slice(0, 5);

        if (suggestions.length > 0) {
          return `File not found: "${filePath}"\n\nDid you mean:\n${suggestions.map(s => `  - ${s}`).join('\n')}`;
        }
        return `File not found: "${filePath}"`;
      }

      const CHAR_LIMIT = 50000;
      const totalLines = resolvedContent.split('\n').length;
      if (resolvedContent.length > CHAR_LIMIT) {
        return `File: ${resolvedPath} (${totalLines} lines, truncated)\n\n${resolvedContent.slice(0, CHAR_LIMIT)}\n\n... [truncated]`;
      }

      return `File: ${resolvedPath} (${totalLines} lines)\n\n${resolvedContent}`;
    },
    {
      name: 'read',
      description: 'Retrieve the full source of a file. Use after search or grep to inspect the actual code.',
      schema: z.object({
        filePath: z.string().describe('Path of the file to read (partial paths like "src/utils.ts" are resolved automatically)'),
      }),
    },
  );
}

function createOverviewTool(
  runQuery: (cypher: string) => Promise<any[]>,
) {
  return tool(
    async () => {
      try {
        /* Run sequentially to avoid concurrent buffer pool pressure in KuzuDB.
           The cross-cluster dep query is rewritten to scan from communities
           inward instead of doing a full CALLS cross-product. */
        const clusterRows = await runQuery(`
          MATCH (c:Community)
          RETURN c.id AS id, c.label AS label, c.cohesion AS cohesion, c.symbolCount AS symbolCount, c.description AS description
          ORDER BY c.symbolCount DESC
          LIMIT 200
        `);

        const processRows = await runQuery(`
          MATCH (p:Process)
          RETURN p.id AS id, p.label AS label, p.processType AS type, p.stepCount AS stepCount, p.communities AS communities
          ORDER BY p.stepCount DESC
          LIMIT 200
        `);

        /* Rewritten: start from communities, walk inward via MEMBER_OF,
           then check for CALLS edges between members of different clusters.
           LIMIT on the inner scan prevents intermediate result explosion. */
        let depRows: any[] = [];
        try {
          depRows = await runQuery(`
            MATCH (c1:Community)<-[:CodeEdge {type: 'MEMBER_OF'}]-(a)
            WITH c1, a LIMIT 500
            MATCH (a)-[:CodeEdge {type: 'CALLS'}]->(b)
            MATCH (b)-[:CodeEdge {type: 'MEMBER_OF'}]->(c2:Community)
            WHERE c1.id <> c2.id
            RETURN c1.label AS \`from\`, c2.label AS \`to\`, COUNT(*) AS calls
            ORDER BY calls DESC
            LIMIT 15
          `);
        } catch {
          /* If still too large, return empty — better than crashing */
        }

        const critRows = await runQuery(`
          MATCH (s)-[r:CodeEdge {type: 'STEP_IN_PROCESS'}]->(p:Process)
          RETURN p.label AS label, COUNT(r) AS steps
          ORDER BY steps DESC
          LIMIT 10
        `);

        const clusterTable = clusterRows.map((row: any) => {
          const lbl = pickField(row, 1, 'label');
          const cnt = pickField(row, 3, 'symbolCount');
          const coh = pickField(row, 2, 'cohesion');
          const desc = pickField(row, 4, 'description');
          const cohStr = coh != null ? Number(coh).toFixed(2) : '';
          return `| ${lbl || ''} | ${cnt ?? ''} | ${cohStr} | ${desc ?? ''} |`;
        });

        const processTable = processRows.map((row: any) => {
          const lbl = pickField(row, 1, 'label');
          const steps = pickField(row, 3, 'stepCount');
          const kind = pickField(row, 2, 'type');
          const comms = pickField(row, 4, 'communities');
          const commCount = Array.isArray(comms) ? comms.length : (comms ? 1 : 0);
          return `| ${lbl || ''} | ${steps ?? ''} | ${kind ?? ''} | ${commCount} |`;
        });

        const depList = depRows.map((row: any) => {
          const src = pickField(row, 0, 'from');
          const dst = pickField(row, 1, 'to');
          const n = pickField(row, 2, 'calls');
          return `- ${src} -> ${dst} (${n} calls)`;
        });

        const critList = critRows.map((row: any) => {
          const lbl = pickField(row, 0, 'label');
          const steps = pickField(row, 1, 'steps');
          return `- ${lbl} (${steps} steps)`;
        });

        return [
          `CLUSTERS (${clusterRows.length} total):`,
          `| Cluster | Symbols | Cohesion | Description |`,
          `| --- | --- | --- | --- |`,
          ...clusterTable,
          ``,
          `PROCESSES (${processRows.length} total):`,
          `| Process | Steps | Type | Clusters |`,
          `| --- | --- | --- | --- |`,
          ...processTable,
          ``,
          `CLUSTER DEPENDENCIES:`,
          ...(depList.length > 0 ? depList : ['- None found']),
          ``,
          `CRITICAL PATHS:`,
          ...(critList.length > 0 ? critList : ['- None found']),
        ].join('\n');
      } catch (err) {
        return `Overview error: ${err instanceof Error ? err.message : String(err)}`;
      }
    },
    {
      name: 'overview',
      description: 'High-level codebase map: lists every cluster and process, plus cross-cluster dependency edges.',
      schema: z.object({}),
    },
  );
}

function createExploreTool(
  runQuery: (cypher: string) => Promise<any[]>,
) {
  async function resolveProcess(nameOrId: string) {
    const escaped = escapeCypher(nameOrId);
    const stmt = `
      MATCH (p:Process)
      WHERE p.id = '${escaped}' OR p.label = '${escaped}'
      RETURN p.id AS id, p.label AS label, p.processType AS type, p.stepCount AS stepCount
      LIMIT 1
    `;
    const rows = await runQuery(stmt);
    return rows.length > 0 ? rows[0] : null;
  }

  async function resolveCommunity(nameOrId: string) {
    const escaped = escapeCypher(nameOrId);
    const stmt = `
      MATCH (c:Community)
      WHERE c.id = '${escaped}' OR c.label = '${escaped}' OR c.heuristicLabel = '${escaped}'
      RETURN c.id AS id, c.label AS label, c.cohesion AS cohesion, c.symbolCount AS symbolCount, c.description AS description
      LIMIT 1
    `;
    const rows = await runQuery(stmt);
    return rows.length > 0 ? rows[0] : null;
  }

  async function resolveSymbol(nameOrId: string) {
    const escaped = escapeCypher(nameOrId);
    const stmt = `
      MATCH (n)
      WHERE n.name = '${escaped}' OR n.id = '${escaped}' OR n.filePath = '${escaped}'
      RETURN n.id AS id, n.name AS name, n.filePath AS filePath, label(n) AS nodeType
      LIMIT 5
    `;
    const rows = await runQuery(stmt);
    return rows.length > 0 ? rows[0] : null;
  }

  async function renderProcessDetail(row: any): Promise<string> {
    const pid = pickField(row, 0, 'id');
    const lbl = pickField(row, 1, 'label');
    const pKind = pickField(row, 2, 'type');
    const totalSteps = pickField(row, 3, 'stepCount');
    const escapedPid = escapeCypher(pid);

    const [stepRows, clusterRows] = await Promise.all([
      runQuery(`
        MATCH (s)-[r:CodeEdge {type: 'STEP_IN_PROCESS'}]->(p:Process {id: '${escapedPid}'})
        RETURN s.name AS name, s.filePath AS filePath, r.step AS step
        ORDER BY r.step
      `),
      runQuery(`
        MATCH (c:Community)<-[:CodeEdge {type: 'MEMBER_OF'}]-(s)
        MATCH (s)-[:CodeEdge {type: 'STEP_IN_PROCESS'}]->(p:Process {id: '${escapedPid}'})
        RETURN DISTINCT c.id AS id, c.label AS label, c.description AS description
        ORDER BY c.label
        LIMIT 20
      `),
    ]);

    const stepLines = stepRows.map((r: any) => {
      const nm = pickField(r, 0, 'name');
      const fp = pickField(r, 1, 'filePath');
      const st = pickField(r, 2, 'step');
      return `- ${st}. ${nm} (${fp || 'n/a'})`;
    });

    const clusterLines = clusterRows.map((r: any) => {
      const cl = pickField(r, 1, 'label');
      const cd = pickField(r, 2, 'description');
      return `- ${cl}${cd ? ` — ${cd}` : ''}`;
    });

    return [
      `PROCESS: ${lbl}`,
      `Type: ${pKind || 'n/a'}`,
      `Steps: ${totalSteps ?? stepRows.length}`,
      ``,
      `STEPS:`,
      ...(stepLines.length > 0 ? stepLines : ['- None found']),
      ``,
      `CLUSTERS TOUCHED:`,
      ...(clusterLines.length > 0 ? clusterLines : ['- None found']),
    ].join('\n');
  }

  async function renderClusterDetail(row: any): Promise<string> {
    const cid = pickField(row, 0, 'id');
    const lbl = pickField(row, 1, 'label');
    const coh = pickField(row, 2, 'cohesion');
    const symCount = pickField(row, 3, 'symbolCount');
    const desc = pickField(row, 4, 'description');
    const escapedCid = escapeCypher(cid);

    const [memberRows, procRows] = await Promise.all([
      runQuery(`
        MATCH (c:Community {id: '${escapedCid}'})<-[:CodeEdge {type: 'MEMBER_OF'}]-(m)
        RETURN m.name AS name, m.filePath AS filePath, label(m) AS nodeType
        LIMIT 50
      `),
      runQuery(`
        MATCH (c:Community {id: '${escapedCid}'})<-[:CodeEdge {type: 'MEMBER_OF'}]-(s)
        MATCH (s)-[:CodeEdge {type: 'STEP_IN_PROCESS'}]->(p:Process)
        RETURN DISTINCT p.id AS id, p.label AS label, p.stepCount AS stepCount
        ORDER BY p.stepCount DESC
        LIMIT 20
      `),
    ]);

    const memberLines = memberRows.map((r: any) => {
      const nm = pickField(r, 0, 'name');
      const fp = pickField(r, 1, 'filePath');
      const nt = pickField(r, 2, 'nodeType');
      return `- ${nt}: ${nm} (${fp || 'n/a'})`;
    });

    const procLines = procRows.map((r: any) => {
      const pl = pickField(r, 1, 'label');
      const ps = pickField(r, 2, 'stepCount');
      return `- ${pl} (${ps} steps)`;
    });

    return [
      `CLUSTER: ${lbl}`,
      `Symbols: ${symCount ?? memberRows.length}`,
      `Cohesion: ${coh != null ? Number(coh).toFixed(2) : 'n/a'}`,
      `Description: ${desc || 'n/a'}`,
      ``,
      `TOP MEMBERS:`,
      ...(memberLines.length > 0 ? memberLines : ['- None found']),
      ``,
      `PROCESSES TOUCHING THIS CLUSTER:`,
      ...(procLines.length > 0 ? procLines : ['- None found']),
    ].join('\n');
  }

  async function renderSymbolDetail(row: any): Promise<string> {
    const nid = pickField(row, 0, 'id');
    const nm = pickField(row, 1, 'name');
    const fp = pickField(row, 2, 'filePath');
    const nt = pickField(row, 3, 'nodeType');
    const escapedNid = escapeCypher(String(nid));

    const [clusterRows, procRows, connRows] = await Promise.all([
      runQuery(`
        MATCH (n:${nt} {id: '${escapedNid}'})
        MATCH (n)-[:CodeEdge {type: 'MEMBER_OF'}]->(c:Community)
        RETURN c.label AS label, c.description AS description
        LIMIT 1
      `),
      runQuery(`
        MATCH (n:${nt} {id: '${escapedNid}'})
        MATCH (n)-[r:CodeEdge {type: 'STEP_IN_PROCESS'}]->(p:Process)
        RETURN p.label AS label, r.step AS step, p.stepCount AS stepCount
        ORDER BY r.step
      `),
      runQuery(`
        MATCH (n:${nt} {id: '${escapedNid}'})
        OPTIONAL MATCH (n)-[r1:CodeEdge]->(dst)
        OPTIONAL MATCH (src)-[r2:CodeEdge]->(n)
        RETURN
          collect(DISTINCT {name: dst.name, type: r1.type, confidence: r1.confidence}) AS outgoing,
          collect(DISTINCT {name: src.name, type: r2.type, confidence: r2.confidence}) AS incoming
        LIMIT 1
      `),
    ]);

    const clusterLbl = clusterRows.length > 0 ? pickField(clusterRows[0], 0, 'label') : 'Unclustered';
    const clusterDesc = clusterRows.length > 0 ? pickField(clusterRows[0], 1, 'description') : '';

    const procLines = procRows.map((r: any) => {
      const pl = pickField(r, 0, 'label');
      const ps = pickField(r, 1, 'step');
      const psc = pickField(r, 2, 'stepCount');
      return `- ${pl} (step ${ps}/${psc ?? '?'})`;
    });

    let connText = 'None';
    if (connRows.length > 0) {
      const cr = connRows[0];
      const rawOut = Array.isArray(cr) ? cr[0] : (cr.outgoing || []);
      const rawIn = Array.isArray(cr) ? cr[1] : (cr.incoming || []);
      const outFiltered = (rawOut || []).filter((c: any) => c && c.name).slice(0, 5);
      const inFiltered = (rawIn || []).filter((c: any) => c && c.name).slice(0, 5);
      const outArrows = outFiltered.map((c: any) => formatEdgeArrow(c, 'outbound'));
      const inArrows = inFiltered.map((c: any) => formatEdgeArrow(c, 'inbound'));
      if (outArrows.length || inArrows.length) {
        connText = [...outArrows, ...inArrows].join(', ');
      }
    }

    return [
      `SYMBOL: ${nt} ${nm}`,
      `ID: ${nid}`,
      `File: ${fp || 'n/a'}`,
      `Cluster: ${clusterLbl}${clusterDesc ? ` — ${clusterDesc}` : ''}`,
      ``,
      `PROCESSES:`,
      ...(procLines.length > 0 ? procLines : ['- None found']),
      ``,
      `CONNECTIONS:`,
      connText,
    ].join('\n');
  }

  return tool(
    async ({ target, type }: { target: string; type?: 'symbol' | 'cluster' | 'process' | null }) => {
      let resolvedKind = type ?? null;
      let matchedRow: any = null;

      if (!resolvedKind || resolvedKind === 'process') {
        const found = await resolveProcess(target);
        if (found) { matchedRow = found; resolvedKind = 'process'; }
      }

      if (!resolvedKind || resolvedKind === 'cluster') {
        const found = await resolveCommunity(target);
        if (found) { matchedRow = found; resolvedKind = 'cluster'; }
      }

      if (!resolvedKind || resolvedKind === 'symbol') {
        const found = await resolveSymbol(target);
        if (found) { matchedRow = found; resolvedKind = 'symbol'; }
      }

      if (!resolvedKind || !matchedRow) {
        return `Could not find "${target}" as a symbol, cluster, or process. Try search first.`;
      }

      if (resolvedKind === 'process') return renderProcessDetail(matchedRow);
      if (resolvedKind === 'cluster') return renderClusterDetail(matchedRow);
      if (resolvedKind === 'symbol') return renderSymbolDetail(matchedRow);

      return `Unable to explore "${target}".`;
    },
    {
      name: 'explore',
      description: 'Drill into a specific symbol, cluster, or process. Returns members, process participation, and connection details.',
      schema: z.object({
        target: z.string().describe('Name or ID of the symbol, cluster, or process to inspect'),
        type: z.enum(['symbol', 'cluster', 'process']).optional().describe('Target kind hint (auto-detected when omitted)'),
      }),
    },
  );
}

function createImpactTool(
  runQuery: (cypher: string) => Promise<any[]>,
  fileStore: Map<string, string>,
) {
  interface AffectedNode {
    id: string;
    name: string;
    nodeType: string;
    filePath: string;
    startLine?: number;
    edgeType: string;
    confidence: number;
    reason: string;
  }

  function parseAffectedRow(row: any): AffectedNode {
    return {
      id: Array.isArray(row) ? row[0] : row.id,
      name: Array.isArray(row) ? row[1] : row.name,
      nodeType: Array.isArray(row) ? row[2] : row.nodeType,
      filePath: Array.isArray(row) ? row[3] : row.filePath,
      startLine: Array.isArray(row) ? row[4] : row.startLine,
      edgeType: Array.isArray(row) ? row[5] : row.edgeType || 'CALLS',
      confidence: Array.isArray(row) ? row[6] : row.confidence ?? 1.0,
      reason: Array.isArray(row) ? row[7] : row.reason || '',
    };
  }

  function formatAffectedNode(n: AffectedNode): string {
    const shortFile = n.filePath?.split('/').pop() || '';
    const loc = n.startLine ? `${shortFile}:${n.startLine}` : shortFile;
    const pct = Math.round((n.confidence ?? 1) * 100);
    const fuzzy = pct < 80 ? '[fuzzy]' : '';
    return `  ${n.nodeType}|${n.name}|${loc}|${n.edgeType}|${pct}%${fuzzy}`;
  }

  function extractSnippet(n: AffectedNode): string | null {
    if (!n.filePath || !n.startLine) return null;
    const normalized = n.filePath.replace(/\\/g, '/');
    let body: string | undefined;
    for (const [key, val] of fileStore.entries()) {
      const nk = key.replace(/\\/g, '/');
      if (nk === normalized || nk.endsWith(normalized) || normalized.endsWith(nk)) {
        body = val;
        break;
      }
    }
    if (!body) return null;
    const allLines = body.split('\n');
    const idx = n.startLine - 1;
    if (idx < 0 || idx >= allLines.length) return null;
    let line = allLines[idx].trim();
    if (line.length > 80) line = line.slice(0, 77) + '...';
    return line;
  }

  function computeRiskLevel(directCnt: number, procCnt: number, clusterCnt: number, totalCnt: number): string {
    if (directCnt >= 30 || procCnt >= 5 || clusterCnt >= 5 || totalCnt >= 200) return 'CRITICAL';
    if (directCnt >= 15 || procCnt >= 3 || clusterCnt >= 3 || totalCnt >= 100) return 'HIGH';
    if (directCnt >= 5 || totalCnt >= 30) return 'MEDIUM';
    return 'LOW';
  }

  function buildDepthQuery(
    targetId: string,
    targetFilePath: string,
    target: string,
    isFile: boolean,
    dir: 'upstream' | 'downstream',
    relFilter: string,
    minConf: number,
    hopDepth: number,
  ): string {
    const escapedId = escapeCypher(targetId);
    const escapedPath = escapeCypher(targetFilePath || target);

    if (hopDepth === 1) {
      if (dir === 'upstream') {
        return isFile
          ? `
            MATCH (affected)-[r:CodeEdge]->(callee)
            WHERE callee.filePath = '${escapedPath}'
              AND r.type IN [${relFilter}]
              AND affected.filePath <> callee.filePath
              AND (r.confidence IS NULL OR r.confidence >= ${minConf})
            RETURN DISTINCT
              affected.id AS id, affected.name AS name, label(affected) AS nodeType,
              affected.filePath AS filePath, affected.startLine AS startLine,
              1 AS depth, r.type AS edgeType, r.confidence AS confidence, r.reason AS reason
            LIMIT 300
          `
          : `
            MATCH (target {id: '${escapedId}'})
            MATCH (affected)-[r:CodeEdge]->(target)
            WHERE r.type IN [${relFilter}]
              AND (r.confidence IS NULL OR r.confidence >= ${minConf})
            RETURN DISTINCT
              affected.id AS id, affected.name AS name, label(affected) AS nodeType,
              affected.filePath AS filePath, affected.startLine AS startLine,
              1 AS depth, r.type AS edgeType, r.confidence AS confidence, r.reason AS reason
            LIMIT 300
          `;
      } else {
        return isFile
          ? `
            MATCH (caller)-[r:CodeEdge]->(affected)
            WHERE caller.filePath = '${escapedPath}'
              AND r.type IN [${relFilter}]
              AND caller.filePath <> affected.filePath
              AND (r.confidence IS NULL OR r.confidence >= ${minConf})
            RETURN DISTINCT
              affected.id AS id, affected.name AS name, label(affected) AS nodeType,
              affected.filePath AS filePath, affected.startLine AS startLine,
              1 AS depth, r.type AS edgeType, r.confidence AS confidence, r.reason AS reason
            LIMIT 300
          `
          : `
            MATCH (target {id: '${escapedId}'})
            MATCH (target)-[r:CodeEdge]->(affected)
            WHERE r.type IN [${relFilter}]
              AND (r.confidence IS NULL OR r.confidence >= ${minConf})
            RETURN DISTINCT
              affected.id AS id, affected.name AS name, label(affected) AS nodeType,
              affected.filePath AS filePath, affected.startLine AS startLine,
              1 AS depth, r.type AS edgeType, r.confidence AS confidence, r.reason AS reason
            LIMIT 300
          `;
      }
    }

    if (hopDepth === 2) {
      const hopLimit = 200;
      return dir === 'upstream'
        ? `
          MATCH (target {id: '${escapedId}'})
          MATCH (a)-[r1:CodeEdge]->(target)
          MATCH (affected)-[r2:CodeEdge]->(a)
          WHERE r1.type IN [${relFilter}] AND r2.type IN [${relFilter}]
            AND affected.id <> target.id
            AND (r1.confidence IS NULL OR r1.confidence >= ${minConf})
            AND (r2.confidence IS NULL OR r2.confidence >= ${minConf})
          RETURN DISTINCT
            affected.id AS id, affected.name AS name, label(affected) AS nodeType,
            affected.filePath AS filePath, affected.startLine AS startLine,
            2 AS depth, r2.type AS edgeType, r2.confidence AS confidence, r2.reason AS reason
          LIMIT ${hopLimit}
        `
        : `
          MATCH (target {id: '${escapedId}'})
          MATCH (target)-[r1:CodeEdge]->(a)
          MATCH (a)-[r2:CodeEdge]->(affected)
          WHERE r1.type IN [${relFilter}] AND r2.type IN [${relFilter}]
            AND affected.id <> target.id
            AND (r1.confidence IS NULL OR r1.confidence >= ${minConf})
            AND (r2.confidence IS NULL OR r2.confidence >= ${minConf})
          RETURN DISTINCT
            affected.id AS id, affected.name AS name, label(affected) AS nodeType,
            affected.filePath AS filePath, affected.startLine AS startLine,
            2 AS depth, r2.type AS edgeType, r2.confidence AS confidence, r2.reason AS reason
          LIMIT ${hopLimit}
        `;
    }

    const hopLimit = 100;
    return dir === 'upstream'
      ? `
        MATCH (target {id: '${escapedId}'})
        MATCH (a)-[r1:CodeEdge]->(target)
        MATCH (b)-[r2:CodeEdge]->(a)
        MATCH (affected)-[r3:CodeEdge]->(b)
        WHERE r1.type IN [${relFilter}] AND r2.type IN [${relFilter}] AND r3.type IN [${relFilter}]
          AND affected.id <> target.id AND affected.id <> a.id
          AND (r1.confidence IS NULL OR r1.confidence >= ${minConf})
          AND (r2.confidence IS NULL OR r2.confidence >= ${minConf})
          AND (r3.confidence IS NULL OR r3.confidence >= ${minConf})
        RETURN DISTINCT
          affected.id AS id, affected.name AS name, label(affected) AS nodeType,
          affected.filePath AS filePath, affected.startLine AS startLine,
          3 AS depth, r3.type AS edgeType, r3.confidence AS confidence, r3.reason AS reason
        LIMIT ${hopLimit}
      `
      : `
        MATCH (target {id: '${escapedId}'})
        MATCH (target)-[r1:CodeEdge]->(a)
        MATCH (a)-[r2:CodeEdge]->(b)
        MATCH (b)-[r3:CodeEdge]->(affected)
        WHERE r1.type IN [${relFilter}] AND r2.type IN [${relFilter}] AND r3.type IN [${relFilter}]
          AND affected.id <> target.id AND affected.id <> a.id
          AND (r1.confidence IS NULL OR r1.confidence >= ${minConf})
          AND (r2.confidence IS NULL OR r2.confidence >= ${minConf})
          AND (r3.confidence IS NULL OR r3.confidence >= ${minConf})
        RETURN DISTINCT
          affected.id AS id, affected.name AS name, label(affected) AS nodeType,
          affected.filePath AS filePath, affected.startLine AS startLine,
          3 AS depth, r3.type AS edgeType, r3.confidence AS confidence, r3.reason AS reason
        LIMIT ${hopLimit}
      `;
  }

  return tool(
    async ({ target, direction, maxDepth, relationTypes, includeTests, minConfidence }: {
      target: string;
      direction: 'upstream' | 'downstream';
      maxDepth?: number;
      relationTypes?: string[];
      includeTests?: boolean;
      minConfidence?: number;
    }) => {
      const traversalDepth = Math.min(maxDepth ?? 3, 10);
      const showTestFiles = includeTests ?? false;
      const confThreshold = minConfidence ?? 0.7;

      const usageRelTypes = ['CALLS', 'IMPORTS', 'EXTENDS', 'IMPLEMENTS'];
      const activeTypes = (relationTypes && relationTypes.length > 0) ? relationTypes : usageRelTypes;
      const relFilterExpr = activeTypes.map(t => `'${t}'`).join(', ');

      const isPathLookup = target.includes('/');
      const escapedTarget = escapeCypher(target);

      const findStmt = isPathLookup
        ? `MATCH (n) WHERE n.filePath IS NOT NULL AND n.filePath CONTAINS '${escapedTarget}' RETURN n.id AS id, label(n) AS nodeType, n.filePath AS filePath LIMIT 10`
        : `MATCH (n) WHERE n.name = '${escapedTarget}' RETURN n.id AS id, label(n) AS nodeType, n.filePath AS filePath LIMIT 10`;

      let foundNodes;
      try {
        foundNodes = await runQuery(findStmt);
      } catch (lookupErr) {
        return `Error finding target "${target}": ${lookupErr}`;
      }

      if (!foundNodes || foundNodes.length === 0) {
        return `Could not find "${target}" in the codebase. Try using the search tool first to find the exact name.`;
      }

      const allFilePaths = foundNodes.map((r: any) => pickField(r, 2, 'filePath')).filter(Boolean);

      if (foundNodes.length > 1 && !target.includes('/')) {
        return `\u26a0\ufe0f AMBIGUOUS TARGET: Multiple files named "${target}" found:\n\n${allFilePaths.map((p: string, i: number) => `${i + 1}. ${p}`).join('\n')}\n\nPlease specify which file you mean by using a more specific path, e.g.:\n- impact("${allFilePaths[0].split('/').slice(-3).join('/')}")\n- impact("${allFilePaths[1]?.split('/').slice(-3).join('/') || allFilePaths[0]}")`;
      }

      let chosen = foundNodes[0];
      if (target.includes('/') && foundNodes.length > 1) {
        const exact = foundNodes.find((r: any) => {
          const rp = pickField(r, 2, 'filePath');
          return rp && rp.toLowerCase().includes(target.toLowerCase());
        });
        if (exact) {
          chosen = exact;
        } else {
          return `\u26a0\ufe0f AMBIGUOUS TARGET: Could not uniquely match "${target}". Found:\n\n${allFilePaths.map((p: string, i: number) => `${i + 1}. ${p}`).join('\n')}\n\nPlease use a more specific path.`;
        }
      }

      const chosenId = pickField(chosen, 0, 'id');
      const chosenType = pickField(chosen, 1, 'nodeType');
      const chosenPath = pickField(chosen, 2, 'filePath');
      const isFileNode = chosenType === 'File';

      const multipleMatchWarning = '';

      const depthPromises: Promise<any[]>[] = [];

      depthPromises.push(
        runQuery(buildDepthQuery(chosenId, chosenPath, target, isFileNode, direction, relFilterExpr, confThreshold, 1))
          .catch(e => { if (import.meta.env.DEV) console.warn('Impact d=1 query failed:', e); return []; }),
      );

      if (traversalDepth >= 2) {
        depthPromises.push(
          runQuery(buildDepthQuery(chosenId, chosenPath, target, isFileNode, direction, relFilterExpr, confThreshold, 2))
            .catch(e => { if (import.meta.env.DEV) console.warn('Impact d=2 query failed:', e); return []; }),
        );
      }

      if (traversalDepth >= 3) {
        depthPromises.push(
          runQuery(buildDepthQuery(chosenId, chosenPath, target, isFileNode, direction, relFilterExpr, confThreshold, 3))
            .catch(e => { if (import.meta.env.DEV) console.warn('Impact d=3 query failed:', e); return []; }),
        );
      }

      const allDepthResults = await Promise.all(depthPromises);

      const depthBuckets = new Map<number, AffectedNode[]>();
      const collectedIds: string[] = [];
      const visitedIds = new Set<string>();

      allDepthResults.forEach((rows, depthIdx) => {
        const d = depthIdx + 1;
        for (const row of rows) {
          const parsed = parseAffectedRow(row);
          if (!showTestFiles && isTestPath(parsed.filePath)) continue;
          if (parsed.id && !visitedIds.has(parsed.id)) {
            visitedIds.add(parsed.id);
            if (!depthBuckets.has(d)) depthBuckets.set(d, []);
            depthBuckets.get(d)!.push(parsed);
            collectedIds.push(parsed.id);
          }
        }
      });

      const totalHits = collectedIds.length;

      if (totalHits === 0) {
        if (isFileNode) {
          const targetFileName = (chosenPath || target).split('/').pop() || target;
          const baseName = targetFileName.replace(/\.[^/.]+$/, '');
          const refPattern = new RegExp(`\\b${regexSafe(baseName)}\\b`, 'g');
          const textHints: Array<{ file: string; line: number; content: string }> = [];
          const hintCap = 15;

          for (const [fp, body] of fileStore.entries()) {
            if (fp === chosenPath) continue;
            const bodyLines = body.split('\n');
            for (let li = 0; li < bodyLines.length; li++) {
              if (refPattern.test(bodyLines[li])) {
                textHints.push({
                  file: fp,
                  line: li + 1,
                  content: bodyLines[li].trim().slice(0, 150),
                });
                if (textHints.length >= hintCap) break;
              }
              refPattern.lastIndex = 0;
            }
            if (textHints.length >= hintCap) break;
          }

          if (textHints.length > 0) {
            const rendered = textHints.map(h => `${h.file}:${h.line}: ${h.content}`).join('\n');
            return `No ${direction} dependencies found for "${target}" (types: ${activeTypes.join(', ')}), but textual references were detected (graph may be incomplete):\n\n${rendered}${multipleMatchWarning}`;
          }
        }

        return `No ${direction} dependencies found for "${target}" (types: ${activeTypes.join(', ')}). This code appears to be ${direction === 'upstream' ? 'unused (not called by anything)' : 'self-contained (no outgoing dependencies)'}.${multipleMatchWarning}`;
      }

      const d1Nodes = depthBuckets.get(1) || [];
      const d2Nodes = depthBuckets.get(2) || [];
      const d3Nodes = depthBuckets.get(3) || [];

      const confBuckets = { high: 0, medium: 0, low: 0 };
      for (const nodes of depthBuckets.values()) {
        for (const n of nodes) {
          const c = n.confidence ?? 1;
          if (c >= 0.9) confBuckets.high++;
          else if (c >= 0.8) confBuckets.medium++;
          else confBuckets.low++;
        }
      }

      const ctxIdLimit = 500;
      const ctxIds = collectedIds.slice(0, ctxIdLimit);
      const idListExpr = ctxIds.map(id => `'${escapeCypher(id)}'`).join(', ');
      let impactedProcesses: Array<{ label: string; hits: number; minStep: number | null; stepCount: number | null }> = [];
      let impactedClusters: Array<{ label: string; hits: number; impact: string }> = [];

      if (ctxIds.length > 0) {
        const d1IdExpr = d1Nodes.map(n => `'${escapeCypher(n.id)}'`).join(', ');

        const [procRows, clusterRows, directClusterRows] = await Promise.all([
          runQuery(`
            MATCH (s)-[r:CodeEdge {type: 'STEP_IN_PROCESS'}]->(p:Process)
            WHERE s.id IN [${idListExpr}]
            RETURN p.label AS label, COUNT(DISTINCT s.id) AS hits, MIN(r.step) AS minStep, p.stepCount AS stepCount
            ORDER BY hits DESC
            LIMIT 20
          `),
          runQuery(`
            MATCH (s)-[:CodeEdge {type: 'MEMBER_OF'}]->(c:Community)
            WHERE s.id IN [${idListExpr}]
            RETURN c.label AS label, COUNT(DISTINCT s.id) AS hits
            ORDER BY hits DESC
            LIMIT 20
          `),
          d1Nodes.length > 0
            ? runQuery(`
                MATCH (s)-[:CodeEdge {type: 'MEMBER_OF'}]->(c:Community)
                WHERE s.id IN [${d1IdExpr}]
                RETURN DISTINCT c.label AS label
              `)
            : Promise.resolve([]),
        ]);

        const directClusterNames = new Set<string>();
        directClusterRows.forEach((r: any) => {
          const lbl = pickField(r, 0, 'label');
          if (lbl) directClusterNames.add(lbl);
        });

        impactedProcesses = procRows.map((r: any) => ({
          label: pickField(r, 0, 'label'),
          hits: pickField(r, 1, 'hits'),
          minStep: pickField(r, 2, 'minStep'),
          stepCount: pickField(r, 3, 'stepCount'),
        }));

        impactedClusters = clusterRows.map((r: any) => {
          const lbl = pickField(r, 0, 'label');
          const hits = pickField(r, 1, 'hits');
          return { label: lbl, hits, impact: directClusterNames.has(lbl) ? 'direct' : 'indirect' };
        });
      }

      const directCount = d1Nodes.length;
      const procCount = impactedProcesses.length;
      const clusterCount = impactedClusters.length;
      const riskLevel = computeRiskLevel(directCount, procCount, clusterCount, totalHits);

      const dirLabel = direction === 'upstream'
        ? 'Consumers of this target (breakage risk)'
        : 'What this target depends on';

      const out: string[] = [
        `\ud83d\udd34 IMPACT: ${target} | ${direction} | ${totalHits} affected`,
        `Confidence: High ${confBuckets.high} | Medium ${confBuckets.medium} | Low ${confBuckets.low}`,
        ``,
        `AFFECTED PROCESSES:`,
        ...(impactedProcesses.length > 0
          ? impactedProcesses.map(p => `- ${p.label} - BROKEN at step ${p.minStep ?? '?'} (${p.hits} symbols, ${p.stepCount ?? '?'} steps)`)
          : ['- None found']),
        ``,
        `AFFECTED CLUSTERS:`,
        ...(impactedClusters.length > 0
          ? impactedClusters.map(c => `- ${c.label} (${c.impact}, ${c.hits} symbols)`)
          : ['- None found']),
        ``,
        `RISK: ${riskLevel}`,
        `- Direct callers: ${directCount}`,
        `- Processes affected: ${procCount}`,
        `- Clusters affected: ${clusterCount}`,
        ``,
      ];

      if (d1Nodes.length > 0) {
        const hdr = direction === 'upstream'
          ? `d=1 (Directly DEPEND ON ${target}):`
          : `d=1 (${target} USES these):`;
        out.push(hdr);
        d1Nodes.slice(0, 15).forEach(n => {
          out.push(formatAffectedNode(n));
          const snip = extractSnippet(n);
          if (snip) out.push(`    \u21b3 "${snip}"`);
        });
        if (d1Nodes.length > 15) out.push(`  ... +${d1Nodes.length - 15} more`);
        out.push(``);
      }

      if (d2Nodes.length > 0) {
        const hdr = direction === 'upstream'
          ? `d=2 (Indirectly DEPEND ON ${target}):`
          : `d=2 (${target} USES these indirectly):`;
        out.push(hdr);
        d2Nodes.slice(0, 15).forEach(n => out.push(formatAffectedNode(n)));
        if (d2Nodes.length > 15) out.push(`  ... +${d2Nodes.length - 15} more`);
        out.push(``);
      }

      if (d3Nodes.length > 0) {
        out.push(`d=3 (Deep impact/dependency):`);
        d3Nodes.slice(0, 5).forEach(n => out.push(formatAffectedNode(n)));
        if (d3Nodes.length > 5) out.push(`  ... +${d3Nodes.length - 5} more`);
        out.push(``);
      }

      out.push(`\u2705 GRAPH ANALYSIS COMPLETE (trusted)`);
      out.push(`\u26a0\ufe0f Optional: grep("${target}") for dynamic patterns`);
      if (multipleMatchWarning) out.push(multipleMatchWarning);
      out.push(``);

      return out.join('\n');
    },
    {
      name: 'impact',
      description: `Change-impact analysis for a function, class, or file.

Typical questions this answers:
- "What breaks if I modify X?"
- "What does X depend on?"
- "Show me the blast radius of X"

Direction:
- upstream: what CALLS/IMPORTS/EXTENDS the target (breakage risk)
- downstream: what the target CALLS/IMPORTS/EXTENDS (its own dependencies)

Output is compact tabular:
  Type|Name|File:Line|EdgeType|Confidence%

EdgeType values: CALLS, IMPORTS, EXTENDS, IMPLEMENTS
Confidence: 100% = definite, <80% = heuristic match (possible false positive)

relationTypes (optional):
- Defaults to usage edges: CALLS, IMPORTS, EXTENDS, IMPLEMENTS
- Add CONTAINS, DEFINES for structural-level analysis

Also reports:
- Impacted processes (with step-level detail)
- Impacted clusters (direct vs indirect)
- Overall risk rating (based on caller count, process spread, cluster spread)`,
      schema: z.object({
        target: z.string().describe('Function, class, or file to analyze'),
        direction: z.enum(['upstream', 'downstream']).describe('upstream = what depends on this; downstream = what this uses'),
        maxDepth: z.number().optional().describe('Traversal depth cap (default: 3, max: 10)'),
        relationTypes: z.array(z.string()).optional().describe('Edge types to follow: CALLS, IMPORTS, EXTENDS, IMPLEMENTS, CONTAINS, DEFINES (default: usage-based)'),
        includeTests: z.boolean().optional().describe('Include test files (default: false — skips .test.ts, .spec.ts, __tests__)'),
        minConfidence: z.number().optional().describe('Confidence floor 0-1 (default: 0.7 — filters out fuzzy/inferred edges)'),
      }),
    },
  );
}

/* ── Public API ──────────────────────────────────────── */

export const buildAnalysisTools = (
  executeQuery: (cypher: string) => Promise<any[]>,
  semanticSearch: (query: string, k?: number, maxDistance?: number) => Promise<any[]>,
  semanticSearchWithContext: (query: string, k?: number, hops?: number) => Promise<any[]>,
  hybridSearch: (query: string, k?: number) => Promise<any[]>,
  isEmbeddingReady: () => boolean,
  isBM25Ready: () => boolean,
  fileContents: Map<string, string>,
) => {
  return [
    createSearchTool(executeQuery, semanticSearch, hybridSearch, isEmbeddingReady, isBM25Ready),
    createCypherTool(executeQuery, isEmbeddingReady),
    createGrepTool(fileContents),
    createReadTool(fileContents),
    createOverviewTool(executeQuery),
    createExploreTool(executeQuery),
    createImpactTool(executeQuery, fileContents),
  ];
};
