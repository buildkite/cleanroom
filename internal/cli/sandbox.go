package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/buildkite/cleanroom/internal/controlclient"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/policy"
)

type SandboxCommand struct {
	Create    SandboxCreateCommand    `cmd:"" help:"Create a repo-agnostic sandbox"`
	Inspect   SandboxInspectCommand   `name:"inspect" aliases:"show" cmd:"" help:"Inspect sandbox state and related execution IDs"`
	List      SandboxListCommand      `name:"ls" aliases:"list" cmd:"" help:"List active sandboxes"`
	Terminate SandboxTerminateCommand `name:"rm" aliases:"terminate" cmd:"" help:"Terminate a sandbox"`
}

type SandboxCreateCommand struct {
	clientFlags
	Backend             string `help:"Execution backend (defaults to runtime config or host default)"`
	From                string `name:"from" help:"Create the sandbox from an existing snapshot ID"`
	Image               string `help:"Override sandbox image ref (tag, digest, or local Docker image)"`
	Docker              bool   `help:"Enable the guest Docker service for this repo-agnostic sandbox"`
	DangerouslyAllowAll bool   `name:"dangerously-allow-all" help:"Disable network egress filtering for this repo-agnostic sandbox"`
	LaunchSeconds       int64  `help:"VM boot/guest-agent readiness timeout in seconds"`
	JSON                bool   `help:"Print sandbox as JSON"`
}

type SandboxListCommand struct {
	clientFlags
	All  bool `help:"Include stopped sandboxes"`
	JSON bool `help:"Print sandboxes as JSON"`
}

type SandboxInspectCommand struct {
	clientFlags
	SandboxID string `arg:"" optional:"" help:"Sandbox ID to inspect"`
	Last      bool   `help:"Inspect the most recent sandbox"`
	JSON      bool   `help:"Print sandbox as JSON"`
}

type SandboxTerminateCommand struct {
	clientFlags
	SandboxID string `arg:"" required:"" help:"Sandbox ID to terminate"`
}

type CreateCommand struct {
	clientFlags
	Chdir   string `short:"c" help:"Change to this directory before running commands"`
	Backend string `help:"Execution backend (defaults to runtime config or host default)"`
	From    string `name:"from" help:"Create the sandbox from an existing snapshot ID"`
	Image   string `help:"Override sandbox image ref (tag, digest, or local Docker image)"`
	repositoryOverrideFlags
	workspaceCopyFlags
	DangerouslyAllowAll bool  `name:"dangerously-allow-all" help:"Disable network egress filtering for a newly created sandbox"`
	LaunchSeconds       int64 `help:"VM boot/guest-agent readiness timeout in seconds"`
	JSON                bool  `help:"Print sandbox as JSON"`
}

func (c *SandboxListCommand) Run(ctx *runtimeContext) error {
	client, err := c.connect(ctx)
	if err != nil {
		return err
	}

	resp, err := client.ListSandboxes(context.Background(), &cleanroomv1.ListSandboxesRequest{})
	if err != nil {
		return err
	}
	sandboxes := filterSandboxList(resp.Sandboxes, c.All)

	if c.JSON {
		enc := json.NewEncoder(ctx.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(sandboxes)
	}

	if len(sandboxes) == 0 {
		message := "no active sandboxes"
		if c.All {
			message = "no sandboxes"
		}
		_, err := fmt.Fprintln(ctx.Stdout, message)
		return err
	}

	tw := tabwriter.NewWriter(ctx.Stdout, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tSTATUS\tBACKEND\tCREATED"); err != nil {
		return err
	}
	for _, sb := range sandboxes {
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

func (c *SandboxInspectCommand) Run(ctx *runtimeContext) error {
	client, err := c.connect(ctx)
	if err != nil {
		return err
	}

	sandboxID := strings.TrimSpace(c.SandboxID)
	if c.Last {
		if sandboxID != "" {
			return errors.New("choose either <sandbox-id> or --last")
		}
		listResp, err := client.ListSandboxes(context.Background(), &cleanroomv1.ListSandboxesRequest{})
		if err != nil {
			return fmt.Errorf("list sandboxes: %w", err)
		}
		for _, sandbox := range listResp.GetSandboxes() {
			if sandbox == nil {
				continue
			}
			sandboxID = strings.TrimSpace(sandbox.GetSandboxId())
			if sandboxID != "" {
				break
			}
		}
		if sandboxID == "" {
			return errors.New("no sandboxes available")
		}
	} else if sandboxID == "" {
		return errors.New("missing <sandbox-id> or use --last")
	}

	resp, err := client.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{
		SandboxId: sandboxID,
	})
	if err != nil {
		return err
	}
	sandbox := resp.GetSandbox()
	if sandbox == nil {
		return fmt.Errorf("sandbox %q not found", sandboxID)
	}

	if c.JSON {
		enc := json.NewEncoder(ctx.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(sandbox)
	}

	if _, err := fmt.Fprintf(ctx.Stdout, "sandbox: %s\n", sandbox.GetSandboxId()); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(ctx.Stdout, "status: %s\n", sandboxStatusString(sandbox.GetStatus())); err != nil {
		return err
	}
	if backend := strings.TrimSpace(sandbox.GetBackend()); backend != "" {
		if _, err := fmt.Fprintf(ctx.Stdout, "backend: %s\n", backend); err != nil {
			return err
		}
	}
	if policyHash := strings.TrimSpace(sandbox.GetPolicyHash()); policyHash != "" {
		if _, err := fmt.Fprintf(ctx.Stdout, "policy_hash: %s\n", policyHash); err != nil {
			return err
		}
	}
	if sourceKind := strings.TrimSpace(sandbox.GetSourceKind()); sourceKind != "" {
		if _, err := fmt.Fprintf(ctx.Stdout, "source_kind: %s\n", sourceKind); err != nil {
			return err
		}
	}
	if sourceID := strings.TrimSpace(sandbox.GetSourceId()); sourceID != "" {
		if _, err := fmt.Fprintf(ctx.Stdout, "source_id: %s\n", sourceID); err != nil {
			return err
		}
	}
	if backingSnapshotID := strings.TrimSpace(sandbox.GetBackingSnapshotId()); backingSnapshotID != "" {
		if _, err := fmt.Fprintf(ctx.Stdout, "backing_snapshot_id: %s\n", backingSnapshotID); err != nil {
			return err
		}
	}
	if created := sandbox.GetCreatedAt(); created != nil {
		if _, err := fmt.Fprintf(ctx.Stdout, "created_at: %s\n", created.AsTime().Format(time.RFC3339)); err != nil {
			return err
		}
	}
	if updated := sandbox.GetUpdatedAt(); updated != nil {
		if _, err := fmt.Fprintf(ctx.Stdout, "updated_at: %s\n", updated.AsTime().Format(time.RFC3339)); err != nil {
			return err
		}
	}
	if lastExecutionID := strings.TrimSpace(sandbox.GetLastExecutionId()); lastExecutionID != "" {
		if _, err := fmt.Fprintf(ctx.Stdout, "last_execution_id: %s\n", lastExecutionID); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(ctx.Stdout, "inspect_last_execution: cleanroom execution inspect %s\n", lastExecutionID); err != nil {
			return err
		}
	}
	if activeExecutionID := strings.TrimSpace(sandbox.GetActiveExecutionId()); activeExecutionID != "" {
		if _, err := fmt.Fprintf(ctx.Stdout, "active_execution_id: %s\n", activeExecutionID); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(ctx.Stdout, "inspect_active_execution: cleanroom execution inspect %s\n", activeExecutionID); err != nil {
			return err
		}
	}
	return nil
}

func filterSandboxList(sandboxes []*cleanroomv1.Sandbox, includeStopped bool) []*cleanroomv1.Sandbox {
	filtered := make([]*cleanroomv1.Sandbox, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		if sandbox == nil {
			continue
		}
		if !includeStopped && sandbox.Status == cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED {
			continue
		}
		filtered = append(filtered, sandbox)
	}
	return filtered
}

func (c *SandboxTerminateCommand) Run(ctx *runtimeContext) error {
	client, err := c.connect(ctx)
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

func runSandboxCreate(ctx *runtimeContext, connectFlags clientFlags, backend, from, imageRefOverride string, requireDockerService, dangerouslyAllowAll bool, launchSeconds int64, outputJSON bool) error {
	resolvedHost := connectFlags.resolvedHost(ctx.Config)
	client, err := connectFlags.connect(ctx)
	if err != nil {
		return err
	}

	from = strings.TrimSpace(from)
	var sandbox *cleanroomv1.Sandbox
	if from != "" {
		if strings.TrimSpace(imageRefOverride) != "" {
			return errors.New("--image cannot be used with --from")
		}
		if strings.TrimSpace(backend) != "" {
			return errors.New("--backend cannot be used with --from")
		}
		if requireDockerService {
			return errors.New("--docker cannot be used with --from")
		}
		if dangerouslyAllowAll {
			return errors.New("--dangerously-allow-all cannot be used with --from")
		}

		resp, _, err := createSandboxWithProgress(context.Background(), os.Stderr, client, &cleanroomv1.CreateSandboxRequest{
			Options: &cleanroomv1.SandboxOptions{
				LaunchSeconds: launchSeconds,
			},
			Source: &cleanroomv1.CreateSandboxRequest_SnapshotId{SnapshotId: from},
		})
		if err != nil {
			err = explainSnapshotRuntimeDisabledError(err, ctx)
			return fmt.Errorf("create sandbox: %w", err)
		}
		sandbox = resp.GetSandbox()
	} else {
		compiled, err := defaultSandboxCreatePolicy(resolvedHost, imageRefOverride, requireDockerService, dangerouslyAllowAll)
		if err != nil {
			return err
		}
		_, sandbox, err = createSandboxWithPolicy(context.Background(), client, compiled, backend, launchSeconds, nil, repositoryLocalChanges{})
		if err != nil {
			return err
		}
	}

	sandboxID := strings.TrimSpace(sandbox.GetSandboxId())
	if sandboxID == "" {
		return errors.New("create sandbox: response missing sandbox id")
	}

	if outputJSON {
		enc := json.NewEncoder(ctx.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(sandbox)
	}

	_, err = fmt.Fprintln(ctx.Stdout, sandboxID)
	return err
}

func (c *SandboxCreateCommand) Run(ctx *runtimeContext) error {
	return runSandboxCreate(ctx, c.clientFlags, c.Backend, c.From, c.Image, c.Docker, c.DangerouslyAllowAll, c.LaunchSeconds, c.JSON)
}

func (c *CreateCommand) Run(ctx *runtimeContext) error {
	if err := c.validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.From) != "" {
		return runSandboxCreate(ctx, c.clientFlags, c.Backend, c.From, c.Image, false, c.DangerouslyAllowAll, c.LaunchSeconds, c.JSON)
	}
	host := c.resolvedHost(ctx.Config)
	client, err := c.connect(ctx)
	if err != nil {
		return err
	}

	cwd, err := resolveCWD(ctx.CWD, c.Chdir)
	if err != nil {
		return err
	}
	repository, err := resolveRepositoryCheckoutWithOverride(cwd, ctx.Loader, c.repositoryOverrideFlags)
	if err != nil {
		return err
	}
	if err := validateTopLevelWorkspaceCopyTransport(repository, c.CopyIn); err != nil {
		return err
	}
	localChanges, err := resolveRepositoryLocalChanges(repository, c.CopyIn)
	if err != nil {
		return err
	}
	warnDirtyRepositoryCheckout(repository, c.CopyIn && repository != nil)

	sandboxID, sandbox, err := createTopLevelSandbox(context.Background(), client, ctx.Loader, cwd, host, c.Backend, c.Image, c.LaunchSeconds, c.DangerouslyAllowAll, repository, localChanges)
	if err != nil {
		return err
	}
	if c.JSON {
		enc := json.NewEncoder(ctx.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(sandbox)
	}
	_, err = fmt.Fprintln(ctx.Stdout, sandboxID)
	return err
}

func (c *CreateCommand) validate() error {
	if _, err := c.repositoryOverrideFlags.resolve(".", nil); err != nil {
		return err
	}
	if err := c.workspaceCopyFlags.validate("", c.From, c.repositoryOverrideFlags); err != nil {
		return err
	}
	if c.repositoryOverrideFlags.hasRepositoryOverride() && strings.TrimSpace(c.From) != "" {
		return errors.New("--repo-url cannot be used with --from")
	}
	if c.DangerouslyAllowAll && strings.TrimSpace(c.From) != "" {
		return errors.New("--dangerously-allow-all cannot be used with --from")
	}
	return nil
}

func createTopLevelSandbox(
	callCtx context.Context,
	client *controlclient.Client,
	loader policyLoader,
	cwd, host, backendName, imageRefOverride string,
	launchSeconds int64,
	dangerouslyAllowAll bool,
	repository *resolvedRepositoryCheckout,
	localChanges repositoryLocalChanges,
) (string, *cleanroomv1.Sandbox, error) {
	compiled, _, err := loader.LoadAndCompile(cwd)
	if err != nil {
		return "", nil, err
	}
	allowLocalImageOverride, err := isLocalControlPlaneEndpoint(host)
	if err != nil {
		return "", nil, err
	}
	compiled, err = overrideCompiledPolicyImage(compiled, imageRefOverride, allowLocalImageOverride)
	if err != nil {
		return "", nil, err
	}
	compiled, err = overrideCompiledPolicyNetworkDefault(compiled, dangerouslyAllowAll)
	if err != nil {
		return "", nil, err
	}

	return createSandboxWithPolicy(callCtx, client, compiled, backendName, launchSeconds, repository, localChanges)
}

func overrideCompiledPolicyNetworkDefault(compiled *policy.CompiledPolicy, dangerouslyAllowAll bool) (*policy.CompiledPolicy, error) {
	if !dangerouslyAllowAll {
		return compiled, nil
	}
	if compiled == nil {
		return nil, errors.New("create sandbox: missing compiled policy")
	}
	pb := compiled.ToProto()
	pb.NetworkDefault = "allow"
	pb.Hash = ""
	return policy.FromProto(pb)
}

func createSandboxWithProgress(
	callCtx context.Context,
	stderr *os.File,
	client *controlclient.Client,
	req *cleanroomv1.CreateSandboxRequest,
) (*cleanroomv1.CreateSandboxResponse, string, error) {
	startedAt := time.Now()
	if callCtx == nil {
		callCtx = context.Background()
	}

	var resp *cleanroomv1.CreateSandboxResponse
	sandboxID := ""
	run := func(progress *sandboxProgress) error {
		stream, err := client.CreateSandboxStream(callCtx, req)
		if err != nil {
			return err
		}
		defer func() {
			_ = stream.Close()
		}()

		for stream.Receive() {
			event := stream.Msg()
			switch payload := event.Payload.(type) {
			case *cleanroomv1.CreateSandboxEvent_Message:
				continue
			case *cleanroomv1.CreateSandboxEvent_Stdout:
				if stderr != nil && len(payload.Stdout) > 0 {
					progress.suppress()
					_, _ = stderr.Write(payload.Stdout)
				}
			case *cleanroomv1.CreateSandboxEvent_Stderr:
				if stderr != nil && len(payload.Stderr) > 0 {
					progress.suppress()
					_, _ = stderr.Write(payload.Stderr)
				}
			case *cleanroomv1.CreateSandboxEvent_Warning:
				progress.suppress()
				if err := writeExecutionWarning(stderr, payload.Warning); err != nil {
					return err
				}
			case *cleanroomv1.CreateSandboxEvent_Response:
				resp = payload.Response
			}
		}
		if err := stream.Err(); err != nil {
			return err
		}
		if resp == nil {
			return errors.New("create sandbox stream returned no response")
		}
		sandboxID = strings.TrimSpace(resp.GetSandbox().GetSandboxId())
		if sandboxID == "" {
			return errors.New("response missing sandbox id")
		}
		return nil
	}

	showProgress := stderr != nil && isTerminalFunc(int(stderr.Fd()))
	var err error
	if showProgress {
		err = withSandboxProgress(stderr, run)
	} else {
		err = run(nil)
		if stderr != nil {
			writeSandboxProgressCompletePlain(stderr, err == nil, time.Since(startedAt))
		}
	}
	if err != nil {
		return nil, "", err
	}
	return resp, sandboxID, nil
}

func createSandboxWithPolicy(
	callCtx context.Context,
	client *controlclient.Client,
	compiled *policy.CompiledPolicy,
	backendName string,
	launchSeconds int64,
	repository *resolvedRepositoryCheckout,
	localChanges repositoryLocalChanges,
) (string, *cleanroomv1.Sandbox, error) {
	if compiled == nil {
		return "", nil, errors.New("create sandbox: missing compiled policy")
	}

	createSandboxResp, sandboxID, err := createSandboxWithProgress(callCtx, os.Stderr, client, &cleanroomv1.CreateSandboxRequest{
		Backend: backendName,
		Options: &cleanroomv1.SandboxOptions{
			LaunchSeconds: launchSeconds,
		},
		Policy:                 compiled.ToProto(),
		RepositoryCheckout:     repositoryCheckoutProto(repository),
		RepositoryChangeset:    localChanges.Changeset,
		RepositoryCommitBundle: localChanges.CommitBundle,
	})
	if err != nil {
		return "", nil, fmt.Errorf("create sandbox: %w", err)
	}

	sandbox := createSandboxResp.GetSandbox()
	return sandboxID, sandbox, nil
}

func defaultSandboxCreatePolicy(host, imageRefOverride string, requireDockerService, dangerouslyAllowAll bool) (*policy.CompiledPolicy, error) {
	imageRefOverride = strings.TrimSpace(imageRefOverride)
	resolvedRef := ""
	if imageRefOverride == "" {
		ref, err := resolveReferenceForPolicyUpdate(context.Background(), defaultBumpRefSource)
		if err != nil {
			return nil, fmt.Errorf("resolve default sandbox image %q: %w", defaultBumpRefSource, err)
		}
		resolvedRef = ref
	} else {
		allowLocalImageOverride, err := isLocalControlPlaneEndpoint(host)
		if err != nil {
			return nil, err
		}
		ref, err := resolveReferenceForImageOverride(context.Background(), imageRefOverride, allowLocalImageOverride)
		if err != nil {
			return nil, fmt.Errorf("invalid --image value: %w", err)
		}
		resolvedRef = ref
	}

	networkDefault := "deny"
	if dangerouslyAllowAll {
		networkDefault = "allow"
	}

	compiled, err := policy.FromProto(&cleanroomv1.Policy{
		Version:        1,
		ImageRef:       resolvedRef,
		NetworkDefault: networkDefault,
		Services: &cleanroomv1.PolicyServices{
			Docker: &cleanroomv1.PolicyDockerService{
				Required: requireDockerService,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("build sandbox policy: %w", err)
	}
	return compiled, nil
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
