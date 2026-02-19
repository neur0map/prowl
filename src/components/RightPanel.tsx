import { useState, useRef, useEffect, useCallback } from 'react';
import {
  Send, Square, MessageSquare, User,
  PanelRightClose, Loader2, AlertTriangle, GitBranch, Radio
} from 'lucide-react';
import { useAppState } from '../hooks/useAppState';
import { ToolCallCard } from './ToolCallCard';
import { isProviderConfigured } from '../core/llm/settings-service';
import { MarkdownRenderer } from './MarkdownRenderer';
import { ProcessesPanel } from './ProcessesPanel';
import { AgentPanel } from './AgentPanel';

const isElectron = typeof window !== 'undefined' && !!(window as any).prowl;

export const RightPanel = () => {
  const {
    isRightPanelOpen,
    setRightPanelOpen,
    fileContents,
    graph,
    addCodeReference,
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

  const findFileNodeIdForUI = useCallback((filePath: string): string | undefined => {
    if (!graph) return undefined;
    const target = filePath.replace(/\\/g, '/').replace(/^\.?\//, '');
    const node = graph.nodes.find(
      (n) => n.label === 'File' && n.properties.filePath.replace(/\\/g, '/').replace(/^\.?\//, '') === target
    );
    return node?.id;
  }, [graph]);

  const handleGroundingClick = useCallback((inner: string) => {
    const raw = inner.trim();
    if (!raw) return;

    let rawPath = raw;
    let startLine1: number | undefined;
    let endLine1: number | undefined;

    // Match line:num or line:num-num (supports both hyphen - and en dash –)
    const lineMatch = raw.match(/^(.*):(\d+)(?:[-–](\d+))?$/);
    if (lineMatch) {
      rawPath = lineMatch[1].trim();
      startLine1 = parseInt(lineMatch[2], 10);
      endLine1 = parseInt(lineMatch[3] || lineMatch[2], 10);
    }

    const resolvedPath = resolveFilePathForUI(rawPath);
    if (!resolvedPath) return;

    const nodeId = findFileNodeIdForUI(resolvedPath);

    addCodeReference({
      filePath: resolvedPath,
      startLine: startLine1 ? Math.max(0, startLine1 - 1) : undefined,
      endLine: endLine1 ? Math.max(0, endLine1 - 1) : (startLine1 ? Math.max(0, startLine1 - 1) : undefined),
      nodeId,
      label: 'File',
      name: resolvedPath.split('/').pop() ?? resolvedPath,
      source: 'ai',
    });
  }, [addCodeReference, findFileNodeIdForUI, resolveFilePathForUI]);

  // Handler for node grounding: [[Class:View]], [[Function:trigger]], etc.
  const handleNodeGroundingClick = useCallback((nodeTypeAndName: string) => {
    const raw = nodeTypeAndName.trim();
    if (!raw || !graph) return;

    // Parse Type:Name format
    const match = raw.match(/^(Class|Function|Method|Interface|File|Folder|Variable|Enum|Type|CodeElement):(.+)$/);
    if (!match) return;

    const [, nodeType, nodeName] = match;
    const trimmedName = nodeName.trim();

    // Find node in graph by type + name
    const node = graph.nodes.find(n =>
      n.label === nodeType &&
      n.properties.name === trimmedName
    );

    if (!node) {
      console.warn(`Node not found: ${nodeType}:${trimmedName}`);
      return;
    }

    // Add to Code Panel (if node has file/line info)
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
          source: 'ai',
        });
      }
    }
  }, [graph, resolveFilePathForUI, addCodeReference]);

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
    <aside className="w-[40%] min-w-[400px] max-w-[600px] flex flex-col bg-void border-l border-white/[0.08] animate-fade-in relative z-30 flex-shrink-0">
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
          {/* Status bar */}
          <div className="flex items-center gap-2.5 px-4 py-2 border-b border-white/[0.08]">
            <div className="ml-auto flex items-center gap-2">
              {!isAgentReady && (
                <span className="text-[11px] px-2 py-1 rounded-md bg-[#FF9F0A]/10 text-[#FF9F0A] border border-[#FF9F0A]/20">
                  Configure AI
                </span>
              )}
              {isAgentInitializing && (
                <span className="text-[11px] px-2 py-1 rounded-md bg-white/[0.06] border border-white/[0.1] flex items-center gap-1 text-text-muted">
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
                <div className="w-12 h-12 mb-4 flex items-center justify-center rounded-full glass">
                  <MessageSquare className="w-5 h-5 text-text-secondary" />
                </div>
                <h3 className="text-[15px] font-normal text-text-primary mb-1">
                  Ask about this codebase
                </h3>
                <p className="text-[12px] text-text-muted leading-relaxed mb-5">
                  Architecture, functions, connections — ask anything.
                </p>
                <div className="flex flex-wrap gap-2 justify-center">
                  {chatSuggestions.map((suggestion) => (
                    <button
                      key={suggestion}
                      onClick={() => setChatInput(suggestion)}
                      className="px-2.5 py-1.5 bg-white/[0.06] border border-white/[0.1] rounded-md text-[11px] text-text-secondary hover:border-white/[0.2] hover:text-text-primary transition-colors"
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
                      <div className="mb-4">
                        <div className="flex items-center gap-2 mb-2">
                          <User className="w-3.5 h-3.5 text-text-muted" />
                          <span className="text-[11px] text-text-muted uppercase tracking-wide">You</span>
                        </div>
                        <div className="pl-6 text-[13px] text-text-primary">
                          {message.content}
                        </div>
                      </div>
                    )}

                    {/* Assistant message */}
                    {message.role === 'assistant' && (
                      <div>
                        <div className="flex items-center gap-2 mb-3">
                          <MessageSquare className="w-3.5 h-3.5 text-accent" />
                          <span className="text-[11px] text-text-muted uppercase tracking-wide">Prowl</span>
                          {isChatLoading && message === chatMessages[chatMessages.length - 1] && (
                            <Loader2 className="w-3 h-3 animate-spin text-accent" />
                          )}
                        </div>
                        <div className="pl-6 chat-prose">
                          {message.steps && message.steps.length > 0 ? (
                            <div className="space-y-4">
                              {message.steps.map((step) => (
                                <div key={step.id}>
                                  {step.type === 'reasoning' && step.content && (
                                    <div className="text-text-secondary text-[13px] italic border-l-2 border-white/[0.15] pl-3 mb-3">
                                      <MarkdownRenderer
                                        content={step.content}
                                        onLinkClick={handleLinkClick}
                                      />
                                    </div>
                                  )}
                                  {step.type === 'tool_call' && step.toolCall && (
                                    <div className="mb-3">
                                      <ToolCallCard toolCall={step.toolCall} defaultExpanded={false} />
                                    </div>
                                  )}
                                  {step.type === 'content' && step.content && (
                                    <MarkdownRenderer
                                      content={step.content}
                                      onLinkClick={handleLinkClick}
                                    />
                                  )}
                                </div>
                              ))}
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

          {/* Input */}
          <div className="p-3 glass border-t border-white/[0.08]">
            <div className="flex items-end gap-2 px-3 py-2 bg-white/[0.06] border border-white/[0.12] rounded-lg transition-all focus-within:border-accent/50">
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
              <button
                onClick={clearChat}
                className="px-2 py-1 text-[11px] text-text-muted hover:text-text-primary transition-colors"
                title="Clear chat"
              >
                Clear
              </button>
              {isChatLoading ? (
                <button
                  onClick={stopChatResponse}
                  className="w-8 h-8 flex items-center justify-center bg-[#FF453A]/80 rounded-md text-white transition-all hover:bg-[#FF453A]"
                  title="Stop response"
                >
                  <Square className="w-3.5 h-3.5 fill-current" />
                </button>
              ) : (
                <button
                  onClick={handleSendMessage}
                  disabled={!chatInput.trim() || isAgentInitializing}
                  className="w-8 h-8 flex items-center justify-center bg-accent rounded-md text-white transition-all hover:bg-accent-dim disabled:opacity-40 disabled:cursor-not-allowed"
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
