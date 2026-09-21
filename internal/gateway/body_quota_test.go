package gateway

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestABodyPublishedBalanceBecomesAnObservation covers the signal a
// header-only observer cannot see: Hyper states the remaining balance in the
// payload, so without this an enrolled Hyper login reports "no published
// quota" no matter how much of its balance is gone.
func TestABodyPublishedBalanceBecomesAnObservation(t *testing.T) {
	t.Parallel()

	ledger, _ := newQuotaTestLedger(t)
	body := []byte(`{"id":"x","usage":{"prompt_tokens":90,"completion_tokens":16,
		"cost":{"usd":0.0001,"hypercredits":0.002},
		"remaining":{"hypercredits":55.641165814}}}`)

	require.True(t, ledger.ObserveBodyQuota("hyper", 7, body))

	var remaining int64
	require.NoError(t, ledger.db.QueryRow(`
		SELECT tokens_remaining FROM provider_quota_state
		 WHERE quota_pool_key = ?`, PoolKey("hyper", 7)).Scan(&remaining))
	require.Equal(t, int64(56), remaining, "a balance is rounded, never truncated to zero")

	var raw string
	require.NoError(t, ledger.db.QueryRow(`
		SELECT headers_json FROM provider_quota_observations
		 WHERE quota_pool_key = ? ORDER BY id DESC LIMIT 1`,
		PoolKey("hyper", 7)).Scan(&raw))
	require.Equal(t, "55.641165814 credits remaining", raw,
		"the logged note must keep what the provider actually said")
}

// TestNoBalanceMeansNoObservation keeps the rule that an absent signal leaves
// no trace: a fabricated zero would bench a healthy pool.
func TestNoBalanceMeansNoObservation(t *testing.T) {
	t.Parallel()

	ledger, _ := newQuotaTestLedger(t)
	require.False(t, ledger.ObserveBodyQuota("hyper", 7,
		[]byte(`{"id":"x","usage":{"prompt_tokens":1,"completion_tokens":1}}`)),
		"a response without the field must not record anything")
	require.False(t, ledger.ObserveBodyQuota("groq", 7,
		[]byte(`{"usage":{"remaining":{"hypercredits":5}}}`)),
		"only platforms known to publish a balance are read")

	var rows int
	require.NoError(t, ledger.db.QueryRow(
		"SELECT COUNT(*) FROM provider_quota_state").Scan(&rows))
	require.Zero(t, rows)
}

// TestAZeroBalanceIsRecorded separates "spent" from "unknown", which is the
// distinction key selection depends on.
func TestAZeroBalanceIsRecorded(t *testing.T) {
	t.Parallel()

	ledger, _ := newQuotaTestLedger(t)
	require.True(t, ledger.ObserveBodyQuota("hyper", 3,
		[]byte(`{"usage":{"remaining":{"hypercredits":0}}}`)))

	var remaining int64
	require.NoError(t, ledger.db.QueryRow(`
		SELECT tokens_remaining FROM provider_quota_state WHERE quota_pool_key = ?`,
		PoolKey("hyper", 3)).Scan(&remaining))
	require.Zero(t, remaining, "an exhausted balance is a reading, not a missing signal")
}
