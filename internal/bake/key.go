package bake

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/buildkite/cleanroom/internal/policy"
)

// keyVersion is bumped whenever the bake key input set changes, so old
// artifacts never false-match a new key scheme.
const keyVersion = 2

// Key computes the bake cache key: pinned inputs whose equality means a
// rebake would produce an equivalent artifact. The policy hash covers image
// ref, network rules, resources, and warmup steps; git facts pin the
// workspace revision and remote. The remote is part of the key because
// gateway grants match on it: changing origin must invalidate the artifact
// rather than leave stale remote provenance eligible for grants. Dirty
// worktrees are recorded but never treated as cache-equivalent, because
// dirty content is not hashed.
func Key(compiled *policy.CompiledPolicy, facts GitFacts) string {
	payload, err := json.Marshal(struct {
		Version    int    `json:"version"`
		PolicyHash string `json:"policy_hash"`
		ImageRef   string `json:"image_ref"`
		Commit     string `json:"commit"`
		Remote     string `json:"remote"`
		Dirty      bool   `json:"dirty"`
	}{
		Version:    keyVersion,
		PolicyHash: compiled.Hash,
		ImageRef:   compiled.ImageRef,
		Commit:     facts.Commit,
		Remote:     facts.Remote,
		Dirty:      facts.Dirty,
	})
	if err != nil {
		// Marshaling a struct of strings and bools cannot fail.
		panic(err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
