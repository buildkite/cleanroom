package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/buildkite/cleanroom/internal/bytesize"
	"github.com/buildkite/cleanroom/internal/guestenv"
	"github.com/buildkite/cleanroom/internal/ociref"
	"gopkg.in/yaml.v3"
)

const (
	PrimaryPolicyPath  = "cleanroom.yaml"
	FallbackPolicyPath = ".buildkite/cleanroom.yaml"
)

var ErrPolicyNotFound = errors.New("policy not found")

type Loader struct{}

type rawPolicy struct {
	Version int `yaml:"version"`
	Sandbox struct {
		Image struct {
			Ref string `yaml:"ref"`
		} `yaml:"image"`
		Docker       rawDockerConfig    `yaml:"docker"`
		Dependencies rawDependencyStage `yaml:"dependencies"`
		Run          rawRunConfig       `yaml:"run"`
		Services     rawPolicyBlocks    `yaml:"services"`
		Resources    rawResources       `yaml:"resources"`
		Network      rawSandboxNetwork  `yaml:"network"`
		Warmup       []string           `yaml:"warmup"`
		Mediation    rawMediation       `yaml:"mediation"`
	} `yaml:"sandbox"`
}

type rawMediation struct {
	Services []string `yaml:"services"`
}

type rawDependencyCommandSpec []string

type rawRunConfig struct {
	Before rawShellCommandSpec `yaml:"before"`
}

type rawShellCommandSpec []string

type rawDockerConfig struct {
	Required bool `yaml:"required"`
}

type rawPolicyBlocks []rawPolicyBlock

type rawDependencyStage struct {
	Reuse       string
	Blocks      rawPolicyBlocks
	blocksField string
}

type rawPolicyBlock struct {
	Name    string                   `yaml:"name"`
	Command rawDependencyCommandSpec `yaml:"command"`
	Inputs  rawPolicyBlockInputs     `yaml:"inputs"`
	Env     map[string]string        `yaml:"env"`
	Outputs rawPolicyBlockOutputs    `yaml:"outputs"`
}

type rawPolicyBlockInputs struct {
	Files []string `yaml:"files"`
}

type rawPolicyBlockOutputs struct {
	Dirs  []string `yaml:"dirs"`
	Files []string `yaml:"files"`
}

type rawResources struct {
	VCPUs  *int64         `yaml:"vcpus"`
	Memory *bytesize.Size `yaml:"memory"`
	Disk   *bytesize.Size `yaml:"disk"`
}

type rawSandboxNetwork struct {
	Default      string                 `yaml:"default"`
	Allow        rawAllowRules          `yaml:"allow"`
	Dependencies *rawStageNetworkConfig `yaml:"dependencies"`
	Services     *rawStageNetworkConfig `yaml:"services"`
	Execution    *rawStageNetworkConfig `yaml:"execution"`
}

type rawStageNetworkConfig struct {
	Allow rawAllowRules `yaml:"allow"`
}

type rawAllowRule struct {
	Host  string `yaml:"host"`
	Ports []int  `yaml:"ports"`
}

type rawAllowRules []rawAllowRule

type CompiledPolicy struct {
	Version        int                   `json:"version"`
	ImageRef       string                `json:"image_ref"`
	ImageDigest    string                `json:"image_digest"`
	Docker         DockerService         `json:"docker"`
	Services       Services              `json:"services"`
	NetworkDefault string                `json:"network_default"`
	Allow          []AllowRule           `json:"allow"`
	NetworkStages  *NetworkStagePolicies `json:"network_stages,omitempty"`
	Dependencies   Dependencies          `json:"dependencies"`
	Run            Run                   `json:"run"`
	Resources      *Resources            `json:"resources,omitempty"`
	Warmup         []string              `json:"warmup,omitempty"`
	Mediation      []string              `json:"mediation,omitempty"`
	Hash           string                `json:"hash"`
}

type Services struct {
	Blocks   []StageBlock `json:"blocks,omitempty"`
	Command  []string     `json:"-"`
	KeyFiles []string     `json:"-"`
}

type Dependencies struct {
	Blocks   []StageBlock `json:"blocks,omitempty"`
	Command  []string     `json:"-"`
	KeyFiles []string     `json:"-"`
	// Reuse is currently not hash-covered because it is omitted from JSON. This
	// is safe only while bake rejects portable reuse; implementing portable
	// block-cache semantics must add reuse to the policy hash and bake key scheme.
	Reuse string `json:"-"`
}

type Run struct {
	Before []string `json:"before,omitempty"`
}

// NetworkStage names the sandbox lifecycle stage whose egress policy is active.
type NetworkStage string

const (
	NetworkStageWorkspace    NetworkStage = "workspace"
	NetworkStageDependencies NetworkStage = "dependencies"
	NetworkStageServices     NetworkStage = "services"
	NetworkStageExecution    NetworkStage = "execution"
)

// NetworkPolicy is the normalized network allowlist for one lifecycle stage.
type NetworkPolicy struct {
	Allow []AllowRule `json:"allow,omitempty"`
}

// NetworkStagePolicies contains the stage-local network policies declared in
// repository policy. Nil stage entries mean that stage has no configured egress.
type NetworkStagePolicies struct {
	Workspace    *NetworkPolicy `json:"workspace,omitempty"`
	Dependencies *NetworkPolicy `json:"dependencies,omitempty"`
	Services     *NetworkPolicy `json:"services,omitempty"`
	Execution    *NetworkPolicy `json:"execution,omitempty"`
}

type DockerService struct {
	Required bool `json:"required"`
}

type StageBlock struct {
	Name    string            `json:"name"`
	Command []string          `json:"command"`
	Inputs  StageBlockInputs  `json:"inputs"`
	Env     map[string]string `json:"env,omitempty"`
	Outputs StageBlockOutputs `json:"outputs"`
}

type StageBlockInputs struct {
	Files []string `json:"files,omitempty"`
}

type StageBlockOutputs struct {
	Dirs  []string `json:"dirs,omitempty"`
	Files []string `json:"files,omitempty"`
}

// Resources declares backend-neutral workload floors. The control plane raises
// backend launch settings to satisfy these values but preserves larger runtime
// defaults and leaves the exact allocation strategy to each backend.
type Resources struct {
	VCPUs       int64 `json:"vcpus,omitempty"`
	MemoryBytes int64 `json:"memory_bytes,omitempty"`
	DiskBytes   int64 `json:"disk_bytes,omitempty"`
}

func (r Resources) IsZero() bool {
	return r.VCPUs == 0 && r.MemoryBytes == 0 && r.DiskBytes == 0
}

type AllowRule struct {
	Host  string `json:"host"`
	Ports []int  `json:"ports"`
}

func normalizeRawAllowRules(raw rawAllowRules) ([]AllowRule, error) {
	allow := make([]AllowRule, 0, len(raw))
	for _, rule := range raw {
		host, err := normalizeAllowRuleHost(rule.Host)
		if err != nil {
			return nil, err
		}
		if len(rule.Ports) == 0 {
			return nil, fmt.Errorf("allow rule for host %q must include at least one port", host)
		}

		ports := make([]int, 0, len(rule.Ports))
		seen := map[int]struct{}{}
		for _, port := range rule.Ports {
			if port < 1 || port > 65535 {
				return nil, fmt.Errorf("allow rule for host %q contains invalid port %d", host, port)
			}
			if _, ok := seen[port]; ok {
				continue
			}
			seen[port] = struct{}{}
			ports = append(ports, port)
		}
		sort.Ints(ports)
		allow = append(allow, AllowRule{Host: host, Ports: ports})
	}

	sort.Slice(allow, func(i, j int) bool {
		return allow[i].Host < allow[j].Host
	})
	return allow, nil
}

func normalizeAllowRuleHost(rawHost string) (string, error) {
	trimmed := strings.TrimSpace(rawHost)
	if trimmed == "" {
		return "", errors.New("allow rule host cannot be empty")
	}
	for _, r := range trimmed {
		if r <= ' ' || r == 0x7f || r > 0x7e {
			return "", fmt.Errorf("allow rule host %q must be a bare hostname without control or non-ASCII characters", trimmed)
		}
	}
	host := strings.ToLower(trimmed)
	if net.ParseIP(host) != nil {
		return host, nil
	}
	if strings.ContainsAny(host, "/\\@[]:?&#%") {
		return "", fmt.Errorf("allow rule host %q must be a bare hostname without scheme, path, userinfo, or port", host)
	}
	return host, nil
}

func normalizeRawStageNetwork(raw *rawStageNetworkConfig) (*NetworkPolicy, error) {
	if raw == nil {
		return nil, nil
	}
	allow, err := normalizeRawAllowRules(raw.Allow)
	if err != nil {
		return nil, err
	}
	return &NetworkPolicy{Allow: allow}, nil
}

func normalizeRawNetworkStages(raw rawPolicy) (*NetworkStagePolicies, error) {
	var out NetworkStagePolicies
	var err error
	out.Dependencies, err = normalizeRawStageNetwork(raw.Sandbox.Network.Dependencies)
	if err != nil {
		return nil, err
	}
	out.Services, err = normalizeRawStageNetwork(raw.Sandbox.Network.Services)
	if err != nil {
		return nil, err
	}
	out.Execution, err = normalizeRawStageNetwork(raw.Sandbox.Network.Execution)
	if err != nil {
		return nil, err
	}
	if !out.HasAny() {
		return nil, nil
	}
	return &out, nil
}
func (l Loader) LoadAndCompile(root string) (*CompiledPolicy, string, error) {
	raw, source, err := l.Load(root)
	if err != nil {
		return nil, "", err
	}

	compiled, err := Compile(raw)
	if err != nil {
		return nil, source, err
	}

	return compiled, source, nil
}
func (l Loader) Load(root string) (rawPolicy, string, error) {
	primary := filepath.Join(root, PrimaryPolicyPath)
	fallback := filepath.Join(root, FallbackPolicyPath)

	primaryExists, err := exists(primary)
	if err != nil {
		return rawPolicy{}, "", fmt.Errorf("check policy %s: %w", primary, err)
	}
	if primaryExists {
		p, err := readPolicy(primary)
		return p, primary, err
	}

	fallbackExists, err := exists(fallback)
	if err != nil {
		return rawPolicy{}, "", fmt.Errorf("check policy %s: %w", fallback, err)
	}
	if fallbackExists {
		p, err := readPolicy(fallback)
		return p, fallback, err
	}

	return rawPolicy{}, "", fmt.Errorf("%w: expected %s or %s", ErrPolicyNotFound, primary, fallback)
}

func Compile(raw rawPolicy) (*CompiledPolicy, error) {
	if raw.Version == 0 {
		return nil, errors.New("policy missing required field: version")
	}
	if raw.Version != 1 {
		return nil, fmt.Errorf("unsupported policy version %d: only version 1 is supported", raw.Version)
	}

	imageRef := strings.TrimSpace(raw.Sandbox.Image.Ref)
	if imageRef == "" {
		return nil, errors.New("policy missing required field: sandbox.image.ref")
	}
	parsedRef, err := ociref.ParseDigestReference(imageRef)
	if err != nil {
		return nil, fmt.Errorf("invalid sandbox.image.ref: %w", err)
	}

	networkDefault := strings.TrimSpace(strings.ToLower(raw.Sandbox.Network.Default))
	if networkDefault == "" {
		networkDefault = "deny"
	}
	if networkDefault != "deny" {
		return nil, fmt.Errorf("unsupported sandbox.network.default %q: cleanroom requires deny-by-default", networkDefault)
	}

	allow, err := normalizeRawAllowRules(raw.Sandbox.Network.Allow)
	if err != nil {
		return nil, err
	}
	networkStages, err := normalizeRawNetworkStages(raw)
	if err != nil {
		return nil, err
	}
	if networkStages != nil && len(allow) > 0 {
		return nil, errors.New("sandbox.network.allow cannot be combined with stage-local network blocks")
	}

	docker := normalizeDocker(raw.Sandbox.Docker)
	dependencies, err := normalizeDependencies(raw.Sandbox.Dependencies, defaultBlockWorkspace)
	if err != nil {
		return nil, err
	}
	services, err := normalizeServices(raw.Sandbox.Services, defaultBlockWorkspace)
	if err != nil {
		return nil, err
	}
	if err := validatePolicyOutputRelationships(dependencies.Blocks, services.Blocks); err != nil {
		return nil, err
	}
	run, err := normalizeRun(raw.Sandbox.Run)
	if err != nil {
		return nil, err
	}
	resources, err := normalizeResources(raw.Sandbox.Resources, "sandbox.resources")
	if err != nil {
		return nil, err
	}
	warmup, err := normalizeWarmup(raw.Sandbox.Warmup)
	if err != nil {
		return nil, err
	}
	effectiveWarmup := lowerEffectiveWarmup(warmup, dependencies.Blocks, services.Blocks)
	mediation, err := normalizeMediation(raw.Sandbox.Mediation)
	if err != nil {
		return nil, err
	}

	compiled := &CompiledPolicy{
		Version:        raw.Version,
		ImageRef:       parsedRef.Original,
		ImageDigest:    parsedRef.Digest(),
		Docker:         docker,
		Services:       services,
		NetworkDefault: networkDefault,
		Allow:          allow,
		NetworkStages:  networkStages,
		Dependencies:   dependencies,
		Run:            run,
		Resources:      resources,
		Warmup:         effectiveWarmup,
		Mediation:      mediation,
	}

	hash, err := hashPolicy(compiled)
	if err != nil {
		return nil, err
	}
	compiled.Hash = hash

	return compiled, nil
}

func (p *CompiledPolicy) Allows(host string, port int) bool {
	if p == nil {
		return false
	}
	if strings.TrimSpace(strings.ToLower(p.NetworkDefault)) == "allow" {
		return true
	}
	host = strings.TrimSpace(strings.ToLower(host))
	for _, rule := range p.Allow {
		if rule.Host != host {
			continue
		}
		for _, candidate := range rule.Ports {
			if candidate == port {
				return true
			}
		}
	}
	return false
}

// HasStageScopedNetwork reports whether any stage-local network block was
// configured in repository policy.
func (p *CompiledPolicy) HasStageScopedNetwork() bool {
	return p != nil && p.NetworkStages != nil && p.NetworkStages.HasAny()
}

// NetworkPolicyForStage returns a copy of p with the active stage allowlist.
// Policies without stage-local network config retain the legacy global allowlist.
func (p *CompiledPolicy) NetworkPolicyForStage(stage NetworkStage) *CompiledPolicy {
	if p == nil {
		return nil
	}
	effective := *p
	effective.Allow = cloneAllowRules(p.Allow)
	effective.NetworkStages = nil
	if !p.HasStageScopedNetwork() {
		return &effective
	}

	effective.NetworkDefault = "deny"
	effective.Allow = nil
	if stagePolicy := p.NetworkStages.ForStage(normalizeNetworkStage(stage)); stagePolicy != nil {
		effective.Allow = cloneAllowRules(stagePolicy.Allow)
	}
	if hash, err := hashPolicy(&effective); err == nil {
		effective.Hash = hash
	} else {
		effective.Hash = ""
	}
	return &effective
}

// AllowsForStage applies the effective network policy for stage.
func (p *CompiledPolicy) AllowsForStage(stage NetworkStage, host string, port int) bool {
	return p.NetworkPolicyForStage(stage).Allows(host, port)
}

// HasAny reports whether any stage policy is configured.
func (p *NetworkStagePolicies) HasAny() bool {
	return p != nil && (p.Workspace != nil || p.Dependencies != nil || p.Services != nil || p.Execution != nil)
}

// ForStage returns the policy configured for stage.
func (p *NetworkStagePolicies) ForStage(stage NetworkStage) *NetworkPolicy {
	if p == nil {
		return nil
	}
	switch normalizeNetworkStage(stage) {
	case NetworkStageWorkspace:
		return p.Workspace
	case NetworkStageDependencies:
		return p.Dependencies
	case NetworkStageServices:
		return p.Services
	default:
		return p.Execution
	}
}

func normalizeNetworkStage(stage NetworkStage) NetworkStage {
	switch stage {
	case NetworkStageWorkspace, NetworkStageDependencies, NetworkStageServices, NetworkStageExecution:
		return stage
	default:
		return NetworkStageExecution
	}
}

func cloneAllowRules(in []AllowRule) []AllowRule {
	if len(in) == 0 {
		return nil
	}
	out := make([]AllowRule, 0, len(in))
	for _, rule := range in {
		out = append(out, AllowRule{
			Host:  rule.Host,
			Ports: append([]int(nil), rule.Ports...),
		})
	}
	return out
}

// HostAllowed returns true when at least one allow rule references the host,
// regardless of port.
func (p *CompiledPolicy) HostAllowed(host string) bool {
	if p == nil {
		return false
	}
	if p.NetworkDefault == "allow" {
		return true
	}
	for _, rule := range p.Allow {
		if rule.Host == host {
			return true
		}
	}
	return false
}

func (p *CompiledPolicy) RequiresDockerService() bool {
	if p == nil {
		return false
	}
	return p.Docker.Required
}

func (s Services) BootstrapEnabled() bool {
	return len(s.Blocks) > 0 || len(s.Command) > 0
}

func (d Dependencies) Enabled() bool {
	return len(d.Blocks) > 0 || len(d.Command) > 0
}

const (
	DependencyReuseExact    = "exact"
	DependencyReusePortable = "portable"
)

func (r Run) HasBefore() bool {
	return len(r.Before) > 0
}

func (c *rawShellCommandSpec) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		*c = nil
		return nil
	}
	node = dereferenceYAMLAlias(node)
	if node.Kind != yaml.ScalarNode || node.ShortTag() != "!!str" {
		return fmt.Errorf("command must be a string")
	}
	script := strings.TrimSpace(node.Value)
	*c = rawShellCommandSpec{"sh", "-lc", script}
	return nil
}

func (c *rawDependencyCommandSpec) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		*c = nil
		return nil
	}
	node = dereferenceYAMLAlias(node)
	switch node.Kind {
	case yaml.ScalarNode:
		if node.ShortTag() == "!!null" {
			*c = nil
			return nil
		}
		if node.ShortTag() != "!!str" {
			return fmt.Errorf("command must be a string or sequence")
		}
		script := strings.TrimSpace(node.Value)
		*c = rawDependencyCommandSpec{"sh", "-lc", script}
		return nil
	case yaml.SequenceNode:
		var command []string
		if err := node.Decode(&command); err != nil {
			return err
		}
		*c = rawDependencyCommandSpec(command)
		return nil
	default:
		return fmt.Errorf("command must be a string or sequence")
	}
}

func (stage *rawDependencyStage) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		return nil
	}
	node = dereferenceYAMLAlias(node)
	if node.Kind == yaml.ScalarNode && node.ShortTag() == "!!null" {
		return nil
	}
	switch node.Kind {
	case yaml.SequenceNode:
		var blocks rawPolicyBlocks
		if err := node.Decode(&blocks); err != nil {
			return err
		}
		stage.Blocks = blocks
		stage.blocksField = "sandbox.dependencies"
		return nil
	case yaml.MappingNode:
		stage.blocksField = "sandbox.dependencies.blocks"
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			value := node.Content[i+1]
			switch key.Value {
			case "reuse":
				if err := value.Decode(&stage.Reuse); err != nil {
					return err
				}
			case "blocks":
				if err := value.Decode(&stage.Blocks); err != nil {
					return err
				}
			default:
				return fmt.Errorf("sandbox.dependencies.%s is not supported; use sandbox.dependencies.blocks", key.Value)
			}
		}
		return nil
	default:
		return fmt.Errorf("sandbox.dependencies must be a list of blocks or an object with reuse and blocks")
	}
}

func (rules *rawAllowRules) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		*rules = nil
		return nil
	}
	node = dereferenceYAMLAlias(node)
	if node.Kind == yaml.ScalarNode && node.ShortTag() == "!!null" {
		*rules = nil
		return nil
	}

	switch node.Kind {
	case yaml.ScalarNode:
		var rule rawAllowRule
		if err := rule.UnmarshalYAML(node); err != nil {
			return err
		}
		*rules = rawAllowRules{rule}
		return nil
	case yaml.SequenceNode:
		out := make(rawAllowRules, 0, len(node.Content))
		for _, item := range node.Content {
			var rule rawAllowRule
			if err := rule.UnmarshalYAML(item); err != nil {
				return err
			}
			out = append(out, rule)
		}
		*rules = out
		return nil
	default:
		return fmt.Errorf("sandbox.network.allow must be a string or sequence")
	}
}

func (rule *rawAllowRule) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		*rule = rawAllowRule{}
		return nil
	}
	node = dereferenceYAMLAlias(node)

	switch node.Kind {
	case yaml.ScalarNode:
		if node.ShortTag() != "!!str" {
			return fmt.Errorf("network allow entry must be a host:port string or mapping")
		}
		parsed, err := parseAllowRuleShorthand(node.Value)
		if err != nil {
			return err
		}
		*rule = parsed
		return nil
	case yaml.MappingNode:
		var out rawAllowRule
		for i := 0; i < len(node.Content); i += 2 {
			keyNode := dereferenceYAMLAlias(node.Content[i])
			valueNode := dereferenceYAMLAlias(node.Content[i+1])
			if keyNode == nil || keyNode.Kind != yaml.ScalarNode || keyNode.ShortTag() != "!!str" {
				return fmt.Errorf("network allow mapping keys must be strings")
			}
			switch keyNode.Value {
			case "host":
				if err := valueNode.Decode(&out.Host); err != nil {
					return err
				}
			case "ports":
				if err := valueNode.Decode(&out.Ports); err != nil {
					return err
				}
			default:
				return fmt.Errorf("unknown network allow field %q", keyNode.Value)
			}
		}
		*rule = out
		return nil
	default:
		return fmt.Errorf("network allow entry must be a host:port string or mapping")
	}
}

func parseAllowRuleShorthand(value string) (rawAllowRule, error) {
	entry := strings.TrimSpace(value)
	if entry == "" {
		return rawAllowRule{}, errors.New("network allow shorthand cannot be empty")
	}
	if strings.Contains(entry, "://") {
		return rawAllowRule{}, fmt.Errorf("network allow shorthand %q must be host:port, not a URL", entry)
	}

	host, portText, err := net.SplitHostPort(entry)
	if err != nil {
		return rawAllowRule{}, fmt.Errorf("network allow shorthand %q must be host:port", entry)
	}
	host = strings.TrimSpace(host)
	portText = strings.TrimSpace(portText)
	if host == "" {
		return rawAllowRule{}, fmt.Errorf("network allow shorthand %q must include a host", entry)
	}
	if strings.Contains(host, "/") {
		return rawAllowRule{}, fmt.Errorf("network allow shorthand %q must be host:port, not a URL or path", entry)
	}
	if strings.Contains(host, ":") {
		return rawAllowRule{}, errors.New("network allow shorthand does not support IPv6 literals; use host and ports")
	}

	port, err := strconv.Atoi(portText)
	if err != nil {
		return rawAllowRule{}, fmt.Errorf("network allow shorthand %q contains invalid port %q", entry, portText)
	}
	if port < 1 || port > 65535 {
		return rawAllowRule{}, fmt.Errorf("network allow shorthand %q contains invalid port %d", entry, port)
	}

	return rawAllowRule{
		Host:  host,
		Ports: []int{port},
	}, nil
}

func dereferenceYAMLAlias(node *yaml.Node) *yaml.Node {
	for node != nil && node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	return node
}

func readPolicy(path string) (rawPolicy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return rawPolicy{}, err
	}

	return parsePolicyBytes(b, path)
}

func parsePolicyBytes(b []byte, source string) (rawPolicy, error) {
	var raw rawPolicy
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		if !errors.Is(err, io.EOF) {
			return rawPolicy{}, fmt.Errorf("parse %s: %w", source, err)
		}
	}

	return raw, nil
}

// CompileBytes parses and compiles policy YAML loaded from source.
func CompileBytes(b []byte, source string) (*CompiledPolicy, error) {
	raw, err := parsePolicyBytes(b, source)
	if err != nil {
		return nil, err
	}
	return Compile(raw)
}

func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// DangerouslyAllowAllEgress returns a copy of compiled with outbound network
// filtering disabled. This is intentionally separate so callers cannot smuggle allow-all egress
// through the policy payload.
func DangerouslyAllowAllEgress(compiled *CompiledPolicy) (*CompiledPolicy, error) {
	if compiled == nil {
		return nil, errors.New("missing policy")
	}
	out := *compiled
	out.NetworkDefault = "allow"
	out.Allow = nil
	out.NetworkStages = nil
	hash, err := hashPolicy(&out)
	if err != nil {
		return nil, err
	}
	out.Hash = hash
	return &out, nil
}

func normalizeDocker(raw rawDockerConfig) DockerService {
	return DockerService{Required: raw.Required}
}

func normalizeDependencies(raw rawDependencyStage, workspaceRoot string) (Dependencies, error) {
	blocksField := raw.blocksField
	if blocksField == "" {
		blocksField = "sandbox.dependencies"
	}
	blocks, err := normalizeStageBlocks(raw.Blocks, blocksField, workspaceRoot)
	if err != nil {
		return Dependencies{}, err
	}
	command := combinedStageBlockCommand(blocks)
	keyFiles := combinedStageBlockInputFiles(blocks)
	reuse, err := normalizeDependencyReuse(raw.Reuse, keyFiles, "sandbox.dependencies.reuse")
	if err != nil {
		return Dependencies{}, err
	}
	return Dependencies{
		Blocks:   blocks,
		Command:  command,
		KeyFiles: keyFiles,
		Reuse:    reuse,
	}, nil
}

func normalizeDependencyReuse(raw string, keyFiles []string, field string) (string, error) {
	reuse := strings.TrimSpace(strings.ToLower(raw))
	switch reuse {
	case "", DependencyReuseExact:
		return "", nil
	case DependencyReusePortable:
		if len(keyFiles) == 0 {
			return "", fmt.Errorf("%s=portable requires key files", field)
		}
		return reuse, nil
	default:
		return "", fmt.Errorf("unsupported %s %q: expected %q", field, raw, DependencyReusePortable)
	}
}

func normalizeServices(raw rawPolicyBlocks, workspaceRoot string) (Services, error) {
	blocks, err := normalizeStageBlocks(raw, "sandbox.services", workspaceRoot)
	if err != nil {
		return Services{}, err
	}
	return Services{
		Blocks:   blocks,
		Command:  combinedStageBlockCommand(blocks),
		KeyFiles: combinedStageBlockInputFiles(blocks),
	}, nil
}

func normalizeRun(raw rawRunConfig) (Run, error) {
	before, err := normalizeShellCommand(raw.Before, "sandbox.run.before")
	if err != nil {
		return Run{}, err
	}
	return Run{Before: before}, nil
}

func normalizeResources(raw rawResources, field string) (*Resources, error) {
	var resources Resources
	if raw.VCPUs != nil {
		if *raw.VCPUs <= 0 {
			return nil, fmt.Errorf("%s.vcpus must be positive", field)
		}
		resources.VCPUs = *raw.VCPUs
	}
	if raw.Memory != nil {
		if *raw.Memory <= 0 {
			return nil, fmt.Errorf("%s.memory must be positive", field)
		}
		resources.MemoryBytes = int64(*raw.Memory)
	}
	if raw.Disk != nil {
		if *raw.Disk <= 0 {
			return nil, fmt.Errorf("%s.disk must be positive", field)
		}
		resources.DiskBytes = int64(*raw.Disk)
	}
	if resources.IsZero() {
		return nil, nil
	}
	return &resources, nil
}

func normalizeShellCommand(raw []string, field string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	command := make([]string, 0, len(raw))
	for i, arg := range raw {
		trimmed := strings.TrimSpace(arg)
		if trimmed == "" {
			return nil, fmt.Errorf("%s[%d] cannot be empty", field, i)
		}
		command = append(command, trimmed)
	}
	return command, nil
}

func normalizeBootstrapCommand(raw []string, field string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	command := make([]string, 0, len(raw))
	for i, arg := range raw {
		trimmed := strings.TrimSpace(arg)
		if trimmed == "" {
			return nil, fmt.Errorf("%s[%d] cannot be empty", field, i)
		}
		command = append(command, trimmed)
	}
	return command, nil
}

func normalizeBootstrapKeyFiles(raw []string, field string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(raw))
	files := make([]string, 0, len(raw))
	for i, candidate := range raw {
		trimmed := strings.TrimSpace(candidate)
		if trimmed == "" {
			return nil, fmt.Errorf("%s[%d] cannot be empty", field, i)
		}
		if strings.HasPrefix(trimmed, "/") {
			return nil, fmt.Errorf("%s[%d] must be relative", field, i)
		}
		cleaned := path.Clean(strings.ReplaceAll(trimmed, "\\", "/"))
		if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
			return nil, fmt.Errorf("%s[%d] must stay within the repository root", field, i)
		}
		if _, ok := seen[cleaned]; ok {
			continue
		}
		seen[cleaned] = struct{}{}
		files = append(files, cleaned)
	}
	sort.Strings(files)
	return files, nil
}

const (
	defaultBlockHome      = guestenv.DefaultHome
	defaultBlockWorkspace = "/workspace"
)

func normalizeStageBlocks(raw []rawPolicyBlock, field string, workspaceRoot string) ([]StageBlock, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	workspaceRoot = normalizeBlockWorkspaceRoot(workspaceRoot)
	seenNames := make(map[string]struct{}, len(raw))
	blocks := make([]StageBlock, 0, len(raw))
	for i, candidate := range raw {
		block, err := normalizeStageBlock(candidate, fmt.Sprintf("%s[%d]", field, i), workspaceRoot)
		if err != nil {
			return nil, err
		}
		if _, ok := seenNames[block.Name]; ok {
			return nil, fmt.Errorf("%s[%d].name %q is duplicated", field, i, block.Name)
		}
		seenNames[block.Name] = struct{}{}
		blocks = append(blocks, block)
	}
	if err := validateStageBlockOutputRelationships(blocks, field); err != nil {
		return nil, err
	}
	return blocks, nil
}

func normalizeStageBlock(raw rawPolicyBlock, field string, workspaceRoot string) (StageBlock, error) {
	name := strings.TrimSpace(raw.Name)
	if name == "" {
		return StageBlock{}, fmt.Errorf("%s.name is required", field)
	}
	if !validStageBlockName(name) {
		return StageBlock{}, fmt.Errorf("%s.name %q must match [A-Za-z0-9][A-Za-z0-9_.-]*", field, name)
	}
	command, err := normalizeBootstrapCommand(raw.Command, field+".command")
	if err != nil {
		return StageBlock{}, err
	}
	if len(command) == 0 {
		return StageBlock{}, fmt.Errorf("%s.command is required", field)
	}
	inputFiles, err := normalizeBootstrapKeyFiles(raw.Inputs.Files, field+".inputs.files")
	if err != nil {
		return StageBlock{}, err
	}
	if len(inputFiles) == 0 {
		return StageBlock{}, fmt.Errorf("%s.inputs.files must include at least one file or glob", field)
	}
	env, err := normalizeBlockEnv(raw.Env, field+".env", workspaceRoot)
	if err != nil {
		return StageBlock{}, err
	}
	outputs, err := normalizeBlockOutputs(raw.Outputs, field+".outputs", workspaceRoot)
	if err != nil {
		return StageBlock{}, err
	}
	if len(outputs.Dirs) == 0 && len(outputs.Files) == 0 {
		return StageBlock{}, fmt.Errorf("%s.outputs must include at least one dir or file", field)
	}
	return StageBlock{
		Name:    name,
		Command: command,
		Inputs:  StageBlockInputs{Files: inputFiles},
		Env:     env,
		Outputs: outputs,
	}, nil
}

func validStageBlockName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			continue
		}
		if i > 0 && (r == '_' || r == '.' || r == '-') {
			continue
		}
		return false
	}
	return true
}

func normalizeBlockEnv(raw map[string]string, field string, workspaceRoot string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	workspaceRoot = normalizeBlockWorkspaceRoot(workspaceRoot)
	env := make(map[string]string, len(raw))
	for key, value := range raw {
		name := strings.TrimSpace(key)
		if name == "" {
			return nil, fmt.Errorf("%s contains an empty key", field)
		}
		if !validEnvName(name) {
			return nil, fmt.Errorf("%s.%s must be a valid environment variable name", field, name)
		}
		env[name] = expandGuestEnvValue(value, workspaceRoot)
	}
	return env, nil
}

func expandGuestEnvValue(value string, workspaceRoot string) string {
	workspaceRoot = normalizeBlockWorkspaceRoot(workspaceRoot)
	switch {
	case value == "~":
		return defaultBlockHome
	case strings.HasPrefix(value, "~/"):
		return defaultBlockHome + value[1:]
	case value == "$HOME":
		return defaultBlockHome
	case strings.HasPrefix(value, "$HOME/"):
		return defaultBlockHome + strings.TrimPrefix(value, "$HOME")
	case value == "${HOME}":
		return defaultBlockHome
	case strings.HasPrefix(value, "${HOME}/"):
		return defaultBlockHome + strings.TrimPrefix(value, "${HOME}")
	case value == "$WORKSPACE":
		return workspaceRoot
	case strings.HasPrefix(value, "$WORKSPACE/"):
		return workspaceRoot + strings.TrimPrefix(value, "$WORKSPACE")
	case value == "${WORKSPACE}":
		return workspaceRoot
	case strings.HasPrefix(value, "${WORKSPACE}/"):
		return workspaceRoot + strings.TrimPrefix(value, "${WORKSPACE}")
	default:
		return value
	}
}

func validEnvName(name string) bool {
	for i, r := range name {
		if i == 0 {
			if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '_' {
				continue
			}
			return false
		}
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			continue
		}
		return false
	}
	return name != ""
}

func normalizeBlockOutputs(raw rawPolicyBlockOutputs, field string, workspaceRoot string) (StageBlockOutputs, error) {
	workspaceRoot = normalizeBlockWorkspaceRoot(workspaceRoot)
	dirs, err := normalizeOutputPaths(raw.Dirs, field+".dirs", workspaceRoot)
	if err != nil {
		return StageBlockOutputs{}, err
	}
	files, err := normalizeOutputPaths(raw.Files, field+".files", workspaceRoot)
	if err != nil {
		return StageBlockOutputs{}, err
	}
	if err := validateOutputPathRelationships(dirs, files, field); err != nil {
		return StageBlockOutputs{}, err
	}
	return StageBlockOutputs{Dirs: dirs, Files: files}, nil
}

func normalizeOutputPaths(raw []string, field string, workspaceRoot string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	workspaceRoot = normalizeBlockWorkspaceRoot(workspaceRoot)
	seen := make(map[string]struct{}, len(raw))
	paths := make([]string, 0, len(raw))
	for i, candidate := range raw {
		normalized, err := expandGuestPathValue(strings.TrimSpace(candidate), fmt.Sprintf("%s[%d]", field, i), workspaceRoot, true)
		if err != nil {
			return nil, err
		}
		if normalized == "/" {
			return nil, fmt.Errorf("%s[%d] must not be /", field, i)
		}
		if normalized == workspaceRoot {
			return nil, fmt.Errorf("%s[%d] must not be the repository root", field, i)
		}
		if _, ok := seen[normalized]; ok {
			return nil, fmt.Errorf("%s[%d] duplicates output path %q", field, i, normalized)
		}
		seen[normalized] = struct{}{}
		paths = append(paths, normalized)
	}
	sort.Strings(paths)
	return paths, nil
}

func expandGuestPathValue(value, field string, workspaceRoot string, requireAbsolute bool) (string, error) {
	workspaceRoot = normalizeBlockWorkspaceRoot(workspaceRoot)
	if value == "" {
		return "", fmt.Errorf("%s cannot be empty", field)
	}
	workspaceAnchored := false
	if value == "~" {
		value = defaultBlockHome
	} else if strings.HasPrefix(value, "~/") {
		value = defaultBlockHome + value[1:]
	} else if strings.HasPrefix(value, "~") {
		return "", fmt.Errorf("%s does not support ~user expansion", field)
	} else if value == "$HOME" {
		value = defaultBlockHome
	} else if strings.HasPrefix(value, "$HOME/") {
		value = defaultBlockHome + strings.TrimPrefix(value, "$HOME")
	} else if value == "${HOME}" {
		value = defaultBlockHome
	} else if strings.HasPrefix(value, "${HOME}/") {
		value = defaultBlockHome + strings.TrimPrefix(value, "${HOME}")
	} else if value == "$WORKSPACE" {
		value = workspaceRoot
		workspaceAnchored = true
	} else if strings.HasPrefix(value, "$WORKSPACE/") {
		value = workspaceRoot + strings.TrimPrefix(value, "$WORKSPACE")
		workspaceAnchored = true
	} else if value == "${WORKSPACE}" {
		value = workspaceRoot
		workspaceAnchored = true
	} else if strings.HasPrefix(value, "${WORKSPACE}/") {
		value = workspaceRoot + strings.TrimPrefix(value, "${WORKSPACE}")
		workspaceAnchored = true
	}
	if strings.Contains(value, "$") {
		return "", fmt.Errorf("%s contains an unsupported variable expansion", field)
	}
	value = strings.ReplaceAll(value, "\\", "/")
	if strings.ContainsAny(value, "*?[") {
		return "", fmt.Errorf("%s must not contain glob characters", field)
	}
	if requireAbsolute && !strings.HasPrefix(value, "/") {
		relative := path.Clean(value)
		if relative == "." || relative == ".." || strings.HasPrefix(relative, "../") {
			return "", fmt.Errorf("%s must stay within the repository root", field)
		}
		value = path.Join(workspaceRoot, relative)
	} else {
		value = path.Clean(value)
	}
	if workspaceAnchored && !pathWithinOrEqual(workspaceRoot, value) {
		return "", fmt.Errorf("%s must stay within the repository root", field)
	}
	if requireAbsolute && !strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("%s must be absolute", field)
	}
	return value, nil
}

func normalizeBlockWorkspaceRoot(workspaceRoot string) string {
	workspaceRoot = strings.TrimSpace(workspaceRoot)
	if workspaceRoot == "" {
		return defaultBlockWorkspace
	}
	if !strings.HasPrefix(workspaceRoot, "/") {
		return defaultBlockWorkspace
	}
	return path.Clean(workspaceRoot)
}

func validateOutputPathRelationships(dirs, files []string, field string) error {
	for i, dir := range dirs {
		for j, other := range dirs {
			if i == j {
				continue
			}
			if pathContains(dir, other) {
				return fmt.Errorf("%s.dirs contains overlapping paths %q and %q", field, dir, other)
			}
		}
		for _, file := range files {
			if file == dir || pathContains(dir, file) {
				return fmt.Errorf("%s.files path %q is inside output dir %q", field, file, dir)
			}
		}
	}
	return nil
}

type stageBlockOutputPath struct {
	Block string
	Kind  string
	Path  string
}

func validateStageBlockOutputRelationships(blocks []StageBlock, field string) error {
	seen := make(map[string]stageBlockOutputPath)
	var dirs []stageBlockOutputPath
	var files []stageBlockOutputPath

	for _, block := range blocks {
		for _, dir := range block.Outputs.Dirs {
			ref := stageBlockOutputPath{Block: block.Name, Kind: "dir", Path: dir}
			if previous, ok := seen[dir]; ok {
				return fmt.Errorf("%s block %q output %s %q duplicates block %q output %s", field, block.Name, ref.Kind, ref.Path, previous.Block, previous.Kind)
			}
			seen[dir] = ref
			dirs = append(dirs, ref)
		}
		for _, file := range block.Outputs.Files {
			ref := stageBlockOutputPath{Block: block.Name, Kind: "file", Path: file}
			if previous, ok := seen[file]; ok {
				return fmt.Errorf("%s block %q output %s %q duplicates block %q output %s", field, block.Name, ref.Kind, ref.Path, previous.Block, previous.Kind)
			}
			seen[file] = ref
			files = append(files, ref)
		}
	}

	for i, dir := range dirs {
		for j, other := range dirs {
			if i == j {
				continue
			}
			if pathContains(dir.Path, other.Path) {
				return fmt.Errorf("%s block %q output dir %q overlaps block %q output dir %q", field, dir.Block, dir.Path, other.Block, other.Path)
			}
		}
		for _, file := range files {
			if pathContains(dir.Path, file.Path) {
				return fmt.Errorf("%s block %q output dir %q overlaps block %q output file %q", field, dir.Block, dir.Path, file.Block, file.Path)
			}
		}
	}
	return nil
}

func validatePolicyOutputRelationships(dependencyBlocks, serviceBlocks []StageBlock) error {
	if len(dependencyBlocks) == 0 || len(serviceBlocks) == 0 {
		return nil
	}
	blocks := make([]StageBlock, 0, len(dependencyBlocks)+len(serviceBlocks))
	blocks = appendLabeledStageBlocks(blocks, "dependencies", dependencyBlocks)
	blocks = appendLabeledStageBlocks(blocks, "services", serviceBlocks)
	return validateStageBlockOutputRelationships(blocks, "sandbox")
}

func appendLabeledStageBlocks(dst []StageBlock, stage string, blocks []StageBlock) []StageBlock {
	for _, block := range blocks {
		candidate := block
		candidate.Name = stage + "." + block.Name
		dst = append(dst, candidate)
	}
	return dst
}

func pathContains(parent, child string) bool {
	parent = path.Clean(parent)
	child = path.Clean(child)
	if parent == "/" {
		return child != "/"
	}
	return strings.HasPrefix(child, parent+"/")
}

func pathWithinOrEqual(parent, child string) bool {
	parent = path.Clean(parent)
	child = path.Clean(child)
	return child == parent || pathContains(parent, child)
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func combinedStageBlockInputFiles(blocks []StageBlock) []string {
	seen := make(map[string]struct{})
	var files []string
	for _, block := range blocks {
		for _, file := range block.Inputs.Files {
			if _, ok := seen[file]; ok {
				continue
			}
			seen[file] = struct{}{}
			files = append(files, file)
		}
	}
	sort.Strings(files)
	return files
}

func combinedStageBlockCommand(blocks []StageBlock) []string {
	if len(blocks) == 0 {
		return nil
	}
	var script strings.Builder
	script.WriteString("set -eu\n")
	for _, block := range blocks {
		script.WriteString("(\n")
		if len(block.Env) > 0 {
			keys := make([]string, 0, len(block.Env))
			for key := range block.Env {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				script.WriteString("export ")
				script.WriteString(key)
				script.WriteString("=")
				script.WriteString(shellQuote(block.Env[key]))
				script.WriteString("\n")
			}
		}
		script.WriteString(commandShellLine(block.Command))
		script.WriteString("\n")
		script.WriteString(")\n")
	}
	return []string{"sh", "-lc", script.String()}
}

func lowerEffectiveWarmup(explicit []string, dependencyBlocks, serviceBlocks []StageBlock) []string {
	if len(explicit) == 0 && len(dependencyBlocks) == 0 && len(serviceBlocks) == 0 {
		return nil
	}
	steps := make([]string, 0, len(explicit)+len(dependencyBlocks)+len(serviceBlocks))
	steps = append(steps, explicit...)
	steps = lowerStageBlocksIntoWarmup(steps, dependencyBlocks)
	steps = lowerStageBlocksIntoWarmup(steps, serviceBlocks)
	return steps
}

func lowerStageBlocksIntoWarmup(steps []string, blocks []StageBlock) []string {
	for _, block := range blocks {
		steps = append(steps, stageBlockWarmupStep(block))
	}
	return steps
}

func stageBlockWarmupStep(block StageBlock) string {
	parts := make([]string, 0, len(block.Env)+1)
	if len(block.Env) > 0 {
		keys := make([]string, 0, len(block.Env))
		for key := range block.Env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			parts = append(parts, key+"="+shellQuote(block.Env[key]))
		}
	}
	parts = append(parts, commandShellLine(block.Command))
	return strings.Join(parts, " ")
}

func commandShellLine(command []string) string {
	parts := make([]string, 0, len(command))
	for _, arg := range command {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func normalizeWarmup(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	steps := make([]string, 0, len(raw))
	for i, step := range raw {
		trimmed := strings.TrimSpace(step)
		if trimmed == "" {
			return nil, fmt.Errorf("sandbox.warmup entry %d is empty", i)
		}
		steps = append(steps, trimmed)
	}
	return steps, nil
}

func normalizeMediation(raw rawMediation) ([]string, error) {
	if len(raw.Services) == 0 {
		return nil, nil
	}
	services := make([]string, 0, len(raw.Services))
	seen := map[string]bool{}
	for i, service := range raw.Services {
		name := strings.TrimSpace(service)
		if name == "" {
			return nil, fmt.Errorf("sandbox.mediation.services entry %d is empty", i)
		}
		if !isMediationServiceName(name) {
			return nil, fmt.Errorf("sandbox.mediation.services entry %d has invalid name %q: use letters, digits, '.', '_', '-'", i, name)
		}
		if seen[name] {
			return nil, fmt.Errorf("sandbox.mediation.services entry %d duplicates %q", i, name)
		}
		seen[name] = true
		services = append(services, name)
	}
	return services, nil
}

func isMediationServiceName(name string) bool {
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return name != ""
}

func hashPolicy(p *CompiledPolicy) (string, error) {
	clone := *p
	clone.Hash = ""

	payload, err := json.Marshal(clone)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}
