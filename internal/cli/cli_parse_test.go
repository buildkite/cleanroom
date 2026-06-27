package cli

import (
	"bytes"
	"errors"
	"reflect"
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

func TestExecCommandParsesNameAndCommand(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"exec", "worker", "--", "echo", "hi"}); err != nil {
		t.Fatalf("parse exec returned error: %v", err)
	}
	if got, want := c.Exec.Name, "worker"; got != want {
		t.Fatalf("unexpected exec name: got %q want %q", got, want)
	}
	if got, want := trimPassthroughSeparator(c.Exec.Command), []string{"echo", "hi"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected exec command: got %v want %v", got, want)
	}
}

func TestExecCommandRejectsRemovedClientLogLevelFlag(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	_, err := parser.Parse([]string{"exec", "--log-level", "debug", "--", "echo", "hello"})
	if err == nil {
		t.Fatal("expected parse error for removed client --log-level flag")
	}
	if !strings.Contains(err.Error(), "unknown flag") || !strings.Contains(err.Error(), "--log-level") {
		t.Fatalf("expected unknown flag parse error, got %v", err)
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

func TestImageResolveCommandParses(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"image", "resolve", "ghcr.io/buildkite/cleanroom-base/alpine:latest"}); err != nil {
		t.Fatalf("parse image resolve returned error: %v", err)
	}
	if got, want := c.Image.Resolve.Ref, "ghcr.io/buildkite/cleanroom-base/alpine:latest"; got != want {
		t.Fatalf("unexpected image resolve ref: got %q want %q", got, want)
	}
}

func TestImageResolveCommandAllowsNoArgs(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"image", "resolve"}); err != nil {
		t.Fatalf("parse image resolve with default ref returned error: %v", err)
	}
}

func TestConfigInitParses(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"config", "init"}); err != nil {
		t.Fatalf("parse config init returned error: %v", err)
	}
}

func TestConfigValidateParses(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"config", "validate", "--path", "./config.yaml", "--json"}); err != nil {
		t.Fatalf("parse config validate returned error: %v", err)
	}
	if got, want := c.Config.Validate.Path, "./config.yaml"; got != want {
		t.Fatalf("unexpected config validate path: got %q want %q", got, want)
	}
	if !c.Config.Validate.JSON {
		t.Fatal("expected config validate --json flag to be set")
	}
}

func TestAuthCheckParses(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"auth", "check", "--config", "./config.yaml", "--policy-file", "./auth-policy.yaml", "--token-file", "./token.jwt", "--action", "sandbox.create", "--request", "./request.json", "--json"}); err != nil {
		t.Fatalf("parse auth check returned error: %v", err)
	}
	if got, want := c.Auth.Check.Config, "./config.yaml"; got != want {
		t.Fatalf("unexpected auth check config: got %q want %q", got, want)
	}
	if got, want := c.Auth.Check.PolicyFile, "./auth-policy.yaml"; got != want {
		t.Fatalf("unexpected auth check policy file: got %q want %q", got, want)
	}
	if got, want := c.Auth.Check.TokenFile, "./token.jwt"; got != want {
		t.Fatalf("unexpected auth check token file: got %q want %q", got, want)
	}
	if got, want := c.Auth.Check.Action, "sandbox.create"; got != want {
		t.Fatalf("unexpected auth check action: got %q want %q", got, want)
	}
	if got, want := c.Auth.Check.Request, "./request.json"; got != want {
		t.Fatalf("unexpected auth check request: got %q want %q", got, want)
	}
	if !c.Auth.Check.JSON {
		t.Fatal("expected auth check --json flag to be set")
	}
}

func TestSnapshotInspectParses(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"snapshot", "inspect", "snap_123"}); err != nil {
		t.Fatalf("parse snapshot inspect returned error: %v", err)
	}
}

func TestTopLevelInspectParses(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"inspect", "exec_123", "--json"}); err != nil {
		t.Fatalf("parse inspect returned error: %v", err)
	}
	if got, want := c.Inspect.ID, "exec_123"; got != want {
		t.Fatalf("unexpected inspect id: got %q want %q", got, want)
	}
	if !c.Inspect.JSON {
		t.Fatal("expected inspect --json flag to be set")
	}
}

func TestSandboxCreateParses(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"sandbox", "create"}); err != nil {
		t.Fatalf("parse sandbox create returned error: %v", err)
	}
}

func TestTopLevelLifecycleCommandsParse(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)
	if _, err := parser.Parse([]string{"create", "--wait", "worker", "."}); err != nil {
		t.Fatalf("parse create returned error: %v", err)
	}
	if got, want := c.Create.Name, "worker"; got != want {
		t.Fatalf("unexpected create name: got %q want %q", got, want)
	}
	if got, want := c.Create.Dir, "."; got != want {
		t.Fatalf("unexpected create dir: got %q want %q", got, want)
	}
	if !c.Create.Wait {
		t.Fatal("expected create --wait flag to be set")
	}

	c = &CLI{}
	parser = newParserForTest(t, c)
	if _, err := parser.Parse([]string{"exec", "worker", "--", "echo", "hi"}); err != nil {
		t.Fatalf("parse exec returned error: %v", err)
	}
	if got, want := c.Exec.Name, "worker"; got != want {
		t.Fatalf("unexpected exec name: got %q want %q", got, want)
	}
	if got, want := trimPassthroughSeparator(c.Exec.Command), []string{"echo", "hi"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected exec command: got %v want %v", got, want)
	}

	c = &CLI{}
	parser = newParserForTest(t, c)
	if _, err := parser.Parse([]string{"capture", "worker", "--out", "./test.spore"}); err != nil {
		t.Fatalf("parse capture returned error: %v", err)
	}
	if got, want := c.Capture.Out, "./test.spore"; got != want {
		t.Fatalf("unexpected capture out: got %q want %q", got, want)
	}

	c = &CLI{}
	parser = newParserForTest(t, c)
	if _, err := parser.Parse([]string{"resume", "./test.spore", "--name", "restored"}); err != nil {
		t.Fatalf("parse resume returned error: %v", err)
	}
	if got, want := c.Resume.SporeDir, "./test.spore"; got != want {
		t.Fatalf("unexpected resume spore dir: got %q want %q", got, want)
	}
	if got, want := c.Resume.Name, "restored"; got != want {
		t.Fatalf("unexpected resume name: got %q want %q", got, want)
	}

	c = &CLI{}
	parser = newParserForTest(t, c)
	if _, err := parser.Parse([]string{"terminate", "worker"}); err != nil {
		t.Fatalf("parse terminate returned error: %v", err)
	}
	if got, want := c.Destroy.Name, "worker"; got != want {
		t.Fatalf("unexpected destroy name: got %q want %q", got, want)
	}
}

func TestNamedLifecycleCommandsBypassStartupRuntimeConfig(t *testing.T) {
	for _, args := range [][]string{
		{"create", "worker"},
		{"exec", "worker", "--", "true"},
		{"capture", "worker", "--out", "./test.spore"},
		{"resume", "./test.spore", "--name", "restored"},
		{"destroy", "worker"},
		{"terminate", "worker"},
		{"rm", "worker"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			c := &CLI{}
			parser := newParserForTest(t, c)
			ctx, err := parser.Parse(args)
			if err != nil {
				t.Fatalf("parse returned error: %v", err)
			}
			if !commandBypassesStartupRuntimeConfig(ctx) {
				t.Fatalf("expected %q to bypass startup runtime config", ctx.Command())
			}
		})
	}
}

func TestSystemCommandsParse(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"system", "df", "--json"}); err != nil {
		t.Fatalf("parse system df returned error: %v", err)
	}
	if !c.System.DF.JSON {
		t.Fatal("expected system df --json flag to be set")
	}

	c = &CLI{}
	parser = newParserForTest(t, c)
	if _, err := parser.Parse([]string{"system", "prune", "--dry-run", "--all", "--table", "--json", "--older-than", "7d"}); err != nil {
		t.Fatalf("parse system prune returned error: %v", err)
	}
	if !c.System.Prune.DryRun {
		t.Fatal("expected system prune --dry-run flag to be set")
	}
	if !c.System.Prune.All {
		t.Fatal("expected system prune --all flag to be set")
	}
	if !c.System.Prune.Table {
		t.Fatal("expected system prune --table flag to be set")
	}
	if !c.System.Prune.JSON {
		t.Fatal("expected system prune --json flag to be set")
	}
	if got, want := c.System.Prune.OlderThan, "7d"; got != want {
		t.Fatalf("unexpected older-than: got %q want %q", got, want)
	}

	c = &CLI{}
	parser = newParserForTest(t, c)
	if _, err := parser.Parse([]string{"system", "warmup", "--backend", "darwin-vz", "--json"}); err != nil {
		t.Fatalf("parse system warmup returned error: %v", err)
	}
	if got, want := c.System.Warmup.Backend, "darwin-vz"; got != want {
		t.Fatalf("unexpected warmup backend: got %q want %q", got, want)
	}
	if !c.System.Warmup.JSON {
		t.Fatal("expected system warmup --json flag to be set")
	}
}

func TestSandboxCreateParsesExposureFlags(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"sandbox", "create", "--expose", "15432:5432", "--expose-https", "buildkite:3000"}); err != nil {
		t.Fatalf("parse sandbox create exposure flags returned error: %v", err)
	}
	if got, want := c.Sandbox.Create.Expose, []string{"15432:5432"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected sandbox create --expose values: got %v want %v", got, want)
	}
	if got, want := c.Sandbox.Create.ExposeHTTPS, []string{"buildkite:3000"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected sandbox create --expose-https values: got %v want %v", got, want)
	}
}

func TestExposeParsesExposureFlags(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"expose", "--in", "sandbox123", "--expose", "15432:5432", "--expose-https", "buildkite:3000"}); err != nil {
		t.Fatalf("parse expose returned error: %v", err)
	}
	if got, want := c.Expose.SandboxID, "sandbox123"; got != want {
		t.Fatalf("unexpected expose sandbox id: got %q want %q", got, want)
	}
	if got, want := c.Expose.Expose, []string{"15432:5432"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected expose --expose values: got %v want %v", got, want)
	}
	if got, want := c.Expose.ExposeHTTPS, []string{"buildkite:3000"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected expose --expose-https values: got %v want %v", got, want)
	}
}

func TestPortForwardParsesSpecs(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"port-forward", "--in", "sandbox123", "3000", "15432:5432"}); err != nil {
		t.Fatalf("parse port-forward returned error: %v", err)
	}
	if got, want := c.PortForward.SandboxID, "sandbox123"; got != want {
		t.Fatalf("unexpected port-forward sandbox id: got %q want %q", got, want)
	}
	if got, want := c.PortForward.Specs, []string{"3000", "15432:5432"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected port-forward specs: got %v want %v", got, want)
	}
}

func TestSandboxListParsesAllFlag(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"sandbox", "ls", "--all"}); err != nil {
		t.Fatalf("parse sandbox ls --all returned error: %v", err)
	}
	if !c.Sandbox.List.All {
		t.Fatal("expected sandbox list all flag to be set")
	}
}

func TestSandboxInspectParsesLastFlag(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"sandbox", "inspect", "--last"}); err != nil {
		t.Fatalf("parse sandbox inspect --last returned error: %v", err)
	}
	if !c.Sandbox.Inspect.Last {
		t.Fatal("expected sandbox inspect last flag to be set")
	}
}

func TestExecutionListParsesAllFlag(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"execution", "ls", "--all"}); err != nil {
		t.Fatalf("parse execution ls --all returned error: %v", err)
	}
	if !c.Execution.List.All {
		t.Fatal("expected execution list all flag to be set")
	}
}

func TestExecutionInspectParsesLastWithoutSandboxID(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"execution", "inspect", "--last"}); err != nil {
		t.Fatalf("parse execution inspect --last returned error: %v", err)
	}
	if !c.Execution.Inspect.Last {
		t.Fatal("expected execution inspect last flag to be set")
	}
}

func TestSandboxCreateParsesImageOverride(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	imageRef := "ghcr.io/buildkite/cleanroom-base/alpine@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	if _, err := parser.Parse([]string{"sandbox", "create", "--image", imageRef}); err != nil {
		t.Fatalf("parse sandbox create --image returned error: %v", err)
	}
	if got, want := c.Sandbox.Create.Image, imageRef; got != want {
		t.Fatalf("unexpected sandbox create image override: got %q want %q", got, want)
	}
}

func TestSandboxCreateParsesFromSnapshot(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"sandbox", "create", "--from", "snap_123"}); err != nil {
		t.Fatalf("parse sandbox create --from returned error: %v", err)
	}
	if got, want := c.Sandbox.Create.From, "snap_123"; got != want {
		t.Fatalf("unexpected sandbox create from: got %q want %q", got, want)
	}
}

func TestSandboxCreateParsesDangerouslyAllowAll(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"sandbox", "create", "--dangerously-allow-all"}); err != nil {
		t.Fatalf("parse sandbox create --dangerously-allow-all returned error: %v", err)
	}
	if !c.Sandbox.Create.DangerouslyAllowAll {
		t.Fatal("expected sandbox create dangerously-allow-all flag to be set")
	}
}

func TestSandboxCreateParsesDocker(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"sandbox", "create", "--docker"}); err != nil {
		t.Fatalf("parse sandbox create --docker returned error: %v", err)
	}
	if !c.Sandbox.Create.Docker {
		t.Fatal("expected sandbox create docker flag to be set")
	}
}

func TestSandboxCreateRejectsChdirFlag(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	_, err := parser.Parse([]string{"sandbox", "create", "--chdir", "."})
	if err == nil {
		t.Fatal("expected sandbox create --chdir to be rejected")
	}
	if !strings.Contains(err.Error(), "--chdir") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTopLevelCreateParses(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"create", "worker"}); err != nil {
		t.Fatalf("parse create returned error: %v", err)
	}
	if got, want := c.Create.Name, "worker"; got != want {
		t.Fatalf("unexpected create name: got %q want %q", got, want)
	}
}

func TestConsoleParsesCopyFlags(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)
	if _, err := parser.Parse([]string{"console", "--copy-in", "--copy-out", "--sync"}); err != nil {
		t.Fatalf("parse console copy flags returned error: %v", err)
	}
	if !c.Console.CopyIn || !c.Console.CopyOut || !c.Console.Sync {
		t.Fatalf("expected console copy flags to be set, got %+v", c.Console.workspaceCopyFlags)
	}
}

func TestWorkspaceCopyInParses(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"workspace", "copy-in", "--dry-run", "cr_123"}); err != nil {
		t.Fatalf("parse workspace copy-in returned error: %v", err)
	}
	if got, want := c.Workspace.CopyIn.SandboxID, "cr_123"; got != want {
		t.Fatalf("unexpected workspace copy-in sandbox id: got %q want %q", got, want)
	}
	if !c.Workspace.CopyIn.DryRun {
		t.Fatal("expected workspace copy-in dry-run flag to be set")
	}
}

func TestWorkspaceCopyOutAndDiffParse(t *testing.T) {
	t.Run("copy-out", func(t *testing.T) {
		c := &CLI{}
		parser := newParserForTest(t, c)
		if _, err := parser.Parse([]string{"workspace", "copy-out", "--dry-run", "--force", "cr_123"}); err != nil {
			t.Fatalf("parse workspace copy-out returned error: %v", err)
		}
		if got, want := c.Workspace.CopyOut.SandboxID, "cr_123"; got != want {
			t.Fatalf("unexpected workspace copy-out sandbox id: got %q want %q", got, want)
		}
		if !c.Workspace.CopyOut.DryRun {
			t.Fatal("expected workspace copy-out dry-run flag to be set")
		}
		if !c.Workspace.CopyOut.Force {
			t.Fatal("expected workspace copy-out force flag to be set")
		}
	})

	t.Run("diff", func(t *testing.T) {
		c := &CLI{}
		parser := newParserForTest(t, c)
		if _, err := parser.Parse([]string{"workspace", "diff", "cr_123"}); err != nil {
			t.Fatalf("parse workspace diff returned error: %v", err)
		}
		if got, want := c.Workspace.Diff.SandboxID, "cr_123"; got != want {
			t.Fatalf("unexpected workspace diff sandbox id: got %q want %q", got, want)
		}
	})
}

func TestIncludeLocalChangesFlagRejected(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	_, err := parser.Parse([]string{"create", "--include-local-changes"})
	if err == nil {
		t.Fatal("expected removed --include-local-changes flag to be rejected")
	}
	if !strings.Contains(err.Error(), "unknown flag") || !strings.Contains(err.Error(), "--include-local-changes") {
		t.Fatalf("expected unknown flag parse error, got %v", err)
	}
}

func TestExecRejectsOldControlPlaneFlags(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	_, err := parser.Parse([]string{"exec", "--in", "cr_123", "--", "echo", "ok"})
	if err == nil {
		t.Fatal("expected exec --in to be rejected")
	}
	if !strings.Contains(err.Error(), "--in") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConsoleParsesImageOverride(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	imageRef := "ghcr.io/buildkite/cleanroom-base/alpine@sha256:4444444444444444444444444444444444444444444444444444444444444444"
	if _, err := parser.Parse([]string{"console", "--image", imageRef, "--", "sh"}); err != nil {
		t.Fatalf("parse console --image returned error: %v", err)
	}
	if got, want := c.Console.Image, imageRef; got != want {
		t.Fatalf("unexpected console image override: got %q want %q", got, want)
	}
}

func TestConsoleParsesDangerouslyAllowAll(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"console", "--dangerously-allow-all"}); err != nil {
		t.Fatalf("parse console --dangerously-allow-all returned error: %v", err)
	}
	if !c.Console.DangerouslyAllowAll {
		t.Fatal("expected console dangerously-allow-all flag to be set")
	}
}

func TestConsoleParsesRepeatedEnvFlags(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"console", "--env", "OPENAI_API_KEY", "--env", "DEBUG=1", "--", "sh"}); err != nil {
		t.Fatalf("parse console --env returned error: %v", err)
	}
	if got, want := c.Console.Env, []string{"OPENAI_API_KEY", "DEBUG=1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected console env flags: got %v want %v", got, want)
	}
}

func TestConsoleParsesExposureFlags(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"console", "--expose", "5432", "--expose-https", "buildkite:3000", "--", "sh"}); err != nil {
		t.Fatalf("parse console exposure flags returned error: %v", err)
	}
	if got, want := c.Console.Expose, []string{"5432"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected console --expose values: got %v want %v", got, want)
	}
	if got, want := c.Console.ExposeHTTPS, []string{"buildkite:3000"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected console --expose-https values: got %v want %v", got, want)
	}
	if got, want := trimPassthroughSeparator(c.Console.Command), []string{"sh"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected console command: got %v want %v", got, want)
	}
}

func TestConsoleParsesInFromAndKeep(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"console", "--in", "cr_123"}); err != nil {
		t.Fatalf("parse console --in returned error: %v", err)
	}
	if got, want := c.Console.In, "cr_123"; got != want {
		t.Fatalf("unexpected console in: got %q want %q", got, want)
	}

	c = &CLI{}
	parser = newParserForTest(t, c)
	if _, err := parser.Parse([]string{"console", "--from", "snap_123", "--keep"}); err != nil {
		t.Fatalf("parse console --from --keep returned error: %v", err)
	}
	if got, want := c.Console.From, "snap_123"; got != want {
		t.Fatalf("unexpected console from: got %q want %q", got, want)
	}
	if !c.Console.Keep {
		t.Fatal("expected console keep flag to be set")
	}
}

func TestExecRejectsUnknownFlagBeforeSeparator(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	_, err := parser.Parse([]string{"exec", "--with", "snap_123", "--", "echo", "ok"})
	if err == nil {
		t.Fatal("expected unknown exec flag to be rejected")
	}
	if !strings.Contains(err.Error(), "unknown flag --with") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExecAllowsOptionLikeArgsAfterCommandStart(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"exec", "worker", "echo", "--with", "snap_123"}); err != nil {
		t.Fatalf("expected option-like args after command start to be allowed, got %v", err)
	}
	if got, want := strings.Join(c.Exec.Command, " "), "echo --with snap_123"; got != want {
		t.Fatalf("unexpected exec command: got %q want %q", got, want)
	}
}

func TestExecAllowsOptionLikeArgsAfterSeparator(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"exec", "worker", "--", "--with", "snap_123"}); err != nil {
		t.Fatalf("expected option-like command after separator to be allowed, got %v", err)
	}
	if got, want := strings.Join(c.Exec.Command, " "), "-- --with snap_123"; got != want {
		t.Fatalf("unexpected exec command after separator: got %q want %q", got, want)
	}
}

func TestConsoleRejectsUnknownFlagBeforeSeparator(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	_, err := parser.Parse([]string{"console", "--with", "snap_123"})
	if err == nil {
		t.Fatal("expected unknown console flag to be rejected")
	}
	if !strings.Contains(err.Error(), "unknown flag --with") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConsoleAllowsOptionLikeArgsAfterCommandStart(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"console", "sh", "--with"}); err != nil {
		t.Fatalf("expected option-like console args after command start to be allowed, got %v", err)
	}
	if got, want := strings.Join(c.Console.Command, " "), "sh --with"; got != want {
		t.Fatalf("unexpected console command: got %q want %q", got, want)
	}
}

func TestConsoleAllowsOptionLikeArgsAfterSeparator(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"console", "--", "--with"}); err != nil {
		t.Fatalf("expected console option-like command after separator to be allowed, got %v", err)
	}
	if got, want := strings.Join(c.Console.Command, " "), "-- --with"; got != want {
		t.Fatalf("unexpected console command after separator: got %q want %q", got, want)
	}
}
func TestConsoleParsesLegacySandboxIDFlag(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"console", "--sandbox-id", "cr_123"}); err != nil {
		t.Fatalf("parse console --sandbox-id returned error: %v", err)
	}
	if got, want := c.Console.In, "cr_123"; got != want {
		t.Fatalf("unexpected console in from --sandbox-id: got %q want %q", got, want)
	}
}

func TestSandboxRestoreCommandRemoved(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	_, err := parser.Parse([]string{"sandbox", "restore", "cr_123", "--from", "snap_123"})
	if err == nil {
		t.Fatal("expected parse error for removed sandbox restore command")
	}
	if !strings.Contains(err.Error(), "restore") {
		t.Fatalf("expected parse error to mention restore, got %v", err)
	}
}

func TestServeCommandParses(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"serve"}); err != nil {
		t.Fatalf("parse serve returned error: %v", err)
	}
}

func TestServeCommandParsesPprofListen(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"serve", "--pprof-listen", "127.0.0.1:6060"}); err != nil {
		t.Fatalf("parse serve --pprof-listen returned error: %v", err)
	}
	if got, want := c.Serve.PprofListen, "127.0.0.1:6060"; got != want {
		t.Fatalf("unexpected pprof listen: got %q want %q", got, want)
	}
}

func TestServeCommandRejectsExposureListenFlags(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"serve", "--exposure-dns-listen", "127.0.0.1:1153"}); err == nil {
		t.Fatal("expected parse error for removed serve exposure listen flag")
	}
}

func TestRuntimeServiceNameUsesDescriptiveNames(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if got, want := runtimeServiceName(nil), "cleanroom-cli"; got != want {
		t.Fatalf("unexpected nil-context service name: got %q want %q", got, want)
	}

	execCtx, err := parser.Parse([]string{"exec", "--", "echo", "ok"})
	if err != nil {
		t.Fatalf("parse exec returned error: %v", err)
	}
	if got, want := runtimeServiceName(execCtx), "cleanroom-cli"; got != want {
		t.Fatalf("unexpected exec service name: got %q want %q", got, want)
	}

	serveCtx, err := parser.Parse([]string{"serve"})
	if err != nil {
		t.Fatalf("parse serve returned error: %v", err)
	}
	if got, want := runtimeServiceName(serveCtx), "cleanroom-server"; got != want {
		t.Fatalf("unexpected serve service name: got %q want %q", got, want)
	}
}

func TestCommandUsesStartupObservabilitySkipsDaemonLifecycleCommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		wantUse bool
	}{
		{name: "serve", args: []string{"serve"}, wantUse: true},
		{name: "exec", args: []string{"exec", "--", "echo", "ok"}, wantUse: true},
		{name: "daemon install", args: []string{"daemon", "install"}, wantUse: false},
		{name: "daemon install restart", args: []string{"daemon", "install", "--restart"}, wantUse: false},
		{name: "daemon status", args: []string{"daemon", "status"}, wantUse: false},
		{name: "daemon restart", args: []string{"daemon", "restart"}, wantUse: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &CLI{}
			parser := newParserForTest(t, c)

			ctx, err := parser.Parse(tt.args)
			if err != nil {
				t.Fatalf("parse %q returned error: %v", strings.Join(tt.args, " "), err)
			}

			if got := commandUsesStartupObservability(ctx); got != tt.wantUse {
				t.Fatalf("unexpected observability startup decision for %q: got %t want %t", ctx.Command(), got, tt.wantUse)
			}
		})
	}
}

func TestReportObservabilityShutdownWarnsWithoutFailingSuccessfulRun(t *testing.T) {
	t.Parallel()

	var runErr error
	var stderr bytes.Buffer

	reportObservabilityShutdown(&runErr, &stderr, errors.New("collector unavailable"))

	if runErr != nil {
		t.Fatalf("expected successful run to stay successful, got %v", runErr)
	}
	if got := stderr.String(); !strings.Contains(got, "shutdown observability: collector unavailable") {
		t.Fatalf("expected shutdown warning in stderr, got %q", got)
	}
}

func TestNewObservabilityErrorReporterWarnsToStderr(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	report := newObservabilityErrorReporter(&stderr)

	report(errors.New("collector unavailable"))

	if got := stderr.String(); !strings.Contains(got, "warning: collector unavailable") {
		t.Fatalf("expected observability warning in stderr, got %q", got)
	}
}

func TestReportObservabilityShutdownAppendsToExistingRunError(t *testing.T) {
	t.Parallel()

	runErr := errors.New("command failed")
	var stderr bytes.Buffer

	reportObservabilityShutdown(&runErr, &stderr, errors.New("collector unavailable"))

	if runErr == nil {
		t.Fatal("expected run error to be preserved")
	}
	if got := runErr.Error(); !strings.Contains(got, "command failed") || !strings.Contains(got, "shutdown observability: collector unavailable") {
		t.Fatalf("expected combined error, got %q", got)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("expected no standalone warning for already failing run, got %q", got)
	}
}

func TestServeInstallIsRejected(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	_, err := parser.Parse([]string{"serve", "install"})
	if err == nil {
		t.Fatal("expected parse error for unexpected serve action")
	}
}

func TestServeParsesGitHubAppCredentialFlags(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{
		"serve",
		"--github-app-id", "3817917",
		"--github-app-installation-id", "134770928",
		"--github-app-private-key-file", "/Users/lachlan/.config/cleanroom/github-app.pem",
		"--github-app-repo-prefixes", "buildkite/,buildkite/cleanroom",
	}); err != nil {
		t.Fatalf("parse serve returned error: %v", err)
	}
	if got, want := c.Serve.GitHubAppID, "3817917"; got != want {
		t.Fatalf("unexpected GitHub App ID: got %q want %q", got, want)
	}
	if got, want := c.Serve.GitHubAppInstallationID, "134770928"; got != want {
		t.Fatalf("unexpected GitHub App installation ID: got %q want %q", got, want)
	}
	if got, want := c.Serve.GitHubAppPrivateKeyFile, "/Users/lachlan/.config/cleanroom/github-app.pem"; got != want {
		t.Fatalf("unexpected GitHub App private key file: got %q want %q", got, want)
	}
	if got, want := c.Serve.GitHubAppRepoPrefixes, []string{"buildkite/", "buildkite/cleanroom"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected GitHub App repo prefixes: got %v want %v", got, want)
	}
}

func TestServeParsesGitHubAppCredentialEnv(t *testing.T) {
	t.Setenv("CLEANROOM_GITHUB_APP_ID", "3817917")
	t.Setenv("CLEANROOM_GITHUB_APP_INSTALLATION_ID", "134770928")
	t.Setenv("CLEANROOM_GITHUB_APP_PRIVATE_KEY_FILE", "/Users/lachlan/.config/cleanroom/github-app.pem")
	t.Setenv("CLEANROOM_GITHUB_APP_REPO_PREFIXES", "buildkite/,buildkite/cleanroom")

	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"serve"}); err != nil {
		t.Fatalf("parse serve returned error: %v", err)
	}
	if got, want := c.Serve.GitHubAppID, "3817917"; got != want {
		t.Fatalf("unexpected GitHub App ID: got %q want %q", got, want)
	}
	if got, want := c.Serve.GitHubAppInstallationID, "134770928"; got != want {
		t.Fatalf("unexpected GitHub App installation ID: got %q want %q", got, want)
	}
	if got, want := c.Serve.GitHubAppPrivateKeyFile, "/Users/lachlan/.config/cleanroom/github-app.pem"; got != want {
		t.Fatalf("unexpected GitHub App private key file: got %q want %q", got, want)
	}
	if got, want := c.Serve.GitHubAppRepoPrefixes, []string{"buildkite/", "buildkite/cleanroom"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected GitHub App repo prefixes: got %v want %v", got, want)
	}
}

func TestDaemonInstallParses(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"daemon", "install"}); err != nil {
		t.Fatalf("parse daemon install returned error: %v", err)
	}
	if got := c.Daemon.Action; got != "install" {
		t.Fatalf("expected daemon action install, got %q", got)
	}
}

func TestDaemonInstallRestartFlagsParse(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"daemon", "install", "--restart", "--init-config", "--dry-run"}); err != nil {
		t.Fatalf("parse daemon install flags returned error: %v", err)
	}
	if got := c.Daemon.Action; got != "install" {
		t.Fatalf("expected daemon action install, got %q", got)
	}
	if !c.Daemon.Restart {
		t.Fatal("expected --restart to set Daemon.Restart")
	}
	if !c.Daemon.InitConfig {
		t.Fatal("expected --init-config to set Daemon.InitConfig")
	}
	if !c.Daemon.DryRun {
		t.Fatal("expected --dry-run to set Daemon.DryRun")
	}
}

func TestDaemonInstallRejectsGitHubAppCredentialFlags(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	_, err := parser.Parse([]string{"daemon", "install", "--github-app-id", "3817917"})
	if err == nil {
		t.Fatal("expected daemon install to reject GitHub App credential flags")
	}
}

func TestDNSInstallParses(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"dns", "install"}); err != nil {
		t.Fatalf("parse dns install returned error: %v", err)
	}
	if got, want := c.DNS.Action, "install"; got != want {
		t.Fatalf("expected dns action install, got %q", got)
	}
}

func TestDNSInstallRejectsListenFlag(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"dns", "install", "--listen", "127.0.0.1:1153"}); err == nil {
		t.Fatal("expected parse error for removed dns listen flag")
	}
}

func TestDaemonStatusUserParses(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"daemon", "status", "--user"}); err != nil {
		t.Fatalf("parse daemon status --user returned error: %v", err)
	}
	if !c.Daemon.User {
		t.Fatal("expected --user to set Daemon.User")
	}
}

func TestDaemonStatusJSONParses(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"daemon", "status", "--json"}); err != nil {
		t.Fatalf("parse daemon status --json returned error: %v", err)
	}
	if !c.Daemon.JSON {
		t.Fatal("expected --json to set Daemon.JSON")
	}
}

func TestDaemonLogsFlagsParse(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"daemon", "logs", "--follow", "--lines", "25"}); err != nil {
		t.Fatalf("parse daemon logs flags returned error: %v", err)
	}
	if got := c.Daemon.Action; got != "logs" {
		t.Fatalf("expected daemon action logs, got %q", got)
	}
	if !c.Daemon.Follow {
		t.Fatal("expected --follow to set Daemon.Follow")
	}
	if got, want := c.Daemon.Lines, "25"; got != want {
		t.Fatalf("expected --lines %q, got %q", want, got)
	}
}

func TestDaemonLogsShortFlagsParse(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"daemon", "logs", "-f", "-n", "10"}); err != nil {
		t.Fatalf("parse daemon logs short flags returned error: %v", err)
	}
	if !c.Daemon.Follow {
		t.Fatal("expected -f to set Daemon.Follow")
	}
	if got, want := c.Daemon.Lines, "10"; got != want {
		t.Fatalf("expected -n %q, got %q", want, got)
	}
}

func TestDaemonStartSystemParses(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"daemon", "start", "--system"}); err != nil {
		t.Fatalf("parse daemon start --system returned error: %v", err)
	}
	if !c.Daemon.System {
		t.Fatal("expected --system to set Daemon.System")
	}
}

func TestDaemonRestartForceParses(t *testing.T) {
	c := &CLI{}
	parser := newParserForTest(t, c)

	if _, err := parser.Parse([]string{"daemon", "restart", "--force"}); err != nil {
		t.Fatalf("parse daemon restart --force returned error: %v", err)
	}
	if got := c.Daemon.Action; got != "restart" {
		t.Fatalf("expected daemon action restart, got %q", got)
	}
	if !c.Daemon.Force {
		t.Fatal("expected --force to set Daemon.Force")
	}
}
