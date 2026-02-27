import { createContext, useContext, useState, useCallback, useRef, useEffect, ReactNode } from 'react';
import * as Comlink from 'comlink';
import { CodeGraph, GraphNode, NodeLabel } from '../core/graph/types';
import { IndexingProgress, IndexingResult, deserializeIndexingResult } from '../types/pipeline';
import { createCodeGraph } from '../core/graph/graph';
import { DEFAULT_VISIBLE_LABELS } from '../lib/constants';
import type { IndexerWorkerApi } from '../workers/ingestion.worker';
import type { FileEntry } from '../types/file-entry';
import type { EmbeddingProgress, SemanticSearchResult } from '../core/embeddings/types';
import type { LLMSettings, ProviderConfig, AgentStreamChunk, ChatMessage, ToolCallInfo, MessageStep, StoredConversation, StoredMessage } from '../core/llm/types';
import { loadSettings, getActiveProviderConfig, saveSettings, initSecureStorage } from '../core/llm/settings-service';
import { initOAuth } from '../core/llm/oauth-service';
import type { AgentMessage } from '../core/llm/agent';
import { estimateHistoryTokens, COMPACTION_THRESHOLD } from '../core/llm/agent';
import { DEFAULT_VISIBLE_EDGES, type EdgeType } from '../lib/constants';
import { useAgentWatcher, type ToolEvent } from './useAgentWatcher';
import type { McpToolName } from '../mcp/types';

export interface AgentWatcherState {
  activeNodeIds: Set<string>;
  recentEvents: ToolEvent[];
  isConnected: boolean;
  workspacePath: string | null;
  logPath: string | null;
}

export type ViewMode = 'startup' | 'onboarding' | 'loading' | 'exploring';
export type RightPanelTab = 'code' | 'chat';
export type EmbeddingStatus = 'idle' | 'loading' | 'embedding' | 'indexing' | 'ready' | 'error';

export interface QueryResult {
  rows: Record<string, any>[];
  nodeIds: string[];
  executionTime: number;
}

export type AnimationType = 'pulse' | 'ripple' | 'glow' | 'watcher';

export interface NodeAnimation {
  type: AnimationType;
  startTime: number;
  duration: number;
}

// A tracked code location pinned by the user or surfaced by a tool call
export interface CodeReference {
  id: string;
  filePath: string;
  startLine?: number;
  endLine?: number;
  nodeId?: string;  // Associated graph node ID
  label?: string;   // File, Function, Class, etc.
  name?: string;    // Display name
  source: 'ai' | 'user';  // How it was added
}

export interface CodeReferenceFocus {
  filePath: string;
  startLine?: number;
  endLine?: number;
  ts: number;
}

interface AppState {
  // View state
  viewMode: ViewMode;
  setViewMode: (mode: ViewMode) => void;

  // Graph data
  graph: CodeGraph | null;
  setGraph: (graph: CodeGraph | null) => void;
  fileContents: Map<string, string>;
  setFileContents: (contents: Map<string, string>) => void;

  // Selection
  selectedNode: GraphNode | null;
  setSelectedNode: (node: GraphNode | null) => void;

  isRightPanelOpen: boolean;
  setRightPanelOpen: (open: boolean) => void;
  rightPanelTab: RightPanelTab;
  setRightPanelTab: (tab: RightPanelTab) => void;
  openCodePanel: () => void;
  openChatPanel: () => void;

  // Filters
  visibleLabels: NodeLabel[];
  toggleLabelVisibility: (label: NodeLabel) => void;
  visibleEdgeTypes: EdgeType[];
  toggleEdgeVisibility: (edgeType: EdgeType) => void;

  // Depth filter (N hops from selection)
  depthFilter: number | null;
  setDepthFilter: (depth: number | null) => void;

  // Query state
  highlightedNodeIds: Set<string>;
  setHighlightedNodeIds: (ids: Set<string>) => void;
  // AI highlights (toggable)
  aiCitationHighlightedNodeIds: Set<string>;
  aiToolHighlightedNodeIds: Set<string>;
  blastRadiusNodeIds: Set<string>;
  isAIHighlightsEnabled: boolean;
  toggleAIHighlights: () => void;
  resetToolHighlights: () => void;
  clearBlastRadius: () => void;
  queryResult: QueryResult | null;
  setQueryResult: (result: QueryResult | null) => void;
  clearQueryHighlights: () => void;

  // Node animations (for MCP tool visual feedback)
  animatedNodes: Map<string, NodeAnimation>;
  triggerNodeAnimation: (nodeIds: string[], type: AnimationType) => void;
  clearAnimations: () => void;

  // Progress
  progress: IndexingProgress | null;
  setProgress: (progress: IndexingProgress | null) => void;

  // Project info
  projectName: string;
  setProjectName: (name: string) => void;

  // Worker API (shared across app)
  runPipelineFromFiles: (files: FileEntry[], onProgress: (p: IndexingProgress) => void, clusteringConfig?: ProviderConfig) => Promise<IndexingResult>;
  runQuery: (cypher: string) => Promise<any[]>;
  isDatabaseReady: () => Promise<boolean>;

  // Embedding state
  embeddingStatus: EmbeddingStatus;
  embeddingProgress: EmbeddingProgress | null;

  // Embedding methods
  startEmbeddings: (forceDevice?: 'webgpu' | 'wasm') => Promise<void>;
  semanticSearch: (query: string, k?: number) => Promise<SemanticSearchResult[]>;
  semanticSearchWithContext: (query: string, k?: number, hops?: number) => Promise<any[]>;
  isEmbeddingReady: boolean;

  // Debug/test methods
  testArrayParams: () => Promise<{ success: boolean; error?: string }>;

  // LLM/Agent state
  llmSettings: LLMSettings;
  updateLLMSettings: (updates: Partial<LLMSettings>) => void;
  isSettingsPanelOpen: boolean;
  setSettingsPanelOpen: (open: boolean) => void;
  isAgentReady: boolean;
  isAgentInitializing: boolean;
  agentError: string | null;

  // Chat state
  chatMessages: ChatMessage[];
  isChatLoading: boolean;
  currentToolCalls: ToolCallInfo[];

  // LLM methods
  refreshLLMSettings: () => void;
  initializeAgent: (overrideProjectName?: string) => Promise<boolean>;
  sendChatMessage: (message: string) => Promise<void>;
  stopChatResponse: () => void;
  clearChat: () => void;

  // Conversation history
  conversationId: string | null;
  conversations: StoredConversation[];
  loadConversation: (id: string) => void;
  startNewConversation: () => void;
  isCompacting: boolean;

  // Code References Panel
  codeReferences: CodeReference[];
  isCodePanelOpen: boolean;
  setCodePanelOpen: (open: boolean) => void;
  addCodeReference: (ref: Omit<CodeReference, 'id'>) => void;
  removeCodeReference: (id: string) => void;
  resetCodeRefs: () => void;
  clearCodeReferences: () => void;
  codeReferenceFocus: CodeReferenceFocus | null;

  // Live update status
  isLiveUpdating: boolean;

  // Agent Watcher (Electron only)
  agentWatcherState: AgentWatcherState;
  startAgentWatcher: (workspacePath: string, logPath?: string) => Promise<void>;
  stopAgentWatcher: () => Promise<void>;
  resetForNewProject: () => Promise<void>;

  // Worker API access (for MCP bridge)
  getWorkerApi: () => Comlink.Remote<IndexerWorkerApi> | null;

  // Snapshot persistence
  projectPath: string | null;
  setProjectPath: (path: string | null) => void;
  loadedFromSnapshot: boolean;
  setLoadedFromSnapshot: (loaded: boolean) => void;
  saveSnapshot: (path: string) => Promise<{ success: boolean; size: number }>;
  loadSnapshot: (path: string, onProgress: (p: IndexingProgress) => void) => Promise<(IndexingResult & { hasEmbeddings?: boolean }) | null>;
  incrementalUpdate: (diff: any, folderPath: string, onProgress: (p: IndexingProgress) => void) => Promise<IndexingResult | null>;
}

const AppStateContext = createContext<AppState | null>(null);

export const AppStateProvider = ({ children }: { children: ReactNode }) => {
  // View state
  const [viewMode, setViewMode] = useState<ViewMode>('startup');

  // Graph data
  const [graph, setGraph] = useState<CodeGraph | null>(null);
  const [fileContents, setFileContents] = useState<Map<string, string>>(new Map());

  // Selection
  const [selectedNode, setSelectedNode] = useState<GraphNode | null>(null);

  // Right Panel
  const [isRightPanelOpen, setRightPanelOpen] = useState(false);
  const [rightPanelTab, setRightPanelTab] = useState<RightPanelTab>('code');

  const openCodePanel = useCallback(() => {
    // Legacy API: used by graph/tree selection.
    // Code is now shown in the Code References Panel (left of the graph),
    // so "openCodePanel" just ensures that panel becomes visible when needed.
    setCodePanelOpen(true);
  }, []);

  const openChatPanel = useCallback(() => {
    setRightPanelOpen(true);
    setRightPanelTab('chat');
  }, []);

  // Filters
  const [visibleLabels, setVisibleLabels] = useState<NodeLabel[]>(DEFAULT_VISIBLE_LABELS);
  const [visibleEdgeTypes, setVisibleEdgeTypes] = useState<EdgeType[]>(DEFAULT_VISIBLE_EDGES);

  // Depth filter
  const [depthFilter, setDepthFilter] = useState<number | null>(null);

  // Query state
  const [highlightedNodeIds, setHighlightedNodeIds] = useState<Set<string>>(new Set());
  const [queryResult, setQueryResult] = useState<QueryResult | null>(null);

  // AI highlights (separate from user/query highlights)
  const [aiCitationHighlightedNodeIds, setAICitationHighlightedNodeIds] = useState<Set<string>>(new Set());
  const [aiToolHighlightedNodeIds, setAIToolHighlightedNodeIds] = useState<Set<string>>(new Set());
  const [blastRadiusNodeIds, setBlastRadiusNodeIds] = useState<Set<string>>(new Set());
  const [isAIHighlightsEnabled, setAIHighlightsEnabled] = useState(true);

  const toggleAIHighlights = useCallback(() => {
    setAIHighlightsEnabled(prev => !prev);
  }, []);

  const resetToolHighlights = useCallback(() => {
    setAIToolHighlightedNodeIds(new Set());
  }, []);

  const clearBlastRadius = useCallback(() => {
    setBlastRadiusNodeIds(new Set());
  }, []);

  const clearQueryHighlights = useCallback(() => {
    setHighlightedNodeIds(new Set());
    setQueryResult(null);
  }, []);

  // Node animations (for MCP tool visual feedback)
  const [animatedNodes, setAnimatedNodes] = useState<Map<string, NodeAnimation>>(new Map());
  const animationTimerRef = useRef<ReturnType<typeof setInterval> | null>(null);

  const triggerNodeAnimation = useCallback((nodeIds: string[], type: AnimationType) => {
    const now = Date.now();
    const duration = type === 'pulse' ? 2000 : type === 'ripple' ? 3000 : type === 'watcher' ? 3000 : 4000;

    setAnimatedNodes(prev => {
      const next = new Map(prev);
      for (const id of nodeIds) {
        next.set(id, { type, startTime: now, duration });
      }
      return next;
    });

    // Auto-cleanup after duration
    setTimeout(() => {
      setAnimatedNodes(prev => {
        const next = new Map(prev);
        for (const id of nodeIds) {
          const anim = next.get(id);
          if (anim && anim.startTime === now) {
            next.delete(id);
          }
        }
        return next;
      });
    }, duration + 100);
  }, []);

  const clearAnimations = useCallback(() => {
    setAnimatedNodes(new Map());
    if (animationTimerRef.current) {
      clearInterval(animationTimerRef.current);
      animationTimerRef.current = null;
    }
  }, []);

  // Progress
  const [progress, setProgress] = useState<IndexingProgress | null>(null);

  // Project info
  const [projectName, setProjectName] = useState<string>('');
  const [projectPath, setProjectPath] = useState<string | null>(null);
  const [loadedFromSnapshot, setLoadedFromSnapshot] = useState(false);

  // Embedding state
  const [embeddingStatus, setEmbeddingStatus] = useState<EmbeddingStatus>('idle');
  const [embeddingProgress, setEmbeddingProgress] = useState<EmbeddingProgress | null>(null);
  const [isLiveUpdating, setIsLiveUpdating] = useState(false);

  // LLM/Agent state
  const [llmSettings, setLLMSettings] = useState<LLMSettings>(loadSettings);
  const [isSettingsPanelOpen, setSettingsPanelOpen] = useState(false);
  const [isAgentReady, setIsAgentReady] = useState(false);
  const [isAgentInitializing, setIsAgentInitializing] = useState(false);
  const [agentError, setAgentError] = useState<string | null>(null);

  useEffect(() => {
    initSecureStorage().then(() => {
      setLLMSettings(loadSettings());
      initOAuth();
    });
  }, []);

  // Chat state
  const [chatMessages, setChatMessages] = useState<ChatMessage[]>([]);
  const [isChatLoading, setIsChatLoading] = useState(false);
  const [currentToolCalls, setCurrentToolCalls] = useState<ToolCallInfo[]>([]);

  // Conversation history
  const [conversationId, setConversationId] = useState<string | null>(null);
  const [conversations, setConversations] = useState<StoredConversation[]>([]);
  const [compactedSummary, setCompactedSummary] = useState<string | null>(null);
  const [isCompacting, setIsCompacting] = useState(false);

  // Code References Panel state
  const [codeReferences, setCodeReferences] = useState<CodeReference[]>([]);
  const [isCodePanelOpen, setCodePanelOpen] = useState(false);
  const [codeReferenceFocus, setCodeReferenceFocus] = useState<CodeReferenceFocus | null>(null);

  // Agent Watcher (Electron only)
  const agentWatcher = useAgentWatcher(graph);

  const prevWatcherIdsRef = useRef<Set<string>>(new Set());
  const watcherContributedIdsRef = useRef<Set<string>>(new Set());
  useEffect(() => {
    const currentIds = agentWatcher.activeNodeIds;
    const prevIds = prevWatcherIdsRef.current;

    if (currentIds.size > 0) {
      // Find newly added node IDs (not already animating)
      const newIds: string[] = [];
      for (const id of currentIds) {
        if (!prevIds.has(id)) {
          newIds.push(id);
        }
      }
      if (newIds.length > 0) {
        triggerNodeAnimation(newIds, 'watcher');
      }
      // Track which IDs the watcher contributed to static highlights
      for (const id of currentIds) {
        watcherContributedIdsRef.current.add(id);
      }
      // Merge into static highlights so they stay visible between pulses
      setAIToolHighlightedNodeIds(prev => {
        const merged = new Set(prev);
        for (const id of currentIds) merged.add(id);
        return merged;
      });
    }

    // Clean up expired watcher IDs from static highlights
    const expiredIds: string[] = [];
    for (const id of prevIds) {
      if (!currentIds.has(id) && watcherContributedIdsRef.current.has(id)) {
        expiredIds.push(id);
        watcherContributedIdsRef.current.delete(id);
      }
    }
    if (expiredIds.length > 0) {
      setAIToolHighlightedNodeIds(prev => {
        const next = new Set(prev);
        for (const id of expiredIds) next.delete(id);
        return next;
      });
    }

    prevWatcherIdsRef.current = new Set(currentIds);
  }, [agentWatcher.activeNodeIds, triggerNodeAnimation]);

  const agentWatcherState: AgentWatcherState = {
    activeNodeIds: agentWatcher.activeNodeIds,
    recentEvents: agentWatcher.recentEvents,
    isConnected: agentWatcher.isConnected,
    workspacePath: agentWatcher.workspacePath,
    logPath: agentWatcher.logPath,
  };

  const startAgentWatcher = useCallback(async (workspacePath: string, logPath?: string) => {
    await agentWatcher.start(workspacePath, logPath);
  }, [agentWatcher.start]);

  const stopAgentWatcher = useCallback(async () => {
    await agentWatcher.stop();
  }, [agentWatcher.stop]);

  /** Tear down state from the current project before loading a new one. */
  const resetForNewProject = useCallback(async () => {
    /* Stop the filesystem watcher and its IPC listeners */
    await agentWatcher.stop();

    /* Dispose the embedding model so the worker doesn't hold stale GPU/WASM refs */
    try {
      const api = apiRef.current;
      if (api) await api.disposeEmbeddingModel();
    } catch { /* worker may not be initialised yet */ }

    /* Renderer-side state resets */
    setEmbeddingStatus('idle');
    setEmbeddingProgress(null);
    setChatMessages([]);
    setCurrentToolCalls([]);
    setIsChatLoading(false);
    setAgentError(null);
    setIsAgentReady(false);
    setCodeReferences([]);
    setCodePanelOpen(false);
    setCodeReferenceFocus(null);
    setSelectedNode(null);
    setHighlightedNodeIds(new Set());
    setAICitationHighlightedNodeIds(new Set());
    setAIToolHighlightedNodeIds(new Set());
    setBlastRadiusNodeIds(new Set());
    setQueryResult(null);
  }, [agentWatcher.stop]);

  const normalizePath = useCallback((p: string) => {
    return p.replace(/\\/g, '/').replace(/^\.?\//, '');
  }, []);

  const resolveFilePath = useCallback((requestedPath: string): string | null => {
    const req = normalizePath(requestedPath).toLowerCase();
    if (!req) return null;

    // Exact match first
    for (const key of fileContents.keys()) {
      if (normalizePath(key).toLowerCase() === req) return key;
    }

    // Ends-with match (best for partial paths like "src/foo.ts")
    let best: { path: string; score: number } | null = null;
    for (const key of fileContents.keys()) {
      const norm = normalizePath(key).toLowerCase();
      if (norm.endsWith(req)) {
        const score = 1000 - norm.length; // shorter is better
        if (!best || score > best.score) best = { path: key, score };
      }
    }
    if (best) return best.path;

    // Segment match fallback
    const segs = req.split('/').filter(Boolean);
    for (const key of fileContents.keys()) {
      const normSegs = normalizePath(key).toLowerCase().split('/').filter(Boolean);
      let idx = 0;
      for (const s of segs) {
        const found = normSegs.findIndex((x, i) => i >= idx && x.includes(s));
        if (found === -1) { idx = -1; break; }
        idx = found + 1;
      }
      if (idx !== -1) return key;
    }

    return null;
  }, [fileContents, normalizePath]);

  const findFileNodeId = useCallback((filePath: string): string | undefined => {
    if (!graph) return undefined;
    const target = normalizePath(filePath);
    const fileNode = graph.nodes.find(
      (n) => n.label === 'File' && normalizePath(n.properties.filePath) === target
    );
    return fileNode?.id;
  }, [graph, normalizePath]);

  // Code References methods
  const addCodeReference = useCallback((ref: Omit<CodeReference, 'id'>) => {
    const id = `ref-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
    const newRef: CodeReference = { ...ref, id };

    let wasAdded = false;
    setCodeReferences(prev => {
      // Don't add duplicates (same file + line range)
      const isDuplicate = prev.some(r =>
        r.filePath === ref.filePath &&
        r.startLine === ref.startLine &&
        r.endLine === ref.endLine
      );
      if (isDuplicate) return prev;
      wasAdded = true;
      return [...prev, newRef];
    });

    // Only auto-open and focus for user-initiated references (graph/tree clicks).
    // AI-streamed citations are collected silently — the user clicks them to open.
    if (ref.source !== 'ai') {
      setCodePanelOpen(true);
      setCodeReferenceFocus({
        filePath: ref.filePath,
        startLine: ref.startLine,
        endLine: ref.endLine,
        ts: Date.now(),
      });
    }

    // Track AI highlights separately so they can be toggled off in the UI
    if (ref.nodeId && ref.source === 'ai') {
      setAICitationHighlightedNodeIds(prev => new Set([...prev, ref.nodeId!]));
    }
  }, []);

  const resetCodeRefs = useCallback(() => {
    setCodeReferences(prev => {
      const removed = prev.filter(r => r.source === 'ai');
      const kept = prev.filter(r => r.source !== 'ai');

      // Remove citation-based AI highlights for removed refs
      const removedNodeIds = new Set(removed.map(r => r.nodeId).filter(Boolean) as string[]);
      if (removedNodeIds.size > 0) {
        setAICitationHighlightedNodeIds(prevIds => {
          const next = new Set(prevIds);
          for (const id of removedNodeIds) next.delete(id);
          return next;
        });
      }

      // Don't auto-close if the user has something selected (top viewer)
      if (kept.length === 0 && !selectedNode) {
        setCodePanelOpen(false);
      }
      return kept;
    });
  }, [queryResult, selectedNode]);

  // Open the inspector when the user picks a node
  useEffect(() => {
    if (!selectedNode) return;
    // User selection should show in the top "Selected file" viewer,
    // not be appended to the AI citations list.
    setCodePanelOpen(true);
  }, [selectedNode]);

  const workerRef = useRef<Worker | null>(null);
  const apiRef = useRef<Comlink.Remote<IndexerWorkerApi> | null>(null);

  const ensureWorker = useCallback(() => {
    if (!workerRef.current) {
      const worker = new Worker(
        new URL('../workers/ingestion.worker.ts', import.meta.url),
        { type: 'module' }
      );
      const api = Comlink.wrap<IndexerWorkerApi>(worker);
      workerRef.current = worker;
      apiRef.current = api;
    }
    return apiRef.current!;
  }, []);

  useEffect(() => {
    return () => {
      workerRef.current?.terminate();
      workerRef.current = null;
      apiRef.current = null;
    };
  }, []);

  // MCP handler: receives tool requests from main process via contextBridge callback.
  // Same pattern as onFileActivity, onToolEvent, terminal.onData — proven IPC flow.
  const projectNameRef = useRef(projectName);
  projectNameRef.current = projectName;

  useEffect(() => {
    let toolHandlersModule: typeof import('../mcp/tool-handlers') | null = null;

    // Poll for MCP requests from the preload queue every 100ms.
    // Avoids contextBridge callback deadlocks — renderer initiates all calls.
    const pollInterval = setInterval(() => {
      const requests = window.prowl.mcp.pollRequests();
      for (let i = 0; i < requests.length; i++) {
        const req = requests[i];
        const { requestId, toolName } = req;
        const api = apiRef.current;

        if (!api) {
          window.prowl.mcp.sendResult(requestId, {
            success: false,
            error: 'Worker not ready. Open a project in Prowl first.',
          });
          continue;
        }

        // Async tool execution
        (async () => {
          try {
            if (!toolHandlersModule) {
              toolHandlersModule = await import('../mcp/tool-handlers');
            }
            const result = await toolHandlersModule.executeMcpTool(
              api,
              toolName as McpToolName,
              req.params,
              { projectName: projectNameRef.current || undefined },
            );
            window.prowl.mcp.sendResult(requestId, result);
          } catch (err) {
            window.prowl.mcp.sendResult(requestId, {
              success: false,
              error: err instanceof Error ? err.message : String(err),
            });
          }
        })();
      }
    }, 100);

    return () => {
      clearInterval(pollInterval);
    };
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  // Live update: listen for file changes from chokidar and debounce updates to worker
  const liveUpdateDirtyRef = useRef(false);

  useEffect(() => {
    if (!window.prowl?.onFileChanged) return;

    const pendingChanges = new Map<string, { type: string; content: string | null }>();
    let debounceTimer: ReturnType<typeof setTimeout> | null = null;
    let inFlight = false;

    const flush = async () => {
      const api = apiRef.current;
      if (!api || pendingChanges.size === 0 || inFlight) return;

      const batch = new Map(pendingChanges);
      pendingChanges.clear();
      inFlight = true;
      setIsLiveUpdating(true);

      try {
        const serialized = await api.liveUpdate(batch);

        if (serialized === null) {
          // Pipeline not ready yet — put changes back so they're retried
          for (const [k, v] of batch) pendingChanges.set(k, v);
          scheduleFlush();
          return;
        }

        liveUpdateDirtyRef.current = true;

        // Refresh renderer graph + fileContents so the visual UI stays current
        const result = deserializeIndexingResult(serialized, createCodeGraph);
        setGraph(result.graph);
        setFileContents(result.fileContents);

        if (import.meta.env.DEV) {
          console.log(`[prowl:live] updated ${batch.size} files`);
        }
      } catch (err) {
        console.warn('[prowl:live] update failed:', err);
      } finally {
        inFlight = false;
        // If more changes arrived while we were running, fire again immediately
        if (pendingChanges.size > 0) {
          scheduleFlush();
        } else {
          setIsLiveUpdating(false);
        }
      }
    };

    const scheduleFlush = () => {
      if (debounceTimer) clearTimeout(debounceTimer);
      debounceTimer = setTimeout(flush, 800);
    };

    window.prowl.onFileChanged((data) => {
      pendingChanges.set(data.filepath, { type: data.type, content: data.content });
      if (!inFlight) scheduleFlush();
    });

    return () => {
      if (debounceTimer) clearTimeout(debounceTimer);
      window.prowl?.removeFileChangedListener?.();
    };
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  const runPipelineFromFiles = useCallback(async (
    files: FileEntry[],
    onProgress: (progress: IndexingProgress) => void,
    clusteringConfig?: ProviderConfig
  ): Promise<IndexingResult> => {
    const api = ensureWorker();

    /* Reset renderer embedding status — worker resets its own state */
    setEmbeddingStatus('idle');
    setEmbeddingProgress(null);

    const proxiedOnProgress = Comlink.proxy(onProgress);
    const serializedResult = await api.runPipelineFromFiles(files, proxiedOnProgress, clusteringConfig);
    return deserializeIndexingResult(serializedResult, createCodeGraph);
  }, [ensureWorker]);

  const runQuery = useCallback(async (cypher: string): Promise<any[]> => {
    const api = ensureWorker();
    return api.runQuery(cypher);
  }, [ensureWorker]);

  const isDatabaseReady = useCallback(async (): Promise<boolean> => {
    if (!workerRef.current) return false;
    const api = ensureWorker();
    try {
      return await api.isReady();
    } catch {
      return false;
    }
  }, [ensureWorker]);

  // Snapshot methods
  const saveSnapshot = useCallback(async (path: string): Promise<{ success: boolean; size: number }> => {
    const prowl = (window as any).prowl;
    if (!prowl?.snapshot) {
      return { success: false, size: 0 };
    }

    const api = ensureWorker();
    try {
      const { data, meta, manifest } = await api.collectAndSerialize();

      // Read git commit in renderer (has access to IPC)
      let gitCommit: string | null = null;
      try {
        if (prowl.fs?.readFile) {
          const head = await prowl.fs.readFile(`${path}/.git/HEAD`);
          const match = head.trim().match(/^ref: (.+)$/);
          if (match) {
            const ref = match[1];
            const commit = await prowl.fs.readFile(`${path}/.git/${ref}`);
            gitCommit = commit.trim();
          } else {
            gitCommit = head.trim();
          }
        }
      } catch { /* not a git repo */ }

      // Patch gitCommit into meta
      const patchedMeta = { ...meta, gitCommit };

      // Generate HMAC
      const hmac = await prowl.snapshot.generateHMAC(data);

      // Write snapshot atomically
      await prowl.snapshot.write(path, data);

      // Write meta with HMAC
      await prowl.snapshot.writeMeta(path, { ...patchedMeta, hmac });

      // Write manifest
      await prowl.snapshot.writeManifest(path, manifest);

      // Ensure .gitignore has .prowl/
      await prowl.snapshot.ensureGitignore(path);

      if (import.meta.env.DEV) {
        console.log(`[prowl:snapshot] Saved: ${(data.byteLength / 1024).toFixed(0)} KB`);
      }

      return { success: true, size: data.byteLength };
    } catch (err) {
      console.warn('[prowl:snapshot] Save failed:', err);
      return { success: false, size: 0 };
    }
  }, [ensureWorker]);

  // Auto-save snapshot every 5 minutes if live updates occurred
  useEffect(() => {
    const timer = setInterval(async () => {
      if (liveUpdateDirtyRef.current && projectPath) {
        try {
          await saveSnapshot(projectPath);
          liveUpdateDirtyRef.current = false;
          if (import.meta.env.DEV) {
            console.log('[prowl:live] auto-saved snapshot');
          }
        } catch {
          /* auto-save is best-effort */
        }
      }
    }, 5 * 60 * 1000);

    return () => clearInterval(timer);
  }, [projectPath, saveSnapshot]);

  // Save snapshot on app close (best-effort)
  useEffect(() => {
    if (!window.prowl?.onSaveSnapshot) return;

    window.prowl.onSaveSnapshot(async () => {
      if (liveUpdateDirtyRef.current && projectPath) {
        try {
          await saveSnapshot(projectPath);
          liveUpdateDirtyRef.current = false;
        } catch {
          /* best-effort on close */
        }
      }
    });
  }, [projectPath, saveSnapshot]);

  // Re-save snapshot when embeddings finish so the next restore includes vectors
  const prevEmbeddingStatusRef = useRef(embeddingStatus);
  useEffect(() => {
    const prev = prevEmbeddingStatusRef.current;
    prevEmbeddingStatusRef.current = embeddingStatus;

    if (embeddingStatus === 'ready' && prev !== 'ready' && projectPath) {
      saveSnapshot(projectPath).catch(console.warn);
    }
  }, [embeddingStatus, projectPath, saveSnapshot]);

  const loadSnapshot = useCallback(async (
    path: string,
    onProgress: (p: IndexingProgress) => void,
  ): Promise<IndexingResult | null> => {
    const prowl = (window as any).prowl;
    if (!prowl?.snapshot) return null;

    const api = ensureWorker();
    try {
      onProgress({ phase: 'extracting', percent: 5, message: 'Reading snapshot...' });

      // Read snapshot data in renderer (IPC)
      const data: Uint8Array | null = await prowl.snapshot.read(path);
      if (!data) return null;

      // Read meta and verify HMAC in renderer (IPC)
      onProgress({ phase: 'extracting', percent: 10, message: 'Verifying integrity...' });
      const meta = await prowl.snapshot.readMeta(path) as any;
      if (!meta?.hmac) return null;

      const valid = await prowl.snapshot.verify(data, meta.hmac);
      if (!valid) {
        console.warn('[prowl:snapshot] HMAC verification failed — full re-index needed');
        return null;
      }

      // Check format version compatibility
      const { SNAPSHOT_FORMAT_VERSION } = await import('../core/snapshot/types');
      if (meta.formatVersion != null && meta.formatVersion !== SNAPSHOT_FORMAT_VERSION) {
        console.warn(`[prowl:snapshot] Format version mismatch: ${meta.formatVersion} vs ${SNAPSHOT_FORMAT_VERSION} — full re-index`);
        return null;
      }

      // Check app version compatibility
      const prowlVersion = (import.meta.env.VITE_APP_VERSION as string) || 'unknown';
      if (meta.prowlVersion && prowlVersion !== 'unknown' && meta.prowlVersion !== prowlVersion) {
        console.warn(`[prowl:snapshot] Version mismatch: ${meta.prowlVersion} vs ${prowlVersion} — full re-index`);
        return null;
      }

      // Send verified data to worker for CPU-heavy restore
      const proxiedOnProgress = Comlink.proxy(onProgress);
      const result = await api.restoreFromSnapshot(
        Comlink.transfer(data, [data.buffer]),
        meta,
        proxiedOnProgress,
      );

      // Set embedding status if snapshot had embeddings
      const hasEmbeddings = !!result.hasEmbeddings;
      if (hasEmbeddings) {
        setEmbeddingStatus('ready');
      }

      // Deserialize to IndexingResult and carry forward hasEmbeddings flag
      const indexingResult = deserializeIndexingResult(result, createCodeGraph);
      return { ...indexingResult, hasEmbeddings };
    } catch (err) {
      console.warn('[prowl:snapshot] Load failed:', err);
      return null;
    }
  }, [ensureWorker]);

  const incrementalUpdate = useCallback(async (
    diff: { added: string[]; modified: string[]; deleted: string[]; isGitRepo: boolean },
    folderPath: string,
    onProgress: (p: IndexingProgress) => void,
  ): Promise<IndexingResult | null> => {
    const prowl = (window as any).prowl;
    const api = ensureWorker();

    try {
      // Read changed/added files from disk in renderer (IPC)
      const newFileContents = new Map<string, string>();
      const filesToRead = [...diff.added, ...diff.modified];
      if (prowl?.fs?.readFile) {
        for (const filePath of filesToRead) {
          try {
            const content = await prowl.fs.readFile(`${folderPath}/${filePath}`);
            newFileContents.set(filePath, content);
          } catch {
            // File might be binary or unreadable — skip
          }
        }
      }

      const proxiedOnProgress = Comlink.proxy(onProgress);
      const serializedResult = await api.incrementalUpdate(diff, newFileContents, proxiedOnProgress);
      if (!serializedResult) return null;
      return deserializeIndexingResult(serializedResult, createCodeGraph);
    } catch {
      return null;
    }
  }, [ensureWorker]);

  // Embedding methods
  const startEmbeddings = useCallback(async (forceDevice?: 'webgpu' | 'wasm'): Promise<void> => {
    const api = ensureWorker();

    setEmbeddingStatus('loading');
    setEmbeddingProgress(null);

    try {
      const proxiedOnProgress = Comlink.proxy((progress: EmbeddingProgress) => {
        setEmbeddingProgress(progress);

        // Update status based on phase
        switch (progress.phase) {
          case 'loading-model':
            setEmbeddingStatus('loading');
            break;
          case 'embedding':
            setEmbeddingStatus('embedding');
            break;
          case 'indexing':
            setEmbeddingStatus('indexing');
            break;
          case 'ready':
            setEmbeddingStatus('ready');
            break;
          case 'error':
            setEmbeddingStatus('error');
            break;
        }
      });

      await api.startEmbeddingPipeline(proxiedOnProgress, forceDevice);
    } catch (error: any) {
      // Check if it's WebGPU not available - let caller handle the dialog
      if (error?.name === 'WebGPUNotAvailableError' ||
        error?.message?.includes('WebGPU not available')) {
        setEmbeddingStatus('idle'); // Reset to idle so user can try again
      } else {
        setEmbeddingStatus('error');
      }
      throw error;
    }
  }, [ensureWorker]);

  const semanticSearch = useCallback(async (
    query: string,
    k: number = 10
  ): Promise<SemanticSearchResult[]> => {
    const api = ensureWorker();
    return api.semanticSearch(query, k);
  }, [ensureWorker]);

  const semanticSearchWithContext = useCallback(async (
    query: string,
    k: number = 5,
    hops: number = 2
  ): Promise<any[]> => {
    const api = ensureWorker();
    return api.semanticSearchWithContext(query, k, hops);
  }, [ensureWorker]);

  const testArrayParams = useCallback(async (): Promise<{ success: boolean; error?: string }> => {
    const api = ensureWorker();
    return api.testArrayParams();
  }, [ensureWorker]);

  // LLM methods
  const updateLLMSettings = useCallback((updates: Partial<LLMSettings>) => {
    setLLMSettings(prev => {
      const next = { ...prev, ...updates };
      saveSettings(next);
      return next;
    });
  }, []);

  const refreshLLMSettings = useCallback(() => {
    setLLMSettings(loadSettings());
  }, []);

  const initializeAgent = useCallback(async (overrideProjectName?: string): Promise<boolean> => {
    const api = ensureWorker();

    const config = getActiveProviderConfig();
    if (!config) {
      setAgentError('Please configure an LLM provider in settings');
      return false;
    }

    if (import.meta.env.DEV) {
      console.log('[prowl:agent] initializing with', config.provider, config.model);
    }

    setIsAgentReady(false);
    setIsAgentInitializing(true);
    setAgentError(null);

    try {
      // Use override if provided (for fresh loads), fallback to state (for re-init)
      const effectiveProjectName = overrideProjectName || projectName || 'project';
      const result = await api.initializeAgent(config, effectiveProjectName);
      if (result.success) {
        setIsAgentReady(true);
        setAgentError(null);
        return true;
      } else {
        setAgentError(result.error ?? 'Failed to initialize agent');
        setIsAgentReady(false);
        return false;
      }
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      setAgentError(message);
      setIsAgentReady(false);
      return false;
    } finally {
      setIsAgentInitializing(false);
    }
  }, [projectName, ensureWorker]);

  /**
   * Build a brief tool usage summary for an assistant message.
   * Included in stored history so the LLM has context about what it already did.
   */
  const buildToolSummary = useCallback((m: ChatMessage): string | undefined => {
    if (m.role !== 'assistant' || !m.steps) return undefined;
    const toolSteps = m.steps.filter(s => s.type === 'tool_call' && s.toolCall);
    if (toolSteps.length === 0) return undefined;

    const summaries = toolSteps.map(s => {
      const tc = s.toolCall!;
      const argStr = Object.entries(tc.args)
        .filter(([, v]) => typeof v === 'string')
        .map(([k, v]) => `${k}="${String(v).slice(0, 60)}"`)
        .join(', ');
      const resultSnippet = tc.result ? tc.result.slice(0, 100) : '';
      return `${tc.name}(${argStr})${resultSnippet ? ` → ${resultSnippet}...` : ''}`;
    });

    return summaries.join('; ');
  }, []);

  /**
   * Build an AgentMessage from a ChatMessage, including tool usage context.
   * This fixes the chatbot loop: the LLM sees what tools it already used.
   */
  const buildAgentMessage = useCallback((m: ChatMessage): AgentMessage => {
    let content = m.content;

    // For assistant messages with tool calls, append a brief summary
    if (m.role === 'assistant' && m.steps) {
      const summary = buildToolSummary(m);
      if (summary) {
        content += `\n\n[Tools used: ${summary}]`;
      }
    }

    return {
      role: m.role === 'tool' ? 'assistant' : m.role,
      content,
    };
  }, [buildToolSummary]);

  const sendChatMessage = useCallback(async (message: string): Promise<void> => {
    const api = ensureWorker();

    // Drop stale AI refs before the new response starts
    resetCodeRefs();
    resetToolHighlights();

    if (!isAgentReady) {
      // Try to initialize first
      const ok = await initializeAgent();
      if (!ok) {
        // Show the user's message + an error so they know what happened
        setChatMessages(prev => [
          ...prev,
          { id: `user-${Date.now()}`, role: 'user', content: message, timestamp: Date.now() },
          { id: `assistant-${Date.now()}`, role: 'assistant', content: 'Could not connect to AI provider. Check your API key and model in settings (Cmd+T).', timestamp: Date.now() },
        ]);
        return;
      }
    }

    // Add user message
    const userMessage: ChatMessage = {
      id: `user-${Date.now()}`,
      role: 'user',
      content: message,
      timestamp: Date.now(),
    };
    setChatMessages(prev => [...prev, userMessage]);

    // If embeddings are running and we're currently creating the vector index,
    // avoid a confusing "Embeddings not ready" error and give a clear wait message.
    if (embeddingStatus === 'indexing') {
      const assistantMessage: ChatMessage = {
        id: `assistant-${Date.now()}`,
        role: 'assistant',
        content: 'Wait a moment, vector index is being created.',
        timestamp: Date.now(),
      };
      setChatMessages(prev => [...prev, assistantMessage]);
      setAgentError(null);
      setIsChatLoading(false);
      setCurrentToolCalls([]);
      return;
    }

    setIsChatLoading(true);
    setCurrentToolCalls([]);

    // Build history with tool summaries (fixes chatbot loop)
    let history: AgentMessage[] = [...chatMessages, userMessage].map(buildAgentMessage);

    // If we have a compacted summary from a prior compaction, prepend it
    if (compactedSummary && history.length > 0) {
      history = [
        { role: 'user', content: `[Previous conversation summary]\n${compactedSummary}\n[End of summary]` },
        { role: 'assistant', content: 'Understood. I have the context from our previous discussion.' },
        ...history,
      ];
    }

    // Context compaction: if history is too long, summarize older messages
    const estimatedTokens = estimateHistoryTokens(history);
    if (estimatedTokens > COMPACTION_THRESHOLD) {
      try {
        setIsCompacting(true);
        const result = await api.compactHistory(history);
        if (result.summary) {
          history = result.compacted;
          setCompactedSummary(result.summary);
        }
      } catch (err) {
        console.warn('[prowl:chat] compaction failed, sending full history:', err);
      } finally {
        setIsCompacting(false);
      }
    }

    // Create placeholder for assistant response
    const assistantMessageId = `assistant-${Date.now()}`;
    const stepsForMessage: MessageStep[] = [];
    const toolCallsForMessage: ToolCallInfo[] = [];
    let stepCounter = 0;
    // Track citations already emitted so the regex scan doesn't re-fire addCodeReference
    const emittedCitations = new Set<string>();

    const updateMessage = () => {
      const contentParts = stepsForMessage
        .filter(s => s.type === 'reasoning' || s.type === 'content')
        .map(s => s.content)
        .filter(Boolean);
      const content = contentParts.join('\n\n');

      setChatMessages(prev => {
        const existing = prev.find(m => m.id === assistantMessageId);
        const newMessage: ChatMessage = {
          id: assistantMessageId,
          role: 'assistant' as const,
          content,
          steps: [...stepsForMessage],
          toolCalls: [...toolCallsForMessage],
          timestamp: existing?.timestamp ?? Date.now(),
        };
        if (existing) {
          return prev.map(m => m.id === assistantMessageId ? newMessage : m);
        } else {
          return [...prev, newMessage];
        }
      });
    };

    try {
      const onChunk = Comlink.proxy((chunk: AgentStreamChunk) => {
        switch (chunk.type) {
          case 'reasoning':
            if (chunk.reasoning) {
              const lastStep = stepsForMessage[stepsForMessage.length - 1];
              if (lastStep && lastStep.type === 'reasoning') {
                stepsForMessage[stepsForMessage.length - 1] = {
                  ...lastStep,
                  content: (lastStep.content || '') + chunk.reasoning,
                };
              } else {
                stepsForMessage.push({
                  id: `step-${stepCounter++}`,
                  type: 'reasoning',
                  content: chunk.reasoning,
                });
              }
              updateMessage();
            }
            break;

          case 'content':
            if (chunk.content) {
              const lastStep = stepsForMessage[stepsForMessage.length - 1];
              if (lastStep && lastStep.type === 'content') {
                stepsForMessage[stepsForMessage.length - 1] = {
                  ...lastStep,
                  content: (lastStep.content || '') + chunk.content,
                };
              } else {
                stepsForMessage.push({
                  id: `step-${stepCounter++}`,
                  type: 'content',
                  content: chunk.content,
                });
              }
              updateMessage();

              // Extract [[file:line]] and [[Type:Name]] citations from the streamed content
              const currentContentStep = stepsForMessage[stepsForMessage.length - 1];
              const fullText = (currentContentStep && currentContentStep.type === 'content')
                ? (currentContentStep.content || '')
                : '';

              // Pattern 1: File refs - [[path/file.ext]] or [[path/file.ext:line]] or [[path/file.ext:line-line]]
              // Line numbers are optional
              const fileRefRegex = /\[\[([a-zA-Z0-9_\-./\\]+\.[a-zA-Z0-9]+)(?::(\d+)(?:[-–](\d+))?)?\]\]/g;
              let fileMatch: RegExpExecArray | null;
              while ((fileMatch = fileRefRegex.exec(fullText)) !== null) {
                const citationKey = fileMatch[0];
                if (emittedCitations.has(citationKey)) continue;
                emittedCitations.add(citationKey);

                const rawPath = fileMatch[1].trim();
                const startLine1 = fileMatch[2] ? parseInt(fileMatch[2], 10) : undefined;
                const endLine1 = fileMatch[3] ? parseInt(fileMatch[3], 10) : startLine1;

                const resolvedPath = resolveFilePath(rawPath);
                if (!resolvedPath) continue;

                const startLine0 = startLine1 !== undefined ? Math.max(0, startLine1 - 1) : undefined;
                const endLine0 = endLine1 !== undefined ? Math.max(0, endLine1 - 1) : startLine0;
                const nodeId = findFileNodeId(resolvedPath);

                addCodeReference({
                  filePath: resolvedPath,
                  startLine: startLine0,
                  endLine: endLine0,
                  nodeId,
                  label: 'File',
                  name: resolvedPath.split('/').pop() ?? resolvedPath,
                  source: 'ai',
                });
              }

              // Pattern 2: Node refs - [[Type:Name]] or [[graph:Type:Name]]
              const nodeRefRegex = /\[\[(?:graph:)?(Class|Function|Method|Interface|File|Folder|Variable|Enum|Type|CodeElement):([^\]]+)\]\]/g;
              let nodeMatch: RegExpExecArray | null;
              while ((nodeMatch = nodeRefRegex.exec(fullText)) !== null) {
                const citationKey = nodeMatch[0];
                if (emittedCitations.has(citationKey)) continue;
                emittedCitations.add(citationKey);

                const nodeType = nodeMatch[1];
                const nodeName = nodeMatch[2].trim();

                // Find node in graph
                if (!graph) continue;
                const node = graph.nodes.find(n =>
                  n.label === nodeType &&
                  n.properties.name === nodeName
                );
                if (!node || !node.properties.filePath) continue;

                const resolvedPath = resolveFilePath(node.properties.filePath);
                if (!resolvedPath) continue;

                addCodeReference({
                  filePath: resolvedPath,
                  startLine: node.properties.startLine ? node.properties.startLine - 1 : undefined,
                  endLine: node.properties.endLine ? node.properties.endLine - 1 : undefined,
                  nodeId: node.id,
                  label: node.label,
                  name: node.properties.name,
                  source: 'ai',
                });
              }
            }
            break;

          case 'tool_call':
            if (chunk.toolCall) {
              const tc = chunk.toolCall;
              toolCallsForMessage.push(tc);
              // Add tool call as a step (in order with reasoning)
              stepsForMessage.push({
                id: `step-${stepCounter++}`,
                type: 'tool_call',
                toolCall: tc,
              });
              setCurrentToolCalls(prev => [...prev, tc]);
              updateMessage();
            }
            break;

          case 'tool_result':
            if (chunk.toolCall) {
              const tc = chunk.toolCall;
              // Update the tool call status in toolCallsForMessage
              let idx = toolCallsForMessage.findIndex(t => t.id === tc.id);
              if (idx < 0) {
                idx = toolCallsForMessage.findIndex(t => t.name === tc.name && t.status === 'running');
              }
              if (idx < 0) {
                idx = toolCallsForMessage.findIndex(t => t.name === tc.name && !t.result);
              }
              if (idx >= 0) {
                toolCallsForMessage[idx] = {
                  ...toolCallsForMessage[idx],
                  result: tc.result,
                  status: 'completed'
                };
              }

              // Also update the tool call in steps
              const stepIdx = stepsForMessage.findIndex(s =>
                s.type === 'tool_call' && s.toolCall && (
                  s.toolCall.id === tc.id ||
                  (s.toolCall.name === tc.name && s.toolCall.status === 'running')
                )
              );
              if (stepIdx >= 0 && stepsForMessage[stepIdx].toolCall) {
                stepsForMessage[stepIdx] = {
                  ...stepsForMessage[stepIdx],
                  toolCall: {
                    ...stepsForMessage[stepIdx].toolCall!,
                    result: tc.result,
                    status: 'completed',
                  },
                };
              }

              // Update currentToolCalls
              setCurrentToolCalls(prev => {
                let targetIdx = prev.findIndex(t => t.id === tc.id);
                if (targetIdx < 0) {
                  targetIdx = prev.findIndex(t => t.name === tc.name && t.status === 'running');
                }
                if (targetIdx < 0) {
                  targetIdx = prev.findIndex(t => t.name === tc.name && !t.result);
                }
                if (targetIdx >= 0) {
                  return prev.map((t, i) => i === targetIdx
                    ? { ...t, result: tc.result, status: 'completed' }
                    : t
                  );
                }
                return prev;
              });

              updateMessage();

              // Detect [HIGHLIGHT_NODES:...] and [IMPACT:...] markers in tool output
              if (tc.result) {
                const highlightMatch = tc.result.match(/\[HIGHLIGHT_NODES:([^\]]+)\]/);
                if (highlightMatch) {
                  const rawIds = highlightMatch[1].split(',').map((id: string) => id.trim()).filter(Boolean);
                  if (rawIds.length > 0 && graph) {
                    const matchedIds = new Set<string>();
                    const graphNodeIds = graph.nodes.map(n => n.id);

                    for (const rawId of rawIds) {
                      if (graphNodeIds.includes(rawId)) {
                        matchedIds.add(rawId);
                      } else {
                        const found = graphNodeIds.find(gid =>
                          gid.endsWith(rawId) || gid.endsWith(':' + rawId)
                        );
                        if (found) {
                          matchedIds.add(found);
                        }
                      }
                    }

                    if (matchedIds.size > 0) {
                      setAIToolHighlightedNodeIds(matchedIds);
                    }
                  } else if (rawIds.length > 0) {
                    setAIToolHighlightedNodeIds(new Set(rawIds));
                  }
                }

                const impactMatch = tc.result.match(/\[IMPACT:([^\]]+)\]/);
                if (impactMatch) {
                  const rawIds = impactMatch[1].split(',').map((id: string) => id.trim()).filter(Boolean);
                  if (rawIds.length > 0 && graph) {
                    const matchedIds = new Set<string>();
                    const graphNodeIds = graph.nodes.map(n => n.id);

                    for (const rawId of rawIds) {
                      if (graphNodeIds.includes(rawId)) {
                        matchedIds.add(rawId);
                      } else {
                        const found = graphNodeIds.find(gid =>
                          gid.endsWith(rawId) || gid.endsWith(':' + rawId)
                        );
                        if (found) {
                          matchedIds.add(found);
                        }
                      }
                    }

                    if (matchedIds.size > 0) {
                      setBlastRadiusNodeIds(matchedIds);
                    }
                  } else if (rawIds.length > 0) {
                    setBlastRadiusNodeIds(new Set(rawIds));
                  }
                }
              }
            }
            break;

          case 'error':
            setAgentError(chunk.error ?? 'Unknown error');
            break;

          case 'done':
            updateMessage();
            break;
        }
      });

      await api.chatStream(history, onChunk);

      // Auto-save conversation after successful response
      if (projectPath && conversationId) {
        setChatMessages(prev => {
          const conv: StoredConversation = {
            id: conversationId,
            projectPath: projectPath!,
            title: prev.find(m => m.role === 'user')?.content.slice(0, 80) || 'Untitled',
            messages: prev
              .filter(m => m.role !== 'tool')
              .map(m => ({
                id: m.id,
                role: m.role as 'user' | 'assistant',
                content: m.content,
                toolSummary: buildToolSummary(m),
                timestamp: m.timestamp,
              })),
            compactedSummary: compactedSummary ?? undefined,
            createdAt: prev[0]?.timestamp ?? Date.now(),
            updatedAt: Date.now(),
          };
          window.prowl?.conversations?.save(projectPath!, conv)
            .then(() => window.prowl?.conversations?.list(projectPath!))
            .then(list => { if (list) setConversations(list); })
            .catch(console.warn);
          return prev; // don't modify messages
        });
      }
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      setAgentError(message);
    } finally {
      setIsChatLoading(false);
      setCurrentToolCalls([]);
    }
  }, [chatMessages, isAgentReady, initializeAgent, resolveFilePath, findFileNodeId, addCodeReference, resetCodeRefs, resetToolHighlights, graph, embeddingStatus, ensureWorker, buildAgentMessage, compactedSummary, projectPath, conversationId, buildToolSummary]);

  const stopChatResponse = useCallback(() => {
    if (workerRef.current && apiRef.current && isChatLoading) {
      apiRef.current.stopChat();
      setIsChatLoading(false);
      setCurrentToolCalls([]);
    }
  }, [isChatLoading]);

  const clearChat = useCallback(() => {
    // Save current conversation before clearing (if it has messages)
    if (projectPath && conversationId && chatMessages.length > 0) {
      const conv: StoredConversation = {
        id: conversationId,
        projectPath,
        title: chatMessages.find(m => m.role === 'user')?.content.slice(0, 80) || 'Untitled',
        messages: chatMessages
          .filter(m => m.role !== 'tool')
          .map(m => ({
            id: m.id,
            role: m.role as 'user' | 'assistant',
            content: m.content,
            toolSummary: buildToolSummary(m),
            timestamp: m.timestamp,
          })),
        compactedSummary: compactedSummary ?? undefined,
        createdAt: chatMessages[0]?.timestamp ?? Date.now(),
        updatedAt: Date.now(),
      };
      window.prowl?.conversations?.save(projectPath, conv).catch(console.warn);
    }

    setChatMessages([]);
    setCurrentToolCalls([]);
    setAgentError(null);
    setConversationId(`conv-${Date.now()}`);
    setCompactedSummary(null);
  }, [projectPath, conversationId, chatMessages, compactedSummary]);

  // Load conversations list from disk when project changes
  useEffect(() => {
    if (!projectPath) return;
    window.prowl?.conversations?.list(projectPath)
      .then(setConversations)
      .catch(console.warn);
    // Start a new conversation ID for the new project
    setConversationId(`conv-${Date.now()}`);
    setCompactedSummary(null);
  }, [projectPath]);

  // Load a previous conversation
  const loadConversation = useCallback((id: string) => {
    const conv = conversations.find(c => c.id === id);
    if (!conv) return;

    const restored: ChatMessage[] = conv.messages.map(m => ({
      id: m.id,
      role: m.role,
      content: m.content,
      timestamp: m.timestamp,
    }));

    setChatMessages(restored);
    setConversationId(conv.id);
    setCompactedSummary(conv.compactedSummary ?? null);
    setCurrentToolCalls([]);
    setAgentError(null);
  }, [conversations]);

  // Start a new conversation (saves current first)
  const startNewConversation = useCallback(() => {
    clearChat();
  }, [clearChat]);

  const removeCodeReference = useCallback((id: string) => {
    setCodeReferences(prev => {
      const ref = prev.find(r => r.id === id);
      const newRefs = prev.filter(r => r.id !== id);

      // Remove AI citation highlight if this was the only AI reference to that node
      if (ref?.nodeId && ref.source === 'ai') {
        const stillReferenced = newRefs.some(r => r.nodeId === ref.nodeId && r.source === 'ai');
        if (!stillReferenced) {
          setAICitationHighlightedNodeIds(prev => {
            const next = new Set(prev);
            next.delete(ref.nodeId!);
            return next;
          });
        }
      }

      // Auto-close panel if no references left AND no selection in top viewer
      if (newRefs.length === 0 && !selectedNode) {
        setCodePanelOpen(false);
      }

      return newRefs;
    });
  }, [selectedNode]);

  const clearCodeReferences = useCallback(() => {
    setCodeReferences([]);
    setCodePanelOpen(false);
    setCodeReferenceFocus(null);
  }, []);

  const toggleLabelVisibility = useCallback((label: NodeLabel) => {
    setVisibleLabels(prev => {
      if (prev.includes(label)) {
        return prev.filter(l => l !== label);
      } else {
        return [...prev, label];
      }
    });
  }, []);

  const toggleEdgeVisibility = useCallback((edgeType: EdgeType) => {
    setVisibleEdgeTypes(prev => {
      if (prev.includes(edgeType)) {
        return prev.filter(t => t !== edgeType);
      } else {
        return [...prev, edgeType];
      }
    });
  }, []);

  const value: AppState = {
    viewMode,
    setViewMode,
    graph,
    setGraph,
    fileContents,
    setFileContents,
    selectedNode,
    setSelectedNode,
    isRightPanelOpen,
    setRightPanelOpen,
    rightPanelTab,
    setRightPanelTab,
    openCodePanel,
    openChatPanel,
    visibleLabels,
    toggleLabelVisibility,
    visibleEdgeTypes,
    toggleEdgeVisibility,
    depthFilter,
    setDepthFilter,
    highlightedNodeIds,
    setHighlightedNodeIds,
    aiCitationHighlightedNodeIds,
    aiToolHighlightedNodeIds,
    blastRadiusNodeIds,
    isAIHighlightsEnabled,
    toggleAIHighlights,
    resetToolHighlights,
    clearBlastRadius,
    queryResult,
    setQueryResult,
    clearQueryHighlights,
    animatedNodes,
    triggerNodeAnimation,
    clearAnimations,
    progress,
    setProgress,
    projectName,
    setProjectName,
    runPipelineFromFiles,
    runQuery,
    isDatabaseReady,
    embeddingStatus,
    embeddingProgress,
    startEmbeddings,
    semanticSearch,
    semanticSearchWithContext,
    isEmbeddingReady: embeddingStatus === 'ready',
    testArrayParams,
    llmSettings,
    updateLLMSettings,
    isSettingsPanelOpen,
    setSettingsPanelOpen,
    isAgentReady,
    isAgentInitializing,
    agentError,
    chatMessages,
    isChatLoading,
    currentToolCalls,
    refreshLLMSettings,
    initializeAgent,
    sendChatMessage,
    stopChatResponse,
    clearChat,
    conversationId,
    conversations,
    loadConversation,
    startNewConversation,
    isCompacting,
    codeReferences,
    isCodePanelOpen,
    setCodePanelOpen,
    addCodeReference,
    removeCodeReference,
    resetCodeRefs,
    clearCodeReferences,
    codeReferenceFocus,
    isLiveUpdating,
    agentWatcherState,
    startAgentWatcher,
    stopAgentWatcher,
    resetForNewProject,
    getWorkerApi: () => apiRef.current,
    projectPath,
    setProjectPath,
    loadedFromSnapshot,
    setLoadedFromSnapshot,
    saveSnapshot,
    loadSnapshot,
    incrementalUpdate,
  };

  return (
    <AppStateContext.Provider value={value}>
      {children}
    </AppStateContext.Provider>
  );
};

export const useAppState = (): AppState => {
  const context = useContext(AppStateContext);
  if (!context) {
    throw new Error('useAppState called outside provider');
  }
  return context;
};

