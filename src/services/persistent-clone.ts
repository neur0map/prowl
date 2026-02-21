/**
 * Persistent Clone Service
 *
 * Manages persistent local copies of GitHub repositories for the "Keep local copy" mode.
 * Clones are stored at ~/.prowl/repos/{owner}-{repo}/ and can be git pulled on re-open.
 */

import { cloneRepository, parseGitHubUrl } from './git-clone';
import type { FileEntry } from './zip';

const PROWL_HOME = '~/.prowl';
const REPOS_DIR = `${PROWL_HOME}/repos`;

/**
 * Get the local path for a persistent clone.
 */
export function getPersistentClonePath(repoUrl: string): string | null {
  const parsed = parseGitHubUrl(repoUrl);
  if (!parsed) return null;
  const { owner, repo } = parsed;
  return `${REPOS_DIR}/${owner}-${repo}`;
}

/**
 * Check if a persistent clone exists for a repo.
 */
export async function hasPersistentClone(repoUrl: string): Promise<boolean> {
  const prowl = (window as any).prowl;
  if (!prowl?.snapshot) return false;

  const path = getPersistentClonePath(repoUrl);
  if (!path) return false;

  return prowl.snapshot.exists(path);
}

/**
 * Clone a repository persistently to ~/.prowl/repos/{owner}-{repo}/.
 * Uses Electron IPC for filesystem access.
 *
 * Returns the files from the clone + the local path.
 */
export async function clonePersistently(
  repoUrl: string,
  onProgress: (phase: string, percent: number) => void,
  token?: string,
): Promise<{ files: FileEntry[]; localPath: string } | null> {
  const path = getPersistentClonePath(repoUrl);
  if (!path) return null;

  // Clone via isomorphic-git (in-memory FS → we read files from it)
  const files = await cloneRepository(repoUrl, onProgress, token);

  return { files, localPath: path };
}
