import { useEffect, useState, useRef } from 'react';
import { PipelineProgress } from '../types/pipeline';

interface LoadingOverlayProps {
  progress: PipelineProgress;
}

const MATRIX_CHARS = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789@#$%&*+=<>{}[]|/\\~^';
const LINE_LENGTH = 42;

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

export const LoadingOverlay = ({ progress }: LoadingOverlayProps) => {
  const matrixLine = useMatrixLine(60);

  return (
    <div className="fixed inset-0 flex flex-col items-center justify-center bg-void/80 backdrop-blur-2xl z-50">
      {/* Ambient glow */}
      <div className="absolute inset-0 pointer-events-none overflow-hidden">
        <div className="absolute top-1/2 left-1/2 -translate-x-1/2 -translate-y-1/2 w-[600px] h-[600px] bg-accent/[0.04] rounded-full blur-[120px]" />
      </div>

      {/* Matrix line */}
      <div className="mb-8 font-mono text-[11px] tracking-[0.3em] text-accent/40 select-none overflow-hidden whitespace-nowrap w-80 text-center">
        {matrixLine}
      </div>

      {/* Progress bar */}
      <div className="w-72 mb-5">
        <div className="h-[3px] bg-white/[0.06] rounded-full overflow-hidden">
          <div
            className="h-full rounded-full transition-all duration-500 ease-out"
            style={{
              width: `${progress.percent}%`,
              background: 'linear-gradient(90deg, rgba(0,122,255,0.8), rgba(48,209,88,0.8))',
            }}
          />
        </div>
      </div>

      {/* Status */}
      <p className="text-[13px] text-text-secondary tracking-wide mb-1">
        {progress.message}
      </p>
      {progress.detail && (
        <p className="text-[11px] text-text-muted font-mono truncate max-w-sm">
          {progress.detail}
        </p>
      )}

      {/* Stats */}
      {progress.stats && (
        <div className="mt-6 flex items-center gap-5 text-[11px] text-text-muted">
          <span>{progress.stats.filesProcessed}/{progress.stats.totalFiles} files</span>
          <span className="w-px h-3 bg-white/[0.1]" />
          <span>{progress.stats.nodesCreated} nodes</span>
        </div>
      )}

      {/* Percent */}
      <p className="mt-3 text-[28px] font-light tracking-tight text-text-primary/60 tabular-nums">
        {progress.percent}%
      </p>
    </div>
  );
};
