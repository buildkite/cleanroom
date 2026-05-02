package repositorychangeset

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

const FormatGitDiffV1 = "git-diff-v1"

type File struct {
	Path    string
	SHA256  string
	Deleted bool
}

type Changeset struct {
	Format        string
	BaseCommitSHA string
	Digest        string
	TreeDigest    string
	Patch         []byte
	Files         []File
}

func BuildFromWorkingTree(repoRoot string, checkout *repositorycheckout.Checkout) (*Changeset, error) {
	if checkout == nil {
		return nil, errors.New("repository changeset requires a repository checkout")
	}
	repoRoot = strings.TrimSpace(repoRoot)
	if repoRoot == "" {
		return nil, errors.New("repository changeset requires a repository root")
	}
	baseCommitSHA := strings.ToLower(strings.TrimSpace(checkout.CommitSHA))
	if baseCommitSHA == "" {
		return nil, errors.New("repository changeset requires a base commit SHA")
	}
	if err := ensureNoDirtySubmoduleWorktrees(repoRoot); err != nil {
		return nil, err
	}

	indexFile, err := os.CreateTemp("", "cleanroom-repository-changeset-index-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary git index: %w", err)
	}
	indexPath := indexFile.Name()
	if err := indexFile.Close(); err != nil {
		_ = os.Remove(indexPath)
		return nil, fmt.Errorf("close temporary git index %q: %w", indexPath, err)
	}
	defer os.Remove(indexPath)

	env := append(os.Environ(), "GIT_INDEX_FILE="+indexPath)
	if _, err := gitOutput(repoRoot, env, "read-tree", baseCommitSHA); err != nil {
		return nil, fmt.Errorf("initialize temporary git index: %w", err)
	}
	if _, err := gitOutput(repoRoot, env, "add", "-A", "--all", "."); err != nil {
		return nil, fmt.Errorf("stage working tree into temporary git index: %w", err)
	}

	patch, err := gitOutput(repoRoot, env, "diff", "--cached", "--binary", "--full-index", "--no-ext-diff", "--no-color", "--no-renames", baseCommitSHA)
	if err != nil {
		return nil, fmt.Errorf("build repository changeset patch: %w", err)
	}
	if len(bytes.TrimSpace(patch)) == 0 {
		return nil, nil
	}
	if gitlinkPaths, err := changedGitlinkPaths(repoRoot, env, baseCommitSHA); err != nil {
		return nil, err
	} else if len(gitlinkPaths) > 0 {
		return nil, fmt.Errorf("repository changeset cannot represent submodule gitlink change(s): %s; commit and push the submodule update, or apply it manually inside the sandbox", strings.Join(gitlinkPaths, ", "))
	}

	treeDigestBytes, err := gitOutput(repoRoot, env, "write-tree")
	if err != nil {
		return nil, fmt.Errorf("resolve post-apply tree digest: %w", err)
	}
	treeDigest := strings.TrimSpace(string(treeDigestBytes))
	if treeDigest == "" {
		return nil, errors.New("post-apply tree digest is empty")
	}

	files, err := changedFiles(repoRoot, env, baseCommitSHA)
	if err != nil {
		return nil, err
	}

	return &Changeset{
		Format:        FormatGitDiffV1,
		BaseCommitSHA: baseCommitSHA,
		Digest:        buildDigest(baseCommitSHA, treeDigest, patch, files),
		TreeDigest:    treeDigest,
		Patch:         append([]byte(nil), patch...),
		Files:         files,
	}, nil
}

func (c *Changeset) DigestPathsFromBase(repoRoot string, paths []string) ([]File, error) {
	if c == nil {
		return nil, errors.New("repository changeset is required")
	}
	repoRoot = strings.TrimSpace(repoRoot)
	if repoRoot == "" {
		return nil, errors.New("repository changeset requires a repository root")
	}
	baseCommitSHA := strings.ToLower(strings.TrimSpace(c.BaseCommitSHA))
	if baseCommitSHA == "" {
		return nil, errors.New("repository changeset base_commit_sha is required")
	}
	if len(c.Patch) == 0 {
		return nil, errors.New("repository changeset patch is required")
	}

	indexFile, err := os.CreateTemp("", "cleanroom-repository-changeset-index-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary git index: %w", err)
	}
	indexPath := indexFile.Name()
	if err := indexFile.Close(); err != nil {
		_ = os.Remove(indexPath)
		return nil, fmt.Errorf("close temporary git index %q: %w", indexPath, err)
	}
	defer os.Remove(indexPath)

	patchFile, err := os.CreateTemp("", "cleanroom-repository-changeset-patch-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary patch file: %w", err)
	}
	patchPath := patchFile.Name()
	if _, err := patchFile.Write(c.Patch); err != nil {
		patchFile.Close()
		_ = os.Remove(indexPath)
		_ = os.Remove(patchPath)
		return nil, fmt.Errorf("write temporary patch file %q: %w", patchPath, err)
	}
	if err := patchFile.Close(); err != nil {
		_ = os.Remove(indexPath)
		_ = os.Remove(patchPath)
		return nil, fmt.Errorf("close temporary patch file %q: %w", patchPath, err)
	}
	defer os.Remove(patchPath)

	env := append(os.Environ(), "GIT_INDEX_FILE="+indexPath)
	if _, err := gitOutput(repoRoot, env, "read-tree", baseCommitSHA); err != nil {
		return nil, fmt.Errorf("initialize temporary git index: %w", err)
	}
	if _, err := gitOutput(repoRoot, env, "apply", "--cached", "--binary", "--whitespace=nowarn", patchPath); err != nil {
		return nil, fmt.Errorf("apply repository changeset patch to temporary git index: %w", err)
	}

	expandedPaths, err := expandDigestPathsInIndex(repoRoot, env, baseCommitSHA, paths)
	if err != nil {
		return nil, err
	}
	files := make([]File, 0, len(expandedPaths))
	for _, normalizedPath := range expandedPaths {
		file := File{Path: normalizedPath}
		exists, err := pathExistsInIndex(repoRoot, env, normalizedPath)
		if err != nil {
			return nil, fmt.Errorf("check repository changeset path %q in temporary git index: %w", normalizedPath, err)
		}
		if !exists {
			file.Deleted = true
			files = append(files, file)
			continue
		}

		blob, err := gitOutput(repoRoot, env, "show", ":"+normalizedPath)
		if err != nil {
			return nil, fmt.Errorf("read repository changeset path %q from temporary git index: %w", normalizedPath, err)
		}
		file.SHA256 = sha256Digest(blob)
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files, nil
}

func expandDigestPathsInIndex(repoRoot string, env []string, baseCommitSHA string, paths []string) ([]string, error) {
	seen := make(map[string]struct{}, len(paths))
	var expanded []string
	var indexPaths []string
	for _, rawPath := range paths {
		normalizedPath := normalizePath(rawPath)
		if normalizedPath == "" {
			return nil, fmt.Errorf("repository changeset digest path %q is invalid", rawPath)
		}
		if !strings.ContainsAny(normalizedPath, "*?[") {
			if _, exists := seen[normalizedPath]; exists {
				continue
			}
			seen[normalizedPath] = struct{}{}
			expanded = append(expanded, normalizedPath)
			continue
		}
		if _, err := path.Match(normalizedPath, ""); err != nil {
			return nil, fmt.Errorf("repository changeset digest path glob %q is invalid: %w", normalizedPath, err)
		}
		if indexPaths == nil {
			var err error
			indexPaths, err = listIndexPathsIncludingDeleted(repoRoot, env, baseCommitSHA)
			if err != nil {
				return nil, err
			}
		}
		matches := 0
		for _, candidate := range indexPaths {
			matched, err := path.Match(normalizedPath, candidate)
			if err != nil {
				return nil, fmt.Errorf("repository changeset digest path glob %q is invalid: %w", normalizedPath, err)
			}
			if !matched {
				continue
			}
			matches++
			if _, exists := seen[candidate]; exists {
				continue
			}
			seen[candidate] = struct{}{}
			expanded = append(expanded, candidate)
		}
		if matches == 0 {
			return nil, fmt.Errorf("repository changeset digest path glob %q matched no files", normalizedPath)
		}
	}
	sort.Strings(expanded)
	return expanded, nil
}

func FromProto(proto *cleanroomv1.RepositoryChangeset) *Changeset {
	if proto == nil {
		return nil
	}
	out := &Changeset{
		Format:        strings.TrimSpace(proto.GetFormat()),
		BaseCommitSHA: strings.ToLower(strings.TrimSpace(proto.GetBaseCommitSha())),
		Digest:        strings.TrimSpace(proto.GetDigest()),
		TreeDigest:    strings.TrimSpace(proto.GetTreeDigest()),
		Patch:         append([]byte(nil), proto.GetPatch()...),
		Files:         make([]File, 0, len(proto.GetFiles())),
	}
	for _, file := range proto.GetFiles() {
		if file == nil {
			continue
		}
		out.Files = append(out.Files, File{
			Path:    normalizePath(file.GetPath()),
			SHA256:  strings.TrimSpace(file.GetSha256()),
			Deleted: file.GetDeleted(),
		})
	}
	return out
}

func (c *Changeset) ToProto() *cleanroomv1.RepositoryChangeset {
	if c == nil {
		return nil
	}
	files := make([]*cleanroomv1.RepositoryChangesetFile, 0, len(c.Files))
	for _, file := range c.Files {
		files = append(files, &cleanroomv1.RepositoryChangesetFile{
			Path:    file.Path,
			Sha256:  file.SHA256,
			Deleted: file.Deleted,
		})
	}
	return &cleanroomv1.RepositoryChangeset{
		Format:        c.Format,
		BaseCommitSha: c.BaseCommitSHA,
		Digest:        c.Digest,
		TreeDigest:    c.TreeDigest,
		Patch:         append([]byte(nil), c.Patch...),
		Files:         files,
	}
}

func (c *Changeset) ValidateForCheckout(checkout *repositorycheckout.Checkout) error {
	if c == nil {
		return nil
	}
	if checkout == nil {
		return errors.New("repository changeset requires a repository checkout")
	}
	if strings.TrimSpace(c.Format) != FormatGitDiffV1 {
		return fmt.Errorf("repository changeset format %q is unsupported", c.Format)
	}
	baseCommitSHA := strings.ToLower(strings.TrimSpace(c.BaseCommitSHA))
	if baseCommitSHA == "" {
		return errors.New("repository changeset base_commit_sha is required")
	}
	if baseCommitSHA != strings.ToLower(strings.TrimSpace(checkout.CommitSHA)) {
		return fmt.Errorf("repository changeset base_commit_sha %q does not match repository checkout commit %q", c.BaseCommitSHA, checkout.CommitSHA)
	}
	if strings.TrimSpace(c.Digest) == "" {
		return errors.New("repository changeset digest is required")
	}
	if strings.TrimSpace(c.TreeDigest) == "" {
		return errors.New("repository changeset tree_digest is required")
	}
	if len(c.Patch) == 0 {
		return errors.New("repository changeset patch is required")
	}
	for _, file := range c.Files {
		if file.Path == "" {
			return errors.New("repository changeset file path is required")
		}
		if file.Deleted {
			continue
		}
		if strings.TrimSpace(file.SHA256) == "" {
			return fmt.Errorf("repository changeset file %q is missing sha256", file.Path)
		}
	}
	return nil
}

func (c *Changeset) ValidateContent() error {
	if c == nil {
		return nil
	}
	if patchHasGitlinkChanges(c.Patch) {
		return errors.New("repository changeset cannot represent submodule gitlink changes")
	}
	files := make([]File, 0, len(c.Files))
	seen := make(map[string]struct{}, len(c.Files))
	for _, file := range c.Files {
		normalizedPath := normalizePath(file.Path)
		if normalizedPath == "" {
			return fmt.Errorf("repository changeset file %q has an invalid path", file.Path)
		}
		if _, exists := seen[normalizedPath]; exists {
			return fmt.Errorf("repository changeset file %q is duplicated", normalizedPath)
		}
		seen[normalizedPath] = struct{}{}

		normalizedFile := File{
			Path:    normalizedPath,
			SHA256:  strings.TrimSpace(file.SHA256),
			Deleted: file.Deleted,
		}
		if normalizedFile.Deleted {
			normalizedFile.SHA256 = ""
		}
		files = append(files, normalizedFile)
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})

	expectedDigest := buildDigest(c.BaseCommitSHA, c.TreeDigest, c.Patch, files)
	if strings.TrimSpace(c.Digest) != expectedDigest {
		return fmt.Errorf("repository changeset digest %q does not match content %q", c.Digest, expectedDigest)
	}
	return nil
}

func (c *Changeset) ChangedFileDigest(path string) (digest string, deleted, ok bool) {
	if c == nil {
		return "", false, false
	}
	normalizedPath := normalizePath(path)
	for _, file := range c.Files {
		if file.Path != normalizedPath {
			continue
		}
		return file.SHA256, file.Deleted, true
	}
	return "", false, false
}

func ApplyCommand(checkout *repositorycheckout.Checkout, changeset *Changeset) []string {
	return applyCommand(checkout, changeset, false)
}

func ApplyCommandResettingCheckout(checkout *repositorycheckout.Checkout, changeset *Changeset) []string {
	return applyCommand(checkout, changeset, true)
}

func ResetCommand(checkout *repositorycheckout.Checkout) []string {
	if checkout == nil {
		return nil
	}
	baseCommitSHA := strings.ToLower(strings.TrimSpace(checkout.CommitSHA))
	if baseCommitSHA == "" {
		return nil
	}
	script := []string{
		"set -eu",
		"dest=" + shellQuote(checkout.DestinationDir),
		"base_commit=" + shellQuote(baseCommitSHA),
		`if [ ! -d "$dest/.git" ]; then echo "repository destination is not a git checkout: $dest" >&2; exit 1; fi`,
		`git -C "$dest" reset --hard "$base_commit" >/dev/null`,
		`git -C "$dest" clean -ffd >/dev/null`,
	}
	return []string{"sh", "-lc", strings.Join(script, "\n")}
}

func WorktreeDiffCommand(checkout *repositorycheckout.Checkout) []string {
	return worktreeChangesCommand(checkout, `GIT_INDEX_FILE="$index_file" git -C "$dest" diff --cached --binary --full-index --no-ext-diff --no-color --no-renames "$base_commit"`)
}

func WorktreeNameStatusCommand(checkout *repositorycheckout.Checkout) []string {
	return worktreeChangesCommand(checkout, `GIT_INDEX_FILE="$index_file" git -C "$dest" diff --cached --name-status --no-renames -z "$base_commit"`)
}

func applyCommand(checkout *repositorycheckout.Checkout, changeset *Changeset, resetCheckout bool) []string {
	if checkout == nil || changeset == nil {
		return nil
	}
	script := []string{
		"set -eu",
		"dest=" + shellQuote(checkout.DestinationDir),
		"base_commit=" + shellQuote(changeset.BaseCommitSHA),
		"expected_tree=" + shellQuote(changeset.TreeDigest),
		`if [ ! -d "$dest/.git" ]; then echo "repository destination is not a git checkout: $dest" >&2; exit 1; fi`,
	}
	if resetCheckout {
		script = append(script,
			`git -C "$dest" reset --hard "$base_commit" >/dev/null`,
			`git -C "$dest" clean -ffd >/dev/null`,
		)
	}
	script = append(script,
		`patch_file="$(mktemp)"`,
		`index_file="$(mktemp)"`,
		`cleanup() { rm -f "$patch_file" "$index_file"; }`,
		`trap cleanup EXIT INT TERM`,
		`cat >"$patch_file"`,
		`git -C "$dest" apply --binary --whitespace=nowarn "$patch_file"`,
		`GIT_INDEX_FILE="$index_file" git -C "$dest" read-tree HEAD`,
		`GIT_INDEX_FILE="$index_file" git -C "$dest" add -A --all .`,
		`got_tree="$(GIT_INDEX_FILE="$index_file" git -C "$dest" write-tree)"`,
		`if [ "$got_tree" != "$expected_tree" ]; then echo "repository changeset tree mismatch: expected $expected_tree got $got_tree" >&2; exit 1; fi`,
	)
	return []string{"sh", "-lc", strings.Join(script, "\n")}
}

func worktreeChangesCommand(checkout *repositorycheckout.Checkout, emit string) []string {
	if checkout == nil {
		return nil
	}
	baseCommitSHA := strings.ToLower(strings.TrimSpace(checkout.CommitSHA))
	if baseCommitSHA == "" || strings.TrimSpace(checkout.DestinationDir) == "" || strings.TrimSpace(emit) == "" {
		return nil
	}
	script := []string{
		"set -eu",
		"dest=" + shellQuote(checkout.DestinationDir),
		"base_commit=" + shellQuote(baseCommitSHA),
		`if [ ! -d "$dest/.git" ]; then echo "repository destination is not a git checkout: $dest" >&2; exit 1; fi`,
		`status_file="$(mktemp)"`,
		`index_file="$(mktemp)"`,
		`raw_file="$(mktemp)"`,
		`cleanup() { rm -f "$status_file" "$index_file" "$raw_file"; }`,
		`trap cleanup EXIT INT TERM`,
		`git -C "$dest" status --porcelain=v2 --ignore-submodules=none >"$status_file"`,
		`dirty_submodule="$(awk '($1 == "1" || $1 == "2") && length($3) == 4 && substr($3, 1, 1) == "S" && (substr($3, 3, 1) != "." || substr($3, 4, 1) != ".") { print $NF; exit }' "$status_file")"`,
		`if [ -n "$dirty_submodule" ]; then echo "repository changeset cannot represent dirty submodule worktree $dirty_submodule; commit the submodule change or clean it first" >&2; exit 1; fi`,
		`GIT_INDEX_FILE="$index_file" git -C "$dest" read-tree "$base_commit"`,
		`GIT_INDEX_FILE="$index_file" git -C "$dest" add -A --all .`,
		`GIT_INDEX_FILE="$index_file" git -C "$dest" diff --cached --raw --no-renames "$base_commit" >"$raw_file"`,
		`gitlink_path="$(awk '($1 == ":160000" || $2 == "160000") { print $NF; exit }' "$raw_file")"`,
		`if [ -n "$gitlink_path" ]; then echo "repository changeset cannot represent submodule gitlink change(s): $gitlink_path; commit and push the submodule update, or apply it manually inside the sandbox" >&2; exit 1; fi`,
		emit,
	}
	return []string{"sh", "-lc", strings.Join(script, "\n")}
}

func changedFiles(repoRoot string, env []string, baseCommitSHA string) ([]File, error) {
	output, err := gitOutput(repoRoot, env, "diff", "--cached", "--name-status", "--no-renames", "-z", baseCommitSHA)
	if err != nil {
		return nil, fmt.Errorf("list repository changeset files: %w", err)
	}
	tokens := splitNullTerminated(output)
	files := make([]File, 0, len(tokens)/2)
	for i := 0; i < len(tokens); i += 2 {
		if i+1 >= len(tokens) {
			return nil, fmt.Errorf("parse repository changeset file status %q", tokens[i])
		}
		status := strings.TrimSpace(tokens[i])
		rawPath := tokens[i+1]
		normalizedPath := normalizePath(rawPath)
		if normalizedPath == "" {
			return nil, fmt.Errorf("parse repository changeset file path from %q", rawPath)
		}

		file := File{Path: normalizedPath}
		if strings.HasPrefix(status, "D") {
			file.Deleted = true
			files = append(files, file)
			continue
		}

		blob, err := gitOutput(repoRoot, env, "show", ":"+normalizedPath)
		if err != nil {
			return nil, fmt.Errorf("read repository changeset file %q from temporary git index: %w", normalizedPath, err)
		}
		file.SHA256 = sha256Digest(blob)
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files, nil
}

func changedGitlinkPaths(repoRoot string, env []string, baseCommitSHA string) ([]string, error) {
	output, err := gitOutput(repoRoot, env, "diff", "--cached", "--raw", "--no-renames", "-z", baseCommitSHA)
	if err != nil {
		return nil, fmt.Errorf("list repository changeset gitlinks: %w", err)
	}
	tokens := splitNullTerminated(output)
	paths := make([]string, 0)
	for i := 0; i < len(tokens); i += 2 {
		if i+1 >= len(tokens) {
			return nil, fmt.Errorf("parse repository changeset raw diff entry %q", tokens[i])
		}
		fields := strings.Fields(tokens[i])
		if len(fields) < 5 {
			return nil, fmt.Errorf("parse repository changeset raw diff metadata %q", tokens[i])
		}
		oldMode := strings.TrimPrefix(fields[0], ":")
		newMode := fields[1]
		if oldMode != "160000" && newMode != "160000" {
			continue
		}
		normalizedPath := normalizePath(tokens[i+1])
		if normalizedPath == "" {
			return nil, fmt.Errorf("parse repository changeset gitlink path from %q", tokens[i+1])
		}
		paths = append(paths, normalizedPath)
	}
	sort.Strings(paths)
	return paths, nil
}

func ensureNoDirtySubmoduleWorktrees(repoRoot string) error {
	output, err := gitOutput(repoRoot, os.Environ(), "status", "--porcelain=v2", "--ignore-submodules=none")
	if err != nil {
		return fmt.Errorf("inspect repository submodule state: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		submoduleState := fields[2]
		if len(submoduleState) != 4 || submoduleState[0] != 'S' {
			continue
		}
		if submoduleState[2] == '.' && submoduleState[3] == '.' {
			continue
		}
		path := fields[len(fields)-1]
		return fmt.Errorf("repository changeset cannot represent dirty submodule worktree %q; commit the submodule change or clean it first", path)
	}
	return nil
}

func patchHasGitlinkChanges(patch []byte) bool {
	for _, line := range strings.Split(string(patch), "\n") {
		line = strings.TrimSuffix(line, "\r")
		switch {
		case strings.HasPrefix(line, "index ") && strings.HasSuffix(line, " 160000"):
			return true
		case strings.HasPrefix(line, "new file mode 160000"):
			return true
		case strings.HasPrefix(line, "deleted file mode 160000"):
			return true
		case line == "old mode 160000":
			return true
		case line == "new mode 160000":
			return true
		}
	}
	return false
}

func pathExistsInIndex(repoRoot string, env []string, normalizedPath string) (bool, error) {
	output, err := gitOutput(repoRoot, env, "ls-files", "--stage", "--", normalizedPath)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(output)) != "", nil
}

func listIndexPaths(repoRoot string, env []string) ([]string, error) {
	output, err := gitOutput(repoRoot, env, "ls-files", "-z")
	if err != nil {
		return nil, fmt.Errorf("list repository changeset index paths: %w", err)
	}
	tokens := splitNullTerminated(output)
	paths := make([]string, 0, len(tokens))
	for _, token := range tokens {
		normalizedPath := normalizePath(token)
		if normalizedPath == "" {
			return nil, fmt.Errorf("parse repository changeset index path from %q", token)
		}
		paths = append(paths, normalizedPath)
	}
	sort.Strings(paths)
	return paths, nil
}

func listIndexPathsIncludingDeleted(repoRoot string, env []string, baseCommitSHA string) ([]string, error) {
	paths, err := listIndexPaths(repoRoot, env)
	if err != nil {
		return nil, err
	}
	deletedPaths, err := listDeletedIndexPaths(repoRoot, env, baseCommitSHA)
	if err != nil {
		return nil, err
	}
	if len(deletedPaths) == 0 {
		return paths, nil
	}
	seen := make(map[string]struct{}, len(paths)+len(deletedPaths))
	combined := make([]string, 0, len(paths)+len(deletedPaths))
	for _, candidate := range append(paths, deletedPaths...) {
		if _, exists := seen[candidate]; exists {
			continue
		}
		seen[candidate] = struct{}{}
		combined = append(combined, candidate)
	}
	sort.Strings(combined)
	return combined, nil
}

func listDeletedIndexPaths(repoRoot string, env []string, baseCommitSHA string) ([]string, error) {
	output, err := gitOutput(repoRoot, env, "diff", "--cached", "--name-only", "-z", "--diff-filter=D", strings.TrimSpace(baseCommitSHA), "--")
	if err != nil {
		return nil, fmt.Errorf("list deleted repository changeset index paths: %w", err)
	}
	tokens := splitNullTerminated(output)
	paths := make([]string, 0, len(tokens))
	for _, token := range tokens {
		normalizedPath := normalizePath(token)
		if normalizedPath == "" {
			return nil, fmt.Errorf("parse deleted repository changeset index path from %q", token)
		}
		paths = append(paths, normalizedPath)
	}
	sort.Strings(paths)
	return paths, nil
}

func buildDigest(baseCommitSHA, treeDigest string, patch []byte, files []File) string {
	sum := sha256.New()
	writeDigestComponent(sum, FormatGitDiffV1)
	writeDigestComponent(sum, strings.ToLower(strings.TrimSpace(baseCommitSHA)))
	writeDigestComponent(sum, strings.TrimSpace(treeDigest))
	writeDigestComponent(sum, sha256Digest(patch))
	for _, file := range files {
		writeDigestComponent(sum, file.Path)
		writeDigestComponent(sum, file.SHA256)
		if file.Deleted {
			writeDigestComponent(sum, "deleted")
		} else {
			writeDigestComponent(sum, "present")
		}
	}
	return "sha256:" + hex.EncodeToString(sum.Sum(nil))
}

func writeDigestComponent(sum interface{ Write([]byte) (int, error) }, value string) {
	_, _ = sum.Write([]byte(value))
	_, _ = sum.Write([]byte{0})
}

func sha256Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func normalizePath(value string) string {
	if value == "" {
		return ""
	}
	if strings.Contains(value, "\x00") {
		return ""
	}
	cleaned := path.Clean(value)
	if cleaned == "." || cleaned == "" || strings.HasPrefix(cleaned, "/") || strings.HasPrefix(cleaned, "../") {
		return ""
	}
	return cleaned
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

func gitOutput(dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
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

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
