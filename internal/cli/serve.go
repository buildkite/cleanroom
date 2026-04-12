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
	"github.com/buildkite/cleanroom/internal/controlserver"
	"github.com/buildkite/cleanroom/internal/controlservice"
	"github.com/buildkite/cleanroom/internal/endpoint"
	"github.com/buildkite/cleanroom/internal/gateway"
	"github.com/buildkite/cleanroom/internal/interactivequic"
	"github.com/buildkite/cleanroom/internal/snapshotstore"
	"github.com/charmbracelet/log"
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
var gatewayScopeTokenSourcePolicyForGatewayHost = gateway.ScopeTokenSourcePolicyForGatewayHost
var newGatewayContentCache = gateway.NewContentCache

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
		if err := writeStartupHeader(os.Stderr, startupHeader{
			Title: "cleanroom serve",
			Fields: []startupField{
				{Key: "workspace", Value: ctx.CWD},
				{Key: "listen", Value: endpointDisplay(ep)},
				{Key: "gateway_listen", Value: gatewayListen},
				{Key: "runtime_config", Value: ctx.ConfigPath},
				{Key: "log_level", Value: effectiveLogLevel(s.LogLevel)},
			},
		}, shouldUseANSI(os.Stderr)); err != nil {
			return err
		}
	}

	logger, err := newLogger(s.LogLevel, "server")
	if err != nil {
		return err
	}
	log.SetDefault(logger)

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
		Credentials: gwCredentials,
		Logger:      logger.With("subsystem", "content-cache"),
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

	configureGatewayBackends(ctx.Backends, gwRegistry, gwPort, gwServer.Addr(), darwinGatewayHost)

	if fcAdapter, ok := ctx.Backends["firecracker"].(*firecracker.Adapter); ok && fcAdapter.GatewayRegistry != nil {
		if shouldInstallGatewayFirewall(runtime.GOOS) {
			fwCfg := backend.FirecrackerConfig{
				PrivilegedHelperPath: ctx.Config.Backends.Firecracker.PrivilegedHelperPath,
			}
			fwCleanup, err := firecracker.SetupGatewayFirewall(context.Background(), gwPort, fwCfg)
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

	service, err := newControlService(ctx, logger.With("subsystem", "service"), gwMirrors)
	if err != nil {
		return err
	}
	serverInterceptors := make([]connect.Interceptor, 0, 1)
	if interceptor := ctx.Observability.ConnectInterceptor(); interceptor != nil {
		serverInterceptors = append(serverInterceptors, interceptor)
	}
	server := controlserver.New(service, logger.With("subsystem", "http"), serverInterceptors...)

	runCtx, cancel := serveSignalNotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	interactiveListen, interactiveHost := resolveInteractiveQUICEndpoint(ep)
	interactiveServer, err := interactivequic.Start(runCtx, interactiveListen, service, logger.With("subsystem", "interactive-quic"))
	if err != nil {
		return fmt.Errorf("start interactive quic server: %w", err)
	}
	defer interactiveServer.Close()

	interactiveEndpoint := interactiveAdvertiseEndpoint(interactiveServer.Addr(), interactiveHost)
	service.ConfigureInteractiveTransport(interactiveEndpoint, interactiveServer.ALPN(), interactiveServer.CertPinSHA256())
	logger.Info(
		"interactive QUIC server ready",
		"listen", interactiveServer.Addr().String(),
		"endpoint", interactiveEndpoint,
		"alpn", interactiveServer.ALPN(),
	)

	runErr := controlserver.Serve(runCtx, ep, server.Handler(), logger, serverTLS)
	gwStopCtx, gwStopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer gwStopCancel()
	_ = gwServer.Stop(gwStopCtx)
	return runErr
}

func gatewayServerConfig(listen string, registry *gateway.Registry, credentials gateway.CredentialProvider, mirrors gateway.GitMirrorStore, contentCache *gateway.ContentCache, logger *log.Logger, darwinGatewayHost string) gateway.ServerConfig {
	sourcePolicy := gatewayScopeTokenSourcePolicyForGatewayHost(strings.TrimSpace(darwinGatewayHost))
	return gateway.ServerConfig{
		ListenAddr:                      listen,
		Registry:                        registry,
		Credentials:                     credentials,
		GitMirrors:                      mirrors,
		ContentCache:                    contentCache,
		Logger:                          logger,
		ScopeTokenTrustedSourcePrefixes: sourcePolicy.TrustedSourcePrefixes,
		AllowScopeTokenFromAnySource:    sourcePolicy.AllowScopeTokenFromAnySource,
	}
}

func configureGatewayBackends(backends map[string]backend.Adapter, gwRegistry *gateway.Registry, gwPort int, gwListenAddr, darwinGatewayHost string) {
	if fcAdapter, ok := backends["firecracker"].(*firecracker.Adapter); ok {
		fcAdapter.GatewayRegistry = gwRegistry
		fcAdapter.GatewayPort = gwPort
	}

	if darwinAdapter, ok := backends["darwin-vz"].(*darwinvz.Adapter); ok {
		darwinAdapter.GatewayRegistry = gwRegistry
		darwinAdapter.GatewayPort = gwPort
		darwinAdapter.GatewayBridgeURL = gatewayBridgeURL(gwPort, gwListenAddr)
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

	if ctx == nil {
		service := &controlservice.Service{
			Logger:            logger,
			RepositoryMirrors: mirrors,
			SnapshotStore:     snapshotMetadataStore,
		}
		if cacheMetadataStore != nil {
			service.CacheStore = cacheMetadataStore
		}
		return service, nil
	}
	service := &controlservice.Service{
		Loader:            ctx.Loader,
		Config:            ctx.Config,
		Backends:          ctx.Backends,
		Logger:            logger,
		Observability:     ctx.Observability,
		RepositoryMirrors: mirrors,
		SnapshotStore:     snapshotMetadataStore,
	}
	if cacheMetadataStore != nil {
		service.CacheStore = cacheMetadataStore
	}
	return service, nil
}

func shouldInstallGatewayFirewall(goos string) bool {
	return strings.EqualFold(strings.TrimSpace(goos), "linux")
}
