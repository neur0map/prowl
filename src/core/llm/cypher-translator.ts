/**
 * Natural Language → Cypher Translator
 *
 * Translates user questions into KuzuDB Cypher queries using the
 * configured LLM provider. One-shot HTTP call on the renderer thread.
 */

import { SystemMessage, HumanMessage } from '@langchain/core/messages';
import type { BaseChatModel } from '@langchain/core/language_models/chat_models';
import { createChatModel } from './agent';
import { KUZU_SCHEMA_REF } from './types';
import type { ProviderConfig } from './types';

// ── System prompt ──

const CYPHER_SYSTEM_PROMPT = `You are a Cypher query generator for a KuzuDB code knowledge graph.
Given a natural language question about a codebase, output ONLY the Cypher query — no explanation, no markdown fences, no commentary.

Rules:
1. Always project explicit properties (never \`RETURN n\` — use \`RETURN n.name, n.filePath\` etc.)
2. Always include a LIMIT clause (default 50)
3. WITH + ORDER BY must include LIMIT or SKIP
4. If you cannot map the question to the schema, output exactly: -- CANNOT_TRANSLATE
5. Use only the node types, edge types, and properties defined in the schema below
6. Output a single Cypher statement only

${KUZU_SCHEMA_REF}`;

// ── Model cache ──

let cachedModel: BaseChatModel | null = null;
let cachedConfigHash: string = '';

function hashConfig(config: ProviderConfig): string {
  return JSON.stringify(config);
}

async function getOrCreateModel(config: ProviderConfig): Promise<BaseChatModel> {
  const hash = hashConfig(config);
  if (cachedModel && cachedConfigHash === hash) {
    return cachedModel;
  }
  cachedModel = await createChatModel(config);
  cachedConfigHash = hash;
  return cachedModel;
}

// ── Valid Cypher start keywords ──

const CYPHER_KEYWORDS = /^\s*(MATCH|CALL|RETURN|WITH|UNWIND|CREATE|MERGE|OPTIONAL)/i;

// ── Post-processing ──

function stripCodeFences(text: string): string {
  // Remove ```cypher ... ``` or ``` ... ```
  return text.replace(/^```(?:cypher)?\s*\n?/i, '').replace(/\n?```\s*$/i, '');
}

function extractFirstStatement(text: string): string {
  // Take only the first statement if multiple are separated by ;
  const idx = text.indexOf(';');
  return idx >= 0 ? text.slice(0, idx).trim() : text.trim();
}

// ── Public API ──

export interface CypherTranslationResult {
  cypher: string;
  rawResponse: string;
  cannotTranslate: boolean;
}

export async function translateNLToCypher(
  question: string,
  config: ProviderConfig,
): Promise<CypherTranslationResult> {
  const model = await getOrCreateModel(config);

  const response = await model.invoke([
    new SystemMessage(CYPHER_SYSTEM_PROMPT),
    new HumanMessage(question),
  ]);

  const rawResponse = typeof response.content === 'string'
    ? response.content
    : Array.isArray(response.content)
      ? response.content
          .filter((block: any) => block.type === 'text' || typeof block === 'string')
          .map((block: any) => typeof block === 'string' ? block : block.text || '')
          .join('')
      : String(response.content);

  // Post-process
  let cypher = stripCodeFences(rawResponse).trim();
  cypher = extractFirstStatement(cypher);

  // Detect CANNOT_TRANSLATE sentinel
  const cannotTranslate = cypher.includes('-- CANNOT_TRANSLATE');

  // Validate that it looks like Cypher (unless it's the sentinel)
  if (!cannotTranslate && !CYPHER_KEYWORDS.test(cypher)) {
    return {
      cypher,
      rawResponse,
      cannotTranslate: false,
    };
  }

  return {
    cypher,
    rawResponse,
    cannotTranslate,
  };
}
