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

## [0.3.0] — 2026-09-28

### Added

- **A block census: learn a site's blocks without its source code.** With GraphQL
  introspection disabled — Payload's production default — discovery sees no block slugs and
  no block schemas. The census learns them from the documents: it pages through every
  collection and global that holds blocks (default 500 documents each), walks rows at every
  depth, and records per field pattern (`layout`, `layout[].blocks`, `group.blocks`) which
  block types occur, how often, which fields each carries and which values they take. Every
  fact names its source (`observed-census`, `graphql`, `project-source`, `configured`). New
  `pay blocks` verbs that read no stdin: `types [COLLECTION] [--field PATH]` (which types
  exist where, with counts), `schema <blockType> [--collection C]` (fields, kinds,
  presence, observed values, required-ness `true`/`likely`/unknown), `example <blockType>
  [--n N]` (real rows, row ids stripped), `new <blockType> [--set …] [--from-example]` (a
  ready-to-edit row shaped like the real ones, with `fill_in` and `defaults_from_values`
  saying what to check), `validate [--data-file F | stdin]` (every row at every depth; exit 5
  on `{}` rows, missing `blockType` or a declared violation, warnings for everything learned
  only from documents) and `learn [COLLECTION…]` (rebuild and report what changed — "grid
  gained splitDesktop"). `pay blocks add <type> --template` inserts the `new` row directly.
  `pay describe --field/--block` and shell completion fall back to the census; `pay doctor`
  says what disabled introspection costs and names the Payload option that turns it back
  on. New config `[defaults] census_max_docs`, env `PAY_CENSUS_MAX_DOCS`, flag
  `--census-max-docs`.
- **Nested block editing.** Every `pay blocks` verb (`ls`, `mv`, `rm`, `add`, `cp`, `set`)
  works at any depth: `--field` takes `layout.2.blocks`, `layout[2].blocks`,
  `layout[id:…].blocks` or `layout[type:group[1]].blocks`, and `pay blocks ls --recursive`
  prints the exact `--field` and selector for every row. `add -` adds a row to a plain array
  field (links, buttons), which has no `blockType`. `edits.fields` still names the top-level
  field, so `pay apply` sends one key. A nested path that does not resolve is an error
  (`selector_no_match` exit 4, `unknown_field` / `invalid_args` exit 5), never an empty
  listing.
- **`pay outline`** (`<collection> <id>`, `--slug`, `--global`, or a piped document): one
  entry per row at every depth with its path, `--field` + selector, block type, a one-line
  summary and flags (`empty_row`, `no_block_type`, `unknown_type`); `--output table` draws an
  indented tree.
- **Rich text ⇄ Markdown.** `pay lexical from-md | to-md | text` convert locally (L0, no
  profile) in the shape Payload 3.87 stores, including `[t](pay:pages/47)` internal links,
  `{target=_blank}`, `![media:40]()` uploads and lossless `<!-- lexical:… -->` placeholders
  for every other node. `--set-md KEY=@file.md` on `create`, `update`, `upsert`,
  `globals update`, `upload`, `diff` and `pay blocks add|set|new` (precedence `--data` →
  `--set` → `--set-json` → `--set-md`; YAML front matter is stripped, `frontmatter_stripped`).
  `--richtext json|md|text` on `get`, `find`, `globals get` and `versions get` renders every
  rich-text value for reading; `pay apply`, `--data`/`--data-file` and the `pay blocks`
  stages refuse such output (`richtext_rendered`, exit 5) because Payload would store the
  string. Without an envelope (`--output raw`, jq) the stored document is the evidence: an
  update by id, `apply`, `globals update`, `upsert` and `sync` refuse a string where the
  document holds an editor state (block rich text included, no census needed) and an object
  where it holds an array (`{"layout":{"0":…}}`, which empties the array; `bad_request_body`).
  `--set-md KEY=@-` with empty stdin is `no_input`; `pay lexical from-md --as` refuses an array
  row (`layout.0.content`) like `--set-md` does.
- **Structural diffs.** `pay diff` compares a document with the result of a write body
  (`--data-file`, `--set`, `--set-md`; Payload's PATCH semantics), `--draft-vs-published`,
  two documents, a global, or two local files (`--left/--right`, offline). Rows are matched
  by id (a move is a move), rich text is shown as text, `createdAt`/`updatedAt` are ignored
  unless `--include-timestamps`, `--ignore PATH` hides more. Exit 0 whether or not the sides
  differ: branch on `data.identical`.
- **Every `--dry-run` of a write to existing documents shows `data.diff`** — `update`,
  `apply`, `globals update`, `publish`, `unpublish`, `backups restore`, `versions restore`;
  bulk forms diff the first 5 sampled documents. When the live row differs from the newest
  draft the preview adds `data.diff.published`: what visitors will see change.
- **Automatic pre-write backups.** With `PAY_BACKUP_DIR`, `backup_dir` (in `pay.toml` or
  `config.toml`; a relative path in a file means "next to that file") or `--backup-dir`,
  every write that changes or removes an existing document — update, apply,
  publish/unpublish, delete (soft, permanent, bulk), restore, versions restore, globals
  update, `upload --replace` — first saves it as
  `<dir>/<profile>/<collection>/<id>/<UTC-time>--<command>.json` (0600) and lists the files
  in `meta.backups` and the audit record. A backup that cannot be written aborts the write
  (`backup_failed`, exit 1); `--no-backup` / `PAY_NO_BACKUP` skip it deliberately.
  `pay backups list | show | restore | prune`: `restore` restores in place (`--draft` /
  `--publish` to choose the state), `--recreate` re-creates a permanently deleted document
  (new id), and a backup from another profile or server is refused
  (`backup_target_mismatch`).
- **Idempotent writes.** `{"$ref":"pages","slug":"about"}` in any write body is resolved to
  the id before the request (`"$as":"polymorphic"` yields `{relationTo, value}`).
  `pay upsert <collection> [--match slug]` creates, updates the fields that differ (plus the
  unchanged scalar fields the body declares, so a hook such as `populatePublishedAt` cannot
  overwrite them), or does nothing (`meta.upsert.action`). A `$ref` naming a collection its
  field's known `relationTo` excludes is `bad_request_body`; in Markdown,
  `[t](pay:pages/slug=about)` links a page by slug. `pay plan <DIR|FILE…>` / `pay sync … --yes` run a
  directory of manifest items (collections and globals) in dependency order
  (`{"$ref":"@key"}` refers to another item); a saved `pay get … --depth 0` envelope is a
  valid manifest, and exit 7 from `sync` is safe to re-run. Without `--publish` /
  `"status"`, a document keeps the state of its newest version: a published page with a
  pending draft is saved into that draft, the live page unchanged (`saved_as_draft`).
- **Frontend URLs and draft preview.** `pay url <collection> <id>|--slug S` returns the
  page's site URL (from nested-docs breadcrumbs, a `url_paths` template or the slug).
  `--preview` performs the Payload website template's draft-preview handshake and writes
  the Next.js draft-mode cookie to a 0600 file, so a browser can look at a draft without
  publishing it. `pay auth preview-secret` stores the secret or reports where it resolves
  from. New per-profile config `site_url`, `url_paths.<collection>`, `preview_path`,
  `preview_secret_env`; env `PAY_SITE_URL`, `PAY_PREVIEW_SECRET`,
  `PAY_PREVIEW_SECRET_<PROFILE>`.
- **`pay get <collection> --slug S`** (also on `outline` and `url`): 0 matches is
  `doc_not_found`, several is `slug_ambiguous` — never "the first match".
  `pay get … --published-only` returns the published state or `not_published` (exit 4).
- **`pay publish` / `pay unpublish` with several ids** (`pay publish pages 38 39 40`): one
  PATCH per id, a `bulk_result`, exit 7 on partial failure with a `next.cmd` that retries
  only the failed ids; `--fail-fast` stops at the first failure.
- **Orphan rows: detected, and repaired without changing the id.** On Postgres, changing a
  *nested* block's type leaves the old nested rows behind, reading back as `{}`. PayCLI now
  predicts it (`orphan_risk` on a dry run), detects it after a write (`orphan_rows`, from a
  read-back of the stored row) and on read (`empty_rows` on `get` / `globals get`).
  `--reset-nested` on `update <id>`, `apply`, `globals update` and `publish|unpublish <id>`
  performs a verified multi-request write that leaves none; with no body it repairs a
  document that already has them. The previous fix was delete + re-create, which changed
  the id.
- `pay doctor` gains `binary_on_path` (warns when the `pay` a shell would run is not this
  binary — agents' non-interactive shells often lack the PATH entry from `~/.bashrc` — with
  the exact fix; `install.sh` prints the same advice), `backups`, `block_census` and the
  URL/preview configuration, and still reports `audit_log`, `backups`, `skill` and `update`
  when no `base_url` is configured.
- `pay globals update` gains `--publish` / `--unpublish`; `pay apply --publish/--unpublish`
  now applies to globals too.
- New error codes: `backup_failed` (exit 1); `not_published`, `ref_unresolved` (4);
  `not_rich_text`, `richtext_rendered`, `backup_target_mismatch`, `slug_ambiguous`,
  `match_ambiguous`, `ref_ambiguous`, `ref_cycle`, `manifest_invalid` (5); `preview_denied`
  (8); `preview_secret_missing` (9); `preview_failed` (10).
- New warning codes, among them `body_publishes`, `publishes_pending_draft`,
  `pending_draft_superseded`, `diff_unavailable`, `orphan_risk`, `orphan_rows`,
  `empty_rows`, `orphan_check_incomplete`, `reset_nested_*`, `blocks_unchecked`,
  `census_truncated`, `census_scope_wide`, `backup_redacted`, `backup_skipped`,
  `backups_disabled`, `restored_as_draft`, `backup_recreated_new_id`,
  `body_status_ignored`, `created_as_draft`, `match_value_normalized`, `redacted_not_sent`,
  `redacted_removed`, `hook_rewrote_unsent`, `ref_target_unchecked`, `relation_id_not_numeric`,
  `saved_as_draft`,
  `stored_as_string`, `set_md_*`, `lexical_*`, `richtext_as_string`, `array_as_object`,
  `url_path_guessed`, `url_not_live`.
  `pay blocks validate` issue codes: `empty_row`, `missing_block_type`, `unknown_block_type`,
  `block_type_not_observed_here`, `unknown_field`, `value_not_in_enum`, `likely_required`,
  `required_field_missing`.
- New `meta` keys, present only when relevant: `backups`, `richtext`, `reset_nested`,
  `upsert`, `restored_from`; the audit event gains `backups`, `step` and `restored_from`.
  **No new `data_kind`**: the new commands answer `op_result`, `doc` or `bulk_result`.

### Changed

Behaviour an upgrading script must know about. None of it is breaking under the rules
above, but several writes that used to go through now stop and ask.

- **A plain `pay update` of a published document with a pending draft is refused**
  (`invalid_args`, exit 5 — in the dry run too). Payload merges an update into the *newest*
  version and stores it as the live row, draft `_status` included, so the write used to
  publish the draft's changes *and* take the page offline. The same applies to a body
  carrying `_status: "draft"` over a published document. Say what you mean: `--draft` (edit
  the draft), `--publish` (publish it with the change; warns `publishes_pending_draft`) or
  `--unpublish` (take it offline deliberately). Covers `update <id>`, `apply` and
  `globals update`. `upsert` / `sync` are not refused: their default status keep saves
  into the pending draft (`saved_as_draft`), and status published warns
  `publishes_pending_draft` — in `pay plan` and `sync --dry-run` too, which read the
  stored row's `_status` for that (one extra GET per such item). To decide, a real non-draft
  write on a drafts-enabled entity first reads the publication state: one or two small
  `select=_status` GETs.
- **`--draft --publish` together is `invalid_args`**, before anything is read or sent.
- **`--draft` wins over a body's `_status`.** A body with `_status: "published"` — every
  export of a published page — used to publish even with `--draft`. Under `--draft` it is now
  removed and the write saves a draft (`body_publishes` says so); without a flag it is sent
  as before and still publishes, now with a loud `body_publishes`. In `upsert` / `sync`,
  status comes only from `--draft` / `--publish` or the manifest's `status`; a body
  `_status` is dropped (`body_status_ignored`).
- **Dry runs now read before they answer.** A `--dry-run` of a write to existing documents
  costs one extra GET (two on a drafts-enabled entity; bulk forms read the sampled
  documents) to build `data.diff`. A failed read is the warning `diff_unavailable`; the
  preview still succeeds, and still writes nothing.
- **Writes may read the stored row back once** — after a non-draft write to a versioned
  entity whose body or echo holds nested lists — for the `orphan_rows` check.
  `--no-echo-check` skips it.
- **`pay publish --where` now sends `?draft=true`**, like the admin panel's bulk Publish, so
  it publishes the newest drafts; before, it re-published the stored content and dropped
  pending drafts. `pay update --where` without `--draft` still merges into the stored rows;
  its dry run now warns `pending_draft_superseded` for sampled documents with a pending
  draft.
- **`pay versions diff` uses the same structural diff as `pay diff`.** The v0.1.0 keys stay
  (`a`, `b`, `changed`, `changes[].{path,kind,a,b}`), but rows are matched by id where the
  versions share ids and by position otherwise — Postgres gives every version snapshot new
  row ids, so a stage page that reported 111 "changes" now reports its 7 real ones. `kind`
  gains `moved`; a re-typed block is one change; a rich-text edit is one change per field,
  carried as text; timestamps are ignored at every depth (before: top level only) and
  `null` equals an absent key. New: `identical`, `summary`, per-change `op` / `value_kind` /
  `id` / `block_type`, `--ignore`, `--include-timestamps`, `--full`, `--output table`.
- **Block warnings on writes are census-aware.** Create, update, apply and `globals update`
  no longer raise `unknown_block_type` on every write to a site without introspection: they
  check rows at every depth against the cached census of that collection and warn only on
  real anomalies. A write never builds the census; with none cached you get one
  `blocks_unchecked` warning (run `pay blocks learn <collection>` once). `pay blocks add`
  refuses a slug (`block_type_unknown`) only when the accepted list is authoritative, not
  merely observed; with a cached census, `pay blocks add` and `set --set blockType=` warn
  `unknown_block_type` (with a did-you-mean) or `block_type_not_observed_here` right away.
- **`pay blocks set --unset KEY` clears the field instead of deleting the key.** Payload
  keeps the stored value of a key a row omits (the row is matched by its id), so the old
  `--unset blockName | pay apply` answered 2xx and changed nothing. It now writes `null`
  (`[]` for an array, every sub-field `null` for a group — `null` there is a 500). The edit
  log says `blockName (cleared)`; `--unset blockType` / `--unset id` are `invalid_args`.
- **New cache file** `$CACHE/v1/<scope>/census.json` (block census; fresh 24 h, hard max 7
  days; cleared with the scope by `pay cache clear`, rebuilt by `--refresh` or
  `pay blocks learn`). It holds aggregates, not documents, and never samples the values of
  secret-named fields. **New state files:** `<state_dir>/preview/<profile>.cookies.json`
  (`url --preview`, 0600) and `<state_dir>/reset-nested/<profile>/…json` (written only when
  a `--reset-nested` restatement fails, for the recovery command).
- `pay doctor` reports `graphql` as `warn` (was `unknown`) when introspection is disabled,
  saying what degrades and how to enable it (`graphQL.disableIntrospectionInProduction:
  false`).
- A per-document failure in a `bulk_result` (`--per-doc`, explicit ids) carries its own
  `error.failures[].code` — `doc_not_found` for a missing id — instead of a blanket
  `validation_failed`. When the required-field pre-check blocks *every* id of a
  several-ids or `--where` publish, the answer is `validation_failed` (exit 5) with nothing
  sent, not `partial_failure`.

### Fixed

- `pay blocks cp` (and `add` from a copied row) stripped every `id` key at every depth, including
  upload and relationship ids and the ids inside rich text, so a copied row lost its images and
  internal links. It now strips only row `id`s and keeps every reference.
- `page.truncated` was `false` on every paginated read that was not `--all`, even with more
  pages to come, so an agent following the documented "run `next.cmd` until
  `page.truncated` is false" stopped after page 1. It now means "more matching documents
  exist than this envelope holds" on `find` and `versions list`: `has_next_page` on a page,
  `--all` stopped by `--max` as before.
- Hints that named commands which do not exist now name ones that answer them:
  `pay globals list` (was `pay collections --globals`), `pay collections --kind upload`
  (was `--uploads`), `pay explain --section query_syntax` (was `--section where`),
  `pay collections` for an unsupported operation (was `--section endpoints`),
  `pay auth login --base-url <url>` (was `pay config init`), `pay auth list` (was
  `pay profile list`), `pay update-self` (was `pay self-update`),
  `pay versions list <collection> --id <id>`. A unit test now checks every `pay …` command
  the code names against the real command tree.
- `pay globals update` reported every sent field as `input_silently_dropped`: Payload 3.87
  answers a global update with `{"message", "result": {…}}`, and the wrapper was diffed as
  the document.
- `pay publish` pre-validation reported required paths inside an **empty** array as
  missing (`hero.links.link.label` with `hero.links: []`). Required paths through arrays are
  now checked per existing row (`hero.links[1].link.label`), as the server does.
- `pay blocks add --set link.url=/x` stored a literal `"link.url"` key, which Payload
  dropped; dotted keys now build `{"link":{"url":"/x"}}`.
- Text redaction no longer swallows the word after a masked value in error messages.
- A numeric id written with a leading zero or whitespace (`pay delete pages 0108`) reached
  the document but skipped its pre-write backup (`backup_skipped`), even on
  `delete --permanent`. Write commands now canonicalize number ids (`0108` → `108`) right
  after validation; the backup pairs documents with ids numerically and re-reads an id the
  batched read missed, so `backup_skipped` now means the server answered `doc_not_found`.
  Several-ids dry runs list such ids in `data.diff.missing` (`diff_unavailable`) instead
  of dropping them, and `would_backup` counts only the documents that exist.
- `--draft --unpublish` (update, apply, globals update) was accepted and saved only a draft
  version while the page stayed live; it is now `invalid_args` like `--draft --publish`.
- `pay versions restore` of a draft version over a published document without `--draft`
  unpublished it silently. It is now refused (`invalid_args`, dry run too); the new
  `--unpublish` flag does it deliberately, `--draft` restores a new draft on top.
- A create rejected for required fields it did not send got the update-only hint "the
  stored document is already invalid"; it now says the fields are missing from the body.
- The dry run of a two-step `pay backups restore` (a backup with `published_doc`) diffed
  only the draft step: it now adds `data.diff.published` (the live row's change) and the
  warning `restore_two_steps`.
- **`"<redacted>"` is no longer written back.** A get → edit → write loop sent the mask
  verbatim through `update --data @-`, `apply`, `globals update`, `create` and `upload`,
  replacing the stored value with the string. Every write now leaves masked values out of
  top-level fields, groups and rows (`redacted_not_sent`: the stored value stays), and puts
  masked keys inside rich text or json — which Payload stores whole, so leaving a key out
  would delete it — back from the stored document (`redacted_removed` when there is none).
  A `null` under a secret-looking key is no longer masked on read.
- The block census counted a document again on every write of it: its echo was folded in
  as new rows each time, so iterating on one page inflated `blocks types`/`schema` counts and
  made fields "likely required" (false `likely_required` warnings after two or three
  writes). The census now records the ids it read (`doc_keys`) and folded (`folded_keys`);
  a document already counted only refreshes it (new positions, types, fields, values), and
  a field it leaves empty is never `likely_required` again (`optional`).
- Writes whose blocks field holds rows that ALL lack a `blockType` (`layout=[{}]`) were not
  checked at all, not even on `--dry-run`, while Payload drops such rows with a 2xx and empties
  the field. The write check now runs for any non-empty array at a known blocks field
  (`empty_row`, `missing_block_type`).
- Block validation accepted a nested blockType when ANY parent at that depth held it
  (a `bentoItem` in a group's `blocks`, because a bento's `blocks` shares the pattern
  `layout[].blocks`). The parent type's own field (`group.blocks`) is now the observed set,
  as `pay blocks ls` already reported it.
- A stray `blockType` on a row of a plain array (`hero.buttons`) made validation treat the
  whole array as blocks and fail its untouched rows (exit 5); the stray key is now reported
  as `unknown_field`.
- `pay blocks learn` after a truncated read reported every type it did not read as "no longer
  used"; such absences are now `change: "not_seen"`.
- `pay blocks new … | pay blocks validate` ran structural checks only and checked the row at
  `layout`: `blocks new --collection` now sets the envelope target, and validate checks a
  `blocks new` row at its most-used `used_in` position.

### Security

- The preview secret is never printed — masked in every URL, log and audit record —
  `preview_secret` in a config file is refused (`config_secret_in_plaintext`, exit 9), and
  `pay auth preview-secret` shows only its source and a fingerprint. When `site_url`'s origin
  differs from `base_url`'s, the API credential is withheld from the preview handshake
  (`preview_credential_withheld`).
- Backup files are 0600 in 0700 directories. Backups mask credentials only (`apiKey`,
  reset/verification tokens, a user's `hash`/`salt`/`password`/`sessions`), and the write
  warns `backup_redacted` when they did; `pay backups restore` never writes a masked value
  back.

## [0.2.0] — 2026-09-23

### Added

- **An edit pipeline for blocks: `pay blocks` + `pay apply`.** Reordering or dropping a block used to
  be read-to-a-file, edit-with-jq, write-back-with-`--set-json`. It is now one pipe:

  ```sh
  pay get pages 12 --depth 0 | pay blocks mv type:cta --after type:mediaBlock | pay apply --yes
  ```

  Six local verbs — `ls`, `mv`, `rm`, `add`, `cp`, `set` — each read ONE document from stdin, edit a
  blocks (or array) field, and write the document to stdout. They compose in any order and any
  number; `pay apply` is the only stage that reaches the network. All six are L0 and need no profile,
  credential or cache, so they also work on a JSON file.
- **`pay apply` writes only the fields the pipeline touched.** Each transform records what it changed
  in the new `envelope.edits` (`{fields, ops}`), and `apply` builds its PATCH from `edits.fields`
  alone. PATCHing the whole piped document back would also rewrite `_status`, `createdAt` and every
  relationship a read above `--depth 0` expanded into a full document — a one-block reorder silently
  republishing a page. `--all-fields` opts out and warns. Otherwise `apply` *is* `pay update <id>`:
  same risk level, confirmation, audit record, echo-diff and code path. The collection and id come
  from the piped envelope's `target`, so nothing is retyped at the end of a pipe.
- **A closed selector grammar** — `N`, `-N`, `first`, `last`, `id:V`, `name:V`, `type:SLUG`,
  `type:SLUG[N]` — shared by every verb. `--before`/`--after` take a *selector*, not an index,
  because "after the media block" survives another stage editing the array and "at index 3" does not.
  `pay blocks ls` prints, per row, the shortest selector that addresses it and no other (an `id:`
  whenever the row has one). A selector matching several rows where one was required is
  `selector_ambiguous` (exit 5) listing every match ready to paste; `rm --all` is the only way to
  mean "every match". No match is `selector_no_match` (exit 4) with the rows that do exist.
- **Four Payload footguns are now local failures or warnings**, all verified live: the anchor index
  shifting once a moved row is lifted out (`mv` resolves the anchor before the removal and recomputes
  after it); a duplicated row keeping its `id` and therefore *overwriting* its original rather than
  adding a row (`cp` strips every `id` at every depth); an unknown `blockType`, which Payload **drops
  while answering 201** (`block_type_unknown`, exit 10, with `did_you_mean` from the field's own
  slugs); and a relationship expanded by a read above `--depth 0` being written back as an object
  (`populated_relationship` warning naming `field[i].key`).
- **`--data @-` now unwraps a piped PayCLI envelope.** `pay get … | pay update … --data @-` used to
  send `{"ok":true,"data":{…},"meta":{…}}` as the request body, which Payload accepts with a 2xx and
  silently drops every key of — the write looked successful and changed nothing. It now uses the
  envelope's `.data` and raises `envelope_unwrapped`; an error envelope fails with the *upstream's*
  code. Detection needs `ok`, `v`, `data_kind` and `meta` together, so a collection with a boolean
  `ok` field is never mistaken for an envelope.
- New error codes: `selector_no_match` (exit 4), `selector_ambiguous`, `field_ambiguous`, `no_input`,
  `no_edits` (exit 5) and `block_type_unknown` (exit 10). New warning codes:
  `blocks_field_inferred`, `field_absent`, `populated_relationship`, `envelope_unwrapped`,
  `local_validation_skipped`, `apply_all_fields`, `upstream_error`.
- New envelope key `edits`, present only on the edit-pipeline commands.

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
- **What a block IS.** Every block type now carries the project's own `label`,
  `label_plural` and a one-sentence `description`, so an agent can choose between `cta`,
  `content` and `mediaBlock` without opening the project. They are printed as
  `block_docs[SLUG]` beside every slug list (`pay describe <e>`,
  `pay describe <e> --field <blocks-field>`, `pay explain --collection <e>`), repeated as
  `.docs` by `--block <slug>`, and rendered as the shell-completion description of
  `--block`. Measured: `pay describe pages` 31 KB → 34 KB, `describe forms` 25 KB → 30 KB;
  the field *schemas* stay behind `--blocks-detail` (70 KB → 81 KB).
- Each field inside a block carries its own `description` from Payload's
  `admin: { description }` — the per-field instruction for filling that field in — and
  `--block <slug>` lists `documented_fields[]`.
- The same mechanism documents ordinary collection and global fields, published as
  `field_docs[PATH]` with `documented_paths`, `field_docs_file` and `field_docs_source`
  (and `field_doc` on `--field PATH`). It is a separate map rather than three more keys on
  every field entry: §7.8.2's fixed 26-key field entry is unchanged, and adding them there
  would have cost ~16 KB on `pay describe pages` to publish `null` 90 times.
- Provenance is tri-state throughout. Payload publishes a block's labels nowhere and
  defines **no** description field for a block at all (a `Block`'s `admin` accepts only
  components/custom/disableBlockName/group/images/jsx), so the text is read from the
  block's `config.ts` — `labels: { singular, plural }` plus `custom.description`,
  `custom.docs` or `custom.summary` — and `description_key` always names the key that
  answered. A project gets this only if its authors wrote it: absent means `null` with
  source `"unknown"` and a `docs_reason`, never a title-cased guess at the slug. A plugin's
  blocks live in `node_modules`, which is never scanned, so theirs are always `null`.
- The §7.10 scan now also reads `**/collections/**` and `**/globals/**`, for field
  documentation only. A collection's slug is never admitted to the block vocabulary — a
  `CollectionConfig` and a `Block` are the same object literal to a byte scanner — and only
  an entity's top-level `fields:` array is read, so a field nested in a group, an array or
  a tab is reported as undocumented rather than given a guessed path.

## [0.1.0] — 2026-09-17

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

[Unreleased]: https://github.com/KLIXPERT-io/pay-cli/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/KLIXPERT-io/pay-cli/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/KLIXPERT-io/pay-cli/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/KLIXPERT-io/pay-cli/releases/tag/v0.1.0
