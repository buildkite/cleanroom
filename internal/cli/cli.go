package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/alecthomas/kong"
	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/backend/firecracker"
	"github.com/buildkite/cleanroom/internal/policy"
)

type runtimeContext struct {
	CWD      string
	Stdout   *os.File
	Loader   policy.Loader
	Backends map[string]backend.Adapter
}

type CLI struct {
	Policy PolicyCommand `cmd:"" help:"Policy commands"`
	Exec   ExecCommand   `cmd:"" help:"Execute a command in a cleanroom backend"`
	Status StatusCommand `cmd:"" help:"Inspect run artifacts"`
}

type PolicyCommand struct {
	Validate PolicyValidateCommand `cmd:"" help:"Validate policy configuration"`
}

type PolicyValidateCommand struct {
	JSON bool `help:"Print compiled policy as JSON"`
}

type ExecCommand struct {
	Backend string `help:"Execution backend" default:"firecracker" enum:"firecracker"`

	FirecrackerBinary string `help:"Path to firecracker binary" default:"firecracker"`
	KernelImage       string `help:"Path to Firecracker kernel image"`
	RootFS            string `help:"Path to Firecracker root filesystem image"`
	RunDir            string `help:"Run directory for generated artifacts (default: /tmp/cleanroom/<run-id>)"`
	VCPUs             int64  `help:"Number of virtual CPUs" default:"1"`
	MemoryMiB         int64  `help:"VM memory in MiB" default:"512"`
	RetainWrites      bool   `help:"Retain rootfs writes from launched runs in the run directory (default: discard)"`
	Launch            bool   `help:"Launch Firecracker for a short MVP window; otherwise generate plan only"`
	LaunchSeconds     int64  `help:"Maximum launch window in seconds before stopping VM" default:"10"`

	Command []string `arg:"" passthrough:"" required:"" help:"Command to execute"`
}

type StatusCommand struct {
	RunID string `help:"Run ID to inspect"`
}

func Run(args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	runtimeCtx := &runtimeContext{
		CWD:    cwd,
		Stdout: os.Stdout,
		Loader: policy.Loader{},
		Backends: map[string]backend.Adapter{
			"firecracker": firecracker.New(),
		},
	}

	cli := CLI{}
	parser, err := kong.New(
		&cli,
		kong.Name("cleanroom"),
		kong.Description("Cleanroom CLI (MVP)"),
	)
	if err != nil {
		return err
	}

	ctx, err := parser.Parse(args)
	if err != nil {
		return err
	}

	return ctx.Run(runtimeCtx)
}

func (c *PolicyValidateCommand) Run(ctx *runtimeContext) error {
	compiled, source, err := ctx.Loader.LoadAndCompile(ctx.CWD)
	if err != nil {
		return err
	}

	if c.JSON {
		payload := map[string]any{
			"source": source,
			"policy": compiled,
		}
		enc := json.NewEncoder(ctx.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(payload)
	}

	_, err = fmt.Fprintf(ctx.Stdout, "policy valid: %s\npolicy hash: %s\n", source, compiled.Hash)
	return err
}

func (e *ExecCommand) Run(ctx *runtimeContext) error {
	adapter, ok := ctx.Backends[e.Backend]
	if !ok {
		return fmt.Errorf("unknown backend %q", e.Backend)
	}

	compiled, source, err := ctx.Loader.LoadAndCompile(ctx.CWD)
	if err != nil {
		return err
	}

	runID := fmt.Sprintf("run-%d", time.Now().UTC().UnixNano())
	req := backend.RunRequest{
		RunID:   runID,
		CWD:     ctx.CWD,
		Command: append([]string(nil), e.Command...),
		Policy:  compiled,
		FirecrackerConfig: backend.FirecrackerConfig{
			BinaryPath:      e.FirecrackerBinary,
			KernelImagePath: e.KernelImage,
			RootFSPath:      e.RootFS,
			RunDir:          e.RunDir,
			VCPUs:           e.VCPUs,
			MemoryMiB:       e.MemoryMiB,
			RetainWrites:    e.RetainWrites,
			Launch:          e.Launch,
			LaunchSeconds:   e.LaunchSeconds,
		},
	}

	result, err := adapter.Run(context.Background(), req)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(
		ctx.Stdout,
		"run id: %s\npolicy source: %s\npolicy hash: %s\nplan: %s\nrun dir: %s\nmessage: %s\n",
		result.RunID,
		source,
		compiled.Hash,
		result.PlanPath,
		result.RunDir,
		result.Message,
	)
	return err
}

func (s *StatusCommand) Run(ctx *runtimeContext) error {
	baseDir := filepath.Join(os.TempDir(), "cleanroom")
	if s.RunID != "" {
		runDir := filepath.Join(baseDir, s.RunID)
		if _, err := os.Stat(runDir); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("run %q not found in %s", s.RunID, baseDir)
			}
			return err
		}
		_, err := fmt.Fprintf(ctx.Stdout, "run: %s\n", runDir)
		return err
	}

	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			_, werr := fmt.Fprintf(ctx.Stdout, "no runs found (%s does not exist)\n", baseDir)
			return werr
		}
		return err
	}

	if len(entries) == 0 {
		_, err := fmt.Fprintf(ctx.Stdout, "no runs found in %s\n", baseDir)
		return err
	}

	_, err = fmt.Fprintf(ctx.Stdout, "runs in %s:\n", baseDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := fmt.Fprintf(ctx.Stdout, "- %s\n", entry.Name()); err != nil {
			return err
		}
	}
	return nil
}
