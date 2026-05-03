package firecracker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
)

type privilegedCommandRunner interface {
	Run(ctx context.Context, args ...string) error
	Output(ctx context.Context, args ...string) ([]byte, error)
	RunBatch(ctx context.Context, commands [][]string) error
}

type directPrivilegedCommandRunner struct{}

type helperPrivilegedCommandRunner struct {
	cfg backend.FirecrackerConfig
}

var privilegedCommandEUID = os.Geteuid

var directPrivilegedCommandPathResolver = resolveDirectPrivilegedCommandPath

func privilegedCommandsRunDirectly() bool {
	return privilegedCommandEUID() == 0
}

func newPrivilegedCommandRunner(cfg backend.FirecrackerConfig) privilegedCommandRunner {
	if privilegedCommandsRunDirectly() {
		return directPrivilegedCommandRunner{}
	}
	return helperPrivilegedCommandRunner{cfg: cfg}
}

func newHelperPrivilegedCommandRunner(cfg backend.FirecrackerConfig) privilegedCommandRunner {
	return helperPrivilegedCommandRunner{cfg: cfg}
}

func resolveTrustedCommandPath(command string, candidates ...string) (string, error) {
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%q not found in trusted locations", command)
}

func resolveDirectPrivilegedCommandPath(command string) (string, error) {
	switch strings.TrimSpace(command) {
	case "true":
		return resolveTrustedCommandPath(command, "/usr/bin/true", "/bin/true")
	case "dd":
		return resolveTrustedCommandPath(command, "/usr/bin/dd", "/bin/dd")
	case "ip":
		return resolveTrustedCommandPath(command, "/usr/sbin/ip", "/sbin/ip")
	case "iptables":
		return resolveTrustedCommandPath(command, "/usr/sbin/iptables", "/sbin/iptables")
	case "sysctl":
		return resolveTrustedCommandPath(command, "/usr/sbin/sysctl", "/sbin/sysctl")
	case "zfs":
		return resolveTrustedCommandPath(command, "/usr/sbin/zfs", "/sbin/zfs")
	default:
		return "", fmt.Errorf("unsupported direct privileged command %q", command)
	}
}

func (directPrivilegedCommandRunner) Run(ctx context.Context, args ...string) error {
	if len(args) == 0 {
		return errors.New("missing privileged command")
	}
	binary, err := directPrivilegedCommandPathResolver(args[0])
	if err != nil {
		return err
	}
	return runCombinedCommand(ctx, append([]string{binary}, args[1:]...), args)
}

func (directPrivilegedCommandRunner) Output(ctx context.Context, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, errors.New("missing privileged command")
	}
	binary, err := directPrivilegedCommandPathResolver(args[0])
	if err != nil {
		return nil, err
	}
	return runCombinedCommandOutput(ctx, append([]string{binary}, args[1:]...), args)
}

func (directPrivilegedCommandRunner) OutputTo(ctx context.Context, dst io.Writer, args ...string) error {
	if len(args) == 0 {
		return errors.New("missing privileged command")
	}
	if dst == nil {
		return errors.New("missing privileged command output writer")
	}
	binary, err := directPrivilegedCommandPathResolver(args[0])
	if err != nil {
		return err
	}
	return runCommandWithIO(ctx, append([]string{binary}, args[1:]...), args, nil, dst)
}

func (directPrivilegedCommandRunner) InputFrom(ctx context.Context, src io.Reader, args ...string) error {
	if len(args) == 0 {
		return errors.New("missing privileged command")
	}
	if src == nil {
		return errors.New("missing privileged command input reader")
	}
	binary, err := directPrivilegedCommandPathResolver(args[0])
	if err != nil {
		return err
	}
	return runCommandWithIO(ctx, append([]string{binary}, args[1:]...), args, src, nil)
}

func (r directPrivilegedCommandRunner) RunBatch(ctx context.Context, commands [][]string) error {
	for _, args := range commands {
		if len(args) == 0 {
			continue
		}
		if err := r.Run(ctx, args...); err != nil {
			return err
		}
	}
	return nil
}

func (r helperPrivilegedCommandRunner) Run(ctx context.Context, args ...string) error {
	if len(args) == 0 {
		return errors.New("missing privileged command")
	}

	helperPath := resolvePrivilegedHelperPath(r.cfg)
	if strings.TrimSpace(helperPath) == "" {
		return errors.New("missing privileged helper path")
	}
	return runCombinedCommand(ctx, append([]string{"sudo", "-n", helperPath}, args...), append([]string{"helper"}, args...))
}

func (r helperPrivilegedCommandRunner) Output(ctx context.Context, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, errors.New("missing privileged command")
	}

	helperPath := resolvePrivilegedHelperPath(r.cfg)
	if strings.TrimSpace(helperPath) == "" {
		return nil, errors.New("missing privileged helper path")
	}
	return runCombinedCommandOutput(ctx, append([]string{"sudo", "-n", helperPath}, args...), append([]string{"helper"}, args...))
}

func (r helperPrivilegedCommandRunner) OutputTo(ctx context.Context, dst io.Writer, args ...string) error {
	if len(args) == 0 {
		return errors.New("missing privileged command")
	}
	if dst == nil {
		return errors.New("missing privileged command output writer")
	}

	helperPath := resolvePrivilegedHelperPath(r.cfg)
	if strings.TrimSpace(helperPath) == "" {
		return errors.New("missing privileged helper path")
	}
	return runCommandWithIO(ctx, append([]string{"sudo", "-n", helperPath}, args...), append([]string{"helper"}, args...), nil, dst)
}

func (r helperPrivilegedCommandRunner) InputFrom(ctx context.Context, src io.Reader, args ...string) error {
	if len(args) == 0 {
		return errors.New("missing privileged command")
	}
	if src == nil {
		return errors.New("missing privileged command input reader")
	}

	helperPath := resolvePrivilegedHelperPath(r.cfg)
	if strings.TrimSpace(helperPath) == "" {
		return errors.New("missing privileged helper path")
	}
	return runCommandWithIO(ctx, append([]string{"sudo", "-n", helperPath}, args...), append([]string{"helper"}, args...), src, nil)
}

func (r helperPrivilegedCommandRunner) RunBatch(ctx context.Context, commands [][]string) error {
	for _, args := range commands {
		if len(args) == 0 {
			continue
		}
		if err := r.Run(ctx, args...); err != nil {
			return err
		}
	}
	return nil
}

func runRootCommand(ctx context.Context, cfg backend.FirecrackerConfig, args ...string) error {
	return newPrivilegedCommandRunner(cfg).Run(ctx, args...)
}

func runRootCommandOutput(ctx context.Context, cfg backend.FirecrackerConfig, args ...string) ([]byte, error) {
	return newPrivilegedCommandRunner(cfg).Output(ctx, args...)
}

func runRootCommandBatch(ctx context.Context, cfg backend.FirecrackerConfig, commands [][]string) error {
	return newPrivilegedCommandRunner(cfg).RunBatch(ctx, commands)
}

func runCommandWithIO(ctx context.Context, command []string, errorContext []string, stdin io.Reader, stdout io.Writer) error {
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = "no stderr output"
		}
		return fmt.Errorf("%s: %w (%s)", strings.Join(errorContext, " "), err, msg)
	}
	return nil
}
