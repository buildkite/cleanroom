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
		Backend:                     "firecracker",
		RuntimeKey:                  "runtime:v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CompiledPolicyHash:          "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		CanonicalRemoteURL:          "https://github.com/buildkite/cleanroom.git",
		CommitSHA:                   "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		SubmoduleMode:               "recursive",
		SubmoduleResolutionDigest:   "sha256:4444444444444444444444444444444444444444444444444444444444444444",
		ChangesetDigest:             "sha256:6666666666666666666666666666666666666666666666666666666666666666",
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
	mutated.ChangesetDigest = "sha256:7777777777777777777777777777777777777777777777777777777777777777"
	if gotMutated := WorkspaceStageKey(mutated); got == gotMutated {
		t.Fatalf("workspace stage key did not change after input mutation: %q", got)
	}
}

func TestDependencyStageKey(t *testing.T) {
	inputs := DependencyStageInputs{
		WorkspaceKey:          "workspace:v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CompiledPolicyHash:    "sha256:7777777777777777777777777777777777777777777777777777777777777777",
		KeyFilesDigest:        "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		BootstrapRecipeDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
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
	mutated.KeyFilesDigest = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	if gotMutated := DependencyStageKey(mutated); got == gotMutated {
		t.Fatalf("dependency stage key did not change after input mutation: %q", got)
	}
}

func TestPortableDependencyStageKey(t *testing.T) {
	inputs := PortableDependencyStageInputs{
		Backend:                     "firecracker",
		RuntimeKey:                  "runtime:v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CompiledPolicyHash:          "sha256:7777777777777777777777777777777777777777777777777777777777777777",
		CanonicalRemoteURL:          "https://github.com/buildkite/cleanroom.git",
		SubmoduleMode:               "disabled",
		DestinationDir:              "/workspace",
		CheckoutRefreshRecipeDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		KeyFilesDigest:              "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		BootstrapRecipeDigest:       "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		OutputMode:                  "outside-workspace",
		ProducerVersion:             "cleanroom/portable-dependency-stage-v1",
	}

	got := PortableDependencyStageKey(inputs)
	if got == "" {
		t.Fatal("portable dependency stage key is empty")
	}
	if !strings.HasPrefix(got, "dependency:v1:") {
		t.Fatalf("portable dependency stage key prefix = %q, want %q", got, "dependency:v1:")
	}
	if gotAgain := PortableDependencyStageKey(inputs); got != gotAgain {
		t.Fatalf("portable dependency stage key changed for identical inputs: first %q second %q", got, gotAgain)
	}

	mutated := inputs
	mutated.KeyFilesDigest = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	if gotMutated := PortableDependencyStageKey(mutated); got == gotMutated {
		t.Fatalf("portable dependency stage key did not change after key-file mutation: %q", got)
	}

	mutated = inputs
	mutated.CanonicalRemoteURL = "https://github.com/buildkite/other.git"
	if gotMutated := PortableDependencyStageKey(mutated); got == gotMutated {
		t.Fatalf("portable dependency stage key did not change after repository mutation: %q", got)
	}
}

func TestDependencyVolumeKey(t *testing.T) {
	inputs := DependencyVolumeInputs{
		Backend:                         "firecracker",
		RuntimeKey:                      "runtime:v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ReuseNamespace:                  "https://github.com/buildkite/cleanroom.git",
		CompiledPolicyHash:              "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		DestinationDir:                  "/workspace",
		RepositorySourceDigest:          "sha256:9999999999999999999999999999999999999999999999999999999999999999",
		BlockName:                       "go-modules",
		CommandDigest:                   "sha256:2222222222222222222222222222222222222222222222222222222222222222",
		EnvDigest:                       "sha256:3333333333333333333333333333333333333333333333333333333333333333",
		InputManifestDigest:             "sha256:4444444444444444444444444444444444444444444444444444444444444444",
		NormalizedOutputsDigest:         "sha256:5555555555555555555555555555555555555555555555555555555555555555",
		PriorDependencyOutputKeysDigest: "sha256:6666666666666666666666666666666666666666666666666666666666666666",
		OutputVolumeLayoutVersion:       "aggregate-v1",
		ProducerVersion:                 "cleanroom/dependency-volume-v1",
	}

	got := DependencyVolumeKey(inputs)
	if !strings.HasPrefix(got, "dependency-volume:v1:") {
		t.Fatalf("dependency volume key prefix = %q, want %q", got, "dependency-volume:v1:")
	}
	if gotAgain := DependencyVolumeKey(inputs); got != gotAgain {
		t.Fatalf("dependency volume key changed for identical inputs: first %q second %q", got, gotAgain)
	}

	mutated := inputs
	mutated.ReuseNamespace = "https://github.com/buildkite/other.git"
	if gotMutated := DependencyVolumeKey(mutated); got == gotMutated {
		t.Fatalf("dependency volume key did not change after reuse namespace mutation: %q", got)
	}

	mutated = inputs
	mutated.DestinationDir = "/src"
	if gotMutated := DependencyVolumeKey(mutated); got == gotMutated {
		t.Fatalf("dependency volume key did not change after destination dir mutation: %q", got)
	}

	mutated = inputs
	mutated.RepositorySourceDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if gotMutated := DependencyVolumeKey(mutated); got == gotMutated {
		t.Fatalf("dependency volume key did not change after repository source digest mutation: %q", got)
	}

	mutated = inputs
	mutated.NormalizedOutputsDigest = "sha256:7777777777777777777777777777777777777777777777777777777777777777"
	if gotMutated := DependencyVolumeKey(mutated); got == gotMutated {
		t.Fatalf("dependency volume key did not change after outputs mutation: %q", got)
	}
}

func TestServiceVolumeKey(t *testing.T) {
	inputs := ServiceVolumeInputs{
		Backend:                      "firecracker",
		RuntimeKey:                   "runtime:v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ReuseNamespace:               "https://github.com/buildkite/cleanroom.git",
		CompiledPolicyHash:           "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		DestinationDir:               "/workspace",
		RepositorySourceDigest:       "sha256:9999999999999999999999999999999999999999999999999999999999999999",
		BlockName:                    "postgres",
		CommandDigest:                "sha256:2222222222222222222222222222222222222222222222222222222222222222",
		EnvDigest:                    "sha256:3333333333333333333333333333333333333333333333333333333333333333",
		InputManifestDigest:          "sha256:4444444444444444444444444444444444444444444444444444444444444444",
		NormalizedOutputsDigest:      "sha256:5555555555555555555555555555555555555555555555555555555555555555",
		DependencyOutputKeysDigest:   "sha256:6666666666666666666666666666666666666666666666666666666666666666",
		PriorServiceOutputKeysDigest: "sha256:7777777777777777777777777777777777777777777777777777777777777777",
		OutputVolumeLayoutVersion:    "aggregate-v1",
		ProducerVersion:              "cleanroom/service-volume-v1",
	}

	got := ServiceVolumeKey(inputs)
	if !strings.HasPrefix(got, "service-volume:v1:") {
		t.Fatalf("service volume key prefix = %q, want %q", got, "service-volume:v1:")
	}
	if gotAgain := ServiceVolumeKey(inputs); got != gotAgain {
		t.Fatalf("service volume key changed for identical inputs: first %q second %q", got, gotAgain)
	}

	mutated := inputs
	mutated.DestinationDir = "/src"
	if gotMutated := ServiceVolumeKey(mutated); got == gotMutated {
		t.Fatalf("service volume key did not change after destination dir mutation: %q", got)
	}

	mutated = inputs
	mutated.RepositorySourceDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if gotMutated := ServiceVolumeKey(mutated); got == gotMutated {
		t.Fatalf("service volume key did not change after repository source digest mutation: %q", got)
	}

	mutated = inputs
	mutated.DependencyOutputKeysDigest = "sha256:8888888888888888888888888888888888888888888888888888888888888888"
	if gotMutated := ServiceVolumeKey(mutated); got == gotMutated {
		t.Fatalf("service volume key did not change after dependency output keys mutation: %q", got)
	}
}

func TestReuseNamespaceDefaultsToCanonicalRemote(t *testing.T) {
	if got, want := ReuseNamespace("", " https://github.com/buildkite/cleanroom.git "), "https://github.com/buildkite/cleanroom.git"; got != want {
		t.Fatalf("unexpected default reuse namespace: got %q want %q", got, want)
	}
	if got, want := ReuseNamespace("org/buildkite", "https://github.com/buildkite/cleanroom.git"), "org/buildkite"; got != want {
		t.Fatalf("unexpected explicit reuse namespace: got %q want %q", got, want)
	}
}
