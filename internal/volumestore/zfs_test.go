package volumestore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type zfsTestRunner struct {
	commands []string
	exists   map[string]bool
}

func (r *zfsTestRunner) Run(_ context.Context, command string, args ...string) error {
	r.commands = append(r.commands, strings.Join(append([]string{command}, args...), " "))
	if command != "zfs" || len(args) == 0 {
		return nil
	}

	switch args[0] {
	case "create":
		if len(args) == 5 {
			r.exists[args[4]] = true
		}
	case "snapshot":
		if len(args) == 2 {
			r.exists[args[1]] = true
		}
	case "clone":
		if len(args) == 4 {
			r.exists[args[3]] = true
		}
	case "destroy":
		if len(args) >= 2 {
			target := args[len(args)-1]
			for ref := range r.exists {
				if ref == target || strings.HasPrefix(ref, target+"@") || strings.HasPrefix(ref, target+"/") {
					delete(r.exists, ref)
				}
			}
		}
	}

	return nil
}

func (r *zfsTestRunner) Output(_ context.Context, command string, args ...string) ([]byte, error) {
	r.commands = append(r.commands, strings.Join(append([]string{command}, args...), " "))
	if command == "zfs" && len(args) == 5 && args[0] == "list" {
		ref := args[4]
		if r.exists[ref] {
			return []byte(ref + "\n"), nil
		}
		return nil, errors.New("cannot open dataset: dataset does not exist")
	}
	return nil, fmt.Errorf("unsupported output command: %s %s", command, strings.Join(args, " "))
}

func TestZFSDriverEnsureBaseVolumeAndCloneLifecycle(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "prepared.ext4")
	if err := os.WriteFile(sourcePath, []byte("base-bytes"), 0o644); err != nil {
		t.Fatalf("write source file: %v", err)
	}

	runner := &zfsTestRunner{exists: map[string]bool{}}
	driver, err := NewZFSDriver(ZFSDriverOptions{
		DatasetRoot: "tank/cleanroom",
		Runner:      runner,
	})
	if err != nil {
		t.Fatalf("NewZFSDriver returned error: %v", err)
	}

	base, err := driver.EnsureBaseVolume(context.Background(), EnsureBaseVolumeRequest{
		BaseID:     "runtime-key",
		SourcePath: sourcePath,
	})
	if err != nil {
		t.Fatalf("EnsureBaseVolume returned error: %v", err)
	}
	if got, want := base.Ref, "tank/cleanroom/base/runtime-key@seed"; got != want {
		t.Fatalf("unexpected base ref: got %q want %q", got, want)
	}

	volume, err := driver.CreateWritableVolume(context.Background(), CreateWritableVolumeRequest{
		VolumeID: "sandbox-1",
		BaseRef:  base.Ref,
	})
	if err != nil {
		t.Fatalf("CreateWritableVolume returned error: %v", err)
	}
	if got, want := volume.Ref, "tank/cleanroom/sandboxes/sandbox-1"; got != want {
		t.Fatalf("unexpected volume ref: got %q want %q", got, want)
	}
	if got, want := volume.AttachmentPath, "/dev/zvol/tank/cleanroom/sandboxes/sandbox-1"; got != want {
		t.Fatalf("unexpected attachment path: got %q want %q", got, want)
	}

	wantCommands := []string{
		"zfs list -H -o name tank/cleanroom/base/runtime-key",
		"zfs create -p -V 10 tank/cleanroom/base/runtime-key",
		"dd if=" + sourcePath + " of=/dev/zvol/tank/cleanroom/base/runtime-key bs=4M conv=fsync status=none",
		"zfs list -H -o name tank/cleanroom/base/runtime-key@seed",
		"zfs snapshot tank/cleanroom/base/runtime-key@seed",
		"zfs list -H -o name tank/cleanroom/sandboxes/sandbox-1",
		"zfs clone -p tank/cleanroom/base/runtime-key@seed tank/cleanroom/sandboxes/sandbox-1",
	}
	if got, want := runner.commands, wantCommands; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unexpected zfs commands:\n got: %v\nwant: %v", got, want)
	}
}

func TestZFSDriverSnapshotSurvivesSourceVolumeDestroy(t *testing.T) {
	runner := &zfsTestRunner{
		exists: map[string]bool{
			"tank/cleanroom/sandboxes/sandbox-1": true,
		},
	}
	driver, err := NewZFSDriver(ZFSDriverOptions{
		DatasetRoot: "tank/cleanroom",
		Runner:      runner,
	})
	if err != nil {
		t.Fatalf("NewZFSDriver returned error: %v", err)
	}

	snapshot, err := driver.SnapshotVolume(context.Background(), SnapshotVolumeRequest{
		SnapshotID: "golden",
		VolumeRef:  "tank/cleanroom/sandboxes/sandbox-1",
	})
	if err != nil {
		t.Fatalf("SnapshotVolume returned error: %v", err)
	}
	if got, want := snapshot.Ref, "tank/cleanroom/snapshots/golden@seed"; got != want {
		t.Fatalf("unexpected snapshot ref: got %q want %q", got, want)
	}

	if err := driver.DestroyVolume(context.Background(), DestroyVolumeRequest{VolumeRef: "tank/cleanroom/sandboxes/sandbox-1"}); err != nil {
		t.Fatalf("DestroyVolume returned error: %v", err)
	}

	clone, err := driver.CloneSnapshotToVolume(context.Background(), CloneSnapshotToVolumeRequest{
		VolumeID:    "sandbox-2",
		SnapshotRef: snapshot.Ref,
	})
	if err != nil {
		t.Fatalf("CloneSnapshotToVolume returned error: %v", err)
	}
	if got, want := clone.Ref, "tank/cleanroom/sandboxes/sandbox-2"; got != want {
		t.Fatalf("unexpected cloned volume ref: got %q want %q", got, want)
	}

	if err := driver.DestroySnapshot(context.Background(), DestroySnapshotRequest{SnapshotRef: snapshot.Ref}); err != nil {
		t.Fatalf("DestroySnapshot returned error: %v", err)
	}
	if err := driver.DestroyVolume(context.Background(), DestroyVolumeRequest{VolumeRef: clone.Ref}); err != nil {
		t.Fatalf("DestroyVolume returned error: %v", err)
	}

	wantCommands := []string{
		"zfs list -H -o name tank/cleanroom/snapshots/golden",
		"zfs snapshot tank/cleanroom/sandboxes/sandbox-1@snap-golden",
		"zfs list -H -o name tank/cleanroom/snapshots/golden",
		"zfs clone -p tank/cleanroom/sandboxes/sandbox-1@snap-golden tank/cleanroom/snapshots/golden",
		"zfs snapshot tank/cleanroom/snapshots/golden@seed",
		"zfs destroy tank/cleanroom/sandboxes/sandbox-1@snap-golden",
		"zfs destroy -r tank/cleanroom/sandboxes/sandbox-1",
		"zfs list -H -o name tank/cleanroom/sandboxes/sandbox-2",
		"zfs clone -p tank/cleanroom/snapshots/golden@seed tank/cleanroom/sandboxes/sandbox-2",
		"zfs destroy -r tank/cleanroom/snapshots/golden",
		"zfs destroy -r tank/cleanroom/sandboxes/sandbox-2",
	}
	if got, want := runner.commands, wantCommands; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unexpected zfs commands:\n got: %v\nwant: %v", got, want)
	}
}

func TestZFSDriverRequiresDatasetRoot(t *testing.T) {
	_, err := NewZFSDriver(ZFSDriverOptions{})
	if err == nil || !strings.Contains(err.Error(), "dataset root") {
		t.Fatalf("expected dataset root error, got %v", err)
	}
}
