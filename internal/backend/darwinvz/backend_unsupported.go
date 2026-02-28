//go:build !darwin

package darwinvz

import (
	"context"
	"fmt"
	"runtime"

	"github.com/buildkite/cleanroom/internal/backend"
)

type Adapter struct{}

func New() *Adapter {
	return &Adapter{}
}

func (a *Adapter) Name() string {
	return "darwin-vz"
}

func (a *Adapter) Run(_ context.Context, _ backend.RunRequest) (*backend.RunResult, error) {
	return nil, fmt.Errorf("darwin-vz backend requires macOS, current OS is %s", runtime.GOOS)
}

func (a *Adapter) RunStream(ctx context.Context, req backend.RunRequest, _ backend.OutputStream) (*backend.RunResult, error) {
	return a.Run(ctx, req)
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
		},
	}, nil
}
