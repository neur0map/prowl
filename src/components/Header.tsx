import { Search, RefreshCw, FolderOpen, GitCompareArrows, Loader2, Check, GitBranch, Files } from 'lucide-react';
import { useAppState } from '../hooks/useAppState';
import { useState, useMemo, useRef, useEffect, useCallback } from 'react';
import { GraphNode } from '../core/graph/types';
import { ComparisonPill } from './ComparisonPill';
import { parseGitHubUrl } from '../services/git-clone';

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
    getWorkerApi,
  } = useAppState();
  const [searchQuery, setSearchQuery] = useState('');
  const [isSearchOpen, setIsSearchOpen] = useState(false);
  const [selectedIndex, setSelectedIndex] = useState(0);
  const searchRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);

  // Compare popover state
  const [isCompareOpen, setIsCompareOpen] = useState(false);
  const [compareUrl, setCompareUrl] = useState('');
  const [compareLoading, setCompareLoading] = useState(false);
  const [comparePhase, setComparePhase] = useState<'idle' | 'fetching-info' | 'fetching-tree' | 'loading' | 'done'>('idle');
  const [compareRepoName, setCompareRepoName] = useState<string | null>(null);
  const [compareFileCount, setCompareFileCount] = useState<number | null>(null);
  const [compareError, setCompareError] = useState<string | null>(null);
  const compareRef = useRef<HTMLDivElement>(null);
  const compareInputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (isCompareOpen) compareInputRef.current?.focus();
  }, [isCompareOpen]);

  useEffect(() => {
    const handleClickOutside = (e: MouseEvent) => {
      if (compareRef.current && !compareRef.current.contains(e.target as Node)) {
        if (!compareLoading) setIsCompareOpen(false);
      }
    };
    document.addEventListener('mousedown', handleClickOutside);
    return () => document.removeEventListener('mousedown', handleClickOutside);
  }, [compareLoading]);

  const handleCompareSubmit = useCallback(async () => {
    const url = compareUrl.trim();
    if (!url) return;

    const api = getWorkerApi();
    if (!api) {
      setCompareError('Worker not ready');
      return;
    }

    const parsed = parseGitHubUrl(url);
    if (!parsed) {
      setCompareError('Invalid GitHub URL');
      return;
    }

    setCompareLoading(true);
    setCompareError(null);
    setComparePhase('fetching-info');
    setCompareRepoName(null);
    setCompareFileCount(null);

    try {
      // Only one comparison at a time — reject if one is already active
      const existing = await api.getComparisonStats();
      if (existing) {
        setCompareError(`"${existing.repoName}" is already loaded. Close it first.`);
        setCompareLoading(false);
        setComparePhase('idle');
        return;
      }

      const { owner, repo } = parsed;
      const repoInfo = await window.prowl.github.getRepoInfo(owner, repo);
      setCompareRepoName(repoInfo.fullName);
      setComparePhase('fetching-tree');

      const { entries } = await window.prowl.github.getRepoTree(owner, repo, repoInfo.defaultBranch);
      setCompareFileCount(entries.filter((e: { type: string }) => e.type === 'blob' || e.type === 'file').length);
      setComparePhase('loading');

      api.loadComparison(
        {
          owner,
          repo,
          branch: repoInfo.defaultBranch,
          repoName: repoInfo.fullName,
          repoUrl: url,
          description: repoInfo.description,
        },
        entries,
      );

      setComparePhase('done');
      // Brief pause to show success state before closing
      await new Promise(r => setTimeout(r, 800));
      setCompareUrl('');
      setIsCompareOpen(false);
      setComparePhase('idle');
    } catch (err) {
      setCompareError(err instanceof Error ? err.message : 'Failed to load');
      setComparePhase('idle');
    } finally {
      setCompareLoading(false);
    }
  }, [compareUrl, getWorkerApi]);

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
            <ComparisonPill />

            {/* Compare repo button */}
            <div className="relative" ref={compareRef}>
              <button
                onClick={() => { setIsCompareOpen(prev => !prev); setCompareError(null); }}
                className={`w-6 h-6 flex items-center justify-center rounded transition-colors shrink-0 ${
                  isCompareOpen
                    ? 'text-violet-300 bg-violet-500/10'
                    : 'text-text-muted hover:text-text-primary'
                }`}
                title="Compare with another repo"
              >
                <GitCompareArrows className="w-3.5 h-3.5" />
              </button>

              {isCompareOpen && (
                <div
                  className="absolute left-0 top-full mt-2 w-80 bg-deep border border-white/[0.12] rounded-lg shadow-lg overflow-hidden z-50"
                  style={{ animation: 'scaleIn 0.12s ease-out' }}
                >
                  {/* Input section */}
                  <div className="px-3 py-2 border-b border-white/[0.06]">
                    <div className="text-[11px] text-text-muted mb-1.5">Compare with GitHub repo</div>
                    <div className="flex gap-1.5">
                      <input
                        ref={compareInputRef}
                        type="text"
                        placeholder="https://github.com/owner/repo"
                        value={compareUrl}
                        onChange={(e) => setCompareUrl(e.target.value)}
                        onKeyDown={(e) => {
                          if (e.key === 'Enter' && !compareLoading) handleCompareSubmit();
                          if (e.key === 'Escape') setIsCompareOpen(false);
                        }}
                        disabled={compareLoading}
                        className="flex-1 min-w-0 bg-white/[0.04] border border-white/[0.08] rounded px-2 py-1 text-[12px] text-text-primary placeholder:text-text-muted outline-none focus:border-violet-400/40 disabled:opacity-50"
                      />
                      <button
                        onClick={handleCompareSubmit}
                        disabled={compareLoading || !compareUrl.trim()}
                        className="px-2.5 py-1 rounded bg-violet-500/20 text-violet-300 text-[11px] font-medium hover:bg-violet-500/30 disabled:opacity-40 transition-colors flex items-center gap-1"
                      >
                        {compareLoading && comparePhase !== 'done' ? (
                          <Loader2 className="w-3 h-3 animate-spin" />
                        ) : comparePhase === 'done' ? (
                          <Check className="w-3 h-3 text-green-400" />
                        ) : (
                          'Load'
                        )}
                      </button>
                    </div>
                    {compareError && (
                      <div className="mt-1.5 text-[10px] text-red-400/80 truncate" title={compareError}>
                        {compareError}
                      </div>
                    )}
                  </div>

                  {/* Loading progress panel */}
                  {compareLoading && (
                    <div className="px-3 py-2.5 space-y-2">
                      {/* Progress bar */}
                      <div className="h-[3px] w-full rounded-full bg-white/[0.04] overflow-hidden">
                        <div
                          className="h-full rounded-full transition-all duration-500 ease-out"
                          style={{
                            width: comparePhase === 'fetching-info' ? '25%'
                              : comparePhase === 'fetching-tree' ? '60%'
                              : comparePhase === 'loading' ? '85%'
                              : comparePhase === 'done' ? '100%' : '0%',
                            background: comparePhase === 'done'
                              ? 'linear-gradient(90deg, #22c55e, #4ade80)'
                              : 'linear-gradient(90deg, #8b5cf6, #a78bfa)',
                          }}
                        />
                      </div>

                      {/* Step indicators */}
                      <div className="space-y-1.5">
                        {/* Step 1: Fetching repo info */}
                        <div className="flex items-center gap-2">
                          {comparePhase === 'fetching-info' ? (
                            <Loader2 className="w-3 h-3 text-violet-400 animate-spin shrink-0" />
                          ) : (
                            <Check className="w-3 h-3 text-green-400/80 shrink-0" />
                          )}
                          <span className={`text-[11px] font-mono ${
                            comparePhase === 'fetching-info' ? 'text-violet-300' : 'text-text-muted'
                          }`}>
                            {comparePhase === 'fetching-info' ? 'Fetching repo info...' : 'Repo info loaded'}
                          </span>
                        </div>

                        {/* Step 2: Fetching tree */}
                        {(comparePhase === 'fetching-tree' || comparePhase === 'loading' || comparePhase === 'done') && (
                          <div className="flex items-center gap-2">
                            {comparePhase === 'fetching-tree' ? (
                              <Loader2 className="w-3 h-3 text-violet-400 animate-spin shrink-0" />
                            ) : (
                              <Check className="w-3 h-3 text-green-400/80 shrink-0" />
                            )}
                            <span className={`text-[11px] font-mono ${
                              comparePhase === 'fetching-tree' ? 'text-violet-300' : 'text-text-muted'
                            }`}>
                              {comparePhase === 'fetching-tree' ? 'Fetching file tree...' : `${compareFileCount ?? 0} files indexed`}
                            </span>
                          </div>
                        )}

                        {/* Step 3: Loading into memory */}
                        {(comparePhase === 'loading' || comparePhase === 'done') && (
                          <div className="flex items-center gap-2">
                            {comparePhase === 'loading' ? (
                              <Loader2 className="w-3 h-3 text-violet-400 animate-spin shrink-0" />
                            ) : (
                              <Check className="w-3 h-3 text-green-400/80 shrink-0" />
                            )}
                            <span className={`text-[11px] font-mono ${
                              comparePhase === 'loading' ? 'text-violet-300' : 'text-text-muted'
                            }`}>
                              {comparePhase === 'loading' ? 'Loading comparison...' : 'Ready'}
                            </span>
                          </div>
                        )}
                      </div>

                      {/* Repo name badge */}
                      {compareRepoName && (
                        <div className="flex items-center gap-1.5 mt-1 px-2 py-1 rounded-md bg-violet-500/8 border border-violet-400/10">
                          <GitBranch className="w-3 h-3 text-violet-400/60 shrink-0" />
                          <span className="text-[11px] font-mono text-violet-300/70 truncate">{compareRepoName}</span>
                          {compareFileCount !== null && (
                            <span className="ml-auto flex items-center gap-1 text-[10px] text-violet-300/50 shrink-0">
                              <Files className="w-2.5 h-2.5" />
                              {compareFileCount}
                            </span>
                          )}
                        </div>
                      )}
                    </div>
                  )}
                </div>
              )}
            </div>

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
