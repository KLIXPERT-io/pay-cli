# Working with blocks

A page in Payload is usually a `layout` field holding **block rows**: objects with a
`blockType`, an `id`, an optional `blockName` and the block's own fields. Rows nest: a
`group` row has its own `blocks`, a `grid` inside it has `blocks` again, a `columns` row
has `columns[].blocks`. This file covers learning which blocks exist, reading a page's
structure, editing rows at any depth, validating, and the orphan-row trap of nested
restructures.

Everything here works with GraphQL introspection **disabled** (the normal production
setup), because it learns from the live documents. Do not read the app's source code to
learn block shapes.

---

## 1. Learn which blocks exist: the block census

```bash
pay blocks types pages                             # every blocks field of pages: types + counts
pay blocks types pages --path .block_types         # just the slugs
pay blocks types pages --field 'layout[].blocks'   # one level down (inside group, …)
pay blocks types pages --field group.blocks        # what real group rows contain
pay blocks types                                   # every collection/global that uses blocks
```

The census reads real documents (`depth=0`, `draft=true` where drafts exist, up to 500
per collection) and records, per **field path pattern** (`layout`, `layout[].blocks`,
`layout[].blocks[].blocks`, and per parent type such as `group.blocks`), the block types
seen there with counts, and per block type every field with its kind, JSON types,
presence, observed values and a reference to its most complete real instance.

* `fields[].authoritative: false` = the list is what documents contain today, not
  everything the field allows. `true` only when a declared source enumerates it.
* `sources` on each type: `observed-census` (real documents), `graphql`,
  `project-source`, `configured`, `census-matched` (a GraphQL member joined to the slug
  documents actually use, e.g. `CallToActionBlock` → `cta`).
* Cost: the first run reads a few requests per collection (`data.census.requests`,
  `meta.http_requests`); after that it is answered from the cache for 24 h. A
  `census_truncated` warning means the budget cut the read short — raise it with
  `--census-max-docs 5000` (config `[defaults] census_max_docs`, env
  `PAY_CENSUS_MAX_DOCS`). `census_scope_wide` means a verb without `--collection` read
  several entities; name the collection to read one.

```bash
pay blocks learn                                   # rebuild after a deploy that changed blocks
pay blocks learn pages --path '.changes[].message' # "grid gained splitDesktop", new block types …
pay blocks learn pages --census-max-docs 5000
```

`--refresh` rebuilds a census for any command; `--no-cache` keeps it in memory only.

## 2. Learn what is inside one block

```bash
pay blocks schema grid --collection pages
pay blocks schema hero --collection pages --path .schema.likely_required   # every real row fills these with varied content
pay blocks example grid --collection pages --n 2                           # real rows, ids stripped
```

* Field paths are row-relative: `section.contentWidth` (group), `buttons[].label` (array
  rows).
* `values` + `enum_like: true` = a select-like vocabulary (identifier strings only; URLs,
  colours, numbers and e-mails are never treated as one).
* `required` is `"true"` only when declared (GraphQL `NON_NULL` / project source),
  `"likely"` when every one of ≥ 3 observed rows fills it in with author-varied content
  (rich text, a relationship/upload, free text), `"unknown"` otherwise — never read
  "unknown" as "optional". Select-like, boolean, constant and few-valued number fields stay
  `"unknown"` even at `presence_pct: 100`: their `defaultValue` fills them on every row.
  `pay blocks new` fills them for you.
* `kind`: `blocks` | `array` | `richText` | `group` | relationship/upload id | `scalar`.
* Pass `--collection`: a slug is not unique across collections (`forms` has a form-builder
  `text` block, `pages` a content `text` block); without it, differing shapes are not
  merged and `block_type_shapes_differ` names the other collection.

## 3. Build a new row

```bash
pay blocks new hero --collection pages --set title='Neue Seite' --path .row --output raw > hero.json
pay blocks new text --collection pages --set heading=Ablauf --set-md content=@ablauf.md
pay blocks new grid --collection pages --from-example     # start from the richest real row
```

`new` fills every field real rows carry: the most common value for select-like, boolean
and constant fields (listed in `defaults_from_values` — change them if wrong), Payload's
empty rich-text value, `[]` for nested blocks and arrays, `null` otherwise. It never
copies a link, path, colour or e-mail from another row. `fill_in` lists fields you still
must fill. `--set` takes dotted keys (`--set section.contentWidth=inhalt`); use
`--set-json` for arrays/objects and `--set-md` for rich text.

In a pipe, `pay blocks add <type> --template` does the same thing in place:

```bash
pay get pages 47 --draft --depth 0 \
  | pay blocks add hero --template --set title=X --first \
  | pay apply --draft --dry-run
```

---

## 4. Read one document's structure

```bash
pay outline pages 47 --draft                         # JSON: one entry per row at every depth
pay outline pages --slug home --draft --output table # an indented tree for reading
pay outline --global footer
pay outline pages 47 --draft --path .counts          # rows, blocks, by_type, flagged
pay outline pages 47 --draft --path '.rows[].field'  # every --field value
pay get pages 47 --draft --depth 0 | pay blocks ls --recursive
```

Each outline row: `path` (positional, `layout[2].blocks[1]`), `field` + `selector` (paste
into `pay blocks … --field`), `block_type`, `summary` (heading/title-like field or the
start of its rich text), `children`, `nested` (arrays inside the row with row counts — an
empty one is where `blocks add` goes) and `flags`:

* `empty_row` — a `{}` row: an orphan left by a nested block restructure (§8).
* `no_block_type` — a row that lost its `blockType` in a blocks array.
* `unknown_type` — a type the cached census/schema does not know (outline never builds a
  census; `pay blocks learn <collection>` does).

`pay blocks ls --recursive` gives the same rows with `depth`, `path`, `field`, `selector`,
`nested{}` and `arrays{}` (`kind: blocks|array`, and `accepts`/`accepts_source`/
`accepts_authoritative` when known). Read with `--draft` when you will write with
`pay apply --draft`.

---

## 5. Edit rows: the pipe

Payload has no per-row endpoint; the only way to change one row is to replace the whole
top-level array. **Do not do that by hand** (`jq` + `--set-json layout=…` gets anchors,
ids and relationships wrong). Use the pipe: every `pay blocks` edit verb reads ONE document
on stdin and writes the edited document to stdout; nothing touches the network until
`pay apply`.

```bash
pay get pages 12 --draft --depth 0 | pay blocks mv type:cta --after type:mediaBlock | pay apply --draft --yes
```

`pay apply` sends **only the top-level fields the pipeline edited** (`edits.fields`, e.g.
`["layout"]`) — never `_status`, `createdAt` or unrelated fields. `--dry-run` prints the
exact PATCH plus `data.diff`; `edits.ops[]` narrates every stage. `pay apply --field` can
only narrow the write; `--all-fields` widens it deliberately (warns `apply_all_fields`).
An envelope no stage touched is `no_edits` (exit 5).

### Selectors

```
3            index 3 (0-based)          -1  the last row; -2 the second to last
first, last  sugar for 0 and -1
id:67f3a1    the row with that id — the only handle stable across stages
name:Hero    blockName, matched exactly
type:cta     EVERY row of that blockType
type:cta[1]  the second cta row — always exactly one
```

`selector_ambiguous` (exit 5) lists every match ready to paste; `selector_no_match`
(exit 4) lists the rows that exist. `pay blocks ls` prints the shortest unambiguous
selector for every row.

### `--field`: any depth

| form | example |
|---|---|
| top level (default: the only blocks field) | `layout` |
| dotted index | `layout.2.blocks` |
| bracketed index | `layout[2].blocks` |
| by id (stable across edits — prefer this) | `layout[id:6ab8ed4df8659c00010b9cd2].blocks` |
| by type (unique on the page) | `layout[type:group].blocks` |
| n-th of a type | `layout[type:group[1]].blocks` (also `layout.type:group[1].blocks`) |
| deeper | `layout[type:group[1]].blocks[type:grid].blocks[0].buttons` |
| a group field | `hero.links` |

Quote bracketed paths in the shell. `layout.blocks` is wrong — layout is an array; the
error's `did_you_mean` lists every `layout[id:…].blocks` that exists. A path that does not
resolve is an error, never an empty list: a missing parent row is `selector_no_match`
(4), a typo'd key `unknown_field` (5, with did_you_mean), a key that is `null` on the row
`invalid_args` (5).

### The verbs

```bash
D='pay get pages 12 --draft --depth 0'

$D | pay blocks mv type:cta --after type:mediaBlock   | pay apply --draft --yes   # move
$D | pay blocks mv last --first --field 'layout[type:group].blocks' | pay apply --draft --yes
$D | pay blocks rm id:67f3a1 id:67f3b2                | pay apply --draft --yes   # remove (resolved together)
$D | pay blocks rm type:content --all                 | pay apply --draft --yes   # every match
$D | pay blocks add mediaBlock --after type:cta       | pay apply --draft --yes   # add (appended by default)
$D | pay blocks add cta --data @cta-row.json --last   | pay apply --draft --yes   # a prepared row (never @-)
$D | pay blocks add processStep --set phase=Plan --after first --field 'layout[type:group].blocks' | pay apply --draft --yes
$D | pay blocks cp type:cta[0] --after type:cta[0]    | pay apply --draft --yes   # duplicate, ids stripped
$D | pay blocks set id:67f3a1 --set blockName='Hero CTA' | pay apply --draft --yes
$D | pay blocks set type:processStep[2] --set phase=Convert --field 'layout[type:group].blocks' | pay apply --draft --yes
$D | pay blocks set id:6aba… --set-md content=@card.md --field 'layout[id:6aba…].blocks' | pay apply --draft --dry-run
$D | pay blocks set id:… --set 'buttons[0].label=Jetzt starten' --field 'layout[type:group].blocks'
$D | pay blocks set last --unset blockName            | pay apply --draft --yes   # clears (null)
```

* `--set` keys are paths **inside the row**: `link.url` creates the `link` group if
  needed; `links.0.url` needs row 0 to exist; replace a whole array with
  `--set-json 'links=[…]'`. `--unset links[0]` is refused — remove rows with `rm --field`.
* `--unset KEY` **clears** the field (null; `[]` for an array; every sub-field null for a
  group) — it does not delete the key. Payload keeps the stored value of a key a row
  omits, so deleting it (`jq 'del(…)'`) is a silent no-op with a 2xx. `--unset blockType`
  / `--unset id` are refused.
* `add` and `cp` strip every row id at every depth (reusing an id would overwrite the
  original row); relationship/upload ids and rich-text content are kept.
* `add -` adds a row to a plain **array** field (buttons, links: rows without
  `blockType`). `add <slug>` on such a field stores the row without blockType and warns
  `block_type_omitted`.
* `add <slug>` refuses an unknown slug (`block_type_unknown`, exit 10) only when the
  accepted list is authoritative. With a cached census it warns right away instead:
  `unknown_block_type` (with `did you mean "bentoItem"?`) or `block_type_not_observed_here`
  (e.g. a `bentoItem` into a group's `blocks`). Same for `set --set blockType=…`.
* There is no verb that moves a row between two different arrays: `add … --data @row.json
  --field <target>` and `rm` it from the source in the same pipe.

Several edits, one write — use id-based fields so earlier stages cannot shift them:

```bash
pay get pages 2 --draft --depth 0 \
  | pay blocks mv type:group --first \
  | pay blocks rm first --field 'layout[id:6ab8ed4df8659c00010b9cd2].blocks' \
  | pay blocks set first --set phase=Neu --field 'layout[id:6ab8ed4df8659c00010b9cd2].blocks' \
  | pay apply --draft --dry-run
```

A `--field` that enters a row by POSITION after an earlier stage reordered that array
warns `stale_index`: the position names a different row than the listing you read.

### Plain array fields and globals

The pipe edits any array of objects, including globals and group sub-fields. A plain
`array` field always needs `--field`, and `type:`/`name:` selectors do not apply (rows
have no `blockType`/`blockName`; use `id:` or an index):

```bash
pay globals get header --depth 0 | pay blocks ls --field navItems
pay globals get header --depth 0 \
  | pay blocks mv id:6a1f7960 --first --field navItems \
  | pay blocks set 0 --set link.label=Plans --field navItems \
  | pay apply --dry-run
```

A global keeps `globals update`'s L2 risk: `pay apply --yes` in a non-TTY.

### Rules that keep the pipe correct

* **Read at `--depth 0`.** Above it a relationship comes back as a whole document and is
  written back as one; the pipe warns `populated_relationship` naming the path.
* **`--draft` on both ends, or on neither.** A read without `--draft` returns the
  published document; `pay apply --draft` would then save published content as a new
  draft and throw the real draft away.
* **Never feed a `--richtext md|text` read** into the pipe: `richtext_rendered` (exit 5).
* `pay get … | pay update … --data @-` also works (the envelope is unwrapped,
  `envelope_unwrapped`) but sends the **whole** document. Prefer `pay apply`.
* Every `pay blocks` verb is local: `cat page.json | pay blocks mv type:cta --first`
  needs no profile.

---

## 6. Validate before writing

```bash
pay get pages 47 --draft --depth 0 | pay blocks validate           # exit 0 clean, 5 on errors
pay blocks validate --data-file page.json --collection pages
pay blocks new text --collection pages --set heading=Hi | pay blocks validate   # checked where text is used
pay blocks validate --data-file row.json --collection pages --field 'layout[].blocks'
pay get pages 47 --draft --depth 0 | pay blocks add text --template --after first | pay blocks validate
```

`validate` ends a pipe (it prints a report: `{checked_rows, errors, warnings, issues[]}`,
each issue `{severity, code, path, block_type, message, hint, source}`).

| Issue | Severity | Means |
|---|---|---|
| `empty_row` | error | a `{}` row — an orphan; remove it with `pay blocks rm` or repair with `--reset-nested` |
| `missing_block_type` | error | a row in a blocks array without `blockType` |
| `invalid_row` | error | a row that is not an object |
| `required_field_missing` | error | a declared-required field (GraphQL / project source) is missing |
| `unknown_block_type` | error if the list is authoritative, else warning | never seen anywhere — Payload silently drops the row |
| `block_type_not_observed_here` | warning | seen elsewhere, never at this path (a nested row: never in its parent type's field, e.g. `group.blocks`) |
| `unknown_field` | warning | a key never seen on that type and absent from any schema — Payload silently drops it |
| `value_not_in_enum` | error for a GraphQL enum, else warning | outside the observed/declared values |
| `likely_required` | warning | every observed row (≥ 3) fills it in (a stored row that leaves it empty cancels this) |

Observed-only facts are never errors. When a warning comes from a cached census, validate
rebuilds that census once and re-checks (`census.revalidated`), unless it was read less
than 10 minutes ago (`census.revalidate_skipped`) — then the warning is most likely your
own edit (a typo'd key).

**Writes check too.** `create`, `update`, `apply`, `upsert`, `plan`, `sync` and
`globals update` check block rows at every depth against the cached census of that
collection and warn once per code — also when no row has a `blockType` at all
(`layout=[{}]` warns `empty_row`: Payload would drop every row and empty the layout). They never build a census: with none cached you get
one `blocks_unchecked` warning — run `pay blocks learn <collection>` (or any
`pay blocks types|schema|new`) once. Successful writes fold their echo into the census, so
your own migration does not make it stale; each document counts once, so re-writing the
same page does not inflate counts or create `likely_required` warnings.
`pay blocks learn` after a truncated read (`--census-max-docs`) reports what it did not see
as `not_seen`, never `removed`.

---

## 7. `pay describe` and blocks

When GraphQL introspection or project source is available, `pay describe` answers from
the declared schema; otherwise it falls back to the census (`blocks_source:
"observed-census"`).

```bash
pay describe pages --field layout --path .block_types          # the writable slugs
pay describe pages --field layout --path .block_type_sources   # where each slug came from
pay describe pages --field 'layout[].blocks'                   # nested, from the census
pay describe pages --block grid                                # one block's fields
pay describe pages --path .block_docs                          # label + description per slug
```

* `.block_types` are the `blockType` slugs REST accepts (`cta`), never GraphQL
  interfaceNames (`CallToActionBlock`) — sending an interfaceName is a silent drop.
* `.block_docs[SLUG]` (label, description) exists only when the project's own
  `config.ts` was readable on disk and its authors wrote `labels`/`custom.description`;
  `null` with `docs_reason` means nobody documented it, not that `pay` failed.
* `id`, `blockName`, `blockType` are `plumbing: true`: `blockType` is mandatory on every
  row you write, `blockName` is an optional admin label, `id` is server-generated.
* `required: null` inside a block means unknown, never "not required";
  `.block.required_unknown` lists those paths.

On a site without introspection, prefer `pay blocks schema` — it knows observed values,
presence and nested accepted types, which `describe` cannot.

---

## 8. Orphan rows and `--reset-nested`

On Postgres, changing a **nested** block's type (bento → grid inside a group; a group whose
inner blocks you replace by other types) leaves the old nested rows in the database. They
show up as `{}` rows in `pay get` (the stored/published row) — not in `pay get --draft`,
not in the write's echo. Top-level swaps in `layout` are fine.

Signals:

* `orphan_risk` on a dry run (or on a real write when backups are on): which rows would be
  left and why (`causes: ["layout[2].blocks[1]: bento → grid"]`).
* `orphan_rows` after a write: PayCLI read the stored row back and found them.
* `empty_rows` on `pay get` / `pay globals get`, `empty_row` in `pay outline`: the
  document already has them.

Fix — keeps the id, no delete/recreate:

```bash
pay update pages 16 --data-file layout.json --reset-nested --dry-run   # data.reset_nested: the plan
pay update pages 16 --data-file layout.json --reset-nested             # meta.reset_nested.verified: true
pay globals update footer --data-file footer.json --reset-nested --yes
pay update pages 16 --reset-nested --dry-run      # repair: no body, rewrites what is stored
pay update pages 16 --reset-nested
pay publish pages 16 --reset-nested               # a draft save cannot orphan; the PUBLISH can
pay get pages 16 --depth 0                        # no empty_rows warning any more
```

In a pipe: `… | pay apply --reset-nested --yes`. What it does: sends your write, then
"carrier" rows (copies of the old parent blocks with empty nested lists) that make the
Postgres adapter clear the stale tables, then restates the lists without carriers. Check
`meta.reset_nested`: `mode`, `writes`, `cleared_block_types`, `steps`, `verified`.

* One document per command (not with `--where` or several publish ids).
* `--draft --reset-nested` → `reset_nested_not_needed` (a draft save cannot orphan):
  publish it with `pay publish <c> <id> --reset-nested`.
* A repair of a published page with a real pending draft is refused (it would publish
  the draft): publish with `--reset-nested` instead; for a global
  `pay globals update footer --publish --reset-nested --yes`.
* If the final restatement fails, carriers are left at the end of the list: do NOT re-run
  the repair; run the command in the error's hint (it replays the saved restatement).
* `reset_nested_unresolved`: some `{}` rows could not be traced to a block type in the
  last 50 versions; the last resort is `pay backups restore <file> --recreate` (new id).
* Rows left by REMOVING a nested container are invisible to every read. Only a dry run of
  that write (or backups on) predicts them (`orphan_risk`, cause `… removed`); run the
  removal with `--reset-nested`.
* On a project with per-locale status (`localizeStatus`) it is refused
  (`feature_unavailable`).

---

## 9. Gotchas

* The census only knows block types some document uses. A brand-new block type is unknown
  until a document uses it and the census is rebuilt (`pay blocks learn`).
* Blocks embedded inside rich text (Lexical block nodes) are not part of the census; they
  survive `--richtext md` as `<!-- lexical:block … -->` placeholders.
* Inside a top-level GROUP without introspection (`hero.links`), an absent key is treated
  as empty with a `field_absent` warning whose hint lists the arrays the group carries —
  check it before you `add`.
* A row that lacks a nested key that OTHER rows of its type carry is treated as empty
  (`field_absent`) — you can `add` into it.
* `blocks_field_inferred`: neither a schema nor a census knew which field to edit; pass
  `--field`, or run `pay blocks learn <collection>` once.
* `pay apply --field 'layout[2].blocks'` is refused: the write is always the top-level
  field (`--field layout`, or nothing).
* Block fields named like secrets (`hash`, `*Token`, `apiKey`) are profiled but their
  values are never sampled.
