package backend

import (
	"context"
	"io"
	"io/fs"
	"testing"
	"time"
)

type testAdapter struct{}

func (testAdapter) Name() string { return "test" }

func (testAdapter) ProvisionSandbox(context.Context, ProvisionRequest) error { return nil }

func (testAdapter) RunInSandbox(context.Context, ExecutionRequest, OutputStream) (*ExecutionResult, error) {
	return &ExecutionResult{}, nil
}

func (testAdapter) TerminateSandbox(context.Context, string) error { return nil }

type testPersistentAdapter struct{ testAdapter }

func (testPersistentAdapter) DownloadSandboxFile(context.Context, string, string, int64) ([]byte, error) {
	return []byte("ok"), nil
}

func (testPersistentAdapter) UploadSandboxFile(context.Context, string, string, []byte, fs.FileMode) error {
	return nil
}

func (testPersistentAdapter) StatSandboxPath(context.Context, string, string) (*SandboxPathInfo, error) {
	return &SandboxPathInfo{}, nil
}

func (testPersistentAdapter) WalkSandboxTree(context.Context, string, string, func(SandboxPathInfo) error) error {
	return nil
}

func (testPersistentAdapter) ReadSandboxFile(context.Context, string, string, int64, func([]byte) error) error {
	return nil
}

func (testPersistentAdapter) WriteSandboxFile(context.Context, string, string, io.Reader, fs.FileMode, time.Time) (int64, error) {
	return 0, nil
}

func (testPersistentAdapter) RemoveSandboxPath(context.Context, string, string, bool) error {
	return nil
}

func (testPersistentAdapter) ArchiveSandboxPaths(context.Context, string, []string, int64, func([]byte) error) error {
	return nil
}

func (testPersistentAdapter) ExtractSandboxArchive(context.Context, string, string, io.Reader) (int64, error) {
	return 0, nil
}

type testReporterAdapter struct{ testAdapter }

func (testReporterAdapter) Capabilities() map[string]bool {
	return map[string]bool{
		CapabilityNetworkDefaultDeny:     true,
		CapabilityNetworkAllowlistEgress: false,
		"custom.example":                 true,
	}
}

func TestCapabilitiesForAdapterInfersInterfaceCapabilities(t *testing.T) {
	caps := CapabilitiesForAdapter(testPersistentAdapter{})

	if !caps[CapabilityExecStreaming] {
		t.Fatalf("expected %s=true", CapabilityExecStreaming)
	}
	if !caps[CapabilitySandboxFileDownload] {
		t.Fatalf("expected %s=true", CapabilitySandboxFileDownload)
	}
	if !caps[CapabilitySandboxFileUpload] {
		t.Fatalf("expected %s=true", CapabilitySandboxFileUpload)
	}
	for _, key := range []string{
		CapabilitySandboxPathStat,
		CapabilitySandboxTreeWalk,
		CapabilitySandboxFileRead,
		CapabilitySandboxFileWrite,
		CapabilitySandboxPathRemove,
		CapabilitySandboxArchiveRead,
		CapabilitySandboxArchiveWrite,
	} {
		if !caps[key] {
			t.Fatalf("expected %s=true", key)
		}
	}
	for _, key := range []string{
		CapabilitySandboxCacheOutputVolumes,
		CapabilitySandboxCacheOutputFastClone,
		CapabilitySandboxOverlayWriteCapture,
	} {
		if caps[key] {
			t.Fatalf("expected %s=false without backend reporter", key)
		}
	}
}

func TestCapabilitiesForAdapterMergesReporterCapabilities(t *testing.T) {
	caps := CapabilitiesForAdapter(testReporterAdapter{})

	if !caps[CapabilityNetworkDefaultDeny] {
		t.Fatalf("expected %s=true", CapabilityNetworkDefaultDeny)
	}
	if caps[CapabilityNetworkAllowlistEgress] {
		t.Fatalf("expected %s=false", CapabilityNetworkAllowlistEgress)
	}
	if !caps["custom.example"] {
		t.Fatalf("expected custom capability key to be preserved")
	}
}
