import type { CodeGraph, GraphNode, GraphRelationship } from '../graph/types';
import { createCodeGraph } from '../graph/graph';
import type { FileEntry } from '../../types/file-entry';
import type { IndexingProgress, IndexingResult } from '../../types/pipeline';
import type { DiffResult } from './types';

/**
 * Re-run all parse phases (structure, parsing, imports, calls, heritage) on a
 * set of changed files and insert the resulting nodes/edges into `graph`.
 *
 * Shared by `applyIncrementalUpdate` (snapshot restore) and `liveUpdate` (real-time).
 */
export async function reparseFiles(
  graph: CodeGraph,
  changedFiles: Array<{ path: string; content: string }>,
  allFileContents: Map<string, string>,
): Promise<void> {
  if (changedFiles.length === 0) return;

  const { processStructure } = await import('../ingestion/structure-processor');
  const { processParsing } = await import('../ingestion/parsing-processor');
  const { processImports, createImportMap } = await import('../ingestion/import-processor');
  const { processCalls } = await import('../ingestion/call-processor');
  const { processHeritage } = await import('../ingestion/heritage-processor');
  const { createSymbolTable } = await import('../ingestion/symbol-table');
  const { createASTCache } = await import('../ingestion/ast-cache');

  const symbolTable = createSymbolTable();
  const astCache = createASTCache(50);
  const importMap = createImportMap();

  const allPaths = Array.from(allFileContents.keys());
  processStructure(graph, allPaths);

  await processParsing(graph, changedFiles, symbolTable, astCache);
  await processImports(graph, changedFiles, astCache, importMap, () => {});
  await processCalls(graph, changedFiles, astCache, symbolTable, importMap, () => {});
  await processHeritage(graph, changedFiles, astCache, symbolTable, () => {});

  astCache.clear();
}

/**
 * Apply an incremental update to an existing graph based on file changes.
 *
 * Steps:
 * 1. Remove all nodes/edges from deleted/modified files
 * 2. Re-run structure, parsing, imports, calls, heritage on changed files
 * 3. Re-run communities + processes on full graph (requires full connectivity)
 * 4. Return new IndexingResult
 */
export async function applyIncrementalUpdate(
  diff: DiffResult,
  newFileContents: Map<string, string>,
  existingGraph: CodeGraph,
  existingFileContents: Map<string, string>,
  onProgress: (p: IndexingProgress) => void,
): Promise<IndexingResult> {
  const changedPaths = new Set([...diff.deleted, ...diff.modified]);

  onProgress({
    phase: 'structure',
    percent: 10,
    message: `Incremental update: ${diff.added.length} added, ${diff.modified.length} modified, ${diff.deleted.length} deleted`,
  });

  // Step 1: Build a new graph — keep File/Folder for modified files, remove symbols
  const graph = createCodeGraph();
  const nodesToRemove = new Set<string>();
  const deletedPaths = new Set(diff.deleted);

  for (const node of existingGraph.nodes) {
    if (node.label === 'Community' || node.label === 'Process') continue;
    const filePath = node.properties.filePath;
    if (filePath && changedPaths.has(filePath)) {
      if (deletedPaths.has(filePath)) {
        nodesToRemove.add(node.id);
      } else if (node.label === 'File' || node.label === 'Folder') {
        graph.addNode(node);
      } else {
        nodesToRemove.add(node.id);
      }
    } else {
      graph.addNode(node);
    }
  }

  // Keep edges; defer incoming edges to removed symbols for post-reparse restore
  const deferredEdges: GraphRelationship[] = [];
  for (const rel of existingGraph.relationships) {
    if (rel.type === 'MEMBER_OF' || rel.type === 'STEP_IN_PROCESS') continue;
    if (nodesToRemove.has(rel.sourceId)) continue;
    if (nodesToRemove.has(rel.targetId)) {
      deferredEdges.push(rel);
      continue;
    }
    graph.addRelationship(rel);
  }

  // Step 2: Update fileContents — remove deleted, update modified, add new
  const updatedFileContents = new Map(existingFileContents);
  for (const path of diff.deleted) {
    updatedFileContents.delete(path);
  }
  for (const [path, content] of newFileContents) {
    updatedFileContents.set(path, content);
  }

  // Prepare the changed files list
  const changedFiles: FileEntry[] = [];
  for (const path of [...diff.added, ...diff.modified]) {
    const content = newFileContents.get(path);
    if (content) {
      changedFiles.push({ path, content });
    }
  }

  if (changedFiles.length > 0) {
    onProgress({
      phase: 'structure',
      percent: 15,
      message: `Re-indexing ${changedFiles.length} files...`,
    });

    await reparseFiles(graph, changedFiles, updatedFileContents);

    // Restore deferred edges whose target was recreated by reparsing
    for (const rel of deferredEdges) {
      if (graph.hasNode(rel.targetId)) {
        graph.addRelationship(rel);
      }
    }

    onProgress({ phase: 'heritage', percent: 80, message: 'Re-indexing complete' });
  }

  // Step 3: Re-run communities on FULL graph
  onProgress({ phase: 'communities', percent: 85, message: 'Re-clustering communities...' });

  const { processCommunities } = await import('../ingestion/community-processor');
  const communityResult = await processCommunities(graph, (message, progress) => {
    onProgress({ phase: 'communities', percent: 85 + Math.round(progress * 0.05), message });
  });

  // Add community nodes and memberships
  for (const comm of communityResult.communities) {
    graph.addNode({
      id: comm.id,
      label: 'Community',
      properties: {
        name: comm.label,
        filePath: '',
        heuristicLabel: comm.heuristicLabel,
        cohesion: comm.cohesion,
        symbolCount: comm.symbolCount,
      },
    });
  }
  for (const membership of communityResult.memberships) {
    graph.addRelationship({
      id: `${membership.nodeId}_member_of_${membership.communityId}`,
      type: 'MEMBER_OF',
      sourceId: membership.nodeId,
      targetId: membership.communityId,
      confidence: 1.0,
      reason: 'louvain-algorithm',
    });
  }

  // Step 4: Re-run processes on FULL graph
  onProgress({ phase: 'processes', percent: 92, message: 'Re-mapping execution flows...' });

  const { processProcesses } = await import('../ingestion/process-processor');
  const processResult = await processProcesses(graph, communityResult.memberships, () => {});

  for (const proc of processResult.processes) {
    graph.addNode({
      id: proc.id,
      label: 'Process',
      properties: {
        name: proc.label,
        filePath: '',
        heuristicLabel: proc.heuristicLabel,
        processType: proc.processType,
        stepCount: proc.stepCount,
        communities: proc.communities,
        entryPointId: proc.entryPointId,
        terminalId: proc.terminalId,
      },
    });
  }
  for (const step of processResult.steps) {
    graph.addRelationship({
      id: `${step.nodeId}_step_${step.step}_${step.processId}`,
      type: 'STEP_IN_PROCESS',
      sourceId: step.nodeId,
      targetId: step.processId,
      confidence: 1.0,
      reason: 'trace-detection',
      step: step.step,
    });
  }

  onProgress({
    phase: 'complete',
    percent: 100,
    message: `Incremental update complete! ${changedFiles.length} files re-indexed.`,
    stats: {
      filesProcessed: updatedFileContents.size,
      totalFiles: updatedFileContents.size,
      nodesCreated: graph.nodeCount,
    },
  });

  return { graph, fileContents: updatedFileContents, communityResult, processResult };
}
