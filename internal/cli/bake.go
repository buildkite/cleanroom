package cli

import (
	"github.com/buildkite/cleanroom/internal/bake"
)

type BakeCommand struct {
	Dir           string `arg:"" optional:"" default:"." help:"Repository directory containing cleanroom policy"`
	Out           string `name:"out" required:"" help:"Output spore directory"`
	GatewaySocket string `name:"gateway-socket" help:"Live gateway Unix socket to bind as gateway.cleanroom.internal:8170 (required when policy requests mediation services)"`
	Spore         string `help:"spore executable" default:"spore"`
}

func (c *BakeCommand) Run(ctx *runtimeContext) error {
	cwd, err := resolveCWD(ctx.CWD, c.Dir)
	if err != nil {
		return err
	}
	compiled, policySource, err := ctx.Loader.LoadAndCompile(cwd)
	if err != nil {
		return err
	}
	runner := &bake.CLIRunner{
		Spore:  c.Spore,
		Stdout: ctx.Stdout,
		Stderr: ctx.stderr(),
	}
	_, err = bake.Run(compiled, bake.Options{
		Dir:           cwd,
		PolicySource:  policySource,
		Out:           c.Out,
		GatewaySocket: c.GatewaySocket,
		Version:       ctx.Version,
		Runner:        runner,
		Log:           ctx.Stdout,
	})
	return err
}
