package endpoint

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeFileInfo struct {
	mode os.FileMode
}

func (f fakeFileInfo) Name() string       { return "fake" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeFileInfo) Sys() any           { return nil }

func TestResolveRejectsTSNetEndpoint(t *testing.T) {
	t.Parallel()

	_, err := Resolve("tsnet://cleanroomd:8443")
	if err == nil {
		t.Fatal("expected tsnet:// to be rejected")
	}
	if !strings.Contains(err.Error(), "unsupported endpoint") {
		t.Fatalf("expected helpful error message, got %q", err)
	}
}

func TestResolveListenRejectsTSNetEndpoint(t *testing.T) {
	t.Parallel()

	_, err := ResolveListen("tsnet://cleanroomd:8443")
	if err == nil {
		t.Fatal("expected tsnet:// to be rejected for listen resolution")
	}
}

func TestResolveRejectsTailscaleServiceEndpoint(t *testing.T) {
	t.Parallel()

	_, err := Resolve("tssvc://cleanroom:8443")
	if err == nil {
		t.Fatal("expected tssvc:// to be rejected")
	}
}

func TestResolveDefaultClientPrefersSystemSocketWhenPresent(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	prevStat := endpointStat
	prevEUID := endpointGeteuid
	endpointStat = func(string) (os.FileInfo, error) {
		return fakeFileInfo{mode: os.ModeSocket}, nil
	}
	endpointGeteuid = func() int { return 0 }
	t.Cleanup(func() {
		endpointStat = prevStat
		endpointGeteuid = prevEUID
	})

	ep, err := Resolve("")
	if err != nil {
		t.Fatalf("resolve default client endpoint: %v", err)
	}
	if ep.Scheme != "unix" {
		t.Fatalf("expected unix scheme, got %q", ep.Scheme)
	}
	if ep.Address != defaultSystemSocketPath {
		t.Fatalf("expected system socket path %q, got %q", defaultSystemSocketPath, ep.Address)
	}
}

func TestResolveDefaultClientFallsBackToRuntimeSocketWhenSystemPathIsNotSocket(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	prevStat := endpointStat
	prevEUID := endpointGeteuid
	endpointStat = func(string) (os.FileInfo, error) {
		return fakeFileInfo{mode: 0}, nil
	}
	endpointGeteuid = func() int { return 0 }
	t.Cleanup(func() {
		endpointStat = prevStat
		endpointGeteuid = prevEUID
	})

	ep, err := Resolve("")
	if err != nil {
		t.Fatalf("resolve default client endpoint: %v", err)
	}
	want := filepath.Join(runtimeDir, "cleanroom", "cleanroom.sock")
	if ep.Address != want {
		t.Fatalf("expected runtime socket path %q when system path is not a socket, got %q", want, ep.Address)
	}
}

func TestResolveDefaultClientFallsBackToRuntimeSocketWhenSystemSocketMissing(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	prevStat := endpointStat
	prevEUID := endpointGeteuid
	endpointStat = func(string) (os.FileInfo, error) {
		return nil, errors.New("missing")
	}
	endpointGeteuid = func() int { return 0 }
	t.Cleanup(func() {
		endpointStat = prevStat
		endpointGeteuid = prevEUID
	})

	ep, err := Resolve("")
	if err != nil {
		t.Fatalf("resolve default client endpoint: %v", err)
	}
	want := filepath.Join(runtimeDir, "cleanroom", "cleanroom.sock")
	if ep.Address != want {
		t.Fatalf("expected runtime socket path %q, got %q", want, ep.Address)
	}
}

func TestResolveDefaultClientFallsBackToRuntimeSocketWhenNotRoot(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	prevStat := endpointStat
	prevEUID := endpointGeteuid
	endpointStat = func(string) (os.FileInfo, error) {
		return fakeFileInfo{mode: os.ModeSocket}, nil
	}
	endpointGeteuid = func() int { return 1000 }
	t.Cleanup(func() {
		endpointStat = prevStat
		endpointGeteuid = prevEUID
	})

	ep, err := Resolve("")
	if err != nil {
		t.Fatalf("resolve default client endpoint: %v", err)
	}
	want := filepath.Join(runtimeDir, "cleanroom", "cleanroom.sock")
	if ep.Address != want {
		t.Fatalf("expected runtime socket path %q for non-root, got %q", want, ep.Address)
	}
}

func TestResolveListenDefaultUsesRuntimeSocketEvenWhenSystemSocketPresent(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	tmpFile, err := os.CreateTemp(t.TempDir(), "cleanroom-system-sock-*")
	if err != nil {
		t.Fatalf("create temp socket stub: %v", err)
	}
	if err := tmpFile.Close(); err != nil {
		t.Fatalf("close temp socket stub: %v", err)
	}
	fi, err := os.Stat(tmpFile.Name())
	if err != nil {
		t.Fatalf("stat temp socket stub: %v", err)
	}

	prevStat := endpointStat
	endpointStat = func(string) (os.FileInfo, error) {
		return fi, nil
	}
	t.Cleanup(func() { endpointStat = prevStat })

	ep, err := ResolveListen("")
	if err != nil {
		t.Fatalf("resolve default listen endpoint: %v", err)
	}
	want := filepath.Join(runtimeDir, "cleanroom", "cleanroom.sock")
	if ep.Address != want {
		t.Fatalf("expected runtime listen socket path %q, got %q", want, ep.Address)
	}
}
