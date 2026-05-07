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
		rest := line[1:]
		fields := strings.Fields(rest)
		if len(fields) < 2 {
			return nil, fmt.Errorf("parse submodule status line %q", line)
		}
		smPath := fields[1]
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

func ListMirrorSubmodulesAtCommit(ctx context.Context, parentMirrorDir, commitSHA string, ensureMirror func(ctx context.Context, remoteURL, commitSHA string) (mirrorDir string, err error)) ([]MirrorSubmodule, error) {
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

		mirrorDir, err := ensureMirror(ctx, entry.URL, gitlinkSHA)
		if err != nil {
			return nil, fmt.Errorf("ensure mirror for submodule %q: %w", entry.Path, err)
		}

		result = append(result, MirrorSubmodule{
			Path:       entry.Path,
			RemoteURL:  entry.URL,
			GitlinkSHA: gitlinkSHA,
			MirrorDir:  mirrorDir,
		})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Path < result[j].Path
	})
	return result, nil
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
