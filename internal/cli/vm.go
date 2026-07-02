package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/buildkite/cleanroom/internal/gateway"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/sporevm"
)

type VMCreateCommand struct {
	Name          string `arg:"" required:"" help:"Named VM lifecycle target"`
	Dir           string `arg:"" optional:"" default:"." help:"Repository directory containing cleanroom policy"`
	Backend       string `help:"SporeVM backend"`
	GatewaySocket string `name:"gateway-socket" help:"Bind an existing Unix socket as gateway.cleanroom.internal:8170"`
	Wait          bool   `help:"Accepted for CLI compatibility; libspore create waits for readiness"`
	LaunchSeconds int64  `help:"VM boot/guest-agent readiness timeout in seconds"`
	JSON          bool   `help:"Print libspore response as JSON"`
}

type VMExecCommand struct {
	Name    string   `arg:"" required:"" help:"Named VM lifecycle target"`
	JSON    bool     `help:"Print libspore response as JSON"`
	Command []string `arg:"" passthrough:"partial" required:"" help:"Command to execute"`
}

type VMCaptureCommand struct {
	Name string `arg:"" required:"" help:"Named VM lifecycle target"`
	Out  string `name:"out" required:"" help:"Output spore bundle directory"`
	JSON bool   `help:"Print libspore response as JSON"`
}

type VMResumeCommand struct {
	SporeDir      string `arg:"" name:"spore-dir" required:"" help:"Spore bundle directory"`
	Name          string `name:"name" required:"" help:"Named VM lifecycle target"`
	GatewaySocket string `name:"gateway-socket" help:"Bind an existing Unix socket for a captured Cleanroom gateway service"`
	JSON          bool   `help:"Print libspore response as JSON"`
}

type VMDestroyCommand struct {
	Name string `arg:"" required:"" help:"Named VM lifecycle target"`
	JSON bool   `help:"Print libspore response as JSON"`
}

const cleanroomAnnotationPrefix = "dev.buildkite.cleanroom."
const cleanroomGatewayServiceName = "cleanroom-gateway"

func (c *VMCreateCommand) Run(ctx *runtimeContext) error {
	cwd, err := resolveCWD(ctx.CWD, c.Dir)
	if err != nil {
		return err
	}
	compiled, policySource, err := ctx.Loader.LoadAndCompile(cwd)
	if err != nil {
		return err
	}
	if err := validateVMPolicy(compiled); err != nil {
		return err
	}
	networkRules, err := vmNetworkRules(compiled)
	if err != nil {
		return err
	}
	boundServices, err := vmGatewayServices(ctx.CWD, c.GatewaySocket)
	if err != nil {
		return err
	}
	annotations, err := vmCreateAnnotations(ctx, cwd, policySource, compiled, networkRules, boundServices)
	if err != nil {
		return err
	}
	client, err := newSporeVMClient()
	if err != nil {
		return err
	}
	defer client.Close()

	runCtx := context.Background()
	if len(networkRules) > 0 || len(boundServices) > 0 {
		caps, err := client.NetworkCapabilities(runCtx)
		if err != nil {
			return fmt.Errorf("read libspore network capabilities: %w", err)
		}
		if len(networkRules) > 0 && (!caps.Supported || !caps.ExactHostPort) {
			return errors.New("libspore does not support exact host-plus-port network rules")
		}
		if len(boundServices) > 0 && (!caps.Supported || !caps.BoundServices) {
			return errors.New("libspore does not support bound Unix services")
		}
	}

	result, err := client.CreateNamed(runCtx, sporevm.CreateNamedOptions{
		Name:           c.Name,
		Backend:        strings.TrimSpace(c.Backend),
		ImageRef:       strings.TrimSpace(compiled.ImageRef),
		MemoryBytes:    vmMemoryBytes(compiled),
		VCPUs:          vmVCPUs(compiled),
		TimeoutMS:      launchTimeoutMS(c.LaunchSeconds),
		NetworkEnabled: len(networkRules) > 0 || len(boundServices) > 0,
		NetworkRules:   networkRules,
		BoundServices:  boundServices,
		Annotations:    annotations,
	})
	if err != nil {
		return err
	}
	return writeVMResult(ctx, result, c.JSON, fmt.Sprintf("created vm %s", c.Name))
}

func (c *VMExecCommand) Run(ctx *runtimeContext) error {
	command := executionCommandArgs(c.Command)
	if len(command) == 0 {
		return errors.New("missing command")
	}
	client, err := newSporeVMClient()
	if err != nil {
		return err
	}
	defer client.Close()

	result, err := client.ExecNamed(context.Background(), sporevm.ExecNamedOptions{
		Name: c.Name,
		Argv: command,
	})
	if err != nil {
		return err
	}
	return writeVMExecResult(ctx, result, c.JSON)
}

func (c *VMCaptureCommand) Run(ctx *runtimeContext) error {
	client, err := newSporeVMClient()
	if err != nil {
		return err
	}
	defer client.Close()

	result, err := client.SnapshotNamed(context.Background(), sporevm.SnapshotNamedOptions{
		Name:        c.Name,
		OutDir:      strings.TrimSpace(c.Out),
		Continue:    true,
		Annotations: vmCaptureAnnotations(ctx),
	})
	if err != nil {
		return err
	}
	return writeVMResult(ctx, result, c.JSON, fmt.Sprintf("captured vm %s to %s", c.Name, c.Out))
}

func (c *VMResumeCommand) Run(ctx *runtimeContext) error {
	client, err := newSporeVMClient()
	if err != nil {
		return err
	}
	defer client.Close()

	runCtx := context.Background()
	sporeDir := strings.TrimSpace(c.SporeDir)
	provenance, err := vmInspectCleanroomProvenance(runCtx, client, sporeDir)
	if err != nil {
		return err
	}
	bindings, err := vmResumeGatewayBindings(ctx.CWD, c.GatewaySocket, provenance.GatewayServices)
	if err != nil {
		return err
	}

	result, err := client.ResumeNamed(runCtx, sporevm.ResumeNamedOptions{
		SporeDir:             sporeDir,
		Name:                 c.Name,
		BoundServiceBindings: bindings,
	})
	if err != nil {
		return err
	}
	return writeVMResult(ctx, result, c.JSON, fmt.Sprintf("resumed vm %s from %s", c.Name, c.SporeDir))
}

func (c *VMDestroyCommand) Run(ctx *runtimeContext) error {
	client, err := newSporeVMClient()
	if err != nil {
		return err
	}
	defer client.Close()

	result, err := client.RemoveNamed(context.Background(), sporevm.RemoveNamedOptions{Name: c.Name})
	if err != nil {
		return err
	}
	return writeVMResult(ctx, result, c.JSON, fmt.Sprintf("destroyed vm %s", c.Name))
}

var newSporeVMClient = func() (sporevm.Client, error) {
	client, err := sporevm.New()
	if err != nil {
		return nil, fmt.Errorf("connect libspore: %w", err)
	}
	return client, nil
}

type vmCleanroomProvenance struct {
	NetworkRules    []sporevm.NetworkRule
	GatewayServices []vmGatewayServiceRequirement
}

type vmGatewayServiceRequirement struct {
	Name      string `json:"name"`
	GuestHost string `json:"guest_host"`
	GuestPort uint16 `json:"guest_port"`
}

func validateVMPolicy(compiled *policy.CompiledPolicy) error {
	if compiled == nil {
		return errors.New("missing compiled policy")
	}
	if strings.TrimSpace(compiled.ImageRef) == "" {
		return errors.New("cleanroom create requires sandbox.image.ref")
	}
	if compiled.HasStageScopedNetwork() {
		return errors.New("cleanroom create does not yet translate stage-scoped network policy to libspore")
	}
	if _, err := vmNetworkRules(compiled); err != nil {
		return err
	}
	if compiled.RequiresDockerService() {
		return errors.New("cleanroom create does not yet translate docker service policy to libspore")
	}
	if len(compiled.Services.Blocks) > 0 || len(compiled.Services.Command) > 0 {
		return errors.New("cleanroom create does not yet translate service stages to libspore")
	}
	if len(compiled.Dependencies.Blocks) > 0 || len(compiled.Dependencies.Command) > 0 {
		return errors.New("cleanroom create does not yet translate dependency stages to libspore")
	}
	if len(compiled.Run.Before) > 0 {
		return errors.New("cleanroom create does not yet translate run.before hooks to libspore")
	}
	return nil
}

func vmNetworkRules(compiled *policy.CompiledPolicy) ([]sporevm.NetworkRule, error) {
	if compiled == nil || len(compiled.Allow) == 0 {
		return nil, nil
	}
	rules := make([]sporevm.NetworkRule, 0, len(compiled.Allow))
	for _, rule := range compiled.Allow {
		host := strings.TrimSpace(rule.Host)
		if host == "" {
			return nil, errors.New("cleanroom create requires network allow rules to include a host")
		}
		if len(rule.Ports) == 0 {
			return nil, fmt.Errorf("cleanroom create requires network allow rule for %s to include at least one port", host)
		}
		ports := make([]uint16, 0, len(rule.Ports))
		for _, port := range rule.Ports {
			if port < 1 || port > 65535 {
				return nil, fmt.Errorf("cleanroom create does not support network allow port %d for %s", port, host)
			}
			ports = append(ports, uint16(port))
		}
		rules = append(rules, sporevm.NetworkRule{
			Host:  host,
			Ports: ports,
		})
	}
	return rules, nil
}

func vmCreateAnnotations(ctx *runtimeContext, cwd, policySource string, compiled *policy.CompiledPolicy, networkRules []sporevm.NetworkRule, boundServices []sporevm.BoundUnixService) (map[string]string, error) {
	annotations := vmBaseAnnotations(ctx)
	if compiled != nil {
		vmSetAnnotation(annotations, cleanroomAnnotationPrefix+"policy.hash", compiled.Hash)
		vmSetAnnotation(annotations, cleanroomAnnotationPrefix+"image.ref", compiled.ImageRef)
		vmSetAnnotation(annotations, cleanroomAnnotationPrefix+"image.digest", compiled.ImageDigest)
	}
	vmSetAnnotation(annotations, cleanroomAnnotationPrefix+"policy.source", vmAnnotationPath(policySource))
	vmSetAnnotation(annotations, cleanroomAnnotationPrefix+"workspace.dir", vmAnnotationPath(cwd))
	vmAddGitAnnotations(annotations, cwd)

	networkValue, err := vmNetworkRulesAnnotation(networkRules)
	if err != nil {
		return nil, err
	}
	vmSetAnnotation(annotations, cleanroomAnnotationPrefix+"network.rules", networkValue)
	gatewayValue, err := vmGatewayServicesAnnotation(boundServices)
	if err != nil {
		return nil, err
	}
	vmSetAnnotation(annotations, cleanroomAnnotationPrefix+"gateway.services", gatewayValue)
	return annotations, nil
}

func vmInspectCleanroomProvenance(ctx context.Context, client sporevm.Client, sporeDir string) (vmCleanroomProvenance, error) {
	sporeDir = strings.TrimSpace(sporeDir)
	if sporeDir == "" {
		return vmCleanroomProvenance{}, errors.New("missing spore directory")
	}
	result, err := client.InspectSpore(ctx, sporevm.InspectSporeOptions{SporeDir: sporeDir})
	if err != nil {
		return vmCleanroomProvenance{}, fmt.Errorf("inspect Cleanroom provenance: %w", err)
	}
	return vmCleanroomProvenanceFromAnnotations(result.Annotations)
}

func vmCleanroomProvenanceFromAnnotations(annotations map[string]string) (vmCleanroomProvenance, error) {
	version := strings.TrimSpace(annotations[cleanroomAnnotationPrefix+"provenance.version"])
	if version == "" {
		return vmCleanroomProvenance{}, errors.New("spore is missing Cleanroom provenance")
	}
	if version != "1" {
		return vmCleanroomProvenance{}, fmt.Errorf("unsupported Cleanroom provenance version %q", version)
	}

	networkRules, err := vmNetworkRulesFromAnnotation(annotations[cleanroomAnnotationPrefix+"network.rules"])
	if err != nil {
		return vmCleanroomProvenance{}, err
	}
	gatewayServices, err := vmGatewayServicesFromAnnotation(annotations[cleanroomAnnotationPrefix+"gateway.services"])
	if err != nil {
		return vmCleanroomProvenance{}, err
	}
	return vmCleanroomProvenance{
		NetworkRules:    networkRules,
		GatewayServices: gatewayServices,
	}, nil
}

func vmNetworkRulesFromAnnotation(value string) ([]sporevm.NetworkRule, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	type ruleAnnotation struct {
		Host  string   `json:"host"`
		Ports []uint16 `json:"ports"`
	}
	var raw []ruleAnnotation
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return nil, fmt.Errorf("decode cleanroom network rule provenance: %w", err)
	}
	rules := make([]sporevm.NetworkRule, 0, len(raw))
	for i, rule := range raw {
		host := strings.TrimSpace(rule.Host)
		if host == "" {
			return nil, fmt.Errorf("cleanroom network rule provenance entry %d is missing host", i)
		}
		if len(rule.Ports) == 0 {
			return nil, fmt.Errorf("cleanroom network rule provenance entry %d is missing ports", i)
		}
		for _, port := range rule.Ports {
			if port == 0 {
				return nil, fmt.Errorf("cleanroom network rule provenance entry %d contains invalid port 0", i)
			}
		}
		rules = append(rules, sporevm.NetworkRule{
			Host:  host,
			Ports: append([]uint16(nil), rule.Ports...),
		})
	}
	return rules, nil
}

func vmGatewayServicesFromAnnotation(value string) ([]vmGatewayServiceRequirement, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	var services []vmGatewayServiceRequirement
	if err := json.Unmarshal([]byte(value), &services); err != nil {
		return nil, fmt.Errorf("decode cleanroom gateway service provenance: %w", err)
	}
	if len(services) == 0 {
		return nil, errors.New("cleanroom gateway service provenance is empty")
	}
	for i, service := range services {
		if strings.TrimSpace(service.Name) == "" {
			return nil, fmt.Errorf("cleanroom gateway service provenance entry %d is missing name", i)
		}
		if strings.TrimSpace(service.GuestHost) == "" {
			return nil, fmt.Errorf("cleanroom gateway service provenance entry %d is missing guest host", i)
		}
		if service.GuestPort == 0 {
			return nil, fmt.Errorf("cleanroom gateway service provenance entry %d contains invalid guest port 0", i)
		}
	}
	return services, nil
}

func vmResumeGatewayBindings(base, socketPath string, services []vmGatewayServiceRequirement) ([]sporevm.BoundUnixServiceBinding, error) {
	socketPath = strings.TrimSpace(socketPath)
	if len(services) == 0 {
		if socketPath != "" {
			return nil, errors.New("captured Cleanroom spore does not require a gateway service")
		}
		return nil, nil
	}
	if len(services) != 1 {
		return nil, fmt.Errorf("cleanroom resume supports one gateway service, found %d", len(services))
	}
	service := services[0]
	if service.Name != cleanroomGatewayServiceName {
		return nil, fmt.Errorf("unsupported Cleanroom gateway service %q", service.Name)
	}
	if service.GuestHost != gateway.GuestGatewayHostname || service.GuestPort != gateway.DefaultPort {
		return nil, fmt.Errorf("unsupported Cleanroom gateway endpoint %s:%d", service.GuestHost, service.GuestPort)
	}
	if socketPath == "" {
		return nil, fmt.Errorf("cleanroom resume requires --gateway-socket for captured service %q", service.Name)
	}

	boundServices, err := vmGatewayServices(base, socketPath)
	if err != nil {
		return nil, err
	}
	if len(boundServices) != 1 {
		return nil, errors.New("expected one cleanroom gateway service binding")
	}
	return []sporevm.BoundUnixServiceBinding{{
		Name:     service.Name,
		UnixPath: boundServices[0].UnixPath,
	}}, nil
}

func vmCaptureAnnotations(ctx *runtimeContext) map[string]string {
	annotations := vmBaseAnnotations(ctx)
	annotations[cleanroomAnnotationPrefix+"capture.continue_after"] = "true"
	return annotations
}

func vmBaseAnnotations(ctx *runtimeContext) map[string]string {
	annotations := map[string]string{
		cleanroomAnnotationPrefix + "provenance.version": "1",
	}
	if ctx != nil {
		vmSetAnnotation(annotations, cleanroomAnnotationPrefix+"version", ctx.Version)
	}
	return annotations
}

func vmAddGitAnnotations(annotations map[string]string, cwd string) {
	if annotations == nil || strings.TrimSpace(cwd) == "" {
		return
	}
	if commit, err := gitOutput(cwd, "rev-parse", "HEAD"); err == nil {
		vmSetAnnotation(annotations, cleanroomAnnotationPrefix+"workspace.git.commit", commit)
	}
	if remote, err := gitOutput(cwd, "config", "--get", "remote.origin.url"); err == nil {
		vmSetAnnotation(annotations, cleanroomAnnotationPrefix+"workspace.git.remote", remote)
	}
	if status, err := gitOutput(cwd, "status", "--porcelain"); err == nil {
		dirty := "false"
		if strings.TrimSpace(status) != "" {
			dirty = "true"
		}
		annotations[cleanroomAnnotationPrefix+"workspace.git.dirty"] = dirty
	}
}

func vmGatewayServices(base, socketPath string) ([]sporevm.BoundUnixService, error) {
	socketPath = strings.TrimSpace(socketPath)
	if socketPath == "" {
		return nil, nil
	}
	if !filepath.IsAbs(socketPath) {
		if strings.TrimSpace(base) != "" {
			socketPath = filepath.Join(base, socketPath)
		} else if absolute, err := filepath.Abs(socketPath); err == nil {
			socketPath = absolute
		}
	}
	socketPath = filepath.Clean(socketPath)
	info, err := os.Stat(socketPath)
	if err != nil {
		return nil, fmt.Errorf("stat gateway socket %q: %w", socketPath, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("gateway socket %q is not a Unix socket", socketPath)
	}
	return []sporevm.BoundUnixService{{
		Name:      cleanroomGatewayServiceName,
		GuestHost: gateway.GuestGatewayHostname,
		GuestPort: gateway.DefaultPort,
		UnixPath:  socketPath,
	}}, nil
}

func vmNetworkRulesAnnotation(rules []sporevm.NetworkRule) (string, error) {
	if len(rules) == 0 {
		return "", nil
	}
	type ruleAnnotation struct {
		Host  string   `json:"host"`
		Ports []uint16 `json:"ports"`
	}
	out := make([]ruleAnnotation, 0, len(rules))
	for _, rule := range rules {
		out = append(out, ruleAnnotation{
			Host:  rule.Host,
			Ports: append([]uint16(nil), rule.Ports...),
		})
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("encode cleanroom network rule annotations: %w", err)
	}
	return string(raw), nil
}

func vmGatewayServicesAnnotation(services []sporevm.BoundUnixService) (string, error) {
	if len(services) == 0 {
		return "", nil
	}
	type serviceAnnotation struct {
		Name      string `json:"name"`
		GuestHost string `json:"guest_host"`
		GuestPort uint16 `json:"guest_port"`
	}
	out := make([]serviceAnnotation, 0, len(services))
	for _, service := range services {
		out = append(out, serviceAnnotation{
			Name:      service.Name,
			GuestHost: service.GuestHost,
			GuestPort: service.GuestPort,
		})
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("encode cleanroom gateway service annotations: %w", err)
	}
	return string(raw), nil
}

func vmAnnotationPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	return filepath.Clean(path)
}

func vmSetAnnotation(annotations map[string]string, key, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	annotations[key] = value
}

func vmMemoryBytes(compiled *policy.CompiledPolicy) uint64 {
	if compiled == nil || compiled.Resources == nil || compiled.Resources.MemoryBytes <= 0 {
		return 0
	}
	return uint64(compiled.Resources.MemoryBytes)
}

func vmVCPUs(compiled *policy.CompiledPolicy) uint32 {
	if compiled == nil || compiled.Resources == nil || compiled.Resources.VCPUs <= 0 {
		return 0
	}
	return uint32(compiled.Resources.VCPUs)
}

func launchTimeoutMS(seconds int64) uint64 {
	if seconds <= 0 {
		return 0
	}
	return uint64(seconds) * 1000
}

func writeVMResult(ctx *runtimeContext, result sporevm.JSONResult, jsonOutput bool, message string) error {
	if jsonOutput || len(result.RawJSON) > 0 && message == "" {
		if len(result.RawJSON) == 0 {
			_, err := fmt.Fprintln(ctx.Stdout, "{}")
			return err
		}
		_, err := ctx.Stdout.Write(result.RawJSON)
		if err != nil {
			return err
		}
		if len(result.RawJSON) > 0 && result.RawJSON[len(result.RawJSON)-1] != '\n' {
			_, err = fmt.Fprintln(ctx.Stdout)
		}
		return err
	}
	_, err := fmt.Fprintln(ctx.Stdout, message)
	return err
}

type vmExecResult struct {
	ExitCode        int         `json:"exit_code"`
	Stdout          vmExecBytes `json:"stdout"`
	Stderr          vmExecBytes `json:"stderr"`
	StdoutTruncated bool        `json:"stdout_truncated"`
	StderrTruncated bool        `json:"stderr_truncated"`
}

type vmExecBytes []byte

func (b *vmExecBytes) UnmarshalJSON(raw []byte) error {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		*b = []byte(s)
		return nil
	}
	var bytes []byte
	if err := json.Unmarshal(raw, &bytes); err == nil {
		*b = bytes
		return nil
	}
	return errors.New("expected string or byte array")
}

func writeVMExecResult(ctx *runtimeContext, result sporevm.JSONResult, jsonOutput bool) error {
	var execResult vmExecResult
	if err := json.Unmarshal(result.RawJSON, &execResult); err != nil {
		if jsonOutput {
			return errors.Join(writeVMResult(ctx, result, true, ""), fmt.Errorf("decode libspore exec result: %w", err))
		}
		return fmt.Errorf("decode libspore exec result: %w", err)
	}
	if execResult.ExitCode < 0 || execResult.ExitCode > 255 {
		return fmt.Errorf("decode libspore exec result: invalid exit code %d", execResult.ExitCode)
	}

	if jsonOutput {
		if err := writeVMResult(ctx, result, true, ""); err != nil {
			return err
		}
	} else {
		if len(execResult.Stdout) > 0 {
			if _, err := ctx.Stdout.Write(execResult.Stdout); err != nil {
				return err
			}
		}
		stderr := ctx.stderr()
		if len(execResult.Stderr) > 0 {
			if _, err := stderr.Write(execResult.Stderr); err != nil {
				return err
			}
		}
		if execResult.StdoutTruncated {
			if _, err := fmt.Fprintln(stderr, "cleanroom exec: stdout truncated by libspore"); err != nil {
				return err
			}
		}
		if execResult.StderrTruncated {
			if _, err := fmt.Fprintln(stderr, "cleanroom exec: stderr truncated by libspore"); err != nil {
				return err
			}
		}
	}

	if execResult.ExitCode != 0 {
		return exitCodeError{code: execResult.ExitCode}
	}
	return nil
}
