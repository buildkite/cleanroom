package submodule

import (
	"strings"
	"testing"
)

func TestParseGitmodules(t *testing.T) {
	data := []byte(`
[submodule "vendor/emojis"]
    path = vendor/emojis
    url = https://github.com/example/emojis
    branch = main
`)
	entries, err := ParseGitmodules(data)
	if err != nil {
		t.Fatalf("ParseGitmodules returned error: %v", err)
	}
	if got, want := len(entries), 1; got != want {
		t.Fatalf("expected %d entries, got %d", want, got)
	}
	if got, want := entries[0].Name, "vendor/emojis"; got != want {
		t.Errorf("Name: got %q want %q", got, want)
	}
	if got, want := entries[0].Path, "vendor/emojis"; got != want {
		t.Errorf("Path: got %q want %q", got, want)
	}
	if got, want := entries[0].URL, "https://github.com/example/emojis"; got != want {
		t.Errorf("URL: got %q want %q", got, want)
	}
}

func TestParseGitmodulesMultipleEntries(t *testing.T) {
	data := []byte(`
[submodule "alpha"]
    path = alpha
    url = https://github.com/example/alpha

[submodule "beta"]
    path = beta
    url = https://github.com/example/beta
`)
	entries, err := ParseGitmodules(data)
	if err != nil {
		t.Fatalf("ParseGitmodules returned error: %v", err)
	}
	if got, want := len(entries), 2; got != want {
		t.Fatalf("expected %d entries, got %d", want, got)
	}
	if got, want := entries[0].Name, "alpha"; got != want {
		t.Errorf("first Name: got %q want %q", got, want)
	}
	if got, want := entries[1].Name, "beta"; got != want {
		t.Errorf("second Name: got %q want %q", got, want)
	}
}

func TestParseGitmodulesNameWithSpaces(t *testing.T) {
	data := []byte(`
[submodule "some module with spaces"]
    path = some/module
    url = https://github.com/example/module
`)
	entries, err := ParseGitmodules(data)
	if err != nil {
		t.Fatalf("ParseGitmodules returned error: %v", err)
	}
	if got, want := len(entries), 1; got != want {
		t.Fatalf("expected %d entries, got %d", want, got)
	}
	if got, want := entries[0].Name, "some module with spaces"; got != want {
		t.Errorf("Name: got %q want %q", got, want)
	}
}

func TestParseGitmodulesMissingURL(t *testing.T) {
	data := []byte(`
[submodule "foo"]
    path = foo
`)
	_, err := ParseGitmodules(data)
	if err == nil {
		t.Fatal("expected error for missing url")
	}
	if !strings.Contains(err.Error(), "missing url") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseGitmodulesMissingPath(t *testing.T) {
	data := []byte(`
[submodule "foo"]
    url = https://github.com/example/foo
`)
	_, err := ParseGitmodules(data)
	if err == nil {
		t.Fatal("expected error for missing path")
	}
	if !strings.Contains(err.Error(), "missing path") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseGitmodulesIgnoresComments(t *testing.T) {
	data := []byte(`
# this is a comment
[submodule "foo"]
    # another comment
    path = foo
    url = https://github.com/example/foo
`)
	entries, err := ParseGitmodules(data)
	if err != nil {
		t.Fatalf("ParseGitmodules returned error: %v", err)
	}
	if got, want := len(entries), 1; got != want {
		t.Fatalf("expected %d entries, got %d", want, got)
	}
}

func TestParseGitmodulesIgnoresBlankLines(t *testing.T) {
	data := []byte(`

[submodule "foo"]

    path = foo

    url = https://github.com/example/foo

`)
	entries, err := ParseGitmodules(data)
	if err != nil {
		t.Fatalf("ParseGitmodules returned error: %v", err)
	}
	if got, want := len(entries), 1; got != want {
		t.Fatalf("expected %d entries, got %d", want, got)
	}
}

func TestParseGitmodulesDuplicatePath(t *testing.T) {
	data := []byte(`
[submodule "foo"]
    path = shared/path
    url = https://github.com/example/foo

[submodule "bar"]
    path = shared/path
    url = https://github.com/example/bar
`)
	_, err := ParseGitmodules(data)
	if err == nil {
		t.Fatal("expected error for duplicate path")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseGitmodulesIgnoresUnknownKeys(t *testing.T) {
	data := []byte(`
[submodule "foo"]
    path = foo
    url = https://github.com/example/foo
    branch = main
    update = rebase
    fetchRecurseSubmodules = true
`)
	entries, err := ParseGitmodules(data)
	if err != nil {
		t.Fatalf("ParseGitmodules returned error: %v", err)
	}
	if got, want := len(entries), 1; got != want {
		t.Fatalf("expected %d entries, got %d", want, got)
	}
}
