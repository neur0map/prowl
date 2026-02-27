import { Brain, FlaskConical } from 'lucide-react';
import { useAppState } from '../hooks/useAppState';
import { useState, type ReactNode } from 'react';
import { WebGPUFallbackDialog } from './WebGPUFallbackDialog';

/* ── Tiny inline progress bar ──────────────────────── */

function MicroBar({ percent, color }: { percent: number; color: string }) {
  return (
    <div className="w-12 h-[2px] rounded-full overflow-hidden bg-white/[0.08]">
      <div
        className={`h-full ${color} rounded-full transition-all duration-300`}
        style={{ width: `${Math.max(percent, 2)}%` }}
      />
    </div>
  );
}

/* ── Pulsing dot indicator ─────────────────────────── */

function PulsingDot({ color }: { color: string }) {
  return <span className={`w-1.5 h-1.5 rounded-full ${color} animate-pulse`} />;
}

/* ── Per-status views (all mono, 11px, flat) ───────── */

function idleView(
  onStart: () => void,
  onTest: () => void,
  testOutput: string | null,
) {
  return (
    <div className="flex items-center gap-2">
      {import.meta.env.DEV && (
        <button
          onClick={onTest}
          className="flex items-center gap-1 px-1.5 py-0.5 rounded text-[11px] font-mono text-text-muted hover:text-text-secondary transition-colors"
          title="Run KuzuDB array parameter diagnostic"
        >
          <FlaskConical size={10} />
          {testOutput || 'test'}
        </button>
      )}
      <button
        onClick={onStart}
        className="flex items-center gap-1.5 px-1.5 py-0.5 rounded text-[11px] font-mono text-text-muted hover:text-text-secondary transition-colors"
        title="Build vector index for semantic search"
      >
        <Brain size={11} className="opacity-50" />
        <span>vectors</span>
      </button>
    </div>
  );
}

function loadingView(downloadPct: number) {
  return (
    <div
      className="flex items-center gap-1.5 px-1.5 py-0.5 text-[11px] font-mono text-text-muted"
      title="Downloading and compiling AI model..."
    >
      <PulsingDot color="bg-accent/70" />
      <span>loading model</span>
      <MicroBar percent={downloadPct} color="bg-accent/60" />
    </div>
  );
}

function embeddingView(done: number, total: number, pct: number) {
  return (
    <div
      className="flex items-center gap-1.5 px-1.5 py-0.5 text-[11px] font-mono text-text-muted"
      title={`Embedding ${done}/${total} nodes (${Math.round(pct)}%)`}
    >
      <PulsingDot color="bg-emerald-400/70" />
      <span>embedding {done}/{total}</span>
      <MicroBar percent={pct} color="bg-emerald-400/50" />
    </div>
  );
}

function indexingView() {
  return (
    <div
      className="flex items-center gap-1.5 px-1.5 py-0.5 text-[11px] font-mono text-text-muted"
      title="Creating vector index..."
    >
      <PulsingDot color="bg-violet-400/70" />
      <span>indexing</span>
    </div>
  );
}

function readyView() {
  return (
    <div
      className="flex items-center gap-1.5 px-1.5 py-0.5 text-[11px] font-mono text-text-muted"
      title="Vector index built. Use natural language in the AI chat."
    >
      <span className="w-1.5 h-1.5 rounded-full bg-emerald-400/70" />
      semantic
    </div>
  );
}

function errorView(onRetry: () => void, errMsg?: string) {
  return (
    <button
      onClick={onRetry}
      className="flex items-center gap-1.5 px-1.5 py-0.5 rounded text-[11px] font-mono text-red-400/70 hover:text-red-400 transition-colors"
      title={errMsg || 'Indexing failed. Click to retry.'}
    >
      <span className="w-1.5 h-1.5 rounded-full bg-red-400/70" />
      failed · retry
    </button>
  );
}

/* ── Main component ─────────────────────────────────── */

export const EmbeddingStatus = () => {
  const {
    embeddingStatus,
    embeddingProgress,
    startEmbeddings,
    graph,
    viewMode,
    testArrayParams,
  } = useAppState();

  const [testOutput, setTestOutput] = useState<string | null>(null);
  const [showFallback, setShowFallback] = useState(false);

  if (viewMode !== 'exploring' || !graph) return null;

  const totalNodes = graph.nodes.length;

  const launchEmbeddings = async (device?: 'webgpu' | 'wasm') => {
    try {
      await startEmbeddings(device);
    } catch (err: any) {
      if (err?.name === 'WebGPUNotAvailableError' || err?.message?.includes('WebGPU not available')) {
        setShowFallback(true);
      } else {
        console.error('Embedding failed:', err);
      }
    }
  };

  const handleCpuFallback = () => {
    setShowFallback(false);
    launchEmbeddings('wasm');
  };

  const handleSkipFallback = () => { setShowFallback(false); };

  const runArrayTest = async () => {
    setTestOutput('testing...');
    const result = await testArrayParams();
    if (result.success) {
      setTestOutput('ok');
    } else {
      setTestOutput('fail');
      console.error('[prowl:embedding] array params test failed:', result.error);
    }
  };

  const dialog = (
    <WebGPUFallbackDialog
      isOpen={showFallback}
      onClose={() => setShowFallback(false)}
      onUseCPU={handleCpuFallback}
      onSkip={handleSkipFallback}
      nodeCount={totalNodes}
    />
  );

  const viewForStatus: Record<string, () => ReactNode> = {
    idle: () => idleView(() => launchEmbeddings(), runArrayTest, testOutput),
    loading: () => loadingView(embeddingProgress?.modelDownloadPercent ?? 0),
    embedding: () => embeddingView(
      embeddingProgress?.nodesProcessed ?? 0,
      embeddingProgress?.totalNodes ?? 0,
      embeddingProgress?.percent ?? 0,
    ),
    indexing: indexingView,
    ready: readyView,
    error: () => errorView(() => launchEmbeddings(), embeddingProgress?.error),
  };

  const render = viewForStatus[embeddingStatus];
  if (!render) return null;

  const content = render();
  const showDialog = embeddingStatus === 'idle' || embeddingStatus === 'loading' || embeddingStatus === 'error';

  if (showDialog) {
    return <>{content}{dialog}</>;
  }

  return <>{content}</>;
};
