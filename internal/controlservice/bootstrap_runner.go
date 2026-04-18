package controlservice

import (
	"bytes"
	"context"

	"github.com/buildkite/cleanroom/internal/backend"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/policy"
)

func (s *Service) runPersistentSandboxCommand(
	ctx context.Context,
	adapter backend.Adapter,
	sandboxID string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	executionID string,
	command []string,
	stream backend.OutputStream,
) (*backend.ExecutionResult, error) {
	return adapter.RunInSandbox(ctx, backend.ExecutionRequest{
		SandboxID:         sandboxID,
		ExecutionID:       executionID,
		Command:           append([]string(nil), command...),
		Policy:            compiled,
		FirecrackerConfig: withRunDir(firecrackerCfg, internalBootstrapArtifactsDir(sandboxID, executionID)),
	}, stream)
}

func (s *Service) runPersistentBootstrapCommand(
	ctx context.Context,
	adapter backend.Adapter,
	sandboxID string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	phase cleanroomv1.CreateSandboxPhase,
	command []string,
	reporter CreateSandboxReporter,
) (string, *backend.ExecutionResult, string, string, error) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	bootstrapExecutionID := s.ids().NewExecutionID()
	result, err := s.runPersistentSandboxCommand(ctx, adapter, sandboxID, compiled, firecrackerCfg, bootstrapExecutionID, command, backend.OutputStream{
		OnStdout: func(chunk []byte) {
			_, _ = stdout.Write(chunk)
			emitCreateSandboxStdout(reporter, phase, chunk)
		},
		OnStderr: func(chunk []byte) {
			_, _ = stderr.Write(chunk)
			emitCreateSandboxStderr(reporter, phase, chunk)
		},
		OnWarning: func(warning string) {
			emitCreateSandboxWarning(reporter, phase, warning)
		},
	})
	return bootstrapExecutionID, result, stdout.String(), stderr.String(), err
}
