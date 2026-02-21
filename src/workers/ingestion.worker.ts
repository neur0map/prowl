import * as Comlink from 'comlink';
import type { PipelineProgress, SerializablePipelineResult } from '../types/pipeline';
import type { FileEntry } from '../services/zip';
import type { EmbeddingProgress, SemanticSearchResult } from '../core/embeddings/types';
import type { ProviderConfig, AgentStreamChunk } from '../core/llm/types';
import type { ClusterEnrichment, ClusterMemberInfo } from '../core/ingestion/cluster-enricher';
import type { CommunityNode } from '../core/ingestion/community-processor';
import type { AgentMessage } from '../core/llm/agent';
import type { HybridSearchResult } from '../core/search';
import type { EmbeddingProgressCallback } from '../core/embeddings/embedding-pipeline';
import { serializePipelineResult, PipelineResult } from '../types/pipeline';

// Lazy loaders (same pattern as existing getKuzuAdapter)
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

// Lazy import for Kuzu to avoid breaking worker if SharedArrayBuffer unavailable
let kuzuAdapter: typeof import('../core/kuzu/kuzu-adapter') | null = null;
const getKuzuAdapter = async () => {
  if (!kuzuAdapter) {
    kuzuAdapter = await import('../core/kuzu/kuzu-adapter');
  }
  return kuzuAdapter;
};

// Snapshot module (lazy loaded)
let snapshotModule: typeof import('../core/snapshot') | null = null;
const getSnapshot = async () => {
  if (!snapshotModule) snapshotModule = await import('../core/snapshot');
  return snapshotModule;
};

// Embedding state
let embeddingProgress: EmbeddingProgress | null = null;
let isEmbeddingComplete = false;

// File contents state - stores full file contents for grep/read tools
let storedFileContents: Map<string, string> = new Map();

// Project path state (for snapshot persistence)
let projectPath: string | null = null;

// Agent state
let currentAgent: any | null = null;
let currentProviderConfig: ProviderConfig | null = null;
let currentGraphResult: PipelineResult | null = null;

// Pending enrichment config (for background processing)
let pendingEnrichmentConfig: ProviderConfig | null = null;
let enrichmentCancelled = false;

// Chat cancellation flag
let chatCancelled = false;

/**
 * Warm the embedding model in the background so semantic search is instant.
 */
async function warmEmbeddingModel(): Promise<void> {
  try {
    const { initEmbedder } = await getEmbedder();
    try {
      await initEmbedder(undefined, {}, 'webgpu');
    } catch {
      await initEmbedder(undefined, {}, 'wasm');
    }
  } catch (err) {
    if (import.meta.env.DEV) {
      console.warn('[prowl:embedding] Model warm-up failed:', err);
    }
  }
}

/**
 * Worker API exposed via Comlink
 *
 * Note: The onProgress callback is passed as a Comlink.proxy() from the main thread,
 * allowing it to be called from the worker and have it execute on the main thread.
 */
const workerApi = {
  /**
   * Run the ingestion pipeline in the worker thread
   * @param file - The ZIP file to process
   * @param onProgress - Proxied callback for progress updates (runs on main thread)
   * @returns Serializable result (nodes, relationships, fileContents as object)
   */
  async runPipeline(
    file: File,
    onProgress: (progress: PipelineProgress) => void,
    clusteringConfig?: ProviderConfig
  ): Promise<SerializablePipelineResult> {
    const { runIngestionPipeline } = await getPipeline();
    const { buildBM25Index } = await getSearch();
    // Run the actual pipeline
    const result = await runIngestionPipeline(file, onProgress);
    currentGraphResult = result;

    // Store file contents for grep/read tools (full content, not truncated)
    storedFileContents = result.fileContents;

    // Build BM25 index for keyword search (instant, ~100ms)
    buildBM25Index(storedFileContents);
    
    // Load graph into KuzuDB for querying (optional - gracefully degrades)
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
      await kuzu.loadGraphToKuzu(result.graph, result.fileContents);
      
    } catch {
      // KuzuDB is optional - silently continue without it
    }
    
    // Store clustering config for background enrichment (runs after graph loads)
    if (clusteringConfig) {
      pendingEnrichmentConfig = clusteringConfig;
    }
    
    // Convert to serializable format for transfer back to main thread
    return serializePipelineResult(result);
  },

  /**
   * Execute a Cypher query against the KuzuDB database
   * @param cypher - The Cypher query string
   * @returns Query results as an array of objects
   */
  async runQuery(cypher: string): Promise<any[]> {
    const kuzu = await getKuzuAdapter();
    if (!kuzu.isKuzuReady()) {
      throw new Error('Database not ready. Please load a repository first.');
    }
    return kuzu.executeQuery(cypher);
  },

  /**
   * Check if the database is ready for queries
   */
  async isReady(): Promise<boolean> {
    try {
      const kuzu = await getKuzuAdapter();
      return kuzu.isKuzuReady();
    } catch {
      return false;
    }
  },

  /**
   * Get database statistics
   */
  async getStats(): Promise<{ nodes: number; edges: number }> {
    try {
      const kuzu = await getKuzuAdapter();
      return kuzu.getKuzuStats();
    } catch {
      return { nodes: 0, edges: 0 };
    }
  },

  /**
   * Run the ingestion pipeline from pre-extracted files (e.g., from git clone)
   * @param files - Array of file entries with path and content
   * @param onProgress - Proxied callback for progress updates
   * @returns Serializable result
   */
  async runPipelineFromFiles(
    files: FileEntry[],
    onProgress: (progress: PipelineProgress) => void,
    clusteringConfig?: ProviderConfig
  ): Promise<SerializablePipelineResult> {
    // Skip extraction phase, start from 15%
    onProgress({
      phase: 'extracting',
      percent: 15,
      message: 'Files ready',
      stats: { filesProcessed: 0, totalFiles: files.length, nodesCreated: 0 },
    });

    const pipeline = await getPipeline();
    const { buildBM25Index } = await getSearch();
    // Run the pipeline
    const result = await pipeline.runPipelineFromFiles(files, onProgress);
    currentGraphResult = result;

    // Store file contents for grep/read tools (full content, not truncated)
    storedFileContents = result.fileContents;

    // Build BM25 index for keyword search (instant, ~100ms)
    buildBM25Index(storedFileContents);
    
    // Load graph into KuzuDB for querying (optional - gracefully degrades)
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
      await kuzu.loadGraphToKuzu(result.graph, result.fileContents);
      
    } catch {
      // KuzuDB is optional - silently continue without it
    }
    
    // Store clustering config for background enrichment (runs after graph loads)
    if (clusteringConfig) {
      pendingEnrichmentConfig = clusteringConfig;
    }
    
    // Convert to serializable format for transfer back to main thread
    return serializePipelineResult(result);
  },

  // ============================================================
  // Embedding Pipeline Methods
  // ============================================================

  /**
   * Start the embedding pipeline in the background
   * Generates embeddings for all embeddable nodes and creates vector index
   * @param onProgress - Proxied callback for embedding progress updates
   * @param forceDevice - Force a specific device ('webgpu' or 'wasm')
   */
  async startEmbeddingPipeline(
    onProgress: (progress: EmbeddingProgress) => void,
    forceDevice?: 'webgpu' | 'wasm'
  ): Promise<void> {
    const kuzu = await getKuzuAdapter();
    if (!kuzu.isKuzuReady()) {
      throw new Error('Database not ready. Please load a repository first.');
    }

    // Reset state
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
  },

  /**
   * Start background cluster enrichment (if pending)
   * Called after graph loads, runs in background like embeddings
   * @param onProgress - Progress callback
   */
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
      pendingEnrichmentConfig = null; // Clear after running
      return { enriched: 1, skipped: false };
    } catch (err) {
      console.error('❌ Background enrichment failed:', err);
      pendingEnrichmentConfig = null;
      return { enriched: 0, skipped: false };
    }
  },

  /**
   * Cancel the current enrichment operation
   */
  async cancelEnrichment(): Promise<void> {
    enrichmentCancelled = true;
    pendingEnrichmentConfig = null;
  },

  /**
   * Perform semantic search on the codebase
   * @param query - Natural language search query
   * @param k - Number of results to return (default: 10)
   * @param maxDistance - Maximum distance threshold (default: 0.5)
   * @returns Array of search results ordered by relevance
   */
  async semanticSearch(
    query: string,
    k: number = 10,
    maxDistance: number = 0.5
  ): Promise<SemanticSearchResult[]> {
    const kuzu = await getKuzuAdapter();
    if (!kuzu.isKuzuReady()) {
      throw new Error('Database not ready. Please load a repository first.');
    }
    if (!isEmbeddingComplete) {
      throw new Error('Embeddings not ready. Please wait for embedding pipeline to complete.');
    }

    const { semanticSearch: doSemanticSearch } = await getEmbedding();
    return doSemanticSearch(kuzu.executeQuery, query, k, maxDistance);
  },

  /**
   * Perform semantic search with graph expansion
   * Finds similar nodes AND their connections
   * @param query - Natural language search query
   * @param k - Number of initial results (default: 5)
   * @param hops - Number of graph hops to expand (default: 2)
   * @returns Search results with connected nodes
   */
  async semanticSearchWithContext(
    query: string,
    k: number = 5,
    hops: number = 2
  ): Promise<any[]> {
    const kuzu = await getKuzuAdapter();
    if (!kuzu.isKuzuReady()) {
      throw new Error('Database not ready. Please load a repository first.');
    }
    if (!isEmbeddingComplete) {
      throw new Error('Embeddings not ready. Please wait for embedding pipeline to complete.');
    }

    const { semanticSearchWithContext: doSemanticSearchWithContext } = await getEmbedding();
    return doSemanticSearchWithContext(kuzu.executeQuery, query, k, hops);
  },

  /**
   * Perform hybrid search combining BM25 (keyword) and semantic (embedding) search
   * Uses Reciprocal Rank Fusion (RRF) to merge results
   * 
   * @param query - Search query
   * @param k - Number of results to return (default: 10)
   * @returns Hybrid search results with RRF scores
   */
  async hybridSearch(
    query: string,
    k: number = 10
  ): Promise<HybridSearchResult[]> {
    const search = await getSearch();
    if (!search.isBM25Ready()) {
      throw new Error('Search index not ready. Please load a repository first.');
    }

    // Get BM25 results (always available after ingestion)
    const bm25Results = search.searchBM25(query, k * 3);  // Get more for better RRF merge

    // Get semantic results if embeddings are ready
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

    // Merge with RRF
    return search.mergeWithRRF(bm25Results, semanticResults, k);
  },

  /**
   * Check if BM25 search index is ready
   */
  async isBM25Ready(): Promise<boolean> {
    const search = await getSearch();
    return search.isBM25Ready();
  },

  /**
   * Get BM25 index statistics
   */
  async getBM25Stats(): Promise<{ documentCount: number; termCount: number }> {
    const search = await getSearch();
    return search.getBM25Stats();
  },

  /**
   * Check if the embedding model is loaded and ready
   */
  async isEmbeddingModelReady(): Promise<boolean> {
    const { isEmbedderReady } = await getEmbedder();
    return isEmbedderReady();
  },

  /**
   * Check if embeddings are fully generated and indexed
   */
  isEmbeddingComplete(): boolean {
    return isEmbeddingComplete;
  },

  /**
   * Get current embedding progress
   */
  getEmbeddingProgress(): EmbeddingProgress | null {
    return embeddingProgress;
  },

  /**
   * Cleanup embedding model resources
   */
  async disposeEmbeddingModel(): Promise<void> {
    const { disposeEmbedder } = await getEmbedder();
    await disposeEmbedder();
    isEmbeddingComplete = false;
    embeddingProgress = null;
  },

  /**
   * Test if KuzuDB supports array parameters in prepared statements
   * This is a diagnostic function
   */
  async testArrayParams(): Promise<{ success: boolean; error?: string }> {
    const kuzu = await getKuzuAdapter();
    if (!kuzu.isKuzuReady()) {
      return { success: false, error: 'Database not ready' };
    }
    return kuzu.testArrayParams();
  },

  // ============================================================
  // Graph RAG Agent Methods
  // ============================================================

  /**
   * Initialize the Graph RAG agent with a provider configuration
   * Must be called before using chat methods
   * @param config - Provider configuration (Azure OpenAI or Gemini)
   * @param projectName - Name of the loaded project/repository
   */
  async initializeAgent(config: ProviderConfig, projectName?: string): Promise<{ success: boolean; error?: string }> {
    try {
      const kuzu = await getKuzuAdapter();
      if (!kuzu.isKuzuReady()) {
        return { success: false, error: 'Database not ready. Please load a repository first.' };
      }

      const { createGraphRAGAgent } = await getAgent();
      const embedding = await getEmbedding();
      const search = await getSearch();
      const context = await getContext();

      // Create semantic search wrappers that handle embedding state
      const semanticSearchWrapper = async (query: string, k?: number, maxDistance?: number) => {
        if (!isEmbeddingComplete) {
          throw new Error('Embeddings not ready');
        }
        return embedding.semanticSearch(kuzu.executeQuery, query, k, maxDistance);
      };

      const semanticSearchWithContextWrapper = async (query: string, k?: number, hops?: number) => {
        if (!isEmbeddingComplete) {
          throw new Error('Embeddings not ready');
        }
        return embedding.semanticSearchWithContext(kuzu.executeQuery, query, k, hops);
      };

      // Hybrid search wrapper - combines BM25 + semantic
      const hybridSearchWrapper = async (query: string, k?: number) => {
        // Get BM25 results (always available after ingestion)
        const bm25Results = search.searchBM25(query, (k ?? 10) * 3);

        // Get semantic results if embeddings are ready
        let semanticResults: any[] = [];
        if (isEmbeddingComplete) {
          try {
            semanticResults = await embedding.semanticSearch(kuzu.executeQuery, query, (k ?? 10) * 3, 0.5);
          } catch {
            // Semantic search failed, continue with BM25 only
          }
        }

        // Merge with RRF
        return search.mergeWithRRF(bm25Results, semanticResults, k ?? 10);
      };

      // Use provided projectName, or fallback to 'project' if not provided
      const resolvedProjectName = projectName || 'project';
      if (import.meta.env.DEV) {
      }

      let codebaseContext;
      try {
        codebaseContext = await context.buildCodebaseContext(kuzu.executeQuery, resolvedProjectName);
      } catch (err) {
        console.warn('Failed to build codebase context, proceeding without:', err);
      }

      currentAgent = await createGraphRAGAgent(
        config,
        kuzu.executeQuery,
        semanticSearchWrapper,
        semanticSearchWithContextWrapper,
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
        console.error('❌ Agent initialization failed:', error);
      }
      return { success: false, error: message };
    }
  },

  /**
   * Check if the agent is initialized
   */
  isAgentReady(): boolean {
    return currentAgent !== null;
  },

  /**
   * Get current provider info
   */
  getAgentProvider(): { provider: string; model: string } | null {
    if (!currentProviderConfig) return null;
    return {
      provider: currentProviderConfig.provider,
      model: currentProviderConfig.model,
    };
  },

  /**
   * Chat with the Graph RAG agent (streaming)
   * Sends response chunks via the onChunk callback
   * @param messages - Conversation history
   * @param onChunk - Proxied callback for streaming chunks (runs on main thread)
   */
  async chatStream(
    messages: AgentMessage[],
    onChunk: (chunk: AgentStreamChunk) => void
  ): Promise<void> {
    if (!currentAgent) {
      onChunk({ type: 'error', error: 'Agent not initialized. Please configure an LLM provider first.' });
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
        // Swallow errors from cancellation
        onChunk({ type: 'done' });
        return;
      }
      const message = error instanceof Error ? error.message : String(error);
      onChunk({ type: 'error', error: message });
    }
  },

  /**
   * Stop the current chat stream
   */
  stopChat(): void {
    chatCancelled = true;
  },

  /**
   * Dispose of the current agent
   */
  disposeAgent(): void {
    currentAgent = null;
    currentProviderConfig = null;
  },

  // ============================================================
  // Snapshot Persistence Methods
  // ============================================================

  /**
   * Set the project path (for snapshot saving)
   */
  setProjectPath(path: string | null): void {
    projectPath = path;
  },

  /**
   * Load a snapshot from disk and restore all state.
   * Returns a SerializablePipelineResult if successful, null on failure.
   */
  async loadSnapshot(
    path: string,
    onProgress: (progress: PipelineProgress) => void,
  ): Promise<(SerializablePipelineResult & { hasEmbeddings: boolean }) | null> {
    const prowl = (globalThis as any).window?.prowl ?? (globalThis as any).prowl;
    if (!prowl?.snapshot) return null;

    try {
      onProgress({ phase: 'extracting', percent: 5, message: 'Reading snapshot...' });

      // Read snapshot data
      const data = await prowl.snapshot.read(path);
      if (!data) return null;

      // Read meta and verify HMAC
      onProgress({ phase: 'extracting', percent: 10, message: 'Verifying integrity...' });
      const meta = await prowl.snapshot.readMeta(path) as any;
      if (!meta?.hmac) return null;

      const valid = await prowl.snapshot.verify(data, meta.hmac);
      if (!valid) {
        console.warn('[prowl:snapshot] HMAC verification failed — full re-index needed');
        return null;
      }

      // Check format version compatibility
      if (meta.formatVersion != null) {
        const { SNAPSHOT_FORMAT_VERSION } = await getSnapshot();
        if (meta.formatVersion !== SNAPSHOT_FORMAT_VERSION) {
          console.warn(`[prowl:snapshot] Format version mismatch: ${meta.formatVersion} vs ${SNAPSHOT_FORMAT_VERSION} — full re-index`);
          return null;
        }
      }

      // Check app version compatibility
      const prowlVersion = (import.meta.env.VITE_APP_VERSION as string) || 'unknown';
      if (meta.prowlVersion && prowlVersion !== 'unknown' && meta.prowlVersion !== prowlVersion) {
        console.warn(`[prowl:snapshot] Version mismatch: ${meta.prowlVersion} vs ${prowlVersion} — full re-index`);
        return null;
      }

      // Deserialize
      onProgress({ phase: 'structure', percent: 20, message: 'Deserializing snapshot...' });
      const { deserializeSnapshot } = await getSnapshot();
      const payload = await deserializeSnapshot(data);

      // Restore graph
      onProgress({ phase: 'parsing', percent: 40, message: 'Restoring graph...' });
      const { restoreGraphFromPayload, restoreFileContents } = await getSnapshot();
      const graph = restoreGraphFromPayload(payload);
      const fileContentsMap = restoreFileContents(payload);

      // Set worker state
      storedFileContents = fileContentsMap;
      currentGraphResult = {
        graph,
        fileContents: fileContentsMap,
        communityResult: { communities: [], memberships: [], stats: { totalCommunities: 0, modularity: 0, nodesProcessed: 0 } },
        processResult: { processes: [], steps: [], stats: { totalProcesses: 0, crossCommunityCount: 0, avgStepCount: 0, entryPointsFound: 0 } },
      };
      projectPath = path;

      // Rebuild BM25 from file contents (fast, ~100ms)
      onProgress({ phase: 'imports', percent: 60, message: 'Rebuilding search index...' });
      const { buildBM25Index } = await getSearch();
      buildBM25Index(storedFileContents);

      // Restore KuzuDB
      onProgress({ phase: 'calls', percent: 70, message: 'Restoring graph database...' });
      try {
        const { restoreKuzuFromSnapshot } = await import('../core/snapshot/kuzu-restorer');
        await restoreKuzuFromSnapshot(payload);
      } catch (err) {
        console.warn('[prowl:snapshot] KuzuDB restore failed (non-fatal):', err);
      }

      // Set embedding state and warm model in background
      const hasEmbeddings = payload.embeddings.length > 0;
      if (hasEmbeddings) {
        isEmbeddingComplete = true;
        // Non-blocking: warm the embedding model so semantic search has zero cold-start
        warmEmbeddingModel().catch(console.warn);
      }

      onProgress({
        phase: 'complete',
        percent: 100,
        message: `Loaded from cache! ${payload.meta.nodeCount} nodes, ${payload.meta.relationshipCount} edges`,
        stats: {
          filesProcessed: payload.meta.fileCount,
          totalFiles: payload.meta.fileCount,
          nodesCreated: payload.meta.nodeCount,
        },
      });

      return {
        nodes: payload.nodes,
        relationships: payload.relationships,
        fileContents: payload.fileContents,
        hasEmbeddings,
      };
    } catch (err) {
      console.warn('[prowl:snapshot] Load failed:', err);
      return null;
    }
  },

  /**
   * Apply an incremental update based on file changes.
   * Reads changed files from disk via IPC, updates the graph, and returns updated result.
   */
  async incrementalUpdate(
    diff: { added: string[]; modified: string[]; deleted: string[]; isGitRepo: boolean },
    folderPath: string,
    onProgress: (progress: PipelineProgress) => void,
  ): Promise<SerializablePipelineResult | null> {
    if (!currentGraphResult) return null;

    const prowl = (globalThis as any).window?.prowl ?? (globalThis as any).prowl;
    if (!prowl?.fs?.readFile) return null;

    try {
      // Read changed/added files from disk
      const newFileContents = new Map<string, string>();
      const filesToRead = [...diff.added, ...diff.modified];
      for (const filePath of filesToRead) {
        try {
          const content = await prowl.fs.readFile(`${folderPath}/${filePath}`);
          newFileContents.set(filePath, content);
        } catch {
          // File might be binary or unreadable — skip
        }
      }

      const { applyIncrementalUpdate } = await import('../core/snapshot/incremental-updater');
      const result = await applyIncrementalUpdate(
        diff,
        newFileContents,
        currentGraphResult.graph,
        currentGraphResult.fileContents,
        onProgress,
      );

      // Update worker state
      currentGraphResult = result;
      storedFileContents = result.fileContents;

      // Rebuild BM25
      const { buildBM25Index } = await getSearch();
      buildBM25Index(storedFileContents);

      // Reload KuzuDB
      try {
        const kuzu = await getKuzuAdapter();
        await kuzu.loadGraphToKuzu(result.graph, result.fileContents);
      } catch {
        // KuzuDB is optional
      }

      return serializePipelineResult(result);
    } catch (err) {
      console.warn('[prowl:snapshot] Incremental update failed:', err);
      return null;
    }
  },

  /**
   * Save the current project state as a snapshot to disk.
   * Requires projectPath to be set and a graph to be loaded.
   */
  async saveSnapshot(path: string): Promise<{ success: boolean; size: number }> {
    if (!currentGraphResult) {
      return { success: false, size: 0 };
    }

    const { saveProjectSnapshot } = await getSnapshot();
    let kuzuQueryFn: ((cypher: string) => Promise<any[]>) | undefined;

    try {
      const kuzu = await getKuzuAdapter();
      if (kuzu.isKuzuReady()) {
        kuzuQueryFn = kuzu.executeQuery;
      }
    } catch { /* no kuzu */ }

    // Get prowl version from package.json (injected by Vite as env)
    const prowlVersion = (import.meta.env.VITE_APP_VERSION as string) || 'unknown';

    const projectName = path.split('/').filter(Boolean).pop() || 'project';

    const result = await saveProjectSnapshot(
      path,
      currentGraphResult.graph,
      currentGraphResult.fileContents,
      projectName,
      prowlVersion,
      kuzuQueryFn,
    );

    return { success: result.success, size: result.size };
  },

  /**
   * Enrich community clusters using LLM
   */
  async enrichCommunities(
    providerConfig: ProviderConfig,
    onProgress: (current: number, total: number) => void
  ): Promise<{ enrichments: Record<string, ClusterEnrichment>, tokensUsed: number }> {
    if (!currentGraphResult) {
      throw new Error('No graph loaded. Please ingest a repository first.');
    }

    const { graph } = currentGraphResult;
    
    // Filter for community nodes
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

    // Build member map: CommunityID -> Member Info
    const memberMap = new Map<string, ClusterMemberInfo[]>();
    
    // Initialize map
    communityNodes.forEach(c => memberMap.set(c.id, []));
    
    // Find all MEMBER_OF edges
    graph.relationships.forEach(rel => {
      if (rel.type === 'MEMBER_OF') {
        const communityId = rel.targetId;
        const memberId = rel.sourceId; // MEMBER_OF goes Member -> Community
        
        if (memberMap.has(communityId)) {
          // Find member node details
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

    // Create LLM client adapter for LangChain model
    const { createChatModel } = await getAgent();
    const { SystemMessage } = await getLangCore();
    const { enrichClustersBatch } = await getEnricher();
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

    // Run enrichment
    const { enrichments, tokensUsed } = await enrichClustersBatch(
      communityNodes,
      memberMap,
      llmClient,
      5, // Batch size
      onProgress
    );

    if (import.meta.env.DEV) {
    }

    // Update graph nodes with enrichment data
    graph.nodes.forEach(node => {
      if (node.label === 'Community' && enrichments.has(node.id)) {
        const enrichment = enrichments.get(node.id)!;
        node.properties.name = enrichment.name; // Update display label
        node.properties.keywords = enrichment.keywords;
        node.properties.description = enrichment.description;
        node.properties.enrichedBy = 'llm';
      }
    });

    // Update KuzuDB with new data
    try {
      const kuzu = await getKuzuAdapter();
        
      onProgress(enrichments.size, enrichments.size); // Done
      
      // Update one by one via Cypher (simplest for now)
      for (const [id, enrichment] of enrichments.entries()) {
         // Escape strings for Cypher - replace backslash first, then quotes
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
      console.error('Failed to update KuzuDB with enrichment:', err);
    }
    
    // Convert Map to Record for serialization
    const enrichmentsRecord: Record<string, ClusterEnrichment> = {};
    for (const [id, val] of enrichments.entries()) {
      enrichmentsRecord[id] = val;
    }
     
    return { enrichments: enrichmentsRecord, tokensUsed };
  
  },
};

// Expose the worker API to the main thread
Comlink.expose(workerApi);

// TypeScript type for the exposed API (used by the hook)
export type IngestionWorkerApi = typeof workerApi;

