package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/buildkite/cleanroom/internal/paths"
)

type StatusCommand struct {
	RunID   string `help:"Run ID to inspect"`
	LastRun bool   `help:"Inspect the most recent run"`
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
