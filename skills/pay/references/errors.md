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
| 1 | generic / internal | no | `internal`, `unknown`, `cache_corrupt`, `update_verification_failed`, `audit_write_failed` |
| 2 | auth — *your credentials* | no | `auth_missing`, `auth_invalid`, `auth_required`, `auth_locked`, `auth_unverified_email`, `auth_insecure_permissions`, `auth_helper_failed` |
| 3 | throttled / temporarily unavailable | **yes** | `rate_limited` (429), `doc_locked` (423), `server_busy` (503 with `Retry-After`) |
| 4 | not found | no | `doc_not_found`, `route_not_found`, `version_not_found`, `selector_no_match` |
| 5 | validation / bad input | no | `validation_failed`, `query_path_invalid`, `invalid_args`, `invalid_where_syntax`, `invalid_sort_field`, `unknown_field`, `invalid_option`, `invalid_id`, `bad_request_body`, `where_required`, `file_missing`, `not_upload_collection`, `unsupported_operator`, `bulk_limit_exceeded`, `request_too_large`, `format_unsupported`, `invalid_path_expr`, `selector_ambiguous`, `field_ambiguous`, `no_input`, `no_edits` |
| 6 | network / server | **yes**, except 500 | `network_unreachable`, `dns_failure`, `tls_error`, `timeout`, `server_error` (500, **not** retried), `server_unavailable` (502/504), `non_json_response` |
| **7** | **partial failure** | **never** | `partial_failure` |
| 8 | access denied — identity valid, permission not | no | `access_denied`, `admin_access_denied` |
| 9 | config / profile | no | `config_missing`, `config_secret_in_plaintext`, `profile_unknown`, `base_url_invalid`, `endpoint_not_payload`, `auth_collection_unknown` |
| 10 | capability / discovery | no | `collection_unknown`, `global_unknown`, `feature_unavailable`, `operation_unsupported` (501), `discovery_failed`, `graphql_disabled`, `schema_stale`, `block_type_unknown` |
| 11 | confirmation required | no | `confirmation_required` |

`pay` never uses 125–128 or 130; those belong to the shell.

### 3a. The edit-pipeline codes

Six codes belong to `pay blocks` / `pay apply` (`recipes.md` §14). They are worth knowing
apart because all six are **local** — nothing was sent, so nothing changed.

| Code | Exit | Means | Do |
|---|---|---|---|
| `selector_no_match` | 4 | the selector addressed no row | the rows that exist are in `hint`, and `did_you_mean` offers the nearest real selectors |
| `selector_ambiguous` | 5 | it matched several rows and the verb acts on one | `did_you_mean` lists every match ready to paste; or `rm --all` to mean all of them |
| `field_ambiguous` | 5 | more than one blocks field, and no `--field` | pass `--field`; the candidates are in `did_you_mean` |
| `no_input` | 5 | stdin was empty | these commands edit a document read from stdin: `pay get … \| pay blocks …` |
| `no_edits` | 5 | `pay apply` got an envelope no transform touched | put a `pay blocks` stage in the pipe, or use `pay update <coll> <id> --data @-` to write the whole document |
| `block_type_unknown` | 10 | this field accepts no such `blockType` | it is exit 10 because it is a fact about the *project*; `did_you_mean` comes from the field's own resolved slugs |

`selector_no_match` is a **4**, not a 5: the selector is well formed and the document
simply does not contain that row — the same distinction `doc_not_found` draws.

### 3b. Warnings the edit pipeline raises

Warnings never change `ok` or the exit code, and these five are the ones that decide
whether a write is going to be correct.

| Warning | Means |
|---|---|
| `populated_relationship` | a row carries a relationship the read expanded into a whole document; writing it back stores the expansion. Re-read at `--depth 0`. `paths` names `field[i].key` |
| `blocks_field_inferred` | the field was chosen by looking at the document, not at a schema — run `pay discover --refresh`, or pass `--field` |
| `local_validation_skipped` | no schema was available, so `blockType` was **not** checked; an unknown one will reach Payload, which drops the row and answers 201 |
| `field_absent` | the piped document has no such field — usually a `--select` on the read that trimmed it |
| `envelope_unwrapped` | `--data` was handed a whole PayCLI envelope and its `.data` was used as the body |
| `apply_all_fields` | `--all-fields` widened the write past the fields the pipeline recorded |

An `upstream_error` never appears as a warning: a stage whose stdin holds `ok:false`
re-emits that error with **its** code and **its** exit status, so a failed `pay get` in the
middle of a pipe surfaces as `doc_not_found` / exit 4 from the stage that could not run —
not as a bad-input error against a selector that was right.

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
