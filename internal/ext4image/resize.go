package ext4image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/buildkite/cleanroom/internal/hosttools"
)

const imageSizeAlignBytes int64 = 4 << 20

var (
	resolveE2FSProgsBinary = hosttools.ResolveE2FSProgsBinary
	runCommand             = runCommandWithAllowedExitCodes
	statPath               = os.Stat
	truncateFile           = os.Truncate
	evalSymlinks           = filepath.EvalSymlinks
	readFile               = os.ReadFile
)

func EnsureMinimumSize(ctx context.Context, imagePath string, minimumBytes int64) error {
	if minimumBytes <= 0 {
		return nil
	}

	currentBytes, isBlockDevice, err := pathSizeBytes(imagePath)
	if err != nil {
		return err
	}
	if currentBytes >= minimumBytes && !isBlockDevice {
		return nil
	}
	if isBlockDevice && currentBytes < minimumBytes {
		return fmt.Errorf("block device %q is %d bytes, below requested minimum %d bytes", imagePath, currentBytes, minimumBytes)
	}

	targetBytes := alignBytes(minimumBytes)

	e2fsckBinary, err := resolveE2FSProgsBinary("e2fsck")
	if err != nil {
		return fmt.Errorf("find e2fsck for rootfs resize: %w", err)
	}
	resize2fsBinary, err := resolveE2FSProgsBinary("resize2fs")
	if err != nil {
		return fmt.Errorf("find resize2fs for rootfs resize: %w", err)
	}

	if err := runCommand(ctx, e2fsckBinary, []string{"-fy", imagePath}, 0, 1); err != nil {
		return fmt.Errorf("prepare ext4 image %q for resize: %w", imagePath, err)
	}
	if !isBlockDevice {
		if err := truncateFile(imagePath, targetBytes); err != nil {
			return fmt.Errorf("truncate ext4 image %q to %d bytes: %w", imagePath, targetBytes, err)
		}
	}
	if err := runCommand(ctx, resize2fsBinary, []string{imagePath}, 0); err != nil {
		return fmt.Errorf("grow ext4 filesystem in %q: %w", imagePath, err)
	}
	return nil
}

func pathSizeBytes(imagePath string) (int64, bool, error) {
	info, err := statPath(imagePath)
	if err != nil {
		return 0, false, fmt.Errorf("stat ext4 image %q: %w", imagePath, err)
	}

	mode := info.Mode()
	isBlockDevice := mode&os.ModeDevice != 0 && mode&os.ModeCharDevice == 0
	if !isBlockDevice {
		return info.Size(), false, nil
	}

	sizeBytes, err := blockDeviceSizeBytes(imagePath)
	if err != nil {
		return 0, true, err
	}
	return sizeBytes, true, nil
}

func blockDeviceSizeBytes(imagePath string) (int64, error) {
	resolvedPath, err := evalSymlinks(imagePath)
	if err != nil {
		return 0, fmt.Errorf("resolve block device %q: %w", imagePath, err)
	}

	sectorsPath := filepath.Join("/sys/class/block", filepath.Base(resolvedPath), "size")
	data, err := readFile(sectorsPath)
	if err != nil {
		return 0, fmt.Errorf("read block device size for %q: %w", imagePath, err)
	}

	sectors, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse block device size for %q: %w", imagePath, err)
	}
	return sectors * 512, nil
}

func alignBytes(size int64) int64 {
	if size <= 0 {
		return imageSizeAlignBytes
	}
	remainder := size % imageSizeAlignBytes
	if remainder == 0 {
		return size
	}
	return size + (imageSizeAlignBytes - remainder)
}

func runCommandWithAllowedExitCodes(ctx context.Context, binary string, args []string, allowedExitCodes ...int) error {
	cmd := exec.CommandContext(ctx, binary, args...)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		for _, code := range allowedExitCodes {
			if exitErr.ExitCode() == code {
				return nil
			}
		}
	}

	trimmedOutput := strings.TrimSpace(string(output))
	if trimmedOutput == "" {
		return fmt.Errorf("run %s %v: %w", binary, args, err)
	}
	return fmt.Errorf("run %s %v: %w: %s", binary, args, err, trimmedOutput)
}
