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

A blocks field answers three different questions. **Which** blocks it accepts is a slug
list; **what each one is for** is its label and description; **what is inside** one is that
block type's own field schema.

```bash
# Which blockTypes does this field accept? (slugs, never interfaceNames)
pay describe pages --field layout --path .block_types
# → ["archive","content","cta","formBlock","mediaBlock"]

# Which one do I actually want? (printed beside every slug list)
pay describe pages --path .block_docs
# → {"cta": {"label":"Call to Action",
#            "description":"A prompt with rich text and one or more buttons, used to push
#                           the reader to a next step such as contact or signup.",
#            "description_key":"custom.description","description_source":"project-source",
#            "fields_count":9,"docs_reason":""}, …}

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
`options` (enum values), `relation_to` + `write_shape` for relationships and uploads,
`required` + `required_source`, and `description` — the field's own instruction from
Payload's `admin: { description }`:

```bash
pay describe pages --block mediaBlock --path '.block.fields[0]'
# → {"name":"media", …, "required":true,
#     "description":"The image or video to display. Pass a media document id.",
#     "description_source":"project-source","description_key":"admin.description"}

pay describe pages --block mediaBlock --path .documented_fields
# → ["media"]        (every field of this block that carries an instruction)
```

**Labels and descriptions are the project's own words, not API data.** They are read from
the block's `config.ts` on disk: `labels: { singular, plural }`, and a description under
`custom.description` / `custom.docs` / `custom.summary` — `description_key` always says
which key answered, because Payload defines **no** description field for a block at all
(a `Block`'s `admin` accepts only components/custom/disableBlockName/group/images/jsx).
A project gets any of this only if its authors wrote it; `null` means nobody did, never
that `pay` failed, and `docs_reason` says which:

```bash
pay describe forms --block textarea --path .docs
# → {"slug":"textarea","label":null,"label_plural":null,"labels_source":"unknown",
#    "description":null,"description_source":"unknown","description_key":"",
#    "fields_count":5,
#    "docs_reason":"blockType \"textarea\" has no config on disk to read them from —
#                   normal for a plugin-provided block, which lives in node_modules —
#                   and the API publishes neither"}
```

The same `admin: { description }` documents ordinary collection fields, published as
`.field_docs[PATH]` so the field entries stay at their fixed 26 keys:

```bash
pay describe pages --path .field_docs            # {PATH: {description, key, source}}
pay describe pages --path .documented_paths      # just the documented paths
pay describe pages --field title --path .field_doc
```

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

---

## 14. Editing a document in a pipe (blocks *and* array fields)

§13 builds a blocks field from nothing. This is the other half: changing what is already
there — the edit content people actually make, over and over. "Move the CTA under the media
block." "Drop that old banner." "Reorder the nav."

There is no per-row endpoint in Payload: the only way to move one row is to replace the
whole array. **Do not do that by hand.** `pay get > file`, edit with jq, `--set-json` back
is three commands and four ways to be silently wrong. Use the pipeline.

Despite the name, `pay blocks` is not blocks-only. It edits **any array of objects** in the
piped document: a `blocks` field, a plain Payload `array` field, at the top level or at a
dotted path, on a collection document or on a global. §14.7 covers the array case; the
differences are small and all about addressing.

### The shape

```bash
pay get pages 12 --depth 0 | pay blocks mv type:cta --after type:mediaBlock | pay apply --yes
```

Each `pay blocks` verb reads ONE document from stdin, edits it, and writes the document to
stdout. Nothing touches the network until `pay apply`. So stages compose, in any order and
any number, and you can stop at any point to look.

### Look first

```bash
pay get pages 12 --depth 0 | pay blocks ls
```

```jsonc
{"field":"layout","count":3,"accepts":["archive","content","cta","formBlock","mediaBlock"],
 "rows":[
   {"index":0,"id":"67f3a1","block_type":"cta","block_name":"Top CTA",
    "selector":"id:67f3a1","fields":["richText","links"]},
   {"index":1,"id":"67f3b2","block_type":"content","selector":"id:67f3b2","fields":["columns"]},
   {"index":2,"id":"67f3c3","block_type":"mediaBlock","selector":"id:67f3c3","fields":["media"]}]}
```

**Use `.selector`, not `.index`.** It is the shortest string that addresses that row and no
other, and it is an `id:` whenever the row has one. An index is stale the moment another
stage in the pipe inserts or removes a row.

`ls` ends a pipe — its `data_kind` is `op_result`, a listing, not a document you can apply.
Put it at the END of a pipe to preview an edit: `… | pay blocks mv … | pay blocks ls`.

### The selector grammar

Closed — eight forms, no wildcards:

```
3            index 3 (0-based)
-1           the last row; -2 the second to last
first, last  sugar for 0 and -1
id:67f3a1    the row with that id — the only handle stable across edits
name:Hero    blockName, matched exactly (never a substring)
type:cta     EVERY row of that blockType
type:cta[1]  the second cta row — always exactly one
```

A selector that matches several rows where the verb needs one is `selector_ambiguous`
(exit 5), listing every match ready to paste. A selector that matches none is
`selector_no_match` (exit **4**) with the rows that do exist in the hint.

### The verbs

```bash
D='pay get pages 12 --depth 0'

# move — --before/--after take a SELECTOR, so they survive the next stage
$D | pay blocks mv type:cta --after type:mediaBlock | pay apply --yes
$D | pay blocks mv last --first                     | pay apply --yes
$D | pay blocks mv id:67f3a1 --at 2                 | pay apply --yes

# remove — several selectors are resolved together, so they cannot shift under each other
$D | pay blocks rm id:67f3a1                  | pay apply --yes
$D | pay blocks rm id:67f3a1 id:67f3b2        | pay apply --yes
$D | pay blocks rm type:content --all         | pay apply --yes   # --all means every match

# add — appended unless a destination says otherwise; blockType is checked locally
$D | pay blocks add mediaBlock --after type:cta                    | pay apply --yes
$D | pay blocks add cta --set-json richText="$(cat /tmp/rt.json)" --first | pay apply --yes
$D | pay blocks add cta --data @/tmp/cta-row.json --last           | pay apply --yes

# duplicate — the copy is stripped of every id, at every depth
$D | pay blocks cp type:cta[0] --after type:cta[0] | pay apply --yes

# set fields INSIDE one row — the key is a path into the ROW, not into the document
$D | pay blocks set id:67f3a1 --set blockName='Hero CTA'            | pay apply --yes
$D | pay blocks set type:mediaBlock --set media=7                   | pay apply --yes
$D | pay blocks set type:cta[0] --set-json richText="$(cat rt.json)" | pay apply --yes
$D | pay blocks set last --unset blockName                          | pay apply --yes
```

Several edits, one write:

```bash
pay get pages 12 --depth 0 \
  | pay blocks rm type:content --all \
  | pay blocks add mediaBlock --first \
  | pay blocks mv type:cta --last \
  | pay apply --dry-run          # then --yes
```

`--dry-run` prints the exact `PATCH` it would send, and `edits.ops` narrates every stage:

```jsonc
"edits":{"fields":["layout"],
 "ops":[{"command":"blocks rm","field":"layout","detail":"removed 1 row(s): id:67f3b2 (content)","rows":2},
        {"command":"blocks add","field":"layout","detail":"added one mediaBlock row at index 0 (first)","rows":3},
        {"command":"blocks mv","field":"layout","detail":"moved id:67f3a1 (cta) from index 1 to index 2 (last)","rows":3}]}
```

### Why `pay apply` rather than `pay update --data @-`

`apply` sends **only the fields the pipeline touched** — that is what `edits.fields` is
for. A document read from Payload also carries `createdAt`, `updatedAt`, `_status` and
every other field; PATCHing all of it back is how a one-block reorder also republishes the
page. The collection and id come from the piped envelope's `target`, so you retype nothing.

```jsonc
// the whole PATCH body for the pipeline above
{"layout":[ …three rows… ]}
```

`--all-fields` opts out (and warns) for a document you edited by hand. `--field PATH`
narrows further. An envelope no transform touched is `no_edits` (exit 5), never a silent
fall back to writing everything.

`pay update … --data @-` still works and now unwraps a piped envelope instead of posting
`{"ok":true,"data":…}` as the body — but it sends the **whole** document. Prefer `apply`.

### Four things this stops you getting wrong

All verified against a live Payload 3.x project.

1. **The anchor shifts.** "Move row 0 after row 3" is an insert at index **2** of the
   remaining three rows, not at 4. Every hand-written reorder gets this wrong once.
   `mv` resolves the anchor before the removal and recomputes after it.
2. **A copy that keeps its `id` is not a copy.** Payload matches a row by id, so the
   "duplicate" overwrites its original and the array **loses** an entry. `cp` and `add`
   strip every `id` at every depth.
3. **An unknown `blockType` vanishes with a 201.** Payload drops the row and reports
   success. `pay blocks add ctaa` fails locally with `block_type_unknown` (exit 10) and
   `did_you_mean: ["cta"]` — when the project has been discovered. Without a schema it
   passes through, with a `local_validation_skipped` warning.
4. **`--depth 0` on the read.** Above it a relationship comes back as a whole document and
   is written back that way. The pipeline warns `populated_relationship` naming
   `layout[2].media`; re-read at depth 0 rather than applying.

And one about drafts: a read **without** `--draft` returns the *published* document.
`pay get … | … | pay apply --draft` would then save published content as a new draft and
discard the real one. Use `--draft` on both ends, or on neither.

### 14.7 The same pipeline on a plain `array` field

A Payload `array` field is rows of objects with no `blockType`. Everything above works on
one, with **two differences**, both about addressing.

**1. `--field` is always required.** Auto-detection only ever picks an array whose rows
*all* carry a `blockType`, because choosing a plain array for you would edit a field you
never named. Without it you get `field_ambiguous` (exit 5) — note the message says "no
blocks field could be identified", which is literally true and still the right fix:

```bash
pay globals get header --depth 0 | pay blocks ls
# → field_ambiguous (exit 5): no blocks field could be identified in the piped document
#   hint: name it with --field <path>
```

**2. `type:` and `name:` selectors do not apply.** They read `blockType` and `blockName`,
which array rows do not have. Address rows by `id:` or by index; `pay blocks ls` falls back
to `id:` automatically:

```bash
pay globals get header --depth 0 | pay blocks ls --field navItems
```

```jsonc
{"field":"navItems","count":3,
 "rows":[{"index":0,"id":"…795e","selector":"id:…795e","fields":["link"]},
         {"index":1,"id":"…795f","selector":"id:…795f","fields":["link"]},
         {"index":2,"id":"…7960","selector":"id:…7960","fields":["link"]}]}
```

There is no `accepts` and no `block_type`: there are no block types here. Otherwise it is
the same family, and `pay apply` writes the same single field:

```bash
pay globals get header --depth 0 \
  | pay blocks mv id:…7960 --first --field navItems \
  | pay blocks set 0 --set link.label='Plans' --field navItems \
  | pay apply --dry-run
```

```
POST http://localhost:3900/api/globals/header?depth=0
body keys ['navItems']   labels ['Plans', 'Home', 'Docs']
  moved id:…7960 from index 2 to index 0 (first)
  set link.label on id:…7960
```

Note `--set link.label=Plans`: the key is a **dotted path inside the row**, so a group
nested in an array row is reachable without `--set-json`. A global is written with `POST
/api/globals/{slug}` and keeps `globals update`'s L2 risk level — going through a pipeline
does not make it cheaper to confirm.

`--field` also takes a dotted path, for a blocks or array field nested in a group or tab:

```bash
pay get pages 12 --depth 0 | pay blocks mv type:cta --last --field hero.items
# edits.fields → ["hero.items"], and that is the only key `pay apply` sends
```

### 14.8 Feeding an envelope to the ordinary write verbs

`pay get … | pay update … --data @-` works, and now unwraps the envelope: it used to send
`{"ok":true,"data":{…},"meta":{…}}` as the request body, which Payload accepts with a 2xx
and silently drops every key of — the write looked fine and changed nothing.

```bash
pay get pages 12 --depth 0 | pay update pages 12 --data @- --dry-run
# body: the document itself; warnings[]: envelope_unwrapped
```

Detection needs `ok` **and** `v` **and** `data_kind` **and** `meta` together, so a
collection with a boolean `ok` field is never mistaken for an envelope. An **error**
envelope fails with the *upstream's* code rather than posting its `error` object.

Prefer `pay apply` anyway: `--data @-` sends the **whole** document (including `_status`
and `createdAt`), `apply` sends only the fields the pipeline recorded.

### 14.9 Without a project

Every `pay blocks` verb is local. No profile, no credential, no cache:

```bash
cat page.json | pay blocks mv type:cta --first > page.edited.json
cat page.json | pay blocks ls --field hero.items
```

`--field` is needed when the collection has more than one blocks field, when the document
alone is ambiguous, or on any plain `array` field (§14.7). Otherwise PayCLI picks the one
blocks field and says so with a `blocks_field_inferred` warning when it had no schema to
confirm it against.
