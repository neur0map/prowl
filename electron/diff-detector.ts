import { exec } from 'child_process'
import { promisify } from 'util'
import { readdir, stat, readFile } from 'fs/promises'
import { join, relative } from 'path'
import { createHash } from 'crypto'

const execAsync = promisify(exec)

export interface FileManifest {
  files: Record<string, { hash: string; mtime: number }>
}

export interface DiffResult {
  added: string[]
  modified: string[]
  deleted: string[]
  isGitRepo: boolean
}

// Ignore rules (same as main.ts scanner)
const IGNORED_DIRS = new Set([
  '.git', '.svn', '.hg', 'node_modules', 'bower_components', 'vendor',
  'venv', '.venv', '__pycache__', '.pytest_cache', '.mypy_cache',
  'dist', 'build', 'out', 'target', '.next', '.nuxt', '.vercel',
  'coverage', '.nyc_output', 'logs', 'tmp', 'temp', 'cache', '.cache',
  '.idea', '.vscode', '.vs', '.DS_Store', '.prowl',
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

function shouldIgnore(name: string): boolean {
  const lower = name.toLowerCase()
  const dot = lower.lastIndexOf('.')
  if (dot !== -1 && IGNORED_EXTENSIONS.has(lower.substring(dot))) return true
  return false
}

async function hashFile(filePath: string): Promise<string> {
  const content = await readFile(filePath, 'utf-8')
  return createHash('sha256').update(content).digest('hex')
}

async function walkDirectory(dirPath: string, basePath: string): Promise<Map<string, number>> {
  const result = new Map<string, number>()

  async function walk(dir: string) {
    let entries
    try {
      entries = await readdir(dir, { withFileTypes: true })
    } catch {
      return
    }
    for (const entry of entries) {
      const fullPath = join(dir, entry.name)
      if (entry.isDirectory()) {
        if (IGNORED_DIRS.has(entry.name) || entry.name.startsWith('.')) continue
        await walk(fullPath)
      } else {
        if (shouldIgnore(entry.name)) continue
        try {
          const s = await stat(fullPath)
          const relPath = relative(basePath, fullPath)
          result.set(relPath, s.mtimeMs)
        } catch { /* skip */ }
      }
    }
  }

  await walk(dirPath)
  return result
}

/**
 * Detect changes between the current project state and the snapshot manifest.
 *
 * Uses git diff if available (fast), falls back to mtime + hash comparison.
 */
export async function detectChanges(
  projectPath: string,
  snapshotGitCommit: string | null,
  manifest: FileManifest
): Promise<DiffResult> {
  const result: DiffResult = { added: [], modified: [], deleted: [], isGitRepo: false }

  // Try git-based detection first
  if (snapshotGitCommit) {
    try {
      // Check if it's a git repo
      await execAsync('git rev-parse --git-dir', { cwd: projectPath })
      result.isGitRepo = true

      // Get changes since snapshot commit
      const { stdout: diffOutput } = await execAsync(
        `git diff --name-status ${snapshotGitCommit}..HEAD`,
        { cwd: projectPath, maxBuffer: 10 * 1024 * 1024 }
      )

      for (const line of diffOutput.trim().split('\n').filter(Boolean)) {
        const [status, ...pathParts] = line.split('\t')
        const filePath = pathParts[0]
        if (!filePath || shouldIgnore(filePath.split('/').pop() || '')) continue

        switch (status?.[0]) {
          case 'A': result.added.push(filePath); break
          case 'M': result.modified.push(filePath); break
          case 'D': result.deleted.push(filePath); break
          case 'R': // Rename = delete old + add new
            result.deleted.push(filePath)  // pathParts[0] = old path
            if (pathParts[1]) result.added.push(pathParts[1])  // pathParts[1] = new path
            break
        }
      }

      // Also check untracked files
      const { stdout: untrackedOutput } = await execAsync(
        'git ls-files --others --exclude-standard',
        { cwd: projectPath, maxBuffer: 10 * 1024 * 1024 }
      )
      for (const filePath of untrackedOutput.trim().split('\n').filter(Boolean)) {
        if (!shouldIgnore(filePath.split('/').pop() || '')) {
          result.added.push(filePath)
        }
      }

      return result
    } catch {
      // Git failed — fall through to mtime-based detection
    }
  }

  // Fallback: mtime-based detection
  const currentFiles = await walkDirectory(projectPath, projectPath)
  const manifestFiles = new Set(Object.keys(manifest.files))

  for (const [filePath, mtime] of currentFiles) {
    if (!manifestFiles.has(filePath)) {
      result.added.push(filePath)
    } else {
      const entry = manifest.files[filePath]
      if (Math.abs(mtime - entry.mtime) > 1000) {
        // mtime changed — verify with hash
        try {
          const currentHash = await hashFile(join(projectPath, filePath))
          if (currentHash !== entry.hash) {
            result.modified.push(filePath)
          }
        } catch {
          result.modified.push(filePath)
        }
      }
      manifestFiles.delete(filePath)
    }
  }

  // Remaining manifest files are deleted
  for (const filePath of manifestFiles) {
    result.deleted.push(filePath)
  }

  return result
}
