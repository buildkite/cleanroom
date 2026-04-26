package backend

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"strconv"
	"strings"
	"time"
)

const DefaultSandboxFileTransferMaxBytes int64 = 10 * 1024 * 1024

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

const SandboxFileUploadScript = `set -eu
path=$1
mode=$2
dir=$(dirname "$path")
mkdir -p "$dir"
tmp="${path}.cleanroom-copy.$$"
cleanup() {
	rm -f "$tmp"
}
trap cleanup EXIT HUP INT TERM
cat > "$tmp"
chmod "$mode" "$tmp"
mv -f "$tmp" "$path"
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
	return []string{"head", "-c", strconv.FormatInt(limit, 10), "--", path}, nil
}

func SandboxFileUploadCommand(path string, mode fs.FileMode) ([]string, error) {
	if err := ValidateSandboxFilePath(path); err != nil {
		return nil, err
	}
	return []string{
		"sh",
		"-c",
		SandboxFileUploadScript,
		"cleanroom-copy",
		path,
		fmt.Sprintf("%04o", NormalizeSandboxFileMode(mode).Perm()),
	}, nil
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
	combined := append(d.partial, data...)
	parts := bytes.Split(combined, []byte{0})
	if len(parts) == 0 {
		return nil, nil
	}
	d.partial = append(d.partial[:0], parts[len(parts)-1]...)
	for _, part := range parts[:len(parts)-1] {
		field := append([]byte(nil), part...)
		d.fields = append(d.fields, field)
	}
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
