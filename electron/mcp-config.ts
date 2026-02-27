/**
 * Auto-configuration for Claude Code MCP integration.
 *
 * Detects the Claude Code config file and writes/updates the
 * Prowl MCP server entry so Claude Code can discover Prowl's
 * tools automatically.
 */

import { readFile, writeFile, access } from 'fs/promises'
import { join } from 'path'
import { homedir } from 'os'

const CLAUDE_CONFIG_PATH = join(homedir(), '.claude.json')

/**
 * Resolves the path to the bundled MCP server script.
 * In development: use the project source directly.
 * In production: use the extraResources path inside the app bundle.
 */
function getMcpServerPath(): string {
  // In packaged app, process.resourcesPath points to the Resources dir
  if (process.resourcesPath && !process.resourcesPath.includes('node_modules')) {
    return join(process.resourcesPath, 'mcp-server.js')
  }

  // Development fallback: __dirname is dist/main/, mcp-server.js is in dist/
  return join(__dirname, '..', 'mcp-server.js')
}

/**
 * Write or update the Prowl MCP server entry in Claude Code's config.
 * Reads existing config, merges (doesn't overwrite other MCP servers), writes back.
 */
export async function configureMcpForClaudeCode(): Promise<{
  success: boolean
  path?: string
  error?: string
}> {
  try {
    const mcpServerPath = getMcpServerPath()

    // Read existing config — must exist already (Claude Code creates it)
    let config: Record<string, unknown> = {}
    try {
      const raw = await readFile(CLAUDE_CONFIG_PATH, 'utf-8')
      config = JSON.parse(raw)
    } catch {
      // File doesn't exist or is invalid JSON — start fresh
    }

    // Ensure mcpServers key exists at the top level
    if (!config.mcpServers || typeof config.mcpServers !== 'object') {
      config.mcpServers = {}
    }

    const mcpServers = config.mcpServers as Record<string, unknown>

    // Write/update the prowl entry with stdio transport
    mcpServers.prowl = {
      type: 'stdio',
      command: 'node',
      args: [mcpServerPath],
    }

    await writeFile(CLAUDE_CONFIG_PATH, JSON.stringify(config, null, 2), 'utf-8')

    return { success: true, path: CLAUDE_CONFIG_PATH }
  } catch (err) {
    const message = err instanceof Error ? err.message : String(err)
    return { success: false, error: message }
  }
}

/**
 * Remove the Prowl MCP server entry from Claude Code's config.
 */
export async function removeMcpFromClaudeCode(): Promise<{
  success: boolean
  error?: string
}> {
  try {
    const raw = await readFile(CLAUDE_CONFIG_PATH, 'utf-8')
    const config = JSON.parse(raw)
    if (config?.mcpServers?.prowl) {
      delete config.mcpServers.prowl
      await writeFile(CLAUDE_CONFIG_PATH, JSON.stringify(config, null, 2), 'utf-8')
    }
    return { success: true }
  } catch (err) {
    const message = err instanceof Error ? err.message : String(err)
    return { success: false, error: message }
  }
}

/**
 * Check if Prowl is already configured in Claude Code's config.
 */
export async function getMcpConfigStatus(): Promise<{
  configured: boolean
  configPath: string | null
  mcpServerPath: string | null
}> {
  try {
    await access(CLAUDE_CONFIG_PATH)
    const raw = await readFile(CLAUDE_CONFIG_PATH, 'utf-8')
    const config = JSON.parse(raw)
    const hasProwl = !!(config?.mcpServers?.prowl)
    return {
      configured: hasProwl,
      configPath: CLAUDE_CONFIG_PATH,
      mcpServerPath: hasProwl ? getMcpServerPath() : null,
    }
  } catch {
    return { configured: false, configPath: null, mcpServerPath: null }
  }
}
