//go:build !darwin

package darwinvz

import (
	"context"
	"fmt"
	"runtime"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/gateway"
	"github.com/buildkite/cleanroom/internal/policy"
	"go.opentelemetry.io/otel/metric"
)

type Adapter struct {
	GatewayRegistry  gatewayRegistry
	GatewayPort      int
	GatewayBridgeURL string
	GatewayRoutes    gateway.ProxyRoutes
	MeterProvider    metric.MeterProvider

	ConfiguredNetworkMode string
}

func New() *Adapter {
	return &Adapter{}
}

func (a *Adapter) Name() string {
	return "darwin-vz"
}

func (a *Adapter) Capabilities() map[string]bool {
	return map[string]bool{
		backend.CapabilityNetworkDefaultDeny:       true,
		backend.CapabilityNetworkAllowlistEgress:   false,
		backend.CapabilityNetworkStageScopedEgress: false,
		backend.CapabilityNetworkGuestInterface:    true,
	}
}

func (a *Adapter) ProvisionSandbox(_ context.Context, _ backend.ProvisionRequest) error {
	return fmt.Errorf("darwin-vz backend requires macOS, current OS is %s", runtime.GOOS)
}

func (a *Adapter) RunInSandbox(_ context.Context, _ backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
	return nil, fmt.Errorf("darwin-vz backend requires macOS, current OS is %s", runtime.GOOS)
}

func (a *Adapter) TerminateSandbox(_ context.Context, _ string) error {
	return fmt.Errorf("darwin-vz backend requires macOS, current OS is %s", runtime.GOOS)
}

func (a *Adapter) RuntimeBaseKey(_ context.Context, _ *policy.CompiledPolicy, _ backend.FirecrackerConfig) (string, error) {
	return "", fmt.Errorf("darwin-vz backend requires macOS, current OS is %s", runtime.GOOS)
}

func (a *Adapter) Doctor(_ context.Context, _ backend.DoctorRequest) (*backend.DoctorReport, error) {
	return &backend.DoctorReport{
		Backend: a.Name(),
		Checks: []backend.DoctorCheck{
			{
				Name:    "os",
				Status:  "fail",
				Message: fmt.Sprintf("darwin-vz backend requires macOS, current OS is %s", runtime.GOOS),
			},
			{
				Name:    "guest_networking",
				Status:  "warn",
				Message: guestNetworkUnavailableWarning,
			},
		},
	}, nil
}
