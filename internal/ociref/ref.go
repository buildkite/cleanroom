package ociref

import (
	"fmt"
	"regexp"
	"strings"
)

var sha256HexPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type DigestReference struct {
	Original        string
	Repository      string
	DigestAlgorithm string
	DigestHex       string
}

func (r DigestReference) Digest() string {
	return r.DigestAlgorithm + ":" + r.DigestHex
}

// ParseDigestReference validates digest-pinned OCI references with the form:
// repo/image@sha256:<64-hex>
func ParseDigestReference(raw string) (DigestReference, error) {
	ref := strings.TrimSpace(raw)
	if ref == "" {
		return DigestReference{}, fmt.Errorf("reference must be digest-pinned (for example repo/image@sha256:<digest>)")
	}

	repo, digestPart, ok := strings.Cut(ref, "@")
	if !ok {
		return DigestReference{}, fmt.Errorf("reference %q is not digest-pinned (expected repo/image@sha256:<digest>)", ref)
	}
	repo = strings.TrimSpace(repo)
	digestPart = strings.TrimSpace(digestPart)
	if repo == "" {
		return DigestReference{}, fmt.Errorf("reference %q has an empty repository", ref)
	}
	if strings.ContainsAny(repo, " \t\n\r") {
		return DigestReference{}, fmt.Errorf("reference %q contains whitespace in repository", ref)
	}

	algo, hexPart, ok := strings.Cut(strings.ToLower(digestPart), ":")
	if !ok || algo != "sha256" || !sha256HexPattern.MatchString(hexPart) {
		return DigestReference{}, fmt.Errorf("reference %q must include sha256 digest with 64 lowercase hex characters", ref)
	}

	return DigestReference{
		Original:        ref,
		Repository:      repo,
		DigestAlgorithm: algo,
		DigestHex:       hexPart,
	}, nil
}
