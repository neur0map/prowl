import { EventEmitter } from 'events'
import { watch, FSWatcher } from 'chokidar'
import { readdir, stat } from 'fs/promises'
import { join, relative, extname } from 'path'

export interface FileNode {
  path: string
  name: string
  type: 'file' | 'directory'
  extension?: string
  children?: FileNode[]
}

const IGNORED_PATTERNS = [
  '**/node_modules/**',
  '**/.git/**',
  '**/dist/**',
  '**/*.log'
]

const WATCHED_EXTENSIONS = ['.md', '.ts', '.js', '.json', '.yaml', '.yml']

export class WorkspaceWatcher extends EventEmitter {
  private watcher: FSWatcher | null = null
  private workspacePath: string
  private fileTree: FileNode[] = []

  constructor(workspacePath: string) {
    super()
    this.workspacePath = workspacePath
  }

  async start(): Promise<void> {
    this.fileTree = await this.buildFileTree(this.workspacePath)

    this.watcher = watch(this.workspacePath, {
      ignored: IGNORED_PATTERNS,
      persistent: true,
      ignoreInitial: true,
      awaitWriteFinish: {
        stabilityThreshold: 100,
        pollInterval: 50
      }
    })

    this.watcher
      .on('add', (filepath) => this.handleAdd(filepath))
      .on('change', (filepath) => this.handleChange(filepath))
      .on('unlink', (filepath) => this.handleRemove(filepath))
  }

  stop(): void {
    this.watcher?.close()
    this.watcher = null
  }

  getFileTree(): FileNode[] {
    return this.fileTree
  }

  private async buildFileTree(dirPath: string): Promise<FileNode[]> {
    const entries = await readdir(dirPath, { withFileTypes: true })
    const nodes: FileNode[] = []

    for (const entry of entries) {
      const fullPath = join(dirPath, entry.name)
      const relativePath = relative(this.workspacePath, fullPath)

      if (this.shouldIgnore(entry.name)) continue

      if (entry.isDirectory()) {
        const children = await this.buildFileTree(fullPath)
        if (children.length > 0) {
          nodes.push({
            path: relativePath,
            name: entry.name,
            type: 'directory',
            children
          })
        }
      } else if (this.isWatchedFile(entry.name)) {
        nodes.push({
          path: relativePath,
          name: entry.name,
          type: 'file',
          extension: extname(entry.name)
        })
      }
    }

    return nodes.sort((a, b) => {
      if (a.type !== b.type) return a.type === 'directory' ? -1 : 1
      return a.name.localeCompare(b.name)
    })
  }

  private shouldIgnore(name: string): boolean {
    return name.startsWith('.') || name === 'node_modules' || name === 'dist'
  }

  private isWatchedFile(name: string): boolean {
    const ext = extname(name).toLowerCase()
    return WATCHED_EXTENSIONS.includes(ext)
  }

  private handleAdd(filepath: string): void {
    const relativePath = relative(this.workspacePath, filepath)
    this.emit('file:add', relativePath)
  }

  private handleChange(filepath: string): void {
    const relativePath = relative(this.workspacePath, filepath)
    this.emit('file:write', relativePath)
  }

  private handleRemove(filepath: string): void {
    const relativePath = relative(this.workspacePath, filepath)
    this.emit('file:remove', relativePath)
  }
}
