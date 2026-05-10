# Rails Example

Runs a targeted `rails/rails` component test inside a Cleanroom sandbox with a
deny-by-default network policy, dependency-stage `bundle install`, and the
embedded RubyGems content-cache route.

## Prerequisites

Install Cleanroom with the main installer. The installer starts the daemon.

The example policy declares enough guest memory and disk for the first Rails bundle:

```yaml
sandbox:
  resources:
    memory: 4GiB
    disk: 14GiB
```

## Usage

Run from this directory:

```bash
cleanroom policy validate

cleanroom exec \
  --repo-url https://github.com/rails/rails.git \
  -e BUNDLE_PATH=/workspace/vendor/bundle \
  -e BUNDLE_APP_CONFIG=/workspace/vendor/bundle/.bundle \
  -- sh -lc 'cd activesupport && bin/test test/benchmarkable_test.rb'
```

## What this exercises

- the `bundle` dependency block fills the remaining guest package gap
  (`default-libmysqlclient-dev`, `libxml2-dev`) and runs a full `bundle install`
- the block sets Bundler env so gems and Bundler app config are materialized
  under the declared repository-local `vendor/bundle` output
- the execution command passes the same Bundler env because dependency block
  env is scoped to the create-time dependency command
- Bundler mirror env injection rewrites the default `https://rubygems.org`
  source through Cleanroom's `/rubygems/` gateway route
- the dependency stage is keyed on `Gemfile`, `Gemfile.lock`, and the component
  gemspecs Rails uses from the monorepo

## Network Allow List

The example policy allows only the hosts needed for the Rails checkout,
Debian package bootstrap, and RubyGems resolution:

| Host | Ports | Why |
|---|---|---|
| `github.com` | `443` | fetch the `rails/rails` repository |
| `rubygems.org` | `443` | upstream host validated by the embedded RubyGems cache |
| `deb.debian.org` | `80` | `apt-get` bootstrap for native gem build dependencies |

## Notes

- The first run is slow because it installs `default-libmysqlclient-dev` and
  `libxml2-dev`, installs Bundler 4 from the Rails lockfile, and resolves the
  full Rails bundle
- The targeted `activesupport` test avoids database services while still
  proving that bundle install completed successfully
