//go:build darwin

package darwinvz

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/hosttools"
	"github.com/buildkite/cleanroom/internal/policy"
)

const (
	darwinVZVMNetE2EEnvEnabled = "CLEANROOM_DARWIN_VZ_VMNET_E2E"
)

func TestVMNetSharedE2E(t *testing.T) {
	if strings.TrimSpace(os.Getenv(darwinVZVMNetE2EEnvEnabled)) == "" {
		t.Skipf("set %s=1 to run darwin-vz vmnet e2e", darwinVZVMNetE2EEnvEnabled)
	}
	if testing.Short() {
		t.Skip("skipping darwin-vz vmnet e2e in short mode")
	}

	helperPath, err := resolveHelperBinaryPath()
	if err != nil {
		t.Fatalf("resolve helper binary: %v", err)
	}
	hasVirtualizationEntitlement, err := helperHasVirtualizationEntitlement(helperPath)
	if err != nil {
		t.Fatalf("verify helper virtualization entitlement: %v", err)
	}
	if !hasVirtualizationEntitlement {
		t.Fatalf("helper %q is missing com.apple.security.virtualization entitlement", helperPath)
	}
	hasVMNetEntitlement, err := helperHasVMNetworkingEntitlement(helperPath)
	if err != nil {
		t.Logf("warning: could not verify helper vmnet entitlement on %q: %v", helperPath, err)
	} else if !hasVMNetEntitlement {
		t.Logf(
			"warning: helper %q does not declare com.apple.developer.networking.vmnet; continuing because runtime behavior is the source of truth for unsandboxed local builds",
			helperPath,
		)
	}
	if _, _, err := New().getGuestAgentBinary(); err != nil {
		t.Fatalf("resolve guest agent binary: %v", err)
	}

	rootFSOverride := strings.TrimSpace(os.Getenv(darwinVZE2EEnvRootFS))
	if rootFSOverride == "" {
		if _, err := hosttools.ResolveE2FSProgsBinary("mkfs.ext4"); err != nil {
			t.Fatalf("resolve mkfs.ext4: %v", err)
		}
		if _, err := hosttools.ResolveE2FSProgsBinary("debugfs"); err != nil {
			t.Fatalf("resolve debugfs: %v", err)
		}
	}

	imageRef := strings.TrimSpace(os.Getenv(darwinVZE2EEnvImageRef))
	if imageRef == "" {
		imageRef = defaultDarwinVZE2EImageRef()
	}

	baseCfg := backend.FirecrackerConfig{
		KernelImagePath:     strings.TrimSpace(os.Getenv(darwinVZE2EEnvKernelImage)),
		RootFSPath:          rootFSOverride,
		DarwinVZNetworkMode: darwinVZNetworkModeVMNetShared,
		VCPUs:               1,
		MemoryMiB:           1024,
		LaunchSeconds:       90,
	}
	compiled := &policy.CompiledPolicy{
		Version:        1,
		ImageRef:       imageRef,
		NetworkDefault: "deny",
	}

	t.Run("default-shared-network-egress", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		adapter := New()
		sandboxID := fmt.Sprintf("cr-vmnet-default-%d", time.Now().UnixNano())
		if err := adapter.ProvisionSandbox(ctx, backend.ProvisionRequest{
			SandboxID:         sandboxID,
			Policy:            compiled,
			FirecrackerConfig: baseCfg,
		}); err != nil {
			t.Fatalf("ProvisionSandbox returned error: %v", err)
		}
		t.Cleanup(func() {
			_ = adapter.TerminateSandbox(context.Background(), sandboxID)
		})

		run := sandboxRunner(ctx, adapter, sandboxID, compiled, baseCfg.LaunchSeconds)
		guestIP := waitForGuestIPv4(t, run)
		if _, err := netip.ParseAddr(guestIP); err != nil {
			t.Fatalf("parse guest IPv4 %q: %v", guestIP, err)
		}
		waitForGuestEgress(t, run)
	})

	t.Run("custom-shared-subnet-egress", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		adapter := New()
		sandboxID := fmt.Sprintf("cr-vmnet-custom-%d", time.Now().UnixNano())
		cfg := baseCfg
		cfg.DarwinVZNetworkSubnet = "10.233.0.0/16"
		if err := adapter.ProvisionSandbox(ctx, backend.ProvisionRequest{
			SandboxID:         sandboxID,
			Policy:            compiled,
			FirecrackerConfig: cfg,
		}); err != nil {
			t.Fatalf("ProvisionSandbox returned error: %v", err)
		}
		t.Cleanup(func() {
			_ = adapter.TerminateSandbox(context.Background(), sandboxID)
		})

		run := sandboxRunner(ctx, adapter, sandboxID, compiled, cfg.LaunchSeconds)
		guestIP := waitForGuestIPv4(t, run)
		if got, want := guestIP, "10.233.0.2"; got != want {
			t.Fatalf("unexpected guest IPv4: got %q want %q", got, want)
		}
		waitForGuestEgress(t, run)
	})

	t.Run("custom-shared-subnet-host-to-guest", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		adapter := New()
		rootFSPath := prepareVMNetHostIngressRootFS(t, ctx, adapter, rootFSOverride, imageRef)
		sandboxID := fmt.Sprintf("cr-vmnet-ingress-%d", time.Now().UnixNano())
		cfg := baseCfg
		cfg.RootFSPath = rootFSPath
		cfg.DarwinVZNetworkSubnet = "10.233.0.0/16"
		if err := adapter.ProvisionSandbox(ctx, backend.ProvisionRequest{
			SandboxID:         sandboxID,
			Policy:            compiled,
			FirecrackerConfig: cfg,
		}); err != nil {
			t.Fatalf("ProvisionSandbox returned error: %v", err)
		}
		t.Cleanup(func() {
			_ = adapter.TerminateSandbox(context.Background(), sandboxID)
		})

		run := sandboxRunner(ctx, adapter, sandboxID, compiled, cfg.LaunchSeconds)
		guestIP := waitForGuestIPv4(t, run)
		if got, want := guestIP, "10.233.0.2"; got != want {
			t.Fatalf("unexpected guest IPv4: got %q want %q", got, want)
		}

		errCh := make(chan error, 1)
		go func() {
			res, err := run("guest-host-ingress-server", "/usr/local/bin/cleanroom-vmnet-echo -listen :18080")
			if err != nil {
				errCh <- err
				return
			}
			if res.ExitCode != 0 {
				errCh <- fmt.Errorf("guest vmnet echo exited %d: stderr=%q stdout=%q", res.ExitCode, res.Stderr, res.Stdout)
				return
			}
			errCh <- nil
		}()

		waitForHostToGuestEcho(t, guestIP, 18080, "ok\n")
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("guest vmnet echo failed: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for guest vmnet echo command to exit")
		}
	})
}

func sandboxRunner(
	ctx context.Context,
	adapter *Adapter,
	sandboxID string,
	compiled *policy.CompiledPolicy,
	launchSeconds int64,
) func(runID, shellCommand string) (*backend.RunResult, error) {
	runCounter := 0
	return func(runID, shellCommand string) (*backend.RunResult, error) {
		runCounter++
		if strings.TrimSpace(runID) == "" {
			runID = fmt.Sprintf("run-%d", runCounter)
		}
		return adapter.RunInSandbox(ctx, backend.RunRequest{
			SandboxID: sandboxID,
			RunID:     runID,
			Command:   []string{"sh", "-lc", shellCommand},
			Policy:    compiled,
			FirecrackerConfig: backend.FirecrackerConfig{
				LaunchSeconds: launchSeconds,
			},
		}, backend.OutputStream{})
	}
}

func waitForGuestIPv4(t *testing.T, run func(string, string) (*backend.RunResult, error)) string {
	t.Helper()

	deadline := time.Now().Add(45 * time.Second)
	var lastStdout string
	var lastErr string
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		res, err := run(fmt.Sprintf("guest-ip-%d", attempt), `set -eu
iface=""
for cand in /sys/class/net/*; do
  name="$(basename "$cand")"
  if [ "$name" = "lo" ]; then
    continue
  fi
  iface="$name"
  break
done
[ -n "$iface" ]
ip_addr=""
if command -v ip >/dev/null 2>&1; then
  ip_addr="$(ip -4 -o addr show dev "$iface" | awk 'NR == 1 { print $4 }' | cut -d/ -f1)"
elif command -v ifconfig >/dev/null 2>&1; then
  ip_addr="$(ifconfig "$iface" | awk '/inet / { print $2; exit }')"
fi
[ -n "$ip_addr" ]
printf '%s\n' "$ip_addr"`)
		if err != nil {
			lastErr = err.Error()
			time.Sleep(1 * time.Second)
			continue
		}
		lastStdout = strings.TrimSpace(res.Stdout)
		lastErr = strings.TrimSpace(res.Stderr)
		if res.ExitCode == 0 && lastStdout != "" {
			return lastStdout
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("timed out waiting for guest IPv4 (stdout=%q stderr=%q)", lastStdout, lastErr)
	return ""
}

func waitForGuestEgress(t *testing.T, run func(string, string) (*backend.RunResult, error)) {
	t.Helper()

	deadline := time.Now().Add(45 * time.Second)
	var lastErr string
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		res, err := run(fmt.Sprintf("guest-egress-%d", attempt), `set -eu
if command -v wget >/dev/null 2>&1; then
  wget -q -T 20 -O /dev/null http://example.com
elif command -v curl >/dev/null 2>&1; then
  curl -fsS --max-time 20 http://example.com >/dev/null
else
  nc -w 10 example.com 80 </dev/null >/dev/null 2>&1
fi
printf 'ok\n'`)
		if err != nil {
			lastErr = err.Error()
			time.Sleep(1 * time.Second)
			continue
		}
		if res.ExitCode == 0 && strings.TrimSpace(res.Stdout) == "ok" {
			return
		}
		lastErr = strings.TrimSpace(res.Stderr)
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("timed out waiting for guest egress (stderr=%q)", lastErr)
}

var (
	vmnetHostIngressBinaryOnce sync.Once
	vmnetHostIngressBinaryPath string
	vmnetHostIngressBinaryErr  error
)

func prepareVMNetHostIngressRootFS(t *testing.T, ctx context.Context, adapter *Adapter, rootFSOverride, imageRef string) string {
	t.Helper()

	if _, err := hosttools.ResolveE2FSProgsBinary("debugfs"); err != nil {
		t.Fatalf("resolve debugfs for vmnet host-ingress rootfs injection: %v", err)
	}

	sourceRootFS := strings.TrimSpace(rootFSOverride)
	if sourceRootFS == "" {
		prepared, err := adapter.ensurePreparedRuntimeRootFSFromImage(ctx, imageRef)
		if err != nil {
			t.Fatalf("prepare runtime rootfs for vmnet host-ingress test: %v", err)
		}
		sourceRootFS = prepared.Path
	}

	rootFSPath := filepath.Join(t.TempDir(), "vmnet-host-ingress.ext4")
	if err := copyFile(sourceRootFS, rootFSPath); err != nil {
		t.Fatalf("copy rootfs for vmnet host-ingress test: %v", err)
	}

	echoBinaryPath := buildGuestVMNetEchoBinary(t)
	if err := injectFileIntoExt4(rootFSPath, echoBinaryPath, "/usr/local/bin/cleanroom-vmnet-echo", 0o755); err != nil {
		t.Fatalf("inject guest vmnet echo binary into rootfs image: %v", err)
	}
	return rootFSPath
}

func buildGuestVMNetEchoBinary(t *testing.T) string {
	t.Helper()

	vmnetHostIngressBinaryOnce.Do(func() {
		tmpDir, err := os.MkdirTemp("", "cleanroom-vmnet-echo-*")
		if err != nil {
			vmnetHostIngressBinaryErr = fmt.Errorf("create vmnet echo temp dir: %w", err)
			return
		}
		vmnetHostIngressBinaryPath = filepath.Join(tmpDir, "cleanroom-vmnet-echo-linux-"+runtime.GOARCH)
		cmd := exec.Command("go", "build", "-trimpath", "-o", vmnetHostIngressBinaryPath, "./cmd/cleanroom-vmnet-echo")
		cmd.Dir = repoRoot(t)
		cmd.Env = append(os.Environ(),
			"GOOS=linux",
			"GOARCH="+runtime.GOARCH,
			"CGO_ENABLED=0",
		)
		output, err := cmd.CombinedOutput()
		if err != nil {
			vmnetHostIngressBinaryErr = fmt.Errorf("build guest vmnet echo binary: %w: %s", err, strings.TrimSpace(string(output)))
			return
		}
	})
	if vmnetHostIngressBinaryErr != nil {
		t.Fatal(vmnetHostIngressBinaryErr)
	}
	return vmnetHostIngressBinaryPath
}

func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate repository root from go.mod")
		}
		dir = parent
	}
}

func waitForHostToGuestEcho(t *testing.T, guestIP string, port int, want string) {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", guestIP, port), 3*time.Second)
		if err != nil {
			lastErr = err
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			conn.Close()
			lastErr = err
			time.Sleep(500 * time.Millisecond)
			continue
		}
		body, readErr := io.ReadAll(conn)
		conn.Close()
		if readErr != nil {
			lastErr = readErr
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if string(body) == want {
			return
		}
		lastErr = fmt.Errorf("unexpected response body %q", string(body))
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for host-to-guest echo: %v", lastErr)
}
