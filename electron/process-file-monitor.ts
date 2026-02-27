import { EventEmitter } from 'events'
import { execFile } from 'child_process'
import { relative, isAbsolute } from 'path'

/**
 * Polls `lsof` to detect which files in the workspace are currently open
 * by any process. Diffs snapshots to detect new file accesses.
 *
 * No sudo needed — lsof can see the current user's processes.
 * Catches file reads, writes, memory-mapped files, and open handles
 * that chokidar would miss (chokidar only sees writes).
 */

export interface FileAccessEvent {
  timestamp: number
  filepath: string     // relative to workspace
  pid: number
  process: string
  type: 'access'
}

const POLL_INTERVAL = 2000 // 2 seconds
const LSOF_TIMEOUT = 5000  // 5 second timeout for lsof

// File extensions to track (source code + config + docs)
const TRACKED_EXTENSIONS = new Set([
  '.ts', '.tsx', '.js', '.jsx', '.mjs', '.cjs',
  '.py', '.pyi',
  '.rs', '.go', '.java', '.c', '.cpp', '.h', '.hpp', '.cs',
  '.rb', '.php', '.swift', '.kt', '.scala',
  '.json', '.yaml', '.yml', '.toml', '.xml',
  '.md', '.txt', '.rst', '.adoc',
  '.css', '.scss', '.less', '.html', '.vue', '.svelte',
  '.sql', '.graphql', '.proto',
  '.sh', '.bash', '.zsh', '.fish',
  '.env', '.conf', '.cfg', '.ini',
  '.dockerfile', '.makefile',
])

export class ProcessFileMonitor extends EventEmitter {
  private workspacePath: string
  private previousFiles: Set<string> = new Set()
  private timer: ReturnType<typeof setInterval> | null = null
  private running: boolean = false

  constructor(workspacePath: string) {
    super()
    this.workspacePath = workspacePath
  }

  start(): void {
    this.running = true
    // Initial snapshot (don't emit events for already-open files)
    this.poll(true)
    this.timer = setInterval(() => {
      if (this.running) this.poll(false)
    }, POLL_INTERVAL)
  }

  stop(): void {
    this.running = false
    if (this.timer) {
      clearInterval(this.timer)
      this.timer = null
    }
    this.previousFiles.clear()
  }

  private poll(isInitial: boolean): void {
    // Use lsof to find all files open under the workspace path
    // -Fn: output filenames only (prefixed with 'n')
    // -c: filter by process command (not used — we want ALL processes)
    // +D: recursive directory scan (can be slow on huge dirs)
    //
    // Instead, we run a general lsof and grep for workspace path.
    // This is faster than +D for large directories.
    execFile(
      '/usr/bin/lsof',
      ['-Fpn'],
      { timeout: LSOF_TIMEOUT, maxBuffer: 10 * 1024 * 1024 },
      (error, stdout) => {
        if (!this.running) return
        if (error) return // lsof exits non-zero sometimes, ignore

        const currentFiles = new Map<string, { pid: number; process: string }>()
        let currentPid = 0
        let currentProcess = ''

        // Parse lsof -Fpn output:
        // p<pid>\n
        // c<command>\n (not available with -Fp, but -Fpn gives p and n)
        // n<filename>\n
        for (const line of stdout.split('\n')) {
          if (line.startsWith('p')) {
            currentPid = parseInt(line.slice(1), 10)
            currentProcess = ''
          } else if (line.startsWith('c')) {
            currentProcess = line.slice(1)
          } else if (line.startsWith('n')) {
            const filepath = line.slice(1)
            if (filepath.startsWith(this.workspacePath + '/')) {
              const rel = relative(this.workspacePath, filepath)
              // Only track source/config files
              const ext = '.' + rel.split('.').pop()?.toLowerCase()
              if (TRACKED_EXTENSIONS.has(ext) || !rel.includes('.')) {
                currentFiles.set(rel, { pid: currentPid, process: currentProcess })
              }
            }
          }
        }

        if (!isInitial) {
          // Emit events for newly accessed files
          for (const [rel, info] of currentFiles) {
            if (!this.previousFiles.has(rel)) {
              this.emit('file:access', {
                timestamp: Date.now(),
                filepath: rel,
                pid: info.pid,
                process: info.process,
                type: 'access',
              } as FileAccessEvent)
            }
          }
        }

        this.previousFiles = new Set(currentFiles.keys())
      }
    )
  }
}
