package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoaderPrefersRootPolicy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, PrimaryPolicyPath), []byte(`
version: 1
sandbox:
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

	_, err := Compile(rawPolicy{
		Version: 1,
		Sandbox: struct {
			Network struct {
				Default string         "yaml:\"default\""
				Allow   []rawAllowRule "yaml:\"allow\""
			} "yaml:\"network\""
		}{
			Network: struct {
				Default string         "yaml:\"default\""
				Allow   []rawAllowRule "yaml:\"allow\""
			}{
				Default: "allow",
			},
		},
	})
	if err == nil {
		t.Fatal("expected compile to fail for default allow")
	}
}

func TestCompileHashStable(t *testing.T) {
	t.Parallel()

	raw := rawPolicy{}
	raw.Version = 1
	raw.Sandbox.Network.Default = "deny"
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

func TestLoadPropagatesPrimaryStatError(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	restricted := filepath.Join(parent, "restricted")
	if err := os.MkdirAll(restricted, 0o700); err != nil {
		t.Fatalf("mkdir restricted root: %v", err)
	}
	if err := os.Chmod(restricted, 0o000); err != nil {
		t.Fatalf("chmod restricted root: %v", err)
	}
	defer os.Chmod(restricted, 0o700)

	loader := Loader{}
	_, _, err := loader.Load(restricted)
	if err == nil {
		t.Fatal("expected load to fail on stat permission error")
	}
	if !strings.Contains(err.Error(), "check policy") {
		t.Fatalf("unexpected error: %v", err)
	}
}
