import { useCallback, useEffect, useRef, useState } from 'react';
import mermaid from 'mermaid';
import { AlertTriangle, Maximize2 } from 'lucide-react';
import { ProcessFlowModal } from './ProcessFlowModal';
import type { ProcessData } from '../lib/mermaid-generator';

/* ── Mermaid global config ──────────────────────────── */

mermaid.initialize({
  startOnLoad: false,
  maxTextSize: 900000,
  theme: 'dark',
  flowchart: {
    curve: 'monotoneX',
    padding: 20,
    nodeSpacing: 60,
    rankSpacing: 60,
    htmlLabels: true,
  },
  sequence: {
    actorMargin: 50,
    boxMargin: 10,
    boxTextMargin: 5,
    noteMargin: 10,
    messageMargin: 35,
  },
  fontFamily: '"JetBrains Mono", "Fira Code", monospace',
  fontSize: 12,
  suppressErrorRendering: true,
});

mermaid.parseError = (_err) => {};

/* ── Component ──────────────────────────────────────── */

interface MermaidDiagramProps {
  code: string;
}

export const MermaidDiagram = ({ code }: MermaidDiagramProps) => {
  const wrapperRef = useRef<HTMLDivElement>(null);
  const sourceRef = useRef(code);
  const frameRef = useRef<number>(0);
  const [error, setError] = useState<string | null>(null);
  const [expanded, setExpanded] = useState(false);
  const [svg, setSvg] = useState<string>('');
  const [detailsHidden, setDetailsHidden] = useState(true);

  sourceRef.current = code;

  const drawChart = useCallback(async () => {
    if (!wrapperRef.current) return;

    const snapshot = sourceRef.current;

    try {
      const id = `mermaid-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
      const offscreen = document.createElement('div');
      const { svg: rendered } = await mermaid.render(id, snapshot.trim(), offscreen);
      setSvg(rendered);
      setError(null);
    } catch (err) {
      console.debug('Mermaid render skipped (incomplete):', err);
    }
  }, []);

  useEffect(() => {
    frameRef.current = requestAnimationFrame(() => {
      drawChart();
    });
    return () => cancelAnimationFrame(frameRef.current);
  }, [code, drawChart]);

  const modalPayload: any = expanded
    ? {
        id: 'ai-generated',
        label: 'AI Generated Diagram',
        processType: 'intra_community',
        steps: [],
        edges: [],
        clusters: [],
        rawMermaid: code,
      }
    : null;

  if (error) {
    return (
      <div className="my-3 p-4 bg-rose-500/10 border border-rose-500/30 rounded-lg">
        <button
          onClick={() => setDetailsHidden((prev) => !prev)}
          className="flex items-center gap-2 text-rose-300 text-sm w-full text-left"
        >
          <AlertTriangle className="w-4 h-4 flex-shrink-0" />
          <span className="font-medium">Diagram Error</span>
          <span className="ml-auto text-xs text-rose-200/50">
            {detailsHidden ? 'Show details' : 'Hide details'}
          </span>
        </button>
        {!detailsHidden && (
          <div className="mt-3 space-y-2">
            <pre className="text-xs text-rose-200/70 font-mono whitespace-pre-wrap">{error}</pre>
            <pre className="p-2 bg-surface rounded text-xs text-text-muted overflow-x-auto">
              {code}
            </pre>
          </div>
        )}
      </div>
    );
  }

  return (
    <>
      <div className="my-3 relative group">
        <div className="relative bg-gradient-to-b from-surface to-elevated border border-border-subtle rounded-xl overflow-hidden">
          <div className="flex items-center justify-between px-3 py-2 bg-surface/60 border-b border-border-subtle">
            <span className="text-[10px] text-text-muted uppercase tracking-wider font-medium">
              Diagram
            </span>
            <button
              onClick={() => setExpanded(true)}
              className="p-1 text-text-muted hover:text-text-primary hover:bg-hover rounded transition-colors"
              title="Expand"
            >
              <Maximize2 className="w-3.5 h-3.5" />
            </button>
          </div>

          <div
            ref={wrapperRef}
            className="flex items-center justify-center p-4 overflow-x-auto max-h-[400px]"
            dangerouslySetInnerHTML={{ __html: svg }}
          />
        </div>
      </div>

      {expanded && modalPayload && (
        <ProcessFlowModal
          process={modalPayload}
          onClose={() => setExpanded(false)}
        />
      )}
    </>
  );
};
