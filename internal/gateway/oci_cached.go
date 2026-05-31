package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"charm.land/log/v2"
	"github.com/buildkite/cleanroom/internal/gatewayauth"
	"github.com/buildkite/cleanroom/internal/observability"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type registryPrefixHandlerProvider interface {
	OCIUpstreamForPrefix(prefix string) (string, int, string, int, error)
	OCIHandlerForPrefix(prefix, cacheScope string) (http.Handler, func(), error)
}

// cachedRegistryHandler wraps a content-cache OCI handler with cleanroom's
// identity-based policy enforcement. It rewrites the gateway's /registry/
// path prefix to the /v2/ prefix that the OCI Distribution handler expects,
// and extracts the upstream registry host from the configured prefix mapping
// for policy evaluation.
type cachedRegistryHandler struct {
	cache        registryPrefixHandlerProvider
	logger       *log.Logger
	requireOwner bool
}

func newCachedRegistryHandler(cache registryPrefixHandlerProvider, logger *log.Logger, requireOwner bool) *cachedRegistryHandler {
	return &cachedRegistryHandler{
		cache:        cache,
		logger:       logger,
		requireOwner: requireOwner,
	}
}

func (h *cachedRegistryHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	scope, ok := ScopeFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	span := trace.SpanFromContext(r.Context())

	// Only GET and HEAD are valid OCI Distribution operations for pulls.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonMethodNotAllowed)
		span.SetAttributes(
			attribute.String(observability.AttrGatewayAction, gatewayActionDeny),
			attribute.String(observability.AttrReasonCode, reasonMethodNotAllowed),
		)
		span.SetStatus(codes.Error, "only GET and HEAD are permitted for registry")
		writeReasonError(w, http.StatusMethodNotAllowed, reasonMethodNotAllowed, "only GET and HEAD are permitted for registry")
		return
	}

	// Rewrite /registry/ → /v2/ for the content-cache OCI handler.
	remainder := strings.TrimPrefix(r.URL.Path, "/registry/")

	// Extract the prefix (first path segment) to look up the upstream host.
	prefix, _, hasSep := strings.Cut(remainder, "/")
	if prefix == "" {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonInvalidRequest)
		writeReasonError(w, http.StatusBadRequest, reasonInvalidRequest, "missing registry prefix")
		return
	}
	if !hasSep {
		// Bare prefix with no trailing path — not a valid OCI request.
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonInvalidRequest)
		writeReasonError(w, http.StatusBadRequest, reasonInvalidRequest, "missing image path")
		return
	}
	rest := strings.TrimPrefix(remainder, prefix+"/")

	normalizedPrefix := strings.ToLower(strings.TrimSpace(prefix))
	policyHost, policyPort, upstreamHost, upstreamPort, err := h.cache.OCIUpstreamForPrefix(normalizedPrefix)
	if err != nil {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonUnknownRegistryPrefix)
		span.RecordError(err)
		span.SetAttributes(
			attribute.String(observability.AttrGatewayAction, gatewayActionDeny),
			attribute.String(observability.AttrReasonCode, reasonUnknownRegistryPrefix),
		)
		span.SetStatus(codes.Error, err.Error())
		h.auditLog(r.Context(), scope.SandboxID, normalizedPrefix, gatewayActionDeny, reasonUnknownRegistryPrefix)
		writeReasonError(w, http.StatusNotFound, reasonUnknownRegistryPrefix, fmt.Sprintf("unknown registry prefix %q", prefix))
		return
	}
	span.SetAttributes(
		attribute.String(observability.AttrGatewayTargetHost, policyHost),
		attribute.String(observability.AttrGatewayRegistryPrefix, normalizedPrefix),
	)
	if !scope.Policy.Allows(policyHost, policyPort) {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonHostNotAllowed)
		span.SetAttributes(
			attribute.String(observability.AttrGatewayAction, gatewayActionDeny),
			attribute.String(observability.AttrReasonCode, reasonHostNotAllowed),
		)
		span.SetStatus(codes.Error, "upstream registry host is not allowed by sandbox policy")
		h.auditLog(r.Context(), scope.SandboxID, policyHost, gatewayActionDeny, reasonHostNotAllowed)
		writeReasonError(w, http.StatusForbidden, reasonHostNotAllowed, "upstream registry host is not allowed by sandbox policy")
		return
	}
	if err := authorizeOCIGatewayRequest(scope, normalizedPrefix, rest, h.requireOwner); err != nil {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonGatewayAuthDenied)
		span.RecordError(err)
		span.SetAttributes(
			attribute.String(observability.AttrGatewayAction, gatewayActionDeny),
			attribute.String(observability.AttrReasonCode, reasonGatewayAuthDenied),
		)
		span.SetStatus(codes.Error, err.Error())
		h.auditLog(r.Context(), scope.SandboxID, normalizedPrefix, gatewayActionDeny, reasonGatewayAuthDenied)
		writeReasonError(w, http.StatusForbidden, reasonGatewayAuthDenied, err.Error())
		return
	}
	span.SetAttributes(
		attribute.String(observability.AttrGatewayAction, gatewayActionAllow),
		attribute.String(observability.AttrReasonCode, reasonCached),
		attribute.Int(observability.AttrGatewayUpstreamPort, upstreamPort),
		attribute.String(observability.AttrGatewayUpstreamHost, upstreamHost),
	)
	setGatewayRequestDecision(r.Context(), gatewayActionAllow, reasonCached)
	h.auditLog(r.Context(), scope.SandboxID, upstreamHost, gatewayActionAllow, reasonCached)

	cacheScope := ociCacheScope(scope, normalizedPrefix, rest)
	cacheHandler, releaseHandler, err := h.cache.OCIHandlerForPrefix(normalizedPrefix, cacheScope)
	if err != nil {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonUnknownRegistryPrefix)
		span.RecordError(err)
		span.SetAttributes(
			attribute.String(observability.AttrGatewayAction, gatewayActionDeny),
			attribute.String(observability.AttrReasonCode, reasonUnknownRegistryPrefix),
		)
		span.SetStatus(codes.Error, err.Error())
		h.auditLog(r.Context(), scope.SandboxID, normalizedPrefix, gatewayActionDeny, reasonUnknownRegistryPrefix)
		writeReasonError(w, http.StatusNotFound, reasonUnknownRegistryPrefix, fmt.Sprintf("unknown registry prefix %q", prefix))
		return
	}
	defer releaseHandler()

	r = r.Clone(withOCIUpstreamPolicy(r.Context(), policyHost, policyPort, upstreamHost, upstreamPort))
	r.URL.Path = rewriteOCICachePath(normalizedPrefix, rest)
	r.URL.RawPath = ""
	cacheHandler.ServeHTTP(w, r)
}

func rewriteOCICachePath(prefix, rest string) string {
	switch rest {
	case "v2", "v2/":
		return "/v2/"
	}
	rest = strings.TrimPrefix(rest, "v2/")
	return "/v2/" + prefix + "/" + rest
}

func ociCacheScope(scope *SandboxScope, prefix, rest string) string {
	repoKey, ok, err := gatewayauth.OCIRepoKeyFromPath(prefix, rest)
	if err != nil || !ok {
		repoKey = gatewayauth.NormalizeOCIRepoPrefix(prefix)
	}
	ownerKey := "owner:none"
	if scope != nil && scope.GatewayScope.HasOwner() {
		ownerKey = "owner:" + strings.TrimSpace(scope.GatewayScope.Owner.PrincipalID) + "\x00scope:" + strings.TrimSpace(scope.GatewayScope.Owner.Scope)
	}
	return ownerKey + "\x00oci:" + repoKey
}

func (h *cachedRegistryHandler) auditLog(ctx context.Context, sandboxID, target, action, reason string) {
	logger := observability.WithTraceContext(h.logger, ctx)
	if logger == nil {
		return
	}
	logger.Info("gateway registry request",
		observability.LogFieldSandboxID, sandboxID,
		"service", "registry",
		"target", target,
		"action", action,
		observability.LogFieldReasonCode, reason,
	)
}
