#!/usr/bin/env node
/**
 * Standalone MCP server for Prowl.
 *
 * Speaks the Model Context Protocol over stdio and bridges every
 * tool call to the Prowl Electron app's internal HTTP API running
 * on localhost.
 *
 * Usage:
 *   node mcp-server.js
 *
 * Expects Prowl to be running so that ~/.prowl/mcp-port and
 * ~/.prowl/mcp-auth exist.
 */

import { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js'
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js'
import { z } from 'zod'
import { readFile } from 'fs/promises'
import { join } from 'path'
import { homedir } from 'os'
import http from 'http'

/* ── Config ────────────────────────────────────────────── */

const PROWL_DIR = join(homedir(), '.prowl')
const PORT_FILE = join(PROWL_DIR, 'mcp-port')
const AUTH_FILE = join(PROWL_DIR, 'mcp-auth')

let prowlPort: number | null = null
let prowlToken: string | null = null

async function loadConnectionInfo(): Promise<void> {
  try {
    prowlPort = parseInt(await readFile(PORT_FILE, 'utf-8'), 10)
    prowlToken = (await readFile(AUTH_FILE, 'utf-8')).trim()
  } catch {
    prowlPort = null
    prowlToken = null
  }
}

/* ── HTTP client to Prowl API ─────────────────────────── */

function doHttpCall(toolName: string, body: string, port: number, token: string): Promise<unknown> {
  return new Promise((resolve, reject) => {
    const req = http.request(
      {
        hostname: '127.0.0.1',
        port,
        path: `/api/${toolName}`,
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'Authorization': `Bearer ${token}`,
          'Content-Length': Buffer.byteLength(body),
        },
        timeout: 120_000,
      },
      (res) => {
        const chunks: Buffer[] = []
        res.on('data', (chunk: Buffer) => chunks.push(chunk))
        res.on('end', () => {
          try {
            const raw = Buffer.concat(chunks).toString('utf-8')
            const parsed = JSON.parse(raw)
            if (parsed.success) {
              resolve(parsed.data)
            } else {
              reject(new Error(parsed.error || 'Unknown error from Prowl'))
            }
          } catch (e) {
            reject(new Error(`Failed to parse Prowl response: ${e}`))
          }
        })
      },
    )

    req.on('error', (err) => {
      reject(err)
    })

    req.on('timeout', () => {
      req.destroy()
      reject(new Error('Request to Prowl timed out'))
    })

    req.write(body)
    req.end()
  })
}

async function callProwlApi(toolName: string, params: unknown): Promise<unknown> {
  if (!prowlPort || !prowlToken) {
    await loadConnectionInfo()
  }

  if (!prowlPort || !prowlToken) {
    throw new Error(
      'Prowl is not running. Start Prowl and load a project, then try again. ' +
      '(Missing ~/.prowl/mcp-port or ~/.prowl/mcp-auth)'
    )
  }

  const body = JSON.stringify(params || {})

  try {
    return await doHttpCall(toolName, body, prowlPort, prowlToken)
  } catch (err) {
    // On connection refused, re-read port/auth (Prowl may have restarted) and retry once
    if ((err as NodeJS.ErrnoException).code === 'ECONNREFUSED') {
      const oldPort = prowlPort
      prowlPort = null
      prowlToken = null
      await loadConnectionInfo()

      if (prowlPort && prowlToken && prowlPort !== oldPort) {
        // Port changed — Prowl was restarted, retry with new connection info
        return await doHttpCall(toolName, body, prowlPort, prowlToken)
      }

      throw new Error(
        'Cannot connect to Prowl. Make sure Prowl is running with a project loaded.'
      )
    }
    throw err
  }
}

/* ── Helper to format result as MCP text content ──────── */

function textResult(data: unknown): { content: Array<{ type: 'text'; text: string }> } {
  const text = typeof data === 'string' ? data : JSON.stringify(data, null, 2)
  return { content: [{ type: 'text' as const, text }] }
}

/* ── MCP Server ────────────────────────────────────────── */

const server = new McpServer({
  name: 'prowl',
  version: '1.0.0',
})

// Tool: prowl_status — health check
server.tool(
  'prowl_status',
  'Check if Prowl is running and has a project loaded.',
  {},
  async () => {
    const data = await callProwlApi('status', {})
    return textResult(data)
  },
)

// Tool: prowl_search — hybrid keyword + semantic search
server.tool(
  'prowl_search',
  'Search code by keyword and semantics. Returns symbols, files, clusters, and process context.',
  {
    query: z.string().describe('The concept or keyword to search for'),
    limit: z.number().optional().describe('Max results (default: 10)'),
    useReranker: z.boolean().optional().describe('Re-rank results with cross-encoder for higher precision (default: true if model loaded)'),
  },
  async (params) => {
    const data = await callProwlApi('search', params)
    return textResult(data)
  },
)

// Tool: prowl_cypher — direct graph query
server.tool(
  'prowl_cypher',
  `Run a Cypher query against the knowledge graph.

Node tables: File, Folder, Function, Class, Interface, Method, CodeElement, Const, Struct, Enum
Edge table: CodeEdge (single table — filter by type property)

IMPORTANT SYNTAX:
  CORRECT: MATCH (a)-[e:CodeEdge]->(b) WHERE e.type = 'IMPORTS'
  WRONG:   MATCH (a)-[:IMPORTS]->(b)  -- IMPORTS is not a table name

Edge types (e.type): CONTAINS, DEFINES, IMPORTS, CALLS, EXTENDS, IMPLEMENTS
Node props: name, filePath, startLine, endLine, content, isExported
Edge props: type, confidence, reason

RULES: Never return whole nodes — project properties (RETURN f.name, f.filePath).
WITH + ORDER BY must include LIMIT. Use label(n) to get node type.

Examples:
  Files importing X: MATCH (f:File)-[e:CodeEdge]->(t:File) WHERE t.filePath CONTAINS 'X' AND e.type = 'IMPORTS' RETURN f.filePath
  Functions in file: MATCH (n) WHERE n.filePath = 'src/foo.ts' AND label(n) <> 'File' RETURN n.name, label(n)`,
  {
    cypher: z.string().describe('Cypher statement to execute'),
    query: z.string().optional().describe('Natural-language query (needed when cypher uses {{QUERY_VECTOR}})'),
  },
  async (params) => {
    const data = await callProwlApi('cypher', params)
    return textResult(data)
  },
)

// Tool: prowl_grep — regex search across indexed files
server.tool(
  'prowl_grep',
  'Regex search across all indexed source files. Best for exact strings, TODOs, error messages.',
  {
    pattern: z.string().describe('Regex to match'),
    fileFilter: z.string().optional().describe('Restrict to files whose path contains this substring'),
    caseSensitive: z.boolean().optional().describe('Match case exactly (default: false)'),
    maxResults: z.number().optional().describe('Result cap (default: 100)'),
  },
  async (params) => {
    const data = await callProwlApi('grep', params)
    return textResult(data)
  },
)

// Tool: prowl_read_file — read full source
server.tool(
  'prowl_read_file',
  'Read the full source code of a file from the indexed project. Supports fuzzy path matching.',
  {
    filePath: z.string().describe('File path (partial paths resolved automatically)'),
  },
  async (params) => {
    const data = await callProwlApi('read-file', params)
    return textResult(data)
  },
)

// Tool: prowl_summary — alias for overview (most LLMs try this name first)
server.tool(
  'prowl_summary',
  'Get a high-level summary of the codebase: clusters, processes, cross-cluster dependencies. Same as prowl_overview.',
  {},
  async () => {
    const data = await callProwlApi('overview', {})
    return textResult(data)
  },
)

// Tool: prowl_overview — high-level codebase map
server.tool(
  'prowl_overview',
  'Get a high-level map of the codebase: clusters, processes, cross-cluster dependencies.',
  {},
  async () => {
    const data = await callProwlApi('overview', {})
    return textResult(data)
  },
)

// Tool: prowl_explore — drill into a symbol, cluster, or process
server.tool(
  'prowl_explore',
  'Drill into a specific symbol, cluster, or process. Returns members, process participation, and connections.',
  {
    target: z.string().describe('Name or ID of the symbol, cluster, or process'),
    type: z.enum(['symbol', 'cluster', 'process']).optional().describe('Target kind hint (auto-detected when omitted)'),
  },
  async (params) => {
    const data = await callProwlApi('explore', params)
    return textResult(data)
  },
)

// Tool: prowl_impact — change-impact analysis
server.tool(
  'prowl_impact',
  `Change-impact analysis (blast radius) for a function, class, or file.
Direction: upstream = what CALLS/IMPORTS this (breakage risk); downstream = what this depends on.`,
  {
    target: z.string().describe('Function, class, or file to analyze'),
    direction: z.enum(['upstream', 'downstream']).describe('upstream = consumers; downstream = dependencies'),
    maxDepth: z.number().optional().describe('Traversal depth (default: 3, max: 10)'),
    relationTypes: z.array(z.string()).optional().describe('Edge types: CALLS, IMPORTS, EXTENDS, IMPLEMENTS'),
    includeTests: z.boolean().optional().describe('Include test files (default: false)'),
    minConfidence: z.number().optional().describe('Confidence floor 0-1 (default: 0.7)'),
  },
  async (params) => {
    const data = await callProwlApi('impact', params)
    return textResult(data)
  },
)

// Tool: prowl_context — project-level context (stats, hotspots, tree)
server.tool(
  'prowl_context',
  'Get project-level context: file counts, symbol stats, hotspots, and directory tree.',
  {
    projectName: z.string().optional().describe('Project name (auto-detected if omitted)'),
  },
  async (params) => {
    const data = await callProwlApi('get-context', params)
    return textResult(data)
  },
)

// Tool: prowl_hotspots — most connected symbols
server.tool(
  'prowl_hotspots',
  'Get the most connected symbols (hotspots) in the codebase.',
  {
    limit: z.number().optional().describe('Number of hotspots to return (default: 10)'),
  },
  async (params) => {
    const data = await callProwlApi('get-hotspots', params)
    return textResult(data)
  },
)

// Tool: prowl_ask — one-shot agent query
server.tool(
  'prowl_ask',
  'Ask Prowl\'s AI agent a question about the codebase. The agent has access to all analysis tools internally.',
  {
    question: z.string().describe('Question about the codebase'),
  },
  async (params) => {
    const data = await callProwlApi('ask', params)
    return textResult(data)
  },
)

// Tool: prowl_investigate — multi-step agent investigation
server.tool(
  'prowl_investigate',
  'Run a thorough, multi-step investigation. The agent uses multiple tools to research a task systematically.',
  {
    task: z.string().describe('Investigation task description'),
    depth: z.number().optional().describe('Max investigation steps (default: 5)'),
  },
  async (params) => {
    const data = await callProwlApi('investigate', params)
    return textResult(data)
  },
)

// Tool: prowl_compare — load a GitHub repo for comparison (lightweight, no clone)
server.tool(
  'prowl_compare',
  `Load a GitHub repo for comparison alongside the primary project.
Fetches the file tree via GitHub REST API (no disk clone, no indexing).
Only one comparison repo at a time — close the current one before loading another.
After loading, use prowl_compare_file_tree to browse, prowl_compare_read_file to read files,
prowl_compare_grep to search, and prowl_compare_summary for stats.`,
  {
    repo_url: z.string().describe('GitHub URL (e.g. https://github.com/owner/repo)'),
    token: z.string().optional().describe('GitHub personal access token for private repos'),
    branch: z.string().optional().describe('Branch (default: repo default branch)'),
  },
  async (params) => {
    const data = await callProwlApi('compare', params)
    return textResult(data)
  },
)

// Tool: prowl_compare_file_tree — browse the comparison repo file tree
server.tool(
  'prowl_compare_file_tree',
  `Browse files and directories in the comparison repo.
Returns immediate children of the given directory (or top-level if no dir_path).
Requires prowl_compare to be called first.`,
  {
    dir_path: z.string().optional().describe('Directory to list (omit for root)'),
  },
  async (params) => {
    const data = await callProwlApi('compare-file-tree', params)
    return textResult(data)
  },
)

// Tool: prowl_compare_read_file — read a file from the comparison repo
server.tool(
  'prowl_compare_read_file',
  `Read the full content of a file from the comparison repo.
Fetches from GitHub on first access, then caches locally.
Requires prowl_compare to be called first.`,
  {
    file_path: z.string().describe('File path in the comparison repo'),
  },
  async (params) => {
    const data = await callProwlApi('compare-read-file', params)
    return textResult(data)
  },
)

// Tool: prowl_compare_grep — regex search across cached comparison files
server.tool(
  'prowl_compare_grep',
  `Regex search across cached files in the comparison repo.
Only searches files previously fetched via prowl_compare_read_file.
Workflow: browse tree → fetch files → grep.
Requires prowl_compare to be called first.`,
  {
    pattern: z.string().describe('Regex pattern to search for'),
    file_filter: z.string().optional().describe('Only search files whose path contains this substring'),
    case_sensitive: z.boolean().optional().describe('Match case exactly (default: false)'),
    max_results: z.number().optional().describe('Result cap (default: 100)'),
  },
  async (params) => {
    const data = await callProwlApi('compare-grep', params)
    return textResult(data)
  },
)

// Tool: prowl_compare_summary — stats about the comparison repo
server.tool(
  'prowl_compare_summary',
  `Get summary stats for the loaded comparison repo: file count, dir count, total size, cached file count.
Requires prowl_compare to be called first.`,
  {},
  async () => {
    const data = await callProwlApi('compare-summary', {})
    return textResult(data)
  },
)

// Tool: prowl_changes — detect uncommitted changes and map to symbols/clusters
server.tool(
  'prowl_changes',
  `Detect uncommitted code changes and map them to affected symbols and clusters.
Scopes: "working" (unstaged, default), "staged", "all" (both + untracked), "branch" (compare against base ref).
Returns changed files, affected symbols, impacted clusters, and risk assessment.`,
  {
    scope: z.enum(['working', 'staged', 'all', 'branch']).optional().describe('Change scope (default: "working")'),
    base_ref: z.string().optional().describe('Base ref for branch comparison (required when scope is "branch")'),
  },
  async (params) => {
    const data = await callProwlApi('detect-changes', params)
    return textResult(data)
  },
)

/* ── Start ─────────────────────────────────────────────── */

async function main(): Promise<void> {
  await loadConnectionInfo()
  const transport = new StdioServerTransport()
  await server.connect(transport)
}

main().catch((err) => {
  console.error('Failed to start Prowl MCP server:', err)
  process.exit(1)
})
