/**
 * Hook that runs ELK.js layout computation.
 * Returns positioned React Flow nodes and edges from a CodeGraph.
 */

import { useState, useEffect, useCallback, useRef } from 'react';
import type { Node, Edge } from '@xyflow/react';
import type { CodeGraph } from '../core/graph/types';
import type { ClusterSummary, CrossClusterEdge } from '../lib/elk-adapter';
import type { ClusterNodeData } from '../lib/flow-adapter';

interface UseElkLayoutReturn {
  nodes: Node<ClusterNodeData>[];
  edges: Edge[];
  clusters: ClusterSummary[];
  crossEdges: CrossClusterEdge[];
  isLayoutReady: boolean;
  recompute: () => void;
}

export function useElkLayout(graph: CodeGraph | null): UseElkLayoutReturn {
  const [nodes, setNodes] = useState<Node<ClusterNodeData>[]>([]);
  const [edges, setEdges] = useState<Edge[]>([]);
  const [clusters, setClusters] = useState<ClusterSummary[]>([]);
  const [crossEdges, setCrossEdges] = useState<CrossClusterEdge[]>([]);
  const [isLayoutReady, setIsLayoutReady] = useState(false);
  const computeIdRef = useRef(0);

  const compute = useCallback(async (codeGraph: CodeGraph) => {
    const id = ++computeIdRef.current;

    /* Lazy-import to keep the main bundle lean */
    const [
      { buildClusterSummaries, buildCrossClusterEdges, computeZoneGridLayout },
      { elkToFlowNodes, crossEdgesToFlowEdges },
    ] = await Promise.all([
      import('../lib/elk-adapter'),
      import('../lib/flow-adapter'),
    ]);

    /* Stale check */
    if (id !== computeIdRef.current) return;

    const summaries = buildClusterSummaries(codeGraph);
    const cross = buildCrossClusterEdges(codeGraph, summaries);
    const gridResult = computeZoneGridLayout(summaries);

    /* Stale check */
    if (id !== computeIdRef.current) return;

    const flowNodes = elkToFlowNodes(gridResult, summaries);
    const flowEdges = crossEdgesToFlowEdges(cross);

    setClusters(summaries);
    setCrossEdges(cross);
    setNodes(flowNodes);
    setEdges(flowEdges);
    setIsLayoutReady(true);
  }, []);

  useEffect(() => {
    if (!graph || graph.nodeCount === 0) {
      setNodes([]);
      setEdges([]);
      setClusters([]);
      setCrossEdges([]);
      setIsLayoutReady(false);
      return;
    }
    compute(graph);
  }, [graph, compute]);

  const recompute = useCallback(() => {
    if (graph) compute(graph);
  }, [graph, compute]);

  return { nodes, edges, clusters, crossEdges, isLayoutReady, recompute };
}
