# Safe writes: prove it, back it up, undo it

Four tools replace "read it back and eyeball it", `diff <(jq -S …)`, hand-made backup
copies and publishing just to look:

1. every `--dry-run` of a write to an existing document shows `data.diff`;
2. `pay diff` compares a write body, a draft, two documents or two files;
3. automatic backups before every destructive write, restorable with one command;
4. explicit draft/publish intent, so a write never publishes or unpublishes by accident.

---

## 1. `--dry-run` shows what changes

```bash
pay update pages 16 --data-file page.json --dry-run      # data.diff, base "latest" (the newest version)
pay publish pages 16 --dry-run                           # base "published": what visitors will see change
pay unpublish pages 16 --dry-run --yes
pay update pages --where 'slug like old-' --set-json 'meta={"title":"x"}' --dry-run   # first 5 sampled
pay globals update footer --data-file footer.json --dry-run
pay backups restore <file> --dry-run
pay versions restore pages 687 --dry-run
```

In a pipe: `… | pay apply --draft --dry-run`.

`data.diff` = `{base, id, status, updated_at, identical, summary:{total, added, removed,
changed, moved, rich_text, fields}, changes:[{op, path, from, to, …}]}` (bulk:
`{sampled, of, truncated, base, documents:[…]}`).

* `summary.fields` = the top-level fields the write touches; `changes[].path` proves it
  only touched one block (`layout[2].blocks[1].title`). `identical: true` = the write
  changes nothing.
* When the live row differs from the newest version (a pending draft), a non-draft
  write's preview also has `data.diff.published`: what the LIVE page will change by.
* `diff_unavailable`: the id does not exist (the real write would fail).
* `pay versions restore`: restoring a DRAFT version of a published document without
  `--draft` would unpublish it, so it is refused (`invalid_args`, dry run too). Pass
  `--draft` (a new draft on top; live page untouched) or `--unpublish` (take it offline
  deliberately).

---

## 2. `pay diff`

```bash
pay diff pages 2 --data-file page-2.json                # what would this write change? (nothing written)
pay diff pages 2 --set title='Neuer Titel'
pay diff pages 47 --draft-vs-published                  # what publishing would take live
pay diff pages 50 58 --draft                            # two documents' newest drafts
pay diff --global header --data-file header.json
pay diff --left before.json --right after.json          # two files or envelopes, offline
pay diff pages 2 --data-file page-2.json --path .summary.fields
pay diff pages 2 --data-file page-2.json --output table
pay diff pages 2 --data-file page-2.json --ignore meta.image --ignore 'layout[].blockName'
```

`op_result` `{left, right, identical, summary, changes}`; **exit 0 whether or not they
differ** — branch on `data.identical`. Rows are matched by `id` (moves are reported as
`move`), a changed `blockType` in the same slot is ONE change, rich text is compared by
structure and shown as `from_text`/`to_text` (`--full` for the editor JSON).
`createdAt`/`updatedAt` are ignored unless `--include-timestamps`.

The body form applies **Payload's PATCH rules to the newest draft**: omitted keys keep
their value, groups merge key by key, arrays/blocks are replaced — rows you omit are
DELETED, a row whose `id` matches keeps its unsent keys, a row without an id is new. So a
partial body `{"layout":[…one row…]}` shows every other row as `remove`: that is what the
write would do.

Not predicted: server defaults of new rows, hook-computed fields (`publishedAt`, slug
formatting, breadcrumbs), `_status` from `--draft`/`--publish`. `field_not_on_document`
= you set a key the stored document does not have (usually a typo Payload would drop).
`never_published` on `--draft-vs-published` = there is no published side.

### The safe edit loop

```bash
pay get pages 2 --depth 0 --draft > page-2.json          # 1. export at depth 0
# 2. edit page-2.json (keep row ids; delete its "_status" key unless you mean to publish)
pay diff pages 2 --data-file page-2.json                 # 3. must list exactly your edits
pay update pages 2 --data-file page-2.json --draft       # 4. write the draft
pay diff pages 2 --draft-vs-published                    # 5. review before publishing
pay publish pages 2                                      # 6. take it live
```

For block edits prefer the pipe (`references/blocks.md`); for many documents prefer
`pay plan` / `pay sync` (`references/migration.md`).

---

## 3. Drafts vs live: say what you mean

Payload merges an update into the **newest version** and, without `?draft=true`, stores
the result as the live row — including that version's `_status`. That produces two traps
PayCLI now guards against.

**A pending draft on a published page.** A plain `pay update pages 16 --set slug=x` on a
published page with a pending draft would publish the draft's content AND set
`_status: "draft"`, taking the page offline; a body with `_status: "draft"` (an export of
the draft) would unpublish it. PayCLI **refuses** both (`invalid_args`, exit 5, the dry
run too) and asks for intent:

```bash
pay diff pages 16 --draft-vs-published                   # what the pending draft holds
pay update pages 16 --set slug=x --draft                 # edit the draft; the live page is untouched
pay update pages 16 --set slug=x --publish               # publish the draft together with the change
pay update pages 16 --set slug=x --unpublish             # take it offline, deliberately
pay globals update footer --data-file f.json --publish --yes   # globals have --publish/--unpublish too
```

`--publish` there warns `publishes_pending_draft`, and its dry run shows the live change
in `data.diff.published`. A real non-draft write reads `_status` first (two small GETs;
`--draft`/`--unpublish` skip them). Bulk forms: `pay update pages --where …` without
`--draft` is merged into the LIVE rows and supersedes pending drafts (its dry run warns
`pending_draft_superseded`) — use `--draft`, or write those documents one by one.
`pay publish pages --where …` publishes the newest drafts.

**A body with `_status: "published"`.** Exports of published pages carry it, and Payload
publishes such a body even with `?draft=true`. With `--draft`, PayCLI removes `_status`
and saves a draft (`body_publishes`, "--draft wins"); with no flag the write publishes and
warns `body_publishes` loudly; `--publish` makes it deliberate. `--draft --publish` is
`invalid_args`. Applies to `update`, `apply` (notably `--all-fields`) and
`globals update`. In `upsert`/`plan`/`sync` `_status` in a body is removed
(`body_status_ignored`) and only the `status` / `--draft` / `--publish` choice counts.

### Publish and unpublish exactly these ids

```bash
pay publish pages 38 39 40 --dry-run       # validates all; blocked ids appear as publish_blocked
pay publish pages 38 39 40                 # one PATCH per id; bulk_result
pay unpublish pages 38 39 40 --yes         # unpublish is L2: --yes in a non-TTY (else 11)
pay publish pages 38 39 40 --fail-fast
```

* Exit 7 = some succeeded (ALREADY COMMITTED) and some failed: run `next.cmd`, which
  lists only the ids a retry can fix; ids that do not exist are named in the hint.
* Exit 5 `validation_failed` = the required-field pre-check blocked EVERY id; nothing was
  sent. `error.failures[]` names the missing fields per id; `--no-pre-validate` lets the
  server decide.
* Ids cannot be combined with `--where`/`--max-docs`/`--all`.
* Required paths through arrays are checked per existing row; an empty optional array
  (`hero.links: []`) passes, as on the server.

---

## 4. Automatic backups

Turn them on once and stop hand-copying documents before risky writes:

```bash
export PAY_BACKUP_DIR=~/pay-backups                 # or in ./pay.toml: backup_dir = ".pay-backups"
pay update pages 16 --set title='New' --dry-run     # data.would_backup: the file it would write
pay update pages 16 --set title='New'               # meta.backups: ["…/<profile>/pages/16/…--update.json"]
pay backups list --collection pages --id 16
pay backups show <file> --path .doc.title
pay backups restore <file> --dry-run
pay backups restore <file> --yes
```

* Taken before every write that changes or removes an existing document: `update` (by id
  and bulk), `apply`, `upsert`/`sync` updates, `publish`/`unpublish`, `delete` (soft,
  `--permanent`, bulk), `restore`, `versions restore`, `globals update`,
  `upload --replace`, `backups restore`. Not for `create`, `duplicate`, `pay raw`, or
  upload file bytes.
* Layout: `<dir>/<profile>/<collection>/<id>/<UTC-timestamp>--<command>.json`, globals
  under `<dir>/<profile>/_globals/<slug>/`. Files 0600; the document at depth 0, the
  newest draft where drafts exist, in the write's locale. A published page with a pending
  draft keeps both (`.doc` = draft, `.published_doc` = live).
* A relative `backup_dir` in `pay.toml` means "next to pay.toml"; a relative
  `PAY_BACKUP_DIR` or `--backup-dir` means "relative to the current directory".
  `pay config explain --path .backup_dir` shows the resolved path and its source;
  `pay doctor --path .backups` shows whether it is writable.
* **A backup that cannot be taken aborts the write**: `backup_failed` (exit 1), nothing
  sent. Fix the directory, or pass `--no-backup` (or `PAY_NO_BACKUP=1`) deliberately.
* The audit log records the same paths (`pay audit tail`, key `backups`).

### Restoring

```bash
pay backups restore <file> --dry-run                  # the exact PATCH (+ data.steps) and the backup it takes itself
pay backups restore <file> --yes                      # L2: --yes in a non-TTY (else exit 11)
pay backups restore <file> --draft --yes              # put old content back as a draft only
pay backups restore <file> --publish --yes            # restore and publish
pay backups restore <file> --recreate --yes           # the document was permanently deleted (NEW id)
```

* A backup of a DRAFT is restored as a draft; over a published document it becomes a new
  draft on top (`restored_as_draft`), never an unpublish.
* A backup with `.published_doc` restores BOTH: the published state first, then the draft
  on top (`restore_two_steps`). Its dry run shows the live change in `data.diff.published`
  and the resulting draft in `data.diff`. `--draft` restores only the draft (`published_state_not_restored` warns if the
  page stays offline).
* `--recreate` changes the id (`backup_recreated_new_id` names old and new);
  relationships to the old id are not updated. A trashed backup is re-created in the trash
  (`backup_recreated_in_trash`).
* A backup from another profile or server is refused (`backup_target_mismatch`, exit 5);
  `--ignore-target` only for a deliberate copy between environments.
* Server-owned fields are never sent; credential values masked at backup time are listed
  in `backup_fields_not_restored`. The write that took the backup warned
  `backup_redacted` with the masked paths.
* A restore backs up what it overwrites first, so a restore is itself undoable.

### Housekeeping

```bash
pay backups list --since 7d --command delete          # what was saved before deletes this week
pay backups list --all-profiles -n 0                  # everything under the directory
pay backups prune --older-than 30d --dry-run
pay backups prune --older-than 30d --yes
```

---

## 5. What did I already change?

```bash
pay audit tail -n 20
pay audit tail --since 2h --collection pages --action update --phase post
pay audit tail --command sync --since 1d
pay audit tail --action restore_backup
```

Every write is recorded before (`phase: pre`) and after (`phase: post`) the request, with
its request id, target, backups and — for multi-request writes such as `--reset-nested` —
the `step`. `--no-audit` opts out deliberately; a failing pre-write audit record aborts
the write (`audit_write_failed`, exit 1).
