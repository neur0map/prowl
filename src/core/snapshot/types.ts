import type { GraphNode, GraphRelationship } from '../graph/types';

export const SNAPSHOT_FORMAT_VERSION = 1;

export interface SnapshotMeta {
  formatVersion: number;
  prowlVersion: string;
  projectName: string;
  gitCommit: string | null;
  createdAt: string; // ISO 8601
  nodeCount: number;
  relationshipCount: number;
  fileCount: number;
  embeddingCount: number;
}

export interface SnapshotPayload {
  meta: SnapshotMeta;
  nodes: GraphNode[];
  relationships: GraphRelationship[];
  fileContents: Record<string, string>;
  embeddings: Array<{ nodeId: string; embedding: number[] }>;
}

export interface FileManifest {
  files: Record<string, { hash: string; mtime: number }>;
}

export interface DiffResult {
  added: string[];
  modified: string[];
  deleted: string[];
  isGitRepo: boolean;
}
