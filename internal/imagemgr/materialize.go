package imagemgr

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	minimumRootFSSizeBytes = 512 << 20
	rootFSHeadroomBytes    = 128 << 20
	rootFSAlignBytes       = 4 << 20
)

func materializeExt4(ctx context.Context, mkfsBinary string, tarStream io.Reader, outputPath string) (int64, error) {
	workDir, err := os.MkdirTemp("", "cleanroom-image-materialize-*")
	if err != nil {
		return 0, fmt.Errorf("create temporary rootfs materialisation directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	rootFSDir := filepath.Join(workDir, "rootfs")
	if err := os.MkdirAll(rootFSDir, 0o755); err != nil {
		return 0, fmt.Errorf("create temporary rootfs extraction directory: %w", err)
	}
	if err := extractTar(rootFSDir, tarStream); err != nil {
		return 0, err
	}

	for _, requiredDir := range []string{"dev", "proc", "run", "sys", "tmp"} {
		if err := os.MkdirAll(filepath.Join(rootFSDir, requiredDir), 0o755); err != nil {
			return 0, fmt.Errorf("prepare rootfs directory %q: %w", requiredDir, err)
		}
	}

	contentBytes, err := dirSize(rootFSDir)
	if err != nil {
		return 0, fmt.Errorf("calculate extracted rootfs size: %w", err)
	}
	targetSize := computeRootFSImageSize(contentBytes)

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return 0, fmt.Errorf("create output directory for %q: %w", outputPath, err)
	}

	f, err := os.OpenFile(outputPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, fmt.Errorf("create rootfs output file %q: %w", outputPath, err)
	}
	if err := f.Truncate(targetSize); err != nil {
		_ = f.Close()
		return 0, fmt.Errorf("truncate rootfs output %q to %d bytes: %w", outputPath, targetSize, err)
	}
	if err := f.Close(); err != nil {
		return 0, fmt.Errorf("close rootfs output file %q: %w", outputPath, err)
	}

	cmd := exec.CommandContext(ctx, mkfsBinary, "-F", "-d", rootFSDir, outputPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("run %s for %q: %w: %s", mkfsBinary, outputPath, err, strings.TrimSpace(string(output)))
	}

	info, err := os.Stat(outputPath)
	if err != nil {
		return 0, fmt.Errorf("stat materialised rootfs image %q: %w", outputPath, err)
	}
	return info.Size(), nil
}

func computeRootFSImageSize(contentBytes int64) int64 {
	target := contentBytes + (contentBytes / 2) + rootFSHeadroomBytes
	if target < minimumRootFSSizeBytes {
		target = minimumRootFSSizeBytes
	}
	remainder := target % rootFSAlignBytes
	if remainder == 0 {
		return target
	}
	return target + (rootFSAlignBytes - remainder)
}

func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

func extractTar(root string, stream io.Reader) error {
	tr := tar.NewReader(stream)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read rootfs tar stream: %w", err)
		}

		targetPath, err := safeJoin(root, hdr.Name)
		if err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := ensureNoSymlinkPath(root, targetPath, true); err != nil {
				return err
			}
			if err := os.MkdirAll(targetPath, os.FileMode(hdr.Mode)); err != nil {
				return fmt.Errorf("create directory %q from tar stream: %w", targetPath, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := ensureNoSymlinkPath(root, filepath.Dir(targetPath), true); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
				return fmt.Errorf("create parent directory for %q: %w", targetPath, err)
			}
			if err := ensureNoSymlinkPath(root, targetPath, true); err != nil {
				return err
			}
			f, err := os.OpenFile(targetPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return fmt.Errorf("create file %q from tar stream: %w", targetPath, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return fmt.Errorf("write file %q from tar stream: %w", targetPath, err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("close file %q from tar stream: %w", targetPath, err)
			}
		case tar.TypeSymlink:
			if err := ensureNoSymlinkPath(root, filepath.Dir(targetPath), true); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
				return fmt.Errorf("create parent directory for symlink %q: %w", targetPath, err)
			}
			if err := validateSymlinkTarget(root, targetPath, hdr.Linkname); err != nil {
				return err
			}
			if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove existing symlink path %q: %w", targetPath, err)
			}
			if err := os.Symlink(hdr.Linkname, targetPath); err != nil && !os.IsExist(err) {
				return fmt.Errorf("create symlink %q -> %q from tar stream: %w", targetPath, hdr.Linkname, err)
			}
		case tar.TypeLink:
			linkTarget, err := safeJoin(root, hdr.Linkname)
			if err != nil {
				return err
			}
			if err := ensureNoSymlinkPath(root, linkTarget, false); err != nil {
				return err
			}
			if err := ensureNoSymlinkPath(root, filepath.Dir(targetPath), true); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
				return fmt.Errorf("create parent directory for hard link %q: %w", targetPath, err)
			}
			if err := ensureNoSymlinkPath(root, targetPath, true); err != nil {
				return err
			}
			if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove existing hardlink path %q: %w", targetPath, err)
			}
			if err := os.Link(linkTarget, targetPath); err != nil {
				return fmt.Errorf("create hard link %q -> %q from tar stream: %w", targetPath, linkTarget, err)
			}
		default:
			// Ignore unsupported tar entry kinds (for example device nodes); /dev is mounted at boot.
		}
	}
}

func safeJoin(root, name string) (string, error) {
	clean := filepath.Clean(name)
	if clean == "." {
		return root, nil
	}
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("refusing tar entry with unsafe path %q", name)
	}
	joined := filepath.Join(root, clean)
	rootPrefix := root + string(filepath.Separator)
	if joined != root && !strings.HasPrefix(joined, rootPrefix) {
		return "", fmt.Errorf("refusing tar entry outside root %q", name)
	}
	return joined, nil
}

func ensureNoSymlinkPath(root, target string, allowMissingLeaf bool) error {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return fmt.Errorf("resolve relative path for %q: %w", target, err)
	}
	if rel == "." {
		return nil
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("refusing path outside extraction root %q", target)
	}

	parts := strings.Split(rel, string(filepath.Separator))
	current := root
	for i, part := range parts {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				if i == len(parts)-1 && allowMissingLeaf {
					return nil
				}
				continue
			}
			return fmt.Errorf("inspect path %q: %w", current, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing archive entry that traverses symlink path component %q", current)
		}
	}
	return nil
}

func validateSymlinkTarget(root, targetPath, linkName string) error {
	if strings.TrimSpace(linkName) == "" {
		return fmt.Errorf("refusing symlink %q with empty target", targetPath)
	}
	var resolved string
	if filepath.IsAbs(linkName) {
		// OCI rootfs archives commonly use absolute symlink targets (for example /var/run -> /run).
		// Treat those as absolute within the extraction root, not host-absolute paths.
		trimmed := strings.TrimPrefix(filepath.Clean(linkName), string(filepath.Separator))
		resolved = filepath.Join(root, trimmed)
	} else {
		resolved = filepath.Clean(filepath.Join(filepath.Dir(targetPath), linkName))
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return fmt.Errorf("resolve symlink target for %q: %w", targetPath, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("refusing symlink %q -> %q that resolves outside extraction root", targetPath, linkName)
	}
	return nil
}
