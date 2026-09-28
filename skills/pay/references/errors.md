# `pay` errors: codes, exits, and what the server actually sends

## 1. Where errors go

The error envelope is printed on **stdout**, exactly like a successful one, with
`ok: false` and `data_kind: "error"`. stderr gets a single human line:

```
pay: validation_failed (exit 5): 3 fields are invalid on "pages" — hint: ...
```

Capture stdout unconditionally. `--errors-to stderr` (or `PAY_ERRORS_TO=stderr`) moves the
envelope to stderr if you prefer the Unix convention. In `--output jsonl` the envelope is
always on stderr so the data file stays clean.

## 2. The error object

```json
{
  "code": "validation_failed",
  "exit": 5,
  "message": "3 fields are invalid on collection \"pages\".",
  "hint": "Fix every path in error.fields and retry. `pay describe pages --required-only` ...",
  "retriable": false,
  "confidence": "certain",
  "fields": [
    {"path": "title",  "label": "Title",            "message": "This field is required.",           "sent": true},
    {"path": "layout", "label": "Content > Layout", "message": "This field requires at least 1 Row.", "sent": false}
  ],
  "failures": [],
  "likely_causes": [],
  "did_you_mean": [],
  "docs": "pay explain --section exit_codes",
  "http": {"status": 400, "method": "POST", "url": "http://localhost:3900/api/pages",
           "payload_error_name": "ValidationError", "attempts": 1},
  "raw": {"errors": [{"name": "ValidationError", "data": {"errors": [...]}}]}
}
```

| Key | Use it for |
|---|---|
| `code` | the stable machine string to branch on |
| `exit` | the same number the process returns |
| `hint` | usually a literally runnable next command |
| `retriable` | whether retrying can possibly help |
| `confidence` | `certain` (classified on `errors[].name`, JSON structure or HTTP status) or `probable` (a structural heuristic) |
| `fields[]` | per-field validation detail, sorted **sent-first** |
| `fields[].sent` | `true` only if you actually sent that path |
| `failures[]` | per-document detail on a bulk (`partial_failure`) result |
| `likely_causes[]` | diagnosis of an opaque 500 |
| `did_you_mean[]` | spelling suggestions for a slug, field or option |
| `http` | status, method, redacted URL, attempt count |
| `raw` | the server's own body, **after redaction** — `--no-raw` drops it, `--no-redact` keeps the true bytes |

When redaction changed `raw`, a `raw_redacted` warning lists the paths, so you always know
the bytes differ from the wire.

## 3. Exit codes

| Exit | Class | Auto-retry? | Codes |
|---|---|---|---|
| 0 | success | — | — |
| 1 | generic / internal | no | `internal`, `unknown`, `cache_corrupt`, `update_verification_failed`, `audit_write_failed`, `backup_failed` |
| 2 | auth — *your credentials* | no | `auth_missing`, `auth_invalid`, `auth_required`, `auth_locked`, `auth_unverified_email`, `auth_insecure_permissions`, `auth_helper_failed` |
| 3 | throttled / temporarily unavailable | **yes** | `rate_limited` (429), `doc_locked` (423), `server_busy` (503 with `Retry-After`) |
| 4 | not found | no | `doc_not_found`, `route_not_found`, `version_not_found`, `selector_no_match`, `not_published`, `ref_unresolved` |
| 5 | validation / bad input | no | `validation_failed`, `query_path_invalid`, `invalid_args`, `invalid_where_syntax`, `invalid_sort_field`, `unknown_field`, `invalid_option`, `invalid_id`, `bad_request_body`, `where_required`, `file_missing`, `not_upload_collection`, `unsupported_operator`, `bulk_limit_exceeded`, `request_too_large`, `format_unsupported`, `invalid_path_expr`, `selector_ambiguous`, `field_ambiguous`, `no_input`, `no_edits`, `not_rich_text`, `richtext_rendered`, `slug_ambiguous`, `match_ambiguous`, `ref_ambiguous`, `ref_cycle`, `manifest_invalid`, `backup_target_mismatch` |
| 6 | network / server | **yes**, except 500 | `network_unreachable`, `dns_failure`, `tls_error`, `timeout`, `server_error` (500, **not** retried), `server_unavailable` (502/504), `non_json_response` |
| **7** | **partial failure** | **never** | `partial_failure` |
| 8 | access denied — identity valid, permission not | no | `access_denied`, `admin_access_denied`, `preview_denied` |
| 9 | config / profile | no | `config_missing`, `config_secret_in_plaintext`, `profile_unknown`, `base_url_invalid`, `endpoint_not_payload`, `auth_collection_unknown`, `preview_secret_missing` |
| 10 | capability / discovery | no | `collection_unknown`, `global_unknown`, `feature_unavailable`, `operation_unsupported` (501), `discovery_failed`, `graphql_disabled`, `schema_stale`, `block_type_unknown`, `preview_failed` |
| 11 | confirmation required | no | `confirmation_required` |

`pay` never uses 125–128 or 130; those belong to the shell.

### 3a. The edit-pipeline codes

Six codes belong to `pay blocks` / `pay apply` (`blocks.md` §5). They are worth knowing
apart because all six are **local** — nothing was sent, so nothing changed.

| Code | Exit | Means | Do |
|---|---|---|---|
| `selector_no_match` | 4 | the selector addressed no row | the rows that exist are in `hint`, and `did_you_mean` offers the nearest real selectors |
| `selector_ambiguous` | 5 | it matched several rows and the verb acts on one | `did_you_mean` lists every match ready to paste; or `rm --all` to mean all of them |
| `field_ambiguous` | 5 | more than one blocks field, and no `--field` | pass `--field`; the candidates are in `did_you_mean` |
| `no_input` | 5 | stdin was empty | these commands edit a document read from stdin: `pay get … \| pay blocks …`; for `--set-md KEY=@-`, pipe the Markdown in (or `KEY=` to clear on purpose) |
| `no_edits` | 5 | `pay apply` got an envelope no transform touched | put a `pay blocks` stage in the pipe, or use `pay update <coll> <id> --data @-` to write the whole document |
| `block_type_unknown` | 10 | this field's **authoritative** accepted list (profile pin, project source or a complete GraphQL union) has no such `blockType` | it is exit 10 because it is a fact about the *project*; `did_you_mean` comes from the field's own slugs. A list learned only from documents (`accepts_authoritative: false`) never refuses; validation warns instead |

`selector_no_match` is a **4**, not a 5: the selector is well formed and the document
simply does not contain that row — the same distinction `doc_not_found` draws.

### 3b. Warnings the edit pipeline raises

Warnings never change `ok` or the exit code, and these are the ones that decide whether a
pipe's write is going to be correct.

| Warning | Means |
|---|---|
| `populated_relationship` | a row carries a relationship the read expanded into a whole document; writing it back stores the expansion. Re-read at `--depth 0`. `paths` names `field[i].key` |
| `blocks_field_inferred` | neither a schema nor a cached block census knew the field; it was chosen by looking at the document — pass `--field`, or run `pay blocks learn <collection>` once |
| `local_validation_skipped` | the piped envelope names a collection but no schema for it could be loaded, so `blockType` was **not** checked locally; run `pay blocks validate` before `pay apply` |
| `field_absent` | the piped document has no such field (usually a `--select` on the read trimmed it), or a row lacks a nested array other rows of its type carry — treated as empty; check the hint before `add` |
| `block_type_omitted` | `pay blocks add <slug>` on a plain ARRAY field: the row was stored without `blockType`; write `add -` |
| `stale_index` | a `--field` entered a row by POSITION in an array an earlier stage reordered; use id-based fields |
| `envelope_unwrapped` | `--data` was handed a whole PayCLI envelope and its `.data` was used as the body |
| `apply_all_fields` | `--all-fields` widened the write past the fields the pipeline recorded |

An `upstream_error` never appears as a warning: a stage whose stdin holds `ok:false`
re-emits that error with **its** code and **its** exit status, so a failed `pay get` in the
middle of a pipe surfaces as `doc_not_found` / exit 4 from the stage that could not run —
not as a bad-input error against a selector that was right.

### 3c. Error codes of blocks, rich text, safe writes and migration

| Code | Exit | Means | Do |
|---|---|---|---|
| `backup_failed` | 1 | the pre-write backup could not be read or written; **nothing was sent** | fix the directory (`pay doctor --path .backups`), or re-run with `--no-backup` deliberately |
| `not_published` | 4 | `pay get --published-only` on a document with no published state (never published, or unpublished) | read it as it is, or its draft with `--draft` |
| `ref_unresolved` | 4 | a `{"$ref": …}` matched no document, or `{"$ref":"@key"}` names no plan item | the hint has the `pay find` to check; fix the reference |
| `not_rich_text` | 5 | `pay lexical` input or `--field` holds no Lexical editor state | `did_you_mean` lists the real rich-text paths |
| `richtext_rendered` | 5 | a write (`pay apply`, a `pay blocks` edit stage, `--data`, a manifest) was handed a document read with `--richtext md\|text`, or a body with a string where the stored document holds rich text | re-read without `--richtext`; write Markdown with `--set-md` |
| `slug_ambiguous` | 5 | `--slug` matched several documents where one is needed | the ids are in `did_you_mean`; use an id |
| `match_ambiguous` | 5 | `pay upsert --match` / a manifest `match` found several documents | widen `--match` (ids in `did_you_mean`) |
| `ref_ambiguous` | 5 | a `$ref` matched several documents | add a field to the `$ref` or use `"where"` |
| `ref_cycle` | 5 | plan items reference each other in a cycle containing a document still to be created | create one of them without the reference first |
| `manifest_invalid` | 5 | a `pay plan`/`pay sync` manifest cannot be planned: bad JSON, unknown key, missing collection, two items for one document, an export read above `--depth 0` | `error.failures` names every item |
| `backup_target_mismatch` | 5 | `pay backups restore` of a file from another profile or server | `--profile <theirs>`, or `--ignore-target` for a deliberate copy |
| `preview_denied` | 8 | the site's preview route answered 403: wrong preview secret, or the credential is not a logged-in user | check `pay auth preview-secret` |
| `preview_secret_missing` | 9 | `pay url --preview` with a `{secret}` preview path and no secret configured; no request sent | `PAY_PREVIEW_SECRET`, `preview_secret_env`, or `pay auth preview-secret --stdin` |
| `preview_failed` | 10 | the preview handshake returned no cookie (no route at `preview_path`, a 5xx) | set the profile's `preview_path` to the site's real route |

Refusals that reuse existing codes: a plain write to a **published document with a
pending draft** is `invalid_args` (exit 5) — say `--draft`, `--publish` or `--unpublish`
(`safe-writes.md` §3); `--draft --publish` is `invalid_args`; `--reset-nested` with
`--where`, several ids or `--autosave` is `invalid_args`, and on a project with per-locale
status `feature_unavailable` (10); `pay blocks validate` with an error-severity issue is
`validation_failed` (exit 5, the report still in `data`); `--set-md` on a plain text field
or on an array row path (`layout.2.content`) is `invalid_args`.

### 3d. Warnings, by area

Warnings never change `ok` or the exit code. Their `hint` is usually the next command.

**Block census and validation** (`blocks.md`)

| Warning | Means |
|---|---|
| `blocks_unchecked` | a write could not check block rows: no census is cached for that collection. Run `pay blocks learn <collection>` once |
| `unknown_block_type` | a `blockType` never observed anywhere (and no declared list): Payload silently drops such a row |
| `block_type_not_observed_here` | the type exists, but was never seen at this field path |
| `unknown_field` | a key never seen on that block type and absent from any schema: Payload silently drops it |
| `value_not_in_enum` | a value outside the observed (or declared) values of a select-like field |
| `likely_required` | every observed row (≥ 3) fills this field; the new row leaves it empty |
| `empty_row` / `missing_block_type` / `invalid_row` / `required_field_missing` | error-severity issues of `pay blocks validate` (as warnings on writes) |
| `census_truncated` | the census read fewer documents than exist; raise `--census-max-docs` |
| `census_scope_wide` | a catalog verb without `--collection` read several entities; it states the cost |
| `census_entity_failed` | one entity's census read failed; the others are still reported |
| `census_unavailable` | `validate` ran with structural checks only (no census could be built) |
| `block_type_shapes_differ` | the slug means different blocks in different collections; pass `--collection` |
| `example_moved` | a referenced example row is no longer where the census saw it |
| `cache_unreadable` | a cached file (e.g. `census.json`) could not be decoded; it is ignored and rebuilt |

**Rich text** (`richtext.md`) — conversion findings are `lexical_<issue>` with
`KEY line N` paths:

| Warning | Means |
|---|---|
| `lexical_link_url_relative` | a link relative to the Markdown file (`../brief.md`); rewrite to `/path` or `pay:pages/ID` |
| `lexical_link_url_invalid` | an empty link URL or one with a space: drafts save, **publishing fails** |
| `lexical_link_reference_undefined` | `[t][label]` without a `[label]: url` definition; kept as text |
| `lexical_image_as_text` / `lexical_table_as_text` / `lexical_html_as_text` | not converted, kept as text (only `![media:ID]()` becomes an upload) |
| `lexical_code_block_flattened` | a fenced code block became inline-code lines |
| `lexical_list_item_block_flattened` / `lexical_list_item_paragraphs_joined` | a list item holds inline content only |
| `lexical_list_became_checklist` | a bullet list with a task item became a check list; its plain items are unchecked checkboxes |
| `lexical_nesting_capped` | quotes/lists nested deeper than 100 levels; the rest is paragraph text |
| `lexical_comment_dropped` | an HTML comment that is not a `<!-- lexical:… -->` placeholder was removed |
| `lexical_lossy_alignment` / `lexical_lossy_style` / `lexical_lossy_format` / `lexical_lossy_state_keys` | lost on the way to Markdown (alignment/indent, inline CSS, case transforms, keys next to `root`) |
| `lexical_placeholder` / `lexical_normalized` | informational: a node written as a lossless placeholder / an unusual shape written in the editor's native form |
| `frontmatter_stripped` | a leading YAML front matter block was removed (line count + first line in the message) |
| `set_md_literal_newline` | an inline `--set-md` value contains a literal `\n`; use `@file.md` or `$'…'` |
| `set_md_empty` | the `--set-md` file holds no Markdown: the field is cleared (inline `KEY=` clears without this warning) |
| `set_md_field_unchecked` | `--set-md` on a key nothing could type-check; confirm it is rich text |
| `set_md_not_richtext` | in a block row, the key held a plain string or the census saw a scalar |
| `stored_as_string` | the editor state you sent came back as JSON text: the field is a text field |
| `richtext_as_string` | a string where rich text is expected inside blocks (census, on `create` / `update --where`; `pay diff` against the stored document) — on other writes this is the error `richtext_rendered` |
| `array_as_object` | `pay diff`: the body has an object where the document has an array; `update` refuses it (`bad_request_body`) |

**Safe writes, drafts and orphans** (`safe-writes.md`, `blocks.md` §8)

| Warning | Means |
|---|---|
| `body_publishes` | the body carries `_status: "published"`: without a flag the write PUBLISHES; with `--draft`, `_status` was removed and a draft is saved |
| `publishes_pending_draft` | `--publish` on a published page with a pending draft publishes the draft's changes too (preview: `data.diff.published`; `pay plan` / `sync --dry-run` warn it for `"status": "published"`) |
| `saved_as_draft` | `upsert` / `sync` with the default status keep on a published page with a pending draft: the change went into that draft, the live page did not change; `--publish` / `"status": "published"` takes it live |
| `pending_draft_superseded` | dry run of a bulk non-draft `update --where`: sampled documents' pending drafts would be superseded, not built on |
| `diff_unavailable` | a dry run could not fetch the current document (the real write would fail) |
| `field_not_on_document` | `pay diff`: a key the stored document does not have — usually a typo Payload would drop |
| `never_published` | `pay diff --draft-vs-published` on a document with no published version |
| `data_redacted` | secret-looking values were masked in the output; `--no-redact` shows them |
| `orphan_risk` | this write (a nested block-type change or removal) will leave `{}` rows; use `--reset-nested` |
| `orphan_rows` | after the write, the stored row has `{}` / untyped / extra rows |
| `empty_rows` | the document read has `{}` rows; repair with `pay update <c> <id> --reset-nested` |
| `orphan_check_incomplete` | the stored row could not be read back, so orphans were neither confirmed nor ruled out |
| `reset_nested_not_needed` | `--reset-nested` had nothing to do (a draft save, or no nested change) |
| `reset_nested_incomplete` | a carrier phase failed after an earlier one was written; the requested body was still written |
| `reset_nested_unresolved` | some `{}` rows could not be traced to a block type in the last 50 versions |
| `publish_blocked` | `publish --dry-run` with several ids: the pre-check would block this id |

**Backups** (`safe-writes.md` §4)

| Warning | Means |
|---|---|
| `backups_disabled` | a `pay backups …` command ran with no backup directory configured |
| `backup_skipped` | the server answered `doc_not_found` for a targeted id (re-read on its own first), so it was not backed up; the write fails for it too |
| `backup_redacted` | the backup masked credential values; a restore cannot bring them back |
| `restored_as_draft` | a draft backup was restored as a new draft over a published document |
| `published_state_not_restored` | `restore --draft` left the published state as it was (the page may stay offline) |
| `backup_recreated_new_id` / `backup_recreated_in_trash` | `--recreate` created a new document (new id) / in the trash |
| `recreate_not_needed` | `--recreate` was given but the document still exists; it was restored in place |
| `restore_two_steps` | dry run: the restore writes the backup's published state live (`data.diff.published`), then its draft on top (`data.diff`) |
| `backup_fields_not_restored` | values masked at backup time were not sent |
| `config_path_anchored` | `pay config set backup_dir <relative>` stored the absolute path |

**Migration** (`migration.md`)

| Warning | Means |
|---|---|
| `body_status_ignored` | `_status` in an upsert body or manifest data was removed; `status` / `--draft` / `--publish` decide |
| `created_as_draft` | a new document was created as a draft; the message names what publishing still needs |
| `body_id_mismatch` | the body's own `id` names another document than the one being written (usually an export whose slug was edited: the plan then creates a copy) |
| `match_value_normalized` | the server stored a different match value (a slug hook); put the stored value in the manifest |
| `redacted_not_sent` | `"<redacted>"` values from an export were left out of a top-level field, group or row: the stored value stays |
| `redacted_removed` | `"<redacted>"` keys inside rich text / json had no stored value to restore from and were removed; export with `--no-redact` to keep them |
| `hook_rewrote_unsent` | a write changed a field the body declares but did not send (a server hook); sync/upsert again to write it back |
| `ref_target_unchecked` | a `$ref` sits in a field whose `relationTo` discovery does not know, so its collection could not be checked |
| `relation_id_not_numeric` | a link/relationship names a numeric-id collection's document by a non-numeric string (`pay:pages/about`); use the id or `pay:pages/slug=about` |

**URLs and preview** (`migration.md` §6)

| Warning | Means |
|---|---|
| `url_path_guessed` | the path is `/{slug}` outside `pages`; teach the route with `url_paths` |
| `url_not_live` | the document was never published: the URL 404s for visitors |
| `preview_credential_withheld` | `site_url`'s origin differs from `base_url`'s, so the handshake was sent without the credential |
| `preview_secret_shadowed` | `pay auth preview-secret --stdin` stored a secret, but an environment variable wins over it |

## 4. Exit 7 in detail — the one that damages data

A Payload bulk write returns HTTP **400 whenever `errors[]` is non-empty, even when some
documents succeeded and committed**, because the outer transaction commits after the
per-document loop. Never trust the status code on a bulk verb; always read the body.

```json
{"ok": false, "v": 1, "command": "update", "data_kind": "bulk_result", "partial": true,
 "data": {"succeeded": [{"id": 11}], "failed": [{"id": 10}], "not_attempted": []},
 "changed": {"created": 0, "updated": 1, "deleted": 0, "trashed": 0, "ids": [11]},
 "error": {"code": "partial_failure", "exit": 7,
           "message": "Updated 1 of 2 documents in \"pages\"; 1 failed.",
           "hint": "THE 1 SUCCESSFUL WRITE IS ALREADY COMMITTED... do not re-run this command...",
           "retriable": false,
           "failures": [{"id": 10, "code": "validation_failed",
                         "message": "The following field is invalid: Content > Layout",
                         "fields": [{"path": "layout", "message": "This field requires at least 1 Row."}]}]},
 "next": {"reason": "retry_failed_subset",
          "cmd": "pay update pages --where 'id in 10' --set title='Bulk Test'",
          "args": {"ids": [10]}}}
```

Rules:

1. Do **not** re-run the original command. The successful writes are committed and would
   be re-applied.
2. Retry only `next.args.ids`, using `next.cmd`.
3. `failures[].fields` is best-effort: Payload gives only a message string for bulk
   sub-errors, so the per-field breakdown may be empty.
4. A failure whose real message Payload withheld (`isPublic: false`) arrives as
   `code: "server_error"` with a note saying so; `debug: true` in `payload.config.ts`
   would reveal it.
5. `--per-doc` turns a bulk update into individual `PATCH /{id}` calls, so one bad
   document cannot poison its batch-mates. It multiplies the request count by N.

## 5. The six server body shapes

`pay` normalises all of these into the one error object. Classification **never reads a
human-readable message**, because Payload runs its strings through i18n:

1. body has `docs` **and** a non-empty `errors` → `partial_failure`
2. `errors[0].name == "ValidationError"` with `errors[0].data.errors` as an array →
   `validation_failed`; `fields` is that array
3. `errors[0].name == "QueryError"` with `errors[0].data` as an **array** →
   `query_path_invalid`. Note the type difference from (2): code that reaches for
   `err.data.errors` on a `QueryError` crashes
4. `errors[*]` carry a `field` key (the Mongoose branch) → `validation_failed`
5. body has `message` and no `errors` → `route_not_found` (404) / `operation_unsupported` (501)
6. anything else → mapped by HTTP status

Unparseable bodies degrade to `code: "unknown"` with `raw` preserved. A response that is
not `application/json` becomes `non_json_response` (exit 6) with a short body excerpt —
and when it contains `id="__next_error__"`, the note that you hit Next.js's error boundary
rather than Payload.

## 6. HTTP status → code

| HTTP | Body signal | Code | Exit |
|---|---|---|---|
| 400 | `name:"ValidationError"` | `validation_failed` | 5 |
| 400 | `name:"QueryError"` | `query_path_invalid` | 5 |
| 400 | `"Invalid JSON"` | `bad_request_body` | 5 |
| 400 | missing `where` on a bulk verb | `where_required` | 5 |
| 400 | upload collection, no `errors[0].name` | `file_missing` | 5 |
| 400 | has `docs` **and** `errors[]` | `partial_failure` | **7** |
| 401 | any | `auth_invalid` | 2 |
| 403 | api-key/jwt **and** a verified identity | `access_denied` | **8** |
| 403 | anonymous, or identity null/unverified | `auth_required` | 2 |
| 404 | `{"errors":[{"message":"Not Found"}]}` | `doc_not_found` | 4 |
| 404 | `{"message":"Route not found …"}` | `route_not_found` / `collection_unknown` | 4 / 10 |
| 404 + `text/html` | `id="__next_error__"` | `non_json_response` | 6 |
| 413 / 414 / 431 | — | `request_too_large` | 5 |
| 423 | `Locked` | `doc_locked` | 3 |
| 429 | proxy / WAF | `rate_limited` | 3 |
| 500 | `"Something went wrong."` | `server_error` + `likely_causes[]` | 6 |
| 501 | `{"message":"Cannot <M> …"}` | `operation_unsupported` | 10 |
| 502 / 504 | — | `server_unavailable` | 6 |
| 503 | with `Retry-After` | `server_busy` | 3 |
| 503 | without `Retry-After` | `server_unavailable` | 6 |

The 401-vs-403 split is the one the server cannot make for you: a 403 body is
byte-identical whether you are unauthenticated or merely unpermitted. `pay` disambiguates
with its own verified identity, which is why "fix your key" (exit 2) and "ask for
permission" (exit 8) are different codes.

## 7. Opaque 500s

Payload masks non-public errors as `{"errors":[{"message":"Something went wrong."}]}`
unless `config.debug` is on. Three client mistakes produce it:

| Cause | What `pay` does |
|---|---|
| an id that does not parse as the collection's id type | rejected before the call: `invalid_id` (exit 5) — but only when the id type is actually known |
| `/versions` on a collection without versions | rejected before the call: `feature_unavailable` (exit 10) |
| a relationship pointing at a non-existent document | diagnosed after the fact in `likely_causes[]`, with `pay get <relTo> <id>` as the fix |

When `pay` does not *know* a fact (id type unknown, drafts unknown), it **sends the
request** and attaches a `*_unknown` warning instead of rejecting locally. A client-side
rejection built on a fact it never learned would be confidently wrong.

## 8. The rule

**Never branch on `error.message` — it is translated.** `followingFieldsInvalid`,
`noFilesUploaded`, `notAllowedToPerformAction` and friends all go through Payload's `req.t`,
so their text changes with the project's `i18n` config, a `payload-lng` cookie, or a
proxy-injected `Accept-Language`. Branch on `ok`, `error.code` and `error.exit`.
