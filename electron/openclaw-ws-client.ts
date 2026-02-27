import { EventEmitter } from 'events'
import WebSocket from 'ws'

/**
 * Connects to OpenClaw's local gateway WebSocket at ws://127.0.0.1:18789
 * to receive real-time tool execution events.
 *
 * Protocol v3: sends a "hello" handshake with caps: ["tool-events"],
 * then listens for agent.toolExecutionStart / agent.toolExecutionEnd events.
 */

export interface OpenClawToolEvent {
  timestamp: number
  tool: string
  action: string
  filepath?: string
  toolCallId: string
  isError?: boolean
}

const DEFAULT_PORT = 18789
const RECONNECT_INTERVAL = 15000 // 15 seconds between reconnect attempts

// Map OpenClaw tool names to action categories
const TOOL_ACTION_MAP: Record<string, string> = {
  read_file: 'read',
  write_file: 'write',
  edit_file: 'edit',
  search_files: 'search',
  list_files: 'search',
  run_command: 'execute',
  bash: 'execute',
  web_search: 'search',
  web_fetch: 'fetch',
  memory_read: 'read',
  memory_write: 'write',
  // Claude-style tool names (OpenClaw may use these too)
  Read: 'read',
  Write: 'write',
  Edit: 'edit',
  Grep: 'search',
  Glob: 'search',
  Bash: 'execute',
}

export class OpenClawWSClient extends EventEmitter {
  private ws: WebSocket | null = null
  private port: number
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null
  private running: boolean = false
  private workspacePath: string
  private requestId: number = 0

  constructor(workspacePath: string, port: number = DEFAULT_PORT) {
    super()
    this.workspacePath = workspacePath
    this.port = port
  }

  start(): void {
    this.running = true
    this.connect()
  }

  stop(): void {
    this.running = false
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer)
      this.reconnectTimer = null
    }
    if (this.ws) {
      this.ws.close()
      this.ws = null
    }
  }

  private connect(): void {
    if (!this.running) return

    const url = `ws://127.0.0.1:${this.port}`

    try {
      this.ws = new WebSocket(url)
    } catch {
      this.scheduleReconnect()
      return
    }

    this.ws.on('open', () => {
      console.log(`[prowl:openclaw-ws] connected to ${url}`)
      this.sendHello()
    })

    this.ws.on('message', (data: WebSocket.Data) => {
      try {
        const frame = JSON.parse(data.toString())
        this.handleFrame(frame)
      } catch {
        // Invalid JSON, skip
      }
    })

    this.ws.on('close', (code: number, reason: Buffer) => {
      const reasonStr = reason.toString() || 'no reason'
      console.log(`[prowl:openclaw-ws] disconnected code=${code} reason="${reasonStr}"`)
      this.ws = null
      this.scheduleReconnect()
    })

    this.ws.on('error', (err: Error) => {
      // Only log if it's not a connection-refused (server not running)
      if (!err.message?.includes('ECONNREFUSED')) {
        console.log(`[prowl:openclaw-ws] error: ${err.message}`)
      }
      this.ws?.close()
    })
  }

  private scheduleReconnect(): void {
    if (!this.running || this.reconnectTimer) return
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null
      if (this.running) this.connect()
    }, RECONNECT_INTERVAL)
  }

  private sendHello(): void {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return

    const hello = {
      type: 'req',
      id: `prowl-${++this.requestId}`,
      method: 'connect',
      params: {
        minProtocol: 3,
        maxProtocol: 3,
        client: {
          id: 'gateway-client',
          displayName: 'Prowl',
          version: '1.0.0',
          platform: process.platform,
          mode: 'ui',
        },
        caps: ['tool-events'],
      },
    }

    this.ws.send(JSON.stringify(hello))
  }

  private handleFrame(frame: any): void {
    // Log all frames for debugging
    console.log(`[prowl:openclaw-ws] frame: type=${frame.type}`, frame.error ? `error=${JSON.stringify(frame.error)}` : '')

    // Handle hello response
    if (frame.type === 'hello-ok') {
      console.log('[prowl:openclaw-ws] handshake complete, server:', frame.server?.version)
      return
    }

    // Handle error responses (server rejecting handshake)
    if (frame.type === 'res' && !frame.ok) {
      console.log(`[prowl:openclaw-ws] server error: ${frame.error?.code} — ${frame.error?.message}`)
      // Stop retrying for auth/pairing errors — user action needed
      if (frame.error?.code === 'NOT_PAIRED' || frame.error?.code === 'AUTH_REQUIRED') {
        console.log('[prowl:openclaw-ws] pairing required — stopping reconnect')
        this.running = false
      }
      return
    }

    // Handle event frames
    if (frame.type === 'event') {
      this.handleEvent(frame)
    }
  }

  private handleEvent(frame: any): void {
    const eventName = frame.event
    const payload = frame.payload

    if (!payload || payload.stream !== 'tool') return

    const data = payload.data
    if (!data) return

    if (eventName === 'agent.toolExecutionStart') {
      this.handleToolStart(data, payload.ts)
    } else if (eventName === 'agent.toolExecutionEnd') {
      this.handleToolEnd(data, payload.ts)
    }
  }

  private handleToolStart(data: any, timestamp: number): void {
    const toolName = data.toolName || ''
    const args = data.args || {}
    const filepath = this.extractFilePath(toolName, args)
    const action = TOOL_ACTION_MAP[toolName] || 'unknown'

    const event: OpenClawToolEvent = {
      timestamp: timestamp || Date.now(),
      tool: toolName,
      action,
      filepath: filepath ? this.makeRelative(filepath) : undefined,
      toolCallId: data.toolCallId || '',
    }

    this.emit('tool:call', event)
  }

  private handleToolEnd(data: any, timestamp: number): void {
    const toolName = data.toolName || ''
    const action = TOOL_ACTION_MAP[toolName] || 'unknown'

    const event: OpenClawToolEvent = {
      timestamp: timestamp || Date.now(),
      tool: toolName,
      action,
      toolCallId: data.toolCallId || '',
      isError: data.isError,
    }

    this.emit('tool:end', event)
  }

  private extractFilePath(toolName: string, args: Record<string, any>): string | undefined {
    // Common patterns for file path arguments
    const pathKeys = ['file_path', 'path', 'filepath', 'filename', 'target', 'source']
    for (const key of pathKeys) {
      if (args[key] && typeof args[key] === 'string') {
        return args[key]
      }
    }

    // For command/bash tools, try to extract file path from command string
    if (toolName === 'run_command' || toolName === 'bash' || toolName === 'Bash') {
      const cmd = args.command || args.cmd || ''
      const pathMatch = cmd.match(/(?:cat|head|tail|less|more|vim|nano|code|python|node|cargo|go)\s+["']?([^\s"'|;&]+)/i)
      if (pathMatch) return pathMatch[1]
    }

    return undefined
  }

  private makeRelative(filepath: string): string {
    if (filepath.startsWith(this.workspacePath)) {
      return filepath.slice(this.workspacePath.length).replace(/^\//, '')
    }
    return filepath
  }
}
