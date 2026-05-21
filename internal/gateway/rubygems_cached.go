package gateway

import (
	"context"
	"net/http"
	"strings"

	"charm.land/log/v2"
	"github.com/buildkite/cleanroom/internal/observability"
)

type rubyGemsHandlerProvider interface {
	RubyGemsUpstream() (string, int, string, int, error)
	RubyGemsHandler() (http.Handler, error)
}

// cachedRubyGemsHandler wraps a content-cache RubyGems handler with Cleanroom's
// identity-based policy enforcement and strips the gateway route prefix before
// delegating to the embedded content-cache handler.
type cachedRubyGemsHandler struct {
	cache  rubyGemsHandlerProvider
	logger *log.Logger
}

func newCachedRubyGemsHandler(cache rubyGemsHandlerProvider, logger *log.Logger) *cachedRubyGemsHandler {
	return &cachedRubyGemsHandler{cache: cache, logger: logger}
}

func (h *cachedRubyGemsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	scope, ok := ScopeFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonMethodNotAllowed)
		writeReasonError(w, http.StatusMethodNotAllowed, reasonMethodNotAllowed, "only GET and HEAD are permitted for rubygems")
		return
	}

	policyHost, policyPort, upstreamHost, _, err := h.cache.RubyGemsUpstream()
	if err != nil {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonUpstreamError)
		h.auditLog(r.Context(), scope.SandboxID, "rubygems", gatewayActionDeny, reasonRubyGemsUnavailable)
		writeReasonError(w, http.StatusBadGateway, reasonUpstreamError, "rubygems cache is not configured")
		return
	}
	if !scope.Policy.Allows(policyHost, policyPort) {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonHostNotAllowed)
		h.auditLog(r.Context(), scope.SandboxID, policyHost, gatewayActionDeny, reasonHostNotAllowed)
		writeReasonError(w, http.StatusForbidden, reasonHostNotAllowed, "upstream rubygems host is not allowed by sandbox policy")
		return
	}

	handler, err := h.cache.RubyGemsHandler()
	if err != nil {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonUpstreamError)
		h.auditLog(r.Context(), scope.SandboxID, "rubygems", gatewayActionDeny, reasonRubyGemsUnavailable)
		writeReasonError(w, http.StatusBadGateway, reasonUpstreamError, "rubygems cache is not configured")
		return
	}

	setGatewayRequestDecision(r.Context(), gatewayActionAllow, reasonCached)
	h.auditLog(r.Context(), scope.SandboxID, upstreamHost, gatewayActionAllow, reasonCached)

	r = r.Clone(r.Context())
	r.URL.Path = strings.TrimPrefix(r.URL.Path, "/rubygems")
	r.URL.RawPath = ""
	if r.URL.Path == "" {
		r.URL.Path = "/"
	}
	handler.ServeHTTP(w, r)
}

func (h *cachedRubyGemsHandler) auditLog(ctx context.Context, sandboxID, target, action, reason string) {
	logger := observability.WithTraceContext(h.logger, ctx)
	if logger == nil {
		return
	}
	logger.Info("gateway rubygems request",
		observability.LogFieldSandboxID, sandboxID,
		"service", "rubygems",
		"target", target,
		"action", action,
		observability.LogFieldReasonCode, reason,
	)
}
