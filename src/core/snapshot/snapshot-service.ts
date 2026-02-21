import type { KnowledgeGraph } from '../graph/types';
import type { SnapshotMeta, FileManifest } from './types';
import { collectSnapshotPayload } from './collector';
import { serializeSnapshot } from './serializer';

/**
 * Compute SHA-256 hash of a string using the Web Crypto API.
 */
async function sha256(content: string): Promise<string> {
  const encoder = new TextEncoder();
  const data = encoder.encode(content);
  const hashBuffer = await crypto.subtle.digest('SHA-256', data);
  const hashArray = Array.from(new Uint8Array(hashBuffer));
  return hashArray.map(b => b.toString(16).padStart(2, '0')).join('');
}

/**
 * Build a FileManifest from the fileContents map.
 */
async function buildFileManifest(fileContents: Map<string, string>): Promise<FileManifest> {
  const files: FileManifest['files'] = {};
  for (const [path, content] of fileContents) {
    files[path] = {
      hash: await sha256(content),
      mtime: Date.now(),
    };
  }
  return { files };
}

/**
 * Try to get the current git HEAD commit hash via Electron IPC.
 */
async function getGitCommit(projectPath: string): Promise<string | null> {
  // Use the snapshot IPC to run git rev-parse — but we don't have a direct git IPC.
  // Instead we'll use the fs:readFile to read .git/HEAD, then the ref.
  const prowl = (globalThis as any).window?.prowl ?? (globalThis as any).prowl;
  if (!prowl?.fs?.readFile) return null;

  try {
    const head = await prowl.fs.readFile(`${projectPath}/.git/HEAD`);
    const match = head.trim().match(/^ref: (.+)$/);
    if (match) {
      const ref = match[1];
      const commit = await prowl.fs.readFile(`${projectPath}/.git/${ref}`);
      return commit.trim();
    }
    // Detached HEAD — HEAD itself is the commit hash
    return head.trim();
  } catch {
    return null;
  }
}

/**
 * Save a project snapshot to disk via Electron IPC.
 *
 * Collects the full payload (graph, fileContents, embeddings),
 * serializes + compresses, writes snapshot + meta + manifest,
 * and ensures .gitignore has .prowl/ entry.
 *
 * Returns stats about the save operation.
 * On any failure, returns { success: false } — caller should not crash.
 */
export async function saveProjectSnapshot(
  projectPath: string,
  graph: KnowledgeGraph,
  fileContents: Map<string, string>,
  projectName: string,
  prowlVersion: string,
  kuzuQueryFn?: (cypher: string) => Promise<any[]>,
): Promise<{ success: boolean; size: number; durationMs: number }> {
  const start = performance.now();
  const prowl = (globalThis as any).window?.prowl ?? (globalThis as any).prowl;
  if (!prowl?.snapshot) {
    return { success: false, size: 0, durationMs: 0 };
  }

  try {
    const gitCommit = await getGitCommit(projectPath);

    // Collect payload
    const payload = await collectSnapshotPayload(
      graph, fileContents, projectName, prowlVersion, kuzuQueryFn, gitCommit
    );

    // Serialize (msgpackr + gzip)
    const data = await serializeSnapshot(payload);

    // Generate HMAC
    const hmac = await prowl.snapshot.generateHMAC(data);

    // Write snapshot atomically
    await prowl.snapshot.write(projectPath, data);

    // Write meta with HMAC
    await prowl.snapshot.writeMeta(projectPath, { ...payload.meta, hmac });

    // Build and write file manifest
    const manifest = await buildFileManifest(fileContents);
    await prowl.snapshot.writeManifest(projectPath, manifest);

    // Ensure .gitignore has .prowl/
    await prowl.snapshot.ensureGitignore(projectPath);

    const durationMs = Math.round(performance.now() - start);
    if (import.meta.env.DEV) {
      console.log(`[prowl:snapshot] Saved: ${(data.byteLength / 1024).toFixed(0)} KB in ${durationMs}ms`);
    }

    return { success: true, size: data.byteLength, durationMs };
  } catch (err) {
    console.warn('[prowl:snapshot] Save failed:', err);
    return { success: false, size: 0, durationMs: Math.round(performance.now() - start) };
  }
}
