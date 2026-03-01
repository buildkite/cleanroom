package cli

type AgentCommand struct {
	Codex AgentCodexCommand `cmd:"" help:"Create and run a long-running Codex agent session in a sandbox"`
}

type AgentCodexCommand struct {
	clientFlags
	Chdir     string `short:"c" help:"Change to this directory before running commands"`
	Backend   string `help:"Execution backend (defaults to runtime config or firecracker)"`
	SandboxID string `help:"Reuse an existing sandbox instead of creating a new one"`

	LaunchSeconds int64 `help:"VM boot/guest-agent readiness timeout in seconds"`

	Args []string `arg:"" passthrough:"" optional:"" help:"Arguments to pass to codex (prefix with '--' to separate cleanroom and codex flags)"`
}

func (a *AgentCodexCommand) Run(ctx *runtimeContext) error {
	command := make([]string, 0, len(a.Args)+1)
	command = append(command, "codex")
	command = append(command, a.Args...)

	console := ConsoleCommand{
		clientFlags:   a.clientFlags,
		Chdir:         a.Chdir,
		Backend:       a.Backend,
		SandboxID:     a.SandboxID,
		LaunchSeconds: a.LaunchSeconds,
		Command:       command,
	}
	return console.Run(ctx)
}
