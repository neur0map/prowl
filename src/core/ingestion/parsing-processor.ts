import type { CodeGraph, GraphNode, GraphRelationship } from '../graph/types';
import { loadParser, loadLanguage } from '../tree-sitter/parser-loader';
import { LANGUAGE_QUERIES } from './tree-sitter-queries';
import { generateId } from '../../lib/utils';
import type { SymbolTable } from './symbol-table';
import type { ASTCache } from './ast-cache';
import { getLanguageFromFilename } from './utils';

export type FileProgressCallback = (current: number, total: number, filePath: string) => void;

/* Mapping from capture-group suffix to the graph label it produces */
const CAPTURE_TO_LABEL: Record<string, string> = {
  'definition.function': 'Function',
  'definition.class': 'Class',
  'definition.interface': 'Interface',
  'definition.method': 'Method',
  'definition.struct': 'Struct',
  'definition.enum': 'Enum',
  'definition.namespace': 'Namespace',
  'definition.module': 'Module',
  'definition.trait': 'Trait',
  'definition.impl': 'Impl',
  'definition.type': 'TypeAlias',
  'definition.const': 'Const',
  'definition.static': 'Static',
  'definition.typedef': 'Typedef',
  'definition.macro': 'Macro',
  'definition.union': 'Union',
  'definition.property': 'Property',
  'definition.record': 'Record',
  'definition.delegate': 'Delegate',
  'definition.annotation': 'Annotation',
  'definition.constructor': 'Constructor',
  'definition.template': 'Template',
};

const ORDERED_KEYS = Object.keys(CAPTURE_TO_LABEL);

/** Pick the first matching capture group and return its label. */
function labelFromCaptures(caps: Record<string, any>): string {
  for (const k of ORDERED_KEYS) {
    if (caps[k] !== undefined) return CAPTURE_TO_LABEL[k];
  }
  return 'CodeElement';
}

/**
 * Language-specific heuristic for whether a symbol is public.
 * Python: underscore prefix = private.
 * Go: uppercase first char = exported.
 * C/C++: no explicit export keyword.
 * JS/TS/Java/C#/Rust: walk ancestors for visibility modifiers.
 */
function determineExportStatus(node: any, name: string, lang: string): boolean {
  if (lang === 'python') return !name.startsWith('_');

  if (lang === 'go') {
    if (name.length === 0) return false;
    const first = name.charAt(0);
    return first === first.toUpperCase() && first !== first.toLowerCase();
  }

  if (lang === 'c' || lang === 'cpp') return false;

  /* Swift: internal is the default access level (visible to other files).
     Only private/fileprivate symbols are non-exported. */
  if (lang === 'swift') {
    let cur2 = node;
    while (cur2) {
      if (cur2.type === 'modifiers' || cur2.type === 'modifier') {
        const txt = cur2.text;
        if (txt?.includes('private') || txt?.includes('fileprivate')) return false;
      }
      cur2 = cur2.parent;
    }
    return true;
  }

  let cur = node;
  while (cur) {
    const t = cur.type;

    if (lang === 'javascript' || lang === 'typescript') {
      if (
        t === 'export_statement' ||
        t === 'export_specifier' ||
        (t === 'lexical_declaration' && cur.parent?.type === 'export_statement')
      ) return true;
      if (cur.text?.startsWith('export ')) return true;
    } else if (lang === 'java') {
      if (cur.parent) {
        const p = cur.parent;
        for (let ci = 0; ci < p.childCount; ci++) {
          const sib = p.child(ci);
          if (sib?.type === 'modifiers' && sib.text?.includes('public')) return true;
        }
        if (
          (p.type === 'method_declaration' || p.type === 'constructor_declaration') &&
          p.text?.trimStart().startsWith('public')
        ) return true;
      }
    } else if (lang === 'csharp') {
      if ((t === 'modifier' || t === 'modifiers') && cur.text?.includes('public')) return true;
    } else if (lang === 'rust') {
      if (t === 'visibility_modifier' && cur.text?.includes('pub')) return true;
    }

    cur = cur.parent;
  }
  return false;
}

export async function processParsing(
  graph: CodeGraph,
  files: { path: string; content: string }[],
  symbolTable: SymbolTable,
  astCache: ASTCache,
  onFileProgress?: FileProgressCallback,
): Promise<void> {
  const parser = await loadParser();

  for (let i = 0; i < files.length; i++) {
    const src = files[i];
    onFileProgress?.(i + 1, files.length, src.path);

    const lang = getLanguageFromFilename(src.path);
    if (lang === null) continue;

    await loadLanguage(lang, src.path);

    const tree = parser.parse(src.content);
    astCache.set(src.path, tree);

    const qText = LANGUAGE_QUERIES[lang];
    if (!qText) continue;

    let matches: any[];
    try {
      const q = parser.getLanguage().query(qText);
      matches = q.matches(tree.rootNode);
    } catch (err) {
      console.warn(`[prowl:parse] query failed for ${src.path}`, err);
      continue;
    }

    const seenInFile = new Set<string>();

    for (const m of matches) {
      const caps: Record<string, any> = {};
      for (const c of m.captures) caps[c.name] = c.node;

      /* imports / calls belong to their own processors */
      if (caps['import'] || caps['call']) continue;

      const nameNode = caps['name'];
      if (!nameNode) continue;

      const symName = nameNode.text;
      if (seenInFile.has(symName)) continue;
      seenInFile.add(symName);
      const tag = labelFromCaptures(caps);
      const symId = generateId(tag, `${src.path}:${symName}`);

      const gNode: GraphNode = {
        id: symId,
        label: tag as any,
        properties: {
          name: symName,
          filePath: src.path,
          startLine: nameNode.startPosition.row,
          endLine: nameNode.endPosition.row,
          language: lang,
          isExported: determineExportStatus(nameNode, symName, lang),
        },
      };
      graph.addNode(gNode);
      symbolTable.add(src.path, symName, symId, tag);

      const fileId = generateId('File', src.path);
      const edge: GraphRelationship = {
        id: generateId('DEFINES', `${fileId}->${symId}`),
        sourceId: fileId,
        targetId: symId,
        type: 'DEFINES',
        confidence: 1.0,
        reason: '',
      };
      graph.addRelationship(edge);
    }
  }
}
