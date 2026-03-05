/**
 * Converts ELK layout output into React Flow nodes and edges.
 */

import type { Node, Edge } from '@xyflow/react';
import type { ClusterSummary, CrossClusterEdge, ClusterZone } from './elk-adapter';
import { ZONE_META } from './elk-adapter';

/* ── React Flow node data shapes ───────────────────── */

export interface ClusterNodeData {
  cluster: ClusterSummary;
  isHighlighted?: boolean;
  [key: string]: unknown;
}

export interface ZoneLabelData {
  zone: ClusterZone;
  label: string;
  color: string;
  width: number;
  height: number;
  [key: string]: unknown;
}

export interface FileNodeData {
  fileNode: import('../core/graph/types').GraphNode;
  lineCount: number;
  exportCount: number;
  [key: string]: unknown;
}

/* ── ELK output shape (subset) ─────────────────────── */

interface ElkLayoutNode {
  id: string;
  x?: number;
  y?: number;
  width?: number;
  height?: number;
}

interface ElkLayoutResult {
  children?: ElkLayoutNode[];
}

/* ── Converters ────────────────────────────────────── */

const ZONE_PAD = 40; /* padding around zone background */

/**
 * Convert ELK output + cluster summaries into React Flow nodes,
 * including zone background panels.
 */
export function elkToFlowNodes(
  elkResult: ElkLayoutResult,
  summaries: ClusterSummary[],
): Node[] {
  const summaryById = new Map(summaries.map(s => [s.id, s]));
  const nodes: Node[] = [];

  /* Compute zone bounding boxes from ELK positions */
  const zoneBounds = new Map<ClusterZone, { minX: number; minY: number; maxX: number; maxY: number }>();
  for (const child of elkResult.children || []) {
    const cluster = summaryById.get(child.id);
    if (!cluster) continue;
    const x = child.x || 0;
    const y = child.y || 0;
    const w = child.width || 280;
    const h = child.height || 220;
    const existing = zoneBounds.get(cluster.zone);
    if (existing) {
      existing.minX = Math.min(existing.minX, x);
      existing.minY = Math.min(existing.minY, y);
      existing.maxX = Math.max(existing.maxX, x + w);
      existing.maxY = Math.max(existing.maxY, y + h);
    } else {
      zoneBounds.set(cluster.zone, { minX: x, minY: y, maxX: x + w, maxY: y + h });
    }
  }

  /* Count non-shared zones */
  const meaningfulZones = Array.from(zoneBounds.keys()).filter(z => z !== 'shared');
  const showZones = meaningfulZones.length >= 2 || (meaningfulZones.length === 1 && zoneBounds.size >= 2);

  /* Zone background panels — rendered BEFORE cards so they sit behind */
  if (showZones) {
    for (const [zone, bounds] of zoneBounds) {
      const meta = ZONE_META[zone];
      const w = bounds.maxX - bounds.minX + ZONE_PAD * 2;
      const h = bounds.maxY - bounds.minY + ZONE_PAD * 2 + 28; /* extra 28 for label */
      nodes.push({
        id: `zone-bg-${zone}`,
        type: 'zoneLabel',
        position: {
          x: bounds.minX - ZONE_PAD,
          y: bounds.minY - ZONE_PAD - 28,
        },
        data: { zone, label: meta.label, color: meta.color, width: w, height: h } as ZoneLabelData,
        selectable: false,
        draggable: false,
        zIndex: -1,
      });
    }
  }

  /* Cluster card nodes */
  for (const child of elkResult.children || []) {
    const cluster = summaryById.get(child.id);
    if (!cluster) continue;
    nodes.push({
      id: child.id,
      type: 'clusterCard',
      position: { x: child.x || 0, y: child.y || 0 },
      data: { cluster } as ClusterNodeData,
      zIndex: 1,
    });
  }

  return nodes;
}

/**
 * Convert cross-cluster edges into React Flow edges.
 */
export function crossEdgesToFlowEdges(
  crossEdges: CrossClusterEdge[],
): Edge[] {
  return crossEdges.map((e, i) => ({
    id: `flow-edge-${i}`,
    source: e.source,
    target: e.target,
    type: 'animatedFlow',
    data: {
      weight: e.weight,
      types: Array.from(e.types),
    },
  }));
}
