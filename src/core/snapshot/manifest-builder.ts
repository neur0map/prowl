import type { FileManifest } from './types';

/**
 * Compute SHA-256 hash of a string using the Web Crypto API.
 * Works in both renderer and Web Worker contexts.
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
export async function buildFileManifest(fileContents: Map<string, string>): Promise<FileManifest> {
  const files: FileManifest['files'] = {};
  for (const [path, content] of fileContents) {
    files[path] = {
      hash: await sha256(content),
      mtime: Date.now(),
    };
  }
  return { files };
}
