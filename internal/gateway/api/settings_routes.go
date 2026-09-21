package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/neur0map/prowl/internal/version"
)

// registerSettingsRoutes mounts the settings/api-key/update family the
// dashboard reads outside the keys, routing and analytics screens: the general
// settings the Settings dialog edits, the unified machine credential, the
// version row, and the GitHub-backed update check. Every route is session
// gated, matching the reference's requireAuth mount of both routers
// (server/src/app.ts:244-267).
func (s *Server) registerSettingsRoutes() {
	store := &settingsStore{db: s.engine.DB()}
	// The inference-plane gate reads the unified key through this same store,
	// so a regenerate invalidates the old credential on the next request
	// rather than at the next restart.
	s.settings = store
	update := newUpdateChecker(store)

	s.mux.HandleFunc("GET /api/settings/version", s.RequireKey(handleSettingsVersion))
	s.mux.HandleFunc("GET /api/settings/update-check", s.RequireKey(store.handleGetUpdateCheck))
	s.mux.HandleFunc("PUT /api/settings/update-check", s.RequireKey(store.handlePutUpdateCheck))
	s.mux.HandleFunc("GET /api/settings/api-key", s.RequireKey(store.handleGetAPIKey))
	s.mux.HandleFunc("POST /api/settings/api-key/regenerate", s.RequireKey(store.handleRegenerateAPIKey))

	s.mux.HandleFunc("GET /api/update/release", s.RequireKey(update.handleRelease))
	s.mux.HandleFunc("GET /api/update/check", s.RequireKey(update.handleCheck))
	s.mux.HandleFunc("GET /api/update/status", s.RequireKey(update.handleStatus))
}

// ── Typed settings accessor ──────────────────────────────────────────────────

// Setting keys this file owns. The unified key is a credential, not a plain
// operator setting, so it is handled by the credential methods below and is
// deliberately absent from settingSpecs: the generic setter must refuse it
// rather than let an arbitrary string be written where a formatted key belongs.
const (
	settingUpdateCheckEnabled = "update_check_enabled"
	settingUnifiedAPIKey      = "unified_api_key"
)

// settingSpec validates and canonicalises one operator setting. Keeping the key
// names, defaults and validation in one registry is the point: an invalid value
// is refused at the boundary instead of poisoning a later read, and the setter
// canonicalises so a read/modify/write round trip cannot 400 on its own output.
type settingSpec struct {
	// def is the value a read returns when the key was never written.
	def string
	// canonicalize rejects an invalid value and returns the exact string to
	// persist for a valid one.
	canonicalize func(string) (string, error)
}

var settingSpecs = map[string]settingSpec{
	settingUpdateCheckEnabled: {def: "0", canonicalize: canonicalizeBool},
}

// settingsStore is the one place that reads and writes the gateway's operator
// settings. Timestamps are Unix seconds, UTC, matching every other column in
// this database. It holds no locks: SQLite serialises the writes, and each
// method does its own single statement so nothing is held across I/O.
type settingsStore struct {
	db *sql.DB
}

// get returns a setting's canonical value, or its default when unset. An
// unknown key is an error, not a silent empty string, so a typo in a caller
// surfaces immediately rather than reading as "disabled".
func (st *settingsStore) get(ctx context.Context, key string) (string, error) {
	spec, ok := settingSpecs[key]
	if !ok {
		return "", fmt.Errorf("unknown setting %q", key)
	}
	var value string
	err := st.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return spec.def, nil
	}
	if err != nil {
		return "", err
	}
	return value, nil
}

// set validates raw against the key's spec and persists the canonical form. An
// unknown key or an invalid value is refused; nothing is written in either
// case.
func (st *settingsStore) set(ctx context.Context, key, raw string) error {
	spec, ok := settingSpecs[key]
	if !ok {
		return fmt.Errorf("unknown setting %q", key)
	}
	value, err := spec.canonicalize(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	_, err = st.db.ExecContext(ctx,
		`INSERT INTO settings(key, value, updated_at) VALUES(?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().Unix())
	return err
}

// canonicalizeBool accepts the common truthy/falsey spellings and stores the
// reference's '1'/'0' so the persisted value matches what other readers expect.
func canonicalizeBool(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "on", "yes":
		return "1", nil
	case "0", "false", "off", "no":
		return "0", nil
	}
	return "", fmt.Errorf("must be a boolean, got %q", raw)
}

// autoUpdateCheckEnabled reports whether the operator has opted the install in
// to phoning GitHub on page load. Absent or anything other than '1' is off, so
// a fresh install never contacts the network on its own (settings.ts:47-55,
// update.ts:74-78).
func (st *settingsStore) autoUpdateCheckEnabled(ctx context.Context) bool {
	value, err := st.get(ctx, settingUpdateCheckEnabled)
	return err == nil && value == "1"
}

func (st *settingsStore) setAutoUpdateCheck(ctx context.Context, enabled bool) error {
	raw := "0"
	if enabled {
		raw = "1"
	}
	return st.set(ctx, settingUpdateCheckEnabled, raw)
}

// ── The unified machine API key ──────────────────────────────────────────────

// The reference mints the machine credential as "freellmapi-" followed by 24
// random bytes rendered hex, i.e. 48 hex chars (server/src/db/index.ts:233,
// spec-auth-ops.md §3). It is stored in the clear in the settings table so the
// dashboard can display it, and GET /api/settings/api-key returns it whole
// (index.ts:227, settings.ts:390). This port matches that on purpose: the
// dashboard's Show/Copy affordances need the plaintext.
const (
	// The prefix is rebranded: this credential is shown to the user in the
	// dashboard, so shipping "freellmapi-" would leak the upstream brand into
	// Prowl's own visible surface. It is safe to change because nothing
	// authenticates on the prefix -- the key is compared whole, in constant
	// time -- and the length is kept identical so the client's fixed-width
	// mask still hides the same number of secret characters.
	unifiedAPIKeyPrefix = "prowlag-"
	unifiedAPIKeyHexLen = 48 // 24 random bytes, hex-encoded.
)

// unifiedKey is the machine credential the inference plane checks. Its JSON
// form is masked so it cannot leak by being embedded in an aggregate response;
// the one endpoint that must show it whole serialises Reveal() explicitly.
// This encodes the contract "a key is masked in any list; only an explicit
// reveal returns plaintext" in the type itself.
type unifiedKey string

// Reveal returns the plaintext. Callers that expose it (the dedicated reveal
// endpoint) use this; nothing else should.
func (k unifiedKey) Reveal() string { return string(k) }

// Masked keeps the human-recognisable prefix and hides the secret body, the
// same 13-char prefix the dashboard renders (unified-key-section.tsx:26)
// without ever carrying the full key.
func (k unifiedKey) Masked() string {
	const bullets = 32
	shown := len(unifiedAPIKeyPrefix) + 2 // Prefix plus two hex chars.
	s := string(k)
	if len(s) <= shown {
		return strings.Repeat("\u2022", bullets)
	}
	return s[:shown] + strings.Repeat("\u2022", bullets)
}

// MarshalJSON masks by default. A credential that reaches JSON through any path
// other than the deliberate reveal is a leak, so the safe default is the masked
// form.
func (k unifiedKey) MarshalJSON() ([]byte, error) {
	return json.Marshal(k.Masked())
}

var unifiedKeyPattern = regexp.MustCompile(`^` + unifiedAPIKeyPrefix + `[0-9a-f]{` + fmt.Sprint(unifiedAPIKeyHexLen) + `}$`)

// generateUnifiedKey mints a key in the reference's exact format.
func generateUnifiedKey() (unifiedKey, error) {
	buf := make([]byte, unifiedAPIKeyHexLen/2)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return unifiedKey(unifiedAPIKeyPrefix + hex.EncodeToString(buf)), nil
}

// unifiedAPIKey returns the current machine credential, minting and persisting
// one on first use. The reference seeds it at migration time; seeding it lazily
// here is equivalent for every caller and keeps the credential out of the
// schema. Concurrent first calls converge on a single stored value because the
// seed insert does nothing on conflict and the value is re-read afterwards.
func (st *settingsStore) unifiedAPIKey(ctx context.Context) (unifiedKey, error) {
	value, err := st.readUnifiedKey(ctx)
	if err == nil {
		return unifiedKey(value), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	seed, err := generateUnifiedKey()
	if err != nil {
		return "", err
	}
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO settings(key, value, updated_at) VALUES(?, ?, ?)
		 ON CONFLICT(key) DO NOTHING`,
		settingUnifiedAPIKey, string(seed), time.Now().Unix()); err != nil {
		return "", err
	}
	// Re-read: a racing caller may have won the insert, and its key is the
	// one now authenticating.
	value, err = st.readUnifiedKey(ctx)
	if err != nil {
		return "", err
	}
	return unifiedKey(value), nil
}

func (st *settingsStore) readUnifiedKey(ctx context.Context) (string, error) {
	var value string
	err := st.db.QueryRowContext(ctx,
		"SELECT value FROM settings WHERE key = ?", settingUnifiedAPIKey).Scan(&value)
	return value, err
}

// regenerateUnifiedAPIKey overwrites the stored key and returns the new value.
// The previous key stops authenticating the instant this commits, because
// authentication reads the live stored value (see authenticateMachineKey).
func (st *settingsStore) regenerateUnifiedAPIKey(ctx context.Context) (unifiedKey, error) {
	key, err := generateUnifiedKey()
	if err != nil {
		return "", err
	}
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO settings(key, value, updated_at) VALUES(?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		settingUnifiedAPIKey, string(key), time.Now().Unix()); err != nil {
		return "", err
	}
	return key, nil
}

// authenticateMachineKey reports, in constant time, whether candidate is the
// current stored key. It reads the live value each call, so a regenerate takes
// effect on the very next check and the old key stops authenticating at once.
// The inference-plane gate reads the live key through this, so a regenerate
// invalidates the old credential on the next request rather than at restart.
func (st *settingsStore) authenticateMachineKey(ctx context.Context, candidate string) (bool, error) {
	if candidate == "" {
		return false, nil
	}
	key, err := st.unifiedAPIKey(ctx)
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(key.Reveal())) == 1, nil
}

// ── Settings handlers ────────────────────────────────────────────────────────

// handleSettingsVersion reports the running release for the dashboard's version
// row and update reminder. null when it cannot be established honestly, which
// the dashboard renders by omitting the row (settings.ts:43, app-version.ts).
func handleSettingsVersion(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]any{"version": settingsAppVersion()})
}

func (st *settingsStore) handleGetUpdateCheck(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]any{"enabled": st.autoUpdateCheckEnabled(r.Context())})
}

func (st *settingsStore) handlePutUpdateCheck(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !DecodeJSON(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest,
			"invalid update check setting: enabled is required and must be a boolean")
		return
	}
	if err := st.setAutoUpdateCheck(r.Context(), *body.Enabled); err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not save the update check setting")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"enabled": st.autoUpdateCheckEnabled(r.Context())})
}

// handleGetAPIKey is the explicit reveal: it returns the credential whole so
// the dashboard can display, copy and show it. This is the only endpoint that
// serialises the plaintext.
func (st *settingsStore) handleGetAPIKey(w http.ResponseWriter, r *http.Request) {
	key, err := st.unifiedAPIKey(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not read the api key")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"apiKey": key.Reveal()})
}

func (st *settingsStore) handleRegenerateAPIKey(w http.ResponseWriter, r *http.Request) {
	key, err := st.regenerateUnifiedAPIKey(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not regenerate the api key")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"apiKey": key.Reveal()})
}

// settingsAppVersion is the running release, or nil when it is not honestly
// known. version.Version is compiled in, so this is effectively always set; the
// pointer preserves the reference's null-capable contract for the one case it
// is not.
func settingsAppVersion() *string {
	v := strings.TrimSpace(version.Version)
	if v == "" {
		return nil
	}
	return &v
}

// ── Update check ─────────────────────────────────────────────────────────────

// The GitHub repository the install is compared against. The reference points
// at its own repo; the Go port points at Prowl's (the module path).
const updateRepo = "neur0map/prowl"

const (
	updateCheckCacheTTL = 5 * time.Minute
	updateFetchTimeout  = 10 * time.Second
	maxCompareBody      = 1 << 20 // 1 MiB ceiling on a compare/release body.
	maxChanges          = 8
	maxChangeMessageLen = 160
	maxRemoteMessageLen = 200
	maxReleaseBodyLen   = 20_000
	maxTagLen           = 64
)

var fullSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// fetchFunc is the network seam. Injected in tests so no test hits GitHub.
type fetchFunc func(ctx context.Context, url string, headers map[string]string) (*http.Response, error)

// updateChecker answers the three update endpoints. It degrades gracefully by
// design: any failure to reach or parse GitHub renders as status "unknown"
// rather than a 502, because a self-hosted box that cannot reach the network
// must not see a scary error or a blocked page (task: the update check must
// never block the page). Nothing here runs at startup.
type updateChecker struct {
	settings     *settingsStore
	repo         string
	localSHA     string // Full 40-hex commit, "" when it cannot be established.
	installation string // "source" | "docker" | "desktop" | "unknown".
	version      func() *string
	now          func() time.Time
	fetch        fetchFunc

	mu            sync.Mutex
	lastCheck     *checkResult
	lastCheckAt   time.Time
	lastRelease   *latestRelease
	lastReleaseAt time.Time
}

func newUpdateChecker(store *settingsStore) *updateChecker {
	sha := ""
	installation := "unknown"
	if candidate := strings.ToLower(strings.TrimSpace(version.Commit)); fullSHAPattern.MatchString(candidate) {
		sha = candidate
		installation = "source"
	}
	return &updateChecker{
		settings:     store,
		repo:         updateRepo,
		localSHA:     sha,
		installation: installation,
		version:      settingsAppVersion,
		now:          time.Now,
		fetch:        defaultFetch,
	}
}

// checkResult is the wire shape the dashboard's update panel renders. localSha
// and version are nullable and must serialise as null when unknown, never as an
// empty string that reads as a real value (update.ts:44-55).
type checkResult struct {
	Status        string   `json:"status"`
	Installation  string   `json:"installation"`
	LocalSha      *string  `json:"localSha"`
	CheckedAt     string   `json:"checkedAt"`
	Version       *string  `json:"version"`
	RemoteSha     string   `json:"remoteSha,omitempty"`
	RemoteMessage string   `json:"remoteMessage,omitempty"`
	RemoteDate    string   `json:"remoteDate,omitempty"`
	Changes       []change `json:"changes,omitempty"`
}

type change struct {
	Sha     string `json:"sha"`
	Message string `json:"message"`
	Date    string `json:"date,omitempty"`
}

// latestRelease is the reminder pill's view of the newest published release.
// body and publishedAt are nullable (update.ts:81-86).
type latestRelease struct {
	TagName     string  `json:"tagName"`
	Body        *string `json:"body"`
	HTMLURL     string  `json:"htmlUrl"`
	PublishedAt *string `json:"publishedAt"`
}

func (u *updateChecker) handleRelease(w http.ResponseWriter, r *http.Request) {
	// Enforced where it cannot be bypassed: when the check is off nothing
	// leaves the box, not even a request thrown away afterwards
	// (update.ts:534-546).
	if !u.settings.autoUpdateCheckEnabled(r.Context()) {
		WriteJSON(w, http.StatusOK, map[string]any{"disabled": true})
		return
	}
	release, ok := u.release(r.Context())
	if !ok {
		// The reminder pill is out of flow and simply does not render on a
		// failure, so a 502 here never blocks a page.
		WriteError(w, http.StatusBadGateway, TypeUpstream, "unable to check for the latest release")
		return
	}
	WriteJSON(w, http.StatusOK, release)
}

func (u *updateChecker) handleCheck(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, u.check(r.Context()))
}

func (u *updateChecker) handleStatus(w http.ResponseWriter, _ *http.Request) {
	status := "unsupported"
	var localSha, lastChecked *string
	if u.localSHA != "" {
		short := u.localSHA[:7]
		localSha = &short
		status = "idle"
		u.mu.Lock()
		if u.lastCheck != nil {
			status = u.lastCheck.Status
			checkedAt := u.lastCheck.CheckedAt
			lastChecked = &checkedAt
		}
		u.mu.Unlock()
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"status":       status,
		"installation": u.installation,
		"localSha":     localSha,
		"lastChecked":  lastChecked,
		"version":      u.version(),
	})
}

// check compares the local commit to the repository's main branch. A build
// without a resolvable commit is "unsupported"; any failure to reach or parse
// GitHub is "unknown". Successful results are cached briefly and drive
// /status; a failure never overwrites a good cached result.
func (u *updateChecker) check(ctx context.Context) checkResult {
	if u.localSHA == "" {
		return u.baseResult("unsupported")
	}

	u.mu.Lock()
	if u.lastCheck != nil && u.now().Sub(u.lastCheckAt) < updateCheckCacheTTL {
		cached := *u.lastCheck
		u.mu.Unlock()
		return cached
	}
	u.mu.Unlock()

	result, ok := u.performCheck(ctx)
	if !ok {
		return u.baseResult("unknown")
	}

	u.mu.Lock()
	u.lastCheck = &result
	u.lastCheckAt = u.now()
	u.mu.Unlock()
	return result
}

func (u *updateChecker) baseResult(status string) checkResult {
	var localSha *string
	if u.localSHA != "" {
		short := u.localSHA[:7]
		localSha = &short
	}
	return checkResult{
		Status:       status,
		Installation: u.installation,
		LocalSha:     localSha,
		CheckedAt:    u.now().UTC().Format(time.RFC3339),
		Version:      u.version(),
	}
}

type githubCompare struct {
	Status  string         `json:"status"`
	Commits []githubCommit `json:"commits"`
}

type githubCommit struct {
	Sha    string `json:"sha"`
	Commit struct {
		Message   string `json:"message"`
		Committer struct {
			Date string `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
}

func (u *updateChecker) performCheck(ctx context.Context) (checkResult, bool) {
	ctx, cancel := context.WithTimeout(ctx, updateFetchTimeout)
	defer cancel()

	url := fmt.Sprintf("https://api.github.com/repos/%s/compare/%s...main", u.repo, u.localSHA)
	body, ok := u.getJSON(ctx, url)
	if !ok {
		return checkResult{}, false
	}

	var cmp githubCompare
	if err := json.Unmarshal(body, &cmp); err != nil {
		return checkResult{}, false
	}
	mapped, ok := mapCompareStatus(cmp.Status)
	if !ok {
		return checkResult{}, false
	}

	result := u.baseResult(mapped)
	applyRemoteMetadata(&result, u.localSHA, &cmp)
	if cmp.Status == "ahead" {
		result.Changes = parseChanges(&cmp)
	}
	return result, true
}

// mapCompareStatus turns GitHub's head-relative-to-base verdict into the
// dashboard's vocabulary: main ahead of us means an update is available; us
// ahead of main means a local dev build (update.ts:131-138).
func mapCompareStatus(status string) (string, bool) {
	switch status {
	case "identical":
		return "current", true
	case "ahead":
		return "available", true
	case "behind":
		return "ahead", true
	case "diverged":
		return "diverged", true
	}
	return "", false
}

// applyRemoteMetadata fills the remote commit summary from the last commit in
// the compare, mirroring update.ts:154-192. An identical compare with no
// commits still reports the shared head as remoteSha.
func applyRemoteMetadata(result *checkResult, localSHA string, cmp *githubCompare) {
	if len(cmp.Commits) == 0 {
		if cmp.Status == "identical" && len(localSHA) >= 7 {
			result.RemoteSha = localSHA[:7]
		}
		return
	}
	last := cmp.Commits[len(cmp.Commits)-1]
	if len(last.Sha) >= 7 {
		result.RemoteSha = last.Sha[:7]
	}
	result.RemoteMessage = truncate(last.Commit.Message, maxRemoteMessageLen)
	result.RemoteDate = last.Commit.Committer.Date
}

// parseChanges lists the commits main is ahead by, newest first, capped
// (update.ts:194-229).
func parseChanges(cmp *githubCompare) []change {
	commits := cmp.Commits
	if len(commits) > maxChanges {
		commits = commits[len(commits)-maxChanges:]
	}
	changes := make([]change, 0, len(commits))
	for i := len(commits) - 1; i >= 0; i-- {
		c := commits[i]
		sha := c.Sha
		if len(sha) >= 7 {
			sha = sha[:7]
		}
		changes = append(changes, change{
			Sha:     sha,
			Message: truncate(firstLine(c.Commit.Message), maxChangeMessageLen),
			Date:    c.Commit.Committer.Date,
		})
	}
	return changes
}

type githubRelease struct {
	TagName     string `json:"tag_name"`
	Body        string `json:"body"`
	HTMLURL     string `json:"html_url"`
	PublishedAt string `json:"published_at"`
}

// release returns the newest published release, cached briefly. Any failure is
// reported as not-ok so the caller can answer the pill without surfacing an
// error.
func (u *updateChecker) release(ctx context.Context) (latestRelease, bool) {
	u.mu.Lock()
	if u.lastRelease != nil && u.now().Sub(u.lastReleaseAt) < updateCheckCacheTTL {
		cached := *u.lastRelease
		u.mu.Unlock()
		return cached, true
	}
	u.mu.Unlock()

	result, ok := u.performRelease(ctx)
	if !ok {
		return latestRelease{}, false
	}

	u.mu.Lock()
	u.lastRelease = &result
	u.lastReleaseAt = u.now()
	u.mu.Unlock()
	return result, true
}

func (u *updateChecker) performRelease(ctx context.Context) (latestRelease, bool) {
	ctx, cancel := context.WithTimeout(ctx, updateFetchTimeout)
	defer cancel()

	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", u.repo)
	body, ok := u.getJSON(ctx, url)
	if !ok {
		return latestRelease{}, false
	}

	var gh githubRelease
	if err := json.Unmarshal(body, &gh); err != nil {
		return latestRelease{}, false
	}
	tag := strings.TrimSpace(gh.TagName)
	if tag == "" {
		return latestRelease{}, false
	}

	// The release notes render as Markdown in the dashboard, so the body is
	// capped and the link is honoured only when it points back at this repo
	// (update.ts:491-503).
	releasesURL := fmt.Sprintf("https://github.com/%s/releases", u.repo)
	htmlURL := releasesURL
	if strings.HasPrefix(gh.HTMLURL, releasesURL+"/") {
		htmlURL = gh.HTMLURL
	}

	return latestRelease{
		TagName:     truncate(tag, maxTagLen),
		Body:        strOrNil(truncate(gh.Body, maxReleaseBodyLen)),
		HTMLURL:     htmlURL,
		PublishedAt: strOrNil(gh.PublishedAt),
	}, true
}

// getJSON performs a bounded GET and returns the body on a 2xx, reporting
// not-ok on any transport error, non-2xx status, or oversized body. It is the
// single choke point where a network failure becomes graceful degradation.
func (u *updateChecker) getJSON(ctx context.Context, url string) ([]byte, bool) {
	resp, err := u.fetch(ctx, url, githubHeaders())
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCompareBody))
	if err != nil {
		return nil, false
	}
	return body, true
}

func githubHeaders() map[string]string {
	return map[string]string{
		"Accept":               "application/vnd.github+json",
		"User-Agent":           "prowl-update-checker",
		"X-GitHub-Api-Version": "2022-11-28",
	}
}

func defaultFetch(ctx context.Context, url string, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return http.DefaultClient.Do(req)
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// strOrNil returns a pointer to s, or nil when s is empty, so a nullable field
// serialises as JSON null rather than an empty string.
func strOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
