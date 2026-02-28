/**
 * Orchestrates the full indexing pipeline: extract → structure →
 * parse → imports → calls → heritage → communities → processes.
 */

import { createCodeGraph } from '../graph/graph';
import type { FileEntry } from '../../types/file-entry';
import { shouldIgnorePath } from '../../config/ignore-service';
import { processStructure } from './structure-processor';
import { processParsing } from './parsing-processor';
import { processImports, createImportMap } from './import-processor';
import { processCalls } from './call-processor';
import { processHeritage } from './heritage-processor';
import { processCommunities, type CommunityDetectionResult } from './community-processor';
import { processProcesses, type ProcessDetectionResult } from './process-processor';
import { createSymbolTable } from './symbol-table';
import { createASTCache } from './ast-cache';
import type { IndexingProgress, IndexingResult } from '../../types/pipeline';

/* ── Progress helpers ─────────────────────────────────── */

function emit(
  cb: (p: IndexingProgress) => void,
  phase: IndexingProgress['phase'],
  pct: number,
  message: string,
  extra?: Partial<Pick<IndexingProgress, 'detail' | 'stats'>>,
): void {
  cb({ phase, percent: pct, message, ...extra });
}

function snap(processed: number, total: number, nodes: number): IndexingProgress['stats'] {
  return { filesProcessed: processed, totalFiles: total, nodesCreated: nodes };
}

/* ── Phase runners ────────────────────────────────────── */

function buildTree(
  graph: ReturnType<typeof createCodeGraph>,
  entries: FileEntry[],
  cb: (p: IndexingProgress) => void,
): void {
  emit(cb, 'structure', 15, 'Mapping file tree...', { stats: snap(0, entries.length, 0) });
  processStructure(graph, entries.map(e => e.path));
  emit(cb, 'structure', 30, 'File tree mapped', { stats: snap(entries.length, entries.length, graph.nodeCount) });
}

async function parseSymbols(
  graph: ReturnType<typeof createCodeGraph>,
  entries: FileEntry[],
  symbols: ReturnType<typeof createSymbolTable>,
  cache: ReturnType<typeof createASTCache>,
  cb: (p: IndexingProgress) => void,
): Promise<void> {
  emit(cb, 'parsing', 30, 'Extracting symbols...', { stats: snap(0, entries.length, graph.nodeCount) });
  await processParsing(graph, entries, symbols, cache, (done, total, file) => {
    emit(cb, 'parsing', Math.round(30 + (done / total) * 40), 'Extracting symbols...', {
      detail: file,
      stats: snap(done, total, graph.nodeCount),
    });
  });
}

async function linkImports(
  graph: ReturnType<typeof createCodeGraph>,
  entries: FileEntry[],
  cache: ReturnType<typeof createASTCache>,
  impMap: ReturnType<typeof createImportMap>,
  cb: (p: IndexingProgress) => void,
): Promise<void> {
  emit(cb, 'imports', 70, 'Resolving dependencies...', { stats: snap(0, entries.length, graph.nodeCount) });
  await processImports(graph, entries, cache, impMap, (done, total) => {
    emit(cb, 'imports', Math.round(70 + (done / total) * 12), 'Resolving dependencies...', {
      stats: snap(done, total, graph.nodeCount),
    });
  });
}

async function linkCalls(
  graph: ReturnType<typeof createCodeGraph>,
  entries: FileEntry[],
  cache: ReturnType<typeof createASTCache>,
  symbols: ReturnType<typeof createSymbolTable>,
  impMap: ReturnType<typeof createImportMap>,
  cb: (p: IndexingProgress) => void,
): Promise<void> {
  emit(cb, 'calls', 82, 'Tracing call graph...', { stats: snap(0, entries.length, graph.nodeCount) });
  await processCalls(graph, entries, cache, symbols, impMap, (done, total) => {
    emit(cb, 'calls', Math.round(82 + (done / total) * 10), 'Tracing call graph...', {
      stats: snap(done, total, graph.nodeCount),
    });
  });
}

async function linkHeritage(
  graph: ReturnType<typeof createCodeGraph>,
  entries: FileEntry[],
  cache: ReturnType<typeof createASTCache>,
  symbols: ReturnType<typeof createSymbolTable>,
  cb: (p: IndexingProgress) => void,
): Promise<void> {
  emit(cb, 'heritage', 92, 'Linking inheritance chains...', { stats: snap(0, entries.length, graph.nodeCount) });
  await processHeritage(graph, entries, cache, symbols, (done, total) => {
    emit(cb, 'heritage', Math.round(88 + (done / total) * 4), 'Linking inheritance chains...', {
      stats: snap(done, total, graph.nodeCount),
    });
  });
}

async function detectCommunities(
  graph: ReturnType<typeof createCodeGraph>,
  fileCount: number,
  cb: (p: IndexingProgress) => void,
): Promise<CommunityDetectionResult> {
  emit(cb, 'communities', 92, 'Clustering modules...', { stats: snap(fileCount, fileCount, graph.nodeCount) });
  return processCommunities(graph, (msg, pct) => {
    emit(cb, 'communities', Math.round(92 + pct * 0.06), msg, {
      stats: snap(fileCount, fileCount, graph.nodeCount),
    });
  });
}

function applyCommunities(
  graph: ReturnType<typeof createCodeGraph>,
  data: CommunityDetectionResult,
): void {
  for (const c of data.communities) {
    graph.addNode({
      id: c.id,
      label: 'Community' as const,
      properties: {
        name: c.label,
        filePath: '',
        heuristicLabel: c.heuristicLabel,
        cohesion: c.cohesion,
        symbolCount: c.symbolCount,
      },
    });
  }
  for (const m of data.memberships) {
    graph.addRelationship({
      id: `${m.nodeId}_member_of_${m.communityId}`,
      type: 'MEMBER_OF',
      sourceId: m.nodeId,
      targetId: m.communityId,
      confidence: 1.0,
      reason: 'louvain-algorithm',
    });
  }
}

async function detectProcesses(
  graph: ReturnType<typeof createCodeGraph>,
  memberships: CommunityDetectionResult['memberships'],
  fileCount: number,
  cb: (p: IndexingProgress) => void,
): Promise<ProcessDetectionResult> {
  emit(cb, 'processes', 98, 'Mapping execution flows...', { stats: snap(fileCount, fileCount, graph.nodeCount) });
  return processProcesses(graph, memberships, (msg, pct) => {
    emit(cb, 'processes', Math.round(98 + pct * 0.01), msg, {
      stats: snap(fileCount, fileCount, graph.nodeCount),
    });
  });
}

function applyProcesses(
  graph: ReturnType<typeof createCodeGraph>,
  data: ProcessDetectionResult,
): void {
  for (const p of data.processes) {
    graph.addNode({
      id: p.id,
      label: 'Process' as const,
      properties: {
        name: p.label,
        filePath: '',
        heuristicLabel: p.heuristicLabel,
        processType: p.processType,
        stepCount: p.stepCount,
        communities: p.communities,
        entryPointId: p.entryPointId,
        terminalId: p.terminalId,
      },
    });
  }
  for (const s of data.steps) {
    graph.addRelationship({
      id: `${s.nodeId}_step_${s.step}_${s.processId}`,
      type: 'STEP_IN_PROCESS',
      sourceId: s.nodeId,
      targetId: s.processId,
      confidence: 1.0,
      reason: 'trace-detection',
      step: s.step,
    });
  }
}

/* ── Public API ───────────────────────────────────────── */

export async function runPipelineFromFiles(
  files: FileEntry[],
  onProgress: (progress: IndexingProgress) => void,
): Promise<IndexingResult> {
  const graph = createCodeGraph();
  const fileContents = new Map<string, string>();
  const symbols = createSymbolTable();
  const cache = createASTCache(50);
  const impMap = createImportMap();

  const cleanup = () => { cache.clear(); symbols.clear(); };

  try {
    /* Filter out files from ignored directories/patterns that slipped past
       the main-process scanner (e.g. node_modules, build output, binary assets). */
    const filtered = files.filter(f => !shouldIgnorePath(f.path));

    for (const f of filtered) fileContents.set(f.path, f.content);

    emit(onProgress, 'extracting', 15, 'Source files loaded', { stats: snap(0, filtered.length, 0) });

    buildTree(graph, filtered, onProgress);
    await parseSymbols(graph, filtered, symbols, cache, onProgress);
    await linkImports(graph, filtered, cache, impMap, onProgress);
    await linkCalls(graph, filtered, cache, symbols, impMap, onProgress);
    await linkHeritage(graph, filtered, cache, symbols, onProgress);

    const communityResult = await detectCommunities(graph, filtered.length, onProgress);
    applyCommunities(graph, communityResult);

    const processResult = await detectProcesses(graph, communityResult.memberships, filtered.length, onProgress);
    applyProcesses(graph, processResult);

    emit(
      onProgress, 'complete', 100,
      `Graph complete! ${communityResult.stats.totalCommunities} communities, ${processResult.stats.totalProcesses} processes detected.`,
      { stats: snap(filtered.length, filtered.length, graph.nodeCount) },
    );

    cache.clear();
    return { graph, fileContents, communityResult, processResult };
  } catch (err) {
    cleanup();
    throw err;
  }
}
