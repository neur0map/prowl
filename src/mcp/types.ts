/**
 * Shared types for the MCP (Model Context Protocol) bridge.
 * Used by the HTTP server, IPC layer, renderer tool handlers,
 * and the standalone MCP server.
 */

/* ── Tool name union ───────────────────────────────────── */

export type McpToolName =
  | 'search'
  | 'cypher'
  | 'grep'
  | 'read-file'
  | 'overview'
  | 'explore'
  | 'impact'
  | 'get-context'
  | 'get-hotspots'
  | 'chat-history'
  | 'ask'
  | 'investigate'
  | 'status';

/* ── Input shapes per tool ─────────────────────────────── */

export interface SearchParams {
  query: string;
  limit?: number;
  useReranker?: boolean;
}

export interface CypherParams {
  cypher: string;
  query?: string;
}

export interface GrepParams {
  pattern: string;
  fileFilter?: string;
  caseSensitive?: boolean;
  maxResults?: number;
}

export interface ReadFileParams {
  filePath: string;
}

export type OverviewParams = Record<string, never>;

export interface ExploreParams {
  target: string;
  type?: 'symbol' | 'cluster' | 'process';
}

export interface ImpactParams {
  target: string;
  direction: 'upstream' | 'downstream';
  maxDepth?: number;
  relationTypes?: string[];
  includeTests?: boolean;
  minConfidence?: number;
}

export interface GetContextParams {
  projectName?: string;
}

export interface GetHotspotsParams {
  limit?: number;
}

export type ChatHistoryParams = Record<string, never>;

export interface AskParams {
  question: string;
}

export interface InvestigateParams {
  task: string;
  depth?: number;
}

export type StatusParams = Record<string, never>;

/* ── Param union map ───────────────────────────────────── */

export interface McpToolParamMap {
  'search': SearchParams;
  'cypher': CypherParams;
  'grep': GrepParams;
  'read-file': ReadFileParams;
  'overview': OverviewParams;
  'explore': ExploreParams;
  'impact': ImpactParams;
  'get-context': GetContextParams;
  'get-hotspots': GetHotspotsParams;
  'chat-history': ChatHistoryParams;
  'ask': AskParams;
  'investigate': InvestigateParams;
  'status': StatusParams;
}

/* ── Request / Response envelopes ──────────────────────── */

export interface McpToolRequest<T extends McpToolName = McpToolName> {
  requestId: string;
  toolName: T;
  params: McpToolParamMap[T];
}

export interface McpToolResponse {
  requestId: string;
  success: boolean;
  data?: unknown;
  error?: string;
}
