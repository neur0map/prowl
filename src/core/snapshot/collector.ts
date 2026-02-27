import type { CodeGraph } from '../graph/types';
import type { SnapshotPayload, SnapshotMeta } from './types';
import { SNAPSHOT_FORMAT_VERSION } from './types';

/**
 * Collect all data needed for a snapshot payload from the current worker state.
 *
 * @param graph - The knowledge graph
 * @param fileContents - Map of file path -> content
 * @param projectName - Name of the project
 * @param prowlVersion - Current Prowl version
 * @param kuzuQueryFn - Optional: function to query KuzuDB for embeddings
 * @param gitCommit - Optional: current git HEAD commit
 */
export async function collectSnapshotPayload(
  graph: CodeGraph,
  fileContents: Map<string, string>,
  projectName: string,
  prowlVersion: string,
  kuzuQueryFn?: (cypher: string) => Promise<any[]>,
  gitCommit?: string | null,
): Promise<SnapshotPayload> {
  // Collect embeddings from KuzuDB if available
  let embeddings: Array<{ nodeId: string; embedding: number[] }> = [];
  if (kuzuQueryFn) {
    try {
      const rows = await kuzuQueryFn(
        'MATCH (e:CodeEmbedding) RETURN e.nodeId AS nodeId, e.embedding AS embedding'
      );
      embeddings = rows.map(row => ({
        nodeId: row.nodeId,
        embedding: Array.from(row.embedding as number[]),
      }));
    } catch {
      // Embeddings table may not exist or be empty
    }
  }

  const nodes = graph.nodes;
  const relationships = graph.relationships;

  const meta: SnapshotMeta = {
    formatVersion: SNAPSHOT_FORMAT_VERSION,
    prowlVersion,
    projectName,
    gitCommit: gitCommit ?? null,
    createdAt: new Date().toISOString(),
    nodeCount: nodes.length,
    relationshipCount: relationships.length,
    fileCount: fileContents.size,
    embeddingCount: embeddings.length,
  };

  return {
    meta,
    nodes,
    relationships,
    fileContents: Object.fromEntries(fileContents),
    embeddings,
  };
}
