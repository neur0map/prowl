import { useState, useRef, useEffect, useCallback } from 'react';
import {
  Send, Square, Loader2,
  AlertTriangle, Radio, Trash2,
  ChevronDown, ChevronRight, Wrench, X, File, Folder,
  History, Plus
} from 'lucide-react';
import { useAppState } from '../hooks/useAppState';
import { useAtMention } from '../hooks/useAtMention';
import { ToolCallPill } from './ToolCallPill';
import { isProviderConfigured, getActiveProviderDisplay } from '../core/llm/settings-service';
import { MarkdownRenderer } from './MarkdownRenderer';
import { AgentPanel } from './AgentPanel';
import { ContextBridge } from './ContextBridge';
import { ContextBridgeIndicator } from './ContextBridge';
import { TerminalDrawer } from './TerminalDrawer';
import { MentionDropdown } from './MentionDropdown';
import { TokenCounter } from './TokenCounter';

const isElectron = typeof window !== 'undefined' && !!(window as any).prowl;

/* ── Three-dot pulse loader for streaming assistant messages ── */
const ThreeDotPulse = () => (
  <div className="flex items-center gap-1 py-1">
    {[0, 1, 2].map(i => (
      <span
        key={i}
        className="w-1.5 h-1.5 rounded-full bg-accent/60"
        style={{
          animation: 'pulse 1.2s ease-in-out infinite',
          animationDelay: `${i * 0.2}s`,
        }}
      />
    ))}
  </div>
);

/* ── Tool call collapsible group (unchanged logic) ── */
const ToolCallGroup = ({ steps }: { steps: { id: string; toolCall: any }[] }) => {
  const [expanded, setExpanded] = useState(false);
  const allDone = steps.every(s => s.toolCall?.status === 'completed' || s.toolCall?.status === 'error');
  const runningCount = steps.filter(s => s.toolCall?.status === 'running').length;
  const MAX_VISIBLE = 3;

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

export type RightPanelTab = 'chat' | 'agent' | 'terminal';

interface RightPanelProps {
  onFocusNode: (nodeId: string) => void;
  activeTab?: RightPanelTab;
  onTabChange?: (tab: RightPanelTab) => void;
  terminalCwd?: string;
}

export const RightPanel = ({ onFocusNode, activeTab: controlledTab, onTabChange, terminalCwd }: RightPanelProps) => {
  const {
    isRightPanelOpen,
    setRightPanelOpen,
    fileContents,
    graph,
    chatMessages,
    isChatLoading,
    currentToolCalls,
    agentError,
    isAgentReady,
    isAgentInitializing,
    sendChatMessage,
    stopChatResponse,
    clearChat,
    addCodeReference,
    conversations,
    loadConversation,
    startNewConversation,
    isCompacting,
  } = useAppState();

  const [chatInput, setChatInput] = useState('');
  const [internalTab, setInternalTab] = useState<RightPanelTab>('chat');
  const activeTab = controlledTab ?? internalTab;
  const setActiveTab = onTabChange ?? setInternalTab;
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const messagesEndRef = useRef<HTMLDivElement>(null);

  const [panelWidth, setPanelWidth] = useState(35);
  const isResizingRef = useRef(false);

  // MCP panel expand state
  const [isMcpExpanded, setIsMcpExpanded] = useState(false);

  // @ mention hook
  const atMention = useAtMention();

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

  useEffect(() => {
    if (messagesEndRef.current) {
      messagesEndRef.current.scrollIntoView({ behavior: 'smooth' });
    }
  }, [chatMessages, isChatLoading]);

  const resolveFilePathForUI = useCallback((requestedPath: string): string | null => {
    const req = requestedPath.replace(/\\/g, '/').replace(/^\.?\//, '').toLowerCase();
    if (!req) return null;

    for (const key of fileContents.keys()) {
      const norm = key.replace(/\\/g, '/').replace(/^\.?\//, '').toLowerCase();
      if (norm === req) return key;
    }

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

  const findFileNodeForUI = useCallback((filePath: string) => {
    if (!graph) return undefined;
    const target = filePath.replace(/\\/g, '/').replace(/^\.?\//, '');
    return graph.nodes.find(
      (n) => n.label === 'File' && n.properties.filePath.replace(/\\/g, '/').replace(/^\.?\//, '') === target
    );
  }, [graph]);

  const handleCitationClick = useCallback((inner: string) => {
    const raw = inner.trim();
    if (!raw) return;

    let rawPath = raw;
    let startLine1: number | undefined;
    let endLine1: number | undefined;

    const lineMatch = raw.match(/^(.*):(\d+)(?:[-–](\d+))?$/);
    if (lineMatch) {
      rawPath = lineMatch[1].trim();
      startLine1 = parseInt(lineMatch[2], 10);
      endLine1 = lineMatch[3] ? parseInt(lineMatch[3], 10) : startLine1;
    }

    const resolvedPath = resolveFilePathForUI(rawPath);
    if (!resolvedPath) return;

    const node = findFileNodeForUI(resolvedPath);
    if (node?.id) {
      onFocusNode(node.id);
    }

    // Open code panel with this file (source: 'user' triggers auto-open)
    const startLine0 = startLine1 !== undefined ? Math.max(0, startLine1 - 1) : undefined;
    const endLine0 = endLine1 !== undefined ? Math.max(0, endLine1 - 1) : startLine0;
    addCodeReference({
      filePath: resolvedPath,
      startLine: startLine0,
      endLine: endLine0,
      nodeId: node?.id,
      label: 'File',
      name: resolvedPath.split('/').pop() ?? resolvedPath,
      source: 'user',
    });
  }, [findFileNodeForUI, resolveFilePathForUI, onFocusNode, addCodeReference]);

  const handleNodeCitationClick = useCallback((nodeTypeAndName: string) => {
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

    onFocusNode(node.id);

    // Open code panel with this symbol (source: 'user' triggers auto-open)
    if (node.properties.filePath) {
      const resolvedPath = resolveFilePathForUI(node.properties.filePath);
      if (resolvedPath) {
        addCodeReference({
          filePath: resolvedPath,
          startLine: node.properties.startLine ? node.properties.startLine - 1 : undefined,
          endLine: node.properties.endLine ? node.properties.endLine - 1 : undefined,
          nodeId: node.id,
          label: node.label,
          name: node.properties.name,
          source: 'user',
        });
      }
    }
  }, [graph, onFocusNode, resolveFilePathForUI, addCodeReference]);

  const handleLinkClick = useCallback((href: string) => {
    if (href.startsWith('code-ref:')) {
      const inner = decodeURIComponent(href.slice('code-ref:'.length));
      handleCitationClick(inner);
    } else if (href.startsWith('node-ref:')) {
      const inner = decodeURIComponent(href.slice('node-ref:'.length));
      handleNodeCitationClick(inner);
    }
  }, [handleCitationClick, handleNodeCitationClick]);

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
    if (!chatInput.trim() && atMention.taggedFiles.length === 0) return;
    const contextBlock = atMention.buildContextBlock();
    const userText = chatInput.trim();
    const fullMessage = contextBlock
      ? `${contextBlock}\n\n${userText}`
      : userText;

    setChatInput('');
    atMention.clearTags();
    if (textareaRef.current) {
      textareaRef.current.style.height = '36px';
      textareaRef.current.style.overflowY = 'hidden';
    }
    await sendChatMessage(fullMessage);
  };

  const handleKeyDown = (e: React.KeyboardEvent) => {
    // If mention dropdown is open, intercept arrow keys / enter / escape
    if (atMention.isMentionOpen) {
      if (e.key === 'ArrowDown') {
        e.preventDefault();
        atMention.setMentionIndex(
          Math.min(atMention.mentionIndex + 1, atMention.filteredCandidates.length - 1)
        );
        return;
      }
      if (e.key === 'ArrowUp') {
        e.preventDefault();
        atMention.setMentionIndex(Math.max(atMention.mentionIndex - 1, 0));
        return;
      }
      if (e.key === 'Enter') {
        e.preventDefault();
        const candidate = atMention.filteredCandidates[atMention.mentionIndex];
        if (candidate) {
          const result = atMention.selectCandidate(candidate);
          if (result !== null) {
            // Strip the @query from chatInput
            const anchor = atMention.mentionAnchorRef.current;
            const before = chatInput.slice(0, anchor);
            const cursorPos = textareaRef.current?.selectionStart ?? chatInput.length;
            const after = chatInput.slice(cursorPos);
            setChatInput(before + after);
          }
        }
        return;
      }
      if (e.key === 'Escape') {
        e.preventDefault();
        atMention.setIsMentionOpen(false);
        return;
      }
    }

    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      handleSendMessage();
    }
  };

  const handleInputChange = (e: React.ChangeEvent<HTMLTextAreaElement>) => {
    const val = e.target.value;
    setChatInput(val);
    const cursorPos = e.target.selectionStart ?? val.length;
    atMention.handleInputChange(val, cursorPos);
  };

  const chatSuggestions = [
    'Explain the project architecture',
    'What does this project do?',
    'Show me the most important files',
    'Find all API handlers',
  ];

  const prowl = (window as any).prowl;
  const hasTerminal = isElectron && !!prowl?.terminal;
  const hasAgent = isElectron;

  const tokenCount = atMention.getTokenCount(chatInput);

  if (!isRightPanelOpen) return null;

  return (
    <aside
      className="flex flex-col bg-void border-l border-white/[0.06] animate-slide-in-right relative z-30 flex-shrink-0"
      style={{ width: `${panelWidth}%`, minWidth: 320 }}
    >
      <div
        onMouseDown={handleResizeStart}
        className="absolute left-0 top-0 bottom-0 w-1 cursor-col-resize z-40 hover:bg-accent/40 active:bg-accent/60 transition-colors"
      />

      {/* Header */}
      <div className="flex items-center h-11 px-3 border-b border-white/[0.06] shrink-0">
        <div className="flex items-center gap-0.5">
          <button
            onClick={() => setActiveTab('chat')}
            className={`px-2.5 py-1 text-[12px] font-mono rounded transition-colors ${
              activeTab === 'chat'
                ? 'text-text-primary bg-white/[0.06]'
                : 'text-text-muted hover:text-text-secondary'
            }`}
          >
            chat
          </button>
          {hasTerminal && (
            <button
              onClick={() => setActiveTab('terminal')}
              className={`px-2.5 py-1 text-[12px] font-mono rounded transition-colors ${
                activeTab === 'terminal'
                  ? 'text-text-primary bg-white/[0.06]'
                  : 'text-text-muted hover:text-text-secondary'
              }`}
            >
              term
            </button>
          )}
        </div>

        <div className="ml-auto flex items-center gap-1">
          {hasAgent && (
            <button
              onClick={() => setActiveTab('agent')}
              className={`p-1.5 rounded transition-colors ${
                activeTab === 'agent'
                  ? 'text-accent bg-accent/10'
                  : 'text-text-muted hover:text-text-secondary'
              }`}
              title="Agent"
            >
              <Radio className="w-4 h-4" />
            </button>
          )}
          <button
            onClick={() => setRightPanelOpen(false)}
            className="p-1.5 text-text-muted hover:text-text-primary rounded transition-colors"
            title="Close"
          >
            <ChevronRight className="w-4 h-4" />
          </button>
        </div>
      </div>

      {activeTab === 'agent' && (
        <div className="flex-1 flex flex-col overflow-hidden">
          <AgentPanel />
        </div>
      )}

      {/* Terminal — keep mounted (hidden) so xterm state is preserved */}
      {prowl?.terminal && (
        <div className={`flex-1 flex flex-col min-h-0 overflow-hidden ${activeTab === 'terminal' ? '' : 'hidden'}`}>
          <TerminalDrawer
            isOpen={activeTab === 'terminal'}
            onToggle={() => setActiveTab('chat')}
            cwd={terminalCwd}
            embedded
          />
        </div>
      )}

      {activeTab === 'chat' && (
        <div className="flex-1 flex flex-col overflow-hidden">

          {/* Compaction indicator */}
          {isCompacting && (
            <div className="px-4 py-1.5 bg-accent/5 border-b border-accent/10 text-accent/70 text-[11px] font-mono flex items-center gap-2">
              <Loader2 className="w-3 h-3 animate-spin" />
              <span>compacting context...</span>
            </div>
          )}

          {agentError && (
            <div className="px-4 py-2.5 bg-[#FF453A]/10 border-b border-[#FF453A]/20 text-[#FF453A] text-[12px] flex items-center gap-2">
              <AlertTriangle className="w-3.5 h-3.5" />
              <span>{agentError}</span>
            </div>
          )}

          {/* Messages area */}
          <div className="flex-1 overflow-y-auto p-4 scrollbar-thin">
            {chatMessages.length === 0 ? (
              /* ── Empty state: bottom-anchored, terminal-style suggestions ── */
              <div className="flex-1 flex flex-col justify-end h-full px-0 pb-3">
                {/* Previous conversations */}
                {conversations.length > 0 && (
                  <div className="mb-4">
                    <p className="text-[11px] text-text-muted/40 font-mono mb-1.5 flex items-center gap-1.5">
                      <History className="w-3 h-3" />
                      recent conversations
                    </p>
                    {conversations.slice(0, 5).map(conv => (
                      <button
                        key={conv.id}
                        onClick={() => loadConversation(conv.id)}
                        className="block w-full text-left px-2.5 py-1.5 text-[11px] font-mono text-text-muted/60 hover:text-text-primary hover:bg-white/[0.04] rounded transition-colors truncate"
                        title={conv.title}
                      >
                        <span className="text-accent/30 mr-1.5">~</span>
                        {conv.title}
                        <span className="text-text-muted/30 ml-2">
                          {conv.messages.length} msgs
                        </span>
                      </button>
                    ))}
                  </div>
                )}

                <p className="text-[12px] text-text-muted/50 font-mono mb-2">try asking:</p>
                {chatSuggestions.map(s => (
                  <button
                    key={s}
                    onClick={() => setChatInput(s)}
                    className="block text-left px-2.5 py-1.5 text-[12px] font-mono text-text-muted hover:text-text-primary hover:bg-white/[0.04] rounded transition-colors"
                  >
                    <span className="text-accent/40 mr-1.5">&gt;</span>{s}
                  </button>
                ))}
              </div>
            ) : (
              /* ── Messages list ── */
              <div className="flex flex-col gap-3">
                {chatMessages.map((message) => (
                  <div key={message.id} className="animate-fade-in">
                    {/* User message — right-aligned, accent-tinted, no avatar/label */}
                    {message.role === 'user' && (
                      <div className="flex justify-end">
                        <div className="max-w-[85%] px-3 py-2 rounded-lg bg-accent/10 border border-accent/15 text-[13px] text-text-primary">
                          {message.content}
                        </div>
                      </div>
                    )}

                    {/* Assistant message — left accent border, no avatar/label */}
                    {message.role === 'assistant' && (
                      <div className="pl-3 border-l-2 border-accent/20">
                        {isChatLoading && message === chatMessages[chatMessages.length - 1] && (
                          (!message.steps || message.steps.length === 0) && !message.content && (
                            <ThreeDotPulse />
                          )
                        )}
                        <div className="chat-prose">
                          {message.steps && message.steps.length > 0 ? (
                            <div className="space-y-2">
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
                    )}
                  </div>
                ))}
              </div>
            )}
            <div ref={messagesEndRef} />
          </div>

          {/* ── Input area ── */}
          <div className="p-3 border-t border-white/[0.08] bg-deep/80">
            {/* MCP: collapsed indicator or full panel */}
            {isMcpExpanded ? (
              <ContextBridge />
            ) : (
              <ContextBridgeIndicator onExpand={() => setIsMcpExpanded(true)} />
            )}

            {/* Context tags row */}
            {atMention.taggedFiles.length > 0 && (
              <div className="flex flex-wrap gap-1.5 mt-2 mb-1">
                {atMention.taggedFiles.map(tag => (
                  <span
                    key={tag.id}
                    className="inline-flex items-center gap-1 px-2 py-0.5 bg-white/[0.06] border border-white/[0.1] rounded text-[11px] text-text-secondary font-mono"
                  >
                    {tag.label === 'Folder' ? (
                      <Folder className="w-3 h-3 text-text-muted/60" />
                    ) : (
                      <File className="w-3 h-3 text-text-muted/60" />
                    )}
                    <span className="truncate max-w-[120px]">{tag.name}</span>
                    <span className="text-text-muted/40">
                      ~{tag.tokenEstimate >= 1000 ? `${(tag.tokenEstimate / 1000).toFixed(1)}k` : tag.tokenEstimate}t
                    </span>
                    <button
                      onClick={() => atMention.removeTag(tag.id)}
                      className="ml-0.5 text-text-muted/40 hover:text-text-primary transition-colors"
                    >
                      <X className="w-3 h-3" />
                    </button>
                  </span>
                ))}
              </div>
            )}

            {/* Input box with mention dropdown */}
            <div className="relative mt-2">
              {atMention.isMentionOpen && (
                <MentionDropdown
                  candidates={atMention.filteredCandidates}
                  selectedIndex={atMention.mentionIndex}
                  onSelect={(candidate) => {
                    const result = atMention.selectCandidate(candidate);
                    if (result !== null) {
                      const anchor = atMention.mentionAnchorRef.current;
                      const cursorPos = textareaRef.current?.selectionStart ?? chatInput.length;
                      const before = chatInput.slice(0, anchor);
                      const after = chatInput.slice(cursorPos);
                      setChatInput(before + after);
                    }
                    textareaRef.current?.focus();
                  }}
                  onClose={() => atMention.setIsMentionOpen(false)}
                />
              )}

              <div className="flex items-end gap-2 px-3 py-1.5 bg-white/[0.04] border border-white/[0.08] rounded-lg transition-all focus-within:border-accent/40 focus-within:bg-white/[0.06]">
                <textarea
                  ref={textareaRef}
                  value={chatInput}
                  onChange={handleInputChange}
                  onKeyDown={handleKeyDown}
                  placeholder="Ask about this codebase... (@ to attach files)"
                  rows={1}
                  className="flex-1 bg-transparent border-none outline-none text-[13px] font-mono text-text-primary placeholder:text-text-muted resize-none min-h-[36px] scrollbar-thin"
                  style={{ height: '36px', overflowY: 'hidden' }}
                />
                {chatMessages.length > 0 && (
                  <button
                    onClick={clearChat}
                    className="w-7 h-7 flex items-center justify-center text-text-muted hover:text-text-secondary transition-colors rounded hover:bg-white/[0.06]"
                    title="Clear chat"
                  >
                    <Trash2 className="w-3.5 h-3.5" />
                  </button>
                )}
                {isChatLoading ? (
                  <button
                    onClick={stopChatResponse}
                    className="w-7 h-7 flex items-center justify-center bg-[#FF453A]/80 rounded text-white transition-all hover:bg-[#FF453A] shrink-0"
                    title="Stop response"
                  >
                    <Square className="w-3 h-3 fill-current" />
                  </button>
                ) : (
                  <button
                    onClick={handleSendMessage}
                    disabled={(!chatInput.trim() && atMention.taggedFiles.length === 0) || isAgentInitializing}
                    className="w-7 h-7 flex items-center justify-center bg-accent rounded text-white transition-all hover:bg-accent-dim disabled:opacity-30 disabled:cursor-not-allowed shrink-0"
                  >
                    <Send className="w-3.5 h-3.5" />
                  </button>
                )}
              </div>
            </div>

            {/* Token counter + model indicator + provider warning row */}
            <div className="flex items-center mt-1.5 min-h-[18px]">
              <TokenCounter tokenCount={tokenCount} />
              {isAgentReady && (() => {
                const display = getActiveProviderDisplay();
                if (!display) return null;
                return (
                  <span className="ml-auto flex items-center gap-1.5 text-[11px] font-mono text-text-muted/50 truncate max-w-[200px]" title={`${display.provider}: ${display.model}`}>
                    {isChatLoading && <Loader2 className="w-3 h-3 animate-spin text-accent/60 shrink-0" />}
                    {display.model}
                  </span>
                );
              })()}
              {!isAgentReady && !isAgentInitializing && (
                <div className="ml-auto text-[11px] text-[#FF9F0A] flex items-center gap-1">
                  <AlertTriangle className="w-3 h-3" />
                  <span>
                    {isProviderConfigured()
                      ? 'Initializing...'
                      : 'Set up AI provider'}
                  </span>
                </div>
              )}
              {isAgentInitializing && (
                <span className="ml-auto flex items-center gap-1.5 text-[11px] font-mono text-text-muted/40">
                  <Loader2 className="w-3 h-3 animate-spin shrink-0" />
                  connecting...
                </span>
              )}
            </div>
          </div>
        </div>
      )}
    </aside>
  );
};
