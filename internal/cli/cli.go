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
	systemdServiceName       = "cleanroom.service"
	launchdServiceName       = "com.buildkite.cleanroom"
	defaultDaemonListen      = "unix://" + endpoint.DefaultSystemSocketPath
	sandboxTerminateTimeout  = 2 * time.Second
	interruptForceExitWindow = 1200 * time.Millisecond
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
	Policy   PolicyCommand   `cmd:"" help:"Policy commands"`
	Config   ConfigCommand   `cmd:"" help:"Runtime config commands"`
	Image    ImageCommand    `cmd:"" help:"Manage OCI image cache artifacts"`
	Snapshot SnapshotCommand `cmd:"" help:"Manage snapshots"`
	Create   CreateCommand   `cmd:"" help:"Create a sandbox"`
	Exec     ExecCommand     `cmd:"" help:"Execute a command in a cleanroom backend"`
	Console  ConsoleCommand  `cmd:"" help:"Attach an interactive console to a cleanroom execution"`
	Serve    ServeCommand    `cmd:"" help:"Run the cleanroom control-plane server in the foreground"`
	Daemon   DaemonCommand   `cmd:"" help:"Manage daemon/service lifecycle"`
	Doctor   DoctorCommand   `cmd:"" help:"Run environment and backend diagnostics"`
	Status   StatusCommand   `cmd:"" help:"Inspect run artifacts"`
	Sandbox  SandboxCommand  `cmd:"" help:"Manage sandboxes"`
	Version  VersionCommand  `cmd:"" help:"Print version information"`
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
	if err := validatePassthroughFlagSyntax(args); err != nil {
		return err
	}

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

type passthroughFlagSpec struct {
	takesValue bool
}

var (
	passthroughCommandFlagSpecs = map[string]passthroughFlagSpec{
		"-h":                 {},
		"--help":             {},
		"--host":             {takesValue: true},
		"--log-level":        {takesValue: true},
		"--tls-ca":           {takesValue: true},
		"--tlsca":            {takesValue: true},
		"-c":                 {takesValue: true},
		"--chdir":            {takesValue: true},
		"--backend":          {takesValue: true},
		"--in":               {takesValue: true},
		"--sandbox-id":       {takesValue: true},
		"--from":             {takesValue: true},
		"--image":            {takesValue: true},
		"--keep":             {},
		"--print-sandbox-id": {},
		"--launch-seconds":   {takesValue: true},
	}
)

func validatePassthroughFlagSyntax(args []string) error {
	if len(args) == 0 {
		return nil
	}

	switch args[0] {
	case "exec":
		return validatePassthroughSubcommandArgs(args[1:], passthroughCommandFlagSpecs)
	case "console":
		return validatePassthroughSubcommandArgs(args[1:], passthroughCommandFlagSpecs)
	default:
		return nil
	}
}

func validatePassthroughSubcommandArgs(args []string, specs map[string]passthroughFlagSpec) error {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return nil
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			return nil
		}

		name, inlineValue := splitPassthroughFlagToken(arg)
		spec, ok := specs[name]
		if !ok {
			return fmt.Errorf("unknown flag %s", arg)
		}
		if inlineValue || !spec.takesValue {
			continue
		}
		if i+1 >= len(args) {
			return nil
		}
		i++
	}
	return nil
}

func splitPassthroughFlagToken(arg string) (string, bool) {
	if idx := strings.IndexRune(arg, '='); idx >= 0 {
		return arg[:idx], true
	}
	if strings.HasPrefix(arg, "-c") && !strings.HasPrefix(arg, "--") && len(arg) > 2 {
		return "-c", true
	}
	return arg, false
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
