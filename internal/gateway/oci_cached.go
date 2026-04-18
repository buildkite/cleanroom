package gateway

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/charmbracelet/log"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type registryPrefixHandlerProvider interface {
	OCIUpstreamForPrefix(prefix string) (string, int, string, int, error)
	OCIHandlerForPrefix(prefix string) (http.Handler, error)
}

// cachedRegistryHandler wraps a content-cache OCI handler with cleanroom's
// identity-based policy enforcement. It rewrites the gateway's /registry/
// path prefix to the /v2/ prefix that the OCI Distribution handler expects,
// and extracts the upstream registry host from the configured prefix mapping
// for policy evaluation.
type cachedRegistryHandler struct {
	cache  registryPrefixHandlerProvider
	logger *log.Logger
}

func newCachedRegistryHandler(cache registryPrefixHandlerProvider, logger *log.Logger) *cachedRegistryHandler {
	return &cachedRegistryHandler{
		cache:  cache,
		logger: logger,
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
		span.SetAttributes(
			attribute.String("cleanroom.gateway.action", "deny"),
			attribute.String("cleanroom.reason_code", reasonMethodNotAllowed),
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
		http.Error(w, "bad request: missing registry prefix", http.StatusBadRequest)
		return
	}
	if !hasSep {
		// Bare prefix with no trailing path — not a valid OCI request.
		http.Error(w, "bad request: missing image path", http.StatusBadRequest)
		return
	}
	rest := strings.TrimPrefix(remainder, prefix+"/")

	normalizedPrefix := strings.ToLower(strings.TrimSpace(prefix))
	policyHost, policyPort, upstreamHost, upstreamPort, err := h.cache.OCIUpstreamForPrefix(normalizedPrefix)
	if err != nil {
		span.RecordError(err)
		span.SetAttributes(
			attribute.String("cleanroom.gateway.action", "deny"),
			attribute.String("cleanroom.reason_code", "unknown_registry_prefix"),
		)
		span.SetStatus(codes.Error, err.Error())
		h.auditLog(scope.SandboxID, normalizedPrefix, "deny", "unknown_registry_prefix")
		writeReasonError(w, http.StatusNotFound, "unknown_registry_prefix", fmt.Sprintf("unknown registry prefix %q", prefix))
		return
	}
	span.SetAttributes(
		attribute.String("cleanroom.gateway.target_host", policyHost),
		attribute.String("cleanroom.gateway.registry_prefix", normalizedPrefix),
	)
	if !scope.Policy.Allows(policyHost, policyPort) {
		span.SetAttributes(
			attribute.String("cleanroom.gateway.action", "deny"),
			attribute.String("cleanroom.reason_code", reasonHostNotAllowed),
		)
		span.SetStatus(codes.Error, "upstream registry host is not allowed by sandbox policy")
		h.auditLog(scope.SandboxID, policyHost, "deny", reasonHostNotAllowed)
		writeReasonError(w, http.StatusForbidden, reasonHostNotAllowed, "upstream registry host is not allowed by sandbox policy")
		return
	}
	span.SetAttributes(
		attribute.String("cleanroom.gateway.action", "allow"),
		attribute.String("cleanroom.reason_code", "cached"),
		attribute.Int("cleanroom.gateway.upstream_port", upstreamPort),
		attribute.String("cleanroom.gateway.upstream_host", upstreamHost),
	)
	h.auditLog(scope.SandboxID, upstreamHost, "allow", "cached")

	cacheHandler, err := h.cache.OCIHandlerForPrefix(normalizedPrefix)
	if err != nil {
		span.RecordError(err)
		span.SetAttributes(
			attribute.String("cleanroom.gateway.action", "deny"),
			attribute.String("cleanroom.reason_code", "unknown_registry_prefix"),
		)
		span.SetStatus(codes.Error, err.Error())
		h.auditLog(scope.SandboxID, normalizedPrefix, "deny", "unknown_registry_prefix")
		writeReasonError(w, http.StatusNotFound, "unknown_registry_prefix", fmt.Sprintf("unknown registry prefix %q", prefix))
		return
	}

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
