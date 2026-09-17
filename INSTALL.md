# Installing PayCLI

`pay` is a single static Go binary. No Node, no Python, no runtime, no dynamic libraries.

- [Quick install](#quick-install)
- [Installer options](#installer-options)
- [Manual download](#manual-download)
- [Verifying a download](#verifying-a-download)
- [go install](#go-install)
- [Where things live](#where-things-live)
- [Self-update](#self-update)
- [Air-gapped and private mirrors](#air-gapped-and-private-mirrors)
- [Uninstalling](#uninstalling)
- [Environment variables](#environment-variables)
- [Troubleshooting](#troubleshooting)

---

## Quick install

**macOS / Linux**

```sh
curl -fsSL https://raw.githubusercontent.com/KLIXPERT-io/pay-cli/main/install.sh | sh
```

**Windows (PowerShell 5.1+)**

```powershell
irm https://raw.githubusercontent.com/KLIXPERT-io/pay-cli/main/install.ps1 | iex
```

Both installers:

1. detect your OS and CPU architecture (including `windows/arm64` and Apple Silicon),
2. resolve the latest release tag from the GitHub API,
3. download the archive **and** `checksums.txt`,
4. **verify the SHA-256 and refuse to install on a mismatch**,
5. verify the cosign signature over `checksums.txt` when `cosign` is on your PATH,
6. extract the binary into the install directory and print its version.

The default install directory is **`$HOME/.local/bin`** on Unix and
`%LOCALAPPDATA%\Programs\pay` on Windows. That is deliberate: agents rarely have `sudo`,
and a user-writable install is a precondition for `pay update-self` working at all.

---

## Installer options

Piping into `sh` accepts flags after `--`:

```sh
curl -fsSL .../install.sh | sh -s -- --version v0.1.0 --dir /usr/local/bin --with-skills
```

| Flag | Environment | Effect |
|---|---|---|
| `--version TAG` | `PAY_VERSION` | install a specific release instead of the latest |
| `--dir PATH` | `INSTALL_DIR` | install directory |
| `--with-skills` | — | run `pay skills install` afterwards |
| `--dry-run` | — | print the plan, touch nothing, exit 0 |
| — | `GH_TOKEN` | authenticate the `api.github.com` call |
| — | `PAY_INSTALL_JSON=1` | print `{"version":…,"path":…}` as the final line |

`GH_TOKEN` matters more than it looks: the anonymous GitHub API is **60 requests per hour
per IP**, and shared CI egress addresses exhaust that constantly. Without a token, a CI
install fails intermittently with "could not resolve the latest release tag".

PowerShell takes the same options as parameters:

```powershell
irm https://raw.githubusercontent.com/KLIXPERT-io/pay-cli/main/install.ps1 -OutFile install.ps1
.\install.ps1 -Version v0.1.0 -Dir C:\tools\pay -WithSkills
```

---

## Manual download

Every release publishes one archive per platform on the
[releases page](https://github.com/KLIXPERT-io/pay-cli/releases):

```
pay_<version>_darwin_amd64.tar.gz
pay_<version>_darwin_arm64.tar.gz
pay_<version>_linux_amd64.tar.gz
pay_<version>_linux_arm64.tar.gz
pay_<version>_windows_amd64.zip
pay_<version>_windows_arm64.zip
checksums.txt
checksums.txt.sig
checksums.txt.pem
*.sbom.json
```

> The archive name template is **frozen**. An already-installed binary constructs this
> exact name to find its own update, so it cannot change without breaking self-update for
> every existing install. A CI check asserts that GoReleaser, `install.sh`, `install.ps1`
> and `internal/update` all agree on it.

```sh
VERSION=0.1.0
OS=linux           # or darwin
ARCH=amd64         # or arm64
BASE="https://github.com/KLIXPERT-io/pay-cli/releases/download/v${VERSION}"

curl -fsSLO "${BASE}/pay_${VERSION}_${OS}_${ARCH}.tar.gz"
curl -fsSLO "${BASE}/checksums.txt"
sha256sum --check --ignore-missing checksums.txt

tar -xzf "pay_${VERSION}_${OS}_${ARCH}.tar.gz"
install -m 0755 pay "$HOME/.local/bin/pay"
```

---

## Verifying a download

There are two independent proofs, and they answer different questions.

**1. The checksum** proves the bytes you got are the bytes that were published.

```sh
sha256sum --check --ignore-missing checksums.txt
```

**2. The signature** proves the publication itself came from this repository's CI.
`checksums.txt` is signed with keyless cosign, so the certificate carries the workflow
identity rather than a long-lived key:

```sh
cosign verify-blob \
  --certificate checksums.txt.pem \
  --signature   checksums.txt.sig \
  --certificate-identity-regexp '^https://github.com/KLIXPERT-io/pay-cli/' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  checksums.txt
```

A checksum alone proves nothing against a compromised release, because `checksums.txt`
comes from the same origin as the artefact. That is why the chain is
**archive → checksums.txt → signature**, and why a mismatch anywhere in it is treated the
same way.

**3. The GitHub attestation** is the same guarantee without needing cosign installed:

```sh
gh attestation verify pay_0.1.0_linux_amd64.tar.gz --repo KLIXPERT-io/pay-cli
```

---

## go install

```sh
go install github.com/KLIXPERT-io/pay-cli/cmd/pay@latest
```

This builds from source through the Go module proxy and the checksum database, so it is
verified end to end — but it produces a binary **PayCLI will not replace**. `pay
update-self` detects a `go install` binary (it is under `GOBIN` or `$GOPATH/bin`) and
tells you to re-run `go install …@latest` instead of overwriting something a package
manager owns.

The same applies to Homebrew, nix, asdf, mise, scoop, chocolatey, snap and flatpak.

---

## Installing the agent skill

PayCLI ships an agent skill that teaches an LLM coding agent how to drive the CLI safely:
the JSON envelope, the exit-code table, the query mini-DSL and the Payload behaviours that
produce confidently wrong answers when guessed. It lives in the repository at
[`skills/pay/`](https://github.com/KLIXPERT-io/pay-cli/tree/main/skills/pay), and there are
two ways to install it.

**From the binary — offline, and the one the installers use:**

```sh
pay skills install
```

No network, no Node, no `git`: the skill is compiled into `pay` with `go:embed`, so this
works on an air-gapped machine and always matches the binary you are running.
`install.sh --with-skills` / `install.ps1 -WithSkills` simply run it for you.

**With the [`skills`](https://github.com/anthropics/skills) CLI** — useful when you want
the skill in an editor or agent before (or without) installing PayCLI itself:

```sh
npx skills add https://github.com/KLIXPERT-io/pay-cli/skills --skill pay
```

This reads `skills/pay/` straight out of the repository and drops `pay/SKILL.md` plus its
`references/` into your agent's skill directory. Re-run it to pull updates.

The two routes install the same bytes. `skills/` is a Go package that owns the `go:embed`
of `skills/pay/`, so there is exactly one copy of the skill in the repository and the
embedded tree cannot drift from the one you can read on GitHub.

Only `pay skills install` can do the two things that need a running binary:

| | `pay skills install` | `npx skills add` |
|---|---|---|
| works offline | yes | no |
| writes `references/PROJECT.md` for the current project (`--with-project-context`) | yes | no |
| records `.pay-skill.json`, so `pay skills status` reports staleness and drift | yes | no |
| installs into every agent directory that already exists | yes | one at a time |
| needs `pay` on `PATH` | yes | no |

`pay skills install --help` covers `--global`, `--agent`, `--dir` and `--force`.

---

## Where things live

PayCLI follows the XDG base-directory spec on Unix and the standard app-data locations on
Windows. `pay config paths` prints the resolved set, and `pay config paths --output json`
gives you all eight as a map.

| | Linux / BSD | macOS | Windows |
|---|---|---|---|
| config | `~/.config/pay` | `~/Library/Application Support/pay` | `%AppData%\pay` |
| cache | `~/.cache/pay` | `~/Library/Caches/pay` | `%LocalAppData%\pay\cache` |
| state | `~/.local/state/pay` | `~/Library/Application Support/pay` | `%LocalAppData%\pay\state` |

Inside them:

| File | Mode | What |
|---|---|---|
| `config.toml` | 0600 | settings and profiles. **Never a credential** |
| `credentials.json` | 0600 | API keys and JWTs, unless they are in the keychain |
| `audit.log` | 0600 | every L1–L3 write, before and after, JSONL, 10 MB × 3 generations |
| `cache/v1/<scope>/` | 0700 | discovery manifests and field schemas, per connection+credential |
| `update-state.json` | 0600 | the last update check |

Override any of them: `PAY_CONFIG_DIR`, `PAY_CACHE_DIR`, `PAY_STATE_DIR`, or `PAY_HOME`
to move all three at once. `PAY_HOME` is what you want for a container, a CI job or a test:

```sh
PAY_HOME="$(mktemp -d)" pay whoami
```

Deleting the cache is always safe:

```sh
rm -rf "$(pay cache path --output raw)"
```

It costs one re-discovery, nothing else. If it ever costs more than that, it is a bug.

---

## Self-update

**Automatic updating is off by default.** A CLI that silently swaps itself mid-session is
a CLI whose version an agent cannot reason about.

```sh
pay update-self --check      # bounded 2 s; records the pending version and reports
pay update-self --apply      # download, verify, swap. 120 s deadline. Then exit
```

`--apply` does only the update: it does not re-exec the new binary and does not continue
to another command. The new version takes effect on your next invocation.

### Opting in to background updates

```sh
pay config set update.auto true
```

With that set, a `pay` invocation that finds a recorded pending version spawns a
**detached child** to perform the swap and returns immediately. Zero network in the
foreground, no chance of a fast command blocking on a download. The swapped binary takes
effect on the next invocation.

On Windows the running `.exe` cannot be renamed over, so the swap moves it to
`pay.exe.old` first and the next start sweeps it. This is functional on Windows, not a
silent no-op.

### The three verification outcomes

| Outcome | Meaning | Behaviour |
|---|---|---|
| `verified` | signature or attestation present and matching | proceed |
| `unverifiable` | **no evidence either way** — cosign absent, attestation API unreachable | warn loudly on stderr, proceed **unless** `PAY_UPDATE_STRICT=1` |
| `verification_failed` | a checksum, signature or attestation **is present and does not match** | **always abort**, exit 1, print expected vs computed. `PAY_UPDATE_STRICT` cannot relax this |

The distinction is the entire point of signing. A mismatching signature is positive
evidence of exactly the compromised release this mechanism exists to defend against, and
installing anyway is strictly worse than never having checked.

### Turning it all off

```sh
export PAY_NO_UPDATE=1        # no release-API call, ever, from any code path
```

Set this in CI. Set it in containers. Nothing about PayCLI's behaviour depends on it.

---

## Air-gapped and private mirrors

`PAY_UPDATE_URL` points the downloader at any HTTP server that serves the release layout
(`<base>/pay_<ver>_<os>_<arch>.<ext>` plus `checksums.txt`):

```sh
export PAY_UPDATE_URL=https://artifacts.internal/pay/v0.2.0
pay update-self --apply
```

Distribution is GitHub-only in v0.1.0; this is a documented escape hatch for private
mirrors and air-gapped installs, not a second supported channel. Checksum verification is
still mandatory — mirror `checksums.txt` alongside the archives, or the install fails.

For a fully offline install, mirror the archive and `checksums.txt` and use the manual
steps above. Everything else about PayCLI is offline already: after one discovery, every
`pay explain`, `pay collections` and `pay describe` is a local read.

---

## Uninstalling

```sh
pay skills uninstall              # remove the agent skill (only files pay installed)
pay auth logout --all             # clear stored credentials and keychain entries
pay cache clear --all             # drop the discovery cache

rm -f "$(command -v pay)"
rm -rf "$(pay config paths --output raw 2>/dev/null || echo ~/.config/pay)"
```

`pay skills uninstall` removes only files recorded in `.pay-skill.json` whose hash still
matches, then prunes the directories left empty. A file you edited is left alone.

---

## Environment variables

### Connection and auth

| Variable | Effect |
|---|---|
| `PAY_PROFILE` | profile to use |
| `PAY_BASE_URL` | Payload origin, e.g. `http://localhost:3900` |
| `PAY_API_PATH` | `routes.api` prefix (default `/api`) |
| `PAY_API_KEY` / `PAY_API_KEY_<PROFILE>` | API key credential |
| `PAY_JWT` / `PAY_JWT_<PROFILE>` | bearer token instead of an API key |
| `PAY_AUTH_COLLECTION` | auth collection slug (`users`, or `auto` to discover it) |
| `PAY_AUTH_MODE` | `auto` \| `api-key` \| `jwt` \| `anonymous` |
| `PAY_KEYRING` | `auto` \| `off` \| `force` |

`<PROFILE>` is the profile name upper-cased with non-alphanumerics replaced by `_`:
profile `staging-eu` reads `PAY_API_KEY_STAGING_EU`.

### Behaviour

| Variable | Effect |
|---|---|
| `PAY_OUTPUT` | default `--output` format |
| `PAY_TIMEOUT`, `PAY_DEADLINE` | per-request and whole-command budgets |
| `PAY_CONCURRENCY` | parallel requests (default 8, hard cap 32) |
| `PAY_NO_CACHE`, `PAY_REFRESH` | bypass or force-refresh discovery |
| `PAY_NO_AUDIT` | disable the write audit log |
| `PAY_YES` | assume `--yes`. Use with care |
| `PAY_NO_COLOR`, `NO_COLOR` | suppress colour in human output |
| `PAY_LOG_LEVEL`, `PAY_LOG_FORMAT` | `debug`\|`info`\|`warn`\|`error`; `text`\|`json` |

### Paths and updates

| Variable | Effect |
|---|---|
| `PAY_HOME` | move config, cache and state at once |
| `PAY_CONFIG_DIR`, `PAY_CACHE_DIR`, `PAY_STATE_DIR` | move one of them (wins over `PAY_HOME`) |
| `PAY_NO_UPDATE` | disable every self-update network call |
| `PAY_UPDATE_STRICT` | refuse an unverifiable release |
| `PAY_UPDATE_URL` | private mirror base for release artefacts |
| `GH_TOKEN` | authenticate the GitHub release API |

### Testing

| Variable | Effect |
|---|---|
| `PAY_TEST_BASE_URL`, `PAY_TEST_API_KEY` | required by `make test-live` and `make fixtures` |

---

## Troubleshooting

**Start here.**

```sh
pay doctor
```

It checks, in order: configuration, base URL reachability, whether the endpoint is
actually Payload, the credential, the auth collection, admin access, the discovery cache,
the audit log's writability, the installed skill's freshness and clock skew — and prints
the fix for each thing that is wrong.

### `pay: not found` after installing

`$HOME/.local/bin` is not on your `PATH`:

```sh
export PATH="$HOME/.local/bin:$PATH"     # add to ~/.bashrc or ~/.zshrc
```

### `config_missing` (exit 9)

No base URL for the selected profile.

```sh
pay config explain            # every setting and which layer set it
pay auth login --profile dev --base-url http://localhost:3900 --api-key-stdin
```

### `auth_invalid` (exit 2)

The key is wrong, revoked, or belongs to a different collection.

```sh
pay auth test                 # verifies the credential against /me
pay auth list                 # which profile, which base URL, where the credential lives
```

Payload returns **HTTP 200 with `user: null`** for an unauthenticated `/me`, which is why
this is `auth_invalid` rather than a network error. Check that `useAPIKey: true` is set on
the collection and that the user's **Enable API Key** box is actually ticked.

### `auth_insecure_permissions` (exit 2)

`credentials.json` is readable by more than you.

```sh
pay auth fix-perms
```

### `config_secret_in_plaintext` (exit 9)

A config file contains an `api_key` key. PayCLI will not load it. Move the credential to
`pay auth login`, the keychain, a `credential_helper`, or `PAY_API_KEY`.

### `endpoint_not_payload` (exit 9)

The base URL responds, but `GET <base>/api/access` is not a Payload access map — usually a
reverse proxy, a login page, or a wrong `--api-path`. If the project customises
`routes.api`:

```sh
pay --api-path /cms-api explain
pay config set profiles.dev.api_path /cms-api
```

### `graphql_disabled` (exit 10)

The project disabled GraphQL, or introspection. PayCLI degrades to REST probes
automatically and records what it could not learn in `manifest.limitations[]`; it does not
fail. If you see this from `pay raw POST /api/graphql`, it is the endpoint itself.

### Everything is slow on the first call

That is discovery. One cold run, then cached for 10 minutes with a 24-hour hard maximum.

```sh
pay cache warm                # pay for it once, deliberately
pay cache info                # how fresh is the current scope
```

In a container image build, `pay cache warm` at build time makes the first real command a
local read.

### The keychain prompts, or hangs

```sh
export PAY_KEYRING=off        # file-based credentials only
```

The keychain is opt-in and every call is bounded at 2 seconds; an unreachable keychain is
a warning, not a failure. `PAY_KEYRING=off` skips loading the backend entirely.

### An update was applied but `pay version` is unchanged

That is by design. `update.auto` swaps the binary in a detached child; the new version
takes effect on the next invocation. `pay update-self --apply` in the foreground has the
same property — it exits having done only the update.

### `update_verification_failed` (exit 1)

A checksum or signature was present and did not match. **Do not work around this.** Report
it as a security issue. `PAY_UPDATE_STRICT` deliberately cannot relax it.

### Reporting a bug

```sh
pay doctor --output json > doctor.json
pay version --output json
```

`doctor.json` is redacted and safe to attach. Open an issue at
<https://github.com/KLIXPERT-io/pay-cli/issues>.
