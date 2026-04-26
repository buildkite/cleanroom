package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const DefaultSandboxFileTransferMaxBytes int64 = 10 * 1024 * 1024

var ErrSandboxPathNotFound = errors.New("sandbox path not found")

// SandboxCommandRunner runs a transfer command inside a persistent sandbox.
type SandboxCommandRunner func(ctx context.Context, sandboxID string, cmd []string, stream OutputStream) (*ExecutionResult, error)

// SandboxFileTransfer implements backend-neutral sandbox file transfer
// semantics over a backend-specific command runner.
type SandboxFileTransfer struct {
	Run SandboxCommandRunner
}

type SandboxPathNotFoundError struct {
	Path string
}

func (e SandboxPathNotFoundError) Error() string {
	if e.Path == "" {
		return ErrSandboxPathNotFound.Error()
	}
	return ErrSandboxPathNotFound.Error() + ": " + e.Path
}

func (e SandboxPathNotFoundError) Is(target error) bool {
	return target == ErrSandboxPathNotFound
}

func NewSandboxPathNotFoundError(path string) error {
	return SandboxPathNotFoundError{Path: path}
}

func SandboxPathNotFoundErrorFromStderr(stderr string) error {
	msg := strings.TrimSpace(stderr)
	const prefix = "path not found:"
	if !strings.HasPrefix(msg, prefix) {
		return nil
	}
	return NewSandboxPathNotFoundError(strings.TrimSpace(strings.TrimPrefix(msg, prefix)))
}

const SandboxPathInfoScript = `set -eu
p=$1
if [ ! -e "$p" ] && [ ! -L "$p" ]; then
	echo "path not found: $p" >&2
	exit 1
fi
if [ -L "$p" ]; then
	kind=symlink
	target=$(readlink "$p" || true)
elif [ -f "$p" ]; then
	kind=file
	target=
elif [ -d "$p" ]; then
	kind=directory
	target=
else
	kind=other
	target=
fi
size=$(stat -c '%s' -- "$p")
mode=$(stat -c '%a' -- "$p")
mtime=$(stat -c '%Y' -- "$p")
printf '%s\000%s\000%s\000%s\000%s\000%s\000' "$p" "$kind" "$size" "$mode" "$mtime" "$target"
`

const SandboxTreeWalkScript = `set -eu
root=$1
if [ ! -e "$root" ] && [ ! -L "$root" ]; then
	echo "path not found: $root" >&2
	exit 1
fi
find "$root" -exec sh -c '
set -eu
for p do
	if [ -L "$p" ]; then
		kind=symlink
		target=$(readlink "$p" || true)
	elif [ -f "$p" ]; then
		kind=file
		target=
	elif [ -d "$p" ]; then
		kind=directory
		target=
	else
		kind=other
		target=
	fi
	size=$(stat -c "%s" -- "$p")
	mode=$(stat -c "%a" -- "$p")
	mtime=$(stat -c "%Y" -- "$p")
	printf "%s\000%s\000%s\000%s\000%s\000%s\000" "$p" "$kind" "$size" "$mode" "$mtime" "$target"
done
' cleanroom-walk {} +
`

const SandboxFileReadScript = `set -eu
path=$1
limit=$2
if [ ! -e "$path" ] && [ ! -L "$path" ]; then
	echo "path not found: $path" >&2
	exit 1
fi
exec head -c "$limit" -- "$path"
`

const SandboxFileUploadScript = `set -eu
path=$1
mode=$2
mtime=${3:-}
dir=$(dirname "$path")
mkdir -p "$dir"
if [ -d "$path" ]; then
	echo "destination is a directory: $path" >&2
	exit 1
fi
write_path=$path
symlink_hops=0
while [ -L "$write_path" ]; do
	symlink_hops=$((symlink_hops + 1))
	if [ "$symlink_hops" -gt 40 ]; then
		echo "too many symlinks resolving destination: $path" >&2
		exit 1
	fi
	link_dir=$(dirname "$write_path")
	link_target=$(readlink "$write_path" || true)
	if [ -z "$link_target" ]; then
		echo "failed to resolve symlink destination: $path" >&2
		exit 1
	fi
	case "$link_target" in
		/*) write_path=$link_target ;;
		*) write_path=$link_dir/$link_target ;;
	esac
done
if [ -d "$write_path" ]; then
	echo "destination is a directory: $path" >&2
	exit 1
fi
write_dir=$(dirname "$write_path")
mkdir -p "$write_dir"
tmp="${write_path}.cleanroom-copy.$$"
cleanup() {
	rm -f "$tmp"
}
trap cleanup EXIT HUP INT TERM
cat > "$tmp"
chmod "$mode" "$tmp"
if [ -n "$mtime" ]; then
	if ! touch -m -d "@$mtime" "$tmp" 2>/dev/null; then
		timestamp=$(date -d "@$mtime" +%Y%m%d%H%M.%S 2>/dev/null || date -r "$mtime" +%Y%m%d%H%M.%S 2>/dev/null || true)
		if [ -z "$timestamp" ] || ! touch -m -t "$timestamp" "$tmp" 2>/dev/null; then
			echo "failed to apply mtime $mtime to $path" >&2
			exit 1
		fi
	fi
fi
mv -f "$tmp" "$write_path"
trap - EXIT HUP INT TERM
`

const SandboxFileExtractScript = `set -eu
dest=$1
mkdir -p "$dest"
tar -C "$dest" -xf -
`

func ValidateSandboxFilePath(path string) error {
	if path == "" {
		return errors.New("missing path")
	}
	if !strings.HasPrefix(path, "/") {
		return errors.New("invalid path: must be absolute")
	}
	return nil
}

func ValidateSandboxFilePaths(paths []string) error {
	if len(paths) == 0 {
		return errors.New("missing paths")
	}
	for _, path := range paths {
		if err := ValidateSandboxFilePath(path); err != nil {
			return err
		}
	}
	return nil
}

func SandboxFileDownloadCommand(path string, maxBytes int64) ([]string, error) {
	return SandboxFileReadCommand(path, maxBytes)
}

func SandboxPathStatCommand(path string) ([]string, error) {
	if err := ValidateSandboxFilePath(path); err != nil {
		return nil, err
	}
	return []string{"sh", "-c", SandboxPathInfoScript, "cleanroom-stat", path}, nil
}

func SandboxTreeWalkCommand(path string) ([]string, error) {
	if err := ValidateSandboxFilePath(path); err != nil {
		return nil, err
	}
	return []string{"sh", "-c", SandboxTreeWalkScript, "cleanroom-walk", path}, nil
}

func SandboxFileReadCommand(path string, maxBytes int64) ([]string, error) {
	if err := ValidateSandboxFilePath(path); err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		maxBytes = DefaultSandboxFileTransferMaxBytes
	}

	limit := maxBytes + 1
	if maxBytes == math.MaxInt64 {
		limit = maxBytes
	}
	return []string{"sh", "-c", SandboxFileReadScript, "cleanroom-read", path, strconv.FormatInt(limit, 10)}, nil
}

func SandboxFileUploadCommand(path string, mode fs.FileMode, mtime time.Time) ([]string, error) {
	if err := ValidateSandboxFilePath(path); err != nil {
		return nil, err
	}
	mtimeArg := ""
	if !mtime.IsZero() {
		mtimeArg = strconv.FormatInt(mtime.Unix(), 10)
	}
	return []string{
		"sh",
		"-c",
		SandboxFileUploadScript,
		"cleanroom-copy",
		path,
		fmt.Sprintf("%04o", NormalizeSandboxFileMode(mode).Perm()),
		mtimeArg,
	}, nil
}

// CopyReaderToAttachStdin streams r to the attached guest stdin and always
// sends EOF after the attach succeeds, including when r returns an error.
func CopyReaderToAttachStdin(r io.Reader, attach AttachIO, payloadName string) (written int64, err error) {
	return copyReaderToAttachStdin(r, attach, payloadName, nil)
}

// AttachStdinCopy tracks a background copy into attached guest stdin.
type AttachStdinCopy struct {
	done    chan struct{}
	result  AttachStdinCopyResult
	written atomic.Int64
}

// AttachStdinCopyResult is the terminal result of an attached stdin copy.
type AttachStdinCopyResult struct {
	Written int64
	Err     error
}

// StartCopyReaderToAttachStdin streams r to guest stdin in the background.
func StartCopyReaderToAttachStdin(r io.Reader, attach AttachIO, payloadName string) *AttachStdinCopy {
	copy := &AttachStdinCopy{done: make(chan struct{})}
	go func() {
		copy.result.Written, copy.result.Err = copyReaderToAttachStdin(r, attach, payloadName, copy.written.Store)
		close(copy.done)
	}()
	return copy
}

// Wait waits for the stdin copy to finish and returns its final result.
func (c *AttachStdinCopy) Wait() AttachStdinCopyResult {
	if c == nil {
		return AttachStdinCopyResult{}
	}
	<-c.done
	return c.result
}

// Written returns the latest byte count observed for the stdin copy.
func (c *AttachStdinCopy) Written() int64 {
	if c == nil {
		return 0
	}
	select {
	case <-c.done:
		return c.result.Written
	default:
		return c.written.Load()
	}
}

func copyReaderToAttachStdin(r io.Reader, attach AttachIO, payloadName string, onWritten func(int64)) (written int64, err error) {
	if attach.WriteStdin == nil || attach.CloseStdin == nil {
		return 0, errors.New("stdin attach unavailable")
	}
	defer func() {
		if closeErr := attach.CloseStdin(); closeErr != nil && err == nil {
			err = fmt.Errorf("close %s stdin: %w", payloadName, closeErr)
		}
	}()

	buf := make([]byte, 32*1024)
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			written += int64(n)
			if onWritten != nil {
				onWritten(written)
			}
			if err := attach.WriteStdin(buf[:n]); err != nil {
				return written, fmt.Errorf("write %s payload: %w", payloadName, err)
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return written, fmt.Errorf("read %s payload: %w", payloadName, readErr)
			}
			return written, nil
		}
	}
}

func SandboxPathRemoveCommand(path string, recursive bool) ([]string, error) {
	if err := ValidateSandboxFilePath(path); err != nil {
		return nil, err
	}
	if recursive {
		return []string{"rm", "-rf", "--", path}, nil
	}
	return []string{"rm", "-f", "--", path}, nil
}

func SandboxArchivePathsCommand(paths []string, maxBytes int64) ([]string, error) {
	if err := ValidateSandboxFilePaths(paths); err != nil {
		return nil, err
	}
	cmd := []string{"tar", "-cf", "-", "--"}
	cmd = append(cmd, paths...)
	return cmd, nil
}

func SandboxExtractArchiveCommand(destination string) ([]string, error) {
	if err := ValidateSandboxFilePath(destination); err != nil {
		return nil, err
	}
	return []string{"sh", "-c", SandboxFileExtractScript, "cleanroom-extract", destination}, nil
}

func NormalizeSandboxFileMode(mode fs.FileMode) fs.FileMode {
	mode = mode.Perm()
	if mode == 0 {
		return 0o644
	}
	return mode
}

func (t SandboxFileTransfer) DownloadSandboxFile(ctx context.Context, sandboxID, path string, maxBytes int64) ([]byte, error) {
	var stdout bytes.Buffer
	if err := t.ReadSandboxFile(ctx, sandboxID, path, maxBytes, func(chunk []byte) error {
		_, _ = stdout.Write(chunk)
		return nil
	}); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

func (t SandboxFileTransfer) UploadSandboxFile(ctx context.Context, sandboxID, path string, data []byte, mode fs.FileMode) error {
	_, err := t.WriteSandboxFile(ctx, sandboxID, path, bytes.NewReader(data), mode, time.Time{})
	return err
}

func (t SandboxFileTransfer) StatSandboxPath(ctx context.Context, sandboxID, path string) (*SandboxPathInfo, error) {
	cmd, err := SandboxPathStatCommand(path)
	if err != nil {
		return nil, err
	}

	var stdout, stderr bytes.Buffer
	result, err := t.run(ctx, sandboxID, cmd, OutputStream{
		OnStdout: func(chunk []byte) {
			_, _ = stdout.Write(chunk)
		},
		OnStderr: func(chunk []byte) {
			_, _ = stderr.Write(chunk)
		},
	})
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, sandboxFileTransferExitError(result, stderr.String(), "stat path command failed")
	}
	records, err := ParseSandboxPathInfoRecords(stdout.Bytes())
	if err != nil {
		return nil, err
	}
	if len(records) != 1 {
		return nil, fmt.Errorf("stat path returned %d records", len(records))
	}
	return &records[0], nil
}

func (t SandboxFileTransfer) WalkSandboxTree(ctx context.Context, sandboxID, path string, emit func(SandboxPathInfo) error) error {
	cmd, err := SandboxTreeWalkCommand(path)
	if err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var decoder SandboxPathInfoDecoder
	var emitErr error
	var stderr bytes.Buffer
	result, err := t.run(runCtx, sandboxID, cmd, OutputStream{
		OnStderr: func(chunk []byte) {
			_, _ = stderr.Write(chunk)
		},
		OnStdout: func(chunk []byte) {
			if emitErr != nil {
				return
			}
			records, err := decoder.Write(chunk)
			if err != nil {
				emitErr = err
				cancel()
				return
			}
			for _, record := range records {
				if err := emit(record); err != nil {
					emitErr = err
					cancel()
					return
				}
			}
		},
	})
	if emitErr != nil {
		return emitErr
	}
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return sandboxFileTransferExitError(result, stderr.String(), "walk tree command failed")
	}
	records, err := decoder.Flush()
	if err != nil {
		return err
	}
	for _, record := range records {
		if err := emit(record); err != nil {
			return err
		}
	}
	return nil
}

func (t SandboxFileTransfer) ReadSandboxFile(ctx context.Context, sandboxID, path string, maxBytes int64, emit func([]byte) error) error {
	if maxBytes <= 0 {
		maxBytes = DefaultSandboxFileTransferMaxBytes
	}
	cmd, err := SandboxFileReadCommand(path, maxBytes)
	if err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var total int64
	var emitErr error
	var stderr bytes.Buffer
	result, err := t.run(runCtx, sandboxID, cmd, OutputStream{
		OnStderr: func(chunk []byte) {
			_, _ = stderr.Write(chunk)
		},
		OnStdout: func(chunk []byte) {
			if emitErr != nil {
				return
			}
			if total+int64(len(chunk)) > maxBytes {
				allowed := maxBytes - total
				if allowed > 0 && emit != nil {
					if err := emit(chunk[:allowed]); err != nil {
						emitErr = err
					}
				}
				if emitErr == nil {
					emitErr = fmt.Errorf("file %q exceeds max_bytes=%d", path, maxBytes)
				}
				cancel()
				return
			}
			total += int64(len(chunk))
			if emit != nil {
				if err := emit(chunk); err != nil {
					emitErr = err
					cancel()
				}
			}
		},
	})
	if emitErr != nil {
		return emitErr
	}
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return sandboxFileTransferExitError(result, stderr.String(), "read file command failed")
	}
	return nil
}

func (t SandboxFileTransfer) WriteSandboxFile(ctx context.Context, sandboxID, path string, r io.Reader, mode fs.FileMode, mtime time.Time) (int64, error) {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return 0, errors.New("missing sandbox_id")
	}
	cmd, err := SandboxFileUploadCommand(path, mode, mtime)
	if err != nil {
		return 0, err
	}

	attached := false
	var attachErr error
	var copy *AttachStdinCopy
	var stderr bytes.Buffer
	result, err := t.run(ctx, sandboxID, cmd, OutputStream{
		OnStderr: func(chunk []byte) {
			_, _ = stderr.Write(chunk)
		},
		OnAttach: func(attach AttachIO) {
			attached = true
			if attach.WriteStdin == nil || attach.CloseStdin == nil {
				attachErr = errors.New("sandbox file upload requires stdin attach")
				return
			}
			copy = StartCopyReaderToAttachStdin(r, attach, "file")
		},
	})
	written := copy.Written()
	if err != nil {
		return written, err
	}
	if !attached {
		return written, errors.New("sandbox file write requires stdin attach")
	}
	if result.ExitCode != 0 {
		return written, sandboxFileTransferExitError(result, stderr.String(), "write file command failed")
	}
	if attachErr != nil {
		return written, attachErr
	}
	copyResult := copy.Wait()
	if copyResult.Err != nil {
		return copyResult.Written, copyResult.Err
	}
	written = copyResult.Written
	return written, nil
}

func (t SandboxFileTransfer) RemoveSandboxPath(ctx context.Context, sandboxID, path string, recursive bool) error {
	cmd, err := SandboxPathRemoveCommand(path, recursive)
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	result, err := t.run(ctx, sandboxID, cmd, OutputStream{OnStderr: func(chunk []byte) {
		_, _ = stderr.Write(chunk)
	}})
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return sandboxFileTransferExitError(result, stderr.String(), "remove path command failed")
	}
	return nil
}

func (t SandboxFileTransfer) ArchiveSandboxPaths(ctx context.Context, sandboxID string, paths []string, maxBytes int64, emit func([]byte) error) error {
	if maxBytes <= 0 {
		maxBytes = DefaultSandboxFileTransferMaxBytes
	}
	cmd, err := SandboxArchivePathsCommand(paths, maxBytes)
	if err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var total int64
	var emitErr error
	var stderr bytes.Buffer
	result, err := t.run(runCtx, sandboxID, cmd, OutputStream{
		OnStderr: func(chunk []byte) {
			_, _ = stderr.Write(chunk)
		},
		OnStdout: func(chunk []byte) {
			if emitErr != nil {
				return
			}
			if total+int64(len(chunk)) > maxBytes {
				allowed := maxBytes - total
				if allowed > 0 && emit != nil {
					if err := emit(chunk[:allowed]); err != nil {
						emitErr = err
					}
				}
				if emitErr == nil {
					emitErr = fmt.Errorf("archive exceeds max_bytes=%d", maxBytes)
				}
				cancel()
				return
			}
			total += int64(len(chunk))
			if emit != nil {
				if err := emit(chunk); err != nil {
					emitErr = err
					cancel()
					return
				}
			}
		},
	})
	if emitErr != nil {
		return emitErr
	}
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return sandboxFileTransferExitError(result, stderr.String(), "archive paths command failed")
	}
	return nil
}

func (t SandboxFileTransfer) ExtractSandboxArchive(ctx context.Context, sandboxID, destination string, r io.Reader) (int64, error) {
	cmd, err := SandboxExtractArchiveCommand(destination)
	if err != nil {
		return 0, err
	}

	attached := false
	var attachErr error
	var copy *AttachStdinCopy
	var stderr bytes.Buffer
	result, err := t.run(ctx, sandboxID, cmd, OutputStream{
		OnStderr: func(chunk []byte) {
			_, _ = stderr.Write(chunk)
		},
		OnAttach: func(attach AttachIO) {
			attached = true
			if attach.WriteStdin == nil || attach.CloseStdin == nil {
				attachErr = errors.New("sandbox archive extract requires stdin attach")
				return
			}
			copy = StartCopyReaderToAttachStdin(r, attach, "archive")
		},
	})
	written := copy.Written()
	if err != nil {
		return written, err
	}
	if !attached {
		return written, errors.New("sandbox archive extract requires stdin attach")
	}
	if result.ExitCode != 0 {
		return written, sandboxFileTransferExitError(result, stderr.String(), "extract archive command failed")
	}
	if attachErr != nil {
		return written, attachErr
	}
	copyResult := copy.Wait()
	if copyResult.Err != nil {
		return copyResult.Written, copyResult.Err
	}
	written = copyResult.Written
	return written, nil
}

func (t SandboxFileTransfer) run(ctx context.Context, sandboxID string, cmd []string, stream OutputStream) (*ExecutionResult, error) {
	if t.Run == nil {
		return nil, errors.New("sandbox file transfer runner is nil")
	}
	return t.Run(ctx, sandboxID, cmd, stream)
}

func sandboxFileTransferExitError(result *ExecutionResult, stderr, fallback string) error {
	if result == nil {
		return errors.New(fallback)
	}
	msg := strings.TrimSpace(stderr)
	if pathErr := SandboxPathNotFoundErrorFromStderr(msg); pathErr != nil {
		return pathErr
	}
	if msg == "" {
		msg = strings.TrimSpace(result.Message)
	}
	if msg == "" {
		msg = fallback
	}
	return errors.New(msg)
}

func ParseSandboxPathInfoRecords(data []byte) ([]SandboxPathInfo, error) {
	if len(data) == 0 {
		return nil, nil
	}
	fields := bytes.Split(data, []byte{0})
	if len(fields) > 0 && len(fields[len(fields)-1]) == 0 {
		fields = fields[:len(fields)-1]
	}
	if len(fields)%6 != 0 {
		return nil, fmt.Errorf("invalid path info record: got %d fields", len(fields))
	}
	records := make([]SandboxPathInfo, 0, len(fields)/6)
	for i := 0; i < len(fields); i += 6 {
		size, err := strconv.ParseInt(string(fields[i+2]), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse path info size: %w", err)
		}
		mode, err := strconv.ParseUint(string(fields[i+3]), 8, 32)
		if err != nil {
			return nil, fmt.Errorf("parse path info mode: %w", err)
		}
		mtime, err := strconv.ParseInt(string(fields[i+4]), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse path info mtime: %w", err)
		}
		records = append(records, SandboxPathInfo{
			Path:          string(fields[i]),
			Type:          SandboxPathType(fields[i+1]),
			SizeBytes:     size,
			Mode:          fs.FileMode(mode),
			MTime:         time.Unix(mtime, 0),
			SymlinkTarget: string(fields[i+5]),
		})
	}
	return records, nil
}

type SandboxPathInfoDecoder struct {
	fields  [][]byte
	partial []byte
}

func (d *SandboxPathInfoDecoder) Write(data []byte) ([]SandboxPathInfo, error) {
	if len(data) == 0 {
		return nil, nil
	}
	combined := append(append([]byte(nil), d.partial...), data...)
	parts := bytes.Split(combined, []byte{0})
	if len(parts) == 0 {
		return nil, nil
	}
	for _, part := range parts[:len(parts)-1] {
		field := append([]byte(nil), part...)
		d.fields = append(d.fields, field)
	}
	d.partial = append([]byte(nil), parts[len(parts)-1]...)
	return d.emitComplete()
}

func (d *SandboxPathInfoDecoder) Flush() ([]SandboxPathInfo, error) {
	if len(d.partial) > 0 || len(d.fields) > 0 {
		if len(d.partial) > 0 {
			d.fields = append(d.fields, append([]byte(nil), d.partial...))
			d.partial = nil
		}
		if len(d.fields)%6 != 0 {
			return nil, fmt.Errorf("invalid path info record: got %d fields", len(d.fields))
		}
	}
	return d.emitComplete()
}

func (d *SandboxPathInfoDecoder) emitComplete() ([]SandboxPathInfo, error) {
	completeFields := (len(d.fields) / 6) * 6
	if completeFields == 0 {
		return nil, nil
	}
	records, err := parseSandboxPathInfoFields(d.fields[:completeFields])
	if err != nil {
		return nil, err
	}
	remaining := d.fields[completeFields:]
	d.fields = append(d.fields[:0], remaining...)
	return records, nil
}

func parseSandboxPathInfoFields(fields [][]byte) ([]SandboxPathInfo, error) {
	if len(fields)%6 != 0 {
		return nil, fmt.Errorf("invalid path info record: got %d fields", len(fields))
	}
	records := make([]SandboxPathInfo, 0, len(fields)/6)
	for i := 0; i < len(fields); i += 6 {
		size, err := strconv.ParseInt(string(fields[i+2]), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse path info size: %w", err)
		}
		mode, err := strconv.ParseUint(string(fields[i+3]), 8, 32)
		if err != nil {
			return nil, fmt.Errorf("parse path info mode: %w", err)
		}
		mtime, err := strconv.ParseInt(string(fields[i+4]), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse path info mtime: %w", err)
		}
		records = append(records, SandboxPathInfo{
			Path:          string(fields[i]),
			Type:          SandboxPathType(fields[i+1]),
			SizeBytes:     size,
			Mode:          fs.FileMode(mode),
			MTime:         time.Unix(mtime, 0),
			SymlinkTarget: string(fields[i+5]),
		})
	}
	return records, nil
}
