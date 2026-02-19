export interface FileNode {
  path: string
  name: string
  type: 'file' | 'directory'
  extension?: string
  children?: FileNode[]
}

export interface NodeActivation {
  filepath: string
  type: 'read' | 'write'
}

export interface GraphUpdate {
  action: 'add' | 'remove'
  filepath: string
}

export interface ToolCall {
  timestamp: number
  tool: string
  action?: string
  filepath?: string
  duration?: number
}
