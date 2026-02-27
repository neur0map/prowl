/**
 * Term-frequency keyword index backed by MiniSearch.
 * Complements the vector search path with exact-term lookups.
 */

import MiniSearch from 'minisearch';

/* ── Shapes ──────────────────────────────────────────── */

export interface BM25Document {
  id: string;
  content: string;
  name: string;
}

export interface BM25SearchResult {
  filePath: string;
  score: number;
  rank: number;
}

/* ── Tokenisation ────────────────────────────────────── */

/* Language keywords and filler words that add no retrieval value */
const STOP_WORDS: ReadonlySet<string> = new Set([
  'const', 'let', 'var', 'function', 'return', 'if', 'else', 'for', 'while',
  'class', 'new', 'this', 'import', 'export', 'from', 'default', 'async', 'await',
  'try', 'catch', 'throw', 'typeof', 'instanceof', 'true', 'false', 'null', 'undefined',
  'the', 'is', 'at', 'which', 'on', 'a', 'an', 'and', 'or', 'but', 'in', 'with',
  'to', 'of', 'it', 'be', 'as', 'by', 'that', 'for', 'are', 'was', 'were',
]);

/* Split on punctuation, break camelCase, drop noise tokens */
function splitTokens(text: string): string[] {
  const raw = text.toLowerCase().split(/[\s\-_./\\(){}[\]<>:;,!?'"]+/);
  const tokens: string[] = [];

  for (let i = 0; i < raw.length; i++) {
    const tok = raw[i];
    if (tok.length === 0) continue;

    const parts = tok
      .replace(/([a-z])([A-Z])/g, '$1 $2')
      .toLowerCase()
      .split(' ');

    for (const p of parts) tokens.push(p);

    if (parts.length > 1) tokens.push(tok);
  }

  return tokens.filter((t) => t.length > 1 && !STOP_WORDS.has(t));
}

/* ── Index state ─────────────────────────────────────── */

let index: MiniSearch<BM25Document> | null = null;
let indexedCount = 0;

/* Populate the keyword index from file contents (invoke after ingestion) */
export function buildBM25Index(fileContents: Map<string, string>): number {
  index = new MiniSearch<BM25Document>({
    fields: ['content', 'name'],
    storeFields: ['id'],
    tokenize: splitTokens,
  });

  const docs: BM25Document[] = [];

  fileContents.forEach((content, filePath) => {
    const sep = filePath.lastIndexOf('/');
    const fileName = sep >= 0 ? filePath.substring(sep + 1) : filePath;
    docs.push({ id: filePath, content, name: fileName });
  });

  index.addAll(docs);
  indexedCount = docs.length;

  return indexedCount;
}

/* Run a keyword query against the index */
export function searchBM25(query: string, limit: number = 20): BM25SearchResult[] {
  if (index === null) return [];

  const hits = index.search(query, {
    fuzzy: 0.2,
    prefix: true,
    boost: { name: 2 },
  });

  const output: BM25SearchResult[] = [];
  const capped = hits.slice(0, limit);

  for (let r = 0; r < capped.length; r++) {
    output.push({ filePath: capped[r].id, score: capped[r].score, rank: r + 1 });
  }

  return output;
}

/* Whether the index has been populated and is queryable */
export function isBM25Ready(): boolean {
  return index !== null && indexedCount > 0;
}

/* Current index size metrics */
export function getBM25Stats(): { documentCount: number; termCount: number } {
  if (index === null) {
    return { documentCount: 0, termCount: 0 };
  }
  return { documentCount: indexedCount, termCount: index.termCount };
}

/* Tear down the index for re-indexing or cleanup */
export function clearBM25Index(): void {
  index = null;
  indexedCount = 0;
}
