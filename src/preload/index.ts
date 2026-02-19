import { contextBridge, ipcRenderer } from 'electron'

export interface NodeActivation {
  filepath: string
  type: 'read' | 'write'
}

export interface GraphUpdate {
  action: 'add' | 'remove'
  filepath: string
}

export interface ToolActivation {
  timestamp: number
  tool: string
  filepath?: string
}

const api = {
  scanWorkspace: (path: string) => ipcRenderer.invoke('workspace:scan', path),
  watchLogs: (path: string) => ipcRenderer.invoke('logs:watch', path),
  readFile: (path: string) => ipcRenderer.invoke('file:read', path),
  selectDirectory: (): Promise<string | null> => ipcRenderer.invoke('dialog:openDirectory'),
  selectFile: (): Promise<string | null> => ipcRenderer.invoke('dialog:openFile'),

  onNodeActivate: (callback: (data: NodeActivation) => void) => {
    ipcRenderer.on('node:activate', (_, data) => callback(data))
  },

  onGraphUpdate: (callback: (data: GraphUpdate) => void) => {
    ipcRenderer.on('graph:update', (_, data) => callback(data))
  },

  onToolActivate: (callback: (data: ToolActivation) => void) => {
    ipcRenderer.on('tool:activate', (_, data) => callback(data))
  },

  removeAllListeners: () => {
    ipcRenderer.removeAllListeners('node:activate')
    ipcRenderer.removeAllListeners('graph:update')
    ipcRenderer.removeAllListeners('tool:activate')
  }
}

contextBridge.exposeInMainWorld('api', api)

declare global {
  interface Window {
    api: typeof api
  }
}
