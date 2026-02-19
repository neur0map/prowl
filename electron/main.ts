import { app, BrowserWindow, dialog, ipcMain, session } from 'electron'
import { join } from 'path'
import { homedir } from 'os'
import { readdir, readFile, stat } from 'fs/promises'
import { WorkspaceWatcher } from './watcher'
import { LogParser } from './parser'

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

  // Watcher handlers
  ipcMain.handle('watcher:start', async (_, workspacePath: string) => {
    if (watcher) watcher.stop()
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

    watcher = new WorkspaceWatcher(resolved)

    watcher.on('file:add', (filepath) => {
      mainWindow?.webContents.send('agent:file-activity', { filepath, type: 'add' })
    })
    watcher.on('file:write', (filepath) => {
      mainWindow?.webContents.send('agent:file-activity', { filepath, type: 'write' })
    })
    watcher.on('file:remove', (filepath) => {
      mainWindow?.webContents.send('agent:file-activity', { filepath, type: 'remove' })
    })

    await watcher.start()
    return watcher.getFileTree()
  })

  ipcMain.handle('watcher:stop', async () => {
    watcher?.stop()
    watcher = null
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
})

app.on('window-all-closed', () => {
  watcher?.stop()
  parser?.stop()
  if (process.platform !== 'darwin') {
    app.quit()
  }
})

app.on('activate', () => {
  if (mainWindow === null) {
    createWindow()
  }
})
