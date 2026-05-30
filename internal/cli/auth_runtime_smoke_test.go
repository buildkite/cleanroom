package cli

import (
	"context"
	"crypto/rsa"
	"encoding/pem"
	"io"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/buildkite/cleanroom/internal/authconfig"
	"github.com/buildkite/cleanroom/internal/authz"
	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/controlclient"
	"github.com/buildkite/cleanroom/internal/controlserver"
	"github.com/buildkite/cleanroom/internal/controlservice"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"github.com/buildkite/cleanroom/internal/snapshotstore"
	"github.com/golang-jwt/jwt/v5"
)

func TestAuthRuntimeSmokeExactPrincipalIsolation(t *testing.T) {
	fixture := newAuthRuntimeSmokeFixture(t)

	missingTokenOutcome := authRuntimeSmokeRun(t, func(ctx *runtimeContext) error {
		return (&SandboxListCommand{
			clientFlags: fixture.noTokenFlags,
		}).Run(ctx)
	})
	requireAuthSmokeConnectCode(t, missingTokenOutcome, connect.CodeUnauthenticated)

	aliceSandboxID := authRuntimeSmokeCreateSandbox(t, fixture.aliceFlags)
	bobSandboxID := authRuntimeSmokeCreateSandbox(t, fixture.bobFlags)

	authRuntimeSmokeRequireSandboxList(t, fixture.aliceFlags, []string{aliceSandboxID}, []string{bobSandboxID})
	authRuntimeSmokeRequireSandboxList(t, fixture.bobFlags, []string{bobSandboxID}, []string{aliceSandboxID})
	authRuntimeSmokeRequireAllowed(t, authRuntimeSmokeRun(t, func(ctx *runtimeContext) error {
		return (&SandboxInspectCommand{
			clientFlags: fixture.aliceFlags,
			SandboxID:   aliceSandboxID,
		}).Run(ctx)
	}))
	requireAuthSmokeConnectCode(t, authRuntimeSmokeRun(t, func(ctx *runtimeContext) error {
		return (&SandboxInspectCommand{
			clientFlags: fixture.bobFlags,
			SandboxID:   aliceSandboxID,
		}).Run(ctx)
	}), connect.CodePermissionDenied)

	authRuntimeSmokeRequireAllowed(t, authRuntimeSmokeRun(t, func(ctx *runtimeContext) error {
		return (&ExecCommand{
			clientFlags: fixture.aliceFlags,
			In:          aliceSandboxID,
			Command:     []string{"true"},
		}).Run(ctx)
	}))
	aliceExecutionID := fixture.adapter.lastExecutionID()
	if aliceExecutionID == "" {
		t.Fatal("expected alice execution id")
	}
	authRuntimeSmokeRequireAllowed(t, authRuntimeSmokeRun(t, func(ctx *runtimeContext) error {
		return (&ExecutionInspectCommand{
			clientFlags: fixture.aliceFlags,
			SandboxID:   aliceSandboxID,
			ExecutionID: aliceExecutionID,
		}).Run(ctx)
	}))
	requireAuthSmokeConnectCode(t, authRuntimeSmokeRun(t, func(ctx *runtimeContext) error {
		return (&ExecCommand{
			clientFlags: fixture.bobFlags,
			In:          aliceSandboxID,
			Command:     []string{"true"},
		}).Run(ctx)
	}), connect.CodePermissionDenied)
	requireAuthSmokeConnectCode(t, authRuntimeSmokeRun(t, func(ctx *runtimeContext) error {
		return (&ExecutionInspectCommand{
			clientFlags: fixture.bobFlags,
			SandboxID:   aliceSandboxID,
			ExecutionID: aliceExecutionID,
		}).Run(ctx)
	}), connect.CodePermissionDenied)

	localInput := filepath.Join(t.TempDir(), "owned.txt")
	if err := os.WriteFile(localInput, []byte("owned by alice\n"), 0o644); err != nil {
		t.Fatalf("write local input: %v", err)
	}
	authRuntimeSmokeRequireAllowed(t, authRuntimeSmokeRun(t, func(ctx *runtimeContext) error {
		return (&CopyCommand{
			clientFlags: fixture.aliceFlags,
			Source:      localInput,
			Destination: aliceSandboxID + ":/tmp/owned.txt",
		}).Run(ctx)
	}))
	localOutput := filepath.Join(t.TempDir(), "downloaded.txt")
	authRuntimeSmokeRequireAllowed(t, authRuntimeSmokeRun(t, func(ctx *runtimeContext) error {
		return (&CopyCommand{
			clientFlags: fixture.aliceFlags,
			Source:      aliceSandboxID + ":/tmp/owned.txt",
			Destination: localOutput,
		}).Run(ctx)
	}))
	if got, want := mustReadFile(t, localOutput), "owned by alice\n"; got != want {
		t.Fatalf("unexpected copied file content: got %q want %q", got, want)
	}
	requireAuthSmokeConnectCode(t, authRuntimeSmokeRun(t, func(ctx *runtimeContext) error {
		return (&CopyCommand{
			clientFlags: fixture.bobFlags,
			Source:      aliceSandboxID + ":/tmp/owned.txt",
			Destination: filepath.Join(t.TempDir(), "stolen.txt"),
		}).Run(ctx)
	}), connect.CodePermissionDenied)
	requireAuthSmokeConnectCode(t, authRuntimeSmokeRun(t, func(ctx *runtimeContext) error {
		return (&CopyCommand{
			clientFlags: fixture.bobFlags,
			Source:      localInput,
			Destination: aliceSandboxID + ":/tmp/bob.txt",
		}).Run(ctx)
	}), connect.CodePermissionDenied)
	requireAuthSmokeConnectCode(t, execOutcome{
		err: authRuntimeSmokeReadSandboxFile(t, fixture.bobFlags, aliceSandboxID, "/tmp/owned.txt"),
	}, connect.CodePermissionDenied)
	requireAuthSmokeConnectCode(t, execOutcome{
		err: authRuntimeSmokeWriteSandboxFile(t, fixture.bobFlags, aliceSandboxID, "/tmp/bob-direct.txt", "nope\n"),
	}, connect.CodePermissionDenied)

	aliceSnapshotID := authRuntimeSmokeCreateSnapshot(t, fixture.aliceFlags, aliceSandboxID)
	authRuntimeSmokeRequireAllowed(t, authRuntimeSmokeRun(t, func(ctx *runtimeContext) error {
		return (&SnapshotInspectCommand{
			clientFlags: fixture.aliceFlags,
			SnapshotID:  aliceSnapshotID,
		}).Run(ctx)
	}))
	authRuntimeSmokeRequireSnapshotList(t, fixture.aliceFlags, []string{aliceSnapshotID}, nil)
	authRuntimeSmokeRequireSnapshotList(t, fixture.bobFlags, nil, []string{aliceSnapshotID})
	requireAuthSmokeConnectCode(t, authRuntimeSmokeRun(t, func(ctx *runtimeContext) error {
		return (&SnapshotInspectCommand{
			clientFlags: fixture.bobFlags,
			SnapshotID:  aliceSnapshotID,
		}).Run(ctx)
	}), connect.CodePermissionDenied)
	requireAuthSmokeConnectCode(t, authRuntimeSmokeRun(t, func(ctx *runtimeContext) error {
		return (&SandboxCreateCommand{
			clientFlags: fixture.bobFlags,
			From:        aliceSnapshotID,
		}).Run(ctx)
	}), connect.CodePermissionDenied)
}

type authRuntimeSmokeFixture struct {
	noTokenFlags clientFlags
	aliceFlags   clientFlags
	bobFlags     clientFlags
	adapter      *authRuntimeSmokeAdapter
}

func newAuthRuntimeSmokeFixture(t *testing.T) authRuntimeSmokeFixture {
	t.Helper()

	restoreResolver := stubPolicyUpdateResolver(t, func(_ context.Context, source string) (string, error) {
		if got, want := source, defaultBumpRefSource; got != want {
			t.Fatalf("unexpected default sandbox image source: got %q want %q", got, want)
		}
		return testImageOverrideRef, nil
	})
	t.Cleanup(restoreResolver)

	key := mustCLIAuthRSAKey(t)
	jwks := cliAuthJWKSServer(t, "smoke-kid", key)
	issuer := "https://issuer.cleanroom-smoke.test"
	validator, err := authz.NewOIDCValidator([]authconfig.OIDCIssuerConfig{{
		Name:                    "smoke",
		Issuer:                  issuer,
		Audiences:               []string{"cleanroom-smoke"},
		JWKSURL:                 jwks.URL,
		RequiredClaims:          map[string]string{"cleanroom_smoke": "true"},
		ClockSkewSeconds:        60,
		MaxTokenLifetimeSeconds: 3600,
	}})
	if err != nil {
		t.Fatalf("NewOIDCValidator returned error: %v", err)
	}
	policy, err := authz.CompilePolicy(authz.Policy{Bindings: []authz.Binding{{
		Name: "smoke-bots",
		When: `token.issuer == "smoke" && claims.cleanroom_smoke == "true"`,
		Principal: authz.PrincipalTemplate{
			ID:    "oidc:${token.issuer}:bot:${claims.bot_id}",
			Scope: "smoke:${claims.bot_id}",
		},
		Grants: []authz.Grant{{
			Name: "owned-resources",
			Actions: []string{
				"sandbox.create",
				"sandbox.get",
				"sandbox.list",
				"sandbox.terminate",
				"sandbox.file.read",
				"sandbox.file.write",
				"sandbox.file.stat",
				"execution.create",
				"execution.get",
				"execution.list",
				"execution.attach",
				"execution.inspect",
				"execution.stream",
				"execution.stdin.write",
				"execution.stdin.close",
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
	if err != nil {
		t.Fatalf("CompilePolicy returned error: %v", err)
	}

	adapter := &authRuntimeSmokeAdapter{
		files: map[string]map[string]authRuntimeSmokeFile{},
	}
	store, err := snapshotstore.New(snapshotstore.Options{
		MetadataDBPath: filepath.Join(t.TempDir(), "snapshots.db"),
	})
	if err != nil {
		t.Fatalf("create snapshot store: %v", err)
	}
	svc := &controlservice.Service{
		Loader: integrationLoader{},
		Config: runtimeconfig.Config{
			DefaultBackend: "firecracker",
			Backends: runtimeconfig.Backends{Firecracker: runtimeconfig.FirecrackerConfig{
				Snapshots: runtimeconfig.SnapshotConfig{Enabled: true, Driver: "file"},
			}},
		},
		SnapshotStore: store,
		Backends: map[string]backend.Adapter{
			"firecracker": adapter,
		},
	}
	auth := controlserver.BearerAuthenticator{
		Validator: validator,
		Policy:    policy,
	}
	httpServer := httptest.NewTLSServer(controlserver.New(svc, nil, auth.Interceptor()).Handler())
	t.Cleanup(httpServer.Close)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: httpServer.Certificate().Raw,
	})
	if err := os.WriteFile(caPath, caPEM, 0o644); err != nil {
		t.Fatalf("write test CA: %v", err)
	}

	tokenDir := t.TempDir()
	return authRuntimeSmokeFixture{
		noTokenFlags: clientFlags{Host: httpServer.URL, TLSCA: caPath},
		aliceFlags:   clientFlags{Host: httpServer.URL, TLSCA: caPath, AuthTokenFile: authRuntimeSmokeWriteToken(t, tokenDir, key, "smoke-kid", issuer, "alice")},
		bobFlags:     clientFlags{Host: httpServer.URL, TLSCA: caPath, AuthTokenFile: authRuntimeSmokeWriteToken(t, tokenDir, key, "smoke-kid", issuer, "bob")},
		adapter:      adapter,
	}
}

func authRuntimeSmokeWriteToken(t *testing.T, dir string, key *rsa.PrivateKey, kid, issuer, botID string) string {
	t.Helper()

	now := time.Now().UTC().Truncate(time.Second)
	token := cliAuthSignToken(t, key, kid, jwt.MapClaims{
		"iss":             issuer,
		"sub":             "bot:" + botID,
		"aud":             []string{"cleanroom-smoke"},
		"iat":             jwt.NewNumericDate(now.Add(-time.Minute)),
		"nbf":             jwt.NewNumericDate(now.Add(-time.Minute)),
		"exp":             jwt.NewNumericDate(now.Add(30 * time.Minute)),
		"cleanroom_smoke": "true",
		"bot_id":          botID,
	})
	path := filepath.Join(dir, botID+".jwt")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write %s token: %v", botID, err)
	}
	return path
}

type authRuntimeSmokeAdapter struct {
	snapshotIntegrationAdapter

	fileMu sync.Mutex
	files  map[string]map[string]authRuntimeSmokeFile
}

type authRuntimeSmokeFile struct {
	data  []byte
	mode  fs.FileMode
	mtime time.Time
}

func (a *authRuntimeSmokeAdapter) lastExecutionID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.TrimSpace(a.runReq.ExecutionID)
}

func (a *authRuntimeSmokeAdapter) StatSandboxPath(_ context.Context, sandboxID, path string) (*backend.SandboxPathInfo, error) {
	a.fileMu.Lock()
	defer a.fileMu.Unlock()

	if path == "/" || path == "/tmp" {
		return &backend.SandboxPathInfo{
			Path:  path,
			Type:  backend.SandboxPathTypeDirectory,
			Mode:  0o755,
			MTime: time.Now(),
		}, nil
	}
	file, ok := a.files[sandboxID][path]
	if !ok {
		return nil, backend.NewSandboxPathNotFoundError(path)
	}
	return &backend.SandboxPathInfo{
		Path:      path,
		Type:      backend.SandboxPathTypeFile,
		SizeBytes: int64(len(file.data)),
		Mode:      file.mode,
		MTime:     file.mtime,
	}, nil
}

func (a *authRuntimeSmokeAdapter) ReadSandboxFile(_ context.Context, sandboxID, path string, maxBytes int64, emit func([]byte) error) error {
	a.fileMu.Lock()
	file, ok := a.files[sandboxID][path]
	data := append([]byte(nil), file.data...)
	a.fileMu.Unlock()
	if !ok {
		return backend.NewSandboxPathNotFoundError(path)
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		data = data[:maxBytes]
	}
	if len(data) == 0 || emit == nil {
		return nil
	}
	return emit(data)
}

func (a *authRuntimeSmokeAdapter) WriteSandboxFile(_ context.Context, sandboxID, path string, r io.Reader, mode fs.FileMode, mtime time.Time) (int64, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	if mtime.IsZero() {
		mtime = time.Now()
	}
	a.fileMu.Lock()
	defer a.fileMu.Unlock()
	if a.files[sandboxID] == nil {
		a.files[sandboxID] = map[string]authRuntimeSmokeFile{}
	}
	a.files[sandboxID][path] = authRuntimeSmokeFile{
		data:  append([]byte(nil), data...),
		mode:  mode,
		mtime: mtime,
	}
	return int64(len(data)), nil
}

func authRuntimeSmokeCreateSandbox(t *testing.T, flags clientFlags) string {
	t.Helper()
	outcome := authRuntimeSmokeRun(t, func(ctx *runtimeContext) error {
		return (&SandboxCreateCommand{
			clientFlags: flags,
			Backend:     "firecracker",
		}).Run(ctx)
	})
	authRuntimeSmokeRequireAllowed(t, outcome)
	sandboxID := strings.TrimSpace(outcome.stdout)
	if sandboxID == "" {
		t.Fatal("sandbox create returned empty sandbox id")
	}
	return sandboxID
}

func authRuntimeSmokeCreateSnapshot(t *testing.T, flags clientFlags, sandboxID string) string {
	t.Helper()
	outcome := authRuntimeSmokeRun(t, func(ctx *runtimeContext) error {
		return (&SnapshotCreateCommand{
			clientFlags: flags,
			SandboxID:   sandboxID,
		}).Run(ctx)
	})
	authRuntimeSmokeRequireAllowed(t, outcome)
	snapshotID := strings.TrimSpace(outcome.stdout)
	if snapshotID == "" {
		t.Fatal("snapshot create returned empty snapshot id")
	}
	return snapshotID
}

func authRuntimeSmokeReadSandboxFile(t *testing.T, flags clientFlags, sandboxID, path string) error {
	t.Helper()
	client := authRuntimeSmokeClient(t, flags)
	stream, err := client.ReadSandboxFile(context.Background(), &cleanroomv1.ReadSandboxFileRequest{
		SandboxId: sandboxID,
		Path:      path,
	})
	if err != nil {
		return err
	}
	for stream.Receive() {
	}
	return stream.Err()
}

func authRuntimeSmokeWriteSandboxFile(t *testing.T, flags clientFlags, sandboxID, path, data string) error {
	t.Helper()
	client := authRuntimeSmokeClient(t, flags)
	stream := client.WriteSandboxFile(context.Background())
	if err := stream.Send(&cleanroomv1.WriteSandboxFileRequest{
		Payload: &cleanroomv1.WriteSandboxFileRequest_Init{Init: &cleanroomv1.WriteSandboxFileInit{
			SandboxId: sandboxID,
			Path:      path,
			Mode:      0o644,
		}},
	}); err != nil {
		return err
	}
	if err := stream.Send(&cleanroomv1.WriteSandboxFileRequest{
		Payload: &cleanroomv1.WriteSandboxFileRequest_Data{Data: []byte(data)},
	}); err != nil {
		return err
	}
	_, err := stream.CloseAndReceive()
	return err
}

func authRuntimeSmokeClient(t *testing.T, flags clientFlags) *controlclient.Client {
	t.Helper()
	client, err := flags.connect(&runtimeContext{
		CWD:           t.TempDir(),
		Config:        runtimeconfig.Config{},
		Observability: newTestObservability(t),
	})
	if err != nil {
		t.Fatalf("connect auth runtime smoke client: %v", err)
	}
	return client
}

func authRuntimeSmokeRequireSandboxList(t *testing.T, flags clientFlags, want, unwanted []string) {
	t.Helper()
	outcome := authRuntimeSmokeRun(t, func(ctx *runtimeContext) error {
		return (&SandboxListCommand{
			clientFlags: flags,
			All:         true,
		}).Run(ctx)
	})
	authRuntimeSmokeRequireListOutput(t, outcome, want, unwanted)
}

func authRuntimeSmokeRequireSnapshotList(t *testing.T, flags clientFlags, want, unwanted []string) {
	t.Helper()
	outcome := authRuntimeSmokeRun(t, func(ctx *runtimeContext) error {
		return (&SnapshotListCommand{
			clientFlags: flags,
		}).Run(ctx)
	})
	authRuntimeSmokeRequireListOutput(t, outcome, want, unwanted)
}

func authRuntimeSmokeRequireListOutput(t *testing.T, outcome execOutcome, want, unwanted []string) {
	t.Helper()
	authRuntimeSmokeRequireAllowed(t, outcome)
	for _, id := range want {
		if !strings.Contains(outcome.stdout, id) {
			t.Fatalf("expected list output to include %q, got:\n%s", id, outcome.stdout)
		}
	}
	for _, id := range unwanted {
		if strings.Contains(outcome.stdout, id) {
			t.Fatalf("expected list output not to include %q, got:\n%s", id, outcome.stdout)
		}
	}
}

func authRuntimeSmokeRun(t *testing.T, runFn func(*runtimeContext) error) execOutcome {
	t.Helper()
	emptyStdin := ""
	outcome := runWithCapture(runFn, &emptyStdin, runtimeContext{
		CWD:           t.TempDir(),
		Loader:        integrationLoader{},
		Observability: newTestObservability(t),
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	return outcome
}

func authRuntimeSmokeRequireAllowed(t *testing.T, outcome execOutcome) {
	t.Helper()
	if outcome.err != nil {
		t.Fatalf("command returned error: %v\nstdout:\n%s\nstderr:\n%s", outcome.err, outcome.stdout, outcome.stderr)
	}
}

func requireAuthSmokeConnectCode(t *testing.T, outcome execOutcome, want connect.Code) {
	t.Helper()
	if outcome.err == nil {
		t.Fatalf("expected connect code %v, got nil error\nstdout:\n%s\nstderr:\n%s", want, outcome.stdout, outcome.stderr)
	}
	if got := connect.CodeOf(outcome.err); got != want {
		t.Fatalf("connect code = %v, want %v (err=%v)\nstdout:\n%s\nstderr:\n%s", got, want, outcome.err, outcome.stdout, outcome.stderr)
	}
	if want == connect.CodePermissionDenied && !strings.Contains(outcome.err.Error(), authz.ReasonOwnerMismatch) {
		t.Fatalf("permission denied error should include owner mismatch reason, got %v", outcome.err)
	}
}
