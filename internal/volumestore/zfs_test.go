package volumestore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type zfsTestRunner struct {
	commands      []string
	exists        map[string]bool
	origins       map[string]string
	guids         map[string]string
	sendEstimates map[string]int64
	receiveErr    error
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
			if r.origins == nil {
				r.origins = map[string]string{}
			}
			r.origins[args[3]] = args[2]
		}
	case "promote":
		if len(args) == 2 {
			delete(r.origins, args[1])
		}
	case "destroy":
		if len(args) >= 2 {
			recursive := len(args) >= 3 && args[1] == "-r"
			target := args[len(args)-1]
			if recursive {
				for clone, origin := range r.origins {
					if strings.HasPrefix(origin, target+"@") && clone != target && !strings.HasPrefix(clone, target+"/") {
						return fmt.Errorf("cannot destroy %s: snapshot has dependent clones", target)
					}
				}
			} else if strings.Contains(target, "@") {
				for _, origin := range r.origins {
					if origin == target {
						return fmt.Errorf("cannot destroy %s: snapshot has dependent clones", target)
					}
				}
			}
			for ref := range r.exists {
				if ref == target || strings.HasPrefix(ref, target+"@") || strings.HasPrefix(ref, target+"/") {
					delete(r.exists, ref)
				}
			}
			for clone := range r.origins {
				if clone == target || strings.HasPrefix(clone, target+"/") {
					delete(r.origins, clone)
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
	if command == "zfs" && len(args) == 6 && args[0] == "get" && args[1] == "-H" && args[2] == "-o" && args[3] == "value" && args[4] == "guid" {
		ref := args[5]
		if !r.exists[ref] {
			return nil, errors.New("cannot open dataset: dataset does not exist")
		}
		if guid := strings.TrimSpace(r.guids[ref]); guid != "" {
			return []byte(guid + "\n"), nil
		}
		return []byte("guid-" + strings.ReplaceAll(ref, "/", "-") + "\n"), nil
	}
	if command == "zfs" && len(args) == 6 && args[0] == "get" && args[1] == "-H" && args[2] == "-o" && args[3] == "value" && args[4] == "origin" {
		ref := args[5]
		if !r.exists[ref] {
			return nil, errors.New("cannot open dataset: dataset does not exist")
		}
		if origin := strings.TrimSpace(r.origins[ref]); origin != "" {
			return []byte(origin + "\n"), nil
		}
		return []byte("-\n"), nil
	}
	if command == "zfs" && len(args) == 5 && args[0] == "send" && args[1] == "-nP" && args[2] == "-i" {
		fromRef := args[3]
		toRef := args[4]
		if !r.exists[fromRef] || !r.exists[toRef] {
			return nil, errors.New("could not find any snapshots to send")
		}
		estimate := r.sendEstimates[fromRef+"\x00"+toRef]
		return []byte(fmt.Sprintf("incremental\t%s\t%s\nsize\t%d\n", fromRef, toRef, estimate)), nil
	}
	return nil, fmt.Errorf("unsupported output command: %s %s", command, strings.Join(args, " "))
}

func (r *zfsTestRunner) OutputTo(_ context.Context, dst io.Writer, command string, args ...string) error {
	r.commands = append(r.commands, strings.Join(append([]string{command}, args...), " "))
	if command == "zfs" && len(args) == 4 && args[0] == "send" && args[1] == "-i" {
		fromRef := args[2]
		toRef := args[3]
		if !r.exists[fromRef] || !r.exists[toRef] {
			return errors.New("could not find any snapshots to send")
		}
		_, err := fmt.Fprintf(dst, "stream:%s:%s", fromRef, toRef)
		return err
	}
	return fmt.Errorf("unsupported output stream command: %s %s", command, strings.Join(args, " "))
}

func (r *zfsTestRunner) InputFrom(_ context.Context, src io.Reader, command string, args ...string) error {
	r.commands = append(r.commands, strings.Join(append([]string{command}, args...), " "))
	if command != "zfs" || len(args) != 4 || args[0] != "receive" || args[1] != "-u" || args[2] != "-F" {
		return fmt.Errorf("unsupported input stream command: %s %s", command, strings.Join(args, " "))
	}
	if r.receiveErr != nil {
		return r.receiveErr
	}
	dataset := args[3]
	if !r.exists[dataset] {
		return errors.New("cannot receive into missing dataset")
	}
	if _, err := io.ReadAll(src); err != nil {
		return err
	}
	snapshotRef := dataset + "@base"
	r.exists[snapshotRef] = true
	if r.guids == nil {
		r.guids = map[string]string{}
	}
	if strings.TrimSpace(r.guids[snapshotRef]) == "" {
		r.guids[snapshotRef] = "guid-" + strings.ReplaceAll(snapshotRef, "/", "-")
	}
	return nil
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
	if got, want := base.Ref, "tank/cleanroom/base/runtime-key@base"; got != want {
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
		"zfs list -H -o name tank/cleanroom/base/runtime-key@base",
		"zfs snapshot tank/cleanroom/base/runtime-key@base",
		"zfs list -H -o name tank/cleanroom/sandboxes/sandbox-1",
		"zfs clone -p tank/cleanroom/base/runtime-key@base tank/cleanroom/sandboxes/sandbox-1",
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
	if got, want := snapshot.Ref, "tank/cleanroom/snapshots/golden@base"; got != want {
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

	if err := driver.DestroyVolume(context.Background(), DestroyVolumeRequest{VolumeRef: clone.Ref}); err != nil {
		t.Fatalf("DestroyVolume returned error: %v", err)
	}
	if err := driver.DestroySnapshot(context.Background(), DestroySnapshotRequest{SnapshotRef: snapshot.Ref}); err != nil {
		t.Fatalf("DestroySnapshot returned error: %v", err)
	}

	wantCommands := []string{
		"zfs get -H -o value origin tank/cleanroom/sandboxes/sandbox-1",
		"zfs list -H -o name tank/cleanroom/snapshots/golden",
		"zfs snapshot tank/cleanroom/sandboxes/sandbox-1@snap-golden",
		"zfs list -H -o name tank/cleanroom/snapshots/golden",
		"zfs clone -p tank/cleanroom/sandboxes/sandbox-1@snap-golden tank/cleanroom/snapshots/golden",
		"zfs promote tank/cleanroom/snapshots/golden",
		"zfs snapshot tank/cleanroom/snapshots/golden@base",
		"zfs destroy tank/cleanroom/sandboxes/sandbox-1@snap-golden",
		"zfs destroy -r tank/cleanroom/sandboxes/sandbox-1",
		"zfs list -H -o name tank/cleanroom/sandboxes/sandbox-2",
		"zfs clone -p tank/cleanroom/snapshots/golden@base tank/cleanroom/sandboxes/sandbox-2",
		"zfs destroy -r tank/cleanroom/sandboxes/sandbox-2",
		"zfs destroy -r tank/cleanroom/snapshots/golden",
	}
	if got, want := runner.commands, wantCommands; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unexpected zfs commands:\n got: %v\nwant: %v", got, want)
	}
}

func TestZFSDriverSnapshotVolumeCanSkipParentMetadata(t *testing.T) {
	runner := &zfsTestRunner{
		exists: map[string]bool{
			"tank/cleanroom/sandboxes/sandbox-1": true,
		},
	}
	driver, err := NewZFSDriver(ZFSDriverOptions{
		DatasetRoot:             "tank/cleanroom",
		Runner:                  runner,
		DisableSnapshotMetadata: true,
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
	if snapshot.ParentSnapshotGUID != "" {
		t.Fatalf("expected empty parent guid when metadata is disabled, got %q", snapshot.ParentSnapshotGUID)
	}
	for _, command := range runner.commands {
		if strings.HasPrefix(command, "zfs get ") {
			t.Fatalf("expected snapshot metadata to be skipped, got commands: %v", runner.commands)
		}
	}
}

func TestZFSDriverEnsureBaseVolumeUsesMinimumBytesNamespace(t *testing.T) {
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
		BaseID:       "runtime-key",
		SourcePath:   sourcePath,
		MinimumBytes: 8 << 20,
	})
	if err != nil {
		t.Fatalf("EnsureBaseVolume returned error: %v", err)
	}
	if got, want := base.Ref, "tank/cleanroom/base/runtime-key-min-8388608@base"; got != want {
		t.Fatalf("unexpected base ref: got %q want %q", got, want)
	}

	wantCommands := []string{
		"zfs list -H -o name tank/cleanroom/base/runtime-key-min-8388608",
		"zfs create -p -V 8388608 tank/cleanroom/base/runtime-key-min-8388608",
		"dd if=" + sourcePath + " of=/dev/zvol/tank/cleanroom/base/runtime-key-min-8388608 bs=4M conv=fsync status=none",
		"zfs list -H -o name tank/cleanroom/base/runtime-key-min-8388608@base",
		"zfs snapshot tank/cleanroom/base/runtime-key-min-8388608@base",
	}
	if got, want := runner.commands, wantCommands; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unexpected zfs commands:\n got: %v\nwant: %v", got, want)
	}
}

func TestZFSDriverDescribeSnapshotReturnsGUIDMetadata(t *testing.T) {
	runner := &zfsTestRunner{
		exists: map[string]bool{
			"tank/cleanroom/snapshots/golden@base": true,
		},
		guids: map[string]string{
			"tank/cleanroom/snapshots/golden@base": "123456789",
		},
	}
	driver, err := NewZFSDriver(ZFSDriverOptions{
		DatasetRoot: "tank/cleanroom",
		Runner:      runner,
	})
	if err != nil {
		t.Fatalf("NewZFSDriver returned error: %v", err)
	}

	desc, err := driver.DescribeSnapshot(context.Background(), DescribeSnapshotRequest{
		StorageRef:         "tank/cleanroom/snapshots/golden@base",
		ParentSnapshotGUID: "parent-guid",
	})
	if err != nil {
		t.Fatalf("DescribeSnapshot returned error: %v", err)
	}
	if got, want := desc.SnapshotRef, "tank/cleanroom/snapshots/golden@base"; got != want {
		t.Fatalf("unexpected snapshot ref: got %q want %q", got, want)
	}
	if got, want := desc.SnapshotGUID, "123456789"; got != want {
		t.Fatalf("unexpected snapshot guid: got %q want %q", got, want)
	}
	if got, want := desc.ParentSnapshotGUID, "parent-guid"; got != want {
		t.Fatalf("unexpected parent snapshot guid: got %q want %q", got, want)
	}

	metadata, err := EncodeZFSDriverMetadata(ZFSDriverMetadataFromDescription(desc))
	if err != nil {
		t.Fatalf("EncodeZFSDriverMetadata returned error: %v", err)
	}
	if !strings.Contains(metadata, `"zfs_snapshot_guid":"123456789"`) || !strings.Contains(metadata, `"zfs_parent_snapshot_guid":"parent-guid"`) {
		t.Fatalf("expected metadata to contain snapshot lineage, got %s", metadata)
	}

	wantCommands := []string{
		"zfs get -H -o value guid tank/cleanroom/snapshots/golden@base",
	}
	if got, want := runner.commands, wantCommands; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unexpected zfs commands:\n got: %v\nwant: %v", got, want)
	}
}

func TestZFSDriverPlansIncrementalSnapshotExport(t *testing.T) {
	fromRef := "tank/cleanroom/snapshots/parent@base"
	toRef := "tank/cleanroom/snapshots/child@base"
	runner := &zfsTestRunner{
		exists: map[string]bool{
			fromRef: true,
			toRef:   true,
		},
		guids: map[string]string{
			fromRef: "parent-guid",
			toRef:   "child-guid",
		},
		sendEstimates: map[string]int64{
			fromRef + "\x00" + toRef: 4096,
		},
	}
	driver, err := NewZFSDriver(ZFSDriverOptions{
		DatasetRoot: "tank/cleanroom",
		Runner:      runner,
	})
	if err != nil {
		t.Fatalf("NewZFSDriver returned error: %v", err)
	}

	plan, err := driver.PlanIncrementalSnapshotExport(context.Background(), IncrementalSnapshotExportRequest{
		FromSnapshotRef:  fromRef,
		FromSnapshotGUID: "parent-guid",
		ToSnapshotRef:    toRef,
		ToSnapshotGUID:   "child-guid",
	})
	if err != nil {
		t.Fatalf("PlanIncrementalSnapshotExport returned error: %v", err)
	}
	if got, want := plan.FromSnapshotGUID, "parent-guid"; got != want {
		t.Fatalf("unexpected parent guid: got %q want %q", got, want)
	}
	if got, want := plan.ToSnapshotGUID, "child-guid"; got != want {
		t.Fatalf("unexpected child guid: got %q want %q", got, want)
	}
	if got, want := plan.EstimatedBytes, int64(4096); got != want {
		t.Fatalf("unexpected estimate: got %d want %d", got, want)
	}

	wantCommands := []string{
		"zfs get -H -o value guid " + fromRef,
		"zfs get -H -o value guid " + toRef,
		"zfs send -nP -i " + fromRef + " " + toRef,
	}
	if got, want := runner.commands, wantCommands; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unexpected zfs commands:\n got: %v\nwant: %v", got, want)
	}
}

func TestZFSDriverExportsIncrementalSnapshotStream(t *testing.T) {
	fromRef := "tank/cleanroom/snapshots/parent@base"
	toRef := "tank/cleanroom/snapshots/child@base"
	runner := &zfsTestRunner{
		exists: map[string]bool{
			fromRef: true,
			toRef:   true,
		},
		guids: map[string]string{
			fromRef: "parent-guid",
			toRef:   "child-guid",
		},
		sendEstimates: map[string]int64{
			fromRef + "\x00" + toRef: 4096,
		},
	}
	driver, err := NewZFSDriver(ZFSDriverOptions{
		DatasetRoot: "tank/cleanroom",
		Runner:      runner,
	})
	if err != nil {
		t.Fatalf("NewZFSDriver returned error: %v", err)
	}

	var stream bytes.Buffer
	err = driver.ExportIncrementalSnapshot(context.Background(), IncrementalSnapshotExportPlan{
		FromSnapshotRef:  fromRef,
		FromSnapshotGUID: "parent-guid",
		ToSnapshotRef:    toRef,
		ToSnapshotGUID:   "child-guid",
	}, &stream)
	if err != nil {
		t.Fatalf("ExportIncrementalSnapshot returned error: %v", err)
	}
	if got, want := stream.String(), "stream:"+fromRef+":"+toRef; got != want {
		t.Fatalf("unexpected stream: got %q want %q", got, want)
	}

	wantCommands := []string{
		"zfs get -H -o value guid " + fromRef,
		"zfs get -H -o value guid " + toRef,
		"zfs send -nP -i " + fromRef + " " + toRef,
		"zfs send -i " + fromRef + " " + toRef,
	}
	if got, want := runner.commands, wantCommands; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unexpected zfs commands:\n got: %v\nwant: %v", got, want)
	}
}

func TestZFSDriverImportsIncrementalSnapshotStream(t *testing.T) {
	parentRef := "tank/cleanroom/snapshots/parent@base"
	importedRef := "tank/cleanroom/snapshots/imports/imported@base"
	runner := &zfsTestRunner{
		exists: map[string]bool{
			parentRef: true,
		},
		guids: map[string]string{
			parentRef:   "parent-guid",
			importedRef: "child-guid",
		},
	}
	driver, err := NewZFSDriver(ZFSDriverOptions{
		DatasetRoot: "tank/cleanroom",
		Runner:      runner,
	})
	if err != nil {
		t.Fatalf("NewZFSDriver returned error: %v", err)
	}

	snapshot, err := driver.ImportIncrementalSnapshot(context.Background(), IncrementalSnapshotImportRequest{
		SnapshotID:           "imported",
		ParentSnapshotRef:    parentRef,
		ParentSnapshotGUID:   "parent-guid",
		ExpectedSnapshotGUID: "child-guid",
	}, strings.NewReader("stream-bytes"))
	if err != nil {
		t.Fatalf("ImportIncrementalSnapshot returned error: %v", err)
	}
	if got, want := snapshot.StorageRef, importedRef; got != want {
		t.Fatalf("unexpected storage ref: got %q want %q", got, want)
	}
	if got, want := snapshot.ParentSnapshotGUID, "parent-guid"; got != want {
		t.Fatalf("unexpected parent guid: got %q want %q", got, want)
	}
	if !strings.Contains(snapshot.DriverMetadata, `"zfs_snapshot_guid":"child-guid"`) {
		t.Fatalf("expected driver metadata to contain child guid, got %s", snapshot.DriverMetadata)
	}

	wantCommands := []string{
		"zfs get -H -o value guid " + parentRef,
		"zfs list -H -o name tank/cleanroom/snapshots/imports/imported",
		"zfs clone -p " + parentRef + " tank/cleanroom/snapshots/imports/imported",
		"zfs receive -u -F tank/cleanroom/snapshots/imports/imported",
		"zfs get -H -o value guid " + importedRef,
	}
	if got, want := runner.commands, wantCommands; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unexpected zfs commands:\n got: %v\nwant: %v", got, want)
	}
}

func TestZFSDriverCleansUpFailedIncrementalImport(t *testing.T) {
	parentRef := "tank/cleanroom/snapshots/parent@base"
	runner := &zfsTestRunner{
		exists: map[string]bool{
			parentRef: true,
		},
		guids: map[string]string{
			parentRef: "parent-guid",
		},
		receiveErr: errors.New("receive failed"),
	}
	driver, err := NewZFSDriver(ZFSDriverOptions{
		DatasetRoot: "tank/cleanroom",
		Runner:      runner,
	})
	if err != nil {
		t.Fatalf("NewZFSDriver returned error: %v", err)
	}

	_, err = driver.ImportIncrementalSnapshot(context.Background(), IncrementalSnapshotImportRequest{
		SnapshotID:         "imported",
		ParentSnapshotRef:  parentRef,
		ParentSnapshotGUID: "parent-guid",
	}, strings.NewReader("stream-bytes"))
	if err == nil {
		t.Fatal("expected ImportIncrementalSnapshot to fail")
	}
	if !strings.Contains(err.Error(), "receive zfs incremental snapshot") {
		t.Fatalf("unexpected error: %v", err)
	}
	if runner.exists["tank/cleanroom/snapshots/imports/imported"] || runner.exists["tank/cleanroom/snapshots/imports/imported@base"] {
		t.Fatalf("expected failed import dataset to be destroyed, refs: %v", runner.exists)
	}

	wantCommands := []string{
		"zfs get -H -o value guid " + parentRef,
		"zfs list -H -o name tank/cleanroom/snapshots/imports/imported",
		"zfs clone -p " + parentRef + " tank/cleanroom/snapshots/imports/imported",
		"zfs receive -u -F tank/cleanroom/snapshots/imports/imported",
		"zfs destroy -r tank/cleanroom/snapshots/imports/imported",
	}
	if got, want := runner.commands, wantCommands; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unexpected zfs commands:\n got: %v\nwant: %v", got, want)
	}
}

func TestZFSDriverRejectsIncrementalExportParentGUIDMismatch(t *testing.T) {
	fromRef := "tank/cleanroom/snapshots/parent@base"
	toRef := "tank/cleanroom/snapshots/child@base"
	runner := &zfsTestRunner{
		exists: map[string]bool{
			fromRef: true,
			toRef:   true,
		},
		guids: map[string]string{
			fromRef: "actual-parent-guid",
			toRef:   "child-guid",
		},
	}
	driver, err := NewZFSDriver(ZFSDriverOptions{
		DatasetRoot: "tank/cleanroom",
		Runner:      runner,
	})
	if err != nil {
		t.Fatalf("NewZFSDriver returned error: %v", err)
	}

	_, err = driver.PlanIncrementalSnapshotExport(context.Background(), IncrementalSnapshotExportRequest{
		FromSnapshotRef:  fromRef,
		FromSnapshotGUID: "expected-parent-guid",
		ToSnapshotRef:    toRef,
		ToSnapshotGUID:   "child-guid",
	})
	if err == nil {
		t.Fatal("expected PlanIncrementalSnapshotExport to reject parent guid mismatch")
	}
	if !strings.Contains(err.Error(), "parent guid mismatch") {
		t.Fatalf("expected parent guid mismatch error, got %v", err)
	}

	wantCommands := []string{
		"zfs get -H -o value guid " + fromRef,
	}
	if got, want := runner.commands, wantCommands; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unexpected zfs commands:\n got: %v\nwant: %v", got, want)
	}
}

func TestZFSDriverSnapshotVolumeRecordsParentGUID(t *testing.T) {
	originRef := "tank/cleanroom/base/runtime-key@base"
	runner := &zfsTestRunner{
		exists: map[string]bool{
			originRef:                            true,
			"tank/cleanroom/sandboxes/sandbox-1": true,
		},
		origins: map[string]string{
			"tank/cleanroom/sandboxes/sandbox-1": originRef,
		},
		guids: map[string]string{
			originRef: "parent-guid",
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
	if got, want := snapshot.ParentSnapshotGUID, "parent-guid"; got != want {
		t.Fatalf("unexpected parent snapshot guid: got %q want %q", got, want)
	}

	wantCommands := []string{
		"zfs get -H -o value origin tank/cleanroom/sandboxes/sandbox-1",
		"zfs get -H -o value guid " + originRef,
		"zfs list -H -o name tank/cleanroom/snapshots/golden",
		"zfs snapshot tank/cleanroom/sandboxes/sandbox-1@snap-golden",
		"zfs list -H -o name tank/cleanroom/snapshots/golden",
		"zfs clone -p tank/cleanroom/sandboxes/sandbox-1@snap-golden tank/cleanroom/snapshots/golden",
		"zfs promote tank/cleanroom/snapshots/golden",
		"zfs snapshot tank/cleanroom/snapshots/golden@base",
		"zfs destroy tank/cleanroom/sandboxes/sandbox-1@snap-golden",
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

func TestZFSDatasetRootFromStoredSnapshotRef(t *testing.T) {
	t.Parallel()

	if got, ok := ZFSDatasetRootFromStoredSnapshotRef("tank/cleanroom/snapshots/golden@base"); !ok || got != "tank/cleanroom" {
		t.Fatalf("unexpected dataset root: got %q ok=%t", got, ok)
	}
	if _, ok := ZFSDatasetRootFromStoredSnapshotRef("tank/cleanroom/sandboxes/sandbox-1@snap-golden"); ok {
		t.Fatal("expected non-stored snapshot ref to be rejected")
	}
}

func TestZFSDatasetRootFromManagedRef(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"tank/cleanroom/base/runtime-key@base":                  "tank/cleanroom",
		"tank/cleanroom/sandboxes/sandbox-1":                    "tank/cleanroom",
		"tank/cleanroom/snapshots/golden@base":                  "tank/cleanroom",
		"tank/cleanroom/snapshots/imports/imported@base":        "tank/cleanroom",
		"tank/cleanroom/snapshots/imports/imported":             "tank/cleanroom",
		"tank/cleanroom/sandboxes/source@snap-golden":           "tank/cleanroom",
		"tank/snapshots/cleanroom/sandboxes/sandbox-1":          "tank/snapshots/cleanroom",
		"tank/base/cleanroom/snapshots/golden@base":             "tank/base/cleanroom",
		"tank/sandboxes/cleanroom/base/runtime-key-min-8388608": "tank/sandboxes/cleanroom",
	}
	for ref, want := range cases {
		got, ok := ZFSDatasetRootFromManagedRef(ref)
		if !ok || got != want {
			t.Fatalf("unexpected dataset root for %q: got %q ok=%t want %q", ref, got, ok, want)
		}
	}

	rejected := []string{
		"/tmp/rootfs.ext4",
		"/var/lib/buildkite-agent/state/cleanroom/snapshots/firecracker/snap-test/rootfs.ext4",
		"/var/lib/buildkite-agent/state/cleanroom/sandboxes/cr-test/rootfs-persistent.ext4",
	}
	for _, ref := range rejected {
		if _, ok := ZFSDatasetRootFromManagedRef(ref); ok {
			t.Fatalf("expected non-zfs ref %q to be rejected", ref)
		}
	}
}

func TestZFSDriverEnsureWritableVolumeMinimumSizeGrowsUndersizedZvol(t *testing.T) {
	runner := &zfsTestRunner{exists: map[string]bool{}}
	driver, err := NewZFSDriver(ZFSDriverOptions{
		DatasetRoot: "tank/cleanroom",
		Runner:      runner,
	})
	if err != nil {
		t.Fatalf("NewZFSDriver returned error: %v", err)
	}

	prevEnsure := ext4imageEnsureMinimumSize
	prevPathSize := ext4imagePathSizeBytes
	prevAlign := ext4imageAlignBytes
	defer func() {
		ext4imageEnsureMinimumSize = prevEnsure
		ext4imagePathSizeBytes = prevPathSize
		ext4imageAlignBytes = prevAlign
	}()

	ext4imagePathSizeBytes = func(string) (int64, bool, error) {
		return 4 << 20, true, nil
	}
	ext4imageAlignBytes = func(size int64) int64 {
		return size + ((4 << 20) - (size % (4 << 20)))
	}

	var (
		gotAttachmentPath string
		gotMinimumBytes   int64
	)
	ext4imageEnsureMinimumSize = func(_ context.Context, attachmentPath string, minimumBytes int64) error {
		gotAttachmentPath = attachmentPath
		gotMinimumBytes = minimumBytes
		return nil
	}

	volume := WritableVolume{
		Ref:            "tank/cleanroom/sandboxes/sandbox-1",
		AttachmentPath: "/dev/zvol/tank/cleanroom/sandboxes/sandbox-1",
	}
	if err := driver.EnsureWritableVolumeMinimumSize(context.Background(), volume, (8<<20)+1); err != nil {
		t.Fatalf("EnsureWritableVolumeMinimumSize returned error: %v", err)
	}

	wantCommands := []string{
		"zfs set volsize=12582912 tank/cleanroom/sandboxes/sandbox-1",
	}
	if got, want := runner.commands, wantCommands; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unexpected zfs commands:\n got: %v\nwant: %v", got, want)
	}
	if got, want := gotAttachmentPath, volume.AttachmentPath; got != want {
		t.Fatalf("unexpected attachment path: got %q want %q", got, want)
	}
	if got, want := gotMinimumBytes, int64((8<<20)+1); got != want {
		t.Fatalf("unexpected minimum bytes: got %d want %d", got, want)
	}
}
