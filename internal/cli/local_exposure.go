package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/controlclient"
	"github.com/buildkite/cleanroom/internal/exposure"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

func startClientExposures(ctx context.Context, client *controlclient.Client, sandboxID string, requested []*cleanroomv1.PortExposure) (*exposure.Manager, []*cleanroomv1.PortExposure, error) {
	if len(requested) == 0 {
		return nil, nil, nil
	}
	if client == nil {
		return nil, nil, errors.New("missing control client")
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return nil, nil, errors.New("missing sandbox id")
	}
	if err := ensureSandboxPortDialSupported(ctx, client, sandboxID); err != nil {
		return nil, nil, err
	}

	manager := exposure.NewManager(exposure.Config{})
	if hasHTTPSExposure(requested) {
		if err := manager.StartDNS(ctx); err != nil {
			_ = manager.Close()
			return nil, nil, err
		}
	}

	dialer := func(ctx context.Context, _ string, port int) (net.Conn, error) {
		return client.DialSandboxPort(ctx, sandboxID, port)
	}
	registered := make([]*cleanroomv1.PortExposure, 0, len(requested))
	for _, req := range requested {
		exposed, err := manager.Register(ctx, exposure.RegisterRequest{
			OwnerID:   "client:" + sandboxID,
			SandboxID: sandboxID,
			Exposure:  req,
			Dialer:    dialer,
		})
		if err != nil {
			_ = manager.Close()
			return nil, nil, err
		}
		registered = append(registered, exposed)
	}
	return manager, registered, nil
}

func prevalidateRequestedExposures(ctx *runtimeContext, cwd string, requested []*cleanroomv1.PortExposure) error {
	if err := prevalidateConfiguredExposures(ctx, cwd, requested); err != nil {
		return err
	}
	if !hasHTTPSExposure(requested) {
		return nil
	}
	_, err := exposure.EnsureRuntimeCertificateAuthority(exposure.Domain, "")
	return err
}

func hasHTTPSExposure(requested []*cleanroomv1.PortExposure) bool {
	for _, req := range requested {
		if req != nil && strings.TrimSpace(req.GetProtocol()) == exposureProtocolHTTPS {
			return true
		}
	}
	return false
}

func ensureSandboxPortDialSupported(ctx context.Context, client *controlclient.Client, sandboxID string) error {
	resp, err := client.GetSandbox(ctx, &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		return err
	}
	sandbox := resp.GetSandbox()
	if sandbox == nil {
		return errors.New("sandbox lookup returned no sandbox")
	}
	switch sandbox.GetStatus() {
	case cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY:
	case cleanroomv1.SandboxStatus_SANDBOX_STATUS_SUSPENDED:
		if !sandbox.GetBackendCapabilities()[backend.CapabilitySandboxSuspend] {
			return fmt.Errorf("sandbox %q is suspended but backend %q does not support sandbox wake", sandboxID, sandbox.GetBackend())
		}
	default:
		return fmt.Errorf("sandbox %q is not ready", sandboxID)
	}
	if !sandbox.GetBackendCapabilities()[backend.CapabilitySandboxPortDial] {
		return fmt.Errorf("backend %q does not support sandbox port dialing", sandbox.GetBackend())
	}
	return nil
}

func runForegroundClientExposures(ctx *runtimeContext, flags clientFlags, sandboxID string, requested []*cleanroomv1.PortExposure) error {
	if len(requested) == 0 {
		return errors.New("at least one --expose or --expose-https flag is required")
	}
	client, err := flags.connect(ctx)
	if err != nil {
		return err
	}

	return runForegroundClientExposuresWithClient(ctx.Stdout, client, sandboxID, requested)
}

func runForegroundClientExposuresWithClient(w io.Writer, client *controlclient.Client, sandboxID string, requested []*cleanroomv1.PortExposure) error {
	if len(requested) == 0 {
		return nil
	}
	exposureCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager, registered, err := startClientExposures(exposureCtx, client, sandboxID, requested)
	if err != nil {
		return err
	}
	defer manager.Close()
	if err := writeExposureLines(w, registered); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "press Ctrl-C to stop exposing ports"); err != nil {
		return err
	}

	signalCh := newSignalChannel()
	notifySignals(signalCh, os.Interrupt, syscall.SIGTERM)
	defer stopSignals(signalCh)
	<-signalCh
	return nil
}
