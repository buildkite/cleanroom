package gateway

import (
	"context"
	"net/http"
	"strings"

	"charm.land/log/v2"
	"github.com/buildkite/cleanroom/internal/observability"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const dockerHubMirrorPrefix = "docker.io"

// dockerHubMirrorHandler exposes a Docker Hub-compatible pull-through mirror at
// /v2/ so guest dockerd can use the shared gateway as a registry mirror.
type dockerHubMirrorHandler struct {
	cache        registryPrefixHandlerProvider
	logger       *log.Logger
	requireOwner bool
}

func newDockerHubMirrorHandler(cache registryPrefixHandlerProvider, logger *log.Logger, requireOwner bool) *dockerHubMirrorHandler {
	return &dockerHubMirrorHandler{
		cache:        cache,
		logger:       logger,
		requireOwner: requireOwner,
	}
}

func (h *dockerHubMirrorHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	scope, ok := ScopeFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	span := trace.SpanFromContext(r.Context())

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonMethodNotAllowed)
		span.SetAttributes(
			attribute.String(observability.AttrGatewayAction, gatewayActionDeny),
			attribute.String(observability.AttrReasonCode, reasonMethodNotAllowed),
		)
		span.SetStatus(codes.Error, "only GET and HEAD are permitted for docker hub mirror")
		writeReasonError(w, http.StatusMethodNotAllowed, reasonMethodNotAllowed, "only GET and HEAD are permitted for docker hub mirror")
		return
	}

	policyHost, policyPort, upstreamHost, upstreamPort, err := h.cache.OCIUpstreamForPrefix(dockerHubMirrorPrefix)
	if err != nil {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonUnknownRegistryPrefix)
		span.RecordError(err)
		span.SetAttributes(
			attribute.String(observability.AttrGatewayAction, gatewayActionDeny),
			attribute.String(observability.AttrReasonCode, reasonUnknownRegistryPrefix),
			attribute.String(observability.AttrGatewayRegistryPrefix, dockerHubMirrorPrefix),
		)
		span.SetStatus(codes.Error, err.Error())
		h.auditLog(r.Context(), scope.SandboxID, dockerHubMirrorPrefix, gatewayActionDeny, reasonUnknownRegistryPrefix)
		writeReasonError(w, http.StatusNotFound, reasonUnknownRegistryPrefix, "docker hub mirror is not configured")
		return
	}

	span.SetAttributes(
		attribute.String(observability.AttrGatewayRegistryPrefix, dockerHubMirrorPrefix),
		attribute.String(observability.AttrGatewayTargetHost, policyHost),
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
	rest := strings.TrimPrefix(r.URL.Path, "/v2/")
	if err := authorizeOCIGatewayRequest(scope, dockerHubMirrorPrefix, rest, h.requireOwner); err != nil {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonGatewayAuthDenied)
		span.RecordError(err)
		span.SetAttributes(
			attribute.String(observability.AttrGatewayAction, gatewayActionDeny),
			attribute.String(observability.AttrReasonCode, reasonGatewayAuthDenied),
		)
		span.SetStatus(codes.Error, err.Error())
		h.auditLog(r.Context(), scope.SandboxID, dockerHubMirrorPrefix, gatewayActionDeny, reasonGatewayAuthDenied)
		writeReasonError(w, http.StatusForbidden, reasonGatewayAuthDenied, err.Error())
		return
	}

	cacheScope := ociCacheScope(scope, dockerHubMirrorPrefix, rest)
	cacheHandler, releaseHandler, err := h.cache.OCIHandlerForPrefix(dockerHubMirrorPrefix, cacheScope)
	if err != nil {
		setGatewayRequestDecision(r.Context(), gatewayActionDeny, reasonUnknownRegistryPrefix)
		span.RecordError(err)
		span.SetAttributes(
			attribute.String(observability.AttrGatewayAction, gatewayActionDeny),
			attribute.String(observability.AttrReasonCode, reasonUnknownRegistryPrefix),
			attribute.String(observability.AttrGatewayRegistryPrefix, dockerHubMirrorPrefix),
		)
		span.SetStatus(codes.Error, err.Error())
		h.auditLog(r.Context(), scope.SandboxID, dockerHubMirrorPrefix, gatewayActionDeny, reasonUnknownRegistryPrefix)
		writeReasonError(w, http.StatusNotFound, reasonUnknownRegistryPrefix, "docker hub mirror is not configured")
		return
	}
	defer releaseHandler()

	span.SetAttributes(
		attribute.String(observability.AttrGatewayAction, gatewayActionAllow),
		attribute.String(observability.AttrReasonCode, reasonCached),
		attribute.Int(observability.AttrGatewayUpstreamPort, upstreamPort),
		attribute.String(observability.AttrGatewayUpstreamHost, upstreamHost),
	)
	setGatewayRequestDecision(r.Context(), gatewayActionAllow, reasonCached)
	h.auditLog(r.Context(), scope.SandboxID, upstreamHost, gatewayActionAllow, reasonCached)

	r = r.Clone(withOCIUpstreamPolicy(r.Context(), policyHost, policyPort, upstreamHost, upstreamPort))
	r.URL.Path = rewriteDockerHubMirrorPath(r.URL.Path)
	r.URL.RawPath = ""
	cacheHandler.ServeHTTP(w, r)
}

func rewriteDockerHubMirrorPath(path string) string {
	switch path {
	case "/v2", "/v2/":
		return "/v2/"
	}
	remainder := strings.TrimPrefix(path, "/v2/")
	return "/v2/" + dockerHubMirrorPrefix + "/" + remainder
}

func (h *dockerHubMirrorHandler) auditLog(ctx context.Context, sandboxID, target, action, reason string) {
	logger := observability.WithTraceContext(h.logger, ctx)
	if logger == nil {
		return
	}
	logger.Info("gateway docker hub mirror request",
		observability.LogFieldSandboxID, sandboxID,
		"service", "registry",
		"target", target,
		"action", action,
		observability.LogFieldReasonCode, reason,
	)
}
