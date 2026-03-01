package cli

import (
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

func newParserForTest(t *testing.T, c *CLI) *kong.Kong {
	t.Helper()

	parser, err := kong.New(
		c,
		kong.Name("cleanroom"),
		kong.Description("Cleanroom CLI (MVP)"),
	)
	if err != nil {
		t.Fatalf("create parser: %v", err)
	}
	return parser
}

func TestConsoleCommandAllowsNoCommandArgs(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"console"}); err != nil {
		t.Fatalf("parse console with no command returned error: %v", err)
	}
	if got := len(c.Console.Command); got != 0 {
		t.Fatalf("expected no explicit command args, got %v", c.Console.Command)
	}
}

func TestExecCommandStillRequiresArgs(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	_, err := parser.Parse([]string{"exec"})
	if err == nil {
		t.Fatal("expected parse error for missing exec command")
	}
	if !strings.Contains(err.Error(), "<command>") {
		t.Fatalf("expected missing command parse error, got %v", err)
	}
}

func TestImagePullRequiresRef(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	_, err := parser.Parse([]string{"image", "pull"})
	if err == nil {
		t.Fatal("expected parse error for missing image ref")
	}
	if !strings.Contains(err.Error(), "<ref>") {
		t.Fatalf("expected missing ref parse error, got %v", err)
	}
}

func TestImageImportAllowsOptionalTarPath(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"image", "import", "ghcr.io/org/base@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatalf("parse image import without tar path returned error: %v", err)
	}
}

func TestImageListAliasParses(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"image", "ls"}); err != nil {
		t.Fatalf("parse image ls returned error: %v", err)
	}
}

func TestImageBumpRefAllowsNoArgs(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"image", "bump-ref"}); err != nil {
		t.Fatalf("parse image bump-ref with default ref returned error: %v", err)
	}
}

func TestConfigInitParses(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"config", "init"}); err != nil {
		t.Fatalf("parse config init returned error: %v", err)
	}
}

func TestServeCommandParsesWithoutAction(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"serve"}); err != nil {
		t.Fatalf("parse serve returned error: %v", err)
	}
	if got := c.Serve.Action; got != "" {
		t.Fatalf("expected empty serve action, got %q", got)
	}
}

func TestServeInstallParses(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"serve", "install"}); err != nil {
		t.Fatalf("parse serve install returned error: %v", err)
	}
	if got := c.Serve.Action; got != "install" {
		t.Fatalf("expected serve action install, got %q", got)
	}
}

func TestServeInstallForceParses(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"serve", "install", "--force"}); err != nil {
		t.Fatalf("parse serve install --force returned error: %v", err)
	}
	if got := c.Serve.Action; got != "install" {
		t.Fatalf("expected serve action install, got %q", got)
	}
	if !c.Serve.Force {
		t.Fatal("expected --force to set Serve.Force")
	}
}
