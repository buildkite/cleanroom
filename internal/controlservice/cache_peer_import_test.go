package controlservice

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/buildkite/cleanroom/internal/cachestore"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/gen/cleanroom/v1/cleanroomv1connect"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"github.com/buildkite/cleanroom/internal/volumestore"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestCreateSandboxImportsDependencyStageCacheFromPeer(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CLEANROOM_CACHE_PEER_TOKEN", "receiver-token")

	adapter := &stubAdapter{}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"go.mod": "module example.com/test\n\ngo 1.26.2\n",
		"go.sum": "example.com/test v0.0.0 h1:abc123\n",
	})
	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors
	svc.Config.Backends.Firecracker.Snapshots.Driver = "zfs"

	compiled, err := policy.FromProto(testRepositoryDependencyPolicy())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(repositoryCheckout)
	workspaceKey := workspaceStageCacheKey("firecracker", "runtime-base:test", compiled.Hash, repository, nil)
	dependencyPlan, ok := dependencyStagePlanForRepository(compiled, repository)
	if !ok {
		t.Fatal("expected dependency stage plan")
	}
	dependencyPlan, ok, err = svc.finalizeDependencyStagePlan(context.Background(), compiled, repository, nil, nil, "firecracker", workspaceKey, "runtime-base:test", dependencyPlan)
	if err != nil {
		t.Fatalf("finalizeDependencyStagePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected finalized dependency stage plan")
	}

	cacheStore := requireMemoryCacheStore(t, svc)
	insertImportedPeerParentRecord(t, cacheStore, importedPeerParentRecordOptions{
		Stage:           workspaceStageName,
		CacheKey:        workspaceKey,
		StorageRef:      "tank/local/workspace@base",
		SnapshotGUID:    "workspace-guid",
		ProducerVersion: workspaceStageProducerVersion,
	})

	importMetadata := encodeZFSMetadataForTest(t, "tank/local/imports/dependency@base", "dependency-guid", "workspace-guid")
	transfer := &stubCachePeerTransferDriver{
		describeSnapshots: map[string]volumestore.SnapshotDescription{
			"tank/local/workspace@base": {
				SnapshotRef:  "tank/local/workspace@base",
				StorageRef:   "tank/local/workspace@base",
				SnapshotGUID: "workspace-guid",
			},
		},
		importSnapshot: volumestore.Snapshot{
			StorageRef:     "tank/local/imports/dependency@base",
			DriverMetadata: importMetadata,
		},
	}
	svc.CachePeerTransferDriver = transfer

	peer := newTestCachePeerImportServer(t, &cleanroomv1.CachePeerCandidate{
		TransferToken:         "peer-token",
		Stage:                 dependencyStageName,
		CacheKey:              dependencyPlan.CacheKey,
		ParentStage:           workspaceStageName,
		ParentCacheKey:        workspaceKey,
		Backend:               "firecracker",
		StorageDriver:         "zfs",
		Architecture:          runtime.GOARCH,
		ProducerVersion:       dependencyStageProducerVersion,
		PolicyHash:            compiled.Hash,
		ZfsSnapshotGuid:       "dependency-guid",
		ZfsParentSnapshotGuid: "workspace-guid",
		BackingSnapshotId:     "peer-dependency-snapshot",
		StorageRef:            "tank/peer/dependency@base",
		EstimatedBytes:        128,
		ExpiresAt:             timestamppb.New(time.Now().Add(time.Hour)),
	})
	svc.Config.Cache.Peers = []runtimeconfig.CachePeerConfig{{URL: peer.URL, TokenEnv: "CLEANROOM_CACHE_PEER_TOKEN"}}

	resp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryDependencyPolicy(),
		RepositoryCheckout: repositoryCheckout,
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if got, want := resp.GetSourceKind(), "dependency stage cache"; got != want {
		t.Fatalf("unexpected source kind: got %q want %q", got, want)
	}
	if got, want := adapter.provisionFromSnapshotCalls, 1; got != want {
		t.Fatalf("expected restored imported dependency cache once, got %d want %d", got, want)
	}
	if got, want := adapter.provisionCalls, 0; got != want {
		t.Fatalf("expected peer import restore to skip cold provision, got %d want %d", got, want)
	}
	if got, want := adapter.runCalls, 0; got != want {
		t.Fatalf("expected peer import restore to skip bootstrap commands, got %d want %d", got, want)
	}
	if got, want := len(transfer.describeRequests), 1; got != want {
		t.Fatalf("expected one parent describe request, got %d", got)
	}
	if got, want := transfer.describeRequests[0].SnapshotRef, "tank/local/workspace@base"; got != want {
		t.Fatalf("unexpected describe ref: got %q want %q", got, want)
	}
	requireCachePeerImportRequest(t, transfer, "tank/local/workspace@base", "workspace-guid", "dependency-guid", "zfs-stream")
	requireCachePeerLookup(t, peer, &cleanroomv1.LookupCachePeerRequest{
		Stage:                 dependencyStageName,
		CacheKey:              dependencyPlan.CacheKey,
		Backend:               "firecracker",
		StorageDriver:         "zfs",
		Architecture:          runtime.GOARCH,
		ProducerVersion:       dependencyStageProducerVersion,
		PolicyHash:            compiled.Hash,
		ParentStage:           workspaceStageName,
		ParentCacheKey:        workspaceKey,
		ParentZfsSnapshotGuid: "workspace-guid",
	})

	record, found, err := cacheStore.GetReady(context.Background(), dependencyStageName, dependencyPlan.CacheKey)
	if err != nil {
		t.Fatalf("GetReady returned error: %v", err)
	}
	if !found {
		t.Fatal("expected imported dependency cache record")
	}
	if !record.ImportedFromPeer {
		t.Fatal("expected dependency cache record to be marked as peer-imported")
	}
	if got, want := record.StorageRef, "tank/local/imports/dependency@base"; got != want {
		t.Fatalf("unexpected imported storage ref: got %q want %q", got, want)
	}
	if got, want := record.ParentCacheKey, workspaceKey; got != want {
		t.Fatalf("unexpected parent cache key: got %q want %q", got, want)
	}
	if got, want := record.DependencyKeyFilesDigest, dependencyPlan.KeyFilesDigest; got != want {
		t.Fatalf("unexpected dependency key digest: got %q want %q", got, want)
	}
}

func TestImportDependencyStageCacheFromPeerCleansUpInvalidSnapshot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CLEANROOM_CACHE_PEER_TOKEN", "receiver-token")

	adapter := &stubAdapter{}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"go.mod": "module example.com/test\n\ngo 1.26.2\n",
		"go.sum": "example.com/test v0.0.0 h1:abc123\n",
	})
	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors
	svc.Config.Backends.Firecracker.Snapshots.Driver = "zfs"

	compiled, err := policy.FromProto(testRepositoryDependencyPolicy())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(repositoryCheckout)
	workspaceKey := workspaceStageCacheKey("firecracker", "runtime-base:test", compiled.Hash, repository, nil)
	dependencyPlan, ok := dependencyStagePlanForRepository(compiled, repository)
	if !ok {
		t.Fatal("expected dependency stage plan")
	}
	dependencyPlan, ok, err = svc.finalizeDependencyStagePlan(context.Background(), compiled, repository, nil, nil, "firecracker", workspaceKey, "runtime-base:test", dependencyPlan)
	if err != nil {
		t.Fatalf("finalizeDependencyStagePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected finalized dependency stage plan")
	}

	cacheStore := requireMemoryCacheStore(t, svc)
	insertImportedPeerParentRecord(t, cacheStore, importedPeerParentRecordOptions{
		Stage:           workspaceStageName,
		CacheKey:        workspaceKey,
		StorageRef:      "tank/local/workspace@base",
		SnapshotGUID:    "workspace-guid",
		ProducerVersion: workspaceStageProducerVersion,
	})

	wrongMetadata := encodeZFSMetadataForTest(t, "tank/local/imports/dependency@base", "wrong-dependency-guid", "workspace-guid")
	transfer := &stubCachePeerTransferDriver{
		describeSnapshots: map[string]volumestore.SnapshotDescription{
			"tank/local/workspace@base": {
				SnapshotRef:  "tank/local/workspace@base",
				StorageRef:   "tank/local/workspace@base",
				SnapshotGUID: "workspace-guid",
			},
		},
		importSnapshot: volumestore.Snapshot{
			StorageRef:     "tank/local/imports/dependency@base",
			DriverMetadata: wrongMetadata,
		},
	}
	svc.CachePeerTransferDriver = transfer

	peer := newTestCachePeerImportServer(t, &cleanroomv1.CachePeerCandidate{
		TransferToken:         "peer-token",
		Stage:                 dependencyStageName,
		CacheKey:              dependencyPlan.CacheKey,
		ParentStage:           workspaceStageName,
		ParentCacheKey:        workspaceKey,
		Backend:               "firecracker",
		StorageDriver:         "zfs",
		Architecture:          runtime.GOARCH,
		ProducerVersion:       dependencyStageProducerVersion,
		PolicyHash:            compiled.Hash,
		ZfsSnapshotGuid:       "dependency-guid",
		ZfsParentSnapshotGuid: "workspace-guid",
		ExpiresAt:             timestamppb.New(time.Now().Add(time.Hour)),
	})
	svc.Config.Cache.Peers = []runtimeconfig.CachePeerConfig{{URL: peer.URL, TokenEnv: "CLEANROOM_CACHE_PEER_TOKEN"}}

	record, imported, err := svc.importDependencyStageCacheFromPeers(
		context.Background(),
		adapter,
		"firecracker",
		compiled,
		runtimeconfig.MergeBackendConfig(svc.Config, "firecracker", 0),
		repository,
		nil,
		dependencyPlan,
	)
	if err == nil {
		t.Fatal("expected invalid peer snapshot metadata to fail")
	}
	if imported {
		t.Fatalf("expected invalid import to report imported=false, got record %#v", record)
	}
	if got, want := adapter.deleteSnapshotCalls, 1; got != want {
		t.Fatalf("expected invalid import to clean up imported snapshot, got delete calls %d want %d", got, want)
	}
	if got, want := adapter.deleteSnapshotReq.StorageRef, "tank/local/imports/dependency@base"; got != want {
		t.Fatalf("unexpected cleanup storage ref: got %q want %q", got, want)
	}
	if got, want := adapter.deleteSnapshotReq.FirecrackerConfig.Snapshots.Driver, "zfs"; got != want {
		t.Fatalf("unexpected cleanup snapshot driver: got %q want %q", got, want)
	}
	if _, found, err := cacheStore.GetReady(context.Background(), dependencyStageName, dependencyPlan.CacheKey); err != nil {
		t.Fatalf("GetReady returned error: %v", err)
	} else if found {
		t.Fatal("expected invalid import to leave no dependency cache metadata")
	}
}

func TestImportDependencyStageCacheFromPeersContinuesAfterExportFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CLEANROOM_CACHE_PEER_TOKEN", "receiver-token")

	adapter := &stubAdapter{}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"go.mod": "module example.com/test\n\ngo 1.26.2\n",
		"go.sum": "example.com/test v0.0.0 h1:abc123\n",
	})
	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors
	svc.Config.Backends.Firecracker.Snapshots.Driver = "zfs"

	compiled, err := policy.FromProto(testRepositoryDependencyPolicy())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(repositoryCheckout)
	workspaceKey := workspaceStageCacheKey("firecracker", "runtime-base:test", compiled.Hash, repository, nil)
	dependencyPlan, ok := dependencyStagePlanForRepository(compiled, repository)
	if !ok {
		t.Fatal("expected dependency stage plan")
	}
	dependencyPlan, ok, err = svc.finalizeDependencyStagePlan(context.Background(), compiled, repository, nil, nil, "firecracker", workspaceKey, "runtime-base:test", dependencyPlan)
	if err != nil {
		t.Fatalf("finalizeDependencyStagePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected finalized dependency stage plan")
	}

	cacheStore := requireMemoryCacheStore(t, svc)
	insertImportedPeerParentRecord(t, cacheStore, importedPeerParentRecordOptions{
		Stage:           workspaceStageName,
		CacheKey:        workspaceKey,
		StorageRef:      "tank/local/workspace@base",
		SnapshotGUID:    "workspace-guid",
		ProducerVersion: workspaceStageProducerVersion,
	})

	importMetadata := encodeZFSMetadataForTest(t, "tank/local/imports/dependency@base", "dependency-guid", "workspace-guid")
	transfer := &stubCachePeerTransferDriver{
		describeSnapshots: map[string]volumestore.SnapshotDescription{
			"tank/local/workspace@base": {
				SnapshotRef:  "tank/local/workspace@base",
				StorageRef:   "tank/local/workspace@base",
				SnapshotGUID: "workspace-guid",
			},
		},
		importSnapshot: volumestore.Snapshot{
			StorageRef:     "tank/local/imports/dependency@base",
			DriverMetadata: importMetadata,
		},
	}
	svc.CachePeerTransferDriver = transfer

	firstCandidate := cachePeerImportTestCandidate(dependencyPlan.CacheKey, workspaceKey, compiled.Hash, "bad-token")
	badPeer := newTestCachePeerImportServerWithExport(t, firstCandidate, http.StatusInternalServerError, "")
	secondCandidate := cachePeerImportTestCandidate(dependencyPlan.CacheKey, workspaceKey, compiled.Hash, "good-token")
	goodPeer := newTestCachePeerImportServerWithExport(t, secondCandidate, http.StatusOK, "good-zfs-stream")
	svc.Config.Cache.Peers = []runtimeconfig.CachePeerConfig{
		{URL: badPeer.URL, TokenEnv: "CLEANROOM_CACHE_PEER_TOKEN"},
		{URL: goodPeer.URL, TokenEnv: "CLEANROOM_CACHE_PEER_TOKEN"},
	}

	record, imported, err := svc.importDependencyStageCacheFromPeers(
		context.Background(),
		adapter,
		"firecracker",
		compiled,
		runtimeconfig.MergeBackendConfig(svc.Config, "firecracker", 0),
		repository,
		nil,
		dependencyPlan,
	)
	if err != nil {
		t.Fatalf("importDependencyStageCacheFromPeers returned error: %v", err)
	}
	if !imported {
		t.Fatal("expected import to continue to second peer")
	}
	if got, want := record.StorageRef, "tank/local/imports/dependency@base"; got != want {
		t.Fatalf("unexpected imported storage ref: got %q want %q", got, want)
	}
	if got, want := badPeer.exportRequests, 1; got != want {
		t.Fatalf("expected first peer export to be attempted once, got %d", got)
	}
	if got, want := goodPeer.exportRequests, 1; got != want {
		t.Fatalf("expected second peer export to be attempted once, got %d", got)
	}
	requireCachePeerImportRequest(t, transfer, "tank/local/workspace@base", "workspace-guid", "dependency-guid", "good-zfs-stream")
	if _, found, err := cacheStore.GetReady(context.Background(), dependencyStageName, dependencyPlan.CacheKey); err != nil {
		t.Fatalf("GetReady returned error: %v", err)
	} else if !found {
		t.Fatal("expected successful second-peer import to publish cache metadata")
	}
}

func TestCreateSandboxImportsServicesStageCacheFromPeer(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CLEANROOM_CACHE_PEER_TOKEN", "receiver-token")

	adapter := &stubAdapter{}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"go.mod":             "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":             "example.com/test v0.0.0 h1:abc123\n",
		"docker-compose.yml": "services:\n  postgres:\n    image: postgres:17\n",
	})
	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors
	svc.Config.Backends.Firecracker.Snapshots.Driver = "zfs"

	compiled, err := policy.FromProto(testRepositoryDependencyAndServicesPolicy())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(repositoryCheckout)
	workspaceKey := workspaceStageCacheKey("firecracker", "runtime-base:test", compiled.Hash, repository, nil)
	dependencyPlan, ok := dependencyStagePlanForRepository(compiled, repository)
	if !ok {
		t.Fatal("expected dependency stage plan")
	}
	dependencyPlan, ok, err = svc.finalizeDependencyStagePlan(context.Background(), compiled, repository, nil, nil, "firecracker", workspaceKey, "runtime-base:test", dependencyPlan)
	if err != nil {
		t.Fatalf("finalizeDependencyStagePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected finalized dependency stage plan")
	}
	servicesPlan, ok := servicesStagePlanForRepository(compiled, repository)
	if !ok {
		t.Fatal("expected services stage plan")
	}
	servicesPlan, ok, err = svc.finalizeServicesStagePlan(context.Background(), compiled, repository, nil, nil, dependencyPlan.CacheKey, "runtime-base:test", servicesPlan)
	if err != nil {
		t.Fatalf("finalizeServicesStagePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected finalized services stage plan")
	}

	cacheStore := requireMemoryCacheStore(t, svc)
	insertImportedPeerParentRecord(t, cacheStore, importedPeerParentRecordOptions{
		Stage:              dependencyStageName,
		CacheKey:           dependencyPlan.CacheKey,
		ParentCacheKey:     workspaceKey,
		StorageRef:         "tank/local/dependency@base",
		SnapshotGUID:       "dependency-guid",
		ParentSnapshotGUID: "workspace-guid",
		ProducerVersion:    dependencyStageProducerVersion,
		ReuseMode:          dependencyStageReuseExact,
	})

	importMetadata := encodeZFSMetadataForTest(t, "tank/local/imports/services@base", "services-guid", "dependency-guid")
	transfer := &stubCachePeerTransferDriver{
		describeSnapshots: map[string]volumestore.SnapshotDescription{
			"tank/local/dependency@base": {
				SnapshotRef:  "tank/local/dependency@base",
				StorageRef:   "tank/local/dependency@base",
				SnapshotGUID: "dependency-guid",
			},
		},
		importSnapshot: volumestore.Snapshot{
			StorageRef:     "tank/local/imports/services@base",
			DriverMetadata: importMetadata,
		},
	}
	svc.CachePeerTransferDriver = transfer

	peer := newTestCachePeerImportServer(t, &cleanroomv1.CachePeerCandidate{
		TransferToken:         "peer-token",
		Stage:                 servicesStageName,
		CacheKey:              servicesPlan.CacheKey,
		ParentStage:           dependencyStageName,
		ParentCacheKey:        dependencyPlan.CacheKey,
		Backend:               "firecracker",
		StorageDriver:         "zfs",
		Architecture:          runtime.GOARCH,
		ProducerVersion:       servicesStageProducerVersion,
		PolicyHash:            compiled.Hash,
		ZfsSnapshotGuid:       "services-guid",
		ZfsParentSnapshotGuid: "dependency-guid",
		BackingSnapshotId:     "peer-services-snapshot",
		StorageRef:            "tank/peer/services@base",
		EstimatedBytes:        128,
		ExpiresAt:             timestamppb.New(time.Now().Add(time.Hour)),
	})
	svc.Config.Cache.Peers = []runtimeconfig.CachePeerConfig{{URL: peer.URL, TokenEnv: "CLEANROOM_CACHE_PEER_TOKEN"}}

	resp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryDependencyAndServicesPolicy(),
		RepositoryCheckout: repositoryCheckout,
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if got, want := resp.GetSourceKind(), "services stage cache"; got != want {
		t.Fatalf("unexpected source kind: got %q want %q", got, want)
	}
	if got, want := adapter.provisionFromSnapshotCalls, 1; got != want {
		t.Fatalf("expected restored imported services cache once, got %d want %d", got, want)
	}
	if got, want := adapter.provisionCalls, 0; got != want {
		t.Fatalf("expected peer import restore to skip cold provision, got %d want %d", got, want)
	}
	if got, want := adapter.runCalls, 0; got != want {
		t.Fatalf("expected peer import restore to skip bootstrap commands, got %d want %d", got, want)
	}
	requireCachePeerImportRequest(t, transfer, "tank/local/dependency@base", "dependency-guid", "services-guid", "zfs-stream")
	requireCachePeerLookup(t, peer, &cleanroomv1.LookupCachePeerRequest{
		Stage:                 servicesStageName,
		CacheKey:              servicesPlan.CacheKey,
		Backend:               "firecracker",
		StorageDriver:         "zfs",
		Architecture:          runtime.GOARCH,
		ProducerVersion:       servicesStageProducerVersion,
		PolicyHash:            compiled.Hash,
		ParentStage:           dependencyStageName,
		ParentCacheKey:        dependencyPlan.CacheKey,
		ParentZfsSnapshotGuid: "dependency-guid",
	})

	record, found, err := cacheStore.GetReady(context.Background(), servicesStageName, servicesPlan.CacheKey)
	if err != nil {
		t.Fatalf("GetReady returned error: %v", err)
	}
	if !found {
		t.Fatal("expected imported services cache record")
	}
	if !record.ImportedFromPeer {
		t.Fatal("expected services cache record to be marked as peer-imported")
	}
	if got, want := record.StorageRef, "tank/local/imports/services@base"; got != want {
		t.Fatalf("unexpected imported storage ref: got %q want %q", got, want)
	}
	if got, want := record.ParentCacheKey, dependencyPlan.CacheKey; got != want {
		t.Fatalf("unexpected parent cache key: got %q want %q", got, want)
	}
}

func TestCreateSandboxImportsServicesStageAfterDependencyPeerImport(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CLEANROOM_CACHE_PEER_TOKEN", "receiver-token")

	adapter := &stubAdapter{}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"go.mod":             "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":             "example.com/test v0.0.0 h1:abc123\n",
		"docker-compose.yml": "services:\n  postgres:\n    image: postgres:17\n",
	})
	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors
	svc.Config.Backends.Firecracker.Snapshots.Driver = "zfs"

	compiled, err := policy.FromProto(testRepositoryDependencyAndServicesPolicy())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(repositoryCheckout)
	workspaceKey := workspaceStageCacheKey("firecracker", "runtime-base:test", compiled.Hash, repository, nil)
	dependencyPlan, ok := dependencyStagePlanForRepository(compiled, repository)
	if !ok {
		t.Fatal("expected dependency stage plan")
	}
	dependencyPlan, ok, err = svc.finalizeDependencyStagePlan(context.Background(), compiled, repository, nil, nil, "firecracker", workspaceKey, "runtime-base:test", dependencyPlan)
	if err != nil {
		t.Fatalf("finalizeDependencyStagePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected finalized dependency stage plan")
	}
	servicesPlan, ok := servicesStagePlanForRepository(compiled, repository)
	if !ok {
		t.Fatal("expected services stage plan")
	}
	servicesPlan, ok, err = svc.finalizeServicesStagePlan(context.Background(), compiled, repository, nil, nil, dependencyPlan.CacheKey, "runtime-base:test", servicesPlan)
	if err != nil {
		t.Fatalf("finalizeServicesStagePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected finalized services stage plan")
	}

	cacheStore := requireMemoryCacheStore(t, svc)
	insertImportedPeerParentRecord(t, cacheStore, importedPeerParentRecordOptions{
		Stage:           workspaceStageName,
		CacheKey:        workspaceKey,
		StorageRef:      "tank/local/workspace@base",
		SnapshotGUID:    "workspace-guid",
		ProducerVersion: workspaceStageProducerVersion,
	})

	dependencyMetadata := encodeZFSMetadataForTest(t, "tank/local/imports/dependency@base", "dependency-guid", "workspace-guid")
	servicesMetadata := encodeZFSMetadataForTest(t, "tank/local/imports/services@base", "services-guid", "dependency-guid")
	transfer := &stubCachePeerTransferDriver{
		describeSnapshots: map[string]volumestore.SnapshotDescription{
			"tank/local/workspace@base": {
				SnapshotRef:  "tank/local/workspace@base",
				StorageRef:   "tank/local/workspace@base",
				SnapshotGUID: "workspace-guid",
			},
			"tank/local/imports/dependency@base": {
				SnapshotRef:  "tank/local/imports/dependency@base",
				StorageRef:   "tank/local/imports/dependency@base",
				SnapshotGUID: "dependency-guid",
			},
		},
		importSnapshots: map[string]volumestore.Snapshot{
			"dependency-guid": {
				StorageRef:     "tank/local/imports/dependency@base",
				DriverMetadata: dependencyMetadata,
			},
			"services-guid": {
				StorageRef:     "tank/local/imports/services@base",
				DriverMetadata: servicesMetadata,
			},
		},
	}
	svc.CachePeerTransferDriver = transfer

	dependencyCandidate := cachePeerImportTestCandidate(dependencyPlan.CacheKey, workspaceKey, compiled.Hash, "dependency-token")
	servicesCandidate := cachePeerImportTestServicesCandidate(servicesPlan.CacheKey, dependencyPlan.CacheKey, compiled.Hash, "services-token")
	peer := newTestCachePeerImportServerWithCandidates(t, []*cleanroomv1.CachePeerCandidate{dependencyCandidate, servicesCandidate}, map[string]testCachePeerExport{
		"dependency-token": {Status: http.StatusOK, Payload: "dependency-zfs-stream"},
		"services-token":   {Status: http.StatusOK, Payload: "services-zfs-stream"},
	})
	svc.Config.Cache.Peers = []runtimeconfig.CachePeerConfig{{URL: peer.URL, TokenEnv: "CLEANROOM_CACHE_PEER_TOKEN"}}

	resp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryDependencyAndServicesPolicy(),
		RepositoryCheckout: repositoryCheckout,
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if got, want := resp.GetSourceKind(), "services stage cache"; got != want {
		t.Fatalf("unexpected source kind: got %q want %q", got, want)
	}
	if got, want := adapter.provisionFromSnapshotCalls, 1; got != want {
		t.Fatalf("expected direct services cache restore once, got %d want %d", got, want)
	}
	if got, want := adapter.provisionCalls, 0; got != want {
		t.Fatalf("expected remote dependency+services imports to skip cold provision, got %d want %d", got, want)
	}
	if got, want := adapter.runCalls, 0; got != want {
		t.Fatalf("expected remote dependency+services imports to skip bootstraps, got %d want %d", got, want)
	}
	if got, want := transfer.importPayloads, []string{"dependency-zfs-stream", "services-zfs-stream"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("unexpected import payloads: got %#v want %#v", got, want)
	}
	if got, want := len(transfer.importRequests), 2; got != want {
		t.Fatalf("expected dependency and services import requests, got %d", got)
	}
	if got, want := transfer.importRequests[0].ParentSnapshotGUID, "workspace-guid"; got != want {
		t.Fatalf("unexpected dependency import parent guid: got %q want %q", got, want)
	}
	if got, want := transfer.importRequests[1].ParentSnapshotGUID, "dependency-guid"; got != want {
		t.Fatalf("unexpected services import parent guid: got %q want %q", got, want)
	}
	if got, want := transfer.importRequests[1].ParentSnapshotRef, "tank/local/imports/dependency@base"; got != want {
		t.Fatalf("unexpected services import parent ref: got %q want %q", got, want)
	}
	if got, want := peer.exportRequests, 2; got != want {
		t.Fatalf("expected dependency and services exports, got %d", got)
	}
	if _, found, err := cacheStore.GetReady(context.Background(), dependencyStageName, dependencyPlan.CacheKey); err != nil {
		t.Fatalf("GetReady dependency returned error: %v", err)
	} else if !found {
		t.Fatal("expected imported dependency cache metadata")
	}
	if _, found, err := cacheStore.GetReady(context.Background(), servicesStageName, servicesPlan.CacheKey); err != nil {
		t.Fatalf("GetReady services returned error: %v", err)
	} else if !found {
		t.Fatal("expected imported services cache metadata")
	}
}

type testCachePeerImportServer struct {
	*httptest.Server
	requests       []*cleanroomv1.LookupCachePeerRequest
	lookupAuth     []string
	exportAuth     []string
	exportRequests int
	candidate      *cleanroomv1.CachePeerCandidate
	candidates     []*cleanroomv1.CachePeerCandidate
	exports        map[string]testCachePeerExport
}

type testCachePeerExport struct {
	Status  int
	Payload string
}

func newTestCachePeerImportServer(t *testing.T, candidate *cleanroomv1.CachePeerCandidate) *testCachePeerImportServer {
	return newTestCachePeerImportServerWithExport(t, candidate, http.StatusOK, "zfs-stream")
}

func newTestCachePeerImportServerWithExport(t *testing.T, candidate *cleanroomv1.CachePeerCandidate, status int, payload string) *testCachePeerImportServer {
	token := "peer-token"
	if candidate != nil && strings.TrimSpace(candidate.GetTransferToken()) != "" {
		token = candidate.GetTransferToken()
	}
	return newTestCachePeerImportServerWithCandidates(t, []*cleanroomv1.CachePeerCandidate{candidate}, map[string]testCachePeerExport{
		token: {Status: status, Payload: payload},
	})
}

func newTestCachePeerImportServerWithCandidates(t *testing.T, candidates []*cleanroomv1.CachePeerCandidate, exports map[string]testCachePeerExport) *testCachePeerImportServer {
	t.Helper()
	peer := &testCachePeerImportServer{
		candidates: candidates,
		exports:    exports,
	}
	if len(candidates) > 0 {
		peer.candidate = candidates[0]
	}
	mux := http.NewServeMux()
	path, handler := cleanroomv1connect.NewCachePeerServiceHandler(peer)
	mux.Handle(path, handler)
	mux.HandleFunc(cachePeerZFSIncrementalExportPathPrefix, func(w http.ResponseWriter, r *http.Request) {
		peer.exportRequests++
		peer.exportAuth = append(peer.exportAuth, r.Header.Get("Authorization"))
		token := strings.TrimPrefix(r.URL.Path, cachePeerZFSIncrementalExportPathPrefix)
		export, ok := peer.exports[token]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if export.Status == 0 {
			export.Status = http.StatusOK
		}
		if export.Status != http.StatusOK {
			http.Error(w, "export failed", export.Status)
			return
		}
		_, _ = w.Write([]byte(export.Payload))
	})
	peer.Server = httptest.NewServer(mux)
	t.Cleanup(peer.Close)
	return peer
}

func cachePeerImportTestCandidate(cacheKey, parentCacheKey, policyHash, token string) *cleanroomv1.CachePeerCandidate {
	return &cleanroomv1.CachePeerCandidate{
		TransferToken:         token,
		Stage:                 dependencyStageName,
		CacheKey:              cacheKey,
		ParentStage:           workspaceStageName,
		ParentCacheKey:        parentCacheKey,
		Backend:               "firecracker",
		StorageDriver:         "zfs",
		Architecture:          runtime.GOARCH,
		ProducerVersion:       dependencyStageProducerVersion,
		PolicyHash:            policyHash,
		ZfsSnapshotGuid:       "dependency-guid",
		ZfsParentSnapshotGuid: "workspace-guid",
		ExpiresAt:             timestamppb.New(time.Now().Add(time.Hour)),
	}
}

func cachePeerImportTestServicesCandidate(cacheKey, parentCacheKey, policyHash, token string) *cleanroomv1.CachePeerCandidate {
	return &cleanroomv1.CachePeerCandidate{
		TransferToken:         token,
		Stage:                 servicesStageName,
		CacheKey:              cacheKey,
		ParentStage:           dependencyStageName,
		ParentCacheKey:        parentCacheKey,
		Backend:               "firecracker",
		StorageDriver:         "zfs",
		Architecture:          runtime.GOARCH,
		ProducerVersion:       servicesStageProducerVersion,
		PolicyHash:            policyHash,
		ZfsSnapshotGuid:       "services-guid",
		ZfsParentSnapshotGuid: "dependency-guid",
		ExpiresAt:             timestamppb.New(time.Now().Add(time.Hour)),
	}
}

func (p *testCachePeerImportServer) LookupCachePeer(_ context.Context, req *connect.Request[cleanroomv1.LookupCachePeerRequest]) (*connect.Response[cleanroomv1.LookupCachePeerResponse], error) {
	p.lookupAuth = append(p.lookupAuth, req.Header().Get("Authorization"))
	p.requests = append(p.requests, req.Msg)
	for _, candidate := range p.candidates {
		if cachePeerCandidateMatchesRequest(candidate, req.Msg, time.Now().Add(-time.Second)) {
			return connect.NewResponse(&cleanroomv1.LookupCachePeerResponse{Candidate: candidate}), nil
		}
	}
	return connect.NewResponse(&cleanroomv1.LookupCachePeerResponse{}), nil
}

func requireMemoryCacheStore(t *testing.T, svc *Service) *memoryCacheStore {
	t.Helper()
	cacheStore, ok := svc.CacheStore.(*memoryCacheStore)
	if !ok {
		t.Fatalf("expected memory cache store, got %T", svc.CacheStore)
	}
	return cacheStore
}

type importedPeerParentRecordOptions struct {
	Stage              string
	CacheKey           string
	ParentCacheKey     string
	StorageRef         string
	SnapshotGUID       string
	ParentSnapshotGUID string
	ProducerVersion    string
	ReuseMode          string
}

func insertImportedPeerParentRecord(t *testing.T, store *memoryCacheStore, opts importedPeerParentRecordOptions) {
	t.Helper()
	metadata := encodeZFSMetadataForTest(t, opts.StorageRef, opts.SnapshotGUID, opts.ParentSnapshotGUID)
	if err := store.Create(context.Background(), cachestore.Record{
		CacheKey:          opts.CacheKey,
		Stage:             opts.Stage,
		ReuseMode:         opts.ReuseMode,
		State:             cacheStateReady,
		BackingSnapshotID: strings.ReplaceAll(opts.CacheKey, ":", "-") + "-snapshot",
		Backend:           "firecracker",
		Architecture:      runtime.GOARCH,
		ParentCacheKey:    opts.ParentCacheKey,
		StorageDriver:     "zfs",
		StorageRef:        opts.StorageRef,
		DriverMetadata:    metadata,
		ProducerVersion:   opts.ProducerVersion,
	}); err != nil {
		t.Fatalf("insert parent cache record: %v", err)
	}
}

func encodeZFSMetadataForTest(t *testing.T, dataset, guid, parentGUID string) string {
	t.Helper()
	metadata, err := volumestore.EncodeZFSDriverMetadata(volumestore.ZFSDriverMetadata{
		Dataset:            dataset,
		SnapshotGUID:       guid,
		ParentSnapshotGUID: parentGUID,
	})
	if err != nil {
		t.Fatalf("EncodeZFSDriverMetadata returned error: %v", err)
	}
	return metadata
}

func requireCachePeerImportRequest(t *testing.T, transfer *stubCachePeerTransferDriver, parentRef, parentGUID, expectedGUID, payload string) {
	t.Helper()
	if got, want := len(transfer.importRequests), 1; got != want {
		t.Fatalf("expected one import request, got %d", got)
	}
	req := transfer.importRequests[0]
	if strings.TrimSpace(req.SnapshotID) == "" {
		t.Fatal("expected imported snapshot id")
	}
	if got, want := req.ParentSnapshotRef, parentRef; got != want {
		t.Fatalf("unexpected import parent ref: got %q want %q", got, want)
	}
	if got, want := req.ParentSnapshotGUID, parentGUID; got != want {
		t.Fatalf("unexpected import parent guid: got %q want %q", got, want)
	}
	if got, want := req.ExpectedSnapshotGUID, expectedGUID; got != want {
		t.Fatalf("unexpected import expected guid: got %q want %q", got, want)
	}
	if got, want := transfer.importPayloads, []string{payload}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("unexpected import payloads: got %#v want %#v", got, want)
	}
}

func requireCachePeerLookup(t *testing.T, peer *testCachePeerImportServer, want *cleanroomv1.LookupCachePeerRequest) {
	t.Helper()
	if got, want := peer.lookupAuth, []string{"Bearer receiver-token"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("unexpected lookup auth headers: got %#v want %#v", got, want)
	}
	if got, want := peer.exportAuth, []string{"Bearer receiver-token"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("unexpected export auth headers: got %#v want %#v", got, want)
	}
	if got, want := peer.exportRequests, 1; got != want {
		t.Fatalf("unexpected export request count: got %d want %d", got, want)
	}
	if got, want := len(peer.requests), 1; got != want {
		t.Fatalf("expected one peer lookup request, got %d", got)
	}
	got := peer.requests[0]
	fields := []struct {
		name string
		got  string
		want string
	}{
		{"stage", got.GetStage(), want.GetStage()},
		{"cache key", got.GetCacheKey(), want.GetCacheKey()},
		{"backend", got.GetBackend(), want.GetBackend()},
		{"storage driver", got.GetStorageDriver(), want.GetStorageDriver()},
		{"architecture", got.GetArchitecture(), want.GetArchitecture()},
		{"producer version", got.GetProducerVersion(), want.GetProducerVersion()},
		{"policy hash", got.GetPolicyHash(), want.GetPolicyHash()},
		{"parent stage", got.GetParentStage(), want.GetParentStage()},
		{"parent cache key", got.GetParentCacheKey(), want.GetParentCacheKey()},
		{"parent zfs guid", got.GetParentZfsSnapshotGuid(), want.GetParentZfsSnapshotGuid()},
	}
	for _, field := range fields {
		if field.got != field.want {
			t.Fatalf("unexpected lookup %s: got %q want %q", field.name, field.got, field.want)
		}
	}
	if strings.TrimSpace(got.GetParentZfsSnapshotGuid()) == "" {
		t.Fatal("expected peer lookup to include local parent zfs guid")
	}
	if got.GetParentZfsSnapshotGuid() != want.GetParentZfsSnapshotGuid() {
		t.Fatalf("unexpected parent zfs guid: got %q want %q", got.GetParentZfsSnapshotGuid(), want.GetParentZfsSnapshotGuid())
	}
}
