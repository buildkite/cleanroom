package darwinvz

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostEgressFilterEnabledReturnsStatusFromSnapshot(t *testing.T) {
	statusPath := filepath.Join(t.TempDir(), "network-filter-status.json")
	t.Setenv(networkFilterStatusPathEnv, statusPath)

	if err := os.WriteFile(
		statusPath,
		[]byte(`{"version":1,"available":true,"loaded":true,"enabled":true}`),
		0o644,
	); err != nil {
		t.Fatalf("write status file: %v", err)
	}

	enabled, detail := hostEgressFilterEnabled()
	if !enabled {
		t.Fatalf("expected enabled=true, detail=%q", detail)
	}
	if detail != "" {
		t.Fatalf("expected empty detail, got %q", detail)
	}
}

func TestHostEgressFilterEnabledReturnsReasonWhenDisabled(t *testing.T) {
	statusPath := filepath.Join(t.TempDir(), "network-filter-status.json")
	t.Setenv(networkFilterStatusPathEnv, statusPath)

	if err := os.WriteFile(
		statusPath,
		[]byte(`{"version":1,"available":true,"loaded":true,"enabled":false,"last_error":"approval pending"}`),
		0o644,
	); err != nil {
		t.Fatalf("write status file: %v", err)
	}

	enabled, detail := hostEgressFilterEnabled()
	if enabled {
		t.Fatalf("expected enabled=false")
	}
	if detail != "approval pending" {
		t.Fatalf("expected detail %q, got %q", "approval pending", detail)
	}
}

func TestHostEgressFilterEnabledHandlesMissingSnapshot(t *testing.T) {
	statusPath := filepath.Join(t.TempDir(), "missing-status.json")
	t.Setenv(networkFilterStatusPathEnv, statusPath)

	enabled, detail := hostEgressFilterEnabled()
	if enabled {
		t.Fatal("expected enabled=false")
	}
	if !strings.Contains(detail, "not found") {
		t.Fatalf("expected not-found detail, got %q", detail)
	}
}
