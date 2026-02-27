import { useState, useEffect } from 'react';
import { Radio, FolderOpen, FileText, Power, PowerOff } from 'lucide-react';
import { useAppState } from '../hooks/useAppState';
import type { ToolEvent } from '../hooks/useAgentWatcher';

const TOOL_COLORS: Record<string, string> = {
  read: '#30D158',
  write: '#FF9F0A',
  edit: '#FF9F0A',
  create: '#0A84FF',
  delete: '#FF453A',
  exec: '#BF5AF2',
};

function formatTime(ts: number): string {
  const d = new Date(ts);
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
}

export const AgentPanel = () => {
  const {
    agentWatcherState,
    startAgentWatcher,
    stopAgentWatcher,
  } = useAppState();

  const { isConnected, recentEvents, workspacePath: watchedPath, logPath: watchedLog } = agentWatcherState;

  const [inputWorkspace, setInputWorkspace] = useState('');
  const [inputLog, setInputLog] = useState('');

  useEffect(() => {
    if (isConnected) {
      if (watchedPath) setInputWorkspace(watchedPath);
      if (watchedLog) setInputLog(watchedLog);
    }
  }, [isConnected, watchedPath, watchedLog]);

  const handleBrowseWorkspace = async () => {
    if (!window.prowl) return;
    const path = await window.prowl.selectDirectory();
    if (path) setInputWorkspace(path);
  };

  const handleBrowseLog = async () => {
    if (!window.prowl) return;
    const path = await window.prowl.selectFile();
    if (path) setInputLog(path);
  };

  const handleToggle = async () => {
    if (isConnected) {
      await stopAgentWatcher();
    } else {
      if (!inputWorkspace) return;
      await startAgentWatcher(inputWorkspace, inputLog || undefined);
    }
  };

  return (
    <div className="flex flex-col h-full">
      <div className="p-4 space-y-3 border-b border-white/[0.08]">
        <div className="flex items-center gap-2 text-[13px]">
          <span className={`w-1.5 h-1.5 rounded-full ${isConnected ? 'bg-[#30D158] opacity-80' : 'bg-text-muted opacity-60'}`} />
          <span className={isConnected ? 'text-[#30D158]/80' : 'text-text-muted'}>
            {isConnected ? 'Watching' : 'Disconnected'}
          </span>
        </div>

        <div className="space-y-1">
          <label className="text-[11px] text-text-muted uppercase tracking-wide">Workspace</label>
          <div className="flex gap-2">
            <input
              type="text"
              value={inputWorkspace}
              onChange={(e) => setInputWorkspace(e.target.value)}
              placeholder="/path/to/project"
              disabled={isConnected}
              className="flex-1 px-2.5 py-1.5 bg-white/[0.06] border border-white/[0.12] rounded-md text-[12px] font-mono text-text-primary placeholder-text-muted focus:outline-none focus:border-accent/50 disabled:opacity-40 transition-colors"
            />
            <button
              onClick={handleBrowseWorkspace}
              disabled={isConnected}
              className="px-2 py-1.5 bg-white/[0.06] border border-white/[0.12] rounded-md text-text-secondary hover:text-text-primary hover:bg-white/[0.1] disabled:opacity-40 transition-colors"
              title="Browse"
            >
              <FolderOpen className="w-3.5 h-3.5" />
            </button>
          </div>
        </div>

        <div className="space-y-1">
          <label className="text-[11px] text-text-muted uppercase tracking-wide">Agent Log (optional)</label>
          <div className="flex gap-2">
            <input
              type="text"
              value={inputLog}
              onChange={(e) => setInputLog(e.target.value)}
              placeholder="/path/to/agent.log"
              disabled={isConnected}
              className="flex-1 px-2.5 py-1.5 bg-white/[0.06] border border-white/[0.12] rounded-md text-[12px] font-mono text-text-primary placeholder-text-muted focus:outline-none focus:border-accent/50 disabled:opacity-40 transition-colors"
            />
            <button
              onClick={handleBrowseLog}
              disabled={isConnected}
              className="px-2 py-1.5 bg-white/[0.06] border border-white/[0.12] rounded-md text-text-secondary hover:text-text-primary hover:bg-white/[0.1] disabled:opacity-40 transition-colors"
              title="Browse"
            >
              <FileText className="w-3.5 h-3.5" />
            </button>
          </div>
        </div>

        <button
          onClick={handleToggle}
          disabled={!isConnected && !inputWorkspace}
          className={`
            w-full flex items-center justify-center gap-2 px-3 py-2 rounded-md text-[13px] transition-all
            ${isConnected
              ? 'bg-[#FF453A]/10 text-[#FF453A] border border-[#FF453A]/20 hover:bg-[#FF453A]/15'
              : 'bg-accent text-white hover:bg-accent-dim disabled:opacity-40 disabled:cursor-not-allowed'
            }
          `}
        >
          {isConnected ? (
            <>
              <PowerOff className="w-3.5 h-3.5" />
              Disconnect
            </>
          ) : (
            <>
              <Power className="w-3.5 h-3.5" />
              Connect
            </>
          )}
        </button>
      </div>

      <div className="flex-1 overflow-y-auto scrollbar-thin">
        {recentEvents.length === 0 ? (
          <div className="flex flex-col items-center justify-center h-full text-center px-4">
            <Radio className="w-5 h-5 text-text-muted mb-2" />
            <p className="text-[12px] text-text-muted">
              {isConnected ? 'Waiting for agent activity...' : 'Connect to see live events'}
            </p>
          </div>
        ) : (
          <div className="divide-y divide-white/[0.06]">
            {recentEvents.map((event: ToolEvent, i: number) => (
              <div key={`${event.timestamp}-${i}`} className="px-4 py-2 hover:bg-white/[0.04] transition-colors">
                <div className="flex items-center gap-2">
                  <span
                    className="w-1 h-1 rounded-full flex-shrink-0"
                    style={{ backgroundColor: TOOL_COLORS[event.tool] || 'rgba(255,255,255,0.35)' }}
                  />
                  <span
                    className="text-[11px] font-mono"
                    style={{ color: TOOL_COLORS[event.tool] || 'rgba(255,255,255,0.55)' }}
                  >
                    {event.tool}
                  </span>
                  <span className="text-[10px] text-text-muted ml-auto">
                    {formatTime(event.timestamp)}
                  </span>
                </div>
                {event.filepath && (
                  <p className="text-[11px] text-text-muted font-mono mt-0.5 truncate pl-3">
                    {event.filepath}
                  </p>
                )}
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  );
};
