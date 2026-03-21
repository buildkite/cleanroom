package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/buildkite/cleanroom/internal/paths"
)

type StatusCommand struct {
	ExecutionID string `name:"execution-id" help:"Execution ID to inspect"`
	Last        bool   `help:"Inspect the most recent retained execution artifacts"`
}

type retainedExecutionEntry struct {
	ID         string
	ModifiedAt time.Time
}

func (s *StatusCommand) Run(ctx *runtimeContext) error {
	baseDir, err := paths.ExecutionBaseDir()
	if err != nil {
		return fmt.Errorf("resolve execution artifacts base directory: %w", err)
	}
	if s.ExecutionID != "" && s.Last {
		return errors.New("choose either --execution-id or --last")
	}
	if s.ExecutionID != "" {
		return inspectExecutionArtifacts(ctx.Stdout, baseDir, s.ExecutionID)
	}
	entries, err := listRetainedExecutions(baseDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			_, werr := fmt.Fprintf(ctx.Stdout, "no retained executions found (%s does not exist)\n", baseDir)
			return werr
		}
		return err
	}
	if len(entries) == 0 {
		_, err := fmt.Fprintf(ctx.Stdout, "no retained executions found in %s\n", baseDir)
		return err
	}
	if s.Last {
		return inspectExecutionArtifacts(ctx.Stdout, baseDir, entries[0].ID)
	}

	_, err = fmt.Fprintf(ctx.Stdout, "retained executions in %s:\n", baseDir)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(ctx.Stdout, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tMODIFIED"); err != nil {
		return err
	}
	for _, entry := range entries {
		if _, err := fmt.Fprintf(tw, "%s\t%s\n", entry.ID, entry.ModifiedAt.Format(time.RFC3339)); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func inspectExecutionArtifacts(stdout *os.File, baseDir, executionID string) error {
	artifactsDir := filepath.Join(baseDir, executionID)
	if _, err := os.Stat(artifactsDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("execution %q not found in %s", executionID, baseDir)
		}
		return err
	}
	if _, err := fmt.Fprintf(stdout, "execution artifacts: %s\n", artifactsDir); err != nil {
		return err
	}
	obsPath := filepath.Join(artifactsDir, "execution-observability.json")
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

func listRetainedExecutions(baseDir string) ([]retainedExecutionEntry, error) {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return nil, err
	}

	retained := make([]retainedExecutionEntry, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		retained = append(retained, retainedExecutionEntry{
			ID:         entry.Name(),
			ModifiedAt: info.ModTime(),
		})
	}

	sort.Slice(retained, func(i, j int) bool {
		if retained[i].ModifiedAt.Equal(retained[j].ModifiedAt) {
			return retained[i].ID < retained[j].ID
		}
		return retained[i].ModifiedAt.After(retained[j].ModifiedAt)
	})
	return retained, nil
}
