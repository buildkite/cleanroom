package cli

import (
	"fmt"
	"strings"

	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

type AgentCommand struct {
	clientFlags
	Chdir     string `short:"c" help:"Change to this directory before running commands"`
	Backend   string `help:"Execution backend (defaults to runtime config or firecracker)"`
	SandboxID string `help:"Reuse an existing sandbox instead of creating a new one"`

	LaunchSeconds int64 `help:"VM boot/guest-agent readiness timeout in seconds"`

	Command string   `arg:"" required:"" help:"Agent command to run inside the sandbox"`
	Args    []string `arg:"" passthrough:"" optional:"" help:"Arguments to pass to the agent command (prefix with '--' to separate cleanroom and agent flags)"`
}

func (a *AgentCommand) Run(ctx *runtimeContext) error {
	keepSandbox := strings.TrimSpace(a.SandboxID) == ""
	command, err := agentShellCommand(a.Command, a.Args, ctx.Config.Agents)
	if err != nil {
		return err
	}

	console := ConsoleCommand{
		clientFlags:   a.clientFlags,
		Chdir:         a.Chdir,
		Backend:       a.Backend,
		In:            a.SandboxID,
		Keep:          keepSandbox,
		LaunchSeconds: a.LaunchSeconds,
		Command:       []string{"sh", "-lc", command},
	}
	return console.Run(ctx)
}

func agentShellCommand(name string, rawArgs []string, agents map[string]runtimeconfig.Agent) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("agent command is required")
	}

	args := append([]string(nil), rawArgs...)
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}

	spec := agents[name]
	command := strings.TrimSpace(spec.Command)
	if command == "" {
		command = name
	}
	test := strings.TrimSpace(spec.Test)
	if test == "" {
		test = "command -v " + shellQuote(name) + " >/dev/null 2>&1"
	}

	var script strings.Builder
	script.WriteString("set -e\n")
	if install := strings.TrimSpace(spec.Install); install != "" {
		script.WriteString("if ! (")
		script.WriteString(test)
		script.WriteString("); then\n")
		script.WriteString(install)
		script.WriteString("\nfi\n")
	}
	script.WriteString("if ! (")
	script.WriteString(test)
	script.WriteString("); then\n")
	script.WriteString("printf '%s\\n' ")
	script.WriteString(shellQuote("cleanroom: agent command not found: " + name))
	script.WriteString(" >&2\n")
	script.WriteString("exit 127\n")
	script.WriteString("fi\n")
	script.WriteString("exec ")
	script.WriteString(command)
	for _, arg := range args {
		script.WriteByte(' ')
		script.WriteString(shellQuote(arg))
	}
	return script.String(), nil
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
