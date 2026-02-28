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

func TestResolveTSNetEndpointRejectedByResolve(t *testing.T) {
	t.Parallel()

	_, err := Resolve("tsnet://cleanroomd:8443")
	if err == nil {
		t.Fatal("expected tsnet:// to be rejected by Resolve (client-side)")
	}
	if !strings.Contains(err.Error(), "server --listen") {
		t.Fatalf("expected helpful error message, got %q", err)
	}
}

func TestResolveTSNetEndpointViaResolveListen(t *testing.T) {
	t.Parallel()

	ep, err := ResolveListen("tsnet://cleanroomd:8443")
	if err != nil {
		t.Fatalf("resolve tsnet endpoint: %v", err)
	}

	if ep.Scheme != "tsnet" {
		t.Fatalf("expected tsnet scheme, got %q", ep.Scheme)
	}
	if ep.Address != ":8443" {
		t.Fatalf("expected listen address :8443, got %q", ep.Address)
	}
	if ep.BaseURL != "http://cleanroomd:8443" {
		t.Fatalf("expected base url http://cleanroomd:8443, got %q", ep.BaseURL)
	}
	if ep.TSNetHostname != "cleanroomd" {
		t.Fatalf("expected hostname cleanroomd, got %q", ep.TSNetHostname)
	}
	if ep.TSNetPort != 8443 {
		t.Fatalf("expected port 8443, got %d", ep.TSNetPort)
	}
}

func TestResolveTSNetEndpointDefaults(t *testing.T) {
	t.Parallel()

	ep, err := ResolveListen("tsnet://")
	if err != nil {
		t.Fatalf("resolve tsnet endpoint with defaults: %v", err)
	}

	if ep.Address != ":7777" {
		t.Fatalf("expected default listen address :7777, got %q", ep.Address)
	}
	if ep.BaseURL != "http://cleanroom:7777" {
		t.Fatalf("expected default base url http://cleanroom:7777, got %q", ep.BaseURL)
	}
	if ep.TSNetHostname != "cleanroom" {
		t.Fatalf("expected default hostname cleanroom, got %q", ep.TSNetHostname)
	}
	if ep.TSNetPort != 7777 {
		t.Fatalf("expected default port 7777, got %d", ep.TSNetPort)
	}
}

func TestResolveTSNetEndpointRejectsInvalidPort(t *testing.T) {
	t.Parallel()

	if _, err := ResolveListen("tsnet://cleanroomd:99999"); err == nil {
		t.Fatal("expected invalid tsnet port to fail")
	}
}

func TestResolveTSNetEndpointRejectsPath(t *testing.T) {
	t.Parallel()

	if _, err := ResolveListen("tsnet://cleanroomd:8443/path"); err == nil {
		t.Fatal("expected tsnet endpoint with path to fail")
	}
}

func TestResolveTailscaleServiceEndpoint(t *testing.T) {
	t.Parallel()

	ep, err := Resolve("tssvc://cleanroom:8443")
	if err != nil {
		t.Fatalf("resolve tssvc endpoint: %v", err)
	}

	if ep.Scheme != "tssvc" {
		t.Fatalf("expected tssvc scheme, got %q", ep.Scheme)
	}
	if ep.Address != "127.0.0.1:8443" {
		t.Fatalf("expected local listen address 127.0.0.1:8443, got %q", ep.Address)
	}
	if ep.BaseURL != "https://cleanroom.<tailnet>.ts.net" {
		t.Fatalf("expected base url https://cleanroom.<tailnet>.ts.net, got %q", ep.BaseURL)
	}
	if ep.TSServiceName != "svc:cleanroom" {
		t.Fatalf("expected service name svc:cleanroom, got %q", ep.TSServiceName)
	}
	if ep.TSServicePort != 8443 {
		t.Fatalf("expected service port 8443, got %d", ep.TSServicePort)
	}
}

func TestResolveTailscaleServiceEndpointDefaults(t *testing.T) {
	t.Parallel()

	ep, err := Resolve("tssvc://")
	if err != nil {
		t.Fatalf("resolve tssvc endpoint with defaults: %v", err)
	}

	if ep.Address != "127.0.0.1:7777" {
		t.Fatalf("expected default local listen address 127.0.0.1:7777, got %q", ep.Address)
	}
	if ep.TSServiceName != "svc:cleanroom" {
		t.Fatalf("expected default service name svc:cleanroom, got %q", ep.TSServiceName)
	}
}

func TestResolveTailscaleServiceEndpointRejectsInvalidLabel(t *testing.T) {
	t.Parallel()

	if _, err := Resolve("tssvc://clean_room:8443"); err == nil {
		t.Fatal("expected invalid tssvc service label to fail")
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
	prevEUID := endpointGeteuid
	endpointStat = func(string) (os.FileInfo, error) {
		return fi, nil
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
