package firecracker

import (
	"context"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
)

type testHostRuntime struct {
	checkAccessFn              func(context.Context) error
	checkNetworkingFn          func(context.Context) error
	setupSandboxNetworkFn      func(context.Context, sandboxNetworkRequest) (sandboxNetworkLease, error)
	setupGatewayFirewallFn     func(context.Context, gatewayFirewallRequest) (gatewayFirewallLease, error)
	validateZFSDatasetRootFn   func(context.Context, string) error
	prepareZFSWritableVolumeFn func(context.Context, zfsWritableVolumeRequest) (zfsWritableVolume, error)
	createZFSSnapshotFn        func(context.Context, zfsSnapshotRequest) (zfsSnapshot, error)
	destroyZFSVolumeFn         func(context.Context, string) error
	destroyZFSSnapshotFn       func(context.Context, string) error
}

func (r testHostRuntime) CheckAccess(ctx context.Context) error {
	if r.checkAccessFn == nil {
		return nil
	}
	return r.checkAccessFn(ctx)
}

func (r testHostRuntime) CheckNetworking(ctx context.Context) error {
	if r.checkNetworkingFn == nil {
		return nil
	}
	return r.checkNetworkingFn(ctx)
}

func (r testHostRuntime) SetupSandboxNetwork(ctx context.Context, req sandboxNetworkRequest) (sandboxNetworkLease, error) {
	if r.setupSandboxNetworkFn == nil {
		return sandboxNetworkLease{}, nil
	}
	return r.setupSandboxNetworkFn(ctx, req)
}

func (r testHostRuntime) SetupGatewayFirewall(ctx context.Context, req gatewayFirewallRequest) (gatewayFirewallLease, error) {
	if r.setupGatewayFirewallFn == nil {
		return gatewayFirewallLease{}, nil
	}
	return r.setupGatewayFirewallFn(ctx, req)
}

func (r testHostRuntime) ValidateZFSDatasetRoot(ctx context.Context, dataset string) error {
	if r.validateZFSDatasetRootFn == nil {
		return nil
	}
	return r.validateZFSDatasetRootFn(ctx, dataset)
}

func (r testHostRuntime) PrepareZFSWritableVolume(ctx context.Context, req zfsWritableVolumeRequest) (zfsWritableVolume, error) {
	if r.prepareZFSWritableVolumeFn == nil {
		return zfsWritableVolume{}, nil
	}
	return r.prepareZFSWritableVolumeFn(ctx, req)
}

func (r testHostRuntime) CreateZFSSnapshot(ctx context.Context, req zfsSnapshotRequest) (zfsSnapshot, error) {
	if r.createZFSSnapshotFn == nil {
		return zfsSnapshot{}, nil
	}
	return r.createZFSSnapshotFn(ctx, req)
}

func (r testHostRuntime) DestroyZFSVolume(ctx context.Context, volumeRef string) error {
	if r.destroyZFSVolumeFn == nil {
		return nil
	}
	return r.destroyZFSVolumeFn(ctx, volumeRef)
}

func (r testHostRuntime) DestroyZFSSnapshot(ctx context.Context, snapshotRef string) error {
	if r.destroyZFSSnapshotFn == nil {
		return nil
	}
	return r.destroyZFSSnapshotFn(ctx, snapshotRef)
}

func TestSetupGatewayFirewallUsesHostRuntime(t *testing.T) {
	t.Parallel()

	called := false
	cleanedUp := false

	cleanup, err := setupGatewayFirewall(context.Background(), 8170, testHostRuntime{
		setupGatewayFirewallFn: func(_ context.Context, req gatewayFirewallRequest) (gatewayFirewallLease, error) {
			called = true
			if got, want := req.Port, 8170; got != want {
				t.Fatalf("unexpected gateway firewall port: got %d want %d", got, want)
			}
			return gatewayFirewallLease{release: func(context.Context) error {
				cleanedUp = true
				return nil
			}}, nil
		},
	})
	if err != nil {
		t.Fatalf("setupGatewayFirewall returned error: %v", err)
	}
	if !called {
		t.Fatal("expected host runtime gateway firewall setup to be called")
	}

	cleanup()
	if !cleanedUp {
		t.Fatal("expected host runtime cleanup to run")
	}
}

func TestPrepareWritableRootVolumeUsesHostRuntimeForZFS(t *testing.T) {
	prevHostRuntimeFn := newHostRuntimeFn
	t.Cleanup(func() { newHostRuntimeFn = prevHostRuntimeFn })

	var gotReq zfsWritableVolumeRequest
	destroyedVolume := ""
	newHostRuntimeFn = func(cfg backend.FirecrackerConfig) hostRuntime {
		if got, want := cfg.Snapshots.Driver, "zfs"; got != want {
			t.Fatalf("unexpected snapshot driver: got %q want %q", got, want)
		}
		return testHostRuntime{
			prepareZFSWritableVolumeFn: func(_ context.Context, req zfsWritableVolumeRequest) (zfsWritableVolume, error) {
				gotReq = req
				return zfsWritableVolume{
					Ref:            "tank/cleanroom/sandboxes/exec-1",
					AttachmentPath: "/dev/zvol/tank/cleanroom/sandboxes/exec-1",
				}, nil
			},
			destroyZFSVolumeFn: func(_ context.Context, volumeRef string) error {
				destroyedVolume = volumeRef
				return nil
			},
		}
	}

	volume, cleanupVolume, err := prepareWritableRootVolume(context.Background(), backend.FirecrackerConfig{
		MinimumRootFSBytes: 8 << 20,
		Snapshots: backend.SnapshotConfig{
			Driver:     "zfs",
			ZFSDataset: "tank/cleanroom",
		},
	}, "exec-1", t.TempDir(), "/tmp/runtime-rootfs.ext4")
	if err != nil {
		t.Fatalf("prepareWritableRootVolume returned error: %v", err)
	}
	if cleanupVolume == nil {
		t.Fatal("expected cleanup function")
	}
	if got, want := gotReq, (zfsWritableVolumeRequest{
		VolumeID:     "exec-1",
		SourcePath:   "/tmp/runtime-rootfs.ext4",
		MinimumBytes: 8 << 20,
	}); got != want {
		t.Fatalf("unexpected zfs writable volume request: got %+v want %+v", got, want)
	}
	if got, want := volume.Ref, "tank/cleanroom/sandboxes/exec-1"; got != want {
		t.Fatalf("unexpected volume ref: got %q want %q", got, want)
	}

	cleanupVolume()
	if got, want := destroyedVolume, "tank/cleanroom/sandboxes/exec-1"; got != want {
		t.Fatalf("unexpected destroyed zfs volume ref: got %q want %q", got, want)
	}
}

func TestSandboxNetworkLeaseReleaseRunsCleanup(t *testing.T) {
	t.Parallel()

	released := false
	lease := sandboxNetworkLease{
		release: func(context.Context) error {
			released = true
			return nil
		},
	}

	if err := lease.Release(context.Background()); err != nil {
		t.Fatalf("Release returned error: %v", err)
	}
	if !released {
		t.Fatal("expected sandbox network lease release to invoke cleanup")
	}
}
