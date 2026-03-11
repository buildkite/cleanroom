package cli

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

type repositoryIntegrationLoader struct {
	compiled   *policy.CompiledPolicy
	repository policy.RepositoryConfig
}

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

func TestCreateCommandBootstrapsRepositoryForCurrentRepo(t *testing.T) {
	repoDir := initGitRepository(t, "git@github.com:buildkite/cleanroom.git")
	wantCommit := headCommit(t, repoDir)
	restore := stubGitCredentialFill(t, func(_, _ string) (string, error) { return "", nil })
	defer restore()

	adapter := &integrationAdapter{}
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

func TestSandboxCreateCommandRemainsGenericWithRepositoryConfig(t *testing.T) {
	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	restore := stubGitCredentialFill(t, func(_, _ string) (string, error) { return "", nil })
	defer restore()

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
	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	restore := stubGitCredentialFill(t, func(_, _ string) (string, error) { return "", nil })
	defer restore()

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
	restore := stubGitCredentialFill(t, func(_, _ string) (string, error) { return "", nil })
	defer restore()

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
	restore := stubGitCredentialFill(t, func(_, _ string) (string, error) { return "", nil })
	defer restore()
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

func TestCreateCommandWarnsWhenRepositoryIsDirty(t *testing.T) {
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	restore := stubGitCredentialFill(t, func(_, _ string) (string, error) { return "", nil })
	defer restore()
	if err := os.WriteFile(filepath.Join(repoDir, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}

	adapter := &integrationAdapter{}
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
	restore := stubGitCredentialFill(t, func(_, _ string) (string, error) { return "", nil })
	defer restore()
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
	restore := stubGitCredentialFill(t, func(_, _ string) (string, error) { return "", nil })
	defer restore()
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

func TestResolveRepositoryCheckoutUsesCredentialHelperForHTTPSRemote(t *testing.T) {
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	restore := stubGitCredentialFill(t, func(dir, input string) (string, error) {
		gotDir, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatalf("resolve credential dir symlinks: %v", err)
		}
		wantDir, err := filepath.EvalSymlinks(repoDir)
		if err != nil {
			t.Fatalf("resolve repo dir symlinks: %v", err)
		}
		if got, want := gotDir, wantDir; got != want {
			t.Fatalf("unexpected credential dir: got %q want %q", got, want)
		}
		if !strings.Contains(input, "host=github.com\n") {
			t.Fatalf("expected github host lookup, got %q", input)
		}
		return "protocol=https\nhost=github.com\nusername=codex\npassword=top-secret\n", nil
	})
	defer restore()

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
	if got, want := checkout.GitConfigKey, "http.https://github.com/.extraHeader"; got != want {
		t.Fatalf("unexpected git config key: got %q want %q", got, want)
	}
	const prefix = "Authorization: Basic "
	if !strings.HasPrefix(checkout.GitConfigValue, prefix) {
		t.Fatalf("expected basic auth header, got %q", checkout.GitConfigValue)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(checkout.GitConfigValue, prefix))
	if err != nil {
		t.Fatalf("decode auth header: %v", err)
	}
	if got, want := string(decoded), "codex:top-secret"; got != want {
		t.Fatalf("unexpected decoded auth payload: got %q want %q", got, want)
	}
}

func TestWrapCommandWithRepositoryBootstrapStripsCommandSeparator(t *testing.T) {
	command := wrapCommandWithRepositoryBootstrap([]string{"--", "sh", "-lc", "pwd"}, &resolvedRepositoryCheckout{
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

func stubGitCredentialFill(t *testing.T, fn func(dir, input string) (string, error)) func() {
	t.Helper()
	prev := gitCredentialFill
	gitCredentialFill = fn
	return func() {
		gitCredentialFill = prev
	}
}
