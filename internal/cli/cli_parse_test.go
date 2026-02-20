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
