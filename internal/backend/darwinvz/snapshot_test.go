//go:build darwin

package darwinvz

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
)

func TestCreateSnapshotSyncsPausesAndClonesRootFS(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	rootfsPath := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(rootfsPath, []byte("snapshot-bytes"), 0o644); err != nil {
		t.Fatalf("write rootfs: %v", err)
	}

	var helperOps []string
	adapter := &Adapter{
		executeInSandboxFn: func(_ context.Context, _ context.Context, instance *sandboxInstance, req backend.RunRequest, _ backend.OutputStream) (*backend.RunResult, error) {
			if instance == nil || instance.SandboxID != "cr-test" {
				t.Fatalf("unexpected sandbox instance: %#v", instance)
			}
			if len(req.Command) != 1 || req.Command[0] != "sync" {
				t.Fatalf("unexpected command: %v", req.Command)
			}
			return &backend.RunResult{ExitCode: 0}, nil
		},
		helperRequestFn: func(_ context.Context, helper *helperSession, req helperControlRequest) (helperControlResponse, error) {
			if helper == nil {
				t.Fatal("expected helper session")
			}
			helperOps = append(helperOps, req.Op+":"+req.VMID)
			return helperControlResponse{OK: true}, nil
		},
		sandboxes: map[string]*sandboxInstance{
			"cr-test": {
				SandboxID:    "cr-test",
				Helper:       &helperSession{},
				VMID:         "vm-test",
				Policy:       &policy.CompiledPolicy{NetworkDefault: "deny"},
				vmRootFSPath: rootfsPath,
				exitedCh:     make(chan struct{}),
			},
		},
	}

	result, err := adapter.CreateSnapshot(context.Background(), backend.SnapshotRequest{
		SandboxID:  "cr-test",
		SnapshotID: "snap-test",
		FirecrackerConfig: backend.FirecrackerConfig{
			Snapshots: backend.SnapshotConfig{Enabled: true},
		},
	})
	if err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}
	if got, want := result.StorageRef, filepath.Join(stateHome, "cleanroom", "snapshots", "darwin-vz", "snap-test", "rootfs.ext4"); got != want {
		t.Fatalf("unexpected snapshot storage ref: got %q want %q", got, want)
	}
	if got, want := strings.Join(helperOps, ","), "PauseVM:vm-test,ResumeVM:vm-test"; got != want {
		t.Fatalf("unexpected helper ops: got %q want %q", got, want)
	}

	data, err := os.ReadFile(result.StorageRef)
	if err != nil {
		t.Fatalf("read snapshot rootfs: %v", err)
	}
	if got, want := string(data), "snapshot-bytes"; got != want {
		t.Fatalf("unexpected snapshot contents: got %q want %q", got, want)
	}
}

func TestProvisionSandboxFromSnapshotUsesSnapshotRootFS(t *testing.T) {
	t.Parallel()

	var (
		gotSandbox string
		gotPolicy  *policy.CompiledPolicy
		gotCfg     backend.FirecrackerConfig
	)
	adapter := &Adapter{
		launchSandboxVMFn: func(_ context.Context, sandboxID string, compiled *policy.CompiledPolicy, cfg backend.FirecrackerConfig) (*sandboxInstance, error) {
			gotSandbox = sandboxID
			gotPolicy = compiled
			gotCfg = cfg
			return &sandboxInstance{SandboxID: sandboxID}, nil
		},
	}
	compiled := &policy.CompiledPolicy{
		NetworkDefault: "deny",
		ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Hash:           "policy-hash",
	}

	if err := adapter.ProvisionSandboxFromSnapshot(context.Background(), backend.ProvisionFromSnapshotRequest{
		SandboxID:  "cr-test",
		SnapshotID: "snap-test",
		StorageRef: "/tmp/snap-test.ext4",
		Policy:     compiled,
	}); err != nil {
		t.Fatalf("ProvisionSandboxFromSnapshot returned error: %v", err)
	}
	if got, want := gotSandbox, "cr-test"; got != want {
		t.Fatalf("unexpected sandbox id: got %q want %q", got, want)
	}
	if gotPolicy != compiled {
		t.Fatal("expected compiled policy to be forwarded")
	}
	if got, want := gotCfg.RootFSPath, "/tmp/snap-test.ext4"; got != want {
		t.Fatalf("unexpected snapshot rootfs path: got %q want %q", got, want)
	}
	if _, ok := adapter.sandboxes["cr-test"]; !ok {
		t.Fatal("expected provisioned sandbox to be stored")
	}
}

func TestDeleteSnapshotRemovesSnapshotRootFS(t *testing.T) {
	t.Parallel()

	snapshotDir := filepath.Join(t.TempDir(), "snapshots", "darwin-vz", "snap-test")
	snapshotPath := filepath.Join(snapshotDir, "rootfs.ext4")
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		t.Fatalf("mkdir snapshot dir: %v", err)
	}
	if err := os.WriteFile(snapshotPath, []byte("snapshot-bytes"), 0o644); err != nil {
		t.Fatalf("write snapshot rootfs: %v", err)
	}

	adapter := &Adapter{}
	if err := adapter.DeleteSnapshot(context.Background(), backend.DeleteSnapshotRequest{
		SnapshotID: "snap-test",
		StorageRef: snapshotPath,
		FirecrackerConfig: backend.FirecrackerConfig{
			Snapshots: backend.SnapshotConfig{Enabled: true},
		},
	}); err != nil {
		t.Fatalf("DeleteSnapshot returned error: %v", err)
	}
	if _, err := os.Stat(snapshotPath); !os.IsNotExist(err) {
		t.Fatalf("expected snapshot to be removed, got err=%v", err)
	}
}

func TestSnapshotVolumeDriverRejectsDisabledSnapshots(t *testing.T) {
	t.Parallel()

	_, err := snapshotVolumeDriver(backend.FirecrackerConfig{})
	if err == nil {
		t.Fatal("expected snapshotVolumeDriver to reject disabled snapshots")
	}
	if got := err.Error(); got == "" || !strings.Contains(got, "not enabled") {
		t.Fatalf("unexpected error: %v", err)
	}
}
