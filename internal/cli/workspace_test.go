package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
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
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

func TestWorkspaceCopyInAppliesGitChangeset(t *testing.T) {
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

func TestWorkspaceCopyInUsesRawArchiveForNonGitWorkspace(t *testing.T) {
	sourceRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sourceRoot, "app"), 0o755); err != nil {
		t.Fatalf("create app dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "app/main.txt"), []byte("payload\n"), 0o644); err != nil {
		t.Fatalf("write workspace file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(sourceRoot, ".git"), 0o755); err != nil {
		t.Fatalf("create skipped git dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, ".git/config"), []byte("skip\n"), 0o644); err != nil {
		t.Fatalf("write skipped git config: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(sourceRoot, "app/submodule"), 0o755); err != nil {
		t.Fatalf("create submodule dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "app/submodule/.git"), []byte("gitdir: ../../.git/modules/submodule\n"), 0o644); err != nil {
		t.Fatalf("write skipped git file: %v", err)
	}

	var (
		mu          sync.Mutex
		commands    [][]string
		destination string
		files       = map[string]string{}
	)
	adapter := &copyIntegrationAdapter{
		statFn: func(_ context.Context, _ string, path string) (*backend.SandboxPathInfo, error) {
			return &backend.SandboxPathInfo{
				Path: path,
				Type: backend.SandboxPathTypeDirectory,
			}, nil
		},
		extractFn: func(_ context.Context, _ string, dest string, r io.Reader) (int64, error) {
			destination = dest
			tr := tar.NewReader(r)
			for {
				header, err := tr.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					return 0, err
				}
				if header.Typeflag != tar.TypeReg {
					continue
				}
				data, err := io.ReadAll(tr)
				if err != nil {
					return 0, err
				}
				files[header.Name] = string(data)
			}
			return 0, nil
		},
	}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createWorkspaceCopyTestSandboxWithRepository(t, host, "/workspace-app")
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}
	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)

	cmd := WorkspaceCopyInCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       sourceRoot,
		SandboxID:   sandboxID,
	}
	if err := cmd.Run(&runtimeContext{
		CWD:           sourceRoot,
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
	if len(commands) != 1 {
		t.Fatalf("expected one raw workspace clean command, got %d", len(commands))
	}
	cleanCommand := strings.Join(commands[0], " ")
	if !strings.Contains(cleanCommand, `basename "$entry"`) || !strings.Contains(cleanCommand, `= ".git"`) {
		t.Fatalf("expected raw workspace clean command to preserve .git, got %q", cleanCommand)
	}
	if got, want := destination, "/workspace-app"; got != want {
		t.Fatalf("unexpected extract destination: got %q want %q", got, want)
	}
	if got, want := files["app/main.txt"], "payload\n"; got != want {
		t.Fatalf("unexpected archived file content: got %q want %q", got, want)
	}
	if _, ok := files[".git/config"]; ok {
		t.Fatalf("expected .git directory to be skipped, got files %#v", files)
	}
	if _, ok := files["app/submodule/.git"]; ok {
		t.Fatalf("expected .git file to be skipped, got files %#v", files)
	}
}

func TestWorkspaceCopyInSkipsRawCleanWhenDestinationMissing(t *testing.T) {
	sourceRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sourceRoot, "app"), 0o755); err != nil {
		t.Fatalf("create app dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "app/main.txt"), []byte("payload\n"), 0o644); err != nil {
		t.Fatalf("write workspace file: %v", err)
	}

	var (
		mu          sync.Mutex
		commands    [][]string
		destination string
		files       = map[string]string{}
	)
	adapter := &copyIntegrationAdapter{
		statFn: func(_ context.Context, _ string, path string) (*backend.SandboxPathInfo, error) {
			return nil, backend.NewSandboxPathNotFoundError(path)
		},
		extractFn: func(_ context.Context, _ string, dest string, r io.Reader) (int64, error) {
			destination = dest
			tr := tar.NewReader(r)
			for {
				header, err := tr.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					return 0, err
				}
				if header.Typeflag != tar.TypeReg {
					continue
				}
				data, err := io.ReadAll(tr)
				if err != nil {
					return 0, err
				}
				files[header.Name] = string(data)
			}
			return 0, nil
		},
	}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createWorkspaceCopyTestSandboxWithRepository(t, host, "/workspace-app")
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}
	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)

	cmd := WorkspaceCopyInCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       sourceRoot,
		SandboxID:   sandboxID,
	}
	if err := cmd.Run(&runtimeContext{
		CWD:           sourceRoot,
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
	if len(commands) != 0 {
		t.Fatalf("expected missing raw workspace destination to skip clean execution, got %d command(s)", len(commands))
	}
	if got, want := destination, "/workspace-app"; got != want {
		t.Fatalf("unexpected extract destination: got %q want %q", got, want)
	}
	if got, want := files["app/main.txt"], "payload\n"; got != want {
		t.Fatalf("unexpected archived file content: got %q want %q", got, want)
	}
}

func TestWorkspaceCopyInRemovesSymlinkedRawDestinationRoot(t *testing.T) {
	sourceRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceRoot, "main.txt"), []byte("payload\n"), 0o644); err != nil {
		t.Fatalf("write workspace file: %v", err)
	}

	var (
		mu               sync.Mutex
		commands         [][]string
		removedPath      string
		removedRecursive bool
		destination      string
	)
	adapter := &copyIntegrationAdapter{
		statFn: func(_ context.Context, _ string, path string) (*backend.SandboxPathInfo, error) {
			return &backend.SandboxPathInfo{
				Path:          path,
				Type:          backend.SandboxPathTypeSymlink,
				SymlinkTarget: "/",
			}, nil
		},
		removeFn: func(_ context.Context, _ string, path string, recursive bool) error {
			removedPath = path
			removedRecursive = recursive
			return nil
		},
		extractFn: func(_ context.Context, _ string, dest string, r io.Reader) (int64, error) {
			destination = dest
			return io.Copy(io.Discard, r)
		},
	}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createWorkspaceCopyTestSandboxWithRepository(t, host, "/workspace-app")
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}
	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)

	cmd := WorkspaceCopyInCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       sourceRoot,
		SandboxID:   sandboxID,
	}
	if err := cmd.Run(&runtimeContext{
		CWD:           sourceRoot,
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
	if len(commands) != 0 {
		t.Fatalf("expected symlinked raw workspace destination to skip clean execution, got %d command(s)", len(commands))
	}
	if got, want := removedPath, "/workspace-app"; got != want {
		t.Fatalf("unexpected removed path: got %q want %q", got, want)
	}
	if removedRecursive {
		t.Fatal("expected symlinked raw workspace destination to be removed non-recursively")
	}
	if got, want := destination, "/workspace-app"; got != want {
		t.Fatalf("unexpected extract destination: got %q want %q", got, want)
	}
}

func TestRawWorkspaceCopyFollowsSymlinkedSourceRoot(t *testing.T) {
	realRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(realRoot, "app"), 0o755); err != nil {
		t.Fatalf("create app dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(realRoot, "app/main.txt"), []byte("payload\n"), 0o644); err != nil {
		t.Fatalf("write workspace file: %v", err)
	}

	linkRoot := filepath.Join(t.TempDir(), "workspace-link")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatalf("create workspace symlink: %v", err)
	}

	entries, err := rawWorkspacePlan(linkRoot, "/workspace")
	if err != nil {
		t.Fatalf("rawWorkspacePlan returned error: %v", err)
	}
	foundPlanEntry := false
	for _, entry := range entries {
		if entry.Action == "write" && entry.Path == "/workspace/app/main.txt" {
			foundPlanEntry = true
			break
		}
	}
	if !foundPlanEntry {
		t.Fatalf("expected symlinked workspace root plan to include app/main.txt, got %#v", entries)
	}

	var archive bytes.Buffer
	if err := writeRawWorkspaceTar(&archive, linkRoot); err != nil {
		t.Fatalf("writeRawWorkspaceTar returned error: %v", err)
	}
	tr := tar.NewReader(&archive)
	files := map[string]string{}
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read archived file: %v", err)
		}
		files[header.Name] = string(data)
	}
	if got, want := files["app/main.txt"], "payload\n"; got != want {
		t.Fatalf("unexpected archived file content: got %q want %q; files=%#v", got, want, files)
	}
}

func TestWorkspaceCopyInRejectsUnsafeRawDestinationRoot(t *testing.T) {
	sourceRoot := t.TempDir()
	err := copyRawWorkspaceToSandbox(context.Background(), &runtimeContext{}, nil, workspaceCopyOptions{
		CWD:         sourceRoot,
		SandboxID:   "cr_123",
		Destination: "/",
	})
	if err == nil {
		t.Fatal("expected unsafe raw workspace destination to be rejected")
	}
	if !strings.Contains(err.Error(), "unsafe for raw workspace copy-in") {
		t.Fatalf("expected unsafe destination error, got %v", err)
	}
}

func TestWorkspaceCopyInRejectsRawCopyWhenSandboxWorkspaceRootUnknown(t *testing.T) {
	sourceRoot := t.TempDir()
	adapter := &copyIntegrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createWorkspaceCopyTestSandbox(t, host, copyTestPolicy())

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
		t.Fatal("expected raw workspace copy-in to reject sandbox without recorded workspace root")
	}
	if !strings.Contains(err.Error(), "does not have a recorded workspace root") {
		t.Fatalf("expected recorded workspace root error, got %v", err)
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

func TestTopLevelCopyInRejectsNonGitWorkspaceWhenCreatingSandbox(t *testing.T) {
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
		"",
		"",
		"",
		0,
		false,
		repositoryOverrideFlags{},
		workspaceCopyFlags{CopyIn: true},
	)
	if err == nil {
		t.Fatal("expected non-Git top-level --copy-in to be rejected before sandbox creation")
	}
	if !strings.Contains(err.Error(), "non-Git workspaces") {
		t.Fatalf("expected non-Git copy error, got %v", err)
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

func runGitInDir(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
}
