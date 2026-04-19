package gateway

import (
	"net/http"
	"strings"

	"github.com/charmbracelet/log"
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
		writeReasonError(w, http.StatusMethodNotAllowed, reasonMethodNotAllowed, "only GET and HEAD are permitted for rubygems")
		return
	}

	policyHost, policyPort, upstreamHost, _, err := h.cache.RubyGemsUpstream()
	if err != nil {
		h.auditLog(scope.SandboxID, "rubygems", "deny", "rubygems_unavailable")
		writeReasonError(w, http.StatusBadGateway, reasonUpstreamError, "rubygems cache is not configured")
		return
	}
	if !scope.Policy.Allows(policyHost, policyPort) {
		h.auditLog(scope.SandboxID, policyHost, "deny", reasonHostNotAllowed)
		writeReasonError(w, http.StatusForbidden, reasonHostNotAllowed, "upstream rubygems host is not allowed by sandbox policy")
		return
	}

	handler, err := h.cache.RubyGemsHandler()
	if err != nil {
		h.auditLog(scope.SandboxID, "rubygems", "deny", "rubygems_unavailable")
		writeReasonError(w, http.StatusBadGateway, reasonUpstreamError, "rubygems cache is not configured")
		return
	}

	h.auditLog(scope.SandboxID, upstreamHost, "allow", "cached")

	r = r.Clone(r.Context())
	r.URL.Path = strings.TrimPrefix(r.URL.Path, "/rubygems")
	r.URL.RawPath = ""
	if r.URL.Path == "" {
		r.URL.Path = "/"
	}
	handler.ServeHTTP(w, r)
}

func (h *cachedRubyGemsHandler) auditLog(sandboxID, target, action, reason string) {
	if h.logger == nil {
		return
	}
	h.logger.Info("gateway rubygems request",
		"sandbox_id", sandboxID,
		"service", "rubygems",
		"target", target,
		"action", action,
		"reason_code", reason,
	)
}
