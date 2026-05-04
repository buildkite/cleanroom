//go:build darwin

package darwinvz

import (
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
