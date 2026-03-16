// Package ext4edit mutates ext4 images in place using debugfs.
package ext4edit

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	cleanDst := filepath.Clean(dstPath)
	if !strings.HasPrefix(cleanDst, "/") {
		return fmt.Errorf("destination path %q must be absolute", dstPath)
	}
	if err := ensureDir(imagePath, filepath.Dir(cleanDst)); err != nil {
		return err
	}

	if PathExists(imagePath, cleanDst) {
		_ = runDebugFS(imagePath, true, fmt.Sprintf("rm %s", cleanDst))
	}
	if err := runDebugFS(imagePath, true, fmt.Sprintf("write %s %s", srcPath, cleanDst)); err != nil {
		return err
	}
	modeValue := fmt.Sprintf("%#o", uint32(0o100000)|uint32(mode.Perm()))
	return runDebugFS(imagePath, true, fmt.Sprintf("set_inode_field %s mode %s", cleanDst, modeValue))
}

// PathExists reports whether the given path can be resolved inside the ext4 image.
func PathExists(imagePath, path string) bool {
	return runDebugFS(imagePath, false, fmt.Sprintf("stat %s", path)) == nil
}

// PathType returns the ext4 inode type for the given path.
func PathType(imagePath, path string) PathKind {
	output, err := runDebugFSOutput(imagePath, false, fmt.Sprintf("stat %s", path))
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

func ensureDir(imagePath, dir string) error {
	cleanDir := filepath.Clean(dir)
	if cleanDir == "." || cleanDir == "/" {
		return nil
	}
	if !strings.HasPrefix(cleanDir, "/") {
		cleanDir = "/" + cleanDir
	}
	parts := strings.Split(strings.TrimPrefix(cleanDir, "/"), "/")
	cur := ""
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			continue
		}
		cur += "/" + part
		if PathExists(imagePath, cur) {
			continue
		}
		if err := runDebugFS(imagePath, true, fmt.Sprintf("mkdir %s", cur)); err != nil {
			return err
		}
	}
	return nil
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
