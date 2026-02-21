import { app, safeStorage } from 'electron'
import { join } from 'path'
import { mkdir, writeFile, readFile, rename, unlink, stat, access, appendFile } from 'fs/promises'
import { createHmac, randomBytes } from 'crypto'

const PROWL_DIR = '.prowl'
const SNAPSHOT_FILE = 'snapshot.bin'
const SNAPSHOT_TMP = 'snapshot.bin.tmp'
const META_FILE = 'meta.json'
const MANIFEST_FILE = 'manifest.json'
const LOCK_FILE = 'lock'

function getProwlDir(projectPath: string): string {
  return join(projectPath, PROWL_DIR)
}

function getHmacKeyPath(): string {
  return join(app.getPath('userData'), 'prowl-hmac-key.enc')
}

/**
 * Get or create the HMAC key for snapshot integrity verification.
 * Key is encrypted via safeStorage (OS keychain) and stored in userData.
 */
async function getOrCreateHmacKey(): Promise<Buffer> {
  const keyPath = getHmacKeyPath()

  try {
    const encryptedKey = await readFile(keyPath)
    if (safeStorage.isEncryptionAvailable()) {
      const decrypted = safeStorage.decryptString(encryptedKey)
      return Buffer.from(decrypted, 'hex')
    }
    // Fallback: use the encrypted bytes directly as key material
    return encryptedKey
  } catch {
    // Key doesn't exist yet — generate one
    const key = randomBytes(32) // 256-bit
    try {
      if (safeStorage.isEncryptionAvailable()) {
        const encrypted = safeStorage.encryptString(key.toString('hex'))
        await writeFile(keyPath, encrypted)
      } else {
        // No safeStorage — store raw (still better than nothing)
        await writeFile(keyPath, key)
      }
    } catch {
      // Can't persist key — use ephemeral
    }
    return key
  }
}

export class SnapshotIO {
  private hmacKey: Buffer | null = null

  private async getKey(): Promise<Buffer> {
    if (!this.hmacKey) {
      this.hmacKey = await getOrCreateHmacKey()
    }
    return this.hmacKey
  }

  async ensureProwlDir(projectPath: string): Promise<void> {
    const dir = getProwlDir(projectPath)
    await mkdir(dir, { recursive: true })
  }

  /**
   * Atomic write: write to tmp file then rename.
   */
  async writeSnapshot(projectPath: string, data: Uint8Array): Promise<void> {
    const dir = getProwlDir(projectPath)
    await mkdir(dir, { recursive: true })

    const tmpPath = join(dir, SNAPSHOT_TMP)
    const finalPath = join(dir, SNAPSHOT_FILE)
    const lockPath = join(dir, LOCK_FILE)

    // Write lock with PID
    await writeFile(lockPath, String(process.pid), 'utf-8')

    try {
      await writeFile(tmpPath, Buffer.from(data))
      await rename(tmpPath, finalPath)
    } finally {
      // Clean up lock
      try { await unlink(lockPath) } catch { /* ignore */ }
    }
  }

  async readSnapshot(projectPath: string): Promise<Uint8Array | null> {
    try {
      const buf = await readFile(join(getProwlDir(projectPath), SNAPSHOT_FILE))
      return new Uint8Array(buf)
    } catch {
      return null
    }
  }

  async writeManifest(projectPath: string, manifest: object): Promise<void> {
    const dir = getProwlDir(projectPath)
    await mkdir(dir, { recursive: true })
    await writeFile(join(dir, MANIFEST_FILE), JSON.stringify(manifest, null, 2), 'utf-8')
  }

  async readManifest(projectPath: string): Promise<object | null> {
    try {
      const raw = await readFile(join(getProwlDir(projectPath), MANIFEST_FILE), 'utf-8')
      return JSON.parse(raw)
    } catch {
      return null
    }
  }

  async writeMeta(projectPath: string, meta: object): Promise<void> {
    const dir = getProwlDir(projectPath)
    await mkdir(dir, { recursive: true })
    await writeFile(join(dir, META_FILE), JSON.stringify(meta, null, 2), 'utf-8')
  }

  async readMeta(projectPath: string): Promise<object | null> {
    try {
      const raw = await readFile(join(getProwlDir(projectPath), META_FILE), 'utf-8')
      return JSON.parse(raw)
    } catch {
      return null
    }
  }

  async snapshotExists(projectPath: string): Promise<boolean> {
    try {
      await access(join(getProwlDir(projectPath), SNAPSHOT_FILE))
      // Clean stale locks while we're checking
      await this.cleanStaleLock(projectPath)
      return true
    } catch {
      return false
    }
  }

  async generateHMAC(data: Uint8Array): Promise<string> {
    const key = await this.getKey()
    return createHmac('sha256', key).update(Buffer.from(data)).digest('hex')
  }

  async verifyHMAC(data: Uint8Array, hmac: string): Promise<boolean> {
    const key = await this.getKey()
    const expected = createHmac('sha256', key).update(Buffer.from(data)).digest('hex')
    return expected === hmac
  }

  /**
   * Append .prowl/ to the project's .gitignore if not already present.
   */
  async ensureGitignore(projectPath: string): Promise<void> {
    const gitignorePath = join(projectPath, '.gitignore')
    try {
      let content = ''
      try {
        content = await readFile(gitignorePath, 'utf-8')
      } catch {
        // .gitignore doesn't exist — we'll create it
      }

      // Check if already has .prowl entry
      const lines = content.split('\n')
      if (lines.some(l => l.trim() === '.prowl/' || l.trim() === '.prowl')) {
        return
      }

      // Append
      const suffix = content.endsWith('\n') || content === '' ? '' : '\n'
      await appendFile(gitignorePath, `${suffix}\n# Prowl snapshot cache\n.prowl/\n`)
    } catch {
      // .gitignore is read-only or other error — don't crash
      console.warn('[prowl:snapshot] Could not update .gitignore')
    }
  }

  /**
   * Clean up stale lockfiles (e.g., from a crash mid-save).
   */
  async cleanStaleLock(projectPath: string): Promise<void> {
    const lockPath = join(getProwlDir(projectPath), LOCK_FILE)
    try {
      await access(lockPath)
      // Lock exists — check if the PID is still running
      const pid = parseInt(await readFile(lockPath, 'utf-8'), 10)
      try {
        process.kill(pid, 0) // Signal 0 = check if alive
        // Process is alive — leave the lock
      } catch {
        // Process is dead — remove stale lock
        await unlink(lockPath)
      }
    } catch {
      // No lock file — nothing to do
    }
  }

  /**
   * List all projects with snapshots under a base directory (e.g. ~/.prowl/).
   */
  async listProjects(basePath: string): Promise<Array<{ path: string; meta: object | null }>> {
    const { readdir } = await import('fs/promises')
    const results: Array<{ path: string; meta: object | null }> = []
    try {
      const entries = await readdir(basePath, { withFileTypes: true })
      for (const entry of entries) {
        if (entry.isDirectory()) {
          const projectDir = join(basePath, entry.name)
          if (await this.snapshotExists(projectDir)) {
            const meta = await this.readMeta(projectDir)
            results.push({ path: projectDir, meta })
          }
        }
      }
    } catch {
      // Directory doesn't exist
    }
    return results
  }

  /**
   * Get total disk usage of .prowl/ directory for a project.
   */
  async getDiskUsage(projectPath: string): Promise<number> {
    const { readdir } = await import('fs/promises')
    const dir = getProwlDir(projectPath)
    let total = 0
    try {
      const entries = await readdir(dir)
      for (const entry of entries) {
        try {
          const s = await stat(join(dir, entry))
          total += s.size
        } catch { /* skip */ }
      }
    } catch {
      // Directory doesn't exist
    }
    return total
  }

  /**
   * Delete the .prowl/ directory for a project.
   */
  async deleteSnapshot(projectPath: string): Promise<void> {
    const { rm } = await import('fs/promises')
    const dir = getProwlDir(projectPath)
    try {
      await rm(dir, { recursive: true, force: true })
    } catch {
      // Already gone
    }
  }
}
