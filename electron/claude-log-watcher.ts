import { EventEmitter } from 'events'
import { homedir } from 'os'
import { join, basename } from 'path'
import { readdir, stat, readFile } from 'fs/promises'
import { watchFile, unwatchFile, existsSync } from 'fs'

/**
 * Watches Claude Code JSONL session logs for tool call events.
 *
 * Claude Code stores transcripts at:
 *   ~/.claude/projects/{slug}/{session-id}.jsonl
 *
 * where {slug} is the absolute workspace path with / replaced by -
 * e.g. /Users/foo/myproject -> -Users-foo-myproject
 *
 * Each JSONL line is a message. Tool calls appear as:
 *   {"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"..."}}]}}
 */

export interface ClaudeToolEvent {
  timestamp: number
  tool: string        // Read, Write, Edit, Grep, Glob, Bash, etc.
  action: string      // read, write, edit, search, execute
  filepath?: string   // resolved file path (if applicable)
}

// Map Claude tool names to action categories
const TOOL_ACTION_MAP: Record<string, string> = {
  Read: 'read',
  Edit: 'edit',
  Write: 'write',
  Grep: 'search',
  Glob: 'search',
  Bash: 'execute',
  WebFetch: 'fetch',
  WebSearch: 'search',
  Task: 'delegate',
}

export class ClaudeLogWatcher extends EventEmitter {
  private workspacePath: string
  private projectSlug: string
  private claudeProjectDir: string
  private activeFile: string | null = null
  private filePosition: number = 0
  private watching: boolean = false
  private pollInterval: ReturnType<typeof setInterval> | null = null

  constructor(workspacePath: string) {
    super()
    this.workspacePath = workspacePath
    // Claude Code slugifies the absolute path: / -> -
    this.projectSlug = workspacePath.replace(/\//g, '-')
    this.claudeProjectDir = join(homedir(), '.claude', 'projects', this.projectSlug)
  }

  async start(): Promise<void> {
    this.watching = true

    // Find the most recent JSONL file
    await this.findActiveSession()

    if (this.activeFile) {
      this.tailFile()
    }

    // Re-check for new sessions every 10 seconds
    this.pollInterval = setInterval(() => {
      if (this.watching) this.findActiveSession()
    }, 10000)
  }

  stop(): void {
    this.watching = false
    if (this.activeFile) {
      unwatchFile(this.activeFile)
    }
    if (this.pollInterval) {
      clearInterval(this.pollInterval)
      this.pollInterval = null
    }
    this.activeFile = null
  }

  private async findActiveSession(): Promise<void> {
    if (!existsSync(this.claudeProjectDir)) return

    try {
      const entries = await readdir(this.claudeProjectDir)
      const jsonlFiles = entries.filter(e => e.endsWith('.jsonl'))

      if (jsonlFiles.length === 0) return

      // Find most recently modified JSONL
      let newest: { file: string; mtime: number } | null = null
      for (const file of jsonlFiles) {
        const fullPath = join(this.claudeProjectDir, file)
        const s = await stat(fullPath)
        if (!newest || s.mtimeMs > newest.mtime) {
          newest = { file: fullPath, mtime: s.mtimeMs }
        }
      }

      if (newest && newest.file !== this.activeFile) {
        // Switch to newer session file
        if (this.activeFile) {
          unwatchFile(this.activeFile)
        }
        this.activeFile = newest.file
        // Start reading from end (only new entries)
        const s = await stat(this.activeFile)
        this.filePosition = s.size
        this.tailFile()
      }
    } catch {
      // Claude projects dir might not exist yet
    }
  }

  private tailFile(): void {
    if (!this.activeFile) return

    watchFile(this.activeFile, { interval: 300 }, async (curr, prev) => {
      if (!this.watching || !this.activeFile) return
      if (curr.size > this.filePosition) {
        await this.readNewLines(this.filePosition, curr.size)
        this.filePosition = curr.size
      }
    })
  }

  private async readNewLines(start: number, end: number): Promise<void> {
    if (!this.activeFile) return

    try {
      const buf = Buffer.alloc(end - start)
      const { createReadStream } = await import('fs')
      const stream = createReadStream(this.activeFile, {
        start,
        end: end - 1,
        encoding: 'utf-8',
      })

      let data = ''
      for await (const chunk of stream) {
        data += chunk
      }

      const lines = data.split('\n').filter(Boolean)
      for (const line of lines) {
        this.parseLine(line)
      }
    } catch {
      // File might be mid-write
    }
  }

  private parseLine(line: string): void {
    try {
      const entry = JSON.parse(line)

      // Look for assistant messages with tool_use content
      if (entry.type === 'assistant' && entry.message?.content) {
        const contents = Array.isArray(entry.message.content)
          ? entry.message.content
          : [entry.message.content]

        for (const block of contents) {
          if (block.type === 'tool_use') {
            this.handleToolUse(block.name, block.input)
          }
        }
      }
    } catch {
      // Invalid JSON line, skip
    }
  }

  private handleToolUse(toolName: string, input: Record<string, any>): void {
    const action = TOOL_ACTION_MAP[toolName] || 'unknown'
    let filepath: string | undefined

    // Extract file path based on tool type
    switch (toolName) {
      case 'Read':
      case 'Edit':
      case 'Write':
        filepath = input?.file_path
        break
      case 'Grep':
      case 'Glob':
        filepath = input?.path
        break
      case 'Bash': {
        // Try to extract file paths from bash commands
        const cmd = input?.command || ''
        const pathMatch = cmd.match(/(?:cat|head|tail|less|more|vim|nano|code)\s+["']?([^\s"'|;&]+)/i)
        if (pathMatch) filepath = pathMatch[1]
        break
      }
    }

    // Make path relative to workspace if it's absolute
    if (filepath && filepath.startsWith(this.workspacePath)) {
      filepath = filepath.slice(this.workspacePath.length).replace(/^\//, '')
    }

    const event: ClaudeToolEvent = {
      timestamp: Date.now(),
      tool: toolName,
      action,
      filepath,
    }

    this.emit('tool:call', event)
  }
}
