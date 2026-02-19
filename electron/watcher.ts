import { EventEmitter } from 'events'
import { watch, FSWatcher } from 'chokidar'
import { readdir, stat } from 'fs/promises'
import { join, relative, extname, basename } from 'path'

export interface FileNode {
  path: string
  name: string
  type: 'file' | 'directory'
  extension?: string
  children?: FileNode[]
}

// Directories to ignore WITHIN the workspace (checked against relative paths only)
const IGNORED_DIR_NAMES = new Set([
  'node_modules', '.git', 'dist', 'build', 'out', 'target',
  '.next', '.nuxt', '__pycache__', '.pytest_cache', 'coverage',
  '.cache', 'tmp', 'temp',
])

// File patterns to ignore
const IGNORED_EXTENSIONS = new Set([
  '.log', '.lock', '.map',
  '.png', '.jpg', '.jpeg', '.gif', '.svg', '.ico', '.webp',
  '.woff', '.woff2', '.ttf', '.eot',
  '.zip', '.tar', '.gz', '.rar',
  '.exe', '.dll', '.so', '.dylib', '.wasm',
  '.pdf', '.mp4', '.mp3',
])

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

    // Use a function for `ignored` so we check the RELATIVE path from workspace root.
    // This prevents false positives when the workspace itself lives inside node_modules
    // (e.g. /opt/homebrew/lib/node_modules/openclaw/).
    const workspaceRoot = this.workspacePath

    this.watcher = watch(workspaceRoot, {
      ignored: (absolutePath: string) => {
        const rel = relative(workspaceRoot, absolutePath)
        if (!rel || rel === '.') return false

        // Check each path segment against ignored directory names
        const segments = rel.split('/')
        for (const seg of segments.slice(0, -1)) {
          if (IGNORED_DIR_NAMES.has(seg)) return true
        }

        // Check file extension
        const ext = extname(absolutePath).toLowerCase()
        if (IGNORED_EXTENSIONS.has(ext)) return true

        // Ignore dotfiles/dotdirs (but not the workspace root itself)
        const name = basename(absolutePath)
        if (name.startsWith('.') && name !== '.') return true

        return false
      },
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
      } else {
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
    return name.startsWith('.') || IGNORED_DIR_NAMES.has(name)
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
