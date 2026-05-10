# Workspaces

Cleanroom separates exact repository checkout from local working tree changes.
That keeps normal runs close to CI, while still giving you explicit commands for
copying local edits into or out of a sandbox.

## Repo-Aware Commands

`cleanroom create`, `cleanroom exec`, and `cleanroom console` inspect the current
Git checkout when run inside a repository. They resolve the remote, exact
commit, and policy, then materialize the repository at `/workspace` unless
policy overrides `repository.path`.

```bash
cleanroom exec -- go test ./...
cleanroom console -- sh
```

`cleanroom sandbox create` is different. It is repo-agnostic and does not read
the local checkout or `cleanroom.yaml` unless you pass explicit options:

```bash
cleanroom sandbox create --image ghcr.io/buildkite/cleanroom-base/alpine:latest
```

For examples or external repos, pass the repo directly:

```bash
cleanroom exec \
  --repo-url https://github.com/buildkite/agent.git \
  -- mise x -- go run . --version
```

`--repo-url` defaults to the remote `HEAD`. Use `--repo-commit` for a specific
commit, tag, or `latest`.

## Local Edits

Local changes are not copied into repo-aware runs by default. Commit and push
when you want the sandbox to see the same tree CI will see. Use copy flags when
you want a local edit loop:

```bash
cleanroom exec --copy-in -- npm test
cleanroom exec --copy-out -- npm run fmt
cleanroom exec --sync -- npm run generate
cleanroom console --sync -- sh
```

`--copy-in` mirrors local Git workspace changes into the sandbox before the
command runs.

`--copy-out` copies sandbox workspace changes back after the command exits.

`--sync` is `--copy-in --copy-out`.

If automatic copy-out fails, Cleanroom keeps the sandbox so you can inspect it
or retry manually.

## Manual Workspace Commands

Use manual commands when you keep a sandbox around:

```bash
cleanroom create --copy-in
# 01kr7p9ksmfa7rmyypyqw2w8r0

cleanroom workspace diff 01kr7p9ksmfa7rmyypyqw2w8r0
cleanroom workspace copy-in --dry-run 01kr7p9ksmfa7rmyypyqw2w8r0
cleanroom workspace copy-in 01kr7p9ksmfa7rmyypyqw2w8r0
cleanroom workspace copy-out --dry-run 01kr7p9ksmfa7rmyypyqw2w8r0
cleanroom workspace copy-out 01kr7p9ksmfa7rmyypyqw2w8r0
```

`workspace copy-out` requires a matching local Git checkout. It refuses to
overwrite unrelated local changes unless you pass `--force`.

```bash
cleanroom workspace copy-out --force 01kr7p9ksmfa7rmyypyqw2w8r0
```

The Git-backed workspace commands are the supported path. Non-Git directories
are not accepted by workspace copy-in/copy-out.

## One-Off File Copy

For a single file or directory, use `cleanroom cp`:

```bash
cleanroom cp ./fixture.txt 01kr7p9ksmfa7rmyypyqw2w8r0:/tmp/fixture.txt
cleanroom cp 01kr7p9ksmfa7rmyypyqw2w8r0:/tmp/result.txt ./result.txt
```

Use workspace copy for repository edits. Use `cp` for small ad hoc transfers.
