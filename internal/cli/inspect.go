package cli

import (
	"fmt"
	"strings"

	"go.jetify.com/typeid"
)

type InspectCommand struct {
	clientFlags
	ID   string `arg:"" required:"" help:"Sandbox, execution, or snapshot ID to inspect"`
	JSON bool   `help:"Print inspection as JSON"`
}

func (c *InspectCommand) Run(ctx *runtimeContext) error {
	targetID := strings.TrimSpace(c.ID)
	kind, err := inspectTargetKind(targetID)
	if err != nil {
		return err
	}

	switch kind {
	case "sandbox":
		return (&SandboxInspectCommand{
			clientFlags: c.clientFlags,
			SandboxID:   targetID,
			JSON:        c.JSON,
		}).Run(ctx)
	case "execution":
		return (&ExecutionInspectCommand{
			clientFlags: c.clientFlags,
			ExecutionID: targetID,
			JSON:        c.JSON,
		}).Run(ctx)
	case "snapshot":
		return (&SnapshotInspectCommand{
			clientFlags: c.clientFlags,
			SnapshotID:  targetID,
			JSON:        c.JSON,
		}).Run(ctx)
	default:
		return fmt.Errorf("unsupported inspect target %q", targetID)
	}
}

func inspectTargetKind(id string) (string, error) {
	targetID := strings.TrimSpace(id)
	if targetID == "" {
		return "", fmt.Errorf("missing inspect target id")
	}

	parsed, err := typeid.FromString(targetID)
	if err == nil {
		switch parsed.Prefix() {
		case "":
			return "sandbox", nil
		case "cr":
			return "sandbox", nil
		case "exec":
			return "execution", nil
		case "snap":
			return "snapshot", nil
		default:
			return "", fmt.Errorf("unsupported inspect target prefix %q", parsed.Prefix())
		}
	}

	switch {
	case strings.HasPrefix(targetID, "cr-"):
		return "sandbox", nil
	case strings.HasPrefix(targetID, "exec-"):
		return "execution", nil
	case strings.HasPrefix(targetID, "snap-"):
		return "snapshot", nil
	default:
		return "", fmt.Errorf("unrecognized inspect target %q: expected sandbox, execution, or snapshot id", targetID)
	}
}
