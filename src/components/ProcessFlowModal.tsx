/**
 * Process Flow Modal — Prowl
 *
 * Single processes: native glass pill chain (no Mermaid).
 * Combined map: restyled Mermaid with Prowl dark monochrome theme.
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

// Mermaid theme — Prowl monochrome (only used for combined map)
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

// ── Glass pill step ──
const StepPill = ({ step, index, isFirst, isLast }: {
  step: { id: string; name: string; filePath?: string; stepNumber: number };
  index: number;
  isFirst: boolean;
  isLast: boolean;
}) => {
  const fileName = step.filePath?.split('/').pop() || '';

  return (
    <div className="flex items-start gap-3">
      {/* Step number */}
      <span className="w-5 pt-2.5 text-[10px] text-text-muted/40 text-right tabular-nums flex-shrink-0">
        {index + 1}
      </span>

      <div className="flex flex-col items-center">
        {/* Pill */}
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

        {/* Connector line */}
        {!isLast && (
          <div className="w-px h-5 bg-white/[0.1]" />
        )}
      </div>
    </div>
  );
};

export const ProcessFlowModal = ({ process, onClose, onFocusInGraph, isFullScreen = false }: ProcessFlowModalProps) => {
  const containerRef = useRef<HTMLDivElement>(null);
  const diagramRef = useRef<HTMLDivElement>(null);
  const scrollContainerRef = useRef<HTMLDivElement>(null);

  const isCombined = isFullScreen || process?.id === 'combined-all';
  const isRawMermaid = !!(process as any)?.rawMermaid;
  const useMermaid = isCombined || isRawMermaid;

  // Zoom state (only for Mermaid/combined view)
  const defaultZoom = isCombined ? 6.67 : 1;
  const maxZoom = isCombined ? 30 : 10;
  const [zoom, setZoom] = useState(defaultZoom);
  const [pan, setPan] = useState({ x: 0, y: 0 });
  const [isPanning, setIsPanning] = useState(false);
  const [panStart, setPanStart] = useState({ x: 0, y: 0 });

  useEffect(() => {
    setZoom(defaultZoom);
    setPan({ x: 0, y: 0 });
  }, [isFullScreen, defaultZoom]);

  // Scroll zoom (Mermaid only)
  useEffect(() => {
    if (!useMermaid) return;
    const handleWheel = (e: WheelEvent) => {
      e.preventDefault();
      setZoom(prev => Math.min(Math.max(0.1, prev + e.deltaY * -0.001), maxZoom));
    };
    const el = scrollContainerRef.current;
    if (el) {
      el.addEventListener('wheel', handleWheel, { passive: false });
      return () => el.removeEventListener('wheel', handleWheel);
    }
  }, [process, maxZoom, useMermaid]);

  // Pan handlers (Mermaid only)
  const handleMouseDown = useCallback((e: React.MouseEvent) => {
    if (!useMermaid) return;
    setIsPanning(true);
    setPanStart({ x: e.clientX - pan.x, y: e.clientY - pan.y });
  }, [pan, useMermaid]);

  const handleMouseMove = useCallback((e: React.MouseEvent) => {
    if (!isPanning) return;
    setPan({ x: e.clientX - panStart.x, y: e.clientY - panStart.y });
  }, [isPanning, panStart]);

  const handleMouseUp = useCallback(() => setIsPanning(false), []);

  // Render Mermaid (combined map only)
  useEffect(() => {
    if (!process || !useMermaid || !diagramRef.current) return;

    const render = async () => {
      try {
        const code = isRawMermaid ? (process as any).rawMermaid : generateProcessMermaid(process);
        const id = `mermaid-${Date.now()}`;
        diagramRef.current!.innerHTML = '';
        const { svg } = await mermaid.render(id, code);
        diagramRef.current!.innerHTML = svg;
      } catch (error) {
        const msg = error instanceof Error ? error.message : String(error);
        const isSize = msg.includes('Maximum') || msg.includes('exceeded');
        diagramRef.current!.innerHTML = `
          <div class="text-center p-8">
            <div class="text-[13px] text-text-muted mb-1">${isSize ? 'Diagram too large' : 'Render error'}</div>
            <div class="text-[11px] text-text-muted/40">${isSize
              ? `${process.steps?.length || 0} steps — try viewing individual processes`
              : `Steps: ${process.steps?.length || 0}`
            }</div>
          </div>
        `;
      }
    };

    render();
  }, [process, useMermaid, isRawMermaid]);

  // Escape to close
  useEffect(() => {
    const handler = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose(); };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [onClose]);

  // Backdrop click
  const handleBackdropClick = useCallback((e: React.MouseEvent) => {
    if (e.target === containerRef.current) onClose();
  }, [onClose]);

  // Copy mermaid
  const handleCopyMermaid = useCallback(async () => {
    if (!process) return;
    const code = (process as any).rawMermaid || generateProcessMermaid(process);
    await navigator.clipboard.writeText(code);
  }, [process]);

  // Focus in graph
  const handleFocusInGraph = useCallback(() => {
    if (!process || !onFocusInGraph) return;
    onFocusInGraph(process.steps.map(s => s.id), process.id);
    onClose();
  }, [process, onFocusInGraph, onClose]);

  if (!process) return null;

  const sortedSteps = [...process.steps].sort((a, b) => a.stepNumber - b.stepNumber);

  return (
    <div
      ref={containerRef}
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/50"
      onClick={handleBackdropClick}
    >
      <div className={`
        relative bg-[#1c1c1e]/95 backdrop-blur-2xl border border-white/[0.06] rounded-xl shadow-2xl
        flex flex-col overflow-hidden
        ${isCombined ? 'w-[95vw] h-[90vh] max-w-none' : 'w-full max-w-[680px] mx-4 max-h-[85vh]'}
      `}>

        {/* Title bar */}
        <div className="flex items-center justify-between px-5 py-3 border-b border-white/[0.06] flex-shrink-0">
          <span className="text-[13px] font-medium text-text-primary truncate pr-4">
            {process.label}
          </span>
          <button onClick={onClose} className="text-text-muted hover:text-text-primary transition-colors flex-shrink-0">
            <X className="w-4 h-4" />
          </button>
        </div>

        {/* Content */}
        {useMermaid ? (
          /* ── Mermaid view (combined map) ── */
          <div
            ref={scrollContainerRef}
            className={`flex-1 flex items-center justify-center overflow-hidden ${isCombined ? 'min-h-[70vh]' : 'min-h-[400px]'}`}
            onMouseDown={handleMouseDown}
            onMouseMove={handleMouseMove}
            onMouseUp={handleMouseUp}
            onMouseLeave={handleMouseUp}
            style={{ cursor: isPanning ? 'grabbing' : 'grab' }}
          >
            <div
              ref={diagramRef}
              className="[&_.edgePath_.path]:!stroke-white/20 [&_.edgePath_.path]:stroke-1 [&_.marker]:fill-white/20 w-fit h-fit"
              style={{ transform: `translate(${pan.x}px, ${pan.y}px) scale(${zoom})` }}
            />
          </div>
        ) : (
          /* ── Glass pill chain (single process) ── */
          <div className="flex-1 overflow-y-auto scrollbar-thin">
            <div className="flex flex-col items-center py-8 px-4">
              {sortedSteps.map((step, i) => (
                <StepPill
                  key={step.id}
                  step={step}
                  index={i}
                  isFirst={i === 0}
                  isLast={i === sortedSteps.length - 1}
                />
              ))}
            </div>
          </div>
        )}

        {/* Footer */}
        <div className="flex items-center justify-between px-5 py-3 border-t border-white/[0.06] flex-shrink-0">
          <div className="flex items-center gap-2">
            {/* Zoom controls (Mermaid only) */}
            {useMermaid && (
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
              onClick={handleCopyMermaid}
              className="flex items-center gap-1 px-2 py-1 text-[11px] text-text-muted hover:text-text-primary transition-colors"
            >
              <Copy className="w-3 h-3" />
              Copy Mermaid
            </button>
          </div>

          <div className="flex items-center gap-2">
            {onFocusInGraph && (
              <button
                onClick={handleFocusInGraph}
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
