import { useState, useRef, useEffect, useCallback } from 'react';
import {
  Send, Square, MessageSquare, User,
  PanelRightClose, Loader2, AlertTriangle, GitBranch, Radio, Sparkles, Trash2,
  ChevronDown, ChevronRight, Wrench
} from 'lucide-react';
import { useAppState } from '../hooks/useAppState';
import { ToolCallPill } from './ToolCallPill';
import { isProviderConfigured, getActiveProviderConfig } from '../core/llm/settings-service';
import { MarkdownRenderer } from './MarkdownRenderer';
import { ProcessesPanel } from './ProcessesPanel';
import { AgentPanel } from './AgentPanel';
import { ContextBridge } from './ContextBridge';

const isElectron = typeof window !== 'undefined' && !!(window as any).prowl;

// Collapsible group for tool call pills — shows summary when many completed calls
const ToolCallGroup = ({ steps }: { steps: { id: string; toolCall: any }[] }) => {
  const [expanded, setExpanded] = useState(false);
  const allDone = steps.every(s => s.toolCall?.status === 'completed' || s.toolCall?.status === 'error');
  const runningCount = steps.filter(s => s.toolCall?.status === 'running').length;
  const MAX_VISIBLE = 3;

  // If few pills or still running, show them all
  if (steps.length <= MAX_VISIBLE || !allDone) {
    return (
      <div className="flex flex-wrap gap-1.5 mb-2 items-center">
        {steps.map((ts) => (
          <ToolCallPill key={ts.id} toolCall={ts.toolCall!} />
        ))}
        {runningCount > 0 && steps.length > MAX_VISIBLE && (
          <span className="text-[11px] text-text-muted ml-1">
            {steps.length - runningCount} done, {runningCount} running...
          </span>
        )}
      </div>
    );
  }

  // Many completed pills — collapse into summary
  return (
    <div className="mb-2">
      <button
        onClick={() => setExpanded(e => !e)}
        className="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-md text-[12px] bg-white/[0.04] border border-white/[0.08] text-text-secondary hover:bg-white/[0.06] transition-all cursor-pointer select-none"
      >
        <Wrench size={12} className="opacity-50" />
        <span>{steps.length} tool calls completed</span>
        {expanded ? <ChevronDown size={11} className="opacity-40" /> : <ChevronRight size={11} className="opacity-40" />}
      </button>
      {expanded && (
        <div className="mt-1.5 flex flex-wrap gap-1.5">
          {steps.map((ts) => (
            <ToolCallPill key={ts.id} toolCall={ts.toolCall!} />
          ))}
        </div>
      )}
    </div>
  );
};

interface RightPanelProps {
  onFocusNode: (nodeId: string) => void;
}

export const RightPanel = ({ onFocusNode }: RightPanelProps) => {
  const {
    isRightPanelOpen,
    setRightPanelOpen,
    fileContents,
    graph,
    // LLM / chat state
    chatMessages,
    isChatLoading,
    currentToolCalls,
    agentError,
    isAgentReady,
    isAgentInitializing,
    sendChatMessage,
    stopChatResponse,
    clearChat,
  } = useAppState();

  const [chatInput, setChatInput] = useState('');
  const [activeTab, setActiveTab] = useState<'chat' | 'processes' | 'agent'>('chat');
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const messagesEndRef = useRef<HTMLDivElement>(null);

  // Resizable width
  const [panelWidth, setPanelWidth] = useState(35); // percentage
  const isResizingRef = useRef(false);

  const handleResizeStart = useCallback((e: React.MouseEvent) => {
    e.preventDefault();
    isResizingRef.current = true;
    const startX = e.clientX;
    const startWidth = panelWidth;
    const windowWidth = window.innerWidth;

    const onMouseMove = (ev: MouseEvent) => {
      if (!isResizingRef.current) return;
      const delta = startX - ev.clientX;
      const newPct = startWidth + (delta / windowWidth) * 100;
      setPanelWidth(Math.min(60, Math.max(20, newPct)));
    };

    const onMouseUp = () => {
      isResizingRef.current = false;
      document.removeEventListener('mousemove', onMouseMove);
      document.removeEventListener('mouseup', onMouseUp);
      document.body.style.cursor = '';
      document.body.style.userSelect = '';
    };

    document.body.style.cursor = 'col-resize';
    document.body.style.userSelect = 'none';
    document.addEventListener('mousemove', onMouseMove);
    document.addEventListener('mouseup', onMouseUp);
  }, [panelWidth]);

  // Auto-scroll to bottom when messages update or while streaming
  useEffect(() => {
    if (messagesEndRef.current) {
      messagesEndRef.current.scrollIntoView({ behavior: 'smooth' });
    }
  }, [chatMessages, isChatLoading]);

  const resolveFilePathForUI = useCallback((requestedPath: string): string | null => {
    const req = requestedPath.replace(/\\/g, '/').replace(/^\.?\//, '').toLowerCase();
    if (!req) return null;

    // Exact match first (case-insensitive)
    for (const key of fileContents.keys()) {
      const norm = key.replace(/\\/g, '/').replace(/^\.?\//, '').toLowerCase();
      if (norm === req) return key;
    }

    // Ends-with match (best for partial paths)
    let best: { path: string; score: number } | null = null;
    for (const key of fileContents.keys()) {
      const norm = key.replace(/\\/g, '/').replace(/^\.?\//, '').toLowerCase();
      if (norm.endsWith(req)) {
        const score = 1000 - norm.length;
        if (!best || score > best.score) best = { path: key, score };
      }
    }
    return best?.path ?? null;
  }, [fileContents]);

  // Find graph node by file path
  const findFileNodeForUI = useCallback((filePath: string) => {
    if (!graph) return undefined;
    const target = filePath.replace(/\\/g, '/').replace(/^\.?\//, '');
    return graph.nodes.find(
      (n) => n.label === 'File' && n.properties.filePath.replace(/\\/g, '/').replace(/^\.?\//, '') === target
    );
  }, [graph]);

  const handleGroundingClick = useCallback((inner: string) => {
    const raw = inner.trim();
    if (!raw) return;

    let rawPath = raw;

    // Strip line numbers for path resolution
    const lineMatch = raw.match(/^(.*):(\d+(?:[,\-–]\d+)*)$/);
    if (lineMatch) {
      rawPath = lineMatch[1].trim();
    }

    const resolvedPath = resolveFilePathForUI(rawPath);
    if (!resolvedPath) return;

    const node = findFileNodeForUI(resolvedPath);

    // Focus the graph node — no code panel, just visual focus
    if (node?.id) {
      onFocusNode(node.id);
    }
  }, [findFileNodeForUI, resolveFilePathForUI, onFocusNode]);

  // Handler for node grounding: [[Class:View]], [[Function:trigger]], etc.
  const handleNodeGroundingClick = useCallback((nodeTypeAndName: string) => {
    const raw = nodeTypeAndName.trim();
    if (!raw || !graph) return;

    const match = raw.match(/^(Class|Function|Method|Interface|File|Folder|Variable|Enum|Type|CodeElement):(.+)$/);
    if (!match) return;

    const [, nodeType, nodeName] = match;
    const trimmedName = nodeName.trim();

    const node = graph.nodes.find(n =>
      n.label === nodeType &&
      n.properties.name === trimmedName
    );

    if (!node) {
      console.warn(`Node not found: ${nodeType}:${trimmedName}`);
      return;
    }

    // Focus the graph node — no code panel, just visual focus
    onFocusNode(node.id);
  }, [graph, onFocusNode]);

  const handleLinkClick = useCallback((href: string) => {
    if (href.startsWith('code-ref:')) {
      const inner = decodeURIComponent(href.slice('code-ref:'.length));
      handleGroundingClick(inner);
    } else if (href.startsWith('node-ref:')) {
      const inner = decodeURIComponent(href.slice('node-ref:'.length));
      handleNodeGroundingClick(inner);
    }
  }, [handleGroundingClick, handleNodeGroundingClick]);

  // Auto-resize textarea as user types
  const adjustTextareaHeight = useCallback(() => {
    const textarea = textareaRef.current;
    if (!textarea) return;

    textarea.style.height = 'auto';
    const maxHeight = 160;
    const newHeight = Math.min(textarea.scrollHeight, maxHeight);
    textarea.style.height = `${newHeight}px`;
    textarea.style.overflowY = textarea.scrollHeight > maxHeight ? 'auto' : 'hidden';
  }, []);

  useEffect(() => {
    adjustTextareaHeight();
  }, [chatInput, adjustTextareaHeight]);

  const handleSendMessage = async () => {
    if (!chatInput.trim()) return;
    const text = chatInput.trim();
    setChatInput('');
    if (textareaRef.current) {
      textareaRef.current.style.height = '36px';
      textareaRef.current.style.overflowY = 'hidden';
    }
    await sendChatMessage(text);
  };

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      handleSendMessage();
    }
  };

  const chatSuggestions = [
    'Explain the project architecture',
    'What does this project do?',
    'Show me the most important files',
    'Find all API handlers',
  ];

  const tabs = [
    { id: 'chat' as const, label: 'Chat', icon: MessageSquare },
    { id: 'processes' as const, label: 'Processes', icon: GitBranch },
    ...(isElectron ? [{ id: 'agent' as const, label: 'Agent', icon: Radio }] : []),
  ];

  if (!isRightPanelOpen) return null;

  return (
    <aside
      className="flex flex-col bg-void border-l border-white/[0.08] animate-fade-in relative z-30 flex-shrink-0"
      style={{ width: `${panelWidth}%`, minWidth: 320 }}
    >
      {/* Resize handle */}
      <div
        onMouseDown={handleResizeStart}
        className="absolute left-0 top-0 bottom-0 w-1 cursor-col-resize z-40 hover:bg-accent/40 active:bg-accent/60 transition-colors"
      />
      {/* Tab bar — underline style */}
      <div className="flex items-center justify-between px-4 py-0 glass border-b border-white/[0.08]">
        <div className="flex items-center gap-0">
          {tabs.map(({ id, label, icon: Icon }) => (
            <button
              key={id}
              onClick={() => setActiveTab(id)}
              className={`
                flex items-center gap-1.5 px-3 py-2.5 text-[13px] transition-all border-b-2 -mb-px
                ${activeTab === id
                  ? 'border-accent text-text-primary'
                  : 'border-transparent text-text-muted hover:text-text-secondary'
                }
              `}
            >
              <Icon className="w-3.5 h-3.5" />
              {label}
            </button>
          ))}
        </div>

        <button
          onClick={() => setRightPanelOpen(false)}
          className="p-1.5 text-text-muted hover:text-text-primary hover:bg-white/[0.08] rounded-md transition-colors"
          title="Close Panel"
        >
          <PanelRightClose className="w-4 h-4" />
        </button>
      </div>

      {/* Processes Tab */}
      {activeTab === 'processes' && (
        <div className="flex-1 flex flex-col overflow-hidden">
          <ProcessesPanel />
        </div>
      )}

      {/* Agent Tab (Electron only) */}
      {activeTab === 'agent' && (
        <div className="flex-1 flex flex-col overflow-hidden">
          <AgentPanel />
        </div>
      )}

      {/* Chat Content */}
      {activeTab === 'chat' && (
        <div className="flex-1 flex flex-col overflow-hidden">
          {/* Status bar with model info */}
          <div className="flex items-center gap-2.5 px-4 py-1.5 border-b border-white/[0.08]">
            {(() => {
              const config = getActiveProviderConfig();
              if (config) {
                const modelName = config.model.split('/').pop() || config.model;
                return (
                  <span className="text-[11px] px-2 py-0.5 rounded-full bg-white/[0.04] border border-white/[0.08] text-text-muted flex items-center gap-1.5">
                    <span className="w-1.5 h-1.5 rounded-full bg-[#30D158]" />
                    {modelName}
                  </span>
                );
              }
              return null;
            })()}
            <div className="ml-auto flex items-center gap-2">
              {!isAgentReady && !isAgentInitializing && (
                <span className="text-[11px] px-2 py-0.5 rounded-full bg-[#FF9F0A]/10 text-[#FF9F0A] border border-[#FF9F0A]/20">
                  Configure AI
                </span>
              )}
              {isAgentInitializing && (
                <span className="text-[11px] px-2 py-0.5 rounded-full bg-white/[0.06] border border-white/[0.1] flex items-center gap-1 text-text-muted">
                  <Loader2 className="w-3 h-3 animate-spin" /> Connecting
                </span>
              )}
            </div>
          </div>

          {/* Errors */}
          {agentError && (
            <div className="px-4 py-2.5 bg-[#FF453A]/10 border-b border-[#FF453A]/20 text-[#FF453A] text-[12px] flex items-center gap-2">
              <AlertTriangle className="w-3.5 h-3.5" />
              <span>{agentError}</span>
            </div>
          )}

          {/* Messages */}
          <div className="flex-1 overflow-y-auto p-4 scrollbar-thin">
            {chatMessages.length === 0 ? (
              <div className="flex flex-col items-center justify-center h-full text-center px-4">
                <div className="w-10 h-10 mb-3 flex items-center justify-center rounded-full bg-accent/10 border border-accent/20">
                  <Sparkles className="w-4 h-4 text-accent" />
                </div>
                <h3 className="text-[14px] font-medium text-text-primary mb-1">
                  Ask about this codebase
                </h3>
                <p className="text-[12px] text-text-muted leading-relaxed mb-5 max-w-[260px]">
                  Architecture, functions, connections — Prowl understands your code graph.
                </p>
                <div className="flex flex-wrap gap-1.5 justify-center max-w-[340px]">
                  {chatSuggestions.map((suggestion) => (
                    <button
                      key={suggestion}
                      onClick={() => setChatInput(suggestion)}
                      className="px-2.5 py-1.5 bg-white/[0.04] border border-white/[0.08] rounded-full text-[11px] text-text-secondary hover:border-accent/30 hover:text-text-primary hover:bg-accent/5 transition-all"
                    >
                      {suggestion}
                    </button>
                  ))}
                </div>
              </div>
            ) : (
              <div className="flex flex-col gap-6">
                {chatMessages.map((message) => (
                  <div key={message.id} className="animate-fade-in">
                    {/* User message */}
                    {message.role === 'user' && (
                      <div className="mb-1">
                        <div className="flex items-start gap-2.5">
                          <div className="w-6 h-6 rounded-full bg-white/[0.08] flex items-center justify-center shrink-0 mt-0.5">
                            <User className="w-3 h-3 text-text-muted" />
                          </div>
                          <div className="flex-1 min-w-0">
                            <span className="text-[11px] text-text-muted uppercase tracking-wide">You</span>
                            <div className="mt-1 px-3 py-2 rounded-lg bg-white/[0.06] border border-white/[0.08] text-[13px] text-text-primary">
                              {message.content}
                            </div>
                          </div>
                        </div>
                      </div>
                    )}

                    {/* Assistant message */}
                    {message.role === 'assistant' && (
                      <div>
                        <div className="flex items-start gap-2.5">
                          <div className="w-6 h-6 rounded-full bg-accent/15 flex items-center justify-center shrink-0 mt-0.5">
                            <MessageSquare className="w-3 h-3 text-accent" />
                          </div>
                          <div className="flex-1 min-w-0">
                            <div className="flex items-center gap-2">
                              <span className="text-[11px] text-text-muted uppercase tracking-wide">Prowl</span>
                              {isChatLoading && message === chatMessages[chatMessages.length - 1] && (
                                <Loader2 className="w-3 h-3 animate-spin text-accent" />
                              )}
                            </div>
                            <div className="mt-1.5 chat-prose">
                              {message.steps && message.steps.length > 0 ? (
                                <div className="space-y-2">
                                  {/* Group consecutive tool calls, collapse large groups */}
                                  {message.steps.reduce<{ groups: React.ReactNode[]; pendingTools: typeof message.steps }>((acc, step, idx) => {
                                    if (step.type === 'tool_call' && step.toolCall) {
                                      acc.pendingTools.push(step);
                                    } else {
                                      if (acc.pendingTools.length > 0) {
                                        acc.groups.push(
                                          <ToolCallGroup key={`tools-${acc.pendingTools[0].id}`} steps={acc.pendingTools as any} />
                                        );
                                        acc.pendingTools = [];
                                      }
                                      if (step.type === 'reasoning' && step.content) {
                                        acc.groups.push(
                                          <div key={step.id} className="text-text-secondary text-[13px] italic border-l-2 border-white/[0.15] pl-3 mb-2">
                                            <MarkdownRenderer content={step.content} onLinkClick={handleLinkClick} />
                                          </div>
                                        );
                                      }
                                      if (step.type === 'content' && step.content) {
                                        acc.groups.push(
                                          <MarkdownRenderer key={step.id} content={step.content} onLinkClick={handleLinkClick} />
                                        );
                                      }
                                    }
                                    if (idx === message.steps!.length - 1 && acc.pendingTools.length > 0) {
                                      acc.groups.push(
                                        <ToolCallGroup key={`tools-${acc.pendingTools[0].id}`} steps={acc.pendingTools as any} />
                                      );
                                    }
                                    return acc;
                                  }, { groups: [], pendingTools: [] }).groups}
                                </div>
                              ) : (
                                <MarkdownRenderer
                                  content={message.content}
                                  onLinkClick={handleLinkClick}
                                  toolCalls={message.toolCalls}
                                />
                              )}
                            </div>
                          </div>
                        </div>
                      </div>
                    )}
                  </div>
                ))}
              </div>
            )}
            <div ref={messagesEndRef} />
          </div>

          {/* Input */}
          <div className="p-3 border-t border-white/[0.08] bg-deep/80">
            {/* Context Bridge */}
            <ContextBridge />

            <div className="flex items-end gap-2 px-3 py-1.5 bg-white/[0.05] border border-white/[0.10] rounded-xl transition-all focus-within:border-accent/40 focus-within:bg-white/[0.06] mt-3">
              <textarea
                ref={textareaRef}
                value={chatInput}
                onChange={(e) => setChatInput(e.target.value)}
                onKeyDown={handleKeyDown}
                placeholder="Ask about the codebase..."
                rows={1}
                className="flex-1 bg-transparent border-none outline-none text-[13px] text-text-primary placeholder:text-text-muted resize-none min-h-[36px] scrollbar-thin"
                style={{ height: '36px', overflowY: 'hidden' }}
              />
              {chatMessages.length > 0 && (
                <button
                  onClick={clearChat}
                  className="w-7 h-7 flex items-center justify-center text-text-muted hover:text-text-secondary transition-colors rounded-md hover:bg-white/[0.06]"
                  title="Clear chat"
                >
                  <Trash2 className="w-3.5 h-3.5" />
                </button>
              )}
              {isChatLoading ? (
                <button
                  onClick={stopChatResponse}
                  className="w-8 h-8 flex items-center justify-center bg-[#FF453A]/80 rounded-lg text-white transition-all hover:bg-[#FF453A] shrink-0"
                  title="Stop response"
                >
                  <Square className="w-3 h-3 fill-current" />
                </button>
              ) : (
                <button
                  onClick={handleSendMessage}
                  disabled={!chatInput.trim() || isAgentInitializing}
                  className="w-8 h-8 flex items-center justify-center bg-accent rounded-lg text-white transition-all hover:bg-accent-dim disabled:opacity-30 disabled:cursor-not-allowed shrink-0"
                >
                  <Send className="w-3.5 h-3.5" />
                </button>
              )}
            </div>
            {!isAgentReady && !isAgentInitializing && (
              <div className="mt-2 text-[11px] text-[#FF9F0A] flex items-center gap-1.5">
                <AlertTriangle className="w-3 h-3" />
                <span>
                  {isProviderConfigured()
                    ? 'Initializing AI agent...'
                    : 'Configure an LLM provider to enable chat.'}
                </span>
              </div>
            )}
          </div>
        </div>
      )}
    </aside>
  );
};
