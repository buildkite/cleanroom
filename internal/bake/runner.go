package bake

import (
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// minSporeVersion is the first spore release carrying create-time
// annotations, exact host-plus-port rules, and suspend annotations.
const minSporeVersion = "0.3.1"

// Runner runs spore lifecycle commands for the bake pipeline.
type Runner interface {
	// Version returns the spore CLI version string, e.g. "0.3.1".
	Version() (string, error)
	// Create starts a named builder VM with raw spore create arguments.
	Create(name string, args []string) error
	// CopyIn copies a host path into the named VM.
	CopyIn(name, hostPath, guestPath string) error
	// ExecShell runs a shell command in the named VM, streaming output.
	ExecShell(name, command string) error
	// Suspend captures the named VM to outDir and stops it.
	Suspend(name, outDir string) error
	// InspectAnnotations reads the annotations of a spore directory.
	InspectAnnotations(sporeDir string) (map[string]string, error)
	// Remove destroys a named VM.
	Remove(name string) error
}

// CLIRunner runs a spore executable as subprocesses. Stdout/Stderr receive
// streamed guest and spore output.
type CLIRunner struct {
	Spore  string
	Stdout io.Writer
	Stderr io.Writer
}

func (r *CLIRunner) Version() (string, error) {
	out, err := exec.Command(r.Spore, "version").Output()
	if err != nil {
		return "", fmt.Errorf("run %s version: %w", r.Spore, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 || fields[0] != "spore" {
		return "", fmt.Errorf("unexpected spore version output %q", strings.TrimSpace(string(out)))
	}
	return strings.TrimPrefix(fields[1], "v"), nil
}

func (r *CLIRunner) Create(name string, args []string) error {
	return r.run(append([]string{"create", name}, args...))
}

func (r *CLIRunner) CopyIn(name, hostPath, guestPath string) error {
	return r.run([]string{"copy-in", name, hostPath, guestPath})
}

func (r *CLIRunner) ExecShell(name, command string) error {
	return r.run([]string{"exec", name, command})
}

func (r *CLIRunner) Suspend(name, outDir string) error {
	return r.run([]string{"suspend", name, "--out", outDir})
}

func (r *CLIRunner) Remove(name string) error {
	return r.run([]string{"rm", name})
}

func (r *CLIRunner) InspectAnnotations(sporeDir string) (map[string]string, error) {
	cmd := exec.Command(r.Spore, "--json", "inspect", sporeDir)
	cmd.Stderr = r.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("spore inspect %s: %w", sporeDir, err)
	}
	var result struct {
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("decode spore inspect output: %w", err)
	}
	return result.Annotations, nil
}

func (r *CLIRunner) run(args []string) error {
	cmd := exec.Command(r.Spore, args...)
	cmd.Stdout = r.Stdout
	cmd.Stderr = r.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("spore %s: %w", args[0], err)
	}
	return nil
}

// CheckVersion enforces the minimum spore release bake depends on.
func CheckVersion(runner Runner) error {
	version, err := runner.Version()
	if err != nil {
		return fmt.Errorf("cleanroom bake requires the spore CLI on PATH: %w", err)
	}
	if compareVersions(version, minSporeVersion) < 0 {
		return fmt.Errorf("cleanroom bake requires spore >= %s, found %s", minSporeVersion, version)
	}
	return nil
}

func compareVersions(a, b string) int {
	as := strings.SplitN(a, ".", 3)
	bs := strings.SplitN(b, ".", 3)
	for i := 0; i < 3; i++ {
		av, bv := 0, 0
		if i < len(as) {
			fmt.Sscanf(as[i], "%d", &av)
		}
		if i < len(bs) {
			fmt.Sscanf(bs[i], "%d", &bv)
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}
