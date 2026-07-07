package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/alecthomas/kong"
	"github.com/buildkite/cleanroom/internal/policy"
)

// policyLoader loads and compiles repository policy. The bake-era surface
// needs only compilation; there is no runtime/control-plane loader.
type policyLoader interface {
	LoadAndCompile(cwd string) (*policy.CompiledPolicy, string, error)
}

type runtimeContext struct {
	CWD     string
	Stdout  *os.File
	Stderr  *os.File
	Stdin   *os.File
	Loader  policyLoader
	Version string
}

// CLI is the cleanroom command surface: a thin bake-and-mediate layer over
// spore. It owns policy compilation, provenance, and credential mediation;
// spore owns all VM lifecycle.
type CLI struct {
	Policy  PolicyCommand  `cmd:"" help:"Validate repository policy"`
	Compile CompileCommand `cmd:"" help:"Compile repo policy into spore create arguments"`
	Stamp   StampCommand   `cmd:"" help:"Emit provenance annotations as spore create arguments"`
	Bake    BakeCommand    `cmd:"" help:"Bake repo policy into a warm spore (consume with spore run --from, fork, fanout)"`
	Verify  VerifyCommand  `cmd:"" help:"Verify cleanroom provenance of a spore and report required bindings"`
	Run     RunCommand     `cmd:"" help:"Run a baked spore, starting the gateway when provenance requires it"`
	Content ContentCommand `cmd:"" name:"content-cache" help:"Run host-side content-cache services for cleanroom gateways"`
	Gateway GatewayCommand `cmd:"" help:"Lineage gateway: mediate credentialed upstream access for baked spores"`
	Version VersionCommand `cmd:"" help:"Print version information"`
}

type VersionCommand struct {
	version string
}

func (c *VersionCommand) Run() error {
	fmt.Println("cleanroom version " + c.version)
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

// Run parses and dispatches the CLI. Every command is a local, stateless
// operation over policy files and the spore CLI, so there is no runtime
// config, backend, or observability startup.
func Run(args []string, version string) error {
	runtimeCtx := &runtimeContext{
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Stdin:   os.Stdin,
		Loader:  policy.Loader{},
		Version: version,
	}

	cli := CLI{}
	cli.Version.version = version
	parser, err := kong.New(
		&cli,
		kong.Name("cleanroom"),
		kong.Description("Bake repository policy into warm spores and mediate their credentials."),
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

func resolveCWD(base, chdir string) (string, error) {
	if chdir == "" {
		return base, nil
	}
	if filepath.IsAbs(chdir) {
		return filepath.Clean(chdir), nil
	}
	return filepath.Clean(filepath.Join(base, chdir)), nil
}

func (ctx *runtimeContext) stderr() *os.File {
	if ctx != nil && ctx.Stderr != nil {
		return ctx.Stderr
	}
	return os.Stderr
}

func (ctx *runtimeContext) stdin() *os.File {
	if ctx != nil && ctx.Stdin != nil {
		return ctx.Stdin
	}
	return os.Stdin
}
