package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/sporevm"
)

type VMCreateCommand struct {
	Name          string `arg:"" required:"" help:"Named VM lifecycle target"`
	Dir           string `arg:"" optional:"" default:"." help:"Repository directory containing cleanroom policy"`
	Backend       string `help:"SporeVM backend"`
	Wait          bool   `help:"Accepted for CLI compatibility; libspore create waits for readiness"`
	LaunchSeconds int64  `help:"VM boot/guest-agent readiness timeout in seconds"`
	JSON          bool   `help:"Print libspore response as JSON"`
}

type VMExecCommand struct {
	Name    string   `arg:"" required:"" help:"Named VM lifecycle target"`
	JSON    bool     `help:"Print libspore response as JSON"`
	Command []string `arg:"" passthrough:"partial" required:"" help:"Command to execute"`
}

type VMCaptureCommand struct {
	Name string `arg:"" required:"" help:"Named VM lifecycle target"`
	Out  string `name:"out" required:"" help:"Output spore bundle directory"`
	JSON bool   `help:"Print libspore response as JSON"`
}

type VMResumeCommand struct {
	SporeDir string `arg:"" name:"spore-dir" required:"" help:"Spore bundle directory"`
	Name     string `name:"name" required:"" help:"Named VM lifecycle target"`
	JSON     bool   `help:"Print libspore response as JSON"`
}

type VMDestroyCommand struct {
	Name string `arg:"" required:"" help:"Named VM lifecycle target"`
	JSON bool   `help:"Print libspore response as JSON"`
}

func (c *VMCreateCommand) Run(ctx *runtimeContext) error {
	cwd, err := resolveCWD(ctx.CWD, c.Dir)
	if err != nil {
		return err
	}
	compiled, _, err := ctx.Loader.LoadAndCompile(cwd)
	if err != nil {
		return err
	}
	if err := validateVMPolicy(compiled); err != nil {
		return err
	}
	networkRules, err := vmNetworkRules(compiled)
	if err != nil {
		return err
	}
	client, err := newSporeVMClient()
	if err != nil {
		return err
	}
	defer client.Close()

	runCtx := context.Background()
	if len(networkRules) > 0 {
		caps, err := client.NetworkCapabilities(runCtx)
		if err != nil {
			return fmt.Errorf("read libspore network capabilities: %w", err)
		}
		if !caps.Supported || !caps.ExactHostPort {
			return errors.New("libspore does not support exact host-plus-port network rules")
		}
	}

	result, err := client.CreateNamed(runCtx, sporevm.CreateNamedOptions{
		Name:           c.Name,
		Backend:        strings.TrimSpace(c.Backend),
		ImageRef:       strings.TrimSpace(compiled.ImageRef),
		MemoryBytes:    vmMemoryBytes(compiled),
		VCPUs:          vmVCPUs(compiled),
		TimeoutMS:      launchTimeoutMS(c.LaunchSeconds),
		NetworkEnabled: len(networkRules) > 0,
		NetworkRules:   networkRules,
	})
	if err != nil {
		return err
	}
	return writeVMResult(ctx, result, c.JSON, fmt.Sprintf("created vm %s", c.Name))
}

func (c *VMExecCommand) Run(ctx *runtimeContext) error {
	command := executionCommandArgs(c.Command)
	if len(command) == 0 {
		return errors.New("missing command")
	}
	client, err := newSporeVMClient()
	if err != nil {
		return err
	}
	defer client.Close()

	result, err := client.ExecNamed(context.Background(), sporevm.ExecNamedOptions{
		Name: c.Name,
		Argv: command,
	})
	if err != nil {
		return err
	}
	return writeVMExecResult(ctx, result, c.JSON)
}

func (c *VMCaptureCommand) Run(ctx *runtimeContext) error {
	client, err := newSporeVMClient()
	if err != nil {
		return err
	}
	defer client.Close()

	result, err := client.SnapshotNamed(context.Background(), sporevm.SnapshotNamedOptions{
		Name:     c.Name,
		OutDir:   strings.TrimSpace(c.Out),
		Continue: true,
	})
	if err != nil {
		return err
	}
	return writeVMResult(ctx, result, c.JSON, fmt.Sprintf("captured vm %s to %s", c.Name, c.Out))
}

func (c *VMResumeCommand) Run(ctx *runtimeContext) error {
	client, err := newSporeVMClient()
	if err != nil {
		return err
	}
	defer client.Close()

	result, err := client.ResumeNamed(context.Background(), sporevm.ResumeNamedOptions{
		SporeDir: strings.TrimSpace(c.SporeDir),
		Name:     c.Name,
	})
	if err != nil {
		return err
	}
	return writeVMResult(ctx, result, c.JSON, fmt.Sprintf("resumed vm %s from %s", c.Name, c.SporeDir))
}

func (c *VMDestroyCommand) Run(ctx *runtimeContext) error {
	client, err := newSporeVMClient()
	if err != nil {
		return err
	}
	defer client.Close()

	result, err := client.RemoveNamed(context.Background(), sporevm.RemoveNamedOptions{Name: c.Name})
	if err != nil {
		return err
	}
	return writeVMResult(ctx, result, c.JSON, fmt.Sprintf("destroyed vm %s", c.Name))
}

func newSporeVMClient() (sporevm.Client, error) {
	client, err := sporevm.New()
	if err != nil {
		return nil, fmt.Errorf("connect libspore: %w", err)
	}
	return client, nil
}

func validateVMPolicy(compiled *policy.CompiledPolicy) error {
	if compiled == nil {
		return errors.New("missing compiled policy")
	}
	if strings.TrimSpace(compiled.ImageRef) == "" {
		return errors.New("cleanroom create requires sandbox.image.ref")
	}
	if compiled.HasStageScopedNetwork() {
		return errors.New("cleanroom create does not yet translate stage-scoped network policy to libspore")
	}
	if _, err := vmNetworkRules(compiled); err != nil {
		return err
	}
	if compiled.RequiresDockerService() {
		return errors.New("cleanroom create does not yet translate docker service policy to libspore")
	}
	if len(compiled.Services.Blocks) > 0 || len(compiled.Services.Command) > 0 {
		return errors.New("cleanroom create does not yet translate service stages to libspore")
	}
	if len(compiled.Dependencies.Blocks) > 0 || len(compiled.Dependencies.Command) > 0 {
		return errors.New("cleanroom create does not yet translate dependency stages to libspore")
	}
	if len(compiled.Run.Before) > 0 {
		return errors.New("cleanroom create does not yet translate run.before hooks to libspore")
	}
	return nil
}

func vmNetworkRules(compiled *policy.CompiledPolicy) ([]sporevm.NetworkRule, error) {
	if compiled == nil || len(compiled.Allow) == 0 {
		return nil, nil
	}
	rules := make([]sporevm.NetworkRule, 0, len(compiled.Allow))
	for _, rule := range compiled.Allow {
		host := strings.TrimSpace(rule.Host)
		if host == "" {
			return nil, errors.New("cleanroom create requires network allow rules to include a host")
		}
		if len(rule.Ports) == 0 {
			return nil, fmt.Errorf("cleanroom create requires network allow rule for %s to include at least one port", host)
		}
		ports := make([]uint16, 0, len(rule.Ports))
		for _, port := range rule.Ports {
			if port < 1 || port > 65535 {
				return nil, fmt.Errorf("cleanroom create does not support network allow port %d for %s", port, host)
			}
			ports = append(ports, uint16(port))
		}
		rules = append(rules, sporevm.NetworkRule{
			Host:  host,
			Ports: ports,
		})
	}
	return rules, nil
}

func vmMemoryBytes(compiled *policy.CompiledPolicy) uint64 {
	if compiled == nil || compiled.Resources == nil || compiled.Resources.MemoryBytes <= 0 {
		return 0
	}
	return uint64(compiled.Resources.MemoryBytes)
}

func vmVCPUs(compiled *policy.CompiledPolicy) uint32 {
	if compiled == nil || compiled.Resources == nil || compiled.Resources.VCPUs <= 0 {
		return 0
	}
	return uint32(compiled.Resources.VCPUs)
}

func launchTimeoutMS(seconds int64) uint64 {
	if seconds <= 0 {
		return 0
	}
	return uint64(seconds) * 1000
}

func writeVMResult(ctx *runtimeContext, result sporevm.JSONResult, jsonOutput bool, message string) error {
	if jsonOutput || len(result.RawJSON) > 0 && message == "" {
		if len(result.RawJSON) == 0 {
			_, err := fmt.Fprintln(ctx.Stdout, "{}")
			return err
		}
		_, err := ctx.Stdout.Write(result.RawJSON)
		if err != nil {
			return err
		}
		if len(result.RawJSON) > 0 && result.RawJSON[len(result.RawJSON)-1] != '\n' {
			_, err = fmt.Fprintln(ctx.Stdout)
		}
		return err
	}
	_, err := fmt.Fprintln(ctx.Stdout, message)
	return err
}

type vmExecResult struct {
	ExitCode        int         `json:"exit_code"`
	Stdout          vmExecBytes `json:"stdout"`
	Stderr          vmExecBytes `json:"stderr"`
	StdoutTruncated bool        `json:"stdout_truncated"`
	StderrTruncated bool        `json:"stderr_truncated"`
}

type vmExecBytes []byte

func (b *vmExecBytes) UnmarshalJSON(raw []byte) error {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		*b = []byte(s)
		return nil
	}
	var bytes []byte
	if err := json.Unmarshal(raw, &bytes); err == nil {
		*b = bytes
		return nil
	}
	return errors.New("expected string or byte array")
}

func writeVMExecResult(ctx *runtimeContext, result sporevm.JSONResult, jsonOutput bool) error {
	var execResult vmExecResult
	if err := json.Unmarshal(result.RawJSON, &execResult); err != nil {
		if jsonOutput {
			return errors.Join(writeVMResult(ctx, result, true, ""), fmt.Errorf("decode libspore exec result: %w", err))
		}
		return fmt.Errorf("decode libspore exec result: %w", err)
	}
	if execResult.ExitCode < 0 || execResult.ExitCode > 255 {
		return fmt.Errorf("decode libspore exec result: invalid exit code %d", execResult.ExitCode)
	}

	if jsonOutput {
		if err := writeVMResult(ctx, result, true, ""); err != nil {
			return err
		}
	} else {
		if len(execResult.Stdout) > 0 {
			if _, err := ctx.Stdout.Write(execResult.Stdout); err != nil {
				return err
			}
		}
		stderr := ctx.stderr()
		if len(execResult.Stderr) > 0 {
			if _, err := stderr.Write(execResult.Stderr); err != nil {
				return err
			}
		}
		if execResult.StdoutTruncated {
			if _, err := fmt.Fprintln(stderr, "cleanroom exec: stdout truncated by libspore"); err != nil {
				return err
			}
		}
		if execResult.StderrTruncated {
			if _, err := fmt.Fprintln(stderr, "cleanroom exec: stderr truncated by libspore"); err != nil {
				return err
			}
		}
	}

	if execResult.ExitCode != 0 {
		return exitCodeError{code: execResult.ExitCode}
	}
	return nil
}
