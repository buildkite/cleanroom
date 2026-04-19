package gateway

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/charmbracelet/log"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// DefaultPort is the default gateway listen port.
const (
	DefaultPort       = 8170
	DefaultListenAddr = ":8170"

	RouteGit      = "/git/"
	RouteRegistry = "/registry/"
	RouteRubyGems = "/rubygems/"
	RouteSecrets  = "/secrets/"
	RouteMeta     = "/meta/"
)

var serviceRoutes = []string{
	RouteGit,
	RouteRegistry,
	RouteRubyGems,
	RouteSecrets,
	RouteMeta,
}

type gatewayHostLookupFunc func(context.Context, string) ([]net.IP, error)

var defaultGatewayHostLookup gatewayHostLookupFunc = func(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// Routes returns the configured gateway service route prefixes.
func Routes() []string {
	out := make([]string, len(serviceRoutes))
	copy(out, serviceRoutes)
	return out
}

type contextKey int

const scopeContextKey contextKey = iota
const gatewayRequestContextKey contextKey = 1

type gatewayRequestObservability struct {
	action     string
	reasonCode string
}

// ScopeTokenHeader is the request header used for capability-token fallback
// identity when source-IP identity is unavailable (for example darwin NAT).
const ScopeTokenHeader = "X-Cleanroom-Scope-Token"

var defaultDarwinVZScopeTokenSourcePrefix = netip.MustParsePrefix("192.168.64.0/24")

// ScopeFromContext retrieves the SandboxScope injected by identity middleware.
func ScopeFromContext(ctx context.Context) (*SandboxScope, bool) {
	scope, ok := ctx.Value(scopeContextKey).(*SandboxScope)
	return scope, ok
}

// ScopeTokenTrustedSourcePrefixesForGatewayHost returns the trusted source
// prefixes to use for scope-token fallback requests that arrive from darwin-vz
// guests routed through a gateway host IP.
func ScopeTokenTrustedSourcePrefixesForGatewayHost(gatewayHost string) []netip.Prefix {
	return ScopeTokenSourcePolicyForGatewayHost(gatewayHost).TrustedSourcePrefixes
}

// ScopeTokenSourcePolicy describes how the gateway should validate scope-token
// requests for a given darwin-vz gateway host configuration.
type ScopeTokenSourcePolicy struct {
	TrustedSourcePrefixes        []netip.Prefix
	AllowScopeTokenFromAnySource bool
}

// ScopeTokenSourcePolicyForGatewayHost derives the source-trust policy for
// scope-token requests that arrive from darwin-vz guests routed through the
// configured gateway host.
func ScopeTokenSourcePolicyForGatewayHost(gatewayHost string) ScopeTokenSourcePolicy {
	return scopeTokenSourcePolicyForGatewayHost(context.Background(), gatewayHost, defaultGatewayHostLookup)
}

func scopeTokenSourcePolicyForGatewayHost(ctx context.Context, gatewayHost string, lookup gatewayHostLookupFunc) ScopeTokenSourcePolicy {
	defaultPrefixes := []netip.Prefix{defaultDarwinVZScopeTokenSourcePrefix}
	gatewayHost = strings.TrimSpace(gatewayHost)
	if gatewayHost == "" {
		return ScopeTokenSourcePolicy{TrustedSourcePrefixes: defaultPrefixes}
	}

	if addr, err := netip.ParseAddr(gatewayHost); err == nil {
		return ScopeTokenSourcePolicy{
			TrustedSourcePrefixes: []netip.Prefix{scopeTokenTrustedSourcePrefixForAddr(addr)},
		}
	}
	if lookup == nil {
		return ScopeTokenSourcePolicy{AllowScopeTokenFromAnySource: true}
	}

	resolved, err := lookup(ctx, gatewayHost)
	if err != nil {
		return ScopeTokenSourcePolicy{AllowScopeTokenFromAnySource: true}
	}

	prefixes := make([]netip.Prefix, 0, len(resolved))
	seen := make(map[netip.Prefix]struct{}, len(resolved))
	for _, ip := range resolved {
		addr, ok := netip.AddrFromSlice(ip)
		if !ok {
			continue
		}
		if addr.Is4In6() {
			addr = addr.Unmap()
		}
		prefix := scopeTokenTrustedSourcePrefixForAddr(addr)
		if _, exists := seen[prefix]; exists {
			continue
		}
		seen[prefix] = struct{}{}
		prefixes = append(prefixes, prefix)
	}
	if len(prefixes) == 0 {
		return ScopeTokenSourcePolicy{AllowScopeTokenFromAnySource: true}
	}
	return ScopeTokenSourcePolicy{TrustedSourcePrefixes: prefixes}
}

func scopeTokenTrustedSourcePrefixForAddr(addr netip.Addr) netip.Prefix {
	if addr.Is4() {
		return netip.PrefixFrom(addr, 24).Masked()
	}
	return netip.PrefixFrom(addr, 64).Masked()
}

// ServerConfig configures a gateway server.
type ServerConfig struct {
	ListenAddr                      string
	Registry                        *Registry
	Credentials                     CredentialProvider
	GitMirrors                      GitMirrorStore
	ContentCache                    *ContentCache
	Logger                          *log.Logger
	TracerProvider                  trace.TracerProvider
	MeterProvider                   metric.MeterProvider
	ScopeTokenTrustedSourcePrefixes []netip.Prefix
	AllowScopeTokenFromAnySource    bool
}

// Server is the host gateway HTTP server.
type Server struct {
	registry                        *Registry
	logger                          *log.Logger
	tracerProvider                  trace.TracerProvider
	meterProvider                   metric.MeterProvider
	metricsOnce                     sync.Once
	metrics                         *observability.GatewayMetrics
	metricsErr                      error
	httpServer                      *http.Server
	scopeTokenTrustedSourcePrefixes []netip.Prefix
	allowScopeTokenFromAnySource    bool

	mu      sync.Mutex
	started bool
	addr    string
}

// NewServer creates a gateway server. Call Start to begin listening.
func NewServer(cfg ServerConfig) *Server {
	addr := cfg.ListenAddr
	if addr == "" {
		addr = DefaultListenAddr
	}

	trustedSourcePrefixes := cloneScopeTokenTrustedSourcePrefixes(cfg.ScopeTokenTrustedSourcePrefixes)
	if trustedSourcePrefixes == nil && !cfg.AllowScopeTokenFromAnySource {
		trustedSourcePrefixes = ScopeTokenTrustedSourcePrefixesForGatewayHost("")
	}

	s := &Server{
		registry:                        cfg.Registry,
		logger:                          cfg.Logger,
		tracerProvider:                  cfg.TracerProvider,
		meterProvider:                   cfg.MeterProvider,
		addr:                            addr,
		scopeTokenTrustedSourcePrefixes: trustedSourcePrefixes,
		allowScopeTokenFromAnySource:    cfg.AllowScopeTokenFromAnySource,
	}
	if s.tracerProvider == nil {
		s.tracerProvider = tracenoop.NewTracerProvider()
	}
	if s.meterProvider == nil {
		s.meterProvider = metricnoop.NewMeterProvider()
	}

	mux := http.NewServeMux()

	// Git: prefer content-cache, fall back to mirror-backed proxy.
	if cfg.ContentCache != nil && cfg.ContentCache.HasGitHandler() {
		mux.Handle(RouteGit, newCachedGitHandler(cfg.ContentCache, newGitHandlerWithMirrors(cfg.Credentials, cfg.GitMirrors, cfg.Logger), cfg.Logger))
	} else {
		mux.Handle(RouteGit, newGitHandlerWithMirrors(cfg.Credentials, cfg.GitMirrors, cfg.Logger))
	}

	// Registry: prefer content-cache OCI handler, fall back to stub.
	if cfg.ContentCache != nil && cfg.ContentCache.HasOCIHandler() {
		mux.Handle(RouteRegistry, newCachedRegistryHandler(cfg.ContentCache, cfg.Logger))
	} else {
		mux.HandleFunc(RouteRegistry, stubHandler("registry"))
	}

	if cfg.ContentCache != nil && cfg.ContentCache.HasRubyGemsHandler() {
		mux.Handle(RouteRubyGems, newCachedRubyGemsHandler(cfg.ContentCache, cfg.Logger))
	} else {
		mux.HandleFunc(RouteRubyGems, stubHandler("rubygems"))
	}

	mux.HandleFunc(RouteSecrets, stubHandler("secrets"))
	mux.HandleFunc(RouteMeta, stubHandler("meta"))

	s.httpServer = &http.Server{
		Handler: s.identityMiddleware(s.pathMiddleware(s.tracingMiddleware(mux))),
	}

	return s
}

func (s *Server) gatewayMetrics() *observability.GatewayMetrics {
	if s == nil {
		return nil
	}
	s.metricsOnce.Do(func() {
		s.metrics, s.metricsErr = observability.NewGatewayMetrics(s.meterProvider)
		if s.metricsErr != nil && s.logger != nil {
			s.logger.Warn("gateway metrics unavailable", "error", s.metricsErr)
		}
	})
	return s.metrics
}

// Start begins listening for connections in the background.
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return errors.New("gateway server already started")
	}

	ln, err := net.Listen("tcp4", s.addr)
	if err != nil {
		return err
	}
	s.started = true
	s.addr = ln.Addr().String()

	go func() {
		if err := s.httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			if s.logger != nil {
				s.logger.Error("gateway server error", "error", err)
			}
		}
	}()

	if s.logger != nil {
		s.logger.Info("gateway server started", "addr", s.addr)
	}
	return nil
}

// Addr returns the listener address. Only meaningful after Start.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Stop gracefully shuts down the server.
func (s *Server) Stop(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

// identityMiddleware resolves sandbox identity and injects scope into the
// request context. It prefers source-IP identity and falls back to a scoped
// capability token header. Returns 403 when neither identity is valid.
func (s *Server) identityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceIP := extractSourceIP(r.RemoteAddr)
		if sourceIP != "" {
			if scope, ok := s.registry.Lookup(sourceIP); ok {
				ctx := context.WithValue(r.Context(), scopeContextKey, scope)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}

		scopeToken := strings.TrimSpace(r.Header.Get(ScopeTokenHeader))
		if scopeToken != "" && s.isScopeTokenSourceTrusted(sourceIP) {
			if scope, ok := s.registry.LookupScopeToken(scopeToken); ok {
				ctx := context.WithValue(r.Context(), scopeContextKey, scope)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}

		http.Error(w, "forbidden", http.StatusForbidden)
	})
}

func (s *Server) isScopeTokenSourceTrusted(sourceIP string) bool {
	if s.allowScopeTokenFromAnySource {
		return true
	}
	addr, err := netip.ParseAddr(sourceIP)
	if err != nil {
		return false
	}
	if addr.IsLoopback() {
		return true
	}
	for _, prefix := range s.scopeTokenTrustedSourcePrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// pathMiddleware validates and canonicalises the request path.
func (s *Server) pathMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		canonical, err := CanonicalisePath(r.URL.Path)
		if err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		r.URL.Path = canonical
		next.ServeHTTP(w, r)
	})
}

func (s *Server) tracingMiddleware(next http.Handler) http.Handler {
	tracer := s.tracerProvider.Tracer("github.com/buildkite/cleanroom/internal/gateway")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedAt := time.Now()
		ctx := r.Context()
		scope, _ := ScopeFromContext(ctx)
		if scope != nil && scope.TraceContext.IsValid() {
			ctx = trace.ContextWithRemoteSpanContext(ctx, scope.TraceContext)
		}
		requestObs := &gatewayRequestObservability{}
		ctx = context.WithValue(ctx, gatewayRequestContextKey, requestObs)

		service := gatewayServiceForPath(r.URL.Path)
		attributes := []attribute.KeyValue{
			attribute.String(observability.AttrGatewayService, service),
			attribute.String("http.request.method", r.Method),
			attribute.String("url.path", r.URL.Path),
		}
		if scope != nil {
			if scope.SandboxID != "" {
				attributes = append(attributes, attribute.String(observability.AttrSandboxID, scope.SandboxID))
			}
			if scope.ExecutionID != "" {
				attributes = append(attributes, attribute.String(observability.AttrExecutionID, scope.ExecutionID))
			}
		}

		ctx, span := tracer.Start(ctx, observability.GatewayRequestSpanName(service), trace.WithAttributes(attributes...))
		defer span.End()

		status := &gatewayStatusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(status, r.WithContext(ctx))
		span.SetAttributes(attribute.Int("http.response.status_code", status.statusCode))
		reasonCode := strings.TrimSpace(requestObs.reasonCode)
		if reasonCode == "" {
			reasonCode = strings.TrimSpace(status.Header().Get(reasonCodeHeader))
		}
		if reasonCode != "" {
			span.SetAttributes(attribute.String(observability.AttrReasonCode, reasonCode))
		}
		if metrics := s.gatewayMetrics(); metrics != nil {
			metrics.RecordRequest(ctx, service, requestObs.action, reasonCode, status.statusCode, time.Since(startedAt))
		}
		if status.statusCode >= http.StatusBadRequest {
			span.SetStatus(codes.Error, http.StatusText(status.statusCode))
			return
		}
		span.SetStatus(codes.Ok, "")
	})
}

func setGatewayRequestDecision(ctx context.Context, action, reasonCode string) {
	obs, ok := ctx.Value(gatewayRequestContextKey).(*gatewayRequestObservability)
	if !ok || obs == nil {
		return
	}
	if strings.TrimSpace(action) != "" {
		obs.action = strings.TrimSpace(action)
	}
	if strings.TrimSpace(reasonCode) != "" {
		obs.reasonCode = strings.TrimSpace(reasonCode)
	}
}

// extractSourceIP returns the IP portion of a RemoteAddr, handling both
// IPv4 ("10.1.1.2:43210") and IPv6-mapped IPv4 ("[::ffff:10.1.1.2]:43210").
func extractSourceIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return strings.TrimSpace(remoteAddr)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.String()
}

func cloneScopeTokenTrustedSourcePrefixes(prefixes []netip.Prefix) []netip.Prefix {
	if prefixes == nil {
		return nil
	}
	out := make([]netip.Prefix, len(prefixes))
	copy(out, prefixes)
	return out
}

type gatewayStatusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (r *gatewayStatusRecorder) Unwrap() http.ResponseWriter {
	if r == nil {
		return nil
	}
	return r.ResponseWriter
}

func (r *gatewayStatusRecorder) WriteHeader(statusCode int) {
	r.statusCode = statusCode
	r.ResponseWriter.WriteHeader(statusCode)
}

func (r *gatewayStatusRecorder) Write(p []byte) (int, error) {
	if r.statusCode == 0 {
		r.statusCode = http.StatusOK
	}
	return r.ResponseWriter.Write(p)
}

func gatewayServiceForPath(path string) string {
	switch {
	case strings.HasPrefix(path, RouteGit):
		return "git"
	case strings.HasPrefix(path, RouteRegistry):
		return "registry"
	case strings.HasPrefix(path, RouteRubyGems):
		return "rubygems"
	case strings.HasPrefix(path, RouteSecrets):
		return "secrets"
	case strings.HasPrefix(path, RouteMeta):
		return "meta"
	default:
		return "request"
	}
}

func stubHandler(service string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(service + " service not yet implemented"))
	}
}
