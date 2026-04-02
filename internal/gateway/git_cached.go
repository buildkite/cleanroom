package gateway

import (
	"net/http"
	"strings"

	"github.com/charmbracelet/log"
)

// cachedGitHandler wraps a content-cache git handler with cleanroom's
// identity-based policy enforcement. The content-cache handler handles
// upstream proxying, caching, and singleflight deduplication; this wrapper
// handles sandbox scope validation and host allowlisting.
type cachedGitHandler struct {
	cache  http.Handler
	logger *log.Logger
}

func newCachedGitHandler(cache http.Handler, logger *log.Logger) *cachedGitHandler {
	return &cachedGitHandler{cache: cache, logger: logger}
}

func (h *cachedGitHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	scope, ok := ScopeFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	upstreamHost, repoPath, err := splitGitRequestPath(r.URL.Path)
	if err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	if !scope.Policy.Allows(upstreamHost, 443) {
		h.auditLog(scope.SandboxID, upstreamHost, repoPath, "deny", reasonHostNotAllowed)
		writeReasonError(w, http.StatusForbidden, reasonHostNotAllowed, "upstream host is not allowed by sandbox policy")
		return
	}

	// Classify the request to reject pushes before hitting the cache layer.
	if _, err := classifyGitRequest(r.Method, repoPath, r.URL.RawQuery); err != nil {
		h.auditLog(scope.SandboxID, upstreamHost, repoPath, "deny", reasonMethodNotAllowed)
		writeReasonError(w, http.StatusForbidden, reasonMethodNotAllowed, err.Error())
		return
	}

	h.auditLog(scope.SandboxID, upstreamHost, repoPath, "allow", "cached")

	// content-cache's git handler expects paths like /{host}/{repo}.git/...
	// Strip the /git/ prefix so the cache handler sees the host-rooted path.
	r = r.Clone(r.Context())
	r.URL.Path = strings.TrimPrefix(r.URL.Path, "/git")
	r.URL.RawPath = ""
	h.cache.ServeHTTP(w, r)
}

func (h *cachedGitHandler) auditLog(sandboxID, upstreamHost, repoPath, action, reason string) {
	if h.logger == nil {
		return
	}
	h.logger.Info("gateway git request",
		"sandbox_id", sandboxID,
		"service", "git",
		"upstream_host", upstreamHost,
		"repo_path", repoPath,
		"action", action,
		"reason_code", reason,
	)
}

// classifyGitRequest determines the git operation and rejects disallowed
// operations. Delegates to gitHandler.classifyRequest.
func classifyGitRequest(method, repoPath, query string) (string, error) {
	return (&gitHandler{}).classifyRequest(method, repoPath, query)
}
