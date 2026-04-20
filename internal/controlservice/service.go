package controlservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/cachestore"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/paths"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"github.com/buildkite/cleanroom/internal/repositorystore"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"github.com/buildkite/cleanroom/internal/snapshotstore"
	"github.com/charmbracelet/log"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Service struct {
	Loader          loader
	Config          runtimeconfig.Config
	Backends        map[string]backend.Adapter
	Logger          *log.Logger
	Observability   *observability.Runtime
	metricsOnce     sync.Once
	metrics         *observability.ServiceMetrics
	metricsErr      error
	RepositoryStore repositorystore.RepositoryStore
	runtime         serviceRuntime
	interactive     interactiveSessionBroker
	// Snapshot lifecycle still lives on Service because it coordinates backend
	// adapters, sandbox state, and metadata persistence in one operation chain.
	// If this grows again, extract a dedicated snapshot manager rather than
	// adding more snapshot-specific branching here.
	SnapshotStore snapshotMetadataStore
	CacheStore    cacheMetadataStore

	mu                sync.RWMutex
	sandboxes         map[string]*sandboxState
	executions        map[string]*executionState
	snapshotOps       map[string]int
	snapshotDeletions map[string]struct{}
}

type sandboxState struct {
	ID                                  string
	Backend                             string
	Policy                              *policy.CompiledPolicy
	Firecracker                         backend.FirecrackerConfig
	Repository                          *repositorycheckout.Checkout
	RepositoryHasChangeset              bool
	RepositoryChangesetPendingExecution bool
	SourceKind                          string
	SourceID                            string
	BackingSnapshotID                   string
	RepositoryBusy                      bool
	ActiveExecutionID                   string
	DownloadInProgress                  bool
	CreatedAt                           time.Time
	UpdatedAt                           time.Time
	LastExecutionID                     string
	Status                              cleanroomv1.SandboxStatus
	events                              eventFeed[*cleanroomv1.SandboxEvent]
	Done                                chan struct{}
	DoneClosed                          bool
}

type executionState struct {
	ID                string
	SandboxID         string
	ImageRef          string
	ImageDigest       string
	Command           []string
	PreRunBefore      []string
	Env               []string
	Options           executionOptions
	TTY               bool
	Kind              cleanroomv1.ExecutionKind
	Status            cleanroomv1.ExecutionStatus
	ExitCode          int32
	StartedAt         *time.Time
	FinishedAt        *time.Time
	Message           string
	Stdout            string
	Stderr            string
	LaunchedVM        bool
	PlanPath          string
	RunDir            string
	PreRunActive      bool
	CancelRequested   bool
	CancelSignal      int32
	Cancel            context.CancelFunc
	AttachStdin       func([]byte) error
	AttachCloseStdin  func() error
	AttachResize      func(cols, rows uint32) error
	ParentSpanContext trace.SpanContext
	events            eventFeed[*cleanroomv1.ExecutionStreamEvent]
	Done              chan struct{}
	DoneClosed        bool
}

type interactiveSessionState struct {
	SessionID   string
	SandboxID   string
	ExecutionID string
	Token       string
	ExpiresAt   time.Time
	InitialCols uint32
	InitialRows uint32
}

type InteractiveSession struct {
	SessionID   string
	SandboxID   string
	ExecutionID string
	InitialCols uint32
	InitialRows uint32
}

type loader interface {
	LoadAndCompile(cwd string) (*policy.CompiledPolicy, string, error)
}

type snapshotMetadataStore interface {
	Create(context.Context, snapshotstore.Record) error
	Get(context.Context, string) (snapshotstore.Record, bool, error)
	List(context.Context) ([]snapshotstore.Record, error)
	Delete(context.Context, string) error
}

type cacheMetadataStore interface {
	Create(context.Context, cachestore.Record) error
	Upsert(context.Context, cachestore.Record) error
	GetReady(context.Context, string, string) (cachestore.Record, bool, error)
	Touch(context.Context, string, string) error
	List(context.Context) ([]cachestore.Record, error)
	Delete(context.Context, string, string) error
}

type executionOptions struct {
	LaunchSeconds int64
}

type executionSnapshot struct {
	Execution   *cleanroomv1.Execution
	ImageRef    string
	ImageDigest string
	Message     string
	Stdout      string
	Stderr      string
	PlanPath    string
	RunDir      string
	Launched    bool
	TraceID     string
}

func (s *Service) serviceMetrics() *observability.ServiceMetrics {
	if s == nil {
		return nil
	}
	s.metricsOnce.Do(func() {
		if s.Observability == nil {
			return
		}
		s.metrics, s.metricsErr = observability.NewServiceMetrics(s.Observability.MeterProvider())
		if s.metricsErr != nil && s.Logger != nil {
			s.Logger.Warn("service metrics unavailable", "error", s.metricsErr)
		}
	})
	return s.metrics
}

func sandboxCreateSourceMetricValue(snapshotID, sourceKind string) string {
	switch strings.TrimSpace(sourceKind) {
	case "snapshot":
		return "snapshot"
	case "workspace stage cache":
		return "workspace_cache"
	case "dependency stage cache":
		return "dependency_cache"
	}
	if strings.TrimSpace(snapshotID) != "" {
		return "snapshot"
	}
	return "fresh"
}

func executionKindMetricValue(kind cleanroomv1.ExecutionKind) string {
	switch kind {
	case cleanroomv1.ExecutionKind_EXECUTION_KIND_INTERACTIVE:
		return "interactive"
	case cleanroomv1.ExecutionKind_EXECUTION_KIND_BATCH:
		return "batch"
	default:
		return strings.ToLower(strings.TrimPrefix(kind.String(), "EXECUTION_KIND_"))
	}
}

func executionOutcomeMetricValue(status cleanroomv1.ExecutionStatus) string {
	return observability.ExecutionOutcome(status)
}

var (
	ErrExecutionStdinUnsupported  = errors.New("execution stdin attach is not supported by the current backend")
	ErrExecutionResizeUnsupported = errors.New("execution resize is not supported by the current backend")
)

func (s *Service) CreateSandbox(ctx context.Context, req *cleanroomv1.CreateSandboxRequest) (*cleanroomv1.CreateSandboxResponse, error) {
	return s.createSandbox(ctx, req, nil)
}

func (s *Service) CreateSandboxWithReporter(ctx context.Context, req *cleanroomv1.CreateSandboxRequest, reporter CreateSandboxReporter) (*cleanroomv1.CreateSandboxResponse, error) {
	return s.createSandbox(ctx, req, reporter)
}

func (s *Service) createSandbox(ctx context.Context, req *cleanroomv1.CreateSandboxRequest, reporter CreateSandboxReporter) (resp *cleanroomv1.CreateSandboxResponse, err error) {
	if req == nil {
		return nil, errors.New("missing request")
	}
	createStarted := s.clock().Now()
	snapshotID := strings.TrimSpace(req.GetSnapshotId())
	metricSourceKind := ""
	changeset := repositoryChangesetFromProto(req.GetRepositoryChangeset())
	backendName := resolveBackendName(strings.TrimSpace(req.GetBackend()), s.Config.DefaultBackend)
	ctx, span := s.Observability.Tracer("github.com/buildkite/cleanroom/internal/controlservice").Start(
		ctx,
		observability.SpanSandboxCreate,
		trace.WithAttributes(
			attribute.Bool(observability.AttrSandboxFromSnapshot, snapshotID != ""),
			attribute.Bool(observability.AttrRepositoryCheckout, req.GetRepositoryCheckout() != nil),
			attribute.Bool(observability.AttrRepositoryChangeset, changeset != nil),
		),
	)
	logger := observability.WithTraceContext(s.Logger, ctx)
	defer func() {
		metricBackendName := backendName
		if resp != nil && resp.GetSandbox() != nil {
			span.SetAttributes(
				attribute.String(observability.AttrBackend, resp.GetSandbox().GetBackend()),
				attribute.String(observability.AttrSandboxID, resp.GetSandbox().GetSandboxId()),
			)
			if resolvedBackend := strings.TrimSpace(resp.GetSandbox().GetBackend()); resolvedBackend != "" {
				metricBackendName = resolvedBackend
			}
		}
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		if metrics := s.serviceMetrics(); metrics != nil {
			metrics.RecordSandboxCreate(
				ctx,
				metricBackendName,
				sandboxCreateSourceMetricValue(snapshotID, metricSourceKind),
				map[bool]string{true: observability.OutcomeFailed, false: observability.OutcomeSucceeded}[err != nil],
				s.clock().Now().Sub(createStarted),
			)
		}
		span.End()
	}()

	if snapshotID != "" {
		if req.GetPolicy() != nil {
			return nil, errors.New("snapshot-backed sandbox creation cannot include policy")
		}
		if req.GetRepositoryCheckout() != nil {
			return nil, errors.New("snapshot-backed sandbox creation cannot include repository checkout")
		}
		if changeset != nil {
			return nil, errors.New("snapshot-backed sandbox creation cannot include repository changeset")
		}
		return s.createSandboxFromSnapshot(ctx, req, snapshotID, reporter)
	}
	if req.GetPolicy() == nil {
		return nil, errors.New("missing policy")
	}

	compiled, err := policy.FromProto(req.GetPolicy())
	if err != nil {
		return nil, fmt.Errorf("invalid policy: %w", err)
	}

	span.SetAttributes(attribute.String(observability.AttrBackend, backendName))
	adapter, ok := s.Backends[backendName]
	if !ok {
		return nil, fmt.Errorf("unknown backend %q", backendName)
	}
	repository := repositorycheckout.FromProto(req.GetRepositoryCheckout())
	if repository != nil {
		if commitSHA := strings.TrimSpace(repository.CommitSHA); commitSHA != "" {
			span.SetAttributes(attribute.String(observability.AttrRepositoryCommitSHA, commitSHA))
		}
	}
	if repository != nil {
		if err := validateRepositoryCheckoutForPolicy(compiled, repository); err != nil {
			return nil, err
		}
	}
	if err := validateRepositoryChangesetForCheckout(repository, changeset); err != nil {
		return nil, err
	}

	opts := req.GetOptions()
	execOpts := executionOptions{}
	if opts != nil {
		execOpts.LaunchSeconds = opts.GetLaunchSeconds()
	}
	firecrackerCfg := runtimeconfig.MergeBackendConfig(s.Config, backendName, execOpts.LaunchSeconds)
	firecrackerCfg.RunDir = ""
	firecrackerCfg = withRepositoryBootstrapRootFSMinimum(firecrackerCfg, compiled, repository)

	var replacedWorkspaceStageRecord *cachestore.Record
	var replacedDependencyStageRecord *cachestore.Record
	workspaceStageRuntimeBaseKey := ""
	workspaceStageKey := ""
	workspaceStageCachingEnabled := false
	dependencyStagePlan := dependencyStagePlan{}
	dependencyStageBootstrapEnabled := false
	dependencyStageCachingEnabled := false
	var restoredWorkspaceResp *cleanroomv1.CreateSandboxResponse
	snapshotAdapter, snapshotCapable := adapter.(backend.SnapshottingAdapter)
	if repository != nil {
		dependencyStagePlan, dependencyStageBootstrapEnabled = dependencyStagePlanForRepository(compiled, repository)
	}
	if repository != nil && snapshotCapable && snapshotOperationsEnabledForBackend(backendName, s.Config) {
		runtimeBaseKey, cacheable, err := s.workspaceStageRuntimeBaseKey(ctx, adapter, compiled, firecrackerCfg)
		if err != nil {
			s.logWorkspaceStageWarning("resolve workspace stage runtime base key", "", err)
		} else if cacheable {
			workspaceStageRuntimeBaseKey = runtimeBaseKey
			workspaceStageCachingEnabled = true
			workspaceStageKey = workspaceStageCacheKey(backendName, workspaceStageRuntimeBaseKey, compiled.Hash, repository, changeset)
			if dependencyStageBootstrapEnabled {
				dependencyStagePlan, dependencyStageCachingEnabled, err = s.finalizeDependencyStagePlan(ctx, compiled, repository, changeset, workspaceStageKey, dependencyStagePlan)
				if err != nil {
					dependencyStageCachingEnabled = false
					s.logDependencyStageWarning("resolve dependency stage cache key", "", err)
				}
			}

			if dependencyStageCachingEnabled {
				emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_LOOKUP_DEPENDENCY_STAGE_CACHE, "checking dependency stage cache")
				var record cachestore.Record
				var found bool
				var lookupReason string
				err := s.traceCreateSandboxPhase(ctx, "cleanroom.sandbox.lookup_dependency_stage_cache", cachePhaseAttributes(
					observability.CacheStageDependency,
					observability.CacheOperationLookup,
					repository,
					attribute.String(observability.AttrBackend, backendName),
				), func(ctx context.Context) error {
					var lookupErr error
					record, found, lookupReason, lookupErr = s.lookupDependencyStageCache(ctx, backendName, compiled, repository, dependencyStagePlan)
					setCacheLookupSpanAttributes(ctx, found, lookupReason, lookupErr)
					return lookupErr
				})
				if err != nil {
					s.logDependencyStageWarning("lookup dependency stage cache", "", err)
				} else if found {
					s.logDependencyStageCacheHit(record)
					emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_LOOKUP_DEPENDENCY_STAGE_CACHE, "dependency stage cache hit")
					emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_RESTORE_DEPENDENCY_STAGE_CACHE, "restoring dependency stage cache")
					restoreReq := &cleanroomv1.CreateSandboxRequest{
						Backend: backendName,
						Options: req.GetOptions(),
					}
					var restoreResp *cleanroomv1.CreateSandboxResponse
					restoreErr := s.traceCreateSandboxPhase(ctx, "cleanroom.sandbox.restore_dependency_stage_cache", cachePhaseAttributes(
						observability.CacheStageDependency,
						observability.CacheOperationRestore,
						repository,
						attribute.String(observability.AttrBackend, backendName),
					), func(ctx context.Context) error {
						var err error
						restoreResp, err = s.createSandboxFromCacheRecord(ctx, restoreReq, compiled, record, reporter)
						setCacheResultSpanAttribute(ctx, map[bool]string{true: observability.CacheResultFailed, false: observability.CacheResultRestored}[err != nil])
						return err
					})
					if restoreErr == nil {
						metricSourceKind = "dependency stage cache"
						if cacheStore, err := s.cacheStoreOrErr(); err == nil {
							if err := cacheStore.Touch(ctx, record.Stage, record.CacheKey); err != nil {
								s.logDependencyStageWarning("touch dependency stage cache", "", err)
							}
						}
						s.logDependencyStageRestore(record, restoreResp.GetSandbox().GetSandboxId())
						return restoreResp, nil
					}
					recordCopy := record
					replacedDependencyStageRecord = &recordCopy
					s.logDependencyStageRestoreWarning(record, restoreErr)
				} else {
					s.logDependencyStageCacheMiss(backendName, dependencyStagePlan.CacheKey)
					emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_LOOKUP_DEPENDENCY_STAGE_CACHE, "dependency stage cache miss")
				}
			}

			emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_LOOKUP_WORKSPACE_STAGE_CACHE, "checking workspace stage cache")
			var record cachestore.Record
			var found bool
			var lookupReason string
			err := s.traceCreateSandboxPhase(ctx, "cleanroom.sandbox.lookup_workspace_stage_cache", cachePhaseAttributes(
				observability.CacheStageWorkspace,
				observability.CacheOperationLookup,
				repository,
				attribute.String(observability.AttrBackend, backendName),
			), func(ctx context.Context) error {
				var lookupErr error
				record, found, lookupReason, lookupErr = s.lookupWorkspaceStageCache(ctx, backendName, compiled, workspaceStageRuntimeBaseKey, repository, changeset)
				setCacheLookupSpanAttributes(ctx, found, lookupReason, lookupErr)
				return lookupErr
			})
			if err != nil {
				s.logWorkspaceStageWarning("lookup workspace stage cache", "", err)
			} else if found {
				s.logWorkspaceStageCacheHit(record)
				emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_LOOKUP_WORKSPACE_STAGE_CACHE, "workspace stage cache hit")
				emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_RESTORE_WORKSPACE_STAGE_CACHE, "restoring workspace stage cache")
				restoreReq := &cleanroomv1.CreateSandboxRequest{
					Backend: backendName,
					Options: req.GetOptions(),
				}
				var restoreResp *cleanroomv1.CreateSandboxResponse
				restoreErr := s.traceCreateSandboxPhase(ctx, "cleanroom.sandbox.restore_workspace_stage_cache", cachePhaseAttributes(
					observability.CacheStageWorkspace,
					observability.CacheOperationRestore,
					repository,
					attribute.String(observability.AttrBackend, backendName),
				), func(ctx context.Context) error {
					var err error
					restoreResp, err = s.createSandboxFromCacheRecord(ctx, restoreReq, compiled, record, reporter)
					setCacheResultSpanAttribute(ctx, map[bool]string{true: observability.CacheResultFailed, false: observability.CacheResultRestored}[err != nil])
					return err
				})
				if restoreErr == nil {
					metricSourceKind = "workspace stage cache"
					if cacheStore, err := s.cacheStoreOrErr(); err == nil {
						if err := cacheStore.Touch(ctx, record.Stage, record.CacheKey); err != nil {
							s.logWorkspaceStageWarning("touch workspace stage cache", "", err)
						}
					}
					s.logWorkspaceStageRestore(record, restoreResp.GetSandbox().GetSandboxId())
					if !dependencyStageBootstrapEnabled {
						return restoreResp, nil
					}
					restoredWorkspaceResp = restoreResp
				} else {
					recordCopy := record
					replacedWorkspaceStageRecord = &recordCopy
					s.logWorkspaceStageRestoreWarning(record, restoreErr)
				}
			} else {
				s.logWorkspaceStageCacheMiss(backendName, workspaceStageKey)
				emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_LOOKUP_WORKSPACE_STAGE_CACHE, "workspace stage cache miss")
			}
		}
	}

	if restoredWorkspaceResp != nil {
		sandboxID := restoredWorkspaceResp.GetSandbox().GetSandboxId()
		span.SetAttributes(attribute.String("cleanroom.sandbox.id", sandboxID))
		if dependencyStageBootstrapEnabled {
			bootstrapAttrs := []attribute.KeyValue{
				attribute.String(observability.AttrBackend, backendName),
				attribute.String(observability.AttrSandboxID, sandboxID),
				attribute.Int(observability.AttrCommandArgc, len(dependencyStagePlan.BootstrapCommand)),
			}
			if repository != nil {
				if commitSHA := strings.TrimSpace(repository.CommitSHA); commitSHA != "" {
					bootstrapAttrs = append(bootstrapAttrs, attribute.String(observability.AttrRepositoryCommitSHA, commitSHA))
				}
			}
			emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_BOOTSTRAP_DEPENDENCIES, "running dependency bootstrap")
			if err := s.traceCreateSandboxPhase(ctx, "cleanroom.sandbox.bootstrap_dependencies", bootstrapAttrs, func(ctx context.Context) error {
				return s.bootstrapDependencyStageInPersistentSandbox(ctx, adapter, sandboxID, compiled, firecrackerCfg, dependencyStagePlan, reporter)
			}); err != nil {
				cleanupErr := s.terminateCreatedSandbox(context.Background(), adapter, sandboxID)
				if cleanupErr != nil {
					return nil, fmt.Errorf("bootstrap dependency stage: %w; cleanup failed: %v", err, cleanupErr)
				}
				return nil, fmt.Errorf("bootstrap dependency stage: %w", err)
			}
			if dependencyStageCachingEnabled {
				emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_PUBLISH_DEPENDENCY_STAGE_CACHE, "publishing dependency stage cache")
				s.maybePublishDependencyStageCache(ctx, snapshotAdapter, sandboxID, backendName, compiled, firecrackerCfg, repository, changeset, dependencyStagePlan, replacedDependencyStageRecord)
			}
		}
		return restoredWorkspaceResp, nil
	}

	now := s.clock().Now()
	sandboxID := s.ids().NewSandboxID()
	span.SetAttributes(attribute.String("cleanroom.sandbox.id", sandboxID))
	if logger != nil {
		logger.Debug("create sandbox requested",
			observability.LogFieldSandboxID, sandboxID,
			observability.LogFieldBackend, backendName,
			"policy_hash", compiled.Hash,
			"repository_checkout", repository != nil,
		)
	}

	if logger != nil {
		logger.Debug("provisioning sandbox",
			observability.LogFieldSandboxID, sandboxID,
			observability.LogFieldBackend, backendName,
		)
	}
	emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_PROVISION_SANDBOX, "provisioning sandbox")
	if err := adapter.ProvisionSandbox(ctx, backend.ProvisionRequest{
		SandboxID:         sandboxID,
		Policy:            compiled,
		FirecrackerConfig: firecrackerCfg,
	}); err != nil {
		return nil, fmt.Errorf("provision sandbox: %w", err)
	}
	if logger != nil {
		logger.Debug("sandbox provisioned",
			observability.LogFieldSandboxID, sandboxID,
			observability.LogFieldBackend, backendName,
		)
	}
	if repository != nil {
		emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_BOOTSTRAP_REPOSITORY, "bootstrapping repository checkout")
	}
	repositoryBootstrapAttrs := []attribute.KeyValue{
		attribute.String(observability.AttrBackend, backendName),
		attribute.String(observability.AttrSandboxID, sandboxID),
		attribute.Bool("cleanroom.repository.refresh_existing", false),
	}
	if repository != nil {
		if commitSHA := strings.TrimSpace(repository.CommitSHA); commitSHA != "" {
			repositoryBootstrapAttrs = append(repositoryBootstrapAttrs, attribute.String(observability.AttrRepositoryCommitSHA, commitSHA))
		}
	}
	if err := s.traceCreateSandboxPhase(ctx, "cleanroom.sandbox.bootstrap_repository", repositoryBootstrapAttrs, func(ctx context.Context) error {
		return s.bootstrapRepositoryInPersistentSandbox(ctx, adapter, sandboxID, compiled, firecrackerCfg, repository, false, reporter)
	}); err != nil {
		if terminateErr := s.terminateCreatedSandbox(context.Background(), adapter, sandboxID); terminateErr != nil {
			return nil, fmt.Errorf("bootstrap repository checkout: %w; cleanup failed: %v", err, terminateErr)
		}
		return nil, fmt.Errorf("bootstrap repository checkout: %w", err)
	}
	if changeset != nil {
		changesetAttrs := []attribute.KeyValue{
			attribute.String(observability.AttrBackend, backendName),
			attribute.String(observability.AttrSandboxID, sandboxID),
		}
		if repository != nil {
			if commitSHA := strings.TrimSpace(repository.CommitSHA); commitSHA != "" {
				changesetAttrs = append(changesetAttrs, attribute.String(observability.AttrRepositoryCommitSHA, commitSHA))
			}
		}
		emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_APPLY_REPOSITORY_CHANGESET, "applying repository changeset")
		if err := s.traceCreateSandboxPhase(ctx, "cleanroom.sandbox.apply_repository_changeset", changesetAttrs, func(ctx context.Context) error {
			return s.bootstrapRepositoryChangesetInPersistentSandbox(ctx, adapter, sandboxID, compiled, firecrackerCfg, repository, changeset, reporter)
		}); err != nil {
			if terminateErr := s.terminateCreatedSandbox(context.Background(), adapter, sandboxID); terminateErr != nil {
				return nil, fmt.Errorf("apply repository changeset: %w; cleanup failed: %v", err, terminateErr)
			}
			return nil, fmt.Errorf("apply repository changeset: %w", err)
		}
	}
	if snapshotCapable && workspaceStageCachingEnabled {
		emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_PUBLISH_WORKSPACE_STAGE_CACHE, "publishing workspace stage cache")
		_ = s.traceCreateSandboxPhase(ctx, "cleanroom.sandbox.publish_workspace_stage_cache", []attribute.KeyValue{
			attribute.String("cleanroom.backend", backendName),
			attribute.String("cleanroom.sandbox.id", sandboxID),
		}, func(ctx context.Context) error {
			s.maybePublishWorkspaceStageCache(ctx, snapshotAdapter, sandboxID, backendName, compiled, firecrackerCfg, workspaceStageRuntimeBaseKey, repository, changeset, replacedWorkspaceStageRecord)
			return nil
		})
	}
	if dependencyStageBootstrapEnabled {
		bootstrapAttrs := []attribute.KeyValue{
			attribute.String(observability.AttrBackend, backendName),
			attribute.String(observability.AttrSandboxID, sandboxID),
			attribute.Int(observability.AttrCommandArgc, len(dependencyStagePlan.BootstrapCommand)),
		}
		if repository != nil {
			if commitSHA := strings.TrimSpace(repository.CommitSHA); commitSHA != "" {
				bootstrapAttrs = append(bootstrapAttrs, attribute.String(observability.AttrRepositoryCommitSHA, commitSHA))
			}
		}
		emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_BOOTSTRAP_DEPENDENCIES, "running dependency bootstrap")
		if err := s.traceCreateSandboxPhase(ctx, "cleanroom.sandbox.bootstrap_dependencies", bootstrapAttrs, func(ctx context.Context) error {
			return s.bootstrapDependencyStageInPersistentSandbox(ctx, adapter, sandboxID, compiled, firecrackerCfg, dependencyStagePlan, reporter)
		}); err != nil {
			if terminateErr := s.terminateCreatedSandbox(context.Background(), adapter, sandboxID); terminateErr != nil {
				return nil, fmt.Errorf("bootstrap dependency stage: %w; cleanup failed: %v", err, terminateErr)
			}
			return nil, fmt.Errorf("bootstrap dependency stage: %w", err)
		}
		if dependencyStageCachingEnabled && snapshotCapable {
			emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_PUBLISH_DEPENDENCY_STAGE_CACHE, "publishing dependency stage cache")
			_ = s.traceCreateSandboxPhase(ctx, "cleanroom.sandbox.publish_dependency_stage_cache", []attribute.KeyValue{
				attribute.String("cleanroom.backend", backendName),
				attribute.String("cleanroom.sandbox.id", sandboxID),
			}, func(ctx context.Context) error {
				s.maybePublishDependencyStageCache(ctx, snapshotAdapter, sandboxID, backendName, compiled, firecrackerCfg, repository, changeset, dependencyStagePlan, replacedDependencyStageRecord)
				return nil
			})
		}
	}

	state := &sandboxState{
		ID:                                  sandboxID,
		Backend:                             backendName,
		Policy:                              compiled,
		Firecracker:                         firecrackerCfg,
		Repository:                          cloneRepositoryCheckout(repository),
		RepositoryHasChangeset:              changeset != nil,
		RepositoryChangesetPendingExecution: changeset != nil,
		CreatedAt:                           now,
		UpdatedAt:                           now,
		Status:                              cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY,
		events:                              newEventFeed[*cleanroomv1.SandboxEvent](s.retention().maxRetainedSandboxEvents),
		Done:                                make(chan struct{}),
	}

	s.mu.Lock()
	s.ensureMapsLocked()
	s.sandboxes[sandboxID] = state
	s.recordSandboxEventLocked(state, cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY, "sandbox created and ready")
	s.pruneStateLocked(now)
	resp = &cleanroomv1.CreateSandboxResponse{
		Sandbox:           cloneSandboxLocked(state),
		Message:           "sandbox created and ready",
		SourceKind:        state.SourceKind,
		SourceId:          state.SourceID,
		BackingSnapshotId: state.BackingSnapshotID,
	}
	s.mu.Unlock()

	if logger != nil {
		logger.Info("sandbox created",
			observability.LogFieldSandboxID, sandboxID,
			observability.LogFieldBackend, backendName,
			"policy_hash", compiled.Hash,
		)
	}

	return resp, nil
}

func (s *Service) traceCreateSandboxPhase(ctx context.Context, name string, attrs []attribute.KeyValue, fn func(context.Context) error) error {
	ctx, span := s.Observability.Tracer("github.com/buildkite/cleanroom/internal/controlservice").Start(
		ctx,
		name,
		trace.WithAttributes(attrs...),
	)
	defer span.End()

	err := fn(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (s *Service) createSandboxFromSnapshot(ctx context.Context, req *cleanroomv1.CreateSandboxRequest, snapshotID string, reporter CreateSandboxReporter) (*cleanroomv1.CreateSandboxResponse, error) {
	snapshotID = strings.TrimSpace(snapshotID)
	if snapshotID == "" {
		return nil, errors.New("missing snapshot_id")
	}
	if err := s.beginSnapshotUse(snapshotID); err != nil {
		return nil, err
	}
	defer s.finishSnapshotUse(snapshotID)

	store, err := s.snapshotStoreOrErr()
	if err != nil {
		return nil, err
	}

	record, ok, err := store.Get(ctx, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("load snapshot %q: %w", snapshotID, err)
	}
	if !ok {
		return nil, fmt.Errorf("unknown snapshot %q", snapshotID)
	}
	return s.createSandboxFromSnapshotRecord(ctx, req, record, reporter)
}

func (s *Service) createSandboxFromSnapshotRecord(ctx context.Context, req *cleanroomv1.CreateSandboxRequest, record snapshotstore.Record, reporter CreateSandboxReporter) (*cleanroomv1.CreateSandboxResponse, error) {
	snapshotID := strings.TrimSpace(record.SnapshotID)
	if snapshotID == "" {
		return nil, errors.New("missing snapshot_id")
	}

	source, err := storedRootFSRecordFromSnapshot(record)
	if err != nil {
		return nil, err
	}
	return s.createSandboxFromStoredRootFS(ctx, req, source, nil, reporter)
}

func (s *Service) GetSandbox(_ context.Context, req *cleanroomv1.GetSandboxRequest) (*cleanroomv1.GetSandboxResponse, error) {
	if req == nil || strings.TrimSpace(req.GetSandboxId()) == "" {
		return nil, errors.New("missing sandbox_id")
	}

	s.mu.RLock()
	state, ok := s.sandboxes[strings.TrimSpace(req.GetSandboxId())]
	if !ok {
		s.mu.RUnlock()
		return nil, fmt.Errorf("unknown sandbox %q", req.GetSandboxId())
	}
	resp := &cleanroomv1.GetSandboxResponse{Sandbox: cloneSandboxLocked(state)}
	s.mu.RUnlock()
	return resp, nil
}

func (s *Service) ListSandboxes(_ context.Context, _ *cleanroomv1.ListSandboxesRequest) (*cleanroomv1.ListSandboxesResponse, error) {
	s.mu.RLock()
	items := make([]*sandboxState, 0, len(s.sandboxes))
	for _, sb := range s.sandboxes {
		items = append(items, sb)
	}
	s.mu.RUnlock()

	sort.Slice(items, func(i, j int) bool {
		left := sandboxTerminalTime(items[i])
		right := sandboxTerminalTime(items[j])
		if left.Equal(right) {
			return items[i].ID < items[j].ID
		}
		return left.After(right)
	})

	resp := &cleanroomv1.ListSandboxesResponse{Sandboxes: make([]*cleanroomv1.Sandbox, 0, len(items))}
	for _, sb := range items {
		resp.Sandboxes = append(resp.Sandboxes, cloneSandboxLocked(sb))
	}
	return resp, nil
}

func (s *Service) ListExecutions(_ context.Context, req *cleanroomv1.ListExecutionsRequest) (*cleanroomv1.ListExecutionsResponse, error) {
	if req == nil {
		req = &cleanroomv1.ListExecutionsRequest{}
	}
	sandboxID := strings.TrimSpace(req.GetSandboxId())
	includeFinal := req.GetAll()

	type executionListItem struct {
		execution *cleanroomv1.Execution
		sortTime  time.Time
	}

	s.mu.RLock()
	items := make([]executionListItem, 0, len(s.executions))
	for _, ex := range s.executions {
		if ex == nil {
			continue
		}
		if sandboxID != "" && ex.SandboxID != sandboxID {
			continue
		}
		if !includeFinal && isFinalExecutionStatus(ex.Status) {
			continue
		}
		items = append(items, executionListItem{
			execution: cloneExecutionLocked(ex),
			sortTime:  executionSortTime(ex, s.sandboxes[ex.SandboxID]),
		})
	}
	s.mu.RUnlock()

	sort.Slice(items, func(i, j int) bool {
		left := items[i].sortTime
		right := items[j].sortTime
		if left.Equal(right) {
			if items[i].execution.GetSandboxId() == items[j].execution.GetSandboxId() {
				return items[i].execution.GetExecutionId() < items[j].execution.GetExecutionId()
			}
			return items[i].execution.GetSandboxId() < items[j].execution.GetSandboxId()
		}
		return left.After(right)
	})

	resp := &cleanroomv1.ListExecutionsResponse{Executions: make([]*cleanroomv1.Execution, 0, len(items))}
	for _, item := range items {
		resp.Executions = append(resp.Executions, item.execution)
	}
	return resp, nil
}

func (s *Service) CreateSnapshot(ctx context.Context, req *cleanroomv1.CreateSnapshotRequest) (*cleanroomv1.CreateSnapshotResponse, error) {
	if req == nil {
		return nil, errors.New("missing request")
	}
	sandboxID := strings.TrimSpace(req.GetSandboxId())
	if sandboxID == "" {
		return nil, errors.New("missing sandbox_id")
	}

	store, err := s.snapshotStoreOrErr()
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	snapshotID := newSnapshotID()
	name := strings.TrimSpace(req.GetName())

	var (
		record          snapshotstore.Record
		snapshotAdapter backend.SnapshottingAdapter
	)

	s.mu.Lock()
	s.ensureMapsLocked()
	state, ok := s.sandboxes[sandboxID]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("unknown sandbox %q", sandboxID)
	}
	if err := ensureSandboxIdleLocked(sandboxID, state, s.executions); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	adapter, ok := s.Backends[state.Backend]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("unknown backend %q", state.Backend)
	}
	snapshotAdapter, ok = adapter.(backend.SnapshottingAdapter)
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("backend %q does not support snapshots", state.Backend)
	}
	if !snapshotOperationsEnabledForBackend(state.Backend, s.Config) {
		s.mu.Unlock()
		return nil, fmt.Errorf("snapshots are not enabled for backend %q", state.Backend)
	}
	if state.Policy == nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("sandbox %q is missing compiled policy", sandboxID)
	}
	snapshotCfg := withSnapshotDriver(state.Backend, state.Firecracker, state.Firecracker.Snapshots.Driver)
	record = snapshotstore.Record{
		SnapshotID:             snapshotID,
		SourceSandboxID:        sandboxID,
		Backend:                state.Backend,
		Name:                   name,
		PolicyHash:             state.Policy.Hash,
		Policy:                 state.Policy.ToProto(),
		Repository:             cloneRepositoryCheckout(state.Repository).ToProto(),
		RepositoryHasChangeset: state.RepositoryHasChangeset,
		StorageDriver:          snapshotCfg.Snapshots.Driver,
		CreatedAt:              now,
	}
	s.recordSandboxEventLocked(state, cleanroomv1.SandboxStatus_SANDBOX_STATUS_PROVISIONING, fmt.Sprintf("snapshot %s in progress", snapshotID))
	s.mu.Unlock()

	result, err := snapshotAdapter.CreateSnapshot(ctx, backend.SnapshotRequest{
		SandboxID:         sandboxID,
		SnapshotID:        snapshotID,
		FirecrackerConfig: snapshotCfg,
	})
	if err != nil {
		s.mu.Lock()
		if current, ok := s.sandboxes[sandboxID]; ok {
			s.completeSnapshotLocked(current, fmt.Sprintf("snapshot %s failed: %v", snapshotID, err))
		}
		s.mu.Unlock()
		return nil, fmt.Errorf("create snapshot: %w", err)
	}

	record.StorageRef = strings.TrimSpace(result.StorageRef)
	if err := store.Create(ctx, record); err != nil {
		deleteErr := snapshotAdapter.DeleteSnapshot(ctx, backend.DeleteSnapshotRequest{
			SnapshotID:        snapshotID,
			StorageRef:        record.StorageRef,
			FirecrackerConfig: snapshotCfg,
		})
		if deleteErr != nil && s.Logger != nil {
			s.Logger.Warn("rollback snapshot after metadata failure failed",
				"snapshot_id", snapshotID,
				"storage_ref", record.StorageRef,
				"error", deleteErr,
			)
		}
		s.mu.Lock()
		if current, ok := s.sandboxes[sandboxID]; ok {
			s.completeSnapshotLocked(current, fmt.Sprintf("snapshot %s failed: %v", snapshotID, err))
		}
		s.mu.Unlock()
		return nil, fmt.Errorf("persist snapshot metadata: %w", err)
	}

	s.mu.Lock()
	if current, ok := s.sandboxes[sandboxID]; ok {
		s.completeSnapshotLocked(current, fmt.Sprintf("snapshot %s created", snapshotID))
	}
	s.mu.Unlock()

	resp := &cleanroomv1.CreateSnapshotResponse{
		Snapshot: cloneSnapshotRecord(record),
		Message:  "snapshot created",
	}
	return resp, nil
}

func (s *Service) completeSnapshotLocked(sb *sandboxState, message string) {
	if sb == nil {
		return
	}
	if sb.Status != cleanroomv1.SandboxStatus_SANDBOX_STATUS_PROVISIONING {
		return
	}
	s.recordSandboxEventLocked(sb, cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY, message)
}

func (s *Service) GetSnapshot(ctx context.Context, req *cleanroomv1.GetSnapshotRequest) (*cleanroomv1.GetSnapshotResponse, error) {
	if req == nil || strings.TrimSpace(req.GetSnapshotId()) == "" {
		return nil, errors.New("missing snapshot_id")
	}

	store, err := s.snapshotStoreOrErr()
	if err != nil {
		return nil, err
	}
	record, ok, err := store.Get(ctx, strings.TrimSpace(req.GetSnapshotId()))
	if err != nil {
		return nil, fmt.Errorf("load snapshot %q: %w", req.GetSnapshotId(), err)
	}
	if !ok {
		return nil, fmt.Errorf("unknown snapshot %q", req.GetSnapshotId())
	}
	return &cleanroomv1.GetSnapshotResponse{Snapshot: cloneSnapshotRecord(record)}, nil
}

func (s *Service) ListSnapshots(ctx context.Context, _ *cleanroomv1.ListSnapshotsRequest) (*cleanroomv1.ListSnapshotsResponse, error) {
	store, err := s.snapshotStoreOrErr()
	if err != nil {
		return nil, err
	}
	records, err := store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}

	resp := &cleanroomv1.ListSnapshotsResponse{Snapshots: make([]*cleanroomv1.Snapshot, 0, len(records))}
	for _, record := range records {
		resp.Snapshots = append(resp.Snapshots, cloneSnapshotRecord(record))
	}
	return resp, nil
}

func (s *Service) DeleteSnapshot(ctx context.Context, req *cleanroomv1.DeleteSnapshotRequest) (*cleanroomv1.DeleteSnapshotResponse, error) {
	if req == nil || strings.TrimSpace(req.GetSnapshotId()) == "" {
		return nil, errors.New("missing snapshot_id")
	}
	snapshotID := strings.TrimSpace(req.GetSnapshotId())

	if err := s.beginSnapshotDelete(snapshotID); err != nil {
		return nil, err
	}
	defer s.finishSnapshotDelete(snapshotID)

	store, err := s.snapshotStoreOrErr()
	if err != nil {
		return nil, err
	}
	record, ok, err := store.Get(ctx, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("load snapshot %q: %w", snapshotID, err)
	}
	if !ok {
		return nil, fmt.Errorf("unknown snapshot %q", snapshotID)
	}

	adapter, ok := s.Backends[record.Backend]
	if !ok {
		return nil, fmt.Errorf("unknown backend %q", record.Backend)
	}
	snapshotAdapter, ok := adapter.(backend.SnapshottingAdapter)
	if !ok {
		return nil, fmt.Errorf("backend %q does not support snapshot deletion", record.Backend)
	}
	firecrackerCfg := withSnapshotDriver(record.Backend, runtimeconfig.MergeBackendConfig(s.Config, record.Backend, 0), record.StorageDriver)
	if err := snapshotAdapter.DeleteSnapshot(ctx, backend.DeleteSnapshotRequest{
		SnapshotID:        snapshotID,
		StorageRef:        record.StorageRef,
		FirecrackerConfig: firecrackerCfg,
	}); err != nil {
		return nil, fmt.Errorf("delete snapshot storage: %w", err)
	}
	if err := store.Delete(ctx, snapshotID); err != nil {
		return nil, fmt.Errorf("delete snapshot metadata: %w", err)
	}
	return &cleanroomv1.DeleteSnapshotResponse{
		SnapshotId: snapshotID,
		Deleted:    true,
		Message:    "snapshot deleted",
	}, nil
}

func (s *Service) DownloadSandboxFile(ctx context.Context, req *cleanroomv1.DownloadSandboxFileRequest) (*cleanroomv1.DownloadSandboxFileResponse, error) {
	if req == nil {
		return nil, errors.New("missing request")
	}
	sandboxID := strings.TrimSpace(req.GetSandboxId())
	if sandboxID == "" {
		return nil, errors.New("missing sandbox_id")
	}
	path := req.GetPath()
	if path == "" {
		return nil, errors.New("missing path")
	}
	if !strings.HasPrefix(path, "/") {
		return nil, errors.New("invalid path: must be absolute")
	}

	maxBytes := req.GetMaxBytes()
	if maxBytes <= 0 {
		maxBytes = s.downloadMaxBytesDefault()
	}

	s.mu.Lock()
	state, ok := s.sandboxes[sandboxID]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("unknown sandbox %q", sandboxID)
	}
	if state.Status != cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY {
		s.mu.Unlock()
		return nil, fmt.Errorf("sandbox %q is not ready", sandboxID)
	}
	adapter, ok := s.Backends[state.Backend]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("unknown backend %q", state.Backend)
	}
	downloader, ok := adapter.(backend.SandboxFileDownloadAdapter)
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("backend %q does not support sandbox file downloads", state.Backend)
	}
	if state.DownloadInProgress {
		s.mu.Unlock()
		return nil, fmt.Errorf("sandbox_busy: sandbox %q already has an active file download", sandboxID)
	}
	if state.RepositoryBusy {
		s.mu.Unlock()
		return nil, fmt.Errorf("sandbox_busy: sandbox %q is preparing repository state", sandboxID)
	}
	if activeID := strings.TrimSpace(state.ActiveExecutionID); activeID != "" {
		if activeExecution, ok := s.executions[executionKey(sandboxID, activeID)]; ok && !isFinalExecutionStatus(activeExecution.Status) {
			s.mu.Unlock()
			return nil, fmt.Errorf("sandbox_busy: sandbox %q already has active execution %q", sandboxID, activeID)
		}
	}
	state.DownloadInProgress = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if current, ok := s.sandboxes[sandboxID]; ok {
			current.DownloadInProgress = false
		}
		s.mu.Unlock()
	}()

	data, err := downloader.DownloadSandboxFile(ctx, sandboxID, path, maxBytes)
	if err != nil {
		return nil, fmt.Errorf("download sandbox file: %w", err)
	}
	return &cleanroomv1.DownloadSandboxFileResponse{
		SandboxId: sandboxID,
		Path:      path,
		Data:      data,
		SizeBytes: int64(len(data)),
	}, nil
}

func (s *Service) TerminateSandbox(ctx context.Context, req *cleanroomv1.TerminateSandboxRequest) (*cleanroomv1.TerminateSandboxResponse, error) {
	if req == nil || strings.TrimSpace(req.GetSandboxId()) == "" {
		return nil, errors.New("missing sandbox_id")
	}
	sandboxID := strings.TrimSpace(req.GetSandboxId())

	type cancelTarget struct {
		execID string
		cancel context.CancelFunc
	}
	cancellations := make([]cancelTarget, 0)
	var adapter backend.Adapter
	var backendName string
	alreadyStopped := false

	s.mu.Lock()
	state, ok := s.sandboxes[sandboxID]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("unknown sandbox %q", sandboxID)
	}
	backendName = state.Backend

	if state.Status == cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED {
		alreadyStopped = true
	} else {
		if currentAdapter, ok := s.Backends[state.Backend]; ok {
			adapter = currentAdapter
		}

		if state.Status != cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPING {
			state.Status = cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPING
			s.recordSandboxEventLocked(state, cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPING, "sandbox termination requested")
		}

		terminatedAt := s.clock().Now()
		for key, ex := range s.executions {
			if ex.SandboxID != sandboxID {
				continue
			}
			if isFinalExecutionStatus(ex.Status) {
				continue
			}
			ex.CancelRequested = true
			ex.CancelSignal = 15
			s.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
				SandboxId:   ex.SandboxID,
				ExecutionId: ex.ID,
				Status:      ex.Status,
				Payload:     &cleanroomv1.ExecutionStreamEvent_Message{Message: "execution canceled due to sandbox termination"},
				OccurredAt:  timestamppb.New(s.clock().Now()),
			})
			if ex.Status == cleanroomv1.ExecutionStatus_EXECUTION_STATUS_QUEUED {
				finished := terminatedAt
				s.finalizeExecutionWithoutPruneLocked(
					ex,
					cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED,
					cancelExitCode(ex.CancelSignal),
					ex.Message,
					"execution canceled before start (sandbox termination)",
					finished,
				)
				continue
			}
			if ex.Cancel != nil {
				cancellations = append(cancellations, cancelTarget{execID: key, cancel: ex.Cancel})
			}
		}
	}
	s.mu.Unlock()

	for _, target := range cancellations {
		target.cancel()
	}

	if !alreadyStopped && adapter != nil {
		if err := adapter.TerminateSandbox(ctx, sandboxID); err != nil {
			if s.Logger != nil {
				s.Logger.Warn("terminate backend sandbox failed", "sandbox_id", sandboxID, "backend", backendName, "error", err)
			}
			return nil, fmt.Errorf("terminate backend sandbox: %w", err)
		}
	}

	now := s.clock().Now()
	s.mu.Lock()
	state, ok = s.sandboxes[sandboxID]
	if ok && state.Status != cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED {
		state.Status = cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED
		s.recordSandboxEventLocked(state, cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED, "sandbox terminated")
		closeSandboxDoneLocked(state)
	}
	s.pruneStateLocked(now)
	s.mu.Unlock()

	resp := &cleanroomv1.TerminateSandboxResponse{
		SandboxId:  sandboxID,
		Terminated: true,
		Message:    "sandbox terminated",
	}

	if s.Logger != nil {
		s.Logger.Info("sandbox terminated",
			"sandbox_id", sandboxID,
			"backend", backendName,
		)
	}
	return resp, nil
}

func (s *Service) CreateExecution(ctx context.Context, req *cleanroomv1.CreateExecutionRequest) (resp *cleanroomv1.CreateExecutionResponse, err error) {
	if req == nil {
		return nil, errors.New("missing request")
	}
	sandboxID := strings.TrimSpace(req.GetSandboxId())
	if sandboxID == "" {
		return nil, errors.New("missing sandbox_id")
	}
	command := normalizeCommand(req.GetCommand())
	if len(command) == 0 {
		return nil, errors.New("missing command")
	}
	ctx, span := s.Observability.Tracer("github.com/buildkite/cleanroom/internal/controlservice").Start(
		ctx,
		observability.SpanExecutionCreate,
		trace.WithAttributes(
			attribute.String(observability.AttrSandboxID, sandboxID),
			attribute.Int(observability.AttrCommandArgc, len(command)),
		),
	)
	logger := observability.WithTraceContext(s.Logger, ctx)
	defer func() {
		if resp != nil && resp.GetExecution() != nil {
			span.SetAttributes(
				attribute.String(observability.AttrExecutionID, resp.GetExecution().GetExecutionId()),
				attribute.String(observability.AttrExecutionKind, resp.GetExecution().GetKind().String()),
			)
		}
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		span.End()
	}()
	executionEnv, err := normalizeExecutionEnv(req.GetEnv())
	if err != nil {
		return nil, err
	}
	repository := repositorycheckout.FromProto(req.GetRepositoryCheckout())
	if repository != nil {
		if err := repository.ValidateBootstrap(); err != nil {
			return nil, err
		}
	}

	execOpts := executionOptions{}
	tty := false
	if opts := req.GetOptions(); opts != nil {
		execOpts = executionOptions{
			LaunchSeconds: opts.GetLaunchSeconds(),
		}
		tty = opts.GetTty()
	}
	kind, err := resolveExecutionKind(req.GetKind(), tty)
	if err != nil {
		return nil, err
	}
	if kind == cleanroomv1.ExecutionKind_EXECUTION_KIND_INTERACTIVE {
		tty = true
	}

	now := s.clock().Now()
	executionID := s.ids().NewExecutionID()

	s.mu.Lock()
	s.ensureMapsLocked()

	sandbox, ok := s.sandboxes[sandboxID]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("unknown sandbox %q", sandboxID)
	}
	span.SetAttributes(attribute.String(observability.AttrBackend, sandbox.Backend))
	if sandbox.Status != cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY {
		s.mu.Unlock()
		return nil, fmt.Errorf("sandbox %q is not ready", sandboxID)
	}
	adapter, ok := s.Backends[sandbox.Backend]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("unknown backend %q", sandbox.Backend)
	}
	sandboxBackend := sandbox.Backend
	if strings.TrimSpace(sandbox.ActiveExecutionID) != "" {
		if activeExecution, ok := s.executions[executionKey(sandboxID, sandbox.ActiveExecutionID)]; ok && !isFinalExecutionStatus(activeExecution.Status) {
			s.mu.Unlock()
			return nil, fmt.Errorf("sandbox_busy: sandbox %q already has active execution %q", sandboxID, sandbox.ActiveExecutionID)
		}
	}
	if sandbox.DownloadInProgress {
		s.mu.Unlock()
		return nil, fmt.Errorf("sandbox_busy: sandbox %q currently has an active file download", sandboxID)
	}
	if sandbox.RepositoryBusy {
		s.mu.Unlock()
		return nil, fmt.Errorf("sandbox_busy: sandbox %q is preparing repository state", sandboxID)
	}
	sandboxPolicy := sandbox.Policy
	firecrackerCfg := sandbox.Firecracker
	sandboxRepository := cloneRepositoryCheckout(sandbox.Repository)
	imageRef := ""
	imageDigest := ""
	if sandboxPolicy != nil {
		imageRef = sandboxPolicy.ImageRef
		imageDigest = sandboxPolicy.ImageDigest
	}
	s.mu.Unlock()

	if repository != nil {
		if err := validateRepositoryCheckoutForPolicy(sandboxPolicy, repository); err != nil {
			return nil, err
		}
		if err := s.preparePersistentSandboxRepository(ctx, sandboxID, sandboxPolicy, firecrackerCfg, adapter, repository); err != nil {
			return nil, err
		}
		command = repositorycheckout.WrapCommandInWorkdir(command, repository)
	} else if sandboxRepository != nil {
		command = repositorycheckout.WrapCommandInWorkdir(command, sandboxRepository)
	}
	runRepository := repository
	if runRepository == nil {
		runRepository = sandboxRepository
	}
	var preRunBefore []string
	if sandboxPolicy != nil && sandboxPolicy.Run.HasBefore() {
		preRunBefore = append([]string(nil), sandboxPolicy.Run.Before...)
		if runRepository != nil {
			preRunBefore = repositorycheckout.WrapCommandInWorkdir(preRunBefore, runRepository)
		}
	}

	s.mu.Lock()
	s.ensureMapsLocked()
	sandbox, ok = s.sandboxes[sandboxID]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("unknown sandbox %q", sandboxID)
	}
	if sandbox.Status != cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY {
		s.mu.Unlock()
		return nil, fmt.Errorf("sandbox %q is not ready", sandboxID)
	}
	if _, ok := s.Backends[sandbox.Backend]; !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("unknown backend %q", sandbox.Backend)
	}
	if strings.TrimSpace(sandbox.ActiveExecutionID) != "" {
		if activeExecution, ok := s.executions[executionKey(sandboxID, sandbox.ActiveExecutionID)]; ok && !isFinalExecutionStatus(activeExecution.Status) {
			s.mu.Unlock()
			return nil, fmt.Errorf("sandbox_busy: sandbox %q already has active execution %q", sandboxID, sandbox.ActiveExecutionID)
		}
		sandbox.ActiveExecutionID = ""
	}
	if sandbox.DownloadInProgress {
		s.mu.Unlock()
		return nil, fmt.Errorf("sandbox_busy: sandbox %q currently has an active file download", sandboxID)
	}
	if sandbox.RepositoryBusy {
		s.mu.Unlock()
		return nil, fmt.Errorf("sandbox_busy: sandbox %q is preparing repository state", sandboxID)
	}
	if sandbox.RepositoryHasChangeset && sandbox.RepositoryChangesetPendingExecution {
		sandbox.RepositoryChangesetPendingExecution = false
	}
	ex := &executionState{
		ID:                executionID,
		SandboxID:         sandboxID,
		ImageRef:          imageRef,
		ImageDigest:       imageDigest,
		Command:           append([]string(nil), command...),
		PreRunBefore:      preRunBefore,
		Env:               executionEnv,
		Options:           execOpts,
		TTY:               tty,
		Kind:              kind,
		Status:            cleanroomv1.ExecutionStatus_EXECUTION_STATUS_QUEUED,
		PreRunActive:      len(preRunBefore) > 0,
		ParentSpanContext: trace.SpanContextFromContext(ctx),
		events:            newEventFeed[*cleanroomv1.ExecutionStreamEvent](s.retention().maxRetainedExecutionEvents),
		Done:              make(chan struct{}),
	}
	s.executions[executionKey(sandboxID, executionID)] = ex
	sandbox.LastExecutionID = executionID
	sandbox.ActiveExecutionID = executionID
	sandbox.UpdatedAt = now
	s.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Status:      cleanroomv1.ExecutionStatus_EXECUTION_STATUS_QUEUED,
		Payload:     &cleanroomv1.ExecutionStreamEvent_Message{Message: "execution queued"},
		OccurredAt:  timestamppb.New(now),
	})
	s.pruneStateLocked(now)

	resp = &cleanroomv1.CreateExecutionResponse{Execution: cloneExecutionLocked(ex)}
	s.mu.Unlock()

	go s.runExecution(sandboxID, executionID)

	if logger != nil {
		logger.Info("execution created",
			observability.LogFieldSandboxID, sandboxID,
			observability.LogFieldExecutionID, executionID,
			observability.LogFieldBackend, sandboxBackend,
			"command_argc", len(command),
			"tty", tty,
			"kind", kind.String(),
		)
	}
	return resp, nil
}

func (s *Service) AttachExecution(_ context.Context, req *cleanroomv1.AttachExecutionRequest) (*cleanroomv1.AttachExecutionResponse, error) {
	if req == nil {
		return nil, errors.New("missing request")
	}
	sandboxID := strings.TrimSpace(req.GetSandboxId())
	executionID := strings.TrimSpace(req.GetExecutionId())
	if sandboxID == "" {
		return nil, errors.New("missing sandbox_id")
	}
	if executionID == "" {
		return nil, errors.New("missing execution_id")
	}

	now := s.clock().Now()
	token, err := s.ids().NewSessionToken()
	if err != nil {
		return nil, fmt.Errorf("generate session token: %w", err)
	}
	sessionID := s.ids().NewInteractiveSessionID()

	s.mu.Lock()
	defer s.mu.Unlock()

	grant, err := s.interactive.open(
		s.executions,
		now,
		s.timeouts().interactiveSessionTokenTTL,
		sessionID,
		token,
		sandboxID,
		executionID,
		req.GetInitialCols(),
		req.GetInitialRows(),
	)
	if err != nil {
		return nil, err
	}

	return &cleanroomv1.AttachExecutionResponse{
		SessionId:           grant.SessionID,
		SessionToken:        grant.SessionToken,
		ExpiresAt:           timestamppb.New(grant.ExpiresAt),
		QuicEndpoint:        grant.QuicEndpoint,
		Alpn:                grant.Alpn,
		ServerCertPinSha256: grant.ServerCertPinSHA256,
	}, nil
}

func (s *Service) InspectExecution(_ context.Context, req *cleanroomv1.InspectExecutionRequest) (*cleanroomv1.InspectExecutionResponse, error) {
	if req == nil {
		return nil, errors.New("missing request")
	}
	sandboxID := strings.TrimSpace(req.GetSandboxId())
	executionID := strings.TrimSpace(req.GetExecutionId())
	if executionID == "" {
		return nil, errors.New("missing execution_id")
	}

	snapshot, err := s.ExecutionSnapshot(sandboxID, executionID)
	if err != nil {
		return nil, err
	}

	resp := &cleanroomv1.InspectExecutionResponse{
		Execution:    snapshot.Execution,
		Message:      snapshot.Message,
		Stdout:       snapshot.Stdout,
		Stderr:       snapshot.Stderr,
		ImageRef:     snapshot.ImageRef,
		ImageDigest:  snapshot.ImageDigest,
		ArtifactsDir: snapshot.RunDir,
		PlanPath:     snapshot.PlanPath,
		LaunchedVm:   snapshot.Launched,
		TraceId:      snapshot.TraceID,
	}

	if obs, err := loadExecutionObservability(snapshot.RunDir); err != nil {
		return nil, err
	} else if obs != nil {
		resp.Observability = obs.Raw
		if strings.TrimSpace(resp.GetTraceId()) == "" {
			resp.TraceId = obs.TraceID
		}
	}

	if traceURL, err := runtimeconfig.RenderTraceURL(
		s.Config.Observability,
		resp.GetTraceId(),
		executionID,
		resp.GetExecution().GetSandboxId(),
	); err == nil {
		resp.TraceUrl = traceURL
	}

	return resp, nil
}

func (s *Service) ConfigureInteractiveTransport(endpoint, alpn, certPinSHA256 string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.interactive.configureTransport(endpoint, alpn, certPinSHA256)
}

type executionObservability struct {
	Raw     *structpb.Struct
	TraceID string
}

func loadExecutionObservability(artifactsDir string) (*executionObservability, error) {
	if strings.TrimSpace(artifactsDir) == "" {
		return nil, nil
	}
	obsPath := filepath.Join(strings.TrimSpace(artifactsDir), "execution-observability.json")
	b, err := os.ReadFile(obsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", obsPath, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(b, &payload); err != nil {
		return nil, fmt.Errorf("parse %s: %w", obsPath, err)
	}
	obs, err := structpb.NewStruct(payload)
	if err != nil {
		return nil, fmt.Errorf("convert %s to protobuf struct: %w", obsPath, err)
	}
	traceID, _ := payload["trace_id"].(string)
	return &executionObservability{Raw: obs, TraceID: strings.TrimSpace(traceID)}, nil
}

func internalBootstrapArtifactsDir(sandboxID, executionID string) string {
	baseDir, err := paths.StateBaseDir()
	if err != nil {
		baseDir = filepath.Join(os.TempDir(), "cleanroom")
	}
	return filepath.Join(baseDir, "internal", "bootstrap-executions", sandboxID, executionID)
}

func withRunDir(cfg backend.FirecrackerConfig, runDir string) backend.FirecrackerConfig {
	cfg.RunDir = runDir
	return cfg
}

func (s *Service) terminateCreatedSandbox(ctx context.Context, adapter backend.Adapter, sandboxID string) error {
	if adapter == nil || strings.TrimSpace(sandboxID) == "" {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, s.timeouts().bootstrapCleanupTimeout)
	defer cancel()
	err := adapter.TerminateSandbox(cleanupCtx, sandboxID)
	if err != nil {
		return err
	}

	s.mu.Lock()
	if sandbox, ok := s.sandboxes[sandboxID]; ok {
		s.dropSandboxLocked(sandboxID, sandbox)
	}
	s.mu.Unlock()

	return err
}

func (s *Service) ConsumeInteractiveSession(sessionID, token string) (*InteractiveSession, error) {
	id := strings.TrimSpace(sessionID)
	tok := strings.TrimSpace(token)
	if id == "" {
		return nil, errors.New("missing session_id")
	}
	if tok == "" {
		return nil, errors.New("missing session_token")
	}

	now := s.clock().Now()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMapsLocked()

	return s.interactive.consume(s.executions, now, id, tok)
}

func (s *Service) ReleaseInteractiveExecution(sandboxID, executionID string) {
	sandboxID = strings.TrimSpace(sandboxID)
	executionID = strings.TrimSpace(executionID)
	if sandboxID == "" || executionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMapsLocked()
	s.interactive.release(sandboxID, executionID)
}

func (s *Service) GetExecution(_ context.Context, req *cleanroomv1.GetExecutionRequest) (*cleanroomv1.GetExecutionResponse, error) {
	if req == nil {
		return nil, errors.New("missing request")
	}
	sandboxID := strings.TrimSpace(req.GetSandboxId())
	executionID := strings.TrimSpace(req.GetExecutionId())
	if executionID == "" {
		return nil, errors.New("missing execution_id")
	}

	s.mu.RLock()
	ex, err := s.lookupExecutionLocked(sandboxID, executionID)
	if err != nil {
		s.mu.RUnlock()
		return nil, err
	}
	resp := &cleanroomv1.GetExecutionResponse{Execution: cloneExecutionLocked(ex)}
	s.mu.RUnlock()
	return resp, nil
}

func (s *Service) CancelExecution(_ context.Context, req *cleanroomv1.CancelExecutionRequest) (*cleanroomv1.CancelExecutionResponse, error) {
	if req == nil {
		return nil, errors.New("missing request")
	}
	sandboxID := strings.TrimSpace(req.GetSandboxId())
	executionID := strings.TrimSpace(req.GetExecutionId())
	if sandboxID == "" {
		return nil, errors.New("missing sandbox_id")
	}
	if executionID == "" {
		return nil, errors.New("missing execution_id")
	}

	var cancel context.CancelFunc
	var accepted bool
	var status cleanroomv1.ExecutionStatus
	signalNum := req.GetSignal()
	if signalNum == 0 {
		signalNum = 2
	}

	now := s.clock().Now()
	s.mu.Lock()
	ex, ok := s.executions[executionKey(sandboxID, executionID)]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("unknown execution %q in sandbox %q", executionID, sandboxID)
	}
	status = ex.Status
	if isFinalExecutionStatus(ex.Status) {
		s.mu.Unlock()
		return &cleanroomv1.CancelExecutionResponse{
			SandboxId:   sandboxID,
			ExecutionId: executionID,
			Accepted:    false,
			Status:      status,
		}, nil
	}

	ex.CancelRequested = true
	ex.CancelSignal = signalNum
	accepted = true
	s.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Status:      ex.Status,
		Payload:     &cleanroomv1.ExecutionStreamEvent_Message{Message: fmt.Sprintf("cancel requested (signal=%d)", signalNum)},
		OccurredAt:  timestamppb.New(now),
	})

	if ex.Status == cleanroomv1.ExecutionStatus_EXECUTION_STATUS_QUEUED {
		finished := now
		s.finalizeExecutionLocked(
			ex,
			cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED,
			cancelExitCode(signalNum),
			ex.Message,
			"execution canceled before start",
			finished,
		)
		status = cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED
		s.mu.Unlock()
		return &cleanroomv1.CancelExecutionResponse{
			SandboxId:   sandboxID,
			ExecutionId: executionID,
			Accepted:    true,
			Status:      status,
		}, nil
	}

	if ex.Cancel != nil {
		cancel = ex.Cancel
	}
	status = ex.Status
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	return &cleanroomv1.CancelExecutionResponse{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Accepted:    accepted,
		Status:      status,
	}, nil
}

func (s *Service) WriteExecutionStdin(sandboxID, executionID string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	sandboxID = strings.TrimSpace(sandboxID)
	executionID = strings.TrimSpace(executionID)
	if sandboxID == "" {
		return errors.New("missing sandbox_id")
	}
	if executionID == "" {
		return errors.New("missing execution_id")
	}

	payload := append([]byte(nil), data...)
	clock := s.clock()
	var deadline time.Time
	for {
		var (
			writeFn func([]byte) error
			done    <-chan struct{}
		)
		s.mu.RLock()
		ex, ok := s.executions[executionKey(sandboxID, executionID)]
		if !ok {
			s.mu.RUnlock()
			return fmt.Errorf("unknown execution %q in sandbox %q", executionID, sandboxID)
		}
		if isFinalExecutionStatus(ex.Status) {
			s.mu.RUnlock()
			return errors.New("execution is not running")
		}
		if deadline.IsZero() {
			deadline = clock.Now().Add(s.executionAttachStdinTotalWaitLocked(sandboxID, ex))
		}
		writeFn = ex.AttachStdin
		done = ex.Done
		s.mu.RUnlock()

		if writeFn != nil {
			return writeFn(payload)
		}
		if !deadline.IsZero() && clock.Now().After(deadline) {
			return ErrExecutionStdinUnsupported
		}
		select {
		case <-done:
		case <-clock.After(s.timeouts().attachPollInterval):
		}
	}
}

func (s *Service) CloseExecutionStdin(sandboxID, executionID string) error {
	sandboxID = strings.TrimSpace(sandboxID)
	executionID = strings.TrimSpace(executionID)
	if sandboxID == "" {
		return errors.New("missing sandbox_id")
	}
	if executionID == "" {
		return errors.New("missing execution_id")
	}

	clock := s.clock()
	var deadline time.Time
	for {
		var (
			closeFn func() error
			done    <-chan struct{}
		)
		s.mu.RLock()
		ex, ok := s.executions[executionKey(sandboxID, executionID)]
		if !ok {
			s.mu.RUnlock()
			return fmt.Errorf("unknown execution %q in sandbox %q", executionID, sandboxID)
		}
		if isFinalExecutionStatus(ex.Status) {
			s.mu.RUnlock()
			return errors.New("execution is not running")
		}
		if deadline.IsZero() {
			deadline = clock.Now().Add(s.executionAttachStdinTotalWaitLocked(sandboxID, ex))
		}
		closeFn = ex.AttachCloseStdin
		done = ex.Done
		s.mu.RUnlock()

		if closeFn != nil {
			return closeFn()
		}
		if !deadline.IsZero() && clock.Now().After(deadline) {
			return ErrExecutionStdinUnsupported
		}
		select {
		case <-done:
		case <-clock.After(s.timeouts().attachPollInterval):
		}
	}
}

func (s *Service) executionAttachStdinWaitLocked(sandboxID string, ex *executionState) time.Duration {
	wait := s.timeouts().attachStdinRegistrationWait
	launchSeconds := int64(0)
	if ex != nil && ex.Options.LaunchSeconds > 0 {
		launchSeconds = ex.Options.LaunchSeconds
	} else if sandbox, ok := s.sandboxes[sandboxID]; ok && sandbox.Firecracker.LaunchSeconds > 0 {
		launchSeconds = sandbox.Firecracker.LaunchSeconds
	}
	if launchSeconds <= 0 {
		return wait
	}
	launchWait := time.Duration(launchSeconds) * time.Second
	if launchWait > wait {
		return launchWait
	}
	return wait
}

func (s *Service) executionAttachStdinTotalWaitLocked(sandboxID string, ex *executionState) time.Duration {
	wait := s.executionAttachStdinWaitLocked(sandboxID, ex)
	if ex != nil && ex.PreRunActive {
		return wait + s.executionAttachStdinWaitLocked(sandboxID, ex)
	}
	return wait
}

func (s *Service) executionAttachResizeTotalWaitLocked(sandboxID string, ex *executionState) time.Duration {
	wait := s.timeouts().attachResizeRegistrationWait
	if ex != nil && ex.PreRunActive {
		return wait + s.executionAttachStdinWaitLocked(sandboxID, ex)
	}
	return wait
}

func (s *Service) ResizeExecutionTTY(sandboxID, executionID string, cols, rows uint32) error {
	sandboxID = strings.TrimSpace(sandboxID)
	executionID = strings.TrimSpace(executionID)
	if sandboxID == "" {
		return errors.New("missing sandbox_id")
	}
	if executionID == "" {
		return errors.New("missing execution_id")
	}

	clock := s.clock()
	var deadline time.Time
	for {
		var (
			resizeFn func(cols, rows uint32) error
			done     <-chan struct{}
		)
		s.mu.RLock()
		ex, ok := s.executions[executionKey(sandboxID, executionID)]
		if !ok {
			s.mu.RUnlock()
			return fmt.Errorf("unknown execution %q in sandbox %q", executionID, sandboxID)
		}
		if isFinalExecutionStatus(ex.Status) {
			s.mu.RUnlock()
			return errors.New("execution is not running")
		}
		if deadline.IsZero() {
			deadline = clock.Now().Add(s.executionAttachResizeTotalWaitLocked(sandboxID, ex))
		}
		resizeFn = ex.AttachResize
		done = ex.Done
		s.mu.RUnlock()

		if resizeFn != nil {
			return resizeFn(cols, rows)
		}
		if !deadline.IsZero() && clock.Now().After(deadline) {
			return ErrExecutionResizeUnsupported
		}
		select {
		case <-done:
		case <-clock.After(s.timeouts().attachPollInterval):
		}
	}
}

func (s *Service) SubscribeSandboxEvents(sandboxID string) ([]*cleanroomv1.SandboxEvent, <-chan *cleanroomv1.SandboxEvent, <-chan struct{}, func(), error) {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return nil, nil, nil, nil, errors.New("missing sandbox_id")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	sb, ok := s.sandboxes[sandboxID]
	if !ok {
		return nil, nil, nil, nil, fmt.Errorf("unknown sandbox %q", sandboxID)
	}

	history, updates, subID := sb.events.subscribe(sb.Done, 64)
	done := sb.Done

	unsubscribe := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		subSB, ok := s.sandboxes[sandboxID]
		if !ok {
			return
		}
		subSB.events.unsubscribe(subID)
	}

	return history, updates, done, unsubscribe, nil
}

func (s *Service) SubscribeExecutionEvents(sandboxID, executionID string) ([]*cleanroomv1.ExecutionStreamEvent, <-chan *cleanroomv1.ExecutionStreamEvent, <-chan struct{}, func(), error) {
	sandboxID = strings.TrimSpace(sandboxID)
	executionID = strings.TrimSpace(executionID)
	if sandboxID == "" {
		return nil, nil, nil, nil, errors.New("missing sandbox_id")
	}
	if executionID == "" {
		return nil, nil, nil, nil, errors.New("missing execution_id")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	ex, ok := s.executions[executionKey(sandboxID, executionID)]
	if !ok {
		return nil, nil, nil, nil, fmt.Errorf("unknown execution %q in sandbox %q", executionID, sandboxID)
	}

	history, updates, subID := ex.events.subscribe(ex.Done, 2048)
	done := ex.Done

	unsubscribe := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		subEx, ok := s.executions[executionKey(sandboxID, executionID)]
		if !ok {
			return
		}
		subEx.events.unsubscribe(subID)
	}

	return history, updates, done, unsubscribe, nil
}

func (s *Service) WaitExecution(ctx context.Context, sandboxID, executionID string) (*cleanroomv1.Execution, error) {
	done, err := s.executionDoneChannel(sandboxID, executionID)
	if err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-done:
	}

	s.mu.RLock()
	ex, ok := s.executions[executionKey(sandboxID, executionID)]
	if !ok {
		s.mu.RUnlock()
		return nil, fmt.Errorf("unknown execution %q in sandbox %q", executionID, sandboxID)
	}
	out := cloneExecutionLocked(ex)
	s.mu.RUnlock()
	return out, nil
}

func (s *Service) ExecutionSnapshot(sandboxID, executionID string) (*executionSnapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ex, err := s.lookupExecutionLocked(sandboxID, executionID)
	if err != nil {
		return nil, err
	}
	return &executionSnapshot{
		Execution:   cloneExecutionLocked(ex),
		ImageRef:    ex.ImageRef,
		ImageDigest: ex.ImageDigest,
		Message:     ex.Message,
		Stdout:      ex.Stdout,
		Stderr:      ex.Stderr,
		PlanPath:    ex.PlanPath,
		RunDir:      ex.RunDir,
		Launched:    ex.LaunchedVM,
		TraceID:     observability.TraceIDFromSpanContext(ex.ParentSpanContext),
	}, nil
}

func (s *Service) lookupExecutionLocked(sandboxID, executionID string) (*executionState, error) {
	if strings.TrimSpace(sandboxID) != "" {
		ex, ok := s.executions[executionKey(sandboxID, executionID)]
		if !ok {
			return nil, fmt.Errorf("unknown execution %q in sandbox %q", executionID, sandboxID)
		}
		return ex, nil
	}

	var match *executionState
	for _, ex := range s.executions {
		if ex == nil || ex.ID != executionID {
			continue
		}
		if match != nil && match.SandboxID != ex.SandboxID {
			return nil, fmt.Errorf("execution %q is not globally unique; specify sandbox_id", executionID)
		}
		match = ex
	}
	if match == nil {
		return nil, fmt.Errorf("unknown execution %q", executionID)
	}
	return match, nil
}

func (s *Service) runExecution(sandboxID, executionID string) {
	key := executionKey(sandboxID, executionID)

	s.mu.Lock()
	ex, ok := s.executions[key]
	if !ok {
		s.mu.Unlock()
		return
	}
	if isFinalExecutionStatus(ex.Status) {
		s.mu.Unlock()
		return
	}
	sb, ok := s.sandboxes[sandboxID]
	if !ok {
		finished := s.clock().Now()
		s.finalizeExecutionLocked(
			ex,
			cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED,
			1,
			ex.Message,
			"sandbox no longer exists",
			finished,
		)
		s.mu.Unlock()
		return
	}
	if sb.Status != cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY {
		finalStatus := cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED
		exitCode := int32(1)
		if sb.Status == cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPING || sb.Status == cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED {
			finalStatus = cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED
			exitCode = cancelExitCode(ex.CancelSignal)
		}
		finished := s.clock().Now()
		s.finalizeExecutionLocked(
			ex,
			finalStatus,
			exitCode,
			ex.Message,
			fmt.Sprintf("sandbox %q is not ready", sandboxID),
			finished,
		)
		s.mu.Unlock()
		return
	}
	adapter, ok := s.Backends[sb.Backend]
	if !ok {
		finished := s.clock().Now()
		s.finalizeExecutionLocked(
			ex,
			cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED,
			1,
			ex.Message,
			fmt.Sprintf("unknown backend %q", sb.Backend),
			finished,
		)
		s.mu.Unlock()
		return
	}

	runParentCtx := context.Background()
	if ex.ParentSpanContext.IsValid() {
		runParentCtx = trace.ContextWithSpanContext(runParentCtx, ex.ParentSpanContext)
	}
	runCtx, cancel := context.WithCancel(runParentCtx)
	ex.Cancel = cancel

	started := s.clock().Now()
	ex.StartedAt = &started
	ex.Status = cleanroomv1.ExecutionStatus_EXECUTION_STATUS_RUNNING
	if sb.Policy != nil {
		ex.ImageRef = sb.Policy.ImageRef
		ex.ImageDigest = sb.Policy.ImageDigest
	}
	s.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Status:      ex.Status,
		Payload:     &cleanroomv1.ExecutionStreamEvent_Message{Message: "execution started"},
		OccurredAt:  timestamppb.New(started),
	})

	firecrackerCfg := sb.Firecracker
	if strings.TrimSpace(firecrackerCfg.RunDir) == "" {
		if executionBaseDir, err := paths.ExecutionBaseDir(); err == nil {
			firecrackerCfg.RunDir = filepath.Join(executionBaseDir, ex.ID)
		}
	}
	if ex.Options.LaunchSeconds != 0 {
		firecrackerCfg.LaunchSeconds = ex.Options.LaunchSeconds
	}
	preRunBefore := append([]string(nil), ex.PreRunBefore...)
	if len(preRunBefore) > 0 {
		ex.PreRunActive = true
		s.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
			SandboxId:   sandboxID,
			ExecutionId: executionID,
			Status:      ex.Status,
			Payload:     &cleanroomv1.ExecutionStreamEvent_Message{Message: "running sandbox.run.before"},
			OccurredAt:  timestamppb.New(s.clock().Now()),
		})
	}

	executionReq := backend.ExecutionRequest{
		SandboxID:         sandboxID,
		ExecutionID:       ex.ID,
		Command:           append([]string(nil), ex.Command...),
		Env:               append([]string(nil), ex.Env...),
		TTY:               ex.TTY,
		Policy:            sb.Policy,
		FirecrackerConfig: firecrackerCfg,
	}
	runCtx, span := s.Observability.Tracer("github.com/buildkite/cleanroom/internal/controlservice").Start(
		runCtx,
		observability.SpanExecutionRun,
		trace.WithAttributes(
			attribute.String(observability.AttrBackend, sb.Backend),
			attribute.String(observability.AttrExecutionID, ex.ID),
			attribute.String(observability.AttrExecutionKind, ex.Kind.String()),
			attribute.String(observability.AttrSandboxID, sandboxID),
			attribute.Int(observability.AttrCommandArgc, len(ex.Command)),
		),
	)
	runLogger := observability.WithTraceContext(s.Logger, runCtx)
	s.mu.Unlock()
	defer span.End()
	recordExecutionMetrics := func(status cleanroomv1.ExecutionStatus, finished time.Time) {
		if metrics := s.serviceMetrics(); metrics != nil {
			metrics.RecordExecution(runCtx, sb.Backend, executionKindMetricValue(ex.Kind), executionOutcomeMetricValue(status), finished.Sub(started))
		}
	}

	if len(preRunBefore) > 0 {
		preRunExecutionID := s.ids().NewExecutionID()
		preRunResult, preRunErr := s.runPersistentSandboxCommand(runCtx, adapter, sandboxID, sb.Policy, firecrackerCfg, preRunExecutionID, preRunBefore, ex.Env, s.executionAuxOutputStream(key))

		s.mu.Lock()
		ex, ok = s.executions[key]
		if !ok {
			s.mu.Unlock()
			return
		}
		ex.PreRunActive = false
		s.applyExecutionResultMetadataLocked(ex, preRunResult)
		if preRunErr != nil {
			span.RecordError(preRunErr)
			span.SetStatus(codes.Error, preRunErr.Error())
			finalStatus, exitCode := executionRunErrorStatus(ex, runCtx)
			if strings.TrimSpace(preRunErr.Error()) != "" {
				s.appendExecutionStderrLocked(ex, finalStatus, []byte(preRunErr.Error()+"\n"))
			}
			clearExecutionRuntimeHandlesLocked(ex)
			finished := s.clock().Now()
			recordExecutionMetrics(finalStatus, finished)
			s.finalizeExecutionLocked(ex, finalStatus, exitCode, preRunErr.Error(), "", finished)
			if sb, ok := s.sandboxes[sandboxID]; ok && sb.ActiveExecutionID == executionID {
				sb.ActiveExecutionID = ""
				sb.UpdatedAt = s.clock().Now()
			}
			s.mu.Unlock()
			return
		}
		if preRunResult == nil {
			clearExecutionRuntimeHandlesLocked(ex)
			finished := s.clock().Now()
			recordExecutionMetrics(cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED, finished)
			s.finalizeExecutionLocked(ex, cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED, 1, "sandbox.run.before returned no result", "", finished)
			if sb, ok := s.sandboxes[sandboxID]; ok && sb.ActiveExecutionID == executionID {
				sb.ActiveExecutionID = ""
				sb.UpdatedAt = s.clock().Now()
			}
			s.mu.Unlock()
			return
		}
		s.mergeBufferedResultOutputLocked(ex, preRunResult, true)
		if preRunResult.ExitCode != 0 {
			msg := strings.TrimSpace(preRunResult.Message)
			if msg == "" {
				if ex.CancelRequested {
					msg = "execution canceled before command start"
				} else {
					msg = fmt.Sprintf("sandbox.run.before failed with exit code %d", preRunResult.ExitCode)
				}
			}
			if msg != "" && !strings.Contains(ex.Stderr, msg) {
				s.appendExecutionStderrLocked(ex, cleanroomv1.ExecutionStatus_EXECUTION_STATUS_RUNNING, []byte(msg+"\n"))
			}
			finished := s.clock().Now()
			finalStatus := cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED
			finalExitCode := int32(preRunResult.ExitCode)
			if ex.CancelRequested {
				finalStatus = cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED
				finalExitCode = cancelExitCode(ex.CancelSignal)
			}
			clearExecutionRuntimeHandlesLocked(ex)
			recordExecutionMetrics(finalStatus, finished)
			s.finalizeExecutionLocked(ex, finalStatus, finalExitCode, msg, "", finished)
			if sb, ok := s.sandboxes[sandboxID]; ok && sb.ActiveExecutionID == executionID {
				sb.ActiveExecutionID = ""
				sb.UpdatedAt = s.clock().Now()
			}
			s.mu.Unlock()
			return
		}
		if ex.CancelRequested {
			clearExecutionRuntimeHandlesLocked(ex)
			finished := s.clock().Now()
			recordExecutionMetrics(cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED, finished)
			s.finalizeExecutionLocked(ex, cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED, cancelExitCode(ex.CancelSignal), ex.Message, "execution canceled before command start", finished)
			if sb, ok := s.sandboxes[sandboxID]; ok && sb.ActiveExecutionID == executionID {
				sb.ActiveExecutionID = ""
				sb.UpdatedAt = s.clock().Now()
			}
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
	}

	commandOutputPrefixStdout := ""
	commandOutputPrefixStderr := ""
	commandStreamStdout := newRetainedOutputCapture(s.retention().maxRetainedExecutionOutputBytes)
	commandStreamStderr := newRetainedOutputCapture(s.retention().maxRetainedExecutionOutputBytes)

	s.mu.Lock()
	if ex, ok := s.executions[key]; ok {
		commandOutputPrefixStdout = ex.Stdout
		commandOutputPrefixStderr = ex.Stderr
	}
	s.mu.Unlock()

	result, err := s.runAdapterExecution(runCtx, adapter, executionReq, key, commandStreamStdout, commandStreamStderr)

	s.mu.Lock()
	defer s.mu.Unlock()
	ex, ok = s.executions[key]
	if !ok {
		return
	}
	if sb, ok := s.sandboxes[sandboxID]; ok && sb.ActiveExecutionID == executionID {
		sb.ActiveExecutionID = ""
		sb.UpdatedAt = s.clock().Now()
	}

	clearExecutionRuntimeHandlesLocked(ex)

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		finalStatus, exitCode := executionRunErrorStatus(ex, runCtx)
		if strings.TrimSpace(err.Error()) != "" {
			s.appendExecutionStderrLocked(ex, finalStatus, []byte(err.Error()+"\n"))
		}
		finished := s.clock().Now()
		recordExecutionMetrics(finalStatus, finished)
		s.finalizeExecutionLocked(ex, finalStatus, exitCode, err.Error(), "", finished)
		if runLogger != nil {
			runLogger.Warn("execution failed",
				observability.LogFieldSandboxID, ex.SandboxID,
				observability.LogFieldExecutionID, ex.ID,
				observability.LogFieldBackend, sb.Backend,
				"image_ref", ex.ImageRef,
				"image_digest", ex.ImageDigest,
				"status", ex.Status.String(),
				"error", err,
			)
		}
		return
	}

	s.applyExecutionResultMetadataLocked(ex, result)
	ex.Message = result.Message
	s.mergeBufferedResultOutputFromStreamLocked(ex, result, commandOutputPrefixStdout, commandOutputPrefixStderr, commandStreamStdout.String(), commandStreamStderr.String())

	if result.ExitCode != 0 && strings.TrimSpace(result.Message) != "" && !strings.Contains(ex.Stderr, result.Message) {
		msg := result.Message + "\n"
		s.appendExecutionStderrLocked(ex, cleanroomv1.ExecutionStatus_EXECUTION_STATUS_RUNNING, []byte(msg))
	}

	finalStatus := cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED
	finalExitCode := int32(result.ExitCode)
	if ex.CancelRequested {
		finalStatus = cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED
		finalExitCode = cancelExitCode(ex.CancelSignal)
	} else if result.ExitCode == 0 {
		finalStatus = cleanroomv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED
	}
	span.SetAttributes(
		attribute.Bool(observability.AttrVMLaunched, result.LaunchedVM),
		attribute.Int(observability.AttrExitCode, result.ExitCode),
		attribute.String(observability.AttrExecutionStatus, finalStatus.String()),
	)
	if finalStatus == cleanroomv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED {
		span.SetStatus(codes.Ok, "")
	} else {
		span.SetStatus(codes.Error, finalStatus.String())
	}
	finished := s.clock().Now()
	recordExecutionMetrics(finalStatus, finished)
	s.finalizeExecutionLocked(ex, finalStatus, finalExitCode, ex.Message, "", finished)

	if runLogger != nil {
		runLogger.Info("execution completed",
			observability.LogFieldSandboxID, ex.SandboxID,
			observability.LogFieldExecutionID, ex.ID,
			observability.LogFieldBackend, sb.Backend,
			"image_ref", ex.ImageRef,
			"image_digest", ex.ImageDigest,
			"exit_code", ex.ExitCode,
			"status", ex.Status.String(),
		)
	}
}

func (s *Service) runAdapterExecution(runCtx context.Context, adapter backend.Adapter, executionReq backend.ExecutionRequest, key string, stdoutCapture, stderrCapture *retainedOutputCapture) (*backend.ExecutionResult, error) {
	return adapter.RunInSandbox(runCtx, executionReq, s.executionOutputStream(key, stdoutCapture, stderrCapture))
}

func (s *Service) applyExecutionResultMetadataLocked(ex *executionState, result *backend.ExecutionResult) {
	if ex == nil || result == nil {
		return
	}
	ex.LaunchedVM = result.LaunchedVM
	ex.PlanPath = result.PlanPath
	ex.RunDir = result.RunDir
	if strings.TrimSpace(result.ImageRef) != "" {
		ex.ImageRef = result.ImageRef
	}
	if strings.TrimSpace(result.ImageDigest) != "" {
		ex.ImageDigest = result.ImageDigest
	}
}

func (s *Service) executionOutputStream(key string, stdoutCapture, stderrCapture *retainedOutputCapture) backend.OutputStream {
	return backend.OutputStream{
		OnStdout: func(chunk []byte) {
			if stdoutCapture != nil {
				_, _ = stdoutCapture.Write(chunk)
			}
			s.recordExecutionOutputChunk(key, true, chunk)
		},
		OnStderr: func(chunk []byte) {
			if stderrCapture != nil {
				_, _ = stderrCapture.Write(chunk)
			}
			s.recordExecutionOutputChunk(key, false, chunk)
		},
		OnWarning: func(message string) {
			s.recordExecutionWarning(key, message)
		},
		OnAttach: func(io backend.AttachIO) {
			s.setExecutionAttachIO(key, io)
		},
	}
}

func (s *Service) executionAuxOutputStream(key string) backend.OutputStream {
	return backend.OutputStream{
		OnStdout: func(chunk []byte) {
			s.recordExecutionOutputChunk(key, true, chunk)
		},
		OnStderr: func(chunk []byte) {
			s.recordExecutionOutputChunk(key, false, chunk)
		},
		OnWarning: func(message string) {
			s.recordExecutionWarning(key, message)
		},
	}
}

func (s *Service) ensureRepositoryCommitAvailable(ctx context.Context, repository *repositorycheckout.Checkout) error {
	if repository == nil || s.RepositoryStore == nil {
		return nil
	}
	return s.RepositoryStore.EnsureCommit(ctx, repository.RemoteURL, repository.CommitSHA, repositorystore.FetchHints{})
}

func (s *Service) preparePersistentSandboxRepository(
	ctx context.Context,
	sandboxID string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	adapter backend.Adapter,
	repository *repositorycheckout.Checkout,
) error {
	if repository == nil {
		return nil
	}

	refreshExisting := false

	s.mu.Lock()
	sandbox, ok := s.sandboxes[sandboxID]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("unknown sandbox %q", sandboxID)
	}
	if sandbox.Status != cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY {
		s.mu.Unlock()
		return fmt.Errorf("sandbox %q is not ready", sandboxID)
	}
	if sandbox.DownloadInProgress {
		s.mu.Unlock()
		return fmt.Errorf("sandbox_busy: sandbox %q currently has an active file download", sandboxID)
	}
	if sandbox.RepositoryBusy {
		s.mu.Unlock()
		return fmt.Errorf("sandbox_busy: sandbox %q is preparing repository state", sandboxID)
	}
	if activeID := strings.TrimSpace(sandbox.ActiveExecutionID); activeID != "" {
		if activeExecution, ok := s.executions[executionKey(sandboxID, activeID)]; ok && !isFinalExecutionStatus(activeExecution.Status) {
			s.mu.Unlock()
			return fmt.Errorf("sandbox_busy: sandbox %q already has active execution %q", sandboxID, activeID)
		}
	}
	switch {
	case sandbox.Repository == nil:
		sandbox.RepositoryBusy = true
	case repositoryCheckoutsEqual(sandbox.Repository, repository) && (!sandbox.RepositoryHasChangeset || sandbox.RepositoryChangesetPendingExecution):
		s.mu.Unlock()
		return nil
	case repositoryCheckoutsEqual(sandbox.Repository, repository):
		sandbox.RepositoryBusy = true
		refreshExisting = true
	default:
		s.mu.Unlock()
		return fmt.Errorf("sandbox %q already has a different repository checkout", sandboxID)
	}
	s.mu.Unlock()

	err := s.bootstrapRepositoryInPersistentSandbox(ctx, adapter, sandboxID, compiled, firecrackerCfg, repository, refreshExisting, nil)

	s.mu.Lock()
	if sandbox, ok := s.sandboxes[sandboxID]; ok {
		sandbox.RepositoryBusy = false
		if err == nil {
			sandbox.Repository = cloneRepositoryCheckout(repository)
			sandbox.RepositoryHasChangeset = false
			sandbox.RepositoryChangesetPendingExecution = false
		}
	}
	s.mu.Unlock()

	if err != nil {
		return fmt.Errorf("prepare repository checkout: %w", err)
	}
	return nil
}

func (s *Service) bootstrapRepositoryInPersistentSandbox(
	ctx context.Context,
	adapter backend.Adapter,
	sandboxID string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	repository *repositorycheckout.Checkout,
	refreshExisting bool,
	reporter CreateSandboxReporter,
) error {
	if repository == nil {
		return nil
	}
	if s.Logger != nil {
		s.Logger.Debug("starting repository bootstrap",
			"sandbox_id", sandboxID,
			"remote_url", repository.RemoteURL,
			"commit_sha", repository.CommitSHA,
			"destination_dir", repository.DestinationDir,
		)
	}
	if err := s.ensureRepositoryCommitAvailable(ctx, repository); err != nil {
		return err
	}
	if s.Logger != nil {
		s.Logger.Debug("repository mirror ready",
			"sandbox_id", sandboxID,
			"remote_url", repository.RemoteURL,
			"commit_sha", repository.CommitSHA,
		)
	}

	command := repositorycheckout.BuildBootstrapCommand(repository)
	if refreshExisting {
		command = repositorycheckout.BuildRefreshCommand(repository)
	}

	bootstrapExecutionID, result, stdout, stderr, err := s.runPersistentBootstrapCommand(
		ctx,
		adapter,
		sandboxID,
		compiled,
		firecrackerCfg,
		cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_BOOTSTRAP_REPOSITORY,
		command,
		nil,
		reporter,
	)
	if s.Logger != nil {
		s.Logger.Debug("repository bootstrap execution finished",
			"sandbox_id", sandboxID,
			"execution_id", bootstrapExecutionID,
			"exit_code", func() int {
				if result == nil {
					return -1
				}
				return result.ExitCode
			}(),
			"error", err,
		)
	}
	return persistentBootstrapCommandError(result, stdout, stderr, err, "bootstrap execution returned no result", "bootstrap command failed with exit code %d")
}

func (s *Service) ensureMapsLocked() {
	if s.sandboxes == nil {
		s.sandboxes = map[string]*sandboxState{}
	}
	if s.executions == nil {
		s.executions = map[string]*executionState{}
	}
	if s.snapshotOps == nil {
		s.snapshotOps = map[string]int{}
	}
	if s.snapshotDeletions == nil {
		s.snapshotDeletions = map[string]struct{}{}
	}
}

// Snapshot operation bookkeeping is the remaining piece of snapshot coordination
// still owned by Service. If snapshot behavior expands beyond basic create/load/
// delete flows, move this state and the snapshot RPC handlers into a collaborator.
func (s *Service) beginSnapshotUseLocked(snapshotID string) error {
	if _, deleting := s.snapshotDeletions[snapshotID]; deleting {
		return fmt.Errorf("snapshot_busy: snapshot %q is being deleted", snapshotID)
	}
	s.snapshotOps[snapshotID]++
	return nil
}

func (s *Service) beginSnapshotUse(snapshotID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMapsLocked()
	return s.beginSnapshotUseLocked(strings.TrimSpace(snapshotID))
}

func (s *Service) finishSnapshotUse(snapshotID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if count := s.snapshotOps[snapshotID]; count > 1 {
		s.snapshotOps[snapshotID] = count - 1
		return
	}
	delete(s.snapshotOps, snapshotID)
}

func (s *Service) beginSnapshotDeleteLocked(snapshotID string) error {
	if _, deleting := s.snapshotDeletions[snapshotID]; deleting {
		return fmt.Errorf("snapshot_busy: snapshot %q is already being deleted", snapshotID)
	}
	if count := s.snapshotOps[snapshotID]; count > 0 {
		return fmt.Errorf("snapshot_busy: snapshot %q is currently in use by another operation", snapshotID)
	}
	s.snapshotDeletions[snapshotID] = struct{}{}
	return nil
}

func (s *Service) beginSnapshotDelete(snapshotID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMapsLocked()
	return s.beginSnapshotDeleteLocked(strings.TrimSpace(snapshotID))
}

func (s *Service) finishSnapshotDelete(snapshotID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.snapshotDeletions, snapshotID)
}

func (s *Service) mergeBufferedResultOutputLocked(ex *executionState, result *backend.ExecutionResult, usedStreaming bool) {
	if ex == nil || result == nil {
		return
	}

	appendStdout := result.Stdout
	appendStderr := result.Stderr
	replaceStdout := false
	replaceStderr := false
	retention := s.retention()
	if usedStreaming {
		appendStdout, replaceStdout = bufferedResultDelta(ex.Stdout, result.Stdout, retention.maxRetainedExecutionOutputBytes)
		appendStderr, replaceStderr = bufferedResultDelta(ex.Stderr, result.Stderr, retention.maxRetainedExecutionOutputBytes)
	}

	if replaceStdout {
		s.replaceExecutionStdoutFromBufferedLocked(ex, cleanroomv1.ExecutionStatus_EXECUTION_STATUS_RUNNING, appendStdout)
	} else {
		s.appendExecutionStdoutLocked(ex, cleanroomv1.ExecutionStatus_EXECUTION_STATUS_RUNNING, []byte(appendStdout))
	}
	if replaceStderr {
		s.replaceExecutionStderrFromBufferedLocked(ex, cleanroomv1.ExecutionStatus_EXECUTION_STATUS_RUNNING, appendStderr)
	} else {
		s.appendExecutionStderrLocked(ex, cleanroomv1.ExecutionStatus_EXECUTION_STATUS_RUNNING, []byte(appendStderr))
	}
}

func (s *Service) mergeBufferedResultOutputFromStreamLocked(ex *executionState, result *backend.ExecutionResult, prefixStdout, prefixStderr, streamedStdout, streamedStderr string) {
	if ex == nil || result == nil {
		return
	}

	appendStdout, replaceStdout := bufferedResultDelta(streamedStdout, result.Stdout, s.retention().maxRetainedExecutionOutputBytes)
	appendStderr, replaceStderr := bufferedResultDelta(streamedStderr, result.Stderr, s.retention().maxRetainedExecutionOutputBytes)

	if replaceStdout {
		s.replaceExecutionStdoutFromBufferedWithPrefixLocked(ex, cleanroomv1.ExecutionStatus_EXECUTION_STATUS_RUNNING, prefixStdout, appendStdout)
	} else {
		s.appendExecutionStdoutLocked(ex, cleanroomv1.ExecutionStatus_EXECUTION_STATUS_RUNNING, []byte(appendStdout))
	}
	if replaceStderr {
		s.replaceExecutionStderrFromBufferedWithPrefixLocked(ex, cleanroomv1.ExecutionStatus_EXECUTION_STATUS_RUNNING, prefixStderr, appendStderr)
	} else {
		s.appendExecutionStderrLocked(ex, cleanroomv1.ExecutionStatus_EXECUTION_STATUS_RUNNING, []byte(appendStderr))
	}
}

func (s *Service) recordExecutionOutputChunk(key string, isStdout bool, chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	ex, ok := s.executions[key]
	if !ok {
		return
	}

	status := ex.Status
	if isFinalExecutionStatus(status) {
		return
	}

	if isStdout {
		s.appendExecutionStdoutLocked(ex, status, chunk)
		return
	}

	s.appendExecutionStderrLocked(ex, status, chunk)
}

func (s *Service) recordExecutionWarning(key, message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	ex, ok := s.executions[key]
	if !ok {
		return
	}

	status := ex.Status
	if isFinalExecutionStatus(status) {
		return
	}

	s.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
		SandboxId:   ex.SandboxID,
		ExecutionId: ex.ID,
		Status:      status,
		Payload:     &cleanroomv1.ExecutionStreamEvent_Warning{Warning: message},
		OccurredAt:  timestamppb.Now(),
	})
}

func (s *Service) appendExecutionStdoutLocked(ex *executionState, status cleanroomv1.ExecutionStatus, chunk []byte) {
	if ex == nil || len(chunk) == 0 {
		return
	}
	ex.Stdout = appendRetainedOutput(ex.Stdout, string(chunk), s.retention().maxRetainedExecutionOutputBytes)
	s.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
		SandboxId:   ex.SandboxID,
		ExecutionId: ex.ID,
		Status:      status,
		Payload:     &cleanroomv1.ExecutionStreamEvent_Stdout{Stdout: append([]byte(nil), chunk...)},
		OccurredAt:  timestamppb.New(s.clock().Now()),
	})
}

func (s *Service) appendExecutionStderrLocked(ex *executionState, status cleanroomv1.ExecutionStatus, chunk []byte) {
	if ex == nil || len(chunk) == 0 {
		return
	}
	ex.Stderr = appendRetainedOutput(ex.Stderr, string(chunk), s.retention().maxRetainedExecutionOutputBytes)
	s.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
		SandboxId:   ex.SandboxID,
		ExecutionId: ex.ID,
		Status:      status,
		Payload:     &cleanroomv1.ExecutionStreamEvent_Stderr{Stderr: append([]byte(nil), chunk...)},
		OccurredAt:  timestamppb.New(s.clock().Now()),
	})
}

func (s *Service) replaceExecutionStdoutFromBufferedLocked(ex *executionState, status cleanroomv1.ExecutionStatus, output string) {
	if ex == nil || output == "" {
		return
	}
	ex.Stdout = appendRetainedOutput("", output, s.retention().maxRetainedExecutionOutputBytes)
	s.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
		SandboxId:   ex.SandboxID,
		ExecutionId: ex.ID,
		Status:      status,
		Payload:     &cleanroomv1.ExecutionStreamEvent_Stdout{Stdout: []byte(output)},
		OccurredAt:  timestamppb.New(s.clock().Now()),
	})
}

func (s *Service) replaceExecutionStdoutFromBufferedWithPrefixLocked(ex *executionState, status cleanroomv1.ExecutionStatus, prefix, output string) {
	if ex == nil || output == "" {
		return
	}
	ex.Stdout = appendRetainedOutput(prefix, output, s.retention().maxRetainedExecutionOutputBytes)
	s.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
		SandboxId:   ex.SandboxID,
		ExecutionId: ex.ID,
		Status:      status,
		Payload:     &cleanroomv1.ExecutionStreamEvent_Stdout{Stdout: []byte(output)},
		OccurredAt:  timestamppb.New(s.clock().Now()),
	})
}

func (s *Service) replaceExecutionStderrFromBufferedLocked(ex *executionState, status cleanroomv1.ExecutionStatus, output string) {
	if ex == nil || output == "" {
		return
	}
	ex.Stderr = appendRetainedOutput("", output, s.retention().maxRetainedExecutionOutputBytes)
	s.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
		SandboxId:   ex.SandboxID,
		ExecutionId: ex.ID,
		Status:      status,
		Payload:     &cleanroomv1.ExecutionStreamEvent_Stderr{Stderr: []byte(output)},
		OccurredAt:  timestamppb.New(s.clock().Now()),
	})
}

func (s *Service) replaceExecutionStderrFromBufferedWithPrefixLocked(ex *executionState, status cleanroomv1.ExecutionStatus, prefix, output string) {
	if ex == nil || output == "" {
		return
	}
	ex.Stderr = appendRetainedOutput(prefix, output, s.retention().maxRetainedExecutionOutputBytes)
	s.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
		SandboxId:   ex.SandboxID,
		ExecutionId: ex.ID,
		Status:      status,
		Payload:     &cleanroomv1.ExecutionStreamEvent_Stderr{Stderr: []byte(output)},
		OccurredAt:  timestamppb.New(s.clock().Now()),
	})
}

func (s *Service) executionDoneChannel(sandboxID, executionID string) (<-chan struct{}, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ex, ok := s.executions[executionKey(sandboxID, executionID)]
	if !ok {
		return nil, fmt.Errorf("unknown execution %q in sandbox %q", executionID, sandboxID)
	}
	return ex.Done, nil
}

func (s *Service) setExecutionAttachIO(key string, io backend.AttachIO) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ex, ok := s.executions[key]
	if !ok || ex == nil || isFinalExecutionStatus(ex.Status) {
		return
	}
	ex.AttachStdin = io.WriteStdin
	ex.AttachCloseStdin = io.CloseStdin
	ex.AttachResize = io.ResizeTTY
}

func (s *Service) dropSandboxLocked(sandboxID string, sb *sandboxState) {
	if sb == nil {
		return
	}
	closeSandboxSubscribersLocked(sb)
	closeSandboxDoneLocked(sb)
	delete(s.sandboxes, sandboxID)
}

func (s *Service) dropExecutionLocked(key string, ex *executionState) {
	if ex == nil {
		return
	}
	closeExecutionSubscribersLocked(ex)
	closeExecutionDoneLocked(ex)
	s.interactive.clearExecution(key)
	delete(s.executions, key)
	s.refreshSandboxExecutionPointersLocked(ex.SandboxID)
}

func (s *Service) hasActiveExecutionLocked(sandboxID string) bool {
	for _, ex := range s.executions {
		if ex.SandboxID == sandboxID && !isFinalExecutionStatus(ex.Status) {
			return true
		}
	}
	return false
}

func (s *Service) refreshSandboxExecutionPointersLocked(sandboxID string) {
	sb, ok := s.sandboxes[sandboxID]
	if !ok || sb == nil {
		return
	}

	var latestActive *executionState
	var latestFinished *executionState
	for _, ex := range s.executions {
		if ex == nil || ex.SandboxID != sandboxID {
			continue
		}
		if !isFinalExecutionStatus(ex.Status) {
			if latestActive == nil || ex.ID > latestActive.ID {
				latestActive = ex
			}
			continue
		}
		if latestFinished == nil || newerExecutionState(ex, latestFinished) {
			latestFinished = ex
		}
	}

	if latestActive != nil {
		sb.ActiveExecutionID = latestActive.ID
		sb.LastExecutionID = latestActive.ID
		return
	}

	sb.ActiveExecutionID = ""
	if latestFinished != nil {
		sb.LastExecutionID = latestFinished.ID
		return
	}
	sb.LastExecutionID = ""
}

func newerExecutionState(left, right *executionState) bool {
	if left == nil {
		return false
	}
	if right == nil {
		return true
	}
	leftTime := executionTerminalTime(left)
	rightTime := executionTerminalTime(right)
	switch {
	case leftTime.After(rightTime):
		return true
	case rightTime.After(leftTime):
		return false
	default:
		return left.ID > right.ID
	}
}

func (s *Service) pruneStateLocked(now time.Time) {
	if now.IsZero() {
		now = s.clock().Now()
	}
	s.pruneExecutionsLocked(now)
	s.pruneSandboxesLocked(now)
}

func (s *Service) pruneExecutionsLocked(now time.Time) {
	type candidate struct {
		key      string
		finished time.Time
	}

	retention := s.retention()
	candidates := make([]candidate, 0, len(s.executions))
	for key, ex := range s.executions {
		if ex == nil || !isFinalExecutionStatus(ex.Status) {
			continue
		}

		finished := executionTerminalTime(ex)
		if retention.retainedStateMaxAge > 0 && !finished.IsZero() && now.Sub(finished) > retention.retainedStateMaxAge {
			s.dropExecutionLocked(key, ex)
			continue
		}

		candidates = append(candidates, candidate{key: key, finished: finished})
	}

	limit := retention.maxRetainedFinishedExecutions
	if limit < 0 {
		limit = 0
	}
	if len(candidates) <= limit {
		return
	}

	sort.Slice(candidates, func(i, j int) bool {
		left := candidates[i]
		right := candidates[j]
		if left.finished.Equal(right.finished) {
			return left.key < right.key
		}
		if left.finished.IsZero() {
			return true
		}
		if right.finished.IsZero() {
			return false
		}
		return left.finished.Before(right.finished)
	})

	removeCount := len(candidates) - limit
	for i := 0; i < removeCount; i++ {
		key := candidates[i].key
		ex, ok := s.executions[key]
		if !ok || ex == nil || !isFinalExecutionStatus(ex.Status) {
			continue
		}
		s.dropExecutionLocked(key, ex)
	}
}

func (s *Service) pruneSandboxesLocked(now time.Time) {
	type candidate struct {
		id      string
		stopped time.Time
	}

	retention := s.retention()
	candidates := make([]candidate, 0, len(s.sandboxes))
	for sandboxID, sb := range s.sandboxes {
		if sb == nil || sb.Status != cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED {
			continue
		}
		if s.hasActiveExecutionLocked(sandboxID) {
			continue
		}

		stopped := sandboxTerminalTime(sb)
		if retention.retainedStateMaxAge > 0 && !stopped.IsZero() && now.Sub(stopped) > retention.retainedStateMaxAge {
			s.dropSandboxLocked(sandboxID, sb)
			continue
		}

		candidates = append(candidates, candidate{id: sandboxID, stopped: stopped})
	}

	limit := retention.maxRetainedStoppedSandboxes
	if limit < 0 {
		limit = 0
	}
	if len(candidates) <= limit {
		return
	}

	sort.Slice(candidates, func(i, j int) bool {
		left := candidates[i]
		right := candidates[j]
		if left.stopped.Equal(right.stopped) {
			return left.id < right.id
		}
		if left.stopped.IsZero() {
			return true
		}
		if right.stopped.IsZero() {
			return false
		}
		return left.stopped.Before(right.stopped)
	})

	removeCount := len(candidates) - limit
	for i := 0; i < removeCount; i++ {
		sandboxID := candidates[i].id
		sb, ok := s.sandboxes[sandboxID]
		if !ok || sb == nil || sb.Status != cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED {
			continue
		}
		if s.hasActiveExecutionLocked(sandboxID) {
			continue
		}
		s.dropSandboxLocked(sandboxID, sb)
	}
}

func ensureSandboxIdleLocked(sandboxID string, sb *sandboxState, executions map[string]*executionState) error {
	if sb == nil {
		return fmt.Errorf("unknown sandbox %q", sandboxID)
	}
	if sb.Status != cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY {
		return fmt.Errorf("sandbox %q is not ready", sandboxID)
	}
	if sb.DownloadInProgress {
		return fmt.Errorf("sandbox_busy: sandbox %q currently has an active file download", sandboxID)
	}
	if sb.RepositoryBusy {
		return fmt.Errorf("sandbox_busy: sandbox %q is preparing repository state", sandboxID)
	}
	if activeID := strings.TrimSpace(sb.ActiveExecutionID); activeID != "" {
		if activeExecution, ok := executions[executionKey(sandboxID, activeID)]; ok && !isFinalExecutionStatus(activeExecution.Status) {
			return fmt.Errorf("sandbox_busy: sandbox %q already has active execution %q", sandboxID, activeID)
		}
	}
	return nil
}

func (s *Service) snapshotStoreOrErr() (snapshotMetadataStore, error) {
	if s.SnapshotStore == nil {
		return nil, errors.New("snapshot metadata store is not configured")
	}
	return s.SnapshotStore, nil
}

func (s *Service) cacheStoreOrErr() (cacheMetadataStore, error) {
	if s.CacheStore == nil {
		return nil, errors.New("cache metadata store is not configured")
	}
	return s.CacheStore, nil
}

func cloneSnapshotRecord(record snapshotstore.Record) *cleanroomv1.Snapshot {
	return &cleanroomv1.Snapshot{
		SnapshotId:      record.SnapshotID,
		SourceSandboxId: record.SourceSandboxID,
		Backend:         record.Backend,
		PolicyHash:      record.PolicyHash,
		Name:            record.Name,
		CreatedAt:       timestamppb.New(record.CreatedAt),
		StorageDriver:   record.StorageDriver,
		StorageRef:      record.StorageRef,
		RepositoryCheckout: func() *cleanroomv1.RepositoryCheckout {
			if record.Repository == nil {
				return nil
			}
			return proto.Clone(record.Repository).(*cleanroomv1.RepositoryCheckout)
		}(),
	}
}

func snapshotOperationsEnabledForBackend(backendName string, cfg runtimeconfig.Config) bool {
	snapshotCfg, ok := runtimeconfig.SnapshotConfigForBackend(cfg, backendName)
	return ok && snapshotCfg.Enabled
}

func withSnapshotDriver(backendName string, cfg backend.FirecrackerConfig, driver string) backend.FirecrackerConfig {
	cfg.Snapshots.Enabled = true
	cfg.Snapshots.Driver = runtimeconfig.SnapshotDriverOrDefault(backendName, driver)
	return cfg
}
func (s *Service) recordSandboxEventLocked(sb *sandboxState, status cleanroomv1.SandboxStatus, message string) {
	now := s.clock().Now()
	sb.Status = status
	sb.UpdatedAt = now
	event := &cleanroomv1.SandboxEvent{
		SandboxId:  sb.ID,
		Status:     status,
		Message:    message,
		OccurredAt: timestamppb.New(now),
	}
	sb.events.publish(event)
}

func (s *Service) recordExecutionEventLocked(ex *executionState, event *cleanroomv1.ExecutionStreamEvent) {
	if event == nil {
		return
	}
	if strings.TrimSpace(event.GetImageRef()) == "" {
		event.ImageRef = ex.ImageRef
	}
	if strings.TrimSpace(event.GetImageDigest()) == "" {
		event.ImageDigest = ex.ImageDigest
	}
	if event.GetOccurredAt() == nil {
		event.OccurredAt = timestamppb.New(s.clock().Now())
	}
	ex.events.publish(event)
}

func (s *Service) finalizeExecutionLocked(ex *executionState, status cleanroomv1.ExecutionStatus, exitCode int32, message, exitMessage string, finished time.Time) {
	s.finalizeExecutionInternalLocked(ex, status, exitCode, message, exitMessage, finished, true)
}

func (s *Service) finalizeExecutionWithoutPruneLocked(ex *executionState, status cleanroomv1.ExecutionStatus, exitCode int32, message, exitMessage string, finished time.Time) {
	s.finalizeExecutionInternalLocked(ex, status, exitCode, message, exitMessage, finished, false)
}

func (s *Service) finalizeExecutionInternalLocked(ex *executionState, status cleanroomv1.ExecutionStatus, exitCode int32, message, exitMessage string, finished time.Time, prune bool) {
	if ex == nil {
		return
	}
	if finished.IsZero() {
		finished = s.clock().Now()
	}
	if exitMessage == "" {
		exitMessage = message
	}
	ex.Status = status
	ex.ExitCode = exitCode
	ex.Message = message
	ex.FinishedAt = &finished
	s.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
		SandboxId:   ex.SandboxID,
		ExecutionId: ex.ID,
		Status:      ex.Status,
		Payload: &cleanroomv1.ExecutionStreamEvent_Exit{Exit: &cleanroomv1.ExecutionExit{
			ExitCode: ex.ExitCode,
			Status:   ex.Status,
			Message:  exitMessage,
		}},
		OccurredAt: timestamppb.New(finished),
	})
	closeExecutionDoneLocked(ex)
	s.interactive.clearExecution(executionKey(ex.SandboxID, ex.ID))
	if prune {
		s.pruneStateLocked(finished)
	}
}
