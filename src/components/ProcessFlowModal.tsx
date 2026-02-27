/**
 * Displays a process flow as either a native pill chain (single processes)
 * or a pan-and-zoom Mermaid diagram (combined / fullscreen views).
 */

import { useEffect, useRef, useCallback, useState } from 'react';
import { X, Copy, Focus, ZoomIn, ZoomOut } from 'lucide-react';
import mermaid from 'mermaid';
import { ProcessData, generateProcessMermaid } from '../lib/mermaid-generator';

interface ProcessFlowModalProps {
  process: ProcessData | null;
  onClose: () => void;
  onFocusInGraph?: (nodeIds: string[], processId: string) => void;
  isFullScreen?: boolean;
}

/* ── Mermaid dark monochrome theme ──────────────────── */

mermaid.initialize({
  startOnLoad: false,
  suppressErrorRendering: true,
  maxTextSize: 900000,
  theme: 'base',
  themeVariables: {
    primaryColor: '#1c1c1e',
    primaryTextColor: '#f5f5f7',
    primaryBorderColor: 'rgba(255,255,255,0.12)',
    lineColor: 'rgba(255,255,255,0.2)',
    secondaryColor: '#1c1c1e',
    tertiaryColor: '#1c1c1e',
    mainBkg: '#1c1c1e',
    nodeBorder: 'rgba(255,255,255,0.12)',
    clusterBkg: 'rgba(255,255,255,0.03)',
    clusterBorder: 'rgba(255,255,255,0.08)',
    titleColor: '#f5f5f7',
    edgeLabelBackground: '#1c1c1e',
  },
  flowchart: {
    curve: 'basis',
    padding: 50,
    nodeSpacing: 120,
    rankSpacing: 140,
    htmlLabels: true,
  },
});

mermaid.parseError = () => {};

/* ── Step pill for single-process view ──────────────── */

const FlowStep = ({ step, index, isFirst, isLast }: {
  step: { id: string; name: string; filePath?: string; stepNumber: number };
  index: number;
  isFirst: boolean;
  isLast: boolean;
}) => {
  const fileName = step.filePath?.split('/').pop() || '';

  return (
    <div className="flex items-start gap-3">
      <span className="w-5 pt-2.5 text-[10px] text-text-muted/40 text-right tabular-nums flex-shrink-0">
        {index + 1}
      </span>

      <div className="flex flex-col items-center">
        <div className={`
          px-5 py-2 rounded-full
          bg-white/[0.06] backdrop-blur border border-white/[0.1]
          transition-colors hover:bg-white/[0.09] hover:border-white/[0.15]
        `}>
          <div className="text-[13px] text-text-primary text-center whitespace-nowrap">
            {step.name}
          </div>
          {fileName && (
            <div className="text-[10px] text-text-muted/50 font-mono text-center mt-0.5">
              {fileName}
            </div>
          )}
        </div>

        {!isLast && (
          <div className="w-px h-5 bg-white/[0.1]" />
        )}
      </div>
    </div>
  );
};

/* ── Modal component ────────────────────────────────── */

export const ProcessFlowModal = ({ process, onClose, onFocusInGraph, isFullScreen = false }: ProcessFlowModalProps) => {
  const backdropRef = useRef<HTMLDivElement>(null);
  const diagramRef = useRef<HTMLDivElement>(null);
  const viewportRef = useRef<HTMLDivElement>(null);

  const fullView = isFullScreen || process?.id === 'combined-all';
  const hasRawCode = !!(process as any)?.rawMermaid;
  const renderAsMermaid = fullView || hasRawCode;

  const initialZoom = fullView ? 6.67 : 1;
  const maxZoom = fullView ? 30 : 10;
  const [zoom, setZoom] = useState(initialZoom);
  const [pan, setPan] = useState({ x: 0, y: 0 });
  const [dragging, setDragging] = useState(false);
  const [dragOrigin, setDragOrigin] = useState({ x: 0, y: 0 });

  useEffect(() => {
    setZoom(initialZoom);
    setPan({ x: 0, y: 0 });
  }, [isFullScreen, initialZoom]);

  useEffect(() => {
    if (!renderAsMermaid) return;
    const handleWheel = (e: WheelEvent) => {
      e.preventDefault();
      setZoom(prev => Math.min(Math.max(0.1, prev + e.deltaY * -0.001), maxZoom));
    };
    const el = viewportRef.current;
    if (el) {
      el.addEventListener('wheel', handleWheel, { passive: false });
      return () => el.removeEventListener('wheel', handleWheel);
    }
  }, [process, maxZoom, renderAsMermaid]);

  const handleMouseDown = useCallback((e: React.MouseEvent) => {
    if (!renderAsMermaid) return;
    setDragging(true);
    setDragOrigin({ x: e.clientX - pan.x, y: e.clientY - pan.y });
  }, [pan, renderAsMermaid]);

  const handleMouseMove = useCallback((e: React.MouseEvent) => {
    if (!dragging) return;
    setPan({ x: e.clientX - dragOrigin.x, y: e.clientY - dragOrigin.y });
  }, [dragging, dragOrigin]);

  const handleMouseUp = useCallback(() => setDragging(false), []);

  useEffect(() => {
    if (!process || !renderAsMermaid || !diagramRef.current) return;

    const paint = async () => {
      try {
        const code = hasRawCode ? (process as any).rawMermaid : generateProcessMermaid(process);
        const id = `mermaid-${Date.now()}`;
        diagramRef.current!.innerHTML = '';
        const { svg } = await mermaid.render(id, code);
        diagramRef.current!.innerHTML = svg;
      } catch (error) {
        const msg = error instanceof Error ? error.message : String(error);
        const tooBig = msg.includes('Maximum') || msg.includes('exceeded');
        diagramRef.current!.innerHTML = `
          <div class="text-center p-8">
            <div class="text-[13px] text-text-muted mb-1">${tooBig ? 'Diagram too large' : 'Render error'}</div>
            <div class="text-[11px] text-text-muted/40">${tooBig
              ? `${process.steps?.length || 0} steps — try viewing individual processes`
              : `Steps: ${process.steps?.length || 0}`
            }</div>
          </div>
        `;
      }
    };

    paint();
  }, [process, renderAsMermaid, hasRawCode]);

  useEffect(() => {
    const handler = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose(); };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [onClose]);

  const onBackdropHit = useCallback((e: React.MouseEvent) => {
    if (e.target === backdropRef.current) onClose();
  }, [onClose]);

  const copyDiagramSource = useCallback(async () => {
    if (!process) return;
    const code = (process as any).rawMermaid || generateProcessMermaid(process);
    await navigator.clipboard.writeText(code);
  }, [process]);

  const focusNodes = useCallback(() => {
    if (!process || !onFocusInGraph) return;
    onFocusInGraph(process.steps.map(s => s.id), process.id);
    onClose();
  }, [process, onFocusInGraph, onClose]);

  if (!process) return null;

  const orderedSteps = [...process.steps].sort((a, b) => a.stepNumber - b.stepNumber);

  return (
    <div
      ref={backdropRef}
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/50"
      onClick={onBackdropHit}
    >
      <div className={`
        relative bg-[#1c1c1e]/95 backdrop-blur-2xl border border-white/[0.06] rounded-xl shadow-2xl
        flex flex-col overflow-hidden
        ${fullView ? 'w-[95vw] h-[90vh] max-w-none' : 'w-full max-w-[680px] mx-4 max-h-[85vh]'}
      `}>

        <div className="flex items-center justify-between px-5 py-3 border-b border-white/[0.06] flex-shrink-0">
          <span className="text-[13px] font-medium text-text-primary truncate pr-4">
            {process.label}
          </span>
          <button onClick={onClose} className="text-text-muted hover:text-text-primary transition-colors flex-shrink-0">
            <X className="w-4 h-4" />
          </button>
        </div>

        {renderAsMermaid ? (
          <div
            ref={viewportRef}
            className={`flex-1 flex items-center justify-center overflow-hidden ${fullView ? 'min-h-[70vh]' : 'min-h-[400px]'}`}
            onMouseDown={handleMouseDown}
            onMouseMove={handleMouseMove}
            onMouseUp={handleMouseUp}
            onMouseLeave={handleMouseUp}
            style={{ cursor: dragging ? 'grabbing' : 'grab' }}
          >
            <div
              ref={diagramRef}
              className="[&_.edgePath_.path]:!stroke-white/20 [&_.edgePath_.path]:stroke-1 [&_.marker]:fill-white/20 w-fit h-fit"
              style={{ transform: `translate(${pan.x}px, ${pan.y}px) scale(${zoom})` }}
            />
          </div>
        ) : (
          <div className="flex-1 overflow-y-auto scrollbar-thin">
            <div className="flex flex-col items-center py-8 px-4">
              {orderedSteps.map((step, i) => (
                <FlowStep
                  key={step.id}
                  step={step}
                  index={i}
                  isFirst={i === 0}
                  isLast={i === orderedSteps.length - 1}
                />
              ))}
            </div>
          </div>
        )}

        <div className="flex items-center justify-between px-5 py-3 border-t border-white/[0.06] flex-shrink-0">
          <div className="flex items-center gap-2">
            {renderAsMermaid && (
              <div className="flex items-center gap-1 mr-2">
                <button onClick={() => setZoom(prev => Math.max(prev - 0.25, 0.1))}
                  className="p-1 text-text-muted/40 hover:text-text-muted transition-colors">
                  <ZoomOut className="w-3.5 h-3.5" />
                </button>
                <span className="text-[10px] text-text-muted/40 font-mono min-w-[3rem] text-center">
                  {Math.round(zoom * 100)}%
                </span>
                <button onClick={() => setZoom(prev => Math.min(prev + 0.25, maxZoom))}
                  className="p-1 text-text-muted/40 hover:text-text-muted transition-colors">
                  <ZoomIn className="w-3.5 h-3.5" />
                </button>
              </div>
            )}
            <button
              onClick={copyDiagramSource}
              className="flex items-center gap-1 px-2 py-1 text-[11px] text-text-muted hover:text-text-primary transition-colors"
            >
              <Copy className="w-3 h-3" />
              Copy Mermaid
            </button>
          </div>

          <div className="flex items-center gap-2">
            {onFocusInGraph && (
              <button
                onClick={focusNodes}
                className="flex items-center gap-1 px-3 py-1 text-[11px] text-accent hover:text-accent-dim transition-colors"
              >
                <Focus className="w-3 h-3" />
                Focus in Graph
              </button>
            )}
          </div>
        </div>
      </div>
    </div>
  );
};
