package cli

import (
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

type persistentIntegrationAdapter struct {
	integrationAdapter
}

func (a *persistentIntegrationAdapter) ProvisionSandbox(context.Context, backend.ProvisionRequest) error {
	return nil
}

func (a *persistentIntegrationAdapter) RunInSandbox(ctx context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
	return a.RunStream(ctx, req, stream)
}

func (a *persistentIntegrationAdapter) TerminateSandbox(context.Context, string) error {
	return nil
}

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

	adapter := &persistentIntegrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)

	var (
		mu       sync.Mutex
		commands [][]string
	)
	adapter.runStreamFn = func(_ context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		return &backend.RunResult{RunID: req.RunID, ExitCode: 0, Message: "ok"}, nil
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

func TestCreateCommandBootstrapsRepositoryOnCurrentBranch(t *testing.T) {
	repoDir := initGitRepository(t, "git@github.com:buildkite/cleanroom.git")
	checkoutGitBranch(t, repoDir, "feature/console-branch")

	adapter := &persistentIntegrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)

	var (
		mu       sync.Mutex
		commands [][]string
	)
	adapter.runStreamFn = func(_ context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		return &backend.RunResult{RunID: req.RunID, ExitCode: 0, Message: "ok"}, nil
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
	adapter := &persistentIntegrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")

	var (
		mu       sync.Mutex
		commands [][]string
	)
	adapter.runStreamFn = func(_ context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		return &backend.RunResult{RunID: req.RunID, ExitCode: 0, Message: "ok"}, nil
	}

	outcome := runSandboxCreateWithCapture(SandboxCreateCommand{
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
		t.Fatalf("SandboxCreateCommand.Run returned error: %v", outcome.err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(commands) != 0 {
		t.Fatalf("expected sandbox create to avoid repository bootstrap, got %d execution(s)", len(commands))
	}
}

func TestExecCommandRunsInsideRepositoryPathForNewSandbox(t *testing.T) {
	adapter := &persistentIntegrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")

	var (
		mu       sync.Mutex
		commands [][]string
	)
	adapter.runStreamFn = func(_ context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		return &backend.RunResult{RunID: req.RunID, ExitCode: 0, Message: "ok"}, nil
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
	if !strings.Contains(joined, "cd '/workspace' && exec 'echo' 'ok'") {
		t.Fatalf("expected user command to run inside /workspace, got %q", joined)
	}
}

func TestExecCommandRunsInsideRepositoryPathWhenReusingSandboxID(t *testing.T) {
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")

	adapter := &persistentIntegrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)

	var (
		mu       sync.Mutex
		commands [][]string
	)
	adapter.runStreamFn = func(_ context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		return &backend.RunResult{RunID: req.RunID, ExitCode: 0, Message: "ok"}, nil
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
		Chdir:       repoDir,
		SandboxID:   sandboxID,
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
	if !strings.Contains(joined, "cd '/workspace' && exec 'echo' 'ok'") {
		t.Fatalf("expected reused sandbox command to run inside /workspace, got %q", joined)
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
	adapter.runStreamFn = func(_ context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		return &backend.RunResult{RunID: req.RunID, ExitCode: 0, Message: "ok"}, nil
	}

	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: host},
		SandboxID:   sandboxID,
		Chdir:       t.TempDir(),
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

	adapter := &persistentIntegrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	adapter.runStreamFn = func(_ context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
		return &backend.RunResult{RunID: req.RunID, ExitCode: 0, Message: "ok"}, nil
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

func TestCreateCommandRejectsRepositoryBootstrapForNonPersistentBackend(t *testing.T) {
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	adapter := &integrationAdapter{}

	outcome := runCreateAliasWithCapture(CreateCommand{
		clientFlags: clientFlags{Host: "unix:///tmp/cleanroom-test.sock"},
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
	if outcome.err == nil {
		t.Fatal("expected create command to reject repository bootstrap on non-persistent backend")
	}
	if !strings.Contains(outcome.err.Error(), "persistent backend") {
		t.Fatalf("unexpected error: %v", outcome.err)
	}
}

func TestExecCommandInlinesRepositoryBootstrapForNonPersistentBackend(t *testing.T) {
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	adapter := &integrationAdapter{}
	host, _ := startUnixIntegrationServer(t, adapter)

	var (
		mu       sync.Mutex
		commands [][]string
	)
	adapter.runStreamFn = func(_ context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		return &backend.RunResult{RunID: req.RunID, ExitCode: 0, Message: "ok"}, nil
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
	if len(commands) != 1 {
		t.Fatalf("expected a single inlined execution, got %d execution(s)", len(commands))
	}
	joined := strings.Join(commands[0], " ")
	if !strings.Contains(joined, "clone --filter=blob:none --no-checkout") {
		t.Fatalf("expected inlined repository clone, got %q", joined)
	}
	if !strings.Contains(joined, "cd '/workspace' && exec 'echo' 'ok'") {
		t.Fatalf("expected inlined command to run inside /workspace, got %q", joined)
	}
}

func TestExecCommandInlinesRepositoryBootstrapForReusedSandboxOnNonPersistentBackend(t *testing.T) {
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
	adapter.runStreamFn = func(_ context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		return &backend.RunResult{RunID: req.RunID, ExitCode: 0, Message: "ok"}, nil
	}

	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       repoDir,
		SandboxID:   sandboxID,
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
		t.Fatalf("expected a single inlined execution, got %d execution(s)", len(commands))
	}
	joined := strings.Join(commands[0], " ")
	if !strings.Contains(joined, "clone --filter=blob:none --no-checkout") {
		t.Fatalf("expected reused sandbox execution to inline repository clone, got %q", joined)
	}
	if !strings.Contains(joined, "cd '/workspace' && exec 'echo' 'ok'") {
		t.Fatalf("expected reused sandbox execution to run inside /workspace, got %q", joined)
	}
}

func TestBackendSupportsRepositoryPersistenceDefersToRemoteControlPlane(t *testing.T) {
	ctx := &runtimeContext{
		Config: runtimeconfig.Config{
			DefaultBackend: "darwin-vz",
		},
		Backends: map[string]backend.Adapter{
			"darwin-vz": &integrationAdapter{},
		},
	}

	if !backendSupportsRepositoryPersistence(ctx, "https://cleanroom.example.com", "darwin-vz") {
		t.Fatal("expected remote control plane to be treated as authoritative for backend persistence")
	}
}

func TestCanonicalizeGitRemoteURLStripsUserInfo(t *testing.T) {
	gotURL, gotHost, err := canonicalizeGitRemoteURL("https://token@github.com/buildkite/cleanroom.git")
	if err != nil {
		t.Fatalf("canonicalizeGitRemoteURL returned error: %v", err)
	}
	if got, want := gotURL, "https://github.com/buildkite/cleanroom.git"; got != want {
		t.Fatalf("unexpected canonical URL: got %q want %q", got, want)
	}
	if got, want := gotHost, "github.com"; got != want {
		t.Fatalf("unexpected host: got %q want %q", got, want)
	}
}

func TestCanonicalizeGitRemoteURLAllowsExplicitDefaultSSHPort(t *testing.T) {
	gotURL, gotHost, err := canonicalizeGitRemoteURL("ssh://git@github.com:22/buildkite/cleanroom.git")
	if err != nil {
		t.Fatalf("canonicalizeGitRemoteURL returned error: %v", err)
	}
	if got, want := gotURL, "https://github.com/buildkite/cleanroom.git"; got != want {
		t.Fatalf("unexpected canonical URL: got %q want %q", got, want)
	}
	if got, want := gotHost, "github.com"; got != want {
		t.Fatalf("unexpected host: got %q want %q", got, want)
	}
}

func TestWrapCommandWithRepositoryBootstrapStripsCommandSeparator(t *testing.T) {
	command := repositorycheckout.WrapCommandWithBootstrap([]string{"--", "sh", "-lc", "pwd"}, &repositorycheckout.Checkout{
		RemoteURL:      "https://github.com/buildkite/cleanroom.git",
		CommitSHA:      "0123456789abcdef0123456789abcdef01234567",
		DestinationDir: "/workspace",
	})
	joined := strings.Join(command, " ")
	if strings.Contains(joined, "exec -- ") {
		t.Fatalf("expected wrapped command to strip passthrough separator, got %q", joined)
	}
	if !strings.Contains(joined, "cd '/workspace' && exec 'sh' '-lc' 'pwd'") {
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
