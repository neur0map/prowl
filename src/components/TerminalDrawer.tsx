import { useState, useRef, useEffect, useCallback } from 'react'
import { Plus, X, Columns2, ChevronDown, ChevronUp, ChevronRight, Search, Terminal as TerminalIcon } from 'lucide-react'
import { Terminal } from '@xterm/xterm'
import { WebglAddon } from '@xterm/addon-webgl'
import { FitAddon } from '@xterm/addon-fit'
import { WebLinksAddon } from '@xterm/addon-web-links'
import { SearchAddon } from '@xterm/addon-search'
import '@xterm/xterm/css/xterm.css'

/* ═══════════════════════════════════════════════════════════════
   THEME — macOS-native dark palette
═══════════════════════════════════════════════════════════════ */
const PROWL_THEME = {
  background: '#1c1c1e',
  foreground: '#f5f5f7',
  cursor: '#f5f5f7',
  cursorAccent: '#1c1c1e',
  selectionBackground: 'rgba(10, 132, 255, 0.3)',
  selectionForeground: undefined,  // inherit — native behavior
  selectionInactiveBackground: 'rgba(10, 132, 255, 0.15)',
  black: '#1c1c1e',
  red: '#FF453A',
  green: '#30D158',
  yellow: '#FFD60A',
  blue: '#0A84FF',
  magenta: '#BF5AF2',
  cyan: '#64D2FF',
  white: '#f5f5f7',
  brightBlack: '#48484a',
  brightRed: '#FF6961',
  brightGreen: '#4CD964',
  brightYellow: '#FFE066',
  brightBlue: '#409CFF',
  brightMagenta: '#DA8FFF',
  brightCyan: '#70D7FF',
  brightWhite: '#ffffff',
}

/* ═══════════════════════════════════════════════════════════════
   TERMINAL FACTORY
═══════════════════════════════════════════════════════════════ */
function makeTerminal(): Terminal {
  return new Terminal({
    theme: PROWL_THEME,
    fontFamily: "'JetBrains Mono', 'SF Mono', 'Fira Code', 'Cascadia Code', monospace",
    fontSize: 13,
    fontWeight: '400',
    fontWeightBold: '600',
    lineHeight: 1.35,
    letterSpacing: 0,
    cursorBlink: true,
    cursorStyle: 'bar',
    cursorWidth: 2,
    allowProposedApi: true,
    scrollback: 10000,
    smoothScrollDuration: 100,
    macOptionIsMeta: true,              // Option key = Meta (for alt+ combos)
    macOptionClickForcesSelection: true, // Option+click = select, not move cursor
    drawBoldTextInBrightColors: false,  // Cleaner bold rendering
    minimumContrastRatio: 1,            // Trust the theme colors
    scrollOnUserInput: true,
  })
}

/* ═══════════════════════════════════════════════════════════════
   CWD HELPERS
═══════════════════════════════════════════════════════════════ */
function abbreviatePath(fullPath: string): string {
  const home = typeof process !== 'undefined' ? '' : ''
  let p = fullPath

  // Strip file:// prefix from OSC 7
  if (p.startsWith('file://')) {
    try { p = new URL(p).pathname } catch { p = p.replace(/^file:\/\/[^/]*/, '') }
  }

  // Replace home dir with ~
  const homeDir = p.includes('/Users/') ? '/Users/' + p.split('/Users/')[1]?.split('/')[0] : ''
  if (homeDir && p.startsWith(homeDir)) {
    p = '~' + p.slice(homeDir.length)
  }

  // Show last 2 segments max
  const parts = p.split('/').filter(Boolean)
  if (parts.length > 2) {
    return parts.slice(-2).join('/')
  }
  return p || '~'
}

/* ═══════════════════════════════════════════════════════════════
   TYPES
═══════════════════════════════════════════════════════════════ */
interface TerminalTab {
  id: string
  name: string
  cwd: string
  terminal: Terminal
  fitAddon: FitAddon
  searchAddon: SearchAddon
  webglLoaded: boolean
  mounted: boolean
  splitId: string | null
  splitTerminal: Terminal | null
  splitFitAddon: FitAddon | null
  splitMounted: boolean
}

interface TerminalDrawerProps {
  isOpen: boolean
  onToggle: () => void
  cwd?: string
  embedded?: boolean
}

/* ═══════════════════════════════════════════════════════════════
   COMPONENT
═══════════════════════════════════════════════════════════════ */
export const TerminalDrawer = ({ isOpen, onToggle, cwd, embedded }: TerminalDrawerProps) => {
  const [tabs, setTabs] = useState<TerminalTab[]>([])
  const [activeTabId, setActiveTabId] = useState<string | null>(null)
  const [drawerHeight, setDrawerHeight] = useState(() => {
    const saved = localStorage.getItem('prowl-terminal-height')
    return saved ? parseInt(saved, 10) : 260
  })
  const [focusedPane, setFocusedPane] = useState<'main' | 'split'>('main')
  const [mountTick, setMountTick] = useState(0)
  const [findOpen, setFindOpen] = useState(false)
  const [findQuery, setFindQuery] = useState('')

  const mainTermRef = useRef<HTMLDivElement>(null)
  const splitTermRef = useRef<HTMLDivElement>(null)
  const findInputRef = useRef<HTMLInputElement>(null)
  const resizeRef = useRef<{ startY: number; startHeight: number } | null>(null)
  const hasInitialized = useRef(false)
  const tabsRef = useRef<TerminalTab[]>([])
  tabsRef.current = tabs

  const prowl = (window as any).prowl

  /* ── Setup OSC handlers on a terminal instance ── */
  const setupOscHandlers = useCallback((terminal: Terminal, termId: string) => {
    // OSC 7: cwd reporting — emitted by modern shells (zsh, bash 5.1+, fish)
    // Format: \x1b]7;file://hostname/path/to/dir\x07
    terminal.parser.registerOscHandler(7, (data: string) => {
      const cwd = abbreviatePath(data)
      setTabs(prev => prev.map(t =>
        t.id === termId ? { ...t, cwd: data, name: cwd } : t
      ))
      return true
    })

    // OSC 2: window title — some shells use this for cwd
    terminal.parser.registerOscHandler(2, (data: string) => {
      if (data.includes('/') || data.includes('~')) {
        const name = abbreviatePath(data)
        setTabs(prev => prev.map(t =>
          t.id === termId ? { ...t, name } : t
        ))
      }
      return true
    })
  }, [])

  /* ── Create tab ── */
  const createTab = useCallback(async () => {
    if (!prowl?.terminal) return

    let id: string
    try {
      id = await prowl.terminal.create(cwd)
    } catch (err) {
      console.error('Failed to create terminal:', err)
      return
    }
    const terminal = makeTerminal()
    const fitAddon = new FitAddon()
    const searchAddon = new SearchAddon()

    terminal.loadAddon(fitAddon)
    terminal.loadAddon(searchAddon)
    terminal.loadAddon(new WebLinksAddon((_event, uri) => {
      window.open(uri, '_blank')
    }))

    // Wire user input → PTY
    terminal.onData((data: string) => {
      prowl.terminal.write(id, data)
    })

    // OSC handlers for cwd tracking
    setupOscHandlers(terminal, id)

    const rawTitle = await prowl.terminal.getTitle(id)
    const name = (rawTitle || 'zsh').split('/').pop() || 'zsh'

    const tab: TerminalTab = {
      id,
      name,
      cwd: cwd || '~',
      terminal,
      fitAddon,
      searchAddon,
      webglLoaded: false,
      mounted: false,
      splitId: null,
      splitTerminal: null,
      splitFitAddon: null,
      splitMounted: false,
    }

    setTabs(prev => [...prev, tab])
    setActiveTabId(id)
    setFocusedPane('main')
    setMountTick(t => t + 1)

    return tab
  }, [prowl, cwd, setupOscHandlers])

  /* ── Close tab ── */
  const closeTab = useCallback(async (tabId: string) => {
    const tab = tabsRef.current.find(t => t.id === tabId)
    if (!tab) return

    tab.terminal.dispose()
    tab.splitTerminal?.dispose()
    prowl?.terminal?.kill(tabId).catch(() => {})
    if (tab.splitId) prowl?.terminal?.kill(tab.splitId).catch(() => {})

    setTabs(prev => {
      const remaining = prev.filter(t => t.id !== tabId)
      if (activeTabId === tabId && remaining.length > 0) {
        setActiveTabId(remaining[remaining.length - 1].id)
      } else if (remaining.length === 0) {
        setActiveTabId(null)
      }
      return remaining
    })
    setMountTick(t => t + 1)
  }, [activeTabId, prowl])

  /* ── Split pane ── */
  const closeSplit = useCallback((tabId: string) => {
    const tab = tabsRef.current.find(t => t.id === tabId)
    if (!tab || !tab.splitId) return

    tab.splitTerminal?.dispose()
    prowl?.terminal?.kill(tab.splitId).catch(() => {})

    setTabs(prev => prev.map(t =>
      t.id === tabId
        ? { ...t, splitId: null, splitTerminal: null, splitFitAddon: null, splitMounted: false }
        : t
    ))
    setFocusedPane('main')
    setMountTick(t => t + 1)
  }, [prowl])

  const toggleSplit = useCallback(async () => {
    if (!prowl?.terminal || !activeTabId) return
    const tab = tabsRef.current.find(t => t.id === activeTabId)
    if (!tab) return

    if (tab.splitId) {
      closeSplit(activeTabId)
      return
    }

    const splitId = await prowl.terminal.create(cwd)
    const splitTerminal = makeTerminal()
    const splitFitAddon = new FitAddon()
    splitTerminal.loadAddon(splitFitAddon)
    splitTerminal.loadAddon(new WebLinksAddon((_event, uri) => {
      window.open(uri, '_blank')
    }))

    splitTerminal.onData((data: string) => {
      prowl.terminal.write(splitId, data)
    })

    setTabs(prev => prev.map(t =>
      t.id === activeTabId
        ? { ...t, splitId, splitTerminal, splitFitAddon, splitMounted: false }
        : t
    ))
    setFocusedPane('split')
    setMountTick(t => t + 1)
  }, [prowl, activeTabId, cwd, closeSplit])

  /* ── Find bar ── */
  const openFind = useCallback(() => {
    setFindOpen(true)
    setTimeout(() => findInputRef.current?.focus(), 50)
  }, [])

  const closeFind = useCallback(() => {
    setFindOpen(false)
    setFindQuery('')
    const tab = tabsRef.current.find(t => t.id === activeTabId)
    tab?.searchAddon.clearDecorations()
    tab?.terminal.focus()
  }, [activeTabId])

  const findNext = useCallback(() => {
    if (!findQuery) return
    const tab = tabsRef.current.find(t => t.id === activeTabId)
    tab?.searchAddon.findNext(findQuery, { caseSensitive: false, regex: false })
  }, [findQuery, activeTabId])

  const findPrevious = useCallback(() => {
    if (!findQuery) return
    const tab = tabsRef.current.find(t => t.id === activeTabId)
    tab?.searchAddon.findPrevious(findQuery, { caseSensitive: false, regex: false })
  }, [findQuery, activeTabId])

  // Trigger search on query change
  useEffect(() => {
    if (findOpen && findQuery) findNext()
  }, [findQuery])

  /* ── IPC data listener ── */
  const disposedRef = useRef(false)
  useEffect(() => {
    if (!prowl?.terminal) return
    disposedRef.current = false

    prowl.terminal.onData(({ id, data }: { id: string; data: string }) => {
      if (disposedRef.current) return
      const tab = tabsRef.current.find(t => t.id === id)
      if (tab) { tab.terminal.write(data); return }
      const split = tabsRef.current.find(t => t.splitId === id)
      if (split?.splitTerminal) split.splitTerminal.write(data)
    })

    prowl.terminal.onExit(({ id }: { id: string; exitCode: number }) => {
      if (disposedRef.current) return
      const tab = tabsRef.current.find(t => t.id === id)
      if (tab) tab.terminal.writeln('\r\n\x1b[90m[Process exited]\x1b[0m')
      const split = tabsRef.current.find(t => t.splitId === id)
      if (split?.splitTerminal) split.splitTerminal.writeln('\r\n\x1b[90m[Process exited]\x1b[0m')
    })

    return () => {
      disposedRef.current = true
      prowl.terminal.removeAllListeners()
    }
  }, [prowl])

  /* ── Mount / reparent xterm DOM ── */
  useEffect(() => {
    if (!isOpen) return
    const activeTab = tabsRef.current.find(t => t.id === activeTabId)
    if (!activeTab) return

    const timer = setTimeout(() => {
      if (mainTermRef.current) {
        if (!activeTab.mounted) {
          mainTermRef.current.innerHTML = ''
          activeTab.terminal.open(mainTermRef.current)
          activeTab.mounted = true
          if (!activeTab.webglLoaded) {
            try {
              activeTab.terminal.loadAddon(new WebglAddon())
              activeTab.webglLoaded = true
            } catch { /* canvas fallback */ }
          }
        } else {
          const el = activeTab.terminal.element
          if (el && el.parentElement !== mainTermRef.current) {
            mainTermRef.current.innerHTML = ''
            mainTermRef.current.appendChild(el)
          }
        }
        activeTab.fitAddon.fit()
        const dims = activeTab.fitAddon.proposeDimensions()
        if (dims) prowl?.terminal?.resize(activeTab.id, dims.cols, dims.rows)
        activeTab.terminal.focus()
      }

      if (activeTab.splitTerminal && splitTermRef.current) {
        if (!activeTab.splitMounted) {
          splitTermRef.current.innerHTML = ''
          activeTab.splitTerminal.open(splitTermRef.current)
          activeTab.splitMounted = true
        } else {
          const el = activeTab.splitTerminal.element
          if (el && el.parentElement !== splitTermRef.current) {
            splitTermRef.current.innerHTML = ''
            splitTermRef.current.appendChild(el)
          }
        }
        activeTab.splitFitAddon?.fit()
        const dims = activeTab.splitFitAddon?.proposeDimensions()
        if (dims && activeTab.splitId) {
          prowl?.terminal?.resize(activeTab.splitId, dims.cols, dims.rows)
        }
      }
    }, 30)

    return () => clearTimeout(timer)
  }, [activeTabId, isOpen, mountTick, prowl])

  /* ── Auto-create first tab ── */
  useEffect(() => {
    if (isOpen && tabs.length === 0 && !hasInitialized.current) {
      hasInitialized.current = true
      createTab()
    }
  }, [isOpen, tabs.length, createTab])

  /* ── Refit on height change ── */
  useEffect(() => {
    if (!isOpen) return
    const activeTab = tabsRef.current.find(t => t.id === activeTabId)
    if (!activeTab) return

    const timer = setTimeout(() => {
      activeTab.fitAddon.fit()
      const dims = activeTab.fitAddon.proposeDimensions()
      if (dims) prowl?.terminal?.resize(activeTab.id, dims.cols, dims.rows)

      if (activeTab.splitFitAddon && activeTab.splitId) {
        activeTab.splitFitAddon.fit()
        const sd = activeTab.splitFitAddon.proposeDimensions()
        if (sd) prowl?.terminal?.resize(activeTab.splitId, sd.cols, sd.rows)
      }
    }, 50)

    return () => clearTimeout(timer)
  }, [drawerHeight, isOpen, activeTabId, prowl])

  /* ── Drag to resize ── */
  useEffect(() => {
    const onMove = (e: MouseEvent) => {
      if (!resizeRef.current) return
      const delta = resizeRef.current.startY - e.clientY
      setDrawerHeight(Math.min(
        Math.max(resizeRef.current.startHeight + delta, 150),
        window.innerHeight * 0.6
      ))
    }
    const onUp = () => {
      if (resizeRef.current) {
        resizeRef.current = null
        document.body.style.cursor = ''
        document.body.style.userSelect = ''
        localStorage.setItem('prowl-terminal-height', String(drawerHeight))
      }
    }
    window.addEventListener('mousemove', onMove)
    window.addEventListener('mouseup', onUp)
    return () => {
      window.removeEventListener('mousemove', onMove)
      window.removeEventListener('mouseup', onUp)
    }
  }, [drawerHeight])

  /* ── Window resize → refit ── */
  useEffect(() => {
    if (!isOpen) return
    const onResize = () => {
      const activeTab = tabsRef.current.find(t => t.id === activeTabId)
      if (!activeTab) return
      activeTab.fitAddon.fit()
      activeTab.splitFitAddon?.fit()
    }
    window.addEventListener('resize', onResize)
    return () => window.removeEventListener('resize', onResize)
  }, [isOpen, activeTabId])

  /* ── Cmd+F for find ── */
  useEffect(() => {
    if (!isOpen) return
    const handleKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === 'f') {
        // Only capture if terminal area is focused
        const termEl = mainTermRef.current
        if (termEl && (termEl.contains(document.activeElement) || document.activeElement === termEl)) {
          e.preventDefault()
          openFind()
        }
      }
    }
    window.addEventListener('keydown', handleKey)
    return () => window.removeEventListener('keydown', handleKey)
  }, [isOpen, openFind])

  const startResize = (e: React.MouseEvent) => {
    e.preventDefault()
    resizeRef.current = { startY: e.clientY, startHeight: drawerHeight }
    document.body.style.cursor = 'ns-resize'
    document.body.style.userSelect = 'none'
  }

  const activeTab = tabs.find(t => t.id === activeTabId)

  /* ═══════════════════════════════════════════════════════════════
     RENDER
  ═══════════════════════════════════════════════════════════════ */
  return (
    <div
      className={`flex flex-col bg-void ${
        embedded
          ? `flex-1 min-h-0 ${isOpen ? '' : 'hidden'}`
          : `border-t border-white/[0.08] shrink-0 ${isOpen ? 'animate-fade-in' : 'hidden'}`
      }`}
      style={embedded ? undefined : { height: drawerHeight }}
    >
      {/* Resize handle — drawer mode only */}
      {!embedded && (
        <div
          onMouseDown={startResize}
          className="h-[3px] cursor-ns-resize hover:bg-accent/40 transition-colors shrink-0"
        />
      )}

      {/* Tab bar */}
      <div className="flex items-center gap-0.5 px-2 py-1 bg-[#18181a] border-b border-white/[0.06] shrink-0">
        <div className="flex items-center gap-0.5 flex-1 min-w-0 overflow-x-auto scrollbar-thin">
          {tabs.map(tab => (
            <button
              key={tab.id}
              onClick={() => { setActiveTabId(tab.id); setFocusedPane('main'); setMountTick(t => t + 1) }}
              className={`
                group flex items-center gap-1.5 px-3 py-1 rounded-lg text-xs
                transition-all duration-150 shrink-0 max-w-[200px]
                ${tab.id === activeTabId
                  ? 'bg-white/[0.08] text-text-primary'
                  : 'text-text-muted hover:text-text-secondary hover:bg-white/[0.04]'
                }
              `}
            >
              <span className={`w-1.5 h-1.5 rounded-full shrink-0 ${
                tab.id === activeTabId ? 'bg-[#30D158]' : 'bg-white/20'
              }`} />
              <TerminalIcon size={11} className="shrink-0 opacity-40" />
              <span className="truncate">{tab.name}</span>
              {tabs.length > 1 && (
                <span
                  onClick={(e) => { e.stopPropagation(); closeTab(tab.id) }}
                  className="ml-auto opacity-0 group-hover:opacity-60 hover:!opacity-100 transition-opacity cursor-pointer shrink-0"
                >
                  <X size={11} />
                </span>
              )}
            </button>
          ))}
        </div>

        <div className="flex items-center gap-0.5 ml-2 shrink-0">
          <button
            onClick={createTab}
            className="p-1 rounded-lg hover:bg-white/[0.06] text-text-muted hover:text-text-secondary transition-colors"
            title="New terminal"
          >
            <Plus size={14} />
          </button>
          <button
            onClick={toggleSplit}
            className={`p-1 rounded-lg hover:bg-white/[0.06] transition-colors ${
              activeTab?.splitId ? 'text-accent' : 'text-text-muted hover:text-text-secondary'
            }`}
            title={activeTab?.splitId ? 'Close split' : 'Split terminal'}
          >
            <Columns2 size={14} />
          </button>
          <button
            onClick={openFind}
            className="p-1 rounded-lg hover:bg-white/[0.06] text-text-muted hover:text-text-secondary transition-colors"
            title="Find in terminal"
          >
            <Search size={14} />
          </button>
          {!embedded && (
            <button
              onClick={onToggle}
              className="p-1 rounded-lg hover:bg-white/[0.06] text-text-muted hover:text-text-secondary transition-colors"
              title="Minimize terminal"
            >
              <ChevronDown size={14} />
            </button>
          )}
        </div>
      </div>

      {/* Find bar */}
      {findOpen && (
        <div className="flex items-center gap-2 px-3 py-1.5 bg-[#18181a] border-b border-white/[0.06] shrink-0 animate-fade-in">
          <Search size={12} className="text-text-muted shrink-0" />
          <input
            ref={findInputRef}
            type="text"
            value={findQuery}
            onChange={e => setFindQuery(e.target.value)}
            onKeyDown={e => {
              if (e.key === 'Enter') {
                e.preventDefault()
                e.shiftKey ? findPrevious() : findNext()
              }
              if (e.key === 'Escape') closeFind()
            }}
            placeholder="Find..."
            className="flex-1 bg-transparent text-xs text-text-primary placeholder:text-text-muted outline-none"
          />
          <button onClick={findPrevious} className="p-0.5 rounded hover:bg-white/[0.06] text-text-muted hover:text-text-secondary transition-colors" title="Previous">
            <ChevronUp size={13} />
          </button>
          <button onClick={findNext} className="p-0.5 rounded hover:bg-white/[0.06] text-text-muted hover:text-text-secondary transition-colors" title="Next">
            <ChevronDown size={13} />
          </button>
          <button onClick={closeFind} className="p-0.5 rounded hover:bg-white/[0.06] text-text-muted hover:text-text-secondary transition-colors">
            <X size={13} />
          </button>
        </div>
      )}

      {/* Terminal panes */}
      <div className="flex-1 flex min-h-0 overflow-hidden">
        <div
          ref={mainTermRef}
          onClick={() => { setFocusedPane('main'); activeTab?.terminal.focus() }}
          className={`flex-1 min-w-0 ${
            activeTab?.splitId && focusedPane === 'main' ? 'border-t-2 border-accent/60' : 'border-t-2 border-transparent'
          }`}
        />

        {activeTab?.splitId && (
          <>
            <div className="relative w-[1px] bg-white/[0.08] shrink-0 group/divider">
              <button
                onClick={() => closeSplit(activeTab.id)}
                className="absolute top-1 left-1/2 -translate-x-1/2 z-10 p-0.5 rounded bg-elevated/80 text-text-muted hover:text-text-primary opacity-0 group-hover/divider:opacity-100 transition-opacity"
                title="Close split"
              >
                <X size={10} />
              </button>
            </div>
            <div
              ref={splitTermRef}
              onClick={() => { setFocusedPane('split'); activeTab?.splitTerminal?.focus() }}
              className={`flex-1 min-w-0 ${
                focusedPane === 'split' ? 'border-t-2 border-accent/60' : 'border-t-2 border-transparent'
              }`}
            />
          </>
        )}
      </div>
    </div>
  )
}
