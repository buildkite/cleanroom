package scripts_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const dockerPullImage = "ghcr.io/buildkite/cleanroom-base/alpine@sha256:91a63856cdf97b2e5659660b41d1a131d3b57bfa4cad254018e391ffef6fa4b9"

var multiHostExposurePattern = regexp.MustCompile(`(?m)^exposed: https://example\.cleanroom\.localhost:([0-9]+)\s*$`)

func TestCIExampleSmoke(t *testing.T) {
	if os.Getenv("CLEANROOM_CI_EXAMPLE_ENABLED") != "1" {
		t.Skip("set CLEANROOM_CI_EXAMPLE_ENABLED=1 to run the CI examples smoke suite")
	}

	h := newExampleSmokeHarness(t)
	h.writeSmokePolicies(t)

	for _, example := range []string{
		"basic",
		"docker",
		"docker-cache-output",
		"seeded-output-cache",
		"rails",
		"buildkite-agent",
		"multi-host-routing",
	} {
		example := example
		t.Run("policy_validate/"+example, func(t *testing.T) {
			h.requireCommand(t, "validate "+example+" example", filepath.Join(h.repoRoot, "examples", example), h.cleanroomPath, "policy", "validate")
		})
	}

	t.Run("exec/basic", func(t *testing.T) {
		result := h.requireCleanroom(t, "basic example smoke",
			"exec", "--host", h.listenEndpoint, "--backend", h.backend, "-c", h.basicPolicyDir,
			"--", "sh", "-lc", "echo basic-example-ok")
		requireOutputLine(t, result.stdout, "basic-example-ok")
	})

	t.Run("docker/version", func(t *testing.T) {
		result := h.requireCleanroom(t, "docker version smoke",
			"exec", "--host", h.listenEndpoint, "--backend", h.backend, "-c", h.dockerPolicyDir,
			"--", "sh", "-lc", "docker version >/dev/null && echo docker-version-ok")
		requireOutputLine(t, result.stdout, "docker-version-ok")
	})

	t.Run("docker/cache_output", func(t *testing.T) {
		result := h.requireCleanroom(t, "docker cache-output smoke",
			"exec", "--host", h.listenEndpoint, "--backend", h.backend, "-c", filepath.Join(h.repoRoot, "examples", "docker-cache-output"),
			"--", "sh", "-lc", `docker image inspect "$1" >/dev/null && echo docker-cached-ok`, "sh", dockerPullImage)
		requireOutputLine(t, result.stdout, "docker-cached-ok")
	})

	t.Run("cache_output/seeded", func(t *testing.T) {
		result := h.requireCleanroom(t, "seeded cache-output smoke",
			"exec", "--host", h.listenEndpoint, "--backend", h.backend, "-c", filepath.Join(h.repoRoot, "examples", "seeded-output-cache"),
			"--", "sh", "-lc", `test -f examples/seeded-output-cache/public/assets/.keep && grep -q "^generated$" examples/seeded-output-cache/public/assets/generated.txt && echo seeded-cache-output-ok`)
		requireOutputLine(t, result.stdout, "seeded-cache-output-ok")
	})

	t.Run("docker/pull", func(t *testing.T) {
		h.retry(t, "docker pull smoke", 3, func(attempt int) error {
			result := h.runCleanroom(t,
				"exec", "--host", h.listenEndpoint, "--backend", h.backend, "-c", h.dockerPolicyDir,
				"--", "sh", "-lc", `docker pull "$1" >/dev/null && echo docker-pull-ok`, "sh", dockerPullImage)
			if result.err != nil {
				return result.failure("docker pull smoke")
			}
			if !hasOutputLine(result.stdout, "docker-pull-ok") {
				return fmt.Errorf("expected docker pull output line missing; stdout:\n%s\nstderr:\n%s", result.stdout, result.stderr)
			}
			return nil
		})
	})

	t.Run("multi_host", func(t *testing.T) {
		multiHost := h.startMultiHost(t)
		port := multiHost.waitForExposure(t)
		h.requireExposureCert(t)

		t.Run("exact_route", func(t *testing.T) {
			out := h.curlOutput(t, "multi-host exact route", port, "example.cleanroom.localhost", "/")
			requireOutputLine(t, out, "exact route ok")
		})

		t.Run("app_route", func(t *testing.T) {
			out := h.curlOutput(t, "multi-host app route", port, "example-app.cleanroom.localhost", "/")
			for _, needle := range []string{
				`"host": "example-app.cleanroom.localhost:` + port + `"`,
				`"x_forwarded_host": "example-app.cleanroom.localhost:` + port + `"`,
				`"x_forwarded_proto": "https"`,
				`"x_forwarded_port": "` + port + `"`,
				`"x_forwarded_for": "127.0.0.1"`,
			} {
				if !strings.Contains(out, needle) {
					t.Fatalf("expected multi-host app response to contain %q\nresponse:\n%s", needle, out)
				}
			}
		})

		t.Run("redirect_route", func(t *testing.T) {
			headers := h.curlHeaders(t, "multi-host redirect route", port, "example-s3.cleanroom.localhost", "/")
			normalized := strings.ReplaceAll(headers, "\r", "")
			if !regexp.MustCompile(`(?m)^HTTP/.* 302`).MatchString(normalized) {
				t.Fatalf("expected multi-host redirect status in headers:\n%s", headers)
			}
			location := "Location: https://example-app.cleanroom.localhost:" + port + "/from-s3?client=127.0.0.1"
			if !hasOutputLineFold(normalized, location) {
				t.Fatalf("expected multi-host redirect location %q in headers:\n%s", location, headers)
			}
		})

		t.Run("unregistered_route", func(t *testing.T) {
			out := h.curlStatus(t, "multi-host unregistered route", port, "example-missing.cleanroom.localhost", "/", "404")
			requireOutputLine(t, out, "404 page not found")
		})
	})

	t.Run("docker/run", func(t *testing.T) {
		if h.backend == "firecracker" {
			t.Skip("guest docker pull path is covered, but guest container start is not yet reliable on firecracker")
		}
		result := h.requireCleanroom(t, "docker run smoke",
			"exec", "--host", h.listenEndpoint, "--backend", h.backend, "-c", h.dockerPolicyDir,
			"--", "docker", "run", "--rm", "--network", "none", dockerPullImage, "echo", "docker-example-ok")
		requireOutputLine(t, result.stdout, "docker-example-ok")
	})
}

type exampleSmokeHarness struct {
	backend          string
	listenEndpoint   string
	repoRoot         string
	tmpDir           string
	cleanroomPath    string
	basicPolicyDir   string
	dockerPolicyDir  string
	exposureCertPath string
}

func newExampleSmokeHarness(t *testing.T) *exampleSmokeHarness {
	t.Helper()

	backend := requireEnv(t, "CLEANROOM_CI_EXAMPLE_BACKEND")
	listenEndpoint := requireEnv(t, "CLEANROOM_CI_EXAMPLE_LISTEN_ENDPOINT")
	repoRoot := requireEnv(t, "CLEANROOM_CI_EXAMPLE_REPO_ROOT")

	absRepoRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		t.Fatalf("abs repo root: %v", err)
	}

	tmpDir := t.TempDir()
	basicPolicyDir := filepath.Join(tmpDir, "basic")
	dockerPolicyDir := filepath.Join(tmpDir, "docker")

	if err := os.MkdirAll(basicPolicyDir, 0o755); err != nil {
		t.Fatalf("create basic policy dir: %v", err)
	}
	if err := os.MkdirAll(dockerPolicyDir, 0o755); err != nil {
		t.Fatalf("create docker policy dir: %v", err)
	}

	return &exampleSmokeHarness{
		backend:          backend,
		listenEndpoint:   listenEndpoint,
		repoRoot:         absRepoRoot,
		tmpDir:           tmpDir,
		cleanroomPath:    filepath.Join(absRepoRoot, "dist", "cleanroom"),
		basicPolicyDir:   basicPolicyDir,
		dockerPolicyDir:  dockerPolicyDir,
		exposureCertPath: filepath.Join(xdgConfigHome(t), "cleanroom", "tls", "exposure-cert.pem"),
	}
}

func (h *exampleSmokeHarness) writeSmokePolicies(t *testing.T) {
	t.Helper()

	writeFile(t, filepath.Join(h.basicPolicyDir, "cleanroom.yaml"), `version: 1
repository:
  enabled: false
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:91a63856cdf97b2e5659660b41d1a131d3b57bfa4cad254018e391ffef6fa4b9
  network:
    default: deny
    allow:
      - host: api.github.com
        ports: [443]
`)
	writeFile(t, filepath.Join(h.dockerPolicyDir, "cleanroom.yaml"), `version: 1
repository:
  enabled: false
sandbox:
  image:
    ref: ghcr.io/buildkite/cleanroom-base/alpine-docker@sha256:19c696770ae8f3f36e786bf25a0e08e5a5c18b9a7fe52bde7d988c3da500bf08
  docker:
    required: true
  network:
    default: deny
    allow:
      - host: ghcr.io
        ports: [443]
      - host: pkg-containers.githubusercontent.com
        ports: [443]
`)
}

func (h *exampleSmokeHarness) runCleanroom(t *testing.T, args ...string) commandResult {
	t.Helper()

	return h.runCommand(t, "cleanroom "+strings.Join(args, " "), h.repoRoot, h.cleanroomPath, args...)
}

func (h *exampleSmokeHarness) requireCleanroom(t *testing.T, label string, args ...string) commandResult {
	t.Helper()

	return h.requireCommand(t, label, h.repoRoot, h.cleanroomPath, args...)
}

func (h *exampleSmokeHarness) requireCommand(t *testing.T, label, dir, command string, args ...string) commandResult {
	t.Helper()

	result := h.runCommand(t, label, dir, command, args...)
	if result.err != nil {
		t.Fatal(result.failure(label))
	}
	return result
}

func (h *exampleSmokeHarness) runCommand(t *testing.T, label, dir, command string, args ...string) commandResult {
	t.Helper()
	t.Logf("running %s", label)

	cmd := exec.CommandContext(t.Context(), command, args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	return commandResult{
		stdout: stdout.String(),
		stderr: stderr.String(),
		err:    err,
	}
}

func (h *exampleSmokeHarness) startMultiHost(t *testing.T) *multiHostServer {
	t.Helper()

	stdoutPath := filepath.Join(h.tmpDir, "multi-host.stdout")
	stderrPath := filepath.Join(h.tmpDir, "multi-host.stderr")

	stdoutFile, err := os.Create(stdoutPath)
	if err != nil {
		t.Fatalf("create multi-host stdout log: %v", err)
	}
	stderrFile, err := os.Create(stderrPath)
	if err != nil {
		_ = stdoutFile.Close()
		t.Fatalf("create multi-host stderr log: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), h.cleanroomPath,
		"exec",
		"--host", h.listenEndpoint,
		"--backend", h.backend,
		"--no-stdin",
		"-c", filepath.Join(h.repoRoot, "examples", "multi-host-routing"),
		"--expose-https", "example:80",
		"--expose-https", "example-app:80",
		"--expose-https", "example-s3:80",
		"--", "sh", "-lc", "cd /workspace/examples/multi-host-routing && sh ./start.sh",
	)
	cmd.Dir = h.repoRoot
	cmd.Env = os.Environ()
	cmd.Stdout = stdoutFile
	cmd.Stderr = stderrFile

	if err := cmd.Start(); err != nil {
		_ = stdoutFile.Close()
		_ = stderrFile.Close()
		t.Fatalf("start multi-host example: %v", err)
	}

	server := &multiHostServer{
		cmd:        cmd,
		done:       make(chan error, 1),
		stdoutFile: stdoutFile,
		stderrFile: stderrFile,
		stdoutPath: stdoutPath,
		stderrPath: stderrPath,
	}
	go func() {
		server.done <- cmd.Wait()
	}()
	t.Cleanup(server.cleanup)

	return server
}

func (h *exampleSmokeHarness) requireExposureCert(t *testing.T) {
	t.Helper()

	if _, err := os.Stat(h.exposureCertPath); err != nil {
		t.Fatalf("expected exposure certificate at %s: %v", h.exposureCertPath, err)
	}
}

func (h *exampleSmokeHarness) curlOutput(t *testing.T, label, port, host, path string) string {
	t.Helper()

	var output string
	h.retry(t, label, 60, func(int) error {
		result := h.runCommand(t, label, h.repoRoot, "curl",
			"--silent", "--show-error", "--fail-with-body",
			"--connect-timeout", "5",
			"--max-time", "15",
			"--cacert", h.exposureCertPath,
			"--resolve", host+":"+port+":127.0.0.1",
			"https://"+host+":"+port+path,
		)
		if result.err != nil {
			return result.failure(label)
		}
		output = result.stdout
		return nil
	})
	return output
}

func (h *exampleSmokeHarness) curlHeaders(t *testing.T, label, port, host, path string) string {
	t.Helper()

	headerPath := filepath.Join(h.tmpDir, strings.ReplaceAll(label, " ", "-")+".headers")
	bodyPath := headerPath + ".body"

	h.retry(t, label, 60, func(int) error {
		result := h.runCommand(t, label, h.repoRoot, "curl",
			"--silent", "--show-error", "--fail-with-body",
			"--connect-timeout", "5",
			"--max-time", "15",
			"--cacert", h.exposureCertPath,
			"--dump-header", headerPath,
			"--output", bodyPath,
			"--resolve", host+":"+port+":127.0.0.1",
			"https://"+host+":"+port+path,
		)
		if result.err != nil {
			return result.failure(label)
		}
		return nil
	})

	content, err := os.ReadFile(headerPath)
	if err != nil {
		t.Fatalf("read %s: %v", headerPath, err)
	}
	return string(content)
}

func (h *exampleSmokeHarness) curlStatus(t *testing.T, label, port, host, path, expectedStatus string) string {
	t.Helper()

	outputPath := filepath.Join(h.tmpDir, strings.ReplaceAll(label, " ", "-")+".out")
	var status string

	h.retry(t, label, 60, func(int) error {
		result := h.runCommand(t, label, h.repoRoot, "curl",
			"--silent", "--show-error",
			"--connect-timeout", "5",
			"--max-time", "15",
			"--cacert", h.exposureCertPath,
			"--write-out", "%{http_code}",
			"--output", outputPath,
			"--resolve", host+":"+port+":127.0.0.1",
			"https://"+host+":"+port+path,
		)
		if result.err != nil {
			return result.failure(label)
		}
		status = strings.TrimSpace(result.stdout)
		if status != expectedStatus {
			return fmt.Errorf("got HTTP %s, want %s", status, expectedStatus)
		}
		return nil
	})

	content, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read %s: %v", outputPath, err)
	}
	return string(content)
}

func (h *exampleSmokeHarness) retry(t *testing.T, label string, maxAttempts int, fn func(attempt int) error) {
	t.Helper()

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := fn(attempt); err == nil {
			return
		} else {
			lastErr = err
		}

		if attempt < maxAttempts {
			t.Logf("%s failed on attempt %d/%d: %v; retrying", label, attempt, maxAttempts, lastErr)
			select {
			case <-time.After(time.Second):
			case <-t.Context().Done():
				t.Fatalf("%s canceled while waiting to retry: %v", label, t.Context().Err())
			}
		}
	}
	t.Fatalf("%s failed after %d attempts: %v", label, maxAttempts, lastErr)
}

type multiHostServer struct {
	cmd        *exec.Cmd
	done       chan error
	waited     bool
	waitErr    error
	stdoutFile *os.File
	stderrFile *os.File
	stdoutPath string
	stderrPath string
}

func (s *multiHostServer) waitForExposure(t *testing.T) string {
	t.Helper()

	for attempt := 1; attempt <= 120; attempt++ {
		if err, exited := s.pollExit(); exited {
			t.Fatalf("multi-host example exited before exposure was ready: %v\n%s", err, s.logs())
		}

		stderr, err := os.ReadFile(s.stderrPath)
		if err != nil {
			t.Fatalf("read multi-host stderr log: %v", err)
		}
		match := multiHostExposurePattern.FindStringSubmatch(string(stderr))
		if len(match) == 2 {
			return match[1]
		}

		select {
		case <-time.After(time.Second):
		case <-t.Context().Done():
			t.Fatalf("canceled waiting for multi-host exposure: %v", t.Context().Err())
		}
	}

	t.Fatalf("timed out waiting for multi-host example exposure\n%s", s.logs())
	return ""
}

func (s *multiHostServer) cleanup() {
	if s.cmd.Process != nil && !s.waited {
		_ = s.cmd.Process.Signal(os.Interrupt)
		select {
		case s.waitErr = <-s.done:
		case <-time.After(30 * time.Second):
			_ = s.cmd.Process.Kill()
			s.waitErr = <-s.done
		}
		s.waited = true
	}
	_ = s.stdoutFile.Close()
	_ = s.stderrFile.Close()
}

func (s *multiHostServer) pollExit() (error, bool) {
	if s.waited {
		return s.waitErr, true
	}
	select {
	case err := <-s.done:
		s.waitErr = err
		s.waited = true
		return err, true
	default:
		return nil, false
	}
}

func (s *multiHostServer) logs() string {
	stdout, _ := os.ReadFile(s.stdoutPath)
	stderr, _ := os.ReadFile(s.stderrPath)
	return fmt.Sprintf("stdout:\n%s\nstderr:\n%s", stdout, stderr)
}

type commandResult struct {
	stdout string
	stderr string
	err    error
}

func (r commandResult) failure(label string) error {
	return fmt.Errorf("%s failed: %w\nstdout:\n%s\nstderr:\n%s", label, r.err, r.stdout, r.stderr)
}

func requireEnv(t *testing.T, name string) string {
	t.Helper()

	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s is required", name)
	}
	return value
}

func xdgConfigHome(t *testing.T) string {
	t.Helper()

	if value := os.Getenv("XDG_CONFIG_HOME"); value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve home dir: %v", err)
	}
	return filepath.Join(home, ".config")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func requireOutputLine(t *testing.T, output, want string) {
	t.Helper()

	if !hasOutputLine(output, want) {
		t.Fatalf("expected output line %q missing from:\n%s", want, output)
	}
}

func hasOutputLine(output, want string) bool {
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		if strings.TrimRight(line, "\r") == want {
			return true
		}
	}
	return false
}

func hasOutputLineFold(output, want string) bool {
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		if strings.EqualFold(strings.TrimRight(line, "\r"), want) {
			return true
		}
	}
	return false
}
