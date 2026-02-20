package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
