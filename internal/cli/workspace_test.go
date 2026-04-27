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
	"strings"
	"sync"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

func TestWorkspaceCopyAppliesGitChangeset(t *testing.T) {
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
	sandboxID := createWorkspaceCopyTestSandbox(t, host, workspaceCopyTestPolicy())

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
	cmd := WorkspaceCopyCommand{
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
		t.Fatalf("WorkspaceCopyCommand.Run returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(commands) < 2 {
		t.Fatalf("expected repository bootstrap and changeset apply commands, got %d", len(commands))
	}
	applyCommand := strings.Join(commands[len(commands)-1], " ")
	if !strings.Contains(applyCommand, `git -C "$dest" reset --hard "$base_commit"`) {
		t.Fatalf("expected workspace copy to reset the checkout before applying, got %q", applyCommand)
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

func TestWorkspaceCopyUsesRawArchiveForNonGitWorkspace(t *testing.T) {
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

	var (
		removedPath string
		recursive   bool
		destination string
		files       = map[string]string{}
	)
	adapter := &copyIntegrationAdapter{
		removeFn: func(_ context.Context, _ string, path string, rec bool) error {
			removedPath = path
			recursive = rec
			return nil
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
	sandboxID := createWorkspaceCopyTestSandbox(t, host, copyTestPolicy())
	stdout, _ := makeStdoutCapture(t)
	stderr, _ := makeStdoutCapture(t)

	cmd := WorkspaceCopyCommand{
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
		t.Fatalf("WorkspaceCopyCommand.Run returned error: %v", err)
	}

	if got, want := removedPath, "/workspace"; got != want {
		t.Fatalf("unexpected removed workspace root: got %q want %q", got, want)
	}
	if !recursive {
		t.Fatal("expected workspace root removal to be recursive")
	}
	if got, want := destination, "/workspace"; got != want {
		t.Fatalf("unexpected extract destination: got %q want %q", got, want)
	}
	if got, want := files["app/main.txt"], "payload\n"; got != want {
		t.Fatalf("unexpected archived file content: got %q want %q", got, want)
	}
	if _, ok := files[".git/config"]; ok {
		t.Fatalf("expected .git directory to be skipped, got files %#v", files)
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
