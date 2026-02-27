/**
 * Architecture Map — the main graph visualization.
 *
 * Replaces the old force-directed graph with a structured,
 * card-based overview using React Flow + ELK.js layout.
 *
 * Supports three navigation levels:
 *   1. Overview — cluster cards with animated dependency edges
 *   2. Drill-in — file list for a selected cluster
 *   3. Code — full Monaco editor (reuses existing CodeEditorPanel)
 */

import { useCallback, useMemo, useState, forwardRef, useImperativeHandle } from 'react';
import {
  ReactFlow,
  Background,
  MiniMap,
  Controls,
  useEdgesState,
  useNodesState,
  type Node,
  type Edge,
  BackgroundVariant,
} from '@xyflow/react';
import '@xyflow/react/dist/style.css';

import { useAppState } from '../hooks/useAppState';
import { useElkLayout } from '../hooks/useElkLayout';
import ClusterCard from './ClusterCard';
import AnimatedEdge from './AnimatedEdge';
import ZoneLabel from './ZoneLabel';
import DrillInPanel from './DrillInPanel';
import BreadcrumbNav from './BreadcrumbNav';
import type { ClusterSummary } from '../lib/elk-adapter';
import { getModuleColor } from '../lib/constants';

/* ── Custom node/edge type registry ───────────────── */

const nodeTypes = { clusterCard: ClusterCard, zoneLabel: ZoneLabel };
const edgeTypes = { animatedFlow: AnimatedEdge };

/* ── Handle exposed to parent ─────────────────────── */

export interface ArchitectureMapHandle {
  focusNode: (nodeId: string) => void;
  refreshGraph: () => void;
}

/* ── Navigation state ─────────────────────────────── */

type NavLevel = 'overview' | 'drillIn';

interface NavState {
  level: NavLevel;
  selectedCluster: ClusterSummary | null;
}

/* ── Component ────────────────────────────────────── */

const ArchitectureMap = forwardRef<ArchitectureMapHandle>(function ArchitectureMap(_, ref) {
  const { graph, setSelectedNode, selectedNode } = useAppState();
  const { nodes: elkNodes, edges: elkEdges, clusters, isLayoutReady, recompute } = useElkLayout(graph);

  const [nodes, setNodes, onNodesChange] = useNodesState([] as Node[]);
  const [edges, setEdges, onEdgesChange] = useEdgesState([] as Edge[]);

  const [nav, setNav] = useState<NavState>({ level: 'overview', selectedCluster: null });

  /* Sync ELK layout results into React Flow state */
  useMemo(() => {
    if (isLayoutReady) {
      setNodes(elkNodes);
      setEdges(elkEdges);
    }
  }, [isLayoutReady, elkNodes, elkEdges, setNodes, setEdges]);

  /* Expose handle for App.tsx compatibility */
  useImperativeHandle(ref, () => ({
    focusNode: (_nodeId: string) => {
      /* Find which cluster contains this node and drill into it */
      for (const c of clusters) {
        const match = c.files.find(f => f.id === _nodeId) || c.symbols.find(s => s.id === _nodeId);
        if (match) {
          setNav({ level: 'drillIn', selectedCluster: c });
          return;
        }
      }
    },
    refreshGraph: () => {
      recompute();
    },
  }), [clusters, recompute]);

  /* Click a cluster card → drill in */
  const handleNodeClick = useCallback((_: React.MouseEvent, node: Node) => {
    const cluster = clusters.find(c => c.id === node.id);
    if (cluster) {
      setNav({ level: 'drillIn', selectedCluster: cluster });
    }
  }, [clusters]);

  /* Navigate back to overview */
  const handleBackToOverview = useCallback(() => {
    setNav({ level: 'overview', selectedCluster: null });
    setSelectedNode(null);
  }, [setSelectedNode]);

  /* Click on empty canvas area → close drill-in panel */
  const handlePaneClick = useCallback(() => {
    if (nav.level === 'drillIn') {
      handleBackToOverview();
    }
  }, [nav.level, handleBackToOverview]);

  /* Handle file selection from drill-in panel */
  const handleFileSelect = useCallback((nodeId: string) => {
    if (!graph) return;
    const node = graph.nodes.find(n => n.id === nodeId);
    if (node) {
      setSelectedNode(node);
    }
  }, [graph, setSelectedNode]);

  /* Keyboard: Escape goes back */
  const handleKeyDown = useCallback((e: React.KeyboardEvent) => {
    if (e.key === 'Escape') {
      if (nav.level === 'drillIn') {
        handleBackToOverview();
      }
    }
  }, [nav.level, handleBackToOverview]);

  /* Breadcrumb segments */
  const breadcrumbs = useMemo(() => {
    const crumbs = [{ label: 'Overview', onClick: handleBackToOverview }];
    if (nav.selectedCluster) {
      crumbs.push({ label: nav.selectedCluster.name, onClick: () => {} });
    }
    if (selectedNode) {
      crumbs.push({ label: selectedNode.properties.name, onClick: () => {} });
    }
    return crumbs;
  }, [nav.selectedCluster, selectedNode, handleBackToOverview]);

  /* Dim non-selected clusters during drill-in */
  const styledNodes = useMemo(() => {
    if (nav.level !== 'drillIn' || !nav.selectedCluster) return nodes;
    return nodes.map(n => ({
      ...n,
      style: {
        ...n.style,
        opacity: n.id === nav.selectedCluster!.id ? 1 : 0.15,
        transition: 'opacity 0.3s ease',
        pointerEvents: (n.id === nav.selectedCluster!.id ? 'auto' : 'none') as React.CSSProperties['pointerEvents'],
      },
    }));
  }, [nodes, nav]);

  const styledEdges = useMemo(() => {
    if (nav.level !== 'drillIn') return edges;
    return edges.map(e => ({
      ...e,
      style: { ...e.style, opacity: 0.05 },
    }));
  }, [edges, nav.level]);

  if (!graph || !isLayoutReady) {
    return (
      <div className="flex-1 flex items-center justify-center bg-void">
        <div className="text-text-muted text-sm font-mono animate-pulse">
          Computing layout...
        </div>
      </div>
    );
  }

  return (
    <div className="w-full h-full relative" onKeyDown={handleKeyDown} tabIndex={0}>
      {/* Breadcrumb */}
      {nav.level !== 'overview' && (
        <BreadcrumbNav crumbs={breadcrumbs} />
      )}

      {/* React Flow canvas */}
      <ReactFlow
        nodes={styledNodes}
        edges={styledEdges}
        onNodesChange={onNodesChange}
        onEdgesChange={onEdgesChange}
        onNodeClick={nav.level === 'overview' ? handleNodeClick : undefined}
        onPaneClick={handlePaneClick}
        nodeTypes={nodeTypes}
        edgeTypes={edgeTypes}
        fitView
        fitViewOptions={{ padding: 0.2 }}
        minZoom={0.1}
        maxZoom={2.5}
        proOptions={{ hideAttribution: true }}
        nodesDraggable={false}
        nodesConnectable={false}
        elementsSelectable={nav.level === 'overview'}
        panOnScroll
        zoomOnScroll={false}
        zoomOnPinch
        panOnDrag
        selectionOnDrag={false}
        zoomOnDoubleClick={false}
      >
        <Background variant={BackgroundVariant.Dots} gap={24} size={1} color="rgba(255,255,255,0.03)" />
        <MiniMap
          nodeColor={(n) => {
            const idx = clusters.findIndex(c => c.id === n.id);
            return idx >= 0 ? getModuleColor(idx) : '#48484a';
          }}
          maskColor="rgba(28, 28, 30, 0.85)"
          className="!bg-deep !border-border-subtle !rounded-lg"
          pannable
          zoomable
        />
        <Controls
          showInteractive={false}
          className="!bg-deep !border-border-subtle !rounded-lg !shadow-lg [&>button]:!bg-deep [&>button]:!border-border-subtle [&>button]:!fill-text-secondary hover:[&>button]:!bg-surface"
        />
      </ReactFlow>

      {/* Drill-in panel overlay */}
      {nav.level === 'drillIn' && nav.selectedCluster && (
        <DrillInPanel
          cluster={nav.selectedCluster}
          graph={graph}
          onFileSelect={handleFileSelect}
          onBack={handleBackToOverview}
        />
      )}
    </div>
  );
});

export default ArchitectureMap;
