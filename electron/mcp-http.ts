/**
 * Internal HTTP API server for the MCP bridge.
 *
 * Runs on 127.0.0.1 with an OS-assigned port and writes connection
 * details to ~/.prowl/mcp-port and ~/.prowl/mcp-auth so the
 * standalone MCP server (and other local tools) can connect.
 *
 * All routes are POST to /api/<tool-name>, expecting JSON body.
 * Every request requires Authorization: Bearer <token>.
 *
 * Architecture: HTTP handler → queue → setInterval dispatcher →
 * webContents.send('mcp:request') → preload ipcRenderer.on → contextBridge
 * callback into renderer → renderer processes → sendResult via contextBridge
 * → ipcRenderer.send('mcp:response') → ipcMain.on → HTTP response.
 */

import { createServer, type IncomingMessage, type ServerResponse, type Server } from 'http'
import { randomUUID } from 'crypto'
import { writeFile, unlink, mkdir, chmod } from 'fs/promises'
import { join } from 'path'
import { homedir } from 'os'
import { ipcMain } from 'electron'
import type { BrowserWindow } from 'electron'

const PROWL_DIR = join(homedir(), '.prowl')
const PORT_FILE = join(PROWL_DIR, 'mcp-port')
const AUTH_FILE = join(PROWL_DIR, 'mcp-auth')

type McpToolName = string

const REQUEST_TIMEOUT_MS = 120_000 // 2 minutes for ask/investigate
const DISPATCH_INTERVAL_MS = 50

const VALID_TOOLS = new Set<McpToolName>([
  'search', 'cypher', 'grep', 'read-file', 'overview', 'explore',
  'impact', 'get-context', 'get-hotspots', 'chat-history', 'ask',
  'investigate', 'status',
])

interface QueuedRequest {
  requestId: string
  toolName: McpToolName
  params: unknown
}

export async function startMcpHttpServer(
  getMainWindow: () => BrowserWindow | null,
): Promise<{ port: number; token: string; close: () => Promise<void> }> {
  const token = randomUUID()

  const pendingRequests = new Map<string, {
    resolve: (value: unknown) => void
    reject: (err: Error) => void
    timer: ReturnType<typeof setTimeout>
  }>()

  const requestQueue: QueuedRequest[] = []

  // Listen for responses from the renderer via preload
  ipcMain.on('mcp:response', (_event, requestId: string, response: unknown) => {
    const pending = pendingRequests.get(requestId)
    if (pending) {
      clearTimeout(pending.timer)
      pendingRequests.delete(requestId)
      pending.resolve(response)
    }
  })

  // Dispatch queued requests to the preload via IPC.
  // Uses setInterval to decouple from HTTP handler context.
  const dispatchTimer = setInterval(() => {
    if (requestQueue.length === 0) return

    const win = getMainWindow()
    if (!win || win.isDestroyed()) return

    while (requestQueue.length > 0) {
      const req = requestQueue.shift()!
      console.log(`[prowl:mcp] Dispatching ${req.requestId}: ${req.toolName}`)
      win.webContents.send('mcp:request', {
        requestId: req.requestId,
        toolName: req.toolName,
        params: req.params,
      })
    }
  }, DISPATCH_INTERVAL_MS)

  function callRenderer(toolName: McpToolName, params: unknown): Promise<unknown> {
    const requestId = randomUUID()
    const timeoutMs = toolName === 'ask' || toolName === 'investigate' ? REQUEST_TIMEOUT_MS : 30_000

    return new Promise<unknown>((resolve, reject) => {
      const timer = setTimeout(() => {
        pendingRequests.delete(requestId)
        reject(new Error(`Renderer call timed out after ${timeoutMs}ms`))
      }, timeoutMs)

      pendingRequests.set(requestId, { resolve, reject, timer })

      requestQueue.push({
        requestId,
        toolName,
        params: params ?? {},
      })
    })
  }

  const server: Server = createServer((req: IncomingMessage, res: ServerResponse) => {
    res.setHeader('Content-Type', 'application/json')
    res.setHeader('Access-Control-Allow-Origin', '127.0.0.1')

    if (req.method === 'OPTIONS') {
      res.setHeader('Access-Control-Allow-Methods', 'POST, OPTIONS')
      res.setHeader('Access-Control-Allow-Headers', 'Content-Type, Authorization')
      res.writeHead(204)
      res.end()
      return
    }

    if (req.method !== 'POST') {
      res.writeHead(405)
      res.end(JSON.stringify({ error: 'Method not allowed' }))
      return
    }

    const authHeader = req.headers.authorization
    if (!authHeader || authHeader !== `Bearer ${token}`) {
      res.writeHead(401)
      res.end(JSON.stringify({ error: 'Unauthorized' }))
      return
    }

    const url = new URL(req.url || '/', `http://${req.headers.host}`)
    const pathParts = url.pathname.split('/').filter(Boolean)

    if (pathParts.length !== 2 || pathParts[0] !== 'api') {
      res.writeHead(404)
      res.end(JSON.stringify({ error: 'Not found. Use POST /api/<tool-name>' }))
      return
    }

    const toolName = pathParts[1] as McpToolName
    if (!VALID_TOOLS.has(toolName)) {
      res.writeHead(404)
      res.end(JSON.stringify({ error: `Unknown tool: ${toolName}` }))
      return
    }

    const chunks: Buffer[] = []
    req.on('data', (chunk: Buffer) => chunks.push(chunk))
    req.on('end', () => {
      let body: unknown = {}
      const raw = Buffer.concat(chunks).toString('utf-8')
      if (raw.length > 0) {
        try {
          body = JSON.parse(raw)
        } catch {
          res.writeHead(400)
          res.end(JSON.stringify({ error: 'Invalid JSON body' }))
          return
        }
      }

      callRenderer(toolName, body)
        .then((result: unknown) => {
          const r = result as { success?: boolean; data?: unknown; error?: string } | null
          if (r && r.success) {
            res.writeHead(200)
            res.end(JSON.stringify({ success: true, data: r.data }))
          } else {
            res.writeHead(500)
            res.end(JSON.stringify({ success: false, error: r?.error || 'Unknown error from renderer' }))
          }
        })
        .catch((err: unknown) => {
          const message = err instanceof Error ? err.message : String(err)
          res.writeHead(500)
          res.end(JSON.stringify({ success: false, error: message }))
        })
    })
  })

  await new Promise<void>((resolve, reject) => {
    server.listen(0, '127.0.0.1', () => resolve())
    server.on('error', reject)
  })

  const address = server.address()
  const port = typeof address === 'object' && address ? address.port : 0

  await mkdir(PROWL_DIR, { recursive: true })
  await writeFile(PORT_FILE, String(port), 'utf-8')
  await writeFile(AUTH_FILE, token, 'utf-8')
  await chmod(AUTH_FILE, 0o600)

  console.log(`[prowl:mcp] HTTP API listening on 127.0.0.1:${port}`)

  async function close(): Promise<void> {
    clearInterval(dispatchTimer)
    ipcMain.removeAllListeners('mcp:response')

    for (const [, pending] of pendingRequests) {
      clearTimeout(pending.timer)
      pending.reject(new Error('Server shutting down'))
    }
    pendingRequests.clear()

    await new Promise<void>((resolve) => {
      server.close(() => resolve())
    })

    await unlink(PORT_FILE).catch(() => {})
    await unlink(AUTH_FILE).catch(() => {})
    console.log('[prowl:mcp] HTTP API stopped')
  }

  return { port, token, close }
}
