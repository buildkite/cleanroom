package main

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/controlclient"
	"github.com/buildkite/cleanroom/internal/controlserver"
	"github.com/buildkite/cleanroom/internal/controlservice"
	"github.com/buildkite/cleanroom/internal/endpoint"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

type downloadTestAdapter struct {
	downloadedSandboxID string
	downloadedPath      string
	downloadedMaxBytes  int64
	downloadErr         error
	downloadData        []byte
}

func (a *downloadTestAdapter) Name() string { return "firecracker" }

func (a *downloadTestAdapter) Provision(context.Context, backend.ProvisionRequest) error {
	return nil
}

func (a *downloadTestAdapter) Run(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
	return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0}, nil
}

func (a *downloadTestAdapter) Terminate(context.Context, string) error {
	return nil
}

func (a *downloadTestAdapter) DownloadSandboxFile(_ context.Context, sandboxID, path string, maxBytes int64) ([]byte, error) {
	a.downloadedSandboxID = sandboxID
	a.downloadedPath = path
	a.downloadedMaxBytes = maxBytes
	if a.downloadErr != nil {
		return nil, a.downloadErr
	}
	return append([]byte(nil), a.downloadData...), nil
}

type downloadTestLoader struct{}

func (downloadTestLoader) LoadAndCompile(_ string) (*policy.CompiledPolicy, string, error) {
	return &policy.CompiledPolicy{
		Version:        1,
		ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		NetworkDefault: "deny",
	}, "/repo/cleanroom.yaml", nil
}

func (downloadTestLoader) LoadRepository(_ string) (policy.RepositoryConfig, string, error) {
	return policy.RepositoryConfig{}, "/repo/cleanroom.yaml", nil
}

func TestRequestContextAddsDeadlineWhenTimeoutPositive(t *testing.T) {
	t.Helper()

	const timeout = 200 * time.Millisecond
	ctx, cancel := requestContext(timeout)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatalf("expected request context with timeout %s to set a deadline", timeout)
	}

	remaining := time.Until(deadline)
	if remaining <= 0 {
		t.Fatalf("expected deadline to be in the future, got remaining=%s", remaining)
	}
	if remaining > timeout+200*time.Millisecond {
		t.Fatalf("expected deadline close to timeout %s, got remaining=%s", timeout, remaining)
	}
}

func TestRequestContextDisablesDeadlineWhenTimeoutZero(t *testing.T) {
	t.Helper()

	ctx, cancel := requestContext(0)
	defer cancel()

	if _, ok := ctx.Deadline(); ok {
		t.Fatalf("expected timeout=0 to disable request deadline")
	}
}

func TestRunRejectsMissingHost(t *testing.T) {
	t.Helper()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{"--sandbox-id", "sbx_123", "--path", "/tmp/test.txt"}, &stdout, &stderr)
	if got, want := code, 1; got != want {
		t.Fatalf("unexpected exit code: got %d want %d", got, want)
	}
	if !strings.Contains(stderr.String(), "missing --host") {
		t.Fatalf("expected missing host error, got %q", stderr.String())
	}
}

func TestRunRejectsNegativeTimeout(t *testing.T) {
	t.Helper()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{
		"--host", "http://127.0.0.1:8080",
		"--sandbox-id", "sbx_123",
		"--path", "/tmp/test.txt",
		"--timeout", "-1s",
	}, &stdout, &stderr)
	if got, want := code, 1; got != want {
		t.Fatalf("unexpected exit code: got %d want %d", got, want)
	}
	if !strings.Contains(stderr.String(), "invalid --timeout") {
		t.Fatalf("expected invalid timeout error, got %q", stderr.String())
	}
}

func TestRunDownloadsSandboxFile(t *testing.T) {
	t.Helper()

	adapter := &downloadTestAdapter{downloadData: []byte("payload\n")}
	host := startDownloadSandboxServer(t, adapter)
	sandboxID := mustCreateDownloadTestSandbox(t, host)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{
		"--host", host,
		"--sandbox-id", sandboxID,
		"--path", "/tmp/persist.txt",
		"--max-bytes", "4096",
	}, &stdout, &stderr)
	if got, want := code, 0; got != want {
		t.Fatalf("unexpected exit code: got %d want %d (stderr=%q)", got, want, stderr.String())
	}
	if got, want := stdout.String(), "payload\n"; got != want {
		t.Fatalf("unexpected stdout: got %q want %q", got, want)
	}
	if got, want := adapter.downloadedSandboxID, sandboxID; got != want {
		t.Fatalf("unexpected sandbox id: got %q want %q", got, want)
	}
	if got, want := adapter.downloadedPath, "/tmp/persist.txt"; got != want {
		t.Fatalf("unexpected download path: got %q want %q", got, want)
	}
	if got, want := adapter.downloadedMaxBytes, int64(4096); got != want {
		t.Fatalf("unexpected max bytes: got %d want %d", got, want)
	}
}

func TestRunReportsDownloadErrors(t *testing.T) {
	t.Helper()

	adapter := &downloadTestAdapter{downloadErr: errors.New("boom")}
	host := startDownloadSandboxServer(t, adapter)
	sandboxID := mustCreateDownloadTestSandbox(t, host)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{
		"--host", host,
		"--sandbox-id", sandboxID,
		"--path", "/tmp/persist.txt",
	}, &stdout, &stderr)
	if got, want := code, 1; got != want {
		t.Fatalf("unexpected exit code: got %d want %d", got, want)
	}
	if !strings.Contains(stderr.String(), "download sandbox file:") {
		t.Fatalf("expected download error, got %q", stderr.String())
	}
}

func startDownloadSandboxServer(t *testing.T, adapter backend.SandboxAdapter) string {
	t.Helper()

	svc := &controlservice.Service{
		Loader: downloadTestLoader{},
		Config: runtimeconfig.Config{DefaultBackend: "firecracker"},
		Backends: map[string]backend.SandboxAdapter{
			"firecracker": adapter,
		},
	}

	server := httptest.NewServer(controlserver.New(svc, nil).Handler())
	t.Cleanup(server.Close)
	return server.URL
}

func mustCreateDownloadTestSandbox(t *testing.T, host string) string {
	t.Helper()

	ep, err := endpoint.Resolve(host)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	client, err := controlclient.New(ep)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	resp, err := client.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Backend: "firecracker",
		Policy: &cleanroomv1.Policy{
			Version:        1,
			ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			NetworkDefault: "deny",
		},
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := resp.GetSandbox().GetSandboxId()
	if sandboxID == "" {
		t.Fatal("expected sandbox id")
	}
	return sandboxID
}
