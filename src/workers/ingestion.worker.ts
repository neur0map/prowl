import * as Comlink from 'comlink';
import type { IndexingProgress, SerializableIndexingResult } from '../types/pipeline';
import type { FileEntry } from '../types/file-entry';
import type { EmbeddingProgress, SemanticSearchResult } from '../core/embeddings/types';
import type { ProviderConfig, AgentStreamChunk } from '../core/llm/types';
import type { ClusterEnrichment, ClusterMemberInfo } from '../core/ingestion/cluster-enricher';
import type { CommunityNode } from '../core/ingestion/community-processor';
import type { AgentMessage } from '../core/llm/agent';
import type { SearchHit } from '../core/search';
import type { EmbeddingProgressCallback } from '../core/embeddings/embedding-pipeline';
import { serializeIndexingResult, IndexingResult } from '../types/pipeline';

let pipelineModule: typeof import('../core/ingestion/pipeline') | null = null;
const getPipeline = async () => {
  if (!pipelineModule) pipelineModule = await import('../core/ingestion/pipeline');
  return pipelineModule;
};

let agentModule: typeof import('../core/llm/agent') | null = null;
const getAgent = async () => {
  if (!agentModule) agentModule = await import('../core/llm/agent');
  return agentModule;
};

let embeddingModule: typeof import('../core/embeddings/embedding-pipeline') | null = null;
const getEmbedding = async () => {
  if (!embeddingModule) embeddingModule = await import('../core/embeddings/embedding-pipeline');
  return embeddingModule;
};

let embedderModule: typeof import('../core/embeddings/embedder') | null = null;
const getEmbedder = async () => {
  if (!embedderModule) embedderModule = await import('../core/embeddings/embedder');
  return embedderModule;
};

let rerankerModule: typeof import('../core/embeddings/reranker') | null = null;
const getReranker = async () => {
  if (!rerankerModule) rerankerModule = await import('../core/embeddings/reranker');
  return rerankerModule;
};

let searchModule: typeof import('../core/search') | null = null;
const getSearch = async () => {
  if (!searchModule) searchModule = await import('../core/search');
  return searchModule;
};

let contextModule: typeof import('../core/llm/context-builder') | null = null;
const getContext = async () => {
  if (!contextModule) contextModule = await import('../core/llm/context-builder');
  return contextModule;
};

let enricherModule: typeof import('../core/ingestion/cluster-enricher') | null = null;
const getEnricher = async () => {
  if (!enricherModule) enricherModule = await import('../core/ingestion/cluster-enricher');
  return enricherModule;
};

let langcoreModule: typeof import('@langchain/core/messages') | null = null;
const getLangCore = async () => {
  if (!langcoreModule) langcoreModule = await import('@langchain/core/messages');
  return langcoreModule;
};

/* KuzuDB needs SharedArrayBuffer; lazy-load so we degrade cleanly without it */
let kuzuAdapter: typeof import('../core/kuzu/kuzu-adapter') | null = null;
const getKuzuAdapter = async () => {
  if (!kuzuAdapter) {
    kuzuAdapter = await import('../core/kuzu/kuzu-adapter');
  }
  return kuzuAdapter;
};

/* Snapshot serialiser (deferred) */
let snapshotModule: typeof import('../core/snapshot') | null = null;
const getSnapshot = async () => {
  if (!snapshotModule) snapshotModule = await import('../core/snapshot');
  return snapshotModule;
};

/* Vector embedding progress */
let embeddingProgress: EmbeddingProgress | null = null;
let isEmbeddingComplete = false;

/* Full source text map — powers the grep and read tools */
let storedFileContents: Map<string, string> = new Map();

/* Path of the loaded project (used by snapshot I/O) */
let projectPath: string | null = null;

/* LLM agent handles */
let currentAgent: any | null = null;
let currentProviderConfig: ProviderConfig | null = null;
let currentGraphResult: IndexingResult | null = null;

let pendingEnrichmentConfig: ProviderConfig | null = null;
let enrichmentCancelled = false;

/* Flag to abort an in-flight chat stream */
let chatCancelled = false;

async function warmEmbeddingModel(): Promise<void> {
  try {
    const { initEmbedder } = await getEmbedder();
    /* initEmbedder auto-falls back from WebGPU → WASM on failure/timeout */
    await initEmbedder();
  } catch (err) {
    if (import.meta.env.DEV) {
      console.warn('[prowl:embedding] Model warm-up failed:', err);
    }
  }
}

async function warmRerankerModel(forceDevice?: 'webgpu' | 'wasm'): Promise<void> {
  try {
    const { initReranker } = await getReranker();
    await initReranker(undefined, {}, forceDevice);
  } catch (err) {
    if (import.meta.env.DEV) {
      console.warn('[prowl:reranker] Model warm-up failed:', err);
    }
  }
}

const workerApi = {
  async runQuery(cypher: string): Promise<any[]> {
    const kuzu = await getKuzuAdapter();
    if (!kuzu.isKuzuReady()) {
      throw new Error('Load a project before querying.');
    }
    return kuzu.executeQuery(cypher);
  },

  async isReady(): Promise<boolean> {
    try {
      const kuzu = await getKuzuAdapter();
      return kuzu.isKuzuReady();
    } catch {
      return false;
    }
  },

  async getStats(): Promise<{ nodes: number; edges: number }> {
    try {
      const kuzu = await getKuzuAdapter();
      return kuzu.getKuzuStats();
    } catch {
      return { nodes: 0, edges: 0 };
    }
  },

  async runPipelineFromFiles(
    files: FileEntry[],
    onProgress: (progress: IndexingProgress) => void,
    clusteringConfig?: ProviderConfig
  ): Promise<SerializableIndexingResult> {
    /* Reset stale embedding state from any prior project session */
    const { resetEmbedderState } = await getEmbedder();
    resetEmbedderState();
    isEmbeddingComplete = false;
    embeddingProgress = null;

    onProgress({
      phase: 'extracting',
      percent: 15,
      message: 'Files ready',
      stats: { filesProcessed: 0, totalFiles: files.length, nodesCreated: 0 },
    });

    const pipeline = await getPipeline();
    const { buildBM25Index } = await getSearch();
    const result = await pipeline.runPipelineFromFiles(files, onProgress);
    currentGraphResult = result;

    /* Retain full source text for the grep/read analysis tools */
    storedFileContents = result.fileContents;

    /* Populate the BM25 keyword index (~100 ms) */
    buildBM25Index(storedFileContents);

    /* Attempt to load the graph into KuzuDB; non-fatal if unavailable */
    try {
      onProgress({
        phase: 'complete',
        percent: 98,
        message: 'Indexing graph database...',
        stats: {
          filesProcessed: result.graph.nodeCount,
          totalFiles: result.graph.nodeCount,
          nodesCreated: result.graph.nodeCount,
        },
      });

      const kuzu = await getKuzuAdapter();
      await kuzu.loadGraphToKuzu(result.graph, result.fileContents, (pct, msg) => {
        onProgress({
          phase: 'complete',
          percent: 98 + Math.round(pct * 0.02),
          message: msg,
          stats: {
            filesProcessed: result.graph.nodeCount,
            totalFiles: result.graph.nodeCount,
            nodesCreated: result.graph.nodeCount,
          },
        });
      });

    } catch {
      /* KuzuDB unavailable — continue without graph queries */
    }

    /* Stash clustering config so background enrichment can pick it up */
    if (clusteringConfig) {
      pendingEnrichmentConfig = clusteringConfig;
    }

    return serializeIndexingResult(result);
  },

  async startEmbeddingPipeline(
    onProgress: (progress: EmbeddingProgress) => void,
    forceDevice?: 'webgpu' | 'wasm'
  ): Promise<void> {
    const kuzu = await getKuzuAdapter();
    if (!kuzu.isKuzuReady()) {
      throw new Error('Load a project before querying.');
    }

    /* If vectors were already loaded from a snapshot, nothing to compute —
       the index is live and the model is warming in the background */
    if (isEmbeddingComplete) {
      onProgress({ phase: 'ready', percent: 100, nodesProcessed: 0, totalNodes: 0 });
      return;
    }

    embeddingProgress = null;
    isEmbeddingComplete = false;

    const progressCallback: EmbeddingProgressCallback = (progress) => {
      embeddingProgress = progress;
      if (progress.phase === 'ready') {
        isEmbeddingComplete = true;
      }
      onProgress(progress);
    };

    const { runEmbeddingPipeline } = await getEmbedding();
    await runEmbeddingPipeline(
      kuzu.executeQuery,
      kuzu.executeWithReusedStatement,
      progressCallback,
      forceDevice ? { device: forceDevice } : {}
    );

    /* Embedder is done with the GPU — now safe to warm the reranker */
    warmRerankerModel(forceDevice).catch(console.warn);
  },

  async startBackgroundEnrichment(
    onProgress?: (current: number, total: number) => void
  ): Promise<{ enriched: number; skipped: boolean }> {
    if (!pendingEnrichmentConfig) {
      return { enriched: 0, skipped: true };
    }
    
    try {
      await workerApi.enrichCommunities(
        pendingEnrichmentConfig,
        onProgress ?? (() => {})
      );
      pendingEnrichmentConfig = null; /* consumed */
      return { enriched: 1, skipped: false };
    } catch (err) {
      console.error('[prowl:worker] background enrichment failed:', err);
      pendingEnrichmentConfig = null;
      return { enriched: 0, skipped: false };
    }
  },

  async cancelEnrichment(): Promise<void> {
    enrichmentCancelled = true;
    pendingEnrichmentConfig = null;
  },

  async semanticSearch(
    query: string,
    k: number = 10,
    maxDistance: number = 0.5
  ): Promise<SemanticSearchResult[]> {
    const kuzu = await getKuzuAdapter();
    if (!kuzu.isKuzuReady()) {
      throw new Error('Load a project before querying.');
    }
    if (!isEmbeddingComplete) {
      throw new Error('Vector index not ready yet.');
    }

    const { semanticSearch: doSemanticSearch } = await getEmbedding();
    return doSemanticSearch(kuzu.executeQuery, query, k, maxDistance);
  },

  async semanticSearchWithContext(
    query: string,
    k: number = 5,
    hops: number = 2
  ): Promise<any[]> {
    const kuzu = await getKuzuAdapter();
    if (!kuzu.isKuzuReady()) {
      throw new Error('Load a project before querying.');
    }
    if (!isEmbeddingComplete) {
      throw new Error('Vector index not ready yet.');
    }

    const { semanticSearchWithContext: doSemanticSearchWithContext } = await getEmbedding();
    return doSemanticSearchWithContext(kuzu.executeQuery, query, k, hops);
  },

  async hybridSearch(
    query: string,
    k: number = 10,
    useReranker?: boolean
  ): Promise<SearchHit[]> {
    const search = await getSearch();
    if (!search.isBM25Ready()) {
      throw new Error('Index not built yet. Load a project first.');
    }

    const bm25Results = search.searchBM25(query, k * 3);

    let semanticResults: SemanticSearchResult[] = [];
    if (isEmbeddingComplete) {
      try {
        const kuzu = await getKuzuAdapter();
        if (kuzu.isKuzuReady()) {
          const { semanticSearch: doSemanticSearch } = await getEmbedding();
          semanticResults = await doSemanticSearch(kuzu.executeQuery, query, k * 3, 0.5);
        }
      } catch {
        // Semantic search failed, continue with BM25 only
      }
    }

    let merged = search.mergeWithRRF(bm25Results, semanticResults, k);

    /* Post-RRF cross-encoder reranking */
    const shouldRerank = useReranker !== false;
    if (shouldRerank) {
      try {
        const { isRerankerReady, rerank } = await getReranker();
        if (isRerankerReady()) {
          merged = await search.rerankSearchHits(
            query,
            merged,
            storedFileContents,
            rerank,
            k,
          );
        }
      } catch {
        /* Reranker failed — return RRF results as-is */
      }
    }

    return merged;
  },

  async isBM25Ready(): Promise<boolean> {
    const search = await getSearch();
    return search.isBM25Ready();
  },

  async getBM25Stats(): Promise<{ documentCount: number; termCount: number }> {
    const search = await getSearch();
    return search.getBM25Stats();
  },

  async isEmbeddingModelReady(): Promise<boolean> {
    const { isEmbedderReady } = await getEmbedder();
    return isEmbedderReady();
  },

  isEmbeddingComplete(): boolean {
    return isEmbeddingComplete;
  },

  getEmbeddingProgress(): EmbeddingProgress | null {
    return embeddingProgress;
  },

  async disposeEmbeddingModel(): Promise<void> {
    const { disposeEmbedder } = await getEmbedder();
    await disposeEmbedder();
    isEmbeddingComplete = false;
    embeddingProgress = null;
  },

  async isRerankerReady(): Promise<boolean> {
    const { isRerankerReady } = await getReranker();
    return isRerankerReady();
  },

  async startRerankerWarmup(forceDevice?: 'webgpu' | 'wasm'): Promise<void> {
    warmRerankerModel(forceDevice).catch(console.warn);
  },

  async disposeRerankerModel(): Promise<void> {
    const { disposeReranker } = await getReranker();
    await disposeReranker();
  },

  async testArrayParams(): Promise<{ success: boolean; error?: string }> {
    const kuzu = await getKuzuAdapter();
    if (!kuzu.isKuzuReady()) {
      return { success: false, error: 'Database not ready' };
    }
    return kuzu.testArrayParams();
  },

  async initializeAgent(config: ProviderConfig, projectName?: string): Promise<{ success: boolean; error?: string }> {
    try {
      const kuzu = await getKuzuAdapter();
      if (!kuzu.isKuzuReady()) {
        return { success: false, error: 'Load a project before querying.' };
      }

      const { buildCodeAgent } = await getAgent();
      const embedding = await getEmbedding();
      const search = await getSearch();
      const context = await getContext();

      const semanticSearchWrapper = async (query: string, k?: number, maxDistance?: number) => {
        if (!isEmbeddingComplete) {
          throw new Error('Embeddings not ready');
        }
        return embedding.semanticSearch(kuzu.executeQuery, query, k, maxDistance);
      };

      const contextualVectorSearch = async (query: string, k?: number, hops?: number) => {
        if (!isEmbeddingComplete) {
          throw new Error('Embeddings not ready');
        }
        return embedding.semanticSearchWithContext(kuzu.executeQuery, query, k, hops);
      };

      const hybridSearchWrapper = async (query: string, k?: number) => {
        const limit = k ?? 10;
        const bm25Results = search.searchBM25(query, limit * 3);

        let semanticResults: any[] = [];
        if (isEmbeddingComplete) {
          try {
            semanticResults = await embedding.semanticSearch(kuzu.executeQuery, query, limit * 3, 0.5);
          } catch {
            // Semantic search failed, continue with BM25 only
          }
        }

        let merged = search.mergeWithRRF(bm25Results, semanticResults, limit);

        /* Apply cross-encoder reranking if available */
        try {
          const { isRerankerReady, rerank } = await getReranker();
          if (isRerankerReady()) {
            merged = await search.rerankSearchHits(query, merged, storedFileContents, rerank, limit);
          }
        } catch {
          /* Reranker unavailable — use RRF results */
        }

        return merged;
      };

      const resolvedProjectName = projectName || 'project';
      if (import.meta.env.DEV) {
      }

      let codebaseContext;
      try {
        codebaseContext = await context.buildProjectContext(kuzu.executeQuery, resolvedProjectName);
      } catch (err) {
        console.warn('Context build skipped:', err);
      }

      currentAgent = await buildCodeAgent(
        config,
        kuzu.executeQuery,
        semanticSearchWrapper,
        contextualVectorSearch,
        hybridSearchWrapper,
        () => isEmbeddingComplete,
        () => search.isBM25Ready(),
        storedFileContents,
        codebaseContext
      );
      currentProviderConfig = config;


      return { success: true };
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      if (import.meta.env.DEV) {
        console.error('[prowl:worker] agent initialization failed:', error);
      }
      return { success: false, error: message };
    }
  },

  isAgentReady(): boolean {
    return currentAgent !== null;
  },

  getAgentProvider(): { provider: string; model: string } | null {
    if (!currentProviderConfig) return null;
    return {
      provider: currentProviderConfig.provider,
      model: currentProviderConfig.model,
    };
  },

  async chatStream(
    messages: AgentMessage[],
    onChunk: (chunk: AgentStreamChunk) => void
  ): Promise<void> {
    if (!currentAgent) {
      onChunk({ type: 'error', error: 'No LLM provider configured.' });
      return;
    }

    chatCancelled = false;

    try {
      const { streamAgentResponse } = await getAgent();
      for await (const chunk of streamAgentResponse(currentAgent, messages)) {
        if (chatCancelled) {
          onChunk({ type: 'done' });
          break;
        }
        onChunk(chunk);
      }
    } catch (error) {
      if (chatCancelled) {
        /* Suppress errors triggered by the cancellation itself */
        onChunk({ type: 'done' });
        return;
      }
      const message = error instanceof Error ? error.message : String(error);
      onChunk({ type: 'error', error: message });
    }
  },

  stopChat(): void {
    chatCancelled = true;
  },

  async compactHistory(
    messages: AgentMessage[]
  ): Promise<{ compacted: AgentMessage[]; summary: string }> {
    if (!currentProviderConfig) {
      return { compacted: messages, summary: '' };
    }
    const { compactHistory } = await getAgent();
    return compactHistory(currentProviderConfig, messages);
  },

  disposeAgent(): void {
    currentAgent = null;
    currentProviderConfig = null;
  },

  setProjectPath(path: string | null): void {
    projectPath = path;
  },

  async restoreFromSnapshot(
    data: Uint8Array,
    meta: import('../core/snapshot/types').SnapshotMeta,
    onProgress: (progress: IndexingProgress) => void,
  ): Promise<import('../core/snapshot/types').RestoreFromSnapshotResult> {
    /* Reset stale embedding state from any prior project session */
    const { resetEmbedderState } = await getEmbedder();
    resetEmbedderState();
    isEmbeddingComplete = false;
    embeddingProgress = null;

    /* Unpack the binary snapshot */
    onProgress({ phase: 'structure', percent: 20, message: 'Deserializing snapshot...' });
    const { deserializeSnapshot, restoreGraphFromPayload, restoreFileContents } = await getSnapshot();
    const payload = await deserializeSnapshot(data);

    /* Reconstruct the in-memory graph */
    onProgress({ phase: 'parsing', percent: 40, message: 'Restoring graph...' });
    const graph = restoreGraphFromPayload(payload);
    const fileContentsMap = restoreFileContents(payload);

    /* Hydrate worker-level state */
    storedFileContents = fileContentsMap;
    currentGraphResult = {
      graph,
      fileContents: fileContentsMap,
      communityResult: { communities: [], memberships: [], stats: { totalCommunities: 0, modularity: 0, nodesProcessed: 0 } },
      processResult: { processes: [], steps: [], stats: { totalProcesses: 0, crossCommunityCount: 0, avgStepCount: 0, entryPointsFound: 0 } },
    };

    /* Re-create the BM25 keyword index from restored files */
    onProgress({ phase: 'imports', percent: 60, message: 'Rebuilding search index...' });
    const { buildBM25Index } = await getSearch();
    buildBM25Index(storedFileContents);

    /* Reload KuzuDB tables from the snapshot payload */
    onProgress({ phase: 'calls', percent: 70, message: 'Restoring graph database...' });
    try {
      const { restoreKuzuFromSnapshot } = await import('../core/snapshot/kuzu-restorer');
      await restoreKuzuFromSnapshot(payload);
    } catch (err) {
      console.warn('[prowl:snapshot] KuzuDB restore failed (non-fatal):', err);
    }

    /* Mark embeddings as live and warm the model in the background.
       The reranker is NOT warmed eagerly — two concurrent WebGPU pipeline()
       calls fight for the GPU device and one hangs indefinitely. The reranker
       loads lazily on the first search that needs it. */
    const hasEmbeddings = payload.embeddings.length > 0;
    if (hasEmbeddings) {
      isEmbeddingComplete = true;
      warmEmbeddingModel().catch(console.warn);
    }

    onProgress({
      phase: 'complete',
      percent: 100,
      message: `Loaded from cache! ${meta.nodeCount} nodes, ${meta.relationshipCount} edges`,
      stats: {
        filesProcessed: meta.fileCount,
        totalFiles: meta.fileCount,
        nodesCreated: meta.nodeCount,
      },
    });

    return {
      nodes: payload.nodes,
      relationships: payload.relationships,
      fileContents: payload.fileContents,
      hasEmbeddings,
    };
  },

  async incrementalUpdate(
    diff: { added: string[]; modified: string[]; deleted: string[]; isGitRepo: boolean },
    newFileContents: Map<string, string>,
    onProgress: (progress: IndexingProgress) => void,
  ): Promise<SerializableIndexingResult | null> {
    if (!currentGraphResult) return null;

    try {
      const { applyIncrementalUpdate } = await import('../core/snapshot/incremental-updater');
      const result = await applyIncrementalUpdate(
        diff,
        newFileContents,
        currentGraphResult.graph,
        currentGraphResult.fileContents,
        onProgress,
      );

      /* Refresh worker-level state with the updated graph */
      currentGraphResult = result;
      storedFileContents = result.fileContents;

      /* Rebuild keyword index */
      const { buildBM25Index } = await getSearch();
      buildBM25Index(storedFileContents);

      /* Refresh KuzuDB with the updated graph */
      try {
        const kuzu = await getKuzuAdapter();
        await kuzu.loadGraphToKuzu(result.graph, result.fileContents);
      } catch {
        /* graph database unavailable */
      }

      return serializeIndexingResult(result);
    } catch (err) {
      console.warn('[prowl:snapshot] Incremental update failed:', err);
      return null;
    }
  },

  async collectAndSerialize(): Promise<import('../core/snapshot/types').CollectAndSerializeResult> {
    if (!currentGraphResult) {
      throw new Error('No project loaded');
    }

    const { collectSnapshotPayload, serializeSnapshot, buildFileManifest } = await getSnapshot();

    let kuzuQueryFn: ((cypher: string) => Promise<any[]>) | undefined;
    try {
      const kuzu = await getKuzuAdapter();
      if (kuzu.isKuzuReady()) {
        kuzuQueryFn = kuzu.executeQuery;
      }
    } catch { /* no kuzu */ }

    const prowlVersion = (import.meta.env.VITE_APP_VERSION as string) || 'unknown';
    const projectName = projectPath?.split('/').filter(Boolean).pop() || 'project';

    /* gitCommit left null — the renderer fills it in post-serialisation */
    const payload = await collectSnapshotPayload(
      currentGraphResult.graph,
      currentGraphResult.fileContents,
      projectName,
      prowlVersion,
      kuzuQueryFn,
      null,
    );

    const data = await serializeSnapshot(payload);
    const manifest = await buildFileManifest(currentGraphResult.fileContents);

    return Comlink.transfer({ data, meta: payload.meta, manifest }, [data.buffer]);
  },

  async enrichCommunities(
    providerConfig: ProviderConfig,
    onProgress: (current: number, total: number) => void
  ): Promise<{ enrichments: Record<string, ClusterEnrichment>, tokensUsed: number }> {
    if (!currentGraphResult) {
      throw new Error('No project loaded.');
    }

    const { graph } = currentGraphResult;
    
    /* Collect the Community-labelled nodes */
    const communityNodes = graph.nodes
      .filter(n => n.label === 'Community')
      .map(n => ({
        id: n.id,
        label: 'Community',
        heuristicLabel: n.properties.heuristicLabel,
        cohesion: n.properties.cohesion,
        symbolCount: n.properties.symbolCount
      } as CommunityNode));

    if (communityNodes.length === 0) {
      return { enrichments: {}, tokensUsed: 0 };
    }

    /* Map each community to its member symbols */
    const memberMap = new Map<string, ClusterMemberInfo[]>();
    
    communityNodes.forEach(c => memberMap.set(c.id, []));
    
    /* Walk MEMBER_OF relationships to populate the member map */
    graph.relationships.forEach(rel => {
      if (rel.type === 'MEMBER_OF') {
        const communityId = rel.targetId;
        const memberId = rel.sourceId; // MEMBER_OF goes Member -> Community
        
        if (memberMap.has(communityId)) {
          const memberNode = graph.nodes.find(n => n.id === memberId);
          if (memberNode) {
            memberMap.get(communityId)?.push({
              name: memberNode.properties.name,
              filePath: memberNode.properties.filePath,
              type: memberNode.label
            });
          }
        }
      }
    });

    const { createChatModel } = await getAgent();
    const { SystemMessage } = await getLangCore();
    const { labelModulesBatch } = await getEnricher();
    const chatModel = await createChatModel(providerConfig);
    const llmClient = {
      generate: async (prompt: string): Promise<string> => {
        const response = await chatModel.invoke([
          new SystemMessage('You are a helpful code analysis assistant.'),
          { role: 'user', content: prompt }
        ]);
        return response.content as string;
      }
    };

    const { enrichments, tokensUsed } = await labelModulesBatch(
      communityNodes,
      memberMap,
      llmClient,
      5, // Batch size
      onProgress
    );

    if (import.meta.env.DEV) {
    }

    /* Patch in-memory graph nodes with the LLM-generated metadata */
    graph.nodes.forEach(node => {
      if (node.label === 'Community' && enrichments.has(node.id)) {
        const enrichment = enrichments.get(node.id)!;
        node.properties.name = enrichment.name;
        node.properties.keywords = enrichment.keywords;
        node.properties.description = enrichment.description;
        node.properties.enrichedBy = 'llm';
      }
    });

    /* Sync the enriched labels back to KuzuDB */
    try {
      const kuzu = await getKuzuAdapter();
        
      onProgress(enrichments.size, enrichments.size); // Done
      
      for (const [id, enrichment] of enrichments.entries()) {
         const escapeCypher = (str: string) => str.replace(/\\/g, '\\\\').replace(/"/g, '\\"');
         
         const keywordsStr = JSON.stringify(enrichment.keywords);
         const descStr = escapeCypher(enrichment.description);
         const nameStr = escapeCypher(enrichment.name);
         const escapedId = escapeCypher(id);
         
         const query = `
           MATCH (c:Community {id: "${escapedId}"})
           SET c.label = "${nameStr}", 
               c.keywords = ${keywordsStr}, 
               c.description = "${descStr}",
               c.enrichedBy = "llm"
         `;
         
         await kuzu.executeQuery(query);
      }
      
    } catch (err) {
      console.error('Enrichment sync failed:', err);
    }
    
    const enrichmentsRecord: Record<string, ClusterEnrichment> = {};
    for (const [id, val] of enrichments.entries()) {
      enrichmentsRecord[id] = val;
    }
     
    return { enrichments: enrichmentsRecord, tokensUsed };

  },

  /* ── Live update (real-time file change pipeline) ────── */

  async liveUpdate(
    changes: Map<string, { type: string; content: string | null }>,
  ): Promise<SerializableIndexingResult | null> {
    if (!currentGraphResult) return null;

    const { createCodeGraph } = await import('../core/graph/graph');
    const { reparseFiles } = await import('../core/snapshot/incremental-updater');

    const changedPaths = new Set(changes.keys());
    const deletedPaths = new Set<string>();
    const addedWithContent = new Map<string, string>(); // path → content for adds
    const removedDirPrefixes: string[] = [];
    for (const [path, change] of changes) {
      if (change.type === 'remove' || change.content === null) deletedPaths.add(path);
      if (change.type === 'removeDir') removedDirPrefixes.push(path.endsWith('/') ? path : path + '/');
      if ((change.type === 'add') && change.content) addedWithContent.set(path, change.content);
    }

    // Rename detection: if a file is deleted and a new file is added with
    // identical content in the same batch, treat as a rename.  This preserves
    // edges from other files that pointed to symbols in the old path.
    const renamedPaths = new Map<string, string>(); // oldPath → newPath
    if (deletedPaths.size > 0 && addedWithContent.size > 0) {
      // Build content → oldPath lookup from deleted files (using stored content)
      const deletedContentMap = new Map<string, string>();
      for (const dp of deletedPaths) {
        const old = storedFileContents.get(dp);
        if (old) deletedContentMap.set(old, dp);
      }
      for (const [newPath, newContent] of addedWithContent) {
        const oldPath = deletedContentMap.get(newContent);
        if (oldPath) {
          renamedPaths.set(oldPath, newPath);
          deletedPaths.delete(oldPath);
          deletedContentMap.delete(newContent); // don't match twice
          if (import.meta.env.DEV) {
            console.log(`[prowl:live] detected rename: ${oldPath} → ${newPath}`);
          }
        }
      }
    }

    // Helper: check if a filePath falls under any removed directory
    const isUnderRemovedDir = (fp: string): boolean =>
      removedDirPrefixes.length > 0 && removedDirPrefixes.some(prefix => fp.startsWith(prefix) || fp === prefix.slice(0, -1));

    const graph = createCodeGraph();
    const nodesToRemove = new Set<string>();

    // Copy nodes — keep File/Folder nodes for changed (non-deleted) files so
    // incoming IMPORTS/CONTAINS edges from other files stay connected.
    // Only remove symbol nodes (Function, Class, etc.) for changed files.
    for (const node of currentGraphResult.graph.nodes) {
      if (node.label === 'Community' || node.label === 'Process') continue;
      const filePath = node.properties.filePath;

      // Nodes under a removed directory → remove entirely
      if (filePath && isUnderRemovedDir(filePath)) {
        nodesToRemove.add(node.id);
        continue;
      }

      if (filePath && changedPaths.has(filePath)) {
        if (deletedPaths.has(filePath) || renamedPaths.has(filePath)) {
          // File deleted or renamed away → remove all nodes for old path
          nodesToRemove.add(node.id);
        } else if (node.label === 'File' || node.label === 'Folder') {
          // File changed → keep File/Folder nodes (preserves IMPORTS/CONTAINS)
          graph.addNode(node);
        } else {
          // Symbol in changed file → remove (will be recreated by reparse)
          nodesToRemove.add(node.id);
        }
      } else {
        graph.addNode(node);
      }
    }

    // Copy edges; defer incoming edges to removed symbols (may be restored after reparse)
    type DeferredEdge = import('../core/graph/types').GraphRelationship;
    const deferredEdges: DeferredEdge[] = [];
    for (const rel of currentGraphResult.graph.relationships) {
      if (rel.type === 'MEMBER_OF' || rel.type === 'STEP_IN_PROCESS') continue;
      if (nodesToRemove.has(rel.sourceId)) continue; // source gone → drop
      if (nodesToRemove.has(rel.targetId)) {
        // Target removed but source intact → defer (restore if target recreated)
        deferredEdges.push(rel);
        continue;
      }
      graph.addRelationship(rel);
    }

    // Snapshot storedFileContents before mutation for rollback on failure
    const fileContentsBefore = new Map(storedFileContents);

    // Update storedFileContents
    for (const [path, change] of changes) {
      if (change.type === 'remove' || change.type === 'removeDir' || change.content === null) {
        storedFileContents.delete(path);
      } else {
        storedFileContents.set(path, change.content);
      }
    }
    // Remove all files under deleted directories
    if (removedDirPrefixes.length > 0) {
      for (const key of [...storedFileContents.keys()]) {
        if (isUnderRemovedDir(key)) storedFileContents.delete(key);
      }
    }

    // Prepare changed files for reparsing
    const changedFiles: Array<{ path: string; content: string }> = [];
    for (const [path, change] of changes) {
      if (change.content !== null && change.type !== 'remove') {
        changedFiles.push({ path, content: change.content });
      }
    }

    try {
      // Reparse changed files (structure, symbols, imports, calls, heritage)
      await reparseFiles(graph, changedFiles, storedFileContents);

      // Restore deferred edges whose target was recreated by reparsing
      for (const rel of deferredEdges) {
        if (graph.hasNode(rel.targetId)) {
          graph.addRelationship(rel);
        }
      }

      // Re-run communities on full graph
      const { processCommunities } = await import('../core/ingestion/community-processor');
      const communityResult = await processCommunities(graph);

      for (const comm of communityResult.communities) {
        graph.addNode({
          id: comm.id,
          label: 'Community',
          properties: {
            name: comm.label,
            filePath: '',
            heuristicLabel: comm.heuristicLabel,
            cohesion: comm.cohesion,
            symbolCount: comm.symbolCount,
          },
        });
      }
      for (const m of communityResult.memberships) {
        graph.addRelationship({
          id: `${m.nodeId}_member_of_${m.communityId}`,
          type: 'MEMBER_OF',
          sourceId: m.nodeId,
          targetId: m.communityId,
          confidence: 1.0,
          reason: 'louvain-algorithm',
        });
      }

      // Re-run processes on full graph
      const { processProcesses } = await import('../core/ingestion/process-processor');
      const processResult = await processProcesses(graph, communityResult.memberships);

      for (const proc of processResult.processes) {
        graph.addNode({
          id: proc.id,
          label: 'Process',
          properties: {
            name: proc.label,
            filePath: '',
            heuristicLabel: proc.heuristicLabel,
            processType: proc.processType,
            stepCount: proc.stepCount,
            communities: proc.communities,
            entryPointId: proc.entryPointId,
            terminalId: proc.terminalId,
          },
        });
      }
      for (const step of processResult.steps) {
        graph.addRelationship({
          id: `${step.nodeId}_step_${step.step}_${step.processId}`,
          type: 'STEP_IN_PROCESS',
          sourceId: step.nodeId,
          targetId: step.processId,
          confidence: 1.0,
          reason: 'trace-detection',
          step: step.step,
        });
      }

      // Commit: update worker state
      currentGraphResult = { graph, fileContents: storedFileContents, communityResult, processResult };

      // Rebuild BM25 keyword index
      const { buildBM25Index } = await getSearch();
      buildBM25Index(storedFileContents);

      // Reload KuzuDB — preserves CodeEmbedding table so existing vector
      // embeddings for unchanged symbols survive.
      try {
        const kuzu = await getKuzuAdapter();
        await kuzu.reloadKuzuData(graph, storedFileContents);
      } catch (err) {
        if (import.meta.env.DEV) console.warn('[prowl:live] KuzuDB reload failed:', err);
      }

      if (import.meta.env.DEV) {
        console.log(`[prowl:live] updated ${changes.size} files — ${graph.nodeCount} nodes, ${graph.relationshipCount} edges`);
      }

      return serializeIndexingResult(currentGraphResult);
    } catch (err) {
      // Rollback: restore storedFileContents to pre-mutation state
      storedFileContents = fileContentsBefore;
      if (import.meta.env.DEV) console.error('[prowl:live] liveUpdate failed, rolled back storedFileContents:', err);
      throw err;
    }
  },

  /* ── MCP wrapper methods ─────────────────────────────── */

  async mcpGrep(
    pattern: string,
    fileFilter?: string,
    caseSensitive?: boolean,
    maxResults?: number,
  ): Promise<Array<{ file: string; line: number; content: string }>> {
    const regexFlags = caseSensitive ? 'g' : 'gi';
    const compiledPattern = new RegExp(pattern, regexFlags);
    const cap = maxResults ?? 100;
    const hits: Array<{ file: string; line: number; content: string }> = [];

    for (const [fp, body] of storedFileContents.entries()) {
      if (fileFilter && !fp.toLowerCase().includes(fileFilter.toLowerCase())) continue;
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
    return hits;
  },

  mcpReadFile(filePath: string): { content: string | null; resolvedPath: string | null } {
    const normalizedInput = filePath.replace(/\\/g, '/').toLowerCase();
    let content = storedFileContents.get(filePath) ?? null;
    let resolvedPath: string | null = content ? filePath : null;

    if (!content) {
      for (const [candidate, body] of storedFileContents.entries()) {
        const normalizedCandidate = candidate.toLowerCase();
        if (normalizedCandidate === normalizedInput || normalizedCandidate.endsWith(normalizedInput)) {
          content = body;
          resolvedPath = candidate;
          break;
        }
      }
    }

    return { content, resolvedPath };
  },

  async mcpGetContext(projectName?: string): Promise<any> {
    const kuzu = await getKuzuAdapter();
    if (!kuzu.isKuzuReady()) return null;
    const context = await getContext();
    return context.buildProjectContext(kuzu.executeQuery, projectName || 'project');
  },

  async mcpGetHotspots(limit?: number): Promise<any[]> {
    const kuzu = await getKuzuAdapter();
    if (!kuzu.isKuzuReady()) return [];
    const context = await getContext();
    return context.getHotspots(kuzu.executeQuery, limit ?? 10);
  },

  async mcpAsk(question: string): Promise<string> {
    if (!currentAgent) {
      throw new Error('No LLM provider configured. Initialize the agent in Prowl first.');
    }

    chatCancelled = false;
    const chunks: string[] = [];

    const { streamAgentResponse } = await getAgent();
    const messages = [{ role: 'user' as const, content: question }];
    for await (const chunk of streamAgentResponse(currentAgent, messages)) {
      if (chatCancelled) break;
      if (chunk.type === 'content' && chunk.content) chunks.push(chunk.content);
      if (chunk.type === 'reasoning' && chunk.reasoning) chunks.push(chunk.reasoning);
      if (chunk.type === 'error') throw new Error(chunk.error);
    }
    return chunks.join('');
  },

  async mcpInvestigate(task: string, depth?: number): Promise<string> {
    if (!currentAgent) {
      throw new Error('No LLM provider configured. Initialize the agent in Prowl first.');
    }

    chatCancelled = false;
    const chunks: string[] = [];

    const maxSteps = depth ?? 5;
    const framedQuestion = `Investigation task (use up to ${maxSteps} tool calls to be thorough):\n\n${task}\n\nBe systematic: search broadly first, then drill into specifics. Summarize findings with file paths and line numbers.`;

    const { streamAgentResponse } = await getAgent();
    const messages = [{ role: 'user' as const, content: framedQuestion }];
    for await (const chunk of streamAgentResponse(currentAgent, messages)) {
      if (chatCancelled) break;
      if (chunk.type === 'content' && chunk.content) chunks.push(chunk.content);
      if (chunk.type === 'reasoning' && chunk.reasoning) chunks.push(chunk.reasoning);
      if (chunk.type === 'error') throw new Error(chunk.error);
    }
    return chunks.join('');
  },
};

Comlink.expose(workerApi);

export type IndexerWorkerApi = typeof workerApi;

