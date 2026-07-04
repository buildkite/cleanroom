package mediation

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

// AttributionHeader carries the guest-presented identity (typically the
// fork/generation hostname). It is logged for attribution and audit only;
// authorization is decided by which socket the request arrived on.
const AttributionHeader = "X-Cleanroom-Client"

// servicePathPrefix is the guest-visible URL namespace:
// http://gateway.cleanroom.internal:8170/services/<name>/<path...>
const servicePathPrefix = "/services/"

// Server serves a resolved mediation scope on a Unix socket.
type Server struct {
	scope map[string]ServiceDefinition
	log   io.Writer
	now   func() time.Time
	// lookupEnv is the credential source, injectable for tests.
	lookupEnv func(string) (string, bool)
}

// NewServer builds a server for a resolved scope. Credentials are read from
// the host environment at request time, never stored in the scope.
func NewServer(scope map[string]ServiceDefinition, log io.Writer) *Server {
	return &Server{
		scope:     scope,
		log:       log,
		now:       time.Now,
		lookupEnv: os.LookupEnv,
	}
}

// Serve listens on a Unix socket path and serves until the listener closes.
// The socket is the capability, so it is restricted to the owning user
// (0600): any local process that can connect gets credentialed access to the
// granted upstreams.
func (s *Server) Serve(socketPath string) error {
	if info, err := os.Lstat(socketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("refusing to replace non-socket path %s", socketPath)
		}
		if err := os.Remove(socketPath); err != nil {
			return fmt.Errorf("remove stale gateway socket %s: %w", socketPath, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat gateway socket %s: %w", socketPath, err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen on gateway socket %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		listener.Close()
		return fmt.Errorf("restrict gateway socket %s: %w", socketPath, err)
	}
	server := &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	return server.Serve(listener)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	client := strings.TrimSpace(r.Header.Get(AttributionHeader))
	if client == "" || containsControl(client) || len(client) > 128 {
		client = "-"
	}

	name, rest, ok := splitServicePath(r.URL.Path)
	definition, defined := s.scope[name]
	if !ok || !defined {
		s.logf("deny client=%s method=%s path=%s reason=unknown-service", client, r.Method, sanitizePath(r.URL.Path))
		http.Error(w, "unknown mediation service", http.StatusNotFound)
		return
	}
	if !isCanonicalPath(rest) {
		s.logf("deny client=%s service=%s method=%s path=%s reason=non-canonical-path", client, name, r.Method, sanitizePath(rest))
		http.Error(w, "non-canonical request path", http.StatusBadRequest)
		return
	}

	upstream, err := url.Parse(definition.Upstream)
	if err != nil {
		s.logf("error client=%s service=%s reason=bad-upstream", client, name)
		http.Error(w, "misconfigured upstream", http.StatusBadGateway)
		return
	}

	// Resolve the host-side credential once, before serving; a missing
	// credential fails closed rather than reaching the upstream unauthenticated.
	var credential string
	if definition.CredentialEnv != "" {
		secret, ok := s.lookupEnv(definition.CredentialEnv)
		if !ok || secret == "" {
			s.logf("error client=%s service=%s reason=missing-credential env=%s", client, name, definition.CredentialEnv)
			http.Error(w, "gateway credential unavailable", http.StatusBadGateway)
			return
		}
		credential = secret
	}

	s.logf("allow client=%s service=%s method=%s path=%s", client, name, r.Method, sanitizePath(rest))
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			pr.Out.URL.Path = joinUpstreamPath(upstream.Path, rest)
			pr.Out.URL.RawPath = ""
			pr.Out.Host = upstream.Host
			// Never forward guest-supplied credentials or attribution
			// upstream; inject the host-side credential when configured.
			pr.Out.Header.Del(AttributionHeader)
			pr.Out.Header.Del("Authorization")
			if definition.CredentialEnv != "" {
				header := definition.Header
				if header == "" {
					header = "Authorization"
				}
				pr.Out.Header.Set(header, definition.HeaderPrefix+credential)
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.logf("error client=%s service=%s reason=%s", client, name, sanitizePath(err.Error()))
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// isCanonicalPath rejects request paths whose cleaned form differs, which
// catches "..", ".", and "//" segments (including percent-decoded variants,
// since r.URL.Path is already decoded) before they reach the upstream with an
// injected credential.
func isCanonicalPath(rest string) bool {
	if rest == "" {
		return true
	}
	cleaned := path.Clean(rest)
	if strings.HasSuffix(rest, "/") && cleaned != "/" {
		cleaned += "/"
	}
	return cleaned == rest
}

func splitServicePath(requestPath string) (name, rest string, ok bool) {
	if !strings.HasPrefix(requestPath, servicePathPrefix) {
		return "", "", false
	}
	trimmed := strings.TrimPrefix(requestPath, servicePathPrefix)
	name, rest, _ = strings.Cut(trimmed, "/")
	if name == "" {
		return "", "", false
	}
	return name, "/" + rest, true
}

func joinUpstreamPath(base, rest string) string {
	base = strings.TrimSuffix(base, "/")
	if rest == "" || rest == "/" {
		if base == "" {
			return "/"
		}
		return base
	}
	return base + rest
}

func (s *Server) logf(format string, args ...any) {
	fmt.Fprintf(s.log, "%s gateway: %s\n", s.now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

func sanitizePath(value string) string {
	if len(value) > 256 {
		value = value[:256]
	}
	var b strings.Builder
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			b.WriteRune('?')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func containsControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
