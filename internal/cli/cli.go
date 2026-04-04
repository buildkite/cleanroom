package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alecthomas/kong"
	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/backend/darwinvz"
	"github.com/buildkite/cleanroom/internal/backend/firecracker"
	"github.com/buildkite/cleanroom/internal/controlclient"
	"github.com/buildkite/cleanroom/internal/endpoint"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"github.com/buildkite/cleanroom/internal/tlsconfig"
)

const defaultBumpRefSource = "ghcr.io/buildkite/cleanroom-base/alpine:latest"

const (
	systemdServiceName        = "cleanroom.service"
	launchdSystemServiceName  = "com.buildkite.cleanroom"
	launchdUserServiceName    = "com.buildkite.cleanroom.user"
	launchdNetworkServiceName = "com.buildkite.cleanroom.network"
	launchdServiceName        = launchdSystemServiceName
	defaultDaemonListen       = "unix://" + endpoint.DefaultSystemSocketPath
	sandboxTerminateTimeout   = 2 * time.Second
	interruptForceExitWindow  = 1200 * time.Millisecond
)

type policyLoader interface {
	LoadAndCompile(cwd string) (*policy.CompiledPolicy, string, error)
	LoadRepository(cwd string) (policy.RepositoryConfig, string, error)
}

type runtimeContext struct {
	CWD        string
	Stdout     *os.File
	Loader     policyLoader
	Config     runtimeconfig.Config
	ConfigPath string
	Backends   map[string]backend.Adapter
}

type CLI struct {
	Policy             PolicyCommand             `cmd:"" help:"Policy commands"`
	Config             ConfigCommand             `cmd:"" help:"Runtime config commands"`
	Image              ImageCommand              `cmd:"" help:"Manage OCI image cache artifacts"`
	Inspect            InspectCommand            `cmd:"" help:"Inspect a sandbox, execution, or snapshot by ID"`
	Snapshot           SnapshotCommand           `cmd:"" help:"Manage snapshots"`
	Create             CreateCommand             `cmd:"" help:"Create a sandbox using repo policy"`
	Exec               ExecCommand               `cmd:"" help:"Execute a command in a sandbox"`
	Console            ConsoleCommand            `cmd:"" help:"Open an interactive console in a sandbox"`
	Serve              ServeCommand              `cmd:"" help:"Run the cleanroom control-plane server in the foreground"`
	Daemon             DaemonCommand             `cmd:"" help:"Manage daemon/service lifecycle"`
	Network            NetworkCommand            `cmd:"" help:"Manage macOS network-filter support"`
	ServeNetworkFilter ServeNetworkFilterCommand `cmd:"serve-network-filter" hidden:"" help:"Run the macOS network-filter state daemon"`
	Doctor             DoctorCommand             `cmd:"" help:"Run environment and backend diagnostics"`
	Execution          ExecutionCommand          `cmd:"" help:"Inspect command executions and diagnostics"`
	Status             StatusCommand             `cmd:"" help:"Browse retained execution artifacts"`
	Sandbox            SandboxCommand            `cmd:"" help:"Manage sandboxes"`
	Version            VersionCommand            `cmd:"" help:"Print version information"`
}

type VersionCommand struct {
	version string
}

func (c *VersionCommand) Run() error {
	fmt.Println("cleanroom version " + c.version)
	return nil
}

type clientFlags struct {
	Host     string `help:"Control-plane endpoint (unix://path, http://host:port, or https://host:port)" env:"CLEANROOM_HOST"`
	LogLevel string `help:"Client log level (debug|info|warn|error)"`
	TLSCA    string `name:"tls-ca" aliases:"tlsca" help:"Path to CA certificate for server verification (auto-discovered from XDG config for https)" env:"CLEANROOM_TLS_CA"`
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
	return controlclient.New(ep, controlclient.WithTLS(tlsconfig.Options{
		CAPath: f.TLSCA,
	}))
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

func Run(args []string, version string) error {
	cfg, cfgPath, err := runtimeconfig.Load()
	if err != nil {
		return err
	}

	runtimeCtx := &runtimeContext{
		Stdout:     os.Stdout,
		Loader:     policy.Loader{},
		Config:     cfg,
		ConfigPath: cfgPath,
		Backends: map[string]backend.Adapter{
			"firecracker": firecracker.New(),
			"darwin-vz":   darwinvz.New(),
		},
	}

	cli := CLI{}
	cli.Version.version = version
	cli.Doctor.version = version
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

	return ctx.Run(runtimeCtx)
}

func ExitCode(err error) int {
	var codeErr hasExitCode
	if errors.As(err, &codeErr) {
		return codeErr.ExitCode()
	}
	return 1
}

func normalizeVersion(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "dev"
	}
	return trimmed
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
