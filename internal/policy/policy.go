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
		Network struct {
			Default string         `yaml:"default"`
			Allow   []rawAllowRule `yaml:"allow"`
		} `yaml:"network"`
	} `yaml:"sandbox"`
}

type rawAllowRule struct {
	Host  string `yaml:"host"`
	Ports []int  `yaml:"ports"`
}

type CompiledPolicy struct {
	Version        int         `json:"version"`
	NetworkDefault string      `json:"network_default"`
	Allow          []AllowRule `json:"allow"`
	Hash           string      `json:"hash"`
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
		NetworkDefault: networkDefault,
		Allow:          allow,
	}

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
