package bake

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/buildkite/cleanroom/internal/policy"
)

// GuestWorkspaceDir is where bake materialises the repository in the guest.
const GuestWorkspaceDir = "/workspace"

// Options configure one bake run.
type Options struct {
	// Dir is the repository directory: policy source and workspace content.
	Dir string
	// PolicySource is the path of the loaded policy file, for provenance.
	PolicySource string
	// Out is the output spore directory.
	Out string
	// Version is the cleanroom version recorded in provenance.
	Version string
	// Runner runs spore commands.
	Runner Runner
	// Log receives human progress output.
	Log io.Writer
}

// Result reports what a bake run did.
type Result struct {
	// UpToDate is true when the existing artifact matched the bake key and
	// no work was performed.
	UpToDate bool
	// Key is the bake key of the artifact.
	Key string
	// Out is the artifact path.
	Out string
}

// Run executes the bake pipeline: compile -> create builder -> copy-in ->
// warmup -> suspend -> verify. The builder VM is ephemeral: it is destroyed
// on failure and consumed by suspend on success.
func Run(compiled *policy.CompiledPolicy, options Options) (Result, error) {
	inputs, err := Compile(compiled)
	if err != nil {
		return Result{}, err
	}
	out := strings.TrimSpace(options.Out)
	if out == "" {
		return Result{}, errors.New("cleanroom bake requires --out")
	}
	if err := CheckVersion(options.Runner); err != nil {
		return Result{}, err
	}

	facts := CollectGitFacts(options.Dir)
	key := Key(compiled, facts)
	if facts.Dirty {
		fmt.Fprintln(options.Log, "cleanroom bake: workspace has uncommitted changes; artifact records workspace.git.dirty=true and is never treated as cache-fresh")
	}

	if upToDate, err := existingArtifactMatches(options.Runner, out, key, facts); err != nil {
		return Result{}, err
	} else if upToDate {
		fmt.Fprintf(options.Log, "cleanroom bake: %s is up to date (bake key %.12s)\n", out, key)
		return Result{UpToDate: true, Key: key, Out: out}, nil
	}

	annotations, err := Stamp(options.Dir, options.PolicySource, compiled, options.Version, inputs.NetworkRules)
	if err != nil {
		return Result{}, err
	}
	annotations[AnnotationPrefix+"bake.key"] = key

	builder := builderName(key)
	createArgs := append(inputs.Args(), AnnotationArgs(annotations)...)
	fmt.Fprintf(options.Log, "cleanroom bake: creating builder %s\n", builder)
	if err := options.Runner.Create(builder, createArgs); err != nil {
		return Result{}, fmt.Errorf("create builder VM: %w", err)
	}
	cleanupBuilder := true
	defer func() {
		if cleanupBuilder {
			_ = options.Runner.Remove(builder)
		}
	}()

	fmt.Fprintf(options.Log, "cleanroom bake: copying workspace into %s\n", GuestWorkspaceDir)
	if err := options.Runner.CopyIn(builder, options.Dir, GuestWorkspaceDir); err != nil {
		return Result{}, fmt.Errorf("copy workspace into builder: %w", err)
	}

	for i, step := range compiled.Warmup {
		fmt.Fprintf(options.Log, "cleanroom bake: warmup %d/%d: %s\n", i+1, len(compiled.Warmup), step)
		command := fmt.Sprintf("cd %s && %s", GuestWorkspaceDir, step)
		if err := options.Runner.ExecShell(builder, command); err != nil {
			return Result{}, fmt.Errorf("warmup step %d (%s): %w", i+1, step, err)
		}
	}

	fmt.Fprintf(options.Log, "cleanroom bake: capturing %s\n", out)
	if err := options.Runner.Suspend(builder, out); err != nil {
		return Result{}, fmt.Errorf("capture builder VM: %w", err)
	}
	// Suspend consumes the builder VM; nothing left to clean up.
	cleanupBuilder = false

	if err := verifyArtifact(options.Runner, out, key); err != nil {
		return Result{}, err
	}
	fmt.Fprintf(options.Log, "cleanroom bake: baked %s (bake key %.12s)\n", out, key)
	return Result{Key: key, Out: out}, nil
}

// existingArtifactMatches reports whether an artifact already exists at out
// with the same bake key. Dirty worktrees never match, because their content
// is not part of the key.
func existingArtifactMatches(runner Runner, out, key string, facts GitFacts) (bool, error) {
	if _, err := os.Stat(out); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	annotations, err := runner.InspectAnnotations(out)
	if err != nil {
		return false, fmt.Errorf("%s exists but is not a readable spore; remove it or choose another --out: %w", out, err)
	}
	if got := annotations[AnnotationPrefix+"provenance.version"]; got != ProvenanceVersion {
		return false, fmt.Errorf("%s exists but is missing cleanroom provenance; remove it or choose another --out", out)
	}
	existing := annotations[AnnotationPrefix+"bake.key"]
	if existing == "" {
		return false, fmt.Errorf("%s exists but carries no cleanroom bake key; remove it or choose another --out", out)
	}
	if facts.Dirty {
		return false, fmt.Errorf("%s exists and the workspace has uncommitted changes; dirty bakes are never cache-fresh, remove it to rebake", out)
	}
	if existing != key {
		return false, fmt.Errorf("%s exists with a different bake key (%.12s != %.12s); remove it to rebake", out, existing, key)
	}
	return true, nil
}

func verifyArtifact(runner Runner, out, key string) error {
	annotations, err := runner.InspectAnnotations(out)
	if err != nil {
		return fmt.Errorf("verify baked artifact: %w", err)
	}
	if got := annotations[AnnotationPrefix+"provenance.version"]; got != ProvenanceVersion {
		return fmt.Errorf("baked artifact is missing cleanroom provenance (version %q)", got)
	}
	if got := annotations[AnnotationPrefix+"bake.key"]; got != key {
		return fmt.Errorf("baked artifact records bake key %.12s, expected %.12s", got, key)
	}
	return nil
}

func builderName(key string) string {
	// Keep the name short: the control socket lives under the runtime dir at
	// .../vms/<name>/control.sock and macOS caps Unix socket paths at 104
	// bytes, which deep TMPDIR-based runtime dirs approach quickly.
	//
	// The random suffix keeps interrupted or concurrent bakes from colliding
	// on a deterministic name; a killed bake leaks a discoverable VM
	// (spore ls) instead of blocking every future bake of the same key.
	suffix := make([]byte, 2)
	if _, err := rand.Read(suffix); err == nil {
		return fmt.Sprintf("cr-bake-%s-%x", key[:8], suffix)
	}
	return "cr-bake-" + key[:8]
}
