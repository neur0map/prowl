package embed

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func newTestEmbedder(t *testing.T) *Embedder {
	t.Helper()
	modelDir := filepath.Join(os.TempDir(), "prowl-test-models")
	emb, err := New(modelDir)
	if err != nil {
		t.Skipf("skipping: model download failed (likely no network): %v", err)
	}
	t.Cleanup(func() { emb.Close() })
	return emb
}

func l2Norm(v []float32) float64 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	return math.Sqrt(sum)
}

func cosineSim(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		va := float64(a[i])
		vb := float64(b[i])
		dot += va * vb
		na += va * va
		nb += vb * vb
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func TestEmbedderEncode(t *testing.T) {
	emb := newTestEmbedder(t)

	texts := []string{
		"function that handles authentication",
		"database connection pool manager",
	}
	vecs, err := emb.Encode(texts)
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 2 {
		t.Fatalf("expected 2 vectors, got %d", len(vecs))
	}
	for i, v := range vecs {
		if len(v) != 384 {
			t.Errorf("vector %d: expected dim 384, got %d", i, len(v))
		}
		norm := l2Norm(v)
		if math.Abs(norm-1.0) > 0.01 {
			t.Errorf("vector %d: L2 norm = %f, expected ~1.0", i, norm)
		}
	}

	if emb.Dim() != 384 {
		t.Errorf("expected Dim() = 384, got %d", emb.Dim())
	}
}

func TestEmbedderBatching(t *testing.T) {
	emb := newTestEmbedder(t)

	// 40 texts to trigger batching (batch size is 32)
	texts := make([]string, 40)
	for i := range texts {
		texts[i] = "test string for batching"
	}
	vecs, err := emb.Encode(texts)
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 40 {
		t.Fatalf("expected 40 vectors, got %d", len(vecs))
	}
	for i, v := range vecs {
		if len(v) != 384 {
			t.Errorf("vector %d: expected dim 384, got %d", i, len(v))
		}
	}
}

func TestSimilarTextsCluster(t *testing.T) {
	emb := newTestEmbedder(t)

	texts := []string{
		"function that handles authentication",
		"auth login handler",
		"database connection pool",
	}
	vecs, err := emb.Encode(texts)
	if err != nil {
		t.Fatal(err)
	}

	authSim := cosineSim(vecs[0], vecs[1])
	dbAuthSim0 := cosineSim(vecs[0], vecs[2])
	dbAuthSim1 := cosineSim(vecs[1], vecs[2])

	t.Logf("auth pair similarity: %.4f", authSim)
	t.Logf("auth[0] vs db: %.4f", dbAuthSim0)
	t.Logf("auth[1] vs db: %.4f", dbAuthSim1)

	if authSim < 0.5 {
		t.Errorf("expected auth pair similarity > 0.5, got %.4f", authSim)
	}
	if dbAuthSim0 >= authSim {
		t.Errorf("expected db-auth similarity (%.4f) < auth pair similarity (%.4f)", dbAuthSim0, authSim)
	}
	if dbAuthSim1 >= authSim {
		t.Errorf("expected db-auth similarity (%.4f) < auth pair similarity (%.4f)", dbAuthSim1, authSim)
	}
}
