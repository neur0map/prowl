package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

const pidFileName = "gateway.pid"

// PIDRecord binds a process id to the loopback port where that exact gateway
// identifies itself. The port lets stop operations distinguish the recorded
// gateway from an unrelated process that later reused a stale pid.
type PIDRecord struct {
	PID  int `json:"pid"`
	Port int `json:"port"`
}

// PIDFilePath is where a gateway serving on port records its process id. The
// default port retains the historical gateway.pid name; custom ports get
// independent records so their lifecycle controls cannot target each other.
func PIDFilePath(dir string, port ...int) string {
	listenPort := pidFilePort(port)
	if listenPort == DefaultPort {
		return filepath.Join(dir, pidFileName)
	}
	return filepath.Join(dir, fmt.Sprintf("gateway-%d.pid", listenPort))
}

func pidFilePort(port []int) int {
	if len(port) > 0 && port[0] > 0 {
		return port[0]
	}
	return DefaultPort
}

// pidLockMu serializes pid read-modify-write cycles within this process; the
// per-port flock beneath withPIDLock serializes them across processes. It
// reuses the gofrs/flock convention already used for the workspace registry
// and index-refresh lock rather than inventing a second mechanism.
var pidLockMu sync.Mutex

// withPIDLock runs fn while holding the exclusive lock for one gateway's pid
// record, so a conditional removal cannot interleave with a replacement
// daemon's write of a fresh record. The lock is per port, keeping each
// gateway's lifecycle independent.
func withPIDLock(dir string, port int, fn func() error) error {
	pidLockMu.Lock()
	defer pidLockMu.Unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create gateway dir: %w", err)
	}
	lock := flock.New(PIDFilePath(dir, port) + ".lock")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	locked, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return err
	}
	if !locked {
		return errors.New("gateway pid lock was not acquired")
	}
	return errors.Join(fn(), lock.Unlock())
}

// WritePIDFile records the current process as the gateway holding dir and port.
// It creates dir if the gateway has never run before. Omitting port addresses
// the default gateway for source compatibility.
func WritePIDFile(dir string, port ...int) error {
	listenPort := pidFilePort(port)
	return withPIDLock(dir, listenPort, func() error {
		blob, err := json.Marshal(PIDRecord{PID: os.Getpid(), Port: listenPort})
		if err != nil {
			return fmt.Errorf("encode gateway pid: %w", err)
		}
		if err := os.WriteFile(PIDFilePath(dir, listenPort), blob, 0o600); err != nil {
			return fmt.Errorf("write gateway pid: %w", err)
		}
		return nil
	})
}

// ReadPIDRecord returns the recorded pid and port for the requested gateway.
// Plain integer records from earlier releases remain readable on the default
// port but carry no verified port identity.
func ReadPIDRecord(dir string, port ...int) (PIDRecord, error) {
	blob, err := os.ReadFile(PIDFilePath(dir, port...))
	if errors.Is(err, os.ErrNotExist) {
		return PIDRecord{}, nil
	}
	if err != nil {
		return PIDRecord{}, fmt.Errorf("read gateway pid: %w", err)
	}
	text := strings.TrimSpace(string(blob))
	var record PIDRecord
	if strings.HasPrefix(text, "{") {
		if err := json.Unmarshal(blob, &record); err != nil {
			return PIDRecord{}, fmt.Errorf("parse gateway pid record: %w", err)
		}
		if record.PID <= 0 || record.Port < 0 {
			return PIDRecord{}, fmt.Errorf("invalid gateway pid record %q", text)
		}
		return record, nil
	}
	pid, err := strconv.Atoi(text)
	if err != nil {
		return PIDRecord{}, fmt.Errorf("parse gateway pid %q: %w", text, err)
	}
	return PIDRecord{PID: pid}, nil
}

// ReadPIDFile returns only the recorded pid for status callers.
func ReadPIDFile(dir string, port ...int) (int, error) {
	record, err := ReadPIDRecord(dir, port...)
	return record.PID, err
}

// RemovePIDFile clears the requested gateway's pid. A missing file is not an
// error: the point is that no record remains.
func RemovePIDFile(dir string, port ...int) error {
	err := os.Remove(PIDFilePath(dir, port...))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// RemovePIDFileIfPID clears the record only while it still names pid. A server
// shutting down must not remove a newer daemon's record after the port has
// already been handed over.
func RemovePIDFileIfPID(dir string, pid int, port ...int) error {
	listenPort := pidFilePort(port)
	return withPIDLock(dir, listenPort, func() error {
		record, err := ReadPIDRecord(dir, listenPort)
		if err != nil {
			return err
		}
		if record.PID == 0 || record.PID != pid {
			return nil
		}
		return RemovePIDFile(dir, listenPort)
	})
}
