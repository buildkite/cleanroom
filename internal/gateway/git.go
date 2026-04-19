package gateway

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/log"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	gitUploadPackService   = "git-upload-pack"
	gitReceivePackService  = "git-receive-pack"
	defaultUpstreamTimeout = 30 * time.Second

	reasonCodeHeader       = "X-Cleanroom-Reason-Code"
	reasonHostNotAllowed   = "host_not_allowed"
	reasonMethodNotAllowed = "method_not_allowed"
	reasonUpstreamError    = "upstream_error"
)

type gitHandler struct {
	credentials CredentialProvider
	mirrors     GitMirrorStore
	logger      *log.Logger
	client      *http.Client
}

func newGitHandler(creds CredentialProvider, logger *log.Logger) *gitHandler {
	return newGitHandlerWithMirrors(creds, nil, logger)
}

func newGitHandlerWithMirrors(creds CredentialProvider, mirrors GitMirrorStore, logger *log.Logger) *gitHandler {
	return &gitHandler{
		credentials: creds,
		mirrors:     mirrors,
		logger:      logger,
		client: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: defaultUpstreamTimeout}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: defaultUpstreamTimeout,
				// Disable keep-alives to avoid sharing any upstream connection pool
				// across sandbox identities.
				DisableKeepAlives: true,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// ServeHTTP handles /git/<upstream-host>/<owner>/<repo>[.git]/...
func (h *gitHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	scope, ok := ScopeFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	span := trace.SpanFromContext(r.Context())

	upstreamHost, repoPath, err := splitGitRequestPath(r.URL.Path)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	span.SetAttributes(
		attribute.String("cleanroom.gateway.target_host", upstreamHost),
		attribute.String("cleanroom.gateway.repo_path", repoPath),
	)

	if !scope.Policy.Allows(upstreamHost, 443) {
		setGatewayRequestDecision(r.Context(), "deny", reasonHostNotAllowed)
		span.SetAttributes(
			attribute.String("cleanroom.gateway.action", "deny"),
			attribute.String("cleanroom.reason_code", reasonHostNotAllowed),
		)
		span.SetStatus(codes.Error, "upstream host is not allowed by sandbox policy")
		h.auditLog(scope.SandboxID, upstreamHost, repoPath, "deny", reasonHostNotAllowed)
		writeReasonError(w, http.StatusForbidden, reasonHostNotAllowed, "upstream host is not allowed by sandbox policy")
		return
	}

	requestType, err := h.classifyRequest(r.Method, repoPath, r.URL.RawQuery)
	if err != nil {
		setGatewayRequestDecision(r.Context(), "deny", reasonMethodNotAllowed)
		span.RecordError(err)
		span.SetAttributes(
			attribute.String("cleanroom.gateway.action", "deny"),
			attribute.String("cleanroom.reason_code", reasonMethodNotAllowed),
		)
		span.SetStatus(codes.Error, err.Error())
		h.auditLog(scope.SandboxID, upstreamHost, repoPath, "deny", reasonMethodNotAllowed)
		writeReasonError(w, http.StatusForbidden, reasonMethodNotAllowed, err.Error())
		return
	}
	span.SetAttributes(attribute.String("cleanroom.gateway.request_type", requestType))

	remoteURL, err := canonicalUpstreamRemoteURL(upstreamHost, repoPath)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		writeReasonError(w, http.StatusBadRequest, reasonMethodNotAllowed, err.Error())
		return
	}

	if h.mirrors != nil {
		if err := h.serveFromMirror(w, r, remoteURL, upstreamHost, repoPath, requestType); err != nil {
			setGatewayRequestDecision(r.Context(), "deny", reasonUpstreamError)
			span.RecordError(err)
			span.SetAttributes(
				attribute.String("cleanroom.gateway.action", "deny"),
				attribute.String("cleanroom.reason_code", reasonUpstreamError),
			)
			span.SetStatus(codes.Error, err.Error())
			h.auditLog(scope.SandboxID, upstreamHost, repoPath, "deny", reasonUpstreamError)
			writeReasonError(w, http.StatusBadGateway, reasonUpstreamError, err.Error())
			return
		}
		setGatewayRequestDecision(r.Context(), "allow", "mirrored")
		span.SetAttributes(
			attribute.String("cleanroom.gateway.action", "allow"),
			attribute.String("cleanroom.reason_code", "mirrored"),
		)
		h.auditLog(scope.SandboxID, upstreamHost, repoPath, "allow", "mirrored")
		return
	}

	upstreamURL, err := upstreamRequestURL(upstreamHost, repoPath, r.URL.RawQuery)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, r.Body)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	copyGitUpstreamHeaders(upstreamReq.Header, r.Header)

	if h.credentials != nil {
		header, err := h.credentials.Resolve(r.Context(), remoteURL)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if header != "" {
			upstreamReq.Header.Set("Authorization", header)
		}
	}

	span.SetAttributes(
		attribute.String("cleanroom.gateway.action", "allow"),
		attribute.String("cleanroom.reason_code", "proxied"),
	)
	setGatewayRequestDecision(r.Context(), "allow", "proxied")
	h.auditLog(scope.SandboxID, upstreamHost, repoPath, "allow", "proxied")

	resp, err := h.client.Do(upstreamReq)
	if err != nil {
		setGatewayRequestDecision(r.Context(), "deny", reasonUpstreamError)
		span.RecordError(err)
		span.SetAttributes(attribute.String("cleanroom.reason_code", reasonUpstreamError))
		span.SetStatus(codes.Error, err.Error())
		writeReasonError(w, http.StatusBadGateway, reasonUpstreamError, "upstream error")
		return
	}
	defer resp.Body.Close()
	span.SetAttributes(attribute.Int("cleanroom.gateway.upstream_status_code", resp.StatusCode))

	for key, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func splitGitRequestPath(rawPath string) (string, string, error) {
	trimmed := strings.TrimPrefix(rawPath, "/git/")
	if trimmed == "" || trimmed == rawPath {
		return "", "", fmt.Errorf("missing upstream host")
	}
	slashIdx := strings.Index(trimmed, "/")
	if slashIdx <= 0 {
		return "", "", fmt.Errorf("missing repository path")
	}
	return trimmed[:slashIdx], trimmed[slashIdx:], nil
}

func canonicalUpstreamRemoteURL(upstreamHost, repoPath string) (string, error) {
	repositoryPath := repoPath
	switch {
	case strings.HasSuffix(repositoryPath, "/info/refs"):
		repositoryPath = strings.TrimSuffix(repositoryPath, "/info/refs")
	case strings.HasSuffix(repositoryPath, "/"+gitUploadPackService):
		repositoryPath = strings.TrimSuffix(repositoryPath, "/"+gitUploadPackService)
	case strings.HasSuffix(repositoryPath, "/"+gitReceivePackService):
		repositoryPath = strings.TrimSuffix(repositoryPath, "/"+gitReceivePackService)
	default:
		return "", fmt.Errorf("unsupported git request path %q", repoPath)
	}
	if strings.TrimSpace(repositoryPath) == "" || repositoryPath == "/" {
		return "", fmt.Errorf("missing repository path")
	}
	return "https://" + upstreamHost + repositoryPath, nil
}

func querySuffix(rawQuery string) string {
	if strings.TrimSpace(rawQuery) == "" {
		return ""
	}
	return "?" + rawQuery
}

func upstreamRequestURL(upstreamHost, repoPath, rawQuery string) (string, error) {
	if strings.TrimSpace(upstreamHost) == "" {
		return "", fmt.Errorf("missing upstream host")
	}
	if strings.TrimSpace(repoPath) == "" || !strings.HasPrefix(repoPath, "/") {
		return "", fmt.Errorf("missing repository path")
	}
	return (&url.URL{
		Scheme:   "https",
		Host:     upstreamHost,
		Path:     repoPath,
		RawQuery: rawQuery,
	}).String(), nil
}

func (h *gitHandler) serveFromMirror(w http.ResponseWriter, r *http.Request, remoteURL, upstreamHost, repoPath, requestType string) error {
	mirrorDir, err := h.mirrors.EnsureMirror(r.Context(), remoteURL)
	if err != nil {
		return fmt.Errorf("ensure mirror %s: %w", remoteURL, err)
	}
	switch requestType {
	case "info-refs":
		return serveMirrorInfoRefs(r, w, mirrorDir)
	case "upload-pack":
		return serveMirrorUploadPack(r, w, mirrorDir)
	default:
		return fmt.Errorf("unsupported git request type %q for %s%s", requestType, upstreamHost, repoPath)
	}
}

func serveMirrorInfoRefs(r *http.Request, w http.ResponseWriter, mirrorDir string) error {
	cmd := exec.CommandContext(r.Context(), "git", append(uploadPackConfigArgs(), "upload-pack", "--stateless-rpc", "--http-backend-info-refs", mirrorDir)...)
	cmd.Env = append(os.Environ(), gitProtocolEnv(r)...)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git upload-pack --http-backend-info-refs: %s: %w", strings.TrimSpace(stderr.String()), err)
	}

	w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
	w.Header().Set("Cache-Control", "no-cache")

	pktLine := "# service=git-upload-pack\n"
	pktHeader := fmt.Sprintf("%04x", len(pktLine)+4)
	if _, err := w.Write([]byte(pktHeader)); err != nil {
		return err
	}
	if _, err := w.Write([]byte(pktLine)); err != nil {
		return err
	}
	if _, err := w.Write([]byte("0000")); err != nil {
		return err
	}
	_, err := w.Write(stdout.Bytes())
	return err
}

func serveMirrorUploadPack(r *http.Request, w http.ResponseWriter, mirrorDir string) error {
	body, err := readUploadPackBody(r)
	if err != nil {
		return err
	}

	cmd := exec.CommandContext(r.Context(), "git", append(uploadPackConfigArgs(), "upload-pack", "--stateless-rpc", mirrorDir)...)
	cmd.Env = append(os.Environ(), gitProtocolEnv(r)...)
	cmd.Stdin = bytes.NewReader(body)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git upload-pack --stateless-rpc: %s: %w", strings.TrimSpace(stderr.String()), err)
	}

	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	_, err = w.Write(stdout.Bytes())
	return err
}

func gitProtocolEnv(r *http.Request) []string {
	proto := strings.TrimSpace(r.Header.Get("Git-Protocol"))
	if proto == "" {
		return nil
	}
	return []string{"GIT_PROTOCOL=" + proto}
}

func readUploadPackBody(r *http.Request) ([]byte, error) {
	reader := io.Reader(r.Body)
	if strings.Contains(strings.ToLower(r.Header.Get("Content-Encoding")), "gzip") {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, fmt.Errorf("decompress git-upload-pack request: %w", err)
		}
		defer gz.Close()
		reader = gz
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read git-upload-pack request: %w", err)
	}
	return body, nil
}

func uploadPackConfigArgs() []string {
	return []string{
		"-c", "uploadpack.allowFilter=true",
	}
}

var hopByHopHeaderNames = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

func copyGitUpstreamHeaders(dst, src http.Header) {
	if dst == nil || src == nil {
		return
	}

	connectionTokens := make(map[string]struct{})
	for _, value := range src.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			name := http.CanonicalHeaderKey(strings.TrimSpace(token))
			if name != "" {
				connectionTokens[name] = struct{}{}
			}
		}
	}

	for key, values := range src {
		canonicalKey := http.CanonicalHeaderKey(key)
		if _, skip := hopByHopHeaderNames[canonicalKey]; skip {
			continue
		}
		if _, skip := connectionTokens[canonicalKey]; skip {
			continue
		}
		switch canonicalKey {
		case ScopeTokenHeader, "Authorization", "Host":
			continue
		}
		for _, value := range values {
			dst.Add(canonicalKey, value)
		}
	}
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
		return "", fmt.Errorf("method_not_allowed: only git smart-HTTP operations are permitted")
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

func writeReasonError(w http.ResponseWriter, status int, reasonCode, message string) {
	w.Header().Set(reasonCodeHeader, reasonCode)
	http.Error(w, reasonCode+": "+message, status)
}
