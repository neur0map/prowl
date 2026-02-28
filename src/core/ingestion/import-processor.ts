import type { CodeGraph } from '../graph/types';
import type { ASTCache } from './ast-cache';
import { loadParser, loadLanguage } from '../tree-sitter/parser-loader';
import { LANGUAGE_QUERIES } from './tree-sitter-queries';
import { generateId } from '../../lib/utils';
import { getLanguageFromFilename } from './utils';

/* Per-file set of resolved dependency paths */
export type ImportMap = Map<string, Set<string>>;
export const createImportMap = (): ImportMap => new Map();

/* Suffixes tried when mapping a specifier to a project file */
const CANDIDATE_EXTENSIONS = [
  '',
  '.tsx', '.ts', '.jsx', '.js',
  '/index.tsx', '/index.ts', '/index.jsx', '/index.js',
  '.py', '/__init__.py',
  '.java',
  '.c', '.h', '.cpp', '.hpp', '.cc', '.cxx', '.hxx', '.hh',
  '.cs',
  '.go',
  '.rs', '/mod.rs',
  '.swift',
];

/**
 * Turn an import specifier into the project-relative path it
 * points to, or null when the target lives outside the project.
 */
function resolveToProjectPath(
  originFile: string,
  specifier: string,
  knownPaths: Set<string>,
  allPaths: string[],
  memo: Map<string, string | null>,
): string | null {
  const cacheKey = `${originFile}\0${specifier}`;
  if (memo.has(cacheKey)) return memo.get(cacheKey)!;

  /* Build the base path by applying relative segments */
  const segments = originFile.split('/').slice(0, -1);
  for (const seg of specifier.split('/')) {
    if (seg === '.') continue;
    if (seg === '..') { segments.pop(); continue; }
    segments.push(seg);
  }
  const basePath = segments.join('/');

  /* Relative specifiers ("./" or "../") */
  if (specifier.startsWith('.')) {
    for (const ext of CANDIDATE_EXTENSIONS) {
      const probe = basePath + ext;
      if (knownPaths.has(probe)) {
        memo.set(cacheKey, probe);
        return probe;
      }
    }
    memo.set(cacheKey, null);
    return null;
  }

  /* Wildcards are ambiguous */
  if (specifier.endsWith('.*')) {
    memo.set(cacheKey, null);
    return null;
  }

  /* Package-style: convert dots to slashes, try tail matching */
  const normalised = specifier.includes('/') ? specifier : specifier.replace(/\./g, '/');
  const parts = normalised.split('/').filter(Boolean);
  const forwardPaths = allPaths.map(fp => fp.replace(/\\/g, '/'));

  for (let offset = 0; offset < parts.length; offset++) {
    const tail = parts.slice(offset).join('/');
    for (const ext of CANDIDATE_EXTENSIONS) {
      const needle = '/' + tail + ext;
      const idx = forwardPaths.findIndex(
        fp => fp.endsWith(needle) || fp.toLowerCase().endsWith(needle.toLowerCase()),
      );
      if (idx !== -1) {
        const found = allPaths[idx];
        memo.set(cacheKey, found);
        return found;
      }
    }
  }

  memo.set(cacheKey, null);
  return null;
}

export async function processImports(
  graph: CodeGraph,
  files: { path: string; content: string }[],
  astCache: ASTCache,
  importMap: ImportMap,
  onProgress?: (current: number, total: number) => void,
): Promise<void> {
  const knownPaths = new Set(files.map(f => f.path));
  const pathList = files.map(f => f.path);
  const parser = await loadParser();
  const memo = new Map<string, string | null>();

  let resolved = 0;
  let total = 0;

  for (let i = 0; i < files.length; i++) {
    const src = files[i];
    onProgress?.(i + 1, files.length);

    const lang = getLanguageFromFilename(src.path);
    if (!lang) continue;

    const qText = LANGUAGE_QUERIES[lang];
    if (!qText) continue;

    await loadLanguage(lang, src.path);

    let tree = astCache.get(src.path);
    let owned = false;
    if (!tree) {
      tree = parser.parse(src.content);
      owned = true;
    }

    let matches: any[];
    try {
      const q = parser.getLanguage().query(qText);
      matches = q.matches(tree.rootNode);
    } catch {
      if (owned) tree.delete();
      continue;
    }

    for (const m of matches) {
      const cap: Record<string, any> = {};
      for (const c of m.captures) cap[c.name] = c.node;

      if (!cap['import']) continue;

      const srcNode = cap['import.source'];
      if (!srcNode) {
        if (import.meta.env.DEV) {
          console.log(`[prowl:imports] import without source in ${src.path}`);
        }
        continue;
      }

      const rawSpec = srcNode.text.replace(/['"]/g, '');
      total++;

      const target = resolveToProjectPath(src.path, rawSpec, knownPaths, pathList, memo);
      if (!target) continue;

      resolved++;

      const fromId = generateId('File', src.path);
      const toId = generateId('File', target);

      graph.addRelationship({
        id: generateId('IMPORTS', `${src.path}->${target}`),
        sourceId: fromId,
        targetId: toId,
        type: 'IMPORTS',
        confidence: 1.0,
        reason: '',
      });

      if (!importMap.has(src.path)) importMap.set(src.path, new Set());
      importMap.get(src.path)!.add(target);
    }

    if (owned) tree.delete();
  }

  if (import.meta.env.DEV) {
    console.log(`[prowl:imports] linked ${resolved}/${total} specifiers`);
  }
}
