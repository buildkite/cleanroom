package scripts_test

import (
	"os"
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
		"firecracker-zfs",
		"    version)",
		"    capabilities)",
		"is_runtime_rootfs_image()",
		"is_zfs_dataset()",
		"is_zfs_snapshot_ref()",
		"contains_cleanroom_zfs_namespace()",
		"is_cleanroom_zfs_dataset()",
		"is_cleanroom_zfs_snapshot_ref()",
		"is_zvol_device_path()",
		"run_zfs()",
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
