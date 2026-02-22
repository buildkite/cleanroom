package gateway

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/charmbracelet/log"
)

const (
	gitUploadPackService   = "git-upload-pack"
	gitReceivePackService  = "git-receive-pack"
	defaultUpstreamTimeout = 30 * time.Second
)

type gitHandler struct {
	credentials CredentialProvider
	logger      *log.Logger
	client      *http.Client
}

func newGitHandler(creds CredentialProvider, logger *log.Logger) *gitHandler {
	return &gitHandler{
		credentials: creds,
		logger:      logger,
		client:      &http.Client{Timeout: defaultUpstreamTimeout},
	}
}

// ServeHTTP handles /git/<upstream-host>/<owner>/<repo>[.git]/...
func (h *gitHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	scope, ok := ScopeFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Parse: /git/<host>/<remainder>
	trimmed := strings.TrimPrefix(r.URL.Path, "/git/")
	if trimmed == "" || trimmed == r.URL.Path {
		http.Error(w, "bad request: missing upstream host", http.StatusBadRequest)
		return
	}

	slashIdx := strings.Index(trimmed, "/")
	if slashIdx <= 0 {
		http.Error(w, "bad request: missing repository path", http.StatusBadRequest)
		return
	}

	upstreamHost := trimmed[:slashIdx]
	repoPath := trimmed[slashIdx:] // includes leading /

	if !scope.Policy.Allows(upstreamHost, 443) {
		h.auditLog(scope.SandboxID, upstreamHost, repoPath, "deny", "host_not_allowed")
		http.Error(w, "host_not_allowed", http.StatusForbidden)
		return
	}

	if _, err := h.classifyRequest(r.Method, repoPath, r.URL.RawQuery); err != nil {
		h.auditLog(scope.SandboxID, upstreamHost, repoPath, "deny", "method_not_allowed")
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	upstreamURL := fmt.Sprintf("https://%s%s", upstreamHost, repoPath)
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, r.Body)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	for _, key := range []string{"Content-Type", "Accept", "Git-Protocol"} {
		if v := r.Header.Get(key); v != "" {
			upstreamReq.Header.Set(key, v)
		}
	}

	if h.credentials != nil {
		token, err := h.credentials.Resolve(r.Context(), upstreamHost)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if token != "" {
			upstreamReq.Header.Set("Authorization", "Bearer "+token)
		}
	}

	h.auditLog(scope.SandboxID, upstreamHost, repoPath, "allow", "proxied")

	resp, err := h.client.Do(upstreamReq)
	if err != nil {
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for key, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// classifyRequest determines the git operation and rejects disallowed operations.
func (h *gitHandler) classifyRequest(method, repoPath, query string) (string, error) {
	switch {
	case method == http.MethodGet && strings.HasSuffix(repoPath, "/info/refs"):
		service := queryParam(query, "service")
		if service == gitReceivePackService {
			return "", fmt.Errorf("method_not_allowed: git push (receive-pack) is denied")
		}
		return "info-refs", nil
	case method == http.MethodPost && strings.HasSuffix(repoPath, "/"+gitUploadPackService):
		return "upload-pack", nil
	case method == http.MethodPost && strings.HasSuffix(repoPath, "/"+gitReceivePackService):
		return "", fmt.Errorf("method_not_allowed: git push (receive-pack) is denied")
	default:
		return "other", nil
	}
}

func (h *gitHandler) auditLog(sandboxID, upstreamHost, repoPath, action, reason string) {
	if h.logger == nil {
		return
	}
	h.logger.Info("gateway git request",
		"sandbox_id", sandboxID,
		"service", "git",
		"upstream_host", upstreamHost,
		"repo_path", repoPath,
		"action", action,
		"reason_code", reason,
	)
}

func queryParam(rawQuery, key string) string {
	for _, part := range strings.Split(rawQuery, "&") {
		k, v, _ := strings.Cut(part, "=")
		if k == key {
			return v
		}
	}
	return ""
}
