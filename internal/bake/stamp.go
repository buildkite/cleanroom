package bake

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/buildkite/cleanroom/internal/policy"
)

// AnnotationPrefix namespaces all cleanroom provenance annotations.
const AnnotationPrefix = "dev.buildkite.cleanroom."

// ProvenanceVersion is the current cleanroom provenance schema version.
const ProvenanceVersion = "1"

// Stamp collects provenance facts for a bake: policy identity, image
// identity, workspace facts, git facts when available, and the network rules
// accepted by Compile. It reads the local filesystem only.
func Stamp(cwd, policySource string, compiled *policy.CompiledPolicy, cleanroomVersion string, rules []NetworkRule) (map[string]string, error) {
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
	addGitAnnotations(annotations, cwd)

	networkValue, err := networkRulesAnnotation(rules)
	if err != nil {
		return nil, err
	}
	setAnnotation(annotations, AnnotationPrefix+"network.rules", networkValue)
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

func addGitAnnotations(annotations map[string]string, cwd string) {
	if strings.TrimSpace(cwd) == "" {
		return
	}
	if commit, err := gitOutput(cwd, "rev-parse", "HEAD"); err == nil {
		setAnnotation(annotations, AnnotationPrefix+"workspace.git.commit", commit)
	}
	if remote, err := gitOutput(cwd, "config", "--get", "remote.origin.url"); err == nil {
		setAnnotation(annotations, AnnotationPrefix+"workspace.git.remote", remote)
	}
	if status, err := gitOutput(cwd, "status", "--porcelain"); err == nil {
		dirty := "false"
		if strings.TrimSpace(status) != "" {
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
