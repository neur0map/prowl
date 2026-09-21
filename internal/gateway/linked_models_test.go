package gateway

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEnrolledLoginBecomesRoutable is the whole point of enrolling: a
// subscription's models have to reach the catalogue and the routing chain.
// Enrolling used to add a credential and nothing else, so the router had a key
// with nothing to send to it and the Models page showed no change.
func TestEnrolledLoginBecomesRoutable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	eng, err := OpenEngine(ctx, t.TempDir(), EngineOptions{SkipCatalogSeed: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })

	keyID, err := eng.Vault().AddLinked(ctx, "anthropic", "Prowl login")
	require.NoError(t, err)

	seeded, err := eng.SeedLoginModels(ctx, keyID, "anthropic", []LinkedModel{
		{ID: "claude-opus-5", Name: "Claude Opus 5", ContextWindow: 1_000_000,
			InputPerM: 5, OutputPerM: 25, CanReason: true, Attachments: true, Tools: true},
		{ID: "claude-haiku-4-5", Name: "Claude Haiku 4.5", ContextWindow: 200_000,
			InputPerM: 0.8, OutputPerM: 4, Tools: true, Small: true},
	})
	require.NoError(t, err)
	require.Equal(t, 2, seeded)

	// In the catalogue, owned by the login and priced as published. Catalogue
	// ranks are ordinal: lower means more capable/faster.
	rows, err := eng.DB().Query(`
		SELECT model_id, intelligence_rank, speed_rank, size_label, context_window,
		       supports_tools, paid_output_per_m, source, key_id
		  FROM models WHERE platform = 'anthropic' ORDER BY intelligence_rank ASC`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	type row struct {
		id                  string
		intelligence, speed int
		size                string
		ctx                 int64
		tools               int
		outPerM             float64
		source              string
		keyID               int64
	}
	var got []row
	for rows.Next() {
		var r row
		require.NoError(t, rows.Scan(&r.id, &r.intelligence, &r.speed, &r.size, &r.ctx,
			&r.tools, &r.outPerM, &r.source, &r.keyID))
		got = append(got, r)
	}
	require.Len(t, got, 2)

	// The flagship gets the strongest capability rank/tier; the small model
	// gets the strongest speed rank.
	require.Equal(t, "claude-opus-5", got[0].id)
	require.Less(t, got[0].intelligence, got[1].intelligence)
	require.Greater(t, got[0].speed, got[1].speed, "the small model must rank faster")
	require.Equal(t, "Large", got[0].size)
	require.Equal(t, "Small", got[1].size)
	require.Equal(t, int64(1_000_000), got[0].ctx)
	require.Equal(t, 1, got[0].tools)
	require.InDelta(t, 25.0, got[0].outPerM, 0.001, "the published price must be kept")
	require.Equal(t, "login", got[0].source)
	require.Equal(t, keyID, got[0].keyID)

	// And in the chain, or the router still cannot reach them.
	var inChain int
	require.NoError(t, eng.DB().QueryRow(`
		SELECT COUNT(*) FROM fallback_config f
		  JOIN models m ON m.id = f.model_db_id
		 WHERE m.platform = 'anthropic' AND f.enabled = 1`).Scan(&inChain))
	require.Equal(t, 2, inChain, "an enrolled login's models must be routable")

	require.Equal(t, 2, eng.LoginModelCount(ctx, keyID))
}

// TestWithdrawingALoginRemovesItsModels keeps a withdrawn subscription from
// leaving models in the chain that nothing can serve.
func TestWithdrawingALoginRemovesItsModels(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	eng, err := OpenEngine(ctx, t.TempDir(), EngineOptions{SkipCatalogSeed: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })

	keyID, err := eng.Vault().AddLinked(ctx, "openai", "Prowl login")
	require.NoError(t, err)
	_, err = eng.SeedLoginModels(ctx, keyID, "openai", []LinkedModel{
		{ID: "gpt-5.2-codex", OutputPerM: 12, Tools: true},
	})
	require.NoError(t, err)

	require.NoError(t, eng.Vault().Delete(ctx, keyID))

	var models, chain int
	require.NoError(t, eng.DB().QueryRow(
		"SELECT COUNT(*) FROM models WHERE platform = 'openai'").Scan(&models))
	require.NoError(t, eng.DB().QueryRow(
		"SELECT COUNT(*) FROM fallback_config").Scan(&chain))
	require.Zero(t, models, "withdrawing must not leave unservable models")
	require.Zero(t, chain, "withdrawing must not leave them in the chain")
}

// TestSubscriptionWithoutPricesStillOrders covers a login that publishes no
// per-token price at all: the vendor's own large/small defaults are the only
// signal, and they must still produce an order.
func TestSubscriptionWithoutPricesStillOrders(t *testing.T) {
	t.Parallel()

	ranked := rankLinkedModels([]LinkedModel{
		{ID: "mid"},
		{ID: "small-one", Small: true},
		{ID: "flagship", Flagship: true},
	})
	require.Equal(t, "flagship", ranked[0].model.ID)
	require.Equal(t, "small-one", ranked[len(ranked)-1].model.ID)
}

// TestRankingPrefersTheVendorsOrderOverPrice is the trap price-based ranking
// walks into. Anthropic's newer flagships are CHEAPER than its older ones, so
// ordering by output price put claude-opus-4-1 ($75/M) above claude-opus-5
// ($25/M) - the hardest work would have gone to the previous generation while
// the current flagship looked like a mid-tier model.
func TestRankingPrefersTheVendorsOrderOverPrice(t *testing.T) {
	t.Parallel()

	// Catalogue order as the vendor publishes it: current generation first.
	ranked := rankLinkedModels([]LinkedModel{
		{ID: "claude-opus-5", OutputPerM: 25},
		{ID: "claude-opus-4-8", OutputPerM: 25},
		{ID: "claude-opus-4-1", OutputPerM: 75},
		{ID: "claude-haiku-4-5", OutputPerM: 4, Small: true},
	})

	order := make([]string, 0, len(ranked))
	for _, r := range ranked {
		order = append(order, r.model.ID)
	}
	require.Equal(t, "claude-opus-5", order[0],
		"the vendor's current flagship must lead, not the dearest legacy model")
	require.Less(t, indexOfModel(order, "claude-opus-5"), indexOfModel(order, "claude-opus-4-1"),
		"a newer, cheaper flagship must outrank an older, dearer one")
	require.Equal(t, "claude-haiku-4-5", order[len(order)-1],
		"the vendor's declared small model belongs at the cheap end")
}

// TestDeclaredFlagshipWinsOverPosition covers the explicit signal beating the
// inferred one: a vendor naming its large model means it.
func TestDeclaredFlagshipWinsOverPosition(t *testing.T) {
	t.Parallel()

	ranked := rankLinkedModels([]LinkedModel{
		{ID: "listed-first", OutputPerM: 30},
		{ID: "declared-large", OutputPerM: 10, Flagship: true},
	})
	require.Equal(t, "declared-large", ranked[0].model.ID)
}

func indexOfModel(order []string, id string) int {
	for i, v := range order {
		if v == id {
			return i
		}
	}
	return -1
}

// TestEngineOpenPrunesOrphanChainRows covers a chain row whose model is gone.
// It is unroutable, and it breaks every copy of the chain: creating a routing
// profile fails on the foreign key with "could not seed the profile". Rows
// like that appear whenever models are deleted by a connection that did not
// have foreign keys enabled, so open must not trust the cascade to have run.
func TestEngineOpenPrunesOrphanChainRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()

	first, err := OpenEngine(ctx, dir, EngineOptions{SkipCatalogSeed: true})
	require.NoError(t, err)
	keyID, err := first.Vault().AddLinked(ctx, "anthropic", "")
	require.NoError(t, err)
	_, err = first.SeedLoginModels(ctx, keyID, "anthropic", []LinkedModel{
		{ID: "claude-opus-5", OutputPerM: 25},
	})
	require.NoError(t, err)

	// Delete the model with foreign keys off, exactly as an external sqlite
	// client does by default, so the cascade does not fire.
	_, err = first.DB().Exec("PRAGMA foreign_keys=OFF")
	require.NoError(t, err)
	_, err = first.DB().Exec("DELETE FROM models")
	require.NoError(t, err)

	var orphans int
	require.NoError(t, first.DB().QueryRow(`
		SELECT COUNT(*) FROM fallback_config f
		 LEFT JOIN models m ON m.id = f.model_db_id
		 WHERE m.id IS NULL`).Scan(&orphans))
	require.Positive(t, orphans, "the setup must actually leave an orphan")
	require.NoError(t, first.Close())

	second, err := OpenEngine(ctx, dir, EngineOptions{SkipCatalogSeed: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = second.Close() })

	require.NoError(t, second.DB().QueryRow(`
		SELECT COUNT(*) FROM fallback_config f
		 LEFT JOIN models m ON m.id = f.model_db_id
		 WHERE m.id IS NULL`).Scan(&orphans))
	require.Zero(t, orphans, "open must prune chain rows with no model")
}

// TestLoginModelCoexistsWithCatalogRow is the identity-collision defect: a
// login publishing a model the shipped catalogue already carries must become
// its OWN routing candidate, not overwrite the catalogue row. Enrolling used to
// conflict on (platform, model_id, endpoint_scope=”) and rebind the catalogue
// row to the login key with source='login', so withdrawing the login
// cascade-deleted the shipped model - and its fallback entry - out from under
// the router.
func TestLoginModelCoexistsWithCatalogRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	eng, err := OpenEngine(ctx, t.TempDir(), EngineOptions{SkipCatalogSeed: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })

	// A shipped catalogue row: platform-owned, empty endpoint scope, no bound
	// key - the router expands it against whatever platform key is configured.
	res, err := eng.DB().ExecContext(ctx, `
		INSERT INTO models(platform, model_id, display_name, intelligence_rank,
			speed_rank, size_label, enabled, supports_tools, source, endpoint_scope)
		VALUES('anthropic', 'claude-opus-5', 'Claude Opus 5', 2, 2, 'Large', 1, 1, 'catalog', '')`)
	require.NoError(t, err)
	catalogID, err := res.LastInsertId()
	require.NoError(t, err)
	_, err = eng.DB().ExecContext(ctx,
		`INSERT INTO fallback_config(model_db_id, position, enabled) VALUES(?, 1, 1)`, catalogID)
	require.NoError(t, err)

	// A real platform credential the catalogue row can expand onto.
	catalogKeyID, err := eng.Vault().Add("anthropic", "sk-catalog", AddOptions{Label: "byo-key"})
	require.NoError(t, err)

	// Enrol a login that publishes the SAME model id.
	loginKeyID, err := eng.Vault().AddLinked(ctx, "anthropic", "Prowl login")
	require.NoError(t, err)
	seeded, err := eng.SeedLoginModels(ctx, loginKeyID, "anthropic", []LinkedModel{
		{ID: "claude-opus-5", Name: "Claude Opus 5 (sub)", OutputPerM: 25, Tools: true, Flagship: true},
	})
	require.NoError(t, err)
	require.Equal(t, 1, seeded)

	// The shipped row is untouched: not rebound to the login key, source intact.
	catalog := readModelRow(t, eng, catalogID)
	require.Equal(t, "catalog", catalog.source, "the shipped row's source must not be flipped to login")
	require.Equal(t, "", catalog.scope, "the shipped row keeps the empty catalogue scope")
	require.False(t, catalog.keyID.Valid, "enrolling a login must not bind the catalogue row to the login key")

	// The login is a SEPARATE row on its own non-URL scope, bound to its key.
	var loginID int64
	var loginScope string
	var loginRowKey sql.NullInt64
	require.NoError(t, eng.DB().QueryRowContext(ctx, `
		SELECT id, endpoint_scope, key_id FROM models
		 WHERE platform='anthropic' AND model_id='claude-opus-5' AND source='login'`).
		Scan(&loginID, &loginScope, &loginRowKey))
	require.NotEqual(t, catalogID, loginID, "the login must be a separate row, not the catalogue row")
	require.Equal(t, "link:anthropic", loginScope, "the login row carries its own non-URL scope")
	require.True(t, loginRowKey.Valid)
	require.Equal(t, loginKeyID, loginRowKey.Int64, "the login row is bound to the login key")

	// Both are routable with correct credential expansion: the catalogue row
	// expands onto the platform key, the login row onto its own login key.
	catalogVia, loginVia := candidateKeys(t, eng, "claude-opus-5")
	require.Contains(t, catalogVia, catalogKeyID, "the catalogue row must route through the platform key")
	require.Contains(t, loginVia, loginKeyID, "the login row must route through the login key")

	// Withdrawing the login sweeps only its own row and chain entry; the
	// shipped model and its fallback survive and still route.
	require.NoError(t, eng.Vault().Delete(ctx, loginKeyID))

	survivor := readModelRow(t, eng, catalogID)
	require.Equal(t, "catalog", survivor.source, "the catalogue model must survive a login withdrawal")

	var loginRows int
	require.NoError(t, eng.DB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM models WHERE source='login'").Scan(&loginRows))
	require.Zero(t, loginRows, "the login's own row must cascade away")

	var catalogInChain int
	require.NoError(t, eng.DB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM fallback_config WHERE model_db_id = ?", catalogID).Scan(&catalogInChain))
	require.Equal(t, 1, catalogInChain, "the catalogue model's fallback entry must survive")

	catalogVia, _ = candidateKeys(t, eng, "claude-opus-5")
	require.Contains(t, catalogVia, catalogKeyID, "the surviving catalogue model must still route on the platform key")
}

type seededModelRow struct {
	source string
	scope  string
	keyID  sql.NullInt64
}

func readModelRow(t *testing.T, eng *Engine, id int64) seededModelRow {
	t.Helper()
	var r seededModelRow
	require.NoError(t, eng.DB().QueryRow(
		"SELECT source, endpoint_scope, key_id FROM models WHERE id = ?", id).
		Scan(&r.source, &r.scope, &r.keyID))
	return r
}

// candidateKeys expands the live fallback chain and splits the key ids a model
// id routes onto into the catalogue candidate (empty endpoint scope) and the
// login candidate (its own scope).
func candidateKeys(t *testing.T, eng *Engine, modelID string) (catalog, login []int64) {
	t.Helper()
	chain, err := fallbackChain(eng.DB())
	require.NoError(t, err)
	expanded, err := expandChainKeys(eng.DB(), chain)
	require.NoError(t, err)
	for _, e := range expanded {
		if e.ModelID != modelID || e.KeyID == nil {
			continue
		}
		if e.EndpointScope == "" {
			catalog = append(catalog, *e.KeyID)
		} else {
			login = append(login, *e.KeyID)
		}
	}
	return catalog, login
}

// TestEnrolledLoginJoinsActiveProfileImmediately proves a newly enrolled login
// routes without waiting for the next boot's EnsureDefaultProfile pass: its
// models join the Default (auto-include) profile the moment they are seeded.
func TestEnrolledLoginJoinsActiveProfileImmediately(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	eng, err := OpenEngine(ctx, t.TempDir(), EngineOptions{SkipCatalogSeed: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })

	res, err := eng.DB().Exec(`INSERT INTO profiles(name, active, created_at) VALUES('Default', 1, 0)`)
	require.NoError(t, err)
	profileID, _ := res.LastInsertId()
	_, err = eng.DB().Exec(`INSERT INTO settings(key, value, updated_at) VALUES('active_profile_id', ?, 0)`, profileID)
	require.NoError(t, err)

	keyID, err := eng.Vault().AddLinked(ctx, "anthropic", "Prowl login")
	require.NoError(t, err)
	_, err = eng.SeedLoginModels(ctx, keyID, "anthropic", []LinkedModel{{ID: "claude-opus-5", Tools: true}})
	require.NoError(t, err)

	var inProfile int
	require.NoError(t, eng.DB().QueryRow(`
		SELECT COUNT(*) FROM profile_models pm JOIN models m ON m.id = pm.model_db_id
		 WHERE pm.profile_id = ? AND m.model_id = 'claude-opus-5' AND m.source = 'login'`, profileID).Scan(&inProfile))
	require.Equal(t, 1, inProfile, "a newly enrolled login model joins the active profile immediately")
}

// TestEnrolledLoginRespectsOperatorExclusion proves the immediate enrollment
// never overrides an operator's removal: once a login model is excluded, a later
// refresh re-seeds the catalogue row but does not resurrect it in the active
// profile.
func TestEnrolledLoginRespectsOperatorExclusion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	eng, err := OpenEngine(ctx, t.TempDir(), EngineOptions{SkipCatalogSeed: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })

	res, err := eng.DB().Exec(`INSERT INTO profiles(name, active, created_at) VALUES('Default', 1, 0)`)
	require.NoError(t, err)
	profileID, _ := res.LastInsertId()
	_, err = eng.DB().Exec(`INSERT INTO settings(key, value, updated_at) VALUES('active_profile_id', ?, 0)`, profileID)
	require.NoError(t, err)

	keyID, err := eng.Vault().AddLinked(ctx, "anthropic", "Prowl login")
	require.NoError(t, err)
	_, err = eng.SeedLoginModels(ctx, keyID, "anthropic", []LinkedModel{{ID: "claude-opus-5", Tools: true}})
	require.NoError(t, err)

	// The operator removes the login model from the active profile.
	var modelID int64
	require.NoError(t, eng.DB().QueryRow(
		`SELECT id FROM models WHERE model_id = 'claude-opus-5' AND source = 'login'`).Scan(&modelID))
	_, err = eng.DB().Exec(`DELETE FROM profile_models WHERE profile_id = ? AND model_db_id = ?`, profileID, modelID)
	require.NoError(t, err)
	_, err = eng.DB().Exec(`INSERT INTO profile_exclusions (profile_id, model_db_id) VALUES (?, ?)`, profileID, modelID)
	require.NoError(t, err)

	// A refresh re-seeds the same login model, but must not re-add the excluded row.
	_, err = eng.SeedLoginModels(ctx, keyID, "anthropic", []LinkedModel{{ID: "claude-opus-5", Tools: true}})
	require.NoError(t, err)

	var inProfile int
	require.NoError(t, eng.DB().QueryRow(
		`SELECT COUNT(*) FROM profile_models WHERE profile_id = ? AND model_db_id = ?`, profileID, modelID).Scan(&inProfile))
	require.Zero(t, inProfile, "a refresh must not re-add a login model the operator excluded")
}

// TestEnrolledLoginLeavesCuratedActiveSetAlone pins the fix for a curated set
// being flooded on every boot: when a hand-picked named set is active, seeding
// a login must add its models to the Default (auto-include) profile only, never
// to the curated set. Otherwise an account's whole catalogue reappears in the
// operator's "Quick chores" set after every restart.
func TestEnrolledLoginLeavesCuratedActiveSetAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	eng, err := OpenEngine(ctx, t.TempDir(), EngineOptions{SkipCatalogSeed: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })

	res, err := eng.DB().Exec(`INSERT INTO profiles(name, active, created_at) VALUES('Default', 0, 0)`)
	require.NoError(t, err)
	defaultID, _ := res.LastInsertId()
	res, err = eng.DB().Exec(`INSERT INTO profiles(name, active, created_at) VALUES('Quick chores', 1, 0)`)
	require.NoError(t, err)
	curatedID, _ := res.LastInsertId()
	_, err = eng.DB().Exec(`INSERT INTO settings(key, value, updated_at) VALUES('active_profile_id', ?, 0)`, curatedID)
	require.NoError(t, err)

	keyID, err := eng.Vault().AddLinked(ctx, "anthropic", "Prowl login")
	require.NoError(t, err)
	_, err = eng.SeedLoginModels(ctx, keyID, "anthropic", []LinkedModel{{ID: "claude-opus-5", Tools: true}})
	require.NoError(t, err)

	var modelID int64
	require.NoError(t, eng.DB().QueryRow(
		`SELECT id FROM models WHERE model_id = 'claude-opus-5' AND source = 'login'`).Scan(&modelID))

	var inDefault, inCurated int
	require.NoError(t, eng.DB().QueryRow(
		`SELECT COUNT(*) FROM profile_models WHERE profile_id = ? AND model_db_id = ?`, defaultID, modelID).Scan(&inDefault))
	require.Equal(t, 1, inDefault, "a login model joins the Default auto-include profile")
	require.NoError(t, eng.DB().QueryRow(
		`SELECT COUNT(*) FROM profile_models WHERE profile_id = ? AND model_db_id = ?`, curatedID, modelID).Scan(&inCurated))
	require.Zero(t, inCurated, "a login model must not flood the curated active set")
}

// retiringLogin is a managed credential source whose model list and
// authoritativeness are controllable, for exercising authoritative retirement.
type retiringLogin struct {
	models        []LinkedModel
	authoritative bool
}

func (r *retiringLogin) Credential(context.Context, string) (string, bool, error) {
	return "live-token", true, nil
}

func (r *retiringLogin) Linkable(context.Context) []LinkableProvider {
	return []LinkableProvider{{ID: "anthropic", Name: "Claude", PoolManaged: true, PoolEnabled: true}}
}

func (r *retiringLogin) Models(context.Context, string) []LinkedModel { return r.models }

func (r *retiringLogin) ModelsAuthoritative(context.Context, string) ([]LinkedModel, bool) {
	return r.models, r.authoritative
}

// loginModelIDs lists a login's ACTIVE model ids - the routing candidates. A
// retired model is marked unavailable (not deleted, not disabled), so it drops
// out here while its operator-preference records survive.
func loginModelIDs(t *testing.T, eng *Engine) []string {
	t.Helper()
	rows, err := eng.DB().Query("SELECT model_id FROM models WHERE source = 'login' AND enabled = 1 AND available = 1 ORDER BY model_id")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		out = append(out, id)
	}
	require.NoError(t, rows.Err())
	return out
}

// TestAuthoritativeDiscoveryRetiresStaleLoginModels proves a model the provider
// stops serving is dropped as a routing candidate once an AUTHORITATIVE
// re-discovery omits it.
func TestAuthoritativeDiscoveryRetiresStaleLoginModels(t *testing.T) {
	t.Parallel()
	eng := linkedTestEngine(t)
	src := &retiringLogin{
		models:        []LinkedModel{{ID: "claude-opus-5", Tools: true}, {ID: "claude-legacy", Tools: true}},
		authoritative: true,
	}
	eng.SetCredentialSource(src)
	require.Equal(t, []string{"claude-legacy", "claude-opus-5"}, loginModelIDs(t, eng))

	// The provider retires claude-legacy; an authoritative re-discovery drops it.
	src.models = []LinkedModel{{ID: "claude-opus-5", Tools: true}}
	eng.ReconcileLoginModels(context.Background())
	require.Equal(t, []string{"claude-opus-5"}, loginModelIDs(t, eng),
		"authoritative discovery retires a model the provider no longer serves")
}

// TestFallbackDiscoveryKeepsStaleLoginModels proves the retirement is gated on
// authoritativeness: a fallback or failed discovery that returns a shorter list
// must never delete a model the provider might still serve.
func TestFallbackDiscoveryKeepsStaleLoginModels(t *testing.T) {
	t.Parallel()
	eng := linkedTestEngine(t)
	src := &retiringLogin{
		models:        []LinkedModel{{ID: "claude-opus-5", Tools: true}, {ID: "claude-legacy", Tools: true}},
		authoritative: true,
	}
	eng.SetCredentialSource(src)
	require.Len(t, loginModelIDs(t, eng), 2)

	// A non-authoritative (fallback/failed) re-discovery returns a reduced list.
	src.models = []LinkedModel{{ID: "claude-opus-5", Tools: true}}
	src.authoritative = false
	eng.ReconcileLoginModels(context.Background())
	require.Equal(t, []string{"claude-legacy", "claude-opus-5"}, loginModelIDs(t, eng),
		"a fallback discovery must never retire a model")
}

// TestAuthoritativeEmptyDiscoveryRetiresAllLoginModels proves an AUTHORITATIVE
// empty result is a meaningful statement - the provider now serves nothing - and
// retires every stale row, rather than leaving them routable forever. The safety
// against wiping on a transient outage lives in the source returning
// authoritative=false, never in a blanket empty guard here.
func TestAuthoritativeEmptyDiscoveryRetiresAllLoginModels(t *testing.T) {
	t.Parallel()
	eng := linkedTestEngine(t)
	src := &retiringLogin{
		models:        []LinkedModel{{ID: "claude-opus-5", Tools: true}, {ID: "claude-legacy", Tools: true}},
		authoritative: true,
	}
	eng.SetCredentialSource(src)
	require.Len(t, loginModelIDs(t, eng), 2)

	// The provider authoritatively serves nothing now: every row is retired.
	src.models = nil
	eng.ReconcileLoginModels(context.Background())
	require.Empty(t, loginModelIDs(t, eng),
		"an authoritative empty discovery retires every stale row")

	// A non-authoritative empty result (fallback/failure), by contrast, changes
	// nothing: it cannot re-add and must never retire.
	src.models = []LinkedModel{{ID: "claude-opus-5", Tools: true}}
	src.authoritative = true
	eng.ReconcileLoginModels(context.Background())
	require.Equal(t, []string{"claude-opus-5"}, loginModelIDs(t, eng))
	src.models = nil
	src.authoritative = false
	eng.ReconcileLoginModels(context.Background())
	require.Equal(t, []string{"claude-opus-5"}, loginModelIDs(t, eng),
		"a non-authoritative empty result must never retire")
}

// TestAuthoritativeDiscoveryRetiresLegacyUnscopedRows proves an install that
// enrolled a login before login models carried their own endpoint scope does
// not list every model twice forever: the legacy empty-scope rows for the same
// key are retired on the next authoritative discovery, while the scoped rows
// - and the operator's flags on them - are untouched.
func TestAuthoritativeDiscoveryRetiresLegacyUnscopedRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	eng, err := OpenEngine(ctx, t.TempDir(), EngineOptions{SkipCatalogSeed: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })

	keyID, err := eng.Vault().AddLinked(ctx, "anthropic", "Prowl login")
	require.NoError(t, err)
	// The legacy row: same platform/model/key, but stored under the
	// catalogue's empty scope by an older release.
	_, err = eng.DB().Exec(`
		INSERT INTO models(platform, model_id, display_name, enabled, key_id, source, endpoint_scope, available)
		VALUES('anthropic', 'claude-opus-5', 'Claude Opus 5', 0, ?, 'login', '', 1)`, keyID)
	require.NoError(t, err)

	_, err = eng.SeedLoginModels(ctx, keyID, "anthropic", []LinkedModel{{ID: "claude-opus-5", Tools: true}})
	require.NoError(t, err)
	var before int
	require.NoError(t, eng.DB().QueryRow(
		`SELECT COUNT(*) FROM models WHERE model_id = 'claude-opus-5' AND available = 1`).Scan(&before))
	require.Equal(t, 2, before, "seeding under the scoped identity leaves the legacy row beside it")

	_, err = eng.retireAbsentLoginModels(ctx, keyID, []LinkedModel{{ID: "claude-opus-5"}})
	require.NoError(t, err)
	rows, err := eng.DB().Query(
		`SELECT endpoint_scope, available FROM models WHERE model_id = 'claude-opus-5' ORDER BY endpoint_scope`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	got := map[string]int{}
	for rows.Next() {
		var scope string
		var available int
		require.NoError(t, rows.Scan(&scope, &available))
		got[scope] = available
	}
	require.NoError(t, rows.Err())
	require.Equal(t, map[string]int{"": 0, "link:anthropic": 1}, got,
		"the legacy unscoped row is retired; the scoped row stays available")
}

// TestRetiredLoginModelReturnsWithExclusionPreserved proves an authoritative
// temporary retirement never destroys the operator's default-profile exclusion:
// after the operator removes a login model, a retire-then-relist cycle brings it
// back available but still excluded, reusing the original row rather than a fresh
// one that would resurrect the model in the profile.
func TestRetiredLoginModelReturnsWithExclusionPreserved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	eng, err := OpenEngine(ctx, t.TempDir(), EngineOptions{SkipCatalogSeed: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })

	res, err := eng.DB().Exec(`INSERT INTO profiles(name, active, created_at) VALUES('Default', 1, 0)`)
	require.NoError(t, err)
	profileID, _ := res.LastInsertId()
	_, err = eng.DB().Exec(`INSERT INTO settings(key, value, updated_at) VALUES('active_profile_id', ?, 0)`, profileID)
	require.NoError(t, err)

	keyID, err := eng.Vault().AddLinked(ctx, "anthropic", "Prowl login")
	require.NoError(t, err)
	models := []LinkedModel{{ID: "claude-opus-5", Tools: true}}
	_, err = eng.SeedLoginModels(ctx, keyID, "anthropic", models)
	require.NoError(t, err)

	// The operator removes the login model from the active profile.
	var modelID int64
	require.NoError(t, eng.DB().QueryRow(
		`SELECT id FROM models WHERE model_id = 'claude-opus-5' AND source = 'login'`).Scan(&modelID))
	_, err = eng.DB().Exec(`DELETE FROM profile_models WHERE profile_id = ? AND model_db_id = ?`, profileID, modelID)
	require.NoError(t, err)
	_, err = eng.DB().Exec(`INSERT INTO profile_exclusions (profile_id, model_db_id) VALUES (?, ?)`, profileID, modelID)
	require.NoError(t, err)

	// The provider temporarily stops serving it: an authoritative discovery
	// omits it and retires the row by marking it unavailable - never by clearing
	// the operator's own enabled flag.
	_, err = eng.retireAbsentLoginModels(ctx, keyID, nil)
	require.NoError(t, err)
	var retiredEnabled, retiredAvailable int
	require.NoError(t, eng.DB().QueryRow(
		`SELECT enabled, available FROM models WHERE id = ?`, modelID).Scan(&retiredEnabled, &retiredAvailable))
	require.Equal(t, 1, retiredEnabled, "retirement must not touch the operator enabled flag")
	require.Zero(t, retiredAvailable, "a retired login model is marked unavailable, not deleted")

	// The provider serves it again.
	_, err = eng.SeedLoginModels(ctx, keyID, "anthropic", models)
	require.NoError(t, err)

	var rowCount, nowAvailable int
	require.NoError(t, eng.DB().QueryRow(
		`SELECT COUNT(*), COALESCE(MAX(available), 0) FROM models WHERE id = ? AND source = 'login'`, modelID).
		Scan(&rowCount, &nowAvailable))
	require.Equal(t, 1, rowCount, "the relisted model reuses its original row, not a fresh id")
	require.Equal(t, 1, nowAvailable, "the returning login model is available again")

	var inProfile, excluded int
	require.NoError(t, eng.DB().QueryRow(
		`SELECT COUNT(*) FROM profile_models WHERE profile_id = ? AND model_db_id = ?`, profileID, modelID).Scan(&inProfile))
	require.Zero(t, inProfile, "a retire/relist cycle must not resurrect an excluded login model in the profile")
	require.NoError(t, eng.DB().QueryRow(
		`SELECT COUNT(*) FROM profile_exclusions WHERE profile_id = ? AND model_db_id = ?`, profileID, modelID).Scan(&excluded))
	require.Equal(t, 1, excluded, "the operator's exclusion survives the retirement")
}

// TestRetiredLoginModelReturnsWithChainDisablePreserved proves the same for the
// global fallback chain: a login model the operator disabled in the chain comes
// back disabled after a retire/relist cycle, never silently re-enabled.
func TestRetiredLoginModelReturnsWithChainDisablePreserved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	eng, err := OpenEngine(ctx, t.TempDir(), EngineOptions{SkipCatalogSeed: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })

	keyID, err := eng.Vault().AddLinked(ctx, "anthropic", "Prowl login")
	require.NoError(t, err)
	models := []LinkedModel{{ID: "claude-opus-5", Tools: true}}
	_, err = eng.SeedLoginModels(ctx, keyID, "anthropic", models)
	require.NoError(t, err)

	var modelID int64
	require.NoError(t, eng.DB().QueryRow(
		`SELECT id FROM models WHERE model_id = 'claude-opus-5' AND source = 'login'`).Scan(&modelID))
	// The operator disables the model in the global fallback chain.
	_, err = eng.DB().Exec(`UPDATE fallback_config SET enabled = 0 WHERE model_db_id = ?`, modelID)
	require.NoError(t, err)

	_, err = eng.retireAbsentLoginModels(ctx, keyID, nil)
	require.NoError(t, err)
	_, err = eng.SeedLoginModels(ctx, keyID, "anthropic", models)
	require.NoError(t, err)

	var modelEnabled, modelAvailable, chainEnabled int
	require.NoError(t, eng.DB().QueryRow(
		`SELECT enabled, available FROM models WHERE id = ?`, modelID).Scan(&modelEnabled, &modelAvailable))
	require.Equal(t, 1, modelEnabled, "the operator never disabled the model itself")
	require.Equal(t, 1, modelAvailable, "the returning login model is available again")
	require.NoError(t, eng.DB().QueryRow(
		`SELECT enabled FROM fallback_config WHERE model_db_id = ?`, modelID).Scan(&chainEnabled))
	require.Zero(t, chainEnabled, "the operator's chain disable survives the retire/relist cycle")
}

// TestRoutineReconcileDoesNotReenableOperatorDisabledModel proves reconciliation
// never resurrects a model the operator disabled: SeedLoginModels runs on every
// reconcile, and it restores provider availability without ever touching the
// operator's own enabled flag.
func TestRoutineReconcileDoesNotReenableOperatorDisabledModel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	eng, err := OpenEngine(ctx, t.TempDir(), EngineOptions{SkipCatalogSeed: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })

	keyID, err := eng.Vault().AddLinked(ctx, "anthropic", "Prowl login")
	require.NoError(t, err)
	models := []LinkedModel{{ID: "claude-opus-5", Tools: true}}
	_, err = eng.SeedLoginModels(ctx, keyID, "anthropic", models)
	require.NoError(t, err)

	var modelID int64
	require.NoError(t, eng.DB().QueryRow(
		`SELECT id FROM models WHERE model_id = 'claude-opus-5' AND source = 'login'`).Scan(&modelID))
	// The operator disables the model itself (models.enabled), the provider still
	// offers it.
	_, err = eng.DB().Exec(`UPDATE models SET enabled = 0 WHERE id = ?`, modelID)
	require.NoError(t, err)

	// A routine reconcile re-seeds the still-offered model.
	_, err = eng.SeedLoginModels(ctx, keyID, "anthropic", models)
	require.NoError(t, err)

	var enabled, available int
	require.NoError(t, eng.DB().QueryRow(
		`SELECT enabled, available FROM models WHERE id = ?`, modelID).Scan(&enabled, &available))
	require.Zero(t, enabled, "reconciliation must never re-enable a model the operator disabled")
	require.Equal(t, 1, available, "the provider still offers it, so it stays available")
}

// TestParseLoginModelListRequiresPresentDataArray proves an authoritative empty
// result is distinguished from an unexpected shape: a present `data` array (even
// empty) is authoritative, while an absent or wrong-typed `data` field is not,
// so a stray error payload never retires the login's models.
func TestParseLoginModelListRequiresPresentDataArray(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		body       string
		wantParsed bool
		wantLen    int
	}{
		{"present non-empty", `{"data":[{"id":"gpt-x"}]}`, true, 1},
		{"present empty", `{"data":[]}`, true, 0},
		{"present empty with whitespace", "{\"data\": \n\t []}", true, 0},
		{"absent data", `{"object":"list"}`, false, 0},
		{"null data", `{"data":null}`, false, 0},
		{"wrong-typed object", `{"data":{"id":"gpt-x"}}`, false, 0},
		{"wrong-typed string", `{"data":"gpt-x"}`, false, 0},
		{"error payload", `{"error":{"message":"unauthorized"}}`, false, 0},
		{"not json", `not json`, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			models, parsed := parseLoginModelList([]byte(tc.body))
			require.Equal(t, tc.wantParsed, parsed, "authoritativeness for %s", tc.name)
			require.Len(t, models, tc.wantLen)
		})
	}
}
