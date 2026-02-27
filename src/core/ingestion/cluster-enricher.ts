/**
 * Refines heuristic module labels into semantic names using
 * an LLM. Supports individual and batched enrichment modes.
 */

import type { CommunityNode } from './community-processor';

/* ── Data shapes ──────────────────────────────────────── */

export interface ClusterEnrichment {
  name: string;
  keywords: string[];
  description: string;
}

export interface EnrichmentResult {
  enrichments: Map<string, ClusterEnrichment>;
  tokensUsed: number;
}

export interface LLMClient {
  generate: (prompt: string) => Promise<string>;
}

export interface ClusterMemberInfo {
  name: string;
  filePath: string;
  type: string;
}

/* ── Internals ────────────────────────────────────────── */

function buildSinglePrompt(members: ClusterMemberInfo[], heuristic: string): string {
  const capped = members.slice(0, 20);
  const listing = capped.map(m => `${m.name} (${m.type})`).join(', ');
  const overflow = members.length > 20 ? ` (+${members.length - 20} more)` : '';

  return [
    'Analyze this code cluster and provide a semantic name and short description.',
    '',
    `Heuristic: "${heuristic}"`,
    `Members: ${listing}${overflow}`,
    '',
    'Reply with JSON only:',
    '{"name": "2-4 word semantic name", "description": "One sentence describing purpose"}',
  ].join('\n');
}

function parseSingleResponse(raw: string, fallback: string): ClusterEnrichment {
  try {
    const json = raw.match(/\{[\s\S]*\}/);
    if (!json) throw new Error('no JSON object in response');
    const obj = JSON.parse(json[0]);
    return {
      name: obj.name || fallback,
      keywords: Array.isArray(obj.keywords) ? obj.keywords : [],
      description: obj.description || '',
    };
  } catch {
    return { name: fallback, keywords: [], description: '' };
  }
}

function roughTokenCount(text: string): number {
  return Math.ceil(text.length / 4);
}

function defaultEnrichment(label: string): ClusterEnrichment {
  return { name: label, keywords: [], description: '' };
}

/* ── Individual enrichment ────────────────────────────── */

/**
 * Enrich each cluster one at a time via individual LLM calls.
 */
export async function enrichClusters(
  communities: CommunityNode[],
  memberMap: Map<string, ClusterMemberInfo[]>,
  llmClient: LLMClient,
  onProgress?: (current: number, total: number) => void,
): Promise<EnrichmentResult> {
  const out = new Map<string, ClusterEnrichment>();
  let tokens = 0;

  for (let i = 0; i < communities.length; i++) {
    const comm = communities[i];
    onProgress?.(i + 1, communities.length);

    const members = memberMap.get(comm.id) ?? [];
    if (members.length === 0) {
      out.set(comm.id, defaultEnrichment(comm.heuristicLabel));
      continue;
    }

    try {
      const prompt = buildSinglePrompt(members, comm.heuristicLabel);
      const reply = await llmClient.generate(prompt);
      tokens += roughTokenCount(prompt) + roughTokenCount(reply);
      out.set(comm.id, parseSingleResponse(reply, comm.heuristicLabel));
    } catch (err) {
      console.warn(`[prowl:enrich] failed for ${comm.id}`, err);
      out.set(comm.id, defaultEnrichment(comm.heuristicLabel));
    }
  }

  return { enrichments: out, tokensUsed: tokens };
}

/* ── Batched enrichment ───────────────────────────────── */

/**
 * Send multiple clusters per LLM call for higher throughput.
 * Falls back to heuristic labels on parse failure.
 */
export async function labelModulesBatch(
  communities: CommunityNode[],
  memberMap: Map<string, ClusterMemberInfo[]>,
  llmClient: LLMClient,
  batchSize = 5,
  onProgress?: (current: number, total: number) => void,
): Promise<EnrichmentResult> {
  const out = new Map<string, ClusterEnrichment>();
  let tokens = 0;

  for (let offset = 0; offset < communities.length; offset += batchSize) {
    const chunk = communities.slice(offset, offset + batchSize);
    onProgress?.(Math.min(offset + batchSize, communities.length), communities.length);

    const blocks = chunk.map((comm, idx) => {
      const members = (memberMap.get(comm.id) ?? []).slice(0, 15);
      const listing = members.map(m => `${m.name} (${m.type})`).join(', ');
      return [
        `Cluster ${idx + 1} (id: ${comm.id}):`,
        `Heuristic: "${comm.heuristicLabel}"`,
        `Members: ${listing}`,
      ].join('\n');
    });

    const prompt = [
      'Analyze these code clusters and generate semantic names, keywords, and descriptions.',
      '', ...blocks.join('\n\n').split('\n'), '',
      'Output JSON array:',
      '[{"id": "comm_X", "name": "...", "keywords": [...], "description": "..."}, ...]',
    ].join('\n');

    try {
      const reply = await llmClient.generate(prompt);
      tokens += roughTokenCount(prompt) + roughTokenCount(reply);

      const arr = reply.match(/\[[\s\S]*\]/);
      if (arr) {
        const items = JSON.parse(arr[0]) as Array<{
          id: string; name: string; keywords: string[]; description: string;
        }>;
        for (const item of items) {
          out.set(item.id, {
            name: item.name,
            keywords: item.keywords ?? [],
            description: item.description ?? '',
          });
        }
      }
    } catch (err) {
      console.warn('[prowl:enrich] batch failed, using defaults', err);
      for (const comm of chunk) {
        out.set(comm.id, defaultEnrichment(comm.heuristicLabel));
      }
    }
  }

  /* Backfill any gaps */
  for (const comm of communities) {
    if (!out.has(comm.id)) out.set(comm.id, defaultEnrichment(comm.heuristicLabel));
  }

  return { enrichments: out, tokensUsed: tokens };
}
