import type { CodeGraph } from '../graph/types';
import { createCodeGraph } from '../graph/graph';
import type { SnapshotPayload } from './types';

/**
 * Rebuild a CodeGraph from a deserialized snapshot payload.
 */
export function restoreGraphFromPayload(payload: SnapshotPayload): CodeGraph {
  const graph = createCodeGraph();

  for (const node of payload.nodes) {
    graph.addNode(node);
  }
  for (const rel of payload.relationships) {
    graph.addRelationship(rel);
  }

  return graph;
}

/**
 * Rebuild the fileContents Map from a snapshot payload.
 */
export function restoreFileContents(payload: SnapshotPayload): Map<string, string> {
  return new Map(Object.entries(payload.fileContents));
}
