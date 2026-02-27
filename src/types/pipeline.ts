import { GraphNode, GraphRelationship, CodeGraph } from '../core/graph/types';
import { CommunityDetectionResult } from '../core/ingestion/community-processor';
import { ProcessDetectionResult } from '../core/ingestion/process-processor';

export type IndexingPhase = 'idle' | 'extracting' | 'structure' | 'parsing' | 'imports' | 'calls' | 'heritage' | 'communities' | 'processes' | 'enriching' | 'complete' | 'error';

export interface IndexingStats {
  filesProcessed: number;
  totalFiles: number;
  nodesCreated: number;
}

export interface IndexingProgress {
  phase: IndexingPhase;
  percent: number;
  message: string;
  detail?: string;
  stats?: IndexingStats;
}

export interface IndexingResult {
  graph: CodeGraph;
  fileContents: Map<string, string>;
  communityResult?: CommunityDetectionResult;
  processResult?: ProcessDetectionResult;
}

export interface SerializableIndexingResult {
  nodes: GraphNode[];
  relationships: GraphRelationship[];
  fileContents: Record<string, string>;
}

export const serializeIndexingResult = (result: IndexingResult): SerializableIndexingResult => {
  const fileContents: Record<string, string> = {};
  for (const [key, value] of result.fileContents) {
    fileContents[key] = value;
  }

  return {
    nodes: result.graph.nodes,
    relationships: result.graph.relationships,
    fileContents,
  };
};

export const deserializeIndexingResult = (
  serialized: SerializableIndexingResult,
  createGraph: () => CodeGraph
): IndexingResult => {
  const graph = createGraph();
  serialized.nodes.forEach(node => graph.addNode(node));
  serialized.relationships.forEach(rel => graph.addRelationship(rel));

  const fileContents = new Map<string, string>();
  for (const key in serialized.fileContents) {
    fileContents.set(key, serialized.fileContents[key]);
  }

  return {
    graph,
    fileContents,
  };
};

export const isPipelineComplete = (phase: IndexingPhase): boolean => phase === 'complete';
