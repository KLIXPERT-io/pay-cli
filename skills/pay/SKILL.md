---
name: payload-cli
description: Read and write any Payload CMS 3.x project from the command line with the `pay` CLI - discover collections, query documents, create/update/delete, manage globals, versions and uploads. Use when the user asks to inspect or change Payload CMS content, or mentions payload.config.ts, a Payload collection, or a Payload admin panel.
---

# Payload CMS from the command line (`pay`)

**Run `pay explain` first.** Every Payload project has different collections, different
fields and different capabilities; nothing below can tell you what *this* project has.
`pay explain` is one cached, offline-after-first-run call that answers it. If a command
fails and you do not understand why, run `pay doctor`.

```bash
pay explain --slim          # connection + every collection slug + exit codes (~1 KB)
pay explain                 # + per-collection ops, features, key fields, gotchas
pay describe pages          # every field of one collection, with types and required flags
pay doctor                  # connectivity, auth, cache, GraphQL, audit log — when confused
```

Payload is a headless CMS. Content lives in **collections** (many documents: `pages`,
`posts`, `media`) and **globals** (exactly one document: `header`, `footer`). Documents
have an `id`, `createdAt`/`updatedAt`, and whatever fields the project defined.

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
   change `ok` and never change the exit code, but `input_silently_dropped` means Payload
   threw away part of your write and still returned success. Read them.

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
| `op_result` | the result of an operation with no document (`--dry-run`, cache ops) |
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

---

## 2. Exit codes

| Exit | Class | Retry? | What to do |
|---|---|---|---|
| 0 | success | — | — |
| 1 | internal / cache corrupt / update verification failed | no | report it; `pay cache clear --all` if `cache_corrupt` |
| 2 | auth — *your credentials are wrong or missing* | no | `pay auth login` / fix `--api-key` |
| 3 | throttled: `rate_limited`, `doc_locked`, `server_busy` | **yes**, after `Retry-After` | back off and retry |
| 4 | not found: `doc_not_found`, `route_not_found`, `version_not_found` | no | check the id and the slug |
| 5 | validation / bad input | no | fix the input; read `error.fields` |
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
> gives you verbatim.

Exit 2 and exit 8 are deliberately different: a 403 body is byte-identical whether you are
unauthenticated or merely unpermitted, so `pay` uses its own verified identity to tell
"fix your key" (2) from "ask for permission" (8).

---

## 3. Querying: the mini-DSL

```
pay find <collection> --where 'PATH OP VALUE' [--where ...] [--or 'PATH OP VALUE' ...]
```

`--where` terms are ANDed. Repeated `--or` terms form **one** OR group, which is ANDed
with the `--where` terms. A term is split into at most three tokens: path, operator,
and everything else as the value — so values with spaces need no quoting.

| Write | Means | Notes |
|---|---|---|
| `eq` `=` `is` | `equals` | typed scalar |
| `ne` `!=` `not` | `not_equals` | |
| `gt` `>` `gte` `>=` `lt` `<` `lte` `<=` | numeric/date comparison | |
| `contains` `~` | substring | `%` `_` `\` are escaped for you |
| `like` | word-AND match | adapter-specific |
| `nlike` `!~` | `not_like` | value is auto-wrapped in `%…%` |
| `in` / `nin` | `in` / `not_in` | comma-separated, or `json:[1,2]` |
| `exists` | field present | value optional, defaults `true` |
| `all` `near` `within` `intersects` | as in Payload | `all` fails on Postgres |

Values are typed for you: `null` → JSON null, `true`/`false` → boolean, a numeric literal
→ number, anything else → string. Force a string with quotes (`--where 'code eq "123"'`),
force raw JSON with `json:` (`--where 'tags in json:[1,2]'`).

Worked examples against a real project:

```bash
pay find pages --where '_status = published' --sort -publishedAt --limit 5
pay find posts --where 'title contains guide' --where 'publishedAt gt 2026-01-01'
pay find crm-contacts --where 'status eq active' --or 'lifecycleStage eq customer' \
                      --or 'lifecycleStage eq lead'
pay find crm-contacts --where 'company.name contains Acme' --depth 1   # dotted relation path
pay find crm-contacts --where 'email exists' --where 'emailOptOut ne true'
pay find posts --where 'categories in 3,4' --select title,slug,_status
pay find pages --id 11 --id 16                 # sugar for {"id":{"in":[11,16]}}
pay find posts --q "launch"                    # OR-contains across title-ish fields
pay find posts --published-only --since 30d    # resolved client-side to an absolute date
pay count crm-contacts --where 'status eq active'
```

Other read flags:

* `--sort FIELD` / `--sort -FIELD` — descending with the leading `-`. An unknown sort
  field is rejected locally, because the server **silently returns unsorted results**.
* `--select a,b,c` — only these fields. An unknown key is rejected locally, because the
  server **silently returns only `id`**.
* `--depth N` — default `0`: relationships come back as bare ids. `--depth 1` embeds the
  related document. Combine `--depth 1 --populate 'crm-companies:name'` to embed one
  relation while keeping only the fields you need.
* `--limit N` — page size, default 20. **`--limit 0` means "unlimited" to Payload and is
  rejected by `pay` (exit 5)**; use `--all` if you really want every page.
* `--all [--max 1000]` — paginate to the end, set `page.truncated` when the cap was hit.
* `--page N`, `--trash` (include soft-deleted), `--locale CODE`.
* `--where-json '{...}'` — a raw Payload `where` object, replacing the DSL entirely.
* `--output jsonl` — bare documents on stdout, envelope on stderr:
  `pay find posts --all --output jsonl > out.jsonl 2> summary.json`.
* `--output id` — one id per line, for shell pipelines.
* `--path EXPR` — narrow `.data` before printing. **Exactly three forms**, and nothing
  else: `.a.b` (field access, arbitrarily nested), `.a[0]` (index into an array; a
  negative index is an error) and `.a[]` (iterate a whole array). Each form composes:
  `.data` of a `doc_list` is itself an array, so `--path '[].title'` iterates it and
  `--path '[0].id'` takes the first id. There is **no `[*]`** — it fails with
  `invalid_path_expr` (exit 5). It is **not** jq; pipe the envelope to jq for anything
  more.

  ```bash
  pay find pages --limit 3 --path '[].title'          # data becomes an array of titles
  pay find pages --limit 3 --path '[0].id'            # data becomes the first id
  pay get pages 11 --path '.meta.title'               # nested field access
  pay collections --path '.collections[].slug'        # iterate a named array
  ```

---

## 4. The gotchas that produce confidently wrong answers

These are verified against a live Payload 3.x instance. Each one is a case where the
obvious behaviour is wrong.

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
`warnings[]` on every write.

**Blocks.** A blocks field asks three separate questions and `pay` answers them separately:

```bash
pay describe pages --field layout --path .block_types   # WHICH blocks may go in it
pay describe pages --path .block_docs                   # what each one is FOR
pay describe pages --block cta                          # WHAT IS INSIDE one
```

`.block_types` are the `blockType` slugs the REST API accepts (`cta`, `mediaBlock`), never
the GraphQL interfaceNames (`CallToActionBlock`) — sending an interfaceName is the silent
drop above. `--block <slug>` prints that block's own fields: types, relationship targets,
enum options and required-ness with provenance. `id`, `blockName` and `blockType` are
Payload plumbing (`plumbing: true`), not content — `blockType` is mandatory on every block
you write, `blockName` is an optional admin label, `id` is server-generated.

`required` inside a block is `null` more often than elsewhere: Payload publishes **no**
input type for a block type, so the only proofs are a GraphQL `NON_NULL` and the block's
own `config.ts`. `.block.required_unknown` names every field nobody could answer for and
`.block.reason` says why — treat those as "unknown", never as "not required".

**What a block is FOR.** Choosing between `cta`, `content` and `mediaBlock` from slugs alone
is guesswork, so `.block_docs[SLUG]` is printed beside every slug list:

```bash
pay describe pages --path .block_docs                   # label + description for each
pay describe pages --block cta --path .docs             # just this one
pay describe pages --block mediaBlock --path .documented_fields
```

```json
{"cta": {"label": "Call to Action", "label_plural": "Calls to Action",
         "description": "A prompt with rich text and one or more buttons, used to push the reader to a next step.",
         "description_key": "custom.description", "description_source": "project-source",
         "fields_count": 9, "docs_reason": ""}}
```

These are **the project's own words**, read from the block's `config.ts` on disk —
`labels: { singular, plural }` plus a description under `custom.description`, `custom.docs`
or `custom.summary`. Payload publishes neither over REST or GraphQL and defines **no**
description field for a block at all, which is why the text lives in `custom` and why
`description_key` always names the key that answered.

**A project gets this only if its authors wrote it.** `null` means nobody did — never that
`pay` failed — and `docs_reason` says which case it is. A plugin's blocks (the form
builder's `textarea`, `email`, `select`, …) live in `node_modules`, which is never scanned,
so their label and description are always `null` with `labels_source: "unknown"`. That is
expected; read `--block <slug>` for their fields instead.

Inside a block, each field carries its own `description` from Payload's `admin.description`
— the per-field instruction for filling that field in ("Pass a media document id"), with
`description_source` and `description_key`. `null` with `"unknown"` means undocumented.

**Field instructions on ordinary collections** use the same `admin: { description }` and are
published as `.field_docs[PATH]` (a separate map, so the field entries stay at 26 keys):

```bash
pay describe pages --path .field_docs          # every documented field of this entity
pay describe pages --path .documented_paths    # just the paths
pay describe pages --field title --path .field_doc
```

`field_docs_source` is `"project-source"` when the entity's config was read (an empty
`field_docs` then means the authors documented nothing) and `"unknown"` when there was no
config to read — the normal case for a plugin-provided collection such as `forms`. Only an
entity's **top-level** `fields:` array is read; a field nested in a group, array or tab is
reported as undocumented rather than guessed at.

**Bulk delete ignores `limit`.** `DELETE /api/{coll}?where=…&limit=1` deletes **every**
match. `pay` therefore never passes `--where` straight through: it counts, resolves the
exact ids client-side, and deletes them by id in chunks — on `update` exactly as on
`delete`. Your blast radius is exactly what `--dry-run` printed. `--max-docs N`
(default 100) is the only cap; `--limit` is page size and is an **error** on a bulk write.

**Editing blocks: read at `--depth 0`, and keep `--draft` on both ends.** The edit pipeline
(`pay get … | pay blocks … | pay apply`) writes back the field it edited. Two things about
the **read** decide whether that is correct:

> Above `--depth 0`, a relationship comes back as a whole document rather than an id, and
> writing it back stores the expansion. `pay blocks` raises `populated_relationship` naming
> `field[i].key` when it sees one — re-read with `--depth 0` rather than applying.
>
> A read **without** `--draft` returns the *published* document. Applying that with
> `--draft` saves published content as a new draft and throws the real draft away. Use
> `--draft` on both ends of the pipe, or on neither.

Never hand-roll the read-modify-write: `pay apply` sends only the fields the pipeline
recorded in `edits.fields`, so a block reorder writes `layout` and nothing else. PATCHing
the whole document back also rewrites `_status` and `createdAt`.

More in `references/gotchas.md`.

---

## 5. Writing safely

```bash
pay create pages --set title='Launch' --set slug=launch --draft
pay update pages 16 --set title='Launch day'
pay update posts 1 --set-json meta='{"title":"SEO title"}'
pay delete pages 16                      # soft delete IF this collection has trash
pay delete pages 16 --permanent --yes    # the real DELETE
pay restore pages 16                     # un-trash; exit 10 if the collection has no trash
pay publish pages 16                     # _status = published
pay unpublish pages 16 --yes             # _status = draft; destructive, so --yes
```

**Trash is per-collection, not global.** `pay collections` lists `trash` under `features`;
without it every `delete` is permanent (PayCLI says so on stderr before acting) and
`pay restore` fails with `feature_unavailable` (exit 10). Check before you rely on undo.

* `--data JSON`, `--data-file F`, `--set k=v`, `--set-json k=JSON` are **combinable** and
  deep-merged in that order, later winning. Arrays are replaced, never merged.
* **`--dry-run` works on every write** and exits 0 with
  `{"would_affect": N, "sample_ids": [...], "request": {...}}`. Use it before any bulk
  verb, always.
* Risk levels: single-document writes just run; destructive single-document writes
  (`--permanent`, `versions restore`, `globals update`, `unpublish`) and **all** bulk
  writes need `--yes` when stdin is not a terminal, else exit **11**.
* `--max-docs N` (default 100) caps a bulk write; `--all` lifts the cap. Over the cap you
  get `bulk_limit_exceeded` (exit 5) rather than a surprise.
* Every write is recorded in the audit log before and after the call (`pay audit tail`).
  `--no-audit` opts out deliberately.
* `--draft` on a write skips required-field validation — that is how you create a stub.
  Publishing later re-runs **exactly** the validation `--draft` skipped, so a draft is a
  deferral, not an escape.

---

## 6. Twelve recipes

```bash
# 1. What is here at all?
pay explain --slim
pay collections --kind content --writable

# 2. What fields does this collection have, and which are required?
pay describe pages --required-only

# 3. Page through a big collection without looping by hand
pay find crm-contacts --limit 50 --page 1        # then run next.cmd until page.truncated is false

# 4. Stream everything into a file, summary separately
pay find crm-contacts --all --max 5000 --output jsonl > contacts.jsonl 2> summary.json

# 5. Only published content, newest first
pay find posts --published-only --sort -publishedAt --limit 10

# 6. Create a stub you can fill in later, then publish it
pay create pages --set title='Pricing' --set slug=pricing --draft
pay publish pages 17          # re-runs the validation --draft skipped

# 7. Preview a bulk change before doing it
pay update crm-contacts --where 'lifecycleStage eq lead' --set status=active --dry-run
pay update crm-contacts --where 'lifecycleStage eq lead' --set status=active --max-docs 50 --yes

# 8. Soft-delete, inspect, restore
pay delete crm-contacts 224                 # PATCH deletedAt, reports "trashed": true
pay find crm-contacts --trash --where 'id eq 224'
pay restore crm-contacts 224

# 9. Build a blocks field: the slugs, what each is for, then what goes inside one
pay describe pages --field layout --path .block_types
pay describe pages --path .block_docs                    # label + description per slug
pay describe pages --block cta --path '.block.fields[].path'
pay describe pages --block mediaBlock --path .required_fields[]
# → a full, runnable create is in references/recipes.md §13

# 10. EDIT an existing document in a pipe — never read-modify-write by hand
pay get pages 12 --depth 0 | pay blocks ls               # rows + a selector for each
pay get pages 12 --depth 0 | pay blocks mv type:cta --after type:mediaBlock | pay apply --yes
pay get pages 12 --depth 0 | pay blocks rm id:67f3a1 | pay apply --yes
# stages compose; one write at the end
pay get pages 12 --depth 0 \
  | pay blocks rm type:content \
  | pay blocks add mediaBlock --first \
  | pay apply --dry-run
# it is NOT blocks-only: any array of objects, incl. globals and dotted paths.
# a plain `array` field always needs --field, and has no type:/name: selectors.
pay globals get header --depth 0 | pay blocks mv id:n3 --first --field navItems | pay apply --yes
# feeding an envelope to the ordinary write verbs also works (it unwraps .data),
# but it sends the WHOLE document — prefer `pay apply`.
pay get pages 12 --depth 0 | pay update pages 12 --data @- --dry-run

# 11. Uploads and downloads
pay upload media ./hero.png --alt 'Hero image'
pay download media 4 -o ./hero.png

# 12. Globals and version history
pay globals get header --depth 1
pay globals update header --set-json navItems='[]' --yes
pay versions list pages --id 11 --limit 5
pay versions restore pages <versionId> --dry-run
```

Two more that pay for themselves:

```bash
pay raw GET pages --query limit=1 --query depth=0      # escape hatch, still enveloped
pay audit tail -n 20 --action delete                   # what did I already change?
```

---

## 7. The closing rule

Branch on `.ok` and on the exit code. Read `error.code` when you need detail, and
`error.hint` for the next command.

> **Never branch on `error.message` — it is translated.** Payload runs its error strings
> through i18n, so the same failure reads differently on a German project, and a proxy
> can change the language by injecting `Accept-Language`. `error.code`, `error.exit` and
> `ok` are the stable signals.

Further reading, installed alongside this file:

* `references/query-syntax.md` — the full filter DSL, encoding, sorting, selection.
* `references/errors.md` — every code, the six server body shapes, the exit-code table.
* `references/recipes.md` — pagination, safe bulk writes, uploads, versions, drafts.
* `references/gotchas.md` — every verified footgun, with the wrong and right command.
* `references/PROJECT.md` — this project's real collections (only if it was generated
  with `pay skills install --with-project-context`; it is a snapshot and may be stale).
