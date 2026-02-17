package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"

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

func errorsIsClosed(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, net.ErrClosed)
}

func listenVsock(port uint32) (net.Listener, error) {
	return vsock.Listen(port, nil)
}
