import { pipeline, env, type FeatureExtractionPipeline } from '@huggingface/transformers';
import { DEFAULT_EMBEDDING_CONFIG, type EmbeddingConfig, type ModelProgress } from './types';

/* ── Runtime singleton ───────────────────────────────── */

const runtime = {
  instance: null as FeatureExtractionPipeline | null,
  loading: false,
  pending: null as Promise<FeatureExtractionPipeline> | null,
  activeDevice: null as 'webgpu' | 'wasm' | null,
};

/* ── Types ───────────────────────────────────────────── */

export type ModelProgressCallback = (progress: ModelProgress) => void;

export class WebGPUNotAvailableError extends Error {
  constructor(originalError?: Error) {
    super('WebGPU not available in this browser');
    this.name = 'WebGPUNotAvailableError';
    this.cause = originalError;
  }
}

/* ── GPU capability test ─────────────────────────────── */

async function testGPUCapability(): Promise<boolean> {
  try {
    const nav = navigator as any;
    if (!nav.gpu) return false;
    const adapter = await nav.gpu.requestAdapter();
    if (!adapter) return false;
    const gpuDevice = await adapter.requestDevice();
    gpuDevice.destroy();
    return true;
  } catch {
    return false;
  }
}

export const checkWebGPUAvailability = testGPUCapability;

/* ── Device accessor ─────────────────────────────────── */

export function getCurrentDevice(): 'webgpu' | 'wasm' | null {
  return runtime.activeDevice;
}

/* ── Environment setup ───────────────────────────────── */

let staleCacheCleared = false;

function setupEnv(): void {
  env.allowLocalModels = false;
  env.allowRemoteModels = true;
  /* Disable all caching — Cache API and FS cache have known conflicts
     in Electron Web Workers (IS_NODE_ENV misdetection). Models download
     directly to memory on each session (~23 MB for arctic-embed-xs). */
  env.useBrowserCache = false;
  env.useFSCache = false;
  env.useCustomCache = false;
  env.logLevel = import.meta.env.DEV ? 'info' : 'error';

  /* Purge stale Cache API entries from prior sessions (one-time) */
  if (!staleCacheCleared && typeof caches !== 'undefined') {
    staleCacheCleared = true;
    caches.delete('transformers-cache').catch(() => {});
  }
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

/** If no progress events fire for this long, assume the pipeline hung. */
const IDLE_TIMEOUT_MS = 120_000;

async function instantiatePipeline(
  modelId: string,
  device: 'webgpu' | 'wasm',
  progressCb?: (data: any) => void,
): Promise<FeatureExtractionPipeline> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  let rejectFn: ((err: Error) => void) | undefined;

  /* Reset the idle timer every time transformers.js emits any event */
  const resetTimer = () => {
    clearTimeout(timer);
    timer = setTimeout(() => {
      rejectFn?.(new Error(`Model loading stalled — no progress for ${IDLE_TIMEOUT_MS / 1000}s (device=${device})`));
    }, IDLE_TIMEOUT_MS);
  };

  const wrappedCb = (data: any) => {
    resetTimer();
    progressCb?.(data);
  };

  resetTimer(); /* start the initial idle clock */

  const result = await Promise.race([
    (pipeline as any)('feature-extraction', modelId, {
      device,
      dtype: 'fp32',
      progress_callback: wrappedCb,
    }),
    new Promise<never>((_, reject) => { rejectFn = reject; }),
  ]);

  clearTimeout(timer);
  return result;
}

/* ── Backend boot helpers ────────────────────────────── */

async function bootWithGPU(
  modelId: string,
  progressCb?: (data: any) => void,
): Promise<FeatureExtractionPipeline> {
  if (import.meta.env.DEV) {
    console.log('[prowl:embedder] probing WebGPU support');
  }

  const gpuOk = await testGPUCapability();

  if (!gpuOk) {
    if (import.meta.env.DEV) {
      console.warn('[prowl:embedder] WebGPU unavailable on this device');
    }
    throw new WebGPUNotAvailableError();
  }

  try {
    if (import.meta.env.DEV) {
      console.log('[prowl:embedder] starting WebGPU backend');
    }
    const inst = await instantiatePipeline(modelId, 'webgpu', progressCb);
    runtime.activeDevice = 'webgpu';
    if (import.meta.env.DEV) {
      console.log('[prowl:embedder] WebGPU backend active');
    }
    return inst;
  } catch (gpuErr) {
    if (import.meta.env.DEV) {
      console.warn('[prowl:embedder] WebGPU boot failed:', gpuErr);
    }
    throw new WebGPUNotAvailableError(gpuErr as Error);
  }
}

async function bootWithWasm(
  modelId: string,
  progressCb?: (data: any) => void,
): Promise<FeatureExtractionPipeline> {
  if (import.meta.env.DEV) {
    console.log('[prowl:embedder] starting WASM backend');
  }
  const inst = await instantiatePipeline(modelId, 'wasm', progressCb);
  runtime.activeDevice = 'wasm';
  if (import.meta.env.DEV) {
    console.log('[prowl:embedder] WASM backend active');
  }
  return inst;
}

/* ── Model initialisation ────────────────────────────── */

export async function initEmbedder(
  onProgress?: ModelProgressCallback,
  config: Partial<EmbeddingConfig> = {},
  forceDevice?: 'webgpu' | 'wasm',
): Promise<FeatureExtractionPipeline> {
  if (runtime.instance) return runtime.instance;

  if (runtime.loading && runtime.pending) return runtime.pending;

  runtime.loading = true;

  const merged = { ...DEFAULT_EMBEDDING_CONFIG, ...config };
  const targetDevice = forceDevice || merged.device;

  runtime.pending = (async () => {
    try {
      setupEnv();

      if (import.meta.env.DEV) {
        console.log(`[prowl:embedder] fetching model: ${merged.modelId}`);
      }

      const wrappedCb = bridgeProgressCb(onProgress);

      if (targetDevice === 'webgpu') {
        try {
          runtime.instance = await bootWithGPU(merged.modelId, wrappedCb);
        } catch (gpuErr) {
          /* WebGPU failed or timed out — fall back to WASM automatically */
          if (import.meta.env.DEV) {
            console.warn('[prowl:embedder] WebGPU failed, falling back to WASM:', gpuErr);
          }
          runtime.instance = await bootWithWasm(merged.modelId, wrappedCb);
        }
      } else {
        runtime.instance = await bootWithWasm(merged.modelId, wrappedCb);
      }

      if (import.meta.env.DEV) {
        console.log(`[prowl:embedder] model ready (${runtime.activeDevice})`);
      }

      return runtime.instance!;
    } catch (err) {
      /* Reset all state on any failure so subsequent calls start fresh */
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

export function isEmbedderReady(): boolean {
  return runtime.instance !== null;
}

export function getEmbedder(): FeatureExtractionPipeline {
  if (!runtime.instance) {
    throw new Error('Embedding pipeline not ready. Initialize with initEmbedder() before use.');
  }
  return runtime.instance;
}

/* ── Single-text embedding ───────────────────────────── */

export async function embedText(text: string): Promise<Float32Array> {
  const emb = getEmbedder();
  const tensor = await emb(text, { pooling: 'mean', normalize: true });
  return new Float32Array(tensor.data as ArrayLike<number>);
}

/* ── Batch embedding ─────────────────────────────────── */

export async function embedBatch(texts: string[]): Promise<Float32Array[]> {
  if (texts.length === 0) return [];

  const emb = getEmbedder();
  const tensor = await emb(texts, { pooling: 'mean', normalize: true });

  const rawData = tensor.data as ArrayLike<number>;
  const dim = DEFAULT_EMBEDDING_CONFIG.dimensions;
  const vectors: Float32Array[] = [];

  for (let i = 0; i < texts.length; i++) {
    const offset = i * dim;
    vectors.push(new Float32Array(Array.prototype.slice.call(rawData, offset, offset + dim)));
  }

  return vectors;
}

/* ── Conversion helper ───────────────────────────────── */

export function embeddingToArray(embedding: Float32Array): number[] {
  return Array.from(embedding);
}

/* ── Teardown ────────────────────────────────────────── */

/** Hard-reset all runtime state. Call before loading a new project to
 *  prevent stale promises / loading flags from a prior session. */
export function resetEmbedderState(): void {
  runtime.instance = null;
  runtime.loading = false;
  runtime.pending = null;
  runtime.activeDevice = null;
}

export async function disposeEmbedder(): Promise<void> {
  const inst = runtime.instance;
  /* Reset all flags immediately so concurrent callers don't see stale state */
  resetEmbedderState();
  if (inst) {
    try {
      if ('dispose' in inst && typeof inst.dispose === 'function') {
        await inst.dispose();
      }
    } catch {
      /* non-fatal disposal failure */
    }
  }
}
