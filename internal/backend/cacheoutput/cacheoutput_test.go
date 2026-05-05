package cacheoutput

import (
	"reflect"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/vsockexec"
)

func TestValidateVolumeSpecRejectsMissingOutputMappings(t *testing.T) {
	t.Parallel()

	err := ValidateVolumeSpec(backend.CacheOutputVolumeSpec{
		Stage:     "dependency-volume",
		BlockName: "toolchains",
		CacheKey:  "cache-key",
		VolumeID:  "volume-id",
	})
	if err == nil {
		t.Fatal("expected missing output mappings to fail")
	}
	if !strings.Contains(err.Error(), "missing output mappings") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMountsAndFileCapturesUsePreparedMounts(t *testing.T) {
	t.Parallel()

	volumes := []PreparedMount{
		{
			Spec: backend.CacheOutputVolumeSpec{
				VolumeID:          "volume-a",
				SourceSnapshotRef: "snapshot-a",
				DirMappings: []backend.CacheOutputDirMapping{
					{GuestPath: " /root/.cache/tool ", Subpath: " dirs/0 "},
				},
				FileMappings: []backend.CacheOutputFileMapping{
					{GuestPath: " /root/.config/tool.json ", Subpath: " files/0 ", Mode: 0o600},
				},
			},
			DevicePath: "/dev/vdb",
			MountPath:  "/run/cleanroom/cache-output-volumes/cacheout0",
		},
	}

	wantMounts := []vsockexec.CacheOutputMount{
		{
			DevicePath:    "/dev/vdb",
			MountPath:     "/run/cleanroom/cache-output-volumes/cacheout0",
			SourcePresent: true,
			DirMappings: []vsockexec.CacheOutputDirMount{
				{GuestPath: "/root/.cache/tool", Subpath: "dirs/0"},
			},
			FileMappings: []vsockexec.CacheOutputFileMount{
				{GuestPath: "/root/.config/tool.json", Subpath: "files/0", Mode: 0o600},
			},
		},
	}
	if got := Mounts(volumes); !reflect.DeepEqual(got, wantMounts) {
		t.Fatalf("unexpected mounts: got %#v want %#v", got, wantMounts)
	}

	captures, err := FileCaptures(volumes, []backend.CacheOutputFileCapture{
		{VolumeID: "volume-a", GuestPath: " /root/.config/tool.json ", VolumeSubpath: " files/0 ", Mode: 0o600},
	})
	if err != nil {
		t.Fatalf("FileCaptures returned error: %v", err)
	}
	wantCaptures := []vsockexec.CacheOutputFileCapture{
		{
			GuestPath: "/root/.config/tool.json",
			MountPath: "/run/cleanroom/cache-output-volumes/cacheout0",
			Subpath:   "files/0",
			Mode:      0o600,
		},
	}
	if !reflect.DeepEqual(captures, wantCaptures) {
		t.Fatalf("unexpected captures: got %#v want %#v", captures, wantCaptures)
	}
}

func TestSelectByVolumeIDRejectsDuplicateRequests(t *testing.T) {
	t.Parallel()

	_, err := SelectByVolumeID(
		[]backend.CacheOutputVolumeSpec{{VolumeID: "volume-a"}},
		[]string{"volume-a", "volume-a"},
		func(spec backend.CacheOutputVolumeSpec) string { return spec.VolumeID },
	)
	if err == nil {
		t.Fatal("expected duplicate volume ids to fail")
	}
	if !strings.Contains(err.Error(), `duplicate cache output volume id "volume-a"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}
