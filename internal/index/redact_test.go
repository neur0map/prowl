package index

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/parse/extract"
	"github.com/neur0map/prowl/internal/redact"
	"github.com/neur0map/prowl/internal/store"
)

// A credential committed in source must not reach the index. Output filtering is
// not enough: chunks.text and the FTS index are on disk and are read by every
// retrieval path, so masking has to happen before storage. The decisive check
// reads the stored chunk text directly (FirstChunk), not a search result: if
// masking were reimplemented as a filter inside SearchChunks, the raw secret
// would still sit in chunks.text and this assertion would fail. SearchChunks is
// used only to confirm the file is still findable after masking.
func TestIndexNeverStoresSecrets(t *testing.T) {
	root := t.TempDir()
	src := "# Load service credentials for billing.\n" +
		"STRIPE_TOKEN = \"sk_live_" + "51H8xQ2eZvKYlo2CabcdefghijklmnopQRST\"\n\n" +
		"def load_credentials():\n    return STRIPE_TOKEN\n"
	if err := os.WriteFile(filepath.Join(root, "config.py"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := Index(s, root, nil); err != nil {
		t.Fatal(err)
	}
	// files.redacted is the durable record that a value was masked -- the same
	// per-file count doctor reports. Assert through it rather than a summary
	// field kept alive only by this line.
	if rows, err := s.FilesWithRedactions(); err != nil {
		t.Fatal(err)
	} else if len(rows) == 0 {
		t.Fatal("no file recorded a redaction")
	}

	rows, err := s.SearchChunks("credentials", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("precondition: the file must still be findable after masking")
	}

	// Direct at-rest read of the stored chunk text.
	chunk, ok, err := s.FirstChunk("config.py")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("config.py has no stored chunk")
	}
	if strings.Contains(chunk.Text, "sk_live_"+"51H8xQ2eZvKYlo2C") {
		t.Fatalf("secret stored in cleartext in chunks.text: %q", chunk.Text)
	}
	if !strings.Contains(chunk.Text, redact.Mask) {
		t.Fatalf("mask marker absent from stored chunk; masking did not run at index time: %q", chunk.Text)
	}
	if !strings.Contains(chunk.Text, "STRIPE_TOKEN") {
		t.Fatalf("identifier destroyed along with the value: %q", chunk.Text)
	}
}

// R28: the same credential is masked in a chunk, a symbol signature and a
// resource value, but the reported count is the chunk pass alone -- one masked
// value is one, not three. Masking must still happen at every sink (C4).
func TestMapResultCountsChunkPassOnly(t *testing.T) {
	secret := "sk_live_" + "51H8xQ2eZvKYlo2CabcdefghijklmnopQRST"
	line := "api_key = " + secret
	r := extract.Result{
		Symbols:   []extract.Symbol{{Name: "api_key", Kind: "setting", Signature: line, StartLine: 1, EndLine: 1}},
		Resources: []extract.Resource{{Kind: "var", Name: "api_key", Value: secret, Line: 1}},
		Chunks:    []extract.Chunk{{StartLine: 1, EndLine: 1, Text: line}},
	}
	syms, ress, _, chunks, n := mapResult(r)
	if n != 1 {
		t.Fatalf("redacted count = %d, want 1 (chunk pass only)", n)
	}
	if strings.Contains(chunks[0].Text, "sk_live") {
		t.Errorf("chunk not masked: %q", chunks[0].Text)
	}
	if strings.Contains(syms[0].Signature, "sk_live") {
		t.Errorf("signature not masked (C4 regression): %q", syms[0].Signature)
	}
	if strings.Contains(ress[0].Value, "sk_live") {
		t.Errorf("resource value not masked (C4 regression): %q", ress[0].Value)
	}
}

// Two distinct credentials on two lines are two masked values.
func TestMapResultCountsDistinctCredentials(t *testing.T) {
	text := "api_key = sk_live_" + "51H8xQ2eZvKYlo2CabcdefghijklmnopQRST\naws = AKIA" + "IOSFODNN7EXAMPLE"
	_, _, _, _, n := mapResult(extract.Result{Chunks: []extract.Chunk{{StartLine: 1, EndLine: 2, Text: text}}})
	if n != 2 {
		t.Fatalf("redacted count = %d, want 2", n)
	}
}

// A doc comment is verbatim source and a new at-rest sink: a docstring showing
// an example connection string can carry a credential (C4). mapResult must mask
// it before it reaches symbols.doc, but must NOT count it -- the doc's text also
// lives inside the enclosing chunk, and the count is the chunk pass alone, so
// counting the doc too would re-count one credential (the R28 double-count fix).
func TestMapResultMasksDocWithoutRecounting(t *testing.T) {
	secret := "sk_live_" + "51H8xQ2eZvKYlo2CabcdefghijklmnopQRST"
	doc := "// connect opens the billing session using key " + secret + "."
	r := extract.Result{
		Symbols: []extract.Symbol{{Name: "connect", Kind: "function", Doc: doc, StartLine: 2, EndLine: 4}},
		Chunks:  []extract.Chunk{{StartLine: 1, EndLine: 4, Text: doc + "\nfunc connect() {}"}},
	}
	syms, _, _, _, n := mapResult(r)
	if strings.Contains(syms[0].Doc, "sk_live") {
		t.Fatalf("doc not masked before storage (C4): %q", syms[0].Doc)
	}
	if !strings.Contains(syms[0].Doc, redact.Mask) {
		t.Fatalf("mask marker absent from doc; masking did not run: %q", syms[0].Doc)
	}
	if n != 1 {
		t.Fatalf("redacted count = %d, want 1 (doc masked but not counted)", n)
	}
}

// End-to-end through Index: files.redacted counts each credential once, tracks
// reindexing, and returns to zero when the credential is removed from source. A
// generic .conf setting masks the value in both a signature and a chunk, so the
// old all-sinks sum would report 2 where this must report 1.
func TestFilesRedactedCountsCredentialsOncePerFile(t *testing.T) {
	secret := "sk_live_" + "51H8xQ2eZvKYlo2CabcdefghijklmnopQRST"
	root := t.TempDir()
	conf := filepath.Join(root, "app.conf")
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	count := func() int {
		rows, err := s.FilesWithRedactions()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.File == "app.conf" {
				return r.Count
			}
		}
		return 0
	}
	reindex := func(body string) {
		if err := os.WriteFile(conf, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Index(s, root, nil); err != nil {
			t.Fatal(err)
		}
	}

	reindex("title = my service\napi_key = " + secret + "\n")
	if got := count(); got != 1 {
		t.Fatalf("one credential: files.redacted = %d, want 1", got)
	}
	if _, err := Index(s, root, nil); err != nil { // reindex unchanged tree
		t.Fatal(err)
	}
	if got := count(); got != 1 {
		t.Fatalf("reindex unchanged: files.redacted = %d, want 1", got)
	}
	reindex("api_key = " + secret + "\naws = AKIA" + "IOSFODNN7EXAMPLE\n")
	if got := count(); got != 2 {
		t.Fatalf("two credentials: files.redacted = %d, want 2", got)
	}
	reindex("api_key = set-me-in-the-environment\naws = none\n")
	if got := count(); got != 0 {
		t.Fatalf("after removing credentials: files.redacted = %d, want 0", got)
	}
}

// C4 at rest: a private-key body longer than one chunk window tiles into a
// middle chunk that carries no BEGIN/END marker. Per-chunk redact.Text scans for
// a marker and skips such a chunk, so it would be stored as raw key material.
// mapResult routes chunk text through redact.Chunks, which masks a marker-free
// chunk seen while a key opened by an earlier BEGIN has not been closed -- this
// pins that mapResult uses that path, so a revert to per-chunk Text fails here.
func TestMapResultMasksMarkerFreeMiddleChunk(t *testing.T) {
	body := "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7VJTUt9Us8cKj"
	r := extract.Result{Chunks: []extract.Chunk{
		{StartLine: 1, EndLine: 3, Text: "-----BEGIN RSA PRIVATE KEY-----\n" + body + "\n" + body},
		{StartLine: 4, EndLine: 5, Text: body + "\n" + body}, // marker-free middle: raw key body
		{StartLine: 6, EndLine: 7, Text: body + "\n-----END RSA PRIVATE KEY-----"},
	}}
	_, _, _, chunks, n := mapResult(r)
	if strings.Contains(chunks[1].Text, body) {
		t.Fatalf("marker-free middle chunk stored raw key body at rest (C4): %q", chunks[1].Text)
	}
	if chunks[1].Text != redact.Mask {
		t.Fatalf("middle chunk not masked wholesale: %q", chunks[1].Text)
	}
	if n < 3 {
		t.Fatalf("count = %d, want >= 3 (head, middle, tail each masked)", n)
	}
}
