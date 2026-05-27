package controlserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/buildkite/cleanroom/internal/cachestore"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/gen/cleanroom/v1/cleanroomv1connect"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"github.com/buildkite/cleanroom/internal/volumestore"
)

func TestCachePeerLookupRequiresBearerToken(t *testing.T) {
	service := newHandlerTestService(newHandlerTestAdapter())
	service.Config.Cache = runtimeconfig.CacheConfig{
		Peers: []runtimeconfig.CachePeerConfig{{URL: "https://peer.example", TokenEnv: "CLEANROOM_CACHE_PEER_TOKEN"}},
	}
	httpServer := httptest.NewServer(New(service, nil).Handler())
	defer httpServer.Close()

	client := cleanroomv1connect.NewCachePeerServiceClient(http.DefaultClient, httpServer.URL)
	_, err := client.LookupCachePeer(context.Background(), connect.NewRequest(&cleanroomv1.LookupCachePeerRequest{
		Stage:    "dependency",
		CacheKey: "cache",
	}))
	if err == nil {
		t.Fatal("expected unauthenticated cache peer lookup")
	}
	if code := connect.CodeOf(err); code != connect.CodeUnauthenticated {
		t.Fatalf("unexpected error code: got %v want %v (err=%v)", code, connect.CodeUnauthenticated, err)
	}
}

func TestCachePeerLookupReturnsMissWhenCacheStoreUnavailable(t *testing.T) {
	t.Setenv("CLEANROOM_CACHE_PEER_TOKEN", "shared-secret")
	service := newHandlerTestService(newHandlerTestAdapter())
	service.Config.Cache = runtimeconfig.CacheConfig{
		Peers: []runtimeconfig.CachePeerConfig{{URL: "https://peer.example", TokenEnv: "CLEANROOM_CACHE_PEER_TOKEN"}},
	}
	service.CacheStore = nil
	service.CachePeerTransferDriver = &handlerCachePeerTransferDriver{payload: "zfs-stream"}
	httpServer := httptest.NewServer(New(service, nil).Handler())
	defer httpServer.Close()

	client := cleanroomv1connect.NewCachePeerServiceClient(http.DefaultClient, httpServer.URL)
	lookupReq := connect.NewRequest(&cleanroomv1.LookupCachePeerRequest{
		Stage:                 "dependency",
		CacheKey:              "dependency-child",
		Backend:               "firecracker",
		StorageDriver:         "zfs",
		Architecture:          runtime.GOARCH,
		ProducerVersion:       "cleanroom/dependency-stage-v1",
		PolicyHash:            "policy-hash",
		ParentStage:           "workspace",
		ParentCacheKey:        "workspace-parent",
		ParentZfsSnapshotGuid: "parent-guid",
	})
	lookupReq.Header().Set("Authorization", "Bearer shared-secret")
	lookupResp, err := client.LookupCachePeer(context.Background(), lookupReq)
	if err != nil {
		t.Fatalf("LookupCachePeer returned error: %v", err)
	}
	if lookupResp.Msg.GetCandidate() != nil {
		t.Fatalf("expected miss, got candidate %#v", lookupResp.Msg.GetCandidate())
	}
	if got, want := lookupResp.Msg.GetMissReason(), "cache metadata unavailable"; got != want {
		t.Fatalf("unexpected miss reason: got %q want %q", got, want)
	}
}

func TestCachePeerLookupAndExportOverHTTP(t *testing.T) {
	t.Setenv("CLEANROOM_CACHE_PEER_TOKEN", "shared-secret")
	store := newHandlerCacheStore()
	insertHandlerCachePeerRecord(t, store, handlerCachePeerRecordOptions{
		Stage:        "workspace",
		CacheKey:     "workspace-parent",
		StorageRef:   "tank/cleanroom/snapshots/workspace@base",
		SnapshotGUID: "parent-guid",
	})
	insertHandlerCachePeerRecord(t, store, handlerCachePeerRecordOptions{
		Stage:              "dependency",
		CacheKey:           "dependency-child",
		ParentCacheKey:     "workspace-parent",
		StorageRef:         "tank/cleanroom/snapshots/dependency@base",
		SnapshotGUID:       "child-guid",
		ParentSnapshotGUID: "parent-guid",
		ProducerVersion:    "cleanroom/dependency-stage-v1",
	})

	service := newHandlerTestService(newHandlerTestAdapter())
	service.Config.Cache = runtimeconfig.CacheConfig{
		Peers: []runtimeconfig.CachePeerConfig{{URL: "https://peer.example", TokenEnv: "CLEANROOM_CACHE_PEER_TOKEN"}},
	}
	service.CacheStore = store
	service.CachePeerTransferDriver = &handlerCachePeerTransferDriver{payload: "zfs-stream"}
	httpServer := httptest.NewServer(New(service, nil).Handler())
	defer httpServer.Close()

	client := cleanroomv1connect.NewCachePeerServiceClient(http.DefaultClient, httpServer.URL)
	lookupReq := connect.NewRequest(&cleanroomv1.LookupCachePeerRequest{
		Stage:                 "dependency",
		CacheKey:              "dependency-child",
		Backend:               "firecracker",
		StorageDriver:         "zfs",
		Architecture:          runtime.GOARCH,
		ProducerVersion:       "cleanroom/dependency-stage-v1",
		PolicyHash:            "policy-hash",
		ParentStage:           "workspace",
		ParentCacheKey:        "workspace-parent",
		ParentZfsSnapshotGuid: "parent-guid",
	})
	lookupReq.Header().Set("Authorization", "Bearer shared-secret")
	lookupResp, err := client.LookupCachePeer(context.Background(), lookupReq)
	if err != nil {
		t.Fatalf("LookupCachePeer returned error: %v", err)
	}
	token := lookupResp.Msg.GetCandidate().GetTransferToken()
	if strings.TrimSpace(token) == "" {
		t.Fatalf("expected transfer token, response %#v", lookupResp.Msg)
	}

	exportReq, err := http.NewRequest(http.MethodGet, httpServer.URL+cachePeerZFSIncrementalExportPathPrefix+token, nil)
	if err != nil {
		t.Fatalf("create export request: %v", err)
	}
	exportReq.Header.Set("Authorization", "Bearer shared-secret")
	exportResp, err := http.DefaultClient.Do(exportReq)
	if err != nil {
		t.Fatalf("GET export returned error: %v", err)
	}
	defer exportResp.Body.Close()
	if got, want := exportResp.StatusCode, http.StatusOK; got != want {
		body, _ := io.ReadAll(exportResp.Body)
		t.Fatalf("unexpected export status: got %d want %d body=%q", got, want, string(body))
	}
	body, err := io.ReadAll(exportResp.Body)
	if err != nil {
		t.Fatalf("read export body: %v", err)
	}
	if got, want := string(body), "zfs-stream"; got != want {
		t.Fatalf("unexpected export body: got %q want %q", got, want)
	}

	secondReq, err := http.NewRequest(http.MethodGet, httpServer.URL+cachePeerZFSIncrementalExportPathPrefix+token, nil)
	if err != nil {
		t.Fatalf("create second export request: %v", err)
	}
	secondReq.Header.Set("Authorization", "Bearer shared-secret")
	secondResp, err := http.DefaultClient.Do(secondReq)
	if err != nil {
		t.Fatalf("second GET export returned error: %v", err)
	}
	defer secondResp.Body.Close()
	if got, want := secondResp.StatusCode, http.StatusNotFound; got != want {
		t.Fatalf("unexpected second export status: got %d want %d", got, want)
	}
}

func TestCachePeerExportDoesNotAppendHTTPErrorAfterStreamStarts(t *testing.T) {
	t.Setenv("CLEANROOM_CACHE_PEER_TOKEN", "shared-secret")
	store := newHandlerCacheStore()
	insertHandlerCachePeerRecord(t, store, handlerCachePeerRecordOptions{
		Stage:        "workspace",
		CacheKey:     "workspace-parent",
		StorageRef:   "tank/cleanroom/snapshots/workspace@base",
		SnapshotGUID: "parent-guid",
	})
	insertHandlerCachePeerRecord(t, store, handlerCachePeerRecordOptions{
		Stage:              "dependency",
		CacheKey:           "dependency-child",
		ParentCacheKey:     "workspace-parent",
		StorageRef:         "tank/cleanroom/snapshots/dependency@base",
		SnapshotGUID:       "child-guid",
		ParentSnapshotGUID: "parent-guid",
		ProducerVersion:    "cleanroom/dependency-stage-v1",
	})

	service := newHandlerTestService(newHandlerTestAdapter())
	service.Config.Cache = runtimeconfig.CacheConfig{
		Peers: []runtimeconfig.CachePeerConfig{{URL: "https://peer.example", TokenEnv: "CLEANROOM_CACHE_PEER_TOKEN"}},
	}
	service.CacheStore = store
	service.CachePeerTransferDriver = &handlerCachePeerTransferDriver{
		writeBeforeErr: "partial-zfs-stream",
		exportErr:      errors.New("zfs send interrupted"),
	}
	httpServer := httptest.NewServer(New(service, nil).Handler())
	defer httpServer.Close()

	client := cleanroomv1connect.NewCachePeerServiceClient(http.DefaultClient, httpServer.URL)
	lookupReq := connect.NewRequest(&cleanroomv1.LookupCachePeerRequest{
		Stage:                 "dependency",
		CacheKey:              "dependency-child",
		Backend:               "firecracker",
		StorageDriver:         "zfs",
		Architecture:          runtime.GOARCH,
		ProducerVersion:       "cleanroom/dependency-stage-v1",
		PolicyHash:            "policy-hash",
		ParentStage:           "workspace",
		ParentCacheKey:        "workspace-parent",
		ParentZfsSnapshotGuid: "parent-guid",
	})
	lookupReq.Header().Set("Authorization", "Bearer shared-secret")
	lookupResp, err := client.LookupCachePeer(context.Background(), lookupReq)
	if err != nil {
		t.Fatalf("LookupCachePeer returned error: %v", err)
	}

	exportReq, err := http.NewRequest(http.MethodGet, httpServer.URL+cachePeerZFSIncrementalExportPathPrefix+lookupResp.Msg.GetCandidate().GetTransferToken(), nil)
	if err != nil {
		t.Fatalf("create export request: %v", err)
	}
	exportReq.Header.Set("Authorization", "Bearer shared-secret")
	exportResp, err := http.DefaultClient.Do(exportReq)
	if err != nil {
		t.Fatalf("GET export returned error: %v", err)
	}
	defer exportResp.Body.Close()
	body, err := io.ReadAll(exportResp.Body)
	if err != nil {
		t.Fatalf("read export body: %v", err)
	}
	if got, want := exportResp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("unexpected export status: got %d want %d body=%q", got, want, string(body))
	}
	if got, want := string(body), "partial-zfs-stream"; got != want {
		t.Fatalf("unexpected export body: got %q want %q", got, want)
	}
}

type handlerCachePeerTransferDriver struct {
	payload        string
	writeBeforeErr string
	exportErr      error
}

func (d *handlerCachePeerTransferDriver) DescribeSnapshot(context.Context, volumestore.DescribeSnapshotRequest) (volumestore.SnapshotDescription, error) {
	return volumestore.SnapshotDescription{}, nil
}

func (d *handlerCachePeerTransferDriver) PlanIncrementalSnapshotExport(_ context.Context, req volumestore.IncrementalSnapshotExportRequest) (volumestore.IncrementalSnapshotExportPlan, error) {
	return volumestore.IncrementalSnapshotExportPlan{
		FromSnapshotRef:  req.FromSnapshotRef,
		FromSnapshotGUID: req.FromSnapshotGUID,
		ToSnapshotRef:    req.ToSnapshotRef,
		ToSnapshotGUID:   req.ToSnapshotGUID,
		EstimatedBytes:   42,
	}, nil
}

func (d *handlerCachePeerTransferDriver) ExportIncrementalSnapshot(_ context.Context, _ volumestore.IncrementalSnapshotExportPlan, dst io.Writer) error {
	if d.writeBeforeErr != "" {
		if _, err := io.WriteString(dst, d.writeBeforeErr); err != nil {
			return err
		}
	}
	if d.exportErr != nil {
		return d.exportErr
	}
	_, err := io.WriteString(dst, d.payload)
	return err
}

func (d *handlerCachePeerTransferDriver) ImportIncrementalSnapshot(context.Context, volumestore.IncrementalSnapshotImportRequest, io.Reader) (volumestore.Snapshot, error) {
	return volumestore.Snapshot{}, nil
}

type handlerCacheStore struct {
	records map[string]cachestore.Record
}

func newHandlerCacheStore() *handlerCacheStore {
	return &handlerCacheStore{records: map[string]cachestore.Record{}}
}

func (s *handlerCacheStore) Create(_ context.Context, record cachestore.Record) error {
	s.records[handlerCacheStoreOwnerKey(record.Stage, record.CacheKey, record.OwnerPrincipalID)] = record
	return nil
}

func (s *handlerCacheStore) Upsert(_ context.Context, record cachestore.Record) error {
	s.records[handlerCacheStoreOwnerKey(record.Stage, record.CacheKey, record.OwnerPrincipalID)] = record
	return nil
}

func (s *handlerCacheStore) GetReady(_ context.Context, stage, cacheKey string) (cachestore.Record, bool, error) {
	record, ok := s.records[handlerCacheStoreOwnerKey(stage, cacheKey, "")]
	if !ok || record.State != "ready" {
		return cachestore.Record{}, false, nil
	}
	return record, true, nil
}

func (s *handlerCacheStore) GetReadyForOwner(_ context.Context, stage, cacheKey, ownerPrincipalID string) (cachestore.Record, bool, error) {
	ownerPrincipalID = strings.TrimSpace(ownerPrincipalID)
	if ownerPrincipalID == "" {
		return cachestore.Record{}, false, nil
	}
	record, ok := s.records[handlerCacheStoreOwnerKey(stage, cacheKey, ownerPrincipalID)]
	if !ok || record.State != "ready" {
		return cachestore.Record{}, false, nil
	}
	return record, true, nil
}

func (s *handlerCacheStore) Touch(context.Context, string, string) error {
	return nil
}

func (s *handlerCacheStore) TouchForOwner(context.Context, string, string, string) error {
	return nil
}

func (s *handlerCacheStore) List(context.Context) ([]cachestore.Record, error) {
	records := make([]cachestore.Record, 0, len(s.records))
	for _, record := range s.records {
		records = append(records, record)
	}
	return records, nil
}

func (s *handlerCacheStore) Delete(_ context.Context, stage, cacheKey string) error {
	for key, record := range s.records {
		if strings.TrimSpace(record.Stage) == strings.TrimSpace(stage) && strings.TrimSpace(record.CacheKey) == strings.TrimSpace(cacheKey) {
			delete(s.records, key)
		}
	}
	return nil
}

func (s *handlerCacheStore) DeleteForOwner(_ context.Context, stage, cacheKey, ownerPrincipalID string) error {
	delete(s.records, handlerCacheStoreOwnerKey(stage, cacheKey, ownerPrincipalID))
	return nil
}

func handlerCacheStoreKey(stage, cacheKey string) string {
	return strings.TrimSpace(stage) + "\x00" + strings.TrimSpace(cacheKey)
}

func handlerCacheStoreOwnerKey(stage, cacheKey, ownerPrincipalID string) string {
	return handlerCacheStoreKey(stage, cacheKey) + "\x00" + strings.TrimSpace(ownerPrincipalID)
}

type handlerCachePeerRecordOptions struct {
	Stage              string
	CacheKey           string
	ParentCacheKey     string
	StorageRef         string
	SnapshotGUID       string
	ParentSnapshotGUID string
	ProducerVersion    string
}

func insertHandlerCachePeerRecord(t *testing.T, store *handlerCacheStore, opts handlerCachePeerRecordOptions) {
	t.Helper()
	producerVersion := opts.ProducerVersion
	if producerVersion == "" {
		producerVersion = "cleanroom/workspace-stage-v1"
	}
	metadata, err := volumestore.EncodeZFSDriverMetadata(volumestore.ZFSDriverMetadata{
		Dataset:            opts.StorageRef,
		SnapshotGUID:       opts.SnapshotGUID,
		ParentSnapshotGUID: opts.ParentSnapshotGUID,
	})
	if err != nil {
		t.Fatalf("encode zfs metadata: %v", err)
	}
	if err := store.Upsert(context.Background(), cachestore.Record{
		CacheKey:          opts.CacheKey,
		Stage:             opts.Stage,
		State:             "ready",
		BackingSnapshotID: opts.CacheKey + "-snapshot",
		Backend:           "firecracker",
		Architecture:      runtime.GOARCH,
		PolicyHash:        "policy-hash",
		ParentCacheKey:    opts.ParentCacheKey,
		StorageDriver:     "zfs",
		StorageRef:        opts.StorageRef,
		DriverMetadata:    metadata,
		ProducerVersion:   producerVersion,
	}); err != nil {
		t.Fatalf("insert handler cache record: %v", err)
	}
}

var _ volumestore.IncrementalSnapshotTransferDriver = (*handlerCachePeerTransferDriver)(nil)
