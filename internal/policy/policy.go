package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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

type Loader struct{}

type rawPolicy struct {
	Version int `yaml:"version"`
	Sandbox struct {
		Image struct {
			Ref string `yaml:"ref"`
		} `yaml:"image"`
		Git     rawGitPolicy `yaml:"git"`
		Network struct {
			Default string         `yaml:"default"`
			Allow   []rawAllowRule `yaml:"allow"`
		} `yaml:"network"`
	} `yaml:"sandbox"`
}

type rawGitPolicy struct {
	Enabled      bool     `yaml:"enabled"`
	Source       string   `yaml:"source"`
	AllowedHosts []string `yaml:"allowed_hosts"`
	AllowedRepos []string `yaml:"allowed_repos"`
}

type rawAllowRule struct {
	Host  string `yaml:"host"`
	Ports []int  `yaml:"ports"`
}

type CompiledPolicy struct {
	Version        int         `json:"version"`
	ImageRef       string      `json:"image_ref"`
	ImageDigest    string      `json:"image_digest"`
	Git            *GitPolicy  `json:"git,omitempty"`
	NetworkDefault string      `json:"network_default"`
	Allow          []AllowRule `json:"allow"`
	Hash           string      `json:"hash"`
}

type GitPolicy struct {
	Enabled      bool     `json:"enabled"`
	Source       string   `json:"source"`
	AllowedHosts []string `json:"allowed_hosts"`
	AllowedRepos []string `json:"allowed_repos,omitempty"`
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

	return rawPolicy{}, "", fmt.Errorf("policy not found: expected %s or %s", primary, fallback)
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

	compiled := &CompiledPolicy{
		Version:        raw.Version,
		ImageRef:       parsedRef.Original,
		ImageDigest:    parsedRef.Digest(),
		Git:            nil,
		NetworkDefault: networkDefault,
		Allow:          allow,
	}

	gitPolicy, err := compileGitPolicy(raw.Sandbox.Git)
	if err != nil {
		return nil, err
	}
	compiled.Git = gitPolicy

	hash, err := hashPolicy(compiled)
	if err != nil {
		return nil, err
	}
	compiled.Hash = hash

	return compiled, nil
}

func (p *CompiledPolicy) Allows(host string, port int) bool {
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

func compileGitPolicy(raw rawGitPolicy) (*GitPolicy, error) {
	if !raw.Enabled {
		return nil, nil
	}

	source := strings.ToLower(strings.TrimSpace(raw.Source))
	if source == "" {
		source = "upstream"
	}
	if source != "upstream" && source != "host_mirror" {
		return nil, fmt.Errorf("invalid sandbox.git.source %q: expected upstream or host_mirror", source)
	}

	allowedHosts := make([]string, 0, len(raw.AllowedHosts))
	hostSeen := make(map[string]struct{}, len(raw.AllowedHosts))
	for _, host := range raw.AllowedHosts {
		normalized := strings.ToLower(strings.TrimSpace(host))
		if normalized == "" {
			return nil, errors.New("sandbox.git.allowed_hosts cannot contain empty entries")
		}
		if strings.Contains(normalized, "/") {
			return nil, fmt.Errorf("sandbox.git.allowed_hosts entry %q must be a hostname, not a path", normalized)
		}
		if _, ok := hostSeen[normalized]; ok {
			continue
		}
		hostSeen[normalized] = struct{}{}
		allowedHosts = append(allowedHosts, normalized)
	}
	if len(allowedHosts) == 0 {
		return nil, errors.New("sandbox.git.allowed_hosts must include at least one host when git is enabled")
	}
	sort.Strings(allowedHosts)

	allowedRepos := make([]string, 0, len(raw.AllowedRepos))
	repoSeen := make(map[string]struct{}, len(raw.AllowedRepos))
	for _, repo := range raw.AllowedRepos {
		normalized := strings.ToLower(strings.TrimSpace(repo))
		if normalized == "" {
			return nil, errors.New("sandbox.git.allowed_repos cannot contain empty entries")
		}
		if strings.Count(normalized, "/") != 1 {
			return nil, fmt.Errorf("sandbox.git.allowed_repos entry %q must be in owner/repo form", normalized)
		}
		parts := strings.SplitN(normalized, "/", 2)
		if parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("sandbox.git.allowed_repos entry %q must be in owner/repo form", normalized)
		}
		if _, ok := repoSeen[normalized]; ok {
			continue
		}
		repoSeen[normalized] = struct{}{}
		allowedRepos = append(allowedRepos, normalized)
	}
	sort.Strings(allowedRepos)

	return &GitPolicy{
		Enabled:      true,
		Source:       source,
		AllowedHosts: allowedHosts,
		AllowedRepos: allowedRepos,
	}, nil
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
	var gitPolicy *cleanroomv1.PolicyGit
	if p.Git != nil && p.Git.Enabled {
		gitPolicy = &cleanroomv1.PolicyGit{
			Enabled:      p.Git.Enabled,
			Source:       p.Git.Source,
			AllowedHosts: append([]string(nil), p.Git.AllowedHosts...),
			AllowedRepos: append([]string(nil), p.Git.AllowedRepos...),
		}
	}
	return &cleanroomv1.Policy{
		Version:        int32(p.Version),
		ImageRef:       p.ImageRef,
		ImageDigest:    p.ImageDigest,
		Git:            gitPolicy,
		NetworkDefault: p.NetworkDefault,
		Allow:          allow,
		Hash:           p.Hash,
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
	if networkDefault != "deny" {
		return nil, fmt.Errorf("unsupported policy network_default %q: cleanroom requires deny-by-default", networkDefault)
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

	compiled := &CompiledPolicy{
		Version:        int(pb.GetVersion()),
		ImageRef:       parsedRef.Original,
		ImageDigest:    parsedRef.Digest(),
		Git:            nil,
		NetworkDefault: networkDefault,
		Allow:          allow,
	}

	if pb.GetGit() != nil {
		gitPolicy, err := compileGitPolicy(rawGitPolicy{
			Enabled:      pb.GetGit().GetEnabled(),
			Source:       pb.GetGit().GetSource(),
			AllowedHosts: pb.GetGit().GetAllowedHosts(),
			AllowedRepos: pb.GetGit().GetAllowedRepos(),
		})
		if err != nil {
			return nil, err
		}
		compiled.Git = gitPolicy
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
