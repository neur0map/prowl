import { EventEmitter } from 'events'
import { createReadStream, statSync, watchFile, unwatchFile } from 'fs'
import { createInterface } from 'readline'

export interface ToolCall {
  timestamp: number
  tool: string
  action?: string
  filepath?: string
  duration?: number
}

const TOOL_PATTERNS: Record<string, RegExp> = {
  read: /Read.*?file[=:]?\s*["']?([^"'\s]+)/i,
  write: /Write.*?file[=:]?\s*["']?([^"'\s]+)/i,
  exec: /exec.*?command[=:]?\s*["']?([^"'\n]+)/i,
  edit: /Edit.*?file[=:]?\s*["']?([^"'\s]+)/i
}

export class LogParser extends EventEmitter {
  private logPath: string
  private position: number = 0
  private watching: boolean = false

  constructor(logPath: string) {
    super()
    this.logPath = logPath
  }

  start(): void {
    try {
      const stats = statSync(this.logPath)
      this.position = stats.size
    } catch {
      this.position = 0
    }

    this.watching = true
    this.watchLog()
  }

  stop(): void {
    this.watching = false
    unwatchFile(this.logPath)
  }

  private watchLog(): void {
    watchFile(this.logPath, { interval: 100 }, (curr, prev) => {
      if (!this.watching) return
      if (curr.size > prev.size) {
        this.readNewLines(prev.size, curr.size)
      } else if (curr.size < prev.size) {
        this.position = 0
        this.readNewLines(0, curr.size)
      }
    })
  }

  private readNewLines(start: number, end: number): void {
    const stream = createReadStream(this.logPath, {
      start,
      end: end - 1,
      encoding: 'utf-8'
    })

    const rl = createInterface({ input: stream })

    rl.on('line', (line) => {
      this.parseLine(line)
    })

    rl.on('close', () => {
      this.position = end
    })
  }

  private parseLine(line: string): void {
    for (const [tool, pattern] of Object.entries(TOOL_PATTERNS)) {
      const match = line.match(pattern)
      if (match) {
        const toolCall: ToolCall = {
          timestamp: Date.now(),
          tool,
          filepath: match[1]
        }
        this.emit('tool:call', toolCall)
        return
      }
    }

    if (line.includes('function_calls') || line.includes('invoke name=')) {
      const toolMatch = line.match(/invoke name="?(\w+)"?/i)
      if (toolMatch) {
        this.emit('tool:call', {
          timestamp: Date.now(),
          tool: toolMatch[1]
        })
      }
    }
  }
}
