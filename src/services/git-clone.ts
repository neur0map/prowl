import * as webHttp from 'isomorphic-git/http/web';
import LightningFS from '@isomorphic-git/lightning-fs';
import { shouldIgnorePath } from '../config/ignore-service';
import type { FileEntry } from '../types/file-entry';

/* ── Lazy module loader for isomorphic-git (CJS compat) */

const loadIsomorphicGit = async () => {
  const m = await import('isomorphic-git');
  return m.default || m;
};

/* ── HTTP client that routes through Electron main process ── */
// Avoids CORS — main process uses Electron net (Chromium networking, no CORS).
// Falls back to isomorphic-git/http/web in pure-browser mode.

const electronHttpClient = {
  async request(config: {
    url: string;
    method?: string;
    headers?: Record<string, string>;
    body?: AsyncIterableIterator<Uint8Array>;
  }) {
    // Collect request body chunks into a single array
    let bodyBytes: number[] = [];
    if (config.body) {
      const chunks: Uint8Array[] = [];
      for await (const chunk of config.body) {
        chunks.push(chunk);
      }
      if (chunks.length > 0) {
        const total = chunks.reduce((s, c) => s + c.length, 0);
        const merged = new Uint8Array(total);
        let offset = 0;
        for (const c of chunks) {
          merged.set(c, offset);
          offset += c.length;
        }
        bodyBytes = Array.from(merged);
      }
    }

    const res = await window.prowl.git.httpRequest({
      url: config.url,
      method: config.method || 'GET',
      headers: config.headers || {},
      body: bodyBytes.length > 0 ? bodyBytes : undefined,
    });

    // Convert response body back to async iterable of Uint8Array
    const responseBody = new Uint8Array(res.body);
    return {
      url: config.url,
      method: config.method || 'GET',
      statusCode: res.statusCode,
      statusMessage: res.statusMessage,
      headers: res.headers,
      body: [responseBody],
    };
  },
};

const isElectron = typeof window !== 'undefined' && !!(window as any).prowl?.git;
const http = isElectron ? electronHttpClient : webHttp;

/* ── Virtual filesystem singleton ───────────────────── */

let fs: LightningFS;
let pfs: any;

function initVFS(): string {
  const tag = `prowl-git-${Date.now()}`;
  fs = new LightningFS(tag);
  pfs = fs.promises;
  return tag;
}

/* ── URL parsing ────────────────────────────────────── */

const GITHUB_REGEX = /github\.com\/([^\/]+)\/([^\/]+)/;

export function parseGitHubUrl(url: string): { owner: string; repo: string } | null {
  const normalized = url.trim().replace(/\.git$/, '');
  const hit = GITHUB_REGEX.exec(normalized);
  if (!hit) return null;
  return { owner: hit[1], repo: hit[2] };
}

type ProgressFn = (phase: string, progress: number) => void;

/* ── Recursive file walker ──────────────────────────── */

async function walkTree(root: string, cwd: string): Promise<FileEntry[]> {
  const entries: FileEntry[] = [];

  let listing: string[];
  try {
    listing = await pfs.readdir(cwd);
  } catch {
    console.warn(`Cannot read directory: ${cwd}`);
    return entries;
  }

  for (const name of listing) {
    if (name === '.git') continue;

    const absolute = `${cwd}/${name}`;
    const relative = absolute.replace(`${root}/`, '');

    if (shouldIgnorePath(relative)) continue;

    let info;
    try {
      info = await pfs.stat(absolute);
    } catch {
      if (import.meta.env.DEV) {
        console.warn(`Skipping unreadable entry: ${relative}`);
      }
      continue;
    }

    if (info.isDirectory()) {
      const nested = await walkTree(root, absolute);
      for (const f of nested) entries.push(f);
    } else {
      try {
        const text = await pfs.readFile(absolute, { encoding: 'utf8' }) as string;
        entries.push({ path: relative, content: text });
      } catch {
        // binary or unreadable — skip silently
      }
    }
  }

  return entries;
}

/* ── Cleanup helper ─────────────────────────────────── */

async function rmRecursive(target: string): Promise<void> {
  try {
    const items = await pfs.readdir(target);
    for (const item of items) {
      const full = `${target}/${item}`;
      const meta = await pfs.stat(full);
      if (meta.isDirectory()) {
        await rmRecursive(full);
      } else {
        await pfs.unlink(full);
      }
    }
    await pfs.rmdir(target);
  } catch {
    // cleanup errors are non-fatal
  }
}

/* ── URL and auth builders ──────────────────────────── */

function githubCloneUrl(owner: string, repo: string): string {
  return `https://github.com/${owner}/${repo}.git`;
}

function authCallback(token?: string) {
  if (!token) return undefined;
  return () => ({ username: token, password: 'x-oauth-basic' });
}

/* ── Public: clone a GitHub repo into virtual FS ────── */

export async function cloneRepository(
  url: string,
  onProgress?: ProgressFn,
  token?: string,
): Promise<FileEntry[]> {
  const parsed = parseGitHubUrl(url);
  if (!parsed) {
    throw new Error('Invalid URL. Expected: https://github.com/owner/repo');
  }

  const dbTag = initVFS();
  const workdir = `/${parsed.repo}`;
  const cloneTarget = githubCloneUrl(parsed.owner, parsed.repo);

  const notify = (phase: string, pct: number) => { onProgress?.(phase, pct); };
  const teardown = async () => {
    try { await rmRecursive(workdir); } catch {}
    try { indexedDB.deleteDatabase(dbTag); } catch {}
  };

  try {
    notify('cloning', 0);

    const git = await loadIsomorphicGit();
    await git.clone({
      fs,
      http,
      dir: workdir,
      url: cloneTarget,
      depth: 1,
      onAuth: authCallback(token),
      onProgress: (ev) => {
        if (ev.total) {
          notify('cloning', Math.round((ev.loaded / ev.total) * 100));
        }
      },
    });

    notify('reading', 0);

    const results = await walkTree(workdir, workdir);

    await teardown();
    notify('complete', 100);

    return results;
  } catch (err) {
    await teardown();
    throw err;
  }
}
