package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/buildkite/cleanroom/internal/networkfilterstate"
)

func TestNetworkInstallDelegatesToSystemNetworkDaemonInstall(t *testing.T) {
	prev := runNetworkDaemonCommand
	defer func() { runNetworkDaemonCommand = prev }()

	var delegated DaemonCommand
	runNetworkDaemonCommand = func(cmd DaemonCommand, _ *runtimeContext) error {
		delegated = cmd
		return nil
	}

	cmd := &NetworkInstallCommand{}
	if err := cmd.Run(&runtimeContext{}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got, want := delegated.Action, "install"; got != want {
		t.Fatalf("delegated action = %q, want %q", got, want)
	}
	if got, want := delegated.Service, "network-filter"; got != want {
		t.Fatalf("delegated service = %q, want %q", got, want)
	}
	if !delegated.System {
		t.Fatal("expected delegated command to set --system")
	}
	if !delegated.Force {
		t.Fatal("expected delegated command to force idempotent install")
	}
}

func TestNetworkStatusDelegatesToSystemNetworkDaemonStatus(t *testing.T) {
	prev := runNetworkDaemonCommand
	defer func() { runNetworkDaemonCommand = prev }()
	prevClient := newNetworkFilterStatusClient
	defer func() { newNetworkFilterStatusClient = prevClient }()
	prevAppStatus := queryNetworkSupportAppStatus
	defer func() { queryNetworkSupportAppStatus = prevAppStatus }()

	var delegated DaemonCommand
	runNetworkDaemonCommand = func(cmd DaemonCommand, _ *runtimeContext) error {
		delegated = cmd
		return nil
	}
	newNetworkFilterStatusClient = func() networkFilterStatusClient {
		return stubNetworkFilterStatusClient{
			healthErr: nil,
			status: networkfilterstate.StatusSnapshot{
				Version:           1,
				UpdatedAt:         "2026-03-16T09:00:00Z",
				Available:         true,
				Loaded:            true,
				Enabled:           true,
				Configured:        true,
				ProviderStartedAt: "2026-03-16T08:59:00Z",
			},
			statusFound: true,
			policy: networkfilterstate.PolicySnapshot{
				Version:       1,
				UpdatedAt:     "2026-03-16T08:58:00Z",
				DefaultAction: "deny",
				Allow: []networkfilterstate.PolicyAllowRule{
					{Host: "github.com", Ports: []int{443}},
				},
				ProcessRules: []networkfilterstate.ProcessRule{
					{PID: 123},
				},
			},
			policyFound: true,
		}
	}
	queryNetworkSupportAppStatus = func(*runtimeContext) (networkSupportAppStatusSnapshot, error) {
		return networkSupportAppStatusSnapshot{
			AppBundlePath:       "/Applications/Cleanroom.app",
			ExtensionInstalled:  true,
			DaemonHealthy:       true,
			Available:           true,
			Loaded:              true,
			Configured:          true,
			Enabled:             true,
			ProviderValidation:  "",
			NetworkFilterLastError: "",
		}, nil
	}
	stdout, readStdout := makeStdoutCapture(t)
	t.Cleanup(func() { _ = stdout.Close() })

	cmd := &NetworkStatusCommand{}
	if err := cmd.Run(&runtimeContext{Stdout: stdout}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got, want := delegated.Action, "status"; got != want {
		t.Fatalf("delegated action = %q, want %q", got, want)
	}
	if got, want := delegated.Service, "network-filter"; got != want {
		t.Fatalf("delegated service = %q, want %q", got, want)
	}
	if !delegated.System {
		t.Fatal("expected delegated command to set --system")
	}

	out := readStdout()
	assertContainsAll(t, out,
		"network filter telemetry",
		"✓ [pass] api: healthy at http://127.0.0.1:8171",
		"✓ [pass] filter: enabled (available=true, loaded=true, enabled=true, configured=true, updated=2026-03-16T09:00:00Z)",
		"✓ [pass] provider: started 2026-03-16T08:59:00Z",
		"✓ [pass] policy: default deny; 1 allow rule(s); 1 process rule(s); updated 2026-03-16T08:58:00Z",
		"network filter app",
		"✓ [pass] app: installed at /Applications/Cleanroom.app",
		"✓ [pass] preferences: enabled",
	)
}

func TestNetworkStatusReportsTelemetryAPIUnreachable(t *testing.T) {
	prev := runNetworkDaemonCommand
	defer func() { runNetworkDaemonCommand = prev }()
	prevClient := newNetworkFilterStatusClient
	defer func() { newNetworkFilterStatusClient = prevClient }()
	prevAppStatus := queryNetworkSupportAppStatus
	defer func() { queryNetworkSupportAppStatus = prevAppStatus }()

	runNetworkDaemonCommand = func(cmd DaemonCommand, _ *runtimeContext) error {
		if got, want := cmd.Action, "status"; got != want {
			t.Fatalf("delegated action = %q, want %q", got, want)
		}
		return nil
	}
	newNetworkFilterStatusClient = func() networkFilterStatusClient {
		return stubNetworkFilterStatusClient{healthErr: errors.New("dial tcp 127.0.0.1:8171: connect: connection refused")}
	}
	queryNetworkSupportAppStatus = func(*runtimeContext) (networkSupportAppStatusSnapshot, error) {
		return networkSupportAppStatusSnapshot{}, errNetworkSupportAppNotFound
	}
	stdout, readStdout := makeStdoutCapture(t)
	t.Cleanup(func() { _ = stdout.Close() })

	cmd := &NetworkStatusCommand{}
	if err := cmd.Run(&runtimeContext{Stdout: stdout}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	out := readStdout()
	assertContainsAll(t, out,
		"network filter telemetry",
		"✗ [fail] api: unreachable:",
		"connection refused",
	)
}

func TestNetworkStatusWarnsWhenSupportAppIsMissing(t *testing.T) {
	prev := runNetworkDaemonCommand
	defer func() { runNetworkDaemonCommand = prev }()
	prevClient := newNetworkFilterStatusClient
	defer func() { newNetworkFilterStatusClient = prevClient }()
	prevAppStatus := queryNetworkSupportAppStatus
	defer func() { queryNetworkSupportAppStatus = prevAppStatus }()

	runNetworkDaemonCommand = func(cmd DaemonCommand, _ *runtimeContext) error { return nil }
	newNetworkFilterStatusClient = func() networkFilterStatusClient {
		return stubNetworkFilterStatusClient{
			status: networkfilterstate.StatusSnapshot{
				Version:    1,
				UpdatedAt:  "2026-03-16T09:00:00Z",
				Available:  true,
				Loaded:     true,
				Enabled:    false,
				Configured: false,
			},
			statusFound: true,
			policyFound: false,
		}
	}
	queryNetworkSupportAppStatus = func(*runtimeContext) (networkSupportAppStatusSnapshot, error) {
		return networkSupportAppStatusSnapshot{}, errNetworkSupportAppNotFound
	}

	stdout, readStdout := makeStdoutCapture(t)
	t.Cleanup(func() { _ = stdout.Close() })

	cmd := &NetworkStatusCommand{}
	if err := cmd.Run(&runtimeContext{Stdout: stdout}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	out := readStdout()
	assertContainsAll(t, out,
		"network filter app",
		"! [warn] app: support app not found",
	)
}

func TestNetworkEnableInvokesSupportApp(t *testing.T) {
	prev := runNetworkSupportAppAction
	defer func() { runNetworkSupportAppAction = prev }()

	var action networkSupportAppAction
	runNetworkSupportAppAction = func(_ *runtimeContext, got networkSupportAppAction) error {
		action = got
		return nil
	}

	cmd := &NetworkEnableCommand{}
	if err := cmd.Run(&runtimeContext{}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got, want := action, networkSupportAppActionEnable; got != want {
		t.Fatalf("action = %q, want %q", got, want)
	}
}

func TestNetworkDisableInvokesSupportApp(t *testing.T) {
	prev := runNetworkSupportAppAction
	defer func() { runNetworkSupportAppAction = prev }()

	var action networkSupportAppAction
	runNetworkSupportAppAction = func(_ *runtimeContext, got networkSupportAppAction) error {
		action = got
		return nil
	}

	cmd := &NetworkDisableCommand{}
	if err := cmd.Run(&runtimeContext{}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got, want := action, networkSupportAppActionDisable; got != want {
		t.Fatalf("action = %q, want %q", got, want)
	}
}

func TestNetworkResetInvokesSupportApp(t *testing.T) {
	prev := runNetworkSupportAppAction
	defer func() { runNetworkSupportAppAction = prev }()

	var action networkSupportAppAction
	runNetworkSupportAppAction = func(_ *runtimeContext, got networkSupportAppAction) error {
		action = got
		return nil
	}

	cmd := &NetworkResetCommand{}
	if err := cmd.Run(&runtimeContext{}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got, want := action, networkSupportAppActionReset; got != want {
		t.Fatalf("action = %q, want %q", got, want)
	}
}

func TestNetworkUninstallDelegatesToSystemNetworkDaemonUninstall(t *testing.T) {
	prev := runNetworkDaemonCommand
	defer func() { runNetworkDaemonCommand = prev }()

	var delegated DaemonCommand
	runNetworkDaemonCommand = func(cmd DaemonCommand, _ *runtimeContext) error {
		delegated = cmd
		return nil
	}

	cmd := &NetworkUninstallCommand{}
	if err := cmd.Run(&runtimeContext{}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got, want := delegated.Action, "uninstall"; got != want {
		t.Fatalf("delegated action = %q, want %q", got, want)
	}
	if got, want := delegated.Service, "network-filter"; got != want {
		t.Fatalf("delegated service = %q, want %q", got, want)
	}
	if !delegated.System {
		t.Fatal("expected delegated command to set --system")
	}
}

type stubNetworkFilterStatusClient struct {
	healthErr   error
	status      networkfilterstate.StatusSnapshot
	statusFound bool
	statusErr   error
	policy      networkfilterstate.PolicySnapshot
	policyFound bool
	policyErr   error
}

func (s stubNetworkFilterStatusClient) Health(context.Context) error {
	return s.healthErr
}

func (s stubNetworkFilterStatusClient) GetStatus(context.Context) (networkfilterstate.StatusSnapshot, bool, error) {
	return s.status, s.statusFound, s.statusErr
}

func (s stubNetworkFilterStatusClient) GetPolicy(context.Context) (networkfilterstate.PolicySnapshot, bool, error) {
	return s.policy, s.policyFound, s.policyErr
}

func TestResolveNetworkSupportAppExecutablePathPrefersContainingAppBundleOverWorkdirDist(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	repoRoot := filepath.Join(tmp, "repo")
	workdir := filepath.Join(repoRoot, "nested")
	distAppExecutable := filepath.Join(repoRoot, "dist", "Cleanroom.app", "Contents", "MacOS", "Cleanroom")
	installedAppExecutable := filepath.Join(tmp, "Applications", "Cleanroom.app", "Contents", "MacOS", "Cleanroom")
	installedHelper := filepath.Join(tmp, "Applications", "Cleanroom.app", "Contents", "Helpers", "cleanroom")
	shimDir := filepath.Join(tmp, "usr-local-bin")
	shimExecutable := filepath.Join(shimDir, "cleanroom")

	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(distAppExecutable), 0o755); err != nil {
		t.Fatalf("mkdir dist app: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(installedAppExecutable), 0o755); err != nil {
		t.Fatalf("mkdir installed app: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(installedHelper), 0o755); err != nil {
		t.Fatalf("mkdir installed helper: %v", err)
	}
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatalf("mkdir shim dir: %v", err)
	}
	if err := os.WriteFile(distAppExecutable, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write dist app executable: %v", err)
	}
	if err := os.WriteFile(installedAppExecutable, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write installed app executable: %v", err)
	}
	if err := os.WriteFile(installedHelper, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write installed helper executable: %v", err)
	}
	if err := os.Symlink(installedHelper, shimExecutable); err != nil {
		t.Fatalf("symlink shim executable: %v", err)
	}

	got, err := resolveNetworkSupportAppExecutablePathWith(
		"",
		"",
		func() (string, error) { return shimExecutable, nil },
		func() (string, error) { return workdir, nil },
		func() (string, error) { return filepath.Join(tmp, "home"), nil },
		os.Stat,
	)
	if err != nil {
		t.Fatalf("resolveNetworkSupportAppExecutablePathWith returned error: %v", err)
	}
	gotResolved, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatalf("resolve got path: %v", err)
	}
	wantResolved, err := filepath.EvalSymlinks(installedAppExecutable)
	if err != nil {
		t.Fatalf("resolve want path: %v", err)
	}
	if gotResolved != wantResolved {
		t.Fatalf("unexpected support app path: got %q want %q", gotResolved, wantResolved)
	}
}
