# `pay` query syntax

Everything here applies to `pay find`, `pay count`, and the `--where` half of
`pay update --where` / `pay delete --where`.

## 1. Terms

```
--where 'PATH OP VALUE'
```

A term is split into **at most three whitespace-separated tokens**: the path, the
operator, and the whole rest of the string as the value. Values containing spaces
therefore need no quoting:

```bash
pay find posts --where 'title contains getting started with payload'
```

Repeat `--where` to AND terms. Repeat `--or` to build **one** OR group, which is then
ANDed with the `--where` terms:

```bash
# (status = active) AND (stage = customer OR stage = lead)
pay find crm-contacts --where 'status eq active' \
                      --or 'lifecycleStage eq customer' --or 'lifecycleStage eq lead'
```

Escape hatches, in increasing order of rawness:

| Flag | Meaning |
|---|---|
| `--where-json '{"title":{"contains":"x"}}'` | a literal Payload `where` object; replaces the DSL entirely |
| `--where-raw 'where[title][equals]=x'` | a literal query-string fragment, appended |
| `--no-validate-where` | skip the local path check and let the server answer |

## 2. Paths

Paths are field names, dotted for nesting and for relationships:

```bash
pay find pages         --where 'meta.title exists'                 # a nested group field
pay find crm-contacts  --where 'company.name contains Acme'        # one hop through a relationship
pay find crm-contacts  --where 'company.owner.name exists'         # two hops
```

**Join fields are not queryable.** A `join` field (Payload's reverse relationship) is
read-only and has no queryable path, so `--where 'activities.type eq email'` is rejected
with `query_path_invalid` (exit 5) even though `activities` is a real field. Fetch a join
with `--joins 'activities:limit=5,sort=-createdAt'`, or filter the OTHER collection and
follow its forward relationship. `pay describe <collection>` lists them under
`join_fields`, and `--queryable` never includes them.

An unknown path is rejected **locally** with `query_path_invalid` (exit 5) and a
`did_you_mean` list, because the server answers a bad path with an opaque 400 `QueryError`
whose `data` is an array of paths (not an object — a different shape from a
`ValidationError`).

Find the queryable paths for a collection with:

```bash
pay describe pages --queryable
```

## 3. Operators

| Alias(es) | Payload operator | Value handling |
|---|---|---|
| `eq` `=` `is` | `equals` | typed scalar |
| `ne` `!=` `not` | `not_equals` | typed scalar |
| `gt` `>` | `greater_than` | number / date |
| `gte` `>=` | `greater_than_equal` | number / date |
| `lt` `<` | `less_than` | number / date |
| `lte` `<=` | `less_than_equal` | number / date |
| `contains` `~` | `contains` | substring; `%`, `_` and `\` are escaped for you |
| `like` | `like` | space-separated words, ANDed; adapter-specific |
| `nlike` `!~` | `not_like` | the value is auto-wrapped in `%…%` |
| `in` | `in` | comma-separated (`\,` escapes a comma) or `json:[…]` |
| `nin` | `not_in` | same |
| `all` | `all` | comma list; **rejected on Postgres**, where it 500s |
| `exists` | `exists` | boolean; the value is optional and defaults to `true` |
| `near` | `near` | `lng,lat,maxMeters[,minMeters]` |
| `within` / `intersects` | same | a GeoJSON literal or `@file.geojson` |

Two operators deliberately do **not** pass through unchanged, because passing them through
returns the opposite of what you asked for:

* `nlike` auto-wraps its value in `%…%`. Raw `not_like=hopper` returned **all 104 rows**
  of a contacts collection, including the one it was supposed to exclude.
* `contains` escapes SQL wildcards. Raw `contains=%` returned **all 104 rows**.

## 4. Value typing

`pay` emits `where` as JSON, so values keep their type:

| You write | Sent as |
|---|---|
| `--where 'archived eq true'` | boolean `true` |
| `--where 'views gt 100'` | number `100` |
| `--where 'deletedAt eq null'` | JSON `null` |
| `--where 'code eq "123"'` | the **string** `"123"` |
| `--where 'tags in json:[1,2]'` | the raw JSON array `[1,2]` |
| `--where 'title eq Home'` | the string `"Home"` |

## 5. Sugar

| Flag | Compiles to |
|---|---|
| `--id 11 --id 16`, `--ids 11,16` | `{"id":{"in":[11,16]}}` |
| `--published-only` | `{"_status":{"equals":"published"}}` |
| `--draft-only` | `{"_status":{"equals":"draft"}}` |
| `--q TEXT` | an OR of `contains` over up to 12 title-ish fields; the chosen fields are echoed in `meta.searched_fields` |
| `--since 7d` / `--until DATE` | `{<date field>:{greater_than_equal|less_than_equal: <absolute ISO instant>}}` |

`--since` / `--until` accept four forms, and nothing else:

| Form | Meaning | Examples |
|---|---|---|
| `N[h\|d\|w\|mo]` | N hours / days / weeks / months before now | `12h` `30d` `4w` `1mo` |
| a Go duration | `time.ParseDuration` | `720h` `90m` `36h30m` |
| `YYYY-MM-DD` | that date at `00:00:00Z` | `2026-08-01` |
| RFC 3339 | verbatim | `2026-08-01T09:30:00Z` |

The **field** they resolve against matters and is always reported in `meta.date_field`:
`--date-field F` wins; otherwise a published-scoped query on a collection with a
`publishedAt` field uses `publishedAt`, and everything else uses `updatedAt`. Without that
rule, `pay find posts --published-only --since 30d` would answer "posts *touched* in the
last month" while you asked for "posts *published* in it".

## 6. Sorting, selection, depth

```bash
pay find posts --sort -publishedAt          # leading '-' is descending
pay find posts --select title,slug,_status  # projection
pay find posts --select-exclude content     # everything but these
pay find posts --depth 1                    # embed relationships one level
pay find posts --depth 1 --populate 'categories:title'   # embed, but only these fields
pay find crm-contacts --joins 'activities:limit=5,sort=-createdAt'
```

* An unknown `--sort` field is rejected locally: the server returns **200 with unsorted
  results**, which is indistinguishable from a real answer.
* An unknown `--select` key is rejected locally: the server returns **200 with only `id`**.
* `--depth` defaults to **0** (Payload's own default is 2). Depth is capped by the
  project's config, so a bigger number is not automatically more data.
* **Join fields are always present** as `{docs: [...], hasNextPage: bool}` even at depth 0,
  and cannot be switched off over the query string. Drop them with `--select`.

## 7. Paging

```bash
pay find pages --limit 25 --page 2
pay find pages --all --max 1000 --output jsonl
```

* `--limit` is **page size only** (default 20). `--limit 0` means "unlimited" to Payload
  and is rejected by `pay` with `invalid_args` (exit 5).
* On a bulk write (`update --where`, `delete --where`, `publish/unpublish --where`)
  `--limit` is an **error**: use `--max-docs N` to cap the blast radius, or `--all`.
* `page.truncated` is the only thing you need to check. When there is more, `next.cmd`
  already contains the exact next command.

## 8. Encoding, if you need to reproduce a call

`where` and `data` are sent as URL-encoded JSON strings; everything else uses Payload's
bracket notation (`select[title]=true`, `populate[crm-companies][name]=true`,
`joins[activities][limit]=5`, `sort`, `depth`, `limit`, `page`, `draft`, `trash`).
`select` as a JSON string is silently dropped by the server, which is why the two styles
are mixed. `--where-style qs` prints bracket notation instead, for pasting into a browser.
