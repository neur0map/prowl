/**
 * Transforms graph nodes into plain-text suitable for embedding models.
 * Dispatches on node label to produce structured representations.
 */

import type { EmbeddableNode, EmbeddingConfig } from './types';
import { DEFAULT_EMBEDDING_CONFIG } from './types';

/* ── Formatting strategy per label ───────────────────── */

type Formatter = (node: EmbeddableNode, snippetCap: number) => string;

/* Pull the trailing filename component from a path */
function basename(fullPath: string): string {
  const sep = fullPath.lastIndexOf('/');
  return sep >= 0 ? fullPath.substring(sep + 1) : fullPath;
}

/* Pull everything before the last slash */
function dirname(fullPath: string): string {
  const sep = fullPath.lastIndexOf('/');
  return sep >= 0 ? fullPath.substring(0, sep) : '';
}

/* Shorten text to a budget, preferring a word boundary cut */
function truncate(text: string, budget: number): string {
  if (text.length <= budget) return text;

  const chunk = text.substring(0, budget);
  const ws = chunk.lastIndexOf(' ');

  if (ws > budget * 0.8) {
    return chunk.substring(0, ws) + '...';
  }
  return chunk + '...';
}

/* Collapse runs of blank lines and strip trailing whitespace on each line */
function normalizeWhitespace(raw: string): string {
  const unified = raw.replace(/\r\n/g, '\n');
  const compacted = unified.replace(/\n{3,}/g, '\n\n');
  return compacted
    .split('\n')
    .reduce<string[]>((acc, ln) => { acc.push(ln.trimEnd()); return acc; }, [])
    .join('\n')
    .trim();
}

/* ── Per-label formatters ────────────────────────────── */

/* Structured text block for code symbols (Function, Class, Method, Interface) */
function formatSymbol(
  kind: string,
  node: EmbeddableNode,
  snippetCap: number,
): string {
  const lines = [`${kind}: ${node.name}`, `File: ${basename(node.filePath)}`];

  const parent = dirname(node.filePath);
  if (parent) lines.push(`Directory: ${parent}`);

  if (node.content) {
    const cleaned = normalizeWhitespace(node.content);
    lines.push('', truncate(cleaned, snippetCap));
  }

  return lines.join('\n');
}

/* Text block for File nodes — uses a smaller snippet ceiling */
const formatFile: Formatter = (node, snippetCap) => {
  const lines = [`File: ${node.name}`, `Path: ${node.filePath}`];

  if (node.content) {
    const cleaned = normalizeWhitespace(node.content);
    const ceiling = snippetCap < 300 ? snippetCap : 300;
    lines.push('', truncate(cleaned, ceiling));
  }

  return lines.join('\n');
};

/* ── Dispatch table ──────────────────────────────────── */

const FORMATTERS: Record<string, Formatter> = {
  Function:  (n, cap) => formatSymbol('Function', n, cap),
  Class:     (n, cap) => formatSymbol('Class', n, cap),
  Method:    (n, cap) => formatSymbol('Method', n, cap),
  Interface: (n, cap) => formatSymbol('Interface', n, cap),
  File:      formatFile,
};

/* ── Public API ──────────────────────────────────────── */

/* Render embedding text for one node, delegating to the right formatter */
export function generateEmbeddingText(
  node: EmbeddableNode,
  config: Partial<EmbeddingConfig> = {},
): string {
  const snippetLimit = config.maxSnippetLength ?? DEFAULT_EMBEDDING_CONFIG.maxSnippetLength;
  const fmt = FORMATTERS[node.label];

  if (fmt) return fmt(node, snippetLimit);

  return `${node.label}: ${node.name}\nPath: ${node.filePath}`;
}

/* Render embedding texts for a batch, keeping original order */
export function prepareBatchTexts(
  nodes: EmbeddableNode[],
  config: Partial<EmbeddingConfig> = {},
): string[] {
  const out: string[] = [];
  for (const nd of nodes) out.push(generateEmbeddingText(nd, config));
  return out;
}
