import { useState, useCallback, useRef, useEffect } from 'react';
import type { KnowledgeGraph } from '../core/graph/types';

export interface ToolEvent {
  timestamp: number;
  tool: string;
  action?: string;
  filepath?: string;
}

interface AgentWatcherState {
  activeNodeIds: Set<string>;
  recentEvents: ToolEvent[];
  isConnected: boolean;
  workspacePath: string | null;
  logPath: string | null;
}

const HIGHLIGHT_DURATION = 3000; // 3 seconds

// Normalize path for matching: strip leading ./ and normalize slashes
const normalizePath = (p: string) => p.replace(/\\/g, '/').replace(/^\.?\//, '');

export function useAgentWatcher(graph: KnowledgeGraph | null) {
  const [state, setState] = useState<AgentWatcherState>({
    activeNodeIds: new Set(),
    recentEvents: [],
    isConnected: false,
    workspacePath: null,
    logPath: null,
  });

  const timersRef = useRef<Map<string, ReturnType<typeof setTimeout>>>(new Map());

  // Keep graph in a ref so IPC listeners always see the latest value
  const graphRef = useRef<KnowledgeGraph | null>(graph);
  useEffect(() => {
    graphRef.current = graph;
  }, [graph]);

  // Find graph node IDs by matching filepath + their 1-hop graph neighbors
  // Uses graphRef (not graph directly) to avoid stale closures in IPC listeners
  const findNodeIdsByPath = useCallback((filepath: string): string[] => {
    const g = graphRef.current;
    if (!g) return [];
    const normalized = normalizePath(filepath);
    const directIds = new Set<string>();

    // Match all nodes defined in this file (File, Function, Class, Method, etc.)
    for (const node of g.nodes) {
      const nodePath = normalizePath(node.properties.filePath);
      if (nodePath === normalized || nodePath.endsWith('/' + normalized) || normalized.endsWith('/' + nodePath)) {
        directIds.add(node.id);
      }
    }

    // Expand to 1-hop neighbors via CALLS, IMPORTS, INHERITS, EXTENDS relationships
    // This creates the "brain lighting up" effect showing connected code
    const neighborIds = new Set<string>();
    const EXPAND_TYPES = new Set(['CALLS', 'IMPORTS', 'INHERITS', 'EXTENDS', 'IMPLEMENTS']);
    for (const rel of g.relationships) {
      if (!EXPAND_TYPES.has(rel.type)) continue;
      if (directIds.has(rel.sourceId)) {
        neighborIds.add(rel.targetId);
      }
      if (directIds.has(rel.targetId)) {
        neighborIds.add(rel.sourceId);
      }
    }

    // Combine direct + neighbors (direct IDs first for priority)
    const all = [...directIds];
    for (const id of neighborIds) {
      if (!directIds.has(id)) all.push(id);
    }
    return all;
  }, []); // No deps — reads from graphRef

  // Highlight nodes temporarily
  const highlightNodes = useCallback((nodeIds: string[]) => {
    if (nodeIds.length === 0) return;

    setState(prev => ({
      ...prev,
      activeNodeIds: new Set([...prev.activeNodeIds, ...nodeIds]),
    }));

    // Clear after duration
    for (const id of nodeIds) {
      const existing = timersRef.current.get(id);
      if (existing) clearTimeout(existing);

      const timer = setTimeout(() => {
        setState(prev => {
          const next = new Set(prev.activeNodeIds);
          next.delete(id);
          return { ...prev, activeNodeIds: next };
        });
        timersRef.current.delete(id);
      }, HIGHLIGHT_DURATION);

      timersRef.current.set(id, timer);
    }
  }, []);

  // Start watching
  const start = useCallback(async (workspacePath: string, logPath?: string) => {
    if (!window.prowl) return;

    // Start filesystem watcher
    await window.prowl.startWatcher(workspacePath);

    // Listen for file activity events
    // findNodeIdsByPath reads from graphRef, so it always has latest graph
    window.prowl.onFileActivity((data) => {
      const nodeIds = findNodeIdsByPath(data.filepath);
      highlightNodes(nodeIds);

      const event: ToolEvent = {
        timestamp: Date.now(),
        tool: data.type === 'write' ? 'write' : data.type === 'add' ? 'create' : 'delete',
        filepath: data.filepath,
      };
      setState(prev => ({
        ...prev,
        recentEvents: [event, ...prev.recentEvents].slice(0, 50),
      }));
    });

    // Listen for parsed tool events
    window.prowl.onToolEvent((data) => {
      if (data.filepath) {
        const nodeIds = findNodeIdsByPath(data.filepath);
        highlightNodes(nodeIds);
      }

      setState(prev => ({
        ...prev,
        recentEvents: [data, ...prev.recentEvents].slice(0, 50),
      }));
    });

    // Start log parser if path provided
    if (logPath) {
      await window.prowl.startParser(logPath);
    }

    setState(prev => ({
      ...prev,
      isConnected: true,
      workspacePath,
      logPath: logPath ?? null,
    }));
  }, [findNodeIdsByPath, highlightNodes]);

  // Stop watching
  const stop = useCallback(async () => {
    if (!window.prowl) return;

    window.prowl.removeAllListeners();
    await window.prowl.stopWatcher();
    await window.prowl.stopParser();

    // Clear all timers
    for (const timer of timersRef.current.values()) {
      clearTimeout(timer);
    }
    timersRef.current.clear();

    setState({
      activeNodeIds: new Set(),
      recentEvents: [],
      isConnected: false,
      workspacePath: null,
      logPath: null,
    });
  }, []);

  // Cleanup on unmount
  useEffect(() => {
    return () => {
      for (const timer of timersRef.current.values()) {
        clearTimeout(timer);
      }
    };
  }, []);

  return {
    ...state,
    start,
    stop,
  };
}
