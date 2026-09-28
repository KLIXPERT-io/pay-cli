---
name: payload-cli
description: Read and write any Payload CMS 3.x project from the command line with the `pay` CLI - discover collections, query documents, create/update/delete, manage globals, versions and uploads, edit block layouts, write rich text as Markdown, run idempotent content migrations and preview drafts. Use when the user asks to inspect or change Payload CMS content, or mentions payload.config.ts, a Payload collection, or a Payload admin panel.
---

# Payload CMS from the command line (`pay`)

**Run `pay explain` first.** Every Payload project has different collections, fields and
capabilities; nothing below can tell you what *this* project has. `pay explain` is one
cached, offline-after-first-run call that answers it. If a command fails and you do not
understand why, run `pay doctor`.

```bash
pay explain --slim          # connection + every collection slug + exit codes (~1 KB)
pay explain                 # + per-collection ops, features, key fields, gotchas
pay describe pages          # every field of one collection, with types and required flags
pay blocks types pages      # which block types this site really uses, where, how often
pay outline pages --slug home --draft --output table   # one page's block tree, readable
pay doctor                  # connectivity, auth, GraphQL, census, backups, PATH — when confused
```

Payload is a headless CMS. Content lives in **collections** (many documents: `pages`,
`posts`, `media`) and **globals** (exactly one document: `header`, `footer`). Documents
have an `id`, `createdAt`/`updatedAt`, and whatever fields the project defined. Pages are
usually built from **blocks** (rows in a `layout` array, each with a `blockType`, often
nested), and long text is **rich text** stored as Lexical JSON.

If `references/PROJECT.md` exists, read it: it is this project's snapshot (collections,
globals, block catalog, introspection, backup and preview state).

| Task | Section | Detail |
|---|---|---|
| read / filter documents | §3, §4 | `references/query-syntax.md` |
| change a page's blocks | §7 | `references/blocks.md` |
| write or read rich text as Markdown | §8 | `references/richtext.md` |
| prove a write is right, back up, undo, drafts vs live | §6 | `references/safe-writes.md` |
| migrate content idempotently (upsert, `$ref`, plan/sync) | §9 | `references/migration.md` |
| look at the result in a browser, drafts included | §10 | `references/migration.md` §6 |
| an error or warning code you do not know | — | `references/errors.md` |

---

## 1. The envelope

Every command prints **one JSON object on stdout**, including failures. The shape never
changes between commands.

```json
{
  "ok": true,
  "v": 1,
  "command": "find",
  "data_kind": "doc_list",
  "target": {"kind": "collection", "slug": "pages", "singular": "Page", "id_type": "number"},
  "data": [{"id": 11, "title": "Home", "slug": "home", "_status": "published"}],
  "page": {"limit": 3, "page": 1, "total_pages": 4, "total_docs": 11, "returned": 3,
           "has_next_page": true, "has_prev_page": false, "next_page": 2, "prev_page": null,
           "truncated": true},
  "next": {"reason": "more_pages",
           "cmd": "pay find pages --limit 3 --page 2 --profile dev",
           "args": {"page": 2}},
  "meta": {"request_id": "01K5Q7TZ4V3B8C", "duration_ms": 41, "http_requests": 1,
           "profile": "dev", "auth_mode": "api-key", "dry_run": false},
  "warnings": []
}
```

Read it in this order:

1. **`ok`** — the one field to branch on. `true` or `false`, always present. Never
   presence-check `error`.
2. **`data_kind`** — tells you the shape of `data` so you never have to guess it.
3. **`page.truncated`** — one boolean, no arithmetic. `true` means there is more data.
4. **`next.cmd`** — a literally runnable command string with the current flags and
   profile already baked in. Copy it; do not reconstruct it.
5. **`warnings[]`** — always an array of `{code, message, paths?, hint}`. Warnings never
   change `ok` and never change the exit code, but some mean Payload threw away part of
   your write (`input_silently_dropped`) or that a write publishes (`body_publishes`).
   Read them; their `hint` is usually the next command.

| `data_kind` | `data` is |
|---|---|
| `doc` | one document object |
| `doc_list` | a bare array of documents (`page` is present) |
| `count` | an integer |
| `global` | one global object |
| `version` / `version_list` | one version object / an array of them |
| `bulk_result` | `{succeeded: [...], failed: [...], not_attempted: [...]}` |
| `capabilities` | what `pay explain` returns |
| `schema` | what `pay describe` returns |
| `command_spec` | machine-readable help |
| `op_result` | an operation's report: `--dry-run`, `pay blocks types`, `pay outline`, `pay diff`, `pay plan`, `pay url`, … |
| `raw` | an unmodified server body (`pay raw`) |
| `error` | absent; read `error` instead |

**Errors are printed on STDOUT, not stderr.** stdout always holds the envelope; stderr
holds only a one-line human summary (`pay: validation_failed (exit 5): ... — hint: ...`).
Capture stdout for both success and failure. `--errors-to stderr` flips this if you need
the Unix convention.

```json
{"ok": false, "v": 1, "command": "create", "data_kind": "error",
 "error": {"code": "validation_failed", "exit": 5,
           "message": "3 fields are invalid on collection \"pages\".",
           "hint": "Fix every path in error.fields and retry...",
           "retriable": false, "confidence": "certain",
           "fields": [{"path": "title", "label": "Title",
                       "message": "This field is required.", "sent": true}],
           "did_you_mean": [], "docs": "pay explain --section exit_codes",
           "http": {"status": 400, "method": "POST", "url": "...", "attempts": 1}},
 "meta": {...}, "warnings": []}
```

`error.code` is a stable machine string. `error.hint` is written for you and usually
contains the exact next command. `error.did_you_mean` carries spelling suggestions.

`--path EXPR` narrows `.data` before printing, in exactly three forms: `.a.b`, `.a[0]`
and `.a[]` (`--path '[].title'`, `--path '.rows[].field'`). There is no `[*]` and no
filter: it is **not** jq. Pipe the envelope to jq for anything more.

---

## 2. Exit codes

| Exit | Class | Retry? | What to do |
|---|---|---|---|
| 0 | success | — | — |
| 1 | internal / cache corrupt / backup or audit could not be written | no | report it; `pay cache clear --all` if `cache_corrupt`; after `backup_failed` nothing was sent |
| 2 | auth — *your credentials are wrong or missing* | no | `pay auth login` / fix `--api-key` |
| 3 | throttled: `rate_limited`, `doc_locked`, `server_busy` | **yes**, after `Retry-After` | back off and retry |
| 4 | not found: `doc_not_found`, `selector_no_match`, `not_published`, `ref_unresolved` | no | check the id, slug or selector |
| 5 | validation / bad input | no | fix the input; read `error.fields` and `error.hint` |
| 6 | network / server: `timeout`, `dns_failure`, `server_error` (500) | **yes**, except 500 | retry, except `server_error` |
| **7** | **`partial_failure` — some writes COMMITTED, some failed** | **NEVER** | see below |
| 8 | access denied — identity is valid, permission is not | no | ask for permission; do not re-auth |
| 9 | config / profile | no | `pay config explain`, `pay auth list` |
| 10 | capability / discovery: unknown collection, feature off | no | `pay explain`; the feature is not enabled here |
| 11 | confirmation required | no | re-run with `--yes` (or `--dry-run` first) |

> **Exit 7 is the one failure you must never auto-retry.** Payload's bulk operations are
> not transactional over REST: the outer transaction commits after the per-document loop,
> so a bulk write returns HTTP 400 *with some documents already written*. Re-running the
> command re-applies the successful half. Retry only the failed ids, which `next.cmd`
> gives you verbatim. (The one exception is `pay sync`: it only writes what still
> differs, so re-running it after fixing the failed items is safe — §9.)

Exit 2 and exit 8 are deliberately different: a 403 body is byte-identical whether you are
unauthenticated or merely unpermitted, so `pay` uses its own verified identity to tell
"fix your key" (2) from "ask for permission" (8).

---

## 3. Querying: the mini-DSL

```
pay find <collection> --where 'PATH OP VALUE' [--where ...] [--or 'PATH OP VALUE' ...]
```

`--where` terms are ANDed; repeated `--or` terms form **one** OR group, ANDed with the
`--where` terms. Operators: `eq` `ne` `gt` `gte` `lt` `lte` `contains` `like` `nlike` `in`
`nin` `exists` (also `=` `!=` `>` `>=` `<` `<=` `~` `!~`). Values are typed for you
(`null`, `true`, numbers); force a string with quotes, raw JSON with `json:`.

```bash
pay find pages --where '_status = published' --sort -publishedAt --limit 5
pay find posts --where 'title contains guide' --where 'publishedAt gt 2026-01-01'
pay find crm-contacts --where 'status eq active' --or 'lifecycleStage eq customer'
pay find posts --where 'categories in 3,4' --select title,slug,_status
pay find pages --id 11 --id 16                 # sugar for {"id":{"in":[11,16]}}
pay find posts --published-only --since 30d    # resolved client-side to an absolute date
pay count crm-contacts --where 'status eq active'
```

* `--sort FIELD` / `--sort -FIELD`; `--select a,b,c`. Unknown sort or select keys are
  rejected locally, because the server **silently** returns unsorted results / only `id`.
* `--depth N` — default `0` (relationships are bare ids). Read at `--depth 0` whenever
  you intend to write the document back.
* `--limit N` — page size, default 20. **`--limit 0` means "unlimited" to Payload and is
  rejected by `pay` (exit 5)**; use `--all [--max 1000]` if you really want every page.
* `--output jsonl` (documents on stdout, envelope on stderr), `--output id` (one id per
  line), `--output table`.

Full grammar: `references/query-syntax.md`.

---

## 4. Reading one document

```bash
pay get pages 11                              # by id
pay get pages --slug about                    # by slug: 0 → doc_not_found (4), >1 → slug_ambiguous (5)
pay get pages --slug about --draft --depth 0  # the newest draft, ready to edit and write back
pay get pages 11 --published-only             # the LIVE state; not_published (4) if there is none
pay get posts 3 --richtext md --path .content # rich text rendered as Markdown (read-only!)
pay outline pages 11 --draft                  # every block row at every depth, with --field + selector
pay url pages 11                              # where it lives on the site
```

* `--published-only` on `get` is an assertion, not a filter: it returns the published
  state even when a newer draft exists, and fails with `not_published` (exit 4) for a
  document that was never published or was unpublished. It cannot be combined with
  `--draft`.
* `--richtext md|text` (also on `find`, `globals get`, `versions get`) replaces every
  Lexical value with a string, for reading. Every write refuses such a document
  (`richtext_rendered`, exit 5); re-read without `--richtext` to edit.
* `empty_rows` on a `get` means the stored document has `{}` rows (orphans of a nested
  block restructure) — see §7.

---

## 5. The gotchas that produce confidently wrong answers

These are verified against a live Payload 3.x instance. Each one is a case where the
obvious behaviour is wrong. More in `references/gotchas.md`.

**Drafts.**

> A read without `--draft` does **not** filter out unpublished documents — documents that
> were never published are returned with `_status:"draft"`. To get only published content,
> always pass `--published-only` (`_status = published`). `--draft` additionally swaps in
> the newest draft for documents that **do** have a published version.

`pay` warns with `mixed_status_results` when a drafts-enabled read returns unpublished
documents and you did not filter. `pay count` ignores `draft` entirely — use
`--published-only` / `--draft-only` there too.

**Validation on update.**

> Payload validates the **whole document** on update: a `PATCH` of one field re-validates
> every field of the stored document, so the error can name fields you never sent.
> `error.fields[].sent` is `true` only for paths that were leaves of the body PayCLI
> actually sent; when every entry is `sent: false`, the stored document was already
> invalid and your change was rejected by pre-existing state, not by your input.

Do not invent values for fields you did not send. Either repair them deliberately in the
same request, or retry with `--draft` to store the change without validation.

**Localisation.**

> `--locale de` on an untranslated field returns the **default locale's** text unless
> `fallback-locale=none`, which PayCLI sends by default and reports in `meta.locale`.

So a German-looking read that comes back in English is a *missing translation*, not a
bug — and writing it back would overwrite `de` with English.

**Silent drops.** Payload discards unknown fields and unknown `blockType` values without
an error and still returns 201. `pay` compares what you sent against what came back and
raises `input_silently_dropped` (absent from the response) or `value_normalized_by_server`
(present but changed, e.g. a `slug` hook rewriting `My Page` → `my-page`). Check
`warnings[]` on every write, and run `pay blocks validate` before writing block rows.

**A body with `_status: "published"` publishes.** Every export of a published page
carries it, and Payload publishes such a body even when the write says `--draft`. PayCLI
removes `_status` when you pass `--draft` and warns `body_publishes`; without a flag the
write publishes and warns loudly. Say `--draft` or `--publish` explicitly.

**Bulk delete ignores `limit`.** `DELETE /api/{coll}?where=…&limit=1` deletes **every**
match. `pay` therefore never passes `--where` straight through: it counts, resolves the
exact ids client-side, and deletes them by id in chunks — on `update` exactly as on
`delete`. Your blast radius is exactly what `--dry-run` printed. `--max-docs N`
(default 100) is the only cap; `--limit` is page size and is an **error** on a bulk write.

**GraphQL introspection is often off in production.** Then `pay describe` knows only what
three sampled documents showed: no block slugs, no block schemas, unknown required flags.
`pay doctor` says so (check `graphql`). Use the block census instead:
`pay blocks types | schema | example | new | validate` learn from real documents and label
every fact `observed-census` (§7). Do not read the app's source code to learn block shapes.

---

## 6. Writing safely

```bash
pay create pages --set title='Launch' --set slug=launch --draft
pay update pages 16 --set title='Launch day' --dry-run   # data.diff: exactly what changes
pay update pages 16 --set title='Launch day' --draft     # edit the draft; live page untouched
pay update pages 16 --set title='Launch day' --publish   # change and publish in one write
pay publish pages 16                     # _status = published (pre-validates required fields)
pay publish pages 38 39 40               # several ids: one PATCH each, bulk_result, exit 7 if partial
pay unpublish pages 16 --yes             # _status = draft; destructive, so --yes
pay delete pages 16                      # soft delete IF this collection has trash
pay delete pages 16 --permanent --yes    # the real DELETE
pay restore pages 16                     # un-trash; exit 10 if the collection has no trash
```

* **Body flags** `--data JSON`, `--data-file F`, `--set k=v`, `--set-json k=JSON`,
  `--set-md k=@file.md` are combinable and applied in that order, later winning. Arrays
  are replaced, never merged. Dotted keys reach groups (`--set meta.title=…`).
* **`--dry-run` works on every write** and exits 0. For a write to existing documents it
  includes `data.diff` = `{base, identical, summary:{total, fields, …}, changes:[{op, path,
  from, to}]}`: read `summary.fields` for the top-level fields touched and
  `changes[].path` to prove you only touched one block. `pay diff` does the same without a
  write verb (`references/safe-writes.md`).
* **Published page with a pending draft.** A write without `--draft`/`--publish`/
  `--unpublish` to such a document is **refused** (`invalid_args`, exit 5): Payload would
  merge it into the draft, publish the draft's changes and take the page offline. Pick one:
  `--draft` (edit the draft), `--publish` (publish draft + change; warns
  `publishes_pending_draft`, and the dry run shows the live change in
  `data.diff.published`), or `--unpublish`. `pay diff pages 16 --draft-vs-published`
  shows what the draft holds. A bulk `update --where` without `--draft` supersedes pending
  drafts (its dry run warns `pending_draft_superseded`). `--draft --unpublish` is refused:
  a draft save never takes the live page offline. `upsert` / `sync` are not refused:
  their default status keep saves INTO the pending draft and warns `saved_as_draft` — the
  live page did not change; `--publish` / `"status": "published"` takes it live.
* **`--draft` on a create/update skips required-field validation** — that is how you make
  a stub. Publishing later re-runs **exactly** the validation `--draft` skipped.
* **Backups.** With `PAY_BACKUP_DIR` (or `backup_dir = ".pay-backups"` in `./pay.toml`)
  set, every write that changes or removes an existing document first saves it as JSON
  and lists the file in `meta.backups`; `pay backups restore <file> --dry-run` shows the
  undo. A backup that cannot be written aborts the write (`backup_failed`, exit 1, nothing
  sent); `--no-backup` skips it deliberately.
* **Risk levels:** single-document writes just run; destructive ones (`--permanent`,
  `versions restore`, `globals update`, `unpublish`, `backups restore`) and **all** bulk
  writes need `--yes` when stdin is not a terminal, else exit **11**. `--max-docs N`
  (default 100) caps a bulk write; over it you get `bulk_limit_exceeded` (exit 5).
* **Trash is per-collection.** `pay collections` lists `trash` under `features`; without
  it every `delete` is permanent and `pay restore` fails with `feature_unavailable` (10).
* **Audit.** Every write is recorded before and after the call (`--no-audit` opts out):
  `pay audit tail --since 2h --collection pages --action update --phase post`.

---

## 7. Working with blocks

Detail, every path form and every warning: `references/blocks.md`.

**Learn what exists** from real documents (the *block census*: cached 24 h; the first run
costs a few requests, shown in `data.census.requests`):

```bash
pay blocks types pages                           # per field path: block types + counts
pay blocks types pages --field 'layout[].blocks' # what sits inside group-like rows
pay blocks schema hero --collection pages        # fields: kind, presence %, observed values, required
pay blocks example grid --collection pages --n 2 # real rows, ids stripped
pay blocks new text --collection pages --set heading=Ablauf --set-md content=@ablauf.md
pay blocks learn pages                           # rebuild after a deploy; reports "grid gained splitDesktop"
```

`authoritative: false` means "what documents contain today", not "everything the field
allows". Pass `--collection`: a slug like `text` can mean different blocks in `pages` and
`forms`.

**Edit in a pipe, never read-modify-write by hand.** Each `pay blocks` edit stage reads one
document on stdin and writes it to stdout; nothing touches the network until `pay apply`,
which PATCHes only the top-level field the stages edited (`edits.fields`).

```bash
pay get pages 47 --draft --depth 0 | pay blocks ls --recursive   # every row: path, field, selector
pay get pages 47 --draft --depth 0 \
  | pay blocks mv type:cta --after type:hero \
  | pay blocks add text --template --set heading=Neu --after first --field 'layout[type:group].blocks' \
  | pay apply --draft --dry-run        # check data.diff; then the same pipe with --yes
```

* **Selectors** (verb argument, `--before`, `--after`): `3`, `-1`, `first`, `last`,
  `id:6ab8…` (stable across stages — prefer it), `name:Hero`, `type:cta` (every cta),
  `type:cta[1]`.
* **`--field` reaches any depth**: `layout[2].blocks`, `layout.2.blocks`,
  `layout[id:6ab8…].blocks`, `layout[type:group[1]].blocks`, deeper by repetition. Copy
  the exact value from `pay blocks ls --recursive` or `pay outline` (`field` + `selector`).
  `layout.blocks` is wrong: name the row.
* **`add <type> --template`** starts from a row shaped like the real ones (like
  `pay blocks new`); `add -` adds a row to a plain array (buttons, links).
* **`--draft` on both ends** of the pipe, or on neither; read at **`--depth 0`**.
* **Validate before writing:** end a pipe with `| pay blocks validate` instead of
  `| pay apply` (it reports, it does not pass the document on). Exit 5 on real errors
  (`{}` rows, missing `blockType`, a type outside a declared list); warnings for what was
  learned from documents (`unknown_block_type`, `unknown_field` — Payload drops those
  silently — `likely_required`). Writes run the same checks against a cached census;
  `blocks_unchecked` means none is cached: run `pay blocks learn <collection>` once.

**Orphan rows.** On Postgres, changing a **nested** block's type (bento → grid inside a
group) leaves the old rows behind; they read back as `{}` (`empty_rows` on `get`,
`empty_row` in `pay outline`). A dry run predicts them (`orphan_risk`), a write detects
them (`orphan_rows`). Fix without changing the id:

```bash
pay update pages 16 --data-file layout.json --reset-nested --dry-run   # the restructure, cleanly
pay update pages 16 --reset-nested            # repair a document that already has {} rows
pay publish pages 16 --reset-nested           # publish a restructured draft without orphans
```

Never delete and recreate a page to get rid of `{}` rows.

---

## 8. Rich text as Markdown

Never hand-build Lexical JSON or parse `children` with jq. Detail: `references/richtext.md`.

```bash
pay update posts 3 --set-md content=@body.md --draft --dry-run   # Markdown file → rich text
pay create posts --set title=Launch --set slug=launch --set-md content=@body.md --draft
pay get posts 3 --richtext md --path .content                    # read as Markdown (read-only)
pay lexical from-md body.md --output raw > body.lexical.json     # an editor state, for manifests
pay get pages 47 --draft --depth 0 | pay lexical text -o -       # every rich-text value as text
```

* Rich text **inside a block**: set it on the row in the pipe —
  `… | pay blocks set id:6aba… --set-md content=@card.md --field '…' | pay apply --draft`.
  `pay update pages 47 --set-md layout.2.content=…` is refused (it would replace `layout`).
* Dialect: `**b**` `*i*` `~~s~~` `` `c` `` `<u>u</u>`, headings, lists (nested, `- [ ]`),
  `> quote`, `***`, `[t](https://…)`, internal links `[t](pay:pages/47)`,
  `{target=_blank}`, uploads `![media:40]()`. Front matter is stripped
  (`frontmatter_stripped`).
* Keep every `<!-- lexical:… -->` comment that `--richtext md` / `to-md` produced: each one
  IS a node (a block, an upload, a table). Deleting the comment deletes the node.
* `lexical_link_url_relative` (`../brief.md`): rewrite to a site path or `pay:pages/ID`.
  `lexical_link_url_invalid`: a draft saves, publishing fails.
* `--set-md` on a plain text field is refused; `stored_as_string` after a write means the
  field was text, not rich text.

---

## 9. Idempotent content migration

Never write a "find by slug, then create or update" script or keep an ids.json. Detail
and a complete example: `references/migration.md`.

```bash
pay upsert pages --data-file about.json --dry-run      # meta.upsert.action: created|updated|unchanged
pay upsert crm-contacts --match email --data-file c.json
pay plan ./content                                     # what sync would do, per item, with diffs
pay sync ./content --dry-run
pay sync ./content --yes                               # exit 7 here IS safe to re-run
pay plan ./content --brief --path .summary             # unchanged == items → migration complete
```

* `pay upsert` matches on `--match` (default `slug`) **values taken from the body**:
  0 → create, 1 → update the fields that differ (plus the unchanged scalar fields the body
  declares, so hooks cannot overwrite them), >1 → `match_ambiguous` (5). No
  difference → `unchanged`, nothing written.
* **`$ref`** in any write body replaces a lookup: `{"$ref":"pages","slug":"about"}`
  becomes the id; add `"$as":"polymorphic"` for `{relationTo, value}`; `{"$ref":"@key"}`
  points at another item of the same plan. `ref_unresolved` (4) / `ref_ambiguous` (5).
  A `$ref` naming a collection its field's `relationTo` excludes is `bad_request_body`.
* A manifest item is `{"key", "collection"|"global", "match", "status", "data"}`; a saved
  `pay get … --depth 0 --draft` envelope is one too. `status` (`keep` | `draft` |
  `published`) is the ONLY place publication is decided in plan/sync/upsert
  (`_status` in data is removed: `body_status_ignored`).
* Rich text in a manifest must be an editor state: convert with `pay lexical from-md`.

---

## 10. Checking the result in a browser

```bash
pay url pages 47                        # {url, path, path_source, status, has_unpublished_changes}
pay url pages --slug dashboards --draft # where the draft WILL live
pay url pages 47 --preview              # draft-preview handshake; cookies saved 0600
```

`--preview` returns `data.preview.cookie_file` and ready-made commands in
`data.preview.load`. Run `data.preview.load.agent_browser` verbatim
(`agent-browser cookies set --curl <file> --domain <host> && agent-browser open <url>`;
`--curl` needs agent-browser ≥ 0.34 — check `agent-browser --version`, else use
`data.preview.load.curl`).
The cookie puts the whole site in draft mode, so you can check drafts **without
publishing**. It needs a preview secret (`PAY_PREVIEW_SECRET`, or
`pay config set profiles.<profile>.preview_secret_env PREVIEW_SECRET`); without one you get
`preview_secret_missing` (exit 9). `path_source: slug` outside `pages` is a guess
(`url_path_guessed`): teach the route once with
`pay config set profiles.<profile>.url_paths.posts '/posts/{slug}'`.

---

## 11. More recipes

```bash
pay collections --kind content --writable
pay describe pages --required-only
pay find crm-contacts --all --max 5000 --output jsonl > contacts.jsonl 2> summary.json
pay update crm-contacts --where 'lifecycleStage eq lead' --set status=active --dry-run
pay update crm-contacts --where 'lifecycleStage eq lead' --set status=active --max-docs 50 --yes
pay upload media ./hero.png --alt 'Hero image'
pay download media 4 -o ./hero.png
pay globals get header --depth 0
pay globals update footer --data-file footer.json --dry-run
pay versions list pages --id 11 --limit 5
pay versions diff pages 687 699 --output table
pay backups list --collection pages --id 16
pay audit tail -n 20 --since 1d --collection pages
pay raw GET pages --query limit=1 --query depth=0      # escape hatch, still enveloped
pay doctor --path .binary                              # is `pay` on this shell's PATH?
```

`pay doctor` warns `binary_on_path` when this shell's `$PATH` does not reach the binary:
agent shells are non-interactive and often skip `~/.bashrc`. Until it is fixed, call the
binary by its absolute path (the hint prints the exact `ln -sf` fix). More recipes:
`references/recipes.md`.

---

## 12. The closing rule

Branch on `.ok` and on the exit code. Read `error.code` when you need detail, and
`error.hint` for the next command.

> **Never branch on `error.message` — it is translated.** Payload runs its error strings
> through i18n, so the same failure reads differently on a German project, and a proxy
> can change the language by injecting `Accept-Language`. `error.code`, `error.exit` and
> `ok` are the stable signals.

Further reading, installed alongside this file:

* `references/blocks.md` — block census, nested `--field` paths, the edit pipe, validate, orphans.
* `references/richtext.md` — `pay lexical`, `--set-md`, `--richtext`, the Markdown dialect.
* `references/safe-writes.md` — dry-run diffs, `pay diff`, drafts vs live, backups and restore.
* `references/migration.md` — upsert, `$ref`, plan/sync manifests, URLs and draft preview.
* `references/query-syntax.md` — the full filter DSL, encoding, sorting, selection.
* `references/errors.md` — every error and warning code, the exit-code table.
* `references/recipes.md` — pagination, bulk writes, uploads, versions, globals, describe.
* `references/gotchas.md` — every verified footgun, with the wrong and right command.
* `references/PROJECT.md` — this project's collections, globals and block catalog (only if
  generated with `pay skills install --with-project-context`; a snapshot, may be stale).
