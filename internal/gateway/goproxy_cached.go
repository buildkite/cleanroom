package gateway

import (
	"context"
	"net/http"
	"strings"

	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/charmbracelet/log"
)

type goProxyHandlerProvider interface {
	GoProxyUpstream() (string, int, string, int, error)
	GoProxyHandler() (http.Handler, error)
	SumDBUpstream() (string, int, string, int, error)
	SumDBHandler() (http.Handler, error)
}

// cachedGoProxyHandler wraps content-cache's goproxy and sumdb handlers with
// Cleanroom's identity-based policy enforcement.
type cachedGoProxyHandler struct {
	cache  goProxyHandlerProvider
	logger *log.Logger
}

func newCachedGoProxyHandler(cache goProxyHandlerProvider, logger *log.Logger) *cachedGoProxyHandler {
	return &cachedGoProxyHandler{cache: cache, logger: logger}
}

func (h *cachedGoProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	scope, ok := ScopeFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonMethodNotAllowed)
		writeReasonError(w, http.StatusMethodNotAllowed, reasonMethodNotAllowed, "only GET and HEAD are permitted for goproxy")
		return
	}

	service := "goproxy"
	trimPrefix := RouteGoProxy[:len(RouteGoProxy)-1]
	policyHost, policyPort, upstreamHost, _, err := h.cache.GoProxyUpstream()
	if strings.HasPrefix(r.URL.Path, "/goproxy/sumdb/") {
		service = "sumdb"
		policyHost, policyPort, upstreamHost, _, err = h.cache.SumDBUpstream()
	}
	if err != nil {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonUpstreamError)
		h.auditLog(r.Context(), scope.SandboxID, service, service, gatewayActionDeny, reasonUpstreamError)
		writeReasonError(w, http.StatusBadGateway, reasonUpstreamError, service+" cache is not configured")
		return
	}

	if !scope.Policy.Allows(policyHost, policyPort) {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonHostNotAllowed)
		h.auditLog(r.Context(), scope.SandboxID, service, policyHost, gatewayActionDeny, reasonHostNotAllowed)
		writeReasonError(w, http.StatusForbidden, reasonHostNotAllowed, "upstream "+service+" host is not allowed by sandbox policy")
		return
	}

	var handler http.Handler
	if service == "sumdb" {
		handler, err = h.cache.SumDBHandler()
	} else {
		handler, err = h.cache.GoProxyHandler()
	}
	if err != nil {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonUpstreamError)
		h.auditLog(r.Context(), scope.SandboxID, service, service, gatewayActionDeny, reasonUpstreamError)
		writeReasonError(w, http.StatusBadGateway, reasonUpstreamError, service+" cache is not configured")
		return
	}

	setGatewayRequestDecision(r.Context(), gatewayActionAllow, reasonCached)
	h.auditLog(r.Context(), scope.SandboxID, service, upstreamHost, gatewayActionAllow, reasonCached)

	r = r.Clone(r.Context())
	r.URL.Path = strings.TrimPrefix(r.URL.Path, trimPrefix)
	r.URL.RawPath = ""
	if r.URL.Path == "" {
		r.URL.Path = "/"
	}
	handler.ServeHTTP(w, r)
}

func (h *cachedGoProxyHandler) auditLog(ctx context.Context, sandboxID, service, target, action, reason string) {
	logger := observability.WithTraceContext(h.logger, ctx)
	if logger == nil {
		return
	}
	logger.Info("gateway "+service+" request",
		observability.LogFieldSandboxID, sandboxID,
		"service", service,
		"target", target,
		"action", action,
		observability.LogFieldReasonCode, reason,
	)
}
