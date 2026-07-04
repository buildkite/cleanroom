package cli

import (
	"errors"
	"fmt"

	"github.com/buildkite/cleanroom/internal/bake"
	"github.com/buildkite/cleanroom/internal/mediation"
)

type GatewayCommand struct {
	Serve GatewayServeCommand `cmd:"" help:"Serve the lineage gateway on a Unix socket"`
}

type GatewayServeCommand struct {
	For    string `help:"Serve the scope of a baked spore directory (verifies provenance)"`
	Dir    string `help:"Serve the scope of a repository's policy (bake-time)"`
	Socket string `required:"" help:"Unix socket path to serve on; bind into VMs with spore --bind-service cleanroom-gateway=unix:PATH"`
	Grants string `help:"Gateway grants config (default: ~/.config/cleanroom/gateway.yaml)"`
	Spore  string `help:"spore executable" default:"spore"`
}

func (c *GatewayServeCommand) Run(ctx *runtimeContext) error {
	if (c.For == "") == (c.Dir == "") {
		return errors.New("cleanroom gateway serve requires exactly one of --for <spore-dir> or --dir <repo>")
	}
	grantsPath := c.Grants
	if grantsPath == "" {
		grantsPath = mediation.DefaultConfigPath()
	}
	config, err := mediation.LoadConfig(grantsPath)
	if err != nil {
		return err
	}

	var requested []string
	var facts mediation.LineageFacts
	if c.For != "" {
		runner := &bake.CLIRunner{Spore: c.Spore, Stdout: ctx.stderr(), Stderr: ctx.stderr()}
		annotations, err := runner.InspectAnnotations(c.For)
		if err != nil {
			return err
		}
		prov, err := bake.ParseProvenance(annotations)
		if err != nil {
			return err
		}
		requested = prov.MediationServices
		facts = mediation.LineageFacts{Remote: prov.GitRemote, PolicyHash: prov.PolicyHash, Dirty: prov.GitDirty}
	} else {
		cwd, err := resolveCWD(ctx.CWD, c.Dir)
		if err != nil {
			return err
		}
		compiled, _, err := ctx.Loader.LoadAndCompile(cwd)
		if err != nil {
			return err
		}
		git := bake.CollectGitFacts(cwd)
		requested = compiled.Mediation
		facts = mediation.LineageFacts{Remote: git.Remote, PolicyHash: compiled.Hash, Dirty: git.Dirty}
	}

	scope, err := mediation.ResolveScope(config, requested, facts)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(scope))
	for name := range scope {
		names = append(names, name)
	}
	if _, err := fmt.Fprintf(ctx.Stdout, "cleanroom gateway: serving %v on %s for lineage (remote %s, policy %.12s)\n", names, c.Socket, facts.Remote, facts.PolicyHash); err != nil {
		return err
	}
	server := mediation.NewServer(scope, ctx.stderr())
	return server.Serve(c.Socket)
}
