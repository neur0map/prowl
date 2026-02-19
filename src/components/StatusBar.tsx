import { useAppState } from '../hooks/useAppState';

export const StatusBar = () => {
  const { graph, progress, agentWatcherState } = useAppState();

  const nodeCount = graph?.nodes.length ?? 0;
  const edgeCount = graph?.relationships.length ?? 0;

  const primaryLanguage = (() => {
    if (!graph) return null;
    const languages = graph.nodes.map(n => n.properties.language).filter(Boolean);
    if (languages.length === 0) return null;
    const counts = languages.reduce((acc, lang) => {
      acc[lang!] = (acc[lang!] || 0) + 1;
      return acc;
    }, {} as Record<string, number>);
    return Object.entries(counts).sort((a, b) => b[1] - a[1])[0]?.[0];
  })();

  return (
    <footer className="flex items-center justify-between px-4 py-1.5 glass border-t border-white/[0.08] text-[11px] text-text-muted">
      {/* Left — status */}
      <div className="flex items-center gap-3">
        {progress && progress.phase !== 'complete' ? (
          <>
            <div className="w-24 h-1 rounded-full overflow-hidden bg-white/[0.08]">
              <div
                className="h-full bg-accent rounded-full transition-all duration-300"
                style={{ width: `${progress.percent}%` }}
              />
            </div>
            <span>{progress.message}</span>
          </>
        ) : (
          <div className="flex items-center gap-1.5">
            <span className="w-1.5 h-1.5 bg-[#30D158] rounded-full opacity-60" />
            <span>Ready</span>
          </div>
        )}
      </div>

      {/* Center — agent */}
      {agentWatcherState.isConnected ? (
        <div className="flex items-center gap-2">
          <span className="w-1.5 h-1.5 bg-[#30D158] rounded-full opacity-60" />
          <span className="text-[#30D158]/80">Watching</span>
          {agentWatcherState.workspacePath && (
            <span className="text-text-muted truncate max-w-[200px] font-mono text-[10px]">
              {agentWatcherState.workspacePath}
            </span>
          )}
        </div>
      ) : (
        <span>Prowl</span>
      )}

      {/* Right — stats */}
      <div className="flex items-center gap-3">
        {graph && (
          <>
            <span>{nodeCount} nodes</span>
            <span className="text-white/10">·</span>
            <span>{edgeCount} edges</span>
            {primaryLanguage && (
              <>
                <span className="text-white/10">·</span>
                <span>{primaryLanguage}</span>
              </>
            )}
          </>
        )}
      </div>
    </footer>
  );
};
