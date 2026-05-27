package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/kong"
	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/backend/darwinvz"
	"github.com/buildkite/cleanroom/internal/backend/firecracker"
	"github.com/buildkite/cleanroom/internal/controlclient"
	"github.com/buildkite/cleanroom/internal/endpoint"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"github.com/buildkite/cleanroom/internal/tlsconfig"
)

const defaultBumpRefSource = "ghcr.io/buildkite/cleanroom-base/alpine:latest"

const (
	systemdServiceName       = "cleanroom.service"
	launchdServiceName       = "com.buildkite.cleanroom"
	defaultDaemonListen      = "unix://" + endpoint.DefaultSystemSocketPath
	sandboxTerminateTimeout  = 2 * time.Second
	interruptForceExitWindow = 1200 * time.Millisecond
)

type policyLoader interface {
	LoadAndCompile(cwd string) (*policy.CompiledPolicy, string, error)
	LoadRepository(cwd string) (policy.RepositoryConfig, string, error)
	LoadExpose(cwd string) (policy.ExposeConfig, string, error)
}

type runtimeContext struct {
	CWD           string
	Stdout        *os.File
	Stderr        *os.File
	Loader        policyLoader
	Config        runtimeconfig.Config
	ConfigPath    string
	Version       string
	Backends      map[string]backend.Adapter
	Observability *observability.Runtime
}

type CLI struct {
	Auth        AuthCommand        `cmd:"" help:"Authentication and authorization diagnostics"`
	Policy      PolicyCommand      `cmd:"" help:"Policy commands"`
	Config      ConfigCommand      `cmd:"" help:"Runtime config commands"`
	Image       ImageCommand       `cmd:"" help:"Manage OCI image cache artifacts"`
	Inspect     InspectCommand     `cmd:"" help:"Inspect a sandbox, execution, or snapshot by ID"`
	Snapshot    SnapshotCommand    `cmd:"" help:"Manage snapshots"`
	Create      CreateCommand      `cmd:"" help:"Create a sandbox using repo policy"`
	Exec        ExecCommand        `cmd:"" help:"Execute a command in a sandbox"`
	Console     ConsoleCommand     `cmd:"" help:"Run a command with an interactive tty in a sandbox"`
	Copy        CopyCommand        `name:"copy" aliases:"cp" cmd:"" help:"Copy one file into or out of a sandbox"`
	Workspace   WorkspaceCommand   `cmd:"" help:"Manage sandbox workspace contents"`
	Expose      ExposeCommand      `cmd:"" help:"Expose sandbox ports on this client"`
	PortForward PortForwardCommand `name:"port-forward" cmd:"" help:"Forward local TCP ports to a sandbox"`
	Serve       ServeCommand       `cmd:"" help:"Run the cleanroom control-plane server in the foreground"`
	Daemon      DaemonCommand      `cmd:"" help:"Manage daemon/service lifecycle"`
	DNS         DNSCommand         `name:"dns" cmd:"" help:"Manage host DNS for local exposures"`
	Doctor      DoctorCommand      `cmd:"" help:"Run environment and backend diagnostics"`
	Execution   ExecutionCommand   `cmd:"" help:"Inspect command executions and diagnostics"`
	Status      StatusCommand      `cmd:"" help:"Browse retained execution artifacts"`
	Sandbox     SandboxCommand     `cmd:"" help:"Manage sandboxes"`
	System      SystemCommand      `cmd:"" help:"Inspect and prune host storage"`
	Version     VersionCommand     `cmd:"" help:"Print version information"`
}

type VersionCommand struct {
	version string
}

func (c *VersionCommand) Run() error {
	fmt.Println("cleanroom version " + c.version)
	return nil
}

type clientFlags struct {
	Host          string `help:"Control-plane endpoint (unix://path, http://host:port, or https://host:port)" env:"CLEANROOM_HOST"`
	TLSCA         string `name:"tls-ca" aliases:"tlsca" help:"Path to CA certificate for server verification (auto-discovered from XDG config for https)" env:"CLEANROOM_TLS_CA"`
	AuthTokenFile string `name:"auth-token-file" help:"Path to a bearer token file" env:"CLEANROOM_AUTH_TOKEN_FILE"`
	AuthTokenEnv  string `name:"auth-token-env" help:"Environment variable containing a bearer token (default CLEANROOM_AUTH_TOKEN)"`
}

func (f *clientFlags) resolvedHost(cfg runtimeconfig.Config) string {
	host := strings.TrimSpace(f.Host)
	if host != "" {
		return host
	}
	return strings.TrimSpace(cfg.ControlHost)
}

func (f *clientFlags) connect(ctx *runtimeContext) (*controlclient.Client, error) {
	ep, err := endpoint.Resolve(f.resolvedHost(ctx.Config))
	if err != nil {
		return nil, err
	}
	options := []controlclient.Option{controlclient.WithTLS(tlsconfig.Options{
		CAPath: f.TLSCA,
	})}
	if token, err := f.resolveAuthToken(ctx); err != nil {
		return nil, err
	} else if token != "" {
		if err := validateBearerAuthClientEndpoint(ep); err != nil {
			return nil, err
		}
		options = append(options, controlclient.WithBearerToken(token))
	}
	if ctx != nil {
		if interceptor := ctx.Observability.ConnectInterceptor(); interceptor != nil {
			options = append(options, controlclient.WithConnectInterceptors(interceptor))
		}
	}
	return controlclient.New(ep, options...)
}

func (f *clientFlags) resolveAuthToken(ctx *runtimeContext) (string, error) {
	tokenFile := strings.TrimSpace(f.AuthTokenFile)
	if tokenFile != "" {
		if ctx != nil && !filepath.IsAbs(tokenFile) {
			tokenFile = filepath.Join(ctx.CWD, tokenFile)
		}
		raw, err := os.ReadFile(tokenFile)
		if err != nil {
			return "", fmt.Errorf("read auth token file %s: %w", tokenFile, err)
		}
		token := strings.TrimSpace(string(raw))
		if token == "" {
			return "", fmt.Errorf("auth token file %s is empty", tokenFile)
		}
		return token, nil
	}

	envName := strings.TrimSpace(f.AuthTokenEnv)
	explicitEnv := envName != ""
	if envName == "" {
		envName = "CLEANROOM_AUTH_TOKEN"
	}
	token := strings.TrimSpace(os.Getenv(envName))
	if token == "" && explicitEnv {
		return "", fmt.Errorf("auth token env %s is empty", envName)
	}
	return token, nil
}

func validateBearerAuthClientEndpoint(ep endpoint.Endpoint) error {
	if ep.Scheme != "http" {
		return nil
	}
	parsed, err := url.Parse(strings.TrimSpace(ep.Address))
	if err != nil {
		return fmt.Errorf("validate auth client endpoint: %w", err)
	}
	host := strings.TrimSpace(parsed.Hostname())
	if host == "" || !isLoopbackListenHost(host) {
		return errors.New("auth token cannot be sent to a non-loopback http endpoint; use https or a loopback http address")
	}
	return nil
}

type exitCodeError struct {
	code int
}

func (e exitCodeError) Error() string {
	return fmt.Sprintf("command failed with exit code %d", e.code)
}

func (e exitCodeError) ExitCode() int {
	return e.code
}

type hasExitCode interface {
	ExitCode() int
}

func Run(args []string, version string) (runErr error) {
	args = normalizeBareExposeHTTPSArgs(args)
	runtimeCtx := &runtimeContext{
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Loader:  policy.Loader{},
		Version: version,
	}

	cli := CLI{}
	cli.Version.version = version
	parser, err := kong.New(
		&cli,
		kong.Name("cleanroom"),
		kong.Description("Cleanroom CLI"),
	)
	if err != nil {
		return err
	}

	ctx, err := parser.Parse(args)
	if err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	runtimeCtx.CWD = cwd

	if !commandBypassesStartupRuntimeConfig(ctx) {
		cfg, cfgPath, err := runtimeconfig.Load()
		if err != nil {
			return err
		}
		runtimeCtx.Config = cfg
		runtimeCtx.ConfigPath = cfgPath
		runtimeCtx.Backends = map[string]backend.Adapter{
			"firecracker": firecracker.New(),
			"darwin-vz":   darwinvz.New(),
		}
		configureBackendRuntimeConfig(runtimeCtx.Backends, cfg, version)

		if commandUsesStartupObservability(ctx) {
			obsRuntime, err := observability.Start(context.Background(), observability.Options{
				Config:         cfg.Observability,
				ServiceName:    runtimeServiceName(ctx),
				ServiceVersion: version,
				ReportError:    newObservabilityErrorReporter(runtimeCtx.stderr()),
			})
			if err != nil {
				return fmt.Errorf("configure observability: %w", err)
			}
			runtimeCtx.Observability = obsRuntime
			configureBackendObservability(runtimeCtx.Backends, obsRuntime)
			defer func() {
				reportObservabilityShutdown(&runErr, runtimeCtx.stderr(), obsRuntime.Shutdown(context.Background()))
			}()
		}
	}

	runErr = ctx.Run(runtimeCtx)
	return runErr
}

func commandBypassesStartupRuntimeConfig(ctx *kong.Context) bool {
	if ctx == nil {
		return false
	}

	command := strings.TrimSpace(ctx.Command())
	switch command {
	case "auth check", "config init", "config validate", "image resolve", "version":
		return true
	default:
		return strings.HasPrefix(command, "image resolve ")
	}
}

func commandUsesStartupObservability(ctx *kong.Context) bool {
	if ctx == nil {
		return true
	}
	return !strings.HasPrefix(strings.TrimSpace(ctx.Command()), "daemon ")
}

func configureBackendRuntimeConfig(backends map[string]backend.Adapter, cfg runtimeconfig.Config, version string) {
	if darwinAdapter, ok := backends["darwin-vz"].(*darwinvz.Adapter); ok {
		darwinAdapter.ConfiguredNetworkMode = strings.TrimSpace(cfg.Backends.DarwinVZ.Network.Mode)
		darwinAdapter.Version = strings.TrimSpace(version)
	}
}

func configureBackendObservability(backends map[string]backend.Adapter, obsRuntime *observability.Runtime) {
	if obsRuntime == nil {
		return
	}
	if firecrackerAdapter, ok := backends["firecracker"].(*firecracker.Adapter); ok {
		firecrackerAdapter.MeterProvider = obsRuntime.MeterProvider()
	}
	if darwinAdapter, ok := backends["darwin-vz"].(*darwinvz.Adapter); ok {
		darwinAdapter.MeterProvider = obsRuntime.MeterProvider()
	}
}

func ExitCode(err error) int {
	var codeErr hasExitCode
	if errors.As(err, &codeErr) {
		return codeErr.ExitCode()
	}
	return 1
}

func resolveCWD(base, chdir string) (string, error) {
	if chdir == "" {
		return base, nil
	}
	if filepath.IsAbs(chdir) {
		return filepath.Clean(chdir), nil
	}
	return filepath.Join(base, chdir), nil
}

func (ctx *runtimeContext) stderr() *os.File {
	if ctx != nil && ctx.Stderr != nil {
		return ctx.Stderr
	}
	return os.Stderr
}

func reportObservabilityShutdown(runErr *error, stderr io.Writer, shutdownErr error) {
	if shutdownErr == nil {
		return
	}

	wrapped := fmt.Errorf("shutdown observability: %w", shutdownErr)
	if runErr != nil && *runErr != nil {
		*runErr = errors.Join(*runErr, wrapped)
		return
	}

	if stderr != nil {
		_ = writeExecutionWarning(stderr, wrapped.Error())
	}
}

func newObservabilityErrorReporter(stderr io.Writer) func(error) {
	var mu sync.Mutex
	return func(err error) {
		if err == nil || stderr == nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		_ = writeExecutionWarning(stderr, err.Error())
	}
}

func runtimeServiceName(ctx *kong.Context) string {
	if ctx == nil {
		return "cleanroom-cli"
	}
	if strings.HasPrefix(strings.TrimSpace(ctx.Command()), "serve") {
		return "cleanroom-server"
	}
	return "cleanroom-cli"
}
