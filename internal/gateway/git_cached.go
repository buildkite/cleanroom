package gateway

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/charmbracelet/log"
)

type gitHostHandlerProvider interface {
	GitHandlerForHost(host string) (http.Handler, error)
}

// cachedGitHandler wraps a content-cache git handler with cleanroom's
// identity-based policy enforcement. The content-cache handler handles
// upstream proxying, caching, and singleflight deduplication; this wrapper
// handles sandbox scope validation and host allowlisting.
type cachedGitHandler struct {
	cache    gitHostHandlerProvider
	fallback http.Handler
	logger   *log.Logger
}

func newCachedGitHandler(cache gitHostHandlerProvider, fallback http.Handler, logger *log.Logger) *cachedGitHandler {
	return &cachedGitHandler{cache: cache, fallback: fallback, logger: logger}
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

	if !gitRequestUsesDotGit(repoPath) {
		if h.fallback == nil {
			writeReasonError(w, http.StatusBadGateway, reasonUpstreamError, "git cache fallback is not configured for non-.git remotes")
			return
		}
		h.fallback.ServeHTTP(w, r)
		return
	}

	if !scope.Policy.Allows(upstreamHost, 443) {
		h.auditLog(scope.SandboxID, upstreamHost, repoPath, gatewayActionDeny, reasonHostNotAllowed)
		writeReasonError(w, http.StatusForbidden, reasonHostNotAllowed, "upstream host is not allowed by sandbox policy")
		return
	}

	// Classify the request to reject pushes before hitting the cache layer.
	if _, err := classifyGitRequest(r.Method, repoPath, r.URL.RawQuery); err != nil {
		h.auditLog(scope.SandboxID, upstreamHost, repoPath, gatewayActionDeny, reasonMethodNotAllowed)
		writeReasonError(w, http.StatusForbidden, reasonMethodNotAllowed, err.Error())
		return
	}

	cacheHandler, err := h.cache.GitHandlerForHost(upstreamHost)
	if err != nil {
		if errors.Is(err, errGitHostNotConfiguredForCaching) {
			if h.fallback == nil {
				h.auditLog(scope.SandboxID, upstreamHost, repoPath, gatewayActionDeny, reasonUpstreamError)
				writeReasonError(w, http.StatusBadGateway, reasonUpstreamError, fmt.Sprintf("git cache fallback is not configured for %s", upstreamHost))
				return
			}
			h.auditLog(scope.SandboxID, upstreamHost, repoPath, gatewayActionAllow, reasonFallback)
			h.fallback.ServeHTTP(w, r)
			return
		}
		h.auditLog(scope.SandboxID, upstreamHost, repoPath, gatewayActionDeny, reasonUpstreamError)
		writeReasonError(w, http.StatusBadGateway, reasonUpstreamError, fmt.Sprintf("git cache handler unavailable for %s: %v", upstreamHost, err))
		return
	}
	h.auditLog(scope.SandboxID, upstreamHost, repoPath, gatewayActionAllow, reasonCached)

	// content-cache's git handler expects paths like /{host}/{repo}.git/...
	// Strip the /git/ prefix so the cache handler sees the host-rooted path.
	r = r.Clone(r.Context())
	r.URL.Path = strings.TrimPrefix(r.URL.Path, "/git")
	r.URL.RawPath = ""
	cacheHandler.ServeHTTP(w, r)
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

func gitRequestUsesDotGit(repoPath string) bool {
	repositoryPath, ok := gitRepositoryPath(repoPath)
	return ok && strings.HasSuffix(repositoryPath, ".git")
}

func gitRepositoryPath(repoPath string) (string, bool) {
	switch {
	case strings.HasSuffix(repoPath, "/info/refs"):
		return strings.TrimSuffix(repoPath, "/info/refs"), true
	case strings.HasSuffix(repoPath, "/"+gitUploadPackService):
		return strings.TrimSuffix(repoPath, "/"+gitUploadPackService), true
	case strings.HasSuffix(repoPath, "/"+gitReceivePackService):
		return strings.TrimSuffix(repoPath, "/"+gitReceivePackService), true
	default:
		return "", false
	}
}

// classifyGitRequest determines the git operation and rejects disallowed
// operations. Delegates to gitHandler.classifyRequest.
func classifyGitRequest(method, repoPath, query string) (string, error) {
	return (&gitHandler{}).classifyRequest(method, repoPath, query)
}
