//go:build darwin

package darwinvz

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/volumestore"
	"golang.org/x/sys/unix"
)

func TestSnapshotCacheOutputVolumesSyncsPausesAndSnapshotsSelectedVolumes(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	volumePath := filepath.Join(t.TempDir(), "cache-output.ext4")
	if err := os.WriteFile(volumePath, []byte("volume-bytes"), 0o644); err != nil {
		t.Fatalf("write cache output volume: %v", err)
	}

	var syncCommands [][]string
	var helperOps []string
	adapter := &Adapter{
		executeInSandboxFn: func(bootCtx context.Context, runCtx context.Context, instance *sandboxInstance, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
			if instance == nil || instance.SandboxID != "cr-test" {
				t.Fatalf("unexpected sandbox instance: %#v", instance)
			}
			syncCommands = append(syncCommands, append([]string(nil), req.Command...))
			bootDeadline, bootOK := bootCtx.Deadline()
			runDeadline, runOK := runCtx.Deadline()
			if !bootOK || !runOK {
				t.Fatalf("expected sync contexts to carry deadlines: boot=%v run=%v", bootOK, runOK)
			}
			if !bootDeadline.Equal(runDeadline) {
				t.Fatalf("expected run context to use sync timeout deadline: boot=%s run=%s", bootDeadline, runDeadline)
			}
			return &backend.ExecutionResult{ExitCode: 0}, nil
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
				SandboxID: "cr-test",
				Helper:    &helperSession{},
				VMID:      "vm-test",
				Policy:    &policy.CompiledPolicy{NetworkDefault: "deny"},
				exitedCh:  make(chan struct{}),
				cacheOutputVolumes: []preparedDarwinVZCacheOutputVolume{
					testPreparedDarwinVZCacheOutputVolume("dependency-volume", "deps", "key-a", "volume-a", volumePath),
					testPreparedDarwinVZCacheOutputVolume("dependency-volume", "tools", "key-b", "volume-b", filepath.Join(t.TempDir(), "other.ext4")),
				},
			},
		},
	}

	result, err := adapter.SnapshotCacheOutputVolumes(context.Background(), backend.SnapshotCacheOutputVolumesRequest{
		SandboxID:        "cr-test",
		SnapshotIDPrefix: "cacheout",
		VolumeIDs:        []string{"volume-a"},
		FirecrackerConfig: backend.FirecrackerConfig{
			Snapshots: backend.SnapshotConfig{Enabled: true, Driver: "file"},
		},
	})
	if err != nil {
		t.Fatalf("SnapshotCacheOutputVolumes returned error: %v", err)
	}
	if got, want := syncCommands, [][]string{{"sync"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected sync commands: got %#v want %#v", got, want)
	}
	if got, want := strings.Join(helperOps, ","), "PauseVM:vm-test,ResumeVM:vm-test"; got != want {
		t.Fatalf("unexpected helper ops: got %q want %q", got, want)
	}
	if result == nil || len(result.Volumes) != 1 {
		t.Fatalf("expected one snapshot result, got %#v", result)
	}
	snapshotID := darwinVZCacheOutputSnapshotID("cacheout", "volume-a")
	wantStorageRef := filepath.Join(stateHome, "cleanroom", "snapshots", "darwin-vz", snapshotID, "rootfs.ext4")
	snapshot := result.Volumes[0]
	if got, want := snapshot.StorageRef, wantStorageRef; got != want {
		t.Fatalf("unexpected storage ref: got %q want %q", got, want)
	}
	if got, want := snapshot.SnapshotID, snapshotID; got != want {
		t.Fatalf("unexpected snapshot id: got %q want %q", got, want)
	}
	if got, want := snapshot.SnapshotRef, wantStorageRef; got != want {
		t.Fatalf("unexpected snapshot ref: got %q want %q", got, want)
	}
	if got, want := snapshot.StorageDriver, "file"; got != want {
		t.Fatalf("unexpected storage driver: got %q want %q", got, want)
	}
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
	if got, want := snapshot.Outputs, []backend.CacheOutputVolumeSnapshotOutput{
		{Kind: "dir", GuestPath: "/deps", VolumeSubpath: "dir/0"},
		{Kind: "file", GuestPath: "/deps.lock", VolumeSubpath: "file/0", Mode: 0o640},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected outputs: got %#v want %#v", got, want)
	}
	data, err := os.ReadFile(snapshot.StorageRef)
	if err != nil {
		t.Fatalf("read snapshot volume: %v", err)
	}
	if got, want := string(data), "volume-bytes"; got != want {
		t.Fatalf("unexpected snapshot contents: got %q want %q", got, want)
	}
}

func TestSnapshotCacheOutputVolumesCleansCreatedSnapshotsOnFailure(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	firstVolumePath := filepath.Join(t.TempDir(), "cache-output-a.ext4")
	if err := os.WriteFile(firstVolumePath, []byte("volume-a"), 0o644); err != nil {
		t.Fatalf("write first cache output volume: %v", err)
	}
	missingVolumePath := filepath.Join(t.TempDir(), "missing.ext4")

	var helperOps []string
	adapter := &Adapter{
		executeInSandboxFn: func(context.Context, context.Context, *sandboxInstance, backend.ExecutionRequest, backend.OutputStream) (*backend.ExecutionResult, error) {
			return &backend.ExecutionResult{ExitCode: 0}, nil
		},
		helperRequestFn: func(_ context.Context, _ *helperSession, req helperControlRequest) (helperControlResponse, error) {
			helperOps = append(helperOps, req.Op+":"+req.VMID)
			return helperControlResponse{OK: true}, nil
		},
		sandboxes: map[string]*sandboxInstance{
			"cr-test": {
				SandboxID: "cr-test",
				Helper:    &helperSession{},
				VMID:      "vm-test",
				Policy:    &policy.CompiledPolicy{NetworkDefault: "deny"},
				exitedCh:  make(chan struct{}),
				cacheOutputVolumes: []preparedDarwinVZCacheOutputVolume{
					testPreparedDarwinVZCacheOutputVolume("dependency-volume", "deps", "key-a", "volume-a", firstVolumePath),
					testPreparedDarwinVZCacheOutputVolume("dependency-volume", "tools", "key-b", "volume-b", missingVolumePath),
				},
			},
		},
	}

	_, err := adapter.SnapshotCacheOutputVolumes(context.Background(), backend.SnapshotCacheOutputVolumesRequest{
		SandboxID:        "cr-test",
		SnapshotIDPrefix: "cacheout",
		FirecrackerConfig: backend.FirecrackerConfig{
			Snapshots: backend.SnapshotConfig{Enabled: true, Driver: "file"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), `snapshot cache output volume "volume-b"`) {
		t.Fatalf("expected volume-b snapshot failure, got %v", err)
	}
	if got, want := strings.Join(helperOps, ","), "PauseVM:vm-test,ResumeVM:vm-test"; got != want {
		t.Fatalf("unexpected helper ops: got %q want %q", got, want)
	}

	firstSnapshotPath := filepath.Join(stateHome, "cleanroom", "snapshots", "darwin-vz", darwinVZCacheOutputSnapshotID("cacheout", "volume-a"), "rootfs.ext4")
	if _, err := os.Stat(firstSnapshotPath); !os.IsNotExist(err) {
		t.Fatalf("expected first snapshot to be cleaned up, stat err=%v", err)
	}
}

func TestSnapshotCacheOutputVolumesRejectsUnknownVolumeID(t *testing.T) {
	adapter := &Adapter{
		sandboxes: map[string]*sandboxInstance{
			"cr-test": {
				SandboxID: "cr-test",
				exitedCh:  make(chan struct{}),
				cacheOutputVolumes: []preparedDarwinVZCacheOutputVolume{
					testPreparedDarwinVZCacheOutputVolume("dependency-volume", "deps", "key-a", "volume-a", "volume-a-ref"),
				},
			},
		},
	}

	_, err := adapter.SnapshotCacheOutputVolumes(context.Background(), backend.SnapshotCacheOutputVolumesRequest{
		SandboxID:        "cr-test",
		SnapshotIDPrefix: "cacheout",
		VolumeIDs:        []string{"missing"},
		FirecrackerConfig: backend.FirecrackerConfig{
			Snapshots: backend.SnapshotConfig{Enabled: true, Driver: "file"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), `unknown cache output volume id "missing"`) {
		t.Fatalf("expected unknown volume id error, got %v", err)
	}
}

func TestSnapshotDarwinVZCacheOutputVolumeReportsFallbackDriver(t *testing.T) {
	primary := &stubVolumeDriver{
		name:        "apfs",
		snapshotErr: fmt.Errorf("clonefile snapshot volume: %w", unix.ENOTSUP),
	}
	fallback := &stubVolumeDriver{
		name:     "file",
		snapshot: volumestore.Snapshot{Ref: "file-snapshot", StorageRef: "file-snapshot"},
	}
	driver := &fallbackVolumeDriver{
		primary:        primary,
		fallback:       fallback,
		shouldFallback: shouldFallbackFromAPFS,
	}

	snapshot, snapshotDriver, storageDriver, err := snapshotDarwinVZCacheOutputVolume(context.Background(), driver, volumestore.SnapshotVolumeRequest{
		SnapshotID: "snap-1",
		VolumeRef:  "volume-ref",
	})
	if err != nil {
		t.Fatalf("snapshotDarwinVZCacheOutputVolume returned error: %v", err)
	}
	if got, want := snapshot.StorageRef, "file-snapshot"; got != want {
		t.Fatalf("unexpected snapshot storage ref: got %q want %q", got, want)
	}
	if snapshotDriver != fallback {
		t.Fatalf("expected fallback driver to own snapshot cleanup, got %#v", snapshotDriver)
	}
	if got, want := storageDriver, "file"; got != want {
		t.Fatalf("unexpected storage driver: got %q want %q", got, want)
	}
	if got, want := primary.snapshotCalls, 1; got != want {
		t.Fatalf("unexpected primary snapshot calls: got %d want %d", got, want)
	}
	if got, want := fallback.snapshotCalls, 1; got != want {
		t.Fatalf("unexpected fallback snapshot calls: got %d want %d", got, want)
	}
}

func testPreparedDarwinVZCacheOutputVolume(stage, blockName, cacheKey, volumeID, volumeRef string) preparedDarwinVZCacheOutputVolume {
	return preparedDarwinVZCacheOutputVolume{
		Spec: backend.CacheOutputVolumeSpec{
			Stage:     stage,
			BlockName: blockName,
			CacheKey:  cacheKey,
			VolumeID:  volumeID,
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
