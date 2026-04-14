package controlservice

import (
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

// CreateSandboxReporter receives progress and output events for sandbox creation.
//
// Reporters are best-effort. Implementations should avoid blocking indefinitely
// and may ignore events when streaming is not needed.
type CreateSandboxReporter interface {
	Message(phase cleanroomv1.CreateSandboxPhase, message string)
	Stdout(phase cleanroomv1.CreateSandboxPhase, chunk []byte)
	Stderr(phase cleanroomv1.CreateSandboxPhase, chunk []byte)
	Warning(phase cleanroomv1.CreateSandboxPhase, warning string)
}

type CreateSandboxReporterFuncs struct {
	OnMessage func(phase cleanroomv1.CreateSandboxPhase, message string)
	OnStdout  func(phase cleanroomv1.CreateSandboxPhase, chunk []byte)
	OnStderr  func(phase cleanroomv1.CreateSandboxPhase, chunk []byte)
	OnWarning func(phase cleanroomv1.CreateSandboxPhase, warning string)
}

func (r CreateSandboxReporterFuncs) Message(phase cleanroomv1.CreateSandboxPhase, message string) {
	if r.OnMessage != nil {
		r.OnMessage(phase, message)
	}
}

func (r CreateSandboxReporterFuncs) Stdout(phase cleanroomv1.CreateSandboxPhase, chunk []byte) {
	if r.OnStdout != nil {
		r.OnStdout(phase, chunk)
	}
}

func (r CreateSandboxReporterFuncs) Stderr(phase cleanroomv1.CreateSandboxPhase, chunk []byte) {
	if r.OnStderr != nil {
		r.OnStderr(phase, chunk)
	}
}

func (r CreateSandboxReporterFuncs) Warning(phase cleanroomv1.CreateSandboxPhase, warning string) {
	if r.OnWarning != nil {
		r.OnWarning(phase, warning)
	}
}

func emitCreateSandboxMessage(reporter CreateSandboxReporter, phase cleanroomv1.CreateSandboxPhase, message string) {
	if reporter == nil || message == "" {
		return
	}
	reporter.Message(phase, message)
}

func emitCreateSandboxStdout(reporter CreateSandboxReporter, phase cleanroomv1.CreateSandboxPhase, chunk []byte) {
	if reporter == nil || len(chunk) == 0 {
		return
	}
	reporter.Stdout(phase, append([]byte(nil), chunk...))
}

func emitCreateSandboxStderr(reporter CreateSandboxReporter, phase cleanroomv1.CreateSandboxPhase, chunk []byte) {
	if reporter == nil || len(chunk) == 0 {
		return
	}
	reporter.Stderr(phase, append([]byte(nil), chunk...))
}

func emitCreateSandboxWarning(reporter CreateSandboxReporter, phase cleanroomv1.CreateSandboxPhase, warning string) {
	if reporter == nil || warning == "" {
		return
	}
	reporter.Warning(phase, warning)
}
