package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/buildkite/cleanroom/internal/controlclient"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

type resolvedRepositoryCheckout struct {
	RemoteURL      string
	RemoteHost     string
	CommitSHA      string
	DestinationDir string
	Submodules     bool
	Dirty          bool
	GitConfigKey   string
	GitConfigValue string
}

var gitCredentialFill = gitCredentialFillFromHost

func resolveRepositoryCheckout(cwd string, loader policyLoader) (*resolvedRepositoryCheckout, error) {
	if loader == nil {
		return nil, nil
	}

	repository, _, err := loader.LoadRepository(cwd)
	if err != nil {
		return nil, err
	}
	if !repository.Enabled() {
		return nil, nil
	}

	switch repository.Mode {
	case "current-repo":
	default:
		return nil, fmt.Errorf("unsupported repository.mode %q", repository.Mode)
	}

	repoRoot, err := gitOutput(cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}
	dirty, err := gitOutput(repoRoot, "status", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("inspect repository status: %w", err)
	}

	commitSHA, err := gitOutput(repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("resolve repository HEAD: %w", err)
	}
	remoteURL, err := gitOutput(repoRoot, "remote", "get-url", repository.Remote)
	if err != nil {
		return nil, fmt.Errorf("resolve repository remote %q: %w", repository.Remote, err)
	}
	canonicalURL, remoteHost, err := canonicalizeGitRemoteURL(remoteURL)
	if err != nil {
		return nil, err
	}
	gitConfigKey, gitConfigValue, err := resolveGitCredentialConfig(repoRoot, canonicalURL, remoteHost)
	if err != nil {
		return nil, err
	}

	compiled, _, err := loader.LoadAndCompile(cwd)
	if err != nil {
		return nil, err
	}
	if compiled != nil && !compiled.Allows(remoteHost, 443) {
		return nil, fmt.Errorf("repository remote host %q is not allowed by sandbox policy", remoteHost)
	}

	return &resolvedRepositoryCheckout{
		RemoteURL:      canonicalURL,
		RemoteHost:     remoteHost,
		CommitSHA:      strings.TrimSpace(commitSHA),
		DestinationDir: repository.Path,
		Submodules:     repository.Submodules,
		Dirty:          strings.TrimSpace(dirty) != "",
		GitConfigKey:   gitConfigKey,
		GitConfigValue: gitConfigValue,
	}, nil
}

func maybeResolveRepositoryCheckout(cwd string, loader policyLoader, existingSandboxID string) (*resolvedRepositoryCheckout, error) {
	if strings.TrimSpace(existingSandboxID) != "" {
		return nil, nil
	}
	return resolveRepositoryCheckout(cwd, loader)
}

func canonicalizeGitRemoteURL(raw string) (string, string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", errors.New("repository remote URL is empty")
	}

	if strings.HasPrefix(trimmed, "https://") {
		parsed, err := url.Parse(trimmed)
		if err != nil {
			return "", "", fmt.Errorf("parse repository remote URL %q: %w", trimmed, err)
		}
		host := strings.TrimSpace(strings.ToLower(parsed.Hostname()))
		if host == "" {
			return "", "", fmt.Errorf("repository remote URL %q has no host", trimmed)
		}
		if parsed.Port() != "" && parsed.Port() != "443" {
			return "", "", fmt.Errorf("repository remote URL %q uses unsupported non-default HTTPS port", trimmed)
		}
		parsed.User = nil
		parsed.Host = host
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return parsed.String(), host, nil
	}

	if strings.HasPrefix(trimmed, "ssh://") {
		parsed, err := url.Parse(trimmed)
		if err != nil {
			return "", "", fmt.Errorf("parse repository remote URL %q: %w", trimmed, err)
		}
		host := strings.TrimSpace(strings.ToLower(parsed.Hostname()))
		if host == "" {
			return "", "", fmt.Errorf("repository remote URL %q has no host", trimmed)
		}
		if parsed.Port() != "" {
			return "", "", fmt.Errorf("repository remote URL %q uses unsupported non-default SSH port", trimmed)
		}
		path := strings.TrimPrefix(parsed.Path, "/")
		if path == "" {
			return "", "", fmt.Errorf("repository remote URL %q has no path", trimmed)
		}
		return "https://" + host + "/" + path, host, nil
	}

	if at := strings.Index(trimmed, "@"); at >= 0 {
		hostAndPath := trimmed[at+1:]
		host, path, ok := strings.Cut(hostAndPath, ":")
		host = strings.TrimSpace(strings.ToLower(host))
		path = strings.TrimPrefix(strings.TrimSpace(path), "/")
		if ok && host != "" && path != "" {
			return "https://" + host + "/" + path, host, nil
		}
	}

	return "", "", fmt.Errorf("repository remote URL %q must use https or a canonicalizable ssh form", trimmed)
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	return strings.TrimSpace(string(out)), nil
}

func createTopLevelSandbox(
	client *controlclient.Client,
	loader policyLoader,
	cwd, host, backendName, imageRefOverride string,
	launchSeconds int64,
	repository *resolvedRepositoryCheckout,
) (string, *cleanroomv1.Sandbox, error) {
	compiled, _, err := loader.LoadAndCompile(cwd)
	if err != nil {
		return "", nil, err
	}
	allowLocalImageOverride, err := isLocalControlPlaneEndpoint(host)
	if err != nil {
		return "", nil, err
	}
	compiled, err = overrideCompiledPolicyImage(compiled, imageRefOverride, allowLocalImageOverride)
	if err != nil {
		return "", nil, err
	}

	createSandboxResp, err := client.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Backend: backendName,
		Options: &cleanroomv1.SandboxOptions{
			LaunchSeconds: launchSeconds,
		},
		Policy: compiled.ToProto(),
	})
	if err != nil {
		return "", nil, fmt.Errorf("create sandbox: %w", err)
	}

	sandbox := createSandboxResp.GetSandbox()
	sandboxID := strings.TrimSpace(sandbox.GetSandboxId())
	if sandboxID == "" {
		return "", nil, errors.New("create sandbox: response missing sandbox id")
	}

	if repository != nil {
		if err := bootstrapRepositorySandbox(client, sandboxID, launchSeconds, repository); err != nil {
			terminateSandboxBestEffort(client, sandboxID, 0, nil, "")
			return "", nil, err
		}
	}

	return sandboxID, sandbox, nil
}

func bootstrapRepositorySandbox(client *controlclient.Client, sandboxID string, launchSeconds int64, repository *resolvedRepositoryCheckout) error {
	if client == nil || strings.TrimSpace(sandboxID) == "" || repository == nil {
		return nil
	}

	command := buildRepositoryBootstrapCommand(repository)
	createExecutionResp, err := client.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   command,
		Kind:      cleanroomv1.ExecutionKind_EXECUTION_KIND_BATCH,
		Options: &cleanroomv1.ExecutionOptions{
			LaunchSeconds: launchSeconds,
		},
	})
	if err != nil {
		return fmt.Errorf("bootstrap repository checkout: create execution: %w", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()

	stream, err := client.StreamExecution(context.Background(), &cleanroomv1.StreamExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Follow:      true,
	})
	if err != nil {
		return fmt.Errorf("bootstrap repository checkout: stream execution: %w", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := 0
	haveExitCode := false
	for stream.Receive() {
		event := stream.Msg()
		switch payload := event.Payload.(type) {
		case *cleanroomv1.ExecutionStreamEvent_Stdout:
			_, _ = stdout.Write(payload.Stdout)
		case *cleanroomv1.ExecutionStreamEvent_Stderr:
			_, _ = stderr.Write(payload.Stderr)
		case *cleanroomv1.ExecutionStreamEvent_Exit:
			exitCode = int(payload.Exit.GetExitCode())
			haveExitCode = true
		}
	}
	if err := stream.Err(); err != nil && !isCanceledStreamErr(err) {
		return fmt.Errorf("bootstrap repository checkout: stream execution: %w", err)
	}
	if !haveExitCode {
		if fetchedExitCode, ok := getFinalExecutionExitCode(client, sandboxID, executionID); ok {
			exitCode = fetchedExitCode
			haveExitCode = true
		}
	}
	if !haveExitCode {
		return errors.New("bootstrap repository checkout: execution ended without exit status")
	}
	if exitCode != 0 {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			msg = fmt.Sprintf("bootstrap command failed with exit code %d", exitCode)
		}
		return fmt.Errorf("bootstrap repository checkout: %s", msg)
	}
	return nil
}

func buildRepositoryBootstrapCommand(repository *resolvedRepositoryCheckout) []string {
	if repository == nil {
		return nil
	}
	return []string{"sh", "-lc", strings.Join(repositoryBootstrapScript(repository), "\n")}
}

func wrapCommandWithRepositoryBootstrap(command []string, repository *resolvedRepositoryCheckout) []string {
	normalized := normalizePassthroughCommand(command)
	if repository == nil || len(normalized) == 0 {
		return normalized
	}
	script := repositoryBootstrapScript(repository)
	script = append(script, fmt.Sprintf("cd %s && exec %s", shellQuote(repository.DestinationDir), shellJoin(normalized)))
	return []string{"sh", "-lc", strings.Join(script, "\n")}
}

func repositoryBootstrapScript(repository *resolvedRepositoryCheckout) []string {
	if repository == nil {
		return nil
	}
	cloneCommand := `git clone --filter=blob:none --no-checkout "$remote" "$dest"`
	checkoutCommand := `git -C "$dest" checkout --detach "$commit"`
	submoduleCommand := `git -C "$dest" submodule update --init --recursive`
	if repository.GitConfigKey != "" && repository.GitConfigValue != "" {
		gitConfigArg := shellQuote(repository.GitConfigKey + "=" + repository.GitConfigValue)
		cloneCommand = "git -c " + gitConfigArg + ` clone --filter=blob:none --no-checkout "$remote" "$dest"`
		checkoutCommand = "git -C \"$dest\" -c " + gitConfigArg + ` checkout --detach "$commit"`
		submoduleCommand = "git -C \"$dest\" -c " + gitConfigArg + ` submodule update --init --recursive`
	}

	script := []string{
		"set -eu",
		"dest=" + shellQuote(repository.DestinationDir),
		"remote=" + shellQuote(repository.RemoteURL),
		"commit=" + shellQuote(repository.CommitSHA),
		`if [ -e "$dest" ]; then echo "repository destination already exists: $dest" >&2; exit 1; fi`,
		`mkdir -p "$(dirname "$dest")"`,
		cloneCommand,
		checkoutCommand,
		`got="$(git -C "$dest" rev-parse HEAD)"`,
		`if [ "$got" != "$commit" ]; then echo "repository checkout mismatch: expected $commit got $got" >&2; exit 1; fi`,
	}
	if repository.Submodules {
		script = append(script, submoduleCommand)
	}
	return script
}

func wrapCommandInRepositoryWorkdir(command []string, repository *resolvedRepositoryCheckout) []string {
	normalized := normalizePassthroughCommand(command)
	if repository == nil || len(normalized) == 0 {
		return normalized
	}
	script := fmt.Sprintf("cd %s && exec %s", shellQuote(repository.DestinationDir), shellJoin(normalized))
	return []string{"sh", "-lc", script}
}

func shellJoin(args []string) string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		out = append(out, shellQuote(arg))
	}
	return strings.Join(out, " ")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func normalizePassthroughCommand(command []string) []string {
	if len(command) > 0 && command[0] == "--" {
		command = command[1:]
	}
	return append([]string(nil), command...)
}

func warnDirtyRepositoryCheckout(repository *resolvedRepositoryCheckout) {
	if repository == nil || !repository.Dirty {
		return
	}
	_, _ = fmt.Fprintf(
		os.Stderr,
		"warning: repository has local modifications; sandbox will use HEAD %s and ignore local changes\n",
		repository.CommitSHA,
	)
}

func resolveGitCredentialConfig(repoRoot, remoteURL, remoteHost string) (string, string, error) {
	if gitCredentialFill == nil {
		return "", "", nil
	}
	parsed, err := url.Parse(remoteURL)
	if err != nil {
		return "", "", fmt.Errorf("parse repository remote URL %q: %w", remoteURL, err)
	}

	var lookup strings.Builder
	lookup.WriteString("protocol=https\n")
	lookup.WriteString("host=")
	lookup.WriteString(remoteHost)
	lookup.WriteString("\n")
	path := strings.TrimPrefix(strings.TrimSpace(parsed.Path), "/")
	if path != "" {
		lookup.WriteString("path=")
		lookup.WriteString(path)
		lookup.WriteString("\n")
	}
	lookup.WriteString("\n")

	output, err := gitCredentialFill(repoRoot, lookup.String())
	if err != nil {
		return "", "", nil
	}

	username, password := parseGitCredentialFillOutput(output)
	if username == "" || password == "" {
		return "", "", nil
	}
	auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	return "http.https://" + remoteHost + "/.extraHeader", "Authorization: Basic " + auth, nil
}

func gitCredentialFillFromHost(dir, input string) (string, error) {
	cmd := exec.Command("git", "credential", "fill")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func parseGitCredentialFillOutput(raw string) (string, string) {
	var username string
	var password string

	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "username":
			username = strings.TrimSpace(value)
		case "password":
			password = strings.TrimSpace(value)
		}
	}
	return username, password
}
