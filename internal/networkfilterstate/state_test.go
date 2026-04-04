package networkfilterstate

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStorePutPolicyAndPatchStatusPersist(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore returned error: %v", err)
	}

	if err := store.PutPolicy(PolicySnapshot{
		Version:       1,
		UpdatedAt:     "2026-03-15T10:00:00Z",
		DefaultAction: "deny",
		Allow: []PolicyAllowRule{
			{Host: "github.com", Ports: []int{443}},
		},
		GuestRules: []GuestRule{
			{
				GuestIP:       "10.233.0.2",
				DefaultAction: "deny",
				AllowDNS:      true,
				Allow: []PolicyAllowRule{
					{Host: "github.com", Ports: []int{443}, RemoteIPs: []string{"140.82.113.4"}},
				},
			},
		},
	}); err != nil {
		t.Fatalf("PutPolicy returned error: %v", err)
	}
	if err := store.PatchStatus(map[string]any{
		"updated_at":          "2026-03-15T10:00:01Z",
		"enabled":             true,
		"provider_started_at": "2026-03-15T10:00:01Z",
	}); err != nil {
		t.Fatalf("PatchStatus returned error: %v", err)
	}

	persisted, err := NewStore(store.stateDir)
	if err != nil {
		t.Fatalf("NewStore(reload) returned error: %v", err)
	}

	policy, ok := persisted.GetPolicy()
	if !ok {
		t.Fatal("expected persisted policy to be available")
	}
	if got, want := policy.DefaultAction, "deny"; got != want {
		t.Fatalf("default action = %q, want %q", got, want)
	}
	if len(policy.GuestRules) != 1 {
		t.Fatalf("expected 1 guest rule, got %#v", policy.GuestRules)
	}
	if got, want := policy.GuestRules[0].GuestIP, "10.233.0.2"; got != want {
		t.Fatalf("guest_ip = %q, want %q", got, want)
	}
	status := persisted.GetStatusRaw()
	if got, ok := status["enabled"].(bool); !ok || !got {
		t.Fatalf("expected enabled=true in persisted status, got %#v", status["enabled"])
	}
}

func TestServerPolicyStatusAndResetEndpoints(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore returned error: %v", err)
	}
	server := NewServer(store)

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	httpServer := &http.Server{Handler: server}
	defer httpServer.Close()
	go func() {
		_ = httpServer.Serve(listener)
	}()

	client := NewClient("http://" + listener.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Health(ctx); err != nil {
		t.Fatalf("Health returned error: %v", err)
	}
	if err := client.PutPolicy(ctx, PolicySnapshot{
		Version:       1,
		UpdatedAt:     "2026-03-15T10:00:00Z",
		DefaultAction: "deny",
		Allow: []PolicyAllowRule{
			{Host: "github.com", Ports: []int{443}},
		},
		GuestRules: []GuestRule{
			{
				GuestIP:       "10.233.0.2",
				DefaultAction: "deny",
				Allow: []PolicyAllowRule{
					{Host: "github.com", Ports: []int{443}, RemoteIPs: []string{"140.82.113.4"}},
				},
			},
		},
	}); err != nil {
		t.Fatalf("PutPolicy returned error: %v", err)
	}
	policy, found, err := client.GetPolicy(ctx)
	if err != nil {
		t.Fatalf("GetPolicy returned error: %v", err)
	}
	if !found {
		t.Fatal("expected policy to be found")
	}
	if got, want := policy.DefaultAction, "deny"; got != want {
		t.Fatalf("default action = %q, want %q", got, want)
	}
	if len(policy.GuestRules) != 1 {
		t.Fatalf("expected 1 guest rule, got %#v", policy.GuestRules)
	}
	if err := client.PatchStatus(ctx, map[string]any{
		"enabled":             true,
		"provider_started_at": "2026-03-15T10:00:01Z",
	}); err != nil {
		t.Fatalf("PatchStatus returned error: %v", err)
	}

	status, found, err := client.GetStatus(ctx)
	if err != nil {
		t.Fatalf("GetStatus returned error: %v", err)
	}
	if !found {
		t.Fatal("expected status to be found")
	}
	if !status.Enabled {
		t.Fatalf("expected enabled=true, got %#v", status)
	}
	if status.UpdatedAt == "" {
		t.Fatal("expected daemon to stamp updated_at on status patches")
	}
	if got, want := strings.TrimSpace(status.ProviderStartedAt), "2026-03-15T10:00:01Z"; got != want {
		t.Fatalf("provider_started_at = %q, want %q", got, want)
	}

	if err := client.Reset(ctx); err != nil {
		t.Fatalf("Reset returned error: %v", err)
	}
	status, found, err = client.GetStatus(ctx)
	if err != nil {
		t.Fatalf("GetStatus after reset returned error: %v", err)
	}
	if !found {
		t.Fatal("expected status endpoint to remain available after reset")
	}
	if status.Enabled {
		t.Fatalf("expected enabled=false after reset, got %#v", status)
	}

	if _, err := os.Stat(filepath.Join(store.stateDir, "policy.json")); !os.IsNotExist(err) {
		t.Fatalf("expected policy.json to be removed after reset, stat err=%v", err)
	}
}
