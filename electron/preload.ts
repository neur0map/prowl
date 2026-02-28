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

// MCP request queue: main → preload via IPC, renderer polls via contextBridge
const mcpRequestQueue: Array<{ requestId: string; toolName: string; params: unknown }> = []
ipcRenderer.on('mcp:request', (_, data) => {
  mcpRequestQueue.push(data)
  ipcRenderer.send('mcp:log', `queued: ${data.toolName} (size=${mcpRequestQueue.length})`)
})

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
  onFileChanged: (cb: (data: { filepath: string; type: string; content: string | null }) => void) => {
    ipcRenderer.on('mcp:file-changed', (_, data) => cb(data))
  },
  onSaveSnapshot: (cb: () => void) => {
    ipcRenderer.on('mcp:save-snapshot', () => cb())
  },

  // Remove only agent-watcher listeners (file activity + tool events).
  // mcp:file-changed and mcp:save-snapshot are managed by their own lifecycle
  // and must survive project switches.
  removeAgentListeners: () => {
    ipcRenderer.removeAllListeners('agent:file-activity')
    ipcRenderer.removeAllListeners('agent:tool-event')
  },

  // Remove the live-reindex listener (only called on full cleanup / unmount)
  removeFileChangedListener: () => {
    ipcRenderer.removeAllListeners('mcp:file-changed')
  },

  removeSaveSnapshotListener: () => {
    ipcRenderer.removeAllListeners('mcp:save-snapshot')
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
    clearAll: (): Promise<void> =>
      ipcRenderer.invoke('secureStorage:clearAll'),
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

  // Snapshot persistence
  snapshot: {
    write: (projectPath: string, data: Uint8Array): Promise<void> =>
      ipcRenderer.invoke('snapshot:write', projectPath, data),
    read: (projectPath: string): Promise<Uint8Array | null> =>
      ipcRenderer.invoke('snapshot:read', projectPath),
    writeMeta: (projectPath: string, meta: object): Promise<void> =>
      ipcRenderer.invoke('snapshot:writeMeta', projectPath, meta),
    readMeta: (projectPath: string): Promise<object | null> =>
      ipcRenderer.invoke('snapshot:readMeta', projectPath),
    writeManifest: (projectPath: string, manifest: object): Promise<void> =>
      ipcRenderer.invoke('snapshot:writeManifest', projectPath, manifest),
    readManifest: (projectPath: string): Promise<object | null> =>
      ipcRenderer.invoke('snapshot:readManifest', projectPath),
    exists: (projectPath: string): Promise<boolean> =>
      ipcRenderer.invoke('snapshot:exists', projectPath),
    verify: (data: Uint8Array, hmac: string): Promise<boolean> =>
      ipcRenderer.invoke('snapshot:verify', data, hmac),
    generateHMAC: (data: Uint8Array): Promise<string> =>
      ipcRenderer.invoke('snapshot:generateHMAC', data),
    ensureGitignore: (projectPath: string): Promise<void> =>
      ipcRenderer.invoke('snapshot:ensureGitignore', projectPath),
    deleteProject: (projectPath: string): Promise<void> =>
      ipcRenderer.invoke('snapshot:deleteProject', projectPath),
    diskUsage: (projectPath: string): Promise<number> =>
      ipcRenderer.invoke('snapshot:diskUsage', projectPath),
    detectChanges: (projectPath: string, gitCommit: string | null, manifest: object): Promise<any> =>
      ipcRenderer.invoke('snapshot:detectChanges', projectPath, gitCommit, manifest),
  },

  // Conversation history
  conversations: {
    list: (projectPath: string): Promise<any[]> =>
      ipcRenderer.invoke('conversation:list', projectPath),
    save: (projectPath: string, conversation: any): Promise<void> =>
      ipcRenderer.invoke('conversation:save', projectPath, conversation),
    delete: (projectPath: string, conversationId: string): Promise<void> =>
      ipcRenderer.invoke('conversation:delete', projectPath, conversationId),
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

  // Git HTTP proxy (avoids CORS in renderer)
  git: {
    httpRequest: (opts: {
      url: string
      method: string
      headers: Record<string, string>
      body?: number[]
    }): Promise<{
      statusCode: number
      statusMessage: string
      headers: Record<string, string>
      body: number[]
    }> => ipcRenderer.invoke('git:http-request', opts),
  },

  // MCP (Model Context Protocol) bridge
  mcp: {
    configureClaudeCode: (): Promise<{ success: boolean; path?: string; error?: string }> =>
      ipcRenderer.invoke('mcp:configure-claude-code'),
    removeClaudeCode: (): Promise<{ success: boolean; error?: string }> =>
      ipcRenderer.invoke('mcp:remove-claude-code'),
    getConfigStatus: (): Promise<{ configured: boolean; configPath: string | null; mcpServerPath: string | null }> =>
      ipcRenderer.invoke('mcp:get-config-status'),
    getServerInfo: (): Promise<{ port: number } | null> =>
      ipcRenderer.invoke('mcp:get-server-info'),
    // MCP request queue: preload collects IPC events, renderer polls for them.
    // This avoids contextBridge callback deadlocks — renderer initiates all calls.
    pollRequests: (): Array<{ requestId: string; toolName: string; params: unknown }> => {
      if (mcpRequestQueue.length > 0) {
        ipcRenderer.send('mcp:log', `poll: draining ${mcpRequestQueue.length} items`)
      }
      const items = [...mcpRequestQueue]
      mcpRequestQueue.length = 0
      return items
    },
    // Called by renderer to send MCP tool results back to main process
    sendResult: (requestId: string, result: unknown): void => {
      ipcRenderer.send('mcp:log', `sendResult: ${requestId} result=${JSON.stringify(result)?.slice(0, 120)}`)
      ipcRenderer.send('mcp:response', requestId, result)
    },
  },
}

contextBridge.exposeInMainWorld('prowl', prowlApi)

declare global {
  interface Window {
    prowl: typeof prowlApi
  }
}
