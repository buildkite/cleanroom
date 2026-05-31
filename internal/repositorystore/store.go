package repositorystore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

var ErrFileNotFound = errors.New("repository file not found")

type FileNotFoundError struct {
	CommitSHA string
	Path      string
}

func NewFileNotFoundError(commitSHA, path string) error {
	return &FileNotFoundError{
		CommitSHA: strings.TrimSpace(commitSHA),
		Path:      strings.TrimSpace(path),
	}
}

func (e *FileNotFoundError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("repository file %s not found at %s", e.Path, e.CommitSHA)
}

func (e *FileNotFoundError) Is(target error) bool {
	return target == ErrFileNotFound
}

type FetchHints struct {
	Branches []string
	Refs     []string
}

type TransportHints struct {
	BundleListURL string
}

type RepositoryStore interface {
	EnsureCommit(ctx context.Context, remoteURL, commitSHA string, hints FetchHints) error
	ReadFileAtCommit(ctx context.Context, remoteURL, commitSHA, path string) ([]byte, error)
	Refresh(ctx context.Context, remoteURL string, hints FetchHints) error
	WithRepository(ctx context.Context, remoteURL, commitSHA string, hints FetchHints, fn func(repoDir string) error) error
	TransportHints(ctx context.Context, remoteURL, commitSHA string, hints FetchHints) (TransportHints, error)
	EnsureSubmoduleMirror(ctx context.Context, submoduleRemoteURL, gitlinkSHA string) (mirrorDir string, err error)
}

type mirrorSource interface {
	MirrorPath(remoteURL string) (string, error)
	EnsureMirror(ctx context.Context, remoteURL string) (string, error)
	RefreshMirror(ctx context.Context, remoteURL string) (string, error)
	EnsureMirrorContains(ctx context.Context, remoteURL, commitSHA string) error
}

type mirrorBackedRepositoryStore struct {
	mirrors mirrorSource
}

func NewMirrorBacked(mirrors mirrorSource) RepositoryStore {
	if mirrors == nil {
		return nil
	}
	return &mirrorBackedRepositoryStore{mirrors: mirrors}
}

func (s *mirrorBackedRepositoryStore) EnsureCommit(ctx context.Context, remoteURL, commitSHA string, _ FetchHints) error {
	if s == nil || s.mirrors == nil {
		return fmt.Errorf("repository store is nil")
	}
	return s.mirrors.EnsureMirrorContains(ctx, remoteURL, commitSHA)
}

func (s *mirrorBackedRepositoryStore) ReadFileAtCommit(ctx context.Context, remoteURL, commitSHA, path string) ([]byte, error) {
	var content []byte
	err := s.WithRepository(ctx, remoteURL, commitSHA, FetchHints{}, func(repoDir string) error {
		output, err := gitShowFileAtCommit(ctx, repoDir, commitSHA, path)
		if err != nil {
			return err
		}
		content = output
		return nil
	})
	if err != nil {
		return nil, err
	}
	return content, nil
}

func (s *mirrorBackedRepositoryStore) Refresh(ctx context.Context, remoteURL string, _ FetchHints) error {
	if s == nil || s.mirrors == nil {
		return fmt.Errorf("repository store is nil")
	}
	_, err := s.mirrors.RefreshMirror(ctx, remoteURL)
	return err
}

func (s *mirrorBackedRepositoryStore) WithRepository(ctx context.Context, remoteURL, commitSHA string, hints FetchHints, fn func(repoDir string) error) error {
	if s == nil || s.mirrors == nil {
		return fmt.Errorf("repository store is nil")
	}
	if fn == nil {
		return fmt.Errorf("repository callback is nil")
	}
	repoDir, err := s.repositoryPath(ctx, remoteURL)
	if err != nil {
		return err
	}
	if err := fn(repoDir); err == nil || strings.TrimSpace(commitSHA) == "" {
		return err
	}
	if err := s.EnsureCommit(ctx, remoteURL, commitSHA, hints); err != nil {
		return err
	}
	repoDir, err = s.repositoryPath(ctx, remoteURL)
	if err != nil {
		return err
	}
	return fn(repoDir)
}

func (s *mirrorBackedRepositoryStore) TransportHints(context.Context, string, string, FetchHints) (TransportHints, error) {
	return TransportHints{}, nil
}

func (s *mirrorBackedRepositoryStore) EnsureSubmoduleMirror(ctx context.Context, submoduleRemoteURL, gitlinkSHA string) (string, error) {
	if s == nil || s.mirrors == nil {
		return "", fmt.Errorf("repository store is nil")
	}
	canonicalRemoteURL, _, err := repositorycheckout.CanonicalizeRemoteURL(submoduleRemoteURL)
	if err != nil {
		return "", fmt.Errorf("validate submodule remote URL: %w", err)
	}
	if err := s.mirrors.EnsureMirrorContains(ctx, canonicalRemoteURL, gitlinkSHA); err != nil {
		return "", err
	}
	return s.mirrors.MirrorPath(canonicalRemoteURL)
}

func (s *mirrorBackedRepositoryStore) repositoryPath(ctx context.Context, remoteURL string) (string, error) {
	repoDir, err := s.mirrors.EnsureMirror(ctx, remoteURL)
	if err != nil {
		return "", err
	}
	if _, statErr := os.Stat(filepath.Join(repoDir, "HEAD")); statErr != nil {
		return "", statErr
	}
	return repoDir, nil
}

func gitShowFileAtCommit(ctx context.Context, repoDir, commitSHA, path string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "show", strings.TrimSpace(commitSHA)+":"+path)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		if isGitShowFileMissingError(message, path) {
			return nil, NewFileNotFoundError(commitSHA, path)
		}
		return nil, fmt.Errorf("git show %s:%s: %s", strings.TrimSpace(commitSHA), path, message)
	}
	return output, nil
}

func isGitShowFileMissingError(message, path string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	path = strings.ToLower(strings.TrimSpace(path))
	if path == "" {
		return false
	}
	quotedPath := "'" + path + "'"
	return strings.Contains(message, "path "+quotedPath+" does not exist") ||
		strings.Contains(message, "path "+quotedPath+" exists on disk, but not in") ||
		strings.Contains(message, "pathspec "+quotedPath)
}
