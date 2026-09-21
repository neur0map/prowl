package selfupdate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSameCommit(t *testing.T) {
	full := "abc1234def5678abc1234def5678abc1234def56"
	cases := []struct {
		a, b string
		same bool
	}{
		{full, full, true},
		{"abc1234", full, true}, // short vs full
		{full, "abc1234", true},
		{"abc1234", "abc1235", false}, // differ within the prefix
		{"", full, false},
		{"ABC1234", "abc1234def", true}, // case-insensitive
	}
	for _, c := range cases {
		if got := sameCommit(c.a, c.b); got != c.same {
			t.Errorf("sameCommit(%q,%q) = %v, want %v", c.a, c.b, got, c.same)
		}
	}
}

func TestParseChecksum(t *testing.T) {
	h, err := parseChecksum([]byte("abcdef0123456789abcdef0123456789  prowl-linux-amd64\n"))
	if err != nil || h != "abcdef0123456789abcdef0123456789" {
		t.Fatalf("parse = %q err=%v", h, err)
	}
	if _, err := parseChecksum([]byte("   ")); err == nil {
		t.Fatal("expected error on empty checksum")
	}
	if _, err := parseChecksum([]byte("short  x")); err == nil {
		t.Fatal("expected error on short digest")
	}
}

func TestCacheRoundTripAndTTL(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	writeCache(cache{CheckedAt: time.Now().Unix(), Latest: "abc123"})
	c, ok := readCache()
	if !ok || c.Latest != "abc123" {
		t.Fatalf("fresh cache = %+v ok=%v", c, ok)
	}
	writeCache(cache{CheckedAt: time.Now().Add(-48 * time.Hour).Unix(), Latest: "abc123"})
	if _, ok := readCache(); ok {
		t.Fatal("stale cache should be ignored")
	}
}

func TestCheckUsesCachedLatest(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	// A cached "latest" that differs from this build means an update is available,
	// computed without any network call.
	writeCache(cache{CheckedAt: time.Now().Unix(), Latest: "0000000000000000000000000000000000000000", Channel: Stable.Name})
	if r := Check("v9.9.9-deadbee"); !r.Available || !r.Checked {
		t.Fatalf("should report available from cached latest without network: %+v", r)
	}
}

// TestManagedGuard pins the self-update guard's decision and message, with no
// network and no filesystem: a build-time managedBy or a non-writable install
// dir defers to the package manager and names it, while a self-built binary in a
// writable dir updates normally.
func TestManagedGuard(t *testing.T) {
	cases := []struct {
		name        string
		managedBy   string
		dirWritable bool
		wantManaged bool
		wantNames   string
	}{
		{"pacman stamp, writable dir", "pacman", true, true, "pacman"},
		{"pacman stamp, read-only dir", "pacman", false, true, "pacman"},
		{"no stamp, read-only dir", "", false, true, "your package manager"},
		{"no stamp, writable dir", "", true, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, managed := managedGuard(tc.managedBy, tc.dirWritable)
			if managed != tc.wantManaged {
				t.Fatalf("managed = %v, want %v", managed, tc.wantManaged)
			}
			if !managed {
				if msg != "" {
					t.Fatalf("unmanaged binary returned a message: %q", msg)
				}
				return
			}
			if !strings.Contains(msg, "managed by "+tc.wantNames) {
				t.Errorf("message %q does not name %q", msg, tc.wantNames)
			}
			if !strings.Contains(msg, "ryoku update") {
				t.Errorf("message %q does not point at the package-manager path", msg)
			}
		})
	}
}

// TestDirWritable proves the writability probe matches reality: a fresh temp dir
// is writable, and a directory with no write bit is not. The read-only leg is
// skipped for root, which bypasses permission bits.
func TestDirWritable(t *testing.T) {
	writable := t.TempDir()
	if !dirWritable(writable) {
		t.Errorf("fresh temp dir reported non-writable: %s", writable)
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permission bits")
	}
	readonly := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(readonly, 0o500); err != nil {
		t.Fatal(err)
	}
	if dirWritable(readonly) {
		t.Errorf("read-only dir reported writable: %s", readonly)
	}
}
