package controlservice

import (
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/cachestore"
	"github.com/buildkite/cleanroom/internal/policy"
)

func TestDependencyBlockVolumeOutputSpecsBuildsHitAndMissSpecs(t *testing.T) {
	plan := dependencyBlockVolumePlan{
		Blocks: []dependencyBlockVolumeBlockPlan{
			{
				BlockName: "toolchains",
				CacheKey:  "dependency-volume:v1:toolchains",
				Outputs: policy.StageBlockOutputs{
					Dirs:  []string{"/root/.local/share/mise"},
					Files: []string{"/root/.config/mise/config.toml"},
				},
				CacheHit: true,
				CacheRecord: cachestore.Record{
					OutputRecords: []cachestore.OutputRecord{
						{
							Kind:          "dir",
							Path:          "/root/.local/share/mise",
							VolumeSubpath: "dirs/0",
							StorageDriver: "zfs",
							StorageRef:    "pool/cleanroom/toolchains",
							SnapshotRef:   "pool/cleanroom/toolchains@snap",
						},
						{
							Kind:          "file",
							Path:          "/root/.config/mise/config.toml",
							VolumeSubpath: "files/0",
							StorageDriver: "zfs",
							StorageRef:    "pool/cleanroom/toolchains",
							SnapshotRef:   "pool/cleanroom/toolchains@snap",
						},
					},
				},
			},
			{
				BlockName:               "go-modules",
				CacheKey:                "dependency-volume:v1:go-modules",
				CacheOutputMinimumBytes: 20 << 30,
				Outputs: policy.StageBlockOutputs{
					Dirs:  []string{"/root/go/pkg/mod"},
					Files: []string{"/root/.cache/go-build/README"},
				},
			},
		},
	}

	specs, err := dependencyBlockVolumeOutputSpecs(plan)
	if err != nil {
		t.Fatalf("dependencyBlockVolumeOutputSpecs returned error: %v", err)
	}
	if got, want := len(specs), 2; got != want {
		t.Fatalf("unexpected spec count: got %d want %d", got, want)
	}

	hit := requireCacheOutputVolumeSpec(t, specs, dependencyVolumeStageName, "toolchains")
	if got, want := hit.CacheKey, "dependency-volume:v1:toolchains"; got != want {
		t.Fatalf("unexpected hit cache key: got %q want %q", got, want)
	}
	if got, want := hit.StorageDriver, "zfs"; got != want {
		t.Fatalf("unexpected hit storage driver: got %q want %q", got, want)
	}
	if got, want := hit.StorageRef, "pool/cleanroom/toolchains"; got != want {
		t.Fatalf("unexpected hit storage ref: got %q want %q", got, want)
	}
	if got, want := hit.SourceSnapshotRef, "pool/cleanroom/toolchains@snap"; got != want {
		t.Fatalf("unexpected hit source snapshot ref: got %q want %q", got, want)
	}
	if got, want := hit.DirMappings[0], (backend.CacheOutputDirMapping{GuestPath: "/root/.local/share/mise", Subpath: "dirs/0"}); got != want {
		t.Fatalf("unexpected hit dir mapping: got %#v want %#v", got, want)
	}
	if got, want := hit.FileMappings[0].GuestPath, "/root/.config/mise/config.toml"; got != want {
		t.Fatalf("unexpected hit file guest path: got %q want %q", got, want)
	}
	if got, want := hit.FileMappings[0].Subpath, "files/0"; got != want {
		t.Fatalf("unexpected hit file subpath: got %q want %q", got, want)
	}

	miss := requireCacheOutputVolumeSpec(t, specs, dependencyVolumeStageName, "go-modules")
	if miss.StorageDriver != "" || miss.StorageRef != "" || miss.SourceSnapshotRef != "" {
		t.Fatalf("expected miss spec without source storage, got driver=%q storage=%q snapshot=%q", miss.StorageDriver, miss.StorageRef, miss.SourceSnapshotRef)
	}
	if got, want := miss.DirMappings[0], (backend.CacheOutputDirMapping{GuestPath: "/root/go/pkg/mod", Subpath: "dirs/0"}); got != want {
		t.Fatalf("unexpected miss dir mapping: got %#v want %#v", got, want)
	}
	if got, want := miss.FileMappings[0].Subpath, "files/0"; got != want {
		t.Fatalf("unexpected miss file subpath: got %q want %q", got, want)
	}
	if got, want := miss.MinimumBytes, int64(20<<30); got != want {
		t.Fatalf("unexpected miss minimum bytes: got %d want %d", got, want)
	}
	if hit.VolumeID == "" || miss.VolumeID == "" || hit.VolumeID == miss.VolumeID {
		t.Fatalf("expected stable distinct volume IDs, got hit=%q miss=%q", hit.VolumeID, miss.VolumeID)
	}
}

func requireCacheOutputVolumeSpec(t *testing.T, specs []backend.CacheOutputVolumeSpec, stage, blockName string) backend.CacheOutputVolumeSpec {
	t.Helper()
	for _, spec := range specs {
		if spec.Stage == stage && spec.BlockName == blockName {
			return spec
		}
	}
	t.Fatalf("missing cache output volume spec for %s/%s in %#v", stage, blockName, specs)
	return backend.CacheOutputVolumeSpec{}
}
