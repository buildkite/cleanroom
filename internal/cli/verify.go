package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/buildkite/cleanroom/internal/bake"
)

type VerifyCommand struct {
	SporeDir string `arg:"" optional:"" name:"spore-dir" help:"Spore directory to verify; omit to read spore --json inspect output from stdin"`
	Dir      string `help:"Audit the bake key against a repository directory's current policy and commit"`
	Spore    string `help:"spore executable" default:"spore"`
	JSON     bool   `help:"Print the verified provenance as JSON"`
}

func (c *VerifyCommand) Run(ctx *runtimeContext) error {
	annotations, err := c.loadAnnotations(ctx)
	if err != nil {
		return err
	}
	prov, err := bake.ParseProvenance(annotations)
	if err != nil {
		return err
	}
	if c.Dir != "" {
		cwd, err := resolveCWD(ctx.CWD, c.Dir)
		if err != nil {
			return err
		}
		compiled, _, err := ctx.Loader.LoadAndCompile(cwd)
		if err != nil {
			return err
		}
		if err := bake.AuditKey(prov, compiled, bake.CollectGitFacts(cwd)); err != nil {
			return err
		}
	}

	if c.JSON {
		encoder := json.NewEncoder(ctx.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(prov)
	}
	for _, line := range prov.Summary() {
		if _, err := fmt.Fprintln(ctx.Stdout, line); err != nil {
			return err
		}
	}
	if len(prov.MediationServices) > 0 {
		if _, err := fmt.Fprintln(ctx.Stdout, "requires a lineage gateway: cleanroom gateway serve --for <spore-dir> --socket <path>"); err != nil {
			return err
		}
	}
	if c.SporeDir != "" {
		if _, err := fmt.Fprintln(ctx.Stdout, "run               : "+prov.RunFromInvocation(c.SporeDir)); err != nil {
			return err
		}
	}
	if c.Dir != "" {
		if _, err := fmt.Fprintln(ctx.Stdout, "bake key matches the repository's current policy and commit"); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintln(ctx.Stdout, "verified")
	return err
}

func (c *VerifyCommand) loadAnnotations(ctx *runtimeContext) (map[string]string, error) {
	if c.SporeDir != "" {
		runner := &bake.CLIRunner{Spore: c.Spore, Stdout: ctx.stderr(), Stderr: ctx.stderr()}
		return runner.InspectAnnotations(c.SporeDir)
	}
	const maxInspectBytes = 32 << 20
	raw, err := io.ReadAll(io.LimitReader(ctx.stdin(), maxInspectBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read inspect output from stdin: %w", err)
	}
	if len(raw) > maxInspectBytes {
		return nil, errors.New("spore inspect output on stdin exceeds 32MiB")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("cleanroom verify requires a spore directory argument or spore --json inspect output on stdin")
	}
	var inspect struct {
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(raw, &inspect); err != nil {
		return nil, fmt.Errorf("decode spore inspect output: %w", err)
	}
	return inspect.Annotations, nil
}
