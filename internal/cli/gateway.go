package cli

import (
	"fmt"

	"github.com/buildkite/cleanroom/internal/bake"
	"github.com/buildkite/cleanroom/internal/mediation"
)

type GatewayCommand struct {
	Serve GatewayServeCommand `cmd:"" help:"Serve the lineage gateway on a Unix socket"`
}

type GatewayServeCommand struct {
	For    string `help:"Baked spore directory to serve; its bake key is audited against --dir before any grant resolves"`
	Dir    string `required:"" help:"Repository whose policy and git facts define the lineage scope; the trust root for grants"`
	Socket string `required:"" help:"Unix socket path to serve on; bind into VMs with spore --bind-service cleanroom-gateway=unix:PATH"`
	Grants string `help:"Gateway grants config (default: ~/.config/cleanroom/gateway.yaml)"`
	Spore  string `help:"spore executable" default:"spore"`
}

func (c *GatewayServeCommand) Run(ctx *runtimeContext) error {
	grantsPath := c.Grants
	if grantsPath == "" {
		grantsPath = mediation.DefaultConfigPath()
	}
	config, err := mediation.LoadConfig(grantsPath)
	if err != nil {
		return err
	}

	// Grants always resolve from the local repository, never from spore
	// annotations: annotations are attacker-influenced, so a foreign spore
	// could otherwise forge the remote, policy hash, and mediation requests
	// of a granted lineage. With --for, the spore's bake key must match the
	// repository's current policy and commit before anything is served.
	cwd, err := resolveCWD(ctx.CWD, c.Dir)
	if err != nil {
		return err
	}
	compiled, _, err := ctx.Loader.LoadAndCompile(cwd)
	if err != nil {
		return err
	}
	var exclusions []string
	if c.For != "" {
		sporeDir, err := resolveCWD(ctx.CWD, c.For)
		if err != nil {
			return err
		}
		exclusions = bake.ArtifactExclusions(cwd, sporeDir)
	}
	git := bake.CollectGitFactsExcluding(cwd, exclusions)
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
		if err := bake.AuditKey(prov, compiled, git); err != nil {
			return fmt.Errorf("refusing to serve %s: %w", c.For, err)
		}
	}
	requested := compiled.Mediation
	facts := mediation.LineageFacts{Remote: git.Remote, PolicyHash: compiled.Hash, Dirty: git.Dirty}

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
