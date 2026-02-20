import { app, BrowserWindow, dialog, ipcMain, safeStorage, session, shell } from 'electron'
import { join } from 'path'
import { homedir } from 'os'
import { readdir, readFile, writeFile, stat } from 'fs/promises'
import { WorkspaceWatcher } from './watcher'
import { LogParser } from './parser'
import { ClaudeLogWatcher } from './claude-log-watcher'
import { ProcessFileMonitor } from './process-file-monitor'
import { OpenClawWSClient } from './openclaw-ws-client'
import { TerminalManager } from './terminal-manager'

function resolvePath(p: string): string {
  if (p.startsWith('~/') || p === '~') {
    return join(homedir(), p.slice(1))
  }
  return p
}

// Ignore rules (mirrors src/config/ignore-service.ts)
const IGNORED_DIRS = new Set([
  '.git', '.svn', '.hg', 'node_modules', 'bower_components', 'vendor',
  'venv', '.venv', '__pycache__', '.pytest_cache', '.mypy_cache',
  'dist', 'build', 'out', 'target', '.next', '.nuxt', '.vercel',
  'coverage', '.nyc_output', 'logs', 'tmp', 'temp', 'cache', '.cache',
  '.idea', '.vscode', '.vs', '.DS_Store',
])

const IGNORED_EXTENSIONS = new Set([
  '.png', '.jpg', '.jpeg', '.gif', '.svg', '.ico', '.webp', '.bmp',
  '.zip', '.tar', '.gz', '.rar', '.7z',
  '.exe', '.dll', '.so', '.dylib', '.o', '.obj', '.class', '.jar',
  '.pyc', '.pyo', '.wasm', '.node',
  '.pdf', '.doc', '.docx', '.xls', '.xlsx',
  '.mp4', '.mp3', '.wav', '.mov', '.avi',
  '.woff', '.woff2', '.ttf', '.eot', '.otf',
  '.db', '.sqlite', '.sqlite3',
  '.map', '.lock',
  '.pem', '.key', '.crt',
  '.csv', '.parquet', '.h5', '.pkl', '.pickle',
  '.bin', '.dat', '.iso', '.img', '.dmg',
])

const IGNORED_FILES = new Set([
  'package-lock.json', 'yarn.lock', 'pnpm-lock.yaml',
  'composer.lock', 'Gemfile.lock', 'poetry.lock', 'Cargo.lock', 'go.sum',
  '.gitignore', '.gitattributes', '.npmrc', '.editorconfig',
  '.prettierrc', '.prettierignore', '.eslintignore', '.dockerignore',
  'Thumbs.db', '.DS_Store', 'LICENSE', 'LICENSE.md', 'LICENSE.txt',
  'CHANGELOG.md', 'CONTRIBUTING.md', 'CODE_OF_CONDUCT.md', 'SECURITY.md',
  '.env', '.env.local', '.env.development', '.env.production', '.env.test',
])

interface FileEntry {
  path: string
  content: string
}

function shouldIgnoreFile(name: string): boolean {
  if (IGNORED_FILES.has(name)) return true
  const lower = name.toLowerCase()
  const dot = lower.lastIndexOf('.')
  if (dot !== -1) {
    const ext = lower.substring(dot)
    if (IGNORED_EXTENSIONS.has(ext)) return true
    // Compound extensions: .min.js, .bundle.js, .d.ts
    const dot2 = lower.lastIndexOf('.', dot - 1)
    if (dot2 !== -1) {
      const compound = lower.substring(dot2)
      if (IGNORED_EXTENSIONS.has(compound)) return true
    }
  }
  if (lower.includes('.bundle.') || lower.includes('.chunk.') ||
      lower.includes('.generated.') || lower.endsWith('.d.ts')) {
    return true
  }
  return false
}

async function scanDirectory(dirPath: string, basePath: string): Promise<FileEntry[]> {
  const files: FileEntry[] = []
  let entries: string[]
  try {
    entries = await readdir(dirPath).then(e => e as unknown as string[])
  } catch {
    return files
  }

  const dirEntries = await readdir(dirPath, { withFileTypes: true })

  for (const entry of dirEntries) {
    const fullPath = join(dirPath, entry.name)
    const relativePath = fullPath.replace(basePath + '/', '')

    if (entry.isDirectory()) {
      if (IGNORED_DIRS.has(entry.name) || entry.name.startsWith('.')) continue
      const subFiles = await scanDirectory(fullPath, basePath)
      files.push(...subFiles)
    } else {
      if (shouldIgnoreFile(entry.name)) continue
      try {
        const content = await readFile(fullPath, 'utf-8')
        files.push({ path: relativePath, content })
      } catch {
        // Skip binary or unreadable files
      }
    }
  }

  return files
}

let mainWindow: BrowserWindow | null = null
let watcher: WorkspaceWatcher | null = null
let parser: LogParser | null = null
let claudeWatcher: ClaudeLogWatcher | null = null
let processMonitor: ProcessFileMonitor | null = null
let openclawClient: OpenClawWSClient | null = null
const terminalManager = new TerminalManager()

// ── Secure Storage (safeStorage) ──
// Encrypts API keys using OS keychain (macOS Keychain, Windows DPAPI, Linux libsecret)
// Stores encrypted data as base64 in a JSON file in userData directory

const getSecureStoragePath = () => join(app.getPath('userData'), 'secure-keys.json')

async function readSecureStore(): Promise<Record<string, string>> {
  try {
    const data = await readFile(getSecureStoragePath(), 'utf-8')
    return JSON.parse(data)
  } catch {
    return {}
  }
}

async function writeSecureStore(store: Record<string, string>): Promise<void> {
  await writeFile(getSecureStoragePath(), JSON.stringify(store), 'utf-8')
}

// ── OAuth Deep Link Protocol ──
// Register prowl:// as default protocol for this app
if (process.defaultApp) {
  if (process.argv.length >= 2) {
    app.setAsDefaultProtocolClient('prowl', process.execPath, [process.argv[1]])
  }
} else {
  app.setAsDefaultProtocolClient('prowl')
}

// Handle deep link on macOS (app already running)
app.on('open-url', (_event, url) => {
  handleDeepLink(url)
})

// Handle deep link on Windows/Linux (second instance)
const gotTheLock = app.requestSingleInstanceLock()
if (!gotTheLock) {
  app.quit()
} else {
  app.on('second-instance', (_event, commandLine) => {
    // Windows/Linux: deep link comes as last argument
    const url = commandLine.find(arg => arg.startsWith('prowl://'))
    if (url) handleDeepLink(url)
    // Focus window
    if (mainWindow) {
      if (mainWindow.isMinimized()) mainWindow.restore()
      mainWindow.focus()
    }
  })
}

function handleDeepLink(url: string): void {
  try {
    const parsed = new URL(url)
    if (parsed.hostname === 'oauth' && parsed.pathname === '/callback') {
      const code = parsed.searchParams.get('code')
      const state = parsed.searchParams.get('state')
      const error = parsed.searchParams.get('error')
      if (mainWindow) {
        mainWindow.webContents.send('oauth:callback', { code, state, error })
      }
    }
  } catch {
    // Invalid URL, ignore
  }
}

function createWindow(): void {
  mainWindow = new BrowserWindow({
    width: 1400,
    height: 900,
    minWidth: 800,
    minHeight: 600,
    backgroundColor: '#00000000',
    transparent: true,
    vibrancy: 'under-window',
    visualEffectState: 'active',
    titleBarStyle: 'hiddenInset',
    trafficLightPosition: { x: 16, y: 18 },
    title: 'Prowl',
    webPreferences: {
      preload: join(__dirname, '../preload/index.js'),
      contextIsolation: true,
      nodeIntegration: false,
    }
  })

  // COOP/COEP headers for SharedArrayBuffer (KuzuDB WASM)
  session.defaultSession.webRequest.onHeadersReceived((details, callback) => {
    callback({
      responseHeaders: {
        ...details.responseHeaders,
        'Cross-Origin-Opener-Policy': ['same-origin'],
        'Cross-Origin-Embedder-Policy': ['require-corp'],
      }
    })
  })

  if (process.env.NODE_ENV === 'development') {
    mainWindow.loadURL('http://localhost:5173')
    mainWindow.webContents.openDevTools()
  } else {
    mainWindow.loadFile(join(__dirname, '../renderer/index.html'))
  }

  mainWindow.on('closed', () => {
    mainWindow = null
  })
}

app.whenReady().then(() => {
  createWindow()
  terminalManager.setWindow(mainWindow!)

  // Dialog handlers
  ipcMain.handle('dialog:openDirectory', async () => {
    const result = await dialog.showOpenDialog({
      properties: ['openDirectory']
    })
    if (result.canceled || result.filePaths.length === 0) return null
    return result.filePaths[0]
  })

  ipcMain.handle('dialog:openFile', async () => {
    const result = await dialog.showOpenDialog({
      properties: ['openFile'],
      filters: [{ name: 'Log Files', extensions: ['log', 'txt', 'jsonl'] }]
    })
    if (result.canceled || result.filePaths.length === 0) return null
    return result.filePaths[0]
  })

  // Folder scan handler (for Local Folder ingestion)
  ipcMain.handle('folder:scan', async (_, dirPath: string) => {
    const resolved = resolvePath(dirPath)
    return scanDirectory(resolved, resolved)
  })

  // Watcher handlers — starts all 4 detection layers:
  // 1. chokidar (filesystem writes/adds/removes)
  // 2. Claude Code JSONL parser (agent tool calls)
  // 3. lsof polling (process file access — reads, mmaps, handles)
  // 4. OpenClaw WebSocket (real-time agent tool events)
  ipcMain.handle('watcher:start', async (_, workspacePath: string) => {
    // Stop any existing watchers
    stopAllWatchers()

    const resolved = resolvePath(workspacePath)

    // Validate path is a directory before starting
    try {
      const stats = await stat(resolved)
      if (!stats.isDirectory()) {
        throw new Error(`Not a directory: ${resolved}`)
      }
    } catch (err: any) {
      throw new Error(`Invalid workspace path: ${err.message}`)
    }

    // Layer 1: chokidar — filesystem writes
    watcher = new WorkspaceWatcher(resolved)
    watcher.on('file:add', (filepath) => {
      console.log('[prowl:chokidar] add:', filepath)
      mainWindow?.webContents.send('agent:file-activity', { filepath, type: 'add' })
    })
    watcher.on('file:write', (filepath) => {
      console.log('[prowl:chokidar] write:', filepath)
      mainWindow?.webContents.send('agent:file-activity', { filepath, type: 'write' })
    })
    watcher.on('file:remove', (filepath) => {
      console.log('[prowl:chokidar] remove:', filepath)
      mainWindow?.webContents.send('agent:file-activity', { filepath, type: 'remove' })
    })
    await watcher.start()

    // Layer 2: Claude Code JSONL log watcher — agent tool calls
    claudeWatcher = new ClaudeLogWatcher(resolved)
    claudeWatcher.on('tool:call', (data) => {
      console.log('[prowl:claude-log] tool:', data.tool, data.filepath || '')
      mainWindow?.webContents.send('agent:tool-event', {
        timestamp: data.timestamp,
        tool: data.tool,
        action: data.action,
        filepath: data.filepath,
      })
    })
    await claudeWatcher.start()

    // Layer 3: lsof polling — process file access (macOS/Linux only)
    if (process.platform === 'darwin' || process.platform === 'linux') {
      processMonitor = new ProcessFileMonitor(resolved)
      processMonitor.on('file:access', (data) => {
        console.log('[prowl:lsof] access:', data.filepath, 'by', data.process)
        mainWindow?.webContents.send('agent:file-activity', {
          filepath: data.filepath,
          type: 'access',
        })
      })
      processMonitor.start()
    }

    // Layer 4: OpenClaw WebSocket — only for OpenClaw workspaces
    const isOpenClawWorkspace = resolved.toLowerCase().includes('openclaw') ||
      resolved.includes('.openclaw')
    if (isOpenClawWorkspace) {
      openclawClient = new OpenClawWSClient(resolved)
      openclawClient.on('tool:call', (data) => {
        console.log('[prowl:openclaw-ws] tool:', data.tool, data.filepath || '')
        mainWindow?.webContents.send('agent:tool-event', {
          timestamp: data.timestamp,
          tool: data.tool,
          action: data.action,
          filepath: data.filepath,
        })
      })
      openclawClient.start()
      console.log('[prowl] openclaw workspace detected, WS client started')
    }

    console.log('[prowl] all detection layers started for:', resolved)
    return watcher.getFileTree()
  })

  ipcMain.handle('watcher:stop', async () => {
    stopAllWatchers()
  })

  // Parser handlers
  ipcMain.handle('parser:start', async (_, logPath: string) => {
    if (parser) parser.stop()
    const resolved = resolvePath(logPath)
    parser = new LogParser(resolved)

    parser.on('tool:call', (data) => {
      mainWindow?.webContents.send('agent:tool-event', data)
    })

    parser.start()
    return true
  })

  ipcMain.handle('parser:stop', async () => {
    parser?.stop()
    parser = null
  })

  // File system handlers (for code editor autosave)
  ipcMain.handle('fs:readFile', async (_, filePath: string) => {
    const resolved = resolvePath(filePath)
    return readFile(resolved, 'utf-8')
  })

  ipcMain.handle('fs:writeFile', async (_, filePath: string, content: string) => {
    const resolved = resolvePath(filePath)
    await writeFile(resolved, content, 'utf-8')
  })

  // Terminal handlers
  ipcMain.handle('terminal:create', (_, cwd?: string) => {
    return terminalManager.create(cwd)
  })

  ipcMain.on('terminal:write', (_, id: string, data: string) => {
    terminalManager.write(id, data)
  })

  ipcMain.on('terminal:resize', (_, id: string, cols: number, rows: number) => {
    terminalManager.resize(id, cols, rows)
  })

  ipcMain.handle('terminal:kill', (_, id: string) => {
    terminalManager.kill(id)
  })

  ipcMain.handle('terminal:getTitle', (_, id: string) => {
    return terminalManager.getTitle(id)
  })

  // Secure storage handlers (safeStorage)
  ipcMain.handle('secureStorage:isAvailable', () => {
    return safeStorage.isEncryptionAvailable()
  })

  ipcMain.handle('secureStorage:store', async (_, key: string, value: string) => {
    if (!safeStorage.isEncryptionAvailable()) {
      throw new Error('Encryption not available')
    }
    const encrypted = safeStorage.encryptString(value)
    const store = await readSecureStore()
    store[key] = encrypted.toString('base64')
    await writeSecureStore(store)
  })

  ipcMain.handle('secureStorage:retrieve', async (_, key: string) => {
    if (!safeStorage.isEncryptionAvailable()) {
      throw new Error('Encryption not available')
    }
    const store = await readSecureStore()
    const encrypted = store[key]
    if (!encrypted) return null
    try {
      return safeStorage.decryptString(Buffer.from(encrypted, 'base64'))
    } catch {
      return null
    }
  })

  ipcMain.handle('secureStorage:delete', async (_, key: string) => {
    const store = await readSecureStore()
    delete store[key]
    await writeSecureStore(store)
  })

  // OAuth handlers
  ipcMain.handle('oauth:openExternal', async (_, url: string) => {
    await shell.openExternal(url)
  })
})

function stopAllWatchers(): void {
  watcher?.stop()
  watcher = null
  claudeWatcher?.stop()
  claudeWatcher = null
  processMonitor?.stop()
  processMonitor = null
  openclawClient?.stop()
  openclawClient = null
}

app.on('window-all-closed', () => {
  stopAllWatchers()
  parser?.stop()
  terminalManager.killAll()
  if (process.platform !== 'darwin') {
    app.quit()
  }
})

app.on('activate', () => {
  if (mainWindow === null) {
    createWindow()
  }
})
