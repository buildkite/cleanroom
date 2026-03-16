package ext4image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/buildkite/cleanroom/internal/hosttools"
)

const imageSizeAlignBytes int64 = 4 << 20

var (
	resolveE2FSProgsBinary = hosttools.ResolveE2FSProgsBinary
	runCommand             = runCommandWithAllowedExitCodes
	truncateFile           = os.Truncate
)

func EnsureMinimumSize(ctx context.Context, imagePath string, minimumBytes int64) error {
	if minimumBytes <= 0 {
		return nil
	}

	info, err := os.Stat(imagePath)
	if err != nil {
		return fmt.Errorf("stat ext4 image %q: %w", imagePath, err)
	}
	if info.Size() >= minimumBytes {
		return nil
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
	if err := truncateFile(imagePath, targetBytes); err != nil {
		return fmt.Errorf("truncate ext4 image %q to %d bytes: %w", imagePath, targetBytes, err)
	}
	if err := runCommand(ctx, resize2fsBinary, []string{imagePath}, 0); err != nil {
		return fmt.Errorf("grow ext4 filesystem in %q: %w", imagePath, err)
	}
	return nil
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
