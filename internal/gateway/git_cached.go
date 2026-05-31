package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"charm.land/log/v2"
	"github.com/buildkite/cleanroom/internal/observability"
)

type gitHostHandlerProvider interface {
	GitHandlerForHost(host, cacheScope string) (http.Handler, func(), error)
}

// cachedGitHandler wraps a content-cache git handler with cleanroom's
// identity-based policy enforcement. The content-cache handler handles
// upstream proxying, caching, and singleflight deduplication; this wrapper
// handles sandbox scope validation and host allowlisting.
type cachedGitHandler struct {
	cache        gitHostHandlerProvider
	fallback     http.Handler
	direct       http.Handler
	logger       *log.Logger
	requireOwner bool
	credentials  CredentialProvider
}

func newCachedGitHandler(cache gitHostHandlerProvider, fallback http.Handler, logger *log.Logger, requireOwner bool, credentials CredentialProvider) *cachedGitHandler {
	return newCachedGitHandlerWithDirectFallback(cache, fallback, fallback, logger, requireOwner, credentials)
}

func newCachedGitHandlerWithDirectFallback(cache gitHostHandlerProvider, fallback, direct http.Handler, logger *log.Logger, requireOwner bool, credentials CredentialProvider) *cachedGitHandler {
	return &cachedGitHandler{cache: cache, fallback: fallback, direct: direct, logger: logger, requireOwner: requireOwner, credentials: credentials}
}

func (h *cachedGitHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	scope, ok := ScopeFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	upstreamHost, repoPath, err := splitGitRequestURL(r.URL)
	if err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	if err := authorizeGitGatewayRequest(scope, upstreamHost, repoPath, h.requireOwner); err != nil {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonGatewayAuthDenied)
		h.auditLog(r.Context(), scope.SandboxID, upstreamHost, repoPath, gatewayActionDeny, reasonGatewayAuthDenied)
		writeReasonError(w, http.StatusForbidden, reasonGatewayAuthDenied, err.Error())
		return
	}

	if !gitRequestUsesDotGit(repoPath) {
		h.serveFallback(w, r, scope, upstreamHost, repoPath, "git cache fallback is not configured for non-.git remotes")
		return
	}

	if !scope.Policy.Allows(upstreamHost, 443) {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonHostNotAllowed)
		h.auditLog(r.Context(), scope.SandboxID, upstreamHost, repoPath, gatewayActionDeny, reasonHostNotAllowed)
		writeReasonError(w, http.StatusForbidden, reasonHostNotAllowed, "upstream host is not allowed by sandbox policy")
		return
	}

	// Classify the request to reject pushes before hitting the cache layer.
	requestType, err := classifyGitRequest(r.Method, repoPath, r.URL.RawQuery)
	if err != nil {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonMethodNotAllowed)
		h.auditLog(r.Context(), scope.SandboxID, upstreamHost, repoPath, gatewayActionDeny, reasonMethodNotAllowed)
		writeReasonError(w, http.StatusForbidden, reasonMethodNotAllowed, err.Error())
		return
	}

	cacheScope, cacheable, err := h.cacheScopeKey(r.Context(), scope, upstreamHost, repoPath, requestType)
	if err != nil {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonUpstreamError)
		h.auditLog(r.Context(), scope.SandboxID, upstreamHost, repoPath, gatewayActionDeny, reasonUpstreamError)
		writeReasonError(w, http.StatusBadGateway, reasonUpstreamError, fmt.Sprintf("git cache authorization unavailable for %s%s: %v", upstreamHost, repoPath, err))
		return
	}
	if !cacheable {
		h.serveDirect(w, r, scope, upstreamHost, repoPath, "direct git fallback is not configured for upload-pack requests without Basic upstream credentials")
		return
	}

	cacheHandler, release, err := h.cache.GitHandlerForHost(upstreamHost, cacheScope)
	if err != nil {
		if errors.Is(err, errGitHostNotConfiguredForCaching) {
			h.serveFallback(w, r, scope, upstreamHost, repoPath, fmt.Sprintf("git cache fallback is not configured for %s", upstreamHost))
			return
		}
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonUpstreamError)
		h.auditLog(r.Context(), scope.SandboxID, upstreamHost, repoPath, gatewayActionDeny, reasonUpstreamError)
		writeReasonError(w, http.StatusBadGateway, reasonUpstreamError, fmt.Sprintf("git cache handler unavailable for %s: %v", upstreamHost, err))
		return
	}
	if release != nil {
		defer release()
	}
	setGatewayRequestDecision(r.Context(), gatewayActionAllow, reasonCached)
	h.auditLog(r.Context(), scope.SandboxID, upstreamHost, repoPath, gatewayActionAllow, reasonCached)

	// content-cache's git handler expects paths like /{host}/{repo}.git/...
	// Rebuild the path from the normalized route host so the cache layer sees
	// the same authority that policy and owner checks already approved.
	r = r.Clone(r.Context())
	r.URL.Path = "/" + upstreamHost + repoPath
	r.URL.RawPath = ""
	cacheHandler.ServeHTTP(w, r)
}

func (h *cachedGitHandler) serveFallback(w http.ResponseWriter, r *http.Request, scope *SandboxScope, upstreamHost, repoPath, unavailableMessage string) {
	if h.fallback == nil {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonUpstreamError)
		h.auditLog(r.Context(), scope.SandboxID, upstreamHost, repoPath, gatewayActionDeny, reasonUpstreamError)
		writeReasonError(w, http.StatusBadGateway, reasonUpstreamError, unavailableMessage)
		return
	}
	setGatewayRequestDecision(r.Context(), gatewayActionAllow, reasonFallback)
	h.auditLog(r.Context(), scope.SandboxID, upstreamHost, repoPath, gatewayActionAllow, reasonFallback)
	h.fallback.ServeHTTP(w, r)
}

func (h *cachedGitHandler) serveDirect(w http.ResponseWriter, r *http.Request, scope *SandboxScope, upstreamHost, repoPath, unavailableMessage string) {
	if h.direct == nil {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonUpstreamError)
		h.auditLog(r.Context(), scope.SandboxID, upstreamHost, repoPath, gatewayActionDeny, reasonUpstreamError)
		writeReasonError(w, http.StatusBadGateway, reasonUpstreamError, unavailableMessage)
		return
	}
	setGatewayRequestDecision(r.Context(), gatewayActionAllow, reasonProxied)
	h.auditLog(r.Context(), scope.SandboxID, upstreamHost, repoPath, gatewayActionAllow, reasonProxied)
	h.direct.ServeHTTP(w, r)
}

func (h *cachedGitHandler) cacheScopeKey(ctx context.Context, scope *SandboxScope, upstreamHost, repoPath, requestType string) (string, bool, error) {
	credentialScope := "credential:anonymous"
	if h.credentials != nil {
		remoteURL, err := canonicalUpstreamRemoteURL(upstreamHost, repoPath)
		if err != nil {
			return "", false, err
		}
		header, err := h.credentials.Resolve(ctx, remoteURL)
		if err != nil {
			return "", false, fmt.Errorf("resolve upstream credentials: %w", err)
		}
		header = strings.TrimSpace(header)
		if header != "" {
			_, _, basic, err := parseBasicAuthHeader(header)
			if err != nil {
				return "", false, err
			}
			if requestType == "upload-pack" && !basic {
				return "", false, nil
			}
			credentialScope = "credential:" + digestString(header)
		}
	}

	policyScope := "policy:nil"
	if scope != nil && scope.Policy != nil {
		policyScope = "policy:" + compiledPolicyCacheKey(scope.Policy)
	}

	ownerScope := "ownerless"
	if scope != nil && scope.GatewayScope.HasOwner() {
		prefixes := append([]string(nil), scope.GatewayScope.Authorization.GitRepoPrefixes...)
		sort.Strings(prefixes)
		ownerScope = strings.Join([]string{
			"owner",
			scope.GatewayScope.Owner.PrincipalID,
			scope.GatewayScope.Owner.Scope,
			strings.Join(prefixes, "\x00"),
		}, "\x00")
	}

	return digestString(strings.Join([]string{
		"git-cache-v2",
		strings.ToLower(strings.TrimSpace(upstreamHost)),
		policyScope,
		ownerScope,
		credentialScope,
	}, "\x00")), true, nil
}

func digestString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (h *cachedGitHandler) auditLog(ctx context.Context, sandboxID, upstreamHost, repoPath, action, reason string) {
	logger := observability.WithTraceContext(h.logger, ctx)
	if logger == nil {
		return
	}
	logger.Info("gateway git request",
		observability.LogFieldSandboxID, sandboxID,
		"service", "git",
		"upstream_host", upstreamHost,
		"repo_path", repoPath,
		"action", action,
		observability.LogFieldReasonCode, reason,
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
