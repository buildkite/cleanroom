package submodule

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

type WorktreeSubmodule struct {
	Path        string
	WorktreeDir string
}

type MirrorSubmodule struct {
	Path       string
	RemoteURL  string
	GitlinkSHA string
	MirrorDir  string
}

func ListWorktreeSubmodules(repoRoot string) ([]WorktreeSubmodule, error) {
	out, err := gitOutput(repoRoot, "submodule", "status", "--recursive")
	if err != nil {
		return nil, fmt.Errorf("list submodules: %w", err)
	}

	seen := make(map[string]struct{})
	var result []WorktreeSubmodule

	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		prefix := line[0]
		smPath, err := parseSubmoduleStatusPath(line)
		if err != nil {
			return nil, err
		}
		switch prefix {
		case '-':
			return nil, fmt.Errorf("submodule %q is not initialised; run \"git submodule update --init\"", smPath)
		case 'U':
			return nil, fmt.Errorf("submodule %q has unresolved merge conflicts", smPath)
		}
		if _, exists := seen[smPath]; exists {
			continue
		}
		seen[smPath] = struct{}{}
		result = append(result, WorktreeSubmodule{
			Path:        smPath,
			WorktreeDir: filepath.Join(repoRoot, smPath),
		})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Path < result[j].Path
	})
	return result, nil
}

// parseSubmoduleStatusPath extracts the path from a single line of
// `git submodule status` output. The format is `<char><sha> <path>[ (describe)]`,
// where path may contain spaces, so splitting on whitespace is unsafe.
func parseSubmoduleStatusPath(line string) (string, error) {
	if len(line) < 42 {
		return "", fmt.Errorf("parse submodule status line %q", line)
	}
	rest := line[1:]
	sp := strings.IndexByte(rest, ' ')
	if sp < 0 {
		return "", fmt.Errorf("parse submodule status line %q", line)
	}
	smPath := rest[sp+1:]
	if strings.HasSuffix(smPath, ")") {
		if open := strings.LastIndex(smPath, " ("); open >= 0 {
			smPath = smPath[:open]
		}
	}
	if smPath == "" {
		return "", fmt.Errorf("parse submodule status line %q", line)
	}
	return smPath, nil
}

func ListWorktreeSubmoduleFiles(sm WorktreeSubmodule) ([]string, error) {
	out, err := gitOutput(sm.WorktreeDir, "ls-files", "-z")
	if err != nil {
		return nil, fmt.Errorf("list submodule files for %q: %w", sm.Path, err)
	}

	var result []string
	for _, f := range splitNullTerminated(out) {
		result = append(result, sm.Path+"/"+f)
	}
	sort.Strings(result)
	return result, nil
}

func ListMirrorSubmodulesAtCommit(ctx context.Context, parentMirrorDir, parentRemoteURL, commitSHA string, ensureMirror func(ctx context.Context, remoteURL, commitSHA string) (mirrorDir string, err error)) ([]MirrorSubmodule, error) {
	raw, err := gitOutputContext(ctx, parentMirrorDir, "show", commitSHA+":.gitmodules")
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "does not exist") || strings.Contains(msg, "exists on disk, but not") {
			return nil, nil
		}
		return nil, fmt.Errorf("read .gitmodules at %s: %w", commitSHA, err)
	}

	entries, err := ParseGitmodules(raw)
	if err != nil {
		return nil, fmt.Errorf("parse .gitmodules at %s: %w", commitSHA, err)
	}

	var result []MirrorSubmodule
	for _, entry := range entries {
		treeOut, err := gitOutputContext(ctx, parentMirrorDir, "ls-tree", "-z", commitSHA, "--", ":(literal)"+entry.Path)
		if err != nil {
			return nil, fmt.Errorf("ls-tree for submodule %q at %s: %w", entry.Path, commitSHA, err)
		}

		treeOut = bytes.TrimRight(treeOut, "\x00\n")
		if len(bytes.TrimSpace(treeOut)) == 0 {
			continue
		}

		gitlinkSHA, err := parseGitlinkSHA(treeOut, entry.Path)
		if err != nil {
			return nil, err
		}

		resolvedURL, err := ResolveMirrorSubmoduleURL(parentRemoteURL, entry.URL)
		if err != nil {
			return nil, fmt.Errorf("validate mirror submodule URL for %q: %w", entry.Path, err)
		}

		mirrorDir, err := ensureMirror(ctx, resolvedURL, gitlinkSHA)
		if err != nil {
			return nil, fmt.Errorf("ensure mirror for submodule %q: %w", entry.Path, err)
		}

		result = append(result, MirrorSubmodule{
			Path:       entry.Path,
			RemoteURL:  resolvedURL,
			GitlinkSHA: gitlinkSHA,
			MirrorDir:  mirrorDir,
		})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Path < result[j].Path
	})
	return result, nil
}

// ListMirrorSubmodulesAtIndex enumerates submodules from a temporary git
// index. It reads .gitmodules from the index, resolves each entry's gitlink
// SHA via `git ls-files --stage`, and ensures every submodule has a mirror
// available. Returns an empty slice (with no error) when the index has no
// .gitmodules.
func ListMirrorSubmodulesAtIndex(ctx context.Context, repoRoot string, env []string, parentRemoteURL string, ensureMirror func(ctx context.Context, remoteURL, commitSHA string) (mirrorDir string, err error)) ([]MirrorSubmodule, error) {
	raw, err := gitOutputWithEnv(repoRoot, env, "show", ":.gitmodules")
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "does not exist") || strings.Contains(msg, "exists on disk, but not") || strings.Contains(msg, "Path '.gitmodules' does not exist") {
			return nil, nil
		}
		return nil, fmt.Errorf("read .gitmodules from index: %w", err)
	}

	entries, err := ParseGitmodules(raw)
	if err != nil {
		return nil, fmt.Errorf("parse .gitmodules from index: %w", err)
	}

	var result []MirrorSubmodule
	for _, entry := range entries {
		stageOut, err := gitOutputWithEnv(repoRoot, env, "ls-files", "--stage", "-z", "--", ":(literal)"+entry.Path)
		if err != nil {
			return nil, fmt.Errorf("ls-files for submodule %q in index: %w", entry.Path, err)
		}
		stageOut = bytes.TrimRight(stageOut, "\x00")
		if len(bytes.TrimSpace(stageOut)) == 0 {
			continue
		}

		gitlinkSHA, err := parseStageGitlinkSHA(stageOut, entry.Path)
		if err != nil {
			return nil, err
		}

		resolvedURL, err := ResolveMirrorSubmoduleURL(parentRemoteURL, entry.URL)
		if err != nil {
			return nil, fmt.Errorf("validate mirror submodule URL for %q: %w", entry.Path, err)
		}

		mirrorDir, err := ensureMirror(ctx, resolvedURL, gitlinkSHA)
		if err != nil {
			return nil, fmt.Errorf("ensure mirror for submodule %q: %w", entry.Path, err)
		}

		result = append(result, MirrorSubmodule{
			Path:       entry.Path,
			RemoteURL:  resolvedURL,
			GitlinkSHA: gitlinkSHA,
			MirrorDir:  mirrorDir,
		})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Path < result[j].Path
	})
	return result, nil
}

func parseStageGitlinkSHA(stageOut []byte, path string) (string, error) {
	for _, raw := range bytes.Split(stageOut, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		meta, _, ok := bytes.Cut(raw, []byte{'\t'})
		if !ok {
			continue
		}
		fields := strings.Fields(string(meta))
		if len(fields) < 3 {
			continue
		}
		if fields[0] != "160000" {
			return "", fmt.Errorf("submodule %q has unexpected mode %q (expected 160000)", path, fields[0])
		}
		return fields[1], nil
	}
	return "", fmt.Errorf("submodule %q not found in index", path)
}

func parseGitlinkSHA(treeOut []byte, path string) (string, error) {
	line := strings.TrimSpace(string(bytes.TrimRight(treeOut, "\x00")))
	meta, _, ok := strings.Cut(line, "\t")
	if !ok {
		return "", fmt.Errorf("parse ls-tree entry for %q: unexpected format %q", path, line)
	}
	fields := strings.Fields(meta)
	if len(fields) < 3 {
		return "", fmt.Errorf("parse ls-tree entry for %q: unexpected format %q", path, line)
	}
	mode := fields[0]
	if mode != "160000" {
		return "", fmt.Errorf("submodule %q has unexpected mode %q (expected 160000)", path, mode)
	}
	return fields[2], nil
}

func ListMirrorSubmoduleFilesAtSHA(ctx context.Context, sm MirrorSubmodule) ([]string, error) {
	out, err := gitOutputContext(ctx, sm.MirrorDir, "ls-tree", "-r", "--name-only", "-z", sm.GitlinkSHA)
	if err != nil {
		return nil, fmt.Errorf("list mirror submodule files for %q at %s: %w", sm.Path, sm.GitlinkSHA, err)
	}

	var result []string
	for _, f := range splitNullTerminated(out) {
		result = append(result, sm.Path+"/"+f)
	}
	sort.Strings(result)
	return result, nil
}

func FindSubmoduleForPath(path string, worktreeSubs []WorktreeSubmodule) (WorktreeSubmodule, bool) {
	var best WorktreeSubmodule
	found := false
	for _, sm := range worktreeSubs {
		if strings.HasPrefix(path, sm.Path+"/") {
			if !found || len(sm.Path) > len(best.Path) {
				best = sm
				found = true
			}
		}
	}
	return best, found
}

func FindMirrorSubmoduleForPath(path string, mirrorSubs []MirrorSubmodule) (MirrorSubmodule, bool) {
	var best MirrorSubmodule
	found := false
	for _, sm := range mirrorSubs {
		if strings.HasPrefix(path, sm.Path+"/") {
			if !found || len(sm.Path) > len(best.Path) {
				best = sm
				found = true
			}
		}
	}
	return best, found
}

// ResolveSubmoduleURL resolves a submodule URL from .gitmodules against the
// parent repository's remote URL, following git's resolve_relative_url rules.
// Absolute URLs are returned as-is; only URLs beginning with `./` or `../`
// are resolved relative to the parent.
func ResolveSubmoduleURL(parentRemoteURL, submoduleURL string) (string, error) {
	if !strings.HasPrefix(submoduleURL, "./") && !strings.HasPrefix(submoduleURL, "../") {
		return submoduleURL, nil
	}
	if parentRemoteURL == "" {
		return "", fmt.Errorf("relative submodule URL %q requires a parent remote URL", submoduleURL)
	}

	base := parentRemoteURL
	scheme := ""
	if i := strings.Index(base, "://"); i >= 0 {
		scheme = base[:i+3]
		base = base[i+3:]
	}

	sep := byte('/')
	for {
		switch {
		case strings.HasPrefix(submoduleURL, "./"):
			submoduleURL = submoduleURL[2:]
		case strings.HasPrefix(submoduleURL, "../"):
			submoduleURL = submoduleURL[3:]
			slash := strings.LastIndexAny(base, "/:")
			if slash < 0 {
				return "", fmt.Errorf("cannot resolve relative submodule URL %q against parent %q", submoduleURL, parentRemoteURL)
			}
			sep = base[slash]
			base = base[:slash]
		default:
			return scheme + base + string(sep) + submoduleURL, nil
		}
	}
}

func ResolveMirrorSubmoduleURL(parentRemoteURL, submoduleURL string) (string, error) {
	resolved, err := ResolveSubmoduleURL(parentRemoteURL, submoduleURL)
	if err != nil {
		return "", err
	}
	_, parentHost, err := repositorycheckout.CanonicalizeRemoteURL(parentRemoteURL)
	if err != nil {
		return "", fmt.Errorf("canonicalize parent remote: %w", err)
	}
	canonicalSubmoduleURL, submoduleHost, err := repositorycheckout.CanonicalizeRemoteURL(resolved)
	if err != nil {
		return "", fmt.Errorf("canonicalize submodule remote: %w", err)
	}
	if submoduleHost != parentHost {
		return "", fmt.Errorf("submodule remote host %q does not match parent repository host %q", submoduleHost, parentHost)
	}
	return canonicalSubmoduleURL, nil
}

func splitNullTerminated(data []byte) []string {
	parts := bytes.Split(data, []byte{0})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		out = append(out, string(part))
	}
	return out
}

func gitOutput(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
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

func gitOutputWithEnv(dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	} else {
		cmd.Env = os.Environ()
	}
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

func gitOutputContext(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
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
