import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Code, PanelLeftClose, PanelLeft, Trash2, X, Target, FileCode, MessageSquare, MousePointerClick } from 'lucide-react';
import { Prism as SyntaxHighlighter } from 'react-syntax-highlighter';
import { vscDarkPlus } from 'react-syntax-highlighter/dist/esm/styles/prism';
import { useAppState } from '../hooks/useAppState';
import { NODE_COLORS } from '../lib/constants';

const codeTheme = {
  ...vscDarkPlus,
  'pre[class*="language-"]': {
    ...vscDarkPlus['pre[class*="language-"]'],
    background: '#0a0a10',
    margin: 0,
    padding: '12px 0',
    fontSize: '13px',
    lineHeight: '1.6',
  },
  'code[class*="language-"]': {
    ...vscDarkPlus['code[class*="language-"]'],
    background: 'transparent',
    fontFamily: '"JetBrains Mono", "Fira Code", monospace',
  },
};

function inferLanguage(filePath: string): string {
  if (filePath.endsWith('.py')) return 'python';
  if (filePath.endsWith('.js') || filePath.endsWith('.jsx')) return 'javascript';
  if (filePath.endsWith('.ts') || filePath.endsWith('.tsx')) return 'typescript';
  return 'text';
}

function buildLineProps(isHighlighted: boolean) {
  return {
    style: {
      display: 'block' as const,
      backgroundColor: isHighlighted ? 'rgba(6, 182, 212, 0.14)' : 'transparent',
      borderLeft: isHighlighted ? '3px solid #06b6d4' : '3px solid transparent',
      paddingLeft: '12px',
      paddingRight: '16px',
    },
  };
}

const LINE_NUMBER_STYLE = {
  minWidth: '3em',
  paddingRight: '1em',
  color: '#5a5a70',
  textAlign: 'right' as const,
  userSelect: 'none' as const,
};

const PANEL_WIDTH_MIN = 420;
const PANEL_WIDTH_MAX = 900;
const PANEL_WIDTH_DEFAULT = 560;
const STORAGE_KEY = 'prowl.codePanelWidth';

function loadSavedWidth(): number {
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    const num = raw ? parseInt(raw, 10) : NaN;
    if (!Number.isFinite(num)) return PANEL_WIDTH_DEFAULT;
    return Math.max(PANEL_WIDTH_MIN, Math.min(num, PANEL_WIDTH_MAX));
  } catch {
    return PANEL_WIDTH_DEFAULT;
  }
}

export interface CodeReferencesPanelProps {
  onFocusNode: (nodeId: string) => void;
}

interface SnippetData {
  ref: ReturnType<typeof useAppState>['codeReferences'][number];
  snippet: string | null;
  lineStart: number;
  lineEnd: number;
  hlStart: number;
  hlEnd: number;
  fileLineCount: number;
}

function SelectedFileViewer({
  node,
  fileContent,
  filePath,
  isFileLabel,
  onClearSelection,
}: {
  node: NonNullable<ReturnType<typeof useAppState>['selectedNode']>;
  fileContent: string | undefined;
  filePath: string;
  isFileLabel: boolean;
  onClearSelection: () => void;
}) {
  const lang = inferLanguage(filePath);
  const sLine = node.properties?.startLine;
  const eLine = node.properties?.endLine ?? sLine;

  return (
    <>
      <div className="px-3 py-2 bg-[#FF9F0A]/5 border-b border-[#FF9F0A]/15 flex items-center gap-2">
        <div className="flex items-center gap-1.5 px-2 py-0.5 bg-amber-500/15 rounded-md border border-amber-500/25">
          <MousePointerClick className="w-3 h-3 text-amber-400" />
          <span className="text-[10px] text-amber-300 font-semibold uppercase tracking-wide">Selected</span>
        </div>
        <FileCode className="w-3.5 h-3.5 text-amber-400/70 ml-1" />
        <span className="text-xs text-text-primary font-mono truncate flex-1">
          {filePath.split('/').pop() ?? node.properties?.name}
        </span>
        <button
          onClick={onClearSelection}
          className="p-1 text-text-muted hover:text-amber-400 hover:bg-amber-500/10 rounded transition-colors"
          title="Clear selection"
        >
          <X className="w-4 h-4" />
        </button>
      </div>
      <div className="flex-1 min-h-0 overflow-auto scrollbar-thin">
        {fileContent ? (
          <SyntaxHighlighter
            language={lang}
            style={codeTheme as any}
            showLineNumbers
            startingLineNumber={1}
            lineNumberStyle={LINE_NUMBER_STYLE}
            lineProps={(ln: number) => {
              const hl = typeof sLine === 'number'
                && ln >= sLine + 1
                && ln <= (eLine ?? sLine) + 1;
              return buildLineProps(hl);
            }}
            wrapLines
          >
            {fileContent}
          </SyntaxHighlighter>
        ) : (
          <div className="px-3 py-3 text-sm text-text-muted">
            {isFileLabel
              ? <>Code not available in memory for <span className="font-mono">{filePath}</span></>
              : <>Select a file node to preview its contents.</>}
          </div>
        )}
      </div>
    </>
  );
}

function CitationCard({
  data,
  isGlowing,
  graphNodes,
  onFocusNode,
  onSelectNode,
  onRemove,
  cardRef,
}: {
  data: SnippetData;
  isGlowing: boolean;
  graphNodes: ReturnType<typeof useAppState>['graph'];
  onFocusNode: (id: string) => void;
  onSelectNode: (n: any) => void;
  onRemove: (id: string) => void;
  cardRef: (el: HTMLDivElement | null) => void;
}) {
  const { ref: r, snippet, lineStart, hlStart, hlEnd, fileLineCount } = data;
  const nodeColor = r.label ? (NODE_COLORS as any)[r.label] || '#6b7280' : '#6b7280';
  const hasLineRange = typeof r.startLine === 'number';
  const lang = inferLanguage(r.filePath);

  const lineFrom = hasLineRange ? (r.startLine ?? 0) + 1 : undefined;
  const lineTo = hasLineRange ? (r.endLine ?? r.startLine ?? 0) + 1 : undefined;

  const containerCls = [
    'bg-elevated border border-border-subtle rounded-xl overflow-hidden transition-all',
    isGlowing ? 'ring-1 ring-accent/40' : '',
  ].join(' ');

  return (
    <div ref={cardRef} className={containerCls}>
      <div className="px-3 py-2 border-b border-border-subtle bg-surface/40 flex items-start gap-2">
        <span
          className="mt-0.5 px-2 py-0.5 rounded text-[10px] font-semibold uppercase tracking-wide flex-shrink-0"
          style={{ backgroundColor: nodeColor, color: '#06060a' }}
          title={r.label ?? 'Code'}
        >
          {r.label ?? 'Code'}
        </span>
        <div className="min-w-0 flex-1">
          <div className="text-xs text-text-primary font-medium truncate">
            {r.name ?? r.filePath.split('/').pop() ?? r.filePath}
          </div>
          <div className="text-[11px] text-text-muted font-mono truncate">
            {r.filePath}
            {lineFrom !== undefined && (
              <span className="text-text-secondary">
                {' '}• L{lineFrom}{lineTo !== lineFrom ? `–${lineTo}` : ''}
              </span>
            )}
            {fileLineCount > 0 && <span className="text-text-muted"> • {fileLineCount} lines</span>}
          </div>
        </div>
        <div className="flex items-center gap-1">
          {r.nodeId && (
            <button
              onClick={() => {
                const nid = r.nodeId!;
                if (graphNodes) {
                  const match = graphNodes.nodes.find(n => n.id === nid);
                  if (match) onSelectNode(match);
                }
                onFocusNode(nid);
              }}
              className="p-1.5 text-text-muted hover:text-text-primary hover:bg-hover rounded transition-colors"
              title="Focus in graph"
            >
              <Target className="w-4 h-4" />
            </button>
          )}
          <button
            onClick={() => onRemove(r.id)}
            className="p-1.5 text-text-muted hover:text-text-primary hover:bg-hover rounded transition-colors"
            title="Remove"
          >
            <X className="w-4 h-4" />
          </button>
        </div>
      </div>
      <div className="overflow-x-auto">
        {snippet ? (
          <SyntaxHighlighter
            language={lang}
            style={codeTheme as any}
            showLineNumbers
            startingLineNumber={lineStart + 1}
            lineNumberStyle={LINE_NUMBER_STYLE}
            lineProps={(ln: number) => {
              const hl = hasLineRange
                && ln >= lineStart + hlStart + 1
                && ln <= lineStart + hlEnd + 1;
              return buildLineProps(hl);
            }}
            wrapLines
          >
            {snippet}
          </SyntaxHighlighter>
        ) : (
          <div className="px-3 py-3 text-sm text-text-muted">
            Code not available in memory for <span className="font-mono">{r.filePath}</span>
          </div>
        )}
      </div>
    </div>
  );
}

export const CodeReferencesPanel = ({ onFocusNode }: CodeReferencesPanelProps) => {
  const {
    graph,
    fileContents,
    selectedNode,
    codeReferences,
    removeCodeReference,
    clearCodeReferences,
    setSelectedNode,
    codeReferenceFocus,
  } = useAppState();

  const [collapsed, setCollapsed] = useState(false);
  const [highlightId, setHighlightId] = useState<string | null>(null);
  const containerRef = useRef<HTMLElement | null>(null);
  const dragState = useRef<{ startX: number; startWidth: number } | null>(null);
  const cardRefs = useRef<Map<string, HTMLDivElement | null>>(new Map());
  const highlightTimer = useRef<number | null>(null);

  useEffect(() => {
    return () => {
      if (highlightTimer.current) {
        window.clearTimeout(highlightTimer.current);
        highlightTimer.current = null;
      }
    };
  }, []);

  const [width, setWidth] = useState<number>(loadSavedWidth);

  useEffect(() => {
    try { window.localStorage.setItem(STORAGE_KEY, String(width)); } catch { /* noop */ }
  }, [width]);

  const initiateResize = useCallback((e: React.MouseEvent) => {
    e.preventDefault();
    e.stopPropagation();
    dragState.current = { startX: e.clientX, startWidth: width };
    document.body.style.cursor = 'col-resize';
    document.body.style.userSelect = 'none';

    const handleMove = (ev: MouseEvent) => {
      const s = dragState.current;
      if (!s) return;
      const dx = ev.clientX - s.startX;
      setWidth(Math.max(PANEL_WIDTH_MIN, Math.min(s.startWidth + dx, PANEL_WIDTH_MAX)));
    };

    const handleUp = () => {
      dragState.current = null;
      document.body.style.cursor = '';
      document.body.style.userSelect = '';
      window.removeEventListener('mousemove', handleMove);
      window.removeEventListener('mouseup', handleUp);
    };

    window.addEventListener('mousemove', handleMove);
    window.addEventListener('mouseup', handleUp);
  }, [width]);

  const aiRefs = useMemo(
    () => codeReferences.filter(r => r.source === 'ai'),
    [codeReferences],
  );

  useEffect(() => {
    if (!codeReferenceFocus) return;

    setCollapsed(false);

    const { filePath, startLine, endLine } = codeReferenceFocus;
    const matched =
      aiRefs.find(r => r.filePath === filePath && r.startLine === startLine && r.endLine === endLine)
      ?? aiRefs.find(r => r.filePath === filePath);

    if (!matched) return;

    requestAnimationFrame(() => {
      requestAnimationFrame(() => {
        const el = cardRefs.current.get(matched.id);
        if (!el) return;

        el.scrollIntoView({ behavior: 'smooth', block: 'center' });
        setHighlightId(matched.id);

        if (highlightTimer.current) window.clearTimeout(highlightTimer.current);
        highlightTimer.current = window.setTimeout(() => {
          setHighlightId(prev => (prev === matched.id ? null : prev));
          highlightTimer.current = null;
        }, 1200);
      });
    });
  }, [codeReferenceFocus?.ts, aiRefs]);

  const snippets = useMemo<SnippetData[]>(() => {
    return aiRefs.map(ref => {
      const raw = fileContents.get(ref.filePath);
      if (!raw) return { ref, snippet: null, lineStart: 0, lineEnd: 0, hlStart: 0, hlEnd: 0, fileLineCount: 0 };

      const allLines = raw.split('\n');
      const total = allLines.length;
      const sLine = ref.startLine ?? 0;
      const eLine = ref.endLine ?? sLine;
      const from = Math.max(0, sLine - 3);
      const to = Math.min(total - 1, eLine + 20);

      return {
        ref,
        snippet: allLines.slice(from, to + 1).join('\n'),
        lineStart: from,
        lineEnd: to,
        hlStart: Math.max(0, sLine - from),
        hlEnd: Math.max(0, eLine - from),
        fileLineCount: total,
      };
    });
  }, [aiRefs, fileContents]);

  const activeFilePath = selectedNode?.properties?.filePath;
  const activeContent = activeFilePath ? fileContents.get(activeFilePath) : undefined;
  const isFileNode = selectedNode?.label === 'File' && !!activeFilePath;
  const hasSelectedViewer = !!selectedNode && !!activeFilePath;
  const hasCitations = aiRefs.length > 0;

  if (collapsed) {
    return (
      <aside className="h-full w-12 bg-surface border-r border-border-subtle flex flex-col items-center py-3 gap-2 flex-shrink-0">
        <button
          onClick={() => setCollapsed(false)}
          className="p-2 text-text-secondary hover:text-cyan-400 hover:bg-cyan-500/10 rounded transition-colors"
          title="Expand Code Panel"
        >
          <PanelLeft className="w-5 h-5" />
        </button>
        <div className="w-6 h-px bg-border-subtle my-1" />
        {hasSelectedViewer && (
          <div className="text-[9px] text-amber-400 rotate-90 whitespace-nowrap font-medium tracking-wide">
            SELECTED
          </div>
        )}
        {hasCitations && (
          <div className="text-[9px] text-cyan-400 rotate-90 whitespace-nowrap font-medium tracking-wide mt-4">
            AI • {aiRefs.length}
          </div>
        )}
      </aside>
    );
  }

  return (
    <aside
      ref={el => { containerRef.current = el; }}
      className="h-full glass border-r border-white/[0.08] flex flex-col animate-fade-in relative"
      style={{ width }}
    >
      <div
        onMouseDown={initiateResize}
        className="absolute top-0 right-0 h-full w-2 cursor-col-resize bg-transparent hover:bg-cyan-500/25 transition-colors"
        title="Drag to resize"
      />

      <div className="flex items-center justify-between px-3 py-2.5 border-b border-white/[0.08]">
        <div className="flex items-center gap-2">
          <Code className="w-4 h-4 text-cyan-400" />
          <span className="text-sm font-semibold text-text-primary">Code Inspector</span>
        </div>
        <div className="flex items-center gap-1.5">
          {hasCitations && (
            <button
              onClick={() => clearCodeReferences()}
              className="p-1.5 text-text-muted hover:text-red-400 hover:bg-red-500/10 rounded transition-colors"
              title="Clear AI citations"
            >
              <Trash2 className="w-4 h-4" />
            </button>
          )}
          <button
            onClick={() => setCollapsed(true)}
            className="p-1.5 text-text-muted hover:text-text-primary hover:bg-hover rounded transition-colors"
            title="Collapse Panel"
          >
            <PanelLeftClose className="w-4 h-4" />
          </button>
        </div>
      </div>

      <div className="flex-1 min-h-0 flex flex-col">
        {hasSelectedViewer && (
          <div className={`${hasCitations ? 'h-[42%]' : 'flex-1'} min-h-0 flex flex-col`}>
            <SelectedFileViewer
              node={selectedNode!}
              fileContent={activeContent}
              filePath={activeFilePath!}
              isFileLabel={isFileNode}
              onClearSelection={() => setSelectedNode(null)}
            />
          </div>
        )}

        {hasSelectedViewer && hasCitations && <div className="h-px bg-white/[0.08]" />}

        {hasCitations && (
          <div className="flex-1 min-h-0 flex flex-col">
            <div className="px-3 py-2 bg-accent/5 border-b border-accent/15 flex items-center gap-2">
              <div className="flex items-center gap-1.5 px-2 py-0.5 bg-accent/10 rounded-md border border-accent/20">
                <MessageSquare className="w-3 h-3 text-accent" />
                <span className="text-[10px] text-accent font-medium uppercase tracking-wide">AI Citations</span>
              </div>
              <span className="text-xs text-text-muted ml-1">
                {aiRefs.length} reference{aiRefs.length !== 1 ? 's' : ''}
              </span>
            </div>
            <div className="flex-1 min-h-0 overflow-y-auto scrollbar-thin p-3 space-y-3">
              {snippets.map(data => (
                <CitationCard
                  key={data.ref.id}
                  data={data}
                  isGlowing={highlightId === data.ref.id}
                  graphNodes={graph}
                  onFocusNode={onFocusNode}
                  onSelectNode={setSelectedNode}
                  onRemove={removeCodeReference}
                  cardRef={el => { cardRefs.current.set(data.ref.id, el); }}
                />
              ))}
            </div>
          </div>
        )}
      </div>
    </aside>
  );
};
