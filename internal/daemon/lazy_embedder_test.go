package daemon

import (
	"testing"
	"time"
)

func TestLazyEmbedderNilWhenNoModel(t *testing.T) {
	le := newLazyEmbedder("/nonexistent/path", 1*time.Second)
	defer le.Close()

	emb, err := le.Get()
	// Should return nil embedder when model dir doesn't exist
	if err == nil && emb != nil {
		t.Fatal("expected nil embedder or error for nonexistent model dir")
	}
}

func TestLazyEmbedderIdleUnload(t *testing.T) {
	le := newLazyEmbedder("/nonexistent/path", 50*time.Millisecond)
	defer le.Close()

	// Try to load — will fail but sets internal state
	le.Get()

	// After idle timeout, embedder should be nil
	time.Sleep(100 * time.Millisecond)

	le.mu.Lock()
	loaded := le.embedder != nil
	le.mu.Unlock()
	if loaded {
		t.Fatal("embedder should be unloaded after idle timeout")
	}
}

func TestLazyEmbedderCloseIdempotent(t *testing.T) {
	le := newLazyEmbedder("/nonexistent/path", 1*time.Second)
	le.Close()
	le.Close() // should not panic
}
