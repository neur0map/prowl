import { useState, useMemo, useCallback, useEffect, useRef, useLayoutEffect } from 'react';
import {
  Folder,
  FolderOpen,
  Search,
  ChevronLeft,
  Settings,
  X,
} from 'lucide-react';
import { useAppState } from '../hooks/useAppState';
import { NODE_COLORS } from '../lib/constants';
import { GraphNode, NodeLabel } from '../core/graph/types';
import { LanguageIcon } from './LanguageIcon';

/* ── Tree data structure ────────────────────────────── */

interface TreeEntry {
  id: string;
  name: string;
  kind: 'folder' | 'file';
  fullPath: string;
  subtree: TreeEntry[];
  source?: GraphNode;
}

function buildFileTree(nodes: GraphNode[]): TreeEntry[] {
  const roots: TreeEntry[] = [];
  const lookup = new Map<string, TreeEntry>();

  const fsNodes = nodes
    .filter(n => n.label === 'Folder' || n.label === 'File')
    .sort((a, b) => a.properties.filePath.localeCompare(b.properties.filePath));

  for (const gn of fsNodes) {
    const segments = gn.properties.filePath.split(/[/\\]/).filter(Boolean);
    let pathSoFar = '';
    let level = roots;

    for (let idx = 0; idx < segments.length; idx++) {
      const seg = segments[idx];
      pathSoFar = pathSoFar ? `${pathSoFar}/${seg}` : seg;

      let entry = lookup.get(pathSoFar);
      if (!entry) {
        const isTail = idx === segments.length - 1;
        const isFileTail = isTail && gn.label === 'File';

        entry = {
          id: isTail ? gn.id : pathSoFar,
          name: seg,
          kind: isFileTail ? 'file' : 'folder',
          fullPath: pathSoFar,
          subtree: [],
          source: isTail ? gn : undefined,
        };

        lookup.set(pathSoFar, entry);
        level.push(entry);
      }

      level = entry.subtree;
    }
  }

  return roots;
}

function collectDescendantIds(node: TreeEntry): string[] {
  const ids: string[] = [];
  if (node.kind === 'file' && node.source) {
    ids.push(node.source.id);
  }
  for (const child of node.subtree) {
    ids.push(...collectDescendantIds(child));
  }
  return ids;
}

/* ── Single tree row (recursive) ────────────────────── */

function TreeRow({
  entry,
  indent,
  filter,
  onSelect,
  openPaths,
  onToggle,
  activePath,
}: {
  entry: TreeEntry;
  indent: number;
  filter: string;
  onSelect: (n: TreeEntry) => void;
  openPaths: Set<string>;
  onToggle: (p: string) => void;
  activePath: string | null;
}) {
  const isOpen = openPaths.has(entry.fullPath);
  const selected = activePath === entry.fullPath;
  const hasKids = entry.subtree.length > 0;

  const childrenRef = useRef<HTMLDivElement>(null);
  const [rendered, setRendered] = useState(isOpen);
  const [height, setHeight] = useState<number | 'auto'>(isOpen ? 'auto' : 0);

  useLayoutEffect(() => {
    if (isOpen) setRendered(true);
  }, [isOpen]);

  useLayoutEffect(() => {
    if (!childrenRef.current) return;
    if (isOpen && rendered) {
      setHeight(0);
      requestAnimationFrame(() => {
        if (!childrenRef.current) return;
        const h = childrenRef.current.scrollHeight;
        requestAnimationFrame(() => setHeight(h));
      });
    } else if (!isOpen && rendered) {
      const h = childrenRef.current.scrollHeight;
      setHeight(h);
      requestAnimationFrame(() => {
        requestAnimationFrame(() => setHeight(0));
      });
    }
  }, [isOpen, rendered]);

  const handleTransitionEnd = () => {
    if (!isOpen) setRendered(false);
    if (isOpen) setHeight('auto');
  };

  const visibleChildren = useMemo(() => {
    if (!filter) return entry.subtree;
    const needle = filter.toLowerCase();
    return entry.subtree.filter(c =>
      c.name.toLowerCase().includes(needle) ||
      c.subtree.some(gc => gc.name.toLowerCase().includes(needle))
    );
  }, [entry.subtree, filter]);

  const nameHit = filter.length > 0 && entry.name.toLowerCase().includes(filter.toLowerCase());
  const leftPad = indent * 14 + 10;

  return (
    <div className="relative">
      {/* Indent guide lines */}
      {indent > 0 && (
        <span
          className="absolute top-0 bottom-0 border-l border-white/[0.06]"
          style={{ left: `${(indent - 1) * 14 + 16}px` }}
        />
      )}

      <button
        onClick={() => { if (hasKids) onToggle(entry.fullPath); onSelect(entry); }}
        className={[
          'w-full flex items-center gap-2 py-[5px] text-left text-[13px] font-mono transition-colors',
          selected
            ? 'bg-accent/10 text-accent'
            : 'text-text-secondary hover:bg-white/[0.04] hover:text-text-primary',
          nameHit ? 'bg-accent/5' : '',
        ].join(' ')}
        style={{ paddingLeft: `${leftPad}px`, paddingRight: 8 }}
      >
        {entry.kind === 'folder' ? (
          <span className="text-[11px] text-text-muted/60 w-4 text-center shrink-0 select-none">
            {isOpen ? '−' : '+'}
          </span>
        ) : (
          <LanguageIcon filename={entry.name} size={14} />
        )}

        <span className="truncate">{entry.name}</span>

        {entry.kind === 'folder' && hasKids && (
          <span className="text-[10px] text-text-muted/40 ml-auto shrink-0 tabular-nums">
            {entry.subtree.length}
          </span>
        )}
      </button>

      {hasKids && rendered && (
        <div
          ref={childrenRef}
          onTransitionEnd={handleTransitionEnd}
          style={{
            height: height === 'auto' ? 'auto' : `${height}px`,
            opacity: isOpen ? 1 : 0,
            overflow: 'hidden',
            transition: height === 'auto' ? 'none' : 'height 150ms ease-out, opacity 150ms ease-out',
          }}
        >
          {visibleChildren.map(child => (
            <TreeRow
              key={child.id}
              entry={child}
              indent={indent + 1}
              filter={filter}
              onSelect={onSelect}
              openPaths={openPaths}
              onToggle={onToggle}
              activePath={activePath}
            />
          ))}
        </div>
      )}
    </div>
  );
}

/* ── Panel component ────────────────────────────────── */

interface FileTreePanelProps {
  onFocusNode: (nodeId: string) => void;
}

export const FileTreePanel = ({ onFocusNode }: FileTreePanelProps) => {
  const {
    graph,
    selectedNode,
    setSelectedNode,
    openCodePanel,
    setHighlightedNodeIds,
    setSettingsPanelOpen,
  } = useAppState();

  const [collapsed, setCollapsed] = useState(true);
  const [searchQuery, setSearchQuery] = useState('');
  const [isSearchVisible, setIsSearchVisible] = useState(false);
  const [openPaths, setOpenPaths] = useState<Set<string>>(new Set());
  const searchInputRef = useRef<HTMLInputElement>(null);

  const tree = useMemo(() => {
    if (!graph) return [];
    return buildFileTree(graph.nodes);
  }, [graph]);

  useEffect(() => {
    if (tree.length > 0 && openPaths.size === 0) {
      setOpenPaths(new Set(tree.map(n => n.fullPath)));
    }
  }, [tree.length]);

  useEffect(() => {
    const fp = selectedNode?.properties?.filePath;
    if (!fp) return;

    const parts = fp.split(/[/\\]/).filter(Boolean);
    const ancestors: string[] = [];
    let accum = '';
    for (let i = 0; i < parts.length - 1; i++) {
      accum = accum ? `${accum}/${parts[i]}` : parts[i];
      ancestors.push(accum);
    }

    if (ancestors.length > 0) {
      setOpenPaths(prev => {
        const updated = new Set(prev);
        for (const a of ancestors) updated.add(a);
        return updated;
      });
    }
  }, [selectedNode?.id]);

  useEffect(() => {
    if (isSearchVisible) {
      searchInputRef.current?.focus();
    }
  }, [isSearchVisible]);

  const togglePath = useCallback((path: string) => {
    setOpenPaths(prev => {
      const next = new Set(prev);
      if (next.has(path)) next.delete(path);
      else next.add(path);
      return next;
    });
  }, []);

  const handleSelect = useCallback((node: TreeEntry) => {
    if (node.kind === 'folder') {
      const childIds = collectDescendantIds(node);
      if (childIds.length > 0) setHighlightedNodeIds(new Set(childIds));
    } else if (node.source) {
      const alreadySelected = selectedNode?.id === node.source.id;
      setSelectedNode(node.source);
      openCodePanel();
      if (!alreadySelected) onFocusNode(node.source.id);
    }
  }, [setSelectedNode, openCodePanel, onFocusNode, selectedNode, setHighlightedNodeIds]);

  const activePath = selectedNode?.properties.filePath || null;

  return (
    <div
      className={`h-full shrink-0 border-r border-white/[0.06] overflow-hidden transition-[width] duration-200 ease-out relative bg-void`}
      style={{ width: collapsed ? 44 : 260 }}
    >
      {/* ── Collapsed rail ── */}
      <div className={`absolute inset-0 flex flex-col items-center pt-2 pb-3 transition-opacity duration-150 ${
        collapsed ? 'opacity-100' : 'opacity-0 pointer-events-none'
      }`}>
        <button
          onClick={() => setCollapsed(false)}
          className="p-1.5 rounded text-text-muted hover:text-text-primary transition-colors"
          title="Files"
        >
          <Folder className="w-[18px] h-[18px]" />
        </button>

        <div className="mt-auto flex flex-col items-center gap-1">
          <button
            onClick={() => setSettingsPanelOpen(true)}
            className="p-1.5 rounded text-text-muted hover:text-text-primary transition-colors"
            title="Settings"
          >
            <Settings className="w-4 h-4" />
          </button>
        </div>
      </div>

      {/* ── Expanded panel ── */}
      <div
        className={`h-full flex flex-col transition-opacity duration-150 ${
          collapsed ? 'opacity-0 pointer-events-none' : 'opacity-100'
        }`}
        style={{ width: 260 }}
      >
        {/* Header — collapse + title + search toggle */}
        <div className="flex items-center gap-1 px-2 py-1.5 border-b border-white/[0.06] shrink-0">
          <button
            onClick={() => setCollapsed(true)}
            className="p-1 text-text-muted hover:text-text-primary rounded transition-colors"
            title="Collapse"
          >
            <ChevronLeft className="w-4 h-4" />
          </button>
          <span className="text-[12px] text-text-muted font-mono flex-1">files</span>
          <button
            onClick={() => {
              setIsSearchVisible(v => !v);
              if (isSearchVisible) setSearchQuery('');
            }}
            className={`p-1 rounded transition-colors ${
              isSearchVisible ? 'text-accent' : 'text-text-muted hover:text-text-primary'
            }`}
            title="Filter files"
          >
            <Search className="w-3.5 h-3.5" />
          </button>
        </div>

        {/* Inline search — only when toggled */}
        {isSearchVisible && (
          <div className="flex items-center gap-1.5 px-2 py-1.5 border-b border-white/[0.06] shrink-0">
            <input
              ref={searchInputRef}
              type="text"
              placeholder="Filter..."
              value={searchQuery}
              onChange={e => setSearchQuery(e.target.value)}
              onKeyDown={e => {
                if (e.key === 'Escape') {
                  setIsSearchVisible(false);
                  setSearchQuery('');
                }
              }}
              className="flex-1 bg-transparent border-none outline-none text-[12px] text-text-primary placeholder:text-text-muted font-mono"
            />
            {searchQuery && (
              <button
                onClick={() => setSearchQuery('')}
                className="p-0.5 text-text-muted hover:text-text-primary"
              >
                <X className="w-3 h-3" />
              </button>
            )}
          </div>
        )}

        <div className="flex-1 overflow-y-auto scrollbar-thin">
          {tree.length === 0 ? (
            <div className="px-3 py-4 text-center text-text-muted text-xs">No files loaded</div>
          ) : (
            tree.map(node => (
              <TreeRow
                key={node.id}
                entry={node}
                indent={0}
                filter={searchQuery}
                onSelect={handleSelect}
                openPaths={openPaths}
                onToggle={togglePath}
                activePath={activePath}
              />
            ))
          )}
        </div>

        {/* Footer */}
        <div className="px-3 py-1.5 border-t border-white/[0.06] shrink-0 flex items-center justify-between">
          {graph && (
            <span className="text-[11px] text-text-muted font-mono">
              {graph.nodes.length} · {graph.relationships.length}
            </span>
          )}
          <button
            onClick={() => setSettingsPanelOpen(true)}
            className="p-1 text-text-muted hover:text-text-primary rounded transition-colors"
            title="Settings"
          >
            <Settings className="w-3.5 h-3.5" />
          </button>
        </div>
      </div>
    </div>
  );
};
