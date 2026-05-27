package repositorystore

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

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
	WithRepository(ctx context.Context, remoteURL, commitSHA string, hints FetchHints, fn func(repoDir string) error) error
	TransportHints(ctx context.Context, remoteURL, commitSHA string, hints FetchHints) (TransportHints, error)
	EnsureSubmoduleMirror(ctx context.Context, submoduleRemoteURL, gitlinkSHA string) (mirrorDir string, err error)
}

type mirrorSource interface {
	MirrorPath(remoteURL string) (string, error)
	EnsureMirror(ctx context.Context, remoteURL string) (string, error)
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
	repoDir, err := s.mirrors.MirrorPath(remoteURL)
	if err == nil {
		return repoDir, nil
	}
	return s.mirrors.EnsureMirror(ctx, remoteURL)
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
		return nil, fmt.Errorf("git show %s:%s: %s", strings.TrimSpace(commitSHA), path, message)
	}
	return output, nil
}
