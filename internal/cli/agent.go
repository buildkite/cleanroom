package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/buildkite/cleanroom/internal/controlclient"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

type AgentCommand struct {
	clientFlags
	Chdir     string `short:"c" help:"Change to this directory before running commands"`
	Backend   string `help:"Execution backend (defaults to runtime config or firecracker)"`
	SandboxID string `help:"Reuse an existing sandbox instead of creating a new one"`

	DangerouslyAllowAll bool  `name:"dangerously-allow-all" help:"Disable network egress filtering for a newly created sandbox"`
	LaunchSeconds       int64 `help:"VM boot/guest-agent readiness timeout in seconds"`

	Agent string   `arg:"" required:"" enum:"${agent_names}" help:"Agent name to run inside the sandbox (one of ${enum})"`
	Args  []string `arg:"" passthrough:"" optional:"" help:"Arguments to pass to the agent (prefix with '--' to separate cleanroom and agent flags)"`
}

func (a *AgentCommand) Run(ctx *runtimeContext) error {
	keepSandbox := strings.TrimSpace(a.SandboxID) == ""
	command, err := agentShellCommand(a.Agent, a.Args, ctx.Config.Agents)
	if err != nil {
		return err
	}
	credentials, err := agentCredentialArchive(a.Agent, ctx.Config.Agents)
	if err != nil {
		return err
	}

	console := ConsoleCommand{
		clientFlags:         a.clientFlags,
		Chdir:               a.Chdir,
		Backend:             a.Backend,
		In:                  a.SandboxID,
		Keep:                keepSandbox,
		DangerouslyAllowAll: a.DangerouslyAllowAll,
		LaunchSeconds:       a.LaunchSeconds,
		Command:             []string{"sh", "-lc", command},
	}
	if len(credentials) > 0 {
		console.preAttach = func(callCtx context.Context, client *controlclient.Client, sandboxID string) error {
			return extractAgentCredentialArchive(callCtx, client, sandboxID, credentials)
		}
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

	spec := resolveAgentSpec(name, agents)
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

func agentCredentialArchive(name string, agents map[string]runtimeconfig.Agent) ([]byte, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}

	credentials := agentCredentials(name, agents)
	if len(credentials) == 0 {
		return nil, nil
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	wrote := false
	for _, credential := range credentials {
		source, err := expandHomePath(credential.Source)
		if err != nil {
			return nil, err
		}
		info, err := os.Lstat(source)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read agent credential %q: %w", credential.Source, err)
		}
		target, err := guestCredentialPath(credential.Target)
		if err != nil {
			return nil, err
		}
		if err := addCredentialToArchive(tw, source, target, info); err != nil {
			return nil, err
		}
		wrote = true
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("write agent credential archive: %w", err)
	}
	if !wrote {
		return nil, nil
	}
	return buf.Bytes(), nil
}

func agentCredentials(name string, agents map[string]runtimeconfig.Agent) []runtimeconfig.AgentCredential {
	if agent := resolveAgentSpec(name, agents); len(agent.Credentials) > 0 {
		return append([]runtimeconfig.AgentCredential(nil), agent.Credentials...)
	}
	return nil
}

func resolveAgentSpec(name string, agents map[string]runtimeconfig.Agent) runtimeconfig.Agent {
	name = strings.TrimSpace(name)
	if name == "" {
		return runtimeconfig.Agent{}
	}
	spec := defaultRuntimeAgentConfig()[name]
	if configured, ok := agents[name]; ok {
		if configured.Command != "" {
			spec.Command = configured.Command
		}
		if configured.Test != "" {
			spec.Test = configured.Test
		}
		if configured.Install != "" {
			spec.Install = configured.Install
		}
		if len(configured.Credentials) > 0 {
			spec.Credentials = configured.Credentials
		}
	}
	return spec
}

func extractAgentCredentialArchive(ctx context.Context, client *controlclient.Client, sandboxID string, archive []byte) error {
	if err := extractSandboxArchive(ctx, client, sandboxID, "/", bytes.NewReader(archive)); err != nil {
		return fmt.Errorf("copy agent credentials: %w", err)
	}
	return nil
}

func addCredentialToArchive(tw *tar.Writer, source, target string, info fs.FileInfo) error {
	if info.IsDir() {
		return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return fmt.Errorf("walk agent credential %q: %w", source, err)
			}
			entryInfo, err := entry.Info()
			if err != nil {
				return fmt.Errorf("stat agent credential %q: %w", path, err)
			}
			rel, err := filepath.Rel(source, path)
			if err != nil {
				return err
			}
			entryTarget := target
			if rel != "." {
				entryTarget = filepath.ToSlash(filepath.Join(target, rel))
			}
			return writeCredentialArchiveEntry(tw, path, entryTarget, entryInfo)
		})
	}
	return writeCredentialArchiveEntry(tw, source, target, info)
}

func writeCredentialArchiveEntry(tw *tar.Writer, source, target string, info fs.FileInfo) error {
	link := ""
	if info.Mode()&os.ModeSymlink != 0 {
		var err error
		link, err = os.Readlink(source)
		if err != nil {
			return fmt.Errorf("read agent credential symlink %q: %w", source, err)
		}
	}
	header, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return fmt.Errorf("create agent credential archive header %q: %w", source, err)
	}
	header.Name = target
	if isCodexConfigTarget(target) {
		raw, err := os.ReadFile(source)
		if err != nil {
			return fmt.Errorf("read agent credential %q: %w", source, err)
		}
		data := codexConfigWithWorkspaceTrust(raw)
		header.Size = int64(len(data))
		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("write agent credential archive header %q: %w", source, err)
		}
		if _, err := tw.Write(data); err != nil {
			return fmt.Errorf("write agent credential %q: %w", source, err)
		}
		return nil
	}
	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("write agent credential archive header %q: %w", source, err)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	file, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open agent credential %q: %w", source, err)
	}
	defer file.Close()
	if _, err := io.Copy(tw, file); err != nil {
		return fmt.Errorf("write agent credential %q: %w", source, err)
	}
	return nil
}

func isCodexConfigTarget(target string) bool {
	return filepath.ToSlash(filepath.Clean(target)) == "root/.codex/config.toml"
}

func codexConfigWithWorkspaceTrust(raw []byte) []byte {
	if bytes.Contains(raw, []byte(`[projects."/workspace"]`)) || bytes.Contains(raw, []byte(`[projects.'/workspace']`)) {
		return raw
	}
	out := append([]byte(nil), raw...)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, []byte("\n[projects.\"/workspace\"]\ntrust_level = \"trusted\"\n")...)
	return out
}

func expandHomePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("agent credential source is required")
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

func guestCredentialPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("agent credential target is required")
	}
	switch {
	case path == "~":
		path = "/root"
	case strings.HasPrefix(path, "~/"):
		path = "/root/" + path[2:]
	}
	path = filepath.ToSlash(filepath.Clean(path))
	path = strings.TrimPrefix(path, "/")
	if path == "." || path == "" || strings.HasPrefix(path, "../") || path == ".." {
		return "", fmt.Errorf("invalid agent credential target %q", path)
	}
	return path, nil
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
