import { useState, useCallback, useEffect, useRef, useMemo } from 'react'
import { X, PanelLeftClose, Check, AlertCircle } from 'lucide-react'
import Editor, { type Monaco } from '@monaco-editor/react'
import type { editor } from 'monaco-editor'
import { useAppState } from '../hooks/useAppState'
import { LanguageIcon } from './LanguageIcon'

function getLanguage(filePath: string): string {
  const name = filePath.split('/').pop()?.toLowerCase() || ''
  const ext = name.split('.').pop() || ''

  // Special filenames first
  if (name === 'dockerfile' || name.startsWith('dockerfile.')) return 'dockerfile'
  if (name === 'makefile' || name === 'gnumakefile') return 'makefile'
  if (name === 'gemfile' || name === 'rakefile' || name === 'guardfile') return 'ruby'
  if (name === 'vagrantfile') return 'ruby'
  if (name === 'cmakelists.txt' || ext === 'cmake') return 'cmake'
  if (name === 'cargo.toml' || name === 'cargo.lock') return 'toml'
  if (name === 'go.mod' || name === 'go.sum') return 'go'
  if (name === 'justfile') return 'makefile'
  if (name.endsWith('.d.ts')) return 'typescript'
  if (name.startsWith('.env')) return 'ini'

  const map: Record<string, string> = {
    // Web
    ts: 'typescript', tsx: 'typescript', mts: 'typescript', cts: 'typescript',
    js: 'javascript', jsx: 'javascript', mjs: 'javascript', cjs: 'javascript',
    html: 'html', htm: 'html', xhtml: 'html',
    css: 'css',
    scss: 'scss', sass: 'scss',
    less: 'less',
    vue: 'html',
    svelte: 'html',
    astro: 'html',

    // Systems
    rs: 'rust',
    go: 'go',
    c: 'c', h: 'c',
    cpp: 'cpp', cc: 'cpp', cxx: 'cpp', hpp: 'cpp', hxx: 'cpp', hh: 'cpp',
    zig: 'c',
    nim: 'nim',

    // JVM
    java: 'java',
    kt: 'kotlin', kts: 'kotlin',
    scala: 'scala', sc: 'scala',
    groovy: 'groovy', gradle: 'groovy',
    clj: 'clojure', cljs: 'clojure', cljc: 'clojure', edn: 'clojure',

    // .NET
    cs: 'csharp',
    fs: 'fsharp', fsx: 'fsharp',
    vb: 'vb',

    // Scripting
    py: 'python', pyw: 'python', pyi: 'python',
    rb: 'ruby', erb: 'ruby',
    php: 'php',
    pl: 'perl', pm: 'perl',
    lua: 'lua',
    r: 'r',
    jl: 'julia',
    ex: 'elixir', exs: 'elixir',
    erl: 'erlang', hrl: 'erlang',

    // Apple
    swift: 'swift',
    m: 'objective-c', mm: 'objective-c',

    // Shell
    sh: 'shell', bash: 'shell', zsh: 'shell', fish: 'shell',
    ps1: 'powershell', psm1: 'powershell', psd1: 'powershell',
    bat: 'bat', cmd: 'bat',

    // Data / Config
    json: 'json', jsonc: 'json', json5: 'json',
    yaml: 'yaml', yml: 'yaml',
    toml: 'toml',
    xml: 'xml', xsl: 'xml', xslt: 'xml', plist: 'xml', svg: 'xml',
    ini: 'ini', cfg: 'ini', conf: 'ini', properties: 'ini',
    env: 'ini',

    // Markup / Docs
    md: 'markdown', mdx: 'markdown',
    rst: 'restructuredtext',
    tex: 'latex', latex: 'latex',
    adoc: 'plaintext',

    // Query
    sql: 'sql',
    graphql: 'graphql', gql: 'graphql',
    prisma: 'graphql',

    // DevOps / IaC
    tf: 'hcl', hcl: 'hcl', tfvars: 'hcl',

    // Functional
    hs: 'haskell', lhs: 'haskell',
    ml: 'fsharp', mli: 'fsharp',
    rkt: 'scheme',
    scm: 'scheme',
    lisp: 'scheme', el: 'scheme',

    // Other
    dart: 'dart',
    v: 'plaintext',
    sol: 'sol',
    proto: 'protobuf',
    thrift: 'plaintext',
    diff: 'diff', patch: 'diff',
    log: 'plaintext',
    txt: 'plaintext',
    csv: 'plaintext',
    lock: 'json',
  }

  return map[ext] || 'plaintext'
}

interface EditorTab {
  filePath: string
  name: string
  language: string
  content: string
  originalContent: string // for dirty detection
  scrollLine?: number     // line to scroll to when opening
}

interface SaveState {
  status: 'idle' | 'saving' | 'saved' | 'error'
  message?: string
}

export interface CodeEditorPanelProps {
  onFocusNode: (nodeId: string) => void
}

export const CodeEditorPanel = ({ onFocusNode }: CodeEditorPanelProps) => {
  const {
    fileContents,
    setFileContents,
    selectedNode,
    setSelectedNode,
    codeReferences,
    codeReferenceFocus,
    setCodePanelOpen,
    agentWatcherState,
  } = useAppState()

  const [tabs, setTabs] = useState<EditorTab[]>([])
  const [activeFilePath, setActiveFilePath] = useState<string | null>(null)
  const [saveState, setSaveState] = useState<SaveState>({ status: 'idle' })
  const [panelWidth, setPanelWidth] = useState<number>(() => {
    try {
      const saved = localStorage.getItem('prowl.editorPanelWidth')
      const parsed = saved ? parseInt(saved, 10) : NaN
      return Number.isFinite(parsed) ? Math.max(350, Math.min(parsed, 900)) : 560
    } catch { return 560 }
  })

  const editorRef = useRef<editor.IStandaloneCodeEditor | null>(null)
  const monacoRef = useRef<Monaco | null>(null)
  const saveTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const savedFadeRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const resizeRef = useRef<{ startX: number; startWidth: number } | null>(null)

  const isLocalFolder = !!agentWatcherState.workspacePath
  const workspacePath = agentWatcherState.workspacePath

  const openFile = useCallback((filePath: string, scrollLine?: number) => {
    const content = fileContents.get(filePath)
    if (content === undefined) return

    setTabs(prev => {
      const existing = prev.find(t => t.filePath === filePath)
      if (existing) {
        // Update scroll position if specified
        if (scrollLine !== undefined) {
          return prev.map(t => t.filePath === filePath ? { ...t, scrollLine } : t)
        }
        return prev
      }
      const name = filePath.split('/').pop() || filePath
      const language = getLanguage(filePath)
      return [...prev, { filePath, name, language, content, originalContent: content, scrollLine }]
    })
    setActiveFilePath(filePath)
  }, [fileContents])

  useEffect(() => {
    if (!selectedNode?.properties?.filePath) return
    const fp = selectedNode.properties.filePath
    const line = selectedNode.properties.startLine
    openFile(fp, typeof line === 'number' ? line + 1 : undefined)
  }, [selectedNode, openFile])

  useEffect(() => {
    if (!codeReferenceFocus) return
    const { filePath, startLine } = codeReferenceFocus
    openFile(filePath, typeof startLine === 'number' ? startLine + 1 : undefined)
  }, [codeReferenceFocus?.ts, openFile])

  const closeTab = useCallback((filePath: string) => {
    setTabs(prev => {
      const remaining = prev.filter(t => t.filePath !== filePath)
      if (activeFilePath === filePath && remaining.length > 0) {
        setActiveFilePath(remaining[remaining.length - 1].filePath)
      } else if (remaining.length === 0) {
        setActiveFilePath(null)
        setSelectedNode(null)
      }
      return remaining
    })
  }, [activeFilePath, setSelectedNode])

  const activeTab = useMemo(() => tabs.find(t => t.filePath === activeFilePath), [tabs, activeFilePath])

  // Debounced autosave for local folders (1.5 s)
  const handleEditorChange = useCallback((value: string | undefined) => {
    if (!value || !activeFilePath) return

    setTabs(prev => prev.map(t =>
      t.filePath === activeFilePath ? { ...t, content: value } : t
    ))

    setFileContents(prev => {
      const next = new Map(prev)
      next.set(activeFilePath, value)
      return next
    })

    if (!isLocalFolder || !workspacePath) return

    if (saveTimerRef.current) clearTimeout(saveTimerRef.current)
    saveTimerRef.current = setTimeout(async () => {
      try {
        setSaveState({ status: 'saving' })
        const fullPath = `${workspacePath}/${activeFilePath}`
        await (window as any).prowl.fs.writeFile(fullPath, value)
        setSaveState({ status: 'saved' })
        setTabs(prev => prev.map(t =>
          t.filePath === activeFilePath ? { ...t, originalContent: value } : t
        ))
        if (savedFadeRef.current) clearTimeout(savedFadeRef.current)
        savedFadeRef.current = setTimeout(() => setSaveState({ status: 'idle' }), 2000)
      } catch (err) {
        setSaveState({ status: 'error', message: err instanceof Error ? err.message : 'Save failed' })
      }
    }, 1500)
  }, [activeFilePath, isLocalFolder, workspacePath, setFileContents])

  const forceSave = useCallback(async () => {
    if (!isLocalFolder || !workspacePath || !activeTab) return
    if (saveTimerRef.current) clearTimeout(saveTimerRef.current)

    try {
      setSaveState({ status: 'saving' })
      const fullPath = `${workspacePath}/${activeTab.filePath}`
      await (window as any).prowl.fs.writeFile(fullPath, activeTab.content)
      setSaveState({ status: 'saved' })
      setTabs(prev => prev.map(t =>
        t.filePath === activeTab.filePath ? { ...t, originalContent: activeTab.content } : t
      ))
      if (savedFadeRef.current) clearTimeout(savedFadeRef.current)
      savedFadeRef.current = setTimeout(() => setSaveState({ status: 'idle' }), 2000)
    } catch (err) {
      setSaveState({ status: 'error', message: err instanceof Error ? err.message : 'Save failed' })
    }
  }, [isLocalFolder, workspacePath, activeTab])

  const handleEditorMount = useCallback((editor: editor.IStandaloneCodeEditor, monaco: Monaco) => {
    editorRef.current = editor
    monacoRef.current = monaco

    monaco.editor.defineTheme('prowl-dark', {
      base: 'vs-dark',
      inherit: true,
      rules: [
        { token: 'comment', foreground: '6B7280', fontStyle: 'italic' },
        { token: 'keyword', foreground: 'C084FC' },
        { token: 'string', foreground: '86EFAC' },
        { token: 'number', foreground: 'FCD34D' },
        { token: 'type', foreground: '67E8F9' },
        { token: 'function', foreground: '93C5FD' },
        { token: 'variable', foreground: 'E2E8F0' },
        { token: 'operator', foreground: 'A5B4FC' },
      ],
      colors: {
        'editor.background': '#141416',
        'editor.foreground': '#E2E8F0',
        'editor.lineHighlightBackground': '#1E1E22',
        'editor.lineHighlightBorder': '#ffffff06',
        'editor.selectionBackground': '#7C3AED30',
        'editor.inactiveSelectionBackground': '#7C3AED18',
        'editorCursor.foreground': '#A78BFA',
        'editorLineNumber.foreground': '#3F3F46',
        'editorLineNumber.activeForeground': '#71717A',
        'editorIndentGuide.background': '#ffffff08',
        'editorIndentGuide.activeBackground': '#ffffff14',
        'editorWidget.background': '#1C1C1E',
        'editorWidget.border': '#ffffff10',
        'editorBracketMatch.background': '#7C3AED20',
        'editorBracketMatch.border': '#7C3AED40',
        'scrollbar.shadow': '#00000000',
        'scrollbarSlider.background': '#ffffff08',
        'scrollbarSlider.hoverBackground': '#ffffff12',
        'scrollbarSlider.activeBackground': '#ffffff18',
        'editorOverviewRuler.border': '#00000000',
        'editor.rangeHighlightBackground': '#7C3AED10',
      },
    })
    monaco.editor.setTheme('prowl-dark')

    editor.addCommand(monaco.KeyMod.CtrlCmd | monaco.KeyCode.KeyS, () => {
      forceSave()
    })
  }, [forceSave])

  useEffect(() => {
    if (!editorRef.current || !activeTab?.scrollLine) return
    const timer = setTimeout(() => {
      editorRef.current?.revealLineInCenter(activeTab.scrollLine!)
      editorRef.current?.setPosition({ lineNumber: activeTab.scrollLine!, column: 1 })
    }, 50)
    return () => clearTimeout(timer)
  }, [activeTab?.filePath, activeTab?.scrollLine])

  useEffect(() => {
    const handleKeyDown = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === 'w') {
        if (activeFilePath && tabs.length > 0) {
          e.preventDefault()
          closeTab(activeFilePath)
        }
      }
    }
    window.addEventListener('keydown', handleKeyDown)
    return () => window.removeEventListener('keydown', handleKeyDown)
  }, [activeFilePath, tabs, closeTab])

  const startResize = useCallback((e: React.MouseEvent) => {
    e.preventDefault()
    resizeRef.current = { startX: e.clientX, startWidth: panelWidth }
    document.body.style.cursor = 'col-resize'
    document.body.style.userSelect = 'none'

    const onMove = (ev: MouseEvent) => {
      if (!resizeRef.current) return
      const delta = ev.clientX - resizeRef.current.startX
      setPanelWidth(Math.max(350, Math.min(resizeRef.current.startWidth + delta, 900)))
    }
    const onUp = () => {
      resizeRef.current = null
      document.body.style.cursor = ''
      document.body.style.userSelect = ''
      localStorage.setItem('prowl.editorPanelWidth', String(panelWidth))
      window.removeEventListener('mousemove', onMove)
      window.removeEventListener('mouseup', onUp)
    }
    window.addEventListener('mousemove', onMove)
    window.addEventListener('mouseup', onUp)
  }, [panelWidth])

  useEffect(() => {
    return () => {
      if (saveTimerRef.current) clearTimeout(saveTimerRef.current)
      if (savedFadeRef.current) clearTimeout(savedFadeRef.current)
    }
  }, [])

  const isDirty = activeTab ? activeTab.content !== activeTab.originalContent : false

  return (
    <aside
      className="h-full bg-[#141416] border border-white/[0.08] flex flex-col relative rounded-xl overflow-hidden shadow-lg"
      style={{ width: panelWidth }}
    >
      <div
        onMouseDown={startResize}
        className="absolute top-0 right-0 h-full w-1.5 cursor-col-resize bg-transparent hover:bg-violet-500/30 transition-colors z-10"
      />

      <div className="flex items-center gap-0 px-1 py-0 bg-[#1C1C1E] border-b border-white/[0.06] shrink-0">
        <div className="flex items-center gap-0 flex-1 min-w-0 overflow-x-auto scrollbar-thin">
          {tabs.map(tab => (
            <button
              key={tab.filePath}
              onClick={() => setActiveFilePath(tab.filePath)}
              onMouseDown={(e) => { if (e.button === 1) { e.preventDefault(); closeTab(tab.filePath) } }}
              className={`
                group flex items-center gap-1.5 px-3 py-2 text-[11px]
                transition-all duration-150 shrink-0 max-w-[180px] border-b-2
                ${tab.filePath === activeFilePath
                  ? 'border-violet-400/60 text-text-primary bg-[#141416]'
                  : 'border-transparent text-text-muted hover:text-text-secondary hover:bg-white/[0.03]'
                }
              `}
            >
              <LanguageIcon filename={tab.filePath} size={13} />
              <span className="truncate">{tab.name}</span>
              {tab.content !== tab.originalContent && (
                <span className="w-1.5 h-1.5 rounded-full bg-violet-400 shrink-0" title="Unsaved changes" />
              )}
              <span
                onClick={(e) => { e.stopPropagation(); closeTab(tab.filePath) }}
                className="ml-auto opacity-0 group-hover:opacity-50 hover:!opacity-100 transition-opacity cursor-pointer shrink-0"
              >
                <X size={11} />
              </span>
            </button>
          ))}
        </div>

        <button
          onClick={() => { setSelectedNode(null); setCodePanelOpen(false); }}
          className="p-1.5 rounded-lg hover:bg-white/[0.06] text-text-muted hover:text-text-secondary transition-colors shrink-0 mx-1"
          title="Close editor (Esc)"
        >
          <PanelLeftClose size={13} />
        </button>
      </div>

      {activeTab && (
        <div className="px-4 py-1.5 text-[10px] font-mono text-text-muted/50 bg-[#18181B] border-b border-white/[0.04] truncate shrink-0">
          {activeTab.filePath}
        </div>
      )}

      <div className="flex-1 min-h-0 relative">
        {activeTab ? (
          <Editor
            key={activeTab.filePath}
            language={activeTab.language}
            value={activeTab.content}
            onChange={handleEditorChange}
            onMount={handleEditorMount}
            theme="prowl-dark"
            options={{
              fontFamily: "'JetBrains Mono', 'SF Mono', 'Fira Code', monospace",
              fontSize: 13,
              fontLigatures: true,
              lineHeight: 22,
              letterSpacing: 0.3,
              minimap: { enabled: false },
              wordWrap: 'on',
              lineNumbers: 'on',
              lineDecorationsWidth: 16,
              lineNumbersMinChars: 3,
              scrollBeyondLastLine: false,
              renderLineHighlight: 'line',
              renderLineHighlightOnlyWhenFocus: true,
              overviewRulerBorder: false,
              hideCursorInOverviewRuler: true,
              cursorBlinking: 'smooth',
              cursorSmoothCaretAnimation: 'on',
              cursorWidth: 2,
              smoothScrolling: true,
              scrollbar: {
                verticalScrollbarSize: 8,
                horizontalScrollbarSize: 8,
                useShadows: false,
                verticalHasArrows: false,
                horizontalHasArrows: false,
                arrowSize: 0,
              },
              padding: { top: 16, bottom: 16 },
              bracketPairColorization: { enabled: true },
              guides: {
                indentation: true,
                bracketPairs: 'active',
              },
              readOnly: !isLocalFolder,
            }}
          />
        ) : (
          <div className="flex items-center justify-center h-full text-text-muted text-sm">
            Click a node in the graph to view its code
          </div>
        )}
      </div>

      <div className="flex items-center justify-between px-4 py-1.5 border-t border-white/[0.04] bg-[#18181B] text-[10px] text-text-muted/60 shrink-0">
        <div className="flex items-center gap-3">
          {activeTab && (
            <>
              <span className="font-mono uppercase tracking-wider">{activeTab.language}</span>
              {!isLocalFolder && (
                <span className="px-1.5 py-0.5 rounded-md bg-white/[0.04] text-[9px]">read-only</span>
              )}
            </>
          )}
        </div>
        <div className="flex items-center gap-1.5">
          {saveState.status === 'saving' && (
            <span className="text-text-muted animate-pulse">Saving...</span>
          )}
          {saveState.status === 'saved' && (
            <span className="flex items-center gap-1 text-emerald-400/80">
              <Check size={9} /> Saved
            </span>
          )}
          {saveState.status === 'error' && (
            <span className="flex items-center gap-1 text-red-400/80">
              <AlertCircle size={9} /> {saveState.message || 'Save failed'}
            </span>
          )}
        </div>
      </div>
    </aside>
  )
}
