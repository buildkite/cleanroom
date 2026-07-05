package bake

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
)

func testPolicy() *policy.CompiledPolicy {
	return &policy.CompiledPolicy{
		ImageRef: testImageRef,
		Hash:     "policy-hash",
		Warmup:   []string{"apk add git", "echo warmed"},
	}
}

func TestKeyIsDeterministicAndInputSensitive(t *testing.T) {
	compiled := testPolicy()
	facts := GitFacts{Commit: "abc", HasGit: true}

	base := Key(compiled, facts)
	if base != Key(compiled, facts) {
		t.Fatal("bake key is not deterministic")
	}
	other := *compiled
	other.Hash = "different"
	if Key(&other, facts) == base {
		t.Fatal("bake key ignores policy hash")
	}
	if Key(compiled, GitFacts{Commit: "def", HasGit: true}) == base {
		t.Fatal("bake key ignores commit")
	}
	if Key(compiled, GitFacts{Commit: "abc", Dirty: true, HasGit: true}) == base {
		t.Fatal("bake key ignores dirty state")
	}
}

type fakeRunner struct {
	version     string
	calls       []string
	createArgs  []string
	execs       []string
	annotations map[string]string
	inspectErr  error
	execErr     error
}

func (f *fakeRunner) Version() (string, error) { return f.version, nil }

func (f *fakeRunner) Create(name string, args []string) error {
	f.calls = append(f.calls, "create "+name)
	f.createArgs = args
	return nil
}

func (f *fakeRunner) CopyIn(name, hostPath, guestPath string) error {
	f.calls = append(f.calls, "copy-in "+hostPath+" "+guestPath)
	return nil
}

func (f *fakeRunner) ExecShell(name, command string) error {
	f.calls = append(f.calls, "exec")
	f.execs = append(f.execs, command)
	return f.execErr
}

func (f *fakeRunner) Suspend(name, outDir string) error {
	f.calls = append(f.calls, "suspend "+outDir)
	return nil
}

func (f *fakeRunner) InspectAnnotations(sporeDir string) (map[string]string, error) {
	f.calls = append(f.calls, "inspect")
	return f.annotations, f.inspectErr
}

func (f *fakeRunner) Remove(name string) error {
	f.calls = append(f.calls, "remove "+name)
	return nil
}

func testOptions(t *testing.T, runner Runner) Options {
	t.Helper()
	dir := t.TempDir()
	return Options{
		Dir:          dir,
		PolicySource: filepath.Join(dir, "cleanroom.yaml"),
		Out:          filepath.Join(dir, "out.spore"),
		Version:      "v0.1.0",
		Runner:       runner,
		Log:          io.Discard,
	}
}

func TestRunBakesEndToEnd(t *testing.T) {
	compiled := testPolicy()
	runner := &fakeRunner{version: "0.3.1"}
	options := testOptions(t, runner)
	key := Key(compiled, CollectGitFacts(options.Dir))
	runner.annotations = map[string]string{
		AnnotationPrefix + "provenance.version": "1",
		AnnotationPrefix + "bake.key":           key,
	}

	result, err := Run(compiled, options)
	if err != nil {
		t.Fatalf("bake: %v", err)
	}
	if result.UpToDate {
		t.Fatal("fresh bake reported up to date")
	}
	if result.Key != key {
		t.Fatalf("result key = %q, want %q", result.Key, key)
	}

	if !strings.HasPrefix(runner.calls[0], "create cr-bake-"+key[:8]+"-") {
		t.Fatalf("unexpected create call: %v", runner.calls)
	}
	want := []string{
		"copy-in " + options.Dir + " " + GuestWorkspaceDir,
		"exec",
		"exec",
		"suspend " + options.Out,
		"inspect",
	}
	if strings.Join(runner.calls[1:], "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls = %v, want %v", runner.calls, want)
	}

	joinedArgs := strings.Join(runner.createArgs, " ")
	if !strings.Contains(joinedArgs, "--image "+testImageRef) {
		t.Fatalf("create args missing image: %v", runner.createArgs)
	}
	if !strings.Contains(joinedArgs, AnnotationPrefix+"bake.key="+key) {
		t.Fatalf("create args missing bake key annotation: %v", runner.createArgs)
	}
	for i, step := range compiled.Warmup {
		want := "cd " + GuestWorkspaceDir + " && " + step
		if runner.execs[i] != want {
			t.Fatalf("warmup %d = %q, want %q", i, runner.execs[i], want)
		}
	}
}

func TestRunDestroysBuilderOnWarmupFailure(t *testing.T) {
	compiled := testPolicy()
	runner := &fakeRunner{version: "0.3.1", execErr: errors.New("guest command failed")}
	options := testOptions(t, runner)

	_, err := Run(compiled, options)
	if err == nil || !strings.Contains(err.Error(), "warmup step 1") {
		t.Fatalf("expected warmup failure, got %v", err)
	}
	last := runner.calls[len(runner.calls)-1]
	if !strings.HasPrefix(last, "remove cr-bake-") {
		t.Fatalf("expected builder removal after failure, calls = %v", runner.calls)
	}
}

func TestRunNoOpsWhenArtifactMatches(t *testing.T) {
	compiled := testPolicy()
	runner := &fakeRunner{version: "0.3.1"}
	options := testOptions(t, runner)
	key := Key(compiled, CollectGitFacts(options.Dir))
	runner.annotations = map[string]string{
		AnnotationPrefix + "provenance.version": ProvenanceVersion,
		AnnotationPrefix + "bake.key":           key,
	}
	if err := os.MkdirAll(options.Out, 0o755); err != nil {
		t.Fatalf("create out dir: %v", err)
	}

	result, err := Run(compiled, options)
	if err != nil {
		t.Fatalf("bake: %v", err)
	}
	if !result.UpToDate {
		t.Fatal("expected up-to-date no-op")
	}
	for _, call := range runner.calls {
		if strings.HasPrefix(call, "create") {
			t.Fatalf("no-op bake created a builder: %v", runner.calls)
		}
	}
}

func TestRunRefusesStaleArtifact(t *testing.T) {
	compiled := testPolicy()
	runner := &fakeRunner{version: "0.3.1"}
	options := testOptions(t, runner)
	runner.annotations = map[string]string{
		AnnotationPrefix + "provenance.version": ProvenanceVersion,
		AnnotationPrefix + "bake.key":           "0000stalekey0000",
	}
	if err := os.MkdirAll(options.Out, 0o755); err != nil {
		t.Fatalf("create out dir: %v", err)
	}

	_, err := Run(compiled, options)
	if err == nil || !strings.Contains(err.Error(), "different bake key") {
		t.Fatalf("expected stale-artifact error, got %v", err)
	}
}

func TestRunRequiresMinimumSporeVersion(t *testing.T) {
	compiled := testPolicy()
	runner := &fakeRunner{version: "0.3.0"}

	_, err := Run(compiled, testOptions(t, runner))
	if err == nil || !strings.Contains(err.Error(), "requires spore >= 0.3.1") {
		t.Fatalf("expected version gate error, got %v", err)
	}
}

func TestOutExclusions(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name string
		out  string
		want []string
	}{
		{"inside repo", filepath.Join(dir, "repo.spore"), []string{"repo.spore"}},
		{"nested inside repo", filepath.Join(dir, "dist", "repo.spore"), []string{filepath.Join("dist", "repo.spore")}},
		{"outside repo", filepath.Join(t.TempDir(), "repo.spore"), nil},
		{"out equals dir", dir, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := outExclusions(dir, tc.out)
			if len(got) != len(tc.want) {
				t.Fatalf("outExclusions(%q, %q) = %v, want %v", dir, tc.out, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("outExclusions(%q, %q) = %v, want %v", dir, tc.out, got, tc.want)
				}
			}
		})
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"0.3.1", "0.3.1", 0},
		{"0.3.0", "0.3.1", -1},
		{"0.10.0", "0.9.9", 1},
		{"1.0.0", "0.3.1", 1},
	}
	for _, tc := range tests {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Fatalf("compareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestGatewayCreateArgs(t *testing.T) {
	base := &policy.CompiledPolicy{ImageRef: testImageRef, Hash: "h"}
	mediated := &policy.CompiledPolicy{ImageRef: testImageRef, Hash: "h", Mediation: []string{"svc"}}

	// no mediation, no socket -> no args
	if args, err := gatewayCreateArgs(base, "", false); err != nil || args != nil {
		t.Fatalf("no-mediation: args=%v err=%v", args, err)
	}
	// no mediation, socket given -> fail closed
	if _, err := gatewayCreateArgs(base, "/tmp/x.sock", false); err == nil || !strings.Contains(err.Error(), "requests no mediation") {
		t.Fatalf("socket-without-mediation: %v", err)
	}
	// mediation, no socket -> fail closed
	if _, err := gatewayCreateArgs(mediated, "", true); err == nil || !strings.Contains(err.Error(), "serve them with cleanroom gateway") {
		t.Fatalf("mediation-without-socket: %v", err)
	}
}

func TestGatewayCreateArgsRequiresAllowRuleForDenyByDefault(t *testing.T) {
	mediated := &policy.CompiledPolicy{ImageRef: testImageRef, Hash: "h", Mediation: []string{"svc"}}
	dir, err := os.MkdirTemp("/tmp", "cr-bake-gw-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "gw.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	// no network allow rules -> refuse to enable bare --net (open egress)
	if _, err := gatewayCreateArgs(mediated, socket, false); err == nil || !strings.Contains(err.Error(), "deny-by-default") {
		t.Fatalf("expected deny-by-default refusal, got %v", err)
	}
	// with network already enabled by allow rules -> just the bind-service
	args, err := gatewayCreateArgs(mediated, socket, true)
	if err != nil {
		t.Fatalf("with-allow-rules: %v", err)
	}
	if len(args) != 2 || args[0] != "--bind-service" || !strings.HasPrefix(args[1], "cleanroom-gateway:8170=unix:") {
		t.Fatalf("unexpected args: %v", args)
	}
}
