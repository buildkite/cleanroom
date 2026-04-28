package repositorycheckout

import (
	"crypto/sha256"
	"encoding/hex"
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
	Branch         string
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
		Branch:         strings.TrimSpace(proto.GetBranch()),
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
		Branch:         c.Branch,
	}
}

func (c *Checkout) ValidateBootstrap() error {
	if c == nil {
		return nil
	}
	normalizedCommitSHA := strings.ToLower(strings.TrimSpace(c.CommitSHA))
	if normalizedCommitSHA == "" {
		return fmt.Errorf("repository commit_sha is required")
	}
	if len(normalizedCommitSHA) != 40 {
		return fmt.Errorf("repository commit_sha %q must be a full 40-character hexadecimal commit SHA", strings.TrimSpace(c.CommitSHA))
	}
	if _, err := hex.DecodeString(normalizedCommitSHA); err != nil {
		return fmt.Errorf("repository commit_sha %q must be a full 40-character hexadecimal commit SHA", strings.TrimSpace(c.CommitSHA))
	}
	c.CommitSHA = normalizedCommitSHA
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
	canonicalURL, host, err := CanonicalizeRemoteURL(trimmed)
	if err != nil {
		return "", err
	}
	c.RemoteURL = canonicalURL
	return host, nil
}

func CanonicalizeRemoteURL(raw string) (string, string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", fmt.Errorf("repository remote URL is empty")
	}

	if parsed, ok, err := parseCanonicalHTTPSRemoteURL(trimmed); ok {
		if err != nil {
			return "", "", err
		}
		host := strings.TrimSpace(strings.ToLower(parsed.Hostname()))
		parsed.User = nil
		parsed.Host = normalizedURLHost(host, parsed.Port())
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return parsed.String(), host, nil
	}

	if strings.HasPrefix(strings.ToLower(trimmed), "ssh://") {
		parsed, err := url.Parse(trimmed)
		if err != nil {
			return "", "", fmt.Errorf("parse repository remote URL %q: %w", trimmed, err)
		}
		host := strings.TrimSpace(strings.ToLower(parsed.Hostname()))
		if host == "" {
			return "", "", fmt.Errorf("repository remote URL %q has no host", trimmed)
		}
		if parsed.Port() != "" && parsed.Port() != "22" {
			return "", "", fmt.Errorf("repository remote URL %q uses unsupported non-default SSH port", trimmed)
		}
		path := strings.TrimPrefix(parsed.Path, "/")
		if path == "" {
			return "", "", fmt.Errorf("repository remote URL %q has no path", trimmed)
		}
		return "https://" + normalizedURLHost(host, "") + "/" + path, host, nil
	}

	if at := strings.Index(trimmed, "@"); at >= 0 {
		hostAndPath := trimmed[at+1:]
		host, path, ok := strings.Cut(hostAndPath, ":")
		host = strings.TrimSpace(strings.ToLower(host))
		path = strings.TrimPrefix(strings.TrimSpace(path), "/")
		if ok && host != "" && path != "" {
			return "https://" + normalizedURLHost(host, "") + "/" + path, host, nil
		}
	}

	return "", "", fmt.Errorf("repository remote URL %q must use https or a canonicalizable ssh form", trimmed)
}

func parseCanonicalHTTPSRemoteURL(raw string) (*url.URL, bool, error) {
	trimmed := strings.TrimSpace(raw)
	explicitHTTPS := strings.HasPrefix(strings.ToLower(trimmed), "https://")
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, explicitHTTPS, fmt.Errorf("parse repository remote URL %q: %w", trimmed, err)
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return nil, false, nil
	}
	host := strings.TrimSpace(strings.ToLower(parsed.Hostname()))
	if host == "" {
		return nil, true, fmt.Errorf("repository remote URL %q has no host", trimmed)
	}
	if parsed.Port() != "" && parsed.Port() != "443" {
		return nil, true, fmt.Errorf("repository remote URL %q uses unsupported non-default HTTPS port", trimmed)
	}
	if strings.TrimSpace(parsed.Path) == "" || parsed.Path == "/" {
		return nil, true, fmt.Errorf("repository remote URL %q has no path", trimmed)
	}
	return parsed, true, nil
}

func normalizedURLHost(host, port string) string {
	if strings.Contains(host, ":") {
		if port != "" {
			return "[" + host + "]:" + port
		}
		return "[" + host + "]"
	}
	if port != "" {
		return host + ":" + port
	}
	return host
}

func BuildBootstrapCommand(checkout *Checkout) []string {
	if checkout == nil {
		return nil
	}
	return []string{"sh", "-lc", strings.Join(bootstrapScript(checkout), "\n")}
}

func BuildBootstrapCommandWithBundle(checkout *Checkout, bundleRef string) []string {
	if checkout == nil {
		return nil
	}
	return []string{"sh", "-lc", strings.Join(bootstrapScriptWithBundle(checkout, bundleRef), "\n")}
}

func BuildRefreshCommand(checkout *Checkout) []string {
	if checkout == nil {
		return nil
	}
	return []string{"sh", "-lc", strings.Join(refreshScript(checkout), "\n")}
}

func BuildRefreshCommandWithBundle(checkout *Checkout, bundleRef string) []string {
	if checkout == nil {
		return nil
	}
	return []string{"sh", "-lc", strings.Join(refreshScriptWithBundle(checkout, bundleRef), "\n")}
}

func BundleRefName(commitSHA string) string {
	commitSHA = strings.ToLower(strings.TrimSpace(commitSHA))
	if commitSHA == "" {
		return "refs/remotes/cleanroom/local"
	}
	return "refs/remotes/cleanroom/" + commitSHA
}

func BootstrapRecipeDigest(checkout *Checkout) string {
	if checkout == nil {
		return ""
	}
	return commandRecipeDigest(BuildBootstrapCommand(checkout))
}

func RefreshRecipeDigest(checkout *Checkout) string {
	if checkout == nil {
		return ""
	}
	normalized := *checkout
	normalized.CommitSHA = ""
	return commandRecipeDigest(BuildRefreshCommand(&normalized))
}

func WorkdirRecipeDigest(command []string, checkout *Checkout) string {
	if checkout == nil {
		return ""
	}
	return commandRecipeDigest(WrapCommandInWorkdir(command, checkout))
}

func WrapCommandWithBootstrap(command []string, checkout *Checkout) []string {
	normalized := NormalizeCommand(command)
	if checkout == nil || len(normalized) == 0 {
		return normalized
	}
	script := bootstrapScript(checkout)
	script = append(script, workdirExecutionScript(normalized, checkout)...)
	return []string{"sh", "-lc", strings.Join(script, "\n")}
}

func WrapCommandInWorkdir(command []string, checkout *Checkout) []string {
	normalized := NormalizeCommand(command)
	if checkout == nil || len(normalized) == 0 {
		return normalized
	}
	return []string{"sh", "-lc", strings.Join(workdirExecutionScript(normalized, checkout), "\n")}
}

func NormalizeCommand(command []string) []string {
	if len(command) > 0 && command[0] == "--" {
		command = command[1:]
	}
	return append([]string(nil), command...)
}

func bootstrapScript(checkout *Checkout) []string {
	return bootstrapScriptWithBundle(checkout, "")
}

func bootstrapScriptWithBundle(checkout *Checkout, bundleRef string) []string {
	cloneCommand := `git clone --filter=blob:none --no-checkout --progress "$remote" "$dest"`

	script := []string{
		"set -eu",
		"dest=" + shellQuote(checkout.DestinationDir),
		"remote=" + shellQuote(checkout.RemoteURL),
		"commit=" + shellQuote(checkout.CommitSHA),
		"branch=" + shellQuote(checkout.Branch),
		`if [ -e "$dest" ] && [ ! -d "$dest" ]; then echo "repository destination is not a directory: $dest" >&2; exit 1; fi`,
		`if [ -d "$dest" ] && [ -n "$(ls -A "$dest")" ]; then echo "repository destination already exists and is not empty: $dest" >&2; exit 1; fi`,
		`mkdir -p "$dest"`,
	}
	if strings.TrimSpace(bundleRef) != "" {
		script = append(script, bundleInputScript()...)
	}
	script = append(script, cloneCommand)
	if strings.TrimSpace(bundleRef) != "" {
		script = append(script, bundleFetchScript(bundleRef)...)
	}
	return append(script, checkoutVerificationScript(checkout)...)
}

func refreshScript(checkout *Checkout) []string {
	return refreshScriptWithBundle(checkout, "")
}

func refreshScriptWithBundle(checkout *Checkout, bundleRef string) []string {
	script := []string{
		"set -eu",
		"dest=" + shellQuote(checkout.DestinationDir),
		"remote=" + shellQuote(checkout.RemoteURL),
		"commit=" + shellQuote(checkout.CommitSHA),
		"branch=" + shellQuote(checkout.Branch),
		`if [ ! -d "$dest" ]; then echo "repository destination does not exist: $dest" >&2; exit 1; fi`,
		`if ! git -C "$dest" rev-parse --is-inside-work-tree >/dev/null 2>&1; then echo "repository destination is not a git checkout: $dest" >&2; exit 1; fi`,
		`if git -C "$dest" remote get-url origin >/dev/null 2>&1; then git -C "$dest" remote set-url origin "$remote"; else git -C "$dest" remote add origin "$remote"; fi`,
		`git -C "$dest" submodule deinit -f --all >/dev/null 2>&1 || true`,
		`git -C "$dest" reset --hard`,
		`git -C "$dest" clean -ffdx`,
	}
	if strings.TrimSpace(bundleRef) != "" {
		script = append(script, bundleInputScript()...)
		script = append(script,
			`git -C "$dest" fetch --filter=blob:none --progress origin`,
		)
		script = append(script, bundleFetchScript(bundleRef)...)
	} else {
		script = append(script, `git -C "$dest" fetch --filter=blob:none --progress origin "$commit"`)
	}
	return append(script, checkoutVerificationScript(checkout)...)
}

func bundleInputScript() []string {
	return []string{
		`bundle_file="$(mktemp)"`,
		`cleanup_bundle() { rm -f "$bundle_file"; }`,
		`trap cleanup_bundle EXIT INT TERM`,
		`cat >"$bundle_file"`,
	}
}

func bundleFetchScript(bundleRef string) []string {
	return []string{
		"bundle_ref=" + shellQuote(bundleRef),
		`git -C "$dest" fetch --progress "$bundle_file" "+HEAD:$bundle_ref"`,
	}
}

func checkoutVerificationScript(checkout *Checkout) []string {
	checkoutCommand := `git -C "$dest" checkout --detach "$commit"`
	if strings.TrimSpace(checkout.Branch) != "" {
		checkoutCommand = `git -C "$dest" checkout -B "$branch" "$commit"`
	}

	script := []string{
		checkoutCommand,
		`git -C "$dest" reset --hard "$commit"`,
		`git -C "$dest" clean -ffdx`,
		`got="$(git -C "$dest" rev-parse HEAD)"`,
		`if [ "$got" != "$commit" ]; then echo "repository checkout mismatch: expected $commit got $got" >&2; exit 1; fi`,
	}
	if checkout.Submodules {
		script = append(script,
			`git -C "$dest" submodule sync --recursive`,
			`git -C "$dest" submodule update --init --recursive --force`,
		)
	}
	return script
}

func workdirExecutionScript(command []string, checkout *Checkout) []string {
	execCommand := shellJoin(command)
	script := []string{
		"set -eu",
		"dest=" + shellQuote(checkout.DestinationDir),
		`cd "$dest"`,
	}
	script = append(script, `exec `+execCommand)
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

func commandRecipeDigest(command []string) string {
	sum := sha256.New()
	for _, part := range command {
		_, _ = sum.Write([]byte(part))
		_, _ = sum.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(sum.Sum(nil))
}
