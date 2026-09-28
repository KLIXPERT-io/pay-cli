# `pay` recipes

Copy-paste patterns for the things agents actually do. Slugs below come from a real
Payload project (`pages`, `posts`, `media`, `categories`, `users`, `crm-contacts`,
globals `header` and `footer`); run `pay explain --slim` to learn the slugs in yours.

## 1. Orient yourself in an unknown project

```bash
pay explain --slim                         # connection + every slug + exit codes, ~1 KB
pay explain                                # + ops, features, key fields, gotchas
pay collections --kind content --writable  # skip Payload's internal payload-* collections
pay describe pages --required-only         # what must be set to create one
pay describe pages --queryable             # what can appear in --where
pay can update pages                       # exit 0 = allowed, exit 8 = denied
pay doctor                                 # when anything is confusing
```

`pay explain` is cached and offline after the first run. `--section collections --offset 40`
pages through the detail when a project has more collections than fit in one response;
`data.collection_slugs` is never truncated.

## 2. Paginate without writing a loop by hand

```bash
pay find crm-contacts --limit 50
# → page.truncated: true, next.cmd: "pay find crm-contacts --limit 50 --page 2 --profile dev"
```

Run `next.cmd` verbatim until `page.truncated` is `false` (or `page.has_next_page` is
`false`). Do not reconstruct the command; the flags and profile are already baked in.

For everything at once:

```bash
pay find crm-contacts --all --max 5000 --output jsonl > contacts.jsonl 2> summary.json
```

`--output jsonl` writes one bare document per line on stdout and the envelope
(`page`, `next`, `meta`, `warnings`) as a single line on stderr — a clean data file and a
clean summary. `--limit 0` is rejected (exit 5): use `--all`.

Just the count, one cheap request:

```bash
pay count crm-contacts --where 'status eq active'
```

## 3. Read published content only

```bash
pay find posts --published-only --sort -publishedAt --limit 10
pay count posts --published-only
```

A bare read returns never-published documents with `_status: "draft"`; without
`--published-only` you will hand back unpublished content as if it were live. `pay` warns
with `mixed_status_results` when that happens.

A bare `pay get` *can* hand back an unpublished document. `pay get --published-only`
asks for "this id, but only its live state": it returns the published version even when a
newer draft exists, and fails with `not_published` (exit 4) when the document has none.

```bash
pay get pages 11 --published-only                     # the live state, or not_published (4)
pay get pages --slug pricing --published-only         # by slug
pay get pages 11 --path '._status'                    # "draft" or "published"
```

## 4. Create a document

```bash
# Everything valid up front:
pay create pages --set title='Pricing' --set slug=pricing

# Or a stub now, fill it in later (--draft skips required-field validation):
pay create pages --set title='Pricing' --draft

# Structured values:
pay create posts --set title='Launch' --set-json categories='[3]' \
                 --set-json meta='{"title":"Launch","description":"..."}'

# From a file, then override two fields (deep-merged, later wins):
pay create pages --data-file ./page.json --set slug=pricing --set-json layout='[]'
```

Check `warnings[]` afterwards: `input_silently_dropped` means Payload discarded part of
your body (an unknown field, or an unknown `blockType`) and still returned 201.

## 5. Update one document

```bash
pay update pages 16 --set title='Launch day'
pay update posts 1 --set-json meta='{"title":"SEO title"}'
pay update pages 16 --set title='Launch day' --draft   # store the change without publishing
pay update pages 16 --set title='Launch day' --publish # store and publish in one call
```

Add `--dry-run` first: its `data.diff` lists exactly what the write changes
(`references/safe-writes.md`). On a **published page with a pending draft** a write
without `--draft`, `--publish` or `--unpublish` is refused (`invalid_args`, exit 5), because
Payload would publish the draft's changes and take the page offline — pick one.
Markdown into a rich-text field: `--set-md content=@body.md` (`references/richtext.md`).

If the response is `validation_failed` naming fields you never sent, that is Payload
re-validating the **whole** document. Check `error.fields[].sent`: when every entry is
`false`, the stored document was already invalid. Repair those fields deliberately, or
retry with `--draft`.

## 6. Bulk-change many documents safely

```bash
# 1. Look first. Always. Exits 0, changes nothing.
pay update crm-contacts --where 'lifecycleStage eq lead' --set status=active --dry-run

# 2. Then do it, with an explicit cap and an explicit confirmation.
pay update crm-contacts --where 'lifecycleStage eq lead' --set status=active \
                        --max-docs 50 --yes

# 3. One document at a time, so one bad record cannot poison the batch:
pay update crm-contacts --where 'lifecycleStage eq lead' --set status=active \
                        --per-doc --max-docs 50 --yes
```

* `--max-docs N` (default 100) is the **only** blast-radius cap. Over it you get
  `bulk_limit_exceeded` (exit 5); `--all` lifts it.
* `--limit` on a bulk write is an **error** — it is page size and means nothing here.
* `pay` counts, resolves the exact ids client-side, and then writes them by id in chunks,
  so the blast radius is exactly what `--dry-run` printed.
* Exit 7 means some writes committed: retry only `next.args.ids`, never the whole command.

## 7. Delete, un-delete, and really delete

```bash
pay delete crm-contacts 224                   # soft delete when the collection has trash
pay find crm-contacts --trash --where 'id eq 224'   # see trashed documents
pay restore crm-contacts 224                  # un-trash
pay delete crm-contacts 224 --permanent --yes # the real DELETE (adds ?trash=true when already trashed)
pay delete pages --where 'slug contains tmp-' --dry-run   # preview a bulk delete
```

On a collection **without** trash, `pay delete` is irreversible: it warns on stderr and
requires `--yes` in a non-interactive shell (else exit 11).

## 8. Drafts and publishing

```bash
pay create pages --set title='Pricing' --draft       # _status: draft
pay find pages --draft-only                          # only unpublished
pay publish pages 17                                 # re-runs the validation --draft skipped
pay publish pages 17 18 19                           # several ids: one PATCH each, bulk_result
pay publish pages 17 --dry-run                       # data.diff (base "published"): what goes live
pay diff pages 17 --draft-vs-published               # the same question, without a write verb
pay unpublish pages 17 --yes                         # destructive: takes content offline
pay unpublish pages 17 18 --yes
pay duplicate pages 17                               # copy, as a draft by default
```

`pay publish` pre-validates against the collection's required fields **before** the network
call, so you get a precise list instead of an opaque 400 (rows of an empty optional array
are not required). With several ids, exit 7 means some were published and are committed:
run `next.cmd`, which retries only the ids a retry can fix. To look at a draft on the site
without publishing it, use `pay url pages 17 --preview` (`references/migration.md` §6).

## 9. Versions

```bash
pay versions list pages --id 11 --limit 5
pay versions get  pages <versionId> --depth 0
pay versions diff pages <versionA> <versionB>        # rows by id, rich text as text (--output table)
pay versions restore pages <versionId> --dry-run
pay versions restore pages <versionId> --yes         # destructive: overwrites the document
pay versions restore pages <versionId> --draft --yes # a draft version over a published page: new draft on top
pay versions list --global <slug>                    # globals have versions too
```

Only collections and globals with versions enabled have these; `pay collections` and
`pay globals list` report `versions`, and asking for versions where they are off fails
locally with `feature_unavailable` (exit 10) instead of a 500. The `id` of a version record
is NOT the document id — `parent` is; feed `id` to `get`, `diff` and `restore`.

## 10. Uploads, downloads and globals

```bash
pay upload media ./hero.png --alt 'Hero image'
pay upload media ./hero.png --replace 4                 # overwrite an existing file
pay upload media - --filename hero.png --content-type image/png < ./hero.png
pay download media 4 -o ./hero.png
pay download media 4 --size thumbnail -o ./thumb.png

pay globals list
pay globals get    header --depth 1
pay globals update header --set-json navItems='[]' --yes   # globals update is destructive: L2
```

`--file` on a collection that is not an upload collection fails locally with
`not_upload_collection` (exit 5) rather than a server 500.

## 11. Escape hatches and provenance

```bash
pay raw GET pages --query limit=1 --query depth=0      # anything the CLI does not model
pay raw PATCH pages/16 --data '{"title":"x"}'
pay audit tail -n 20 --action delete                   # what have I already changed?
pay audit tail --since 2h --collection pages --phase post   # filters combine
pay cache info                                         # discovery cache state
pay discover --refresh                                 # re-probe after a schema change
pay config explain --output json                       # where every setting came from
pay doctor --path .checks                              # every check, incl. binary_on_path
```

`pay doctor` also answers "why does `pay` not run in the agent's shell?": the
`binary_on_path` check warns when `$PATH` does not reach this binary (non-interactive
shells skip `~/.bashrc`) and its hint prints the `ln -sf` fix. It also reports GraphQL
introspection (and what its absence degrades), the block census, the backup directory,
and the URL/preview configuration.

`pay raw` still returns the standard envelope (`data_kind: "raw"`), so error handling is
unchanged.

**Mind the leading slash.** A path *without* a leading slash is resolved under the
project's discovered `api_path` (`pages` → `/api/pages`). A path *with* a leading slash is
sent **verbatim**, which is how you reach a non-API route (`pay raw GET /admin`). So
`pay raw GET /pages` asks the Next.js app for the page `/pages`, gets HTML back and fails
with `non_json_response` (exit 6). Write `pages`, or `/api/pages` in full.

## 12. Shell composition

```bash
# ids only, one per line
pay find posts --published-only --output id | while read -r id; do
  pay get posts "$id" --select title,slug
done

# a CSV for a human: the columns are the collection's discovered top-level scalar
# fields, in schema order. There is no --columns flag; use --select to choose them.
pay find crm-contacts --limit 100 --select name,email,status --output csv > contacts.csv

# branch on the exit code, not on any message
if pay get pages 999 > out.json; then echo ok; else
  case $? in
    4)  echo "no such document" ;;
    2)  echo "fix credentials" ;;
    8)  echo "not permitted" ;;
    7)  echo "PARTIAL - do not retry" ;;
    *)  jq -r '.error.code + ": " + .error.hint' out.json ;;
  esac
fi
```

## 13. Blocks: create a page with blocks

Learn the shapes from the live site (works without GraphQL introspection), build rows
from real ones, validate, then write. Everything about blocks is in
`references/blocks.md`; this is the short path for a NEW document.

```bash
pay blocks types pages                                     # which types, where, how often
pay blocks schema cta --collection pages                   # what is inside one
pay blocks new cta --collection pages --set-md richText=@cta.md --path .row --output raw > cta.json
pay blocks new mediaBlock --collection pages --set media=7 --path .row --output raw > media.json
jq -s '{title: "Pricing", slug: "pricing", layout: .}' cta.json media.json > page.json
pay blocks validate --data-file page.json --collection pages     # exit 5 on real errors
pay create pages --data-file page.json --draft --dry-run
pay create pages --data-file page.json --draft
pay outline pages --slug pricing --draft --output table          # check the structure
```

Check `warnings[]` after the write: `input_silently_dropped` for `layout` means a
`blockType` or key was wrong and Payload discarded it with a 201. `created_as_draft`
names the required fields still missing before the page can be published.

When the project's schema is declared (GraphQL introspection on, or the project source
readable), `pay describe` answers too:

```bash
pay describe pages --field layout --path .block_types     # the writable slugs (never interfaceNames)
pay describe pages --path .block_docs                     # label + description per slug, if the authors wrote them
pay describe pages --block cta --path '.block.fields[].path'
pay describe pages --block mediaBlock --path .required_fields[]
pay describe pages --field layout --blocks-detail         # every block's fields at once
```

`required: null` inside a block means unknown, never "optional"; `.block.required_unknown`
lists those paths (`block_required_unknown` warns). `id`, `blockName` and `blockType` are
plumbing: `blockType` is mandatory on every row, `id` is server-generated — omit it.

## 14. Editing an existing document in a pipe

Never `pay get > file`, edit with jq, `--set-json layout=…` back: the anchors shift, a
copied row keeps its id and overwrites the original, a depth-1 read writes whole documents
into relationships, and the whole document (with `_status`) is PATCHed. Use the pipe —
full reference in `references/blocks.md` §5:

```bash
pay get pages 12 --draft --depth 0 | pay blocks ls --recursive
pay get pages 12 --draft --depth 0 | pay blocks mv type:cta --after type:mediaBlock | pay apply --draft --dry-run
pay get pages 12 --draft --depth 0 \
  | pay blocks rm type:content --all \
  | pay blocks add mediaBlock --template --first \
  | pay blocks set type:cta[0] --set-md richText=@cta.md \
  | pay apply --draft --yes
pay globals get header --depth 0 | pay blocks mv id:6a1f7960 --first --field navItems | pay apply --yes
```

`pay apply` writes only the top-level fields the stages edited (`edits.fields`); `--draft`
belongs on both ends of the pipe; read at `--depth 0`. Nested rows are addressed with
`--field 'layout[id:…].blocks'` (copy it from `pay blocks ls --recursive` or
`pay outline`).
