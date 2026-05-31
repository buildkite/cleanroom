package controlservice

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
	"unsafe"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/cachestore"
	"github.com/buildkite/cleanroom/internal/changesetstore"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/paths"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorybundle"
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
	"google.golang.org/protobuf/proto"
)

type stubAdapter struct {
	result                     *backend.ExecutionResult
	runFn                      func(context.Context, backend.ExecutionRequest) (*backend.ExecutionResult, error)
	runStreamFn                func(context.Context, backend.ExecutionRequest, backend.OutputStream) (*backend.ExecutionResult, error)
	provisionFn                func(context.Context, backend.ProvisionRequest) error
	provisionFromSnapshotFn    func(context.Context, backend.ProvisionFromSnapshotRequest) error
	createSnapshotFn           func(context.Context, backend.SnapshotRequest) (*backend.SnapshotResult, error)
	snapshotCacheOutputsFn     func(context.Context, backend.SnapshotCacheOutputVolumesRequest) (*backend.SnapshotCacheOutputVolumesResult, error)
	deleteSnapshotFn           func(context.Context, backend.DeleteSnapshotRequest) error
	terminateFn                func(context.Context, string) error
	downloadFn                 func(context.Context, string, string, int64) ([]byte, error)
	uploadFn                   func(context.Context, string, string, []byte, fs.FileMode) error
	statFn                     func(context.Context, string, string) (*backend.SandboxPathInfo, error)
	walkFn                     func(context.Context, string, string, func(backend.SandboxPathInfo) error) error
	readFn                     func(context.Context, string, string, int64, func([]byte) error) error
	writeFn                    func(context.Context, string, string, io.Reader, fs.FileMode, time.Time) (int64, error)
	removeFn                   func(context.Context, string, string, bool) error
	archiveFn                  func(context.Context, string, []string, int64, func([]byte) error) error
	extractFn                  func(context.Context, string, string, io.Reader) (int64, error)
	req                        backend.ExecutionRequest
	provisionReq               backend.ProvisionRequest
	provisionFromSnapshotReq   backend.ProvisionFromSnapshotRequest
	createSnapshotReq          backend.SnapshotRequest
	snapshotCacheOutputsReq    backend.SnapshotCacheOutputVolumesRequest
	deleteSnapshotReq          backend.DeleteSnapshotRequest
	deleteSnapshotRequests     []backend.DeleteSnapshotRequest
	runCalls                   int
	provisionCalls             int
	provisionFromSnapshotCalls int
	createSnapshotCalls        int
	snapshotCacheOutputsCalls  int
	deleteSnapshotCalls        int
	terminateCalls             int
	runtimeBaseKeyOverride     string
	runtimeBaseKeyErr          error
}

type portDialAdapter struct {
	stubAdapter
	capabilities map[string]bool
	dialFn       func(context.Context, string, int) (net.Conn, error)
}

type suspendablePortDialAdapter struct {
	suspendableAdapter
	dialMu    sync.Mutex
	dialFn    func(context.Context, string, int) (net.Conn, error)
	dialCalls int
}

type suspendableAdapter struct {
	stubAdapter
	capabilities   map[string]bool
	suspendFn      func(context.Context, string) error
	resumeFn       func(context.Context, string) error
	suspendCalls   int
	resumeCalls    int
	suspendSandbox string
	resumeSandbox  string
}

type suspendOnlyAdapter struct {
	resumeFn     func(context.Context, string) error
	suspendCalls int
	resumeCalls  int
}

func (s *suspendableAdapter) SuspendSandbox(ctx context.Context, sandboxID string) error {
	s.suspendCalls++
	s.suspendSandbox = sandboxID
	if s.suspendFn != nil {
		return s.suspendFn(ctx, sandboxID)
	}
	return nil
}

func (s *suspendOnlyAdapter) Name() string { return "suspend-only" }

func (s *suspendOnlyAdapter) ProvisionSandbox(context.Context, backend.ProvisionRequest) error {
	return nil
}

func (s *suspendOnlyAdapter) RunInSandbox(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
	return &backend.ExecutionResult{
		ExecutionID: req.ExecutionID,
		ExitCode:    0,
		LaunchedVM:  true,
		PlanPath:    "/tmp/plan",
		RunDir:      "/tmp/run",
		Message:     "ok",
	}, nil
}

func (s *suspendOnlyAdapter) TerminateSandbox(context.Context, string) error {
	return nil
}

func (s *suspendOnlyAdapter) SuspendSandbox(context.Context, string) error {
	s.suspendCalls++
	return nil
}

func (s *suspendOnlyAdapter) ResumeSandbox(ctx context.Context, sandboxID string) error {
	s.resumeCalls++
	if s.resumeFn != nil {
		return s.resumeFn(ctx, sandboxID)
	}
	return nil
}

func (s *suspendableAdapter) ResumeSandbox(ctx context.Context, sandboxID string) error {
	s.resumeCalls++
	s.resumeSandbox = sandboxID
	if s.resumeFn != nil {
		return s.resumeFn(ctx, sandboxID)
	}
	return nil
}

func (s *suspendableAdapter) Capabilities() map[string]bool {
	return s.capabilities
}

func (s *portDialAdapter) DialSandboxPort(ctx context.Context, sandboxID string, port int) (net.Conn, error) {
	if s.dialFn != nil {
		return s.dialFn(ctx, sandboxID, port)
	}
	return nil, errors.New("dial not configured")
}

func (s *suspendablePortDialAdapter) DialSandboxPort(ctx context.Context, sandboxID string, port int) (net.Conn, error) {
	s.dialMu.Lock()
	s.dialCalls++
	s.dialMu.Unlock()
	if s.dialFn != nil {
		return s.dialFn(ctx, sandboxID, port)
	}
	return nil, errors.New("dial not configured")
}

func (s *portDialAdapter) Capabilities() map[string]bool {
	return s.capabilities
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

func (s *stubAdapter) SnapshotCacheOutputVolumes(ctx context.Context, req backend.SnapshotCacheOutputVolumesRequest) (*backend.SnapshotCacheOutputVolumesResult, error) {
	s.snapshotCacheOutputsReq = req
	s.snapshotCacheOutputsCalls++
	if s.snapshotCacheOutputsFn != nil {
		return s.snapshotCacheOutputsFn(ctx, req)
	}
	return &backend.SnapshotCacheOutputVolumesResult{}, nil
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
	s.deleteSnapshotRequests = append(s.deleteSnapshotRequests, req)
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

func (s *stubAdapter) UploadSandboxFile(ctx context.Context, sandboxID, path string, data []byte, mode fs.FileMode) error {
	if s.uploadFn != nil {
		return s.uploadFn(ctx, sandboxID, path, data, mode)
	}
	return errors.New("upload not configured")
}

func (s *stubAdapter) StatSandboxPath(ctx context.Context, sandboxID, path string) (*backend.SandboxPathInfo, error) {
	if s.statFn != nil {
		return s.statFn(ctx, sandboxID, path)
	}
	return nil, errors.New("stat not configured")
}

func (s *stubAdapter) WalkSandboxTree(ctx context.Context, sandboxID, path string, emit func(backend.SandboxPathInfo) error) error {
	if s.walkFn != nil {
		return s.walkFn(ctx, sandboxID, path, emit)
	}
	return errors.New("walk not configured")
}

func (s *stubAdapter) ReadSandboxFile(ctx context.Context, sandboxID, path string, maxBytes int64, emit func([]byte) error) error {
	if s.readFn != nil {
		return s.readFn(ctx, sandboxID, path, maxBytes, emit)
	}
	return errors.New("read not configured")
}

func (s *stubAdapter) WriteSandboxFile(ctx context.Context, sandboxID, path string, r io.Reader, mode fs.FileMode, mtime time.Time) (int64, error) {
	if s.writeFn != nil {
		return s.writeFn(ctx, sandboxID, path, r, mode, mtime)
	}
	return 0, errors.New("write not configured")
}

func (s *stubAdapter) RemoveSandboxPath(ctx context.Context, sandboxID, path string, recursive bool) error {
	if s.removeFn != nil {
		return s.removeFn(ctx, sandboxID, path, recursive)
	}
	return errors.New("remove not configured")
}

func (s *stubAdapter) ArchiveSandboxPaths(ctx context.Context, sandboxID string, paths []string, maxBytes int64, emit func([]byte) error) error {
	if s.archiveFn != nil {
		return s.archiveFn(ctx, sandboxID, paths, maxBytes, emit)
	}
	return errors.New("archive not configured")
}

func (s *stubAdapter) ExtractSandboxArchive(ctx context.Context, sandboxID, destination string, r io.Reader) (int64, error) {
	if s.extractFn != nil {
		return s.extractFn(ctx, sandboxID, destination, r)
	}
	return 0, errors.New("extract not configured")
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
	remoteURL                 string
	commitSHA                 string
	commitSHAs                []string
	withRepositorySHAs        []string
	calls                     int
	err                       error
	ensureContainsFn          func(remoteURL, commitSHA string) error
	mirrorPath                string
	mirrorPathCalls           int
	mirrorPathErr             error
	ensureMirrorCalls         int
	ensureMirrorErr           error
	refreshCalls              int
	refreshErr                error
	ensureSubmoduleMirrorFunc func(ctx context.Context, submoduleRemoteURL, gitlinkSHA string) (string, error)
}

type stubClock struct {
	now time.Time
}

func (s *stubRepositoryMirrorStore) EnsureCommit(_ context.Context, remoteURL, commitSHA string, _ repositorystore.FetchHints) error {
	s.remoteURL = remoteURL
	s.commitSHA = commitSHA
	s.commitSHAs = append(s.commitSHAs, commitSHA)
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

func (s *stubRepositoryMirrorStore) Refresh(_ context.Context, remoteURL string, _ repositorystore.FetchHints) error {
	s.remoteURL = remoteURL
	s.refreshCalls++
	if s.refreshErr != nil {
		return s.refreshErr
	}
	return nil
}

func (s *stubRepositoryMirrorStore) RefreshMirror(ctx context.Context, remoteURL string) (string, error) {
	if err := s.Refresh(ctx, remoteURL, repositorystore.FetchHints{}); err != nil {
		return "", err
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
			if isGitShowFileMissingError(message, path) {
				return repositorystore.NewFileNotFoundError(commitSHA, path)
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
	s.withRepositorySHAs = append(s.withRepositorySHAs, commitSHA)
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

func (s *stubRepositoryMirrorStore) EnsureSubmoduleMirror(ctx context.Context, submoduleRemoteURL, gitlinkSHA string) (string, error) {
	if s.ensureSubmoduleMirrorFunc != nil {
		return s.ensureSubmoduleMirrorFunc(ctx, submoduleRemoteURL, gitlinkSHA)
	}
	return "", nil
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
		Blocks: []*cleanroomv1.PolicyBlock{testPolicyBlock(
			"go-modules",
			[]string{"mise", "exec", "--", "go", "mod", "download"},
			[]string{"go.mod", "go.sum"},
			[]string{"/root/go/pkg/mod"},
			nil,
		)},
	}
	return policyProto
}

func testRepositoryDependencyAndServicesPolicy() *cleanroomv1.Policy {
	policyProto := testRepositoryDependencyPolicy()
	policyProto.Docker = &cleanroomv1.PolicyDocker{Required: true}
	policyProto.Services = &cleanroomv1.PolicyServices{
		Blocks: []*cleanroomv1.PolicyBlock{testPolicyBlock(
			"postgres",
			[]string{"docker", "compose", "up", "-d", "postgres"},
			[]string{"docker-compose.yml"},
			[]string{"/var/lib/cleanroom/services/postgres"},
			nil,
		)},
	}

	return policyProto
}

func testRepositoryPolicyYAML(destinationDir string, submodules, allowGitHub bool) string {
	allowBlock := ""
	if allowGitHub {
		allowBlock = `
    allow:
      - host: github.com
        ports: [443]`
	}
	return fmt.Sprintf(`version: 1
repository:
  path: %s
  submodules: %t
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  network:
    default: deny%s
`, destinationDir, submodules, allowBlock)
}

func testRepositoryPortableDependencyPolicy() *cleanroomv1.Policy {
	policyProto := testRepositoryDependencyPolicy()
	policyProto.Dependencies.Reuse = policy.DependencyReusePortable

	return policyProto
}

func writePortableDependencyValidationDigest(command []string, stream backend.OutputStream, keyFilesDigest, toolchainInputsDigest string) {
	if stream.OnStdout == nil {
		return
	}
	joined := strings.Join(command, "\n")
	if !strings.Contains(joined, "sha256sum") {
		return
	}
	if strings.Contains(joined, "mise.toml") || strings.Contains(joined, ".tool-versions") {
		stream.OnStdout([]byte(toolchainInputsDigest + "\n"))
		return
	}
	stream.OnStdout([]byte(keyFilesDigest + "\n"))
}

func testPolicyBlock(name string, command, inputFiles, outputDirs, outputFiles []string) *cleanroomv1.PolicyBlock {
	return &cleanroomv1.PolicyBlock{
		Name:    name,
		Command: append([]string(nil), command...),
		Inputs: &cleanroomv1.PolicyBlockInputs{
			Files: append([]string(nil), inputFiles...),
		},
		Outputs: &cleanroomv1.PolicyBlockOutputs{
			Dirs:  append([]string(nil), outputDirs...),
			Files: append([]string(nil), outputFiles...),
		},
	}
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
		Backends:       map[string]backend.Adapter{"firecracker": adapter},
		SnapshotStore:  store,
		CacheStore:     newMemoryCacheStore(),
		ChangesetStore: newMemoryChangesetStore(),
	}
}

func TestCreateSandboxRejectsAllowDefaultPolicyProto(t *testing.T) {
	t.Parallel()

	policyProto := testPolicy()
	policyProto.NetworkDefault = "allow"
	svc := newTestService(&stubAdapter{})

	_, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy: policyProto,
	})
	if err == nil {
		t.Fatal("expected CreateSandbox to reject allow-default policy protobuf")
	}
	if !strings.Contains(err.Error(), "network_default=allow") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreateSandboxDangerouslyAllowAllOptionAppliesServerSide(t *testing.T) {
	t.Parallel()

	adapter := &stubAdapter{}
	svc := newTestService(adapter)
	policyProto := testPolicy()
	policyProto.Allow = []*cleanroomv1.PolicyAllowRule{{Host: "api.github.com", Ports: []int32{443}}}

	_, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy: policyProto,
		Options: &cleanroomv1.SandboxOptions{
			DangerouslyAllowAllEgress: true,
		},
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if adapter.provisionReq.Policy == nil {
		t.Fatal("expected provisioned policy")
	}
	if got, want := adapter.provisionReq.Policy.NetworkDefault, "allow"; got != want {
		t.Fatalf("unexpected provisioned network default: got %q want %q", got, want)
	}
	if len(adapter.provisionReq.Policy.Allow) != 0 {
		t.Fatalf("expected allow rules to be cleared, got %v", adapter.provisionReq.Policy.Allow)
	}
}

func TestDialSandboxPortUsesReadySandboxBackendDialer(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	var gotSandboxID string
	var gotPort int
	adapter := &portDialAdapter{
		dialFn: func(_ context.Context, sandboxID string, port int) (net.Conn, error) {
			gotSandboxID = sandboxID
			gotPort = port
			return serverConn, nil
		},
	}
	svc := newTestService(adapter)
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()

	conn, err := svc.DialSandboxPort(context.Background(), &cleanroomv1.SandboxPortOpen{
		SandboxId: sandboxID,
		GuestPort: 3000,
	})
	if err != nil {
		t.Fatalf("DialSandboxPort returned error: %v", err)
	}
	defer conn.Close()
	if gotSandboxID != sandboxID {
		t.Fatalf("unexpected sandbox id: got %q want %q", gotSandboxID, sandboxID)
	}
	if gotPort != 3000 {
		t.Fatalf("unexpected port: got %d want 3000", gotPort)
	}
}

func TestCreateSandboxReportsBackendPortDialCapability(t *testing.T) {
	svc := newTestService(&portDialAdapter{})
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	capabilities := createResp.GetSandbox().GetBackendCapabilities()
	if !capabilities[backend.CapabilitySandboxPortDial] {
		t.Fatalf("expected sandbox to report %s=true, got %v", backend.CapabilitySandboxPortDial, capabilities)
	}
}

func TestDialSandboxPortRejectsDisabledBackendCapability(t *testing.T) {
	dialCalled := false
	adapter := &portDialAdapter{
		capabilities: map[string]bool{backend.CapabilitySandboxPortDial: false},
		dialFn: func(context.Context, string, int) (net.Conn, error) {
			dialCalled = true
			return nil, errors.New("dial should not be called")
		},
	}
	svc := newTestService(adapter)
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	_, err = svc.DialSandboxPort(context.Background(), &cleanroomv1.SandboxPortOpen{
		SandboxId: createResp.GetSandbox().GetSandboxId(),
		GuestPort: 3000,
	})
	if err == nil {
		t.Fatal("expected DialSandboxPort to reject disabled backend capability")
	}
	if !strings.Contains(err.Error(), "does not support sandbox port dialing") {
		t.Fatalf("unexpected DialSandboxPort error: %v", err)
	}
	if dialCalled {
		t.Fatal("expected DialSandboxPort not to call the backend dialer")
	}
}

func TestDialSandboxPortRejectsBackendWithoutDialer(t *testing.T) {
	svc := newTestService(&stubAdapter{})
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	_, err = svc.DialSandboxPort(context.Background(), &cleanroomv1.SandboxPortOpen{
		SandboxId: createResp.GetSandbox().GetSandboxId(),
		GuestPort: 3000,
	})
	if err == nil {
		t.Fatal("expected DialSandboxPort to reject backend without dialer")
	}
	if !strings.Contains(err.Error(), "does not support sandbox port dialing") {
		t.Fatalf("unexpected DialSandboxPort error: %v", err)
	}
}

func TestSuspendSandboxTransitionsReadyToSuspended(t *testing.T) {
	adapter := &suspendableAdapter{}
	svc := newTestService(adapter)
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()
	if !createResp.GetSandbox().GetBackendCapabilities()[backend.CapabilitySandboxSuspend] {
		t.Fatalf("expected sandbox to report %s=true", backend.CapabilitySandboxSuspend)
	}

	resp, err := svc.SuspendSandbox(context.Background(), &cleanroomv1.SuspendSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("SuspendSandbox returned error: %v", err)
	}
	if got, want := resp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_SUSPENDED; got != want {
		t.Fatalf("unexpected suspend response status: got %v want %v", got, want)
	}
	if got, want := adapter.suspendCalls, 1; got != want {
		t.Fatalf("unexpected suspend call count: got %d want %d", got, want)
	}
	if adapter.suspendSandbox != sandboxID {
		t.Fatalf("unexpected suspended sandbox id: got %q want %q", adapter.suspendSandbox, sandboxID)
	}

	getResp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := getResp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_SUSPENDED; got != want {
		t.Fatalf("unexpected sandbox status: got %v want %v", got, want)
	}

	listResp, err := svc.ListSandboxes(context.Background(), &cleanroomv1.ListSandboxesRequest{})
	if err != nil {
		t.Fatalf("ListSandboxes returned error: %v", err)
	}
	if len(listResp.GetSandboxes()) != 1 {
		t.Fatalf("unexpected sandbox count: got %d want 1", len(listResp.GetSandboxes()))
	}
	if got, want := listResp.GetSandboxes()[0].GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_SUSPENDED; got != want {
		t.Fatalf("unexpected listed sandbox status: got %v want %v", got, want)
	}
}

func TestSuspendSandboxUnsupportedBackendLeavesSandboxReady(t *testing.T) {
	svc := newTestService(&stubAdapter{})
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()

	_, err = svc.SuspendSandbox(context.Background(), &cleanroomv1.SuspendSandboxRequest{SandboxId: sandboxID})
	if err == nil {
		t.Fatal("expected SuspendSandbox to reject unsupported backend")
	}
	if !strings.Contains(err.Error(), "does not support sandbox suspend") {
		t.Fatalf("unexpected SuspendSandbox error: %v", err)
	}

	getResp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := getResp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY; got != want {
		t.Fatalf("unexpected sandbox status: got %v want %v", got, want)
	}
}

func TestSuspendSandboxFailureRecoversReady(t *testing.T) {
	adapter := &suspendableAdapter{
		suspendFn: func(context.Context, string) error {
			return errors.New("pause failed")
		},
	}
	svc := newTestService(adapter)
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()

	_, err = svc.SuspendSandbox(context.Background(), &cleanroomv1.SuspendSandboxRequest{SandboxId: sandboxID})
	if err == nil {
		t.Fatal("expected SuspendSandbox to return backend error")
	}
	if !strings.Contains(err.Error(), "suspend backend sandbox") {
		t.Fatalf("unexpected SuspendSandbox error: %v", err)
	}

	getResp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := getResp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY; got != want {
		t.Fatalf("unexpected sandbox status after failed suspend: got %v want %v", got, want)
	}
}

func TestSuspendSandboxIndeterminateFailureLeavesSuspended(t *testing.T) {
	adapter := &suspendableAdapter{
		suspendFn: func(context.Context, string) error {
			return context.DeadlineExceeded
		},
	}
	svc := newTestService(adapter)
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()

	_, err = svc.SuspendSandbox(context.Background(), &cleanroomv1.SuspendSandboxRequest{SandboxId: sandboxID})
	if err == nil {
		t.Fatal("expected SuspendSandbox to return backend error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", err)
	}

	getResp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := getResp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_SUSPENDED; got != want {
		t.Fatalf("unexpected sandbox status after indeterminate suspend: got %v want %v", got, want)
	}

	adapter.suspendFn = nil
	if _, err := svc.ResumeSandbox(context.Background(), &cleanroomv1.ResumeSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("ResumeSandbox returned error: %v", err)
	}
	if got, want := adapter.resumeCalls, 1; got != want {
		t.Fatalf("unexpected resume call count: got %d want %d", got, want)
	}
}

func TestSuspendSandboxBackendIndeterminateFailureLeavesSuspended(t *testing.T) {
	adapter := &suspendableAdapter{
		suspendFn: func(context.Context, string) error {
			return fmt.Errorf("helper response lost: %w", backend.ErrSandboxLifecycleIndeterminate)
		},
	}
	svc := newTestService(adapter)
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()

	_, err = svc.SuspendSandbox(context.Background(), &cleanroomv1.SuspendSandboxRequest{SandboxId: sandboxID})
	if err == nil {
		t.Fatal("expected SuspendSandbox to return backend error")
	}
	if !errors.Is(err, backend.ErrSandboxLifecycleIndeterminate) {
		t.Fatalf("expected indeterminate lifecycle error, got %v", err)
	}

	getResp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := getResp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_SUSPENDED; got != want {
		t.Fatalf("unexpected sandbox status after indeterminate suspend: got %v want %v", got, want)
	}
}

func TestIdleSuspendWorkerSuspendsIdleSandboxAndPublishesEvents(t *testing.T) {
	base := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	adapter := &suspendableAdapter{}
	svc := newTestService(adapter)
	svc.Config.SandboxLifecycle.IdleSuspendAfterSeconds = 600
	svc.runtime.clock = stubClock{now: base}

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()

	svc.runtime.clock = stubClock{now: base.Add(601 * time.Second)}
	svc.runIdleSuspendOnce(context.Background(), 600*time.Second)

	if got, want := adapter.suspendCalls, 1; got != want {
		t.Fatalf("unexpected suspend call count: got %d want %d", got, want)
	}
	getResp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := getResp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_SUSPENDED; got != want {
		t.Fatalf("unexpected sandbox status: got %v want %v", got, want)
	}

	history, _, _, unsubscribe, err := svc.SubscribeSandboxEvents(sandboxID)
	if err != nil {
		t.Fatalf("SubscribeSandboxEvents returned error: %v", err)
	}
	defer unsubscribe()
	if len(history) < 3 {
		t.Fatalf("expected create and idle suspend events, got %d", len(history))
	}
	if got, want := history[len(history)-2].GetMessage(), "sandbox idle suspend requested"; got != want {
		t.Fatalf("unexpected idle suspend request event: got %q want %q", got, want)
	}
	if got, want := history[len(history)-1].GetMessage(), "sandbox suspended"; got != want {
		t.Fatalf("unexpected idle suspend result event: got %q want %q", got, want)
	}
}

func TestIdleSuspendRechecksThresholdBeforeSuspending(t *testing.T) {
	base := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	adapter := &suspendableAdapter{}
	svc := newTestService(adapter)
	svc.runtime.clock = stubClock{now: base}

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()

	svc.runtime.clock = stubClock{now: base.Add(10 * time.Second)}
	_, err = svc.suspendSandbox(context.Background(), sandboxID, suspendSandboxOptions{
		requestedMessage: "sandbox idle suspend requested",
		idleThreshold:    600 * time.Second,
	})
	if err == nil {
		t.Fatal("expected idle suspend to reject a recently active sandbox")
	}
	if !strings.Contains(err.Error(), "not idle long enough") {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := adapter.suspendCalls; got != 0 {
		t.Fatalf("expected backend suspend not to run, got %d calls", got)
	}
}

func TestIdleSuspendWorkerSkipsBusySandboxes(t *testing.T) {
	base := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		busy func(*Service, string)
	}{
		{
			name: "file transfer",
			busy: func(svc *Service, sandboxID string) {
				svc.sandboxes[sandboxID].FileTransferInProgress = true
			},
		},
		{
			name: "repository",
			busy: func(svc *Service, sandboxID string) {
				svc.sandboxes[sandboxID].RepositoryBusy = true
			},
		},
		{
			name: "guest interaction",
			busy: func(svc *Service, sandboxID string) {
				svc.sandboxes[sandboxID].GuestInteractionCount = 1
			},
		},
		{
			name: "execution",
			busy: func(svc *Service, sandboxID string) {
				executionID := "exec_busy"
				svc.sandboxes[sandboxID].ActiveExecutionID = executionID
				svc.executions[executionKey(sandboxID, executionID)] = &executionState{
					ID:        executionID,
					SandboxID: sandboxID,
					Status:    cleanroomv1.ExecutionStatus_EXECUTION_STATUS_RUNNING,
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &suspendableAdapter{}
			svc := newTestService(adapter)
			svc.Config.SandboxLifecycle.IdleSuspendAfterSeconds = 600
			svc.runtime.clock = stubClock{now: base}
			createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
			if err != nil {
				t.Fatalf("CreateSandbox returned error: %v", err)
			}
			sandboxID := createResp.GetSandbox().GetSandboxId()

			svc.mu.Lock()
			tc.busy(svc, sandboxID)
			svc.sandboxes[sandboxID].UpdatedAt = base
			svc.mu.Unlock()

			svc.runtime.clock = stubClock{now: base.Add(601 * time.Second)}
			svc.runIdleSuspendOnce(context.Background(), 600*time.Second)

			if got := adapter.suspendCalls; got != 0 {
				t.Fatalf("expected busy sandbox not to suspend, got %d suspend calls", got)
			}
			getResp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
			if err != nil {
				t.Fatalf("GetSandbox returned error: %v", err)
			}
			if got, want := getResp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY; got != want {
				t.Fatalf("unexpected sandbox status: got %v want %v", got, want)
			}
		})
	}
}

func TestIdleSuspendWorkerSkipsUnsupportedBackends(t *testing.T) {
	base := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	svc := newTestService(&stubAdapter{})
	svc.Config.SandboxLifecycle.IdleSuspendAfterSeconds = 600
	svc.runtime.clock = stubClock{now: base}

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()

	svc.runtime.clock = stubClock{now: base.Add(601 * time.Second)}
	svc.runIdleSuspendOnce(context.Background(), 600*time.Second)

	getResp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := getResp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY; got != want {
		t.Fatalf("unexpected sandbox status: got %v want %v", got, want)
	}
}

func TestResumeSandboxUsesConfiguredWakeTimeout(t *testing.T) {
	adapter := &suspendableAdapter{}
	var sawDeadline bool
	var deadlineErr error
	adapter.resumeFn = func(ctx context.Context, _ string) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			deadlineErr = errors.New("expected resume context to have a deadline")
			return nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 || remaining > 6*time.Second {
			deadlineErr = fmt.Errorf("unexpected resume deadline: %s", remaining)
			return nil
		}
		sawDeadline = true
		return nil
	}
	svc := newTestService(adapter)
	svc.Config.SandboxLifecycle.WakeTimeoutSeconds = 5
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()
	if _, err := svc.SuspendSandbox(context.Background(), &cleanroomv1.SuspendSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("SuspendSandbox returned error: %v", err)
	}

	if _, err := svc.ResumeSandbox(context.Background(), &cleanroomv1.ResumeSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("ResumeSandbox returned error: %v", err)
	}
	if deadlineErr != nil {
		t.Fatal(deadlineErr)
	}
	if !sawDeadline {
		t.Fatal("resume adapter was not called")
	}
}

func TestResumeSandboxDefaultsWakeTimeoutToLaunchTimeout(t *testing.T) {
	adapter := &suspendableAdapter{}
	var sawDeadline bool
	var deadlineErr error
	adapter.resumeFn = func(ctx context.Context, _ string) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			deadlineErr = errors.New("expected resume context to have a deadline")
			return nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 || remaining > 8*time.Second {
			deadlineErr = fmt.Errorf("unexpected resume deadline: %s", remaining)
			return nil
		}
		sawDeadline = true
		return nil
	}
	svc := newTestService(adapter)
	svc.Config.Backends.Firecracker.LaunchSeconds = 7
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()
	if _, err := svc.SuspendSandbox(context.Background(), &cleanroomv1.SuspendSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("SuspendSandbox returned error: %v", err)
	}

	if _, err := svc.ResumeSandbox(context.Background(), &cleanroomv1.ResumeSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("ResumeSandbox returned error: %v", err)
	}
	if deadlineErr != nil {
		t.Fatal(deadlineErr)
	}
	if !sawDeadline {
		t.Fatal("resume adapter was not called")
	}
}

func TestResumeSandboxTransitionsSuspendedToReady(t *testing.T) {
	adapter := &suspendableAdapter{}
	svc := newTestService(adapter)
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()
	if _, err := svc.SuspendSandbox(context.Background(), &cleanroomv1.SuspendSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("SuspendSandbox returned error: %v", err)
	}

	resp, err := svc.ResumeSandbox(context.Background(), &cleanroomv1.ResumeSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("ResumeSandbox returned error: %v", err)
	}
	if got, want := resp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY; got != want {
		t.Fatalf("unexpected resume response status: got %v want %v", got, want)
	}
	if got, want := adapter.resumeCalls, 1; got != want {
		t.Fatalf("unexpected resume call count: got %d want %d", got, want)
	}
	if adapter.resumeSandbox != sandboxID {
		t.Fatalf("unexpected resumed sandbox id: got %q want %q", adapter.resumeSandbox, sandboxID)
	}
}

func TestResumeSandboxReadyUnsupportedBackendIsNoop(t *testing.T) {
	svc := newTestService(&stubAdapter{})
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	resp, err := svc.ResumeSandbox(context.Background(), &cleanroomv1.ResumeSandboxRequest{
		SandboxId: createResp.GetSandbox().GetSandboxId(),
	})
	if err != nil {
		t.Fatalf("ResumeSandbox returned error: %v", err)
	}
	if got, want := resp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY; got != want {
		t.Fatalf("unexpected resume response status: got %v want %v", got, want)
	}
}

func TestResumeSandboxSuspendedUnsupportedBackendReturnsCapabilityError(t *testing.T) {
	svc := newTestService(&stubAdapter{})
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()

	svc.mu.Lock()
	svc.sandboxes[sandboxID].Status = cleanroomv1.SandboxStatus_SANDBOX_STATUS_SUSPENDED
	svc.mu.Unlock()

	_, err = svc.ResumeSandbox(context.Background(), &cleanroomv1.ResumeSandboxRequest{SandboxId: sandboxID})
	if err == nil {
		t.Fatal("expected ResumeSandbox to reject unsupported suspended backend")
	}
	if !strings.Contains(err.Error(), "does not support sandbox suspend") {
		t.Fatalf("unexpected ResumeSandbox error: %v", err)
	}
}

func TestResumeSandboxFailureMarksFailed(t *testing.T) {
	adapter := &suspendableAdapter{}
	svc := newTestService(adapter)
	policyProto := testPolicy()
	policyProto.Resources = &cleanroomv1.PolicyResources{
		Vcpus:       2,
		MemoryBytes: 2048 << 20,
		DiskBytes:   8 << 30,
	}
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: policyProto})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()
	if _, err := svc.SuspendSandbox(context.Background(), &cleanroomv1.SuspendSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("SuspendSandbox returned error: %v", err)
	}
	adapter.resumeFn = func(context.Context, string) error {
		return errors.New("resume failed")
	}

	_, err = svc.ResumeSandbox(context.Background(), &cleanroomv1.ResumeSandboxRequest{SandboxId: sandboxID})
	if err == nil {
		t.Fatal("expected ResumeSandbox to return backend error")
	}
	if !strings.Contains(err.Error(), "resume backend sandbox") {
		t.Fatalf("unexpected ResumeSandbox error: %v", err)
	}

	getResp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := getResp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_FAILED; got != want {
		t.Fatalf("unexpected sandbox status after failed resume: got %v want %v", got, want)
	}
	snapshots := svc.sandboxResourceMetricSnapshots(context.Background())
	if len(snapshots) != 1 {
		t.Fatalf("expected one resource metric snapshot, got %#v", snapshots)
	}
	if got, want := snapshots[0].Status, "failed"; got != want {
		t.Fatalf("unexpected resource metric status: got %q want %q", got, want)
	}
	if got, want := snapshots[0].Count, int64(1); got != want {
		t.Fatalf("unexpected resource metric count: got %d want %d", got, want)
	}
	if got, want := snapshots[0].EffectiveMemoryBytes, int64(2048<<20); got != want {
		t.Fatalf("unexpected resource metric memory: got %d want %d", got, want)
	}
}

func TestResumeSandboxIndeterminateFailureLeavesSuspended(t *testing.T) {
	adapter := &suspendableAdapter{}
	svc := newTestService(adapter)
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()
	if _, err := svc.SuspendSandbox(context.Background(), &cleanroomv1.SuspendSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("SuspendSandbox returned error: %v", err)
	}
	adapter.resumeFn = func(context.Context, string) error {
		return context.DeadlineExceeded
	}

	_, err = svc.ResumeSandbox(context.Background(), &cleanroomv1.ResumeSandboxRequest{SandboxId: sandboxID})
	if err == nil {
		t.Fatal("expected ResumeSandbox to return backend error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", err)
	}

	getResp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := getResp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_SUSPENDED; got != want {
		t.Fatalf("unexpected sandbox status after indeterminate resume: got %v want %v", got, want)
	}

	adapter.resumeFn = nil
	if _, err := svc.ResumeSandbox(context.Background(), &cleanroomv1.ResumeSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("second ResumeSandbox returned error: %v", err)
	}
	if got, want := adapter.resumeCalls, 2; got != want {
		t.Fatalf("unexpected resume call count: got %d want %d", got, want)
	}
}

func TestResumeSandboxBackendIndeterminateFailureLeavesSuspended(t *testing.T) {
	adapter := &suspendableAdapter{}
	svc := newTestService(adapter)
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()
	if _, err := svc.SuspendSandbox(context.Background(), &cleanroomv1.SuspendSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("SuspendSandbox returned error: %v", err)
	}
	adapter.resumeFn = func(context.Context, string) error {
		return fmt.Errorf("helper response lost: %w", backend.ErrSandboxLifecycleIndeterminate)
	}

	_, err = svc.ResumeSandbox(context.Background(), &cleanroomv1.ResumeSandboxRequest{SandboxId: sandboxID})
	if err == nil {
		t.Fatal("expected ResumeSandbox to return backend error")
	}
	if !errors.Is(err, backend.ErrSandboxLifecycleIndeterminate) {
		t.Fatalf("expected indeterminate lifecycle error, got %v", err)
	}

	getResp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := getResp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_SUSPENDED; got != want {
		t.Fatalf("unexpected sandbox status after indeterminate resume: got %v want %v", got, want)
	}
}

func TestCreateExecutionWakesSuspendedSandbox(t *testing.T) {
	adapter := &suspendableAdapter{}
	svc := newTestService(adapter)
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()
	if _, err := svc.SuspendSandbox(context.Background(), &cleanroomv1.SuspendSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("SuspendSandbox returned error: %v", err)
	}

	execResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"echo", "awake"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	waitExecutionDone(t, svc, sandboxID, execResp.GetExecution().GetExecutionId())

	if got, want := adapter.resumeCalls, 1; got != want {
		t.Fatalf("unexpected resume calls: got %d want %d", got, want)
	}
	if got, want := adapter.runCalls, 1; got != want {
		t.Fatalf("unexpected run calls: got %d want %d", got, want)
	}
	getResp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := getResp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY; got != want {
		t.Fatalf("unexpected sandbox status after transparent wake: got %v want %v", got, want)
	}
}

func TestReadSandboxFileWakesSuspendedSandbox(t *testing.T) {
	adapter := &suspendableAdapter{
		stubAdapter: stubAdapter{
			readFn: func(_ context.Context, _ string, path string, _ int64, emit func([]byte) error) error {
				if path != "/tmp/marker" {
					t.Fatalf("unexpected read path: got %q want /tmp/marker", path)
				}
				return emit([]byte("awake"))
			},
		},
	}
	svc := newTestService(adapter)
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()
	if _, err := svc.SuspendSandbox(context.Background(), &cleanroomv1.SuspendSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("SuspendSandbox returned error: %v", err)
	}

	var chunks [][]byte
	err = svc.ReadSandboxFile(context.Background(), &cleanroomv1.ReadSandboxFileRequest{
		SandboxId: sandboxID,
		Path:      "/tmp/marker",
	}, func(resp *cleanroomv1.ReadSandboxFileResponse) error {
		chunks = append(chunks, append([]byte(nil), resp.GetData()...))
		return nil
	})
	if err != nil {
		t.Fatalf("ReadSandboxFile returned error: %v", err)
	}
	if got, want := adapter.resumeCalls, 1; got != want {
		t.Fatalf("unexpected resume calls: got %d want %d", got, want)
	}
	if got, want := string(bytes.Join(chunks, nil)), "awake"; got != want {
		t.Fatalf("unexpected read data: got %q want %q", got, want)
	}
}

func TestCreateSnapshotWakesSuspendedSandbox(t *testing.T) {
	store := newMemorySnapshotStore()
	adapter := &suspendableAdapter{
		stubAdapter: stubAdapter{
			createSnapshotFn: func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
				return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
			},
		},
	}
	svc := newTestServiceWithSnapshotStore(adapter, store)
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()
	if _, err := svc.SuspendSandbox(context.Background(), &cleanroomv1.SuspendSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("SuspendSandbox returned error: %v", err)
	}

	snapshotResp, err := svc.CreateSnapshot(context.Background(), &cleanroomv1.CreateSnapshotRequest{
		SandboxId: sandboxID,
		Name:      "after-wake",
	})
	if err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}

	if snapshotResp.GetSnapshot() == nil {
		t.Fatal("expected snapshot in response")
	}
	if got, want := adapter.resumeCalls, 1; got != want {
		t.Fatalf("unexpected resume calls: got %d want %d", got, want)
	}
	if got, want := adapter.createSnapshotCalls, 1; got != want {
		t.Fatalf("unexpected snapshot calls: got %d want %d", got, want)
	}
	getResp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := getResp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY; got != want {
		t.Fatalf("unexpected sandbox status after snapshot: got %v want %v", got, want)
	}
}

func TestDialSandboxPortWakesSuspendedSandbox(t *testing.T) {
	adapter := &suspendablePortDialAdapter{
		dialFn: func(context.Context, string, int) (net.Conn, error) {
			serverConn, clientConn := net.Pipe()
			_ = serverConn.Close()
			return clientConn, nil
		},
	}
	svc := newTestService(adapter)
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()
	if _, err := svc.SuspendSandbox(context.Background(), &cleanroomv1.SuspendSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("SuspendSandbox returned error: %v", err)
	}

	conn, err := svc.DialSandboxPort(context.Background(), &cleanroomv1.SandboxPortOpen{
		SandboxId: sandboxID,
		GuestPort: 3000,
	})
	if err != nil {
		t.Fatalf("DialSandboxPort returned error: %v", err)
	}
	_ = conn.Close()
	if got, want := adapter.resumeCalls, 1; got != want {
		t.Fatalf("unexpected resume calls: got %d want %d", got, want)
	}
	if got, want := adapter.dialCalls, 1; got != want {
		t.Fatalf("unexpected dial calls: got %d want %d", got, want)
	}
}

func TestDialSandboxPortPreservesCloseWrite(t *testing.T) {
	var dialedConn *closeWriteTrackingConn
	adapter := &suspendablePortDialAdapter{
		dialFn: func(context.Context, string, int) (net.Conn, error) {
			serverConn, clientConn := net.Pipe()
			t.Cleanup(func() { _ = serverConn.Close() })
			dialedConn = &closeWriteTrackingConn{Conn: clientConn}
			return dialedConn, nil
		},
	}
	svc := newTestService(adapter)
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	conn, err := svc.DialSandboxPort(context.Background(), &cleanroomv1.SandboxPortOpen{
		SandboxId: createResp.GetSandbox().GetSandboxId(),
		GuestPort: 3000,
	})
	if err != nil {
		t.Fatalf("DialSandboxPort returned error: %v", err)
	}
	defer conn.Close()

	closeWriter, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("expected sandbox port connection to preserve CloseWrite")
	}
	if err := closeWriter.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite returned error: %v", err)
	}
	if got, want := dialedConn.closeWriteCalls, 1; got != want {
		t.Fatalf("unexpected CloseWrite calls: got %d want %d", got, want)
	}
}

func TestDialSandboxPortHoldsGuestInteractionLease(t *testing.T) {
	var serverConn net.Conn
	adapter := &suspendablePortDialAdapter{
		dialFn: func(context.Context, string, int) (net.Conn, error) {
			server, client := net.Pipe()
			serverConn = server
			return client, nil
		},
	}
	svc := newTestService(adapter)
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()

	conn, err := svc.DialSandboxPort(context.Background(), &cleanroomv1.SandboxPortOpen{
		SandboxId: sandboxID,
		GuestPort: 3000,
	})
	if err != nil {
		t.Fatalf("DialSandboxPort returned error: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		if serverConn != nil {
			_ = serverConn.Close()
		}
	})

	svc.mu.RLock()
	activeInteractions := svc.sandboxes[sandboxID].GuestInteractionCount
	svc.mu.RUnlock()
	if got, want := activeInteractions, 1; got != want {
		t.Fatalf("unexpected active guest interactions: got %d want %d", got, want)
	}
	_, err = svc.SuspendSandbox(context.Background(), &cleanroomv1.SuspendSandboxRequest{SandboxId: sandboxID})
	if err == nil {
		t.Fatal("expected SuspendSandbox to reject active guest interaction")
	}
	if !strings.Contains(err.Error(), "active guest interactions") {
		t.Fatalf("unexpected SuspendSandbox error: %v", err)
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("port connection close returned error: %v", err)
	}
	svc.mu.RLock()
	activeInteractions = svc.sandboxes[sandboxID].GuestInteractionCount
	svc.mu.RUnlock()
	if got, want := activeInteractions, 0; got != want {
		t.Fatalf("unexpected active guest interactions after close: got %d want %d", got, want)
	}
}

type closeWriteTrackingConn struct {
	net.Conn
	closeWriteCalls int
}

func (c *closeWriteTrackingConn) CloseWrite() error {
	c.closeWriteCalls++
	return nil
}

func TestDialSandboxPortUnsupportedBackendDoesNotWakeSuspendedSandbox(t *testing.T) {
	adapter := &suspendableAdapter{
		resumeFn: func(context.Context, string) error {
			return errors.New("unexpected wake")
		},
	}
	svc := newTestService(adapter)
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()
	if _, err := svc.SuspendSandbox(context.Background(), &cleanroomv1.SuspendSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("SuspendSandbox returned error: %v", err)
	}

	_, err = svc.DialSandboxPort(context.Background(), &cleanroomv1.SandboxPortOpen{
		SandboxId: sandboxID,
		GuestPort: 3000,
	})
	if err == nil {
		t.Fatal("expected DialSandboxPort to reject unsupported backend")
	}
	if !strings.Contains(err.Error(), "does not support sandbox port dialing") {
		t.Fatalf("unexpected DialSandboxPort error: %v", err)
	}
	if got, want := adapter.resumeCalls, 0; got != want {
		t.Fatalf("unexpected resume calls: got %d want %d", got, want)
	}
	getResp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := getResp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_SUSPENDED; got != want {
		t.Fatalf("unexpected sandbox status after rejected port dial: got %v want %v", got, want)
	}
}

func TestConcurrentWakeAttemptsCoalesce(t *testing.T) {
	resumeStarted := make(chan struct{})
	releaseResume := make(chan struct{})
	var resumeStartedOnce sync.Once
	adapter := &suspendablePortDialAdapter{}
	adapter.resumeFn = func(context.Context, string) error {
		resumeStartedOnce.Do(func() { close(resumeStarted) })
		<-releaseResume
		return nil
	}
	adapter.dialFn = func(context.Context, string, int) (net.Conn, error) {
		serverConn, clientConn := net.Pipe()
		_ = serverConn.Close()
		return clientConn, nil
	}
	svc := newTestService(adapter)
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()
	if _, err := svc.SuspendSandbox(context.Background(), &cleanroomv1.SuspendSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("SuspendSandbox returned error: %v", err)
	}

	const callers = 5
	start := make(chan struct{})
	errCh := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() {
			<-start
			conn, err := svc.DialSandboxPort(context.Background(), &cleanroomv1.SandboxPortOpen{
				SandboxId: sandboxID,
				GuestPort: 3000,
			})
			if conn != nil {
				_ = conn.Close()
			}
			errCh <- err
		}()
	}
	close(start)
	select {
	case <-resumeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for resume to start")
	}
	time.Sleep(50 * time.Millisecond)
	close(releaseResume)
	for i := 0; i < callers; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("DialSandboxPort caller %d returned error: %v", i+1, err)
		}
	}
	if got, want := adapter.resumeCalls, 1; got != want {
		t.Fatalf("unexpected resume calls: got %d want %d", got, want)
	}
	if got, want := adapter.dialCalls, callers; got != want {
		t.Fatalf("unexpected dial calls: got %d want %d", got, want)
	}
}

func TestConcurrentWakeIgnoresFirstCallerCancellation(t *testing.T) {
	resumeStarted := make(chan struct{})
	releaseResume := make(chan struct{})
	resumeContextErr := make(chan error, 2)
	var resumeStartedOnce sync.Once
	adapter := &suspendablePortDialAdapter{}
	adapter.resumeFn = func(ctx context.Context, _ string) error {
		resumeStartedOnce.Do(func() { close(resumeStarted) })
		select {
		case <-releaseResume:
			resumeContextErr <- ctx.Err()
			return nil
		case <-ctx.Done():
			resumeContextErr <- ctx.Err()
			return ctx.Err()
		}
	}
	adapter.dialFn = func(context.Context, string, int) (net.Conn, error) {
		serverConn, clientConn := net.Pipe()
		_ = serverConn.Close()
		return clientConn, nil
	}
	svc := newTestService(adapter)
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()
	if _, err := svc.SuspendSandbox(context.Background(), &cleanroomv1.SuspendSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("SuspendSandbox returned error: %v", err)
	}

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstErr := make(chan error, 1)
	go func() {
		conn, err := svc.DialSandboxPort(firstCtx, &cleanroomv1.SandboxPortOpen{
			SandboxId: sandboxID,
			GuestPort: 3000,
		})
		if conn != nil {
			_ = conn.Close()
		}
		firstErr <- err
	}()
	select {
	case <-resumeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for resume to start")
	}

	secondErr := make(chan error, 1)
	go func() {
		conn, err := svc.DialSandboxPort(context.Background(), &cleanroomv1.SandboxPortOpen{
			SandboxId: sandboxID,
			GuestPort: 3000,
		})
		if conn != nil {
			_ = conn.Close()
		}
		secondErr <- err
	}()
	select {
	case err := <-secondErr:
		t.Fatalf("second caller returned before resume completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancelFirst()
	select {
	case err := <-firstErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected first caller cancellation, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first caller cancellation")
	}
	select {
	case err := <-secondErr:
		t.Fatalf("second caller returned after first cancellation but before resume completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseResume)
	select {
	case err := <-secondErr:
		if err != nil {
			t.Fatalf("second DialSandboxPort returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for second caller")
	}
	if got, want := adapter.resumeCalls, 1; got != want {
		t.Fatalf("unexpected resume calls: got %d want %d", got, want)
	}
	select {
	case err := <-resumeContextErr:
		if err != nil {
			t.Fatalf("backend resume context was canceled: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for resume context result")
	}
}

func TestTerminateSandboxStopsSuspendedSandbox(t *testing.T) {
	adapter := &suspendableAdapter{}
	svc := newTestService(adapter)
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()
	if _, err := svc.SuspendSandbox(context.Background(), &cleanroomv1.SuspendSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("SuspendSandbox returned error: %v", err)
	}

	if _, err := svc.TerminateSandbox(context.Background(), &cleanroomv1.TerminateSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("TerminateSandbox returned error: %v", err)
	}
	getResp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := getResp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED; got != want {
		t.Fatalf("unexpected sandbox status after terminate: got %v want %v", got, want)
	}
	if got, want := adapter.terminateCalls, 1; got != want {
		t.Fatalf("unexpected terminate call count: got %d want %d", got, want)
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

func requireInt64GaugeMetricValue(t *testing.T, metrics metricdata.ResourceMetrics, name string, attrs map[string]string, want int64) {
	t.Helper()
	for _, scopeMetrics := range metrics.ScopeMetrics {
		for _, metric := range scopeMetrics.Metrics {
			if metric.Name != name {
				continue
			}
			gauge, ok := metric.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("metric %q had unexpected data type %T", name, metric.Data)
			}
			for _, point := range gauge.DataPoints {
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
				return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 1, LaunchedVM: true, Message: strings.TrimSpace(message)}, nil
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

func TestServiceEmitsSandboxLifecycleMetrics(t *testing.T) {
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

	adapter := &suspendableAdapter{}
	svc := newTestService(adapter)
	svc.Observability = obs

	sandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := sandboxResp.GetSandbox().GetSandboxId()

	if _, err := svc.SuspendSandbox(context.Background(), &cleanroomv1.SuspendSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("SuspendSandbox returned error: %v", err)
	}
	if _, err := svc.ResumeSandbox(context.Background(), &cleanroomv1.ResumeSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("ResumeSandbox returned error: %v", err)
	}

	adapter.suspendFn = func(context.Context, string) error {
		return errors.New("pause failed")
	}
	if _, err := svc.SuspendSandbox(context.Background(), &cleanroomv1.SuspendSandboxRequest{SandboxId: sandboxID}); err == nil {
		t.Fatal("expected failed suspend")
	}

	adapter.suspendFn = nil
	if _, err := svc.SuspendSandbox(context.Background(), &cleanroomv1.SuspendSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("second SuspendSandbox returned error: %v", err)
	}
	adapter.resumeFn = func(context.Context, string) error {
		return errors.New("resume failed")
	}
	if _, err := svc.ResumeSandbox(context.Background(), &cleanroomv1.ResumeSandboxRequest{SandboxId: sandboxID}); err == nil {
		t.Fatal("expected failed resume")
	}

	attrs := map[string]string{
		observability.MetricLabelBackend: "firecracker",
		observability.MetricLabelOutcome: observability.OutcomeSucceeded,
	}
	metrics := collectResourceMetrics(t, reader)
	requireHistogramMetricCount(t, metrics, observability.MetricSandboxSuspendDurationSeconds, attrs, 2)
	requireHistogramMetricCount(t, metrics, observability.MetricSandboxWakeDurationSeconds, attrs, 1)
	requireHistogramMetricCount(t, metrics, observability.MetricSandboxSuspendDurationSeconds, map[string]string{
		observability.MetricLabelBackend: "firecracker",
		observability.MetricLabelOutcome: observability.OutcomeFailed,
	}, 1)
	requireHistogramMetricCount(t, metrics, observability.MetricSandboxWakeDurationSeconds, map[string]string{
		observability.MetricLabelBackend: "firecracker",
		observability.MetricLabelOutcome: observability.OutcomeFailed,
	}, 1)
}

func TestSandboxLifecycleOutcomeMetricValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "success", want: observability.OutcomeSucceeded},
		{name: "canceled", err: context.Canceled, want: observability.OutcomeCanceled},
		{name: "timed out", err: context.DeadlineExceeded, want: observability.OutcomeTimedOut},
		{name: "indeterminate", err: fmt.Errorf("helper response lost: %w", backend.ErrSandboxLifecycleIndeterminate), want: observability.OutcomeIndeterminate},
		{name: "failed", err: errors.New("pause failed"), want: observability.OutcomeFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sandboxLifecycleOutcomeMetricValue(tc.err); got != tc.want {
				t.Fatalf("sandboxLifecycleOutcomeMetricValue(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestServiceEmitsSandboxResourceMetrics(t *testing.T) {
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

	svc := newTestService(&stubAdapter{})
	svc.Observability = obs

	policyProto := testPolicy()
	policyProto.Resources = &cleanroomv1.PolicyResources{
		Vcpus:       4,
		MemoryBytes: (3 << 30) + 1,
		DiskBytes:   16 << 30,
	}
	if _, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Backend: "firecracker",
		Policy:  policyProto,
	}); err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	attrs := map[string]string{
		observability.MetricLabelBackend: "firecracker",
		observability.MetricLabelStatus:  "ready",
	}
	metrics := collectResourceMetrics(t, reader)
	requireInt64GaugeMetricValue(t, metrics, observability.MetricSandboxActiveCount, attrs, 1)
	requireInt64GaugeMetricValue(t, metrics, observability.MetricSandboxEffectiveVCPUs, attrs, 4)
	requireInt64GaugeMetricValue(t, metrics, observability.MetricSandboxEffectiveMemoryBytes, attrs, 3073<<20)
	requireInt64GaugeMetricValue(t, metrics, observability.MetricSandboxEffectiveDiskBytes, attrs, 16<<30)
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
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			runCalls++
			result := &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, LaunchedVM: true, Message: "ok"}
			if runCalls == 3 {
				result.ExitCode = 1
				result.Message = "dependency bootstrap failed"
				if stream.OnStderr != nil {
					stream.OnStderr([]byte("dependency bootstrap failed\n"))
				}
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

func TestSandboxCreateSourceMetricValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		snapshotID string
		sourceKind string
		want       string
	}{
		{name: "fresh", want: "fresh"},
		{name: "snapshot source kind", sourceKind: "snapshot", want: "snapshot"},
		{name: "snapshot id fallback", snapshotID: "snap-1", want: "snapshot"},
		{name: "workspace cache", sourceKind: "workspace stage cache", want: "workspace_cache"},
		{name: "dependency cache", sourceKind: "dependency stage cache", want: "dependency_cache"},
		{name: "portable dependency cache", sourceKind: "portable dependency stage cache", want: "dependency_cache"},
		{name: "services cache", sourceKind: "services stage cache", want: "services_cache"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := sandboxCreateSourceMetricValue(tt.snapshotID, tt.sourceKind); got != tt.want {
				t.Fatalf("sandboxCreateSourceMetricValue(%q, %q) = %q, want %q", tt.snapshotID, tt.sourceKind, got, tt.want)
			}
		})
	}
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

func TestCreateSandboxLoadsPolicyFromRepositoryCheckout(t *testing.T) {
	adapter := &stubAdapter{}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"cleanroom.yaml": testRepositoryPolicyYAML("/repo", true, true),
	})
	wantCommit := repositoryCheckout.GetCommitSha()
	repositoryCheckout.CommitSha = ""
	repositoryCheckout.DestinationDir = ""

	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors

	resp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		RepositoryCheckout: repositoryCheckout,
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if got, want := resp.GetPolicySource(), "repository:cleanroom.yaml"; got != want {
		t.Fatalf("unexpected policy source: got %q want %q", got, want)
	}
	repository := resp.GetSandbox().GetRepositoryCheckout()
	if repository == nil {
		t.Fatal("expected repository checkout on sandbox")
	}
	if got := repository.GetCommitSha(); got != wantCommit {
		t.Fatalf("unexpected resolved commit: got %q want %q", got, wantCommit)
	}
	if got := repository.GetDestinationDir(); got != "/repo" {
		t.Fatalf("unexpected destination dir: got %q", got)
	}
	if !repository.GetSubmodules() {
		t.Fatal("expected repository submodules to come from policy")
	}
	if adapter.provisionCalls != 1 {
		t.Fatalf("expected one provision call, got %d", adapter.provisionCalls)
	}
}

func TestCreateSandboxLoadsFallbackPolicyFromRepositoryCheckout(t *testing.T) {
	adapter := &stubAdapter{}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		".buildkite/cleanroom.yaml": testRepositoryPolicyYAML("/workspace", false, true),
	})
	repositoryCheckout.CommitSha = ""
	repositoryCheckout.DestinationDir = ""

	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors

	resp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		RepositoryCheckout: repositoryCheckout,
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if got, want := resp.GetPolicySource(), "repository:.buildkite/cleanroom.yaml"; got != want {
		t.Fatalf("unexpected policy source: got %q want %q", got, want)
	}
}

func TestCreateSandboxRepositoryPolicyResolvesBranch(t *testing.T) {
	adapter := &stubAdapter{}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"cleanroom.yaml": testRepositoryPolicyYAML("/main", false, true),
	})
	runTestGit(t, mirrors.mirrorPath, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(mirrors.mirrorPath, "cleanroom.yaml"), []byte(testRepositoryPolicyYAML("/feature", false, true)), 0o644); err != nil {
		t.Fatalf("write branch policy: %v", err)
	}
	runTestGit(t, mirrors.mirrorPath, "add", "cleanroom.yaml")
	runTestGit(t, mirrors.mirrorPath, "commit", "-m", "feature policy")
	wantCommit := strings.TrimSpace(runTestGit(t, mirrors.mirrorPath, "rev-parse", "HEAD"))
	repositoryCheckout.CommitSha = ""
	repositoryCheckout.Branch = "feature"
	repositoryCheckout.DestinationDir = ""

	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors

	resp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		RepositoryCheckout: repositoryCheckout,
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	repository := resp.GetSandbox().GetRepositoryCheckout()
	if got := repository.GetCommitSha(); got != wantCommit {
		t.Fatalf("unexpected resolved commit: got %q want %q", got, wantCommit)
	}
	if got := repository.GetBranch(); got != "feature" {
		t.Fatalf("unexpected branch: got %q", got)
	}
	if got := repository.GetDestinationDir(); got != "/feature" {
		t.Fatalf("unexpected destination dir: got %q", got)
	}
	if got := mirrors.refreshCalls; got != 1 {
		t.Fatalf("expected one mirror refresh before resolving branch, got %d", got)
	}
}

func TestCreateSandboxRepositoryPolicyRejectsCommitOutsideBranch(t *testing.T) {
	adapter := &stubAdapter{}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"cleanroom.yaml": testRepositoryPolicyYAML("/main", false, true),
	})
	baseBranch := strings.TrimSpace(runTestGit(t, mirrors.mirrorPath, "rev-parse", "--abbrev-ref", "HEAD"))
	runTestGit(t, mirrors.mirrorPath, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(mirrors.mirrorPath, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatalf("write feature file: %v", err)
	}
	runTestGit(t, mirrors.mirrorPath, "add", "feature.txt")
	runTestGit(t, mirrors.mirrorPath, "commit", "-m", "feature commit")
	featureCommit := strings.TrimSpace(runTestGit(t, mirrors.mirrorPath, "rev-parse", "HEAD"))
	repositoryCheckout.CommitSha = featureCommit
	repositoryCheckout.Branch = baseBranch

	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors

	_, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		RepositoryCheckout: repositoryCheckout,
	})
	if err == nil || !strings.Contains(err.Error(), "is not reachable from branch") {
		t.Fatalf("CreateSandbox error = %v, want commit outside branch error", err)
	}
	if got := adapter.provisionCalls; got != 0 {
		t.Fatalf("ProvisionSandbox calls = %d, want 0", got)
	}
}

func TestCreateSandboxLoadsRepositoryPolicyFromCommitBundle(t *testing.T) {
	adapter := &stubAdapter{}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"README.md":      "hello\n",
		"cleanroom.yaml": testRepositoryPolicyYAML("/base", false, true),
	})
	baseCommit := repositoryCheckout.GetCommitSha()

	localRepo := filepath.Join(t.TempDir(), "local")
	runTestGit(t, t.TempDir(), "clone", mirrors.mirrorPath, localRepo)
	runTestGit(t, localRepo, "config", "user.email", "test@example.com")
	runTestGit(t, localRepo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(localRepo, "cleanroom.yaml"), []byte(testRepositoryPolicyYAML("/bundle", true, true)), 0o644); err != nil {
		t.Fatalf("WriteFile(cleanroom.yaml) returned error: %v", err)
	}
	runTestGit(t, localRepo, "add", "cleanroom.yaml")
	runTestGit(t, localRepo, "commit", "-m", "local policy")
	localCommit := strings.TrimSpace(runTestGit(t, localRepo, "rev-parse", "HEAD"))

	commitBundle, err := repositorybundle.BuildFromRepository(localRepo, "origin", &repositorycheckout.Checkout{CommitSHA: localCommit})
	if err != nil {
		t.Fatalf("BuildFromRepository returned error: %v", err)
	}
	if commitBundle == nil {
		t.Fatal("expected repository commit bundle")
	}
	repositoryCheckout.CommitSha = localCommit
	repositoryCheckout.DestinationDir = ""
	mirrors.ensureContainsFn = func(_ string, commitSHA string) error {
		switch strings.TrimSpace(commitSHA) {
		case baseCommit:
			return nil
		case localCommit:
			return errors.New("local-only commit should be supplied by commit bundle")
		default:
			return fmt.Errorf("unexpected mirror commit %q", commitSHA)
		}
	}

	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors

	resp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		RepositoryCheckout:     repositoryCheckout,
		RepositoryCommitBundle: commitBundle.ToProto(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if got, want := resp.GetPolicySource(), "repository:cleanroom.yaml"; got != want {
		t.Fatalf("unexpected policy source: got %q want %q", got, want)
	}
	repository := resp.GetSandbox().GetRepositoryCheckout()
	if repository == nil {
		t.Fatal("expected repository checkout on sandbox")
	}
	if got := repository.GetCommitSha(); got != localCommit {
		t.Fatalf("unexpected resolved commit: got %q want %q", got, localCommit)
	}
	if got := repository.GetDestinationDir(); got != "/bundle" {
		t.Fatalf("unexpected destination dir: got %q", got)
	}
	if !repository.GetSubmodules() {
		t.Fatal("expected repository submodules to come from bundled policy")
	}
	if !slices.Contains(mirrors.commitSHAs, baseCommit) {
		t.Fatalf("expected bundle prerequisite %q to be ensured, got %v", baseCommit, mirrors.commitSHAs)
	}
	if slices.Contains(mirrors.commitSHAs, localCommit) {
		t.Fatalf("expected local-only commit %q to avoid direct mirror ensure, got %v", localCommit, mirrors.commitSHAs)
	}
}

func TestCreateSandboxLoadsDisabledRepositoryPolicyFromCommitBundle(t *testing.T) {
	adapter := &stubAdapter{}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"README.md":      "hello\n",
		"cleanroom.yaml": testRepositoryPolicyYAML("/base", false, true),
	})
	baseCommit := repositoryCheckout.GetCommitSha()

	localRepo := filepath.Join(t.TempDir(), "local")
	runTestGit(t, t.TempDir(), "clone", mirrors.mirrorPath, localRepo)
	runTestGit(t, localRepo, "config", "user.email", "test@example.com")
	runTestGit(t, localRepo, "config", "user.name", "Test User")
	disabledPolicy := strings.ReplaceAll(testRepositoryPolicyYAML("/bundle", false, true), "  path: /bundle\n  submodules: false", "  enabled: false")
	if err := os.WriteFile(filepath.Join(localRepo, "cleanroom.yaml"), []byte(disabledPolicy), 0o644); err != nil {
		t.Fatalf("WriteFile(cleanroom.yaml) returned error: %v", err)
	}
	runTestGit(t, localRepo, "add", "cleanroom.yaml")
	runTestGit(t, localRepo, "commit", "-m", "disable checkout")
	localCommit := strings.TrimSpace(runTestGit(t, localRepo, "rev-parse", "HEAD"))

	commitBundle, err := repositorybundle.BuildFromRepository(localRepo, "origin", &repositorycheckout.Checkout{CommitSHA: localCommit})
	if err != nil {
		t.Fatalf("BuildFromRepository returned error: %v", err)
	}
	if commitBundle == nil {
		t.Fatal("expected repository commit bundle")
	}
	repositoryCheckout.CommitSha = localCommit
	repositoryCheckout.DestinationDir = ""
	mirrors.ensureContainsFn = func(_ string, commitSHA string) error {
		if strings.TrimSpace(commitSHA) != baseCommit {
			return fmt.Errorf("unexpected mirror commit %q", commitSHA)
		}
		return nil
	}

	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors

	resp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		RepositoryCheckout:     repositoryCheckout,
		RepositoryCommitBundle: commitBundle.ToProto(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if got, want := resp.GetPolicySource(), "repository:cleanroom.yaml"; got != want {
		t.Fatalf("unexpected policy source: got %q want %q", got, want)
	}
	if repository := resp.GetSandbox().GetRepositoryCheckout(); repository != nil {
		t.Fatalf("expected repository bootstrap to be disabled, got %#v", repository)
	}
	if got := adapter.runCalls; got != 0 {
		t.Fatalf("expected no repository bootstrap execution, got %d", got)
	}
}

func TestCreateSandboxRepositoryPolicyHonorsDisabledRepositoryBootstrap(t *testing.T) {
	adapter := &stubAdapter{}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"cleanroom.yaml": strings.ReplaceAll(testRepositoryPolicyYAML("/workspace", false, true), "  path: /workspace\n  submodules: false", "  enabled: false"),
	})
	repositoryCheckout.CommitSha = ""
	repositoryCheckout.DestinationDir = ""

	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors

	resp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		RepositoryCheckout: repositoryCheckout,
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if repository := resp.GetSandbox().GetRepositoryCheckout(); repository != nil {
		t.Fatalf("expected repository bootstrap to be disabled, got %#v", repository)
	}
	if got := adapter.runCalls; got != 0 {
		t.Fatalf("expected no repository bootstrap execution, got %d", got)
	}
}

func TestCreateSandboxRepositoryPolicyRequiresRepositoryStore(t *testing.T) {
	svc := newTestService(&stubAdapter{})

	_, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		RepositoryCheckout: &cleanroomv1.RepositoryCheckout{
			RemoteUrl: "https://github.com/buildkite/cleanroom.git",
		},
	})
	if err == nil {
		t.Fatal("expected CreateSandbox to fail")
	}
	if !strings.Contains(err.Error(), "requires repository store") {
		t.Fatalf("expected repository store error, got %v", err)
	}
}

func TestCreateSandboxRepositoryPolicyPreservesRepositoryAccessError(t *testing.T) {
	adapter := &stubAdapter{}
	mirrors := &stubRepositoryMirrorStore{
		mirrorPathErr:   errors.New("no cached mirror"),
		ensureMirrorErr: errors.New("repository https://github.com/buildkite/missing.git does not exist"),
	}
	repositoryCheckout := testRepositoryCheckoutProto()
	repositoryCheckout.RemoteUrl = "https://github.com/buildkite/missing.git"

	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors

	_, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		RepositoryCheckout: repositoryCheckout,
	})
	if err == nil {
		t.Fatal("expected CreateSandbox to fail")
	}
	if !strings.Contains(err.Error(), "repository https://github.com/buildkite/missing.git does not exist") {
		t.Fatalf("expected repository access error, got %v", err)
	}
	if strings.Contains(err.Error(), "policy not found") {
		t.Fatalf("expected repository access error to be preserved, got %v", err)
	}
}

func TestCreateSandboxRepositoryPolicyRejectsDisallowedRemote(t *testing.T) {
	adapter := &stubAdapter{}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"cleanroom.yaml": testRepositoryPolicyYAML("/workspace", false, false),
	})
	repositoryCheckout.CommitSha = ""

	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors

	_, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		RepositoryCheckout: repositoryCheckout,
	})
	if err == nil {
		t.Fatal("expected CreateSandbox to fail")
	}
	if !strings.Contains(err.Error(), `repository remote host "github.com" is not allowed`) {
		t.Fatalf("expected host allowlist error, got %v", err)
	}
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
	key := cacheStoreOwnerKey(record.Stage, record.CacheKey, record.OwnerPrincipalID)
	if _, exists := s.records[key]; exists {
		return errors.New("cache record already exists")
	}
	s.records[key] = record
	return nil
}

func (s *memoryCacheStore) Upsert(_ context.Context, record cachestore.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[cacheStoreOwnerKey(record.Stage, record.CacheKey, record.OwnerPrincipalID)] = record
	return nil
}

func (s *memoryCacheStore) GetReady(_ context.Context, stage, cacheKey string) (cachestore.Record, bool, error) {
	return s.getReadyForOwner(stage, cacheKey, "")
}

func (s *memoryCacheStore) GetReadyForOwner(_ context.Context, stage, cacheKey, ownerPrincipalID string) (cachestore.Record, bool, error) {
	ownerPrincipalID = strings.TrimSpace(ownerPrincipalID)
	if ownerPrincipalID == "" {
		return cachestore.Record{}, false, nil
	}
	return s.getReadyForOwner(stage, cacheKey, ownerPrincipalID)
}

func (s *memoryCacheStore) getReadyForOwner(stage, cacheKey, ownerPrincipalID string) (cachestore.Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[cacheStoreOwnerKey(stage, cacheKey, ownerPrincipalID)]
	if !ok || record.State != cacheStateReady {
		return cachestore.Record{}, false, nil
	}
	return record, true, nil
}

func (s *memoryCacheStore) Touch(_ context.Context, stage, cacheKey string) error {
	return s.touchForOwner(stage, cacheKey, "")
}

func (s *memoryCacheStore) TouchForOwner(_ context.Context, stage, cacheKey, ownerPrincipalID string) error {
	ownerPrincipalID = strings.TrimSpace(ownerPrincipalID)
	if ownerPrincipalID == "" {
		return errors.New("cache record missing owner")
	}
	return s.touchForOwner(stage, cacheKey, ownerPrincipalID)
}

func (s *memoryCacheStore) touchForOwner(stage, cacheKey, ownerPrincipalID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := cacheStoreOwnerKey(stage, cacheKey, ownerPrincipalID)
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
	for key, record := range s.records {
		if strings.TrimSpace(record.Stage) == strings.TrimSpace(stage) && strings.TrimSpace(record.CacheKey) == strings.TrimSpace(cacheKey) {
			delete(s.records, key)
		}
	}
	return nil
}

func (s *memoryCacheStore) DeleteForOwner(_ context.Context, stage, cacheKey, ownerPrincipalID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, cacheStoreOwnerKey(stage, cacheKey, ownerPrincipalID))
	return nil
}

type memoryZFSImportDatasetStore struct {
	mu        sync.Mutex
	datasets  []string
	destroyed []string
}

func (s *memoryZFSImportDatasetStore) ListZFSImportDatasets(context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.datasets...), nil
}

func (s *memoryZFSImportDatasetStore) DestroyZFSImportDataset(_ context.Context, dataset string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.destroyed = append(s.destroyed, dataset)
	return nil
}

type memoryChangesetStore struct {
	mu      sync.Mutex
	records map[string]changesetstore.Record
}

func newMemoryChangesetStore() *memoryChangesetStore {
	return &memoryChangesetStore{records: map[string]changesetstore.Record{}}
}

func (s *memoryChangesetStore) Put(_ context.Context, repository *repositorycheckout.Checkout, changeset *repositorychangeset.Changeset) (changesetstore.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	record := changesetstore.Record{
		ChangesetID:        changesetstore.RecordID(repository, changeset),
		CanonicalRemoteURL: strings.TrimSpace(repository.RemoteURL),
		BaseCommitSHA:      strings.TrimSpace(changeset.BaseCommitSHA),
		SubmoduleMode: func() string {
			if repository.Submodules {
				return "enabled"
			}
			return "disabled"
		}(),
		ChangesetDigest: strings.TrimSpace(changeset.Digest),
		FinalTreeDigest: strings.TrimSpace(changeset.TreeDigest),
		TransportFormat: changesetstore.TransportFormatProtoV1,
		TransportRef:    "memory",
		PayloadDigest:   "memory",
		CreatedAt:       now,
		LastUsedAt:      now,
	}
	if record.ChangesetID == "" {
		return changesetstore.Record{}, errors.New("missing changeset id")
	}
	s.records[record.ChangesetID] = record
	return record, nil
}

func cacheStoreKey(stage, cacheKey string) string {
	return strings.TrimSpace(stage) + "\x00" + strings.TrimSpace(cacheKey)
}

func cacheStoreOwnerKey(stage, cacheKey, ownerPrincipalID string) string {
	return cacheStoreKey(stage, cacheKey) + "\x00" + strings.TrimSpace(ownerPrincipalID)
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
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("hello stdout\n"))
			}
			if stream.OnStderr != nil {
				stream.OnStderr([]byte("hello stderr\n"))
			}
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    7,
				LaunchedVM:  true,
				PlanPath:    "/tmp/plan",
				RunDir:      "/tmp/run",
				ImageRef:    "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				ImageDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
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

func TestCreateSnapshotUnsupportedBackendDoesNotWakeSuspendedSandbox(t *testing.T) {
	store := newMemorySnapshotStore()
	adapter := &suspendOnlyAdapter{
		resumeFn: func(context.Context, string) error {
			return errors.New("unexpected wake")
		},
	}
	svc := newTestServiceWithSnapshotStore(adapter, store)

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()
	if _, err := svc.SuspendSandbox(context.Background(), &cleanroomv1.SuspendSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("SuspendSandbox returned error: %v", err)
	}

	_, err = svc.CreateSnapshot(context.Background(), &cleanroomv1.CreateSnapshotRequest{SandboxId: sandboxID})
	if err == nil {
		t.Fatal("expected CreateSnapshot to reject unsupported backend")
	}
	if !strings.Contains(err.Error(), "does not support snapshots") {
		t.Fatalf("unexpected CreateSnapshot error: %v", err)
	}
	if got, want := adapter.resumeCalls, 0; got != want {
		t.Fatalf("unexpected resume calls: got %d want %d", got, want)
	}
	getResp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := getResp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_SUSPENDED; got != want {
		t.Fatalf("unexpected sandbox status: got %v want %v", got, want)
	}
}

func TestCreateSnapshotDisabledBackendDoesNotWakeSuspendedSandbox(t *testing.T) {
	store := newMemorySnapshotStore()
	adapter := &suspendableAdapter{
		resumeFn: func(context.Context, string) error {
			return errors.New("unexpected wake")
		},
	}
	svc := newTestServiceWithSnapshotStore(adapter, store)
	svc.Config.Backends.Firecracker.Snapshots.Enabled = false

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()
	if _, err := svc.SuspendSandbox(context.Background(), &cleanroomv1.SuspendSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("SuspendSandbox returned error: %v", err)
	}

	_, err = svc.CreateSnapshot(context.Background(), &cleanroomv1.CreateSnapshotRequest{SandboxId: sandboxID})
	if err == nil {
		t.Fatal("expected CreateSnapshot to reject disabled snapshots")
	}
	if !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("unexpected CreateSnapshot error: %v", err)
	}
	if got, want := adapter.resumeCalls, 0; got != want {
		t.Fatalf("unexpected resume calls: got %d want %d", got, want)
	}
	if got, want := adapter.createSnapshotCalls, 0; got != want {
		t.Fatalf("unexpected snapshot calls: got %d want %d", got, want)
	}
	getResp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := getResp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_SUSPENDED; got != want {
		t.Fatalf("unexpected sandbox status: got %v want %v", got, want)
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
			return &backend.SnapshotResult{
				StorageRef:         "/snapshots/" + req.SnapshotID + ".ext4",
				StorageSizeBytes:   8192,
				ExclusiveSizeBytes: 4096,
				DriverMetadata:     `{"version":1,"driver":"file"}`,
			}, nil
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
	if got, want := record.RuntimeBaseKey, "runtime-base:test"; got != want {
		t.Fatalf("unexpected runtime base key: got %q want %q", got, want)
	}
	if got, want := record.Architecture, runtime.GOARCH; got != want {
		t.Fatalf("unexpected architecture: got %q want %q", got, want)
	}
	if got, want := record.StorageSizeBytes, int64(8192); got != want {
		t.Fatalf("unexpected storage size bytes: got %d want %d", got, want)
	}
	if got, want := record.ExclusiveSizeBytes, int64(4096); got != want {
		t.Fatalf("unexpected exclusive size bytes: got %d want %d", got, want)
	}
	if got, want := record.DriverMetadata, `{"version":1,"driver":"file"}`; got != want {
		t.Fatalf("unexpected driver metadata: got %q want %q", got, want)
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
	if got, want := record.LastValidatedAt, publishedAt; !got.Equal(want) {
		t.Fatalf("unexpected workspace stage last_validated_at: got %s want %s", got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
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

func TestCreateSandboxDoesNotFallbackWhenWorkspaceStageRestoreIsTerminated(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	restoreStarted := make(chan struct{})
	releaseRestore := make(chan struct{})
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
	adapter.provisionFromSnapshotFn = func(context.Context, backend.ProvisionFromSnapshotRequest) error {
		close(restoreStarted)
		<-releaseRestore
		return errors.New("snapshot restore interrupted")
	}

	createDone := make(chan error, 1)
	go func() {
		_, err := svc.CreateSandbox(context.Background(), req)
		createDone <- err
	}()

	<-restoreStarted
	sandboxID := requireProvisioningSandboxID(t, svc)
	if _, err := svc.TerminateSandbox(context.Background(), &cleanroomv1.TerminateSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("TerminateSandbox returned error: %v", err)
	}
	close(releaseRestore)
	if err := <-createDone; !errors.Is(err, errSandboxCreateAborted) {
		t.Fatalf("CreateSandbox error = %v, want %v", err, errSandboxCreateAborted)
	}
	if got, want := adapter.provisionFromSnapshotCalls, 1; got != want {
		t.Fatalf("unexpected provision-from-snapshot calls: got %d want %d", got, want)
	}
	if got, want := adapter.provisionCalls, 1; got != want {
		t.Fatalf("terminated restore should not fall back to cold provision: got %d want %d", got, want)
	}
	if got, want := adapter.createSnapshotCalls, 1; got != want {
		t.Fatalf("terminated restore should not republish cache: got %d want %d", got, want)
	}
	if got, want := adapter.terminateCalls, 1; got != want {
		t.Fatalf("unexpected terminate calls: got %d want %d", got, want)
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
	plan, ok, err = svc.finalizeDependencyStagePlan(context.Background(), compiled, repository, nil, nil, "firecracker", workspaceKey, "runtime-base:test", plan)
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
	if !strings.Contains(dependencyBootstrap, "mise") || !strings.Contains(dependencyBootstrap, "go") || !strings.Contains(dependencyBootstrap, "download") {
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
	dependencyPlan, ok, err = svc.finalizeDependencyStagePlan(context.Background(), compiled, repository, nil, nil, "firecracker", workspaceKey, "runtime-base:test", dependencyPlan)
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
	servicesPlan, ok, err = svc.finalizeServicesStagePlan(context.Background(), compiled, repository, nil, nil, dependencyPlan.CacheKey, "runtime-base:test", servicesPlan)
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
	if !strings.Contains(servicesBootstrap, "docker") || !strings.Contains(servicesBootstrap, "compose") || !strings.Contains(servicesBootstrap, "postgres") {
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

func TestCreateSandboxPrefersExactDependencyStageOverPortableWhenServicesStageMissing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{}
	var snapshotReqs []backend.SnapshotRequest
	var restoreReqs []backend.ProvisionFromSnapshotRequest
	adapter.createSnapshotFn = func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
		snapshotReqs = append(snapshotReqs, req)
		return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
	}
	adapter.provisionFromSnapshotFn = func(_ context.Context, req backend.ProvisionFromSnapshotRequest) error {
		restoreReqs = append(restoreReqs, req)
		return nil
	}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"go.mod":             "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":             "example.com/test v0.0.0 h1:abc123\n",
		"docker-compose.yml": "services:\n  postgres:\n    image: postgres:17\n",
	})
	svc := newTestServiceWithSnapshotStore(adapter, store)
	svc.RepositoryStore = mirrors

	portableServicesPolicy := testRepositoryDependencyAndServicesPolicy()
	portableServicesPolicy.Dependencies.Reuse = policy.DependencyReusePortable
	req := &cleanroomv1.CreateSandboxRequest{
		Policy:             portableServicesPolicy,
		RepositoryCheckout: repositoryCheckout,
	}

	if _, err := svc.CreateSandbox(context.Background(), req); err != nil {
		t.Fatalf("first CreateSandbox returned error: %v", err)
	}

	records, err := svc.CacheStore.List(context.Background())
	if err != nil {
		t.Fatalf("List cache records returned error: %v", err)
	}
	var exactDependencyRecord, portableDependencyRecord *cachestore.Record
	for i := range records {
		record := records[i]
		switch {
		case record.Stage == dependencyStageName && record.ReuseMode == dependencyStageReuseExact:
			exactDependencyRecord = &record
		case record.Stage == dependencyStageName && record.ReuseMode == dependencyStageReusePortable:
			portableDependencyRecord = &record
		case record.Stage == servicesStageName:
			if err := svc.CacheStore.Delete(context.Background(), record.Stage, record.CacheKey); err != nil {
				t.Fatalf("Delete services cache record returned error: %v", err)
			}
		}
	}
	if exactDependencyRecord == nil {
		t.Fatal("expected published exact dependency stage cache record")
	}
	if portableDependencyRecord == nil {
		t.Fatal("expected published portable dependency stage cache record")
	}

	secondResp, err := svc.CreateSandbox(context.Background(), req)
	if err != nil {
		t.Fatalf("second CreateSandbox returned error: %v", err)
	}
	if got, want := secondResp.GetSourceKind(), "dependency stage cache"; got != want {
		t.Fatalf("unexpected response source kind: got %q want %q", got, want)
	}
	if got, want := adapter.provisionCalls, 1; got != want {
		t.Fatalf("expected exact dependency-stage hit to avoid second cold provision, got %d want %d", got, want)
	}
	if got, want := adapter.provisionFromSnapshotCalls, 1; got != want {
		t.Fatalf("expected only one exact dependency-stage restore, got %d want %d", got, want)
	}
	if got, want := len(restoreReqs), 1; got != want {
		t.Fatalf("expected one restore request, got %d want %d", got, want)
	}
	if got, want := restoreReqs[0].SnapshotID, exactDependencyRecord.BackingSnapshotID; got != want {
		t.Fatalf("unexpected restore snapshot id: got %q want %q", got, want)
	}
	if got, want := adapter.runCalls, 4; got != want {
		t.Fatalf("expected only one extra services bootstrap after dependency restore, got %d want %d", got, want)
	}
	if got, want := len(snapshotReqs), 4; got != want {
		t.Fatalf("expected one replacement services-stage snapshot publish, got %d want %d", got, want)
	}
}

func TestCreateSandboxDependencyRestoreFailureUsesWorkspaceCache(t *testing.T) {
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
	var (
		workspaceRecord  *cachestore.Record
		dependencyRecord *cachestore.Record
	)
	for _, record := range records {
		switch record.Stage {
		case workspaceStageName:
			recordCopy := record
			workspaceRecord = &recordCopy
		case dependencyStageName:
			recordCopy := record
			dependencyRecord = &recordCopy
		case servicesStageName:
			if err := svc.CacheStore.Delete(context.Background(), record.Stage, record.CacheKey); err != nil {
				t.Fatalf("Delete services cache record returned error: %v", err)
			}
		}
	}
	if workspaceRecord == nil {
		t.Fatal("expected published workspace stage cache record")
	}
	if dependencyRecord == nil {
		t.Fatal("expected published dependency stage cache record")
	}

	var restoreReqs []backend.ProvisionFromSnapshotRequest
	adapter.provisionFromSnapshotFn = func(_ context.Context, req backend.ProvisionFromSnapshotRequest) error {
		restoreReqs = append(restoreReqs, req)
		if req.SnapshotID == dependencyRecord.BackingSnapshotID {
			return errors.New("dependency stage restore failed")
		}
		return nil
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
	if got, want := adapter.provisionCalls, 1; got != want {
		t.Fatalf("expected dependency restore failure to fall back through workspace cache without cold provision, got %d want %d", got, want)
	}
	if got, want := adapter.provisionFromSnapshotCalls, 2; got != want {
		t.Fatalf("expected failed dependency restore plus successful workspace restore, got %d want %d", got, want)
	}
	if got, want := len(restoreReqs), 2; got != want {
		t.Fatalf("expected two restore requests, got %d want %d", got, want)
	}
	if got, want := restoreReqs[0].SnapshotID, dependencyRecord.BackingSnapshotID; got != want {
		t.Fatalf("unexpected first restore snapshot id: got %q want %q", got, want)
	}
	if got, want := restoreReqs[1].SnapshotID, workspaceRecord.BackingSnapshotID; got != want {
		t.Fatalf("unexpected second restore snapshot id: got %q want %q", got, want)
	}
	if got, want := adapter.runCalls, 5; got != want {
		t.Fatalf("expected dependency and services bootstrap after workspace restore, got %d want %d", got, want)
	}
	if got, want := adapter.createSnapshotCalls, 5; got != want {
		t.Fatalf("expected replacement dependency and services stage snapshots, got %d want %d", got, want)
	}
	if got, want := len(snapshotReqs), 5; got != want {
		t.Fatalf("expected five snapshot publish requests, got %d want %d", got, want)
	}
	if got, want := adapter.deleteSnapshotCalls, 1; got != want {
		t.Fatalf("expected stale dependency stage snapshot deletion, got %d want %d", got, want)
	}
	if got, want := adapter.deleteSnapshotReq.SnapshotID, dependencyRecord.BackingSnapshotID; got != want {
		t.Fatalf("unexpected deleted snapshot id: got %q want %q", got, want)
	}
	if got, want := adapter.deleteSnapshotReq.StorageRef, dependencyRecord.StorageRef; got != want {
		t.Fatalf("unexpected deleted storage ref: got %q want %q", got, want)
	}

	records, err = svc.CacheStore.List(context.Background())
	if err != nil {
		t.Fatalf("List cache records returned error: %v", err)
	}
	var (
		replacementDependencyRecord *cachestore.Record
		replacementServicesRecord   *cachestore.Record
	)
	for _, record := range records {
		switch record.Stage {
		case dependencyStageName:
			recordCopy := record
			replacementDependencyRecord = &recordCopy
		case servicesStageName:
			recordCopy := record
			replacementServicesRecord = &recordCopy
		}
	}
	if replacementDependencyRecord == nil {
		t.Fatal("expected replacement dependency stage cache record")
	}
	if replacementServicesRecord == nil {
		t.Fatal("expected replacement services stage cache record")
	}
	if got, stale := replacementDependencyRecord.BackingSnapshotID, dependencyRecord.BackingSnapshotID; got == stale {
		t.Fatalf("expected dependency stage cache record to be replaced, still has backing snapshot id %q", got)
	}
}

func TestCreateSandboxReusesPortableDependencyStageAfterCheckoutRefresh(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{}
	var snapshotReqs []backend.SnapshotRequest
	var runCommands [][]string
	keyFilesDigest := ""
	toolchainInputsDigest := ""
	adapter.createSnapshotFn = func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
		snapshotReqs = append(snapshotReqs, req)
		return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
	}
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		runCommands = append(runCommands, append([]string(nil), req.Command...))
		writePortableDependencyValidationDigest(req.Command, stream, keyFilesDigest, toolchainInputsDigest)
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
	updatedRepository := repositorycheckout.FromProto(&updatedCheckout)
	keyFilesDigest, toolchainInputsDigest, err = svc.dependencyStagePlanInputDigests(context.Background(), updatedRepository, nil, nil, []string{"go.mod", "go.sum"}, dependencyStageToolchainInputFiles)
	if err != nil {
		t.Fatalf("dependencyStagePlanInputDigests returned error: %v", err)
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
	if got, want := len(runCommands), 5; got != want {
		t.Fatalf("expected repository bootstrap, dependency bootstrap, checkout refresh, and validations, got %d commands", got)
	}
	refreshCommand := strings.Join(runCommands[2], "\n")
	if !strings.Contains(refreshCommand, updatedCommit) || !strings.Contains(refreshCommand, `git -C "$dest" fetch --filter=blob:none --progress origin "$commit"`) {
		t.Fatalf("expected portable hit to refresh checkout to %s, got %q", updatedCommit, refreshCommand)
	}
	if dependencyBootstrap := strings.Join(runCommands[1], " "); !strings.Contains(dependencyBootstrap, "mise") || !strings.Contains(dependencyBootstrap, "go") || !strings.Contains(dependencyBootstrap, "download") {
		t.Fatalf("expected first run to bootstrap dependencies, got %q", dependencyBootstrap)
	}
	if validationCommand := strings.Join(runCommands[3], "\n"); !strings.Contains(validationCommand, "sha256sum") {
		t.Fatalf("expected portable hit to validate dependency key files, got %q", validationCommand)
	}
	if validationCommand := strings.Join(runCommands[4], "\n"); !strings.Contains(validationCommand, "mise.toml") || !strings.Contains(validationCommand, ".tool-versions") {
		t.Fatalf("expected portable hit to validate dependency toolchain inputs, got %q", validationCommand)
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
		case dependencyStageReusePortable:
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

func TestCreateSandboxDoesNotReusePortableDependencyStageAfterToolchainInputChange(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{}
	var runCommands [][]string
	adapter.createSnapshotFn = func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
		return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
	}
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
		runCommands = append(runCommands, append([]string(nil), req.Command...))
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"go.mod":    "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":    "example.com/test v0.0.0 h1:abc123\n",
		"mise.toml": "[tools]\ngo = \"1.26.2\"\n",
		"app.go":    "package main\n\nfunc main() {}\n",
	})
	if err := os.WriteFile(filepath.Join(mirrors.mirrorPath, "mise.toml"), []byte("[tools]\ngo = \"1.27.0\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(mise.toml) returned error: %v", err)
	}
	runTestGit(t, mirrors.mirrorPath, "add", ".")
	runTestGit(t, mirrors.mirrorPath, "commit", "-m", "toolchain change")
	updatedCommit := strings.TrimSpace(runTestGit(t, mirrors.mirrorPath, "rev-parse", "HEAD"))
	updatedCheckout := *repositoryCheckout
	updatedCheckout.CommitSha = updatedCommit

	svc := newTestServiceWithSnapshotStore(adapter, store)
	svc.RepositoryStore = mirrors

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
	if got := secondResp.GetSourceKind(); got == "portable dependency stage cache" {
		t.Fatalf("unexpected portable dependency-stage hit after toolchain input changed")
	}
	if got, want := adapter.provisionCalls, 2; got != want {
		t.Fatalf("expected toolchain input change to require a second cold provision, got %d want %d", got, want)
	}
	if got, want := adapter.provisionFromSnapshotCalls, 0; got != want {
		t.Fatalf("expected no portable dependency-stage restore after toolchain input change, got %d want %d", got, want)
	}
	if got, want := adapter.createSnapshotCalls, 4; got != want {
		t.Fatalf("expected both creates to publish workspace and dependency snapshots, got %d want %d", got, want)
	}
	for _, command := range runCommands {
		if strings.Contains(strings.Join(command, "\n"), "sha256sum") {
			t.Fatalf("did not expect portable dependency-stage validation command after toolchain input change, got %q", command)
		}
	}
}

func TestPortableDependencyRestoreFailureUsesWorkspace(t *testing.T) {
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
		Policy:             testRepositoryPortableDependencyPolicy(),
		RepositoryCheckout: repositoryCheckout,
	}

	if _, err := svc.CreateSandbox(context.Background(), req); err != nil {
		t.Fatalf("first CreateSandbox returned error: %v", err)
	}

	records, err := svc.CacheStore.List(context.Background())
	if err != nil {
		t.Fatalf("List cache records returned error: %v", err)
	}
	var (
		workspaceRecord *cachestore.Record
		exactRecord     *cachestore.Record
		portableRecord  *cachestore.Record
	)
	for i := range records {
		record := records[i]
		switch {
		case record.Stage == workspaceStageName:
			workspaceRecord = &record
		case record.Stage == dependencyStageName && record.ReuseMode == dependencyStageReuseExact:
			exactRecord = &record
		case record.Stage == dependencyStageName && record.ReuseMode == dependencyStageReusePortable:
			portableRecord = &record
		}
	}
	if workspaceRecord == nil {
		t.Fatal("expected published workspace stage cache record")
	}
	if exactRecord == nil {
		t.Fatal("expected published exact dependency stage cache record")
	}
	if portableRecord == nil {
		t.Fatal("expected published portable dependency stage cache record")
	}
	if err := svc.CacheStore.Delete(context.Background(), exactRecord.Stage, exactRecord.CacheKey); err != nil {
		t.Fatalf("Delete exact dependency cache record returned error: %v", err)
	}

	var restoreReqs []backend.ProvisionFromSnapshotRequest
	adapter.provisionFromSnapshotFn = func(_ context.Context, req backend.ProvisionFromSnapshotRequest) error {
		restoreReqs = append(restoreReqs, req)
		if req.SnapshotID == portableRecord.BackingSnapshotID {
			return errors.New("portable dependency stage restore failed")
		}
		return nil
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
	if got, want := adapter.provisionCalls, 1; got != want {
		t.Fatalf("expected portable restore failure to fall back through workspace cache without cold provision, got %d want %d", got, want)
	}
	if got, want := adapter.provisionFromSnapshotCalls, 2; got != want {
		t.Fatalf("expected failed portable restore plus successful workspace restore, got %d want %d", got, want)
	}
	if got, want := len(restoreReqs), 2; got != want {
		t.Fatalf("expected two restore requests, got %d want %d", got, want)
	}
	if got, want := restoreReqs[0].SnapshotID, portableRecord.BackingSnapshotID; got != want {
		t.Fatalf("unexpected first restore snapshot id: got %q want %q", got, want)
	}
	if got, want := restoreReqs[1].SnapshotID, workspaceRecord.BackingSnapshotID; got != want {
		t.Fatalf("unexpected second restore snapshot id: got %q want %q", got, want)
	}
	if got, want := adapter.runCalls, 3; got != want {
		t.Fatalf("expected only dependency bootstrap after workspace restore, got %d want %d", got, want)
	}
	if got, want := adapter.createSnapshotCalls, 3; got != want {
		t.Fatalf("expected replacement dependency snapshot after workspace restore, got %d want %d", got, want)
	}
	if got, want := len(snapshotReqs), 3; got != want {
		t.Fatalf("expected three snapshot publish requests, got %d want %d", got, want)
	}

	records, err = svc.CacheStore.List(context.Background())
	if err != nil {
		t.Fatalf("List cache records returned error: %v", err)
	}
	var replacementExactRecord, replacementPortableRecord *cachestore.Record
	for i := range records {
		record := records[i]
		switch {
		case record.Stage == dependencyStageName && record.ReuseMode == dependencyStageReuseExact:
			replacementExactRecord = &record
		case record.Stage == dependencyStageName && record.ReuseMode == dependencyStageReusePortable:
			replacementPortableRecord = &record
		}
	}
	if replacementExactRecord == nil {
		t.Fatal("expected replacement exact dependency stage cache record")
	}
	if replacementPortableRecord == nil {
		t.Fatal("expected replacement portable dependency stage cache record")
	}
	if got, stale := replacementExactRecord.BackingSnapshotID, exactRecord.BackingSnapshotID; got == stale {
		t.Fatalf("expected exact dependency stage cache record to be republished, still has backing snapshot id %q", got)
	}
	if got, want := replacementPortableRecord.BackingSnapshotID, replacementExactRecord.BackingSnapshotID; got != want {
		t.Fatalf("expected portable metadata to share replacement dependency snapshot: got %q want %q", got, want)
	}
}

func TestCreateSandboxStoresCommitBundleAfterPortableDependencyStageRestore(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{}
	var runCommands [][]string
	keyFilesDigest := ""
	toolchainInputsDigest := ""
	adapter.createSnapshotFn = func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
		return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
	}

	var (
		mu              sync.Mutex
		bundlePayload   []byte
		bundleStdinUses int
	)
	runCalled := make(chan struct{}, 16)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		if stream.OnAttach != nil {
			var stdin bytes.Buffer
			stream.OnAttach(backend.AttachIO{
				WriteStdin: func(data []byte) error {
					_, err := stdin.Write(data)
					return err
				},
				CloseStdin: func() error {
					mu.Lock()
					if bytes.Equal(stdin.Bytes(), bundlePayload) {
						bundleStdinUses++
					}
					mu.Unlock()
					return nil
				},
			})
		}
		mu.Lock()
		runCommands = append(runCommands, append([]string(nil), req.Command...))
		mu.Unlock()
		writePortableDependencyValidationDigest(req.Command, stream, keyFilesDigest, toolchainInputsDigest)
		select {
		case runCalled <- struct{}{}:
		default:
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"go.mod": "module example.com/test\n\ngo 1.26.2\n",
		"go.sum": "example.com/test v0.0.0 h1:abc123\n",
		"app.go": "package main\n\nfunc main() {}\n",
	})
	baseCommit := repositoryCheckout.GetCommitSha()

	localRepo := filepath.Join(t.TempDir(), "local")
	runTestGit(t, t.TempDir(), "clone", mirrors.mirrorPath, localRepo)
	runTestGit(t, localRepo, "config", "user.email", "test@example.com")
	runTestGit(t, localRepo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(localRepo, "app.go"), []byte("package main\n\nfunc main() { println(\"local\") }\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(app.go) returned error: %v", err)
	}
	runTestGit(t, localRepo, "add", ".")
	runTestGit(t, localRepo, "commit", "-m", "local source-only change")
	localCommit := strings.TrimSpace(runTestGit(t, localRepo, "rev-parse", "HEAD"))
	updatedCheckout := *repositoryCheckout
	updatedCheckout.CommitSha = localCommit
	updatedRepository := repositorycheckout.FromProto(&updatedCheckout)

	commitBundle, err := repositorybundle.BuildFromRepository(localRepo, "origin", updatedRepository)
	if err != nil {
		t.Fatalf("BuildFromRepository returned error: %v", err)
	}
	if commitBundle == nil {
		t.Fatal("expected repository commit bundle")
	}
	bundlePayload = append([]byte(nil), commitBundle.Payload...)
	if err := os.WriteFile(filepath.Join(localRepo, "app.go"), []byte("package main\n\nfunc main() { println(\"dirty\") }\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(app.go dirty) returned error: %v", err)
	}
	changeset, err := repositorychangeset.BuildFromWorkingTree(localRepo, updatedRepository)
	if err != nil {
		t.Fatalf("BuildFromWorkingTree returned error: %v", err)
	}
	if changeset == nil {
		t.Fatal("expected repository changeset")
	}

	mirrors.ensureContainsFn = func(_ string, commitSHA string) error {
		switch strings.TrimSpace(commitSHA) {
		case baseCommit:
			return nil
		case localCommit:
			return errors.New("local-only commit should be provided by stored bundle")
		default:
			return fmt.Errorf("unexpected mirror commit %q", commitSHA)
		}
	}

	svc := newTestServiceWithSnapshotStore(adapter, store)
	svc.RepositoryStore = mirrors
	keyFilesDigest, toolchainInputsDigest, err = svc.dependencyStagePlanInputDigests(context.Background(), updatedRepository, changeset, commitBundle, []string{"go.mod", "go.sum"}, dependencyStageToolchainInputFiles)
	if err != nil {
		t.Fatalf("dependencyStagePlanInputDigests returned error: %v", err)
	}

	if _, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:             testRepositoryPortableDependencyPolicy(),
		RepositoryCheckout: repositoryCheckout,
	}); err != nil {
		t.Fatalf("first CreateSandbox returned error: %v", err)
	}
	secondResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:                 testRepositoryPortableDependencyPolicy(),
		RepositoryCheckout:     &updatedCheckout,
		RepositoryChangeset:    changeset.ToProto(),
		RepositoryCommitBundle: commitBundle.ToProto(),
	})
	if err != nil {
		t.Fatalf("second CreateSandbox returned error: %v", err)
	}
	if got, want := secondResp.GetSourceKind(), "portable dependency stage cache"; got != want {
		t.Fatalf("unexpected response source kind: got %q want %q", got, want)
	}

	sandboxID := secondResp.GetSandbox().GetSandboxId()
	svc.mu.Lock()
	restoredState := svc.sandboxes[sandboxID]
	hasBundle := restoredState != nil && restoredState.RepositoryCommitBundle != nil
	svc.mu.Unlock()
	if !hasBundle {
		t.Fatal("expected portable-cache restored sandbox to retain repository commit bundle")
	}

	for i := 0; i < 2; i++ {
		execResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
			SandboxId:          sandboxID,
			Command:            []string{"sh", "-lc", "pwd"},
			RepositoryCheckout: &updatedCheckout,
		})
		if err != nil {
			t.Fatalf("CreateExecution #%d returned error: %v", i+1, err)
		}
		execution, err := svc.WaitExecution(context.Background(), sandboxID, execResp.GetExecution().GetExecutionId())
		if err != nil {
			t.Fatalf("WaitExecution #%d returned error: %v", i+1, err)
		}
		if got, want := execution.GetStatus(), cleanroomv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED; got != want {
			t.Fatalf("unexpected execution #%d status: got %v want %v", i+1, got, want)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if got, want := bundleStdinUses, 2; got != want {
		t.Fatalf("expected portable restore refresh and consumed-changeset refresh to receive commit bundle stdin, got %d", got)
	}
	foundBundledRefresh := false
	for _, command := range runCommands {
		joined := strings.Join(command, "\n")
		if strings.Contains(joined, `git -C "$dest" fetch --progress "$bundle_file" "+HEAD:$bundle_ref"`) {
			foundBundledRefresh = true
		}
		if strings.Contains(joined, `git -C "$dest" fetch --filter=blob:none --progress origin "$commit"`) && strings.Contains(joined, localCommit) {
			t.Fatalf("expected local-only commit refreshes to use bundle, got %q", joined)
		}
	}
	if !foundBundledRefresh {
		t.Fatalf("expected at least one bundled refresh command, got %#v", runCommands)
	}
}

func TestCreateSandboxBootstrapsServicesAfterPortableDependencyStageRestore(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{}
	var snapshotReqs []backend.SnapshotRequest
	var runCommands [][]string
	keyFilesDigest := ""
	toolchainInputsDigest := ""
	adapter.createSnapshotFn = func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
		snapshotReqs = append(snapshotReqs, req)
		return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
	}
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		runCommands = append(runCommands, append([]string(nil), req.Command...))
		writePortableDependencyValidationDigest(req.Command, stream, keyFilesDigest, toolchainInputsDigest)
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
	updatedRepository := repositorycheckout.FromProto(&updatedCheckout)
	keyFilesDigest, toolchainInputsDigest, err = svc.dependencyStagePlanInputDigests(context.Background(), updatedRepository, nil, nil, []string{"go.mod", "go.sum"}, dependencyStageToolchainInputFiles)
	if err != nil {
		t.Fatalf("dependencyStagePlanInputDigests returned error: %v", err)
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
	if got, want := len(runCommands), 7; got != want {
		t.Fatalf("expected cold bootstraps plus portable refresh, validation, and services bootstrap, got %d want %d", got, want)
	}
	if refreshCommand := strings.Join(runCommands[3], "\n"); !strings.Contains(refreshCommand, updatedCommit) {
		t.Fatalf("expected portable hit to refresh checkout to %s, got %q", updatedCommit, refreshCommand)
	}
	servicesBootstrap := strings.Join(runCommands[6], " ")
	if !strings.Contains(servicesBootstrap, "docker") || !strings.Contains(servicesBootstrap, "compose") || !strings.Contains(servicesBootstrap, "postgres") {
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

	baseDigest, err := svc.dependencyStageKeyFilesDigest(context.Background(), repository, nil, nil, []string{"go.mod", "go.sum"})
	if err != nil {
		t.Fatalf("dependencyStageKeyFilesDigest without changeset returned error: %v", err)
	}
	changesetDigest, err := svc.dependencyStageKeyFilesDigest(context.Background(), repository, changeset, nil, []string{"go.mod", "go.sum"})
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

	tamperedDigest, err := svc.dependencyStageKeyFilesDigest(context.Background(), repository, &tampered, nil, []string{"go.mod", "go.sum"})
	if err != nil {
		t.Fatalf("dependencyStageKeyFilesDigest with tampered changeset metadata returned error: %v", err)
	}
	if got, want := tamperedDigest, changesetDigest; got != want {
		t.Fatalf("expected dependency key file digest to ignore tampered file hashes: got %q want %q", got, want)
	}
}

func TestDependencyStageToolchainInputsDigestDerivesHashesFromRepositoryChangesetPatch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"go.mod":    "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":    "example.com/test v0.0.0 h1:abc123\n",
		"mise.toml": "[tools]\ngo = \"1.26.2\"\n",
	})
	repository := repositorycheckout.FromProto(repositoryCheckout)
	repoDir := mirrors.mirrorPath
	if err := os.WriteFile(filepath.Join(repoDir, "mise.toml"), []byte("[tools]\ngo = \"1.27.0\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(mise.toml) returned error: %v", err)
	}
	changeset, err := repositorychangeset.BuildFromWorkingTree(repoDir, repository)
	if err != nil {
		t.Fatalf("BuildFromWorkingTree returned error: %v", err)
	}
	if changeset == nil {
		t.Fatal("expected repository changeset for modified toolchain input")
	}

	svc := newTestService(&stubAdapter{})
	svc.RepositoryStore = mirrors

	_, baseDigest, err := svc.dependencyStagePlanInputDigests(context.Background(), repository, nil, nil, []string{"go.mod", "go.sum"}, dependencyStageToolchainInputFiles)
	if err != nil {
		t.Fatalf("dependencyStagePlanInputDigests without changeset returned error: %v", err)
	}
	_, changesetDigest, err := svc.dependencyStagePlanInputDigests(context.Background(), repository, changeset, nil, []string{"go.mod", "go.sum"}, dependencyStageToolchainInputFiles)
	if err != nil {
		t.Fatalf("dependencyStagePlanInputDigests with changeset returned error: %v", err)
	}
	if changesetDigest == "" {
		t.Fatal("expected dependency toolchain input digest with repository changeset")
	}
	if changesetDigest == baseDigest {
		t.Fatalf("expected changeset-aware dependency toolchain input digest to differ from base digest %q", baseDigest)
	}
}

func TestDependencyStagePlanNormalizesToolchainInputFilesForValidation(t *testing.T) {
	t.Parallel()

	compiled, err := policy.FromProto(testRepositoryPortableDependencyPolicy())
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	plan, ok := dependencyStagePlanForRepository(compiled, &repositorycheckout.Checkout{DestinationDir: "/workspace"})
	if !ok {
		t.Fatal("expected dependency stage plan")
	}
	if got, want := strings.Join(plan.ToolchainInputFiles, "\x00"), strings.Join(normalizeStageOptionalKeyFiles(dependencyStageToolchainInputFiles), "\x00"); got != want {
		t.Fatalf("unexpected toolchain input file order: got %q want %q", got, want)
	}
}

func TestFinalizeDependencyStagePlanAllowsExactCacheWithoutRepositoryStoreWhenNoKeyFiles(t *testing.T) {
	t.Parallel()

	compiled := &policy.CompiledPolicy{
		Hash: "sha256:7777777777777777777777777777777777777777777777777777777777777777",
		Dependencies: policy.Dependencies{
			Command: []string{"true"},
		},
	}
	repository := &repositorycheckout.Checkout{
		RemoteURL:      "https://github.com/buildkite/cleanroom.git",
		CommitSHA:      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DestinationDir: "/workspace",
	}
	plan, ok := dependencyStagePlanForRepository(compiled, repository)
	if !ok {
		t.Fatal("expected dependency stage plan")
	}

	svc := newTestService(&stubAdapter{})
	plan, ok, err := svc.finalizeDependencyStagePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "workspace:v1:test", "runtime:v1:test", plan)
	if err != nil {
		t.Fatalf("finalizeDependencyStagePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected dependency stage cache plan")
	}
	if plan.CacheKey == "" {
		t.Fatal("expected exact dependency stage cache key")
	}
	if plan.PortableCacheKey != "" {
		t.Fatalf("expected no portable dependency stage cache key without dependency key files, got %q", plan.PortableCacheKey)
	}
	if plan.ToolchainInputsDigest != "" {
		t.Fatalf("expected toolchain inputs to be skipped without repository store, got %q", plan.ToolchainInputsDigest)
	}
}

func TestDependencyStageKeyFilesDigestExpandsGlobsDeterministically(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"go.mod":             "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":             "example.com/test v0.0.0 h1:abc123\n",
		"docker-compose.yml": "services:\n  postgres:\n    image: postgres:17\n",
	})
	repository := repositorycheckout.FromProto(repositoryCheckout)

	svc := newTestService(&stubAdapter{})
	svc.RepositoryStore = mirrors

	globDigest, err := svc.dependencyStageKeyFilesDigest(context.Background(), repository, nil, nil, []string{"*.sum", "go.*"})
	if err != nil {
		t.Fatalf("dependencyStageKeyFilesDigest with glob returned error: %v", err)
	}
	explicitDigest, err := svc.dependencyStageKeyFilesDigest(context.Background(), repository, nil, nil, []string{"go.mod", "go.sum"})
	if err != nil {
		t.Fatalf("dependencyStageKeyFilesDigest with explicit files returned error: %v", err)
	}
	if got, want := globDigest, explicitDigest; got != want {
		t.Fatalf("expected glob and explicit digest to match: got %q want %q", got, want)
	}

	_, err = svc.dependencyStageKeyFilesDigest(context.Background(), repository, nil, nil, []string{"*.missing"})
	if err == nil {
		t.Fatal("expected empty glob to fail")
	}
	if !strings.Contains(err.Error(), "matched no files") {
		t.Fatalf("unexpected empty glob error: %v", err)
	}
}

func TestFinalizeDependencyStagePlanExpandsPortableGlobKeyFilesForValidation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"go.mod": "module example.com/test\n\ngo 1.26.2\n",
		"go.sum": "example.com/test v0.0.0 h1:abc123\n",
	})
	repository := repositorycheckout.FromProto(repositoryCheckout)

	policyProto := testRepositoryPortableDependencyPolicy()
	policyProto.Dependencies.Blocks[0].Inputs.Files = []string{"go.*"}
	compiled, err := policy.FromProto(policyProto)
	if err != nil {
		t.Fatalf("FromProto returned error: %v", err)
	}
	plan, ok := dependencyStagePlanForRepository(compiled, repository)
	if !ok {
		t.Fatal("expected dependency stage plan")
	}

	svc := newTestService(&stubAdapter{})
	svc.RepositoryStore = mirrors
	plan, ok, err = svc.finalizeDependencyStagePlan(context.Background(), compiled, repository, nil, nil, "firecracker", "workspace-base:test", "runtime-base:test", plan)
	if err != nil {
		t.Fatalf("finalizeDependencyStagePlan returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected finalized dependency stage plan")
	}
	if got, want := plan.KeyFiles, []string{"go.*"}; !slices.Equal(got, want) {
		t.Fatalf("unexpected original key files: got %v want %v", got, want)
	}
	if got, want := plan.ExpandedKeyFiles, []string{"go.mod", "go.sum"}; !slices.Equal(got, want) {
		t.Fatalf("unexpected expanded key files: got %v want %v", got, want)
	}

	command, err := dependencyStageKeyFilesDigestCommand(repository, plan.ExpandedKeyFiles)
	if err != nil {
		t.Fatalf("dependencyStageKeyFilesDigestCommand returned error: %v", err)
	}
	script := strings.Join(command, "\n")
	if strings.Contains(script, "go.*") {
		t.Fatalf("expected validation command to use expanded key files, got %q", script)
	}
	for _, want := range []string{"go.mod", "go.sum"} {
		if !strings.Contains(script, want) {
			t.Fatalf("expected validation command to contain %q, got %q", want, script)
		}
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
	if !strings.Contains(dependencyBootstrap, "mise") || !strings.Contains(dependencyBootstrap, "go") || !strings.Contains(dependencyBootstrap, "download") {
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

func TestTerminateCreatedSandboxCleansRuntimeDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	stateHome := filepath.Join(tmpDir, "state-home")
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmpDir, "cache-home"))

	adapter := &stubAdapter{}
	svc := newTestService(adapter)
	sb := &sandboxState{
		ID:        "sandbox_test",
		Status:    cleanroomv1.SandboxStatus_SANDBOX_STATUS_PROVISIONING,
		CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
		UpdatedAt: time.Unix(1_700_000_000, 0).UTC(),
		events:    newEventFeed[*cleanroomv1.SandboxEvent](0),
		Done:      make(chan struct{}),
	}
	svc.mu.Lock()
	svc.ensureMapsLocked()
	svc.sandboxes[sb.ID] = sb
	svc.mu.Unlock()

	runtimeDir := filepath.Join(stateHome, "cleanroom", "sandboxes", sb.ID)
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatalf("create runtime dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "rootfs.ext4"), []byte("runtime"), 0o644); err != nil {
		t.Fatalf("write runtime file: %v", err)
	}

	if err := svc.terminateCreatedSandbox(context.Background(), adapter, sb.ID); err != nil {
		t.Fatalf("terminateCreatedSandbox returned error: %v", err)
	}
	waitForPathRemoved(t, runtimeDir)
	svc.mu.RLock()
	_, ok := svc.sandboxes[sb.ID]
	svc.mu.RUnlock()
	if ok {
		t.Fatal("expected sandbox state to be dropped after successful cleanup")
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
				if stream.OnStdout != nil {
					stream.OnStdout([]byte("pre-run output\n"))
				}
				return &backend.ExecutionResult{
					ExecutionID: req.ExecutionID,
					ExitCode:    0,
					Message:     "ok",
				}, nil
			case "sh -lc pwd":
				if stream.OnStdout != nil {
					stream.OnStdout([]byte("user output\n"))
				}
				return &backend.ExecutionResult{
					ExecutionID: req.ExecutionID,
					ExitCode:    0,
					Message:     "ok",
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
				if stream.OnStdout != nil {
					stream.OnStdout([]byte("pre-run output\n"))
				}
				return &backend.ExecutionResult{
					ExecutionID: req.ExecutionID,
					ExitCode:    23,
					LaunchedVM:  true,
					PlanPath:    "/tmp/pre-run-plan",
					RunDir:      "/tmp/pre-run-run",
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

func TestCreateExecutionCanPreservePendingChangesetForInternalWorkspaceCopy(t *testing.T) {
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
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
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

	copyResp, err := svc.CreateInternalWorkspaceCopyInExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId:          sandboxID,
		Command:            []string{"sh", "-lc", "true"},
		RepositoryCheckout: repositoryCheckout,
		Options: &cleanroomv1.ExecutionOptions{
			PreserveRepositoryChangesetPendingExecution: true,
		},
	})
	if err != nil {
		t.Fatalf("CreateExecution internal copy returned error: %v", err)
	}
	if _, err := svc.WaitExecution(context.Background(), sandboxID, copyResp.GetExecution().GetExecutionId()); err != nil {
		t.Fatalf("WaitExecution internal copy returned error: %v", err)
	}

	userResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId:          sandboxID,
		Command:            []string{"sh", "-lc", "pwd"},
		RepositoryCheckout: repositoryCheckout,
	})
	if err != nil {
		t.Fatalf("CreateExecution user command returned error: %v", err)
	}
	if _, err := svc.WaitExecution(context.Background(), sandboxID, userResp.GetExecution().GetExecutionId()); err != nil {
		t.Fatalf("WaitExecution user command returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if got, want := len(commands), 4; got != want {
		t.Fatalf("expected create bootstrap + changeset apply + internal copy + user execution, got %d command(s)", got)
	}
	userCommand := strings.Join(commands[3], " ")
	if strings.Contains(userCommand, `git -C "$dest" fetch --filter=blob:none --progress origin "$commit"`) {
		t.Fatalf("expected user execution to preserve pending changeset state instead of refreshing checkout, got %q", userCommand)
	}
	if !repositoryWrappedCommandContains(userCommand, `exec 'sh' '-lc' 'pwd'`) {
		t.Fatalf("expected wrapped user command, got %q", userCommand)
	}
}

func TestCreateExecutionRejectsInternalWorkspaceCopyInOptions(t *testing.T) {
	svc := newTestService(&stubAdapter{})

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy: testRepositoryRunBeforePolicy(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	for name, opts := range map[string]*cleanroomv1.ExecutionOptions{
		"preserve pending changeset": {
			PreserveRepositoryChangesetPendingExecution: true,
		},
		"skip run before": {
			SkipRunBefore: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
				SandboxId: sandboxID,
				Command:   []string{"sh", "-lc", "pwd"},
				Options:   opts,
			})
			if err == nil {
				t.Fatal("expected CreateExecution to reject internal workspace copy-in option")
			}
			if !strings.Contains(err.Error(), "internal workspace copy-in executions") {
				t.Fatalf("expected internal workspace copy-in error, got %v", err)
			}
		})
	}
}

func TestCreateInternalWorkspaceCopyInExecutionCanSkipRunBefore(t *testing.T) {
	adapter := &stubAdapter{}
	svc := newTestService(adapter)

	var (
		mu       sync.Mutex
		commands [][]string
	)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		mu.Unlock()
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy: testRepositoryRunBeforePolicy(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	resp, err := svc.CreateInternalWorkspaceCopyInExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"sh", "-lc", "pwd"},
		Options: &cleanroomv1.ExecutionOptions{
			SkipRunBefore: true,
		},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	if _, err := svc.WaitExecution(context.Background(), sandboxID, resp.GetExecution().GetExecutionId()); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if got, want := len(commands), 1; got != want {
		t.Fatalf("expected only user command when run.before is skipped, got %d command(s)", got)
	}
	joined := strings.Join(commands[0], " ")
	if strings.Contains(joined, "echo pre-run") {
		t.Fatalf("expected run.before to be skipped, got %q", joined)
	}
}

func TestCreateExecutionKeepsPendingChangesetAfterRunBeforeFailure(t *testing.T) {
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
		mu             sync.Mutex
		commands       [][]string
		preRunAttempts int
	)
	runCalled := make(chan struct{}, 8)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
		joined := strings.Join(req.Command, " ")
		isPreRun := repositoryWrappedCommandContains(joined, `exec 'sh' '-lc' 'echo pre-run'`)

		mu.Lock()
		commands = append(commands, append([]string(nil), req.Command...))
		attempt := 0
		if isPreRun {
			preRunAttempts++
			attempt = preRunAttempts
		}
		mu.Unlock()

		select {
		case runCalled <- struct{}{}:
		default:
		}

		if isPreRun && attempt == 1 {
			return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 23, Message: "pre-run failed"}, nil
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:              testRepositoryRunBeforePolicy(),
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

	firstResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId:          sandboxID,
		Command:            []string{"sh", "-lc", "pwd"},
		RepositoryCheckout: repositoryCheckout,
	})
	if err != nil {
		t.Fatalf("CreateExecution first attempt returned error: %v", err)
	}
	firstExecution, err := svc.WaitExecution(context.Background(), sandboxID, firstResp.GetExecution().GetExecutionId())
	if err != nil {
		t.Fatalf("WaitExecution first attempt returned error: %v", err)
	}
	if got, want := firstExecution.GetStatus(), cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED; got != want {
		t.Fatalf("unexpected first execution status: got %v want %v", got, want)
	}

	secondResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId:          sandboxID,
		Command:            []string{"sh", "-lc", "pwd"},
		RepositoryCheckout: repositoryCheckout,
	})
	if err != nil {
		t.Fatalf("CreateExecution retry returned error: %v", err)
	}
	secondExecution, err := svc.WaitExecution(context.Background(), sandboxID, secondResp.GetExecution().GetExecutionId())
	if err != nil {
		t.Fatalf("WaitExecution retry returned error: %v", err)
	}
	if got, want := secondExecution.GetStatus(), cleanroomv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED; got != want {
		t.Fatalf("unexpected retry execution status: got %v want %v", got, want)
	}

	mu.Lock()
	defer mu.Unlock()
	if got, want := preRunAttempts, 2; got != want {
		t.Fatalf("expected two run.before attempts, got %d", got)
	}
	if got, want := len(commands), 5; got != want {
		t.Fatalf("expected create bootstrap + changeset apply + failed run.before + retry run.before + retry execution, got %d command(s)", got)
	}
	failedPreRun := strings.Join(commands[2], " ")
	if strings.Contains(failedPreRun, "git clone --filter=blob:none --no-checkout") {
		t.Fatalf("expected failed run.before to use created changeset state, got %q", failedPreRun)
	}
	if !repositoryWrappedCommandContains(failedPreRun, `exec 'sh' '-lc' 'echo pre-run'`) {
		t.Fatalf("expected wrapped run.before command on first attempt, got %q", failedPreRun)
	}
	retryPreRun := strings.Join(commands[3], " ")
	if strings.Contains(retryPreRun, `git -C "$dest" fetch --filter=blob:none --progress origin "$commit"`) {
		t.Fatalf("expected retry to preserve pending changeset state instead of refreshing checkout, got %q", retryPreRun)
	}
	if !repositoryWrappedCommandContains(retryPreRun, `exec 'sh' '-lc' 'echo pre-run'`) {
		t.Fatalf("expected wrapped run.before command on retry, got %q", retryPreRun)
	}
	retryExecution := strings.Join(commands[4], " ")
	if !repositoryWrappedCommandContains(retryExecution, `exec 'sh' '-lc' 'pwd'`) {
		t.Fatalf("expected wrapped user command on retry, got %q", retryExecution)
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

func TestCreateExecutionRebootstrapsConsumedChangesetWithStoredCommitBundle(t *testing.T) {
	adapter := &stubAdapter{}
	svc := newTestService(adapter)

	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"README.md": "hello\n",
	})
	baseCommit := repositoryCheckout.GetCommitSha()

	localRepo := filepath.Join(t.TempDir(), "local")
	runTestGit(t, t.TempDir(), "clone", mirrors.mirrorPath, localRepo)
	runTestGit(t, localRepo, "config", "user.email", "test@example.com")
	runTestGit(t, localRepo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(localRepo, "local.txt"), []byte("local\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(local.txt) returned error: %v", err)
	}
	runTestGit(t, localRepo, "add", "local.txt")
	runTestGit(t, localRepo, "commit", "-m", "local commit")
	localCommit := strings.TrimSpace(runTestGit(t, localRepo, "rev-parse", "HEAD"))
	repositoryCheckout.CommitSha = localCommit
	repository := repositorycheckout.FromProto(repositoryCheckout)

	commitBundle, err := repositorybundle.BuildFromRepository(localRepo, "origin", repository)
	if err != nil {
		t.Fatalf("BuildFromRepository returned error: %v", err)
	}
	if commitBundle == nil {
		t.Fatal("expected repository commit bundle")
	}
	if err := os.WriteFile(filepath.Join(localRepo, "README.md"), []byte("hello from changeset\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(README.md) returned error: %v", err)
	}
	changeset, err := repositorychangeset.BuildFromWorkingTree(localRepo, repository)
	if err != nil {
		t.Fatalf("BuildFromWorkingTree returned error: %v", err)
	}
	if changeset == nil {
		t.Fatal("expected repository changeset")
	}

	mirrors.ensureContainsFn = func(_ string, commitSHA string) error {
		if strings.TrimSpace(commitSHA) == localCommit {
			return errors.New("local-only commit should be provided by stored bundle")
		}
		if strings.TrimSpace(commitSHA) != baseCommit {
			return fmt.Errorf("unexpected mirror commit %q", commitSHA)
		}
		return nil
	}
	svc.RepositoryStore = mirrors

	var (
		mu              sync.Mutex
		commands        [][]string
		bundleStdinUses int
	)
	runCalled := make(chan struct{}, 8)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		if stream.OnAttach != nil {
			var stdin bytes.Buffer
			stream.OnAttach(backend.AttachIO{
				WriteStdin: func(data []byte) error {
					_, err := stdin.Write(data)
					return err
				},
				CloseStdin: func() error {
					if bytes.Equal(stdin.Bytes(), commitBundle.Payload) {
						mu.Lock()
						bundleStdinUses++
						mu.Unlock()
					}
					return nil
				},
			})
		}
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
		Policy:                 testRepositoryPolicy(),
		RepositoryCheckout:     repositoryCheckout,
		RepositoryChangeset:    changeset.ToProto(),
		RepositoryCommitBundle: commitBundle.ToProto(),
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
	if got, want := bundleStdinUses, 2; got != want {
		t.Fatalf("expected initial bootstrap and refresh to receive commit bundle stdin, got %d", got)
	}
	if got, want := len(commands), 5; got != want {
		t.Fatalf("expected create bootstrap + changeset apply + first execution + bundled refresh + second execution, got %d command(s)", got)
	}
	rebootstrap := strings.Join(commands[3], " ")
	if !strings.Contains(rebootstrap, `git -C "$dest" fetch --progress "$bundle_file" "+HEAD:$bundle_ref"`) {
		t.Fatalf("expected second matching repository execution to fetch stored bundle, got %q", rebootstrap)
	}
	if strings.Contains(rebootstrap, `git -C "$dest" fetch --filter=blob:none --progress origin "$commit"`) {
		t.Fatalf("expected stored bundle refresh to avoid fetching the local-only commit directly, got %q", rebootstrap)
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

func TestCreateSandboxPersistsRepositoryChangeset(t *testing.T) {
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

	store := newMemoryChangesetStore()
	svc.ChangesetStore = store
	_, err = svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:              testRepositoryPolicy(),
		RepositoryCheckout:  repositoryCheckout,
		RepositoryChangeset: changeset.ToProto(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if got, want := len(store.records), 1; got != want {
		t.Fatalf("expected one persisted changeset, got %d", got)
	}
	record := store.records[changesetstore.RecordID(repository, changeset)]
	if record.ChangesetID == "" {
		t.Fatalf("expected persisted changeset id in records: %#v", store.records)
	}
	if record.ChangesetDigest != changeset.Digest {
		t.Fatalf("unexpected persisted changeset digest: got %q want %q", record.ChangesetDigest, changeset.Digest)
	}
	if record.FinalTreeDigest != changeset.TreeDigest {
		t.Fatalf("unexpected persisted tree digest: got %q want %q", record.FinalTreeDigest, changeset.TreeDigest)
	}
}

func TestCreateSandboxBootstrapsLocalOnlyCommitBundle(t *testing.T) {
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"README.md": "hello\n",
	})
	baseCommit := repositoryCheckout.GetCommitSha()

	localRepo := filepath.Join(t.TempDir(), "local")
	runTestGit(t, t.TempDir(), "clone", mirrors.mirrorPath, localRepo)
	runTestGit(t, localRepo, "config", "user.email", "test@example.com")
	runTestGit(t, localRepo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(localRepo, "local.txt"), []byte("local\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(local.txt) returned error: %v", err)
	}
	runTestGit(t, localRepo, "add", "local.txt")
	runTestGit(t, localRepo, "commit", "-m", "local commit")
	localCommit := strings.TrimSpace(runTestGit(t, localRepo, "rev-parse", "HEAD"))

	commitBundle, err := repositorybundle.BuildFromRepository(localRepo, "origin", &repositorycheckout.Checkout{CommitSHA: localCommit})
	if err != nil {
		t.Fatalf("BuildFromRepository returned error: %v", err)
	}
	if commitBundle == nil {
		t.Fatal("expected repository commit bundle")
	}
	repositoryCheckout.CommitSha = localCommit

	var capturedBundle []byte
	adapter := &stubAdapter{}
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		if stream.OnAttach != nil {
			var stdin bytes.Buffer
			stream.OnAttach(backend.AttachIO{
				WriteStdin: func(data []byte) error {
					_, err := stdin.Write(data)
					return err
				},
				CloseStdin: func() error {
					capturedBundle = append([]byte(nil), stdin.Bytes()...)
					return nil
				},
			})
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}
	svc := newTestService(adapter)
	svc.RepositoryStore = mirrors

	_, err = svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:                 testRepositoryPolicy(),
		RepositoryCheckout:     repositoryCheckout,
		RepositoryCommitBundle: commitBundle.ToProto(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if got, want := mirrors.commitSHA, baseCommit; got != want {
		t.Fatalf("expected mirror to ensure bundle prerequisite %q, got %q", want, got)
	}
	if !bytes.Equal(capturedBundle, commitBundle.Payload) {
		t.Fatalf("bootstrap stdin did not receive repository commit bundle")
	}
	joined := strings.Join(adapter.req.Command, " ")
	if !strings.Contains(joined, `git -C "$dest" fetch --progress "$bundle_file" "+HEAD:$bundle_ref"`) {
		t.Fatalf("expected bootstrap command to fetch attached bundle, got %q", joined)
	}
}

func TestCreateSandboxStoresCommitBundleAfterServicesStageRestore(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{}
	adapter.createSnapshotFn = func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
		return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
	}

	var (
		mu              sync.Mutex
		bundlePayload   []byte
		bundleStdinUses int
	)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		if stream.OnAttach != nil {
			var stdin bytes.Buffer
			stream.OnAttach(backend.AttachIO{
				WriteStdin: func(data []byte) error {
					_, err := stdin.Write(data)
					return err
				},
				CloseStdin: func() error {
					mu.Lock()
					if bytes.Equal(stdin.Bytes(), bundlePayload) {
						bundleStdinUses++
					}
					mu.Unlock()
					return nil
				},
			})
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"go.mod":             "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":             "example.com/test v0.0.0 h1:abc123\n",
		"docker-compose.yml": "services:\n  postgres:\n    image: postgres:17\n",
		"app.go":             "package main\n\nfunc main() {}\n",
	})
	baseCommit := repositoryCheckout.GetCommitSha()

	localRepo := filepath.Join(t.TempDir(), "local")
	runTestGit(t, t.TempDir(), "clone", mirrors.mirrorPath, localRepo)
	runTestGit(t, localRepo, "config", "user.email", "test@example.com")
	runTestGit(t, localRepo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(localRepo, "app.go"), []byte("package main\n\nfunc main() { println(\"local\") }\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(app.go) returned error: %v", err)
	}
	runTestGit(t, localRepo, "add", ".")
	runTestGit(t, localRepo, "commit", "-m", "local source-only change")
	localCommit := strings.TrimSpace(runTestGit(t, localRepo, "rev-parse", "HEAD"))
	updatedCheckout := *repositoryCheckout
	updatedCheckout.CommitSha = localCommit
	updatedRepository := repositorycheckout.FromProto(&updatedCheckout)

	commitBundle, err := repositorybundle.BuildFromRepository(localRepo, "origin", updatedRepository)
	if err != nil {
		t.Fatalf("BuildFromRepository returned error: %v", err)
	}
	if commitBundle == nil {
		t.Fatal("expected repository commit bundle")
	}
	bundlePayload = append([]byte(nil), commitBundle.Payload...)
	if err := os.WriteFile(filepath.Join(localRepo, "app.go"), []byte("package main\n\nfunc main() { println(\"dirty\") }\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(app.go dirty) returned error: %v", err)
	}
	changeset, err := repositorychangeset.BuildFromWorkingTree(localRepo, updatedRepository)
	if err != nil {
		t.Fatalf("BuildFromWorkingTree returned error: %v", err)
	}
	if changeset == nil {
		t.Fatal("expected repository changeset")
	}

	mirrors.ensureContainsFn = func(_ string, commitSHA string) error {
		switch strings.TrimSpace(commitSHA) {
		case baseCommit:
			return nil
		case localCommit:
			return errors.New("local-only commit should be provided by stored bundle")
		default:
			return fmt.Errorf("unexpected mirror commit %q", commitSHA)
		}
	}

	svc := newTestServiceWithSnapshotStore(adapter, store)
	svc.RepositoryStore = mirrors
	req := &cleanroomv1.CreateSandboxRequest{
		Policy:                 testRepositoryDependencyAndServicesPolicy(),
		RepositoryCheckout:     &updatedCheckout,
		RepositoryChangeset:    changeset.ToProto(),
		RepositoryCommitBundle: commitBundle.ToProto(),
	}
	if _, err := svc.CreateSandbox(context.Background(), req); err != nil {
		t.Fatalf("first CreateSandbox returned error: %v", err)
	}
	secondResp, err := svc.CreateSandbox(context.Background(), req)
	if err != nil {
		t.Fatalf("second CreateSandbox returned error: %v", err)
	}
	if got, want := secondResp.GetSourceKind(), "services stage cache"; got != want {
		t.Fatalf("unexpected response source kind: got %q want %q", got, want)
	}

	sandboxID := secondResp.GetSandbox().GetSandboxId()
	svc.mu.Lock()
	restoredState := svc.sandboxes[sandboxID]
	hasBundle := restoredState != nil && restoredState.RepositoryCommitBundle != nil
	svc.mu.Unlock()
	if !hasBundle {
		t.Fatal("expected services-cache restored sandbox to retain repository commit bundle")
	}

	for i := 0; i < 2; i++ {
		execResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
			SandboxId:          sandboxID,
			Command:            []string{"sh", "-lc", "pwd"},
			RepositoryCheckout: &updatedCheckout,
		})
		if err != nil {
			t.Fatalf("CreateExecution #%d returned error: %v", i+1, err)
		}
		execution, err := svc.WaitExecution(context.Background(), sandboxID, execResp.GetExecution().GetExecutionId())
		if err != nil {
			t.Fatalf("WaitExecution #%d returned error: %v", i+1, err)
		}
		if got, want := execution.GetStatus(), cleanroomv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED; got != want {
			t.Fatalf("unexpected execution #%d status: got %v want %v", i+1, got, want)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if got, want := bundleStdinUses, 2; got != want {
		t.Fatalf("expected cold bootstrap and consumed-changeset refresh to receive commit bundle stdin, got %d", got)
	}
}

func TestCreateSandboxCachesStagesForLocalOnlyCommitBundle(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := newMemorySnapshotStore()
	adapter := &stubAdapter{}
	mirrors, repositoryCheckout := testRepositoryMirror(t, map[string]string{
		"go.mod":             "module example.com/test\n\ngo 1.26.2\n",
		"go.sum":             "example.com/test v0.0.0 h1:abc123\n",
		"docker-compose.yml": "services:\n  postgres:\n    image: postgres:17\n",
	})
	baseCommit := repositoryCheckout.GetCommitSha()

	localRepo := filepath.Join(t.TempDir(), "local")
	runTestGit(t, t.TempDir(), "clone", mirrors.mirrorPath, localRepo)
	runTestGit(t, localRepo, "config", "user.email", "test@example.com")
	runTestGit(t, localRepo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(localRepo, "go.sum"), []byte("example.com/test v0.0.0 h1:abc123\nexample.com/lib v1.0.0 h1:def456\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(go.sum) returned error: %v", err)
	}
	runTestGit(t, localRepo, "add", "go.sum")
	runTestGit(t, localRepo, "commit", "-m", "local dependency update")
	localCommit := strings.TrimSpace(runTestGit(t, localRepo, "rev-parse", "HEAD"))

	commitBundle, err := repositorybundle.BuildFromRepository(localRepo, "origin", &repositorycheckout.Checkout{CommitSHA: localCommit})
	if err != nil {
		t.Fatalf("BuildFromRepository returned error: %v", err)
	}
	if commitBundle == nil {
		t.Fatal("expected repository commit bundle")
	}
	repositoryCheckout.CommitSha = localCommit

	svc := newTestServiceWithSnapshotStore(adapter, store)
	svc.RepositoryStore = mirrors
	if _, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy:                 testRepositoryDependencyAndServicesPolicy(),
		RepositoryCheckout:     repositoryCheckout,
		RepositoryCommitBundle: commitBundle.ToProto(),
	}); err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	cacheStore, ok := svc.CacheStore.(*memoryCacheStore)
	if !ok {
		t.Fatalf("expected memory cache store, got %T", svc.CacheStore)
	}
	records, err := cacheStore.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	stages := map[string]bool{}
	for _, record := range records {
		stages[record.Stage] = true
	}
	for _, stage := range []string{workspaceStageName, dependencyStageName, servicesStageName} {
		if !stages[stage] {
			t.Fatalf("expected %s stage cache record, got stages %v", stage, stages)
		}
	}
	if !slices.Contains(mirrors.commitSHAs, baseCommit) {
		t.Fatalf("expected bundle prerequisite %q to be ensured before cache planning/bootstrap, got %v", baseCommit, mirrors.commitSHAs)
	}
	if slices.Contains(mirrors.commitSHAs, localCommit) {
		t.Fatalf("expected local-only commit %q to come from bundle, got direct ensure calls %v", localCommit, mirrors.commitSHAs)
	}
	if !slices.Contains(mirrors.withRepositorySHAs, baseCommit) {
		t.Fatalf("expected repository views to use bundle prerequisite %q, got %v", baseCommit, mirrors.withRepositorySHAs)
	}
	if slices.Contains(mirrors.withRepositorySHAs, "") {
		t.Fatalf("expected bundle repository views to use a prerequisite commit, got %v", mirrors.withRepositorySHAs)
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

func TestCreateSandboxRejectsDependencyBlocksWithoutRepositoryCheckout(t *testing.T) {
	adapter := &stubAdapter{}
	svc := newTestService(adapter)

	_, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy: testRepositoryDependencyPolicy(),
	})
	if err == nil {
		t.Fatal("expected dependency blocks to require repository checkout")
	}
	if !strings.Contains(err.Error(), "sandbox.dependencies") || !strings.Contains(err.Error(), "repository checkout") {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := adapter.provisionCalls; got != 0 {
		t.Fatalf("expected no provision call without repository checkout, got %d", got)
	}
}

func TestCreateSandboxRejectsServiceBlocksWithoutRepositoryCheckout(t *testing.T) {
	adapter := &stubAdapter{}
	svc := newTestService(adapter)

	policyProto := testRepositoryPolicy()
	policyProto.Services = &cleanroomv1.PolicyServices{
		Blocks: []*cleanroomv1.PolicyBlock{testPolicyBlock(
			"postgres",
			[]string{"docker", "compose", "up", "-d", "postgres"},
			[]string{"docker-compose.yml"},
			[]string{"/var/lib/cleanroom/services/postgres"},
			nil,
		)},
	}

	_, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy: policyProto,
	})
	if err == nil {
		t.Fatal("expected service blocks to require repository checkout")
	}
	if !strings.Contains(err.Error(), "sandbox.services") || !strings.Contains(err.Error(), "repository checkout") {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := adapter.provisionCalls; got != 0 {
		t.Fatalf("expected no provision call without repository checkout, got %d", got)
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

func TestCreateSandboxAppliesPolicyResourceMinimums(t *testing.T) {
	adapter := &stubAdapter{}
	svc := &Service{
		Config: runtimeconfig.Config{
			DefaultBackend: "darwin-vz",
			Backends: runtimeconfig.Backends{
				DarwinVZ: runtimeconfig.DarwinVZConfig{
					VCPUs:              2,
					MemoryMiB:          1024,
					MinimumRootFSBytes: runtimeconfig.ByteSize(8 << 30),
				},
			},
		},
		Backends: map[string]backend.Adapter{"darwin-vz": adapter},
	}

	policyProto := testPolicy()
	policyProto.Resources = &cleanroomv1.PolicyResources{
		Vcpus:       4,
		MemoryBytes: (3 << 30) + 1,
		DiskBytes:   16 << 30,
	}

	resp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Policy: policyProto,
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	gotCfg := adapter.provisionReq.FirecrackerConfig
	if got, want := gotCfg.VCPUs, int64(4); got != want {
		t.Fatalf("unexpected vcpus: got %d want %d", got, want)
	}
	if got, want := gotCfg.MemoryMiB, int64(3073); got != want {
		t.Fatalf("unexpected memory_mib: got %d want %d", got, want)
	}
	if got, want := gotCfg.MinimumRootFSBytes, int64(16<<30); got != want {
		t.Fatalf("unexpected minimum_rootfs_bytes: got %d want %d", got, want)
	}

	resources := resp.GetSandbox().GetEffectiveResources()
	if resources == nil {
		t.Fatal("expected sandbox effective resources")
	}
	if got, want := resources.GetVcpus(), int64(4); got != want {
		t.Fatalf("unexpected effective vcpus: got %d want %d", got, want)
	}
	if got, want := resources.GetMemoryBytes(), int64(3073<<20); got != want {
		t.Fatalf("unexpected effective memory_bytes: got %d want %d", got, want)
	}
	if got, want := resources.GetDiskBytes(), int64(16<<30); got != want {
		t.Fatalf("unexpected effective disk_bytes: got %d want %d", got, want)
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
	policyProto.Docker = &cleanroomv1.PolicyDocker{Required: true}

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
	if got, want := adapter.provisionReq.FirecrackerConfig.MinimumRootFSBytesSource, backend.RootFSMinimumSourceDockerRepositoryBootstrap; got != want {
		t.Fatalf("unexpected minimum_rootfs_bytes source: got %q want %q", got, want)
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

func TestUploadSandboxFileWritesData(t *testing.T) {
	expectedSandboxID := ""
	adapter := &stubAdapter{
		uploadFn: func(_ context.Context, sandboxID, path string, data []byte, mode fs.FileMode) error {
			if got, want := sandboxID, expectedSandboxID; got != want {
				t.Fatalf("unexpected sandbox id: got %q want %q", got, want)
			}
			if got, want := path, "/home/sprite/artifacts/result.txt"; got != want {
				t.Fatalf("unexpected path: got %q want %q", got, want)
			}
			if got, want := string(data), "artifact-data"; got != want {
				t.Fatalf("unexpected data: got %q want %q", got, want)
			}
			if got, want := mode, fs.FileMode(0o600); got != want {
				t.Fatalf("unexpected mode: got %04o want %04o", got, want)
			}
			return nil
		},
	}
	svc := newTestService(adapter)

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	expectedSandboxID = createResp.GetSandbox().GetSandboxId()

	resp, err := svc.UploadSandboxFile(context.Background(), &cleanroomv1.UploadSandboxFileRequest{
		SandboxId: expectedSandboxID,
		Path:      "/home/sprite/artifacts/result.txt",
		Data:      []byte("artifact-data"),
		Mode:      0o600,
	})
	if err != nil {
		t.Fatalf("UploadSandboxFile returned error: %v", err)
	}
	if got, want := resp.GetSizeBytes(), int64(len("artifact-data")); got != want {
		t.Fatalf("unexpected size_bytes: got %d want %d", got, want)
	}
}

func TestUploadSandboxFileDefaultsMode(t *testing.T) {
	adapter := &stubAdapter{
		uploadFn: func(_ context.Context, _, _ string, _ []byte, mode fs.FileMode) error {
			if got, want := mode, fs.FileMode(0o644); got != want {
				t.Fatalf("unexpected mode: got %04o want %04o", got, want)
			}
			return nil
		},
	}
	svc := newTestService(adapter)

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	_, err = svc.UploadSandboxFile(context.Background(), &cleanroomv1.UploadSandboxFileRequest{
		SandboxId: createResp.GetSandbox().GetSandboxId(),
		Path:      "/tmp/upload.txt",
		Data:      []byte("data"),
	})
	if err != nil {
		t.Fatalf("UploadSandboxFile returned error: %v", err)
	}
}

func TestUploadSandboxFileRejectsOversizedData(t *testing.T) {
	adapter := &stubAdapter{
		uploadFn: func(context.Context, string, string, []byte, fs.FileMode) error {
			t.Fatal("backend upload should not be called")
			return nil
		},
	}
	svc := newTestService(adapter)
	svc.runtime.defaultDownloadMaxBytes = 4

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	_, err = svc.UploadSandboxFile(context.Background(), &cleanroomv1.UploadSandboxFileRequest{
		SandboxId: createResp.GetSandbox().GetSandboxId(),
		Path:      "/tmp/upload.txt",
		Data:      []byte("12345"),
	})
	if err == nil {
		t.Fatal("expected oversized upload error")
	}
	if !strings.Contains(err.Error(), "exceeds max_bytes=4") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReadSandboxFileStreamsChunks(t *testing.T) {
	adapter := &stubAdapter{
		readFn: func(_ context.Context, _, path string, maxBytes int64, emit func([]byte) error) error {
			if got, want := path, "/tmp/result.txt"; got != want {
				t.Fatalf("unexpected path: got %q want %q", got, want)
			}
			if got, want := maxBytes, int64(99); got != want {
				t.Fatalf("unexpected max bytes: got %d want %d", got, want)
			}
			if err := emit([]byte("hello ")); err != nil {
				return err
			}
			return emit([]byte("world"))
		},
	}
	svc := newTestService(adapter)
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	var got []byte
	err = svc.ReadSandboxFile(context.Background(), &cleanroomv1.ReadSandboxFileRequest{
		SandboxId: createResp.GetSandbox().GetSandboxId(),
		Path:      "/tmp/result.txt",
		MaxBytes:  99,
	}, func(resp *cleanroomv1.ReadSandboxFileResponse) error {
		got = append(got, resp.GetData()...)
		return nil
	})
	if err != nil {
		t.Fatalf("ReadSandboxFile returned error: %v", err)
	}
	if string(got) != "hello world" {
		t.Fatalf("unexpected streamed data: %q", string(got))
	}
}

func TestWriteSandboxFileStreamsReader(t *testing.T) {
	adapter := &stubAdapter{
		writeFn: func(_ context.Context, _, path string, r io.Reader, mode fs.FileMode, _ time.Time) (int64, error) {
			if got, want := path, "/tmp/upload.txt"; got != want {
				t.Fatalf("unexpected path: got %q want %q", got, want)
			}
			if got, want := mode, fs.FileMode(0o600); got != want {
				t.Fatalf("unexpected mode: got %04o want %04o", got, want)
			}
			data, err := io.ReadAll(r)
			if err != nil {
				return 0, err
			}
			if got, want := string(data), "payload"; got != want {
				t.Fatalf("unexpected data: got %q want %q", got, want)
			}
			return int64(len(data)), nil
		},
	}
	svc := newTestService(adapter)
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	resp, err := svc.WriteSandboxFile(context.Background(), &cleanroomv1.WriteSandboxFileInit{
		SandboxId: createResp.GetSandbox().GetSandboxId(),
		Path:      "/tmp/upload.txt",
		Mode:      0o600,
	}, strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("WriteSandboxFile returned error: %v", err)
	}
	if got, want := resp.GetSizeBytes(), int64(len("payload")); got != want {
		t.Fatalf("unexpected size_bytes: got %d want %d", got, want)
	}
}

func TestSandboxPathPrimitiveDispatch(t *testing.T) {
	adapter := &stubAdapter{
		statFn: func(_ context.Context, _, path string) (*backend.SandboxPathInfo, error) {
			return &backend.SandboxPathInfo{Path: path, Type: backend.SandboxPathTypeFile, SizeBytes: 7, Mode: 0o4644}, nil
		},
		walkFn: func(_ context.Context, _, path string, emit func(backend.SandboxPathInfo) error) error {
			if err := emit(backend.SandboxPathInfo{Path: path, Type: backend.SandboxPathTypeDirectory, Mode: 0o1755}); err != nil {
				return err
			}
			return emit(backend.SandboxPathInfo{Path: path + "/file.txt", Type: backend.SandboxPathTypeFile, SizeBytes: 4, Mode: 0o2644})
		},
		removeFn: func(_ context.Context, _, path string, recursive bool) error {
			if got, want := path, "/tmp/remove"; got != want {
				t.Fatalf("unexpected remove path: got %q want %q", got, want)
			}
			if !recursive {
				t.Fatal("expected recursive remove")
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

	statResp, err := svc.StatSandboxPath(context.Background(), &cleanroomv1.StatSandboxPathRequest{SandboxId: sandboxID, Path: "/tmp/file.txt"})
	if err != nil {
		t.Fatalf("StatSandboxPath returned error: %v", err)
	}
	if got, want := statResp.GetInfo().GetType(), cleanroomv1.SandboxPathType_SANDBOX_PATH_TYPE_FILE; got != want {
		t.Fatalf("unexpected stat type: got %v want %v", got, want)
	}
	if got, want := statResp.GetInfo().GetMode(), uint32(0o4644); got != want {
		t.Fatalf("unexpected stat mode: got %04o want %04o", got, want)
	}

	var walked []string
	var walkedModes []string
	err = svc.WalkSandboxTree(context.Background(), &cleanroomv1.WalkSandboxTreeRequest{SandboxId: sandboxID, Path: "/tmp/tree"}, func(resp *cleanroomv1.WalkSandboxTreeResponse) error {
		walked = append(walked, resp.GetInfo().GetPath())
		walkedModes = append(walkedModes, fmt.Sprintf("%04o", resp.GetInfo().GetMode()))
		return nil
	})
	if err != nil {
		t.Fatalf("WalkSandboxTree returned error: %v", err)
	}
	if got, want := strings.Join(walked, ","), "/tmp/tree,/tmp/tree/file.txt"; got != want {
		t.Fatalf("unexpected walk paths: got %q want %q", got, want)
	}
	if got, want := strings.Join(walkedModes, ","), "1755,2644"; got != want {
		t.Fatalf("unexpected walk modes: got %q want %q", got, want)
	}

	if _, err := svc.RemoveSandboxPath(context.Background(), &cleanroomv1.RemoveSandboxPathRequest{SandboxId: sandboxID, Path: "/tmp/remove", Recursive: true}); err != nil {
		t.Fatalf("RemoveSandboxPath returned error: %v", err)
	}
}

func TestSandboxArchivePrimitiveDispatch(t *testing.T) {
	adapter := &stubAdapter{
		archiveFn: func(_ context.Context, _ string, paths []string, maxBytes int64, emit func([]byte) error) error {
			if got, want := strings.Join(paths, ","), "/tmp/a,/tmp/b"; got != want {
				t.Fatalf("unexpected archive paths: got %q want %q", got, want)
			}
			if got, want := maxBytes, int64(1234); got != want {
				t.Fatalf("unexpected max bytes: got %d want %d", got, want)
			}
			return emit([]byte("tar-data"))
		},
		extractFn: func(_ context.Context, _ string, destination string, r io.Reader) (int64, error) {
			if got, want := destination, "/tmp/out"; got != want {
				t.Fatalf("unexpected destination: got %q want %q", got, want)
			}
			data, err := io.ReadAll(r)
			if err != nil {
				return 0, err
			}
			if got, want := string(data), "tar-data"; got != want {
				t.Fatalf("unexpected archive data: got %q want %q", got, want)
			}
			return int64(len(data)), nil
		},
	}
	svc := newTestService(adapter)
	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()

	var archived []byte
	err = svc.ArchiveSandboxPaths(context.Background(), &cleanroomv1.ArchiveSandboxPathsRequest{
		SandboxId: sandboxID,
		Paths:     []string{"/tmp/a", "/tmp/b"},
		MaxBytes:  1234,
	}, func(resp *cleanroomv1.ArchiveSandboxPathsResponse) error {
		archived = append(archived, resp.GetData()...)
		return nil
	})
	if err != nil {
		t.Fatalf("ArchiveSandboxPaths returned error: %v", err)
	}
	if got, want := string(archived), "tar-data"; got != want {
		t.Fatalf("unexpected archive stream: got %q want %q", got, want)
	}

	resp, err := svc.ExtractSandboxArchive(context.Background(), &cleanroomv1.ExtractSandboxArchiveInit{
		SandboxId:   sandboxID,
		Destination: "/tmp/out",
	}, strings.NewReader("tar-data"))
	if err != nil {
		t.Fatalf("ExtractSandboxArchive returned error: %v", err)
	}
	if got, want := resp.GetSizeBytes(), int64(len("tar-data")); got != want {
		t.Fatalf("unexpected extract size: got %d want %d", got, want)
	}
}

func TestUnsupportedSandboxFileOperationsDoNotWakeSuspendedSandbox(t *testing.T) {
	tests := []struct {
		name string
		want string
		call func(*Service, string) error
	}{
		{
			name: "download",
			want: "does not support sandbox file downloads",
			call: func(svc *Service, sandboxID string) error {
				_, err := svc.DownloadSandboxFile(context.Background(), &cleanroomv1.DownloadSandboxFileRequest{
					SandboxId: sandboxID,
					Path:      "/tmp/file",
				})
				return err
			},
		},
		{
			name: "upload",
			want: "does not support sandbox file uploads",
			call: func(svc *Service, sandboxID string) error {
				_, err := svc.UploadSandboxFile(context.Background(), &cleanroomv1.UploadSandboxFileRequest{
					SandboxId: sandboxID,
					Path:      "/tmp/file",
					Data:      []byte("payload"),
				})
				return err
			},
		},
		{
			name: "stat",
			want: "does not support sandbox path stat",
			call: func(svc *Service, sandboxID string) error {
				_, err := svc.StatSandboxPath(context.Background(), &cleanroomv1.StatSandboxPathRequest{
					SandboxId: sandboxID,
					Path:      "/tmp/file",
				})
				return err
			},
		},
		{
			name: "walk",
			want: "does not support sandbox tree walks",
			call: func(svc *Service, sandboxID string) error {
				return svc.WalkSandboxTree(context.Background(), &cleanroomv1.WalkSandboxTreeRequest{
					SandboxId: sandboxID,
					Path:      "/tmp/tree",
				}, nil)
			},
		},
		{
			name: "read",
			want: "does not support sandbox file reads",
			call: func(svc *Service, sandboxID string) error {
				return svc.ReadSandboxFile(context.Background(), &cleanroomv1.ReadSandboxFileRequest{
					SandboxId: sandboxID,
					Path:      "/tmp/file",
				}, nil)
			},
		},
		{
			name: "write",
			want: "does not support sandbox file writes",
			call: func(svc *Service, sandboxID string) error {
				_, err := svc.WriteSandboxFile(context.Background(), &cleanroomv1.WriteSandboxFileInit{
					SandboxId: sandboxID,
					Path:      "/tmp/file",
				}, strings.NewReader("payload"))
				return err
			},
		},
		{
			name: "remove",
			want: "does not support sandbox path removal",
			call: func(svc *Service, sandboxID string) error {
				_, err := svc.RemoveSandboxPath(context.Background(), &cleanroomv1.RemoveSandboxPathRequest{
					SandboxId: sandboxID,
					Path:      "/tmp/file",
				})
				return err
			},
		},
		{
			name: "archive",
			want: "does not support sandbox archive reads",
			call: func(svc *Service, sandboxID string) error {
				return svc.ArchiveSandboxPaths(context.Background(), &cleanroomv1.ArchiveSandboxPathsRequest{
					SandboxId: sandboxID,
					Paths:     []string{"/tmp/file"},
				}, nil)
			},
		},
		{
			name: "extract",
			want: "does not support sandbox archive writes",
			call: func(svc *Service, sandboxID string) error {
				_, err := svc.ExtractSandboxArchive(context.Background(), &cleanroomv1.ExtractSandboxArchiveInit{
					SandboxId:   sandboxID,
					Destination: "/tmp/out",
				}, strings.NewReader("payload"))
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter := &suspendOnlyAdapter{
				resumeFn: func(context.Context, string) error {
					return errors.New("unexpected wake")
				},
			}
			svc := newTestService(adapter)
			createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
			if err != nil {
				t.Fatalf("CreateSandbox returned error: %v", err)
			}
			sandboxID := createResp.GetSandbox().GetSandboxId()
			if _, err := svc.SuspendSandbox(context.Background(), &cleanroomv1.SuspendSandboxRequest{SandboxId: sandboxID}); err != nil {
				t.Fatalf("SuspendSandbox returned error: %v", err)
			}

			err = tt.call(svc, sandboxID)
			if err == nil {
				t.Fatal("expected unsupported operation error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("unexpected error: got %v want substring %q", err, tt.want)
			}
			if got, want := adapter.resumeCalls, 0; got != want {
				t.Fatalf("unexpected resume calls: got %d want %d", got, want)
			}
			getResp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
			if err != nil {
				t.Fatalf("GetSandbox returned error: %v", err)
			}
			if got, want := getResp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_SUSPENDED; got != want {
				t.Fatalf("unexpected sandbox status: got %v want %v", got, want)
			}
		})
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

func TestCreateExecutionRejectsWhileSandboxFileTransferInProgress(t *testing.T) {
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

func TestTerminateSandboxSchedulesStorageCleanupAsync(t *testing.T) {
	adapter := &stubAdapter{}
	svc := newTestService(adapter)

	started := make(chan string, 1)
	release := make(chan struct{})
	defer close(release)
	svc.runtime.terminatedSandboxStorageCleanup = func(sandboxID string) {
		started <- sandboxID
		<-release
	}

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()

	done := make(chan error, 1)
	go func() {
		_, err := svc.TerminateSandbox(context.Background(), &cleanroomv1.TerminateSandboxRequest{SandboxId: sandboxID})
		done <- err
	}()

	select {
	case got := <-started:
		if got != sandboxID {
			t.Fatalf("unexpected cleanup sandbox id: got %q want %q", got, sandboxID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for storage cleanup to start")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("TerminateSandbox returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("TerminateSandbox blocked behind storage cleanup")
	}
}

func TestTerminateSandboxStorageCleanupWorkerRunsSeriallyAndDedupes(t *testing.T) {
	adapter := &stubAdapter{}
	svc := newTestService(adapter)
	svc.runtime.storageCleanupQueueSize = 4

	started := make(chan string, 3)
	finished := make(chan string, 3)
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() {
		releaseOnce.Do(func() {
			close(release)
		})
	}
	defer closeRelease()

	svc.runtime.terminatedSandboxStorageCleanup = func(sandboxID string) {
		started <- sandboxID
		<-release
		finished <- sandboxID
	}

	svc.scheduleTerminatedSandboxStorageCleanup("sandbox-1")
	svc.scheduleTerminatedSandboxStorageCleanup("sandbox-1")
	svc.scheduleTerminatedSandboxStorageCleanup("sandbox-2")

	select {
	case got := <-started:
		if got != "sandbox-1" {
			t.Fatalf("unexpected first cleanup: got %q want sandbox-1", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first storage cleanup")
	}
	select {
	case got := <-started:
		t.Fatalf("storage cleanup worker started %q while first cleanup was still running", got)
	case <-time.After(100 * time.Millisecond):
	}

	closeRelease()

	select {
	case got := <-finished:
		if got != "sandbox-1" {
			t.Fatalf("unexpected first completed cleanup: got %q want sandbox-1", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first storage cleanup to finish")
	}
	select {
	case got := <-started:
		if got != "sandbox-2" {
			t.Fatalf("unexpected second cleanup: got %q want sandbox-2", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for second storage cleanup")
	}
	select {
	case got := <-finished:
		if got != "sandbox-2" {
			t.Fatalf("unexpected second completed cleanup: got %q want sandbox-2", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for second storage cleanup to finish")
	}
	select {
	case got := <-started:
		t.Fatalf("duplicate cleanup was not deduped, started %q", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestScheduleStartupStorageCleanupQueuesZFSImportCleanup(t *testing.T) {
	adapter := &stubAdapter{}
	svc := newTestService(adapter)
	svc.ZFSImportDatasetStore = &memoryZFSImportDatasetStore{}

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	svc.runtime.zfsImportDatasetStorageCleanup = func() {
		started <- struct{}{}
		<-release
	}

	svc.ScheduleStartupStorageCleanup()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for zfs import storage cleanup to start")
	}
}

func TestZFSImportDatasetStorageCleanupDestroysOnlyUnreferencedImports(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpDir, "state-home"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmpDir, "cache-home"))

	snapshotStore := newMemorySnapshotStore()
	if err := snapshotStore.Create(context.Background(), snapshotstore.Record{
		SnapshotID: "snap-import",
		Backend:    "firecracker",
		StorageRef: "tank/cleanroom/snapshots/imports/protected-snapshot@base",
	}); err != nil {
		t.Fatalf("create snapshot record: %v", err)
	}
	cacheStore := newMemoryCacheStore()
	if err := cacheStore.Upsert(context.Background(), cachestore.Record{
		Stage:      "dependencies",
		CacheKey:   "cache-import",
		Backend:    "firecracker",
		StorageRef: "tank/cleanroom/snapshots/imports/protected-cache@base",
	}); err != nil {
		t.Fatalf("create cache record: %v", err)
	}
	zfsStore := &memoryZFSImportDatasetStore{datasets: []string{
		"tank/cleanroom/snapshots/imports/protected-snapshot",
		"tank/cleanroom/snapshots/imports/protected-cache",
		"tank/cleanroom/snapshots/imports/stale",
	}}
	svc := newTestServiceWithSnapshotStore(&stubAdapter{}, snapshotStore)
	svc.CacheStore = cacheStore
	svc.ZFSImportDatasetStore = zfsStore

	svc.cleanupZFSImportDatasets()

	zfsStore.mu.Lock()
	defer zfsStore.mu.Unlock()
	if got, want := zfsStore.destroyed, []string{"tank/cleanroom/snapshots/imports/stale"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("unexpected destroyed datasets: got %v want %v", got, want)
	}
}

func TestTerminateSandboxCleansUpRuntimeDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	stateHome := filepath.Join(tmpDir, "state-home")
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmpDir, "cache-home"))

	adapter := &stubAdapter{}
	svc := newTestService(adapter)

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()
	runtimeDir := filepath.Join(stateHome, "cleanroom", "sandboxes", sandboxID)
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatalf("create runtime dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "rootfs.ext4"), []byte("runtime"), 0o644); err != nil {
		t.Fatalf("write runtime file: %v", err)
	}

	if _, err := svc.TerminateSandbox(context.Background(), &cleanroomv1.TerminateSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("TerminateSandbox returned error: %v", err)
	}
	waitForPathRemoved(t, runtimeDir)

	getResp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := getResp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED; got != want {
		t.Fatalf("unexpected sandbox status: got %v want %v", got, want)
	}
}

func waitForPathRemoved(t *testing.T, path string) {
	t.Helper()

	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		_, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}

		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s to be removed", path)
		case <-ticker.C:
		}
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
	if got, want := parsedSandboxID.Prefix(), ""; got != want {
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

func TestAppendRetainedOutputSanitizesInvalidUTF8(t *testing.T) {
	got := appendRetainedOutput("", string([]byte{'o', 0xff, 'k'}), 32)
	if !utf8.ValidString(got) {
		t.Fatalf("expected retained output to be valid UTF-8, got %q", got)
	}
	if want := "o\uFFFDk"; got != want {
		t.Fatalf("unexpected retained output: got %q want %q", got, want)
	}
}

func TestAppendRetainedOutputTruncatesOnUTF8Boundary(t *testing.T) {
	got := appendRetainedOutput("ab\u20AC", "cd", 5)
	if !utf8.ValidString(got) {
		t.Fatalf("expected retained output to be valid UTF-8, got %q", got)
	}
	if want := "\u20ACcd"; got != want {
		t.Fatalf("unexpected retained output: got %q want %q", got, want)
	}
}

func TestAppendRetainedOutputBytesPreservesSplitUTF8(t *testing.T) {
	var pending []byte
	got, pending := appendRetainedOutputBytes("", pending, []byte{0xe2}, 32)
	if got != "" {
		t.Fatalf("expected incomplete rune to stay pending, got %q", got)
	}
	if want := []byte{0xe2}; !bytes.Equal(pending, want) {
		t.Fatalf("unexpected pending bytes: got %v want %v", pending, want)
	}

	got, pending = appendRetainedOutputBytes(got, pending, []byte{0x82, 0xac}, 32)
	if len(pending) != 0 {
		t.Fatalf("expected pending bytes to be consumed, got %v", pending)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("expected retained output to be valid UTF-8, got %q", got)
	}
	if want := "\u20AC"; got != want {
		t.Fatalf("unexpected retained output: got %q want %q", got, want)
	}
}

func TestFlushRetainedOutputPendingSanitizesIncompleteUTF8(t *testing.T) {
	got, pending := flushRetainedOutputPending("er", []byte{0xe2, 0x82}, 32)
	if len(pending) != 0 {
		t.Fatalf("expected pending bytes to be cleared, got %v", pending)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("expected retained output to be valid UTF-8, got %q", got)
	}
	if want := "er\uFFFD"; got != want {
		t.Fatalf("unexpected retained output: got %q want %q", got, want)
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

func TestListSandboxesIncludesProvisioningSandbox(t *testing.T) {
	provisionStarted := make(chan struct{})
	releaseProvision := make(chan struct{})
	adapter := &stubAdapter{
		provisionFn: func(context.Context, backend.ProvisionRequest) error {
			close(provisionStarted)
			<-releaseProvision
			return nil
		},
	}
	svc := newTestService(adapter)

	createDone := make(chan error, 1)
	go func() {
		_, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
		createDone <- err
	}()

	<-provisionStarted
	resp, err := svc.ListSandboxes(context.Background(), &cleanroomv1.ListSandboxesRequest{})
	if err != nil {
		t.Fatalf("ListSandboxes returned error: %v", err)
	}
	sandboxes := resp.GetSandboxes()
	if got, want := len(sandboxes), 1; got != want {
		t.Fatalf("unexpected sandbox count: got %d want %d", got, want)
	}
	if got, want := sandboxes[0].GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_PROVISIONING; got != want {
		t.Fatalf("unexpected sandbox status: got %v want %v", got, want)
	}

	close(releaseProvision)
	if err := <-createDone; err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
}

func TestCreateSandboxAbortsWhenTerminatedDuringProvision(t *testing.T) {
	provisionStarted := make(chan struct{})
	releaseProvision := make(chan struct{})
	adapter := &stubAdapter{
		provisionFn: func(context.Context, backend.ProvisionRequest) error {
			close(provisionStarted)
			<-releaseProvision
			return nil
		},
	}
	svc := newTestService(adapter)

	createDone := make(chan error, 1)
	go func() {
		_, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
		createDone <- err
	}()

	<-provisionStarted
	sandboxID := requireProvisioningSandboxID(t, svc)
	if _, err := svc.TerminateSandbox(context.Background(), &cleanroomv1.TerminateSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("TerminateSandbox returned error: %v", err)
	}
	close(releaseProvision)
	if err := <-createDone; !errors.Is(err, errSandboxCreateAborted) {
		t.Fatalf("CreateSandbox error = %v, want %v", err, errSandboxCreateAborted)
	}

	resp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := resp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED; got != want {
		t.Fatalf("unexpected sandbox status: got %v want %v", got, want)
	}
	if got, want := adapter.terminateCalls, 1; got != want {
		t.Fatalf("unexpected terminate calls: got %d want %d", got, want)
	}
}

func TestCreateSandboxFromSnapshotAbortsWhenTerminatedDuringRestore(t *testing.T) {
	store := newMemorySnapshotStore()
	restoreStarted := make(chan struct{})
	releaseRestore := make(chan struct{})
	adapter := &stubAdapter{
		createSnapshotFn: func(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
			return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
		},
		provisionFromSnapshotFn: func(context.Context, backend.ProvisionFromSnapshotRequest) error {
			close(restoreStarted)
			<-releaseRestore
			return nil
		},
	}
	svc := newTestServiceWithSnapshotStore(adapter, store)

	sourceResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	snapshotResp, err := svc.CreateSnapshot(context.Background(), &cleanroomv1.CreateSnapshotRequest{
		SandboxId: sourceResp.GetSandbox().GetSandboxId(),
		Name:      "base",
	})
	if err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}

	createDone := make(chan error, 1)
	go func() {
		_, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
			Source: &cleanroomv1.CreateSandboxRequest_SnapshotId{SnapshotId: snapshotResp.GetSnapshot().GetSnapshotId()},
		})
		createDone <- err
	}()

	<-restoreStarted
	sandboxID := requireProvisioningSandboxID(t, svc)
	if _, err := svc.TerminateSandbox(context.Background(), &cleanroomv1.TerminateSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("TerminateSandbox returned error: %v", err)
	}
	close(releaseRestore)
	if err := <-createDone; !errors.Is(err, errSandboxCreateAborted) {
		t.Fatalf("CreateSandbox from snapshot error = %v, want %v", err, errSandboxCreateAborted)
	}

	resp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := resp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED; got != want {
		t.Fatalf("unexpected sandbox status: got %v want %v", got, want)
	}
	if got, want := adapter.provisionFromSnapshotCalls, 1; got != want {
		t.Fatalf("unexpected provision-from-snapshot calls: got %d want %d", got, want)
	}
	if got, want := adapter.terminateCalls, 1; got != want {
		t.Fatalf("unexpected terminate calls: got %d want %d", got, want)
	}
}

func requireProvisioningSandboxID(t *testing.T, svc *Service) string {
	t.Helper()
	resp, err := svc.ListSandboxes(context.Background(), &cleanroomv1.ListSandboxesRequest{})
	if err != nil {
		t.Fatalf("ListSandboxes returned error: %v", err)
	}
	for _, sandbox := range resp.GetSandboxes() {
		if sandbox.GetStatus() == cleanroomv1.SandboxStatus_SANDBOX_STATUS_PROVISIONING {
			return sandbox.GetSandboxId()
		}
	}
	t.Fatalf("no provisioning sandbox in %d sandboxes", len(resp.GetSandboxes()))
	return ""
}

func TestExecutionRetentionBoundsOutput(t *testing.T) {
	adapter := &stubAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
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
}

func TestExecutionRetentionSanitizesInvalidUTF8Output(t *testing.T) {
	adapter := &stubAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if stream.OnStdout != nil {
				stream.OnStdout([]byte{'o', 0xff, 'k'})
				stream.OnStdout([]byte{' ', 0xe2})
				stream.OnStdout([]byte{0x82, 0xac})
			}
			if stream.OnStderr != nil {
				stream.OnStderr([]byte{'e', 'r', 0xe2, 0x82})
			}
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    0,
				Message:     "ok",
			}, nil
		},
	}
	svc := newTestService(adapter)
	retention := testRetentionPolicy()
	retention.maxRetainedExecutionOutputBytes = 32
	svc.runtime.retention = retention

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()
	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"emit", "invalid-utf8"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()
	if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}

	inspect, err := svc.InspectExecution(context.Background(), &cleanroomv1.InspectExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
	})
	if err != nil {
		t.Fatalf("InspectExecution returned error: %v", err)
	}
	if !utf8.ValidString(inspect.GetStdout()) || !utf8.ValidString(inspect.GetStderr()) {
		t.Fatalf("expected retained output to be valid UTF-8, stdout=%q stderr=%q", inspect.GetStdout(), inspect.GetStderr())
	}
	if got, want := inspect.GetStdout(), "o\uFFFDk \u20AC"; got != want {
		t.Fatalf("unexpected retained stdout: got %q want %q", got, want)
	}
	if got, want := inspect.GetStderr(), "er\uFFFD"; got != want {
		t.Fatalf("unexpected retained stderr: got %q want %q", got, want)
	}
	if _, err := proto.Marshal(inspect); err != nil {
		t.Fatalf("marshal InspectExecutionResponse: %v", err)
	}
}

func TestExecutionRetentionZeroesSnapshotOutputButKeepsEvents(t *testing.T) {
	adapter := &stubAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("stdout"))
			}
			if stream.OnStderr != nil {
				stream.OnStderr([]byte("stderr"))
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

	snapshot, err := svc.ExecutionSnapshot(sandboxID, executionID)
	if err != nil {
		t.Fatalf("ExecutionSnapshot returned error: %v", err)
	}
	if got := snapshot.Stdout; got != "" {
		t.Fatalf("expected retained stdout to be empty, got %q", got)
	}
	if got := snapshot.Stderr; got != "" {
		t.Fatalf("expected retained stderr to be empty, got %q", got)
	}

	history, _, _, unsubscribe, err := svc.SubscribeExecutionEvents(sandboxID, executionID)
	if err != nil {
		t.Fatalf("SubscribeExecutionEvents returned error: %v", err)
	}
	defer unsubscribe()
	var sawStdout, sawStderr bool
	for _, event := range history {
		if string(event.GetStdout()) == "stdout" {
			sawStdout = true
		}
		if string(event.GetStderr()) == "stderr" {
			sawStderr = true
		}
	}
	if !sawStdout || !sawStderr {
		t.Fatalf("expected stream events to be retained, saw stdout=%t stderr=%t", sawStdout, sawStderr)
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

func waitExecutionDone(t *testing.T, svc *Service, sandboxID, executionID string) {
	t.Helper()
	key := executionKey(sandboxID, executionID)
	svc.mu.RLock()
	ex := svc.executions[key]
	svc.mu.RUnlock()
	if ex == nil {
		t.Fatalf("unknown execution %s", executionID)
	}
	select {
	case <-ex.Done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for execution %s", executionID)
	}
}
