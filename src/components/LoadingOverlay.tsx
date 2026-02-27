import { useEffect, useState, useRef } from 'react';
import { IndexingProgress } from '../types/pipeline';

const MATRIX_CHARS = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789@#$%&*+=<>{}[]|/\\~^';
const LINE_LENGTH = 48;

function useMatrixLine(speed: number = 50) {
  const [line, setLine] = useState('');
  const rafRef = useRef<number>(0);
  const lastRef = useRef(0);

  useEffect(() => {
    const tick = (time: number) => {
      if (time - lastRef.current > speed) {
        let s = '';
        for (let i = 0; i < LINE_LENGTH; i++) {
          s += MATRIX_CHARS[Math.floor(Math.random() * MATRIX_CHARS.length)];
        }
        setLine(s);
        lastRef.current = time;
      }
      rafRef.current = requestAnimationFrame(tick);
    };
    rafRef.current = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(rafRef.current);
  }, [speed]);

  return line;
}

interface LoadingOverlayProps {
  progress: IndexingProgress;
  onCancel?: () => void;
}

export const LoadingOverlay = ({ progress, onCancel }: LoadingOverlayProps) => {
  const matrixLine = useMatrixLine(60);

  return (
    <div className="fixed inset-0 flex items-end bg-void/80 backdrop-blur-2xl z-50">
      {/* Ambient glow — off-center, subtle */}
      <div className="absolute inset-0 pointer-events-none overflow-hidden">
        <div className="absolute bottom-0 left-1/4 w-[500px] h-[400px] bg-accent/[0.03] rounded-full blur-[120px]" />
      </div>

      {/* Cancel */}
      {onCancel && (
        <button
          onClick={onCancel}
          className="absolute top-5 right-5 px-3 py-1.5 text-[11px] tracking-wide text-text-muted hover:text-text-secondary border border-white/[0.08] hover:border-white/[0.15] rounded-md transition-colors bg-white/[0.03] hover:bg-white/[0.06]"
        >
          Cancel
        </button>
      )}

      {/* Matrix ticker — runs full width along the top */}
      <div className="absolute top-6 left-0 right-0 font-mono text-[11px] tracking-[0.25em] text-accent/30 select-none overflow-hidden whitespace-nowrap text-center">
        {matrixLine}
      </div>

      {/* Bottom-left content block */}
      <div className="relative p-10 pb-12 w-full max-w-lg">
        {/* Phase message + percentage inline */}
        <div className="flex items-baseline gap-3 mb-3">
          <span className="text-[13px] text-text-secondary">
            {progress.message}
          </span>
          <span className="text-[13px] font-mono text-text-muted tabular-nums">
            {progress.percent}%
          </span>
        </div>

        {/* Progress — full-width thin line */}
        <div className="h-px bg-white/[0.08] rounded-full overflow-hidden mb-4">
          <div
            className="h-full transition-all duration-500 ease-out"
            style={{
              width: `${progress.percent}%`,
              background: `rgba(90, 158, 170, 0.6)`,
            }}
          />
        </div>

        {/* Detail + stats as mono log lines */}
        <div className="space-y-1 font-mono text-[11px] text-text-muted">
          {progress.detail && (
            <p className="truncate max-w-md">{progress.detail}</p>
          )}
          {progress.stats && (
            <p className="tabular-nums">
              {progress.stats.filesProcessed}/{progress.stats.totalFiles} files
              {' \u00b7 '}
              {progress.stats.nodesCreated} symbols
            </p>
          )}
        </div>
      </div>
    </div>
  );
};
