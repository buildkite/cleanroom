package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/alecthomas/kong"
	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/backend/darwinvz"
	"github.com/buildkite/cleanroom/internal/backend/firecracker"
	"github.com/buildkite/cleanroom/internal/controlclient"
	"github.com/buildkite/cleanroom/internal/controlserver"
	"github.com/buildkite/cleanroom/internal/controlservice"
	"github.com/buildkite/cleanroom/internal/endpoint"
	"github.com/buildkite/cleanroom/internal/gateway"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/imagemgr"
	"github.com/buildkite/cleanroom/internal/ociref"
	"github.com/buildkite/cleanroom/internal/paths"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"github.com/buildkite/cleanroom/internal/tlsbootstrap"
	"github.com/buildkite/cleanroom/internal/tlsconfig"
	"github.com/charmbracelet/log"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

const defaultBumpRefSource = "ghcr.io/buildkite/cleanroom-base/alpine:latest"

type policyLoader interface {
	LoadAndCompile(cwd string) (*policy.CompiledPolicy, string, error)
}

type runtimeContext struct {
	CWD        string
	Stdout     *os.File
	Loader     policyLoader
	Config     runtimeconfig.Config
	ConfigPath string
	Backends   map[string]backend.Adapter
}

type CLI struct {
	Policy  PolicyCommand  `cmd:"" help:"Policy commands"`
	Config  ConfigCommand  `cmd:"" help:"Runtime config commands"`
	Image   ImageCommand   `cmd:"" help:"Manage OCI image cache artifacts"`
	Exec    ExecCommand    `cmd:"" help:"Execute a command in a cleanroom backend"`
	Console ConsoleCommand `cmd:"" help:"Attach an interactive console to a cleanroom execution"`
	Serve   ServeCommand   `cmd:"" help:"Run the cleanroom control-plane server"`
	TLS     TLSCommand     `cmd:"" help:"Manage TLS certificates for mTLS"`
	Doctor  DoctorCommand  `cmd:"" help:"Run environment and backend diagnostics"`
	Status  StatusCommand  `cmd:"" help:"Inspect run artifacts"`
	Sandbox SandboxCommand `cmd:"" help:"Manage sandboxes"`
}

type ImageCommand struct {
	Pull    ImagePullCommand    `cmd:"" help:"Pull and cache a digest-pinned OCI image"`
	List    ImageListCommand    `name:"ls" aliases:"list" cmd:"" help:"List cached images"`
	Remove  ImageRemoveCommand  `name:"rm" aliases:"remove" cmd:"" help:"Remove a cached image by ref or digest"`
	Import  ImageImportCommand  `cmd:"" help:"Import a rootfs tar stream into the cache for a digest-pinned ref"`
	BumpRef ImageBumpRefCommand `name:"bump-ref" aliases:"set-ref" cmd:"" help:"Resolve an image tag to digest and update sandbox.image.ref in cleanroom policy"`
}

type ImagePullCommand struct {
	Ref string `arg:"" required:"" help:"Digest-pinned OCI reference (repo/image@sha256:...)"`
}

type ImageListCommand struct {
	JSON bool `help:"Print image records as JSON"`
}

type ImageRemoveCommand struct {
	Selector string `arg:"" required:"" help:"Image selector (ref, sha256:<digest>, or digest hex)"`
}

type ImageImportCommand struct {
	Ref     string `arg:"" required:"" help:"Digest-pinned OCI reference for this import"`
	TarPath string `arg:"" optional:"" help:"Tar/tar.gz path, or '-' for stdin (default: '-')"`
}

type ImageBumpRefCommand struct {
	Source     string `arg:"" optional:"" help:"Image ref to resolve (default: ghcr.io/buildkite/cleanroom-base/alpine:latest)"`
	Chdir      string `short:"c" help:"Change to this directory before running commands"`
	PolicyPath string `help:"Policy file path (default: cleanroom.yaml, or .buildkite/cleanroom.yaml when primary is missing)"`
}

type PolicyCommand struct {
	Validate PolicyValidateCommand `cmd:"" help:"Validate policy configuration"`
}

type PolicyValidateCommand struct {
	Chdir string `short:"c" help:"Change to this directory before running commands"`
	JSON  bool   `help:"Print compiled policy as JSON"`
}

type ConfigCommand struct {
	Init ConfigInitCommand `cmd:"" help:"Create a runtime config file with defaults"`
}

type ConfigInitCommand struct {
	Path           string `help:"Output path (default: $XDG_CONFIG_HOME/cleanroom/config.yaml)"`
	Force          bool   `help:"Overwrite existing config file"`
	DefaultBackend string `help:"Default backend value for config (firecracker|darwin-vz)"`
}

type clientFlags struct {
	Host     string `help:"Control-plane endpoint (unix://path, http://host:port, or https://host:port)"`
	LogLevel string `help:"Client log level (debug|info|warn|error)"`
	TLSCert  string `help:"Path to TLS client certificate (auto-discovered from XDG config for https)"`
	TLSKey   string `help:"Path to TLS client private key (auto-discovered from XDG config for https)"`
	TLSCA    string `help:"Path to CA certificate for server verification (auto-discovered from XDG config for https)"`
}

func (f *clientFlags) connect() (*controlclient.Client, error) {
	ep, err := endpoint.Resolve(f.Host)
	if err != nil {
		return nil, err
	}
	if err := validateClientEndpoint(ep); err != nil {
		return nil, err
	}
	return controlclient.New(ep, controlclient.WithTLS(tlsconfig.Options{
		CertPath: f.TLSCert,
		KeyPath:  f.TLSKey,
		CAPath:   f.TLSCA,
	}))
}

type ExecCommand struct {
	clientFlags
	Chdir     string `short:"c" help:"Change to this directory before running commands"`
	Backend   string `help:"Execution backend (defaults to runtime config or firecracker)"`
	SandboxID string `help:"Reuse an existing sandbox instead of creating a new one"`
	Remove    bool   `name:"rm" help:"Terminate a newly created sandbox after command completion"`

	LaunchSeconds int64 `help:"VM boot/guest-agent readiness timeout in seconds"`

	Command []string `arg:"" passthrough:"" required:"" help:"Command to execute"`
}

type ConsoleCommand struct {
	clientFlags
	Chdir     string `short:"c" help:"Change to this directory before running commands"`
	Backend   string `help:"Execution backend (defaults to runtime config or firecracker)"`
	SandboxID string `help:"Reuse an existing sandbox instead of creating a new one"`
	Remove    bool   `name:"rm" help:"Terminate a newly created sandbox after console exits"`

	LaunchSeconds int64 `help:"VM boot/guest-agent readiness timeout in seconds"`

	Command []string `arg:"" passthrough:"" optional:"" help:"Command to run in the console (default: sh)"`
}

type ServeCommand struct {
	Listen        string `help:"Listen endpoint for control API (defaults to runtime endpoint; supports tsnet://hostname[:port] and tssvc://service[:local-port])"`
	GatewayListen string `help:"Listen address for the host gateway (default :8170, use :0 for ephemeral port)"`
	LogLevel      string `help:"Server log level (debug|info|warn|error)"`
	TLSCert       string `help:"Path to TLS server certificate (auto-discovered from XDG config for https)"`
	TLSKey        string `help:"Path to TLS server private key (auto-discovered from XDG config for https)"`
	TLSCA         string `help:"Path to CA certificate for client verification (auto-discovered from XDG config for https)"`
}

type TLSCommand struct {
	Init  TLSInitCommand  `cmd:"" help:"Generate CA and server/client certificates"`
	Issue TLSIssueCommand `cmd:"" help:"Issue an additional certificate signed by the CA"`
}

type TLSInitCommand struct {
	Dir   string `help:"Output directory for TLS material (default: $XDG_CONFIG_HOME/cleanroom/tls)"`
	Force bool   `help:"Overwrite existing CA and certificates"`
}

type TLSIssueCommand struct {
	Name  string   `arg:"" required:"" help:"Common name for the certificate"`
	SAN   []string `help:"Subject alternative names (DNS names or IP addresses)"`
	Dir   string   `help:"TLS directory containing CA material (default: $XDG_CONFIG_HOME/cleanroom/tls)"`
	Force bool     `help:"Overwrite existing certificate and key files"`
}

type StatusCommand struct {
	RunID   string `help:"Run ID to inspect"`
	LastRun bool   `help:"Inspect the most recent run"`
}

type DoctorCommand struct {
	Chdir   string `short:"c" help:"Change to this directory before running commands"`
	Backend string `help:"Execution backend to diagnose (defaults to runtime config or firecracker)"`
	JSON    bool   `help:"Print doctor report as JSON"`
}

type SandboxCommand struct {
	List      SandboxListCommand      `name:"ls" aliases:"list" cmd:"" help:"List active sandboxes"`
	Terminate SandboxTerminateCommand `name:"rm" aliases:"terminate" cmd:"" help:"Terminate a sandbox"`
}

type SandboxListCommand struct {
	clientFlags
	JSON bool `help:"Print sandboxes as JSON"`
}

type SandboxTerminateCommand struct {
	clientFlags
	SandboxID string `arg:"" required:"" help:"Sandbox ID to terminate"`
}

type exitCodeError struct {
	code int
}

func (e exitCodeError) Error() string {
	return fmt.Sprintf("command failed with exit code %d", e.code)
}

func (e exitCodeError) ExitCode() int {
	return e.code
}

type hasExitCode interface {
	ExitCode() int
}

var (
	newSignalChannel = func() chan os.Signal {
		return make(chan os.Signal, 2)
	}
	notifySignals = func(ch chan os.Signal, sig ...os.Signal) {
		signal.Notify(ch, sig...)
	}
	stopSignals = func(ch chan os.Signal) {
		signal.Stop(ch)
	}
	resolveReferenceForPolicyUpdate = func(ctx context.Context, source string) (string, error) {
		resolved := strings.TrimSpace(source)
		if resolved == "" {
			resolved = defaultBumpRefSource
		}

		if parsed, err := ociref.ParseDigestReference(resolved); err == nil {
			return parsed.Original, nil
		}

		tag, err := name.NewTag(resolved, name.WeakValidation)
		if err != nil {
			return "", fmt.Errorf("parse image ref %q: %w", resolved, err)
		}

		desc, err := remote.Head(tag, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain))
		if err != nil {
			return "", fmt.Errorf("resolve image digest for %q: %w", resolved, err)
		}

		return fmt.Sprintf("%s@%s", tag.Context().Name(), desc.Digest.String()), nil
	}
)

func Run(args []string) error {
	cfg, cfgPath, err := runtimeconfig.Load()
	if err != nil {
		return err
	}

	runtimeCtx := &runtimeContext{
		Stdout:     os.Stdout,
		Loader:     policy.Loader{},
		Config:     cfg,
		ConfigPath: cfgPath,
		Backends: map[string]backend.Adapter{
			"firecracker": firecracker.New(),
			"darwin-vz":   darwinvz.New(),
		},
	}

	cli := CLI{}
	parser, err := kong.New(
		&cli,
		kong.Name("cleanroom"),
		kong.Description("Cleanroom CLI (MVP)"),
	)
	if err != nil {
		return err
	}

	ctx, err := parser.Parse(args)
	if err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	runtimeCtx.CWD = cwd

	return ctx.Run(runtimeCtx)
}

func ExitCode(err error) int {
	var codeErr hasExitCode
	if errors.As(err, &codeErr) {
		return codeErr.ExitCode()
	}
	return 1
}

func (c *PolicyValidateCommand) Run(ctx *runtimeContext) error {
	cwd, err := resolveCWD(ctx.CWD, c.Chdir)
	if err != nil {
		return err
	}
	compiled, source, err := ctx.Loader.LoadAndCompile(cwd)
	if err != nil {
		return err
	}

	if c.JSON {
		payload := map[string]any{
			"source": source,
			"policy": compiled,
		}
		enc := json.NewEncoder(ctx.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(payload)
	}

	_, err = fmt.Fprintf(ctx.Stdout, "policy valid: %s\npolicy hash: %s\n", source, compiled.Hash)
	return err
}

func (c *ConfigInitCommand) Run(ctx *runtimeContext) error {
	path := strings.TrimSpace(c.Path)
	if path == "" {
		resolved, err := runtimeconfig.Path()
		if err != nil {
			return err
		}
		path = resolved
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(ctx.CWD, path)
	}

	defaultBackend := strings.TrimSpace(c.DefaultBackend)
	if defaultBackend == "" {
		defaultBackend = hostDefaultBackend()
	}
	switch defaultBackend {
	case "firecracker", "darwin-vz":
	default:
		return fmt.Errorf("unsupported default backend %q (expected firecracker or darwin-vz)", defaultBackend)
	}

	if st, err := os.Stat(path); err == nil && !st.IsDir() && !c.Force {
		return fmt.Errorf("runtime config already exists at %s (use --force to overwrite)", path)
	} else if err == nil && st.IsDir() {
		return fmt.Errorf("runtime config path %s is a directory", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	payload, err := yaml.Marshal(defaultRuntimeConfig(defaultBackend))
	if err != nil {
		return fmt.Errorf("marshal runtime config template: %w", err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		return fmt.Errorf("write runtime config %s: %w", path, err)
	}

	_, err = fmt.Fprintf(ctx.Stdout, "runtime config written: %s\n", path)
	return err
}

func hostDefaultBackend() string {
	if runtime.GOOS == "darwin" {
		return "darwin-vz"
	}
	return "firecracker"
}

func defaultRuntimeConfig(defaultBackend string) runtimeconfig.Config {
	return runtimeconfig.Config{
		DefaultBackend: defaultBackend,
		Backends: runtimeconfig.Backends{
			Firecracker: runtimeconfig.FirecrackerConfig{
				BinaryPath:           "firecracker",
				KernelImage:          "",
				RootFS:               "",
				PrivilegedMode:       "sudo",
				PrivilegedHelperPath: "/usr/local/sbin/cleanroom-root-helper",
				VCPUs:                2,
				MemoryMiB:            1024,
				GuestCID:             3,
				GuestPort:            10700,
				LaunchSeconds:        30,
			},
			DarwinVZ: runtimeconfig.DarwinVZConfig{
				KernelImage:   "",
				RootFS:        "",
				VCPUs:         2,
				MemoryMiB:     1024,
				GuestPort:     10700,
				LaunchSeconds: 30,
			},
		},
	}
}

func newImageManager() (*imagemgr.Manager, error) {
	return imagemgr.New(imagemgr.Options{})
}

func (c *ImagePullCommand) Run(ctx *runtimeContext) error {
	mgr, err := newImageManager()
	if err != nil {
		return err
	}
	result, err := mgr.Pull(context.Background(), c.Ref)
	if err != nil {
		return err
	}

	status := "pulled"
	if result.CacheHit {
		status = "cached"
	}
	_, err = fmt.Fprintf(
		ctx.Stdout,
		"%s image\nref=%s\ndigest=%s\nrootfs=%s\nsize_bytes=%d\n",
		status,
		result.Record.Ref,
		result.Record.Digest,
		result.Record.RootFSPath,
		result.Record.SizeBytes,
	)
	return err
}

func (c *ImageListCommand) Run(ctx *runtimeContext) error {
	mgr, err := newImageManager()
	if err != nil {
		return err
	}
	items, err := mgr.List(context.Background())
	if err != nil {
		return err
	}

	if c.JSON {
		enc := json.NewEncoder(ctx.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(items)
	}

	if len(items) == 0 {
		_, err := fmt.Fprintln(ctx.Stdout, "no cached images")
		return err
	}

	tw := tabwriter.NewWriter(ctx.Stdout, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "DIGEST\tREF\tSIZE\tLAST_USED\tROOTFS"); err != nil {
		return err
	}
	for _, item := range items {
		if _, err := fmt.Fprintf(
			tw,
			"%s\t%s\t%d\t%s\t%s\n",
			item.Digest,
			item.Ref,
			item.SizeBytes,
			item.LastUsedAt.Format(time.RFC3339),
			item.RootFSPath,
		); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func (c *ImageRemoveCommand) Run(ctx *runtimeContext) error {
	mgr, err := newImageManager()
	if err != nil {
		return err
	}
	removed, err := mgr.Remove(context.Background(), c.Selector)
	if err != nil {
		return err
	}
	if len(removed) == 0 {
		_, err := fmt.Fprintf(ctx.Stdout, "no cached images match %q\n", c.Selector)
		return err
	}
	for _, item := range removed {
		if _, err := fmt.Fprintf(ctx.Stdout, "removed %s (%s)\n", item.Digest, item.Ref); err != nil {
			return err
		}
	}
	return nil
}

func (c *ImageImportCommand) Run(ctx *runtimeContext) error {
	mgr, err := newImageManager()
	if err != nil {
		return err
	}
	record, err := mgr.Import(context.Background(), c.Ref, c.TarPath, os.Stdin)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(
		ctx.Stdout,
		"imported image\nref=%s\ndigest=%s\nrootfs=%s\nsize_bytes=%d\n",
		record.Ref,
		record.Digest,
		record.RootFSPath,
		record.SizeBytes,
	)
	return err
}

func (c *ImageBumpRefCommand) Run(ctx *runtimeContext) error {
	cwd, err := resolveCWD(ctx.CWD, c.Chdir)
	if err != nil {
		return err
	}

	resolvedRef, err := resolveReferenceForPolicyUpdate(context.Background(), c.Source)
	if err != nil {
		return err
	}

	policyPath, err := resolvePolicyPathForUpdate(cwd, c.PolicyPath)
	if err != nil {
		return err
	}

	raw, err := os.ReadFile(policyPath)
	if err != nil {
		return fmt.Errorf("read policy %s: %w", policyPath, err)
	}

	updated, err := setSandboxImageRef(raw, resolvedRef)
	if err != nil {
		return fmt.Errorf("update policy %s: %w", policyPath, err)
	}

	info, err := os.Stat(policyPath)
	if err != nil {
		return fmt.Errorf("stat policy %s: %w", policyPath, err)
	}

	if err := os.WriteFile(policyPath, updated, info.Mode().Perm()); err != nil {
		return fmt.Errorf("write policy %s: %w", policyPath, err)
	}

	source := strings.TrimSpace(c.Source)
	if source == "" {
		source = defaultBumpRefSource
	}
	_, err = fmt.Fprintf(ctx.Stdout, "updated sandbox.image.ref\npolicy=%s\nsource=%s\nref=%s\n", policyPath, source, resolvedRef)
	return err
}

func (c *SandboxListCommand) Run(ctx *runtimeContext) error {
	client, err := c.connect()
	if err != nil {
		return err
	}

	resp, err := client.ListSandboxes(context.Background(), &cleanroomv1.ListSandboxesRequest{})
	if err != nil {
		return err
	}

	if c.JSON {
		enc := json.NewEncoder(ctx.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp.Sandboxes)
	}

	if len(resp.Sandboxes) == 0 {
		_, err := fmt.Fprintln(ctx.Stdout, "no active sandboxes")
		return err
	}

	tw := tabwriter.NewWriter(ctx.Stdout, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tSTATUS\tBACKEND\tCREATED"); err != nil {
		return err
	}
	for _, sb := range resp.Sandboxes {
		status := sandboxStatusString(sb.Status)
		created := ""
		if sb.CreatedAt != nil {
			created = sb.CreatedAt.AsTime().Format(time.RFC3339)
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", sb.SandboxId, status, sb.Backend, created); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func (c *SandboxTerminateCommand) Run(ctx *runtimeContext) error {
	client, err := c.connect()
	if err != nil {
		return err
	}

	resp, err := client.TerminateSandbox(context.Background(), &cleanroomv1.TerminateSandboxRequest{
		SandboxId: c.SandboxID,
	})
	if err != nil {
		return err
	}

	_, err = fmt.Fprintln(ctx.Stdout, resp.Message)
	return err
}

func (e *ExecCommand) Run(ctx *runtimeContext) error {
	logger, err := newLogger(e.LogLevel, "client")
	if err != nil {
		return err
	}

	client, err := e.connect()
	if err != nil {
		return err
	}
	cwd, err := resolveCWD(ctx.CWD, e.Chdir)
	if err != nil {
		return err
	}

	logger.Debug("sending execution request",
		"host", e.Host,
		"backend", e.Backend,
		"sandbox_id", strings.TrimSpace(e.SandboxID),
		"command_argc", len(e.Command),
	)
	sandboxID := strings.TrimSpace(e.SandboxID)
	createdSandbox := false
	if sandboxID == "" {
		compiled, _, err := ctx.Loader.LoadAndCompile(cwd)
		if err != nil {
			return err
		}
		createSandboxResp, err := client.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
			Backend: e.Backend,
			Options: &cleanroomv1.SandboxOptions{
				LaunchSeconds: e.LaunchSeconds,
			},
			Policy: compiled.ToProto(),
		})
		if err != nil {
			return fmt.Errorf("create sandbox: %w", err)
		}
		sandboxID = createSandboxResp.GetSandbox().GetSandboxId()
		createdSandbox = true
	}
	detached := false
	autoTerminateSandbox := createdSandbox && e.Remove
	defer func() {
		if detached || !autoTerminateSandbox || sandboxID == "" {
			return
		}
		_, _ = client.TerminateSandbox(context.Background(), &cleanroomv1.TerminateSandboxRequest{SandboxId: sandboxID})
	}()

	createExecutionResp, err := client.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   append([]string(nil), e.Command...),
		Options: &cleanroomv1.ExecutionOptions{
			LaunchSeconds: e.LaunchSeconds,
		},
	})
	if err != nil {
		return fmt.Errorf("create execution: %w", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()

	logger.Debug("execution started", "sandbox_id", sandboxID, "execution_id", executionID)

	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()
	stream, err := client.StreamExecution(streamCtx, &cleanroomv1.StreamExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Follow:      true,
	})
	if err != nil {
		return fmt.Errorf("stream execution: %w", err)
	}

	signalCh := newSignalChannel()
	notifySignals(signalCh, os.Interrupt, syscall.SIGTERM)
	defer stopSignals(signalCh)

	secondInterrupt := make(chan struct{}, 1)
	go func() {
		interrupts := 0
		for range signalCh {
			interrupts++
			if interrupts == 1 {
				cancelResp, cancelErr := client.CancelExecution(context.Background(), &cleanroomv1.CancelExecutionRequest{
					SandboxId:   sandboxID,
					ExecutionId: executionID,
					Signal:      2,
				})
				if cancelErr != nil && logger != nil {
					logger.Warn("cancel execution request failed", "sandbox_id", sandboxID, "execution_id", executionID, "error", cancelErr)
				} else if logger != nil && cancelResp != nil {
					logger.Debug("cancel execution requested",
						"sandbox_id", sandboxID,
						"execution_id", executionID,
						"accepted", cancelResp.GetAccepted(),
						"status", cancelResp.GetStatus().String(),
					)
				}
				continue
			}

			select {
			case secondInterrupt <- struct{}{}:
			default:
			}
			streamCancel()
			return
		}
	}()

	var exitCode int
	haveExitCode := false
	for stream.Receive() {
		event := stream.Msg()
		switch payload := event.Payload.(type) {
		case *cleanroomv1.ExecutionStreamEvent_Stdout:
			if _, err := fmt.Fprint(ctx.Stdout, string(payload.Stdout)); err != nil {
				return err
			}
		case *cleanroomv1.ExecutionStreamEvent_Stderr:
			if _, err := fmt.Fprint(os.Stderr, string(payload.Stderr)); err != nil {
				return err
			}
		case *cleanroomv1.ExecutionStreamEvent_Exit:
			exitCode = int(payload.Exit.GetExitCode())
			haveExitCode = true
		}
	}

	streamErr := stream.Err()
	select {
	case <-secondInterrupt:
		detached = true
		terminateCtx, terminateCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, terminateErr := client.TerminateSandbox(terminateCtx, &cleanroomv1.TerminateSandboxRequest{SandboxId: sandboxID})
		terminateCancel()
		if terminateErr != nil && logger != nil {
			logger.Warn("terminate sandbox after detach failed", "sandbox_id", sandboxID, "error", terminateErr)
		}
		return exitCodeError{code: 130}
	default:
	}

	if streamErr != nil && !isCanceledStreamErr(streamErr) {
		return fmt.Errorf("stream execution: %w", streamErr)
	}

	if !haveExitCode {
		getResp, getErr := client.GetExecution(context.Background(), &cleanroomv1.GetExecutionRequest{
			SandboxId:   sandboxID,
			ExecutionId: executionID,
		})
		if getErr == nil && getResp.GetExecution() != nil && isFinalExecutionStatus(getResp.GetExecution().GetStatus()) {
			exitCode = int(getResp.GetExecution().GetExitCode())
			haveExitCode = true
		}
	}

	logger.Debug("execution complete",
		"sandbox_id", sandboxID,
		"execution_id", executionID,
		"have_exit_code", haveExitCode,
		"exit_code", exitCode,
	)

	if !haveExitCode {
		return errors.New("execution stream ended without exit status")
	}
	if exitCode != 0 {
		return exitCodeError{code: exitCode}
	}
	return nil
}

type attachFrameSender struct {
	mu          sync.Mutex
	sandboxID   string
	executionID string
	stream      *connect.BidiStreamForClient[cleanroomv1.ExecutionAttachFrame, cleanroomv1.ExecutionAttachFrame]
}

func (s *attachFrameSender) Send(frame *cleanroomv1.ExecutionAttachFrame) error {
	if frame == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if frame.SandboxId == "" {
		frame.SandboxId = s.sandboxID
	}
	if frame.ExecutionId == "" {
		frame.ExecutionId = s.executionID
	}
	return s.stream.Send(frame)
}

func (c *ConsoleCommand) Run(ctx *runtimeContext) error {
	logger, err := newLogger(c.LogLevel, "client")
	if err != nil {
		return err
	}

	client, err := c.connect()
	if err != nil {
		return err
	}
	cwd, err := resolveCWD(ctx.CWD, c.Chdir)
	if err != nil {
		return err
	}

	command := append([]string(nil), c.Command...)
	if len(command) == 0 {
		command = []string{"sh"}
	}
	logger.Debug("starting interactive console",
		"host", c.Host,
		"backend", c.Backend,
		"sandbox_id", strings.TrimSpace(c.SandboxID),
		"command_argc", len(command),
	)
	sandboxID := strings.TrimSpace(c.SandboxID)
	createdSandbox := false
	if sandboxID == "" {
		compiled, _, err := ctx.Loader.LoadAndCompile(cwd)
		if err != nil {
			return err
		}
		createSandboxResp, err := client.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
			Backend: c.Backend,
			Options: &cleanroomv1.SandboxOptions{
				LaunchSeconds: c.LaunchSeconds,
			},
			Policy: compiled.ToProto(),
		})
		if err != nil {
			return fmt.Errorf("create sandbox: %w", err)
		}
		sandboxID = createSandboxResp.GetSandbox().GetSandboxId()
		createdSandbox = true
	}
	autoTerminateSandbox := createdSandbox && c.Remove
	defer func() {
		if sandboxID == "" || !autoTerminateSandbox {
			return
		}
		_, _ = client.TerminateSandbox(context.Background(), &cleanroomv1.TerminateSandboxRequest{SandboxId: sandboxID})
	}()

	createExecutionResp, err := client.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   command,
		Options: &cleanroomv1.ExecutionOptions{
			LaunchSeconds: c.LaunchSeconds,
			Tty:           true,
		},
	})
	if err != nil {
		return fmt.Errorf("create execution: %w", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()
	logger.Debug("console execution started", "sandbox_id", sandboxID, "execution_id", executionID)

	attachCtx, attachCancel := context.WithCancel(context.Background())
	defer attachCancel()
	attach := client.AttachExecution(attachCtx)
	sender := &attachFrameSender{
		sandboxID:   sandboxID,
		executionID: executionID,
		stream:      attach,
	}
	if err := sender.Send(&cleanroomv1.ExecutionAttachFrame{
		Payload: &cleanroomv1.ExecutionAttachFrame_Open{
			Open: &cleanroomv1.ExecutionAttachOpen{
				SandboxId:   sandboxID,
				ExecutionId: executionID,
			},
		},
	}); err != nil {
		return fmt.Errorf("open attach stream: %w", err)
	}

	stdinFD := int(os.Stdin.Fd())
	rawMode := false
	if term.IsTerminal(stdinFD) {
		oldState, rawErr := term.MakeRaw(stdinFD)
		if rawErr != nil {
			logger.Warn("failed to enter raw mode", "error", rawErr)
		} else {
			rawMode = true
			defer func() {
				_ = term.Restore(stdinFD, oldState)
			}()
			if cols, rows, sizeErr := term.GetSize(stdinFD); sizeErr == nil {
				_ = sender.Send(&cleanroomv1.ExecutionAttachFrame{
					Payload: &cleanroomv1.ExecutionAttachFrame_Resize{
						Resize: &cleanroomv1.ExecutionResize{
							Cols: uint32(cols),
							Rows: uint32(rows),
						},
					},
				})
			}
		}
	}

	signalCh := newSignalChannel()
	notifySignals(signalCh, os.Interrupt, syscall.SIGTERM)
	defer stopSignals(signalCh)

	if rawMode {
		resizeSignalCh := make(chan os.Signal, 4)
		signal.Notify(resizeSignalCh, syscall.SIGWINCH)
		defer signal.Stop(resizeSignalCh)
		go func() {
			for range resizeSignalCh {
				cols, rows, sizeErr := term.GetSize(stdinFD)
				if sizeErr != nil {
					continue
				}
				_ = sender.Send(&cleanroomv1.ExecutionAttachFrame{
					Payload: &cleanroomv1.ExecutionAttachFrame_Resize{
						Resize: &cleanroomv1.ExecutionResize{
							Cols: uint32(cols),
							Rows: uint32(rows),
						},
					},
				})
			}
		}()
	}

	go func() {
		for sig := range signalCh {
			num := int32(2)
			if sig == syscall.SIGTERM {
				num = 15
			}
			_ = sender.Send(&cleanroomv1.ExecutionAttachFrame{
				Payload: &cleanroomv1.ExecutionAttachFrame_Signal{
					Signal: &cleanroomv1.ExecutionSignal{Signal: num},
				},
			})
		}
	}()

	go func() {
		buf := make([]byte, 4096)
		for {
			n, readErr := os.Stdin.Read(buf)
			if n > 0 {
				payload := append([]byte(nil), buf[:n]...)
				if sendErr := sender.Send(&cleanroomv1.ExecutionAttachFrame{
					Payload: &cleanroomv1.ExecutionAttachFrame_Stdin{Stdin: payload},
				}); sendErr != nil {
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}()

	var exitCode int
	haveExitCode := false
	stdoutEndedCR := false
	stderrEndedCR := false
	for {
		frame, recvErr := attach.Receive()
		if recvErr != nil {
			if errors.Is(recvErr, io.EOF) || isCanceledStreamErr(recvErr) {
				break
			}
			return fmt.Errorf("attach execution: %w", recvErr)
		}
		switch payload := frame.Payload.(type) {
		case *cleanroomv1.ExecutionAttachFrame_Stdout:
			chunk := payload.Stdout
			if rawMode {
				chunk, stdoutEndedCR = normalizeLineEndingsForRawTTY(chunk, stdoutEndedCR)
			}
			if _, err := ctx.Stdout.Write(chunk); err != nil {
				return err
			}
		case *cleanroomv1.ExecutionAttachFrame_Stderr:
			chunk := payload.Stderr
			if rawMode {
				chunk, stderrEndedCR = normalizeLineEndingsForRawTTY(chunk, stderrEndedCR)
			}
			if _, err := os.Stderr.Write(chunk); err != nil {
				return err
			}
		case *cleanroomv1.ExecutionAttachFrame_Exit:
			exitCode = int(payload.Exit.GetExitCode())
			haveExitCode = true
		case *cleanroomv1.ExecutionAttachFrame_Error:
			_ = payload
		}
	}

	if !haveExitCode {
		getResp, getErr := client.GetExecution(context.Background(), &cleanroomv1.GetExecutionRequest{
			SandboxId:   sandboxID,
			ExecutionId: executionID,
		})
		if getErr == nil && getResp.GetExecution() != nil && isFinalExecutionStatus(getResp.GetExecution().GetStatus()) {
			exitCode = int(getResp.GetExecution().GetExitCode())
			haveExitCode = true
		}
	}

	if !haveExitCode {
		return errors.New("console stream ended without exit status")
	}
	if exitCode != 0 {
		return exitCodeError{code: exitCode}
	}
	return nil
}

func normalizeLineEndingsForRawTTY(chunk []byte, prevEndedCR bool) ([]byte, bool) {
	if len(chunk) == 0 {
		return chunk, prevEndedCR
	}

	if bytes.IndexByte(chunk, '\n') < 0 {
		return chunk, chunk[len(chunk)-1] == '\r'
	}

	out := make([]byte, 0, len(chunk)+4)
	endedCR := prevEndedCR
	for _, b := range chunk {
		if b == '\n' && !endedCR {
			out = append(out, '\r')
		}
		out = append(out, b)
		endedCR = b == '\r'
	}
	return out, endedCR
}

func (c *TLSInitCommand) Run(ctx *runtimeContext) error {
	dir := c.Dir
	if dir == "" {
		d, err := paths.TLSDir()
		if err != nil {
			return err
		}
		dir = d
	}
	if err := tlsbootstrap.Init(dir, c.Force); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "TLS material written to %s\n", dir)
	return nil
}

func (c *TLSIssueCommand) Run(ctx *runtimeContext) error {
	if strings.ContainsAny(c.Name, "/\\") || strings.Contains(c.Name, "..") {
		return fmt.Errorf("invalid certificate name %q: must not contain path separators or '..'", c.Name)
	}

	dir := c.Dir
	if dir == "" {
		d, err := paths.TLSDir()
		if err != nil {
			return err
		}
		dir = d
	}

	caCertPEM, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		return fmt.Errorf("read CA certificate: %w (run 'cleanroom tls init' first)", err)
	}
	caKeyPEM, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		return fmt.Errorf("read CA key: %w", err)
	}

	kp, err := tlsbootstrap.IssueCert(caCertPEM, caKeyPEM, c.Name, c.SAN)
	if err != nil {
		return err
	}

	certPath := filepath.Join(dir, c.Name+".pem")
	keyPath := filepath.Join(dir, c.Name+".key")
	if !c.Force {
		for _, p := range []string{certPath, keyPath} {
			if _, err := os.Stat(p); err == nil {
				return fmt.Errorf("%s already exists (use --force to overwrite)", p)
			}
		}
	}
	if err := os.WriteFile(certPath, kp.CertPEM, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, kp.KeyPEM, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Certificate written to %s\n", certPath)
	return nil
}

func (s *ServeCommand) Run(ctx *runtimeContext) error {
	logger, err := newLogger(s.LogLevel, "server")
	if err != nil {
		return err
	}

	ep, err := endpoint.ResolveListen(s.Listen)
	if err != nil {
		return err
	}

	gwRegistry := gateway.NewRegistry()
	gwCredentials := gateway.NewEnvCredentialProvider()
	gwServer := gateway.NewServer(gateway.ServerConfig{
		ListenAddr:  s.GatewayListen,
		Registry:    gwRegistry,
		Credentials: gwCredentials,
		Logger:      logger.With("subsystem", "gateway"),
	})
	if err := gwServer.Start(); err != nil {
		return fmt.Errorf("start gateway: %w", err)
	}

	gwPort := gateway.DefaultPort
	if _, portStr, err := net.SplitHostPort(gwServer.Addr()); err == nil {
		if p, err := strconv.Atoi(portStr); err == nil && p > 0 {
			gwPort = p
		}
	}

	if fcAdapter, ok := ctx.Backends["firecracker"].(*firecracker.Adapter); ok {
		fcAdapter.GatewayRegistry = gwRegistry
		fcAdapter.GatewayPort = gwPort

		if shouldInstallGatewayFirewall(runtime.GOOS) {
			fwCfg := backend.FirecrackerConfig{
				PrivilegedMode:       ctx.Config.Backends.Firecracker.PrivilegedMode,
				PrivilegedHelperPath: ctx.Config.Backends.Firecracker.PrivilegedHelperPath,
			}
			fwCleanup, err := firecracker.SetupGatewayFirewall(context.Background(), gwPort, fwCfg)
			if err != nil {
				logger.Warn("failed to install gateway firewall rules", "error", err)
			} else {
				defer fwCleanup()
			}
		}
	}
	if darwinAdapter, ok := ctx.Backends["darwin-vz"].(*darwinvz.Adapter); ok {
		darwinAdapter.GatewayRegistry = gwRegistry
		darwinAdapter.GatewayPort = gwPort
		if host := strings.TrimSpace(os.Getenv("CLEANROOM_DARWIN_GATEWAY_HOST")); host != "" {
			darwinAdapter.GatewayHost = host
		}
	}

	var serverTLS *controlserver.TLSOptions
	if ep.Scheme == "https" {
		serverTLS = &controlserver.TLSOptions{
			CertPath: s.TLSCert,
			KeyPath:  s.TLSKey,
			CAPath:   s.TLSCA,
		}
	}

	service := &controlservice.Service{
		Loader:   ctx.Loader,
		Config:   ctx.Config,
		Backends: ctx.Backends,
		Logger:   logger.With("subsystem", "service"),
	}
	server := controlserver.New(service, logger.With("subsystem", "http"))

	runCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	runErr := controlserver.Serve(runCtx, ep, server.Handler(), logger, serverTLS)
	gwStopCtx, gwStopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer gwStopCancel()
	_ = gwServer.Stop(gwStopCtx)
	return runErr
}

func (d *DoctorCommand) Run(ctx *runtimeContext) error {
	cwd, err := resolveCWD(ctx.CWD, d.Chdir)
	if err != nil {
		return err
	}
	backendName := resolveBackendName(d.Backend, ctx.Config.DefaultBackend)
	adapter, ok := ctx.Backends[backendName]
	if !ok {
		return fmt.Errorf("unknown backend %q", backendName)
	}
	capabilities := backend.CapabilitiesForAdapter(adapter)

	checks := []backend.DoctorCheck{
		{Name: "runtime_config", Status: "pass", Message: fmt.Sprintf("using runtime config path %s", ctx.ConfigPath)},
		{Name: "backend", Status: "pass", Message: fmt.Sprintf("selected backend %s", backendName)},
	}
	for _, key := range backend.SortedCapabilityKeys(capabilities) {
		status := "warn"
		message := fmt.Sprintf("%s: unsupported", key)
		if capabilities[key] {
			status = "pass"
			message = fmt.Sprintf("%s: supported", key)
		}
		checks = append(checks, backend.DoctorCheck{
			Name:    capabilityCheckName(key),
			Status:  status,
			Message: message,
		})
	}

	compiled, source, err := ctx.Loader.LoadAndCompile(cwd)
	if err != nil {
		checks = append(checks, backend.DoctorCheck{
			Name:    "repository_policy",
			Status:  "warn",
			Message: fmt.Sprintf("policy not loaded from %s: %v", cwd, err),
		})
	} else {
		checks = append(checks, backend.DoctorCheck{
			Name:    "repository_policy",
			Status:  "pass",
			Message: fmt.Sprintf("policy loaded from %s (hash %s)", source, compiled.Hash),
		})
	}

	type doctorCapable interface {
		Doctor(context.Context, backend.DoctorRequest) (*backend.DoctorReport, error)
	}
	if checker, ok := adapter.(doctorCapable); ok {
		report, err := checker.Doctor(context.Background(), backend.DoctorRequest{
			Policy:            compiled,
			FirecrackerConfig: mergeBackendConfig(backendName, 0, ctx.Config),
		})
		if err != nil {
			return err
		}
		checks = append(checks, report.Checks...)
	} else {
		checks = append(checks, backend.DoctorCheck{
			Name:    "backend_doctor",
			Status:  "warn",
			Message: "selected backend does not expose doctor diagnostics",
		})
	}

	if d.JSON {
		payload := map[string]any{
			"backend":      backendName,
			"capabilities": backend.CloneCapabilities(capabilities),
			"checks":       checks,
		}
		enc := json.NewEncoder(ctx.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(payload)
	}

	_, err = fmt.Fprintf(ctx.Stdout, "doctor report (%s)\n", backendName)
	if err != nil {
		return err
	}
	for _, check := range checks {
		if _, err := fmt.Fprintf(ctx.Stdout, "- [%s] %s: %s\n", check.Status, check.Name, check.Message); err != nil {
			return err
		}
	}
	return nil
}

func resolveBackendName(requested, configuredDefault string) string {
	if requested != "" {
		return requested
	}
	if configuredDefault != "" {
		return configuredDefault
	}
	return "firecracker"
}

func shouldInstallGatewayFirewall(goos string) bool {
	return strings.EqualFold(strings.TrimSpace(goos), "linux")
}

var capabilityNameReplacer = strings.NewReplacer(".", "_", "-", "_")

func capabilityCheckName(key string) string {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return "capability_unknown"
	}
	return "capability_" + capabilityNameReplacer.Replace(trimmed)
}

func validateClientEndpoint(ep endpoint.Endpoint) error {
	if ep.Scheme != "tssvc" {
		return nil
	}
	return errors.New("tssvc:// endpoints are listen-only; use https://<service>.<your-tailnet>.ts.net for --host")
}

func mergeBackendConfig(backendName string, launchSeconds int64, cfg runtimeconfig.Config) backend.FirecrackerConfig {
	out := backend.FirecrackerConfig{
		BinaryPath:           cfg.Backends.Firecracker.BinaryPath,
		KernelImagePath:      cfg.Backends.Firecracker.KernelImage,
		RootFSPath:           cfg.Backends.Firecracker.RootFS,
		PrivilegedMode:       cfg.Backends.Firecracker.PrivilegedMode,
		PrivilegedHelperPath: cfg.Backends.Firecracker.PrivilegedHelperPath,
		VCPUs:                cfg.Backends.Firecracker.VCPUs,
		MemoryMiB:            cfg.Backends.Firecracker.MemoryMiB,
		GuestCID:             cfg.Backends.Firecracker.GuestCID,
		GuestPort:            cfg.Backends.Firecracker.GuestPort,
		LaunchSeconds:        cfg.Backends.Firecracker.LaunchSeconds,
	}
	if backendName == "darwin-vz" {
		out.KernelImagePath = cfg.Backends.DarwinVZ.KernelImage
		out.RootFSPath = cfg.Backends.DarwinVZ.RootFS
		out.VCPUs = cfg.Backends.DarwinVZ.VCPUs
		out.MemoryMiB = cfg.Backends.DarwinVZ.MemoryMiB
		out.GuestPort = cfg.Backends.DarwinVZ.GuestPort
		out.LaunchSeconds = cfg.Backends.DarwinVZ.LaunchSeconds
	}

	out.Launch = true
	if launchSeconds != 0 {
		out.LaunchSeconds = launchSeconds
	}
	return out
}

func (s *StatusCommand) Run(ctx *runtimeContext) error {
	baseDir, err := paths.RunBaseDir()
	if err != nil {
		return fmt.Errorf("resolve run base directory: %w", err)
	}
	if s.RunID != "" && s.LastRun {
		return errors.New("choose either --run-id or --last-run")
	}
	if s.RunID != "" {
		return inspectRun(ctx.Stdout, baseDir, s.RunID)
	}
	if s.LastRun {
		entries, err := os.ReadDir(baseDir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				_, werr := fmt.Fprintf(ctx.Stdout, "no runs found (%s does not exist)\n", baseDir)
				return werr
			}
			return err
		}
		var newest string
		var newestTime time.Time
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if newest == "" || info.ModTime().After(newestTime) {
				newest = entry.Name()
				newestTime = info.ModTime()
			}
		}
		if newest == "" {
			_, err := fmt.Fprintf(ctx.Stdout, "no runs found in %s\n", baseDir)
			return err
		}
		return inspectRun(ctx.Stdout, baseDir, newest)
	}

	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			_, werr := fmt.Fprintf(ctx.Stdout, "no runs found (%s does not exist)\n", baseDir)
			return werr
		}
		return err
	}

	if len(entries) == 0 {
		_, err := fmt.Fprintf(ctx.Stdout, "no runs found in %s\n", baseDir)
		return err
	}

	_, err = fmt.Fprintf(ctx.Stdout, "runs in %s:\n", baseDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := fmt.Fprintf(ctx.Stdout, "- %s\n", entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

func inspectRun(stdout *os.File, baseDir, runID string) error {
	runDir := filepath.Join(baseDir, runID)
	if _, err := os.Stat(runDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("run %q not found in %s", runID, baseDir)
		}
		return err
	}
	if _, err := fmt.Fprintf(stdout, "run: %s\n", runDir); err != nil {
		return err
	}
	obsPath := filepath.Join(runDir, "run-observability.json")
	b, err := os.ReadFile(obsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			_, werr := fmt.Fprintf(stdout, "observability: not found (%s)\n", obsPath)
			return werr
		}
		return err
	}
	var obs map[string]any
	if err := json.Unmarshal(b, &obs); err != nil {
		return fmt.Errorf("parse %s: %w", obsPath, err)
	}
	out, err := json.MarshalIndent(obs, "", "  ")
	if err != nil {
		return fmt.Errorf("format %s: %w", obsPath, err)
	}
	_, err = fmt.Fprintf(stdout, "observability (%s):\n%s\n", obsPath, out)
	return err
}

func isCanceledStreamErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	var connectErr *connect.Error
	if errors.As(err, &connectErr) && connectErr.Code() == connect.CodeCanceled {
		return true
	}
	return false
}

func sandboxStatusString(s cleanroomv1.SandboxStatus) string {
	switch s {
	case cleanroomv1.SandboxStatus_SANDBOX_STATUS_PROVISIONING:
		return "provisioning"
	case cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY:
		return "ready"
	case cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPING:
		return "stopping"
	case cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED:
		return "stopped"
	case cleanroomv1.SandboxStatus_SANDBOX_STATUS_FAILED:
		return "failed"
	default:
		return "unknown"
	}
}

func isFinalExecutionStatus(status cleanroomv1.ExecutionStatus) bool {
	switch status {
	case cleanroomv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED,
		cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED,
		cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED,
		cleanroomv1.ExecutionStatus_EXECUTION_STATUS_TIMED_OUT:
		return true
	default:
		return false
	}
}

func resolveCWD(base, chdir string) (string, error) {
	if chdir == "" {
		return base, nil
	}
	if filepath.IsAbs(chdir) {
		return filepath.Clean(chdir), nil
	}
	return filepath.Join(base, chdir), nil
}

func resolvePolicyPathForUpdate(cwd, candidate string) (string, error) {
	if strings.TrimSpace(candidate) != "" {
		if filepath.IsAbs(candidate) {
			return filepath.Clean(candidate), nil
		}
		return filepath.Join(cwd, candidate), nil
	}

	primary := filepath.Join(cwd, policy.PrimaryPolicyPath)
	primaryExists, err := fileExists(primary)
	if err != nil {
		return "", fmt.Errorf("check policy %s: %w", primary, err)
	}
	if primaryExists {
		return primary, nil
	}

	fallback := filepath.Join(cwd, policy.FallbackPolicyPath)
	fallbackExists, err := fileExists(fallback)
	if err != nil {
		return "", fmt.Errorf("check policy %s: %w", fallback, err)
	}
	if fallbackExists {
		return fallback, nil
	}

	return "", fmt.Errorf("policy not found: expected %s or %s", primary, fallback)
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func setSandboxImageRef(raw []byte, ref string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 {
		doc.Kind = yaml.DocumentNode
	}

	bootstrapped := false
	if len(doc.Content) == 0 {
		doc.Content = append(doc.Content, &yaml.Node{Kind: yaml.MappingNode})
		bootstrapped = true
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("policy root must be a mapping")
	}
	if bootstrapped {
		setMapInt(root, "version", 1)
	}

	sandbox := ensureMapEntry(root, "sandbox")
	image := ensureMapEntry(sandbox, "image")
	setMapString(image, "ref", ref)

	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	err := enc.Encode(&doc)
	_ = enc.Close()
	if err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func ensureMapEntry(parent *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value != key {
			continue
		}
		if parent.Content[i+1].Kind != yaml.MappingNode {
			parent.Content[i+1].Kind = yaml.MappingNode
			parent.Content[i+1].Tag = ""
			parent.Content[i+1].Value = ""
			parent.Content[i+1].Content = nil
		}
		return parent.Content[i+1]
	}

	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	valueNode := &yaml.Node{Kind: yaml.MappingNode}
	parent.Content = append(parent.Content, keyNode, valueNode)
	return valueNode
}

func setMapString(parent *yaml.Node, key, value string) {
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value != key {
			continue
		}
		parent.Content[i+1].Kind = yaml.ScalarNode
		parent.Content[i+1].Tag = "!!str"
		parent.Content[i+1].Value = value
		parent.Content[i+1].Content = nil
		return
	}

	parent.Content = append(
		parent.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}

func setMapInt(parent *yaml.Node, key string, value int) {
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value != key {
			continue
		}
		parent.Content[i+1].Kind = yaml.ScalarNode
		parent.Content[i+1].Tag = "!!int"
		parent.Content[i+1].Value = fmt.Sprintf("%d", value)
		parent.Content[i+1].Content = nil
		return
	}

	parent.Content = append(
		parent.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprintf("%d", value)},
	)
}

func newLogger(rawLevel, component string) (*log.Logger, error) {
	levelName := strings.TrimSpace(strings.ToLower(rawLevel))
	if levelName == "" {
		levelName = "info"
	}
	level, err := log.ParseLevel(levelName)
	if err != nil {
		return nil, fmt.Errorf("invalid --log-level %q: %w", rawLevel, err)
	}
	logger := log.NewWithOptions(os.Stderr, log.Options{
		Level:     level,
		Formatter: log.TextFormatter,
	})
	return logger.With("component", component), nil
}
