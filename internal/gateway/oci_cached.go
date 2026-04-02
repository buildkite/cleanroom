package gateway

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/charmbracelet/log"
)

// cachedRegistryHandler wraps a content-cache OCI handler with cleanroom's
// identity-based policy enforcement. It rewrites the gateway's /registry/
// path prefix to the /v2/ prefix that the OCI Distribution handler expects,
// and extracts the upstream registry host from the configured prefix mapping
// for policy evaluation.
type cachedRegistryHandler struct {
	cache http.Handler
	// prefixHosts maps OCI router prefixes to upstream registry hostnames
	// for policy evaluation. Example: {"docker-hub": "registry-1.docker.io"}
	prefixHosts map[string]string
	logger      *log.Logger
}

func newCachedRegistryHandler(cache http.Handler, prefixHosts map[string]string, logger *log.Logger) *cachedRegistryHandler {
	return &cachedRegistryHandler{
		cache:       cache,
		prefixHosts: prefixHosts,
		logger:      logger,
	}
}

func (h *cachedRegistryHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	scope, ok := ScopeFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Only GET and HEAD are valid OCI Distribution operations for pulls.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeReasonError(w, http.StatusMethodNotAllowed, reasonMethodNotAllowed, "only GET and HEAD are permitted for registry")
		return
	}

	// Rewrite /registry/ → /v2/ for the content-cache OCI handler.
	remainder := strings.TrimPrefix(r.URL.Path, "/registry/")

	// Extract the prefix (first path segment) to look up the upstream host.
	prefix, _, hasSep := strings.Cut(remainder, "/")
	if prefix == "" {
		http.Error(w, "bad request: missing registry prefix", http.StatusBadRequest)
		return
	}
	if !hasSep {
		// Bare prefix with no trailing path — not a valid OCI request.
		http.Error(w, "bad request: missing image path", http.StatusBadRequest)
		return
	}

	// Policy check: resolve the upstream host from the prefix mapping.
	{
		upstreamHost := h.resolveHost(prefix)
		if upstreamHost == "" {
			h.auditLog(scope.SandboxID, prefix, "deny", "unknown_registry_prefix")
			writeReasonError(w, http.StatusNotFound, "unknown_registry_prefix", fmt.Sprintf("unknown registry prefix %q", prefix))
			return
		}
		if !scope.Policy.Allows(upstreamHost, 443) {
			h.auditLog(scope.SandboxID, upstreamHost, "deny", reasonHostNotAllowed)
			writeReasonError(w, http.StatusForbidden, reasonHostNotAllowed, "upstream registry host is not allowed by sandbox policy")
			return
		}
		h.auditLog(scope.SandboxID, upstreamHost, "allow", "cached")
	}

	r = r.Clone(r.Context())
	r.URL.Path = "/v2/" + remainder
	r.URL.RawPath = ""
	h.cache.ServeHTTP(w, r)
}

func (h *cachedRegistryHandler) resolveHost(prefix string) string {
	if h.prefixHosts == nil {
		return ""
	}
	return h.prefixHosts[prefix]
}

func (h *cachedRegistryHandler) auditLog(sandboxID, target, action, reason string) {
	if h.logger == nil {
		return
	}
	h.logger.Info("gateway registry request",
		"sandbox_id", sandboxID,
		"service", "registry",
		"target", target,
		"action", action,
		"reason_code", reason,
	)
}
