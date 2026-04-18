package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"
)

const (
	benchmarkName = "repository-bootstrap"
	pollInterval  = 50 * time.Millisecond
)

type scenario string

const (
	scenarioColdHost            scenario = "cold-host"
	scenarioWarmRepositoryStore scenario = "warm-repository-store"
	scenarioWarmWorkspaceStage  scenario = "warm-workspace-stage"
)

type config struct {
	Scenario       scenario
	Iterations     int
	Warmup         int
	Backend        string
	Chdir          string
	OutputDir      string
	CleanroomBin   string
	TimeoutSeconds int
	KeepTempDir    bool
}

type gitMetadata struct {
	Commit string `json:"commit,omitempty"`
	Dirty  bool   `json:"dirty"`
}

type runLogs struct {
	Server       string `json:"server,omitempty"`
	CreateJSON   string `json:"create_json,omitempty"`
	CreateStderr string `json:"create_stderr,omitempty"`
	RMStdout     string `json:"rm_stdout,omitempty"`
	RMStderr     string `json:"rm_stderr,omitempty"`
}

type runResult struct {
	Label          string    `json:"label"`
	Index          int       `json:"index"`
	SandboxID      string    `json:"sandbox_id"`
	Backend        string    `json:"backend"`
	ElapsedNS      int64     `json:"elapsed_ns"`
	ElapsedSeconds float64   `json:"elapsed_seconds"`
	Host           string    `json:"host"`
	EnvRoot        string    `json:"env_root,omitempty"`
	Logs           runLogs   `json:"logs,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at"`
}

type summary struct {
	Mean   float64 `json:"mean"`
	Median float64 `json:"median"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
}

type payload struct {
	Benchmark    string      `json:"benchmark"`
	Timestamp    string      `json:"timestamp"`
	Scenario     scenario    `json:"scenario"`
	Config       any         `json:"config"`
	Git          gitMetadata `json:"git,omitempty"`
	Platform     any         `json:"platform"`
	TempRoot     string      `json:"temp_root"`
	TempRootKept bool        `json:"temp_root_kept"`
	Seed         *runResult  `json:"seed"`
	WarmupRuns   []runResult `json:"warmup_runs"`
	Runs         []runResult `json:"runs"`
	Summary      summary     `json:"summary"`
}

type benchmarkRunner struct {
	cfg          config
	tmpRoot      string
	socketRoot   string
	outputPath   string
	timestamp    string
	git          gitMetadata
	keepTempDir  bool
	cleanupArmed bool
}

type runningServer struct {
	cmd      *exec.Cmd
	host     string
	logPath  string
	waitErrC chan error
}

type createResponse struct {
	SandboxIDSnake string `json:"sandbox_id"`
	SandboxIDCamel string `json:"sandboxId"`
	Backend        string `json:"backend"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) (runErr error) {
	cfg, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}

	runner, err := newBenchmarkRunner(cfg)
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := runner.cleanup(); cleanupErr != nil && runErr == nil {
			runErr = cleanupErr
		}
	}()

	fmt.Fprintln(stdout, "Benchmarking repository bootstrap")
	fmt.Fprintf(stdout, "- scenario: %s\n", runner.cfg.Scenario)
	fmt.Fprintf(stdout, "- directory: %s\n", runner.cfg.Chdir)
	fmt.Fprintf(stdout, "- iterations: %d\n", runner.cfg.Iterations)
	fmt.Fprintf(stdout, "- warmup: %d\n", runner.cfg.Warmup)
	fmt.Fprintf(stdout, "- output: %s\n", runner.outputPath)
	fmt.Fprintf(stdout, "- temp root: %s\n", runner.tmpRoot)

	seed, err := runner.seedRun()
	if err != nil {
		runner.keepTempDir = true
		return err
	}
	if seed != nil {
		fmt.Fprintln(stdout, "- seed: performed")
	}

	if runner.cfg.Warmup > 0 {
		fmt.Fprintln(stdout, "Running warmup iterations")
	}
	warmupRuns := make([]runResult, 0, runner.cfg.Warmup)
	for i := range runner.cfg.Warmup {
		result, err := runner.runIteration("warmup", i+1)
		if err != nil {
			runner.keepTempDir = true
			return err
		}
		warmupRuns = append(warmupRuns, result)
	}

	fmt.Fprintln(stdout, "Running measured iterations")
	runs := make([]runResult, 0, runner.cfg.Iterations)
	elapsed := make([]float64, 0, runner.cfg.Iterations)
	for i := range runner.cfg.Iterations {
		result, err := runner.runIteration("run", i+1)
		if err != nil {
			runner.keepTempDir = true
			return err
		}
		runs = append(runs, result)
		elapsed = append(elapsed, result.ElapsedSeconds)
		fmt.Fprintf(stdout, "  run %d: %.6fs\n", i+1, result.ElapsedSeconds)
	}

	data := payload{
		Benchmark: benchmarkName,
		Timestamp: runner.timestamp,
		Scenario:  runner.cfg.Scenario,
		Config: map[string]any{
			"chdir":           runner.cfg.Chdir,
			"cleanroom_bin":   runner.cfg.CleanroomBin,
			"backend":         defaultString(runner.cfg.Backend, "default"),
			"iterations":      runner.cfg.Iterations,
			"warmup":          runner.cfg.Warmup,
			"timeout_seconds": runner.cfg.TimeoutSeconds,
		},
		Git: runner.git,
		Platform: map[string]string{
			"goos":   runtime.GOOS,
			"goarch": runtime.GOARCH,
		},
		TempRoot:     runner.tmpRoot,
		TempRootKept: runner.keepTempDir,
		Seed:         seed,
		WarmupRuns:   warmupRuns,
		Runs:         runs,
		Summary:      computeSummary(elapsed),
	}

	if err := writeJSONFile(runner.outputPath, data); err != nil {
		runner.keepTempDir = true
		return fmt.Errorf("write output: %w", err)
	}

	fmt.Fprintf(stdout, "Results written to %s\n", runner.outputPath)
	return nil
}

func parseFlags(args []string, stderr io.Writer) (config, error) {
	cfg := config{
		Scenario:       scenarioColdHost,
		Iterations:     5,
		Warmup:         1,
		Chdir:          ".",
		OutputDir:      "benchmarks/results",
		CleanroomBin:   defaultCleanroomBin(),
		TimeoutSeconds: 20,
	}

	fs := flag.NewFlagSet("benchmark_repository_bootstrap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, `Benchmark repo-aware repository bootstrap under isolated cold/warm host-cache scenarios.

Usage:
  go run ./scripts/benchmark_repository_bootstrap [options]

Options:
  --scenario <name>         Benchmark scenario: cold-host | warm-repository-store | warm-workspace-stage
                            (default: cold-host)
  -n, --iterations <count>  Number of measured runs (default: 5)
  --warmup <count>          Number of warmup runs before measuring (default: 1)
  --backend <name>          Optional backend override for cleanroom create
  -c, --chdir <path>        Repository/policy directory to benchmark (default: current directory)
  --output-dir <path>       Output directory (default: benchmarks/results)
  --cleanroom-bin <path>    cleanroom binary path (default: cleanroom from PATH, then ./dist/cleanroom)
  --timeout <seconds>       Server readiness timeout per run (default: 20)
  --keep-temp-dir           Keep the temporary benchmark directory instead of removing it
  -h, --help                Show this help

Notes:
  - This tool starts its own cleanroom server for each seed/run sequence.
  - It isolates XDG cache/state/data/runtime directories per scenario while
    preserving the caller's runtime config discovery.
  - The measured command is: cleanroom create --json
  - The created sandbox is terminated after each run.
`)
	}

	fs.Func("scenario", "benchmark scenario", func(value string) error {
		cfg.Scenario = scenario(value)
		return nil
	})
	fs.IntVar(&cfg.Iterations, "n", cfg.Iterations, "number of measured iterations")
	fs.IntVar(&cfg.Iterations, "iterations", cfg.Iterations, "number of measured iterations")
	fs.IntVar(&cfg.Warmup, "warmup", cfg.Warmup, "number of warmup iterations")
	fs.StringVar(&cfg.Backend, "backend", cfg.Backend, "backend override")
	fs.StringVar(&cfg.Chdir, "c", cfg.Chdir, "benchmark chdir")
	fs.StringVar(&cfg.Chdir, "chdir", cfg.Chdir, "benchmark chdir")
	fs.StringVar(&cfg.OutputDir, "output-dir", cfg.OutputDir, "output directory")
	fs.StringVar(&cfg.CleanroomBin, "cleanroom-bin", cfg.CleanroomBin, "cleanroom binary path")
	fs.IntVar(&cfg.TimeoutSeconds, "timeout", cfg.TimeoutSeconds, "server readiness timeout in seconds")
	fs.BoolVar(&cfg.KeepTempDir, "keep-temp-dir", cfg.KeepTempDir, "keep temp directory")
	help := false
	fs.BoolVar(&help, "h", false, "show help")
	fs.BoolVar(&help, "help", false, "show help")

	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if help {
		fs.Usage()
		return config{}, flag.ErrHelp
	}
	if err := validateConfig(&cfg); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func validateConfig(cfg *config) error {
	switch cfg.Scenario {
	case scenarioColdHost, scenarioWarmRepositoryStore, scenarioWarmWorkspaceStage:
	default:
		return fmt.Errorf("scenario must be one of: %s, %s, %s", scenarioColdHost, scenarioWarmRepositoryStore, scenarioWarmWorkspaceStage)
	}

	if cfg.Iterations <= 0 {
		return errors.New("iterations must be a positive integer")
	}
	if cfg.Warmup < 0 {
		return errors.New("warmup must be a non-negative integer")
	}
	if cfg.TimeoutSeconds <= 0 {
		return errors.New("timeout must be a positive integer")
	}

	absChdir, err := filepath.Abs(cfg.Chdir)
	if err != nil {
		return fmt.Errorf("resolve chdir: %w", err)
	}
	cfg.Chdir = absChdir

	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	if strings.ContainsRune(cfg.CleanroomBin, filepath.Separator) {
		info, err := os.Stat(cfg.CleanroomBin)
		if err != nil {
			return fmt.Errorf("cleanroom binary not found or not executable: %s", cfg.CleanroomBin)
		}
		if info.Mode()&0o111 == 0 {
			return fmt.Errorf("cleanroom binary not found or not executable: %s", cfg.CleanroomBin)
		}
		return nil
	}

	if _, err := exec.LookPath(cfg.CleanroomBin); err != nil {
		return fmt.Errorf("cleanroom binary not found in PATH: %s", cfg.CleanroomBin)
	}
	return nil
}

func newBenchmarkRunner(cfg config) (*benchmarkRunner, error) {
	tmpRoot, err := os.MkdirTemp("/tmp", "crb.")
	if err != nil {
		return nil, fmt.Errorf("create temp root: %w", err)
	}
	socketRoot, err := os.MkdirTemp("/tmp", "crb-sock.")
	if err != nil {
		_ = os.RemoveAll(tmpRoot)
		return nil, fmt.Errorf("create socket root: %w", err)
	}

	timestamp := time.Now().UTC().Format("2006-01-02T15-04-05Z")
	outputPath := filepath.Join(cfg.OutputDir, fmt.Sprintf("%s-%s-%s.json", timestamp, benchmarkName, cfg.Scenario))

	return &benchmarkRunner{
		cfg:          cfg,
		tmpRoot:      tmpRoot,
		socketRoot:   socketRoot,
		outputPath:   outputPath,
		timestamp:    timestamp,
		git:          readGitMetadata(cfg.Chdir),
		keepTempDir:  cfg.KeepTempDir,
		cleanupArmed: true,
	}, nil
}

func (r *benchmarkRunner) cleanup() error {
	if !r.cleanupArmed || r.keepTempDir {
		return nil
	}
	if err := os.RemoveAll(r.tmpRoot); err != nil {
		return fmt.Errorf("remove temp root: %w", err)
	}
	if err := os.RemoveAll(r.socketRoot); err != nil {
		return fmt.Errorf("remove socket root: %w", err)
	}
	r.cleanupArmed = false
	return nil
}

func (r *benchmarkRunner) seedRun() (*runResult, error) {
	sharedEnvRoot := filepath.Join(r.tmpRoot, "shared")
	switch r.cfg.Scenario {
	case scenarioWarmRepositoryStore:
		r.resetStageState(sharedEnvRoot)
		r.resetTransportState(sharedEnvRoot)
		result, err := r.runCreateIteration(sharedEnvRoot, "seed", 0)
		if err != nil {
			return nil, err
		}
		r.resetStageState(sharedEnvRoot)
		return &result, nil
	case scenarioWarmWorkspaceStage:
		r.resetStageState(sharedEnvRoot)
		r.resetTransportState(sharedEnvRoot)
		result, err := r.runCreateIteration(sharedEnvRoot, "seed", 0)
		if err != nil {
			return nil, err
		}
		return &result, nil
	default:
		return nil, nil
	}
}

func (r *benchmarkRunner) runIteration(phase string, ordinal int) (runResult, error) {
	label := fmt.Sprintf("%s-%d", phase, ordinal)
	var envRoot string

	switch r.cfg.Scenario {
	case scenarioColdHost:
		envRoot = filepath.Join(r.tmpRoot, label)
		r.resetStageState(envRoot)
		r.resetTransportState(envRoot)
	case scenarioWarmRepositoryStore:
		envRoot = filepath.Join(r.tmpRoot, "shared")
		r.resetStageState(envRoot)
	case scenarioWarmWorkspaceStage:
		envRoot = filepath.Join(r.tmpRoot, "shared")
	default:
		return runResult{}, fmt.Errorf("unsupported scenario %q", r.cfg.Scenario)
	}

	return r.runCreateIteration(envRoot, label, ordinal)
}

func (r *benchmarkRunner) runCreateIteration(envRoot, label string, index int) (runResult, error) {
	server, err := r.startServer(envRoot, label)
	if err != nil {
		return runResult{}, err
	}
	defer server.stop()

	logDir := filepath.Join(envRoot, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return runResult{}, fmt.Errorf("create log dir: %w", err)
	}

	createJSONPath := filepath.Join(logDir, fmt.Sprintf("%s-create.json", label))
	createStderrPath := filepath.Join(logDir, fmt.Sprintf("%s-create.stderr", label))
	rmStdoutPath := filepath.Join(logDir, fmt.Sprintf("%s-rm.stdout", label))
	rmStderrPath := filepath.Join(logDir, fmt.Sprintf("%s-rm.stderr", label))

	createJSONFile, err := os.Create(createJSONPath)
	if err != nil {
		return runResult{}, fmt.Errorf("create create-json log: %w", err)
	}
	defer createJSONFile.Close()

	createStderrFile, err := os.Create(createStderrPath)
	if err != nil {
		return runResult{}, fmt.Errorf("create create-stderr log: %w", err)
	}
	defer createStderrFile.Close()

	args := []string{"create", "--host", server.host, "-c", r.cfg.Chdir, "--json"}
	if r.cfg.Backend != "" {
		args = append(args, "--backend", r.cfg.Backend)
	}

	startedAt := time.Now().UTC()
	start := time.Now()
	if err := r.runCleanroomCommand(envRoot, r.cfg.Chdir, createJSONFile, createStderrFile, args...); err != nil {
		return runResult{}, fmt.Errorf("benchmark create failed for %s: %w (server log: %s, create stderr: %s)", label, err, server.logPath, createStderrPath)
	}
	elapsed := time.Since(start)
	finishedAt := time.Now().UTC()

	response, err := readCreateResponse(createJSONPath)
	if err != nil {
		return runResult{}, fmt.Errorf("read create output for %s: %w", label, err)
	}
	sandboxID := defaultString(response.SandboxIDCamel, response.SandboxIDSnake)
	if sandboxID == "" {
		return runResult{}, fmt.Errorf("create output missing sandbox id for %s", label)
	}

	rmStdoutFile, err := os.Create(rmStdoutPath)
	if err != nil {
		return runResult{}, fmt.Errorf("create rm-stdout log: %w", err)
	}
	defer rmStdoutFile.Close()

	rmStderrFile, err := os.Create(rmStderrPath)
	if err != nil {
		return runResult{}, fmt.Errorf("create rm-stderr log: %w", err)
	}
	defer rmStderrFile.Close()

	if err := r.runCleanroomCommand(envRoot, r.cfg.Chdir, rmStdoutFile, rmStderrFile, "sandbox", "rm", "--host", server.host, sandboxID); err != nil {
		return runResult{}, fmt.Errorf("terminate sandbox %s for %s: %w", sandboxID, label, err)
	}

	result := runResult{
		Label:          label,
		Index:          index,
		SandboxID:      sandboxID,
		Backend:        defaultString(response.Backend, "unknown"),
		ElapsedNS:      elapsed.Nanoseconds(),
		ElapsedSeconds: elapsed.Seconds(),
		Host:           server.host,
		StartedAt:      startedAt,
		FinishedAt:     finishedAt,
	}
	if r.keepTempDir {
		result.EnvRoot = envRoot
		result.Logs = runLogs{
			Server:       server.logPath,
			CreateJSON:   createJSONPath,
			CreateStderr: createStderrPath,
			RMStdout:     rmStdoutPath,
			RMStderr:     rmStderrPath,
		}
	}
	return result, nil
}

func (r *benchmarkRunner) startServer(envRoot, label string) (*runningServer, error) {
	if err := os.MkdirAll(filepath.Join(envRoot, "logs"), 0o755); err != nil {
		return nil, fmt.Errorf("create logs dir: %w", err)
	}

	socketPath := filepath.Join(r.socketRoot, label+".sock")
	if err := os.RemoveAll(socketPath); err != nil {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}
	host := "unix://" + socketPath
	logPath := filepath.Join(envRoot, "logs", fmt.Sprintf("%s-server.log", label))

	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("create server log: %w", err)
	}

	cmd := exec.Command(r.cfg.CleanroomBin, "serve", "--listen", host, "--gateway-listen", "127.0.0.1:0")
	cmd.Dir = r.cfg.Chdir
	cmd.Env = cleanroomEnv(envRoot)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("start cleanroom serve: %w", err)
	}
	if err := logFile.Close(); err != nil {
		return nil, fmt.Errorf("close server log: %w", err)
	}

	server := &runningServer{
		cmd:      cmd,
		host:     host,
		logPath:  logPath,
		waitErrC: make(chan error, 1),
	}
	go func() {
		server.waitErrC <- cmd.Wait()
		close(server.waitErrC)
	}()

	if err := r.waitForServer(envRoot, server); err != nil {
		_ = server.stop()
		return nil, err
	}
	return server, nil
}

func (r *benchmarkRunner) waitForServer(envRoot string, server *runningServer) error {
	deadline := time.Now().Add(time.Duration(r.cfg.TimeoutSeconds) * time.Second)
	for {
		select {
		case err, ok := <-server.waitErrC:
			if !ok || err == nil {
				return fmt.Errorf("cleanroom serve exited before becoming ready (server log: %s)", server.logPath)
			}
			return fmt.Errorf("cleanroom serve exited before becoming ready: %w (server log: %s)", err, server.logPath)
		default:
		}

		if err := r.runCleanroomCommand(envRoot, r.cfg.Chdir, io.Discard, io.Discard, "sandbox", "ls", "--host", server.host); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for cleanroom serve readiness at %s (server log: %s)", server.host, server.logPath)
		}
		time.Sleep(pollInterval)
	}
}

func (s *runningServer) stop() error {
	if s == nil || s.cmd == nil || s.cmd.Process == nil || s.cmd.ProcessState != nil {
		return nil
	}
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		_ = s.cmd.Process.Kill()
		<-s.waitErrC
		return nil
	}

	select {
	case <-s.waitErrC:
		return nil
	case <-time.After(5 * time.Second):
		if err := s.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		<-s.waitErrC
		return nil
	}
}

func (r *benchmarkRunner) runCleanroomCommand(envRoot, dir string, stdout, stderr io.Writer, args ...string) error {
	cmd := exec.Command(r.cfg.CleanroomBin, args...)
	cmd.Dir = dir
	cmd.Env = cleanroomEnv(envRoot)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func (r *benchmarkRunner) resetStageState(envRoot string) {
	_ = os.RemoveAll(filepath.Join(cleanroomStateDir(envRoot), "snapshots"))
	_ = os.RemoveAll(filepath.Join(cleanroomStateDir(envRoot), "executions"))
	_ = os.RemoveAll(filepath.Join(cleanroomCacheDir(envRoot), "stage-caches"))
}

func (r *benchmarkRunner) resetTransportState(envRoot string) {
	_ = os.RemoveAll(filepath.Join(cleanroomStateDir(envRoot), "repos"))
	_ = os.RemoveAll(filepath.Join(cleanroomCacheDir(envRoot), "content-cache"))
}

func cleanroomStateDir(envRoot string) string {
	return filepath.Join(envRoot, "state", "cleanroom")
}

func cleanroomCacheDir(envRoot string) string {
	return filepath.Join(envRoot, "cache", "cleanroom")
}

func cleanroomEnv(envRoot string) []string {
	env := make([]string, 0, len(os.Environ())+4)
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "XDG_CACHE_HOME="),
			strings.HasPrefix(kv, "XDG_STATE_HOME="),
			strings.HasPrefix(kv, "XDG_DATA_HOME="),
			strings.HasPrefix(kv, "XDG_RUNTIME_DIR="):
			continue
		default:
			env = append(env, kv)
		}
	}
	return append(env,
		"XDG_CACHE_HOME="+filepath.Join(envRoot, "cache"),
		"XDG_STATE_HOME="+filepath.Join(envRoot, "state"),
		"XDG_DATA_HOME="+filepath.Join(envRoot, "data"),
		"XDG_RUNTIME_DIR="+filepath.Join(envRoot, "runtime"),
	)
}

func readCreateResponse(path string) (createResponse, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return createResponse{}, err
	}
	var response createResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return createResponse{}, err
	}
	return response, nil
}

func computeSummary(values []float64) summary {
	if len(values) == 0 {
		return summary{}
	}
	ordered := append([]float64(nil), values...)
	slices.Sort(ordered)

	total := 0.0
	for _, value := range ordered {
		total += value
	}

	median := ordered[len(ordered)/2]
	if len(ordered)%2 == 0 {
		median = (ordered[len(ordered)/2-1] + ordered[len(ordered)/2]) / 2
	}

	return summary{
		Mean:   round6(total / float64(len(ordered))),
		Median: round6(median),
		Min:    round6(ordered[0]),
		Max:    round6(ordered[len(ordered)-1]),
	}
}

func round6(value float64) float64 {
	return math.Round(value*1_000_000) / 1_000_000
}

func writeJSONFile(path string, value any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func readGitMetadata(chdir string) gitMetadata {
	var meta gitMetadata
	headCmd := exec.Command("git", "-C", chdir, "rev-parse", "HEAD")
	if output, err := headCmd.Output(); err == nil {
		meta.Commit = strings.TrimSpace(string(output))
	}
	statusCmd := exec.Command("git", "-C", chdir, "status", "--short")
	if output, err := statusCmd.Output(); err == nil {
		meta.Dirty = strings.TrimSpace(string(output)) != ""
	}
	return meta
}

func defaultCleanroomBin() string {
	if path, err := exec.LookPath("cleanroom"); err == nil {
		return path
	}
	if info, err := os.Stat("./dist/cleanroom"); err == nil && info.Mode()&0o111 != 0 {
		return "./dist/cleanroom"
	}
	return "cleanroom"
}

func defaultString(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
