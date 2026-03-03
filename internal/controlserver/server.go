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
	"time"

	"crypto/tls"

	"connectrpc.com/connect"
	"github.com/buildkite/cleanroom/internal/controlservice"
	"github.com/buildkite/cleanroom/internal/endpoint"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/gen/cleanroom/v1/cleanroomv1connect"
	"github.com/buildkite/cleanroom/internal/tlsconfig"
	"github.com/charmbracelet/log"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// TLSOptions holds explicit TLS paths for the server.
type TLSOptions struct {
	CertPath string
	KeyPath  string
}

type Server struct {
	service *controlservice.Service
	logger  *log.Logger
}

func New(service *controlservice.Service, logger *log.Logger) *Server {
	return &Server{service: service, logger: logger}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	sandboxPath, sandboxHandler := cleanroomv1connect.NewSandboxServiceHandler(s)
	executionPath, executionHandler := cleanroomv1connect.NewExecutionServiceHandler(s)
	mux.Handle(sandboxPath, sandboxHandler)
	mux.Handle(executionPath, executionHandler)

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
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

func (s *Server) DownloadSandboxFile(ctx context.Context, req *connect.Request[cleanroomv1.DownloadSandboxFileRequest]) (*connect.Response[cleanroomv1.DownloadSandboxFileResponse], error) {
	resp, err := s.service.DownloadSandboxFile(ctx, req.Msg)
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

func (s *Server) OpenInteractiveExecution(ctx context.Context, req *connect.Request[cleanroomv1.OpenInteractiveExecutionRequest]) (*connect.Response[cleanroomv1.OpenInteractiveExecutionResponse], error) {
	resp, err := s.service.OpenInteractiveExecution(ctx, req.Msg)
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
