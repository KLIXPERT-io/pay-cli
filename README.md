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
* **It shows a write before it makes it.** Every `--dry-run` of a write to an existing
  document carries a structural diff of what would change, and every document a write
  changes can be backed up automatically first. A save that would silently take a live
  page offline is refused until you say `--draft`, `--publish` or `--unpublish`.
* **It learns the content model from the content.** Where the project hides its schema
  (GraphQL introspection is off in production by default), a block census learns every
  block type, its fields and its typical values from the documents themselves, so an
  agent can build and validate a block row without reading the app's source.
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
handful of Payload behaviours that produce confidently wrong answers when guessed. It
lives in this repository at [`skills/pay/`](skills/pay/), and there are two ways to
install it.

**1. From the binary (offline, recommended).** The skill is compiled into `pay`, so this
needs no network and no Node:

```sh
pay skills install
```

**2. With the [`skills`](https://github.com/anthropics/skills) CLI**, if you would rather
not install PayCLI first, or you want the skill in a tool PayCLI does not know about:

```sh
npx skills add https://github.com/KLIXPERT-io/pay-cli/skills --skill pay
```

Both routes copy the *same* files — `skills/pay/` is a Go package that owns the
`go:embed`, so the tree inside the binary is the tree in the repository, and there is only
ever one copy to keep up to date. Only route 1 can also write `references/PROJECT.md` for
the project you are standing in, and only route 1 records `.pay-skill.json` so
`pay skills status` can tell you when your copy has gone stale.

`pay skills install` copies the skill into every agent skills directory that already exists under your
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

**Make sure the agent can find `pay`.** Agents run commands in non-interactive shells,
which usually read neither `~/.bashrc` nor `~/.profile`. A PATH entry you added there works
in your terminal, but the agent gets "command not found". Run `pay doctor` from the agent's
shell. Its `binary_on_path` check prints the exact fix. The simplest fix is a link from a
directory that is already on the agent's PATH:
[INSTALL.md](./INSTALL.md#pay-not-found-after-installing).

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
pay find crm-contacts --q 'acme'
pay get pages 16
pay get pages 16 --depth 1 --select title,hero
pay get pages --slug about                   # 0 matches: doc_not_found; 2+: slug_ambiguous
pay get pages 16 --published-only            # the live state; not_published (exit 4) if none
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
pay versions diff pages <vA> <vB>            # rows matched by id, rich text as text
pay versions restore pages <versionId> --yes
pay diff pages 16 --draft-vs-published      # what publishing would take live

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
pay publish pages 38 39 40                      # one PATCH per id; exit 7 names the ids that failed
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

> **The pending-draft rule.** Payload merges an update into the *newest* version and, without
> `?draft`, stores the result as the live row, draft `_status` included. So a plain save of a
> published page that has a pending draft would publish the draft's changes **and** take the
> page offline. PayCLI refuses that write (`invalid_args`, exit 5) and asks you to choose:
> `--draft` (edit the draft, the live page is untouched), `--publish` (publish the draft with
> your change) or `--unpublish` (take it offline deliberately). Under `--draft`, a
> `_status: "published"` in the body is removed rather than obeyed (`body_publishes`), and
> `--draft --publish` together is `invalid_args`.

### Blocks

Payload's blocks fields hold most of a page. With GraphQL introspection disabled (the
production default) the API does not tell you which block types exist or what is inside
them. PayCLI's **block census** learns this from the documents themselves. The first run
reads each collection's documents (500 by default); the result is cached for 24 hours.

```sh
pay blocks types pages                           # which block types appear where, and how often
pay blocks types pages --field 'layout[].blocks' # one level down: what sits inside a group
pay blocks schema grid --collection pages        # fields, kinds, observed values, required-ness
pay blocks example grid --collection pages --n 2 # real rows, row ids stripped
pay blocks new hero --collection pages --set title='New page'   # a ready-to-edit row
pay blocks learn pages                           # rebuild after a deploy; reports what changed
```

Editing is a pipe: read one document, transform it locally, write back **only the fields you
touched**. The transforms (`ls`, `mv`, `rm`, `add`, `cp`, `set`) need no network, and they work
at any depth:

```sh
pay get pages 47 --draft --depth 0 | pay blocks ls --recursive       # every row, with its --field and selector
pay get pages 47 --draft --depth 0 \
  | pay blocks mv last --first --field 'layout[type:group[0]].blocks' \
  | pay apply --draft --dry-run                                     # data.diff shows exactly one move
pay get pages 47 --draft --depth 0 \
  | pay blocks add text --template --set heading=Ablauf --after first \
  | pay blocks validate                                             # exit 5 on {} rows, missing blockType, …
```

`pay outline` is the quickest way to read a page's structure. It prints one line per row at
any depth, with a summary of each row and flags for broken rows:

```sh
pay outline pages --slug home --draft --output table
pay outline pages 47 --draft --path '.counts.flagged'   # {} when nothing is wrong
```

> **Orphan rows (Postgres).** Changing the type of a *nested* block (a bento inside a group
> becomes a grid) leaves the old nested rows in the database. They read back as `{}` rows.
> PayCLI predicts this on a dry run (`orphan_risk`), detects it after a write (`orphan_rows`)
> and on read (`empty_rows`). `--reset-nested` makes the write clean and keeps the document
> id. `pay update pages 47 --reset-nested` with no body repairs a page that already has
> orphan rows.

### Rich text as Markdown

Payload stores rich text as Lexical JSON. PayCLI converts it in both directions, in the
shape Payload stores:

```sh
pay update posts 3 --set-md content=@body.md --draft           # Markdown into a richText field
pay create posts --set title=Launch --set slug=launch --set-md content=@launch.md --draft
pay get posts 3 --richtext md                                  # every rich-text value as Markdown
pay get pages 47 --draft --depth 0 | pay lexical text -o -     # what does this page say?
pay lexical from-md body.md --as content | pay update posts 3 --data @- --draft
```

The Markdown dialect adds `[text](pay:pages/47)` for internal links, `{target=_blank}`,
`![media:40]()` for uploads, and `<!-- lexical:… -->` placeholders that carry every other
node losslessly. A `--richtext md|text` read is for reading only: every write refuses it
(`richtext_rendered`), because Payload would store the string.

### Diffs and dry runs

```sh
pay diff pages 16 --data-file page-16.json     # what would this write change? nothing is sent
pay diff pages 16 --set title='New title'
pay diff pages 50 58                           # two documents
pay diff --left before.json --right after.json # two files, offline
pay update pages 16 --data-file page-16.json --draft --dry-run   # data.diff: the same change list
```

`data.identical` and `data.summary.fields` answer "does this touch only what I meant?".
`changes[]` lists `add`, `remove`, `change` and `move` with paths like
`layout[2].blocks[1].title`, and rich text appears as text. The exit code is 0 whether or
not the sides differ. Every `--dry-run` of `update`, `apply`, `globals update`, `publish`,
`unpublish`, `backups restore` and `versions restore` includes this diff, at the cost of one
or two extra GETs.

### Backups

Name a directory once and every write that changes or removes an existing document saves
it there first:

```sh
export PAY_BACKUP_DIR=~/pay-backups          # or backup_dir = ".pay-backups" in ./pay.toml
pay update pages 16 --set title='New' --draft   # meta.backups lists the file it wrote
pay backups list --collection pages --id 16
pay backups restore <file> --dry-run         # the exact PATCH, with its diff
pay backups restore <file> --yes
pay backups prune --older-than 30d --dry-run
```

A backup that cannot be written aborts the write (`backup_failed`, exit 1). `--no-backup` or
`PAY_NO_BACKUP=1` skips the backup, deliberately. `pay backups restore --recreate` brings
back a permanently deleted document; Payload assigns it a new id.

### Idempotent migrations: upsert, plan, sync

```sh
pay upsert pages --data-file about.json      # created | updated | unchanged, matched on slug
pay upsert crm-contacts --match email --data-file contact.json
pay plan ./content                           # what sync would do, per item, with diffs
pay sync ./content --dry-run
pay sync ./content --yes                     # exit 7 is safe to re-run: only what differs is written
```

A manifest is a JSON file holding one item or an array of items. A saved `pay get … --depth 0`
envelope also counts as a manifest:

```json
[{"key": "about", "collection": "pages", "status": "draft",
  "data": {"title": "About", "slug": "about"}},
 {"collection": "pages", "status": "draft",
  "data": {"title": "Team", "slug": "team", "parent": {"$ref": "@about"}}}]
```

`{"$ref": "pages", "slug": "about"}` works in **any** write body, and PayCLI resolves it to
the id before the request. `{"$ref": "@key"}` refers to another item of the same plan, and
`sync` creates items in dependency order.

### Frontend URLs and draft preview

```sh
pay url pages 47                   # {url, path, path_source, status, has_unpublished_changes}
pay url pages --slug about --draft # where the newest draft will live
pay url pages 47 --preview         # draft-preview handshake; saves the cookie to a 0600 file
```

The path comes from nested-docs breadcrumbs, then from a `url_paths` template
(`pay config set profiles.<p>.url_paths.posts '/posts/{slug}'`), then from the slug. The
host is `site_url`, which defaults to the Payload origin. `--preview` works with the Payload
website template's `/next/preview` route. It needs the site's preview secret, from
`PAY_PREVIEW_SECRET`, a variable named by `preview_secret_env`, or
`pay auth preview-secret --stdin`. It returns ready-made commands that load the cookie into
agent-browser or curl, so you can look at a draft without publishing it. PayCLI never prints
the secret or the cookie value.

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
pay doctor                       # connection, auth, cache, audit log, backups, block census, PATH, skill, clock
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
| `4` | not found | the document, route or version does not exist | `doc_not_found`, `route_not_found`, `selector_no_match` |
| `5` | validation | **your input is wrong** — fix it and retry | `validation_failed`, `invalid_sort_field`, `unknown_field`, `invalid_args`, `slug_ambiguous` |
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
| `PAY_BACKUP_DIR` | turn on pre-write backups into this directory |
| `PAY_NO_BACKUP` | skip the pre-write backup |
| `PAY_CENSUS_MAX_DOCS` | documents per collection the block census reads (default 500) |
| `PAY_SITE_URL` | the site's origin for `pay url` (default: the Payload origin) |
| `PAY_PREVIEW_SECRET`, `PAY_PREVIEW_SECRET_<PROFILE>` | the site's preview secret, for `pay url --preview` |

Full list: [INSTALL.md](./INSTALL.md) and `pay explain --section connection`.

Config-file keys for the same features (`pay config set KEY VALUE` writes them):

| Key | Where | Effect |
|---|---|---|
| `backup_dir` | top level of `config.toml` / `pay.toml` | backup directory; a relative path is relative to that file |
| `census_max_docs` | `[defaults]` | documents per collection the block census reads |
| `site_url` | `[profiles.<p>]` | the site's origin, when it is not the Payload origin |
| `url_paths.<collection>` | `[profiles.<p>]` | the site path of a collection, e.g. `"/posts/{slug}"` |
| `preview_path` | `[profiles.<p>]` | the site's preview route (default `/next/preview?path={path}&previewSecret={secret}`) |
| `preview_secret_env` | `[profiles.<p>]` | the name of an environment variable that holds the preview secret |

The preview secret itself, like an API key, is refused in any config file.

---

## Security

* **Credentials are never written to a config file.** PayCLI refuses to load one that
  contains an `api_key` key at any depth, and tells you the three places it belongs.
* **`credentials.json` is `0600`**, and PayCLI refuses to read it when the permissions are
  wider (`auth_insecure_permissions`, exit 2; `pay auth fix-perms` repairs it).
  **On Windows this check does not apply:** NTFS has no Unix mode bits, `os.Chmod` only
  toggles the read-only attribute, and Go reports `0666` regardless. Confidentiality there
  rests on the file living under your `%AppData%` profile directory. If you need a stronger
  guarantee on Windows, use `--keyring` (Windows Credential Manager) or a
  `credential_helper`.
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
make test-focus RUN=TestGet PKG=./internal/cli   # one test, verbose
make test-live    # integration tests; needs PAY_TEST_BASE_URL + PAY_TEST_API_KEY
make build        # ./pay
make snapshot     # full release artefacts, locally, without publishing
```

To drive the CLI by hand against a local Payload, `make run` and friends use a sandbox
`PAY_HOME` at `.pay-test/`, so a hand-run can neither read your real credentials nor
rewrite your real config:

```sh
make dev                        # build, log a sandbox 'local' profile in, run pay doctor
make run ARGS='get posts -l 3'  # run this build against that profile
make dev-shell                  # a subshell where `pay` IS this build
make dev-reset                  # throw the sandbox home away
```

Those need `PAY_TEST_BASE_URL` and `PAY_TEST_API_KEY` — export them, or write a
gitignored `.env.local` that the Makefile includes:

```make
PAY_TEST_BASE_URL ?= http://localhost:3900
PAY_TEST_API_KEY  ?= 1a2b3c...
```

`?=` keeps a value already exported in your shell winning over the file.

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
