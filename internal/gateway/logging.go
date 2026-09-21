package gateway

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// RouteLogsToFile keeps gateway diagnostics out of interactive terminal frames.
// The returned restore function closes the file and reinstates the prior logger.
func RouteLogsToFile(dir string) (path string, restore func(), err error) {
	if dir == "" {
		dir = Dir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	path = filepath.Join(dir, "gateway.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return "", nil, err
	}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(file, &slog.HandlerOptions{Level: slog.LevelInfo})))
	var once sync.Once
	return path, func() {
		once.Do(func() {
			slog.SetDefault(previous)
			_ = file.Close()
		})
	}, nil
}
