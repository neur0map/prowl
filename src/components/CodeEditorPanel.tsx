import { useState, useCallback, useEffect, useRef, useMemo } from 'react'
import { X, PanelLeftClose, FileCode, Check, AlertCircle } from 'lucide-react'
import Editor, { type Monaco } from '@monaco-editor/react'
import type { editor } from 'monaco-editor'
import { useAppState } from '../hooks/useAppState'

// Map file extensions to Monaco language IDs
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

  // Open a file in a tab (or focus existing tab)
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

  // When selectedNode changes, open its file
  useEffect(() => {
    if (!selectedNode?.properties?.filePath) return
    const fp = selectedNode.properties.filePath
    const line = selectedNode.properties.startLine
    openFile(fp, typeof line === 'number' ? line + 1 : undefined)
  }, [selectedNode, openFile])

  // When a grounding link is clicked, open the referenced file in the editor
  useEffect(() => {
    if (!codeReferenceFocus) return
    const { filePath, startLine } = codeReferenceFocus
    // startLine is 0-indexed from addCodeReference, Monaco is 1-indexed
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

  // Autosave: debounced 1.5s after edit, only for local folders
  const handleEditorChange = useCallback((value: string | undefined) => {
    if (!value || !activeFilePath) return

    // Update the tab content
    setTabs(prev => prev.map(t =>
      t.filePath === activeFilePath ? { ...t, content: value } : t
    ))

    // Update in-memory fileContents
    setFileContents(prev => {
      const next = new Map(prev)
      next.set(activeFilePath, value)
      return next
    })

    // Autosave to disk for local folders
    if (!isLocalFolder || !workspacePath) return

    if (saveTimerRef.current) clearTimeout(saveTimerRef.current)
    saveTimerRef.current = setTimeout(async () => {
      try {
        setSaveState({ status: 'saving' })
        const fullPath = `${workspacePath}/${activeFilePath}`
        await (window as any).prowl.fs.writeFile(fullPath, value)
        setSaveState({ status: 'saved' })
        // Update original content after save
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

  // Force save on Cmd+S
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

  // Monaco mount handler
  const handleEditorMount = useCallback((editor: editor.IStandaloneCodeEditor, monaco: Monaco) => {
    editorRef.current = editor
    monacoRef.current = monaco

    // Define Prowl theme
    monaco.editor.defineTheme('prowl-dark', {
      base: 'vs-dark',
      inherit: true,
      rules: [],
      colors: {
        'editor.background': '#2c2c2e',
        'editor.foreground': '#f5f5f7',
        'editor.lineHighlightBackground': '#38383a',
        'editor.selectionBackground': 'rgba(10, 132, 255, 0.25)',
        'editorCursor.foreground': '#0A84FF',
        'editorLineNumber.foreground': 'rgba(255,255,255,0.35)',
        'editorLineNumber.activeForeground': 'rgba(255,255,255,0.55)',
        'editor.inactiveSelectionBackground': 'rgba(10, 132, 255, 0.12)',
        'editorWidget.background': '#2c2c2e',
        'editorWidget.border': 'rgba(255,255,255,0.08)',
        'scrollbar.shadow': '#00000000',
        'scrollbarSlider.background': 'rgba(255,255,255,0.08)',
        'scrollbarSlider.hoverBackground': 'rgba(255,255,255,0.15)',
        'scrollbarSlider.activeBackground': 'rgba(255,255,255,0.20)',
      },
    })
    monaco.editor.setTheme('prowl-dark')

    // Cmd+S override
    editor.addCommand(monaco.KeyMod.CtrlCmd | monaco.KeyCode.KeyS, () => {
      forceSave()
    })
  }, [forceSave])

  // Scroll to line when tab opens with a scrollLine
  useEffect(() => {
    if (!editorRef.current || !activeTab?.scrollLine) return
    const timer = setTimeout(() => {
      editorRef.current?.revealLineInCenter(activeTab.scrollLine!)
      editorRef.current?.setPosition({ lineNumber: activeTab.scrollLine!, column: 1 })
    }, 50)
    return () => clearTimeout(timer)
  }, [activeTab?.filePath, activeTab?.scrollLine])

  // Keyboard shortcuts
  useEffect(() => {
    const handleKeyDown = (e: KeyboardEvent) => {
      // Cmd+W — close active tab
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

  // Resize handle
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

  // Cleanup timers
  useEffect(() => {
    return () => {
      if (saveTimerRef.current) clearTimeout(saveTimerRef.current)
      if (savedFadeRef.current) clearTimeout(savedFadeRef.current)
    }
  }, [])

  const isDirty = activeTab ? activeTab.content !== activeTab.originalContent : false

  return (
    <aside
      className="h-full glass border-r border-white/[0.08] flex flex-col relative"
      style={{ width: panelWidth }}
    >
      {/* Resize handle */}
      <div
        onMouseDown={startResize}
        className="absolute top-0 right-0 h-full w-2 cursor-col-resize bg-transparent hover:bg-accent/25 transition-colors z-10"
      />

      {/* Tab bar */}
      <div className="flex items-center gap-0.5 px-2 py-1.5 border-b border-white/[0.08] shrink-0">
        <div className="flex items-center gap-0.5 flex-1 min-w-0 overflow-x-auto scrollbar-thin">
          {tabs.map(tab => (
            <button
              key={tab.filePath}
              onClick={() => setActiveFilePath(tab.filePath)}
              onMouseDown={(e) => { if (e.button === 1) { e.preventDefault(); closeTab(tab.filePath) } }}
              className={`
                group flex items-center gap-1.5 px-2.5 py-1 rounded-md text-xs
                transition-all duration-150 shrink-0 max-w-[160px]
                ${tab.filePath === activeFilePath
                  ? 'glass-elevated text-text-primary font-medium'
                  : 'text-text-secondary hover:text-text-primary hover:bg-white/[0.04]'
                }
              `}
            >
              <FileCode size={11} className="shrink-0 opacity-50" />
              <span className="truncate">{tab.name}</span>
              {tab.content !== tab.originalContent && (
                <span className="w-1.5 h-1.5 rounded-full bg-accent shrink-0" title="Unsaved changes" />
              )}
              <span
                onClick={(e) => { e.stopPropagation(); closeTab(tab.filePath) }}
                className="ml-auto opacity-0 group-hover:opacity-60 hover:!opacity-100 transition-opacity cursor-pointer shrink-0"
              >
                <X size={11} />
              </span>
            </button>
          ))}
        </div>

        {/* Close panel button */}
        <button
          onClick={() => { setSelectedNode(null); setCodePanelOpen(false); }}
          className="p-1 rounded hover:bg-white/[0.06] text-text-muted hover:text-text-secondary transition-colors shrink-0 ml-1"
          title="Close editor (Esc)"
        >
          <PanelLeftClose size={14} />
        </button>
      </div>

      {/* Editor area */}
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
              fontFamily: "'JetBrains Mono', 'Fira Code', 'Cascadia Code', monospace",
              fontSize: 13,
              lineHeight: 1.6,
              minimap: { enabled: false },
              wordWrap: 'on',
              lineNumbers: 'on',
              scrollBeyondLastLine: false,
              renderLineHighlight: 'line',
              overviewRulerBorder: false,
              hideCursorInOverviewRuler: true,
              scrollbar: {
                verticalScrollbarSize: 6,
                horizontalScrollbarSize: 6,
              },
              padding: { top: 8, bottom: 8 },
              readOnly: !isLocalFolder,
            }}
          />
        ) : (
          <div className="flex items-center justify-center h-full text-text-muted text-sm">
            Click a node in the graph to view its code
          </div>
        )}
      </div>

      {/* Status bar */}
      <div className="flex items-center justify-between px-3 py-1 border-t border-white/[0.08] text-[11px] text-text-muted shrink-0">
        <div className="flex items-center gap-2">
          {activeTab && (
            <>
              <span className="font-mono">{activeTab.language}</span>
              {!isLocalFolder && (
                <span className="text-text-muted/60">read-only</span>
              )}
            </>
          )}
        </div>
        <div className="flex items-center gap-1.5">
          {saveState.status === 'saving' && (
            <span className="text-text-muted animate-pulse">Saving...</span>
          )}
          {saveState.status === 'saved' && (
            <span className="flex items-center gap-1 text-[#30D158]">
              <Check size={10} /> Saved
            </span>
          )}
          {saveState.status === 'error' && (
            <span className="flex items-center gap-1 text-[#FF453A]">
              <AlertCircle size={10} /> {saveState.message || 'Save failed'}
            </span>
          )}
        </div>
      </div>
    </aside>
  )
}
