/**
 * Idle timer for comparison project cleanup.
 * Resets on every compare/diff/extract access.
 * Fires onExpire callback after TTL with no access.
 */

const DEFAULT_TTL_MS = 30 * 60 * 1000; // 30 minutes

export interface ComparisonTimer {
  reset(): void;
  pin(): void;
  unpin(): void;
  stop(): void;
  isPinned(): boolean;
  getExpiresAt(): number | null;
}

export function createComparisonTimer(
  onExpire: () => void,
  ttlMs: number = DEFAULT_TTL_MS,
): ComparisonTimer {
  let timer: ReturnType<typeof setTimeout> | null = null;
  let pinned = false;
  let expiresAt: number | null = null;

  function schedule(): void {
    clear();
    if (pinned) return;
    expiresAt = Date.now() + ttlMs;
    timer = setTimeout(() => {
      timer = null;
      expiresAt = null;
      onExpire();
    }, ttlMs);
  }

  function clear(): void {
    if (timer !== null) {
      clearTimeout(timer);
      timer = null;
    }
    expiresAt = null;
  }

  // Start the timer immediately
  schedule();

  return {
    reset() {
      if (!pinned) schedule();
    },
    pin() {
      pinned = true;
      clear();
    },
    unpin() {
      pinned = false;
      schedule();
    },
    stop() {
      pinned = false;
      clear();
    },
    isPinned() {
      return pinned;
    },
    getExpiresAt() {
      return expiresAt;
    },
  };
}
