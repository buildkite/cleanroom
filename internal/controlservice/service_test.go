package controlservice

import (
	"context"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/controlapi"
	"github.com/buildkite/cleanroom/internal/policy"
)

type stubAdapter struct {
	result *backend.RunResult
	req    backend.RunRequest
}

func (s *stubAdapter) Name() string { return "stub" }

func (s *stubAdapter) Run(_ context.Context, req backend.RunRequest) (*backend.RunResult, error) {
	s.req = req
	return s.result, nil
}

type stubLoader struct {
	compiled *policy.CompiledPolicy
	source   string
}

func (l stubLoader) LoadAndCompile(_ string) (*policy.CompiledPolicy, string, error) {
	return l.compiled, l.source, nil
}

func TestExecForwardsBackendOutput(t *testing.T) {
	adapter := &stubAdapter{result: &backend.RunResult{
		RunID:      "run-1",
		ExitCode:   0,
		LaunchedVM: true,
		PlanPath:   "/tmp/plan",
		RunDir:     "/tmp/run",
		Message:    "ok",
		Stdout:     "hello stdout\n",
		Stderr:     "hello stderr\n",
	}}

	svc := &Service{
		Loader: stubLoader{
			compiled: &policy.CompiledPolicy{Hash: "hash-1", NetworkDefault: "deny"},
			source:   "/repo/cleanroom.yaml",
		},
		Backends: map[string]backend.Adapter{"firecracker": adapter},
	}

	resp, err := svc.Exec(context.Background(), controlapi.ExecRequest{
		CWD:     "/repo",
		Command: []string{"--", "echo", "hi"},
	})
	if err != nil {
		t.Fatalf("Exec returned error: %v", err)
	}

	if adapter.req.Command[0] != "echo" {
		t.Fatalf("expected normalized command to start with echo, got %q", adapter.req.Command)
	}
	if resp.Stdout != "hello stdout\n" {
		t.Fatalf("expected stdout to be forwarded, got %q", resp.Stdout)
	}
	if resp.Stderr != "hello stderr\n" {
		t.Fatalf("expected stderr to be forwarded, got %q", resp.Stderr)
	}
}
