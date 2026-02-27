/**
 * Ranks functions as potential entry points using four weighted
 * signals: call fan-out ratio, export visibility, naming
 * conventions, and framework-aware path multipliers.
 */

import { detectFrameworkFromPath } from './framework-detection';

/* ── Demotion patterns (utility / accessor names) ─────── */

const DEMOTION_RULES: RegExp[] = [
  /^(get|set|is|has|can|should|will|did)[A-Z]/,
  /^_/,
  /^(format|parse|validate|convert|transform)/i,
  /^(log|debug|error|warn|info)$/i,
  /^(to|from)[A-Z]/,
  /^(encode|decode)/i,
  /^(serialize|deserialize)/i,
  /^(clone|copy|deep)/i,
  /^(merge|extend|assign)/i,
  /^(filter|map|reduce|sort|find)/i,
  /Helper$/,
  /Util$/,
  /Utils$/,
  /^utils?$/i,
  /^helpers?$/i,
];

/* ── Promotion patterns (per language) ────────────────── */

const PROMOTION_RULES: Record<string, RegExp[]> = {
  '*': [
    /^(main|init|bootstrap|start|run|setup|configure)$/i,
    /^handle[A-Z]/, /^on[A-Z]/,
    /Handler$/, /Controller$/,
    /^process[A-Z]/, /^execute[A-Z]/, /^perform[A-Z]/,
    /^dispatch[A-Z]/, /^trigger[A-Z]/, /^fire[A-Z]/, /^emit[A-Z]/,
  ],
  javascript: [/^use[A-Z]/],
  typescript: [/^use[A-Z]/],
  python: [/^app$/, /^(get|post|put|delete|patch)_/i, /^api_/, /^view_/],
  java: [/^do[A-Z]/, /^create[A-Z]/, /^build[A-Z]/, /Service$/],
  csharp: [/^(Get|Post|Put|Delete)/, /Action$/, /^On[A-Z]/, /Async$/],
  go: [/Handler$/, /^Serve/, /^New[A-Z]/, /^Make[A-Z]/],
  rust: [/^(get|post|put|delete)_handler$/i, /^handle_/, /^new$/, /^run$/, /^spawn/],
  c: [/^main$/, /^init_/, /^start_/, /^run_/],
  cpp: [/^main$/, /^init_/, /^Create[A-Z]/, /^Run$/, /^Start$/],
};

/* ── Public API ───────────────────────────────────────── */

export interface EntryPointScoreResult {
  score: number;
  reasons: string[];
}

/**
 * Produce a composite importance score for a single function
 * along with human-readable tags explaining each factor.
 */
export function calculateEntryPointScore(
  name: string,
  language: string,
  isExported: boolean,
  callerCount: number,
  calleeCount: number,
  filePath = '',
): EntryPointScoreResult {
  const tags: string[] = [];

  /* Cannot anchor a trace without outgoing calls */
  if (calleeCount === 0) return { score: 0, reasons: ['no-outgoing-calls'] };

  /* Factor 1 — fan-out ratio */
  const ratio = calleeCount / (callerCount + 1);
  tags.push(`base:${ratio.toFixed(2)}`);

  /* Factor 2 — visibility boost */
  const visFactor = isExported ? 2.0 : 1.0;
  if (isExported) tags.push('exported');

  /* Factor 3 — naming convention */
  let nameFactor = 1.0;
  if (DEMOTION_RULES.some(rx => rx.test(name))) {
    nameFactor = 0.3;
    tags.push('utility-pattern');
  } else {
    const universal = PROMOTION_RULES['*'] ?? [];
    const langRules = PROMOTION_RULES[language] ?? [];
    if ([...universal, ...langRules].some(rx => rx.test(name))) {
      nameFactor = 1.5;
      tags.push('entry-pattern');
    }
  }

  /* Factor 4 — framework path hint */
  let fwFactor = 1.0;
  if (filePath) {
    const hint = detectFrameworkFromPath(filePath);
    if (hint) {
      fwFactor = hint.entryPointMultiplier;
      tags.push(`framework:${hint.reason}`);
    }
  }

  return { score: ratio * visFactor * nameFactor * fwFactor, reasons: tags };
}

/* ── File classification helpers ──────────────────────── */

/** True when the path belongs to a test/spec directory or file. */
export function isTestFile(filePath: string): boolean {
  const p = filePath.toLowerCase().replace(/\\/g, '/');

  const markers = [
    '.test.', '.spec.',
    '__tests__/', '__mocks__/',
    '/test/', '/tests/', '/testing/',
    '/src/test/', '.tests/',
    'tests.cs',
  ];
  if (markers.some(m => p.includes(m))) return true;
  if (p.endsWith('_test.py') || p.endsWith('_test.go')) return true;
  if (p.includes('/test_')) return true;
  return false;
}

/** True when the path sits inside a utility/helper directory. */
export function isUtilityFile(filePath: string): boolean {
  const p = filePath.toLowerCase().replace(/\\/g, '/');

  const dirs = ['/utils/', '/util/', '/helpers/', '/helper/', '/common/', '/shared/', '/lib/'];
  const suffixes = ['/utils.ts', '/utils.js', '/helpers.ts', '/helpers.js', '_utils.py', '_helpers.py'];

  return dirs.some(d => p.includes(d)) || suffixes.some(s => p.endsWith(s));
}
