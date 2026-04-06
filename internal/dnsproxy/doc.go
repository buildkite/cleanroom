// Package dnsproxy implements a backend-neutral DNS observation runtime.
//
// The runtime records DNS answers per sandbox and source IP, preserves
// effective TTL semantics across CNAME chains, and exposes connection checks
// that can distinguish new flows from already-established ones.
package dnsproxy
