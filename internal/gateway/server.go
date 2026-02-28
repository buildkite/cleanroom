package gateway

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/charmbracelet/log"
)

// DefaultPort is the default gateway listen port.
const DefaultPort = 8170

type contextKey int

const scopeContextKey contextKey = iota

// ScopeTokenHeader is the request header used for capability-token fallback
// identity when source-IP identity is unavailable (for example darwin NAT).
const ScopeTokenHeader = "X-Cleanroom-Scope-Token"

// ScopeFromContext retrieves the SandboxScope injected by identity middleware.
func ScopeFromContext(ctx context.Context) (*SandboxScope, bool) {
	scope, ok := ctx.Value(scopeContextKey).(*SandboxScope)
	return scope, ok
}

// ServerConfig configures a gateway server.
type ServerConfig struct {
	ListenAddr  string
	Registry    *Registry
	Credentials CredentialProvider
	Logger      *log.Logger
}

// Server is the host gateway HTTP server.
type Server struct {
	registry   *Registry
	logger     *log.Logger
	httpServer *http.Server

	mu      sync.Mutex
	started bool
	addr    string
}

// NewServer creates a gateway server. Call Start to begin listening.
func NewServer(cfg ServerConfig) *Server {
	addr := cfg.ListenAddr
	if addr == "" {
		addr = ":8170"
	}

	s := &Server{
		registry: cfg.Registry,
		logger:   cfg.Logger,
		addr:     addr,
	}

	mux := http.NewServeMux()
	mux.Handle("/git/", newGitHandler(cfg.Credentials, cfg.Logger))
	mux.HandleFunc("/registry/", stubHandler("registry"))
	mux.HandleFunc("/secrets/", stubHandler("secrets"))
	mux.HandleFunc("/meta/", stubHandler("meta"))

	s.httpServer = &http.Server{
		Handler: s.identityMiddleware(s.pathMiddleware(mux)),
	}

	return s
}

// Start begins listening for connections in the background.
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return errors.New("gateway server already started")
	}

	ln, err := net.Listen("tcp4", s.addr)
	if err != nil {
		return err
	}
	s.started = true
	s.addr = ln.Addr().String()

	go func() {
		if err := s.httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			if s.logger != nil {
				s.logger.Error("gateway server error", "error", err)
			}
		}
	}()

	if s.logger != nil {
		s.logger.Info("gateway server started", "addr", s.addr)
	}
	return nil
}

// Addr returns the listener address. Only meaningful after Start.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Stop gracefully shuts down the server.
func (s *Server) Stop(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

// identityMiddleware resolves sandbox identity and injects scope into the
// request context. It prefers source-IP identity and falls back to a scoped
// capability token header. Returns 403 when neither identity is valid.
func (s *Server) identityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceIP := extractSourceIP(r.RemoteAddr)
		if sourceIP != "" {
			if scope, ok := s.registry.Lookup(sourceIP); ok {
				ctx := context.WithValue(r.Context(), scopeContextKey, scope)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}

		scopeToken := strings.TrimSpace(r.Header.Get(ScopeTokenHeader))
		if scopeToken != "" {
			if scope, ok := s.registry.LookupScopeToken(scopeToken); ok {
				ctx := context.WithValue(r.Context(), scopeContextKey, scope)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}

		http.Error(w, "forbidden", http.StatusForbidden)
	})
}

// pathMiddleware validates and canonicalises the request path.
func (s *Server) pathMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		canonical, err := CanonicalisePath(r.URL.Path)
		if err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		r.URL.Path = canonical
		next.ServeHTTP(w, r)
	})
}

// extractSourceIP returns the IP portion of a RemoteAddr, handling both
// IPv4 ("10.1.1.2:43210") and IPv6-mapped IPv4 ("[::ffff:10.1.1.2]:43210").
func extractSourceIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return strings.TrimSpace(remoteAddr)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.String()
}

func stubHandler(service string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(service + " service not yet implemented"))
	}
}
