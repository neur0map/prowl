export interface ProcessStep {
  id: string;
  name: string;
  filePath?: string;
  stepNumber: number;
  cluster?: string;
}

export interface ProcessEdge {
  from: string;
  to: string;
  type: string;
}

export interface ProcessData {
  id: string;
  label: string;
  processType: 'intra_community' | 'cross_community';
  steps: ProcessStep[];
  edges?: ProcessEdge[];
  clusters?: string[];
}

const STYLE_DEFS = [
  'classDef default fill:#1c1c1e,stroke:rgba(255,255,255,0.12),stroke-width:1px,color:#f5f5f7,rx:20,ry:20,font-size:24px;',
  'classDef entry fill:#1c1c1e,stroke:rgba(255,255,255,0.2),stroke-width:2px,color:#f5f5f7,rx:20,ry:20,font-size:24px;',
  'classDef step fill:#1c1c1e,stroke:rgba(255,255,255,0.12),stroke-width:1px,color:#f5f5f7,rx:20,ry:20,font-size:24px;',
  'classDef terminal fill:#1c1c1e,stroke:rgba(255,255,255,0.2),stroke-width:2px,color:#f5f5f7,rx:20,ry:20,font-size:24px;',
  'classDef cluster fill:rgba(255,255,255,0.03),stroke:rgba(255,255,255,0.08),stroke-width:1px,color:rgba(255,255,255,0.35),rx:8,ry:8,font-size:20px;',
];

function safeId(raw: string): string {
  return raw.replace(/[^a-zA-Z0-9_]/g, '_');
}

function truncateLabel(text: string): string {
  const cleaned = text.replace(/["\[\]<>{}()]/g, '');
  return cleaned.length > 30 ? cleaned.slice(0, 30) : cleaned;
}

function fileBasename(fullPath: string | undefined): string {
  if (!fullPath) return '';
  const idx = fullPath.lastIndexOf('/');
  return idx === -1 ? fullPath : fullPath.substring(idx + 1);
}

function classifyStep(
  step: ProcessStep,
  firstId: string,
  lastId: string,
): 'entry' | 'terminal' | 'step' {
  if (step.id === firstId) return 'entry';
  if (step.id === lastId) return 'terminal';
  return 'step';
}

function formatNodeLine(
  step: ProcessStep,
  indent: string,
  firstId: string,
  lastId: string,
): string {
  const mid = safeId(step.id);
  const caption = `${step.stepNumber}. ${truncateLabel(step.name)}`;
  const fname = fileBasename(step.filePath);
  const cls = classifyStep(step, firstId, lastId);
  return `${indent}${mid}["${caption}<br/><small>${fname}</small>"]:::${cls}`;
}

function partitionByClusters(steps: ProcessStep[]) {
  const grouped = new Map<string, ProcessStep[]>();
  const ungrouped: ProcessStep[] = [];
  for (const s of steps) {
    if (s.cluster) {
      let bucket = grouped.get(s.cluster);
      if (!bucket) { bucket = []; grouped.set(s.cluster, bucket); }
      bucket.push(s);
    } else {
      ungrouped.push(s);
    }
  }
  return { grouped, ungrouped };
}

function buildEdgeLines(
  steps: ProcessStep[],
  edges: ProcessEdge[] | undefined,
): string[] {
  const result: string[] = [];

  if (edges && edges.length > 0) {
    const lookup = new Map<string, ProcessStep>();
    for (const s of steps) lookup.set(s.id, s);
    for (const e of edges) {
      const origin = lookup.get(e.from);
      const dest = lookup.get(e.to);
      if (origin && dest) {
        result.push(`  ${safeId(origin.id)} --> ${safeId(dest.id)}`);
      }
    }
  } else {
    const ordered = [...steps].sort((a, b) => a.stepNumber - b.stepNumber);
    for (let k = 0; k < ordered.length - 1; k++) {
      result.push(`  ${safeId(ordered[k].id)} --> ${safeId(ordered[k + 1].id)}`);
    }
  }

  return result;
}

export function generateProcessMermaid(process: ProcessData): string {
  const { steps, edges } = process;

  if (!steps || steps.length === 0) {
    return 'graph TD\n  A[No steps found]';
  }

  const sorted = [...steps].sort((a, b) => a.stepNumber - b.stepNumber);
  const headId = sorted[0].id;
  const tailId = sorted[sorted.length - 1].id;

  const output: string[] = ['graph TD', '  %% Styles'];
  for (const def of STYLE_DEFS) output.push(`  ${def}`);

  const wantSubgraphs =
    process.processType === 'cross_community' &&
    new Set(steps.filter((s) => s.cluster).map((s) => s.cluster)).size > 1;

  if (wantSubgraphs) {
    const { grouped, ungrouped } = partitionByClusters(steps);

    for (const [clusterLabel, members] of grouped) {
      const safe = truncateLabel(clusterLabel);
      output.push(`  subgraph ${safe}["${safe}"]:::cluster`);
      for (const member of members) {
        output.push(formatNodeLine(member, '    ', headId, tailId));
      }
      output.push('  end');
    }

    for (const loose of ungrouped) {
      output.push(formatNodeLine(loose, '  ', headId, tailId));
    }
  } else {
    for (const s of steps) {
      output.push(formatNodeLine(s, '  ', headId, tailId));
    }
  }

  output.push(...buildEdgeLines(steps, edges));

  return output.join('\n');
}

export function generateSimpleMermaid(processLabel: string, stepCount: number): string {
  const halves = processLabel.split(' → ');
  const startName = (halves[0] || 'Start').trim();
  const endName = (halves[1] || 'End').trim();
  const middle = stepCount - 2;

  return [
    'graph LR',
    '  classDef default fill:#1c1c1e,stroke:rgba(255,255,255,0.12),stroke-width:1px,color:#f5f5f7,rx:20,ry:20;',
    '  classDef entry fill:#1c1c1e,stroke:rgba(255,255,255,0.2),stroke-width:2px,color:#f5f5f7,rx:20,ry:20;',
    `  A["${startName}"]:::entry --> B["${middle} steps"] --> C["${endName}"]:::entry`,
  ].join('\n');
}
