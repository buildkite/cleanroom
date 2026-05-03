package controlservice

import (
	"bytes"
	"context"
	"io"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/cachestore"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"github.com/buildkite/cleanroom/internal/volumestore"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type stubCachePeerTransferDriver struct {
	describeSnapshots map[string]volumestore.SnapshotDescription
	describeRequests  []volumestore.DescribeSnapshotRequest
	planRequests      []volumestore.IncrementalSnapshotExportRequest
	exportPlans       []volumestore.IncrementalSnapshotExportPlan
	exportPayload     string
	exportFn          func(context.Context, volumestore.IncrementalSnapshotExportPlan, io.Writer) error
	importErr         error
	importRequests    []volumestore.IncrementalSnapshotImportRequest
	importPayloads    []string
	importSnapshots   map[string]volumestore.Snapshot
	importSnapshot    volumestore.Snapshot
}

func (d *stubCachePeerTransferDriver) DescribeSnapshot(_ context.Context, req volumestore.DescribeSnapshotRequest) (volumestore.SnapshotDescription, error) {
	d.describeRequests = append(d.describeRequests, req)
	if d.describeSnapshots != nil {
		if desc, ok := d.describeSnapshots[req.SnapshotRef]; ok {
			return desc, nil
		}
		if desc, ok := d.describeSnapshots[req.StorageRef]; ok {
			return desc, nil
		}
	}
	return volumestore.SnapshotDescription{}, nil
}

func (d *stubCachePeerTransferDriver) PlanIncrementalSnapshotExport(_ context.Context, req volumestore.IncrementalSnapshotExportRequest) (volumestore.IncrementalSnapshotExportPlan, error) {
	d.planRequests = append(d.planRequests, req)
	return volumestore.IncrementalSnapshotExportPlan{
		FromSnapshotRef:  req.FromSnapshotRef,
		FromSnapshotGUID: req.FromSnapshotGUID,
		ToSnapshotRef:    req.ToSnapshotRef,
		ToSnapshotGUID:   req.ToSnapshotGUID,
		EstimatedBytes:   1234,
	}, nil
}

func (d *stubCachePeerTransferDriver) ExportIncrementalSnapshot(ctx context.Context, plan volumestore.IncrementalSnapshotExportPlan, dst io.Writer) error {
	d.exportPlans = append(d.exportPlans, plan)
	if d.exportFn != nil {
		return d.exportFn(ctx, plan, dst)
	}
	_, err := io.WriteString(dst, d.exportPayload)
	return err
}

func (d *stubCachePeerTransferDriver) ImportIncrementalSnapshot(_ context.Context, req volumestore.IncrementalSnapshotImportRequest, src io.Reader) (volumestore.Snapshot, error) {
	d.importRequests = append(d.importRequests, req)
	payload, err := io.ReadAll(src)
	if err != nil {
		return volumestore.Snapshot{}, err
	}
	if d.importErr != nil {
		return volumestore.Snapshot{}, d.importErr
	}
	d.importPayloads = append(d.importPayloads, string(payload))
	if d.importSnapshots != nil {
		if snapshot, ok := d.importSnapshots[req.ExpectedSnapshotGUID]; ok {
			return snapshot, nil
		}
	}
	if strings.TrimSpace(d.importSnapshot.StorageRef) != "" || strings.TrimSpace(d.importSnapshot.DriverMetadata) != "" {
		return d.importSnapshot, nil
	}
	return volumestore.Snapshot{}, nil
}

func TestLookupCachePeerDependencyStageMintsBoundExportToken(t *testing.T) {
	store := newMemoryCacheStore()
	insertCachePeerRecord(t, store, cachePeerRecordOptions{
		Stage:        workspaceStageName,
		CacheKey:     "workspace-parent",
		StorageRef:   "tank/cleanroom/snapshots/workspace@base",
		SnapshotGUID: "parent-guid",
	})
	insertCachePeerRecord(t, store, cachePeerRecordOptions{
		Stage:              dependencyStageName,
		CacheKey:           "dependency-child",
		ParentCacheKey:     "workspace-parent",
		StorageRef:         "tank/cleanroom/snapshots/dependency@base",
		SnapshotGUID:       "child-guid",
		ParentSnapshotGUID: "parent-guid",
		ProducerVersion:    dependencyStageProducerVersion,
	})
	transfer := &stubCachePeerTransferDriver{exportPayload: "zfs-stream"}
	svc := newTestService(&stubAdapter{})
	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	tracerProvider := sdktrace.NewTracerProvider()
	defer func() {
		_ = meterProvider.Shutdown(context.Background())
		_ = tracerProvider.Shutdown(context.Background())
	}()
	obs, err := observability.NewWithProviders(tracerProvider, meterProvider)
	if err != nil {
		t.Fatalf("NewWithProviders returned error: %v", err)
	}
	svc.Observability = obs
	svc.CacheStore = store
	svc.CachePeerTransferDriver = transfer
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	svc.runtime.clock = stubClock{now: now}
	svc.runtime.cachePeerExportTokenTTL = time.Minute

	resp, err := svc.LookupCachePeer(context.Background(), dependencyCachePeerLookupRequest())
	if err != nil {
		t.Fatalf("LookupCachePeer returned error: %v", err)
	}
	candidate := resp.GetCandidate()
	if candidate == nil {
		t.Fatalf("expected cache peer candidate, miss reason %q", resp.GetMissReason())
	}
	if got, want := candidate.GetTransferToken(), ""; got == want {
		t.Fatal("expected transfer token")
	}
	if got, want := candidate.GetZfsSnapshotGuid(), "child-guid"; got != want {
		t.Fatalf("unexpected child guid: got %q want %q", got, want)
	}
	if got, want := candidate.GetZfsParentSnapshotGuid(), "parent-guid"; got != want {
		t.Fatalf("unexpected parent guid: got %q want %q", got, want)
	}
	if got, want := candidate.GetParentStage(), workspaceStageName; got != want {
		t.Fatalf("unexpected parent stage: got %q want %q", got, want)
	}
	if got, want := candidate.GetEstimatedBytes(), int64(1234); got != want {
		t.Fatalf("unexpected estimated bytes: got %d want %d", got, want)
	}
	if got, want := candidate.GetExpiresAt().AsTime(), now.Add(time.Minute); !got.Equal(want) {
		t.Fatalf("unexpected expiry: got %s want %s", got, want)
	}
	if got, want := transfer.planRequests, []volumestore.IncrementalSnapshotExportRequest{{
		FromSnapshotRef:  "tank/cleanroom/snapshots/workspace@base",
		FromSnapshotGUID: "parent-guid",
		ToSnapshotRef:    "tank/cleanroom/snapshots/dependency@base",
		ToSnapshotGUID:   "child-guid",
	}}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("unexpected plan requests: got %#v want %#v", got, want)
	}

	var stream bytes.Buffer
	if err := svc.ExportCachePeerZFSIncremental(context.Background(), candidate.GetTransferToken(), &stream); err != nil {
		t.Fatalf("ExportCachePeerZFSIncremental returned error: %v", err)
	}
	if got, want := stream.String(), "zfs-stream"; got != want {
		t.Fatalf("unexpected exported stream: got %q want %q", got, want)
	}
	if got, want := len(transfer.planRequests), 2; got != want {
		t.Fatalf("expected export to revalidate the candidate, plan calls got %d want %d", got, want)
	}
	if err := svc.ExportCachePeerZFSIncremental(context.Background(), candidate.GetTransferToken(), &stream); err != ErrCachePeerExportTokenNotFound {
		t.Fatalf("expected single-use token error, got %v", err)
	}

	metrics := collectResourceMetrics(t, reader)
	requireInt64SumMetricValue(t, metrics, observability.MetricCachePeerLookupTotal, map[string]string{
		observability.MetricLabelStage:     dependencyStageName,
		observability.MetricLabelDirection: observability.CachePeerLookupDirectionInbound,
		observability.MetricLabelResult:    observability.CacheResultHit,
	}, 1)
	requireInt64SumMetricValue(t, metrics, observability.MetricCachePeerTransferBytesTotal, map[string]string{
		observability.MetricLabelStage:     dependencyStageName,
		observability.MetricLabelDirection: observability.CachePeerDirectionExport,
		observability.MetricLabelResult:    observability.CacheResultExported,
	}, int64(len("zfs-stream")))
	requireHistogramMetricCount(t, metrics, observability.MetricCachePeerTransferDuration, map[string]string{
		observability.MetricLabelStage:     dependencyStageName,
		observability.MetricLabelDirection: observability.CachePeerDirectionExport,
		observability.MetricLabelResult:    observability.CacheResultExported,
	}, 1)
}

func TestLookupCachePeerMetricNormalizesUnsupportedStage(t *testing.T) {
	svc := newTestService(&stubAdapter{})
	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	tracerProvider := sdktrace.NewTracerProvider()
	defer func() {
		_ = meterProvider.Shutdown(context.Background())
		_ = tracerProvider.Shutdown(context.Background())
	}()
	obs, err := observability.NewWithProviders(tracerProvider, meterProvider)
	if err != nil {
		t.Fatalf("NewWithProviders returned error: %v", err)
	}
	svc.Observability = obs

	req := dependencyCachePeerLookupRequest()
	req.Stage = "workspace:" + strings.Repeat("x", 64)
	resp, err := svc.LookupCachePeer(context.Background(), req)
	if err != nil {
		t.Fatalf("LookupCachePeer returned error: %v", err)
	}
	if got, want := resp.GetMissReason(), cachePeerMissUnsupportedStage; got != want {
		t.Fatalf("unexpected miss reason: got %q want %q", got, want)
	}

	metrics := collectResourceMetrics(t, reader)
	requireInt64SumMetricValue(t, metrics, observability.MetricCachePeerLookupTotal, map[string]string{
		observability.MetricLabelStage:     "unsupported",
		observability.MetricLabelDirection: observability.CachePeerLookupDirectionInbound,
		observability.MetricLabelResult:    observability.CacheResultMiss,
	}, 1)
}

func TestExportCachePeerZFSIncrementalLimitsConcurrentExports(t *testing.T) {
	store := newMemoryCacheStore()
	insertCachePeerRecord(t, store, cachePeerRecordOptions{
		Stage:        workspaceStageName,
		CacheKey:     "workspace-parent",
		StorageRef:   "tank/cleanroom/snapshots/workspace@base",
		SnapshotGUID: "parent-guid",
	})
	insertCachePeerRecord(t, store, cachePeerRecordOptions{
		Stage:              dependencyStageName,
		CacheKey:           "dependency-child",
		ParentCacheKey:     "workspace-parent",
		StorageRef:         "tank/cleanroom/snapshots/dependency@base",
		SnapshotGUID:       "child-guid",
		ParentSnapshotGUID: "parent-guid",
		ProducerVersion:    dependencyStageProducerVersion,
	})

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var active atomic.Int32
	transfer := &stubCachePeerTransferDriver{
		exportFn: func(_ context.Context, _ volumestore.IncrementalSnapshotExportPlan, dst io.Writer) error {
			if got := active.Add(1); got > 1 {
				t.Errorf("expected one active export at a time, got %d", got)
			}
			started <- struct{}{}
			<-release
			active.Add(-1)
			_, err := io.WriteString(dst, "zfs-stream")
			return err
		},
	}
	svc := newTestService(&stubAdapter{})
	svc.CacheStore = store
	svc.CachePeerTransferDriver = transfer
	svc.runtime.cachePeerExportConcurrency = 1

	first, err := svc.LookupCachePeer(context.Background(), dependencyCachePeerLookupRequest())
	if err != nil {
		t.Fatalf("first LookupCachePeer returned error: %v", err)
	}
	second, err := svc.LookupCachePeer(context.Background(), dependencyCachePeerLookupRequest())
	if err != nil {
		t.Fatalf("second LookupCachePeer returned error: %v", err)
	}

	errCh := make(chan error, 2)
	go func() {
		var stream bytes.Buffer
		errCh <- svc.ExportCachePeerZFSIncremental(context.Background(), first.GetCandidate().GetTransferToken(), &stream)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first export to start")
	}

	go func() {
		var stream bytes.Buffer
		errCh <- svc.ExportCachePeerZFSIncremental(context.Background(), second.GetCandidate().GetTransferToken(), &stream)
	}()
	select {
	case <-started:
		t.Fatal("second export started before first export released its slot")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	for range 2 {
		if err := <-errCh; err != nil {
			t.Fatalf("ExportCachePeerZFSIncremental returned error: %v", err)
		}
	}
}

func TestLookupCachePeerSupportsServicesStageWithExplicitParent(t *testing.T) {
	store := newMemoryCacheStore()
	insertCachePeerRecord(t, store, cachePeerRecordOptions{
		Stage:        dependencyStageName,
		CacheKey:     "dependency-parent",
		StorageRef:   "tank/cleanroom/snapshots/dependency@base",
		SnapshotGUID: "parent-guid",
	})
	insertCachePeerRecord(t, store, cachePeerRecordOptions{
		Stage:              servicesStageName,
		CacheKey:           "services-child",
		ParentCacheKey:     "dependency-parent",
		StorageRef:         "tank/cleanroom/snapshots/services@base",
		SnapshotGUID:       "child-guid",
		ParentSnapshotGUID: "parent-guid",
		ProducerVersion:    servicesStageProducerVersion,
	})
	transfer := &stubCachePeerTransferDriver{}
	svc := newTestService(&stubAdapter{})
	svc.CacheStore = store
	svc.CachePeerTransferDriver = transfer

	req := dependencyCachePeerLookupRequest()
	req.Stage = servicesStageName
	req.CacheKey = "services-child"
	req.ProducerVersion = servicesStageProducerVersion
	req.ParentStage = dependencyStageName
	req.ParentCacheKey = "dependency-parent"
	resp, err := svc.LookupCachePeer(context.Background(), req)
	if err != nil {
		t.Fatalf("LookupCachePeer returned error: %v", err)
	}
	candidate := resp.GetCandidate()
	if candidate == nil {
		t.Fatalf("expected services cache peer candidate, miss reason %q", resp.GetMissReason())
	}
	if got, want := candidate.GetParentStage(), dependencyStageName; got != want {
		t.Fatalf("unexpected parent stage: got %q want %q", got, want)
	}
	if got, want := transfer.planRequests, []volumestore.IncrementalSnapshotExportRequest{{
		FromSnapshotRef:  "tank/cleanroom/snapshots/dependency@base",
		FromSnapshotGUID: "parent-guid",
		ToSnapshotRef:    "tank/cleanroom/snapshots/services@base",
		ToSnapshotGUID:   "child-guid",
	}}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("unexpected services plan requests: got %#v want %#v", got, want)
	}
}

func TestLookupCachePeerMissesOnParentGUIDMismatch(t *testing.T) {
	store := newMemoryCacheStore()
	insertCachePeerRecord(t, store, cachePeerRecordOptions{
		Stage:        workspaceStageName,
		CacheKey:     "workspace-parent",
		StorageRef:   "tank/cleanroom/snapshots/workspace@base",
		SnapshotGUID: "different-parent-guid",
	})
	insertCachePeerRecord(t, store, cachePeerRecordOptions{
		Stage:              dependencyStageName,
		CacheKey:           "dependency-child",
		ParentCacheKey:     "workspace-parent",
		StorageRef:         "tank/cleanroom/snapshots/dependency@base",
		SnapshotGUID:       "child-guid",
		ParentSnapshotGUID: "different-parent-guid",
		ProducerVersion:    dependencyStageProducerVersion,
	})
	svc := newTestService(&stubAdapter{})
	svc.CacheStore = store
	svc.CachePeerTransferDriver = &stubCachePeerTransferDriver{}

	resp, err := svc.LookupCachePeer(context.Background(), dependencyCachePeerLookupRequest())
	if err != nil {
		t.Fatalf("LookupCachePeer returned error: %v", err)
	}
	if resp.GetCandidate() != nil {
		t.Fatalf("expected miss, got candidate %#v", resp.GetCandidate())
	}
	if got, want := resp.GetMissReason(), cachePeerMissParentGUIDMismatch; got != want {
		t.Fatalf("unexpected miss reason: got %q want %q", got, want)
	}
}

func TestLookupCachePeerMissesWhenCacheStoreUnavailable(t *testing.T) {
	svc := newTestService(&stubAdapter{})
	svc.CacheStore = nil
	svc.CachePeerTransferDriver = &stubCachePeerTransferDriver{}

	resp, err := svc.LookupCachePeer(context.Background(), dependencyCachePeerLookupRequest())
	if err != nil {
		t.Fatalf("LookupCachePeer returned error: %v", err)
	}
	if resp.GetCandidate() != nil {
		t.Fatalf("expected miss, got candidate %#v", resp.GetCandidate())
	}
	if got, want := resp.GetMissReason(), cachePeerMissCacheStoreUnavailable; got != want {
		t.Fatalf("unexpected miss reason: got %q want %q", got, want)
	}
}

func TestLookupCachePeerMissesForIncompatibleRequests(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*cleanroomv1.LookupCachePeerRequest)
		want   string
	}{
		{
			name: "unsupported stage",
			mutate: func(req *cleanroomv1.LookupCachePeerRequest) {
				req.Stage = workspaceStageName
			},
			want: cachePeerMissUnsupportedStage,
		},
		{
			name: "unsupported parent stage",
			mutate: func(req *cleanroomv1.LookupCachePeerRequest) {
				req.ParentStage = dependencyStageName
			},
			want: cachePeerMissUnsupportedParent,
		},
		{
			name: "unsupported backend",
			mutate: func(req *cleanroomv1.LookupCachePeerRequest) {
				req.Backend = "darwin-vz"
			},
			want: cachePeerMissUnsupportedBackend,
		},
		{
			name: "unsupported driver",
			mutate: func(req *cleanroomv1.LookupCachePeerRequest) {
				req.StorageDriver = "file"
			},
			want: cachePeerMissUnsupportedDriver,
		},
		{
			name: "policy mismatch",
			mutate: func(req *cleanroomv1.LookupCachePeerRequest) {
				req.PolicyHash = "different-policy"
			},
			want: cachePeerMissRecordMismatch,
		},
		{
			name:   "transfer unavailable",
			mutate: func(*cleanroomv1.LookupCachePeerRequest) {},
			want:   cachePeerMissTransferUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newMemoryCacheStore()
			insertCachePeerRecord(t, store, cachePeerRecordOptions{
				Stage:        workspaceStageName,
				CacheKey:     "workspace-parent",
				StorageRef:   "tank/cleanroom/snapshots/workspace@base",
				SnapshotGUID: "parent-guid",
			})
			insertCachePeerRecord(t, store, cachePeerRecordOptions{
				Stage:              dependencyStageName,
				CacheKey:           "dependency-child",
				ParentCacheKey:     "workspace-parent",
				StorageRef:         "tank/cleanroom/snapshots/dependency@base",
				SnapshotGUID:       "child-guid",
				ParentSnapshotGUID: "parent-guid",
				ProducerVersion:    dependencyStageProducerVersion,
			})
			svc := newTestService(&stubAdapter{})
			svc.CacheStore = store
			if tt.want != cachePeerMissTransferUnavailable {
				svc.CachePeerTransferDriver = &stubCachePeerTransferDriver{}
			}
			req := dependencyCachePeerLookupRequest()
			tt.mutate(req)

			resp, err := svc.LookupCachePeer(context.Background(), req)
			if err != nil {
				t.Fatalf("LookupCachePeer returned error: %v", err)
			}
			if resp.GetCandidate() != nil {
				t.Fatalf("expected miss, got candidate %#v", resp.GetCandidate())
			}
			if got := resp.GetMissReason(); got != tt.want {
				t.Fatalf("unexpected miss reason: got %q want %q", got, tt.want)
			}
		})
	}
}

func TestExportCachePeerZFSIncrementalRejectsExpiredToken(t *testing.T) {
	store := newMemoryCacheStore()
	insertCachePeerRecord(t, store, cachePeerRecordOptions{
		Stage:        workspaceStageName,
		CacheKey:     "workspace-parent",
		StorageRef:   "tank/cleanroom/snapshots/workspace@base",
		SnapshotGUID: "parent-guid",
	})
	insertCachePeerRecord(t, store, cachePeerRecordOptions{
		Stage:              dependencyStageName,
		CacheKey:           "dependency-child",
		ParentCacheKey:     "workspace-parent",
		StorageRef:         "tank/cleanroom/snapshots/dependency@base",
		SnapshotGUID:       "child-guid",
		ParentSnapshotGUID: "parent-guid",
		ProducerVersion:    dependencyStageProducerVersion,
	})
	svc := newTestService(&stubAdapter{})
	svc.CacheStore = store
	svc.CachePeerTransferDriver = &stubCachePeerTransferDriver{}
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	svc.runtime.clock = stubClock{now: now}
	svc.runtime.cachePeerExportTokenTTL = time.Minute

	resp, err := svc.LookupCachePeer(context.Background(), dependencyCachePeerLookupRequest())
	if err != nil {
		t.Fatalf("LookupCachePeer returned error: %v", err)
	}
	svc.runtime.clock = stubClock{now: now.Add(2 * time.Minute)}

	var stream bytes.Buffer
	if err := svc.ExportCachePeerZFSIncremental(context.Background(), resp.GetCandidate().GetTransferToken(), &stream); err != ErrCachePeerExportTokenNotFound {
		t.Fatalf("expected expired token error, got %v", err)
	}
}

func TestAuthorizeCachePeerBearerUsesConfiguredTokenEnv(t *testing.T) {
	t.Setenv("CLEANROOM_CACHE_PEER_TOKEN", "shared-secret")
	svc := newTestService(&stubAdapter{})
	svc.Config.Cache = runtimeconfig.CacheConfig{
		Peers: []runtimeconfig.CachePeerConfig{{URL: "https://peer.example", TokenEnv: "CLEANROOM_CACHE_PEER_TOKEN"}},
	}

	if err := svc.AuthorizeCachePeerBearer("Bearer shared-secret"); err != nil {
		t.Fatalf("AuthorizeCachePeerBearer returned error: %v", err)
	}
	if err := svc.AuthorizeCachePeerBearer("Bearer wrong-secret"); err != ErrCachePeerUnauthorized {
		t.Fatalf("expected unauthorized error for wrong token, got %v", err)
	}
}

type cachePeerRecordOptions struct {
	Stage              string
	CacheKey           string
	ParentCacheKey     string
	StorageRef         string
	SnapshotGUID       string
	ParentSnapshotGUID string
	ProducerVersion    string
}

func insertCachePeerRecord(t *testing.T, store *memoryCacheStore, opts cachePeerRecordOptions) {
	t.Helper()
	producerVersion := opts.ProducerVersion
	if producerVersion == "" {
		producerVersion = workspaceStageProducerVersion
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
		State:             cacheStateReady,
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
		t.Fatalf("insert cache peer record: %v", err)
	}
}

func dependencyCachePeerLookupRequest() *cleanroomv1.LookupCachePeerRequest {
	return &cleanroomv1.LookupCachePeerRequest{
		Stage:                 dependencyStageName,
		CacheKey:              "dependency-child",
		Backend:               "firecracker",
		StorageDriver:         "zfs",
		Architecture:          runtime.GOARCH,
		ProducerVersion:       dependencyStageProducerVersion,
		PolicyHash:            "policy-hash",
		ParentStage:           workspaceStageName,
		ParentCacheKey:        "workspace-parent",
		ParentZfsSnapshotGuid: "parent-guid",
	}
}
