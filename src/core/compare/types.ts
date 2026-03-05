/**
 * Types for the Compare Mode feature.
 * Lightweight GitHub REST API approach — no disk, no tree-sitter.
 */

export interface ComparisonMeta {
  owner: string;
  repo: string;
  branch: string;
  repoName: string;
  repoUrl: string;
  description: string;
  token?: string;
}

export interface ComparisonTreeEntry {
  path: string;
  type: 'file' | 'dir';
  size: number;
}

export interface ComparisonStats {
  repoName: string;
  repoUrl: string;
  description: string;
  fileCount: number;
  dirCount: number;
  totalSize: number;
  cachedFileCount: number;
}
