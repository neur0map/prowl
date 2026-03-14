package daemon

import (
	"sync"
	"time"

	"github.com/neur0map/prowl/internal/embed"
)

// lazyEmbedder loads the embedding model on demand and unloads after idle timeout.
// Self-contained with its own mutex — does not require the daemon's main mutex.
type lazyEmbedder struct {
	embedder    *embed.Embedder
	modelDir    string
	idleTimeout time.Duration
	timer       *time.Timer
	mu          sync.Mutex
	closed      bool
}

func newLazyEmbedder(modelDir string, idleTimeout time.Duration) *lazyEmbedder {
	return &lazyEmbedder{
		modelDir:    modelDir,
		idleTimeout: idleTimeout,
	}
}

// Get returns the embedder, loading it if necessary. Resets the idle timer.
// Returns (nil, err) if model is not downloaded or loading fails.
func (le *lazyEmbedder) Get() (*embed.Embedder, error) {
	le.mu.Lock()
	defer le.mu.Unlock()

	if le.closed {
		return nil, nil
	}

	if le.embedder == nil {
		emb, err := embed.New(le.modelDir)
		if err != nil {
			// Start idle timer even on failure to avoid retry storms
			le.resetTimerLocked()
			return nil, err
		}
		le.embedder = emb
	}

	le.resetTimerLocked()
	return le.embedder, nil
}

// Close releases the embedder and stops the idle timer.
func (le *lazyEmbedder) Close() {
	le.mu.Lock()
	defer le.mu.Unlock()

	le.closed = true
	if le.timer != nil {
		le.timer.Stop()
		le.timer = nil
	}
	if le.embedder != nil {
		le.embedder.Close()
		le.embedder = nil
	}
}

func (le *lazyEmbedder) resetTimerLocked() {
	if le.timer != nil {
		le.timer.Stop()
	}
	le.timer = time.AfterFunc(le.idleTimeout, func() {
		le.mu.Lock()
		defer le.mu.Unlock()
		if le.embedder != nil {
			le.embedder.Close()
			le.embedder = nil
		}
	})
}
