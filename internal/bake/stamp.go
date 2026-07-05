package bake

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/buildkite/cleanroom/internal/mediation"
	"github.com/buildkite/cleanroom/internal/policy"
)

// AnnotationPrefix namespaces all cleanroom provenance annotations.
const AnnotationPrefix = "dev.buildkite.cleanroom."

// ProvenanceVersion is the current cleanroom provenance schema version.
const ProvenanceVersion = "1"

// Stamp renders provenance facts for a bake: policy identity, image
// identity, workspace facts, the supplied git facts, and the network rules
// accepted by Compile. Callers collect facts once (see CollectGitFacts) so
// the recorded annotations and the bake key always describe the same
// workspace state.
func Stamp(cwd, policySource string, compiled *policy.CompiledPolicy, cleanroomVersion string, rules []NetworkRule, facts GitFacts) (map[string]string, error) {
	annotations := map[string]string{
		AnnotationPrefix + "provenance.version": ProvenanceVersion,
	}
	setAnnotation(annotations, AnnotationPrefix+"version", cleanroomVersion)
	if compiled != nil {
		setAnnotation(annotations, AnnotationPrefix+"policy.hash", compiled.Hash)
		setAnnotation(annotations, AnnotationPrefix+"image.ref", compiled.ImageRef)
		setAnnotation(annotations, AnnotationPrefix+"image.digest", compiled.ImageDigest)
	}
	setAnnotation(annotations, AnnotationPrefix+"policy.source", annotationPath(policySource))
	setAnnotation(annotations, AnnotationPrefix+"workspace.dir", annotationPath(cwd))
	addGitAnnotations(annotations, facts)

	networkValue, err := networkRulesAnnotation(rules)
	if err != nil {
		return nil, err
	}
	setAnnotation(annotations, AnnotationPrefix+"network.rules", networkValue)

	if compiled != nil && len(compiled.Mediation) > 0 {
		mediationValue, err := json.Marshal(compiled.Mediation)
		if err != nil {
			return nil, fmt.Errorf("encode cleanroom mediation annotations: %w", err)
		}
		annotations[AnnotationPrefix+"mediation.services"] = string(mediationValue)
		gatewayValue, err := json.Marshal([]GatewayService{{
			Name:      mediation.BoundServiceName,
			GuestHost: mediation.GuestHostname,
			GuestPort: mediation.GuestPort,
		}})
		if err != nil {
			return nil, fmt.Errorf("encode cleanroom gateway service annotations: %w", err)
		}
		annotations[AnnotationPrefix+"gateway.services"] = string(gatewayValue)
	}
	return annotations, nil
}

// AnnotationArgs renders annotations as deterministic (sorted) spore create
// --annotation arguments.
func AnnotationArgs(annotations map[string]string) []string {
	keys := make([]string, 0, len(annotations))
	for key := range annotations {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	args := make([]string, 0, len(keys)*2)
	for _, key := range keys {
		args = append(args, "--annotation", key+"="+annotations[key])
	}
	return args
}

func networkRulesAnnotation(rules []NetworkRule) (string, error) {
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

// GitFacts are the workspace version-control facts recorded in provenance and
// folded into the bake key.
type GitFacts struct {
	Commit string
	Remote string
	Dirty  bool
	HasGit bool
}

// CollectGitFacts reads git facts for a workspace directory. Missing git or a
// non-repository directory yields zero facts, not an error.
func CollectGitFacts(cwd string) GitFacts {
	return CollectGitFactsExcluding(cwd, nil)
}

// CollectGitFactsExcluding is CollectGitFacts but ignores the given paths
// (relative to cwd) when deciding dirty state. Bake passes its output spore
// path so a `--out` inside the repository is not mistaken for uncommitted
// source, which would otherwise break the idempotent-rebake path.
func CollectGitFactsExcluding(cwd string, excludeRel []string) GitFacts {
	if strings.TrimSpace(cwd) == "" {
		return GitFacts{}
	}
	facts := GitFacts{}
	if commit, err := gitOutput(cwd, "rev-parse", "HEAD"); err == nil {
		facts.Commit = commit
		facts.HasGit = true
	}
	if remote, err := gitOutput(cwd, "config", "--get", "remote.origin.url"); err == nil {
		facts.Remote = remote
	}
	statusArgs := []string{"status", "--porcelain"}
	if len(excludeRel) > 0 {
		statusArgs = append(statusArgs, "--", ".")
		for _, rel := range excludeRel {
			rel = strings.TrimSpace(rel)
			if rel == "" {
				continue
			}
			statusArgs = append(statusArgs, ":(exclude)"+rel, ":(exclude)"+rel+"/**")
		}
	}
	if status, err := gitOutput(cwd, statusArgs...); err == nil {
		facts.Dirty = strings.TrimSpace(status) != ""
		facts.HasGit = true
	}
	return facts
}

func addGitAnnotations(annotations map[string]string, facts GitFacts) {
	setAnnotation(annotations, AnnotationPrefix+"workspace.git.commit", facts.Commit)
	setAnnotation(annotations, AnnotationPrefix+"workspace.git.remote", facts.Remote)
	if facts.HasGit {
		dirty := "false"
		if facts.Dirty {
			dirty = "true"
		}
		annotations[AnnotationPrefix+"workspace.git.dirty"] = dirty
	}
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func annotationPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	return filepath.Clean(path)
}

func setAnnotation(annotations map[string]string, key, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	annotations[key] = value
}
