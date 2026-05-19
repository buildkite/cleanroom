//go:build darwin

package darwinvz

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/volumestore"
	"github.com/buildkite/cleanroom/internal/vsockexec"
)

func TestDarwinVZCacheOutputVolumeMountsBuildsDeterministicGuestPlan(t *testing.T) {
	t.Parallel()

	mounts := darwinVZCacheOutputVolumeMounts([]preparedDarwinVZCacheOutputVolume{
		{
			Spec: backend.CacheOutputVolumeSpec{
				Stage:             "dependency-volume",
				BlockName:         "toolchains",
				CacheKey:          "dependency-volume:v1:toolchains",
				VolumeID:          "dependency-volume-abc123",
				SourceSnapshotRef: "snapshot-ref",
				DirMappings: []backend.CacheOutputDirMapping{
					{GuestPath: "/root/.local/share/mise", Subpath: "dirs/0"},
				},
				FileMappings: []backend.CacheOutputFileMapping{
					{GuestPath: "/root/.config/mise/config.toml", Subpath: "files/0", Mode: 0o600},
				},
			},
			Volume:     volumestore.WritableVolume{AttachmentPath: "/tmp/cache-output-00.ext4"},
			DevicePath: "/dev/vdb",
			MountPath:  "/run/cleanroom/cache-output-volumes/cacheout0",
		},
		{
			Spec: backend.CacheOutputVolumeSpec{
				Stage:     "service-volume",
				BlockName: "postgres",
				CacheKey:  "service-volume:v1:postgres",
				VolumeID:  "service-volume-def456",
				DirMappings: []backend.CacheOutputDirMapping{
					{GuestPath: "/var/lib/postgresql/data", Subpath: "dirs/0"},
				},
			},
			Volume:     volumestore.WritableVolume{AttachmentPath: "/tmp/cache-output-01.ext4"},
			DevicePath: "/dev/vdc",
			MountPath:  "/run/cleanroom/cache-output-volumes/cacheout1",
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
			DevicePath:    "/dev/vdc",
			MountPath:     "/run/cleanroom/cache-output-volumes/cacheout1",
			SourcePresent: false,
			DirMappings: []vsockexec.CacheOutputDirMount{
				{GuestPath: "/var/lib/postgresql/data", Subpath: "dirs/0"},
			},
		},
	}
	if !reflect.DeepEqual(mounts, want) {
		t.Fatalf("unexpected cache output mounts: got %#v want %#v", mounts, want)
	}
}

func TestDarwinVZCacheOutputDiskPathsPreserveLaunchOrder(t *testing.T) {
	t.Parallel()

	paths := darwinVZCacheOutputDiskPaths([]preparedDarwinVZCacheOutputVolume{
		{Volume: volumestore.WritableVolume{AttachmentPath: "/tmp/cache-output-00.ext4"}},
		{Volume: volumestore.WritableVolume{AttachmentPath: "/tmp/cache-output-01.ext4"}},
	})

	want := []string{"/tmp/cache-output-00.ext4", "/tmp/cache-output-01.ext4"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("unexpected cache output disk paths: got %#v want %#v", paths, want)
	}
}

func TestDarwinVZCacheOutputFileCapturesUsePreparedMountPath(t *testing.T) {
	t.Parallel()

	captures, err := darwinVZCacheOutputFileCaptures([]preparedDarwinVZCacheOutputVolume{
		{
			Spec:      backend.CacheOutputVolumeSpec{VolumeID: "volume-a"},
			MountPath: "/run/cleanroom/cache-output-volumes/cacheout0",
		},
	}, []backend.CacheOutputFileCapture{
		{
			VolumeID:      "volume-a",
			GuestPath:     "/root/.config/tool/index.json",
			VolumeSubpath: "files/0",
			Mode:          0o600,
		},
	})
	if err != nil {
		t.Fatalf("darwinVZCacheOutputFileCaptures returned error: %v", err)
	}
	want := []vsockexec.CacheOutputFileCapture{
		{
			GuestPath: "/root/.config/tool/index.json",
			MountPath: "/run/cleanroom/cache-output-volumes/cacheout0",
			Subpath:   "files/0",
			Mode:      0o600,
		},
	}
	if !reflect.DeepEqual(captures, want) {
		t.Fatalf("unexpected cache output file captures: got %#v want %#v", captures, want)
	}
}

func TestDarwinVZCacheOutputDevicePathSkipsRootDisk(t *testing.T) {
	t.Parallel()

	for index, want := range []string{"/dev/vdb", "/dev/vdc", "/dev/vdd"} {
		if got := darwinVZCacheOutputDevicePath(index); got != want {
			t.Fatalf("cacheOutputDevicePath(%d) = %q, want %q", index, got, want)
		}
	}
}

func TestPrepareDarwinVZCacheOutputVolumesCfgMinimumBytesOverridesPackageDefault(t *testing.T) {
	runDir := t.TempDir()
	prevCreateEmpty := createEmptyDarwinVZCacheOutputExt4ImageFn
	prevPrepareFn := prepareDarwinVZCacheOutputWritableVolumeFn
	prevMinimumBytes := darwinVZCacheOutputVolumeMinimumBytes
	t.Cleanup(func() {
		createEmptyDarwinVZCacheOutputExt4ImageFn = prevCreateEmpty
		prepareDarwinVZCacheOutputWritableVolumeFn = prevPrepareFn
		darwinVZCacheOutputVolumeMinimumBytes = prevMinimumBytes
	})
	darwinVZCacheOutputVolumeMinimumBytes = 1024

	var capturedEmptyMinimumBytes int64
	createEmptyDarwinVZCacheOutputExt4ImageFn = func(_ context.Context, path string, minimumBytes int64) error {
		capturedEmptyMinimumBytes = minimumBytes
		return os.WriteFile(path, []byte("empty-ext4"), 0o644)
	}

	var capturedWritableMinimumBytes int64
	prepareDarwinVZCacheOutputWritableVolumeFn = func(_ context.Context, _ backend.FirecrackerConfig, volumeID, attachmentPath, sourceRef string, minimumBytes int64) (volumestore.WritableVolume, func(), error) {
		capturedWritableMinimumBytes = minimumBytes
		if sourceRef == "" {
			return volumestore.WritableVolume{}, nil, errors.New("unexpected empty source ref")
		}
		return volumestore.WritableVolume{
			Ref:            "volume:" + volumeID,
			AttachmentPath: attachmentPath,
		}, func() {}, nil
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

	_, cleanup, err := prepareDarwinVZCacheOutputVolumes(context.Background(), cfg, "sandbox-1", runDir, specs)
	if err != nil {
		t.Fatalf("prepareDarwinVZCacheOutputVolumes returned error: %v", err)
	}
	defer cleanup()

	if got, want := capturedEmptyMinimumBytes, int64(16<<30); got != want {
		t.Fatalf("minimumBytes passed to createEmptyDarwinVZCacheOutputExt4ImageFn: got %d want %d", got, want)
	}
	if got, want := capturedWritableMinimumBytes, int64(16<<30); got != want {
		t.Fatalf("minimumBytes passed to prepareDarwinVZCacheOutputWritableVolumeFn: got %d want %d", got, want)
	}
}

func TestPrepareDarwinVZCacheOutputVolumesSpecMinimumBytesOverridesCfgMinimum(t *testing.T) {
	runDir := t.TempDir()
	prevCreateEmpty := createEmptyDarwinVZCacheOutputExt4ImageFn
	prevPrepareFn := prepareDarwinVZCacheOutputWritableVolumeFn
	t.Cleanup(func() {
		createEmptyDarwinVZCacheOutputExt4ImageFn = prevCreateEmpty
		prepareDarwinVZCacheOutputWritableVolumeFn = prevPrepareFn
	})

	var capturedEmptyMinimumBytes int64
	createEmptyDarwinVZCacheOutputExt4ImageFn = func(_ context.Context, path string, minimumBytes int64) error {
		capturedEmptyMinimumBytes = minimumBytes
		return os.WriteFile(path, []byte("empty-ext4"), 0o644)
	}
	var capturedWritableMinimumBytes int64
	prepareDarwinVZCacheOutputWritableVolumeFn = func(_ context.Context, _ backend.FirecrackerConfig, volumeID, attachmentPath, sourceRef string, minimumBytes int64) (volumestore.WritableVolume, func(), error) {
		capturedWritableMinimumBytes = minimumBytes
		return volumestore.WritableVolume{Ref: "volume:" + volumeID, AttachmentPath: attachmentPath}, func() {}, nil
	}

	_, cleanup, err := prepareDarwinVZCacheOutputVolumes(context.Background(), backend.FirecrackerConfig{
		MinimumCacheOutputVolumeBytes: 16 << 30,
	}, "sandbox-1", runDir, []backend.CacheOutputVolumeSpec{{
		Stage:        "service-volume",
		BlockName:    "docker-images",
		CacheKey:     "service-volume:v1:docker-images",
		VolumeID:     "service-volume-def456",
		MinimumBytes: 32 << 30,
		DirMappings: []backend.CacheOutputDirMapping{
			{GuestPath: "/var/lib/docker", Subpath: "dirs/0"},
		},
	}})
	if err != nil {
		t.Fatalf("prepareDarwinVZCacheOutputVolumes returned error: %v", err)
	}
	defer cleanup()

	if got, want := capturedEmptyMinimumBytes, int64(32<<30); got != want {
		t.Fatalf("minimumBytes passed to createEmptyDarwinVZCacheOutputExt4ImageFn: got %d want %d", got, want)
	}
	if got, want := capturedWritableMinimumBytes, int64(32<<30); got != want {
		t.Fatalf("minimumBytes passed to prepareDarwinVZCacheOutputWritableVolumeFn: got %d want %d", got, want)
	}
}
