/* ── Provider identifiers ────────────────────────────── */

export type LLMProvider = 'openai' | 'azure-openai' | 'gemini' | 'anthropic' | 'ollama' | 'openrouter' | 'groq';

/* ── Per-provider configuration shapes ───────────────── */

export interface BaseProviderConfig {
  provider: LLMProvider;
  model: string;
  temperature?: number;
  maxTokens?: number;
}

export interface OpenAIConfig extends BaseProviderConfig {
  provider: 'openai';
  apiKey: string;
  model: string;
  baseUrl?: string;
}

export interface AzureOpenAIConfig extends BaseProviderConfig {
  provider: 'azure-openai';
  apiKey: string;
  endpoint: string;
  deploymentName: string;
  apiVersion?: string;
}

export interface GeminiConfig extends BaseProviderConfig {
  provider: 'gemini';
  apiKey: string;
  model: string;
  baseUrl?: string;
}

export interface AnthropicConfig extends BaseProviderConfig {
  provider: 'anthropic';
  apiKey: string;
  model: string;
  baseUrl?: string;
}

export interface OllamaConfig extends BaseProviderConfig {
  provider: 'ollama';
  baseUrl?: string;
  model: string;
}

export interface OpenRouterConfig extends BaseProviderConfig {
  provider: 'openrouter';
  apiKey: string;
  model: string;
  baseUrl?: string;
}

export interface GroqConfig extends BaseProviderConfig {
  provider: 'groq';
  apiKey: string;
  model: string;
  baseUrl?: string;
}

export type ProviderConfig =
  | OpenAIConfig
  | AzureOpenAIConfig
  | GeminiConfig
  | AnthropicConfig
  | OllamaConfig
  | OpenRouterConfig
  | GroqConfig;

/* ── Persisted user preferences ──────────────────────── */

export interface LLMSettings {
  activeProvider: LLMProvider;

  openai?: Partial<Omit<OpenAIConfig, 'provider'>>;
  azureOpenAI?: Partial<Omit<AzureOpenAIConfig, 'provider'>>;
  gemini?: Partial<Omit<GeminiConfig, 'provider'>>;
  anthropic?: Partial<Omit<AnthropicConfig, 'provider'>>;
  ollama?: Partial<Omit<OllamaConfig, 'provider'>>;
  openrouter?: Partial<Omit<OpenRouterConfig, 'provider'>>;
  groq?: Partial<Omit<GroqConfig, 'provider'>>;

  intelligentClustering: boolean;
  hasSeenClusteringPrompt: boolean;
  useSameModelForClustering: boolean;
  clusteringProvider?: Partial<ProviderConfig>;
}

/* ── Initial values per provider ─────────────────────── */

const INITIAL_PROVIDER_VALUES = {
  openai: {
    apiKey: '',
    model: 'gpt-4o',
    temperature: 0.1,
  },
  gemini: {
    apiKey: '',
    model: 'gemini-2.0-flash',
    temperature: 0.1,
  },
  azureOpenAI: {
    apiKey: '',
    endpoint: '',
    deploymentName: '',
    model: 'gpt-4o',
    apiVersion: '2024-08-01-preview',
    temperature: 0.1,
  },
  anthropic: {
    apiKey: '',
    model: 'claude-sonnet-4-20250514',
    temperature: 0.1,
  },
  ollama: {
    baseUrl: 'http://localhost:11434',
    model: 'llama3.2',
    temperature: 0.1,
  },
  openrouter: {
    apiKey: '',
    model: '',
    baseUrl: 'https://openrouter.ai/api/v1',
    temperature: 0.1,
  },
  groq: {
    apiKey: '',
    model: 'llama-3.3-70b-versatile',
    baseUrl: 'https://api.groq.com/openai/v1',
    temperature: 0.1,
  },
} as const;

export const DEFAULT_LLM_SETTINGS: LLMSettings = {
  activeProvider: 'gemini',
  intelligentClustering: false,
  hasSeenClusteringPrompt: false,
  useSameModelForClustering: true,
  openai: INITIAL_PROVIDER_VALUES.openai,
  gemini: INITIAL_PROVIDER_VALUES.gemini,
  azureOpenAI: INITIAL_PROVIDER_VALUES.azureOpenAI,
  anthropic: INITIAL_PROVIDER_VALUES.anthropic,
  ollama: INITIAL_PROVIDER_VALUES.ollama,
  openrouter: INITIAL_PROVIDER_VALUES.openrouter,
  groq: INITIAL_PROVIDER_VALUES.groq,
};

/* ── Agent execution types ───────────────────────────── */

export interface ToolCallInfo {
  id: string;
  name: string;
  args: Record<string, unknown>;
  result?: string;
  status: 'pending' | 'running' | 'completed' | 'error';
}

export interface MessageStep {
  id: string;
  type: 'reasoning' | 'tool_call' | 'content';
  content?: string;
  toolCall?: ToolCallInfo;
}

export interface AgentStep {
  id: string;
  type: 'reasoning' | 'tool_call' | 'answer';
  content?: string;
  toolCall?: ToolCallInfo;
  timestamp: number;
}

/* ── Chat and streaming types ────────────────────────── */

export interface ChatMessage {
  id: string;
  role: 'user' | 'assistant' | 'tool';
  content: string;
  /** @deprecated Use steps instead for proper ordering */
  toolCalls?: ToolCallInfo[];
  steps?: MessageStep[];
  toolCallId?: string;
  timestamp: number;
}

export interface AgentStreamChunk {
  type: 'reasoning' | 'tool_call' | 'tool_result' | 'content' | 'error' | 'done';
  reasoning?: string;
  content?: string;
  toolCall?: ToolCallInfo;
  error?: string;
}

/* ── Conversation persistence ────────────────────────── */

export interface StoredMessage {
  id: string;
  role: 'user' | 'assistant';
  content: string;
  /** Brief summary of tool calls made (for history reconstruction) */
  toolSummary?: string;
  timestamp: number;
}

export interface StoredConversation {
  id: string;
  projectPath: string;
  title: string;
  messages: StoredMessage[];
  /** If set, this is a compacted summary of earlier messages */
  compactedSummary?: string;
  createdAt: number;
  updatedAt: number;
}

/* ── KuzuDB graph schema reference (injected into LLM prompts) ── */

export const KUZU_SCHEMA_REF = `
Kuzu Graph Schema (multi-table layout):

Nodes:
1. File — source files
   - id: STRING (pk), name: STRING, filePath: STRING, content: STRING

2. Folder — directory entries
   - id: STRING (pk), name: STRING, filePath: STRING

3. Function — standalone function definitions
   - id: STRING (pk), name: STRING, filePath: STRING, startLine: INT64, endLine: INT64, content: STRING

4. Class — class definitions
   - id: STRING (pk), name: STRING, filePath: STRING, startLine: INT64, endLine: INT64, content: STRING

5. Interface — interface / type alias definitions
   - id: STRING (pk), name: STRING, filePath: STRING, startLine: INT64, endLine: INT64, content: STRING

6. Method — methods bound to a class
   - id: STRING (pk), name: STRING, filePath: STRING, startLine: INT64, endLine: INT64, content: STRING

7. CodeElement — catch-all for other code entities
   - id: STRING (pk), name: STRING, filePath: STRING, startLine: INT64, endLine: INT64, content: STRING

8. CodeEmbedding — vector store (separate table for performance)
   - nodeId: STRING (pk), embedding: FLOAT[384]

9. Struct, Enum, Macro, Typedef, Union, Namespace, Trait, Impl, TypeAlias, Const, Static, Property, Record, Delegate, Annotation, Constructor, Template, Module
   — language-specific code elements (same schema as CodeElement)
   - id: STRING (pk), name: STRING, filePath: STRING, startLine: INT64, endLine: INT64, content: STRING

Edges:
CodeEdge — unified edge table linking all node types via a 'type' property
  type values: CONTAINS, DEFINES, IMPORTS, CALLS, EXTENDS, IMPLEMENTS

Edge semantics:
- CONTAINS: Folder->Folder, Folder->File
- DEFINES: File->Function, File->Class, File->Interface, File->Method, File->CodeElement, File->Struct, File->Enum, etc.
- IMPORTS: File->File
- CALLS: File->Function, File->Method, Function->Function, Function->Method
- EXTENDS: Class->Class, Interface->Interface, Struct->Struct, etc.
- IMPLEMENTS: Class->Interface, Struct->Trait, Struct->Enum, Impl->Trait, etc.

Example Queries:

1. All functions:
   MATCH (f:Function) RETURN f.name, f.filePath LIMIT 10

2. Definitions in a file:
   MATCH (f:File)-[:CodeEdge {type: 'DEFINES'}]->(fn:Function)
   WHERE f.name = 'utils.ts'
   RETURN fn.name

3. Callers of a function:
   MATCH (caller:File)-[:CodeEdge {type: 'CALLS'}]->(fn:Function {name: 'myFunction'})
   RETURN caller.name, caller.filePath

4. Imports from a file:
   MATCH (f:File {name: 'main.ts'})-[:CodeEdge {type: 'IMPORTS'}]->(imported:File)
   RETURN imported.name

5. Files that import a given file:
   MATCH (f:File)-[:CodeEdge {type: 'IMPORTS'}]->(target:File {name: 'utils.ts'})
   RETURN f.name, f.filePath

6. Vector / semantic search (embeddings in a separate table — requires join):
   CALL QUERY_VECTOR_INDEX('CodeEmbedding', 'code_embedding_idx', \$queryVector, 10)
   YIELD node AS emb, distance
   WITH emb, distance
   WHERE distance < 0.4
   MATCH (n:Function {id: emb.nodeId})
   RETURN n.name, n.filePath, distance
   ORDER BY distance

7. Cross-type name search (use UNION):
   MATCH (f:Function) WHERE f.name CONTAINS 'auth' RETURN f.id, f.name, 'Function' AS type
   UNION ALL
   MATCH (c:Class) WHERE c.name CONTAINS 'auth' RETURN c.id, c.name, 'Class' AS type

8. Directory contents:
   MATCH (parent:Folder)-[:CodeEdge {type: 'CONTAINS'}]->(child)
   WHERE parent.name = 'src'
   RETURN child.name, labels(child)[0] AS type

9. All edges for a node:
   MATCH (f:File {name: 'index.ts'})-[r:CodeEdge]-(m)
   RETURN m.name, r.type

Vector search note (for execute_vector_cypher):
- Place {{QUERY_VECTOR}} in the Cypher where the vector literal should go.
- The runtime substitutes it with CAST([..] AS FLOAT[384]).

KuzuDB constraints (violating these will produce errors):
1. Never return raw nodes (RETURN f) — always project properties: RETURN f.name, f.filePath
2. WITH + ORDER BY requires LIMIT or SKIP: WITH n, cnt ORDER BY cnt DESC LIMIT 20 RETURN n, cnt
   Omitting LIMIT after ORDER BY in a WITH clause triggers "ORDER BY must be followed by SKIP or LIMIT"
3. Valid properties: name, filePath, startLine, endLine, content, isExported. Not "path", "label", or "file".
4. Use LABEL(n) to get the node table name (e.g. "File", "Function", "Class")
5. Table names: File, Folder, Function, Class, Interface, Method, CodeElement, Struct, Enum, Macro, Typedef, Union, Namespace, Trait, Impl, TypeAlias, Const, Static, Property, Record, Delegate, Annotation, Constructor, Template, Module
6. Edge syntax: [:CodeEdge {type: 'DEFINES'}]
7. Join CodeEmbedding.nodeId to the target table's id for vector results
8. Always use LIMIT to keep result sets manageable

Reliable patterns:
- Most-connected files: MATCH (f:File)-[r:CodeEdge]-(m) WITH f.name AS name, f.filePath AS fp, COUNT(r) AS c ORDER BY c DESC LIMIT 10 RETURN name, fp, c
- Functions in a file: MATCH (f:File)-[:CodeEdge {type: 'DEFINES'}]->(fn:Function) WHERE f.name = 'main.rs' RETURN fn.name, fn.startLine
`;
