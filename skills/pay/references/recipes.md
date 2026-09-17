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

**`pay get` has no `--published-only`.** It fetches one document by id and the id is the
whole filter, so there is nothing to filter. A bare `pay get` therefore *can* hand back an
unpublished document: read `_status` on the document it returned. When you need "this id,
but only if it is live", ask `find` instead — it is a filter, so it can answer:

```bash
pay get pages 11 --path '._status'                         # "draft" or "published"
pay find pages --published-only --where 'id eq 11' --limit 1   # empty result = not live
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
pay update pages 16 --publish               # store and publish in one call
```

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
pay unpublish pages 17 --yes                         # destructive: takes content offline
pay duplicate pages 17                               # copy, as a draft by default
```

`pay publish` pre-validates against the collection's required fields **before** the network
call, so you get a precise list instead of an opaque 400.

## 9. Versions

```bash
pay versions list pages --id 11 --limit 5
pay versions get  pages <versionId> --depth 0
pay versions diff pages <versionA> <versionB>        # client-side JSON diff
pay versions restore pages <versionId> --dry-run
pay versions restore pages <versionId> --yes         # destructive: overwrites the document
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
pay cache info                                         # discovery cache state
pay discover --refresh                                 # re-probe after a schema change
pay config explain --output json                       # where every setting came from
```

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

## 13. Blocks: what goes inside one, then write it

A blocks field answers two different questions. **Which** blocks it accepts is a slug
list; **what is inside** one is that block type's own field schema.

```bash
# Which blockTypes does this field accept? (slugs, never interfaceNames)
pay describe pages --field layout --path .block_types
# → ["archive","content","cta","formBlock","mediaBlock"]

# What is inside one of them?
pay describe pages --block cta --path '.block.fields[].path'
# → ["richText","links","links.link","links.link.type","links.link.newTab",
#    "links.link.reference","links.link.url","links.link.label",
#    "links.link.appearance","links.id","id","blockName","blockType"]

# Only the fields that are proved required:
pay describe pages --block mediaBlock --path .required_fields[]
# → ["media"]       (an upload; .block.fields[].relation_to says it points at `media`)

# Everything at once for one field, when you really do want all of them:
pay describe pages --field layout --blocks-detail
```

Each entry of `.block.fields[]` carries `payload_type`, `json_type`, `has_many`,
`options` (enum values), `relation_to` + `write_shape` for relationships and uploads, and
`required` + `required_source`.

**`plumbing: true` marks Payload's own keys.** `blockType` is mandatory on every block you
write and must be the **slug**; `blockName` is an optional admin-UI label; `id` is
server-generated — omit it when creating. They are not content fields.

**`required: null` is not `false`.** Payload generates no input type for a block type, so
the only two proofs of required-ness are a GraphQL `NON_NULL` on the block's object type
and the block's own `config.ts` on disk. A plugin's blocks live in `node_modules`, which
is never scanned, so their optional fields stay unknown:

```bash
pay describe forms --block textarea --path '.block.fields'
# name          text     required: true   required_source: graphql          (NON_NULL)
# label,width,
# defaultValue,
# required      …        required: null   required_source: unknown
# id,blockName,blockType                   required_source: payload-protocol (plumbing)
```

`.block.required_unknown` lists those paths and `.block.reason` says why, and the command
raises a `block_required_unknown` warning so it cannot be missed.

### A real create, end to end

Everything below was run against a live project (`pages.layout`, `cta`). `richText` is a
lexical field: send the object, never a string.

```bash
cat > /tmp/cta.json <<'JSON'
[{"blockType":"cta","blockName":"Scratch CTA",
  "richText":{"root":{"type":"root","format":"","indent":0,"version":1,"direction":"ltr",
    "children":[{"type":"paragraph","format":"","indent":0,"version":1,"direction":"ltr",
      "textFormat":0,"children":[{"type":"text","detail":0,"format":0,"mode":"normal",
        "style":"","text":"Ready to ship?","version":1}]}]}},
  "links":[{"link":{"type":"custom","url":"/docs","label":"Read the docs",
                    "appearance":"default"}}]}]
JSON

# 1. Look first. Prints the exact body; changes nothing.
pay create pages --set title='PayCLI block scratch' --set slug=paycli-block-scratch \
  --set-json layout="$(cat /tmp/cta.json)" --dry-run

# 2. Do it.
pay create pages --set title='PayCLI block scratch' --set slug=paycli-block-scratch \
  --set-json layout="$(cat /tmp/cta.json)" --yes

# 3. Verify the block survived — Payload drops an unknown blockType with a 201.
pay get pages 35 --select id,title,layout

# 4. Clean up.
pay delete pages 35 --yes
```

Notes from that run:

* `--set-json` is required for a blocks field. `--set` refuses it (`write_shape: json`).
* The response echoed the block back with a server-generated `id` on the block row **and**
  on each `links` row. Those ids are plumbing; you never send them.
* `pages` has drafts, so the new document came back `_status: "draft"` with a
  `created_as_draft` warning naming the required fields still missing elsewhere in the
  document (`hero.links.link.label`). The `cta` block itself was stored in full.
* If `warnings[]` had contained `input_silently_dropped` for `layout`, the `blockType`
  would have been wrong — check it every time.
