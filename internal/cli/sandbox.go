package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/buildkite/cleanroom/internal/controlclient"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

type SandboxCommand struct {
	Create    SandboxCreateCommand    `cmd:"" help:"Create a sandbox"`
	Inspect   SandboxInspectCommand   `name:"inspect" aliases:"show" cmd:"" help:"Inspect sandbox state and related execution IDs"`
	List      SandboxListCommand      `name:"ls" aliases:"list" cmd:"" help:"List active sandboxes"`
	Terminate SandboxTerminateCommand `name:"rm" aliases:"terminate" cmd:"" help:"Terminate a sandbox"`
}

type SandboxCreateCommand struct {
	clientFlags
	Chdir         string `short:"c" help:"Change to this directory before running commands"`
	Backend       string `help:"Execution backend (defaults to runtime config or host default)"`
	From          string `name:"from" help:"Create the sandbox from an existing snapshot ID"`
	Image         string `help:"Override sandbox image ref (tag, digest, or local Docker image)"`
	LaunchSeconds int64  `help:"VM boot/guest-agent readiness timeout in seconds"`
	JSON          bool   `help:"Print sandbox as JSON"`
}

type SandboxListCommand struct {
	clientFlags
	All  bool `help:"Include stopped sandboxes"`
	JSON bool `help:"Print sandboxes as JSON"`
}

type SandboxInspectCommand struct {
	clientFlags
	SandboxID string `arg:"" required:"" help:"Sandbox ID to inspect"`
	JSON      bool   `help:"Print sandbox as JSON"`
}

type SandboxTerminateCommand struct {
	clientFlags
	SandboxID string `arg:"" required:"" help:"Sandbox ID to terminate"`
}

type CreateCommand struct {
	clientFlags
	Chdir         string `short:"c" help:"Change to this directory before running commands"`
	Backend       string `help:"Execution backend (defaults to runtime config or host default)"`
	From          string `name:"from" help:"Create the sandbox from an existing snapshot ID"`
	Image         string `help:"Override sandbox image ref (tag, digest, or local Docker image)"`
	LaunchSeconds int64  `help:"VM boot/guest-agent readiness timeout in seconds"`
	JSON          bool   `help:"Print sandbox as JSON"`
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

	resp, err := client.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{
		SandboxId: c.SandboxID,
	})
	if err != nil {
		return err
	}
	sandbox := resp.GetSandbox()
	if sandbox == nil {
		return fmt.Errorf("sandbox %q not found", c.SandboxID)
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
		if _, err := fmt.Fprintf(ctx.Stdout, "inspect_last_execution: cleanroom execution inspect --sandbox-id %s --last\n", sandbox.GetSandboxId()); err != nil {
			return err
		}
	}
	if activeExecutionID := strings.TrimSpace(sandbox.GetActiveExecutionId()); activeExecutionID != "" {
		if _, err := fmt.Fprintf(ctx.Stdout, "active_execution_id: %s\n", activeExecutionID); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(ctx.Stdout, "inspect_active_execution: cleanroom execution inspect --sandbox-id %s %s\n", sandbox.GetSandboxId(), activeExecutionID); err != nil {
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

func runSandboxCreate(ctx *runtimeContext, connectFlags clientFlags, chdir, backend, from, imageRefOverride string, launchSeconds int64, outputJSON bool) error {
	resolvedHost := connectFlags.resolvedHost(ctx.Config)
	client, err := connectFlags.connect(ctx)
	if err != nil {
		return err
	}

	createReq := &cleanroomv1.CreateSandboxRequest{
		Backend: backend,
		Options: &cleanroomv1.SandboxOptions{
			LaunchSeconds: launchSeconds,
		},
	}
	from = strings.TrimSpace(from)
	if from != "" {
		if strings.TrimSpace(imageRefOverride) != "" {
			return errors.New("--image cannot be used with --from")
		}
		if strings.TrimSpace(backend) != "" {
			return errors.New("--backend cannot be used with --from")
		}
		createReq.Source = &cleanroomv1.CreateSandboxRequest_SnapshotId{SnapshotId: from}
	} else {
		cwd, err := resolveCWD(ctx.CWD, chdir)
		if err != nil {
			return err
		}
		compiled, _, err := ctx.Loader.LoadAndCompile(cwd)
		if err != nil {
			return err
		}
		allowLocalImageOverride, err := isLocalControlPlaneEndpoint(resolvedHost)
		if err != nil {
			return err
		}
		compiled, err = overrideCompiledPolicyImage(compiled, imageRefOverride, allowLocalImageOverride)
		if err != nil {
			return err
		}
		createReq.Policy = compiled.ToProto()
	}

	resp, err := client.CreateSandbox(context.Background(), createReq)
	if err != nil {
		if from != "" {
			err = explainSnapshotRuntimeDisabledError(err, ctx)
		}
		return fmt.Errorf("create sandbox: %w", err)
	}

	sandbox := resp.GetSandbox()
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
	return runSandboxCreate(ctx, c.clientFlags, c.Chdir, c.Backend, c.From, c.Image, c.LaunchSeconds, c.JSON)
}

func (c *CreateCommand) Run(ctx *runtimeContext) error {
	if strings.TrimSpace(c.From) != "" {
		return runSandboxCreate(ctx, c.clientFlags, c.Chdir, c.Backend, c.From, c.Image, c.LaunchSeconds, c.JSON)
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
	repository, err := resolveRepositoryCheckout(cwd, ctx.Loader)
	if err != nil {
		return err
	}
	warnDirtyRepositoryCheckout(repository)
	if repository != nil && !backendSupportsRepositoryPersistence(ctx, host, c.Backend) {
		return errors.New("repository bootstrap for cleanroom create requires a persistent backend; use cleanroom exec, cleanroom console, or select a persistent backend")
	}

	sandboxID, sandbox, err := createTopLevelSandbox(client, ctx.Loader, cwd, host, c.Backend, c.Image, c.LaunchSeconds, repository)
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

func createTopLevelSandbox(
	client *controlclient.Client,
	loader policyLoader,
	cwd, host, backendName, imageRefOverride string,
	launchSeconds int64,
	repository *resolvedRepositoryCheckout,
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

	createSandboxResp, err := client.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Backend: backendName,
		Options: &cleanroomv1.SandboxOptions{
			LaunchSeconds: launchSeconds,
		},
		Policy:             compiled.ToProto(),
		RepositoryCheckout: repositoryCheckoutProto(repository),
	})
	if err != nil {
		return "", nil, fmt.Errorf("create sandbox: %w", err)
	}

	sandbox := createSandboxResp.GetSandbox()
	sandboxID := strings.TrimSpace(sandbox.GetSandboxId())
	if sandboxID == "" {
		return "", nil, errors.New("create sandbox: response missing sandbox id")
	}

	return sandboxID, sandbox, nil
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
