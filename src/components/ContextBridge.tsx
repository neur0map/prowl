import { useState, useEffect, useCallback } from 'react';
import {
  Plug, PlugZap, Settings, ChevronDown, ChevronUp,
  Clock, CheckCircle, XCircle, Loader2
} from 'lucide-react';
import { getRecentQueries } from '../mcp/tool-handlers';

export const ContextBridge = () => {
  const [isExpanded, setIsExpanded] = useState(false);
  const [serverInfo, setServerInfo] = useState<{ port: number } | null>(null);
  const [configStatus, setConfigStatus] = useState<{ configured: boolean; configPath: string | null } | null>(null);
  const [configuring, setConfiguring] = useState(false);
  const [configResult, setConfigResult] = useState<string | null>(null);
  const [recentQueries, setRecentQueries] = useState<Array<{ tool: string; ts: number }>>([]);

  // Fetch server info on mount
  useEffect(() => {
    if (!window.prowl?.mcp) return;
    window.prowl.mcp.getServerInfo().then(setServerInfo);
    window.prowl.mcp.getConfigStatus().then(setConfigStatus);
  }, []);

  // Poll recent queries every 3s while expanded
  useEffect(() => {
    if (!isExpanded) return;
    setRecentQueries(getRecentQueries());
    const interval = setInterval(() => {
      setRecentQueries(getRecentQueries());
    }, 3000);
    return () => clearInterval(interval);
  }, [isExpanded]);

  const handleConfigureClaudeCode = useCallback(async () => {
    if (!window.prowl?.mcp) return;

    setConfiguring(true);
    setConfigResult(null);

    try {
      const result = await window.prowl.mcp.configureClaudeCode();
      if (result.success) {
        setConfigResult('Configured! Restart Claude Code to pick up the changes.');
        setConfigStatus({ configured: true, configPath: result.path || null });
      } else {
        setConfigResult(`Failed: ${result.error}`);
      }
    } catch (err) {
      setConfigResult(`Error: ${err instanceof Error ? err.message : String(err)}`);
    } finally {
      setConfiguring(false);
    }
  }, []);

  const isRunning = !!serverInfo;

  return (
    <div className="border-t border-white/[0.08] bg-void/30">
      <button
        onClick={() => setIsExpanded(!isExpanded)}
        className="w-full flex items-center justify-between px-4 py-2 hover:bg-white/[0.04] transition-colors"
      >
        <div className="flex items-center gap-2">
          {isRunning ? (
            <PlugZap className="w-4 h-4 text-green-400" />
          ) : (
            <Plug className="w-4 h-4 text-text-muted/60" />
          )}
          <span className="text-[12px] font-medium text-text-primary">
            MCP Server
          </span>
          {isRunning && (
            <span className="px-1.5 py-0.5 rounded bg-green-500/10 text-[10px] text-green-400 font-medium">
              port {serverInfo.port}
            </span>
          )}
        </div>
        <div className="flex items-center gap-2">
          {configStatus?.configured && (
            <span className="text-[10px] text-text-muted/60">
              Claude Code linked
            </span>
          )}
          {isExpanded ? (
            <ChevronUp className="w-4 h-4 text-text-muted/60" />
          ) : (
            <ChevronDown className="w-4 h-4 text-text-muted/60" />
          )}
        </div>
      </button>

      {isExpanded && (
        <div className="px-4 pb-4 space-y-3">
          {/* Status */}
          <div className="p-3 bg-white/[0.03] border border-white/[0.08] rounded-lg space-y-2">
            <div className="flex items-center justify-between">
              <span className="text-[11px] text-text-muted">Status</span>
              <div className="flex items-center gap-1.5">
                {isRunning ? (
                  <>
                    <CheckCircle className="w-3 h-3 text-green-400" />
                    <span className="text-[11px] text-green-400">Running</span>
                  </>
                ) : (
                  <>
                    <XCircle className="w-3 h-3 text-text-muted/60" />
                    <span className="text-[11px] text-text-muted/60">Not running</span>
                  </>
                )}
              </div>
            </div>

            {isRunning && (
              <div className="flex items-center justify-between">
                <span className="text-[11px] text-text-muted">Endpoint</span>
                <span className="text-[11px] text-text-secondary font-mono">
                  127.0.0.1:{serverInfo.port}
                </span>
              </div>
            )}
          </div>

          {/* Claude Code Configuration */}
          <div className="p-3 bg-white/[0.03] border border-white/[0.08] rounded-lg space-y-2">
            <div className="flex items-center justify-between">
              <span className="text-[11px] text-text-muted">Claude Code</span>
              {configStatus?.configured ? (
                <span className="text-[10px] text-green-400">Configured</span>
              ) : (
                <span className="text-[10px] text-text-muted/60">Not configured</span>
              )}
            </div>

            <button
              onClick={handleConfigureClaudeCode}
              disabled={configuring}
              className="w-full flex items-center justify-center gap-1.5 px-3 py-1.5 bg-accent/10 border border-accent/30 rounded-lg text-[11px] text-accent hover:bg-accent/20 transition-colors disabled:opacity-50"
            >
              {configuring ? (
                <>
                  <Loader2 className="w-3.5 h-3.5 animate-spin" />
                  Configuring...
                </>
              ) : (
                <>
                  <Settings className="w-3.5 h-3.5" />
                  {configStatus?.configured ? 'Reconfigure Claude Code' : 'Configure Claude Code'}
                </>
              )}
            </button>

            {configResult && (
              <p className="text-[10px] text-text-muted/80 mt-1">{configResult}</p>
            )}
          </div>

          {/* Recent Queries */}
          {recentQueries.length > 0 && (
            <div className="p-3 bg-white/[0.03] border border-white/[0.08] rounded-lg space-y-2">
              <span className="text-[11px] text-text-muted">Recent MCP Queries</span>
              <div className="space-y-1">
                {recentQueries.map((q, i) => (
                  <div key={i} className="flex items-center justify-between">
                    <div className="flex items-center gap-1.5">
                      <Clock className="w-3 h-3 text-text-muted/40" />
                      <span className="text-[10px] text-text-secondary font-mono">
                        {q.tool}
                      </span>
                    </div>
                    <span className="text-[10px] text-text-muted/60">
                      {formatTimeAgo(q.ts)}
                    </span>
                  </div>
                ))}
              </div>
            </div>
          )}

          {/* Help text */}
          <p className="text-[10px] text-text-muted/50 leading-relaxed">
            The MCP server exposes Prowl's knowledge graph to terminal coding agents.
            Configure Claude Code to auto-discover Prowl's tools (prowl_search, prowl_overview, prowl_impact, etc).
          </p>
        </div>
      )}
    </div>
  );
};

/** Collapsed single-line MCP indicator. Click to expand full panel. */
export const ContextBridgeIndicator = ({ onExpand }: { onExpand: () => void }) => {
  const [serverInfo, setServerInfo] = useState<{ port: number } | null>(null);
  const [configStatus, setConfigStatus] = useState<{ configured: boolean } | null>(null);

  useEffect(() => {
    if (!window.prowl?.mcp) return;
    window.prowl.mcp.getServerInfo().then(setServerInfo);
    window.prowl.mcp.getConfigStatus().then((s: any) => setConfigStatus(s));
  }, []);

  const isRunning = !!serverInfo;
  if (!isRunning) return null;

  return (
    <button
      onClick={onExpand}
      className="flex items-center gap-1.5 px-2 py-1 rounded text-[11px] text-text-muted hover:text-text-secondary hover:bg-white/[0.04] transition-colors"
    >
      <span className="w-1.5 h-1.5 rounded-full bg-green-400 shrink-0" />
      <span className="font-mono">MCP</span>
      {configStatus?.configured && (
        <span className="text-text-muted/40 font-mono">Claude Code</span>
      )}
    </button>
  );
};

function formatTimeAgo(ts: number): string {
  const diff = Date.now() - ts;
  if (diff < 60_000) return 'just now';
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)}m ago`;
  return `${Math.floor(diff / 3_600_000)}h ago`;
}
