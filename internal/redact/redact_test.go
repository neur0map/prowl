package redact

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestTextMasksProviderCredentials(t *testing.T) {
	cases := []struct{ name, in, leak string }{
		{"stripe", `STRIPE_TOKEN = "sk_live_` + `51H8xQ2eZvKYlo2CabcdefghijklmnopQRST"`, "sk_live_" + "51H8xQ2eZvKYlo2C"},
		{"openai", `key = "sk-proj-` + `1234567890abcdefGHIJKLMNOPqrstuvwxyz0987654321"`, "sk-proj-" + "1234567890abcdef"},
		{"github", `token: ghp_` + `1234567890abcdefghijklmnopqrstuvwx`, "ghp_" + "1234567890abcdef"},
		{"aws id", `AWS_ACCESS_KEY_ID=AKIA` + `IOSFODNN7EXAMPLE`, "AKIA" + "IOSFODNN7EXAMPLE"},
		{"url creds", `DATABASE_URL=postgres://admin:sup3rs3cr3tpw@db.internal:5432/prod`, "sup3rs3cr3tpw"},
		// R27: one positive fixture per alternation branch not otherwise covered,
		// so a typo in any pre-filter trigger fails a case instead of silently
		// disabling masking for that credential class.
		{"stripe pk", `pk_live_ABCDEFGHIJ0123456789xyz`, "pk_live_ABCDEFGHIJ"},
		{"stripe rk", `rk_test_ABCDEFGHIJ0123456789xyz`, "rk_test_ABCDEFGHIJ"},
		{"github oauth", `gho_` + `ABCDEFGHIJ0123456789abcdefghij`, "gho_" + "ABCDEFGHIJ"},
		{"github user", `ghu_ABCDEFGHIJ0123456789abcdefghij`, "ghu_ABCDEFGHIJ"},
		{"github server", `ghs_ABCDEFGHIJ0123456789abcdefghij`, "ghs_ABCDEFGHIJ"},
		{"github refresh", `ghr_ABCDEFGHIJ0123456789abcdefghij`, "ghr_ABCDEFGHIJ"},
		{"slack", `SLACK_TOKEN=xoxb-` + `1234567890-ABCDEFabcdef0123`, "xoxb-" + "1234567890"},
		{"google", `GOOGLE_API_KEY=AIza` + `SyD1234567890abcdefghijklmnopqrstuv`, "AIza" + "SyD1234567890"},
		{"jwt", `Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJVadQssw5c`, "eyJhbGciOiJIUzI1NiJ9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, n := Text(c.in)
			if n == 0 {
				t.Fatalf("nothing masked in %q", c.in)
			}
			if strings.Contains(got, c.leak) {
				t.Fatalf("secret survived masking: %q", got)
			}
			if !strings.Contains(got, Mask) {
				t.Fatalf("no mask marker in %q", got)
			}
		})
	}
}

// The identifier must survive so retrieval still answers "where is this set".
func TestTextKeepsSurroundingCode(t *testing.T) {
	got, n := Text(`STRIPE_TOKEN = "sk_live_` + `51H8xQ2eZvKYlo2CabcdefghijklmnopQRST"`)
	if n != 1 {
		t.Fatalf("masked %d values, want 1", n)
	}
	if !strings.Contains(got, "STRIPE_TOKEN") {
		t.Fatalf("identifier lost: %q", got)
	}
}

// False positives are the real risk: masking ordinary code would hide the very
// lines an agent is looking for. Under R13 the generic heuristic is gone, so
// every one of these -- including secret-named prose, paths, timestamps, and the
// sk- boundary case -- must be left byte-identical.
func TestTextLeavesOrdinaryCodeAlone(t *testing.T) {
	ordinary := []string{
		`func BuildVectors(ctx context.Context, s *store.Store) (VectorPass, error) {`,
		`const releaseAt = "https://github.com/neur0map/prowl/releases/download/"`,
		`hash := sha256.Sum256(body)`,
		`// Applies pattern matching to mask API keys, tokens, and credentials.`,
		`import "github.com/neur0map/prowl/internal/store"`,
		`css := "#cdd6f4"`,
		`BudgetTokens: budgetTokens,`,
		`Author: proposal.Author,`,
		`MaxTokens:    rerankMaxTokens(len(candidates))`,
		`const tokensDocURL = "github.com/neur0map/prowl/blob/main/docs/TOKENS.md"`,
		`sum := "d41d8cd98f00b204e9800998ecf8427e"`,
		`auth_note = "Use the token from the dashboard"`,
		`private_key_path = "C:\\Users\\me\\keys\\id_rsa"`,
		`token_expires_at = "2026-08-17T12:34:56.000Z"`,
		`disk-space-check-interval`,
	}
	for _, in := range ordinary {
		got, n := Text(in)
		if n != 0 {
			t.Fatalf("false positive on %q -> %q (masked %d)", in, got, n)
		}
		if got != in {
			t.Fatalf("output changed on %q -> %q", in, got)
		}
	}
}

// R13 recall tradeoff: a homegrown credential with no vendor prefix assigned to a
// secret-named variable is deliberately NOT masked. Asserted explicitly so the
// loss is visible and intentional, not an accident.
func TestTextDoesNotMaskUnprefixedSecret(t *testing.T) {
	in := `API_KEY = "8f3kd93kfj39dk20fjs93jf"`
	got, n := Text(in)
	if n != 0 {
		t.Fatalf("expected no masking under R13, masked %d -> %q", n, got)
	}
	if got != in {
		t.Fatalf("output changed: %q", got)
	}
}

// A complete PEM block: the body is destroyed but BOTH markers survive.
func TestTextMasksPairedPEM(t *testing.T) {
	in := "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA1234\n-----END RSA PRIVATE KEY-----"
	got, n := Text(in)
	if n != 1 {
		t.Fatalf("masked %d values, want 1: %q", n, got)
	}
	if strings.Contains(got, "MIIEowIBAAKCAQEA1234") {
		t.Fatalf("key body survived: %q", got)
	}
	if !strings.Contains(got, "-----BEGIN RSA PRIVATE KEY-----") {
		t.Fatalf("BEGIN marker lost: %q", got)
	}
	if !strings.Contains(got, "-----END RSA PRIVATE KEY-----") {
		t.Fatalf("END marker lost: %q", got)
	}
}

// P0 regression: a paired block preceded by source must not destroy that source.
// The single-alternation regex let Go's leftmost-match rule prefer a start-anchored
// tail alternative, wiping everything before the BEGIN marker.
func TestTextMasksPairedPEMPrecededBySource(t *testing.T) {
	in := "func loadKey() {\n-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA1234\n-----END RSA PRIVATE KEY-----\n}"
	got, n := Text(in)
	if n != 1 {
		t.Fatalf("masked %d values, want 1: %q", n, got)
	}
	if !strings.HasPrefix(got, "func loadKey() {\n") {
		t.Fatalf("preceding source lost: %q", got)
	}
	if !strings.HasSuffix(got, "\n}") {
		t.Fatalf("trailing source lost: %q", got)
	}
	if strings.Contains(got, "MIIEowIBAAKCAQEA1234") {
		t.Fatalf("key body survived: %q", got)
	}
	if !strings.Contains(got, "-----BEGIN RSA PRIVATE KEY-----") {
		t.Fatalf("BEGIN marker lost: %q", got)
	}
	if !strings.Contains(got, "-----END RSA PRIVATE KEY-----") {
		t.Fatalf("END marker lost: %q", got)
	}
}

// Head half of a key split across chunks: BEGIN marker with a body and no END.
func TestTextMasksUnpairedBeginPEM(t *testing.T) {
	in := "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA1234\n"
	got, n := Text(in)
	if n != 1 {
		t.Fatalf("masked %d values, want 1: %q", n, got)
	}
	if strings.Contains(got, "MIIEowIBAAKCAQEA1234") {
		t.Fatalf("key body survived: %q", got)
	}
	if !strings.Contains(got, "-----BEGIN RSA PRIVATE KEY-----") {
		t.Fatalf("BEGIN marker lost: %q", got)
	}
	if !strings.Contains(got, Mask) {
		t.Fatalf("no mask marker in %q", got)
	}
}

// Tail half of a split key: a body and an END marker with no BEGIN.
func TestTextMasksUnpairedEndPEM(t *testing.T) {
	in := "MIIEowIBAAKCAQEA1234\n-----END RSA PRIVATE KEY-----"
	got, n := Text(in)
	if n != 1 {
		t.Fatalf("masked %d values, want 1: %q", n, got)
	}
	if strings.Contains(got, "MIIEowIBAAKCAQEA1234") {
		t.Fatalf("key body survived: %q", got)
	}
	if !strings.Contains(got, "-----END RSA PRIVATE KEY-----") {
		t.Fatalf("END marker lost: %q", got)
	}
	if !strings.Contains(got, Mask) {
		t.Fatalf("no mask marker in %q", got)
	}
}

// A second pass over already-masked text masks nothing and changes nothing.
func TestTextIdempotence(t *testing.T) {
	cases := []struct{ name, in string }{
		{"provider", `STRIPE_TOKEN = "sk_live_` + `51H8xQ2eZvKYlo2CabcdefghijklmnopQRST"`},
		{"url", `DATABASE_URL=postgres://user:secretPassword123456789@db.example.com:5432/db`},
		{"paired pem", "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA1234\n-----END RSA PRIVATE KEY-----"},
		{"unpaired begin pem", "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA1234\n"},
		{"unpaired end pem", "MIIEowIBAAKCAQEA1234\n-----END RSA PRIVATE KEY-----"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			masked1, n1 := Text(c.in)
			if n1 != 1 {
				t.Fatalf("first pass masked %d, want 1: %q", n1, masked1)
			}
			masked2, n2 := Text(masked1)
			if n2 != 0 {
				t.Fatalf("second pass masked %d, want 0: %q", n2, masked2)
			}
			if masked1 != masked2 {
				t.Fatalf("not idempotent: %q != %q", masked1, masked2)
			}
		})
	}
}

// Inputs with no key material to redact must not inflate the count: a lone END
// marker with no body, and a paired block whose body is empty.
func TestTextNoCountInflation(t *testing.T) {
	cases := []struct{ name, in string }{
		{"lone end marker", "-----END RSA PRIVATE KEY-----"},
		{"empty body", "-----BEGIN RSA PRIVATE KEY-----" + "-----END RSA PRIVATE KEY-----"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, n := Text(c.in)
			if n != 0 {
				t.Fatalf("count inflated to %d on %q -> %q", n, c.in, got)
			}
			if got != c.in {
				t.Fatalf("output changed: %q", got)
			}
		})
	}
}

// R16: all three PEM patterns run unconditionally with bounded unpaired bodies,
// so a chunk holding more than one key redacts every body without one match
// destroying another. Covers the reported residual leak (a complete key plus a
// second key's split half) and two complete keys sharing a chunk. Each case must
// also be idempotent.
func TestTextMasksMultipleKeysInChunk(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		bodiesGone []string
		markers    []string
		wantN      int
	}{
		{
			name:       "complete key plus split half",
			in:         "-----BEGIN A PRIVATE KEY-----\nBODYONE\n-----END A PRIVATE KEY-----\n-----BEGIN B PRIVATE KEY-----\nLEAKEDBODYTWO",
			bodiesGone: []string{"BODYONE", "LEAKEDBODYTWO"},
			markers:    []string{"-----BEGIN A PRIVATE KEY-----", "-----END A PRIVATE KEY-----", "-----BEGIN B PRIVATE KEY-----"},
			wantN:      2,
		},
		{
			name:       "two complete keys",
			in:         "-----BEGIN A PRIVATE KEY-----\nBODYONE\n-----END A PRIVATE KEY-----\n-----BEGIN B PRIVATE KEY-----\nBODYTWO\n-----END B PRIVATE KEY-----",
			bodiesGone: []string{"BODYONE", "BODYTWO"},
			markers:    []string{"-----BEGIN A PRIVATE KEY-----", "-----END A PRIVATE KEY-----", "-----BEGIN B PRIVATE KEY-----", "-----END B PRIVATE KEY-----"},
			wantN:      2,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, n := Text(c.in)
			if n != c.wantN {
				t.Fatalf("masked %d values, want %d: %q", n, c.wantN, got)
			}
			for _, b := range c.bodiesGone {
				if strings.Contains(got, b) {
					t.Fatalf("key body %q survived: %q", b, got)
				}
			}
			for _, m := range c.markers {
				if !strings.Contains(got, m) {
					t.Fatalf("marker %q lost: %q", m, got)
				}
			}
			got2, n2 := Text(got)
			if n2 != 0 {
				t.Fatalf("second pass masked %d, want 0: %q", n2, got2)
			}
			if got2 != got {
				t.Fatalf("not idempotent: %q != %q", got, got2)
			}
		})
	}
}

// R17: a head chunk cut inside a marker's opening dash run leaves a trailing dash
// run with no content after it. The trailing `-*` in pemBegin lets that body be
// masked rather than leaking the key material. (A cut that runs past the opening
// dashes into the END keyword, e.g. "...\n-----END RSA PRIVATE", shares the same
// five-dash prefix the anchored bound must reject to stay safe against a live
// marker, so it cannot be recovered without risking the P0 regression; it is
// unreachable in practice given line-aligned chunking. See the R18 note.)
func TestTextMasksHeadChunkCutInMarkerDashes(t *testing.T) {
	in := "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA1234\n-----"
	got, n := Text(in)
	if n != 1 {
		t.Fatalf("masked %d values, want 1: %q", n, got)
	}
	if strings.Contains(got, "MIIEowIBAAKCAQEA1234") {
		t.Fatalf("key body survived: %q", got)
	}
	if !strings.Contains(got, "-----BEGIN RSA PRIVATE KEY-----") {
		t.Fatalf("BEGIN marker lost: %q", got)
	}
	if !strings.Contains(got, Mask) {
		t.Fatalf("no mask marker in %q", got)
	}
	got2, n2 := Text(got)
	if n2 != 0 || got2 != got {
		t.Fatalf("not idempotent: n=%d %q != %q", n2, got2, got)
	}
}

// R23: URL userinfo in documentation uses placeholder passwords that the
// four-character password floor cannot tell from real credentials. A documented
// placeholder set is skipped so prose survives byte-identical, while a real weak
// password is still masked.
func TestTextSkipsPlaceholderURLPasswords(t *testing.T) {
	identical := []string{
		`# URLs containing userinfo -- scheme://user:pass@host`,
		`# Match userinfo in both absolute (scheme://user:pass@host)`,
		`conn := "scheme://user:<password>@host"`,
		`db = "postgres://user:${DB_PASSWORD}@host:5432/db"`,
	}
	for _, in := range identical {
		if got, n := Text(in); got != in || n != 0 {
			t.Errorf("Text(%q) = %q, %d; want unchanged, 0", in, got, n)
		}
	}
	real := `postgres://admin:sup3rs3cr3tpw@db:5432/prod`
	got, n := Text(real)
	if n != 1 || strings.Contains(got, "sup3rs3cr3tpw") || !strings.Contains(got, Mask) {
		t.Errorf("Text(%q) = %q, %d; want the password masked", real, got, n)
	}
}

// Finding 1 (release blocker): the sk- provider branch must not mask kebab-case
// identifiers or documentation placeholders. A real vendor sk- key is a long
// random run (OpenAI/DeepSeek sk-+48, Anthropic sk-ant-api03-+95, OpenRouter
// sk-or-v1-+64), so its body is >=40 chars, or -- for a short OpenAI-compatible
// key -- a single dash-free run of >=24 chars. A false positive is a
// human-readable dash-separated identifier or an sk-your-... placeholder, always
// short and dash-separated. These must survive byte-identical. Includes the
// reviewer's four verified examples and the thirteen dash-separated (or
// too-short) fixtures from the Hermes-corpus measurement.
func TestTextKeepsSkKebabAndPlaceholders(t *testing.T) {
	survive := []string{
		// Reviewer's verified false positives (from other repositories / a real corpus).
		`--sk-color-primary-background`,
		`sk-fading-circle-container`,
		`sk-chase-dot-before-animation`,
		`OPENAI_API_KEY=sk-your-openai-key-here`,
		// Dash-separated credential-shaped fixtures measured in the Hermes corpus:
		// short and dash-separated, structurally indistinguishable from an
		// identifier. Skipped because destroying a real dash-separated identifier
		// is exactly the failure this blocker is about; the fixtures they mimic
		// carry no real secret, so the recall loss is on synthetic data only.
		`sk-ant-` + `api-rotated-away`,
		`sk-ant-` + `oat-cc-test-token`,
		`sk-ant-` + `oat-legacy-manual`,
		`sk-ant-` + `setup-oauth-token`,
		`sk-ant-` + `real-value-cafebabe`,
		`sk-live-deadbeefdeadbeef`,
		`sk-live-example-provider`,
		`sk-live-neuralwatt-secret`,
		`sk-future-secret-token-xyz`,
		`sk-real-upstream-value-deadbeef`,
		`sk-super-secret-should-not-leak`,
		`sk-super-secret-value-do-not-log`,
		`sk-xxxxxxxxxxxxxxxxxxxx`, // dash-free but 20<24: too short to be any real key
	}
	for _, in := range survive {
		got, n := Text(in)
		if n != 0 || got != in {
			t.Errorf("Text(%q) = %q, %d; want unchanged, 0", in, got, n)
		}
	}
}

// Finding 1: real vendor sk- keys and dash-free credential runs must still mask.
// Pins the (?:proj-)? simplification (a real sk-proj- key still masks through the
// general branch) and the short-provider recall path (an OpenAI-compatible key of
// ~32 dash-free chars is a single random run and masks via the no-internal-dash
// clause). The lone dash-free fixture from the corpus's 14 (sk-deadbeef...) masks
// because a dash-free hex run is key-shaped, not identifier-shaped.
func TestTextMasksRealSkKeys(t *testing.T) {
	mask := []struct{ name, in, leak string }{
		{"openai project (pins proj- redundancy)", `key = "sk-proj-` + `1234567890abcdefGHIJKLMNOPqrstuvwxyz0987654321"`, "sk-proj-" + "1234567890abcdef"},
		{"openai legacy 48", `sk-` + strings.Repeat("A1b2C3d4", 6), "sk-A1b2C3d4"},
		{"deepseek 48 hex", `sk-` + strings.Repeat("0123456789abcdef", 3), "sk-0123456789abcdef"},
		{"anthropic api03", `sk-ant-` + `api03-` + strings.Repeat("aB3", 32), "sk-ant-" + "api03-aB3"},
		{"short dash-free provider ~33", `sk-` + strings.Repeat("Ab3", 11), "sk-Ab3Ab3"},
		{"dash-free hex fixture 24", `sk-deadbeefdeadbeefdeadbeef`, "sk-deadbeef"},
	}
	for _, c := range mask {
		got, n := Text(c.in)
		if n == 0 || strings.Contains(got, c.leak) || !strings.Contains(got, Mask) {
			t.Errorf("%s: Text(%q) = %q, %d; want masked", c.name, c.in, got, n)
		}
	}
}

// Finding 2 (C4): URL userinfo with an EMPTY username -- the redis:// and amqp://
// connection-string shape that carries a password and no user -- must be masked,
// not stored in cleartext. The placeholder skip must keep working with an empty
// username too.
func TestTextMasksEmptyUsernameURLCreds(t *testing.T) {
	mask := []struct{ in, leak string }{
		{`REDIS_URL=redis://:s3cr3tP4ss@redis.example.com:6379/0`, "s3cr3tP4ss"},
		{`amqp://:guestpassword@rabbit:5672`, "guestpassword"},
	}
	for _, c := range mask {
		got, n := Text(c.in)
		if n != 1 || strings.Contains(got, c.leak) || !strings.Contains(got, Mask) {
			t.Errorf("Text(%q) = %q, %d; want the password masked", c.in, got, n)
		}
	}
	skip := []string{
		`redis://:password@localhost:6379`,
		`amqp://:${RABBIT_PASSWORD}@rabbit`,
	}
	for _, in := range skip {
		if got, n := Text(in); got != in || n != 0 {
			t.Errorf("Text(%q) = %q, %d; want unchanged, 0", in, got, n)
		}
	}
}

// Finding 3: a PEM BEGIN marker named in prose (a comment, README, or error
// string) must not destroy the rest of the chunk. The marker is mid-line in a
// comment and no key material follows, so nothing is masked and the function
// body survives byte-identical.
func TestTextPEMBeginInProseKeepsSource(t *testing.T) {
	in := "// The file must start with -----BEGIN RSA PRIVATE KEY-----\nfunc LoadKey(path string) error {\n\treturn readPEM(path)\n}"
	got, n := Text(in)
	if n != 0 || got != in {
		t.Fatalf("Text(%q) = %q, %d; want unchanged, 0", in, got, n)
	}
}

// Finding 3 (related): a prose mention of a BEGIN marker followed later in the
// same chunk by a real key must mask only the real key, not the intervening
// source. Line-anchoring the BEGIN marker makes pemPaired start at the real key.
func TestTextPEMProseThenRealKeyKeepsIntervening(t *testing.T) {
	in := "// keys look like -----BEGIN RSA PRIVATE KEY----- in prose\ncfg := loadConfig()\n-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA1234\n-----END RSA PRIVATE KEY-----"
	got, n := Text(in)
	if n != 1 {
		t.Fatalf("masked %d, want 1: %q", n, got)
	}
	if !strings.Contains(got, "cfg := loadConfig()") {
		t.Fatalf("intervening source lost: %q", got)
	}
	if !strings.Contains(got, "// keys look like -----BEGIN RSA PRIVATE KEY----- in prose") {
		t.Fatalf("prose comment lost: %q", got)
	}
	if strings.Contains(got, "MIIEowIBAAKCAQEA1234") {
		t.Fatalf("real key body survived: %q", got)
	}
	if !strings.Contains(got, "-----END RSA PRIVATE KEY-----") {
		t.Fatalf("END marker lost: %q", got)
	}
}

// buildChunks tiles src into window-line chunks the way the indexer does for a
// symbol-less region, so a test key is split exactly as it would be on disk.
func buildChunks(src string, window int) []string {
	lines := strings.Split(src, "\n")
	var out []string
	for i := 0; i < len(lines); i += window {
		e := i + window
		if e > len(lines) {
			e = len(lines)
		}
		out = append(out, strings.Join(lines[i:e], "\n"))
	}
	return out
}

// Finding 4 (C4): a private-key body longer than one chunk window tiles into a
// middle chunk with no BEGIN/END marker. Text alone stores that chunk verbatim
// (raw key material at rest); Chunks masks it using whole-file order.
func TestChunksMasksMarkerFreeMiddleOfLongKey(t *testing.T) {
	body := strings.TrimRight(strings.Repeat("MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQDabcdef01234567\n", 98), "\n")
	key := "-----BEGIN PRIVATE KEY-----\n" + body + "\n-----END PRIVATE KEY-----"
	texts := buildChunks(key, 40) // 100 lines -> chunks 1-40, 41-80, 81-100
	if len(texts) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(texts))
	}
	// The middle chunk carries no marker: Text (per-chunk) leaks it.
	if _, n := Text(texts[1]); n != 0 {
		t.Fatalf("precondition: middle chunk should be a marker-free leak under Text, masked %d", n)
	}
	got, total := Chunks(texts)
	if total < 3 {
		t.Fatalf("total masked = %d, want >=3 (head, middle, tail)", total)
	}
	for i, c := range got {
		if strings.Contains(c, "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQDabcdef01234567") {
			t.Fatalf("chunk %d still holds raw key material: %q", i, c)
		}
	}
	if !strings.Contains(got[0], "-----BEGIN PRIVATE KEY-----") || !strings.Contains(got[2], "-----END PRIVATE KEY-----") {
		t.Fatalf("markers lost: %q ... %q", got[0], got[2])
	}
	// Idempotent: a second pass over the masked chunks masks nothing more.
	got2, total2 := Chunks(got)
	if total2 != 0 {
		t.Fatalf("second pass masked %d, want 0", total2)
	}
	for i := range got {
		if got2[i] != got[i] {
			t.Fatalf("not idempotent at chunk %d: %q != %q", i, got2[i], got[i])
		}
	}
}

// Chunks must NOT mask a base64 blob that is not inside a private key: no BEGIN
// precedes it, so it is legitimate embedded data, not key material.
func TestChunksLeavesStandaloneBase64Alone(t *testing.T) {
	blob := strings.TrimRight(strings.Repeat("YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXowMTIzNDU2Nzg5QUJDREVGR0hJSg==\n", 90), "\n")
	texts := buildChunks("const asset = `\n"+blob+"\n`", 40)
	got, total := Chunks(texts)
	if total != 0 {
		t.Fatalf("masked %d in a marker-free base64 blob, want 0", total)
	}
	if strings.Join(got, "\n") != strings.Join(texts, "\n") {
		t.Fatalf("standalone base64 was altered")
	}
}

// A key that fits in two chunks (head+tail, no marker-free middle) is fully
// handled by the per-chunk pass; Chunks must not double-count or over-mask.
func TestChunksTwoChunkKeyUnchangedBehaviour(t *testing.T) {
	body := strings.TrimRight(strings.Repeat("MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcw\n", 50), "\n")
	key := "-----BEGIN PRIVATE KEY-----\n" + body + "\n-----END PRIVATE KEY-----"
	texts := buildChunks(key, 40) // ~52 lines -> 2 chunks, both carry a marker
	if len(texts) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(texts))
	}
	got, total := Chunks(texts)
	if total != 2 {
		t.Fatalf("total masked = %d, want 2 (head, tail)", total)
	}
	if !strings.Contains(got[0], "-----BEGIN PRIVATE KEY-----") || !strings.Contains(got[1], "-----END PRIVATE KEY-----") {
		t.Fatalf("markers lost")
	}
	if strings.Contains(strings.Join(got, "\n"), "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcw") {
		t.Fatalf("key body survived")
	}
}

// Finding 3 regression (C4): a private key embedded in a raw-string literal opens
// with the BEGIN marker MID-LINE (const rsaKey = `-----BEGIN...). The head chunk's
// base64 body must still be masked. Anchoring the marker to the END of its line
// (a required newline after the closing dashes), not the start, is what keeps
// this working while still rejecting a marker named mid-sentence in prose.
func TestTextMasksEmbeddedKeyHeadChunkMidLineBegin(t *testing.T) {
	body := strings.TrimRight(strings.Repeat("MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcw\n", 37), "\n")
	in := "const rsaKey = `-----BEGIN RSA PRIVATE KEY-----\n" + body
	got, n := Text(in)
	if n != 1 {
		t.Fatalf("masked %d, want 1: %q", n, got)
	}
	if strings.Contains(got, "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcw") {
		t.Fatalf("head-chunk key body survived: %q", got)
	}
	if !strings.Contains(got, "const rsaKey = `-----BEGIN RSA PRIVATE KEY-----") {
		t.Fatalf("marker/surrounding source lost: %q", got)
	}
}

// A complete key embedded inline (mid-line BEGIN) in one chunk masks its body
// and keeps the surrounding source byte-identical around the markers.
func TestTextMasksInlineCompleteKeyMidLineBegin(t *testing.T) {
	in := "const k = `-----BEGIN EC PRIVATE KEY-----\nMHcCAQEEIAAAAAAAAAAAAAAAAAAAAAbbbbccccddddeeee\n-----END EC PRIVATE KEY-----`"
	got, n := Text(in)
	if n != 1 {
		t.Fatalf("masked %d, want 1: %q", n, got)
	}
	if strings.Contains(got, "MHcCAQEEIAAAAAAAAAAAAAAAAAAAAAbbbbccccddddeeee") {
		t.Fatalf("inline key body survived: %q", got)
	}
	if !strings.HasPrefix(got, "const k = `-----BEGIN EC PRIVATE KEY-----") || !strings.HasSuffix(got, "-----END EC PRIVATE KEY-----`") {
		t.Fatalf("surrounding source/markers lost: %q", got)
	}
}

// Finding 3/4 embedding matrix: a private key appears in source in many
// separator styles, and each must mask, while a marker merely NAMED in prose in
// each style must survive byte-identical. Built as an explicit matrix so the
// boundary is pinned rather than chased one input shape at a time. `b` is a
// base64 body line; `\n` inside a raw-string literal is a LITERAL backslash-n
// (the .env/JSON/quoted-string escape), while "\n" is a real newline.
func TestTextPEMEmbeddingMatrix(t *testing.T) {
	const b = "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQDabcXYZ0123"
	blob := base64.StdEncoding.EncodeToString([]byte("-----BEGIN RSA PRIVATE KEY-----\n" + b + "\n" + b + "\n-----END RSA PRIVATE KEY-----"))
	cases := []struct {
		name     string
		in       string
		wantMask bool
	}{
		// Real key present -> MUST mask (body b gone, markers kept).
		{"bare multi-line", "-----BEGIN RSA PRIVATE KEY-----\n" + b + "\n" + b + "\n-----END RSA PRIVATE KEY-----", true},
		{"go raw string (mid-line BEGIN)", "var key = `-----BEGIN RSA PRIVATE KEY-----\n" + b + "\n" + b + "\n-----END RSA PRIVATE KEY-----`", true},
		{"quoted string escaped \\n", `PRIVATE_KEY = "-----BEGIN RSA PRIVATE KEY-----\n` + b + `\n` + b + `\n-----END RSA PRIVATE KEY-----"`, true},
		{"json string value escaped \\n", `{"private_key": "-----BEGIN PRIVATE KEY-----\n` + b + `\n` + b + `\n-----END PRIVATE KEY-----\n"}`, true},
		{"yaml block scalar indented", "private_key: |\n  -----BEGIN PRIVATE KEY-----\n  " + b + "\n  " + b + "\n  -----END PRIVATE KEY-----", true},
		// Prose mention only -> MUST survive byte-identical.
		{"prose bare (marker ends line)", "// format is -----BEGIN RSA PRIVATE KEY-----\nfunc load() error { return nil }", false},
		{"prose mid-sentence", "# see -----BEGIN RSA PRIVATE KEY----- for the on-disk format here", false},
		{"prose escaped \\n", `msg := "expected -----BEGIN RSA PRIVATE KEY-----\nbut the file was empty"`, false},
		{"prose json", `{"hint": "keys start with -----BEGIN PRIVATE KEY----- normally"}`, false},
		{"prose yaml", "note: each file opens with -----BEGIN PRIVATE KEY----- as its header", false},
		// Opaque base64 of a WHOLE key -> MUST NOT match (no literal marker; masking
		// arbitrary base64 is the destructive class we refuse).
		{"base64-of-whole-key", blob, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, n := Text(c.in)
			if c.wantMask {
				if n == 0 || strings.Contains(got, b) || !strings.Contains(got, Mask) {
					t.Fatalf("want masked: n=%d bodyPresent=%v got=%q", n, strings.Contains(got, b), got)
				}
				if !strings.Contains(got, "PRIVATE KEY-----") {
					t.Fatalf("markers lost: %q", got)
				}
			} else {
				if n != 0 || got != c.in {
					t.Fatalf("want survive byte-identical: n=%d got=%q", n, got)
				}
			}
			// Idempotent regardless of cell.
			if got2, n2 := Text(got); n2 != 0 || got2 != got {
				t.Fatalf("not idempotent: n2=%d got2=%q", n2, got2)
			}
		})
	}
}
