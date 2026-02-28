package client

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
)

func TestNewFromEnvUsesCLEANROOMHOST(t *testing.T) {
	host := startIntegrationServer(t)
	t.Setenv("CLEANROOM_HOST", host)

	c, err := NewFromEnv()
	if err != nil {
		t.Fatalf("NewFromEnv returned error: %v", err)
	}

	resp, err := c.ListSandboxes(context.Background(), &ListSandboxesRequest{})
	if err != nil {
		t.Fatalf("ListSandboxes returned error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected response")
	}
}

func TestMustPanicsOnError(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic")
		}
	}()

	_ = Must((*Client)(nil), errors.New("boom"))
}

func TestPolicyFromAllowlist(t *testing.T) {
	policy := PolicyFromAllowlist(
		"ghcr.io/buildkite/cleanroom-base/alpine@sha256:abc",
		"sha256:abc",
		Allow("api.github.com", 443),
		Allow("registry.npmjs.org", 443, 80),
	)

	if policy.GetVersion() != 1 {
		t.Fatalf("unexpected version: got %d", policy.GetVersion())
	}
	if policy.GetNetworkDefault() != "deny" {
		t.Fatalf("unexpected network default: got %q", policy.GetNetworkDefault())
	}
	if len(policy.GetAllow()) != 2 {
		t.Fatalf("unexpected allow length: got %d", len(policy.GetAllow()))
	}
	if policy.GetAllow()[0].GetHost() != "api.github.com" {
		t.Fatalf("unexpected first allow host: %q", policy.GetAllow()[0].GetHost())
	}
}

func TestEnsureSandboxReusesKey(t *testing.T) {
	host := startIntegrationServer(t)
	c, err := New(host)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	opts := EnsureSandboxOptions{
		Backend: "firecracker",
		Policy:  testPolicy(),
	}

	first, err := c.EnsureSandbox(ctx, "thread:abc", opts)
	if err != nil {
		t.Fatalf("first EnsureSandbox returned error: %v", err)
	}
	if !first.Created {
		t.Fatal("expected first call to create sandbox")
	}

	second, err := c.EnsureSandbox(ctx, "thread:abc", opts)
	if err != nil {
		t.Fatalf("second EnsureSandbox returned error: %v", err)
	}
	if second.Created {
		t.Fatal("expected second call to reuse sandbox")
	}
	if first.ID != second.ID {
		t.Fatalf("expected same sandbox id, got %q and %q", first.ID, second.ID)
	}
}

func TestEnsureSandboxAcceptsExplicitSandboxID(t *testing.T) {
	host := startIntegrationServer(t)
	c, err := New(host)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	ctx := context.Background()
	created, err := c.CreateSandbox(ctx, &CreateSandboxRequest{Backend: "firecracker", Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	id := created.GetSandbox().GetSandboxId()

	ensured, err := c.EnsureSandbox(ctx, "thread:explicit", EnsureSandboxOptions{SandboxID: id})
	if err != nil {
		t.Fatalf("EnsureSandbox returned error: %v", err)
	}
	if ensured.ID != id {
		t.Fatalf("sandbox id mismatch: got %q want %q", ensured.ID, id)
	}
	if ensured.Created {
		t.Fatal("expected ensured sandbox to be marked reused")
	}
}

func TestExecAndWait(t *testing.T) {
	host := startIntegrationServer(t)
	c, err := New(host)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sb, err := c.EnsureSandbox(ctx, "thread:exec", EnsureSandboxOptions{
		Backend: "firecracker",
		Policy:  testPolicy(),
	})
	if err != nil {
		t.Fatalf("EnsureSandbox returned error: %v", err)
	}

	var stdout bytes.Buffer
	result, err := c.ExecAndWait(ctx, sb.ID, []string{"echo", "hello"}, ExecOptions{Stdout: &stdout})
	if err != nil {
		t.Fatalf("ExecAndWait returned error: %v", err)
	}
	if result.Status != ExecutionStatus_EXECUTION_STATUS_SUCCEEDED {
		t.Fatalf("unexpected status: %v", result.Status)
	}
	if result.ExitCode != 0 {
		t.Fatalf("unexpected exit code: %d", result.ExitCode)
	}
	if !strings.Contains(result.Stdout, "hello from cleanroom") {
		t.Fatalf("expected stdout in result, got %q", result.Stdout)
	}
	if !strings.Contains(stdout.String(), "hello from cleanroom") {
		t.Fatalf("expected stdout writer to receive output, got %q", stdout.String())
	}
}

func TestErrCode(t *testing.T) {
	if got := ErrCode(connect.NewError(connect.CodeNotFound, errors.New("unknown sandbox \"x\""))); got != ErrorCodeNotFound {
		t.Fatalf("unexpected code for not found: %q", got)
	}

	if got := ErrCode(connect.NewError(connect.CodeInternal, errors.New("backend_capability_mismatch: denied"))); got != ErrorCodeBackendCapabilityMismatch {
		t.Fatalf("unexpected code for backend mismatch: %q", got)
	}
}
