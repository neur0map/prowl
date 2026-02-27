/**
 * Produces a deterministic identifier by joining a classification
 * tag with a descriptor using a fixed delimiter.
 */
export function generateId(tag: string, descriptor: string): string {
  return `${tag}:${descriptor}`;
}
