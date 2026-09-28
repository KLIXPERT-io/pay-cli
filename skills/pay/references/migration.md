# Idempotent content migration

Moving content into Payload (from Markdown files, another CMS, a spreadsheet) means
running the same import many times while you fix things. `pay` makes every run safe to
repeat: documents are matched by a natural key, only real differences are written,
relationships are resolved by reference, and the whole set is planned before anything is
sent. Never write a "find by slug → update else create" script, and never keep an
`ids.json`.

| Goal | Command |
|---|---|
| One document: create it if missing, else update only what differs | `pay upsert <coll> --data-file doc.json` |
| Many documents + globals, re-runnable, dependency-ordered | `pay plan ./content` → `pay sync ./content --dry-run` → `pay sync ./content --yes` |
| A relationship without looking the id up | `{"$ref":"pages","slug":"about"}` in any write body |
| Prove a migration is complete | `pay plan ./content --brief --path .summary` → `unchanged == items` |
| Look at the result, drafts included | `pay url pages 47 --preview` (§6) |

---

## 1. References: `$ref`

Works in `create`, `update`, `upsert`, `globals update`, `upload`, `diff`, `apply`,
`plan` and `sync` bodies:

```jsonc
{"$ref": "pages", "slug": "about"}                          // → 12
{"$ref": "pages", "slug": "about", "$as": "polymorphic"}    // → {"relationTo":"pages","value":12}
{"$ref": "crm-contacts", "email": "a@b.c", "company": 7}    // several fields = AND
{"$ref": "posts", "where": {"title": {"like": "Launch"}}}   // any Payload filter
{"$ref": "@home"}                                           // another item of the same plan (plan/sync only)
```

```bash
pay update pages 12 --set-json 'parent={"$ref":"pages","slug":"home"}' --dry-run
pay create posts --set title=X --set slug=x --set-json 'categories=[{"$ref":"categories","slug":"news"}]' --draft
```

* Resolved before the request (a `--dry-run` shows the real ids), against the newest
  draft, in the write's locale; cached per run.
* `$as: "polymorphic"` is for links and multi-collection relationships (`{relationTo,
  value}`), e.g. a Lexical internal link or a `link.reference` field.
* `ref_unresolved` (exit 4): no such document — the hint has the `pay find` to check.
  `ref_ambiguous` (exit 5): several — ids in `error.did_you_mean`; add a field or use
  `where`.

---

## 2. One document: `pay upsert`

```bash
pay upsert pages --data-file about.json --dry-run      # action + diff + the exact request
pay upsert pages --data-file about.json                # created | updated | unchanged
pay upsert crm-contacts --match email,company --data-file c.json
pay upsert categories --set slug=news --set title=News # --set/--set-json/--set-md work too
pay upsert pages --data-file about.json --publish      # or --draft; neither = keep the state
```

* `--match` (default `slug`) values are read FROM THE BODY — include them.
* 0 matches → create (a new document on a drafts collection is created as a draft unless
  `--publish`; `created_as_draft` says what publishing needs); 1 → update, sending the
  top-level fields whose value differs plus the unchanged string/number/boolean fields the
  body declares (so a hook such as `populatePublishedAt` cannot overwrite them);
  >1 → `match_ambiguous` (exit 5, ids listed).
* Read `meta.upsert` = `{action: created|updated|unchanged, matched_by, id}`.
  `unchanged` = nothing was written. `--force` rewrites anyway; `--ignore PATH` leaves a
  path out of the comparison (and out of the write).
* `--draft` saves a draft version (a published page keeps its live version);
  `--publish` publishes. `_status` in the body is ignored (`body_status_ignored`).
* Neither flag = keep the state of the NEWEST version. A published page that has a
  pending draft is therefore saved INTO that draft — the live page does not change —
  and the envelope warns `saved_as_draft`. Add `--publish` to take it live (that also
  publishes the draft's other changes; `pay diff pages <id> --draft-vs-published` shows them).

---

## 3. Many documents: manifests, `pay plan`, `pay sync`

A manifest is a directory of `*.json` files (recursive; dot-files skipped), each holding
one item or an array of items:

```jsonc
{"key": "about",                 // optional; default "pages/about" (collection/match values) or "globals/footer"
 "collection": "pages",          // or "global": "footer"
 "match": ["slug"],              // default ["slug"]; values come from data
 "status": "keep",               // keep (default) | draft | published
 "locale": "de",                 // optional
 "data": {"title": "About", "slug": "about", "parent": {"$ref": "@home"}}}
```

```bash
pay plan ./content                          # per item: action, write_mode, fields, diff, depends_on
pay plan ./content --brief --output table   # one line per item
pay sync ./content --dry-run
pay sync ./content --yes                    # needs --yes in a non-TTY (else exit 11)
pay sync ./content --yes --fail-fast
pay get pages 12 --depth 0 --draft | pay plan -   # a saved export IS a manifest
```

**Status is the only place publication is decided:**

* `published`: the write publishes (full validation — may fail on required fields).
* `draft`: saved as a draft version; a published page keeps its live version.
* `keep` (default): the newest version's state. A published page is saved live; a draft
  page gets a new draft; a new document is created as a draft. A published page with a
  PENDING DRAFT is saved into that draft, the live page unchanged — warning
  `saved_as_draft` on the plan, the dry run and the sync. Use `published` to take it live.
* `published` over a pending draft publishes the draft's other changes too: `pay plan` and
  `sync --dry-run` warn `publishes_pending_draft` (review
  `pay diff pages <id> --draft-vs-published` first).
* `"_status"` inside `data` is removed (`body_status_ignored`).

**Reading the plan:** `data.items[]` in execution order — `key`, `action`
(`create`|`update`|`unchanged`), `write_mode` (`publish`|`draft`|`save`), `fields`
(top-level keys the write sends), `diff.changes` (drop with `--brief`), `depends_on`,
`current_status` (the newest version's) and, over a pending draft, `stored_status`
(`published` = the page is live). Read the envelope `warnings` too: `[key]`-prefixed.
`data.summary` counts `items`, `create`, `update`, `unchanged`.

**Order is automatic:** an item referenced by another (`{"$ref":"@key"}`, or an
equals-`$ref` on its match fields) is written first. Two NEW documents referencing each
other = `ref_cycle` (exit 5): create one without the reference first.

**Sync** writes each item through the normal create/update paths: backups
(`PAY_BACKUP_DIR`), audit, echo checks, block checks, orphan checks; item warnings are
prefixed `[key]`. `data` = `{summary, succeeded:[{key, action, collection, id}], failed,
not_attempted}`.

* **Exit 7 from sync is safe to re-run** (unlike a bulk update): fix the failed items
  (`error.failures`, `data.failed`), then run the same `pay sync … --yes`; only what still
  differs is written. Items referencing a failed item are `not_attempted`.
* A failed plan (exit 4/5/10) lists every problem at once in `error.failures`, with the
  partial plan in `data`; nothing was written. `manifest_invalid` = unreadable JSON, an
  unknown key, a missing collection, two items writing the same document.
* A 500/timeout during a sync may still have committed: when the error says "no write of
  this sync was confirmed", run `pay plan <dir> --brief` before re-running.

---

## 4. Exports as manifests

A saved `pay get <coll> <id> --depth 0 --draft`, `pay find … --depth 0 --draft` or
`pay globals get` envelope is a manifest (status `keep`, match `slug` — or `id` for a
collection without slugs, which then only ever updates). Export → edit → `pay plan` shows
exactly your edit.

* An export read above `--depth 0` is refused (`manifest_invalid`): re-export at depth 0.
* An export read with `--richtext md|text` is refused (`richtext_rendered`).
* Editing an export's SLUG does not rename the page: the match finds nothing and the plan
  says `create` (a full copy), with warning `body_id_mismatch`. Rename by id:
  `pay update pages 12 --set slug=new --dry-run`.
* `"<redacted>"` values in an export are never written back. In a top-level field, a group
  or a row they are left out (`redacted_not_sent`: the stored value stays). Inside rich text
  or a json field — stored whole — they are put back from the stored document; only when
  there is none (a create) is the key removed (`redacted_removed`). Export with
  `--no-redact` when you need the real values in a new document.

---

## 5. A complete example: Markdown → categories + a post

Verified end to end. Two items, one referencing the other, rich text from a Markdown file
with YAML front matter.

```bash
mkdir -p content

cat > content/categories.json <<'JSON'
[{"key": "cat-guides", "collection": "categories",
  "data": {"title": "Guides", "slug": "guides"}}]
JSON

# rich text in a manifest must be an editor state: convert the Markdown once
pay lexical from-md body.md --output raw > body.lexical.json
jq -n --slurpfile body body.lexical.json '{
  key: "post-hello", collection: "posts", match: ["slug"], status: "draft",
  data: {title: "Hello", slug: "hello", content: $body[0],
         categories: [{"$ref": "@cat-guides"}]}}' > content/post-hello.json

pay plan ./content --brief
# items: cat-guides create (save), post-hello create (draft, depends_on ["cat-guides"])
pay sync ./content --yes
# data.succeeded: [{key:"cat-guides",action:"create",id:22},{key:"post-hello",action:"create",id:21}]
pay plan ./content --brief --path .summary
# {"items":2,"create":0,"update":0,"unchanged":2}   ← re-running changes nothing
```

Then change `body.md`, regenerate `body.lexical.json` and the item, and run
`pay plan` → one `update` of `content`, `pay sync --yes` → written, `pay plan` → all
`unchanged` again.

For block pages, build rows with `pay blocks new <type> --collection pages --path .row
--output raw` and put them into `data.layout`; `pay plan` runs the same block checks as a
write (`references/blocks.md`).

---

## 6. Checking the result in a browser

### Where does this document live?

```bash
pay url pages 47                          # {url, path, path_source, status, has_unpublished_changes}
pay url pages --slug dashboards           # same, addressed by slug
pay url pages 47 --path .url --output id  # just the URL, as plain text
pay url pages 47 --draft                  # where it WILL live (slug/parent changed in a draft)
```

* `path_source`: `breadcrumbs` (nested-docs; trustworthy) · `template` (the profile's
  `url_paths.<collection>`) · `slug` (`/{slug}` — a GUESS outside `pages`, warning
  `url_path_guessed`) · `home` (`/home` is served at `/`).
* `status: draft` = never published: the URL 404s for visitors (`url_not_live`).
  `has_unpublished_changes: true` = the newest version is a draft; `data.published_path`
  is where it is live now, if different.
* Teach a collection its route once:
  `pay config set profiles.<profile>.url_paths.posts '/posts/{slug}'` (placeholders: any field
  or dotted path at depth 0, `{id}`, `{locale}`).
* The host is `site_url`, defaulting to base_url's origin (Payload 3 is one Next.js app).
  Set `site_url` only when the site really lives elsewhere.

### View a draft without publishing it

```bash
pay url pages 47 --preview
pay url pages 47 --preview --cookie-file ./preview-cookies.json
```

`data.preview` holds `cookie_file` (0600 JSON array of cookies, usually Next.js
`__prerender_bypass`), `cookies[]` (names and domains only, never values), `url`, and
`load` with ready-made commands:

```bash
agent-browser cookies set --curl <cookie_file> --domain <site host> && agent-browser open <url>
curl -sS -H "Cookie: $(jq -r 'map(.name+"="+.value)|join("; ")' <cookie_file>)" <url>
```

Copy `data.preview.load.agent_browser` verbatim (`--domain` is required for
agent-browser's cookie import). `cookies set --curl` exists from agent-browser **0.34**;
an older one (`agent-browser --version`) has only `cookies set <name> <value>` and the page
renders without draft mode (a never-published page 404s) — upgrade, or check with the
`curl` form. The cookie is draft mode for the WHOLE site: every page
you open with it renders its newest draft, until the site is rebuilt (re-run `--preview`
if drafts stop showing). Never paste its value anywhere. This replaces publishing to a
staging site just to look at a page.

**The preview secret** (needed when `preview_path` contains `{secret}`, the default
`/next/preview?path={path}&previewSecret={secret}`), resolved first-hit-wins, never
printed:

1. `PAY_PREVIEW_SECRET_<PROFILE_UPPER_SNAKE>` 2. `PAY_PREVIEW_SECRET`
3. the env var named by the profile's `preview_secret_env` (e.g. the app's own
   `PREVIEW_SECRET`) 4. the credentials store:
   `printf %s "$PREVIEW_SECRET" | pay auth preview-secret --stdin`.

```bash
pay auth preview-secret                   # configured? source? fingerprint? (never the value)
pay config set profiles.<profile>.preview_secret_env PREVIEW_SECRET
pay doctor --path .site                   # site_url, url_paths, preview state
```

`preview_secret` in a config file is refused (`config_secret_in_plaintext`, exit 9).
Failures: `preview_secret_missing` (9, nothing configured, no request sent),
`preview_denied` (8, the route said 403: wrong secret, or the credential is not a
logged-in user), `preview_failed` (10, no route at `preview_path` or no cookie). A
non-template route: `pay config set profiles.<profile>.preview_path '/api/preview?slug={path}&secret={secret}'`.
If `site_url`'s origin differs from `base_url`'s, the credential is withheld from the
handshake (`preview_credential_withheld`) and the template's route answers 403.

---

## 7. Gotchas

* Rows without ids are fine in a manifest or upsert body: on an update each id-less row
  takes the id of the stored row it replaces (same blockType, in order), so keys you do not
  mention — server defaults, values an editor set in the admin UI, nested rows — are KEPT
  and the plan's diff is exactly what is written. Removing a key from a manifest row
  therefore changes nothing — set it to `null` to clear it.
* Dates may be written as `"2026-01-01"`; the stored `"2026-01-01T00:00:00.000Z"` counts
  as equal.
* Presence-based hooks (the website template's `populatePublishedAt` sets `publishedAt` to
  now whenever the request has none) are handled: an update re-sends every unchanged
  string/number/boolean field the item declares. A hook that rewrites a field the write
  did not send (a group, an array) warns `hook_rewrote_unsent` after the write; syncing
  again writes the declared value back. Values a hook COMPUTES on every write (breadcrumbs)
  keep an item `update`: leave them out of the manifest, or `--ignore <path>`. A slug a hook
  FORMATS (`"About Us"` → `"about-us"`) makes the next run try to create the document
  again: after the first write `match_value_normalized` names the stored value — put that
  value in the manifest.
* `orphan_risk` in `pay plan` / `sync --dry-run` = a nested block restructure that leaves
  `{}` rows; its hint names the `--reset-nested` repair (`references/blocks.md` §8).
* Creating a document in an upload collection is refused (`file_missing`): `pay upload`
  the file, then re-run — the item then matches and updates.
* Trashed documents are not matched.
* Relative Markdown links (`../brief.md`) warn `lexical_link_url_relative`: rewrite them to
  site paths, `[t](pay:pages/ID)` or `[t](pay:pages/slug=about)` before converting. The slug
  form becomes a `{"$ref"}` that every write resolves to the id; `[t](pay:pages/about)`
  would store the slug AS the id (a dead link; `relation_id_not_numeric` warns).
* A `$ref` must name a collection its field accepts: where discovery knows `relationTo`
  (GraphQL introspection) a mismatch is `bad_request_body` before anything is sent; where it
  does not (a REST-only site), `ref_target_unchecked` says the check could not be made.
* Relationship ids written as strings (`"4"`, `["66"]`) count as equal to the stored numbers.
