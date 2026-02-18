package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/buildkite/cleanroom/internal/vsockexec"
	"github.com/mdlayher/vsock"
)

func main() {
	port := vsockexec.DefaultPort
	if raw := os.Getenv("CLEANROOM_VSOCK_PORT"); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 32)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid CLEANROOM_VSOCK_PORT %q: %v\n", raw, err)
			os.Exit(2)
		}
		port = uint32(parsed)
	}

	ln, err := listenVsock(port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen vsock: %v\n", err)
		os.Exit(1)
	}
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if errorsIsClosed(err) {
				return
			}
			fmt.Fprintf(os.Stderr, "accept: %v\n", err)
			continue
		}
		handleConn(conn)
	}
}

func handleConn(conn net.Conn) {
	defer conn.Close()

	req, err := vsockexec.DecodeRequest(conn)
	if err != nil {
		_ = vsockexec.EncodeResponse(conn, vsockexec.ExecResponse{ExitCode: 1, Error: err.Error()})
		return
	}
	if len(req.WorkspaceTarGz) > 0 {
		if err := materializeWorkspace(req.WorkspaceTarGz, "/workspace", req.WorkspaceAccess); err != nil {
			_ = vsockexec.EncodeResponse(conn, vsockexec.ExecResponse{ExitCode: 1, Error: err.Error()})
			return
		}
		if req.Dir == "" {
			req.Dir = "/workspace"
		}
	}

	cmd := exec.Command(req.Command[0], req.Command[1:]...)
	if req.Dir != "" {
		cmd.Dir = req.Dir
	}
	if len(req.Env) > 0 {
		cmd.Env = req.Env
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = vsockexec.EncodeResponse(conn, vsockexec.ExecResponse{ExitCode: 1, Error: err.Error()})
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = vsockexec.EncodeResponse(conn, vsockexec.ExecResponse{ExitCode: 1, Error: err.Error()})
		return
	}

	if err := cmd.Start(); err != nil {
		_ = vsockexec.EncodeResponse(conn, vsockexec.ExecResponse{ExitCode: 1, Error: err.Error()})
		return
	}

	outBytes, _ := io.ReadAll(stdout)
	errBytes, _ := io.ReadAll(stderr)
	waitErr := cmd.Wait()

	res := vsockexec.ExecResponse{
		Stdout: string(outBytes),
		Stderr: string(errBytes),
	}
	if waitErr == nil {
		res.ExitCode = 0
	} else if exitErr, ok := waitErr.(*exec.ExitError); ok {
		res.ExitCode = exitErr.ExitCode()
	} else {
		res.ExitCode = 1
		res.Error = waitErr.Error()
	}

	_ = vsockexec.EncodeResponse(conn, res)
}

func materializeWorkspace(tarGz []byte, destRoot, access string) error {
	if err := os.RemoveAll(destRoot); err != nil {
		return fmt.Errorf("reset workspace root: %w", err)
	}
	if err := os.MkdirAll(destRoot, 0o755); err != nil {
		return fmt.Errorf("create workspace root: %w", err)
	}
	if err := extractTarGz(tarGz, destRoot); err != nil {
		return fmt.Errorf("extract workspace: %w", err)
	}
	if strings.EqualFold(strings.TrimSpace(access), "ro") {
		if err := makeTreeReadOnly(destRoot); err != nil {
			return fmt.Errorf("mark workspace read-only: %w", err)
		}
	}
	return nil
}

func extractTarGz(content []byte, destRoot string) error {
	gr, err := gzip.NewReader(bytes.NewReader(content))
	if err != nil {
		return err
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	root := filepath.Clean(destRoot)
	prefix := root + string(os.PathSeparator)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(filepath.Clean("/"+hdr.Name), "/")
		if name == "." || name == "" {
			continue
		}
		target := filepath.Join(root, name)
		cleanTarget := filepath.Clean(target)
		if cleanTarget != root && !strings.HasPrefix(cleanTarget, prefix) {
			return fmt.Errorf("invalid archive path %q", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(cleanTarget, fs.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(cleanTarget), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(cleanTarget, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fs.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(hdr.Linkname) {
				return fmt.Errorf("absolute symlink target not supported: %q", hdr.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(cleanTarget), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, cleanTarget); err != nil {
				return err
			}
		default:
			// Skip unsupported entries (devices, fifos, etc.) in MVP.
		}
	}
}

func makeTreeReadOnly(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm()
		if d.IsDir() {
			return os.Chmod(path, mode&^0o222|0o555)
		}
		return os.Chmod(path, mode&^0o222|0o444)
	})
}

func errorsIsClosed(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, net.ErrClosed)
}

func listenVsock(port uint32) (net.Listener, error) {
	return vsock.Listen(port, nil)
}
