package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"charm.land/log/v2"
	"connectrpc.com/connect"
	"github.com/buildkite/cleanroom/internal/authz"
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
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

type ServeCommand struct {
	Listen                  string   `help:"Listen endpoint for control API (defaults to runtime endpoint)"`
	GatewayListen           string   `help:"Listen address for the host gateway (default :8170, use :0 for ephemeral port)"`
	LogLevel                string   `help:"Server log level (debug|info|warn|error)"`
	PprofListen             string   `help:"Enable local Go pprof HTTP server at this loopback address, for example 127.0.0.1:6060"`
	TLSCert                 string   `help:"Path to TLS server certificate (auto-discovered from XDG config for https)" env:"CLEANROOM_TLS_CERT"`
	TLSKey                  string   `help:"Path to TLS server private key (auto-discovered from XDG config for https)" env:"CLEANROOM_TLS_KEY"`
	GitHubAppID             string   `name:"github-app-id" help:"GitHub App ID for host-side Git authentication" env:"CLEANROOM_GITHUB_APP_ID"`
	GitHubAppInstallationID string   `name:"github-app-installation-id" help:"GitHub App installation ID for host-side Git authentication" env:"CLEANROOM_GITHUB_APP_INSTALLATION_ID"`
	GitHubAppPrivateKeyFile string   `name:"github-app-private-key-file" help:"Path to the GitHub App private key PEM for host-side Git authentication" env:"CLEANROOM_GITHUB_APP_PRIVATE_KEY_FILE"`
	GitHubAppRepoPrefixes   []string `name:"github-app-repo-prefixes" sep:"," help:"Comma-separated GitHub owner/ or owner/repo scopes where GitHub App credentials may be used" env:"CLEANROOM_GITHUB_APP_REPO_PREFIXES"`
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
	pprofServer, err := startLocalPprofServer(s.PprofListen)
	if err != nil {
		return err
	}
	if pprofServer != nil {
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = pprofServer.Shutdown(shutdownCtx)
		}()
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
		if pprofServer != nil {
			fields = append(fields, startupField{Key: "pprof", Value: pprofServer.URL()})
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
	githubAppConfig, err := s.githubAppCredentialsConfig(ctx.Config.Gateway.Credentials.GitHubApp, ctx.CWD)
	if err != nil {
		return err
	}
	githubAppCredentials, err := gatewayGitHubAppCredentials(githubAppConfig, ctx.ConfigPath)
	if err != nil {
		return fmt.Errorf("configure GitHub App credentials: %w", err)
	}
	gwCredentials := gateway.NewChainCredentialProvider(
		githubAppCredentials,
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
		DockerHubMirror:       contentCache != nil && contentCache.HasOCIHandler(),
		DockerRegistryMirrors: dockerRegistryMirrorHosts(contentCache),
		RubyGems:              contentCache != nil && contentCache.HasRubyGemsHandler(),
		GoProxy:               contentCache != nil && contentCache.HasGoProxyHandler() && contentCache.HasSumDBHandler(),
		Fetch:                 contentCache != nil && contentCache.FetchAllowsHost("dl.google.com"),
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
	authInterceptor, err := s.authInterceptor(ctx, ep)
	if err != nil {
		return err
	}

	service, err := newControlService(ctx, serviceLogger, gwMirrors)
	if err != nil {
		return err
	}
	service.ScheduleStartupStorageCleanup()
	serverInterceptors := make([]connect.Interceptor, 0, 1)
	if authInterceptor != nil {
		serverInterceptors = append(serverInterceptors, authInterceptor)
	}
	if interceptor := ctx.Observability.ConnectInterceptor(); interceptor != nil {
		serverInterceptors = append(serverInterceptors, interceptor)
	}
	server := controlserver.New(service, httpLogger, serverInterceptors...)
	if ep.Scheme == "unix" {
		server.TrustInternalWorkspaceCopyInRequests()
	} else {
		server.TrustInternalWorkspaceCopyInRequestsFromLoopback()
	}

	runCtx, cancel := serveSignalNotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	service.StartIdleSuspendWorker(runCtx)
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

func (s *ServeCommand) authInterceptor(ctx *runtimeContext, ep endpoint.Endpoint) (connect.Interceptor, error) {
	if ctx == nil || !ctx.Config.Auth.Required || ep.Scheme == "unix" {
		return nil, nil
	}
	if err := validateBearerAuthListenEndpoint(ep); err != nil {
		return nil, err
	}
	policyPath, err := resolveAuthPolicyPath(ctx.CWD, ctx.ConfigPath, "", ctx.Config.Auth.PolicyFile)
	if err != nil {
		return nil, err
	}
	policy, err := authz.LoadPolicyFile(policyPath)
	if err != nil {
		return nil, err
	}
	validator, err := authz.NewOIDCValidator(ctx.Config.Auth.OIDC.Issuers)
	if err != nil {
		return nil, err
	}
	return controlserver.BearerAuthenticator{
		Validator: validator,
		Policy:    policy,
	}.Interceptor(), nil
}

func validateBearerAuthListenEndpoint(ep endpoint.Endpoint) error {
	if ep.Scheme != "http" {
		return nil
	}
	parsed, err := url.Parse(strings.TrimSpace(ep.Address))
	if err != nil {
		return fmt.Errorf("validate auth listen endpoint: %w", err)
	}
	host := strings.TrimSpace(parsed.Hostname())
	if host == "" || host == "0.0.0.0" || host == "::" {
		return errors.New("auth.required cannot use bearer tokens on a non-loopback http listen endpoint; use https or a loopback http address")
	}
	if !isLoopbackListenHost(host) {
		return errors.New("auth.required cannot use bearer tokens on a non-loopback http listen endpoint; use https or a loopback http address")
	}
	return nil
}

func isLoopbackListenHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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

func (s *ServeCommand) githubAppCredentialsConfig(base runtimeconfig.GatewayGitHubAppCredentialsConfig, cwd string) (runtimeconfig.GatewayGitHubAppCredentialsConfig, error) {
	privateKeyFile := s.GitHubAppPrivateKeyFile
	if strings.TrimSpace(privateKeyFile) != "" {
		resolved, err := resolveInvocationCredentialPath(cwd, privateKeyFile)
		if err != nil {
			return runtimeconfig.GatewayGitHubAppCredentialsConfig{}, fmt.Errorf("resolve --github-app-private-key-file path: %w", err)
		}
		privateKeyFile = resolved
	}
	return overlayGatewayGitHubAppCredentials(
		base,
		s.GitHubAppID,
		s.GitHubAppInstallationID,
		privateKeyFile,
		s.GitHubAppRepoPrefixes,
	), nil
}

func overlayGatewayGitHubAppCredentials(base runtimeconfig.GatewayGitHubAppCredentialsConfig, appID, installationID, privateKeyFile string, repoPrefixes []string) runtimeconfig.GatewayGitHubAppCredentialsConfig {
	out := base
	if value := strings.TrimSpace(appID); value != "" {
		out.AppID = runtimeconfig.ScalarString(value)
	}
	if value := strings.TrimSpace(installationID); value != "" {
		out.InstallationID = runtimeconfig.ScalarString(value)
	}
	if value := strings.TrimSpace(privateKeyFile); value != "" {
		out.PrivateKeyFile = value
	}
	if values := trimNonEmptyStrings(repoPrefixes); len(values) > 0 {
		out.RepoPrefixes = values
	}
	return out
}

func trimNonEmptyStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func gatewayGitHubAppCredentials(cfg runtimeconfig.GatewayGitHubAppCredentialsConfig, configPath string) (gateway.CredentialProvider, error) {
	if !runtimeconfig.GatewayGitHubAppCredentialsConfigured(cfg) {
		return nil, nil
	}

	privateKeyFile, err := resolveRuntimeConfigCredentialPath(configPath, cfg.PrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("resolve private_key_file: %w", err)
	}
	return gateway.NewGitHubAppCredentialProviderFromConfig(gateway.GitHubAppCredentialConfig{
		AppID:          string(cfg.AppID),
		InstallationID: string(cfg.InstallationID),
		PrivateKeyFile: privateKeyFile,
		RepoPrefixes:   cfg.RepoPrefixes,
	})
}

func resolveRuntimeConfigCredentialPath(configPath, value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", nil
	}
	if strings.HasPrefix(trimmed, "~/") {
		home, err := os.UserHomeDir()
		if err != nil || strings.TrimSpace(home) == "" {
			if err != nil {
				return "", err
			}
			return "", errors.New("home directory is not available")
		}
		return filepath.Clean(filepath.Join(home, strings.TrimPrefix(trimmed, "~/"))), nil
	}
	if filepath.IsAbs(trimmed) {
		return filepath.Clean(trimmed), nil
	}
	if configPath = strings.TrimSpace(configPath); configPath != "" {
		return filepath.Clean(filepath.Join(filepath.Dir(configPath), trimmed)), nil
	}
	return filepath.Abs(trimmed)
}

func resolveInvocationCredentialPath(cwd, value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", nil
	}
	if strings.HasPrefix(trimmed, "~/") {
		home, err := os.UserHomeDir()
		if err != nil || strings.TrimSpace(home) == "" {
			if err != nil {
				return "", err
			}
			return "", errors.New("home directory is not available")
		}
		return filepath.Clean(filepath.Join(home, strings.TrimPrefix(trimmed, "~/"))), nil
	}
	if filepath.IsAbs(trimmed) {
		return filepath.Clean(trimmed), nil
	}
	base := strings.TrimSpace(cwd)
	if base == "" {
		base = "."
	}
	if !filepath.IsAbs(base) {
		absBase, err := filepath.Abs(base)
		if err != nil {
			return "", fmt.Errorf("resolve absolute working directory: %w", err)
		}
		base = absBase
	}
	return filepath.Clean(filepath.Join(base, trimmed)), nil
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

type localPprofServer struct {
	server   *http.Server
	listener net.Listener
}

func startLocalPprofServer(listen string) (*localPprofServer, error) {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return nil, nil
	}
	if err := validateLocalPprofListen(listen); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("listen for pprof on %q: %w", listen, err)
	}
	server := &http.Server{
		Handler:           pprofHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	pprofServer := &localPprofServer{
		server:   server,
		listener: listener,
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("pprof server stopped unexpectedly", "listen", listener.Addr().String(), "error", err)
		}
	}()
	return pprofServer, nil
}

func validateLocalPprofListen(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("parse pprof listen address %q: %w", listen, err)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("pprof listen host must be localhost or a loopback IP, got %q", host)
	}
	if !addr.IsLoopback() {
		return fmt.Errorf("pprof listen host must be loopback, got %q", host)
	}
	return nil
}

func (s *localPprofServer) URL() string {
	addr := s.listener.Addr().String()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr + "/debug/pprof/"
	}
	return "http://" + net.JoinHostPort(host, port) + "/debug/pprof/"
}

func (s *localPprofServer) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

func pprofHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
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

func dockerRegistryMirrorHosts(contentCache *gateway.ContentCache) []string {
	if contentCache == nil || !contentCache.HasOCIHandler() {
		return nil
	}
	return contentCache.OCIMirrorHosts()
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
		Loader:                  ctx.Loader,
		Config:                  ctx.Config,
		Backends:                ctx.Backends,
		Logger:                  logger,
		Observability:           ctx.Observability,
		RepositoryStore:         repositorystore.NewMirrorBacked(mirrors),
		SnapshotStore:           snapshotMetadataStore,
		ZFSImportDatasetStore:   systemZFSImportDatasetStore(ctx.Config),
		CachePeerTransferDriver: systemCachePeerTransferDriver(ctx.Config),
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
