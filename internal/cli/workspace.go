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
	Copy WorkspaceCopyCommand `cmd:"" help:"Copy local workspace changes into a sandbox"`
}

type WorkspaceCopyCommand struct {
	clientFlags
	Chdir     string `short:"c" help:"Change to this local directory before planning the workspace copy"`
	DryRun    bool   `name:"dry-run" help:"Show the workspace paths that would be copied without modifying the sandbox"`
	SandboxID string `arg:"" required:"" help:"Sandbox ID to copy into"`
}

type workspaceCopyOptions struct {
	CWD           string
	SandboxID     string
	DryRun        bool
	Repository    *resolvedRepositoryCheckout
	Destination   string
	ForceGitReset bool
}

type workspacePlanEntry struct {
	Action string
	Path   string
}

func (c *WorkspaceCopyCommand) Run(ctx *runtimeContext) error {
	cwd, err := resolveCWD(ctx.CWD, c.Chdir)
	if err != nil {
		return err
	}
	client, err := c.connect(ctx)
	if err != nil {
		return err
	}
	repository, err := resolveRepositoryCheckout(cwd, ctx.Loader)
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
		return errors.New("workspace copy requires a local repository checkout")
	}
	checkout := toRepositoryCheckout(repository)
	changeset, err := repositorychangeset.BuildFromWorkingTree(repository.RootDir, checkout)
	if err != nil {
		return fmt.Errorf("package local workspace changes: %w", err)
	}
	if changeset == nil {
		if opts.DryRun {
			return nil
		}
		if opts.ForceGitReset {
			return runWorkspaceExecution(callCtx, ctx, client, opts.SandboxID, repository, repositorychangeset.ResetCommand(checkout), nil)
		}
		return nil
	}
	if opts.DryRun {
		return printWorkspacePlan(runtimeStdout(ctx), gitWorkspacePlan(repository.DestinationDir, changeset.Files))
	}

	command := repositorychangeset.ApplyCommand(checkout, changeset)
	if opts.ForceGitReset {
		command = repositorychangeset.ApplyCommandResettingCheckout(checkout, changeset)
	}
	return runWorkspaceExecution(callCtx, ctx, client, opts.SandboxID, repository, command, bytes.NewReader(changeset.Patch))
}

func runWorkspaceExecution(callCtx context.Context, ctx *runtimeContext, client *controlclient.Client, sandboxID string, repository *resolvedRepositoryCheckout, command []string, input io.Reader) error {
	if len(command) == 0 {
		return errors.New("workspace copy execution command is empty")
	}
	createResp, err := client.CreateExecution(tracePreservingContext(callCtx), &cleanroomv1.CreateExecutionRequest{
		SandboxId:          sandboxID,
		Command:            command,
		Kind:               cleanroomv1.ExecutionKind_EXECUTION_KIND_BATCH,
		RepositoryCheckout: repositoryCheckoutProto(repository),
	})
	if err != nil {
		return fmt.Errorf("create workspace copy execution: %w", err)
	}
	executionID := strings.TrimSpace(createResp.GetExecution().GetExecutionId())
	if executionID == "" {
		return errors.New("workspace copy execution response missing execution id")
	}
	return streamWorkspaceExecutionWithInput(callCtx, ctx, client, sandboxID, executionID, input)
}

func gitWorkspacePlan(destination string, files []repositorychangeset.File) []workspacePlanEntry {
	entries := make([]workspacePlanEntry, 0, len(files))
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
	if err := cleanRawWorkspaceDestination(callCtx, ctx, client, opts.SandboxID, destination); err != nil {
		return err
	}
	if err := extractRawWorkspaceArchive(callCtx, client, opts.SandboxID, destination, opts.CWD); err != nil {
		return err
	}
	return nil
}

func cleanRawWorkspaceDestination(callCtx context.Context, ctx *runtimeContext, client *controlclient.Client, sandboxID, destination string) error {
	return runWorkspaceExecution(callCtx, ctx, client, sandboxID, nil, rawWorkspaceCleanCommand(destination), nil)
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
	if client == nil {
		return "", errors.New("workspace copy requires a control client")
	}
	resp, err := client.GetSandbox(tracePreservingContext(callCtx), &cleanroomv1.GetSandboxRequest{
		SandboxId: sandboxID,
	})
	if err != nil {
		return "", fmt.Errorf("inspect sandbox workspace: %w", err)
	}
	sandbox := resp.GetSandbox()
	if sandbox == nil {
		return "", fmt.Errorf("sandbox %q not found", sandboxID)
	}
	destination := strings.TrimSpace(sandbox.GetRepositoryCheckout().GetDestinationDir())
	if destination == "" {
		return "", fmt.Errorf("sandbox %q does not have a recorded workspace root; create it from a repository checkout or use cleanroom copy for explicit paths", sandboxID)
	}
	return destination, nil
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
		return fmt.Errorf("workspace destination root %q is unsafe for raw workspace copy", destination)
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
	sourceRoot = filepath.Clean(sourceRoot)
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
			return fmt.Errorf("workspace copy cannot archive unsupported file %s", path)
		}
	})
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

func streamWorkspaceExecutionWithInput(callCtx context.Context, ctx *runtimeContext, client *controlclient.Client, sandboxID, executionID string, input io.Reader) error {
	streamCtx, streamCancel := context.WithCancel(tracePreservingContext(callCtx))
	defer streamCancel()
	stream, err := client.StreamExecution(streamCtx, &cleanroomv1.StreamExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Follow:      true,
	})
	if err != nil {
		return fmt.Errorf("stream workspace copy execution: %w", err)
	}

	var stdinErrCh <-chan error
	if input != nil {
		ch := make(chan error, 1)
		stdinErrCh = ch
		go func() {
			ch <- writeExecutionInput(tracePreservingContext(callCtx), client, sandboxID, executionID, input)
			close(ch)
		}()
	}

	exitCode := 0
	haveExitCode := false
	for stream.Receive() {
		event := stream.Msg()
		switch payload := event.Payload.(type) {
		case *cleanroomv1.ExecutionStreamEvent_Stdout:
			if _, err := runtimeStdout(ctx).Write(payload.Stdout); err != nil {
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
		return fmt.Errorf("stream workspace copy execution: %w", err)
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
		return errors.New("workspace copy execution ended without exit status")
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
				return fmt.Errorf("write workspace copy payload: %w", err)
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return fmt.Errorf("read workspace copy payload: %w", readErr)
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
