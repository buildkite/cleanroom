package controlservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/authz"
	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/cachestore"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"github.com/buildkite/cleanroom/internal/snapshotstore"
)

func TestAuthzEnforcesExactSandboxAndExecutionOwnership(t *testing.T) {
	svc := &Service{
		Config: runtimeconfig.Config{DefaultBackend: "firecracker"},
		Backends: map[string]backend.Adapter{
			"firecracker": &stubAdapter{},
		},
	}
	aliceCtx := testAuthContext(t, "alice")
	bobCtx := testAuthContext(t, "bob")

	createResp, err := svc.CreateSandbox(aliceCtx, &cleanroomv1.CreateSandboxRequest{
		Backend: "firecracker",
		Policy:  testPolicy(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()

	if _, err := svc.GetSandbox(bobCtx, &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID}); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("GetSandbox as different principal error = %v, want authorization denied", err)
	}
	bobList, err := svc.ListSandboxes(bobCtx, &cleanroomv1.ListSandboxesRequest{})
	if err != nil {
		t.Fatalf("ListSandboxes as bob returned error: %v", err)
	}
	if got := len(bobList.GetSandboxes()); got != 0 {
		t.Fatalf("bob ListSandboxes returned %d sandboxes, want 0", got)
	}
	if _, err := svc.CreateExecution(bobCtx, &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"echo", "nope"},
	}); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("CreateExecution as different principal error = %v, want authorization denied", err)
	}

	execResp, err := svc.CreateExecution(aliceCtx, &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"echo", "ok"},
	})
	if err != nil {
		t.Fatalf("CreateExecution as owner returned error: %v", err)
	}
	executionID := execResp.GetExecution().GetExecutionId()
	if _, err := svc.GetExecution(bobCtx, &cleanroomv1.GetExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
	}); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("GetExecution as different principal error = %v, want authorization denied", err)
	}
}

func TestAuthzAuditLogIncludesStableReason(t *testing.T) {
	var logs bytes.Buffer
	logger, err := observability.NewLogger(&logs, "info", runtimeconfig.ObservabilityConfig{
		Logs: runtimeconfig.LogConfig{Format: "json"},
	})
	if err != nil {
		t.Fatalf("NewLogger returned error: %v", err)
	}
	svc := &Service{
		Config: runtimeconfig.Config{DefaultBackend: "firecracker"},
		Backends: map[string]backend.Adapter{
			"firecracker": &stubAdapter{},
		},
		Logger: logger,
	}
	aliceCtx := testAuthContext(t, "alice")
	bobCtx := testAuthContext(t, "bob")

	createResp, err := svc.CreateSandbox(aliceCtx, &cleanroomv1.CreateSandboxRequest{
		Backend: "firecracker",
		Policy:  testPolicy(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if _, err := svc.GetSandbox(bobCtx, &cleanroomv1.GetSandboxRequest{SandboxId: createResp.GetSandbox().GetSandboxId()}); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("GetSandbox as different principal error = %v, want authorization denied", err)
	}

	payload := findAuthorizationDeniedLog(t, logs.String())
	if got, want := payload[observability.LogFieldReasonCode], authz.ReasonOwnerMismatch; got != want {
		t.Fatalf("unexpected reason code: got %#v want %#v", got, want)
	}
	if got, want := payload["principal_id"], "oidc:test:bob"; got != want {
		t.Fatalf("unexpected principal_id: got %#v want %#v", got, want)
	}
	if _, ok := payload["token"]; ok {
		t.Fatalf("authorization audit log must not include token material: %#v", payload)
	}
}

func TestAuthzPropagatesGatewayScopeToBackendRequests(t *testing.T) {
	adapter := &stubAdapter{}
	svc := &Service{
		Config: runtimeconfig.Config{DefaultBackend: "firecracker"},
		Backends: map[string]backend.Adapter{
			"firecracker": adapter,
		},
	}
	ctx := testAuthContext(t, "alice")

	createResp, err := svc.CreateSandbox(ctx, &cleanroomv1.CreateSandboxRequest{
		Backend:            "firecracker",
		Policy:             testRepositoryPolicy(),
		RepositoryCheckout: testRepositoryCheckoutProto(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if got, want := adapter.provisionReq.GatewayScope.Owner.PrincipalID, "oidc:test:alice"; got != want {
		t.Fatalf("provision owner mismatch: got %q want %q", got, want)
	}
	if got, want := adapter.provisionReq.GatewayScope.Authorization.GitRepoPrefixes, []string{"github.com/buildkite/cleanroom"}; !slices.Equal(got, want) {
		t.Fatalf("provision git auth mismatch: got %v want %v", got, want)
	}
	if got, want := adapter.provisionReq.GatewayScope.Authorization.OCIRepoPrefixes, []string{"ghcr.io/buildkite/cleanroom-base/alpine"}; !slices.Equal(got, want) {
		t.Fatalf("provision OCI auth mismatch: got %v want %v", got, want)
	}

	execResp, err := svc.CreateExecution(ctx, &cleanroomv1.CreateExecutionRequest{
		SandboxId: createResp.GetSandbox().GetSandboxId(),
		Command:   []string{"true"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	waitExecutionDone(t, svc, createResp.GetSandbox().GetSandboxId(), execResp.GetExecution().GetExecutionId())
	if got, want := adapter.req.GatewayScope.Owner.PrincipalID, "oidc:test:alice"; got != want {
		t.Fatalf("execution owner mismatch: got %q want %q", got, want)
	}
	if got, want := adapter.req.GatewayScope.Authorization.GitRepoPrefixes, []string{"github.com/buildkite/cleanroom"}; !slices.Equal(got, want) {
		t.Fatalf("execution git auth mismatch: got %v want %v", got, want)
	}
}

func findAuthorizationDeniedLog(t *testing.T, raw string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			t.Fatalf("decode authorization log %q: %v", line, err)
		}
		if payload["msg"] == "authorization denied" {
			return payload
		}
	}
	t.Fatalf("authorization denied log not found in:\n%s", raw)
	return nil
}

func TestAuthzPropagatesRequestedRepositoryToBootstrapGatewayScope(t *testing.T) {
	var requests []backend.ExecutionRequest
	adapter := &stubAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
			requests = append(requests, req)
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    0,
				LaunchedVM:  true,
				PlanPath:    "/tmp/plan",
				RunDir:      "/tmp/run",
				Message:     "ok",
			}, nil
		},
	}
	svc := &Service{
		Config: runtimeconfig.Config{DefaultBackend: "firecracker"},
		Backends: map[string]backend.Adapter{
			"firecracker": adapter,
		},
	}
	ctx := testAuthContext(t, "alice")

	createResp, err := svc.CreateSandbox(ctx, &cleanroomv1.CreateSandboxRequest{
		Backend: "firecracker",
		Policy:  testRepositoryPolicy(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	execResp, err := svc.CreateExecution(ctx, &cleanroomv1.CreateExecutionRequest{
		SandboxId:          createResp.GetSandbox().GetSandboxId(),
		Command:            []string{"true"},
		RepositoryCheckout: testRepositoryCheckoutProto(),
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	waitExecutionDone(t, svc, createResp.GetSandbox().GetSandboxId(), execResp.GetExecution().GetExecutionId())

	if got := len(requests); got < 2 {
		t.Fatalf("expected bootstrap and execution requests, got %d", got)
	}
	bootstrapReq := requests[0]
	if got, want := bootstrapReq.GatewayScope.Owner.PrincipalID, "oidc:test:alice"; got != want {
		t.Fatalf("bootstrap owner mismatch: got %q want %q", got, want)
	}
	if got, want := bootstrapReq.GatewayScope.Authorization.GitRepoPrefixes, []string{"github.com/buildkite/cleanroom"}; !slices.Equal(got, want) {
		t.Fatalf("bootstrap git auth mismatch: got %v want %v", got, want)
	}
}

func TestAuthzStampsAndEnforcesSnapshotOwnership(t *testing.T) {
	store := newMemorySnapshotStore()
	svc := &Service{
		Config: runtimeconfig.Config{
			DefaultBackend: "firecracker",
			Backends: runtimeconfig.Backends{Firecracker: runtimeconfig.FirecrackerConfig{
				Snapshots: runtimeconfig.SnapshotConfig{Enabled: true, Driver: "file"},
			}},
		},
		Backends: map[string]backend.Adapter{
			"firecracker": &stubAdapter{},
		},
		SnapshotStore: store,
	}
	aliceCtx := testAuthContext(t, "alice")
	bobCtx := testAuthContext(t, "bob")

	createResp, err := svc.CreateSandbox(aliceCtx, &cleanroomv1.CreateSandboxRequest{
		Backend: "firecracker",
		Policy:  testPolicy(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()
	snapshotResp, err := svc.CreateSnapshot(aliceCtx, &cleanroomv1.CreateSnapshotRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}
	snapshotID := snapshotResp.GetSnapshot().GetSnapshotId()

	record, ok, err := store.Get(context.Background(), snapshotID)
	if err != nil {
		t.Fatalf("Get snapshot record returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected snapshot record")
	}
	if got, want := record.OwnerPrincipalID, "oidc:test:alice"; got != want {
		t.Fatalf("snapshot owner mismatch: got %q want %q", got, want)
	}

	if _, err := svc.GetSnapshot(bobCtx, &cleanroomv1.GetSnapshotRequest{SnapshotId: snapshotID}); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("GetSnapshot as different principal error = %v, want authorization denied", err)
	}
	bobList, err := svc.ListSnapshots(bobCtx, &cleanroomv1.ListSnapshotsRequest{})
	if err != nil {
		t.Fatalf("ListSnapshots as bob returned error: %v", err)
	}
	if got := len(bobList.GetSnapshots()); got != 0 {
		t.Fatalf("bob ListSnapshots returned %d snapshots, want 0", got)
	}
	if _, err := svc.CreateSandbox(bobCtx, &cleanroomv1.CreateSandboxRequest{
		Source: &cleanroomv1.CreateSandboxRequest_SnapshotId{SnapshotId: snapshotID},
	}); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("restore as different principal error = %v, want authorization denied", err)
	}
}

func TestAuthzPropagatesGatewayScopeToSnapshotProvision(t *testing.T) {
	store := newMemorySnapshotStore()
	record := snapshotstoreRecord("snap-owner")
	record.Repository = testRepositoryCheckoutProto()
	record.OwnerPrincipalID = "oidc:test:alice"
	record.OwnerScope = "scope:alice"
	if err := store.Create(context.Background(), record); err != nil {
		t.Fatalf("Create snapshot record returned error: %v", err)
	}
	adapter := &stubAdapter{}
	svc := &Service{
		Config: runtimeconfig.Config{
			DefaultBackend: "firecracker",
			Backends: runtimeconfig.Backends{Firecracker: runtimeconfig.FirecrackerConfig{
				Snapshots: runtimeconfig.SnapshotConfig{Enabled: true, Driver: "file"},
			}},
		},
		Backends: map[string]backend.Adapter{
			"firecracker": adapter,
		},
		SnapshotStore: store,
	}

	if _, err := svc.CreateSandbox(testAuthContext(t, "alice"), &cleanroomv1.CreateSandboxRequest{
		Source: &cleanroomv1.CreateSandboxRequest_SnapshotId{SnapshotId: record.SnapshotID},
	}); err != nil {
		t.Fatalf("CreateSandbox from snapshot returned error: %v", err)
	}

	if got, want := adapter.provisionFromSnapshotReq.GatewayScope.Owner.PrincipalID, "oidc:test:alice"; got != want {
		t.Fatalf("snapshot provision owner mismatch: got %q want %q", got, want)
	}
	if got, want := adapter.provisionFromSnapshotReq.GatewayScope.Authorization.GitRepoPrefixes, []string{"github.com/buildkite/cleanroom"}; !slices.Equal(got, want) {
		t.Fatalf("snapshot provision git auth mismatch: got %v want %v", got, want)
	}
}

func TestAuthzSnapshotRestoreEvaluatesSandboxCreateAgainstSnapshotBackend(t *testing.T) {
	store := newMemorySnapshotStore()
	record := snapshotstoreRecord("snap-darwin")
	record.Backend = "darwin-vz"
	record.StorageDriver = "apfs"
	record.StorageRef = "/tmp/snapshot.apfs"
	record.OwnerPrincipalID = "oidc:test:alice"
	record.OwnerScope = "scope:alice"
	if err := store.Create(context.Background(), record); err != nil {
		t.Fatalf("Create snapshot record returned error: %v", err)
	}
	adapter := &stubAdapter{}
	svc := &Service{
		Config: runtimeconfig.Config{
			DefaultBackend: "firecracker",
			Backends: runtimeconfig.Backends{DarwinVZ: runtimeconfig.DarwinVZConfig{
				Snapshots: runtimeconfig.SnapshotConfig{Enabled: true, Driver: "apfs"},
			}},
		},
		Backends: map[string]backend.Adapter{
			"darwin-vz": adapter,
		},
		SnapshotStore: store,
	}
	ctx := testAuthContextWithPolicy(t, "alice", authz.Policy{Bindings: []authz.Binding{{
		Name: "test",
		Principal: authz.PrincipalTemplate{
			ID:    "oidc:${token.issuer}:${token.subject}",
			Scope: "scope:${token.subject}",
		},
		Grants: []authz.Grant{
			{
				Name:      "firecracker-create-only",
				Actions:   []string{"sandbox.create"},
				Resources: []string{"sandbox"},
				Condition: `request.backend == "firecracker"`,
			},
			{
				Name:      "snapshot-restore",
				Actions:   []string{"snapshot.restore"},
				Resources: []string{"snapshot"},
			},
		},
	}}})

	_, err := svc.CreateSandbox(ctx, &cleanroomv1.CreateSandboxRequest{
		Source: &cleanroomv1.CreateSandboxRequest_SnapshotId{SnapshotId: "snap-darwin"},
	})
	if !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("CreateSandbox from darwin snapshot error = %v, want authorization denied", err)
	}
	if got := adapter.provisionFromSnapshotCalls; got != 0 {
		t.Fatalf("ProvisionSandboxFromSnapshot calls = %d, want 0", got)
	}
}

func TestAuthzCreateEvaluatesEffectiveRuntimeResources(t *testing.T) {
	adapter := &stubAdapter{}
	svc := &Service{
		Config: runtimeconfig.Config{
			DefaultBackend: "firecracker",
			Backends: runtimeconfig.Backends{Firecracker: runtimeconfig.FirecrackerConfig{
				VCPUs: 8,
			}},
		},
		Backends: map[string]backend.Adapter{
			"firecracker": adapter,
		},
	}
	ctx := testAuthContextWithPolicy(t, "alice", authz.Policy{Bindings: []authz.Binding{{
		Name: "test",
		Principal: authz.PrincipalTemplate{
			ID:    "oidc:${token.issuer}:${token.subject}",
			Scope: "scope:${token.subject}",
		},
		Grants: []authz.Grant{{
			Name:      "small-sandboxes",
			Actions:   []string{"sandbox.create"},
			Resources: []string{"sandbox"},
			Condition: `request.policy.resources.vcpus <= 4`,
		}},
	}}})

	_, err := svc.CreateSandbox(ctx, &cleanroomv1.CreateSandboxRequest{
		Backend: "firecracker",
		Policy:  testPolicy(),
	})
	if !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("CreateSandbox error = %v, want authorization denied", err)
	}
	if got := adapter.provisionCalls; got != 0 {
		t.Fatalf("ProvisionSandbox calls = %d, want 0", got)
	}
}

func TestAuthzCreateExposesRepositoryChangeset(t *testing.T) {
	repoDir := initControlServiceGitRepo(t)
	baseCommit := headControlServiceCommit(t, repoDir)
	repository := &repositorycheckout.Checkout{
		RemoteURL:      "https://github.com/buildkite/cleanroom.git",
		CommitSHA:      baseCommit,
		DestinationDir: "/workspace",
	}
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("hello from changeset\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	changeset, err := repositorychangeset.BuildFromWorkingTree(repoDir, repository)
	if err != nil {
		t.Fatalf("BuildFromWorkingTree returned error: %v", err)
	}
	if changeset == nil {
		t.Fatal("expected repository changeset")
	}

	adapter := &stubAdapter{}
	svc := newTestService(adapter)
	ctx := testAuthContextWithPolicy(t, "alice", authz.Policy{Bindings: []authz.Binding{{
		Name: "test",
		Principal: authz.PrincipalTemplate{
			ID:    "oidc:${token.issuer}:${token.subject}",
			Scope: "scope:${token.subject}",
		},
		Grants: []authz.Grant{{
			Name:      "clean-checkouts-only",
			Actions:   []string{"sandbox.create"},
			Resources: []string{"sandbox"},
			Condition: `request.repository.changeset.present == false`,
		}},
	}}})

	_, err = svc.CreateSandbox(ctx, &cleanroomv1.CreateSandboxRequest{
		Backend:             "firecracker",
		Policy:              testRepositoryPolicy(),
		RepositoryCheckout:  repository.ToProto(),
		RepositoryChangeset: changeset.ToProto(),
	})
	if !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("CreateSandbox error = %v, want authorization denied", err)
	}
	if got := adapter.provisionCalls; got != 0 {
		t.Fatalf("ProvisionSandbox calls = %d, want 0", got)
	}
}

func TestAuthzCreateExecutionExposesRequestedRepository(t *testing.T) {
	adapter := &stubAdapter{}
	mirrors := &stubRepositoryMirrorStore{}
	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors
	ctx := testAuthContextWithPolicy(t, "alice", authz.Policy{Bindings: []authz.Binding{{
		Name: "test",
		Principal: authz.PrincipalTemplate{
			ID:    "oidc:${token.issuer}:${token.subject}",
			Scope: "scope:${token.subject}",
		},
		Grants: []authz.Grant{
			{
				Name:      "sandbox-create",
				Actions:   []string{"sandbox.create"},
				Resources: []string{"sandbox"},
			},
			{
				Name:      "known-execution-repo",
				Actions:   []string{"execution.create"},
				Resources: []string{"execution"},
				Condition: `request.repository.remote_url == "https://github.com/buildkite/cleanroom.git"`,
			},
		},
	}}})

	createResp, err := svc.CreateSandbox(ctx, &cleanroomv1.CreateSandboxRequest{
		Backend: "firecracker",
		Policy:  testPolicy(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	repository := testRepositoryCheckoutProto()
	repository.RemoteUrl = "https://github.com/buildkite/private.git"
	_, err = svc.CreateExecution(ctx, &cleanroomv1.CreateExecutionRequest{
		SandboxId:          createResp.GetSandbox().GetSandboxId(),
		Command:            []string{"sh", "-lc", "pwd"},
		RepositoryCheckout: repository,
	})
	if !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("CreateExecution error = %v, want authorization denied", err)
	}
	if got := mirrors.calls; got != 0 {
		t.Fatalf("repository mirror calls = %d, want 0", got)
	}
}

func TestAuthzDeniesOwnerlessSnapshotWhenAuthenticated(t *testing.T) {
	store := newMemorySnapshotStore()
	if err := store.Create(context.Background(), snapshotstoreRecord("snap-ownerless")); err != nil {
		t.Fatalf("Create snapshot record returned error: %v", err)
	}
	svc := &Service{SnapshotStore: store}

	_, err := svc.GetSnapshot(testAuthContext(t, "alice"), &cleanroomv1.GetSnapshotRequest{SnapshotId: "snap-ownerless"})
	if !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("GetSnapshot ownerless error = %v, want authorization denied", err)
	}
}

func TestAuthzStageCacheLookupRequiresMatchingOwner(t *testing.T) {
	cacheStore := newMemoryCacheStore()
	svc := &Service{CacheStore: cacheStore}
	compiled, err := policy.FromProto(testRepositoryPolicy())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(testRepositoryCheckoutProto())
	cacheKey := workspaceStageCacheKey("firecracker", "runtime-base:test", compiled.Hash, repository, nil)
	record := cachestore.Record{
		CacheKey:          cacheKey,
		Stage:             workspaceStageName,
		OwnerPrincipalID:  "oidc:test:alice",
		OwnerScope:        "scope:alice",
		State:             cacheStateReady,
		BackingSnapshotID: "snapshot-alice",
		Backend:           "firecracker",
		PolicyHash:        compiled.Hash,
		Policy:            compiled.ToProto(),
		Repository:        cloneRepositoryCheckout(normalizeRepositoryCheckoutForComparison(repository)).ToProto(),
		ParentCacheKey:    "runtime-base:test",
		StorageDriver:     "file",
		StorageRef:        "/tmp/alice.ext4",
		ProducerVersion:   workspaceStageProducerVersion,
	}
	if err := cacheStore.Create(context.Background(), record); err != nil {
		t.Fatalf("Create owned cache record returned error: %v", err)
	}
	ownerlessRecord := record
	ownerlessRecord.OwnerPrincipalID = ""
	ownerlessRecord.OwnerScope = ""
	ownerlessRecord.BackingSnapshotID = "snapshot-ownerless"
	ownerlessRecord.StorageRef = "/tmp/ownerless.ext4"
	if err := cacheStore.Create(context.Background(), ownerlessRecord); err != nil {
		t.Fatalf("Create ownerless cache record returned error: %v", err)
	}

	got, ok, reason, err := svc.lookupWorkspaceStageCache(testAuthContext(t, "alice"), "firecracker", compiled, "runtime-base:test", repository, nil)
	if err != nil {
		t.Fatalf("lookup as owner returned error: %v", err)
	}
	if !ok {
		t.Fatalf("lookup as owner missed with reason %q", reason)
	}
	if got.OwnerPrincipalID != "oidc:test:alice" {
		t.Fatalf("lookup as owner returned owner %q", got.OwnerPrincipalID)
	}

	if _, ok, reason, err := svc.lookupWorkspaceStageCache(testAuthContext(t, "bob"), "firecracker", compiled, "runtime-base:test", repository, nil); err != nil {
		t.Fatalf("lookup as other principal returned error: %v", err)
	} else if ok || reason != observability.CacheLookupReasonRecordNotFound {
		t.Fatalf("lookup as other principal = ok %v reason %q, want owner miss", ok, reason)
	}

	got, ok, reason, err = svc.lookupWorkspaceStageCache(context.Background(), "firecracker", compiled, "runtime-base:test", repository, nil)
	if err != nil {
		t.Fatalf("ownerless lookup returned error: %v", err)
	}
	if !ok {
		t.Fatalf("ownerless lookup missed with reason %q", reason)
	}
	if got.OwnerPrincipalID != "" || got.StorageRef != "/tmp/ownerless.ext4" {
		t.Fatalf("ownerless lookup returned %#v", got)
	}
}

func TestAuthzStageCachePublishStampsOwner(t *testing.T) {
	adapter := &stubAdapter{}
	svc := newTestServiceWithSnapshotStore(adapter, newMemorySnapshotStore())
	cacheStore := newMemoryCacheStore()
	svc.CacheStore = cacheStore
	compiled, err := policy.FromProto(testRepositoryPolicy())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(testRepositoryCheckoutProto())

	svc.maybePublishWorkspaceStageCache(
		testAuthContext(t, "alice"),
		adapter,
		"sandbox-1",
		"firecracker",
		compiled,
		backend.FirecrackerConfig{},
		"runtime-base:test",
		repository,
		nil,
		nil,
	)

	records, err := cacheStore.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if got, want := len(records), 1; got != want {
		t.Fatalf("unexpected cache record count: got %d want %d", got, want)
	}
	if got, want := records[0].OwnerPrincipalID, "oidc:test:alice"; got != want {
		t.Fatalf("cache owner mismatch: got %q want %q", got, want)
	}
	if got, want := records[0].OwnerScope, "scope:alice"; got != want {
		t.Fatalf("cache owner scope mismatch: got %q want %q", got, want)
	}
}

func testAuthContext(t *testing.T, subject string) context.Context {
	t.Helper()
	return testAuthContextWithPolicy(t, subject, authz.Policy{Bindings: []authz.Binding{{
		Name: "test",
		Principal: authz.PrincipalTemplate{
			ID:    "oidc:${token.issuer}:${token.subject}",
			Scope: "scope:${token.subject}",
		},
		Grants: []authz.Grant{{
			Name: "all",
			Actions: []string{
				"sandbox.create",
				"sandbox.get",
				"sandbox.list",
				"sandbox.terminate",
				"execution.create",
				"execution.get",
				"execution.list",
				"execution.attach",
				"execution.inspect",
				"execution.cancel",
				"snapshot.create",
				"snapshot.get",
				"snapshot.list",
				"snapshot.delete",
				"snapshot.restore",
			},
			Resources: []string{"sandbox", "execution", "snapshot"},
		}},
	}}})
}

func testAuthContextWithPolicy(t *testing.T, subject string, spec authz.Policy) context.Context {
	t.Helper()
	policy, err := authz.CompilePolicy(spec)
	if err != nil {
		t.Fatalf("CompilePolicy returned error: %v", err)
	}
	bound, err := policy.Bind(authz.ValidatedToken{
		IssuerName: "test",
		Issuer:     "https://issuer.example.test",
		Subject:    subject,
		Claims:     map[string]any{"sub": subject},
		ExpiresAt:  time.Now().Add(time.Hour),
		IssuedAt:   time.Now(),
	})
	if err != nil {
		t.Fatalf("Bind returned error: %v", err)
	}
	return authz.ContextWithBoundPrincipal(context.Background(), bound)
}

func snapshotstoreRecord(snapshotID string) snapshotstore.Record {
	return snapshotstore.Record{
		SnapshotID:      snapshotID,
		SourceSandboxID: "sandbox-source",
		Backend:         "firecracker",
		PolicyHash:      "policy-hash",
		Policy:          testPolicy(),
		StorageDriver:   "file",
		StorageRef:      "/tmp/snapshot.ext4",
		CreatedAt:       time.Now().UTC(),
	}
}
