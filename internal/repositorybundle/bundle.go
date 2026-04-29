package repositorybundle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

const (
	FormatGitBundleV1 = "git-bundle-v1"
	MaxBytes          = 64 * 1024 * 1024
)

type Bundle struct {
	Format          string
	TargetCommitSHA string
	Digest          string
	Payload         []byte
}

// BuildFromRepository bundles local-only commits ending at HEAD.
// The checkout commit must match HEAD because the bundle target is HEAD.
func BuildFromRepository(repoRoot, remoteName string, checkout *repositorycheckout.Checkout) (*Bundle, error) {
	if checkout == nil {
		return nil, errors.New("repository commit bundle requires a repository checkout")
	}
	repoRoot = strings.TrimSpace(repoRoot)
	if repoRoot == "" {
		return nil, errors.New("repository commit bundle requires a repository root")
	}
	remoteName = strings.TrimSpace(remoteName)
	if remoteName == "" {
		return nil, errors.New("repository commit bundle requires a repository remote")
	}
	if strings.ContainsAny(remoteName, "\x00 \t\r\n") {
		return nil, fmt.Errorf("repository remote %q is not supported for local commit bundling", remoteName)
	}

	targetCommitSHA := strings.ToLower(strings.TrimSpace(checkout.CommitSHA))
	head, err := gitOutput(repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("resolve repository HEAD for commit bundle: %w", err)
	}
	if strings.ToLower(strings.TrimSpace(string(head))) != targetCommitSHA {
		return nil, fmt.Errorf("repository commit bundle target %q does not match HEAD %q", targetCommitSHA, strings.TrimSpace(string(head)))
	}

	countOutput, err := gitOutput(repoRoot, "rev-list", "--count", "HEAD", "--not", "--remotes="+remoteName)
	if err != nil {
		return nil, fmt.Errorf("inspect local-only repository commits: %w", err)
	}
	localOnlyCount, err := strconv.Atoi(strings.TrimSpace(string(countOutput)))
	if err != nil {
		return nil, fmt.Errorf("parse local-only repository commit count %q: %w", strings.TrimSpace(string(countOutput)), err)
	}
	if localOnlyCount == 0 {
		return nil, nil
	}

	tmpDir, err := os.MkdirTemp("", "cleanroom-repository-bundle-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary repository bundle directory: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	bundlePath := filepath.Join(tmpDir, "commits.bundle")
	if _, err := gitOutput(repoRoot, "bundle", "create", bundlePath, "HEAD", "--not", "--remotes="+remoteName); err != nil {
		if isEmptyBundleError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("create repository commit bundle: %w", err)
	}

	info, err := os.Stat(bundlePath)
	if err != nil {
		return nil, fmt.Errorf("stat repository commit bundle: %w", err)
	}
	if info.Size() == 0 {
		return nil, errors.New("repository commit bundle is empty")
	}
	if info.Size() > MaxBytes {
		return nil, fmt.Errorf("repository commit bundle is %d bytes, exceeding the %d byte limit", info.Size(), MaxBytes)
	}
	payload, err := os.ReadFile(bundlePath)
	if err != nil {
		return nil, fmt.Errorf("read repository commit bundle: %w", err)
	}

	bundle := &Bundle{
		Format:          FormatGitBundleV1,
		TargetCommitSHA: targetCommitSHA,
		Digest:          sha256Digest(payload),
		Payload:         payload,
	}
	if err := bundle.ValidateContent(); err != nil {
		return nil, err
	}
	return bundle, nil
}

func FromProto(proto *cleanroomv1.RepositoryCommitBundle) *Bundle {
	if proto == nil {
		return nil
	}
	return &Bundle{
		Format:          strings.TrimSpace(proto.GetFormat()),
		TargetCommitSHA: strings.ToLower(strings.TrimSpace(proto.GetTargetCommitSha())),
		Digest:          strings.TrimSpace(proto.GetDigest()),
		Payload:         append([]byte(nil), proto.GetBundle()...),
	}
}

func (b *Bundle) ToProto() *cleanroomv1.RepositoryCommitBundle {
	if b == nil {
		return nil
	}
	return &cleanroomv1.RepositoryCommitBundle{
		Format:          b.Format,
		TargetCommitSha: b.TargetCommitSHA,
		Digest:          b.Digest,
		Bundle:          append([]byte(nil), b.Payload...),
	}
}

func (b *Bundle) ValidateForCheckout(checkout *repositorycheckout.Checkout) error {
	if b == nil {
		return nil
	}
	if checkout == nil {
		return errors.New("repository commit bundle requires a repository checkout")
	}
	if strings.TrimSpace(b.Format) != FormatGitBundleV1 {
		return fmt.Errorf("repository commit bundle format %q is unsupported", b.Format)
	}
	targetCommitSHA := strings.ToLower(strings.TrimSpace(b.TargetCommitSHA))
	if err := validateCommitSHA(targetCommitSHA); err != nil {
		return fmt.Errorf("repository commit bundle target_commit_sha: %w", err)
	}
	if targetCommitSHA != strings.ToLower(strings.TrimSpace(checkout.CommitSHA)) {
		return fmt.Errorf("repository commit bundle target_commit_sha %q does not match repository checkout commit %q", b.TargetCommitSHA, checkout.CommitSHA)
	}
	return nil
}

func (b *Bundle) ValidateContent() error {
	if b == nil {
		return nil
	}
	if len(b.Payload) == 0 {
		return errors.New("repository commit bundle payload is required")
	}
	if len(b.Payload) > MaxBytes {
		return fmt.Errorf("repository commit bundle is %d bytes, exceeding the %d byte limit", len(b.Payload), MaxBytes)
	}
	if strings.TrimSpace(b.Digest) == "" {
		return errors.New("repository commit bundle digest is required")
	}
	expectedDigest := sha256Digest(b.Payload)
	if strings.TrimSpace(b.Digest) != expectedDigest {
		return fmt.Errorf("repository commit bundle digest %q does not match content %q", b.Digest, expectedDigest)
	}
	prerequisites, err := b.PrerequisiteCommits()
	if err != nil {
		return err
	}
	if len(prerequisites) == 0 {
		return errors.New("repository commit bundle has no remote prerequisites; push the base commits to the remote before bundling; full-history bundles are not supported yet")
	}
	return nil
}

func (b *Bundle) PrerequisiteCommits() ([]string, error) {
	if b == nil {
		return nil, nil
	}
	return parseBundlePrerequisites(b.Payload)
}

func (b *Bundle) VerifyAgainstRepository(ctx context.Context, repoDir string) error {
	if b == nil {
		return nil
	}
	if err := b.ValidateContent(); err != nil {
		return err
	}
	repoDir = strings.TrimSpace(repoDir)
	if repoDir == "" {
		return errors.New("repository path is required to verify commit bundle")
	}
	bundlePath, cleanup, err := writeTempBundle(b.Payload)
	if err != nil {
		return err
	}
	defer cleanup()

	if _, err := gitOutputContext(ctx, repoDir, "bundle", "verify", bundlePath); err != nil {
		return fmt.Errorf("verify repository commit bundle against mirror: %w", err)
	}
	heads, err := gitOutputContext(ctx, repoDir, "bundle", "list-heads", bundlePath, "HEAD")
	if err != nil {
		return fmt.Errorf("list repository commit bundle heads: %w", err)
	}
	if !bundleHeadsContainTarget(heads, b.TargetCommitSHA) {
		return fmt.Errorf("repository commit bundle does not advertise target commit %q as HEAD", b.TargetCommitSHA)
	}
	return nil
}

func (b *Bundle) WithRepository(ctx context.Context, repoDir string, fn func(repoDir string) error) error {
	if b == nil {
		return nil
	}
	if fn == nil {
		return errors.New("repository commit bundle callback is nil")
	}
	if err := b.ValidateContent(); err != nil {
		return err
	}
	repoDir = strings.TrimSpace(repoDir)
	if repoDir == "" {
		return errors.New("repository path is required to read commit bundle")
	}

	objectDir, err := gitOutputContext(ctx, repoDir, "rev-parse", "--git-path", "objects")
	if err != nil {
		return fmt.Errorf("resolve repository object directory: %w", err)
	}
	objectDirPath := strings.TrimSpace(string(objectDir))
	if objectDirPath == "" {
		return errors.New("repository object directory is empty")
	}
	if !filepath.IsAbs(objectDirPath) {
		objectDirPath = filepath.Join(repoDir, objectDirPath)
	}

	tmpDir, err := os.MkdirTemp("", "cleanroom-repository-bundle-view-*")
	if err != nil {
		return fmt.Errorf("create temporary repository commit bundle view: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	viewDir := filepath.Join(tmpDir, "repo.git")
	if _, err := gitOutputContext(ctx, tmpDir, "init", "--bare", viewDir); err != nil {
		return fmt.Errorf("initialize temporary repository commit bundle view: %w", err)
	}
	alternatesPath := filepath.Join(viewDir, "objects", "info", "alternates")
	if err := os.MkdirAll(filepath.Dir(alternatesPath), 0o755); err != nil {
		return fmt.Errorf("prepare temporary repository alternates: %w", err)
	}
	if err := os.WriteFile(alternatesPath, []byte(objectDirPath+"\n"), 0o644); err != nil {
		return fmt.Errorf("write temporary repository alternates: %w", err)
	}

	bundlePath, cleanup, err := writeTempBundle(b.Payload)
	if err != nil {
		return err
	}
	defer cleanup()
	if _, err := gitOutputContext(ctx, viewDir, "fetch", bundlePath, "+HEAD:refs/cleanroom/target"); err != nil {
		return fmt.Errorf("fetch repository commit bundle into temporary view: %w", err)
	}
	if _, err := gitOutputContext(ctx, viewDir, "cat-file", "-e", strings.TrimSpace(b.TargetCommitSHA)+"^{commit}"); err != nil {
		return fmt.Errorf("verify repository commit bundle target in temporary view: %w", err)
	}
	return fn(viewDir)
}

func parseBundlePrerequisites(payload []byte) ([]string, error) {
	header, _, _ := bytes.Cut(payload, []byte("\n\n"))
	lines := bytes.Split(header, []byte("\n"))
	out := make([]string, 0)
	seen := map[string]struct{}{}
	for _, rawLine := range lines {
		line := strings.TrimSuffix(string(rawLine), "\r")
		if strings.TrimSpace(line) == "" {
			break
		}
		if !strings.HasPrefix(line, "-") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "-"))
		if len(fields) == 0 {
			return nil, fmt.Errorf("parse repository commit bundle prerequisite %q", line)
		}
		commitSHA := strings.ToLower(strings.TrimSpace(fields[0]))
		if err := validateCommitSHA(commitSHA); err != nil {
			return nil, fmt.Errorf("parse repository commit bundle prerequisite %q: %w", line, err)
		}
		if _, ok := seen[commitSHA]; ok {
			continue
		}
		seen[commitSHA] = struct{}{}
		out = append(out, commitSHA)
	}
	return out, nil
}

func bundleHeadsContainTarget(output []byte, targetCommitSHA string) bool {
	targetCommitSHA = strings.ToLower(strings.TrimSpace(targetCommitSHA))
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if strings.EqualFold(fields[0], targetCommitSHA) && fields[1] == "HEAD" {
			return true
		}
	}
	return false
}

func validateCommitSHA(value string) error {
	if value == "" {
		return errors.New("commit SHA is required")
	}
	if len(value) != 40 {
		return fmt.Errorf("%q must be a full 40-character hexadecimal commit SHA", value)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("%q must be a full 40-character hexadecimal commit SHA", value)
	}
	return nil
}

func writeTempBundle(payload []byte) (string, func(), error) {
	file, err := os.CreateTemp("", "cleanroom-repository-bundle-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temporary repository commit bundle: %w", err)
	}
	path := file.Name()
	cleanup := func() {
		_ = os.Remove(path)
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		cleanup()
		return "", nil, fmt.Errorf("write temporary repository commit bundle %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("close temporary repository commit bundle %q: %w", path, err)
	}
	return path, cleanup, nil
}

func isEmptyBundleError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "refusing to create empty bundle")
}

func gitOutput(dir string, args ...string) ([]byte, error) {
	return gitOutputContext(context.Background(), dir, args...)
}

func gitOutputContext(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(msg)
	}
	return out, nil
}

func sha256Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
