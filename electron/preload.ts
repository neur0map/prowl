import { contextBridge, ipcRenderer } from 'electron'

export interface FileActivity {
  filepath: string
  type: 'add' | 'write' | 'remove'
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
}

contextBridge.exposeInMainWorld('prowl', prowlApi)

declare global {
  interface Window {
    prowl: typeof prowlApi
  }
}
