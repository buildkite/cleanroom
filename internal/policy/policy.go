package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
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
	Version    int            `yaml:"version"`
	Repository *rawRepository `yaml:"repository"`
	Sandbox    struct {
		Image struct {
			Ref string `yaml:"ref"`
		} `yaml:"image"`
		Dependencies rawDependenciesConfig `yaml:"dependencies"`
		Services     rawServices           `yaml:"services"`
		Network      struct {
			Default string         `yaml:"default"`
			Allow   []rawAllowRule `yaml:"allow"`
		} `yaml:"network"`
	} `yaml:"sandbox"`
}

type rawRepository struct {
	Enabled    *bool  `yaml:"enabled"`
	Mode       string `yaml:"mode"`
	Remote     string `yaml:"remote"`
	Path       string `yaml:"path"`
	Submodules bool   `yaml:"submodules"`
}

type rawDependencyKey struct {
	Files []string `yaml:"files"`
}

type rawDependenciesConfig struct {
	Command []string         `yaml:"command"`
	Key     rawDependencyKey `yaml:"key"`
}

type rawServices struct {
	Docker rawDockerService `yaml:"docker"`
}

type rawDockerService struct {
	Required bool `yaml:"required"`
}

type rawAllowRule struct {
	Host  string `yaml:"host"`
	Ports []int  `yaml:"ports"`
}

type CompiledPolicy struct {
	Version        int          `json:"version"`
	ImageRef       string       `json:"image_ref"`
	ImageDigest    string       `json:"image_digest"`
	Services       Services     `json:"services"`
	NetworkDefault string       `json:"network_default"`
	Allow          []AllowRule  `json:"allow"`
	Dependencies   Dependencies `json:"dependencies"`
	Hash           string       `json:"hash"`
}

type RepositoryConfig struct {
	Implicit   bool   `json:"-"`
	Mode       string `json:"mode"`
	Remote     string `json:"remote"`
	Path       string `json:"path"`
	Submodules bool   `json:"submodules"`
}

type Services struct {
	Docker DockerService `json:"docker"`
}

type Dependencies struct {
	Command  []string `json:"command,omitempty"`
	KeyFiles []string `json:"key_files,omitempty"`
}

type DockerService struct {
	Required bool `json:"required"`
}

type AllowRule struct {
	Host  string `json:"host"`
	Ports []int  `json:"ports"`
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

func (l Loader) LoadRepository(root string) (RepositoryConfig, string, error) {
	raw, source, err := l.Load(root)
	if err != nil {
		return RepositoryConfig{}, "", err
	}

	cfg, err := normalizeRepositoryConfig(raw.Repository)
	if err != nil {
		return RepositoryConfig{}, source, err
	}
	return cfg, source, nil
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

	allow := make([]AllowRule, 0, len(raw.Sandbox.Network.Allow))
	for _, rule := range raw.Sandbox.Network.Allow {
		host := strings.TrimSpace(strings.ToLower(rule.Host))
		if host == "" {
			return nil, errors.New("allow rule host cannot be empty")
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

	dependencies, err := normalizeDependencies(raw.Sandbox.Dependencies)
	if err != nil {
		return nil, err
	}

	compiled := &CompiledPolicy{
		Version:     raw.Version,
		ImageRef:    parsedRef.Original,
		ImageDigest: parsedRef.Digest(),
		Services: Services{
			Docker: DockerService{
				Required: raw.Sandbox.Services.Docker.Required,
			},
		},
		NetworkDefault: networkDefault,
		Allow:          allow,
		Dependencies:   dependencies,
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
	return p.Services.Docker.Required
}

func (d Dependencies) Enabled() bool {
	return len(d.Command) > 0
}

func (c RepositoryConfig) Enabled() bool {
	return strings.TrimSpace(strings.ToLower(c.Mode)) != "" && strings.TrimSpace(strings.ToLower(c.Mode)) != "none"
}

func normalizeRepositoryConfig(raw *rawRepository) (RepositoryConfig, error) {
	if raw == nil {
		return RepositoryConfig{
			Implicit: true,
			Mode:     "current-repo",
			Remote:   "origin",
			Path:     "/workspace",
		}, nil
	}
	if raw.Enabled != nil && !*raw.Enabled {
		return RepositoryConfig{}, nil
	}

	mode := strings.TrimSpace(strings.ToLower(raw.Mode))
	switch mode {
	case "", "current-repo":
		mode = "current-repo"
	case "none":
		return RepositoryConfig{}, nil
	default:
		return RepositoryConfig{}, fmt.Errorf("unsupported repository.mode %q", raw.Mode)
	}

	remote := strings.TrimSpace(raw.Remote)
	if remote == "" {
		remote = "origin"
	}

	path := strings.TrimSpace(raw.Path)
	if path == "" {
		path = "/workspace"
	}
	if !strings.HasPrefix(path, "/") {
		return RepositoryConfig{}, fmt.Errorf("repository.path %q must be absolute", raw.Path)
	}

	return RepositoryConfig{
		Implicit:   false,
		Mode:       mode,
		Remote:     remote,
		Path:       path,
		Submodules: raw.Submodules,
	}, nil
}

func readPolicy(path string) (rawPolicy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return rawPolicy{}, err
	}

	var raw rawPolicy
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return rawPolicy{}, fmt.Errorf("parse %s: %w", path, err)
	}

	return raw, nil
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

// ToProto converts a CompiledPolicy to the proto Policy message.
func (p *CompiledPolicy) ToProto() *cleanroomv1.Policy {
	if p == nil {
		return nil
	}
	allow := make([]*cleanroomv1.PolicyAllowRule, 0, len(p.Allow))
	for _, rule := range p.Allow {
		ports := make([]int32, 0, len(rule.Ports))
		for _, port := range rule.Ports {
			ports = append(ports, int32(port))
		}
		allow = append(allow, &cleanroomv1.PolicyAllowRule{
			Host:  rule.Host,
			Ports: ports,
		})
	}
	return &cleanroomv1.Policy{
		Version:     int32(p.Version),
		ImageRef:    p.ImageRef,
		ImageDigest: p.ImageDigest,
		Services: &cleanroomv1.PolicyServices{
			Docker: &cleanroomv1.PolicyDockerService{
				Required: p.Services.Docker.Required,
			},
		},
		NetworkDefault: p.NetworkDefault,
		Allow:          allow,
		Dependencies: &cleanroomv1.PolicyDependencies{
			Command: append([]string(nil), p.Dependencies.Command...),
			Key: &cleanroomv1.PolicyDependencyKey{
				Files: append([]string(nil), p.Dependencies.KeyFiles...),
			},
		},
		Hash: p.Hash,
	}
}

// FromProto converts a proto Policy message to a CompiledPolicy, validating required fields.
func FromProto(pb *cleanroomv1.Policy) (*CompiledPolicy, error) {
	if pb == nil {
		return nil, errors.New("missing policy")
	}
	if pb.GetVersion() == 0 {
		return nil, errors.New("policy missing required field: version")
	}
	if pb.GetVersion() != 1 {
		return nil, fmt.Errorf("unsupported policy version %d: only version 1 is supported", pb.GetVersion())
	}
	imageRef := strings.TrimSpace(pb.GetImageRef())
	if imageRef == "" {
		return nil, errors.New("policy missing required field: image_ref")
	}
	parsedRef, err := ociref.ParseDigestReference(imageRef)
	if err != nil {
		return nil, fmt.Errorf("invalid policy image_ref: %w", err)
	}
	if providedDigest := strings.TrimSpace(pb.GetImageDigest()); providedDigest != "" && providedDigest != parsedRef.Digest() {
		return nil, fmt.Errorf("policy image_digest %q does not match image_ref digest %q", providedDigest, parsedRef.Digest())
	}
	networkDefault := strings.TrimSpace(strings.ToLower(pb.GetNetworkDefault()))
	if networkDefault == "" {
		networkDefault = "deny"
	}
	switch networkDefault {
	case "deny", "allow":
	default:
		return nil, fmt.Errorf("unsupported policy network_default %q: expected deny or allow", networkDefault)
	}

	allow := make([]AllowRule, 0, len(pb.GetAllow()))
	for _, rule := range pb.GetAllow() {
		host := strings.TrimSpace(strings.ToLower(rule.GetHost()))
		if host == "" {
			return nil, errors.New("allow rule host cannot be empty")
		}
		ports := make([]int, 0, len(rule.GetPorts()))
		seen := map[int]struct{}{}
		for _, port := range rule.GetPorts() {
			if port < 1 || port > 65535 {
				return nil, fmt.Errorf("allow rule for host %q contains invalid port %d", host, port)
			}
			candidate := int(port)
			if _, ok := seen[candidate]; ok {
				continue
			}
			seen[candidate] = struct{}{}
			ports = append(ports, candidate)
		}
		if len(ports) == 0 {
			return nil, fmt.Errorf("allow rule for host %q must include at least one port", host)
		}
		sort.Ints(ports)
		allow = append(allow, AllowRule{Host: host, Ports: ports})
	}

	sort.Slice(allow, func(i, j int) bool {
		return allow[i].Host < allow[j].Host
	})

	dependencies, err := dependenciesFromProto(pb.GetDependencies())
	if err != nil {
		return nil, err
	}

	compiled := &CompiledPolicy{
		Version:     int(pb.GetVersion()),
		ImageRef:    parsedRef.Original,
		ImageDigest: parsedRef.Digest(),
		Services: Services{
			Docker: DockerService{
				Required: pb.GetServices().GetDocker().GetRequired(),
			},
		},
		NetworkDefault: networkDefault,
		Allow:          allow,
		Dependencies:   dependencies,
	}

	hash, err := hashPolicy(compiled)
	if err != nil {
		return nil, err
	}

	if pb.GetHash() != "" && pb.GetHash() != hash {
		return nil, fmt.Errorf("policy hash mismatch: expected %q, got %q", hash, pb.GetHash())
	}
	compiled.Hash = hash
	return compiled, nil
}

func normalizeDependencies(raw rawDependenciesConfig) (Dependencies, error) {
	command, err := normalizeDependencyCommand(raw.Command)
	if err != nil {
		return Dependencies{}, err
	}
	keyFiles, err := normalizeDependencyKeyFiles(raw.Key.Files)
	if err != nil {
		return Dependencies{}, err
	}
	if len(command) == 0 {
		if len(keyFiles) > 0 {
			return Dependencies{}, errors.New("sandbox.dependencies.key.files requires sandbox.dependencies.command")
		}
		return Dependencies{}, nil
	}
	return Dependencies{
		Command:  command,
		KeyFiles: keyFiles,
	}, nil
}

func dependenciesFromProto(pb *cleanroomv1.PolicyDependencies) (Dependencies, error) {
	if pb == nil {
		return Dependencies{}, nil
	}
	command, err := normalizeDependencyCommand(pb.GetCommand())
	if err != nil {
		return Dependencies{}, err
	}
	keyFiles, err := normalizeDependencyKeyFiles(pb.GetKey().GetFiles())
	if err != nil {
		return Dependencies{}, err
	}
	if len(command) == 0 {
		if len(keyFiles) > 0 {
			return Dependencies{}, errors.New("policy dependencies.key.files requires dependencies.command")
		}
		return Dependencies{}, nil
	}
	return Dependencies{
		Command:  command,
		KeyFiles: keyFiles,
	}, nil
}

func normalizeDependencyCommand(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	command := make([]string, 0, len(raw))
	for i, arg := range raw {
		trimmed := strings.TrimSpace(arg)
		if trimmed == "" {
			return nil, fmt.Errorf("sandbox.dependencies.command[%d] cannot be empty", i)
		}
		command = append(command, trimmed)
	}
	return command, nil
}

func normalizeDependencyKeyFiles(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(raw))
	files := make([]string, 0, len(raw))
	for i, candidate := range raw {
		trimmed := strings.TrimSpace(candidate)
		if trimmed == "" {
			return nil, fmt.Errorf("sandbox.dependencies.key.files[%d] cannot be empty", i)
		}
		if strings.HasPrefix(trimmed, "/") {
			return nil, fmt.Errorf("sandbox.dependencies.key.files[%d] must be relative", i)
		}
		cleaned := path.Clean(strings.ReplaceAll(trimmed, "\\", "/"))
		if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
			return nil, fmt.Errorf("sandbox.dependencies.key.files[%d] must stay within the repository root", i)
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
