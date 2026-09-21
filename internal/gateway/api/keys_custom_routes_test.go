package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/neur0map/prowl/internal/gateway"
)

// keysCustomFakeUpstream is a stand-in for a user's OpenAI-compatible endpoint. Every test
// drives it over loopback; the SSRF guard allows loopback by default (local
// Ollama is the documented use case), so no test ever touches the network.
func keysCustomFakeUpstream(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func decodeMap(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &m), "body was %q", body)
	return m
}

func customModelDBID(t *testing.T, s *Server, modelID, scope string) int64 {
	t.Helper()
	var id int64
	err := s.engine.DB().QueryRow(
		`SELECT id FROM models WHERE platform='custom' AND model_id=? AND endpoint_scope=?`,
		modelID, scope).Scan(&id)
	require.NoError(t, err, "model %q at %q must exist", modelID, scope)
	return id
}

func customModelCount(t *testing.T, s *Server, scope string) int {
	t.Helper()
	var n int
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT COUNT(*) FROM models WHERE platform='custom' AND endpoint_scope=?`, scope).Scan(&n))
	return n
}

// TestDiscoverListsUpstreamModels: discovery reads the endpoint's own /models
// and returns the ids it serves, with the metadata chips the picker renders.
func TestDiscoverListsUpstreamModels(t *testing.T) {
	t.Parallel()
	up := keysCustomFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/models", r.URL.Path)
		_, _ = w.Write([]byte(`{"data":[
			{"id":"gpt-y"},
			{"id":"gpt-x","owned_by":"acme","context_length":8192}
		]}`))
	})
	s := testServer(t, Options{})
	token := session(t, s)

	resp, body := do(t, s, http.MethodPost, "/api/keys/custom/discover-models",
		`{"baseUrl":"`+up.URL+`"}`, authed(token))
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	out := decodeMap(t, body)
	require.EqualValues(t, 2, out["total"])
	models := out["models"].([]any)
	require.Len(t, models, 2)
	// Sorted by id: gpt-x before gpt-y.
	first := models[0].(map[string]any)
	require.Equal(t, "gpt-x", first["id"])
	require.Equal(t, "acme", first["ownedBy"])
	require.EqualValues(t, 8192, first["contextWindow"])
}

// TestDiscoverRejectsUnreadableCatalog: an endpoint that answers HTML (a login
// page or error page) has not "served no models", it returned something we
// cannot read - that is an upstream error, never a fabricated empty success, and
// nothing is written to the catalog.
func TestDiscoverRejectsUnreadableCatalog(t *testing.T) {
	t.Parallel()
	up := keysCustomFakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body>login</body></html>`))
	})
	s := testServer(t, Options{})
	token := session(t, s)

	resp, body := do(t, s, http.MethodPost, "/api/keys/custom/discover-models",
		`{"baseUrl":"`+up.URL+`"}`, authed(token))
	require.Equal(t, http.StatusBadGateway, resp.StatusCode, body)
	require.Equal(t, string(TypeUpstream), errorType(t, body))

	var n int
	require.NoError(t, s.engine.DB().QueryRow(`SELECT COUNT(*) FROM models`).Scan(&n))
	require.Zero(t, n, "a malformed catalog must never insert a model")
}

// TestDiscoverBoundsAndValidatesModelIDs: a remote /models names rows in our
// database, so the list is bounded and over-long ids are skipped rather than
// inserted (discovery output is untrusted input).
func TestDiscoverBoundsAndValidatesModelIDs(t *testing.T) {
	t.Parallel()
	oversizedID := strings.Repeat("x", 300)
	up := keysCustomFakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		var b strings.Builder
		b.WriteString(`{"data":[{"id":"` + oversizedID + `"}`)
		for i := range 600 {
			b.WriteString(fmt.Sprintf(`,{"id":"model-%03d"}`, i))
		}
		b.WriteString(`]}`)
		_, _ = w.Write([]byte(b.String()))
	})
	s := testServer(t, Options{})
	token := session(t, s)

	resp, body := do(t, s, http.MethodPost, "/api/keys/custom/discover-models",
		`{"baseUrl":"`+up.URL+`"}`, authed(token))
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	out := decodeMap(t, body)
	models := out["models"].([]any)
	require.Len(t, models, keysCustomMaxDiscovered, "the returned list must be capped")
	require.NotContains(t, body, oversizedID, "an over-long id must be dropped, not returned")
}

// TestProbeReportsSuccess: a probe against a working endpoint returns the model
// it measured and a latency, and records a success sample the analytics surface
// can read.
func TestProbeReportsSuccess(t *testing.T) {
	t.Parallel()
	up := keysCustomFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/chat/completions", r.URL.Path)
		_, _ = w.Write([]byte(`{"model":"m1","choices":[{"message":{"content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	s := testServer(t, Options{})
	token := session(t, s)

	// Register the endpoint + a model so the probe has one to measure.
	resp, body := do(t, s, http.MethodPost, "/api/keys/custom",
		`{"baseUrl":"`+up.URL+`","models":["m1"],"apiKey":"sk-probe"}`, authed(token))
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	keyID := int64(decodeMap(t, body)["keyId"].(float64))

	resp, body = do(t, s, http.MethodPost, "/api/keys/custom/probe",
		fmt.Sprintf(`{"keyId":%d}`, keyID), authed(token))
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	out := decodeMap(t, body)
	require.Equal(t, "m1", out["modelId"])
	_, ok := out["latencyMs"]
	require.True(t, ok, "a successful probe reports a latency")

	// The success sample was recorded against the endpoint.
	var n int
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT COUNT(*) FROM requests WHERE platform='custom' AND model_id='m1' AND outcome='success'`).Scan(&n))
	require.Equal(t, 1, n, "a successful probe records exactly one sample")
}

// TestProbeRelayed401IsNotAnAuthError is the trap the whole surface turns on: a
// provider-confirmed bad key is relayed with its 401 intact but MUST NOT carry
// authentication_error - that single combination signs the operator out, and
// typing a bad key must never do it.
func TestProbeRelayed401IsNotAnAuthError(t *testing.T) {
	t.Parallel()
	up := keysCustomFakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid API key"}}`))
	})
	s := testServer(t, Options{})
	token := session(t, s)

	resp, body := do(t, s, http.MethodPost, "/api/keys/custom/probe",
		`{"baseUrl":"`+up.URL+`","apiKey":"sk-bad"}`, authed(token))
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, body)
	require.NotEqual(t, string(TypeAuthentication), errorType(t, body),
		"a relayed upstream 401 must not sign the operator out")
	require.Equal(t, string(TypeUpstream), errorType(t, body))
}

// TestProbeDeadAddressIsAProbeFailure: an unreachable endpoint is reported as an
// upstream/probe failure, not as a credential rejection.
func TestProbeDeadAddressIsAProbeFailure(t *testing.T) {
	t.Parallel()
	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead := down.URL
	down.Close()

	s := testServer(t, Options{})
	token := session(t, s)

	resp, body := do(t, s, http.MethodPost, "/api/keys/custom/probe",
		`{"baseUrl":"`+dead+`","apiKey":"sk-x"}`, authed(token))
	require.Equal(t, http.StatusBadGateway, resp.StatusCode, body)
	require.Equal(t, string(TypeUpstream), errorType(t, body))
	require.NotEqual(t, string(TypeAuthentication), errorType(t, body))
}

// TestProbeHasABoundedTimeout: a hung endpoint does not hang the probe. The
// interactive timeout bounds it and the caller gets a clean upstream failure.
func TestProbeHasABoundedTimeout(t *testing.T) {
	// Not parallel: it briefly shortens the package-level probe timeout.
	prev := keysCustomProbeTimeout
	keysCustomProbeTimeout = 100 * time.Millisecond
	defer func() { keysCustomProbeTimeout = prev }()

	up := keysCustomFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	})
	s := testServer(t, Options{})
	token := session(t, s)

	resp, body := do(t, s, http.MethodPost, "/api/keys/custom",
		`{"baseUrl":"`+up.URL+`","models":["m1"],"apiKey":"sk-x"}`, authed(token))
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	keyID := int64(decodeMap(t, body)["keyId"].(float64))

	start := time.Now()
	resp, body = do(t, s, http.MethodPost, "/api/keys/custom/probe",
		fmt.Sprintf(`{"keyId":%d}`, keyID), authed(token))
	elapsed := time.Since(start)

	require.Less(t, elapsed, 3*time.Second, "the probe must not wait for the hung endpoint")
	require.Equal(t, http.StatusBadGateway, resp.StatusCode, body)
	require.Equal(t, string(TypeUpstream), errorType(t, body))
}

// TestProbeSelectsChatModelFromMixedCatalog: discovery is sorted by id and can
// mix modalities, so an embedding id may lead the list. The /chat/completions
// probe must skip past it to the first chat-compatible model - probing the
// embedding endpoint would fail a working key for no reason.
func TestProbeSelectsChatModelFromMixedCatalog(t *testing.T) {
	t.Parallel()
	up := keysCustomFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			// Sorted by id, the embedding model leads and the chat model trails.
			_, _ = w.Write([]byte(`{"data":[{"id":"aaa-embedding"},{"id":"zzz-gpt"}]}`))
		case "/chat/completions":
			// Echo the requested model so the response proves which id was probed.
			var body struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, _ = w.Write([]byte(`{"model":"` + body.Model + `","choices":[{"message":{"content":"pong"},"finish_reason":"stop"}]}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	})
	s := testServer(t, Options{})
	token := session(t, s)

	resp, body := do(t, s, http.MethodPost, "/api/keys/custom/probe",
		`{"baseUrl":"`+up.URL+`","apiKey":"sk-x"}`, authed(token))
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Equal(t, "zzz-gpt", decodeMap(t, body)["modelId"],
		"the probe must select the chat model, not the alphabetically-first embedding model")
}

// TestProbeAllNonChatCatalogMakesNoChatRequest: when every discovered model is a
// non-chat (embedding/media) model there is nothing to chat-probe. The probe
// must NOT fire /chat/completions at a non-chat endpoint; it returns a specific,
// honest no-chat-model error instead.
func TestProbeAllNonChatCatalogMakesNoChatRequest(t *testing.T) {
	t.Parallel()
	up := keysCustomFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"text-embedding-3-small"},{"id":"whisper-1"},{"id":"flux-1-dev"}]}`))
		case "/chat/completions":
			t.Errorf("a chat probe must never fire when the endpoint serves no chat model")
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	})
	s := testServer(t, Options{})
	token := session(t, s)

	resp, body := do(t, s, http.MethodPost, "/api/keys/custom/probe",
		`{"baseUrl":"`+up.URL+`","apiKey":"sk-x"}`, authed(token))
	require.Equal(t, http.StatusBadGateway, resp.StatusCode, body)
	require.Equal(t, string(TypeUpstream), errorType(t, body))
	require.Contains(t, body, "no chat model",
		"the operator must be told the endpoint serves no chat model")
}

// TestImportSelectedCreatesModelsIdempotently: importing a custom endpoint with
// a model list creates exactly those models, and a second identical import adds
// nothing and - critically - leaves every models.id unchanged, because
// fallback_config and profile_models address a model by id (trap 5).
func TestImportSelectedCreatesModelsIdempotently(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})
	token := session(t, s)

	base := "http://127.0.0.1:9/v1"
	scope := normalizeBaseURL(base)
	payload := `{"keys":[{"keyName":"ep","keyValue":"sk-imp","platform":"custom","baseUrl":"` + base +
		`","models":[{"id":"a"},{"id":"b"}]}]}`

	resp, body := do(t, s, http.MethodPost, "/api/keys/import-selected", payload, authed(token))
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	out := decodeMap(t, body)
	require.EqualValues(t, 1, out["imported"])
	require.EqualValues(t, 2, out["modelsRegistered"])
	require.Empty(t, out["errors"].([]any))
	require.Equal(t, 2, customModelCount(t, s, scope))

	idA := customModelDBID(t, s, "a", scope)
	idB := customModelDBID(t, s, "b", scope)

	// A second identical import must be idempotent: no duplicate keys, no
	// duplicate models, and the same ids.
	resp, body = do(t, s, http.MethodPost, "/api/keys/import-selected", payload, authed(token))
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Equal(t, 2, customModelCount(t, s, scope), "a re-import must not duplicate models")
	require.Equal(t, idA, customModelDBID(t, s, "a", scope), "import must not renumber models.id")
	require.Equal(t, idB, customModelDBID(t, s, "b", scope), "import must not renumber models.id")

	var keyCount int
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT COUNT(*) FROM api_keys WHERE platform='custom' AND base_url=?`, scope).Scan(&keyCount))
	require.Equal(t, 1, keyCount, "re-importing the same key must not duplicate the credential")

	// The models must be routable via the global chain.
	var inChain int
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT COUNT(*) FROM fallback_config WHERE model_db_id IN (?,?)`, idA, idB).Scan(&inChain))
	require.Equal(t, 2, inChain, "registered models must join the fallback chain")
}

// TestCustomRegisterIsAnUpsert: re-registering a model through POST /custom
// updates its row in place (created:0) and preserves its id.
func TestCustomRegisterIsAnUpsert(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})
	token := session(t, s)

	base := "http://127.0.0.1:9/v1"
	scope := normalizeBaseURL(base)

	resp, body := do(t, s, http.MethodPost, "/api/keys/custom",
		`{"baseUrl":"`+base+`","models":["m1","m2"],"apiKey":"sk-c"}`, authed(token))
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	out := decodeMap(t, body)
	require.EqualValues(t, 2, out["created"])
	id1 := customModelDBID(t, s, "m1", scope)

	resp, body = do(t, s, http.MethodPost, "/api/keys/custom",
		`{"baseUrl":"`+base+`","models":["m1","m2"],"apiKey":"sk-c"}`, authed(token))
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	out = decodeMap(t, body)
	require.EqualValues(t, 0, out["created"], "a re-register creates nothing new")
	require.EqualValues(t, 2, out["alreadyRegistered"])
	require.Equal(t, id1, customModelDBID(t, s, "m1", scope), "an upsert must not renumber models.id")
	require.Equal(t, 2, customModelCount(t, s, scope))
}

// TestSubmittedKeyNeverAppearsInResponseOrError proves the credential the
// operator submits is never echoed back - masked in a success body, and scrubbed
// from a relayed error even when the upstream echoes it verbatim.
func TestSubmittedKeyNeverAppearsInResponseOrError(t *testing.T) {
	t.Parallel()
	// A token that matches no known key shape, so only exact-literal redaction
	// can remove it - the strongest form of the guarantee.
	const secret = "zzleakzz-endpoint-000111222"

	up := keysCustomFakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"rejected key ` + secret + `"}}`))
	})
	s := testServer(t, Options{})
	token := session(t, s)

	// Success body: the masked preview, never the plaintext.
	resp, body := do(t, s, http.MethodPost, "/api/keys/custom",
		`{"baseUrl":"http://127.0.0.1:9/v1","models":["m1"],"apiKey":"`+secret+`"}`, authed(token))
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	require.NotContains(t, body, secret, "POST /custom must not echo the submitted key")

	// Relayed-error bodies: even an upstream that echoes the key must not leak.
	for _, path := range []string{"/api/keys/custom/discover-models", "/api/keys/custom/probe"} {
		resp, body := do(t, s, http.MethodPost, path,
			`{"baseUrl":"`+up.URL+`","apiKey":"`+secret+`"}`, authed(token))
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode, body)
		require.NotContains(t, body, secret, "%s must scrub the submitted key from a relayed error", path)
	}

	// import-selected: neither the custom nor the plain path echoes the value.
	resp, body = do(t, s, http.MethodPost, "/api/keys/import-selected",
		`{"keys":[{"keyName":"ep","keyValue":"`+secret+`","platform":"custom","baseUrl":"http://127.0.0.1:9/v2"}]}`,
		authed(token))
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NotContains(t, body, secret, "import-selected must not echo the submitted key")
}

// TestPreviewParsesExportAndEnvFiles: the import round-trip's first call parses
// uploaded key files into candidate rows the client can then import, detecting
// platforms and folding a CUSTOM_n endpoint pair into one custom row.
func TestPreviewParsesExportAndEnvFiles(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})
	token := session(t, s)

	envFile := "GROQ_API_KEY=gsk_abcdefabcdef\n" +
		"CUSTOM_1_BASE_URL=http://localhost:1234/v1\n" +
		"CUSTOM_1_KEY=sk-localsecret9\n"
	exportFile := `{"version":1,"keys":[{"platform":"openrouter","key":"sk-or-xyz123","label":"prod"}]}`

	ctype, mbody := multipartFiles(t, map[string]struct{ name, content string }{
		"a": {"keys.env", envFile},
		"b": {"export.json", exportFile},
	})
	resp, body := do(t, s, http.MethodPost, "/api/keys/preview", mbody, map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  ctype,
	})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	out := decodeMap(t, body)
	keys := out["keys"].([]any)
	byPlatform := map[string]map[string]any{}
	for _, k := range keys {
		m := k.(map[string]any)
		if p, ok := m["detectedPlatform"].(string); ok {
			byPlatform[p] = m
		}
	}
	require.Contains(t, byPlatform, "groq")
	require.Contains(t, byPlatform, "openrouter")
	require.Contains(t, byPlatform, "custom")
	require.Equal(t, "http://localhost:1234/v1", byPlatform["custom"]["baseUrl"],
		"a CUSTOM_n pair must fold into one custom row carrying its endpoint")
	require.Equal(t, "prod", byPlatform["openrouter"]["keyName"],
		"the export label becomes the key name")
}

// multipartFiles builds a multipart/form-data body with each file under the
// "files" field, matching what the dashboard's import form uploads.
func multipartFiles(t *testing.T, files map[string]struct{ name, content string }) (contentType, body string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range files {
		fw, err := mw.CreateFormFile("files", f.name)
		require.NoError(t, err)
		_, err = fw.Write([]byte(f.content))
		require.NoError(t, err)
	}
	require.NoError(t, mw.Close())
	return mw.FormDataContentType(), buf.String()
}

// TestBlockedMetadataURLIsRejected: a base_url resolving to a cloud metadata
// address is refused before any request is made - the SSRF guard, not a probe
// failure.
func TestBlockedMetadataURLIsRejected(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})
	token := session(t, s)

	resp, body := do(t, s, http.MethodPost, "/api/keys/custom/discover-models",
		`{"baseUrl":"http://169.254.169.254/latest/meta-data/"}`, authed(token))
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
	require.Contains(t, strings.ToLower(body), "metadata")
}

// TestCustomSurfaceRequiresACredential keeps the whole custom surface gated:
// no credential, no mutation. The 401 must not carry authentication_error -
// this surface has no session for such a verdict to end.
func TestCustomSurfaceRequiresACredential(t *testing.T) {
	t.Parallel()
	s := bareServer(t)
	for _, path := range []string{
		"/api/keys/custom", "/api/keys/custom/probe",
		"/api/keys/custom/discover-models", "/api/keys/import-selected",
	} {
		resp, body := do(t, s, http.MethodPost, path, `{}`, nil)
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode, path)
		require.NotEqual(t, string(TypeAuthentication), errorType(t, body), path)
	}
}

// TestPatchBaseURLIsGuardedAgainstSSRF: editing an existing key's base_url runs
// the same SSRF check as creating one - otherwise edit is the way around the
// create-time guard - and a refusal must not write.
func TestPatchBaseURLIsGuardedAgainstSSRF(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})
	token := session(t, s)

	const safe = "http://127.0.0.1:1234/v1"
	id, err := s.engine.Vault().Add("custom", "sk-edit", gateway.AddOptions{BaseURL: safe})
	require.NoError(t, err)

	resp, body := do(t, s, http.MethodPatch, keyPath(id, ""),
		`{"baseUrl":"http://169.254.169.254/latest/meta-data/"}`, authed(token))
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
	require.Equal(t, string(TypeInvalidRequest), errorType(t, body))
	require.NotEqual(t, string(TypeAuthentication), errorType(t, body),
		"an SSRF refusal must not sign the operator out")

	// A refusal that still writes is the bug wearing a different hat.
	var stored sql.NullString
	require.NoError(t, s.engine.DB().QueryRow(`SELECT base_url FROM api_keys WHERE id=?`, id).Scan(&stored))
	require.Equal(t, safe, stored.String, "a refused PATCH must leave the stored base_url unchanged")
}

// TestBlockPrivateEnvVarIsHonoured proves the private-address switch is read
// from the renamed PROWL_BLOCK_PRIVATE_PROVIDER_URLS (no upstream brand leak).
func TestBlockPrivateEnvVarIsHonoured(t *testing.T) {
	t.Setenv("PROWL_BLOCK_PRIVATE_PROVIDER_URLS", "1")
	ok, reason := keysCustomAssessURL("http://127.0.0.1:1234/v1")
	require.False(t, ok, "loopback must be refused when the switch is set")
	require.Contains(t, reason, "private")
}
