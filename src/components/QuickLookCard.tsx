/**
 * Quick Look floating card — glassmorphic file preview.
 *
 * Shows file name, stats, export list, and dependents.
 * "Open" button triggers the full CodeEditorPanel.
 */

import { memo } from 'react';
import { ExternalLink, X, FileCode } from 'lucide-react';
import type { GraphNode } from '../core/graph/types';

interface QuickLookCardProps {
  file: GraphNode;
  exports: string[];
  dependents: string[];
  lineCount: number;
  onOpen: () => void;
  onClose: () => void;
}

function QuickLookCard({ file, exports, dependents, lineCount, onOpen, onClose }: QuickLookCardProps) {
  return (
    <div
      className="absolute left-[-320px] top-4 z-30 rounded-xl overflow-hidden animate-fade-in"
      style={{
        width: 300,
        background: 'rgba(28, 28, 30, 0.9)',
        backdropFilter: 'blur(24px)',
        WebkitBackdropFilter: 'blur(24px)',
        border: '1px solid rgba(255, 255, 255, 0.1)',
        boxShadow: '0 12px 40px rgba(0, 0, 0, 0.5)',
      }}
    >
      {/* Header */}
      <div className="flex items-center justify-between px-3 py-2.5 border-b border-border-subtle">
        <div className="flex items-center gap-2 min-w-0">
          <FileCode size={14} className="text-accent shrink-0" />
          <span className="text-xs font-semibold text-text-primary font-mono truncate">
            {file.properties.name}
          </span>
        </div>
        <div className="flex items-center gap-1 shrink-0">
          <button
            onClick={onOpen}
            className="p-1 rounded-md hover:bg-surface transition-colors text-accent"
            title="Open in editor"
          >
            <ExternalLink size={13} />
          </button>
          <button
            onClick={onClose}
            className="p-1 rounded-md hover:bg-surface transition-colors text-text-muted"
          >
            <X size={13} />
          </button>
        </div>
      </div>

      <div className="p-3 flex flex-col gap-2.5">
        {/* Stats */}
        <div className="flex items-center gap-3 text-[10px] text-text-muted font-mono">
          {lineCount > 0 && <span>{lineCount} lines</span>}
          <span>{exports.length} export{exports.length !== 1 ? 's' : ''}</span>
          <span className="text-text-muted/60">{file.properties.filePath}</span>
        </div>

        {/* Exports */}
        {exports.length > 0 && (
          <div className="flex flex-col gap-1">
            <span className="text-[10px] text-text-muted uppercase tracking-wider">Exports</span>
            <div className="flex flex-wrap gap-1">
              {exports.slice(0, 8).map(name => (
                <span
                  key={name}
                  className="px-1.5 py-0.5 rounded text-[10px] font-mono text-accent bg-accent/10"
                >
                  {name}
                </span>
              ))}
            </div>
          </div>
        )}

        {/* Dependents */}
        {dependents.length > 0 && (
          <div className="flex flex-col gap-1">
            <span className="text-[10px] text-text-muted uppercase tracking-wider">Used by</span>
            <div className="flex flex-wrap gap-1">
              {dependents.map(name => (
                <span
                  key={name}
                  className="px-1.5 py-0.5 rounded text-[10px] font-mono text-text-secondary bg-surface/60"
                >
                  {name}
                </span>
              ))}
            </div>
          </div>
        )}
      </div>
    </div>
  );
}

export default memo(QuickLookCard);
