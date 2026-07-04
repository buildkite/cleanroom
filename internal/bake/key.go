package bake

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/buildkite/cleanroom/internal/policy"
)

// keyVersion is bumped whenever the bake key input set changes, so old
// artifacts never false-match a new key scheme.
const keyVersion = 1

// Key computes the bake cache key: pinned inputs whose equality means a
// rebake would produce an equivalent artifact. The policy hash covers image
// ref, network rules, resources, and warmup steps; git facts pin the
// workspace revision. Dirty worktrees are recorded but never treated as
// cache-equivalent, because dirty content is not hashed.
func Key(compiled *policy.CompiledPolicy, facts GitFacts) string {
	payload, err := json.Marshal(struct {
		Version    int    `json:"version"`
		PolicyHash string `json:"policy_hash"`
		ImageRef   string `json:"image_ref"`
		Commit     string `json:"commit"`
		Dirty      bool   `json:"dirty"`
	}{
		Version:    keyVersion,
		PolicyHash: compiled.Hash,
		ImageRef:   compiled.ImageRef,
		Commit:     facts.Commit,
		Dirty:      facts.Dirty,
	})
	if err != nil {
		// Marshaling a struct of strings and bools cannot fail.
		panic(err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
