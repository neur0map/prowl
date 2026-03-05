import { useState } from 'react';

interface RoadmapItem {
  id: string;
  title: string;
  description: string;
  status: 'completed' | 'in-progress' | 'planned' | 'backlog';
  category: string;
}

const categoryLabels: Record<string, string> = {
  core: 'Core',
  chat: 'Chat & Context',
  graph: 'Graph & Viz',
  auth: 'Auth & Security',
  ui: 'UI/UX',
  int: 'Integrations',
};

const categoryOrder = ['core', 'chat', 'graph', 'auth', 'ui', 'int'];

const roadmapItems: RoadmapItem[] = [
  { id: 'core-graph', title: 'Code Graph', description: 'Multi-table graph with File, Function, Class, Method nodes', status: 'completed', category: 'core' },
  { id: 'core-search', title: 'Hybrid Search', description: 'BM25 + semantic embeddings with rank fusion', status: 'completed', category: 'core' },
  { id: 'core-cypher', title: 'Cypher Query Support', description: 'Direct graph database querying', status: 'completed', category: 'core' },
  { id: 'core-embedding', title: 'Code Embeddings', description: 'Vector search with WebGPU/WASM fallback', status: 'completed', category: 'core' },
  { id: 'core-community', title: 'Community Detection', description: 'Louvain algorithm for code clustering', status: 'completed', category: 'core' },
  { id: 'core-process', title: 'Process Tracing', description: 'Execution path analysis and call graphs', status: 'completed', category: 'core' },
  { id: 'core-impact', title: 'Impact Analysis', description: 'Trace what depends on what', status: 'completed', category: 'core' },
  { id: 'core-snapshot', title: 'Snapshot Persistence', description: 'Persist code graph, search index, and embeddings to disk for instant re-open', status: 'completed', category: 'core' },
  { id: 'core-incremental', title: 'Incremental Re-indexing', description: 'Git-aware diffing — only re-process changed files on re-open', status: 'completed', category: 'core' },
  { id: 'core-embedding-warm', title: 'Embedding Model Warm-on-Boot', description: 'Pre-load embedding model from snapshot so semantic search has zero cold-start', status: 'completed', category: 'core' },
  { id: 'core-hmac', title: 'Snapshot Integrity (HMAC)', description: 'HMAC-SHA256 signing with machine-local key via OS keychain', status: 'completed', category: 'core' },
  { id: 'core-lang-ext', title: 'Extended Language Support', description: 'Struct, Enum, Trait, Impl, Macro, Namespace, Union, Template + 10 more node types', status: 'completed', category: 'core' },
  { id: 'core-kuzu-hardening', title: 'KuzuDB Stability', description: 'Race condition fixes, graceful DB-closed handling, atomic snapshot writes', status: 'completed', category: 'core' },
  { id: 'chat-injection', title: 'Context Bridge', description: 'Expose codebase context to external AI agents via MCP Server', status: 'completed', category: 'chat' },
  { id: 'chat-history', title: 'Chat History', description: 'Persistent conversation sessions with auto-save and restore', status: 'completed', category: 'chat' },
  { id: 'chat-compaction', title: 'Context Compaction', description: 'Auto-summarize older messages when context exceeds 40K tokens', status: 'completed', category: 'chat' },
  { id: 'chat-loop-fix', title: 'Tool-Aware History', description: 'Include tool usage summaries in history to prevent investigation loops', status: 'completed', category: 'chat' },
  { id: 'chat-context', title: 'Context Management', description: 'Manage and reuse code context snippets', status: 'planned', category: 'chat' },
  { id: 'chat-templates', title: 'Prompt Templates', description: 'Reusable prompts for common tasks', status: 'planned', category: 'chat' },
  { id: 'graph-reactflow', title: 'React Flow Graph', description: 'Zone-grid layout with glassmorphic cluster cards', status: 'completed', category: 'graph' },
  { id: 'graph-layout', title: 'Advanced Layouts', description: 'Force-directed, hierarchical, circular layouts', status: 'in-progress', category: 'graph' },
  { id: 'graph-timeline', title: 'Code Timeline', description: 'Visualize code changes over time', status: 'planned', category: 'graph' },
  { id: 'graph-filter', title: 'Advanced Filtering', description: 'Filter by type, complexity, ownership', status: 'planned', category: 'graph' },
  { id: 'auth-oauth', title: 'OAuth Login', description: 'Sign in with Claude and OpenAI', status: 'planned', category: 'auth' },
  { id: 'auth-oauth-refresh', title: 'Token Refresh', description: 'Automatic OAuth token refresh', status: 'planned', category: 'auth' },
  { id: 'auth-multiple', title: 'Multi-Provider Auth', description: 'Simultaneous OAuth for multiple providers', status: 'planned', category: 'auth' },
  { id: 'ui-styling', title: 'Design System', description: 'Consistent color palette, typography, spacing', status: 'completed', category: 'ui' },
  { id: 'ui-responsive', title: 'Responsive Design', description: 'Adaptive layouts for all screen sizes', status: 'in-progress', category: 'ui' },
  { id: 'ui-themes', title: 'Custom Themes', description: 'Dark, light, and accent color themes', status: 'planned', category: 'ui' },
  { id: 'ui-shortcuts', title: 'Keyboard Shortcuts', description: 'Configurable hotkeys and command palette', status: 'planned', category: 'ui' },
  { id: 'ui-animations', title: 'Micro-animations', description: 'Smooth transitions, layout easing, and visual feedback', status: 'completed', category: 'ui' },
  { id: 'ui-embedding-banner', title: 'Embedding Status Banner', description: 'Non-intrusive progress indicator for background embedding generation', status: 'completed', category: 'ui' },
  { id: 'ui-devtools-prod', title: 'Production Hardening', description: 'DevTools disabled in production builds, keyboard shortcut blocking', status: 'completed', category: 'ui' },
  { id: 'int-mcp', title: 'MCP Server', description: '12 tools over stdio — Claude Code, Cursor, and other agents query the knowledge graph directly', status: 'completed', category: 'int' },
  { id: 'int-mcp-config', title: 'One-Click MCP Setup', description: 'Auto-configure Claude Code from Prowl Settings — writes ~/.claude.json with correct server path', status: 'completed', category: 'int' },
  { id: 'int-mcp-live', title: 'Live MCP Reindexing', description: 'Chokidar watches for file changes, debounced pipeline refreshes all 6 MCP layers within seconds', status: 'completed', category: 'int' },
  { id: 'int-graph-const', title: 'Capture Exported Constants', description: 'Zod schemas, config objects, and module-level variables become graph nodes', status: 'completed', category: 'int' },
  { id: 'int-graph-resolve', title: 'Smart Symbol Resolution', description: 'Impact and explore auto-detect file paths vs symbol names', status: 'completed', category: 'int' },
  { id: 'int-graph-imports', title: 'Reliable Import Edges', description: 'All node types flow to KuzuDB, edge-preserving live updates', status: 'completed', category: 'int' },
  { id: 'int-live-pipeline', title: 'Live Update Pipeline', description: 'Chokidar watcher with debounce, concurrency guard, rollback on failure', status: 'completed', category: 'int' },
  { id: 'int-dir-events', title: 'Directory Events', description: 'Handle addDir/unlinkDir for folder create and delete during live updates', status: 'completed', category: 'int' },
  { id: 'int-compare', title: 'Compare Mode', description: 'Load any GitHub repo via REST API for lightweight side-by-side comparison — no clone, no indexing', status: 'completed', category: 'int' },
  { id: 'int-mcp-compare', title: 'MCP Compare Tools', description: '5 new MCP tools: browse, read, grep, and summarize a comparison repo from your AI agent', status: 'completed', category: 'int' },
  { id: 'int-mcp-changes', title: 'MCP Change Detection', description: 'prowl_changes tool maps git diffs to affected symbols, clusters, and risk level', status: 'completed', category: 'int' },
  { id: 'int-mcp-retry', title: 'MCP Auto-Reconnect', description: 'HTTP client retries on Prowl restart — re-reads port/auth and reconnects automatically', status: 'completed', category: 'int' },
  { id: 'int-github', title: 'GitHub PR Integration', description: 'Preview PRs with code graph context', status: 'planned', category: 'int' },
  { id: 'int-vscode', title: 'VS Code Extension', description: 'Native IDE integration', status: 'backlog', category: 'int' },
  { id: 'graph-highlight', title: 'AI Tool Highlighting', description: 'Clusters glow and animate when AI agent accesses related files or symbols', status: 'completed', category: 'graph' },
  { id: 'graph-perf', title: 'Graph Performance', description: 'Animated edges only when selected, MiniMap hidden for 60+ cluster graphs', status: 'completed', category: 'graph' },
  { id: 'chat-thinking', title: 'Extended Thinking', description: 'Collapsible thinking pills show Claude model reasoning before responses', status: 'completed', category: 'chat' },
  { id: 'ui-compare-timer', title: 'Comparison Timer', description: '30-minute countdown in status bar for shadow clone repos, turns amber under 5 minutes', status: 'completed', category: 'ui' },
  { id: 'ui-cached-status', title: 'Cached Status Badge', description: 'Snapshot cache indicator moved to status bar with database icon', status: 'completed', category: 'ui' },
];

const statusMark: Record<string, string> = {
  completed: '✓',
  'in-progress': '○',
  planned: '·',
  backlog: '—',
};

const statusTextClass: Record<string, string> = {
  completed: 'text-text-muted/50',
  'in-progress': 'text-text-secondary',
  planned: 'text-text-muted/40',
  backlog: 'text-text-muted/30',
};

interface RoadmapPanelProps {
  isOpen: boolean;
  onClose: () => void;
}

export const RoadmapPanel = ({ isOpen, onClose }: RoadmapPanelProps) => {
  const [selectedCategory, setSelectedCategory] = useState<string | null>(null);

  const filteredItems = selectedCategory
    ? roadmapItems.filter(item => item.category === selectedCategory)
    : roadmapItems;

  const grouped = categoryOrder.reduce<Record<string, RoadmapItem[]>>((acc, catId) => {
    const items = filteredItems.filter(item => item.category === catId);
    if (items.length > 0) acc[catId] = items;
    return acc;
  }, {});

  const completed = roadmapItems.filter(i => i.status === 'completed').length;
  const total = roadmapItems.length;

  if (!isOpen) return null;

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center">
      <div className="absolute inset-0 bg-black/50" onClick={onClose} />

      <div
        className="relative w-full max-w-2xl max-h-[80vh] mx-4 bg-void border border-white/[0.08] rounded-lg shadow-2xl overflow-hidden flex flex-col"
        style={{ animation: 'scaleIn 0.15s ease-out' }}
      >
        {/* Header — flat, no icons */}
        <div className="flex items-center justify-between px-5 py-4 border-b border-white/[0.06]">
          <div>
            <h2 className="text-[15px] font-mono text-text-primary">roadmap</h2>
            <p className="text-[12px] text-text-muted font-mono mt-0.5">
              {completed}/{total} done
            </p>
          </div>
          <button
            onClick={onClose}
            className="text-[12px] text-text-muted hover:text-text-primary font-mono transition-colors"
          >
            esc
          </button>
        </div>

        {/* Progress — thin line, no gradient */}
        <div className="px-5 py-2.5 border-b border-white/[0.06]">
          <div className="h-[2px] bg-white/[0.06] rounded-full overflow-hidden">
            <div
              className="h-full bg-accent/60 rounded-full"
              style={{ width: `${Math.round((completed / total) * 100)}%` }}
            />
          </div>
        </div>

        {/* Category filters — plain text, no pills */}
        <div className="px-5 py-2 border-b border-white/[0.06] flex items-center gap-3 overflow-x-auto">
          <button
            onClick={() => setSelectedCategory(null)}
            className={`text-[12px] font-mono transition-colors whitespace-nowrap ${
              !selectedCategory ? 'text-text-primary' : 'text-text-muted hover:text-text-secondary'
            }`}
          >
            all
          </button>
          {categoryOrder.map(catId => (
            <button
              key={catId}
              onClick={() => setSelectedCategory(selectedCategory === catId ? null : catId)}
              className={`text-[12px] font-mono transition-colors whitespace-nowrap ${
                selectedCategory === catId ? 'text-text-primary' : 'text-text-muted hover:text-text-secondary'
              }`}
            >
              {categoryLabels[catId]?.toLowerCase()}
            </button>
          ))}
        </div>

        {/* Items */}
        <div className="flex-1 overflow-y-auto px-5 py-3 scrollbar-thin">
          {Object.entries(grouped).map(([catId, items]) => (
            <div key={catId} className="mb-4 last:mb-0">
              <div className="text-[11px] text-text-muted/50 font-mono uppercase tracking-wider mb-2">
                {categoryLabels[catId]}
              </div>
              <div className="space-y-0.5">
                {items.map(item => (
                  <div
                    key={item.id}
                    className="flex items-start gap-3 py-1.5 group"
                  >
                    <span className={`font-mono text-[13px] w-4 text-center shrink-0 ${statusTextClass[item.status]}`}>
                      {statusMark[item.status]}
                    </span>
                    <div className="flex-1 min-w-0">
                      <span className={`text-[13px] font-mono ${
                        item.status === 'completed' ? 'text-text-muted/50 line-through decoration-white/10' : 'text-text-secondary'
                      }`}>
                        {item.title}
                      </span>
                      <span className="text-[11px] text-text-muted/30 ml-2 hidden group-hover:inline">
                        {item.description}
                      </span>
                    </div>
                    {item.status === 'in-progress' && (
                      <span className="text-[10px] text-text-muted/40 font-mono shrink-0">wip</span>
                    )}
                  </div>
                ))}
              </div>
            </div>
          ))}
        </div>

        {/* Footer */}
        <div className="px-5 py-2.5 border-t border-white/[0.06] flex items-center justify-between">
          <span className="text-[11px] text-text-muted/40 font-mono">
            <a href="https://github.com/neur0map/prowl/issues" target="_blank" rel="noopener noreferrer" className="hover:text-text-muted transition-colors">
              request a feature
            </a>
          </span>
          <span className="text-[11px] text-text-muted/30 font-mono">
            ✓ done · ○ wip · planned · — backlog
          </span>
        </div>
      </div>
    </div>
  );
};
