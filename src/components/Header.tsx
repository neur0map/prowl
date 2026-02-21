import { Search, Settings, PanelRight, RefreshCw, Rocket, Database } from 'lucide-react';
import { useAppState } from '../hooks/useAppState';
import { useState, useMemo, useRef, useEffect } from 'react';
import { GraphNode } from '../core/graph/types';
import { EmbeddingStatus } from './EmbeddingStatus';

const NODE_TYPE_COLORS: Record<string, string> = {
  Folder: '#8085EC',
  File: '#5A9CF5',
  Function: '#3EBD8C',
  Class: '#E0A243',
  Method: '#3AADA0',
  Interface: '#D96FA0',
  Variable: '#7A7F8A',
  Import: '#636870',
  Type: '#9B8FD0',
};

interface HeaderProps {
  onFocusNode?: (nodeId: string) => void;
  onRefreshGraph?: () => void;
  onOpenRoadmap?: () => void;
}

export const Header = ({ onFocusNode, onRefreshGraph, onOpenRoadmap }: HeaderProps) => {
  const {
    projectName,
    graph,
    isRightPanelOpen,
    setRightPanelOpen,
    setSettingsPanelOpen,
    loadedFromSnapshot,
  } = useAppState();
  const [searchQuery, setSearchQuery] = useState('');
  const [isSearchOpen, setIsSearchOpen] = useState(false);
  const [selectedIndex, setSelectedIndex] = useState(0);
  const searchRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);

  const nodeCount = graph?.nodes.length ?? 0;
  const edgeCount = graph?.relationships.length ?? 0;

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
      className="flex items-center justify-between px-4 py-2 glass border-b border-white/[0.08]"
      style={{ WebkitAppRegion: 'drag' } as React.CSSProperties}
    >
      {/* Left — logo + project */}
      <div className="flex items-center gap-3" style={{ WebkitAppRegion: 'no-drag', paddingLeft: 70 } as React.CSSProperties}>
        <div className="flex items-center gap-2">
          <span className="text-white/80 text-sm">◇</span>
          <span className="text-[14px] font-normal text-text-primary tracking-tight">Prowl</span>
        </div>

        {projectName && (
          <>
            <div className="flex items-center gap-1.5 px-2.5 py-1 glass-subtle rounded-md text-[12px] text-text-secondary">
              <span className="w-1.5 h-1.5 bg-node-function rounded-full opacity-70" />
              <span className="truncate max-w-[180px]">{projectName}</span>
            </div>
            {loadedFromSnapshot && (
              <div className="flex items-center gap-1 px-2 py-0.5 rounded-md text-[10px] text-text-muted bg-white/[0.04] border border-white/[0.06]">
                <Database className="w-3 h-3" />
                Cached
              </div>
            )}
            {onRefreshGraph && (
              <button
                onClick={onRefreshGraph}
                className="w-6 h-6 flex items-center justify-center rounded-md text-text-muted hover:text-text-primary hover:bg-white/[0.08] transition-colors"
                title="Refresh graph layout"
              >
                <RefreshCw className="w-3.5 h-3.5" />
              </button>
            )}
          </>
        )}
      </div>

      {/* Center — search */}
      <div className="flex-1 max-w-sm mx-4 relative" ref={searchRef} style={{ WebkitAppRegion: 'no-drag' } as React.CSSProperties}>
        <div className="flex items-center gap-2 px-3 py-1.5 glass-subtle rounded-md transition-all focus-within:border-accent/50">
          <Search className="w-3.5 h-3.5 text-text-muted flex-shrink-0" />
          <input
            ref={inputRef}
            type="text"
            placeholder="Search nodes..."
            value={searchQuery}
            onChange={(e) => {
              setSearchQuery(e.target.value);
              setIsSearchOpen(true);
              setSelectedIndex(0);
            }}
            onFocus={() => setIsSearchOpen(true)}
            onKeyDown={handleKeyDown}
            className="flex-1 bg-transparent border-none outline-none text-[13px] text-text-primary placeholder:text-text-muted"
          />
          <kbd className="px-1.5 py-0.5 rounded text-[10px] text-text-muted font-mono bg-white/[0.06] border border-white/[0.1]">
            ⌘K
          </kbd>
        </div>

        {/* Search dropdown */}
        {isSearchOpen && searchQuery.trim() && (
          <div className="absolute top-full left-0 right-0 mt-1 glass-elevated rounded-lg shadow-lg overflow-hidden z-50">
            {searchResults.length === 0 ? (
              <div className="px-3 py-2.5 text-[12px] text-text-muted">
                No results for "{searchQuery}"
              </div>
            ) : (
              <div className="max-h-72 overflow-y-auto">
                {searchResults.map((node, index) => (
                  <button
                    key={node.id}
                    onClick={() => handleSelectNode(node)}
                    className={`w-full px-3 py-2 flex items-center gap-2.5 text-left transition-colors ${
                      index === selectedIndex
                        ? 'bg-accent/15 text-text-primary'
                        : 'hover:bg-white/[0.06] text-text-secondary'
                    }`}
                  >
                    <span
                      className="w-2 h-2 rounded-full flex-shrink-0"
                      style={{ backgroundColor: NODE_TYPE_COLORS[node.label] || '#7A7F8A' }}
                    />
                    <span className="flex-1 truncate text-[12px]">
                      {node.properties.name}
                    </span>
                    <span className="text-[10px] text-text-muted px-1.5 py-0.5 rounded bg-white/[0.06]">
                      {node.label}
                    </span>
                  </button>
                ))}
              </div>
            )}
          </div>
        )}
      </div>

      {/* Right — actions */}
      <div className="flex items-center gap-1.5" style={{ WebkitAppRegion: 'no-drag' } as React.CSSProperties}>
        {graph && (
          <div className="flex items-center gap-3 mr-2 text-[11px] text-text-muted">
            <span>{nodeCount} nodes</span>
            <span>{edgeCount} edges</span>
          </div>
        )}

        <EmbeddingStatus />

        <button
          onClick={onOpenRoadmap}
          className="w-7 h-7 flex items-center justify-center rounded-md text-text-muted hover:text-text-primary hover:bg-white/[0.08] transition-colors"
          title="Roadmap"
        >
          <Rocket className="w-4 h-4" />
        </button>

        <button
          onClick={() => setSettingsPanelOpen(true)}
          className="w-7 h-7 flex items-center justify-center rounded-md text-text-muted hover:text-text-primary hover:bg-white/[0.08] transition-colors"
          title="Settings"
        >
          <Settings className="w-4 h-4" />
        </button>

        <button
          onClick={() => setRightPanelOpen(!isRightPanelOpen)}
          className={`
            w-7 h-7 flex items-center justify-center rounded-md transition-colors
            ${isRightPanelOpen
              ? 'bg-white/[0.1] text-text-primary'
              : 'text-text-muted hover:text-text-primary hover:bg-white/[0.08]'
            }
          `}
          title="Toggle panel"
        >
          <PanelRight className="w-4 h-4" />
        </button>
      </div>
    </header>
  );
};
