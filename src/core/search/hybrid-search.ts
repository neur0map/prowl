/**
 * Reciprocal Rank Fusion (RRF) combiner for BM25 and vector search.
 * Uses rank positions instead of raw scores so the two retrieval
 * pipelines stay comparable regardless of their scoring scales.
 */

import { searchBM25, isBM25Ready, type BM25SearchResult } from './bm25-index';
import type { SemanticSearchResult } from '../embeddings/types';

/* ── Constants ───────────────────────────────────────── */

/* Standard RRF smoothing parameter */
const RRF_CONSTANT = 60;

/* ── Result shape ────────────────────────────────────── */

export interface SearchHit {
  filePath: string;
  score: number;
  rank: number;
  sources: ('bm25' | 'semantic' | 'reranker')[];

  nodeId?: string;
  name?: string;
  label?: string;
  startLine?: number;
  endLine?: number;

  bm25Score?: number;
  semanticScore?: number;
}

/* ── Internals ───────────────────────────────────────── */

/* Compute the RRF contribution for a zero-based position */
function reciprocalRank(position: number): number {
  return 1 / (RRF_CONSTANT + position + 1);
}

/* Transfer semantic metadata fields onto a merged hit */
function attachSemanticMeta(
  dest: SearchHit,
  src: SemanticSearchResult,
): void {
  dest.nodeId = src.nodeId;
  dest.name = src.name;
  dest.label = src.label;
  dest.startLine = src.startLine;
  dest.endLine = src.endLine;
}

/* ── Public API ──────────────────────────────────────── */

/* Combine BM25 and semantic results via RRF into a ranked list */
export function mergeWithRRF(
  bm25Results: BM25SearchResult[],
  semanticResults: SemanticSearchResult[],
  limit: number = 10,
): SearchHit[] {
  const accumulator = new Map<string, SearchHit>();

  for (let i = 0; i < bm25Results.length; i++) {
    const hit = bm25Results[i];
    accumulator.set(hit.filePath, {
      filePath: hit.filePath,
      score: reciprocalRank(i),
      rank: 0,
      sources: ['bm25'],
      bm25Score: hit.score,
    });
  }

  for (let i = 0; i < semanticResults.length; i++) {
    const hit = semanticResults[i];
    const w = reciprocalRank(i);
    const existing = accumulator.get(hit.filePath);

    if (existing !== undefined) {
      existing.score += w;
      existing.sources.push('semantic');
      existing.semanticScore = 1 - hit.distance;
      attachSemanticMeta(existing, hit);
    } else {
      const fresh: SearchHit = {
        filePath: hit.filePath,
        score: w,
        rank: 0,
        sources: ['semantic'],
        semanticScore: 1 - hit.distance,
      };
      attachSemanticMeta(fresh, hit);
      accumulator.set(hit.filePath, fresh);
    }
  }

  const ranked = [...accumulator.values()]
    .sort((a, b) => b.score - a.score)
    .slice(0, limit);

  for (let pos = 0; pos < ranked.length; pos++) {
    ranked[pos].rank = pos + 1;
  }

  return ranked;
}

/* Whether at least the keyword index is available */
export function isHybridSearchReady(): boolean {
  return isBM25Ready();
}

/* Render search hits as a readable text block for LLM context */
export function formatHybridResults(results: SearchHit[]): string {
  if (results.length === 0) return 'No results found.';

  const parts: string[] = [];

  for (let idx = 0; idx < results.length; idx++) {
    const entry = results[idx];
    const methodStr = entry.sources.join(' + ');
    const locSuffix = entry.startLine ? ` (lines ${entry.startLine}-${entry.endLine})` : '';
    const prefix = entry.label ? `${entry.label}: ` : 'File: ';
    const displayName = entry.name || entry.filePath.split('/').pop() || entry.filePath;

    parts.push(
      `[${idx + 1}] ${prefix}${displayName}\n` +
      `    File: ${entry.filePath}${locSuffix}\n` +
      `    Found by: ${methodStr}\n` +
      `    Relevance: ${entry.score.toFixed(4)}`,
    );
  }

  return `Found ${results.length} results:\n\n${parts.join('\n\n')}`;
}

/* ── Post-RRF cross-encoder reranking ────────────────── */

export async function rerankSearchHits(
  query: string,
  hits: SearchHit[],
  fileContents: Map<string, string>,
  rerankerFn: (query: string, docs: string[]) => Promise<{ index: number; score: number }[]>,
  topK: number = 10,
): Promise<SearchHit[]> {
  if (hits.length === 0) return [];

  /* Take a wider candidate set for the reranker to reshuffle */
  const candidateCount = Math.min(topK * 3, 30, hits.length);
  const candidates = hits.slice(0, candidateCount);

  /* Build document texts from file contents */
  const documents: string[] = [];
  const validIndices: number[] = [];

  for (let i = 0; i < candidates.length; i++) {
    const content = fileContents.get(candidates[i].filePath);
    if (content) {
      /* Truncate to keep cross-encoder input manageable */
      documents.push(content.slice(0, 2000));
      validIndices.push(i);
    }
  }

  if (documents.length === 0) return hits.slice(0, topK);

  const scored = await rerankerFn(query, documents);

  /* Map reranker indices back to candidate indices */
  const reranked: SearchHit[] = scored
    .slice(0, topK)
    .map((s) => {
      const hit = { ...candidates[validIndices[s.index]] };
      hit.score = s.score;
      if (!hit.sources.includes('reranker')) {
        hit.sources = [...hit.sources, 'reranker'];
      }
      return hit;
    });

  /* Reassign ranks */
  for (let pos = 0; pos < reranked.length; pos++) {
    reranked[pos].rank = pos + 1;
  }

  return reranked;
}
