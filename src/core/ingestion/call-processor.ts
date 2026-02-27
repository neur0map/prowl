import type { CodeGraph } from '../graph/types';
import type { ASTCache } from './ast-cache';
import type { SymbolTable } from './symbol-table';
import type { ImportMap } from './import-processor';
import { loadParser, loadLanguage } from '../tree-sitter/parser-loader';
import { LANGUAGE_QUERIES } from './tree-sitter-queries';
import { generateId } from '../../lib/utils';
import { getLanguageFromFilename } from './utils';

/* Names that are part of the language runtime and should never
   produce a CALLS edge into project code. */
const RUNTIME_NAMES: ReadonlySet<string> = new Set([
  /* JS/TS globals */
  'console', 'log', 'warn', 'error', 'info', 'debug',
  'setTimeout', 'setInterval', 'clearTimeout', 'clearInterval',
  'parseInt', 'parseFloat', 'isNaN', 'isFinite',
  'encodeURI', 'decodeURI', 'encodeURIComponent', 'decodeURIComponent',
  'JSON', 'parse', 'stringify',
  'Object', 'Array', 'String', 'Number', 'Boolean', 'Symbol', 'BigInt',
  'Map', 'Set', 'WeakMap', 'WeakSet',
  'Promise', 'resolve', 'reject', 'then', 'catch', 'finally',
  'Math', 'Date', 'RegExp', 'Error',
  'require', 'import', 'export',
  'fetch', 'Response', 'Request',
  /* React primitives */
  'useState', 'useEffect', 'useCallback', 'useMemo', 'useRef', 'useContext',
  'useReducer', 'useLayoutEffect', 'useImperativeHandle', 'useDebugValue',
  'createElement', 'createContext', 'createRef', 'forwardRef', 'memo', 'lazy',
  /* Collection methods */
  'map', 'filter', 'reduce', 'forEach', 'find', 'findIndex', 'some', 'every',
  'includes', 'indexOf', 'slice', 'splice', 'concat', 'join', 'split',
  'push', 'pop', 'shift', 'unshift', 'sort', 'reverse',
  'keys', 'values', 'entries', 'assign', 'freeze', 'seal',
  'hasOwnProperty', 'toString', 'valueOf',
  /* Python builtins */
  'print', 'len', 'range', 'str', 'int', 'float', 'list', 'dict', 'set', 'tuple',
  'open', 'read', 'write', 'close', 'append', 'extend', 'update',
  'super', 'type', 'isinstance', 'issubclass', 'getattr', 'setattr', 'hasattr',
  'enumerate', 'zip', 'sorted', 'reversed', 'min', 'max', 'sum', 'abs',
]);

/* AST node types that delimit a callable scope */
const SCOPE_BOUNDARIES: ReadonlySet<string> = new Set([
  'function_declaration', 'arrow_function', 'function_expression',
  'method_definition', 'generator_function_declaration',
  'function_definition', 'async_function_declaration',
  'async_arrow_function', 'method_declaration', 'constructor_declaration',
  'local_function_statement', 'function_item', 'impl_item',
]);

interface MatchedTarget {
  nodeId: string;
  confidence: number;
  reason: string;
}

/**
 * Walk up from a call-site AST node to the nearest enclosing
 * callable and return its graph node ID (or null for top-level).
 */
function enclosingScope(
  callNode: any,
  filePath: string,
  symbols: SymbolTable,
): string | null {
  let anc = callNode.parent;

  while (anc) {
    if (!SCOPE_BOUNDARIES.has(anc.type)) { anc = anc.parent; continue; }

    let fnName: string | null = null;
    let tag = 'Function';

    switch (anc.type) {
      case 'function_declaration':
      case 'function_definition':
      case 'async_function_declaration':
      case 'generator_function_declaration':
      case 'function_item': {
        const n = anc.childForFieldName?.('name')
          ?? anc.children?.find((c: any) => c.type === 'identifier' || c.type === 'property_identifier');
        fnName = n?.text ?? null;
        break;
      }
      case 'impl_item': {
        const inner = anc.children?.find((c: any) => c.type === 'function_item');
        if (inner) {
          const n = inner.childForFieldName?.('name')
            ?? inner.children?.find((c: any) => c.type === 'identifier');
          fnName = n?.text ?? null;
          tag = 'Method';
        }
        break;
      }
      case 'method_definition': {
        const n = anc.childForFieldName?.('name')
          ?? anc.children?.find((c: any) => c.type === 'property_identifier');
        fnName = n?.text ?? null;
        tag = 'Method';
        break;
      }
      case 'method_declaration':
      case 'constructor_declaration': {
        const n = anc.childForFieldName?.('name')
          ?? anc.children?.find((c: any) => c.type === 'identifier');
        fnName = n?.text ?? null;
        tag = 'Method';
        break;
      }
      case 'arrow_function':
      case 'function_expression': {
        const decl = anc.parent;
        if (decl?.type === 'variable_declarator') {
          const n = decl.childForFieldName?.('name')
            ?? decl.children?.find((c: any) => c.type === 'identifier');
          fnName = n?.text ?? null;
        }
        break;
      }
      default:
        break;
    }

    if (fnName) {
      const exact = symbols.lookupExact(filePath, fnName);
      return exact ?? generateId(tag, `${filePath}:${fnName}`);
    }

    anc = anc.parent;
  }

  return null;
}

/**
 * Tiered symbol resolution: imported deps -> same file -> global.
 */
function matchTarget(
  name: string,
  callerFile: string,
  symbols: SymbolTable,
  imports: ImportMap,
): MatchedTarget | null {
  /* Tier 1: symbols from explicitly imported files */
  const deps = imports.get(callerFile);
  if (deps) {
    for (const dep of deps) {
      const hit = symbols.lookupExact(dep, name);
      if (hit) return { nodeId: hit, confidence: 0.9, reason: 'import-resolved' };
    }
  }

  /* Tier 2: defined in same file */
  const local = symbols.lookupExact(callerFile, name);
  if (local) return { nodeId: local, confidence: 0.85, reason: 'same-file' };

  /* Tier 3: project-wide scan */
  const global = symbols.lookupFuzzy(name);
  if (global.length > 0) {
    const conf = global.length === 1 ? 0.5 : 0.3;
    return { nodeId: global[0].nodeId, confidence: conf, reason: 'fuzzy-global' };
  }

  return null;
}

export async function processCalls(
  graph: CodeGraph,
  files: { path: string; content: string }[],
  astCache: ASTCache,
  symbolTable: SymbolTable,
  importMap: ImportMap,
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
      console.warn(`[prowl:calls] query failed for ${src.path}`, err);
      if (owned) tree.delete();
      continue;
    }

    for (const m of matches) {
      const cap: Record<string, any> = {};
      for (const c of m.captures) cap[c.name] = c.node;

      if (!cap['call']) continue;
      const calleeNode = cap['call.name'];
      if (!calleeNode) continue;

      const calleeName = calleeNode.text;
      if (RUNTIME_NAMES.has(calleeName)) continue;

      const target = matchTarget(calleeName, src.path, symbolTable, importMap);
      if (!target) continue;

      const callerId = enclosingScope(cap['call'], src.path, symbolTable)
        ?? generateId('File', src.path);

      graph.addRelationship({
        id: generateId('CALLS', `${callerId}:${calleeName}->${target.nodeId}`),
        sourceId: callerId,
        targetId: target.nodeId,
        type: 'CALLS',
        confidence: target.confidence,
        reason: target.reason,
      });
    }

    if (owned) tree.delete();
  }
}
