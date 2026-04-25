package controlservice

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/cachestore"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/paths"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"github.com/buildkite/cleanroom/internal/repositorystore"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"github.com/buildkite/cleanroom/internal/snapshotstore"
	"go.jetify.com/typeid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type stubAdapter struct {
	result                     *backend.ExecutionResult
	runFn                      func(context.Context, backend.ExecutionRequest) (*backend.ExecutionResult, error)
	runStreamFn                func(context.Context, backend.ExecutionRequest, backend.OutputStream) (*backend.ExecutionResult, error)
	provisionFn                func(context.Context, backend.ProvisionRequest) error
	provisionFromSnapshotFn    func(context.Context, backend.ProvisionFromSnapshotRequest) error
	createSnapshotFn           func(context.Context, backend.SnapshotRequest) (*backend.SnapshotResult, error)
	deleteSnapshotFn           func(context.Context, backend.DeleteSnapshotRequest) error
	terminateFn                func(context.Context, string) error
	downloadFn                 func(context.Context, string, string, int64) ([]byte, error)
	req                        backend.ExecutionRequest
	provisionReq               backend.ProvisionRequest
	provisionFromSnapshotReq   backend.ProvisionFromSnapshotRequest
	createSnapshotReq          backend.SnapshotRequest
	deleteSnapshotReq          backend.DeleteSnapshotRequest
	runCalls                   int
	provisionCalls             int
	provisionFromSnapshotCalls int
	createSnapshotCalls        int
	deleteSnapshotCalls        int
	terminateCalls             int
	runtimeBaseKeyOverride     string
	runtimeBaseKeyErr          error
}

func (s *stubAdapter) Name() string { return "stub" }

func (s *stubAdapter) ProvisionSandbox(ctx context.Context, req backend.ProvisionRequest) error {
	s.provisionReq = req
	s.provisionCalls++
	if s.provisionFn != nil {
		return s.provisionFn(ctx, req)
	}
	return nil
}

func (s *stubAdapter) RunInSandbox(ctx context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
	s.req = req
	s.runCalls++
	if s.runStreamFn != nil {
		return s.runStreamFn(ctx, req, stream)
	}
	var (
		result *backend.ExecutionResult
		err    error
	)
	if s.runFn != nil {
		result, err = s.runFn(ctx, req)
		if err != nil {
			return nil, err
		}
	} else {
		result = s.result
	}
	if result == nil {
		result = &backend.ExecutionResult{
			ExecutionID: req.ExecutionID,
			ExitCode:    0,
			LaunchedVM:  true,
			PlanPath:    "/tmp/plan",
			RunDir:      "/tmp/run",
			Message:     "ok",
		}
	}
	if stream.OnStdout != nil && result.Stdout != "" {
		stream.OnStdout([]byte(result.Stdout))
	}
	if stream.OnStderr != nil && result.Stderr != "" {
		stream.OnStderr([]byte(result.Stderr))
	}
	return result, nil
}

func (s *stubAdapter) CreateSnapshot(ctx context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
	s.createSnapshotReq = req
	s.createSnapshotCalls++
	if s.createSnapshotFn != nil {
		return s.createSnapshotFn(ctx, req)
	}
	return &backend.SnapshotResult{StorageRef: "/tmp/snapshot.ext4"}, nil
}

func (s *stubAdapter) ProvisionSandboxFromSnapshot(ctx context.Context, req backend.ProvisionFromSnapshotRequest) error {
	s.provisionFromSnapshotReq = req
	s.provisionFromSnapshotCalls++
	if s.provisionFromSnapshotFn != nil {
		return s.provisionFromSnapshotFn(ctx, req)
	}
	return nil
}

func (s *stubAdapter) DeleteSnapshot(ctx context.Context, req backend.DeleteSnapshotRequest) error {
	s.deleteSnapshotReq = req
	s.deleteSnapshotCalls++
	if s.deleteSnapshotFn != nil {
		return s.deleteSnapshotFn(ctx, req)
	}
	return nil
}

func (s *stubAdapter) TerminateSandbox(ctx context.Context, sandboxID string) error {
	s.terminateCalls++
	if s.terminateFn != nil {
		return s.terminateFn(ctx, sandboxID)
	}
	return nil
}

func (s *stubAdapter) DownloadSandboxFile(ctx context.Context, sandboxID, path string, maxBytes int64) ([]byte, error) {
	if s.downloadFn != nil {
		return s.downloadFn(ctx, sandboxID, path, maxBytes)
	}
	return nil, errors.New("download not configured")
}

func (s *stubAdapter) RuntimeBaseKey(_ context.Context, _ *policy.CompiledPolicy, _ backend.FirecrackerConfig) (string, error) {
	if s.runtimeBaseKeyErr != nil {
		return "", s.runtimeBaseKeyErr
	}
	if strings.TrimSpace(s.runtimeBaseKeyOverride) != "" {
		return s.runtimeBaseKeyOverride, nil
	}
	return "runtime-base:test", nil
}

type stubLoader struct {
	compiled *policy.CompiledPolicy
	source   string
}

type stubRepositoryMirrorStore struct {
	remoteURL         string
	commitSHA         string
	calls             int
	err               error
	ensureContainsFn  func(remoteURL, commitSHA string) error
	mirrorPath        string
	mirrorPathCalls   int
	mirrorPathErr     error
	ensureMirrorCalls int
	ensureMirrorErr   error
}

type stubClock struct {
	now time.Time
}

func (s *stubRepositoryMirrorStore) EnsureCommit(_ context.Context, remoteURL, commitSHA string, _ repositorystore.FetchHints) error {
	s.remoteURL = remoteURL
	s.commitSHA = commitSHA
	s.calls++
	if s.ensureContainsFn != nil {
		return s.ensureContainsFn(remoteURL, commitSHA)
	}
	return s.err
}

func (s *stubRepositoryMirrorStore) EnsureMirrorContains(ctx context.Context, remoteURL, commitSHA string) error {
	return s.EnsureCommit(ctx, remoteURL, commitSHA, repositorystore.FetchHints{})
}

func (s *stubRepositoryMirrorStore) MirrorPath(remoteURL string) (string, error) {
	s.remoteURL = remoteURL
	s.mirrorPathCalls++
	if s.mirrorPathErr != nil {
		return "", s.mirrorPathErr
	}
	return s.mirrorPath, nil
}

func (s *stubRepositoryMirrorStore) EnsureMirror(_ context.Context, remoteURL string) (string, error) {
	s.remoteURL = remoteURL
	s.ensureMirrorCalls++
	if s.ensureMirrorErr != nil {
		return "", s.ensureMirrorErr
	}
	return s.mirrorPath, nil
}

func (s *stubRepositoryMirrorStore) ReadFileAtCommit(ctx context.Context, remoteURL, commitSHA, path string) ([]byte, error) {
	var content []byte
	err := s.WithRepository(ctx, remoteURL, commitSHA, repositorystore.FetchHints{}, func(repoDir string) error {
		cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "show", commitSHA+":"+path)
		output, err := cmd.CombinedOutput()
		if err != nil {
			message := strings.TrimSpace(string(output))
			if message == "" {
				message = err.Error()
			}
			return errors.New(message)
		}
		content = output
		return nil
	})
	if err != nil {
		return nil, err
	}
	return content, nil
}

func (s *stubRepositoryMirrorStore) WithRepository(ctx context.Context, remoteURL, commitSHA string, _ repositorystore.FetchHints, fn func(repoDir string) error) error {
	if fn == nil {
		return errors.New("repository callback is nil")
	}
	repoDir, err := s.MirrorPath(remoteURL)
	if err != nil {
		repoDir, err = s.EnsureMirror(ctx, remoteURL)
		if err != nil {
			return err
		}
	}
	if err := fn(repoDir); err == nil || strings.TrimSpace(commitSHA) == "" {
		return err
	}
	if err := s.EnsureCommit(ctx, remoteURL, commitSHA, repositorystore.FetchHints{}); err != nil {
		return err
	}
	repoDir, err = s.MirrorPath(remoteURL)
	if err != nil {
		repoDir, err = s.EnsureMirror(ctx, remoteURL)
		if err != nil {
			return err
		}
	}
	return fn(repoDir)
}

func (s *stubRepositoryMirrorStore) TransportHints(context.Context, string, string, repositorystore.FetchHints) (repositorystore.TransportHints, error) {
	return repositorystore.TransportHints{}, nil
}

func (c stubClock) Now() time.Time {
	return c.now
}

func (c stubClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- c.now.Add(d)
	return ch
}

func (l stubLoader) LoadAndCompile(_ string) (*policy.CompiledPolicy, string, error) {
	return l.compiled, l.source, nil
}

func testPolicy() *cleanroomv1.Policy {
	return &cleanroomv1.Policy{
		Version:        1,
		ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		NetworkDefault: "deny",
	}
}

func testRepositoryPolicy() *cleanroomv1.Policy {
	return &cleanroomv1.Policy{
		Version:        1,
		ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		NetworkDefault: "deny",
		Allow: []*cleanroomv1.PolicyAllowRule{
			{Host: "github.com", Ports: []int32{443}},
		},
	}
}

func testRepositoryDependencyPolicy() *cleanroomv1.Policy {
	policyProto := testRepositoryPolicy()
	policyProto.Dependencies = &cleanroomv1.PolicyDependencies{
		Command: []string{"mise", "exec", "--", "go", "mod", "download"},
		Key: &cleanroomv1.PolicyDependencyKey{
			Files: []string{"go.mod", "go.sum"},
		},
	}
	return policyProto
}

func testRepositoryDependencyAndServicesPolicy() *cleanroomv1.Policy {
	policyProto := testRepositoryDependencyPolicy()
	policyProto.Services = &cleanroomv1.PolicyServices{
		Docker:  &cleanroomv1.PolicyDockerService{Required: true},
		Command: []string{"docker", "compose", "up", "-d", "postgres"},
		Key: &cleanroomv1.PolicyDependencyKey{
			Files: []string{"docker-compose.yml"},
		},
	}

	return policyProto
}

func testRepositoryPortableDependencyPolicy() *cleanroomv1.Policy {
	policyProto := testRepositoryDependencyPolicy()
	policyProto.Dependencies.Reuse = policy.DependencyReusePortable

	return policyProto
}

func testRepositoryRunBeforePolicy() *cleanroomv1.Policy {
	policyProto := testRepositoryPolicy()
	policyProto.Run = &cleanroomv1.PolicyRun{
		Before: []string{"sh", "-lc", "echo pre-run"},
	}
	return policyProto
}

func newTestService(adapter backend.Adapter) *Service {
	return newTestServiceWithSnapshotStore(adapter, nil)
}

func newTestServiceWithSnapshotStore(adapter backend.Adapter, store snapshotMetadataStore) *Service {
	if store == nil {
		store = newMemorySnapshotStore()
	}
	return &Service{
		Loader: stubLoader{
			compiled: &policy.CompiledPolicy{
				Version:        1,
				NetworkDefault: "deny",
				ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			},
			source: "/repo/cleanroom.yaml",
		},
		Config: runtimeconfig.Config{
			DefaultBackend: "firecracker",
			Backends: runtimeconfig.Backends{
				Firecracker: runtimeconfig.FirecrackerConfig{
					Snapshots: runtimeconfig.SnapshotConfig{
						Enabled: true,
						Driver:  "file",
					},
				},
			},
		},
		Backends:      map[string]backend.Adapter{"firecracker": adapter},
		SnapshotStore: store,
		CacheStore:    newMemoryCacheStore(),
	}
}

func findEndedSpanByName(spans []trace.ReadOnlySpan, name string) trace.ReadOnlySpan {
	for _, span := range spans {
		if span.Name() == name {
			return span
		}
	}
	return nil
}

func spanAttributeValue(span trace.ReadOnlySpan, key string) (string, bool) {
	if span == nil {
		return "", false
	}
	for _, kv := range span.Attributes() {
		if string(kv.Key) != key {
			continue
		}
		return fmt.Sprint(kv.Value.AsInterface()), true
	}
	return "", false
}

func requireSpanAttributeValue(t *testing.T, span trace.ReadOnlySpan, key, want string) {
	t.Helper()
	got, ok := spanAttributeValue(span, key)
	if !ok {
		t.Fatalf("expected span %q to include attribute %q", span.Name(), key)
	}
	if got != want {
		t.Fatalf("unexpected span %q attribute %q: got %q want %q", span.Name(), key, got, want)
	}
}

func requireSpanMissingAttribute(t *testing.T, span trace.ReadOnlySpan, key string) {
	t.Helper()
	if got, ok := spanAttributeValue(span, key); ok {
		t.Fatalf("expected span %q to omit attribute %q, got %q", span.Name(), key, got)
	}
}

func collectResourceMetrics(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var metrics metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &metrics); err != nil {
		t.Fatalf("Collect returned error: %v", err)
	}
	return metrics
}

func requireInt64SumMetricValue(t *testing.T, metrics metricdata.ResourceMetrics, name string, attrs map[string]string, want int64) {
	t.Helper()
	for _, scopeMetrics := range metrics.ScopeMetrics {
		for _, metric := range scopeMetrics.Metrics {
			if metric.Name != name {
				continue
			}
			sum, ok := metric.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric %q had unexpected data type %T", name, metric.Data)
			}
			for _, point := range sum.DataPoints {
				if metricAttributesMatch(point.Attributes, attrs) {
					if point.Value != want {
						t.Fatalf("metric %q had value %d, want %d", name, point.Value, want)
					}
					return
				}
			}
		}
	}
	t.Fatalf("metric %q with attrs %#v not found", name, attrs)
}

func requireHistogramMetricCount(t *testing.T, metrics metricdata.ResourceMetrics, name string, attrs map[string]string, want uint64) {
	t.Helper()
	for _, scopeMetrics := range metrics.ScopeMetrics {
		for _, metric := range scopeMetrics.Metrics {
			if metric.Name != name {
				continue
			}
			histogram, ok := metric.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("metric %q had unexpected data type %T", name, metric.Data)
			}
			for _, point := range histogram.DataPoints {
				if metricAttributesMatch(point.Attributes, attrs) {
					if point.Count != want {
						t.Fatalf("metric %q had count %d, want %d", name, point.Count, want)
					}
					return
				}
			}
		}
	}
	t.Fatalf("metric %q with attrs %#v not found", name, attrs)
}

func metricAttributesMatch(set attribute.Set, want map[string]string) bool {
	if len(want) == 0 {
		return true
	}
	got := map[string]string{}
	for _, kv := range set.ToSlice() {
		got[string(kv.Key)] = kv.Value.AsString()
	}
	for key, wantValue := range want {
		if got[key] != wantValue {
			return false
		}
	}
	return true
}

func testRetentionPolicy() *retentionPolicy {
	retention := defaultRetentionPolicy
	return &retention
}

func testRepositoryCheckoutProto() *cleanroomv1.RepositoryCheckout {
	return &cleanroomv1.RepositoryCheckout{
		RemoteUrl:      "https://github.com/buildkite/cleanroom.git",
		CommitSha:      "0123456789abcdef0123456789abcdef01234567",
		DestinationDir: "/workspace",
		Submodules:     true,
	}
}

func TestCreateSandboxTracesBootstrapFailure(t *testing.T) {
	t.Parallel()

	recorder := tracetest.NewSpanRecorder()
	tracerProvider := trace.NewTracerProvider()
	tracerProvider.RegisterSpanProcessor(recorder)
	defer func() {
		_ = tracerProvider.Shutdown(context.Background())
	}()

	parentCtx, parentSpan := tracerProvider.Tracer("test").Start(context.Background(), "cleanroom.parent")
	parentSpanContext := parentSpan.SpanContext()

	runCount := 0
	adapter := &stubAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			runCount++
			switch runCount {
			case 1:
				if stream.OnStdout != nil {
					stream.OnStdout([]byte("repository bootstrap ok\n"))
				}
				return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, LaunchedVM: true, Message: "ok"}, nil
			case 2:
				message := "dns error: failed to lookup address information: Try again\n"
				if stream.OnStderr != nil {
					stream.OnStderr([]byte(message))
				}
				return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 1, LaunchedVM: true, Stderr: message, Message: strings.TrimSpace(message)}, nil
			default:
				t.Fatalf("unexpected bootstrap run %d for command %q", runCount, req.Command)
				return nil, nil
			}
		},
	}
	svc := newTestService(adapter)
	obs, err := observability.NewWithTracerProvider(tracerProvider)
	if err != nil {
		t.Fatalf("NewWithTracerProvider returned error: %v", err)
	}
	svc.Observability = obs

	mirrors, repository := testRepositoryMirror(t, map[string]string{
		".mise.toml": "[tools]\ngo = '1.25.9'\n",
		"go.mod":     "module example.com/test\n",
		"go.sum":     "",
	})
	svc.RepositoryStore = mirrors

	_, err = svc.CreateSandbox(parentCtx, &cleanroomv1.CreateSandboxRequest{
		Backend:            "firecracker",
		Policy:             testRepositoryDependencyPolicy(),
		RepositoryCheckout: repository,
	})
	parentSpan.End()
	if err == nil {
		t.Fatal("expected dependency bootstrap failure")
	}
	if !strings.Contains(err.Error(), "dns error") {
		t.Fatalf("unexpected create sandbox error: %v", err)
	}

	spans := recorder.Ended()
	createSpan := findEndedSpanByName(spans, "cleanroom.sandbox.create")
	if createSpan == nil {
		t.Fatalf("expected cleanroom.sandbox.create span, got spans %#v", spans)
	}
	repositorySpan := findEndedSpanByName(spans, "cleanroom.sandbox.bootstrap_repository")
	if repositorySpan == nil {
		t.Fatalf("expected cleanroom.sandbox.bootstrap_repository span, got spans %#v", spans)
	}
	workspaceLookupSpan := findEndedSpanByName(spans, "cleanroom.sandbox.lookup_workspace_stage_cache")
	if workspaceLookupSpan == nil {
		t.Fatalf("expected cleanroom.sandbox.lookup_workspace_stage_cache span, got spans %#v", spans)
	}
	dependencyLookupSpan := findEndedSpanByName(spans, "cleanroom.sandbox.lookup_dependency_stage_cache")
	if dependencyLookupSpan == nil {
		t.Fatalf("expected cleanroom.sandbox.lookup_dependency_stage_cache span, got spans %#v", spans)
	}
	dependencySpan := findEndedSpanByName(spans, "cleanroom.sandbox.bootstrap_dependencies")
	if dependencySpan == nil {
		t.Fatalf("expected cleanroom.sandbox.bootstrap_dependencies span, got spans %#v", spans)
	}

	if got, want := createSpan.SpanContext().TraceID(), parentSpanContext.TraceID(); got != want {
		t.Fatalf("unexpected create sandbox trace id: got %s want %s", got, want)
	}
	if got, want := createSpan.Parent().SpanID(), parentSpanContext.SpanID(); got != want {
		t.Fatalf("unexpected create sandbox parent span id: got %s want %s", got, want)
	}
	if got, want := repositorySpan.Parent().SpanID(), createSpan.SpanContext().SpanID(); got != want {
		t.Fatalf("unexpected repository bootstrap parent span id: got %s want %s", got, want)
	}
	if got, want := workspaceLookupSpan.Parent().SpanID(), createSpan.SpanContext().SpanID(); got != want {
		t.Fatalf("unexpected workspace lookup parent span id: got %s want %s", got, want)
	}
	if got, want := dependencyLookupSpan.Parent().SpanID(), createSpan.SpanContext().SpanID(); got != want {
		t.Fatalf("unexpected dependency lookup parent span id: got %s want %s", got, want)
	}
	if got, want := dependencySpan.Parent().SpanID(), createSpan.SpanContext().SpanID(); got != want {
		t.Fatalf("unexpected dependency bootstrap parent span id: got %s want %s", got, want)
	}
	if got, want := repositorySpan.Status().Code, codes.Ok; got != want {
		t.Fatalf("unexpected repository bootstrap span status: got %v want %v", got, want)
	}
	if got, want := dependencySpan.Status().Code, codes.Error; got != want {
		t.Fatalf("unexpected dependency bootstrap span status: got %v want %v", got, want)
	}
	if got, want := createSpan.Status().Code, codes.Error; got != want {
		t.Fatalf("unexpected create sandbox span status: got %v want %v", got, want)
	}
	repositoryCommitSHA := repository.GetCommitSha()
	requireSpanAttributeValue(t, createSpan, observability.AttrRepositoryCommitSHA, repositoryCommitSHA)
	requireSpanAttributeValue(t, repositorySpan, observability.AttrRepositoryCommitSHA, repositoryCommitSHA)
	requireSpanAttributeValue(t, dependencySpan, observability.AttrRepositoryCommitSHA, repositoryCommitSHA)
	requireSpanAttributeValue(t, workspaceLookupSpan, observability.AttrRepositoryCommitSHA, repositoryCommitSHA)
	requireSpanAttributeValue(t, dependencyLookupSpan, observability.AttrRepositoryCommitSHA, repositoryCommitSHA)
	requireSpanAttributeValue(t, workspaceLookupSpan, observability.AttrCacheStage, observability.CacheStageWorkspace)
	requireSpanAttributeValue(t, workspaceLookupSpan, observability.AttrCacheOperation, observability.CacheOperationLookup)
	requireSpanAttributeValue(t, workspaceLookupSpan, observability.AttrCacheResult, observability.CacheResultMiss)
	requireSpanAttributeValue(t, workspaceLookupSpan, observability.AttrCacheLookupReason, observability.CacheLookupReasonRecordNotFound)
	requireSpanAttributeValue(t, dependencyLookupSpan, observability.AttrCacheStage, observability.CacheStageDependency)
	requireSpanAttributeValue(t, dependencyLookupSpan, observability.AttrCacheOperation, observability.CacheOperationLookup)
	requireSpanAttributeValue(t, dependencyLookupSpan, observability.AttrCacheResult, observability.CacheResultMiss)
	requireSpanAttributeValue(t, dependencyLookupSpan, observability.AttrCacheLookupReason, observability.CacheLookupReasonRecordNotFound)
}

func TestCreateSandboxTracesDependencyCacheHitAttributes(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"go.mod": "module example.com/test\n\ngo 1.26.2\n",
		"go.sum": "example.com/test v0.0.0 h1:abc123\n",
	})
	svc := newTestServiceWithSnapshotStore(adapter, store)
	svc.RepositoryStore = mirrors

	req := &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryDependencyPolicy(),
		RepositoryCheckout: repositoryCheckout,
	}
	if _, err := svc.CreateSandbox(context.Background(), req); err != nil {
		t.Fatalf("first CreateSandbox returned error: %v", err)
	}
	mirrors.err = errors.New("offline")

	recorder := tracetest.NewSpanRecorder()
	tracerProvider := trace.NewTracerProvider()
	tracerProvider.RegisterSpanProcessor(recorder)
	defer func() {
		_ = tracerProvider.Shutdown(context.Background())
	}()
	obs, err := observability.NewWithTracerProvider(tracerProvider)
	if err != nil {
		t.Fatalf("NewWithTracerProvider returned error: %v", err)
	}
	svc.Observability = obs

	resp, err := svc.CreateSandbox(context.Background(), req)
	if err != nil {
		t.Fatalf("second CreateSandbox returned error: %v", err)
	}
	if got, want := resp.GetSourceKind(), "dependency stage cache"; got != want {
		t.Fatalf("unexpected response source kind: got %q want %q", got, want)
	}

	spans := recorder.Ended()
	lookupSpan := findEndedSpanByName(spans, "cleanroom.sandbox.lookup_dependency_stage_cache")
	if lookupSpan == nil {
		t.Fatalf("expected cleanroom.sandbox.lookup_dependency_stage_cache span, got spans %#v", spans)
	}
	restoreSpan := findEndedSpanByName(spans, "cleanroom.sandbox.restore_dependency_stage_cache")
	if restoreSpan == nil {
		t.Fatalf("expected cleanroom.sandbox.restore_dependency_stage_cache span, got spans %#v", spans)
	}
	if workspaceLookupSpan := findEndedSpanByName(spans, "cleanroom.sandbox.lookup_workspace_stage_cache"); workspaceLookupSpan != nil {
		t.Fatalf("expected dependency cache hit to skip workspace lookup span, got spans %#v", spans)
	}

	repositoryCommitSHA := repositoryCheckout.GetCommitSha()
	requireSpanAttributeValue(t, lookupSpan, observability.AttrRepositoryCommitSHA, repositoryCommitSHA)
	requireSpanAttributeValue(t, lookupSpan, observability.AttrCacheStage, observability.CacheStageDependency)
	requireSpanAttributeValue(t, lookupSpan, observability.AttrCacheOperation, observability.CacheOperationLookup)
	requireSpanAttributeValue(t, lookupSpan, observability.AttrCacheResult, observability.CacheResultHit)
	requireSpanMissingAttribute(t, lookupSpan, observability.AttrCacheLookupReason)
	requireSpanAttributeValue(t, restoreSpan, observability.AttrRepositoryCommitSHA, repositoryCommitSHA)
	requireSpanAttributeValue(t, restoreSpan, observability.AttrCacheStage, observability.CacheStageDependency)
	requireSpanAttributeValue(t, restoreSpan, observability.AttrCacheOperation, observability.CacheOperationRestore)
	requireSpanAttributeValue(t, restoreSpan, observability.AttrCacheResult, observability.CacheResultRestored)
}

func TestCreateSandboxTracesServicesCacheHitAttributes(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"go.mod":             "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":             "example.com/test v0.0.0 h1:abc123\n",
		"docker-compose.yml": "services:\n  postgres:\n    image: postgres:17\n",
	})
	svc := newTestServiceWithSnapshotStore(adapter, store)
	svc.RepositoryStore = mirrors

	req := &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryDependencyAndServicesPolicy(),
		RepositoryCheckout: repositoryCheckout,
	}
	if _, err := svc.CreateSandbox(context.Background(), req); err != nil {
		t.Fatalf("first CreateSandbox returned error: %v", err)
	}
	mirrors.err = errors.New("offline")

	recorder := tracetest.NewSpanRecorder()
	tracerProvider := trace.NewTracerProvider()
	tracerProvider.RegisterSpanProcessor(recorder)
	defer func() {
		_ = tracerProvider.Shutdown(context.Background())
	}()
	obs, err := observability.NewWithTracerProvider(tracerProvider)
	if err != nil {
		t.Fatalf("NewWithTracerProvider returned error: %v", err)
	}
	svc.Observability = obs

	resp, err := svc.CreateSandbox(context.Background(), req)
	if err != nil {
		t.Fatalf("second CreateSandbox returned error: %v", err)
	}
	if got, want := resp.GetSourceKind(), "services stage cache"; got != want {
		t.Fatalf("unexpected response source kind: got %q want %q", got, want)
	}

	spans := recorder.Ended()
	lookupSpan := findEndedSpanByName(spans, "cleanroom.sandbox.lookup_services_stage_cache")
	if lookupSpan == nil {
		t.Fatalf("expected cleanroom.sandbox.lookup_services_stage_cache span, got spans %#v", spans)
	}
	restoreSpan := findEndedSpanByName(spans, "cleanroom.sandbox.restore_services_stage_cache")
	if restoreSpan == nil {
		t.Fatalf("expected cleanroom.sandbox.restore_services_stage_cache span, got spans %#v", spans)
	}
	if dependencyLookupSpan := findEndedSpanByName(spans, "cleanroom.sandbox.lookup_dependency_stage_cache"); dependencyLookupSpan != nil {
		t.Fatalf("expected services cache hit to skip dependency lookup span, got spans %#v", spans)
	}
	if workspaceLookupSpan := findEndedSpanByName(spans, "cleanroom.sandbox.lookup_workspace_stage_cache"); workspaceLookupSpan != nil {
		t.Fatalf("expected services cache hit to skip workspace lookup span, got spans %#v", spans)
	}

	repositoryCommitSHA := repositoryCheckout.GetCommitSha()
	requireSpanAttributeValue(t, lookupSpan, observability.AttrRepositoryCommitSHA, repositoryCommitSHA)
	requireSpanAttributeValue(t, lookupSpan, observability.AttrCacheStage, observability.CacheStageServices)
	requireSpanAttributeValue(t, lookupSpan, observability.AttrCacheOperation, observability.CacheOperationLookup)
	requireSpanAttributeValue(t, lookupSpan, observability.AttrCacheResult, observability.CacheResultHit)
	requireSpanMissingAttribute(t, lookupSpan, observability.AttrCacheLookupReason)
	requireSpanAttributeValue(t, restoreSpan, observability.AttrRepositoryCommitSHA, repositoryCommitSHA)
	requireSpanAttributeValue(t, restoreSpan, observability.AttrCacheStage, observability.CacheStageServices)
	requireSpanAttributeValue(t, restoreSpan, observability.AttrCacheOperation, observability.CacheOperationRestore)
	requireSpanAttributeValue(t, restoreSpan, observability.AttrCacheResult, observability.CacheResultRestored)
}

func TestServiceEmitsSandboxAndExecutionMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	tracerProvider := trace.NewTracerProvider()
	defer func() {
		_ = meterProvider.Shutdown(context.Background())
		_ = tracerProvider.Shutdown(context.Background())
	}()

	obs, err := observability.NewWithProviders(tracerProvider, meterProvider)
	if err != nil {
		t.Fatalf("NewWithProviders returned error: %v", err)
	}

	adapter := &stubAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
			return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, LaunchedVM: true, Message: "ok"}, nil
		},
	}
	svc := newTestService(adapter)
	svc.Observability = obs

	compiled := &policy.CompiledPolicy{
		Version:        1,
		NetworkDefault: "deny",
		ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	sandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Backend: "firecracker",
		Policy:  compiled.ToProto(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	executionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxResp.GetSandbox().GetSandboxId(),
		Command:   []string{"echo", "hello"},
		Kind:      cleanroomv1.ExecutionKind_EXECUTION_KIND_BATCH,
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}

	key := executionKey(sandboxResp.GetSandbox().GetSandboxId(), executionResp.GetExecution().GetExecutionId())
	svc.mu.RLock()
	done := svc.executions[key].Done
	svc.mu.RUnlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for execution to finish")
	}

	metrics := collectResourceMetrics(t, reader)
	requireHistogramMetricCount(t, metrics, "cleanroom_sandbox_create_duration_seconds", map[string]string{
		"backend": "firecracker",
		"source":  "fresh",
		"outcome": "succeeded",
	}, 1)
	requireInt64SumMetricValue(t, metrics, "cleanroom_execution_total", map[string]string{
		"backend": "firecracker",
		"kind":    "batch",
		"outcome": "succeeded",
	}, 1)
	requireHistogramMetricCount(t, metrics, "cleanroom_execution_duration_seconds", map[string]string{
		"backend": "firecracker",
		"kind":    "batch",
		"outcome": "succeeded",
	}, 1)
}

func TestServiceSandboxCreateMetricsTrackWorkspaceCacheFailureSource(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	tracerProvider := trace.NewTracerProvider()
	defer func() {
		_ = meterProvider.Shutdown(context.Background())
		_ = tracerProvider.Shutdown(context.Background())
	}()

	obs, err := observability.NewWithProviders(tracerProvider, meterProvider)
	if err != nil {
		t.Fatalf("NewWithProviders returned error: %v", err)
	}

	store := newMemorySnapshotStore()
	runCalls := 0
	adapter := &stubAdapter{
		createSnapshotFn: func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
			return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
		},
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
			runCalls++
			result := &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, LaunchedVM: true, Message: "ok"}
			if runCalls == 3 {
				result.ExitCode = 1
				result.Message = "dependency bootstrap failed"
				result.Stderr = "dependency bootstrap failed\n"
			}
			return result, nil
		},
	}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"go.mod": "module example.com/test\n\ngo 1.26.2\n",
		"go.sum": "example.com/test v0.0.0 h1:abc123\n",
	})
	svc := newTestServiceWithSnapshotStore(adapter, store)
	svc.Observability = obs
	svc.RepositoryStore = mirrors

	req := &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryDependencyPolicy(),
		RepositoryCheckout: repositoryCheckout,
	}
	if _, err := svc.CreateSandbox(context.Background(), req); err != nil {
		t.Fatalf("first CreateSandbox returned error: %v", err)
	}

	records, err := svc.CacheStore.List(context.Background())
	if err != nil {
		t.Fatalf("List cache records returned error: %v", err)
	}
	for _, record := range records {
		if record.Stage != "dependency" {
			continue
		}
		if err := svc.CacheStore.Delete(context.Background(), record.Stage, record.CacheKey); err != nil {
			t.Fatalf("Delete dependency cache record returned error: %v", err)
		}
	}

	_, err = svc.CreateSandbox(context.Background(), req)
	if err == nil {
		t.Fatal("expected second CreateSandbox to fail during dependency bootstrap")
	}
	if !strings.Contains(err.Error(), "bootstrap dependency stage") {
		t.Fatalf("unexpected CreateSandbox error: %v", err)
	}

	metrics := collectResourceMetrics(t, reader)
	requireHistogramMetricCount(t, metrics, "cleanroom_sandbox_create_duration_seconds", map[string]string{
		"backend": "firecracker",
		"source":  "fresh",
		"outcome": "succeeded",
	}, 1)
	requireHistogramMetricCount(t, metrics, "cleanroom_sandbox_create_duration_seconds", map[string]string{
		"backend": "firecracker",
		"source":  "workspace_cache",
		"outcome": "failed",
	}, 1)
}

func TestServiceSandboxCreateMetricsUseSnapshotBackend(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	tracerProvider := trace.NewTracerProvider()
	defer func() {
		_ = meterProvider.Shutdown(context.Background())
		_ = tracerProvider.Shutdown(context.Background())
	}()

	obs, err := observability.NewWithProviders(tracerProvider, meterProvider)
	if err != nil {
		t.Fatalf("NewWithProviders returned error: %v", err)
	}

	store := newMemorySnapshotStore()
	if err := store.Create(context.Background(), snapshotstore.Record{
		SnapshotID:      "snap-1",
		SourceSandboxID: "sandbox-source",
		Backend:         "darwin-vz",
		PolicyHash:      "policy-hash",
		Policy:          testPolicy(),
		StorageDriver:   "apfs",
		StorageRef:      "/snapshots/snap-1.apfs",
		CreatedAt:       time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Create snapshot record returned error: %v", err)
	}

	adapter := &stubAdapter{}
	svc := &Service{
		Config: runtimeconfig.Config{
			DefaultBackend: "firecracker",
			Backends: runtimeconfig.Backends{
				DarwinVZ: runtimeconfig.DarwinVZConfig{
					Snapshots: runtimeconfig.SnapshotConfig{
						Enabled: true,
						Driver:  "apfs",
					},
				},
			},
		},
		Backends:      map[string]backend.Adapter{"darwin-vz": adapter},
		Observability: obs,
		SnapshotStore: store,
	}

	if _, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Source: &cleanroomv1.CreateSandboxRequest_SnapshotId{SnapshotId: "snap-1"},
	}); err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	metrics := collectResourceMetrics(t, reader)
	requireHistogramMetricCount(t, metrics, "cleanroom_sandbox_create_duration_seconds", map[string]string{
		"backend": "darwin-vz",
		"source":  "snapshot",
		"outcome": "succeeded",
	}, 1)
}

func testRepositoryMirror(t *testing.T, files map[string]string) (*stubRepositoryMirrorStore, *cleanroomv1.RepositoryCheckout) {
	t.Helper()

	repoDir := t.TempDir()
	runTestGit(t, repoDir, "init")
	runTestGit(t, repoDir, "config", "user.email", "test@example.com")
	runTestGit(t, repoDir, "config", "user.name", "Test User")
	for name, contents := range files {
		target := filepath.Join(repoDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) returned error: %v", target, err)
		}
		if err := os.WriteFile(target, []byte(contents), 0o644); err != nil {
			t.Fatalf("WriteFile(%q) returned error: %v", target, err)
		}
	}
	runTestGit(t, repoDir, "add", ".")
	runTestGit(t, repoDir, "commit", "-m", "test")
	commitSHA := strings.TrimSpace(runTestGit(t, repoDir, "rev-parse", "HEAD"))

	return &stubRepositoryMirrorStore{mirrorPath: repoDir}, &cleanroomv1.RepositoryCheckout{
		RemoteUrl:      "https://github.com/buildkite/cleanroom.git",
		CommitSha:      commitSHA,
		DestinationDir: "/workspace",
	}
}

func runTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s returned error: %v\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output)
}

type memorySnapshotStore struct {
	mu      sync.Mutex
	records map[string]snapshotstore.Record
}

func newMemorySnapshotStore() *memorySnapshotStore {
	return &memorySnapshotStore{records: map[string]snapshotstore.Record{}}
}

func (s *memorySnapshotStore) Create(_ context.Context, record snapshotstore.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.records[record.SnapshotID]; exists {
		return errors.New("snapshot already exists")
	}
	s.records[record.SnapshotID] = record
	return nil
}

func (s *memorySnapshotStore) Get(_ context.Context, snapshotID string) (snapshotstore.Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[snapshotID]
	return record, ok, nil
}

func (s *memorySnapshotStore) List(_ context.Context) ([]snapshotstore.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]snapshotstore.Record, 0, len(s.records))
	for _, record := range s.records {
		items = append(items, record)
	}
	return items, nil
}

func (s *memorySnapshotStore) Delete(_ context.Context, snapshotID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, snapshotID)
	return nil
}

type blockingSnapshotStore struct {
	*memorySnapshotStore
	getStarted chan struct{}
	getRelease chan struct{}
}

func (s *blockingSnapshotStore) Get(ctx context.Context, snapshotID string) (snapshotstore.Record, bool, error) {
	select {
	case s.getStarted <- struct{}{}:
	default:
	}
	select {
	case <-s.getRelease:
	case <-ctx.Done():
		return snapshotstore.Record{}, false, ctx.Err()
	}
	return s.memorySnapshotStore.Get(ctx, snapshotID)
}

type memoryCacheStore struct {
	mu      sync.Mutex
	records map[string]cachestore.Record
}

func newMemoryCacheStore() *memoryCacheStore {
	return &memoryCacheStore{records: map[string]cachestore.Record{}}
}

func (s *memoryCacheStore) Create(_ context.Context, record cachestore.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := cacheStoreKey(record.Stage, record.CacheKey)
	if _, exists := s.records[key]; exists {
		return errors.New("cache record already exists")
	}
	s.records[key] = record
	return nil
}

func (s *memoryCacheStore) Upsert(_ context.Context, record cachestore.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[cacheStoreKey(record.Stage, record.CacheKey)] = record
	return nil
}

func (s *memoryCacheStore) GetReady(_ context.Context, stage, cacheKey string) (cachestore.Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[cacheStoreKey(stage, cacheKey)]
	if !ok || record.State != cacheStateReady {
		return cachestore.Record{}, false, nil
	}
	return record, true, nil
}

func (s *memoryCacheStore) Touch(_ context.Context, stage, cacheKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := cacheStoreKey(stage, cacheKey)
	record, ok := s.records[key]
	if !ok {
		return errors.New("cache record not found")
	}
	record.LastUsedAt = time.Now().UTC()
	s.records[key] = record
	return nil
}

func (s *memoryCacheStore) List(_ context.Context) ([]cachestore.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]cachestore.Record, 0, len(s.records))
	for _, record := range s.records {
		items = append(items, record)
	}
	return items, nil
}

func (s *memoryCacheStore) Delete(_ context.Context, stage, cacheKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, cacheStoreKey(stage, cacheKey))
	return nil
}

func cacheStoreKey(stage, cacheKey string) string {
	return strings.TrimSpace(stage) + "\x00" + strings.TrimSpace(cacheKey)
}

func cacheRecordBackingSnapshotID(record cachestore.Record) (string, bool) {
	value := reflect.ValueOf(record)
	for _, fieldName := range []string{"BackingSnapshotID", "SnapshotID", "BackingSnapshotIdentity"} {
		field := value.FieldByName(fieldName)
		if field.IsValid() && field.Kind() == reflect.String {
			return field.String(), true
		}
	}
	return "", false
}

func TestExecutionStreamIncludesExitEvent(t *testing.T) {
	adapter := &stubAdapter{
		runFn: func(_ context.Context, req backend.ExecutionRequest) (*backend.ExecutionResult, error) {
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    7,
				LaunchedVM:  true,
				PlanPath:    "/tmp/plan",
				RunDir:      "/tmp/run",
				ImageRef:    "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				ImageDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				Message:     "done",
				Stdout:      "hello stdout\n",
				Stderr:      "hello stderr\n",
			}, nil
		},
	}
	svc := newTestService(adapter)

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"--", "echo", "hi"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()

	history, updates, done, unsubscribe, err := svc.SubscribeExecutionEvents(sandboxID, executionID)
	if err != nil {
		t.Fatalf("SubscribeExecutionEvents returned error: %v", err)
	}
	defer unsubscribe()

	events := collectExecutionEvents(t, history, updates, done)
	var sawStdout bool
	var sawStderr bool
	var exit *cleanroomv1.ExecutionExit
	for _, event := range events {
		if got, want := event.GetImageDigest(), "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"; got != want {
			t.Fatalf("expected image digest on event, got %q want %q", got, want)
		}
		switch payload := event.Payload.(type) {
		case *cleanroomv1.ExecutionStreamEvent_Stdout:
			if strings.Contains(string(payload.Stdout), "hello stdout") {
				sawStdout = true
			}
		case *cleanroomv1.ExecutionStreamEvent_Stderr:
			if strings.Contains(string(payload.Stderr), "hello stderr") {
				sawStderr = true
			}
		case *cleanroomv1.ExecutionStreamEvent_Exit:
			exit = payload.Exit
		}
	}
	if !sawStdout {
		t.Fatalf("expected stdout event in stream, events=%d", len(events))
	}
	if !sawStderr {
		t.Fatalf("expected stderr event in stream, events=%d", len(events))
	}
	if exit == nil {
		t.Fatalf("expected exit event in stream, events=%d", len(events))
	}
	if got, want := exit.GetExitCode(), int32(7); got != want {
		t.Fatalf("unexpected exit code: got %d want %d", got, want)
	}
	if got, want := exit.GetStatus(), cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED; got != want {
		t.Fatalf("unexpected exit status: got %v want %v", got, want)
	}
}

func TestCreateExecutionRejectsEnvWithoutAssignment(t *testing.T) {
	adapter := &stubAdapter{}
	svc := newTestService(adapter)

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	_, err = svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"echo", "hi"},
		Env:       []string{"OPENAI_API_KEY"},
	})
	if err == nil {
		t.Fatal("expected CreateExecution to reject env entries without KEY=VALUE")
	}
	if !strings.Contains(err.Error(), "KEY=VALUE") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreateSnapshotPersistsMetadataAndDeletesIt(t *testing.T) {
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{
		createSnapshotFn: func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
			return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
		},
	}
	svc := newTestServiceWithSnapshotStore(adapter, store)

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryPolicy(),
		RepositoryCheckout: testRepositoryCheckoutProto(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandbox := createResp.GetSandbox()

	snapshotResp, err := svc.CreateSnapshot(context.Background(), &cleanroomv1.CreateSnapshotRequest{
		SandboxId: sandbox.GetSandboxId(),
		Name:      "golden",
	})
	if err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}

	snapshot := snapshotResp.GetSnapshot()
	if snapshot == nil {
		t.Fatal("expected snapshot in response")
	}
	if got, want := snapshot.GetSourceSandboxId(), sandbox.GetSandboxId(); got != want {
		t.Fatalf("unexpected source sandbox id: got %q want %q", got, want)
	}
	if got, want := snapshot.GetBackend(), "firecracker"; got != want {
		t.Fatalf("unexpected backend: got %q want %q", got, want)
	}
	if got, want := snapshot.GetName(), "golden"; got != want {
		t.Fatalf("unexpected snapshot name: got %q want %q", got, want)
	}
	if got, want := snapshot.GetStorageDriver(), "file"; got != want {
		t.Fatalf("unexpected snapshot storage driver: got %q want %q", got, want)
	}
	if got, want := snapshot.GetStorageRef(), "/snapshots/"+snapshot.GetSnapshotId()+".ext4"; got != want {
		t.Fatalf("unexpected snapshot storage ref: got %q want %q", got, want)
	}
	if snapshot.GetRepositoryCheckout() == nil {
		t.Fatal("expected snapshot repository metadata")
	}
	if got, want := snapshot.GetRepositoryCheckout().GetDestinationDir(), "/workspace"; got != want {
		t.Fatalf("unexpected snapshot repository destination: got %q want %q", got, want)
	}
	if got, want := adapter.createSnapshotReq.SandboxID, sandbox.GetSandboxId(); got != want {
		t.Fatalf("unexpected create snapshot sandbox id: got %q want %q", got, want)
	}
	if adapter.createSnapshotCalls != 2 {
		t.Fatalf("expected two snapshot create calls (workspace stage + manual snapshot), got %d", adapter.createSnapshotCalls)
	}

	getResp, err := svc.GetSnapshot(context.Background(), &cleanroomv1.GetSnapshotRequest{
		SnapshotId: snapshot.GetSnapshotId(),
	})
	if err != nil {
		t.Fatalf("GetSnapshot returned error: %v", err)
	}
	if got, want := getResp.GetSnapshot().GetSnapshotId(), snapshot.GetSnapshotId(); got != want {
		t.Fatalf("unexpected snapshot id from get: got %q want %q", got, want)
	}
	if got, want := getResp.GetSnapshot().GetStorageDriver(), "file"; got != want {
		t.Fatalf("unexpected snapshot storage driver from get: got %q want %q", got, want)
	}
	if got, want := getResp.GetSnapshot().GetRepositoryCheckout().GetCommitSha(), testRepositoryCheckoutProto().GetCommitSha(); got != want {
		t.Fatalf("unexpected snapshot repository commit from get: got %q want %q", got, want)
	}

	listResp, err := svc.ListSnapshots(context.Background(), &cleanroomv1.ListSnapshotsRequest{})
	if err != nil {
		t.Fatalf("ListSnapshots returned error: %v", err)
	}
	if got, want := len(listResp.GetSnapshots()), 1; got != want {
		t.Fatalf("unexpected snapshot count: got %d want %d", got, want)
	}

	deleteResp, err := svc.DeleteSnapshot(context.Background(), &cleanroomv1.DeleteSnapshotRequest{
		SnapshotId: snapshot.GetSnapshotId(),
	})
	if err != nil {
		t.Fatalf("DeleteSnapshot returned error: %v", err)
	}
	if !deleteResp.GetDeleted() {
		t.Fatal("expected snapshot delete to report deleted=true")
	}
	if got, want := adapter.deleteSnapshotReq.SnapshotID, snapshot.GetSnapshotId(); got != want {
		t.Fatalf("unexpected deleted snapshot id: got %q want %q", got, want)
	}

	listResp, err = svc.ListSnapshots(context.Background(), &cleanroomv1.ListSnapshotsRequest{})
	if err != nil {
		t.Fatalf("ListSnapshots after delete returned error: %v", err)
	}
	if got := len(listResp.GetSnapshots()); got != 0 {
		t.Fatalf("expected only the manual snapshot to be deleted from snapshot metadata, got %d snapshots", got)
	}
}

func TestCreateSnapshotRejectsDisabledSnapshots(t *testing.T) {
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{}
	svc := newTestServiceWithSnapshotStore(adapter, store)
	svc.Config.Backends.Firecracker.Snapshots.Enabled = false

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	_, err = svc.CreateSnapshot(context.Background(), &cleanroomv1.CreateSnapshotRequest{
		SandboxId: createResp.GetSandbox().GetSandboxId(),
	})
	if err == nil {
		t.Fatal("expected CreateSnapshot to fail when snapshots are disabled")
	}
	if !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("expected disabled snapshots error, got %v", err)
	}
	if adapter.createSnapshotCalls != 0 {
		t.Fatalf("expected no backend snapshot calls, got %d", adapter.createSnapshotCalls)
	}
}

func TestCreateSnapshotAllowsWorkspaceStageLikeNames(t *testing.T) {
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{}
	svc := newTestServiceWithSnapshotStore(adapter, store)

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	resp, err := svc.CreateSnapshot(context.Background(), &cleanroomv1.CreateSnapshotRequest{
		SandboxId: createResp.GetSandbox().GetSandboxId(),
		Name:      "workspace-stage:manual",
	})
	if err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}
	if got, want := resp.GetSnapshot().GetName(), "workspace-stage:manual"; got != want {
		t.Fatalf("unexpected snapshot name: got %q want %q", got, want)
	}
	if got, want := adapter.createSnapshotCalls, 1; got != want {
		t.Fatalf("unexpected create snapshot call count: got %d want %d", got, want)
	}
}

func TestCreateSnapshotRejectsRepositoryBusySandbox(t *testing.T) {
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{}
	svc := newTestServiceWithSnapshotStore(adapter, store)

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()

	svc.mu.Lock()
	svc.sandboxes[sandboxID].RepositoryBusy = true
	svc.mu.Unlock()

	_, err = svc.CreateSnapshot(context.Background(), &cleanroomv1.CreateSnapshotRequest{
		SandboxId: sandboxID,
	})
	if err == nil {
		t.Fatal("expected CreateSnapshot to fail while repository bootstrap is in progress")
	}
	if !strings.Contains(err.Error(), "preparing repository state") {
		t.Fatalf("unexpected CreateSnapshot error: %v", err)
	}
	if adapter.createSnapshotCalls != 0 {
		t.Fatalf("expected no backend snapshot calls, got %d", adapter.createSnapshotCalls)
	}
}

func TestCreateSandboxFromSnapshotRejectsDisabledSnapshots(t *testing.T) {
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{
		createSnapshotFn: func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
			return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
		},
	}
	svc := newTestServiceWithSnapshotStore(adapter, store)

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	snapshotResp, err := svc.CreateSnapshot(context.Background(), &cleanroomv1.CreateSnapshotRequest{
		SandboxId: createResp.GetSandbox().GetSandboxId(),
	})
	if err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}

	svc.Config.Backends.Firecracker.Snapshots.Enabled = false
	_, err = svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Source: &cleanroomv1.CreateSandboxRequest_SnapshotId{
			SnapshotId: snapshotResp.GetSnapshot().GetSnapshotId(),
		},
	})
	if err == nil {
		t.Fatal("expected CreateSandbox from snapshot to fail when snapshots are disabled")
	}
	if !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("expected disabled snapshots error, got %v", err)
	}
}

func TestCreateSandboxFromSnapshotUsesStoredPolicyAndSnapshotBackend(t *testing.T) {
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{
		createSnapshotFn: func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
			return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
		},
	}
	svc := newTestServiceWithSnapshotStore(adapter, store)

	sourceResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sourceSandbox := sourceResp.GetSandbox()

	snapshotResp, err := svc.CreateSnapshot(context.Background(), &cleanroomv1.CreateSnapshotRequest{
		SandboxId: sourceSandbox.GetSandboxId(),
		Name:      "deps",
	})
	if err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}

	forkResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Source: &cleanroomv1.CreateSandboxRequest_SnapshotId{
			SnapshotId: snapshotResp.GetSnapshot().GetSnapshotId(),
		},
	})
	if err != nil {
		t.Fatalf("CreateSandbox from snapshot returned error: %v", err)
	}
	forkSandbox := forkResp.GetSandbox()
	if got, want := forkSandbox.GetBackend(), "firecracker"; got != want {
		t.Fatalf("unexpected backend: got %q want %q", got, want)
	}
	snapshotID := snapshotResp.GetSnapshot().GetSnapshotId()
	if got, want := forkResp.GetSourceKind(), "snapshot"; got != want {
		t.Fatalf("unexpected response source kind: got %q want %q", got, want)
	}
	if got, want := forkResp.GetSourceId(), snapshotID; got != want {
		t.Fatalf("unexpected response source id: got %q want %q", got, want)
	}
	if got, want := forkResp.GetBackingSnapshotId(), snapshotID; got != want {
		t.Fatalf("unexpected response backing snapshot id: got %q want %q", got, want)
	}
	if got, want := forkSandbox.GetSourceKind(), "snapshot"; got != want {
		t.Fatalf("unexpected sandbox source kind: got %q want %q", got, want)
	}
	if got, want := forkSandbox.GetSourceId(), snapshotID; got != want {
		t.Fatalf("unexpected sandbox source id: got %q want %q", got, want)
	}
	if got, want := forkSandbox.GetBackingSnapshotId(), snapshotID; got != want {
		t.Fatalf("unexpected sandbox backing snapshot id: got %q want %q", got, want)
	}
	getResp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: forkSandbox.GetSandboxId()})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := getResp.GetSandbox().GetSourceKind(), "snapshot"; got != want {
		t.Fatalf("unexpected persisted sandbox source kind: got %q want %q", got, want)
	}
	if got, want := getResp.GetSandbox().GetSourceId(), snapshotID; got != want {
		t.Fatalf("unexpected persisted sandbox source id: got %q want %q", got, want)
	}
	if got, want := getResp.GetSandbox().GetBackingSnapshotId(), snapshotID; got != want {
		t.Fatalf("unexpected persisted sandbox backing snapshot id: got %q want %q", got, want)
	}
	if got, want := adapter.provisionFromSnapshotReq.SnapshotID, snapshotResp.GetSnapshot().GetSnapshotId(); got != want {
		t.Fatalf("unexpected provision snapshot id: got %q want %q", got, want)
	}
	if got, want := adapter.provisionFromSnapshotReq.StorageRef, "/snapshots/"+snapshotResp.GetSnapshot().GetSnapshotId()+".ext4"; got != want {
		t.Fatalf("unexpected snapshot storage ref: got %q want %q", got, want)
	}
	if got, want := adapter.provisionFromSnapshotReq.FirecrackerConfig.Snapshots.Driver, "file"; got != want {
		t.Fatalf("unexpected snapshot driver: got %q want %q", got, want)
	}
	if adapter.provisionFromSnapshotReq.Policy == nil {
		t.Fatal("expected compiled policy on provision-from-snapshot request")
	}
	if got, want := adapter.provisionFromSnapshotReq.Policy.Hash, sourceSandbox.GetPolicyHash(); got != want {
		t.Fatalf("unexpected policy hash: got %q want %q", got, want)
	}
}

func TestCreateSandboxFromSnapshotUsesStoredSnapshotDriverWhenConfigChanges(t *testing.T) {
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{
		createSnapshotFn: func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
			return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
		},
	}
	svc := newTestServiceWithSnapshotStore(adapter, store)

	sourceResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	snapshotResp, err := svc.CreateSnapshot(context.Background(), &cleanroomv1.CreateSnapshotRequest{
		SandboxId: sourceResp.GetSandbox().GetSandboxId(),
	})
	if err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}

	svc.Config.Backends.Firecracker.Snapshots.Driver = "zfs"
	_, err = svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Source: &cleanroomv1.CreateSandboxRequest_SnapshotId{
			SnapshotId: snapshotResp.GetSnapshot().GetSnapshotId(),
		},
	})
	if err != nil {
		t.Fatalf("CreateSandbox from snapshot returned error: %v", err)
	}
	if got, want := adapter.provisionFromSnapshotReq.FirecrackerConfig.Snapshots.Driver, "file"; got != want {
		t.Fatalf("unexpected snapshot driver after config change: got %q want %q", got, want)
	}
}

func TestDeleteSnapshotUsesStoredSnapshotDriverWhenConfigChanges(t *testing.T) {
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{
		createSnapshotFn: func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
			return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
		},
	}
	svc := newTestServiceWithSnapshotStore(adapter, store)

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	snapshotResp, err := svc.CreateSnapshot(context.Background(), &cleanroomv1.CreateSnapshotRequest{
		SandboxId: createResp.GetSandbox().GetSandboxId(),
	})
	if err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}

	svc.Config.Backends.Firecracker.Snapshots.Driver = "zfs"
	_, err = svc.DeleteSnapshot(context.Background(), &cleanroomv1.DeleteSnapshotRequest{
		SnapshotId: snapshotResp.GetSnapshot().GetSnapshotId(),
	})
	if err != nil {
		t.Fatalf("DeleteSnapshot returned error: %v", err)
	}
	if got, want := adapter.deleteSnapshotReq.FirecrackerConfig.Snapshots.Driver, "file"; got != want {
		t.Fatalf("unexpected delete snapshot driver after config change: got %q want %q", got, want)
	}
}

func TestDeleteSnapshotAllowsDeleteAfterSnapshotBackedSandboxReady(t *testing.T) {
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{
		createSnapshotFn: func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
			return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
		},
	}
	svc := newTestServiceWithSnapshotStore(adapter, store)

	sourceResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sourceSandbox := sourceResp.GetSandbox()

	snapshotResp, err := svc.CreateSnapshot(context.Background(), &cleanroomv1.CreateSnapshotRequest{
		SandboxId: sourceSandbox.GetSandboxId(),
	})
	if err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}
	snapshotID := snapshotResp.GetSnapshot().GetSnapshotId()

	fromSnapshotResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Source: &cleanroomv1.CreateSandboxRequest_SnapshotId{SnapshotId: snapshotID},
	})
	if err != nil {
		t.Fatalf("CreateSandbox from snapshot returned error: %v", err)
	}
	fromSnapshotSandboxID := fromSnapshotResp.GetSandbox().GetSandboxId()
	if fromSnapshotSandboxID == "" {
		t.Fatal("expected snapshot-backed sandbox id")
	}

	deleteResp, err := svc.DeleteSnapshot(context.Background(), &cleanroomv1.DeleteSnapshotRequest{SnapshotId: snapshotID})
	if err != nil {
		t.Fatalf("DeleteSnapshot returned error: %v", err)
	}
	if !deleteResp.GetDeleted() {
		t.Fatal("expected deleted=true")
	}
	if got, want := adapter.deleteSnapshotCalls, 1; got != want {
		t.Fatalf("unexpected backend delete call count: got %d want %d", got, want)
	}
	for _, sandboxID := range []string{sourceSandbox.GetSandboxId(), fromSnapshotSandboxID} {
		getResp, getErr := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
		if getErr != nil {
			t.Fatalf("GetSandbox returned error for %q: %v", sandboxID, getErr)
		}
		if got, want := getResp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY; got != want {
			t.Fatalf("unexpected sandbox status for %q: got %v want %v", sandboxID, got, want)
		}
	}
}

func TestDeleteSnapshotRejectsSnapshotWithInFlightProvisionFromSnapshot(t *testing.T) {
	store := newMemorySnapshotStore()
	provisionStarted := make(chan struct{}, 1)
	provisionRelease := make(chan struct{})
	createDone := make(chan error, 1)
	adapter := &stubAdapter{
		createSnapshotFn: func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
			return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
		},
		provisionFromSnapshotFn: func(_ context.Context, _ backend.ProvisionFromSnapshotRequest) error {
			select {
			case provisionStarted <- struct{}{}:
			default:
			}
			<-provisionRelease
			return nil
		},
	}
	svc := newTestServiceWithSnapshotStore(adapter, store)

	sourceResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sourceSandbox := sourceResp.GetSandbox()

	snapshotResp, err := svc.CreateSnapshot(context.Background(), &cleanroomv1.CreateSnapshotRequest{
		SandboxId: sourceSandbox.GetSandboxId(),
	})
	if err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}
	snapshotID := snapshotResp.GetSnapshot().GetSnapshotId()

	go func() {
		_, createErr := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
			Source: &cleanroomv1.CreateSandboxRequest_SnapshotId{SnapshotId: snapshotID},
		})
		createDone <- createErr
	}()

	select {
	case <-provisionStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("expected create-from-snapshot provision to start")
	}

	_, err = svc.DeleteSnapshot(context.Background(), &cleanroomv1.DeleteSnapshotRequest{SnapshotId: snapshotID})
	if err == nil {
		t.Fatal("expected delete snapshot to fail while provision is in flight")
	}
	if !strings.Contains(err.Error(), "snapshot_busy") || !strings.Contains(err.Error(), "another operation") {
		t.Fatalf("unexpected delete snapshot error: %v", err)
	}

	close(provisionRelease)
	select {
	case createErr := <-createDone:
		if createErr != nil {
			t.Fatalf("CreateSandbox from snapshot returned error: %v", createErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected create-from-snapshot to finish")
	}
}

func TestDeleteSnapshotRejectsSnapshotWithMetadataLoadInFlight(t *testing.T) {
	baseStore := newMemorySnapshotStore()
	store := &blockingSnapshotStore{
		memorySnapshotStore: baseStore,
		getStarted:          make(chan struct{}, 1),
		getRelease:          make(chan struct{}),
	}
	adapter := &stubAdapter{
		createSnapshotFn: func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
			return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
		},
	}
	svc := newTestServiceWithSnapshotStore(adapter, store)

	sourceResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	snapshotResp, err := svc.CreateSnapshot(context.Background(), &cleanroomv1.CreateSnapshotRequest{
		SandboxId: sourceResp.GetSandbox().GetSandboxId(),
	})
	if err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}
	snapshotID := snapshotResp.GetSnapshot().GetSnapshotId()

	createDone := make(chan error, 1)
	go func() {
		_, createErr := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
			Source: &cleanroomv1.CreateSandboxRequest_SnapshotId{SnapshotId: snapshotID},
		})
		createDone <- createErr
	}()

	select {
	case <-store.getStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("expected create-from-snapshot metadata load to start")
	}

	_, err = svc.DeleteSnapshot(context.Background(), &cleanroomv1.DeleteSnapshotRequest{SnapshotId: snapshotID})
	if err == nil {
		t.Fatal("expected delete snapshot to fail while metadata load is in flight")
	}
	if !strings.Contains(err.Error(), "snapshot_busy") || !strings.Contains(err.Error(), "another operation") {
		t.Fatalf("unexpected delete snapshot error: %v", err)
	}

	close(store.getRelease)
	select {
	case createErr := <-createDone:
		if createErr != nil {
			t.Fatalf("CreateSandbox from snapshot returned error: %v", createErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected create-from-snapshot to finish")
	}
}

func TestCreateSnapshotDoesNotResurrectTerminatedSandbox(t *testing.T) {
	store := newMemorySnapshotStore()
	snapshotStarted := make(chan struct{}, 1)
	releaseSnapshot := make(chan struct{})
	createDone := make(chan error, 1)
	adapter := &stubAdapter{
		createSnapshotFn: func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
			select {
			case snapshotStarted <- struct{}{}:
			default:
			}
			<-releaseSnapshot
			return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
		},
	}
	svc := newTestServiceWithSnapshotStore(adapter, store)

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()

	go func() {
		_, createErr := svc.CreateSnapshot(context.Background(), &cleanroomv1.CreateSnapshotRequest{
			SandboxId: sandboxID,
		})
		createDone <- createErr
	}()

	select {
	case <-snapshotStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("expected snapshot creation to start")
	}

	if _, err := svc.TerminateSandbox(context.Background(), &cleanroomv1.TerminateSandboxRequest{
		SandboxId: sandboxID,
	}); err != nil {
		t.Fatalf("TerminateSandbox returned error: %v", err)
	}

	close(releaseSnapshot)
	select {
	case createErr := <-createDone:
		if createErr != nil {
			t.Fatalf("CreateSnapshot returned error: %v", createErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected snapshot creation to finish")
	}

	getResp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := getResp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED; got != want {
		t.Fatalf("unexpected sandbox status after terminate+racing snapshot: got %v want %v", got, want)
	}

	_, err = svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"true"},
	})
	if err == nil {
		t.Fatal("expected CreateExecution to fail for terminated sandbox")
	}
	if !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("unexpected CreateExecution error: %v", err)
	}
}

func TestExecutionStreamIncludesStructuredWarnings(t *testing.T) {
	const warningText = "darwin-vz guest networking is enabled without host-side egress filtering"

	adapter := &stubAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if stream.OnWarning != nil {
				stream.OnWarning(warningText)
			}
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("hello stdout\n"))
			}
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    0,
				Stdout:      "hello stdout\n",
				Message:     "done",
			}, nil
		},
	}
	svc := newTestService(adapter)

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"--", "echo", "hi"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()

	history, updates, done, unsubscribe, err := svc.SubscribeExecutionEvents(sandboxID, executionID)
	if err != nil {
		t.Fatalf("SubscribeExecutionEvents returned error: %v", err)
	}
	defer unsubscribe()

	events := collectExecutionEvents(t, history, updates, done)
	var sawWarning bool
	var sawStderr bool
	for _, event := range events {
		switch payload := event.Payload.(type) {
		case *cleanroomv1.ExecutionStreamEvent_Warning:
			if payload.Warning == warningText {
				sawWarning = true
			}
		case *cleanroomv1.ExecutionStreamEvent_Stderr:
			if strings.Contains(string(payload.Stderr), warningText) {
				sawStderr = true
			}
		}
	}
	if !sawWarning {
		t.Fatalf("expected warning event in stream, events=%d", len(events))
	}
	if sawStderr {
		t.Fatalf("expected structured warning to stay out of stderr events, events=%d", len(events))
	}
}

func TestCancelExecutionTransitionsToCanceled(t *testing.T) {
	started := make(chan struct{}, 1)
	adapter := &stubAdapter{
		runFn: func(ctx context.Context, _ backend.ExecutionRequest) (*backend.ExecutionResult, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	svc := newTestService(adapter)

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"sleep", "10"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()

	history, updates, done, unsubscribe, err := svc.SubscribeExecutionEvents(sandboxID, executionID)
	if err != nil {
		t.Fatalf("SubscribeExecutionEvents returned error: %v", err)
	}
	defer unsubscribe()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for execution to start")
	}

	cancelResp, err := svc.CancelExecution(context.Background(), &cleanroomv1.CancelExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Signal:      15,
	})
	if err != nil {
		t.Fatalf("CancelExecution returned error: %v", err)
	}
	if !cancelResp.GetAccepted() {
		t.Fatal("expected cancel request to be accepted")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for canceled execution to finish")
	}

	getResp, err := svc.GetExecution(context.Background(), &cleanroomv1.GetExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
	})
	if err != nil {
		t.Fatalf("GetExecution returned error: %v", err)
	}
	if got, want := getResp.GetExecution().GetStatus(), cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED; got != want {
		t.Fatalf("unexpected execution status: got %v want %v", got, want)
	}

	events := collectExecutionEvents(t, history, updates, done)
	var sawCancelMessage bool
	var exit *cleanroomv1.ExecutionExit
	for _, event := range events {
		if payload, ok := event.Payload.(*cleanroomv1.ExecutionStreamEvent_Message); ok && strings.Contains(payload.Message, "cancel requested") {
			sawCancelMessage = true
		}
		if payload, ok := event.Payload.(*cleanroomv1.ExecutionStreamEvent_Exit); ok {
			exit = payload.Exit
		}
	}
	if !sawCancelMessage {
		t.Fatalf("expected cancel message event, events=%d", len(events))
	}
	if exit == nil {
		t.Fatalf("expected exit event after cancel, events=%d", len(events))
	}
	if got, want := exit.GetStatus(), cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED; got != want {
		t.Fatalf("unexpected exit status: got %v want %v", got, want)
	}
	if got, want := exit.GetExitCode(), int32(143); got != want {
		t.Fatalf("unexpected exit code: got %d want %d", got, want)
	}
}

func TestCreateSandboxProvisionsPersistentBackend(t *testing.T) {
	adapter := &stubAdapter{}
	svc := newTestService(adapter)

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if createResp.GetSandbox().GetSandboxId() == "" {
		t.Fatal("expected sandbox id")
	}
	if got, want := adapter.provisionCalls, 1; got != want {
		t.Fatalf("unexpected provision call count: got %d want %d", got, want)
	}

	if _, err := svc.TerminateSandbox(context.Background(), &cleanroomv1.TerminateSandboxRequest{SandboxId: createResp.GetSandbox().GetSandboxId()}); err != nil {
		t.Fatalf("TerminateSandbox returned error: %v", err)
	}
	if got, want := adapter.terminateCalls, 1; got != want {
		t.Fatalf("unexpected terminate call count: got %d want %d", got, want)
	}
}

func TestCreateSandboxBootstrapsRepositoryInService(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	adapter := &stubAdapter{}
	mirrors := &stubRepositoryMirrorStore{}
	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryPolicy(),
		RepositoryCheckout: testRepositoryCheckoutProto(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if createResp.GetSandbox().GetSandboxId() == "" {
		t.Fatal("expected sandbox id")
	}
	if got, want := mirrors.calls, 1; got != want {
		t.Fatalf("unexpected mirror prewarm call count: got %d want %d", got, want)
	}
	if got, want := mirrors.remoteURL, "https://github.com/buildkite/cleanroom.git"; got != want {
		t.Fatalf("unexpected prewarmed remote URL: got %q want %q", got, want)
	}
	if got, want := mirrors.commitSHA, "0123456789abcdef0123456789abcdef01234567"; got != want {
		t.Fatalf("unexpected prewarmed commit SHA: got %q want %q", got, want)
	}
	joined := strings.Join(adapter.req.Command, " ")
	if !strings.Contains(joined, "git clone --filter=blob:none --no-checkout") {
		t.Fatalf("expected repository bootstrap clone in command, got %q", joined)
	}
	if !strings.Contains(joined, "git -C \"$dest\" checkout --detach \"$commit\"") {
		t.Fatalf("expected repository checkout command, got %q", joined)
	}
	if strings.Contains(joined, "Authorization:") || strings.Contains(joined, ".extraHeader") {
		t.Fatalf("expected bootstrap command to avoid embedded auth, got %q", joined)
	}
	wantRunDir := internalBootstrapArtifactsDir(createResp.GetSandbox().GetSandboxId(), adapter.req.ExecutionID)
	if got := adapter.req.RunDir; got != wantRunDir {
		t.Fatalf("unexpected bootstrap run dir: got %q want %q", got, wantRunDir)
	}
	executionBaseDir, err := paths.ExecutionBaseDir()
	if err != nil {
		t.Fatalf("ExecutionBaseDir returned error: %v", err)
	}
	if rel, err := filepath.Rel(executionBaseDir, adapter.req.RunDir); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		t.Fatalf("expected bootstrap run dir to stay out of execution artifacts base dir %q, got %q", executionBaseDir, adapter.req.RunDir)
	}
}

func TestCreateSandboxPublishesWorkspaceStageCache(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{
		createSnapshotFn: func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
			return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
		},
	}
	mirrors := &stubRepositoryMirrorStore{}
	svc := newTestServiceWithSnapshotStore(adapter, store)
	svc.RepositoryStore = mirrors
	publishedAt := time.Unix(1_700_000_123, 0).UTC()
	svc.runtime.clock = stubClock{now: publishedAt}

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryPolicy(),
		RepositoryCheckout: testRepositoryCheckoutProto(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	if got, want := adapter.provisionCalls, 1; got != want {
		t.Fatalf("unexpected provision call count: got %d want %d", got, want)
	}
	if got, want := adapter.createSnapshotCalls, 1; got != want {
		t.Fatalf("unexpected workspace stage snapshot create count: got %d want %d", got, want)
	}
	if got, want := adapter.createSnapshotReq.SandboxID, createResp.GetSandbox().GetSandboxId(); got != want {
		t.Fatalf("unexpected snapshot sandbox id: got %q want %q", got, want)
	}

	cacheStore, ok := svc.CacheStore.(*memoryCacheStore)
	if !ok {
		t.Fatalf("expected memory cache store, got %T", svc.CacheStore)
	}
	records, err := cacheStore.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if got, want := len(records), 1; got != want {
		t.Fatalf("unexpected workspace cache record count: got %d want %d", got, want)
	}
	record := records[0]
	compiled, err := policy.FromProto(testRepositoryPolicy())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	if got, want := record.CacheKey, workspaceStageCacheKey("firecracker", "runtime-base:test", compiled.Hash, repositorycheckout.FromProto(testRepositoryCheckoutProto()), nil); got != want {
		t.Fatalf("unexpected workspace cache key: got %q want %q", got, want)
	}
	if got, want := record.Stage, workspaceStageName; got != want {
		t.Fatalf("unexpected workspace cache stage: got %q want %q", got, want)
	}
	if got, want := record.State, cacheStateReady; got != want {
		t.Fatalf("unexpected workspace cache state: got %q want %q", got, want)
	}
	if got, want := record.ParentCacheKey, "runtime-base:test"; got != want {
		t.Fatalf("unexpected parent cache key: got %q want %q", got, want)
	}
	if got, want := record.PolicyHash, compiled.Hash; got != want {
		t.Fatalf("unexpected policy hash: got %q want %q", got, want)
	}
	if record.Repository == nil {
		t.Fatal("expected repository metadata on workspace stage cache")
	}
	if got, want := record.Repository.GetCommitSha(), testRepositoryCheckoutProto().GetCommitSha(); got != want {
		t.Fatalf("unexpected workspace stage commit: got %q want %q", got, want)
	}
	backingSnapshotID, ok := cacheRecordBackingSnapshotID(record)
	if !ok {
		t.Fatal("expected workspace stage cache to include backing snapshot identity")
	}
	if got, want := backingSnapshotID, adapter.createSnapshotReq.SnapshotID; got != want {
		t.Fatalf("unexpected workspace stage backing snapshot identity: got %q want %q", got, want)
	}
	if got, want := record.CreatedAt, publishedAt; !got.Equal(want) {
		t.Fatalf("unexpected workspace stage created_at: got %s want %s", got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

func TestCreateSandboxReusesWorkspaceStageCache(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{
		createSnapshotFn: func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
			return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
		},
	}
	mirrors := &stubRepositoryMirrorStore{}
	svc := newTestServiceWithSnapshotStore(adapter, store)
	svc.RepositoryStore = mirrors

	req := &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryPolicy(),
		RepositoryCheckout: testRepositoryCheckoutProto(),
	}

	if _, err := svc.CreateSandbox(context.Background(), req); err != nil {
		t.Fatalf("first CreateSandbox returned error: %v", err)
	}
	if got, want := adapter.provisionCalls, 1; got != want {
		t.Fatalf("unexpected initial provision calls: got %d want %d", got, want)
	}
	if got, want := adapter.createSnapshotCalls, 1; got != want {
		t.Fatalf("unexpected initial snapshot create calls: got %d want %d", got, want)
	}
	if got, want := adapter.provisionFromSnapshotCalls, 0; got != want {
		t.Fatalf("unexpected initial snapshot restore calls: got %d want %d", got, want)
	}
	if got, want := mirrors.calls, 1; got != want {
		t.Fatalf("unexpected initial mirror calls: got %d want %d", got, want)
	}

	secondResp, err := svc.CreateSandbox(context.Background(), req)
	if err != nil {
		t.Fatalf("second CreateSandbox returned error: %v", err)
	}
	if secondResp.GetSandbox().GetSandboxId() == "" {
		t.Fatal("expected sandbox id from cache-backed create")
	}
	if got, want := adapter.provisionCalls, 1; got != want {
		t.Fatalf("expected warm hit to avoid reprovision bootstrap path, got provisionCalls=%d want=%d", got, want)
	}
	if got, want := adapter.createSnapshotCalls, 1; got != want {
		t.Fatalf("expected warm hit to avoid publishing another workspace stage, got createSnapshotCalls=%d want=%d", got, want)
	}
	if got, want := adapter.provisionFromSnapshotCalls, 1; got != want {
		t.Fatalf("expected warm hit to provision from snapshot once, got %d want %d", got, want)
	}
	if got, want := mirrors.calls, 1; got != want {
		t.Fatalf("expected warm hit to avoid another mirror prewarm, got %d want %d", got, want)
	}
	if got, want := adapter.runCalls, 1; got != want {
		t.Fatalf("expected warm hit to skip repository bootstrap execution, got runCalls=%d want=%d", got, want)
	}
	if got, want := adapter.provisionFromSnapshotReq.StorageRef, "/snapshots/"+adapter.createSnapshotReq.SnapshotID+".ext4"; got != want {
		t.Fatalf("unexpected snapshot storage ref on warm hit: got %q want %q", got, want)
	}
}

func TestCreateExecutionPreservesFirstMatchingRepositoryAfterChangesetWorkspaceStageCacheRestore(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{
		createSnapshotFn: func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
			return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
		},
	}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"README.md": "hello\n",
	})
	svc := newTestServiceWithSnapshotStore(adapter, store)
	svc.RepositoryStore = mirrors

	repository := repositorycheckout.FromProto(repositoryCheckout)
	if err := os.WriteFile(filepath.Join(mirrors.mirrorPath, "README.md"), []byte("hello from changeset\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(README.md) returned error: %v", err)
	}
	changeset, err := repositorychangeset.BuildFromWorkingTree(mirrors.mirrorPath, repository)
	if err != nil {
		t.Fatalf("BuildFromWorkingTree returned error: %v", err)
	}
	if changeset == nil {
		t.Fatal("expected repository changeset")
	}

	var (
		mu       sync.Mutex
		commands [][]string
	)
	runCalled := make(chan struct{}, 8)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		select {
		case runCalled <- struct{}{}:
		default:
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	req := &cleanroomv1.CreateSandboxRequest{
		Policy:              testRepositoryPolicy(),
		RepositoryCheckout:  repositoryCheckout,
		RepositoryChangeset: changeset.ToProto(),
	}
	if _, err := svc.CreateSandbox(context.Background(), req); err != nil {
		t.Fatalf("first CreateSandbox returned error: %v", err)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-runCalled:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for cold changeset bootstrap")
		}
	}

	secondResp, err := svc.CreateSandbox(context.Background(), req)
	if err != nil {
		t.Fatalf("second CreateSandbox returned error: %v", err)
	}
	if got, want := secondResp.GetSourceKind(), "workspace stage cache"; got != want {
		t.Fatalf("unexpected response source kind: got %q want %q", got, want)
	}
	if got, want := secondResp.GetSandbox().GetSourceKind(), "workspace stage cache"; got != want {
		t.Fatalf("unexpected sandbox source kind: got %q want %q", got, want)
	}
	if got, want := adapter.provisionFromSnapshotCalls, 1; got != want {
		t.Fatalf("expected warm changeset workspace-stage restore once, got %d want %d", got, want)
	}

	_, err = svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId:          secondResp.GetSandbox().GetSandboxId(),
		Command:            []string{"sh", "-lc", "pwd"},
		RepositoryCheckout: repositoryCheckout,
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	select {
	case <-runCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for warm-cache execution")
	}

	mu.Lock()
	defer mu.Unlock()
	if got, want := len(commands), 3; got != want {
		t.Fatalf("expected create bootstrap + changeset apply + warm-cache execution, got %d command(s)", got)
	}
	joined := strings.Join(commands[2], " ")
	if strings.Contains(joined, "git clone --filter=blob:none --no-checkout") {
		t.Fatalf("expected first matching repository execution after workspace stage cache restore to preserve changeset state, got %q", joined)
	}
	if !repositoryWrappedCommandContains(joined, `exec 'sh' '-lc' 'pwd'`) {
		t.Fatalf("expected wrapped user command in repository workdir, got %q", joined)
	}
}

func TestCreateSandboxReusesWorkspaceStageCacheForNormalizedDestinationDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{
		createSnapshotFn: func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
			return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
		},
	}
	mirrors := &stubRepositoryMirrorStore{}
	svc := newTestServiceWithSnapshotStore(adapter, store)
	svc.RepositoryStore = mirrors

	firstRepo := testRepositoryCheckoutProto()
	firstRepo.DestinationDir = "/workspace/"
	secondRepo := testRepositoryCheckoutProto()
	secondRepo.DestinationDir = "/workspace"

	if _, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryPolicy(),
		RepositoryCheckout: firstRepo,
	}); err != nil {
		t.Fatalf("first CreateSandbox returned error: %v", err)
	}
	if _, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryPolicy(),
		RepositoryCheckout: secondRepo,
	}); err != nil {
		t.Fatalf("second CreateSandbox returned error: %v", err)
	}

	if got, want := adapter.createSnapshotCalls, 1; got != want {
		t.Fatalf("expected normalized destination dir to reuse existing workspace stage cache, got createSnapshotCalls=%d want=%d", got, want)
	}
	if got, want := adapter.provisionFromSnapshotCalls, 1; got != want {
		t.Fatalf("expected normalized destination dir to warm-hit the workspace stage cache, got provisionFromSnapshotCalls=%d want=%d", got, want)
	}
}

func TestCreateSandboxDoesNotReuseWorkspaceStageCacheAcrossPolicies(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{
		createSnapshotFn: func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
			return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
		},
	}
	mirrors := &stubRepositoryMirrorStore{}
	svc := newTestServiceWithSnapshotStore(adapter, store)
	svc.RepositoryStore = mirrors

	firstPolicy := testRepositoryPolicy()
	if _, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             firstPolicy,
		RepositoryCheckout: testRepositoryCheckoutProto(),
	}); err != nil {
		t.Fatalf("first CreateSandbox returned error: %v", err)
	}

	secondPolicy := testRepositoryPolicy()
	secondPolicy.Allow = append(secondPolicy.Allow, &cleanroomv1.PolicyAllowRule{
		Host:  "pkg.buildkite.test",
		Ports: []int32{443},
	})
	if _, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             secondPolicy,
		RepositoryCheckout: testRepositoryCheckoutProto(),
	}); err != nil {
		t.Fatalf("second CreateSandbox returned error: %v", err)
	}

	if got, want := adapter.provisionCalls, 2; got != want {
		t.Fatalf("expected both sandboxes to provision from scratch, got %d want %d", got, want)
	}
	if got, want := adapter.provisionFromSnapshotCalls, 0; got != want {
		t.Fatalf("expected no snapshot restores across policy changes, got %d want %d", got, want)
	}
	if got, want := adapter.createSnapshotCalls, 2; got != want {
		t.Fatalf("expected each policy variant to publish its own workspace stage cache, got %d want %d", got, want)
	}

	cacheStore, ok := svc.CacheStore.(*memoryCacheStore)
	if !ok {
		t.Fatalf("expected memory cache store, got %T", svc.CacheStore)
	}
	records, err := cacheStore.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if got, want := len(records), 2; got != want {
		t.Fatalf("expected one workspace stage cache per policy, got %d want %d", got, want)
	}

	firstCompiled, err := policy.FromProto(firstPolicy)
	if err != nil {
		t.Fatalf("FromProto(firstPolicy) returned error: %v", err)
	}
	secondCompiled, err := policy.FromProto(secondPolicy)
	if err != nil {
		t.Fatalf("FromProto(secondPolicy) returned error: %v", err)
	}
	if firstCompiled.Hash == secondCompiled.Hash {
		t.Fatalf("expected distinct compiled policy hashes, got %q", firstCompiled.Hash)
	}
	firstKey := workspaceStageCacheKey("firecracker", "runtime-base:test", firstCompiled.Hash, repositorycheckout.FromProto(testRepositoryCheckoutProto()), nil)
	secondKey := workspaceStageCacheKey("firecracker", "runtime-base:test", secondCompiled.Hash, repositorycheckout.FromProto(testRepositoryCheckoutProto()), nil)
	if firstKey == secondKey {
		t.Fatalf("expected workspace stage cache keys to differ across policies, got %q", firstKey)
	}
}

func TestCreateSandboxPublishesWorkspaceStageCachePerBackend(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	firecrackerAdapter := &stubAdapter{
		createSnapshotFn: func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
			return &backend.SnapshotResult{StorageRef: "/snapshots/firecracker/" + req.SnapshotID + ".ext4"}, nil
		},
	}
	darwinAdapter := &stubAdapter{
		createSnapshotFn: func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
			return &backend.SnapshotResult{StorageRef: "/snapshots/darwin-vz/" + req.SnapshotID + ".img"}, nil
		},
	}
	mirrors := &stubRepositoryMirrorStore{}
	svc := newTestServiceWithSnapshotStore(firecrackerAdapter, store)
	svc.RepositoryStore = mirrors
	svc.Backends["darwin-vz"] = darwinAdapter
	svc.Config.Backends.DarwinVZ.Snapshots.Enabled = true
	svc.Config.Backends.DarwinVZ.Snapshots.Driver = "apfs"

	req := &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryPolicy(),
		RepositoryCheckout: testRepositoryCheckoutProto(),
	}
	if _, err := svc.CreateSandbox(context.Background(), req); err != nil {
		t.Fatalf("CreateSandbox firecracker returned error: %v", err)
	}
	if _, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Backend:            "darwin-vz",
		Policy:             testRepositoryPolicy(),
		RepositoryCheckout: testRepositoryCheckoutProto(),
	}); err != nil {
		t.Fatalf("CreateSandbox darwin-vz returned error: %v", err)
	}

	if got, want := firecrackerAdapter.createSnapshotCalls, 1; got != want {
		t.Fatalf("expected one firecracker workspace stage publish, got %d want %d", got, want)
	}
	if got, want := darwinAdapter.createSnapshotCalls, 1; got != want {
		t.Fatalf("expected one darwin-vz workspace stage publish, got %d want %d", got, want)
	}
	if got := firecrackerAdapter.deleteSnapshotCalls + darwinAdapter.deleteSnapshotCalls; got != 0 {
		t.Fatalf("expected no snapshot rollback deletes across backends, got %d", got)
	}

	cacheStore, ok := svc.CacheStore.(*memoryCacheStore)
	if !ok {
		t.Fatalf("expected memory cache store, got %T", svc.CacheStore)
	}
	records, err := cacheStore.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if got, want := len(records), 2; got != want {
		t.Fatalf("expected one workspace stage cache per backend, got %d want %d", got, want)
	}

	compiled, err := policy.FromProto(testRepositoryPolicy())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	firecrackerKey := workspaceStageCacheKey("firecracker", "runtime-base:test", compiled.Hash, repositorycheckout.FromProto(testRepositoryCheckoutProto()), nil)
	darwinKey := workspaceStageCacheKey("darwin-vz", "runtime-base:test", compiled.Hash, repositorycheckout.FromProto(testRepositoryCheckoutProto()), nil)
	if firecrackerKey == darwinKey {
		t.Fatalf("expected backend-specific workspace stage cache keys, got %q", firecrackerKey)
	}
}

func TestCreateSandboxDoesNotReuseWorkspaceStageWhenRuntimeBaseKeyChanges(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{
		runtimeBaseKeyOverride: "runtime-base:a",
		createSnapshotFn: func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
			return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
		},
	}
	mirrors := &stubRepositoryMirrorStore{}
	svc := newTestServiceWithSnapshotStore(adapter, store)
	svc.RepositoryStore = mirrors

	req := &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryPolicy(),
		RepositoryCheckout: testRepositoryCheckoutProto(),
	}

	if _, err := svc.CreateSandbox(context.Background(), req); err != nil {
		t.Fatalf("first CreateSandbox returned error: %v", err)
	}

	adapter.runtimeBaseKeyOverride = "runtime-base:b"

	secondResp, err := svc.CreateSandbox(context.Background(), req)
	if err != nil {
		t.Fatalf("second CreateSandbox returned error: %v", err)
	}
	if secondResp.GetSandbox().GetSandboxId() == "" {
		t.Fatal("expected sandbox id after runtime base change")
	}
	if got, want := adapter.provisionFromSnapshotCalls, 0; got != want {
		t.Fatalf("expected runtime base change to avoid snapshot restore, got %d want %d", got, want)
	}
	if got, want := adapter.provisionCalls, 2; got != want {
		t.Fatalf("expected runtime base change to reprovision sandbox, got %d want %d", got, want)
	}
	if got, want := adapter.runCalls, 2; got != want {
		t.Fatalf("expected runtime base change to rerun repository bootstrap, got %d want %d", got, want)
	}
	if got, want := adapter.createSnapshotCalls, 2; got != want {
		t.Fatalf("expected runtime base change to publish a new workspace stage, got %d want %d", got, want)
	}

	cacheStore, ok := svc.CacheStore.(*memoryCacheStore)
	if !ok {
		t.Fatalf("expected memory cache store, got %T", svc.CacheStore)
	}
	records, err := cacheStore.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if got, want := len(records), 2; got != want {
		t.Fatalf("expected two workspace stage cache records after runtime base change, got %d want %d", got, want)
	}
}

func TestCreateSandboxFallsBackWhenWorkspaceStageRestoreFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{
		createSnapshotFn: func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
			return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
		},
		provisionFromSnapshotFn: func(_ context.Context, _ backend.ProvisionFromSnapshotRequest) error {
			return errors.New("snapshot restore failed")
		},
	}
	mirrors := &stubRepositoryMirrorStore{}
	svc := newTestServiceWithSnapshotStore(adapter, store)
	svc.RepositoryStore = mirrors

	req := &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryPolicy(),
		RepositoryCheckout: testRepositoryCheckoutProto(),
	}

	if _, err := svc.CreateSandbox(context.Background(), req); err != nil {
		t.Fatalf("first CreateSandbox returned error: %v", err)
	}
	firstSnapshotID := adapter.createSnapshotReq.SnapshotID

	secondResp, err := svc.CreateSandbox(context.Background(), req)
	if err != nil {
		t.Fatalf("second CreateSandbox returned error: %v", err)
	}
	if secondResp.GetSandbox().GetSandboxId() == "" {
		t.Fatal("expected sandbox id after falling back to cold bootstrap")
	}
	if got, want := adapter.provisionFromSnapshotCalls, 1; got != want {
		t.Fatalf("expected one failed snapshot restore attempt, got %d want %d", got, want)
	}
	if got, want := adapter.provisionCalls, 2; got != want {
		t.Fatalf("expected fallback path to reprovision sandbox, got %d want %d", got, want)
	}
	if got, want := adapter.runCalls, 2; got != want {
		t.Fatalf("expected fallback path to rerun repository bootstrap, got %d want %d", got, want)
	}
	if got, want := adapter.createSnapshotCalls, 2; got != want {
		t.Fatalf("expected fallback path to republish workspace stage snapshot, got %d want %d", got, want)
	}
	if got, want := mirrors.calls, 2; got != want {
		t.Fatalf("expected fallback path to re-prewarm mirror, got %d want %d", got, want)
	}
	if got, want := adapter.deleteSnapshotCalls, 1; got != want {
		t.Fatalf("expected fallback replacement cleanup to delete stale snapshot once, got %d want %d", got, want)
	}
	if got, want := adapter.deleteSnapshotReq.SnapshotID, firstSnapshotID; got != want {
		t.Fatalf("unexpected stale snapshot cleanup target: got %q want %q", got, want)
	}
	cacheStore, ok := svc.CacheStore.(*memoryCacheStore)
	if !ok {
		t.Fatalf("expected memory cache store, got %T", svc.CacheStore)
	}
	records, err := cacheStore.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if got, want := len(records), 1; got != want {
		t.Fatalf("expected failed restore replacement to leave one workspace stage cache record, got %d want %d", got, want)
	}
	backingSnapshotID, ok := cacheRecordBackingSnapshotID(records[0])
	if !ok {
		t.Fatal("expected workspace stage cache to include backing snapshot identity")
	}
	if got, want := backingSnapshotID, adapter.createSnapshotReq.SnapshotID; got != want {
		t.Fatalf("unexpected workspace stage backing snapshot identity after replacement: got %q want %q", got, want)
	}

	snapshotRecords, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if got, want := len(snapshotRecords), 0; got != want {
		t.Fatalf("expected workspace stage snapshots to stay out of snapshot metadata, got %d want %d", got, want)
	}
}

func TestDeleteWorkspaceStageCacheSnapshotRejectsInFlightRestore(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	provisionStarted := make(chan struct{}, 1)
	provisionRelease := make(chan struct{})
	adapter := &stubAdapter{
		provisionFromSnapshotFn: func(_ context.Context, _ backend.ProvisionFromSnapshotRequest) error {
			select {
			case provisionStarted <- struct{}{}:
			default:
			}
			<-provisionRelease
			return nil
		},
	}
	svc := newTestServiceWithSnapshotStore(adapter, store)

	compiled, err := policy.FromProto(testRepositoryPolicy())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(testRepositoryCheckoutProto())
	record := cachestore.Record{
		CacheKey:          workspaceStageCacheKey("firecracker", "runtime-base:test", compiled.Hash, repository, nil),
		Stage:             workspaceStageName,
		State:             cacheStateReady,
		BackingSnapshotID: "workspace-stage-backing-snapshot",
		Backend:           "firecracker",
		PolicyHash:        compiled.Hash,
		Policy:            compiled.ToProto(),
		Repository:        cloneRepositoryCheckout(normalizeRepositoryCheckoutForComparison(repository)).ToProto(),
		ParentCacheKey:    "runtime-base:test",
		StorageDriver:     "file",
		StorageRef:        "/snapshots/workspace-stage-backing-snapshot.ext4",
		ProducerVersion:   workspaceStageProducerVersion,
	}
	cacheStore, ok := svc.CacheStore.(*memoryCacheStore)
	if !ok {
		t.Fatalf("expected memory cache store, got %T", svc.CacheStore)
	}
	if err := cacheStore.Create(context.Background(), record); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	createDone := make(chan error, 1)
	go func() {
		_, createErr := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
			Policy:             testRepositoryPolicy(),
			RepositoryCheckout: testRepositoryCheckoutProto(),
		})
		createDone <- createErr
	}()

	select {
	case <-provisionStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("expected workspace stage restore to start")
	}

	firecrackerCfg := runtimeconfig.MergeBackendConfig(svc.Config, "firecracker", 0)
	err = svc.deleteWorkspaceStageCacheSnapshot(context.Background(), adapter, "firecracker", firecrackerCfg, record)
	if err == nil {
		t.Fatal("expected stale workspace stage cleanup to fail while restore is in flight")
	}
	if !strings.Contains(err.Error(), "snapshot_busy") || !strings.Contains(err.Error(), "another operation") {
		t.Fatalf("unexpected stale workspace stage cleanup error: %v", err)
	}
	if got, want := adapter.deleteSnapshotCalls, 0; got != want {
		t.Fatalf("expected no backend delete while restore is in flight, got %d want %d", got, want)
	}

	close(provisionRelease)
	select {
	case createErr := <-createDone:
		if createErr != nil {
			t.Fatalf("CreateSandbox returned error: %v", createErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected workspace stage restore to finish")
	}
}

func TestCreateSandboxPublishesDependencyStageCacheForConfiguredDependencies(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{}
	var (
		runMu        sync.Mutex
		runCommands  [][]string
		snapshotReqs []backend.SnapshotRequest
	)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		runMu.Lock()
		runCommands = append(runCommands, append([]string(nil), req.Command...))
		runMu.Unlock()
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}
	adapter.createSnapshotFn = func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
		snapshotReqs = append(snapshotReqs, req)
		return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
	}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"go.mod": "module example.com/test\n\ngo 1.26.2\n",
		"go.sum": "example.com/test v0.0.0 h1:abc123\n",
		"README": "ignored\n",
	})
	svc := newTestServiceWithSnapshotStore(adapter, store)
	svc.RepositoryStore = mirrors

	if _, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryDependencyPolicy(),
		RepositoryCheckout: repositoryCheckout,
	}); err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	if got, want := adapter.provisionCalls, 1; got != want {
		t.Fatalf("unexpected provision call count: got %d want %d", got, want)
	}
	if got, want := adapter.runCalls, 2; got != want {
		t.Fatalf("expected repository + dependency bootstrap executions, got %d want %d", got, want)
	}
	if got, want := adapter.createSnapshotCalls, 2; got != want {
		t.Fatalf("expected workspace + dependency stage snapshots, got %d want %d", got, want)
	}
	if got, want := len(snapshotReqs), 2; got != want {
		t.Fatalf("expected two snapshot publish requests, got %d want %d", got, want)
	}

	cacheStore, ok := svc.CacheStore.(*memoryCacheStore)
	if !ok {
		t.Fatalf("expected memory cache store, got %T", svc.CacheStore)
	}
	records, err := cacheStore.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if got, want := len(records), 2; got != want {
		t.Fatalf("expected workspace + dependency cache records, got %d want %d", got, want)
	}

	var (
		workspaceRecord  *cachestore.Record
		dependencyRecord *cachestore.Record
	)
	for i := range records {
		record := records[i]
		switch record.Stage {
		case workspaceStageName:
			recordCopy := record
			workspaceRecord = &recordCopy
		case dependencyStageName:
			recordCopy := record
			dependencyRecord = &recordCopy
		}
	}
	if workspaceRecord == nil {
		t.Fatal("expected published workspace stage cache record")
	}
	if dependencyRecord == nil {
		t.Fatal("expected published dependency stage cache record")
	}

	compiled, err := policy.FromProto(testRepositoryDependencyPolicy())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(repositoryCheckout)
	workspaceKey := workspaceStageCacheKey("firecracker", "runtime-base:test", compiled.Hash, repository, nil)
	plan, ok := dependencyStagePlanForRepository(compiled, repository)
	if !ok {
		t.Fatal("expected configured dependency stage plan to be enabled")
	}
	plan, ok, err = svc.finalizeDependencyStagePlan(context.Background(), compiled, repository, nil, "firecracker", workspaceKey, "runtime-base:test", plan)
	if err != nil {
		t.Fatalf("finalizeDependencyStagePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected configured dependency stage cache plan to be enabled")
	}
	if got, want := dependencyRecord.CacheKey, plan.CacheKey; got != want {
		t.Fatalf("unexpected dependency stage cache key: got %q want %q", got, want)
	}
	if got, want := dependencyRecord.ParentCacheKey, workspaceKey; got != want {
		t.Fatalf("unexpected dependency stage parent cache key: got %q want %q", got, want)
	}

	runMu.Lock()
	defer runMu.Unlock()
	if got, want := len(runCommands), 2; got != want {
		t.Fatalf("expected two bootstrap commands, got %d want %d", got, want)
	}
	dependencyBootstrap := strings.Join(runCommands[1], " ")
	if !repositoryWrappedCommandContains(dependencyBootstrap, `exec 'mise' 'exec' '--' 'go' 'mod' 'download'`) {
		t.Fatalf("expected configured dependency bootstrap to preserve explicit mise command, got %q", dependencyBootstrap)
	}
}

func TestCreateSandboxReusesDependencyStageCacheForConfiguredDependencies(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{}
	var snapshotReqs []backend.SnapshotRequest
	adapter.createSnapshotFn = func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
		snapshotReqs = append(snapshotReqs, req)
		return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
	}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"go.mod": "module example.com/test\n\ngo 1.26.2\n",
		"go.sum": "example.com/test v0.0.0 h1:abc123\n",
	})
	svc := newTestServiceWithSnapshotStore(adapter, store)
	svc.RepositoryStore = mirrors

	req := &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryDependencyPolicy(),
		RepositoryCheckout: repositoryCheckout,
	}

	if _, err := svc.CreateSandbox(context.Background(), req); err != nil {
		t.Fatalf("first CreateSandbox returned error: %v", err)
	}
	mirrors.err = errors.New("offline")
	secondResp, err := svc.CreateSandbox(context.Background(), req)
	if err != nil {
		t.Fatalf("second CreateSandbox returned error: %v", err)
	}
	if got, want := secondResp.GetSourceKind(), "dependency stage cache"; got != want {
		t.Fatalf("unexpected response source kind: got %q want %q", got, want)
	}
	if got, want := secondResp.GetSandbox().GetSourceKind(), "dependency stage cache"; got != want {
		t.Fatalf("unexpected sandbox source kind: got %q want %q", got, want)
	}
	records, err := svc.CacheStore.List(context.Background())
	if err != nil {
		t.Fatalf("List cache records returned error: %v", err)
	}
	var dependencyRecord *cachestore.Record
	for i := range records {
		if records[i].Stage == "dependency" {
			dependencyRecord = &records[i]
			break
		}
	}
	if dependencyRecord == nil {
		t.Fatal("expected published dependency stage cache record")
	}
	if got, want := secondResp.GetSourceId(), dependencyRecord.CacheKey; got != want {
		t.Fatalf("unexpected response source id: got %q want %q", got, want)
	}
	if got, want := secondResp.GetBackingSnapshotId(), dependencyRecord.BackingSnapshotID; got != want {
		t.Fatalf("unexpected response backing snapshot id: got %q want %q", got, want)
	}
	if got, want := secondResp.GetSandbox().GetSourceId(), dependencyRecord.CacheKey; got != want {
		t.Fatalf("unexpected sandbox source id: got %q want %q", got, want)
	}
	if got, want := secondResp.GetSandbox().GetBackingSnapshotId(), dependencyRecord.BackingSnapshotID; got != want {
		t.Fatalf("unexpected sandbox backing snapshot id: got %q want %q", got, want)
	}

	if got, want := adapter.provisionCalls, 1; got != want {
		t.Fatalf("expected warm dependency-stage hit to avoid reprovision bootstrap path, got %d want %d", got, want)
	}
	if got, want := adapter.provisionFromSnapshotCalls, 1; got != want {
		t.Fatalf("expected warm dependency-stage hit to restore once, got %d want %d", got, want)
	}
	if got, want := adapter.runCalls, 2; got != want {
		t.Fatalf("expected warm dependency-stage hit to skip bootstrap executions, got %d want %d", got, want)
	}
	if got, want := adapter.createSnapshotCalls, 2; got != want {
		t.Fatalf("expected warm dependency-stage hit to avoid republishing stage caches, got %d want %d", got, want)
	}
	if got, want := len(snapshotReqs), 2; got != want {
		t.Fatalf("expected one workspace and one dependency snapshot publish, got %d want %d", got, want)
	}
	if got, want := mirrors.calls, 1; got != want {
		t.Fatalf("expected only the first dependency-stage key resolution to refresh the mirror, got %d want %d", got, want)
	}
	if got, want := mirrors.mirrorPathCalls, 2; got != want {
		t.Fatalf("expected dependency-stage keying to use local mirror paths on both attempts, got %d want %d", got, want)
	}
	if got, want := adapter.provisionFromSnapshotReq.StorageRef, "/snapshots/"+snapshotReqs[1].SnapshotID+".ext4"; got != want {
		t.Fatalf("unexpected dependency stage storage ref on warm hit: got %q want %q", got, want)
	}
	if got, want := adapter.provisionFromSnapshotReq.SnapshotID, snapshotReqs[1].SnapshotID; got != want {
		t.Fatalf("unexpected dependency stage snapshot id on warm hit: got %q want %q", got, want)
	}
}

func TestCreateSandboxPublishesServicesStageCacheForConfiguredServices(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{}
	var (
		runMu        sync.Mutex
		runCommands  [][]string
		snapshotReqs []backend.SnapshotRequest
	)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		runMu.Lock()
		runCommands = append(runCommands, append([]string(nil), req.Command...))
		runMu.Unlock()
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}
	adapter.createSnapshotFn = func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
		snapshotReqs = append(snapshotReqs, req)
		return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
	}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"go.mod":             "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":             "example.com/test v0.0.0 h1:abc123\n",
		"docker-compose.yml": "services:\n  postgres:\n    image: postgres:17\n",
	})
	svc := newTestServiceWithSnapshotStore(adapter, store)
	svc.RepositoryStore = mirrors

	if _, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryDependencyAndServicesPolicy(),
		RepositoryCheckout: repositoryCheckout,
	}); err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	if got, want := adapter.provisionCalls, 1; got != want {
		t.Fatalf("unexpected provision call count: got %d want %d", got, want)
	}
	if got, want := adapter.runCalls, 3; got != want {
		t.Fatalf("expected repository + dependency + services bootstrap executions, got %d want %d", got, want)
	}
	if got, want := adapter.createSnapshotCalls, 3; got != want {
		t.Fatalf("expected workspace + dependency + services stage snapshots, got %d want %d", got, want)
	}
	if got, want := len(snapshotReqs), 3; got != want {
		t.Fatalf("expected three snapshot publish requests, got %d want %d", got, want)
	}

	cacheStore, ok := svc.CacheStore.(*memoryCacheStore)
	if !ok {
		t.Fatalf("expected memory cache store, got %T", svc.CacheStore)
	}
	records, err := cacheStore.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if got, want := len(records), 3; got != want {
		t.Fatalf("expected workspace + dependency + services cache records, got %d want %d", got, want)
	}

	var (
		workspaceRecord  *cachestore.Record
		dependencyRecord *cachestore.Record
		servicesRecord   *cachestore.Record
	)
	for i := range records {
		record := records[i]
		switch record.Stage {
		case workspaceStageName:
			recordCopy := record
			workspaceRecord = &recordCopy
		case dependencyStageName:
			recordCopy := record
			dependencyRecord = &recordCopy
		case servicesStageName:
			recordCopy := record
			servicesRecord = &recordCopy
		}
	}
	if workspaceRecord == nil {
		t.Fatal("expected published workspace stage cache record")
	}
	if dependencyRecord == nil {
		t.Fatal("expected published dependency stage cache record")
	}
	if servicesRecord == nil {
		t.Fatal("expected published services stage cache record")
	}

	compiled, err := policy.FromProto(testRepositoryDependencyAndServicesPolicy())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(repositoryCheckout)
	workspaceKey := workspaceStageCacheKey("firecracker", "runtime-base:test", compiled.Hash, repository, nil)
	dependencyPlan, ok := dependencyStagePlanForRepository(compiled, repository)
	if !ok {
		t.Fatal("expected configured dependency stage plan to be enabled")
	}
	dependencyPlan, ok, err = svc.finalizeDependencyStagePlan(context.Background(), compiled, repository, nil, "firecracker", workspaceKey, "runtime-base:test", dependencyPlan)
	if err != nil {
		t.Fatalf("finalizeDependencyStagePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected configured dependency stage cache plan to be enabled")
	}
	servicesPlan, ok := servicesStagePlanForRepository(compiled, repository)
	if !ok {
		t.Fatal("expected configured services stage plan to be enabled")
	}
	servicesPlan, ok, err = svc.finalizeServicesStagePlan(context.Background(), compiled, repository, nil, dependencyPlan.CacheKey, servicesPlan)
	if err != nil {
		t.Fatalf("finalizeServicesStagePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected configured services stage cache plan to be enabled")
	}
	if got, want := servicesRecord.CacheKey, servicesPlan.CacheKey; got != want {
		t.Fatalf("unexpected services stage cache key: got %q want %q", got, want)
	}
	if got, want := servicesRecord.ParentCacheKey, dependencyPlan.CacheKey; got != want {
		t.Fatalf("unexpected services stage parent cache key: got %q want %q", got, want)
	}

	runMu.Lock()
	defer runMu.Unlock()
	if got, want := len(runCommands), 3; got != want {
		t.Fatalf("expected three bootstrap commands, got %d want %d", got, want)
	}
	servicesBootstrap := strings.Join(runCommands[2], " ")
	if !repositoryWrappedCommandContains(servicesBootstrap, `exec 'docker' 'compose' 'up' '-d' 'postgres'`) {
		t.Fatalf("expected services bootstrap to preserve explicit docker compose command, got %q", servicesBootstrap)
	}
}

func TestCreateSandboxReusesServicesStageCacheForConfiguredServices(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{}
	var snapshotReqs []backend.SnapshotRequest
	adapter.createSnapshotFn = func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
		snapshotReqs = append(snapshotReqs, req)
		return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
	}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"go.mod":             "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":             "example.com/test v0.0.0 h1:abc123\n",
		"docker-compose.yml": "services:\n  postgres:\n    image: postgres:17\n",
	})
	svc := newTestServiceWithSnapshotStore(adapter, store)
	svc.RepositoryStore = mirrors

	req := &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryDependencyAndServicesPolicy(),
		RepositoryCheckout: repositoryCheckout,
	}

	if _, err := svc.CreateSandbox(context.Background(), req); err != nil {
		t.Fatalf("first CreateSandbox returned error: %v", err)
	}
	mirrors.err = errors.New("offline")
	secondResp, err := svc.CreateSandbox(context.Background(), req)
	if err != nil {
		t.Fatalf("second CreateSandbox returned error: %v", err)
	}
	if got, want := secondResp.GetSourceKind(), "services stage cache"; got != want {
		t.Fatalf("unexpected response source kind: got %q want %q", got, want)
	}
	if got, want := secondResp.GetSandbox().GetSourceKind(), "services stage cache"; got != want {
		t.Fatalf("unexpected sandbox source kind: got %q want %q", got, want)
	}
	records, err := svc.CacheStore.List(context.Background())
	if err != nil {
		t.Fatalf("List cache records returned error: %v", err)
	}
	var servicesRecord *cachestore.Record
	for i := range records {
		if records[i].Stage == servicesStageName {
			servicesRecord = &records[i]
			break
		}
	}
	if servicesRecord == nil {
		t.Fatal("expected published services stage cache record")
	}
	if got, want := secondResp.GetSourceId(), servicesRecord.CacheKey; got != want {
		t.Fatalf("unexpected response source id: got %q want %q", got, want)
	}
	if got, want := secondResp.GetBackingSnapshotId(), servicesRecord.BackingSnapshotID; got != want {
		t.Fatalf("unexpected response backing snapshot id: got %q want %q", got, want)
	}
	if got, want := secondResp.GetSandbox().GetSourceId(), servicesRecord.CacheKey; got != want {
		t.Fatalf("unexpected sandbox source id: got %q want %q", got, want)
	}
	if got, want := secondResp.GetSandbox().GetBackingSnapshotId(), servicesRecord.BackingSnapshotID; got != want {
		t.Fatalf("unexpected sandbox backing snapshot id: got %q want %q", got, want)
	}

	if got, want := adapter.provisionCalls, 1; got != want {
		t.Fatalf("expected warm services-stage hit to avoid reprovision bootstrap path, got %d want %d", got, want)
	}
	if got, want := adapter.provisionFromSnapshotCalls, 1; got != want {
		t.Fatalf("expected warm services-stage hit to restore once, got %d want %d", got, want)
	}
	if got, want := adapter.runCalls, 3; got != want {
		t.Fatalf("expected warm services-stage hit to skip bootstrap executions, got %d want %d", got, want)
	}
	if got, want := adapter.createSnapshotCalls, 3; got != want {
		t.Fatalf("expected warm services-stage hit to avoid republishing stage caches, got %d want %d", got, want)
	}
	if got, want := len(snapshotReqs), 3; got != want {
		t.Fatalf("expected one workspace, one dependency, and one services snapshot publish, got %d want %d", got, want)
	}
	if got, want := mirrors.calls, 1; got != want {
		t.Fatalf("expected only the first repository bootstrap to ensure the exact commit, got %d want %d", got, want)
	}
	if got, want := mirrors.mirrorPathCalls, 4; got != want {
		t.Fatalf("expected dependency and services stage keying to use local mirror paths on both attempts, got %d want %d", got, want)
	}
	if got, want := adapter.provisionFromSnapshotReq.StorageRef, "/snapshots/"+snapshotReqs[2].SnapshotID+".ext4"; got != want {
		t.Fatalf("unexpected services stage storage ref on warm hit: got %q want %q", got, want)
	}
	if got, want := adapter.provisionFromSnapshotReq.SnapshotID, snapshotReqs[2].SnapshotID; got != want {
		t.Fatalf("unexpected services stage snapshot id on warm hit: got %q want %q", got, want)
	}
}

func TestCreateSandboxBootstrapsServicesAfterDependencyStageRestoreWithoutServicesStageCache(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{}
	var snapshotReqs []backend.SnapshotRequest
	adapter.createSnapshotFn = func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
		snapshotReqs = append(snapshotReqs, req)
		return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
	}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"go.mod":             "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":             "example.com/test v0.0.0 h1:abc123\n",
		"docker-compose.yml": "services:\n  postgres:\n    image: postgres:17\n",
	})
	svc := newTestServiceWithSnapshotStore(adapter, store)
	svc.RepositoryStore = mirrors

	req := &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryDependencyAndServicesPolicy(),
		RepositoryCheckout: repositoryCheckout,
	}

	if _, err := svc.CreateSandbox(context.Background(), req); err != nil {
		t.Fatalf("first CreateSandbox returned error: %v", err)
	}

	records, err := svc.CacheStore.List(context.Background())
	if err != nil {
		t.Fatalf("List cache records returned error: %v", err)
	}
	var dependencyRecord *cachestore.Record
	for _, record := range records {
		switch record.Stage {
		case dependencyStageName:
			recordCopy := record
			dependencyRecord = &recordCopy
		case servicesStageName:
			if err := svc.CacheStore.Delete(context.Background(), record.Stage, record.CacheKey); err != nil {
				t.Fatalf("Delete services cache record returned error: %v", err)
			}
		}
	}
	if dependencyRecord == nil {
		t.Fatal("expected published dependency stage cache record")
	}

	secondResp, err := svc.CreateSandbox(context.Background(), req)
	if err != nil {
		t.Fatalf("second CreateSandbox returned error: %v", err)
	}
	if got, want := secondResp.GetSourceKind(), "dependency stage cache"; got != want {
		t.Fatalf("unexpected response source kind: got %q want %q", got, want)
	}
	if got, want := adapter.provisionCalls, 1; got != want {
		t.Fatalf("expected dependency-stage restore path to avoid cold provision, got %d want %d", got, want)
	}
	if got, want := adapter.provisionFromSnapshotCalls, 1; got != want {
		t.Fatalf("expected exactly one dependency-stage restore, got %d want %d", got, want)
	}
	if got, want := adapter.runCalls, 4; got != want {
		t.Fatalf("expected only one extra services bootstrap after dependency restore, got %d want %d", got, want)
	}
	if got, want := adapter.createSnapshotCalls, 4; got != want {
		t.Fatalf("expected only one extra services-stage snapshot publish, got %d want %d", got, want)
	}
	if got, want := adapter.provisionFromSnapshotReq.SnapshotID, dependencyRecord.BackingSnapshotID; got != want {
		t.Fatalf("unexpected restore snapshot id: got %q want %q", got, want)
	}
	if got, want := len(snapshotReqs), 4; got != want {
		t.Fatalf("expected one replacement services-stage snapshot publish, got %d want %d", got, want)
	}
}

func TestCreateSandboxReusesPortableDependencyStageAfterCheckoutRefresh(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{}
	var snapshotReqs []backend.SnapshotRequest
	var runCommands [][]string
	validationDigest := ""
	adapter.createSnapshotFn = func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
		snapshotReqs = append(snapshotReqs, req)
		return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
	}
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		runCommands = append(runCommands, append([]string(nil), req.Command...))
		if strings.Contains(strings.Join(req.Command, "\n"), "sha256sum") && stream.OnStdout != nil {
			stream.OnStdout([]byte(validationDigest + "\n"))
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"go.mod": "module example.com/test\n\ngo 1.26.2\n",
		"go.sum": "example.com/test v0.0.0 h1:abc123\n",
		"app.go": "package main\n\nfunc main() {}\n",
	})
	if err := os.WriteFile(filepath.Join(mirrors.mirrorPath, "app.go"), []byte("package main\n\nfunc main() { println(\"changed\") }\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(app.go) returned error: %v", err)
	}
	runTestGit(t, mirrors.mirrorPath, "add", ".")
	runTestGit(t, mirrors.mirrorPath, "commit", "-m", "source-only change")
	updatedCommit := strings.TrimSpace(runTestGit(t, mirrors.mirrorPath, "rev-parse", "HEAD"))
	updatedCheckout := *repositoryCheckout
	updatedCheckout.CommitSha = updatedCommit

	svc := newTestServiceWithSnapshotStore(adapter, store)
	svc.RepositoryStore = mirrors
	var err error
	validationDigest, err = svc.dependencyStageKeyFilesDigest(context.Background(), repositorycheckout.FromProto(&updatedCheckout), nil, []string{"go.mod", "go.sum"})
	if err != nil {
		t.Fatalf("dependencyStageKeyFilesDigest returned error: %v", err)
	}

	if _, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryPortableDependencyPolicy(),
		RepositoryCheckout: repositoryCheckout,
	}); err != nil {
		t.Fatalf("first CreateSandbox returned error: %v", err)
	}
	secondResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryPortableDependencyPolicy(),
		RepositoryCheckout: &updatedCheckout,
	})
	if err != nil {
		t.Fatalf("second CreateSandbox returned error: %v", err)
	}
	if got, want := secondResp.GetSourceKind(), "portable dependency stage cache"; got != want {
		t.Fatalf("unexpected response source kind: got %q want %q", got, want)
	}
	if got, want := secondResp.GetSandbox().GetSourceKind(), "portable dependency stage cache"; got != want {
		t.Fatalf("unexpected sandbox source kind: got %q want %q", got, want)
	}
	if got, want := adapter.provisionCalls, 1; got != want {
		t.Fatalf("expected portable dependency-stage hit to avoid second cold provision, got %d want %d", got, want)
	}
	if got, want := adapter.provisionFromSnapshotCalls, 1; got != want {
		t.Fatalf("expected portable dependency-stage hit to restore once, got %d want %d", got, want)
	}
	if got, want := adapter.createSnapshotCalls, 2; got != want {
		t.Fatalf("expected portable metadata to share the dependency snapshot, got %d snapshot creates", got)
	}
	if got, want := len(snapshotReqs), 2; got != want {
		t.Fatalf("expected workspace and dependency snapshot publishes only, got %d want %d", got, want)
	}
	if got, want := len(runCommands), 4; got != want {
		t.Fatalf("expected repository bootstrap, dependency bootstrap, and checkout refresh, got %d commands", got)
	}
	refreshCommand := strings.Join(runCommands[2], "\n")
	if !strings.Contains(refreshCommand, updatedCommit) || !strings.Contains(refreshCommand, `git -C "$dest" fetch --filter=blob:none --progress origin "$commit"`) {
		t.Fatalf("expected portable hit to refresh checkout to %s, got %q", updatedCommit, refreshCommand)
	}
	if dependencyBootstrap := strings.Join(runCommands[1], " "); !repositoryWrappedCommandContains(dependencyBootstrap, `exec 'mise' 'exec' '--' 'go' 'mod' 'download'`) {
		t.Fatalf("expected first run to bootstrap dependencies, got %q", dependencyBootstrap)
	}
	if validationCommand := strings.Join(runCommands[3], "\n"); !strings.Contains(validationCommand, "sha256sum") {
		t.Fatalf("expected portable hit to validate dependency key files, got %q", validationCommand)
	}

	cacheStore, ok := svc.CacheStore.(*memoryCacheStore)
	if !ok {
		t.Fatalf("expected memory cache store, got %T", svc.CacheStore)
	}
	records, err := cacheStore.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	var exactRecord, portableRecord *cachestore.Record
	for i := range records {
		switch records[i].ReuseMode {
		case dependencyStageReuseExact:
			record := records[i]
			exactRecord = &record
		case dependencyStageReusePortableSeed:
			record := records[i]
			portableRecord = &record
		}
	}
	if exactRecord == nil {
		t.Fatal("expected exact dependency stage metadata")
	}
	if portableRecord == nil {
		t.Fatal("expected portable dependency stage metadata")
	}
	if got, want := portableRecord.BackingSnapshotID, exactRecord.BackingSnapshotID; got != want {
		t.Fatalf("expected portable record to share exact dependency snapshot: got %q want %q", got, want)
	}
	if got, want := portableRecord.ParentCacheKey, "runtime-base:test"; got != want {
		t.Fatalf("unexpected portable parent cache key: got %q want %q", got, want)
	}
	if portableRecord.DependencyKeyFilesDigest == "" {
		t.Fatal("expected portable dependency key-file digest metadata")
	}

	svc.mu.Lock()
	restoredState := svc.sandboxes[secondResp.GetSandbox().GetSandboxId()]
	svc.mu.Unlock()
	if restoredState == nil || restoredState.Repository == nil {
		t.Fatal("expected restored sandbox repository state")
	}
	if got, want := restoredState.Repository.CommitSHA, updatedCommit; got != want {
		t.Fatalf("expected restored sandbox repository to be refreshed: got %q want %q", got, want)
	}
}

func TestCreateSandboxBootstrapsServicesAfterPortableDependencyStageRestore(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{}
	var snapshotReqs []backend.SnapshotRequest
	var runCommands [][]string
	validationDigest := ""
	adapter.createSnapshotFn = func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
		snapshotReqs = append(snapshotReqs, req)
		return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
	}
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		runCommands = append(runCommands, append([]string(nil), req.Command...))
		if strings.Contains(strings.Join(req.Command, "\n"), "sha256sum") && stream.OnStdout != nil {
			stream.OnStdout([]byte(validationDigest + "\n"))
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"go.mod":             "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":             "example.com/test v0.0.0 h1:abc123\n",
		"docker-compose.yml": "services:\n  postgres:\n    image: postgres:17\n",
		"app.go":             "package main\n\nfunc main() {}\n",
	})
	if err := os.WriteFile(filepath.Join(mirrors.mirrorPath, "app.go"), []byte("package main\n\nfunc main() { println(\"changed\") }\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(app.go) returned error: %v", err)
	}
	runTestGit(t, mirrors.mirrorPath, "add", ".")
	runTestGit(t, mirrors.mirrorPath, "commit", "-m", "source-only change")
	updatedCommit := strings.TrimSpace(runTestGit(t, mirrors.mirrorPath, "rev-parse", "HEAD"))
	updatedCheckout := *repositoryCheckout
	updatedCheckout.CommitSha = updatedCommit

	svc := newTestServiceWithSnapshotStore(adapter, store)
	svc.RepositoryStore = mirrors
	var err error
	validationDigest, err = svc.dependencyStageKeyFilesDigest(context.Background(), repositorycheckout.FromProto(&updatedCheckout), nil, []string{"go.mod", "go.sum"})
	if err != nil {
		t.Fatalf("dependencyStageKeyFilesDigest returned error: %v", err)
	}

	portableServicesPolicy := testRepositoryDependencyAndServicesPolicy()
	portableServicesPolicy.Dependencies.Reuse = policy.DependencyReusePortable
	if _, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             portableServicesPolicy,
		RepositoryCheckout: repositoryCheckout,
	}); err != nil {
		t.Fatalf("first CreateSandbox returned error: %v", err)
	}
	secondResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             portableServicesPolicy,
		RepositoryCheckout: &updatedCheckout,
	})
	if err != nil {
		t.Fatalf("second CreateSandbox returned error: %v", err)
	}
	if got, want := secondResp.GetSourceKind(), "portable dependency stage cache"; got != want {
		t.Fatalf("unexpected response source kind: got %q want %q", got, want)
	}
	if got, want := adapter.provisionCalls, 1; got != want {
		t.Fatalf("expected portable dependency-stage hit to avoid second cold provision, got %d want %d", got, want)
	}
	if got, want := adapter.provisionFromSnapshotCalls, 1; got != want {
		t.Fatalf("expected portable dependency-stage hit to restore once, got %d want %d", got, want)
	}
	if got, want := adapter.createSnapshotCalls, 4; got != want {
		t.Fatalf("expected first workspace/dependency/services snapshots plus refreshed services snapshot, got %d want %d", got, want)
	}
	if got, want := len(snapshotReqs), 4; got != want {
		t.Fatalf("expected four snapshot publish requests, got %d want %d", got, want)
	}
	if got, want := len(runCommands), 6; got != want {
		t.Fatalf("expected cold bootstraps plus portable refresh, validation, and services bootstrap, got %d want %d", got, want)
	}
	if refreshCommand := strings.Join(runCommands[3], "\n"); !strings.Contains(refreshCommand, updatedCommit) {
		t.Fatalf("expected portable hit to refresh checkout to %s, got %q", updatedCommit, refreshCommand)
	}
	servicesBootstrap := strings.Join(runCommands[5], " ")
	if !repositoryWrappedCommandContains(servicesBootstrap, `exec 'docker' 'compose' 'up' '-d' 'postgres'`) {
		t.Fatalf("expected services bootstrap after portable restore, got %q", servicesBootstrap)
	}
}

func TestDependencyStageKeyFilesDigestDerivesHashesFromRepositoryChangesetPatch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"go.mod": "module example.com/test\n\ngo 1.26.2\n",
		"go.sum": "example.com/test v0.0.0 h1:abc123\n",
	})
	repository := repositorycheckout.FromProto(repositoryCheckout)
	repoDir := mirrors.mirrorPath
	if err := os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte("module example.com/test\n\ngo 1.26.2\nrequire example.com/lib v1.0.0\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(go.mod) returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "go.sum"), []byte("example.com/test v0.0.0 h1:abc123\nexample.com/lib v1.0.0 h1:def456\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(go.sum) returned error: %v", err)
	}
	changeset, err := repositorychangeset.BuildFromWorkingTree(repoDir, repository)
	if err != nil {
		t.Fatalf("BuildFromWorkingTree returned error: %v", err)
	}
	if changeset == nil {
		t.Fatal("expected repository changeset for modified dependency key files")
	}

	svc := newTestService(&stubAdapter{})
	svc.RepositoryStore = mirrors

	baseDigest, err := svc.dependencyStageKeyFilesDigest(context.Background(), repository, nil, []string{"go.mod", "go.sum"})
	if err != nil {
		t.Fatalf("dependencyStageKeyFilesDigest without changeset returned error: %v", err)
	}
	changesetDigest, err := svc.dependencyStageKeyFilesDigest(context.Background(), repository, changeset, []string{"go.mod", "go.sum"})
	if err != nil {
		t.Fatalf("dependencyStageKeyFilesDigest with changeset returned error: %v", err)
	}
	if changesetDigest == "" {
		t.Fatal("expected dependency key file digest with repository changeset")
	}
	if changesetDigest == baseDigest {
		t.Fatalf("expected changeset-aware dependency key digest to differ from base digest %q", baseDigest)
	}

	tampered := *changeset
	tampered.Files = append([]repositorychangeset.File(nil), changeset.Files...)
	for i := range tampered.Files {
		if tampered.Files[i].Deleted {
			continue
		}
		tampered.Files[i].SHA256 = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	}

	tamperedDigest, err := svc.dependencyStageKeyFilesDigest(context.Background(), repository, &tampered, []string{"go.mod", "go.sum"})
	if err != nil {
		t.Fatalf("dependencyStageKeyFilesDigest with tampered changeset metadata returned error: %v", err)
	}
	if got, want := tamperedDigest, changesetDigest; got != want {
		t.Fatalf("expected dependency key file digest to ignore tampered file hashes: got %q want %q", got, want)
	}
}

func TestDependencyStageKeyFilesDigestCommandHashesSymlinkTargets(t *testing.T) {
	t.Parallel()

	command, err := dependencyStageKeyFilesDigestCommand(&repositorycheckout.Checkout{DestinationDir: "/workspace"}, []string{"Gemfile.lock"})
	if err != nil {
		t.Fatalf("dependencyStageKeyFilesDigestCommand returned error: %v", err)
	}
	script := strings.Join(command, "\n")
	for _, want := range []string{
		`if [ -L "$path" ]; then`,
		`target="$(readlink "$path")"`,
		`printf '%s' "$target" | sha256sum`,
		`elif [ -e "$path" ]; then`,
		`sha256sum "$path"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("expected validation script to contain %q, got %q", want, script)
		}
	}
}

func TestCreateSandboxBootstrapsDependenciesAfterWorkspaceStageRestoreWithoutDependencyStageCache(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{}
	var snapshotReqs []backend.SnapshotRequest
	adapter.createSnapshotFn = func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
		snapshotReqs = append(snapshotReqs, req)
		return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
	}
	var runCommands [][]string
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		runCommands = append(runCommands, append([]string(nil), req.Command...))
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"go.mod": "module example.com/test\n\ngo 1.26.2\n",
		"go.sum": "example.com/test v0.0.0 h1:abc123\n",
	})
	mirrors.mirrorPathErr = errors.New("mirror path unavailable")
	svc := newTestServiceWithSnapshotStore(adapter, store)
	svc.RepositoryStore = mirrors

	compiled, err := policy.FromProto(testRepositoryDependencyPolicy())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	repository := repositorycheckout.FromProto(repositoryCheckout)
	workspaceRecord := cachestore.Record{
		CacheKey:          workspaceStageCacheKey("firecracker", "runtime-base:test", compiled.Hash, repository, nil),
		Stage:             workspaceStageName,
		State:             cacheStateReady,
		BackingSnapshotID: "workspace-stage-backing-snapshot",
		Backend:           "firecracker",
		PolicyHash:        compiled.Hash,
		Policy:            compiled.ToProto(),
		Repository:        cloneRepositoryCheckout(normalizeRepositoryCheckoutForComparison(repository)).ToProto(),
		ParentCacheKey:    "runtime-base:test",
		StorageDriver:     "file",
		StorageRef:        "/snapshots/workspace-stage-backing-snapshot.ext4",
		ProducerVersion:   workspaceStageProducerVersion,
	}
	cacheStore, ok := svc.CacheStore.(*memoryCacheStore)
	if !ok {
		t.Fatalf("expected memory cache store, got %T", svc.CacheStore)
	}
	if err := cacheStore.Create(context.Background(), workspaceRecord); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	resp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryDependencyPolicy(),
		RepositoryCheckout: repositoryCheckout,
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if resp.GetSandbox().GetSandboxId() == "" {
		t.Fatal("expected sandbox id")
	}
	if got, want := adapter.provisionCalls, 0; got != want {
		t.Fatalf("expected workspace restore path to avoid cold provision, got %d want %d", got, want)
	}
	if got, want := adapter.provisionFromSnapshotCalls, 1; got != want {
		t.Fatalf("expected workspace stage restore once, got %d want %d", got, want)
	}
	if got, want := adapter.runCalls, 1; got != want {
		t.Fatalf("expected dependency bootstrap to run once after workspace restore, got %d want %d", got, want)
	}
	if got, want := adapter.createSnapshotCalls, 1; got != want {
		t.Fatalf("expected dependency cache publish after mirror creation, got %d want %d", got, want)
	}
	if got, want := mirrors.calls, 0; got != want {
		t.Fatalf("expected dependency-stage key resolution to avoid an extra ensure-contains refresh after creating the mirror, got %d want %d", got, want)
	}
	if got, want := mirrors.mirrorPathCalls, 1; got != want {
		t.Fatalf("expected one local mirror path lookup while resolving dependency cache key, got %d want %d", got, want)
	}
	if got, want := mirrors.ensureMirrorCalls, 1; got != want {
		t.Fatalf("expected dependency-stage key resolution to create a local mirror, got %d want %d", got, want)
	}
	if got, want := len(snapshotReqs), 1; got != want {
		t.Fatalf("expected one dependency-stage snapshot publish, got %d want %d", got, want)
	}
	records, err := cacheStore.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	dependencyRecords := 0
	for _, record := range records {
		if record.Stage == dependencyStageName {
			dependencyRecords++
		}
	}
	if got, want := dependencyRecords, 1; got != want {
		t.Fatalf("expected one published dependency-stage cache record, got %d want %d", got, want)
	}
	if got, want := len(runCommands), 1; got != want {
		t.Fatalf("expected one dependency bootstrap command, got %d want %d", got, want)
	}
	dependencyBootstrap := strings.Join(runCommands[0], " ")
	if !repositoryWrappedCommandContains(dependencyBootstrap, `exec 'mise' 'exec' '--' 'go' 'mod' 'download'`) {
		t.Fatalf("expected dependency bootstrap to preserve explicit mise command, got %q", dependencyBootstrap)
	}
}

func TestCreateSandboxPublishesDependencyStageCacheWhenLocalMirrorStartsEmpty(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{}
	var snapshotReqs []backend.SnapshotRequest
	adapter.createSnapshotFn = func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
		snapshotReqs = append(snapshotReqs, req)
		return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
	}

	actualMirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"go.mod": "module example.com/test\n\ngo 1.26.2\n",
		"go.sum": "example.com/test v0.0.0 h1:abc123\n",
	})
	emptyMirrorPath := filepath.Join(t.TempDir(), "missing.git")
	mirrors := &stubRepositoryMirrorStore{mirrorPath: emptyMirrorPath}
	mirrors.ensureContainsFn = func(remoteURL, commitSHA string) error {
		mirrors.mirrorPath = actualMirrors.mirrorPath
		return nil
	}

	svc := newTestServiceWithSnapshotStore(adapter, store)
	svc.RepositoryStore = mirrors

	if _, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryDependencyPolicy(),
		RepositoryCheckout: repositoryCheckout,
	}); err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	if got, want := adapter.createSnapshotCalls, 2; got != want {
		t.Fatalf("expected workspace and dependency stage snapshots after populating mirror, got %d want %d", got, want)
	}
	if got, want := len(snapshotReqs), 2; got != want {
		t.Fatalf("expected one workspace and one dependency stage publish request, got %d want %d", got, want)
	}
	if got, want := mirrors.calls, 2; got != want {
		t.Fatalf("expected dependency keying fallback plus repository bootstrap to ensure the exact commit, got %d want %d", got, want)
	}

	cacheStore, ok := svc.CacheStore.(*memoryCacheStore)
	if !ok {
		t.Fatalf("expected memory cache store, got %T", svc.CacheStore)
	}
	records, err := cacheStore.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if got, want := len(records), 2; got != want {
		t.Fatalf("expected workspace and dependency cache records, got %d want %d", got, want)
	}

	var dependencyRecord *cachestore.Record
	for i := range records {
		record := records[i]
		if record.Stage == dependencyStageName {
			recordCopy := record
			dependencyRecord = &recordCopy
		}
	}
	if dependencyRecord == nil {
		t.Fatal("expected published dependency stage cache record")
	}
}

func TestMemoryCacheStoreRejectsDuplicateStageCacheKey(t *testing.T) {
	t.Parallel()

	store := newMemoryCacheStore()
	record := cachestore.Record{
		CacheKey:        "workspace:test",
		Stage:           workspaceStageName,
		State:           cacheStateReady,
		Backend:         "firecracker",
		PolicyHash:      "policy-hash",
		Policy:          testRepositoryPolicy(),
		StorageRef:      "/tmp/workspace-test.ext4",
		StorageDriver:   "file",
		ProducerVersion: workspaceStageProducerVersion,
	}

	if err := store.Create(context.Background(), record); err != nil {
		t.Fatalf("first Create returned error: %v", err)
	}
	if err := store.Create(context.Background(), record); err == nil {
		t.Fatal("expected duplicate cache insert to fail")
	}

	items, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if got, want := len(items), 1; got != want {
		t.Fatalf("expected duplicate cache insert to keep one record, got %d want %d", got, want)
	}
}

func TestTerminateCreatedSandboxKeepsStateWhenTerminateFails(t *testing.T) {
	adapter := &stubAdapter{
		terminateFn: func(_ context.Context, sandboxID string) error {
			return errors.New("terminate failed")
		},
	}
	svc := newTestService(adapter)
	sb := &sandboxState{
		ID:        "sandbox_test",
		Status:    cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY,
		CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
		UpdatedAt: time.Unix(1_700_000_000, 0).UTC(),
		events:    newEventFeed[*cleanroomv1.SandboxEvent](0),
		Done:      make(chan struct{}),
	}
	svc.mu.Lock()
	svc.ensureMapsLocked()
	svc.sandboxes[sb.ID] = sb
	svc.mu.Unlock()

	err := svc.terminateCreatedSandbox(context.Background(), adapter, sb.ID)
	if err == nil {
		t.Fatal("expected terminateCreatedSandbox to return an error")
	}
	if got, want := adapter.terminateCalls, 1; got != want {
		t.Fatalf("expected one backend terminate attempt, got %d want %d", got, want)
	}

	svc.mu.RLock()
	kept, ok := svc.sandboxes[sb.ID]
	svc.mu.RUnlock()
	if !ok || kept == nil {
		t.Fatal("expected sandbox state to remain after terminate failure")
	}
	if kept.DoneClosed {
		t.Fatal("expected sandbox done channel to remain open after terminate failure")
	}
}

func TestCreateExecutionWrapsRepositoryBootstrapInService(t *testing.T) {
	adapter := &stubAdapter{}
	mirrors := &stubRepositoryMirrorStore{}
	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors
	runCalled := make(chan struct{}, 4)
	var (
		mu       sync.Mutex
		commands [][]string
	)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		select {
		case runCalled <- struct{}{}:
		default:
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testRepositoryPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId:          sandboxID,
		Command:            []string{"sh", "-lc", "pwd"},
		RepositoryCheckout: testRepositoryCheckoutProto(),
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	if createExecutionResp.GetExecution().GetExecutionId() == "" {
		t.Fatal("expected execution id")
	}
	for i := 0; i < 2; i++ {
		select {
		case <-runCalled:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for repository bootstrap execution")
		}
	}
	if got, want := mirrors.calls, 1; got != want {
		t.Fatalf("unexpected mirror prewarm call count: got %d want %d", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if got, want := len(commands), 2; got != want {
		t.Fatalf("expected bootstrap + user execution, got %d command(s)", got)
	}
	bootstrap := strings.Join(commands[0], " ")
	if !strings.Contains(bootstrap, "git clone --filter=blob:none --no-checkout") {
		t.Fatalf("expected repository bootstrap clone in command, got %q", bootstrap)
	}
	joined := strings.Join(commands[1], " ")
	if strings.Contains(joined, "git clone --filter=blob:none --no-checkout") {
		t.Fatalf("expected execution command to run after bootstrap, got %q", joined)
	}
	if !repositoryWrappedCommandContains(joined, `exec 'sh' '-lc' 'pwd'`) {
		t.Fatalf("expected wrapped user command in repository workdir, got %q", joined)
	}
	if strings.Contains(bootstrap, "Authorization:") || strings.Contains(bootstrap, ".extraHeader") {
		t.Fatalf("expected bootstrap command to avoid embedded auth, got %q", bootstrap)
	}
	if strings.Contains(joined, "Authorization:") || strings.Contains(joined, ".extraHeader") {
		t.Fatalf("expected wrapped command to avoid embedded auth, got %q", joined)
	}
	if got := strings.Join(createExecutionResp.GetExecution().GetCommand(), " "); strings.Contains(got, "Authorization:") || strings.Contains(got, ".extraHeader") {
		t.Fatalf("expected execution command snapshot to avoid embedded auth, got %q", got)
	}
}

func TestCreateExecutionSkipsBootstrapForMatchingPersistentRepository(t *testing.T) {
	adapter := &stubAdapter{}
	mirrors := &stubRepositoryMirrorStore{}
	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors

	var (
		mu       sync.Mutex
		commands [][]string
	)
	runCalled := make(chan struct{}, 4)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		select {
		case runCalled <- struct{}{}:
		default:
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryPolicy(),
		RepositoryCheckout: testRepositoryCheckoutProto(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()
	select {
	case <-runCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for sandbox bootstrap")
	}

	_, err = svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId:          sandboxID,
		Command:            []string{"sh", "-lc", "pwd"},
		RepositoryCheckout: testRepositoryCheckoutProto(),
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	select {
	case <-runCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for execution")
	}
	mu.Lock()
	defer mu.Unlock()
	if got, want := len(commands), 2; got != want {
		t.Fatalf("expected create bootstrap + execution, got %d command(s)", got)
	}
	joined := strings.Join(commands[1], " ")
	if strings.Contains(joined, "git clone --filter=blob:none --no-checkout") {
		t.Fatalf("expected matching repository execution to reuse existing checkout, got %q", joined)
	}
	if !repositoryWrappedCommandContains(joined, `exec 'sh' '-lc' 'pwd'`) {
		t.Fatalf("expected matching repository execution to run inside repository workdir, got %q", joined)
	}
	if got, want := mirrors.calls, 1; got != want {
		t.Fatalf("expected mirror prewarm only during sandbox create, got %d call(s)", got)
	}
}

func TestCreateExecutionRunsRunBeforeBeforeUserCommand(t *testing.T) {
	adapter := &stubAdapter{}
	svc := newTestService(adapter)

	var (
		mu       sync.Mutex
		commands [][]string
	)
	runCalled := make(chan struct{}, 4)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		select {
		case runCalled <- struct{}{}:
		default:
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryRunBeforePolicy(),
		RepositoryCheckout: testRepositoryCheckoutProto(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()
	select {
	case <-runCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for sandbox bootstrap")
	}

	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"sh", "-lc", "pwd"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	if createExecutionResp.GetExecution().GetExecutionId() == "" {
		t.Fatal("expected execution id")
	}
	for i := 0; i < 2; i++ {
		select {
		case <-runCalled:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for run.before + user execution")
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if got, want := len(commands), 3; got != want {
		t.Fatalf("expected create bootstrap, run.before, and user execution, got %d command(s)", got)
	}
	if runBefore := strings.Join(commands[1], " "); !repositoryWrappedCommandContains(runBefore, `exec 'sh' '-lc' 'echo pre-run'`) {
		t.Fatalf("expected wrapped run.before command, got %q", runBefore)
	}
	if joined := strings.Join(commands[2], " "); !repositoryWrappedCommandContains(joined, `exec 'sh' '-lc' 'pwd'`) {
		t.Fatalf("expected wrapped user command, got %q", joined)
	}
}

func TestCreateExecutionPassesEnvToRunBefore(t *testing.T) {
	adapter := &stubAdapter{}
	svc := newTestService(adapter)

	var (
		mu   sync.Mutex
		envs [][]string
	)
	runCalled := make(chan struct{}, 4)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		envs = append(envs, append([]string(nil), req.Env...))
		mu.Unlock()
		select {
		case runCalled <- struct{}{}:
		default:
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryRunBeforePolicy(),
		RepositoryCheckout: testRepositoryCheckoutProto(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()
	select {
	case <-runCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for sandbox bootstrap")
	}

	_, err = svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"sh", "-lc", "pwd"},
		Env:       []string{"DATABASE_URL=postgres://example", "REDIS_URL=redis://example"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-runCalled:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for run.before + user execution")
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if got, want := len(envs), 3; got != want {
		t.Fatalf("expected create bootstrap, run.before, and user execution env captures, got %d", got)
	}
	for i, got := range envs[1:] {
		if want := []string{"DATABASE_URL=postgres://example", "REDIS_URL=redis://example"}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("unexpected env on command #%d: got %v want %v", i+2, got, want)
		}
	}
}

func TestWriteExecutionStdinWaitsForRunBeforeToComplete(t *testing.T) {
	preRunStarted := make(chan struct{}, 1)
	stdinChunks := make(chan string, 1)
	adapter := &stubAdapter{
		runStreamFn: func(ctx context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			switch strings.Join(req.Command, " ") {
			case "sh -lc echo pre-run":
				select {
				case preRunStarted <- struct{}{}:
				default:
				}
				time.Sleep(150 * time.Millisecond)
				return &backend.ExecutionResult{
					ExecutionID: req.ExecutionID,
					ExitCode:    0,
					Message:     "ok",
				}, nil
			case "sh":
				if stream.OnAttach != nil {
					stream.OnAttach(backend.AttachIO{
						WriteStdin: func(data []byte) error {
							stdinChunks <- string(data)
							return nil
						},
					})
				}
				<-ctx.Done()
				return nil, ctx.Err()
			default:
				t.Fatalf("unexpected command: %v", req.Command)
				return nil, nil
			}
		},
	}
	svc := newTestService(adapter)
	timeouts := defaultServiceTimeouts
	timeouts.attachStdinRegistrationWait = 100 * time.Millisecond
	svc.runtime.timeouts = &timeouts

	policyProto := testPolicy()
	policyProto.Run = &cleanroomv1.PolicyRun{
		Before: []string{"sh", "-lc", "echo pre-run"},
	}

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: policyProto})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"sh"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()

	select {
	case <-preRunStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for run.before to start")
	}

	if err := svc.WriteExecutionStdin(sandboxID, executionID, []byte("hello\n")); err != nil {
		t.Fatalf("WriteExecutionStdin returned error: %v", err)
	}

	select {
	case got := <-stdinChunks:
		if got != "hello\n" {
			t.Fatalf("unexpected stdin payload: got %q want %q", got, "hello\n")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stdin after run.before")
	}

	if _, err := svc.CancelExecution(context.Background(), &cleanroomv1.CancelExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Signal:      2,
	}); err != nil {
		t.Fatalf("CancelExecution returned error: %v", err)
	}
	if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}
}

func TestCreateExecutionPreservesRunBeforeOutputInFinalSnapshot(t *testing.T) {
	adapter := &stubAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			switch strings.Join(req.Command, " ") {
			case "sh -lc echo pre-run":
				return &backend.ExecutionResult{
					ExecutionID: req.ExecutionID,
					ExitCode:    0,
					Message:     "ok",
					Stdout:      "pre-run output\n",
				}, nil
			case "sh -lc pwd":
				return &backend.ExecutionResult{
					ExecutionID: req.ExecutionID,
					ExitCode:    0,
					Message:     "ok",
					Stdout:      "user output\n",
				}, nil
			default:
				t.Fatalf("unexpected command: %v", req.Command)
				return nil, nil
			}
		},
	}
	svc := newTestService(adapter)

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy: testRepositoryRunBeforePolicy(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"sh", "-lc", "pwd"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()

	if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}

	snapshot, err := svc.ExecutionSnapshot(sandboxID, executionID)
	if err != nil {
		t.Fatalf("ExecutionSnapshot returned error: %v", err)
	}
	if got, want := snapshot.Stdout, "pre-run output\nuser output\n"; got != want {
		t.Fatalf("unexpected retained stdout: got %q want %q", got, want)
	}
}

func TestCreateExecutionRetainsRunBeforeFailureArtifacts(t *testing.T) {
	adapter := &stubAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			switch strings.Join(req.Command, " ") {
			case "sh -lc echo pre-run":
				return &backend.ExecutionResult{
					ExecutionID: req.ExecutionID,
					ExitCode:    23,
					LaunchedVM:  true,
					PlanPath:    "/tmp/pre-run-plan",
					RunDir:      "/tmp/pre-run-run",
					Message:     "pre-run failed",
					Stdout:      "pre-run output\n",
				}, nil
			default:
				t.Fatalf("unexpected command: %v", req.Command)
				return nil, nil
			}
		},
	}
	svc := newTestService(adapter)

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy: testRepositoryRunBeforePolicy(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"sh", "-lc", "pwd"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()

	if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}

	snapshot, err := svc.ExecutionSnapshot(sandboxID, executionID)
	if err != nil {
		t.Fatalf("ExecutionSnapshot returned error: %v", err)
	}
	if got, want := snapshot.RunDir, "/tmp/pre-run-run"; got != want {
		t.Fatalf("unexpected run dir: got %q want %q", got, want)
	}
	if got, want := snapshot.PlanPath, "/tmp/pre-run-plan"; got != want {
		t.Fatalf("unexpected plan path: got %q want %q", got, want)
	}
	if got, want := snapshot.Launched, true; got != want {
		t.Fatalf("unexpected launched flag: got %t want %t", got, want)
	}
	if got, want := snapshot.Stdout, "pre-run output\n"; got != want {
		t.Fatalf("unexpected retained stdout: got %q want %q", got, want)
	}
	if got, want := snapshot.Execution.GetExitCode(), int32(23); got != want {
		t.Fatalf("unexpected exit code: got %d want %d", got, want)
	}
}

func TestCreateExecutionRunBeforeFailureClearsRuntimeHandles(t *testing.T) {
	adapter := &stubAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			switch strings.Join(req.Command, " ") {
			case "sh -lc echo pre-run":
				return &backend.ExecutionResult{
					ExecutionID: req.ExecutionID,
					ExitCode:    23,
					Message:     "pre-run failed",
				}, nil
			default:
				t.Fatalf("unexpected command: %v", req.Command)
				return nil, nil
			}
		},
	}
	svc := newTestService(adapter)

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy: testRepositoryRunBeforePolicy(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"sh", "-lc", "pwd"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()

	if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}

	svc.mu.RLock()
	defer svc.mu.RUnlock()
	ex, ok := svc.executions[executionKey(sandboxID, executionID)]
	if !ok {
		t.Fatalf("expected execution %q to remain available", executionID)
	}
	if ex.Cancel != nil {
		t.Fatal("expected run.before failure to clear retained cancel handler")
	}
	if ex.AttachStdin != nil || ex.AttachCloseStdin != nil || ex.AttachResize != nil {
		t.Fatal("expected run.before failure to clear retained attach handlers")
	}
}

func TestWriteExecutionStdinTimesOutWhenRunBeforeNeverCompletes(t *testing.T) {
	preRunStarted := make(chan struct{}, 1)
	adapter := &stubAdapter{
		runStreamFn: func(ctx context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			switch strings.Join(req.Command, " ") {
			case "sh -lc echo pre-run":
				select {
				case preRunStarted <- struct{}{}:
				default:
				}
				<-ctx.Done()
				return nil, ctx.Err()
			default:
				t.Fatalf("unexpected command: %v", req.Command)
				return nil, nil
			}
		},
	}
	svc := newTestService(adapter)
	timeouts := defaultServiceTimeouts
	timeouts.attachStdinRegistrationWait = 100 * time.Millisecond
	svc.runtime.timeouts = &timeouts

	policyProto := testPolicy()
	policyProto.Run = &cleanroomv1.PolicyRun{
		Before: []string{"sh", "-lc", "echo pre-run"},
	}

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: policyProto})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"sh"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()

	select {
	case <-preRunStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for run.before to start")
	}

	if err := svc.WriteExecutionStdin(sandboxID, executionID, []byte("hello\n")); !errors.Is(err, ErrExecutionStdinUnsupported) {
		t.Fatalf("expected ErrExecutionStdinUnsupported, got %v", err)
	}

	if _, err := svc.CancelExecution(context.Background(), &cleanroomv1.CancelExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Signal:      2,
	}); err != nil {
		t.Fatalf("CancelExecution returned error: %v", err)
	}
	if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}
}

func TestCancelExecutionDuringRunBeforeTransitionsToCanceled(t *testing.T) {
	started := make(chan struct{}, 1)
	adapter := &stubAdapter{
		runStreamFn: func(ctx context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if strings.Join(req.Command, " ") == "sh -lc echo pre-run" {
				select {
				case started <- struct{}{}:
				default:
				}
				<-ctx.Done()
				return &backend.ExecutionResult{
					ExecutionID: req.ExecutionID,
					ExitCode:    1,
					Message:     "terminated",
				}, nil
			}
			t.Fatalf("expected user command not to run after run.before cancel, got %v", req.Command)
			return nil, nil
		},
	}
	svc := newTestService(adapter)

	policyProto := testPolicy()
	policyProto.Run = &cleanroomv1.PolicyRun{
		Before: []string{"sh", "-lc", "echo pre-run"},
	}

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: policyProto})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"sleep", "10"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()

	history, updates, done, unsubscribe, err := svc.SubscribeExecutionEvents(sandboxID, executionID)
	if err != nil {
		t.Fatalf("SubscribeExecutionEvents returned error: %v", err)
	}
	defer unsubscribe()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for run.before to start")
	}

	cancelResp, err := svc.CancelExecution(context.Background(), &cleanroomv1.CancelExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Signal:      15,
	})
	if err != nil {
		t.Fatalf("CancelExecution returned error: %v", err)
	}
	if !cancelResp.GetAccepted() {
		t.Fatal("expected cancel request to be accepted")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for canceled execution to finish")
	}

	getResp, err := svc.GetExecution(context.Background(), &cleanroomv1.GetExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
	})
	if err != nil {
		t.Fatalf("GetExecution returned error: %v", err)
	}
	if got, want := getResp.GetExecution().GetStatus(), cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED; got != want {
		t.Fatalf("unexpected execution status: got %v want %v", got, want)
	}

	events := collectExecutionEvents(t, history, updates, done)
	var exit *cleanroomv1.ExecutionExit
	for _, event := range events {
		if payload, ok := event.Payload.(*cleanroomv1.ExecutionStreamEvent_Exit); ok {
			exit = payload.Exit
		}
	}
	if exit == nil {
		t.Fatalf("expected exit event after cancel, events=%d", len(events))
	}
	if got, want := exit.GetStatus(), cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED; got != want {
		t.Fatalf("unexpected exit status: got %v want %v", got, want)
	}
	if got, want := exit.GetExitCode(), int32(143); got != want {
		t.Fatalf("unexpected exit code: got %d want %d", got, want)
	}
}

func TestCreateExecutionPreservesFirstMatchingRepositoryAfterChangesetSandboxCreate(t *testing.T) {
	adapter := &stubAdapter{}
	svc := newTestService(adapter)

	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"README.md": "hello\n",
	})
	svc.RepositoryStore = mirrors
	repository := repositorycheckout.FromProto(repositoryCheckout)
	if err := os.WriteFile(filepath.Join(mirrors.mirrorPath, "README.md"), []byte("hello from changeset\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(README.md) returned error: %v", err)
	}
	changeset, err := repositorychangeset.BuildFromWorkingTree(mirrors.mirrorPath, repository)
	if err != nil {
		t.Fatalf("BuildFromWorkingTree returned error: %v", err)
	}
	if changeset == nil {
		t.Fatal("expected repository changeset")
	}

	var (
		mu       sync.Mutex
		commands [][]string
	)
	runCalled := make(chan struct{}, 8)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		select {
		case runCalled <- struct{}{}:
		default:
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:              testRepositoryPolicy(),
		RepositoryCheckout:  repositoryCheckout,
		RepositoryChangeset: changeset.ToProto(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()
	for i := 0; i < 2; i++ {
		select {
		case <-runCalled:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for changeset sandbox bootstrap")
		}
	}

	_, err = svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId:          sandboxID,
		Command:            []string{"sh", "-lc", "pwd"},
		RepositoryCheckout: repositoryCheckout,
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	select {
	case <-runCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first execution")
	}

	mu.Lock()
	defer mu.Unlock()
	if got, want := len(commands), 3; got != want {
		t.Fatalf("expected create bootstrap + changeset apply + execution, got %d command(s)", got)
	}
	joined := strings.Join(commands[2], " ")
	if strings.Contains(joined, "git clone --filter=blob:none --no-checkout") {
		t.Fatalf("expected first matching repository execution to preserve created changeset state, got %q", joined)
	}
	if !repositoryWrappedCommandContains(joined, `exec 'sh' '-lc' 'pwd'`) {
		t.Fatalf("expected wrapped user command in repository workdir, got %q", joined)
	}
}

func TestCreateExecutionRebootstrapsSecondMatchingRepositoryAfterChangesetSandboxCreate(t *testing.T) {
	adapter := &stubAdapter{}
	svc := newTestService(adapter)

	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"README.md": "hello\n",
	})
	svc.RepositoryStore = mirrors
	repository := repositorycheckout.FromProto(repositoryCheckout)
	if err := os.WriteFile(filepath.Join(mirrors.mirrorPath, "README.md"), []byte("hello from changeset\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(README.md) returned error: %v", err)
	}
	changeset, err := repositorychangeset.BuildFromWorkingTree(mirrors.mirrorPath, repository)
	if err != nil {
		t.Fatalf("BuildFromWorkingTree returned error: %v", err)
	}
	if changeset == nil {
		t.Fatal("expected repository changeset")
	}

	var (
		mu       sync.Mutex
		commands [][]string
	)
	runCalled := make(chan struct{}, 8)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		select {
		case runCalled <- struct{}{}:
		default:
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:              testRepositoryPolicy(),
		RepositoryCheckout:  repositoryCheckout,
		RepositoryChangeset: changeset.ToProto(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()
	for i := 0; i < 2; i++ {
		select {
		case <-runCalled:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for changeset sandbox bootstrap")
		}
	}

	for i := 0; i < 2; i++ {
		_, err = svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
			SandboxId:          sandboxID,
			Command:            []string{"sh", "-lc", "pwd"},
			RepositoryCheckout: repositoryCheckout,
		})
		if err != nil {
			t.Fatalf("CreateExecution #%d returned error: %v", i+1, err)
		}
		select {
		case <-runCalled:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for execution #%d", i+1)
		}
		if i == 1 {
			select {
			case <-runCalled:
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for second execution rebootstrap")
			}
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if got, want := len(commands), 5; got != want {
		t.Fatalf("expected create bootstrap + changeset apply + first execution + second rebootstrap + second execution, got %d command(s)", got)
	}
	firstExecution := strings.Join(commands[2], " ")
	if strings.Contains(firstExecution, "git clone --filter=blob:none --no-checkout") {
		t.Fatalf("expected first matching repository execution to preserve created changeset state, got %q", firstExecution)
	}
	rebootstrap := strings.Join(commands[3], " ")
	if strings.Contains(rebootstrap, "git clone --filter=blob:none --no-checkout") {
		t.Fatalf("expected second matching repository execution to refresh the existing checkout in place, got %q", rebootstrap)
	}
	if !strings.Contains(rebootstrap, `git -C "$dest" fetch --filter=blob:none --progress origin "$commit"`) {
		t.Fatalf("expected second matching repository execution to fetch the exact checkout commit, got %q", rebootstrap)
	}
	secondExecution := strings.Join(commands[4], " ")
	if strings.Contains(secondExecution, "git clone --filter=blob:none --no-checkout") {
		t.Fatalf("expected final execution command after second rebootstrap, got %q", secondExecution)
	}
	if !repositoryWrappedCommandContains(secondExecution, `exec 'sh' '-lc' 'pwd'`) {
		t.Fatalf("expected wrapped user command in repository workdir, got %q", secondExecution)
	}
}

func TestCreateExecutionSkipsBootstrapForSnapshotBackedSandboxWithMatchingRepository(t *testing.T) {
	adapter := &stubAdapter{
		createSnapshotFn: func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
			return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
		},
	}
	mirrors := &stubRepositoryMirrorStore{}
	store := newMemorySnapshotStore()
	svc := newTestServiceWithSnapshotStore(adapter, store)
	svc.RepositoryStore = mirrors

	var (
		mu       sync.Mutex
		commands [][]string
	)
	runCalled := make(chan struct{}, 8)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		select {
		case runCalled <- struct{}{}:
		default:
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	sourceResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryPolicy(),
		RepositoryCheckout: testRepositoryCheckoutProto(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	select {
	case <-runCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for source sandbox bootstrap")
	}

	snapshotResp, err := svc.CreateSnapshot(context.Background(), &cleanroomv1.CreateSnapshotRequest{
		SandboxId: sourceResp.GetSandbox().GetSandboxId(),
	})
	if err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}

	forkResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Source: &cleanroomv1.CreateSandboxRequest_SnapshotId{
			SnapshotId: snapshotResp.GetSnapshot().GetSnapshotId(),
		},
	})
	if err != nil {
		t.Fatalf("CreateSandbox from snapshot returned error: %v", err)
	}

	_, err = svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: forkResp.GetSandbox().GetSandboxId(),
		Command:   []string{"sh", "-lc", "pwd"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	select {
	case <-runCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for snapshot-backed execution")
	}

	mu.Lock()
	defer mu.Unlock()
	if got, want := len(commands), 2; got != want {
		t.Fatalf("expected source bootstrap + execution only, got %d command(s)", got)
	}
	joined := strings.Join(commands[1], " ")
	if strings.Contains(joined, "git clone --filter=blob:none --no-checkout") {
		t.Fatalf("expected snapshot-backed sandbox execution to reuse existing checkout, got %q", joined)
	}
	if !repositoryWrappedCommandContains(joined, `exec 'sh' '-lc' 'pwd'`) {
		t.Fatalf("expected snapshot-backed sandbox execution to run inside repository workdir, got %q", joined)
	}
	if got, want := mirrors.calls, 1; got != want {
		t.Fatalf("expected mirror prewarm only during source sandbox create, got %d call(s)", got)
	}
}

func TestCreateExecutionRebootstrapsMatchingRepositoryAfterChangesetSnapshotRestore(t *testing.T) {
	adapter := &stubAdapter{
		createSnapshotFn: func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
			return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
		},
	}
	store := newMemorySnapshotStore()
	svc := newTestServiceWithSnapshotStore(adapter, store)

	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"README.md": "hello\n",
	})
	svc.RepositoryStore = mirrors
	repository := repositorycheckout.FromProto(repositoryCheckout)
	if err := os.WriteFile(filepath.Join(mirrors.mirrorPath, "README.md"), []byte("hello from changeset\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(README.md) returned error: %v", err)
	}
	changeset, err := repositorychangeset.BuildFromWorkingTree(mirrors.mirrorPath, repository)
	if err != nil {
		t.Fatalf("BuildFromWorkingTree returned error: %v", err)
	}
	if changeset == nil {
		t.Fatal("expected repository changeset")
	}

	var (
		mu       sync.Mutex
		commands [][]string
	)
	runCalled := make(chan struct{}, 8)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		select {
		case runCalled <- struct{}{}:
		default:
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	sourceResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:              testRepositoryPolicy(),
		RepositoryCheckout:  repositoryCheckout,
		RepositoryChangeset: changeset.ToProto(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sourceSandboxID := sourceResp.GetSandbox().GetSandboxId()
	for i := 0; i < 2; i++ {
		select {
		case <-runCalled:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for source changeset bootstrap")
		}
	}

	snapshotResp, err := svc.CreateSnapshot(context.Background(), &cleanroomv1.CreateSnapshotRequest{
		SandboxId: sourceSandboxID,
		Name:      "changeset",
	})
	if err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}

	mu.Lock()
	commands = nil
	mu.Unlock()

	restoreResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Source: &cleanroomv1.CreateSandboxRequest_SnapshotId{SnapshotId: snapshotResp.GetSnapshot().GetSnapshotId()},
	})
	if err != nil {
		t.Fatalf("CreateSandbox from snapshot returned error: %v", err)
	}
	restoreSandboxID := restoreResp.GetSandbox().GetSandboxId()

	_, err = svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId:          restoreSandboxID,
		Command:            []string{"sh", "-lc", "pwd"},
		RepositoryCheckout: repositoryCheckout,
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-runCalled:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for snapshot restore rebootstrap execution")
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if got, want := len(commands), 2; got != want {
		t.Fatalf("expected restore execution to run rebootstrap + user command, got %d command(s)", got)
	}
	rebootstrap := strings.Join(commands[0], " ")
	if strings.Contains(rebootstrap, "git clone --filter=blob:none --no-checkout") {
		t.Fatalf("expected snapshot-backed execution to refresh the existing checkout in place after changeset restore, got %q", rebootstrap)
	}
	if !strings.Contains(rebootstrap, `git -C "$dest" fetch --filter=blob:none --progress origin "$commit"`) {
		t.Fatalf("expected snapshot-backed execution to fetch the exact checkout commit after changeset restore, got %q", rebootstrap)
	}
	joined := strings.Join(commands[1], " ")
	if strings.Contains(joined, "git clone --filter=blob:none --no-checkout") {
		t.Fatalf("expected final execution command after snapshot rebootstrap, got %q", joined)
	}
	if !repositoryWrappedCommandContains(joined, `exec 'sh' '-lc' 'pwd'`) {
		t.Fatalf("expected wrapped user command in repository workdir, got %q", joined)
	}
}

func TestCreateSandboxRejectsRepositoryRemoteOutsidePolicy(t *testing.T) {
	adapter := &stubAdapter{}
	svc := newTestService(adapter)

	repository := testRepositoryCheckoutProto()
	repository.RemoteUrl = "https://example.com/buildkite/cleanroom.git"
	_, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryPolicy(),
		RepositoryCheckout: repository,
	})
	if err == nil {
		t.Fatal("expected repository bootstrap to reject disallowed host")
	}
	if !strings.Contains(err.Error(), `repository remote host "example.com" is not allowed`) {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := adapter.provisionCalls; got != 0 {
		t.Fatalf("expected no provision call on invalid repository remote, got %d", got)
	}
}

func repositoryWrappedCommandContains(joined, execSnippet string) bool {
	return strings.Contains(joined, "dest='/workspace'") &&
		strings.Contains(joined, `cd "$dest"`) &&
		strings.Contains(joined, execSnippet)
}

func TestCreateSandboxRejectsRepositoryFileRemote(t *testing.T) {
	adapter := &stubAdapter{}
	svc := newTestService(adapter)

	repository := testRepositoryCheckoutProto()
	repository.RemoteUrl = "file:///tmp/evil.git"
	_, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryPolicy(),
		RepositoryCheckout: repository,
	})
	if err == nil {
		t.Fatal("expected repository bootstrap to reject non-https remote")
	}
	if !strings.Contains(err.Error(), "must use https") {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := adapter.provisionCalls; got != 0 {
		t.Fatalf("expected no provision call on invalid repository remote, got %d", got)
	}
}

func TestCreateSandboxRejectsMutableRepositoryCommitRef(t *testing.T) {
	adapter := &stubAdapter{}
	svc := newTestService(adapter)

	repository := testRepositoryCheckoutProto()
	repository.CommitSha = "main"
	_, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryPolicy(),
		RepositoryCheckout: repository,
	})
	if err == nil {
		t.Fatal("expected repository bootstrap to reject mutable commit refs")
	}
	if !strings.Contains(err.Error(), "full 40-character hexadecimal commit SHA") {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := adapter.provisionCalls; got != 0 {
		t.Fatalf("expected no provision call on invalid repository commit ref, got %d", got)
	}
}

func TestCreateExecutionRejectsRepositoryRemoteOutsidePolicy(t *testing.T) {
	adapter := &stubAdapter{}
	mirrors := &stubRepositoryMirrorStore{}
	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testRepositoryPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	repository := testRepositoryCheckoutProto()
	repository.RemoteUrl = "https://example.com/buildkite/cleanroom.git"
	_, err = svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId:          sandboxID,
		Command:            []string{"sh", "-lc", "pwd"},
		RepositoryCheckout: repository,
	})
	if err == nil {
		t.Fatal("expected repository bootstrap to reject disallowed host")
	}
	if !strings.Contains(err.Error(), `repository remote host "example.com" is not allowed`) {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := mirrors.calls; got != 0 {
		t.Fatalf("expected no mirror prewarm call on invalid repository remote, got %d", got)
	}
}

func TestCreateSandboxBootstrapCleanupUsesFreshContext(t *testing.T) {
	adapter := &stubAdapter{}
	svc := newTestService(adapter)
	adapter.runStreamFn = func(ctx context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		return nil, ctx.Err()
	}
	adapter.terminateFn = func(ctx context.Context, sandboxID string) error {
		if sandboxID == "" {
			t.Fatal("expected sandbox id during cleanup")
		}
		if err := ctx.Err(); err != nil {
			t.Fatalf("expected cleanup context to remain usable, got %v", err)
		}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := svc.CreateSandbox(ctx, &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryPolicy(),
		RepositoryCheckout: testRepositoryCheckoutProto(),
	})
	if err == nil {
		t.Fatal("expected repository bootstrap to fail")
	}
	if got := adapter.terminateCalls; got != 1 {
		t.Fatalf("expected a single cleanup terminate call, got %d", got)
	}
}

func TestCreateSandboxMergesDarwinVZConfig(t *testing.T) {
	adapter := &stubAdapter{}
	svc := &Service{
		Loader: stubLoader{
			compiled: &policy.CompiledPolicy{
				Version:        1,
				NetworkDefault: "deny",
				ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			},
			source: "/repo/cleanroom.yaml",
		},
		Config: runtimeconfig.Config{
			DefaultBackend: "darwin-vz",
			Backends: runtimeconfig.Backends{
				Firecracker: runtimeconfig.FirecrackerConfig{
					KernelImage:   "/firecracker-kernel",
					RootFS:        "/firecracker-rootfs",
					Services:      runtimeconfig.ServicesConfig{Docker: runtimeconfig.DockerServiceConfig{StartupTimeoutSeconds: 11, StorageDriver: "btrfs", IPTables: true}},
					VCPUs:         1,
					MemoryMiB:     256,
					GuestPort:     10700,
					LaunchSeconds: 10,
				},
				DarwinVZ: runtimeconfig.DarwinVZConfig{
					KernelImage:   "/darwin-vz-kernel",
					RootFS:        "/darwin-vz-rootfs",
					Services:      runtimeconfig.ServicesConfig{Docker: runtimeconfig.DockerServiceConfig{StartupTimeoutSeconds: 55, StorageDriver: "overlay2", IPTables: false}},
					VCPUs:         4,
					MemoryMiB:     2048,
					GuestPort:     12000,
					LaunchSeconds: 45,
				},
			},
		},
		Backends: map[string]backend.Adapter{"darwin-vz": adapter},
	}

	_, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy: testPolicy(),
		Options: &cleanroomv1.SandboxOptions{
			LaunchSeconds: 33,
		},
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if got, want := adapter.provisionCalls, 1; got != want {
		t.Fatalf("unexpected provision call count: got %d want %d", got, want)
	}

	gotCfg := adapter.provisionReq.FirecrackerConfig
	if got, want := gotCfg.KernelImagePath, "/darwin-vz-kernel"; got != want {
		t.Fatalf("unexpected kernel image: got %q want %q", got, want)
	}
	if got, want := gotCfg.RootFSPath, "/darwin-vz-rootfs"; got != want {
		t.Fatalf("unexpected rootfs: got %q want %q", got, want)
	}
	if got, want := gotCfg.VCPUs, int64(4); got != want {
		t.Fatalf("unexpected vcpus: got %d want %d", got, want)
	}
	if got, want := gotCfg.MemoryMiB, int64(2048); got != want {
		t.Fatalf("unexpected memory_mib: got %d want %d", got, want)
	}
	if got, want := gotCfg.GuestPort, uint32(12000); got != want {
		t.Fatalf("unexpected guest_port: got %d want %d", got, want)
	}
	if got, want := gotCfg.LaunchSeconds, int64(33); got != want {
		t.Fatalf("unexpected launch_seconds: got %d want %d", got, want)
	}
	if got, want := gotCfg.DockerStartupSeconds, int64(55); got != want {
		t.Fatalf("unexpected docker startup timeout: got %d want %d", got, want)
	}
	if got, want := gotCfg.DockerStorageDriver, "overlay2"; got != want {
		t.Fatalf("unexpected docker storage driver: got %q want %q", got, want)
	}
	if got := gotCfg.DockerIPTables; got {
		t.Fatal("expected docker iptables=false from darwin-vz config")
	}
	if got := gotCfg.Launch; !got {
		t.Fatalf("expected launch=true")
	}
}

func TestCreateSandboxMergesFirecrackerSnapshotConfig(t *testing.T) {
	adapter := &stubAdapter{}
	svc := &Service{
		Loader: stubLoader{
			compiled: &policy.CompiledPolicy{
				Version:        1,
				NetworkDefault: "deny",
				ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			},
			source: "/repo/cleanroom.yaml",
		},
		Config: runtimeconfig.Config{
			DefaultBackend: "firecracker",
			Backends: runtimeconfig.Backends{
				Firecracker: runtimeconfig.FirecrackerConfig{
					KernelImage: "/firecracker-kernel",
					RootFS:      "/firecracker-rootfs",
					Snapshots: runtimeconfig.SnapshotConfig{
						Enabled:               true,
						Driver:                "file",
						BaseDir:               "/var/tmp/cleanroom-snapshots",
						QuiesceTimeoutSeconds: 15,
					},
				},
			},
		},
		Backends: map[string]backend.Adapter{"firecracker": adapter},
	}

	_, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if got, want := adapter.provisionCalls, 1; got != want {
		t.Fatalf("unexpected provision call count: got %d want %d", got, want)
	}

	gotCfg := adapter.provisionReq.FirecrackerConfig
	if !gotCfg.Snapshots.Enabled {
		t.Fatal("expected snapshots.enabled=true")
	}
	if got, want := gotCfg.Snapshots.Driver, "file"; got != want {
		t.Fatalf("unexpected snapshot driver: got %q want %q", got, want)
	}
	if got, want := gotCfg.Snapshots.BaseDir, "/var/tmp/cleanroom-snapshots"; got != want {
		t.Fatalf("unexpected snapshot base_dir: got %q want %q", got, want)
	}
	if got, want := gotCfg.Snapshots.QuiesceTimeoutSeconds, int64(15); got != want {
		t.Fatalf("unexpected snapshot quiesce timeout: got %d want %d", got, want)
	}
}

func TestCreateSandboxSetsRepositoryBootstrapRootFSMinimum(t *testing.T) {
	adapter := &stubAdapter{}
	svc := &Service{
		Config: runtimeconfig.Config{
			DefaultBackend: "darwin-vz",
		},
		Backends: map[string]backend.Adapter{"darwin-vz": adapter},
	}

	policyProto := testRepositoryPolicy()
	policyProto.Services = &cleanroomv1.PolicyServices{
		Docker: &cleanroomv1.PolicyDockerService{Required: true},
	}

	_, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Backend:            "darwin-vz",
		Policy:             policyProto,
		RepositoryCheckout: testRepositoryCheckoutProto(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	if got, want := adapter.provisionReq.FirecrackerConfig.MinimumRootFSBytes, repositoryBootstrapDockerMinimumRootFSBytes; got != want {
		t.Fatalf("unexpected minimum_rootfs_bytes: got %d want %d", got, want)
	}
}

func TestCreateExecutionRejectsWhenSandboxBusy(t *testing.T) {
	started := make(chan struct{}, 1)
	adapter := &stubAdapter{
		runFn: func(ctx context.Context, req backend.ExecutionRequest) (*backend.ExecutionResult, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	svc := newTestService(adapter)

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	firstExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"sleep", "30"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	firstExecutionID := firstExecutionResp.GetExecution().GetExecutionId()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first execution to start")
	}

	_, err = svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"echo", "second"},
	})
	if err == nil {
		t.Fatal("expected sandbox_busy error")
	}
	if !strings.Contains(err.Error(), "sandbox_busy") {
		t.Fatalf("expected sandbox_busy error, got: %v", err)
	}

	if _, err := svc.CancelExecution(context.Background(), &cleanroomv1.CancelExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: firstExecutionID,
		Signal:      15,
	}); err != nil {
		t.Fatalf("CancelExecution returned error: %v", err)
	}
	if _, err := svc.WaitExecution(context.Background(), sandboxID, firstExecutionID); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}
}

func TestDownloadSandboxFileReturnsData(t *testing.T) {
	expectedSandboxID := ""
	adapter := &stubAdapter{
		downloadFn: func(_ context.Context, sandboxID, path string, maxBytes int64) ([]byte, error) {
			if got, want := sandboxID, expectedSandboxID; got != want {
				t.Fatalf("unexpected sandbox id: got %q want %q", got, want)
			}
			if got, want := path, "/home/sprite/artifacts/result.txt"; got != want {
				t.Fatalf("unexpected path: got %q want %q", got, want)
			}
			if got, want := maxBytes, int64(1024); got != want {
				t.Fatalf("unexpected max_bytes: got %d want %d", got, want)
			}
			return []byte("artifact-data"), nil
		},
	}
	svc := newTestService(adapter)

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	expectedSandboxID = createResp.GetSandbox().GetSandboxId()

	resp, err := svc.DownloadSandboxFile(context.Background(), &cleanroomv1.DownloadSandboxFileRequest{
		SandboxId: expectedSandboxID,
		Path:      "/home/sprite/artifacts/result.txt",
		MaxBytes:  1024,
	})
	if err != nil {
		t.Fatalf("DownloadSandboxFile returned error: %v", err)
	}
	if got, want := string(resp.GetData()), "artifact-data"; got != want {
		t.Fatalf("unexpected data: got %q want %q", got, want)
	}
	if got, want := resp.GetSizeBytes(), int64(len("artifact-data")); got != want {
		t.Fatalf("unexpected size_bytes: got %d want %d", got, want)
	}
}

func TestDownloadSandboxFileRejectsWhenSandboxBusy(t *testing.T) {
	started := make(chan struct{}, 1)
	adapter := &stubAdapter{
		runFn: func(ctx context.Context, req backend.ExecutionRequest) (*backend.ExecutionResult, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
		downloadFn: func(_ context.Context, _, _ string, _ int64) ([]byte, error) {
			t.Fatal("download should not be called while sandbox is busy")
			return nil, nil
		},
	}
	svc := newTestService(adapter)

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"sleep", "30"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for execution to start")
	}

	_, err = svc.DownloadSandboxFile(context.Background(), &cleanroomv1.DownloadSandboxFileRequest{
		SandboxId: sandboxID,
		Path:      "/home/sprite/artifacts/result.txt",
	})
	if err == nil {
		t.Fatal("expected sandbox_busy error")
	}
	if !strings.Contains(err.Error(), "sandbox_busy") {
		t.Fatalf("expected sandbox_busy error, got: %v", err)
	}

	if _, err := svc.CancelExecution(context.Background(), &cleanroomv1.CancelExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Signal:      15,
	}); err != nil {
		t.Fatalf("CancelExecution returned error: %v", err)
	}
	if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}
}

func TestCreateExecutionRejectsWhileSandboxFileDownloadInProgress(t *testing.T) {
	downloadStarted := make(chan struct{}, 1)
	allowDownloadFinish := make(chan struct{})
	downloadDone := make(chan error, 1)

	adapter := &stubAdapter{
		downloadFn: func(_ context.Context, _, _ string, _ int64) ([]byte, error) {
			select {
			case downloadStarted <- struct{}{}:
			default:
			}
			<-allowDownloadFinish
			return []byte("artifact-data"), nil
		},
	}
	svc := newTestService(adapter)

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	go func() {
		_, err := svc.DownloadSandboxFile(context.Background(), &cleanroomv1.DownloadSandboxFileRequest{
			SandboxId: sandboxID,
			Path:      "/home/sprite/artifacts/result.txt",
		})
		downloadDone <- err
	}()

	select {
	case <-downloadStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for download to start")
	}

	_, err = svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"echo", "hi"},
	})
	if err == nil {
		t.Fatal("expected sandbox_busy error")
	}
	if !strings.Contains(err.Error(), "sandbox_busy") {
		t.Fatalf("expected sandbox_busy error, got: %v", err)
	}

	close(allowDownloadFinish)
	if err := <-downloadDone; err != nil {
		t.Fatalf("DownloadSandboxFile returned error: %v", err)
	}
}

func TestDownloadSandboxFilePreservesPathWhitespace(t *testing.T) {
	expectedPath := "/home/sprite/artifacts/result.txt "
	adapter := &stubAdapter{
		downloadFn: func(_ context.Context, _, path string, _ int64) ([]byte, error) {
			if got, want := path, expectedPath; got != want {
				t.Fatalf("unexpected path: got %q want %q", got, want)
			}
			return []byte("artifact-data"), nil
		},
	}
	svc := newTestService(adapter)

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	_, err = svc.DownloadSandboxFile(context.Background(), &cleanroomv1.DownloadSandboxFileRequest{
		SandboxId: createResp.GetSandbox().GetSandboxId(),
		Path:      expectedPath,
		MaxBytes:  1024,
	})
	if err != nil {
		t.Fatalf("DownloadSandboxFile returned error: %v", err)
	}
}

func TestTerminateSandboxReturnsBackendTerminateError(t *testing.T) {
	adapter := &stubAdapter{
		terminateFn: func(context.Context, string) error {
			return errors.New("boom")
		},
	}
	svc := newTestService(adapter)

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	_, err = svc.TerminateSandbox(context.Background(), &cleanroomv1.TerminateSandboxRequest{SandboxId: createResp.GetSandbox().GetSandboxId()})
	if err == nil {
		t.Fatal("expected terminate backend error")
	}
	if !strings.Contains(err.Error(), "terminate backend sandbox") {
		t.Fatalf("unexpected terminate error: %v", err)
	}
}

func TestTerminateSandboxAllowsRetryAfterBackendFailure(t *testing.T) {
	terminateAttempts := 0
	adapter := &stubAdapter{
		terminateFn: func(context.Context, string) error {
			terminateAttempts++
			if terminateAttempts == 1 {
				return errors.New("transient terminate failure")
			}
			return nil
		},
	}
	svc := newTestService(adapter)

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()

	if _, err := svc.TerminateSandbox(context.Background(), &cleanroomv1.TerminateSandboxRequest{SandboxId: sandboxID}); err == nil {
		t.Fatal("expected terminate backend error on first attempt")
	}

	getResp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := getResp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPING; got != want {
		t.Fatalf("unexpected sandbox status after failed terminate: got %v want %v", got, want)
	}

	if _, err := svc.TerminateSandbox(context.Background(), &cleanroomv1.TerminateSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("second TerminateSandbox returned error: %v", err)
	}

	getResp, err = svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := getResp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED; got != want {
		t.Fatalf("unexpected sandbox status after successful retry: got %v want %v", got, want)
	}
	if got, want := terminateAttempts, 2; got != want {
		t.Fatalf("unexpected terminate attempt count: got %d want %d", got, want)
	}
}

func TestTerminateSandboxPropagatesRequestContextToBackend(t *testing.T) {
	var terminateCtxErr error
	adapter := &stubAdapter{
		terminateFn: func(ctx context.Context, _ string) error {
			terminateCtxErr = ctx.Err()
			return nil
		},
	}
	svc := newTestService(adapter)

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	terminateCtx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := svc.TerminateSandbox(terminateCtx, &cleanroomv1.TerminateSandboxRequest{SandboxId: createResp.GetSandbox().GetSandboxId()}); err != nil {
		t.Fatalf("TerminateSandbox returned error: %v", err)
	}

	if !errors.Is(terminateCtxErr, context.Canceled) {
		t.Fatalf("expected backend terminate context to be canceled, got %v", terminateCtxErr)
	}
}

func TestServiceGeneratedIDsUseTypeIDFormat(t *testing.T) {
	adapter := &stubAdapter{}
	svc := newTestService(adapter)

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()
	parsedSandboxID, err := typeid.FromString(sandboxID)
	if err != nil {
		t.Fatalf("expected typeid-formatted sandbox id, got %q: %v", sandboxID, err)
	}
	if got, want := parsedSandboxID.Prefix(), "cr"; got != want {
		t.Fatalf("unexpected sandbox id prefix: got %q want %q", got, want)
	}

	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"echo", "ok"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}

	executionID := createExecutionResp.GetExecution().GetExecutionId()
	parsedExecutionID, err := typeid.FromString(executionID)
	if err != nil {
		t.Fatalf("expected typeid-formatted execution id, got %q: %v", executionID, err)
	}
	if got, want := parsedExecutionID.Prefix(), "exec"; got != want {
		t.Fatalf("unexpected execution id prefix: got %q want %q", got, want)
	}

	if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}

	getResp, err := svc.GetExecution(context.Background(), &cleanroomv1.GetExecutionRequest{SandboxId: sandboxID, ExecutionId: executionID})
	if err != nil {
		t.Fatalf("GetExecution returned error: %v", err)
	}
	if got, want := getResp.GetExecution().GetExecutionId(), executionID; got != want {
		t.Fatalf("unexpected execution id from GetExecution: got %q want %q", got, want)
	}

	getResp, err = svc.GetExecution(context.Background(), &cleanroomv1.GetExecutionRequest{ExecutionId: executionID})
	if err != nil {
		t.Fatalf("GetExecution without sandbox_id returned error: %v", err)
	}
	if got, want := getResp.GetExecution().GetSandboxId(), sandboxID; got != want {
		t.Fatalf("unexpected sandbox id from global GetExecution lookup: got %q want %q", got, want)
	}

	sandboxResp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := sandboxResp.GetSandbox().GetLastExecutionId(), executionID; got != want {
		t.Fatalf("unexpected last execution id: got %q want %q", got, want)
	}
	if got := sandboxResp.GetSandbox().GetActiveExecutionId(); got != "" {
		t.Fatalf("expected no active execution after completion, got %q", got)
	}
}

func TestExecutionAttachIOForwarding(t *testing.T) {
	started := make(chan struct{}, 1)
	stdinChunks := make(chan string, 1)
	stdinClosed := make(chan struct{}, 1)
	resizes := make(chan [2]uint32, 1)
	adapter := &stubAdapter{
		runStreamFn: func(ctx context.Context, _ backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if stream.OnAttach != nil {
				stream.OnAttach(backend.AttachIO{
					WriteStdin: func(data []byte) error {
						stdinChunks <- string(data)
						return nil
					},
					CloseStdin: func() error {
						select {
						case stdinClosed <- struct{}{}:
						default:
						}
						return nil
					},
					ResizeTTY: func(cols, rows uint32) error {
						resizes <- [2]uint32{cols, rows}
						return nil
					},
				})
			}
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	svc := newTestService(adapter)

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"sh"},
		Options: &cleanroomv1.ExecutionOptions{
			Tty: true,
		},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for execution to start")
	}

	if err := svc.WriteExecutionStdin(sandboxID, executionID, []byte("hello\n")); err != nil {
		t.Fatalf("WriteExecutionStdin returned error: %v", err)
	}
	if err := svc.CloseExecutionStdin(sandboxID, executionID); err != nil {
		t.Fatalf("CloseExecutionStdin returned error: %v", err)
	}
	if err := svc.ResizeExecutionTTY(sandboxID, executionID, 120, 40); err != nil {
		t.Fatalf("ResizeExecutionTTY returned error: %v", err)
	}

	select {
	case got := <-stdinChunks:
		if got != "hello\n" {
			t.Fatalf("unexpected stdin payload: got %q want %q", got, "hello\n")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stdin callback")
	}

	select {
	case <-stdinClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stdin close callback")
	}

	select {
	case got := <-resizes:
		if got != [2]uint32{120, 40} {
			t.Fatalf("unexpected resize payload: got %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for resize callback")
	}

	if _, err := svc.CancelExecution(context.Background(), &cleanroomv1.CancelExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Signal:      2,
	}); err != nil {
		t.Fatalf("CancelExecution returned error: %v", err)
	}
	if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}
}

func TestExecutionAttachIOWaitsForDelayedAttachRegistration(t *testing.T) {
	started := make(chan struct{}, 1)
	stdinChunks := make(chan string, 1)
	stdinClosed := make(chan struct{}, 1)
	resizes := make(chan [2]uint32, 1)
	adapter := &stubAdapter{
		runStreamFn: func(ctx context.Context, _ backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			time.Sleep(200 * time.Millisecond)
			if stream.OnAttach != nil {
				stream.OnAttach(backend.AttachIO{
					WriteStdin: func(data []byte) error {
						stdinChunks <- string(data)
						return nil
					},
					CloseStdin: func() error {
						select {
						case stdinClosed <- struct{}{}:
						default:
						}
						return nil
					},
					ResizeTTY: func(cols, rows uint32) error {
						resizes <- [2]uint32{cols, rows}
						return nil
					},
				})
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	svc := newTestService(adapter)
	timeouts := defaultServiceTimeouts
	timeouts.attachStdinRegistrationWait = 100 * time.Millisecond
	svc.runtime.timeouts = &timeouts
	svc.Config.Backends.Firecracker.LaunchSeconds = 1

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"sh"},
		Options: &cleanroomv1.ExecutionOptions{
			Tty: true,
		},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for execution to start")
	}

	if err := svc.WriteExecutionStdin(sandboxID, executionID, []byte("hello\n")); err != nil {
		t.Fatalf("WriteExecutionStdin returned error: %v", err)
	}
	if err := svc.CloseExecutionStdin(sandboxID, executionID); err != nil {
		t.Fatalf("CloseExecutionStdin returned error: %v", err)
	}
	if err := svc.ResizeExecutionTTY(sandboxID, executionID, 120, 40); err != nil {
		t.Fatalf("ResizeExecutionTTY returned error: %v", err)
	}

	select {
	case got := <-stdinChunks:
		if got != "hello\n" {
			t.Fatalf("unexpected stdin payload: got %q want %q", got, "hello\n")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delayed stdin callback")
	}

	select {
	case <-stdinClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delayed stdin close callback")
	}

	select {
	case got := <-resizes:
		if got != [2]uint32{120, 40} {
			t.Fatalf("unexpected resize payload: got %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delayed resize callback")
	}

	if _, err := svc.CancelExecution(context.Background(), &cleanroomv1.CancelExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Signal:      2,
	}); err != nil {
		t.Fatalf("CancelExecution returned error: %v", err)
	}
	if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}
}

func TestExecutionAttachIOUnsupportedWhenBackendDoesNotExposeHandlers(t *testing.T) {
	started := make(chan struct{}, 1)
	adapter := &stubAdapter{
		runStreamFn: func(ctx context.Context, _ backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	svc := newTestService(adapter)

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"sh"},
		Options: &cleanroomv1.ExecutionOptions{
			Tty: true,
		},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for execution to start")
	}

	if err := svc.WriteExecutionStdin(sandboxID, executionID, []byte("hello\n")); !errors.Is(err, ErrExecutionStdinUnsupported) {
		t.Fatalf("expected ErrExecutionStdinUnsupported, got %v", err)
	}
	if err := svc.CloseExecutionStdin(sandboxID, executionID); !errors.Is(err, ErrExecutionStdinUnsupported) {
		t.Fatalf("expected ErrExecutionStdinUnsupported from CloseExecutionStdin, got %v", err)
	}
	if err := svc.ResizeExecutionTTY(sandboxID, executionID, 80, 24); !errors.Is(err, ErrExecutionResizeUnsupported) {
		t.Fatalf("expected ErrExecutionResizeUnsupported, got %v", err)
	}

	if _, err := svc.CancelExecution(context.Background(), &cleanroomv1.CancelExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Signal:      2,
	}); err != nil {
		t.Fatalf("CancelExecution returned error: %v", err)
	}
	if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}
}

func TestTerminateRetainsStoppedSandboxState(t *testing.T) {
	svc := newTestService(&stubAdapter{})

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()

	terminateResp, err := svc.TerminateSandbox(context.Background(), &cleanroomv1.TerminateSandboxRequest{
		SandboxId: sandboxID,
	})
	if err != nil {
		t.Fatalf("TerminateSandbox returned error: %v", err)
	}
	if !terminateResp.GetTerminated() {
		t.Fatal("expected terminated=true")
	}

	getResp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{
		SandboxId: sandboxID,
	})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := getResp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED; got != want {
		t.Fatalf("unexpected sandbox status: got %v want %v", got, want)
	}
}

func TestRunExecutionSkipsAlreadyFinalExecution(t *testing.T) {
	adapter := &stubAdapter{}
	svc := newTestService(adapter)

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()

	finished := time.Now().UTC()
	executionID := "exec-final"
	key := executionKey(sandboxID, executionID)

	svc.mu.Lock()
	sb := svc.sandboxes[sandboxID]
	sb.Status = cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED

	ex := &executionState{
		ID:         executionID,
		SandboxID:  sandboxID,
		Command:    []string{"echo", "stale"},
		Status:     cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED,
		ExitCode:   143,
		FinishedAt: &finished,
		events:     newEventFeed[*cleanroomv1.ExecutionStreamEvent](defaultRetentionPolicy.maxRetainedExecutionEvents),
		Done:       make(chan struct{}),
	}
	svc.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Status:      ex.Status,
		Payload: &cleanroomv1.ExecutionStreamEvent_Exit{Exit: &cleanroomv1.ExecutionExit{
			ExitCode: ex.ExitCode,
			Status:   ex.Status,
			Message:  "already canceled",
		}},
	})
	closeExecutionDoneLocked(ex)
	svc.executions[key] = ex
	initialEvents := len(ex.events.snapshot())
	svc.mu.Unlock()

	svc.runExecution(sandboxID, executionID)

	svc.mu.RLock()
	gotEx := svc.executions[key]
	svc.mu.RUnlock()

	if gotEx == nil {
		t.Fatal("expected execution state to exist")
	}
	if got, want := len(gotEx.events.snapshot()), initialEvents; got != want {
		t.Fatalf("expected no additional events, got %d want %d", got, want)
	}
	if got, want := gotEx.Status, cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED; got != want {
		t.Fatalf("unexpected status: got %v want %v", got, want)
	}
	if got, want := adapter.runCalls, 0; got != want {
		t.Fatalf("adapter should not run for finalized execution: got %d want %d", got, want)
	}
}

func TestFinalizeExecutionWithoutPruneSkipsImmediateStatePruning(t *testing.T) {
	svc := newTestService(&stubAdapter{})
	retention := testRetentionPolicy()
	retention.maxRetainedFinishedExecutions = 0
	svc.runtime.retention = retention
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()
	executionID := "exec-no-prune"
	key := executionKey(sandboxID, executionID)

	now := time.Now().UTC()
	ex := &executionState{
		ID:        executionID,
		SandboxID: sandboxID,
		Command:   []string{"echo", "ok"},
		Status:    cleanroomv1.ExecutionStatus_EXECUTION_STATUS_QUEUED,
		events:    newEventFeed[*cleanroomv1.ExecutionStreamEvent](retention.maxRetainedExecutionEvents),
		Done:      make(chan struct{}),
	}

	svc.mu.Lock()
	svc.executions[key] = ex
	svc.finalizeExecutionWithoutPruneLocked(
		ex,
		cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED,
		130,
		"canceled",
		"",
		now,
	)
	if _, ok := svc.executions[key]; !ok {
		svc.mu.Unlock()
		t.Fatal("execution should remain when finalize skips pruning")
	}
	svc.pruneStateLocked(now)
	if _, ok := svc.executions[key]; ok {
		svc.mu.Unlock()
		t.Fatal("execution should be pruned once explicit prune runs")
	}
	svc.mu.Unlock()
}

func TestPruneFinishedExecutionClearsSandboxExecutionPointers(t *testing.T) {
	svc := newTestService(&stubAdapter{})

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"echo", "ok"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()
	if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}

	retention := testRetentionPolicy()
	retention.maxRetainedFinishedExecutions = 0
	svc.runtime.retention = retention

	svc.mu.Lock()
	svc.pruneStateLocked(time.Now().UTC())
	svc.mu.Unlock()

	getSandboxResp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got := getSandboxResp.GetSandbox().GetLastExecutionId(); got != "" {
		t.Fatalf("expected last_execution_id to clear after pruning, got %q", got)
	}
	if got := getSandboxResp.GetSandbox().GetActiveExecutionId(); got != "" {
		t.Fatalf("expected active_execution_id to remain empty after pruning, got %q", got)
	}
	if _, err := svc.ExecutionSnapshot(sandboxID, executionID); err == nil {
		t.Fatal("expected pruned execution snapshot lookup to fail")
	}
}

func TestBufferedResultDeltaModes(t *testing.T) {
	if got, replace := bufferedResultDelta("abc", "abcabc", 3); got != "" || replace {
		t.Fatalf("expected saturated suffix overlap to suppress duplicate delta, got delta=%q replace=%t", got, replace)
	}
	if got, replace := bufferedResultDelta("prefix-", "prefix-tail", 1024); got != "tail" || replace {
		t.Fatalf("expected prefix-only append delta, got delta=%q replace=%t", got, replace)
	}
	if got, replace := bufferedResultDelta("tail", "head-tail", 1024); got != "head-tail" || !replace {
		t.Fatalf("expected suffix-only backfill replacement, got delta=%q replace=%t", got, replace)
	}
}

func TestMergeBufferedResultOutputReplacesMissingStreamPrefix(t *testing.T) {
	svc := newTestService(&stubAdapter{})
	ex := &executionState{
		ID:        "exec-1",
		SandboxID: "sb-1",
		Stdout:    "tail",
		Status:    cleanroomv1.ExecutionStatus_EXECUTION_STATUS_RUNNING,
		events:    newEventFeed[*cleanroomv1.ExecutionStreamEvent](defaultRetentionPolicy.maxRetainedExecutionEvents),
	}

	svc.mergeBufferedResultOutputLocked(ex, &backend.ExecutionResult{
		Stdout: "head-tail",
	}, true)

	if got, want := ex.Stdout, "head-tail"; got != want {
		t.Fatalf("expected buffered replacement to preserve missing prefix: got %q want %q", got, want)
	}
	history := ex.events.snapshot()
	if got, want := len(history), 1; got != want {
		t.Fatalf("expected single buffered stdout event, got %d want %d", got, want)
	}
	if got, want := string(history[0].GetStdout()), "head-tail"; got != want {
		t.Fatalf("unexpected buffered stdout event payload: got %q want %q", got, want)
	}
}

func TestAppendRetainedOutputClonesTailSlice(t *testing.T) {
	source := strings.Repeat("x", 1024) + "tail"
	tail := source[len(source)-4:]
	got := appendRetainedOutput("", source, 4)
	if got != "tail" {
		t.Fatalf("unexpected retained tail: got %q want %q", got, "tail")
	}
	if unsafe.StringData(got) == unsafe.StringData(tail) {
		t.Fatal("expected retained tail to be copied, but it reuses source backing storage")
	}
}

func TestRetainedOutputCaptureBoundsStoredSuffix(t *testing.T) {
	capture := newRetainedOutputCapture(8)
	if _, err := capture.Write([]byte("hello")); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if _, err := capture.Write([]byte("-world")); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if got, want := capture.String(), "lo-world"; got != want {
		t.Fatalf("unexpected retained capture suffix: got %q want %q", got, want)
	}
}

func TestStatePruningBoundsRetainedTerminalState(t *testing.T) {
	svc := newTestService(&stubAdapter{})
	retention := testRetentionPolicy()
	retention.maxRetainedStoppedSandboxes = 1
	retention.maxRetainedFinishedExecutions = 2
	retention.retainedStateMaxAge = 24 * time.Hour
	svc.runtime.retention = retention

	runOnce := func() (string, string) {
		createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
		if err != nil {
			t.Fatalf("CreateSandbox returned error: %v", err)
		}
		sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

		createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
			SandboxId: sandboxID,
			Command:   []string{"echo", "ok"},
		})
		if err != nil {
			t.Fatalf("CreateExecution returned error: %v", err)
		}
		executionID := createExecutionResp.GetExecution().GetExecutionId()

		if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
			t.Fatalf("WaitExecution returned error: %v", err)
		}

		if _, err := svc.TerminateSandbox(context.Background(), &cleanroomv1.TerminateSandboxRequest{
			SandboxId: sandboxID,
		}); err != nil {
			t.Fatalf("TerminateSandbox returned error: %v", err)
		}
		return sandboxID, executionID
	}

	firstSandboxID, firstExecutionID := runOnce()
	_, _ = runOnce()
	lastSandboxID, lastExecutionID := runOnce()

	svc.mu.RLock()
	defer svc.mu.RUnlock()

	stoppedSandboxes := 0
	for _, sb := range svc.sandboxes {
		if sb.Status == cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED {
			stoppedSandboxes++
		}
	}
	if got, want := stoppedSandboxes, 1; got != want {
		t.Fatalf("unexpected retained stopped sandboxes: got %d want %d", got, want)
	}
	if got, want := len(svc.executions), 2; got != want {
		t.Fatalf("unexpected retained finished executions: got %d want %d", got, want)
	}
	if _, ok := svc.sandboxes[firstSandboxID]; ok {
		t.Fatalf("expected oldest stopped sandbox %q to be pruned", firstSandboxID)
	}
	if _, ok := svc.executions[executionKey(firstSandboxID, firstExecutionID)]; ok {
		t.Fatalf("expected oldest finished execution %q to be pruned", firstExecutionID)
	}
	if _, ok := svc.sandboxes[lastSandboxID]; !ok {
		t.Fatalf("expected newest stopped sandbox %q to be retained", lastSandboxID)
	}
	if _, ok := svc.executions[executionKey(lastSandboxID, lastExecutionID)]; !ok {
		t.Fatalf("expected newest finished execution %q to be retained", lastExecutionID)
	}
}

func TestListExecutionsSupportsRetainedExecutionFromPrunedSandbox(t *testing.T) {
	svc := newTestService(&stubAdapter{})
	retention := testRetentionPolicy()
	retention.maxRetainedStoppedSandboxes = 1
	retention.maxRetainedFinishedExecutions = 2
	retention.retainedStateMaxAge = 24 * time.Hour
	svc.runtime.retention = retention

	runOnce := func() (string, string) {
		createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
		if err != nil {
			t.Fatalf("CreateSandbox returned error: %v", err)
		}
		sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

		createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
			SandboxId: sandboxID,
			Command:   []string{"echo", "ok"},
		})
		if err != nil {
			t.Fatalf("CreateExecution returned error: %v", err)
		}
		executionID := createExecutionResp.GetExecution().GetExecutionId()

		if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
			t.Fatalf("WaitExecution returned error: %v", err)
		}
		if _, err := svc.TerminateSandbox(context.Background(), &cleanroomv1.TerminateSandboxRequest{SandboxId: sandboxID}); err != nil {
			t.Fatalf("TerminateSandbox returned error: %v", err)
		}
		return sandboxID, executionID
	}

	_, _ = runOnce()
	prunedSandboxID, retainedExecutionID := runOnce()
	_, _ = runOnce()

	resp, err := svc.ListExecutions(context.Background(), &cleanroomv1.ListExecutionsRequest{
		SandboxId: prunedSandboxID,
		All:       true,
	})
	if err != nil {
		t.Fatalf("ListExecutions returned error: %v", err)
	}
	if got, want := len(resp.GetExecutions()), 1; got != want {
		t.Fatalf("unexpected execution count: got %d want %d", got, want)
	}
	if got, want := resp.GetExecutions()[0].GetExecutionId(), retainedExecutionID; got != want {
		t.Fatalf("unexpected execution id: got %q want %q", got, want)
	}
	if got, want := resp.GetExecutions()[0].GetSandboxId(), prunedSandboxID; got != want {
		t.Fatalf("unexpected sandbox id: got %q want %q", got, want)
	}
}

func TestListSandboxesReturnsNewestSnapshotFirst(t *testing.T) {
	svc := newTestService(&stubAdapter{})
	first := &sandboxState{
		ID:        "sandbox_first",
		Status:    cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY,
		CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
		UpdatedAt: time.Unix(1_700_000_010, 0).UTC(),
		events:    newEventFeed[*cleanroomv1.SandboxEvent](0),
		Done:      make(chan struct{}),
	}
	second := &sandboxState{
		ID:        "sandbox_second",
		Status:    cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED,
		CreatedAt: time.Unix(1_700_000_100, 0).UTC(),
		UpdatedAt: time.Unix(1_700_000_200, 0).UTC(),
		events:    newEventFeed[*cleanroomv1.SandboxEvent](0),
		Done:      make(chan struct{}),
	}

	svc.mu.Lock()
	svc.ensureMapsLocked()
	svc.sandboxes[first.ID] = first
	svc.sandboxes[second.ID] = second
	svc.mu.Unlock()

	resp, err := svc.ListSandboxes(context.Background(), &cleanroomv1.ListSandboxesRequest{})
	if err != nil {
		t.Fatalf("ListSandboxes returned error: %v", err)
	}

	sandboxes := resp.GetSandboxes()
	if got, want := len(sandboxes), 2; got != want {
		t.Fatalf("unexpected sandbox count: got %d want %d", got, want)
	}
	if got, want := sandboxes[0].GetSandboxId(), second.ID; got != want {
		t.Fatalf("unexpected first sandbox id: got %q want %q", got, want)
	}
	if got, want := sandboxes[0].GetUpdatedAt().AsTime(), second.UpdatedAt; !got.Equal(want) {
		t.Fatalf("unexpected first sandbox updated_at: got %v want %v", got, want)
	}
	if got, want := sandboxes[1].GetSandboxId(), first.ID; got != want {
		t.Fatalf("unexpected second sandbox id: got %q want %q", got, want)
	}
}

func TestExecutionRetentionBoundsOutput(t *testing.T) {
	var gotStreamLimit *int
	adapter := &stubAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if stream.BufferedOutputLimitBytes != nil {
				limit := *stream.BufferedOutputLimitBytes
				gotStreamLimit = &limit
			}
			for _, chunk := range []string{"1234", "5678", "90"} {
				if stream.OnStdout != nil {
					stream.OnStdout([]byte(chunk))
				}
			}
			for _, chunk := range []string{"abcd", "efgh", "ij"} {
				if stream.OnStderr != nil {
					stream.OnStderr([]byte(chunk))
				}
			}
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    0,
				LaunchedVM:  false,
				PlanPath:    "/tmp/plan",
				RunDir:      "/tmp/run",
				Message:     "ok",
				Stdout:      "1234567890",
				Stderr:      "abcdefghij",
			}, nil
		},
	}
	svc := newTestService(adapter)
	retention := testRetentionPolicy()
	retention.maxRetainedExecutionOutputBytes = 8
	svc.runtime.retention = retention

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"echo", "bounded"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()

	if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}

	snapshot, err := svc.ExecutionSnapshot(sandboxID, executionID)
	if err != nil {
		t.Fatalf("ExecutionSnapshot returned error: %v", err)
	}
	if got, want := snapshot.Stdout, "34567890"; got != want {
		t.Fatalf("unexpected retained stdout: got %q want %q", got, want)
	}
	if got, want := snapshot.Stderr, "cdefghij"; got != want {
		t.Fatalf("unexpected retained stderr: got %q want %q", got, want)
	}
	if gotStreamLimit == nil {
		t.Fatalf("expected stream buffered output limit to be set")
	}
	if got, want := *gotStreamLimit, 8; got != want {
		t.Fatalf("unexpected stream buffered output limit: got %d want %d", got, want)
	}
}

func TestExecutionRetentionSetsZeroStreamOutputLimit(t *testing.T) {
	var gotStreamLimit *int
	adapter := &stubAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if stream.BufferedOutputLimitBytes != nil {
				limit := *stream.BufferedOutputLimitBytes
				gotStreamLimit = &limit
			}
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    0,
			}, nil
		},
	}
	svc := newTestService(adapter)
	retention := testRetentionPolicy()
	retention.maxRetainedExecutionOutputBytes = 0
	svc.runtime.retention = retention

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"echo", "disabled"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()

	if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}
	if gotStreamLimit == nil {
		t.Fatalf("expected stream buffered output limit to be set")
	}
	if got := *gotStreamLimit; got != 0 {
		t.Fatalf("unexpected stream buffered output limit: got %d want 0", got)
	}
}

func TestExecutionRetentionBoundsEventHistory(t *testing.T) {
	adapter := &stubAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			for _, chunk := range []string{"1", "2", "3", "4"} {
				if stream.OnStdout != nil {
					stream.OnStdout([]byte(chunk))
				}
			}
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    0,
				LaunchedVM:  false,
				PlanPath:    "/tmp/plan",
				RunDir:      "/tmp/run",
				Message:     "ok",
				Stdout:      "1234",
			}, nil
		},
	}
	svc := newTestService(adapter)
	retention := testRetentionPolicy()
	retention.maxRetainedExecutionEvents = 3
	svc.runtime.retention = retention

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"echo", "events"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()

	if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}

	svc.mu.RLock()
	ex := svc.executions[executionKey(sandboxID, executionID)]
	if ex == nil {
		svc.mu.RUnlock()
		t.Fatal("expected execution state to exist")
	}
	history := ex.events.snapshot()
	svc.mu.RUnlock()

	if got, want := len(history), 3; got != want {
		t.Fatalf("unexpected retained execution events: got %d want %d", got, want)
	}
	if got, want := string(history[0].GetStdout()), "3"; got != want {
		t.Fatalf("unexpected first retained stdout event: got %q want %q", got, want)
	}
	if got, want := string(history[1].GetStdout()), "4"; got != want {
		t.Fatalf("unexpected second retained stdout event: got %q want %q", got, want)
	}
	if history[2].GetExit() == nil {
		t.Fatalf("expected exit event in retained history, got %+v", history[2].Payload)
	}
}

func TestSandboxRetentionBoundsEventHistory(t *testing.T) {
	svc := newTestService(&stubAdapter{})
	retention := testRetentionPolicy()
	retention.maxRetainedSandboxEvents = 2
	svc.runtime.retention = retention

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()

	if _, err := svc.TerminateSandbox(context.Background(), &cleanroomv1.TerminateSandboxRequest{
		SandboxId: sandboxID,
	}); err != nil {
		t.Fatalf("TerminateSandbox returned error: %v", err)
	}

	history, _, _, unsubscribe, err := svc.SubscribeSandboxEvents(sandboxID)
	if err != nil {
		t.Fatalf("SubscribeSandboxEvents returned error: %v", err)
	}
	defer unsubscribe()

	if got, want := len(history), 2; got != want {
		t.Fatalf("unexpected retained sandbox events: got %d want %d", got, want)
	}
	if got, want := history[0].GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPING; got != want {
		t.Fatalf("unexpected first retained sandbox status: got %v want %v", got, want)
	}
	if got, want := history[1].GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED; got != want {
		t.Fatalf("unexpected second retained sandbox status: got %v want %v", got, want)
	}
}

func TestStreamedOutputArrivesBeforeExecutionExit(t *testing.T) {
	adapter := &stubAdapter{
		runStreamFn: func(ctx context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("chunk-1\n"))
			}
			time.Sleep(50 * time.Millisecond)
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("chunk-2\n"))
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    0,
				LaunchedVM:  false,
				PlanPath:    "/tmp/plan",
				RunDir:      "/tmp/run",
				Message:     "ok",
				Stdout:      "chunk-1\nchunk-2\n",
			}, nil
		},
	}
	svc := newTestService(adapter)

	sandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := sandboxResp.GetSandbox().GetSandboxId()

	execResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"echo", "stream"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := execResp.GetExecution().GetExecutionId()

	_, updates, done, unsubscribe, err := svc.SubscribeExecutionEvents(sandboxID, executionID)
	if err != nil {
		t.Fatalf("SubscribeExecutionEvents returned error: %v", err)
	}
	defer unsubscribe()

	sawStdoutBeforeDone := false
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	for !sawStdoutBeforeDone {
		select {
		case event, ok := <-updates:
			if !ok {
				t.Fatal("stream closed before stdout event")
			}
			if _, ok := event.Payload.(*cleanroomv1.ExecutionStreamEvent_Stdout); ok {
				sawStdoutBeforeDone = true
			}
		case <-done:
			t.Fatal("execution finished before any streamed stdout event")
		case <-timeout.C:
			t.Fatal("timed out waiting for streamed stdout event")
		}
	}
}

func TestCreateExecutionDerivesKindFromTTY(t *testing.T) {
	svc := newTestService(&stubAdapter{})

	sandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := sandboxResp.GetSandbox().GetSandboxId()

	batchResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"echo", "batch"},
	})
	if err != nil {
		t.Fatalf("CreateExecution batch returned error: %v", err)
	}
	if got, want := batchResp.GetExecution().GetKind(), cleanroomv1.ExecutionKind_EXECUTION_KIND_BATCH; got != want {
		t.Fatalf("unexpected batch kind: got %v want %v", got, want)
	}

	if _, err := svc.WaitExecution(context.Background(), sandboxID, batchResp.GetExecution().GetExecutionId()); err != nil {
		t.Fatalf("WaitExecution batch returned error: %v", err)
	}

	interactiveResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"echo", "interactive"},
		Options: &cleanroomv1.ExecutionOptions{
			Tty: true,
		},
	})
	if err != nil {
		t.Fatalf("CreateExecution interactive returned error: %v", err)
	}
	if got, want := interactiveResp.GetExecution().GetKind(), cleanroomv1.ExecutionKind_EXECUTION_KIND_INTERACTIVE; got != want {
		t.Fatalf("unexpected interactive kind: got %v want %v", got, want)
	}
}

func TestAttachExecutionRejectsBatchExecution(t *testing.T) {
	release := make(chan struct{})
	adapter := &stubAdapter{
		runFn: func(ctx context.Context, req backend.ExecutionRequest) (*backend.ExecutionResult, error) {
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    0,
				LaunchedVM:  false,
				PlanPath:    "/tmp/plan",
				RunDir:      "/tmp/run",
				Message:     "ok",
			}, nil
		},
	}
	svc := newTestService(adapter)
	svc.ConfigureInteractiveTransport("127.0.0.1:4433", "cleanroom-interactive-v1", "abc123")

	sandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := sandboxResp.GetSandbox().GetSandboxId()

	execResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"echo", "batch"},
		Kind:      cleanroomv1.ExecutionKind_EXECUTION_KIND_BATCH,
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}

	_, err = svc.AttachExecution(context.Background(), &cleanroomv1.AttachExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: execResp.GetExecution().GetExecutionId(),
	})
	if err == nil {
		t.Fatal("expected AttachExecution to fail for batch execution")
	}
	if !strings.Contains(err.Error(), "not interactive") {
		t.Fatalf("expected not interactive error, got %v", err)
	}

	close(release)
}

func TestAttachExecutionReturnsSessionToken(t *testing.T) {
	release := make(chan struct{})
	adapter := &stubAdapter{
		runFn: func(ctx context.Context, req backend.ExecutionRequest) (*backend.ExecutionResult, error) {
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    0,
				LaunchedVM:  false,
				PlanPath:    "/tmp/plan",
				RunDir:      "/tmp/run",
				Message:     "ok",
			}, nil
		},
	}
	svc := newTestService(adapter)
	svc.ConfigureInteractiveTransport("127.0.0.1:4433", "cleanroom-interactive-v1", "abc123")

	sandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := sandboxResp.GetSandbox().GetSandboxId()

	execResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"sh"},
		Kind:      cleanroomv1.ExecutionKind_EXECUTION_KIND_INTERACTIVE,
		Options: &cleanroomv1.ExecutionOptions{
			Tty: true,
		},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := execResp.GetExecution().GetExecutionId()

	openResp, err := svc.AttachExecution(context.Background(), &cleanroomv1.AttachExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		InitialCols: 120,
		InitialRows: 42,
	})
	if err != nil {
		t.Fatalf("AttachExecution returned error: %v", err)
	}
	if openResp.GetSessionId() == "" {
		t.Fatal("expected session_id")
	}
	if openResp.GetSessionToken() == "" {
		t.Fatal("expected session_token")
	}
	if openResp.GetExpiresAt() == nil {
		t.Fatal("expected expires_at")
	}
	if got, want := openResp.GetQuicEndpoint(), "127.0.0.1:4433"; got != want {
		t.Fatalf("unexpected quic endpoint: got %q want %q", got, want)
	}
	if got, want := openResp.GetAlpn(), "cleanroom-interactive-v1"; got != want {
		t.Fatalf("unexpected alpn: got %q want %q", got, want)
	}
	if got, want := openResp.GetServerCertPinSha256(), "abc123"; got != want {
		t.Fatalf("unexpected cert pin: got %q want %q", got, want)
	}
	if !openResp.GetExpiresAt().AsTime().After(time.Now().UTC()) {
		t.Fatalf("expected expires_at in the future, got %v", openResp.GetExpiresAt().AsTime())
	}

	close(release)
	if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}
}

func TestConsumeInteractiveSessionIsSingleUse(t *testing.T) {
	release := make(chan struct{})
	adapter := &stubAdapter{
		runFn: func(ctx context.Context, req backend.ExecutionRequest) (*backend.ExecutionResult, error) {
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    0,
				LaunchedVM:  false,
				PlanPath:    "/tmp/plan",
				RunDir:      "/tmp/run",
				Message:     "ok",
			}, nil
		},
	}
	svc := newTestService(adapter)
	svc.ConfigureInteractiveTransport("127.0.0.1:4433", "cleanroom-interactive-v1", "abc123")

	sandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := sandboxResp.GetSandbox().GetSandboxId()

	execResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"sh"},
		Kind:      cleanroomv1.ExecutionKind_EXECUTION_KIND_INTERACTIVE,
		Options: &cleanroomv1.ExecutionOptions{
			Tty: true,
		},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}

	openResp, err := svc.AttachExecution(context.Background(), &cleanroomv1.AttachExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: execResp.GetExecution().GetExecutionId(),
	})
	if err != nil {
		t.Fatalf("AttachExecution returned error: %v", err)
	}

	session, err := svc.ConsumeInteractiveSession(openResp.GetSessionId(), openResp.GetSessionToken())
	if err != nil {
		t.Fatalf("ConsumeInteractiveSession returned error: %v", err)
	}
	if got, want := session.ExecutionID, execResp.GetExecution().GetExecutionId(); got != want {
		t.Fatalf("unexpected execution id: got %q want %q", got, want)
	}

	if _, err := svc.ConsumeInteractiveSession(openResp.GetSessionId(), openResp.GetSessionToken()); err == nil {
		t.Fatal("expected second ConsumeInteractiveSession call to fail")
	}

	close(release)
	if _, err := svc.WaitExecution(context.Background(), sandboxID, execResp.GetExecution().GetExecutionId()); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}
}

func TestAttachExecutionEnforcesSingleActiveAttach(t *testing.T) {
	release := make(chan struct{})
	adapter := &stubAdapter{
		runFn: func(ctx context.Context, req backend.ExecutionRequest) (*backend.ExecutionResult, error) {
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    0,
				LaunchedVM:  false,
				PlanPath:    "/tmp/plan",
				RunDir:      "/tmp/run",
				Message:     "ok",
			}, nil
		},
	}
	svc := newTestService(adapter)
	svc.ConfigureInteractiveTransport("127.0.0.1:4433", "cleanroom-interactive-v1", "abc123")

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"sh"},
		Kind:      cleanroomv1.ExecutionKind_EXECUTION_KIND_INTERACTIVE,
		Options: &cleanroomv1.ExecutionOptions{
			Tty: true,
		},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()

	firstOpenResp, err := svc.AttachExecution(context.Background(), &cleanroomv1.AttachExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
	})
	if err != nil {
		t.Fatalf("first AttachExecution returned error: %v", err)
	}

	if _, err := svc.AttachExecution(context.Background(), &cleanroomv1.AttachExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
	}); err == nil || !strings.Contains(err.Error(), "pending interactive session") {
		t.Fatalf("expected pending interactive session error, got %v", err)
	}

	if _, err := svc.ConsumeInteractiveSession(firstOpenResp.GetSessionId(), firstOpenResp.GetSessionToken()); err != nil {
		t.Fatalf("ConsumeInteractiveSession returned error: %v", err)
	}

	if _, err := svc.AttachExecution(context.Background(), &cleanroomv1.AttachExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
	}); err == nil || !strings.Contains(err.Error(), "active interactive session") {
		t.Fatalf("expected active interactive session error, got %v", err)
	}

	svc.ReleaseInteractiveExecution(sandboxID, executionID)

	if _, err := svc.AttachExecution(context.Background(), &cleanroomv1.AttachExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
	}); err != nil {
		t.Fatalf("expected open to succeed after releasing active session, got %v", err)
	}

	close(release)
	if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}
}

func collectExecutionEvents(t *testing.T, history []*cleanroomv1.ExecutionStreamEvent, updates <-chan *cleanroomv1.ExecutionStreamEvent, done <-chan struct{}) []*cleanroomv1.ExecutionStreamEvent {
	t.Helper()
	events := append([]*cleanroomv1.ExecutionStreamEvent(nil), history...)
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()

	for {
		select {
		case event, ok := <-updates:
			if ok {
				events = append(events, event)
			}
		case <-done:
			for {
				select {
				case event, ok := <-updates:
					if !ok {
						return events
					}
					events = append(events, event)
				default:
					return events
				}
			}
		case <-timer.C:
			t.Fatalf("timed out collecting execution events")
		}
	}
}
