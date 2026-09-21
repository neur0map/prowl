package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEnsureTokenConcurrentCreatorsAgree proves first-creation is atomic across
// concurrent callers: exactly one mints the bootstrap token and every other
// reads that winner instead of clobbering it with its own.
func TestEnsureTokenConcurrentCreatorsAgree(t *testing.T) {
	dir := t.TempDir()
	const n = 32
	tokens := make([]string, n)
	errs := make([]error, n)

	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			<-start
			tokens[i], errs[i] = EnsureToken(dir)
		}(i)
	}
	close(start)
	wg.Wait()

	for i := range n {
		require.NoErrorf(t, errs[i], "creator %d", i)
		require.NotEmptyf(t, tokens[i], "creator %d observed an empty token", i)
	}
	for i := 1; i < n; i++ {
		require.Equalf(t, tokens[0], tokens[i], "creator %d disagreed on the winning token", i)
	}

	onDisk, err := os.ReadFile(filepath.Join(dir, tokenFileName))
	require.NoError(t, err)
	require.Equal(t, tokens[0], strings.TrimSpace(string(onDisk)),
		"the persisted token must be the one every creator observed")
}

// TestEnsureTokenRecoversStaleEmptyToken proves an empty token file - the
// crash-window artifact of an interrupted create - is recovered rather than
// wedging the gateway forever. A fresh token is minted, and concurrent creators
// still converge on it, so recovery never yields divergent secrets.
func TestEnsureTokenRecoversStaleEmptyToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, tokenFileName)
	require.NoError(t, os.WriteFile(path, []byte(""), 0o600),
		"seed the stale empty token an interrupted create would leave behind")

	const n = 16
	tokens := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			<-start
			tokens[i], errs[i] = EnsureToken(dir)
		}(i)
	}
	close(start)
	wg.Wait()

	for i := range n {
		require.NoErrorf(t, errs[i], "creator %d", i)
		require.NotEmptyf(t, tokens[i], "creator %d recovered an empty token", i)
	}
	for i := 1; i < n; i++ {
		require.Equalf(t, tokens[0], tokens[i], "creator %d diverged after empty-file recovery", i)
	}

	onDisk, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotEmpty(t, strings.TrimSpace(string(onDisk)), "the stale empty file must have been replaced")
	require.Equal(t, tokens[0], strings.TrimSpace(string(onDisk)),
		"the recovered token every creator observed must be the one persisted")
}
