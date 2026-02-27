import { useState, type ReactNode } from 'react';
import { ChevronDown, ChevronRight, Circle, Check, Loader2, AlertCircle } from 'lucide-react';
import type { ToolCallInfo } from '../core/llm/types';

interface ToolCallCardProps {
  toolCall: ToolCallInfo;
  defaultExpanded?: boolean;
}

/* ── Display name mapping for each tool ─────────────── */

const DISPLAY_NAMES: Record<string, string> = {
  search: '🔍 Search Code',
  cypher: '🔗 Cypher Query',
  grep: '🔎 Pattern Search',
  read: '📄 Read File',
  overview: '🗺️ Codebase Overview',
  explore: '🔬 Deep Dive',
  impact: '💥 Impact Analysis',
};

/* ── Visual styling per status ──────────────────────── */

type VisualStyle = {
  indicator: ReactNode;
  textClass: string;
  bgClass: string;
  border: string;
};

function styleForStatus(st: ToolCallInfo['status']): VisualStyle {
  const styles: Record<string, VisualStyle> = {
    running: {
      indicator: <Loader2 className="w-3.5 h-3.5 animate-spin" />,
      textClass: 'text-amber-400',
      bgClass: 'bg-amber-500/10',
      border: 'border-amber-500/30',
    },
    completed: {
      indicator: <Check className="w-3.5 h-3.5" />,
      textClass: 'text-emerald-400',
      bgClass: 'bg-emerald-500/10',
      border: 'border-emerald-500/30',
    },
    error: {
      indicator: <AlertCircle className="w-3.5 h-3.5" />,
      textClass: 'text-rose-400',
      bgClass: 'bg-rose-500/10',
      border: 'border-rose-500/30',
    },
  };

  return styles[st] ?? {
    indicator: <Circle className="w-3.5 h-3.5" />,
    textClass: 'text-text-muted',
    bgClass: 'bg-white/[0.04]',
    border: 'border-white/[0.1]',
  };
}

/* ── Argument formatting ────────────────────────────── */

function formatArguments(toolName: string, args: Record<string, unknown>): string {
  if (!args || Object.keys(args).length === 0) return '';

  const hasCypher = 'cypher' in args && typeof args.cypher === 'string';
  if (hasCypher) {
    const prefix = 'query' in args && typeof args.query === 'string'
      ? `Search: "${args.query}"\n\n`
      : '';
    return prefix + args.cypher;
  }

  if ('query' in args && typeof args.query === 'string') return args.query;

  return JSON.stringify(args, null, 2);
}

/* ── Expanded detail pane ───────────────────────────── */

function expandedContent(
  tc: ToolCallInfo,
  argDisplay: string,
) {
  const inputHeading = tc.name === 'cypher' ? 'Query' : 'Input';
  const truncatedResult = tc.result && tc.result.length > 3000
    ? tc.result.slice(0, 3000) + '\n\n... (truncated)'
    : tc.result;

  return (
    <div className="border-t border-border-subtle/50">
      {argDisplay.length > 0 && (
        <div className="px-3 py-2 border-b border-border-subtle/50">
          <div className="text-[10px] uppercase tracking-wider text-text-muted mb-1.5">
            {inputHeading}
          </div>
          <pre className="text-xs text-text-secondary bg-surface/50 rounded p-2 overflow-x-auto whitespace-pre-wrap font-mono">
            {argDisplay}
          </pre>
        </div>
      )}

      {truncatedResult && (
        <div className="px-3 py-2">
          <div className="text-[10px] uppercase tracking-wider text-text-muted mb-1.5">
            Result
          </div>
          <div className="max-h-[400px] overflow-y-auto bg-surface/50 rounded">
            <pre className="text-xs text-text-secondary p-2 whitespace-pre-wrap font-mono">
              {truncatedResult}
            </pre>
          </div>
        </div>
      )}

      {tc.status === 'running' && !tc.result && (
        <div className="px-3 py-3 flex items-center gap-2 text-xs text-text-muted">
          <Loader2 className="w-3 h-3 animate-spin" />
          <span>Executing...</span>
        </div>
      )}
    </div>
  );
}

/* ── Card component ─────────────────────────────────── */

export const ToolCallCard = ({ toolCall: tc, defaultExpanded = false }: ToolCallCardProps) => {
  const [open, setOpen] = useState(defaultExpanded);

  const visual = styleForStatus(tc.status);
  const argDisplay = formatArguments(tc.name, tc.args);
  const label = DISPLAY_NAMES[tc.name] ?? tc.name;

  const toggle = () => setOpen(prev => !prev);
  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      toggle();
    }
  };

  const outerClasses = `rounded-lg border ${visual.border} ${visual.bgClass} overflow-hidden transition-all`;
  const chevron = open
    ? <ChevronDown className="w-4 h-4" />
    : <ChevronRight className="w-4 h-4" />;

  return (
    <div className={outerClasses}>
      <div
        role="button"
        tabIndex={0}
        onClick={toggle}
        onKeyDown={handleKeyDown}
        className="w-full flex items-center gap-2 px-3 py-2 text-left hover:bg-white/5 transition-colors cursor-pointer select-none"
      >
        <span className="text-text-muted">{chevron}</span>
        <span className="flex-1 text-sm font-medium text-text-primary">{label}</span>
        <span className={`flex items-center gap-1 text-xs ${visual.textClass}`}>
          {visual.indicator}
          <span className="capitalize">{tc.status}</span>
        </span>
      </div>
      {open && expandedContent(tc, argDisplay)}
    </div>
  );
};

export default ToolCallCard;
