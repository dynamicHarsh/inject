# inject

**Inject a project's secrets into a foreground child process, without a plaintext `.env` file.**

`inject` is a command-line adapter, not a secret manager. It reads a non-secret `inject.toml` configuration, retrieves a complete secret set from your existing provider or the OS credential store, and supplies it only to the child process it starts.

The legacy `env-pull` binary remains a temporary compatibility alias and prints a migration notice.

## Why

Plaintext `.env` files persist on developer machines, can be committed accidentally, and tend to drift from the approved secret source. `inject` keeps secret values off the project disk while preserving normal development commands through named bindings.

- Secrets are passed only through process environment inheritance.
- Project configuration is safe to commit: it contains provider references, never values or provider credentials.
- Remote providers remain the system of record for authentication, authorization, audit history, and rotation.
- A child process receives a complete validated secret set or does not start.

## Installation

### Homebrew (macOS / Linux)

```bash
brew tap dynamicHarsh/tap && brew install inject
```

### Scoop (Windows)

```powershell
scoop bucket add env-pull https://github.com/dynamicHarsh/scoop-bucket
scoop install inject
```

### Build from source

```bash
git clone https://github.com/harsh-sonkar/env-pull.git
cd env-pull
make build
```

The binary is written to `./bin/inject`.

## Quick Start

Install `inject` before configuring a project. For a remote source, also install
the provider CLI and sign in with your existing provider account.

### Team Project: 1Password

Create a Secure Note in your team's 1Password vault. Its body must use standard `.env` syntax:

```dotenv
DATABASE_URL=postgres://...
API_KEY=...
```

Sign in to the 1Password CLI, then run setup from the project root. Setup previews
the non-secret project configuration and package-script changes before applying them.

```bash
op signin
inject setup \
  --project-id acme-web \
  --account acme.1password.com \
  --vault Engineering \
  --item acme-web-development \
  --package-script dev \
  --validate npm --validate test \
  --yes
```

Commit the generated `inject.toml` and `package.json` changes. Each developer
signs in to `op` using their own account, then uses the same project-native
command they used before setup:

```bash
npm run dev
```

Setup preserves the selected script and its `pre` and `post` lifecycle hooks
behind an injected wrapper. The equivalent unchanged commands are `pnpm run dev`,
`yarn dev`, and `bun run dev` when the project declares that package manager.
Only scripts declared by Node projects in `package.json` can be wrapped this way.

For a command that is not declared in `package.json`, use the explicit fallback:

```bash
inject run -- npm run db:migrate
```

### Local-Only Project

From a directory with an existing `.env`, setup imports into macOS Keychain or Linux Secret Service by default. The project ID defaults to the directory name and can be overridden with `--project-id`:

```bash
inject setup \
  --package-script dev \
  --yes
npm run dev
```

The imported values stay in the operating system credential store. They are not
shared through `inject.toml`, and `.env` remains unless removal is explicitly
requested with both `--remove-env` and `--yes-remove-env`. Use `--local` to force
local source selection when needed. Run `inject edit` to add, rename, update, or
delete values in a local secret set; changes remain in memory until you confirm
them. Remote secret sets must be edited in their owning provider.

### One-off command

Run any command using the default profile without creating a binding:

```bash
inject run -- npm run db:migrate
inject run --profile staging -- ./bin/server --check
```

Injected values replace same-named variables inherited from the invoking shell. They are not exported back to that shell.

## Configuration

`inject.toml` is committed at the project root. It contains no secret values, provider tokens, private item URLs, or developer-specific paths.

```toml
format_version = 1
project_id = "acme-web"

[profiles.default]
provider = "1password"
account = "acme.1password.com"
vault = "Engineering"
item = "acme-web-development"
item_id = "provider-item-id"

[commands]
dev = { command = ["npm", "run", "dev:app"] }

[cache]
enabled = false
max_age = "24h"
```

Supported profile providers are `1password`, `bitwarden`, and `local`. A `local` profile needs no provider reference. Provider item IDs take precedence over display names when both are configured.

For a named profile, set the binding's `profile` field:

```toml
[profiles.staging]
provider = "bitwarden"
item = "acme-web-staging"

[commands.staging]
profile = "staging"
command = ["npm", "run", "dev"]
```

## Providers and Offline Use

Remote secret notes, access control, authentication, audit history, and rotation
remain owned by their provider. `inject` stores no provider credentials and
reuses the authenticated session from the relevant CLI:

- 1Password: `op signin`
- Bitwarden: `bw login`

Normal remote runs always fetch fresh values. You may explicitly enable a credential-store cache in `inject.toml` and request it for offline work:

```toml
[cache]
enabled = true
max_age = "24h"
```

```bash
inject run --offline -- npm run dev
```

Offline use fails when the cache is disabled, unavailable, expired, or used in CI.

## Command Reference

```text
inject setup [flags]
inject <binding>
inject run [--profile <name>] [--offline] -- <command> [args...]
inject remove --yes
inject edit
inject export               # legacy encrypted-vault export
```

- `setup` previews and, with `--yes`, creates or updates project configuration and selected `package.json` script bindings. Use `--validate` repeatedly to supply a finite validation command. A rerun is idempotent when generated scripts are unchanged; if an inject-owned script was modified, interactive setup asks whether to retain or replace it, while non-interactive setup stops before mutation.
- `<binding>` runs an explicit named command declared under `[commands.<name>]` in `inject.toml` as an injected foreground child process.
- `run` injects a selected profile into an undeclared child command. Place all `inject` flags before the child command.
- `remove` first previews the project configuration, package scripts, local secret sets, and remote caches it will remove. `--yes` confirms restoration of original scripts and deletion of only that project's local state. It never deletes remote provider items.
- `edit` changes configured local secret sets without a plaintext temporary file. Remote secret sets remain provider-owned. Without `inject.toml`, it retains compatibility with the legacy encrypted-vault editor.
- `export` remains for compatibility with the former encrypted `.env.pull.enc` workflow. New projects should use `setup` instead.

Run `inject <command> --help` for detailed flags; because `run` passes its arguments through unchanged, use `inject help run` for that command's help.

## Security Model

| Property | Behavior |
| --- | --- |
| Project disk | `inject.toml` contains no secret values. |
| Environment scope | Secrets are available only to the child process tree. |
| Parent shell | The invoking shell is never modified. |
| Provider access | Authentication and authorization stay with 1Password or Bitwarden. |
| Local values | Stored in macOS Keychain or Linux Secret Service. |
| Failure mode | Invalid configuration, unavailable source, or malformed secret sets prevent process launch. |
| Output | Child stdout and stderr pass through unchanged; `inject` does not redact application output. |

## Contributing

```bash
go test ./...
make build
make test
```

## License

MIT. See [LICENSE](LICENSE).
