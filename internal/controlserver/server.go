package controlserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/buildkite/cleanroom/internal/controlapi"
	"github.com/buildkite/cleanroom/internal/controlservice"
	"github.com/buildkite/cleanroom/internal/endpoint"
	"github.com/charmbracelet/log"
)

type Server struct {
	service *controlservice.Service
	logger  *log.Logger
}

func New(service *controlservice.Service, logger *log.Logger) *Server {
	return &Server{service: service, logger: logger}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/exec", s.handleExec)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
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

func Serve(ctx context.Context, ep endpoint.Endpoint, handler http.Handler, logger *log.Logger) error {
	listener, err := listen(ep)
	if err != nil {
		return err
	}
	defer listener.Close()

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

func listen(ep endpoint.Endpoint) (net.Listener, error) {
	if ep.Scheme == "unix" {
		if err := os.MkdirAll(filepath.Dir(ep.Address), 0o755); err != nil {
			return nil, err
		}
		if err := os.Remove(ep.Address); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		listener, err := net.Listen("unix", ep.Address)
		if err != nil {
			return nil, err
		}
		if err := os.Chmod(ep.Address, 0o600); err != nil {
			_ = listener.Close()
			return nil, err
		}
		return listener, nil
	}

	if ep.Scheme == "http" || ep.Scheme == "https" {
		addr := ep.Address
		if len(addr) >= 7 && addr[:7] == "http://" {
			addr = addr[7:]
		}
		if len(addr) >= 8 && addr[:8] == "https://" {
			addr = addr[8:]
		}
		return net.Listen("tcp", addr)
	}

	return nil, fmt.Errorf("unsupported endpoint scheme %q", ep.Scheme)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, controlapi.ErrorResponse{Error: message})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
