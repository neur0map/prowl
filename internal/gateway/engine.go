package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/neur0map/prowl/internal/gateway/catalog"
	"github.com/neur0map/prowl/internal/gateway/provider"
	"github.com/neur0map/prowl/internal/gateway/store"
)

// Engine assembles the ported routing subsystems into one object with a
// lifetime, so the HTTP layer holds a single dependency instead of six.
//
// The parts are deliberately separate types with narrow seams - scoring knows
// nothing about quota, the failover loop knows nothing about SQL - and this is
// the only place that knows how they fit together. That is what keeps the
// wiring reviewable: a change to one subsystem shows up here or nowhere.
type Engine struct {
	store    *store.Store
	vault    *KeyVault
	registry *provider.Registry

	ledger    *Ledger
	penalties *PenaltyStore
	cooldowns *CooldownEngine
	sticky    *StickyStore
	degraded  *DegradationMonitor
	failover  *Failover

	// credentials resolves providers the pool borrows from Prowl's own
	// logins; nil when nothing is wired.
	credentials CredentialSource

	// cooldownWriter persists benches off the router's lock; nil when the
	// engine opened without persistence.
	cooldownWriter *sqlCooldowns

	now func() time.Time
}

// EngineOptions configures assembly. The zero value is usable.
type EngineOptions struct {
	// Strategy is the routing strategy to start with; empty uses the
	// upstream default, which is the analytics-driven balanced preset.
	Strategy RoutingStrategy

	// KeySelection chooses between the per-key bandit and least-remaining.
	KeySelection KeySelectionStrategy

	// Failover tunes depth, budget, breaker and cooldown ceiling.
	Failover FailoverConfig

	// SkipCatalogSeed opts a freshly opened engine out of seeding the shipped
	// model catalog. Production always seeds - an empty catalog is a broken
	// product - so the zero value is "seed". It exists for tests that need a
	// deliberately empty catalog to say so out loud, rather than depending on
	// a fresh database happening to hold no rows.
	SkipCatalogSeed bool
}

// OpenEngine opens the gateway's state under dir and wires the subsystems.
func OpenEngine(ctx context.Context, dir string, opts EngineOptions) (*Engine, error) {
	st, err := store.Open(ctx, dir)
	if err != nil {
		return nil, err
	}

	registry := provider.NewRegistry()
	vault, err := OpenKeyVault(ctx, st.DB(), dir, RegistryValidator{Registry: registry})
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("open key vault: %w", err)
	}

	e := &Engine{
		store:     st,
		vault:     vault,
		registry:  registry,
		ledger:    NewLedger(st.DB()),
		penalties: NewPenaltyStore(),
		cooldowns: NewCooldownEngine(),
		sticky:    NewStickyStore(),
		degraded:  NewDegradationMonitor(),
		now:       time.Now,
	}

	// The loop's dependencies are the long-lived subsystems; the chain and
	// dispatcher are per-request and supplied at Run time, because both are
	// bound to the body being routed.
	e.failover = NewFailover(FailoverDeps{
		Cooldowns:   cooldownAdapter{engine: e.cooldowns},
		Scorer:      e.penalties,
		Learner:     limitLearner{ledger: e.ledger},
		Revalidator: revalidator{vault: vault},
	}, opts.Failover)

	// A chain row whose model is gone is unroutable, and it breaks anything
	// that copies the chain: creating a routing profile fails outright on the
	// foreign key. Rows like that appear whenever models are removed by
	// something that did not have foreign keys enabled, so the engine prunes
	// them rather than trusting the cascade to have run.
	if res, err := st.DB().ExecContext(ctx,
		"DELETE FROM fallback_config WHERE model_db_id NOT IN (SELECT id FROM models)"); err == nil {
		if pruned, _ := res.RowsAffected(); pruned > 0 {
			slog.Warn("Pruned routing-chain rows whose model no longer exists",
				"rows", pruned)
		}
	}

	// Benches are restored before the first request so a restart does not
	// hand a rate-limited key straight back to the router.
	if restore, err := loadCooldowns(ctx, st.DB()); err != nil {
		slog.Warn("Could not restore cooldowns", "error", err)
	} else {
		e.cooldownWriter = newSQLCooldowns(st.DB())
		e.cooldowns.SetSink(e.cooldownWriter, restore)
		if len(restore) > 0 {
			slog.Info("Restored active cooldowns", "count", len(restore))
		}
	}

	// Seed the shipped model catalog. This is what keeps a fresh install from
	// opening to an empty product, and it refreshes catalog metadata on every
	// boot while preserving the operator's edits. A failure must be loud, not
	// swallowed: an empty catalog is the exact bug this fixes. It is not fatal
	// to engine open, though - the operator can still reach the dashboard to
	// see the error and manage keys - so it is logged rather than returned.
	if !opts.SkipCatalogSeed {
		if seeded, err := catalog.SeedModels(ctx, st.DB()); err != nil {
			slog.Error("Gateway model catalog seed failed", "error", err,
				"note", "the Models screens will be empty until this succeeds")
		} else {
			slog.Info("Gateway model catalog seeded",
				"inserted", seeded.Inserted(), "updated", seeded.Updated(),
				"chat_inserted", seeded.ChatInserted, "chat_updated", seeded.ChatUpdated,
				"embedding_inserted", seeded.EmbeddingInserted, "embedding_updated", seeded.EmbeddingUpdated,
				"media_inserted", seeded.MediaInserted, "media_updated", seeded.MediaUpdated,
				"catalog_sha", seeded.AssetSHA)
		}

		// Both chains are established after the models exist, since they are
		// built from them. fallback_config is the one the router walks and the
		// dashboard's model table renders, so an empty one shows no models
		// even with a key configured.
		if added, err := catalog.EnsureRoutingChain(ctx, st.DB()); err != nil {
			slog.Error("Gateway routing chain seed failed", "error", err,
				"note", "the Routing chain screen will be empty until this succeeds")
		} else if added > 0 {
			slog.Info("Gateway routing chain seeded", "models_added", added)
		}
		if added, err := catalog.EnsureDefaultProfile(ctx, st.DB()); err != nil {
			slog.Error("Gateway default profile seed failed", "error", err)
		} else if added > 0 {
			slog.Info("Gateway default profile updated", "models_added", added)
		}
		// Seed the offline multi-axis capability priors so the router can
		// match a task to a model's real strength without an AA key. Live AA
		// rows are never clobbered (the upsert is gated to seed rows).
		if seeded, err := SeedBenchmarks(ctx, st.DB()); err != nil {
			slog.Error("Gateway benchmark seed failed", "error", err)
		} else if seeded > 0 {
			slog.Info("Gateway benchmark priors seeded", "models", seeded)
		}
	}

	return e, nil
}

// Close releases the engine's state.
// Close stops background writers before the database, so a queued bench is
// not written to a closed store.
func (e *Engine) Close() error {
	if e.cooldownWriter != nil {
		e.cooldownWriter.Close()
	}
	return e.store.Close()
}

// DB exposes the gateway database for the HTTP layer's read queries.
func (e *Engine) DB() *sql.DB { return e.store.DB() }

// Vault is the provider credential store.
func (e *Engine) Vault() *KeyVault { return e.vault }

// Ledger is the quota and rate-window ledger.
func (e *Engine) Ledger() *Ledger { return e.ledger }

// Penalties exposes the penalty store, which the dashboard's inspector reads.
func (e *Engine) Penalties() *PenaltyStore { return e.penalties }

// Cooldowns exposes the benching engine for the dashboard's health view.
func (e *Engine) Cooldowns() *CooldownEngine { return e.cooldowns }

// Sticky exposes the conversation-affinity store.
func (e *Engine) Sticky() *StickyStore { return e.sticky }

// Degradation exposes the fleet-mode monitor. Its hysteresis only works if it
// outlives a request, which is why it belongs to the engine rather than being
// rebuilt per call.
func (e *Engine) Degradation() *DegradationMonitor { return e.degraded }

// Failover exposes the attempt loop.
func (e *Engine) Failover() *Failover { return e.failover }

// SetCooldownCeiling pushes the operator's live routing_cooldown_ceiling_ms
// into the failover driver, so a routing-config change bounds our own guessed
// benches immediately. 0 = unlimited; it never shortens a provider-stated wait.
func (e *Engine) SetCooldownCeiling(d time.Duration) { e.failover.SetCooldownCeiling(d) }

// Registry exposes the provider adapters.
func (e *Engine) Registry() *provider.Registry { return e.registry }

// StartBackground launches the engine's scheduled work and returns a stop
// function. Nothing starts itself at construction: a test that opens an engine
// must not acquire goroutines it did not ask for.
func (e *Engine) StartBackground(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	healthDone := make(chan struct{})
	benchmarkDone := make(chan struct{})

	go func() {
		defer close(healthDone)
		// Jittered so several gateway instances on one machine do not probe
		// the same providers in lockstep.
		timer := time.NewTimer(NextHealthCheckDelay(nil))
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				res, err := e.vault.CheckAllKeys(ctx, HealthPassOptions{})
				if err != nil {
					slog.Debug("Key health pass failed", "error", err)
				} else if len(res.Checked) > 0 {
					slog.Debug("Key health pass complete",
						"checked", len(res.Checked), "skipped", len(res.Skipped))
				}
				timer.Reset(NextHealthCheckDelay(nil))
			}
		}
	}()

	go func() {
		defer close(benchmarkDone)
		timer := time.NewTimer(e.nextBenchmarkRefreshDelay(ctx))
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				_, err := e.RefreshBenchmarks(ctx)
				switch {
				case errors.Is(err, ErrBenchmarkKeyMissing):
					timer.Reset(time.Hour)
				case err != nil:
					slog.Debug("Benchmark refresh failed", "error", err)
					timer.Reset(benchmarkRetryInterval)
				default:
					slog.Debug("Benchmark refresh complete", "source", benchmarkSource)
					timer.Reset(benchmarkRefreshInterval)
				}
			}
		}
	}()

	return func() {
		cancel()
		<-healthDone
		<-benchmarkDone
	}
}

func (e *Engine) nextBenchmarkRefreshDelay(ctx context.Context) time.Duration {
	status := e.BenchmarkStatus(ctx)
	if !status.Configured {
		return time.Hour
	}
	now := time.Now()
	if status.Status == "error" && status.LastAttempt != nil {
		if remaining := benchmarkRetryInterval - now.Sub(*status.LastAttempt); remaining > 0 {
			return remaining
		}
	}
	if status.LastSuccess != nil {
		if remaining := benchmarkRefreshInterval - now.Sub(*status.LastSuccess); remaining > 0 {
			return remaining
		}
	}
	return 0
}

// cooldownAdapter maps the loop's triple-keyed view onto the cooldown engine's
// string keys. The engine is keyed by string so it can also bench a pool or a
// whole platform; the loop only ever benches a (platform, model, key).
type cooldownAdapter struct {
	engine *CooldownEngine
}

func (c cooldownAdapter) Decide(platform, model string, keyID int64, req CooldownRequest) Cooldown {
	return c.engine.Decide(QuotaKey(platform, model, keyID), req)
}

func (c cooldownAdapter) Bench(platform, model string, keyID int64, d time.Duration, source CooldownSource) Cooldown {
	return c.engine.Bench(QuotaKey(platform, model, keyID), d, source)
}

func (c cooldownAdapter) Active(platform, model string, keyID int64) (Cooldown, bool) {
	return c.engine.Active(QuotaKey(platform, model, keyID))
}

func (c cooldownAdapter) SoonestExpiry() time.Time { return c.engine.SoonestExpiry() }

func (c cooldownAdapter) Succeeded(platform, model string, keyID int64, sinceKnownAt time.Time) {
	c.engine.Succeeded(QuotaKey(platform, model, keyID), sinceKnownAt)
}

// limitLearner lets a provider's own error body tighten the stored ceiling, so
// the next request is admitted against the real limit instead of a guess.
type limitLearner struct {
	ledger *Ledger
}

func (l limitLearner) LearnLimit(modelDBID int64, err error) {
	if err == nil {
		return
	}
	l.ledger.LearnLimitFromError(modelDBID, err.Error())
}

// revalidator re-probes a key immediately after a 401 so a confirmed-bad
// credential leaves routing in seconds rather than waiting for the next
// scheduled health pass.
type revalidator struct {
	vault *KeyVault
}

func (r revalidator) Revalidate(platform string, keyID int64) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// This only fires after an upstream rejected the credential, so the
		// key is marked bad on that evidence first. Probing afterwards can
		// still clear it, but only if the probe genuinely authenticates -
		// otherwise a provider with a public validation endpoint would report
		// a key as healthy moments after inference refused it.
		if err := r.vault.MarkInvalidFromRequest(ctx, keyID,
			"upstream rejected this credential during a routed request"); err != nil {
			slog.Debug("Could not demote key", "platform", platform, "key_id", keyID, "error", err)
		}
		if status, err := r.vault.CheckKey(ctx, keyID); err != nil {
			slog.Debug("Key revalidation failed", "platform", platform, "key_id", keyID, "error", err)
		} else {
			slog.Info("Key revalidated after an upstream rejection",
				"platform", platform, "key_id", keyID, "status", status)
		}
	}()
}

// ErrEngineNotConfigured means no provider key is usable yet, so there is
// nothing to route to.
var ErrEngineNotConfigured = errors.New("no provider keys configured yet - run `prowl` and add one in Credentials")
