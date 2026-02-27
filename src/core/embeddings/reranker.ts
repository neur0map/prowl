import { pipeline, env } from '@huggingface/transformers';
import { DEFAULT_RERANKER_CONFIG, type RerankerConfig, type ModelProgress } from './types';
import { checkWebGPUAvailability, type ModelProgressCallback } from './embedder';

/* ── Runtime singleton ───────────────────────────────── */

const runtime = {
  instance: null as any | null,
  loading: false,
  pending: null as Promise<any> | null,
  activeDevice: null as 'webgpu' | 'wasm' | null,
};

/* ── Environment setup ───────────────────────────────── */

function setupEnv(): void {
  env.allowLocalModels = false;
  env.allowRemoteModels = true;
  env.useBrowserCache = false;
  env.useFSCache = false;
  env.useCustomCache = false;
  env.logLevel = 'error';
}

function bridgeProgressCb(
  onProgress?: ModelProgressCallback,
): ((data: any) => void) | undefined {
  if (!onProgress) return undefined;
  return (data: any) => {
    onProgress({
      status: data.status || 'progress',
      file: data.file,
      progress: data.progress,
      loaded: data.loaded,
      total: data.total,
    });
  };
}

/* ── Model initialisation ────────────────────────────── */

export async function initReranker(
  onProgress?: ModelProgressCallback,
  config: Partial<RerankerConfig> = {},
  forceDevice?: 'webgpu' | 'wasm',
): Promise<any> {
  if (runtime.instance) return runtime.instance;
  if (runtime.loading && runtime.pending) return runtime.pending;

  runtime.loading = true;

  const merged = { ...DEFAULT_RERANKER_CONFIG, ...config };
  const targetDevice = forceDevice || merged.device;

  runtime.pending = (async () => {
    try {
      setupEnv();

      if (import.meta.env.DEV) {
        console.log(`[prowl:reranker] fetching model: ${merged.modelId}`);
      }

      const wrappedCb = bridgeProgressCb(onProgress);
      let device = targetDevice;

      if (device === 'webgpu') {
        const gpuOk = await checkWebGPUAvailability();
        if (!gpuOk) {
          if (import.meta.env.DEV) {
            console.warn('[prowl:reranker] WebGPU unavailable, falling back to WASM');
          }
          device = 'wasm';
        }
      }

      runtime.instance = await (pipeline as any)(
        'text-classification',
        merged.modelId,
        {
          device,
          dtype: 'fp32',
          progress_callback: wrappedCb,
        },
      );
      runtime.activeDevice = device;

      if (import.meta.env.DEV) {
        console.log(`[prowl:reranker] model ready (${device})`);
      }

      return runtime.instance!;
    } catch (err) {
      runtime.instance = null;
      runtime.pending = null;
      throw err;
    } finally {
      runtime.loading = false;
    }
  })();

  return runtime.pending;
}

/* ── Status checks ───────────────────────────────────── */

export function isRerankerReady(): boolean {
  return runtime.instance !== null;
}

export function getRerankerDevice(): 'webgpu' | 'wasm' | null {
  return runtime.activeDevice;
}

/* ── Reranking ───────────────────────────────────────── */

export interface RerankResult {
  index: number;
  score: number;
}

export async function rerank(
  query: string,
  documents: string[],
): Promise<RerankResult[]> {
  if (!runtime.instance) {
    throw new Error('Reranker not ready. Initialize with initReranker() first.');
  }
  if (documents.length === 0) return [];

  const pairs = documents.map((doc) => ({
    text: query,
    text_pair: doc,
  }));

  const scores: RerankResult[] = [];

  for (let i = 0; i < pairs.length; i++) {
    const result = await runtime.instance(pairs[i].text, {
      text_pair: pairs[i].text_pair,
      top_k: null,
    });

    // Cross-encoders return [{ label, score }, ...] per pair
    // We want the score of the positive/relevant class
    let score = 0;
    if (Array.isArray(result) && result.length > 0) {
      // Pick the highest score (typically the relevance score)
      score = result[0].score ?? 0;
    }

    scores.push({ index: i, score });
  }

  scores.sort((a, b) => b.score - a.score);
  return scores;
}

/* ── Teardown ────────────────────────────────────────── */

export async function disposeReranker(): Promise<void> {
  if (runtime.instance) {
    try {
      if ('dispose' in runtime.instance && typeof runtime.instance.dispose === 'function') {
        await runtime.instance.dispose();
      }
    } catch {
      /* non-fatal disposal failure */
    }
    runtime.instance = null;
    runtime.pending = null;
    runtime.activeDevice = null;
  }
}
