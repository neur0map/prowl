/**
 * Converts a CodeGraph into ELK-compatible input for hierarchical layout.
 *
 * Aggregates community data into cluster summaries, then builds
 * an ELK graph where each cluster is a node and cross-cluster
 * dependencies are edges.
 */

import type { CodeGraph, GraphNode, GraphRelationship, NodeLabel } from '../core/graph/types';
import {
  DIR_FRONTEND, DIR_BACKEND, DIR_CONFIG, DIR_INFRA, DIR_DOCS,
  CONTENT_BACKEND, CONTENT_FRONTEND,
  FRONTEND_LANGS,
} from './zone-keywords';

/* ── Cluster summary shape ─────────────────────────── */

export type ClusterZone = 'frontend' | 'backend' | 'shared' | 'config' | 'infra' | 'docs';

export interface ClusterSummary {
  id: string;
  name: string;
  fileCount: number;
  functionCount: number;
  files: GraphNode[];
  symbols: GraphNode[];
  topExports: string[];
  inDegree: number;
  outDegree: number;
  languageBreakdown: Map<string, number>;
  primaryLanguage: string;
  complexity: 'low' | 'moderate' | 'high';
  communityIndex: number;
  internalEdges: GraphRelationship[];
  zone: ClusterZone;
}

export interface CrossClusterEdge {
  source: string;
  target: string;
  weight: number;
  types: Set<string>;
}

export interface ElkInput {
  id: string;
  layoutOptions: Record<string, string>;
  children: ElkNode[];
  edges: ElkEdge[];
}

export interface ElkNode {
  id: string;
  width: number;
  height: number;
  labels?: Array<{ text: string }>;
  layoutOptions?: Record<string, string>;
}

export interface ElkEdge {
  id: string;
  sources: string[];
  targets: string[];
}

/* ── Language detection ─────────────────────────────── */

const EXT_TO_LANG: Record<string, string> = {
  '.ts': 'TypeScript', '.tsx': 'TypeScript',
  '.js': 'JavaScript', '.jsx': 'JavaScript',
  '.py': 'Python', '.rs': 'Rust', '.go': 'Go',
  '.java': 'Java', '.rb': 'Ruby', '.css': 'CSS',
  '.scss': 'CSS', '.html': 'HTML', '.vue': 'Vue',
  '.svelte': 'Svelte', '.swift': 'Swift', '.kt': 'Kotlin',
  '.c': 'C', '.cpp': 'C++', '.h': 'C', '.hpp': 'C++',
  /* Non-code files */
  '.md': 'Markdown', '.mdx': 'Markdown', '.markdown': 'Markdown',
  '.txt': 'Text', '.rst': 'reStructuredText', '.adoc': 'AsciiDoc',
  '.json': 'JSON', '.jsonc': 'JSON',
  '.yaml': 'YAML', '.yml': 'YAML',
  '.toml': 'TOML', '.ini': 'INI', '.cfg': 'INI',
  '.xml': 'XML', '.svg': 'XML',
  '.sh': 'Shell', '.bash': 'Shell', '.zsh': 'Shell',
  '.dockerfile': 'Docker', '.prisma': 'Prisma',
  '.graphql': 'GraphQL', '.gql': 'GraphQL',
  '.sql': 'SQL', '.proto': 'Protobuf',
  '.env': 'Env',
};

function detectLanguage(filePath: string): string {
  const dot = filePath.lastIndexOf('.');
  if (dot === -1) return 'Other';
  return EXT_TO_LANG[filePath.substring(dot)] || 'Other';
}

/* ── Cluster naming ────────────────────────────────── */

const STRUCTURAL_LABELS = new Set<NodeLabel>([
  'Project', 'Package', 'Module', 'Folder', 'File', 'Community', 'Process',
]);

const SYMBOL_LABELS = new Set<NodeLabel>([
  'Class', 'Function', 'Method', 'Interface', 'Enum', 'Struct',
  'Const', 'TypeAlias', 'Trait', 'Namespace', 'Constructor',
]);

function deriveClusterName(files: GraphNode[], symbols: GraphNode[], index: number): string {
  /* Collect all file paths — from File nodes and symbol filePaths */
  const allPaths: string[] = [];
  for (const f of files) allPaths.push(f.properties.filePath);
  if (allPaths.length === 0) {
    const seen = new Set<string>();
    for (const s of symbols) {
      if (s.properties.filePath && !seen.has(s.properties.filePath)) {
        seen.add(s.properties.filePath);
        allPaths.push(s.properties.filePath);
      }
    }
  }

  /* Strategy 1: dominant directory (skip generic names like src, lib, core) */
  const SKIP_DIRS = new Set(['src', 'lib', 'core', 'app', 'main', 'dist', 'build', '.']);
  const dirCounts = new Map<string, number>();
  for (const fp of allPaths) {
    const parts = fp.split('/');
    /* Try the deepest non-file segment, then work upwards */
    for (let i = parts.length - 2; i >= 0; i--) {
      const dir = parts[i];
      if (!SKIP_DIRS.has(dir.toLowerCase()) && dir.length > 1) {
        dirCounts.set(dir, (dirCounts.get(dir) || 0) + 1);
        break;
      }
    }
  }
  if (dirCounts.size > 0 && allPaths.length > 0) {
    const sorted = Array.from(dirCounts.entries()).sort((a, b) => b[1] - a[1]);
    const [topDir, topCount] = sorted[0];
    if (topCount / allPaths.length >= 0.4) {
      return titleCase(topDir);
    }
  }

  /* Strategy 2: dominant export name */
  const exported = symbols.filter(s => s.properties.isExported);
  if (exported.length > 0) {
    const first = exported[0].properties.name;
    if (first.length <= 24) return first;
  }

  /* Strategy 3: most common file name stem */
  if (allPaths.length > 0) {
    const lastName = allPaths[0].split('/').pop() || '';
    const stem = lastName.replace(/\.(ts|tsx|js|jsx|py|rs|go|c|cpp|h|hpp|rb|java|kt|swift)$/, '');
    if (stem && stem !== 'index' && stem !== 'mod' && stem !== 'main') return titleCase(stem);
  }

  /* Fallback */
  return `Module ${String.fromCharCode(65 + (index % 26))}`;
}

/**
 * Deduplicate cluster names by appending a qualifier when names collide.
 * Mutates the summaries array in place.
 */
function deduplicateNames(summaries: ClusterSummary[]): void {
  const nameGroups = new Map<string, ClusterSummary[]>();
  for (const s of summaries) {
    const key = s.name.toLowerCase();
    const group = nameGroups.get(key) || [];
    group.push(s);
    nameGroups.set(key, group);
  }

  for (const group of nameGroups.values()) {
    if (group.length <= 1) continue;

    /* Compute a unique qualifier for each member */
    const qualifiers = group.map(s => {
      /* Try 1: top export */
      if (s.topExports.length > 0) return s.topExports[0];

      /* Try 2: distinctive directory from file paths */
      const firstPath = s.files[0]?.properties.filePath
        || s.symbols[0]?.properties.filePath || '';
      if (firstPath) {
        const parts = firstPath.split('/');
        const nameLower = s.name.toLowerCase().replace(/[^a-z0-9]/g, '');
        for (let i = parts.length - 2; i >= 0; i--) {
          const seg = parts[i].toLowerCase().replace(/[^a-z0-9]/g, '');
          if (seg !== nameLower && seg !== 'src' && seg !== 'lib') return parts[i];
        }
        /* Use file stem if nothing else */
        const stem = (parts.pop() || '').replace(/\.[^.]+$/, '');
        if (stem && stem.toLowerCase() !== nameLower) return stem;
      }

      return '';
    });

    /* Check if qualifiers are actually unique within the group */
    const qualLower = qualifiers.map(q => q.toLowerCase());
    const allUnique = new Set(qualLower).size === qualLower.length
      && qualLower.every(q => q !== '');

    if (allUnique) {
      group.forEach((s, i) => { s.name = `${s.name} (${qualifiers[i]})`; });
    } else {
      /* Qualifiers collided — use numeric suffix */
      group.forEach((s, i) => {
        const q = qualifiers[i];
        s.name = q ? `${s.name} (${q} #${i + 1})` : `${s.name} #${i + 1}`;
      });
    }
  }
}

function titleCase(s: string): string {
  return s
    .replace(/[-_]/g, ' ')
    .replace(/([a-z])([A-Z])/g, '$1 $2')
    .split(' ')
    .map(w => w.charAt(0).toUpperCase() + w.slice(1).toLowerCase())
    .join(' ');
}

/* ── Zone classification ──────────────────────────── */

/**
 * Split a camelCase/PascalCase identifier into lowercase words.
 * "getSupabase" → ["get", "supabase"]
 * "onAuthStateChange" → ["on", "auth", "state", "change"]
 */
function splitCamel(name: string): string[] {
  return name
    .replace(/([a-z])([A-Z])/g, '$1 $2')
    .replace(/([A-Z]+)([A-Z][a-z])/g, '$1 $2')
    .toLowerCase()
    .split(/[\s_\-]+/)
    .filter(Boolean);
}

function classifyZone(cluster: {
  files: GraphNode[];
  symbols: GraphNode[];
  primaryLanguage: string;
  name: string;
}): ClusterZone {
  /* Collect all file paths */
  const allPaths: string[] = [];
  for (const f of cluster.files) allPaths.push(f.properties.filePath);
  if (allPaths.length === 0) {
    const seen = new Set<string>();
    for (const s of cluster.symbols) {
      if (s.properties.filePath && !seen.has(s.properties.filePath)) {
        seen.add(s.properties.filePath);
        allPaths.push(s.properties.filePath);
      }
    }
  }

  let feScore = 0;
  let beScore = 0;
  let cfgScore = 0;
  let infraScore = 0;
  let docsScore = 0;

  /* 1. Directory segment scoring (weight 1 each) */
  for (const fp of allPaths) {
    const segments = fp.toLowerCase().split('/');
    for (const seg of segments) {
      if (DIR_FRONTEND.has(seg)) feScore++;
      if (DIR_BACKEND.has(seg)) beScore++;
      if (DIR_CONFIG.has(seg)) cfgScore++;
      if (DIR_INFRA.has(seg)) infraScore++;
      if (DIR_DOCS.has(seg)) docsScore++;
    }
    if (fp.endsWith('.tsx') || fp.endsWith('.vue') || fp.endsWith('.svelte') || fp.endsWith('.jsx')) feScore++;
  }

  /* 2. Frontend language boost */
  if (FRONTEND_LANGS.has(cluster.primaryLanguage)) feScore += 2;

  /* 3. Content-based scoring from symbol names (weight 2 — strongest signal) */
  for (const s of cluster.symbols) {
    const words = splitCamel(s.properties.name);
    let matchedBe = false;
    let matchedFe = false;
    for (const w of words) {
      if (!matchedBe && CONTENT_BACKEND.some(kw => w.includes(kw))) { beScore += 2; matchedBe = true; }
      if (!matchedFe && CONTENT_FRONTEND.some(kw => w.includes(kw))) { feScore += 2; matchedFe = true; }
    }
  }

  /* 4. Cluster name content matching (weight 3 — if the name itself says "supabase" etc.) */
  const nameWords = splitCamel(cluster.name);
  for (const w of nameWords) {
    if (CONTENT_BACKEND.some(kw => w.includes(kw))) { beScore += 3; break; }
  }
  for (const w of nameWords) {
    if (CONTENT_FRONTEND.some(kw => w.includes(kw))) { feScore += 3; break; }
  }
  for (const w of nameWords) {
    if (DIR_CONFIG.has(w)) { cfgScore += 3; break; }
  }
  for (const w of nameWords) {
    if (DIR_INFRA.has(w)) { infraScore += 3; break; }
  }
  for (const w of nameWords) {
    if (DIR_DOCS.has(w)) { docsScore += 3; break; }
  }

  const max = Math.max(feScore, beScore, cfgScore, infraScore, docsScore);
  if (max === 0) return 'shared';
  if (docsScore === max && docsScore >= 2) return 'docs';
  if (cfgScore === max && cfgScore >= 2) return 'config';
  if (infraScore === max && infraScore >= 2) return 'infra';
  if (feScore === max && feScore >= 2) return 'frontend';
  if (beScore === max && beScore >= 2) return 'backend';
  return 'shared';
}

export const ZONE_META: Record<ClusterZone, { label: string; color: string }> = {
  frontend: { label: 'Frontend', color: 'rgba(49, 120, 198, 0.12)' },
  backend:  { label: 'Backend',  color: 'rgba(222, 165, 132, 0.12)' },
  shared:   { label: 'Shared',   color: 'rgba(255, 255, 255, 0.04)' },
  config:   { label: 'Config',   color: 'rgba(184, 144, 64, 0.10)' },
  infra:    { label: 'Infra',    color: 'rgba(176, 80, 80, 0.10)' },
  docs:     { label: 'Docs',     color: 'rgba(120, 170, 120, 0.10)' },
};

/* ── Non-code file classification ─────────────────── */

const DOC_EXTENSIONS = new Set([
  '.md', '.mdx', '.markdown', '.txt', '.rst', '.adoc',
  '.asciidoc', '.wiki', '.org', '.tex', '.rtf',
]);

const CONFIG_FILE_EXTENSIONS = new Set([
  '.json', '.jsonc', '.json5',
  '.yaml', '.yml',
  '.toml',
  '.ini', '.cfg', '.conf',
  '.xml', '.xsl',
  '.properties',
  '.env',
  '.editorconfig', '.browserslistrc',
  '.prettierrc', '.eslintrc', '.babelrc',
  '.prisma', '.graphql', '.gql',
  '.sql',
  '.proto',
]);

const SCRIPT_EXTENSIONS = new Set([
  '.sh', '.bash', '.zsh', '.fish',
  '.ps1', '.bat', '.cmd',
  '.dockerfile',
  '.makefile',
]);

/**
 * Build synthetic clusters for non-code files that don't belong to any
 * Louvain community (docs, config files, scripts, etc.).
 */
function buildNonCodeClusters(
  graph: CodeGraph,
  assignedFileIds: Set<string>,
  startIndex: number,
): ClusterSummary[] {
  const docFiles: GraphNode[] = [];
  const configFiles: GraphNode[] = [];
  const scriptFiles: GraphNode[] = [];

  for (const node of graph.nodes) {
    if (node.label !== 'File') continue;
    if (assignedFileIds.has(node.id)) continue;

    const fp = node.properties.filePath;
    const dot = fp.lastIndexOf('.');
    const ext = dot >= 0 ? fp.substring(dot).toLowerCase() : '';
    /* Also catch Dockerfile, Makefile by name */
    const baseName = fp.split('/').pop()?.toLowerCase() || '';

    if (DOC_EXTENSIONS.has(ext)) {
      docFiles.push(node);
    } else if (CONFIG_FILE_EXTENSIONS.has(ext)
      || baseName.startsWith('.') && ext === '' /* dotfiles */
    ) {
      configFiles.push(node);
    } else if (SCRIPT_EXTENSIONS.has(ext)
      || baseName === 'dockerfile' || baseName === 'makefile'
    ) {
      scriptFiles.push(node);
    }
  }

  const clusters: ClusterSummary[] = [];
  let idx = startIndex;

  if (docFiles.length > 0) {
    clusters.push(makeSyntheticCluster(
      'synth_docs', 'Documentation', docFiles, idx++, 'docs',
    ));
  }

  if (configFiles.length > 0) {
    clusters.push(makeSyntheticCluster(
      'synth_config', 'Config Files', configFiles, idx++, 'config',
    ));
  }

  if (scriptFiles.length > 0) {
    clusters.push(makeSyntheticCluster(
      'synth_scripts', 'Scripts', scriptFiles, idx++, 'infra',
    ));
  }

  return clusters;
}

function makeSyntheticCluster(
  id: string,
  name: string,
  files: GraphNode[],
  communityIndex: number,
  zone: ClusterZone,
): ClusterSummary {
  const langMap = new Map<string, number>();
  for (const f of files) {
    const lang = detectLanguage(f.properties.filePath);
    langMap.set(lang, (langMap.get(lang) || 0) + 1);
  }
  const primary = Array.from(langMap.entries())
    .sort((a, b) => b[1] - a[1])[0]?.[0] || 'Other';

  return {
    id,
    name,
    fileCount: files.length,
    functionCount: 0,
    files,
    symbols: [],
    topExports: [],
    inDegree: 0,
    outDegree: 0,
    languageBreakdown: langMap,
    primaryLanguage: primary,
    complexity: 'low',
    communityIndex,
    internalEdges: [],
    zone,
  };
}

/* ── Main adapter ──────────────────────────────────── */

/**
 * Build cluster summaries from a CodeGraph using MEMBER_OF relationships.
 */
export function buildClusterSummaries(graph: CodeGraph): ClusterSummary[] {
  /* Map each node → its community ID via MEMBER_OF edges */
  const memberOf = new Map<string, string>();
  for (const rel of graph.relationships) {
    if (rel.type === 'MEMBER_OF') {
      memberOf.set(rel.sourceId, rel.targetId);
    }
  }

  /* Build a node-ID lookup */
  const nodeById = new Map<string, GraphNode>();
  for (const n of graph.nodes) nodeById.set(n.id, n);

  /* Map each File → community:
   * 1. File itself has MEMBER_OF → use that directly
   * 2. File's symbols have MEMBER_OF → majority-vote community
   * 3. File shares a directory with files in a community → assign by proximity
   */
  const fileToCommunity = new Map<string, string>();

  /* Pass 1: Direct MEMBER_OF on File nodes */
  for (const node of graph.nodes) {
    if (node.label === 'File' && memberOf.has(node.id)) {
      fileToCommunity.set(node.id, memberOf.get(node.id)!);
    }
  }

  /* Pass 2: Via DEFINES/CONTAINS → symbol MEMBER_OF (majority vote) */
  const fileSymbolComms = new Map<string, Map<string, number>>();
  for (const rel of graph.relationships) {
    if (rel.type === 'DEFINES' || rel.type === 'CONTAINS') {
      const parent = nodeById.get(rel.sourceId);
      const child = nodeById.get(rel.targetId);
      if (parent?.label === 'File' && child && memberOf.has(child.id)) {
        if (!fileToCommunity.has(parent.id)) {
          const comm = memberOf.get(child.id)!;
          const votes = fileSymbolComms.get(parent.id) || new Map<string, number>();
          votes.set(comm, (votes.get(comm) || 0) + 1);
          fileSymbolComms.set(parent.id, votes);
        }
      }
    }
  }
  for (const [fileId, votes] of fileSymbolComms) {
    if (fileToCommunity.has(fileId)) continue;
    /* Pick the community with the most symbols in this file */
    let bestComm = '';
    let bestCount = 0;
    for (const [comm, count] of votes) {
      if (count > bestCount) { bestComm = comm; bestCount = count; }
    }
    if (bestComm) fileToCommunity.set(fileId, bestComm);
  }

  /* Pass 3: Orphan files → assign by directory proximity */
  const orphanFiles: GraphNode[] = [];
  for (const node of graph.nodes) {
    if (node.label === 'File' && !fileToCommunity.has(node.id)) {
      orphanFiles.push(node);
    }
  }
  if (orphanFiles.length > 0) {
    /* Build dir → community map from already-assigned files */
    const dirComm = new Map<string, Map<string, number>>();
    for (const node of graph.nodes) {
      if (node.label !== 'File' || !fileToCommunity.has(node.id)) continue;
      const parts = node.properties.filePath.split('/');
      const dir = parts.slice(0, -1).join('/');
      if (!dir) continue;
      const comm = fileToCommunity.get(node.id)!;
      const counts = dirComm.get(dir) || new Map<string, number>();
      counts.set(comm, (counts.get(comm) || 0) + 1);
      dirComm.set(dir, counts);
    }
    for (const orphan of orphanFiles) {
      const parts = orphan.properties.filePath.split('/');
      const dir = parts.slice(0, -1).join('/');
      const counts = dirComm.get(dir);
      if (counts) {
        let bestComm = '';
        let bestCount = 0;
        for (const [comm, count] of counts) {
          if (count > bestCount) { bestComm = comm; bestCount = count; }
        }
        if (bestComm) fileToCommunity.set(orphan.id, bestComm);
      }
    }
  }

  /* ── File-level coherence ──────────────────────────
   * Louvain assigns symbols individually, so symbols from the same file
   * can end up in different communities.  Force every symbol to follow
   * its parent file's community so we never split a file across clusters.
   */
  const fileIdToComm = new Map(fileToCommunity);          // fileNodeId → commId
  const filePathToComm = new Map<string, string>();        // filePath → commId
  for (const [fileId, comm] of fileToCommunity) {
    const node = nodeById.get(fileId);
    if (node) filePathToComm.set(node.properties.filePath, comm);
  }

  /* Build DEFINES/CONTAINS index: symbolId → parentFileId */
  const symbolToFileId = new Map<string, string>();
  for (const rel of graph.relationships) {
    if (rel.type === 'DEFINES' || rel.type === 'CONTAINS') {
      const parent = nodeById.get(rel.sourceId);
      const child = nodeById.get(rel.targetId);
      if (parent?.label === 'File' && child && SYMBOL_LABELS.has(child.label)) {
        symbolToFileId.set(child.id, parent.id);
      }
    }
  }

  /* Effective community for a symbol: prefer parent file's community,
   * then fall back to its own MEMBER_OF, then try filePath-based lookup. */
  function symbolCommunity(sym: GraphNode): string | undefined {
    const parentFileId = symbolToFileId.get(sym.id);
    if (parentFileId) {
      const fc = fileIdToComm.get(parentFileId);
      if (fc) return fc;
    }
    /* Fallback: match by filePath (covers symbols without DEFINES edge) */
    if (sym.properties.filePath) {
      const fc = filePathToComm.get(sym.properties.filePath);
      if (fc) return fc;
    }
    return memberOf.get(sym.id);
  }

  /* Group files and symbols by community */
  const clusterFiles = new Map<string, GraphNode[]>();
  const clusterSymbols = new Map<string, GraphNode[]>();

  for (const node of graph.nodes) {
    if (node.label === 'File') {
      const comm = fileToCommunity.get(node.id);
      if (comm) {
        const arr = clusterFiles.get(comm) || [];
        arr.push(node);
        clusterFiles.set(comm, arr);
      }
    } else if (SYMBOL_LABELS.has(node.label)) {
      const comm = symbolCommunity(node);
      if (comm) {
        const arr = clusterSymbols.get(comm) || [];
        arr.push(node);
        clusterSymbols.set(comm, arr);
      }
    }
  }

  /* Collect community node IDs */
  const communityNodes = graph.nodes.filter(n => n.label === 'Community');
  if (communityNodes.length === 0) {
    /* No communities detected — put everything in a single cluster */
    const allFiles = graph.nodes.filter(n => n.label === 'File');
    const allSymbols = graph.nodes.filter(n => SYMBOL_LABELS.has(n.label));
    if (allFiles.length === 0) return [];
    const langMap = new Map<string, number>();
    for (const f of allFiles) {
      const lang = detectLanguage(f.properties.filePath);
      langMap.set(lang, (langMap.get(lang) || 0) + 1);
    }
    const primary = Array.from(langMap.entries()).sort((a, b) => b[1] - a[1])[0]?.[0] || 'Other';
    return [{
      id: 'cluster_0',
      name: deriveClusterName(allFiles, allSymbols, 0),
      fileCount: allFiles.length,
      functionCount: allSymbols.filter(s => s.label === 'Function' || s.label === 'Method').length,
      files: allFiles,
      symbols: allSymbols,
      topExports: allSymbols.filter(s => s.properties.isExported).slice(0, 5).map(s => s.properties.name),
      inDegree: 0,
      outDegree: 0,
      languageBreakdown: langMap,
      primaryLanguage: primary,
      complexity: allSymbols.length > 50 ? 'high' : allSymbols.length > 20 ? 'moderate' : 'low',
      communityIndex: 0,
      internalEdges: [],
      zone: 'shared',
    }];
  }

  /* Build summaries for each community */
  const summaries: ClusterSummary[] = [];
  let idx = 0;
  for (const commNode of communityNodes) {
    const files = clusterFiles.get(commNode.id) || [];
    const symbols = clusterSymbols.get(commNode.id) || [];
    if (files.length === 0 && symbols.length === 0) continue;

    /* Language breakdown — derive from files; if none, infer from symbol filePaths */
    const langMap = new Map<string, number>();
    if (files.length > 0) {
      for (const f of files) {
        const lang = detectLanguage(f.properties.filePath);
        langMap.set(lang, (langMap.get(lang) || 0) + 1);
      }
    } else {
      /* No files mapped — infer language from symbol filePaths */
      const seenPaths = new Set<string>();
      for (const s of symbols) {
        const fp = s.properties.filePath;
        if (fp && !seenPaths.has(fp)) {
          seenPaths.add(fp);
          const lang = detectLanguage(fp);
          langMap.set(lang, (langMap.get(lang) || 0) + 1);
        }
      }
    }
    const primary = Array.from(langMap.entries()).sort((a, b) => b[1] - a[1])[0]?.[0] || 'Unknown';

    /* Top exports */
    const exported = symbols.filter(s => s.properties.isExported);
    const topExports = exported.slice(0, 5).map(s => s.properties.name);

    /* Use heuristic label from community node if available */
    const name = commNode.properties.heuristicLabel
      || deriveClusterName(files, symbols, idx);

    /* Internal edges */
    const clusterNodeIds = new Set([...files.map(f => f.id), ...symbols.map(s => s.id)]);
    const internal = graph.relationships.filter(
      r => clusterNodeIds.has(r.sourceId) && clusterNodeIds.has(r.targetId)
        && r.type !== 'MEMBER_OF' && r.type !== 'STEP_IN_PROCESS'
    );

    const funcCount = symbols.filter(s => s.label === 'Function' || s.label === 'Method').length;
    const complexity: 'low' | 'moderate' | 'high' =
      funcCount > 30 ? 'high' : funcCount > 10 ? 'moderate' : 'low';

    const zone = classifyZone({ files, symbols, primaryLanguage: primary, name });

    summaries.push({
      id: commNode.id,
      name,
      fileCount: files.length,
      functionCount: funcCount,
      files,
      symbols,
      topExports,
      inDegree: 0,
      outDegree: 0,
      languageBreakdown: langMap,
      primaryLanguage: primary,
      complexity,
      communityIndex: idx,
      internalEdges: internal,
      zone,
    });
    idx++;
  }

  /* ── Collect unassigned non-code files into synthetic clusters ── */
  const assignedFileIds = new Set<string>();
  for (const s of summaries) {
    for (const f of s.files) assignedFileIds.add(f.id);
  }
  const syntheticClusters = buildNonCodeClusters(graph, assignedFileIds, idx);
  summaries.push(...syntheticClusters);

  /* Deduplicate cluster names — append qualifier when names collide */
  deduplicateNames(summaries);

  /* Compute cross-cluster edges and in/out degree */
  const nodeToCluster = new Map<string, string>();
  for (const s of summaries) {
    for (const f of s.files) nodeToCluster.set(f.id, s.id);
    for (const sym of s.symbols) nodeToCluster.set(sym.id, s.id);
  }

  const crossEdges = new Map<string, { weight: number; types: Set<string> }>();
  for (const rel of graph.relationships) {
    if (rel.type === 'MEMBER_OF' || rel.type === 'STEP_IN_PROCESS' || rel.type === 'CONTAINS' || rel.type === 'DEFINES') continue;
    const srcCluster = nodeToCluster.get(rel.sourceId);
    const dstCluster = nodeToCluster.get(rel.targetId);
    if (srcCluster && dstCluster && srcCluster !== dstCluster) {
      const key = `${srcCluster}|${dstCluster}`;
      const existing = crossEdges.get(key);
      if (existing) {
        existing.weight++;
        existing.types.add(rel.type);
      } else {
        crossEdges.set(key, { weight: 1, types: new Set([rel.type]) });
      }
    }
  }

  const summaryById = new Map(summaries.map(s => [s.id, s]));
  for (const [key, data] of crossEdges) {
    const [srcId, dstId] = key.split('|');
    const src = summaryById.get(srcId);
    const dst = summaryById.get(dstId);
    if (src) src.outDegree += data.weight;
    if (dst) dst.inDegree += data.weight;
  }

  return summaries;
}

/**
 * Build cross-cluster edge list from a CodeGraph.
 */
export function buildCrossClusterEdges(
  graph: CodeGraph,
  summaries: ClusterSummary[],
): CrossClusterEdge[] {
  const nodeToCluster = new Map<string, string>();
  for (const s of summaries) {
    for (const f of s.files) nodeToCluster.set(f.id, s.id);
    for (const sym of s.symbols) nodeToCluster.set(sym.id, s.id);
  }

  const edgeMap = new Map<string, CrossClusterEdge>();
  for (const rel of graph.relationships) {
    if (rel.type === 'MEMBER_OF' || rel.type === 'STEP_IN_PROCESS' || rel.type === 'CONTAINS' || rel.type === 'DEFINES') continue;
    const src = nodeToCluster.get(rel.sourceId);
    const dst = nodeToCluster.get(rel.targetId);
    if (src && dst && src !== dst) {
      const key = `${src}|${dst}`;
      const existing = edgeMap.get(key);
      if (existing) {
        existing.weight++;
        existing.types.add(rel.type);
      } else {
        edgeMap.set(key, { source: src, target: dst, weight: 1, types: new Set([rel.type]) });
      }
    }
  }

  return Array.from(edgeMap.values());
}

/* ── Card dimensions ───────────────────────────────── */

const CARD_WIDTH = 280;
const CARD_HEIGHT = 220;
const CARD_GAP_X = 48;
const CARD_GAP_Y = 48;
const ZONE_GAP = 160;

/* Zone display order (left → right) */
const ZONE_ORDER: ClusterZone[] = ['frontend', 'shared', 'config', 'docs', 'backend', 'infra'];

/* ── Zone-grid layout result (same shape as ELK output) ── */

export interface GridLayoutResult {
  children: Array<{ id: string; x: number; y: number; width: number; height: number }>;
}

/**
 * Arrange a list of clusters into a grid starting at (offsetX, offsetY).
 */
function layoutGrid(
  clusters: ClusterSummary[],
  offsetX: number,
  offsetY: number,
): GridLayoutResult {
  const n = clusters.length;
  const cols = n <= 2 ? n : Math.min(4, Math.ceil(Math.sqrt(n)));
  return {
    children: clusters.map((s, i) => ({
      id: s.id,
      x: offsetX + (i % cols) * (CARD_WIDTH + CARD_GAP_X),
      y: offsetY + Math.floor(i / cols) * (CARD_HEIGHT + CARD_GAP_Y),
      width: CARD_WIDTH,
      height: CARD_HEIGHT,
    })),
  };
}

/**
 * Compute zone-based grid layout.
 *
 * Clusters are grouped by zone, sorted by connectivity within each zone,
 * and arranged in compact grids. Zones are placed side-by-side with a
 * visible gap so frontend/backend/shared sections are clearly separated.
 */
export function computeZoneGridLayout(summaries: ClusterSummary[]): GridLayoutResult {
  /* Group by zone */
  const zoneGroups = new Map<ClusterZone, ClusterSummary[]>();
  for (const s of summaries) {
    const group = zoneGroups.get(s.zone) || [];
    group.push(s);
    zoneGroups.set(s.zone, group);
  }

  /* Sort within each zone: most connected first (top-left) */
  for (const group of zoneGroups.values()) {
    group.sort((a, b) => (b.inDegree + b.outDegree) - (a.inDegree + a.outDegree));
  }

  /* Decide if multi-zone layout is appropriate */
  const meaningfulZones = Array.from(zoneGroups.keys()).filter(z => z !== 'shared');
  const useZones = meaningfulZones.length >= 2
    || (meaningfulZones.length === 1 && zoneGroups.size >= 2);

  if (!useZones) {
    /* Flat grid — no zone separation */
    const all = summaries
      .slice()
      .sort((a, b) => (b.inDegree + b.outDegree) - (a.inDegree + a.outDegree));
    return layoutGrid(all, 0, 0);
  }

  /* Multi-zone: place zones left-to-right in ZONE_ORDER */
  const children: GridLayoutResult['children'] = [];
  let zoneX = 0;

  for (const zone of ZONE_ORDER) {
    const group = zoneGroups.get(zone);
    if (!group || group.length === 0) continue;

    const gridResult = layoutGrid(group, zoneX, 0);
    children.push(...gridResult.children);

    /* Advance cursor past this zone */
    const maxX = gridResult.children.reduce(
      (max, c) => Math.max(max, c.x + c.width),
      zoneX,
    );
    zoneX = maxX + ZONE_GAP;
  }

  return { children };
}
