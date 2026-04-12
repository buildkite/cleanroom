package cachekey

import (
	"strings"
	"testing"
)

func TestRuntimeStageKey(t *testing.T) {
	inputs := RuntimeStageInputs{
		Backend:                       "firecracker",
		Architecture:                  "amd64",
		ImageDigest:                   "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		GuestAgentHash:                "sha256:2222222222222222222222222222222222222222222222222222222222222222",
		PreparedRuntimeRootFSVersion:  "v1",
		GuestInitScriptTemplateDigest: "sha256:3333333333333333333333333333333333333333333333333333333333333333",
	}

	got := RuntimeStageKey(inputs)
	if got == "" {
		t.Fatal("runtime stage key is empty")
	}
	if !strings.HasPrefix(got, "runtime:v1:") {
		t.Fatalf("runtime stage key prefix = %q, want %q", got, "runtime:v1:")
	}
	if gotAgain := RuntimeStageKey(inputs); got != gotAgain {
		t.Fatalf("runtime stage key changed for identical inputs: first %q second %q", got, gotAgain)
	}

	mutated := inputs
	mutated.ImageDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if gotMutated := RuntimeStageKey(mutated); got == gotMutated {
		t.Fatalf("runtime stage key did not change after input mutation: %q", got)
	}
}

func TestWorkspaceStageKey(t *testing.T) {
	inputs := WorkspaceStageInputs{
		RuntimeKey:                  "runtime:v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CanonicalRemoteURL:          "https://github.com/buildkite/cleanroom.git",
		CommitSHA:                   "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		SubmoduleMode:               "recursive",
		SubmoduleResolutionDigest:   "sha256:4444444444444444444444444444444444444444444444444444444444444444",
		CheckoutMode:                "detached",
		DestinationDir:              "/workspace",
		MaterializationRecipeDigest: "sha256:5555555555555555555555555555555555555555555555555555555555555555",
	}

	got := WorkspaceStageKey(inputs)
	if got == "" {
		t.Fatal("workspace stage key is empty")
	}
	if !strings.HasPrefix(got, "workspace:v1:") {
		t.Fatalf("workspace stage key prefix = %q, want %q", got, "workspace:v1:")
	}
	if gotAgain := WorkspaceStageKey(inputs); got != gotAgain {
		t.Fatalf("workspace stage key changed for identical inputs: first %q second %q", got, gotAgain)
	}

	mutated := inputs
	mutated.MaterializationRecipeDigest = "sha256:6666666666666666666666666666666666666666666666666666666666666666"
	if gotMutated := WorkspaceStageKey(mutated); got == gotMutated {
		t.Fatalf("workspace stage key did not change after input mutation: %q", got)
	}
}

func TestDependencyStageKey(t *testing.T) {
	inputs := DependencyStageInputs{
		WorkspaceKey:               "workspace:v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CompiledPolicyHash:         "sha256:7777777777777777777777777777777777777777777777777777777777777777",
		ToolchainManifestDigest:    "sha256:8888888888888888888888888888888888888888888888888888888888888888",
		ResolvedToolVersionsDigest: "sha256:9999999999999999999999999999999999999999999999999999999999999999",
		LockfileInputsDigest:       "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		LockfileParserVersion:      "v1",
		BootstrapRecipeDigest:      "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
	}

	got := DependencyStageKey(inputs)
	if got == "" {
		t.Fatal("dependency stage key is empty")
	}
	if !strings.HasPrefix(got, "dependency:v1:") {
		t.Fatalf("dependency stage key prefix = %q, want %q", got, "dependency:v1:")
	}
	if gotAgain := DependencyStageKey(inputs); got != gotAgain {
		t.Fatalf("dependency stage key changed for identical inputs: first %q second %q", got, gotAgain)
	}

	mutated := inputs
	mutated.LockfileParserVersion = "v2"
	if gotMutated := DependencyStageKey(mutated); got == gotMutated {
		t.Fatalf("dependency stage key did not change after input mutation: %q", got)
	}
}
