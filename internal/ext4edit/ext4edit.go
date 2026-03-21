// Package ext4edit mutates ext4 images in place using debugfs.
package ext4edit

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/buildkite/cleanroom/internal/hosttools"
)

type PathKind string

const (
	PathKindUnknown   PathKind = ""
	PathKindDirectory PathKind = "directory"
	PathKindRegular   PathKind = "regular"
	PathKindSymlink   PathKind = "symlink"
)

// InjectFile copies a host file into an ext4 image and sets its final mode.
func InjectFile(imagePath, srcPath, dstPath string, mode os.FileMode) error {
	if err := ensureOwnerWritable(imagePath); err != nil {
		return err
	}

	cleanDst := filepath.Clean(dstPath)
	if !strings.HasPrefix(cleanDst, "/") {
		return fmt.Errorf("destination path %q must be absolute", dstPath)
	}
	resolvedDir, err := ensureDir(imagePath, filepath.Dir(cleanDst))
	if err != nil {
		return err
	}
	resolvedDst := filepath.Join(resolvedDir, filepath.Base(cleanDst))

	if pathExistsDirect(imagePath, resolvedDst) {
		rmCommand, err := debugFSCommand("rm", resolvedDst)
		if err != nil {
			return err
		}
		_ = runDebugFS(imagePath, true, rmCommand)
	}
	writeCommand, err := debugFSCommand("write", srcPath, resolvedDst)
	if err != nil {
		return err
	}
	if err := runDebugFS(imagePath, true, writeCommand); err != nil {
		return err
	}
	modeValue := fmt.Sprintf("%#o", uint32(0o100000)|uint32(mode.Perm()))
	setModeCommand, err := debugFSSetInodeFieldCommand(resolvedDst, "mode", modeValue)
	if err != nil {
		return err
	}
	return runDebugFS(imagePath, true, setModeCommand)
}

// PathExists reports whether the given path can be resolved inside the ext4 image.
func PathExists(imagePath, path string) bool {
	resolvedPath, err := resolvePath(imagePath, path)
	if err != nil {
		return false
	}
	return pathExistsDirect(imagePath, resolvedPath)
}

// PathType returns the ext4 inode type for the given path.
func PathType(imagePath, path string) PathKind {
	resolvedPath, err := resolvePath(imagePath, path)
	if err != nil {
		return PathKindUnknown
	}
	output, err := statPathDirect(imagePath, resolvedPath)
	if err != nil {
		return PathKindUnknown
	}
	return DebugFSStatType(output)
}

// DebugFSCommandOutputError extracts actionable debugfs stderr/stdout error text.
func DebugFSCommandOutputError(output string) string {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return ""
	}
	for _, line := range strings.Split(trimmed, "\n") {
		msg := strings.TrimSpace(line)
		if msg == "" {
			continue
		}
		lower := strings.ToLower(msg)
		if strings.HasPrefix(lower, "debugfs ") {
			// Version banner.
			continue
		}
		if strings.Contains(lower, "file not found") ||
			strings.Contains(lower, "ext2_lookup") ||
			strings.Contains(lower, "command not found") ||
			strings.Contains(lower, "no such file or directory") ||
			strings.Contains(lower, "not a directory") {
			return msg
		}
	}
	return ""
}

// DebugFSStatType parses a debugfs `stat` response into a PathKind.
func DebugFSStatType(output string) PathKind {
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "Type:") {
			continue
		}
		rest := line[strings.Index(line, "Type:")+len("Type:"):]
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return PathKindUnknown
		}
		switch strings.ToLower(fields[0]) {
		case string(PathKindDirectory):
			return PathKindDirectory
		case string(PathKindRegular):
			return PathKindRegular
		case string(PathKindSymlink):
			return PathKindSymlink
		default:
			return PathKindUnknown
		}
	}
	return PathKindUnknown
}

// DebugFSStatLinkTarget parses a debugfs `stat` response for a symlink target.
func DebugFSStatLinkTarget(output string) (string, error) {
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		for _, prefix := range []string{"Fast link dest:", "Long link dest:"} {
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			target := strings.TrimSpace(strings.TrimPrefix(line, prefix))
			if target == "" {
				return "", errors.New("debugfs stat did not include a symlink target")
			}
			if unquoted, err := strconv.Unquote(target); err == nil {
				return unquoted, nil
			}
			return strings.Trim(target, `"`), nil
		}
	}
	return "", errors.New("debugfs stat did not include a symlink target")
}

func ensureDir(imagePath, dir string) (string, error) {
	cleanDir := filepath.Clean(dir)
	if cleanDir == "." || cleanDir == "/" {
		return "/", nil
	}
	if !strings.HasPrefix(cleanDir, "/") {
		cleanDir = "/" + cleanDir
	}
	return resolveDir(imagePath, cleanDir, true)
}

func resolvePath(imagePath, path string) (string, error) {
	cleanPath := filepath.Clean(path)
	if cleanPath == "." || cleanPath == "/" {
		return "/", nil
	}
	if !strings.HasPrefix(cleanPath, "/") {
		cleanPath = "/" + cleanPath
	}
	resolvedDir, err := resolveDir(imagePath, filepath.Dir(cleanPath), false)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedDir, filepath.Base(cleanPath)), nil
}

func resolveDir(imagePath, dir string, createMissing bool) (string, error) {
	cleanDir := filepath.Clean(dir)
	if cleanDir == "." || cleanDir == "/" {
		return "/", nil
	}
	if !strings.HasPrefix(cleanDir, "/") {
		cleanDir = "/" + cleanDir
	}

	cur := "/"
	remaining := strings.Split(strings.TrimPrefix(cleanDir, "/"), "/")
	symlinkDepth := 0

	for len(remaining) > 0 {
		part := remaining[0]
		remaining = remaining[1:]
		if strings.TrimSpace(part) == "" {
			continue
		}

		candidate := filepath.Join(cur, part)
		output, err := statPathDirect(imagePath, candidate)
		if err != nil {
			if createMissing && isMissingPathError(err) {
				command, commandErr := debugFSCommand("mkdir", candidate)
				if commandErr != nil {
					return "", commandErr
				}
				if err := runDebugFS(imagePath, true, command); err != nil {
					return "", err
				}
				cur = candidate
				continue
			}
			return "", err
		}

		switch DebugFSStatType(output) {
		case PathKindDirectory:
			cur = candidate
		case PathKindSymlink:
			target, err := DebugFSStatLinkTarget(output)
			if err != nil {
				return "", fmt.Errorf("resolve symlink %q: %w", candidate, err)
			}
			symlinkDepth++
			if symlinkDepth > 40 {
				return "", fmt.Errorf("resolve symlink %q: too many nested symlinks", candidate)
			}

			resolvedTarget := target
			if !strings.HasPrefix(resolvedTarget, "/") {
				resolvedTarget = filepath.Join(filepath.Dir(candidate), resolvedTarget)
			}
			cur = "/"
			remaining = append(splitExt4Path(resolvedTarget), remaining...)
		default:
			return "", fmt.Errorf("path %q exists but is not a directory", candidate)
		}
	}

	return cur, nil
}

func splitExt4Path(path string) []string {
	cleanPath := filepath.Clean(path)
	if cleanPath == "." || cleanPath == "/" {
		return nil
	}
	if !strings.HasPrefix(cleanPath, "/") {
		cleanPath = "/" + cleanPath
	}
	return strings.Split(strings.TrimPrefix(cleanPath, "/"), "/")
}

func pathExistsDirect(imagePath, path string) bool {
	_, err := statPathDirect(imagePath, path)
	return err == nil
}

func statPathDirect(imagePath, path string) (string, error) {
	command, err := debugFSCommand("stat", path)
	if err != nil {
		return "", err
	}
	return runDebugFSOutput(imagePath, false, command)
}

func isMissingPathError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "file not found") ||
		strings.Contains(lower, "ext2_lookup") ||
		strings.Contains(lower, "no such file or directory")
}

func ensureOwnerWritable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat image %q: %w", path, err)
	}
	mode := info.Mode().Perm()
	if mode&0o200 != 0 {
		return nil
	}
	if err := os.Chmod(path, mode|0o200); err != nil {
		return fmt.Errorf("make image %q owner-writable: %w", path, err)
	}
	return nil
}

func debugFSCommand(name string, args ...string) (string, error) {
	quotedArgs := make([]string, 0, len(args))
	for _, arg := range args {
		quoted, err := debugFSQuoteArg(arg)
		if err != nil {
			return "", err
		}
		quotedArgs = append(quotedArgs, quoted)
	}
	return strings.Join(append([]string{name}, quotedArgs...), " "), nil
}

func debugFSSetInodeFieldCommand(path, field, value string) (string, error) {
	quotedPath, err := debugFSQuoteArg(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("set_inode_field %s %s %s", quotedPath, field, value), nil
}

func debugFSQuoteArg(arg string) (string, error) {
	if strings.ContainsAny(arg, "\x00\r\n") {
		return "", fmt.Errorf("debugfs argument %q contains an unsupported control character", arg)
	}
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(arg)
	return `"` + escaped + `"`, nil
}

func runDebugFS(imagePath string, writable bool, command string) error {
	_, err := runDebugFSOutput(imagePath, writable, command)
	return err
}

func runDebugFSOutput(imagePath string, writable bool, command string) (string, error) {
	debugfsBinary, err := hosttools.ResolveE2FSProgsBinary("debugfs")
	if err != nil {
		return "", fmt.Errorf("find debugfs for runtime rootfs preparation: %w", err)
	}

	args := make([]string, 0, 4)
	if writable {
		args = append(args, "-w")
	}
	args = append(args, "-R", command, imagePath)
	cmd := exec.Command(debugfsBinary, args...)
	output, err := cmd.CombinedOutput()
	trimmedOutput := strings.TrimSpace(string(output))
	if err != nil {
		return "", fmt.Errorf("debugfs command %q failed: %w: %s", command, err, trimmedOutput)
	}
	if outputErr := DebugFSCommandOutputError(trimmedOutput); outputErr != "" {
		return "", fmt.Errorf("debugfs command %q reported error: %s", command, outputErr)
	}
	return trimmedOutput, nil
}
