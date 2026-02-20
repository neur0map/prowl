import { useState, useEffect, useRef } from 'react';
import {
  ArrowRight, Copy, Trash2, Plus, Lightbulb,
  ChevronDown, ChevronUp, BookOpen, Hash, Sparkles
} from 'lucide-react';
import { useAppState } from '../hooks/useAppState';
import type { ChatMessage } from '../core/llm/types';

interface ContextItem {
  id: string;
  type: 'question' | 'answer' | 'code';
  title: string;
  content: string;
  timestamp: number;
  sourceChatId: string;
  sourceIndex: number;
}

interface ContextGroup {
  id: string;
  name: string;
  items: ContextItem[];
  createdAt: number;
}

export const ContextBridge = () => {
  const {
    chatMessages,
    sendChatMessage,
  } = useAppState();

  const [isExpanded, setIsExpanded] = useState(false);
  const [groups, setGroups] = useState<ContextGroup[]>([]);
  const [selectedItems, setSelectedItems] = useState<Set<string>>(new Set());
  const [newGroup, setNewGroup] = useState<ContextGroup | null>(null);

  // Load saved groups from localStorage
  useEffect(() => {
    try {
      const saved = localStorage.getItem('prowl-context-groups');
      if (saved) {
        setGroups(JSON.parse(saved));
      }
    } catch (error) {
      console.error('Failed to load context groups:', error);
    }
  }, []);

  // Save groups to localStorage
  useEffect(() => {
    if (groups.length > 0) {
      localStorage.setItem('prowl-context-groups', JSON.stringify(groups));
    }
  }, [groups]);

  // Extract context from current chat
  const extractFromChat = (chatId: string, message: ChatMessage, index: number) => {
    const timestamp = Date.now();
    const items: ContextItem[] = [];

    if (message.role === 'user') {
      items.push({
        id: `${chatId}-${index}-question`,
        type: 'question',
        title: message.content.slice(0, 50) + '...',
        content: message.content,
        timestamp,
        sourceChatId: chatId,
        sourceIndex: index,
      });
    }

    if (message.role === 'assistant' && message.content) {
      items.push({
        id: `${chatId}-${index}-answer`,
        type: 'answer',
        title: 'AI Response',
        content: message.content,
        timestamp,
        sourceChatId: chatId,
        sourceIndex: index,
      });

      // Extract code references if available
      if (message.toolCalls && message.toolCalls.length > 0) {
        items.push({
          id: `${chatId}-${index}-code`,
          type: 'code',
          title: `Code Analysis (${message.toolCalls.length} tools)`,
          content: JSON.stringify(message.toolCalls, null, 2),
          timestamp,
          sourceChatId: chatId,
          sourceIndex: index,
        });
      }
    }

    return items;
  };

  // Create a new group from current chat
  const createGroupFromChat = () => {
    const chatId = `chat-${Date.now()}`;
    const allItems: ContextItem[] = [];

    chatMessages.forEach((message, index) => {
      const items = extractFromChat(chatId, message, index);
      allItems.push(...items);
    });

    if (allItems.length === 0) {
      alert('No content to extract from current chat.');
      return;
    }

    const group: ContextGroup = {
      id: `group-${Date.now()}`,
      name: newGroup?.name || `Context ${groups.length + 1}`,
      items: allItems,
      createdAt: Date.now(),
    };

    setGroups(prev => [...prev, group]);
    setNewGroup(null);
  };

  // Inject selected context as a new message
  const injectContext = () => {
    if (selectedItems.size === 0) {
      alert('Please select at least one context item to inject.');
      return;
    }

    // Find all selected items across all groups
    const selectedContext: ContextItem[] = [];
    groups.forEach(group => {
      group.items.forEach(item => {
        if (selectedItems.has(item.id)) {
          selectedContext.push(item);
        }
      });
    });

    if (selectedContext.length === 0) return;

    // Build a nicely formatted context message
    const contextText = selectedContext.map(item => {
      const prefix = item.type === 'question' ? 'Q:' : item.type === 'answer' ? 'A:' : 'Code:';
      return `${prefix} ${item.content.slice(0, 200)}${item.content.length > 200 ? '...' : ''}`;
    }).join('\n\n');

    const fullMessage = `Here is some context from previous conversations:\n\n${contextText}\n\n---\n\nPlease use this context to help answer my next question.`;

    // Send as a system-like message
    sendChatMessage(fullMessage);

    // Clear selection after injection
    setSelectedItems(new Set());
    setIsExpanded(false);
  };

  const toggleItem = (itemId: string) => {
    setSelectedItems(prev => {
      const next = new Set(prev);
      if (next.has(itemId)) {
        next.delete(itemId);
      } else {
        next.add(itemId);
      }
      return next;
    });
  };

  const toggleGroupItems = (groupId: string) => {
    const group = groups.find(g => g.id === groupId);
    if (!group) return;

    const allGroupIds = new Set(group.items.map(i => i.id));
    const allSelected = allGroupIds.size > 0 && Array.from(selectedItems).every(id => allGroupIds.has(id));

    if (allSelected) {
      setSelectedItems(prev => {
        const next = new Set(prev);
        allGroupIds.forEach(id => next.delete(id));
        return next;
      });
    } else {
      setSelectedItems(prev => new Set([...prev, ...allGroupIds]));
    }
  };

  const deleteGroup = (groupId: string) => {
    setGroups(prev => prev.filter(g => g.id !== groupId));
  };

  const deleteItem = (groupId: string, itemId: string) => {
    setGroups(prev => prev.map(group => {
      if (group.id === groupId) {
        return {
          ...group,
          items: group.items.filter(i => i.id !== itemId),
        };
      }
      return group;
    }));
  };

  const itemCount = selectedItems.size;
  const allItems = groups.reduce((acc, g) => acc + g.items.length, 0);

  return (
    <div className="border-t border-white/[0.08] bg-void/30">
      {/* Toggle Header */}
      <button
        onClick={() => setIsExpanded(!isExpanded)}
        className="w-full flex items-center justify-between px-4 py-2 hover:bg-white/[0.04] transition-colors"
      >
        <div className="flex items-center gap-2">
          <Lightbulb className="w-4 h-4 text-accent" />
          <span className="text-[12px] font-medium text-text-primary">
            Context Bridge
          </span>
          {itemCount > 0 && (
            <span className="px-1.5 py-0.5 rounded bg-accent/20 text-[10px] text-accent font-medium">
              {itemCount} selected
            </span>
          )}
        </div>
        <div className="flex items-center gap-2">
          {allItems > 0 && (
            <span className="text-[11px] text-text-muted">
              {allItems} saved items
            </span>
          )}
          {isExpanded ? (
            <ChevronUp className="w-4 h-4 text-text-muted/60" />
          ) : (
            <ChevronDown className="w-4 h-4 text-text-muted/60" />
          )}
        </div>
      </button>

      {/* Expanded Content */}
      {isExpanded && (
        <div className="px-4 pb-4 space-y-3">
          {/* Actions */}
          <div className="flex items-center gap-2">
            <button
              onClick={() => setNewGroup({ id: '', name: `Context ${groups.length + 1}`, items: [], createdAt: Date.now() })}
              className="flex items-center gap-1.5 px-2.5 py-1.5 bg-accent/10 border border-accent/30 rounded-lg text-[11px] text-accent hover:bg-accent/20 transition-colors"
            >
              <Plus className="w-3.5 h-3.5" />
              New Group
            </button>
            {itemCount > 0 && (
              <button
                onClick={injectContext}
                className="flex items-center gap-1.5 px-2.5 py-1.5 bg-white/[0.06] border border-white/[0.10] rounded-lg text-[11px] text-text-primary hover:bg-white/[0.10] transition-colors"
              >
                <ArrowRight className="w-3.5 h-3.5" />
                Inject Context
              </button>
            )}
            {itemCount > 0 && (
              <button
                onClick={() => setSelectedItems(new Set())}
                className="flex items-center gap-1.5 px-2.5 py-1.5 text-[11px] text-text-muted hover:text-text-secondary transition-colors"
              >
                Clear Selection
              </button>
            )}
          </div>

          {/* New Group Input */}
          {newGroup && (
            <div className="p-3 bg-white/[0.03] border border-white/[0.08] rounded-lg space-y-2">
              <input
                type="text"
                value={newGroup.name}
                onChange={(e) => setNewGroup(prev => prev ? { ...prev, name: e.target.value } : null)}
                placeholder="Group name..."
                className="w-full px-3 py-1.5 bg-transparent border border-white/[0.10] rounded text-[12px] text-text-primary outline-none focus:border-accent/50"
              />
              <div className="flex items-center gap-2">
                <button
                  onClick={() => {
                    if (newGroup?.name.trim()) {
                      setGroups(prev => [...prev, { ...newGroup!, id: `group-${Date.now()}` }]);
                      setNewGroup(null);
                    }
                  }}
                  className="flex-1 px-3 py-1.5 bg-accent/10 border border-accent/30 rounded-lg text-[11px] text-accent hover:bg-accent/20 transition-colors"
                >
                  Save Group
                </button>
                <button
                  onClick={() => setNewGroup(null)}
                  className="px-3 py-1.5 text-[11px] text-text-muted hover:text-text-secondary transition-colors"
                >
                  Cancel
                </button>
              </div>
            </div>
          )}

          {/* Current Chat Quick Action */}
          {chatMessages.length > 0 && (
            <div className="p-3 bg-accent/5 border border-accent/10 rounded-lg">
              <p className="text-[11px] text-text-muted mb-2">
                Extract context from current chat ({chatMessages.length} messages)
              </p>
              <button
                onClick={createGroupFromChat}
                className="w-full flex items-center justify-center gap-1.5 px-3 py-1.5 bg-accent/10 border border-accent/30 rounded-lg text-[11px] text-accent hover:bg-accent/20 transition-colors"
              >
                <Sparkles className="w-3.5 h-3.5" />
                Extract from Current Chat
              </button>
            </div>
          )}

          {/* Saved Groups */}
          {groups.length > 0 ? (
            <div className="space-y-2">
              {groups.map(group => (
                <div
                  key={group.id}
                  className="p-3 bg-white/[0.02] border border-white/[0.06] rounded-lg space-y-2"
                >
                  <div className="flex items-center justify-between">
                    <button
                      onClick={() => toggleGroupItems(group.id)}
                      className="flex items-center gap-2 flex-1 min-w-0"
                    >
                      <BookOpen className="w-3.5 h-3.5 text-text-muted/60 flex-shrink-0" />
                      <span className="text-[12px] font-medium text-text-primary truncate">
                        {group.name}
                      </span>
                      <span className="text-[10px] text-text-muted whitespace-nowrap">
                        ({group.items.length})
                      </span>
                    </button>
                    <button
                      onClick={() => deleteGroup(group.id)}
                      className="p-1 text-text-muted/40 hover:text-text-muted hover:bg-white/[0.08] rounded transition-colors"
                      title="Delete group"
                    >
                      <Trash2 className="w-3 h-3" />
                    </button>
                  </div>

                  <div className="space-y-1.5">
                    {group.items.map(item => {
                      const isSelected = selectedItems.has(item.id);
                      return (
                        <div
                          key={item.id}
                          className={`flex items-start gap-2 p-2 rounded border transition-colors ${
                            isSelected
                              ? 'bg-accent/10 border-accent/30'
                              : 'bg-white/[0.02] border-white/[0.04] hover:border-white/[0.08]'
                          }`}
                        >
                          <button
                            onClick={() => toggleItem(item.id)}
                            className="mt-0.5 flex-shrink-0"
                          >
                            <div className={`w-3.5 h-3.5 rounded border flex items-center justify-center ${
                              isSelected
                                ? 'bg-accent border-accent'
                                : 'border-white/[0.20]'
                            }`}>
                              {isSelected && (
                                <svg className="w-2 h-2 text-white" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                                  <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={3} d="M5 13l4 4L19 7" />
                                </svg>
                              )}
                            </div>
                          </button>
                          <div className="flex-1 min-w-0">
                            <div className="flex items-center gap-2 mb-0.5">
                              {item.type === 'question' && (
                                <span className="px-1.5 py-0.5 rounded bg-blue-500/10 text-blue-400 text-[9px] font-medium">
                                  Question
                                </span>
                              )}
                              {item.type === 'answer' && (
                                <span className="px-1.5 py-0.5 rounded bg-green-500/10 text-green-400 text-[9px] font-medium">
                                  Answer
                                </span>
                              )}
                              {item.type === 'code' && (
                                <span className="px-1.5 py-0.5 rounded bg-purple-500/10 text-purple-400 text-[9px] font-medium">
                                  Code
                                </span>
                              )}
                            </div>
                            <p className="text-[11px] text-text-secondary truncate">
                              {item.title}
                            </p>
                          </div>
                          <div className="flex items-center gap-1">
                            <button
                              onClick={() => {
                                navigator.clipboard.writeText(item.content);
                              }}
                              className="p-1 text-text-muted/40 hover:text-text-muted transition-colors"
                              title="Copy"
                            >
                              <Copy className="w-3 h-3" />
                            </button>
                            <button
                              onClick={() => deleteItem(group.id, item.id)}
                              className="p-1 text-text-muted/40 hover:text-text-muted transition-colors"
                              title="Delete"
                            >
                              <Trash2 className="w-3 h-3" />
                            </button>
                          </div>
                        </div>
                      );
                    })}
                  </div>
                </div>
              ))}
            </div>
          ) : (
            <div className="text-center py-6">
              <Hash className="w-8 h-8 mx-auto mb-2 text-text-muted/30" />
              <p className="text-[12px] text-text-muted">
                No saved context groups yet
              </p>
              <p className="text-[11px] text-text-muted/60 mt-1">
                Extract context from your chats to reuse it later
              </p>
            </div>
          )}
        </div>
      )}
    </div>
  );
};
