import { pack, unpack } from 'msgpackr';
import type { SnapshotPayload } from './types';

/**
 * Serialize a SnapshotPayload to a gzip-compressed Uint8Array.
 * Uses msgpackr for compact binary encoding + browser-native CompressionStream.
 */
export async function serializeSnapshot(payload: SnapshotPayload): Promise<Uint8Array> {
  const packed = pack(payload) as Uint8Array;

  // Compress with gzip via streaming API (available in Electron/Chromium)
  const cs = new CompressionStream('gzip');
  const writer = cs.writable.getWriter();
  const reader = cs.readable.getReader();

  // Write packed data and close
  writer.write(packed as unknown as BufferSource);
  writer.close();

  // Collect compressed chunks
  const chunks: Uint8Array[] = [];
  let totalLength = 0;
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    chunks.push(value);
    totalLength += value.byteLength;
  }

  // Concatenate chunks into a single Uint8Array
  const result = new Uint8Array(totalLength);
  let offset = 0;
  for (const chunk of chunks) {
    result.set(chunk, offset);
    offset += chunk.byteLength;
  }

  return result;
}

/**
 * Deserialize a gzip-compressed Uint8Array back to a SnapshotPayload.
 * Reverses the compression + msgpackr encoding.
 */
export async function deserializeSnapshot(data: Uint8Array): Promise<SnapshotPayload> {
  // Decompress with gzip via streaming API
  const ds = new DecompressionStream('gzip');
  const writer = ds.writable.getWriter();
  const reader = ds.readable.getReader();

  // Write compressed data and close
  writer.write(data as unknown as BufferSource);
  writer.close();

  // Collect decompressed chunks
  const chunks: Uint8Array[] = [];
  let totalLength = 0;
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    chunks.push(value);
    totalLength += value.byteLength;
  }

  // Concatenate into single buffer
  const decompressed = new Uint8Array(totalLength);
  let offset = 0;
  for (const chunk of chunks) {
    decompressed.set(chunk, offset);
    offset += chunk.byteLength;
  }

  return unpack(decompressed) as SnapshotPayload;
}
