/**
 * Tracks where every named symbol is declared so that
 * call-resolution and import-linking can look them up
 * by file (precise) or project-wide (approximate).
 */

export interface SymbolRecord {
  nodeId: string;
  filePath: string;
  kind: string;
}

export type SymbolDefinition = SymbolRecord;

export interface SymbolTable {
  register(filePath: string, name: string, nodeId: string, kind: string): void;
  resolve(filePath: string, name: string): string | undefined;
  scan(name: string): SymbolRecord[];
  metrics(): { files: number; symbols: number };
  reset(): void;
  /* Aliases kept for downstream consumers until they are refactored */
  add: SymbolTable['register'];
  lookupExact: SymbolTable['resolve'];
  lookupFuzzy: SymbolTable['scan'];
  getStats(): { fileCount: number; globalSymbolCount: number };
  clear: SymbolTable['reset'];
}

export function createSymbolTable(): SymbolTable {
  /* Precise index — keyed by "file\0name" for zero-collision scoped lookup */
  const scoped = new Map<string, string>();

  /* Broad index — every name that has been registered, regardless of origin */
  const broad = new Map<string, SymbolRecord[]>();

  /* Track distinct source files */
  const origins = new Set<string>();

  function scopeKey(fp: string, sym: string): string {
    return `${fp}\0${sym}`;
  }

  return {
    register(filePath, name, nodeId, kind) {
      origins.add(filePath);
      scoped.set(scopeKey(filePath, name), nodeId);

      const rec: SymbolRecord = { nodeId, filePath, kind };
      const list = broad.get(name);
      if (list) list.push(rec);
      else broad.set(name, [rec]);
    },

    resolve(filePath, name) {
      return scoped.get(scopeKey(filePath, name));
    },

    scan(name) {
      return broad.get(name) ?? [];
    },

    metrics() {
      return { files: origins.size, symbols: broad.size };
    },

    reset() {
      scoped.clear();
      broad.clear();
      origins.clear();
    },

    /* backward-compat aliases */
    add(filePath: string, name: string, nodeId: string, kind: string) {
      this.register(filePath, name, nodeId, kind);
    },
    lookupExact(filePath: string, name: string) {
      return this.resolve(filePath, name);
    },
    lookupFuzzy(name: string) {
      return this.scan(name);
    },
    getStats() {
      const m = this.metrics();
      return { fileCount: m.files, globalSymbolCount: m.symbols };
    },
    clear() {
      this.reset();
    },
  };
}
