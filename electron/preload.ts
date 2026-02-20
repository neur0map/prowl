import { contextBridge, ipcRenderer } from 'electron'

export interface FileActivity {
  filepath: string
  type: 'add' | 'write' | 'remove' | 'access'
}

export interface ToolEvent {
  timestamp: number
  tool: string
  action?: string
  filepath?: string
  duration?: number
}

export interface FileEntry {
  path: string
  content: string
}

export interface UpdateInfo {
  currentVersion: string
  latestVersion: string
  releaseUrl: string
  releaseName: string
}

const prowlApi = {
  // Dialog
  selectDirectory: (): Promise<string | null> =>
    ipcRenderer.invoke('dialog:openDirectory'),
  selectFile: (): Promise<string | null> =>
    ipcRenderer.invoke('dialog:openFile'),

  // Folder scan
  scanFolder: (path: string): Promise<FileEntry[]> =>
    ipcRenderer.invoke('folder:scan', path),

  // Watcher
  startWatcher: (path: string) =>
    ipcRenderer.invoke('watcher:start', path),
  stopWatcher: () =>
    ipcRenderer.invoke('watcher:stop'),

  // Parser
  startParser: (path: string) =>
    ipcRenderer.invoke('parser:start', path),
  stopParser: () =>
    ipcRenderer.invoke('parser:stop'),

  // Events from main process
  onFileActivity: (cb: (data: FileActivity) => void) => {
    ipcRenderer.on('agent:file-activity', (_, data) => cb(data))
  },
  onToolEvent: (cb: (data: ToolEvent) => void) => {
    ipcRenderer.on('agent:tool-event', (_, data) => cb(data))
  },

  removeAllListeners: () => {
    ipcRenderer.removeAllListeners('agent:file-activity')
    ipcRenderer.removeAllListeners('agent:tool-event')
  },

  // File system (for code editor)
  fs: {
    readFile: (filePath: string): Promise<string> =>
      ipcRenderer.invoke('fs:readFile', filePath),
    writeFile: (filePath: string, content: string): Promise<void> =>
      ipcRenderer.invoke('fs:writeFile', filePath, content),
  },

  // Secure storage (safeStorage — OS keychain encryption)
  secureStorage: {
    isAvailable: (): Promise<boolean> =>
      ipcRenderer.invoke('secureStorage:isAvailable'),
    store: (key: string, value: string): Promise<void> =>
      ipcRenderer.invoke('secureStorage:store', key, value),
    retrieve: (key: string): Promise<string | null> =>
      ipcRenderer.invoke('secureStorage:retrieve', key),
    delete: (key: string): Promise<void> =>
      ipcRenderer.invoke('secureStorage:delete', key),
  },

  // OAuth
  oauth: {
    openExternal: (url: string): Promise<void> =>
      ipcRenderer.invoke('oauth:openExternal', url),
    onCallback: (cb: (data: { code: string | null; state: string | null; error: string | null }) => void) => {
      ipcRenderer.on('oauth:callback', (_, data) => cb(data))
    },
    removeCallbackListener: () => {
      ipcRenderer.removeAllListeners('oauth:callback')
    },
  },

  // Terminal
  terminal: {
    create: (cwd?: string): Promise<string> =>
      ipcRenderer.invoke('terminal:create', cwd),
    write: (id: string, data: string): void =>
      ipcRenderer.send('terminal:write', id, data),
    resize: (id: string, cols: number, rows: number): void =>
      ipcRenderer.send('terminal:resize', id, cols, rows),
    kill: (id: string): Promise<void> =>
      ipcRenderer.invoke('terminal:kill', id),
    getTitle: (id: string): Promise<string> =>
      ipcRenderer.invoke('terminal:getTitle', id),
    onData: (cb: (data: { id: string; data: string }) => void) => {
      ipcRenderer.on('terminal:data', (_, payload) => cb(payload))
    },
    onExit: (cb: (data: { id: string; exitCode: number }) => void) => {
      ipcRenderer.on('terminal:exit', (_, payload) => cb(payload))
    },
    removeAllListeners: () => {
      ipcRenderer.removeAllListeners('terminal:data')
      ipcRenderer.removeAllListeners('terminal:exit')
    },
  },

  // Update checker
  updater: {
    check: (): Promise<UpdateInfo | null> =>
      ipcRenderer.invoke('updater:check'),
    onUpdateAvailable: (cb: (info: UpdateInfo) => void) => {
      ipcRenderer.on('updater:update-available', (_, info) => cb(info))
    },
    removeUpdateListener: () => {
      ipcRenderer.removeAllListeners('updater:update-available')
    },
  },
}

contextBridge.exposeInMainWorld('prowl', prowlApi)

declare global {
  interface Window {
    prowl: typeof prowlApi
  }
}
