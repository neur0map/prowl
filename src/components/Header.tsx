import { Search, RefreshCw, Database, FolderOpen } from 'lucide-react';
import { useAppState } from '../hooks/useAppState';
import { useState, useMemo, useRef, useEffect } from 'react';
import { GraphNode } from '../core/graph/types';

const NODE_TYPE_COLORS: Record<string, string> = {
  Folder: '#D4A868',
  File: '#6AAAD4',
  Function: '#88B878',
  Class: '#D08060',
  Method: '#60B8A0',
  Interface: '#C080A0',
  Variable: '#808080',
  Import: '#808080',
  Type: '#A088B8',
};

interface HeaderProps {
  onFocusNode?: (nodeId: string) => void;
  onRefreshGraph?: () => void;
  onOpenFolder?: () => void;
}

export const Header = ({ onFocusNode, onRefreshGraph, onOpenFolder }: HeaderProps) => {
  const {
    projectName,
    graph,
    loadedFromSnapshot,
  } = useAppState();
  const [searchQuery, setSearchQuery] = useState('');
  const [isSearchOpen, setIsSearchOpen] = useState(false);
  const [selectedIndex, setSelectedIndex] = useState(0);
  const searchRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);

  const searchResults = useMemo(() => {
    if (!graph || !searchQuery.trim()) return [];
    const query = searchQuery.toLowerCase();
    return graph.nodes
      .filter(node => node.properties.name.toLowerCase().includes(query))
      .slice(0, 10);
  }, [graph, searchQuery]);

  useEffect(() => {
    const handleClickOutside = (e: MouseEvent) => {
      if (searchRef.current && !searchRef.current.contains(e.target as Node)) {
        setIsSearchOpen(false);
      }
    };
    document.addEventListener('mousedown', handleClickOutside);
    return () => document.removeEventListener('mousedown', handleClickOutside);
  }, []);

  useEffect(() => {
    const handleKeyDown = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === 'k') {
        e.preventDefault();
        inputRef.current?.focus();
        setIsSearchOpen(true);
      }
      if (e.key === 'Escape') {
        setIsSearchOpen(false);
        inputRef.current?.blur();
      }
    };
    document.addEventListener('keydown', handleKeyDown);
    return () => document.removeEventListener('keydown', handleKeyDown);
  }, []);

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (!isSearchOpen || searchResults.length === 0) return;
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      setSelectedIndex(i => Math.min(i + 1, searchResults.length - 1));
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      setSelectedIndex(i => Math.max(i - 1, 0));
    } else if (e.key === 'Enter') {
      e.preventDefault();
      const selected = searchResults[selectedIndex];
      if (selected) handleSelectNode(selected);
    }
  };

  const handleSelectNode = (node: GraphNode) => {
    onFocusNode?.(node.id);
    setSearchQuery('');
    setIsSearchOpen(false);
    setSelectedIndex(0);
  };

  return (
    <header
      className="flex items-center h-11 px-4 bg-void border-b border-white/[0.06] relative z-50"
      style={{ WebkitAppRegion: 'drag' } as React.CSSProperties}
    >
      {/* Left — project info */}
      <div className="flex items-center gap-2 min-w-0" style={{ WebkitAppRegion: 'no-drag', paddingLeft: 70 } as React.CSSProperties}>
        {projectName ? (
          <>
            <button
              onClick={onOpenFolder}
              className="flex items-center gap-1.5 min-w-0 text-[13px] text-text-secondary hover:text-text-primary transition-colors group"
              title="Open another folder (⌘O)"
            >
              <FolderOpen className="w-3.5 h-3.5 shrink-0 text-text-muted group-hover:text-text-primary transition-colors" />
              <span className="truncate max-w-[200px] font-mono">{projectName}</span>
            </button>
            {loadedFromSnapshot && (
              <span className="text-[10px] text-text-muted/60 font-mono shrink-0">cached</span>
            )}
            {onRefreshGraph && (
              <button
                onClick={onRefreshGraph}
                className="w-6 h-6 flex items-center justify-center rounded text-text-muted hover:text-text-primary transition-colors shrink-0"
                title="Refresh layout"
              >
                <RefreshCw className="w-3.5 h-3.5" />
              </button>
            )}
          </>
        ) : (
          <span className="text-[13px] text-text-muted font-mono">Prowl</span>
        )}
      </div>

      {/* Center — logo */}
      <div className="absolute left-1/2 -translate-x-1/2 pointer-events-none">
        <span className="text-white/50 text-base font-mono">:{'}'}</span>
      </div>

      {/* Right — search trigger only */}
      <div className="ml-auto relative" ref={searchRef} style={{ WebkitAppRegion: 'no-drag' } as React.CSSProperties}>
        <button
          onClick={() => { inputRef.current?.focus(); setIsSearchOpen(true); }}
          className={`flex items-center gap-2 px-2.5 py-1.5 rounded text-[12px] transition-colors ${
            isSearchOpen
              ? 'bg-white/[0.08] text-text-primary'
              : 'text-text-muted hover:text-text-secondary'
          }`}
        >
          <Search className="w-3.5 h-3.5" />
          <kbd className="font-mono text-[11px] opacity-60">⌘K</kbd>
        </button>

        {/* Floating search palette */}
        {isSearchOpen && (
          <>
          <div
            className="fixed inset-0 z-40"
            onClick={() => { setIsSearchOpen(false); setSearchQuery(''); }}
          />
          <div
            className="absolute right-0 top-full mt-2 w-80 bg-deep border border-white/[0.12] rounded-lg shadow-lg overflow-hidden z-50"
            style={{ animation: 'scaleIn 0.12s ease-out' }}
          >
            <div className="px-3 py-2 border-b border-white/[0.06]">
              <input
                ref={inputRef}
                type="text"
                placeholder="Search symbols..."
                value={searchQuery}
                onChange={(e) => {
                  setSearchQuery(e.target.value);
                  setSelectedIndex(0);
                }}
                onKeyDown={handleKeyDown}
                className="w-full bg-transparent border-none outline-none text-[13px] text-text-primary placeholder:text-text-muted"
              />
            </div>
            {searchQuery.trim() && (
              searchResults.length === 0 ? (
                <div className="px-3 py-2.5 text-[12px] text-text-muted">
                  No results for "{searchQuery}"
                </div>
              ) : (
                <div className="max-h-64 overflow-y-auto scrollbar-thin">
                  {searchResults.map((node, index) => (
                    <button
                      key={node.id}
                      onClick={() => handleSelectNode(node)}
                      className={`w-full px-3 py-2 flex items-center gap-2.5 text-left transition-colors ${
                        index === selectedIndex
                          ? 'bg-accent/10 text-text-primary'
                          : 'hover:bg-white/[0.04] text-text-secondary'
                      }`}
                    >
                      <span
                        className="w-2 h-2 rounded-full flex-shrink-0"
                        style={{ backgroundColor: NODE_TYPE_COLORS[node.label] || '#7A7F8A' }}
                      />
                      <span className="flex-1 truncate text-[13px] font-mono">
                        {node.properties.name}
                      </span>
                      <span className="text-[11px] text-text-muted">
                        {node.label}
                      </span>
                    </button>
                  ))}
                </div>
              )
            )}
          </div>
          </>
        )}
      </div>
    </header>
  );
};
