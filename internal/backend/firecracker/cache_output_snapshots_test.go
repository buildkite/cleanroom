package firecracker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/volumestore"
	"github.com/buildkite/cleanroom/internal/vsockexec"
)

func TestSnapshotCacheOutputVolumesSyncsPausesAndSnapshotsSelectedVolumes(t *testing.T) {
	prevSnapshotDriver := snapshotVolumeStoreDriverFn
	prevSignal := sendProcessSignal
	t.Cleanup(func() {
		snapshotVolumeStoreDriverFn = prevSnapshotDriver
		sendProcessSignal = prevSignal
	})

	var signals []syscall.Signal
	sendProcessSignal = func(_ *os.Process, sig syscall.Signal) error {
		signals = append(signals, sig)
		return nil
	}

	var snapshotReqs []volumestore.SnapshotVolumeRequest
	snapshotVolumeStoreDriverFn = func(cfg backend.FirecrackerConfig) (volumestore.Driver, error) {
		if !cfg.Snapshots.Enabled {
			t.Fatal("expected snapshots to be enabled")
		}
		if got, want := cfg.Snapshots.Driver, "file"; got != want {
			t.Fatalf("expected per-volume storage driver override, got %q want %q", got, want)
		}
		return testVolumeDriver{
			snapshotVolumeFn: func(_ context.Context, req volumestore.SnapshotVolumeRequest) (volumestore.Snapshot, error) {
				snapshotReqs = append(snapshotReqs, req)
				return volumestore.Snapshot{
					StorageRef:         "snapshot-storage-ref",
					StorageSizeBytes:   123,
					ExclusiveSizeBytes: 45,
					DriverMetadata:     "metadata",
				}, nil
			},
		}, nil
	}

	var syncCommands [][]string
	adapter := &Adapter{
		runGuestCommandFn: func(_ context.Context, _ context.Context, _ <-chan struct{}, _ func() error, _ string, _ uint32, req vsockexec.ExecRequest, _ backend.OutputStream) (vsockexec.ExecResponse, guestExecTiming, error) {
			syncCommands = append(syncCommands, append([]string(nil), req.Command...))
			return vsockexec.ExecResponse{ExitCode: 0}, guestExecTiming{}, nil
		},
		sandboxes: map[string]*sandboxInstance{
			"cr-test": {
				SandboxID: "cr-test",
				VsockPath: "/tmp/fake.sock",
				GuestPort: 10700,
				fcCmd:     &exec.Cmd{Process: &os.Process{Pid: 42}},
				exitedCh:  make(chan struct{}),
				cacheOutputVolumes: []preparedCacheOutputVolume{
					testPreparedCacheOutputVolumeWithDriver("dependency-volume", "deps", "key-a", "volume-a", "volume-a-ref", "file"),
					testPreparedCacheOutputVolume("dependency-volume", "tools", "key-b", "volume-b", "volume-b-ref"),
				},
			},
		},
	}

	result, err := adapter.SnapshotCacheOutputVolumes(context.Background(), backend.SnapshotCacheOutputVolumesRequest{
		SandboxID:        "cr-test",
		SnapshotIDPrefix: "cacheout",
		VolumeIDs:        []string{"volume-a"},
		FirecrackerConfig: backend.FirecrackerConfig{
			Snapshots: backend.SnapshotConfig{Enabled: true, Driver: "zfs"},
		},
	})
	if err != nil {
		t.Fatalf("SnapshotCacheOutputVolumes returned error: %v", err)
	}
	if got, want := syncCommands, [][]string{{"sync"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected sync commands: got %#v want %#v", got, want)
	}
	if got, want := signals, []syscall.Signal{syscall.SIGSTOP, syscall.SIGCONT}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected signals: got %v want %v", got, want)
	}
	if got, want := snapshotReqs, []volumestore.SnapshotVolumeRequest{{
		SnapshotID: cacheOutputSnapshotID("cacheout", "volume-a"),
		VolumeRef:  "volume-a-ref",
	}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected snapshot requests: got %#v want %#v", got, want)
	}
	if result == nil || len(result.Volumes) != 1 {
		t.Fatalf("expected one snapshot result, got %#v", result)
	}
	snapshot := result.Volumes[0]
	if got, want := snapshot.Stage, "dependency-volume"; got != want {
		t.Fatalf("unexpected stage: got %q want %q", got, want)
	}
	if got, want := snapshot.BlockName, "deps"; got != want {
		t.Fatalf("unexpected block name: got %q want %q", got, want)
	}
	if got, want := snapshot.CacheKey, "key-a"; got != want {
		t.Fatalf("unexpected cache key: got %q want %q", got, want)
	}
	if got, want := snapshot.VolumeID, "volume-a"; got != want {
		t.Fatalf("unexpected volume id: got %q want %q", got, want)
	}
	if got, want := snapshot.StorageDriver, "file"; got != want {
		t.Fatalf("unexpected storage driver: got %q want %q", got, want)
	}
	if got, want := snapshot.StorageRef, "snapshot-storage-ref"; got != want {
		t.Fatalf("unexpected storage ref: got %q want %q", got, want)
	}
	if got, want := snapshot.SnapshotRef, "snapshot-storage-ref"; got != want {
		t.Fatalf("unexpected snapshot ref: got %q want %q", got, want)
	}
	if got, want := snapshot.StorageSizeBytes, int64(123); got != want {
		t.Fatalf("unexpected storage size: got %d want %d", got, want)
	}
	if got, want := snapshot.ExclusiveSizeBytes, int64(45); got != want {
		t.Fatalf("unexpected exclusive size: got %d want %d", got, want)
	}
	if got, want := snapshot.DriverMetadata, "metadata"; got != want {
		t.Fatalf("unexpected driver metadata: got %q want %q", got, want)
	}
	if got, want := snapshot.Outputs, []backend.CacheOutputVolumeSnapshotOutput{
		{Kind: "dir", GuestPath: "/deps", VolumeSubpath: "dir/0"},
		{Kind: "file", GuestPath: "/deps.lock", VolumeSubpath: "file/0", Mode: 0o640},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected outputs: got %#v want %#v", got, want)
	}
}

func TestSnapshotCacheOutputVolumesCleansCreatedSnapshotsOnFailure(t *testing.T) {
	prevSnapshotDriver := snapshotVolumeStoreDriverFn
	prevSignal := sendProcessSignal
	t.Cleanup(func() {
		snapshotVolumeStoreDriverFn = prevSnapshotDriver
		sendProcessSignal = prevSignal
	})

	var signals []syscall.Signal
	sendProcessSignal = func(_ *os.Process, sig syscall.Signal) error {
		signals = append(signals, sig)
		return nil
	}

	var destroyed []string
	snapshotVolumeStoreDriverFn = func(backend.FirecrackerConfig) (volumestore.Driver, error) {
		return testVolumeDriver{
			snapshotVolumeFn: func(_ context.Context, req volumestore.SnapshotVolumeRequest) (volumestore.Snapshot, error) {
				if req.VolumeRef == "volume-b-ref" {
					return volumestore.Snapshot{}, errors.New("boom")
				}
				return volumestore.Snapshot{StorageRef: "snapshot-a"}, nil
			},
			destroySnapshotFn: func(_ context.Context, req volumestore.DestroySnapshotRequest) error {
				destroyed = append(destroyed, req.SnapshotRef)
				return nil
			},
		}, nil
	}

	adapter := &Adapter{
		runGuestCommandFn: func(context.Context, context.Context, <-chan struct{}, func() error, string, uint32, vsockexec.ExecRequest, backend.OutputStream) (vsockexec.ExecResponse, guestExecTiming, error) {
			return vsockexec.ExecResponse{ExitCode: 0}, guestExecTiming{}, nil
		},
		sandboxes: map[string]*sandboxInstance{
			"cr-test": {
				SandboxID: "cr-test",
				VsockPath: "/tmp/fake.sock",
				GuestPort: 10700,
				fcCmd:     &exec.Cmd{Process: &os.Process{Pid: 42}},
				exitedCh:  make(chan struct{}),
				cacheOutputVolumes: []preparedCacheOutputVolume{
					testPreparedCacheOutputVolume("dependency-volume", "deps", "key-a", "volume-a", "volume-a-ref"),
					testPreparedCacheOutputVolume("dependency-volume", "tools", "key-b", "volume-b", "volume-b-ref"),
				},
			},
		},
	}

	_, err := adapter.SnapshotCacheOutputVolumes(context.Background(), backend.SnapshotCacheOutputVolumesRequest{
		SandboxID:        "cr-test",
		SnapshotIDPrefix: "cacheout",
		FirecrackerConfig: backend.FirecrackerConfig{
			Snapshots: backend.SnapshotConfig{Enabled: true},
		},
	})
	if err == nil || !strings.Contains(err.Error(), `snapshot cache output volume "volume-b"`) {
		t.Fatalf("expected volume-b snapshot failure, got %v", err)
	}
	if got, want := signals, []syscall.Signal{syscall.SIGSTOP, syscall.SIGCONT}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected signals: got %v want %v", got, want)
	}
	if got, want := destroyed, []string{"snapshot-a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected destroyed snapshots: got %#v want %#v", got, want)
	}
}

func TestSnapshotCacheOutputVolumesRejectsUnknownVolumeID(t *testing.T) {
	adapter := &Adapter{
		sandboxes: map[string]*sandboxInstance{
			"cr-test": {
				SandboxID: "cr-test",
				exitedCh:  make(chan struct{}),
				cacheOutputVolumes: []preparedCacheOutputVolume{
					testPreparedCacheOutputVolume("dependency-volume", "deps", "key-a", "volume-a", "volume-a-ref"),
				},
			},
		},
	}

	_, err := adapter.SnapshotCacheOutputVolumes(context.Background(), backend.SnapshotCacheOutputVolumesRequest{
		SandboxID:        "cr-test",
		SnapshotIDPrefix: "cacheout",
		VolumeIDs:        []string{"missing"},
		FirecrackerConfig: backend.FirecrackerConfig{
			Snapshots: backend.SnapshotConfig{Enabled: true},
		},
	})
	if err == nil || !strings.Contains(err.Error(), `unknown cache output volume id "missing"`) {
		t.Fatalf("expected unknown volume id error, got %v", err)
	}
}

func testPreparedCacheOutputVolume(stage, blockName, cacheKey, volumeID, volumeRef string) preparedCacheOutputVolume {
	return testPreparedCacheOutputVolumeWithDriver(stage, blockName, cacheKey, volumeID, volumeRef, "")
}

func testPreparedCacheOutputVolumeWithDriver(stage, blockName, cacheKey, volumeID, volumeRef, storageDriver string) preparedCacheOutputVolume {
	return preparedCacheOutputVolume{
		Spec: backend.CacheOutputVolumeSpec{
			Stage:         stage,
			BlockName:     blockName,
			CacheKey:      cacheKey,
			VolumeID:      volumeID,
			StorageDriver: storageDriver,
			DirMappings: []backend.CacheOutputDirMapping{{
				GuestPath: "/deps",
				Subpath:   "dir/0",
			}},
			FileMappings: []backend.CacheOutputFileMapping{{
				GuestPath: "/deps.lock",
				Subpath:   "file/0",
				Mode:      0o640,
			}},
		},
		Volume: volumestore.WritableVolume{Ref: volumeRef, AttachmentPath: volumeRef},
	}
}
