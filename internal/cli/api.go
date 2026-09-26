package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/neur0map/prowl/internal/application"
	"github.com/neur0map/prowl/internal/config"
	"github.com/neur0map/prowl/internal/doctor"
	"github.com/neur0map/prowl/internal/gateway"
	"github.com/neur0map/prowl/internal/gateway/catalog"
	"github.com/neur0map/prowl/internal/query"
)

// DefaultAPIPort is where `prowl api serve` listens on loopback. It sits one
// off the gateway's port so the two local services never collide.
const DefaultAPIPort = 8787

// newAPICmd serves the read-only code-intelligence answers over a local HTTP
// API, so a program (the Ryoku Rashin dashboard, for example) consumes the
// same results the CLI prints without spawning a process per question. The
// project store is opened once per repo and kept current by a lazy, TTL-gated
// refresh, which is what turns "index answers" into a service instead of a
// cold-start script. Structural answers work with no AI, no daemon, and no
// network; the surface binds loopback only and answers state-changing
// requests with a refusal, because every verb here is a read.
func newAPICmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "api",
		Short: "Serve the read-only code-intelligence answers over a local HTTP API",
		Long: `Answer indexed-project questions over HTTP instead of one process per question:
GET /health, /api/repos, /api/status, /api/overview, /api/find, /api/def,
/api/outline, /api/references, /api/impact, /api/search, /api/peek, /api/doctor,
/api/history, and /api/providers. Every response is the same JSON the CLI's
--format json prints. Loopback only; the machine-local gateway token
authenticates everything except /health.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			port, _ := cmd.Flags().GetInt("port")
			repos, _ := cmd.Flags().GetStringSlice("repo")
			ttl, _ := cmd.Flags().GetDuration("refresh")
			if len(repos) == 0 {
				wd, err := os.Getwd()
				if err != nil {
					return err
				}
				repos = []string{wd}
			}
			token, err := gateway.EnsureToken(gateway.Dir())
			if err != nil {
				return fmt.Errorf("api: machine token: %w", err)
			}
			srv, err := newAPIServer(cmd.Context(), repos, token, ttl)
			if err != nil {
				return err
			}
			defer srv.close()
			ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err != nil {
				return fmt.Errorf("api: cannot bind 127.0.0.1:%d: %w", port, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "api up on http://127.0.0.1:%d (repos: %d)\n", port, len(repos))
			httpSrv := &http.Server{Handler: srv, ReadHeaderTimeout: 5 * time.Second}
			errCh := make(chan error, 1)
			go func() { errCh <- httpSrv.Serve(ln) }()
			select {
			case <-cmd.Context().Done():
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_ = httpSrv.Shutdown(shutdownCtx)
				return nil
			case err := <-errCh:
				if errors.Is(err, http.ErrServerClosed) {
					return nil
				}
				return err
			}
		},
	}
	cmd.Flags().Int("port", DefaultAPIPort, "port to listen on (loopback only)")
	cmd.Flags().StringSlice("repo", nil, "indexed project root to serve (repeatable; default: current directory)")
	cmd.Flags().Duration("refresh", 20*time.Second, "minimum age before an answered request triggers an incremental re-index")
	return cmd
}

// apiServer holds one open project per repo and the shared token.
type apiServer struct {
	token string
	repos []*apiRepo
}

type apiRepo struct {
	mu      sync.Mutex
	path    string
	name    string
	project *application.Project
	last    time.Time
	ttl     time.Duration
}

// refreshDue reports whether the index has aged past the TTL and a request
// should pay for an incremental re-index first. The first request always
// refreshes: the index may have drifted while nothing was serving it.
func (rp *apiRepo) refreshDue(now time.Time) bool {
	return rp.last.IsZero() || now.Sub(rp.last) >= rp.ttl
}

func newAPIServer(ctx context.Context, repos []string, token string, ttl time.Duration) (*apiServer, error) {
	if ttl <= 0 {
		ttl = 20 * time.Second
	}
	s := &apiServer{token: token}
	for _, r := range repos {
		abs, err := filepath.Abs(r)
		if err != nil {
			s.close()
			return nil, err
		}
		if _, err := os.Stat(abs); err != nil {
			s.close()
			return nil, fmt.Errorf("api: repo %s: %w", abs, err)
		}
		project, err := application.OpenProject(ctx, abs, application.Options{EnableAI: true, InferencerProvider: maybeInferencer})
		if err != nil {
			s.close()
			return nil, fmt.Errorf("api: open %s: %w (is it indexed? run `prowl init` there)", abs, err)
		}
		s.repos = append(s.repos, &apiRepo{path: abs, name: filepath.Base(abs), project: project, ttl: ttl})
	}
	return s, nil
}

func (s *apiServer) close() {
	for _, r := range s.repos {
		_ = r.project.Close()
	}
}

// repo resolves ?repo= against name or path; the first repo answers when the
// query is empty and more than one project is not being addressed by name.
func (s *apiServer) repo(w http.ResponseWriter, r *http.Request) (*apiRepo, bool) {
	want := r.URL.Query().Get("repo")
	if want == "" && len(s.repos) == 1 {
		return s.repos[0], true
	}
	for _, rp := range s.repos {
		if want == rp.name || want == rp.path || filepath.Base(want) == rp.name {
			return rp, true
		}
	}
	writeAPIErr(w, http.StatusNotFound, "unknown repo "+strconv.Quote(want)+"; /api/repos lists the served roots")
	return nil, false
}

// fresh returns the querier for a repo, refreshing the index when it has aged
// past the TTL. One refresh at a time per repo; readers queue behind it.
func (rp *apiRepo) fresh(ctx context.Context) (*query.Querier, error) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	if rp.refreshDue(time.Now()) {
		if _, err := rp.project.Refresh(ctx); err != nil {
			return nil, err
		}
		rp.last = time.Now()
	}
	return rp.project.Query, nil
}

func (s *apiServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/health" {
		writeAPIJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": time.Now().UTC().Format(time.RFC3339)})
		return
	}
	if r.Method != http.MethodGet {
		writeAPIErr(w, http.StatusMethodNotAllowed, "the api surface is read-only")
		return
	}
	if !s.authorized(r) {
		writeAPIErr(w, http.StatusUnauthorized, "missing or wrong bearer token")
		return
	}
	switch r.URL.Path {
	case "/api/repos":
		s.handleRepos(w)
	case "/api/providers":
		s.handleProviders(w)
	case "/api/status":
		s.handleRepo(w, r, func(q *query.Querier, rp *apiRepo) (any, error) { return q.Status() })
	case "/api/overview":
		s.handleRepo(w, r, func(q *query.Querier, rp *apiRepo) (any, error) { return q.Overview() })
	case "/api/find":
		s.handleRepo(w, r, func(q *query.Querier, rp *apiRepo) (any, error) {
			t, err := arg(r, "q")
			if err != nil {
				return nil, err
			}
			return q.FindSymbol(t)
		})
	case "/api/def":
		s.handleRepo(w, r, func(q *query.Querier, rp *apiRepo) (any, error) {
			t, err := arg(r, "q")
			if err != nil {
				return nil, err
			}
			return q.Definition(rp.path, t)
		})
	case "/api/outline":
		s.handleRepo(w, r, func(q *query.Querier, rp *apiRepo) (any, error) {
			t, err := arg(r, "path")
			if err != nil {
				return nil, err
			}
			return q.Outline(t)
		})
	case "/api/references":
		s.handleRepo(w, r, func(q *query.Querier, rp *apiRepo) (any, error) {
			t, err := arg(r, "q")
			if err != nil {
				return nil, err
			}
			return q.References(t)
		})
	case "/api/impact":
		s.handleRepo(w, r, func(q *query.Querier, rp *apiRepo) (any, error) {
			t, err := arg(r, "path")
			if err != nil {
				return nil, err
			}
			return q.BlastSummarize(t)
		})
	case "/api/search":
		s.handleRepo(w, r, func(q *query.Querier, rp *apiRepo) (any, error) {
			t, err := arg(r, "q")
			if err != nil {
				return nil, err
			}
			if r.URL.Query().Get("smart") == "1" {
				return q.SmartSearch(r.Context(), t)
			}
			return q.SimilarCode(r.Context(), t)
		})
	case "/api/peek":
		s.handleRepo(w, r, func(q *query.Querier, rp *apiRepo) (any, error) {
			p, err := arg(r, "path")
			if err != nil {
				return nil, err
			}
			start, end := 1, 0
			if v := r.URL.Query().Get("start"); v != "" {
				n, err := strconv.Atoi(v)
				if err != nil || n < 1 {
					return nil, &apiError{http.StatusBadRequest, "start must be a positive line number"}
				}
				start = n
			}
			if v := r.URL.Query().Get("end"); v != "" {
				n, err := strconv.Atoi(v)
				if err != nil || n < start {
					return nil, &apiError{http.StatusBadRequest, "end must be a line number >= start"}
				}
				end = n
			}
			return query.PeekLines(rp.path, p, start, end)
		})
	case "/api/history":
		s.handleRepo(w, r, func(q *query.Querier, rp *apiRepo) (any, error) {
			t, err := arg(r, "q")
			if err != nil {
				return nil, err
			}
			limit := 10
			if v := r.URL.Query().Get("limit"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					limit = min(n, 50)
				}
			}
			return q.History(r.Context(), rp.path, t, limit)
		})
	case "/api/doctor":
		s.handleRepo(w, r, func(q *query.Querier, rp *apiRepo) (any, error) {
			return rp.doctor(r.Context())
		})
	default:
		writeAPIErr(w, http.StatusNotFound, "no such endpoint: "+r.URL.Path)
	}
}

func (s *apiServer) authorized(r *http.Request) bool {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return got != "" && got == s.token
}

// handleRepo runs one querier-backed endpoint against the addressed repo,
// refreshing its index first when it has aged out.
func (s *apiServer) handleRepo(w http.ResponseWriter, r *http.Request, run func(*query.Querier, *apiRepo) (any, error)) {
	rp, ok := s.repo(w, r)
	if !ok {
		return
	}
	q, err := rp.fresh(r.Context())
	if err != nil {
		writeAPIErr(w, http.StatusServiceUnavailable, "index refresh: "+err.Error())
		return
	}
	out, err := run(q, rp)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) {
			writeAPIErr(w, ae.status, ae.msg)
		} else {
			writeAPIErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeAPIJSON(w, http.StatusOK, out)
}

func (rp *apiRepo) doctor(ctx context.Context) (doctor.Report, error) {
	release, err := rp.project.ReadGuard(ctx)
	if err != nil {
		return doctor.Report{}, err
	}
	defer release()
	if err := rp.project.Store.RequirePublishedGeneration(); err != nil {
		return doctor.Report{}, err
	}
	rules, err := config.LoadRules(rp.project.Workspace.Path)
	if err != nil {
		return doctor.Report{}, err
	}
	return doctor.Run(rp.project.Store, rules, doctor.Options{Root: rp.path})
}

func (s *apiServer) handleRepos(w http.ResponseWriter) {
	type row struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	out := make([]row, 0, len(s.repos))
	for _, r := range s.repos {
		out = append(out, row{Name: r.name, Path: r.path})
	}
	writeAPIJSON(w, http.StatusOK, out)
}

// handleProviders answers the consolidated provider directory (free tiers,
// credits, paid, subscriptions) the gateway routes and the TUI console list.
// It is static shipped data, so it is the one /api/* route that does not touch
// a project index.
func (s *apiServer) handleProviders(w http.ResponseWriter) {
	entries, err := catalog.Directory()
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "provider directory: "+err.Error())
		return
	}
	writeAPIJSON(w, http.StatusOK, entries)
}

// apiError is an endpoint failure with its own HTTP status; handlers return it
// instead of writing the response so exactly one body is ever written.
type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func arg(r *http.Request, key string) (string, error) {
	v := strings.TrimSpace(r.URL.Query().Get(key))
	if v == "" {
		return "", &apiError{http.StatusBadRequest, key + " is required"}
	}
	return v, nil
}

func writeAPIJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeAPIErr(w http.ResponseWriter, status int, msg string) {
	writeAPIJSON(w, status, map[string]any{"error": msg})
}
