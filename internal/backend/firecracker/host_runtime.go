package firecracker

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/volumestore"
)

type hostRuntime interface {
	CheckAccess(context.Context) error
	CheckNetworking(context.Context) error
	SetupSandboxNetwork(context.Context, sandboxNetworkRequest) (sandboxNetworkLease, error)
	SetupGatewayFirewall(context.Context, gatewayFirewallRequest) (gatewayFirewallLease, error)
	ValidateZFSDatasetRoot(context.Context, string) error
	PrepareZFSWritableVolume(context.Context, zfsWritableVolumeRequest) (zfsWritableVolume, error)
	CreateZFSSnapshot(context.Context, zfsSnapshotRequest) (zfsSnapshot, error)
	DestroyZFSVolume(context.Context, string) error
	DestroyZFSSnapshot(context.Context, string) error
}

type hostRuntimeFactory func(backend.FirecrackerConfig) hostRuntime

type sandboxNetworkRequest struct {
	SandboxID   string
	AllowAll    bool
	Allow       []policy.AllowRule
	GatewayPort int
	OnDeny      func(sandboxID, queryName string)
	OnBlocked   func(string)
}

type gatewayFirewallRequest struct {
	Port int
}

type sandboxNetworkLease struct {
	Config  hostNetworkConfig
	release func(context.Context) error
}

type gatewayFirewallLease struct {
	release func(context.Context) error
}

type zfsWritableVolumeRequest struct {
	VolumeID     string
	SourcePath   string
	SnapshotRef  string
	MinimumBytes int64
}

type zfsWritableVolume struct {
	Ref            string
	AttachmentPath string
}

type zfsSnapshotRequest struct {
	SnapshotID string
	VolumeRef  string
}

type zfsSnapshot struct {
	StorageRef string
}

type runnerBackedHostRuntime struct {
	cfg    backend.FirecrackerConfig
	runner privilegedCommandRunner
}

type hostRuntimeVolumeCommandRunner struct {
	runner privilegedCommandRunner
}

var newHostRuntimeFn hostRuntimeFactory = newRunnerBackedHostRuntime

func hostRuntimeForConfig(cfg backend.FirecrackerConfig) hostRuntime {
	if newHostRuntimeFn == nil {
		return newRunnerBackedHostRuntime(cfg)
	}
	return newHostRuntimeFn(cfg)
}

func newRunnerBackedHostRuntime(cfg backend.FirecrackerConfig) hostRuntime {
	return runnerBackedHostRuntime{
		cfg:    cfg,
		runner: newPrivilegedCommandRunner(cfg),
	}
}

func (r runnerBackedHostRuntime) CheckAccess(ctx context.Context) error {
	return r.runner.Run(ctx, "true")
}

func (r runnerBackedHostRuntime) CheckNetworking(ctx context.Context) error {
	return r.runner.Run(ctx, "ip", "link", "show")
}

func (l sandboxNetworkLease) Release(ctx context.Context) error {
	if l.release == nil {
		return nil
	}
	return l.release(ctx)
}

func (l gatewayFirewallLease) Release(ctx context.Context) error {
	if l.release == nil {
		return nil
	}
	return l.release(ctx)
}

func (r runnerBackedHostRuntime) SetupSandboxNetwork(ctx context.Context, req sandboxNetworkRequest) (sandboxNetworkLease, error) {
	config, cleanup, err := setupHostNetwork(ctx, req.SandboxID, req.AllowAll, req.Allow, req.GatewayPort, r.runner, req.OnDeny, req.OnBlocked)
	if err != nil {
		return sandboxNetworkLease{}, err
	}
	return sandboxNetworkLease{
		Config: config,
		release: func(context.Context) error {
			cleanup()
			return nil
		},
	}, nil
}

func (r runnerBackedHostRuntime) SetupGatewayFirewall(ctx context.Context, req gatewayFirewallRequest) (gatewayFirewallLease, error) {
	cleanup, err := setupGatewayFirewallWithRunner(ctx, req.Port, r.runner)
	if err != nil {
		return gatewayFirewallLease{}, err
	}
	return gatewayFirewallLease{
		release: func(context.Context) error {
			cleanup()
			return nil
		},
	}, nil
}

func (r runnerBackedHostRuntime) ValidateZFSDatasetRoot(ctx context.Context, dataset string) error {
	return validateZFSDatasetRootWithRunner(ctx, r.runner, dataset)
}

func (r runnerBackedHostRuntime) PrepareZFSWritableVolume(ctx context.Context, req zfsWritableVolumeRequest) (zfsWritableVolume, error) {
	driver, err := r.openZFSVolumeStore(r.cfg.Snapshots.ZFSDataset)
	if err != nil {
		return zfsWritableVolume{}, err
	}

	sourcePath := strings.TrimSpace(req.SourcePath)
	snapshotRef := strings.TrimSpace(req.SnapshotRef)
	if (sourcePath == "") == (snapshotRef == "") {
		return zfsWritableVolume{}, fmt.Errorf("zfs writable volume requires exactly one of source path or snapshot ref")
	}

	cleanupVolume := func(ref string) {
		if err := driver.DestroyVolume(context.Background(), volumestore.DestroyVolumeRequest{VolumeRef: ref}); err != nil {
			log.Printf("firecracker: cleanup persistent volume %q: %v", ref, err)
		}
	}
	resizeVolume := func(volume volumestore.WritableVolume) (zfsWritableVolume, error) {
		if err := volumestore.EnsureWritableVolumeMinimumSize(ctx, driver, volume, req.MinimumBytes); err != nil {
			cleanupVolume(volume.Ref)
			return zfsWritableVolume{}, fmt.Errorf("resize persistent rootfs: %w", err)
		}
		return zfsWritableVolume{Ref: volume.Ref, AttachmentPath: volume.AttachmentPath}, nil
	}

	if sourcePath != "" {
		rootfsPath, err := filepath.Abs(sourcePath)
		if err != nil {
			return zfsWritableVolume{}, err
		}
		if _, err := os.Stat(rootfsPath); err != nil {
			return zfsWritableVolume{}, fmt.Errorf("rootfs %s: %w", rootfsPath, err)
		}

		baseVolume, err := driver.EnsureBaseVolume(ctx, volumestore.EnsureBaseVolumeRequest{
			BaseID:       strings.TrimSuffix(filepath.Base(rootfsPath), filepath.Ext(rootfsPath)),
			SourcePath:   rootfsPath,
			MinimumBytes: req.MinimumBytes,
		})
		if err != nil {
			return zfsWritableVolume{}, fmt.Errorf("prepare base volume: %w", err)
		}
		volume, err := driver.CreateWritableVolume(ctx, volumestore.CreateWritableVolumeRequest{
			VolumeID: req.VolumeID,
			BaseRef:  baseVolume.Ref,
		})
		if err != nil {
			return zfsWritableVolume{}, err
		}
		return resizeVolume(volume)
	}

	volume, err := driver.CloneSnapshotToVolume(ctx, volumestore.CloneSnapshotToVolumeRequest{
		VolumeID:    req.VolumeID,
		SnapshotRef: snapshotRef,
	})
	if err != nil {
		return zfsWritableVolume{}, err
	}
	return resizeVolume(volume)
}

func (r runnerBackedHostRuntime) CreateZFSSnapshot(ctx context.Context, req zfsSnapshotRequest) (zfsSnapshot, error) {
	driver, err := r.openZFSVolumeStore(r.cfg.Snapshots.ZFSDataset)
	if err != nil {
		return zfsSnapshot{}, err
	}
	snapshot, err := driver.SnapshotVolume(ctx, volumestore.SnapshotVolumeRequest{
		SnapshotID: req.SnapshotID,
		VolumeRef:  req.VolumeRef,
	})
	if err != nil {
		return zfsSnapshot{}, err
	}
	return zfsSnapshot{StorageRef: snapshot.StorageRef}, nil
}

func (r runnerBackedHostRuntime) DestroyZFSVolume(ctx context.Context, volumeRef string) error {
	driver, err := r.openZFSVolumeStore(r.cfg.Snapshots.ZFSDataset)
	if err != nil {
		return err
	}
	return driver.DestroyVolume(ctx, volumestore.DestroyVolumeRequest{VolumeRef: volumeRef})
}

func (r runnerBackedHostRuntime) DestroyZFSSnapshot(ctx context.Context, snapshotRef string) error {
	driver, err := r.openZFSVolumeStore(r.cfg.Snapshots.ZFSDataset)
	if err != nil {
		return err
	}
	return driver.DestroySnapshot(ctx, volumestore.DestroySnapshotRequest{SnapshotRef: snapshotRef})
}

func (r runnerBackedHostRuntime) openZFSVolumeStore(datasetRoot string) (*volumestore.ZFSDriver, error) {
	return volumestore.NewZFSDriver(volumestore.ZFSDriverOptions{
		DatasetRoot: datasetRoot,
		Runner:      hostRuntimeVolumeCommandRunner{runner: r.runner},
	})
}

func (r hostRuntimeVolumeCommandRunner) Run(ctx context.Context, command string, args ...string) error {
	return r.runner.Run(ctx, append([]string{command}, args...)...)
}

func (r hostRuntimeVolumeCommandRunner) Output(ctx context.Context, command string, args ...string) ([]byte, error) {
	return r.runner.Output(ctx, append([]string{command}, args...)...)
}

func validateZFSDatasetRootWithRunner(ctx context.Context, runner privilegedCommandRunner, dataset string) error {
	dataset = strings.TrimSpace(dataset)
	if dataset == "" {
		return fmt.Errorf("zfs dataset root is empty")
	}
	if !isCleanroomZFSDatasetRoot(dataset) {
		return fmt.Errorf("zfs dataset root %q must be cleanroom or */cleanroom", dataset)
	}
	out, err := runner.Output(ctx, "zfs", "list", "-H", "-d", "0", "-o", "name", dataset)
	if err != nil {
		return fmt.Errorf("unable to access zfs dataset root %q: %v", dataset, err)
	}
	if strings.TrimSpace(string(out)) != dataset {
		return fmt.Errorf("zfs dataset probe for %q returned %q", dataset, strings.TrimSpace(string(out)))
	}
	return nil
}
