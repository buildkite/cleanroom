package controlservice

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/paths"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"github.com/buildkite/cleanroom/internal/snapshotstore"
	"github.com/charmbracelet/log"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Service struct {
	Loader            loader
	Config            runtimeconfig.Config
	Backends          map[string]backend.Adapter
	Logger            *log.Logger
	RepositoryMirrors repositoryMirrorStore
	runtime           serviceRuntime
	interactive       interactiveSessionBroker
	SnapshotStore snapshotMetadataStore

	mu                sync.RWMutex
	sandboxes         map[string]*sandboxState
	executions        map[string]*executionState
	snapshotOps       map[string]int
	snapshotDeletions map[string]struct{}
}

type sandboxState struct {
	ID                 string
	Backend            string
	Policy             *policy.CompiledPolicy
	Firecracker        backend.FirecrackerConfig
	Repository         *repositorycheckout.Checkout
	RepositoryBusy     bool
	ActiveExecutionID  string
	DownloadInProgress bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
	LastExecutionID    string
	Status             cleanroomv1.SandboxStatus
	events             eventFeed[*cleanroomv1.SandboxEvent]
	Done               chan struct{}
	DoneClosed         bool
}

type executionState struct {
	ID              string
	SandboxID       string
	RunID           string
	ImageRef        string
	ImageDigest     string
	Command         []string
	Options         executionOptions
	TTY             bool
	Kind            cleanroomv1.ExecutionKind
	Status          cleanroomv1.ExecutionStatus
	ExitCode        int32
	StartedAt       *time.Time
	FinishedAt      *time.Time
	Message         string
	Stdout          string
	Stderr          string
	LaunchedVM      bool
	PlanPath        string
	RunDir          string
	CancelRequested bool
	CancelSignal    int32
	Cancel          context.CancelFunc
	AttachStdin     func([]byte) error
	AttachResize    func(cols, rows uint32) error
	events          eventFeed[*cleanroomv1.ExecutionStreamEvent]
	Done            chan struct{}
	DoneClosed      bool
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

type repositoryMirrorStore interface {
	EnsureMirrorContains(ctx context.Context, remoteURL, commitSHA string) error
}

type snapshotMetadataStore interface {
	Create(context.Context, snapshotstore.Record) error
	Get(context.Context, string) (snapshotstore.Record, bool, error)
	List(context.Context) ([]snapshotstore.Record, error)
	Delete(context.Context, string) error
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
}

var (
	ErrExecutionStdinUnsupported  = errors.New("execution stdin attach is not supported by the current backend")
	ErrExecutionResizeUnsupported = errors.New("execution resize is not supported by the current backend")
)

func (s *Service) CreateSandbox(ctx context.Context, req *cleanroomv1.CreateSandboxRequest) (*cleanroomv1.CreateSandboxResponse, error) {
	if req == nil {
		return nil, errors.New("missing request")
	}
	snapshotID := strings.TrimSpace(req.GetSnapshotId())
	if snapshotID != "" {
		if req.GetPolicy() != nil {
			return nil, errors.New("snapshot-backed sandbox creation cannot include policy")
		}
		if req.GetRepositoryCheckout() != nil {
			return nil, errors.New("snapshot-backed sandbox creation cannot include repository checkout")
		}
		return s.createSandboxFromSnapshot(ctx, req, snapshotID)
	}
	if req.GetPolicy() == nil {
		return nil, errors.New("missing policy")
	}

	compiled, err := policy.FromProto(req.GetPolicy())
	if err != nil {
		return nil, fmt.Errorf("invalid policy: %w", err)
	}

	backendName := resolveBackendName(strings.TrimSpace(req.GetBackend()), s.Config.DefaultBackend)
	adapter, ok := s.Backends[backendName]
	if !ok {
		return nil, fmt.Errorf("unknown backend %q", backendName)
	}
	repository := repositorycheckout.FromProto(req.GetRepositoryCheckout())
	if repository != nil {
		if err := validateRepositoryCheckoutForPolicy(compiled, repository); err != nil {
			return nil, err
		}
	}

	opts := req.GetOptions()
	execOpts := executionOptions{}
	if opts != nil {
		execOpts.LaunchSeconds = opts.GetLaunchSeconds()
	}
	firecrackerCfg := runtimeconfig.MergeBackendConfig(s.Config, backendName, execOpts.LaunchSeconds)
	firecrackerCfg.RunDir = ""

	now := s.clock().Now()
	sandboxID := s.ids().NewSandboxID()

	if persistentAdapter, ok := adapter.(backend.PersistentSandboxAdapter); ok {
		if err := persistentAdapter.ProvisionSandbox(ctx, backend.ProvisionRequest{
			SandboxID:         sandboxID,
			Policy:            compiled,
			FirecrackerConfig: firecrackerCfg,
		}); err != nil {
			return nil, fmt.Errorf("provision sandbox: %w", err)
		}
		if err := s.bootstrapRepositoryInPersistentSandbox(ctx, persistentAdapter, sandboxID, compiled, firecrackerCfg, repository); err != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), s.timeouts().bootstrapCleanupTimeout)
			defer cancel()
			if terminateErr := persistentAdapter.TerminateSandbox(cleanupCtx, sandboxID); terminateErr != nil {
				return nil, fmt.Errorf("bootstrap repository checkout: %w; cleanup failed: %v", err, terminateErr)
			}
			return nil, fmt.Errorf("bootstrap repository checkout: %w", err)
		}
	} else if repository != nil {
		return nil, errors.New("repository bootstrap for sandbox creation requires a persistent backend")
	}

	state := &sandboxState{
		ID:          sandboxID,
		Backend:     backendName,
		Policy:      compiled,
		Firecracker: firecrackerCfg,
		Repository:  cloneRepositoryCheckout(repository),
		CreatedAt:   now,
		UpdatedAt:   now,
		Status:      cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY,
		events:      newEventFeed[*cleanroomv1.SandboxEvent](s.retention().maxRetainedSandboxEvents),
		Done:        make(chan struct{}),
	}

	s.mu.Lock()
	s.ensureMapsLocked()
	s.sandboxes[sandboxID] = state
	s.recordSandboxEventLocked(state, cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY, "sandbox created and ready")
	s.pruneStateLocked(now)
	resp := &cleanroomv1.CreateSandboxResponse{
		Sandbox: cloneSandboxLocked(state),
		Message: "sandbox created and ready",
	}
	s.mu.Unlock()

	if s.Logger != nil {
		s.Logger.Info("sandbox created",
			"sandbox_id", sandboxID,
			"backend", backendName,
			"policy_hash", compiled.Hash,
		)
	}

	return resp, nil
}

func (s *Service) createSandboxFromSnapshot(ctx context.Context, req *cleanroomv1.CreateSandboxRequest, snapshotID string) (*cleanroomv1.CreateSandboxResponse, error) {
	store, err := s.snapshotStoreOrErr()
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.ensureMapsLocked()
	if err := s.beginSnapshotUseLocked(snapshotID); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()
	defer s.finishSnapshotUse(snapshotID)

	record, ok, err := store.Get(ctx, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("load snapshot %q: %w", snapshotID, err)
	}
	if !ok {
		return nil, fmt.Errorf("unknown snapshot %q", snapshotID)
	}

	backendName := record.Backend
	if requested := strings.TrimSpace(req.GetBackend()); requested != "" && requested != backendName {
		return nil, fmt.Errorf("snapshot %q requires backend %q, got %q", snapshotID, backendName, requested)
	}

	adapter, ok := s.Backends[backendName]
	if !ok {
		return nil, fmt.Errorf("unknown backend %q", backendName)
	}
	snapshotAdapter, ok := adapter.(backend.SnapshottingAdapter)
	if !ok {
		return nil, fmt.Errorf("backend %q does not support snapshot-backed sandbox creation", backendName)
	}
	if !snapshotOperationsEnabledForBackend(backendName, s.Config) {
		return nil, fmt.Errorf("snapshots are not enabled for backend %q", backendName)
	}

	compiled, err := policy.FromProto(record.Policy)
	if err != nil {
		return nil, fmt.Errorf("invalid snapshot policy %q: %w", snapshotID, err)
	}

	opts := req.GetOptions()
	execOpts := executionOptions{}
	if opts != nil {
		execOpts.LaunchSeconds = opts.GetLaunchSeconds()
	}
	firecrackerCfg := runtimeconfig.MergeBackendConfig(s.Config, backendName, execOpts.LaunchSeconds)
	firecrackerCfg.RunDir = ""
	firecrackerCfg = withSnapshotDriver(firecrackerCfg, record.StorageDriver)

	now := time.Now().UTC()
	sandboxID := newSandboxID()
	if err := snapshotAdapter.ProvisionSandboxFromSnapshot(ctx, backend.ProvisionFromSnapshotRequest{
		SandboxID:         sandboxID,
		SnapshotID:        record.SnapshotID,
		StorageRef:        record.StorageRef,
		Policy:            compiled,
		FirecrackerConfig: firecrackerCfg,
	}); err != nil {
		return nil, fmt.Errorf("provision sandbox from snapshot: %w", err)
	}

	state := &sandboxState{
		ID:               sandboxID,
		Backend:          backendName,
		Policy:           compiled,
		Firecracker:      firecrackerCfg,
		Repository:       repositorycheckout.FromProto(record.Repository),
		CreatedAt:        now,
		UpdatedAt:        now,
		Status:           cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY,
		EventSubscribers: map[int]chan *cleanroomv1.SandboxEvent{},
		Done:             make(chan struct{}),
	}

	s.mu.Lock()
	s.ensureMapsLocked()
	s.sandboxes[sandboxID] = state
	s.recordSandboxEventLocked(state, cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY, fmt.Sprintf("sandbox created from snapshot %s and ready", snapshotID))
	s.pruneStateLocked(now)
	resp := &cleanroomv1.CreateSandboxResponse{
		Sandbox: cloneSandboxLocked(state),
		Message: "sandbox created from snapshot and ready",
	}
	s.mu.Unlock()

	if s.Logger != nil {
		s.Logger.Info("sandbox created from snapshot",
			"sandbox_id", sandboxID,
			"snapshot_id", snapshotID,
			"backend", backendName,
			"policy_hash", compiled.Hash,
		)
	}

	return resp, nil
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
		return items[i].CreatedAt.Before(items[j].CreatedAt)
	})

	resp := &cleanroomv1.ListSandboxesResponse{Sandboxes: make([]*cleanroomv1.Sandbox, 0, len(items))}
	for _, sb := range items {
		resp.Sandboxes = append(resp.Sandboxes, cloneSandboxLocked(sb))
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
	snapshotCfg := withSnapshotDriver(state.Firecracker, state.Firecracker.Snapshots.Driver)
	record = snapshotstore.Record{
		SnapshotID:      snapshotID,
		SourceSandboxID: sandboxID,
		Backend:         state.Backend,
		Name:            name,
		PolicyHash:      state.Policy.Hash,
		Policy:          state.Policy.ToProto(),
		Repository:      cloneRepositoryCheckout(state.Repository).ToProto(),
		StorageDriver:   snapshotCfg.Snapshots.Driver,
		CreatedAt:       now,
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

	s.mu.Lock()
	s.ensureMapsLocked()
	if err := s.beginSnapshotDeleteLocked(snapshotID); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()
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
	firecrackerCfg := withSnapshotDriver(runtimeconfig.MergeBackendConfig(s.Config, record.Backend, 0), record.StorageDriver)
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
	var persistentAdapter backend.PersistentSandboxAdapter
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
		if adapter, ok := s.Backends[state.Backend]; ok {
			if persistent, ok := adapter.(backend.PersistentSandboxAdapter); ok {
				persistentAdapter = persistent
			}
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

	if !alreadyStopped && persistentAdapter != nil {
		if err := persistentAdapter.TerminateSandbox(ctx, sandboxID); err != nil {
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

func (s *Service) CreateExecution(ctx context.Context, req *cleanroomv1.CreateExecutionRequest) (*cleanroomv1.CreateExecutionResponse, error) {
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
	if sandbox.Status != cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY {
		s.mu.Unlock()
		return nil, fmt.Errorf("sandbox %q is not ready", sandboxID)
	}
	adapter, ok := s.Backends[sandbox.Backend]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("unknown backend %q", sandbox.Backend)
	}
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
		if persistentAdapter, ok := adapter.(backend.PersistentSandboxAdapter); ok {
			if err := s.preparePersistentSandboxRepository(ctx, sandboxID, sandboxPolicy, firecrackerCfg, persistentAdapter, repository); err != nil {
				return nil, err
			}
			command = repositorycheckout.WrapCommandInWorkdir(command, repository)
		} else {
			if err := s.ensureRepositoryMirrorContains(ctx, repository); err != nil {
				return nil, fmt.Errorf("prepare repository checkout: %w", err)
			}
			command = repositorycheckout.WrapCommandWithBootstrap(command, repository)
		}
	} else if sandboxRepository != nil {
		command = repositorycheckout.WrapCommandInWorkdir(command, sandboxRepository)
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
	ex := &executionState{
		ID:          executionID,
		SandboxID:   sandboxID,
		ImageRef:    imageRef,
		ImageDigest: imageDigest,
		Command:     append([]string(nil), command...),
		Options:     execOpts,
		TTY:         tty,
		Kind:        kind,
		Status:      cleanroomv1.ExecutionStatus_EXECUTION_STATUS_QUEUED,
		events:      newEventFeed[*cleanroomv1.ExecutionStreamEvent](s.retention().maxRetainedExecutionEvents),
		Done:        make(chan struct{}),
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

	resp := &cleanroomv1.CreateExecutionResponse{Execution: cloneExecutionLocked(ex)}
	s.mu.Unlock()

	go s.runExecution(sandboxID, executionID)

	if s.Logger != nil {
		s.Logger.Info("execution created",
			"sandbox_id", sandboxID,
			"execution_id", executionID,
			"command_argc", len(command),
			"tty", tty,
			"kind", kind.String(),
		)
	}
	return resp, nil
}

func (s *Service) OpenInteractiveExecution(_ context.Context, req *cleanroomv1.OpenInteractiveExecutionRequest) (*cleanroomv1.OpenInteractiveExecutionResponse, error) {
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

	return &cleanroomv1.OpenInteractiveExecutionResponse{
		SessionId:           grant.SessionID,
		SessionToken:        grant.SessionToken,
		ExpiresAt:           timestamppb.New(grant.ExpiresAt),
		QuicEndpoint:        grant.QuicEndpoint,
		Alpn:                grant.Alpn,
		ServerCertPinSha256: grant.ServerCertPinSHA256,
	}, nil
}

func (s *Service) ConfigureInteractiveTransport(endpoint, alpn, certPinSHA256 string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.interactive.configureTransport(endpoint, alpn, certPinSHA256)
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
	if sandboxID == "" {
		return nil, errors.New("missing sandbox_id")
	}
	if executionID == "" {
		return nil, errors.New("missing execution_id")
	}

	s.mu.RLock()
	ex, ok := s.executions[executionKey(sandboxID, executionID)]
	if !ok {
		s.mu.RUnlock()
		return nil, fmt.Errorf("unknown execution %q in sandbox %q", executionID, sandboxID)
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
	deadline := clock.Now().Add(s.timeouts().attachStdinRegistrationWait)
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
		writeFn = ex.AttachStdin
		done = ex.Done
		s.mu.RUnlock()

		if writeFn != nil {
			return writeFn(payload)
		}
		if clock.Now().After(deadline) {
			return ErrExecutionStdinUnsupported
		}
		select {
		case <-done:
		case <-clock.After(s.timeouts().attachPollInterval):
		}
	}
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
	deadline := clock.Now().Add(s.timeouts().attachResizeRegistrationWait)
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
		resizeFn = ex.AttachResize
		done = ex.Done
		s.mu.RUnlock()

		if resizeFn != nil {
			return resizeFn(cols, rows)
		}
		if clock.Now().After(deadline) {
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
	ex, ok := s.executions[executionKey(sandboxID, executionID)]
	if !ok {
		return nil, fmt.Errorf("unknown execution %q in sandbox %q", executionID, sandboxID)
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
	}, nil
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

	runCtx, cancel := context.WithCancel(context.Background())
	ex.Cancel = cancel

	started := s.clock().Now()
	ex.StartedAt = &started
	ex.Status = cleanroomv1.ExecutionStatus_EXECUTION_STATUS_RUNNING
	ex.RunID = s.ids().NewRunID()
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
		if runBaseDir, err := paths.RunBaseDir(); err == nil {
			firecrackerCfg.RunDir = filepath.Join(runBaseDir, ex.RunID)
		}
	}
	if ex.Options.LaunchSeconds != 0 {
		firecrackerCfg.LaunchSeconds = ex.Options.LaunchSeconds
	}

	runReq := backend.RunRequest{
		SandboxID:         sandboxID,
		RunID:             ex.RunID,
		Command:           append([]string(nil), ex.Command...),
		TTY:               ex.TTY,
		Policy:            sb.Policy,
		FirecrackerConfig: firecrackerCfg,
	}
	s.mu.Unlock()

	result, usedStreaming, err := s.runAdapterExecution(runCtx, adapter, runReq, key)

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

	if ex.Cancel != nil {
		ex.Cancel = nil
	}
	clearExecutionAttachIOLocked(ex)

	if err != nil {
		finalStatus, exitCode := executionRunErrorStatus(ex, runCtx)
		if strings.TrimSpace(err.Error()) != "" {
			s.appendExecutionStderrLocked(ex, finalStatus, []byte(err.Error()+"\n"))
		}
		finished := s.clock().Now()
		s.finalizeExecutionLocked(ex, finalStatus, exitCode, err.Error(), "", finished)
		if s.Logger != nil {
			s.Logger.Warn("execution failed",
				"sandbox_id", ex.SandboxID,
				"execution_id", ex.ID,
				"run_id", ex.RunID,
				"image_ref", ex.ImageRef,
				"image_digest", ex.ImageDigest,
				"status", ex.Status.String(),
				"error", err,
			)
		}
		return
	}

	ex.RunID = result.RunID
	ex.LaunchedVM = result.LaunchedVM
	ex.PlanPath = result.PlanPath
	ex.RunDir = result.RunDir
	if strings.TrimSpace(result.ImageRef) != "" {
		ex.ImageRef = result.ImageRef
	}
	if strings.TrimSpace(result.ImageDigest) != "" {
		ex.ImageDigest = result.ImageDigest
	}
	ex.Message = result.Message
	s.mergeBufferedResultOutputLocked(ex, result, usedStreaming)

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
	finished := s.clock().Now()
	s.finalizeExecutionLocked(ex, finalStatus, finalExitCode, ex.Message, "", finished)

	if s.Logger != nil {
		s.Logger.Info("execution completed",
			"sandbox_id", ex.SandboxID,
			"execution_id", ex.ID,
			"run_id", ex.RunID,
			"image_ref", ex.ImageRef,
			"image_digest", ex.ImageDigest,
			"exit_code", ex.ExitCode,
			"status", ex.Status.String(),
		)
	}
}

func (s *Service) runAdapterExecution(runCtx context.Context, adapter backend.Adapter, runReq backend.RunRequest, key string) (*backend.RunResult, bool, error) {
	if persistentAdapter, ok := adapter.(backend.PersistentSandboxAdapter); ok {
		result, err := persistentAdapter.RunInSandbox(runCtx, runReq, s.executionOutputStream(key))
		return result, true, err
	}
	if streamAdapter, ok := adapter.(backend.StreamingAdapter); ok {
		result, err := streamAdapter.RunStream(runCtx, runReq, s.executionOutputStream(key))
		return result, true, err
	}

	result, err := adapter.Run(runCtx, runReq)
	return result, false, err
}

func (s *Service) executionOutputStream(key string) backend.OutputStream {
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
		OnAttach: func(io backend.AttachIO) {
			s.setExecutionAttachIO(key, io)
		},
	}
}

func (s *Service) ensureRepositoryMirrorContains(ctx context.Context, repository *repositorycheckout.Checkout) error {
	if repository == nil || s.RepositoryMirrors == nil {
		return nil
	}
	return s.RepositoryMirrors.EnsureMirrorContains(ctx, repository.RemoteURL, repository.CommitSHA)
}

func (s *Service) preparePersistentSandboxRepository(
	ctx context.Context,
	sandboxID string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	adapter backend.PersistentSandboxAdapter,
	repository *repositorycheckout.Checkout,
) error {
	if repository == nil {
		return nil
	}

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
	case repositoryCheckoutsEqual(sandbox.Repository, repository):
		s.mu.Unlock()
		return nil
	default:
		s.mu.Unlock()
		return fmt.Errorf("sandbox %q already has a different repository checkout", sandboxID)
	}
	s.mu.Unlock()

	err := s.bootstrapRepositoryInPersistentSandbox(ctx, adapter, sandboxID, compiled, firecrackerCfg, repository)

	s.mu.Lock()
	if sandbox, ok := s.sandboxes[sandboxID]; ok {
		sandbox.RepositoryBusy = false
		if err == nil {
			sandbox.Repository = cloneRepositoryCheckout(repository)
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
	adapter backend.PersistentSandboxAdapter,
	sandboxID string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	repository *repositorycheckout.Checkout,
) error {
	if repository == nil {
		return nil
	}
	if err := s.ensureRepositoryMirrorContains(ctx, repository); err != nil {
		return err
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	result, err := adapter.RunInSandbox(ctx, backend.RunRequest{
		SandboxID:         sandboxID,
		RunID:             s.ids().NewRunID(),
		Command:           repositorycheckout.BuildBootstrapCommand(repository),
		Policy:            compiled,
		FirecrackerConfig: firecrackerCfg,
	}, backend.OutputStream{
		OnStdout: func(chunk []byte) {
			_, _ = stdout.Write(chunk)
		},
		OnStderr: func(chunk []byte) {
			_, _ = stderr.Write(chunk)
		},
	})
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	if result == nil {
		return errors.New("bootstrap execution returned no result")
	}
	if result.ExitCode != 0 {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			msg = strings.TrimSpace(result.Message)
		}
		if msg == "" {
			msg = fmt.Sprintf("bootstrap command failed with exit code %d", result.ExitCode)
		}
		return errors.New(msg)
	}
	return nil
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

func (s *Service) beginSnapshotUseLocked(snapshotID string) error {
	if _, deleting := s.snapshotDeletions[snapshotID]; deleting {
		return fmt.Errorf("snapshot_busy: snapshot %q is being deleted", snapshotID)
	}
	s.snapshotOps[snapshotID]++
	return nil
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

func (s *Service) finishSnapshotDelete(snapshotID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.snapshotDeletions, snapshotID)
}
}

func (s *Service) mergeBufferedResultOutputLocked(ex *executionState, result *backend.RunResult, usedStreaming bool) {
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
}

func (s *Service) hasActiveExecutionLocked(sandboxID string) bool {
	for _, ex := range s.executions {
		if ex.SandboxID == sandboxID && !isFinalExecutionStatus(ex.Status) {
			return true
		}
	}
	return false
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

func withSnapshotDriver(cfg backend.FirecrackerConfig, driver string) backend.FirecrackerConfig {
	cfg.Snapshots.Enabled = true
	cfg.Snapshots.Driver = runtimeconfig.SnapshotDriverOrDefault(driver)
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
