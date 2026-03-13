package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/buildkite/cleanroom/internal/paths"
)

const defaultGitMirrorMaxAge = 30 * time.Second

// GitMirrorStore ensures a local bare mirror exists for a canonical upstream
// remote URL and returns its path.
type GitMirrorStore interface {
	EnsureMirror(ctx context.Context, remoteURL string) (string, error)
	EnsureMirrorContains(ctx context.Context, remoteURL, commitSHA string) error
}

type gitMirrorStore struct {
	baseDir     string
	maxAge      time.Duration
	credentials CredentialProvider

	group singleflight.Group

	mu        sync.RWMutex
	lastFetch map[string]time.Time
}

func NewGitMirrorStore(baseDir string, maxAge time.Duration, credentials CredentialProvider) *gitMirrorStore {
	return &gitMirrorStore{
		baseDir:     baseDir,
		maxAge:      maxAge,
		credentials: credentials,
		lastFetch:   make(map[string]time.Time),
	}
}

func NewDefaultGitMirrorStore(credentials CredentialProvider) (*gitMirrorStore, error) {
	baseDir, err := paths.StateBaseDir()
	if err != nil {
		return nil, err
	}
	return NewGitMirrorStore(filepath.Join(baseDir, "repos"), defaultGitMirrorMaxAge, credentials), nil
}

func (s *gitMirrorStore) EnsureMirror(ctx context.Context, remoteURL string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("mirror store is nil")
	}
	parsed, err := normalizeMirrorRemoteURL(remoteURL)
	if err != nil {
		return "", err
	}
	canonicalRemoteURL := parsed.String()
	mirrorDir := s.mirrorPath(canonicalRemoteURL)
	key := canonicalRemoteURL

	result, err, _ := s.group.Do(key, func() (any, error) {
		if _, statErr := os.Stat(filepath.Join(mirrorDir, "HEAD")); os.IsNotExist(statErr) {
			if err := s.cloneMirror(ctx, canonicalRemoteURL, mirrorDir); err != nil {
				return "", err
			}
			return mirrorDir, nil
		}
		if s.isStale(canonicalRemoteURL) {
			if err := s.fetchMirror(ctx, canonicalRemoteURL, mirrorDir); err != nil {
				return "", err
			}
		}
		return mirrorDir, nil
	})
	if err != nil {
		return "", err
	}
	return result.(string), nil
}

func (s *gitMirrorStore) EnsureMirrorContains(ctx context.Context, remoteURL, commitSHA string) error {
	if s == nil {
		return fmt.Errorf("mirror store is nil")
	}
	trimmedCommit := strings.TrimSpace(commitSHA)
	if trimmedCommit == "" {
		return fmt.Errorf("empty commit SHA")
	}
	parsed, err := normalizeMirrorRemoteURL(remoteURL)
	if err != nil {
		return err
	}
	canonicalRemoteURL := parsed.String()
	key := canonicalRemoteURL + "#" + trimmedCommit

	_, err, _ = s.group.Do(key, func() (any, error) {
		mirrorDir, err := s.EnsureMirror(ctx, canonicalRemoteURL)
		if err != nil {
			return nil, err
		}
		commitPresent, err := gitCommitExists(ctx, mirrorDir, trimmedCommit)
		if err != nil {
			return nil, err
		}
		if commitPresent {
			return nil, nil
		}
		if err := s.fetchMirror(ctx, canonicalRemoteURL, mirrorDir); err != nil {
			return nil, err
		}
		commitPresent, err = gitCommitExists(ctx, mirrorDir, trimmedCommit)
		if err != nil {
			return nil, err
		}
		if !commitPresent {
			return nil, fmt.Errorf("commit %s not found in remote %s", trimmedCommit, canonicalRemoteURL)
		}
		return nil, nil
	})
	return err
}

func normalizeMirrorRemoteURL(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("empty remote URL")
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("parse remote URL %q: %w", raw, err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		if strings.TrimSpace(parsed.Host) == "" {
			return nil, fmt.Errorf("remote URL %q missing host", raw)
		}
	case "file":
		if strings.TrimSpace(parsed.Path) == "" {
			return nil, fmt.Errorf("remote URL %q missing path", raw)
		}
	default:
		return nil, fmt.Errorf("remote URL %q must use https or file", raw)
	}
	parsed.User = nil
	return parsed, nil
}

func (s *gitMirrorStore) mirrorPath(remoteURL string) string {
	sum := sha256.Sum256([]byte(remoteURL))
	hash := hex.EncodeToString(sum[:])
	return filepath.Join(s.baseDir, hash[:2], hash+".git")
}

func (s *gitMirrorStore) isStale(remoteURL string) bool {
	if s.maxAge == 0 {
		return true
	}
	s.mu.RLock()
	last, ok := s.lastFetch[remoteURL]
	s.mu.RUnlock()
	if !ok {
		return true
	}
	return time.Since(last) > s.maxAge
}

func (s *gitMirrorStore) markFetched(remoteURL string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastFetch[remoteURL] = time.Now()
}

func (s *gitMirrorStore) cloneMirror(ctx context.Context, remoteURL, mirrorDir string) error {
	if err := os.MkdirAll(filepath.Dir(mirrorDir), 0o755); err != nil {
		return fmt.Errorf("create mirror directory: %w", err)
	}
	args := s.gitArgsWithAuth(ctx, remoteURL, "clone", "--mirror", remoteURL, mirrorDir)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.RemoveAll(mirrorDir)
		return fmt.Errorf("git clone --mirror %s: %s: %w", remoteURL, strings.TrimSpace(string(output)), err)
	}
	s.markFetched(remoteURL)
	return nil
}

func (s *gitMirrorStore) fetchMirror(ctx context.Context, remoteURL, mirrorDir string) error {
	setURL := exec.CommandContext(ctx, "git", "-C", mirrorDir, "remote", "set-url", "origin", remoteURL)
	setURL.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if output, err := setURL.CombinedOutput(); err != nil {
		return fmt.Errorf("git remote set-url origin %s: %s: %w", remoteURL, strings.TrimSpace(string(output)), err)
	}

	args := s.gitArgsWithAuth(ctx, remoteURL, "-C", mirrorDir, "fetch", "--prune", "origin")
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git fetch --prune origin: %s: %w", strings.TrimSpace(string(output)), err)
	}
	s.markFetched(remoteURL)
	return nil
}

func (s *gitMirrorStore) authConfigEntry(remoteURL string) (string, string) {
	if s == nil || s.credentials == nil {
		return "", ""
	}
	header, err := s.credentials.Resolve(context.Background(), remoteURL)
	if err != nil {
		return "", ""
	}
	header = strings.TrimSpace(header)
	if header == "" {
		return "", ""
	}
	return "http." + remoteURL + "/.extraHeader", "Authorization: " + header
}

func (s *gitMirrorStore) gitArgsWithAuth(ctx context.Context, remoteURL string, args ...string) []string {
	key := ""
	value := ""
	if s != nil && s.credentials != nil {
		header, err := s.credentials.Resolve(ctx, remoteURL)
		if err == nil && strings.TrimSpace(header) != "" {
			key = "http." + remoteURL + "/.extraHeader"
			value = "Authorization: " + strings.TrimSpace(header)
		}
	}

	if key == "" || value == "" {
		return append([]string(nil), args...)
	}

	out := make([]string, 0, len(args)+2)
	out = append(out, "-c", key+"="+value)
	out = append(out, args...)
	return out
}

func gitCommitExists(ctx context.Context, repoDir, commitSHA string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "cat-file", "-e", strings.TrimSpace(commitSHA)+"^{commit}")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 128 {
		return false, nil
	}
	return false, fmt.Errorf("git cat-file -e %s^{commit}: %s: %w", strings.TrimSpace(commitSHA), strings.TrimSpace(string(output)), err)
}
