# Rails Example

Runs a targeted `rails/rails` component test inside a Cleanroom sandbox with a
deny-by-default network policy, dependency-stage `bundle install`, and the
embedded RubyGems content-cache route.

## Prerequisites

Install Cleanroom from this checkout:

```bash
mise run install
```

Pull the Debian Ruby base image used by the example:

```bash
cleanroom image pull ghcr.io/buildkite/cleanroom-base/debian-ruby@sha256:cba9876d67beb8971e791f1bc6af175f0bff47e938f955059813c4dd6914eb53
```

The example policy declares enough guest memory and disk for the first Rails bundle:

```yaml
sandbox:
  resources:
    memory: 4GiB
    disk: 14GiB
```

## Usage

Run from this directory with a `cleanroom serve` instance running:

```bash
# Validate the policy
cleanroom policy validate

# Start the control plane if it is not already running
cleanroom serve &

# Run a small Rails component test
cleanroom exec \
  --backend darwin-vz \
  --repo-url https://github.com/rails/rails.git \
  --repo-commit cfa4e1b475472c7980a42dd810f237951db5108a \
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

## Network allow list

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
