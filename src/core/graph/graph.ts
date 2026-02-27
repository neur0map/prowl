import { GraphNode, GraphRelationship, CodeGraph } from './types'

/* Builds a deduplicated node/relationship store backed by plain objects */
export function createCodeGraph(): CodeGraph {
  const nodeMap: Record<string, GraphNode> = Object.create(null);
  const relMap: Record<string, GraphRelationship> = Object.create(null);
  let nCount = 0;
  let rCount = 0;

  function putNode(node: GraphNode): void {
    if (nodeMap[node.id] !== undefined) return;
    nodeMap[node.id] = node;
    nCount += 1;
  }

  function putRel(rel: GraphRelationship): void {
    if (relMap[rel.id] !== undefined) return;
    relMap[rel.id] = rel;
    rCount += 1;
  }

  function allNodes(): GraphNode[] {
    const buf: GraphNode[] = [];
    for (const k in nodeMap) buf.push(nodeMap[k]);
    return buf;
  }

  function allRels(): GraphRelationship[] {
    const buf: GraphRelationship[] = [];
    for (const k in relMap) buf.push(relMap[k]);
    return buf;
  }

  return {
    get nodes() {
      return allNodes();
    },

    get relationships() {
      return allRels();
    },

    get nodeCount() {
      return nCount;
    },

    get relationshipCount() {
      return rCount;
    },

    addNode: putNode,
    addRelationship: putRel,
    hasNode: (id: string) => nodeMap[id] !== undefined,
  };
}
