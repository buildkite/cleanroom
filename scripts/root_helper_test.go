package scripts_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestCleanroomRootHelperDeclaresCapabilitiesAndZFSSupport(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("cleanroom-root-helper.sh")
	if err != nil {
		t.Fatalf("read cleanroom-root-helper.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		"helper_contract_version()",
		"helper_has_zfs()",
		"helper_capabilities()",
		"firecracker-network",
		"firecracker-trusted-dns",
		"firecracker-zfs",
		"firecracker-zfs-metadata",
		"firecracker-zfs-transfer",
		"    version)",
		"    capabilities)",
		"is_runtime_rootfs_image()",
		"is_zfs_dataset()",
		"is_zfs_snapshot_ref()",
		"contains_cleanroom_zfs_namespace()",
		"is_cleanroom_zfs_dataset()",
		"is_cleanroom_zfs_snapshot_ref()",
		"is_cleanroom_zfs_stored_snapshot_ref()",
		"is_cleanroom_zfs_snapshot_import_dataset()",
		"is_cleanroom_zfs_snapshot_import_namespace_dataset()",
		"is_zvol_device_path()",
		"run_zfs()",
		"zfs get: unsupported ref",
		"zfs send: unsupported parent snapshot",
		"zfs send: unsupported child snapshot",
		"zfs receive: unsupported dataset",
		`is_cleanroom_zfs_snapshot_import_namespace_dataset "$3"`,
		"zfs create: unsupported dataset",
		`is_cleanroom_zfs_stored_snapshot_ref "$5"`,
		`is_cleanroom_zfs_stored_snapshot_ref "$4"`,
		`is_cleanroom_zfs_snapshot_import_dataset "$4"`,
		`is_cleanroom_zfs_snapshot_import_namespace_dataset "$7"`,
		"zfs set: unsupported dataset",
		"zfs promote: unsupported dataset",
		"run_dd()",
		"bin=\"$(zfs_bin)\"",
		"exec \"$bin\" \"$@\"",
		"exec \"$(dd_bin)\" \"$@\"",
		"    zfs)",
		"    dd)",
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected cleanroom-root-helper.sh to contain %q", needle)
		}
	}
}

func TestCleanroomRootHelperRestrictsZFSTransferRefs(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("cleanroom-root-helper.sh")
	if err != nil {
		t.Fatalf("read cleanroom-root-helper.sh: %v", err)
	}
	script, ok := strings.CutSuffix(string(content), "\nmain \"$@\"\n")
	if !ok {
		t.Fatal("expected cleanroom-root-helper.sh to end with main invocation")
	}

	checks := script + `
is_cleanroom_zfs_stored_snapshot_ref tank/cleanroom/snapshots/child@base
is_cleanroom_zfs_stored_snapshot_ref tank/cleanroom/snapshots/imports/imported@base
! is_cleanroom_zfs_stored_snapshot_ref tank/cleanroom/base/runtime@base
! is_cleanroom_zfs_stored_snapshot_ref tank/cleanroom/sandboxes/sandbox@snap-child
! is_cleanroom_zfs_stored_snapshot_ref tank/cleanroom/snapshots/child@snap-child
! is_cleanroom_zfs_stored_snapshot_ref tank/cleanroom/snapshots/imports@base
! is_cleanroom_zfs_stored_snapshot_ref tank/cleanroom/snapshots/imports/imported/nested@base

is_cleanroom_zfs_snapshot_import_dataset tank/cleanroom/snapshots/imports/imported
! is_cleanroom_zfs_snapshot_import_dataset tank/cleanroom/snapshots/imported
! is_cleanroom_zfs_snapshot_import_dataset tank/cleanroom/base/imported
! is_cleanroom_zfs_snapshot_import_dataset tank/cleanroom/sandboxes/imported
! is_cleanroom_zfs_snapshot_import_dataset tank/cleanroom/snapshots/imports/imported/nested

is_cleanroom_zfs_snapshot_import_namespace_dataset tank/cleanroom/snapshots/imports
! is_cleanroom_zfs_snapshot_import_namespace_dataset tank/cleanroom/snapshots/imports/imported
! is_cleanroom_zfs_snapshot_import_namespace_dataset tank/cleanroom/snapshots/imports/imported/nested
! is_cleanroom_zfs_snapshot_import_namespace_dataset tank/cleanroom/snapshots
`
	cmd := exec.Command("bash", "-c", checks)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper zfs transfer predicate checks failed: %v\n%s", err, out)
	}
}
