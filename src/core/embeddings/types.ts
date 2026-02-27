/* Labels eligible for vector encoding */
export const VECTORIZABLE_TYPES = [
  'Class',
  'Function',
  'Interface',
  'Method',
  'File',
] as const;

export type EmbeddableLabel = (typeof VECTORIZABLE_TYPES)[number];

/** Returns true when the label should receive an embedding vector. */
export function isEmbeddableLabel(label: string): label is EmbeddableLabel {
  return (VECTORIZABLE_TYPES as readonly string[]).includes(label);
}

/* ── Lifecycle ────────────────────────────────────────── */

export type EmbeddingPhase =
  | 'idle'
  | 'loading-model'
  | 'embedding'
  | 'indexing'
  | 'ready'
  | 'error';

/** Snapshot of how far the embedding pipeline has progressed. */
export interface EmbeddingProgress {
  phase: EmbeddingPhase;
  percent: number;
  modelDownloadPercent?: number;
  nodesProcessed?: number;
  totalNodes?: number;
  currentBatch?: number;
  totalBatches?: number;
  error?: string;
}

/* ── Configuration ────────────────────────────────────── */

export interface EmbeddingConfig {
  /** transformers.js model handle */
  modelId: string;
  /** how many nodes per inference batch */
  batchSize: number;
  /** output vector width */
  dimensions: number;
  /** runtime backend — prefer GPU when available */
  device: 'webgpu' | 'wasm';
  /** truncate source snippets beyond this length */
  maxSnippetLength: number;
}

export const DEFAULT_EMBEDDING_CONFIG: EmbeddingConfig = {
  modelId: 'Snowflake/snowflake-arctic-embed-xs',
  batchSize: 64,
  dimensions: 384,
  device: 'wasm',
  maxSnippetLength: 500,
};

/* ── Reranker configuration ───────────────────────────── */

export interface RerankerConfig {
  modelId: string;
  device: 'webgpu' | 'wasm';
}

export const DEFAULT_RERANKER_CONFIG: RerankerConfig = {
  modelId: 'jinaai/jina-reranker-v1-tiny-en',
  device: 'wasm',
};

/* ── Data shapes ──────────────────────────────────────── */

/** Nearest-neighbour hit returned by the vector index. */
export interface SemanticSearchResult {
  nodeId: string;
  name: string;
  label: string;
  filePath: string;
  distance: number;
  startLine?: number;
  endLine?: number;
}

/** Minimal node payload sent into the embedding pipeline. */
export interface EmbeddableNode {
  id: string;
  name: string;
  label: string;
  filePath: string;
  content: string;
  startLine?: number;
  endLine?: number;
}

/** Progress callback shape emitted by transformers.js during download. */
export interface ModelProgress {
  status: 'initiate' | 'download' | 'progress' | 'done' | 'ready';
  file?: string;
  progress?: number;
  loaded?: number;
  total?: number;
}
