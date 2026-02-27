/**
 * Detects execution flows by ranking entry points, tracing
 * forward CALLS edges via bounded BFS, pruning overlapping
 * traces, and emitting Process nodes with step metadata.
 */

import type { CodeGraph, GraphNode, GraphRelationship, NodeLabel } from '../graph/types';
import type { CommunityMembership } from './community-processor';
import { calculateEntryPointScore, isTestFile } from './entry-point-scoring';

/* ── Tuning knobs ─────────────────────────────────────── */

export interface ProcessDetectionConfig {
  maxTraceDepth: number;
  maxBranching: number;
  maxProcesses: number;
  minSteps: number;
}

const DEFAULTS: ProcessDetectionConfig = {
  maxTraceDepth: 10,
  maxBranching: 4,
  maxProcesses: 75,
  minSteps: 2,
};

/* ── Public result shapes ─────────────────────────────── */

export interface ProcessNode {
  id: string;
  label: string;
  heuristicLabel: string;
  processType: 'intra_community' | 'cross_community';
  stepCount: number;
  communities: string[];
  entryPointId: string;
  terminalId: string;
  trace: string[];
}

export interface ProcessStep {
  nodeId: string;
  processId: string;
  step: number;
}

export interface ProcessDetectionResult {
  processes: ProcessNode[];
  steps: ProcessStep[];
  stats: {
    totalProcesses: number;
    crossCommunityCount: number;
    avgStepCount: number;
    entryPointsFound: number;
  };
}

/* ── Adjacency construction ───────────────────────────── */

type AdjList = Map<string, string[]>;

function buildForwardAdj(cg: CodeGraph): AdjList {
  const adj: AdjList = new Map();
  for (const r of cg.relationships) {
    if (r.type !== 'CALLS') continue;
    const list = adj.get(r.sourceId);
    if (list) list.push(r.targetId);
    else adj.set(r.sourceId, [r.targetId]);
  }
  return adj;
}

function buildReverseAdj(cg: CodeGraph): AdjList {
  const adj: AdjList = new Map();
  for (const r of cg.relationships) {
    if (r.type !== 'CALLS') continue;
    const list = adj.get(r.targetId);
    if (list) list.push(r.sourceId);
    else adj.set(r.targetId, [r.sourceId]);
  }
  return adj;
}

/* ── Entry-point ranking ──────────────────────────────── */

function rankSeeds(cg: CodeGraph, fwd: AdjList, rev: AdjList): string[] {
  const eligible = new Set<NodeLabel>(['Function', 'Method']);
  const scored: Array<{ id: string; value: number }> = [];

  for (const n of cg.nodes) {
    if (!eligible.has(n.label)) continue;
    const fp = n.properties.filePath ?? '';
    if (isTestFile(fp)) continue;

    const out = fwd.get(n.id) ?? [];
    if (out.length === 0) continue;
    const inc = rev.get(n.id) ?? [];

    const { score } = calculateEntryPointScore(
      n.properties.name,
      n.properties.language ?? 'javascript',
      n.properties.isExported ?? false,
      inc.length,
      out.length,
      fp,
    );
    if (score > 0) scored.push({ id: n.id, value: score });
  }

  scored.sort((a, b) => b.value - a.value);
  return scored.slice(0, 200).map(s => s.id);
}

/* ── BFS trace ────────────────────────────────────────── */

function traceForward(
  origin: string,
  fwd: AdjList,
  cfg: ProcessDetectionConfig,
): string[][] {
  const paths: string[][] = [];
  const queue: Array<[string, string[]]> = [[origin, [origin]]];

  while (queue.length > 0 && paths.length < cfg.maxBranching * 3) {
    const [cur, trail] = queue.shift()!;
    const next = fwd.get(cur) ?? [];

    if (next.length === 0 || trail.length >= cfg.maxTraceDepth) {
      if (trail.length >= cfg.minSteps) paths.push([...trail]);
      continue;
    }

    let extended = false;
    for (const tgt of next.slice(0, cfg.maxBranching)) {
      if (!trail.includes(tgt)) {
        queue.push([tgt, [...trail, tgt]]);
        extended = true;
      }
    }
    if (!extended && trail.length >= cfg.minSteps) paths.push([...trail]);
  }

  return paths;
}

/* ── Deduplication ────────────────────────────────────── */

function pruneSubsumed(raw: string[][]): string[][] {
  if (raw.length === 0) return [];
  const sorted = [...raw].sort((a, b) => b.length - a.length);
  const kept: string[][] = [];

  for (const candidate of sorted) {
    const sig = candidate.join('->');
    if (!kept.some(k => k.join('->').includes(sig))) kept.push(candidate);
  }
  return kept;
}

/* ── String helpers ───────────────────────────────────── */

function capitalize(s: string): string {
  return s.length === 0 ? s : s[0].toUpperCase() + s.substring(1);
}

function safeId(s: string): string {
  return s.replace(/[^a-zA-Z0-9]/g, '_').substring(0, 20).toLowerCase();
}

/* ── Main processor ───────────────────────────────────── */

export async function processProcesses(
  codeGraph: CodeGraph,
  memberships: CommunityMembership[],
  onProgress?: (message: string, progress: number) => void,
  config: Partial<ProcessDetectionConfig> = {},
): Promise<ProcessDetectionResult> {
  const cfg: ProcessDetectionConfig = { ...DEFAULTS, ...config };

  onProgress?.('Ranking entry points...', 0);

  const commOf = new Map<string, string>();
  for (const m of memberships) commOf.set(m.nodeId, m.communityId);

  const fwd = buildForwardAdj(codeGraph);
  const rev = buildReverseAdj(codeGraph);
  const nodeIndex = new Map<string, GraphNode>();
  for (const n of codeGraph.nodes) nodeIndex.set(n.id, n);

  const seeds = rankSeeds(codeGraph, fwd, rev);
  onProgress?.(`Tracing flows from ${seeds.length} seeds...`, 20);

  /* Collect traces */
  const rawTraces: string[][] = [];
  const cap = cfg.maxProcesses * 2;

  for (let i = 0; i < seeds.length && rawTraces.length < cap; i++) {
    for (const p of traceForward(seeds[i], fwd, cfg)) {
      if (p.length >= cfg.minSteps) rawTraces.push(p);
    }
    if (i % 10 === 0) {
      onProgress?.(`Traced seed ${i + 1}/${seeds.length}...`, 20 + (i / seeds.length) * 40);
    }
  }

  onProgress?.(`Pruning ${rawTraces.length} traces...`, 60);

  const unique = pruneSubsumed(rawTraces)
    .sort((a, b) => b.length - a.length)
    .slice(0, cfg.maxProcesses);

  onProgress?.(`Building ${unique.length} process nodes...`, 80);

  /* Assemble output */
  const processes: ProcessNode[] = [];
  const steps: ProcessStep[] = [];

  for (let seq = 0; seq < unique.length; seq++) {
    const trace = unique[seq];
    const headId = trace[0];
    const tailId = trace[trace.length - 1];

    const comms = new Set<string>();
    for (const nid of trace) {
      const c = commOf.get(nid);
      if (c) comms.add(c);
    }
    const commList = [...comms];
    const kind: ProcessNode['processType'] = commList.length > 1 ? 'cross_community' : 'intra_community';

    const headName = nodeIndex.get(headId)?.properties.name ?? 'Unknown';
    const tailName = nodeIndex.get(tailId)?.properties.name ?? 'Unknown';
    const tag = `${capitalize(headName)} \u2192 ${capitalize(tailName)}`;
    const pid = `proc_${seq}_${safeId(headName)}`;

    processes.push({
      id: pid,
      label: tag,
      heuristicLabel: tag,
      processType: kind,
      stepCount: trace.length,
      communities: commList,
      entryPointId: headId,
      terminalId: tailId,
      trace,
    });

    for (let s = 0; s < trace.length; s++) {
      steps.push({ nodeId: trace[s], processId: pid, step: s + 1 });
    }
  }

  onProgress?.('Flow detection done.', 100);

  let crossN = 0;
  let totalSteps = 0;
  for (const p of processes) {
    if (p.processType === 'cross_community') crossN++;
    totalSteps += p.stepCount;
  }

  return {
    processes,
    steps,
    stats: {
      totalProcesses: processes.length,
      crossCommunityCount: crossN,
      avgStepCount: processes.length > 0 ? Math.round((totalSteps / processes.length) * 10) / 10 : 0,
      entryPointsFound: seeds.length,
    },
  };
}
