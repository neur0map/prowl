import type { SnapshotPayload } from './types';
import { restoreGraphFromPayload, restoreFileContents } from './restorer';

/**
 * Restore KuzuDB from a snapshot payload.
 *
 * - Creates fresh in-memory DB with schema
 * - Loads reconstructed graph via the existing CSV bulk-load pipeline
 * - Re-inserts embeddings and recreates vector index
 */
export async function restoreKuzuFromSnapshot(
  payload: SnapshotPayload,
): Promise<void> {
  // Lazy import kuzu-adapter to avoid pulling it in if not needed
  const kuzu = await import('../kuzu/kuzu-adapter');

  // Rebuild graph and fileContents from payload
  const graph = restoreGraphFromPayload(payload);
  const fileContents = restoreFileContents(payload);

  // Load graph into KuzuDB (creates fresh DB + schema, bulk-loads via CSV)
  await kuzu.loadGraphToKuzu(graph, fileContents);

  // Re-insert embeddings if present
  if (payload.embeddings.length > 0) {
    const batchSize = 100;
    for (let i = 0; i < payload.embeddings.length; i += batchSize) {
      const batch = payload.embeddings.slice(i, i + batchSize);
      for (const { nodeId, embedding } of batch) {
        const embStr = `[${embedding.join(',')}]`;
        // Escape for Cypher string: backslashes first, then quotes
        const escaped = nodeId.replace(/\\/g, '\\\\').replace(/'/g, "\\'");
        try {
          await kuzu.executeQuery(
            `CREATE (e:CodeEmbedding {nodeId: '${escaped}', embedding: ${embStr}})`
          );
        } catch {
          // Duplicate or error — skip
        }
      }
    }

    // Recreate vector index
    try {
      const { CREATE_VECTOR_INDEX_QUERY } = await import('../kuzu/schema');
      await kuzu.executeQuery(CREATE_VECTOR_INDEX_QUERY);
    } catch {
      // Index creation may fail if not enough embeddings — non-fatal
    }
  }
}
