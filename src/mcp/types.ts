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
  | 'status'
  | 'compare'
  | 'compare-file-tree'
  | 'compare-read-file'
  | 'compare-grep'
  | 'compare-summary'
  | 'detect-changes';

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

export interface CompareParams {
  repo_url: string;
  token?: string;
  branch?: string;
}

export interface CompareFileTreeParams {
  dir_path?: string;
}

export interface CompareReadFileParams {
  file_path: string;
}

export interface CompareGrepParams {
  pattern: string;
  file_filter?: string;
  case_sensitive?: boolean;
  max_results?: number;
}

export type CompareSummaryParams = Record<string, never>;

export interface DetectChangesParams {
  scope?: 'working' | 'staged' | 'all' | 'branch';
  base_ref?: string;
}

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
  'compare': CompareParams;
  'compare-file-tree': CompareFileTreeParams;
  'compare-read-file': CompareReadFileParams;
  'compare-grep': CompareGrepParams;
  'compare-summary': CompareSummaryParams;
  'detect-changes': DetectChangesParams;
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
