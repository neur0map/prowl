import { useState } from 'react';
import {
  CheckCircle2, Circle, Clock, Rocket, GitBranch,
  Lightbulb, Database, Zap, Shield, Settings, ChevronDown, ChevronRight
} from 'lucide-react';

interface RoadmapItem {
  id: string;
  title: string;
  description: string;
  status: 'completed' | 'in-progress' | 'planned' | 'backlog';
  category: string;
  effort?: 'small' | 'medium' | 'large';
}

interface RoadmapCategory {
  id: string;
  name: string;
  icon: React.ElementType;
  color: string;
}

const categories: RoadmapCategory[] = [
  { id: 'core', name: 'Core', icon: Zap, color: '#F55036' },
  { id: 'chat', name: 'Chat & Context', icon: Lightbulb, color: '#30D158' },
  { id: 'graph', name: 'Graph & Viz', icon: Database, color: '#0A84FF' },
  { id: 'auth', name: 'Auth & Security', icon: Shield, color: '#BF5AF2' },
  { id: 'ui', name: 'UI/UX', icon: Settings, color: '#FF9F0A' },
];

const roadmapItems: RoadmapItem[] = [
  // Core Features - Completed
  {
    id: 'core-graph',
    title: 'Knowledge Graph',
    description: 'Multi-table graph with File, Function, Class, Method nodes',
    status: 'completed',
    category: 'core',
  },
  {
    id: 'core-search',
    title: 'Hybrid Search',
    description: 'BM25 + semantic embeddings with rank fusion',
    status: 'completed',
    category: 'core',
  },
  {
    id: 'core-cypher',
    title: 'Cypher Query Support',
    description: 'Direct graph database querying',
    status: 'completed',
    category: 'core',
  },
  {
    id: 'core-embedding',
    title: 'Code Embeddings',
    description: 'Vector search with WebGPU/WASM fallback',
    status: 'completed',
    category: 'core',
  },
  {
    id: 'core-community',
    title: 'Community Detection',
    description: 'Leiden algorithm for code clustering',
    status: 'completed',
    category: 'core',
  },
  {
    id: 'core-process',
    title: 'Process Tracing',
    description: 'Execution path analysis and call graphs',
    status: 'completed',
    category: 'core',
  },
  {
    id: 'core-impact',
    title: 'Impact Analysis',
    description: 'Trace what depends on what',
    status: 'completed',
    category: 'core',
  },

  // Core Features - Completed (v0.1.2)
  {
    id: 'core-snapshot',
    title: 'Snapshot Persistence',
    description: 'Persist knowledge graph, search index, and embeddings to disk for instant re-open',
    status: 'completed',
    category: 'core',
  },
  {
    id: 'core-incremental',
    title: 'Incremental Re-indexing',
    description: 'Git-aware diffing — only re-process changed files on re-open',
    status: 'completed',
    category: 'core',
  },
  {
    id: 'core-embedding-warm',
    title: 'Embedding Model Warm-on-Boot',
    description: 'Pre-load embedding model from snapshot so semantic search has zero cold-start',
    status: 'completed',
    category: 'core',
  },
  {
    id: 'core-hmac',
    title: 'Snapshot Integrity (HMAC)',
    description: 'HMAC-SHA256 signing with machine-local key via OS keychain',
    status: 'completed',
    category: 'core',
  },

  // Chat & Context - In Progress
  {
    id: 'chat-injection',
    title: 'Context Bridge',
    description: 'Inject context between chat sessions',
    status: 'in-progress',
    category: 'chat',
    effort: 'medium',
  },
  {
    id: 'chat-history',
    title: 'Chat History',
    description: 'Persistent conversation sessions',
    status: 'in-progress',
    category: 'chat',
    effort: 'large',
  },

  // Chat & Context - Planned
  {
    id: 'chat-context',
    title: 'Context Management',
    description: 'Manage and reuse code context snippets',
    status: 'planned',
    category: 'chat',
    effort: 'medium',
  },
  {
    id: 'chat-templates',
    title: 'Prompt Templates',
    description: 'Reusable prompts for common tasks',
    status: 'planned',
    category: 'chat',
    effort: 'small',
  },

  // Graph & Viz - In Progress
  {
    id: 'graph-layout',
    title: 'Advanced Layouts',
    description: 'Force-directed, hierarchical, circular layouts',
    status: 'in-progress',
    category: 'graph',
    effort: 'medium',
  },

  // Graph & Viz - Planned
  {
    id: 'graph-timeline',
    title: 'Code Timeline',
    description: 'Visualize code changes over time',
    status: 'planned',
    category: 'graph',
    effort: 'large',
  },
  {
    id: 'graph-filter',
    title: 'Advanced Filtering',
    description: 'Filter by type, complexity, ownership',
    status: 'planned',
    category: 'graph',
    effort: 'medium',
  },

  // Auth & Security - In Progress
  {
    id: 'auth-oauth',
    title: 'OAuth Login',
    description: 'Sign in with Claude and OpenAI',
    status: 'in-progress',
    category: 'auth',
    effort: 'medium',
  },

  // Auth & Security - Planned
  {
    id: 'auth-oauth-refresh',
    title: 'Token Refresh',
    description: 'Automatic OAuth token refresh',
    status: 'planned',
    category: 'auth',
    effort: 'small',
  },
  {
    id: 'auth-multiple',
    title: 'Multi-Provider Auth',
    description: 'Simultaneous OAuth for multiple providers',
    status: 'planned',
    category: 'auth',
    effort: 'medium',
  },

  // UI/UX - In Progress
  {
    id: 'ui-styling',
    title: 'Design System',
    description: 'Consistent color palette, typography, spacing',
    status: 'in-progress',
    category: 'ui',
    effort: 'large',
  },
  {
    id: 'ui-responsive',
    title: 'Responsive Design',
    description: 'Adaptive layouts for all screen sizes',
    status: 'in-progress',
    category: 'ui',
    effort: 'medium',
  },

  // UI/UX - Planned
  {
    id: 'ui-themes',
    title: 'Custom Themes',
    description: 'Dark, light, and accent color themes',
    status: 'planned',
    category: 'ui',
    effort: 'medium',
  },
  {
    id: 'ui-shortcuts',
    title: 'Keyboard Shortcuts',
    description: 'Configurable hotkeys and command palette',
    status: 'planned',
    category: 'ui',
    effort: 'medium',
  },
  {
    id: 'ui-animations',
    title: 'Micro-animations',
    description: 'Smooth transitions and feedback',
    status: 'planned',
    category: 'ui',
    effort: 'small',
  },

  // Integrations - Planned
  {
    id: 'int-mcp',
    title: 'MCP Server',
    description: 'Model Context Protocol for external tools',
    status: 'planned',
    category: 'core',
    effort: 'large',
  },
  {
    id: 'int-github',
    title: 'GitHub PR Integration',
    description: 'Preview PRs with code graph context',
    status: 'planned',
    category: 'core',
    effort: 'large',
  },
  {
    id: 'int-vscode',
    title: 'VS Code Extension',
    description: 'Native IDE integration',
    status: 'backlog',
    category: 'core',
    effort: 'large',
  },
];

const StatusIcon = ({ status }: { status: RoadmapItem['status'] }) => {
  switch (status) {
    case 'completed':
      return <CheckCircle2 className="w-4 h-4 text-[#30D158]" />;
    case 'in-progress':
      return <Clock className="w-4 h-4 text-[#FF9F0A] animate-spin" style={{ animationDuration: '2s' }} />;
    case 'planned':
      return <Circle className="w-4 h-4 text-[#0A84FF]" fill="none" />;
    case 'backlog':
      return <Circle className="w-4 h-4 text-text-muted/40" fill="none" />;
  }
};

const StatusBadge = ({ status }: { status: RoadmapItem['status'] }) => {
  const styles = {
    completed: 'bg-[#30D158]/10 border-[#30D158]/30 text-[#30D158]',
    'in-progress': 'bg-[#FF9F0A]/10 border-[#FF9F0A]/30 text-[#FF9F0A]',
    planned: 'bg-[#0A84FF]/10 border-[#0A84FF]/30 text-[#0A84FF]',
    backlog: 'bg-white/[0.04] border-white/[0.08] text-text-muted/50',
  };

  return (
    <span className={`px-2 py-0.5 rounded text-[10px] font-medium ${styles[status]}`}>
      {status === 'completed' && 'Done'}
      {status === 'in-progress' && 'In Progress'}
      {status === 'planned' && 'Planned'}
      {status === 'backlog' && 'Backlog'}
    </span>
  );
};

const EffortBadge = ({ effort }: { effort?: RoadmapItem['effort'] }) => {
  if (!effort) return null;

  const styles = {
    small: 'bg-green-500/10 border-green-500/30 text-green-400',
    medium: 'bg-yellow-500/10 border-yellow-500/30 text-yellow-400',
    large: 'bg-red-500/10 border-red-500/30 text-red-400',
  };

  return (
    <span className={`px-2 py-0.5 rounded text-[10px] ${styles[effort]}`}>
      {effort === 'small' && 'Small'}
      {effort === 'medium' && 'Medium'}
      {effort === 'large' && 'Large'}
    </span>
  );
};

interface RoadmapPanelProps {
  isOpen: boolean;
  onClose: () => void;
}

export const RoadmapPanel = ({ isOpen, onClose }: RoadmapPanelProps) => {
  const [selectedCategory, setSelectedCategory] = useState<string | null>(null);
  const [expandedCategories, setExpandedCategories] = useState<Set<string>>(new Set(['core', 'chat', 'graph', 'auth', 'ui']));

  const toggleCategory = (catId: string) => {
    setExpandedCategories(prev => {
      const next = new Set(prev);
      if (next.has(catId)) {
        next.delete(catId);
      } else {
        next.add(catId);
      }
      return next;
    });
  };

  const filteredItems = selectedCategory
    ? roadmapItems.filter(item => item.category === selectedCategory)
    : roadmapItems;

  const groupedItems = categories.reduce<Record<string, RoadmapItem[]>>((acc, cat) => {
    acc[cat.id] = filteredItems.filter(item => item.category === cat.id);
    return acc;
  }, {} as Record<string, RoadmapItem[]>);

  const stats = {
    completed: roadmapItems.filter(i => i.status === 'completed').length,
    inProgress: roadmapItems.filter(i => i.status === 'in-progress').length,
    planned: roadmapItems.filter(i => i.status === 'planned').length,
    backlog: roadmapItems.filter(i => i.status === 'backlog').length,
  };

  const completionRate = Math.round((stats.completed / roadmapItems.length) * 100);

  if (!isOpen) return null;

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center">
      <div className="absolute inset-0 bg-black/60 backdrop-blur-sm" onClick={onClose} />

      <div className="relative w-full max-w-4xl max-h-[85vh] mx-4 bg-[#1c1c1e]/98 backdrop-blur-xl border border-white/[0.08] rounded-2xl shadow-2xl overflow-hidden flex flex-col">
        {/* Header */}
        <div className="flex items-center justify-between px-6 py-4 border-b border-white/[0.08] bg-void/50">
          <div className="flex items-center gap-3">
            <div className="w-10 h-10 rounded-xl bg-gradient-to-br from-accent/20 to-accent/5 flex items-center justify-center">
              <Rocket className="w-5 h-5 text-accent" />
            </div>
            <div>
              <h2 className="text-[16px] font-semibold text-text-primary">Roadmap</h2>
              <p className="text-[11px] text-text-muted">
                {stats.completed} of {roadmapItems.length} features complete
                <span className="mx-2 text-white/[0.1]">•</span>
                {completionRate}%
              </p>
            </div>
          </div>
          <button
            onClick={onClose}
            className="p-2 text-text-muted hover:text-text-primary hover:bg-white/[0.08] rounded-lg transition-colors"
          >
            <svg className="w-5 h-5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M6 18L18 6M6 6l12 12" />
            </svg>
          </button>
        </div>

        {/* Progress Bar */}
        <div className="px-6 py-3 border-b border-white/[0.08] bg-void/30">
          <div className="flex items-center gap-3">
            <div className="flex-1 h-2 bg-white/[0.06] rounded-full overflow-hidden">
              <div
                className="h-full bg-gradient-to-r from-accent to-accent-dim rounded-full transition-all duration-500"
                style={{ width: `${completionRate}%` }}
              />
            </div>
            <div className="flex items-center gap-4 text-[11px]">
              <span className="flex items-center gap-1.5 text-[#30D158]">
                <CheckCircle2 className="w-3.5 h-3.5" />
                {stats.completed}
              </span>
              <span className="flex items-center gap-1.5 text-[#FF9F0A]">
                <Clock className="w-3.5 h-3.5" />
                {stats.inProgress}
              </span>
              <span className="flex items-center gap-1.5 text-[#0A84FF]">
                <Circle className="w-3.5 h-3.5" fill="none" />
                {stats.planned}
              </span>
            </div>
          </div>
        </div>

        {/* Category Filter */}
        <div className="px-6 py-3 border-b border-white/[0.08] flex items-center gap-2 overflow-x-auto">
          <button
            onClick={() => setSelectedCategory(null)}
            className={`px-3 py-1.5 rounded-full text-[12px] font-medium transition-all whitespace-nowrap ${
              !selectedCategory
                ? 'bg-accent text-white'
                : 'bg-white/[0.04] text-text-muted hover:text-text-secondary hover:bg-white/[0.08]'
            }`}
          >
            All
          </button>
          {categories.map(cat => (
            <button
              key={cat.id}
              onClick={() => setSelectedCategory(selectedCategory === cat.id ? null : cat.id)}
              className={`px-3 py-1.5 rounded-full text-[12px] font-medium transition-all flex items-center gap-1.5 whitespace-nowrap ${
                selectedCategory === cat.id
                  ? 'bg-white/[0.08] text-text-primary'
                  : 'bg-white/[0.04] text-text-muted hover:text-text-secondary hover:bg-white/[0.08]'
              }`}
              style={{
                ...(selectedCategory === cat.id && { borderColor: cat.color, borderWidth: '1px' }),
              }}
            >
              <cat.icon className="w-3.5 h-3.5" style={{ color: selectedCategory === cat.id ? cat.color : 'inherit' }} />
              {cat.name}
            </button>
          ))}
        </div>

        {/* Content */}
        <div className="flex-1 overflow-y-auto px-6 py-4 scrollbar-thin">
          {!selectedCategory ? (
            <div className="space-y-4">
              {categories.map(cat => {
                const items = groupedItems[cat.id];
                if (!items || items.length === 0) return null;

                const isExpanded = expandedCategories.has(cat.id);
                const completedCount = items.filter(i => i.status === 'completed').length;
                const categoryProgress = Math.round((completedCount / items.length) * 100);

                return (
                  <div key={cat.id} className="border border-white/[0.08] rounded-xl overflow-hidden bg-white/[0.02]">
                    {/* Category Header */}
                    <button
                      onClick={() => toggleCategory(cat.id)}
                      className="w-full flex items-center justify-between px-4 py-3 hover:bg-white/[0.04] transition-colors"
                    >
                      <div className="flex items-center gap-3">
                        <div
                          className="w-8 h-8 rounded-lg flex items-center justify-center"
                          style={{ backgroundColor: cat.color + '20' }}
                        >
                          <cat.icon className="w-4 h-4" style={{ color: cat.color }} />
                        </div>
                        <div className="text-left">
                          <h3 className="text-[13px] font-semibold text-text-primary">{cat.name}</h3>
                          <p className="text-[11px] text-text-muted">
                            {completedCount} / {items.length} features
                          </p>
                        </div>
                      </div>
                      <div className="flex items-center gap-3">
                        <div className="w-16 h-1.5 bg-white/[0.06] rounded-full overflow-hidden">
                          <div
                            className="h-full rounded-full transition-all"
                            style={{ width: `${categoryProgress}%`, backgroundColor: cat.color }}
                          />
                        </div>
                        {isExpanded ? (
                          <ChevronDown className="w-4 h-4 text-text-muted/60" />
                        ) : (
                          <ChevronRight className="w-4 h-4 text-text-muted/60" />
                        )}
                      </div>
                    </button>

                    {/* Items */}
                    {isExpanded && (
                      <div className="px-4 pb-3 space-y-2">
                        {items.map(item => (
                          <div
                            key={item.id}
                            className="flex items-start gap-3 p-3 rounded-lg bg-white/[0.02] border border-white/[0.04] hover:border-white/[0.08] transition-colors"
                          >
                            <StatusIcon status={item.status} />
                            <div className="flex-1 min-w-0">
                              <div className="flex items-center gap-2 mb-1">
                                <h4 className="text-[12px] font-medium text-text-primary truncate">
                                  {item.title}
                                </h4>
                                <StatusBadge status={item.status} />
                                {item.effort && <EffortBadge effort={item.effort} />}
                              </div>
                              <p className="text-[11px] text-text-muted leading-relaxed">
                                {item.description}
                              </p>
                            </div>
                          </div>
                        ))}
                      </div>
                    )}
                  </div>
                );
              })}
            </div>
          ) : (
            <div className="space-y-2">
              {filteredItems.map(item => (
                <div
                  key={item.id}
                  className="flex items-start gap-3 p-4 rounded-xl bg-white/[0.02] border border-white/[0.04] hover:border-white/[0.08] transition-colors"
                >
                  <StatusIcon status={item.status} />
                  <div className="flex-1 min-w-0">
                    <div className="flex items-center gap-2 mb-1">
                      <h4 className="text-[13px] font-medium text-text-primary truncate">
                        {item.title}
                      </h4>
                      <StatusBadge status={item.status} />
                      {item.effort && <EffortBadge effort={item.effort} />}
                    </div>
                    <p className="text-[12px] text-text-muted leading-relaxed">
                      {item.description}
                    </p>
                  </div>
                </div>
              ))}
            </div>
          )}
        </div>

        {/* Footer */}
        <div className="px-6 py-3 border-t border-white/[0.08] bg-void/30 flex items-center justify-between">
          <p className="text-[11px] text-text-muted">
            Have a feature request? <a href="https://github.com/neur0map/prowl/issues" target="_blank" rel="noopener noreferrer" className="text-accent hover:text-accent-dim transition-colors">Open an issue</a>
          </p>
          <div className="flex items-center gap-2 text-[11px] text-text-muted/60">
            <span className="flex items-center gap-1">
              <div className="w-2 h-2 rounded-full bg-[#30D158]" />
              Done
            </span>
            <span className="flex items-center gap-1">
              <div className="w-2 h-2 rounded-full bg-[#FF9F0A]" />
              In Progress
            </span>
            <span className="flex items-center gap-1">
              <div className="w-2 h-2 rounded-full bg-[#0A84FF]" />
              Planned
            </span>
          </div>
        </div>
      </div>
    </div>
  );
};
