package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	posixpath "path"
	"path/filepath"
	"sort"
	"strings"

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
	Destination   string
	ForceGitReset bool
	LaunchSeconds int64
}

type workspacePlanEntry struct {
	Action string
	Path   string
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
	if !c.DryRun {
		return errors.New("workspace copy-out writes are not implemented yet; use --dry-run to preview sandbox changes")
	}
	repository, err := resolveWorkspaceCopyRepositoryCheckout(cwd, ctx.Loader)
	if err != nil {
		return err
	}
	localRoot := cwd
	if repository != nil && strings.TrimSpace(repository.RootDir) != "" {
		localRoot = repository.RootDir
	}
	client, err := c.connect(ctx)
	if err != nil {
		return err
	}
	return previewWorkspaceCopyOut(context.Background(), ctx, client, workspaceCopyOptions{
		CWD:        localRoot,
		SandboxID:  c.SandboxID,
		DryRun:     true,
		Repository: repository,
	})
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
	return copyRawWorkspaceToSandbox(callCtx, ctx, client, opts)
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
			return runWorkspaceExecution(callCtx, ctx, client, opts.SandboxID, effectiveRepository, repositorychangeset.ResetCommand(checkout), nil, opts.LaunchSeconds)
		}
		return nil
	}
	if opts.DryRun {
		return printWorkspacePlan(runtimeStdout(ctx), gitWorkspacePlan(checkout.DestinationDir, changeset.Files, opts.ForceGitReset))
	}

	command := repositorychangeset.ApplyCommand(checkout, changeset)
	if opts.ForceGitReset {
		command = repositorychangeset.ApplyCommandResettingCheckout(checkout, changeset)
	}
	return runWorkspaceExecution(callCtx, ctx, client, opts.SandboxID, effectiveRepository, command, bytes.NewReader(changeset.Patch), opts.LaunchSeconds)
}

func previewWorkspaceCopyOut(callCtx context.Context, ctx *runtimeContext, client *controlclient.Client, opts workspaceCopyOptions) error {
	if strings.TrimSpace(opts.SandboxID) == "" {
		return errors.New("missing sandbox id")
	}
	if strings.TrimSpace(opts.CWD) == "" {
		return errors.New("missing local workspace root")
	}
	checkout, err := sandboxRepositoryCheckout(callCtx, client, opts.SandboxID)
	if err != nil {
		return err
	}
	if err := validateWorkspaceCopyOutLocalRepository(opts.Repository, checkout); err != nil {
		return err
	}
	command := repositorychangeset.WorktreeNameStatusCommand(checkout)
	if len(command) == 0 {
		return errors.New("sandbox repository checkout is missing the information needed to plan workspace copy-out")
	}
	output, err := runWorkspaceExecutionCapture(callCtx, ctx, client, opts.SandboxID, command, opts.LaunchSeconds)
	if err != nil {
		return err
	}
	entries, err := gitWorkspaceCopyOutPlan(opts.CWD, output)
	if err != nil {
		return err
	}
	return printWorkspacePlan(runtimeStdout(ctx), entries)
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
	if cleaned == "." || cleaned == "" || strings.HasPrefix(cleaned, "/") || hasWindowsDrivePathPrefix(windowsCleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("workspace path %q is not a safe relative path", rel)
	}
	return cleaned, nil
}

func hasWindowsDrivePathPrefix(path string) bool {
	if len(path) < 2 || path[1] != ':' {
		return false
	}
	drive := path[0]
	return (drive >= 'A' && drive <= 'Z') || (drive >= 'a' && drive <= 'z')
}

func copyRawWorkspaceToSandbox(callCtx context.Context, ctx *runtimeContext, client *controlclient.Client, opts workspaceCopyOptions) error {
	destination := strings.TrimSpace(opts.Destination)
	if destination == "" {
		var err error
		destination, err = resolveSandboxWorkspaceDestination(callCtx, client, opts.SandboxID)
		if err != nil {
			return err
		}
	}
	if err := validateRawWorkspaceDestinationRoot(destination); err != nil {
		return err
	}
	if opts.DryRun {
		entries, err := rawWorkspacePlan(opts.CWD, destination)
		if err != nil {
			return err
		}
		return printWorkspacePlan(runtimeStdout(ctx), entries)
	}
	if _, err := rawWorkspacePlan(opts.CWD, destination); err != nil {
		return err
	}
	if err := cleanRawWorkspaceDestination(callCtx, ctx, client, opts.SandboxID, destination, opts.LaunchSeconds); err != nil {
		return err
	}
	if err := extractRawWorkspaceArchive(callCtx, client, opts.SandboxID, destination, opts.CWD); err != nil {
		return err
	}
	return nil
}

func cleanRawWorkspaceDestination(callCtx context.Context, ctx *runtimeContext, client *controlclient.Client, sandboxID, destination string, launchSeconds int64) error {
	statResp, err := client.StatSandboxPath(tracePreservingContext(callCtx), &cleanroomv1.StatSandboxPathRequest{
		SandboxId: sandboxID,
		Path:      destination,
	})
	if err != nil {
		if isSandboxPathNotFoundError(err) {
			return nil
		}
		return fmt.Errorf("inspect sandbox workspace destination: %w", err)
	}
	info := statResp.GetInfo()
	if info == nil {
		return errors.New("inspect sandbox workspace destination: missing path info")
	}
	if info.GetType() != cleanroomv1.SandboxPathType_SANDBOX_PATH_TYPE_DIRECTORY {
		return removeRawWorkspaceDestinationRoot(callCtx, client, sandboxID, destination)
	}
	return runWorkspaceExecution(callCtx, ctx, client, sandboxID, nil, rawWorkspaceCleanCommand(destination), nil, launchSeconds)
}

func removeRawWorkspaceDestinationRoot(callCtx context.Context, client *controlclient.Client, sandboxID, destination string) error {
	if _, err := client.RemoveSandboxPath(tracePreservingContext(callCtx), &cleanroomv1.RemoveSandboxPathRequest{
		SandboxId: sandboxID,
		Path:      destination,
	}); err != nil {
		if isSandboxPathNotFoundError(err) {
			return nil
		}
		return fmt.Errorf("remove non-directory workspace destination: %w", err)
	}
	return nil
}

func rawWorkspaceCleanCommand(destination string) []string {
	script := []string{
		"set -eu",
		"dest=" + workspaceShellQuote(destination),
		`if [ -e "$dest" ] && [ ! -d "$dest" ]; then echo "workspace destination is not a directory: $dest" >&2; exit 1; fi`,
		`mkdir -p "$dest"`,
		`for entry in "$dest"/* "$dest"/.[!.]* "$dest"/..?*; do`,
		`  [ -e "$entry" ] || [ -L "$entry" ] || continue`,
		`  [ "$(basename "$entry")" = ".git" ] && continue`,
		`  rm -rf -- "$entry"`,
		`done`,
	}
	return []string{"sh", "-lc", strings.Join(script, "\n")}
}

func workspaceShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
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

func validateRawWorkspaceDestinationRoot(destination string) error {
	cleaned := posixpath.Clean(strings.TrimSpace(destination))
	if cleaned == "." || cleaned == "" {
		return errors.New("workspace destination root is required")
	}
	if !strings.HasPrefix(cleaned, "/") {
		return fmt.Errorf("workspace destination root %q must be absolute", destination)
	}
	switch cleaned {
	case "/",
		"/bin",
		"/boot",
		"/dev",
		"/etc",
		"/home",
		"/lib",
		"/lib64",
		"/proc",
		"/root",
		"/run",
		"/sbin",
		"/sys",
		"/tmp",
		"/usr",
		"/var":
		return fmt.Errorf("workspace destination root %q is unsafe for raw workspace copy-in", destination)
	default:
		return nil
	}
}

func rawWorkspacePlan(sourceRoot, destination string) ([]workspacePlanEntry, error) {
	entries := []workspacePlanEntry{{
		Action: "delete",
		Path:   destination,
	}}
	err := walkRawWorkspace(sourceRoot, func(path string, d fs.DirEntry, info fs.FileInfo, rel string) error {
		if rel == "" {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		entries = append(entries, workspacePlanEntry{
			Action: "write",
			Path:   workspaceRemotePath(destination, rel),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortWorkspacePlan(entries)
	return entries, nil
}

func extractRawWorkspaceArchive(callCtx context.Context, client *controlclient.Client, sandboxID, destination, sourceRoot string) error {
	stream := client.ExtractSandboxArchive(tracePreservingContext(callCtx))
	if err := stream.Send(&cleanroomv1.ExtractSandboxArchiveRequest{
		Payload: &cleanroomv1.ExtractSandboxArchiveRequest_Init{Init: &cleanroomv1.ExtractSandboxArchiveInit{
			SandboxId:   sandboxID,
			Destination: destination,
		}},
	}); err != nil {
		return fmt.Errorf("start sandbox workspace archive extract: %w", err)
	}

	reader, writer := io.Pipe()
	writeDone := make(chan error, 1)
	go func() {
		err := writeRawWorkspaceTar(writer, sourceRoot)
		if err != nil {
			_ = writer.CloseWithError(err)
		} else {
			_ = writer.Close()
		}
		writeDone <- err
	}()

	buf := make([]byte, 32*1024)
	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			if err := stream.Send(&cleanroomv1.ExtractSandboxArchiveRequest{
				Payload: &cleanroomv1.ExtractSandboxArchiveRequest_Data{Data: append([]byte(nil), buf[:n]...)},
			}); err != nil {
				_ = reader.CloseWithError(err)
				<-writeDone
				return fmt.Errorf("write sandbox workspace archive: %w", err)
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				<-writeDone
				return fmt.Errorf("read local workspace archive: %w", readErr)
			}
			break
		}
	}
	if writeErr := <-writeDone; writeErr != nil {
		return writeErr
	}
	if _, err := stream.CloseAndReceive(); err != nil {
		return fmt.Errorf("extract sandbox workspace archive: %w", err)
	}
	return nil
}

func writeRawWorkspaceTar(w io.Writer, sourceRoot string) (err error) {
	tw := tar.NewWriter(w)
	defer func() {
		if closeErr := tw.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	return walkRawWorkspace(sourceRoot, func(path string, d fs.DirEntry, info fs.FileInfo, rel string) error {
		if rel == "" {
			return nil
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("read symlink %s: %w", path, err)
			}
			link = target
		}

		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return fmt.Errorf("create archive header for %s: %w", path, err)
		}
		header.Name = filepath.ToSlash(rel)
		header.Uid = 0
		header.Gid = 0
		header.Uname = ""
		header.Gname = ""
		if info.IsDir() {
			header.Name = strings.TrimRight(header.Name, "/") + "/"
		}
		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("write archive header for %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		if _, err := io.Copy(tw, file); err != nil {
			_ = file.Close()
			return fmt.Errorf("archive %s: %w", path, err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("close %s: %w", path, err)
		}
		return nil
	})
}

func walkRawWorkspace(sourceRoot string, visit func(path string, d fs.DirEntry, info fs.FileInfo, rel string) error) error {
	sourceRoot, err := resolveRawWorkspaceSourceRoot(sourceRoot)
	if err != nil {
		return err
	}
	return filepath.WalkDir(sourceRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		if rel == "." {
			rel = ""
		}
		if shouldSkipRawWorkspacePath(rel, d) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir(), info.Mode().IsRegular(), info.Mode()&os.ModeSymlink != 0:
			return visit(path, d, info, filepath.ToSlash(rel))
		default:
			return fmt.Errorf("workspace copy-in cannot archive unsupported file %s", path)
		}
	})
}

func resolveRawWorkspaceSourceRoot(sourceRoot string) (string, error) {
	sourceRoot = strings.TrimSpace(sourceRoot)
	if sourceRoot == "" {
		return "", errors.New("missing local workspace root")
	}
	cleaned := filepath.Clean(sourceRoot)
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		return "", fmt.Errorf("resolve workspace source root %q: %w", cleaned, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect workspace source root %q: %w", resolved, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace source root %q is not a directory", cleaned)
	}
	return resolved, nil
}

func shouldSkipRawWorkspacePath(rel string, d fs.DirEntry) bool {
	if rel == "" {
		return false
	}
	name := filepath.Base(rel)
	if name == ".git" {
		return true
	}
	return d.IsDir() && name == ".cleanroom"
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
