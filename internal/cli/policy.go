package cli

import (
	"encoding/json"
	"fmt"
	"strings"
)

type PolicyCommand struct {
	Validate PolicyValidateCommand `cmd:"" help:"Validate repository policy"`
}

type PolicyValidateCommand struct {
	Chdir string `name:"chdir" short:"C" help:"Directory containing cleanroom policy"`
	JSON  bool   `help:"Print the compiled policy as JSON"`
}

func (c *PolicyValidateCommand) Run(ctx *runtimeContext) error {
	cwd, err := resolveCWD(ctx.CWD, c.Chdir)
	if err != nil {
		return err
	}
	compiled, source, err := ctx.Loader.LoadAndCompile(cwd)
	if err != nil {
		return err
	}

	if c.JSON {
		payload := map[string]any{
			"source": source,
			"policy": compiled,
		}
		enc := json.NewEncoder(ctx.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(payload)
	}

	color := shouldUseANSI(ctx.Stdout)
	var out strings.Builder
	out.WriteString(renderStatusValueLine("policy valid", source, defaultTerminalPalette().info, color))
	out.WriteByte('\n')
	out.WriteString(renderKeyValueLine("", "policy hash", compiled.Hash, color, defaultTerminalPalette()))
	out.WriteByte('\n')
	_, err = fmt.Fprint(ctx.Stdout, out.String())
	return err
}
