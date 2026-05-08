package controlservice

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/policy"
)

func TestServicePassesNetworkStagesToBootstrapAndExecution(t *testing.T) {
	t.Parallel()

	var (
		mu     sync.Mutex
		stages []policy.NetworkStage
	)
	adapter := &stubAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
			mu.Lock()
			stages = append(stages, req.NetworkStage)
			mu.Unlock()
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    0,
				LaunchedVM:  true,
				Message:     "ok",
			}, nil
		},
	}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"go.mod":             "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":             "example.com/test v0.0.0 h1:abc123\n",
		"docker-compose.yml": "services: {}\n",
	})
	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testStageScopedNetworkPolicy(),
		RepositoryCheckout: repositoryCheckout,
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	executionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: createResp.GetSandbox().GetSandboxId(),
		Command:   []string{"echo", "hello"},
		Kind:      cleanroomv1.ExecutionKind_EXECUTION_KIND_BATCH,
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}

	key := executionKey(createResp.GetSandbox().GetSandboxId(), executionResp.GetExecution().GetExecutionId())
	svc.mu.RLock()
	done := svc.executions[key].Done
	svc.mu.RUnlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for execution to finish")
	}

	mu.Lock()
	got := append([]policy.NetworkStage(nil), stages...)
	mu.Unlock()
	want := []policy.NetworkStage{
		policy.NetworkStageWorkspace,
		policy.NetworkStageDependencies,
		policy.NetworkStageServices,
		policy.NetworkStageExecution,
		policy.NetworkStageExecution,
	}
	if len(got) != len(want) {
		t.Fatalf("unexpected network stages: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unexpected network stages: got %v want %v", got, want)
		}
	}
}

func TestInternalWorkspaceCopyInRunsAsWorkspaceStage(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		requests []backend.ExecutionRequest
	)
	adapter := &stubAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
			mu.Lock()
			requests = append(requests, req)
			mu.Unlock()
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    0,
				LaunchedVM:  true,
				Message:     "ok",
			}, nil
		},
	}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"public/assets/app.js": "console.log('asset')\n",
	})
	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryPolicy(),
		RepositoryCheckout: repositoryCheckout,
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()

	copyResp, err := svc.CreateInternalWorkspaceCopyInExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId:          sandboxID,
		Command:            []string{"sh", "-lc", "git -C /workspace clean -ffd"},
		RepositoryCheckout: repositoryCheckout,
		Options: &cleanroomv1.ExecutionOptions{
			SkipRunBefore: true,
		},
	})
	if err != nil {
		t.Fatalf("CreateInternalWorkspaceCopyInExecution returned error: %v", err)
	}
	if _, err := svc.WaitExecution(context.Background(), sandboxID, copyResp.GetExecution().GetExecutionId()); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}

	mu.Lock()
	got := append([]backend.ExecutionRequest(nil), requests...)
	mu.Unlock()
	if len(got) < 2 {
		t.Fatalf("expected repository bootstrap and workspace copy-in execution, got %d request(s)", len(got))
	}
	copyInReq := got[len(got)-1]
	if copyInReq.NetworkStage != policy.NetworkStageWorkspace {
		t.Fatalf("workspace copy-in should run as workspace stage to clear cache output mounts, got %q", copyInReq.NetworkStage)
	}
	if !strings.Contains(strings.Join(copyInReq.Command, "\n"), "git -C /workspace clean -ffd") {
		t.Fatalf("expected copy-in refresh command, got %#v", copyInReq.Command)
	}

	diffResp, err := svc.CreateInternalWorkspaceCopyInExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"sh", "-lc", "git -C /workspace diff --name-only"},
		Options: &cleanroomv1.ExecutionOptions{
			SkipRunBefore: true,
		},
	})
	if err != nil {
		t.Fatalf("CreateInternalWorkspaceCopyInExecution for diff returned error: %v", err)
	}
	if _, err := svc.WaitExecution(context.Background(), sandboxID, diffResp.GetExecution().GetExecutionId()); err != nil {
		t.Fatalf("WaitExecution for diff returned error: %v", err)
	}

	mu.Lock()
	got = append([]backend.ExecutionRequest(nil), requests...)
	mu.Unlock()
	diffReq := got[len(got)-1]
	if diffReq.NetworkStage != policy.NetworkStageExecution {
		t.Fatalf("workspace operation without repository refresh should keep execution stage, got %q", diffReq.NetworkStage)
	}
}

func testStageScopedNetworkPolicy() *cleanroomv1.Policy {
	return &cleanroomv1.Policy{
		Version:        1,
		ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		NetworkDefault: "deny",
		NetworkStages: &cleanroomv1.PolicyNetworkStages{
			Workspace: &cleanroomv1.PolicyNetwork{
				Allow: []*cleanroomv1.PolicyAllowRule{
					{Host: "github.com", Ports: []int32{443}},
				},
			},
			Dependencies: &cleanroomv1.PolicyNetwork{
				Allow: []*cleanroomv1.PolicyAllowRule{
					{Host: "proxy.golang.org", Ports: []int32{443}},
				},
			},
			Services: &cleanroomv1.PolicyNetwork{
				Allow: []*cleanroomv1.PolicyAllowRule{
					{Host: "registry-1.docker.io", Ports: []int32{443}},
				},
			},
			Execution: &cleanroomv1.PolicyNetwork{},
		},
		Dependencies: &cleanroomv1.PolicyDependencies{
			Blocks: []*cleanroomv1.PolicyBlock{testPolicyBlock(
				"go-modules",
				[]string{"mise", "exec", "--", "go", "mod", "download"},
				[]string{"go.mod", "go.sum"},
				[]string{"/root/go/pkg/mod"},
				nil,
			)},
		},
		Services: &cleanroomv1.PolicyServices{
			Blocks: []*cleanroomv1.PolicyBlock{testPolicyBlock(
				"postgres",
				[]string{"docker", "compose", "up", "-d", "postgres"},
				[]string{"docker-compose.yml"},
				[]string{"/var/lib/cleanroom/services/postgres"},
				nil,
			)},
		},
		Run: &cleanroomv1.PolicyRun{
			Before: []string{"sh", "-lc", "echo pre-run"},
		},
	}
}
