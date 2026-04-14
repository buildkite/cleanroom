package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

const validImageRef = "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func baseRawPolicy() rawPolicy {
	raw := rawPolicy{}
	raw.Version = 1
	raw.Sandbox.Image.Ref = validImageRef
	raw.Sandbox.Network.Default = "deny"
	return raw
}

func TestLoaderPrefersRootPolicy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, PrimaryPolicyPath), []byte(`
version: 1
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  network:
    default: deny
    allow:
      - host: api.github.com
        ports: [443]
`), 0o644); err != nil {
		t.Fatalf("write primary policy: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(dir, ".buildkite"), 0o755); err != nil {
		t.Fatalf("mkdir .buildkite: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, FallbackPolicyPath), []byte(`
version: 1
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  network:
    default: deny
    allow:
      - host: registry.npmjs.org
        ports: [443]
`), 0o644); err != nil {
		t.Fatalf("write fallback policy: %v", err)
	}

	loader := Loader{}
	compiled, source, err := loader.LoadAndCompile(dir)
	if err != nil {
		t.Fatalf("load and compile: %v", err)
	}

	if source != filepath.Join(dir, PrimaryPolicyPath) {
		t.Fatalf("unexpected source %q", source)
	}
	if !compiled.Allows("api.github.com", 443) {
		t.Fatalf("expected api.github.com:443 to be allowed")
	}
	if compiled.Allows("registry.npmjs.org", 443) {
		t.Fatalf("did not expect fallback policy host to be used")
	}
}

func TestCompileRejectsAllowDefault(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	raw.Sandbox.Network.Default = "allow"
	_, err := Compile(raw)
	if err == nil {
		t.Fatal("expected compile to fail for default allow")
	}
}

func TestCompileRejectsUnsupportedVersion(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	raw.Version = 2

	_, err := Compile(raw)
	if err == nil {
		t.Fatal("expected compile to fail for unsupported version")
	}
}

func TestCompileRejectsMissingImageRef(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	raw.Sandbox.Image.Ref = ""

	_, err := Compile(raw)
	if err == nil {
		t.Fatal("expected compile to fail for missing image ref")
	}
}

func TestCompileRejectsTagOnlyImageRef(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	raw.Sandbox.Image.Ref = "ghcr.io/buildkite/cleanroom-base/alpine:latest"

	_, err := Compile(raw)
	if err == nil {
		t.Fatal("expected compile to fail for tag-only image ref")
	}
}

func TestCompileHashStable(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	raw.Sandbox.Network.Allow = []rawAllowRule{
		{Host: "api.github.com", Ports: []int{443, 443, 80}},
		{Host: "registry.npmjs.org", Ports: []int{443}},
	}

	compiledA, err := Compile(raw)
	if err != nil {
		t.Fatalf("compile A: %v", err)
	}
	compiledB, err := Compile(raw)
	if err != nil {
		t.Fatalf("compile B: %v", err)
	}

	if compiledA.Hash != compiledB.Hash {
		t.Fatalf("hash mismatch: %s != %s", compiledA.Hash, compiledB.Hash)
	}
}

func TestCompileCapturesImageDigest(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	compiled, err := Compile(raw)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	if compiled.ImageRef != validImageRef {
		t.Fatalf("unexpected image ref: got %q want %q", compiled.ImageRef, validImageRef)
	}
	if compiled.ImageDigest != "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
		t.Fatalf("unexpected image digest: %q", compiled.ImageDigest)
	}
}

func TestCompileCapturesDockerServiceRequirement(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	raw.Sandbox.Services.Docker.Required = true

	compiled, err := Compile(raw)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !compiled.Services.Docker.Required {
		t.Fatal("expected compiled policy to require docker service")
	}
}

func TestCompileDefaultsDependenciesDisabled(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	compiled, err := Compile(raw)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled.Dependencies.Enabled() {
		t.Fatal("expected compiled policy to disable dependency bootstrap by default")
	}
}

func TestCompileNormalizesDependencyBootstrapConfig(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	raw.Sandbox.Dependencies.Command = []string{"go", "mod", "download"}
	raw.Sandbox.Dependencies.Key.Files = []string{"./go.sum", "go.mod", "go.sum"}

	compiled, err := Compile(raw)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if got, want := compiled.Dependencies.Command, []string{"go", "mod", "download"}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("unexpected dependency command: got %v want %v", got, want)
	}
	if got, want := compiled.Dependencies.KeyFiles, []string{"go.mod", "go.sum"}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("unexpected dependency key files: got %v want %v", got, want)
	}
}

func TestCompileRejectsDependencyKeyFilesWithoutCommand(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	raw.Sandbox.Dependencies.Key.Files = []string{"go.sum"}

	_, err := Compile(raw)
	if err == nil {
		t.Fatal("expected compile to reject dependency key files without a command")
	}
	if !strings.Contains(err.Error(), "sandbox.dependencies.key.files") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadPropagatesPrimaryStatError(t *testing.T) {
	t.Parallel()

	loader := Loader{}
	_, _, err := loader.Load(string([]byte{'b', 'a', 'd', 0, 'p', 'a', 't', 'h'}))
	if err == nil {
		t.Fatal("expected load to fail on primary policy stat error")
	}
	if !strings.Contains(err.Error(), "check policy") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadRepositoryDefaultsCurrentRepoSettings(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, PrimaryPolicyPath), []byte(`
version: 1
repository:
  mode: current-repo
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  network:
    default: deny
`), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}

	cfg, source, err := Loader{}.LoadRepository(dir)
	if err != nil {
		t.Fatalf("LoadRepository returned error: %v", err)
	}
	if got, want := source, filepath.Join(dir, PrimaryPolicyPath); got != want {
		t.Fatalf("unexpected source: got %q want %q", got, want)
	}
	if got, want := cfg.Mode, "current-repo"; got != want {
		t.Fatalf("unexpected mode: got %q want %q", got, want)
	}
	if got, want := cfg.Remote, "origin"; got != want {
		t.Fatalf("unexpected remote default: got %q want %q", got, want)
	}
	if got, want := cfg.Path, "/workspace"; got != want {
		t.Fatalf("unexpected path default: got %q want %q", got, want)
	}
}

func TestLoadRepositoryDefaultsImplicitCurrentRepoWhenOmitted(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, PrimaryPolicyPath), []byte(`
version: 1
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  network:
    default: deny
`), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}

	cfg, _, err := Loader{}.LoadRepository(dir)
	if err != nil {
		t.Fatalf("LoadRepository returned error: %v", err)
	}
	if !cfg.Enabled() {
		t.Fatal("expected omitted repository block to default to enabled")
	}
	if got, want := cfg.Mode, "current-repo"; got != want {
		t.Fatalf("unexpected mode: got %q want %q", got, want)
	}
	if got, want := cfg.Remote, "origin"; got != want {
		t.Fatalf("unexpected remote default: got %q want %q", got, want)
	}
	if got, want := cfg.Path, "/workspace"; got != want {
		t.Fatalf("unexpected path default: got %q want %q", got, want)
	}
	if !cfg.Implicit {
		t.Fatal("expected omitted repository block to be marked implicit")
	}
}

func TestLoadRepositoryAllowsDisablingImplicitCurrentRepo(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, PrimaryPolicyPath), []byte(`
version: 1
repository:
  enabled: false
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  network:
    default: deny
`), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}

	cfg, _, err := Loader{}.LoadRepository(dir)
	if err != nil {
		t.Fatalf("LoadRepository returned error: %v", err)
	}
	if cfg.Enabled() {
		t.Fatal("expected repository.enabled=false to disable repository bootstrap")
	}
}

func TestLoadRepositoryRejectsRelativePath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, PrimaryPolicyPath), []byte(`
version: 1
repository:
  mode: current-repo
  path: workspace
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  network:
    default: deny
`), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}

	_, _, err := Loader{}.LoadRepository(dir)
	if err == nil {
		t.Fatal("expected LoadRepository to reject relative repository.path")
	}
	if !strings.Contains(err.Error(), "repository.path") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFromProtoRejectsMismatchedImageDigest(t *testing.T) {
	t.Parallel()

	_, err := FromProto(&cleanroomv1.Policy{
		Version:        1,
		ImageRef:       validImageRef,
		ImageDigest:    "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		NetworkDefault: "deny",
	})
	if err == nil {
		t.Fatal("expected FromProto to reject mismatched image_digest")
	}
	if !strings.Contains(err.Error(), "image_digest") {
		t.Fatalf("expected image_digest error, got %v", err)
	}
}

func TestFromProtoCanonicalisesAllowRules(t *testing.T) {
	t.Parallel()

	compiled, err := FromProto(&cleanroomv1.Policy{
		Version:        1,
		ImageRef:       validImageRef,
		NetworkDefault: "deny",
		Allow: []*cleanroomv1.PolicyAllowRule{
			{Host: "registry.npmjs.org", Ports: []int32{443}},
			{Host: "api.github.com", Ports: []int32{443, 80, 443}},
		},
	})
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}

	if len(compiled.Allow) != 2 {
		t.Fatalf("unexpected allow rule count: got %d want 2", len(compiled.Allow))
	}
	if got, want := compiled.Allow[0].Host, "api.github.com"; got != want {
		t.Fatalf("expected allow rules to be sorted by host: got %q want %q", got, want)
	}
	if got, want := compiled.Allow[1].Host, "registry.npmjs.org"; got != want {
		t.Fatalf("expected second host %q, got %q", want, got)
	}
	if got, want := compiled.Allow[0].Ports, []int{80, 443}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("expected deduplicated/sorted ports %v, got %v", want, got)
	}
}

func TestFromProtoPropagatesDockerServiceRequirement(t *testing.T) {
	t.Parallel()

	compiled, err := FromProto(&cleanroomv1.Policy{
		Version:        1,
		ImageRef:       validImageRef,
		NetworkDefault: "deny",
		Services: &cleanroomv1.PolicyServices{
			Docker: &cleanroomv1.PolicyDockerService{
				Required: true,
			},
		},
	})
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	if !compiled.Services.Docker.Required {
		t.Fatal("expected docker service requirement from proto policy")
	}
}

func TestFromProtoAcceptsAllowDefault(t *testing.T) {
	t.Parallel()

	compiled, err := FromProto(&cleanroomv1.Policy{
		Version:        1,
		ImageRef:       validImageRef,
		NetworkDefault: "allow",
	})
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	if got, want := compiled.NetworkDefault, "allow"; got != want {
		t.Fatalf("unexpected network default: got %q want %q", got, want)
	}
	if !compiled.Allows("example.com", 443) {
		t.Fatal("expected allow-default policy to allow arbitrary host:port")
	}
}

func TestCompiledPolicyProtoRoundTripPreservesDependencies(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	raw.Sandbox.Dependencies.Command = []string{"go", "mod", "download"}
	raw.Sandbox.Dependencies.Key.Files = []string{"go.mod", "go.sum"}
	compiled, err := Compile(raw)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	roundTripped, err := FromProto(compiled.ToProto())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	if got, want := strings.Join(roundTripped.Dependencies.Command, "\x00"), strings.Join(compiled.Dependencies.Command, "\x00"); got != want {
		t.Fatalf("unexpected dependency command after round trip: got %q want %q", got, want)
	}
	if got, want := strings.Join(roundTripped.Dependencies.KeyFiles, "\x00"), strings.Join(compiled.Dependencies.KeyFiles, "\x00"); got != want {
		t.Fatalf("unexpected dependency key files after round trip: got %q want %q", got, want)
	}
}
