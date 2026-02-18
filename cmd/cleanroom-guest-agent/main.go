package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/binary"
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
	"sync"
	"unsafe"

	"github.com/buildkite/cleanroom/internal/vsockexec"
	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"
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
	if len(req.EntropySeed) > 0 {
		_ = injectEntropy(req.EntropySeed)
	}

	cmd := exec.Command(req.Command[0], req.Command[1:]...)
	if req.Dir != "" {
		cmd.Dir = req.Dir
	}
	cmd.Env = buildCommandEnv(req.Env)

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

	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	copyErrCh := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := io.Copy(&stdoutBuf, stdout)
		copyErrCh <- err
	}()
	go func() {
		defer wg.Done()
		_, err := io.Copy(&stderrBuf, stderr)
		copyErrCh <- err
	}()

	waitErr := cmd.Wait()
	wg.Wait()
	close(copyErrCh)
	for copyErr := range copyErrCh {
		if copyErr != nil && waitErr == nil {
			waitErr = copyErr
		}
	}

	res := vsockexec.ExecResponse{
		Stdout: stdoutBuf.String(),
		Stderr: stderrBuf.String(),
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

func buildCommandEnv(requestEnv []string) []string {
	// Start from the current process environment so caller-provided values can
	// override, while ensuring we have baseline HOME/PATH defaults for lookups.
	base := map[string]string{}
	for _, entry := range os.Environ() {
		parts := strings.SplitN(entry, "=", 2)
		key := parts[0]
		value := ""
		if len(parts) == 2 {
			value = parts[1]
		}
		base[key] = value
	}

	for _, entry := range requestEnv {
		parts := strings.SplitN(entry, "=", 2)
		key := parts[0]
		value := ""
		if len(parts) == 2 {
			value = parts[1]
		}
		base[key] = value
	}

	if strings.TrimSpace(base["HOME"]) == "" {
		base["HOME"] = "/root"
	}
	if strings.TrimSpace(base["PATH"]) == "" {
		base["PATH"] = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/root/.local/bin"
	}

	out := make([]string, 0, len(base))
	for key, value := range base {
		out = append(out, key+"="+value)
	}
	return out
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
			return fmt.Errorf("symlinks are not supported in workspace copy mode: %q", hdr.Name)
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

func injectEntropy(seed []byte) error {
	if len(seed) == 0 {
		return nil
	}

	// Best effort fallback: mix seed into urandom even if entropy credit ioctl is unavailable.
	if err := os.WriteFile("/dev/urandom", seed, 0o000); err != nil {
		_ = err
	}

	f, err := os.OpenFile("/dev/random", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	// Linux rand_pool_info:
	// struct rand_pool_info { int entropy_count; int buf_size; __u32 buf[0]; };
	payload := make([]byte, 8+len(seed))
	binary.LittleEndian.PutUint32(payload[0:4], uint32(len(seed)*8))
	binary.LittleEndian.PutUint32(payload[4:8], uint32(len(seed)))
	copy(payload[8:], seed)

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), uintptr(unix.RNDADDENTROPY), uintptr(unsafe.Pointer(&payload[0])))
	if errno != 0 {
		return errno
	}
	return nil
}
