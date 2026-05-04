//go:build linux

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/vsockexec"
)

func TestScanOverlayCaptureReportsEscapedWrites(t *testing.T) {
	t.Parallel()

	upperDir := t.TempDir()
	writeOverlayCaptureTestFile(t, upperDir, "workspace/result.txt")
	writeOverlayCaptureTestFile(t, upperDir, "etc/profile")

	result, err := scanOverlayCapture(&vsockexec.OverlayCapture{
		UpperDir:            upperDir,
		DeclaredFileOutputs: []string{"/workspace/result.txt"},
	})
	if err != nil {
		t.Fatalf("scanOverlayCapture returned error: %v", err)
	}
	if result == nil {
		t.Fatal("expected overlay capture result")
	}
	assertOverlayCaptureEntry(t, result.EscapedWrites, vsockexec.OverlayCaptureEntry{Path: "/etc/profile", Kind: "write", Mode: 0o644})
}

func TestSendExitResultFailsWhenOverlayCaptureScanFails(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	sendExitResult(newFrameSender(&buf), nil, &vsockexec.OverlayCapture{UpperDir: filepath.Join(t.TempDir(), "missing")})

	res, err := vsockexec.DecodeStreamResponse(&buf, vsockexec.StreamCallbacks{})
	if err != nil {
		t.Fatalf("DecodeStreamResponse returned error: %v", err)
	}
	if got, want := res.ExitCode, 1; got != want {
		t.Fatalf("unexpected exit code: got %d want %d", got, want)
	}
	if !strings.Contains(res.Error, "scan overlay capture") {
		t.Fatalf("unexpected error: %q", res.Error)
	}
}

func TestSendExitResultIncludesOverlayCapture(t *testing.T) {
	t.Parallel()

	upperDir := t.TempDir()
	writeOverlayCaptureTestFile(t, upperDir, "etc/profile")

	var buf bytes.Buffer
	sendExitResult(newFrameSender(&buf), nil, &vsockexec.OverlayCapture{UpperDir: upperDir})

	res, err := vsockexec.DecodeStreamResponse(&buf, vsockexec.StreamCallbacks{})
	if err != nil {
		t.Fatalf("DecodeStreamResponse returned error: %v", err)
	}
	if got, want := res.ExitCode, 0; got != want {
		t.Fatalf("unexpected exit code: got %d want %d", got, want)
	}
	if res.OverlayCapture == nil {
		t.Fatal("expected overlay capture result")
	}
	assertOverlayCaptureEntry(t, res.OverlayCapture.EscapedWrites, vsockexec.OverlayCaptureEntry{Path: "/etc/profile", Kind: "write", Mode: 0o644})
}

func assertOverlayCaptureEntry(t *testing.T, entries []vsockexec.OverlayCaptureEntry, want vsockexec.OverlayCaptureEntry) {
	t.Helper()
	for _, got := range entries {
		if got.Path == want.Path && got.Kind == want.Kind && got.Mode&0o777 == want.Mode&0o777 {
			return
		}
	}
	t.Fatalf("missing overlay capture entry %#v in %#v", want, entries)
}

func writeOverlayCaptureTestFile(t *testing.T, root, rel string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(rel), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}
