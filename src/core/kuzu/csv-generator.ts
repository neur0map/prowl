/**
 * Generates RFC 4180 CSV payloads for KuzuDB bulk import.
 * One CSV per node table plus a single edge CSV.
 * All text values are unconditionally double-quoted so
 * embedded source code never breaks the field boundary.
 */

import { CodeGraph, GraphNode } from '../graph/types';
import { NODE_TABLES, NodeTableName } from './schema';

/* ── Field-level utilities ───────────────────────────── */

/* Strip control characters and broken surrogate pairs from a field */
function sanitizeField(raw: string): string {
  let out = raw.replace(/\r\n/g, '\n');
  out = out.replace(/\r/g, '\n');
  out = out.replace(/[\x00-\x08\x0B\x0C\x0E-\x1F\x7F]/g, '');
  out = out.replace(/[\uD800-\uDFFF]/g, '');
  out = out.replace(/[\uFFFE\uFFFF]/g, '');
  return out;
}

/* Wrap a value in double-quotes, escaping inner quotes per RFC 4180 */
function csvQuote(val: string | number | undefined | null): string {
  if (val === undefined || val === null) return '""';
  const safe = sanitizeField(String(val));
  return '"' + safe.replace(/"/g, '""') + '"';
}

/* Emit an unquoted numeric literal, falling back when missing */
function numVal(val: number | undefined | null, fallback: number = -1): string {
  return (val === undefined || val === null) ? String(fallback) : String(val);
}

/* ── Content extraction ──────────────────────────────── */

/* Check whether the leading bytes look like binary rather than text */
function isBinaryContent(text: string): boolean {
  if (!text || text.length === 0) return false;
  const probe = text.slice(0, 1000);
  let ctrl = 0;
  for (let i = 0; i < probe.length; i++) {
    const ch = probe.charCodeAt(i);
    if ((ch < 9) || (ch > 13 && ch < 32) || ch === 127) ctrl++;
  }
  return ctrl / probe.length > 0.1;
}

/* Pull source text for a node, applying per-label caps */
function extractSourceText(
  entry: GraphNode,
  sourceMap: Map<string, string>,
): string {
  const fPath = entry.properties.filePath;
  const src = sourceMap.get(fPath);

  if (!src) return '';
  if (entry.label === 'Folder') return '';
  if (isBinaryContent(src)) return '[Binary file - content not stored]';

  if (entry.label === 'File') {
    const CAP = 10000;
    return src.length > CAP
      ? src.slice(0, CAP) + '\n... [truncated]'
      : src;
  }

  const first = entry.properties.startLine;
  const last = entry.properties.endLine;
  if (first === undefined || last === undefined) return '';

  const lines = src.split('\n');
  const PAD = 2;
  const lo = Math.max(0, first - PAD);
  const hi = Math.min(lines.length - 1, last + PAD);
  const fragment = lines.slice(lo, hi + 1).join('\n');

  const LIMIT = 5000;
  return fragment.length > LIMIT
    ? fragment.slice(0, LIMIT) + '\n... [truncated]'
    : fragment;
}

/* ── Public shape ────────────────────────────────────── */

export interface CSVData {
  nodes: Map<NodeTableName, string>;
  relCSV: string;
}

/* Tables that have the isExported column (matches schema DDL) */
const TABLES_WITH_IS_EXPORTED = new Set(['Function', 'Class', 'Interface', 'Method', 'CodeElement']);

/* ── Per-table builders ──────────────────────────────── */

function fileNodeCSV(items: GraphNode[], sourceMap: Map<string, string>): string {
  const rows: string[] = ['id,name,filePath,content'];
  const visited = new Set<string>();
  for (const nd of items) {
    if (nd.label !== 'File') continue;
    if (visited.has(nd.id)) continue;
    visited.add(nd.id);
    const body = extractSourceText(nd, sourceMap);
    rows.push(
      [csvQuote(nd.id), csvQuote(nd.properties.name || ''), csvQuote(nd.properties.filePath || ''), csvQuote(body)].join(','),
    );
  }
  return rows.join('\n');
}

function folderNodeCSV(items: GraphNode[]): string {
  const rows: string[] = ['id,name,filePath'];
  const visited = new Set<string>();
  for (const nd of items) {
    if (nd.label !== 'Folder') continue;
    if (visited.has(nd.id)) continue;
    visited.add(nd.id);
    rows.push(
      [csvQuote(nd.id), csvQuote(nd.properties.name || ''), csvQuote(nd.properties.filePath || '')].join(','),
    );
  }
  return rows.join('\n');
}

function symbolNodeCSV(
  items: GraphNode[],
  kind: string,
  sourceMap: Map<string, string>,
): string {
  const hasExported = TABLES_WITH_IS_EXPORTED.has(kind);
  const header = hasExported
    ? 'id,name,filePath,startLine,endLine,isExported,content'
    : 'id,name,filePath,startLine,endLine,content';
  const rows: string[] = [header];
  const visited = new Set<string>();
  for (const nd of items) {
    if (nd.label !== kind) continue;
    if (visited.has(nd.id)) continue;
    visited.add(nd.id);
    const body = extractSourceText(nd, sourceMap);
    const fields = [
      csvQuote(nd.id),
      csvQuote(nd.properties.name || ''),
      csvQuote(nd.properties.filePath || ''),
      numVal(nd.properties.startLine, -1),
      numVal(nd.properties.endLine, -1),
    ];
    if (hasExported) {
      rows.push([...fields, nd.properties.isExported ? 'true' : 'false', csvQuote(body)].join(','));
    } else {
      rows.push([...fields, csvQuote(body)].join(','));
    }
  }
  return rows.join('\n');
}

function communityNodeCSV(items: GraphNode[]): string {
  const rows: string[] = ['id,label,heuristicLabel,keywords,description,enrichedBy,cohesion,symbolCount'];
  const visited = new Set<string>();
  for (const nd of items) {
    if (nd.label !== 'Community') continue;
    if (visited.has(nd.id)) continue;
    visited.add(nd.id);
    const props = nd.properties as any;
    const kws: string[] = props.keywords || [];
    const kwLiteral = '[' + kws.map((k: string) => "'" + k.replace(/'/g, "''") + "'").join(',') + ']';
    rows.push([
      csvQuote(nd.id),
      csvQuote(nd.properties.name || ''),
      csvQuote(nd.properties.heuristicLabel || ''),
      kwLiteral,
      csvQuote(props.description || ''),
      csvQuote(props.enrichedBy || 'heuristic'),
      numVal(nd.properties.cohesion, 0),
      numVal(nd.properties.symbolCount, 0),
    ].join(','));
  }
  return rows.join('\n');
}

function processNodeCSV(items: GraphNode[]): string {
  const rows: string[] = ['id,label,heuristicLabel,processType,stepCount,communities,entryPointId,terminalId'];
  const visited = new Set<string>();
  for (const nd of items) {
    if (nd.label !== 'Process') continue;
    if (visited.has(nd.id)) continue;
    visited.add(nd.id);
    const props = nd.properties as any;
    const comms: string[] = props.communities || [];
    const commLiteral = '[' + comms.map((c: string) => "'" + c.replace(/'/g, "''") + "'").join(',') + ']';
    rows.push([
      csvQuote(nd.id),
      csvQuote(nd.properties.name || ''),
      csvQuote(props.heuristicLabel || ''),
      csvQuote(props.processType || ''),
      numVal(props.stepCount, 0),
      csvQuote(commLiteral),
      csvQuote(props.entryPointId || ''),
      csvQuote(props.terminalId || ''),
    ].join(','));
  }
  return rows.join('\n');
}

function edgeCSV(graph: CodeGraph): string {
  const rows: string[] = ['from,to,type,confidence,reason,step'];
  for (const rel of graph.relationships) {
    rows.push([
      csvQuote(rel.sourceId),
      csvQuote(rel.targetId),
      csvQuote(rel.type),
      numVal(rel.confidence, 1.0),
      csvQuote(rel.reason),
      numVal((rel as any).step, 0),
    ].join(','));
  }
  return rows.join('\n');
}

/* ── Orchestrator ────────────────────────────────────── */

/* Produce CSV payloads for every node table and the edge table */
export function generateAllCSVs(
  graph: CodeGraph,
  fileContents: Map<string, string>,
): CSVData {
  /* Global dedup: the graph may contain multiple node objects with the same ID
     (e.g. during incremental live updates). Keep the first occurrence. */
  const seenIds = new Set<string>();
  const allNodes: GraphNode[] = [];
  for (const nd of graph.nodes) {
    if (seenIds.has(nd.id)) continue;
    seenIds.add(nd.id);
    allNodes.push(nd);
  }

  const nodeCSVs = new Map<NodeTableName, string>();

  nodeCSVs.set('File', fileNodeCSV(allNodes, fileContents));
  nodeCSVs.set('Folder', folderNodeCSV(allNodes));

  const SPECIAL_TABLES = new Set(['File', 'Folder', 'Community', 'Process']);
  const symbolKinds = NODE_TABLES.filter(t => !SPECIAL_TABLES.has(t));
  for (const kind of symbolKinds) {
    nodeCSVs.set(kind, symbolNodeCSV(allNodes, kind, fileContents));
  }

  nodeCSVs.set('Community', communityNodeCSV(allNodes));
  nodeCSVs.set('Process', processNodeCSV(allNodes));

  const rels = edgeCSV(graph);

  return { nodes: nodeCSVs, relCSV: rels };
}
