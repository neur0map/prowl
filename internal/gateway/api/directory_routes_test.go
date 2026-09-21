package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// dirRow mirrors the enriched directory row: only the fields the honesty of
// "ready" turns on.
type dirRow struct {
	ID              string `json:"id"`
	Platform        string `json:"platform"`
	Class           string `json:"class"`
	Adapter         bool   `json:"adapter"`
	Routable        bool   `json:"routable"`
	Note            string `json:"note"`
	APIKeyURL       string `json:"apiKeyUrl"`
	DocsURL         string `json:"docsUrl"`
	Ready           bool   `json:"ready"`
	Configured      bool   `json:"configured"`
	Keyless         bool   `json:"keyless"`
	KeyCount        int    `json:"keyCount"`
	EnabledKeyCount int    `json:"enabledKeyCount"`
	HealthyKeyCount int    `json:"healthyKeyCount"`
	ErrorKeyCount   int    `json:"errorKeyCount"`
	ModelCount      int    `json:"modelCount"`
}

type dirResp struct {
	Providers []dirRow `json:"providers"`
	Counts    struct {
		Ready    int `json:"ready"`
		Routable int `json:"routable"`
	} `json:"counts"`
}

func fetchDirectory(t *testing.T, s *Server, token string) dirResp {
	t.Helper()
	resp, body := do(t, s, http.MethodGet, "/api/providers/directory", "", authed(token))
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)
	var out dirResp
	require.NoError(t, json.Unmarshal([]byte(body), &out), "body was %s", body)
	return out
}

func findDirRow(t *testing.T, rows []dirRow, id string) dirRow {
	t.Helper()
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("provider %q not in directory (%d rows)", id, len(rows))
	return dirRow{}
}

// TestDirectoryReadyRequiresAdapterModelAndUsableKey proves the storefront's
// "ready" flag is honest: an adapter alone, a key with no served model, an
// erroring key, and an unprobed key do not count as routable. Only an adapter
// plus a served model plus a credential that passed a real health probe is
// ready, and the per-provider key health tally is reported alongside it.
func TestDirectoryReadyRequiresAdapterModelAndUsableKey(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: compatMachineKey})
	token := session(t, s)
	db := s.engine.DB()

	// Pick three distinct adapter-backed providers from the live directory so
	// the test does not hard-code platform names that may be renamed.
	first := fetchDirectory(t, s, token)
	seenPlatform := map[string]bool{}
	var adapters []dirRow
	var keyless dirRow
	for _, r := range first.Providers {
		if !r.Adapter || r.Platform == "" || seenPlatform[r.Platform] {
			continue
		}
		seenPlatform[r.Platform] = true
		require.False(t, r.Ready, "an empty database must not mark %s ready", r.ID)
		if r.Keyless && keyless.ID == "" {
			keyless = r
			continue
		}
		if !r.Keyless {
			adapters = append(adapters, r)
		}
	}
	require.NotEmpty(t, keyless.ID, "the directory must expose a keyless adapter")
	require.GreaterOrEqual(t, len(adapters), 4, "the directory must expose several keyed adapters")
	ready, noModel, errored, unprobed := adapters[0], adapters[1], adapters[2], adapters[3]

	// ready: adapter + served model + a healthy key.
	seedCatKey(t, db, ready.Platform, "healthy")
	seedCat(t, db, catSpec{platform: ready.Platform, modelID: "rm", name: "Ready Model", sizeLabel: "Large"})
	// noModel: adapter + a healthy key but nothing to serve.
	seedCatKey(t, db, noModel.Platform, "healthy")
	// errored: adapter + a served model but the only key is in error.
	seedCatKey(t, db, errored.Platform, "error")
	seedCat(t, db, catSpec{platform: errored.Platform, modelID: "em", name: "Errored Model", sizeLabel: "Large"})
	// unprobed: a model and enabled key exist, but neither saving a secret nor
	// public model discovery proves inference authentication succeeds.
	seedCatKey(t, db, unprobed.Platform, "unknown")
	seedCat(t, db, catSpec{platform: unprobed.Platform, modelID: "um", name: "Unprobed Model", sizeLabel: "Large"})
	// keyless: the enabled sentinel is enough because there is no secret whose
	// authentication could be falsely called healthy.
	seedCatKey(t, db, keyless.Platform, "unknown")
	seedCat(t, db, catSpec{platform: keyless.Platform, modelID: "km", name: "Keyless Model", sizeLabel: "Large"})

	got := fetchDirectory(t, s, token)

	r := findDirRow(t, got.Providers, ready.ID)
	require.True(t, r.Ready, "adapter + served model + usable key must be ready")
	require.Equal(t, 1, r.EnabledKeyCount)
	require.Equal(t, 1, r.HealthyKeyCount)
	require.Equal(t, 0, r.ErrorKeyCount)
	require.GreaterOrEqual(t, r.ModelCount, 1)

	nm := findDirRow(t, got.Providers, noModel.ID)
	require.True(t, nm.Adapter)
	require.Equal(t, 1, nm.EnabledKeyCount, "the key is on file")
	require.False(t, nm.Ready, "an adapter with a key but no served model must not be ready")

	er := findDirRow(t, got.Providers, errored.ID)
	require.Equal(t, 1, er.ErrorKeyCount)
	require.False(t, er.Ready, "a served model whose only key is in error is not routable")

	up := findDirRow(t, got.Providers, unprobed.ID)
	require.Equal(t, 1, up.EnabledKeyCount)
	require.Equal(t, 0, up.HealthyKeyCount)
	require.False(t, up.Ready, "an unprobed key is connected, not proven ready")

	kl := findDirRow(t, got.Providers, keyless.ID)
	require.True(t, kl.Keyless)
	require.True(t, kl.Ready, "an enabled keyless adapter with a model needs no health proof")

	// counts.ready must agree with the rows, and never inflate past routable.
	rowReady := 0
	for _, row := range got.Providers {
		if row.Ready {
			rowReady++
		}
	}
	require.Equal(t, rowReady, got.Counts.Ready, "counts.ready must match the ready rows")
	require.GreaterOrEqual(t, got.Counts.Ready, 1)
	require.LessOrEqual(t, got.Counts.Ready, got.Counts.Routable, "ready is a subset of routable")
}

// TestDirectoryOnlyIncludesActionableAdapters locks the storefront to things
// an operator can actually configure and route through. The underlying
// catalogue may retain non-OpenAI providers as model metadata, but the setup
// directory must not advertise an unsupported wire or duplicate one adapter
// under several upstream names.
func TestDirectoryOnlyIncludesActionableAdapters(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: compatMachineKey})
	token := session(t, s)
	dir := fetchDirectory(t, s, token)

	ids := make(map[string]struct{}, len(dir.Providers))
	platforms := make(map[string]string, len(dir.Providers))
	for _, row := range dir.Providers {
		require.True(t, row.Adapter, "%s has no inference adapter", row.ID)
		require.True(t, row.Routable, "%s is classified as an unsupported wire", row.ID)
		require.NotEmpty(t, row.Platform, "%s has no routable platform", row.ID)
		require.NotEmpty(t, row.APIKeyURL+row.DocsURL, "%s has no setup or documentation action", row.ID)
		if previous, duplicate := platforms[row.Platform]; duplicate {
			t.Fatalf("%s and %s duplicate the same %q adapter", previous, row.ID, row.Platform)
		}
		platforms[row.Platform] = row.ID
		ids[row.ID] = struct{}{}
	}
	require.Equal(t, len(dir.Providers), dir.Counts.Routable)

	for _, id := range []string{
		"google-gemini", "sail", "github-models", "github",
		"anyscale", "glhf-chat", "empower", "galadriel", "gigachat", "manus",
	} {
		_, advertised := ids[id]
		require.False(t, advertised, "%s must not be advertised without a production inference wire", id)
	}
}

// TestProviderQuotaProbeIsAnInformationalGet locks the quota read to a GET: it
// is a measurement of what a provider published, not a state change, so it must
// answer a plain read.
func TestProviderQuotaProbeIsAnInformationalGet(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: compatMachineKey})
	token := session(t, s)

	dir := fetchDirectory(t, s, token)
	var id string
	for _, r := range dir.Providers {
		if r.Adapter {
			id = r.ID
			break
		}
	}
	require.NotEmpty(t, id, "the directory must expose an adapter to probe")

	resp, body := do(t, s, http.MethodGet, "/api/providers/"+id+"/probe", "", authed(token))
	require.Equal(t, http.StatusOK, resp.StatusCode, "quota probe must be an informational GET: %s", body)

	// The old POST verb must no longer serve it: a read is not a mutation.
	post, _ := do(t, s, http.MethodPost, "/api/providers/"+id+"/probe", "", authed(token))
	require.NotEqual(t, http.StatusOK, post.StatusCode, "the quota read must not answer a POST")
}

// TestDirectoryCountsModalityOnlyProviderAsReady proves the storefront counts
// embedding and media models toward a provider's served-model total and its
// "ready" flag, not chat models alone: a provider serving only an embedding
// model, with a healthy key, is counted and ready. Before this, an
// embedding/media-only provider read as serving zero models and could never be
// ready.
func TestDirectoryCountsModalityOnlyProviderAsReady(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: compatMachineKey})
	token := session(t, s)
	db := s.engine.DB()

	var target dirRow
	for _, r := range fetchDirectory(t, s, token).Providers {
		if r.Adapter && r.Platform != "" && !r.Keyless {
			target = r
			break
		}
	}
	require.NotEmpty(t, target.ID, "need a keyed adapter-backed provider")
	require.Equal(t, 0, target.ModelCount, "provider starts with no served models")
	require.False(t, target.Ready)

	// Serve ONLY an embedding model - no chat row - plus a healthy key.
	seedCatKey(t, db, target.Platform, "healthy")
	seedEmbeddingModel(t, db, target.Platform, "emb-1", "Embed One")

	row := findDirRow(t, fetchDirectory(t, s, token).Providers, target.ID)
	require.GreaterOrEqual(t, row.ModelCount, 1, "an embedding-only provider must be counted")
	require.True(t, row.Ready, "adapter + served embedding model + healthy key must be ready")
}
