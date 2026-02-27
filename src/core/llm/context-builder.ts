/**
 * Assembles project-level context (symbol counts, high-connectivity
 * hotspots, directory tree) and injects it into the system prompt
 * so the LLM operates with structural awareness.
 */

/* ── Exported shapes ─────────────────────────────────── */

export interface CodebaseStats {
  projectName: string;
  fileCount: number;
  functionCount: number;
  classCount: number;
  interfaceCount: number;
  methodCount: number;
}

export interface Hotspot {
  name: string;
  type: string;
  filePath: string;
  connections: number;
}

export interface ProjectContext {
  stats: CodebaseStats;
  hotspots: Hotspot[];
  folderTree: string;
}

/* ── Internal shapes ─────────────────────────────────── */

interface DirNode {
  isLeaf: boolean;
  subtree: Map<string, DirNode>;
  descendantFiles: number;
}

/* ── Helpers ──────────────────────────────────────────── */

/* Read numeric count from a named-property or positional result row */
function readCount(row: any): number {
  if (Array.isArray(row)) return row[0] ?? 0;
  return row?.count ?? 0;
}

const TALLY_QUERIES: ReadonlyArray<{ key: string; cypher: string }> = [
  { key: 'files',      cypher: 'MATCH (n:File) RETURN COUNT(n) AS count' },
  { key: 'functions',  cypher: 'MATCH (n:Function) RETURN COUNT(n) AS count' },
  { key: 'classes',    cypher: 'MATCH (n:Class) RETURN COUNT(n) AS count' },
  { key: 'interfaces', cypher: 'MATCH (n:Interface) RETURN COUNT(n) AS count' },
  { key: 'methods',    cypher: 'MATCH (n:Method) RETURN COUNT(n) AS count' },
];

/* ── Directory tree construction ─────────────────────── */

function mkDirNode(leaf: boolean): DirNode {
  return { isLeaf: leaf, subtree: new Map(), descendantFiles: 0 };
}

/* Register a file path in the tree, incrementing ancestor counters */
function addPathToTree(root: DirNode, filePath: string): void {
  const segments = filePath.replace(/\\/g, '/').split('/').filter(Boolean);
  let cursor = root;

  for (let idx = 0; idx < segments.length; idx++) {
    const seg = segments[idx];
    const leaf = idx === segments.length - 1;

    if (!cursor.subtree.has(seg)) {
      cursor.subtree.set(seg, mkDirNode(leaf));
    }

    const next = cursor.subtree.get(seg)!;
    if (leaf) {
      let ancestor = root;
      for (let j = 0; j < idx; j++) {
        ancestor = ancestor.subtree.get(segments[j])!;
        ancestor.descendantFiles++;
      }
    }
    cursor = next;
  }
}

/* Recursively render tree nodes as indented text, collapsing deep dirs */
function drawTree(
  node: DirNode,
  prefix: string,
  depth: number,
  depthCap: number,
  out: string[],
): void {
  const items = [...node.subtree.entries()];

  items.sort(([nameA, a], [nameB, b]) => {
    if (a.isLeaf !== b.isLeaf) return a.isLeaf ? 1 : -1;
    if (!a.isLeaf && !b.isLeaf) return b.descendantFiles - a.descendantFiles;
    return nameA.localeCompare(nameB);
  });

  for (const [name, child] of items) {
    if (child.isLeaf) {
      out.push(`${prefix}${name}`);
    } else if (depth >= depthCap) {
      out.push(`${prefix}${name}/ (${child.descendantFiles} files)`);
    } else {
      out.push(`${prefix}${name}/`);
      drawTree(child, prefix + '  ', depth + 1, depthCap, out);
    }
  }
}

/* Convert a flat path list into an indented directory listing */
function buildDirTree(paths: string[], depthCap: number): string {
  const root = mkDirNode(false);
  for (const p of paths) addPathToTree(root, p);

  const lines: string[] = [];
  drawTree(root, '', 0, depthCap, lines);
  return lines.join('\n');
}

/* ── Legacy tree helpers (kept for internal use) ─────── */

function treeFromPaths(paths: string[], maxDepth: number): Map<string, any> {
  const root = new Map<string, any>();

  for (const fullPath of paths) {
    const normalized = fullPath.replace(/\\/g, '/');
    const parts = normalized.split('/').filter(Boolean);
    let cursor = root;
    const limit = Math.min(parts.length, maxDepth + 1);

    for (let i = 0; i < limit; i++) {
      const segment = parts[i];
      const leaf = i === parts.length - 1;

      if (!cursor.has(segment)) {
        cursor.set(segment, leaf ? null : new Map<string, any>());
      }

      const child = cursor.get(segment);
      if (child instanceof Map) {
        cursor = child;
      } else {
        break;
      }
    }
  }

  return root;
}

function countChildren(node: Map<string, any>): number {
  let total = 0;
  node.forEach((val) => {
    total += val instanceof Map ? 1 + countChildren(val) : 1;
  });
  return total;
}

function asciiTree(
  tree: Map<string, any>,
  prefix: string,
  isLast: boolean = true,
): string {
  const entries = [...tree.entries()];

  entries.sort(([aKey, aVal], [bKey, bVal]) => {
    const aDir = aVal instanceof Map;
    const bDir = bVal instanceof Map;
    if (aDir !== bDir) return bDir ? 1 : -1;
    return aKey.localeCompare(bKey);
  });

  const fragments: string[] = [];

  for (let pos = 0; pos < entries.length; pos++) {
    const [label, subtree] = entries[pos];
    const last = pos === entries.length - 1;
    const branch = last ? '\u2514\u2500\u2500 ' : '\u251C\u2500\u2500 ';
    const nextPrefix = prefix + (last ? '    ' : '\u2502   ');

    if (subtree instanceof Map && subtree.size > 0) {
      const desc = countChildren(subtree);
      const suffix = desc > 3 ? ` (${desc} items)` : '';
      fragments.push(`${prefix}${branch}${label}/${suffix}`);
      fragments.push(asciiTree(subtree, nextPrefix, last));
    } else if (subtree instanceof Map) {
      fragments.push(`${prefix}${branch}${label}/`);
    } else {
      fragments.push(`${prefix}${branch}${label}`);
    }
  }

  return fragments.filter(Boolean).join('\n');
}

/* ── Exported query functions ────────────────────────── */

export async function getCodebaseStats(
  executeQuery: (cypher: string) => Promise<any[]>,
  projectName: string,
): Promise<CodebaseStats> {
  try {
    const tally: Record<string, number> = {};

    await Promise.all(
      TALLY_QUERIES.map(async ({ key, cypher }) => {
        try {
          const rows = await executeQuery(cypher);
          tally[key] = readCount(rows[0]);
        } catch {
          tally[key] = 0;
        }
      }),
    );

    return {
      projectName,
      fileCount: tally.files,
      functionCount: tally.functions,
      classCount: tally.classes,
      interfaceCount: tally.interfaces,
      methodCount: tally.methods,
    };
  } catch (err) {
    console.error('Failed to gather codebase statistics:', err);
    return {
      projectName,
      fileCount: 0,
      functionCount: 0,
      classCount: 0,
      interfaceCount: 0,
      methodCount: 0,
    };
  }
}

export async function getHotspots(
  executeQuery: (cypher: string) => Promise<any[]>,
  limit: number = 8,
): Promise<Hotspot[]> {
  try {
    const cypher = `
      MATCH (n)-[r:CodeEdge]-(m)
      WHERE n.name IS NOT NULL
      WITH n, COUNT(r) AS connections
      ORDER BY connections DESC
      LIMIT ${limit}
      RETURN n.name AS name, LABEL(n) AS type, n.filePath AS filePath, connections
    `;

    const rows = await executeQuery(cypher);

    return rows.reduce<Hotspot[]>((acc, row) => {
      const h: Hotspot = Array.isArray(row)
        ? { name: row[0], type: row[1], filePath: row[2], connections: row[3] }
        : { name: row.name, type: row.type, filePath: row.filePath, connections: row.connections };

      if (h.name && h.type) acc.push(h);
      return acc;
    }, []);
  } catch (err) {
    console.error('Failed to fetch hotspots:', err);
    return [];
  }
}

/* Build an indented directory tree from file paths stored in the graph */
export async function getFolderTree(
  executeQuery: (cypher: string) => Promise<any[]>,
  maxDepth: number = 10,
): Promise<string> {
  try {
    const cypher = 'MATCH (f:File) RETURN f.filePath AS path ORDER BY path';
    const rows = await executeQuery(cypher);

    const paths: string[] = rows.reduce<string[]>((acc, row) => {
      const p = Array.isArray(row) ? row[0] : row.path;
      if (p) acc.push(p);
      return acc;
    }, []);

    if (paths.length === 0) return '';

    return buildDirTree(paths, maxDepth);
  } catch (err) {
    console.error('Failed to build folder tree:', err);
    return '';
  }
}

/* Collect stats, hotspots, and directory tree in one call */
export async function buildProjectContext(
  executeQuery: (cypher: string) => Promise<any[]>,
  projectName: string,
): Promise<ProjectContext> {
  const [stats, hotspots, folderTree] = await Promise.all([
    getCodebaseStats(executeQuery, projectName),
    getHotspots(executeQuery),
    getFolderTree(executeQuery),
  ]);

  return { stats, hotspots, folderTree };
}

/* Render gathered context as markdown for prompt injection */
export function formatContextForPrompt(context: ProjectContext): string {
  const { stats, hotspots, folderTree } = context;

  const sections: string[] = [];

  sections.push(`### CODEBASE: ${stats.projectName}`);

  const counters = [
    `Files: ${stats.fileCount}`,
    `Functions: ${stats.functionCount}`,
    stats.classCount > 0 ? `Classes: ${stats.classCount}` : null,
    stats.interfaceCount > 0 ? `Interfaces: ${stats.interfaceCount}` : null,
  ].filter(Boolean);

  sections.push(counters.join(' | '));
  sections.push('');

  if (hotspots.length > 0) {
    sections.push('**Hotspots** (most connected):');
    for (const h of hotspots.slice(0, 5)) {
      sections.push(`- \`${h.name}\` (${h.type}) — ${h.connections} edges`);
    }
    sections.push('');
  }

  if (folderTree) {
    sections.push('### STRUCTURE');
    sections.push('```');
    sections.push(stats.projectName + '/');
    sections.push(folderTree);
    sections.push('```');
  }

  return sections.join('\n');
}

/* Concatenate base system prompt with dynamic project context */
export function composeSystemPrompt(
  basePrompt: string,
  context: ProjectContext,
): string {
  const rendered = formatContextForPrompt(context);

  return `${basePrompt}\n\n---\n\n## CURRENT PROJECT\n${rendered}`;
}
