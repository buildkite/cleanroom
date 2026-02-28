package backend

import (
	"context"
	"testing"
)

type testAdapter struct{}

func (testAdapter) Name() string { return "test" }

func (testAdapter) Run(context.Context, RunRequest) (*RunResult, error) {
	return &RunResult{}, nil
}

type testStreamingAdapter struct{ testAdapter }

func (testStreamingAdapter) RunStream(context.Context, RunRequest, OutputStream) (*RunResult, error) {
	return &RunResult{}, nil
}

type testPersistentAdapter struct{ testStreamingAdapter }

func (testPersistentAdapter) ProvisionSandbox(context.Context, ProvisionRequest) error { return nil }

func (testPersistentAdapter) RunInSandbox(context.Context, RunRequest, OutputStream) (*RunResult, error) {
	return &RunResult{}, nil
}

func (testPersistentAdapter) TerminateSandbox(context.Context, string) error { return nil }

func (testPersistentAdapter) DownloadSandboxFile(context.Context, string, string, int64) ([]byte, error) {
	return []byte("ok"), nil
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
	if !caps[CapabilitySandboxPersistent] {
		t.Fatalf("expected %s=true", CapabilitySandboxPersistent)
	}
	if !caps[CapabilitySandboxFileDownload] {
		t.Fatalf("expected %s=true", CapabilitySandboxFileDownload)
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
