# pay — the Payload CMS CLI

[![Latest release](https://img.shields.io/github/v/release/KLIXPERT-io/pay-cli?sort=semver)](https://github.com/KLIXPERT-io/pay-cli/releases/latest)
[![ci](https://github.com/KLIXPERT-io/pay-cli/actions/workflows/ci.yml/badge.svg)](https://github.com/KLIXPERT-io/pay-cli/actions/workflows/ci.yml)

Drive **any** Payload CMS 3.x project from the command line — discover its collections,
query documents, create, update, delete, manage globals, versions, drafts, locales and
uploads — through the REST API that project already exposes.

PayCLI is built for **LLM coding agents first** and humans second. That is not a slogan,
it is the set of design constraints:

* **One envelope, always.** Every command prints a single JSON object with the same
  shape: `ok`, `data_kind`, `data`, `error`, `meta`, `warnings`. Success and failure are
  the same shape. Output is **never** TTY-dependent.
* **Exit codes mean something.** Twelve classes, from `2` (auth) to `10` (this project
  cannot do that). An agent branches on the exit code and `error.code`, never on prose.
* **It discovers the project.** One cold run learns the collections, globals, field
  schemas, capabilities, id types and locales, then caches them. `pay explain` answers
  "what can I do here?" in a single call.
* **It fails locally, before the network.** An unknown `--sort` field returns **200 and
  silently unsorted data** from Payload. PayCLI rejects it with `invalid_sort_field`,
  exit 5, and the list of fields that do work. Same for unknown collections, unknown
  `--select` keys, bad enum values and uncastable ids.
* **It is never a dead end.** `pay raw` reaches any endpoint PayCLI does not model —
  custom collection endpoints, `POST /api/graphql`, plugin routes — with auth, retry,
  redaction, audit and write-safety intact.
* **Secrets never reach stdout.** Not in output, not in logs, not in the audit log, not
  in the cache, not in error messages. A Payload project with `useAPIKey` returns the
  API key in plaintext from `GET /api/users/me`; PayCLI redacts it on the way past.
* **It is strictly an API client.** PayCLI never writes to your Payload project's source
  files. Not `payload.config.ts`, not a collection file, not `package.json`. Every change
  it makes goes through the API. This is enforced in CI.

Single static binary. No Node, no Python, no runtime. Works against a Payload project you
did not write and cannot read the source of.

---

## Install

macOS / Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/KLIXPERT-io/pay-cli/main/install.sh | sh
```

Windows (PowerShell):

```powershell
irm https://raw.githubusercontent.com/KLIXPERT-io/pay-cli/main/install.ps1 | iex
```

Or with Go:

```sh
go install github.com/KLIXPERT-io/pay-cli/cmd/pay@latest
```

The installer verifies the archive against `checksums.txt` and, when `cosign` is on your
PATH, verifies the signature over `checksums.txt` too. See
[INSTALL.md](./INSTALL.md) for manual downloads, version pinning, air-gapped installs and
the self-update model.

```sh
pay version
pay --help
```

---

## Setup

### 1. Get an API key from your Payload project

PayCLI authenticates as a **user in an auth-enabled collection** — normally `users`.
Payload's API-key auth has to be turned on for that collection:

```ts
// src/collections/Users.ts
export const Users: CollectionConfig = {
  slug: 'users',
  auth: {
    useAPIKey: true,        // <- this
  },
  // ...
}
```

Then, in the Payload admin panel, open the user you want the CLI to act as, tick
**Enable API Key**, and copy the generated key.

> An API key carries that user's full access-control profile. Create a dedicated user for
> automation rather than handing an agent your own admin account, and give it only the
> collection permissions it needs. `pay can update pages` tells you exactly what a key can
> do before you trust it with anything.

### 2. Log in

```sh
pay auth login --profile dev --base-url http://localhost:3900 --api-key-stdin <<< "$PAYLOAD_API_KEY"
```

`--api-key-stdin` reads one line and keeps the key out of your shell history and out of
`ps`. `login` verifies the key against `GET /api/users/me`, discovers which collection the
key belongs to, and stores it in `~/.config/pay/credentials.json` with mode `0600`.

Prefer your OS keychain:

```sh
pay auth login --profile dev --base-url http://localhost:3900 --api-key-stdin --keyring
```

Or skip storage entirely — PayCLI reads the environment first:

```sh
export PAY_BASE_URL=http://localhost:3900
export PAY_API_KEY=...
pay whoami
```

Check it worked:

```sh
pay whoami
pay auth status
```

### 3. Discover the project

```sh
pay discover          # one cold run; cached for 10 minutes, hard max 24 hours
pay explain           # what can I do here? — offline after the first discover
```

`pay explain` is the command to run first, every time, and the command to hand an agent.
It is the answer to *what collections exist, what can this key do with them, which ones
have drafts or versions or uploads, what are the locales, what are the id types*.

---

## Multi-profile

A profile is one `(base URL, api path, auth collection, credential)` tuple. Profiles are
how you keep `local`, `staging` and `prod` apart, and PayCLI keys its cache by the
connection **and** the credential fingerprint, so two keys with different permissions
never see each other's view of what is readable.

```sh
pay auth login --profile local --base-url http://localhost:3900     --api-key-stdin
pay auth login --profile prod  --base-url https://cms.example.com   --api-key-stdin --keyring

pay auth list                 # every profile, its base URL and where its credential lives
pay auth use local            # set the default
pay find pages --profile prod # or override per command
```

Profiles also live in config, which is where the non-secret settings belong:

```toml
# ~/.config/pay/config.toml
version = 1
default_profile = "local"

[defaults]
output      = "json"
depth       = 0
limit       = 20
timeout     = "30s"
max_bulk    = 100

[profiles.local]
base_url        = "http://localhost:3900"
auth_collection = "users"

[profiles.prod]
base_url        = "https://cms.example.com"
auth_collection = "users"
label           = "production — writes are audited"
```

A **credential never goes in a config file.** PayCLI refuses to load a config containing
an `api_key` key at any nesting depth and tells you where to put it instead
(`config_secret_in_plaintext`, exit 9). Use `pay auth login`, the keychain, a
`credential_helper`, or an environment variable.

A project-local `pay.toml` at your repository root layers on top of the user config, so a
checked-in `pay.toml` can pin `base_url` and `auth_collection` for the whole team without
carrying a secret.

```sh
pay config paths      # where everything lives
pay config explain    # every resolved setting and exactly which layer set it
```

---

## Use with coding agents

PayCLI ships an agent skill — the envelope, the exit-code table, the query DSL and the
handful of Payload behaviours that produce confidently wrong answers when guessed.

```sh
pay skills install
```

That copies the skill into every agent skills directory that already exists under your
project root — `.claude/skills/pay/`, `.codex/`, `.cursor/`, `.gemini/`, `.antigravity/`,
`.opencode/`, `.windsurf/`, `.continue/`, `.crush/`, `.kiro/`, `.qwen/`, `.qoder/` — and
never creates one for an agent you do not use.

```sh
pay skills install --with-project-context   # + references/PROJECT.md for THIS project
pay skills install --global                 # into $HOME instead of the project
pay skills install --agent claude,codex     # only these, creating the directories
pay skills install --dir ~/.config/anything/skills
pay skills status                           # what is installed, and is it stale
pay skills update                           # refresh to this binary's version
```

`--with-project-context` runs discovery and writes `references/PROJECT.md`: your actual
collection table, globals, auth collection, locales, and three examples written against
real slugs. It never contains a credential, and `base_url` is redacted. Files you edit are
**skipped with a warning** on reinstall, never silently overwritten.

Not using a skills-aware agent? `pay skills print --output raw > SKILL.md` gives you the
same document.

---

## Quick tour

Everything below is real output shape from a real Payload 3.x project — a website with
`pages`, `posts`, `media`, `categories`, `forms` and a bolted-on CRM.

### What is here?

```sh
pay collections
pay collections --capability upload          # media, crm-attachments
pay collections --kind auth                  # users
pay collections --grep crm --include-internal
pay describe pages                           # fields, types, required, queryable, sortable
pay describe pages --field hero.links        # one field, including block slugs
```

### Read

```sh
pay find pages --limit 5
pay find pages --select title,slug,_status --sort -updatedAt --limit 10
pay find pages --where 'slug equals home'
pay find pages --where '_status equals published' --where 'title contains guide'
pay find pages --or 'title contains guide' --or 'title contains tutorial'
pay find posts --since 7d --date-field updatedAt
pay find crm-contacts --q 'acme' --q-fields name,email
pay get pages 16
pay get pages 16 --depth 1 --select title,hero
pay count pages
```

Pagination, without writing a loop:

```sh
pay find pages --all --max 1000 --output jsonl > pages.jsonl
```

Only ids:

```sh
pay find pages --where '_status equals draft' --output id
```

### Drafts, versions, globals

**The draft rule, which everyone gets wrong once:** a read *without* `--draft` does **not**
filter out unpublished documents. Payload returns never-published documents from a plain
read. To get only published content you must say so:

```sh
pay find pages --published-only     # actually published
pay find pages --draft-only         # only drafts
pay find pages --draft              # draft versions where they exist
```

```sh
pay versions list pages --id 16
pay versions get pages <versionId>
pay versions diff pages <vA> <vB>
pay versions restore pages <versionId> --yes

pay globals list
pay globals get header
pay globals update header --set 'navItems.0.link.label=Docs' --yes
```

### Write

Writes are classified L0–L3 and the risky ones need `--yes`. Everything L1 and above is
written to a local audit log **before and after** the call, so an interrupted destructive
operation still leaves a trace.

```sh
pay create posts --set title='Hello world' --set slug=hello-world --draft
pay create posts --data-file ./post.json
pay update pages 16 --set title='New title' --yes
pay update pages 16 --publish --yes
pay delete pages 16 --yes                       # soft delete when trash is enabled
pay delete pages 16 --permanent --yes           # irreversible; says so
pay restore pages 16 --yes                      # un-trash
pay duplicate pages 16
```

Bulk writes always resolve their blast radius first and print it:

```sh
pay delete pages --where '_status equals draft' --dry-run
# -> {"ok":true,"data_kind":"op_result","data":{"would_affect":7,"sample_ids":[...],
#     "request":{"method":"DELETE","url":"...","body":null}},"meta":{"dry_run":true,...}}

pay delete pages --where '_status equals draft' --max-docs 10 --yes
pay update pages --where 'category equals 3' --set featured=true --all --yes
```

`--max-docs` caps the damage; exceeding it is `bulk_limit_exceeded` (exit 5) with the
count and the two ways forward. `--all` is the explicit "yes, everything that matches".

> **Whole-document validation.** Payload validates the **entire** document on every
> update, not just the fields you sent. A validation error can therefore name a field you
> never touched. PayCLI marks which is which: `error.fields[].sent` is `true` for the
> fields your command supplied and `false` for pre-existing invalid data.

### Files

```sh
pay upload media ./logo.png --alt 'Company logo'
pay upload media - --filename shot.png < screenshot.png
pay upload media https://example.com/x.jpg --allow-remote
pay download media 42 -o ./out.png
pay download media --filename logo.png --size thumbnail -o -
```

### Locales

```sh
pay get pages 16 --locale de
pay find pages --locale de --fallback-locale none
```

> **The locale rule.** `--locale de` on an untranslated field returns the **default
> locale's** text, not an empty value — unless `fallback-locale=none`, which PayCLI sends
> by default and reports in `meta.locale` so you always know which you got.

### Anything else

```sh
pay raw GET users/me
pay raw GET pages --query 'where[slug][equals]=home' --query limit=1
pay raw POST /api/graphql --data '{"query":"{ Pages { totalDocs } }"}'
pay raw POST media --file ./logo.png --data '{"alt":"Logo"}'
pay raw DELETE pages/42 --yes
```

`pay raw` applies the same auth, retry, redaction, audit and risk classification as every
other command. `DELETE` still needs `--yes`.

### Maintenance

```sh
pay doctor                       # connection, auth, cache, audit log, skill, clock
pay cache info                   # freshness of this profile's discovery
pay cache ls                     # every cached scope and why they are separate
pay cache warm                   # run discovery now, so the next command is local
pay cache clear --all
pay audit tail -n 50 --action delete
pay update-self --check
```

`rm -rf "$(pay cache path --output raw)"` is always safe. It costs one re-discovery.

---

## The envelope

Every command, success or failure, prints one JSON object:

```json
{
  "ok": true,
  "v": 1,
  "command": "find",
  "data_kind": "doc_list",
  "data": [
    { "id": 16, "title": "PayCLI Probe 2", "slug": "paycli-probe-2", "_status": "draft" }
  ],
  "page": {
    "limit": 20, "page": 1, "total_pages": 1, "total_docs": 11, "returned": 1,
    "has_next_page": false, "has_prev_page": false
  },
  "meta": {
    "request_id": "01K5…", "cli_version": "0.1.0", "profile": "local",
    "base_url": "http://localhost:3900", "api_path": "/api", "auth_mode": "api-key",
    "duration_ms": 41, "http_requests": 1, "retries": 0,
    "cache": { "discovery": "hit", "age_s": 92, "ttl_s": 600 }
  },
  "warnings": []
}
```

`data_kind` tells you what `data` is without inspecting it: `doc`, `doc_list`, `count`,
`global`, `version`, `version_list`, `bulk_result`, `capabilities`, `schema`,
`command_spec`, `op_result`, `raw`, `error`.

On failure, `ok` is `false`, `data` is absent, and `error` is present:

```json
{
  "ok": false,
  "v": 1,
  "command": "find",
  "data_kind": "error",
  "error": {
    "code": "invalid_sort_field",
    "exit": 5,
    "message": "\"titel\" is not a sortable field on pages.",
    "hint": "Sortable fields: createdAt, id, publishedAt, slug, title, updatedAt.",
    "retriable": false,
    "confidence": "certain",
    "did_you_mean": ["title"],
    "docs": "pay explain --section exit_codes"
  },
  "meta": { "…": "…" },
  "warnings": []
}
```

**Branch on `.ok` and the exit code, never on `error.message`.** Payload's messages are
translated — the same failure reads differently under `Accept-Language: de`. The `code`
and the field paths are stable; the prose is not.

### Other output formats

| `--output` | For | Notes |
|---|---|---|
| `json` | **agents — the default** | one pretty envelope |
| `jsonl` | streaming large reads | bare documents on stdout, envelope on stderr |
| `id` | shell pipelines | one id per line, nothing else |
| `raw` | Payload's own body | verbatim, after redaction unless `--no-redact` |
| `csv` | humans, spreadsheets | RFC 4180, CRLF |
| `table` | humans | aligned, width-truncated. Never recommended to agents |

`--path` extracts from `.data` without a jq dependency. It supports exactly three forms —
`.a.b`, `.a[0]` and `.a[]` — and nothing else:

```sh
pay find pages --path '.[].slug'
pay get pages 16 --path '.title'
```

This is not jq. Pipe the envelope to jq when you want jq.

---

## Exit codes

| Exit | Class | Means | Typical codes |
|---:|---|---|---|
| `0` | OK | it worked | — |
| `1` | internal | a PayCLI bug, a corrupt cache, an unwritable audit log | `internal`, `cache_corrupt`, `audit_write_failed` |
| `2` | auth | the credential is missing, wrong, expired or locked | `auth_missing`, `auth_invalid`, `auth_required` |
| `3` | throttled | back off and retry | `rate_limited`, `doc_locked`, `server_busy` |
| `4` | not found | the document, route or version does not exist | `doc_not_found`, `route_not_found` |
| `5` | validation | **your input is wrong** — fix it and retry | `validation_failed`, `invalid_sort_field`, `unknown_field`, `invalid_args` |
| `6` | network | DNS, TLS, timeout, or the server is down | `timeout`, `network_unreachable`, `server_error` |
| **`7`** | **partial** | **some items succeeded and some failed** | `partial_failure` |
| `8` | access denied | authenticated, but not permitted | `access_denied` |
| `9` | config | no base URL, unknown profile, secret in plaintext config | `config_missing`, `profile_unknown` |
| `10` | capability | **this project cannot do that** | `collection_unknown`, `feature_unavailable`, `graphql_disabled` |
| `11` | confirmation | a destructive operation needs `--yes` | `confirmation_required` |

**Exit 7 is the one to handle explicitly.** A bulk write that partially succeeded is not a
failure to retry wholesale — re-running it would re-apply the half that worked. Read
`error.failures[]`, which names every item that failed and why, and retry only those. The
envelope's `next` block gives you the command.

`pay explain --section exit_codes` prints this table live, from the binary you are
running.

---

## Configuration reference

Every setting resolves through the same chain, highest wins:

1. command-line flag
2. environment variable (`PAY_*`, and `PAY_*_<PROFILE>` for per-profile overrides)
3. project config (`./pay.toml`, found by walking up to the git root)
4. user config (`~/.config/pay/config.toml`)
5. built-in default

`pay config explain` prints every resolved value **and the layer that set it**, which is
the fastest way to answer "why is it talking to the wrong server".

Common environment variables:

| Variable | Effect |
|---|---|
| `PAY_PROFILE` | profile to use |
| `PAY_BASE_URL` | Payload origin |
| `PAY_API_KEY`, `PAY_API_KEY_<PROFILE>` | the credential |
| `PAY_JWT`, `PAY_JWT_<PROFILE>` | a bearer token instead of an API key |
| `PAY_AUTH_COLLECTION` | auth collection slug (default `users`, `auto` discovers it) |
| `PAY_CONFIG_DIR`, `PAY_CACHE_DIR`, `PAY_STATE_DIR`, `PAY_HOME` | directory layout |
| `PAY_KEYRING` | `auto` \| `off` \| `force` |
| `PAY_NO_AUDIT` | disable the write audit log |
| `PAY_NO_UPDATE` | disable every self-update network call |
| `PAY_UPDATE_STRICT` | refuse an unverifiable release |
| `PAY_YES` | assume `--yes` (use with care) |

Full list: [INSTALL.md](./INSTALL.md) and `pay explain --section connection`.

---

## Security

* **Credentials are never written to a config file.** PayCLI refuses to load one that
  contains an `api_key` key at any depth, and tells you the three places it belongs.
* **`credentials.json` is `0600`**, and PayCLI refuses to read it when the permissions are
  wider (`auth_insecure_permissions`, exit 2; `pay auth fix-perms` repairs it).
* **The OS keychain is opt-in**, never the default, and never blocks: a keychain that is
  unreachable is a warning, not a failure.
* **Redaction is structural, not textual.** Secret-shaped keys (`apiKey`, `hash`, `salt`,
  `password`, `sessions`, `resetPasswordToken`, anything matching `token` or `secret`),
  JWT-shaped values, URL userinfo and secret query parameters are masked on every path out
  of the program: stdout, stderr, logs, the cache, the audit log, `--dry-run` previews and
  error messages. `--no-redact` opts out, deliberately and per invocation.
* **The audit log never contains a credential**, an `Authorization` header in any form, or
  a document body.
* **Releases are signed.** Archives are checksummed, `checksums.txt` is cosign-signed and
  the artefacts carry a GitHub build-provenance attestation. `pay update-self` verifies
  both and **always aborts** on a mismatch — `PAY_UPDATE_STRICT` cannot relax that.
* **PayCLI never writes to your project's source.** Enforced by `scripts/arch-lint.sh` in
  CI, not by convention.

---

## Compatibility

PayCLI targets **Payload 3.0 and later**, best-effort. There is no hard version floor: it
discovers what the project supports and adapts, and every capability it could not
establish stays `null` — *unknown*, which means "attempt the operation and classify the
answer" rather than "refuse". When a project reports a version older than 3.0 it is a
warning, never a refusal.

What that buys you: PayCLI works against a Payload project with a custom `routes.api`, a
non-`users` auth collection, GraphQL disabled, introspection disabled, `endpoints: false`
on a collection, plugin routes that shadow built-in ones, Postgres or MongoDB or SQLite,
localisation on or off. Where it cannot learn something, it says so in
`manifest.limitations[]` and falls back to trying.

`pay doctor` is the one command that tells you what is and is not working, in order,
with the fix for each.

---

## Contributing

```sh
make check        # exactly what CI runs: fmt, vet, staticcheck, arch-lint, tests
make test         # offline unit tests, -race -shuffle=on
make test-live    # integration tests; needs PAY_TEST_BASE_URL + PAY_TEST_API_KEY
make build        # ./pay
make snapshot     # full release artefacts, locally, without publishing
```

`make lint` runs [`scripts/arch-lint.sh`](./scripts/arch-lint.sh), which enforces the
architectural rules that cannot be expressed as types — process-surface confinement,
single-point HTTP request construction, the redaction boundary, atomic writes, and the
"PayCLI never writes project source" invariant. A violation is a build failure.

Releases: bump [`VERSION`](./VERSION) on `main`. That is the whole process — CI creates the
tag and runs the release.

The design is documented in [`docs/ARCHITECTURE.md`](./docs/ARCHITECTURE.md), and the
Payload behaviours it is built around — all verified against a live instance — are in
[`docs/GROUNDING.md`](./docs/GROUNDING.md).

---

## License

MIT. See [LICENSE](./LICENSE).
