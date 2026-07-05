package bake

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// StageWorkspace materializes the git-visible workspace file set into a
// temporary directory for copy-in: tracked files plus untracked files that
// are not ignored — exactly the set `git status` covers and the bake key's
// dirty decision sees. Ignored files (.env, node_modules, build output) and
// the .git directory (which can hold credentialed remote URLs) never enter
// the builder or its captured artifact. Exclusions use the same pathspec
// semantics as CollectGitFactsExcluding so the output spore is skipped when
// it lives inside the repository.
//
// The returned cleanup removes the staging directory.
func StageWorkspace(dir string, excludeRel []string) (string, func(), error) {
	args := []string{"ls-files", "-z", "--cached", "--others", "--exclude-standard"}
	if len(excludeRel) > 0 {
		args = append(args, "--", ".")
		for _, rel := range excludeRel {
			rel = strings.TrimSpace(rel)
			if rel == "" {
				continue
			}
			args = append(args, ":(exclude)"+rel, ":(exclude)"+rel+"/**")
		}
	}
	out, err := gitOutputRaw(dir, args...)
	if err != nil {
		return "", nil, fmt.Errorf("list workspace files: %w", err)
	}

	staged, err := os.MkdirTemp("", "cleanroom-stage-")
	if err != nil {
		return "", nil, fmt.Errorf("create staging directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(staged) }

	for _, rel := range strings.Split(strings.TrimRight(out, "\x00"), "\x00") {
		if rel == "" {
			continue
		}
		src := filepath.Join(dir, rel)
		info, err := os.Lstat(src)
		if err != nil {
			if os.IsNotExist(err) {
				// Tracked but deleted from the worktree; the dirty flag
				// records the deletion and the artifact must not resurrect it.
				continue
			}
			cleanup()
			return "", nil, fmt.Errorf("stat workspace file %s: %w", rel, err)
		}
		if info.IsDir() {
			// Submodule entry; its content is not part of this repository's
			// file set.
			continue
		}
		dst := filepath.Join(staged, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("stage workspace file %s: %w", rel, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(src)
			if err == nil {
				err = os.Symlink(target, dst)
			}
			if err != nil {
				cleanup()
				return "", nil, fmt.Errorf("stage workspace symlink %s: %w", rel, err)
			}
			continue
		}
		if err := copyFile(src, dst, info.Mode().Perm()); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("stage workspace file %s: %w", rel, err)
		}
	}
	return staged, cleanup, nil
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
