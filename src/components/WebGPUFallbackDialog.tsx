import { useState, useEffect } from 'react';
import { AlertTriangle, Snail, Rocket, SkipForward } from 'lucide-react';

interface WebGPUFallbackDialogProps {
  isOpen: boolean;
  onClose: () => void;
  onUseCPU: () => void;
  onSkip: () => void;
  nodeCount: number;
}

export const WebGPUFallbackDialog = ({
  isOpen,
  onClose,
  onUseCPU,
  onSkip,
  nodeCount,
}: WebGPUFallbackDialogProps) => {
  const [revealed, setRevealed] = useState(false);

  useEffect(() => {
    if (isOpen) {
      requestAnimationFrame(() => setRevealed(true));
    } else {
      setRevealed(false);
    }
  }, [isOpen]);

  if (!isOpen) return null;

  const cpuMinutes = Math.ceil((nodeCount * 50) / 60000);
  const compactProject = nodeCount < 200;

  return (
    <div
      className="fixed bottom-0 left-0 right-0 z-50 flex justify-center pointer-events-none"
    >
      <div
        className={`pointer-events-auto w-full max-w-lg mx-4 mb-4 bg-surface border border-border-subtle rounded-xl shadow-2xl overflow-hidden transition-all duration-300 ease-out ${
          revealed
            ? 'opacity-100 translate-y-0'
            : 'opacity-0 translate-y-full'
        }`}
        style={{ borderTopWidth: '2px', borderTopColor: 'transparent', borderImage: 'linear-gradient(to right, #f59e0b, #f97316, #f59e0b) 1' }}
      >
        <div className="px-5 py-4 flex items-start gap-3">
          <AlertTriangle className="w-5 h-5 text-amber-400 flex-shrink-0 mt-0.5" />
          <div className="flex-1 min-w-0">
            <h2 className="text-sm font-semibold text-text-primary">
              WebGPU said &ldquo;nope&rdquo;
            </h2>
            <p className="text-xs text-text-muted mt-0.5">
              Your browser doesn&apos;t support GPU acceleration.
              Vector search won&apos;t be as fast, but the graph still works fine.
            </p>
          </div>
          <button
            onClick={onClose}
            className="text-text-muted hover:text-text-primary transition-colors text-xs px-1"
            aria-label="Dismiss"
          >
            &times;
          </button>
        </div>

        <div className="px-5 pb-3 space-y-2">
          <div className="bg-elevated/50 rounded-lg p-3 border border-border-subtle">
            <ul className="space-y-1.5 text-sm text-text-muted">
              <li className="flex items-start gap-2">
                <Snail className="w-4 h-4 mt-0.5 text-amber-400 flex-shrink-0" />
                <span>
                  <strong className="text-text-secondary">Use CPU</strong> — Works but {compactProject ? 'a bit' : 'way'} slower
                  {nodeCount > 0 && (
                    <span className="text-text-muted"> (~{cpuMinutes} min for {nodeCount} nodes)</span>
                  )}
                </span>
              </li>
              <li className="flex items-start gap-2">
                <SkipForward className="w-4 h-4 mt-0.5 text-blue-400 flex-shrink-0" />
                <span>
                  <strong className="text-text-secondary">Skip it</strong> — Graph works, just no AI semantic search
                </span>
              </li>
            </ul>
          </div>

          {compactProject && (
            <p className="text-xs text-node-function flex items-center gap-1.5 bg-node-function/10 px-3 py-2 rounded-lg">
              <Rocket className="w-3.5 h-3.5" />
              Small codebase detected! CPU should be fine.
            </p>
          )}

          <p className="text-xs text-text-muted">
            Tip: Try Chrome or Edge for WebGPU support
          </p>
        </div>

        <div className="px-5 py-3 bg-elevated/30 border-t border-border-subtle flex gap-3">
          <button
            onClick={onSkip}
            className="flex-1 px-4 py-2 text-sm font-medium text-text-secondary bg-surface border border-border-subtle rounded-lg hover:bg-hover hover:text-text-primary transition-all flex items-center justify-center gap-2"
          >
            <SkipForward className="w-4 h-4" />
            Skip Embeddings
          </button>
          <button
            onClick={onUseCPU}
            className={`flex-1 px-4 py-2 text-sm font-medium rounded-lg transition-all flex items-center justify-center gap-2 ${
              compactProject
                ? 'bg-node-function text-white hover:bg-node-function/90'
                : 'bg-amber-500/20 text-amber-300 border border-amber-500/30 hover:bg-amber-500/30'
            }`}
          >
            <Snail className="w-4 h-4" />
            Use CPU {compactProject ? '(Recommended)' : '(Slow)'}
          </button>
        </div>
      </div>
    </div>
  );
};
