package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

func TestWorkspaceCopyInAppliesGitChangeset(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	if err := os.WriteFile(filepath.Join(repoDir, ".gitignore"), []byte("node_modules/\n"), 0o644); err != nil {
		t.Fatalf("write gitignore: %v", err)
	}
	runGitInDir(t, repoDir, "add", ".gitignore")
	runGitInDir(t, repoDir, "commit", "-m", "ignore dependencies")
	if err := os.WriteFile(filepath.Join(repoDir, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "node_modules/pkg"), 0o755); err != nil {
		t.Fatalf("create ignored dependency dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "node_modules/pkg/index.js"), []byte("ignored\n"), 0o644); err != nil {
		t.Fatalf("write ignored dependency file: %v", err)
	}

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	head, err := gitOutput(repoDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("resolve repository HEAD: %v", err)
	}
	branch, err := gitOutput(repoDir, "branch", "--show-current")
	if err != nil {
		t.Fatalf("resolve repository branch: %v", err)
	}
	sandboxID := createWorkspaceCopyTestSandboxWithRepositoryCommitBranch(t, host, "/sandbox-workspace", head, branch)

	var (
		mu       sync.Mutex
		commands [][]string
		patches  []string
	)
	adapter.runStreamFn = func(ctx context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()

		if !strings.Contains(strings.Join(req.Command, " "), `git -C "$dest" apply --binary --whitespace=nowarn "$patch_file"`) {
			return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
		}

		closed := make(chan struct{})
		var closeOnce sync.Once
		var stdin bytes.Buffer
		if stream.OnAttach != nil {
			stream.OnAttach(backend.AttachIO{
				WriteStdin: func(data []byte) error {
					_, err := stdin.Write(data)
					return err
				},
				CloseStdin: func() error {
					mu.Lock()
					patches = append(patches, stdin.String())
					mu.Unlock()
					closeOnce.Do(func() { close(closed) })
					return nil
				},
			})
		}
		select {
		case <-closed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyInCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       repoDir,
		SandboxID:   sandboxID,
	}
	if err := cmd.Run(&runtimeContext{
		CWD:           repoDir,
		Loader:        workspaceCopyRepositoryLoader(),
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("WorkspaceCopyInCommand.Run returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(commands) < 1 {
		t.Fatalf("expected changeset apply command, got %d", len(commands))
	}
	applyCommand := strings.Join(commands[len(commands)-1], " ")
	if !strings.Contains(applyCommand, `git -C "$dest" reset --hard "$base_commit"`) {
		t.Fatalf("expected workspace copy-in to reset the checkout before applying, got %q", applyCommand)
	}
	if !strings.Contains(applyCommand, "dest='/sandbox-workspace'") {
		t.Fatalf("expected workspace copy-in to target sandbox-recorded checkout root, got %q", applyCommand)
	}
	if strings.Contains(applyCommand, "dest='/workspace'") {
		t.Fatalf("expected workspace copy-in not to use stale local policy checkout root, got %q", applyCommand)
	}
	if got, want := len(patches), 1; got != want {
		t.Fatalf("expected one attached changeset patch, got %d", got)
	}
	if !strings.Contains(patches[0], "dirty.txt") {
		t.Fatalf("expected changeset patch to reference dirty.txt, got %q", patches[0])
	}
	if strings.Contains(patches[0], "node_modules") {
		t.Fatalf("expected ignored dependency tree to be excluded, got %q", patches[0])
	}
}

func TestWorkspaceCopyInAppliesLocalOnlyCommitsFromSandboxBase(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	baseCommit := headCommit(t, repoDir)
	branch, err := gitOutput(repoDir, "branch", "--show-current")
	if err != nil {
		t.Fatalf("resolve repository branch: %v", err)
	}
	commitFile(t, repoDir, "local.txt", "local\n", "local commit")
	localCommit := headCommit(t, repoDir)

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createWorkspaceCopyTestSandboxWithRepositoryCommitBranch(t, host, "/sandbox-workspace", baseCommit, strings.TrimSpace(branch))

	var (
		mu       sync.Mutex
		commands [][]string
		patches  []string
	)
	adapter.runStreamFn = func(ctx context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()

		if !strings.Contains(strings.Join(req.Command, " "), `git -C "$dest" apply --binary --whitespace=nowarn "$patch_file"`) {
			return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
		}

		closed := make(chan struct{})
		var closeOnce sync.Once
		var stdin bytes.Buffer
		if stream.OnAttach != nil {
			stream.OnAttach(backend.AttachIO{
				WriteStdin: func(data []byte) error {
					_, err := stdin.Write(data)
					return err
				},
				CloseStdin: func() error {
					mu.Lock()
					patches = append(patches, stdin.String())
					mu.Unlock()
					closeOnce.Do(func() { close(closed) })
					return nil
				},
			})
		}
		select {
		case <-closed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyInCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       repoDir,
		SandboxID:   sandboxID,
	}
	if err := cmd.Run(&runtimeContext{
		CWD:           repoDir,
		Loader:        workspaceCopyRepositoryLoader(),
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("WorkspaceCopyInCommand.Run returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(commands) < 1 {
		t.Fatalf("expected changeset apply command, got %d", len(commands))
	}
	applyCommand := strings.Join(commands[len(commands)-1], " ")
	if !strings.Contains(applyCommand, baseCommit) {
		t.Fatalf("expected workspace copy-in to reset to sandbox base commit %q, got %q", baseCommit, applyCommand)
	}
	if strings.Contains(applyCommand, localCommit) {
		t.Fatalf("expected workspace copy-in not to request local-only commit %q, got %q", localCommit, applyCommand)
	}
	if got, want := len(patches), 1; got != want {
		t.Fatalf("expected one attached changeset patch, got %d", got)
	}
	if !strings.Contains(patches[0], "local.txt") {
		t.Fatalf("expected changeset patch to include local-only commit file, got %q", patches[0])
	}
}

func TestWorkspaceCopyInReturnsWhenPatchStdinWriteFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	if err := os.WriteFile(filepath.Join(repoDir, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	head, err := gitOutput(repoDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("resolve repository HEAD: %v", err)
	}
	branch, err := gitOutput(repoDir, "branch", "--show-current")
	if err != nil {
		t.Fatalf("resolve repository branch: %v", err)
	}
	sandboxID := createWorkspaceCopyTestSandboxWithRepositoryCommitBranch(t, host, "/sandbox-workspace", head, branch)

	applyExecutionIDCh := make(chan string, 1)
	adapter.runStreamFn = func(ctx context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		if !strings.Contains(strings.Join(req.Command, " "), `git -C "$dest" apply --binary --whitespace=nowarn "$patch_file"`) {
			return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
		}
		select {
		case applyExecutionIDCh <- req.ExecutionID:
		default:
		}
		if stream.OnAttach != nil {
			stream.OnAttach(backend.AttachIO{
				WriteStdin: func([]byte) error {
					return errors.New("broken stdin")
				},
				CloseStdin: func() error {
					return nil
				},
			})
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}

	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyInCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       repoDir,
		SandboxID:   sandboxID,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- cmd.Run(&runtimeContext{
			CWD:           repoDir,
			Loader:        workspaceCopyRepositoryLoader(),
			Config:        runtimeconfig.Config{},
			Observability: newTestObservability(t),
			Stdout:        stdout,
			Stderr:        stderr,
		})
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected WorkspaceCopyInCommand.Run to return stdin write error")
		}
		if !strings.Contains(err.Error(), "write workspace operation payload") || !strings.Contains(err.Error(), "broken stdin") {
			t.Fatalf("expected stdin write error, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for workspace copy-in to return stdin write error")
	}

	client := mustNewControlClient(t, host)
	select {
	case executionID := <-applyExecutionIDCh:
		_, _ = client.CancelExecution(context.Background(), &cleanroomv1.CancelExecutionRequest{
			SandboxId:   sandboxID,
			ExecutionId: executionID,
			Signal:      2,
		})
	default:
	}
}

func TestWorkspaceCopyInUsesGitChangesetForGitWorktreeWithoutRepositoryPolicy(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	if err := os.WriteFile(filepath.Join(repoDir, ".gitignore"), []byte("node_modules/\n"), 0o644); err != nil {
		t.Fatalf("write gitignore: %v", err)
	}
	runGitInDir(t, repoDir, "add", ".gitignore")
	runGitInDir(t, repoDir, "commit", "-m", "ignore dependencies")
	if err := os.WriteFile(filepath.Join(repoDir, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "node_modules/pkg"), 0o755); err != nil {
		t.Fatalf("create ignored dependency dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "node_modules/pkg/index.js"), []byte("ignored\n"), 0o644); err != nil {
		t.Fatalf("write ignored dependency file: %v", err)
	}

	var rawArchiveCalled bool
	adapter := &copyIntegrationAdapter{
		extractFn: func(context.Context, string, string, io.Reader) (int64, error) {
			rawArchiveCalled = true
			return 0, errors.New("raw archive should not run")
		},
	}
	host, _ := startIntegrationServer(t, adapter)
	head, err := gitOutput(repoDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("resolve repository HEAD: %v", err)
	}
	branch, err := gitOutput(repoDir, "branch", "--show-current")
	if err != nil {
		t.Fatalf("resolve repository branch: %v", err)
	}
	sandboxID := createWorkspaceCopyTestSandboxWithRepositoryCommitBranch(t, host, "/sandbox-workspace", head, branch)

	var (
		mu       sync.Mutex
		commands [][]string
		patches  []string
	)
	adapter.runStreamFn = func(ctx context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()

		if !strings.Contains(strings.Join(req.Command, " "), `git -C "$dest" apply --binary --whitespace=nowarn "$patch_file"`) {
			return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
		}

		closed := make(chan struct{})
		var closeOnce sync.Once
		var stdin bytes.Buffer
		if stream.OnAttach != nil {
			stream.OnAttach(backend.AttachIO{
				WriteStdin: func(data []byte) error {
					_, err := stdin.Write(data)
					return err
				},
				CloseStdin: func() error {
					mu.Lock()
					patches = append(patches, stdin.String())
					mu.Unlock()
					closeOnce.Do(func() { close(closed) })
					return nil
				},
			})
		}
		select {
		case <-closed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyInCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       repoDir,
		SandboxID:   sandboxID,
	}
	if err := cmd.Run(&runtimeContext{
		CWD:           repoDir,
		Loader:        repositoryNotFoundLoader{},
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("WorkspaceCopyInCommand.Run returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if rawArchiveCalled {
		t.Fatal("expected Git worktree workspace copy-in to avoid raw archive transport")
	}
	if len(commands) < 1 {
		t.Fatalf("expected changeset apply command, got %d", len(commands))
	}
	applyCommand := strings.Join(commands[len(commands)-1], " ")
	if !strings.Contains(applyCommand, `git -C "$dest" apply --binary --whitespace=nowarn "$patch_file"`) {
		t.Fatalf("expected workspace copy-in to use Git changeset apply, got %q", applyCommand)
	}
	if got, want := len(patches), 1; got != want {
		t.Fatalf("expected one attached changeset patch, got %d", got)
	}
	if !strings.Contains(patches[0], "dirty.txt") {
		t.Fatalf("expected changeset patch to reference dirty.txt, got %q", patches[0])
	}
	if strings.Contains(patches[0], "node_modules") {
		t.Fatalf("expected ignored dependency tree to be excluded, got %q", patches[0])
	}
}

func TestWorkspaceCopyInResetsGitCheckoutWhenLocalRepoClean(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	head, err := gitOutput(repoDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("resolve repository HEAD: %v", err)
	}
	branch, err := gitOutput(repoDir, "branch", "--show-current")
	if err != nil {
		t.Fatalf("resolve repository branch: %v", err)
	}
	sandboxID := createWorkspaceCopyTestSandboxWithRepositoryCommitBranch(t, host, "/sandbox-workspace", head, branch)

	var (
		mu          sync.Mutex
		commands    [][]string
		stdinWrites int
		stdinCloses int
	)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		if stream.OnAttach != nil {
			stream.OnAttach(backend.AttachIO{
				WriteStdin: func(_ []byte) error {
					mu.Lock()
					stdinWrites++
					mu.Unlock()
					return nil
				},
				CloseStdin: func() error {
					mu.Lock()
					stdinCloses++
					mu.Unlock()
					return nil
				},
			})
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyInCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       repoDir,
		SandboxID:   sandboxID,
	}
	if err := cmd.Run(&runtimeContext{
		CWD:           repoDir,
		Loader:        workspaceCopyRepositoryLoader(),
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("WorkspaceCopyInCommand.Run returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(commands) < 1 {
		t.Fatalf("expected checkout reset command, got %d", len(commands))
	}
	resetCommand := strings.Join(commands[len(commands)-1], " ")
	if !strings.Contains(resetCommand, `git -C "$dest" reset --hard "$base_commit"`) {
		t.Fatalf("expected clean workspace copy-in to reset the checkout, got %q", resetCommand)
	}
	if !strings.Contains(resetCommand, "dest='/sandbox-workspace'") {
		t.Fatalf("expected clean workspace copy-in to target sandbox-recorded checkout root, got %q", resetCommand)
	}
	if strings.Contains(resetCommand, "dest='/workspace'") {
		t.Fatalf("expected clean workspace copy-in not to use stale local policy checkout root, got %q", resetCommand)
	}
	if strings.Contains(resetCommand, `git -C "$dest" apply --binary`) {
		t.Fatalf("expected clean workspace copy-in to avoid applying an empty patch, got %q", resetCommand)
	}
	if stdinWrites != 0 || stdinCloses != 0 {
		t.Fatalf("expected reset-only copy to avoid stdin attachment, got writes=%d closes=%d", stdinWrites, stdinCloses)
	}
}

func TestResolveExecutionSandboxClearsRepositoryAfterGitCopyInExistingSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	head, err := gitOutput(repoDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("resolve repository HEAD: %v", err)
	}
	branch, err := gitOutput(repoDir, "branch", "--show-current")
	if err != nil {
		t.Fatalf("resolve repository branch: %v", err)
	}
	sandboxID := createWorkspaceCopyTestSandboxWithRepositoryCommitBranch(t, host, "/sandbox-workspace", head, branch)
	var (
		mu            sync.Mutex
		launchSeconds []int64
	)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		launchSeconds = append(launchSeconds, req.LaunchSeconds)
		mu.Unlock()
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	client := mustNewControlClient(t, host)
	target, err := resolveExecutionSandbox(
		context.Background(),
		client,
		&runtimeContext{
			CWD:           repoDir,
			Loader:        workspaceCopyRepositoryLoader(),
			Config:        runtimeconfig.Config{},
			Observability: newTestObservability(t),
		},
		repoDir,
		host,
		"",
		sandboxID,
		"",
		"",
		37,
		false,
		repositoryOverrideFlags{},
		workspaceCopyFlags{CopyIn: true},
	)
	if err != nil {
		t.Fatalf("resolveExecutionSandbox returned error: %v", err)
	}
	if target.Repository != nil {
		t.Fatalf("expected returned execution sandbox to avoid repository checkout after copy, got %#v", target.Repository)
	}
	if got, want := target.WorkspaceRoot, "/sandbox-workspace"; got != want {
		t.Fatalf("unexpected returned workspace root: got %q want %q", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(launchSeconds) == 0 {
		t.Fatal("expected workspace copy-in execution to run")
	}
	if got, want := launchSeconds[0], int64(37); got != want {
		t.Fatalf("unexpected workspace copy-in launch seconds: got %d want %d", got, want)
	}
}

func TestResolveExecutionSandboxCopyInUsesGitWorktreeWithoutRepositoryPolicy(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	if err := os.WriteFile(filepath.Join(repoDir, ".gitignore"), []byte("node_modules/\n"), 0o644); err != nil {
		t.Fatalf("write gitignore: %v", err)
	}
	runGitInDir(t, repoDir, "add", ".gitignore")
	runGitInDir(t, repoDir, "commit", "-m", "ignore dependencies")
	if err := os.WriteFile(filepath.Join(repoDir, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "node_modules/pkg"), 0o755); err != nil {
		t.Fatalf("create ignored dependency dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "node_modules/pkg/index.js"), []byte("ignored\n"), 0o644); err != nil {
		t.Fatalf("write ignored dependency file: %v", err)
	}

	var rawArchiveCalled bool
	adapter := &copyIntegrationAdapter{
		extractFn: func(context.Context, string, string, io.Reader) (int64, error) {
			rawArchiveCalled = true
			return 0, errors.New("raw archive should not run")
		},
	}
	host, _ := startIntegrationServer(t, adapter)
	head, err := gitOutput(repoDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("resolve repository HEAD: %v", err)
	}
	branch, err := gitOutput(repoDir, "branch", "--show-current")
	if err != nil {
		t.Fatalf("resolve repository branch: %v", err)
	}
	sandboxID := createWorkspaceCopyTestSandboxWithRepositoryCommitBranch(t, host, "/sandbox-workspace", head, branch)

	var (
		mu       sync.Mutex
		commands [][]string
		patches  []string
	)
	adapter.runStreamFn = func(ctx context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()

		if !strings.Contains(strings.Join(req.Command, " "), `git -C "$dest" apply --binary --whitespace=nowarn "$patch_file"`) {
			return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
		}

		closed := make(chan struct{})
		var closeOnce sync.Once
		var stdin bytes.Buffer
		if stream.OnAttach != nil {
			stream.OnAttach(backend.AttachIO{
				WriteStdin: func(data []byte) error {
					_, err := stdin.Write(data)
					return err
				},
				CloseStdin: func() error {
					mu.Lock()
					patches = append(patches, stdin.String())
					mu.Unlock()
					closeOnce.Do(func() { close(closed) })
					return nil
				},
			})
		}
		select {
		case <-closed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	client := mustNewControlClient(t, host)
	target, err := resolveExecutionSandbox(
		context.Background(),
		client,
		&runtimeContext{
			CWD:           repoDir,
			Loader:        repositoryNotFoundLoader{},
			Config:        runtimeconfig.Config{},
			Observability: newTestObservability(t),
		},
		repoDir,
		host,
		"",
		sandboxID,
		"",
		"",
		0,
		false,
		repositoryOverrideFlags{},
		workspaceCopyFlags{CopyIn: true},
	)
	if err != nil {
		t.Fatalf("resolveExecutionSandbox returned error: %v", err)
	}
	if target.Repository != nil {
		t.Fatalf("expected returned execution sandbox to avoid repository checkout after copy, got %#v", target.Repository)
	}
	if got, want := target.WorkspaceRoot, "/sandbox-workspace"; got != want {
		t.Fatalf("unexpected returned workspace root: got %q want %q", got, want)
	}

	mu.Lock()
	defer mu.Unlock()
	if rawArchiveCalled {
		t.Fatal("expected Git worktree --copy-in to avoid raw archive transport")
	}
	if len(commands) < 1 {
		t.Fatalf("expected changeset apply command, got %d", len(commands))
	}
	applyCommand := strings.Join(commands[len(commands)-1], " ")
	if !strings.Contains(applyCommand, `git -C "$dest" apply --binary --whitespace=nowarn "$patch_file"`) {
		t.Fatalf("expected --copy-in to use Git changeset apply, got %q", applyCommand)
	}
	if got, want := len(patches), 1; got != want {
		t.Fatalf("expected one attached changeset patch, got %d", got)
	}
	if strings.Contains(patches[0], "node_modules") {
		t.Fatalf("expected ignored dependency tree to be excluded, got %q", patches[0])
	}
}

func TestWorkspaceCopyInDryRunReportsCleanGitResetWithoutExecuting(t *testing.T) {
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	head, err := gitOutput(repoDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("resolve repository HEAD: %v", err)
	}
	branch, err := gitOutput(repoDir, "branch", "--show-current")
	if err != nil {
		t.Fatalf("resolve repository branch: %v", err)
	}
	sandboxID := createWorkspaceCopyTestSandboxWithRepositoryCommitBranch(t, host, "/sandbox-workspace", head, branch)
	var (
		mu       sync.Mutex
		commands [][]string
	)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	stdout, stdoutText := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyInCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       repoDir,
		DryRun:      true,
		SandboxID:   sandboxID,
	}
	if err := cmd.Run(&runtimeContext{
		CWD:           repoDir,
		Loader:        workspaceCopyRepositoryLoader(),
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("WorkspaceCopyInCommand.Run returned error: %v", err)
	}
	if got, want := stdoutText(), "reset\t/sandbox-workspace\n"; got != want {
		t.Fatalf("unexpected clean dry-run plan: got %q want %q", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(commands) != 0 {
		t.Fatalf("dry-run should not run workspace copy-in execution, got %q", strings.Join(commands[0], " "))
	}
}

func TestWorkspaceCopyInRecordsGitBinding(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	head, err := gitOutput(repoDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("resolve repository HEAD: %v", err)
	}
	branch, err := gitOutput(repoDir, "branch", "--show-current")
	if err != nil {
		t.Fatalf("resolve repository branch: %v", err)
	}

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createWorkspaceCopyTestSandboxWithRepositoryCommitBranch(t, host, "/sandbox-workspace", head, branch)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
		command := strings.Join(req.Command, " ")
		if !strings.Contains(command, "reset --hard") {
			t.Fatalf("expected clean copy-in reset command, got %q", command)
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyInCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       repoDir,
		SandboxID:   sandboxID,
	}
	if err := cmd.Run(&runtimeContext{
		CWD:           repoDir,
		Loader:        repositoryNotFoundLoader{},
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("WorkspaceCopyInCommand.Run returned error: %v", err)
	}

	binding := mustReadWorkspaceBinding(t, sandboxID)
	if got, want := binding.LocalRoot, mustNormalizeWorkspaceLocalRoot(t, repoDir); got != want {
		t.Fatalf("unexpected binding local root: got %q want %q", got, want)
	}
	if got, want := binding.RepositoryRemoteURL, "https://github.com/buildkite/cleanroom.git"; got != want {
		t.Fatalf("unexpected binding remote: got %q want %q", got, want)
	}
	if got, want := binding.RepositoryCommitSHA, head; got != want {
		t.Fatalf("unexpected binding commit: got %q want %q", got, want)
	}
	if got, want := binding.SandboxWorkspace, "/sandbox-workspace"; got != want {
		t.Fatalf("unexpected binding workspace: got %q want %q", got, want)
	}
	if got, want := binding.Transport, workspaceBindingTransportGit; got != want {
		t.Fatalf("unexpected binding transport: got %q want %q", got, want)
	}
	if got, want := binding.LastOperation, "copy-in"; got != want {
		t.Fatalf("unexpected binding operation: got %q want %q", got, want)
	}
}

func TestWorkspaceCopyInWarnsWhenBindingCannotBeSaved(t *testing.T) {
	setBrokenWorkspaceStateHome(t)
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	head, err := gitOutput(repoDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("resolve repository HEAD: %v", err)
	}
	branch, err := gitOutput(repoDir, "branch", "--show-current")
	if err != nil {
		t.Fatalf("resolve repository branch: %v", err)
	}

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createWorkspaceCopyTestSandboxWithRepositoryCommitBranch(t, host, "/sandbox-workspace", head, branch)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
		command := strings.Join(req.Command, " ")
		if !strings.Contains(command, "reset --hard") {
			t.Fatalf("expected clean copy-in reset command, got %q", command)
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	stdout, _ := makeStdoutCapture(t)
	stderr, stderrText := makeStdoutCapture(t)
	cmd := WorkspaceCopyInCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       repoDir,
		SandboxID:   sandboxID,
	}
	if err := cmd.Run(&runtimeContext{
		CWD:           repoDir,
		Loader:        repositoryNotFoundLoader{},
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("WorkspaceCopyInCommand.Run returned error: %v", err)
	}
	if got := stderrText(); !strings.Contains(got, "warning: workspace binding was not saved") {
		t.Fatalf("expected binding warning on stderr, got %q", got)
	}
}

func TestWorkspaceBindingRecordsCopyInManifest(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	repository, err := resolveWorkspaceCopyRepositoryCheckout(repoDir, repositoryNotFoundLoader{})
	if err != nil {
		t.Fatalf("resolve repository checkout: %v", err)
	}
	files := []repositorychangeset.File{{
		Path:   "dirty.txt",
		SHA256: "sha256:dirty",
		Mode:   "100644",
	}, {
		Path:    "removed.txt",
		Deleted: true,
	}}
	checkout := toRepositoryCheckout(repository)
	checkout.DestinationDir = "/sandbox-workspace"

	if err := recordGitWorkspaceBinding("cr_manifest", repository, checkout, files, "copy-in"); err != nil {
		t.Fatalf("recordGitWorkspaceBinding returned error: %v", err)
	}
	binding := mustReadWorkspaceBinding(t, "cr_manifest")
	expected := []workspaceBindingFile{
		{Path: "dirty.txt", SHA256: "sha256:dirty", Mode: "100644"},
		{Path: "removed.txt", Deleted: true},
	}
	if !reflect.DeepEqual(binding.CopyInManifest, expected) {
		t.Fatalf("unexpected binding manifest: got %+v want %+v", binding.CopyInManifest, expected)
	}
}

func TestWorkspaceCopyOutDryRunReportsSandboxGitPlan(t *testing.T) {
	localRoot := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	resolvedLocalRoot, err := gitOutput(localRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatalf("resolve local repository root: %v", err)
	}
	subdir := filepath.Join(localRoot, "subdir")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("create subdir: %v", err)
	}
	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createWorkspaceCopyTestSandboxWithRepository(t, host, "/sandbox-workspace")

	var (
		mu       sync.Mutex
		commands [][]string
	)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		if stream.OnStdout != nil {
			stream.OnStdout([]byte("M\x00changed.txt\x00D\x00removed.txt\x00A\x00nested/new.txt\x00"))
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	stdout, stdoutText := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyOutCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       subdir,
		DryRun:      true,
		SandboxID:   sandboxID,
	}
	if err := cmd.Run(&runtimeContext{
		CWD:           localRoot,
		Loader:        repositoryNotFoundLoader{},
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("WorkspaceCopyOutCommand.Run returned error: %v", err)
	}

	expected := strings.Join([]string{
		"delete\t" + filepath.Join(resolvedLocalRoot, "removed.txt"),
		"write\t" + filepath.Join(resolvedLocalRoot, "changed.txt"),
		"write\t" + filepath.Join(resolvedLocalRoot, "nested", "new.txt"),
		"",
	}, "\n")
	if got := stdoutText(); got != expected {
		t.Fatalf("unexpected copy-out dry-run plan: got %q want %q", got, expected)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(commands) != 1 {
		t.Fatalf("expected one workspace copy-out planning execution, got %d", len(commands))
	}
	planCommand := strings.Join(commands[0], " ")
	if !strings.Contains(planCommand, "dest='/sandbox-workspace'") {
		t.Fatalf("expected copy-out planning to use sandbox workspace root, got %q", planCommand)
	}
	if !strings.Contains(planCommand, "diff --cached --name-status --no-renames -z") {
		t.Fatalf("expected copy-out planning to use git name-status, got %q", planCommand)
	}
}

func TestWorkspaceCopyOutDryRunUsesBoundLocalRoot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	localRoot := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	resolvedLocalRoot, err := gitOutput(localRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatalf("resolve local repository root: %v", err)
	}
	head, err := gitOutput(localRoot, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("resolve repository HEAD: %v", err)
	}
	repository, err := resolveWorkspaceCopyRepositoryCheckout(localRoot, repositoryNotFoundLoader{})
	if err != nil {
		t.Fatalf("resolve repository checkout: %v", err)
	}

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createWorkspaceCopyTestSandboxWithRepositoryCommitBranch(t, host, "/sandbox-workspace", head, "main")
	checkout := toRepositoryCheckout(repository)
	checkout.DestinationDir = "/sandbox-workspace"
	if err := recordGitWorkspaceBinding(sandboxID, repository, checkout, nil, "copy-in"); err != nil {
		t.Fatalf("record workspace binding: %v", err)
	}

	var executionCalled bool
	adapter.runStreamFn = func(_ context.Context, _ backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		executionCalled = true
		if stream.OnStdout != nil {
			stream.OnStdout([]byte("M\x00changed.txt\x00"))
		}
		return &backend.ExecutionResult{ExitCode: 0, Message: "ok"}, nil
	}

	stdout, stdoutText := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyOutCommand{
		clientFlags: clientFlags{Host: host},
		DryRun:      true,
		SandboxID:   sandboxID,
	}
	if err := cmd.Run(&runtimeContext{
		CWD:           t.TempDir(),
		Loader:        repositoryNotFoundLoader{},
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("WorkspaceCopyOutCommand.Run returned error: %v", err)
	}
	if !executionCalled {
		t.Fatal("expected sandbox planning execution")
	}
	expected := "write\t" + filepath.Join(resolvedLocalRoot, "changed.txt") + "\n"
	if got := stdoutText(); got != expected {
		t.Fatalf("unexpected copy-out dry-run plan: got %q want %q", got, expected)
	}
}

func TestWorkspaceCopyOutRejectsExplicitRootOutsideBinding(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	localRoot := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	otherRoot := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	head, err := gitOutput(localRoot, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("resolve repository HEAD: %v", err)
	}
	repository, err := resolveWorkspaceCopyRepositoryCheckout(localRoot, repositoryNotFoundLoader{})
	if err != nil {
		t.Fatalf("resolve repository checkout: %v", err)
	}

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createWorkspaceCopyTestSandboxWithRepositoryCommitBranch(t, host, "/sandbox-workspace", head, "main")
	checkout := toRepositoryCheckout(repository)
	checkout.DestinationDir = "/sandbox-workspace"
	if err := recordGitWorkspaceBinding(sandboxID, repository, checkout, nil, "copy-in"); err != nil {
		t.Fatalf("record workspace binding: %v", err)
	}

	var executionCalled bool
	adapter.runStreamFn = func(_ context.Context, _ backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
		executionCalled = true
		return &backend.ExecutionResult{ExitCode: 0, Message: "ok"}, nil
	}

	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyOutCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       otherRoot,
		DryRun:      true,
		SandboxID:   sandboxID,
	}
	err = cmd.Run(&runtimeContext{
		CWD:           localRoot,
		Loader:        repositoryNotFoundLoader{},
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	})
	if err == nil {
		t.Fatal("expected mismatched explicit root to be rejected")
	}
	if !strings.Contains(err.Error(), "is bound to local root") {
		t.Fatalf("expected bound local root error, got %v", err)
	}
	if executionCalled {
		t.Fatal("expected copy-out to fail before sandbox planning execution")
	}
}

func TestWorkspaceCopyOutAppliesSandboxGitPatch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	localRoot := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	if err := os.WriteFile(filepath.Join(localRoot, "removed.txt"), []byte("remove me\n"), 0o644); err != nil {
		t.Fatalf("write removed file: %v", err)
	}
	runGitInDir(t, localRoot, "add", "removed.txt")
	runGitInDir(t, localRoot, "commit", "-m", "add removed file")
	baseCommit := headCommit(t, localRoot)

	if err := os.WriteFile(filepath.Join(localRoot, "README.md"), []byte("sandbox\n"), 0o644); err != nil {
		t.Fatalf("write sandbox readme: %v", err)
	}
	if err := os.Remove(filepath.Join(localRoot, "removed.txt")); err != nil {
		t.Fatalf("remove sandbox file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localRoot, "new.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatalf("write sandbox new file: %v", err)
	}
	runGitInDir(t, localRoot, "add", "-A")
	nameStatus := gitOutputBytes(t, localRoot, "diff", "--cached", "--name-status", "--no-renames", "-z", baseCommit)
	patch := gitOutputBytes(t, localRoot, "diff", "--cached", "--binary", "--full-index", "--no-ext-diff", "--no-color", "--no-renames", baseCommit)
	runGitInDir(t, localRoot, "reset", "--hard", baseCommit)
	runGitInDir(t, localRoot, "clean", "-fd")
	resolvedLocalRoot, err := gitOutput(localRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatalf("resolve local repository root: %v", err)
	}

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createWorkspaceCopyTestSandboxWithRepositoryCommitBranch(t, host, "/sandbox-workspace", baseCommit, "main")

	var (
		mu       sync.Mutex
		commands [][]string
	)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		command := strings.Join(req.Command, " ")
		switch {
		case strings.Contains(command, "cleanroom-copy-out-v1"):
			if stream.OnStdout != nil {
				stream.OnStdout(workspaceCopyOutTestPayload(nameStatus, patch))
			}
		default:
			t.Fatalf("unexpected workspace copy-out command: %q", command)
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	stdout, stdoutText := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyOutCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       localRoot,
		SandboxID:   sandboxID,
	}
	if err := cmd.Run(&runtimeContext{
		CWD:           localRoot,
		Loader:        repositoryNotFoundLoader{},
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("WorkspaceCopyOutCommand.Run returned error: %v", err)
	}

	readme, err := os.ReadFile(filepath.Join(localRoot, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	if got, want := string(readme), "sandbox\n"; got != want {
		t.Fatalf("unexpected README content: got %q want %q", got, want)
	}
	newFile, err := os.ReadFile(filepath.Join(localRoot, "new.txt"))
	if err != nil {
		t.Fatalf("read new file: %v", err)
	}
	if got, want := string(newFile), "new\n"; got != want {
		t.Fatalf("unexpected new file content: got %q want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(localRoot, "removed.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected removed file to be deleted, got err %v", err)
	}

	expected := strings.Join([]string{
		"delete\t" + filepath.Join(resolvedLocalRoot, "removed.txt"),
		"write\t" + filepath.Join(resolvedLocalRoot, "README.md"),
		"write\t" + filepath.Join(resolvedLocalRoot, "new.txt"),
		"",
	}, "\n")
	if got := stdoutText(); got != expected {
		t.Fatalf("unexpected copy-out output: got %q want %q", got, expected)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(commands) != 1 {
		t.Fatalf("expected one combined copy-out execution, got %d", len(commands))
	}
	assertNoWorkspaceCopyOutRecoveryPayload(t)
}

func TestWorkspaceCopyOutAppliesFromCopyInManifestBase(t *testing.T) {
	fixture := setupWorkspaceCopyOutManifestBase(t, func(localRoot string) {
		if err := os.WriteFile(filepath.Join(localRoot, "README.md"), []byte("local\nsandbox\n"), 0o644); err != nil {
			t.Fatalf("write sandbox README: %v", err)
		}
		if err := os.WriteFile(filepath.Join(localRoot, "local.txt"), []byte("local only\nsandbox\n"), 0o644); err != nil {
			t.Fatalf("write sandbox local file: %v", err)
		}
		if err := os.WriteFile(filepath.Join(localRoot, "generated.txt"), []byte("generated\n"), 0o644); err != nil {
			t.Fatalf("write sandbox generated file: %v", err)
		}
	})

	stdout, stdoutText := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyOutCommand{
		clientFlags: clientFlags{Host: fixture.host},
		SandboxID:   fixture.sandboxID,
	}
	if err := cmd.Run(&runtimeContext{
		CWD:           t.TempDir(),
		Loader:        repositoryNotFoundLoader{},
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("WorkspaceCopyOutCommand.Run returned error: %v", err)
	}

	for path, want := range map[string]string{
		"README.md":     "local\nsandbox\n",
		"local.txt":     "local only\nsandbox\n",
		"generated.txt": "generated\n",
	} {
		got, err := os.ReadFile(filepath.Join(fixture.localRoot, path))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(got) != want {
			t.Fatalf("unexpected %s content: got %q want %q", path, got, want)
		}
	}

	expected := strings.Join([]string{
		"write\t" + filepath.Join(fixture.resolvedLocalRoot, "README.md"),
		"write\t" + filepath.Join(fixture.resolvedLocalRoot, "generated.txt"),
		"write\t" + filepath.Join(fixture.resolvedLocalRoot, "local.txt"),
		"",
	}, "\n")
	if got := stdoutText(); got != expected {
		t.Fatalf("unexpected copy-out output: got %q want %q", got, expected)
	}
	assertNoWorkspaceCopyOutRecoveryPayload(t)
}

func TestWorkspaceCopyOutDryRunUsesCopyInManifestBase(t *testing.T) {
	fixture := setupWorkspaceCopyOutManifestBase(t, func(localRoot string) {
		if err := os.WriteFile(filepath.Join(localRoot, "README.md"), []byte("local\nsandbox\n"), 0o644); err != nil {
			t.Fatalf("write sandbox README: %v", err)
		}
		if err := os.WriteFile(filepath.Join(localRoot, "generated.txt"), []byte("generated\n"), 0o644); err != nil {
			t.Fatalf("write sandbox generated file: %v", err)
		}
	})

	stdout, stdoutText := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyOutCommand{
		clientFlags: clientFlags{Host: fixture.host},
		DryRun:      true,
		SandboxID:   fixture.sandboxID,
	}
	if err := cmd.Run(&runtimeContext{
		CWD:           t.TempDir(),
		Loader:        repositoryNotFoundLoader{},
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("WorkspaceCopyOutCommand.Run returned error: %v", err)
	}

	readme, err := os.ReadFile(filepath.Join(fixture.localRoot, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	if got, want := string(readme), "local\n"; got != want {
		t.Fatalf("dry-run should not modify README: got %q want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(fixture.localRoot, "generated.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run should not write generated file, got err %v", err)
	}

	expected := strings.Join([]string{
		"write\t" + filepath.Join(fixture.resolvedLocalRoot, "README.md"),
		"write\t" + filepath.Join(fixture.resolvedLocalRoot, "generated.txt"),
		"",
	}, "\n")
	if got := stdoutText(); got != expected {
		t.Fatalf("unexpected copy-out dry-run output: got %q want %q", got, expected)
	}
}

func TestWorkspaceCopyOutRejectsLocalDivergenceFromCopyInManifestBase(t *testing.T) {
	fixture := setupWorkspaceCopyOutManifestBase(t, func(localRoot string) {
		if err := os.WriteFile(filepath.Join(localRoot, "README.md"), []byte("local\nsandbox\n"), 0o644); err != nil {
			t.Fatalf("write sandbox README: %v", err)
		}
	})
	if err := os.WriteFile(filepath.Join(fixture.localRoot, "README.md"), []byte("local divergent\n"), 0o644); err != nil {
		t.Fatalf("write divergent local README: %v", err)
	}

	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyOutCommand{
		clientFlags: clientFlags{Host: fixture.host},
		SandboxID:   fixture.sandboxID,
	}
	err := cmd.Run(&runtimeContext{
		CWD:           t.TempDir(),
		Loader:        repositoryNotFoundLoader{},
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	})
	if err == nil {
		t.Fatal("expected local divergence from copy-in manifest to be rejected")
	}
	if !strings.Contains(err.Error(), `local workspace path "README.md" changed independently`) {
		t.Fatalf("expected manifest divergence error, got %v", err)
	}
	readme, err := os.ReadFile(filepath.Join(fixture.localRoot, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	if got, want := string(readme), "local divergent\n"; got != want {
		t.Fatalf("copy-out should not overwrite divergent local file: got %q want %q", got, want)
	}
	assertNoWorkspaceCopyOutRecoveryPayload(t)
}

func TestWorkspaceCopyOutReportsAllLocalDivergenceFromCopyInManifestBase(t *testing.T) {
	fixture := setupWorkspaceCopyOutManifestBase(t, func(localRoot string) {
		if err := os.WriteFile(filepath.Join(localRoot, "README.md"), []byte("local\nsandbox\n"), 0o644); err != nil {
			t.Fatalf("write sandbox README: %v", err)
		}
		if err := os.WriteFile(filepath.Join(localRoot, "local.txt"), []byte("local only\nsandbox\n"), 0o644); err != nil {
			t.Fatalf("write sandbox local file: %v", err)
		}
	})
	if err := os.WriteFile(filepath.Join(fixture.localRoot, "README.md"), []byte("local divergent\n"), 0o644); err != nil {
		t.Fatalf("write divergent local README: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fixture.localRoot, "local.txt"), []byte("local divergent\n"), 0o644); err != nil {
		t.Fatalf("write divergent local file: %v", err)
	}

	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyOutCommand{
		clientFlags: clientFlags{Host: fixture.host},
		SandboxID:   fixture.sandboxID,
	}
	err := cmd.Run(&runtimeContext{
		CWD:           t.TempDir(),
		Loader:        repositoryNotFoundLoader{},
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	})
	if err == nil {
		t.Fatal("expected local divergence from copy-in manifest to be rejected")
	}
	for _, want := range []string{
		"workspace copy-out found 2 local conflicts",
		"README.md: changed independently",
		"local.txt: changed independently",
		"--force",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error to contain %q, got %v", want, err)
		}
	}
	assertNoWorkspaceCopyOutRecoveryPayload(t)
}

func TestWorkspaceCopyOutForceOverwritesLocalDivergenceFromCopyInManifestBase(t *testing.T) {
	fixture := setupWorkspaceCopyOutManifestBase(t, func(localRoot string) {
		if err := os.WriteFile(filepath.Join(localRoot, "README.md"), []byte("local\nsandbox\n"), 0o644); err != nil {
			t.Fatalf("write sandbox README: %v", err)
		}
		if err := os.WriteFile(filepath.Join(localRoot, "local.txt"), []byte("local only\nsandbox\n"), 0o644); err != nil {
			t.Fatalf("write sandbox local file: %v", err)
		}
	})
	if err := os.WriteFile(filepath.Join(fixture.localRoot, "README.md"), []byte("local divergent\n"), 0o644); err != nil {
		t.Fatalf("write divergent local README: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fixture.localRoot, "local.txt"), []byte("local divergent\n"), 0o644); err != nil {
		t.Fatalf("write divergent local file: %v", err)
	}

	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyOutCommand{
		clientFlags: clientFlags{Host: fixture.host},
		Force:       true,
		SandboxID:   fixture.sandboxID,
	}
	if err := cmd.Run(&runtimeContext{
		CWD:           t.TempDir(),
		Loader:        repositoryNotFoundLoader{},
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("WorkspaceCopyOutCommand.Run returned error: %v", err)
	}
	for path, want := range map[string]string{
		"README.md": "local\nsandbox\n",
		"local.txt": "local only\nsandbox\n",
	} {
		got, err := os.ReadFile(filepath.Join(fixture.localRoot, path))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(got) != want {
			t.Fatalf("unexpected %s content: got %q want %q", path, got, want)
		}
	}
	assertNoWorkspaceCopyOutRecoveryPayload(t)
}

func TestWorkspaceCopyOutForceClearsStagedDivergenceFromCopyInManifestBase(t *testing.T) {
	fixture := setupWorkspaceCopyOutManifestBase(t, func(localRoot string) {
		if err := os.WriteFile(filepath.Join(localRoot, "README.md"), []byte("local\nsandbox\n"), 0o644); err != nil {
			t.Fatalf("write sandbox README: %v", err)
		}
	})
	readmePath := filepath.Join(fixture.localRoot, "README.md")
	if err := os.WriteFile(readmePath, []byte("staged divergent\n"), 0o644); err != nil {
		t.Fatalf("write staged divergent README: %v", err)
	}
	runGitInDir(t, fixture.localRoot, "add", "README.md")
	if err := os.WriteFile(readmePath, []byte("local\n"), 0o644); err != nil {
		t.Fatalf("restore working tree README: %v", err)
	}

	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyOutCommand{
		clientFlags: clientFlags{Host: fixture.host},
		Force:       true,
		SandboxID:   fixture.sandboxID,
	}
	if err := cmd.Run(&runtimeContext{
		CWD:           t.TempDir(),
		Loader:        repositoryNotFoundLoader{},
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("WorkspaceCopyOutCommand.Run returned error: %v", err)
	}
	readme, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	if got, want := string(readme), "local\nsandbox\n"; got != want {
		t.Fatalf("unexpected README content: got %q want %q", got, want)
	}
	if staged := gitOutputBytes(t, fixture.localRoot, "diff", "--cached", "--name-only", "--", "README.md"); len(staged) != 0 {
		t.Fatalf("force copy-out should clear stale staged target content, got cached diff %q", staged)
	}
	if status := gitOutputBytes(t, fixture.localRoot, "status", "--short", "--", "README.md"); string(status) != " M README.md\n" {
		t.Fatalf("unexpected README status after force copy-out: got %q", status)
	}
	assertNoWorkspaceCopyOutRecoveryPayload(t)
}

func TestWorkspaceCopyOutRejectsLocalModeDivergenceFromCopyInManifestBase(t *testing.T) {
	fixture := setupWorkspaceCopyOutManifestBase(t, func(localRoot string) {
		if err := os.WriteFile(filepath.Join(localRoot, "README.md"), []byte("local\nsandbox\n"), 0o644); err != nil {
			t.Fatalf("write sandbox README: %v", err)
		}
	})
	readmePath := filepath.Join(fixture.localRoot, "README.md")
	if err := os.Chmod(readmePath, 0o755); err != nil {
		t.Fatalf("chmod local README: %v", err)
	}

	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyOutCommand{
		clientFlags: clientFlags{Host: fixture.host},
		SandboxID:   fixture.sandboxID,
	}
	err := cmd.Run(&runtimeContext{
		CWD:           t.TempDir(),
		Loader:        repositoryNotFoundLoader{},
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	})
	if err == nil {
		t.Fatal("expected local mode divergence from copy-in manifest to be rejected")
	}
	if !strings.Contains(err.Error(), `local workspace path "README.md" changed independently`) {
		t.Fatalf("expected manifest divergence error, got %v", err)
	}
	info, err := os.Stat(readmePath)
	if err != nil {
		t.Fatalf("stat README: %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o755); got != want {
		t.Fatalf("copy-out should leave local README mode unchanged: got %o want %o", got, want)
	}
	readme, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	if got, want := string(readme), "local\n"; got != want {
		t.Fatalf("copy-out should leave local README content unchanged: got %q want %q", got, want)
	}
	assertNoWorkspaceCopyOutRecoveryPayload(t)
}

func TestWorkspaceCopyOutAppliesLiteralPathspecFromCopyInManifestBase(t *testing.T) {
	fixture := setupWorkspaceCopyOutManifestBaseWithLocalMutate(t, func(localRoot string) {
		if err := os.WriteFile(filepath.Join(localRoot, "A.txt"), []byte("decoy\n"), 0o755); err != nil {
			t.Fatalf("write decoy file: %v", err)
		}
		if err := os.Chmod(filepath.Join(localRoot, "A.txt"), 0o755); err != nil {
			t.Fatalf("chmod decoy file: %v", err)
		}
		if err := os.WriteFile(filepath.Join(localRoot, "[AB].txt"), []byte("local brackets\n"), 0o644); err != nil {
			t.Fatalf("write bracket file: %v", err)
		}
	}, func(localRoot string) {
		if err := os.WriteFile(filepath.Join(localRoot, "[AB].txt"), []byte("local brackets\nsandbox\n"), 0o644); err != nil {
			t.Fatalf("write sandbox bracket file: %v", err)
		}
	})

	stdout, stdoutText := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyOutCommand{
		clientFlags: clientFlags{Host: fixture.host},
		SandboxID:   fixture.sandboxID,
	}
	if err := cmd.Run(&runtimeContext{
		CWD:           t.TempDir(),
		Loader:        repositoryNotFoundLoader{},
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("WorkspaceCopyOutCommand.Run returned error: %v", err)
	}

	bracketPath := filepath.Join(fixture.localRoot, "[AB].txt")
	bracket, err := os.ReadFile(bracketPath)
	if err != nil {
		t.Fatalf("read bracket file: %v", err)
	}
	if got, want := string(bracket), "local brackets\nsandbox\n"; got != want {
		t.Fatalf("unexpected bracket file content: got %q want %q", got, want)
	}
	decoyInfo, err := os.Stat(filepath.Join(fixture.localRoot, "A.txt"))
	if err != nil {
		t.Fatalf("stat decoy file: %v", err)
	}
	if got, want := decoyInfo.Mode().Perm(), os.FileMode(0o755); got != want {
		t.Fatalf("copy-out should leave decoy mode unchanged: got %o want %o", got, want)
	}
	expected := strings.Join([]string{
		"write\t" + filepath.Join(fixture.resolvedLocalRoot, "[AB].txt"),
		"",
	}, "\n")
	if got := stdoutText(); got != expected {
		t.Fatalf("unexpected copy-out output: got %q want %q", got, expected)
	}
	assertNoWorkspaceCopyOutRecoveryPayload(t)
}

func TestWorkspaceCopyOutRejectsStagedDivergenceFromCopyInManifestBase(t *testing.T) {
	fixture := setupWorkspaceCopyOutManifestBase(t, func(localRoot string) {
		if err := os.WriteFile(filepath.Join(localRoot, "README.md"), []byte("local\nsandbox\n"), 0o644); err != nil {
			t.Fatalf("write sandbox README: %v", err)
		}
	})
	readmePath := filepath.Join(fixture.localRoot, "README.md")
	if err := os.WriteFile(readmePath, []byte("staged divergent\n"), 0o644); err != nil {
		t.Fatalf("write staged divergent README: %v", err)
	}
	runGitInDir(t, fixture.localRoot, "add", "README.md")
	if err := os.WriteFile(readmePath, []byte("local\n"), 0o644); err != nil {
		t.Fatalf("restore working tree README: %v", err)
	}

	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyOutCommand{
		clientFlags: clientFlags{Host: fixture.host},
		SandboxID:   fixture.sandboxID,
	}
	err := cmd.Run(&runtimeContext{
		CWD:           t.TempDir(),
		Loader:        repositoryNotFoundLoader{},
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	})
	if err == nil {
		t.Fatal("expected staged divergence from copy-in manifest to be rejected")
	}
	if !strings.Contains(err.Error(), `local workspace path "README.md" changed independently`) {
		t.Fatalf("expected staged divergence error, got %v", err)
	}
	readme, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	if got, want := string(readme), "local\n"; got != want {
		t.Fatalf("copy-out should leave local README content unchanged: got %q want %q", got, want)
	}
	staged := gitOutputBytes(t, fixture.localRoot, "show", ":README.md")
	if got, want := string(staged), "staged divergent\n"; got != want {
		t.Fatalf("copy-out should leave staged README content unchanged: got %q want %q", got, want)
	}
	assertNoWorkspaceCopyOutRecoveryPayload(t)
}

func TestWorkspaceCopyOutRejectsLocalDivergence(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	localRoot := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	baseCommit := headCommit(t, localRoot)

	if err := os.WriteFile(filepath.Join(localRoot, "README.md"), []byte("sandbox\n"), 0o644); err != nil {
		t.Fatalf("write sandbox readme: %v", err)
	}
	runGitInDir(t, localRoot, "add", "README.md")
	nameStatus := gitOutputBytes(t, localRoot, "diff", "--cached", "--name-status", "--no-renames", "-z", baseCommit)
	patch := gitOutputBytes(t, localRoot, "diff", "--cached", "--binary", "--full-index", "--no-ext-diff", "--no-color", "--no-renames", baseCommit)
	runGitInDir(t, localRoot, "reset", "--hard", baseCommit)
	if err := os.WriteFile(filepath.Join(localRoot, "README.md"), []byte("local\n"), 0o644); err != nil {
		t.Fatalf("write local readme: %v", err)
	}

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createWorkspaceCopyTestSandboxWithRepositoryCommitBranch(t, host, "/sandbox-workspace", baseCommit, "main")
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		command := strings.Join(req.Command, " ")
		switch {
		case strings.Contains(command, "cleanroom-copy-out-v1"):
			if stream.OnStdout != nil {
				stream.OnStdout(workspaceCopyOutTestPayload(nameStatus, patch))
			}
		default:
			t.Fatalf("unexpected workspace copy-out command: %q", command)
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyOutCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       localRoot,
		SandboxID:   sandboxID,
	}
	err := cmd.Run(&runtimeContext{
		CWD:           localRoot,
		Loader:        repositoryNotFoundLoader{},
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	})
	if err == nil {
		t.Fatal("expected local divergence to be rejected")
	}
	if !strings.Contains(err.Error(), `local workspace path "README.md" changed independently`) {
		t.Fatalf("expected local path divergence error, got %v", err)
	}
	readme, err := os.ReadFile(filepath.Join(localRoot, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	if got, want := string(readme), "local\n"; got != want {
		t.Fatalf("copy-out should not overwrite divergent local file: got %q want %q", got, want)
	}
	assertNoWorkspaceCopyOutRecoveryPayload(t)
}

func TestWorkspaceCopyOutRejectsIgnoredLocalTarget(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	localRoot := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	if err := os.WriteFile(filepath.Join(localRoot, ".gitignore"), []byte("ignored/\n"), 0o644); err != nil {
		t.Fatalf("write gitignore: %v", err)
	}
	runGitInDir(t, localRoot, "add", ".gitignore")
	runGitInDir(t, localRoot, "commit", "-m", "ignore output")
	baseCommit := headCommit(t, localRoot)

	if err := os.MkdirAll(filepath.Join(localRoot, "ignored"), 0o755); err != nil {
		t.Fatalf("create ignored dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localRoot, "ignored", "output.txt"), []byte("sandbox\n"), 0o644); err != nil {
		t.Fatalf("write sandbox ignored output: %v", err)
	}
	runGitInDir(t, localRoot, "add", "-f", "ignored/output.txt")
	nameStatus := gitOutputBytes(t, localRoot, "diff", "--cached", "--name-status", "--no-renames", "-z", baseCommit)
	patch := gitOutputBytes(t, localRoot, "diff", "--cached", "--binary", "--full-index", "--no-ext-diff", "--no-color", "--no-renames", baseCommit)
	runGitInDir(t, localRoot, "reset", "--hard", baseCommit)
	if err := os.MkdirAll(filepath.Join(localRoot, "ignored"), 0o755); err != nil {
		t.Fatalf("recreate ignored dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localRoot, "ignored", "output.txt"), []byte("local ignored\n"), 0o644); err != nil {
		t.Fatalf("write local ignored output: %v", err)
	}

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createWorkspaceCopyTestSandboxWithRepositoryCommitBranch(t, host, "/sandbox-workspace", baseCommit, "main")
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		command := strings.Join(req.Command, " ")
		switch {
		case strings.Contains(command, "cleanroom-copy-out-v1"):
			if stream.OnStdout != nil {
				stream.OnStdout(workspaceCopyOutTestPayload(nameStatus, patch))
			}
		default:
			t.Fatalf("unexpected workspace copy-out command: %q", command)
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyOutCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       localRoot,
		SandboxID:   sandboxID,
	}
	err := cmd.Run(&runtimeContext{
		CWD:           localRoot,
		Loader:        repositoryNotFoundLoader{},
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	})
	if err == nil {
		t.Fatal("expected ignored local target to be rejected")
	}
	if !strings.Contains(err.Error(), `local workspace path "ignored/output.txt" exists outside sandbox baseline`) {
		t.Fatalf("expected ignored local path conflict, got %v", err)
	}
	output, err := os.ReadFile(filepath.Join(localRoot, "ignored", "output.txt"))
	if err != nil {
		t.Fatalf("read ignored output: %v", err)
	}
	if got, want := string(output), "local ignored\n"; got != want {
		t.Fatalf("copy-out should not overwrite ignored local file: got %q want %q", got, want)
	}
	assertNoWorkspaceCopyOutRecoveryPayload(t)
}

func TestWorkspaceCopyOutRejectsBaselineMismatch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	localRoot := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	baseCommit := headCommit(t, localRoot)

	if err := os.WriteFile(filepath.Join(localRoot, "README.md"), []byte("sandbox\n"), 0o644); err != nil {
		t.Fatalf("write sandbox readme: %v", err)
	}
	runGitInDir(t, localRoot, "add", "README.md")
	nameStatus := gitOutputBytes(t, localRoot, "diff", "--cached", "--name-status", "--no-renames", "-z", baseCommit)
	patch := gitOutputBytes(t, localRoot, "diff", "--cached", "--binary", "--full-index", "--no-ext-diff", "--no-color", "--no-renames", baseCommit)
	runGitInDir(t, localRoot, "reset", "--hard", baseCommit)
	if err := os.WriteFile(filepath.Join(localRoot, "local.txt"), []byte("local commit\n"), 0o644); err != nil {
		t.Fatalf("write local commit file: %v", err)
	}
	runGitInDir(t, localRoot, "add", "local.txt")
	runGitInDir(t, localRoot, "commit", "-m", "local commit")

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createWorkspaceCopyTestSandboxWithRepositoryCommitBranch(t, host, "/sandbox-workspace", baseCommit, "main")
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		command := strings.Join(req.Command, " ")
		switch {
		case strings.Contains(command, "cleanroom-copy-out-v1"):
			if stream.OnStdout != nil {
				stream.OnStdout(workspaceCopyOutTestPayload(nameStatus, patch))
			}
		default:
			t.Fatalf("unexpected workspace copy-out command: %q", command)
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyOutCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       localRoot,
		SandboxID:   sandboxID,
	}
	err := cmd.Run(&runtimeContext{
		CWD:           localRoot,
		Loader:        repositoryNotFoundLoader{},
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	})
	if err == nil {
		t.Fatal("expected baseline mismatch to be rejected")
	}
	if !strings.Contains(err.Error(), "requires local checkout HEAD") {
		t.Fatalf("expected baseline mismatch error, got %v", err)
	}
	assertNoWorkspaceCopyOutRecoveryPayload(t)
}

func TestWorkspaceCopyOutForceAllowsBaselineMismatch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	localRoot := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	baseCommit := headCommit(t, localRoot)

	if err := os.WriteFile(filepath.Join(localRoot, "README.md"), []byte("sandbox\n"), 0o644); err != nil {
		t.Fatalf("write sandbox readme: %v", err)
	}
	runGitInDir(t, localRoot, "add", "README.md")
	nameStatus := gitOutputBytes(t, localRoot, "diff", "--cached", "--name-status", "--no-renames", "-z", baseCommit)
	patch := gitOutputBytes(t, localRoot, "diff", "--cached", "--binary", "--full-index", "--no-ext-diff", "--no-color", "--no-renames", baseCommit)
	runGitInDir(t, localRoot, "reset", "--hard", baseCommit)
	if err := os.WriteFile(filepath.Join(localRoot, "local.txt"), []byte("local commit\n"), 0o644); err != nil {
		t.Fatalf("write local commit file: %v", err)
	}
	runGitInDir(t, localRoot, "add", "local.txt")
	runGitInDir(t, localRoot, "commit", "-m", "local commit")

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createWorkspaceCopyTestSandboxWithRepositoryCommitBranch(t, host, "/sandbox-workspace", baseCommit, "main")
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		command := strings.Join(req.Command, " ")
		switch {
		case strings.Contains(command, "cleanroom-copy-out-v1"):
			if stream.OnStdout != nil {
				stream.OnStdout(workspaceCopyOutTestPayload(nameStatus, patch))
			}
		default:
			t.Fatalf("unexpected workspace copy-out command: %q", command)
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyOutCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       localRoot,
		Force:       true,
		SandboxID:   sandboxID,
	}
	if err := cmd.Run(&runtimeContext{
		CWD:           localRoot,
		Loader:        repositoryNotFoundLoader{},
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("WorkspaceCopyOutCommand.Run returned error: %v", err)
	}
	readme, err := os.ReadFile(filepath.Join(localRoot, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	if got, want := string(readme), "sandbox\n"; got != want {
		t.Fatalf("unexpected README content: got %q want %q", got, want)
	}
	local, err := os.ReadFile(filepath.Join(localRoot, "local.txt"))
	if err != nil {
		t.Fatalf("read local file: %v", err)
	}
	if got, want := string(local), "local commit\n"; got != want {
		t.Fatalf("force copy-out should leave untargeted local file alone: got %q want %q", got, want)
	}
	assertNoWorkspaceCopyOutRecoveryPayload(t)
}

func TestWorkspaceCopyOutForceDryRunUsesLocalApplyBase(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	localRoot := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	baseCommit := headCommit(t, localRoot)

	if err := os.WriteFile(filepath.Join(localRoot, "README.md"), []byte("sandbox\n"), 0o644); err != nil {
		t.Fatalf("write sandbox readme: %v", err)
	}
	runGitInDir(t, localRoot, "add", "README.md")
	nameStatus := gitOutputBytes(t, localRoot, "diff", "--cached", "--name-status", "--no-renames", "-z", baseCommit)
	patch := gitOutputBytes(t, localRoot, "diff", "--cached", "--binary", "--full-index", "--no-ext-diff", "--no-color", "--no-renames", baseCommit)
	runGitInDir(t, localRoot, "commit", "-m", "local already has sandbox readme")

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createWorkspaceCopyTestSandboxWithRepositoryCommitBranch(t, host, "/sandbox-workspace", baseCommit, "main")
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		command := strings.Join(req.Command, " ")
		switch {
		case strings.Contains(command, "cleanroom-copy-out-v1"):
			if stream.OnStdout != nil {
				stream.OnStdout(workspaceCopyOutTestPayload(nameStatus, patch))
			}
		default:
			t.Fatalf("unexpected workspace copy-out command: %q", command)
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	stdout, stdoutText := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyOutCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       localRoot,
		DryRun:      true,
		Force:       true,
		SandboxID:   sandboxID,
	}
	if err := cmd.Run(&runtimeContext{
		CWD:           localRoot,
		Loader:        repositoryNotFoundLoader{},
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("WorkspaceCopyOutCommand.Run returned error: %v", err)
	}
	if got := stdoutText(); got != "" {
		t.Fatalf("force dry-run should plan from local apply base: got %q", got)
	}
	assertNoWorkspaceCopyOutRecoveryPayload(t)
}

func TestWorkspaceCopyOutPlanRejectsWindowsDrivePaths(t *testing.T) {
	for _, rel := range []string{
		"C:/tmp.txt",
		"c:/tmp.txt",
		"D:\\tmp.txt",
	} {
		t.Run(rel, func(t *testing.T) {
			_, err := gitWorkspaceCopyOutPlan(t.TempDir(), []byte("M\x00"+rel+"\x00"))
			if err == nil {
				t.Fatal("expected Windows drive path to be rejected")
			}
			if !strings.Contains(err.Error(), "not a safe relative path") {
				t.Fatalf("expected safe relative path error, got %v", err)
			}
		})
	}
}

func TestWorkspaceCopyOutPlanAllowsRelativeColonPaths(t *testing.T) {
	localRoot := t.TempDir()
	entries, err := gitWorkspaceCopyOutPlan(localRoot, []byte("M\x00a:b.txt\x00A\x00dir/Z:tmp.txt\x00"))
	if err != nil {
		t.Fatalf("gitWorkspaceCopyOutPlan returned error: %v", err)
	}
	expected := []workspacePlanEntry{
		{Action: "write", Path: filepath.Join(localRoot, "a:b.txt")},
		{Action: "write", Path: filepath.Join(localRoot, "dir", "Z:tmp.txt")},
	}
	if !reflect.DeepEqual(entries, expected) {
		t.Fatalf("unexpected copy-out plan: got %+v want %+v", entries, expected)
	}
}

func TestParseGitWorkspaceCopyOutPayload(t *testing.T) {
	nameStatus := []byte("M\x00changed.txt\x00")
	patch := []byte("diff --git a/changed.txt b/changed.txt\n")
	gotNameStatus, gotPatch, err := parseGitWorkspaceCopyOutPayload(workspaceCopyOutTestPayload(nameStatus, patch))
	if err != nil {
		t.Fatalf("parseGitWorkspaceCopyOutPayload returned error: %v", err)
	}
	if !bytes.Equal(gotNameStatus, nameStatus) {
		t.Fatalf("unexpected name-status payload: got %q want %q", gotNameStatus, nameStatus)
	}
	if !bytes.Equal(gotPatch, patch) {
		t.Fatalf("unexpected patch payload: got %q want %q", gotPatch, patch)
	}
	if _, _, err := parseGitWorkspaceCopyOutPayload([]byte("cleanroom-copy-out-v1 1 1\nabc")); err == nil {
		t.Fatal("expected trailing payload data to be rejected")
	}
}

func TestWorkspaceCopyOutDryRunRejectsNonGitLocalRoot(t *testing.T) {
	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createWorkspaceCopyTestSandboxWithRepository(t, host, "/sandbox-workspace")

	var executionCalled bool
	adapter.runStreamFn = func(_ context.Context, _ backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
		executionCalled = true
		return &backend.ExecutionResult{ExitCode: 0, Message: "ok"}, nil
	}

	cwd := t.TempDir()
	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyOutCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		DryRun:      true,
		SandboxID:   sandboxID,
	}
	err := cmd.Run(&runtimeContext{
		CWD:           cwd,
		Loader:        repositoryNotFoundLoader{},
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	})
	if err == nil {
		t.Fatal("expected non-Git copy-out root to be rejected")
	}
	if !strings.Contains(err.Error(), "requires a local Git repository checkout") {
		t.Fatalf("expected local Git checkout error, got %v", err)
	}
	if executionCalled {
		t.Fatal("expected copy-out to fail before sandbox planning execution")
	}
}

func TestWorkspaceCopyOutDryRunRejectsMismatchedLocalRepository(t *testing.T) {
	localRoot := initGitRepository(t, "https://github.com/buildkite/not-cleanroom.git")
	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createWorkspaceCopyTestSandboxWithRepository(t, host, "/sandbox-workspace")

	var executionCalled bool
	adapter.runStreamFn = func(_ context.Context, _ backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
		executionCalled = true
		return &backend.ExecutionResult{ExitCode: 0, Message: "ok"}, nil
	}

	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyOutCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       localRoot,
		DryRun:      true,
		SandboxID:   sandboxID,
	}
	err := cmd.Run(&runtimeContext{
		CWD:           localRoot,
		Loader:        repositoryNotFoundLoader{},
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	})
	if err == nil {
		t.Fatal("expected mismatched local repository to be rejected")
	}
	if !strings.Contains(err.Error(), "does not match sandbox repository remote") {
		t.Fatalf("expected repository mismatch error, got %v", err)
	}
	if executionCalled {
		t.Fatal("expected copy-out to fail before sandbox planning execution")
	}
}

func TestWorkspaceDiffStreamsSandboxGitDiff(t *testing.T) {
	diff := []byte("diff --git a/changed.txt b/changed.txt\n+changed\n")
	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createWorkspaceCopyTestSandboxWithRepository(t, host, "/sandbox-workspace")

	var (
		mu       sync.Mutex
		commands [][]string
	)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		if stream.OnStdout != nil {
			stream.OnStdout(diff)
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	stdout, stdoutText := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceDiffCommand{
		clientFlags: clientFlags{Host: host},
		SandboxID:   sandboxID,
	}
	if err := cmd.Run(&runtimeContext{
		CWD:           t.TempDir(),
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	}); err != nil {
		t.Fatalf("WorkspaceDiffCommand.Run returned error: %v", err)
	}
	if got := stdoutText(); got != string(diff) {
		t.Fatalf("unexpected workspace diff output: got %q want %q", got, diff)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(commands) != 1 {
		t.Fatalf("expected one workspace diff execution, got %d", len(commands))
	}
	diffCommand := strings.Join(commands[0], " ")
	if !strings.Contains(diffCommand, "dest='/sandbox-workspace'") {
		t.Fatalf("expected workspace diff to use sandbox workspace root, got %q", diffCommand)
	}
	if !strings.Contains(diffCommand, "diff --cached --binary --full-index") {
		t.Fatalf("expected workspace diff to use git binary diff, got %q", diffCommand)
	}
}

func TestWorkspaceCopyInRejectsNonGitWorkspace(t *testing.T) {
	sourceRoot := t.TempDir()
	adapter := &copyIntegrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createWorkspaceCopyTestSandboxWithRepository(t, host, "/workspace-app")
	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)

	cmd := WorkspaceCopyInCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       sourceRoot,
		SandboxID:   sandboxID,
	}
	err := cmd.Run(&runtimeContext{
		CWD:           sourceRoot,
		Loader:        repositoryNotFoundLoader{},
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	})
	if err == nil {
		t.Fatal("expected non-Git workspace copy-in to be rejected")
	}
	if !strings.Contains(err.Error(), "requires a local Git repository checkout") {
		t.Fatalf("expected Git checkout error, got %v", err)
	}
}

func TestWorkspaceCopyInRejectsGitCopyWhenSandboxWorkspaceRootUnknown(t *testing.T) {
	repoDir := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	if err := os.WriteFile(filepath.Join(repoDir, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}
	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createWorkspaceCopyTestSandbox(t, host, workspaceCopyTestPolicy())

	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)
	cmd := WorkspaceCopyInCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       repoDir,
		SandboxID:   sandboxID,
	}
	err := cmd.Run(&runtimeContext{
		CWD:           repoDir,
		Loader:        workspaceCopyRepositoryLoader(),
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
		Stdout:        stdout,
		Stderr:        stderr,
	})
	if err == nil {
		t.Fatal("expected git workspace copy-in to reject sandbox without recorded workspace root")
	}
	if !strings.Contains(err.Error(), "does not have a recorded workspace root") {
		t.Fatalf("expected recorded workspace root error, got %v", err)
	}
}

func TestTopLevelCopyInRejectsNonGitWorkspace(t *testing.T) {
	for _, existingSandboxID := range []string{"", "cr_existing"} {
		t.Run(fmt.Sprintf("existing=%t", existingSandboxID != ""), func(t *testing.T) {
			cwd := t.TempDir()
			_, err := resolveExecutionSandbox(
				context.Background(),
				nil,
				&runtimeContext{
					CWD:    cwd,
					Loader: repositoryNotFoundLoader{},
				},
				cwd,
				"",
				"",
				existingSandboxID,
				"",
				"",
				0,
				false,
				repositoryOverrideFlags{},
				workspaceCopyFlags{CopyIn: true},
			)
			if err == nil {
				t.Fatal("expected non-Git top-level --copy-in to be rejected")
			}
			if !strings.Contains(err.Error(), "requires a local Git repository checkout") {
				t.Fatalf("expected non-Git copy error, got %v", err)
			}
		})
	}
}

func createWorkspaceCopyTestSandbox(t *testing.T, host string, compiled *cleanroomv1.Policy) string {
	t.Helper()

	client := mustNewControlClient(t, host)
	resp, err := client.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Backend: "firecracker",
		Policy:  compiled,
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	sandboxID := strings.TrimSpace(resp.GetSandbox().GetSandboxId())
	if sandboxID == "" {
		t.Fatal("create sandbox response missing sandbox id")
	}
	return sandboxID
}

func createWorkspaceCopyTestSandboxWithRepository(t *testing.T, host, destination string) string {
	return createWorkspaceCopyTestSandboxWithRepositoryCommitBranch(t, host, destination, "0123456789abcdef0123456789abcdef01234567", "")
}

func createWorkspaceCopyTestSandboxWithRepositoryCommitBranch(t *testing.T, host, destination, commit, branch string) string {
	t.Helper()

	client := mustNewControlClient(t, host)
	resp, err := client.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Backend: "firecracker",
		Policy:  workspaceCopyTestPolicy(),
		RepositoryCheckout: &cleanroomv1.RepositoryCheckout{
			RemoteUrl:      "https://github.com/buildkite/cleanroom.git",
			CommitSha:      strings.TrimSpace(commit),
			DestinationDir: destination,
			Branch:         strings.TrimSpace(branch),
		},
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	sandboxID := strings.TrimSpace(resp.GetSandbox().GetSandboxId())
	if sandboxID == "" {
		t.Fatal("create sandbox response missing sandbox id")
	}
	return sandboxID
}

func workspaceCopyTestPolicy() *cleanroomv1.Policy {
	return (&policy.CompiledPolicy{
		Version:        1,
		ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		NetworkDefault: "deny",
		Allow:          []policy.AllowRule{{Host: "github.com", Ports: []int{443}}},
	}).ToProto()
}

func workspaceCopyRepositoryLoader() repositoryIntegrationLoader {
	return repositoryIntegrationLoader{
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
}

type workspaceCopyOutManifestFixture struct {
	localRoot         string
	resolvedLocalRoot string
	host              string
	sandboxID         string
}

func setupWorkspaceCopyOutManifestBase(t *testing.T, sandboxMutate func(string)) workspaceCopyOutManifestFixture {
	t.Helper()
	return setupWorkspaceCopyOutManifestBaseWithLocalMutate(t, nil, sandboxMutate)
}

func setupWorkspaceCopyOutManifestBaseWithLocalMutate(t *testing.T, localMutate, sandboxMutate func(string)) workspaceCopyOutManifestFixture {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	localRoot := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	resolvedLocalRoot, err := gitOutput(localRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatalf("resolve local repository root: %v", err)
	}
	baseCommit := headCommit(t, localRoot)
	checkout := &repositorycheckout.Checkout{
		RemoteURL:      "https://github.com/buildkite/cleanroom.git",
		CommitSHA:      baseCommit,
		DestinationDir: "/sandbox-workspace",
		Branch:         "main",
	}

	if err := os.WriteFile(filepath.Join(localRoot, "README.md"), []byte("local\n"), 0o644); err != nil {
		t.Fatalf("write local README: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localRoot, "local.txt"), []byte("local only\n"), 0o644); err != nil {
		t.Fatalf("write local-only file: %v", err)
	}
	if localMutate != nil {
		localMutate(localRoot)
	}
	runGitInDir(t, localRoot, "add", "-A")
	runGitInDir(t, localRoot, "commit", "-m", "local copy-in state")
	copyInCommit := headCommit(t, localRoot)

	changeset, err := repositorychangeset.BuildFromWorkingTree(localRoot, checkout)
	if err != nil {
		t.Fatalf("build copy-in manifest changeset: %v", err)
	}
	if changeset == nil {
		t.Fatal("expected copy-in manifest changeset")
	}
	repository, err := resolveWorkspaceCopyRepositoryCheckout(localRoot, repositoryNotFoundLoader{})
	if err != nil {
		t.Fatalf("resolve local repository checkout: %v", err)
	}

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createWorkspaceCopyTestSandboxWithRepositoryCommitBranch(t, host, "/sandbox-workspace", baseCommit, "main")
	if err := recordGitWorkspaceBinding(sandboxID, repository, checkout, changeset.Files, "copy-in"); err != nil {
		t.Fatalf("record workspace binding: %v", err)
	}

	sandboxMutate(localRoot)
	runGitInDir(t, localRoot, "add", "-A")
	nameStatus := gitOutputBytes(t, localRoot, "diff", "--cached", "--name-status", "--no-renames", "-z", baseCommit)
	patch := gitOutputBytes(t, localRoot, "diff", "--cached", "--binary", "--full-index", "--no-ext-diff", "--no-color", "--no-renames", baseCommit)
	runGitInDir(t, localRoot, "reset", "--hard", copyInCommit)
	runGitInDir(t, localRoot, "clean", "-fd")

	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		command := strings.Join(req.Command, " ")
		switch {
		case strings.Contains(command, "cleanroom-copy-out-v1"):
			if stream.OnStdout != nil {
				stream.OnStdout(workspaceCopyOutTestPayload(nameStatus, patch))
			}
		default:
			t.Fatalf("unexpected workspace copy-out command: %q", command)
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	return workspaceCopyOutManifestFixture{
		localRoot:         localRoot,
		resolvedLocalRoot: resolvedLocalRoot,
		host:              host,
		sandboxID:         sandboxID,
	}
}

func assertNoWorkspaceCopyOutRecoveryPayload(t *testing.T) {
	t.Helper()
	recoveryRoot := filepath.Join(os.Getenv("XDG_STATE_HOME"), "cleanroom", "workspace-copy-out")
	entries, err := os.ReadDir(recoveryRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		t.Fatalf("read workspace copy-out recovery root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no workspace copy-out recovery payloads, got %d", len(entries))
	}
}

func mustReadWorkspaceBinding(t *testing.T, sandboxID string) *workspaceBinding {
	t.Helper()
	binding, err := readWorkspaceBinding(sandboxID)
	if err != nil {
		t.Fatalf("readWorkspaceBinding returned error: %v", err)
	}
	if binding == nil {
		t.Fatalf("expected workspace binding for %q", sandboxID)
	}
	return binding
}

func mustNormalizeWorkspaceLocalRoot(t *testing.T, root string) string {
	t.Helper()
	normalized, err := normalizeWorkspaceLocalRoot(root)
	if err != nil {
		t.Fatalf("normalizeWorkspaceLocalRoot returned error: %v", err)
	}
	return normalized
}

func setBrokenWorkspaceStateHome(t *testing.T) {
	t.Helper()
	stateHome := filepath.Join(t.TempDir(), "state-home-file")
	if err := os.WriteFile(stateHome, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatalf("write broken XDG_STATE_HOME: %v", err)
	}
	t.Setenv("XDG_STATE_HOME", stateHome)
}

func gitOutputBytes(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
	return out
}

func workspaceCopyOutTestPayload(nameStatus, patch []byte) []byte {
	payload := []byte(fmt.Sprintf("cleanroom-copy-out-v1 %d %d\n", len(nameStatus), len(patch)))
	payload = append(payload, nameStatus...)
	payload = append(payload, patch...)
	return payload
}

func runGitInDir(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
}
