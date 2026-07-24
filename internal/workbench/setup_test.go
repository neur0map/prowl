package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prowl-agent/prowl-agent/internal/setup"
)

func TestSetupDetectRouteIsBoundedAndRedacted(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(`{"mcpServers":{"private":{"command":"secret-token"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	service, err := setup.NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	serveSetupRoute(service).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/setup/detect", nil))
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), root) || strings.Contains(response.Body.String(), "secret-token") {
		t.Fatalf("unsafe detect response status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestSetupPlanRouteRejectsUnknownAndOversizedInput(t *testing.T) {
	service, err := setup.NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"integrations":["agents"],"unexpected":true}`, `{"integrations":["` + strings.Repeat("x", MaxSetupRequestBytes) + `"]}`} {
		response := httptest.NewRecorder()
		serveSetupRoute(service).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/setup/plan", strings.NewReader(body)))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("body %q status=%d want bad request", body[:min(len(body), 32)], response.Code)
		}
	}
}

func TestSetupApplyRouteRequiresReviewAndReplays(t *testing.T) {
	root := t.TempDir()
	service, err := setup.NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	handler := serveSetupRoute(service)
	plan := setupPlanFromRoute(t, handler, []string{setup.IntegrationAgents})
	denied := setup.ApplyRequest{Integrations: plan.Integrations, PlanHash: plan.Hash, ExpectedProjectConfigVersion: plan.ProjectConfigVersion, IdempotencyKey: "review-key"}
	response := postSetup(t, handler, "/api/v1/setup/apply", denied)
	if response.Code != http.StatusForbidden {
		t.Fatalf("denied apply status=%d body=%s", response.Code, response.Body.String())
	}
	denied.Approved = true
	first := postSetup(t, handler, "/api/v1/setup/apply", denied)
	if first.Code != http.StatusOK {
		t.Fatalf("approved apply status=%d body=%s", first.Code, first.Body.String())
	}
	second := postSetup(t, handler, "/api/v1/setup/apply", denied)
	if second.Code != http.StatusOK {
		t.Fatalf("replay status=%d body=%s", second.Code, second.Body.String())
	}
	var firstBody, secondBody struct { Data setup.ApplyOutcome `json:"data"` }
	if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondBody); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(firstBody.Data, secondBody.Data) {
		t.Fatalf("replay data changed: first=%+v second=%+v", firstBody.Data, secondBody.Data)
	}
}

func TestSetupVerifyRouteRejectsUnknownFields(t *testing.T) {
	service, err := setup.NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	serveSetupRoute(service).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/setup/verify", bytes.NewBufferString(`{"integrations":[],"unexpected":true}`)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("verify status=%d body=%s", response.Code, response.Body.String())
	}
}

func setupPlanFromRoute(t *testing.T, handler http.Handler, integrations []string) setup.Plan {
	t.Helper()
	response := postSetup(t, handler, "/api/v1/setup/plan", map[string]any{"integrations": integrations})
	if response.Code != http.StatusOK {
		t.Fatalf("plan status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct { Data setup.Plan `json:"data"` }
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Data
}

func postSetup(t *testing.T, handler http.Handler, path string, value any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, bytes.NewReader(data)))
	return response
}

func min(left, right int) int { if left < right { return left }; return right }
