package workbench

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/prowl-agent/prowl-agent/internal/setup"
)

const (
	MaxSetupRequestBytes  = 4096
	MaxSetupResponseBytes = 16384
	maxSetupIntegrations  = 16
	maxSetupStringBytes   = 128
)

// serveSetupRoute provides the setup route subtree for API composition. It does
// not establish authentication; NewAPI's boundary remains responsible for that.
func serveSetupRoute(service *setup.Service) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if service == nil {
			writeError(response, request, http.StatusServiceUnavailable, "service_unavailable", "setup service is unavailable", unavailableVersion)
			return
		}
		switch request.URL.Path {
		case "/api/v1/setup/detect":
			serveSetupDetect(response, request, service)
		case "/api/v1/setup/plan":
			serveSetupPlan(response, request, service)
		case "/api/v1/setup/apply":
			serveSetupApply(response, request, service)
		case "/api/v1/setup/verify":
			serveSetupVerify(response, request, service)
		default:
			writeError(response, request, http.StatusNotFound, "not_found", "API route was not found", unavailableVersion)
		}
	})
}

func serveSetupDetect(response http.ResponseWriter, request *http.Request, service *setup.Service) {
	if request.Method != http.MethodGet {
		methodNotAllowed(response, request, http.MethodGet)
		return
	}
	result, err := service.Detect(request.Context())
	if err != nil {
		writeSetupError(response, request, err)
		return
	}
	writeSuccess(response, request, result.ProjectConfigVersion, result, MaxSetupResponseBytes)
}

type setupPlanRequest struct { Integrations []string `json:"integrations"` }
type setupVerifyRequest struct { Integrations []string `json:"integrations"` }

func serveSetupPlan(response http.ResponseWriter, request *http.Request, service *setup.Service) {
	if request.Method != http.MethodPost {
		methodNotAllowed(response, request, http.MethodPost)
		return
	}
	var input setupPlanRequest
	if !decodeSetupRequest(response, request, &input) || !validIntegrations(input.Integrations) {
		writeError(response, request, http.StatusBadRequest, "invalid_request", "setup request is invalid", unavailableVersion)
		return
	}
	plan, err := service.Plan(request.Context(), input.Integrations)
	if err != nil {
		writeSetupError(response, request, err)
		return
	}
	writeSuccess(response, request, plan.ProjectConfigVersion, plan, MaxSetupResponseBytes)
}

func serveSetupApply(response http.ResponseWriter, request *http.Request, service *setup.Service) {
	if request.Method != http.MethodPost {
		methodNotAllowed(response, request, http.MethodPost)
		return
	}
	var input setup.ApplyRequest
	if !decodeSetupRequest(response, request, &input) || !validApplyRequest(input) {
		writeError(response, request, http.StatusBadRequest, "invalid_request", "setup request is invalid", unavailableVersion)
		return
	}
	outcome, err := service.Apply(request.Context(), input)
	if err != nil {
		writeSetupError(response, request, err)
		return
	}
	writeSuccess(response, request, outcome.ProjectConfigVersion, outcome, MaxSetupResponseBytes)
}

func serveSetupVerify(response http.ResponseWriter, request *http.Request, service *setup.Service) {
	if request.Method != http.MethodPost {
		methodNotAllowed(response, request, http.MethodPost)
		return
	}
	var input setupVerifyRequest
	if !decodeSetupRequest(response, request, &input) || !validIntegrations(input.Integrations) {
		writeError(response, request, http.StatusBadRequest, "invalid_request", "setup request is invalid", unavailableVersion)
		return
	}
	plan, err := service.Plan(request.Context(), input.Integrations)
	if err == nil {
		err = service.Verify(request.Context(), plan)
	}
	if err != nil {
		writeSetupError(response, request, err)
		return
	}
	writeSuccess(response, request, plan.ProjectConfigVersion, struct { Verified bool `json:"verified"` }{Verified: true}, MaxSetupResponseBytes)
}

func decodeSetupRequest(response http.ResponseWriter, request *http.Request, destination any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(response, request.Body, MaxSetupRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return false
	}
	return decoder.Decode(&struct{}{}) == io.EOF
}

func validIntegrations(values []string) bool {
	if len(values) > maxSetupIntegrations {
		return false
	}
	for _, value := range values {
		if len(value) == 0 || len(value) > maxSetupStringBytes || strings.ContainsAny(value, "\x00\r\n") {
			return false
		}
	}
	return true
}

func validApplyRequest(request setup.ApplyRequest) bool {
	return validIntegrations(request.Integrations) &&
		validSetupString(request.PlanHash) &&
		validSetupString(request.ExpectedProjectConfigVersion) &&
		validSetupString(request.IdempotencyKey)
}

func validSetupString(value string) bool {
	return len(value) > 0 && len(value) <= maxSetupStringBytes && !strings.ContainsAny(value, "\x00\r\n")
}

func methodNotAllowed(response http.ResponseWriter, request *http.Request, method string) {
	response.Header().Set("Allow", method)
	writeError(response, request, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed", unavailableVersion)
}

func writeSetupError(response http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, setup.ErrApprovalRequired):
		writeError(response, request, http.StatusForbidden, "approval_required", "setup approval is required", unavailableVersion)
	case errors.Is(err, setup.ErrPlanConflict):
		writeError(response, request, http.StatusConflict, "setup_conflict", "setup plan conflicts with current project state", unavailableVersion)
	default:
		writeError(response, request, http.StatusUnprocessableEntity, "setup_unavailable", "setup operation is unavailable", unavailableVersion)
	}
}
