package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/bake"
	"github.com/buildkite/cleanroom/internal/mediation"
	"github.com/buildkite/cleanroom/internal/policy"
)

func TestSporeRunArgs(t *testing.T) {
	argv := []string{"make", "test"}
	plain := bake.Provenance{}
	if got, want := sporeRunArgs("repo.spore", plain, "", argv), []string{"run", "--from", "repo.spore", "--", "make", "test"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("plain args = %#v, want %#v", got, want)
	}

	mediated := bake.Provenance{GatewayServices: []bake.GatewayService{{
		Name:      "cleanroom-gateway",
		GuestHost: "cleanroom-gateway.spore.internal",
		GuestPort: 8170,
	}}}
	want := []string{"run", "--from", "repo.spore", "--bind-service", "cleanroom-gateway=unix:/tmp/gw.sock", "--", "make", "test"}
	if got := sporeRunArgs("repo.spore", mediated, "/tmp/gw.sock", argv); !reflect.DeepEqual(got, want) {
		t.Fatalf("mediated args = %#v, want %#v", got, want)
	}
}

func TestContentCacheEnvSynthesizesGitAndGoRouting(t *testing.T) {
	prov := bake.Provenance{
		MediationServices: []string{"content-cache"},
		NetworkRules: []bake.NetworkRule{
			{Host: "github.com", Ports: []uint16{443}},
			{Host: "proxy.golang.org", Ports: []uint16{443}},
			{Host: "dl.google.com", Ports: []uint16{443}},
			{Host: "example.com", Ports: []uint16{80}},
		},
	}
	lookup := func(string) (string, bool) { return "", false }
	got := contentCacheEnv(prov, []string{"go", "test"}, lookup)
	base := "http://cleanroom-gateway.spore.internal:8170/services/content-cache"
	want := []string{
		"GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_0=url." + base + "/git/dl.google.com/.insteadOf",
		"GIT_CONFIG_VALUE_0=https://dl.google.com/",
		"GIT_CONFIG_KEY_1=url." + base + "/git/github.com/.insteadOf",
		"GIT_CONFIG_VALUE_1=https://github.com/",
		"GIT_CONFIG_KEY_2=url." + base + "/git/proxy.golang.org/.insteadOf",
		"GIT_CONFIG_VALUE_2=https://proxy.golang.org/",
		"GOPROXY=" + base + "/goproxy,direct",
		"MISE_GO_DOWNLOAD_MIRROR=" + base + "/fetch/dl.google.com/go",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("content cache env = %#v, want %#v", got, want)
	}
}

func TestContentCacheEnvRequiresServiceAndRespectsExplicitGoEnv(t *testing.T) {
	prov := bake.Provenance{
		NetworkRules: []bake.NetworkRule{{Host: "proxy.golang.org", Ports: []uint16{443}}},
	}
	lookup := func(string) (string, bool) { return "", false }
	if got := contentCacheEnv(prov, []string{"go", "test"}, lookup); len(got) != 0 {
		t.Fatalf("content cache env without service = %#v, want none", got)
	}

	prov.MediationServices = []string{"content-cache"}
	argv := []string{"/usr/bin/env", "GOPROXY=https://proxy.example,direct", "go", "test"}
	got := strings.Join(contentCacheEnv(prov, argv, lookup), "\n")
	if strings.Contains(got, "GOPROXY=") {
		t.Fatalf("content cache env overrode explicit GOPROXY: %q", got)
	}
	if !strings.Contains(got, "GIT_CONFIG_COUNT=1") {
		t.Fatalf("content cache env did not keep git routing: %q", got)
	}
}

func TestWrapArgvWithEnv(t *testing.T) {
	got := wrapArgvWithEnv([]string{"go", "test"}, []string{"GOPROXY=http://cache,direct"})
	want := []string{"/usr/bin/env", "GOPROXY=http://cache,direct", "go", "test"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wrapped argv = %#v, want %#v", got, want)
	}
}

func TestContentCacheGatewayConfigGrantsCurrentPolicyHash(t *testing.T) {
	hash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	path, err := writeContentCacheGatewayConfig(t.TempDir(), hash)
	if err != nil {
		t.Fatalf("write content-cache gateway config: %v", err)
	}
	config, err := mediation.LoadConfig(path)
	if err != nil {
		t.Fatalf("load generated config: %v", err)
	}
	scope, err := mediation.ResolveScope(config, []string{"content-cache"}, mediation.LineageFacts{PolicyHash: hash})
	if err != nil {
		t.Fatalf("resolve generated config: %v", err)
	}
	if got, want := scope["content-cache"].Upstream, "http://"+defaultContentCacheListen; got != want {
		t.Fatalf("content-cache upstream = %q, want %q", got, want)
	}
	if _, err := mediation.ResolveScope(config, []string{"content-cache"}, mediation.LineageFacts{PolicyHash: "different"}); err == nil {
		t.Fatal("expected generated config to reject a different policy hash")
	}
}

func TestRunCommandAuditsAndRunsPlainSpore(t *testing.T) {
	repo := t.TempDir()
	runGitCLI(t, repo, "init")
	runGitCLI(t, repo, "config", "user.email", "cleanroom-test@example.com")
	runGitCLI(t, repo, "config", "user.name", "Cleanroom Test")
	if err := os.WriteFile(filepath.Join(repo, "cleanroom.yaml"), []byte("policy\n"), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	runGitCLI(t, repo, "add", "cleanroom.yaml")
	runGitCLI(t, repo, "-c", "commit.gpgsign=false", "commit", "-m", "initial")

	compiled := &policy.CompiledPolicy{
		ImageRef:    "ghcr.io/x@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ImageDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Hash:        "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}

	sporeDir := filepath.Join(repo, "repo.spore")
	if err := os.MkdirAll(sporeDir, 0o755); err != nil {
		t.Fatalf("mkdir spore dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sporeDir, "manifest.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write spore manifest: %v", err)
	}
	facts := bake.CollectGitFactsExcluding(repo, bake.ArtifactExclusions(repo, sporeDir))
	annotations := map[string]string{
		bake.AnnotationPrefix + "provenance.version":   bake.ProvenanceVersion,
		bake.AnnotationPrefix + "bake.key":             bake.Key(compiled, facts),
		bake.AnnotationPrefix + "policy.hash":          compiled.Hash,
		bake.AnnotationPrefix + "image.ref":            compiled.ImageRef,
		bake.AnnotationPrefix + "image.digest":         compiled.ImageDigest,
		bake.AnnotationPrefix + "workspace.dir":        repo,
		bake.AnnotationPrefix + "workspace.git.commit": facts.Commit,
		bake.AnnotationPrefix + "workspace.git.dirty":  "false",
	}
	inspectJSON, err := json.Marshal(map[string]any{"annotations": annotations})
	if err != nil {
		t.Fatalf("encode inspect payload: %v", err)
	}

	binDir := t.TempDir()
	fakeSpore := filepath.Join(binDir, "spore")
	payloadPath := filepath.Join(binDir, "inspect.json")
	argsPath := filepath.Join(binDir, "args.txt")
	if err := os.WriteFile(payloadPath, inspectJSON, 0o644); err != nil {
		t.Fatalf("write inspect payload: %v", err)
	}
	script := `#!/bin/sh
if [ "$1" = "--json" ] && [ "$2" = "inspect" ]; then
  cat "` + payloadPath + `"
  exit 0
fi
printf '%s\n' "$@" > "` + argsPath + `"
`
	if err := os.WriteFile(fakeSpore, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake spore: %v", err)
	}

	stdout, _ := makeStdoutCapture(t)
	cmd := &RunCommand{SporeDir: sporeDir, Dir: repo, Spore: fakeSpore, Argv: []string{"--", "make", "test"}}
	err = cmd.Run(&runtimeContext{
		CWD:    repo,
		Stdout: stdout,
		Loader: stubLoader{compiled: compiled},
	})
	if err != nil {
		t.Fatalf("run plain spore: %v", err)
	}
	rawArgs, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read fake spore args: %v", err)
	}
	if got, want := strings.TrimSpace(string(rawArgs)), "run\n--from\n"+sporeDir+"\n--\nmake\ntest"; got != want {
		t.Fatalf("spore args = %q, want %q", got, want)
	}
}
