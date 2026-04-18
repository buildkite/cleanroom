package firecracker

import (
	"context"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/volumestore"
)

type testHostRuntime struct {
	checkAccessFn            func(context.Context) error
	checkNetworkingFn        func(context.Context) error
	setupSandboxNetworkFn    func(context.Context, sandboxNetworkRequest) (sandboxNetworkLease, error)
	setupGatewayFirewallFn   func(context.Context, gatewayFirewallRequest) (gatewayFirewallLease, error)
	validateZFSDatasetRootFn func(context.Context, string) error
	openZFSVolumeStoreFn     func(string) (volumestore.Driver, error)
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

func (r testHostRuntime) OpenZFSVolumeStore(datasetRoot string) (volumestore.Driver, error) {
	if r.openZFSVolumeStoreFn == nil {
		return nil, nil
	}
	return r.openZFSVolumeStoreFn(datasetRoot)
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

func TestRootFSVolumeStoreDriverUsesHostRuntimeForZFS(t *testing.T) {
	prevHostRuntimeFn := newHostRuntimeFn
	t.Cleanup(func() { newHostRuntimeFn = prevHostRuntimeFn })

	var gotDatasetRoot string
	newHostRuntimeFn = func(cfg backend.FirecrackerConfig) hostRuntime {
		if got, want := cfg.Snapshots.Driver, "zfs"; got != want {
			t.Fatalf("unexpected snapshot driver: got %q want %q", got, want)
		}
		return testHostRuntime{
			openZFSVolumeStoreFn: func(datasetRoot string) (volumestore.Driver, error) {
				gotDatasetRoot = datasetRoot
				return testVolumeDriver{}, nil
			},
		}
	}

	driver, err := rootFSVolumeStoreDriver(backend.FirecrackerConfig{
		Snapshots: backend.SnapshotConfig{
			Driver:     "zfs",
			ZFSDataset: "tank/cleanroom",
		},
	})
	if err != nil {
		t.Fatalf("rootFSVolumeStoreDriver returned error: %v", err)
	}
	if got, want := gotDatasetRoot, "tank/cleanroom"; got != want {
		t.Fatalf("unexpected zfs dataset root: got %q want %q", got, want)
	}
	if _, ok := driver.(testVolumeDriver); !ok {
		t.Fatalf("unexpected zfs driver type: %T", driver)
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
