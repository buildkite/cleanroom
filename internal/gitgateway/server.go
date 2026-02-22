package gitgateway

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/buildkite/cleanroom/internal/policy"
)

type Config struct {
	ListenHost string
	GitPolicy  *policy.GitPolicy

	// PolicyForScope resolves policy by scope identifier for requests routed via
	// /git/<scope>/<host>/<owner>/<repo>.git/...
	PolicyForScope func(string) (*policy.GitPolicy, bool)
	MirrorRoot     string
	Client         *http.Client

	// UpstreamScheme defaults to https and is exposed for tests.
	UpstreamScheme string
}

type Server struct {
	listener net.Listener
	httpSrv  *http.Server
}

func (s *Server) Close() error {
	if s == nil || s.httpSrv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.httpSrv.Shutdown(ctx)
}

func Start(ctx context.Context, cfg Config) (*Server, error) {
	if cfg.GitPolicy == nil || !cfg.GitPolicy.Enabled {
		return nil, nil
	}
	listenHost := strings.TrimSpace(cfg.ListenHost)
	if listenHost == "" {
		return nil, fmt.Errorf("git gateway listen host is required")
	}

	ln, err := net.Listen("tcp", net.JoinHostPort(listenHost, "0"))
	if err != nil {
		return nil, fmt.Errorf("listen git gateway on %s: %w", listenHost, err)
	}
	return StartOnListener(ctx, ln, cfg)
}

func StartOnListener(ctx context.Context, ln net.Listener, cfg Config) (*Server, error) {
	if cfg.GitPolicy == nil || !cfg.GitPolicy.Enabled {
		return nil, nil
	}
	if ln == nil {
		return nil, fmt.Errorf("git gateway listener is required")
	}

	upstreamScheme := strings.ToLower(strings.TrimSpace(cfg.UpstreamScheme))
	if upstreamScheme == "" {
		upstreamScheme = "https"
	}
	if upstreamScheme != "https" && upstreamScheme != "http" {
		return nil, fmt.Errorf("unsupported upstream scheme %q", upstreamScheme)
	}

	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}

	handler := newHandler(cfg.GitPolicy, cfg.PolicyForScope, strings.TrimSpace(cfg.MirrorRoot), upstreamScheme, client)

	httpSrv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	server := &Server{
		listener: ln,
		httpSrv:  httpSrv,
	}

	go func() {
		_ = httpSrv.Serve(ln)
	}()

	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()

	return server, nil
}

type gatewayHandler struct {
	policy         *policy.GitPolicy
	policyForScope func(string) (*policy.GitPolicy, bool)
	mirrorRoot     string
	upstreamScheme string
	client         *http.Client
}

func newHandler(policy *policy.GitPolicy, policyForScope func(string) (*policy.GitPolicy, bool), mirrorRoot string, upstreamScheme string, client *http.Client) *gatewayHandler {
	return &gatewayHandler{policy: policy, policyForScope: policyForScope, mirrorRoot: mirrorRoot, upstreamScheme: upstreamScheme, client: client}
}

func (h *gatewayHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	req, ok := parseGatewayPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	requestPolicy := h.policy
	if req.Scope != "" {
		if h.policyForScope == nil {
			http.Error(w, "scope_not_allowed", http.StatusForbidden)
			return
		}
		resolved, found := h.policyForScope(req.Scope)
		if !found || resolved == nil || !resolved.Enabled {
			http.Error(w, "scope_not_allowed", http.StatusForbidden)
			return
		}
		requestPolicy = resolved
	}
	if requestPolicy == nil || !requestPolicy.Enabled {
		http.Error(w, "scope_not_allowed", http.StatusForbidden)
		return
	}
	if req.Op == "info/refs" {
		if r.Method != http.MethodGet || strings.TrimSpace(r.URL.Query().Get("service")) != "git-upload-pack" {
			http.Error(w, "unsupported git service", http.StatusBadRequest)
			return
		}
	} else {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
	}

	if !contains(requestPolicy.AllowedHosts, req.Host) {
		http.Error(w, "host_not_allowed", http.StatusForbidden)
		return
	}
	if len(requestPolicy.AllowedRepos) > 0 && !contains(requestPolicy.AllowedRepos, req.RepoKey()) {
		http.Error(w, "repository_not_allowed", http.StatusForbidden)
		return
	}

	if requestPolicy.Source == "host_mirror" {
		if mirrorPath, ok := h.resolveMirror(req); ok {
			h.serveLocalUploadPack(r.Context(), w, r, req, mirrorPath)
			return
		}
	}
	h.proxyUpstream(w, r, req)
}

func (h *gatewayHandler) resolveMirror(req gatewayRequest) (string, bool) {
	if h.mirrorRoot == "" {
		return "", false
	}
	path := filepath.Join(h.mirrorRoot, req.Host, req.Owner, req.Repo+".git")
	st, err := os.Stat(path)
	if err != nil || !st.IsDir() {
		return "", false
	}
	return path, true
}

func (h *gatewayHandler) serveLocalUploadPack(ctx context.Context, w http.ResponseWriter, r *http.Request, req gatewayRequest, repoPath string) {
	switch req.Op {
	case "info/refs":
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, pktLine("# service=git-upload-pack\n"))
		_, _ = io.WriteString(w, "0000")

		cmd := exec.CommandContext(ctx, "git", "upload-pack", "--stateless-rpc", "--advertise-refs", repoPath)
		cmd.Stdout = w
		if err := cmd.Run(); err != nil {
			return
		}
	case "git-upload-pack":
		cmd := exec.CommandContext(ctx, "git", "upload-pack", "--stateless-rpc", repoPath)
		cmd.Stdin = r.Body
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.WriteHeader(http.StatusOK)
		cmd.Stdout = w
		if err := cmd.Run(); err != nil {
			return
		}
	default:
		http.NotFound(w, r)
	}
}

func (h *gatewayHandler) proxyUpstream(w http.ResponseWriter, r *http.Request, req gatewayRequest) {
	upstream := fmt.Sprintf("%s://%s/%s/%s.git/%s", h.upstreamScheme, req.Host, req.Owner, req.Repo, req.Op)
	upstreamURL, err := url.Parse(upstream)
	if err != nil {
		http.Error(w, "runtime_launch_failed", http.StatusBadGateway)
		return
	}
	upstreamURL.RawQuery = r.URL.RawQuery

	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL.String(), r.Body)
	if err != nil {
		http.Error(w, "runtime_launch_failed", http.StatusBadGateway)
		return
	}
	copyProxyHeaders(upstreamReq.Header, r.Header)

	resp, err := h.client.Do(upstreamReq)
	if err != nil {
		http.Error(w, "runtime_launch_failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func copyProxyHeaders(dst, src http.Header) {
	for _, key := range []string{"Accept", "Content-Type", "Content-Encoding", "Git-Protocol", "Authorization", "User-Agent"} {
		values := src.Values(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

type gatewayRequest struct {
	Scope string
	Host  string
	Owner string
	Repo  string
	Op    string
}

func (r gatewayRequest) RepoKey() string {
	return r.Owner + "/" + r.Repo
}

func parseGatewayPath(path string) (gatewayRequest, bool) {
	trimmed := strings.Trim(strings.TrimSpace(path), "/")
	if trimmed == "" {
		return gatewayRequest{}, false
	}
	parts := strings.Split(trimmed, "/")
	if len(parts) < 5 || len(parts) > 7 {
		return gatewayRequest{}, false
	}
	if parts[0] != "git" {
		return gatewayRequest{}, false
	}

	scope := ""
	hostIdx := 1
	ownerIdx := 2
	repoIdx := 3
	opStartIdx := 4

	if len(parts) == 6 && !strings.HasSuffix(strings.TrimSpace(parts[3]), ".git") {
		scope = strings.TrimSpace(parts[1])
		hostIdx = 2
		ownerIdx = 3
		repoIdx = 4
		opStartIdx = 5
	}
	if len(parts) == 7 {
		scope = strings.TrimSpace(parts[1])
		hostIdx = 2
		ownerIdx = 3
		repoIdx = 4
		opStartIdx = 5
	}
	if scope != "" {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			return gatewayRequest{}, false
		}
	}

	repoWithSuffix := strings.TrimSpace(parts[repoIdx])
	if !strings.HasSuffix(repoWithSuffix, ".git") {
		return gatewayRequest{}, false
	}
	repo := strings.TrimSuffix(repoWithSuffix, ".git")
	if repo == "" {
		return gatewayRequest{}, false
	}
	op := strings.TrimSpace(parts[opStartIdx])
	if opStartIdx+1 < len(parts) {
		op = strings.TrimSpace(parts[opStartIdx]) + "/" + strings.TrimSpace(parts[opStartIdx+1])
	}
	if op != "info/refs" && op != "git-upload-pack" {
		return gatewayRequest{}, false
	}

	return gatewayRequest{
		Scope: scope,
		Host:  strings.ToLower(strings.TrimSpace(parts[hostIdx])),
		Owner: strings.ToLower(strings.TrimSpace(parts[ownerIdx])),
		Repo:  strings.ToLower(strings.TrimSpace(repo)),
		Op:    op,
	}, true
}

func contains(values []string, needle string) bool {
	needle = strings.ToLower(strings.TrimSpace(needle))
	for _, value := range values {
		if strings.ToLower(strings.TrimSpace(value)) == needle {
			return true
		}
	}
	return false
}

func pktLine(s string) string {
	return fmt.Sprintf("%04x%s", len(s)+4, s)
}
