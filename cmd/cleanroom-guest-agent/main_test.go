//go:build linux

package main

import (
	"reflect"
	"testing"
)

func TestEnsureTTYTERMAddsDefaultWhenMissing(t *testing.T) {
	in := []string{"PATH=/usr/bin"}
	got := ensureTTYTERM(append([]string(nil), in...))
	if !containsEnv(got, "TERM=xterm-256color") {
		t.Fatalf("expected TERM=xterm-256color to be added, got %v", got)
	}
}

func TestEnsureTTYTERMRewritesDumbAndLinux(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
	}{
		{name: "dumb", in: []string{"TERM=dumb", "PATH=/usr/bin"}},
		{name: "linux", in: []string{"PATH=/usr/bin", "TERM=linux"}},
		{name: "empty", in: []string{"TERM=", "PATH=/usr/bin"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ensureTTYTERM(append([]string(nil), tc.in...))
			if !containsEnv(got, "TERM=xterm-256color") {
				t.Fatalf("expected TERM=xterm-256color, got %v", got)
			}
		})
	}
}

func TestEnsureTTYTERMPreservesExplicitTerm(t *testing.T) {
	in := []string{"TERM=screen-256color", "PATH=/usr/bin"}
	got := ensureTTYTERM(append([]string(nil), in...))
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("expected TERM to be preserved, got %v want %v", got, in)
	}
}

func TestSplitEnvEntry(t *testing.T) {
	tests := []struct {
		in    string
		key   string
		value string
		ok    bool
	}{
		{in: "TERM=xterm-256color", key: "TERM", value: "xterm-256color", ok: true},
		{in: "TERM", key: "TERM", value: "", ok: true},
		{in: "=x", ok: false},
		{in: "", ok: false},
	}

	for _, tc := range tests {
		key, value, ok := splitEnvEntry(tc.in)
		if key != tc.key || value != tc.value || ok != tc.ok {
			t.Fatalf("splitEnvEntry(%q) => (%q,%q,%v), want (%q,%q,%v)", tc.in, key, value, ok, tc.key, tc.value, tc.ok)
		}
	}
}

func containsEnv(env []string, entry string) bool {
	for _, current := range env {
		if current == entry {
			return true
		}
	}
	return false
}
