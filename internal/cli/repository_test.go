package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

type repositoryIntegrationLoader struct {
	compiled   *policy.CompiledPolicy
	repository policy.RepositoryConfig
}

type repositoryNotFoundLoader struct{}

func (l repositoryIntegrationLoader) LoadAndCompile(_ string) (*policy.CompiledPolicy, string, error) {
	return l.compiled, "/repo/cleanroom.yaml", nil
}

func (l repositoryIntegrationLoader) LoadRepository(_ string) (policy.RepositoryConfig, string, error) {
	return l.repository, "/repo/cleanroom.yaml", nil
}

func (repositoryNotFoundLoader) LoadAndCompile(_ string) (*policy.CompiledPolicy, string, error) {
	return &policy.CompiledPolicy{
		Version:        1,
		ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		NetworkDefault: "deny",
	}, "", nil
}

func (repositoryNotFoundLoader) LoadRepository(_ string) (policy.RepositoryConfig, string, error) {
	return policy.RepositoryConfig{}, "", fmt.Errorf("%w: expected /tmp/cleanroom.yaml or /tmp/.buildkite/cleanroom.yaml", policy.ErrPolicyNotFound)
}

func initGitRepository(t *testing.T, remoteURL string) string {
	t.Helper()

	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
	}

	runGit("init")
	runGit("config", "user.name", "Cleanroom Test")
	runGit("config", "user.email", "cleanroom-test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	runGit("add", "README.md")
	runGit("commit", "-m", "initial")
	runGit("remote", "add", "origin", remoteURL)
	return dir
}

func headCommit(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse HEAD failed: %v\n%s", err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func checkoutGitBranch(t *testing.T, dir, branch string) {
	t.Helper()
	cmd := exec.Command("git", "checkout", "-b", branch)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git checkout -b %q failed: %v\n%s", branch, err, string(out))
	}
}

func TestCreateCommandBootstrapsRepositoryForCurrentRepo(t *testing.T) {
	repoDir := initGitRepository(t, "git@github.com:buildkite/cleanroom.git")
	wantCommit := headCommit(t, repoDir)

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)

	var (
		mu       sync.Mutex
		commands [][]string
	)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	outcome := runCreateAliasWithCapture(CreateCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       repoDir,
	}, runtimeContext{
		CWD: repoDir,
		Loader: repositoryIntegrationLoader{
			compiled: &policy.CompiledPolicy{
				Version:        1,
				ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				NetworkDefault: "deny",
				Allow:          []policy.AllowRule{{Host: "github.com", Ports: []int{443}}},
			},
			repository: policy.RepositoryConfig{
				Mode:   "current-repo",
				Remote: "origin",
				Path:   "/workspace",
			},
		},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("CreateCommand.Run returned error: %v", outcome.err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(commands) != 1 {
		t.Fatalf("expected one bootstrap execution, got %d", len(commands))
	}
	if got, want := commands[0][0], "sh"; got != want {
		t.Fatalf("expected bootstrap command to use sh, got %q", got)
	}
	joined := strings.Join(commands[0], " ")
	if !strings.Contains(joined, "https://github.com/buildkite/cleanroom.git") {
		t.Fatalf("expected bootstrap command to include canonical https remote, got %q", joined)
	}
	if !strings.Contains(joined, "--filter=blob:none") {
		t.Fatalf("expected bootstrap command to request blobless clone, got %q", joined)
	}
	if !strings.Contains(joined, wantCommit) {
		t.Fatalf("expected bootstrap command to include head commit %q, got %q", wantCommit, joined)
	}
}

func TestCreateCommandShowsDependencyBootstrapOutputDuringSandboxCreate(t *testing.T) {
	repoDir := initGitRepository(t, "git@github.com:buildkite/cleanroom.git")

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)

	var callCount int
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		callCount++
		switch callCount {
		case 1:
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("repo bootstrap output\n"))
			}
		case 2:
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("dependency bootstrap output\n"))
			}
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	outcome := runCreateAliasWithCapture(CreateCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       repoDir,
	}, runtimeContext{
		CWD: repoDir,
		Loader: repositoryIntegrationLoader{
			compiled: &policy.CompiledPolicy{
				Version:        1,
				ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				NetworkDefault: "deny",
				Allow:          []policy.AllowRule{{Host: "github.com", Ports: []int{443}}},
				Dependencies: policy.Dependencies{
					Command: []string{"go", "mod", "download"},
				},
			},
			repository: policy.RepositoryConfig{
				Mode:   "current-repo",
				Remote: "origin",
				Path:   "/workspace",
			},
		},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("CreateCommand.Run returned error: %v", outcome.err)
	}
	assertContainsAll(t, outcome.stderr,
		"repo bootstrap output",
		"dependency bootstrap output",
	)
	if strings.Contains(outcome.stderr, "bootstrapping repository checkout") {
		t.Fatalf("expected repository bootstrap phase chatter to be hidden, got %q", outcome.stderr)
	}
	if strings.Contains(outcome.stderr, "running dependency bootstrap") {
		t.Fatalf("expected dependency bootstrap phase chatter to be hidden, got %q", outcome.stderr)
	}
	if got, want := callCount, 2; got != want {
		t.Fatalf("expected repository and dependency bootstrap executions, got %d want %d", got, want)
	}
}

func TestCreateCommandBootstrapsRepositoryForExplicitOverride(t *testing.T) {
	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)

	var (
		mu       sync.Mutex
		commands [][]string
	)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	const wantCommit = "0123456789abcdef0123456789abcdef01234567"
	outcome := runCreateAliasWithCapture(CreateCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       t.TempDir(),
		repositoryOverrideFlags: repositoryOverrideFlags{
			RepoURL:    "https://github.com/buildkite/agent.git",
			RepoCommit: wantCommit,
		},
	}, runtimeContext{
		CWD: t.TempDir(),
		Loader: repositoryIntegrationLoader{
			compiled: &policy.CompiledPolicy{
				Version:        1,
				ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				NetworkDefault: "deny",
				Allow:          []policy.AllowRule{{Host: "github.com", Ports: []int{443}}},
			},
		},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("CreateCommand.Run returned error: %v", outcome.err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(commands) != 1 {
		t.Fatalf("expected one bootstrap execution, got %d", len(commands))
	}
	joined := strings.Join(commands[0], " ")
	if !strings.Contains(joined, "https://github.com/buildkite/agent.git") {
		t.Fatalf("expected bootstrap command to include override remote, got %q", joined)
	}
	if !strings.Contains(joined, wantCommit) {
		t.Fatalf("expected bootstrap command to include override commit %q, got %q", wantCommit, joined)
	}
	if !strings.Contains(joined, "'/workspace'") {
		t.Fatalf("expected bootstrap command to use default workspace path, got %q", joined)
	}
}

func TestCreateCommandRejectsPartialRepositoryOverride(t *testing.T) {
	outcome := runCreateAliasWithCapture(CreateCommand{
		repositoryOverrideFlags: repositoryOverrideFlags{
			RepoURL:    "https://github.com/buildkite/agent.git",
			RepoCommit: "",
		},
	}, runtimeContext{})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected create command to reject partial repository override")
	}
	if !strings.Contains(outcome.err.Error(), "--repo-url and --repo-commit must be used together") {
		t.Fatalf("unexpected error: %v", outcome.err)
	}
}

func TestCreateCommandBootstrapsRepositoryOnCurrentBranch(t *testing.T) {
	repoDir := initGitRepository(t, "git@github.com:buildkite/cleanroom.git")
	checkoutGitBranch(t, repoDir, "feature/console-branch")

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)

	var (
		mu       sync.Mutex
		commands [][]string
	)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	outcome := runCreateAliasWithCapture(CreateCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       repoDir,
	}, runtimeContext{
		CWD: repoDir,
		Loader: repositoryIntegrationLoader{
			compiled: &policy.CompiledPolicy{
				Version:        1,
				ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				NetworkDefault: "deny",
				Allow:          []policy.AllowRule{{Host: "github.com", Ports: []int{443}}},
			},
			repository: policy.RepositoryConfig{
				Mode:   "current-repo",
				Remote: "origin",
				Path:   "/workspace",
			},
		},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("CreateCommand.Run returned error: %v", outcome.err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(commands) != 1 {
		t.Fatalf("expected one bootstrap execution, got %d", len(commands))
	}
	joined := strings.Join(commands[0], " ")
	if !strings.Contains(joined, "feature/console-branch") {
		t.Fatalf("expected bootstrap command to include current branch name, got %q", joined)
	}
	if !strings.Contains(joined, "checkout -B") {
		t.Fatalf("expected bootstrap command to create the branch at the pinned commit, got %q", joined)
	}
}

func TestSandboxCreateCommandRemainsGenericWithRepositoryConfig(t *testing.T) {
	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	restore := stubPolicyUpdateResolver(t, func(_ context.Context, source string) (string, error) {
		if got, want := source, defaultBumpRefSource; got != want {
			t.Fatalf("unexpected default sandbox image source: got %q want %q", got, want)
		}
		return testImageOverrideRef, nil
	})
	defer restore()

	var (
		mu       sync.Mutex
		commands [][]string
	)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	outcome := runSandboxCreateWithCapture(SandboxCreateCommand{
		clientFlags: clientFlags{Host: host},
	}, runtimeContext{
		CWD:    repoDir,
		Loader: failingLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("SandboxCreateCommand.Run returned error: %v", outcome.err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(commands) != 0 {
		t.Fatalf("expected sandbox create to avoid repository bootstrap, got %d execution(s)", len(commands))
	}
}

func TestExecCommandRunsInsideRepositoryPathForNewSandbox(t *testing.T) {
	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")

	var (
		mu       sync.Mutex
		commands [][]string
	)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       repoDir,
		Command:     []string{"echo", "ok"},
	}, runtimeContext{
		CWD: repoDir,
		Loader: repositoryIntegrationLoader{
			compiled: &policy.CompiledPolicy{
				Version:        1,
				ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				NetworkDefault: "deny",
				Allow:          []policy.AllowRule{{Host: "github.com", Ports: []int{443}}},
			},
			repository: policy.RepositoryConfig{
				Mode:   "current-repo",
				Remote: "origin",
				Path:   "/workspace",
			},
		},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(commands) != 2 {
		t.Fatalf("expected bootstrap + user execution, got %d execution(s)", len(commands))
	}
	joined := strings.Join(commands[1], " ")
	if !repositoryWrappedCommandContains(joined, `exec 'echo' 'ok'`) {
		t.Fatalf("expected user command to run inside /workspace, got %q", joined)
	}
}

func TestExecCommandRunsInsideRepositoryPathWhenReusingSandboxID(t *testing.T) {
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)

	var (
		mu       sync.Mutex
		commands [][]string
	)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	loader := repositoryIntegrationLoader{
		compiled: &policy.CompiledPolicy{
			Version:        1,
			ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			NetworkDefault: "deny",
			Allow:          []policy.AllowRule{{Host: "github.com", Ports: []int{443}}},
		},
		repository: policy.RepositoryConfig{
			Mode:   "current-repo",
			Remote: "origin",
			Path:   "/workspace",
		},
	}

	createOutcome := runCreateAliasWithCapture(CreateCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       repoDir,
	}, runtimeContext{
		CWD:    repoDir,
		Loader: loader,
	})
	if createOutcome.cause != nil {
		t.Fatalf("capture failure: %v", createOutcome.cause)
	}
	if createOutcome.err != nil {
		t.Fatalf("CreateCommand.Run returned error: %v", createOutcome.err)
	}
	sandboxID := strings.TrimSpace(createOutcome.stdout)
	if sandboxID == "" {
		t.Fatalf("expected sandbox id output, got %q", createOutcome.stdout)
	}

	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: host},
		In:          sandboxID,
		Command:     []string{"echo", "ok"},
	}, runtimeContext{
		CWD:    repoDir,
		Loader: loader,
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(commands) != 2 {
		t.Fatalf("expected bootstrap + reused sandbox execution, got %d execution(s)", len(commands))
	}
	joined := strings.Join(commands[1], " ")
	if !repositoryWrappedCommandContains(joined, `exec 'echo' 'ok'`) {
		t.Fatalf("expected reused sandbox command to run inside /workspace, got %q", joined)
	}
}

func TestExecCommandSkipsRepositoryBootstrapForExistingSandboxID(t *testing.T) {
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	client := mustNewControlClient(t, host)
	createSandboxResp, err := client.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy: &cleanroomv1.Policy{
			Version:        1,
			ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			NetworkDefault: "deny",
			Allow: []*cleanroomv1.PolicyAllowRule{
				{Host: "github.com", Ports: []int32{443}},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()
	if sandboxID == "" {
		t.Fatal("expected sandbox id")
	}

	var (
		mu       sync.Mutex
		commands [][]string
	)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	loader := repositoryIntegrationLoader{
		compiled: &policy.CompiledPolicy{
			Version:        1,
			ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			NetworkDefault: "deny",
			Allow:          []policy.AllowRule{{Host: "github.com", Ports: []int{443}}},
		},
		repository: policy.RepositoryConfig{
			Mode:   "current-repo",
			Remote: "origin",
			Path:   "/workspace",
		},
	}

	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: host},
		In:          sandboxID,
		Command:     []string{"echo", "ok"},
	}, runtimeContext{
		CWD:    repoDir,
		Loader: loader,
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(commands) != 1 {
		t.Fatalf("expected existing sandbox execution only, got %d execution(s)", len(commands))
	}
	joined := strings.Join(commands[0], " ")
	if strings.Contains(joined, "git clone --filter=blob:none --no-checkout") {
		t.Fatalf("expected existing sandbox execution to avoid repository bootstrap, got %q", joined)
	}
	if strings.Contains(joined, "cd '/workspace' && exec 'echo' 'ok'") {
		t.Fatalf("expected existing sandbox execution to avoid implicit repository workdir reuse from host cwd, got %q", joined)
	}
}

func TestResolveRepositoryCheckoutAllowsDirtyCurrentRepoAtHead(t *testing.T) {
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	wantCommit := headCommit(t, repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}

	checkout, err := resolveRepositoryCheckout(repoDir, repositoryIntegrationLoader{
		compiled: &policy.CompiledPolicy{
			Version:        1,
			ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			NetworkDefault: "deny",
			Allow:          []policy.AllowRule{{Host: "github.com", Ports: []int{443}}},
		},
		repository: policy.RepositoryConfig{
			Mode:   "current-repo",
			Remote: "origin",
			Path:   "/workspace",
		},
	})
	if err != nil {
		t.Fatalf("resolveRepositoryCheckout returned error: %v", err)
	}
	if checkout == nil {
		t.Fatal("expected repository checkout result")
	}
	if !checkout.Dirty {
		t.Fatal("expected repository checkout to record dirty worktree")
	}
	if got, want := checkout.CommitSHA, wantCommit; got != want {
		t.Fatalf("unexpected commit: got %q want %q", got, want)
	}
}

func TestResolveRepositoryCheckoutSkipsImplicitDefaultOutsideGitRepo(t *testing.T) {
	dir := t.TempDir()

	checkout, err := resolveRepositoryCheckout(dir, repositoryIntegrationLoader{
		compiled: &policy.CompiledPolicy{
			Version:        1,
			ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			NetworkDefault: "deny",
			Allow:          []policy.AllowRule{{Host: "github.com", Ports: []int{443}}},
		},
		repository: policy.RepositoryConfig{
			Mode:     "current-repo",
			Remote:   "origin",
			Path:     "/workspace",
			Implicit: true,
		},
	})
	if err != nil {
		t.Fatalf("resolveRepositoryCheckout returned error: %v", err)
	}
	if checkout != nil {
		t.Fatalf("expected implicit repository default to be skipped outside a git repo, got %+v", checkout)
	}
}

func TestExecCommandWithSandboxIDAllowsMissingPolicyInCurrentDirectory(t *testing.T) {
	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	client := mustNewControlClient(t, host)
	sandboxID := mustCreateSandbox(t, client)

	var (
		mu       sync.Mutex
		commands [][]string
	)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: host},
		In:          sandboxID,
		Command:     []string{"echo", "ok"},
	}, runtimeContext{
		CWD:    t.TempDir(),
		Loader: repositoryNotFoundLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(commands) != 1 {
		t.Fatalf("expected one execution, got %d", len(commands))
	}
	if got, want := strings.Join(commands[0], " "), "echo ok"; !strings.Contains(got, want) {
		t.Fatalf("expected passthrough command, got %q", got)
	}
}

func TestCreateCommandWarnsWhenRepositoryIsDirty(t *testing.T) {
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	if err := os.WriteFile(filepath.Join(repoDir, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	outcome := runCreateAliasWithCapture(CreateCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       repoDir,
	}, runtimeContext{
		CWD: repoDir,
		Loader: repositoryIntegrationLoader{
			compiled: &policy.CompiledPolicy{
				Version:        1,
				ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				NetworkDefault: "deny",
				Allow:          []policy.AllowRule{{Host: "github.com", Ports: []int{443}}},
			},
			repository: policy.RepositoryConfig{
				Mode:   "current-repo",
				Remote: "origin",
				Path:   "/workspace",
			},
		},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("CreateCommand.Run returned error: %v", outcome.err)
	}
	if !strings.Contains(outcome.stderr, "repository has local modifications") {
		t.Fatalf("expected dirty repository warning, got %q", outcome.stderr)
	}
}

func TestCreateCommandWarnsWhenRepositoryIsDirtyUsesANSIWhenForced(t *testing.T) {
	t.Setenv("CLICOLOR_FORCE", "1")

	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	if err := os.WriteFile(filepath.Join(repoDir, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	outcome := runCreateAliasWithCapture(CreateCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       repoDir,
	}, runtimeContext{
		CWD: repoDir,
		Loader: repositoryIntegrationLoader{
			compiled: &policy.CompiledPolicy{
				Version:        1,
				ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				NetworkDefault: "deny",
				Allow:          []policy.AllowRule{{Host: "github.com", Ports: []int{443}}},
			},
			repository: policy.RepositoryConfig{
				Mode:   "current-repo",
				Remote: "origin",
				Path:   "/workspace",
			},
		},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("CreateCommand.Run returned error: %v", outcome.err)
	}
	if !strings.Contains(outcome.stderr, "\x1b[") {
		t.Fatalf("expected ANSI escapes in color output: %q", outcome.stderr)
	}
	if !strings.Contains(stripANSI(outcome.stderr), "repository has local modifications") {
		t.Fatalf("expected dirty repository warning, got %q", outcome.stderr)
	}
}

func TestCreateCommandIncludesLocalChangesWithoutDirtyWarning(t *testing.T) {
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	if err := os.WriteFile(filepath.Join(repoDir, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)

	var (
		mu       sync.Mutex
		commands [][]string
		patches  []string
	)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		if stream.OnAttach != nil {
			var stdin bytes.Buffer
			stream.OnAttach(backend.AttachIO{
				WriteStdin: func(data []byte) error {
					_, err := stdin.Write(data)
					return err
				},
				CloseStdin: func() error {
					mu.Lock()
					patches = append(patches, stdin.String())
					mu.Unlock()
					return nil
				},
			})
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	outcome := runCreateAliasWithCapture(CreateCommand{
		clientFlags:              clientFlags{Host: host},
		Chdir:                    repoDir,
		repositoryChangesetFlags: repositoryChangesetFlags{IncludeLocalChanges: true},
	}, runtimeContext{
		CWD: repoDir,
		Loader: repositoryIntegrationLoader{
			compiled: &policy.CompiledPolicy{
				Version:        1,
				ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				NetworkDefault: "deny",
				Allow:          []policy.AllowRule{{Host: "github.com", Ports: []int{443}}},
			},
			repository: policy.RepositoryConfig{
				Mode:   "current-repo",
				Remote: "origin",
				Path:   "/workspace",
			},
		},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("CreateCommand.Run returned error: %v", outcome.err)
	}
	if strings.Contains(outcome.stderr, "repository has local modifications") {
		t.Fatalf("expected dirty repository warning to be suppressed, got %q", outcome.stderr)
	}

	mu.Lock()
	defer mu.Unlock()
	if got, want := len(commands), 2; got != want {
		t.Fatalf("expected repository bootstrap plus changeset apply, got %d want %d", got, want)
	}
	if joined := strings.Join(commands[1], " "); !strings.Contains(joined, `git -C "$dest" apply --binary --whitespace=nowarn "$patch_file"`) {
		t.Fatalf("expected second bootstrap command to apply the repository changeset, got %q", joined)
	}
	if got, want := len(patches), 1; got != want {
		t.Fatalf("expected one attached changeset patch, got %d want %d", got, want)
	}
	if !strings.Contains(patches[0], "dirty.txt") {
		t.Fatalf("expected changeset patch to reference dirty.txt, got %q", patches[0])
	}
}

func TestExecCommandBootstrapsRepositoryOnLocalControlPlane(t *testing.T) {
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	adapter := &integrationAdapter{}
	host, _ := startUnixIntegrationServer(t, adapter)

	var (
		mu       sync.Mutex
		commands [][]string
	)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       repoDir,
		Command:     []string{"echo", "ok"},
	}, runtimeContext{
		CWD: repoDir,
		Loader: repositoryIntegrationLoader{
			compiled: &policy.CompiledPolicy{
				Version:        1,
				ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				NetworkDefault: "deny",
				Allow:          []policy.AllowRule{{Host: "github.com", Ports: []int{443}}},
			},
			repository: policy.RepositoryConfig{
				Mode:   "current-repo",
				Remote: "origin",
				Path:   "/workspace",
			},
		},
		Config: runtimeconfig.Config{
			DefaultBackend: "firecracker",
		},
		Backends: map[string]backend.Adapter{
			"firecracker": adapter,
		},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(commands) != 2 {
		t.Fatalf("expected bootstrap + user execution, got %d execution(s)", len(commands))
	}
	bootstrap := strings.Join(commands[0], " ")
	if !strings.Contains(bootstrap, "clone --filter=blob:none --no-checkout") {
		t.Fatalf("expected repository bootstrap clone, got %q", bootstrap)
	}
	joined := strings.Join(commands[1], " ")
	if !repositoryWrappedCommandContains(joined, `exec 'echo' 'ok'`) {
		t.Fatalf("expected user command to run inside /workspace, got %q", joined)
	}
}

func TestExecCommandBootstrapsExplicitRepositoryOverrideOnLocalControlPlane(t *testing.T) {
	adapter := &integrationAdapter{}
	host, _ := startUnixIntegrationServer(t, adapter)

	var (
		mu       sync.Mutex
		commands [][]string
	)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	const wantCommit = "0123456789abcdef0123456789abcdef01234567"
	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       t.TempDir(),
		repositoryOverrideFlags: repositoryOverrideFlags{
			RepoURL:    "https://github.com/buildkite/agent.git",
			RepoCommit: wantCommit,
		},
		Command: []string{"echo", "ok"},
	}, runtimeContext{
		CWD: t.TempDir(),
		Loader: repositoryIntegrationLoader{
			compiled: &policy.CompiledPolicy{
				Version:        1,
				ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				NetworkDefault: "deny",
				Allow:          []policy.AllowRule{{Host: "github.com", Ports: []int{443}}},
			},
		},
		Config: runtimeconfig.Config{
			DefaultBackend: "firecracker",
		},
		Backends: map[string]backend.Adapter{
			"firecracker": adapter,
		},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(commands) != 2 {
		t.Fatalf("expected bootstrap + user execution, got %d execution(s)", len(commands))
	}
	bootstrap := strings.Join(commands[0], " ")
	if !strings.Contains(bootstrap, "https://github.com/buildkite/agent.git") {
		t.Fatalf("expected bootstrap command to include override remote, got %q", bootstrap)
	}
	if !strings.Contains(bootstrap, wantCommit) {
		t.Fatalf("expected bootstrap command to include override commit %q, got %q", wantCommit, bootstrap)
	}
	joined := strings.Join(commands[1], " ")
	if !repositoryWrappedCommandContains(joined, `exec 'echo' 'ok'`) {
		t.Fatalf("expected user command to run inside /workspace, got %q", joined)
	}
}

func TestExecCommandSkipsRepositoryBootstrapForExistingSandboxOnLocalControlPlane(t *testing.T) {
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	adapter := &integrationAdapter{}
	host, _ := startUnixIntegrationServer(t, adapter)
	client := mustNewControlClient(t, host)
	createSandboxResp, err := client.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy: &cleanroomv1.Policy{
			Version:        1,
			ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			NetworkDefault: "deny",
			Allow: []*cleanroomv1.PolicyAllowRule{
				{Host: "github.com", Ports: []int32{443}},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()
	if sandboxID == "" {
		t.Fatal("expected sandbox id")
	}

	var (
		mu       sync.Mutex
		commands [][]string
	)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: host},
		In:          sandboxID,
		Command:     []string{"echo", "ok"},
	}, runtimeContext{
		CWD: repoDir,
		Loader: repositoryIntegrationLoader{
			compiled: &policy.CompiledPolicy{
				Version:        1,
				ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				NetworkDefault: "deny",
				Allow:          []policy.AllowRule{{Host: "github.com", Ports: []int{443}}},
			},
			repository: policy.RepositoryConfig{
				Mode:   "current-repo",
				Remote: "origin",
				Path:   "/workspace",
			},
		},
		Config: runtimeconfig.Config{
			DefaultBackend: "firecracker",
		},
		Backends: map[string]backend.Adapter{
			"firecracker": adapter,
		},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(commands) != 1 {
		t.Fatalf("expected a single execution, got %d execution(s)", len(commands))
	}
	joined := strings.Join(commands[0], " ")
	if strings.Contains(joined, "clone --filter=blob:none --no-checkout") {
		t.Fatalf("expected reused sandbox execution to avoid repository bootstrap, got %q", joined)
	}
	if strings.Contains(joined, "cd '/workspace' && exec 'echo' 'ok'") {
		t.Fatalf("expected reused sandbox execution to avoid implicit repository workdir reuse from host cwd, got %q", joined)
	}
}

func TestWrapCommandWithRepositoryBootstrapStripsCommandSeparator(t *testing.T) {
	command := repositorycheckout.WrapCommandWithBootstrap([]string{"--", "sh", "-lc", "pwd"}, &repositorycheckout.Checkout{
		RemoteURL:      "https://github.com/buildkite/cleanroom.git",
		CommitSHA:      "0123456789abcdef0123456789abcdef01234567",
		DestinationDir: "/workspace",
	})
	joined := strings.Join(command, " ")
	if strings.Contains(joined, `'--'`) {
		t.Fatalf("expected wrapped command to strip passthrough separator, got %q", joined)
	}
	if !repositoryWrappedCommandContains(joined, `exec 'sh' '-lc' 'pwd'`) {
		t.Fatalf("expected wrapped command to execute normalized command, got %q", joined)
	}
}

func TestWrapCommandWithRepositoryBootstrapDoesNotEmbedAuthHeaders(t *testing.T) {
	command := repositorycheckout.WrapCommandWithBootstrap([]string{"sh", "-lc", "pwd"}, &repositorycheckout.Checkout{
		RemoteURL:      "https://github.com/buildkite/cleanroom.git",
		CommitSHA:      "0123456789abcdef0123456789abcdef01234567",
		DestinationDir: "/workspace",
	})
	joined := strings.Join(command, " ")
	if strings.Contains(joined, ".extraHeader") {
		t.Fatalf("expected bootstrap command to avoid git extra headers, got %q", joined)
	}
	if strings.Contains(joined, "Authorization:") {
		t.Fatalf("expected bootstrap command to avoid embedding authorization headers, got %q", joined)
	}
}

func repositoryWrappedCommandContains(joined, execSnippet string) bool {
	return strings.Contains(joined, "dest='/workspace'") &&
		strings.Contains(joined, `cd "$dest"`) &&
		strings.Contains(joined, execSnippet)
}
