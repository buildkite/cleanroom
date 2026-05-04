package controlservice

import (
	"bytes"
	"context"
	"fmt"
	"strings"

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
	networkStage policy.NetworkStage,
	executionID string,
	command []string,
	env []string,
	stream backend.OutputStream,
) (*backend.ExecutionResult, error) {
	return s.runPersistentSandboxCommandWithOptions(ctx, adapter, sandboxID, compiled, firecrackerCfg, networkStage, executionID, command, env, persistentSandboxCommandOptions{}, stream)
}

type persistentSandboxCommandOptions struct {
	Dir                     string
	ClosedEnv               bool
	InputProjection         *backend.InputProjection
	CacheOutputFileCaptures []backend.CacheOutputFileCapture
	OverlayCapture          *backend.OverlayCapture
}

func (s *Service) runPersistentSandboxCommandWithOptions(
	ctx context.Context,
	adapter backend.Adapter,
	sandboxID string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	networkStage policy.NetworkStage,
	executionID string,
	command []string,
	env []string,
	opts persistentSandboxCommandOptions,
	stream backend.OutputStream,
) (*backend.ExecutionResult, error) {
	return adapter.RunInSandbox(ctx, backend.ExecutionRequest{
		SandboxID:               sandboxID,
		ExecutionID:             executionID,
		Command:                 append([]string(nil), command...),
		Dir:                     strings.TrimSpace(opts.Dir),
		Env:                     append([]string(nil), env...),
		ClosedEnv:               opts.ClosedEnv,
		InputProjection:         cloneInputProjection(opts.InputProjection),
		CacheOutputFileCaptures: cloneCacheOutputFileCaptures(opts.CacheOutputFileCaptures),
		OverlayCapture:          cloneOverlayCapture(opts.OverlayCapture),
		Policy:                  compiled,
		NetworkStage:            networkStage,
		FirecrackerConfig:       withRunDir(firecrackerCfg, internalBootstrapArtifactsDir(sandboxID, executionID)),
	}, stream)
}

func cloneOverlayCapture(capture *backend.OverlayCapture) *backend.OverlayCapture {
	if capture == nil {
		return nil
	}
	return &backend.OverlayCapture{
		UpperDir:            capture.UpperDir,
		BaselinePaths:       append([]string(nil), capture.BaselinePaths...),
		DeclaredFileOutputs: append([]string(nil), capture.DeclaredFileOutputs...),
		IgnoredPrefixes:     append([]string(nil), capture.IgnoredPrefixes...),
	}
}

func cloneCacheOutputFileCaptures(captures []backend.CacheOutputFileCapture) []backend.CacheOutputFileCapture {
	if len(captures) == 0 {
		return nil
	}
	out := make([]backend.CacheOutputFileCapture, len(captures))
	copy(out, captures)
	return out
}

func (s *Service) runPersistentBootstrapCommand(
	ctx context.Context,
	adapter backend.Adapter,
	sandboxID string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	phase cleanroomv1.CreateSandboxPhase,
	networkStage policy.NetworkStage,
	command []string,
	stdin []byte,
	reporter CreateSandboxReporter,
) (string, *backend.ExecutionResult, string, string, error) {
	return s.runPersistentBootstrapCommandWithOptions(ctx, adapter, sandboxID, compiled, firecrackerCfg, phase, networkStage, command, nil, stdin, persistentSandboxCommandOptions{}, reporter)
}

func (s *Service) runPersistentBootstrapCommandWithOptions(
	ctx context.Context,
	adapter backend.Adapter,
	sandboxID string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	phase cleanroomv1.CreateSandboxPhase,
	networkStage policy.NetworkStage,
	command []string,
	env []string,
	stdin []byte,
	opts persistentSandboxCommandOptions,
	reporter CreateSandboxReporter,
) (string, *backend.ExecutionResult, string, string, error) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var attachErr error
	bootstrapExecutionID := s.ids().NewExecutionID()
	result, err := s.runPersistentSandboxCommandWithOptions(ctx, adapter, sandboxID, compiled, firecrackerCfg, networkStage, bootstrapExecutionID, command, env, opts, backend.OutputStream{
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
		OnAttach: func(io backend.AttachIO) {
			if len(stdin) == 0 {
				return
			}
			if io.WriteStdin == nil {
				attachErr = fmt.Errorf("bootstrap phase %q does not support stdin attach", phase.String())
				return
			}
			if err := io.WriteStdin(append([]byte(nil), stdin...)); err != nil {
				attachErr = fmt.Errorf("write bootstrap stdin: %w", err)
				return
			}
			if io.CloseStdin != nil {
				if err := io.CloseStdin(); err != nil {
					attachErr = fmt.Errorf("close bootstrap stdin: %w", err)
				}
			}
		},
	})
	if err == nil && attachErr != nil {
		err = attachErr
	}
	return bootstrapExecutionID, result, stdout.String(), stderr.String(), err
}

func cloneInputProjection(projection *backend.InputProjection) *backend.InputProjection {
	if projection == nil {
		return nil
	}
	return &backend.InputProjection{
		SourceRoot:          strings.TrimSpace(projection.SourceRoot),
		TargetRoot:          strings.TrimSpace(projection.TargetRoot),
		Files:               append([]string(nil), projection.Files...),
		MountSourceReadOnly: projection.MountSourceReadOnly,
	}
}
