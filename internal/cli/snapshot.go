package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

type SnapshotCommand struct {
	Create  SnapshotCreateCommand  `cmd:"" help:"Create a snapshot from a sandbox"`
	Inspect SnapshotInspectCommand `cmd:"" help:"Inspect snapshot metadata"`
	List    SnapshotListCommand    `name:"ls" aliases:"list" cmd:"" help:"List snapshots"`
	Delete  SnapshotDeleteCommand  `name:"rm" aliases:"delete" cmd:"" help:"Delete a snapshot"`
}

type SnapshotCreateCommand struct {
	clientFlags
	SandboxID string `arg:"" required:"" help:"Sandbox ID to snapshot"`
	Name      string `help:"Optional snapshot label"`
	JSON      bool   `help:"Print snapshot as JSON"`
}

type SnapshotInspectCommand struct {
	clientFlags
	SnapshotID string `arg:"" required:"" help:"Snapshot ID to inspect"`
	JSON       bool   `help:"Print snapshot as JSON"`
}

type SnapshotListCommand struct {
	clientFlags
	JSON bool `help:"Print snapshots as JSON"`
}

type SnapshotDeleteCommand struct {
	clientFlags
	SnapshotID string `arg:"" required:"" help:"Snapshot ID to delete"`
}

func (c *SnapshotCreateCommand) Run(ctx *runtimeContext) error {
	client, err := c.connect(ctx)
	if err != nil {
		return err
	}

	resp, err := client.CreateSnapshot(context.Background(), &cleanroomv1.CreateSnapshotRequest{
		SandboxId: c.SandboxID,
		Name:      c.Name,
	})
	if err != nil {
		return explainSnapshotRuntimeDisabledError(err, ctx)
	}
	if c.JSON {
		enc := json.NewEncoder(ctx.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp.GetSnapshot())
	}
	_, err = fmt.Fprintln(ctx.Stdout, resp.GetSnapshot().GetSnapshotId())
	return err
}

func (c *SnapshotInspectCommand) Run(ctx *runtimeContext) error {
	client, err := c.connect(ctx)
	if err != nil {
		return err
	}

	resp, err := client.GetSnapshot(context.Background(), &cleanroomv1.GetSnapshotRequest{
		SnapshotId: c.SnapshotID,
	})
	if err != nil {
		return err
	}
	if c.JSON {
		enc := json.NewEncoder(ctx.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp.GetSnapshot())
	}
	return writeSnapshotInspect(ctx.Stdout, resp.GetSnapshot())
}

func (c *SnapshotListCommand) Run(ctx *runtimeContext) error {
	client, err := c.connect(ctx)
	if err != nil {
		return err
	}

	resp, err := client.ListSnapshots(context.Background(), &cleanroomv1.ListSnapshotsRequest{})
	if err != nil {
		return err
	}
	if c.JSON {
		enc := json.NewEncoder(ctx.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp.GetSnapshots())
	}
	if len(resp.GetSnapshots()) == 0 {
		_, err := fmt.Fprintln(ctx.Stdout, "no snapshots")
		return err
	}

	tw := tabwriter.NewWriter(ctx.Stdout, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tSANDBOX\tBACKEND\tNAME\tCREATED"); err != nil {
		return err
	}
	for _, snapshot := range resp.GetSnapshots() {
		created := ""
		if snapshot.GetCreatedAt() != nil {
			created = snapshot.GetCreatedAt().AsTime().Format(time.RFC3339)
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", snapshot.GetSnapshotId(), snapshot.GetSourceSandboxId(), snapshot.GetBackend(), snapshot.GetName(), created); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func (c *SnapshotDeleteCommand) Run(ctx *runtimeContext) error {
	client, err := c.connect(ctx)
	if err != nil {
		return err
	}

	resp, err := client.DeleteSnapshot(context.Background(), &cleanroomv1.DeleteSnapshotRequest{
		SnapshotId: c.SnapshotID,
	})
	if err != nil {
		return explainSnapshotRuntimeDisabledError(err, ctx)
	}
	_, err = fmt.Fprintln(ctx.Stdout, resp.GetMessage())
	return err
}

func writeSnapshotInspect(w io.Writer, snapshot *cleanroomv1.Snapshot) error {
	if snapshot == nil {
		return errors.New("missing snapshot")
	}

	lines := []string{
		"snapshot_id: " + snapshot.GetSnapshotId(),
		"source_sandbox_id: " + snapshot.GetSourceSandboxId(),
		"backend: " + snapshot.GetBackend(),
		"policy_hash: " + snapshot.GetPolicyHash(),
		"name: " + snapshot.GetName(),
		"storage_driver: " + snapshot.GetStorageDriver(),
		"storage_ref: " + snapshot.GetStorageRef(),
	}
	if snapshot.GetCreatedAt() != nil {
		lines = append(lines, "created_at: "+snapshot.GetCreatedAt().AsTime().Format(time.RFC3339))
	}
	for _, line := range lines {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}

	repository := snapshot.GetRepositoryCheckout()
	if repository == nil {
		return nil
	}
	if _, err := fmt.Fprintln(w, "repository_checkout:"); err != nil {
		return err
	}
	for _, line := range []string{
		"  remote_url: " + repository.GetRemoteUrl(),
		"  commit_sha: " + repository.GetCommitSha(),
		"  destination_dir: " + repository.GetDestinationDir(),
		fmt.Sprintf("  submodules: %t", repository.GetSubmodules()),
		"  branch: " + repository.GetBranch(),
	} {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

func explainSnapshotRuntimeDisabledError(err error, ctx *runtimeContext) error {
	if err == nil {
		return nil
	}
	backendName, ok := snapshotDisabledBackendName(err)
	if !ok {
		return err
	}
	return fmt.Errorf("%w (disabled by runtime config; set %s in %s)", err, snapshotConfigEnableHint(backendName), ctx.ConfigPath)
}

func snapshotDisabledBackendName(err error) (string, bool) {
	message := err.Error()
	const prefix = `snapshots are not enabled for backend "`
	start := strings.Index(message, prefix)
	if start < 0 {
		return "", false
	}
	start += len(prefix)
	end := strings.Index(message[start:], `"`)
	if end < 0 {
		return "", false
	}
	return message[start : start+end], true
}
