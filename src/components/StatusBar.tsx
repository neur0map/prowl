import { Terminal, PanelRight } from 'lucide-react';
import { useAppState } from '../hooks/useAppState';
import { EmbeddingStatus } from './EmbeddingStatus';

/* ── Pipeline status indicator ─────────────────────── */

const PHASE_LABELS: Record<string, string> = {
  extracting: 'extracting',
  structure: 'resolving',
  parsing: 'parsing',
  imports: 'imports',
  calls: 'calls',
  heritage: 'heritage',
  communities: 'clustering',
  processes: 'processes',
  enriching: 'enriching',
};

function PipelineStatus() {
  const { progress } = useAppState();
  if (!progress || progress.phase === 'idle') return null;

  if (progress.phase === 'error') {
    return (
      <div
        className="flex items-center gap-1.5 px-1.5 py-0.5 text-[11px] font-mono text-red-400/70"
        title={progress.message}
      >
        <span className="w-1.5 h-1.5 rounded-full bg-red-400/70" />
        error
      </div>
    );
  }

  if (progress.phase === 'complete') {
    return (
      <div
        className="flex items-center gap-1.5 px-1.5 py-0.5 text-[11px] font-mono text-text-muted"
        title={progress.message}
      >
        <span className="w-1.5 h-1.5 rounded-full bg-emerald-400/70" />
        indexed
      </div>
    );
  }

  const label = PHASE_LABELS[progress.phase] ?? progress.phase;
  return (
    <div
      className="flex items-center gap-1.5 px-1.5 py-0.5 text-[11px] font-mono text-text-muted"
      title={progress.message}
    >
      <span className="w-1.5 h-1.5 rounded-full bg-accent/70 animate-pulse" />
      <span>{label}</span>
      <div className="w-12 h-[2px] rounded-full overflow-hidden bg-white/[0.08]">
        <div
          className="h-full bg-accent/60 rounded-full transition-all duration-300"
          style={{ width: `${Math.max(progress.percent, 2)}%` }}
        />
      </div>
    </div>
  );
}

/* ── Status bar ──────────────────────────────────────── */

interface StatusBarProps {
  isTerminalOpen?: boolean;
  onTerminalToggle?: () => void;
  onOpenRoadmap?: () => void;
}

export const StatusBar = ({ isTerminalOpen, onTerminalToggle, onOpenRoadmap }: StatusBarProps) => {
  const { graph, agentWatcherState, isRightPanelOpen, setRightPanelOpen, isLiveUpdating } = useAppState();

  const nodeCount = graph?.nodes.length ?? 0;
  const edgeCount = graph?.relationships.length ?? 0;

  return (
    <footer className="flex items-center justify-between px-3 h-8 border-t border-white/[0.06] text-[11px] text-text-muted font-mono bg-void">
      <div className="flex items-center gap-3">
        {agentWatcherState.isConnected ? (
          <div className="flex items-center gap-1.5">
            <span className="w-2 h-2 rounded-full bg-violet-400/70" />
            <span className="text-violet-300/70">linked</span>
          </div>
        ) : (
          <span className="opacity-50">prowl</span>
        )}

        {graph && (
          <>
            <span>{nodeCount} symbols</span>
            <span>{edgeCount} edges</span>
          </>
        )}

        {isLiveUpdating && (
          <div className="flex items-center gap-1.5">
            <span className="w-1.5 h-1.5 rounded-full bg-accent/70 animate-pulse" />
            <span className="text-accent/70">syncing</span>
          </div>
        )}
      </div>

      <div className="flex items-center gap-2">
        <PipelineStatus />
        <EmbeddingStatus />

        {/* Social links */}
        <button
          onClick={() => {
            const url = 'https://x.com/neur0map';
            (window as any).prowl?.oauth?.openExternal?.(url) ?? window.open(url);
          }}
          className="p-1 rounded text-text-muted hover:text-text-secondary transition-colors"
          title="@neur0map on X"
        >
          <svg width="12" height="12" viewBox="0 0 24 24" fill="currentColor">
            <path d="M18.244 2.25h3.308l-7.227 8.26 8.502 11.24H16.17l-5.214-6.817L4.99 21.75H1.68l7.73-8.835L1.254 2.25H8.08l4.713 6.231zm-1.161 17.52h1.833L7.084 4.126H5.117z" />
          </svg>
        </button>
        <button
          onClick={() => {
            const url = 'https://github.com/neur0map';
            (window as any).prowl?.oauth?.openExternal?.(url) ?? window.open(url);
          }}
          className="p-1 rounded text-text-muted hover:text-text-secondary transition-colors"
          title="neur0map on GitHub"
        >
          <svg width="12" height="12" viewBox="0 0 24 24" fill="currentColor">
            <path d="M12 .297c-6.63 0-12 5.373-12 12 0 5.303 3.438 9.8 8.205 11.385.6.113.82-.258.82-.577 0-.285-.01-1.04-.015-2.04-3.338.724-4.042-1.61-4.042-1.61C4.422 18.07 3.633 17.7 3.633 17.7c-1.087-.744.084-.729.084-.729 1.205.084 1.838 1.236 1.838 1.236 1.07 1.835 2.809 1.305 3.495.998.108-.776.417-1.305.76-1.605-2.665-.3-5.466-1.332-5.466-5.93 0-1.31.465-2.38 1.235-3.22-.135-.303-.54-1.523.105-3.176 0 0 1.005-.322 3.3 1.23.96-.267 1.98-.399 3-.405 1.02.006 2.04.138 3 .405 2.28-1.552 3.285-1.23 3.285-1.23.645 1.653.24 2.873.12 3.176.765.84 1.23 1.91 1.23 3.22 0 4.61-2.805 5.625-5.475 5.92.42.36.81 1.096.81 2.22 0 1.606-.015 2.896-.015 3.286 0 .315.21.69.825.57C20.565 22.092 24 17.592 24 12.297c0-6.627-5.373-12-12-12" />
          </svg>
        </button>

        {/* Roadmap */}
        {onOpenRoadmap && (
          <button
            onClick={onOpenRoadmap}
            className="px-1.5 py-0.5 rounded text-text-muted hover:text-text-secondary transition-colors"
            title="Roadmap"
          >
            roadmap
          </button>
        )}

        {/* Right panel toggle */}
        <button
          onClick={() => setRightPanelOpen(!isRightPanelOpen)}
          className={`flex items-center gap-1 px-1.5 py-0.5 rounded transition-colors ${
            isRightPanelOpen
              ? 'text-accent'
              : 'text-text-muted hover:text-text-secondary'
          }`}
          title="Toggle sidebar (chat, terminal)"
        >
          <PanelRight size={12} />
          <span>sidebar</span>
        </button>

        {onTerminalToggle && (window as any).prowl?.terminal && (
          <button
            onClick={onTerminalToggle}
            className={`flex items-center gap-1 px-1.5 py-0.5 rounded transition-colors ${
              isTerminalOpen
                ? 'text-accent'
                : 'text-text-muted hover:text-text-secondary'
            }`}
            title="Terminal (Ctrl+`)"
          >
            <Terminal size={12} />
          </button>
        )}
      </div>
    </footer>
  );
};
