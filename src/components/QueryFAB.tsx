import { useState, useRef, useEffect, useCallback } from 'react';
import { flushSync } from 'react-dom';
import { Database, Play, X, ChevronDown, ChevronUp, Loader2, Code, Table, MessageSquare, AlertTriangle } from 'lucide-react';
import { useAppState } from '../hooks/useAppState';
import { getActiveProviderConfig, translateNLToCypher } from '../core/llm';

const PRESET_QUERIES = [
  { title: 'Show all functions',        cypher: `MATCH (n:Function) RETURN n.id AS id, n.name AS name, n.filePath AS path LIMIT 50` },
  { title: 'Show all classes',          cypher: `MATCH (n:Class) RETURN n.id AS id, n.name AS name, n.filePath AS path LIMIT 50` },
  { title: 'Show all interfaces',       cypher: `MATCH (n:Interface) RETURN n.id AS id, n.name AS name, n.filePath AS path LIMIT 50` },
  { title: 'Show the call graph',       cypher: `MATCH (a:File)-[r:CodeEdge {type: 'CALLS'}]->(b:Function) RETURN a.id AS id, a.name AS caller, b.name AS callee LIMIT 50` },
  { title: 'Show import relationships', cypher: `MATCH (a:File)-[r:CodeEdge {type: 'IMPORTS'}]->(b:File) RETURN a.id AS id, a.name AS from, b.name AS imports LIMIT 50` },
];

const NODE_ID_RE = /^(File|Function|Class|Method|Interface|Folder|CodeElement):/;
const CYPHER_KEYWORDS = /^\s*(MATCH|CALL|RETURN|WITH|UNWIND|CREATE|MERGE|OPTIONAL)/i;

function extractNodeIds(rows: Record<string, unknown>[]): string[] {
  const seen = new Set<string>();

  for (const row of rows) {
    if (Array.isArray(row)) {
      for (const cell of row) {
        if (typeof cell === 'string' && (NODE_ID_RE.test(cell) || cell.includes(':'))) {
          seen.add(cell);
        }
      }
    } else if (typeof row === 'object' && row !== null) {
      for (const [key, val] of Object.entries(row)) {
        if (typeof val !== 'string') continue;
        const k = key.toLowerCase();
        if (k.includes('id') || k === 'id' || NODE_ID_RE.test(val)) {
          seen.add(val);
        }
      }
    }
  }

  return Array.from(seen);
}

function ResultsTable({ rows, maxDisplay }: { rows: Record<string, unknown>[]; maxDisplay: number }) {
  const headers = Object.keys(rows[0]);
  const visible = rows.slice(0, maxDisplay);

  return (
    <div className="max-h-48 overflow-auto scrollbar-thin border-t border-border-subtle">
      <table className="w-full text-xs">
        <thead className="bg-surface sticky top-0">
          <tr>
            {headers.map(h => (
              <th key={h} className="px-3 py-2 text-left text-text-muted font-medium border-b border-border-subtle">
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {visible.map((row, ri) => (
            <tr key={ri} className="hover:bg-hover/50 transition-colors">
              {Object.values(row).map((cell, ci) => (
                <td key={ci} className="px-3 py-1.5 text-text-secondary border-b border-border-subtle/50 font-mono truncate max-w-[200px]">
                  {typeof cell === 'object' ? JSON.stringify(cell) : String(cell ?? '')}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
      {rows.length > maxDisplay && (
        <div className="px-3 py-2 text-xs text-text-muted bg-surface border-t border-border-subtle">
          Showing {maxDisplay} of {rows.length} rows
        </div>
      )}
    </div>
  );
}

export const QueryFAB = () => {
  const {
    setHighlightedNodeIds,
    setQueryResult,
    queryResult,
    clearQueryHighlights,
    graph,
    runQuery,
    isDatabaseReady,
  } = useAppState();

  const [panelOpen, setPanelOpen] = useState(false);
  const [inputText, setInputText] = useState('');
  const [executing, setExecuting] = useState(false);
  const [errorMsg, setErrorMsg] = useState<string | null>(null);
  const [warningMsg, setWarningMsg] = useState<string | null>(null);
  const [presetsVisible, setPresetsVisible] = useState(false);
  const [tableVisible, setTableVisible] = useState(true);

  /* Natural-language mode state */
  const [mode, setMode] = useState<'nl' | 'cypher'>('nl');
  const [generatedCypher, setGeneratedCypher] = useState<string | null>(null);
  const [cypherVisible, setCypherVisible] = useState(false);
  const [translating, setTranslating] = useState(false);
  const [llmAvailable, setLlmAvailable] = useState(true);

  const inputRef = useRef<HTMLTextAreaElement>(null);
  const wrapperRef = useRef<HTMLDivElement>(null);

  /* ── Refs for stable callback access ──
     Mirror the latest state so executeQuery never reads stale closures. */
  const inputTextRef = useRef(inputText);
  inputTextRef.current = inputText;

  const modeRef = useRef(mode);
  modeRef.current = mode;

  const graphRef = useRef(graph);
  graphRef.current = graph;

  const generatedCypherRef = useRef(generatedCypher);
  generatedCypherRef.current = generatedCypher;

  /* Re-check LLM availability each time the panel opens */
  useEffect(() => {
    const config = getActiveProviderConfig();
    const available = config !== null;
    setLlmAvailable(available);
    if (!available) {
      setMode('cypher');
    }
  }, [panelOpen]);

  useEffect(() => {
    if (panelOpen && inputRef.current) inputRef.current.focus();
  }, [panelOpen]);

  useEffect(() => {
    const handler = (e: MouseEvent) => {
      if (wrapperRef.current && !wrapperRef.current.contains(e.target as Node)) {
        setPresetsVisible(false);
      }
    };
    document.addEventListener('mousedown', handler);
    return () => document.removeEventListener('mousedown', handler);
  }, []);

  useEffect(() => {
    const onEsc = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && panelOpen) {
        setPanelOpen(false);
        setPresetsVisible(false);
      }
    };
    document.addEventListener('keydown', onEsc);
    return () => document.removeEventListener('keydown', onEsc);
  }, [panelOpen]);

  /* ── Execute a Cypher string against the graph DB ──
     Shared by both executeQuery and pickPreset — takes no closure state. */
  const runCypher = useCallback(async (cypher: string) => {
    const t0 = performance.now();
    const rows = await runQuery(cypher);
    const elapsed = performance.now() - t0;
    const nodeIds = extractNodeIds(rows);
    /* flushSync needed under Electron — automatic batching swallows updates otherwise */
    flushSync(() => {
      setQueryResult({ rows, nodeIds, executionTime: elapsed });
      setHighlightedNodeIds(new Set(nodeIds));
    });
  }, [runQuery, setQueryResult, setHighlightedNodeIds]);

  /* ── Primary execute handler — reads from refs to avoid stale closures ── */
  const executeQuery = useCallback(async () => {
    const trimmed = inputTextRef.current.trim();
    const currentMode = modeRef.current;
    const currentGraph = graphRef.current;

    if (!trimmed) return;

    setExecuting(true);
    setErrorMsg(null);
    setWarningMsg(null);

    try {
      if (!currentGraph) {
        setErrorMsg('No project loaded. Load a project first.');
        return;
      }

      const dbReady = await isDatabaseReady();
      if (!dbReady) {
        setErrorMsg('Database not ready. Please wait for loading to complete.');
        return;
      }

      if (currentMode === 'cypher') {
        await runCypher(trimmed);
        return;
      }

      /* ── Natural-language path: translate to Cypher, then run ── */
      const config = getActiveProviderConfig();
      if (!config) {
        setErrorMsg('No AI model configured. Switch to Cypher mode or configure a model in Settings.');
        return;
      }

      setTranslating(true);
      setGeneratedCypher(null);

      const result = await translateNLToCypher(trimmed, config);
      setGeneratedCypher(result.cypher);
      setCypherVisible(true);
      setTranslating(false);

      if (result.cannotTranslate) {
        setWarningMsg("Can't translate this question. Try rephrasing or switch to Cypher.");
        return;
      }

      if (!CYPHER_KEYWORDS.test(result.cypher)) {
        setWarningMsg('The AI returned an unexpected response. Check the generated output below.');
        return;
      }

      await runCypher(result.cypher);
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'Query execution failed';
      setErrorMsg(msg);
      setQueryResult(null);
      setHighlightedNodeIds(new Set());
      if (modeRef.current === 'nl') setCypherVisible(true);
    } finally {
      flushSync(() => {
        setExecuting(false);
        setTranslating(false);
      });
    }
  }, [isDatabaseReady, runCypher, setQueryResult, setHighlightedNodeIds]);

  const onTextareaKey = useCallback((e: React.KeyboardEvent) => {
    if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
      e.preventDefault();
      executeQuery();
    }
  }, [executeQuery]);

  const pickPreset = useCallback((preset: typeof PRESET_QUERIES[number]) => {
    setPresetsVisible(false);
    setErrorMsg(null);
    setWarningMsg(null);

    const currentMode = modeRef.current;

    if (currentMode === 'nl') {
      /* NL path: display the preset title while executing its Cypher directly */
      setInputText(preset.title);
      setGeneratedCypher(preset.cypher);
      setCypherVisible(true);

      /* Fire the preset Cypher immediately */
      (async () => {
        const currentGraph = graphRef.current;
        if (!currentGraph) {
          setErrorMsg('No project loaded. Load a project first.');
          return;
        }

        setExecuting(true);
        try {
          const dbReady = await isDatabaseReady();
          if (!dbReady) {
            setErrorMsg('Database not ready. Please wait for loading to complete.');
            return;
          }
          await runCypher(preset.cypher);
        } catch (err) {
          setErrorMsg(err instanceof Error ? err.message : 'Query execution failed');
          setQueryResult(null);
          setHighlightedNodeIds(new Set());
        } finally {
          flushSync(() => { setExecuting(false); });
        }
      })();
    } else {
      /* Cypher path: paste the raw query into the textarea */
      setInputText(preset.cypher);
      inputRef.current?.focus();
    }
  }, [isDatabaseReady, runCypher, setQueryResult, setHighlightedNodeIds]);

  const switchMode = useCallback((newMode: 'nl' | 'cypher') => {
    const currentMode = modeRef.current;
    if (newMode === currentMode) return;

    /* Pre-fill the textarea with the generated Cypher when switching away from NL mode */
    if (newMode === 'cypher' && generatedCypherRef.current) {
      setInputText(generatedCypherRef.current);
    } else if (newMode === 'nl' && CYPHER_KEYWORDS.test(inputTextRef.current.trim())) {
      setInputText('');
    }

    setMode(newMode);
    setErrorMsg(null);
    setWarningMsg(null);
    inputRef.current?.focus();
  }, []);

  const closePanel = () => {
    setPanelOpen(false);
    setPresetsVisible(false);
    clearQueryHighlights();
    setErrorMsg(null);
    setWarningMsg(null);
  };

  const resetInput = () => {
    setInputText('');
    setGeneratedCypher(null);
    setCypherVisible(false);
    clearQueryHighlights();
    setErrorMsg(null);
    setWarningMsg(null);
    inputRef.current?.focus();
  };

  if (!panelOpen) {
    return (
      <button
        onClick={() => setPanelOpen(true)}
        className="group absolute top-4 left-4 z-20 flex items-center gap-2 px-3 py-2 glass-elevated rounded-md text-text-primary text-[12px] hover:bg-white/[0.14] transition-all duration-200"
      >
        <Database className="w-3.5 h-3.5" />
        <span>Console</span>
        {queryResult && queryResult.nodeIds.length > 0 && (
          <span className="px-1.5 py-0.5 ml-1 bg-white/20 rounded-md text-xs font-semibold">
            {queryResult.nodeIds.length}
          </span>
        )}
      </button>
    );
  }

  const hasResults = queryResult && !errorMsg && !warningMsg;
  const highlightCount = queryResult?.nodeIds.length ?? 0;
  const isBusy = translating || executing;

  // Button label
  let buttonLabel = 'Run';
  if (translating) buttonLabel = 'Translating...';
  else if (executing) buttonLabel = 'Running...';

  return (
    <div
      ref={wrapperRef}
      className="absolute top-4 left-4 z-20 w-[480px] max-w-[calc(100%-2rem)] bg-deep/95 backdrop-blur-md glass-elevated rounded-lg animate-fade-in"
    >
      <div className="flex items-center justify-between px-4 py-3 border-b border-border-subtle">
        <div className="flex items-center gap-2">
          <div className="w-7 h-7 flex items-center justify-center bg-white/[0.08] border border-white/[0.12] rounded-md">
            <Database className="w-4 h-4 text-text-secondary" />
          </div>
          <span className="font-medium text-sm">Graph Console</span>
        </div>
        <button
          onClick={closePanel}
          className="p-1.5 text-text-muted hover:text-text-primary hover:bg-hover rounded-md transition-colors"
        >
          <X className="w-4 h-4" />
        </button>
      </div>

      {/* Ask / Cypher mode switcher */}
      <div className="px-3 pt-3 pb-1">
        <div className="inline-flex rounded-md bg-surface border border-border-subtle p-0.5">
          <button
            onClick={() => switchMode('nl')}
            disabled={!llmAvailable}
            className={`flex items-center gap-1.5 px-3 py-1 rounded text-xs font-medium transition-all ${
              mode === 'nl'
                ? 'bg-violet-500/20 text-violet-300 shadow-sm'
                : 'text-text-muted hover:text-text-secondary'
            } ${!llmAvailable ? 'opacity-40 cursor-not-allowed' : ''}`}
          >
            <MessageSquare className="w-3 h-3" />
            Ask
          </button>
          <button
            onClick={() => switchMode('cypher')}
            className={`flex items-center gap-1.5 px-3 py-1 rounded text-xs font-medium transition-all ${
              mode === 'cypher'
                ? 'bg-violet-500/20 text-violet-300 shadow-sm'
                : 'text-text-muted hover:text-text-secondary'
            }`}
          >
            <Code className="w-3 h-3" />
            Cypher
          </button>
        </div>
      </div>

      {/* Missing-model warning */}
      {!llmAvailable && (
        <div className="mx-3 mt-2 px-3 py-2 bg-amber-500/10 border border-amber-500/20 rounded-md flex items-center gap-2">
          <AlertTriangle className="w-3.5 h-3.5 text-amber-400 shrink-0" />
          <p className="text-xs text-amber-300">Configure an AI model in Settings to enable natural language queries</p>
        </div>
      )}

      <div className="p-3 pt-2">
        <div className="relative">
          <textarea
            ref={inputRef}
            value={inputText}
            onChange={e => setInputText(e.target.value)}
            onKeyDown={onTextareaKey}
            placeholder={
              mode === 'nl'
                ? "Ask about your code graph...\ne.g. \"Which files import utils?\" or \"Show me the largest classes\""
                : "MATCH (n:Function)\nRETURN n.name, n.filePath\nLIMIT 10"
            }
            rows={3}
            className={`w-full px-3 py-2.5 bg-surface border border-border-subtle rounded-lg text-sm text-text-primary placeholder:text-text-muted/60 focus:border-violet-500/50 focus:ring-2 focus:ring-violet-500/20 outline-none resize-none transition-all ${
              mode === 'cypher' ? 'font-mono' : ''
            }`}
          />
        </div>

        {/* Collapsible generated-Cypher preview */}
        {generatedCypher && mode === 'nl' && (
          <div className="mt-2 border border-border-subtle rounded-lg overflow-hidden">
            <button
              onClick={() => setCypherVisible(v => !v)}
              className="w-full flex items-center justify-between px-3 py-1.5 text-xs text-text-muted hover:text-text-secondary transition-colors bg-surface/50"
            >
              <span className="font-medium">Generated Cypher</span>
              {cypherVisible ? <ChevronUp className="w-3 h-3" /> : <ChevronDown className="w-3 h-3" />}
            </button>
            {cypherVisible && (
              <pre className="px-3 py-2 text-xs font-mono text-text-secondary bg-surface/30 overflow-x-auto whitespace-pre-wrap break-all">
                {generatedCypher}
              </pre>
            )}
          </div>
        )}

        <div className="flex items-center justify-between mt-3">
          <div className="relative">
            <button
              onClick={() => setPresetsVisible(v => !v)}
              className="flex items-center gap-1.5 px-3 py-1.5 text-xs text-text-secondary hover:text-text-primary hover:bg-hover rounded-md transition-colors"
            >
              <Code className="w-3.5 h-3.5" />
              <span>Examples</span>
              <ChevronDown className={`w-3.5 h-3.5 transition-transform ${presetsVisible ? 'rotate-180' : ''}`} />
            </button>

            {presetsVisible && (
              <div className="absolute top-full left-0 mt-1 w-72 py-1 bg-surface border border-border-subtle rounded-lg shadow-xl z-50 animate-fade-in">
                {PRESET_QUERIES.map(p => (
                  <button
                    key={p.title}
                    onClick={() => pickPreset(p)}
                    className="w-full px-3 py-2 text-left text-sm text-text-secondary hover:bg-hover hover:text-text-primary transition-colors"
                  >
                    {p.title}
                  </button>
                ))}
              </div>
            )}
          </div>

          <div className="flex items-center gap-2">
            {inputText && (
              <button
                onClick={resetInput}
                className="px-3 py-1.5 text-xs text-text-secondary hover:text-text-primary hover:bg-hover rounded-md transition-colors"
              >
                Clear
              </button>
            )}
            <button
              onClick={executeQuery}
              disabled={!inputText.trim() || isBusy}
              className="flex items-center gap-1.5 px-4 py-1.5 bg-accent rounded-md text-white text-[13px] hover:bg-accent-dim disabled:opacity-40 disabled:cursor-not-allowed transition-all"
            >
              {isBusy ? <Loader2 className="w-3.5 h-3.5 animate-spin" /> : <Play className="w-3.5 h-3.5" />}
              <span>{buttonLabel}</span>
              <kbd className="ml-1 px-1 py-0.5 bg-white/20 rounded text-[10px]">&#8984;&#9166;</kbd>
            </button>
          </div>
        </div>
      </div>

      {/* Amber warning — e.g. untranslatable question or unexpected response */}
      {warningMsg && (
        <div className="px-4 py-2 bg-amber-500/10 border-t border-amber-500/20 flex items-center gap-2">
          <AlertTriangle className="w-3.5 h-3.5 text-amber-400 shrink-0" />
          <p className="text-xs text-amber-300">{warningMsg}</p>
        </div>
      )}

      {/* Red error — API or query execution failures */}
      {errorMsg && (
        <div className="px-4 py-2 bg-red-500/10 border-t border-red-500/20">
          <p className="text-xs text-red-400 font-mono">{errorMsg}</p>
        </div>
      )}

      {hasResults && (
        <div className="border-t border-violet-500/20">
          <div className="px-4 py-2.5 bg-violet-500/5 flex items-center justify-between">
            <div className="flex items-center gap-3 text-xs">
              <span className="text-text-secondary">
                <span className="text-violet-400 font-semibold">{queryResult.rows.length}</span> rows
              </span>
              {highlightCount > 0 && (
                <span className="text-text-secondary">
                  <span className="text-violet-400 font-semibold">{highlightCount}</span> highlighted
                </span>
              )}
              <span className="text-text-muted">{queryResult.executionTime.toFixed(1)}ms</span>
            </div>
            <div className="flex items-center gap-2">
              {highlightCount > 0 && (
                <button
                  onClick={clearQueryHighlights}
                  className="text-xs text-text-muted hover:text-text-primary transition-colors"
                >
                  Clear
                </button>
              )}
              <button
                onClick={() => setTableVisible(v => !v)}
                className="flex items-center gap-1 text-xs text-text-muted hover:text-text-primary transition-colors"
              >
                <Table className="w-3 h-3" />
                {tableVisible ? <ChevronDown className="w-3 h-3" /> : <ChevronUp className="w-3 h-3" />}
              </button>
            </div>
          </div>

          {tableVisible && queryResult.rows.length > 0 && (
            <ResultsTable rows={queryResult.rows} maxDisplay={50} />
          )}
        </div>
      )}
    </div>
  );
};
