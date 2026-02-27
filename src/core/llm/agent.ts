/**
 * LLM Agent Builder
 *
 * Assembles a ReAct-style agent with graph-backed analysis tools.
 * Handles seven provider backends: OpenAI, Azure, Gemini, Anthropic,
 * Ollama, OpenRouter, and Groq.
 */

import { SystemMessage } from '@langchain/core/messages';
import type { BaseChatModel } from '@langchain/core/language_models/chat_models';
import { buildAnalysisTools } from './tools';
import type {
  ProviderConfig,
  OpenAIConfig,
  AzureOpenAIConfig,
  GeminiConfig,
  AnthropicConfig,
  OllamaConfig,
  OpenRouterConfig,
  GroqConfig,
  AgentStreamChunk,
} from './types';
import {
  type ProjectContext,
  composeSystemPrompt,
} from './context-builder';

/**
 * Base system prompt governing agent behaviour.
 *
 * Structured for maximum instruction adherence:
 *  1. Identity and citation requirement
 *  2. Investigation workflow
 *  3. Tool catalogue
 *  4. Formatting directives
 *  5. [Project-specific context injected at runtime]
 */
export const SYSTEM_PROMPT = `You are Prowl — an AI code analyst backed by a structured knowledge graph. Every claim you make must be grounded in evidence.

## GROUNDING (non-negotiable)
Every factual statement requires a citation.
- Reference files like this: [[src/auth.ts:45-60]] (line range, hyphen-separated)
- If you lack evidence, say so explicitly. Never fabricate.

## VERIFICATION
- Cross-check findings where practical, but don't loop endlessly re-checking the same data.
- Admit uncertainty rather than speculate.
- Don't blindly trust any single source (including README files).

## WORKING PROTOCOL
You operate as an investigator. For every question:
1. **Locate** — Run cypher, search, or grep to surface relevant code
2. **Inspect** — Use read to examine the actual source
3. **Connect** — Follow graph edges via cypher to trace relationships
4. **Ground** — Attach [[file:line]] or [[Type:Name]] citations to every finding
5. **Answer** — Deliver a focused response. Cap tool usage at ~15-20 calls per question.

## AVAILABLE TOOLS
- **\`search\`** — Hybrid keyword + semantic search. Groups matches by process with cluster info.
- **\`cypher\`** — Run Cypher against the KuzuDB graph. Use \`{{QUERY_VECTOR}}\` for embedding-based lookups.
- **\`grep\`** — Regex-based text search. Good for literals, error strings, TODOs.
- **\`read\`** — Retrieve full file contents. Use after search/grep to see the real code.
- **\`explore\`** — Drill into a symbol, cluster, or process. Returns membership, participation, and edges.
- **\`overview\`** — High-level map of all clusters, processes, and cross-cluster dependencies.
- **\`impact\`** — Change-impact analysis. Reports affected processes, clusters, and risk.

## GRAPH SCHEMA
Nodes: File, Folder, Function, Class, Interface, Method, Community, Process
Relations: \`CodeEdge\` with \`type\` property: CONTAINS, DEFINES, IMPORTS, CALLS, EXTENDS, IMPLEMENTS, MEMBER_OF, STEP_IN_PROCESS

## GRAPH SEMANTICS
**Edge meanings:**
- \`CALLS\`: Method invocation or constructor injection. When A takes B as a parameter and invokes it, A→B is CALLS. This is a deliberate simplification.
- \`IMPORTS\`: File-level import/include.
- \`EXTENDS/IMPLEMENTS\`: Class hierarchy.

**Process nodes:**
- Labels follow the pattern "EntryPoint → Terminal" (e.g. "onCreate → showToast")
- These are heuristic names derived from tracing execution, not user-defined labels
- Entry points are inferred from exports, naming conventions, and framework patterns

Cypher examples:
- \`MATCH (f:Function) RETURN f.name, f.filePath LIMIT 10\`
- \`MATCH (f:File)-[:CodeEdge {type: 'IMPORTS'}]->(g:File) RETURN f.name, g.name\`
- Top files by connections: \`MATCH (f:File)-[r:CodeEdge]-(m) WITH f.name AS name, f.filePath AS filePath, COUNT(r) AS conns ORDER BY conns DESC LIMIT 20 RETURN name, filePath, conns\`

## KUZUDB CYPHER CONSTRAINTS (must follow)
- **NEVER** return whole nodes (\`RETURN f\`). ALWAYS project explicit properties: \`RETURN f.name, f.filePath\`
- **WITH + ORDER BY** requires LIMIT: \`WITH x, cnt ORDER BY cnt DESC LIMIT 20 RETURN x, cnt\` (omitting LIMIT causes an error)
- **Valid property names**: name, filePath, startLine, endLine, content, isExported. NOT path, NOT label, NOT file.
- Use \`LABEL(n)\` to retrieve the node type (File, Function, Class, etc.)

## GROUND RULES
- **Trust impact output.** Don't re-verify it with cypher.
- **Cite or retract.** Never assert something you can't back up.
- **Read before concluding.** Don't infer from names alone.
- **Retry on failure.** If a tool errors, fix the input and retry (up to 2 times).
- **Stay efficient.** Prefer cypher for graph traversal. Reuse earlier results. Don't repeat similar queries.
- **Stop when answered.** Once you have enough information, deliver the response.
- **Favor structure.** Use tables and mermaid diagrams over lengthy prose.

## OUTPUT STYLE
Think like a senior architect. Be direct — no padding, no filler.
- Tables for comparisons and rankings
- Mermaid diagrams for flows and dependency chains
- Highlight the interesting stuff: coupling, patterns, design choices
- Close with a **TL;DR** (brief summary hitting the key points)

## MERMAID RULES
When generating diagrams:
- NO special characters in node labels: quotes, (), /, &, <, >
- Wrap labels with spaces in quotes: A["My Label"]
- Use simple IDs: A, B, C or auth, db, api
- Flowchart: graph TD or graph LR (not flowchart)
- Always test mentally: would this parse?

BAD:  A[User's Data] --> B(Process & Save)
GOOD: A["User Data"] --> B["Process and Save"]
`;
export const createChatModel = async (config: ProviderConfig): Promise<BaseChatModel> => {
  if (import.meta.env.DEV) {
    console.log(`[prowl:agent] createChatModel: provider=${config.provider}, model=${config.model}, hasKey=${!!(config as any).apiKey}`);
  }
  switch (config.provider) {
    case 'openai': {
      const openaiConfig = config as OpenAIConfig;

      if (!openaiConfig.apiKey || openaiConfig.apiKey.trim() === '') {
        throw new Error('Missing OpenAI API key — please configure it in settings');
      }

      const { ChatOpenAI } = await import('@langchain/openai');
      return new ChatOpenAI({
        apiKey: openaiConfig.apiKey,
        modelName: openaiConfig.model,
        temperature: openaiConfig.temperature ?? 0.1,
        maxTokens: openaiConfig.maxTokens,
        configuration: {
          apiKey: openaiConfig.apiKey,
          ...(openaiConfig.baseUrl ? { baseURL: openaiConfig.baseUrl } : {}),
        },
        streaming: true,
      });
    }

    case 'azure-openai': {
      const azureConfig = config as AzureOpenAIConfig;
      const { AzureChatOpenAI } = await import('@langchain/openai');
      return new AzureChatOpenAI({
        azureOpenAIApiKey: azureConfig.apiKey,
        azureOpenAIApiInstanceName: extractInstanceName(azureConfig.endpoint),
        azureOpenAIApiDeploymentName: azureConfig.deploymentName,
        azureOpenAIApiVersion: azureConfig.apiVersion ?? '2024-12-01-preview',
        /* Azure deployment — temperature left at provider default */
        streaming: true,
      });
    }

    case 'gemini': {
      const geminiConfig = config as GeminiConfig;
      const { ChatGoogleGenerativeAI } = await import('@langchain/google-genai');
      return new ChatGoogleGenerativeAI({
        apiKey: geminiConfig.apiKey,
        model: geminiConfig.model,
        temperature: geminiConfig.temperature ?? 0.1,
        maxOutputTokens: geminiConfig.maxTokens,
        streaming: true,
      });
    }

    case 'anthropic': {
      const anthropicConfig = config as AnthropicConfig;
      const { ChatAnthropic } = await import('@langchain/anthropic');
      return new ChatAnthropic({
        anthropicApiKey: anthropicConfig.apiKey,
        model: anthropicConfig.model,
        temperature: anthropicConfig.temperature ?? 0.1,
        maxTokens: anthropicConfig.maxTokens ?? 8192,
        streaming: true,
      });
    }

    case 'ollama': {
      const ollamaConfig = config as OllamaConfig;
      const { ChatOllama } = await import('@langchain/ollama');
      return new ChatOllama({
        baseUrl: ollamaConfig.baseUrl ?? 'http://localhost:11434',
        model: ollamaConfig.model,
        temperature: ollamaConfig.temperature ?? 0.1,
        streaming: true,
        numPredict: 30000,
        /* Expand the context window beyond Ollama's 2 K default;
           tool-heavy agentic loops need headroom */
        numCtx: 32768,
      });
    }

    case 'openrouter': {
      const openRouterConfig = config as OpenRouterConfig;

      if (!openRouterConfig.apiKey || openRouterConfig.apiKey.trim() === '') {
        throw new Error('Missing OpenRouter API key — please configure it in settings');
      }

      const { ChatOpenAI } = await import('@langchain/openai');
      return new ChatOpenAI({
        openAIApiKey: openRouterConfig.apiKey,
        apiKey: openRouterConfig.apiKey,
        modelName: openRouterConfig.model,
        temperature: openRouterConfig.temperature ?? 0.1,
        maxTokens: openRouterConfig.maxTokens,
        configuration: {
          apiKey: openRouterConfig.apiKey,
          baseURL: openRouterConfig.baseUrl ?? 'https://openrouter.ai/api/v1',
          defaultHeaders: {
            'HTTP-Referer': 'https://github.com/neur0map/prowl',
            'X-Title': 'Prowl',
          },
        },
        streaming: true,
      });
    }

    case 'groq': {
      const groqConfig = config as GroqConfig;

      if (!groqConfig.apiKey || groqConfig.apiKey.trim() === '') {
        throw new Error('Groq API key is required but was not provided');
      }

      const { ChatOpenAI } = await import('@langchain/openai');
      return new ChatOpenAI({
        apiKey: groqConfig.apiKey,
        modelName: groqConfig.model,
        temperature: groqConfig.temperature ?? 0.1,
        maxTokens: groqConfig.maxTokens,
        configuration: {
          apiKey: groqConfig.apiKey,
          baseURL: groqConfig.baseUrl ?? 'https://api.groq.com/openai/v1',
        },
        streaming: true,
      });
    }

    default:
      throw new Error(`Unsupported provider: ${(config as any).provider}`);
  }
};

/* Pull the instance name from an Azure OpenAI endpoint.
   "https://my-resource.openai.azure.com" → "my-resource" */
const extractInstanceName = (endpoint: string): string => {
  try {
    const url = new URL(endpoint);
    const hostname = url.hostname;
    const match = hostname.match(/^([^.]+)\.openai\.azure\.com/);
    if (match) {
      return match[1];
    }
    return hostname.split('.')[0];
  } catch {
    return endpoint;
  }
};

/* Wire up a ReAct agent with all code-graph analysis tools attached */
export const buildCodeAgent = async (
  config: ProviderConfig,
  executeQuery: (cypher: string) => Promise<any[]>,
  semanticSearch: (query: string, k?: number, maxDistance?: number) => Promise<any[]>,
  semanticSearchWithContext: (query: string, k?: number, hops?: number) => Promise<any[]>,
  hybridSearch: (query: string, k?: number) => Promise<any[]>,
  isEmbeddingReady: () => boolean,
  isBM25Ready: () => boolean,
  fileContents: Map<string, string>,
  codebaseContext?: ProjectContext
) => {
  const { createReactAgent } = await import('@langchain/langgraph/prebuilt');
  const model = await createChatModel(config);
  const tools = buildAnalysisTools(
    executeQuery,
    semanticSearch,
    semanticSearchWithContext,
    hybridSearch,
    isEmbeddingReady,
    isBM25Ready,
    fileContents
  );

  const systemPrompt = codebaseContext
    ? composeSystemPrompt(SYSTEM_PROMPT, codebaseContext)
    : SYSTEM_PROMPT;

  const agent = createReactAgent({
    llm: model as any,
    tools: tools as any,
    messageModifier: new SystemMessage(systemPrompt) as any,
  });

  return agent;
};

export interface AgentMessage {
  role: 'user' | 'assistant';
  content: string;
}

/**
 * Yield chunks from the agent in real time.
 *
 * Runs two LangGraph stream modes simultaneously:
 *  - 'values': ordered state snapshots (tool invocations and their results)
 *  - 'messages': incremental token output
 *
 * The dual mode preserves the natural rhythm: think → act → think → answer.
 */
export async function* streamAgentResponse(
  agent: ReturnType<typeof createReactAgent>,
  messages: AgentMessage[]
): AsyncGenerator<AgentStreamChunk> {
  try {
    const formattedMessages = messages.map(m => ({
      role: m.role,
      content: m.content,
    }));

    /* Both stream modes active: state snapshots + per-token delivery */
    const stream = await agent.stream(
      { messages: formattedMessages },
      {
        streamMode: ['values', 'messages'] as any,
        recursionLimit: 100,
      } as any
    );

    /* De-duplication sets */
    const emittedCalls = new Set<string>();
    const emittedResults = new Set<string>();
    let lastProcessedMsgCount = formattedMessages.length;
    let allToolsDone = true;
    /* Track whether any tool has been invoked this turn — content before
       tools is classified as narration; content after tools complete is the answer */
    let hasSeenToolCallThisTurn = false;

    for await (const event of stream) {
      let mode: string;
      let data: any;

      if (Array.isArray(event) && event.length === 2 && typeof event[0] === 'string') {
        [mode, data] = event;
      } else if (Array.isArray(event) && event[0]?._getType) {
        mode = 'messages';
        data = event;
      } else {
        mode = 'values';
        data = event;
      }

      /* Per-token output from the 'messages' stream */
      if (mode === 'messages') {
        const [msg] = Array.isArray(data) ? data : [data];
        if (!msg) continue;

        const msgType = msg._getType?.() || msg.type || msg.constructor?.name || 'unknown';

        if (msgType === 'ai' || msgType === 'AIMessage' || msgType === 'AIMessageChunk') {
          const rawContent = msg.content;
          const toolCalls = msg.tool_calls || [];

          let content: string = '';
          if (typeof rawContent === 'string') {
            content = rawContent;
          } else if (Array.isArray(rawContent)) {
            content = rawContent
              .filter((block: any) => block.type === 'text' || typeof block === 'string')
              .map((block: any) => typeof block === 'string' ? block : block.text || '')
              .join('');
          }

          if (content && content.length > 0) {
            /* Decide if this is mid-reasoning narration or part of the final answer */
            const isReasoning =
              !hasSeenToolCallThisTurn ||
              toolCalls.length > 0 ||
              !allToolsDone;
            yield {
              type: isReasoning ? 'reasoning' : 'content',
              [isReasoning ? 'reasoning' : 'content']: content,
            };
          }

          if (toolCalls.length > 0) {
            hasSeenToolCallThisTurn = true;
            allToolsDone = false;
            for (const tc of toolCalls) {
              const toolId = tc.id || `tool-${Date.now()}-${Math.random().toString(36).slice(2)}`;
              if (!emittedCalls.has(toolId)) {
                emittedCalls.add(toolId);
                yield {
                  type: 'tool_call',
                  toolCall: {
                    id: toolId,
                    name: tc.name || tc.function?.name || 'unknown',
                    args: tc.args || (tc.function?.arguments ? JSON.parse(tc.function.arguments) : {}),
                    status: 'running',
                  },
                };
              }
            }
          }
        }

        if (msgType === 'tool' || msgType === 'ToolMessage') {
          const toolCallId = msg.tool_call_id || '';
          if (toolCallId && !emittedResults.has(toolCallId)) {
            emittedResults.add(toolCallId);
            const result = typeof msg.content === 'string' ? msg.content : JSON.stringify(msg.content);
            yield {
              type: 'tool_result',
              toolCall: {
                id: toolCallId,
                name: msg.name || 'tool',
                args: {},
                result: result,
                status: 'completed',
              },
            };
            allToolsDone = true;
          }
        }
      }

      /* 'values' mode fallback — picks up anything the token stream missed */
      if (mode === 'values' && data?.messages) {
        const stepMessages = data.messages || [];

        for (let i = lastProcessedMsgCount; i < stepMessages.length; i++) {
          const msg = stepMessages[i];
          const msgType = msg._getType?.() || msg.type || 'unknown';

          /* Safety net: emit any tool invocations not already surfaced */
          if ((msgType === 'ai' || msgType === 'AIMessage') && !emittedCalls.size) {
            const toolCalls = msg.tool_calls || [];
            for (const tc of toolCalls) {
              const toolId = tc.id || `tool-${Date.now()}`;
              if (!emittedCalls.has(toolId)) {
                allToolsDone = false;
                emittedCalls.add(toolId);
                yield {
                  type: 'tool_call',
                  toolCall: {
                    id: toolId,
                    name: tc.name || 'unknown',
                    args: tc.args || {},
                    status: 'running',
                  },
                };
              }
            }
          }

          /* Safety net: emit any tool results not already surfaced */
          if (msgType === 'tool' || msgType === 'ToolMessage') {
            const toolCallId = msg.tool_call_id || '';
            if (toolCallId && !emittedResults.has(toolCallId)) {
              emittedResults.add(toolCallId);
              const result = typeof msg.content === 'string' ? msg.content : JSON.stringify(msg.content);
              yield {
                type: 'tool_result',
                toolCall: {
                  id: toolCallId,
                  name: msg.name || 'tool',
                  args: {},
                  result: result,
                  status: 'completed',
                },
              };
              allToolsDone = true;
            }
          }
        }

        lastProcessedMsgCount = stepMessages.length;
      }
    }

    yield { type: 'done' };
  } catch (error) {
    const raw = error instanceof Error ? error.message : String(error);
    if (import.meta.env.DEV) {
      console.error('[prowl:agent] stream error:', raw, error);
    }

    let message = raw;
    if (raw.toLowerCase().includes('recursion limit')) {
      message = 'The analysis was too complex and hit the step limit. Try a more specific question, or break it into smaller parts.';
    } else if (raw.toLowerCase().includes('failed to call a function') || raw.toLowerCase().includes('failed_generation')) {
      message = 'The model failed to generate a valid tool call. Try rephrasing your question, or switch to a model with better function-calling support (e.g. GPT-4o, Claude).';
    } else if (raw.includes('401') || raw.toLowerCase().includes('unauthorized') || raw.toLowerCase().includes('authentication')) {
      message = 'Authentication failed. Check your API key in settings.';
    }

    yield {
      type: 'error',
      error: message,
    };
  }
}

/* Single-shot (non-streaming) agent call — returns the full response as a string */
export const invokeAgent = async (
  agent: ReturnType<typeof createReactAgent>,
  messages: AgentMessage[]
): Promise<string> => {
  const formattedMessages = messages.map(m => ({
    role: m.role,
    content: m.content,
  }));

  const result = await agent.invoke({ messages: formattedMessages });

  const lastMessage = result.messages[result.messages.length - 1];
  return lastMessage?.content?.toString() ?? 'No response generated.';
};

/* ── Context compaction ──────────────────────────────────
 *
 * Inspired by Anthropic's context engineering patterns:
 * - Estimate tokens (~4 chars/token)
 * - When history exceeds threshold, summarize older messages
 * - Keep the most recent messages intact (sliding window)
 * - Use the same LLM to generate the summary
 */

/** Rough token estimation: ~4 characters per token */
export function estimateTokens(text: string): number {
  return Math.ceil(text.length / 4);
}

/** Estimate total tokens across an array of messages */
export function estimateHistoryTokens(messages: AgentMessage[]): number {
  return messages.reduce((sum, m) => sum + estimateTokens(m.content) + 4, 0); // +4 per msg overhead
}

/** Default compaction threshold (in estimated tokens) */
export const COMPACTION_THRESHOLD = 40_000;

/** Number of recent messages to always keep intact during compaction */
const KEEP_RECENT = 6; // 3 user + 3 assistant turns

const COMPACTION_PROMPT = `Summarize this conversation concisely. You MUST preserve:
- Key findings, conclusions, and code references (file paths, line numbers)
- Decisions made and their rationale
- Outstanding questions or next steps
- Tool usage results and important data discovered

Be brief but technically precise. Use bullet points. Do not add commentary — just summarize what happened.`;

/**
 * Compact a conversation history by summarizing older messages.
 *
 * Uses the LLM (via createChatModel) to generate the summary.
 * Returns the compacted message array + the summary text.
 */
export async function compactHistory(
  config: import('./types').ProviderConfig,
  messages: AgentMessage[]
): Promise<{ compacted: AgentMessage[]; summary: string }> {
  const totalTokens = estimateHistoryTokens(messages);

  if (totalTokens < COMPACTION_THRESHOLD || messages.length <= KEEP_RECENT + 1) {
    return { compacted: messages, summary: '' };
  }

  // Split: older messages to summarize, recent messages to keep
  const toSummarize = messages.slice(0, -KEEP_RECENT);
  const toKeep = messages.slice(-KEEP_RECENT);

  // Build a conversation transcript for the summarizer
  const transcript = toSummarize
    .map(m => `[${m.role}]: ${m.content}`)
    .join('\n\n');

  // Use the same LLM to generate the summary
  const model = await createChatModel(config);
  const { HumanMessage: HMsg, SystemMessage: SMsg } = await import('@langchain/core/messages');

  const result = await model.invoke([
    new SMsg(COMPACTION_PROMPT),
    new HMsg(`Here is the conversation to summarize:\n\n${transcript}`),
  ]);

  const summary = typeof result.content === 'string'
    ? result.content
    : Array.isArray(result.content)
      ? result.content.filter((b: any) => typeof b === 'string' || b.type === 'text').map((b: any) => typeof b === 'string' ? b : b.text).join('')
      : String(result.content);

  // Build the compacted history: summary as a system-level user message, then recent messages
  const compacted: AgentMessage[] = [
    {
      role: 'user',
      content: `[Previous conversation summary]\n${summary}\n[End of summary — continue from here]`,
    },
    {
      role: 'assistant',
      content: 'Understood. I have the context from our previous discussion. How can I help?',
    },
    ...toKeep,
  ];

  return { compacted, summary };
}
