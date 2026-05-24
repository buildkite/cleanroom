package controlserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"crypto/tls"

	"charm.land/log/v2"
	"connectrpc.com/connect"
	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/controlservice"
	"github.com/buildkite/cleanroom/internal/endpoint"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/gen/cleanroom/v1/cleanroomv1connect"
	"github.com/buildkite/cleanroom/internal/tlsconfig"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TLSOptions holds explicit TLS paths for the server.
type TLSOptions struct {
	CertPath string
	KeyPath  string
}

type Server struct {
	service                      *controlservice.Service
	logger                       *log.Logger
	interceptors                 []connect.Interceptor
	internalWorkspaceCopyInTrust internalWorkspaceCopyInTrustMode
}

type internalWorkspaceCopyInTrustMode int

const (
	internalWorkspaceCopyInTrustNone internalWorkspaceCopyInTrustMode = iota
	internalWorkspaceCopyInTrustAll
	internalWorkspaceCopyInTrustLoopback
)

const (
	internalWorkspaceCopyInHeader        = "Cleanroom-Internal-Workspace-Copy-In"
	internalWorkspaceCopyInTrustedHeader = "Cleanroom-Internal-Workspace-Copy-In-Trusted"
)

func New(service *controlservice.Service, logger *log.Logger, interceptors ...connect.Interceptor) *Server {
	return &Server{service: service, logger: logger, interceptors: interceptors}
}

func (s *Server) TrustInternalWorkspaceCopyInRequests() *Server {
	if s != nil {
		s.internalWorkspaceCopyInTrust = internalWorkspaceCopyInTrustAll
	}
	return s
}

func (s *Server) TrustInternalWorkspaceCopyInRequestsFromLoopback() *Server {
	if s != nil {
		s.internalWorkspaceCopyInTrust = internalWorkspaceCopyInTrustLoopback
	}
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	handlerOptions := make([]connect.HandlerOption, 0, 1)
	if len(s.interceptors) > 0 {
		handlerOptions = append(handlerOptions, connect.WithInterceptors(s.interceptors...))
	}

	sandboxPath, sandboxHandler := cleanroomv1connect.NewSandboxServiceHandler(s, handlerOptions...)
	snapshotPath, snapshotHandler := cleanroomv1connect.NewSnapshotServiceHandler(s, handlerOptions...)
	cachePeerPath, cachePeerHandler := cleanroomv1connect.NewCachePeerServiceHandler(s, handlerOptions...)
	executionPath, executionHandler := cleanroomv1connect.NewExecutionServiceHandler(s, handlerOptions...)
	mux.Handle(sandboxPath, sandboxHandler)
	mux.Handle(snapshotPath, snapshotHandler)
	mux.Handle(cachePeerPath, cachePeerHandler)
	mux.Handle(executionPath, executionHandler)
	mux.HandleFunc(cachePeerZFSIncrementalExportPathPrefix, s.handleCachePeerZFSIncrementalExport)

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	handler := h2c.NewHandler(mux, &http2.Server{})
	if s.internalWorkspaceCopyInTrust == internalWorkspaceCopyInTrustLoopback {
		return markLoopbackInternalWorkspaceCopyInRequests(handler)
	}
	return handler
}

const cachePeerZFSIncrementalExportPathPrefix = "/v1/cache/export/zfs-incremental/"

func (s *Server) LookupCachePeer(ctx context.Context, req *connect.Request[cleanroomv1.LookupCachePeerRequest]) (*connect.Response[cleanroomv1.LookupCachePeerResponse], error) {
	if err := s.service.AuthorizeCachePeerBearer(req.Header().Get("Authorization")); err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	resp, err := s.service.LookupCachePeer(ctx, req.Msg)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) handleCachePeerZFSIncrementalExport(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.service.AuthorizeCachePeerBearer(req.Header.Get("Authorization")); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	token := strings.TrimSpace(strings.TrimPrefix(req.URL.Path, cachePeerZFSIncrementalExportPathPrefix))
	if token == "" || strings.Contains(token, "/") {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	tracker := &cachePeerExportResponseWriter{ResponseWriter: w}
	if err := s.service.ExportCachePeerZFSIncremental(req.Context(), token, tracker); err != nil {
		if tracker.bodyStarted {
			if s.logger != nil {
				s.logger.Warn("cache peer zfs export failed after streaming started", "error", err)
			}
			return
		}
		if errors.Is(err, controlservice.ErrCachePeerExportTokenNotFound) {
			http.NotFound(w, req)
			return
		}
		if s.logger != nil {
			s.logger.Warn("cache peer zfs export failed", "error", err)
		}
		http.Error(w, "cache peer export failed", http.StatusInternalServerError)
	}
}

type cachePeerExportResponseWriter struct {
	http.ResponseWriter
	bodyStarted bool
}

func (w *cachePeerExportResponseWriter) Write(data []byte) (int, error) {
	if len(data) > 0 {
		w.bodyStarted = true
	}
	return w.ResponseWriter.Write(data)
}

func (s *Server) CreateSandbox(ctx context.Context, req *connect.Request[cleanroomv1.CreateSandboxRequest]) (*connect.Response[cleanroomv1.CreateSandboxResponse], error) {
	resp, err := s.service.CreateSandbox(ctx, req.Msg)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) CreateSandboxStream(ctx context.Context, req *connect.Request[cleanroomv1.CreateSandboxRequest], stream *connect.ServerStream[cleanroomv1.CreateSandboxEvent]) error {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	reporter := &createSandboxEventStreamReporter{
		stream: stream,
		cancel: cancel,
	}
	resp, err := s.service.CreateSandboxWithReporter(streamCtx, req.Msg, reporter)
	if sendErr := reporter.Err(); sendErr != nil {
		return sendErr
	}
	if err != nil {
		return toConnectError(err)
	}
	if resp == nil {
		return nil
	}
	if err := reporter.Send(&cleanroomv1.CreateSandboxEvent{
		Phase:      cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_READY,
		Payload:    &cleanroomv1.CreateSandboxEvent_Response{Response: resp},
		OccurredAt: timestamppb.Now(),
	}); err != nil {
		return err
	}
	return nil
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

func (s *Server) SuspendSandbox(ctx context.Context, req *connect.Request[cleanroomv1.SuspendSandboxRequest]) (*connect.Response[cleanroomv1.SuspendSandboxResponse], error) {
	resp, err := s.service.SuspendSandbox(ctx, req.Msg)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) ResumeSandbox(ctx context.Context, req *connect.Request[cleanroomv1.ResumeSandboxRequest]) (*connect.Response[cleanroomv1.ResumeSandboxResponse], error) {
	resp, err := s.service.ResumeSandbox(ctx, req.Msg)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) DownloadSandboxFile(ctx context.Context, req *connect.Request[cleanroomv1.DownloadSandboxFileRequest]) (*connect.Response[cleanroomv1.DownloadSandboxFileResponse], error) {
	resp, err := s.service.DownloadSandboxFile(ctx, req.Msg)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) UploadSandboxFile(ctx context.Context, req *connect.Request[cleanroomv1.UploadSandboxFileRequest]) (*connect.Response[cleanroomv1.UploadSandboxFileResponse], error) {
	resp, err := s.service.UploadSandboxFile(ctx, req.Msg)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) StatSandboxPath(ctx context.Context, req *connect.Request[cleanroomv1.StatSandboxPathRequest]) (*connect.Response[cleanroomv1.StatSandboxPathResponse], error) {
	resp, err := s.service.StatSandboxPath(ctx, req.Msg)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) WalkSandboxTree(ctx context.Context, req *connect.Request[cleanroomv1.WalkSandboxTreeRequest], stream *connect.ServerStream[cleanroomv1.WalkSandboxTreeResponse]) error {
	return toConnectError(s.service.WalkSandboxTree(ctx, req.Msg, stream.Send))
}

func (s *Server) ReadSandboxFile(ctx context.Context, req *connect.Request[cleanroomv1.ReadSandboxFileRequest], stream *connect.ServerStream[cleanroomv1.ReadSandboxFileResponse]) error {
	return toConnectError(s.service.ReadSandboxFile(ctx, req.Msg, stream.Send))
}

func (s *Server) WriteSandboxFile(ctx context.Context, stream *connect.ClientStream[cleanroomv1.WriteSandboxFileRequest]) (*connect.Response[cleanroomv1.WriteSandboxFileResponse], error) {
	if !stream.Receive() {
		if err := stream.Err(); err != nil {
			return nil, toConnectError(err)
		}
		return nil, toConnectError(errors.New("missing write init"))
	}
	init := stream.Msg().GetInit()
	if init == nil {
		return nil, toConnectError(errors.New("first write message must contain init"))
	}

	reader, writer := io.Pipe()
	receiveDone := make(chan error, 1)
	go func() {
		for stream.Receive() {
			msg := stream.Msg()
			if msg.GetInit() != nil {
				err := errors.New("write init must only be sent once")
				_ = writer.CloseWithError(err)
				receiveDone <- err
				return
			}
			dataPayload, ok := msg.GetPayload().(*cleanroomv1.WriteSandboxFileRequest_Data)
			if !ok {
				continue
			}
			if len(dataPayload.Data) == 0 {
				continue
			}
			if _, err := writer.Write(dataPayload.Data); err != nil {
				receiveDone <- err
				return
			}
		}
		if err := stream.Err(); err != nil {
			_ = writer.CloseWithError(err)
			receiveDone <- err
			return
		}
		receiveDone <- writer.Close()
	}()

	resp, err := s.service.WriteSandboxFile(ctx, init, reader)
	if err != nil {
		_ = reader.CloseWithError(err)
		return nil, toConnectError(err)
	}
	if err := <-receiveDone; err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) RemoveSandboxPath(ctx context.Context, req *connect.Request[cleanroomv1.RemoveSandboxPathRequest]) (*connect.Response[cleanroomv1.RemoveSandboxPathResponse], error) {
	resp, err := s.service.RemoveSandboxPath(ctx, req.Msg)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) ArchiveSandboxPaths(ctx context.Context, req *connect.Request[cleanroomv1.ArchiveSandboxPathsRequest], stream *connect.ServerStream[cleanroomv1.ArchiveSandboxPathsResponse]) error {
	return toConnectError(s.service.ArchiveSandboxPaths(ctx, req.Msg, stream.Send))
}

func (s *Server) ExtractSandboxArchive(ctx context.Context, stream *connect.ClientStream[cleanroomv1.ExtractSandboxArchiveRequest]) (*connect.Response[cleanroomv1.ExtractSandboxArchiveResponse], error) {
	if !stream.Receive() {
		if err := stream.Err(); err != nil {
			return nil, toConnectError(err)
		}
		return nil, toConnectError(errors.New("missing extract init"))
	}
	init := stream.Msg().GetInit()
	if init == nil {
		return nil, toConnectError(errors.New("first extract message must contain init"))
	}

	reader, writer := io.Pipe()
	receiveDone := make(chan error, 1)
	go func() {
		for stream.Receive() {
			msg := stream.Msg()
			if msg.GetInit() != nil {
				err := errors.New("extract init must only be sent once")
				_ = writer.CloseWithError(err)
				receiveDone <- err
				return
			}
			dataPayload, ok := msg.GetPayload().(*cleanroomv1.ExtractSandboxArchiveRequest_Data)
			if !ok {
				continue
			}
			if len(dataPayload.Data) == 0 {
				continue
			}
			if _, err := writer.Write(dataPayload.Data); err != nil {
				receiveDone <- err
				return
			}
		}
		if err := stream.Err(); err != nil {
			_ = writer.CloseWithError(err)
			receiveDone <- err
			return
		}
		receiveDone <- writer.Close()
	}()

	resp, err := s.service.ExtractSandboxArchive(ctx, init, reader)
	if err != nil {
		_ = reader.CloseWithError(err)
		return nil, toConnectError(err)
	}
	if err := <-receiveDone; err != nil {
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

func (s *Server) CreateSnapshot(ctx context.Context, req *connect.Request[cleanroomv1.CreateSnapshotRequest]) (*connect.Response[cleanroomv1.CreateSnapshotResponse], error) {
	resp, err := s.service.CreateSnapshot(ctx, req.Msg)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) GetSnapshot(ctx context.Context, req *connect.Request[cleanroomv1.GetSnapshotRequest]) (*connect.Response[cleanroomv1.GetSnapshotResponse], error) {
	resp, err := s.service.GetSnapshot(ctx, req.Msg)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) ListSnapshots(ctx context.Context, req *connect.Request[cleanroomv1.ListSnapshotsRequest]) (*connect.Response[cleanroomv1.ListSnapshotsResponse], error) {
	resp, err := s.service.ListSnapshots(ctx, req.Msg)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) DeleteSnapshot(ctx context.Context, req *connect.Request[cleanroomv1.DeleteSnapshotRequest]) (*connect.Response[cleanroomv1.DeleteSnapshotResponse], error) {
	resp, err := s.service.DeleteSnapshot(ctx, req.Msg)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) ListExecutions(ctx context.Context, req *connect.Request[cleanroomv1.ListExecutionsRequest]) (*connect.Response[cleanroomv1.ListExecutionsResponse], error) {
	resp, err := s.service.ListExecutions(ctx, req.Msg)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) CreateExecution(ctx context.Context, req *connect.Request[cleanroomv1.CreateExecutionRequest]) (*connect.Response[cleanroomv1.CreateExecutionResponse], error) {
	createExecution := s.service.CreateExecution
	if req.Header().Get(internalWorkspaceCopyInHeader) == "1" {
		switch s.internalWorkspaceCopyInTrust {
		case internalWorkspaceCopyInTrustAll:
		case internalWorkspaceCopyInTrustLoopback:
			if req.Header().Get(internalWorkspaceCopyInTrustedHeader) != "1" {
				return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("internal workspace copy-in requests require a trusted control endpoint"))
			}
		default:
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("internal workspace copy-in requests require a trusted control endpoint"))
		}
		createExecution = s.service.CreateInternalWorkspaceCopyInExecution
	}
	resp, err := createExecution(ctx, req.Msg)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(resp), nil
}

func markLoopbackInternalWorkspaceCopyInRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del(internalWorkspaceCopyInTrustedHeader)
		if r.Header.Get(internalWorkspaceCopyInHeader) == "1" && isLoopbackRemoteAddr(r.RemoteAddr) {
			r.Header.Set(internalWorkspaceCopyInTrustedHeader, "1")
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackRemoteAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) AttachExecution(ctx context.Context, req *connect.Request[cleanroomv1.AttachExecutionRequest]) (*connect.Response[cleanroomv1.AttachExecutionResponse], error) {
	resp, err := s.service.AttachExecution(ctx, req.Msg)
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

func (s *Server) InspectExecution(ctx context.Context, req *connect.Request[cleanroomv1.InspectExecutionRequest]) (*connect.Response[cleanroomv1.InspectExecutionResponse], error) {
	resp, err := s.service.InspectExecution(ctx, req.Msg)
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

func (s *Server) WriteExecutionStdin(_ context.Context, req *connect.Request[cleanroomv1.WriteExecutionStdinRequest]) (*connect.Response[cleanroomv1.WriteExecutionStdinResponse], error) {
	if err := s.service.WriteExecutionStdin(req.Msg.GetSandboxId(), req.Msg.GetExecutionId(), req.Msg.GetData()); err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&cleanroomv1.WriteExecutionStdinResponse{}), nil
}

func (s *Server) CloseExecutionStdin(_ context.Context, req *connect.Request[cleanroomv1.CloseExecutionStdinRequest]) (*connect.Response[cleanroomv1.CloseExecutionStdinResponse], error) {
	if err := s.service.CloseExecutionStdin(req.Msg.GetSandboxId(), req.Msg.GetExecutionId()); err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&cleanroomv1.CloseExecutionStdinResponse{}), nil
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

type createSandboxEventStreamReporter struct {
	stream *connect.ServerStream[cleanroomv1.CreateSandboxEvent]
	cancel context.CancelFunc

	mu  sync.Mutex
	err error
}

func (r *createSandboxEventStreamReporter) Send(event *cleanroomv1.CreateSandboxEvent) error {
	if r == nil || event == nil {
		return nil
	}
	if event.GetOccurredAt() == nil {
		event.OccurredAt = timestamppb.Now()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	if err := r.stream.Send(event); err != nil {
		r.err = err
		if r.cancel != nil {
			r.cancel()
		}
		return err
	}
	return nil
}

func (r *createSandboxEventStreamReporter) Err() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *createSandboxEventStreamReporter) Message(phase cleanroomv1.CreateSandboxPhase, message string) {
	if strings.TrimSpace(message) == "" {
		return
	}
	_ = r.Send(&cleanroomv1.CreateSandboxEvent{
		Phase:   phase,
		Payload: &cleanroomv1.CreateSandboxEvent_Message{Message: message},
	})
}

func (r *createSandboxEventStreamReporter) Stdout(phase cleanroomv1.CreateSandboxPhase, chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	_ = r.Send(&cleanroomv1.CreateSandboxEvent{
		Phase:   phase,
		Payload: &cleanroomv1.CreateSandboxEvent_Stdout{Stdout: append([]byte(nil), chunk...)},
	})
}

func (r *createSandboxEventStreamReporter) Stderr(phase cleanroomv1.CreateSandboxPhase, chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	_ = r.Send(&cleanroomv1.CreateSandboxEvent{
		Phase:   phase,
		Payload: &cleanroomv1.CreateSandboxEvent_Stderr{Stderr: append([]byte(nil), chunk...)},
	})
}

func (r *createSandboxEventStreamReporter) Warning(phase cleanroomv1.CreateSandboxPhase, warning string) {
	if strings.TrimSpace(warning) == "" {
		return
	}
	_ = r.Send(&cleanroomv1.CreateSandboxEvent{
		Phase:   phase,
		Payload: &cleanroomv1.CreateSandboxEvent_Warning{Warning: warning},
	})
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
	case errors.Is(err, backend.ErrSandboxPathNotFound):
		code = connect.CodeNotFound
	case strings.Contains(message, "missing "), strings.Contains(message, "invalid"):
		code = connect.CodeInvalidArgument
	case strings.Contains(message, "unknown sandbox"), strings.Contains(message, "unknown cleanroom"), strings.Contains(message, "unknown execution"):
		code = connect.CodeNotFound
	case strings.Contains(message, "not ready"),
		strings.Contains(message, "not suspended"),
		strings.Contains(message, "does not support sandbox suspend"):
		code = connect.CodeFailedPrecondition
	}
	return connect.NewError(code, err)
}

func Serve(ctx context.Context, ep endpoint.Endpoint, handler http.Handler, logger *log.Logger, tlsOpts *TLSOptions) error {
	listener, cleanup, err := listen(ep, tlsOpts)
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
	if ep.Scheme == "https" {
		if err := http2.ConfigureServer(httpServer, nil); err != nil {
			return fmt.Errorf("configure HTTP/2 for TLS: %w", err)
		}
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

func listen(ep endpoint.Endpoint, tlsOpts *TLSOptions) (net.Listener, func() error, error) {
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

	if ep.Scheme == "https" {
		var opts tlsconfig.Options
		if tlsOpts != nil {
			opts = tlsconfig.Options{
				CertPath: tlsOpts.CertPath,
				KeyPath:  tlsOpts.KeyPath,
			}
		}
		tlsCfg, err := tlsconfig.ResolveServer(opts)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve server TLS config: %w", err)
		}
		if tlsCfg == nil {
			return nil, nil, errors.New("https listen endpoint requires TLS certificates (provide --tls-cert/--tls-key)")
		}
		addr := ep.Address
		for _, prefix := range []string{"https://", "http://"} {
			addr = strings.TrimPrefix(addr, prefix)
		}
		listener, err := tls.Listen("tcp", addr, tlsCfg)
		if err != nil {
			return nil, nil, fmt.Errorf("start TLS listener for %q: %w", addr, err)
		}
		return listener, nil, nil
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
