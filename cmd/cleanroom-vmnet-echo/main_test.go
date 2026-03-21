package main

import (
	"bytes"
	"io"
	"net"
	"strings"
	"testing"
)

func TestServeWritesConfiguredResponse(t *testing.T) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen returned error: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- serve(listener, "pong\n")
	}()

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial returned error: %v", err)
	}
	defer conn.Close()

	body, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("ReadAll returned error: %v", err)
	}
	if got, want := string(body), "pong\n"; got != want {
		t.Fatalf("unexpected response body: got %q want %q", got, want)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("serve returned error: %v", err)
	}
}

func TestRunRejectsUnknownFlags(t *testing.T) {
	t.Helper()

	var stderr bytes.Buffer
	if got, want := run([]string{"--unknown-flag"}, &stderr), 2; got != want {
		t.Fatalf("unexpected exit code: got %d want %d", got, want)
	}
	if !strings.Contains(stderr.String(), "flag provided but not defined") {
		t.Fatalf("expected flag parse error, got %q", stderr.String())
	}
}

func TestRunReportsListenFailures(t *testing.T) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen returned error: %v", err)
	}
	defer listener.Close()

	var stderr bytes.Buffer
	if got, want := run([]string{"--listen", listener.Addr().String()}, &stderr), 1; got != want {
		t.Fatalf("unexpected exit code: got %d want %d", got, want)
	}
	if !strings.Contains(stderr.String(), "listen") {
		t.Fatalf("expected listen error in stderr, got %q", stderr.String())
	}
}
