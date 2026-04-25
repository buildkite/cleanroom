package cachekey

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"path"
	"strings"
)

const formatVersion = "v1"

const keyNamespace = "cleanroom/cachekey"

// RuntimeStageInputs are the inputs that define the reusable runtime stage.
type RuntimeStageInputs struct {
	Backend                       string
	Architecture                  string
	ImageDigest                   string
	GuestAgentHash                string
	PreparedRuntimeRootFSVersion  string
	GuestInitScriptTemplateDigest string
}

// WorkspaceStageInputs are the inputs that define the reusable workspace stage.
type WorkspaceStageInputs struct {
	Backend                     string
	RuntimeKey                  string
	CompiledPolicyHash          string
	CanonicalRemoteURL          string
	CommitSHA                   string
	SubmoduleMode               string
	SubmoduleResolutionDigest   string
	ChangesetDigest             string
	CheckoutMode                string
	DestinationDir              string
	MaterializationRecipeDigest string
}

// DependencyStageInputs are the inputs that define the reusable dependency stage.
type DependencyStageInputs struct {
	WorkspaceKey          string
	CompiledPolicyHash    string
	KeyFilesDigest        string
	BootstrapRecipeDigest string
}

// ServicesStageInputs are the inputs that define the reusable services stage.
type ServicesStageInputs struct {
	ParentStageKey        string
	CompiledPolicyHash    string
	KeyFilesDigest        string
	BootstrapRecipeDigest string
}

// RuntimeStageKey returns the canonical cache key for a runtime stage output.
func RuntimeStageKey(in RuntimeStageInputs) string {
	return buildStageKey("runtime", []component{
		{name: "backend", value: canonicalIdentifier(in.Backend)},
		{name: "architecture", value: canonicalIdentifier(in.Architecture)},
		{name: "image_digest", value: canonicalDigest(in.ImageDigest)},
		{name: "guest_agent_hash", value: canonicalDigest(in.GuestAgentHash)},
		{name: "prepared_runtime_rootfs_version", value: canonicalIdentifier(in.PreparedRuntimeRootFSVersion)},
		{name: "guest_init_script_template_digest", value: canonicalDigest(in.GuestInitScriptTemplateDigest)},
	})
}

// WorkspaceStageKey returns the canonical cache key for a workspace stage output.
func WorkspaceStageKey(in WorkspaceStageInputs) string {
	return buildStageKey("workspace", []component{
		{name: "backend", value: canonicalIdentifier(in.Backend)},
		{name: "runtime_key", value: canonicalReference(in.RuntimeKey)},
		{name: "compiled_policy_hash", value: canonicalDigest(in.CompiledPolicyHash)},
		{name: "canonical_remote_url", value: canonicalRemoteURL(in.CanonicalRemoteURL)},
		{name: "commit_sha", value: canonicalDigest(in.CommitSHA)},
		{name: "submodule_mode", value: canonicalIdentifier(in.SubmoduleMode)},
		{name: "submodule_resolution_digest", value: canonicalDigest(in.SubmoduleResolutionDigest)},
		{name: "changeset_digest", value: canonicalDigest(in.ChangesetDigest)},
		{name: "checkout_mode", value: canonicalIdentifier(in.CheckoutMode)},
		{name: "destination_dir", value: canonicalAbsolutePath(in.DestinationDir)},
		{name: "materialization_recipe_digest", value: canonicalDigest(in.MaterializationRecipeDigest)},
	})
}

// DependencyStageKey returns the canonical cache key for a dependency stage output.
func DependencyStageKey(in DependencyStageInputs) string {
	return buildStageKey("dependency", []component{
		{name: "workspace_key", value: canonicalReference(in.WorkspaceKey)},
		{name: "compiled_policy_hash", value: canonicalDigest(in.CompiledPolicyHash)},
		{name: "key_files_digest", value: canonicalDigest(in.KeyFilesDigest)},
		{name: "bootstrap_recipe_digest", value: canonicalDigest(in.BootstrapRecipeDigest)},
	})
}

// ServicesStageKey returns the canonical cache key for a services stage output.
func ServicesStageKey(in ServicesStageInputs) string {
	return buildStageKey("services", []component{
		{name: "parent_stage_key", value: canonicalReference(in.ParentStageKey)},
		{name: "compiled_policy_hash", value: canonicalDigest(in.CompiledPolicyHash)},
		{name: "key_files_digest", value: canonicalDigest(in.KeyFilesDigest)},
		{name: "bootstrap_recipe_digest", value: canonicalDigest(in.BootstrapRecipeDigest)},
	})
}

type component struct {
	name  string
	value string
}

func buildStageKey(stage string, components []component) string {
	sum := sha256.Sum256(encodeStagePayload(stage, components))
	return stage + ":" + formatVersion + ":" + hex.EncodeToString(sum[:])
}

func encodeStagePayload(stage string, components []component) []byte {
	var buf bytes.Buffer
	writeComponent(&buf, keyNamespace)
	writeComponent(&buf, formatVersion)
	writeComponent(&buf, stage)
	for _, c := range components {
		writeComponent(&buf, c.name)
		writeComponent(&buf, c.value)
	}
	return buf.Bytes()
}

func writeComponent(buf *bytes.Buffer, value string) {
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(len(value)))
	_, _ = buf.Write(lenBuf[:])
	_, _ = buf.WriteString(value)
}

func canonicalDigest(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func canonicalIdentifier(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func canonicalReference(value string) string {
	return strings.TrimSpace(value)
}

func canonicalRemoteURL(value string) string {
	return strings.TrimSpace(value)
}

func canonicalAbsolutePath(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "/") {
		return path.Clean(trimmed)
	}
	return trimmed
}
