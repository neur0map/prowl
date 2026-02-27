import { Brain, Download, Cpu, CheckCircle } from 'lucide-react';
import { useAppState } from '../hooks/useAppState';

/**
 * Floating banner shown over the graph viewport while the embedding
 * pipeline is actively working (downloading model / embedding nodes).
 * Replaces the tiny 11px status-bar indicator with something the user
 * can actually notice.
 */
export function EmbeddingBanner() {
  const { embeddingStatus, embeddingProgress, viewMode, graph } = useAppState();

  if (viewMode !== 'exploring' || !graph) return null;

  /* Only render during active phases */
  if (
    embeddingStatus !== 'loading' &&
    embeddingStatus !== 'embedding' &&
    embeddingStatus !== 'indexing'
  )
    return null;

  const percent = embeddingProgress?.percent ?? 0;

  /* Phase-specific content */
  let icon: React.ReactNode;
  let label: string;
  let detail: string;
  let barColor: string;

  switch (embeddingStatus) {
    case 'loading': {
      const dlPct = embeddingProgress?.modelDownloadPercent ?? 0;
      icon = <Download size={14} className="text-accent animate-pulse" />;
      label = 'Downloading AI model';
      detail = dlPct > 0 ? `${Math.round(dlPct)}%` : 'connecting...';
      barColor = 'bg-accent/70';
      break;
    }
    case 'embedding': {
      const done = embeddingProgress?.nodesProcessed ?? 0;
      const total = embeddingProgress?.totalNodes ?? 0;
      icon = <Cpu size={14} className="text-emerald-400 animate-pulse" />;
      label = 'Embedding code';
      detail = total > 0 ? `${done} / ${total} symbols` : 'preparing...';
      barColor = 'bg-emerald-400/70';
      break;
    }
    case 'indexing':
      icon = <CheckCircle size={14} className="text-violet-400 animate-pulse" />;
      label = 'Building search index';
      detail = 'almost done';
      barColor = 'bg-violet-400/70';
      break;
    default:
      return null;
  }

  return (
    <div className="absolute top-3 left-1/2 -translate-x-1/2 z-40 animate-slide-up pointer-events-auto">
      <div className="glass-elevated rounded-xl px-4 py-2.5 flex items-center gap-3 shadow-lg min-w-[260px]">
        {icon}

        <div className="flex flex-col gap-1 flex-1 min-w-0">
          <div className="flex items-center justify-between gap-3">
            <span className="text-[12px] font-medium text-text-primary truncate">
              {label}
            </span>
            <span className="text-[11px] font-mono text-text-muted whitespace-nowrap">
              {detail}
            </span>
          </div>

          {/* Progress bar */}
          <div className="h-[3px] w-full rounded-full overflow-hidden bg-white/[0.08]">
            <div
              className={`h-full ${barColor} rounded-full transition-all duration-500 ease-out`}
              style={{ width: `${Math.max(percent, 2)}%` }}
            />
          </div>
        </div>
      </div>
    </div>
  );
}
