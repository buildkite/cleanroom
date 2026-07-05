package bake

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/buildkite/cleanroom/internal/mediation"
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
	// GatewaySocket is a live Unix socket serving the lineage gateway.
	// Required when the policy requests mediation services; rejected
	// otherwise.
	GatewaySocket string
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

	facts := CollectGitFactsExcluding(options.Dir, ArtifactExclusions(options.Dir, out))
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

	annotations, err := Stamp(options.Dir, options.PolicySource, compiled, options.Version, inputs.NetworkRules, facts)
	if err != nil {
		return Result{}, err
	}
	annotations[AnnotationPrefix+"bake.key"] = key

	gatewayArgs, err := gatewayCreateArgs(compiled, options.GatewaySocket, len(inputs.NetworkRules) > 0)
	if err != nil {
		return Result{}, err
	}

	builder := builderName(key)
	createArgs := append(inputs.Args(), gatewayArgs...)
	createArgs = append(createArgs, AnnotationArgs(annotations)...)
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
	// Copy the git-visible file set, not the raw checkout: ignored files
	// (.env, node_modules) are invisible to the bake key's dirty decision and
	// may hold secrets, and .git can carry credentialed remote URLs. Neither
	// belongs in the captured artifact. Non-git workspaces have no ignore
	// semantics and copy as-is.
	copySrc := options.Dir
	if facts.HasGit {
		staged, cleanupStaged, err := StageWorkspace(options.Dir, ArtifactExclusions(options.Dir, out))
		if err != nil {
			return Result{}, err
		}
		defer cleanupStaged()
		copySrc = staged
	}
	if err := options.Runner.CopyIn(builder, copySrc, GuestWorkspaceDir); err != nil {
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

// gatewayCreateArgs translates the policy's mediation requests plus a live
// gateway socket into spore create arguments. Mediation without a socket and
// a socket without mediation both fail closed.
func gatewayCreateArgs(compiled *policy.CompiledPolicy, socketPath string, networkAlreadyEnabled bool) ([]string, error) {
	socketPath = strings.TrimSpace(socketPath)
	if len(compiled.Mediation) == 0 {
		if socketPath != "" {
			return nil, errors.New("cleanroom bake: --gateway-socket was given but the policy requests no mediation services")
		}
		return nil, nil
	}
	if socketPath == "" {
		return nil, fmt.Errorf("cleanroom bake: the policy requests mediation services %v; serve them with cleanroom gateway serve and pass --gateway-socket", compiled.Mediation)
	}
	info, err := os.Stat(socketPath)
	if err != nil {
		return nil, fmt.Errorf("stat gateway socket %q: %w", socketPath, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("gateway socket %q is not a Unix socket", socketPath)
	}
	if !networkAlreadyEnabled {
		// spore create with bare --net (no allow rules) permits all public
		// egress, which would let a compromised warmup exfiltrate around the
		// gateway. Require an allow rule so networking is deny-by-default;
		// bound-service routing happens before egress policy, so mediation
		// itself does not need open egress. Lifts once spore create gains a
		// create-time default-deny flag (filed upstream alongside the
		// bound-service DNS fix).
		return nil, errors.New("cleanroom bake: the policy requests mediation but declares no network allow rules; add a sandbox.network.allow rule so builder networking stays deny-by-default")
	}
	declaration := fmt.Sprintf("%s:%d=unix:%s", mediation.BoundServiceName, mediation.GuestPort, socketPath)
	return []string{"--bind-service", declaration}, nil
}

// ArtifactExclusions returns the artifact path relative to the repository dir
// when the artifact lives inside it, so git dirty detection ignores the spore
// bake itself writes (and verify --dir audits). An artifact outside the repo
// needs no exclusion.
func ArtifactExclusions(dir, out string) []string {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil
	}
	absOut, err := filepath.Abs(out)
	if err != nil {
		return nil
	}
	rel, err := filepath.Rel(absDir, absOut)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return nil
	}
	return []string{rel}
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
