package controlservice

import (
	"context"
	"fmt"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"go.opentelemetry.io/otel/attribute"
)

type createSandboxCacheBootstrapConfig struct {
	Adapter                    backend.Adapter
	BackendName                string
	SandboxID                  string
	Compiled                   *policy.CompiledPolicy
	FirecrackerConfig          backend.FirecrackerConfig
	Repository                 *repositorycheckout.Checkout
	Changeset                  *repositorychangeset.Changeset
	CacheOutputSnapshotAdapter backend.CacheOutputVolumeSnapshottingAdapter
	Reporter                   CreateSandboxReporter
}

func (s *Service) bootstrapDependencyForCreateSandbox(
	ctx context.Context,
	cfg createSandboxCacheBootstrapConfig,
	dependencyStagePlan dependencyStagePlan,
	dependencyBlockVolumePlan dependencyBlockVolumePlan,
	dependencyBlockVolumePlanAvailable bool,
) (bool, error) {
	emitCreateSandboxMessage(cfg.Reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_BOOTSTRAP_DEPENDENCIES, "running dependency bootstrap")
	dependencyPublishable := true
	err := s.traceCreateSandboxPhase(ctx, "cleanroom.sandbox.bootstrap_dependencies", createSandboxBootstrapAttrs(cfg.BackendName, cfg.SandboxID, len(dependencyStagePlan.BootstrapCommand), cfg.Repository), func(ctx context.Context) error {
		if dependencyBlockVolumePlanAvailable {
			var err error
			dependencyPublishable, err = s.bootstrapDependencyBlockVolumePlanInPersistentSandbox(ctx, cfg.Adapter, cfg.SandboxID, dependencyBlockVolumePublishConfig{
				Adapter:    cfg.CacheOutputSnapshotAdapter,
				Backend:    cfg.BackendName,
				Changeset:  cfg.Changeset,
				Repository: cfg.Repository,
			}, cfg.Compiled, cfg.FirecrackerConfig, cfg.Repository, dependencyBlockVolumePlan, cfg.Reporter)
			return err
		}
		return s.bootstrapDependencyStageInPersistentSandbox(ctx, cfg.Adapter, cfg.SandboxID, cfg.Compiled, cfg.FirecrackerConfig, dependencyStagePlan, cfg.Reporter)
	})
	if err != nil {
		if terminateErr := s.terminateCreatedSandbox(context.Background(), cfg.Adapter, cfg.SandboxID); terminateErr != nil {
			return dependencyPublishable, fmt.Errorf("bootstrap dependency stage: %w; cleanup failed: %v", err, terminateErr)
		}
		return dependencyPublishable, fmt.Errorf("bootstrap dependency stage: %w", err)
	}
	return dependencyPublishable, nil
}

func (s *Service) bootstrapServicesForCreateSandbox(
	ctx context.Context,
	cfg createSandboxCacheBootstrapConfig,
	servicesStagePlan servicesStagePlan,
	serviceBlockVolumePlan serviceBlockVolumePlan,
	serviceBlockVolumePlanAvailable bool,
	dependencyBlockVolumePublicationSafe bool,
	dependencyBlockVolumeOutputsMounted bool,
) error {
	emitCreateSandboxMessage(cfg.Reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_BOOTSTRAP_SERVICES, "running services bootstrap")
	err := s.traceCreateSandboxPhase(ctx, "cleanroom.sandbox.bootstrap_services", createSandboxBootstrapAttrs(cfg.BackendName, cfg.SandboxID, len(servicesStagePlan.BootstrapCommand), cfg.Repository), func(ctx context.Context) error {
		if serviceBlockVolumePlanAvailable {
			if !dependencyBlockVolumeOutputsMounted {
				serviceBlockVolumePlan.DependencyOutputDirs = nil
			}
			publishConfig := serviceBlockVolumePublishConfig{
				Adapter:    cfg.CacheOutputSnapshotAdapter,
				Backend:    cfg.BackendName,
				Changeset:  cfg.Changeset,
				Repository: cfg.Repository,
			}
			if !dependencyBlockVolumePublicationSafe {
				publishConfig.Adapter = nil
				publishConfig.ForceExactFallback = true
			}
			_, err := s.bootstrapServiceBlockVolumePlanInPersistentSandbox(ctx, cfg.Adapter, cfg.SandboxID, publishConfig, cfg.Compiled, cfg.FirecrackerConfig, cfg.Repository, serviceBlockVolumePlan, cfg.Reporter)
			return err
		}
		return s.bootstrapServicesStageInPersistentSandbox(ctx, cfg.Adapter, cfg.SandboxID, cfg.Compiled, cfg.FirecrackerConfig, servicesStagePlan, cfg.Reporter)
	})
	if err != nil {
		if terminateErr := s.terminateCreatedSandbox(context.Background(), cfg.Adapter, cfg.SandboxID); terminateErr != nil {
			return fmt.Errorf("bootstrap services stage: %w; cleanup failed: %v", err, terminateErr)
		}
		return fmt.Errorf("bootstrap services stage: %w", err)
	}
	return nil
}

func createSandboxBootstrapAttrs(backendName, sandboxID string, argc int, repository *repositorycheckout.Checkout) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String(observability.AttrBackend, backendName),
		attribute.String(observability.AttrSandboxID, sandboxID),
		attribute.Int(observability.AttrCommandArgc, argc),
	}
	if repository != nil {
		if commitSHA := strings.TrimSpace(repository.CommitSHA); commitSHA != "" {
			attrs = append(attrs, attribute.String(observability.AttrRepositoryCommitSHA, commitSHA))
		}
	}
	return attrs
}
