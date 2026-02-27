import Parser from 'web-tree-sitter';

/**
 * Bounded cache for parsed AST trees.
 *
 * Evicts the oldest entry once the ceiling is hit,
 * calling `tree.delete()` to release WASM heap memory.
 */
export interface ASTCache {
  get(path: string): Parser.Tree | undefined;
  set(path: string, tree: Parser.Tree): void;
  clear(): void;
  stats(): { size: number; ceiling: number };
}

export function createASTCache(ceiling = 50): ASTCache {
  const entries = new Map<string, Parser.Tree>();

  function evictOldest(): void {
    const oldest = entries.keys().next().value;
    if (oldest === undefined) return;
    const stale = entries.get(oldest);
    entries.delete(oldest);
    try { stale?.delete(); } catch { /* tree already freed */ }
  }

  function safeRelease(tree: Parser.Tree): void {
    try { tree.delete(); } catch { /* already freed */ }
  }

  return {
    get(path) {
      return entries.get(path);
    },

    set(path, tree) {
      if (entries.has(path)) {
        const prev = entries.get(path)!;
        entries.delete(path);
        safeRelease(prev);
      }
      if (entries.size >= ceiling) evictOldest();
      entries.set(path, tree);
    },

    clear() {
      for (const t of entries.values()) safeRelease(t);
      entries.clear();
    },

    stats() {
      return { size: entries.size, ceiling };
    },
  };
}
