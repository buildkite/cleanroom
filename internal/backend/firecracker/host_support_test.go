package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
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

func TestDetectHostSupportUsesHelperCompatibleCommandFallbacks(t *testing.T) {
	prevResolveCommand := hostSupportResolveCommand
	prevGOOS := hostSupportGOOS
	t.Cleanup(func() {
		hostSupportResolveCommand = prevResolveCommand
		hostSupportGOOS = prevGOOS
	})

	hostSupportGOOS = "linux"
	hostSupportResolveCommand = func(command string) (string, error) {
		switch command {
		case "sudo", "ip", "iptables", "sysctl":
			return "/fallback/" + command, nil
		default:
			return "", nil
		}
	}

	tmpDir := t.TempDir()
	setupFakeSudo(t, filepath.Join(tmpDir, "sudo.log"))
	helperPath := writeExecutable(t, tmpDir, "cleanroom-root-helper", `#!/bin/sh
set -eu
case "$1" in
  version) printf '6
' ;;
  capabilities) printf 'firecracker-network
firecracker-trusted-dns
' ;;
  true) exit 0 ;;
  ip) exit 0 ;;
  *) exit 0 ;;
esac
`)
	if _, err := os.Stat(helperPath); err != nil {
		t.Fatalf("stat helper path: %v", err)
	}

	support := DetectHostSupport(context.Background(), backend.FirecrackerConfig{PrivilegedHelperPath: helperPath})
	if !support.RuntimeUsable {
		t.Fatalf("expected runtime usable with fallback command resolution, got %+v", support)
	}
}

func TestDetectHostSupportUsesHelperForZFSDatasetValidation(t *testing.T) {
	prevResolveCommand := hostSupportResolveCommand
	prevGOOS := hostSupportGOOS
	t.Cleanup(func() {
		hostSupportResolveCommand = prevResolveCommand
		hostSupportGOOS = prevGOOS
	})

	hostSupportGOOS = "linux"
	hostSupportResolveCommand = func(command string) (string, error) {
		switch command {
		case "sudo", "ip", "iptables", "sysctl":
			return "/fallback/" + command, nil
		default:
			return "", nil
		}
	}

	tmpDir := t.TempDir()
	setupFakeSudo(t, filepath.Join(tmpDir, "sudo.log"))
	zfsPath := writeExecutable(t, tmpDir, "zfs", "#!/bin/sh\nset -eu\necho 'direct zfs access should not be used for dataset validation' >&2\nexit 23\n")
	t.Setenv("PATH", tmpDir+":"+os.Getenv("PATH"))
	helperPath := writeExecutable(t, tmpDir, "cleanroom-root-helper", `#!/bin/sh
set -eu
case "$1" in
  version) printf '6
' ;;
  capabilities) printf 'firecracker-network
firecracker-trusted-dns
firecracker-zfs
' ;;
  true) exit 0 ;;
  ip) exit 0 ;;
  zfs)
    if [ "$2" = "list" ] && [ "$3" = "-H" ] && [ "$4" = "-d" ] && [ "$5" = "0" ] && [ "$6" = "-o" ] && [ "$7" = "name" ]; then
      printf '%s
' "$8"
      exit 0
    fi
    echo 'unexpected helper zfs args' >&2
    exit 2
    ;;
  *) exit 0 ;;
esac
`)
	if _, err := os.Stat(helperPath); err != nil {
		t.Fatalf("stat helper path: %v", err)
	}
	if _, err := os.Stat(zfsPath); err != nil {
		t.Fatalf("stat zfs path: %v", err)
	}

	support := DetectHostSupport(context.Background(), backend.FirecrackerConfig{
		PrivilegedHelperPath: helperPath,
		Snapshots: backend.SnapshotConfig{
			Driver:     "zfs",
			ZFSDataset: "cleanroom",
		},
	})
	if !support.ZFSUsable {
		t.Fatalf("expected zfs usable when helper can validate dataset, got %+v", support)
	}
	if got, want := support.ZFSDatasetRoot, "cleanroom"; got != want {
		t.Fatalf("unexpected zfs dataset root: got %q want %q", got, want)
	}
}
