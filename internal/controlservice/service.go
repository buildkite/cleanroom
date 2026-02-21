package controlservice

import (
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
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"github.com/charmbracelet/log"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Service struct {
	Loader   loader
	Config   runtimeconfig.Config
	Backends map[string]backend.Adapter
	Logger   *log.Logger

	mu         sync.RWMutex
	sandboxes  map[string]*sandboxState
	executions map[string]*executionState
}

type sandboxState struct {
	ID                string
	Backend           string
	Policy            *policy.CompiledPolicy
	Firecracker       backend.FirecrackerConfig
	ActiveExecutionID string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	LastExecutionID   string
	Status            cleanroomv1.SandboxStatus
	EventHistory      []*cleanroomv1.SandboxEvent
	EventSubscribers  map[int]chan *cleanroomv1.SandboxEvent
	NextSubID         int
	Done              chan struct{}
	DoneClosed        bool
}

type executionState struct {
	ID               string
	SandboxID        string
	RunID            string
	ImageRef         string
	ImageDigest      string
	Command          []string
	Options          executionOptions
	TTY              bool
	Status           cleanroomv1.ExecutionStatus
	ExitCode         int32
	StartedAt        *time.Time
	FinishedAt       *time.Time
	Message          string
	Stdout           string
	Stderr           string
	LaunchedVM       bool
	PlanPath         string
	RunDir           string
	CancelRequested  bool
	CancelSignal     int32
	Cancel           context.CancelFunc
	AttachStdin      func([]byte) error
	AttachResize     func(cols, rows uint32) error
	EventHistory     []*cleanroomv1.ExecutionStreamEvent
	EventSubscribers map[int]chan *cleanroomv1.ExecutionStreamEvent
	NextSubID        int
	Done             chan struct{}
	DoneClosed       bool
}

type loader interface {
	LoadAndCompile(cwd string) (*policy.CompiledPolicy, string, error)
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
	maxRetainedStoppedSandboxes   = 256
	maxRetainedFinishedExecutions = 2048
	retainedStateMaxAge           = 24 * time.Hour

	ErrExecutionStdinUnsupported  = errors.New("execution stdin attach is not supported by the current backend")
	ErrExecutionResizeUnsupported = errors.New("execution resize is not supported by the current backend")
)

const (
	attachRegistrationWait = 250 * time.Millisecond
	attachPollInterval     = 10 * time.Millisecond
)

func (s *Service) CreateSandbox(ctx context.Context, req *cleanroomv1.CreateSandboxRequest) (*cleanroomv1.CreateSandboxResponse, error) {
	if req == nil {
		return nil, errors.New("missing request")
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

	opts := req.GetOptions()
	execOpts := executionOptions{}
	if opts != nil {
		execOpts.LaunchSeconds = opts.GetLaunchSeconds()
	}
	firecrackerCfg := mergeFirecrackerConfig(execOpts, s.Config)
	firecrackerCfg.RunDir = ""

	now := time.Now().UTC()
	sandboxID := fmt.Sprintf("cr-%d", now.UnixNano())

	if persistentAdapter, ok := adapter.(backend.PersistentSandboxAdapter); ok {
		if err := persistentAdapter.ProvisionSandbox(ctx, backend.ProvisionRequest{
			SandboxID:         sandboxID,
			Policy:            compiled,
			FirecrackerConfig: firecrackerCfg,
		}); err != nil {
			return nil, fmt.Errorf("provision sandbox: %w", err)
		}
	}

	state := &sandboxState{
		ID:               sandboxID,
		Backend:          backendName,
		Policy:           compiled,
		Firecracker:      firecrackerCfg,
		CreatedAt:        now,
		UpdatedAt:        now,
		Status:           cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY,
		EventSubscribers: map[int]chan *cleanroomv1.SandboxEvent{},
		Done:             make(chan struct{}),
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

func (s *Service) TerminateSandbox(_ context.Context, req *cleanroomv1.TerminateSandboxRequest) (*cleanroomv1.TerminateSandboxResponse, error) {
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

		terminatedAt := time.Now().UTC()
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
				OccurredAt:  timestamppb.Now(),
			})
			if ex.Status == cleanroomv1.ExecutionStatus_EXECUTION_STATUS_QUEUED {
				ex.Status = cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED
				ex.ExitCode = cancelExitCode(ex.CancelSignal)
				finished := terminatedAt
				ex.FinishedAt = &finished
				s.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
					SandboxId:   ex.SandboxID,
					ExecutionId: ex.ID,
					Status:      ex.Status,
					Payload: &cleanroomv1.ExecutionStreamEvent_Exit{Exit: &cleanroomv1.ExecutionExit{
						ExitCode: ex.ExitCode,
						Status:   ex.Status,
						Message:  "execution canceled before start (sandbox termination)",
					}},
					OccurredAt: timestamppb.New(finished),
				})
				closeExecutionDoneLocked(ex)
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
		if err := persistentAdapter.TerminateSandbox(context.Background(), sandboxID); err != nil {
			if s.Logger != nil {
				s.Logger.Warn("terminate backend sandbox failed", "sandbox_id", sandboxID, "backend", backendName, "error", err)
			}
			return nil, fmt.Errorf("terminate backend sandbox: %w", err)
		}
	}

	now := time.Now().UTC()
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

func (s *Service) CreateExecution(_ context.Context, req *cleanroomv1.CreateExecutionRequest) (*cleanroomv1.CreateExecutionResponse, error) {
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

	execOpts := executionOptions{}
	tty := false
	if opts := req.GetOptions(); opts != nil {
		execOpts = executionOptions{
			LaunchSeconds: opts.GetLaunchSeconds(),
		}
		tty = opts.GetTty()
	}

	now := time.Now().UTC()
	executionID := fmt.Sprintf("exec-%d", now.UnixNano())

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
	imageRef := ""
	imageDigest := ""
	if sandbox.Policy != nil {
		imageRef = sandbox.Policy.ImageRef
		imageDigest = sandbox.Policy.ImageDigest
	}

	ex := &executionState{
		ID:               executionID,
		SandboxID:        sandboxID,
		ImageRef:         imageRef,
		ImageDigest:      imageDigest,
		Command:          append([]string(nil), command...),
		Options:          execOpts,
		TTY:              tty,
		Status:           cleanroomv1.ExecutionStatus_EXECUTION_STATUS_QUEUED,
		EventSubscribers: map[int]chan *cleanroomv1.ExecutionStreamEvent{},
		Done:             make(chan struct{}),
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
		)
	}
	return resp, nil
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

	now := time.Now().UTC()
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
		ex.Status = cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED
		ex.ExitCode = cancelExitCode(signalNum)
		finished := now
		ex.FinishedAt = &finished
		s.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
			SandboxId:   sandboxID,
			ExecutionId: executionID,
			Status:      ex.Status,
			Payload: &cleanroomv1.ExecutionStreamEvent_Exit{Exit: &cleanroomv1.ExecutionExit{
				ExitCode: ex.ExitCode,
				Status:   ex.Status,
				Message:  "execution canceled before start",
			}},
			OccurredAt: timestamppb.New(finished),
		})
		closeExecutionDoneLocked(ex)
		s.pruneStateLocked(now)
		status = ex.Status
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
	deadline := time.Now().Add(attachRegistrationWait)
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
		if time.Now().After(deadline) {
			return ErrExecutionStdinUnsupported
		}
		select {
		case <-done:
		case <-time.After(attachPollInterval):
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

	deadline := time.Now().Add(attachRegistrationWait)
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
		if time.Now().After(deadline) {
			return ErrExecutionResizeUnsupported
		}
		select {
		case <-done:
		case <-time.After(attachPollInterval):
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

	history := append([]*cleanroomv1.SandboxEvent(nil), sb.EventHistory...)
	updates := make(chan *cleanroomv1.SandboxEvent, 64)
	done := sb.Done

	subID := sb.NextSubID
	sb.NextSubID++
	if sb.EventSubscribers == nil {
		sb.EventSubscribers = map[int]chan *cleanroomv1.SandboxEvent{}
	}

	select {
	case <-done:
		close(updates)
	default:
		sb.EventSubscribers[subID] = updates
	}

	unsubscribe := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		subSB, ok := s.sandboxes[sandboxID]
		if !ok {
			return
		}
		ch, ok := subSB.EventSubscribers[subID]
		if !ok {
			return
		}
		delete(subSB.EventSubscribers, subID)
		close(ch)
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

	history := append([]*cleanroomv1.ExecutionStreamEvent(nil), ex.EventHistory...)
	updates := make(chan *cleanroomv1.ExecutionStreamEvent, 128)
	done := ex.Done

	subID := ex.NextSubID
	ex.NextSubID++
	if ex.EventSubscribers == nil {
		ex.EventSubscribers = map[int]chan *cleanroomv1.ExecutionStreamEvent{}
	}

	select {
	case <-done:
		close(updates)
	default:
		ex.EventSubscribers[subID] = updates
	}

	unsubscribe := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		subEx, ok := s.executions[executionKey(sandboxID, executionID)]
		if !ok {
			return
		}
		ch, ok := subEx.EventSubscribers[subID]
		if !ok {
			return
		}
		delete(subEx.EventSubscribers, subID)
		close(ch)
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
		ex.Status = cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED
		ex.ExitCode = 1
		finished := time.Now().UTC()
		ex.FinishedAt = &finished
		s.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
			SandboxId:   sandboxID,
			ExecutionId: executionID,
			Status:      ex.Status,
			Payload: &cleanroomv1.ExecutionStreamEvent_Exit{Exit: &cleanroomv1.ExecutionExit{
				ExitCode: ex.ExitCode,
				Status:   ex.Status,
				Message:  "sandbox no longer exists",
			}},
			OccurredAt: timestamppb.New(finished),
		})
		closeExecutionDoneLocked(ex)
		s.pruneStateLocked(finished)
		s.mu.Unlock()
		return
	}
	if sb.Status != cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY {
		ex.Status = cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED
		ex.ExitCode = 1
		if sb.Status == cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPING || sb.Status == cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED {
			ex.Status = cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED
			ex.ExitCode = cancelExitCode(ex.CancelSignal)
		}
		finished := time.Now().UTC()
		ex.FinishedAt = &finished
		s.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
			SandboxId:   sandboxID,
			ExecutionId: executionID,
			Status:      ex.Status,
			Payload: &cleanroomv1.ExecutionStreamEvent_Exit{Exit: &cleanroomv1.ExecutionExit{
				ExitCode: ex.ExitCode,
				Status:   ex.Status,
				Message:  fmt.Sprintf("sandbox %q is not ready", sandboxID),
			}},
			OccurredAt: timestamppb.New(finished),
		})
		closeExecutionDoneLocked(ex)
		s.pruneStateLocked(finished)
		s.mu.Unlock()
		return
	}
	adapter, ok := s.Backends[sb.Backend]
	if !ok {
		ex.Status = cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED
		ex.ExitCode = 1
		finished := time.Now().UTC()
		ex.FinishedAt = &finished
		s.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
			SandboxId:   sandboxID,
			ExecutionId: executionID,
			Status:      ex.Status,
			Payload: &cleanroomv1.ExecutionStreamEvent_Exit{Exit: &cleanroomv1.ExecutionExit{
				ExitCode: ex.ExitCode,
				Status:   ex.Status,
				Message:  fmt.Sprintf("unknown backend %q", sb.Backend),
			}},
			OccurredAt: timestamppb.New(finished),
		})
		closeExecutionDoneLocked(ex)
		s.pruneStateLocked(finished)
		s.mu.Unlock()
		return
	}

	runCtx, cancel := context.WithCancel(context.Background())
	ex.Cancel = cancel

	started := time.Now().UTC()
	ex.StartedAt = &started
	ex.Status = cleanroomv1.ExecutionStatus_EXECUTION_STATUS_RUNNING
	ex.RunID = fmt.Sprintf("run-%d", started.UnixNano())
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

	usedStreaming := false
	var result *backend.RunResult
	var err error
	if persistentAdapter, ok := adapter.(backend.PersistentSandboxAdapter); ok {
		usedStreaming = true
		result, err = persistentAdapter.RunInSandbox(runCtx, runReq, backend.OutputStream{
			OnStdout: func(chunk []byte) {
				s.recordExecutionOutputChunk(key, true, chunk)
			},
			OnStderr: func(chunk []byte) {
				s.recordExecutionOutputChunk(key, false, chunk)
			},
			OnAttach: func(io backend.AttachIO) {
				s.setExecutionAttachIO(key, io)
			},
		})
	} else if streamAdapter, ok := adapter.(backend.StreamingAdapter); ok {
		usedStreaming = true
		result, err = streamAdapter.RunStream(runCtx, runReq, backend.OutputStream{
			OnStdout: func(chunk []byte) {
				s.recordExecutionOutputChunk(key, true, chunk)
			},
			OnStderr: func(chunk []byte) {
				s.recordExecutionOutputChunk(key, false, chunk)
			},
			OnAttach: func(io backend.AttachIO) {
				s.setExecutionAttachIO(key, io)
			},
		})
	} else {
		result, err = adapter.Run(runCtx, runReq)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	ex, ok = s.executions[key]
	if !ok {
		return
	}
	if sb, ok := s.sandboxes[sandboxID]; ok && sb.ActiveExecutionID == executionID {
		sb.ActiveExecutionID = ""
		sb.UpdatedAt = time.Now().UTC()
	}

	if ex.Cancel != nil {
		ex.Cancel = nil
	}
	clearExecutionAttachIOLocked(ex)

	if err != nil {
		finalStatus := cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED
		exitCode := int32(1)
		if ex.CancelRequested || errors.Is(runCtx.Err(), context.Canceled) {
			finalStatus = cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED
			exitCode = cancelExitCode(ex.CancelSignal)
		} else if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			finalStatus = cleanroomv1.ExecutionStatus_EXECUTION_STATUS_TIMED_OUT
			exitCode = 124
		}

		ex.Status = finalStatus
		ex.ExitCode = exitCode
		ex.Message = err.Error()
		if strings.TrimSpace(err.Error()) != "" {
			ex.Stderr += err.Error() + "\n"
			s.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
				SandboxId:   ex.SandboxID,
				ExecutionId: ex.ID,
				Status:      finalStatus,
				Payload:     &cleanroomv1.ExecutionStreamEvent_Stderr{Stderr: []byte(err.Error() + "\n")},
				OccurredAt:  timestamppb.Now(),
			})
		}
		finished := time.Now().UTC()
		ex.FinishedAt = &finished
		s.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
			SandboxId:   ex.SandboxID,
			ExecutionId: ex.ID,
			Status:      ex.Status,
			Payload: &cleanroomv1.ExecutionStreamEvent_Exit{Exit: &cleanroomv1.ExecutionExit{
				ExitCode: ex.ExitCode,
				Status:   ex.Status,
				Message:  ex.Message,
			}},
			OccurredAt: timestamppb.New(finished),
		})
		closeExecutionDoneLocked(ex)
		s.pruneStateLocked(finished)
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
		ex.Stderr += msg
		s.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
			SandboxId:   ex.SandboxID,
			ExecutionId: ex.ID,
			Status:      cleanroomv1.ExecutionStatus_EXECUTION_STATUS_RUNNING,
			Payload:     &cleanroomv1.ExecutionStreamEvent_Stderr{Stderr: []byte(msg)},
			OccurredAt:  timestamppb.Now(),
		})
	}

	if ex.CancelRequested {
		ex.Status = cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED
		ex.ExitCode = cancelExitCode(ex.CancelSignal)
	} else if result.ExitCode == 0 {
		ex.Status = cleanroomv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED
		ex.ExitCode = int32(result.ExitCode)
	} else {
		ex.Status = cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED
		ex.ExitCode = int32(result.ExitCode)
	}
	finished := time.Now().UTC()
	ex.FinishedAt = &finished

	s.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
		SandboxId:   ex.SandboxID,
		ExecutionId: ex.ID,
		Status:      ex.Status,
		Payload: &cleanroomv1.ExecutionStreamEvent_Exit{Exit: &cleanroomv1.ExecutionExit{
			ExitCode: ex.ExitCode,
			Status:   ex.Status,
			Message:  ex.Message,
		}},
		OccurredAt: timestamppb.New(finished),
	})
	closeExecutionDoneLocked(ex)
	s.pruneStateLocked(finished)

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

func (s *Service) ensureMapsLocked() {
	if s.sandboxes == nil {
		s.sandboxes = map[string]*sandboxState{}
	}
	if s.executions == nil {
		s.executions = map[string]*executionState{}
	}
}

func (s *Service) mergeBufferedResultOutputLocked(ex *executionState, result *backend.RunResult, usedStreaming bool) {
	if ex == nil || result == nil {
		return
	}

	appendStdout := result.Stdout
	appendStderr := result.Stderr

	if usedStreaming {
		if len(ex.Stdout) > 0 && strings.HasPrefix(result.Stdout, ex.Stdout) {
			appendStdout = result.Stdout[len(ex.Stdout):]
		}
		if len(ex.Stderr) > 0 && strings.HasPrefix(result.Stderr, ex.Stderr) {
			appendStderr = result.Stderr[len(ex.Stderr):]
		}
	}

	if appendStdout != "" {
		ex.Stdout += appendStdout
		s.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
			SandboxId:   ex.SandboxID,
			ExecutionId: ex.ID,
			Status:      cleanroomv1.ExecutionStatus_EXECUTION_STATUS_RUNNING,
			Payload:     &cleanroomv1.ExecutionStreamEvent_Stdout{Stdout: []byte(appendStdout)},
			OccurredAt:  timestamppb.Now(),
		})
	}
	if appendStderr != "" {
		ex.Stderr += appendStderr
		s.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
			SandboxId:   ex.SandboxID,
			ExecutionId: ex.ID,
			Status:      cleanroomv1.ExecutionStatus_EXECUTION_STATUS_RUNNING,
			Payload:     &cleanroomv1.ExecutionStreamEvent_Stderr{Stderr: []byte(appendStderr)},
			OccurredAt:  timestamppb.Now(),
		})
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
		ex.Stdout += string(chunk)
		s.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
			SandboxId:   ex.SandboxID,
			ExecutionId: ex.ID,
			Status:      status,
			Payload:     &cleanroomv1.ExecutionStreamEvent_Stdout{Stdout: append([]byte(nil), chunk...)},
			OccurredAt:  timestamppb.Now(),
		})
		return
	}

	ex.Stderr += string(chunk)
	s.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
		SandboxId:   ex.SandboxID,
		ExecutionId: ex.ID,
		Status:      status,
		Payload:     &cleanroomv1.ExecutionStreamEvent_Stderr{Stderr: append([]byte(nil), chunk...)},
		OccurredAt:  timestamppb.Now(),
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

func closeSandboxDoneLocked(sb *sandboxState) {
	if sb.DoneClosed {
		return
	}
	close(sb.Done)
	sb.DoneClosed = true
}

func closeExecutionDoneLocked(ex *executionState) {
	if ex.DoneClosed {
		return
	}
	close(ex.Done)
	ex.DoneClosed = true
}

func clearExecutionAttachIOLocked(ex *executionState) {
	if ex == nil {
		return
	}
	ex.AttachStdin = nil
	ex.AttachResize = nil
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

func closeSandboxSubscribersLocked(sb *sandboxState) {
	for id, ch := range sb.EventSubscribers {
		close(ch)
		delete(sb.EventSubscribers, id)
	}
}

func closeExecutionSubscribersLocked(ex *executionState) {
	for id, ch := range ex.EventSubscribers {
		close(ch)
		delete(ex.EventSubscribers, id)
	}
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

func executionTerminalTime(ex *executionState) time.Time {
	if ex == nil {
		return time.Time{}
	}
	if ex.FinishedAt != nil {
		return *ex.FinishedAt
	}
	if ex.StartedAt != nil {
		return *ex.StartedAt
	}
	return time.Time{}
}

func sandboxTerminalTime(sb *sandboxState) time.Time {
	if sb == nil {
		return time.Time{}
	}
	if !sb.UpdatedAt.IsZero() {
		return sb.UpdatedAt
	}
	return sb.CreatedAt
}

func (s *Service) pruneStateLocked(now time.Time) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	s.pruneExecutionsLocked(now)
	s.pruneSandboxesLocked(now)
}

func (s *Service) pruneExecutionsLocked(now time.Time) {
	type candidate struct {
		key      string
		finished time.Time
	}

	candidates := make([]candidate, 0, len(s.executions))
	for key, ex := range s.executions {
		if ex == nil || !isFinalExecutionStatus(ex.Status) {
			continue
		}

		finished := executionTerminalTime(ex)
		if retainedStateMaxAge > 0 && !finished.IsZero() && now.Sub(finished) > retainedStateMaxAge {
			s.dropExecutionLocked(key, ex)
			continue
		}

		candidates = append(candidates, candidate{key: key, finished: finished})
	}

	limit := maxRetainedFinishedExecutions
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

	candidates := make([]candidate, 0, len(s.sandboxes))
	for sandboxID, sb := range s.sandboxes {
		if sb == nil || sb.Status != cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED {
			continue
		}
		if s.hasActiveExecutionLocked(sandboxID) {
			continue
		}

		stopped := sandboxTerminalTime(sb)
		if retainedStateMaxAge > 0 && !stopped.IsZero() && now.Sub(stopped) > retainedStateMaxAge {
			s.dropSandboxLocked(sandboxID, sb)
			continue
		}

		candidates = append(candidates, candidate{id: sandboxID, stopped: stopped})
	}

	limit := maxRetainedStoppedSandboxes
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

func isFinalExecutionStatus(status cleanroomv1.ExecutionStatus) bool {
	switch status {
	case cleanroomv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED,
		cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED,
		cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED,
		cleanroomv1.ExecutionStatus_EXECUTION_STATUS_TIMED_OUT:
		return true
	default:
		return false
	}
}

func cancelExitCode(signal int32) int32 {
	if signal <= 0 || signal > 127 {
		return 130
	}
	return 128 + signal
}

func executionKey(sandboxID, executionID string) string {
	return sandboxID + "/" + executionID
}

func cloneSandboxLocked(state *sandboxState) *cleanroomv1.Sandbox {
	if state == nil {
		return nil
	}
	policyHash := ""
	if state.Policy != nil {
		policyHash = state.Policy.Hash
	}
	return &cleanroomv1.Sandbox{
		SandboxId:  state.ID,
		Status:     state.Status,
		Backend:    state.Backend,
		PolicyHash: policyHash,
		CreatedAt:  timestamppb.New(state.CreatedAt),
		UpdatedAt:  timestamppb.New(state.UpdatedAt),
	}
}

func cloneExecutionLocked(state *executionState) *cleanroomv1.Execution {
	if state == nil {
		return nil
	}
	out := &cleanroomv1.Execution{
		ExecutionId: state.ID,
		SandboxId:   state.SandboxID,
		Status:      state.Status,
		Command:     append([]string(nil), state.Command...),
		ExitCode:    state.ExitCode,
		Tty:         state.TTY,
		RunId:       state.RunID,
	}
	if state.StartedAt != nil {
		out.StartedAt = timestamppb.New(*state.StartedAt)
	}
	if state.FinishedAt != nil {
		out.FinishedAt = timestamppb.New(*state.FinishedAt)
	}
	return out
}

func (s *Service) recordSandboxEventLocked(sb *sandboxState, status cleanroomv1.SandboxStatus, message string) {
	now := time.Now().UTC()
	sb.Status = status
	sb.UpdatedAt = now
	event := &cleanroomv1.SandboxEvent{
		SandboxId:  sb.ID,
		Status:     status,
		Message:    message,
		OccurredAt: timestamppb.New(now),
	}
	sb.EventHistory = append(sb.EventHistory, event)

	for id, ch := range sb.EventSubscribers {
		select {
		case ch <- event:
		default:
			close(ch)
			delete(sb.EventSubscribers, id)
		}
	}
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
		event.OccurredAt = timestamppb.Now()
	}
	ex.EventHistory = append(ex.EventHistory, event)

	for id, ch := range ex.EventSubscribers {
		select {
		case ch <- event:
		default:
			close(ch)
			delete(ex.EventSubscribers, id)
		}
	}
}

func normalizeCommand(command []string) []string {
	if len(command) > 0 && command[0] == "--" {
		return command[1:]
	}
	return command
}

func resolveBackendName(requested, configuredDefault string) string {
	if requested != "" {
		return requested
	}
	if configuredDefault != "" {
		return configuredDefault
	}
	return "firecracker"
}

func mergeFirecrackerConfig(opts executionOptions, cfg runtimeconfig.Config) backend.FirecrackerConfig {
	out := backend.FirecrackerConfig{
		BinaryPath:           cfg.Backends.Firecracker.BinaryPath,
		KernelImagePath:      cfg.Backends.Firecracker.KernelImage,
		RootFSPath:           cfg.Backends.Firecracker.RootFS,
		PrivilegedMode:       cfg.Backends.Firecracker.PrivilegedMode,
		PrivilegedHelperPath: cfg.Backends.Firecracker.PrivilegedHelperPath,
		VCPUs:                cfg.Backends.Firecracker.VCPUs,
		MemoryMiB:            cfg.Backends.Firecracker.MemoryMiB,
		GuestCID:             cfg.Backends.Firecracker.GuestCID,
		GuestPort:            cfg.Backends.Firecracker.GuestPort,
		LaunchSeconds:        cfg.Backends.Firecracker.LaunchSeconds,
	}

	out.Launch = true
	if opts.LaunchSeconds != 0 {
		out.LaunchSeconds = opts.LaunchSeconds
	}
	return out
}
