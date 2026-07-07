package cli

import (
	"reflect"
	"testing"

	"github.com/alecthomas/kong"
)

func newParserForTest(t *testing.T, c *CLI) *kong.Kong {
	t.Helper()

	parser, err := kong.New(
		c,
		kong.Name("cleanroom"),
		kong.Description("Cleanroom CLI"),
	)
	if err != nil {
		t.Fatalf("create parser: %v", err)
	}
	return parser
}

func TestTopLevelCommandsAreTheBakeSurface(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	got := map[string]bool{}
	for _, command := range parser.Model.Children {
		if command.Name == "help" {
			continue
		}
		got[command.Name] = true
	}
	want := map[string]bool{
		"policy":        true,
		"compile":       true,
		"stamp":         true,
		"bake":          true,
		"verify":        true,
		"run":           true,
		"content-cache": true,
		"gateway":       true,
		"version":       true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("top-level commands = %#v, want %#v", got, want)
	}
}

func TestBakeStageCommandsParse(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)
	if _, err := parser.Parse([]string{"compile", "./repo"}); err != nil {
		t.Fatalf("parse compile returned error: %v", err)
	}
	if got, want := c.Compile.Dir, "./repo"; got != want {
		t.Fatalf("unexpected compile dir: got %q want %q", got, want)
	}

	c = &CLI{}
	parser = newParserForTest(t, c)
	if _, err := parser.Parse([]string{"stamp"}); err != nil {
		t.Fatalf("parse stamp returned error: %v", err)
	}
	if got, want := c.Stamp.Dir, "."; got != want {
		t.Fatalf("unexpected stamp dir: got %q want %q", got, want)
	}
}

func TestBakeCommandParses(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)
	if _, err := parser.Parse([]string{"bake", "./repo", "--out", "./repo.spore", "--gateway-socket", "/tmp/gw.sock"}); err != nil {
		t.Fatalf("parse bake returned error: %v", err)
	}
	if got, want := c.Bake.Dir, "./repo"; got != want {
		t.Fatalf("unexpected bake dir: got %q want %q", got, want)
	}
	if got, want := c.Bake.Out, "./repo.spore"; got != want {
		t.Fatalf("unexpected bake out: got %q want %q", got, want)
	}
	if got, want := c.Bake.GatewaySocket, "/tmp/gw.sock"; got != want {
		t.Fatalf("unexpected bake gateway socket: got %q want %q", got, want)
	}
	if got, want := c.Bake.Spore, "spore"; got != want {
		t.Fatalf("unexpected bake spore default: got %q want %q", got, want)
	}
}

func TestVerifyCommandParses(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)
	if _, err := parser.Parse([]string{"verify", "./repo.spore", "--dir", "./repo", "--json"}); err != nil {
		t.Fatalf("parse verify returned error: %v", err)
	}
	if got, want := c.Verify.SporeDir, "./repo.spore"; got != want {
		t.Fatalf("unexpected verify spore dir: got %q want %q", got, want)
	}
	if got, want := c.Verify.Dir, "./repo"; got != want {
		t.Fatalf("unexpected verify dir: got %q want %q", got, want)
	}
	if !c.Verify.JSON {
		t.Fatal("expected verify --json flag to be set")
	}

	c = &CLI{}
	parser = newParserForTest(t, c)
	if _, err := parser.Parse([]string{"verify"}); err != nil {
		t.Fatalf("parse bare verify returned error: %v", err)
	}
	if c.Verify.SporeDir != "" {
		t.Fatalf("unexpected default spore dir: %q", c.Verify.SporeDir)
	}
}

func TestRunCommandParses(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)
	if _, err := parser.Parse([]string{"run", "./repo.spore", "--dir", "./repo", "--grants", "./gateway.yaml", "--", "make", "test"}); err != nil {
		t.Fatalf("parse run returned error: %v", err)
	}
	if got, want := c.Run.SporeDir, "./repo.spore"; got != want {
		t.Fatalf("unexpected run spore dir: got %q want %q", got, want)
	}
	if got, want := c.Run.Dir, "./repo"; got != want {
		t.Fatalf("unexpected run dir: got %q want %q", got, want)
	}
	if got, want := c.Run.Grants, "./gateway.yaml"; got != want {
		t.Fatalf("unexpected run grants: got %q want %q", got, want)
	}
	if got, want := c.Run.Spore, "spore"; got != want {
		t.Fatalf("unexpected run spore default: got %q want %q", got, want)
	}
	if got, want := cleanPassthroughArgv(c.Run.Argv), []string{"make", "test"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected run argv: got %#v want %#v", got, want)
	}
	if _, err := newParserForTest(t, &CLI{}).Parse([]string{"run", "./repo.spore", "--", "make", "test"}); err == nil {
		t.Fatal("expected parse error when --dir is missing")
	}
}

func TestContentCacheServeParses(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)
	if _, err := parser.Parse([]string{
		"content-cache", "serve",
		"--listen", "127.0.0.1:9999",
		"--storage", "/tmp/cache",
		"--git-allowed-hosts", "github.com,gitlab.com",
		"--fetch-allowed-hosts", "dl.google.com,releases.hashicorp.com",
	}); err != nil {
		t.Fatalf("parse content-cache serve returned error: %v", err)
	}
	if got, want := c.Content.Serve.Listen, "127.0.0.1:9999"; got != want {
		t.Fatalf("unexpected listen: got %q want %q", got, want)
	}
	if got, want := c.Content.Serve.Storage, "/tmp/cache"; got != want {
		t.Fatalf("unexpected storage: got %q want %q", got, want)
	}
	if got, want := c.Content.Serve.GitAllowedHosts, []string{"github.com", "gitlab.com"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected git hosts: got %#v want %#v", got, want)
	}
	if got, want := c.Content.Serve.FetchAllowedHosts, []string{"dl.google.com", "releases.hashicorp.com"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected fetch hosts: got %#v want %#v", got, want)
	}
}

func TestGatewayServeParses(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)
	if _, err := parser.Parse([]string{"gateway", "serve", "--for", "./repo.spore", "--dir", ".", "--socket", "/tmp/gw.sock", "--grants", "./gw.yaml"}); err != nil {
		t.Fatalf("parse gateway serve returned error: %v", err)
	}
	if got, want := c.Gateway.Serve.For, "./repo.spore"; got != want {
		t.Fatalf("unexpected gateway for: got %q want %q", got, want)
	}
	if got, want := c.Gateway.Serve.Dir, "."; got != want {
		t.Fatalf("unexpected gateway dir: got %q want %q", got, want)
	}
	// --dir is the trust root for grants; serving a spore without it must
	// fail at parse time.
	if _, err := newParserForTest(t, &CLI{}).Parse([]string{"gateway", "serve", "--for", "./repo.spore", "--socket", "/tmp/gw.sock"}); err == nil {
		t.Fatal("expected parse error when --dir is missing")
	}
	if got, want := c.Gateway.Serve.Socket, "/tmp/gw.sock"; got != want {
		t.Fatalf("unexpected gateway socket: got %q want %q", got, want)
	}
	if got, want := c.Gateway.Serve.Grants, "./gw.yaml"; got != want {
		t.Fatalf("unexpected gateway grants: got %q want %q", got, want)
	}
}

func TestPolicyValidateParsesChdir(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)
	if _, err := parser.Parse([]string{"policy", "validate", "--chdir", "./repo", "--json"}); err != nil {
		t.Fatalf("parse policy validate returned error: %v", err)
	}
	if got, want := c.Policy.Validate.Chdir, "./repo"; got != want {
		t.Fatalf("unexpected policy chdir: got %q want %q", got, want)
	}
	if !c.Policy.Validate.JSON {
		t.Fatal("expected policy validate --json flag to be set")
	}
}
