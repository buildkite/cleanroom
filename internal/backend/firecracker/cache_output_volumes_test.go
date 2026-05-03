package firecracker

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/volumestore"
	"github.com/buildkite/cleanroom/internal/vsockexec"
)

func TestCapabilitiesAdvertiseCacheOutputVolumesWithoutOverlayCapture(t *testing.T) {
	t.Parallel()

	caps := (&Adapter{}).Capabilities()
	if !caps[backend.CapabilitySandboxCacheOutputVolumes] {
		t.Fatalf("expected %s capability", backend.CapabilitySandboxCacheOutputVolumes)
	}
	if caps[backend.CapabilitySandboxOverlayWriteCapture] {
		t.Fatalf("did not expect %s capability before overlay execution is implemented", backend.CapabilitySandboxOverlayWriteCapture)
	}
}

func TestPrepareCacheOutputVolumesClonesHitsAndCreatesMisses(t *testing.T) {
	runDir := t.TempDir()
	prevDriverFn := rootFSVolumeStoreDriverFn
	prevCreateEmpty := createEmptyCacheOutputExt4ImageFn
	prevMinimumBytes := cacheOutputVolumeMinimumBytes
	t.Cleanup(func() {
		rootFSVolumeStoreDriverFn = prevDriverFn
		createEmptyCacheOutputExt4ImageFn = prevCreateEmpty
		cacheOutputVolumeMinimumBytes = prevMinimumBytes
	})
	cacheOutputVolumeMinimumBytes = 0

	var cloneReqs []volumestore.CloneSnapshotToVolumeRequest
	var createReqs []volumestore.CreateWritableVolumeRequest
	var destroyReqs []volumestore.DestroyVolumeRequest
	rootFSVolumeStoreDriverFn = func(backend.FirecrackerConfig) (volumestore.Driver, error) {
		return testVolumeDriver{
			ensureBaseVolumeFn: func(_ context.Context, req volumestore.EnsureBaseVolumeRequest) (volumestore.BaseVolume, error) {
				if strings.TrimSpace(req.SourcePath) == "" {
					t.Fatal("expected source path for empty cache output base volume")
				}
				return volumestore.BaseVolume{Ref: "base:" + filepath.Base(req.SourcePath)}, nil
			},
			createWritableVolumeFn: func(_ context.Context, req volumestore.CreateWritableVolumeRequest) (volumestore.WritableVolume, error) {
				createReqs = append(createReqs, req)
				return volumestore.WritableVolume{
					Ref:            "volume:" + req.VolumeID,
					AttachmentPath: req.AttachmentPath,
				}, nil
			},
			cloneSnapshotToVolumeFn: func(_ context.Context, req volumestore.CloneSnapshotToVolumeRequest) (volumestore.WritableVolume, error) {
				cloneReqs = append(cloneReqs, req)
				return volumestore.WritableVolume{
					Ref:            "volume:" + req.VolumeID,
					AttachmentPath: req.AttachmentPath,
				}, nil
			},
			destroyVolumeFn: func(_ context.Context, req volumestore.DestroyVolumeRequest) error {
				destroyReqs = append(destroyReqs, req)
				return nil
			},
		}, nil
	}
	createEmptyCacheOutputExt4ImageFn = func(_ context.Context, path string, minimumBytes int64) error {
		if got, want := minimumBytes, cacheOutputVolumeMinimumBytes; got != want {
			t.Fatalf("unexpected empty volume minimum bytes: got %d want %d", got, want)
		}
		return os.WriteFile(path, []byte("empty-ext4"), 0o644)
	}

	specs := []backend.CacheOutputVolumeSpec{
		{
			Stage:             "dependency-volume",
			BlockName:         "toolchains",
			CacheKey:          "dependency-volume:v1:toolchains",
			VolumeID:          "dependency-volume-abc123",
			SourceSnapshotRef: "snapshot:toolchains",
			StorageDriver:     "file",
			StorageRef:        "snapshot:toolchains",
			DirMappings: []backend.CacheOutputDirMapping{
				{GuestPath: "/root/.local/share/mise", Subpath: "dirs/0"},
			},
		},
		{
			Stage:     "service-volume",
			BlockName: "postgres",
			CacheKey:  "service-volume:v1:postgres",
			VolumeID:  "service-volume-def456",
			DirMappings: []backend.CacheOutputDirMapping{
				{GuestPath: "/var/lib/cleanroom/services/postgres", Subpath: "dirs/0"},
			},
		},
	}

	prepared, cleanup, err := prepareCacheOutputVolumes(context.Background(), nil, backend.FirecrackerConfig{}, "sandbox-1", runDir, specs)
	if err != nil {
		t.Fatalf("prepareCacheOutputVolumes returned error: %v", err)
	}
	if got, want := len(prepared), 2; got != want {
		t.Fatalf("unexpected prepared volume count: got %d want %d", got, want)
	}
	if got, want := len(cloneReqs), 1; got != want {
		t.Fatalf("unexpected clone request count: got %d want %d", got, want)
	}
	if got, want := cloneReqs[0].SnapshotRef, "snapshot:toolchains"; got != want {
		t.Fatalf("unexpected clone snapshot ref: got %q want %q", got, want)
	}
	if got, want := len(createReqs), 1; got != want {
		t.Fatalf("unexpected create request count: got %d want %d", got, want)
	}
	if got, want := createReqs[0].BaseRef, "base:cache-output-empty-base.ext4"; got != want {
		t.Fatalf("unexpected miss base ref: got %q want suffix %q", createReqs[0].BaseRef, want)
	}
	wantDrives := []drive{
		{DriveID: "cacheout0", PathOnHost: filepath.Join(runDir, "cache-output-00.ext4")},
		{DriveID: "cacheout1", PathOnHost: filepath.Join(runDir, "cache-output-01.ext4")},
	}
	gotDrives := cacheOutputVolumeDrives(prepared)
	for i := range gotDrives {
		gotDrives[i].IsReadOnly = false
		gotDrives[i].IsRootDevice = false
	}
	if !reflect.DeepEqual(gotDrives, wantDrives) {
		t.Fatalf("unexpected cache output drives: got %#v want %#v", gotDrives, wantDrives)
	}

	cleanup()
	if got, want := len(destroyReqs), 2; got != want {
		t.Fatalf("unexpected destroy request count: got %d want %d", got, want)
	}
	if got, want := destroyReqs[0].VolumeRef, prepared[1].Volume.Ref; got != want {
		t.Fatalf("expected reverse cleanup order first ref %q, got %q", want, got)
	}
}

func TestCacheOutputVolumeMountsBuildsDeterministicGuestPlan(t *testing.T) {
	t.Parallel()

	mounts := cacheOutputVolumeMounts([]preparedCacheOutputVolume{
		{
			Spec: backend.CacheOutputVolumeSpec{
				SourceSnapshotRef: "snapshot:toolchains",
				StorageRef:        "snapshot:toolchains",
				DirMappings: []backend.CacheOutputDirMapping{
					{GuestPath: " /root/.local/share/mise ", Subpath: " dirs/0 "},
				},
				FileMappings: []backend.CacheOutputFileMapping{
					{GuestPath: " /root/.config/mise/config.toml ", Subpath: " files/0 ", Mode: 0o600},
				},
			},
			Drive: drive{DriveID: "cacheout0"},
		},
		{
			Spec: backend.CacheOutputVolumeSpec{
				DirMappings: []backend.CacheOutputDirMapping{
					{GuestPath: "/root/go/pkg/mod", Subpath: "dirs/0"},
				},
			},
			Drive: drive{DriveID: "cacheout1"},
		},
	})

	want := []vsockexec.CacheOutputMount{
		{
			DevicePath:    "/dev/vdb",
			MountPath:     "/run/cleanroom/cache-output-volumes/cacheout0",
			SourcePresent: true,
			DirMappings: []vsockexec.CacheOutputDirMount{
				{GuestPath: "/root/.local/share/mise", Subpath: "dirs/0"},
			},
			FileMappings: []vsockexec.CacheOutputFileMount{
				{GuestPath: "/root/.config/mise/config.toml", Subpath: "files/0", Mode: 0o600},
			},
		},
		{
			DevicePath: "/dev/vdc",
			MountPath:  "/run/cleanroom/cache-output-volumes/cacheout1",
			DirMappings: []vsockexec.CacheOutputDirMount{
				{GuestPath: "/root/go/pkg/mod", Subpath: "dirs/0"},
			},
		},
	}
	if !reflect.DeepEqual(mounts, want) {
		t.Fatalf("unexpected cache output mounts: got %#v want %#v", mounts, want)
	}
}

func TestCacheOutputDevicePathUsesVirtioBlockOrderAfterRoot(t *testing.T) {
	t.Parallel()

	tests := map[int]string{
		0:  "/dev/vdb",
		1:  "/dev/vdc",
		24: "/dev/vdz",
		25: "/dev/vdaa",
	}
	for index, want := range tests {
		if got := cacheOutputDevicePath(index); got != want {
			t.Fatalf("cacheOutputDevicePath(%d) = %q, want %q", index, got, want)
		}
	}
}

func TestPrepareCacheOutputVolumesRejectsMalformedSpecs(t *testing.T) {
	t.Parallel()

	_, _, err := prepareCacheOutputVolumes(context.Background(), nil, backend.FirecrackerConfig{}, "sandbox-1", t.TempDir(), []backend.CacheOutputVolumeSpec{
		{
			Stage:     "dependency-volume",
			BlockName: "toolchains",
			CacheKey:  "dependency-volume:v1:toolchains",
			VolumeID:  "dependency-volume-abc123",
		},
	})
	if err == nil {
		t.Fatal("expected malformed spec to fail")
	}
	if !strings.Contains(err.Error(), "missing output mappings") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPrepareCacheOutputVolumesCleansUpOnFailure(t *testing.T) {
	runDir := t.TempDir()
	prevDriverFn := rootFSVolumeStoreDriverFn
	prevMinimumBytes := cacheOutputVolumeMinimumBytes
	t.Cleanup(func() {
		rootFSVolumeStoreDriverFn = prevDriverFn
		cacheOutputVolumeMinimumBytes = prevMinimumBytes
	})
	cacheOutputVolumeMinimumBytes = 0

	var destroyReqs []volumestore.DestroyVolumeRequest
	rootFSVolumeStoreDriverFn = func(backend.FirecrackerConfig) (volumestore.Driver, error) {
		return testVolumeDriver{
			cloneSnapshotToVolumeFn: func(_ context.Context, req volumestore.CloneSnapshotToVolumeRequest) (volumestore.WritableVolume, error) {
				if strings.Contains(req.SnapshotRef, "bad") {
					return volumestore.WritableVolume{}, errors.New("clone failed")
				}
				return volumestore.WritableVolume{
					Ref:            "volume:" + req.VolumeID,
					AttachmentPath: req.AttachmentPath,
				}, nil
			},
			destroyVolumeFn: func(_ context.Context, req volumestore.DestroyVolumeRequest) error {
				destroyReqs = append(destroyReqs, req)
				return nil
			},
		}, nil
	}

	_, _, err := prepareCacheOutputVolumes(context.Background(), nil, backend.FirecrackerConfig{}, "sandbox-1", runDir, []backend.CacheOutputVolumeSpec{
		{
			Stage:             "dependency-volume",
			BlockName:         "toolchains",
			CacheKey:          "dependency-volume:v1:toolchains",
			VolumeID:          "dependency-volume-abc123",
			SourceSnapshotRef: "snapshot:good",
			DirMappings: []backend.CacheOutputDirMapping{
				{GuestPath: "/root/.local/share/mise", Subpath: "dirs/0"},
			},
		},
		{
			Stage:             "dependency-volume",
			BlockName:         "go-modules",
			CacheKey:          "dependency-volume:v1:go-modules",
			VolumeID:          "dependency-volume-def456",
			SourceSnapshotRef: "snapshot:bad",
			DirMappings: []backend.CacheOutputDirMapping{
				{GuestPath: "/root/go/pkg/mod", Subpath: "dirs/0"},
			},
		},
	})
	if err == nil {
		t.Fatal("expected prepareCacheOutputVolumes to fail")
	}
	if !strings.Contains(err.Error(), "clone failed") {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := len(destroyReqs), 1; got != want {
		t.Fatalf("expected prepared volume cleanup, got %d destroy requests want %d", got, want)
	}
}

func TestSnapshotCacheOutputVolumesSnapshotsPreparedVolumes(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	volumePath := filepath.Join(t.TempDir(), "output-volume.ext4")
	volumeBytes := []byte("dependency output volume bytes")
	if err := os.WriteFile(volumePath, volumeBytes, 0o644); err != nil {
		t.Fatalf("write output volume: %v", err)
	}

	var signals []syscall.Signal
	prevSignal := sendProcessSignal
	sendProcessSignal = func(_ *os.Process, sig syscall.Signal) error {
		signals = append(signals, sig)
		return nil
	}
	t.Cleanup(func() { sendProcessSignal = prevSignal })

	adapter := &Adapter{
		runGuestCommandFn: func(_ context.Context, _ context.Context, _ <-chan struct{}, _ func() error, _ string, _ uint32, req vsockexec.ExecRequest, _ backend.OutputStream) (vsockexec.ExecResponse, guestExecTiming, error) {
			if len(req.Command) != 1 || req.Command[0] != "sync" {
				t.Fatalf("unexpected command: %v", req.Command)
			}
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
					{
						Spec: backend.CacheOutputVolumeSpec{
							Stage:     "dependency-volume",
							BlockName: "go-modules",
							CacheKey:  "dependency-volume:v1:test",
							VolumeID:  "volume-test",
						},
						Volume: volumestore.WritableVolume{
							Ref:            volumePath,
							AttachmentPath: volumePath,
						},
					},
				},
			},
		},
	}

	result, err := adapter.SnapshotCacheOutputVolumes(context.Background(), backend.SnapshotCacheOutputVolumesRequest{
		SandboxID: "cr-test",
		Volumes: []backend.CacheOutputVolumeSnapshotRequest{
			{
				Stage:      "dependency-volume",
				BlockName:  "go-modules",
				CacheKey:   "dependency-volume:v1:test",
				VolumeID:   "volume-test",
				SnapshotID: "snapshot-test",
			},
		},
		FirecrackerConfig: backend.FirecrackerConfig{
			Snapshots: backend.SnapshotConfig{Enabled: true},
		},
	})
	if err != nil {
		t.Fatalf("SnapshotCacheOutputVolumes returned error: %v", err)
	}
	if got, want := len(result.Volumes), 1; got != want {
		t.Fatalf("unexpected snapshot count: got %d want %d", got, want)
	}
	snapshot := result.Volumes[0]
	if got, want := snapshot.StorageDriver, "file"; got != want {
		t.Fatalf("unexpected storage driver: got %q want %q", got, want)
	}
	if got, want := snapshot.StorageRef, filepath.Join(stateHome, "cleanroom", "snapshots", "firecracker", "snapshot-test", "rootfs.ext4"); got != want {
		t.Fatalf("unexpected storage ref: got %q want %q", got, want)
	}
	if got, want := snapshot.Stage, "dependency-volume"; got != want {
		t.Fatalf("unexpected stage: got %q want %q", got, want)
	}
	if got, want := snapshot.BlockName, "go-modules"; got != want {
		t.Fatalf("unexpected block name: got %q want %q", got, want)
	}
	if got, want := snapshot.VolumeID, "volume-test"; got != want {
		t.Fatalf("unexpected volume id: got %q want %q", got, want)
	}
	data, err := os.ReadFile(snapshot.StorageRef)
	if err != nil {
		t.Fatalf("read output volume snapshot: %v", err)
	}
	if !bytes.Equal(data, volumeBytes) {
		t.Fatalf("snapshot bytes mismatch: got %q want %q", data, volumeBytes)
	}
	if want := []syscall.Signal{syscall.SIGSTOP, syscall.SIGCONT}; !reflect.DeepEqual(signals, want) {
		t.Fatalf("unexpected signals: got %v want %v", signals, want)
	}
}

func testCacheOutputVolumeSpecs() []backend.CacheOutputVolumeSpec {
	return []backend.CacheOutputVolumeSpec{
		{
			Stage:             "dependency-volume",
			BlockName:         "toolchains",
			CacheKey:          "dependency-volume:v1:toolchains",
			VolumeID:          "dependency-volume-abc123",
			SourceSnapshotRef: "snapshot:toolchains",
			StorageDriver:     "file",
			StorageRef:        "snapshot:toolchains",
			DirMappings: []backend.CacheOutputDirMapping{
				{GuestPath: "/root/.local/share/mise", Subpath: "dirs/0"},
			},
		},
	}
}
