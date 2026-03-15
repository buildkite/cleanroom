package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/buildkite/cleanroom/internal/policy"
	"gopkg.in/yaml.v3"
)

type ImageBumpRefCommand struct {
	Source     string `arg:"" optional:"" help:"Image ref to resolve (default: ghcr.io/buildkite/cleanroom-base/alpine:latest)"`
	Chdir      string `short:"c" help:"Change to this directory before running commands"`
	PolicyPath string `help:"Policy file path (default: cleanroom.yaml, or .buildkite/cleanroom.yaml when primary is missing)"`
}

func (c *ImageBumpRefCommand) Run(ctx *runtimeContext) error {
	cwd, err := resolveCWD(ctx.CWD, c.Chdir)
	if err != nil {
		return err
	}

	resolvedRef, err := resolveReferenceForPolicyUpdate(context.Background(), c.Source)
	if err != nil {
		return err
	}

	policyPath, err := resolvePolicyPathForUpdate(cwd, c.PolicyPath)
	if err != nil {
		return err
	}

	raw, err := os.ReadFile(policyPath)
	if err != nil {
		return fmt.Errorf("read policy %s: %w", policyPath, err)
	}

	updated, err := setSandboxImageRef(raw, resolvedRef)
	if err != nil {
		return fmt.Errorf("update policy %s: %w", policyPath, err)
	}

	info, err := os.Stat(policyPath)
	if err != nil {
		return fmt.Errorf("stat policy %s: %w", policyPath, err)
	}

	if err := os.WriteFile(policyPath, updated, info.Mode().Perm()); err != nil {
		return fmt.Errorf("write policy %s: %w", policyPath, err)
	}

	source := strings.TrimSpace(c.Source)
	if source == "" {
		source = defaultBumpRefSource
	}
	_, err = fmt.Fprint(ctx.Stdout, renderSummaryBlock(summaryBlock{
		Title:      "updated sandbox.image.ref",
		TitleStyle: defaultTerminalPalette().info,
		Fields: []startupField{
			{Key: "policy", Value: policyPath},
			{Key: "source", Value: source},
			{Key: "ref", Value: resolvedRef},
		},
	}, shouldUseANSI(ctx.Stdout)))
	return err
}

func resolvePolicyPathForUpdate(cwd, candidate string) (string, error) {
	if strings.TrimSpace(candidate) != "" {
		if filepath.IsAbs(candidate) {
			return filepath.Clean(candidate), nil
		}
		return filepath.Join(cwd, candidate), nil
	}

	primary := filepath.Join(cwd, policy.PrimaryPolicyPath)
	primaryExists, err := fileExists(primary)
	if err != nil {
		return "", fmt.Errorf("check policy %s: %w", primary, err)
	}
	if primaryExists {
		return primary, nil
	}

	fallback := filepath.Join(cwd, policy.FallbackPolicyPath)
	fallbackExists, err := fileExists(fallback)
	if err != nil {
		return "", fmt.Errorf("check policy %s: %w", fallback, err)
	}
	if fallbackExists {
		return fallback, nil
	}

	return "", fmt.Errorf("policy not found: expected %s or %s", primary, fallback)
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func setSandboxImageRef(raw []byte, ref string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 {
		doc.Kind = yaml.DocumentNode
	}

	bootstrapped := false
	if len(doc.Content) == 0 {
		doc.Content = append(doc.Content, &yaml.Node{Kind: yaml.MappingNode})
		bootstrapped = true
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("policy root must be a mapping")
	}
	if bootstrapped {
		setMapInt(root, "version", 1)
	}

	sandbox := ensureMapEntry(root, "sandbox")
	image := ensureMapEntry(sandbox, "image")
	setMapString(image, "ref", ref)

	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	err := enc.Encode(&doc)
	_ = enc.Close()
	if err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func ensureMapEntry(parent *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value != key {
			continue
		}
		if parent.Content[i+1].Kind != yaml.MappingNode {
			parent.Content[i+1].Kind = yaml.MappingNode
			parent.Content[i+1].Tag = ""
			parent.Content[i+1].Value = ""
			parent.Content[i+1].Content = nil
		}
		return parent.Content[i+1]
	}

	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	valueNode := &yaml.Node{Kind: yaml.MappingNode}
	parent.Content = append(parent.Content, keyNode, valueNode)
	return valueNode
}

func setMapString(parent *yaml.Node, key, value string) {
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value != key {
			continue
		}
		parent.Content[i+1].Kind = yaml.ScalarNode
		parent.Content[i+1].Tag = "!!str"
		parent.Content[i+1].Value = value
		parent.Content[i+1].Content = nil
		return
	}

	parent.Content = append(
		parent.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}

func setMapInt(parent *yaml.Node, key string, value int) {
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value != key {
			continue
		}
		parent.Content[i+1].Kind = yaml.ScalarNode
		parent.Content[i+1].Tag = "!!int"
		parent.Content[i+1].Value = fmt.Sprintf("%d", value)
		parent.Content[i+1].Content = nil
		return
	}

	parent.Content = append(
		parent.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprintf("%d", value)},
	)
}
