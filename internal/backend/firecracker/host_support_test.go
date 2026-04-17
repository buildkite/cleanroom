package firecracker

import (
	"context"
	"reflect"
	"testing"
)

func TestDiscoverCleanroomZFSDatasetRootsAcceptsDedicatedCleanroomPool(t *testing.T) {
	prev := hostSupportCommandOutput
	hostSupportCommandOutput = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("cleanroom\ncleanroom/data\ncleanroom/data/snapshots\n"), nil
	}
	t.Cleanup(func() {
		hostSupportCommandOutput = prev
	})

	got, err := discoverCleanroomZFSDatasetRoots(context.Background(), "/usr/sbin/zfs")
	if err != nil {
		t.Fatalf("discoverCleanroomZFSDatasetRoots returned error: %v", err)
	}
	if want := []string{"cleanroom"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected dataset roots: got %v want %v", got, want)
	}
}

func TestIsCleanroomZFSDatasetRoot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		dataset string
		want    bool
	}{
		{name: "dedicated cleanroom pool", dataset: "cleanroom", want: true},
		{name: "nested cleanroom dataset", dataset: "tank/cleanroom", want: true},
		{name: "cleanroom child dataset", dataset: "cleanroom/data", want: false},
		{name: "non cleanroom dataset", dataset: "tank/data", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCleanroomZFSDatasetRoot(tt.dataset); got != tt.want {
				t.Fatalf("isCleanroomZFSDatasetRoot(%q) = %t, want %t", tt.dataset, got, tt.want)
			}
		})
	}
}
