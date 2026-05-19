package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/bytesize"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/guestenv"
	"gopkg.in/yaml.v3"
)

const validImageRef = "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func baseRawPolicy() rawPolicy {
	raw := rawPolicy{}
	raw.Version = 1
	raw.Sandbox.Image.Ref = validImageRef
	raw.Sandbox.Network.Default = "deny"
	return raw
}

func rawBlock(name string, command []string, inputs []string, outputs rawPolicyBlockOutputs) rawPolicyBlock {
	return rawPolicyBlock{
		Name:    name,
		Command: rawDependencyCommandSpec(command),
		Inputs:  rawPolicyBlockInputs{Files: inputs},
		Outputs: outputs,
	}
}

func rawDependencyBlocks(blocks ...rawPolicyBlock) rawDependencyStage {
	return rawDependencyStage{
		Blocks:      rawPolicyBlocks(blocks),
		blocksField: "sandbox.dependencies",
	}
}

func protoBlock(name string, command []string, inputs []string, outputs *cleanroomv1.PolicyBlockOutputs) *cleanroomv1.PolicyBlock {
	return &cleanroomv1.PolicyBlock{
		Name:    name,
		Command: command,
		Inputs:  &cleanroomv1.PolicyBlockInputs{Files: inputs},
		Outputs: outputs,
	}
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
	raw.Sandbox.Network.Allow = rawAllowRules{
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

func TestCompileNormalizesNetworkAllowShorthand(t *testing.T) {
	t.Parallel()

	var raw rawPolicy
	if err := yaml.Unmarshal([]byte(`
version: 1
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  network:
    default: deny
    allow:
      - GitHub.com:443
      - host: registry.npmjs.org
        ports: [443, 80, 443]
`), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	compiled, err := Compile(raw)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	if len(compiled.Allow) != 2 {
		t.Fatalf("unexpected allow rule count: got %d want 2", len(compiled.Allow))
	}
	if got, want := compiled.Allow[0].Host, "github.com"; got != want {
		t.Fatalf("unexpected first allow host: got %q want %q", got, want)
	}
	if got, want := compiled.Allow[0].Ports, []int{443}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("unexpected first allow ports: got %v want %v", got, want)
	}
	if got, want := compiled.Allow[1].Host, "registry.npmjs.org"; got != want {
		t.Fatalf("unexpected second allow host: got %q want %q", got, want)
	}
	if got, want := compiled.Allow[1].Ports, []int{80, 443}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("unexpected second allow ports: got %v want %v", got, want)
	}
}

func TestCompileNormalizesNetworkAllowScalar(t *testing.T) {
	t.Parallel()

	var raw rawPolicy
	if err := yaml.Unmarshal([]byte(`
version: 1
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  network:
    default: deny
    allow: proxy.golang.org:443
`), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	compiled, err := Compile(raw)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(compiled.Allow) != 1 {
		t.Fatalf("unexpected allow rule count: got %d want 1", len(compiled.Allow))
	}
	if !compiled.Allows("proxy.golang.org", 443) {
		t.Fatal("expected proxy.golang.org:443 to be allowed")
	}
	if compiled.Allows("proxy.golang.org", 80) {
		t.Fatal("did not expect proxy.golang.org:80 to be allowed")
	}
}

func TestCompileNormalizesStageScopedNetwork(t *testing.T) {
	t.Parallel()

	var raw rawPolicy
	if err := yaml.Unmarshal([]byte(`
version: 1
repository:
  network:
    allow: github.com:443
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  network:
    default: deny
    dependencies:
      allow:
        - proxy.golang.org:443
        - host: storage.googleapis.com
          ports: [443, 443]
    services:
      allow: registry-1.docker.io:443
    execution: {}
`), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	compiled, err := Compile(raw)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !compiled.HasStageScopedNetwork() {
		t.Fatal("expected stage-scoped network policy")
	}
	if len(compiled.Allow) != 0 {
		t.Fatalf("expected global allowlist to be empty, got %v", compiled.Allow)
	}
	if !compiled.AllowsForStage(NetworkStageWorkspace, "github.com", 443) {
		t.Fatal("expected workspace stage to allow github.com:443")
	}
	if compiled.AllowsForStage(NetworkStageExecution, "github.com", 443) {
		t.Fatal("did not expect execution stage to inherit workspace allowlist")
	}
	if !compiled.AllowsForStage(NetworkStageDependencies, "proxy.golang.org", 443) {
		t.Fatal("expected dependencies stage to allow proxy.golang.org:443")
	}
	if !compiled.AllowsForStage(NetworkStageDependencies, "storage.googleapis.com", 443) {
		t.Fatal("expected dependencies stage to allow storage.googleapis.com:443")
	}
	if !compiled.AllowsForStage(NetworkStageServices, "registry-1.docker.io", 443) {
		t.Fatal("expected services stage to allow registry-1.docker.io:443")
	}
	if compiled.NetworkStages.Execution == nil {
		t.Fatal("expected explicit empty execution stage network block")
	}
	if compiled.AllowsForStage(NetworkStageExecution, "registry-1.docker.io", 443) {
		t.Fatal("did not expect execution stage egress")
	}
}

func TestNetworkPolicyForStageRehashesStageScopedPolicies(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	raw.Repository = &rawRepository{
		Network: &rawStageNetworkConfig{
			Allow: rawAllowRules{{Host: "github.com", Ports: []int{443}}},
		},
	}
	raw.Sandbox.Network.Dependencies = &rawStageNetworkConfig{
		Allow: rawAllowRules{{Host: "proxy.golang.org", Ports: []int{443}}},
	}
	raw.Sandbox.Network.Execution = &rawStageNetworkConfig{}

	compiled, err := Compile(raw)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	workspace := compiled.NetworkPolicyForStage(NetworkStageWorkspace)
	dependencies := compiled.NetworkPolicyForStage(NetworkStageDependencies)
	execution := compiled.NetworkPolicyForStage(NetworkStageExecution)

	for stage, effective := range map[NetworkStage]*CompiledPolicy{
		NetworkStageWorkspace:    workspace,
		NetworkStageDependencies: dependencies,
		NetworkStageExecution:    execution,
	} {
		if effective.Hash == "" {
			t.Fatalf("expected %s effective policy hash", stage)
		}
		if effective.Hash == compiled.Hash {
			t.Fatalf("expected %s effective policy hash to differ from full policy hash", stage)
		}
		if effective.NetworkStages != nil {
			t.Fatalf("expected %s effective policy to drop stage table", stage)
		}
	}
	if workspace.Hash == dependencies.Hash {
		t.Fatal("expected workspace and dependencies effective policies to have distinct hashes")
	}
	if dependencies.Hash == execution.Hash {
		t.Fatal("expected dependencies and execution effective policies to have distinct hashes")
	}
	if got := compiled.NetworkPolicyForStage(NetworkStageDependencies).Hash; got != dependencies.Hash {
		t.Fatalf("expected dependencies effective policy hash to be stable: got %q want %q", got, dependencies.Hash)
	}
}

func TestNetworkPolicyForStageRetainsLegacyPolicyHash(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	raw.Sandbox.Network.Allow = rawAllowRules{{Host: "api.github.com", Ports: []int{443}}}

	compiled, err := Compile(raw)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	effective := compiled.NetworkPolicyForStage(NetworkStageExecution)
	if effective.Hash != compiled.Hash {
		t.Fatalf("expected legacy effective policy hash to remain unchanged: got %q want %q", effective.Hash, compiled.Hash)
	}
}

func TestCompileRejectsMixedGlobalAndStageNetwork(t *testing.T) {
	t.Parallel()

	var raw rawPolicy
	if err := yaml.Unmarshal([]byte(`
version: 1
repository:
  network:
    allow: github.com:443
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  network:
    default: deny
    allow: proxy.golang.org:443
`), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	_, err := Compile(raw)
	if err == nil {
		t.Fatal("expected compile to reject mixed global and stage-local allowlists")
	}
	if !strings.Contains(err.Error(), "sandbox.network.allow") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUnmarshalRejectsInvalidNetworkAllowShorthand(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		allow    string
		contains string
	}{
		{name: "bare host", allow: "github.com", contains: "host:port"},
		{name: "url", allow: "https://github.com:443", contains: "not a URL"},
		{name: "invalid port", allow: "github.com:notaport", contains: "invalid port"},
		{name: "zero port", allow: "github.com:0", contains: "invalid port 0"},
		{name: "ipv6", allow: `"[2001:db8::1]:443"`, contains: "does not support IPv6"},
		{name: "non string", allow: "[443]", contains: "host:port string or mapping"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var raw rawPolicy
			err := yaml.Unmarshal([]byte(`
version: 1
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  network:
    default: deny
    allow: `+tc.allow+`
`), &raw)
			if err == nil {
				t.Fatal("expected unmarshal to reject invalid network allow shorthand")
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("unexpected error: got %v want substring %q", err, tc.contains)
			}
		})
	}
}

func TestUnmarshalRejectsUnknownNetworkAllowField(t *testing.T) {
	t.Parallel()

	var raw rawPolicy
	err := yaml.Unmarshal([]byte(`
version: 1
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  network:
    default: deny
    allow:
      - host: github.com
        ports: [443]
        protocol: tcp
`), &raw)
	if err == nil {
		t.Fatal("expected unmarshal to reject unknown network allow field")
	}
	if !strings.Contains(err.Error(), "protocol") {
		t.Fatalf("unexpected error: %v", err)
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
	raw.Sandbox.Docker.Required = true

	compiled, err := Compile(raw)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !compiled.Docker.Required {
		t.Fatal("expected compiled policy to require docker service")
	}
}

func TestCompileNormalizesResourceRequirements(t *testing.T) {
	t.Parallel()

	var raw rawPolicy
	if err := yaml.Unmarshal([]byte(`
version: 1
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  resources:
    vcpus: 4
    memory: 12GiB
    disk: 16GiB
  network:
    default: deny
`), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	compiled, err := Compile(raw)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled.Resources == nil {
		t.Fatal("expected resource requirements")
	}
	if got, want := compiled.Resources.VCPUs, int64(4); got != want {
		t.Fatalf("unexpected vcpus: got %d want %d", got, want)
	}
	if got, want := compiled.Resources.MemoryBytes, int64(12<<30); got != want {
		t.Fatalf("unexpected memory bytes: got %d want %d", got, want)
	}
	if got, want := compiled.Resources.DiskBytes, int64(16<<30); got != want {
		t.Fatalf("unexpected disk bytes: got %d want %d", got, want)
	}
}

func TestCompileRejectsNonPositiveResourceRequirements(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	vcpus := int64(0)
	raw.Sandbox.Resources.VCPUs = &vcpus

	_, err := Compile(raw)
	if err == nil {
		t.Fatal("expected compile to reject zero vcpus")
	}
	if !strings.Contains(err.Error(), "sandbox.resources.vcpus") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCompileDefaultsServicesBootstrapDisabled(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	compiled, err := Compile(raw)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled.Services.BootstrapEnabled() {
		t.Fatal("expected compiled policy to disable services bootstrap by default")
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

func TestCompileRejectsRepositoryDisabledWithDependencyBlocks(t *testing.T) {
	t.Parallel()

	var raw rawPolicy
	if err := yaml.Unmarshal([]byte(`
version: 1
repository:
  enabled: false
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  dependencies:
    - name: go-modules
      command: go mod download
      inputs:
        files:
          - go.mod
          - go.sum
      outputs:
        dirs:
          - ${HOME}/go/pkg/mod
  network:
    default: deny
`), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	_, err := Compile(raw)
	if err == nil {
		t.Fatal("expected compile to reject dependency blocks without repository bootstrap")
	}
	if !strings.Contains(err.Error(), "sandbox.dependencies") || !strings.Contains(err.Error(), "repository bootstrap is disabled") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCompileNormalizesDependencyBootstrapConfig(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	raw.Sandbox.Dependencies = rawDependencyBlocks(
		rawBlock("go-modules", []string{"go", "mod", "download"}, []string{"./go.sum", "go.mod", "go.sum"}, rawPolicyBlockOutputs{
			Dirs: []string{"${HOME}/go/pkg/mod"},
		}),
	)

	compiled, err := Compile(raw)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(compiled.Dependencies.Blocks) != 1 {
		t.Fatalf("unexpected dependency block count: got %d want 1", len(compiled.Dependencies.Blocks))
	}
	block := compiled.Dependencies.Blocks[0]
	if got, want := block.Name, "go-modules"; got != want {
		t.Fatalf("unexpected dependency block name: got %q want %q", got, want)
	}
	if got, want := block.Command, []string{"go", "mod", "download"}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("unexpected dependency command: got %v want %v", got, want)
	}
	if got, want := compiled.Dependencies.KeyFiles, []string{"go.mod", "go.sum"}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("unexpected dependency key files: got %v want %v", got, want)
	}
	if got, want := block.Outputs.Dirs, []string{guestenv.DefaultHome + "/go/pkg/mod"}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("unexpected dependency outputs: got %v want %v", got, want)
	}
}

func TestCompileNormalizesWorkspaceRelativeOutputPaths(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	raw.Sandbox.Dependencies = rawDependencyBlocks(
		rawPolicyBlock{
			Name:    "node",
			Command: rawDependencyCommandSpec{"npm", "ci"},
			Inputs:  rawPolicyBlockInputs{Files: []string{"package-lock.json"}},
			Env: map[string]string{
				"WORKDIR":   "${WORKSPACE}",
				"WORKCACHE": "$WORKSPACE/.cache",
			},
			Outputs: rawPolicyBlockOutputs{
				Dirs:  []string{"node_modules", "./vendor/bundle", "${WORKSPACE}/.cache/yarn"},
				Files: []string{"tmp/state.json", "${WORKSPACE}/.npmrc"},
			},
		},
	)

	compiled, err := Compile(raw)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	block := compiled.Dependencies.Blocks[0]
	if got, want := block.Outputs.Dirs, []string{"/workspace/.cache/yarn", "/workspace/node_modules", "/workspace/vendor/bundle"}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("unexpected output dirs: got %v want %v", got, want)
	}
	if got, want := block.Outputs.Files, []string{"/workspace/.npmrc", "/workspace/tmp/state.json"}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("unexpected output files: got %v want %v", got, want)
	}
	if got, want := block.Env["WORKDIR"], "/workspace"; got != want {
		t.Fatalf("unexpected WORKDIR: got %q want %q", got, want)
	}
	if got, want := block.Env["WORKCACHE"], "/workspace/.cache"; got != want {
		t.Fatalf("unexpected WORKCACHE: got %q want %q", got, want)
	}
}

func TestCompileNormalizesBlockWorkspaceAgainstRepositoryPath(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	raw.Repository = &rawRepository{Path: "/src/"}
	raw.Sandbox.Dependencies = rawDependencyBlocks(
		rawPolicyBlock{
			Name:    "node",
			Command: rawDependencyCommandSpec{"npm", "ci"},
			Inputs:  rawPolicyBlockInputs{Files: []string{"package-lock.json"}},
			Env: map[string]string{
				"WORKDIR":   "${WORKSPACE}",
				"WORKCACHE": "$WORKSPACE/.cache",
			},
			Outputs: rawPolicyBlockOutputs{
				Dirs: []string{"node_modules", "${WORKSPACE}/vendor/bundle"},
			},
		},
	)

	compiled, err := Compile(raw)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	block := compiled.Dependencies.Blocks[0]
	if got, want := block.Outputs.Dirs, []string{"/src/node_modules", "/src/vendor/bundle"}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("unexpected output dirs: got %v want %v", got, want)
	}
	if got, want := block.Env["WORKDIR"], "/src"; got != want {
		t.Fatalf("unexpected WORKDIR: got %q want %q", got, want)
	}
	if got, want := block.Env["WORKCACHE"], "/src/.cache"; got != want {
		t.Fatalf("unexpected WORKCACHE: got %q want %q", got, want)
	}

	defaultPathRaw := raw
	defaultPathRaw.Repository = nil
	defaultPathCompiled, err := Compile(defaultPathRaw)
	if err != nil {
		t.Fatalf("compile default repository path: %v", err)
	}
	if defaultPathCompiled.Hash == compiled.Hash {
		t.Fatalf("expected repository.path to affect normalized block policy hash, got %q", compiled.Hash)
	}
}

func TestCompilePreservesDependencyReuseFromYAML(t *testing.T) {
	t.Parallel()

	var raw rawPolicy
	if err := yaml.Unmarshal([]byte(`
version: 1
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  dependencies:
    reuse: portable
    blocks:
      - name: go-modules
        command: go mod download
        inputs:
          files:
            - go.mod
            - go.sum
        outputs:
          dirs:
            - ${HOME}/go/pkg/mod
  network:
    default: deny
`), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	compiled, err := Compile(raw)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if got, want := compiled.Dependencies.Reuse, DependencyReusePortable; got != want {
		t.Fatalf("unexpected dependency reuse mode: got %q want %q", got, want)
	}
	if got, want := compiled.Dependencies.KeyFiles, []string{"go.mod", "go.sum"}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("unexpected dependency key files: got %v want %v", got, want)
	}
}

func TestUnmarshalRejectsOldDependenciesObjectShape(t *testing.T) {
	t.Parallel()

	var raw rawPolicy
	err := yaml.Unmarshal([]byte(`
version: 1
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  dependencies:
    command: go mod download
    key:
      files: [go.mod, go.sum]
  network:
    default: deny
`), &raw)
	if err == nil {
		t.Fatal("expected unmarshal to reject old dependencies object shape")
	}
}

func TestCompileNormalizesServicesBootstrapConfig(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	raw.Sandbox.Docker.Required = true
	raw.Sandbox.Services = rawPolicyBlocks{
		rawBlock("postgres", []string{"docker", "compose", "up", "-d", "postgres"}, []string{"./docker-compose.yml", "docker-compose.yml"}, rawPolicyBlockOutputs{
			Dirs: []string{"/var/lib/cleanroom/services/postgres"},
		}),
	}

	compiled, err := Compile(raw)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !compiled.Docker.Required {
		t.Fatal("expected compiled policy to preserve docker service requirement")
	}
	if len(compiled.Services.Blocks) != 1 {
		t.Fatalf("unexpected services block count: got %d want 1", len(compiled.Services.Blocks))
	}
	if got, want := compiled.Services.Blocks[0].Command, []string{"docker", "compose", "up", "-d", "postgres"}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("unexpected services command: got %v want %v", got, want)
	}
	if got, want := compiled.Services.KeyFiles, []string{"docker-compose.yml"}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("unexpected services key files: got %v want %v", got, want)
	}
}

func TestCompileRejectsRepositoryDisabledWithServiceBlocks(t *testing.T) {
	t.Parallel()

	var raw rawPolicy
	if err := yaml.Unmarshal([]byte(`
version: 1
repository:
  enabled: false
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  services:
    - name: postgres
      command: docker compose up -d postgres
      inputs:
        files:
          - docker-compose.yml
      outputs:
        dirs:
          - /var/lib/cleanroom/services/postgres
  network:
    default: deny
`), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	_, err := Compile(raw)
	if err == nil {
		t.Fatal("expected compile to reject service blocks without repository bootstrap")
	}
	if !strings.Contains(err.Error(), "sandbox.services") || !strings.Contains(err.Error(), "repository bootstrap is disabled") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUnmarshalRejectsOldServicesObjectShape(t *testing.T) {
	t.Parallel()

	var raw rawPolicy
	err := yaml.Unmarshal([]byte(`
version: 1
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  services:
    docker:
      required: true
    command: docker compose up -d postgres
    key:
      files: [docker-compose.yml]
  network:
    default: deny
`), &raw)
	if err == nil {
		t.Fatal("expected unmarshal to reject old services object shape")
	}
}

func TestCompileRejectsDuplicateDependencyBlockNames(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	raw.Sandbox.Dependencies = rawDependencyBlocks(
		rawBlock("go-modules", []string{"go", "mod", "download"}, []string{"go.mod"}, rawPolicyBlockOutputs{Dirs: []string{"${HOME}/go/pkg/mod"}}),
		rawBlock("go-modules", []string{"go", "env"}, []string{"go.sum"}, rawPolicyBlockOutputs{Dirs: []string{"${HOME}/.cache/go-build"}}),
	)

	_, err := Compile(raw)
	if err == nil {
		t.Fatal("expected compile to reject duplicate block names")
	}
	if !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCompileRejectsInvalidOutputPaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		outputs rawPolicyBlockOutputs
		want    string
	}{
		{name: "root dir", outputs: rawPolicyBlockOutputs{Dirs: []string{"/"}}, want: "must not be /"},
		{name: "repository root dir", outputs: rawPolicyBlockOutputs{Dirs: []string{"/workspace"}}, want: "must not be the repository root"},
		{name: "workspace variable root dir", outputs: rawPolicyBlockOutputs{Dirs: []string{"${WORKSPACE}"}}, want: "must not be the repository root"},
		{name: "relative escape", outputs: rawPolicyBlockOutputs{Dirs: []string{"../cache"}}, want: "must stay within the repository root"},
		{name: "workspace variable escape", outputs: rawPolicyBlockOutputs{Dirs: []string{"${WORKSPACE}/../cache"}}, want: "must stay within the repository root"},
		{name: "glob dir", outputs: rawPolicyBlockOutputs{Dirs: []string{"node_modules/*"}}, want: "must not contain glob characters"},
		{name: "unknown variable", outputs: rawPolicyBlockOutputs{Dirs: []string{"${CACHE}/go"}}, want: "unsupported variable expansion"},
		{name: "home variable typo", outputs: rawPolicyBlockOutputs{Dirs: []string{"$HOMECACHE/go"}}, want: "unsupported variable expansion"},
		{name: "home braced variable typo", outputs: rawPolicyBlockOutputs{Dirs: []string{"${HOME}CACHE/go"}}, want: "unsupported variable expansion"},
		{name: "workspace variable typo", outputs: rawPolicyBlockOutputs{Dirs: []string{"$WORKSPACECACHE/go"}}, want: "unsupported variable expansion"},
		{name: "workspace braced variable typo", outputs: rawPolicyBlockOutputs{Dirs: []string{"${WORKSPACE}CACHE/go"}}, want: "unsupported variable expansion"},
		{name: "other user home", outputs: rawPolicyBlockOutputs{Dirs: []string{"~builder/.cache"}}, want: "does not support ~user expansion"},
		{name: "duplicate normalized dirs", outputs: rawPolicyBlockOutputs{Dirs: []string{"~/go/pkg/mod", "${HOME}/go/pkg/mod"}}, want: "duplicates output path"},
		{name: "overlapping dirs", outputs: rawPolicyBlockOutputs{Dirs: []string{"${HOME}/go", "${HOME}/go/pkg/mod"}}, want: "overlapping paths"},
		{name: "file inside dir", outputs: rawPolicyBlockOutputs{Dirs: []string{"${HOME}/.cache/mise"}, Files: []string{"${HOME}/.cache/mise/index.json"}}, want: "inside output dir"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			raw := baseRawPolicy()
			raw.Sandbox.Dependencies = rawDependencyBlocks(
				rawBlock("deps", []string{"true"}, []string{"go.mod"}, tc.outputs),
			)

			_, err := Compile(raw)
			if err == nil {
				t.Fatal("expected compile to reject output path")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestCompileRejectsOverlappingOutputsAcrossDependencyBlocks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		first  rawPolicyBlockOutputs
		second rawPolicyBlockOutputs
		want   string
	}{
		{
			name:   "duplicate dir",
			first:  rawPolicyBlockOutputs{Dirs: []string{"${HOME}/go/pkg/mod"}},
			second: rawPolicyBlockOutputs{Dirs: []string{"~/go/pkg/mod"}},
			want:   "duplicates",
		},
		{
			name:   "nested dir",
			first:  rawPolicyBlockOutputs{Dirs: []string{"${HOME}/go"}},
			second: rawPolicyBlockOutputs{Dirs: []string{"${HOME}/go/pkg/mod"}},
			want:   "overlaps",
		},
		{
			name:   "file inside dir",
			first:  rawPolicyBlockOutputs{Dirs: []string{"${HOME}/.cache/mise"}},
			second: rawPolicyBlockOutputs{Files: []string{"${HOME}/.cache/mise/state.json"}},
			want:   "overlaps",
		},
		{
			name:   "duplicate file",
			first:  rawPolicyBlockOutputs{Files: []string{"${HOME}/.bundle/config"}},
			second: rawPolicyBlockOutputs{Files: []string{"~/.bundle/config"}},
			want:   "duplicates",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			raw := baseRawPolicy()
			raw.Sandbox.Dependencies = rawDependencyBlocks(
				rawBlock("first", []string{"true"}, []string{"go.mod"}, tc.first),
				rawBlock("second", []string{"true"}, []string{"go.sum"}, tc.second),
			)

			_, err := Compile(raw)
			if err == nil {
				t.Fatal("expected compile to reject overlapping outputs")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestCompileRejectsOverlappingOutputsAcrossDependencyAndServiceBlocks(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	raw.Sandbox.Dependencies = rawDependencyBlocks(
		rawBlock("go", []string{"true"}, []string{"go.mod"}, rawPolicyBlockOutputs{Dirs: []string{"${HOME}/go"}}),
	)
	raw.Sandbox.Services = rawPolicyBlocks{
		rawBlock("postgres", []string{"true"}, []string{"docker-compose.yml"}, rawPolicyBlockOutputs{Files: []string{"${HOME}/go/service.state"}}),
	}

	_, err := Compile(raw)
	if err == nil {
		t.Fatal("expected compile to reject overlapping dependency and service outputs")
	}
	if !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCompilePreservesLiteralBlockEnvValues(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	raw.Sandbox.Dependencies = rawDependencyBlocks(
		rawPolicyBlock{
			Name:    "go",
			Command: rawDependencyCommandSpec{"true"},
			Inputs:  rawPolicyBlockInputs{Files: []string{"go.mod"}},
			Env: map[string]string{
				"EMPTY":      "",
				"GOCACHE":    "${HOME}/.cache/go-build",
				"GOPROXY":    "https://proxy.golang.org,direct",
				"RAW_PATH":   "a/../b",
				"TRAILING":   "value ",
				"UNEXPANDED": "$CACHE/go",
				"WORKDIR":    "${WORKSPACE}",
				"WORKCACHE":  "$WORKSPACE/.cache",
			},
			Outputs: rawPolicyBlockOutputs{Dirs: []string{"${HOME}/go/pkg/mod"}},
		},
	)

	compiled, err := Compile(raw)
	if err != nil {
		t.Fatalf("Compile returned error: %v", err)
	}
	env := compiled.Dependencies.Blocks[0].Env
	for key, want := range map[string]string{
		"EMPTY":      "",
		"GOCACHE":    guestenv.DefaultHome + "/.cache/go-build",
		"GOPROXY":    "https://proxy.golang.org,direct",
		"RAW_PATH":   "a/../b",
		"TRAILING":   "value ",
		"UNEXPANDED": "$CACHE/go",
		"WORKDIR":    "/workspace",
		"WORKCACHE":  "/workspace/.cache",
	} {
		if got := env[key]; got != want {
			t.Fatalf("unexpected env %s: got %q want %q", key, got, want)
		}
	}
}

func TestCompileNormalizesDependencyCommandString(t *testing.T) {
	t.Parallel()

	var raw rawPolicy
	if err := yaml.Unmarshal([]byte(`
version: 1
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  dependencies:
    - name: gems
      command: bundle install
      inputs:
        files: [Gemfile.lock]
      outputs:
        dirs: ["${HOME}/.bundle"]
  network:
    default: deny
`), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	compiled, err := Compile(raw)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(compiled.Dependencies.Blocks) != 1 {
		t.Fatalf("unexpected dependency block count: got %d want 1", len(compiled.Dependencies.Blocks))
	}
	if got, want := compiled.Dependencies.Blocks[0].Command, []string{"sh", "-lc", "bundle install"}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("unexpected dependency command: got %v want %v", got, want)
	}
	if got, want := compiled.Dependencies.KeyFiles, []string{"Gemfile.lock"}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("unexpected dependency key files: got %v want %v", got, want)
	}
}

func TestUnmarshalRejectsOldDependencyObjectShape(t *testing.T) {
	t.Parallel()

	var raw rawPolicy
	err := yaml.Unmarshal([]byte(`
version: 1
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  dependencies:
    command: bundle install
    key:
      files: [Gemfile.lock]
  network:
    default: deny
`), &raw)
	if err == nil {
		t.Fatal("expected unmarshal to reject old dependency object shape")
	}
}

func TestCompileAcceptsAliasedDependencyCommandSequence(t *testing.T) {
	t.Parallel()

	var raw rawPolicy
	if err := yaml.Unmarshal([]byte(`
version: 1
common_cmd: &deps [mise, exec, --, go, mod, download]
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  dependencies:
    - name: go-modules
      command: *deps
      inputs:
        files: [go.mod, go.sum]
      outputs:
        dirs: [~/go/pkg/mod]
  network:
    default: deny
`), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	compiled, err := Compile(raw)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(compiled.Dependencies.Blocks) != 1 {
		t.Fatalf("unexpected dependency block count: got %d want 1", len(compiled.Dependencies.Blocks))
	}
	if got, want := compiled.Dependencies.Blocks[0].Command, []string{"mise", "exec", "--", "go", "mod", "download"}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("unexpected dependency command: got %v want %v", got, want)
	}
}

func TestCompileRejectsDependencyInputsWithoutCommand(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	raw.Sandbox.Dependencies = rawDependencyBlocks(
		rawPolicyBlock{
			Name: "go-modules",
			Inputs: rawPolicyBlockInputs{
				Files: []string{"go.sum"},
			},
			Outputs: rawPolicyBlockOutputs{
				Dirs: []string{"${HOME}/go/pkg/mod"},
			},
		},
	)

	_, err := Compile(raw)
	if err == nil {
		t.Fatal("expected compile to reject dependency inputs without a command")
	}
	if !strings.Contains(err.Error(), "sandbox.dependencies[0].command") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCompileRejectsDependencyBlockWithoutOutputs(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	raw.Sandbox.Dependencies = rawDependencyBlocks(
		rawBlock("go-modules", []string{"go", "mod", "download"}, []string{"go.sum"}, rawPolicyBlockOutputs{}),
	)

	_, err := Compile(raw)
	if err == nil {
		t.Fatal("expected compile to reject dependency block without outputs")
	}
	if !strings.Contains(err.Error(), "sandbox.dependencies[0].outputs") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUnmarshalRejectsNonStringOrSequenceDependencyCommand(t *testing.T) {
	t.Parallel()

	var raw rawPolicy
	err := yaml.Unmarshal([]byte(`
version: 1
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  dependencies:
    - name: go-modules
      command: false
      inputs:
        files: [go.mod]
      outputs:
        dirs: ["${HOME}/go/pkg/mod"]
  network:
    default: deny
`), &raw)
	if err == nil {
		t.Fatal("expected unmarshal to reject non-string dependency command")
	}
	if !strings.Contains(err.Error(), "command must be a string or sequence") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCompileNormalizesRunBeforeCommand(t *testing.T) {
	t.Parallel()

	var raw rawPolicy
	if err := yaml.Unmarshal([]byte(`
version: 1
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  run:
    before: |
      docker compose up -d postgres valkey
      bin/rails db:prepare
  network:
    default: deny
`), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	compiled, err := Compile(raw)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if got, want := compiled.Run.Before, []string{"sh", "-lc", "docker compose up -d postgres valkey\nbin/rails db:prepare"}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("unexpected run.before command: got %v want %v", got, want)
	}
}

func TestUnmarshalRejectsNonStringRunBefore(t *testing.T) {
	t.Parallel()

	var raw rawPolicy
	err := yaml.Unmarshal([]byte(`
version: 1
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  run:
    before: false
  network:
    default: deny
`), &raw)
	if err == nil {
		t.Fatal("expected unmarshal to reject non-string run.before")
	}
	if !strings.Contains(err.Error(), "command must be a string") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUnmarshalRejectsSequenceRunBefore(t *testing.T) {
	t.Parallel()

	var raw rawPolicy
	err := yaml.Unmarshal([]byte(`
version: 1
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  run:
    before: [bin/rails, db:prepare]
  network:
    default: deny
`), &raw)
	if err == nil {
		t.Fatal("expected unmarshal to reject sequence run.before")
	}
	if !strings.Contains(err.Error(), "command must be a string") {
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

func TestReadPolicyRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), PrimaryPolicyPath)
	if err := os.WriteFile(path, []byte(`
version: 1
unknown_top_level: true
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  network:
    default: deny
`), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}

	_, err := readPolicy(path)
	if err == nil {
		t.Fatal("expected unknown policy field to be rejected")
	}
	if !strings.Contains(err.Error(), "unknown_top_level") {
		t.Fatalf("expected error to name unknown field, got %v", err)
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

func TestLoadExposeNormalizesConfiguredHTTPS(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, PrimaryPolicyPath), []byte(`
version: 1
expose:
  https:
    base: "{sandbox_id}.LOCALHOST"
    routes:
      - port: 3000
        hosts:
          - "{base}"
          - "{container_id}.localhost"
          - "*.{base}"
          - "*.{base}"
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  network:
    default: deny
`), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}

	cfg, source, err := Loader{}.LoadExpose(dir)
	if err != nil {
		t.Fatalf("LoadExpose returned error: %v", err)
	}
	if got, want := source, filepath.Join(dir, PrimaryPolicyPath); got != want {
		t.Fatalf("unexpected source: got %q want %q", got, want)
	}
	if got, want := cfg.HTTPS.Base, "{sandbox_id}.localhost"; got != want {
		t.Fatalf("unexpected https base: got %q want %q", got, want)
	}
	if got, want := len(cfg.HTTPS.Routes), 1; got != want {
		t.Fatalf("unexpected route count: got %d want %d", got, want)
	}
	if got, want := cfg.HTTPS.Routes[0].Port, 3000; got != want {
		t.Fatalf("unexpected route port: got %d want %d", got, want)
	}
	if got, want := cfg.HTTPS.Routes[0].Hosts, []string{"{base}", "*.{base}"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected route hosts: got %v want %v", got, want)
	}
}

func TestCompileValidatesExposeWithoutChangingHash(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	baseline, err := Compile(raw)
	if err != nil {
		t.Fatalf("Compile baseline returned error: %v", err)
	}

	raw.Expose.HTTPS = rawExposeHTTPS{
		Base: "{sandbox_id}.localhost",
		Routes: []rawExposeHTTPSRoute{{
			Port:  3000,
			Hosts: []string{"{base}", "*.{base}", "*.*.{base}"},
		}},
	}
	withExpose, err := Compile(raw)
	if err != nil {
		t.Fatalf("Compile with expose returned error: %v", err)
	}
	if got, want := withExpose.Hash, baseline.Hash; got != want {
		t.Fatalf("expected expose config to stay out of policy hash: got %q want %q", got, want)
	}

	raw.Expose.HTTPS.Routes[0].Port = 0
	if _, err := Compile(raw); err == nil {
		t.Fatal("expected Compile to validate expose config")
	}
}

func TestCompileValidatesConfiguredHTTPSHosts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		host string
		want string
	}{
		{name: "external host", host: "google.com", want: "localhost"},
		{name: "unscoped wildcard", host: "*.*.localhost", want: "concrete localhost subdomain"},
		{
			name: "expanded sandbox id exceeds label limit",
			host: strings.Repeat("a", 40) + "-{sandbox_id}.localhost",
			want: "longer than 63 characters",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := baseRawPolicy()
			raw.Expose.HTTPS = rawExposeHTTPS{
				Base: "{sandbox_id}.localhost",
				Routes: []rawExposeHTTPSRoute{{
					Port:  3000,
					Hosts: []string{tt.host},
				}},
			}

			_, err := Compile(raw)
			if err == nil {
				t.Fatal("expected Compile to reject invalid expose host")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestCompileRejectsDuplicateExpandedConfiguredHTTPSHostsAcrossRoutes(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	raw.Expose.HTTPS = rawExposeHTTPS{
		Base: "{sandbox_id}.localhost",
		Routes: []rawExposeHTTPSRoute{
			{Port: 3000, Hosts: []string{"{base}"}},
			{Port: 4000, Hosts: []string{"{container_id}.localhost"}},
		},
	}

	_, err := Compile(raw)
	if err == nil {
		t.Fatal("expected Compile to reject duplicate expanded expose host")
	}
	if !strings.Contains(err.Error(), "duplicates configured host") {
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
		Docker: &cleanroomv1.PolicyDocker{
			Required: true,
		},
	})
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	if !compiled.Docker.Required {
		t.Fatal("expected docker service requirement from proto policy")
	}
}

func TestFromProtoRejectsOverlappingDependencyAndServiceOutputs(t *testing.T) {
	t.Parallel()

	_, err := FromProto(&cleanroomv1.Policy{
		Version:        1,
		ImageRef:       validImageRef,
		NetworkDefault: "deny",
		Dependencies: &cleanroomv1.PolicyDependencies{
			Blocks: []*cleanroomv1.PolicyBlock{protoBlock(
				"go",
				[]string{"true"},
				[]string{"go.mod"},
				&cleanroomv1.PolicyBlockOutputs{Dirs: []string{guestenv.DefaultHome + "/go"}},
			)},
		},
		Services: &cleanroomv1.PolicyServices{
			Blocks: []*cleanroomv1.PolicyBlock{protoBlock(
				"postgres",
				[]string{"true"},
				[]string{"docker-compose.yml"},
				&cleanroomv1.PolicyBlockOutputs{Files: []string{guestenv.DefaultHome + "/go/service.state"}},
			)},
		},
	})
	if err == nil {
		t.Fatal("expected FromProto to reject overlapping dependency and service outputs")
	}
	if !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("unexpected error: %v", err)
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

func TestFromProtoRejectsMixedAllowAndStageNetwork(t *testing.T) {
	t.Parallel()

	_, err := FromProto(&cleanroomv1.Policy{
		Version:        1,
		ImageRef:       validImageRef,
		NetworkDefault: "deny",
		Allow: []*cleanroomv1.PolicyAllowRule{
			{Host: "proxy.golang.org", Ports: []int32{443}},
		},
		NetworkStages: &cleanroomv1.PolicyNetworkStages{
			Workspace: &cleanroomv1.PolicyNetwork{
				Allow: []*cleanroomv1.PolicyAllowRule{
					{Host: "github.com", Ports: []int32{443}},
				},
			},
		},
	})
	if err == nil {
		t.Fatal("expected FromProto to reject mixed global and stage-local allowlists")
	}
	if !strings.Contains(err.Error(), "policy allow") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFromProtoRejectsAllowDefaultWithStageNetwork(t *testing.T) {
	t.Parallel()

	_, err := FromProto(&cleanroomv1.Policy{
		Version:        1,
		ImageRef:       validImageRef,
		NetworkDefault: "allow",
		NetworkStages: &cleanroomv1.PolicyNetworkStages{
			Execution: &cleanroomv1.PolicyNetwork{},
		},
	})
	if err == nil {
		t.Fatal("expected FromProto to reject allow default with stage-local network blocks")
	}
	if !strings.Contains(err.Error(), "network_default") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCompiledPolicyProtoRoundTripPreservesStageScopedNetwork(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	raw.Repository = &rawRepository{
		Network: &rawStageNetworkConfig{
			Allow: rawAllowRules{{Host: "github.com", Ports: []int{443}}},
		},
	}
	raw.Sandbox.Network.Dependencies = &rawStageNetworkConfig{
		Allow: rawAllowRules{{Host: "proxy.golang.org", Ports: []int{443}}},
	}
	raw.Sandbox.Network.Execution = &rawStageNetworkConfig{}

	compiled, err := Compile(raw)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	roundTripped, err := FromProto(compiled.ToProto())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	if !roundTripped.HasStageScopedNetwork() {
		t.Fatal("expected stage-scoped network after round trip")
	}
	if !roundTripped.AllowsForStage(NetworkStageWorkspace, "github.com", 443) {
		t.Fatal("expected workspace allowlist after round trip")
	}
	if !roundTripped.AllowsForStage(NetworkStageDependencies, "proxy.golang.org", 443) {
		t.Fatal("expected dependencies allowlist after round trip")
	}
	if roundTripped.NetworkStages.Execution == nil {
		t.Fatal("expected explicit empty execution stage after round trip")
	}
	if roundTripped.AllowsForStage(NetworkStageExecution, "github.com", 443) {
		t.Fatal("did not expect execution stage to inherit workspace allowlist after round trip")
	}
}

func TestCompiledPolicyProtoRoundTripPreservesDependenciesAndServices(t *testing.T) {
	t.Parallel()

	raw := baseRawPolicy()
	raw.Sandbox.Docker.Required = true
	raw.Sandbox.Dependencies = rawDependencyBlocks(
		rawBlock("go-modules", []string{"go", "mod", "download"}, []string{"go.mod", "go.sum"}, rawPolicyBlockOutputs{
			Dirs: []string{"${HOME}/go/pkg/mod"},
		}),
	)
	raw.Sandbox.Services = rawPolicyBlocks{
		rawBlock("postgres", []string{"docker", "compose", "up", "-d", "postgres"}, []string{"docker-compose.yml"}, rawPolicyBlockOutputs{
			Dirs: []string{"/var/lib/cleanroom/services/postgres"},
		}),
	}
	raw.Sandbox.Run.Before = rawShellCommandSpec{"sh", "-lc", "bin/rails db:prepare"}
	vcpus := int64(6)
	memory := bytesize.Size(12 << 30)
	disk := bytesize.Size(18 << 30)
	raw.Sandbox.Resources.VCPUs = &vcpus
	raw.Sandbox.Resources.Memory = &memory
	raw.Sandbox.Resources.Disk = &disk
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
	if got, want := len(roundTripped.Dependencies.Blocks), len(compiled.Dependencies.Blocks); got != want {
		t.Fatalf("unexpected dependency block count after round trip: got %d want %d", got, want)
	}
	if got, want := roundTripped.Docker.Required, compiled.Docker.Required; got != want {
		t.Fatalf("unexpected docker requirement after round trip: got %t want %t", got, want)
	}
	if got, want := strings.Join(roundTripped.Services.Command, "\x00"), strings.Join(compiled.Services.Command, "\x00"); got != want {
		t.Fatalf("unexpected services command after round trip: got %q want %q", got, want)
	}
	if got, want := strings.Join(roundTripped.Services.KeyFiles, "\x00"), strings.Join(compiled.Services.KeyFiles, "\x00"); got != want {
		t.Fatalf("unexpected services key files after round trip: got %q want %q", got, want)
	}
	if got, want := strings.Join(roundTripped.Run.Before, "\x00"), strings.Join(compiled.Run.Before, "\x00"); got != want {
		t.Fatalf("unexpected run.before after round trip: got %q want %q", got, want)
	}
	if roundTripped.Resources == nil {
		t.Fatal("expected resources after round trip")
	}
	if got, want := *roundTripped.Resources, *compiled.Resources; got != want {
		t.Fatalf("unexpected resources after round trip: got %#v want %#v", got, want)
	}
}

func TestFromProtoRejectsNegativeResourceRequirements(t *testing.T) {
	t.Parallel()

	_, err := FromProto(&cleanroomv1.Policy{
		Version:        1,
		ImageRef:       validImageRef,
		NetworkDefault: "deny",
		Resources: &cleanroomv1.PolicyResources{
			MemoryBytes: -1,
		},
	})
	if err == nil {
		t.Fatal("expected FromProto to reject negative resource requirement")
	}
	if !strings.Contains(err.Error(), "resources.memory_bytes") {
		t.Fatalf("unexpected error: %v", err)
	}
}
