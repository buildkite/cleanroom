package repositorycheckout

import (
	"fmt"
	"net/url"
	"strings"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

type Checkout struct {
	RemoteURL      string
	CommitSHA      string
	DestinationDir string
	Submodules     bool
}

func FromProto(proto *cleanroomv1.RepositoryCheckout) *Checkout {
	if proto == nil {
		return nil
	}
	return &Checkout{
		RemoteURL:      strings.TrimSpace(proto.GetRemoteUrl()),
		CommitSHA:      strings.TrimSpace(proto.GetCommitSha()),
		DestinationDir: strings.TrimSpace(proto.GetDestinationDir()),
		Submodules:     proto.GetSubmodules(),
	}
}

func (c *Checkout) ToProto() *cleanroomv1.RepositoryCheckout {
	if c == nil {
		return nil
	}
	return &cleanroomv1.RepositoryCheckout{
		RemoteUrl:      c.RemoteURL,
		CommitSha:      c.CommitSHA,
		DestinationDir: c.DestinationDir,
		Submodules:     c.Submodules,
	}
}

func (c *Checkout) ValidateBootstrap() error {
	if c == nil {
		return nil
	}
	if strings.TrimSpace(c.CommitSHA) == "" {
		return fmt.Errorf("repository commit_sha is required")
	}
	if _, err := c.NormalizeRemoteURL(); err != nil {
		return err
	}
	return c.validateDestination()
}

func (c *Checkout) ValidateWorkdir() error {
	if c == nil {
		return nil
	}
	return c.validateDestination()
}

func (c *Checkout) validateDestination() error {
	if strings.TrimSpace(c.DestinationDir) == "" {
		return fmt.Errorf("repository destination_dir is required")
	}
	if !strings.HasPrefix(c.DestinationDir, "/") {
		return fmt.Errorf("repository destination_dir %q must be absolute", c.DestinationDir)
	}
	return nil
}

func (c *Checkout) NormalizeRemoteURL() (string, error) {
	if c == nil {
		return "", nil
	}
	trimmed := strings.TrimSpace(c.RemoteURL)
	if trimmed == "" {
		return "", fmt.Errorf("repository remote_url is required")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("parse repository remote_url %q: %w", trimmed, err)
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return "", fmt.Errorf("repository remote_url %q must use https", trimmed)
	}
	if parsed.User != nil {
		return "", fmt.Errorf("repository remote_url %q must not include userinfo", trimmed)
	}
	host := strings.TrimSpace(strings.ToLower(parsed.Hostname()))
	if host == "" {
		return "", fmt.Errorf("repository remote_url %q has no host", trimmed)
	}
	if parsed.Port() != "" && parsed.Port() != "443" {
		return "", fmt.Errorf("repository remote_url %q uses unsupported non-default HTTPS port", trimmed)
	}
	if strings.TrimSpace(parsed.Path) == "" || parsed.Path == "/" {
		return "", fmt.Errorf("repository remote_url %q has no path", trimmed)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("repository remote_url %q must not include query or fragment", trimmed)
	}
	parsed.Host = host
	parsed.RawQuery = ""
	parsed.Fragment = ""
	c.RemoteURL = parsed.String()
	return host, nil
}

func BuildBootstrapCommand(checkout *Checkout) []string {
	if checkout == nil {
		return nil
	}
	return []string{"sh", "-lc", strings.Join(bootstrapScript(checkout), "\n")}
}

func WrapCommandWithBootstrap(command []string, checkout *Checkout) []string {
	normalized := NormalizeCommand(command)
	if checkout == nil || len(normalized) == 0 {
		return normalized
	}
	script := bootstrapScript(checkout)
	script = append(script, fmt.Sprintf("cd %s && exec %s", shellQuote(checkout.DestinationDir), shellJoin(normalized)))
	return []string{"sh", "-lc", strings.Join(script, "\n")}
}

func WrapCommandInWorkdir(command []string, checkout *Checkout) []string {
	normalized := NormalizeCommand(command)
	if checkout == nil || len(normalized) == 0 {
		return normalized
	}
	script := fmt.Sprintf("cd %s && exec %s", shellQuote(checkout.DestinationDir), shellJoin(normalized))
	return []string{"sh", "-lc", script}
}

func NormalizeCommand(command []string) []string {
	if len(command) > 0 && command[0] == "--" {
		command = command[1:]
	}
	return append([]string(nil), command...)
}

func bootstrapScript(checkout *Checkout) []string {
	cloneCommand := `git clone --filter=blob:none --no-checkout "$remote" "$dest"`
	checkoutCommand := `git -C "$dest" checkout --detach "$commit"`
	submoduleCommand := `git -C "$dest" submodule update --init --recursive`

	script := []string{
		"set -eu",
		"dest=" + shellQuote(checkout.DestinationDir),
		"remote=" + shellQuote(checkout.RemoteURL),
		"commit=" + shellQuote(checkout.CommitSHA),
		`if [ -e "$dest" ]; then echo "repository destination already exists: $dest" >&2; exit 1; fi`,
		`mkdir -p "$(dirname "$dest")"`,
		cloneCommand,
		checkoutCommand,
		`got="$(git -C "$dest" rev-parse HEAD)"`,
		`if [ "$got" != "$commit" ]; then echo "repository checkout mismatch: expected $commit got $got" >&2; exit 1; fi`,
	}
	if checkout.Submodules {
		script = append(script, submoduleCommand)
	}
	return script
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
