package bake

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/buildkite/cleanroom/internal/mediation"
	"github.com/buildkite/cleanroom/internal/ociref"
	"github.com/buildkite/cleanroom/internal/policy"
)

// GatewayService is a recorded gateway service requirement: the stable
// service name and guest endpoint, never a host socket path.
type GatewayService struct {
	Name      string `json:"name"`
	GuestHost string `json:"guest_host"`
	GuestPort uint16 `json:"guest_port"`
}

// Provenance is the cleanroom fact set parsed from a spore's annotations.
type Provenance struct {
	Version           string           `json:"provenance_version"`
	CleanroomVersion  string           `json:"cleanroom_version,omitempty"`
	BakeKey           string           `json:"bake_key,omitempty"`
	PolicyHash        string           `json:"policy_hash,omitempty"`
	PolicySource      string           `json:"policy_source,omitempty"`
	ImageRef          string           `json:"image_ref,omitempty"`
	ImageDigest       string           `json:"image_digest,omitempty"`
	WorkspaceDir      string           `json:"workspace_dir,omitempty"`
	GitCommit         string           `json:"git_commit,omitempty"`
	GitRemote         string           `json:"git_remote,omitempty"`
	GitDirty          bool             `json:"git_dirty"`
	NetworkRules      []NetworkRule    `json:"network_rules,omitempty"`
	GatewayServices   []GatewayService `json:"gateway_services,omitempty"`
	MediationServices []string         `json:"mediation_services,omitempty"`
}

// ParseProvenance validates and extracts cleanroom provenance from spore
// annotations, failing closed on missing, unsupported, or malformed facts.
// Annotation values are attacker-influenced when verifying a foreign
// artifact, so every string fact is rejected if it carries control
// characters that could forge terminal output.
func ParseProvenance(annotations map[string]string) (Provenance, error) {
	version := strings.TrimSpace(annotations[AnnotationPrefix+"provenance.version"])
	if version == "" {
		return Provenance{}, errors.New("spore is missing cleanroom provenance")
	}
	if version != ProvenanceVersion {
		return Provenance{}, fmt.Errorf("unsupported cleanroom provenance version %q", version)
	}
	fact := func(key string) (string, error) {
		value := strings.TrimSpace(annotations[AnnotationPrefix+key])
		if containsControlCharacters(value) {
			return "", fmt.Errorf("cleanroom provenance %s contains control characters", key)
		}
		return value, nil
	}
	prov := Provenance{Version: version}
	for _, field := range []struct {
		key string
		dst *string
	}{
		{"version", &prov.CleanroomVersion},
		{"bake.key", &prov.BakeKey},
		{"policy.hash", &prov.PolicyHash},
		{"policy.source", &prov.PolicySource},
		{"image.ref", &prov.ImageRef},
		{"image.digest", &prov.ImageDigest},
		{"workspace.dir", &prov.WorkspaceDir},
		{"workspace.git.commit", &prov.GitCommit},
		{"workspace.git.remote", &prov.GitRemote},
	} {
		value, err := fact(field.key)
		if err != nil {
			return Provenance{}, err
		}
		*field.dst = value
	}
	switch dirty := strings.TrimSpace(annotations[AnnotationPrefix+"workspace.git.dirty"]); dirty {
	case "", "false":
	case "true":
		prov.GitDirty = true
	default:
		return Provenance{}, fmt.Errorf("cleanroom provenance workspace.git.dirty has invalid value %q", dirty)
	}
	// A cleanroom-produced spore always carries the full core fact set:
	// stamp records the policy hash, digest-pinned image ref and digest, and
	// the workspace dir, and both bake and the stamp CLI record the bake
	// key. Requiring all of them means a foreign manifest cannot reach
	// "verified" by forging a version marker plus a few weak facts.
	for _, core := range []struct {
		name  string
		value string
	}{
		{"bake.key", prov.BakeKey},
		{"policy.hash", prov.PolicyHash},
		{"image.ref", prov.ImageRef},
		{"image.digest", prov.ImageDigest},
		{"workspace.dir", prov.WorkspaceDir},
	} {
		if core.value == "" {
			return Provenance{}, fmt.Errorf("spore is missing cleanroom create provenance (%s)", core.name)
		}
	}
	// Presence is not enough: these values are attacker-influenced, so each
	// core fact must also carry the shape cleanroom stamps. The bake key and
	// policy hash are SHA-256 hex, and the image ref must be a digest-pinned
	// OCI reference whose digest matches the recorded image digest.
	if !isHexSHA256(prov.BakeKey) {
		return Provenance{}, fmt.Errorf("cleanroom provenance bake.key %q is not a SHA-256 hash", prov.BakeKey)
	}
	if !isHexSHA256(prov.PolicyHash) {
		return Provenance{}, fmt.Errorf("cleanroom provenance policy.hash %q is not a SHA-256 hash", prov.PolicyHash)
	}
	parsedRef, err := ociref.ParseDigestReference(prov.ImageRef)
	if err != nil {
		return Provenance{}, fmt.Errorf("cleanroom provenance image.ref is invalid: %w", err)
	}
	if parsedRef.Digest() != prov.ImageDigest {
		return Provenance{}, fmt.Errorf("cleanroom provenance image.digest %q does not match image.ref digest %q", prov.ImageDigest, parsedRef.Digest())
	}

	rules, err := parseNetworkRulesAnnotation(annotations[AnnotationPrefix+"network.rules"])
	if err != nil {
		return Provenance{}, err
	}
	prov.NetworkRules = rules

	services, err := parseGatewayServicesAnnotation(annotations[AnnotationPrefix+"gateway.services"])
	if err != nil {
		return Provenance{}, err
	}
	prov.GatewayServices = services

	mediationServices, err := parseMediationServicesAnnotation(annotations[AnnotationPrefix+"mediation.services"])
	if err != nil {
		return Provenance{}, err
	}
	prov.MediationServices = mediationServices
	// Stamp writes mediation.services and gateway.services together: the
	// former declares what the lineage may reach, the latter how to bind it.
	// One without the other means verify would either omit the required
	// --bind-service from the run hint or advertise an unrequested gateway.
	if (len(prov.MediationServices) > 0) != (len(prov.GatewayServices) > 0) {
		return Provenance{}, errors.New("cleanroom provenance mediation.services and gateway.services must be recorded together")
	}
	return prov, nil
}

// isHexSHA256 reports whether the value is a lowercase 64-character hex
// string, the shape of every hash cleanroom stamps.
func isHexSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

func parseMediationServicesAnnotation(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	var services []string
	if err := json.Unmarshal([]byte(value), &services); err != nil {
		return nil, fmt.Errorf("decode cleanroom mediation service provenance: %w", err)
	}
	if len(services) == 0 {
		return nil, errors.New("cleanroom mediation service provenance is empty")
	}
	for i, name := range services {
		if !isServiceToken(name) {
			return nil, fmt.Errorf("cleanroom mediation service provenance entry %d has invalid name %q", i, name)
		}
	}
	return services, nil
}

func containsControlCharacters(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func isServiceToken(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func parseNetworkRulesAnnotation(value string) ([]NetworkRule, error) {
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
	rules := make([]NetworkRule, 0, len(raw))
	for i, rule := range raw {
		host := strings.TrimSpace(rule.Host)
		if host == "" {
			return nil, fmt.Errorf("cleanroom network rule provenance entry %d is missing host", i)
		}
		if !isServiceToken(host) {
			return nil, fmt.Errorf("cleanroom network rule provenance entry %d has invalid host %q", i, host)
		}
		if len(rule.Ports) == 0 {
			return nil, fmt.Errorf("cleanroom network rule provenance entry %d is missing ports", i)
		}
		for _, port := range rule.Ports {
			if port == 0 {
				return nil, fmt.Errorf("cleanroom network rule provenance entry %d contains invalid port 0", i)
			}
		}
		rules = append(rules, NetworkRule{Host: host, Ports: append([]uint16(nil), rule.Ports...)})
	}
	return rules, nil
}

func parseGatewayServicesAnnotation(value string) ([]GatewayService, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	var services []GatewayService
	if err := json.Unmarshal([]byte(value), &services); err != nil {
		return nil, fmt.Errorf("decode cleanroom gateway service provenance: %w", err)
	}
	// Stamp only ever records the one fixed cleanroom binding, so anything
	// else is forged or corrupt. This also keeps RunFromInvocation from
	// rendering an attacker-chosen name or port into the suggested command.
	stamped := GatewayService{
		Name:      mediation.BoundServiceName,
		GuestHost: mediation.GuestHostname,
		GuestPort: mediation.GuestPort,
	}
	if len(services) != 1 || services[0] != stamped {
		return nil, fmt.Errorf("cleanroom gateway service provenance must be exactly %s at %s:%d", stamped.Name, stamped.GuestHost, stamped.GuestPort)
	}
	return services, nil
}

// RunFromInvocation renders the spore invocation that satisfies the spore's
// recorded requirements. Gateway services need live socket bindings; the
// placeholder path makes the requirement explicit without inventing a path.
func (p Provenance) RunFromInvocation(sporeDir string) string {
	parts := []string{"spore", "run", "--from", QuoteArgs([]string{sporeDir})}
	for _, service := range p.GatewayServices {
		binding := fmt.Sprintf("%s=unix:/path/to/%s.sock", service.Name, service.Name)
		parts = append(parts, "--bind-service", QuoteArgs([]string{binding}))
	}
	parts = append(parts, "'COMMAND'")
	return strings.Join(parts, " ")
}

// AuditKey recomputes the bake key from a repository's current state and
// compares it with the recorded key. A mismatch means the artifact was not
// produced from the repository's current policy and commit.
func AuditKey(prov Provenance, compiled *policy.CompiledPolicy, facts GitFacts) error {
	if prov.BakeKey == "" {
		return errors.New("spore records no bake key; it was not produced by cleanroom bake")
	}
	if !facts.HasGit {
		// Without git facts the key has no content input, so a match would
		// only prove the policy is the same — not that the artifact reflects
		// this directory's files.
		return errors.New("directory has no git metadata; the bake key cannot be audited without a commit")
	}
	expected := Key(compiled, facts)
	if prov.BakeKey != expected {
		return fmt.Errorf("bake key mismatch: spore records %.12s, repository state computes %.12s (policy, commit, or dirty state differ)", prov.BakeKey, expected)
	}
	if facts.Dirty {
		return errors.New("repository has uncommitted changes; a dirty workspace never matches a baked artifact")
	}
	return nil
}

// Summary renders the provenance facts as stable human-readable lines.
func (p Provenance) Summary() []string {
	lines := []string{
		"provenance version: " + p.Version,
	}
	add := func(label, value string) {
		if value != "" {
			lines = append(lines, label+": "+value)
		}
	}
	add("cleanroom version ", p.CleanroomVersion)
	add("policy hash       ", p.PolicyHash)
	add("bake key          ", p.BakeKey)
	add("image             ", p.ImageRef)
	workspace := p.WorkspaceDir
	if p.GitCommit != "" {
		state := "clean"
		if p.GitDirty {
			state = "dirty"
		}
		workspace = fmt.Sprintf("%s @ %.12s (%s)", workspace, p.GitCommit, state)
	}
	add("workspace         ", workspace)
	if len(p.NetworkRules) > 0 {
		endpoints := make([]string, 0, len(p.NetworkRules))
		for _, rule := range p.NetworkRules {
			for _, port := range rule.Ports {
				endpoints = append(endpoints, fmt.Sprintf("%s:%d", rule.Host, port))
			}
		}
		sort.Strings(endpoints)
		lines = append(lines, "network rules     : "+strings.Join(endpoints, " "))
	}
	if len(p.MediationServices) > 0 {
		lines = append(lines, "mediation services: "+strings.Join(p.MediationServices, " "))
	}
	if len(p.GatewayServices) > 0 {
		names := make([]string, 0, len(p.GatewayServices))
		for _, service := range p.GatewayServices {
			names = append(names, fmt.Sprintf("%s (%s:%d)", service.Name, service.GuestHost, service.GuestPort))
		}
		lines = append(lines, "gateway services  : "+strings.Join(names, " "))
	}
	return lines
}
