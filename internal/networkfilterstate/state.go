package networkfilterstate

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
	"sync"
	"time"
)

const (
	DefaultListenAddress = "127.0.0.1:8171"
	DefaultBaseURL       = "http://" + DefaultListenAddress
	DefaultStateDir      = "/Library/Application Support/Cleanroom/network-filter"
	PolicyPath           = "/v1/policy"
	StatusPath           = "/v1/status"
	StatePath            = "/v1/state"
	HealthPath           = "/healthz"
)

type PolicySnapshot struct {
	Version           int               `json:"version"`
	UpdatedAt         string            `json:"updated_at"`
	DefaultAction     string            `json:"default_action"`
	TargetProcessPath string            `json:"target_process_path,omitempty"`
	Allow             []PolicyAllowRule `json:"allow"`
	GuestRules        []GuestRule       `json:"guest_rules,omitempty"`
	ProcessRules      []ProcessRule     `json:"process_rules,omitempty"`
}

type PolicyAllowRule struct {
	Host      string   `json:"host"`
	Ports     []int    `json:"ports"`
	RemoteIPs []string `json:"remote_ips,omitempty"`
}

type GuestRule struct {
	GuestIP       string            `json:"guest_ip"`
	DefaultAction string            `json:"default_action,omitempty"`
	AllowDNS      bool              `json:"allow_dns,omitempty"`
	Allow         []PolicyAllowRule `json:"allow"`
}

type ProcessRule struct {
	PID   int32             `json:"pid"`
	Allow []PolicyAllowRule `json:"allow"`
}

type StatusSnapshot struct {
	Version           int    `json:"version"`
	UpdatedAt         string `json:"updated_at,omitempty"`
	Available         bool   `json:"available"`
	Loaded            bool   `json:"loaded"`
	Enabled           bool   `json:"enabled"`
	Configured        bool   `json:"configured"`
	LastError         string `json:"last_error,omitempty"`
	ProviderStartedAt string `json:"provider_started_at,omitempty"`
	ProviderUpdatedAt string `json:"provider_updated_at,omitempty"`
	ProviderLastError string `json:"provider_last_error,omitempty"`
}

type Store struct {
	mu         sync.RWMutex
	stateDir   string
	policyPath string
	statusPath string
	policy     *PolicySnapshot
	status     map[string]any
}

func defaultStatusSnapshot() map[string]any {
	return map[string]any{
		"version":    1,
		"available":  false,
		"loaded":     false,
		"enabled":    false,
		"configured": false,
	}
}

func NewStore(stateDir string) (*Store, error) {
	dir := strings.TrimSpace(stateDir)
	if dir == "" {
		dir = DefaultStateDir
	}
	store := &Store{
		stateDir:   filepath.Clean(dir),
		policyPath: filepath.Join(dir, "policy.json"),
		statusPath: filepath.Join(dir, "status.json"),
		status:     defaultStatusSnapshot(),
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if raw, err := os.ReadFile(s.policyPath); err == nil {
		var snapshot PolicySnapshot
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			return fmt.Errorf("parse persisted network filter policy: %w", err)
		}
		s.policy = &snapshot
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read persisted network filter policy: %w", err)
	}

	if raw, err := os.ReadFile(s.statusPath); err == nil {
		payload := map[string]any{}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("parse persisted network filter status: %w", err)
		}
		if payload["version"] == nil {
			payload["version"] = 1
		}
		for key, value := range defaultStatusSnapshot() {
			if payload[key] == nil {
				payload[key] = value
			}
		}
		s.status = payload
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read persisted network filter status: %w", err)
	}

	return nil
}

func (s *Store) PutPolicy(snapshot PolicySnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if snapshot.Version == 0 {
		snapshot.Version = 1
	}
	if strings.TrimSpace(snapshot.UpdatedAt) == "" {
		snapshot.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if err := os.MkdirAll(s.stateDir, 0o755); err != nil {
		return fmt.Errorf("create network filter state directory: %w", err)
	}
	raw, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal network filter policy snapshot: %w", err)
	}
	raw = append(raw, '\n')
	if err := writeAtomically(s.policyPath, raw, 0o644); err != nil {
		return err
	}
	clone := snapshot
	s.policy = &clone
	return nil
}

func (s *Store) GetPolicy() (*PolicySnapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.policy == nil {
		return nil, false
	}
	clone := *s.policy
	return &clone, true
}

func (s *Store) PatchStatus(patch map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.stateDir, 0o755); err != nil {
		return fmt.Errorf("create network filter state directory: %w", err)
	}
	if s.status == nil {
		s.status = defaultStatusSnapshot()
	}
	for key, value := range defaultStatusSnapshot() {
		if s.status[key] == nil {
			s.status[key] = value
		}
	}
	for key, value := range patch {
		if value == nil {
			delete(s.status, key)
			continue
		}
		if _, ok := value.(map[string]any); ok {
			s.status[key] = value
			continue
		}
		s.status[key] = value
	}
	if _, ok := patch["updated_at"]; !ok {
		s.status["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	}
	raw, err := json.MarshalIndent(s.status, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal network filter status snapshot: %w", err)
	}
	raw = append(raw, '\n')
	if err := writeAtomically(s.statusPath, raw, 0o644); err != nil {
		return err
	}
	return nil
}

func (s *Store) GetStatusRaw() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make(map[string]any, len(s.status))
	for key, value := range s.status {
		out[key] = value
	}
	for key, value := range defaultStatusSnapshot() {
		if out[key] == nil {
			out[key] = value
		}
	}
	return out
}

func (s *Store) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.policy = nil
	s.status = defaultStatusSnapshot()

	if err := os.Remove(s.policyPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove network filter policy: %w", err)
	}
	if err := os.Remove(s.statusPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove network filter status: %w", err)
	}
	return nil
}

func writeAtomically(path string, data []byte, mode os.FileMode) error {
	tempPath := path + ".tmp"
	if err := os.WriteFile(tempPath, data, mode); err != nil {
		return fmt.Errorf("write temp file %s: %w", tempPath, err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("rename temp file into place %s: %w", path, err)
	}
	return nil
}

type Server struct {
	store *Store
	mux   *http.ServeMux
}

func NewServer(store *Store) *Server {
	mux := http.NewServeMux()
	server := &Server{store: store, mux: mux}
	mux.HandleFunc(HealthPath, server.handleHealth)
	mux.HandleFunc(PolicyPath, server.handlePolicy)
	mux.HandleFunc(StatusPath, server.handleStatus)
	mux.HandleFunc(StatePath, server.handleState)
	return server
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handlePolicy(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		snapshot, ok := s.store.GetPolicy()
		if !ok {
			http.Error(w, "policy not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, snapshot)
	case http.MethodPut:
		defer r.Body.Close()
		var snapshot PolicySnapshot
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&snapshot); err != nil {
			http.Error(w, "invalid policy payload", http.StatusBadRequest)
			return
		}
		if err := s.store.PutPolicy(snapshot); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.store.GetStatusRaw())
	case http.MethodPatch:
		defer r.Body.Close()
		patch := map[string]any{}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&patch); err != nil {
			http.Error(w, "invalid status payload", http.StatusBadRequest)
			return
		}
		if err := s.store.PatchStatus(patch); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, s.store.GetStatusRaw())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.store.Reset(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func writeJSON(w http.ResponseWriter, statusCode int, value any) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data = append(data, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_, _ = w.Write(data)
}

type Client struct {
	baseURL string
	client  *http.Client
}

func NewClient(baseURL string) *Client {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	return &Client{
		baseURL: base,
		client: &http.Client{
			Timeout: 2 * time.Second,
		},
	}
}

func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+HealthPath, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("network filter daemon health returned %s", resp.Status)
	}
	return nil
}

func (c *Client) PutPolicy(ctx context.Context, snapshot PolicySnapshot) error {
	return c.doJSON(ctx, http.MethodPut, PolicyPath, snapshot, nil)
}

func (c *Client) GetPolicy(ctx context.Context) (PolicySnapshot, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+PolicyPath, nil)
	if err != nil {
		return PolicySnapshot{}, false, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return PolicySnapshot{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return PolicySnapshot{}, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return PolicySnapshot{}, false, fmt.Errorf("network filter daemon policy returned %s", resp.Status)
	}
	var snapshot PolicySnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snapshot); err != nil {
		return PolicySnapshot{}, false, fmt.Errorf("decode network filter policy response: %w", err)
	}
	if snapshot.Version == 0 {
		snapshot.Version = 1
	}
	return snapshot, true, nil
}

func (c *Client) GetStatus(ctx context.Context) (StatusSnapshot, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+StatusPath, nil)
	if err != nil {
		return StatusSnapshot{}, false, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return StatusSnapshot{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return StatusSnapshot{}, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return StatusSnapshot{}, false, fmt.Errorf("network filter daemon status returned %s", resp.Status)
	}
	var snapshot StatusSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snapshot); err != nil {
		return StatusSnapshot{}, false, fmt.Errorf("decode network filter status response: %w", err)
	}
	if snapshot.Version == 0 {
		snapshot.Version = 1
	}
	return snapshot, true, nil
}

func (c *Client) PatchStatus(ctx context.Context, patch map[string]any) error {
	return c.doJSON(ctx, http.MethodPatch, StatusPath, patch, nil)
}

func (c *Client) Reset(ctx context.Context) error {
	return c.doJSON(ctx, http.MethodDelete, StatePath, nil, nil)
}

func (c *Client) doJSON(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(payload))
		if msg == "" {
			msg = resp.Status
		}
		return fmt.Errorf("network filter daemon %s %s failed: %s", method, path, msg)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode network filter daemon response: %w", err)
	}
	return nil
}

func ListenAndServe(ctx context.Context, listenAddress string, handler http.Handler) error {
	addr := strings.TrimSpace(listenAddress)
	if addr == "" {
		addr = DefaultListenAddress
	}
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		return fmt.Errorf("listen network filter daemon on %s: %w", addr, err)
	}
	defer ln.Close()

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 2 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Serve(ln)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		err := <-errCh
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
