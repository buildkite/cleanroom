//go:build linux

package controlservice

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/cachestore"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/gen/cleanroom/v1/cleanroomv1connect"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"github.com/buildkite/cleanroom/internal/volumestore"
)

const (
	cachePeerZFSE2EEnv        = "CLEANROOM_CACHE_PEER_ZFS_E2E"
	cachePeerZFSE2EDatasetEnv = "CLEANROOM_CACHE_PEER_ZFS_E2E_DATASET"
	cachePeerZFSE2EHelperEnv  = "CLEANROOM_CACHE_PEER_ZFS_E2E_HELPER"
)

func TestCachePeerZFSIncrementalImportWithRealZFS(t *testing.T) {
	if os.Getenv(cachePeerZFSE2EEnv) != "1" {
		t.Skipf("set %s=1 and %s=<pool/cleanroom> to run the real cache peer ZFS e2e", cachePeerZFSE2EEnv, cachePeerZFSE2EDatasetEnv)
	}

	datasetRoot := strings.Trim(strings.TrimSpace(os.Getenv(cachePeerZFSE2EDatasetEnv)), "/")
	if datasetRoot == "" {
		datasetRoot = strings.Trim(strings.TrimSpace(os.Getenv("CLEANROOM_ZFS_DATASET")), "/")
	}
	if datasetRoot == "" {
		t.Skipf("set %s=<pool/cleanroom> to run the real cache peer ZFS e2e", cachePeerZFSE2EDatasetEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	runner := cachePeerZFSIntegrationRunner{
		helperPath: strings.TrimSpace(os.Getenv(cachePeerZFSE2EHelperEnv)),
	}
	testID := cachePeerZFSTestComponent(fmt.Sprintf("cache-peer-zfs-e2e-%d", time.Now().UnixNano()))
	senderRoot := datasetRoot + "/" + testID + "-sender"
	receiverRoot := datasetRoot + "/" + testID + "-receiver"
	cleanupZFSRoot(t, runner, senderRoot)
	cleanupZFSRoot(t, runner, receiverRoot)

	senderDriver := newCachePeerZFSTestDriver(t, senderRoot, runner)
	receiverDriver := newCachePeerZFSTestDriver(t, receiverRoot, runner)

	basePath := filepath.Join(t.TempDir(), "base.raw")
	if err := writeCachePeerZFSTestImage(basePath, "cleanroom cache peer parent\n"); err != nil {
		t.Fatalf("write base image: %v", err)
	}

	parent, err := senderDriver.EnsureBaseVolume(ctx, volumestore.EnsureBaseVolumeRequest{
		BaseID:     "workspace-parent",
		SourcePath: basePath,
	})
	if err != nil {
		t.Fatalf("sender EnsureBaseVolume returned error: %v", err)
	}
	senderParentDesc, err := senderDriver.DescribeSnapshot(ctx, volumestore.DescribeSnapshotRequest{SnapshotRef: parent.Ref})
	if err != nil {
		t.Fatalf("sender DescribeSnapshot(parent) returned error: %v", err)
	}

	receiverParentRef := seedReceiverParentFromSender(ctx, t, runner, parent.Ref, receiverRoot+"/base/workspace-parent")
	receiverParentDesc, err := receiverDriver.DescribeSnapshot(ctx, volumestore.DescribeSnapshotRequest{SnapshotRef: receiverParentRef})
	if err != nil {
		t.Fatalf("receiver DescribeSnapshot(parent) returned error: %v", err)
	}
	if receiverParentDesc.SnapshotGUID != senderParentDesc.SnapshotGUID {
		t.Fatalf("seeded receiver parent GUID mismatch: got %q want %q", receiverParentDesc.SnapshotGUID, senderParentDesc.SnapshotGUID)
	}

	childPath := filepath.Join(t.TempDir(), "child.raw")
	if err := writeCachePeerZFSTestImage(childPath, "cleanroom cache peer child mutation\n"); err != nil {
		t.Fatalf("write child image: %v", err)
	}
	childVolume, err := senderDriver.CreateWritableVolume(ctx, volumestore.CreateWritableVolumeRequest{
		VolumeID: "dependency-source",
		BaseRef:  parent.Ref,
	})
	if err != nil {
		t.Fatalf("sender CreateWritableVolume returned error: %v", err)
	}
	if err := runner.Run(ctx, "dd", "if="+childPath, "of="+childVolume.AttachmentPath, "bs=4M", "conv=fsync", "status=none"); err != nil {
		t.Fatalf("mutate sender child volume: %v", err)
	}
	child, err := senderDriver.SnapshotVolume(ctx, volumestore.SnapshotVolumeRequest{
		SnapshotID: "dependency-child",
		VolumeRef:  childVolume.Ref,
	})
	if err != nil {
		t.Fatalf("sender SnapshotVolume returned error: %v", err)
	}
	childDesc, err := senderDriver.DescribeSnapshot(ctx, volumestore.DescribeSnapshotRequest{
		SnapshotRef:        child.StorageRef,
		ParentSnapshotGUID: senderParentDesc.SnapshotGUID,
	})
	if err != nil {
		t.Fatalf("sender DescribeSnapshot(child) returned error: %v", err)
	}
	if childDesc.ParentSnapshotGUID != senderParentDesc.SnapshotGUID {
		t.Fatalf("unexpected sender child parent GUID: got %q want %q", childDesc.ParentSnapshotGUID, senderParentDesc.SnapshotGUID)
	}

	tokenEnv := "CLEANROOM_CACHE_PEER_ZFS_E2E_TOKEN"
	t.Setenv(tokenEnv, "shared-cache-peer-token")
	const (
		parentCacheKey = "workspace-parent-cache"
		childCacheKey  = "dependency-child-cache"
		policyHash     = "policy-hash"
	)

	senderSvc := newTestService(&stubAdapter{})
	senderSvc.Config.Cache.Peers = runtimeconfigCachePeer(tokenEnv)
	senderSvc.CachePeerTransferDriver = senderDriver
	senderStore := requireMemoryCacheStore(t, senderSvc)
	insertCachePeerZFSTestRecord(t, senderStore, cachePeerZFSTestRecord{
		Stage:           workspaceStageName,
		CacheKey:        parentCacheKey,
		StorageRef:      parent.Ref,
		SnapshotGUID:    senderParentDesc.SnapshotGUID,
		ProducerVersion: workspaceStageProducerVersion,
	})
	insertCachePeerZFSTestRecord(t, senderStore, cachePeerZFSTestRecord{
		Stage:              dependencyStageName,
		CacheKey:           childCacheKey,
		ParentCacheKey:     parentCacheKey,
		StorageRef:         child.StorageRef,
		SnapshotGUID:       childDesc.SnapshotGUID,
		ParentSnapshotGUID: senderParentDesc.SnapshotGUID,
		ProducerVersion:    dependencyStageProducerVersion,
		PolicyHash:         policyHash,
	})

	lookup := cachePeerLookup{
		Stage:                 dependencyStageName,
		CacheKey:              childCacheKey,
		Backend:               "firecracker",
		StorageDriver:         "zfs",
		Architecture:          runtime.GOARCH,
		ProducerVersion:       dependencyStageProducerVersion,
		PolicyHash:            policyHash,
		ParentStage:           workspaceStageName,
		ParentCacheKey:        parentCacheKey,
		ParentZFSSnapshotGUID: receiverParentDesc.SnapshotGUID,
	}
	match, missReason, err := senderSvc.planCachePeerExport(ctx, lookup)
	if err != nil {
		t.Fatalf("sender planCachePeerExport returned error: %v", err)
	}
	if missReason != "" {
		t.Fatalf("sender planCachePeerExport missed: %s", missReason)
	}
	var directStream bytes.Buffer
	if err := senderDriver.ExportIncrementalSnapshot(ctx, match.Plan, &directStream); err != nil {
		t.Fatalf("direct ExportIncrementalSnapshot returned error: %v", err)
	}
	directImport, err := receiverDriver.ImportIncrementalSnapshot(ctx, volumestore.IncrementalSnapshotImportRequest{
		SnapshotID:           "direct-import",
		ParentSnapshotRef:    receiverParentRef,
		ParentSnapshotGUID:   receiverParentDesc.SnapshotGUID,
		ExpectedSnapshotGUID: childDesc.SnapshotGUID,
	}, bytes.NewReader(directStream.Bytes()))
	if err != nil {
		t.Fatalf("direct ImportIncrementalSnapshot returned error: %v", err)
	}
	if _, err := receiverDriver.CloneSnapshotToVolume(ctx, volumestore.CloneSnapshotToVolumeRequest{
		VolumeID:    "direct-import-clone",
		SnapshotRef: directImport.StorageRef,
	}); err != nil {
		t.Fatalf("direct imported snapshot clone returned error: %v", err)
	}

	senderServer := newCachePeerZFSIntegrationServer(t, senderSvc)
	receiverSvc := newTestService(&stubAdapter{})
	receiverSvc.Config.Cache.Peers = runtimeconfigCachePeer(tokenEnv, senderServer.URL)
	receiverSvc.CachePeerTransferDriver = receiverDriver
	receiverStore := requireMemoryCacheStore(t, receiverSvc)
	insertCachePeerZFSTestRecord(t, receiverStore, cachePeerZFSTestRecord{
		Stage:           workspaceStageName,
		CacheKey:        parentCacheKey,
		StorageRef:      receiverParentRef,
		SnapshotGUID:    receiverParentDesc.SnapshotGUID,
		ProducerVersion: workspaceStageProducerVersion,
	})

	lookupReq := &cleanroomv1.LookupCachePeerRequest{
		Stage:                 dependencyStageName,
		CacheKey:              childCacheKey,
		Backend:               "firecracker",
		StorageDriver:         "zfs",
		Architecture:          runtime.GOARCH,
		ProducerVersion:       dependencyStageProducerVersion,
		PolicyHash:            policyHash,
		ParentStage:           workspaceStageName,
		ParentCacheKey:        parentCacheKey,
		ParentZfsSnapshotGuid: receiverParentDesc.SnapshotGUID,
	}
	lookupCtx, cancelLookup := context.WithCancel(ctx)
	candidates := receiverSvc.lookupCachePeerImportCandidates(lookupCtx, lookupReq)
	select {
	case candidate, ok := <-candidates:
		if !ok {
			t.Fatal("receiver peer lookup returned no candidates")
		}
		if got, want := candidate.candidate.GetZfsSnapshotGuid(), childDesc.SnapshotGUID; got != want {
			t.Fatalf("receiver peer lookup candidate guid: got %q want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for receiver peer lookup candidate")
	}
	cancelLookup()

	result, err := receiverSvc.importCachePeerStage(ctx, &stubAdapter{}, backend.FirecrackerConfig{
		Snapshots: backend.SnapshotConfig{
			Enabled:    true,
			Driver:     "zfs",
			ZFSDataset: receiverRoot,
		},
	}, cachePeerImportOptions{
		Stage:           dependencyStageName,
		CacheKey:        childCacheKey,
		ParentStage:     workspaceStageName,
		ParentCacheKey:  parentCacheKey,
		Backend:         "firecracker",
		StorageDriver:   "zfs",
		ProducerVersion: dependencyStageProducerVersion,
		PolicyHash:      policyHash,
		NewRecord: func(snapshotID string, snapshot volumestore.Snapshot, now time.Time) cachestore.Record {
			return cachestore.Record{
				CacheKey:          childCacheKey,
				Stage:             dependencyStageName,
				State:             cacheStateReady,
				BackingSnapshotID: snapshotID,
				Backend:           "firecracker",
				Architecture:      runtime.GOARCH,
				PolicyHash:        policyHash,
				ParentCacheKey:    parentCacheKey,
				StorageDriver:     "zfs",
				StorageRef:        strings.TrimSpace(snapshot.StorageRef),
				DriverMetadata:    snapshot.DriverMetadata,
				ImportedFromPeer:  true,
				CreatedAt:         now,
				LastUsedAt:        now,
				ProducerVersion:   dependencyStageProducerVersion,
			}
		},
		ValidateRecord: func(ctx context.Context) (cachestore.Record, bool, string, error) {
			record, found, err := receiverStore.GetReady(ctx, dependencyStageName, childCacheKey)
			if err != nil || !found {
				return record, found, "imported dependency record not found", err
			}
			metadata, ok := cachePeerRecordZFSMetadata(record)
			if !ok {
				return cachestore.Record{}, false, "imported dependency zfs metadata missing", nil
			}
			if metadata.SnapshotGUID != childDesc.SnapshotGUID {
				return cachestore.Record{}, false, "imported dependency zfs snapshot guid mismatch", nil
			}
			if metadata.ParentSnapshotGUID != receiverParentDesc.SnapshotGUID {
				return cachestore.Record{}, false, "imported dependency zfs parent guid mismatch", nil
			}
			return record, true, "", nil
		},
	})
	if err != nil {
		t.Fatalf("importCachePeerStage returned error: %v", err)
	}
	if !result.imported {
		t.Fatal("expected receiver to import dependency child from peer")
	}
	if result.record.StorageRef == child.StorageRef {
		t.Fatalf("imported record kept sender storage ref %q", result.record.StorageRef)
	}
	if !strings.HasPrefix(result.record.StorageRef, receiverRoot+"/snapshots/imports/") {
		t.Fatalf("imported record storage ref %q is not under receiver import namespace %q", result.record.StorageRef, receiverRoot+"/snapshots/imports/")
	}

	importedClone, err := receiverDriver.CloneSnapshotToVolume(ctx, volumestore.CloneSnapshotToVolumeRequest{
		VolumeID:    "dependency-imported-clone",
		SnapshotRef: result.record.StorageRef,
	})
	if err != nil {
		t.Fatalf("receiver CloneSnapshotToVolume(imported) returned error: %v", err)
	}
	if strings.TrimSpace(importedClone.AttachmentPath) == "" {
		t.Fatal("expected imported clone attachment path")
	}
}

type cachePeerZFSTestRecord struct {
	Stage              string
	CacheKey           string
	ParentCacheKey     string
	StorageRef         string
	SnapshotGUID       string
	ParentSnapshotGUID string
	ProducerVersion    string
	PolicyHash         string
}

func insertCachePeerZFSTestRecord(t *testing.T, store *memoryCacheStore, opts cachePeerZFSTestRecord) {
	t.Helper()
	metadata := encodeZFSMetadataForTest(t, opts.StorageRef, opts.SnapshotGUID, opts.ParentSnapshotGUID)
	if err := store.Create(context.Background(), cachestore.Record{
		CacheKey:          opts.CacheKey,
		Stage:             opts.Stage,
		State:             cacheStateReady,
		BackingSnapshotID: opts.CacheKey + "-snapshot",
		Backend:           "firecracker",
		Architecture:      runtime.GOARCH,
		PolicyHash:        opts.PolicyHash,
		ParentCacheKey:    opts.ParentCacheKey,
		StorageDriver:     "zfs",
		StorageRef:        opts.StorageRef,
		DriverMetadata:    metadata,
		ProducerVersion:   opts.ProducerVersion,
	}); err != nil {
		t.Fatalf("insert cache record: %v", err)
	}
}

func runtimeconfigCachePeer(tokenEnv string, urls ...string) []runtimeconfig.CachePeerConfig {
	url := "https://peer.example.invalid"
	if len(urls) > 0 {
		url = urls[0]
	}
	return []runtimeconfig.CachePeerConfig{{URL: url, TokenEnv: tokenEnv}}
}

func newCachePeerZFSTestDriver(t *testing.T, datasetRoot string, runner cachePeerZFSIntegrationRunner) *volumestore.ZFSDriver {
	t.Helper()
	driver, err := volumestore.NewZFSDriver(volumestore.ZFSDriverOptions{
		DatasetRoot: datasetRoot,
		Runner:      runner,
	})
	if err != nil {
		t.Fatalf("NewZFSDriver(%q) returned error: %v", datasetRoot, err)
	}
	return driver
}

func seedReceiverParentFromSender(ctx context.Context, t *testing.T, runner cachePeerZFSIntegrationRunner, senderParentRef, receiverParentDataset string) string {
	t.Helper()
	if err := runner.Run(ctx, "zfs", "create", "-p", datasetParent(receiverParentDataset)); err != nil {
		t.Fatalf("create receiver parent namespace: %v", err)
	}
	var stream bytes.Buffer
	if err := runner.OutputTo(ctx, &stream, "zfs", "send", senderParentRef); err != nil {
		t.Fatalf("send parent snapshot: %v", err)
	}
	if err := runner.InputFrom(ctx, bytes.NewReader(stream.Bytes()), "zfs", "receive", "-u", receiverParentDataset); err != nil {
		t.Fatalf("receive parent snapshot: %v", err)
	}
	return receiverParentDataset + "@base"
}

func cleanupZFSRoot(t *testing.T, runner cachePeerZFSIntegrationRunner, dataset string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := runner.Run(ctx, "zfs", "destroy", "-r", dataset); err != nil && !isCachePeerZFSMissingError(err) {
			t.Logf("cleanup zfs dataset %q: %v", dataset, err)
		}
	})
}

func writeCachePeerZFSTestImage(path, contents string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(contents); err != nil {
		_ = file.Close()
		return err
	}
	err = file.Truncate(4 << 20)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

type cachePeerZFSIntegrationHandler struct {
	service *Service
}

func newCachePeerZFSIntegrationServer(t *testing.T, service *Service) *httptest.Server {
	t.Helper()
	handler := cachePeerZFSIntegrationHandler{service: service}
	rpcPath, rpcHandler := cleanroomv1connect.NewCachePeerServiceHandler(handler)
	mux := http.NewServeMux()
	mux.Handle(rpcPath, rpcHandler)
	mux.HandleFunc(cachePeerZFSIncrementalExportPathPrefix, handler.handleExport)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func (h cachePeerZFSIntegrationHandler) LookupCachePeer(ctx context.Context, req *connect.Request[cleanroomv1.LookupCachePeerRequest]) (*connect.Response[cleanroomv1.LookupCachePeerResponse], error) {
	if err := h.service.AuthorizeCachePeerBearer(req.Header().Get("Authorization")); err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	resp, err := h.service.LookupCachePeer(ctx, req.Msg)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(resp), nil
}

func (h cachePeerZFSIntegrationHandler) handleExport(w http.ResponseWriter, req *http.Request) {
	if err := h.service.AuthorizeCachePeerBearer(req.Header.Get("Authorization")); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	token := strings.TrimPrefix(req.URL.Path, cachePeerZFSIncrementalExportPathPrefix)
	if token == "" || strings.Contains(token, "/") {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if err := h.service.ExportCachePeerZFSIncremental(req.Context(), token, w); err != nil {
		if errors.Is(err, ErrCachePeerExportTokenNotFound) {
			http.NotFound(w, req)
			return
		}
		http.Error(w, "cache peer export failed", http.StatusInternalServerError)
	}
}

type cachePeerZFSIntegrationRunner struct {
	helperPath string
}

func (r cachePeerZFSIntegrationRunner) Run(ctx context.Context, command string, args ...string) error {
	return r.run(ctx, nil, io.Discard, command, args...)
}

func (r cachePeerZFSIntegrationRunner) Output(ctx context.Context, command string, args ...string) ([]byte, error) {
	var stdout bytes.Buffer
	if err := r.run(ctx, nil, &stdout, command, args...); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

func (r cachePeerZFSIntegrationRunner) OutputTo(ctx context.Context, dst io.Writer, command string, args ...string) error {
	return r.run(ctx, nil, dst, command, args...)
}

func (r cachePeerZFSIntegrationRunner) InputFrom(ctx context.Context, src io.Reader, command string, args ...string) error {
	return r.run(ctx, src, io.Discard, command, args...)
}

func (r cachePeerZFSIntegrationRunner) run(ctx context.Context, stdin io.Reader, stdout io.Writer, command string, args ...string) error {
	argv := append([]string{command}, args...)
	execArgv := argv
	if r.helperPath != "" {
		execArgv = append([]string{"sudo", "-n", r.helperPath}, argv...)
	}

	cmd := exec.CommandContext(ctx, execArgv[0], execArgv[1:]...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, msg)
		}
		return fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}
	return nil
}

func datasetParent(dataset string) string {
	dataset = strings.TrimSpace(dataset)
	idx := strings.LastIndex(dataset, "/")
	if idx <= 0 {
		return ""
	}
	return dataset[:idx]
}

func cachePeerZFSTestComponent(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '-'
	}, value)
	return strings.Trim(value, "-")
}

func isCachePeerZFSMissingError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "dataset does not exist") ||
		strings.Contains(msg, "no such pool or dataset") ||
		strings.Contains(msg, "snapshot does not exist")
}

var _ volumestore.ZFSCommandOutputStreamer = cachePeerZFSIntegrationRunner{}
var _ volumestore.ZFSCommandInputStreamer = cachePeerZFSIntegrationRunner{}
