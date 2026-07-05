// Package bake translates repository policy and workspace facts into SporeVM
// inputs. Compile and Stamp are the pure stages of the bake pipeline: they
// read policy and local filesystem state and make no runtime calls.
package bake

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/buildkite/cleanroom/internal/policy"
)

// CreateInputs is the validated translation of repo policy into spore create
// inputs.
type CreateInputs struct {
	ImageRef     string
	MemoryBytes  uint64
	VCPUs        uint32
	NetworkRules []NetworkRule
}

// NetworkRule is one exact host-plus-port network allow rule.
type NetworkRule struct {
	Host  string
	Ports []uint16
}

// sporePageAlignment satisfies SporeVM's --memory page alignment requirement,
// which is the host's minimum page size: 16KiB on macOS arm64, 4KiB on Linux.
// 16KiB is a multiple of both, so compiled arguments stay portable.
const sporePageAlignment = 16384

// Compile validates the enforceable policy subset and translates it into
// SporeVM create inputs. Policy that SporeVM cannot enforce fails closed.
func Compile(compiled *policy.CompiledPolicy) (CreateInputs, error) {
	if compiled == nil {
		return CreateInputs{}, errors.New("missing compiled policy")
	}
	imageRef := strings.TrimSpace(compiled.ImageRef)
	if imageRef == "" {
		return CreateInputs{}, errors.New("cleanroom compile requires sandbox.image.ref")
	}
	inputs := CreateInputs{ImageRef: imageRef}
	if compiled.Resources != nil {
		if compiled.Resources.DiskBytes > 0 {
			return CreateInputs{}, errors.New("cleanroom compile does not yet translate sandbox.resources.disk to SporeVM")
		}
		if compiled.Resources.MemoryBytes > 0 {
			// Cleanroom policy sizes use decimal units (1gb = 10^9), but
			// spore's --memory requires page alignment; round up to the next
			// page so common policy values stay valid.
			inputs.MemoryBytes = pageAlign(uint64(compiled.Resources.MemoryBytes))
		}
		if compiled.Resources.VCPUs > 0 {
			if compiled.Resources.VCPUs > math.MaxUint32 {
				return CreateInputs{}, fmt.Errorf("cleanroom compile does not support vcpus %d", compiled.Resources.VCPUs)
			}
			inputs.VCPUs = uint32(compiled.Resources.VCPUs)
		}
	}
	if compiled.HasStageScopedNetwork() {
		return CreateInputs{}, errors.New("cleanroom compile does not yet translate stage-scoped network policy to SporeVM")
	}
	rules, err := networkRules(compiled)
	if err != nil {
		return CreateInputs{}, err
	}
	inputs.NetworkRules = rules
	if compiled.RequiresDockerService() {
		return CreateInputs{}, errors.New("cleanroom compile does not yet translate docker service policy to SporeVM")
	}
	if len(compiled.Services.Blocks) > 0 || len(compiled.Services.Command) > 0 {
		return CreateInputs{}, errors.New("cleanroom compile does not yet translate service stages to SporeVM")
	}
	if len(compiled.Dependencies.Blocks) > 0 || len(compiled.Dependencies.Command) > 0 {
		return CreateInputs{}, errors.New("cleanroom compile does not yet translate dependency stages to SporeVM")
	}
	if len(compiled.Run.Before) > 0 {
		return CreateInputs{}, errors.New("cleanroom compile does not yet translate run.before hooks to SporeVM")
	}
	return inputs, nil
}

func networkRules(compiled *policy.CompiledPolicy) ([]NetworkRule, error) {
	if len(compiled.Allow) == 0 {
		return nil, nil
	}
	rules := make([]NetworkRule, 0, len(compiled.Allow))
	for _, rule := range compiled.Allow {
		host := strings.TrimSpace(rule.Host)
		if host == "" {
			return nil, errors.New("cleanroom compile requires network allow rules to include a host")
		}
		// Hosts must survive the stamp/verify round-trip: provenance parsing
		// accepts only hostname characters, so anything else (IPv6 literals
		// in particular) would bake a spore that fails its own verify. Fail
		// closed until SporeVM has a supported encoding.
		if !isServiceToken(host) {
			return nil, fmt.Errorf("cleanroom compile does not yet support network allow host %q (IPv6 literals and non-hostname characters are not translatable to SporeVM)", host)
		}
		if len(rule.Ports) == 0 {
			return nil, fmt.Errorf("cleanroom compile requires network allow rule for %s to include at least one port", host)
		}
		ports := make([]uint16, 0, len(rule.Ports))
		for _, port := range rule.Ports {
			if port < 1 || port > 65535 {
				return nil, fmt.Errorf("cleanroom compile does not support network allow port %d for %s", port, host)
			}
			ports = append(ports, uint16(port))
		}
		rules = append(rules, NetworkRule{Host: host, Ports: ports})
	}
	return rules, nil
}

// Args renders the inputs as spore create arguments. Network rules target the
// exact host-plus-port contract (--allow-host-port), which requires a spore
// CLI with create-time parity; older spore versions reject the flag rather
// than silently widening the policy.
func (in CreateInputs) Args() []string {
	args := []string{"--image", in.ImageRef}
	if in.MemoryBytes > 0 {
		args = append(args, "--memory", formatMemory(in.MemoryBytes))
	}
	if in.VCPUs > 0 {
		args = append(args, "--vcpus", strconv.FormatUint(uint64(in.VCPUs), 10))
	}
	if len(in.NetworkRules) > 0 {
		args = append(args, "--net")
		for _, rule := range in.NetworkRules {
			for _, port := range rule.Ports {
				args = append(args, "--allow-host-port", fmt.Sprintf("%s:%d", rule.Host, port))
			}
		}
	}
	return args
}

func pageAlign(bytes uint64) uint64 {
	if remainder := bytes % sporePageAlignment; remainder != 0 {
		return bytes + sporePageAlignment - remainder
	}
	return bytes
}

// formatMemory renders bytes in the smallest exact binary unit accepted by
// spore's --memory parser (b, kb, mb, gb).
func formatMemory(bytes uint64) string {
	const (
		kib = uint64(1024)
		mib = kib * 1024
		gib = mib * 1024
	)
	switch {
	case bytes%gib == 0:
		return fmt.Sprintf("%dgb", bytes/gib)
	case bytes%mib == 0:
		return fmt.Sprintf("%dmb", bytes/mib)
	case bytes%kib == 0:
		return fmt.Sprintf("%dkb", bytes/kib)
	default:
		return fmt.Sprintf("%db", bytes)
	}
}
