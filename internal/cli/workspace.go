package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	posixpath "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/buildkite/cleanroom/internal/controlclient"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

type WorkspaceCommand struct {
	CopyIn  WorkspaceCopyInCommand  `name:"copy-in" cmd:"" help:"Copy local workspace changes into a sandbox"`
	CopyOut WorkspaceCopyOutCommand `name:"copy-out" cmd:"" help:"Preview or copy sandbox workspace changes back locally"`
	Diff    WorkspaceDiffCommand    `cmd:"" help:"Show sandbox workspace changes"`
}

type WorkspaceCopyInCommand struct {
	clientFlags
	Chdir     string `short:"c" help:"Change to this local directory before planning the workspace copy-in"`
	DryRun    bool   `name:"dry-run" help:"Show the workspace paths that would be copied in without modifying the sandbox"`
	SandboxID string `arg:"" required:"" help:"Sandbox ID to copy into"`
}

type WorkspaceCopyOutCommand struct {
	clientFlags
	Chdir     string `short:"c" help:"Change to this local directory before planning the workspace copy-out"`
	DryRun    bool   `name:"dry-run" help:"Show the local workspace paths that would be copied out without modifying local files"`
	Force     bool   `help:"Overwrite conflicting local workspace paths with sandbox changes"`
	SandboxID string `arg:"" required:"" help:"Sandbox ID to copy out from"`
}

type WorkspaceDiffCommand struct {
	clientFlags
	SandboxID string `arg:"" required:"" help:"Sandbox ID to diff"`
}

type workspaceCopyOptions struct {
	CWD           string
	SandboxID     string
	DryRun        bool
	Repository    *resolvedRepositoryCheckout
	Binding       *workspaceBinding
	Destination   string
	ForceGitReset bool
	ForceCopyOut  bool
	ForceOutHint  string
	LaunchSeconds int64
	PlanOutput    io.Writer
}

type workspacePlanEntry struct {
	Action string
	Path   string
}

type workspaceCopyOutConflict struct {
	Path   string
	Reason string
}

func (c *WorkspaceCopyInCommand) Run(ctx *runtimeContext) error {
	cwd, err := resolveCWD(ctx.CWD, c.Chdir)
	if err != nil {
		return err
	}
	client, err := c.connect(ctx)
	if err != nil {
		return err
	}
	repository, err := resolveWorkspaceCopyRepositoryCheckout(cwd, ctx.Loader)
	if err != nil {
		return err
	}
	return copyWorkspaceToSandbox(context.Background(), ctx, client, workspaceCopyOptions{
		CWD:           cwd,
		SandboxID:     c.SandboxID,
		DryRun:        c.DryRun,
		Repository:    repository,
		ForceGitReset: true,
	})
}

func (c *WorkspaceCopyOutCommand) Run(ctx *runtimeContext) error {
	cwd, err := resolveCWD(ctx.CWD, c.Chdir)
	if err != nil {
		return err
	}
	client, err := c.connect(ctx)
	if err != nil {
		return err
	}
	opts, err := resolveWorkspaceCopyOutOptions(ctx, cwd, c.Chdir, c.SandboxID, c.DryRun, 0)
	if err != nil {
		return err
	}
	opts.ForceCopyOut = c.Force
	return copyWorkspaceOut(context.Background(), ctx, client, opts)
}

func resolveWorkspaceCopyOutOptions(ctx *runtimeContext, cwd, chdir, sandboxID string, dryRun bool, launchSeconds int64) (workspaceCopyOptions, error) {
	binding, err := readWorkspaceBinding(sandboxID)
	if err != nil {
		return workspaceCopyOptions{}, err
	}
	repositoryRoot := cwd
	if binding != nil && binding.Transport == workspaceBindingTransportGit && strings.TrimSpace(binding.LocalRoot) != "" {
		if strings.TrimSpace(chdir) != "" {
			chdirRepository, err := resolveWorkspaceCopyRepositoryCheckout(cwd, ctx.Loader)
			if err != nil {
				return workspaceCopyOptions{}, err
			}
			if chdirRepository == nil || !sameWorkspaceLocalRoot(chdirRepository.RootDir, binding.LocalRoot) {
				return workspaceCopyOptions{}, fmt.Errorf("workspace copy-out sandbox %q is bound to local root %q, but --chdir resolved to %q", sandboxID, binding.LocalRoot, cwd)
			}
		}
		repositoryRoot = binding.LocalRoot
	}
	repository, err := resolveWorkspaceCopyRepositoryCheckout(repositoryRoot, ctx.Loader)
	if err != nil {
		return workspaceCopyOptions{}, err
	}
	localRoot := cwd
	if repository != nil && strings.TrimSpace(repository.RootDir) != "" {
		localRoot = repository.RootDir
	}
	return workspaceCopyOptions{
		CWD:           localRoot,
		SandboxID:     sandboxID,
		DryRun:        dryRun,
		Repository:    repository,
		Binding:       binding,
		ForceOutHint:  workspaceCopyOutForceHint(sandboxID),
		LaunchSeconds: launchSeconds,
	}, nil
}

func (c *WorkspaceDiffCommand) Run(ctx *runtimeContext) error {
	client, err := c.connect(ctx)
	if err != nil {
		return err
	}
	return diffWorkspaceInSandbox(context.Background(), ctx, client, workspaceCopyOptions{
		SandboxID: c.SandboxID,
	})
}

func copyWorkspaceToSandbox(callCtx context.Context, ctx *runtimeContext, client *controlclient.Client, opts workspaceCopyOptions) error {
	if strings.TrimSpace(opts.SandboxID) == "" {
		return errors.New("missing sandbox id")
	}
	if strings.TrimSpace(opts.CWD) == "" {
		return errors.New("missing local workspace root")
	}
	if opts.Repository != nil {
		return copyGitWorkspaceToSandbox(callCtx, ctx, client, opts)
	}
	return errors.New("workspace copy-in requires a local Git repository checkout")
}

func copyGitWorkspaceToSandbox(callCtx context.Context, ctx *runtimeContext, client *controlclient.Client, opts workspaceCopyOptions) error {
	repository := opts.Repository
	if strings.TrimSpace(repository.RootDir) == "" {
		return errors.New("workspace copy-in requires a local repository checkout")
	}
	effectiveRepository, checkout, err := resolveGitWorkspaceCheckout(callCtx, client, opts)
	if err != nil {
		return err
	}
	changeset, err := repositorychangeset.BuildFromWorkingTree(repository.RootDir, checkout)
	if err != nil {
		return fmt.Errorf("package local workspace changes: %w", err)
	}
	if changeset == nil {
		if opts.DryRun {
			if !opts.ForceGitReset {
				return nil
			}
			return printWorkspacePlan(runtimeStdout(ctx), gitWorkspacePlan(checkout.DestinationDir, nil, true))
		}
		if opts.ForceGitReset {
			if err := runWorkspaceExecution(callCtx, ctx, client, opts.SandboxID, effectiveRepository, repositorychangeset.ResetCommand(checkout), nil, opts.LaunchSeconds); err != nil {
				return err
			}
		}
		warnWorkspaceBindingError(ctx, recordGitWorkspaceBinding(opts.SandboxID, effectiveRepository, checkout, nil, "copy-in"))
		return nil
	}
	if opts.DryRun {
		return printWorkspacePlan(runtimeStdout(ctx), gitWorkspacePlan(checkout.DestinationDir, changeset.Files, opts.ForceGitReset))
	}

	command := repositorychangeset.ApplyCommand(checkout, changeset)
	if opts.ForceGitReset {
		command = repositorychangeset.ApplyCommandResettingCheckout(checkout, changeset)
	}
	if err := runWorkspaceExecution(callCtx, ctx, client, opts.SandboxID, effectiveRepository, command, bytes.NewReader(changeset.Patch), opts.LaunchSeconds); err != nil {
		return err
	}
	warnWorkspaceBindingError(ctx, recordGitWorkspaceBinding(opts.SandboxID, effectiveRepository, checkout, changeset.Files, "copy-in"))
	return nil
}

func copyWorkspaceOut(callCtx context.Context, ctx *runtimeContext, client *controlclient.Client, opts workspaceCopyOptions) error {
	checkout, err := validateWorkspaceCopyOutInputs(callCtx, client, opts)
	if err != nil {
		return err
	}
	if opts.DryRun {
		var files []repositorychangeset.File
		if opts.ForceCopyOut || useGitWorkspaceCopyOutApplyBase(opts.Binding, opts.Repository, checkout) {
			var patch []byte
			files, patch, err = captureGitWorkspaceCopyOutPayload(callCtx, ctx, client, opts, checkout)
			if err != nil {
				return err
			}
			files, err = addGitWorkspaceCopyOutManifestReverts(opts.Repository.RootDir, checkout.CommitSHA, opts.Binding, files)
			if err != nil {
				return err
			}
			if len(files) > 0 {
				var obstaclePaths []string
				var cleanupApply func()
				patchPath, cleanup, err := writeTemporaryWorkspaceCopyOutPatch(patch)
				if err != nil {
					return err
				}
				defer cleanup()
				if err := ensureGitWorkspaceCopyOutSafe(opts.Repository, checkout, opts.Binding, files, opts.ForceCopyOut, opts.ForceOutHint); err != nil {
					return err
				}
				if opts.ForceCopyOut {
					obstaclePaths, err = gitWorkspaceCopyOutForceObstaclePathsForFiles(opts.Repository.RootDir, files)
					if err != nil {
						return err
					}
				}
				files, _, cleanupApply, err = prepareGitWorkspaceCopyOutApplyPatchWithOmittedPaths(opts.Repository, checkout, files, patchPath, obstaclePaths)
				if err != nil {
					return err
				}
				defer cleanupApply()
				files, err = gitWorkspaceCopyOutForcePlanFiles(files, obstaclePaths)
				if err != nil {
					return err
				}
			}
		} else {
			command := repositorychangeset.WorktreeNameStatusCommand(checkout)
			if len(command) == 0 {
				return errors.New("sandbox repository checkout is missing the information needed to plan workspace copy-out")
			}
			output, err := runWorkspaceExecutionCapture(callCtx, ctx, client, opts.SandboxID, command, opts.LaunchSeconds)
			if err != nil {
				return err
			}
			files, err = parseGitNameStatusWorkspaceFiles(output)
			if err != nil {
				return err
			}
		}
		entries, err := gitWorkspaceCopyOutPlanFiles(opts.CWD, files)
		if err != nil {
			return err
		}
		return printWorkspacePlan(workspacePlanOutput(ctx, opts), entries)
	}

	files, patch, err := captureGitWorkspaceCopyOutPayload(callCtx, ctx, client, opts, checkout)
	if err != nil {
		return err
	}
	files, err = addGitWorkspaceCopyOutManifestReverts(opts.Repository.RootDir, checkout.CommitSHA, opts.Binding, files)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}
	patchPath, cleanupPatch, err := writeTemporaryWorkspaceCopyOutPatch(patch)
	if err != nil {
		return err
	}
	defer cleanupPatch()
	if err := ensureGitWorkspaceCopyOutSafe(opts.Repository, checkout, opts.Binding, files, opts.ForceCopyOut, opts.ForceOutHint); err != nil {
		return err
	}
	applyPath := patchPath
	cleanupApply := func() {}
	var forceQuarantine *workspaceCopyOutForceQuarantine
	if opts.ForceCopyOut {
		forceQuarantine, err = quarantineGitWorkspaceCopyOutForceObstacles(opts.Repository.RootDir, files)
		if err != nil {
			return err
		}
	}
	if opts.ForceCopyOut || useGitWorkspaceCopyOutApplyBase(opts.Binding, opts.Repository, checkout) {
		var omittedPaths []string
		if forceQuarantine != nil {
			omittedPaths, err = forceQuarantine.OmittedPaths(opts.Repository.RootDir)
			if err != nil {
				err = errors.Join(err, forceQuarantine.Restore())
				return err
			}
		}
		files, applyPath, cleanupApply, err = prepareGitWorkspaceCopyOutApplyPatchWithOmittedPaths(opts.Repository, checkout, files, patchPath, omittedPaths)
		if err != nil {
			if forceQuarantine != nil {
				err = errors.Join(err, forceQuarantine.Restore())
			}
			return err
		}
		defer cleanupApply()
		if len(files) == 0 && forceQuarantine == nil {
			return nil
		}
	}
	var obstaclePaths []string
	if forceQuarantine != nil {
		obstaclePaths = forceQuarantine.Paths()
	}
	planFiles, err := gitWorkspaceCopyOutForcePlanFiles(files, obstaclePaths)
	if err != nil {
		if forceQuarantine != nil {
			err = errors.Join(err, forceQuarantine.Restore())
		}
		return err
	}
	entries, err := gitWorkspaceCopyOutPlanFiles(opts.CWD, planFiles)
	if err != nil {
		if forceQuarantine != nil {
			err = errors.Join(err, forceQuarantine.Restore())
		}
		return err
	}
	restoreForceIndex := func() error { return nil }
	cleanupForceIndex := func() {}
	if opts.ForceCopyOut {
		restoreForceIndex, cleanupForceIndex, err = snapshotGitWorkspaceCopyOutForceIndex(opts.Repository.RootDir, files, obstaclePaths)
		if err != nil {
			if forceQuarantine != nil {
				err = errors.Join(err, forceQuarantine.Restore())
			}
			return err
		}
		defer cleanupForceIndex()
		if err := resetGitWorkspaceCopyOutForceIndex(opts.Repository.RootDir, files, obstaclePaths); err != nil {
			err = errors.Join(err, restoreForceIndex())
			if forceQuarantine != nil {
				err = errors.Join(err, forceQuarantine.Restore())
			}
			return err
		}
	}
	if len(files) > 0 {
		if err := applyGitWorkspaceCopyOutPatch(opts.Repository.RootDir, applyPath); err != nil {
			if opts.ForceCopyOut {
				err = errors.Join(err, restoreForceIndex())
			}
			if forceQuarantine != nil {
				err = errors.Join(err, forceQuarantine.Restore())
			}
			return err
		}
	}
	if forceQuarantine != nil {
		forceQuarantine.Cleanup()
	}
	return printWorkspacePlan(workspacePlanOutput(ctx, opts), entries)
}

func addGitWorkspaceCopyOutManifestReverts(localRoot, baseCommit string, binding *workspaceBinding, files []repositorychangeset.File) ([]repositorychangeset.File, error) {
	manifest, err := workspaceCopyInManifestMap(binding)
	if err != nil {
		return nil, err
	}
	if len(manifest) == 0 {
		return files, nil
	}
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		path, err := workspaceRelativePath(file.Path)
		if err != nil {
			return nil, err
		}
		seen[path] = struct{}{}
	}
	paths := make([]string, 0, len(manifest))
	for path := range manifest {
		if _, ok := seen[path]; ok {
			continue
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return files, nil
	}
	out := append([]repositorychangeset.File(nil), files...)
	for _, path := range paths {
		baseFile, err := gitWorkspaceCommitFile(localRoot, baseCommit, path)
		if err != nil {
			return nil, err
		}
		out = append(out, repositorychangeset.File{
			Path:    path,
			SHA256:  baseFile.SHA256,
			Mode:    baseFile.Mode,
			Deleted: baseFile.Deleted,
		})
	}
	return out, nil
}

func gitWorkspaceCopyOutForcePlanFiles(files []repositorychangeset.File, obstaclePaths []string) ([]repositorychangeset.File, error) {
	if len(obstaclePaths) == 0 {
		return files, nil
	}
	seen := make(map[string]struct{}, len(files))
	planFiles := append([]repositorychangeset.File(nil), files...)
	for _, file := range files {
		path, err := workspaceRelativePath(file.Path)
		if err != nil {
			return nil, err
		}
		seen[path] = struct{}{}
	}
	for _, path := range obstaclePaths {
		path, err := workspaceRelativePath(path)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		planFiles = append(planFiles, repositorychangeset.File{
			Path:    path,
			Deleted: true,
		})
	}
	return planFiles, nil
}

func workspacePlanOutput(ctx *runtimeContext, opts workspaceCopyOptions) io.Writer {
	if opts.PlanOutput != nil {
		return opts.PlanOutput
	}
	return runtimeStdout(ctx)
}

func validateWorkspaceCopyOutInputs(callCtx context.Context, client *controlclient.Client, opts workspaceCopyOptions) (*repositorycheckout.Checkout, error) {
	if strings.TrimSpace(opts.SandboxID) == "" {
		return nil, errors.New("missing sandbox id")
	}
	if strings.TrimSpace(opts.CWD) == "" {
		return nil, errors.New("missing local workspace root")
	}
	checkout, err := sandboxRepositoryCheckout(callCtx, client, opts.SandboxID)
	if err != nil {
		return nil, err
	}
	if err := validateWorkspaceBinding(opts.Binding, opts.SandboxID, checkout); err != nil {
		return nil, err
	}
	if err := validateWorkspaceCopyOutLocalRepository(opts.Repository, checkout); err != nil {
		return nil, err
	}
	return checkout, nil
}

func previewWorkspaceCopyOut(callCtx context.Context, ctx *runtimeContext, client *controlclient.Client, opts workspaceCopyOptions) error {
	opts.DryRun = true
	return copyWorkspaceOut(callCtx, ctx, client, opts)
}

func diffWorkspaceInSandbox(callCtx context.Context, ctx *runtimeContext, client *controlclient.Client, opts workspaceCopyOptions) error {
	if strings.TrimSpace(opts.SandboxID) == "" {
		return errors.New("missing sandbox id")
	}
	checkout, err := sandboxRepositoryCheckout(callCtx, client, opts.SandboxID)
	if err != nil {
		return err
	}
	command := repositorychangeset.WorktreeDiffCommand(checkout)
	if len(command) == 0 {
		return errors.New("sandbox repository checkout is missing the information needed to diff workspace changes")
	}
	return runWorkspaceExecution(callCtx, ctx, client, opts.SandboxID, nil, command, nil, opts.LaunchSeconds)
}

func sandboxRepositoryCheckout(callCtx context.Context, client *controlclient.Client, sandboxID string) (*repositorycheckout.Checkout, error) {
	repository, err := resolveSandboxRepositoryCheckout(callCtx, client, sandboxID)
	if err != nil {
		return nil, err
	}
	checkout := repositorycheckout.FromProto(repository)
	if checkout == nil {
		return nil, fmt.Errorf("sandbox %q does not have a recorded repository checkout", sandboxID)
	}
	return checkout, nil
}

func validateWorkspaceCopyOutLocalRepository(local *resolvedRepositoryCheckout, sandbox *repositorycheckout.Checkout) error {
	if local == nil || strings.TrimSpace(local.RootDir) == "" {
		return errors.New("workspace copy-out requires a local Git repository checkout matching the sandbox repository")
	}
	localRemote := strings.TrimSpace(local.RemoteURL)
	if localRemote == "" {
		return errors.New("workspace copy-out requires a local repository remote")
	}
	if sandbox == nil || strings.TrimSpace(sandbox.RemoteURL) == "" {
		return errors.New("sandbox repository checkout is missing the remote needed to validate workspace copy-out")
	}
	sandboxRemote, _, err := repositorycheckout.CanonicalizeRemoteURL(sandbox.RemoteURL)
	if err != nil {
		return fmt.Errorf("validate sandbox repository remote for workspace copy-out: %w", err)
	}
	if localRemote != sandboxRemote {
		return fmt.Errorf("workspace copy-out local repository remote %q does not match sandbox repository remote %q", localRemote, sandboxRemote)
	}
	return nil
}

func ensureGitWorkspaceCopyOutSafe(local *resolvedRepositoryCheckout, checkout *repositorycheckout.Checkout, binding *workspaceBinding, files []repositorychangeset.File, force bool, forceHint string) error {
	if local == nil || strings.TrimSpace(local.RootDir) == "" {
		return errors.New("workspace copy-out requires a local Git repository checkout matching the sandbox repository")
	}
	localCommit := strings.TrimSpace(local.CommitSHA)
	sandboxCommit := ""
	if checkout != nil {
		sandboxCommit = strings.TrimSpace(checkout.CommitSHA)
	}
	if localCommit == "" || sandboxCommit == "" {
		return errors.New("workspace copy-out requires local and sandbox repository commits")
	}
	if binding == nil && !strings.EqualFold(localCommit, sandboxCommit) {
		if !force {
			return fmt.Errorf("workspace copy-out requires local checkout HEAD %s to match sandbox baseline %s; %s", shortGitSHA(localCommit), shortGitSHA(sandboxCommit), workspaceCopyOutForceSuggestion(forceHint))
		}
	}
	if force {
		return nil
	}
	if binding != nil {
		return ensureGitWorkspaceCopyOutSafeWithBinding(local.RootDir, sandboxCommit, binding, files, forceHint)
	}
	var conflicts []workspaceCopyOutConflict
	for _, file := range files {
		conflict, err := gitWorkspaceCopyOutPathConflict(local.RootDir, sandboxCommit, file.Path)
		if err != nil {
			return err
		}
		if conflict != nil {
			conflicts = append(conflicts, *conflict)
		}
	}
	return workspaceCopyOutConflictError(conflicts, forceHint)
}

func ensureGitWorkspaceCopyOutSafeWithBinding(localRoot, baseCommit string, binding *workspaceBinding, files []repositorychangeset.File, forceHint string) error {
	manifest, err := workspaceCopyInManifestMap(binding)
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(files))
	for _, file := range files {
		path, err := workspaceRelativePath(file.Path)
		if err != nil {
			return err
		}
		paths = append(paths, path)
	}
	current, err := gitWorkspaceCurrentFiles(localRoot, paths)
	if err != nil {
		return err
	}
	staged, err := gitWorkspaceStagedPaths(localRoot, paths)
	if err != nil {
		return err
	}
	var conflicts []workspaceCopyOutConflict
	for _, path := range paths {
		expected, ok := manifest[path]
		if !ok {
			expected, err = gitWorkspaceCommitFile(localRoot, baseCommit, path)
			if err != nil {
				return err
			}
		}
		conflict, err := gitWorkspaceFileConflict(localRoot, path, current[path], expected)
		if err != nil {
			return err
		}
		if conflict != nil {
			conflicts = append(conflicts, *conflict)
		}
		if staged[path] {
			index, err := gitWorkspaceIndexFile(localRoot, nil, path)
			if err != nil {
				return err
			}
			conflict, err := gitWorkspaceFileConflict(localRoot, path, index, expected)
			if err != nil {
				return err
			}
			if conflict != nil {
				conflicts = append(conflicts, *conflict)
			}
		}
	}
	return workspaceCopyOutConflictError(conflicts, forceHint)
}

func workspaceCopyInManifestMap(binding *workspaceBinding) (map[string]workspaceBindingFile, error) {
	manifest := make(map[string]workspaceBindingFile)
	if binding == nil {
		return manifest, nil
	}
	for _, file := range binding.CopyInManifest {
		path, err := workspaceRelativePath(file.Path)
		if err != nil {
			return nil, err
		}
		if _, exists := manifest[path]; exists {
			return nil, fmt.Errorf("workspace binding copy-in manifest path %q is duplicated", path)
		}
		entry := workspaceBindingFile{
			Path:    path,
			SHA256:  strings.TrimSpace(file.SHA256),
			Mode:    strings.TrimSpace(file.Mode),
			Deleted: file.Deleted,
		}
		if entry.Deleted {
			entry.SHA256 = ""
			entry.Mode = ""
		} else if entry.SHA256 == "" {
			return nil, fmt.Errorf("workspace binding copy-in manifest path %q is missing sha256", path)
		} else if entry.Mode == "" {
			return nil, fmt.Errorf("workspace binding copy-in manifest path %q is missing git mode", path)
		}
		manifest[path] = entry
	}
	return manifest, nil
}

func gitWorkspaceFileConflict(localRoot, rel string, current, expected workspaceBindingFile) (*workspaceCopyOutConflict, error) {
	if expected.Deleted {
		if !current.Deleted {
			return &workspaceCopyOutConflict{Path: rel, Reason: "changed independently"}, nil
		}
		localPath, err := workspaceLocalPath(localRoot, rel)
		if err != nil {
			return nil, err
		}
		if _, err := os.Lstat(localPath); err == nil {
			return &workspaceCopyOutConflict{Path: rel, Reason: "exists outside workspace copy-in base"}, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("inspect local workspace path %q: %w", rel, err)
		}
		return nil, nil
	}
	if current.Deleted || current.SHA256 != expected.SHA256 || current.Mode != expected.Mode {
		return &workspaceCopyOutConflict{Path: rel, Reason: "changed independently"}, nil
	}
	return nil, nil
}

func gitWorkspaceCopyOutPathConflict(localRoot, baseCommit, rel string) (*workspaceCopyOutConflict, error) {
	normalized, err := workspaceRelativePath(rel)
	if err != nil {
		return nil, err
	}
	status, err := gitOutput(localRoot, "status", "--porcelain", "--", normalized)
	if err != nil {
		return nil, fmt.Errorf("inspect local workspace path %q: %w", normalized, err)
	}
	if strings.TrimSpace(status) != "" {
		return &workspaceCopyOutConflict{Path: normalized, Reason: "changed independently"}, nil
	}
	existsInBaseline, err := gitPathExistsInCommit(localRoot, baseCommit, normalized)
	if err != nil {
		return nil, err
	}
	if existsInBaseline {
		return nil, nil
	}
	localPath, err := workspaceLocalPath(localRoot, normalized)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(localPath); err == nil {
		return &workspaceCopyOutConflict{Path: normalized, Reason: "exists outside sandbox baseline"}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect local workspace path %q: %w", normalized, err)
	}
	return nil, nil
}

func gitPathExistsInCommit(localRoot, commit, rel string) (bool, error) {
	output, err := gitOutput(localRoot, "ls-tree", "--name-only", "-z", commit, "--", rel)
	if err != nil {
		return false, fmt.Errorf("inspect sandbox baseline path %q: %w", rel, err)
	}
	return strings.Trim(output, "\x00 \n\r\t") != "", nil
}

func workspaceCopyOutConflictError(conflicts []workspaceCopyOutConflict, forceHint string) error {
	if len(conflicts) == 0 {
		return nil
	}
	byPath := make(map[string]workspaceCopyOutConflict, len(conflicts))
	for _, conflict := range conflicts {
		path := strings.TrimSpace(conflict.Path)
		reason := strings.TrimSpace(conflict.Reason)
		if path == "" {
			continue
		}
		if reason == "" {
			reason = "changed independently"
		}
		if _, ok := byPath[path]; !ok {
			byPath[path] = workspaceCopyOutConflict{Path: path, Reason: reason}
		}
	}
	if len(byPath) == 0 {
		return nil
	}
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	if len(paths) == 1 {
		conflict := byPath[paths[0]]
		return fmt.Errorf("local workspace path %q %s; refusing workspace copy-out; %s", conflict.Path, conflict.Reason, workspaceCopyOutForceSuggestion(forceHint))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "workspace copy-out found %d local conflicts; refusing workspace copy-out; %s:", len(paths), workspaceCopyOutForceSuggestion(forceHint))
	for _, path := range paths {
		conflict := byPath[path]
		fmt.Fprintf(&b, "\n- %s: %s", conflict.Path, conflict.Reason)
	}
	return errors.New(b.String())
}

func workspaceCopyOutForceHint(sandboxID string) string {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		sandboxID = "<sandbox-id>"
	}
	return "cleanroom workspace copy-out --force " + sandboxID
}

func workspaceCopyOutForceSuggestion(forceHint string) string {
	forceHint = strings.TrimSpace(forceHint)
	if forceHint == "" {
		forceHint = workspaceCopyOutForceHint("")
	}
	return "run " + forceHint + " to overwrite local target paths"
}

func captureGitWorkspaceCopyOutPayload(callCtx context.Context, ctx *runtimeContext, client *controlclient.Client, opts workspaceCopyOptions, checkout *repositorycheckout.Checkout) ([]repositorychangeset.File, []byte, error) {
	command := repositorychangeset.WorktreeCopyOutCommand(checkout)
	if len(command) == 0 {
		return nil, nil, errors.New("sandbox repository checkout is missing the information needed to copy workspace changes out")
	}
	output, err := runWorkspaceExecutionCapture(callCtx, ctx, client, opts.SandboxID, command, opts.LaunchSeconds)
	if err != nil {
		return nil, nil, err
	}
	nameStatus, patch, err := parseGitWorkspaceCopyOutPayload(output)
	if err != nil {
		return nil, nil, err
	}
	files, err := parseGitNameStatusWorkspaceFiles(nameStatus)
	if err != nil {
		return nil, nil, err
	}
	return files, patch, nil
}

func useGitWorkspaceCopyOutApplyBase(binding *workspaceBinding, local *resolvedRepositoryCheckout, checkout *repositorycheckout.Checkout) bool {
	if binding == nil {
		return false
	}
	if len(binding.CopyInManifest) > 0 {
		return true
	}
	if local == nil || checkout == nil {
		return false
	}
	localCommit := strings.TrimSpace(local.CommitSHA)
	sandboxCommit := strings.TrimSpace(checkout.CommitSHA)
	return localCommit != "" && sandboxCommit != "" && !strings.EqualFold(localCommit, sandboxCommit)
}

func prepareGitWorkspaceCopyOutApplyPatch(local *resolvedRepositoryCheckout, checkout *repositorycheckout.Checkout, files []repositorychangeset.File, sandboxPatchPath string) ([]repositorychangeset.File, string, func(), error) {
	return prepareGitWorkspaceCopyOutApplyPatchWithOmittedPaths(local, checkout, files, sandboxPatchPath, nil)
}

func prepareGitWorkspaceCopyOutApplyPatchWithOmittedPaths(local *resolvedRepositoryCheckout, checkout *repositorycheckout.Checkout, files []repositorychangeset.File, sandboxPatchPath string, omittedLocalPaths []string) ([]repositorychangeset.File, string, func(), error) {
	if local == nil || strings.TrimSpace(local.RootDir) == "" {
		return nil, "", nil, errors.New("workspace copy-out requires a local Git repository checkout matching the sandbox repository")
	}
	if checkout == nil || strings.TrimSpace(checkout.CommitSHA) == "" {
		return nil, "", nil, errors.New("workspace copy-out requires a sandbox repository baseline")
	}
	nameStatus, patch, err := buildGitWorkspaceCopyOutPatchFromLocalBase(local.RootDir, checkout.CommitSHA, sandboxPatchPath, files, omittedLocalPaths)
	if err != nil {
		return nil, "", nil, err
	}
	applyFiles, err := parseGitNameStatusWorkspaceFiles(nameStatus)
	if err != nil {
		return nil, "", nil, err
	}
	if len(applyFiles) == 0 {
		return applyFiles, "", func() {}, nil
	}
	patchPath, cleanup, err := writeTemporaryWorkspaceCopyOutPatch(patch)
	if err != nil {
		return nil, "", nil, fmt.Errorf("write workspace copy-out local apply patch: %w", err)
	}
	return applyFiles, patchPath, cleanup, nil
}

func resetGitWorkspaceCopyOutForceIndex(localRoot string, files []repositorychangeset.File, extraPaths []string) error {
	paths, err := workspaceCopyOutPaths(files)
	if err != nil {
		return err
	}
	paths, err = mergeWorkspaceCopyOutPaths(paths, extraPaths)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return nil
	}
	args := append([]string{"reset", "-q", "--"}, gitWorkspacePathspecs(paths)...)
	if _, err := gitOutputRaw(localRoot, nil, args...); err != nil {
		return fmt.Errorf("reset staged workspace copy-out target paths: %w", err)
	}
	return nil
}

func snapshotGitWorkspaceCopyOutForceIndex(localRoot string, files []repositorychangeset.File, extraPaths []string) (func() error, func(), error) {
	paths, err := workspaceCopyOutPaths(files)
	if err != nil {
		return nil, nil, err
	}
	paths, err = mergeWorkspaceCopyOutPaths(paths, extraPaths)
	if err != nil {
		return nil, nil, err
	}
	if len(paths) == 0 {
		return func() error { return nil }, func() {}, nil
	}
	args := append([]string{"diff", "--cached", "--binary", "--full-index", "--no-ext-diff", "--no-color", "--no-renames", "--"}, gitWorkspacePathspecs(paths)...)
	patch, err := gitOutputRaw(localRoot, nil, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("snapshot staged workspace copy-out target paths: %w", err)
	}
	if len(bytes.TrimSpace(patch)) == 0 {
		return func() error { return nil }, func() {}, nil
	}
	patchPath, cleanup, err := writeTemporaryWorkspaceCopyOutPatch(patch)
	if err != nil {
		return nil, nil, err
	}
	return func() error {
		if _, err := gitOutputRaw(localRoot, nil, "apply", "--cached", "--binary", "--whitespace=nowarn", patchPath); err != nil {
			return fmt.Errorf("restore staged workspace copy-out target paths: %w", err)
		}
		return nil
	}, cleanup, nil
}

func mergeWorkspaceCopyOutPaths(paths ...[]string) ([]string, error) {
	seen := make(map[string]struct{})
	var out []string
	for _, group := range paths {
		for _, path := range group {
			normalized, err := workspaceRelativePath(path)
			if err != nil {
				return nil, err
			}
			if _, ok := seen[normalized]; ok {
				continue
			}
			seen[normalized] = struct{}{}
			out = append(out, normalized)
		}
	}
	sort.Strings(out)
	return out, nil
}

type workspaceCopyOutForceQuarantine struct {
	tempRoot string
	moves    []workspaceCopyOutForceMove
}

type workspaceCopyOutForceMove struct {
	path     string
	original string
	stashed  string
}

func quarantineGitWorkspaceCopyOutForceObstacles(localRoot string, files []repositorychangeset.File) (*workspaceCopyOutForceQuarantine, error) {
	candidates, err := gitWorkspaceCopyOutForceObstaclePathsForFiles(localRoot, files)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	tempRoot, err := os.MkdirTemp(localRoot, ".cleanroom-copy-out-obstacles-*")
	if err != nil {
		return nil, fmt.Errorf("create forced copy-out obstacle quarantine: %w", err)
	}
	q := &workspaceCopyOutForceQuarantine{tempRoot: tempRoot}
	for _, path := range candidates {
		if q.hasMovedAncestor(path) {
			continue
		}
		localPath, err := workspaceLocalPath(localRoot, path)
		if err != nil {
			_ = q.Restore()
			return nil, err
		}
		if _, err := os.Lstat(localPath); err != nil {
			if isWorkspacePathAbsent(err) {
				continue
			}
			_ = q.Restore()
			return nil, fmt.Errorf("inspect forced copy-out obstacle %q: %w", path, err)
		}
		stashedPath := filepath.Join(tempRoot, strconv.Itoa(len(q.moves)))
		if err := os.Rename(localPath, stashedPath); err != nil {
			_ = q.Restore()
			return nil, fmt.Errorf("quarantine forced copy-out obstacle %q: %w", path, err)
		}
		q.moves = append(q.moves, workspaceCopyOutForceMove{
			path:     path,
			original: localPath,
			stashed:  stashedPath,
		})
	}
	if len(q.moves) == 0 {
		q.Cleanup()
		return nil, nil
	}
	return q, nil
}

func gitWorkspaceCopyOutForceObstaclePathsForFiles(localRoot string, files []repositorychangeset.File) ([]string, error) {
	paths, err := workspaceCopyOutPaths(files)
	if err != nil {
		return nil, err
	}
	return gitWorkspaceCopyOutForceObstaclePaths(localRoot, paths)
}

func gitWorkspaceCopyOutForceObstaclePaths(localRoot string, paths []string) ([]string, error) {
	candidates := make(map[string]struct{})
	for _, target := range paths {
		parts := strings.Split(target, "/")
		for i := 1; i < len(parts); i++ {
			prefix := strings.Join(parts[:i], "/")
			localPath, err := workspaceLocalPath(localRoot, prefix)
			if err != nil {
				return nil, err
			}
			info, err := os.Lstat(localPath)
			if err != nil {
				if isWorkspacePathAbsent(err) {
					continue
				}
				return nil, fmt.Errorf("inspect forced copy-out obstacle %q: %w", prefix, err)
			}
			if !info.IsDir() {
				candidates[prefix] = struct{}{}
				break
			}
		}
		localPath, err := workspaceLocalPath(localRoot, target)
		if err != nil {
			return nil, err
		}
		info, err := os.Lstat(localPath)
		if err != nil {
			if isWorkspacePathAbsent(err) {
				continue
			}
			return nil, fmt.Errorf("inspect forced copy-out obstacle %q: %w", target, err)
		}
		if info.IsDir() {
			candidates[target] = struct{}{}
		}
	}
	out := make([]string, 0, len(candidates))
	for path := range candidates {
		out = append(out, path)
	}
	sort.Slice(out, func(i, j int) bool {
		leftDepth := strings.Count(out[i], "/")
		rightDepth := strings.Count(out[j], "/")
		if leftDepth != rightDepth {
			return leftDepth < rightDepth
		}
		return out[i] < out[j]
	})
	return out, nil
}

func isWorkspacePathAbsent(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

func (q *workspaceCopyOutForceQuarantine) hasMovedAncestor(path string) bool {
	if q == nil {
		return false
	}
	for _, move := range q.moves {
		if path == move.path || strings.HasPrefix(path, move.path+"/") {
			return true
		}
	}
	return false
}

func (q *workspaceCopyOutForceQuarantine) Restore() error {
	if q == nil {
		return nil
	}
	var restoreErr error
	for i := len(q.moves) - 1; i >= 0; i-- {
		move := q.moves[i]
		if err := os.RemoveAll(move.original); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("remove partial forced copy-out path %q: %w", move.path, err))
			continue
		}
		if err := os.MkdirAll(filepath.Dir(move.original), 0o755); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("prepare restore for forced copy-out obstacle %q: %w", move.path, err))
			continue
		}
		if err := os.Rename(move.stashed, move.original); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore forced copy-out obstacle %q: %w", move.path, err))
		}
	}
	if restoreErr != nil {
		return fmt.Errorf("%w; forced copy-out obstacle backup remains at %s", restoreErr, q.tempRoot)
	}
	q.Cleanup()
	return nil
}

func (q *workspaceCopyOutForceQuarantine) Cleanup() {
	if q == nil || strings.TrimSpace(q.tempRoot) == "" {
		return
	}
	_ = os.RemoveAll(q.tempRoot)
}

func (q *workspaceCopyOutForceQuarantine) Paths() []string {
	if q == nil || len(q.moves) == 0 {
		return nil
	}
	paths := make([]string, 0, len(q.moves))
	for _, move := range q.moves {
		paths = append(paths, move.path)
	}
	return paths
}

func (q *workspaceCopyOutForceQuarantine) OmittedPaths(localRoot string) ([]string, error) {
	if q == nil {
		return nil, nil
	}
	paths := q.Paths()
	if strings.TrimSpace(q.tempRoot) == "" {
		return paths, nil
	}
	rel, err := filepath.Rel(localRoot, q.tempRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve forced copy-out quarantine path: %w", err)
	}
	rel = filepath.ToSlash(rel)
	if strings.HasPrefix(rel, "../") || rel == ".." || filepath.IsAbs(rel) {
		return nil, fmt.Errorf("forced copy-out quarantine path %q is outside local workspace root %q", q.tempRoot, localRoot)
	}
	path, err := workspaceRelativePath(rel)
	if err != nil {
		return nil, err
	}
	paths = append(paths, path)
	return paths, nil
}

func buildGitWorkspaceCopyOutPatchFromLocalBase(localRoot, baseCommit, sandboxPatchPath string, files []repositorychangeset.File, omittedLocalPaths []string) ([]byte, []byte, error) {
	paths, err := workspaceCopyOutPaths(files)
	if err != nil {
		return nil, nil, err
	}
	if len(paths) == 0 {
		return nil, nil, nil
	}
	pathspecs := gitWorkspacePathspecs(paths)
	localTree, err := gitWorkspaceCurrentTree(localRoot, omittedLocalPaths)
	if err != nil {
		return nil, nil, err
	}
	sandboxTree, err := gitWorkspaceSandboxTree(localRoot, baseCommit, sandboxPatchPath)
	if err != nil {
		return nil, nil, err
	}
	nameStatusArgs := append([]string{"diff", "--name-status", "--no-renames", "-z", localTree, sandboxTree, "--"}, pathspecs...)
	nameStatus, err := gitOutputRaw(localRoot, nil, nameStatusArgs...)
	if err != nil {
		return nil, nil, fmt.Errorf("build workspace copy-out local name-status: %w", err)
	}
	patchArgs := append([]string{"diff", "--binary", "--full-index", "--no-ext-diff", "--no-color", "--no-renames", localTree, sandboxTree, "--"}, pathspecs...)
	patch, err := gitOutputRaw(localRoot, nil, patchArgs...)
	if err != nil {
		return nil, nil, fmt.Errorf("build workspace copy-out local patch: %w", err)
	}
	return nameStatus, patch, nil
}

func gitWorkspaceCurrentTree(localRoot string, omittedLocalPaths []string) (string, error) {
	indexPath, cleanup, err := temporaryGitIndex()
	if err != nil {
		return "", err
	}
	defer cleanup()
	env := append(os.Environ(), "GIT_INDEX_FILE="+indexPath)
	if _, err := gitOutputRaw(localRoot, env, "read-tree", "HEAD"); err != nil {
		return "", fmt.Errorf("initialize local workspace copy-out index: %w", err)
	}
	omittedPaths, err := mergeWorkspaceCopyOutPaths(omittedLocalPaths)
	if err != nil {
		return "", err
	}
	addArgs := []string{"add", "-A", "--all", "."}
	if len(omittedPaths) > 0 {
		addArgs = append(addArgs, gitWorkspaceExcludePathspecs(omittedPaths)...)
	}
	if _, err := gitOutputRaw(localRoot, env, addArgs...); err != nil {
		return "", fmt.Errorf("stage local workspace copy-out base: %w", err)
	}
	if len(omittedPaths) > 0 {
		args := append([]string{"rm", "-r", "--cached", "--ignore-unmatch", "--"}, gitWorkspacePathspecs(omittedPaths)...)
		if _, err := gitOutputRaw(localRoot, env, args...); err != nil {
			return "", fmt.Errorf("omit forced workspace copy-out obstacles from local tree: %w", err)
		}
	}
	tree, err := gitOutputRaw(localRoot, env, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write local workspace copy-out tree: %w", err)
	}
	return strings.TrimSpace(string(tree)), nil
}

func gitWorkspaceSandboxTree(localRoot, baseCommit, sandboxPatchPath string) (string, error) {
	indexPath, cleanup, err := temporaryGitIndex()
	if err != nil {
		return "", err
	}
	defer cleanup()
	env := append(os.Environ(), "GIT_INDEX_FILE="+indexPath)
	if _, err := gitOutputRaw(localRoot, env, "read-tree", strings.ToLower(strings.TrimSpace(baseCommit))); err != nil {
		return "", fmt.Errorf("initialize sandbox workspace copy-out index: %w", err)
	}
	if info, err := os.Stat(sandboxPatchPath); err != nil {
		return "", fmt.Errorf("inspect sandbox workspace copy-out patch: %w", err)
	} else if info.Size() > 0 {
		if _, err := gitOutputRaw(localRoot, env, "apply", "--cached", "--binary", "--whitespace=nowarn", sandboxPatchPath); err != nil {
			return "", fmt.Errorf("apply sandbox workspace copy-out patch to temporary index: %w", err)
		}
	}
	tree, err := gitOutputRaw(localRoot, env, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write sandbox workspace copy-out tree: %w", err)
	}
	return strings.TrimSpace(string(tree)), nil
}

func gitWorkspaceCurrentFiles(localRoot string, paths []string) (map[string]workspaceBindingFile, error) {
	indexPath, cleanup, err := temporaryGitIndex()
	if err != nil {
		return nil, err
	}
	defer cleanup()
	env := append(os.Environ(), "GIT_INDEX_FILE="+indexPath)
	if _, err := gitOutputRaw(localRoot, env, "read-tree", "HEAD"); err != nil {
		return nil, fmt.Errorf("initialize local workspace manifest index: %w", err)
	}
	if _, err := gitOutputRaw(localRoot, env, "add", "-A", "--all", "."); err != nil {
		return nil, fmt.Errorf("stage local workspace manifest state: %w", err)
	}
	files := make(map[string]workspaceBindingFile, len(paths))
	for _, path := range paths {
		file, err := gitWorkspaceIndexFile(localRoot, env, path)
		if err != nil {
			return nil, err
		}
		files[path] = file
	}
	return files, nil
}

func gitWorkspaceStagedPaths(localRoot string, paths []string) (map[string]bool, error) {
	staged := make(map[string]bool)
	if len(paths) == 0 {
		return staged, nil
	}
	args := append([]string{"diff", "--cached", "--name-only", "--no-renames", "-z", "--"}, gitWorkspacePathspecs(paths)...)
	output, err := gitOutputRaw(localRoot, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("inspect staged local workspace paths: %w", err)
	}
	for _, rawPath := range splitNullTerminatedWorkspaceFields(output) {
		path, err := workspaceRelativePath(rawPath)
		if err != nil {
			return nil, err
		}
		staged[path] = true
	}
	return staged, nil
}

func gitWorkspaceCommitFile(localRoot, commit, rel string) (workspaceBindingFile, error) {
	file := workspaceBindingFile{Path: rel}
	mode, exists, err := gitWorkspacePathModeInCommit(localRoot, commit, rel)
	if err != nil {
		return file, err
	}
	if !exists {
		file.Deleted = true
		return file, nil
	}
	file.Mode = mode
	blob, err := gitOutputRaw(localRoot, nil, "show", strings.TrimSpace(commit)+":"+rel)
	if err != nil {
		return file, fmt.Errorf("read sandbox baseline path %q: %w", rel, err)
	}
	file.SHA256 = workspaceSHA256Digest(blob)
	return file, nil
}

func gitWorkspaceIndexFile(localRoot string, env []string, rel string) (workspaceBindingFile, error) {
	file := workspaceBindingFile{Path: rel}
	mode, exists, err := gitWorkspacePathModeInIndex(localRoot, env, rel)
	if err != nil {
		return file, err
	}
	if !exists {
		file.Deleted = true
		return file, nil
	}
	file.Mode = mode
	blob, err := gitOutputRaw(localRoot, env, "show", ":"+rel)
	if err != nil {
		return file, fmt.Errorf("read local workspace path %q from temporary index: %w", rel, err)
	}
	file.SHA256 = workspaceSHA256Digest(blob)
	return file, nil
}

func gitWorkspacePathModeInIndex(localRoot string, env []string, rel string) (string, bool, error) {
	output, err := gitOutputRaw(localRoot, env, "ls-files", "--stage", "-z", "--", gitWorkspacePathspec(rel))
	if err != nil {
		return "", false, fmt.Errorf("inspect local workspace path %q in temporary index: %w", rel, err)
	}
	mode, exists, err := gitWorkspaceEntryMode(output, rel)
	if err != nil {
		return "", false, fmt.Errorf("parse local workspace path %q in temporary index: %w", rel, err)
	}
	return mode, exists, nil
}

func gitWorkspacePathModeInCommit(localRoot, commit, rel string) (string, bool, error) {
	output, err := gitOutputRaw(localRoot, nil, "ls-tree", "-z", strings.TrimSpace(commit), "--", gitWorkspacePathspec(rel))
	if err != nil {
		return "", false, fmt.Errorf("inspect sandbox baseline path %q: %w", rel, err)
	}
	mode, exists, err := gitWorkspaceEntryMode(output, rel)
	if err != nil {
		return "", false, fmt.Errorf("parse sandbox baseline path %q: %w", rel, err)
	}
	return mode, exists, nil
}

func gitWorkspaceEntryMode(output []byte, rel string) (string, bool, error) {
	normalized, err := workspaceRelativePath(rel)
	if err != nil {
		return "", false, err
	}
	output = bytes.TrimRight(output, "\x00")
	if len(bytes.TrimSpace(output)) == 0 {
		return "", false, nil
	}
	for _, entry := range bytes.Split(output, []byte{0}) {
		metadata, rawPath, ok := bytes.Cut(entry, []byte{'\t'})
		if !ok {
			return "", false, fmt.Errorf("parse git entry %q", string(entry))
		}
		path, err := workspaceRelativePath(string(rawPath))
		if err != nil {
			return "", false, err
		}
		if path != normalized {
			continue
		}
		fields := strings.Fields(string(metadata))
		if len(fields) < 3 || strings.TrimSpace(fields[0]) == "" {
			return "", false, fmt.Errorf("parse git entry %q", string(entry))
		}
		return fields[0], true, nil
	}
	return "", false, nil
}

func workspaceCopyOutPaths(files []repositorychangeset.File) ([]string, error) {
	paths := make([]string, 0, len(files))
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		path, err := workspaceRelativePath(file.Path)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

func gitWorkspacePathspecs(paths []string) []string {
	pathspecs := make([]string, 0, len(paths))
	for _, path := range paths {
		pathspecs = append(pathspecs, gitWorkspacePathspec(path))
	}
	return pathspecs
}

func gitWorkspaceExcludePathspecs(paths []string) []string {
	pathspecs := make([]string, 0, len(paths))
	for _, path := range paths {
		pathspecs = append(pathspecs, gitWorkspaceExcludePathspec(path))
	}
	return pathspecs
}

func gitWorkspacePathspec(path string) string {
	return ":(literal)" + path
}

func gitWorkspaceExcludePathspec(path string) string {
	return ":(exclude,literal)" + path
}

func temporaryGitIndex() (string, func(), error) {
	indexFile, err := os.CreateTemp("", "cleanroom-workspace-copy-out-index-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temporary git index: %w", err)
	}
	indexPath := indexFile.Name()
	if err := indexFile.Close(); err != nil {
		_ = os.Remove(indexPath)
		return "", nil, fmt.Errorf("close temporary git index %q: %w", indexPath, err)
	}
	return indexPath, func() { _ = os.Remove(indexPath) }, nil
}

func writeTemporaryWorkspaceCopyOutPatch(patch []byte) (string, func(), error) {
	patchFile, err := os.CreateTemp("", "cleanroom-workspace-copy-out-*.patch")
	if err != nil {
		return "", nil, fmt.Errorf("create temporary workspace copy-out patch: %w", err)
	}
	patchPath := patchFile.Name()
	if _, err := patchFile.Write(patch); err != nil {
		_ = patchFile.Close()
		_ = os.Remove(patchPath)
		return "", nil, fmt.Errorf("write temporary workspace copy-out patch: %w", err)
	}
	if err := patchFile.Close(); err != nil {
		_ = os.Remove(patchPath)
		return "", nil, fmt.Errorf("close temporary workspace copy-out patch %q: %w", patchPath, err)
	}
	return patchPath, func() { _ = os.Remove(patchPath) }, nil
}

func gitOutputRaw(dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.Output()
	if err != nil {
		msg := err.Error()
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if stderr := strings.TrimSpace(string(exitErr.Stderr)); stderr != "" {
				msg = stderr
			}
		}
		return nil, errors.New(msg)
	}
	return out, nil
}

func workspaceSHA256Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func applyGitWorkspaceCopyOutPatch(localRoot, patchPath string) error {
	if strings.TrimSpace(localRoot) == "" {
		return errors.New("missing local workspace root")
	}
	if strings.TrimSpace(patchPath) == "" {
		return errors.New("missing workspace copy-out patch")
	}
	if _, err := gitOutput(localRoot, "apply", "--binary", "--whitespace=nowarn", patchPath); err != nil {
		return fmt.Errorf("apply workspace copy-out patch: %w", err)
	}
	return nil
}

func safeWorkspaceStateName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "sandbox"
	}
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-.")
	if name == "" {
		return "sandbox"
	}
	return name
}

func shortGitSHA(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

func resolveGitWorkspaceCheckout(callCtx context.Context, client *controlclient.Client, opts workspaceCopyOptions) (*resolvedRepositoryCheckout, *repositorycheckout.Checkout, error) {
	repository := opts.Repository
	if repository == nil {
		return nil, nil, errors.New("workspace copy-in requires a local repository checkout")
	}
	effectiveRepository := *repository
	destination := strings.TrimSpace(opts.Destination)
	if destination == "" {
		sandboxRepository, err := resolveSandboxRepositoryCheckout(callCtx, client, opts.SandboxID)
		if err != nil {
			return nil, nil, err
		}
		destination = strings.TrimSpace(sandboxRepository.GetDestinationDir())
		if remoteURL := strings.TrimSpace(sandboxRepository.GetRemoteUrl()); remoteURL != "" {
			effectiveRepository.RemoteURL = remoteURL
		}
		if commitSHA := strings.TrimSpace(sandboxRepository.GetCommitSha()); commitSHA != "" {
			effectiveRepository.CommitSHA = commitSHA
		}
		effectiveRepository.Submodules = sandboxRepository.GetSubmodules()
		effectiveRepository.Branch = strings.TrimSpace(sandboxRepository.GetBranch())
	}
	effectiveRepository.DestinationDir = destination
	checkout := toRepositoryCheckout(&effectiveRepository)
	return &effectiveRepository, checkout, nil
}

func runWorkspaceExecution(callCtx context.Context, ctx *runtimeContext, client *controlclient.Client, sandboxID string, repository *resolvedRepositoryCheckout, command []string, input io.Reader, launchSeconds int64) error {
	return runWorkspaceExecutionWithStdout(callCtx, ctx, client, sandboxID, repository, command, input, launchSeconds, runtimeStdout(ctx))
}

func runWorkspaceExecutionCapture(callCtx context.Context, ctx *runtimeContext, client *controlclient.Client, sandboxID string, command []string, launchSeconds int64) ([]byte, error) {
	var stdout bytes.Buffer
	if err := runWorkspaceExecutionWithStdout(callCtx, ctx, client, sandboxID, nil, command, nil, launchSeconds, &stdout); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

func runWorkspaceExecutionWithStdout(callCtx context.Context, ctx *runtimeContext, client *controlclient.Client, sandboxID string, repository *resolvedRepositoryCheckout, command []string, input io.Reader, launchSeconds int64, stdout io.Writer) error {
	if len(command) == 0 {
		return errors.New("workspace operation execution command is empty")
	}
	createResp, err := client.CreateWorkspaceCopyInExecution(tracePreservingContext(callCtx), &cleanroomv1.CreateExecutionRequest{
		SandboxId:          sandboxID,
		Command:            command,
		Kind:               cleanroomv1.ExecutionKind_EXECUTION_KIND_BATCH,
		RepositoryCheckout: repositoryCheckoutProto(repository),
		Options: &cleanroomv1.ExecutionOptions{
			PreserveRepositoryChangesetPendingExecution: true,
			SkipRunBefore: true,
			LaunchSeconds: launchSeconds,
		},
	})
	if err != nil {
		return fmt.Errorf("create workspace operation execution: %w", err)
	}
	executionID := strings.TrimSpace(createResp.GetExecution().GetExecutionId())
	if executionID == "" {
		return errors.New("workspace operation execution response missing execution id")
	}
	return streamWorkspaceExecutionWithInput(callCtx, ctx, client, sandboxID, executionID, input, stdout)
}

func gitWorkspacePlan(destination string, files []repositorychangeset.File, reset bool) []workspacePlanEntry {
	entries := make([]workspacePlanEntry, 0, len(files)+1)
	if reset {
		entries = append(entries, workspacePlanEntry{
			Action: "reset",
			Path:   workspaceRemotePath(destination, ""),
		})
	}
	for _, file := range files {
		action := "write"
		if file.Deleted {
			action = "delete"
		}
		entries = append(entries, workspacePlanEntry{
			Action: action,
			Path:   workspaceRemotePath(destination, file.Path),
		})
	}
	sortWorkspacePlan(entries)
	return entries
}

func gitWorkspaceCopyOutPlan(localRoot string, nameStatus []byte) ([]workspacePlanEntry, error) {
	files, err := parseGitNameStatusWorkspaceFiles(nameStatus)
	if err != nil {
		return nil, err
	}
	return gitWorkspaceCopyOutPlanFiles(localRoot, files)
}

func gitWorkspaceCopyOutPlanFiles(localRoot string, files []repositorychangeset.File) ([]workspacePlanEntry, error) {
	entries := make([]workspacePlanEntry, 0, len(files))
	for _, file := range files {
		localPath, err := workspaceLocalPath(localRoot, file.Path)
		if err != nil {
			return nil, err
		}
		action := "write"
		if file.Deleted {
			action = "delete"
		}
		entries = append(entries, workspacePlanEntry{
			Action: action,
			Path:   localPath,
		})
	}
	sortWorkspacePlan(entries)
	return entries, nil
}

func parseGitWorkspaceCopyOutPayload(output []byte) ([]byte, []byte, error) {
	header, payload, ok := bytes.Cut(output, []byte("\n"))
	if !ok {
		return nil, nil, errors.New("parse workspace copy-out payload: missing header")
	}
	fields := strings.Fields(string(header))
	if len(fields) != 3 || fields[0] != "cleanroom-copy-out-v1" {
		return nil, nil, fmt.Errorf("parse workspace copy-out payload header %q", string(header))
	}
	nameStatusSize, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || nameStatusSize < 0 {
		return nil, nil, fmt.Errorf("parse workspace copy-out name-status size %q", fields[1])
	}
	patchSize, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || patchSize < 0 {
		return nil, nil, fmt.Errorf("parse workspace copy-out patch size %q", fields[2])
	}
	totalSize := nameStatusSize + patchSize
	if totalSize < nameStatusSize || totalSize > int64(len(payload)) {
		return nil, nil, fmt.Errorf("parse workspace copy-out payload: expected %d bytes after header, got %d", totalSize, len(payload))
	}
	if totalSize != int64(len(payload)) {
		return nil, nil, fmt.Errorf("parse workspace copy-out payload: unexpected trailing data after %d bytes", totalSize)
	}
	nameStatus := payload[:nameStatusSize]
	patch := payload[nameStatusSize:totalSize]
	return nameStatus, patch, nil
}

func parseGitNameStatusWorkspaceFiles(output []byte) ([]repositorychangeset.File, error) {
	tokens := splitNullTerminatedWorkspaceFields(output)
	files := make([]repositorychangeset.File, 0, len(tokens)/2)
	for i := 0; i < len(tokens); i += 2 {
		if i+1 >= len(tokens) {
			return nil, fmt.Errorf("parse workspace changes file status %q", tokens[i])
		}
		status := strings.TrimSpace(tokens[i])
		if status == "" {
			return nil, errors.New("parse workspace changes: empty file status")
		}
		rel, err := workspaceRelativePath(tokens[i+1])
		if err != nil {
			return nil, err
		}
		files = append(files, repositorychangeset.File{
			Path:    rel,
			Deleted: strings.HasPrefix(status, "D"),
		})
	}
	return files, nil
}

func splitNullTerminatedWorkspaceFields(output []byte) []string {
	trimmed := bytes.TrimRight(output, "\x00")
	if len(trimmed) == 0 {
		return nil
	}
	parts := bytes.Split(trimmed, []byte{0})
	fields := make([]string, 0, len(parts))
	for _, part := range parts {
		fields = append(fields, string(part))
	}
	return fields
}

func workspaceLocalPath(root, rel string) (string, error) {
	normalized, err := workspaceRelativePath(rel)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, filepath.FromSlash(normalized)), nil
}

func workspaceRelativePath(rel string) (string, error) {
	slashed := filepath.ToSlash(rel)
	cleaned := posixpath.Clean(slashed)
	windowsCleaned := posixpath.Clean(strings.ReplaceAll(slashed, "\\", "/"))
	if cleaned == "." || cleaned == "" || strings.HasPrefix(cleaned, "/") || hasWindowsDriveAbsolutePathPrefix(windowsCleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("workspace path %q is not a safe relative path", rel)
	}
	return cleaned, nil
}

func hasWindowsDriveAbsolutePathPrefix(path string) bool {
	if len(path) < 3 || path[1] != ':' || path[2] != '/' {
		return false
	}
	drive := path[0]
	return (drive >= 'A' && drive <= 'Z') || (drive >= 'a' && drive <= 'z')
}

func resolveSandboxWorkspaceDestination(callCtx context.Context, client *controlclient.Client, sandboxID string) (string, error) {
	repository, err := resolveSandboxRepositoryCheckout(callCtx, client, sandboxID)
	if err != nil {
		return "", err
	}
	destination := strings.TrimSpace(repository.GetDestinationDir())
	if destination == "" {
		return "", fmt.Errorf("sandbox %q does not have a recorded workspace root; create it from a repository checkout or use cleanroom copy for explicit paths", sandboxID)
	}
	return destination, nil
}

func resolveSandboxWorkspaceDestinationIfRecorded(callCtx context.Context, client *controlclient.Client, sandboxID string) (string, error) {
	repository, err := resolveSandboxRepositoryCheckoutIfRecorded(callCtx, client, sandboxID)
	if err != nil || repository == nil {
		return "", err
	}
	return strings.TrimSpace(repository.GetDestinationDir()), nil
}

func resolveSandboxRepositoryCheckout(callCtx context.Context, client *controlclient.Client, sandboxID string) (*cleanroomv1.RepositoryCheckout, error) {
	repository, err := resolveSandboxRepositoryCheckoutIfRecorded(callCtx, client, sandboxID)
	if err != nil {
		return nil, err
	}
	if repository == nil || strings.TrimSpace(repository.GetDestinationDir()) == "" {
		return nil, fmt.Errorf("sandbox %q does not have a recorded workspace root; create it from a repository checkout or use cleanroom copy for explicit paths", sandboxID)
	}
	return repository, nil
}

func resolveSandboxRepositoryCheckoutIfRecorded(callCtx context.Context, client *controlclient.Client, sandboxID string) (*cleanroomv1.RepositoryCheckout, error) {
	if client == nil {
		return nil, errors.New("workspace copy-in requires a control client")
	}
	resp, err := client.GetSandbox(tracePreservingContext(callCtx), &cleanroomv1.GetSandboxRequest{
		SandboxId: sandboxID,
	})
	if err != nil {
		return nil, fmt.Errorf("inspect sandbox workspace: %w", err)
	}
	sandbox := resp.GetSandbox()
	if sandbox == nil {
		return nil, fmt.Errorf("sandbox %q not found", sandboxID)
	}
	if sandbox.GetRepositoryCheckout() == nil {
		return nil, nil
	}
	return sandbox.GetRepositoryCheckout(), nil
}

func workspaceRemotePath(root, rel string) string {
	root = strings.TrimRight(strings.TrimSpace(root), "/")
	if root == "" {
		root = defaultRepositoryOverridePath
	}
	rel = strings.TrimPrefix(posixpath.Clean(filepath.ToSlash(rel)), "/")
	if rel == "." || rel == "" {
		return root
	}
	return posixpath.Join(root, rel)
}

func printWorkspacePlan(w io.Writer, entries []workspacePlanEntry) error {
	for _, entry := range entries {
		if _, err := fmt.Fprintf(w, "%s\t%s\n", entry.Action, entry.Path); err != nil {
			return err
		}
	}
	return nil
}

func sortWorkspacePlan(entries []workspacePlanEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Action != entries[j].Action {
			return entries[i].Action < entries[j].Action
		}
		return entries[i].Path < entries[j].Path
	})
}

func streamWorkspaceExecutionWithInput(callCtx context.Context, ctx *runtimeContext, client *controlclient.Client, sandboxID, executionID string, input io.Reader, stdout io.Writer) error {
	if stdout == nil {
		stdout = runtimeStdout(ctx)
	}
	streamCtx, streamCancel := context.WithCancel(tracePreservingContext(callCtx))
	defer streamCancel()
	stream, err := client.StreamExecution(streamCtx, &cleanroomv1.StreamExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Follow:      true,
	})
	if err != nil {
		return fmt.Errorf("stream workspace operation execution: %w", err)
	}

	var stdinErrCh <-chan error
	if input != nil {
		ch := make(chan error, 1)
		stdinErrCh = ch
		go func() {
			err := writeExecutionInput(tracePreservingContext(callCtx), client, sandboxID, executionID, input)
			if err != nil {
				streamCancel()
			}
			ch <- err
			close(ch)
		}()
	}

	exitCode := 0
	haveExitCode := false
	for stream.Receive() {
		event := stream.Msg()
		switch payload := event.Payload.(type) {
		case *cleanroomv1.ExecutionStreamEvent_Stdout:
			if _, err := stdout.Write(payload.Stdout); err != nil {
				return err
			}
		case *cleanroomv1.ExecutionStreamEvent_Stderr:
			if _, err := ctx.stderr().Write(payload.Stderr); err != nil {
				return err
			}
		case *cleanroomv1.ExecutionStreamEvent_Warning:
			if err := writeExecutionWarning(ctx.stderr(), payload.Warning); err != nil {
				return err
			}
		case *cleanroomv1.ExecutionStreamEvent_Exit:
			exitCode = int(payload.Exit.GetExitCode())
			haveExitCode = true
		}
		if err := pollExecutionStdinErr(stdinErrCh); err != nil {
			streamCancel()
			return err
		}
	}
	if err := stream.Err(); err != nil && !isCanceledStreamErr(err) {
		return fmt.Errorf("stream workspace operation execution: %w", err)
	}
	if err := waitExecutionStdinErr(stdinErrCh, executionStdinErrDrainTimeout); err != nil {
		return err
	}
	if !haveExitCode {
		if fetchedExitCode, ok := getFinalExecutionExitCode(callCtx, client, sandboxID, executionID); ok {
			exitCode = fetchedExitCode
			haveExitCode = true
		}
	}
	if !haveExitCode {
		return errors.New("workspace operation execution ended without exit status")
	}
	if exitCode != 0 {
		return exitCodeError{code: exitCode}
	}
	return nil
}

func writeExecutionInput(callCtx context.Context, client *controlclient.Client, sandboxID, executionID string, r io.Reader) error {
	buf := make([]byte, 32*1024)
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			if _, err := client.WriteExecutionStdin(callCtx, &cleanroomv1.WriteExecutionStdinRequest{
				SandboxId:   sandboxID,
				ExecutionId: executionID,
				Data:        append([]byte(nil), buf[:n]...),
			}); err != nil {
				return fmt.Errorf("write workspace operation payload: %w", err)
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return fmt.Errorf("read workspace operation payload: %w", readErr)
			}
			break
		}
	}
	return closeExecutionStdin(callCtx, client, sandboxID, executionID)
}

func runtimeStdout(ctx *runtimeContext) io.Writer {
	if ctx != nil && ctx.Stdout != nil {
		return ctx.Stdout
	}
	return os.Stdout
}

func workspaceWorkdirCheckout(destination string) *repositorycheckout.Checkout {
	return &repositorycheckout.Checkout{DestinationDir: destination}
}
