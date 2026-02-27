import { generateId } from '@/lib/utils';
import type { CodeGraph, GraphNode, GraphRelationship } from '../graph/types';

/**
 * Walks every source path and materialises the folder/file
 * hierarchy as graph nodes linked by CONTAINS edges.
 */
export function buildDirectoryTree(graph: CodeGraph, sourcePaths: string[]): void {
  for (const raw of sourcePaths) {
    const parts = raw.split(/[/\\]/);
    let running = '';
    let parentId = '';

    for (let i = 0; i < parts.length; i++) {
      const name = parts[i];
      running = running === '' ? name : `${running}/${name}`;
      const leaf = i === parts.length - 1;
      const tag = leaf ? 'File' : 'Folder';
      const nodeId = generateId(tag, running);

      const node: GraphNode = {
        id: nodeId,
        label: tag,
        properties: { name, filePath: running },
      };
      graph.addNode(node);

      if (parentId) {
        const edgeId = generateId('CONTAINS', `${parentId}->${nodeId}`);
        const edge: GraphRelationship = {
          id: edgeId,
          type: 'CONTAINS',
          sourceId: parentId,
          targetId: nodeId,
          confidence: 1.0,
          reason: '',
        };
        graph.addRelationship(edge);
      }

      parentId = nodeId;
    }
  }
}

/* preserve the old export name so nothing downstream breaks */
export { buildDirectoryTree as processStructure };
