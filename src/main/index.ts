import { app, BrowserWindow, dialog, ipcMain } from 'electron'
import { join } from 'path'
import { WorkspaceWatcher } from './watcher'
import { LogParser } from './parser'

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
    webPreferences: {
      preload: join(__dirname, '../preload/index.js'),
      contextIsolation: true,
      nodeIntegration: false
    }
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

async function initializeWatcher(workspacePath: string): Promise<void> {
  watcher = new WorkspaceWatcher(workspacePath)

  watcher.on('file:read', (filepath) => {
    mainWindow?.webContents.send('node:activate', { filepath, type: 'read' })
  })

  watcher.on('file:write', (filepath) => {
    mainWindow?.webContents.send('node:activate', { filepath, type: 'write' })
  })

  watcher.on('file:add', (filepath) => {
    mainWindow?.webContents.send('graph:update', { action: 'add', filepath })
  })

  watcher.on('file:remove', (filepath) => {
    mainWindow?.webContents.send('graph:update', { action: 'remove', filepath })
  })

  await watcher.start()
}

function initializeLogParser(logPath: string): void {
  parser = new LogParser(logPath)

  parser.on('tool:call', (data) => {
    mainWindow?.webContents.send('tool:activate', data)
  })

  parser.start()
}

app.whenReady().then(() => {
  createWindow()

  ipcMain.handle('workspace:scan', async (_, workspacePath: string) => {
    if (watcher) watcher.stop()
    await initializeWatcher(workspacePath)
    return watcher?.getFileTree() ?? []
  })

  ipcMain.handle('logs:watch', async (_, logPath: string) => {
    if (parser) parser.stop()
    initializeLogParser(logPath)
    return true
  })

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
      filters: [{ name: 'Log Files', extensions: ['log', 'txt'] }]
    })
    if (result.canceled || result.filePaths.length === 0) return null
    return result.filePaths[0]
  })

  ipcMain.handle('file:read', async (_, filepath: string) => {
    const fs = await import('fs/promises')
    try {
      return await fs.readFile(filepath, 'utf-8')
    } catch {
      return null
    }
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
