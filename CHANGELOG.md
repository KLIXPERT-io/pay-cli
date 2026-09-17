# Changelog

All notable changes to PayCLI are recorded here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the
project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Releases are cut by bumping [`VERSION`](./VERSION) on `main`; CI creates the matching `v*`
tag and publishes the release. The per-release notes on the
[releases page](https://github.com/KLIXPERT-io/pay-cli/releases) are generated from commit
messages; this file records the changes that matter to someone deciding whether to
upgrade.

## What counts as a breaking change

PayCLI is consumed by programs, so the compatibility surface is larger than the flag list.
The following are **breaking** and only change in a major release:

- the envelope's shape and its `v` field;
- the meaning of an exit code;
- the spelling of an `error.code`;
- the set of `data_kind` values;
- the `--output` format grammars;
- the release archive name template (`pay_<version>_<os>_<arch>`), which is **frozen
  forever** — an installed binary constructs it to find its own update.

Adding a field to the envelope, adding a `warning.code`, adding a command or a flag, and
improving an `error.message` or `hint` are **not** breaking. Never branch on
`error.message`: it is translated by the server.

---

## [Unreleased]

### Added

- **Block field schemas.** `pay describe <entity> --block <slug>` prints what is INSIDE a
  block type — its fields, their payload types, enum options, relationship targets and
  `write_shape` — instead of only the slugs a blocks field accepts. `--blocks-detail`
  inlines every reachable block type on the collection and `--field` views; it is opt-in
  because the interiors are several times the size of the rest of the answer.
  `id`, `blockName` and `blockType` are marked `plumbing: true`: they are Payload's keys,
  not content, and `blockType` is the one that must be sent.
- Block schemas are discovered from each union member's GraphQL OBJECT type in the same
  adaptive `__type` batch as every other leaf — measured on the live project, that is one
  extra batched request (17 → 18 requests, 7 → 8 GraphQL batches, +15 KB; cold-run time
  stays inside run-to-run noise: 3.9–5.0 s before, 4.1–5.3 s after, three runs each) — and
  are persisted in the field shard as `block_schemas`, so they are cache-backed and
  offline after discovery.
- Required-ness inside a block is tri-state with provenance: a GraphQL `NON_NULL` proves
  `required: true`, the block's own `config.ts` supplies the rest for a project's own
  blocks, and anything neither source answers stays `null` with `required_source:
  "unknown"`, a named `required_unknown` list, a reason, and a `block_required_unknown`
  warning. Payload publishes no input type for a block type, so it is never guessed.
- `shard.block_fields[].slug_interface_names` records which GraphQL union member each
  blockType slug came from. A shard written before `block_schemas` existed still decodes
  and still resolves slugs; the block interiors report as not discovered until the next
  `pay discover --refresh`.

## [0.1.0] — unreleased

First public release.

### Added

**Discovery**
- `pay discover`, `pay explain`, `pay collections`, `pay describe` — one cold run learns
  the collection and global inventory, per-entity field schemas, capability flags, id
  types, locales and the permission map, then serves everything from a local cache.
- Graceful degradation at every stage: GraphQL disabled, introspection disabled,
  `endpoints: false` on a collection, custom routes shadowing built-in ones, and
  access-denied entities all produce a reduced but correct picture, recorded in
  `manifest.limitations[]`, rather than a failure.
- Capabilities that could not be established stay `null` — *unknown* — which means
  "attempt the operation and classify the answer", never "refuse".

**Read**
- `pay find`, `pay get`, `pay count` with a `--where PATH OP VALUE` mini-DSL over all 16
  Payload operators, `--or`, `--where-json`, `--where-raw`, `--q` full-text, `--since`,
  `--sort`, `--select`, `--select-exclude`, `--populate`, `--joins`, `--depth`, `--all`
  with automatic pagination, `--draft` / `--draft-only` / `--published-only`, `--trash`.
- Client-side validation before the network for the inputs Payload answers **200 and
  wrong**: unknown sort fields, unknown select keys, unknown where paths, invalid enum
  values, uncastable ids, `--limit 0`, and locales the project does not have.

**Write**
- `pay create`, `pay update`, `pay delete`, `pay restore`, `pay duplicate`,
  `pay publish` / `pay unpublish`, `pay globals update`, `pay versions restore`.
- Combinable, deep-merged `--data` / `--data-file` / `--set` / `--set-json`.
- Four-level risk classification (L0–L3) with `--yes`, `--dry-run` and a resolved blast
  radius printed before every bulk write. `--max-docs` caps it; `--all` is the explicit
  opt-in to everything that matches.
- A local JSONL audit log of every L1–L3 write, **before and after** the call, so an
  interrupted destructive operation still leaves a trace. The pre-record failing aborts
  the operation; the post-record failing is a warning only.

**Files, globals, versions, locales**
- `pay upload` (file, stdin or remote URL), `pay download`.
- `pay globals list|get|update`, `pay versions list|get|diff|restore`.
- Locale handling that reports what it actually got: `--fallback-locale none` is sent by
  default, and `meta.locale` always names the requested and fallback locales.

**Auth and configuration**
- `pay auth login|list|use|logout|test|status|rename|fix-perms`, API key and JWT.
- Opt-in OS keychain, `credential_helper` support, `0600` credential files, and a
  ten-step resolution chain whose source is reported in `meta`.
- Named profiles, a user config, a project-local `pay.toml`, `PAY_*` environment
  variables, and `pay config explain`, which prints every resolved value **and the layer
  that set it**.
- Configs containing a plaintext `api_key` at any nesting depth are rejected.

**Output**
- One envelope for everything: `ok`, `v`, `command`, `data_kind`, `data`, `error`, `meta`,
  `warnings`, never TTY-dependent.
- `--output json|jsonl|id|raw|csv|table`, and `--path` with a deliberately tiny three-form
  grammar (`.a.b`, `.a[0]`, `.a[]`) so nobody mistakes it for jq.
- Twelve exit-code classes, a stable `error.code` vocabulary, `did_you_mean` suggestions,
  per-field validation detail marking which fields your command actually sent, and a
  `next` block naming the follow-up command.

**Operations**
- `pay raw <METHOD> <path>` — the guarantee that PayCLI is never a dead end, with the same
  auth, retry, redaction, audit and risk classification as every other command.
- `pay cache info|ls|path|show|clear|warm`, `pay audit tail`, `pay doctor`,
  `pay completion`, `pay version`.
- `pay skills install|update|status|list|print|uninstall` — the agent skill, embedded in
  the binary, installable into twelve agent layouts plus any directory you name, with a
  hash manifest so a reinstall never silently overwrites a file you edited.

**Distribution**
- Signed releases for darwin, linux and windows on amd64 and arm64, with SBOMs, cosign
  signatures over `checksums.txt` and GitHub build-provenance attestations.
- `install.sh` and `install.ps1` that verify checksums (and signatures, when `cosign` is
  present) and refuse to install on a mismatch.
- `pay update-self` with three verification outcomes — `verified`, `unverifiable`,
  `verification_failed` — where a failure **always** aborts and `PAY_UPDATE_STRICT` cannot
  relax it. Automatic updating is **off by default**.

### Security

- Secrets never reach stdout, stderr, logs, the cache, the audit log, `--dry-run`
  previews or error messages. Redaction is structural (key names, JWT-shaped values, URL
  userinfo, secret query parameters), not a string blacklist.
- PayCLI is strictly an API client: it never writes to a Payload project's source files.
  Enforced in CI by `scripts/arch-lint.sh`, not by convention.

### Known limitations

- **No MCP server mode.** PayCLI is a CLI; agents drive it through the shell and the
  envelope. This is a deliberate v0.1.0 scope decision, not a permanent one.
- **Binaries and script installers only.** Homebrew and `.deb`/`.rpm`/`.apk` packaging is
  configured in `.goreleaser.yaml` but commented out: the tap repository and a package
  signing story do not exist yet. `go install` works today.
- **Payload 3.0+, best-effort, no hard floor.** A project reporting a version older than
  3.0 produces a warning, never a refusal. Older projects may hit capabilities PayCLI
  cannot discover; those surface as `limitations[]` and fall back to attempting the
  operation.
- `--verify-sort` and `--allow-write-probes` are opt-in because they cost extra requests
  and, in the second case, write to the project.

[Unreleased]: https://github.com/KLIXPERT-io/pay-cli/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/KLIXPERT-io/pay-cli/releases/tag/v0.1.0
