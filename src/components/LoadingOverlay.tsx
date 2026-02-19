import { PipelineProgress } from '../types/pipeline';

interface LoadingOverlayProps {
  progress: PipelineProgress;
}

export const LoadingOverlay = ({ progress }: LoadingOverlayProps) => {
  return (
    <div className="fixed inset-0 flex flex-col items-center justify-center bg-void z-50">
      {/* Subtle background */}
      <div className="absolute inset-0 pointer-events-none">
        <div className="absolute top-1/3 left-1/3 w-96 h-96 bg-accent/5 rounded-full blur-3xl" />
        <div className="absolute bottom-1/3 right-1/3 w-96 h-96 bg-white/[0.02] rounded-full blur-3xl" />
      </div>

      {/* Icon */}
      <div className="relative mb-10">
        <div className="w-20 h-20 flex items-center justify-center glass rounded-full">
          <span className="text-[28px] text-text-secondary">&#9671;</span>
        </div>
      </div>

      {/* Progress bar */}
      <div className="w-80 mb-4">
        <div className="h-1 bg-white/[0.08] rounded-full overflow-hidden">
          <div
            className="h-full bg-accent rounded-full transition-all duration-300 ease-out"
            style={{ width: `${progress.percent}%` }}
          />
        </div>
      </div>

      {/* Status text */}
      <div className="text-center">
        <p className="font-mono text-[13px] text-text-secondary mb-1">
          {progress.message}
        </p>
        {progress.detail && (
          <p className="font-mono text-[11px] text-text-muted truncate max-w-md">
            {progress.detail}
          </p>
        )}
      </div>

      {/* Stats */}
      {progress.stats && (
        <div className="mt-8 flex items-center gap-6 text-[11px] text-text-muted">
          <div className="flex items-center gap-2">
            <span className="w-1.5 h-1.5 bg-node-file rounded-full" />
            <span>{progress.stats.filesProcessed} / {progress.stats.totalFiles} files</span>
          </div>
          <div className="flex items-center gap-2">
            <span className="w-1.5 h-1.5 bg-node-function rounded-full" />
            <span>{progress.stats.nodesCreated} nodes</span>
          </div>
        </div>
      )}

      {/* Percent */}
      <p className="mt-4 font-mono text-3xl font-normal text-text-primary">
        {progress.percent}%
      </p>
    </div>
  );
};
