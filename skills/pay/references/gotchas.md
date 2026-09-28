# Verified Payload gotchas

Every item here was verified against a live Payload 3.x instance. Each one is a case where
the obvious behaviour returns a **confidently wrong answer** rather than an error, so
nothing in this file can be inferred from the API by trying it once.

---

## 1. Drafts: absence of `--draft` is not a published filter

> A read without `--draft` does **not** filter out unpublished documents — documents that
> were never published are returned with `_status:"draft"`. To get only published content,
> always pass `--published-only` (`_status = published`). `--draft` additionally swaps in
> the newest draft for documents that **do** have a published version.

Verified: `GET /api/pages?limit=100&depth=0` with no `draft` parameter returned all 11
documents, **every one** `_status:"draft"`, byte-identical to the same call with
`draft=true`.

```bash
pay find pages                      # WRONG if you meant "live content"
pay find pages --published-only     # RIGHT
```

`pay` raises `mixed_status_results` when a drafts-enabled read comes back with unpublished
documents and no `_status` filter. `pay count` **ignores** `draft` entirely, so use
`--published-only` / `--draft-only` there as well.

`--draft` on a **write** skips required-field validation, which is how you create a stub.
Publishing later re-runs exactly the validation `--draft` skipped
(`PATCH {"_status":"published"}` → 400 on the missing field), so a draft is a deferral,
not an escape.

---

## 2. Update validates the whole document

> Payload validates the **whole document** on update: a `PATCH` of one field re-validates
> every field of the stored document, so the error can name fields you never sent.
> `error.fields[].sent` is `true` only for paths that were leaves of the body PayCLI
> actually sent; when every entry is `sent: false`, the stored document was already
> invalid and your change was rejected by pre-existing state, not by your input.

Verified: `PATCH /api/posts/1 {"heroImage":4}` → 400 naming `title`, `slug` and `content`,
none of which were sent.

Entries are sorted sent-first. When every entry is `sent: false`, do **not** invent values
for someone else's document: repair the listed fields deliberately in the same request, or
retry with `--draft` to store your change without validation.

---

## 3. Localisation silently answers in the wrong language

> `--locale de` on an untranslated field returns the **default locale's** text unless
> `fallback-locale=none`, which PayCLI sends by default and reports in `meta.locale`.

`config.localization.fallback` defaults to `true`, so an untranslated German field comes
back as English and is indistinguishable from a real translation — and writing it back
overwrites `de` with English. `pay` sends `fallback-locale=none` on every read of a
localised project and echoes the resolved values in `meta.locale = {requested, fallback}`,
so a `null` field is visibly "not translated" rather than invisibly "translated to
English".

An **unknown** locale code is coerced to the default locale with HTTP 200 and no warning,
so acceptance proves nothing. `pay` validates `--locale` only when it actually knows this
project's codes, and otherwise passes it through with a `locale_unverified` warning.

---

## 4. Payload silently discards what it does not recognise

An unknown field, or a block with an unknown `blockType`, is **thrown away with HTTP 201**
and a success message. A whole `layout` array can disappear while the response says
"Page successfully created."

`pay` diffs what you sent against what came back and splits the residue into two warnings:

| Warning | Meaning |
|---|---|
| `input_silently_dropped` | the path is **absent** from the echoed document — the real footgun |
| `value_normalized_by_server` | the path is present with a **different** value (a `slug` hook rewriting `My Page` → `my-page`, an email being lower-cased) |

Read `warnings[]` on every write. `--no-echo-check` disables the comparison.

Block `blockType` slugs are **not** a plain API read. GraphQL publishes a blocks field's
union as interfaceNames (`CallToActionBlock`) while REST only accepts the slug (`cta`),
and in production GraphQL introspection is usually disabled altogether (§13). `pay`
therefore learns block slugs from the documents themselves — the **block census** — and
merges declared facts when it has them, labelling every fact with its source
(`observed-census`, `graphql`, `project-source`, `configured`, `census-matched`):

```bash
pay blocks types pages                                        # slugs per field path, with counts
pay blocks schema cta --collection pages                      # what is inside one
pay describe pages --field layout --path .block_type_sources  # where each declared slug came from
```

A list learned from documents (`authoritative: false`) is "what exists today", not
"everything allowed": it never blocks a write. `pay blocks add` refuses a slug
(`block_type_unknown`, exit 10) only when the accepted list is authoritative. Otherwise
`pay blocks validate` and every write warn `unknown_block_type` (never seen anywhere — the
row would be dropped) or `block_type_not_observed_here`, and `unknown_field` for a key
never seen on that type (Payload drops unknown keys too). These warnings are real
signals, not noise: a block type deployed since the last census is re-checked
automatically, and `pay blocks learn <collection>` rebuilds the census on demand. With no
census cached a write says `blocks_unchecked` once instead of guessing.

---

## 4a. Editing blocks by hand gets the index arithmetic wrong

Payload has no per-row endpoint: the only way to move one block is to replace the whole
array. So everyone writes the same read-modify-write with jq, and it fails in the same
four ways. All four are verified.

| Wrong | Right |
|---|---|
| `jq` the array, remove the row, insert it at the anchor's old index | **the anchor shifts.** "Move row 0 after row 3" is an insert at index **2** of the remaining three rows, not at 4. `pay blocks mv id:… --after id:…` resolves the anchor before the removal and recomputes after it |
| Duplicate a row by copying the JSON | **the copy keeps its `id`, so Payload matches it to the original**, overwrites it, and the array *loses* a row. `pay blocks cp` strips every `id` at every depth |
| `pay get pages 12` then write the array back | above `--depth 0` a relationship comes back as a whole document and is written back as one. Read with `--depth 0`; `pay blocks` warns `populated_relationship` naming `layout[2].media` |
| `pay update pages 12 --data @-` with the document you read | that PATCHes back `_status`, `createdAt` and every unrelated field — a block reorder republishing the page. `pay apply` sends only the fields the pipeline recorded in `edits.fields` |

```bash
# wrong: three commands, a temp file, and the four failures above
pay get pages 12 --path .layout > /tmp/l.json
jq '...' /tmp/l.json > /tmp/l2.json
pay update pages 12 --set-json layout="$(cat /tmp/l2.json)" --yes

# right: one pipe, one write, nothing on the wire until `apply`
pay get pages 12 --depth 0 | pay blocks mv type:cta --after type:mediaBlock | pay apply --yes
```

Two more traps in the pipe itself:

* **Indexes go stale between stages.** Each stage renumbers the array, so an index read
  from an earlier `pay blocks ls` addresses a different row two stages later. `ls` prints
  an `id:` selector per row for exactly this reason.
* **`--draft` has to match on both ends.** A read *without* `--draft` returns the
  **published** document, so `pay get … | … | pay apply --draft` saves published content
  as a new draft and discards the real one. Use it on both ends, or on neither.
* **Nested rows need a real path.** `--field layout.blocks` is wrong (layout is an array);
  name the row: `--field 'layout[id:6ab8…].blocks'`, copied from
  `pay blocks ls --recursive` or `pay outline`.

Full walkthrough in `blocks.md` §5.

---

## 5. Bulk `DELETE` ignores `limit`

Verified: with 4 matching documents, `DELETE /api/crm-contacts?limit=1&where=…` deleted
**all 4**. (Bulk `PATCH` does honour `limit` — the two verbs disagree.)

`pay` never passes `--where` straight through to a bulk write. It counts, resolves the
exact ids client-side, and then addresses them explicitly in chunks — on `update` exactly
as on `delete`, so both verbs behave identically for the same flags. Your blast radius is
exactly what `--dry-run` printed.

* `--max-docs N` (default 100) is the **only** blast-radius cap. `--all` lifts it.
* `--limit` on a bulk write is an **error** (exit 5): it is page size, not a cap.
* `--unsafe-passthrough-where` restores the raw server semantics if you really want them.
* Documents created between the count and the write are not included; documents deleted in
  the interim come back as per-id failures, not as hard errors.

---

## 6. Bulk writes return 400 with committed successes

A bulk verb returns HTTP **400 whenever `errors[]` is non-empty, even when some documents
succeeded and committed** — the outer transaction commits after the per-document loop.
Never trust the status code; always parse the body.

That is exit **7** `partial_failure`, and it is the one failure mode where the default
agent heuristic ("non-zero exit → retry") causes real damage: re-running re-applies the
committed half. Retry only `next.args.ids`. Use `--per-doc` to stop one bad document from
poisoning its batch-mates.

---

## 7. `limit=0` means unlimited

Verified: `?limit=0` returned every document with `"limit":0, "totalPages":1`. An agent
that means "no results, just the count" gets the entire collection instead. `pay` rejects
`--limit 0` with `invalid_args` (exit 5); use `pay count` for a count, or `--all` for
everything.

---

## 8. Bad input frequently returns 200

| Input | Server behaviour | What `pay` does |
|---|---|---|
| unknown `sort` field | **200, silently unsorted** | `invalid_sort_field` (exit 5) + the sortable list |
| unknown `select` key | **200, only `id` returned** | `unknown_field` (exit 5) + the field list |
| unknown `locale` | **200, default locale** | `invalid_option` (exit 5) when the codes are known |
| `where[x][equals]=null` in bracket notation | 0 results, silently wrong | `pay` sends `where` as JSON, where `null` works |
| unknown `where` path | 400 `QueryError` | rejected locally with `did_you_mean` |
| `contains=%` | **all rows** (`%` is a SQL wildcard) | escaped for you |
| `not_like=x` | **all rows** (no auto-wrapping) | wrapped in `%…%` for you |
| invalid enum value in `where` | **500** on Postgres, 0 docs on Mongo | `invalid_option` (exit 5) when the values are known |
| `all` / `near` / `within` / `intersects` | **500** on Postgres | rejected when the adapter is known to be Postgres |

When `pay` does **not** know the relevant fact (unknown id type, unknown locale list,
unknown adapter), it sends the request and attaches a `*_unknown` warning rather than
rejecting locally. A rejection built on a fact it never learned would itself be a
confidently wrong answer.

---

## 9. Authentication: HTTP status tells you nothing

* A **wrong API key returns HTTP 200** with `{"user": null}`. The only reliable check is
  `GET /api/{authCollection}/me` returning a non-null `user` — which is what
  `pay whoami` and `pay doctor` assert.
* Unauthenticated reads can succeed: public read access is common, so a 200 on
  `GET /api/pages` does not prove your credentials work.
* The auth header is `Authorization: {authCollectionSlug} API-Key {key}` — the
  **collection slug is part of the header** and is project-specific (`users`, `admins`,
  `api-keys`, …). A wrong slug silently degrades you to the anonymous view: 49 collections
  become 10, with no error anywhere.
* A 403 body is byte-identical whether you are unauthenticated or merely unpermitted, so
  `pay` uses its own verified identity to split exit 2 ("fix your key") from exit 8
  ("ask for permission").
* `GET /api/{auth}/me` echoes the API key **in plaintext**. `pay` redacts it everywhere:
  stdout, logs, the audit log, cache files and error bodies. `--no-redact` opts out, and
  the envelope tells you when redaction changed the bytes.

---

## 10. Trash, delete and restore

* `DELETE /{coll}/{id}` **hard-deletes** even on a trash-enabled collection. `pay delete`
  therefore issues the soft-delete `PATCH {"deletedAt": "<now>"}` by default and reports
  `"trashed": true`; `--permanent` is the real DELETE.
* Hard-deleting an **already-trashed** document requires `?trash=true`, otherwise it 404s.
  `pay` adds it for you.
* Trashed documents are hidden from `find` and `count` unless you pass `--trash`.
* The trash field is only *conventionally* named `deletedAt`; `pay` reads the real name
  from discovery rather than hardcoding it.
* On a collection **without** trash, delete is irreversible — `pay` warns on stderr and
  requires confirmation.
* `POST /{coll}/{id}/duplicate` with a **non-castable** id creates a document instead of
  404ing. Never hand a duplicate a hand-built id.

---

## 11. Shape surprises in responses

* `depth` is capped by project config, not by your request: depth 1, 2 and 5 returned
  byte-identical responses on a live collection. A bigger number is not more data.
* **Join fields are always present** as `{docs: [...], hasNextPage: bool}` even at depth 0
  and cannot be disabled over the query string. Drop them with `--select`.
* `select` as a JSON string is silently dropped; it must be bracket notation. `pay` emits
  the right style per parameter.
* `HEAD` is not routed (404). Health checks use `GET /api/access`.
* Relationship ids are type-sensitive on write: `{"heroImage": "4"}` is rejected while
  `{"heroImage": 4}` is accepted. `pay` coerces relationship ids to the collection's id
  type for you.
* Payload's own `payload-*` collections are real and reachable but are de-emphasised in
  listings; pass `--include-internal` when you actually want them.

---

## 12. Uploads

* The multipart part must be named exactly `file`; sibling fields travel in a `_payload`
  part holding a JSON string. A JSON body with `{"alt": "x"}` fails with the same
  "no files" error as a wrongly named part. `pay upload` handles both parts.
* `--file` against a collection that is not an upload collection produces a 500 from the
  server; `pay` rejects it locally with `not_upload_collection` (exit 5).
* A 400 on an upload collection with no `errors[0].name` is a **missing file**, not a
  validation failure. `pay` classifies it structurally, so the diagnosis survives a
  non-English project.

---

## 13. GraphQL introspection is off in production

Payload 3 disables GraphQL introspection when `NODE_ENV=production`
(`graphQL.disableIntrospectionInProduction`, default `true`). GraphQL still answers
queries, so this is easy to miss. Discovery then falls back to REST samples:

* the field list comes from three sampled documents — a field that was `null` there has
  no type;
* required flags are unknown;
* block slugs and block schemas are not published at all.

`pay doctor` reports it (check `graphql`, value `introspection_disabled`) and names the
config switch. Until the app enables it, use the block census
(`pay blocks types | schema | example | new | validate`) instead of `pay describe` for
anything about blocks, and never infer "optional" from `required: null`.

---

## 14. A write can publish or unpublish without saying so

Payload merges an update into the **newest version** and, without `?draft=true`, stores
the result as the live row, including that version's `_status`. Verified consequences:

| Wrong | What happens | Right |
|---|---|---|
| `pay update pages 16 --set slug=x` on a published page with a pending draft | the draft's content goes live AND `_status` becomes `draft` — the page drops offline | PayCLI refuses (`invalid_args`, exit 5): say `--draft`, `--publish` or `--unpublish` |
| `pay update pages 16 --data-file export.json --draft` where the export has `_status: "published"` | Payload publishes despite `?draft=true` | PayCLI removes `_status` with `--draft` and warns `body_publishes`; without a flag it warns that the write publishes |
| `pay get pages 16` (no `--draft`) piped to `pay apply --draft` | published content is saved as the new draft; the real draft is lost | `--draft` on both ends |
| `pay update pages --where … --set …` over pages with pending drafts | the live rows are rewritten; pending drafts are superseded | `--draft`, or write them one by one (the dry run warns `pending_draft_superseded`) |

`pay diff pages 16 --draft-vs-published` shows what a pending draft holds; every dry run
shows `data.diff` (and `data.diff.published` when the live row would change too).

---

## 15. Nested block restructures leave `{}` rows (Postgres)

Changing the `blockType` of a NESTED row (bento → grid inside a group), or removing a
nested container, leaves the old nested rows in the database. They read back as `{}`
entries in `pay get` (the stored row) — not in `pay get --draft`, not in the write's echo.
Top-level swaps in `layout` are clean.

The old workaround (permanently delete the page and recreate it) changes the id and
breaks every relationship and link to it. Instead:

```bash
pay update pages 16 --data-file layout.json --reset-nested --dry-run   # orphan_risk + the plan
pay update pages 16 --reset-nested                                     # repair a page that has {} rows
pay publish pages 16 --reset-nested                                    # publish a restructured draft
```

Signals: `orphan_risk` (dry run), `orphan_rows` (after a write), `empty_rows` (on read),
`empty_row` in `pay outline` and `pay blocks validate`. Details: `blocks.md` §8.

---

## 16. Rich text is JSON, and a string is silently accepted

A `richText` field holds a Lexical editor state. Payload does **not** reject a string in
it: it stores the string, and the admin editor can no longer open the field. Two ways
agents get there:

* writing Markdown or text straight into the field — use `--set-md content=@body.md`,
  which converts to the editor state (`richtext.md`);
* writing back a document read with `--richtext md|text` — PayCLI refuses it
  (`richtext_rendered`, exit 5).

The reverse also bites: `--set-md` on a plain **text** field would store an editor state
as JSON text; PayCLI refuses when it can tell and warns `stored_as_string` when the echo
shows it happened.

---

## 17. The rule that outranks the rest

> **Never branch on `error.message` — it is translated.** Payload runs its error strings
> through i18n, so the same failure reads differently on a German project, and a proxy
> can change the language by injecting `Accept-Language`. `error.code`, `error.exit` and
> `ok` are the stable signals.

The only two Payload strings that are hardcoded English are `Route not found "…"` and
`Cannot <METHOD> …`; everything else — including "The following fields are invalid",
"No files were uploaded" and "You are not allowed to perform this action" — goes through
`req.t` and changes with the project's i18n configuration.
