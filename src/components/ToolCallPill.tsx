import { useState } from 'react'
import { Check, Loader2, AlertCircle, Search, Database, FileText, Globe, Zap, ChevronDown, ChevronRight } from 'lucide-react'
import type { ToolCallInfo } from '../core/llm/types'

const TOOL_ICONS: Record<string, typeof Search> = {
  search: Search,
  cypher: Database,
  grep: Search,
  read: FileText,
  overview: Globe,
  explore: Zap,
  impact: Zap,
}

const TOOL_LABELS: Record<string, string> = {
  search: 'Searched code',
  cypher: 'Queried graph',
  grep: 'Pattern search',
  read: 'Read file',
  overview: 'Codebase overview',
  explore: 'Deep dive',
  impact: 'Impact analysis',
}

const formatArgs = (args: Record<string, unknown>): string => {
  if (!args || Object.keys(args).length === 0) return ''
  if ('cypher' in args && typeof args.cypher === 'string') {
    let r = ''
    if ('query' in args && typeof args.query === 'string') r += `Search: "${args.query}"\n\n`
    r += args.cypher
    return r
  }
  if ('query' in args && typeof args.query === 'string') return args.query
  return JSON.stringify(args, null, 2)
}

interface ToolCallPillProps {
  toolCall: ToolCallInfo
}

export const ToolCallPill = ({ toolCall }: ToolCallPillProps) => {
  const [expanded, setExpanded] = useState(false)
  const Icon = TOOL_ICONS[toolCall.name] || Zap
  const label = TOOL_LABELS[toolCall.name] || toolCall.name
  const args = formatArgs(toolCall.args)

  return (
    <div className="group">
      <button
        onClick={() => setExpanded(e => !e)}
        className={`
          inline-flex items-center gap-1.5 px-2.5 py-1 rounded-md text-[12px]
          transition-all cursor-pointer select-none
          ${toolCall.status === 'running'
            ? 'bg-accent/10 border border-accent/20 text-accent'
            : toolCall.status === 'error'
              ? 'bg-[#FF453A]/10 border border-[#FF453A]/20 text-[#FF453A]'
              : 'bg-white/[0.04] border border-white/[0.08] text-text-secondary hover:bg-white/[0.06]'
          }
        `}
      >
        {toolCall.status === 'running' ? (
          <Loader2 size={12} className="animate-spin shrink-0" />
        ) : toolCall.status === 'error' ? (
          <AlertCircle size={12} className="shrink-0" />
        ) : (
          <Icon size={12} className="shrink-0 opacity-60" />
        )}

        <span>
          {toolCall.status === 'running'
            ? label.replace(/^[A-Z]/, c => c.toLowerCase()).replace(/ed /, 'ing ').replace(/Searched/, 'Searching').replace(/Queried/, 'Querying').replace(/Read/, 'Reading')
            : label
          }
        </span>

        {toolCall.status === 'completed' && (
          <Check size={11} className="text-[#30D158] shrink-0" />
        )}

        {toolCall.status === 'error' && (
          <span className="text-[10px]">Failed</span>
        )}

        {expanded ? (
          <ChevronDown size={11} className="shrink-0 opacity-40" />
        ) : (
          <ChevronRight size={11} className="shrink-0 opacity-40" />
        )}
      </button>

      {expanded && (
        <div className="mt-1.5 ml-1 rounded-md glass-subtle border border-white/[0.06] overflow-hidden text-[12px] max-h-[400px] overflow-y-auto scrollbar-thin">
          {args && (
            <div className="px-3 py-2 border-b border-white/[0.06]">
              <div className="text-[10px] uppercase tracking-wider text-text-muted mb-1">Input</div>
              <pre className="text-text-secondary font-mono whitespace-pre-wrap break-words max-h-[120px] overflow-y-auto scrollbar-thin">{args}</pre>
            </div>
          )}
          {toolCall.result && (
            <div className="px-3 py-2">
              <div className="text-[10px] uppercase tracking-wider text-text-muted mb-1">Result</div>
              <pre className="text-text-secondary font-mono whitespace-pre-wrap break-words max-h-[260px] overflow-y-auto scrollbar-thin">
                {toolCall.result.length > 4000
                  ? toolCall.result.slice(0, 4000) + '\n... (truncated)'
                  : toolCall.result
                }
              </pre>
            </div>
          )}
          {toolCall.status === 'running' && !toolCall.result && (
            <div className="px-3 py-2 flex items-center gap-1.5 text-text-muted">
              <Loader2 size={11} className="animate-spin" /> Running...
            </div>
          )}
        </div>
      )}
    </div>
  )
}
