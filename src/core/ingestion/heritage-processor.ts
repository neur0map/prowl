/**
 * Materialises EXTENDS and IMPLEMENTS edges by scanning
 * class/struct/trait heritage clauses in the AST.
 */

import type { CodeGraph } from '../graph/types';
import type { ASTCache } from './ast-cache';
import type { SymbolTable } from './symbol-table';
import { loadParser, loadLanguage } from '../tree-sitter/parser-loader';
import { LANGUAGE_QUERIES } from './tree-sitter-queries';
import { generateId } from '../../lib/utils';
import { getLanguageFromFilename } from './utils';

/* Locate a node ID first via scoped lookup, then global, then fabricate one. */
function pinpointNode(
  symbols: SymbolTable,
  file: string,
  name: string,
  fallbackTag: string,
): string {
  const scoped = symbols.lookupExact(file, name);
  if (scoped) return scoped;

  const global = symbols.lookupFuzzy(name);
  if (global.length > 0) return global[0].nodeId;

  return generateId(fallbackTag, file ? `${file}:${name}` : name);
}

/* Global-only lookup when the parent type lives in another file. */
function findGlobalNode(
  symbols: SymbolTable,
  name: string,
  fallbackTag: string,
): string {
  const hits = symbols.lookupFuzzy(name);
  return hits.length > 0 ? hits[0].nodeId : generateId(fallbackTag, name);
}

export async function processHeritage(
  graph: CodeGraph,
  files: { path: string; content: string }[],
  astCache: ASTCache,
  symbolTable: SymbolTable,
  onProgress?: (current: number, total: number) => void,
): Promise<void> {
  const parser = await loadParser();

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
    } catch (err) {
      console.warn(`[prowl:heritage] query failed for ${src.path}`, err);
      if (owned) tree.delete();
      continue;
    }

    for (const m of matches) {
      const cap: Record<string, any> = {};
      for (const c of m.captures) cap[c.name] = c.node;

      const classN = cap['heritage.class'];
      const extendsN = cap['heritage.extends'];
      const implN = cap['heritage.implements'];
      const traitN = cap['heritage.trait'];

      /* Inheritance: extends a base class/struct */
      if (classN && extendsN) {
        const childId = pinpointNode(symbolTable, src.path, classN.text, 'Class');
        const parentId = findGlobalNode(symbolTable, extendsN.text, 'Class');
        if (childId !== parentId) {
          graph.addRelationship({
            id: generateId('EXTENDS', `${childId}->${parentId}`),
            sourceId: childId,
            targetId: parentId,
            type: 'EXTENDS',
            confidence: 1.0,
            reason: '',
          });
        }
      }

      /* Conformance: implements an interface */
      if (classN && implN) {
        const typeId = pinpointNode(symbolTable, src.path, classN.text, 'Class');
        const ifaceId = findGlobalNode(symbolTable, implN.text, 'Interface');
        if (typeId && ifaceId) {
          graph.addRelationship({
            id: generateId('IMPLEMENTS', `${typeId}->${ifaceId}`),
            sourceId: typeId,
            targetId: ifaceId,
            type: 'IMPLEMENTS',
            confidence: 1.0,
            reason: '',
          });
        }
      }

      /* Rust: impl Trait for Type */
      if (traitN && classN) {
        const concreteId = pinpointNode(symbolTable, src.path, classN.text, 'Struct');
        const traitId = findGlobalNode(symbolTable, traitN.text, 'Trait');
        if (concreteId && traitId) {
          graph.addRelationship({
            id: generateId('IMPLEMENTS', `${concreteId}->${traitId}`),
            sourceId: concreteId,
            targetId: traitId,
            type: 'IMPLEMENTS',
            confidence: 1.0,
            reason: 'trait-impl',
          });
        }
      }
    }

    if (owned) tree.delete();
  }
}
