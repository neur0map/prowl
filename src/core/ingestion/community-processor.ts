/**
 * Clusters related code symbols into modules via Louvain
 * community detection over CALLS / EXTENDS / IMPLEMENTS edges.
 */

import Graph from 'graphology';
import louvain from '../../lib/louvain';
import type { CodeGraph, NodeLabel } from '../graph/types';

/* ── Visualisation palette ────────────────────────────── */

export const MODULE_PALETTE = [
  '#3a7ef2', '#0eb0cf', '#875af2', '#e84040',
  '#f56f12', '#e5ae04', '#1fbf58', '#d540eb',
  '#e84494', '#f03a59', '#10b3a1', '#80c812',
];

export function getModuleColor(idx: number): string {
  return MODULE_PALETTE[idx % MODULE_PALETTE.length];
}

/* ── Result shapes ────────────────────────────────────── */

export interface CommunityNode {
  id: string;
  label: string;
  heuristicLabel: string;
  cohesion: number;
  symbolCount: number;
}

export interface CommunityMembership {
  nodeId: string;
  communityId: string;
}

export interface CommunityDetectionResult {
  communities: CommunityNode[];
  memberships: CommunityMembership[];
  stats: {
    totalCommunities: number;
    modularity: number;
    nodesProcessed: number;
  };
}

/* ── Internal helpers ─────────────────────────────────── */

/** Longest shared prefix across a list of strings. */
function sharedPrefix(items: string[]): string {
  if (items.length === 0) return '';
  const sorted = [...items].sort();
  const a = sorted[0];
  const b = sorted[sorted.length - 1];
  let n = 0;
  while (n < a.length && a[n] === b[n]) n++;
  return a.slice(0, n);
}

/** Internal edge density (0..1) for a set of nodes. */
function edgeDensity(nodeIds: string[], g: Graph): number {
  const count = nodeIds.length;
  if (count <= 1) return 1.0;

  const members = new Set(nodeIds);
  let pairs = 0;
  for (const nid of nodeIds) {
    if (!g.hasNode(nid)) continue;
    g.forEachNeighbor(nid, adj => { if (members.has(adj)) pairs++; });
  }

  const edges = pairs / 2;
  const maxEdges = (count * (count - 1)) / 2;
  return maxEdges === 0 ? 1.0 : Math.min(1.0, edges / maxEdges);
}

/* Directory names too generic to use as labels */
const NOISE_DIRS = new Set([
  'src', 'lib', 'core', 'utils', 'common', 'shared', 'helpers',
]);

/** Derive a readable label from the file paths and names of cluster members. */
function deriveClusterLabel(
  ids: string[],
  pathOf: Map<string, string>,
  g: Graph,
  seq: number,
): string {
  /* Pick the most frequent parent directory */
  const tally: Record<string, number> = {};
  for (const nid of ids) {
    const fp = pathOf.get(nid) ?? '';
    const parts = fp.split('/').filter(Boolean);
    if (parts.length >= 2) {
      const dir = parts[parts.length - 2];
      if (!NOISE_DIRS.has(dir.toLowerCase())) {
        tally[dir] = (tally[dir] || 0) + 1;
      }
    }
  }

  let best = '';
  let bestN = 0;
  for (const [dir, n] of Object.entries(tally)) {
    if (n > bestN) { bestN = n; best = dir; }
  }
  if (best) return best.charAt(0).toUpperCase() + best.slice(1);

  /* Fall back to shared name prefix */
  const names: string[] = [];
  for (const nid of ids) {
    const nm = g.getNodeAttribute(nid, 'name');
    if (nm) names.push(nm);
  }
  if (names.length > 2) {
    const pfx = sharedPrefix(names);
    if (pfx.length > 2) return pfx.charAt(0).toUpperCase() + pfx.slice(1);
  }

  return `Cluster_${seq}`;
}

/** Project an undirected graphology graph from the code graph. */
function projectUndirected(cg: CodeGraph): Graph {
  const g = new Graph({ type: 'undirected', allowSelfLoops: false });

  const wanted = new Set<NodeLabel>(['Function', 'Class', 'Method', 'Interface']);
  for (const node of cg.nodes) {
    if (wanted.has(node.label)) {
      g.addNode(node.id, {
        name: node.properties.name,
        filePath: node.properties.filePath,
        type: node.label,
      });
    }
  }

  const edgeKinds = new Set(['CALLS', 'EXTENDS', 'IMPLEMENTS']);
  for (const rel of cg.relationships) {
    if (!edgeKinds.has(rel.type)) continue;
    if (rel.sourceId === rel.targetId) continue;
    if (!g.hasNode(rel.sourceId) || !g.hasNode(rel.targetId)) continue;
    if (g.hasEdge(rel.sourceId, rel.targetId)) continue;
    g.addEdge(rel.sourceId, rel.targetId);
  }

  return g;
}

/* ── Entry point ──────────────────────────────────────── */

export async function processCommunities(
  codeGraph: CodeGraph,
  onProgress?: (message: string, progress: number) => void,
): Promise<CommunityDetectionResult> {
  onProgress?.('Projecting graph for clustering...', 0);

  const ug = projectUndirected(codeGraph);

  if (ug.order === 0) {
    return {
      communities: [],
      memberships: [],
      stats: { totalCommunities: 0, modularity: 0, nodesProcessed: 0 },
    };
  }

  onProgress?.(`Clustering ${ug.order} symbols...`, 30);

  const result = louvain.detailed(ug, { resolution: 1.0 });

  onProgress?.(`Detected ${result.count} clusters...`, 60);

  /* Build a path lookup for labelling */
  const pathOf = new Map<string, string>();
  for (const n of codeGraph.nodes) {
    if (n.properties.filePath) pathOf.set(n.id, n.properties.filePath);
  }

  /* Bucket nodes by cluster index */
  const buckets = new Map<number, string[]>();
  const assign = result.communities as Record<string, number>;
  for (const [nid, idx] of Object.entries(assign)) {
    const arr = buckets.get(idx);
    if (arr) arr.push(nid);
    else buckets.set(idx, [nid]);
  }

  /* Create CommunityNode entries (skip singletons) */
  const comms: CommunityNode[] = [];
  buckets.forEach((ids, idx) => {
    if (ids.length < 2) return;
    const tag = deriveClusterLabel(ids, pathOf, ug, idx);
    comms.push({
      id: `comm_${idx}`,
      label: tag,
      heuristicLabel: tag,
      cohesion: edgeDensity(ids, ug),
      symbolCount: ids.length,
    });
  });

  comms.sort((a, b) => b.symbolCount - a.symbolCount);

  onProgress?.('Assembling membership edges...', 80);

  const memberships: CommunityMembership[] = Object.entries(assign).map(
    ([nodeId, idx]) => ({ nodeId, communityId: `comm_${idx}` }),
  );

  onProgress?.('Clustering complete.', 100);

  return {
    communities: comms,
    memberships,
    stats: {
      totalCommunities: result.count,
      modularity: result.modularity,
      nodesProcessed: ug.order,
    },
  };
}
