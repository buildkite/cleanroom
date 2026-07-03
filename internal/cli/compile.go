package cli

import (
	"fmt"

	"github.com/buildkite/cleanroom/internal/bake"
)

type CompileCommand struct {
	Dir string `arg:"" optional:"" default:"." help:"Repository directory containing cleanroom policy"`
}

func (c *CompileCommand) Run(ctx *runtimeContext) error {
	cwd, err := resolveCWD(ctx.CWD, c.Dir)
	if err != nil {
		return err
	}
	compiled, _, err := ctx.Loader.LoadAndCompile(cwd)
	if err != nil {
		return err
	}
	inputs, err := bake.Compile(compiled)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(ctx.Stdout, bake.QuoteArgs(inputs.Args()))
	return err
}

type StampCommand struct {
	Dir string `arg:"" optional:"" default:"." help:"Repository directory containing cleanroom policy. Output contains shell-quoted values; consume via eval, e.g. eval \"spore create name $(cleanroom compile .) $(cleanroom stamp .)\""`
}

func (c *StampCommand) Run(ctx *runtimeContext) error {
	cwd, err := resolveCWD(ctx.CWD, c.Dir)
	if err != nil {
		return err
	}
	compiled, policySource, err := ctx.Loader.LoadAndCompile(cwd)
	if err != nil {
		return err
	}
	inputs, err := bake.Compile(compiled)
	if err != nil {
		return err
	}
	annotations, err := bake.Stamp(cwd, policySource, compiled, ctx.Version, inputs.NetworkRules)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(ctx.Stdout, bake.QuoteArgs(bake.AnnotationArgs(annotations)))
	return err
}
