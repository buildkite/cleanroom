package cli

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/sporevm"
)

func TestValidateVMPolicyAllowsMinimalPolicy(t *testing.T) {
	compiled := &policy.CompiledPolicy{
		ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		NetworkDefault: "deny",
	}

	if err := validateVMPolicy(compiled); err != nil {
		t.Fatalf("validate minimal policy: %v", err)
	}
}

func TestValidateVMPolicyAllowsExactNetworkAllow(t *testing.T) {
	compiled := &policy.CompiledPolicy{
		ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		NetworkDefault: "deny",
		Allow: []policy.AllowRule{
			{Host: "github.com", Ports: []int{443}},
		},
	}

	if err := validateVMPolicy(compiled); err != nil {
		t.Fatalf("validate network allow policy: %v", err)
	}
}

func TestValidateVMPolicyRejectsUntranslatedFeatures(t *testing.T) {
	tests := []struct {
		name     string
		policy   *policy.CompiledPolicy
		contains string
	}{
		{
			name: "disk resources",
			policy: &policy.CompiledPolicy{
				ImageRef: "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				Resources: &policy.Resources{
					DiskBytes: 16 << 30,
				},
			},
			contains: "sandbox.resources.disk",
		},
		{
			name: "docker service",
			policy: &policy.CompiledPolicy{
				ImageRef: "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				Docker:   policy.DockerService{Required: true},
			},
			contains: "docker service",
		},
		{
			name: "stage scoped network",
			policy: &policy.CompiledPolicy{
				ImageRef: "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				NetworkStages: &policy.NetworkStagePolicies{
					Execution: &policy.NetworkPolicy{},
				},
			},
			contains: "stage-scoped network",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateVMPolicy(tc.policy)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("expected error containing %q, got %v", tc.contains, err)
			}
		})
	}
}

func TestValidateVMPolicyRejectsMalformedNetworkAllow(t *testing.T) {
	tests := []struct {
		name     string
		rule     policy.AllowRule
		contains string
	}{
		{name: "empty host", rule: policy.AllowRule{Ports: []int{443}}, contains: "include a host"},
		{name: "missing ports", rule: policy.AllowRule{Host: "github.com"}, contains: "at least one port"},
		{name: "bad port", rule: policy.AllowRule{Host: "github.com", Ports: []int{0}}, contains: "port 0"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			compiled := &policy.CompiledPolicy{
				ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				NetworkDefault: "deny",
				Allow:          []policy.AllowRule{tc.rule},
			}

			err := validateVMPolicy(compiled)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("expected error containing %q, got %v", tc.contains, err)
			}
		})
	}
}

func TestVMNetworkRulesTranslatesExactAllows(t *testing.T) {
	compiled := &policy.CompiledPolicy{
		ImageRef: "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Allow: []policy.AllowRule{
			{Host: "github.com", Ports: []int{443, 8443}},
			{Host: "api.github.com", Ports: []int{443}},
		},
	}

	got, err := vmNetworkRules(compiled)
	if err != nil {
		t.Fatalf("network rules: %v", err)
	}
	want := []sporevm.NetworkRule{
		{Host: "github.com", Ports: []uint16{443, 8443}},
		{Host: "api.github.com", Ports: []uint16{443}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected network rules: got %#v want %#v", got, want)
	}
}

func TestVMCreateAnnotationsRecordsCleanroomFacts(t *testing.T) {
	cwd := t.TempDir()
	compiled := &policy.CompiledPolicy{
		ImageRef:    "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ImageDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Hash:        "policy-hash",
	}

	got, err := vmCreateAnnotations(
		&runtimeContext{Version: "v0.1.0"},
		cwd,
		filepath.Join(cwd, "cleanroom.yaml"),
		compiled,
		[]sporevm.NetworkRule{{Host: "github.com", Ports: []uint16{443, 8443}}},
		nil,
	)
	if err != nil {
		t.Fatalf("create annotations: %v", err)
	}

	wantPairs := map[string]string{
		"dev.buildkite.cleanroom.provenance.version": "1",
		"dev.buildkite.cleanroom.version":            "v0.1.0",
		"dev.buildkite.cleanroom.policy.hash":        "policy-hash",
		"dev.buildkite.cleanroom.policy.source":      filepath.Join(cwd, "cleanroom.yaml"),
		"dev.buildkite.cleanroom.image.ref":          compiled.ImageRef,
		"dev.buildkite.cleanroom.image.digest":       compiled.ImageDigest,
		"dev.buildkite.cleanroom.workspace.dir":      cwd,
	}
	for key, want := range wantPairs {
		if got[key] != want {
			t.Fatalf("annotation %s = %q, want %q", key, got[key], want)
		}
	}

	type networkRuleAnnotation struct {
		Host  string   `json:"host"`
		Ports []uint16 `json:"ports"`
	}
	var networkRules []networkRuleAnnotation
	if err := json.Unmarshal([]byte(got["dev.buildkite.cleanroom.network.rules"]), &networkRules); err != nil {
		t.Fatalf("decode network rules annotation: %v", err)
	}
	wantRules := []networkRuleAnnotation{{Host: "github.com", Ports: []uint16{443, 8443}}}
	if !reflect.DeepEqual(networkRules, wantRules) {
		t.Fatalf("network rules annotation = %#v, want %#v", networkRules, wantRules)
	}
}

func TestVMCreateAnnotationsRecordsGitFactsWhenAvailable(t *testing.T) {
	cwd := t.TempDir()
	runGitInRepository(t, cwd, "init")
	runGitInRepository(t, cwd, "config", "user.email", "cleanroom-test@example.com")
	runGitInRepository(t, cwd, "config", "user.name", "Cleanroom Test")
	if err := os.WriteFile(filepath.Join(cwd, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	runGitInRepository(t, cwd, "add", "README.md")
	runGitInRepository(t, cwd, "-c", "commit.gpgsign=false", "commit", "-m", "initial")
	runGitInRepository(t, cwd, "remote", "add", "origin", "https://example.com/acme/repo.git")
	if err := os.WriteFile(filepath.Join(cwd, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write dirty.txt: %v", err)
	}

	got, err := vmCreateAnnotations(nil, cwd, "", &policy.CompiledPolicy{}, nil, nil)
	if err != nil {
		t.Fatalf("create annotations: %v", err)
	}

	wantPairs := map[string]string{
		"dev.buildkite.cleanroom.workspace.git.commit": headCommit(t, cwd),
		"dev.buildkite.cleanroom.workspace.git.remote": "https://example.com/acme/repo.git",
		"dev.buildkite.cleanroom.workspace.git.dirty":  "true",
	}
	for key, want := range wantPairs {
		if got[key] != want {
			t.Fatalf("annotation %s = %q, want %q", key, got[key], want)
		}
	}
	if _, ok := got["dev.buildkite.cleanroom.network.rules"]; ok {
		t.Fatalf("network rules annotation should be omitted when there are no rules")
	}
}

func TestVMGatewayServicesBindsUnixSocket(t *testing.T) {
	base, socketPath := newTestGatewaySocket(t)

	got, err := vmGatewayServices(base, "gateway.sock")
	if err != nil {
		t.Fatalf("gateway services: %v", err)
	}
	want := []sporevm.BoundUnixService{{
		Name:      "cleanroom-gateway",
		GuestHost: "gateway.cleanroom.internal",
		GuestPort: 8170,
		UnixPath:  socketPath,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gateway services = %#v, want %#v", got, want)
	}
}

func TestVMGatewayServicesRejectsNonSocket(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "not-a-socket")
	if err := os.WriteFile(path, []byte("nope\n"), 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	_, err := vmGatewayServices(base, path)
	if err == nil {
		t.Fatal("expected non-socket error")
	}
	if !strings.Contains(err.Error(), "is not a Unix socket") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestVMCreateAnnotationsRecordsGatewayServicesWithoutSocketPath(t *testing.T) {
	cwd := t.TempDir()
	got, err := vmCreateAnnotations(
		nil,
		cwd,
		"",
		&policy.CompiledPolicy{},
		nil,
		[]sporevm.BoundUnixService{{
			Name:      "cleanroom-gateway",
			GuestHost: "gateway.cleanroom.internal",
			GuestPort: 8170,
			UnixPath:  "/tmp/live-cleanroom-gateway.sock",
		}},
	)
	if err != nil {
		t.Fatalf("create annotations: %v", err)
	}

	raw := got["dev.buildkite.cleanroom.gateway.services"]
	if strings.Contains(raw, "/tmp/live-cleanroom-gateway.sock") {
		t.Fatalf("gateway services annotation leaked host socket path: %s", raw)
	}
	type gatewayServiceAnnotation struct {
		Name      string `json:"name"`
		GuestHost string `json:"guest_host"`
		GuestPort uint16 `json:"guest_port"`
	}
	var services []gatewayServiceAnnotation
	if err := json.Unmarshal([]byte(raw), &services); err != nil {
		t.Fatalf("decode gateway services annotation: %v", err)
	}
	want := []gatewayServiceAnnotation{{
		Name:      "cleanroom-gateway",
		GuestHost: "gateway.cleanroom.internal",
		GuestPort: 8170,
	}}
	if !reflect.DeepEqual(services, want) {
		t.Fatalf("gateway services annotation = %#v, want %#v", services, want)
	}
}

func TestVMCleanroomProvenanceFromAnnotationsParsesResumeFacts(t *testing.T) {
	got, err := vmCleanroomProvenanceFromAnnotations(map[string]string{
		"dev.buildkite.cleanroom.provenance.version": "1",
		"dev.buildkite.cleanroom.workspace.dir":      "/work/repo",
		"dev.buildkite.cleanroom.network.rules":      `[{"host":"github.com","ports":[443,8443]}]`,
		"dev.buildkite.cleanroom.gateway.services":   `[{"name":"cleanroom-gateway","guest_host":"gateway.cleanroom.internal","guest_port":8170}]`,
	})
	if err != nil {
		t.Fatalf("parse provenance: %v", err)
	}
	want := vmCleanroomProvenance{
		NetworkRules: []sporevm.NetworkRule{{
			Host:  "github.com",
			Ports: []uint16{443, 8443},
		}},
		GatewayServices: []vmGatewayServiceRequirement{{
			Name:      "cleanroom-gateway",
			GuestHost: "gateway.cleanroom.internal",
			GuestPort: 8170,
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("provenance = %#v, want %#v", got, want)
	}
}

func TestVMCleanroomProvenanceFromAnnotationsRequiresCreateFact(t *testing.T) {
	_, err := vmCleanroomProvenanceFromAnnotations(map[string]string{
		"dev.buildkite.cleanroom.provenance.version": "1",
	})
	if err == nil {
		t.Fatal("expected missing create provenance error")
	}
	if !strings.Contains(err.Error(), "missing Cleanroom create provenance") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestVMCleanroomProvenanceFromAnnotationsRequiresVersion(t *testing.T) {
	_, err := vmCleanroomProvenanceFromAnnotations(nil)
	if err == nil {
		t.Fatal("expected missing provenance error")
	}
	if !strings.Contains(err.Error(), "missing Cleanroom provenance") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestVMResumeGatewayBindingsBindsRequiredService(t *testing.T) {
	base, socketPath := newTestGatewaySocket(t)

	got, err := vmResumeGatewayBindings(base, "gateway.sock", []vmGatewayServiceRequirement{{
		Name:      "cleanroom-gateway",
		GuestHost: "gateway.cleanroom.internal",
		GuestPort: 8170,
	}})
	if err != nil {
		t.Fatalf("resume gateway bindings: %v", err)
	}
	want := []sporevm.BoundUnixServiceBinding{{
		Name:     "cleanroom-gateway",
		UnixPath: socketPath,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resume gateway bindings = %#v, want %#v", got, want)
	}
}

func TestVMResumeGatewayBindingsRequiresSocketForRequiredService(t *testing.T) {
	_, err := vmResumeGatewayBindings("", "", []vmGatewayServiceRequirement{{
		Name:      "cleanroom-gateway",
		GuestHost: "gateway.cleanroom.internal",
		GuestPort: 8170,
	}})
	if err == nil {
		t.Fatal("expected required socket error")
	}
	if !strings.Contains(err.Error(), "requires --gateway-socket") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestVMResumeCommandInspectsProvenanceAndPassesGatewayBinding(t *testing.T) {
	base, socketPath := newTestGatewaySocket(t)
	client := &fakeSporeVMClient{
		inspectResult: sporevm.SporeInspectResult{
			Annotations: map[string]string{
				"dev.buildkite.cleanroom.provenance.version": "1",
				"dev.buildkite.cleanroom.workspace.dir":      base,
				"dev.buildkite.cleanroom.gateway.services":   `[{"name":"cleanroom-gateway","guest_host":"gateway.cleanroom.internal","guest_port":8170}]`,
			},
		},
		resumeResult: sporevm.JSONResult{RawJSON: json.RawMessage(`{"action":"resume"}`)},
	}
	replaceSporeVMClient(t, client)
	stdout := newTempFile(t, "stdout-*")

	err := (&VMResumeCommand{
		SporeDir:      "./captured.spore",
		Name:          "restored",
		GatewaySocket: "gateway.sock",
	}).Run(&runtimeContext{CWD: base, Stdout: stdout})
	if err != nil {
		t.Fatalf("resume command: %v", err)
	}

	if got, want := client.inspectOptions.SporeDir, "./captured.spore"; got != want {
		t.Fatalf("inspect spore dir = %q, want %q", got, want)
	}
	wantResume := sporevm.ResumeNamedOptions{
		SporeDir: "./captured.spore",
		Name:     "restored",
		BoundServiceBindings: []sporevm.BoundUnixServiceBinding{{
			Name:     "cleanroom-gateway",
			UnixPath: socketPath,
		}},
	}
	if !reflect.DeepEqual(client.resumeOptions, wantResume) {
		t.Fatalf("resume options = %#v, want %#v", client.resumeOptions, wantResume)
	}
	if !client.closed {
		t.Fatal("expected resume command to close spore client")
	}
}

func TestVMCaptureAnnotationsRecordsContinueAfter(t *testing.T) {
	got := vmCaptureAnnotations(&runtimeContext{Version: "v0.1.0"})

	wantPairs := map[string]string{
		"dev.buildkite.cleanroom.provenance.version":     "1",
		"dev.buildkite.cleanroom.version":                "v0.1.0",
		"dev.buildkite.cleanroom.capture.continue_after": "true",
	}
	for key, want := range wantPairs {
		if got[key] != want {
			t.Fatalf("annotation %s = %q, want %q", key, got[key], want)
		}
	}
}

type fakeSporeVMClient struct {
	inspectOptions sporevm.InspectSporeOptions
	inspectResult  sporevm.SporeInspectResult
	inspectErr     error
	resumeOptions  sporevm.ResumeNamedOptions
	resumeResult   sporevm.JSONResult
	resumeErr      error
	closed         bool
}

func (f *fakeSporeVMClient) Close() error {
	f.closed = true
	return nil
}

func (f *fakeSporeVMClient) NetworkCapabilities(context.Context) (sporevm.NetworkCapabilities, error) {
	return sporevm.NetworkCapabilities{}, nil
}

func (f *fakeSporeVMClient) InspectSpore(_ context.Context, options sporevm.InspectSporeOptions) (sporevm.SporeInspectResult, error) {
	f.inspectOptions = options
	return f.inspectResult, f.inspectErr
}

func (f *fakeSporeVMClient) CreateNamed(context.Context, sporevm.CreateNamedOptions) (sporevm.JSONResult, error) {
	return sporevm.JSONResult{}, nil
}

func (f *fakeSporeVMClient) ExecNamed(context.Context, sporevm.ExecNamedOptions) (sporevm.JSONResult, error) {
	return sporevm.JSONResult{}, nil
}

func (f *fakeSporeVMClient) ResumeNamed(_ context.Context, options sporevm.ResumeNamedOptions) (sporevm.JSONResult, error) {
	f.resumeOptions = options
	return f.resumeResult, f.resumeErr
}

func (f *fakeSporeVMClient) SnapshotNamed(context.Context, sporevm.SnapshotNamedOptions) (sporevm.JSONResult, error) {
	return sporevm.JSONResult{}, nil
}

func (f *fakeSporeVMClient) RemoveNamed(context.Context, sporevm.RemoveNamedOptions) (sporevm.JSONResult, error) {
	return sporevm.JSONResult{}, nil
}

func replaceSporeVMClient(t *testing.T, client sporevm.Client) {
	t.Helper()
	previous := newSporeVMClient
	newSporeVMClient = func() (sporevm.Client, error) {
		return client, nil
	}
	t.Cleanup(func() {
		newSporeVMClient = previous
	})
}

func newTestGatewaySocket(t *testing.T) (string, string) {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "cleanroom-gateway-test-*")
	if err != nil {
		t.Fatalf("create short temp dir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Fatalf("remove temp dir: %v", err)
		}
	})
	socketPath := filepath.Join(base, "gateway.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on unix socket: %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Fatalf("close unix socket: %v", err)
		}
	})
	return base, socketPath
}

func newTempFile(t *testing.T, pattern string) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), pattern)
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Fatalf("close temp file: %v", err)
		}
	})
	return file
}
