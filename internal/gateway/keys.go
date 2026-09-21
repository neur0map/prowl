package gateway

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/neur0map/prowl/internal/gateway/catalog"
	"github.com/neur0map/prowl/internal/gateway/provider"
)

// KeyStatus is a credential's last-known health. It is deliberately three
// states, not the reference's four: `unknown` (never probed), `healthy` (a
// probe or a live request succeeded), and `error` (a provider confirmed the
// credential is bad). A transport failure is none of these - it leaves the
// status untouched (health.ts:143-158).
type KeyStatus string

const (
	StatusUnknown KeyStatus = "unknown"
	StatusHealthy KeyStatus = "healthy"
	StatusError   KeyStatus = "error"
)

// KeyRow is one api_keys row's metadata. It never carries the decrypted
// secret: the only way to the plaintext is Reveal, so a future /api/keys
// handler cannot leak every credential by marshalling this struct. Field names
// track the api_keys columns so the router's key candidate maps one-to-one.
type KeyRow struct {
	ID                  int64     `json:"id"`
	Platform            string    `json:"platform"`
	Label               string    `json:"label"`
	Masked              string    `json:"masked"`
	Status              KeyStatus `json:"status"`
	Enabled             bool      `json:"enabled"`
	BaseURL             string    `json:"base_url,omitempty"`
	ModelScope          []string  `json:"model_scope,omitempty"`
	LastCheckedAt       time.Time `json:"last_checked_at,omitempty"`
	LastHealthError     string    `json:"last_health_error,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	CreatedAt           time.Time `json:"created_at"`
}

// AddOptions carries the optional columns on a new key row.
type AddOptions struct {
	Label      string
	BaseURL    string   // relay endpoint for a custom provider
	ModelScope []string // nil => the key serves every model of its platform
	Enabled    *bool    // nil => enabled
}

// Validator probes a decrypted credential. The provider registry satisfies it
// through RegistryValidator; tests inject a scripted stub. It is the whole of
// the vault's coupling to the provider layer, so the two can be built and
// tested independently.
type Validator interface {
	ValidateKey(ctx context.Context, platform, baseURL, apiKey string) provider.KeyValidationResult
}

// RegistryValidator adapts a provider registry to Validator.
type RegistryValidator struct {
	Registry *provider.Registry
}

func (r RegistryValidator) ValidateKey(ctx context.Context, platform, baseURL, apiKey string) provider.KeyValidationResult {
	p, ok := r.Registry.Resolve(platform, baseURL)
	if !ok {
		return provider.Inconclusive("no provider registered for platform " + platform)
	}
	return p.ValidateKey(ctx, apiKey)
}

// Encryption parameters, all pinned to the reference (lib/crypto.ts):
//   - AES-256-GCM with a 32-byte key (crypto.ts:7,18).
//   - a fresh 16-byte IV per secret (crypto.ts:184); Go's GCM defaults to a
//     12-byte nonce, so the vault uses NewGCMWithNonceSize(…, 16) to match.
//   - a 16-byte auth tag, pinned so a rewritten 4-byte tag cannot open a
//     truncated-tag forgery path (AUTH_TAG_BYTES, crypto.ts:198-202).
//
// Ciphertext, IV and tag are stored as three hex columns per row, exactly the
// api_keys.encrypted_key/iv/auth_tag shape.
const (
	keyIVBytes      = 16
	keyAuthTagBytes = 16
)

const (
	encryptionKeyEnv         = "ENCRYPTION_KEY"
	encryptionKeyPlaceholder = "your-64-char-hex-key-here" // crypto.ts:20
	vaultMetaPrefix          = "keys."
)

// Health-pass cadence, pinned to services/health.ts:13-40.
const (
	keyCheckInterval                = 5 * time.Minute
	keyCheckIntervalJitter          = 0.2
	keyRecentCheckSkip              = 210 * time.Second // 3.5 min
	keyHealthPassBudget             = keyCheckInterval / 2
	keyDefaultConcurrency           = 8
	keyDefaultMinSpacing            = time.Second
	keyConsecutiveFailuresToDisable = 3 // CONSECUTIVE_FAILURES_TO_DISABLE, health.ts:14
)

// PlatformForCatalogID resolves a catalog or directory provider id to the
// platform the registry keys on. The canonical id-to-adapter mapping lives in
// the catalog package (catalog.AdapterPlatform), which every consumer - this
// vault, the directory routes, and the registry's own auto-registration -
// resolves through, so the answer cannot drift between them. It is the explicit
// answer where name-shape heuristics fail: "ovhcloud-ai-endpoints" is the
// adapter "ovh", which no prefix rule with a sane minimum length can infer.
func PlatformForCatalogID(id string) (string, bool) {
	return catalog.AdapterPlatform(id)
}

// KeyVault is the relational credential store: many keys per platform, each a
// row in api_keys with its own encrypted secret, status, enabled flag and
// consecutive-failure count. It supersedes the single-key-per-provider
// KeyStore in keystore.go, which the integration pass retires.
type KeyVault struct {
	db        *sql.DB
	dir       string
	aead      cipher.AEAD
	validator Validator // nil => the health verbs are unavailable

	// credentials resolves linked rows, whose secret lives in a Prowl login
	// rather than in this vault.
	credentials CredentialSource

	// passMu serialises CheckAllKeys: two overlapping passes could each
	// increment the same bad key's counter and disable it in fewer than the
	// three consecutive checks the threshold promises (health.ts:overlap guard).
	passMu sync.Mutex
}

// OpenKeyVault opens the vault against db, using dir to hold (or read) the
// master key. validator may be nil, in which case the health verbs return an
// error but every other operation works. On first open it verifies the master
// key against a stored fingerprint (failing loudly on a mismatch) and imports
// any legacy keys.enc exactly once.
func OpenKeyVault(ctx context.Context, db *sql.DB, dir string, validator Validator) (*KeyVault, error) {
	if db == nil {
		return nil, errors.New("gateway key vault needs a database")
	}
	if dir == "" {
		return nil, errors.New("gateway key vault needs a directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create key vault directory: %w", err)
	}
	key, source, err := resolveVaultMasterKey(dir)
	if err != nil {
		return nil, err
	}
	aead, err := newVaultAEAD(key)
	if err != nil {
		return nil, err
	}
	v := &KeyVault{db: db, dir: dir, aead: aead, validator: validator}
	if err := v.verifyMasterKey(ctx, key, source); err != nil {
		return nil, err
	}
	if err := v.importLegacyKeys(ctx); err != nil {
		return nil, err
	}
	return v, nil
}

// resolveVaultMasterKey follows the reference's precedence (crypto.ts:94-171),
// trimmed to what a local install needs: ENCRYPTION_KEY env wins if set and not
// the placeholder; otherwise the master.key file beside the DB, generated on
// first use. The file is the same one keystore.go uses, so keys.enc stays
// decryptable for the import.
func resolveVaultMasterKey(dir string) ([]byte, string, error) {
	if raw := strings.TrimSpace(os.Getenv(encryptionKeyEnv)); raw != "" && raw != encryptionKeyPlaceholder {
		if len(raw) != masterKeySize*2 || !isHexString(raw) {
			return nil, "", fmt.Errorf("invalid %s: expected %d hex chars (%d bytes), got %d chars",
				encryptionKeyEnv, masterKeySize*2, masterKeySize, len(raw))
		}
		b, err := hex.DecodeString(raw)
		if err != nil {
			return nil, "", fmt.Errorf("invalid %s: %w", encryptionKeyEnv, err)
		}
		return b, "env", nil
	}

	// The file master key is created with crash-safe first-writer-wins
	// semantics: concurrent openers must agree on one key (a divergent second
	// key would fail every stored credential's fingerprint check), and a crash
	// mid-write must never leave a half-written key for the next open to reject.
	key, err := firstWriterSecretFile(dir, masterFileName, loadMasterKey, generateMasterKey)
	if err != nil {
		return nil, "", err
	}
	return key, "file", nil
}

// loadMasterKey reports the stored master key, or (nil, false, nil) when the
// file is absent so the caller mints one. A present-but-wrong-length key is
// corruption, not a recoverable empty create: it fails loudly so the operator
// restores the original rather than silently minting a new key that cannot
// decrypt existing credentials.
func loadMasterKey(path string) ([]byte, bool, error) {
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(b) != masterKeySize {
			return nil, false, fmt.Errorf("master key at %s is %d bytes, want %d; restore the original file rather than re-entering keys", path, len(b), masterKeySize)
		}
		return b, true, nil
	case errors.Is(err, os.ErrNotExist):
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("read master key: %w", err)
	}
}

func generateMasterKey() ([]byte, error) {
	b := make([]byte, masterKeySize)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generate master key: %w", err)
	}
	return b, nil
}

func newVaultAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("init cipher: %w", err)
	}
	return cipher.NewGCMWithNonceSize(block, keyIVBytes)
}

// masterKeyFingerprint is sha256:<first 16 hex> of the active key
// (encryptionKeyFingerprint, crypto.ts:173-180): enough to detect a changed
// key, never enough to recover it.
func masterKeyFingerprint(key []byte) string {
	sum := sha256.Sum256(key)
	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}

// verifyMasterKey fails loudly if the active master key differs from the one
// the vault was initialised with, so an operator sees one clear "restore the
// master.key" error instead of a per-key "invalid" for every credential.
func (v *KeyVault) verifyMasterKey(ctx context.Context, key []byte, source string) error {
	fp := masterKeyFingerprint(key)
	stored, ok, err := v.metaGet(ctx, "master_fingerprint")
	if err != nil {
		return err
	}
	if !ok {
		return v.metaSet(ctx, "master_fingerprint", fp)
	}
	if stored != fp {
		hint := "master.key file"
		if source == "env" {
			hint = encryptionKeyEnv + " environment variable"
		}
		return fmt.Errorf("gateway key vault: the master key does not match this store - the %s changed or is missing; restore the original key rather than re-entering credentials", hint)
	}
	return nil
}

// ── meta (settings kv, namespaced) ───────────────────────────────────────────

func (v *KeyVault) metaGet(ctx context.Context, k string) (string, bool, error) {
	var val string
	err := v.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", vaultMetaPrefix+k).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return val, true, nil
}

func (v *KeyVault) metaSet(ctx context.Context, k, val string) error {
	_, err := v.db.ExecContext(ctx,
		`INSERT INTO settings(key, value, updated_at) VALUES(?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		vaultMetaPrefix+k, val, time.Now().Unix())
	return err
}

// ── encryption ───────────────────────────────────────────────────────────────

func (v *KeyVault) encryptSecret(plain string) (encHex, ivHex, tagHex string, err error) {
	nonce := make([]byte, keyIVBytes)
	if _, err := rand.Read(nonce); err != nil {
		return "", "", "", fmt.Errorf("generate iv: %w", err)
	}
	sealed := v.aead.Seal(nil, nonce, []byte(plain), nil)
	tagStart := len(sealed) - keyAuthTagBytes
	return hex.EncodeToString(sealed[:tagStart]), hex.EncodeToString(nonce), hex.EncodeToString(sealed[tagStart:]), nil
}

func (v *KeyVault) decryptSecret(encHex, ivHex, tagHex string) (string, error) {
	nonce, err := hex.DecodeString(ivHex)
	if err != nil || len(nonce) != keyIVBytes {
		return "", errors.New("stored iv is malformed")
	}
	ct, err := hex.DecodeString(encHex)
	if err != nil {
		return "", errors.New("stored ciphertext is malformed")
	}
	tag, err := hex.DecodeString(tagHex)
	if err != nil || len(tag) != keyAuthTagBytes {
		return "", errors.New("stored auth tag is malformed")
	}
	sealed := make([]byte, 0, len(ct)+len(tag))
	sealed = append(sealed, ct...)
	sealed = append(sealed, tag...)
	plain, err := v.aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		// GCM authentication failed: the ciphertext or tag was tampered with,
		// or the master key is wrong for this row.
		return "", fmt.Errorf("decrypt key: %w", err)
	}
	return string(plain), nil
}

// resolveSecret returns the usable credential for a stored row. A linked row
// carries a `link:<provider>` reference with an empty IV/tag, so it is resolved
// through the login source that refreshes it rather than decrypted; every other
// row is ciphertext. Reveal and the health probe share this one path, so a
// health check tests the same token a request would actually send.
func (v *KeyVault) resolveSecret(ctx context.Context, enc, iv, tag string) (string, error) {
	if ref, linked := LinkedRef(enc); linked {
		return v.resolveLinked(ctx, ref)
	}
	return v.decryptSecret(enc, iv, tag)
}

// ── CRUD ─────────────────────────────────────────────────────────────────────

// Add inserts a new credential and returns its row id. A new key starts
// `unknown` until the first health probe.
func (v *KeyVault) Add(platform, apiKey string, opts AddOptions) (int64, error) {
	platform = strings.TrimSpace(platform)
	if platform == "" {
		return 0, errors.New("platform is required")
	}
	if strings.TrimSpace(apiKey) == "" {
		return 0, errors.New("api key is required")
	}
	enc, iv, tag, err := v.encryptSecret(apiKey)
	if err != nil {
		return 0, err
	}
	enabled := 1
	if opts.Enabled != nil && !*opts.Enabled {
		enabled = 0
	}
	var baseURL any
	if b := strings.TrimSpace(opts.BaseURL); b != "" {
		// The vault is the one chokepoint every base_url writer passes through,
		// so the SSRF verdict lives here: a route that forgets to pre-check
		// still cannot persist an internal address the health pass would then
		// probe on a timer.
		if ok, reason := AssessProviderURL(b); !ok {
			return 0, fmt.Errorf("base URL rejected: %s", reason)
		}
		baseURL = opts.BaseURL
	}
	var scope any
	if len(opts.ModelScope) > 0 {
		b, err := json.Marshal(opts.ModelScope)
		if err != nil {
			return 0, err
		}
		scope = string(b)
	}
	res, err := v.db.Exec(
		`INSERT INTO api_keys(platform, label, encrypted_key, iv, auth_tag, status, enabled, base_url, model_scope_json, consecutive_failures, created_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
		platform, opts.Label, enc, iv, tag, string(StatusUnknown), enabled, baseURL, scope, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const keyRowColumns = `id, platform, label, encrypted_key, iv, auth_tag, status, enabled, base_url, model_scope_json, last_checked_at, last_health_error, consecutive_failures, created_at`

// AddLinked adds a pool row whose credential lives in a Prowl login.
//
// Nothing is encrypted: a subscription token is refreshed by the login that
// owns it, so a copy in the vault would be stale within the hour and would
// outlive a sign-out. The row stores a reference and Reveal resolves it per
// request.
func (v *KeyVault) AddLinked(ctx context.Context, provider, label string) (int64, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return 0, errors.New("provider is required")
	}
	res, err := v.db.ExecContext(ctx,
		`INSERT INTO api_keys(platform, label, encrypted_key, iv, auth_tag, status, enabled, consecutive_failures, created_at)
		 VALUES(?, ?, ?, '', '', ?, 1, 0, ?)`,
		provider, label, MarkLinked(provider), string(StatusUnknown), time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// List returns every key's metadata, newest first, masked and without secrets.
func (v *KeyVault) List(ctx context.Context) ([]KeyRow, error) {
	rows, err := v.db.QueryContext(ctx,
		`SELECT `+keyRowColumns+` FROM api_keys ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyRow
	for rows.Next() {
		row, createdAt, err := scanKeyRowFull(v, rows)
		if err != nil {
			return nil, err
		}
		row.CreatedAt = createdAt
		out = append(out, row)
	}
	return out, rows.Err()
}

// scanKeyRowFull scans a full row including created_at. It exists so List and
// Get share one column contract with the masking guarantee.
func scanKeyRowFull(v *KeyVault, s interface {
	Scan(dest ...any) error
}) (KeyRow, time.Time, error) {
	var (
		row           KeyRow
		enc, iv, tag  string
		enabled       int
		baseURL       sql.NullString
		scope         sql.NullString
		lastChecked   sql.NullInt64
		lastHealthErr sql.NullString
		statusStr     string
		createdAt     int64
	)
	if err := s.Scan(&row.ID, &row.Platform, &row.Label, &enc, &iv, &tag, &statusStr, &enabled,
		&baseURL, &scope, &lastChecked, &lastHealthErr, &row.ConsecutiveFailures, &createdAt); err != nil {
		return KeyRow{}, time.Time{}, err
	}
	row.Status = KeyStatus(statusStr)
	row.Enabled = enabled != 0
	row.BaseURL = baseURL.String
	row.LastHealthError = lastHealthErr.String
	if scope.Valid && scope.String != "" {
		_ = json.Unmarshal([]byte(scope.String), &row.ModelScope)
	}
	if lastChecked.Valid {
		row.LastCheckedAt = time.Unix(lastChecked.Int64, 0).UTC()
	}
	if secret, err := v.decryptSecret(enc, iv, tag); err == nil {
		row.Masked = maskKey(secret)
	} else {
		row.Masked = "(unreadable)"
	}
	return row, time.Unix(createdAt, 0).UTC(), nil
}

// Get returns one key's metadata.
func (v *KeyVault) Get(ctx context.Context, id int64) (KeyRow, bool, error) {
	r := v.db.QueryRowContext(ctx, `SELECT `+keyRowColumns+` FROM api_keys WHERE id = ?`, id)
	row, createdAt, err := scanKeyRowFull(v, r)
	if errors.Is(err, sql.ErrNoRows) {
		return KeyRow{}, false, nil
	}
	if err != nil {
		return KeyRow{}, false, err
	}
	row.CreatedAt = createdAt
	return row, true, nil
}

// Reveal returns the decrypted secret for one key. This is the ONLY path to the
// plaintext; callers use it transiently to make a request and never store it.
func (v *KeyVault) Reveal(ctx context.Context, id int64) (string, error) {
	var enc, iv, tag string
	err := v.db.QueryRowContext(ctx, "SELECT encrypted_key, iv, auth_tag FROM api_keys WHERE id = ?", id).Scan(&enc, &iv, &tag)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("key %d not found", id)
	}
	if err != nil {
		return "", err
	}
	return v.resolveSecret(ctx, enc, iv, tag)
}

// SetEnabled toggles a key's enabled flag.
func (v *KeyVault) SetEnabled(ctx context.Context, id int64, enabled bool) error {
	e := 0
	if enabled {
		e = 1
	}
	_, err := v.db.ExecContext(ctx, "UPDATE api_keys SET enabled = ? WHERE id = ?", e, id)
	return err
}

// Delete removes a key.
func (v *KeyVault) Delete(ctx context.Context, id int64) error {
	_, err := v.db.ExecContext(ctx, "DELETE FROM api_keys WHERE id = ?", id)
	return err
}

// ── health ───────────────────────────────────────────────────────────────────

// CheckKey probes one key and applies the verdict. A confirmed-invalid probe
// records the reason, increments the consecutive-failure counter and - on the
// third consecutive confirmation - disables the key. A valid probe clears the
// counter and marks it healthy. A transport-inconclusive probe records only the
// diagnostic and leaves status and counter untouched, so a flaky network can
// never disable a working key.
func (v *KeyVault) CheckKey(ctx context.Context, id int64) (KeyStatus, error) {
	if v.validator == nil {
		return "", errors.New("key vault has no validator configured")
	}
	var (
		platform     string
		baseURL      sql.NullString
		enc, iv, tag string
		statusStr    string
	)
	err := v.db.QueryRowContext(ctx,
		"SELECT platform, base_url, encrypted_key, iv, auth_tag, status FROM api_keys WHERE id = ?", id).
		Scan(&platform, &baseURL, &enc, &iv, &tag, &statusStr)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("key %d not found", id)
	}
	if err != nil {
		return "", err
	}

	apiKey, derr := v.resolveSecret(ctx, enc, iv, tag)
	if derr != nil {
		// A row whose credential cannot be resolved - an undecryptable secret
		// or a linked login that is gone - is not a provider verdict; record it
		// and leave the status alone, the same restraint transport errors get.
		_ = v.recordInconclusive(ctx, id, derr.Error())
		return KeyStatus(statusStr), nil
	}

	res := v.validator.ValidateKey(ctx, platform, baseURL.String, apiKey)
	now := time.Now().UTC().Unix()
	switch res.Status {
	case provider.KeyValid:
		_, err := v.db.ExecContext(ctx,
			"UPDATE api_keys SET status = ?, last_health_error = NULL, last_checked_at = ?, consecutive_failures = 0 WHERE id = ?",
			string(StatusHealthy), now, id)
		return StatusHealthy, err
	case provider.KeyInvalid:
		if _, err := v.db.ExecContext(ctx,
			"UPDATE api_keys SET status = ?, last_health_error = ?, last_checked_at = ?, consecutive_failures = consecutive_failures + 1 WHERE id = ?",
			string(StatusError), sanitizeHealthError(RedactWith(res.Reason, apiKey)), now, id); err != nil {
			return "", err
		}
		var failures int
		if err := v.db.QueryRowContext(ctx, "SELECT consecutive_failures FROM api_keys WHERE id = ?", id).Scan(&failures); err != nil {
			return "", err
		}
		if failures >= keyConsecutiveFailuresToDisable {
			if _, err := v.db.ExecContext(ctx, "UPDATE api_keys SET enabled = 0 WHERE id = ?", id); err != nil {
				return "", err
			}
			slog.Warn("gateway auto-disabled a key after consecutive invalid checks",
				"key_id", id, "platform", platform, "failures", failures)
		}
		return StatusError, nil
	default: // inconclusive
		_ = v.recordInconclusive(ctx, id, res.Reason)
		return KeyStatus(statusStr), nil
	}
}

// recordInconclusive stores why a probe reached no verdict, and withdraws a
// healthy claim the probe itself cannot support.
//
// A key shows healthy only on evidence. Real traffic is evidence and survives;
// a probe against an endpoint that authenticates nothing is not, and leaving
// green on screen because of one would misreport a dead credential - which is
// exactly what happened with Ollama Cloud.
func (v *KeyVault) recordInconclusive(ctx context.Context, id int64, reason string) error {
	var served int
	if err := v.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM requests
		 WHERE key_id = ? AND outcome = 'success'`, id).Scan(&served); err != nil {
		served = 0
	}
	if served > 0 {
		_, err := v.db.ExecContext(ctx,
			"UPDATE api_keys SET last_health_error = ?, last_checked_at = ? WHERE id = ?",
			sanitizeHealthError(reason), time.Now().UTC().Unix(), id)
		return err
	}
	_, err := v.db.ExecContext(ctx, `
		UPDATE api_keys
		   SET status = ?, last_health_error = ?, last_checked_at = ?
		 WHERE id = ? AND status <> ?`,
		string(StatusUnknown), sanitizeHealthError(reason),
		time.Now().UTC().Unix(), id, string(StatusError))
	return err
}

// MarkHealthyFromRequest promotes a key out of `error` after it successfully
// served a live request - stronger evidence than any probe (health.ts:196-213).
// Deliberately narrow: it only clears `error`, never re-enables a key an
// operator disabled.
func (v *KeyVault) MarkHealthyFromRequest(ctx context.Context, id int64) error {
	_, err := v.db.ExecContext(ctx,
		"UPDATE api_keys SET status = ?, last_health_error = NULL, consecutive_failures = 0 WHERE id = ? AND status = ?",
		string(StatusHealthy), id, string(StatusError))
	return err
}

// MarkInvalidFromRequest demotes a key that a live request proved bad.
//
// It is the mirror of MarkHealthyFromRequest and exists because a probe can be
// weaker evidence than real traffic: several providers serve their validation
// endpoint without a credential, so a probe reports success while inference
// answers 401. When the two disagree, the request wins - it is the thing the
// operator actually cares about.
func (v *KeyVault) MarkInvalidFromRequest(ctx context.Context, id int64, reason string) error {
	if _, err := v.db.ExecContext(ctx, `
		UPDATE api_keys
		   SET status = ?, last_health_error = ?, last_checked_at = ?,
		       consecutive_failures = consecutive_failures + 1
		 WHERE id = ?`,
		string(StatusError), sanitizeHealthError(RedactWith(reason)),
		time.Now().UTC().Unix(), id); err != nil {
		return err
	}

	// A credential the provider keeps rejecting is spent, not flaky. Left
	// enabled it costs a wasted hop on every single request - a live run
	// burned four attempts on one dead key - and the operator has no signal
	// that it needs attention. Disabling it stops the waste and shows up in
	// the dashboard as something to fix.
	var failures int
	if err := v.db.QueryRowContext(ctx,
		"SELECT consecutive_failures FROM api_keys WHERE id = ?", id).Scan(&failures); err != nil {
		return err
	}
	if failures < keyConsecutiveFailuresToDisable {
		return nil
	}
	_, err := v.db.ExecContext(ctx,
		"UPDATE api_keys SET enabled = 0 WHERE id = ?", id)
	if err == nil {
		slog.Warn("Disabled a key the provider kept rejecting",
			"key_id", id, "consecutive_failures", failures)
	}
	return err
}

// HealthPassOptions tune one CheckAllKeys pass; the func fields are test seams.
type HealthPassOptions struct {
	Force       bool                                                   // probe every enabled key, no recency skip or spacing
	Now         func() time.Time                                       // injectable clock for pacing
	Sleep       func(time.Duration)                                    // injectable sleep
	Check       func(ctx context.Context, id int64) (KeyStatus, error) // injectable probe
	Concurrency int
	MinSpacing  time.Duration
}

// HealthPassResult reports what a pass did.
type HealthPassResult struct {
	Checked []int64 // ids probed, in start order
	Skipped []int64 // enabled ids left alone because checked recently
}

type healthKey struct {
	id     int64
	bucket string
	status string
	ageMS  int64
	hasAge bool
}

// CheckAllKeys runs one health pass: round-robin across providers, skip keys
// probed within the recency window (unless forced or parked at `error`), and
// bound concurrency and per-provider spacing so a large fleet neither hammers a
// single provider nor overruns the interval it is scheduled on
// (health.ts:runHealthPass). It is exposed for the lead to schedule; it never
// starts a goroutine of its own.
func (v *KeyVault) CheckAllKeys(ctx context.Context, opts HealthPassOptions) (HealthPassResult, error) {
	v.passMu.Lock()
	defer v.passMu.Unlock()

	if opts.Check == nil && v.validator == nil {
		return HealthPassResult{}, errors.New("key vault has no validator configured")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	check := opts.Check
	if check == nil {
		check = v.CheckKey
	}

	rows, err := v.db.QueryContext(ctx,
		"SELECT id, platform, COALESCE(base_url, ''), status, last_checked_at FROM api_keys WHERE enabled = 1")
	if err != nil {
		return HealthPassResult{}, err
	}
	nowMS := now().UnixMilli()
	var all []healthKey
	for rows.Next() {
		var (
			id          int64
			platform    string
			baseURL     string
			status      string
			lastChecked sql.NullInt64
		)
		if err := rows.Scan(&id, &platform, &baseURL, &status, &lastChecked); err != nil {
			rows.Close()
			return HealthPassResult{}, err
		}
		hk := healthKey{id: id, status: status}
		hk.bucket = platform
		if baseURL != "" {
			hk.bucket = platform + "|" + baseURL
		}
		if lastChecked.Valid {
			hk.ageMS = nowMS - lastChecked.Int64*1000
			hk.hasAge = true
		}
		all = append(all, hk)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return HealthPassResult{}, err
	}

	result := HealthPassResult{}
	var due []healthKey
	for _, hk := range all {
		switch {
		case opts.Force:
			due = append(due, hk)
		case hk.status == string(StatusError):
			// A parked-error key is the one whose verdict is worth re-asking.
			due = append(due, hk)
		case hk.hasAge && hk.ageMS < keyRecentCheckSkip.Milliseconds():
			result.Skipped = append(result.Skipped, hk.id)
		default:
			due = append(due, hk)
		}
	}

	queue := interleaveByBucket(due)
	spacing := time.Duration(0)
	if !opts.Force {
		largest := largestBucket(queue)
		if largest >= 2 {
			requested := opts.MinSpacing
			if requested <= 0 {
				requested = keyDefaultMinSpacing
			}
			budgeted := keyHealthPassBudget / time.Duration(largest-1)
			spacing = requested
			if budgeted < spacing {
				spacing = budgeted
			}
		}
	}

	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = keyDefaultConcurrency
	}
	if concurrency > len(queue) {
		concurrency = len(queue)
	}

	var (
		mu          sync.Mutex
		cursor      int
		nextAllowed = map[string]int64{} // bucket -> earliest start (ms)
		wg          sync.WaitGroup
	)
	worker := func() {
		defer wg.Done()
		for {
			mu.Lock()
			if cursor >= len(queue) {
				mu.Unlock()
				return
			}
			hk := queue[cursor]
			cursor++
			var wait time.Duration
			if spacing > 0 {
				slot := nextAllowed[hk.bucket]
				cur := now().UnixMilli()
				if slot < cur {
					slot = cur
				}
				nextAllowed[hk.bucket] = slot + spacing.Milliseconds()
				if slot > cur {
					wait = time.Duration(slot-cur) * time.Millisecond
				}
			}
			result.Checked = append(result.Checked, hk.id)
			mu.Unlock()

			if wait > 0 {
				sleep(wait)
			}
			if _, err := check(ctx, hk.id); err != nil {
				slog.Debug("gateway health probe errored", "key_id", hk.id, "err", err)
			}
		}
	}
	wg.Add(concurrency)
	for range concurrency {
		go worker()
	}
	wg.Wait()
	return result, nil
}

// interleaveByBucket round-robins the queue across provider buckets so a fleet
// with many keys on one provider does not fire them back to back
// (interleaveByProvider, health.ts).
func interleaveByBucket(rows []healthKey) []healthKey {
	order := []string{}
	buckets := map[string][]healthKey{}
	for _, r := range rows {
		if _, ok := buckets[r.bucket]; !ok {
			order = append(order, r.bucket)
		}
		buckets[r.bucket] = append(buckets[r.bucket], r)
	}
	out := make([]healthKey, 0, len(rows))
	for i := 0; len(out) < len(rows); i++ {
		for _, b := range order {
			list := buckets[b]
			if i < len(list) {
				out = append(out, list[i])
			}
		}
	}
	return out
}

func largestBucket(rows []healthKey) int {
	counts := map[string]int{}
	max := 0
	for _, r := range rows {
		counts[r.bucket]++
		if counts[r.bucket] > max {
			max = counts[r.bucket]
		}
	}
	return max
}

// NextHealthCheckDelay is the base interval ±20%, so restarts and co-deployed
// gateways do not stay phase-locked probing the same providers
// (nextHealthCheckDelayMs, health.ts). jitter returns [0,1); nil uses the
// process RNG.
func NextHealthCheckDelay(jitter func() float64) time.Duration {
	if jitter == nil {
		jitter = mrand.Float64
	}
	factor := 1 + (jitter()*2-1)*keyCheckIntervalJitter
	return time.Duration(float64(keyCheckInterval) * factor)
}

// ── legacy import ────────────────────────────────────────────────────────────

// importLegacyKeys copies the single-key-per-provider keys.enc into the table
// exactly once, so an existing install keeps working without the user
// re-pasting keys. The file is left in place. It is idempotent: a flag plus a
// per-platform existence guard means a second open adds nothing.
func (v *KeyVault) importLegacyKeys(ctx context.Context) error {
	if done, ok, err := v.metaGet(ctx, "legacy_import_done"); err != nil {
		return err
	} else if ok && done == "1" {
		return nil
	}

	old, err := OpenKeyStore(v.dir)
	if err != nil {
		// keys.enc exists but will not decrypt: surface it rather than silently
		// skipping, so a wrong master.key is fixed before the rows are lost.
		return fmt.Errorf("import legacy keys.enc: %w", err)
	}

	entries := old.List()
	// Deterministic order so a partial failure re-runs identically.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Provider < entries[j].Provider })

	for _, e := range entries {
		secret, ok := old.Get(e.Provider)
		if !ok || strings.TrimSpace(secret) == "" {
			continue
		}
		platform, ok := catalog.AdapterPlatform(e.Provider)
		if !ok {
			platform = e.Provider
		}

		var exists int
		err := v.db.QueryRowContext(ctx, "SELECT 1 FROM api_keys WHERE platform = ? LIMIT 1", platform).Scan(&exists)
		if err == nil {
			continue // already have a row for this platform; do not duplicate.
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		opts := AddOptions{Label: "imported from keys.enc"}
		// Cloudflare's wire needs a compound account_id:token; fold a stored
		// account id back into the key if the legacy row split them.
		if platform == "cloudflare" {
			if acct := strings.TrimSpace(e.Vars["account_id"]); acct != "" && !strings.Contains(secret, ":") {
				secret = acct + ":" + secret
			}
		}
		if base := strings.TrimSpace(e.Vars["base_url"]); base != "" {
			opts.BaseURL = base
		}

		// A platform the registry does not know - one neither the catalog maps
		// nor a handwritten adapter serves - is imported DISABLED so the
		// credential is preserved and visible for the operator to fix, never
		// silently dropped.
		if !provider.Known(platform) {
			disabled := false
			opts.Enabled = &disabled
			slog.Warn("imported a legacy key under an unrecognised platform id; left disabled for review",
				"platform", platform)
		}

		if _, err := v.Add(platform, secret, opts); err != nil {
			return err
		}
	}

	return v.metaSet(ctx, "legacy_import_done", "1")
}

// ── small helpers ────────────────────────────────────────────────────────────

// maskKey renders a credential as a recognisable but unusable preview
// (maskKey, crypto.ts:227-231): nothing below 5 chars, at most the last two up
// to 8, else first4…last4.
func maskKey(key string) string {
	n := len(key)
	switch {
	case n < 5:
		return "****"
	case n <= 8:
		return "****" + key[n-2:]
	default:
		return key[:4] + "..." + key[n-4:]
	}
}

func isHexString(s string) bool {
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return len(s) > 0
}

func sanitizeHealthError(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if r < 0x20 {
			return -1
		}
		return r
	}, s)
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}
