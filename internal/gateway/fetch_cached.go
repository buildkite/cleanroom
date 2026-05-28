package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"charm.land/log/v2"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/policy"
)

type fetchHandlerProvider interface {
	FetchAllowsHost(host string) bool
	FetchHandlerForPolicy(compiled *policy.CompiledPolicy) (http.Handler, func(), error)
}

// cachedFetchHandler wraps content-cache's immutable artifact fetch handler
// with Cleanroom's identity-based policy enforcement.
type cachedFetchHandler struct {
	cache  fetchHandlerProvider
	logger *log.Logger
}

func newCachedFetchHandler(cache fetchHandlerProvider, logger *log.Logger) *cachedFetchHandler {
	return &cachedFetchHandler{cache: cache, logger: logger}
}

func (h *cachedFetchHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	scope, ok := ScopeFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonMethodNotAllowed)
		writeReasonError(w, http.StatusMethodNotAllowed, reasonMethodNotAllowed, "only GET and HEAD are permitted for fetch")
		return
	}

	host, _, err := parseFetchRequestPath(r.URL.Path)
	if err != nil {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonInvalidRequest)
		writeReasonError(w, http.StatusBadRequest, reasonInvalidRequest, err.Error())
		return
	}
	if !scope.Policy.Allows(host, 443) {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonHostNotAllowed)
		h.auditLog(r.Context(), scope.SandboxID, host, gatewayActionDeny, reasonHostNotAllowed)
		writeReasonError(w, http.StatusForbidden, reasonHostNotAllowed, "upstream fetch host is not allowed by sandbox policy")
		return
	}
	if !h.cache.FetchAllowsHost(host) {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonHostNotAllowed)
		h.auditLog(r.Context(), scope.SandboxID, host, gatewayActionDeny, reasonHostNotAllowed)
		writeReasonError(w, http.StatusForbidden, reasonHostNotAllowed, "upstream fetch host is not configured")
		return
	}

	handler, releaseHandler, err := h.cache.FetchHandlerForPolicy(scope.Policy)
	if err != nil {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonUpstreamError)
		h.auditLog(r.Context(), scope.SandboxID, "fetch", gatewayActionDeny, reasonUpstreamError)
		writeReasonError(w, http.StatusBadGateway, reasonUpstreamError, "fetch cache is not configured")
		return
	}
	defer releaseHandler()

	setGatewayRequestDecision(r.Context(), gatewayActionAllow, reasonCached)
	h.auditLog(r.Context(), scope.SandboxID, host, gatewayActionAllow, reasonCached)

	r = r.Clone(r.Context())
	r.URL.Path = strings.TrimPrefix(r.URL.Path, "/fetch")
	r.URL.RawPath = ""
	if r.URL.Path == "" {
		r.URL.Path = "/"
	}
	handler.ServeHTTP(w, r)
}

func parseFetchRequestPath(requestPath string) (string, string, error) {
	trimmed := strings.TrimPrefix(requestPath, "/fetch/")
	host, remainder, ok := strings.Cut(trimmed, "/")
	if !ok || host == "" || remainder == "" {
		return "", "", fmt.Errorf("invalid fetch path")
	}
	return strings.ToLower(strings.TrimSpace(host)), "/" + remainder, nil
}

func (h *cachedFetchHandler) auditLog(ctx context.Context, sandboxID, target, action, reason string) {
	logger := observability.WithTraceContext(h.logger, ctx)
	if logger == nil {
		return
	}
	logger.Info("gateway fetch request",
		observability.LogFieldSandboxID, sandboxID,
		"service", "fetch",
		"target", target,
		"action", action,
		observability.LogFieldReasonCode, reason,
	)
}
