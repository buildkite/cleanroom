//go:build linux

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/vsockexec"
)

const guestOverlayCaptureE2EEnv = "CLEANROOM_GUEST_AGENT_OVERLAY_E2E"

func TestOverlayCaptureExecutesCommandInMergedRootE2E(t *testing.T) {
	if strings.TrimSpace(os.Getenv(guestOverlayCaptureE2EEnv)) == "" {
		t.Skipf("set %s=1 to run privileged guest overlay capture e2e", guestOverlayCaptureE2EEnv)
	}
	if os.Geteuid() != 0 {
		t.Skip("guest overlay capture e2e requires root")
	}
	if rootFSType := mountFSType("/"); rootFSType == "overlay" {
		t.Skip("guest overlay capture e2e requires a non-overlay root filesystem for lowerdir=/")
	}

	tempDir := t.TempDir()
	restoreOverlayRoot := setTestOverlayCaptureRoot(t, filepath.Join(tempDir, "overlay-captures"))
	defer restoreOverlayRoot()
	outputRoot := filepath.Join(string(filepath.Separator), "mnt", "cleanroom-overlay-e2e")
	declaredFile := filepath.Join(outputRoot, "declared.txt")
	escapedFile := filepath.Join(string(filepath.Separator), "etc", "cleanroom-overlay-e2e-escaped")
	t.Cleanup(func() {
		_ = os.RemoveAll(outputRoot)
		_ = os.Remove(escapedFile)
	})
	if err := os.MkdirAll(outputRoot, 0o755); err != nil {
		t.Fatalf("create output root: %v", err)
	}
	mountPath := filepath.Join(tempDir, "volume")
	if err := os.MkdirAll(mountPath, 0o755); err != nil {
		t.Fatalf("create capture volume: %v", err)
	}

	var req bytes.Buffer
	if err := vsockexec.EncodeRequest(&req, vsockexec.ExecRequest{
		Command: []string{
			"sh",
			"-lc",
			"printf declared > " + shellQuote(declaredFile) + "; printf escaped > " + shellQuote(escapedFile),
		},
		CacheOutputFileCaptures: []vsockexec.CacheOutputFileCapture{
			{GuestPath: declaredFile, MountPath: mountPath, Subpath: "files/declared.txt"},
		},
		OverlayCapture: &vsockexec.OverlayCapture{
			UpperDir:            filepath.Join(overlayCaptureRoot, "e2e", "upper"),
			DeclaredFileOutputs: []string{declaredFile},
			IgnoredPrefixes:     []string{"/tmp", "/var/tmp", "/run"},
		},
	}); err != nil {
		t.Fatalf("encode request: %v", err)
	}
	conn := &guestOverlayCaptureTestConn{Reader: bytes.NewReader(req.Bytes())}
	handleConn(conn)

	res, err := vsockexec.DecodeStreamResponse(bytes.NewReader(conn.output.Bytes()), vsockexec.StreamCallbacks{})
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got, want := res.ExitCode, 0; got != want {
		t.Fatalf("unexpected exit code: got %d want %d error %q", got, want, res.Error)
	}
	if res.OverlayCapture == nil {
		t.Fatal("expected overlay capture result")
	}
	if !slices.ContainsFunc(res.OverlayCapture.EscapedWrites, func(entry vsockexec.OverlayCaptureEntry) bool {
		return entry.Path == escapedFile && entry.Kind == "write"
	}) {
		t.Fatalf("missing escaped write for %s in %#v", escapedFile, res.OverlayCapture.EscapedWrites)
	}
	if data, err := os.ReadFile(filepath.Join(mountPath, "files", "declared.txt")); err != nil {
		t.Fatalf("read captured declared file: %v", err)
	} else if got, want := string(data), "declared"; got != want {
		t.Fatalf("unexpected captured file: got %q want %q", got, want)
	}
	if data, err := os.ReadFile(declaredFile); err != nil {
		t.Fatalf("read materialized declared file: %v", err)
	} else if got, want := string(data), "declared"; got != want {
		t.Fatalf("unexpected materialized file: got %q want %q", got, want)
	}
	if _, err := os.Stat(escapedFile); !os.IsNotExist(err) {
		t.Fatalf("escaped write leaked into base root: %v", err)
	}
}

type guestOverlayCaptureTestConn struct {
	*bytes.Reader
	output bytes.Buffer
}

func (c *guestOverlayCaptureTestConn) Write(p []byte) (int, error) {
	return c.output.Write(p)
}

func (c *guestOverlayCaptureTestConn) Close() error {
	return nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

var _ io.ReadWriteCloser = (*guestOverlayCaptureTestConn)(nil)

func mountFSType(target string) string {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[1] == target {
			return fields[2]
		}
	}
	return ""
}
