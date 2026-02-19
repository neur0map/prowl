/**
 * Processes Panel — Prowl
 *
 * Lists detected execution flows grouped by entry file.
 * Clicking a process opens the ProcessFlowModal.
 */

import { useState, useMemo, useCallback, useEffect } from 'react';
import { GitBranch, Search, Eye, ChevronDown, ChevronRight, Loader2, ArrowRight } from 'lucide-react';
import { useAppState } from '../hooks/useAppState';
import { ProcessFlowModal } from './ProcessFlowModal';
import type { ProcessData, ProcessStep } from '../lib/mermaid-generator';

interface ProcessItem {
  id: string;
  label: string;
  stepCount: number;
  clusters: string[];
  processType: string;
  entryFile: string;
}

export const ProcessesPanel = () => {
  const { graph, runQuery, setHighlightedNodeIds, highlightedNodeIds } = useAppState();
  const [searchQuery, setSearchQuery] = useState('');
  const [selectedProcess, setSelectedProcess] = useState<ProcessData | null>(null);
  const [expandedSections, setExpandedSections] = useState<Set<string>>(new Set());
  const [loadingProcess, setLoadingProcess] = useState<string | null>(null);
  const [focusedProcessId, setFocusedProcessId] = useState<string | null>(null);
  const [processStepsCache, setProcessStepsCache] = useState<Map<string, string[]>>(new Map());

  // Extract processes from graph, grouped by entry file
  const { allProcesses, byFile, totalCount } = useMemo(() => {
    if (!graph) return { allProcesses: [], byFile: new Map<string, ProcessItem[]>(), totalCount: 0 };

    const processNodes = graph.nodes.filter(n => n.label === 'Process');
    const items: ProcessItem[] = [];

    // Build a lookup: node ID → filePath (for grouping by entry file)
    const nodeFileMap = new Map<string, string>();
    for (const n of graph.nodes) {
      if (n.properties.filePath) {
        nodeFileMap.set(n.id, n.properties.filePath);
      }
    }

    for (const node of processNodes) {
      const label = node.properties.heuristicLabel || node.properties.name || node.id;
      const entryPointId = node.properties.entryPointId || '';
      // Resolve entry file from the entry point node
      const entryFilePath = nodeFileMap.get(entryPointId) || '';
      const entryFile = entryFilePath ? (entryFilePath.split('/').pop() || entryFilePath) : (label.split('→')[0]?.trim() || 'unknown');

      items.push({
        id: node.id,
        label,
        stepCount: node.properties.stepCount || 0,
        clusters: node.properties.communities || [],
        processType: node.properties.processType || 'intra_community',
        entryFile,
      });
    }

    items.sort((a, b) => b.stepCount - a.stepCount);

    // Group by entry file
    const grouped = new Map<string, ProcessItem[]>();
    for (const item of items) {
      const group = grouped.get(item.entryFile) || [];
      group.push(item);
      grouped.set(item.entryFile, group);
    }

    return { allProcesses: items, byFile: grouped, totalCount: items.length };
  }, [graph]);

  // Auto-expand first 3 file groups
  useEffect(() => {
    const keys = Array.from(byFile.keys()).slice(0, 3);
    setExpandedSections(new Set(keys));
  }, [byFile]);

  // Filter by search
  const filteredByFile = useMemo(() => {
    if (!searchQuery.trim()) return byFile;
    const q = searchQuery.toLowerCase();
    const result = new Map<string, ProcessItem[]>();
    for (const [file, items] of byFile) {
      const matched = items.filter(p => p.label.toLowerCase().includes(q) || file.toLowerCase().includes(q));
      if (matched.length > 0) result.set(file, matched);
    }
    return result;
  }, [byFile, searchQuery]);

  const toggleSection = useCallback((section: string) => {
    setExpandedSections(prev => {
      const next = new Set(prev);
      next.has(section) ? next.delete(section) : next.add(section);
      return next;
    });
  }, []);

  // View combined map
  const handleViewAllProcesses = useCallback(async () => {
    setLoadingProcess('all');
    try {
      const allIds = allProcesses.map(p => p.id);
      if (allIds.length === 0) return;

      const allStepsMap = new Map<string, ProcessStep>();
      const allEdges: Array<{ from: string; to: string; type: string }> = [];

      const stepsQuery = `
        MATCH (s)-[r:CodeRelation {type: 'STEP_IN_PROCESS'}]->(p:Process)
        WHERE p.id IN [${allIds.map(id => `'${id.replace(/'/g, "''")}'`).join(',')}]
        RETURN s.id AS id, s.name AS name, s.filePath AS filePath, r.step AS stepNumber
      `;
      const stepsResult = await runQuery(stepsQuery);
      for (const row of stepsResult) {
        const stepId = row.id || row[0];
        if (!allStepsMap.has(stepId)) {
          allStepsMap.set(stepId, {
            id: stepId,
            name: row.name || row[1] || 'Unknown',
            filePath: row.filePath || row[2],
            stepNumber: row.stepNumber || row.step || row[3] || 0,
          });
        }
      }

      const allSteps = Array.from(allStepsMap.values());
      const stepIds = allSteps.map(s => s.id);

      if (stepIds.length > 0) {
        const edgesQuery = `
          MATCH (from)-[r:CodeRelation {type: 'CALLS'}]->(to)
          WHERE from.id IN [${stepIds.map(id => `'${id.replace(/'/g, "''")}'`).join(',')}]
            AND to.id IN [${stepIds.map(id => `'${id.replace(/'/g, "''")}'`).join(',')}]
          RETURN from.id AS fromId, to.id AS toId, r.type AS type
        `;
        try {
          const edgesResult = await runQuery(edgesQuery);
          allEdges.push(...edgesResult
            .map((row: any) => ({ from: row.fromId || row[0], to: row.toId || row[1], type: row.type || row[2] || 'CALLS' }))
            .filter(edge => edge.from !== edge.to));
        } catch { /* continue without edges */ }
      }

      setSelectedProcess({
        id: 'combined-all',
        label: `All Processes (${allIds.length})`,
        processType: 'cross_community',
        steps: allSteps,
        edges: allEdges,
        clusters: [],
      });
    } catch (error) {
      console.error('Failed to load combined processes:', error);
    } finally {
      setLoadingProcess(null);
    }
  }, [allProcesses, runQuery]);

  // View single process
  const handleViewProcess = useCallback(async (process: ProcessItem) => {
    setLoadingProcess(process.id);
    try {
      const stepsQuery = `
        MATCH (s)-[r:CodeRelation {type: 'STEP_IN_PROCESS'}]->(p:Process {id: '${process.id.replace(/'/g, "''")}'})
        RETURN s.id AS id, s.name AS name, s.filePath AS filePath, r.step AS stepNumber
        ORDER BY r.step
      `;
      const stepsResult = await runQuery(stepsQuery);
      const steps: ProcessStep[] = stepsResult.map((row: any) => ({
        id: row.id || row[0],
        name: row.name || row[1] || 'Unknown',
        filePath: row.filePath || row[2],
        stepNumber: row.stepNumber || row.step || row[3] || 0,
      }));

      const stepIds = steps.map(s => s.id);
      let edges: Array<{ from: string; to: string; type: string }> = [];

      if (stepIds.length > 0) {
        const edgesQuery = `
          MATCH (from)-[r:CodeRelation {type: 'CALLS'}]->(to)
          WHERE from.id IN [${stepIds.map(id => `'${id.replace(/'/g, "''")}'`).join(',')}]
            AND to.id IN [${stepIds.map(id => `'${id.replace(/'/g, "''")}'`).join(',')}]
          RETURN from.id AS fromId, to.id AS toId, r.type AS type
        `;
        try {
          const edgesResult = await runQuery(edgesQuery);
          edges = edgesResult
            .map((row: any) => ({ from: row.fromId || row[0], to: row.toId || row[1], type: row.type || row[2] || 'CALLS' }))
            .filter(edge => edge.from !== edge.to);
        } catch { /* fallback to linear */ }
      }

      const processNode = graph?.nodes.find(n => n.id === process.id);
      setSelectedProcess({
        id: process.id,
        label: process.label,
        processType: process.processType as 'cross_community' | 'intra_community',
        steps,
        edges,
        clusters: processNode?.properties.communities || [],
      });
    } catch (error) {
      console.error('Failed to load process steps:', error);
    } finally {
      setLoadingProcess(null);
    }
  }, [runQuery, graph]);

  // Toggle focus highlight
  const handleToggleFocus = useCallback(async (processId: string) => {
    if (focusedProcessId === processId) {
      setHighlightedNodeIds(new Set());
      setFocusedProcessId(null);
      return;
    }

    if (processStepsCache.has(processId)) {
      setHighlightedNodeIds(new Set(processStepsCache.get(processId)!));
      setFocusedProcessId(processId);
      return;
    }

    setLoadingProcess(processId);
    try {
      const stepsQuery = `
        MATCH (s)-[r:CodeRelation {type: 'STEP_IN_PROCESS'}]->(p:Process {id: '${processId.replace(/'/g, "''")}'})
        RETURN s.id AS id
      `;
      const stepsResult = await runQuery(stepsQuery);
      const stepIds = stepsResult.map((row: any) => row.id || row[0]);
      setProcessStepsCache(prev => new Map(prev).set(processId, stepIds));
      setHighlightedNodeIds(new Set(stepIds));
      setFocusedProcessId(processId);
    } catch (error) {
      console.error('Failed to load process steps for focus:', error);
    } finally {
      setLoadingProcess(null);
    }
  }, [focusedProcessId, processStepsCache, runQuery, setHighlightedNodeIds]);

  // Focus from modal
  const handleFocusInGraph = useCallback((nodeIds: string[], processId: string) => {
    if (focusedProcessId === processId) {
      setHighlightedNodeIds(new Set());
      setFocusedProcessId(null);
    } else {
      setHighlightedNodeIds(new Set(nodeIds));
      setFocusedProcessId(processId);
      setProcessStepsCache(prev => new Map(prev).set(processId, nodeIds));
    }
  }, [focusedProcessId, setHighlightedNodeIds]);

  // Sync focus clear
  useEffect(() => {
    if (highlightedNodeIds.size === 0 && focusedProcessId !== null) {
      setFocusedProcessId(null);
    }
  }, [highlightedNodeIds, focusedProcessId]);

  if (totalCount === 0) {
    return (
      <div className="flex flex-col items-center justify-center h-full p-6 text-center">
        <GitBranch className="w-5 h-5 text-text-muted mb-3" />
        <p className="text-[13px] text-text-muted">No processes detected</p>
        <p className="text-[11px] text-text-muted/50 mt-1 max-w-[200px]">
          Load a codebase to trace execution flows
        </p>
      </div>
    );
  }

  return (
    <div className="flex flex-col h-full">
      {/* Search */}
      <div className="px-4 pt-3 pb-2">
        <div className="flex items-center gap-2 px-2.5 py-1.5 border-b border-white/[0.08] focus-within:border-white/[0.25] transition-colors">
          <Search className="w-3.5 h-3.5 text-text-muted/50" />
          <input
            type="text"
            value={searchQuery}
            onChange={(e) => setSearchQuery(e.target.value)}
            placeholder="Filter..."
            className="flex-1 bg-transparent border-none outline-none text-[13px] text-text-primary placeholder:text-text-muted/40"
          />
        </div>
        <div className="flex items-center justify-between mt-2">
          <span className="text-[11px] text-text-muted/50">{totalCount} processes</span>
          <button
            onClick={handleViewAllProcesses}
            disabled={loadingProcess !== null}
            className="text-[11px] text-text-muted hover:text-text-primary transition-colors disabled:opacity-40"
          >
            {loadingProcess === 'all' ? (
              <Loader2 className="w-3 h-3 animate-spin inline mr-1" />
            ) : null}
            View combined map
            <ArrowRight className="w-3 h-3 inline ml-0.5" />
          </button>
        </div>
      </div>

      {/* Process list grouped by file */}
      <div className="flex-1 overflow-y-auto scrollbar-thin">
        {Array.from(filteredByFile.entries()).map(([file, items]) => (
          <div key={file}>
            {/* File group header */}
            <button
              onClick={() => toggleSection(file)}
              className="w-full flex items-center gap-2 px-4 py-2 text-left hover:bg-white/[0.03] transition-colors"
            >
              {expandedSections.has(file) ? (
                <ChevronDown className="w-3 h-3 text-text-muted/40" />
              ) : (
                <ChevronRight className="w-3 h-3 text-text-muted/40" />
              )}
              <span className="text-[12px] font-mono text-text-muted truncate">{file}</span>
              <span className="ml-auto text-[10px] text-text-muted/40">{items.length}</span>
            </button>

            {/* Process rows */}
            {expandedSections.has(file) && (
              <div className="pb-1">
                {items.map((process) => {
                  const isFocused = focusedProcessId === process.id;
                  const isLoading = loadingProcess === process.id;

                  return (
                    <div
                      key={process.id}
                      className={`flex items-center gap-2 pl-8 pr-4 py-1.5 group transition-colors cursor-default ${
                        isFocused ? 'bg-white/[0.06]' : 'hover:bg-white/[0.03]'
                      }`}
                    >
                      <div className="flex-1 min-w-0">
                        <div className="text-[12px] text-text-secondary truncate group-hover:text-text-primary transition-colors">
                          {process.label}
                        </div>
                        <div className="text-[10px] text-text-muted/40">
                          {process.stepCount} steps
                        </div>
                      </div>

                      {/* Actions — show on hover */}
                      <div className={`flex items-center gap-1 transition-opacity ${isFocused ? 'opacity-100' : 'opacity-0 group-hover:opacity-100'}`}>
                        <button
                          onClick={() => handleToggleFocus(process.id)}
                          className={`px-2 py-0.5 rounded text-[10px] transition-colors ${
                            isFocused
                              ? 'text-accent bg-accent/10'
                              : 'text-text-muted hover:text-text-secondary bg-white/[0.04] hover:bg-white/[0.06]'
                          }`}
                        >
                          {isFocused ? 'Unfocus' : 'Focus'}
                        </button>
                        <button
                          onClick={() => handleViewProcess(process)}
                          disabled={isLoading}
                          className="px-2 py-0.5 rounded text-[10px] text-text-muted hover:text-text-primary bg-white/[0.04] hover:bg-white/[0.06] transition-colors disabled:opacity-40"
                        >
                          {isLoading ? <Loader2 className="w-3 h-3 animate-spin" /> : (
                            <><Eye className="w-3 h-3 inline mr-0.5" />View</>
                          )}
                        </button>
                      </div>
                    </div>
                  );
                })}
              </div>
            )}
          </div>
        ))}
      </div>

      {/* Modal */}
      <ProcessFlowModal
        process={selectedProcess}
        onClose={() => setSelectedProcess(null)}
        onFocusInGraph={handleFocusInGraph}
        isFullScreen={selectedProcess?.id === 'combined-all'}
      />
    </div>
  );
};
