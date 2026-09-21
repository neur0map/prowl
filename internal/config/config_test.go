package config

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/boundedio"
)

func TestConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := Default()
	c.AI.Enabled = true
	c.AI.AssistModel = "test-assist"
	if err := Save(dir, c); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !got.AI.Enabled || got.AI.AssistModel != "test-assist" {
		t.Fatalf("roundtrip = %+v", got.AI)
	}
	if len(got.Languages) == 0 {
		t.Fatal("languages not persisted")
	}

	// Missing config returns defaults, which enable semantic assist by default.
	if d, _ := Load(t.TempDir()); !d.AI.Enabled {
		t.Fatal("default config should have AI enabled")
	}

	if err := SaveRules(dir, DefaultRules()); err != nil {
		t.Fatal(err)
	}
	r, err := LoadRules(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rule) != 1 {
		t.Fatalf("general rules = %d, want 1", len(r.Rule))
	}
	if rice := RiceRules(); len(rice.Rule) != 3 {
		t.Fatalf("rice rules = %d, want 3", len(rice.Rule))
	}
}

func TestLoadContextBoundsAndValidatesConfigInput(t *testing.T) {
	t.Run("regular and absent", func(t *testing.T) {
		dir := t.TempDir()
		cfg := Default()
		cfg.AI.Enabled = true
		if err := Save(dir, cfg); err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadContext(context.Background(), dir)
		if err != nil || !loaded.AI.Enabled {
			t.Fatalf("loaded=%+v error=%v", loaded, err)
		}
		absent, err := LoadContext(context.Background(), t.TempDir())
		if err != nil || !absent.AI.Enabled {
			t.Fatalf("absent=%+v error=%v", absent, err)
		}
	})

	t.Run("size limit", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, configName), []byte(strings.Repeat("#", int(MaxConfigBytes)+1)), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadContext(context.Background(), dir); !errors.Is(err, boundedio.ErrTooLarge) {
			t.Fatalf("error=%v want size limit", err)
		}
	})

	t.Run("special file", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows has no POSIX FIFO contract")
		}
		if _, err := exec.LookPath("mkfifo"); err != nil {
			t.Skip("mkfifo unavailable")
		}
		dir := t.TempDir()
		if err := exec.Command("mkfifo", filepath.Join(dir, configName)).Run(); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadContext(context.Background(), dir); !errors.Is(err, boundedio.ErrNonRegular) {
			t.Fatalf("error=%v want non-regular input", err)
		}
	})
}
