package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/backend/darwinvz"
	"github.com/buildkite/cleanroom/internal/backend/firecracker"
	"github.com/buildkite/cleanroom/internal/cachestore"
	"github.com/buildkite/cleanroom/internal/changesetstore"
	"github.com/buildkite/cleanroom/internal/controlserver"
	"github.com/buildkite/cleanroom/internal/controlservice"
	"github.com/buildkite/cleanroom/internal/endpoint"
	"github.com/buildkite/cleanroom/internal/gateway"
	"github.com/buildkite/cleanroom/internal/interactivequic"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/repositorystore"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"github.com/buildkite/cleanroom/internal/snapshotstore"
	"github.com/charmbracelet/log"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

type ServeCommand struct {
	Listen        string `help:"Listen endpoint for control API (defaults to runtime endpoint)"`
	GatewayListen string `help:"Listen address for the host gateway (default :8170, use :0 for ephemeral port)"`
	LogLevel      string `help:"Server log level (debug|info|warn|error)"`
	TLSCert       string `help:"Path to TLS server certificate (auto-discovered from XDG config for https)" env:"CLEANROOM_TLS_CERT"`
	TLSKey        string `help:"Path to TLS server private key (auto-discovered from XDG config for https)" env:"CLEANROOM_TLS_KEY"`
}

var serveSignalNotifyContext = signal.NotifyContext
var newSnapshotMetadataStore = snapshotstore.New
var newCacheMetadataStore = cachestore.New
var newChangesetMetadataStore = changesetstore.New
var gatewayScopeTokenSourcePolicyForGatewayHost = gateway.ScopeTokenSourcePolicyForGatewayHost
var newGatewayContentCache = gateway.NewContentCache

var defaultGatewayFetchHosts = []string{"dl.google.com"}

func (s *ServeCommand) Run(ctx *runtimeContext) error {
	return s.runServer(ctx)
}

func (s *ServeCommand) runServer(ctx *runtimeContext) error {
	ep, err := endpoint.ResolveListen(s.Listen)
	if err != nil {
		return err
	}
	if shouldShowStartupHeader(os.Stderr) {
		gatewayListen := strings.TrimSpace(s.GatewayListen)
		if gatewayListen == "" {
			gatewayListen = fmt.Sprintf(":%d", gateway.DefaultPort)
		}
		fields := []startupField{
			{Key: "workspace", Value: ctx.CWD},
			{Key: "listen", Value: endpointDisplay(ep)},
			{Key: "gateway_listen", Value: gatewayListen},
			{Key: "runtime_config", Value: ctx.ConfigPath},
			{Key: "log_level", Value: effectiveLogLevel(s.LogLevel)},
		}
		fields = append(fields, observabilityStartupFields(ctx.Config.Observability)...)
		if err := writeStartupHeader(os.Stderr, startupHeader{
			Title:  "cleanroom serve",
			Fields: fields,
		}, shouldUseANSI(os.Stderr)); err != nil {
			return err
		}
	}

	logger, err := newLogger(s.LogLevel, ctx.Config.Observability, "server")
	if err != nil {
		return err
	}
	log.SetDefault(logger)
	configureBackendLogging(ctx.Backends, logger)
	serviceLogger := logger.With("subsystem", "service")
	httpLogger := logger.With("subsystem", "http")
	interactiveLogger := logger.With("subsystem", "interactive-quic")

	gwRegistry := gateway.NewRegistry()
	gwCredentials := gateway.NewChainCredentialProvider(
		gateway.NewEnvCredentialProvider(),
		gateway.NewGitCredentialFillProvider(ctx.CWD, nil),
	)
	gwMirrors, err := gateway.NewDefaultGitMirrorStore(gwCredentials)
	if err != nil {
		return fmt.Errorf("configure git mirror store: %w", err)
	}

	var contentCache *gateway.ContentCache
	contentCache, err = newGatewayContentCache(gateway.ContentCacheConfig{
		GitAllowedHosts:   ctx.Config.Gateway.Git.CacheHosts,
		OCIRegistries:     ctx.Config.Gateway.OCI.Registries,
		FetchAllowedHosts: append([]string(nil), defaultGatewayFetchHosts...),
		Credentials:       gwCredentials,
		Logger:            logger.With("subsystem", "content-cache"),
	})
	if err != nil {
		logger.Warn("content cache unavailable; continuing without cache-backed gateway routes", "error", err)
	} else {
		defer contentCache.Close()
	}

	darwinGatewayHost := strings.TrimSpace(os.Getenv("CLEANROOM_DARWIN_GATEWAY_HOST"))
	gwServer := gateway.NewServer(gatewayServerConfig(
		s.GatewayListen,
		gwRegistry,
		gwCredentials,
		gwMirrors,
		contentCache,
		ctx.Observability.TracerProvider(),
		ctx.Observability.MeterProvider(),
		logger.With("subsystem", "gateway"),
		darwinGatewayHost,
	))
	if err := gwServer.Start(); err != nil {
		return fmt.Errorf("start gateway: %w", err)
	}

	gwPort := gateway.DefaultPort
	if _, portStr, err := net.SplitHostPort(gwServer.Addr()); err == nil {
		if p, err := strconv.Atoi(portStr); err == nil && p > 0 {
			gwPort = p
		}
	}

	configureGatewayBackends(ctx.Backends, gwRegistry, gwPort, gwServer.Addr(), darwinGatewayHost, gateway.ProxyRoutes{
		DockerHubMirror: contentCache != nil && contentCache.HasOCIHandler(),
		RubyGems:        contentCache != nil && contentCache.HasRubyGemsHandler(),
		GoProxy:         contentCache != nil && contentCache.HasGoProxyHandler() && contentCache.HasSumDBHandler(),
		Fetch:           contentCache != nil && contentCache.FetchAllowsHost("dl.google.com"),
	})

	if fcAdapter, ok := ctx.Backends["firecracker"].(*firecracker.Adapter); ok && fcAdapter.GatewayRegistry != nil {
		if shouldInstallGatewayFirewall(runtime.GOOS) {
			fwCfg := backend.FirecrackerConfig{
				PrivilegedHelperPath: ctx.Config.Backends.Firecracker.PrivilegedHelperPath,
			}
			fwCleanup, err := firecracker.SetupGatewayFirewall(context.Background(), gwPort, fwCfg, logger.With("subsystem", "gateway"))
			if err != nil {
				logger.Warn("failed to install gateway firewall rules", "error", err)
			} else {
				defer fwCleanup()
			}
		}
	}

	var serverTLS *controlserver.TLSOptions
	if ep.Scheme == "https" {
		serverTLS = &controlserver.TLSOptions{
			CertPath: s.TLSCert,
			KeyPath:  s.TLSKey,
		}
	}

	service, err := newControlService(ctx, serviceLogger, gwMirrors)
	if err != nil {
		return err
	}
	serverInterceptors := make([]connect.Interceptor, 0, 1)
	if interceptor := ctx.Observability.ConnectInterceptor(); interceptor != nil {
		serverInterceptors = append(serverInterceptors, interceptor)
	}
	server := controlserver.New(service, httpLogger, serverInterceptors...).TrustInternalWorkspaceCopyInRequests()

	runCtx, cancel := serveSignalNotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	interactiveListen, interactiveHost := resolveInteractiveQUICEndpoint(ep)
	interactiveServer, err := interactivequic.Start(runCtx, interactiveListen, service, interactiveLogger)
	if err != nil {
		return fmt.Errorf("start interactive quic server: %w", err)
	}
	defer interactiveServer.Close()

	interactiveEndpoint := interactiveAdvertiseEndpoint(interactiveServer.Addr(), interactiveHost)
	service.ConfigureInteractiveTransport(interactiveEndpoint, interactiveServer.ALPN(), interactiveServer.CertPinSHA256())
	interactiveLogger.Info(
		"interactive QUIC server ready",
		"listen", interactiveServer.Addr().String(),
		"endpoint", interactiveEndpoint,
		"alpn", interactiveServer.ALPN(),
	)

	runErr := controlserver.Serve(runCtx, ep, server.Handler(), httpLogger, serverTLS)
	gwStopCtx, gwStopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer gwStopCancel()
	_ = gwServer.Stop(gwStopCtx)
	return runErr
}

func gatewayServerConfig(listen string, registry *gateway.Registry, credentials gateway.CredentialProvider, mirrors gateway.GitMirrorStore, contentCache *gateway.ContentCache, tracerProvider trace.TracerProvider, meterProvider metric.MeterProvider, logger *log.Logger, darwinGatewayHost string) gateway.ServerConfig {
	sourcePolicy := gatewayScopeTokenSourcePolicyForGatewayHost(strings.TrimSpace(darwinGatewayHost))
	return gateway.ServerConfig{
		ListenAddr:                      listen,
		Registry:                        registry,
		Credentials:                     credentials,
		GitMirrors:                      mirrors,
		ContentCache:                    contentCache,
		Logger:                          logger,
		TracerProvider:                  tracerProvider,
		MeterProvider:                   meterProvider,
		ScopeTokenTrustedSourcePrefixes: sourcePolicy.TrustedSourcePrefixes,
		AllowScopeTokenFromAnySource:    sourcePolicy.AllowScopeTokenFromAnySource,
	}
}

func observabilityStartupFields(cfg runtimeconfig.ObservabilityConfig) []startupField {
	format, err := runtimeconfig.ResolveObservabilityLogFormat(cfg)
	if err != nil {
		return []startupField{{Key: "observability", Value: "invalid"}}
	}
	if !cfg.Enabled {
		return []startupField{
			{Key: "observability", Value: "disabled"},
			{Key: "log_format", Value: format},
		}
	}

	protocol, err := runtimeconfig.ResolveOTLPTraceProtocol(cfg)
	if err != nil {
		return []startupField{{Key: "observability", Value: "invalid"}}
	}
	fields := []startupField{{Key: "observability", Value: "enabled"}}
	fields = append(fields, startupField{Key: "log_format", Value: format})
	fields = append(fields, startupField{Key: "trace_export", Value: fmt.Sprintf("otlp/%s -> %s", protocol, strings.TrimSpace(cfg.OTLP.Endpoint))})
	fields = append(fields, startupField{Key: "trace_sampling", Value: formatTraceSampling(cfg.Traces.Sampling)})
	if strings.TrimSpace(cfg.Traces.URLTemplate) != "" {
		fields = append(fields, startupField{Key: "trace_links", Value: "enabled"})
	}
	return fields
}

func formatTraceSampling(cfg runtimeconfig.TraceSamplingConfig) string {
	mode := strings.TrimSpace(cfg.Mode)
	if mode == "" {
		mode = "parentbased_traceidratio"
	}
	if cfg.Ratio == nil {
		return mode
	}
	return fmt.Sprintf("%s ratio=%g", mode, *cfg.Ratio)
}

func configureBackendLogging(backends map[string]backend.Adapter, logger *log.Logger) {
	if logger == nil {
		return
	}
	if fcAdapter, ok := backends["firecracker"].(*firecracker.Adapter); ok {
		fcAdapter.Logger = observability.WithLoggerFields(
			logger,
			observability.LogFieldSubsystem, "firecracker",
			observability.LogFieldBackend, "firecracker",
		)
	}
}

func configureGatewayBackends(backends map[string]backend.Adapter, gwRegistry *gateway.Registry, gwPort int, gwListenAddr, darwinGatewayHost string, routes gateway.ProxyRoutes) {
	if fcAdapter, ok := backends["firecracker"].(*firecracker.Adapter); ok {
		fcAdapter.GatewayRegistry = gwRegistry
		fcAdapter.GatewayPort = gwPort
		fcAdapter.GatewayRoutes = routes
	}

	if darwinAdapter, ok := backends["darwin-vz"].(*darwinvz.Adapter); ok {
		darwinAdapter.GatewayRegistry = gwRegistry
		darwinAdapter.GatewayPort = gwPort
		darwinAdapter.GatewayBridgeURL = gatewayBridgeURL(gwPort, gwListenAddr)
		darwinAdapter.GatewayRoutes = routes
	}
}

func gatewayBridgeURL(port int, listenAddr string) string {
	if port <= 0 {
		port = gateway.DefaultPort
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(listenAddr))
	if err != nil {
		host = ""
	}
	host = strings.Trim(host, "[]")
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%d", host, port)
}

func newControlService(ctx *runtimeContext, logger *log.Logger, mirrors gateway.GitMirrorStore) (*controlservice.Service, error) {
	snapshotMetadataStore, err := newSnapshotMetadataStore(snapshotstore.Options{})
	if err != nil {
		return nil, fmt.Errorf("configure snapshot metadata store: %w", err)
	}
	var cacheMetadataStore *cachestore.Store
	cacheMetadataStore, err = newCacheMetadataStore(cachestore.Options{})
	if err != nil {
		if logger != nil {
			logger.Warn("cache metadata store unavailable; stage caches disabled", "error", err)
		}
		cacheMetadataStore = nil
	}
	var changesetMetadataStore *changesetstore.Store
	changesetMetadataStore, err = newChangesetMetadataStore(changesetstore.Options{})
	if err != nil {
		if logger != nil {
			logger.Warn("changeset metadata store unavailable; repository changeset persistence disabled", "error", err)
		}
		changesetMetadataStore = nil
	}

	if ctx == nil {
		service := &controlservice.Service{
			Logger:          logger,
			RepositoryStore: repositorystore.NewMirrorBacked(mirrors),
			SnapshotStore:   snapshotMetadataStore,
		}
		if cacheMetadataStore != nil {
			service.CacheStore = cacheMetadataStore
		}
		if changesetMetadataStore != nil {
			service.ChangesetStore = changesetMetadataStore
		}
		return service, nil
	}
	service := &controlservice.Service{
		Loader:          ctx.Loader,
		Config:          ctx.Config,
		Backends:        ctx.Backends,
		Logger:          logger,
		Observability:   ctx.Observability,
		RepositoryStore: repositorystore.NewMirrorBacked(mirrors),
		SnapshotStore:   snapshotMetadataStore,
	}
	if cacheMetadataStore != nil {
		service.CacheStore = cacheMetadataStore
	}
	if changesetMetadataStore != nil {
		service.ChangesetStore = changesetMetadataStore
	}
	return service, nil
}

func shouldInstallGatewayFirewall(goos string) bool {
	return strings.EqualFold(strings.TrimSpace(goos), "linux")
}
