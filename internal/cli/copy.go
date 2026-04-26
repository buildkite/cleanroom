package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	posixpath "path"
	"path/filepath"
	"strings"
	"syscall"

	"connectrpc.com/connect"
	"github.com/buildkite/cleanroom/internal/controlclient"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type CopyCommand struct {
	clientFlags
	Source      string `arg:"" required:"" help:"Source path: local path or <sandbox-id>:/absolute/path"`
	Destination string `arg:"" required:"" help:"Destination path: local path or <sandbox-id>:/absolute/path"`
	MaxBytes    int64  `name:"max-bytes" help:"Maximum bytes to copy out of a sandbox (defaults to the server limit)"`
}

type copyOperand struct {
	raw       string
	localPath string
	remote    *copyRemotePath
}

type copyRemotePath struct {
	sandboxID string
	path      string
}

func (c *CopyCommand) Run(ctx *runtimeContext) error {
	source, err := parseCopyOperand(c.Source)
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	destination, err := parseCopyOperand(c.Destination)
	if err != nil {
		return fmt.Errorf("destination: %w", err)
	}

	switch {
	case source.remote != nil && destination.remote != nil:
		return errors.New("copying directly between sandboxes is not supported")
	case source.remote == nil && destination.remote == nil:
		return errors.New("one operand must be <sandbox-id>:/absolute/path")
	case source.remote != nil:
		return c.copyFromSandbox(ctx, source.remote, destination.localPath)
	default:
		return c.copyToSandbox(ctx, source.localPath, destination.remote)
	}
}

func (c *CopyCommand) copyFromSandbox(ctx *runtimeContext, remote *copyRemotePath, localPath string) error {
	client, err := c.connect(ctx)
	if err != nil {
		return err
	}

	destination, err := resolveLocalCopyDestination(localPath, remote.path)
	if err != nil {
		return err
	}
	installDestination, err := resolveLocalInstallDestination(destination)
	if err != nil {
		return err
	}
	sourceInfo, err := sandboxSourceFileInfo(context.Background(), client, remote.sandboxID, remote.path)
	if err != nil {
		return fmt.Errorf("stat sandbox source: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(installDestination), 0o755); err != nil {
		return fmt.Errorf("create local destination parent: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(installDestination), "."+filepath.Base(installDestination)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create local temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()

	stream, err := client.ReadSandboxFile(context.Background(), &cleanroomv1.ReadSandboxFileRequest{
		SandboxId: remote.sandboxID,
		Path:      remote.path,
		MaxBytes:  c.MaxBytes,
	})
	if err != nil {
		return fmt.Errorf("read sandbox file: %w", err)
	}
	for stream.Receive() {
		if _, err := tmp.Write(stream.Msg().GetData()); err != nil {
			return fmt.Errorf("write local file: %w", err)
		}
	}
	if err := stream.Err(); err != nil {
		return fmt.Errorf("read sandbox file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close local temporary file: %w", err)
	}
	if err := applyLocalCopyMetadata(tmpPath, sourceInfo); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, installDestination); err != nil {
		return fmt.Errorf("install local file: %w", err)
	}
	committed = true
	return nil
}

func (c *CopyCommand) copyToSandbox(ctx *runtimeContext, localPath string, remote *copyRemotePath) error {
	client, err := c.connect(ctx)
	if err != nil {
		return err
	}

	info, err := os.Stat(localPath)
	if err != nil {
		return fmt.Errorf("stat local source: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("local source %q is a directory; copy supports files", localPath)
	}

	remotePath, err := resolveRemoteCopyDestination(remote.path, localPath)
	if err != nil {
		return err
	}
	if strings.HasSuffix(remote.path, "/") {
		state, err := sandboxDestinationDirectoryState(context.Background(), client, remote.sandboxID, remoteDirectoryProbePath(remote.path))
		if err != nil {
			return fmt.Errorf("stat sandbox destination: %w", err)
		}
		if state == sandboxDestinationOther {
			return fmt.Errorf("remote destination %q ends with / but is not a directory", remote.path)
		}
	} else if remotePath == remote.path {
		state, err := sandboxDestinationDirectoryState(context.Background(), client, remote.sandboxID, remote.path)
		if err != nil {
			return fmt.Errorf("stat sandbox destination: %w", err)
		}
		if state == sandboxDestinationDirectory {
			remotePath, err = remoteCopyDestinationWithLocalBasename(remote.path, localPath)
			if err != nil {
				return err
			}
		}
	}
	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open local source: %w", err)
	}
	defer file.Close()

	stream := client.WriteSandboxFile(context.Background())
	if err := stream.Send(&cleanroomv1.WriteSandboxFileRequest{
		Payload: &cleanroomv1.WriteSandboxFileRequest_Init{Init: &cleanroomv1.WriteSandboxFileInit{
			SandboxId: remote.sandboxID,
			Path:      remotePath,
			Mode:      uint32(info.Mode().Perm()),
			Mtime:     timestamppb.New(info.ModTime()),
		}},
	}); err != nil {
		return fmt.Errorf("start sandbox file write: %w", err)
	}
	buf := make([]byte, 32*1024)
	for {
		n, readErr := file.Read(buf)
		if n > 0 {
			if err := stream.Send(&cleanroomv1.WriteSandboxFileRequest{
				Payload: &cleanroomv1.WriteSandboxFileRequest_Data{Data: append([]byte(nil), buf[:n]...)},
			}); err != nil {
				return fmt.Errorf("write sandbox file: %w", err)
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return fmt.Errorf("read local source: %w", readErr)
			}
			break
		}
	}
	_, err = stream.CloseAndReceive()
	if err != nil {
		return fmt.Errorf("write sandbox file: %w", err)
	}
	return nil
}

func sandboxSourceFileInfo(ctx context.Context, client *controlclient.Client, sandboxID, remotePath string) (*cleanroomv1.SandboxPathInfo, error) {
	current := remotePath
	seen := map[string]struct{}{}
	for range 40 {
		if _, ok := seen[current]; ok {
			return nil, fmt.Errorf("remote source symlink cycle at %q", remotePath)
		}
		seen[current] = struct{}{}
		statResp, err := client.StatSandboxPath(ctx, &cleanroomv1.StatSandboxPathRequest{
			SandboxId: sandboxID,
			Path:      current,
		})
		if err != nil {
			return nil, err
		}
		info := statResp.GetInfo()
		switch info.GetType() {
		case cleanroomv1.SandboxPathType_SANDBOX_PATH_TYPE_FILE:
			return info, nil
		case cleanroomv1.SandboxPathType_SANDBOX_PATH_TYPE_SYMLINK:
			target := resolveRemoteSymlinkTarget(current, info.GetSymlinkTarget())
			if target == "" {
				return nil, fmt.Errorf("remote source symlink at %q has empty target", current)
			}
			current = target
		case cleanroomv1.SandboxPathType_SANDBOX_PATH_TYPE_DIRECTORY:
			return nil, fmt.Errorf("remote source %q is a directory; copy supports files", remotePath)
		default:
			return nil, fmt.Errorf("remote source %q is not a file", remotePath)
		}
	}
	return nil, fmt.Errorf("remote source symlink chain too deep at %q", remotePath)
}

func applyLocalCopyMetadata(path string, info *cleanroomv1.SandboxPathInfo) error {
	if info == nil {
		return errors.New("missing sandbox source metadata")
	}
	if mtime := info.GetMtime(); mtime != nil {
		if err := mtime.CheckValid(); err != nil {
			return fmt.Errorf("invalid sandbox source mtime: %w", err)
		}
		t := mtime.AsTime()
		if err := os.Chtimes(path, t, t); err != nil {
			return fmt.Errorf("apply local file mtime: %w", err)
		}
	}
	if err := os.Chmod(path, os.FileMode(info.GetMode()).Perm()); err != nil {
		return fmt.Errorf("apply local file mode: %w", err)
	}
	return nil
}

func extractSandboxArchive(ctx context.Context, client *controlclient.Client, sandboxID, destination string, r io.Reader) error {
	stream := client.ExtractSandboxArchive(ctx)
	if err := stream.Send(&cleanroomv1.ExtractSandboxArchiveRequest{
		Payload: &cleanroomv1.ExtractSandboxArchiveRequest_Init{Init: &cleanroomv1.ExtractSandboxArchiveInit{
			SandboxId:   sandboxID,
			Destination: destination,
		}},
	}); err != nil {
		return fmt.Errorf("start sandbox archive extract: %w", err)
	}

	buf := make([]byte, 32*1024)
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			if err := stream.Send(&cleanroomv1.ExtractSandboxArchiveRequest{
				Payload: &cleanroomv1.ExtractSandboxArchiveRequest_Data{Data: append([]byte(nil), buf[:n]...)},
			}); err != nil {
				return fmt.Errorf("extract sandbox archive: %w", err)
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return fmt.Errorf("read sandbox archive payload: %w", readErr)
			}
			break
		}
	}
	if _, err := stream.CloseAndReceive(); err != nil {
		return fmt.Errorf("extract sandbox archive: %w", err)
	}
	return nil
}

func parseCopyOperand(spec string) (copyOperand, error) {
	if strings.Contains(spec, "\x00") {
		return copyOperand{}, errors.New("path contains NUL")
	}
	if idx := strings.Index(spec, ":/"); idx >= 0 {
		sandboxID := strings.TrimSpace(spec[:idx])
		remotePath := spec[idx+1:]
		if sandboxID == "" {
			return copyOperand{}, errors.New("missing sandbox id")
		}
		if remotePath == "" || !strings.HasPrefix(remotePath, "/") {
			return copyOperand{}, errors.New("remote path must be absolute")
		}
		return copyOperand{
			raw: spec,
			remote: &copyRemotePath{
				sandboxID: sandboxID,
				path:      remotePath,
			},
		}, nil
	}
	return copyOperand{raw: spec, localPath: spec}, nil
}

func resolveLocalCopyDestination(localPath, remotePath string) (string, error) {
	if localPath == "" {
		return "", errors.New("missing local destination")
	}
	info, err := os.Stat(localPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if strings.HasSuffix(localPath, string(filepath.Separator)) {
				return "", fmt.Errorf("local destination %q ends with a path separator but does not exist", localPath)
			}
			return localPath, nil
		}
		if errors.Is(err, syscall.ELOOP) {
			if _, resolveErr := resolveLocalInstallDestination(localPath); resolveErr != nil {
				return "", resolveErr
			}
		}
		return "", fmt.Errorf("stat local destination: %w", err)
	}
	if !info.IsDir() {
		return localPath, nil
	}
	base := posixpath.Base(remotePath)
	if base == "." || base == "/" || base == "" {
		return "", fmt.Errorf("cannot infer local filename from remote path %q", remotePath)
	}
	return filepath.Join(localPath, base), nil
}

func resolveLocalInstallDestination(destination string) (string, error) {
	current := destination
	seen := map[string]struct{}{}
	for range 40 {
		info, err := os.Lstat(current)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return current, nil
			}
			return "", fmt.Errorf("lstat local destination: %w", err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return current, nil
		}
		if _, ok := seen[current]; ok {
			return "", fmt.Errorf("local destination symlink cycle at %q", destination)
		}
		seen[current] = struct{}{}
		target, err := os.Readlink(current)
		if err != nil {
			return "", fmt.Errorf("read local destination symlink: %w", err)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(current), target)
		}
		current = filepath.Clean(target)
	}
	return "", fmt.Errorf("local destination symlink chain too deep at %q", destination)
}

func resolveRemoteCopyDestination(remotePath, localPath string) (string, error) {
	if remotePath == "" || !strings.HasPrefix(remotePath, "/") {
		return "", errors.New("remote path must be absolute")
	}
	if !strings.HasSuffix(remotePath, "/") {
		return remotePath, nil
	}
	return remoteCopyDestinationWithLocalBasename(remotePath, localPath)
}

func remoteCopyDestinationWithLocalBasename(remotePath, localPath string) (string, error) {
	base := filepath.Base(localPath)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return "", fmt.Errorf("cannot infer remote filename from local path %q", localPath)
	}
	return posixpath.Join(remotePath, base), nil
}

type sandboxDestinationDirectoryStatus int

const (
	sandboxDestinationMissing sandboxDestinationDirectoryStatus = iota
	sandboxDestinationDirectory
	sandboxDestinationOther
)

func sandboxDestinationDirectoryState(ctx context.Context, client *controlclient.Client, sandboxID, remotePath string) (sandboxDestinationDirectoryStatus, error) {
	current := remotePath
	seen := map[string]struct{}{}
	followedSymlink := false
	for range 40 {
		if _, ok := seen[current]; ok {
			return sandboxDestinationOther, fmt.Errorf("remote destination symlink cycle at %q", remotePath)
		}
		seen[current] = struct{}{}
		statResp, err := client.StatSandboxPath(ctx, &cleanroomv1.StatSandboxPathRequest{
			SandboxId: sandboxID,
			Path:      current,
		})
		if err != nil {
			if isSandboxPathNotFoundError(err) {
				if followedSymlink {
					return sandboxDestinationOther, nil
				}
				return sandboxDestinationMissing, nil
			}
			return sandboxDestinationOther, err
		}
		info := statResp.GetInfo()
		switch info.GetType() {
		case cleanroomv1.SandboxPathType_SANDBOX_PATH_TYPE_DIRECTORY:
			return sandboxDestinationDirectory, nil
		case cleanroomv1.SandboxPathType_SANDBOX_PATH_TYPE_SYMLINK:
			target := resolveRemoteSymlinkTarget(current, info.GetSymlinkTarget())
			if target == "" {
				return sandboxDestinationOther, nil
			}
			current = target
			followedSymlink = true
		default:
			return sandboxDestinationOther, nil
		}
	}
	return sandboxDestinationOther, fmt.Errorf("remote destination symlink chain too deep at %q", remotePath)
}

func remoteDirectoryProbePath(remotePath string) string {
	trimmed := strings.TrimRight(remotePath, "/")
	if trimmed == "" {
		return "/"
	}
	return trimmed
}

func resolveRemoteSymlinkTarget(linkPath, target string) string {
	if target == "" {
		return ""
	}
	if strings.HasPrefix(target, "/") {
		return posixpath.Clean(target)
	}
	return posixpath.Clean(posixpath.Join(posixpath.Dir(linkPath), target))
}

func isSandboxPathNotFoundError(err error) bool {
	return connect.CodeOf(err) == connect.CodeNotFound
}
