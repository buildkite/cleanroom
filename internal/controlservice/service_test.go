package controlservice

import (
	"context"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/controlapi"
	"github.com/buildkite/cleanroom/internal/policy"
)

type stubAdapter struct {
	result   *backend.RunResult
	req      backend.RunRequest
	runCalls int
}

func (s *stubAdapter) Name() string { return "stub" }

func (s *stubAdapter) Run(_ context.Context, req backend.RunRequest) (*backend.RunResult, error) {
	s.req = req
	s.runCalls++
	if s.result != nil {
		return s.result, nil
	}
	return &backend.RunResult{
		RunID:      req.RunID,
		ExitCode:   0,
		LaunchedVM: true,
		PlanPath:   "/tmp/plan",
		RunDir:     "/tmp/run",
		Message:    "ok",
	}, nil
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

func TestLaunchRunTerminateLifecycle(t *testing.T) {
	adapter := &stubAdapter{}
	svc := &Service{
		Loader: stubLoader{
			compiled: &policy.CompiledPolicy{Hash: "hash-1", NetworkDefault: "deny"},
			source:   "/repo/cleanroom.yaml",
		},
		Backends: map[string]backend.Adapter{"firecracker": adapter},
	}

	launchResp, err := svc.LaunchCleanroom(context.Background(), controlapi.LaunchCleanroomRequest{
		CWD: "/repo",
		Options: controlapi.LaunchCleanroomOptions{
			RunDir: "/tmp/cleanrooms",
		},
	})
	if err != nil {
		t.Fatalf("LaunchCleanroom returned error: %v", err)
	}
	if launchResp.CleanroomID == "" {
		t.Fatal("expected cleanroom_id to be set")
	}
	if launchResp.PolicyHash != "hash-1" {
		t.Fatalf("unexpected policy hash: %q", launchResp.PolicyHash)
	}

	runResp, err := svc.RunCleanroom(context.Background(), controlapi.RunCleanroomRequest{
		CleanroomID: launchResp.CleanroomID,
		Command:     []string{"--", "pi", "run", "fix tests"},
	})
	if err != nil {
		t.Fatalf("RunCleanroom returned error: %v", err)
	}
	if runResp.CleanroomID != launchResp.CleanroomID {
		t.Fatalf("unexpected cleanroom id: got %q want %q", runResp.CleanroomID, launchResp.CleanroomID)
	}
	if got := adapter.req.Command[0]; got != "pi" {
		t.Fatalf("expected normalized command to start with pi, got %q", got)
	}
	if !strings.HasPrefix(adapter.req.RunDir, "/tmp/cleanrooms/") {
		t.Fatalf("expected run dir under run root, got %q", adapter.req.RunDir)
	}
	if adapter.runCalls != 1 {
		t.Fatalf("expected exactly one run call, got %d", adapter.runCalls)
	}

	terminateResp, err := svc.TerminateCleanroom(context.Background(), controlapi.TerminateCleanroomRequest{
		CleanroomID: launchResp.CleanroomID,
	})
	if err != nil {
		t.Fatalf("TerminateCleanroom returned error: %v", err)
	}
	if !terminateResp.Terminated {
		t.Fatal("expected terminated=true")
	}

	_, err = svc.RunCleanroom(context.Background(), controlapi.RunCleanroomRequest{
		CleanroomID: launchResp.CleanroomID,
		Command:     []string{"echo", "should fail"},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown cleanroom") {
		t.Fatalf("expected unknown cleanroom error after terminate, got %v", err)
	}
}
