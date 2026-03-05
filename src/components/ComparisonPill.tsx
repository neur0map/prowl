import { X } from 'lucide-react';
import { useState, useEffect, useCallback, useRef } from 'react';
import type { ComparisonStats } from '../core/compare/types';
import { useAppState } from '../hooks/useAppState';

export function ComparisonPill() {
  const { getWorkerApi } = useAppState();
  const [stats, setStats] = useState<ComparisonStats | null>(null);
  const [isNew, setIsNew] = useState(false);
  const prevRepoRef = useRef<string | null>(null);

  useEffect(() => {
    const poll = async () => {
      const api = getWorkerApi();
      if (!api) return;
      try {
        const s = await api.getComparisonStats();
        // Detect new comparison loaded
        if (s && s.repoName !== prevRepoRef.current) {
          setIsNew(true);
          setTimeout(() => setIsNew(false), 1500);
        }
        prevRepoRef.current = s?.repoName ?? null;
        setStats(s);
      } catch {
        prevRepoRef.current = null;
        setStats(null);
      }
    };

    poll();
    const interval = setInterval(poll, 3_000);
    return () => clearInterval(interval);
  }, [getWorkerApi]);

  const handleClose = useCallback(async () => {
    const api = getWorkerApi();
    if (!api || !stats) return;
    try {
      api.closeComparison();
    } catch { /* best-effort */ }
    prevRepoRef.current = null;
    setStats(null);
  }, [getWorkerApi, stats]);

  if (!stats) return null;

  return (
    <div
      className="flex items-center gap-1.5 px-2 py-0.5 rounded-full bg-violet-500/10 border border-violet-400/20 text-[11px] font-mono transition-all duration-300"
      style={{
        animation: isNew ? 'compareAppear 0.3s ease-out' : undefined,
        boxShadow: isNew ? '0 0 12px rgba(139, 92, 246, 0.3)' : undefined,
      }}
    >
      <style>{`
        @keyframes compareAppear {
          0% { opacity: 0; transform: scale(0.9); }
          100% { opacity: 1; transform: scale(1); }
        }
      `}</style>
      <span className="w-1.5 h-1.5 rounded-full bg-violet-400/70" />
      <span className="text-violet-300/80 truncate max-w-[120px]">{stats.repoName}</span>
      <span className="text-violet-300/50">{stats.fileCount} files</span>
      <button
        onClick={handleClose}
        className="ml-0.5 p-0.5 rounded hover:bg-violet-400/20 text-violet-300/60 hover:text-violet-300 transition-colors"
        title="Close comparison project"
      >
        <X className="w-2.5 h-2.5" />
      </button>
    </div>
  );
}
