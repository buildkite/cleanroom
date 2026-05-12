package firecracker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/ext4image"
	"github.com/buildkite/cleanroom/internal/volumestore"
	"github.com/buildkite/cleanroom/internal/vsockexec"
)

func TestCapabilitiesAdvertiseCacheOutputVolumesWithOverlayCapture(t *testing.T) {
	t.Parallel()

	caps := (&Adapter{}).Capabilities()
	if !caps[backend.CapabilitySandboxCacheOutputVolumes] {
		t.Fatalf("expected %s capability", backend.CapabilitySandboxCacheOutputVolumes)
	}
	if !caps[backend.CapabilitySandboxOverlayWriteCapture] {
		t.Fatalf("expected %s capability", backend.CapabilitySandboxOverlayWriteCapture)
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

func TestCreateEmptyCacheOutputExt4ImageCreatesMissingFile(t *testing.T) {
	tmpDir := t.TempDir()
	mkfsPath := filepath.Join(tmpDir, "mkfs.ext4")
	if err := os.WriteFile(mkfsPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake mkfs.ext4: %v", err)
	}
	t.Setenv("PATH", tmpDir)

	imagePath := filepath.Join(tmpDir, "nested", "cache-output-empty-base.ext4")
	if err := createEmptyCacheOutputExt4Image(context.Background(), imagePath, 1); err != nil {
		t.Fatalf("createEmptyCacheOutputExt4Image returned error: %v", err)
	}
	info, err := os.Stat(imagePath)
	if err != nil {
		t.Fatalf("stat image: %v", err)
	}
	if got, want := info.Size(), ext4image.AlignBytes(1); got != want {
		t.Fatalf("unexpected image size: got %d want %d", got, want)
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

func TestPrepareCacheOutputVolumesCfgMinimumBytesOverridesPackageDefault(t *testing.T) {
	runDir := t.TempDir()
	prevDriverFn := rootFSVolumeStoreDriverFn
	prevCreateEmpty := createEmptyCacheOutputExt4ImageFn
	prevMinimumBytes := cacheOutputVolumeMinimumBytes
	t.Cleanup(func() {
		rootFSVolumeStoreDriverFn = prevDriverFn
		createEmptyCacheOutputExt4ImageFn = prevCreateEmpty
		cacheOutputVolumeMinimumBytes = prevMinimumBytes
	})
	cacheOutputVolumeMinimumBytes = 1024

	var capturedMinimumBytes int64
	createEmptyCacheOutputExt4ImageFn = func(_ context.Context, path string, minimumBytes int64) error {
		capturedMinimumBytes = minimumBytes
		return os.WriteFile(path, []byte("empty-ext4"), 0o644)
	}

	rootFSVolumeStoreDriverFn = func(backend.FirecrackerConfig) (volumestore.Driver, error) {
		return resizingTestVolumeDriver{
			ensureBaseVolumeFn: func(_ context.Context, req volumestore.EnsureBaseVolumeRequest) (volumestore.BaseVolume, error) {
				return volumestore.BaseVolume{Ref: "base:" + filepath.Base(req.SourcePath)}, nil
			},
			createWritableVolumeFn: func(_ context.Context, req volumestore.CreateWritableVolumeRequest) (volumestore.WritableVolume, error) {
				return volumestore.WritableVolume{
					Ref:            "volume:" + req.VolumeID,
					AttachmentPath: req.AttachmentPath,
				}, nil
			},
		}, nil
	}

	cfg := backend.FirecrackerConfig{
		MinimumCacheOutputVolumeBytes: 16 << 30,
	}
	specs := []backend.CacheOutputVolumeSpec{
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

	_, cleanup, err := prepareCacheOutputVolumes(context.Background(), nil, cfg, "sandbox-1", runDir, specs)
	if err != nil {
		t.Fatalf("prepareCacheOutputVolumes returned error: %v", err)
	}
	defer cleanup()

	if got, want := capturedMinimumBytes, int64(16<<30); got != want {
		t.Fatalf("minimumBytes passed to createEmptyCacheOutputExt4ImageFn: got %d want %d", got, want)
	}
}

type resizingTestVolumeDriver struct {
	ensureBaseVolumeFn     func(context.Context, volumestore.EnsureBaseVolumeRequest) (volumestore.BaseVolume, error)
	createWritableVolumeFn func(context.Context, volumestore.CreateWritableVolumeRequest) (volumestore.WritableVolume, error)
}

func (d resizingTestVolumeDriver) Name() string { return "resizing-test" }

func (d resizingTestVolumeDriver) EnsureBaseVolume(ctx context.Context, req volumestore.EnsureBaseVolumeRequest) (volumestore.BaseVolume, error) {
	if d.ensureBaseVolumeFn == nil {
		return volumestore.BaseVolume{}, errors.New("unexpected EnsureBaseVolume call")
	}
	return d.ensureBaseVolumeFn(ctx, req)
}

func (d resizingTestVolumeDriver) CreateWritableVolume(ctx context.Context, req volumestore.CreateWritableVolumeRequest) (volumestore.WritableVolume, error) {
	if d.createWritableVolumeFn == nil {
		return volumestore.WritableVolume{}, errors.New("unexpected CreateWritableVolume call")
	}
	return d.createWritableVolumeFn(ctx, req)
}

func (d resizingTestVolumeDriver) SnapshotVolume(_ context.Context, _ volumestore.SnapshotVolumeRequest) (volumestore.Snapshot, error) {
	return volumestore.Snapshot{}, errors.New("unexpected SnapshotVolume call")
}

func (d resizingTestVolumeDriver) CloneSnapshotToVolume(_ context.Context, _ volumestore.CloneSnapshotToVolumeRequest) (volumestore.WritableVolume, error) {
	return volumestore.WritableVolume{}, errors.New("unexpected CloneSnapshotToVolume call")
}

func (d resizingTestVolumeDriver) DestroyVolume(_ context.Context, _ volumestore.DestroyVolumeRequest) error {
	return nil
}

func (d resizingTestVolumeDriver) DestroySnapshot(_ context.Context, _ volumestore.DestroySnapshotRequest) error {
	return nil
}

func (d resizingTestVolumeDriver) EnsureWritableVolumeMinimumSize(_ context.Context, _ volumestore.WritableVolume, _ int64) error {
	return nil
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
