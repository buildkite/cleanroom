package controlserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/buildkite/cleanroom/internal/controlapi"
	"github.com/buildkite/cleanroom/internal/controlservice"
	"github.com/buildkite/cleanroom/internal/endpoint"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/gen/cleanroom/v1/cleanroomv1connect"
	"github.com/buildkite/cleanroom/internal/paths"
	"github.com/charmbracelet/log"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"tailscale.com/tsnet"
)

type Server struct {
	service *controlservice.Service
	logger  *log.Logger
}

func New(service *controlservice.Service, logger *log.Logger) *Server {
	return &Server{service: service, logger: logger}
}

type tsnetServer interface {
	Listen(network, addr string) (net.Listener, error)
	Close() error
}

var newTSNetServer = func(ep endpoint.Endpoint, stateDir string) tsnetServer {
	return &tsnet.Server{
		Dir:      stateDir,
		Hostname: ep.TSNetHostname,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	sandboxPath, sandboxHandler := cleanroomv1connect.NewSandboxServiceHandler(s)
	executionPath, executionHandler := cleanroomv1connect.NewExecutionServiceHandler(s)
	mux.Handle(sandboxPath, sandboxHandler)
	mux.Handle(executionPath, executionHandler)

	// Backward-compatible JSON endpoints.
	mux.HandleFunc("/v1/exec", s.handleExec)
	mux.HandleFunc("/v1/cleanrooms/launch", s.handleLaunchCleanroom)
	mux.HandleFunc("/v1/cleanrooms/run", s.handleRunCleanroom)
	mux.HandleFunc("/v1/cleanrooms/terminate", s.handleTerminateCleanroom)

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return h2c.NewHandler(mux, &http2.Server{})
}

func (s *Server) CreateSandbox(ctx context.Context, req *connect.Request[cleanroomv1.CreateSandboxRequest]) (*connect.Response[cleanroomv1.CreateSandboxResponse], error) {
	resp, err := s.service.CreateSandbox(ctx, req.Msg)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) GetSandbox(ctx context.Context, req *connect.Request[cleanroomv1.GetSandboxRequest]) (*connect.Response[cleanroomv1.GetSandboxResponse], error) {
	resp, err := s.service.GetSandbox(ctx, req.Msg)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) ListSandboxes(ctx context.Context, req *connect.Request[cleanroomv1.ListSandboxesRequest]) (*connect.Response[cleanroomv1.ListSandboxesResponse], error) {
	resp, err := s.service.ListSandboxes(ctx, req.Msg)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) TerminateSandbox(ctx context.Context, req *connect.Request[cleanroomv1.TerminateSandboxRequest]) (*connect.Response[cleanroomv1.TerminateSandboxResponse], error) {
	resp, err := s.service.TerminateSandbox(ctx, req.Msg)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) StreamSandboxEvents(ctx context.Context, req *connect.Request[cleanroomv1.StreamSandboxEventsRequest], stream *connect.ServerStream[cleanroomv1.SandboxEvent]) error {
	history, updates, done, unsubscribe, err := s.service.SubscribeSandboxEvents(req.Msg.GetSandboxId())
	if err != nil {
		return toConnectError(err)
	}
	defer unsubscribe()

	for _, event := range history {
		if err := stream.Send(event); err != nil {
			return err
		}
	}
	if !req.Msg.GetFollow() {
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-updates:
			if !ok {
				return streamSubscriberDroppedErr(done, "sandbox")
			}
			if err := stream.Send(event); err != nil {
				return err
			}
		case <-done:
			return drainSandboxEvents(stream, updates)
		}
	}
}

func (s *Server) CreateExecution(ctx context.Context, req *connect.Request[cleanroomv1.CreateExecutionRequest]) (*connect.Response[cleanroomv1.CreateExecutionResponse], error) {
	resp, err := s.service.CreateExecution(ctx, req.Msg)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) GetExecution(ctx context.Context, req *connect.Request[cleanroomv1.GetExecutionRequest]) (*connect.Response[cleanroomv1.GetExecutionResponse], error) {
	resp, err := s.service.GetExecution(ctx, req.Msg)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) CancelExecution(ctx context.Context, req *connect.Request[cleanroomv1.CancelExecutionRequest]) (*connect.Response[cleanroomv1.CancelExecutionResponse], error) {
	resp, err := s.service.CancelExecution(ctx, req.Msg)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) StreamExecution(ctx context.Context, req *connect.Request[cleanroomv1.StreamExecutionRequest], stream *connect.ServerStream[cleanroomv1.ExecutionStreamEvent]) error {
	history, updates, done, unsubscribe, err := s.service.SubscribeExecutionEvents(req.Msg.GetSandboxId(), req.Msg.GetExecutionId())
	if err != nil {
		return toConnectError(err)
	}
	defer unsubscribe()

	for _, event := range history {
		if err := stream.Send(event); err != nil {
			return err
		}
	}
	if !req.Msg.GetFollow() {
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-updates:
			if !ok {
				return streamSubscriberDroppedErr(done, "execution")
			}
			if err := stream.Send(event); err != nil {
				return err
			}
		case <-done:
			return drainExecutionEvents(stream, updates)
		}
	}
}

func (s *Server) AttachExecution(ctx context.Context, stream *connect.BidiStream[cleanroomv1.ExecutionAttachFrame, cleanroomv1.ExecutionAttachFrame]) error {
	first, err := stream.Receive()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return toConnectError(errors.New("missing attach open frame"))
		}
		return err
	}

	sandboxID, executionID := resolveAttachTarget(first)
	if sandboxID == "" || executionID == "" {
		return toConnectError(errors.New("attach frame missing sandbox_id or execution_id"))
	}

	if detached, err := s.applyAttachInput(ctx, sandboxID, executionID, first); err != nil {
		return err
	} else if detached {
		return nil
	}

	history, updates, done, unsubscribe, err := s.service.SubscribeExecutionEvents(sandboxID, executionID)
	if err != nil {
		return toConnectError(err)
	}
	defer unsubscribe()

	recvErr := make(chan error, 1)
	go func() {
		for {
			frame, recvErrInner := stream.Receive()
			if recvErrInner != nil {
				if errors.Is(recvErrInner, io.EOF) {
					recvErr <- nil
					return
				}
				recvErr <- recvErrInner
				return
			}
			detach, applyErr := s.applyAttachInput(ctx, sandboxID, executionID, frame)
			if applyErr != nil {
				recvErr <- applyErr
				return
			}
			if detach {
				recvErr <- nil
				return
			}
		}
	}()

	for _, event := range history {
		if err := stream.Send(executionEventToAttachFrame(event)); err != nil {
			return err
		}
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-recvErr:
			if err == nil {
				return nil
			}
			return err
		case event, ok := <-updates:
			if !ok {
				return streamSubscriberDroppedErr(done, "execution attach")
			}
			if err := stream.Send(executionEventToAttachFrame(event)); err != nil {
				return err
			}
		case <-done:
			if err := drainAttachEvents(stream, updates); err != nil {
				return err
			}
			return nil
		}
	}
}

func (s *Server) applyAttachInput(ctx context.Context, sandboxID, executionID string, frame *cleanroomv1.ExecutionAttachFrame) (bool, error) {
	if frame == nil {
		return false, nil
	}

	switch payload := frame.Payload.(type) {
	case *cleanroomv1.ExecutionAttachFrame_Open:
		return false, nil
	case *cleanroomv1.ExecutionAttachFrame_Signal:
		_, err := s.service.CancelExecution(ctx, &cleanroomv1.CancelExecutionRequest{
			SandboxId:   sandboxID,
			ExecutionId: executionID,
			Signal:      payload.Signal.GetSignal(),
		})
		if err != nil {
			return false, toConnectError(err)
		}
		return false, nil
	case *cleanroomv1.ExecutionAttachFrame_Close:
		if !payload.Close.GetDetach() {
			_, err := s.service.CancelExecution(ctx, &cleanroomv1.CancelExecutionRequest{
				SandboxId:   sandboxID,
				ExecutionId: executionID,
				Signal:      2,
			})
			if err != nil {
				return false, toConnectError(err)
			}
		}
		return true, nil
	case *cleanroomv1.ExecutionAttachFrame_Heartbeat:
		return false, nil
	case *cleanroomv1.ExecutionAttachFrame_Resize:
		resize := payload.Resize
		if resize == nil {
			return false, nil
		}
		if err := s.service.ResizeExecutionTTY(sandboxID, executionID, resize.GetCols(), resize.GetRows()); err != nil {
			if errors.Is(err, controlservice.ErrExecutionResizeUnsupported) {
				return false, connect.NewError(connect.CodeUnimplemented, err)
			}
			return false, toConnectError(err)
		}
		return false, nil
	case *cleanroomv1.ExecutionAttachFrame_Stdin:
		if err := s.service.WriteExecutionStdin(sandboxID, executionID, payload.Stdin); err != nil {
			if errors.Is(err, controlservice.ErrExecutionStdinUnsupported) {
				return false, connect.NewError(connect.CodeUnimplemented, err)
			}
			return false, toConnectError(err)
		}
		return false, nil
	default:
		return false, nil
	}
}

func resolveAttachTarget(frame *cleanroomv1.ExecutionAttachFrame) (string, string) {
	if frame == nil {
		return "", ""
	}
	sandboxID := strings.TrimSpace(frame.GetSandboxId())
	executionID := strings.TrimSpace(frame.GetExecutionId())
	if open := frame.GetOpen(); open != nil {
		if sandboxID == "" {
			sandboxID = strings.TrimSpace(open.GetSandboxId())
		}
		if executionID == "" {
			executionID = strings.TrimSpace(open.GetExecutionId())
		}
	}
	return sandboxID, executionID
}

func executionEventToAttachFrame(event *cleanroomv1.ExecutionStreamEvent) *cleanroomv1.ExecutionAttachFrame {
	if event == nil {
		return &cleanroomv1.ExecutionAttachFrame{}
	}
	frame := &cleanroomv1.ExecutionAttachFrame{
		SandboxId:   event.GetSandboxId(),
		ExecutionId: event.GetExecutionId(),
		OccurredAt:  event.GetOccurredAt(),
	}
	switch payload := event.Payload.(type) {
	case *cleanroomv1.ExecutionStreamEvent_Stdout:
		frame.Payload = &cleanroomv1.ExecutionAttachFrame_Stdout{Stdout: payload.Stdout}
	case *cleanroomv1.ExecutionStreamEvent_Stderr:
		frame.Payload = &cleanroomv1.ExecutionAttachFrame_Stderr{Stderr: payload.Stderr}
	case *cleanroomv1.ExecutionStreamEvent_Exit:
		frame.Payload = &cleanroomv1.ExecutionAttachFrame_Exit{Exit: payload.Exit}
	case *cleanroomv1.ExecutionStreamEvent_Message:
		frame.Payload = &cleanroomv1.ExecutionAttachFrame_Error{Error: payload.Message}
	default:
		frame.Payload = &cleanroomv1.ExecutionAttachFrame_Error{Error: event.GetStatus().String()}
	}
	return frame
}

func streamSubscriberDroppedErr(done <-chan struct{}, streamName string) error {
	select {
	case <-done:
		return nil
	default:
		return connect.NewError(
			connect.CodeResourceExhausted,
			fmt.Errorf("%s stream closed because the client could not keep up with event throughput", streamName),
		)
	}
}

func drainSandboxEvents(stream *connect.ServerStream[cleanroomv1.SandboxEvent], updates <-chan *cleanroomv1.SandboxEvent) error {
	for {
		select {
		case event, ok := <-updates:
			if !ok {
				return nil
			}
			if err := stream.Send(event); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

func drainExecutionEvents(stream *connect.ServerStream[cleanroomv1.ExecutionStreamEvent], updates <-chan *cleanroomv1.ExecutionStreamEvent) error {
	for {
		select {
		case event, ok := <-updates:
			if !ok {
				return nil
			}
			if err := stream.Send(event); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

func drainAttachEvents(stream *connect.BidiStream[cleanroomv1.ExecutionAttachFrame, cleanroomv1.ExecutionAttachFrame], updates <-chan *cleanroomv1.ExecutionStreamEvent) error {
	for {
		select {
		case event, ok := <-updates:
			if !ok {
				return nil
			}
			if err := stream.Send(executionEventToAttachFrame(event)); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

func toConnectError(err error) error {
	if err == nil {
		return nil
	}
	var connectErr *connect.Error
	if errors.As(err, &connectErr) {
		return err
	}

	code := connect.CodeInternal
	message := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, context.Canceled):
		code = connect.CodeCanceled
	case errors.Is(err, context.DeadlineExceeded):
		code = connect.CodeDeadlineExceeded
	case strings.Contains(message, "missing "), strings.Contains(message, "invalid"):
		code = connect.CodeInvalidArgument
	case strings.Contains(message, "unknown sandbox"), strings.Contains(message, "unknown cleanroom"), strings.Contains(message, "unknown execution"):
		code = connect.CodeNotFound
	case strings.Contains(message, "not ready"):
		code = connect.CodeFailedPrecondition
	}
	return connect.NewError(code, err)
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	in := controlapi.ExecRequest{}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	if s.logger != nil {
		s.logger.Debug("exec request decoded",
			"remote_addr", r.RemoteAddr,
			"cwd", in.CWD,
			"backend", in.Backend,
			"command_argc", len(in.Command),
			"dry_run", in.Options.DryRun,
			"host_passthrough", in.Options.HostPassthrough,
		)
	}

	out, err := s.service.Exec(r.Context(), in)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("exec request failed", "error", err, "duration_ms", time.Since(started).Milliseconds())
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.logger != nil {
		s.logger.Info("exec request finished",
			"run_id", out.RunID,
			"exit_code", out.ExitCode,
			"duration_ms", time.Since(started).Milliseconds(),
		)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleLaunchCleanroom(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	in := controlapi.LaunchCleanroomRequest{}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	if s.logger != nil {
		s.logger.Debug("launch cleanroom request decoded",
			"remote_addr", r.RemoteAddr,
			"cwd", in.CWD,
			"backend", in.Backend,
		)
	}

	out, err := s.service.LaunchCleanroom(r.Context(), in)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("launch cleanroom request failed", "error", err, "duration_ms", time.Since(started).Milliseconds())
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.logger != nil {
		s.logger.Info("launch cleanroom request finished",
			"cleanroom_id", out.CleanroomID,
			"duration_ms", time.Since(started).Milliseconds(),
		)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRunCleanroom(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	in := controlapi.RunCleanroomRequest{}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	if s.logger != nil {
		s.logger.Debug("run cleanroom request decoded",
			"remote_addr", r.RemoteAddr,
			"cleanroom_id", in.CleanroomID,
			"command_argc", len(in.Command),
		)
	}

	out, err := s.service.RunCleanroom(r.Context(), in)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("run cleanroom request failed", "error", err, "duration_ms", time.Since(started).Milliseconds())
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.logger != nil {
		s.logger.Info("run cleanroom request finished",
			"cleanroom_id", out.CleanroomID,
			"run_id", out.RunID,
			"exit_code", out.ExitCode,
			"duration_ms", time.Since(started).Milliseconds(),
		)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleTerminateCleanroom(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	in := controlapi.TerminateCleanroomRequest{}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	if s.logger != nil {
		s.logger.Debug("terminate cleanroom request decoded",
			"remote_addr", r.RemoteAddr,
			"cleanroom_id", in.CleanroomID,
		)
	}

	out, err := s.service.TerminateCleanroom(r.Context(), in)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("terminate cleanroom request failed", "error", err, "duration_ms", time.Since(started).Milliseconds())
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.logger != nil {
		s.logger.Info("terminate cleanroom request finished",
			"cleanroom_id", out.CleanroomID,
			"duration_ms", time.Since(started).Milliseconds(),
		)
	}
	writeJSON(w, http.StatusOK, out)
}

func Serve(ctx context.Context, ep endpoint.Endpoint, handler http.Handler, logger *log.Logger) error {
	listener, cleanup, err := listen(ep)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer func() {
			_ = cleanup()
		}()
	}
	defer listener.Close()
	if logger != nil {
		logger.Info("serving cleanroom control API", "endpoint", ep.Address, "scheme", ep.Scheme, "base_url", ep.BaseURL)
	}

	httpServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- httpServer.Serve(listener)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		if ep.Scheme == "unix" {
			_ = os.Remove(ep.Address)
		}
		if logger != nil {
			logger.Info("control API shutdown complete", "endpoint", ep.Address)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		if logger != nil {
			logger.Error("control API serve failed", "error", err)
		}
		return err
	}
}

func listen(ep endpoint.Endpoint) (net.Listener, func() error, error) {
	if ep.Scheme == "unix" {
		if err := os.MkdirAll(filepath.Dir(ep.Address), 0o755); err != nil {
			return nil, nil, err
		}
		if err := os.Remove(ep.Address); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, nil, err
		}
		listener, err := net.Listen("unix", ep.Address)
		if err != nil {
			return nil, nil, err
		}
		if err := os.Chmod(ep.Address, 0o600); err != nil {
			_ = listener.Close()
			return nil, nil, err
		}
		return listener, nil, nil
	}

	if ep.Scheme == "tsnet" {
		stateDir, err := paths.TSNetStateDir()
		if err != nil {
			return nil, nil, fmt.Errorf("resolve tsnet state directory: %w", err)
		}
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			return nil, nil, fmt.Errorf("create tsnet state directory: %w", err)
		}
		server := newTSNetServer(ep, stateDir)
		listener, err := server.Listen("tcp", ep.Address)
		if err != nil {
			_ = server.Close()
			return nil, nil, fmt.Errorf("start tsnet listener for %q: %w", ep.Address, err)
		}
		return listener, server.Close, nil
	}

	if ep.Scheme == "https" {
		return nil, nil, errors.New("https listen endpoints are not supported yet: TLS configuration is not implemented")
	}
	if ep.Scheme == "http" {
		addr := ep.Address
		if len(addr) >= 7 && addr[:7] == "http://" {
			addr = addr[7:]
		}
		listener, err := net.Listen("tcp", addr)
		return listener, nil, err
	}

	return nil, nil, fmt.Errorf("unsupported endpoint scheme %q", ep.Scheme)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, controlapi.ErrorResponse{Error: message})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
