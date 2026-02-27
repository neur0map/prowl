/**
 * Drill-in panel — file list for a selected cluster.
 *
 * Shows a searchable, sortable list of files in the cluster.
 * Clicking a file opens a Quick Look floating card.
 */

import { memo, useState, useMemo, useCallback } from 'react';
import { ArrowLeft, Search, FileText, Code2, ArrowUpDown } from 'lucide-react';
import type { ClusterSummary } from '../lib/elk-adapter';
import type { CodeGraph, GraphNode } from '../core/graph/types';
import { getModuleColor } from '../lib/constants';
import QuickLookCard from './QuickLookCard';

interface DrillInPanelProps {
  cluster: ClusterSummary;
  graph: CodeGraph;
  onFileSelect: (nodeId: string) => void;
  onBack: () => void;
}

type SortKey = 'name' | 'lines' | 'exports';

function DrillInPanel({ cluster, graph, onFileSelect, onBack }: DrillInPanelProps) {
  const [search, setSearch] = useState('');
  const [sortBy, setSortBy] = useState<SortKey>('name');
  const [quickLookFile, setQuickLookFile] = useState<GraphNode | null>(null);

  const accent = getModuleColor(cluster.communityIndex);

  /* Compute file/source rows — uses File nodes when available,
   * falls back to grouping symbols by filePath for symbol-only clusters */
  const fileRows = useMemo(() => {
    if (cluster.files.length > 0) {
      return cluster.files.map(file => {
        const exports = cluster.symbols.filter(
          s => s.properties.filePath === file.properties.filePath && s.properties.isExported
        );
        const lineCount = file.properties.endLine || 0;
        return {
          node: file,
          name: file.properties.name,
          path: file.properties.filePath,
          lineCount,
          exportCount: exports.length,
          exports: exports.map(e => e.properties.name),
          isVirtual: false,
        };
      });
    }

    /* No File nodes — group symbols by their filePath */
    const byPath = new Map<string, GraphNode[]>();
    for (const s of cluster.symbols) {
      const fp = s.properties.filePath || 'unknown';
      const arr = byPath.get(fp) || [];
      arr.push(s);
      byPath.set(fp, arr);
    }

    return Array.from(byPath.entries()).map(([fp, symbols]) => {
      const fileName = fp.split('/').pop() || fp;
      const exports = symbols.filter(s => s.properties.isExported);
      /* Use the first symbol as a representative node for selection */
      return {
        node: symbols[0],
        name: fileName,
        path: fp,
        lineCount: 0,
        exportCount: exports.length,
        exports: exports.map(e => e.properties.name),
        isVirtual: true,
      };
    });
  }, [cluster]);

  /* Filter and sort */
  const displayRows = useMemo(() => {
    let rows = fileRows;
    if (search) {
      const lower = search.toLowerCase();
      rows = rows.filter(r => r.name.toLowerCase().includes(lower) || r.path.toLowerCase().includes(lower));
    }
    rows = [...rows].sort((a, b) => {
      switch (sortBy) {
        case 'lines': return b.lineCount - a.lineCount;
        case 'exports': return b.exportCount - a.exportCount;
        default: return a.name.localeCompare(b.name);
      }
    });
    return rows;
  }, [fileRows, search, sortBy]);

  /* Cycle sort key */
  const cycleSortKey = useCallback(() => {
    setSortBy(prev => prev === 'name' ? 'lines' : prev === 'lines' ? 'exports' : 'name');
  }, []);

  /* Find files that call/import this file */
  const getFileDependents = useCallback((filePath: string): string[] => {
    const fileNode = graph.nodes.find(n => n.properties.filePath === filePath && n.label === 'File');
    if (!fileNode) return [];
    const dependents = new Set<string>();
    for (const rel of graph.relationships) {
      if (rel.type === 'IMPORTS' && rel.targetId === fileNode.id) {
        const src = graph.nodes.find(n => n.id === rel.sourceId);
        if (src) dependents.add(src.properties.name);
      }
    }
    return Array.from(dependents).slice(0, 5);
  }, [graph]);

  return (
    <div
      className="absolute right-3 top-12 bottom-3 z-20 flex flex-col rounded-xl overflow-hidden animate-slide-in-right"
      style={{
        width: 360,
        background: 'rgba(28, 28, 30, 0.85)',
        backdropFilter: 'blur(24px)',
        WebkitBackdropFilter: 'blur(24px)',
        border: '1px solid rgba(255, 255, 255, 0.08)',
        boxShadow: '0 8px 32px rgba(0, 0, 0, 0.4)',
      }}
    >
      {/* Header */}
      <div className="flex items-center gap-3 px-4 py-3 border-b border-border-subtle">
        <button
          onClick={onBack}
          className="p-1 rounded-md hover:bg-surface transition-colors text-text-secondary"
        >
          <ArrowLeft size={16} />
        </button>
        <div className="flex items-center gap-2 flex-1 min-w-0">
          <div className="w-2.5 h-2.5 rounded-sm shrink-0" style={{ background: accent }} />
          <span className="text-sm font-semibold text-text-primary truncate">{cluster.name}</span>
        </div>
        <span className="text-xs text-text-muted font-mono shrink-0">
          {fileRows.length} {cluster.files.length > 0 ? 'file' : 'source'}{fileRows.length !== 1 ? 's' : ''}
        </span>
      </div>

      {/* Search + Sort */}
      <div className="flex items-center gap-2 px-3 py-2 border-b border-border-subtle">
        <div className="flex items-center gap-1.5 flex-1 px-2 py-1 rounded-md bg-surface/50">
          <Search size={12} className="text-text-muted shrink-0" />
          <input
            type="text"
            value={search}
            onChange={e => setSearch(e.target.value)}
            placeholder="Filter files..."
            className="bg-transparent text-xs text-text-primary placeholder:text-text-muted outline-none flex-1 font-mono"
          />
        </div>
        <button
          onClick={cycleSortKey}
          className="flex items-center gap-1 px-2 py-1 rounded-md text-[10px] text-text-muted hover:bg-surface/50 transition-colors font-mono"
          title={`Sort by ${sortBy}`}
        >
          <ArrowUpDown size={10} />
          {sortBy}
        </button>
      </div>

      {/* File list */}
      <div className="flex-1 overflow-y-auto scrollbar-thin">
        {displayRows.map(row => (
          <button
            key={row.node.id}
            onClick={() => setQuickLookFile(prev => prev?.id === row.node.id ? null : row.node)}
            onDoubleClick={() => onFileSelect(row.node.id)}
            className={`
              w-full flex items-center gap-3 px-4 py-2.5 text-left transition-colors
              ${quickLookFile?.id === row.node.id ? 'bg-surface/60' : 'hover:bg-surface/30'}
            `}
          >
            {row.isVirtual
              ? <Code2 size={14} className="text-accent/60 shrink-0" />
              : <FileText size={14} className="text-text-muted shrink-0" />
            }
            <div className="flex-1 min-w-0">
              <div className="text-xs text-text-primary font-mono truncate">{row.name}</div>
              <div className="text-[10px] text-text-muted font-mono truncate">{row.path}</div>
            </div>
            <div className="flex flex-col items-end gap-0.5 shrink-0">
              {row.lineCount > 0 && (
                <span className="text-[10px] text-text-muted font-mono">{row.lineCount} ln</span>
              )}
              {row.exportCount > 0 && (
                <span className="text-[10px] text-accent font-mono">{row.exportCount} exp</span>
              )}
            </div>
          </button>
        ))}

        {displayRows.length === 0 && (
          <div className="flex flex-col items-center justify-center py-8 gap-1">
            <span className="text-xs text-text-muted">
              {search ? 'No matching files' : 'No files in this cluster'}
            </span>
            {!search && cluster.symbols.length > 0 && (
              <span className="text-[10px] text-text-muted/60">
                {cluster.symbols.length} symbol{cluster.symbols.length !== 1 ? 's' : ''} detected
              </span>
            )}
          </div>
        )}
      </div>

      {/* Quick Look floating card */}
      {quickLookFile && (
        <QuickLookCard
          file={quickLookFile}
          exports={fileRows.find(r => r.node.id === quickLookFile.id)?.exports || []}
          dependents={getFileDependents(quickLookFile.properties.filePath)}
          lineCount={fileRows.find(r => r.node.id === quickLookFile.id)?.lineCount || 0}
          onOpen={() => onFileSelect(quickLookFile.id)}
          onClose={() => setQuickLookFile(null)}
        />
      )}
    </div>
  );
}

export default memo(DrillInPanel);
