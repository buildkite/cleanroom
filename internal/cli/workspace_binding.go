package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/buildkite/cleanroom/internal/paths"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

const (
	workspaceBindingVersion      = 1
	workspaceBindingTransportGit = "git"
)

type workspaceBinding struct {
	Version             int                    `json:"version"`
	SandboxID           string                 `json:"sandbox_id"`
	LocalRoot           string                 `json:"local_root"`
	SandboxWorkspace    string                 `json:"sandbox_workspace"`
	RepositoryRemoteURL string                 `json:"repository_remote_url"`
	RepositoryCommitSHA string                 `json:"repository_commit_sha"`
	RepositoryBranch    string                 `json:"repository_branch,omitempty"`
	Transport           string                 `json:"transport"`
	LastOperation       string                 `json:"last_operation"`
	LastOperationAt     string                 `json:"last_operation_at"`
	CopyInManifest      []workspaceBindingFile `json:"copy_in_manifest,omitempty"`
}

type workspaceBindingFile struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256,omitempty"`
	Mode    string `json:"mode,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
}

func readWorkspaceBinding(sandboxID string) (*workspaceBinding, error) {
	path, err := workspaceBindingPath(sandboxID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read workspace binding: %w", err)
	}
	var binding workspaceBinding
	if err := json.Unmarshal(data, &binding); err != nil {
		return nil, fmt.Errorf("decode workspace binding: %w", err)
	}
	return &binding, nil
}

func recordGitWorkspaceBinding(sandboxID string, repository *resolvedRepositoryCheckout, checkout *repositorycheckout.Checkout, files []repositorychangeset.File, operation string) error {
	if repository == nil || strings.TrimSpace(repository.RootDir) == "" {
		return nil
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return errors.New("workspace binding requires a sandbox id")
	}
	localRoot, err := normalizeWorkspaceLocalRoot(repository.RootDir)
	if err != nil {
		return err
	}
	remoteURL := strings.TrimSpace(repository.RemoteURL)
	commitSHA := strings.TrimSpace(repository.CommitSHA)
	branch := strings.TrimSpace(repository.Branch)
	workspaceRoot := strings.TrimSpace(repository.DestinationDir)
	if checkout != nil {
		if value := strings.TrimSpace(checkout.RemoteURL); value != "" {
			remoteURL = value
		}
		if value := strings.TrimSpace(checkout.CommitSHA); value != "" {
			commitSHA = value
		}
		if value := strings.TrimSpace(checkout.Branch); value != "" {
			branch = value
		}
		if value := strings.TrimSpace(checkout.DestinationDir); value != "" {
			workspaceRoot = value
		}
	}
	remoteURL, err = canonicalWorkspaceRemoteURL(remoteURL)
	if err != nil {
		return err
	}
	if commitSHA == "" {
		return errors.New("workspace binding requires a repository commit")
	}
	if workspaceRoot == "" {
		return errors.New("workspace binding requires a sandbox workspace root")
	}
	operation = strings.TrimSpace(operation)
	if operation == "" {
		operation = "copy-in"
	}
	manifest, err := workspaceBindingFiles(files)
	if err != nil {
		return err
	}
	return writeWorkspaceBinding(&workspaceBinding{
		Version:             workspaceBindingVersion,
		SandboxID:           sandboxID,
		LocalRoot:           localRoot,
		SandboxWorkspace:    workspaceRoot,
		RepositoryRemoteURL: remoteURL,
		RepositoryCommitSHA: commitSHA,
		RepositoryBranch:    branch,
		Transport:           workspaceBindingTransportGit,
		LastOperation:       operation,
		LastOperationAt:     time.Now().UTC().Format(time.RFC3339Nano),
		CopyInManifest:      manifest,
	})
}

func warnWorkspaceBindingError(ctx *runtimeContext, err error) {
	if err == nil {
		return
	}
	_ = writeExecutionWarning(ctx.stderr(), "workspace binding was not saved: "+err.Error())
}

func writeWorkspaceBinding(binding *workspaceBinding) error {
	if binding == nil {
		return errors.New("workspace binding is required")
	}
	path, err := workspaceBindingPath(binding.SandboxID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create workspace binding directory: %w", err)
	}
	data, err := json.MarshalIndent(binding, "", "  ")
	if err != nil {
		return fmt.Errorf("encode workspace binding: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write workspace binding: %w", err)
	}
	return nil
}

func workspaceBindingPath(sandboxID string) (string, error) {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return "", errors.New("workspace binding requires a sandbox id")
	}
	base, err := paths.StateBaseDir()
	if err != nil {
		return "", fmt.Errorf("resolve workspace binding state directory: %w", err)
	}
	return filepath.Join(base, "workspace-bindings", safeWorkspaceStateName(sandboxID)+".json"), nil
}

func validateWorkspaceBinding(binding *workspaceBinding, sandboxID string, checkout *repositorycheckout.Checkout) error {
	if binding == nil {
		return nil
	}
	if binding.Version != workspaceBindingVersion {
		return fmt.Errorf("workspace binding has unsupported version %d", binding.Version)
	}
	if got, want := strings.TrimSpace(binding.SandboxID), strings.TrimSpace(sandboxID); got != want {
		return fmt.Errorf("workspace binding sandbox %q does not match requested sandbox %q", got, want)
	}
	if binding.Transport != workspaceBindingTransportGit {
		return fmt.Errorf("workspace binding transport %q is not supported for Git copy-out", binding.Transport)
	}
	if checkout == nil {
		return errors.New("workspace binding requires a sandbox repository checkout")
	}
	boundRemote, err := canonicalWorkspaceRemoteURL(binding.RepositoryRemoteURL)
	if err != nil {
		return err
	}
	sandboxRemote, err := canonicalWorkspaceRemoteURL(checkout.RemoteURL)
	if err != nil {
		return err
	}
	if boundRemote != sandboxRemote {
		return fmt.Errorf("workspace binding repository remote %q does not match sandbox repository remote %q", boundRemote, sandboxRemote)
	}
	boundCommit := strings.TrimSpace(binding.RepositoryCommitSHA)
	sandboxCommit := strings.TrimSpace(checkout.CommitSHA)
	if boundCommit == "" || sandboxCommit == "" {
		return errors.New("workspace binding requires local and sandbox repository commits")
	}
	if !strings.EqualFold(boundCommit, sandboxCommit) {
		return fmt.Errorf("workspace binding repository commit %s does not match sandbox baseline %s", shortGitSHA(boundCommit), shortGitSHA(sandboxCommit))
	}
	boundWorkspace := strings.TrimSpace(binding.SandboxWorkspace)
	sandboxWorkspace := strings.TrimSpace(checkout.DestinationDir)
	if boundWorkspace == "" || sandboxWorkspace == "" {
		return errors.New("workspace binding requires a sandbox workspace root")
	}
	if boundWorkspace != sandboxWorkspace {
		return fmt.Errorf("workspace binding sandbox workspace %q does not match sandbox workspace %q", boundWorkspace, sandboxWorkspace)
	}
	return nil
}

func workspaceBindingFiles(files []repositorychangeset.File) ([]workspaceBindingFile, error) {
	if len(files) == 0 {
		return nil, nil
	}
	out := make([]workspaceBindingFile, 0, len(files))
	for _, file := range files {
		path, err := workspaceRelativePath(file.Path)
		if err != nil {
			return nil, err
		}
		mode := strings.TrimSpace(file.Mode)
		sha256 := strings.TrimSpace(file.SHA256)
		if file.Deleted {
			mode = ""
			sha256 = ""
		} else if sha256 == "" {
			return nil, fmt.Errorf("workspace binding copy-in manifest path %q is missing sha256", path)
		} else if mode == "" {
			return nil, fmt.Errorf("workspace binding copy-in manifest path %q is missing git mode", path)
		}
		out = append(out, workspaceBindingFile{
			Path:    path,
			SHA256:  sha256,
			Mode:    mode,
			Deleted: file.Deleted,
		})
	}
	return out, nil
}

func repositoryLocalChangesFiles(localChanges repositoryLocalChanges) []repositorychangeset.File {
	if len(localChanges.Files) > 0 {
		return append([]repositorychangeset.File(nil), localChanges.Files...)
	}
	changeset := localChanges.Changeset
	if changeset == nil {
		return nil
	}
	files := make([]repositorychangeset.File, 0, len(changeset.GetFiles()))
	for _, file := range changeset.GetFiles() {
		files = append(files, repositorychangeset.File{
			Path:    file.GetPath(),
			SHA256:  file.GetSha256(),
			Deleted: file.GetDeleted(),
		})
	}
	return files
}

func canonicalWorkspaceRemoteURL(remoteURL string) (string, error) {
	remoteURL = strings.TrimSpace(remoteURL)
	if remoteURL == "" {
		return "", errors.New("workspace binding requires a repository remote")
	}
	canonical, _, err := repositorycheckout.CanonicalizeRemoteURL(remoteURL)
	if err != nil {
		return "", err
	}
	return canonical, nil
}

func normalizeWorkspaceLocalRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", errors.New("workspace binding requires a local root")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve workspace binding local root: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = resolved
	}
	return filepath.Clean(absolute), nil
}

func sameWorkspaceLocalRoot(a, b string) bool {
	left, err := normalizeWorkspaceLocalRoot(a)
	if err != nil {
		return false
	}
	right, err := normalizeWorkspaceLocalRoot(b)
	if err != nil {
		return false
	}
	return left == right
}
