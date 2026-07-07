package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/buildkite/cleanroom/internal/bake"
	"github.com/buildkite/cleanroom/internal/mediation"
	"github.com/buildkite/cleanroom/internal/policy"
)

const contentCacheServiceName = "content-cache"

type RunCommand struct {
	SporeDir string   `arg:"" name:"spore-dir" help:"Baked spore directory to run"`
	Dir      string   `required:"" help:"Repository whose current policy and commit must match the spore bake key"`
	Grants   string   `help:"Gateway grants config (default: ~/.config/cleanroom/gateway.yaml)"`
	Spore    string   `help:"spore executable" default:"spore"`
	Argv     []string `arg:"" optional:"" passthrough:"" name:"argv" help:"Command argv to run inside the spore"`
}

func (c *RunCommand) Run(ctx *runtimeContext) error {
	argv := cleanPassthroughArgv(c.Argv)
	if len(argv) == 0 {
		return errors.New("cleanroom run requires a command after --")
	}

	runner := &bake.CLIRunner{Spore: c.Spore, Stdout: ctx.stderr(), Stderr: ctx.stderr()}
	annotations, err := runner.InspectAnnotations(c.SporeDir)
	if err != nil {
		return err
	}
	prov, err := bake.ParseProvenance(annotations)
	if err != nil {
		return err
	}
	compiled, _, err := c.audit(ctx, prov)
	if err != nil {
		return err
	}
	argv = wrapArgvWithEnv(argv, contentCacheEnv(prov, argv, os.LookupEnv))

	if len(prov.GatewayServices) == 0 {
		return c.runSpore(ctx, sporeRunArgs(c.SporeDir, prov, "", argv))
	}

	tempDir, err := os.MkdirTemp("", "cleanroom-run-*")
	if err != nil {
		return fmt.Errorf("create gateway temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)
	socketPath := filepath.Join(tempDir, "gateway.sock")
	grantsPath := c.Grants

	cache, cacheDone, err := c.startContentCache(ctx, prov)
	if err != nil {
		return err
	}
	defer stopGateway(cache, cacheDone)

	if grantsPath == "" && onlyMediationService(prov, contentCacheServiceName) {
		grantsPath, err = writeContentCacheGatewayConfig(tempDir, compiled.Hash)
		if err != nil {
			return err
		}
	}

	gateway, done, err := c.startGateway(ctx, socketPath, grantsPath)
	if err != nil {
		return err
	}
	if err := waitForSocket(socketPath, done); err != nil {
		if gateway.Process != nil {
			_ = gateway.Process.Kill()
		}
		return err
	}
	defer stopGateway(gateway, done)

	return c.runSpore(ctx, sporeRunArgs(c.SporeDir, prov, socketPath, argv))
}

func (c *RunCommand) audit(ctx *runtimeContext, prov bake.Provenance) (*policy.CompiledPolicy, bake.GitFacts, error) {
	cwd, err := resolveCWD(ctx.CWD, c.Dir)
	if err != nil {
		return nil, bake.GitFacts{}, err
	}
	compiled, _, err := ctx.Loader.LoadAndCompile(cwd)
	if err != nil {
		return nil, bake.GitFacts{}, err
	}
	sporeDir, err := resolveCWD(ctx.CWD, c.SporeDir)
	if err != nil {
		return nil, bake.GitFacts{}, err
	}
	facts := bake.CollectGitFactsExcluding(cwd, bake.ArtifactExclusions(cwd, sporeDir))
	if err := bake.AuditKey(prov, compiled, facts); err != nil {
		return nil, bake.GitFacts{}, err
	}
	return compiled, facts, nil
}

func (c *RunCommand) startGateway(ctx *runtimeContext, socketPath, grantsPath string) (*exec.Cmd, <-chan error, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, nil, fmt.Errorf("find cleanroom executable: %w", err)
	}
	args := []string{"gateway", "serve", "--for", c.SporeDir, "--dir", c.Dir, "--socket", socketPath, "--spore", c.Spore}
	if grantsPath != "" {
		args = append(args, "--grants", grantsPath)
	}
	cmd := exec.Command(exe, args...)
	cmd.Dir = ctx.CWD
	cmd.Stdout = ctx.stderr()
	cmd.Stderr = ctx.stderr()
	cmd.Stdin = ctx.stdin()
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start cleanroom gateway: %w", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	return cmd, done, nil
}

func (c *RunCommand) startContentCache(ctx *runtimeContext, prov bake.Provenance) (*exec.Cmd, <-chan error, error) {
	if c.Grants != "" || !onlyMediationService(prov, contentCacheServiceName) || contentCacheHealthy() {
		return nil, nil, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, nil, fmt.Errorf("find cleanroom executable: %w", err)
	}
	cmd := exec.Command(exe, "content-cache", "serve", "--listen", defaultContentCacheListen)
	cmd.Dir = ctx.CWD
	cmd.Stdout = ctx.stderr()
	cmd.Stderr = ctx.stderr()
	cmd.Stdin = ctx.stdin()
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start cleanroom content-cache: %w", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	if err := waitForContentCache(done); err != nil {
		_ = cmd.Process.Kill()
		return nil, nil, err
	}
	return cmd, done, nil
}

func (c *RunCommand) runSpore(ctx *runtimeContext, args []string) error {
	cmd := exec.Command(c.Spore, args...)
	cmd.Dir = ctx.CWD
	cmd.Stdin = ctx.stdin()
	cmd.Stdout = ctx.Stdout
	cmd.Stderr = ctx.stderr()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("spore run: %w", err)
	}
	return nil
}

func sporeRunArgs(sporeDir string, prov bake.Provenance, socketPath string, argv []string) []string {
	args := []string{"run", "--from", sporeDir}
	if socketPath != "" {
		for _, service := range prov.GatewayServices {
			args = append(args, "--bind-service", fmt.Sprintf("%s=unix:%s", service.Name, socketPath))
		}
	}
	args = append(args, "--")
	args = append(args, argv...)
	return args
}

func cleanPassthroughArgv(argv []string) []string {
	if len(argv) > 0 && argv[0] == "--" {
		return argv[1:]
	}
	return argv
}

func wrapArgvWithEnv(argv, env []string) []string {
	if len(env) == 0 {
		return argv
	}
	wrapped := append([]string{"/usr/bin/env"}, env...)
	return append(wrapped, argv...)
}

func contentCacheEnv(prov bake.Provenance, argv []string, lookupEnv func(string) (string, bool)) []string {
	if !hasString(prov.MediationServices, contentCacheServiceName) {
		return nil
	}
	hosts := allowedHTTPSHosts(prov.NetworkRules)
	if len(hosts) == 0 {
		return nil
	}

	base := fmt.Sprintf("http://%s:%d/services/%s", mediation.GuestHostname, mediation.GuestPort, contentCacheServiceName)
	var env []string
	if !explicitEnv(argv, lookupEnv, "GIT_CONFIG_COUNT") {
		env = append(env, gitContentCacheEnv(base, hosts)...)
	}
	if hasString(hosts, "proxy.golang.org") && !explicitEnv(argv, lookupEnv, "GOPROXY") {
		env = append(env, "GOPROXY="+base+"/goproxy,direct")
	}
	if hasString(hosts, "dl.google.com") && !explicitEnv(argv, lookupEnv, "MISE_GO_DOWNLOAD_MIRROR") {
		env = append(env, "MISE_GO_DOWNLOAD_MIRROR="+base+"/fetch/dl.google.com/go")
	}
	return env
}

func gitContentCacheEnv(base string, hosts []string) []string {
	env := []string{fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(hosts))}
	for i, host := range hosts {
		cacheURL := base + "/git/" + url.PathEscape(host) + "/"
		env = append(env,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=url.%s.insteadOf", i, cacheURL),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=https://%s/", i, host),
		)
	}
	return env
}

func allowedHTTPSHosts(rules []bake.NetworkRule) []string {
	seen := map[string]bool{}
	for _, rule := range rules {
		for _, port := range rule.Ports {
			if port == 443 {
				seen[rule.Host] = true
			}
		}
	}
	hosts := make([]string, 0, len(seen))
	for host := range seen {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	return hosts
}

func explicitEnv(argv []string, lookupEnv func(string) (string, bool), key string) bool {
	if _, ok := lookupEnv(key); ok {
		return true
	}
	if len(argv) == 0 || (argv[0] != "env" && argv[0] != "/usr/bin/env") {
		return false
	}
	for _, arg := range argv[1:] {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		name, _, ok := strings.Cut(arg, "=")
		if !ok {
			return false
		}
		if name == key {
			return true
		}
	}
	return false
}

func hasString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func onlyMediationService(prov bake.Provenance, name string) bool {
	return len(prov.MediationServices) == 1 && prov.MediationServices[0] == name
}

func writeContentCacheGatewayConfig(dir, policyHash string) (string, error) {
	path := filepath.Join(dir, "gateway.yaml")
	config := fmt.Sprintf(`services:
  content-cache:
    upstream: http://%s
grants:
  - match: { policy_hash: "%s" }
    services: [content-cache]
`, defaultContentCacheListen, policyHash)
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		return "", fmt.Errorf("write content-cache gateway config: %w", err)
	}
	return path, nil
}

func contentCacheHealthy() bool {
	client := http.Client{Timeout: 200 * time.Millisecond}
	resp, err := client.Get("http://" + defaultContentCacheListen + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func waitForContentCache(done <-chan error) error {
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if contentCacheHealthy() {
			return nil
		}
		select {
		case err := <-done:
			if err != nil {
				return fmt.Errorf("content-cache exited before health check passed: %w", err)
			}
			return errors.New("content-cache exited before health check passed")
		case <-deadline:
			return fmt.Errorf("content-cache was not healthy at http://%s/health after 5s", defaultContentCacheListen)
		case <-ticker.C:
		}
	}
}

func waitForSocket(socketPath string, done <-chan error) error {
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if info, err := os.Stat(socketPath); err == nil && info.Mode()&os.ModeSocket != 0 {
			return nil
		}
		select {
		case err := <-done:
			if err != nil {
				return fmt.Errorf("gateway exited before socket was ready: %w", err)
			}
			return errors.New("gateway exited before socket was ready")
		case <-deadline:
			return fmt.Errorf("gateway socket %s was not ready after 5s", socketPath)
		case <-ticker.C:
		}
	}
}

func stopGateway(cmd *exec.Cmd, done <-chan error) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	select {
	case <-done:
	case <-time.After(time.Second):
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}
}
