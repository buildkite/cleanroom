package cli

import (
	"errors"
	"strings"
)

type ExposeCommand struct {
	clientFlags
	SandboxID   string   `name:"in" aliases:"sandbox-id" required:"" help:"Sandbox ID to expose"`
	Expose      []string `name:"expose" help:"Expose raw TCP as <guest-port> or <host-port>:<guest-port>"`
	ExposeHTTPS []string `name:"expose-https" help:"Expose HTTPS as [name:]<guest-port>, or configured expose.https routes when omitted"`
}

type PortForwardCommand struct {
	clientFlags
	SandboxID string   `name:"in" aliases:"sandbox-id" required:"" help:"Sandbox ID to forward to"`
	Specs     []string `arg:"" required:"" help:"Forward specs: <guest-port> or <host-port>:<guest-port>"`
}

func (c *ExposeCommand) Run(ctx *runtimeContext) error {
	exposures, err := parseExposureFlags(c.Expose, c.ExposeHTTPS)
	if err != nil {
		return err
	}
	exposures, err = resolveRequestedExposures(ctx, ctx.CWD, c.SandboxID, exposures)
	if err != nil {
		return err
	}
	return runForegroundClientExposures(ctx, c.clientFlags, c.SandboxID, exposures)
}

func (c *PortForwardCommand) Run(ctx *runtimeContext) error {
	if len(c.Specs) == 0 {
		return errors.New("at least one port forward spec is required")
	}
	for _, spec := range c.Specs {
		if strings.TrimSpace(spec) == "" {
			return errors.New("port forward spec cannot be empty")
		}
	}
	exposures, err := parseExposureFlags(c.Specs, nil)
	if err != nil {
		return err
	}
	return runForegroundClientExposures(ctx, c.clientFlags, c.SandboxID, exposures)
}
