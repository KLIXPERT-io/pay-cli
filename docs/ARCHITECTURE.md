# PayCLI — Architecture Specification

**Status:** authoritative. Implement from this document; it leaves no design decisions open.
**Binary:** `pay` · **Module:** `github.com/KLIXPERT-io/pay-cli` · **Repo:** `KLIXPERT-io/pay-cli` (public)
**Companion:** `docs/GROUNDING.md` (facts). This document supersedes GROUNDING.md wherever they differ; §2 lists the corrections.
**Reference conventions:** `KLIXPERT-io/gsc-cli` (mirrored where noted, overridden where noted).

---

## 0. Scope and design premise

PayCLI drives **any** Payload CMS 3.x project through its REST API. It hardcodes no collection, field,
or slug. Its primary consumer is an **LLM coding agent**, and every trade-off in this document is
resolved in favour of determinism and machine-parseability over human ergonomics.

Five non-negotiable invariants:

1. **Adaptive.** The command surface is static; the *data* it accepts (collection slugs, field paths,
   operators, enum values) is discovered per project and validated client-side before any network call.
2. **Cheap cold start.** Warm path is disk-cache read + JSON decode, no network. Cold path is
   ≤ 4 HTTP requests.
3. **Fault tolerant.** Classified retries, explicit partial-failure outcomes, never a silent wrong answer.
4. **Agent-first.** One JSON envelope on stdout for success *and* failure, discriminated by `ok`.
   Deterministic exit codes. Help text contains this project's real values, never placeholders.
5. **No dead ends.** `pay raw` reaches any endpoint PayCLI does not model, with auth, retry and audit intact.

---

## 1. Conflict resolutions

Five research agents produced overlapping and in places contradictory recommendations. Every
disagreement is resolved here. Where a recommendation was discarded, the reason is stated.

| # | Conflict | Decision | Why (one sentence) |
|---|---|---|---|
| 1 | Command shape: verb-first `pay find pages` (commands) vs noun-grouped `pay docs list pages` (agentux) | **Verb-first, flat: `pay find pages`** | The extra `docs` level buys nothing and lengthens every agent-emitted command; both forms are equally static so the Cobra cold-start argument is a wash. |
| 2 | Dynamic per-collection Cobra subcommands (`pay pages list`) | **Never built**, and the argv-rewrite alias shim proposed as a fallback is **dropped entirely** | Two grammars gated behind a config flag means an agent cannot know which one works; collections are always arguments. |
| 3 | Error envelope destination: stdout (agentux) vs stderr (commands, gopkg, gsc) | **stdout**, plus a one-line human summary to stderr; `PAY_ERRORS_TO=stderr` restores Unix convention | Requirement #4 is a *stable envelope* — an agent must read exactly one stream and branch on `.ok`, not guess which pipe holds the answer. |
| 4 | Envelope shape: `{ok,v,data_kind,data,page,next,meta,warnings}` (agentux) vs `{data,meta}` (commands, gsc) | **agentux's shape**, with `meta.cache` folded in from the caching agent | `data_kind` and `page.truncated` remove the two guesses agents get wrong most often ("what shape is this?" and "did I get everything?"). |
| 5 | Exit-code tables: three incompatible ones (agentux 0–10, commands 0–9, caching 0–7) | **agentux's 0–10 table**, with `validation_failed` mapped to 5 (not commands' 9) and a new **11 = confirmation_required** | agentux's is the superset and is the only one that separates "your key is wrong" (2) from "your key is fine but denied" (8), which the server cannot distinguish for us. |
| 6 | `where` encoding: qs bracket notation (GROUNDING, discovery, agentux) vs URL-encoded JSON string (commands) | **JSON string for `where` and `data` only; bracket notation for everything else** | Re-verified live: `where[jobTitle][equals]=null` returns 0 rows while `where={"jobTitle":{"equals":null}}` returns the true answer 23 — bracket notation is silently wrong for null/bool/number. |
| 7 | Bulk delete: pass `--where` straight through (implied by GROUNDING) vs resolve IDs client-side (commands) | **Resolve IDs client-side, then delete by explicit id list in chunks of 100** | Re-verified live: `DELETE /api/crm-contacts?limit=1&where=...` deleted **all 4** matches — `limit` is not destructured by the bulk-delete endpoint at all. |
| 8 | `pay delete <id>`: hard delete vs soft delete | **Soft delete by default on trash-enabled collections** (`PATCH {deletedAt}`); `--permanent` for the real `DELETE` | Re-verified live: `DELETE /api/crm-contacts/225` destroyed the document outright even though the collection has trash, and it was unrecoverable with `?trash=true`. |
| 9 | Cache on-disk format: two-tier JSON + gob index (caching) vs plain JSON (gopkg, implicit) | **Plain pretty-printed JSON, one consolidated file per scope; no gob** | The measured gob win is 0.6 ms — noise against Go process start — and an opaque binary cache contradicts the agent-first premise that a stuck agent can `cat` the cache. |
| 10 | Cold discovery cost: "exactly 2 requests" (caching) vs "4 requests" (discovery) | **4 requests** (`/access`, `/{auth}/me`, GraphQL stage 1, GraphQL stage 2) | The 2-request figure omits identity verification and the leaf-type batch that enum options and polymorphic targets require; measured cost of all four is ~0.95 s. |
| 11 | Entity inventory source: `/api/access` (GROUNDING, agentux) vs GraphQL `Access` type (discovery) | **Union of both, with a `reachability` field recording which source saw it** | Re-verified live: `/api/access` omits `payload-kv` (zero permission) and GraphQL omits `payload-migrations` (`endpoints:false`) — each source alone loses a collection. |
| 12 | `/api/access` `fields` as a field-name source | **Never used for field names** | Re-verified live: `collections.pages.fields` is the boolean `true` for a privileged key and an object for a restricted one; typed as `true \| false \| map`. |
| 13 | Retry on HTTP 500: retry once for GETs (caching) vs never (agentux, commands) | **Never auto-retry 500** | Re-verified live that the three common 500s (uncastable id, `/versions` on a non-versioned collection, dangling relationship) are deterministic client mistakes, so a retry only doubles the latency of a guaranteed failure. |
| 14 | `X-Payload-HTTP-Method-Override` auto-promotion for long URLs | **Read-shaped requests only; never for `PATCH`/`DELETE`** | Re-verified live: a `Content-Type` of `application/x-www-form-urlencoded; charset=utf-8` makes Payload silently discard the whole body (a `limit=2` body returned 10 docs), which on a scoped bulk delete would become delete-everything. |
| 15 | Output format auto-switching to `table` on a TTY (gsc, commands, gopkg) | **Never.** `json` always, unless `--output` / `PAY_OUTPUT` / `defaults.output` says otherwise; there is no TTY switching at all | Agent harnesses frequently allocate a PTY, so TTY-dependent output makes the same command emit different bytes in different harnesses. |
| 16 | Secret storage: OS keychain with silent file fallback (gsc) vs file-first with opt-in keychain (gopkg) | **File-first** (`credentials.json`, 0600); keychain only when explicitly requested | Re-verified on this machine: `go-keyring` fails even with a live D-Bus session bus (`org.freedesktop.secrets was not provided by any .service files`), and a silent fallback means the secret lands in a different place depending on invisible environment state. |
| 17 | Config dir on macOS: `os.UserConfigDir()` (gsc) vs `~/.config/pay` everywhere (gopkg) | **`~/.config/pay` on both Linux and macOS** | gsc's split between `DataDir()` and `Path()` puts config and cache in different trees on macOS; one guessable path is worth more to an agent than platform idiom. |
| 18 | Skills: shell out to `npx skills add` (agentux considered it) vs native Go + `go:embed` | **Native, embedded, offline** — and, by **product-owner override**, the skill lives at the repository root in `skills/pay/`, not nested under `internal/` | The original decision refused a root `skills/` because `go:embed` cannot reach a *parent* directory, so a root copy would have meant a *second* copy that drifts. That reasoning is about the **copy**, not the location, and it is answered by making `skills/` itself a Go package (`skills/embed.go`, `//go:embed all:pay`): the embed reaches a *child*, `internal/skills` imports it, and there is still exactly one tree. What the move buys is the `skills` convention — `npx skills add https://github.com/KLIXPERT-io/pay-cli/skills --skill pay` resolves against a root `skills/` and nothing else — so non-Go agents get the skill without a PayCLI install. `skills/embed_test.go` enforces the invariant the old rule enforced by construction: the embed root **is** the on-disk directory, byte for byte, and the repository contains exactly one `SKILL.md`. |
| 19 | Generated `SKILL.md` per project | **No.** Static `SKILL.md` + generated `references/PROJECT.md` with a staleness banner | A generated SKILL.md is undiffable in git and invites an agent to trust a stale snapshot as gospel. |
| 20 | Auto-update default | **Off.** `pay update check` is cheap; applying requires opt-in or an explicit command | An agent invokes `pay` hundreds of times per task, and a binary that mutates itself mid-task can change the (discovery-driven) command surface between two calls. |
| 21 | agentux's "auto-append `draft=true` to reads that follow a write in the same process" | **Rejected** | Hidden cross-command state makes identical commands return different data, which is exactly the nondeterminism this spec exists to eliminate. |
| 22 | agentux's claim that `/api/access` permissions may be `{permission:true, where:{...}}` | **Parser must accept it; no UX is built on it** | The claim came with no evidence and my live probe shows plain booleans, so it is handled defensively but not surfaced as a "conditional" permission state. |
| 23 | commands' `--q` fuzzy-search flag | **Kept, capped at 12 fields**, and `meta.searched_fields` always reports which fields it used | An uncapped OR across every text field on a 49-collection project produces an unbounded query; naming the fields it searched keeps the result explainable. |
| 24 | Duplicate-route probing during discovery | **Never probed.** Inferred from the GraphQL `duplicate{Singular}` mutation | The discovery researcher's probe created four real documents on the live instance; re-verified that a *castable* id is safe (`POST /api/crm-contacts/999999999/duplicate` → 404) but the GraphQL signal is free and has no write path at all. |
| 25 | Empty-`POST` probe to harvest required fields | **Opt-in only** (`pay discover --deep --allow-write-probes`), never on by default | It creates a real document in any collection that has no required fields. |
| 26 | `limit=0` pass-through | **Rejected client-side** with `invalid_args` | Re-verified live: `limit=0` means *unlimited* (returned all 11 pages with `"limit":0`), the exact opposite of what an agent writing "limit 0 = none" intends. |
| 27 | Auth: API-key only (original draft) vs API-key + JWT + anonymous | **Three `auth_mode`s: `api-key`, `jwt`, `anonymous`** (§5.0) | `useAPIKey` is opt-in per auth collection (verified `payload/dist/auth/getAuthFields.js`: `if (authConfig.useAPIKey)`), so an API-key-only CLI is unusable against the majority of Payload projects; `Authorization: JWT <t>` and `Bearer <t>` are first-class in `extractJWT.js`. |
| 28 | GraphQL-derived facts typed as non-nullable (`id_type: "number"`, `duplicate: true`) | **Every GraphQL-only fact is tri-state** (`"unknown"` / `null`) with a `*_source` sibling; unknown ⇒ never block locally | A defaulted `id_type` makes PayCLI reject valid Mongo ObjectIds locally with a false `invalid_id`, which is a silent wrong answer produced by PayCLI itself — the one thing §0 forbids. |
| 29 | One consolidated `manifest.json` (original §8.2) | **Split: `manifest.json` index + `fields/<slug>.json` shards**, both plain JSON | Re-measured with Go 1.22 `encoding/json` on §7.8-shaped structs: 49×40 fields = 2.03 MB → **18.4 ms**, 150×200 = 31 MB → **282 ms**, 150×500 = 77.6 MB → **714 ms** — paid on every `pay --help`. The index alone is 55 KB / **496 µs** at 49 entries and 170 KB / **1.44 ms** at 150. |
| 30 | `--limit` meaning on bulk writes (page size vs blast radius) | **`--limit` is page size and is an error on bulk-write verbs; `--max-docs` is the only blast-radius cap** | The original §12.3 used `--limit` for both with two different defaults (20 and 100), so two correct implementations would delete different document sets. |
| 31 | `--jq EXPR` | **Renamed `--path EXPR`** with a closed three-form grammar (§10.3) | jq is not a dependency; advertising jq's name for a hand-rolled evaluator guarantees an agent sends `map(.title)` and gets an unspecified failure. |
| 32 | Matching on English response strings | **`Accept-Language: en` on every request, and classification never depends on a translated string** | `deletedCountSuccessfully`, `followingFieldsInvalid`, `noFilesUploaded` and `notAllowedToPerformAction` all go through `req.t` (verified in `payload/dist`), and `getRequestLanguage` honours `Accept-Language`. |
| 33 | Level-3 reactive invalidation as an ambient transport rule | **Opt-in per request via `payload.WithReactiveInvalidation(ctx)`, set only by `internal/cli`** | Discovery *deliberately* generates every Level-3 trigger (§7.5 probes 1, 2 and 4), so an ambient rule turns one `pay discover` into unbounded nested re-discovery. |
| 34 | `graphql_path` as an independent literal default | **Derived: `{api_path}{graphql_route}`**, `graphql_route` defaulting to `/graphql` | Verified in `payload/dist/utilities/createPayloadRequest.js`: `isGraphQL = pathname === formatAdminURL({apiRoute: config.routes.api, path: config.routes.graphQL})` — the two are nested, not independent. |
| 35 | Localisation fallback left to the server default | **`fallback-locale=none` on every read by default** when localisation is enabled | `config.localization.fallback` defaults to `true` (`payload/dist/config/sanitize.js`), so untranslated fields come back in the default locale and are indistinguishable from real translations. |
| 36 | `unsupported_operators` pre-computed from an assumed adapter | **Starts empty; learned reactively** from observed failures; pre-emptive blocking only when `db_adapter_source == "configured"` | The adapter is not discoverable from the API, and the shipped list (`all`, `near`, `within`, `intersects`) is wrong on MongoDB, where `all` works — PayCLI would refuse a query the server would have answered. |
| 37 | Block `blockType` slugs declared permanently unrecoverable | **Recoverable from project source** when §4.3's upward walk finds one; otherwise the collection is marked `publishable: false` with the reason | Verified live: `Page_Layout.possibleTypes` = `CallToActionBlock…` while `src/blocks/*/config.ts` carries `slug: 'cta'`, `slug: 'content'`, `slug: 'mediaBlock'` — the answer exists on disk, and without it `pay publish pages <id>` is unachievable. |
| 38 | `pay explain` hard-truncating `collections[]` at 40 | **Never truncate the slug list**; truncate only per-collection detail, with an explicit order and real paging flags | Verified: at 49 collections in key-sorted order the entries past 40 are `payload-*`, **`posts`**, `redirects`, `search`, **`users`** — an agent asking "what can I do here?" would conclude blog posts do not exist. |
| 39 | Self-update "fail closed only under `PAY_UPDATE_STRICT=1`" | **Three outcomes**: `verified` / `unverifiable` (warn, proceed) / `verification_failed` (**always abort**) | A signature that is present and mismatches is positive evidence of the exact attack the signing exists to stop; installing anyway is worse than not checking. |
| 40 | Self-update applied in-process at the top of `main()` | **Implicit apply is always a detached child; the swap takes effect on the next invocation** | An in-process swap makes the running process report the new version while executing the old command surface, which is conflict 20's hazard in a subtler form, and `os.Rename` over a running `.exe` fails on Windows outright. |
| 41 | Block knowledge on a site with introspection off and no project source: give up (`blocks_source: "unknown"`) vs learn from content | **The block census (§7.10c)**: blocks learned from real documents read at depth 0, every fact labelled `observed-census`, and an observation only ever **warns** | Payload 3.87 disables introspection in production by default and the app's source is not on the agent's machine, but a depth-0 REST read returns every key of every block row — the rows are a measured schema (§2.9). |
| 42 | Writes and the census: build it on demand vs use it only when cached | **Writes never build the census** (§9.7); they use a cached one, overlay the pre-image they already hold, fold their echo back in, and otherwise emit one `blocks_unchecked` warning | A write — even a `--dry-run` — silently fanning out into hundreds of GETs is the more surprising behaviour; the catalog verbs an agent runs before writing build it anyway. |
| 43 | A body carrying `_status: "published"` written with `--draft` | **`--draft` wins**: `_status` is stripped and `body_publishes` says so (§10.2.2) | Payload's `isSavingDraft` is false whenever the body says `published`, so every edited export of a published page would otherwise publish despite the flag the caller typed. |
| 44 | A non-draft write to a published document that has a pending draft: silently re-route it into the draft vs refuse | **Refuse** (`invalid_args`, exit 5) until `--draft`, `--publish` or `--unpublish` states the outcome (§10.2.3) | Re-routing a write on invisible server state makes one command do two different things (conflict 21's nondeterminism); a refusal that names the three choices costs one re-run and never takes a live page offline. The re-routing variant was implemented during this release and removed before it shipped. |
| 45 | Orphan `{}` rows after a nested block restructure on Postgres: delete + re-create vs an in-place remedy | **`--reset-nested`** — validation-free carrier phases that make the adapter clear the nested tables, then a restatement (§10.2.1) | Re-creating changes the document id and drops its version history; the carrier write is derived from the drizzle adapter's clearing rule (§10.2.1.1) and was verified on the stage document it was designed for. |
| 46 | Writing back a document read with `--richtext md\|text` | **Refused** — `richtext_rendered` (exit 5), nothing sent (§9.12.8.3) | Payload stores a string in a richText field without an error (verified, §2.9); the admin editor can then no longer open the field. |
| 47 | Automatic pre-write backups: on by default vs opt-in | **Off until a directory is configured** (`--backup-dir` / `PAY_BACKUP_DIR` / `backup_dir`); once configured, a backup that cannot be taken **aborts** the write (§12.8) | Writing document bodies into a directory nobody chose is a surprise; once the user opted in, a write without its backup breaks the promise — the same rule as §12.7's pre-audit record. |

---

## 2. Verified ground truth

Everything in this section was re-verified by me against the live Payload 3.86.0 instance at
`http://localhost:3900` on 2026-09-16 with `Authorization: users API-Key paycli-dev-key-…`.
Implementations may rely on these without further checking. Items marked **[correction]** contradict
`docs/GROUNDING.md`.

### 2.1 Discovery surface

| Fact | Evidence |
|---|---|
| `/api/access` is 30,585 B, 14–17 ms, byte-stable for a fixed identity | 3 × `curl /api/access` → identical sha256, `time_total` 0.0145 / 0.0175 / 0.055 |
| **[correction]** `/api/access` is **not** a complete inventory: `payload-kv` is absent because the key has zero permissions on it | `jq '.collections\|keys\|length'` → 49, no `payload-kv`; `GET /api/payload-kv?limit=0` → 403 (so it exists) |
| **[correction]** `collections.{slug}.fields` is the boolean `true` when all field perms are granted | `type(.collections.pages.fields)` → `bool`; `type(.collections.posts.fields)` → `dict` |
| The GraphQL `Access` type zips index-for-index with `Query.docAccess*`, 54 ↔ 54, collections first then globals | `__type(name:"Access"){fields{name}}` minus `canAccessAdmin` = 54; `docAccess*` = 54; tail pairs `payload_preferences↔PayloadPreference`, `crm_features↔CrmFeature` |
| GraphQL misses `payload-migrations`; REST misses `payload-kv`. **Union is mandatory.** | set difference of the two inventories: REST-only `['payload-migrations']`, GQL-only `['payload-kv']` |
| `count{Plural}` exists for exactly the 49 collections and for no global; globals' singular Query field has no `id` arg | `count*` count = 49; `Query.Header.args` = `[draft, select]`, `Query.Page.args` = `[id:Int, draft, select, trash]` |
| Pluralisation is irregular and must not be predicted: `media` → `Media`/`allMedia`/`countallMedia`; `search` → `Search`/`Searches` | `countallMedia` present in the Query field list |
| Required-ness must come from `mutation{Singular}Input`, not the object type | `mutationPageInput.title` = `NON_NULL String`; `Page.title` = nullable `String` (drafts force-nullable the object type) |
| Blocks are opaque: `mutationPageInput.layout` = scalar `JSON`, and `Page_where` has no `layout` key | batched introspection, 23 `Page_where` inputFields, `layout` absent |
| A 30-alias batched `__type` query costs the same as a single one | 30 aliases: 0.391 s / 48,087 B. Single `__type(name:"Page"){fields{name}}`: 0.372 / 0.416 / 0.371 s |
| `depth` is capped by project config, not by the request | `crm-contacts/174` at depth 0/1/2/5 → 1078 / 2195 / 2195 / 2195 bytes |
| `X-Powered-By: Next.js, Payload` on every `/api` response; **no** `ETag`, `Cache-Control`, or `Last-Modified`; `HEAD /api/access` → 404 | header dump + `curl -I` |
| `POST /api/{coll}/access/{id}` works (per-document permissions) | `POST /api/pages/access/1` → 200 `{"fields":true,"readVersions":true,"create":true,…}` |

### 2.2 Capability probes (read-only, 100 % precision on all 49 live collections)

| Capability | Probe | Positive signal | Live result |
|---|---|---|---|
| endpoints disabled | any request | HTTP **501** + body `message` starting `Cannot ` | `payload-migrations` only. **Must be checked first** — it otherwise reads as an upload positive |
| upload | `GET /{slug}/file/__paycli_probe__` | body `message` does **not** start with `Route not found` | `media` → 500; `pages`/`users`/`crm-contacts` → 404 `Route not found "…"` |
| versions | `GET /{slug}/versions?limit=1&depth=0` | 200 **and** `docs` is an array | `pages`,`posts` → 200 + docs; `media`,`crm-contacts` → 500; `payload-preferences` → 200 `{"message":"Not Found","value":null}` (**custom `GET /:key` route shadows it — the `docs` check is mandatory**) |
| drafts | `GET /{slug}?limit=0&where[_status][equals]=published` | 200 | `pages`,`posts` → 200; `crm-contacts` → 400 `QueryError` path `_status` |
| auth | `GET /{slug}/init` (no auth header needed) | body has an `initialized` boolean | `users` → 200 `{"initialized":true}`; `pages` → 500; `crm-contacts` → 403 |
| trash | `GET /{slug}/count?where[deletedAt][exists]=true` | 200 | `crm-contacts` → 200 `{"totalDocs":0}` |
| folders | `GET /{slug}/count?where[folder][exists]=true` | 200 | `media`, `crm-lists`, `payload-folders` |
| plural label | `DELETE /{slug}?where[id][equals]=-1` | `"Deleted 0 <Label> successfully."` | `crm-contacts` → `"Deleted 0 Crm Contacts successfully."`, zero side effects |

### 2.3 Query semantics

| Fact | Evidence |
|---|---|
| `where` and `data` accept a URL-encoded **JSON string**; `select`/`populate`/`joins`/`sort` do **not** | `?where={"id":{"equals":1}}` → the right doc; `?select={"title":true}` → returned only `{"id":16}` (silently dropped) |
| Bracket notation **cannot express `null`** and fails silently | `where[jobTitle][equals]=null` → 0; `where={"jobTitle":{"equals":null}}` → 23; `where[jobTitle][exists]=false` → 23 (the truth) |
| `sort` with an unknown field is **silently ignored** | `sort=bogusField`, `sort=-bogusField` and no sort all returned `[174,173,124]` |
| `limit=0` means **unlimited** | `?limit=0` → 11 docs, `"limit":0`, `"totalPages":1` |
| `not_like` does **not** auto-wrap `%` | `not_like=hopper` → 104 (all rows); `not_like=%hopper%` → 103 |
| `contains`/`like` do **not** escape `%` or `_` | `contains=%` → 104 (all rows); `contains=a` → 85 |
| `all` is unimplemented on the Postgres adapter | `where[tags][all]=1` → 500 |
| `count` accepts only `where` and `trash`; it ignores `draft` | `/pages/count` = 11, `?draft=true` = 11, `?where[_status][equals]=published` = 0 |
| The 16 valid operators are fixed | `equals not_equals greater_than greater_than_equal less_than less_than_equal like not_like contains in not_in all exists near within intersects` |

### 2.4 Write semantics

| Fact | Evidence |
|---|---|
| **Bulk `DELETE` ignores `limit` entirely** | 4 matching docs, `DELETE ?limit=1&where[firstName][equals]=PayCLIArch` → `"Deleted 4 Crm Contacts successfully."`, count afterwards 0 |
| Bulk `PATCH` **does** honour `limit` | 5 matching docs, `PATCH ?limit=2&where=…` → `"Updated 2 Crm Contacts successfully."`, verified 2 rows changed |
| `DELETE /{id}` **hard-deletes** on a trash-enabled collection | `DELETE /crm-contacts/225` → `"Deleted successfully."`; `GET /crm-contacts/225?trash=true` → 404 |
| Soft delete = `PATCH {"deletedAt":"<ISO>"}`; hidden from find/count without `trash=true`; restore = `PATCH ?trash=true {"deletedAt":null}` | count without trash → 0, with trash → 1; restore → `"Updated successfully."` |
| Hard-deleting an already-trashed doc **requires** `?trash=true` | `DELETE /crm-contacts/226` → 404 `{"errors":[{"message":"Not Found"}]}` |
| Bulk verbs hard-require `where` | `DELETE /api/pages` → 400 `"Missing 'where' query of documents to delete."` |
| Bulk ops return **HTTP 400 whenever `errors[]` is non-empty, even with committed successes** in `docs[]` | source `collections/endpoints/{update,delete}.js`; corroborated live by two independent researchers |
| Uploads require the multipart part to be named exactly `file`; sibling fields go in a `_payload` part holding a JSON string | `-F upload=@…` → 400 `"No files were uploaded."`; JSON body with `{"alt":"x"}` → same error |
| `POST /{coll}/{castableId}/duplicate` on a missing id is safe | `POST /api/crm-contacts/999999999/duplicate` → 404 `Not Found`. A **non-castable** id creates a document — never do it |

### 2.5 Error shapes (six, all live-verified)

```
route unknown      404  {"message":"Route not found \"/api/nope-nope\""}          (no `errors` key)
document unknown   404  {"errors":[{"message":"Not Found"}]}
validation         400  {"errors":[{"name":"ValidationError","data":{"collection":"pages",
                          "errors":[{"label":"Title","message":"This field is required.","path":"title"},…]},
                          "message":"The following fields are invalid: Title, Content > Layout, Slug"}]}
query path         400  {"errors":[{"name":"QueryError","data":[{"path":"nosuch"}],
                          "message":"The following path cannot be queried: nosuch"}]}      (`data` is an ARRAY)
endpoints disabled 501  {"message":"Cannot GET http://localhost:3900/api/payload-migrations"}
masked server err  500  {"errors":[{"message":"Something went wrong."}]}
non-/api path      404  text/html, 22,729 bytes, body starts `<!DOCTYPE html><html id="__next_error__">`
malformed body     400  {"errors":[{"message":"Invalid JSON"}]}
bulk missing where 400  {"errors":[{"message":"Missing 'where' query of documents to delete."}]}
```

### 2.6 Auth

| Fact | Evidence |
|---|---|
| A wrong API key returns **HTTP 200** with `{"user":null,"message":"Account"}` — status codes are useless for auth | `-H 'Authorization: users API-Key WRONG' /api/users/me` → 200 |
| There is no 401 in this deployment; denied writes are 403 with an identical body whether unauthenticated or merely unpermitted | anon `POST /api/crm-contacts` → 403 `"You are not allowed to perform this action."` |
| `/api/{auth}/me` echoes the API key **in plaintext** | `{"user":{…,"apiKey":"<LOCAL_DEV_API_KEY>",…}}` |
| The auth-collection slug is part of the header and is project-specific | `Authorization: {authCollectionSlug} API-Key {key}` |

### 2.7 Environment

Go 1.22.2 system toolchain with `GOTOOLCHAIN=auto` (verified to transparently fetch go1.26.x).
`goreleaser` is not installed locally. `go-keyring` v0.2.8 fails on this machine.

### 2.8 Second-pass verification (critique review, 2026-09-16)

Everything here was verified after the first draft, and is the evidence base for §§5, 7, 8, 9, 15 and 21.

| Fact | Evidence |
|---|---|
| The API-key fields exist **only** when `useAPIKey: true` | `payload/dist/auth/getAuthFields.js`: `if (authConfig.useAPIKey) authFields.push(...apiKeyFields)` |
| Payload also accepts `Authorization: JWT <token>` and `Authorization: Bearer <token>` | `payload/dist/auth/extractJWT.js`, `extractionMethods = {Bearer, cookie, JWT}` |
| `POST {api}/{auth}/login` is routed and validates input | `POST /api/users/login` with junk creds → 400 `ValidationError` (a routed handler, not a 404) |
| A **wrong or nonexistent** auth-collection slug in the header silently yields the anonymous view | `Authorization: not-a-collection API-Key <valid key>` → 200, **10** collections; valid header → **49**; unauthenticated → the same 10 |
| `/{slug}/init` is access-gated, so the candidate list cannot be widened by blind probing | `/api/users/init` → 200 `{"initialized":true}`; `/api/crm-contacts/init` → **403** |
| `useAPIKey` is discoverable over GraphQL | `mutationUserInput.inputFields` contains `enableAPIKey` and `apiKey` |
| No response carries the Payload version; `/api/access` has exactly three keys | `curl -I` → `X-Powered-By: Next.js, Payload` only; `keys(/api/access)` = `["canAccessAdmin","collections","globals"]` |
| The project's `package.json` **does** carry it (local filesystem, no network) | `dependencies.payload` = `3.86.0` |
| The GraphQL endpoint is `{routes.api}{routes.graphQL}`, not an independent path | `payload/dist/utilities/createPayloadRequest.js`: `isGraphQL = pathname === formatAdminURL({apiRoute: config.routes.api, path: config.routes.graphQL})`; defaults are `/api` and `/graphql` |
| The introspection guard fires **only** on a field literally named `__schema` or `__type`, and **only** under `NODE_ENV=production` | `@payloadcms/graphql/dist/index.js`: `NoProductionIntrospection` → `if (process.env.NODE_ENV === 'production') { if (node.name.value === '__schema' \|\| node.name.value === '__type') … }`. A `{__typename}` probe therefore can never detect it |
| A 450-alias `__type` batch is fine; `maxComplexity` is not the binding constraint here | 450 aliases, 37 KB request → 200, 529 KB, 0.47 s |
| `localization.fallback` defaults to **true**, and an absent request fallback resolves to `defaultLocale` | `payload/dist/config/sanitize.js` `config.localization.fallback = config.localization?.fallback ?? true`; `sanitizeFallbackLocale.js` returns `localization.defaultLocale` |
| `fallback-locale` accepts `false` / `none` / `null` to disable fallback, and is read from the query string | `sanitizeFallbackLocale.js`; `createPayloadRequest.js` reads `fallback-locale` / `fallbackLocale` |
| An **unknown** locale code is silently coerced to the default locale, HTTP 200, no warning | `addLocalesToRequest.js` `sanitizeLocales`: `else if (localization && !localization.localeCodes.includes(locale) && localization.fallback) locale = localization.defaultLocale` |
| GraphQL `LocaleInputType` exposes only `formatName(code)`-mangled **names**, never the underlying values | `buildLocaleInputType.js` keys by `formatName(locale)`; `formatName.js` replaces `.` `-` `/` `+` `,` `(` `)` `'` `[` `]` with `_` and strips spaces |
| The strings discovery and error classification match on are **translated** | `collections/endpoints/delete.js` → `req.t('general:deletedCountSuccessfully')`; `errors/ValidationError.js` → `t('error:followingFieldsInvalid')`; `errors/MissingFile.js` → `t('error:noFilesUploaded')` |
| Request language comes from cookie → `Accept-Language` → `config.i18n.fallbackLanguage` | `payload/dist/utilities/getRequestLanguage.js` |
| A read **without** `draft` does **not** filter to published documents | `GET /api/pages?limit=100&depth=0` → 11 docs, **every one** `_status:"draft"`, byte-identical to the same call with `draft=true` |
| Payload validates the **whole document** on `PATCH`, reporting fields the caller never sent | `PATCH /api/posts/1 {"heroImage":"4"}` → 400 listing `title`, `slug`, `content` **and** `heroImage` |
| Relationship ids are type-sensitive on write | `{"heroImage":"4"}` → 400 `"This relationship field has the following invalid relationships: 4 0"`; `{"heroImage":4}` is accepted |
| Publishing re-runs the validation `--draft` skipped | `PATCH /api/pages/1 {"_status":"published"}` → 400 on `layout` (`"This field requires at least 1 Row."`) |
| Block `blockType` slugs are absent from GraphQL but present in project source | `Page_Layout.possibleTypes` = `CallToActionBlock, ContentBlock, MediaBlock, ArchiveBlock, FormBlock`; `blockType` is a bare `String`; `src/blocks/*/config.ts` → `slug: 'cta' \| 'content' \| 'mediaBlock' \| 'archive' \| 'formBlock' \| 'banner' \| 'code'` |
| `select[id]=true` is a working REST id-type probe | `GET /api/pages?limit=1&depth=0&select[id]=true` → `{"docs":[{"id":16}],…}`, 156 B |
| `/{coll}/versions` exposes the parent document id, typed like the document id | `GET /api/pages/versions?limit=1` → `docs[0].parent` = `16` (int), `docs[0].id` = `20` |
| Key-sorted truncation at 40 hides the collections that matter | sorted slugs `[40:]` = `payload-folders, payload-jobs, payload-locked-documents, payload-migrations, payload-preferences, posts, redirects, search, users` |
| Manifest decode cost (Go 1.22.2, `encoding/json`, §7.8-shaped structs, best of 5) | 49×20 = 1.02 MB → 9.05 ms · 49×40 = 2.03 MB → 18.4 ms · 150×200 = 31.0 MB → 282 ms · 150×500 = 77.6 MB → 714 ms |
| Index-only decode cost (§8.2's split layout, same method) | 49 entries = 55.5 KB → **496 µs** · 150 = 170 KB → **1.44 ms** · 500 = 567 KB → **4.79 ms**; one 200-field shard = 210 KB → **1.81 ms** |

### 2.9 Third-pass verification (agent-ergonomics release, 2026-09-28)

Verified against **Payload 3.87.1** — the klixpert stage (Postgres, `NODE_ENV=production`,
**read-only** except the one approved scratch page named in §10.2.1.2) and the local website-template
instance at `localhost:3900` (Postgres, introspection on; only `paywt-*` test documents, all deleted
afterwards) — and against the installed sources `payload`, `@payloadcms/drizzle`,
`@payloadcms/db-postgres` and `@payloadcms/richtext-lexical` 3.87.1. This is the evidence base for
§§7.10c, 9.11–9.17, 10.2.1–10.2.3 and 12.8.

| Fact | Evidence |
|---|---|
| Introspection is **off by default in production** | `payload/dist/config/types.d.ts`: `graphQL.disableIntrospectionInProduction?: boolean` `@default true`; set in `config/defaults.js`; stage → `capabilities.graphql.mode = "introspection_disabled"`, `pay describe pages` lists 13 REST-observed fields, no block slug, no block schema |
| A depth-0 REST read returns **every key of every block row, nulls included**, at every depth | stage `pay get pages 47 --draft --depth 0`: 26 blocks nested three deep (`layout[].blocks[].blocks`), each row carrying its full key set |
| Slugs inferred from GraphQL interface names are wrong, and Payload drops the row silently | localhost with no project source: inferred `callToActionBlock`; `update pages 49 --set-json 'layout=[{"blockType":"callToActionBlock"},{"blockType":"cta"}]'` → 200, the echo holds only the `cta` row |
| Payload pages list reads by offset `(page-1)*limit` | stage `find posts --limit 50 --page 2` = documents 51–100 of `--limit 100 --page 1`; a smaller last-page limit re-reads and skips documents |
| An update-by-id merges the body into the **newest version** (the pending draft, if any) | `payload/dist/collections/operations/updateByID.js` l.79 `getLatestCollectionVersion`; globals `globals/operations/update.js` `getLatestGlobalVersion` |
| A draft save never touches the main (stored) row | `collections/operations/utilities/update.js` l.253 `if (!isSavingDraft)` → `db.updateOne` |
| `isSavingDraft = draftArg && drafts && data._status !== 'published'` | same file l.29 (globals `update.js` l.42); localhost page 47: `update --data-file <published export> --draft` published the page |
| A plain save of a published page with a pending draft stores the **draft and its `_status: "draft"`** as the live row | localhost pages 63/67/68: stored row went `published` → `draft`, the site answered 404, no warning from the pre-release build |
| Bulk update merges into the **stored** rows unless `?draft=true` | `collections/operations/update.js` l.100-127 (`db.find` vs `queryDrafts`); the admin's bulk Publish sends `?draft=true` (`@payloadcms/ui` `PublishMany`) |
| Array/blocks rows merge **by id**; an id-less row is a new row whose unsent keys take their `defaultValue` | `fields/hooks/beforeChange/getExistingRowDoc.js`, `fields/hooks/beforeValidate/promise.js` (`getFallbackValue`); rows the body omits are not carried over |
| The Postgres adapter clears a nested block table **only if the new document still reaches** a blocks field that accepts that type | `@payloadcms/drizzle/dist/upsertRow/index.js` l.516-525 (`blocksToDelete`, `DELETE WHERE _parentID = <doc id>`), `transform/write/traverseFields.js` l.130-134; rows read back grouped by `_path` → stale rows reappear as `{}` (§10.2.1.1) |
| The orphan is real and survives a clean echo | stage page 71 (`paywt-orphan-test`, created for this, permanently deleted): bento→grid inside a group → echo clean, `pay get --draft` clean, `pay get` → `[{}, text A, {}, text B, {}]` |
| `?unpublishAllLocales=true` skips validation on a non-draft update and updates the newest version in place (no per-locale status) | `utilities/update.js` l.154-157; localhost page 56: `layout: []` accepted with the switch, rejected without; version count unchanged |
| A string in a richText field is **stored**, not rejected | `@payloadcms/richtext-lexical/dist/validate/index.js` walks nodes only when `value.root.children` exists; localhost `content: "## just a string"` draft → 201, read back as a string |
| An editor state sent to a **text** field is stored as JSON text | localhost `meta.description` read back as `"{\"root\":{\"children\":…"` |
| A global update answers `{"message", "result": {…}}` | localhost `POST /api/globals/header`; `payload.writeDoc` now unwraps `result` (before, every field was reported `input_silently_dropped`) |
| `trash=true` returns trashed **and** live documents | localhost `crm-companies` |
| A create ignores a body `id` | localhost `create crm-companies` with `"id":125` → id 127 |
| Postgres version snapshots carry **different row ids** than the document | stage pages 47 versions 699 vs 687: a leaf diff reports 111 changes (almost all `layout[…].id`), a structural one 7 |
| The website template's draft preview is a GET that sets a cookie and redirects | `src/app/(frontend)/next/preview/route.ts` in `~/payload-dummy`; localhost: never-published page → 404 without cookie; handshake `307` + `__prerender_bypass` (HttpOnly, Path=/, SameSite=Lax); with the cookie → 200 with the draft's title |
| A link node with an empty URL fails validation on a non-draft write | localhost: `400 … link node failed to validate`; `richtext-lexical` `validate/validateNodes.js` + `validateUrlMinimal` |
| An agent's non-interactive shell may not see `~/.local/bin` | the migration agent's shell: `~/.local/bin/pay` existed, PATH lacked the directory, `pay` = "command not found" |

---

## 3. Repository and package layout

Every file. Nothing else is created.

```
pay-cli/
├── VERSION                       # "0.1.0" — single source of truth, drives the git tag
├── .go-version                   # "1.27.1" — the toolchain CI and goreleaser build with
├── go.mod  go.sum
├── LICENSE                       # MIT
├── README.md  INSTALL.md  CHANGELOG.md
├── Makefile  .goreleaser.yaml  .golangci.yml  .editorconfig  .gitignore
├── install.sh  install.ps1        # install.sh also prints the PATH advice of §18's binary_on_path
├── scripts/arch-lint.sh          # the §3.1 greps; `make lint`
├── docs/
│   ├── GROUNDING.md              # probed facts (input)
│   └── ARCHITECTURE.md           # this file
├── .github/
│   ├── dependabot.yml
│   └── workflows/{ci,live,release,tag-and-release,codeql}.yml
├── cmd/pay/main.go               # 20 lines: build vars → os.Exit(cli.Main())
├── tools/recordfixtures/main.go  # re-records + redacts testdata/fixtures from a live instance
├── testdata/
│   ├── fixtures/                 # access.json, pages_list.json, pages_get.json, users_me.json,
│   │                             # users_me_anon.json, users_login.json,
│   │                             # error_{validation,notfound,route,501,500}.json,
│   │                             # error_validation_de.json      <- German body, §11.2 regression guard
│   │                             # graphql_stage1.json, graphql_stage2.json, graphql_disabled.json,
│   │                             # graphql_introspection_disabled.json, graphql_partial_errors.json,
│   │                             # globals_header.json, versions_list.json, locale_all.json,
│   │                             # manifest.json, manifest_id_unknown.json, fields_pages.json
│   └── golden/                   # <case>.out / .err / .exit for CLI-level tests
├── skills/                       # the agent skill — ROOT level, one copy, §14
│   ├── embed.go                  # package skills: //go:embed all:pay, exports FS()
│   ├── embed_test.go             # asserts the embedded tree IS this directory
│   └── pay/                      SKILL.md
│                                 references/{query-syntax,errors,recipes,gotchas}.md
└── internal/                     # every package also holds its *_test.go files
    ├── buildinfo/      buildinfo.go
    ├── apierr/         code.go  error.go  exit.go  payload.go  diagnose.go
    ├── redact/         redact.go  headers.go
    │                   url.go            # URL, and URLWithSecrets for literal secrets (§5.3, §9.16)
    ├── fsatomic/       fsatomic.go  rename.go  read_other.go  read_windows.go   # §3.1 atomicWrite
    ├── fetch/          fetch.go          # client-side download of `pay upload <URL>` (§13)
    ├── config/         paths.go  file.go  profile.go  resolve.go  project.go  interpolate.go  pathjoin.go
    │                   projectscan.go    # reads package.json + scans block slugs (§7.10) — local FS only
    ├── secret/         store.go  file.go  env.go  keyring.go  helper.go  chain.go  fingerprint.go
    │                   jwt.go            # JWT credential record: {token, exp}; never the password
    │                   preview.go        # §5.5 preview-secret chain (never the keychain)
    ├── payload/        client.go  transport.go  retry.go  errors.go  find.go  mutate.go  globals.go
    │                   versions.go  upload.go  auth.go  login.go  graphql.go  override.go
    │                   reactive.go       # WithReactiveInvalidation(ctx) — §8.4 Level-3 opt-in
    │                   site.go           # §6.3 site requests: SiteGet, SameOrigin (no redirect follow)
    │   └── query/      json.go  brackets.go  dsl.go  operators.go  validate.go
    ├── discovery/      manifest.go  index.go  shard.go  authboot.go  stage0.go  stage1.go  stage2.go
    │                   stage3.go  degrade.go  fieldkind.go  labels.go  idtype.go  locales.go
    │                   blocks.go  blockschema.go  provenance.go  fingerprint.go
    │                   census.go  census_fetch.go  census_merge.go  census_validate.go   # §7.10c
    │                   census_overlay.go  census_diff.go  census_secret.go
    ├── cache/          store.go  key.go  scope.go  ttl.go  gc.go  generation.go
    │                   census.go  census_json.go         # census.json: verified read, locked RMW (§7.10c)
    │                   lock_unix.go  lock_windows.go
    ├── rows/           rows.go           # selectors, move/copy/remove/set, StripIDs (§9.11)
    │                   path.go           # the nested --field path grammar and resolution (§9.11.3)
    │                   outline.go        # DocTree: every row array of a document (§9.13)
    ├── lexical/        doc.go  nodes.go  mdblock.go  mdinline.go  frommd.go  tomd.go
    │                   plaintext.go  placeholder.go  testdata/{golden,md,fuzz}/     # §9.12 — pure
    ├── docdiff/        docdiff.go  patch.go  path.go  richtext.go                   # §9.14.5 — pure
    ├── orphans/        orphans.go                                                   # §10.2.1.8 — pure
    ├── manifest/       manifest.go  ref.go  order.go     # §9.15 — reads manifest files; no HTTP
    ├── backup/         backup.go                         # §12.8 on-disk format only; no HTTP
    ├── output/         envelope.go  json.go  jsonl.go  table.go  csv.go  id.go  format.go  writer.go
    ├── logging/        logging.go
    ├── audit/          audit.go  rotate.go
    ├── safety/         risk.go  confirm.go  dryrun.go  blastradius.go
    ├── skills/         embed.go  install.go  manifest.go  project.go
    │                   # installer only: the bytes come from the root skills package
    ├── update/         update.go  state.go  detach.go  managed.go  verify.go
    │                   lock_unix.go  lock_windows.go  platform_unix.go  platform_windows.go
    ├── payloadtest/    server.go  fixture.go  golden.go  clock.go      # imported only by _test.go
    └── cli/            app.go  root.go  help.go  local.go  opmemo.go
                        complete.go  complete_blocks.go  completion.go
                        auth.go  auth_preview.go  profile.go  config.go  whoami.go  access.go  can.go
                        doctor.go  doctor_census.go  doctor_site.go
                        discover.go  explain.go  collections.go  describe.go
                        find.go  get.go  count.go  create.go  update.go  delete.go  restore.go
                        duplicate.go  publish.go  upload.go  download.go  globals.go  versions.go
                        pipeline.go  blocks.go  blocks_path.go  apply.go        # §9.11
                        blocks_catalog.go  blocks_census.go                     # §7.10c
                        lexical.go  richtext.go  outline.go  diff.go            # §9.12–§9.14
                        refs.go  upsert.go  plan.go  sync.go                    # §9.15
                        docresolve.go  url.go  url_preview.go                   # §9.16, §9.17
                        orphans.go  reset_nested.go  pubguard.go                # §10.2.1–§10.2.3
                        backup.go  backups.go                                   # §12.8
                        raw.go  cache.go  skills.go  selfupdate.go  version.go  audit.go
```

### 3.1 Architectural rules (CI-enforced)

* `internal/cli/app.go` is the **only** file outside `cmd/` allowed to reference `os.Stdout`,
  `os.Stderr`, `os.Stdin`, `os.Args`, `os.Getenv`, `os.Exit` or `time.Now`. A `make lint` grep fails
  the build otherwise. (This is the single change that makes CLI-level testing possible; gsc-cli
  hardwires `os.Stdout` in `internal/cmd/root.go:277-296` and therefore has 2 test files for 5,705 LOC.)
* `internal/config.Resolve` is a **pure function** — no I/O — so the precedence matrix is a table test.
* Every `http.Transport` sets `Proxy: http.ProxyFromEnvironment`.
* Every durable write goes through one `atomicWrite(path string, data []byte, perm fs.FileMode) error`
  (temp file in the same directory with a `.<pid>.<rand6>.tmp` suffix → fsync → chmod → rename → fsync dir).
* `internal/discovery` and `internal/payload` never import `internal/cli` or `spf13/cobra`.
* **`arch-lint.sh` additionally fails on:** (a) the literal string `api_key` appearing as a TOML key in
  any example or test `config.toml` (`config_secret_in_plaintext`); (b) any call to
  `payload.WithReactiveInvalidation` from a file under `internal/discovery/` (§8.4); (c) any
  `http.Request` construction outside `internal/payload/transport.go` (so `Accept-Language` and the
  `Authorization` redaction rule cannot be bypassed); (d) any `fmt`/`log` call that takes a
  `*http.Request`'s `Header` map as an argument.
* **Pure packages.** `internal/lexical`, `internal/docdiff`, `internal/orphans` and `internal/rows`
  perform no I/O, read no clock or environment and import nothing from `internal/cli`, cobra or
  `internal/payload`; `internal/docdiff` and `internal/orphans` import only `internal/lexical`.
  `internal/manifest` (reads manifest files) and `internal/backup` (reads and writes backup files
  through `fsatomic`) may touch the filesystem but never HTTP and never `internal/cli`. These are
  held by the import graph (`go list -f '{{.Imports}}'`) and review; `arch-lint.sh` does not grep
  for them. They are what make the diff, orphan and Markdown logic table-testable against recorded
  documents.
* Every new durable file of this release — backup files (§12.8), the preview cookie file (§9.16),
  the saved `--reset-nested` restatement (§10.2.1.5), `pay lexical … -o FILE`, `census.json` — is
  written through `fsatomic`, so the atomic-write rule above covers them. Site requests (§6.3) are
  built in `internal/payload/transport.go` like every other request, so rule (c) still holds.
* **Repo-wide secret assertion:** a test walks every byte of `testdata/golden/*.{out,err}` and every
  line every test writes to the audit log or a log sink, and fails if it contains the fixture key
  string (`PAY_TEST_API_KEY`'s value, or the fixture literal `paycli-dev-key-…`). This is the test
  that keeps §5.3 honest.

### 3.2 Dependencies

Direct (7):

```
github.com/spf13/cobra          v1.10.2   command tree, completions
github.com/spf13/pflag          v1.0.10   declared direct: help generation introspects FlagSets
github.com/BurntSushi/toml      v1.6.0    config
golang.org/x/term               v0.46.0   IsTerminal, ReadPassword
golang.org/x/sys                v0.48.0   flock
golang.org/x/mod                v0.41.0   semver.Compare for self-update
github.com/zalando/go-keyring   v0.2.8    OPT-IN keychain only (pure Go, +2 indirects)
```

Test-only: `github.com/google/go-cmp v0.7.0`.

The agent-ergonomics release adds **no dependency**: the Markdown parser and renderer of §9.12 are
~1.5k lines of stdlib Go (`html.UnescapeString` resolves entities), and the structural diff (§9.14),
the orphan analysis (§10.2.1) and the manifest planner (§9.15) are plain Go over decoded JSON.
Dev tools via go.mod `tool` directives (`go tool staticcheck`, `go tool goimports`).

**Rejected:** `viper` (untestable precedence), `go-retryablehttp`/`resty` (Payload-specific
classification is ~120 lines over net/http anyway), `go-querystring` (cannot emit
`where[or][0][and][0][x][equals]=y`), `testify`, `go-vcr` (opaque YAML cassettes defeat the
agent-readable-fixtures goal), any TUI/color library, `encoding/json/v2` (verified still gated behind
`GOEXPERIMENT=jsonv2` in Go 1.26).

`go.mod` declares `go 1.26.0` (language minimum, **not** a patch pin — gsc-cli's `go 1.26.2`
needlessly rejects distro toolchains). `.go-version` holds `1.27.1` for CI and releases.
The Makefile fails fast with an explanatory message when `go env GOTOOLCHAIN` is `local` and the
system Go is older than 1.26.

---

## 4. Configuration

### 4.1 Paths

One source of truth, `internal/config/paths.go`. `$PAY_HOME` overrides all three.

| | Linux **and macOS** | Windows |
|---|---|---|
| config | `$PAY_CONFIG_DIR` → `$XDG_CONFIG_HOME/pay` → `~/.config/pay` | `%AppData%\pay` |
| cache | `$PAY_CACHE_DIR` → `$XDG_CACHE_HOME/pay` → `~/.cache/pay` | `%LocalAppData%\pay\cache` |
| state | `$PAY_STATE_DIR` → `$XDG_STATE_HOME/pay` → `~/.local/state/pay` | `%LocalAppData%\pay\state` |

Files: `config.toml` (0600), `credentials.json` (0600), `audit.log` (0600) in config dir;
`v1/<scope>/…` (including `census.json`, §7.10c) in cache dir; `update-state.json`,
`preview/<profile>.cookies.json` (§9.16) and `reset-nested/<profile>/…json` (§10.2.1.5) in state
dir. All directories 0700. The backup directory (§12.8) is **not** one of these: it exists only when
the user names one, and `pay config paths` reports it as `backup_dir` (`""` when off).

`rm -rf $(pay cache path)` must never break the CLI, only slow it down.
`pay config paths --output json` prints all of them so an agent never guesses.

### 4.2 `~/.config/pay/config.toml`

```toml
version = 1
default_profile = "local"
# backup_dir    = "~/pay-backups"  # §12.8. TOP-LEVEL key (profiles are sub-directories of it);
#                                  # a relative value is anchored at the directory of THIS file

[defaults]
output        = "json"     # json | jsonl | table | csv | id | raw
depth         = 0
limit         = 20
timeout       = "30s"
deadline      = "120s"
max_retries   = 3          # flag: --max-retries (alias --retries)
concurrency   = 8
redact        = true
accept_language = "en"     # §6 — load-bearing, see §21 A11
max_bulk      = 100        # default for --max-docs: blast-radius cap on bulk WRITES only.
                           # --limit is page size and is an error on a bulk-write verb (§12.3).
confirm_writes = false     # true => even L1 single-doc writes prompt
census_max_docs = 500      # §7.10c: documents the block census reads per collection (negative rejected)

[cache]
dir            = ""        # "" = default
discovery_ttl  = "24h"
access_ttl     = "10m"
schema_ttl     = "24h"
identity_ttl   = "60s"     # memory only, never written to disk
skills_ttl     = "6h"

[logging]
level  = "info"            # debug | info | warn | error
format = "text"            # text | json

[update]
auto    = false
channel = "stable"

[profiles.local]
base_url         = "http://localhost:3900"
api_path         = "/api"
graphql_route    = "/graphql"      # appended to api_path; this is Payload's own nesting
# graphql_path   =                 # SET ONLY TO OVERRIDE the derivation above
auth_collection  = "auto"          # "auto" = discover (§7.0); or an explicit slug e.g. "users"
auth_mode        = "auto"          # auto | api-key | jwt | anonymous            (§5.0)
label            = "payload-dummy dev"
api_key_env      = ""              # read the key from this env var
credential_helper = ""             # shell command whose stdout is the key
insecure_skip_verify = false
blocks           = {}              # optional manual blockType map, see §9.7

# --- Optional pins. Each one turns a discovered-or-unknown value into a configured one and
# --- flips the matching *_source to "configured". Never guessed, never defaulted.
# id_type         = "string"       # per-profile override for EVERY collection's id type
# locales         = ["en", "de"]   # true locale codes; see §7.9
# payload_version = "3.86.0"
# db_adapter      = "mongodb"      # postgres | mongodb | sqlite
# echo_check_ignore = ["slug", "email"]   # paths this project's hooks rewrite (§10.2)
# custom_endpoints  = []

# --- Frontend URLs and draft preview (§9.16). All optional.
# site_url           = "https://www.example.com"   # default: base_url's ORIGIN (source "derived:base_url")
# preview_path       = "/next/preview?path={path}&previewSecret={secret}"   # the website template's route (default)
# preview_secret_env = "PREVIEW_SECRET"            # the NAME of an env var holding the preview secret
# [profiles.local.url_paths]
# posts = "/posts/{slug}"                          # collection -> path template (§9.16.2)

[profiles.staging]
base_url        = "https://staging.example.com"
api_path        = "/api"
auth_collection = "admins"
auth_mode       = "jwt"
[profiles.staging.headers]
"X-Vercel-Protection-Bypass" = "${VERCEL_BYPASS}"   # ${ENV} interpolated at load
```

`base_url` and `api_path` are **separate settings** — Payload's `routes.api` is configurable and the
app may sit under a Next.js `basePath`. Nothing may hardcode `/api`.

**`graphql_path` is derived, not an independent default.** Payload nests the two routes:
`routes.api` defaults to `/api`, `routes.graphQL` defaults to `/graphql`, and the endpoint is always
`{routes.api}{routes.graphQL}` (verified in `createPayloadRequest.js`, §2.8). PayCLI therefore computes
`graphql_path = api_path + graphql_route`, and only an explicitly set `graphql_path`
(flag / `PAY_GRAPHQL_PATH` / config key) overrides it. `pay config explain` reports the derived case as
`{"value": "/cms-api/graphql", "source": "derived:api_path+graphql_route"}` so the coupling is visible.
This is the single most common `routes` customisation — a project that sets only
`routes.api: '/cms-api'` would otherwise leave PayCLI pointed at a stale `/api/graphql`, get an HTML
404, classify it `route_missing`, and silently drop to REST-only discovery on a project whose GraphQL
works perfectly.

**`base_url` may not carry userinfo in any output.** `config.Resolve` parses `base_url`, strips any
`url.Userinfo` into a separate, non-serialisable `basicAuth` field on the resolved profile, and stores
only the userinfo-free URL. The transport applies `basicAuth` as an `Authorization: Basic` header,
which is then redacted like every other credential (§5.3). `pay config explain` and a startup warning
name where the credential was moved. Rationale: `base_url` is echoed into `meta.base_url`, `error.http.url`,
`Event.BaseURL`, the cache manifest and `--dry-run`'s `request.url`, and §5.3's key-name
redaction cannot see a credential embedded in a URL string.

The key `api_key` is **not a valid field anywhere in config.toml**; its presence is a hard error
(`config_secret_in_plaintext`). `base_url` containing userinfo is on the same arch-lint list.
**`preview_secret` is forbidden the same way, at any depth of any config file**
(`config_secret_in_plaintext`, exit 9, hint
`printf %s "$PREVIEW_SECRET" | pay auth preview-secret --stdin` or `preview_secret_env`).

**Site keys (§9.16).** `site_url` must be absolute http(s); userinfo, a query or a fragment is
`invalid_option` (exit 5); a base path is allowed (Next.js `basePath`). Every `url_paths` template
and `preview_path` must start with `/` (`invalid_option`, with a runnable `pay config set` hint).
`url_paths` merges per collection across layers (project wins per key, like `blocks`).
`preview_secret_env` holds an env-var **name**; it is on redaction's non-secret allowlist (next to
`token_exp`) so `pay config explain` can show it. `pay config explain` reports `site_url`,
`url_paths`, `preview_path`, `preview_secret_env`, `census_max_docs`, `backup_dir` and `no_backup`.

**`census_max_docs`** (`[defaults]`, int, default 500, negative rejected) bounds the block census
(§7.10c); `PAY_CENSUS_MAX_DOCS` and the catalog verbs' `--census-max-docs N` override it.

### 4.3 Project config

`./pay.toml`, discovered by walking up from CWD to the git root (or filesystem root). Same schema
minus secrets. It is intended to be committed, so a repo like `/home/flo/payload-dummy` can ship one
and an agent that `cd`s in needs zero flags.

### 4.4 `~/.config/pay/credentials.json` (0600)

```json
{
  "version": 1,
  "profiles": {
    "local":   {"auth_mode": "api-key", "api_key": "paycli-dev-key-…",
                "fingerprint": "a1b2c3d4e5f60718", "updated_at": "2026-09-16T17:00:00Z"},
    "staging": {"auth_mode": "api-key", "keyring": true,
                "fingerprint": "9f8e7d6c5b4a3928", "updated_at": "2026-09-16T17:00:00Z"},
    "cloud":   {"auth_mode": "jwt", "token": "eyJhbGciOi…", "token_exp": "2026-09-17T17:00:00Z",
                "login_identifier": "ops@example.com", "login_field": "email",
                "fingerprint": "3c2b1a0918273645", "updated_at": "2026-09-16T17:00:00Z"}
  }
}
```

`fingerprint` = the **first 16 hex chars** of `sha256("paycli-key-v1\x00" + credential)`, where
`credential` is the API key in `api-key` mode and the JWT in `jwt` mode; the literal `"anon"` in
`anonymous` mode. **16 hex everywhere** — `credentials.json`, `manifest.meta.key_fingerprint`,
`manifest.identity.key_fingerprint`,
`pay auth status`, `pay cache ls`, `pay doctor` and the §8.1 scope input all use the same 16 characters,
so a profile can always be matched to a scope by string equality. A unit test asserts every producer
emits the identical value for a fixed input.

The raw key and the raw token never appear in a cache key, a log line, an audit record, or any output.
A password is **never** stored: `pay auth login --jwt` keeps only the minted token and its expiry.

A profile record may also carry **`"preview_secret": "…"`** (§5.5), written by
`pay auth preview-secret --stdin`. It is independent of the API credential:
`pay auth login` carries an existing preview secret over, `pay auth preview-secret --clear` touches
only that field, clearing the last credential of a record removes the record, `pay auth logout`
removes it with the record and `pay auth rename` moves it. A record holding **only** a preview secret
is not an API credential: §5.1's chain skips it (auth stays anonymous), `pay auth status` reports
`authenticated: false` and `pay auth list` shows `credential: "none"`. The OS keychain is never used
for the preview secret.

### 4.5 Precedence

Two independent chains, both implemented in the pure `Resolve`.

**Profile selection** (first non-empty wins):
`--profile` → `PAY_PROFILE` → project `pay.toml` `default_profile` → user `config.toml`
`default_profile` → the sole profile if exactly one exists → literal `"default"`.

**Field resolution** (first non-empty wins):
flag → env var → project `[profiles.X]` → user `[profiles.X]` → project `[defaults]` →
user `[defaults]` → built-in default.

An env var that is **set but empty counts as unset** (`os.LookupEnv` then skip empty), so
`PAY_API_KEY= pay …` cannot silently blank the key.

`pay config explain --output json` prints, for every effective setting,
`{"value": …, "source": "flag|env:PAY_X|project:/abs/pay.toml|user:/abs/config.toml|default"}`.

Three settings of this release resolve through their own chains, stated here so the general rule is
not misapplied:

* **`backup_dir`** (top-level, never per profile): `--backup-dir` → `PAY_BACKUP_DIR` →
  `./pay.toml backup_dir` → `config.toml backup_dir` → off. A relative value is anchored **where it
  was written**: a config-file value at the directory of that file (a committed
  `backup_dir = ".pay-backups"` means "next to pay.toml" from any sub-directory), a flag or env value
  at the working directory; a leading `~/` expands to `$HOME` (`%USERPROFILE%`). `Resolve` stays pure:
  the working directory arrives as `Input.WorkDir` (from `App.WorkDir`), never from `os.Getwd`.
  `--no-backup` / `PAY_NO_BACKUP=1` (flag beats env) disables writing for one invocation without
  forgetting the directory. `pay config set backup_dir <relative>` writes the **user** config, where a
  relative value would anchor at the config directory, so it stores `<working directory>/<value>` and
  warns `config_path_anchored` (hint: put it in `./pay.toml` instead).
* **`site_url`**: `PAY_SITE_URL` → project profile → user profile → derived from `base_url`'s origin
  (source `derived:base_url`). No flag.
* **the preview secret**: §5.5.

### 4.6 Environment variables

```
PAY_PROFILE  PAY_BASE_URL  PAY_API_PATH  PAY_GRAPHQL_PATH  PAY_GRAPHQL_ROUTE  PAY_AUTH_COLLECTION
PAY_AUTH_MODE                # auto|api-key|jwt|anonymous
PAY_API_KEY  PAY_API_KEY_<PROFILE_UPPER_SNAKE>  PAY_JWT  PAY_JWT_<PROFILE_UPPER_SNAKE>
PAY_HOME  PAY_CONFIG  PAY_CONFIG_DIR  PAY_CACHE_DIR  PAY_STATE_DIR
PAY_OUTPUT  PAY_ERRORS_TO
PAY_DEPTH  PAY_LIMIT  PAY_MAX_DOCS  PAY_TIMEOUT  PAY_DEADLINE  PAY_MAX_RETRIES  PAY_CONCURRENCY
PAY_NO_CACHE  PAY_CACHE_TTL  PAY_REFRESH
PAY_LOG_LEVEL  PAY_LOG_FORMAT  PAY_VERBOSE  PAY_QUIET
PAY_NO_REDACT  PAY_INSECURE_SKIP_VERIFY  PAY_KEYRING (auto|off|force)
PAY_NO_AUDIT  PAY_NO_UPDATE  PAY_UPDATE_URL  PAY_UPDATE_STRICT
PAY_YES                     # equivalent to --yes; for non-interactive automation
PAY_BACKUP_DIR  PAY_NO_BACKUP                              # §12.8 automatic pre-write backups
PAY_CENSUS_MAX_DOCS                                        # §7.10c block census document cap
PAY_SITE_URL  PAY_PREVIEW_SECRET  PAY_PREVIEW_SECRET_<PROFILE_UPPER_SNAKE>   # §9.16, §5.5
PAY_TEST_BASE_URL  PAY_TEST_API_KEY   # live tests only
```

The variable named by a profile's `preview_secret_env` (e.g. `PREVIEW_SECRET`) is read too (§5.5).

---

## 5. Authentication and secrets

### 5.0 Three auth modes

API-key auth is **opt-in per auth collection** in Payload: the `apiKey` / `enableAPIKey` /
`apiKeyIndex` fields are appended only `if (authConfig.useAPIKey)` (verified,
`payload/dist/auth/getAuthFields.js`), and the live test project needed `useAPIKey: true` added to
`users` before a key could be minted at all. A CLI that can only send
`Authorization: {coll} API-Key {key}` is therefore **unusable** — including for the public reads it
could otherwise serve — against the majority of real Payload projects. Payload's own `extractJWT`
accepts two further header forms. PayCLI supports all three:

| `auth_mode` | Header sent | Credential | Obtained by |
|---|---|---|---|
| `api-key` | `Authorization: {authCollection} API-Key {key}` | the API key | `pay auth login --api-key…` |
| `jwt` | `Authorization: JWT {token}` | a minted JWT + its expiry | `pay auth login --jwt --email X --password-stdin` |
| `anonymous` | *(no `Authorization` header)* | none | no credential resolves, or `--auth-mode anonymous` |

`auth_mode = "auto"` (the default) resolves to `api-key` if a key resolves through §5.1, else `jwt` if
a stored non-expired token resolves, else `anonymous`. The resolved mode is reported in
`pay config explain`, `pay auth status`, `pay doctor`, `manifest.identity.auth_mode` and
`meta.auth_mode`. **`auto` never prompts and never logs in on its own.**

`Bearer` is accepted by Payload but PayCLI always sends `JWT`, because `Bearer` is also what many
reverse proxies and API gateways consume and would strip. `--auth-header-scheme Bearer` switches it
for a project whose `auth.jwtOrder` excludes `JWT`.

**`jwt` mode mechanics.**

* `pay auth login --jwt [--email X | --username X] --password-stdin [--auth-collection SLUG]` POSTs
  `{api_path}/{authCollection}/login` with `{"email"|"username": …, "password": …}`. Which identifier
  field the collection wants is discoverable: `mutation{Singular}Input` / the collection's object type
  contains `email`, `username`, or both (`loginWithUsername`). When both exist and neither flag was
  given, PayCLI fails `invalid_args` (exit 5) naming both flags rather than guessing. Verified live
  that a wrong guess is a clean 400 `ValidationError` naming the missing field, so the error is
  actionable either way.
* A 200 response carries `{token, exp, user}`. PayCLI stores **`{token, token_exp}` only** — never the
  password — in `credentials.json` (0600) or the keyring, and reports the expiry in `pay auth status`.
* On any **read** that returns 403 while `GET {api_path}/{authCollection}/me` now returns
  `user: null`, PayCLI re-logs-in **exactly once** using the stored identifier and a credential
  re-resolved through §5.1, then retries the read. It needs a live password source to do this; if none
  resolves, it fails `auth_invalid` (exit 2) with the hint `pay auth login --jwt --profile <p>`.
* **Writes are never auto-retried after a re-login**, for the same §6.1 reason a write is never
  retried at all: the original request may have committed before the 403 was produced by a later hook.
  PayCLI refreshes the token, then returns `auth_invalid` (exit 2) with a `next.cmd` that re-runs the
  command verbatim.
* A token whose `token_exp` is in the past (or within 60 s) is treated as absent: PayCLI re-logs-in
  **before** sending, which is a fresh request and therefore safe for writes too.

**`anonymous` mode mechanics.**

* No `Authorization` header is sent. Stage 0's `/me` assertion is **skipped**.
* `identity.verified = false`, `identity.auth_collection = null`, `key_fingerprint = "anon"`.
* Every collection's `permissions` come from the anonymous `GET {api_path}/access` result, which is
  the true answer for an anonymous caller (verified: 10 collections anonymous vs 49 authenticated).
* **Every envelope** — success and error — carries
  `{"code":"anonymous_session","message":"No credential resolved for profile \"local\"; running unauthenticated. Permissions, the collection list and field visibility are all reduced.","hint":"pay auth login --profile local --base-url http://localhost:3900"}`
  in `warnings[]`. It is a warning, not an error: anonymous reads are a legitimate, useful mode, and
  §8.1's `keyFP = "anon"` sentinel exists precisely so its discovery cannot contaminate an
  authenticated scope.
* §11.5's 403 handling keys off the mode: `anonymous` ⇒ `auth_required` (exit 2);
  `jwt`/`api-key` with `identity.verified == true` ⇒ `access_denied` (exit 8).

### 5.1 Resolution chain

`internal/secret/chain.go`, first hit wins, each step records its source string for `pay config explain`:

1. `--api-key-stdin` — read one line from stdin, trimmed
2. `--api-key-file <path>` — trimmed; refuses group/world-readable files
3. `--api-key <value>` — works, but warns on a TTY (visible in `ps` and shell history)
4. `$PAY_API_KEY_<PROFILE_UPPER_SNAKE>`
5. `$PAY_API_KEY`
6. the env var named by the profile's `api_key_env`
7. `credentials.json` entry for the profile — refuses to read if the file mode is group/world
   readable, emitting `auth_insecure_permissions` with the hint `pay auth fix-perms`
8. OS keychain — **only** if the entry has `"keyring": true` or `PAY_KEYRING=force`
   (service `pay-cli`, account `profile:<name>`), wrapped in a 2 s context deadline
9. the profile's `credential_helper` — `sh -c`, 10 s timeout, stdout trimmed; non-zero exit →
   `auth_helper_failed`. This is how 1Password (`op read op://vault/payload/key`), Vault and AWS
   Secrets Manager are supported with zero extra Go dependencies.
10. a stored, unexpired JWT for the profile (`credentials.json` or keyring) ⇒ `auth_mode = "jwt"`
11. **nothing resolved.** If `auth_mode` was set explicitly to `api-key` or `jwt`, this is
    `auth_missing` (exit 2) with the hint `pay auth login --profile <p> --base-url <url>`. If
    `auth_mode` is `auto` or `anonymous`, PayCLI **continues in `anonymous` mode** (§5.0) — it does
    **not** fail. Refusing to run without a credential would make PayCLI useless against a project
    with public read access, and §8.1 already has the `anon` cache sentinel to keep the two apart.

Steps 1–9 yield an API key ⇒ `auth_mode = "api-key"`. `--api-key…` flags and `PAY_API_KEY*` force
`api-key`; `PAY_JWT*` and a stored token force `jwt`; `--auth-mode anonymous` short-circuits the whole
chain and sends no credential even when one is available (useful for "what can an anonymous visitor
see?").

### 5.2 Storage policy

Default write target is `credentials.json`. `--keyring` writes to the keychain and records
`"keyring": true`. `PAY_KEYRING=force` makes the keychain the default **and fails loudly** if it is
unavailable — never a silent fallback, because the user must know where the secret went.
`PAY_KEYRING=auto` tries the keychain, falls back to the file, and prints a stderr warning naming the
fallback path. `PAY_KEYRING=off` never touches dbus/wincred at all.

### 5.3 Compensating controls (mandatory, because the default is a file)

* `internal/redact` scrubs `apiKey`, `apiKeyIndex`, `hash`, `salt`, `password`, `sessions`,
  `resetPasswordToken`, `resetPasswordExpiration`, and any key matching `(?i)token|secret` from **all**
  output, logs, audit records and cache writes. Disabled only by `--no-redact`. A `null` under such
  a key is left as `null`: it hides nothing, and masking it turned null into the string `"<redacted>"`
  in every document a get → edit → write loop sent back (write bodies handle masks, §9.15.2).
  Required because `GET /api/{auth}/me` returns the API key in plaintext (verified).
* **Header names are matched too**, which the value-key list above does not cover:
  `redact.Header(name)` returns true for
  `(?i)^(authorization|proxy-authorization|cookie|set-cookie|x-api-key|api[-_]?key|x-payload-.*-token)$`.
  This closes the `[logging] level = "debug"` / `PAY_LOG_LEVEL=debug` hole — the `Authorization`
  header is **synthesised by the transport**, not read from `[profiles.X.headers]`, so the
  "configured headers are secret" rule never covered it.
* **Hard transport rule (§6):** the transport logs the fixed literal
  `Authorization: <redacted:fp=a1b2c3d4e5f60718>` and never the header value, in any log level, any
  log format, `--dry-run` output or audit record. A unit test asserts that no log line, audit record,
  golden `.out`/`.err` byte, or envelope produced anywhere in the test suite contains the fixture key
  string (§3.1's repo-wide secret assertion).
* **`redact.URL(string) string`** strips `url.Userinfo` and rewrites any query parameter whose name
  matches the header/value matchers above to `<redacted>`. It is **required** on every URL that
  crosses a boundary: `meta.base_url`, `error.http.url`, §12.2's `request.url`, `Event.BaseURL`,
  `Event.Path`, the manifest's `meta.base_url` and `source.base_url`, and `references/PROJECT.md`. A table
  test covers `user:pass@host`, `user@host`, `?api-key=…`, `?token=…` and a URL with neither.
* **`error.raw` and `--output raw` are redacted by default** — see §11.1 and §10.3. The original
  "`error.raw` is always byte-faithful" rule contradicted this bullet and is withdrawn: the concrete
  leak was §12.5's `partial_failure` envelope embedding `raw: {docs:[…]}` for a bulk update against
  the auth collection, which returns every touched user's `apiKey` in plaintext (verified §2.6) — and
  those envelopes are exactly what §17.3 commits to the repo as golden files.
* `/api/{auth}/me` responses are **memory-only**. They are never written to disk in any form; a
  redacted projection `{id, collection, strategy, username_or_email}` may be.
* All configured `[profiles.X.headers]` values are treated as secret and redacted.
* `pay auth status` prints `{profile, base_url, auth_mode, auth_collection, auth_collection_source,
  key_fingerprint, key_source, token_exp, authenticated}` and never the key or the token.
* `credentials.json` is in the `.gitignore` template; `pay auth login` warns if the detected project
  root's `.gitignore` does not cover the agent skill directory.
* **Literal secrets in URLs.** `redact.URLWithSecrets(raw, secrets)` replaces each literal (≥ 8
  chars), and its `QueryEscape`/`PathEscape` forms, with `<redacted>`, then applies `redact.URL`.
  The structural rule already masks `?previewSecret=…` (the name contains "secret"); the literal pass
  covers a custom `preview_path` that puts the secret in a path segment or under an innocuous
  parameter name. `payload.Request.Secrets` carries the literals, and the transport's shown URL is
  the only form that reaches `Response.URL`, `error.http.url`, a transport error and every `-v` debug
  line. `Runtime.PreviewSecret()` adds the resolved value to the logger/audit literal scrubber.
* **Envelope keys never contain `secret`/`token` unless the value is meant to be masked** — the
  structural rule would mask the whole value. The preview fields are therefore named `key_source`,
  `key_fingerprint`, `preview_key_source`, `preview_profile_env_var`. Error messages and hints are
  text-scrubbed (`name=value` around "secret"), so no message quotes `previewSecret={secret}`.
* **A structural diff masks by path**, because the key-based pass cannot see a secret whose name is
  only in `path` while its value sits under `from`/`to`: `pay diff`, `pay versions diff` and every
  dry-run `data.diff` mask `from`/`to` of a change whose path ends in a `redact.Key` and raise
  `data_redacted` naming the paths (§9.14.6).
* **Backups and the census use narrower rule sets on purpose** — a masked value can never be
  restored, and the census is keyed by schema names, not by values. §12.8.4 and §7.10c state them.

### 5.4 Verification

Auth is verified **only** by `GET {base}{api_path}/{authCollection}/me` returning a **non-null
`user`**. Status codes are meaningless: a wrong key returns 200 `{"user":null}` (verified), and this
project allows unauthenticated reads. `pay auth login` verifies by default; `--no-verify` skips it.

The assertion is conditional on the mode:

| `auth_mode` | Stage 0 requirement | `identity.verified` |
|---|---|---|
| `api-key`, `jwt` | `body.user != null`, else `auth_invalid` (exit 2) | `true` |
| `anonymous` | `/me` is **not** requested at all | `false` |

Resolution of `auth_collection = "auto"` is **Stage -1** (§7.0) and runs *before* everything else,
because the slug is part of the `api-key` header and therefore part of the very first request. It is
not a Stage-3 concern; the original spec's placement was circular.

`pay doctor` reports, for every auth collection it found, whether that collection actually **has**
API-key support — discoverable without a write: the collection's `mutation{Singular}Input` (or its
object type) contains `apiKey` and `enableAPIKey` (verified live on `mutationUserInput`). When no
auth collection on the project has `useAPIKey`, `pay doctor` and every `auth_missing` /
`auth_invalid` hint print the **JWT** command
(`pay auth login --jwt --email you@example.com --password-stdin`) rather than
`pay auth login --api-key`, because the latter can never succeed there.

### 5.5 The preview secret

`pay url --preview` (§9.16) needs the site's draft-preview secret (the website template's
`PREVIEW_SECRET`). `secret.ResolvePreviewSecret`, first hit wins, set-but-empty counts as unset:

1. `$PAY_PREVIEW_SECRET_<PROFILE_UPPER_SNAKE>`
2. `$PAY_PREVIEW_SECRET`
3. the variable named by the profile's `preview_secret_env`
4. `credentials.json` → the profile's `preview_secret` (§4.4)

Sources are reported as `env:PAY_PREVIEW_SECRET`, `env:PREVIEW_SECRET`,
`file:credentials.json (preview_secret)`; the fingerprint is §4.4's formula (unkeyed on purpose, so
`pay auth preview-secret` and `pay doctor` fingerprints compare across machines). Nothing resolved is
not an error in itself; it becomes `preview_secret_missing` (exit 9) only for `pay url --preview`
with a `preview_path` containing `{secret}`, raised **before any request**.

`pay auth preview-secret` reports `{profile, configured, source, fingerprint, env_vars,
profile_env_var, credentials_file}`; `--stdin` stores one line in `credentials.json` (0600) and warns
`preview_secret_shadowed` when an env var still wins over it; `--clear` removes only the stored copy;
`--stdin --clear` is `invalid_args`. The value is never printed (tested).

---

## 6. HTTP transport

`internal/payload/transport.go`. One `*http.Client` per process.

```go
&http.Transport{
    Proxy:                 http.ProxyFromEnvironment,
    DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
    TLSHandshakeTimeout:   5 * time.Second,
    ResponseHeaderTimeout: 15 * time.Second,
    ExpectContinueTimeout: 1 * time.Second,
    IdleConnTimeout:       60 * time.Second,
    MaxIdleConns:          64,
    MaxIdleConnsPerHost:   32,   // Go's default of 2 silently serialises every fan-out
    MaxConnsPerHost:       <concurrency>,
    ForceAttemptHTTP2:     true,
}
```

Per-request `context.WithTimeout` (default 30 s, 120 s for uploads and bulk) plus a per-command
`context.WithDeadline` (default 120 s). `ResponseHeaderTimeout` rather than `Client.Timeout` so a
slow-but-progressing large download is not killed.

Every request carries `User-Agent: pay/<version> (<os>/<arch>)`, `X-Request-Id: <ULID>` (echoed in
`meta.request_id`, so the user can grep the Payload server log for exactly the request the agent made),
and **`Accept-Language: en`**.

`Accept-Language: en` is **load-bearing, not cosmetic.** Payload renders user-facing strings through
`req.t`, and `getRequestLanguage` resolves cookie → `Accept-Language` → `config.i18n.fallbackLanguage`
(verified). A project configured with
`i18n: { supportedLanguages: { en, de }, fallbackLanguage: 'de' }` returns
`"0 Crm Contacts erfolgreich gelöscht."` instead of `"Deleted 0 Crm Contacts successfully."`. Pinning
`en` makes every *English-only* signal PayCLI still uses (§7.5 probe 8's label harvest) behave the same
on every project. It is a **belt**, not the braces: §7.5 and §11.2 additionally require that no
classification depend on a translated string, because a project may ship `supportedLanguages` without
`en` at all, or a proxy may inject its own `Accept-Language`. `--accept-language <tag>` overrides it
for anyone who wants localized server messages in `error.raw`.

The transport is the **only** place an `*http.Request` is constructed (arch-lint rule, §3.1), so the
`Accept-Language` header and the `Authorization`-never-logged rule (§5.3) cannot be bypassed by a
call site.

Response bodies are always fully drained and closed, including on error paths, or connections never
return to the pool. The server advertises `Keep-Alive: timeout=5`, so a pooled connection idle for
more than 5 s will fail its first reuse — that specific error is retried once immediately with zero
backoff and does **not** consume the retry budget, because the request never left the client.

`CheckRedirect`: max 3 hops, **same host only**, and **no `https` → `http` downgrade**; a host change
drops the `Authorization` header and aborts, and a downgrade aborts even when the host is unchanged
(`https://cms.example.com` and `http://cms.example.com` share a `Host`, so the host check alone cannot
see it, and Go copies `Authorization` across a same-hostname redirect). An `http` → `https` *upgrade*
is still followed. Trailing slashes produce a 308 (`GET /api/pages/` → `Location: /api/pages`), which
is followed and does not count against the retry budget.

### 6.1 Retry classification

Retry is permitted only for **idempotency-safe** requests: `GET`, `POST /api/graphql` carrying a
read-only query, and `POST` with `X-Payload-HTTP-Method-Override: GET`.

Real writes (`POST`/`PATCH`/`DELETE`/`PUT`) are retried **only** on a proven pre-flight failure —
`ECONNREFUSED`, DNS failure, TLS handshake failure, or a write error before any response byte arrived.
Once any response byte is read, never retry a write: Payload has no idempotency-key mechanism, so a
retried `POST` duplicates the document.

| Status | Policy |
|---|---|
| 502, 503, 504 | retry |
| 429 | retry, honour `Retry-After` (seconds or HTTP-date), cap 120 s. Payload 3.x has no rate limiter, so a 429 always came from a proxy/WAF |
| 500 | **never retry** — verified deterministic (uncastable id, `/versions` on a non-versioned collection, dangling relationship); diagnose instead (§11.3) |
| 400, 401, 403, 404, 405, 409, 413, 414, 422, 423, 431, 501 | never retry |
| 431 / 414 / 413 | trigger the method-override fallback once (§6.2), not a retry |
| 307, 308 | follow, ≤3 hops, same host, no https → http downgrade |

Network errors retried: `ECONNREFUSED`, `ECONNRESET`, `EPIPE`, `ETIMEDOUT`, `EHOSTUNREACH`,
`ENETUNREACH`, temporary DNS failure, HTTP/2 GOAWAY, `io.ErrUnexpectedEOF` mid-body, and
`http: server closed idle connection`.
Never retried: x509 errors of any kind, `context.Canceled` from the user's SIGINT.

Backoff: **decorrelated jitter** — `sleep₀ = 250 ms`, `sleepₙ = min(8 s, rand(250 ms, sleepₙ₋₁ × 3))`.
Max 4 attempts total (**`--max-retries`**, default 3 retries; `--retries` is a registered alias kept
for muscle memory and both resolve to the same field — one name, one value, asserted by a unit test). The wall-clock deadline is authoritative
over the attempt count; a retry whose sleep would exceed the remaining budget is skipped.
Parallel operations share one atomic retry budget of `3 × concurrency` so a dead server cannot turn a
500-item batch into 1,500 doomed requests.

### 6.2 URL-length fallback

Node's default `--max-http-header-size` is 16,384 bytes for the entire header block. Before sending
any `GET` whose full URL exceeds **8,000 bytes**, and on receiving 431/414/413, PayCLI rewrites to:

```
POST <same path, no query>
X-Payload-HTTP-Method-Override: GET
Content-Type: application/x-www-form-urlencoded      <- EXACTLY this byte string
<body> = the encoded query string
```

**Hard requirements**, because Payload's check is `=== 'application/x-www-form-urlencoded'` and a
`; charset=utf-8` suffix makes it silently discard the entire body (verified: a `limit=2` body
returned 10 docs):

* A runtime assertion refuses to send the override unless the `Content-Type` is byte-equal.
* A unit test asserts the exact header string.
* The override path is **never** used for `PATCH` or `DELETE`. Bulk writes that exceed the URL budget
  are split into multiple smaller id-scoped requests instead. A silently-unscoped bulk delete is not
  a risk worth taking for a convenience.

### 6.3 Site requests (the frontend, not the API)

`pay url --preview` (§9.16) GETs a route of the **website**, not of `/api`. `payload.Request` gains:

* `URL string` — an absolute URL replacing `base_url + api_path + Path`.
* `NoRedirect bool` — the transport returns the 3xx as is. Implemented as a context value checked
  first in the single `CheckRedirect` (`http.ErrUseLastResponse`), so the one-client/one-pool rule
  holds.
* `Secrets []string` — literals to scrub from every shown URL (§5.3).

**Credentials never cross an origin.** When `Request.URL`'s scheme://host[:port] differs from
`base_url`'s (`payload.SameOrigin`, default ports normalised), the transport sends neither the
`Authorization` header nor the profile's `[profiles.X.headers]`. Same origin — the Payload 3 norm, one
Next.js app — sends both, which the website template's route needs (`payload.auth({headers})`).

`Client.SiteGet(ctx, SiteRequest{URL, Secrets})` → `SiteResult{Status, Location (scrubbed), Cookies,
CrossOrigin, BodyExcerpt (≤200 chars, scrubbed), HTTP}`: a GET, never follows a redirect, never
promoted to a method-override POST (a secret-bearing URL must not move into a body), normal §6.1 retry
for a read. It **never classifies the status**: a Next.js route handler's 403 is not Payload's
`access_denied`; the caller maps it (§9.16.3).

---

## 7. Discovery

### 7.0 Stage -1 — auth-collection bootstrap (only when `auth_collection == "auto"`)

The `api-key` header embeds the auth-collection slug, so with `auth_collection = "auto"` there is no
slug to send on the very first request. Worse, guessing wrong does **not** error: verified live that
`Authorization: admins API-Key <valid key>` and `Authorization: not-a-collection API-Key <valid key>`
both return HTTP 200 with the *anonymous* 10-collection view, byte-indistinguishable from a real
low-privilege key. A bootstrap that guessed would silently produce a truncated inventory in which 39
of 49 collections appear not to exist. Stage -1 therefore runs **before Stage 0** and is explicit
about which header it sends at each step.

Skipped entirely when: `auth_collection` is set explicitly; or `auth_mode == "anonymous"`; or
`$CACHE/v1/auth-resolution.json` already holds a resolved slug for this `(normURL, keyFP)` pair
(see (e)).

1. **`GET {api_path}/access` with NO `Authorization` header.** Purely to obtain a *candidate* slug
   set. This result is **truncated by construction** and is never cached, never used as an inventory,
   and never written to a manifest. (1 request.)
2. **Widen the candidate set.** Union step 1's slugs with: the GraphQL Stage-1 signal when GraphQL is
   reachable — an entity is an auth collection iff `Query` has both `me{S}` and `initialized{S}`, and
   this is the **only** source that sees all 49 collections regardless of permission; plus the
   project's own `payload.config.ts` slug literals when §4.3's upward walk found one (local FS, no
   network). If GraphQL is unreachable and no project source resolves, the candidate set is step 1's
   alone and the limitation `AUTH_CANDIDATES_TRUNCATED` is recorded.
3. **Filter to real auth collections.** For each candidate, `GET {api_path}/{slug}/init` (no auth
   header needed) and keep it iff the body has an `initialized` **boolean**. Note `/init` is itself
   access-gated — verified `/api/crm-contacts/init` → 403 — so this filter removes non-auth
   collections but cannot widen the set. Run in parallel at `--concurrency`.
4. **Test the credential.** For each surviving candidate in sorted order, `GET {api_path}/{slug}/me`
   with `Authorization: {slug} API-Key {key}` (or `Authorization: JWT {token}` in `jwt` mode, where
   the slug does not appear in the header and the first candidate returning `user != null` wins).
   **Accept the first whose `body.user != null`.** Ties are impossible by construction (first wins);
   a deterministic sort makes the choice reproducible, and the losing candidates are recorded in
   `diagnostics.auth_candidates[]`.
5. **Nothing matched.** `auth_collection_unknown` (exit 9), hint
   `--auth-collection <slug>  (candidates tried: admins, users)`. PayCLI **never** guesses `users`.

(e) **Persistence.** The resolved slug is written to `$CACHE/v1/auth-resolution.json` as
`{"<normURL>\x00<keyFP> hashed": {"auth_collection": "admins", "resolved_at": …, "cli_major": 0}}`,
mode 0600. It survives manifest expiry and cache-scope GC, so Stage -1's extra requests are paid once
per credential, not once per cold start. `pay auth logout` and `pay cache clear --all` remove it.

**Cost.** Stage -1 costs `1 + N_candidates_init + N_auth_candidates_me` requests. On the live project:
1 + 10 + 1 = 12 requests, ~0.2 s, all of them cheap REST calls. This is why the §7.1 cold budget is
stated per-profile-shape rather than as one number.

### 7.1 Cost budget

| Profile shape | Cold | Warm |
|---|---|---|
| explicit `auth_collection`, `api-key`/`jwt` | **4 HTTP requests, ~0.95 s, ~120 KB** (49 collections) | 0 requests |
| `auth_collection = "auto"`, first ever run | 4 + Stage -1 (`1 + N_candidates + N_auth`) — 16 requests / ~1.15 s live | 0 requests |
| `auth_collection = "auto"`, slug already in `auth-resolution.json` | 4 requests | 0 requests |
| `anonymous` | 3 requests (no `/me`) | 0 requests |

Warm path is a **stat + read + decode of the manifest index**, plus at most one field shard for the
collection named on the command line (§8.2). Measured with Go 1.22 `encoding/json` on the §7.8 struct
shapes: index 49 entries = 55.5 KB → **496 µs**; index 150 entries = 170 KB → **1.44 ms**; one
200-field shard = 210 KB → **1.81 ms**. So a 150-collection project resolves a targeted command in
**~3.3 ms** and `pay --help` in **~1.4 ms**.

The original "~250 µs, one consolidated file" figure was sized to this one project and was wrong by
two orders of magnitude at scale: decoding a single consolidated manifest costs 18.4 ms at 49×40
fields, 282 ms at 150×200 and 714 ms at 150×500 (§2.8) — paid on every invocation including
`pay --help`, and impossible to fit inside §9.9's 100 ms completion budget.

In development Payload rebuilds its entire GraphQL schema on every `/api/graphql` request, so every
GraphQL call costs ~0.39 s regardless of query size (verified: a 30-alias batch cost 0.391 s, a
single-type query 0.372 s). **Batch aggressively; never loop.** Never issue a full
`__schema{types}` dump — 17,361 types / ~866 KB on this project.

### 7.2 Stage 0 — connectivity and identity (2 requests, always)

1. `GET {base}{api_path}/access` with the profile's `Authorization: {authCollection} API-Key {key}`.
   * Non-2xx or non-`application/json` → `endpoint_not_payload` (exit 9) naming the resolved URL.
     A wrong base URL returns 200 `text/html` (verified), so the Content-Type check is load-bearing.
   * Record `X-Powered-By` (expect `Next.js, Payload`).
   * Record `topology_sha256` (§8.4).
2. `GET {base}{api_path}/{authCollection}/me`, **iff a credential was supplied** (`auth_mode` is
   `api-key` or `jwt`). Require `body.user != null`, else `auth_invalid` (exit 2). Store
   `identity.user_id`, `identity.can_access_admin` (the key is **absent** when false, never `false`),
   `identity.strategy`, `identity.verified = true`. **Memory only.**
   In `anonymous` mode this request is **not made**: `identity.verified = false`,
   `identity.auth_collection = null`, and the `anonymous_session` warning (§5.0) is attached to every
   envelope for the life of the process. Discovery **continues** — it does not fail.

**`api_path` autodiscovery.** Only when `api_path` was not explicitly configured. Probe the candidate
list `[configured, "/api", "/cms-api", "/payload-api", ""]` concurrently with
`GET {base}{candidate}/access`, and accept a candidate only on **200 + `Content-Type:
application/json` + a decoded object containing a `collections` key**. Status alone is useless: a
wrong base URL returns 200 `text/html` (verified). First acceptance in list order wins; the choice is
recorded as `source.api_path_source: "configured"|"probed"`. If none accepts and §4.3's upward walk
found a `payload.config.ts`, grep it for a `routes:` object literal and name the match in the
`endpoint_not_payload` hint (`--api-path /cms-api`). `graphql_path` is then **derived** from the
resolved `api_path` (§4.2), never left at a stale literal.

### 7.3 Stage 1 — entity inventory (1 GraphQL request)

```graphql
{
  q:   __schema { queryType { fields { name args { name type { kind name ofType { kind name } } } } }
                  mutationType { fields { name } } }
  acc: __type(name: "Access") { fields { name } }
}
```

Derivation, all verified against the live instance:

* `acc.fields` minus `canAccessAdmin` is `formatName(slug)` for every collection **then** every global,
  in exactly the same order as the `Query.docAccess*` fields. **Zip them index-for-index** (54 ↔ 54
  verified). This is the authoritative slug ↔ GraphQL-type map and requires **zero pluralisation
  guessing** — which matters because `media` → `Media`/`allMedia`, `search` → `Search`/`Searches`,
  and pluralize mangles real slugs (`cms` → `Cm`, `physics` → `Physic`).
* Recover the true slug by matching `slug.replace(/[-./+,()'\[\] ]/g, "_")` against the `acc` field
  name, using the slug set from `/api/access` as the candidate list.
* An entity is a **collection** iff `Query` has `count{Plural}`; it is a **global** iff its singular
  Query field has no `id` argument (`Query.Header.args == [draft, select]`, verified).
* `id_type` from the `Query.{Singular}(id: Int|String)` arg — critical, because passing a non-castable
  id causes 500s and, on `/duplicate`, unintended writes. Sets `id_type_source: "graphql"`.
* `versions` iff `Query` has `version{S}` **and** `versions{P}`.
* `auth` iff `Query` has `me{S}` and `initialized{S}`.
* `use_api_key` iff `mutation{Singular}Input` (Stage 2) contains `apiKey` **and** `enableAPIKey`
  (verified live on `mutationUserInput`). Drives §5.4's `pay doctor` reporting.
* `duplicate` iff `Mutation` has `duplicate{S}`. **This is the only duplicate detection; never probe**
  (§1 conflict 24).
* `localization` iff the plural Query field has a `locale` arg. The locale **codes** do not come from
  here — see §7.9.

**Everything in this list is a GraphQL-only fact.** When GraphQL is unavailable, each one is either
recovered by the REST fallback named in §7.6 or set to the explicit unknown value (`"unknown"` for
`id_type`, `null` for a capability flag). **Nothing in this list is ever defaulted.** See §7.6's
"Tri-state rule".

**Guard:** if `len(acc) != len(docAccess)` (possible with `graphQL: { disableQueries: true }`), abandon
the zip and fall back to per-slug `__type` verification, then normalised fuzzy matching, then mark
that entity `graphql_name: null` and run it REST-only.

**Union the result with the `/api/access` key set.** Anything in `/api/access` but not in GraphQL gets
`reachability: "graphql-disabled"`; anything in GraphQL but not in `/api/access` gets
`reachability: "access-denied"`. Verified necessary in both directions: `payload-migrations` is
REST-only, `payload-kv` is GraphQL-only.

### 7.4 Stage 2 — field schemas (1 GraphQL request, + 1 leaf batch)

One aliased request, three `__type` calls per entity:

```
o{i}: __type(name:"{Singular}")          -> output fields, relationship targets, join fields
i{i}: __type(name:"mutation{Singular}Input") -> REQUIRED-NESS (NON_NULL) and writability
w{i}: __type(name:"{Singular}_where")    -> queryable paths
```

**Required-ness comes from the input type, never the object type** — verified: `mutationPageInput.title`
is `NON_NULL String` while `Page.title` is nullable, because drafts force-nullable the object type.

A second batched request resolves the leaves referenced by the first: every ENUM (`enumValues` →
select/radio options), every `*_operator` INPUT_OBJECT (→ per-field operator list), every
`*_RelationTo` ENUM plus its matching UNION `possibleTypes` (→ polymorphic targets, same order), every
block UNION `possibleTypes`, and nested group/array object types. Stages 1 and 2's leaf batch may be
merged into one request when the entity count allows.

**Adaptive batching (replaces the original "at most 2 GraphQL requests" invariant).** Two requests is
the target, not a guarantee, because `config.graphQL.maxComplexity` (default 1000) and any
project-supplied `graphQL.validationRules` can reject a large batch. The rule:

* Start with one batch containing every alias.
* A **200 whose `errors[]` mentions complexity** (`/complexity|too complex|maximum/i`), or a response
  body over 8 MB, or any `data` alias that came back `null`, **halves the batch and retries**, up to
  **4 batches total**. Past that, record `diagnostics.degraded[] += "stage2"` and fall through to
  §7.6's REST-only path for the entities that never resolved — never a hard failure.
* Measured headroom on the live instance: a **450-alias** `__type` batch (37 KB request) returned
  200 / 529 KB / 0.47 s with no errors, so complexity is not the binding constraint at 150
  collections; the ladder exists so that a project which *has* lowered `maxComplexity` degrades
  instead of breaking.
* `diagnostics.graphql_batches` records how many were actually issued.

**Field-kind inference table** (recorded as `payload_type` + `payload_type_confidence`):

| GraphQL shape | `payload_type` |
|---|---|
| ENUM `{T}_{field}` | `select` / `radio` |
| `[ENUM]` | `select` with `has_many: true` |
| OBJECT with exactly `{docs, hasNextPage, totalDocs}` | `join` (read-only; use `joins[field][limit]`) |
| OBJECT `{T}_{field}_Relationship` with `relationTo` + `value` | polymorphic `relationship`/`upload` |
| OBJECT named as another entity's singular | monomorphic `relationship`, or `upload` if the target has `filename`+`mimeType`+`filesize`+`url` |
| `[UNION]` | `blocks` |
| scalar `JSON` | `richText` or `json` — **must be sent as an object, not a string** |
| `DateTime` / `EmailAddress` / `String` / `Int` / `Float` / `Boolean` | `date` / `email` / `text` / `number` / `number` / `checkbox` |

### 7.5 Stage 3 — REST capability probes (parallel, read-only, default on)

Worker pool of `--concurrency` (default 8). Per collection, **in this order**:

1. **501 guard first.** Any probe returning 501 with `message` starting `Cannot ` sets
   `endpoints_disabled: true` and **skips every other probe** for that collection. Without this guard
   `payload-migrations` reads as an upload positive (verified; it was the discovery researcher's
   first-pass misclassification).
2. `GET /{slug}/file/__paycli_probe__` → `upload` iff `body.message` does **not** start with
   `Route not found`.
3. `GET /{slug}/versions?limit=1&depth=0` → `versions` iff 200 **and** `docs` is an array. The
   `docs` check is mandatory: `payload-preferences` registers a custom `GET /:key` that shadows
   `/versions` and returns 200 `{"message":"Not Found","value":null}` (verified).
4. `GET /{slug}?limit=0&where[_status][equals]=published` → `drafts` iff 200 (400 `QueryError` iff not).
5. `GET /{slug}/count?where[deletedAt][exists]=true` → `trash` iff 200.
6. `GET /{slug}/count?where[folder][exists]=true` → `folders` iff 200.
7. `GET /{slug}/init` → `auth` iff the body has an `initialized` boolean.
8. `DELETE /{slug}?where[id][equals]=-1` → harvests the plural label with zero side effects
   (verified). Runs by default; `--no-labels` skips the DELETE verb entirely.

   **Fail-soft parse, because the string is translated.** The message is produced by
   `req.t('general:deletedCountSuccessfully', …)` (verified, `collections/endpoints/delete.js`), so it
   is English only because `Accept-Language: en` (§6) happens to be honoured and the project happens
   to support `en`. Parse with `^Deleted \d+ (.+) successfully\.$`:
   * match ⇒ `labels.plural` = group 1, `labels.source: "bulk-delete-message"`.
   * **no match** (translated, reworded by a custom `translations` override, or a custom endpoint) ⇒
     `labels.plural` = title-cased slug with `-`/`_` → space (`crm-contacts` → `Crm Contacts`),
     `labels.source: "derived"`, and a `LABELS_UNAVAILABLE` entry in `limitations[]`.
   * Labels are **never** left blank, `null`, or half-parsed, and no other behaviour keys off them.

Every probe validates the **response shape**, never the status code alone — custom collection
endpoints can shadow any built-in route. **"Response shape" excludes any human-readable sentence:** a
probe may match on a JSON key's presence, a value's JSON type, or an HTTP status, but the only English
substrings any probe is permitted to depend on are the two Payload emits **without** `req.t` —
`Route not found "…"` and `Cannot <METHOD> …` (both hardcoded, verified). Probe 8's label harvest is
the sole exception and it is fail-soft by the rule above.

Project-level probes (3 requests): `POST {api_path}/reorder` (404 `Route not found` → no orderable
fields), `GET {api_path}/og`, `GET {api_path}/payload-preferences/__paycli__`.

**Never probed:** `GET /payload-jobs/run` and `/handle-schedules` (they execute jobs);
`/{slug}/{id}/duplicate` (inferred from GraphQL instead); empty `POST` (opt-in only, §7.7).

### 7.6 Degradation ladder

Detect the GraphQL mode with one `POST {graphql_path}` carrying

```json
{"query":"{__typename __type(name:\"Query\"){name}}"}
```

The probe **must contain a field literally named `__type`**. Payload's introspection guard is a
validation rule that reports an error only when the query AST contains `__schema` or `__type`, and
only under `NODE_ENV=production` (verified, `@payloadcms/graphql/dist/index.js`). The original
`{__typename}` probe contains neither, so against the exact deployment the guard targets — a
production build with `graphQL.disableIntrospectionInProduction: true` — Payload 3.87's
**default** (§2.9), not merely a hardening option — it returns a happy 200 and PayCLI would record `mode: "ok"`, then watch Stages 1 and 2 both
come back 200-with-`errors[]`.

| Signal | Mode | Consequence |
|---|---|---|
| 200, `data.__typename == "Query"` **and** `data.__type.name == "Query"` | `ok` | full path |
| 200, `data` absent or `data.__type == null`, `errors[]` non-empty matching `/introspection/i` | `introspection_disabled` | data queries still work; **schema via REST** |
| 200, `data` absent or an expected alias `null`, `errors[]` non-empty, no `/introspection/i` match | `errors` | REST-only; the first error message goes in `limitations[].detail` |
| 404 with an **empty body** | `disabled` (`graphQL.disable: true`) | REST-only discovery |
| non-JSON 404 / HTML | `route_missing` | REST-only + a **path-specific** hint (below) |
| any 5xx, timeout, or non-JSON 200 | `unreachable` | REST-only |

Match `/introspection/i`, **never** the full English sentence — the message text is a
`@payloadcms/graphql` implementation detail and the rest of §7 already forbids classifying on prose.

**Universal GraphQL degrade trigger.** At *any* GraphQL stage (mode probe, Stage 1, Stage 2, Level-2
fingerprint), a **200 whose `errors[]` is non-empty, or whose expected `data` alias is `null`, is a
degrade trigger, not a failure.** PayCLI records the mode, appends
`diagnostics.degraded[] += "stage1"|"stage2"|"fingerprint"`, falls through to REST-only discovery for
whatever did not resolve, and still emits a usable manifest with the corresponding `limitations[]`.
`discovery_failed` (exit 10) is reserved for the case where **REST-only discovery also fails** —
i.e. `GET {api_path}/access` itself did not return parseable JSON. A working project must never be
made unusable by a GraphQL-layer refusal.

**`route_missing` hint.** When the mode is `route_missing` **and** `graphql_path` was derived (not
explicitly set) **and** `api_path != "/api"`, the hint names the derived candidate explicitly —
`GraphQL not found at /cms-api/graphql (derived from api_path=/cms-api + graphql_route=/graphql). If this project sets routes.graphQL, pass --graphql-path <path>.` —
because that is the exact situation in which a working GraphQL endpoint is being missed.

**REST-only discovery** yields per collection: slug + CRUD permissions + `readVersions`/`unlock` hints
(from `/api/access`); every Stage-3 capability; the plural label; field **names** and JSON types from
`GET /{slug}?limit=1&depth=0` (Payload returns every field including nulls); polymorphic `relationTo`
inline at depth 0; monomorphic relationship targets by binary search over
`?depth=1&select[F]=true&populate[<candidate-slug>][id]=true` (the populated object shrinks only when
the candidate is the real target); **and `id_type` by the ladder below.**

**`id_type` without GraphQL** (`internal/discovery/idtype.go`), in order, stopping at the first hit:

1. `GET /{slug}?limit=1&depth=0&select[id]=true` → `typeof docs[0].id`: JSON number ⇒ `"number"`,
   JSON string ⇒ `"string"`. Sets `id_type_source: "observed"`. Verified live: returns
   `{"docs":[{"id":16}],…}` in 156 B.
2. If `docs` is empty and the collection has versions: `GET /{slug}/versions?limit=1&depth=0` →
   `typeof docs[0].parent` (verified: `parent` is the **document** id, `16`, while the version's own
   `id` is `20`). Same `id_type_source: "observed"`.
3. The profile's `id_type` pin ⇒ `id_type_source: "configured"`.
4. Otherwise `id_type: "unknown"`, `id_type_source: "unknown"`, and an `ID_TYPE_UNKNOWN` limitation.

**Tri-state rule — the general form.** Every fact that only GraphQL can establish is **tri-state** in
the manifest and carries provenance:

* `id_type: "number" | "string" | "unknown"` with `id_type_source: "graphql"|"observed"|"configured"|"unknown"`.
* `flags.{upload,auth,use_api_key,versions,drafts,trash,folders,duplicate,orderable,endpoints_disabled}:
  true | false | null`, with `flags_source` per flag (`"graphql"|"probe"|"access"|"configured"|"unknown"`).
* `fields[].localized: true | false | null`; `fields[].required`, `options`, `relation_to`,
  `write_shape` likewise nullable with `*_source`.

And three consequences that are **mandatory**, because a guessed default here is PayCLI generating the
silent wrong answer it exists to prevent:

* **(a) `"unknown"` ⇒ skip the client-side check entirely** and let the server answer. `id_type ==
  "unknown"` means `pay get <coll> 66f1a2b3c4d5e6f708192a3b` issues the request; it does **not** exit 5
  `invalid_id`. The envelope carries `warning{code:"id_type_unknown", hint:"pin it once with
  `pay config set profiles.local.id_type string`"}`. A defaulted `"number"` would reject every valid
  24-hex ObjectId with an error message that is a **lie** — it says the id is malformed when PayCLI
  simply never learned the id type — and makes the collection unreachable by id. A defaulted
  `"string"` would silently lose the Postgres protection the check exists for.
* **(b) A `null` capability flag ⇒ attempt the operation.** Do not block locally. Map the server's
  404 `Route not found` / 501 `Cannot …` to `feature_unavailable` (exit 10) **after the fact**, with
  the hint `run 'pay discover --refresh'`, and record the learned value in the scope's negative memo.
  `duplicate: null` must never produce `feature_unavailable` on a project where
  `POST /{coll}/{id}/duplicate` works.
* **(c) Never synthesize a default.** There is no "sensible default" for a discovered fact; `unknown`
  is the honest value and every consumer (validation, help text, `pay explain`, completion) must
  render it as such.

Note the REST fallback is itself not universally available: the 10-of-49 live collections with zero
documents produce `FIELDS_UNAVAILABLE`, and they are exactly the collections a sample-document probe
cannot type either — which is why step 2 (versions) and step 3 (profile pin) exist, and why step 4
must be reachable rather than swept under a default.

Holes are **declared, never guessed**, via `manifest.limitations[]` with stable codes:

| Code | Meaning | Mitigation recorded in the manifest |
|---|---|---|
| `FIELDS_UNAVAILABLE` | collection has zero documents and no GraphQL (10 of 49 live collections) | try `GET /{slug}/versions?limit=1`; or `--deep --allow-write-probes` |
| `SELECT_OPTIONS_UNAVAILABLE` | enum options not enumerable over REST | `options_source: "observed"` from a `limit=200&select[f]=true` sample |
| `BLOCK_SLUGS_UNKNOWN` | the GraphQL union names are `interfaceName`s, not `blockType` slugs | resolve from project source (§7.10), the block census (§7.10c: `census-matched`), or the profile's `blocks` map |
| `CUSTOM_ENDPOINTS_NOT_ENUMERABLE` | `config.endpoints` is not introspectable | profile `custom_endpoints`, or `pay raw` |
| `SORTABILITY_HEURISTIC` | sortability inferred, not measured | `pay discover --deep --verify-sort` runs the asc/desc differential probe |
| `LOCALIZATION_UNKNOWN` | whether the project is localised at all is not detectable over REST with certainty (`locale` is accepted and ignored when it is not) | `?locale=all` differential heuristic (§7.9b); until then no locale parameters are sent |
| `RICHTEXT_SHAPE_UNKNOWN` | lexical maps to the `JSON` scalar | `pay describe <c> --field <f> --sample` prints a real document's value |
| `ID_TYPE_UNKNOWN` | no GraphQL, zero documents, no versions row, no profile pin | pin `id_type` in the profile; until then ids are passed through unchecked |
| `CAPABILITY_UNKNOWN` | one or more `flags.*` are `null` (GraphQL-only signals with no REST probe) | the operation is attempted and the server's answer is classified after the fact |
| `LABELS_UNAVAILABLE` | probe 8's message did not match the English pattern (translated or custom) | `labels.source: "derived"` from the slug; nothing else depends on labels |
| `LOCALES_UNKNOWN` | localisation is enabled but the true locale codes could not be enumerated (§7.9) | `--locale` is passed through unvalidated with `locale_unverified`; pin `locales` in the profile |
| `LOCALIZATION_PER_FIELD_UNKNOWN` | per-field `localized` is not introspectable over REST **or** GraphQL | `fields[].localized: null`; §10.2's echo-diff skips those fields |
| `BLOCK_SLUGS_UNKNOWN_NO_SOURCE` | block slugs unresolvable from the API **and** no project source found (§7.10) | the collection is marked `publishable: false` with that reason; the block census (§7.10c) learns the slugs from real documents as `observed-census` facts (they inform, never gate) |
| `AUTH_CANDIDATES_TRUNCATED` | Stage -1 could only see the anonymous slug list | pass `--auth-collection <slug>` |
| `HOOK_MUTATION_UNKNOWN` | `beforeChange`/`beforeValidate` hooks are not introspectable | `fields[].hook_mutated` is `"unknown"` until observed; profile `echo_check_ignore` |
| `PAYLOAD_VERSION_UNKNOWN` | no endpoint or header carries it and no `package.json` was found (§7.11) | version-gated hints use their unconditional wording |
| `DB_ADAPTER_UNKNOWN` | not discoverable from the API (§7.11) | `unsupported_operators` stays reactive, never pre-emptive |

### 7.7 Write-probe mode (opt-in)

`pay discover --deep --allow-write-probes` additionally runs `POST /{slug}` with `{}` to harvest the
structured required-field list with admin breadcrumb labels. It **will create a document** in any
collection that has no required fields, so it immediately `DELETE`s anything it creates and records
the ids in `diagnostics.created_and_deleted[]`. Off by default, and the flag name is deliberately
unpleasant.

### 7.8 Capability manifest

Written under `$CACHE/v1/<scope>/`, mode 0600, atomically. **Two artefact kinds**, for the reason in
§8.2: an *index* that every invocation decodes, and per-collection *field shards* that are decoded
only for the collection named on the command line.

#### 7.8.1 `manifest.json` — the index

Everything help text, `pay collections`, `pay explain --slim`, shell completion and target resolution
need. It stays ~1 KB per collection regardless of field count (measured: 55.5 KB / 496 µs at 49
entries, 170 KB / 1.44 ms at 150).

```json
{
  "manifest_version": 2,
  "generation": "01K5Q7TZ4V3B8CJK2M9N0P1Q2R",
  "cli_version": "0.1.0",
  "generated_at": "2026-09-16T17:00:00Z",
  "expires_at": "2026-09-17T17:00:00Z",

  "meta": {
    "scope": "0df77681bf2d6bfc",
    "profiles": ["local", "dev"],
    "base_url": "http://localhost:3900",
    "api_path": "/api",
    "graphql_path": "/api/graphql",
    "header_names": ["x-vercel-protection-bypass"],
    "key_fingerprint": "a1b2c3d4e5f60718",
    "created_at": "2026-09-16T17:00:00Z",
    "confirmed_at": "2026-09-16T17:00:00Z"
  },
  "fingerprint": {
    "topology_sha256": "0df77681bf2d6bfc…",
    "schema_sha256": "c0fe892dd4be665a…",
    "checked_at": "2026-09-16T17:00:00Z",
    "server_identity": "Next.js, Payload"
  },

  "source": {
    "base_url": "http://localhost:3900",
    "api_path": "/api", "api_path_source": "configured",
    "graphql_path": "/api/graphql", "graphql_path_source": "derived:api_path+graphql_route",
    "powered_by": "Next.js, Payload",
    "payload_version": "3.86.0", "payload_version_source": "project-package-json",
    "db_adapter": "postgres",     "db_adapter_source": "inferred"
  },
  "identity": {
    "auth_mode": "api-key",
    "auth_collection": "users", "auth_collection_source": "bootstrap",
    "user_id": 66, "verified": true,
    "can_access_admin": true, "strategy": "api-key",
    "key_fingerprint": "a1b2c3d4e5f60718"
  },
  "capabilities": {
    "graphql": {"mode": "ok", "introspection": true},
    "localization": {"enabled": false, "locales": [], "default": null,
                     "locales_source": "graphql-arg-absent", "fallback_default": null},
    "reorder": false, "og": true, "method_override": true,
    "jobs": {"collection": "payload-jobs", "stats_global": "payload-jobs-stats"},
    "preferences": {"collection": "payload-preferences"},
    "custom_endpoints": []
  },

  "collections": [{
    "slug": "crm-contacts",
    "labels": {"singular": "Crm Contact", "plural": "Crm Contacts", "source": "bulk-delete-message"},
    "graphql": {"singular": "CrmContact", "plural": "CrmContacts", "count": "countCrmContacts",
                "source": "access-zip"},
    "id_type": "number", "id_type_source": "graphql",
    "reachability": "ok",
    "internal": false,
    "publishable": true, "publishable_reason": null,
    "flags": {"upload": false, "auth": false, "use_api_key": false, "versions": false, "drafts": false,
              "trash": true, "folders": false, "duplicate": true, "orderable": false,
              "endpoints_disabled": false},
    "flags_source": {"upload": "probe", "auth": "probe", "use_api_key": "graphql",
                     "versions": "graphql", "drafts": "probe", "trash": "probe", "folders": "probe",
                     "duplicate": "graphql", "orderable": "probe", "endpoints_disabled": "probe"},
    "permissions": {"create": true, "read": true, "update": true, "delete": true,
                    "read_versions": false, "unlock": false, "field_level": null},
    "stats": {"total_docs": 104, "sampled": true},
    "fields_count": 31,
    "fields_sha256": "4b1f0a97…",
    "fields_shard": "fields/crm-contacts.json",
    "fields_source": "graphql",
    "title_field": "firstName",
    "date_fields": ["createdAt", "updatedAt"],
    "warnings": []
  }],
  "globals": [{
    "slug": "crm-brand",
    "graphql": {"singular": "CrmBrand"},
    "flags": {"versions": true, "drafts": false},
    "flags_source": {"versions": "graphql", "drafts": "probe"},
    "permissions": {"read": true, "update": false, "read_versions": true},
    "fields_count": 8, "fields_sha256": "9c2e…", "fields_shard": "fields/_global_crm-brand.json"
  }],
  "unreachable": [
    {"slug": "payload-kv", "reason": "access-denied",
     "detail": "present in the GraphQL schema, absent from /api/access"},
    {"slug": "payload-migrations", "reason": "endpoints-disabled", "detail": "HTTP 501 Cannot GET"}
  ],
  "limitations": [],
  "unsupported_operators": [],
  "learned_operator_failures": [],
  "diagnostics": {"requests": 7, "elapsed_ms": 947, "bytes": 410244, "graphql_batches": 2,
                  "degraded": [], "warnings": [], "created_and_deleted": [],
                  "auth_candidates": ["users"]}
}
```

`manifest_version` is **2** because the layout changed; a `manifest_version: 1` file on disk is a cache
miss, not a parse error.

#### 7.8.2 `fields/<slug>.json` — one shard per entity

```json
{
  "generation": "01K5Q7TZ4V3B8CJK2M9N0P1Q2R",
  "slug": "crm-contacts",
  "sha256": "4b1f0a97…",
  "fields": [
    {"name": "status", "path": "status", "graphql_path": "status", "parent": null,
     "payload_type": "select", "payload_type_confidence": "inferred",
     "graphql_type": "CrmContact_status", "json_type": "string",
     "required": false, "required_source": "graphql-input",
     "has_many": false, "localized": null, "localized_source": "unknown",
     "read_only": false,
     "options": ["lead","prospect","customer","churned"], "options_source": "graphql-enum",
     "relation_to": null, "relation_to_source": "n/a", "polymorphic": false,
     "write_shape": null,
     "queryable": true, "operators": ["equals","not_equals","in","not_in","exists"],
     "sortable": true, "sortable_confidence": "heuristic",
     "hook_mutated": "unknown",
     "label": "Status"},

    {"name": "company", "path": "company", "graphql_path": "company", "parent": null,
     "payload_type": "relationship", "payload_type_confidence": "graphql",
     "graphql_type": "CrmCompany", "json_type": "number|object",
     "required": false, "required_source": "graphql-input",
     "has_many": false, "localized": null, "localized_source": "unknown",
     "read_only": false,
     "options": null, "options_source": "n/a",
     "relation_to": ["crm-companies"], "relation_to_source": "graphql", "polymorphic": false,
     "write_shape": "id",
     "queryable": true, "operators": ["equals","not_equals","in","not_in","exists"],
     "sortable": true, "sortable_confidence": "heuristic",
     "hook_mutated": "unknown",
     "label": "Company"}
  ],
  "join_fields": ["activities","bookings","memberships","enrolments"],
  "blocks": null,
  "blocks_source": "unknown",
  "block_schemas": null,
  "required_paths": ["firstName", "email"]
}
```

**Every key listed above is mandatory on every field entry**, using `null` (or the documented sentinel
`"n/a"` / `"unknown"`) where it does not apply. An agent parses a field entry without presence checks;
`jq '.fields[] | select(.required)'` is total. A unit test asserts that every entry produced by
discovery has exactly the declared key set. The original spec's three example entries each had a
*different* key set and `write_shape` appeared exactly once — an agent could not rely on any of them.

**`write_shape` is a closed enum**, mandatory and non-null on every `relationship`, `upload`, and
polymorphic field, and `null` on every other kind:

| `write_shape` | Send | Inferred from |
|---|---|---|
| `id` | `4` — a bare id of the target's `id_type` | monomorphic, `has_many: false` |
| `id_array` | `[4, 7]` | monomorphic, `has_many: true` |
| `{relationTo,value}` | `{"relationTo":"posts","value":4}` | polymorphic, `has_many: false` |
| `[{relationTo,value}]` | `[{"relationTo":"posts","value":4}]` | polymorphic, `has_many: true` |
| `json` | the value verbatim | `payload_type` ∈ `richText`, `json`, `blocks` |

It is added as a row to §7.4's field-kind inference table and is what §9.10's `--set` coercion reads.

#### 7.8.3 Provenance and redaction

Every derived value carries provenance (`*_source`, `*_confidence`) so an agent knows when to trust it,
and `unreachable` + `limitations` make the gaps explicit instead of silently missing. The rule is
uniform: **a value and the story of where it came from are written together, or neither is written.**

**Never** written into any manifest artefact: `apiKey`, `apiKeyIndex`, `hash`, `salt`,
`resetPasswordToken`, `sessions`, any `/me` response body, any `[profiles.X.headers]` **value** (only
the sorted header **names**, in `meta.header_names`), and any URL that did not pass `redact.URL`.

### 7.9 Locale enumeration

Localisation is not a display detail. Three verified behaviours make an un-modelled locale a
silent-wrong-answer generator:

1. `config.localization.fallback` defaults to **true**, and `sanitizeFallbackLocale` resolves an
   absent request fallback to `localization.defaultLocale`. So `pay get pages 1 --locale de` returns
   **English** strings for every untranslated field, indistinguishable from real German content. An
   agent that read-modify-writes copies English into `de` and believes it translated the page.
2. An **unknown** locale code is silently coerced to the default locale with HTTP 200 and no warning
   (`sanitizeLocales`). `--locale de-DE` on a project whose code is `de` returns the default locale.
3. The GraphQL `LocaleInputType` enum exposes only `formatName(code)`-**mangled** names
   (`en-US` → `en_US`, `es.419` → `es_419`), and introspection never exposes the underlying `value`.
   Codes harvested that way are wrong and would be rejected by PayCLI's own validation if fed back.

**(a) Fallback default.** When `capabilities.localization.enabled` is true, PayCLI sends
**`fallback-locale=none`** on every read by default (verified accepted: `sanitizeFallbackLocale` maps
`'false' | 'none' | 'null'` → `false`), so an untranslated field comes back `null`/absent instead of
masquerading as translated. `--fallback-locale <code|default>` opts back in. The resolved pair is
always echoed as `meta.locale = {"requested": "de", "fallback": "none"}`. When localisation is
**disabled or unknown**, no locale parameters are sent at all.

**(b) Enumerating the codes**, in order, stopping at the first hit:

1. The profile's `locales` pin ⇒ `locales_source: "configured"`.
2. **`locale=all` over REST.** Pick the collection with the most fields whose value is a JSON object
   at `depth=0`, and issue `GET /{slug}?limit=3&depth=0&locale=all`. A localized field comes back as
   `{"en": …, "de": …}` — **the keys are true codes.** Take the candidate set = the intersection of
   the key sets of every field that produced the *same* key set in ≥2 fields across the sampled
   documents. Cross-validate when GraphQL is reachable: accept only if `|candidates| ==
   |LocaleInputType.enumValues|` **and** every candidate `formatName()`s to an enum name. Result:
   `locales_source: "locale-all"` (cross-validated) or `"locale-all-unverified"` (no GraphQL).
3. GraphQL enum names alone ⇒ record them as `locales_source: "graphql-enum-mangled"` and treat them
   as **not usable as codes**: they inform `capabilities.localization.locale_count` only.
4. Otherwise `locales: []`, `locales_source: "unknown"`, `LOCALES_UNKNOWN` limitation.

**(c) Validation.** `--locale` is validated client-side **only** when `locales_source` ∈
`{"configured", "locale-all"}`. Otherwise it is passed through with
`warning{code:"locale_unverified", message:"This project's locale codes could not be enumerated. Payload silently returns the DEFAULT locale for an unrecognised code (HTTP 200, no error), so a typo here is indistinguishable from a correct request.", hint:"pin them once: pay config set profiles.local.locales en,de"}`.

**(d) Per-field `localized`.** Neither REST nor GraphQL exposes it: the GraphQL object type is
identical for a localized and a non-localized field. So `fields[].localized` is `null` with
`localized_source: "unknown"` unless a `locale=all` sample proved it (`true`, source `"locale-all"`)
or the field was absent from every `locale=all` map while siblings were present (`false`, source
`"locale-all"`). The flat `"localized": false` of the original spec was a fabrication. `LOCALIZATION_PER_FIELD_UNKNOWN`
records it, and §10.2's echo-diff **skips every field whose `localized` is `null`**, because a
`locale=all` write echoes `{code: value}` maps and would otherwise produce a bogus
`input_silently_dropped` warning on every localized field — poisoning the one warning an agent is
told to act on.

### 7.10 Block slugs from project source

`blockType` slugs are not in the API when `interfaceName` is set (verified: `Page_Layout.possibleTypes`
= `CallToActionBlock, ContentBlock, MediaBlock, ArchiveBlock, FormBlock`, while `blockType` is a bare
`String` and the real slugs are `cta`, `content`, `mediaBlock`, `archive`, `formBlock`). The
documented "harvest observed `blockType` from sample documents" fallback yields nothing on a fresh
project — verified that all 12 live pages have `layout: []`.

**The answer exists on disk.** When §4.3's upward walk finds a `payload.config.ts` (or a `src/`
sibling), `internal/config/projectscan.go` scans `**/blocks/**/*.ts`, `**/*.block.ts` and
`payload.config.ts` for `slug:` string literals inside an object that also contains `fields:`,
capped at 200 files and 2 MB total, **local filesystem only, never a network call, never an import or
eval**. Matches are recorded as `blocks: ["cta","content","mediaBlock",…]`,
`blocks_source: "project-source"`, with the absolute file each slug came from in
`diagnostics.block_slug_files[]`.

Resolution order for a collection's block field: profile `blocks` map (`"configured"`) → project
source (`"project-source"`) → observed `blockType` values in sample documents (`"observed"`) →
`blocks: null`, `blocks_source: "unknown"`, limitation `BLOCK_SLUGS_UNKNOWN_NO_SOURCE`.
That is what **discovery** records in the shard. Every consumer that needs block knowledge then
merges the shard with the block census (§7.10c, `BlockKnowledge`): a slug inferred from an interface
name that no document carries is joined to the observed slug it names (`census-matched`), observed
slugs no declared source names are added as `observed-census`, and the verbatim text below is
printed only when the census has nothing either.

**When unresolved, say so in plain words and stop pointing at empty sources.** The collection's index
entry gets `publishable: false` with
`publishable_reason: "required field \"layout\" is a blocks field whose allowed blockType values cannot be determined from the Payload API for this project"`.
`pay describe <c> --field layout` prints, verbatim:

```
allowed blockType values cannot be determined from the Payload API for this project.
GraphQL exposes interfaceNames (CallToActionBlock, ContentBlock, ...), not the real slugs,
and no document in this collection has a non-empty layout to observe.
Read them from payload.config.ts / src/blocks/*/config.ts (look for `slug:`),
then pin them: pay config set profiles.local.blocks.layout cta,content,mediaBlock
```

**No hint anywhere in PayCLI may name a command that cannot answer it.** This is a hard rule, enforced
by a unit test over the hint strings: every `hint` that contains a `pay …` command is executed against
the fixture manifest in the test suite and must produce a non-empty answer for the situation that
emitted it.

### 7.10a Block field schemas — what is INSIDE a block

§7.10 answers *which* blocks a field accepts. It does not answer what one contains, and a
slug alone is not enough to construct a block: an agent that knows `cta` exists still cannot
write one.

**The shape is readable from the block's own GraphQL OBJECT type** — the union member, not an
input type. Verified live:

```
__type(name:"CallToActionBlock").fields -> richText: JSON, links: [CallToActionBlock_Links!],
                                           id: String, blockName: String, blockType: String
__type(name:"MediaBlock").fields        -> media: Media, id, blockName, blockType
__type(name:"Textarea").fields          -> name: String!, label, width: Float, defaultValue,
                                           required: Boolean, id, blockName, blockType
```

Every union member reachable from a blocks field is therefore added to §7.4's leaf set and
resolved **in the same adaptive batch as every other leaf** — one more alias each, never one
more request each — together with its nested group/array row types, bounded at three levels.
A block's `mutation{Type}Input` is deliberately NOT requested: Payload does not generate one.
Measured on the live project: 17 → 18 requests, 7 → 8 GraphQL batches, 683 KB → 698 KB.

**Required-ness is the one fact GraphQL cannot fully answer.** Payload generates no
INPUT_OBJECT for a block type (the blocks mutation argument is the `JSON` scalar), so §7.4's
`mutation{Singular}Input` NON_NULL trick has nothing to read — every INPUT_OBJECT in the live
schema was enumerated and there is no `CallToActionBlock` / `Textarea` input. Two partial
sources remain and each is labelled:

1. **`NON_NULL` on the block object type** ⇒ `required: true`, `required_source: "graphql"`.
   Payload emits it only for `required: true` (verified: `Textarea.name` is `String!`). Its
   **absence proves nothing**: a draft-enabled collection force-nullables every field beneath
   it, which is why `MediaBlock.media` is `required: true` in
   `src/blocks/MediaBlock/config.ts` and still nullable in the schema.
2. **The block's own `config.ts`**, harvested by §7.10's scan, which also records whether the
   whole `fields:` array was parseable. Only a **complete** declaration may answer `false` for
   a field it does not list; `CallToAction`'s array contains `linkGroup({…})`, a helper call
   the scanner never expands, so `links` stays unknown rather than becoming "not required".

Anything neither source answers is `required: null`, `required_source: "unknown"`, listed in
`required_unknown[]`, explained by `reason`, and surfaced as a `block_required_unknown`
warning. It is never defaulted to `false`.

`id`, `blockName` and `blockType` carry `plumbing: true` and
`required_source: "payload-protocol"`: they are Payload's wire format rather than this
project's content, and `blockType` is the only one that is mandatory to send.

The result is persisted per entity as `block_schemas`, **keyed by slug** (§7.8.2), with
`block_fields[].slug_interface_names` recording which union member each slug came from. A
shard written before the key existed decodes unchanged and reports the interiors as not
discovered — `null` there means "not discovered", never "this block has no fields".

`pay describe <c> --block <slug>` prints one block's schema; `--blocks-detail` inlines every
one of them and is **opt-in**, because measured on the live project it takes
`pay describe pages` from 34 KB to 81 KB and `pay describe pages --field layout` from 7.2 KB
to 53 KB (re-measured after §7.10b added the documentation keys; it was 31 KB → 70 KB and
4.4 KB → 43 KB before).

### 7.10b What a block IS — labels, descriptions and per-field instructions

A slug says a block *exists*. It does not say what it is **for**, and an agent choosing between `cta`,
`content` and `mediaBlock` was left inferring intent from three names. `interface_name` and a field
list do not help: both describe shape, not purpose.

**The API cannot answer this at all.** A block's `labels` are admin-UI metadata and appear in neither
a REST response nor GraphQL introspection, and Payload defines **no description field for a block
whatsoever** — verified: a `Block`'s `admin` accepts only
`components / custom / disableBlockName / group / images / jsx`, and `tsc --noEmit` rejects
`admin: { description }` on one. So the same §7.10 scan that recovers the slugs recovers the words,
from the same object literal, in the same pass — no extra file read and no extra request:

| Fact | Read from | Reported as |
|---|---|---|
| block label | `labels: { singular, plural }` | `label`, `label_plural`, `labels_source` |
| what the block is FOR | `custom.description`, else `custom.docs`, else `custom.summary` | `description`, `description_source`, **`description_key`** |
| per-field instruction | a field's `admin: { description }` (also `custom.*`) | that field's `description`, `description_source`, `description_key` |

`custom` is Payload's sanctioned arbitrary-metadata escape hatch and is **free-form**, so every
project spells this differently. Three keys are accepted and **the one that answered is always
reported**: `description_key: "custom.description"` is the difference between reporting a project
convention and implying Payload has a standard field. `admin.description` outranks the `custom.*`
neighbours because on a *field* it is Payload's own documented key; a Block cannot carry it.

**Tri-state, as everywhere else.** An absent label or description is `null` with source `"unknown"`,
never a title-cased guess at the slug — `mediaBlock` → "Media Block" is a label the admin UI never
shows. `docs_reason` says which of the two cases it is and is empty **exactly** when both were found:

* **a plugin's block** (`textarea`, from `@payloadcms/plugin-form-builder`) has no config on disk at
  all, because node_modules is deliberately never scanned. This is expected and is **stated**:
  `blockType "textarea" has no config on disk to read them from — normal for a plugin-provided block,
  which lives in node_modules — and the API publishes neither`.
* **a project block whose author wrote only half** names the declaring file and the keys PayCLI reads,
  so the fix is one line in a file the message points at.

**A project gets any of this only if its authors wrote it.** PayCLI reports what is there and invents
nothing; that sentence is in `pay help describe`, in `data.block_docs_note` and in the shipped skill,
so a `null` is never read as a PayCLI failure.

**Where it surfaces.** The rule is that the words appear **wherever the slugs appear**, because
needing a second command per candidate is the cost this exists to remove:

* `pay describe <e>` and `pay describe <e> --field <blocks-field>` gain `block_docs[SLUG]` — a compact
  `{label, label_plural, labels_source, description, description_source, description_key,
  fields_count, docs_reason}` per slug — plus `block_docs_note`. **Measured on the live project:
  `pay describe pages` 31 KB → 34 KB, `describe forms` 25 KB → 30 KB (9 plugin blocks, each carrying
  its reason), `--field layout` 4.4 KB → 7.2 KB.** The field *schemas* stay behind `--blocks-detail`
  (70 KB → 81 KB) exactly as before: this key carries what is needed to CHOOSE, never what is needed
  to CONSTRUCT.
* `pay describe <e> --block <slug>` prints the full schema (label/description on `.block`), repeats
  the compact form as `.docs` so `--path .docs.description` works, and lists
  `documented_fields[]` — every field of that block carrying an instruction.
* `pay explain --collection <slug>` gets the same keys.
* shell completion of `--block` renders the label/description as the candidate's description, so
  `pay describe pages --block <TAB>` shows what each block is for.

**Ordinary collection fields get the same treatment**, because `admin: { description }` is one
mechanism documenting both and a per-field instruction ("Pass a media document id") is precisely what
an agent needs while filling a field in. The scan therefore also reads `**/collections/**` and
`**/globals/**`, with two constraints:

1. **A collection slug is never a block slug.** A `CollectionConfig` and a `Block` are the same object
   literal to a byte scanner — both carry `slug:` and `fields:` — so the candidate's *directory*
   decides which list its decls join. Without that, `pages` would be offered as a `blockType` and
   Payload would silently drop the row (§9.7).
2. **Top-level `fields:` only.** A field inside a group, an array or a tab is not reached and is
   reported as undocumented rather than given a dotted path the scanner would have had to guess.

They are published as **`field_docs[PATH]`** — a separate map, deliberately *not* three more keys on
each field entry: `pay describe pages` carries ~90 field entries, so that would have added **~16 KB**
to the most-run command in order to publish `null` 90 times. `documented_paths[]` lists them,
`field_docs_file` names the file, and `--field PATH` answers for one field as `field_doc`. §7.8.2's
fixed 26-key field entry is unchanged.

`field_docs_source` is a three-way answer and each value is a different fact:
`"project-source"` with entries (read the config, here is what it says), `"project-source"` with none
(read the config, nobody documented anything), `"unknown"` (never saw a config — the normal case for
a plugin-provided collection such as `forms`, whose config lives in node_modules). Collapsing the
middle case into the last would turn a project's deliberate silence into a PayCLI failure.

### 7.10c The block census — blocks learned from the live site

§7.10 answers *which* blockTypes a field accepts and §7.10a *what is inside* one, but both need
GraphQL introspection or the project's source on disk, and neither is dependable (§2.9):

* **Introspection is off by default in production.** Payload 3.87.1 declares
  `graphQL.disableIntrospectionInProduction` `@default true`, and `NoProductionIntrospection`
  rejects every `__schema`/`__type` field while `NODE_ENV === 'production'`
  (`GraphQL introspection is not allowed, but the query contained __schema or __type`). On the
  klixpert stage discovery is therefore REST-observed: `pay describe pages` lists 13 fields, `layout`
  is `payload_type: "unknown"` (it was `[]` in the three sampled documents), and no block slug or block
  schema is published.
* **The app's source is not on the machine running PayCLI** in general (§7.10's scan finds nothing).
* **Even with introspection on, slugs can be wrong.** Without project source, §7.10 infers
  `callToActionBlock` from the union member while the real slug is `cta`, and Payload drops the row
  with a 200 (§2.9).

Real documents are always available, and a depth-0 REST read returns **every key of every block row,
nulls included** — the rows *are* a schema, measured rather than declared. The census reads them.

#### 7.10c.1 What is read

`CensusTargets(manifest, shards, only)` chooses the entities:

* An entity with a **GraphQL-sourced** shard is read only if some field is `payload_type: blocks`;
  `select` is narrowed to those fields' top-level roots.
* Any other entity (REST-observed shard, or none) is read with `select` narrowed to the top-level
  fields that could hold a blocks array — `blocks`/`array`/`group`/`unknown` kinds and anything whose
  JSON type is array/object/unknown, except rich text, relationships and join fields. An entity with no
  such field is skipped unless named explicitly.
* Internal collections, `access-denied` entities and endpoint-disabled collections are skipped unless
  named. Globals are read whole (one document; `select` saves nothing).

Per entity: `GET /{slug}?depth=0&draft=true&limit=100&page=N&select[…]=true` (`draft` omitted when the
entity is known to have no drafts), up to `max_docs` documents (default **500**;
`[defaults] census_max_docs`, `PAY_CENSUS_MAX_DOCS`, `--census-max-docs N`).

* **Paging** uses one `limit` for every page of an entity (100, or `max_docs` when smaller), because
  Payload's offset is `(page-1)*limit` (§2.9); the budget is enforced by consuming only as many
  documents of a page as it has left, and a document is counted once by id.
* **Early stop.** After a full page (100 documents) of the NEWEST documents (Payload's default order)
  without a single block row, the census reads ONE more page from the other end (`sort=id`) before
  concluding anything — a migration that creates empty stub pages first makes the newest page of a
  blocks collection blockless (stage: pages 52–69, the 17 newest, had empty layouts). If that page has
  block rows too the read continues; otherwise it stops (`stopped_early: true` only when documents
  remain unread). Stage `media` (848 docs) costs 2 requests.
* **Relationship probe.** Up to **6** documents are re-read at depth 1, chosen greedily to cover every
  (blockType, field) holding an id-like value (integer, ObjectID, UUID) at least once. A value that
  became an object with `id` is a `relationship`, and an `upload` when it has `filename`/`mimeType`
  (`kind_source: "observed-depth-1"`).
* Entities are read ≤ 4 at a time. A failing entity is kept with `error` set, never fails the run,
  and the previous good census of that entity is kept instead of the failure.

Measured on stage: `pages` (41 docs, 487 block rows, 37 types) costs **7 requests** (1 list + 6
depth-1); answering from the cache costs **0** (`meta.http_requests: 0`).

#### 7.10c.2 What is recorded — `$CACHE/v1/<scope>/census.json`

```
census_version, scope, cli_version, updated_at,
collections|globals: { <slug>: {
  slug, kind, built_at, draft, select[], docs_scanned, total_docs, truncated, stopped_early,
  max_docs, requests, relation_probe (ok|none|failed|skipped), error, block_rows, folded_docs?,
  doc_keys[] (ids the read counted; "_global" for a global), folded_keys[]? (ids folded in since),
  paths: { <pattern>: {path, parent, parent_types{type:n}, field, types{type:n}, rows, docs,
                       empty_rows, untyped_rows} },
  types: { <blockType>: {slug, count, docs, used_in{pattern:n},
           fields: { <key>: CensusField },
           instances: [ {kind, slug, id, path, score} ] } } } }

CensusField = {kind, kind_source, types{json-type:n}, present, non_null, non_empty,
               values[{value,count}], distinct, distinct_capped, enum_like,
               accepts{type:n} (nested blocks), items (array/blocks rows),
               fields{…} (group keys / array-row keys),
               optional? (a fold of an already counted document held it empty: never likely)}
```

* **Path patterns** erase indices: `layout`, `layout[].blocks`, `layout[].blocks[].blocks`,
  `layout[].blocks[].columns[].blocks`. Each nested pattern records `parent_types`, so a parent-scoped
  `group.blocks` is answerable from `types.group.fields.blocks.accepts`.
* `kind` ∈ `blocks | array | richText | group | relationship | upload | list | scalar | unknown`; the
  most informative observation wins (one filled blocks array outweighs any number of `[]`). Rich-text
  internals are never walked. `id` and `blockType` are never profiled; `blockName` is.
* **Values:** ≤ 12 per field, strings ≤ 120 chars, ≤ 64 distinct counted before `distinct_capped`.
  For a field that is not enum-like only **repeated** values are kept — a value seen once is content,
  not vocabulary. Booleans keep their values (for defaults).
* **`enum_like`** is deliberately conservative (a false enum turns every new value into a warning):
  scalar, ≥ 4 non-null observations, **2..12** distinct values, distinct × 2 ≤ non-null, not boolean,
  **never a number** (Payload stores select values as strings; the form builder's `width` is a
  number that merely happens to be 50 or 100), and every value an identifier — token-like (≤ 40 chars,
  no whitespace) and **not link-like** (`LinkLikeValue`: a leading `/`, `./`, `../`, `#` or `?`; `://`;
  `@`, `(`, `)`; a URI scheme; a trailing file extension or domain). A ratio such as `1/1` or `50%`
  stays an identifier. The check is re-applied when a census is **used** (`CensusField.IsVocabulary`),
  so a census written by an older build cannot keep producing false findings.
* **Instances:** the 8 most complete rows (score = non-empty keys), as references only.
* **§8.3 rule 4 still holds.** The census is an aggregate — counts, JSON types, short repeated scalars
  and document references. It stores no document; `pay blocks example` fetches the referenced rows
  live. Class `block_census`: fresh 24 h, hard max 7 d, persistable (§8.5). Size on stage: 315 KB
  pretty-printed for 28 entities.
* **Read-modify-write under the scope lock** (`Store.UpdateCensus`): the entities this invocation read
  are merged into the census as it is on disk at that moment (per entity, the newer `built_at` wins),
  because agents run tools in parallel (stage: `learn ctas & learn forms & learn pages` kept only
  `pages` in 3 of 3 runs before; all three now). `--refresh` replaces the whole file. Verified on
  read (`census_version`, `scope`); anything else is a miss plus `cache_unreadable`. Removed with the
  scope by `pay cache clear` and GC. `census.json` is not part of the discovery set (not
  generation-bound, §8.2).
* **Census-shaped redaction.** The file is written through `censusJSON`, not the document-shaped
  `redact.JSON`: the census keys its maps by schema **names** (field names, slugs, patterns), and key
  redaction turned the profile of a field named `hash`, `csrfToken` or `clientSecret` into a string,
  after which the file no longer decoded. Under a secret-looking key an object keeps its structure and
  loses only its `values`, a string is masked, counts are kept; any JWT-shaped string is masked. The
  builder never samples scalar values of a field whose name matches `redact.Key`. A census whose
  header matches but whose body does not decode is a miss **plus** `cache_unreadable` (hint
  `pay blocks learn`), never a silent miss.

#### 7.10c.3 Merging with declared facts — `BlockKnowledge`

Every fact keeps the list of sources that support it. For a top-level blocks field:

1. The shard's declared answer (`block_fields[path]`, or the legacy `blocks`/`blocks_source` pair).
2. Each declared slug a document carries gains `observed-census`.
3. **Census matching:** a slug inferred from a GraphQL interface name that no document carries is
   joined to an observed slug the declared list lacks when exactly one matches by name — equal, equal
   without the `Block` suffix, or the interface's initials (`CallToAction` → `cta`). The joined slug is
   labelled `census-matched` and reaches the union member's §7.10a block schema. A mere prefix is never
   a match (`text` ≠ `Textarea`).
4. Observed slugs no declared source names are added with `observed-census` only.

`authoritative: true` — the only state in which "not in the list" is an **error** — requires a profile
pin, **or** a project-source list with no unresolved member, **or** a GraphQL union whose every member
resolved to a confirmed slug (project source, observation or census match) with no observed extra.
Nested patterns are always observation (GraphQL never reaches a nested union), so never authoritative.
Verified on localhost: `layout` → `archive, content, cta` (`census-matched`), `formBlock, mediaBlock`
(`inferred-from-interface-name` + `observed-census`), `authoritative: true`.

**A slug is not globally unique.** The form builder's `text` field block and a page's `text` content
block share a slug and nothing else (stage: `forms` and `pages`). Type statistics therefore come from
the entity's own rows; another entity's rows of the same slug are merged only when ≥ 80 % of the union
of their keys is shared (`CompatibleTypes`), and a cross-entity answer that drops an incompatible
shape warns `block_type_shapes_differ`. Validation uses another entity's statistics for a type this
entity never used only when the row fits that shape (≥ 80 % of its own keys are keys the foreign type
carries); otherwise the row gets no observation-based finding at all.

**Required-ness** in the census view is `required` ∈ `"true" | "false" | "likely" | "unknown"` with
`required_source`. `"true"`/`"false"` only from a declared source (GraphQL NON_NULL, project source;
`blockName` is `"false"`/`payload-protocol`). **`"likely"`** (`observed-census`) = non-null and
non-empty in every one of ≥ 3 observed rows, for varying scalars (≥ 2 distinct), rich text,
relationships and uploads — never checkboxes, enum-like fields, constants, or numbers set to one of a
few values, because a `defaultValue` fills those whether or not the field is required.

#### 7.10c.4 Validation — `ValidateBlocks`

Every block row at every depth of a document or write body:

| code | severity | when |
|---|---|---|
| `empty_row` | error | a `{}` entry in a blocks array (the Postgres orphan of §10.2.1) |
| `missing_block_type` | error | a non-empty row with no blockType |
| `invalid_row` | error | a non-object entry in a blocks array |
| `unknown_block_type` | **error** if the accepted set is authoritative; **warning** if the census read this entity, saw block rows in it, and the slug occurs nowhere in the project | Payload silently drops such a row |
| `block_type_not_observed_here` | warning | the census saw block rows in this entity, and the slug exists in the project but never at this position: for a nested row whose parent type's field has observed rows, the **parent-scoped** set (`group.blocks`) is the observed set — it replaces, not widens, the depth pattern (`layout[].blocks` is the union of every parent at that depth, so it would admit a `bentoItem` into a group); otherwise the depth pattern |
| `unknown_field` | warning | a key never observed on that type and absent from every declared schema — Payload silently drops unknown fields |
| `value_not_in_enum` | **error** if outside GraphQL enum options; **warning** if outside an observed enum-like vocabulary | |
| `required_field_missing` | error | a declared-required field missing or null |
| `likely_required` | warning | a `likely` field empty |

**Rule:** an error needs a declared fact or Payload's wire format; an observation can only ever produce
a warning; with no fact at all nothing is said. An entity whose census holds **no block row** is "no
fact": neither placement finding is made against it. Each issue carries `path`
(`layout[2].blocks[1].textSize`), `block_type`, `message`, `hint` (a runnable `pay` command) and
`source`. Verified on stage (`create --dry-run`, nothing written): real types silent; `heroo` →
`unknown_block_type` with `did you mean "hero"?`; `headline` on text → `unknown_field`;
`textSize: "huge"` → `value_not_in_enum` (observed: regular, large); a hero inside `group.blocks` →
`block_type_not_observed_here`; `{}` → `empty_row`; a `bentoItem` added to a group's `blocks`
(`--field 'layout[type:group[0]].blocks'`, `group.blocks` or the document) → `block_type_not_observed_here`
against `group.blocks`, as `pay blocks ls` reports it.

**`pay blocks new … | pay blocks validate`.** `blocks new --collection C` sets the envelope `target`
to C, so validate checks against C's census without repeating `--collection`. For a `blocks new` row
without `--field`, validate checks the row at the most-used position in its `used_in` (and takes the
entity from it when no target is known) instead of `layout`: stage `blocks new bentoItem | blocks
validate` → `layout[].blocks[].blocks`, no `census_unavailable`, no spurious
`block_type_not_observed_here`.

**Nested array kind.** Whether an array inside a row is a blocks field is decided by what the field
IS — a declared `blocks`/`array` type, else the census kind when real rows were observed (`items > 0`)
— and only for an unknown field by its content (a row with a blockType). A stray `blockType` on a row
of a plain array (`hero.buttons`) is therefore `unknown_field` on that key (Payload drops the key, not
the row), never `missing_block_type` on the untouched real rows (stage home page: previously exit 5).

#### 7.10c.5 When the census is (re)built

* **Lazily, by the catalog verbs** — `pay blocks types | schema | example | new | validate` and
  `pay blocks add --template` (§9.2) — for the entities they need, reusing a cached entity when fresh
  (< 24 h), read without error, and read with at least the requested `max_docs` (or complete).
* **Scope of the per-type verbs.** `schema | example | new <type>` without `--collection` answer from
  the cached census first: the entities whose cache (read without error, within the 7-day hard max)
  saw `<type>` are the scope; they are reused when fresh (0 requests) and only they are rebuilt when
  stale (`data.census.scope`: `narrowed: …`). If the rebuilt holders no longer carry the type, the
  verb falls back to the wide scan before answering `block_type_unknown`.
* **Wide scan** (no cached holder; `pay blocks types` without an entity): every `CensusTargets`
  entity, except that an entity whose cached census held **no block rows**, was read completely (or
  `stopped_early`) without error within the hard max, and whose shard declares no blocks field is
  reused even when older than 24 h. When a wide scan made requests it warns `census_scope_wide`
  ("--collection was not given, so the block census read N of M entities that can hold blocks …"),
  hint `pay blocks schema <type> --collection <entity>`. Measured on stage: `blocks schema hero`
  with a 48 h-old census 35 → **4** requests; `blocks types` 35 → **10**.
* **`pay blocks learn [ENTITY…]`** forces a rebuild and reports the difference to the previous census
  (`grid gained splitDesktop`, `pages: layout now also holds "statement"`). Absence proves removal only
  for a complete read: when the new read is truncated or failed, what it lacks is `change: "not_seen"`
  ("… was not seen in this partial read (7 of 41 documents …); it may still be in use"), never
  `removed`; additions after a partial previous read say they "may not be new". A complete read with
  fewer documents (pages deleted) still reports `removed`. Stage: `learn pages --census-max-docs 7`
  → 45 × `not_seen`, 0 × `removed`.
* `--refresh` rebuilds the entities the command needs and discards the rest; `--no-cache` builds in
  memory only (`persisted: false`).
* **Reactive rebuild (validate).** When validation against a cached census yields an `observed-census`
  finding of kind `unknown_block_type`, `block_type_not_observed_here`, `unknown_field` or
  `value_not_in_enum`, the entity is rebuilt once and validated again (`census.revalidated: true`).
  Verified on stage: a deploy added four block types mid-session; the rebuild cleared the findings.
  **Cooldown:** an entity read less than **10 minutes** ago is not rebuilt again — in an edit-and-validate
  loop the anomaly is almost always the edit itself; `data.census.revalidate_skipped` says so and names
  `pay blocks learn <slug>`.
* **Reactive re-read (example).** `pay blocks example` / `blocks new --from-example` against a cached
  census whose references all moved re-read the holding entities once (`census.revalidated: true`).
* **Writes never build it** (§9.7) but fold their echoes into it. `pay describe` builds it (one
  entity) only when the GraphQL mode is not `ok`; otherwise it uses a cached census if there is one.
  Pipe stages (`pay blocks ls|mv|…`, `pay outline`) and completion only ever read a cached one.

Every catalog envelope carries `data.census = {source: cache|built|mixed, requests, max_docs,
persisted, ttl_s, revalidated?, revalidate_skipped?, scope?, entities[{kind, slug, built_at, age_s,
from_cache, draft, docs_scanned, total_docs, truncated, stopped_early?, max_docs, requests,
relation_probe, block_rows, block_types, error?}], note}`. Warning codes: `blocks_unchecked`,
`census_entity_failed`, `census_truncated` (a read cut short by the budget), `census_unavailable`,
`census_scope_wide`, `example_moved`, `block_type_shapes_differ`, plus the §7.10c.4 codes when a
write reports them.

#### 7.10c.6 `pay describe` answers from the census

* `pay describe <e> --field PATH` answers from the census when the shard cannot: a nested pattern
  (`layout[2].blocks`, `layout[].blocks`, `group.blocks`), or a top-level field with no declared slugs
  (stage `layout` is `payload_type: unknown`). Output: `field` (synthesised
  `{path, payload_type: blocks, payload_type_source: observed-census}` when not in the shard),
  `block_types`, `blocks_source: observed-census`, `block_type_sources`, `blocks_reason`, `accepted`,
  `blocks_help`, `census`.
* When the declared slugs are inferred/mixed and the census reconciles them, `block_types`,
  `blocks_source`, `block_type_sources` and `accepted` are replaced and the declared list is kept as
  `declared_block_types` (localhost: `callToActionBlock…` → `archive, content, cta, formBlock,
  mediaBlock`, `blocks_source: graphql`).
* `pay describe <e> --block TYPE` with no declared schema for TYPE answers with the census view in
  `.block` (the shape of `pay blocks schema`), `block_source: observed-census`, `block_fields`,
  `required_fields`, `likely_required`, `examples` (`pay blocks example|new …`).
* `--block TYPO` → `invalid_option` whose `did_you_mean` draws on the census types too, and — when
  the schema names no slug — the hint `pay blocks types <e>` (stage `--block heroo` → `["hero"]`).

### 7.11 `payload_version` and `db_adapter`

Neither is obtainable from the API. Verified: `X-Powered-By: Next.js, Payload` carries no version,
`/api/access` has exactly the keys `["canAccessAdmin","collections","globals"]`, and there is no
version endpoint. Both are therefore **nullable with provenance**, and nothing may fabricate them.

Schema (`|` denotes the permitted value set, not literal JSON):

```
"payload_version":        string | null
"payload_version_source": "project-package-json" | "configured" | "unknown"
"db_adapter":             "postgres" | "mongodb" | "sqlite" | "unknown"
"db_adapter_source":      "configured" | "inferred" | "unknown"
```

A concrete example, from the live project:

```json
{"payload_version": "3.86.0", "payload_version_source": "project-package-json",
 "db_adapter": "postgres", "db_adapter_source": "inferred"}
```

**Sources, in order:**

1. The profile keys `payload_version` / `db_adapter` ⇒ `"configured"`.
2. When §4.3's upward walk found a `package.json`, read the resolved `payload` version from it —
   **local filesystem only, never a network call** (verified: `dependencies.payload` = `3.86.0`).
   ⇒ `"project-package-json"`.
3. **Inference for the adapter only:** every collection's `id_type == "number"` ⇒ a relational
   adapter (`"postgres"`, `db_adapter_source: "inferred"`); `id_type == "string"` with 24-hex sample
   ids ⇒ `"mongodb"`; `id_type == "string"` with UUID-shaped ids ⇒ `"postgres"` (relational with
   UUID pks); anything else ⇒ `"unknown"`.
4. Otherwise `null` / `"unknown"` plus `PAYLOAD_VERSION_UNKNOWN` / `DB_ADAPTER_UNKNOWN`.

**`unsupported_operators` is no longer derived from an assumed adapter.** It initialises **empty** and
is populated *reactively*: a 500 or a `QueryError` provably caused by operator `O` on collection `C`
appends `{"operator":"all","collection":"crm-contacts","evidence":"HTTP 500 Something went wrong.","learned_at":…}`
to `learned_operator_failures[]` in the scope cache, and the **next** use of that operator on that
collection emits `warning{code:"operator_previously_failed"}` before sending. The client-side
**block** (§9.3's `unsupported_operator`, exit 5) applies **only** when
`db_adapter == "postgres" && db_adapter_source == "configured"`.

Why: on MongoDB `all` maps to `$all` and works, so the original hardcoded list
`["all","near","within","intersects"]` would have made PayCLI refuse a query the server would have
answered — the CLI inventing a failure. On Postgres it 500s (verified). Learning beats guessing, and
an explicit profile pin beats both.

**When `payload_version` is unknown:** §19.6's exit-7 hint uses its **unconditional** wording
("assume the successful writes committed"), and any "minimum supported Payload version" gate is
**advisory only** — it emits `warning{code:"payload_version_unknown"}` and proceeds. Refusing to run
against an unknown version would make PayCLI fail on every project it cannot introspect, which is the
opposite of the portability this document exists to guarantee.


## 8. Cache

### 8.1 Scope key

```
scope = sha256hex( normURL + "\x00" + graphqlPath + "\x00" + keyFP + "\x00" + headersFP )[:16]

normURL   = lowercase(scheme) + "://" + lowercase(host) + [":" + port if non-default] + api_path
            // userinfo is stripped by config.Resolve before this point (§4.2)
keyFP     = sha256hex("paycli-key-v1\x00" + credential)[:16]   // or the literal "anon" when none
headersFP = sha256hex( "\n".join(sorted("name:value" for every [profiles.X.headers] entry,
                                        AFTER ${ENV} interpolation)) )[:16]
            // or the literal "none" when no extra headers are configured
```

`credential` is the API key in `api-key` mode, the JWT in `jwt` mode, absent in `anonymous` mode.
It is **hashed, never stored**; `headersFP` is a hash too — header **values** are never written
anywhere (§5.3), only the sorted header **names**, into `meta.header_names`, so `pay cache ls` can
explain why two profiles do or do not share a scope.

**`authCollectionSlug` is deliberately NOT an input.** It is a *derived function* of
`(normURL, credential)`, not an independent credential — §7.0 resolves it *from* those two. Including
it made the scope path uncomputable until after the answer it stores was already known, so an `auto`
profile could never cache its discovery and re-paid 0.95 s on every cold start. The resolved slug
lives in `identity.auth_collection` inside the manifest, and in `$CACHE/v1/auth-resolution.json` so it
survives manifest expiry.

**`graphql_path` and the extra-header set ARE inputs**, for §8.1's own stated reason. `graphql_path`
determines `capabilities.graphql.mode` and every GraphQL-derived field schema, so a scope that does not
record which endpoint produced them would let one profile silently reuse another's answer. Extra
headers can be **access-granting** — §4.2's own example is
`"X-Vercel-Protection-Bypass" = "${VERCEL_BYPASS}"` — so two profiles with identical `base_url`,
`api_path` and key but different header sets see *different projects*, and sharing a scope would make
the bypass-less one validate against collections and fields it cannot reach, surfacing as unexplained
4xx/HTML instead of a permissions problem.

A table test enumerates scope equality/inequality for: same key + different header set; same key +
different `graphql_path`; same key + different `api_path`; different key; same everything + different
profile name (must be **equal**); and `auto` vs an explicit `auth_collection` that resolves to the
same slug (must be **equal**).

The profile **name is deliberately not in the hash** — two profiles pointing at the same server with
the same key share one 0.95 s discovery instead of duplicating it. Profile names are recorded in
`manifest.json` → `meta.profiles[]` for `pay cache ls`.

This binding is mandatory: verified that `/api/access` returns 8,610 B / 10 collections + 2 globals
unauthenticated and 30,585 B / 49 collections + 5 globals authenticated. A cache key that did not bind the
credential would poison discovery with a truncated collection list and make whole collections appear
not to exist.

The `api_path` is in `normURL` because two Payload projects on the same host can use different
`routes.api` prefixes.

### 8.2 Layout

```
$CACHE/v1/auth-resolution.json          §7.0(e) — resolved auth-collection slug per (normURL,keyFP)
$CACHE/v1/<scope>/manifest.json         the index (§7.8.1) — INCLUDES `meta` and `fingerprint`
$CACHE/v1/<scope>/fields/<slug>.json    one field shard per entity (§7.8.2)
$CACHE/v1/<scope>/fields/_global_<slug>.json
$CACHE/v1/<scope>/graphql/<Type>.json   per-type raw introspection, a pure derived blob cache
$CACHE/v1/<scope>/census.json           §7.10c block census — an aggregate, own header, NOT generation-bound
$CACHE/v1/<scope>/.lock
```

`v1` is a cache-format epoch. Any directory under a different `vN` older than 30 days is deleted
opportunistically.

**Index plus shards, not one consolidated file.** The original decision (one file; "749 µs vs 1,198 µs
for 57 files") was measured on a manifest for *this* project and does not survive scale. Re-measured
with Go 1.22 `encoding/json` on §7.8-shaped structs (§2.8): one consolidated manifest costs **18.4 ms**
at 49 collections × 40 fields, **282 ms** at 150 × 200 and **714 ms** at 150 × 500 — paid on *every*
invocation, including `pay --help`, and flatly incompatible with §9.9's 100 ms completion budget.
Splitting it:

* **`manifest.json` is what almost everything needs** — help text, `pay collections`,
  `pay explain --slim`, target resolution, completion. 55.5 KB / **496 µs** at 49 entries, 170 KB /
  **1.44 ms** at 150, 567 KB / **4.79 ms** at 500.
* **Field shards are loaded lazily and only for the collection named on the command line.**
  `pay find pages --where 'title eq x'` reads exactly one shard (210 KB / **1.81 ms** for 200 fields).
  `pay explain --full` is the only command that loads all of them, and it is the one command where
  paying for it is the point.
* `pay explain`, `pay describe` and `--full` output are unchanged from a caller's perspective; the
  split is an on-disk layout, not an API.

**`meta` and `fingerprint` are folded INTO `manifest.json`** as top-level objects. This is not
cosmetic: it eliminates the torn-read class described in §8.7, because one atomic rename then
publishes the entire consistent set. `confirmed_at` lives in exactly one place —
`manifest.json` → `meta.confirmed_at` — and that is the authoritative value §8.7's GC reads. The
original spec listed `confirmed_at` in both a separate `meta.json` and a separate `fingerprint.json`
with no statement of
which wins, so two correct implementations would GC different scopes.

**Every artefact written by one discovery run carries the same `generation`** (a ULID minted once per
run), including each `fields/<slug>.json` and each `graphql/<Type>.json`. On read, PayCLI loads the
index first and treats a shard whose `generation` **or** `sha256` does not match the index's
`fields_shard`/`fields_sha256` entry as a **miss for that entity only** — it re-discovers rather than
mixing two runs. `graphql/` is keyed by `schema_sha256` as well, so it can never disagree with the
manifest that references it.

**A CI benchmark asserts warm-path resolution stays under 5 ms** for a synthetic 150-collection ×
200-field manifest: index decode + one shard decode + target resolution. It fails the build, not a
report.

**Format is plain, pretty-printed, key-sorted JSON.** No gob. (§1 conflict 9.)

### 8.3 What may be cached

`internal/cache.cacheable(req, resp) (bool, reason)` is the **only** function authorised to write to
disk. Call sites never decide.

Hard never-cache-to-disk:

1. Any non-`GET` response.
2. Any status other than 200 — including 501 (endpoints-disabled) and 500. Negative results live in
   the in-memory negative memo only, because a cached 501 would permanently hide a collection that the
   project later re-enables.
3. Any `Content-Type` other than `application/json`. A wrong base URL returns 200 `text/html`
   (verified), and a 22,729-byte Next.js error page must never be stored as a discovery result.
4. **Document data of any kind** — find, findByID, count, versions, global content. Payload content is
   mutable by definition, and stale content served to an agent causes wrong edits. The block census
   (§7.10c) is not document data: it stores counts, JSON types, short *repeated* scalar values and
   document references, never a document, and `pay blocks example` fetches referenced rows live.
5. Any `/api/{auth}/me` response (plaintext API key, verified).
6. Anything fetched anonymously into an authenticated scope (structurally impossible given the
   `keyFP="anon"` sentinel, but asserted).
7. Any response obtained through a redirect that changed host.
8. Any body that did not survive a full `io.ReadAll` plus a strict `json.Unmarshal` into the expected
   shape — responses are `Transfer-Encoding: chunked` with no `Content-Length`, so a truncated body can
   still look like valid JSON.

Must-do: every entry embeds its own identity (`scope`, `generation`, `method`, `url`, `status`,
`content_type`, `fetched_at`, `ttl`, `cli_major`) and it is **verified on read**; a mismatch is a miss
and the entry is deleted. `url` is stored only after `redact.URL` (§5.3).

**Any I/O error on the cache read path is a miss, never a command failure.** `ENOENT`, `EACCES`, a
Windows sharing violation, a short read, malformed JSON, a `manifest_version` PayCLI does not know, or
a generation mismatch all produce a cache **miss** plus
`warnings[] {"code":"cache_unreadable","message":"<path>: <errno>","hint":"pay cache path"}`. §4.1's
promise that `rm -rf $(pay cache path)` never breaks the CLI is only true if every read path obeys
this.

### 8.4 Invalidation

There is nothing to piggyback on — verified: no `ETag`, no `Cache-Control`, no `Last-Modified`,
`If-None-Match` and `If-Modified-Since` both return 200, and `HEAD /api/*` is 404. So:

**Level 1 — topology fingerprint** (identity-scoped, 14 ms / 30 KB).
`topology_sha256 = sha256` over a normalised projection of `GET /api/access`:

```json
{"c": {slug: sorted(keys(entry))}, "g": {slug: sorted(keys(entry))}, "admin": canAccessAdmin}
```

Only key **sets** are hashed, not values, so a permission flip still changes it (a collapsed entry has
a different key set) while transient data does not. Detects: collection/global added or removed,
versions enabled/disabled (`readVersions` appears), and access-control changes for this identity.

**Level 2 — schema fingerprint** (identity-independent, ~0.39 s / 5.9 KB).
`schema_sha256 = sha256` over the sorted field names of `{__type(name:"Query"){fields{name}}}`.
This is byte-identical authenticated and unauthenticated, making it the only permission-free
schema-version proxy in the API.

**Known blind spot, stated honestly:** neither level detects "a field was added to an existing
collection" — Level 1 cannot because `fields` collapses to `true` for a privileged key (verified), and
Level 2 cannot because the Query field list does not change. Therefore:

**Level 3 — reactive invalidation.** A response that is 400 with
`{"errors":[{"name":"QueryError","data":[{"path":"…"}]}]}`, or 404 `Route not found "…"` for a slug the
manifest claims exists, or 501 `Cannot <METHOD>`, is **proof** the cached schema is stale. Three
constraints make this safe to implement:

**(a) Level 3 is opt-in per request, never ambient.** It fires only when the request context carries
`payload.WithReactiveInvalidation(ctx)`, which is set **only** by user-facing command handlers in
`internal/cli`. `internal/discovery` never sets it, so every Stage-3 probe is exempt *by
construction*. This is mandatory, not a tidiness preference: discovery **deliberately generates every
Level-3 trigger** — §7.5 probe 2's upload negative *is* a body starting `Route not found` (47 of 49
live collections produce it), probe 4's no-drafts negative *is* a 400 `QueryError` on `_status`, and
probe 1's entire purpose is catching 501 `Cannot GET` on `payload-migrations`. An ambient rule in the
transport turns one `pay discover` into dozens of nested scope invalidations, each of which
re-triggers them — unbounded recursion, or at best exactly the request storm §9.1 says PayCLI must
never create against a dev server. §3.1's arch-lint fails the build if
`WithReactiveInvalidation` appears anywhere under `internal/discovery/`, and a unit test asserts a
full discovery run issues zero flagged requests.

**(b) At most one Level-3 re-discovery per scope per process.** `cache.Session` holds the re-entrancy
guard. The first occurrence invalidates, re-discovers, and records
`meta.cache.discovery = "revalidated"`. A **second** occurrence in the same process skips straight to
`schema_stale` (exit 10) with the refreshed field list in `error.details` — no second re-discovery, no
ping-pong between a genuinely stale scope and a genuinely invalid path.

**(c) The retry is restricted to idempotency-safe requests, using exactly the §6.1 predicate**: `GET`,
read-only `POST /api/graphql`, and `POST` with `X-Payload-HTTP-Method-Override: GET`. For any
non-idempotent method, PayCLI performs the invalidation and re-discovery but **does not re-send**;
it returns `schema_stale` (exit 10) carrying the refreshed field list and a `next.cmd` that re-runs
the original command, so a human or agent re-issues it deliberately. This is stated explicitly here
because the original wording ("retries the original request exactly once") directly contradicted
§6.1's "once any response byte is read, never retry a write" — Level 3 by definition fires *after* the
body was read. A 404 `Route not found` on `POST /api/{coll}` behind a proxy mid-deploy, or a 501 on
`pay upload`, would otherwise re-send the write and re-stream the whole multipart file.

### 8.5 TTLs

| Data class | Fresh | Hard max | Revalidation |
|---|---|---|---|
| discovery manifest (topology) | 10 min | 24 h | Level-1 probe |
| per-identity permissions | 5 min | 24 h | Level-1 probe |
| GraphQL per-type schema | 24 h | 7 d | Level-2 + Level-3 |
| `_where` input types | 24 h | 7 d | Level-2 + Level-3 |
| identity (`/me`) | 60 s | — | **memory only** |
| GraphQL mode / capability probe | 24 h | 7 d | full re-discovery |
| skills manifest / release metadata | 6 h | — | — |
| block census (`block_census`, §7.10c) | 24 h | 7 d | per-entity rebuild: `pay blocks learn`, stale on use, `--refresh`, reactive in `validate`/`example`; write echoes folded in |
| **any document data** | **never cached** | | |

**Staleness ladder.** Within `fresh` → use with zero network, `meta.cache.discovery = "hit"`.

Past `fresh` but within `hard max`, precisely (the original two-clause wording was self-contradictory
— "serve immediately" and "re-discover before producing output" cannot both hold, and the two readings
differ in whether a stale-data window exists at all):

1. Compute the full response using the stale manifest and **hold it unwritten**.
2. Fire the Level-1 probe concurrently and wait **up to 250 ms** for it.
3. Hash matches → emit the held response with `meta.cache.discovery = "stale-served"` and bump
   `meta.confirmed_at`.
4. Hash differs → discard the held response, re-discover, re-issue the operation, emit with
   `meta.cache.discovery = "revalidated"`.
5. Probe times out or errors → emit the held response with `meta.cache.discovery = "stale-served"`
   plus `warnings[] {"code":"revalidation_timed_out","message":"served from a cache entry older than its freshness window; the 250 ms revalidation probe did not complete","hint":"pay discover --refresh"}`.

Nothing is written to stdout before step 3/4/5 resolves, so the advertised warm latency is honest: a
stale-but-valid entry costs at most 250 ms more than a fresh one, and that cost appears in
`meta.duration_ms`.

**Serve-stale is read-path only.** Any write whose client-side validation depends on cached field
names forces a synchronous Level-1 revalidation first.

**Cold cache (the case the original spec never stated, and step zero of every agent task).**
Any command that needs the manifest **runs discovery synchronously on a miss**, single-flighted
through `cache.Session`, inside the command's `--timeout`/`--deadline` budget. It sets
`meta.cache.discovery = "miss"` and emits
`warnings[] {"code":"discovery_ran","message":"no cached schema for this profile; ran discovery first","elapsed_ms":947,"hint":"pay discover pre-warms this"}`,
and it **never** fails with `discover_first`. `pay discover` exists only to pre-warm or force-refresh.

`next.reason: "discover_first"` is emitted **only** when discovery itself failed — i.e. alongside
exit 10 `discovery_failed`. It is never a way of telling a caller "run discovery and try again".

`--no-cache` / `PAY_NO_CACHE=1` still discovers **in-process** but writes nothing to disk
(`meta.cache.discovery = "disabled"`). It never disables client-side validation — the manifest exists
either way, it is simply not persisted. `--refresh` forces a re-discovery and *does* write.

### 8.6 In-process layer

`internal/cache.Session`, built once in `PersistentPreRun`, threaded on the context. Holds: the decoded
manifest; the resolved identity (**memory only**); the Level-1 probe result (so a command touching 3
collections issues one `/api/access`, not three); a `singleflight` group keyed by
`method + url + sha256(body)`; a resolved-reference memo for relationship hydration; the compiled
command tree; and a negative memo for 404/501 paths so a fan-out over 49 collections hits
`payload-migrations`'s 501 once rather than 49 times.

The RAM layer **may** hold document data; the disk layer may not. Any successful write flushes the RAM
memo for the affected collection immediately.

### 8.7 Concurrency and locking

Discovery writes a set of interdependent files, so the write of the **set** takes an advisory
exclusive `flock` on `.lock` (via `golang.org/x/sys`) with a 5 s acquisition timeout. **Shards and
`graphql/` blobs are written first; `manifest.json` is renamed last**, so the index — which is what
names and checksums every shard — only ever becomes visible after everything it points at exists.

On timeout PayCLI **skips the cache write and continues** with the in-memory result — a cache failure
must never fail a command, and a stale lock from a crashed process must never wedge the CLI. A skipped
write is **never silent**: it emits
`warnings[] {"code":"cache_write_failed","message":"<path>: <errno>","hint":"pay cache path"}` and
`pay doctor` reports it. Without this, a read-only or full `$CACHE` makes every invocation silently
re-pay discovery forever while `meta.cache.discovery` reports a plain `miss`, indistinguishable from a
legitimate cold start.

Reads are lock-free. The flock protects writers from each other, never a reader from a writer, so
lock-free reads are made safe by three things together: atomic rename per file; the self-describing
entry header (§8.3); and the **`generation` field on every artefact of a run** (§8.2). A reader that
observes a shard from writer A and an index from writer B sees mismatched generations and treats it as
a miss. Without `generation` the torn read is structurally undetectable — the original four-file layout
could publish a *new* `manifest.json` against an *old* separate `fingerprint.json`, after which Level-2
revalidation either re-discovers on every single invocation forever or confirms a stale manifest as
fresh and rejects a field that exists.

A lock is never held across a network call.

The manifest is immutable after construction and shared by read-only pointer; only the mutable memos
are guarded.

**GC:** on any write, with probability 1/50 or when a sentinel mtime is >24 h old, sweep `$CACHE/v1`
and collect scope directories whose **`manifest.json` → `meta.confirmed_at`** (the single
authoritative copy, §8.2) is older than 30 days, plus any foreign epoch directory. Never on the read
path.

**GC never unlinks a directory a reader may be traversing.** It first `rename`s the scope directory to
`<scope>.trash-<ulid>` — atomic, and invisible to the scope lookup, which only ever resolves an exact
16-hex name — and then removes the renamed tree best-effort, skipping and retrying on the next sweep
if removal fails. On Windows, unlinking a file another process has open fails outright, so the rename
is what makes GC safe there; on unix it additionally guarantees a mid-traversal reader gets a clean
`ENOENT`, which §8.3 already defines as a **miss**, not an error.

---

## 9. Command tree

Grammar: `pay <verb> <entity> [id] [flags]`. Collections and globals are **always arguments**, never
subcommands.

### 9.1 Root persistent flags

```
--profile NAME              --base-url URL              --api-path PATH
--api-key KEY               --api-key-stdin             --api-key-file PATH
--auth-collection SLUG      --auth-mode MODE   auto|api-key|jwt|anonymous
--graphql-path PATH         --graphql-route PATH         --auth-header-scheme JWT|Bearer
--output FORMAT             json|jsonl|table|csv|id|raw        (default json, never TTY-dependent)
--path EXPR                 a three-form path expression applied to .data (§10.3; NOT jq)
--quiet, -q                 --verbose, -v                --no-color
--timeout DURATION  30s     --deadline DURATION  120s    --max-retries N  3  (alias: --retries)
--concurrency N  8          (hard cap 32)
--accept-language TAG  en   (§6; load-bearing for label harvesting)
--no-cache                  --refresh                    --cache-ttl DURATION
--locale CODE               --fallback-locale CODE|none|default
--yes                       --dry-run                    --no-audit
--no-redact                 --no-raw                     --config PATH   --insecure-skip-verify
--backup-dir DIR            --no-backup                  (§12.8: back up every document a write changes)
--errors-to stdout|stderr   --log-level LEVEL            --log-format text|json
```

Why concurrency defaults to 8 and not higher (300 requests at 50-way concurrency all returned 200 in
3.14 s, so the server tolerates far more): the target is usually a dev machine running Next.js in dev
mode; Payload runs hooks and DB writes per document; and Payload 3.x has **no server-side rate
limiter**, so PayCLI is the only backpressure that exists. This reasoning is in the help text so an
agent knows it may deliberately raise it.

### 9.2 Full tree

```
pay auth login   --profile NAME --base-url URL [--api-key KEY|--api-key-stdin|--api-key-file F]
                 [--auth-collection SLUG] [--keyring] [--no-verify]
pay auth list | use <profile> | logout [<profile>] | test [--profile P]
             | rename <old> <new> | status | fix-perms
pay auth preview-secret [--stdin | --clear]  # §5.5: the site's draft-preview secret; never printed
pay whoami                                   # GET /{auth}/me; asserts user != null; redacted
pay access [<collection>] [--doc ID] [--global SLUG]
pay can <create|read|update|delete> <collection>     # exit 0 permitted, 8 denied

pay discover [--refresh] [--deep] [--verify-sort] [--allow-write-probes] [--no-labels]
pay explain  [--slim|--full] [--section connection|collections|globals|commands|query_syntax|exit_codes|gotchas]
             [--collection SLUG] [--max-bytes N] [--offset N] [--limit M] [--grep PATTERN]
                                                        (alias: pay capabilities)
pay collections [--kind content|upload|auth|internal] [--writable] [--include-internal]
                [--capability upload|versions|drafts|auth|trash|folders] [--grep PATTERN]
                                                        (alias: pay ls)
pay describe <collection|global> [--field PATH] [--block SLUG] [--blocks-detail]
             [--required-only] [--queryable] [--examples] [--sample]
             # --field also takes nested block patterns (layout[].blocks, layout[2].blocks, group.blocks);
             # --field/--block answer from the block census when the schema cannot (§7.10c.6)
pay doctor
pay cache info | ls | path | show <scope> | clear [--scope S|--all] | warm

pay auth login --jwt --profile NAME --base-url URL (--email X | --username X) --password-stdin
               [--auth-collection SLUG] [--keyring] [--no-verify]

pay find <collection>                                                    (aliases: list, ls)
    --where 'PATH OP VALUE'...   --or 'PATH OP VALUE'...   --where-json JSON   --where-raw QS
    --q TEXT     --id ID...   --ids a,b,c
    --sort FIELD   --limit N (default 20)   --page N   --all [--max 1000]
    --select f1,f2   --select-exclude f1,f2   --depth N (default 0)
    --populate 'coll:f1,f2'...   --joins 'field:limit=5,sort=-createdAt'...
    --draft   --draft-only   --published-only   --trash
                 # --published-only is REQUIRED to filter to published docs. A bare read does
                 # NOT exclude never-published documents — see §9.6.2, verified live.
    --since 7d   --until DATE   --date-field updatedAt   --count-only
    --no-validate-sort   --no-validate-where
    --richtext json|md|text      # render every Lexical editor state for reading (§9.12.8.2)
pay get   <collection> <id> | <collection> --slug SLUG                    # §9.17
          --depth --select --select-exclude --populate --joins --draft --published-only --trash
          --locale --fallback-locale --richtext json|md|text
          # get --published-only is an ASSERTION (not_published, exit 4), not a filter (§9.6.2)
pay count <collection>        --where/--or/--where-json --trash        (NOTE: --draft is a no-op)
# --data / --data-file / --set / --set-json / --set-md are COMBINABLE and deep-merged, later wins.
# Value typing, relationship-id coercion and the full precedence order are §9.10 — identical for
# create, update, upload, globals update, upsert and the body form of diff. Every write body may
# hold {"$ref": …} references, resolved to ids before anything is previewed or sent (§9.10.5).
pay create <collection>
    [--data JSON] [--data-file F] [--data @-] [--set k=v]... [--set-json k=JSON]...
    [--set-md k=@file.md|k=@-|k=MARKDOWN]...
    --draft --autosave --depth --select --file PATH --locale --no-echo-check --dry-run --yes
pay update <collection> <id>            (same data flags)
    --draft --autosave --publish --unpublish --trash --override-lock --reset-nested
    --depth --select --locale --no-echo-check --dry-run --yes
    # no data flags + --reset-nested = the orphan-row repair (§10.2.1.5)
pay update <collection> --where ... [--max-docs N | --all] [--per-doc] [--fail-fast] --dry-run --yes
pay delete <collection> <id>            --permanent --override-lock --dry-run --yes
pay delete <collection> --where ... [--max-docs N | --all] --permanent
                                        --unsafe-passthrough-where --dry-run --yes
pay restore   <collection> <id>                       # un-trash
pay duplicate <collection> <id> [--no-draft]
pay publish|unpublish <collection> <id> [--reset-nested] [--depth] [--select] [--locale]
pay publish|unpublish <collection> <id> <id>... [--fail-fast]      # one PATCH per id (§12.5)
pay publish|unpublish <collection> --where ... [--max-docs N | --all]
                 # publish only: [--no-pre-validate]; all forms: --dry-run --yes

pay upsert <collection> [--match F[,F…]] (--data|--data-file|--set|--set-json|--set-md …)
           [--publish|--draft] [--force] [--ignore PATH]... [--locale] [--depth] [--no-echo-check]
           [--dry-run]                                                   # §9.15.2
pay plan   <DIR|FILE|->... [--brief] [--ignore PATH]... [--full]         # §9.15.6, read-only
pay sync   <DIR|FILE|->... [--brief] [--ignore PATH]... [--full] [--fail-fast] [--no-echo-check]
           [--dry-run] [--yes]                                           # §9.15.7

pay diff <collection> <id> (--data-file F | --data JSON | --set k=v … | --set-json k=JSON … | --set-md …)
pay diff <collection> <id> --draft-vs-published
pay diff <collection> <idA> <idB> [--draft]
pay diff --global <slug> (--data-file F | --data … | --draft-vs-published)
pay diff --left FILE --right FILE                   # local: no network, no profile
         # every form: [--ignore PATH]... [--include-timestamps] [--full]  (§9.14)

pay outline <collection> <id> [--draft]                                  # §9.13
pay outline <collection> --slug SLUG [--draft]
pay outline --global SLUG [--draft]
pay get … --depth 0 | pay outline

pay url <collection> <id> | <collection> --slug SLUG                     # §9.16
        [--draft] [--locale CODE] [--preview [--cookie-file FILE]]

# Rich text ⇄ Markdown (§9.12). Local: no profile, credential, cache or network.
pay lexical from-md [FILE|-] [--as FIELD] [--keep-frontmatter]
pay lexical to-md   [FILE|-] [--field PATH] [-o|--out FILE|-]
pay lexical text    [FILE|-] [--field PATH] [-o|--out FILE|-]

pay upload   <collection> <file|-|URL> [--alt TEXT] [--set k=v]... [--set-json k=JSON]...
             [--set-md k=…]... [--data JSON] [--data-file F]
             [--filename NAME] [--replace ID] [--content-type MIME] [--max-size 100MB] [--allow-remote]
pay download <collection> <id> | --filename NAME [-o PATH|-] [--size thumbnail]

pay globals list
pay globals get    <slug> [--draft] [--depth] [--select] [--locale] [--richtext json|md|text]
pay globals update <slug> (--data|--data-file|--set|--set-json|--set-md)
                   [--draft|--publish|--unpublish] [--reset-nested] [--depth] [--select]
                   [--locale] [--no-echo-check] [--dry-run] [--yes]
                   # no data flags + --reset-nested = the orphan-row repair (§10.2.1.5)

pay versions list    <collection>|--global SLUG [--id DOC] [--autosave] [--latest]
                     [--limit] [--page] [--sort]
pay versions get     <collection>|--global SLUG <versionId> [--depth] [--richtext json|md|text]
pay versions restore <collection>|--global SLUG <versionId> [--draft|--unpublish] [--depth] [--dry-run] [--yes]
pay versions diff    <collection>|--global SLUG <vA> <vB>
                     [--ignore PATH]... [--include-timestamps] [--full]   # structural (§9.14.8)

# §9.11's edit pipeline. Every edit verb reads ONE document from stdin, edits it and writes it to
# stdout; `pay apply` is the only stage that reaches the network. --field takes nested paths
# (layout[2].blocks, layout[id:…].blocks, layout[type:group[1]].blocks) — §9.11.3.
pay blocks ls  [--field PATH] [--long] [--recursive|-r]
pay blocks mv  <selector> (--at N | --before SEL | --after SEL | --first | --last) [--field PATH]
pay blocks rm  <selector>... [--all] [--field PATH]
pay blocks add <blockType>|- [--data JSON|@F] [--set k=v]... [--set-json k=JSON]... [--set-md k=…]...
               [--name NAME] [--template] [--at N | --before SEL | --after SEL | --first | --last]
               [--field PATH]                 # `-` = a plain array row without blockType (§9.11.9)
pay blocks cp  <selector> [--at N | --before SEL | --after SEL | --first | --last] [--field PATH]
pay blocks set <selector> [--data JSON] [--set k=v]... [--set-json k=JSON]... [--set-md k=…]...
               [--unset KEY]... [--name NAME] [--field PATH]
pay apply [collection] [id] [--field PATH]... [--all-fields] [--depth] [--select]
          [--draft|--publish|--unpublish] [--reset-nested] [--locale CODE] [--no-echo-check]
          [--dry-run] [--yes]

# The block catalog (§7.10c): the site's blocks, learned from real documents. Only `validate`
# reads stdin; none writes to Payload.
pay blocks types    [collection|global] [--field PATH] [--census-max-docs N]
pay blocks schema   <blockType> [--collection C] [--field PATH] [--census-max-docs N]
pay blocks example  <blockType> [--collection C] [--field PATH] [--n 1..8] [--census-max-docs N]
pay blocks new      <blockType> [--collection C] [--field PATH] [--set k=v]... [--set-json k=JSON]...
                    [--set-md k=…]... [--from-example] [--census-max-docs N]
pay blocks validate [--data-file F | stdin] [--collection C] [--field PATH] [--census-max-docs N]
pay blocks learn    [collection|global...] [--census-max-docs N]

# §12.8 automatic pre-write backups
pay backups list    [-n N] [--since D] [--collection C] [--id ID] [--global G] [--command C] [--all-profiles]
pay backups show    <file>
pay backups restore <file> [--draft|--publish] [--recreate] [--ignore-target] [--depth N]
                    [--no-echo-check] [--dry-run] [--yes]
pay backups prune   --older-than D [--collection C] [--global G] [--id ID] [--all-profiles]
                    [--dry-run] [--yes]

pay raw <GET|POST|PATCH|DELETE> <path> [--query k=v]... [--data JSON] [--file F] [--raw-body]
pay config get|set|unset|list|paths|explain
pay skills install [--global] [--dir PATH] [--agent a,b|all] [--force] [--with-project-context]
pay skills list | status | print [NAME] | update [--project-only] | uninstall
pay completion bash|zsh|fish|powershell
pay version [--check]
pay update-self [--check] [--apply] [--force]
pay audit tail [-n N] [--since D] [--action ACTION] [--phase pre|post] [--command C] [--collection C]
               [--profile-filter P] [--rotated]
               # ACTION ∈ create update delete trash restore publish unpublish upload
               #          restore_version raw restore_backup
```

### 9.3 Client-side validation (before any network call)

Everything below is validated against the cached manifest and fails locally with a precise error and
the valid list attached. This is PayCLI's core value-add, because the server's own behaviour on these
inputs ranges from unhelpful to silently wrong:

| Input | Server behaviour (verified) | PayCLI behaviour |
|---|---|---|
| unknown collection slug | 404 `Route not found "…"` | `collection_unknown` (exit 10) + `did_you_mean` from the manifest |
| unknown `--sort` field | **200, silently unsorted** | `invalid_sort_field` (exit 5) + the sortable-field list |
| unknown `--select` key | **200, silently returns only `id`** | `unknown_field` (exit 5) + the field list |
| unknown `where` path | 400 `QueryError` | `query_path_invalid` (exit 5) locally + `did_you_mean` |
| invalid enum value in `where` | **500 `Something went wrong.`** on Postgres, **0 docs** on Mongo | `invalid_option` (exit 5) + the enum values — **only** when `options_source != "unknown"` |
| `--limit 0` | unlimited | `invalid_args` (exit 5) |
| id not castable to `id_type` | **500** | `invalid_id` (exit 5) — **only** when `id_type != "unknown"` |
| `all`/`near`/`within`/`intersects` | **500** on Postgres, `all` **works** on Mongo | `unsupported_operator` (exit 5) **only** when `db_adapter == "postgres"` and `db_adapter_source == "configured"`; otherwise send it and learn (§7.11) |
| `--draft` on a collection without drafts | `QueryError` or silent | `feature_unavailable` (exit 10) — **only** when `flags.drafts == false`; `null` ⇒ send it |
| `--file` on a non-upload collection | 500 | `not_upload_collection` (exit 5) — **only** when `flags.upload == false` |
| `--locale X` not in this project's locales | **200, silently the DEFAULT locale** | `invalid_option` (exit 5) **only** when `locales_source` ∈ `{configured, locale-all}`; otherwise pass through + `locale_unverified` (§7.9c) |
| `--date-field F` not a date field | `QueryError` or silent | `unknown_field` (exit 5) + the date-field list |
| `--limit` on a bulk-write verb | n/a | `invalid_args` (exit 5), hint `use --max-docs N to cap the blast radius, or --all` |
| `--richtext` value other than `json`/`md`/`text` (aliases `lexical`, `markdown`, `plain`, `plaintext`) | n/a | `invalid_option` (exit 5), 0 requests |
| a `{"$ref": …}` naming a field the target's shard does not know | the reference is sent verbatim; `validation_failed` or a masked 500 | `unknown_field` (exit 5) + `did_you_mean`, 0 requests (§9.15.1) |
| `--set-md` on a field the shard types as neither `richText` nor `json` | **stored** — an editor state in a text field becomes JSON text | `invalid_args` (exit 5), 0 requests (§9.12.8.1) |
| `--draft --publish` together | contradictory | `invalid_args` (exit 5) before anything is read (§10.2.2) |
| `--draft --unpublish` together (update, apply, globals update, versions restore) | `?draft=true` saves a draft version; the live document stays published while the echo says `draft` | `invalid_args` (exit 5) before anything is read (§10.2.2) |

`--no-validate-sort` / `--no-validate-where` bypass the check for escape-hatch cases.

**The tri-state rule governs this entire table** (§7.6). Every row above is gated on the manifest
actually *knowing* the fact. When the relevant value is `"unknown"` or `null`, PayCLI **sends the
request** and attaches the matching `*_unknown` warning, rather than rejecting locally. A client-side
rejection built on a fact PayCLI never learned is a fabricated error, and it is worse than the
server's own bad message because it is confidently wrong: `invalid_id` on a valid 24-hex ObjectId says
the id is malformed when the truth is that PayCLI could not determine the id type.

A unit test loads `testdata/fixtures/manifest_id_unknown.json` (`id_type: "unknown"`) and asserts that
`pay get <coll> 66f1a2b3c4d5e6f708192a3b` **issues an HTTP request** rather than exiting 5.

**Canonical ids on writes.** Payload casts a numeric id in the URL path and in `where id in`, so `0108`
and ` 108` reach document 108. The write commands that take an id (`update`, `apply`'s id, `delete`,
`publish`/`unpublish` — one or several ids —, `restore`, `upload --replace`) therefore rewrite a
numeric id to the decimal the server reports (`108`) right after validation, when `id_type` is
`number`, and use that form for the request path, the backup, the dry-run diff and the audit.
`016 16` in one publish list is one document. String and unknown id types are never rewritten
(`0108` and `108` are two documents there). Found on localhost:3900: `pay delete pages 0108 --yes`
deleted page 108 while the backup, pairing the returned document `108` with the requested `0108` by
string, recorded it as skipped (§12.8.3).

### 9.4 Filter DSL

`--where` terms are ANDed. Repeatable `--or` terms form **one** OR group, which is ANDed with the
`--where` terms. `--where-json` replaces everything else. `--where-raw` appends a literal query string.

**Grammar:** split into at most 3 whitespace-separated tokens — `path`, `op`, `rest-of-string-as-value`.
Values containing spaces need no quoting. Paths may be dotted (`company.name`, `enrichment.status`,
`activities.type` — all verified working, including 2-hop relational paths).

| Alias(es) | Payload operator | Value handling |
|---|---|---|
| `eq` `=` `is` | `equals` | typed scalar |
| `ne` `!=` `not` | `not_equals` | typed scalar |
| `gt` `>` | `greater_than` | number/date |
| `gte` `>=` | `greater_than_equal` | number/date |
| `lt` `<` | `less_than` | number/date |
| `lte` `<=` | `less_than_equal` | number/date |
| `contains` `~` | `contains` | substring; `%` `_` `\` **escaped** |
| `like` | `like` | space-separated word AND (adapter-specific, see below) |
| `nlike` `!~` | `not_like` | value **auto-wrapped** in `%…%` |
| `in` | `in` | comma-split (`\,` escapes) or `json:[…]` |
| `nin` | `not_in` | same |
| `all` | `all` | same — **blocked client-side on Postgres** |
| `exists` | `exists` | bool; value optional, defaults `true` |
| `near` | `near` | `lng,lat,maxMeters[,minMeters]` |
| `within` / `intersects` | same | GeoJSON literal or `@file.geojson` |

The two mandatory deviations from pass-through, both verified footguns:
`nlike` auto-wraps (`not_like=hopper` returned **all 104 rows** including the one it should exclude),
and `contains` escapes (`contains=%` returned **all 104 rows**).

**Value typing** (only possible because PayCLI emits JSON): `null` → JSON null; `true`/`false` → bool;
integer/float literal → number; otherwise string. Force a string with quotes
(`--where 'code eq "123"'`); force raw JSON with the `json:` prefix (`--where 'tags in json:[1,2]'`).

**Sugar** compiled to the same tree: `--id`/`--ids` → `{"id":{"in":[…]}}`;
`--draft-only`/`--published-only` → `{"_status":{"equals":…}}`;
`--q TEXT` → an OR of `contains` across up to 12 text-ish fields chosen in the order
`useAsTitle, name, title, slug, email, <remaining text fields>`, with the chosen fields reported in
`meta.searched_fields`.

**`--since` / `--until` — grammar and field resolution.** Both are resolved **client-side** to an
absolute ISO-8601 instant (the server understands no relative dates) and compiled to
`{<date_field>: {greater_than_equal: …}}` / `{less_than_equal: …}`.

*Grammar*, identical for both flags — the original spec gave only the example `7d`, from which an
agent could not tell whether `30d`, `1mo`, `4w`, `720h` or `2026-08-01` parse:

| Form | Meaning | Examples |
|---|---|---|
| `N[h\|d\|w\|mo]` | N hours / days / weeks / months before *now* | `12h` `30d` `4w` `1mo` |
| a Go duration | `time.ParseDuration` | `720h` `90m` `36h30m` |
| `YYYY-MM-DD` | that date at `00:00:00Z` | `2026-08-01` |
| full ISO-8601 / RFC 3339 | verbatim | `2026-08-01T09:30:00Z` |

Anything else is `invalid_args` (exit 5) listing all four forms. `1mo` is calendar-aware (subtract one
month, clamping the day), everything else is exact arithmetic. `now` is `App.Now()` (§3.1), so the
resolution is deterministic under test.

*Field resolution*, and this is the part that used to generate a silent wrong answer. `--date-field`
wins when given. When it is **not** given:

* if the query is published-scoped (`--published-only`, or a `_status` term) **and** the collection
  has a date field named `publishedAt` → use **`publishedAt`**;
* otherwise → use **`updatedAt`**.

The original unconditional `updatedAt` default answers a different question than the one asked:
`pay find posts --published-only --since 30d` returns posts *touched* in the last month, not posts
*published* in it, on a collection that carries a real `publishedAt` field (verified present live).
That is exactly the class of silent wrong answer this spec rejects everywhere else — except here it
was PayCLI, not the server, generating it.

*Reporting is mandatory, so the resolution is never invisible.* Every envelope from a command that
used `--since`/`--until` carries `meta.date_field`, `meta.since` and `meta.until` (the resolved
absolute bounds), plus, whenever the field was **defaulted** rather than given:

```json
{"code": "date_field_defaulted",
 "message": "--since resolved against publishedAt >= 2026-08-17T00:00:00Z (posts has a publishedAt field and the query is published-scoped)",
 "hint": "override with --date-field updatedAt"}
```

`--date-field F` is validated against the manifest: `F` must exist and have `payload_type: "date"`,
else `unknown_field` (exit 5) listing the collection's date fields (`index.date_fields`).

### 9.5 Query encoding

`where` and `data` are emitted as **URL-encoded JSON strings**; everything else uses bracket notation.

```go
type Where map[string]any   // {"and":[…]} | {"or":[…]} | {"field":{"op":val}}
func (w Where) Encode() string { b, _ := json.Marshal(w); return url.QueryEscape(string(b)) }
```

Bracket notation stays for `select[title]=true`, `populate[crm-companies][name]=true`,
`joins[activities][limit]=5`, `sort`, `depth`, `limit`, `page`, `draft`, `trash` — verified that
`select` as a JSON string is silently dropped.

`--where-style qs` emits bracket notation for debugging and URL-pasting.

### 9.6 Payload-specific behaviour encoded in help and validation

1. **`depth` defaults to 0** (Payload's own default is 2). At depth 0 relations are bare ids; depth is
   capped by project config, so a bigger number is not automatically more data (verified: depth 1, 2
   and 5 returned identical bytes). Combine `--depth 1 --populate 'crm-companies:name'` to expand a
   relation while keeping only the needed fields.
2. **`--draft` on write** skips required-field validation — this is how an agent creates a stub.
   Publishing later **re-runs exactly the validation `--draft` skipped** (verified:
   `PATCH /api/pages/1 {"_status":"published"}` → 400 on `layout`), so a draft is a deferral, not an
   escape. A body that itself carries `_status: "published"` would publish despite `--draft`; PayCLI
   strips it and warns (§10.2.2). A write **without** `--draft` to a published document that has a
   pending draft is refused until the outcome is stated (§10.2.3).

   **On read, `--draft` is NOT a published filter and its absence is NOT one either.** The original
   blanket rule ("omitting `draft=true` returns the last published snapshot, so draft edits are
   invisible") is **false as stated** and, taught in help text, makes an agent hand back unpublished
   drafts as published content. Verified live: `GET /api/pages?limit=100&depth=0` with no `draft`
   parameter returned all 11 documents, **every one `_status:"draft"`**, byte-identical to the same
   call with `draft=true`; `GET /api/pages/18` on a never-published document returned the draft.

   The real rule is **per document**: a document that *has* a published version yields that published
   snapshot; a document that never had one yields its draft row. The exact wording required in help,
   `SKILL.md` and `references/gotchas.md`:

   > A read without `--draft` does **not** filter out unpublished documents — documents that were
   > never published are returned with `_status:"draft"`. To get only published content, always pass
   > `--published-only` (`_status = published`). `--draft` additionally swaps in the newest draft for
   > documents that **do** have a published version.

   **Enforcing warning.** When `find` or `get` targets a drafts-enabled collection with no `_status`
   filter and the result contains any non-published document:
   `{"code":"mixed_status_results","message":"3 of 11 returned documents are not published","hint":"pay find posts --published-only"}`.

   `_status` is always shown in table output for versioned collections. PayCLI does **not** infer
   `draft` from prior commands (§1 conflict 21).

   **`pay get … --published-only` is an assertion, not a filter.** It reads without `draft` (the
   stored main row, §2.9) and returns the document when its `_status` is `published`; otherwise it
   fails `not_published` (exit 4) — "has no published state (never published, or unpublished)" —
   with the hints `pay get <c> <id>` / `pay get <c> <id> --draft`. `--published-only --draft` is
   `invalid_args`; on a collection proven to have no drafts it is `feature_unavailable` (as for
   `find`). A `--select` that lacks `_status` gets `_status` added so the assertion can be made.
3. **`count` ignores `draft`** — use `--published-only` / `--draft-only`.
4. **Join fields** are always present as `{docs:[…], hasNextPage:bool}` even at depth 0, are queryable
   by dotted path, and `joins=false` does not disable them over the query string. Drop them with
   `--select`.
5. **`locale`/`fallback-locale`** are accepted and ignored on non-localised projects, so their
   acceptance proves nothing. On a **localised** project PayCLI sends `fallback-locale=none` by
   default so untranslated fields return null instead of the default locale's text, echoes
   `meta.locale = {requested, fallback}` on every read, and validates `--locale` only when the true
   codes are known. The full rules, the three verified coercion behaviours, and where the code list
   comes from are §7.9 — they are the difference between translating a page and overwriting `de` with
   English.
6. **`--all`** paginates with `--max` (default 1000) and sets `page.truncated`. It never passes
   `limit=0` through.
7. **`HEAD` is not routed** (404). Health checks use `GET /api/access`.

### 9.7 Block types

Block `blockType` literals are **unrecoverable** from the API when `interfaceName` is set (which the
official Payload website template does for every block): the GraphQL union exposes
`CallToActionBlock`, `ContentBlock`, `MediaBlock`… while the real slugs are `cta`, `content`,
`mediaBlock`. Payload **silently drops an unknown `blockType`** and returns 201.

PayCLI therefore resolves them in the order **profile `blocks` map → project source → observed
values → unknown**, exactly as specified in §7.10 — the project-source scan is what makes the
common case solvable, because the "harvest observed values" fallback yields nothing on a project whose
`layout` arrays are all empty (verified: all 12 live pages have `layout: []`).

When the collection's *required* paths include an unresolvable blocks field, the collection is
`publishable: false` and `pay publish` refuses locally (§9.10.4) rather than relaying a 400 the agent
cannot act on.

**The write check (replaces the pre-census rule).** The original rule — emit `unknown_block_type`
for every blockType the discovered slug list does not contain — fired on **every** block of **every**
write on an introspection-disabled project, where the list is always empty, and ignored nested rows
entirely; the agent grep'd the warnings away and the real ones drowned. Now `create`, `update` (by id
and bulk), `upload`, `globals update`, `apply` (collection and global), `upsert`, `sync` and the body
form of `pay diff` run `ValidateBlocks` (§7.10c.4) over the body whenever it holds a block row at any
depth **or a non-empty array at a position known to be a blocks field** (a census pattern with rows,
or a top-level field the schema declares as blocks — `discovery.TouchesBlocks`), and emit the findings
as **warnings only**. The second half catches `layout=[{}]` / `layout=[{"heading":"x"}]`: no row is
typed, Payload drops every one with a 2xx and the layout becomes `[]`; before, dry-run, plan and apply
said nothing (stage page 69, dry-run: now `empty_row` / `missing_block_type`) — a write is never blocked on observation. Findings
fold to one warning per code (first three messages; `paths` lists all; "… and N more (pay blocks
validate lists every one)"). An observation-based warning names the command that renews the evidence
first and states the census age ("`pay blocks learn pages` first if the site's blocks changed since
the block census was read (32m0s ago); then …"). `input_silently_dropped` after the write (§10.2) is
unchanged.

**Writes never build the census** (§1 conflict 42). They use a cached census of any age within the
7-day hard max — of **this entity**: another entity's census is not enough (a slug is not globally
unique, §7.10c.3). With neither a cached census with block rows for the entity nor an authoritative
declared list for every blocks field written, the body gets **one** `blocks_unchecked` warning
(hint `pay blocks learn <slug>`) and no observation-based finding.

**The document in hand outranks the census.** During a migration the site changes under a census,
often through PayCLI itself (stage: a restructure moved six `questions` rows into `group.blocks`; a
later plan against the still-fresh census warned `block_type_not_observed_here` for rows it had just
fetched). Two rules close the gap at no request cost:

* **Pre-image overlay.** When a write already holds the document's current state — `pay plan`/`sync`
  (the fetched match), `pay upsert` (the matched document), `update <id>`/`apply`/`globals update` with
  `--dry-run` (the diff's pre-image), and their real writes when a backup was taken — it is folded into
  the entity's census in memory (`discovery.OverlayDocs`) before the body is checked. An overlay only
  ADDS evidence; it never makes a field select-like, and a type no document carries stays unknown.
  With no census for the entity it does nothing (one document is not a census).
* **Each document counts once.** The entity records `doc_keys` (the ids its read counted) and
  `folded_keys` (ids folded in since). A fold or pre-image of a document in neither is new evidence and
  raises counts and presence like a read row. One already counted (the page an agent iterates on, the
  pre-image of a page the census read) only **refreshes**: a position, type, field or value it shows
  for the first time is recorded (a new field capped at the type's count), a known field it leaves
  empty is marked `optional` (never `likely_required` again), and no count grows. A census written
  before `doc_keys` existed treats every document as counted. Localhost (formBlock on one real page):
  create + three title-only updates of a test page used to give count 2→5 and a false
  `likely_required introContent`; now count 2 after all four, `folded_docs: 1`, and the folded census
  equals a fresh `blocks learn` exactly.
* **Echo fold.** The echo of every successful write (`create`, `update <id>`, `apply`, `upsert`,
  `sync`, `globals update`; read like the census reads: depth 0, not `locale=all`) is folded into the
  **cached** census of that entity when the invocation ends (read-modify-write under the scope lock;
  only an entity that is cached, read without error and within the hard max; `folded_docs` counts the
  distinct documents folded;
  `built_at` unchanged). A value seen once is dropped unless the field is already a vocabulary, so no
  content reaches the disk. A dry run folds nothing.

A string in a block row's field the census observed as `richText` warns `richtext_as_string`
(§9.12.8.3); `pay blocks add <slug>` on a field known to be a plain array warns `block_type_omitted`
(§9.11.9).

### 9.8 `pay raw`

`pay raw <METHOD> <path> [--query k=v]... [--data JSON|--data-file F] [--file PATH] [--raw-body]`
resolves the base URL, injects profile auth, and applies retry, audit, safety classification and the
envelope. The path is relative to `api_path` unless it starts with `/`. It is routed through the same
L0–L3 risk classification by HTTP method, so `pay raw DELETE …` still requires `--yes`.
`--output raw` returns Payload's body verbatim.

This is what guarantees PayCLI is never a dead end: custom collection/global `endpoints`,
`POST /api/graphql`, plugin routes, `POST /{coll}/access/{id}`, `GET /{coll}/file/{filename}`,
`GET /payload-jobs/run`, and anything added in a future Payload version.

### 9.9 Completion

Cobra `ValidArgsFunction`s complete collection and global slugs, `--where`/`--sort`/`--select` field
paths for the collection already on the line, `--profile` names, and the `--output` enum.

**Hard rule: completion never touches the network and never blocks.** Budget 100 ms; on a cold or
expired cache return zero suggestions plus `cobra.ShellCompDirectiveNoFileComp` rather than triggering
discovery. A shell that hangs on TAB is worse than one that suggests nothing. `pay __complete` never
prints the envelope.

**Completion reads `manifest.json` (the index) and, for field-path suggestions, at most the single
shard for the collection already on the command line.** A missing or generation-mismatched shard
yields zero suggestions, per the never-block rule. This is what makes the 100 ms budget satisfiable at
any project size: index decode is 1.44 ms at 150 collections and 4.79 ms at 500 (§2.8), while the
original consolidated layout cost 282 ms at 150 × 200 fields — it could not have met the budget above
roughly 10 MB, and §9.9 requires `--where`/`--sort`/`--select` path suggestions.

**Block slugs and blocks fields come from the cached census too** (never built at TAB time; measured
14–21 ms on stage with a 315 KB `census.json`):

| where | candidates |
|---|---|
| `pay describe <e> --block` | the entity's merged knowledge (§7.10c.3): each top-level field's reconciled accepted set (an inferred interface-name slug the census matched is replaced by the real one, `callToActionBlock` → `cta`) plus every type seen at a nested level; no census → the declared list |
| `pay blocks schema\|example\|new <TAB>` | with `--collection C`: as above for C; without: every census slug across entities |
| `pay blocks add <TAB>` | every census slug (the piped document's collection is unknown at TAB time) |
| `--collection` on `schema/example/new/validate`; `pay blocks types <TAB>`; `pay blocks learn <TAB>…` | collection and global slugs (`learn`: those not already given) |
| `--field` on `types/schema/example/new`, and `pay describe <e> --field` | declared blocks fields + every census pattern (`layout[].blocks`) + parent-scoped `type.field` (`group.blocks`); describe keeps its ordinary field paths |

A declared slug keeps the project's label/description as the candidate description; a slug known only
from documents says `observed: 167 rows in pages`. The pipe verbs' `--field` is not completed: the
document is on stdin, so there is no collection at TAB time.

### 9.10 Write input semantics

Referenced identically from `create`, `update`, `upload`, `globals update`, `upsert` and the body form
of `pay diff`. The original spec
defined value typing for `--where` only (§9.4) and said nothing about `--set`, which left the most
common write in the CLI — `pay update posts 1 --set heroImage=4` — working or failing on an unstated
implementation choice. Verified live: `PATCH /api/posts/1 {"heroImage":"4"}` → 400
`"This relationship field has the following invalid relationships: 4 0"`, while `{"heroImage":4}` is
accepted.

**9.10.1 `--set k=v` typing.** Apply §9.4's value-typing rules (`null` → JSON null, `true`/`false` →
bool, integer/float literal → number, otherwise string; quotes force a string; `json:` forces raw
JSON), **then coerce against the manifest field**:

| Field | Coercion |
|---|---|
| `relationship` / `upload`, `write_shape: id` | to the **target collection's** `id_type` |
| `relationship` / `upload`, `write_shape: id_array` | comma-split, each element to the target's `id_type` |
| polymorphic (`{relationTo,value}` shapes) | `--set` is refused; use `--set-json` or `--data` |
| `number` | JSON number |
| `checkbox` | JSON bool |
| `date` | ISO-8601 (accepts the §9.4 `--since` grammar's absolute forms) |
| `richText` / `json` / `blocks` (`write_shape: json`) | `--set` is refused; use `--set-json` |
| everything else | string |

A mismatch fails **locally** with `invalid_args` (exit 5) naming the field, the expected type and the
`write_shape` — e.g.
`heroImage expects a number id (posts.heroImage → media, id_type=number); got the string "4". Payload rejects this with an unintelligible "invalid relationships" message.`
When the field's `id_type` or `write_shape` is unknown (tri-state rule), **no coercion and no local
rejection** — send the §9.4-typed value and attach `warning{code:"write_shape_unknown"}`.

`--set-json k=JSON` bypasses coercion entirely and is the documented escape hatch.

**9.10.2 Combining data flags.** `--data`, `--data-file`, `--set` and `--set-json` are **combinable**,
not alternatives. They are deep-merged in this order, later winning on a key collision:

```
--data / --data-file   (base object)
--set                  (typed + coerced scalars)
--set-json             (raw JSON)
--set-md               (Markdown → a Lexical editor state; §9.12.8.1)
```

`--set-md` **replaces** the value at its key — it never deep-merges an editor state into an earlier
one, so no stale key of an earlier state survives. The same order holds for `upsert` and the body form
of `pay diff`; `pay blocks add|set` use `--data` → `--set` → `--set-json` → `--set-md` → `--name`.
Arrays are **replaced**, never merged element-wise. The `|` in §9.2's `create`/`update` grammar that
implied mutual exclusion is removed. `pay upload` uses the identical order with `--alt` slotted in as
the lowest-precedence contributor (`--alt` → `--data`/`--data-file` → `--set` → `--set-json` →
`--set-md`), which
is the same "later wins" rule, stated explicitly because §13 previously said "merged in that
precedence order" without saying which end won.

**9.10.3 Payload validates the whole document on update.** A `PATCH` of one field re-validates every
field of the stored document (verified: `PATCH /api/posts/1 {"heroImage":4}` → 400 naming `title`,
`slug` and `content`, none of which were sent). Consequences are specified in §11.1 (`error.fields[].sent`)
and this rule is stated verbatim in `pay update`'s `COMMON MISTAKES` section.

**9.10.4 `pay publish` pre-validates locally.** Before issuing `PATCH {"_status":"published"}`,
`pay publish` checks the target document against the manifest's `required_paths` for the collection
and fails with `validation_failed` (exit 5) naming every missing path — **before** the network call —
instead of relaying an opaque server 400. When the collection is `publishable: false` (§7.10), it
fails with `feature_unavailable` (exit 10) carrying `publishable_reason` and the
read-them-from-source instruction, because no amount of retrying will make an unknown `blockType`
knowable from the API. `--no-pre-validate` skips the check and lets the server decide.

*Required paths through arrays.* A required path may run through an array or blocks field
(`hero.links.link.label`: `label` is required in every row of `hero.links`). The check walks each
required path through objects and arrays: through an array it is required in **every row that
exists** and reported per row (`hero.links[1].link.label`); an **empty** array makes it vacuous; a
missing or null object on the way reports the remaining path (`hero.type` when `hero` is null); a
required list itself is missing when empty. The pre-release rule reported `hero.links.link.label`
missing on every fresh website-template page (`hero.links: []`) — a false refusal; verified fixed on
localhost (the server then published the page with `hero.links` still `[]`). The same walk feeds
`created_as_draft`'s `still_missing`.

*The batched pre-check* of the several-ids and `--where` forms reads every document in one `find`
whose `select` is `id` plus the **top-level** key of each required path (`id,hero,slug,title`), never
the paths themselves: selecting a group together with its own sub-paths makes Payload return the group
narrowed to the scalar sub-paths (`hero: {type}`, `links` dropped), which the walk would read as a
missing array. Blocked ids become per-id failures ("cannot be published: missing required field(s)
…", warning `publish_blocked` in a dry run). **When every id is blocked, nothing is sent** and the
answer is `validation_failed` (exit 5) with one `error.failures[]` entry per id and the hint "set the
missing fields first (pay describe <c> --required-only lists them), or pass --no-pre-validate" —
never `partial_failure` with its "already committed" hint. Some blocked → `partial_failure` (exit 7)
as before.

**9.10.5 References.** Every write body — `create`, `update` (by id and `--where`), `globals update`,
`upload` metadata, the body form of `pay diff`, `apply`, `upsert` and every `sync` item — may hold
`{"$ref": …}` objects anywhere, replaced by the id (or `{relationTo, value}`) they resolve to before
anything is previewed or sent. Grammar, lookup, errors and cost are §9.15.1.

### 9.11 The edit pipeline (`pay blocks` + `pay apply`)

Reordering blocks on a page is the single most common content edit and the one Payload's REST API
models worst: there is no per-row endpoint, so the only way to move one block is to replace the whole
array. Before §9.11 that meant read to a file, edit with jq, write back with `--set-json` — three
commands, a temp file, and four failure modes PayCLI can see and jq cannot:

| Failure | Why it happens | What §9.11 does |
|---|---|---|
| The moved row lands past its anchor | "move row 0 after row 3" is an insert at index **2** of the remaining three rows, not at 4; the anchor shifts when the row is lifted out | `rows.Move` resolves the anchor **before** the removal and recomputes the destination **after** it; every direction is a table test |
| A duplicated row overwrites its original | Payload matches a row by `id`, so a copy that kept its ids rewrites the row it came from and the array **loses** an entry | `pay blocks cp` strips every `id` at every depth, unconditionally |
| A mistyped `blockType` vanishes | Payload **drops** a row whose `blockType` it does not recognise and still answers **201** | `block_type_unknown` (exit 10) against the field's own resolved slugs (§7.10) when they are declared and complete, with `did_you_mean`; otherwise the census warnings of §9.7 |
| A relationship is written back expanded | a read above `--depth 0` replaces `"media": 7` with the whole media document | `populated_relationship` warning naming `field[i].key`, hinting at `--depth 0` |

**9.11.1 The stage contract.** Each stage reads one JSON value on stdin and writes one envelope on
stdout. It accepts a PayCLI envelope **or** a bare document, which is what makes the family usable
against a file. Three rules hold at every hand-off:

1. **An upstream failure stays an upstream failure.** A stage whose stdin holds `ok:false` re-emits
   that error with its original `error.code` and exit status, `command` set to the stage that could
   not run. Re-classifying it locally would turn a `doc_not_found` from `pay get` into a
   `bad_request_body` from `pay blocks mv`, sending the caller to fix a selector that was right.
2. **Upstream warnings are carried forward.** A locale-fallback warning raised by the read is about
   the document the LAST stage will write.
3. **The edit is recorded, not the document.** Every stage appends to `envelope.edits`
   (§10.1), which is the list `pay apply` narrows its write to. `edits.fields` records the **root**
   of the edited location — the object-key prefix up to the first array row: `--field
   layout[2].blocks` records `layout`, `--field hero.items` still records `hero.items`. Payload has no
   per-row endpoint and replaces a whole top-level array on PATCH, so `pay apply` sends all of
   `layout` with the nested change inside it — one key, same as a top-level reorder.
   `edits.ops[].field` records the positional path actually edited (`layout[4].blocks[1].blocks`);
   `next.args.field` is the root. The edited rows are written back **copy-on-write** along the location
   (`rows.Replace`): everything outside the edited array is byte-for-byte the input (verified on stage
   pages 2 and 47 for mv/rm/add/cp/set: `jq 'del(<edited array>)'` of input and output identical).
4. **A rendered document is refused.** An envelope whose `meta.richtext.format` is `md` or `text`
   (§9.12.8.3) fails every stage that passes a document on (mv, rm, add, cp, set, apply) with
   `richtext_rendered` (exit 5); the reading stages (`ls`, `validate`, `pay outline`) accept it.

**9.11.2 `edits` is why `apply` is safe.** The obvious implementation of "write the piped document
back" is to PATCH all of it, and that also rewrites `_status`, `createdAt` and every expanded
relationship — a one-block reorder silently republishing a page. `apply` instead sends
`{field: value}` for each path in `edits.fields`, so a pipeline that moved one block writes one key.
`--all-fields` opts out, drops `id`/`createdAt`/`updatedAt` and **warns** (`apply_all_fields`);
`--field` narrows further and can never widen beyond the piped document. An envelope with no `edits`
is `no_edits` (exit 5), never a silent fall back to writing everything. `pay apply --field PATH`
accepts **top-level** fields only: a path that resolves through a row (`--field layout[1].columns`)
is `invalid_args` ("… is inside a row of layout, and Payload can only write layout as a whole", hint
`pass --field layout`).

`apply` also resolves `{"$ref": …}` objects a `pay blocks … --set-json` stage put into the document
(§9.15.1; before, it sent them verbatim: `validation_failed` on a plain save, a masked 500 with
`--draft`), and runs the §10.2.2/§10.2.3 publication rules and `--reset-nested` (§10.2.1) exactly like
`pay update <id>`.

`apply` is `pay update <id>` with the body assembled from a pipe: same L1 risk level, same
confirmation policy, same audit record, same echo-diff, and literally the same `runUpdateOne`. A
global target routes to `POST /globals/{slug}` and keeps `globals update`'s **L2** level, because
going through a pipeline must not make a global cheaper to confirm.

**9.11.3 The selector grammar** is closed, for the same reason `--path` is not jq (conflict 31):

```
N | -N | first | last | id:VALUE | name:VALUE | type:SLUG | type:SLUG[N]
```

`--before`/`--after` take a **selector**, not an index, because "after the media block" survives the
next stage editing the array and "at index 3" does not. A selector matching several rows is
`selector_ambiguous` (exit 5) with every match listed as a ready-to-paste selector; `rm --all` is the
only way to mean "every match". A selector matching none is `selector_no_match` (exit **4**, the same
not-found class a missing document gets) with the rows that DO exist in the hint. `pay blocks ls`
emits, per row, the shortest selector that addresses it and no other — `id:` whenever the row has
one, because an index is stale the moment another stage inserts or removes a row.

**The field-path grammar.** Real pages nest (stage pages 2: `layout[2]` is a `group` whose `blocks`
holds six `processStep` rows; pages 47: a `grid` inside a `group` whose five `text` rows each carry a
`buttons` array). Before this release `pay blocks ls --field layout.2.blocks` answered `ok:true`,
`count:0` and a `field_absent` warning — it looked for the object key `"2"` on the `layout` array — and
`add` on the same path would have written a one-row array over the six rows. `--field` (every
`pay blocks` verb, and `pay lexical --field`) and every `--set`/`--set-json`/`--set-md`/`--unset` KEY
now take a **field path** that passes through rows:

```
PATH := KEY ( "." KEY | "." N | "[" SELECTOR "]" )*
```

* A step is an object key or a row, decided by **what the document holds at that point**, never by
  the spelling: an array is always entered by a row selector, an object always by a key.
  `layout.2.blocks` and `layout[2].blocks` are the same path.
* Inside `[...]` the full selector grammar applies (brackets nest: `layout[type:group[1]].blocks` is
  the second group). A bracketed selector is parsed eagerly — a malformed one fails before stdin is
  read. A bare dotted step on an array is parsed as a selector lazily (`layout.first.blocks`,
  `layout.id:6ab8….blocks`); `layout.type:group[1].blocks` is ONE step (a key never contains `:`).
* A path starts with a key. Empty segments, a leading/trailing `.`, `[]`, an unclosed `[`, a stray
  `]` are `invalid_args` (exit 5) with the grammar in the hint. Keys cannot contain `.`, `[` or `]`
  (Payload field names do not); a `name:` value with an unbalanced `]` cannot be written inside
  brackets — use `id:` or an index.

A resolved path has three renderings, all reported: **positional** (`layout[2].blocks`, exact for the
document this stage read; what `edits.ops[].field` and errors name), **stable** (`layout[id:6ab8…].blocks`,
each row by its shortest selector — what a LATER stage should be given, because it survives a reorder
of the parent array) and **root** (`layout`, the write unit of §9.11.1 rule 3).

**9.11.4 Field resolution.** `--field` given → parse, then walk the document (`rows.Resolve`). Not
given → (1) the shard's declared blocks fields; (1b) **the cached block census**: the top-level blocks
patterns it recorded for the piped document's collection (same rule as (1): the one the document
carries wins, several carried → `field_ambiguous`, exit 5; none carried and exactly one known → that
one); (2) only then the document's own shape — the one array whose rows **all** carry a `blockType` —
which alone warns `blocks_field_inferred` (hint: ``run `pay blocks learn <slug>` once … or name the
field explicitly with --field``; a bare document without a target gets only the `--field` advice).
More than one shape candidate is `field_ambiguous` with both named, never a guess. Only top-level
fields are ever inferred; a nested field is always named. A pipe stage never builds the census.

The accepted blockType set is looked up by the path's **pattern** (`layout`, `layout[].blocks`, or the
parent-scoped `group.blocks`). `rowField.Accepts` — the list that makes `pay blocks add <slug>` fail
locally with `block_type_unknown` (exit 10) — is set **only** when every slug of the field comes from
a source that is not a guess; slugs sourced `inferred-from-interface-name`, `observed`,
`observed-census`, `mixed` or `unknown` never gate (before, the inferred list refused the real slug on
localhost: `pay blocks add cta` → `block_type_unknown`). A collection with no discovered schema and no
census still edits, with `local_validation_skipped` saying that `blockType` was not checked.

Resolution outcomes, first match wins:

| situation | result |
|---|---|
| the path resolves to an array | edit it |
| `null` at the end of a single top-level key (`layout: null`) | empty array + `field_absent` warning (a null layout is what `add` must fill) |
| `null` at the end, on a row | empty + `field_absent` only when the block schema, the census or another row of the same blockType knows it as an array; otherwise `invalid_args` ("… is null, and nothing says it is an array"), `did_you_mean` = the arrays the row carries. Postgres reads an empty blocks/array field back as `[]`, never null (stage pages 47: every null is a scalar) |
| absent or `null` inside a present top-level group (`hero.linsk`) | when the schema lists the group's sub-fields: a known array → empty + `field_absent`; a known non-array → `invalid_args`; an unknown key → `unknown_field` (exit 5) with did_you_mean among the group's arrays. Without such a schema: empty + `field_absent`, the hint naming the arrays the group does carry |
| a row selector matches no row (`layout.9.blocks`) | `selector_no_match` (exit **4**) naming the array, with the rows that exist in the hint |
| a row selector matches several (`layout[type:text].blocks`) | `selector_ambiguous` (exit 5), matches as ready-to-paste selectors |
| a top-level key is absent and the rest is plain keys | `field_absent` warning + empty array (a `--select` on the read, or a null layout `add` must fill) |
| a key is absent/null and the next step addresses a row (`layuot.2.blocks`) | `selector_no_match` (exit 4) — except an ABSENT key on a row nothing knows (`layout[3].blokcs[1].rows`), which is a typo → `unknown_field` (exit 5) |
| a key absent on a **row** as the last step, and the row's type is known to have it | empty array + `field_absent` naming the source ("other group rows in this document carry it as an array" / "the group block's schema declares it") and an `add … --field <stable>` hint |
| a key absent on a row and nothing knows it (`layout[2].blokcs`) | `unknown_field` (exit 5); did_you_mean = close matches among the row's arrays as full stable paths |
| a key step on an array (`layout.blocks`, the commonest mistake) | `invalid_args`; did_you_mean = `layout[<selector>].blocks` for every row that HAS `blocks` |
| a `[selector]` on an object, a step into a scalar, or a path ending on a non-array (`layout[2]`) | `invalid_args` naming where the walk stopped / the arrays inside that row |

"Known" for an absent row key = the discovered `BlockSchemas[blockType].fields[]` has an entry at
that path with `payload_type` ∈ {`blocks`, `array`}, **or** any row anywhere in the piped document
with the same `blockType` carries an array there (Lexical states and expanded relationships are not
walked). The second is what makes stage work without a schema. No new error codes.

**9.11.5 Every `pay blocks` edit verb is L0** and uses a lenient runtime: no profile, no credential and
no cache are required, because none of them is needed to rewrite an array. Discovery and a cached
census are used when they are there. `pay blocks ls` alone ends the pipe — its data is a listing,
reported as `op_result` so it cannot be mistaken for a document to apply. The catalog verbs of §7.10c
are L0 too, but `types`/`schema`/`example`/`new`/`learn` read the server and need the data runtime
(a profile); `validate` is lenient (no profile → structural checks only + `census_unavailable`).

**9.11.6 `--data @-` unwraps an envelope.** `pay get … | pay update … --data @-` is the shape every
caller reaches for first, and it used to POST `{"ok":true,"data":{…},"meta":{…}}` as the request
body — which Payload accepts with a 2xx and silently drops every key of. The write verbs now detect a
PayCLI envelope (`ok` **and** `v` **and** `data_kind` **and** `meta` together, four keys no Payload
document carries) and use its `.data`, with an `envelope_unwrapped` warning; an error envelope fails
with the upstream's own code. Detection requires all four so a collection with a boolean `ok` column
is never mistaken for an envelope.

**9.11.7 Row-internal keys** (`--set`, `--set-json`, `--set-md`, `--unset`, `add --set`). KEY is a
field path **inside the row** in the §9.11.3 grammar (`link.url`, `links.0.label`,
`links[id:6ab8…].label`, `buttons[0]`). Before this release `--set links.0.label=Buy` replaced the
`links` ARRAY with `{"0":{"label":"Buy"}}` (which Payload drops) and `add --set link.url=/x` stored a
literal `"link.url"` key.

* An array on the way is entered by a row selector and **never replaced**; a selector that addresses
  no row is `selector_no_match` (exit 4) with the full location.
* An absent or null object key is created as an object (a group not filled yet), unless the next step
  addresses a row — then `selector_no_match` (hint `--set-json 'links=[…]'`).
* A path ending on a row (`--set-json 'links[0]={…}'`) replaces that entry. `--unset` of an absent
  path is a no-op; of a row selector matching nothing `selector_no_match`; of a whole row
  (`--unset links[0]`) `invalid_args` pointing at `pay blocks rm --field`; of the row's own
  `blockType` or `id` `invalid_args`.
* **`--unset` CLEARS, it never deletes** (`rows.ClearPath`). Payload matches a row by its id and keeps
  the stored value of every key the row omits, so a deleted key was written back unchanged with a 2xx
  (`blocks set --unset blockName | apply` changed nothing; the echo check cannot see it because
  absent and null diff alike). The cleared value, verified on localhost (Payload 3.87, Postgres):
  `null` for a scalar, rich text or relationship/upload value; `[]` for an array, blocks or hasMany
  field (`null` there is a 500); a named group gets **every sub-field** cleared (`null` for the group
  is a 500, `{}` keeps every stored sub-field). The edit log says `blockName (cleared)`.
* `pay blocks add` expands path keys into the new row (`--set link.url=/x` → `{"link":{"url":"/x"}}`):
  plain keys first, then path keys in sorted order, so `--set-json buttons=[…] --set
  buttons.0.style=fill` composes.

**9.11.8 `pay blocks ls --recursive` / `-r`** lists the rows of the resolved field and, depth-first (a
row, then its children), every row of every array nested inside them — directly on the row or inside
its group objects, never inside a Lexical editor state or an expanded relationship document. An array
qualifies when it is empty or all its elements are objects.

```json
{"field":"layout","count":5,"total":25,
 "accepts":["hero","text","group",…],"accepts_source":"observed-census","accepts_authoritative":false,
 "accepts_pattern":"layout",
 "rows":[
  {"depth":0,"path":"layout[4]","field":"layout","index":4,"id":"6ab9…c1f","block_type":"group",
   "selector":"id:6ab9…c1f","fields":["anchor","blocks",…],"nested":{"blocks":2},
   "arrays":{"blocks":{"kind":"blocks","accepts":["text","grid",…],"accepts_source":"observed-census"}}},
  {"depth":2,"path":"layout[4].blocks[1].blocks[0]",
   "field":"layout[id:6ab9…c1f].blocks[id:6ab9…c1e].blocks","index":0,"id":"6ab9…c15",
   "block_type":"text","selector":"id:6ab9…c15","nested":{"buttons":1},"arrays":{"buttons":{"kind":"array"}}}]}
```

* Every node's `(field, selector)` pair is directly pasteable: `pay blocks <verb> <selector> --field
  <field>` addresses exactly that row (tested for every node of the fixture). With bare rows on stdin
  (`--path .layout` on the read) nodes carry no `field`.
* `nested` maps each array inside the row to its row count, **including empty arrays** — an empty
  nested blocks field is exactly where a caller wants to `add`. `arrays` gives each one's `kind`
  (`blocks|array`) and, when known, `accepts`/`accepts_source`/`accepts_authoritative`.
* `--long` fills `.row` on every node; `--recursive` composes with a nested `--field`. Without
  `--recursive`, `data.field` is the stable form and, for a nested field, `data.path` the positional.
* **`accepts` of the listed field** (`accepts_source`, `accepts_authoritative`, `accepts_pattern`):
  a declared complete list (`rowField.Accepts`) → the shard's source, `accepts_authoritative: true`;
  else the cached census merged with the shard — the parent-scoped set first for a nested field whose
  owning row has a blockType (`group.blocks`), else the position pattern; nothing known → the four keys
  are absent (not "accepts nothing"). **A census list never gates an edit.** Stage pages 47: `layout`
  → 21 slugs `observed-census`; `layout[4].blocks` → 14 at `group.blocks`; the grid inside → `["text"]`.
* **`add <slug>` and `set --set blockType=<slug>` warn from that same set** when it is an observed
  (non-authoritative) one: `unknown_block_type` (the slug occurs nowhere in the cached census; hint
  `did you mean "bentoItem"? pay blocks types pages --field layout …`) or
  `block_type_not_observed_here` (it occurs, but never at this position — e.g. `bentoItem` into a
  group's `blocks`). Warnings only; a declared list still refuses with `block_type_unknown` (exit 10),
  and with no cached census nothing is said.

**9.11.9 Plain array fields in `pay blocks add`.** A plain `array` field (a cta's `links`, a card's
`buttons`) holds rows WITHOUT `blockType`. `rowArrayKind` decides blocks vs array from, strongest
first: a declared accepted set; the collection or parent block schema; the cached census; the array's
own rows (all typed → blocks, none → array; `{}` ignored); rows of the same parent blockType elsewhere
in the document (which decides an EMPTY `buttons: []`).

* `pay blocks add - [--set …|--data …] --field <array>` adds a row with no blockType. On a known
  blocks field `-` is `invalid_args`; `-` with `--template` is `invalid_args`.
* `pay blocks add <slug>` on a field known to be a plain array stores the row **without** blockType
  and warns `block_type_omitted` naming the source and the `-` form.
* A row that would be `{}` is `invalid_args`: `{}` is what an orphan looks like, and Payload stores it
  as a row of nulls.

**9.11.10 Row ids on copy** (`cp`, `add`, `new --from-example`). `rows.StripIDs` strips **row ids
only** — the copied row's own `id` and the `id` of every element of every row array nested in it, at
every depth. It never enters and never strips a Lexical editor state (block node `fields.id`,
link/upload node `id` and a block node's populated upload stay: Payload's lexical hooks key
`nodeIDMap` by them, `richtext-lexical/dist/utilities/recurseNodeTree.js`), nor a relationship/upload
value (an expanded document, a `{relationTo, value}`). This is exactly what the admin's "duplicate
row" does (`@payloadcms/ui` `fieldReducer` `DUPLICATE_ROW` regenerates only paths ending in `.id`).
Before the fix a copied `mediaBlock` with its populated media stripped made `apply` fail with HTTP 500
(localhost); verified after it on stage pages 35 (`fields.id`, `media.id 848`, `folder.id 7` kept;
nested `buttons[0].id` dropped).

**9.11.11 Stale positions and cross-array ids.**

* **`stale_index`** warning: a nested `--field` that enters a row BY POSITION (`N`, `first`, `last`,
  `-N`, `type:SLUG[N]`) of an array an earlier stage of the same pipe moved/removed/added/copied rows
  in. The message names what the position resolves to now; the hint says to take id-based fields
  from `ls --recursive` on the unedited document. (Positional SELECTOR arguments of `rm`/`set` after
  an upstream edit are not checked.)
* An `id:` that is not in the addressed array but exists elsewhere says where ("the row id:X is at
  layout[9].blocks[1].blocks[0], in the array --field layout[id:…].blocks[id:…].blocks"); for an
  anchor, the hint explains that a destination must be in the SAME array.
* `type:SLUG[N]` out of range suggests the `type:SLUG[i]` that exist, never the ambiguous `type:SLUG`.
* Cross-array moves are not a verb: `ls --long` + `add --data @row.json --field …` + `rm`.

### 9.12 Rich text ⇄ Markdown (`internal/lexical`, `pay lexical`, `--set-md`, `--richtext`)

The measured migration agent hand-wrote Markdown→Lexical builders in Python (`t()`, `p()`, `h()`,
`link()`, `root()`) and a jq Lexical→text parser before it could move any content, rediscovering per
project the node versions, link `fields` shapes, list `start`/`tag` and the wrapper items of nested
lists. §9.12 puts one tested conversion in the CLI and wires it into the everyday commands.

**9.12.1 Ground truth — the shapes Payload 3.87 stores.** Verified read-only on 351 stage editor states
(`pages`, `posts`, `customer-stories`, `team-blog`, `jobs`, `lead-magnets`), 23 local ones, and each
node's `exportJSON()` in `lexical`/`@lexical/*` 0.41.0 and `@payloadcms/richtext-lexical` 3.87.1:

| node | keys (sorted) | notes |
|---|---|---|
| root | children direction format indent type version(1) | |
| paragraph | children direction format indent textFormat textStyle type version(1) | 0.41 derives textFormat/textStyle from the FIRST direct text child |
| heading | … tag(h1–h6) | no textFormat |
| quote | element keys | editor import: inline children; imported content also holds paragraphs |
| list | … listType(bullet\|number\|check) start tag(ul\|ol) | a nested list node has indent 0 |
| listitem | … value (+ checked only in a check list) | nested items carry indent = depth; a nested list lives in a **wrapper item** whose only child is the list |
| text | detail format mode style text type version(1) | format bitmask: bold 1, italic 2, strike 4, underline 8, code 16, sub 32, sup 64, highlight 128 (256/512/1024 lower/upper/capitalize) |
| tab | as text, text `"\t"`, detail 2 | |
| linebreak | type version(1) | |
| link | … `fields{linkType,newTab,url}` **or** `fields{doc:{relationTo,value},linkType:"internal",newTab}`, version 3, optional id | `exportJSON` deletes `doc` for custom and `url` for internal |
| autolink | … `fields{linkType:"custom",url}`, version 2 | |
| upload | fields relationTo value format version(3) [id] [pending] | 10 stage uploads are `pending` (unfinished imports) |
| relationship | relationTo value format version(2) | |
| block / inlineBlock | fields{blockType,blockName,id,…} format version(2) / fields version(1) | 230 blocks on stage |
| horizontalrule | type version(1) | |
| table/tablerow/tablecell | Lexical table nodes | 4 on stage |

`direction` is `"ltr"` on 431 stage paragraphs and **null** on 3,129 (imported content); both are
accepted and Lexical's reconciler recomputes it. FromMarkdown writes what the reconciler would.

**9.12.2 The dialect.** CommonMark restricted to what Lexical can hold, plus five extensions (full
table in `internal/lexical/doc.go`):

* Blocks: ATX/setext headings, paragraphs (soft break → space), block quotes with lazy continuation,
  bullet/ordered/task lists nested by indentation, thematic breaks (`---`, `***`, `___`;
  ToMarkdown writes `***`), fenced/indented code (→ a paragraph of code-formatted lines +
  `code_block_flattened`), HTML-comment blocks (placeholders; other comments dropped).
* Inlines: CommonMark's delimiter-stack emphasis, code spans, backslash escapes, entities, hard breaks
  (`\`, two spaces, `<br>`), links with `<…>` destinations and titles (titles dropped), autolinks;
  link reference definitions and reference links (`[text][label]`, `[label][]`, `[label]`; an
  undefined full/collapsed reference raises `link_reference_undefined`). ToMarkdown never writes
  reference links and always escapes `[`/`]`.
* Extensions: `~~strike~~`; format tags `<u>`, `<sub>`, `<sup>`, `<mark>` (the only spelling of those
  formats) and `<strong> <b> <em> <i> <s> <del> <ins> <code>`; **internal links**
  `[t](pay:COLLECTION/ID)` (all-digit ids → number), or by field `[t](pay:COLLECTION/FIELD=VALUE)`
  → `doc.value` `{"$ref": COLLECTION, FIELD: VALUE}` (a reference the write verbs resolve, §9.15.1;
  to-md renders it back); link attributes
  `{target=_blank key=value key="str" key='{json}' autolink}` directly after `)`; **uploads**
  `![COLLECTION:ID]()` (Payload's own Markdown export form); **placeholders** `<!-- lexical:TYPE {json} -->`.
* A bullet list with any task item is a check list (its plain items become unchecked checkboxes:
  `list_became_checklist`, naming the task item's line). Changing the bullet character or ordered
  delimiter starts a new list. A block quote (or code block) inside a list item is flattened
  recursively: its paragraphs/headings — at any quote depth — become the item's inline text joined
  by line breaks, a nested list becomes a wrapper item; a listitem never gets a block child.
  Reference images (`![x][r]`, `![x][]`, `![x]` with a definition) are kept verbatim as text with
  `image_as_text`, like inline images. Quotes/lists nest at most 100 levels; deeper lines are
  paragraph text (`nesting_capped`; 20 000 `>` took 39 s / 400 MB before, now 0.02 s). Input is
  normalised: CRLF/CR → LF, invalid UTF-8 → U+FFFD, a leading U+FEFF byte order mark is dropped.

**9.12.3 Placeholders** carry any node Markdown cannot spell: the node minus `type` and minus the keys
at their export default, so `<!-- lexical:upload {"relationTo":"media","value":12} -->` is a complete,
hand-writable upload node. The JSON is HTML-escaped (no value can contain `-->`); a populated `value`
(read at depth > 0) is reduced to its `id`. A `lexical:` comment that does not hold one JSON object is
an **error** (`bad_request_body`, exit 5) — turning it into text would silently delete a node.
`<!-- -->` and `<!-- pay:… -->` are dropped silently, any other comment with `comment_dropped`.

**9.12.4 ToMarkdown is lossless by construction and verified per paragraph.** Every block is rendered,
**read back** with FromMarkdown and compared in canonical form; a delimiter run CommonMark's flanking
rules defeat in its final context (`**Tier 1 (**0,4 %`, 11 such runs on stage) switches to its HTML
tag, then an all-tags attempt, and only if that fails too is the block written as a placeholder. On
the 351 stage states the only 10 placeholder paragraphs are exactly those holding a `pending` upload.
Properties (tested): lexical→md→lexical is equal in canonical form, and ToMarkdown is a fixed point,
for all 351 stage states (checked during development, not committed — the content is the customer's),
the golden fixtures, 3,000 random adversarial paragraphs and a native fuzz target
(`FuzzMarkdownRoundTrip`, 5 min / 1.7 M inputs clean; the 7 regressions it found are seeds).
Findings (`ToMarkdownIssues`): `lossy_alignment`, `lossy_style`, `lossy_format`, `placeholder`,
`normalized`, `lossy_state_keys` (a non-null key next to `root`, e.g. `{"media": 12, "root": …}`).

**9.12.5 FromMarkdown findings:** `html_as_text`, `comment_dropped`, `image_as_text`,
`code_block_flattened`, `table_as_text`, `list_item_paragraphs_joined`, `list_item_block_flattened`,
`list_became_checklist`, `nesting_capped`, `link_reference_undefined`, `link_url_invalid` (empty or space-containing URL: `linkValidation` runs
for every link node, and a draft save skips it so the failure would surface at publish),
`link_url_relative` (`../brief.md`, `page.html`, `www.example.com` resolve against the page URL on
the site). FromMarkdown writes links without `id`, so the url field's `beforeChange` re-encoding hook
(which runs only for id'd nodes) never rewrites them.

**9.12.6 Package API** (pure): `FromMarkdown(Issues)`, `ToMarkdown(Issues)`, `PlainText`,
`PlainTextWithPlaceholders`, `IsEditorState` (object with `root.type == "root"` and array
`root.children`), `Walk(state, fn(node, path) bool)`, `NodeTypes`, `StripFrontmatter`.
`PlainText` separates blocks by `\n\n`, list items and table rows by `\n`, cells by ` | `; uploads,
blocks, relationships and rules contribute nothing. `PlainTextWithPlaceholders` shares the walker
(`textWalker{placeholders}`) and renders every text-less node instead: `[upload media:12]`,
`[relationship pages:47]`, `[block cta]`, `[inlineBlock badge]`, `***`, `[TYPE]` — for reading only
(it is what `pay diff`'s `from_text`/`to_text` and `pay outline`'s summaries use). Derived keys
FromMarkdown recomputes and ToMarkdown does not preserve: `direction`, paragraph
`textFormat`/`textStyle`, list item `value`/`indent`, link `id`.

**9.12.7 `pay lexical`** — three verbs, `op_result`, **L0**, run with `runOffline` (a lenient runtime
that does not even construct the client or discovery; tested with a transport that fails the test on
any request). Input: `FILE`, or stdin for none/`-`. An upstream error envelope is re-raised with its
own code (§9.11.1). Exit codes 0, 1, 5 (`no_input`, `file_missing`, `bad_request_body`,
`invalid_args`, `not_rich_text`, `richtext_rendered`).

* `from-md [FILE|-] [--as FIELD] [--keep-frontmatter]`: `data` = the editor state; `--as FIELD`
  (dotted for groups) makes `data` the write body `{FIELD: state}`, which `--data @-` accepts via the
  §9.11.6 unwrap (`pay lexical from-md body.md --as content | pay update posts 12 --data @- --draft`).
  An array hop (`layout.0.content`, `layout[0].content`) is `invalid_args` with the `pay blocks set
  --set-md` pipe as hint, as for `--set-md` (it built `{"layout":{"0":…}}`, which empties the array).
  **Front matter** (`lexical.StripFrontmatter`) is stripped with `frontmatter_stripped` (the message
  gives the line count and the first line) unless `--keep-frontmatter`: a `---` line, a body whose
  first line directly follows the fence and whose every line is a YAML `key:` line, list item, comment,
  blank or indented continuation, and a closing `---`/`...`. Anything else is Markdown that starts
  with a thematic break and is converted in full (verified: all 67 real-world Markdown source files strip as
  intended; all 565 stage states render to Markdown that is not taken for front matter).
* `to-md [FILE|-] [--field PATH] [-o|--out FILE|-]`: input is an editor state, a document, a `pay get`
  or a `pay find` envelope — every editor state at any depth is found (`content`,
  `layout[2].blocks[0].content`, `[1].content` in a list). `data = {markdown, fields:[{path, markdown}],
  count, out?}`; several fields are joined, each introduced by `<!-- pay:field PATH -->`. `--field`
  takes the §9.11.3 nested grammar (`layout[id:…].blocks[type:grid].blocks[0].content`); a selector
  that matches no row is `selector_no_match` (4), several `selector_ambiguous` (5), a non-state or
  missing key `not_rich_text` (5, `did_you_mean` = every rich-text path), a malformed path
  `invalid_args`. `-o FILE` writes atomically (0644); `-o -` writes the bare Markdown to stdout and
  moves the envelope to stderr. Findings → `lexical_<code>` warnings with paths
  `FIELD > root.children[3]`.
* `text …`: same input and flags; `data = {text, fields:[{path, text, chars}], count}`.

Findings of every verb are warnings named `lexical_<code>`, one per code, `paths` ≤ 20
(`line N` for from-md, file lines including front matter).

#### 9.12.8 Rich text in the everyday commands

**9.12.8.1 `--set-md KEY=VALUE`** (repeatable) on `create`, `update` (by id and `--where`),
`globals update`, `upload`, `upsert`, the body form of `diff`, and `pay blocks add|set|new`.

* VALUE: `@FILE`, `@-` (stdin), anything else is the Markdown itself (a literal leading `@` is written
  `\@`). `@-` is `invalid_args` when stdin is already taken (`--data @-`, the document of a
  `blocks add|set` stage, a second `@-`; `--data-file -` and `/dev/stdin` count as taking stdin, and
  `--data-file -` reads it). `@-` that reads nothing but whitespace is `no_input` (as `pay lexical
  from-md`): a closed stdin would otherwise clear the field. An `@FILE` with no Markdown (empty, or
  only front matter) converts but warns `set_md_empty`; inline `KEY=` is the silent deliberate clear.
  Inline Markdown holding the two characters `\n` and no real
  line break warns `set_md_literal_newline` (the hint names `@file.md`, a heredoc and `$'…'`).
* The conversion is exactly `pay lexical from-md` (the stored shape is byte-identical, verified), with
  front matter stripped (`frontmatter_stripped`), findings as `lexical_<code>` warnings with
  `paths: ["KEY line N"]`, a broken placeholder `bad_request_body`, a missing file `file_missing`.
* **Write commands — key rules.** A dotted key reaches a GROUP field (`hero.richText`,
  `meta.body`). A key with an array hop (`layout.2.content`, `layout[id:…].content`) is `invalid_args`:
  a write body replaces a whole array, so a row cannot be addressed without it; the hint names the pipe
  (`pay get … | pay blocks set <selector> --set-md <field>=@file.md --field <array> | pay apply`)
  and `pay outline`.
* **Write commands — field check.** A field the shard types as neither `richText` nor `json` (nor
  unknown) is `invalid_args` with no request. A field the shard does **not** type (on an
  introspection-disabled site every group sub-field) is checked against stored values before
  anything is previewed or sent, because Payload stores an editor state sent to a text field as JSON
  text (§2.9): (1) the target document's newest version (update by id, upsert's match, `upload
  --replace`, the global), (2) else up to 10 documents holding the key
  (`GET /COLL?where[KEY][exists]=true&select[KEY]=true&limit=10&depth=0&draft=true`). A non-empty
  string/number/boolean/array there **refuses** the write (`invalid_args`, naming the document and
  the deliberate path `pay lexical from-md FILE.md --as KEY | pay update COLL ID --data @-`, keeping
  the command's `--draft`/`--locale`); a
  non-editor object warns `set_md_not_richtext`; no evidence at all warns `set_md_field_unchecked`; a
  failed read never blocks. A known richText/json field costs no request; `pay diff` only warns (after
  the same sampling, naming the sampled document).
* **`pay blocks add|set` — row-relative keys** in the §9.11.7 grammar (`content`, `section.body`,
  `items[0].answer`); `pay blocks new` takes dotted keys into the template row; with `add --template`
  the converted state replaces the template's empty rich text. `set_md_not_richtext` when the key
  holds a non-empty non-rich-text value, or the cached census observed the key of that blockType with
  another kind. `pay blocks set` counts `--set-md` as something to change.

**9.12.8.2 `--richtext json|md|text`** (read) on `get` (id and `--slug`), `find`, `globals get` and
`versions get`. Default `json` (the stored state; nothing recorded). Aliases: `markdown` → md,
`plain`/`plaintext` → text, `lexical` → json; anything else `invalid_option` before any request.
Every editor state anywhere in `data` (groups, blocks at any depth, array rows, `version.*`, every
document of a list) is replaced by a string — `md` = `lexical.ToMarkdown`, `text` = `lexical.PlainText`
— and `meta.richtext = {format, rendered, paths[, paths_truncated]}` records it (paths ≤ 100). Only
the lossy `lexical_lossy_*` findings are raised on a read. `--output raw` prints the rendered data;
`--output jsonl` prints rendered bare documents with `meta.richtext` on the stderr summary; `find`'s
`next.cmd` re-emits `--richtext`. Verified on stage: `pay get pages 47 --draft --richtext md` →
`rendered: 19`.

**9.12.8.3 A rendered document is never written back.** `richtext_rendered` (exit 5), nothing sent,
when (1) an ENVELOPE whose `meta.richtext.format` is `md`/`text` reaches `pay apply`, a `pay blocks`
stage that passes a document on, `--data @-`/`--data @FILE`/`--data-file` of
create/update/globals update/upload/diff, or `pay lexical to-md|text` (reading verbs — `blocks ls`,
`blocks validate`, `pay outline` — accept it; refusing at the first edit stage is deliberate, because
that stage's own output would no longer carry the marker); or (2) a BARE body (jsonl, a jq extract)
holds a string where the shard declares a `richText` field — on the write paths and `pay apply`; or
(3) the body holds a non-empty string where the **stored document** holds an editor state (rows matched
by `id`, else by index when the `blockType` agrees; `""` is Payload's empty rich text and passes). (3)
needs no schema and no census, so it covers block rich text on introspection-disabled sites: a dry run
uses its pre-image, `upsert`/`plan`/`sync` their match, and a real `update <id>`, `apply` or `globals
update` whose body holds block rows (or an index-keyed object) reads the newest version once before the
confirmation (a failed read never blocks). Verified on stage (read-only): `pay get pages 35 --richtext md
--output raw` written back → refused, 58 paths, with and without a cached census. The same comparison
refuses an OBJECT where the document holds an ARRAY (`bad_request_body`, exit 5): `{"layout":{"0":…}}`
(what `--set layout.0.x=…` builds) makes Payload replace the whole array — verified on localhost
(Payload 3.87 / Postgres: 2 rows → 0, still 200). Before any read, an object with an all-digit,
`blockType` or `id` key at a path the shard types `array`/`blocks` or the cached census knows as a blocks
field is refused the same way (an object keyed by locale, `--locale all`, is not). `pay diff` warns
(`richtext_as_string`, `array_as_object`) instead of refusing. `update --where` and `create` have no
per-document pre-image: a string in a block row's field the cached census observed as `richText` is
only a warning there, `richtext_as_string` (§9.7). A `pay plan`/`sync` manifest item hits the same error with the
manifest's hint (re-export without `--richtext`, or convert with `pay lexical from-md`).

### 9.13 `pay outline`

```
pay outline <collection> <id> [--draft]
pay outline <collection> --slug SLUG [--draft]
pay outline --global SLUG [--draft]
pay get <collection> <id> --depth 0 [--draft] | pay outline      # any envelope or bare document
```

The agent used 54 of 77 `pay` calls through jq, mostly to find out what a page contains and how to
address a row. `pay outline` answers that in one call. `op_result`, **L0**.

* **Reading:** always `depth=0`; `--draft` adds `draft=true`. `--slug` uses §9.17's resolver. 1 request
  (2 with `--slug`). With no arguments the document is read from stdin with the lenient runtime (no
  profile, no request; an envelope's warnings and target are carried); an array on stdin is
  `bad_request_body`, empty stdin `no_input`. Invalid combinations are `invalid_args` before any request.
* **Rows** (`rows.DocTree`): every row of every array of objects — top-level fields and group fields,
  at every depth, never into rich text or an expanded relationship — in document order (a row, then its
  nested rows); arrays holding blockTypes first, then the other row arrays; scalar arrays are not rows.
* **Row object:** `depth`, `path` (positional), `field` (the stable `--field` for the row's array),
  `selector` (shortest, `id:` whenever the row has one), `index`, `id`, `block_type`, `block_name`,
  `label`, `summary`, `children`, `nested` (each array inside → its row count, empty included),
  `flags` (always an array). `field` + `selector` are exactly what `pay blocks … --field '<field>'
  <selector>` accepts (verified for all 75 rows of stage pages 47 and 2).
* **summary:** the first non-empty of `heading, title, headline, eyebrow, name, label, question, phase,
  caption, tag, alt` (rich text read as plain text), then the same one group level down, then the first
  rich-text field as `PlainText` (or `PlainTextWithPlaceholders` when it has no text), then
  `[image 5]`-style for media keys, then `url`; joined with ` · `, whitespace collapsed, ≤ 80 chars.
* **flags:** `empty_row` (a `{}` row — the orphan of §10.2.1), `no_block_type` (an untyped row in a
  blocks array), `unknown_type` (a blockType the knowledge does not name — only when there IS knowledge).
* **label/knowledge:** `discovery.NewBlockKnowledge` over the **cached** census (never built) and the
  entity's shard; `label` is the declared block label and absent when unknown.
* **counts:** `{rows, blocks, by_type{slug: n}, flagged{flag: n}}`. `--output table` renders an
  indented tree (`OUTLINE SUMMARY FLAGS`).

Verified on stage: `pay outline pages 47 --draft` → 48 rows (26 blocks), 1 request; the piped form → 0
requests.

### 9.14 `pay diff` — structural diff of two documents

Proving that a write touches exactly the one block it should used to take a read to a file, a hand
edit, a second read and `diff <(jq -S …) <(jq -S …)` — a line diff that reports every re-indented line
and every index shifted by an insert, and knows nothing about how Payload applies a partial body.
`pay diff` is that comparison done structurally by `internal/docdiff`, with Payload's write semantics
applied. The same engine produces every dry-run `data.diff` (§12.2), `pay plan`'s per-item diff
(§9.15) and `pay versions diff` (§9.14.8).

#### 9.14.1 Forms

| Form | left | right | requests |
|---|---|---|---|
| `<c> <id>` + body (`--data-file`/`--data`/`--set`/`--set-json`/`--set-md`, combined as §9.10.2) | server, `?depth=0&draft=true` | `docdiff.ApplyPatch(left, body)` | 1 GET |
| `<c> <id> --draft-vs-published` | server, `?depth=0` (no draft: the stored row) | server, `?depth=0&draft=true` | 2 GET |
| `<c> <idA> <idB> [--draft]` | server `idA` | server `idB` | 2 GET |
| `--global <slug>` + body or `--draft-vs-published` | as above, against `GET /globals/{slug}` | | 1–2 GET |
| `--left FILE --right FILE` | file (or `-`) | file (or `-`) | 0 |

* Every form is **L0**: nothing but GETs is ever sent, the body form included; nothing is audited;
  `--dry-run` is accepted as a no-op. The flag combination is validated **before** any dependency is
  built (an impossible one is `invalid_args` listing the five forms, under a panicking transport); the
  file form builds no client and needs no profile.
* `--draft-vs-published` on an entity whose manifest says `drafts: false` → `feature_unavailable`
  (exit 10) before the read; unknown drafts support is attempted (§9.3 tri-state).
* Local sides accept a bare value or a PayCLI envelope (`.data`; a `version` envelope by its `version`
  sub-document; an error envelope re-raises the upstream code). Unreadable file → `file_missing`,
  invalid JSON → `bad_request_body`, empty → `no_input` (exit 5).
* Exit status is **0 whether or not the sides differ**; callers branch on `data.identical`.

#### 9.14.2 Output

```json
{"left":  {"source":"server","collection":"pages","id":2,"draft":true,"status":"published","updated_at":"…"},
 "right": {"source":"server+patch","path":"page-2.json","collection":"pages","id":2,
           "semantics":"payload-patch","body_keys":["_status","layout","meta","title"]},
 "identical": false,
 "summary": {"total":1,"added":0,"removed":0,"changed":1,"moved":0,"rich_text":0,"fields":["layout"]},
 "changes": [{"op":"change","path":"layout[2].blocks[3].question","from":"Welche Angabe fehlt …?","to":"Welche Angabe fehlt … (lokal)?"}]}
```

* `source` ∈ `file | stdin | server | server+patch`. `summary.fields` — the sorted top-level fields any
  change touches: exactly the keys a minimal PATCH must carry.
* `changes[]`: `add` → `to`; `remove` → `from`; `change` → `from`/`to`; `move` → `from_index`,
  `to_index`; row-level changes carry `kind: "row"`, `id`, `block_type`; rich-text changes carry
  `kind: "richtext"`, `from_text`/`to_text` and (unless `--full`) no `from`/`to`; `detail: "formatting"`
  marks a rich-text change whose plain text is identical.
* `--output table` renders `OP PATH FROM TO` (or `(no differences)`); `--path` works on the object.
* Warnings: `never_published` (the `--draft-vs-published` left side is not published),
  `field_not_on_document` (body top-level keys the stored document does not have — Payload returns
  every field it defines, so an absent key is almost always a typo it would drop), `body_id_mismatch`,
  `body_publishes` (§10.2.2), `envelope_unwrapped`, the §9.10 coercion warnings, the §9.7 block
  warnings, locale warnings, `set_md_not_richtext`, and `data_redacted` (§9.14.6).

#### 9.14.3 PATCH semantics (`docdiff.ApplyPatch`) — verified against payload@3.87.1

The right side of the body form is what Payload would STORE, not the body itself:

1. **Absent keys keep their stored value** (`fields/hooks/beforeValidate/promise.js`:
   `getFallbackValue` → `cloneDataFromOriginalDoc(siblingDoc[field.name])`).
2. **Named groups merge key by key** (rule 1 recursed with the stored group as sibling document).
3. **Arrays/blocks replace; rows merge by id** (`fields/hooks/beforeChange/getExistingRowDoc.js`):
   a body row whose `id` equals a stored row's gets that row as its sibling document; any other row
   is new; rows the body omits are gone.
4. **Non-object rows are stripped** (`fields/hooks/beforeValidate/stripNullRows.js`).
5. **The base is the newest version** (`updateByID.js` l.79), and a draft save does not update the main
   row (`utilities/update.js` l.29/l.253). Hence the body form reads `?draft=true`, and
   `--draft-vs-published`'s left side (no `draft`) is the stored main row.
6. **A body `_status: "published"` publishes, even with `?draft=true`** (`isSavingDraft`, §2.9).
   `pay diff` warns `body_publishes`; the write commands act on it (§10.2.2).

`ApplyPatch` models 1–4 exactly, ignores the body's top-level `id`, and replaces rich text, scalars and
scalar lists wholesale. It deliberately does not merge a stored row into a body row with the same id
but a different `blockType` (one whole-row change). Not modelled without the schema, and stated where
results are shown: a `json` field holding an object merges like a group unless passed in
`Options.LeafPaths`, and a NEW row's unsent keys are absent where Payload fills in defaults. Flags
(`--draft`/`--publish`) and server hooks (`publishedAt`) are not part of the body and not predicted.
Verified on localhost: predicted and actual change lists of a partial body were identical
path-for-path and value-for-value except those two stated limits.

#### 9.14.4 The diff algorithm (`docdiff.Diff`)

* Paths use the §10.2 notation (`layout[2].blocks[1].title`); every index addresses the RIGHT document
  except the last index of a row removal (its LEFT position). Order is deterministic: keys sorted;
  within an array, removed rows first, then the right rows in order.
* **Ignored by default:** `createdAt`, `updatedAt` at every depth (`--include-timestamps` keeps them);
  the documents' own root `id` in the two-id form. `--ignore PATH` (repeatable) ignores a path and its
  subtree; `[]`/`[*]` match any index; a pattern never matches a sibling that merely shares a prefix.
* **Null ≡ absent** as values; the op follows key presence (`Options.NullDistinct` reports the
  difference; not a CLI flag). **Numbers** compare by value. **Scalar arrays** are one value.
* **Row arrays** are matched **by id** when every id on each side is unique and at least one is
  shared; otherwise **by position**, and each row's own `id` is then not compared (two independently
  generated ids are noise). In id mode: unmatched left rows → `remove`, unmatched right → `add`,
  matched rows outside one longest increasing subsequence of left indices → `move` — so one insert is
  one `add`, never a cascade of moves. A **`blockType` change** of paired rows is ONE `change` with
  `kind: "row"`.
* **Rich text** (an object whose `root.type` is `"root"`) is compared structurally; a difference is ONE
  `change` with `kind: "richtext"` and `from_text`/`to_text` from `lexical.PlainTextWithPlaceholders`
  (§9.12.6), so a swapped image or block is visible. States nested inside a reported value are
  replaced by `{"$richtext": "<plain text>"}` unless `--full` (the marker key cannot be a field name).
* Inputs are never mutated and reported values never alias them.

#### 9.14.5 Package API (`internal/docdiff`, pure)

```go
type Op string   // OpAdd "add" | OpRemove "remove" | OpChange "change" | OpMove "move"
const KindRichText = "richtext"; const KindRow = "row"; const RichTextMarker = "$richtext"
var DefaultIgnoreKeys = []string{"createdAt", "updatedAt"}
type Change  struct { Op Op; Path, Kind string; ID any; BlockType string; FromIndex, ToIndex *int;
                      From, To any; FromText, ToText *string; Detail string }
type Summary struct { Total, Added, Removed, Changed, Moved, RichText int; Fields []string }
type Result  struct { Identical bool; Summary Summary; Changes []Change }     // Changes never nil
type Options struct { IncludeTimestamps bool; IgnoreKeys, IgnorePaths, LeafPaths []string;
                      NullDistinct, Full bool }
func Diff(left, right any, opts Options) Result
func ApplyPatch(current, body map[string]any, opts Options) map[string]any
func DiffPatch(current, body map[string]any, opts Options) Result   // Diff(current, ApplyPatch(…))
func Equal(a, b any) bool; func JoinKey(path, key string) string; func JoinIndex(path string, i int) string
func RootField(path string) string                                   // "layout" for "layout[2].x"
```

Consumers (dry-run diffs, `plan`/`sync`, `upsert`'s unchanged test) call `DiffPatch(current, body,
opts)` with the current document read at `depth=0` (`draft=true` where drafts exist);
`Result.Identical` is "no write needed" and `Summary.Fields` the minimal PATCH key set.

#### 9.14.6 Redaction

The envelope's redaction is key-based, and in a diff a secret's field name appears only in `path`:
without an extra rule `pay diff users 1 2` printed API keys (reproduced before the fix). Every
structural diff therefore masks `from`/`to` of a change whose path ends in a `redact.Key` (`apiKey`,
`sessions[0]`, anything matching `token|secret`), keeps the change, and raises `data_redacted` naming
the paths. `--no-redact` shows the true values.

#### 9.14.7 Verified live (read-only on stage)

`pay diff pages 47 --draft-vs-published` → identical, 2 GETs; pages 69 → 3 changes + `never_published`;
the unmodified `pay get pages 2 --depth 0 --draft` envelope as `--data-file` → `identical: true` +
`envelope_unwrapped`; the same export with one nested question edited → exactly one change, 1 GET; a
rich-text edit plus a row move compared with `--left/--right` → `move layout[1]` + one `richtext`
change, 0 requests.

#### 9.14.8 `pay versions diff` on docdiff

`pay versions diff <collection>|--global SLUG <vA> <vB> [--ignore PATH]… [--include-timestamps]
[--full]` compares `docdiff.Diff(versionBody(A), versionBody(B))` with the records' own `id`/`_id`
ignored. Two GETs, unchanged; redaction as §9.14.6. The output is a **compatible extension** of the
v0.1.0 shape — `a`, `b`, `changed`, `changes[].path|kind|a|b` keep their meaning:

```json
{"a":"20","b":"37","identical":false,"changed":3,
 "summary":{"total":3,"added":0,"removed":0,"changed":2,"moved":1,"rich_text":1,"fields":["layout","slug"]},
 "changes":[{"path":"slug","kind":"changed","a":"old","b":"new","op":"change"},
  {"path":"layout[1]","kind":"moved","a":null,"b":null,"op":"move","value_kind":"row","id":"6ab8…","block_type":"cta","from_index":3,"to_index":1},
  {"path":"layout[2].content","kind":"changed","a":"Alter Text","b":"Neuer Text","op":"change","value_kind":"richtext","a_text":"Alter Text","b_text":"Neuer Text"}]}
```

`kind` is the v0.1.0 word `added|removed|changed` plus new `moved`; `a`/`b` are docdiff's `from`/`to`
(a rich-text change carries its plain text there too; `--full` the editor states); new keys `op`,
`value_kind`, `id`, `block_type`, `from_index`/`to_index`, `a_text`/`b_text`, `detail`, and top-level
`identical`, `summary`; `changed` = `len(changes)`. `--output table` renders like `pay diff`.
**Semantic changes** (documented, keys unchanged): rows matched by id when shared, else by position
with row ids ignored — Postgres gives version snapshots different row ids, so the old leaf diff
reported **111** changes for stage pages 47 versions 699 vs 687 where there are **7**; a blockType change
is one row change; a rich-text edit is one change; timestamps ignored at every depth; null equals absent.

### 9.15 Idempotent writes — `$ref`, `pay upsert`, `pay plan`, `pay sync`

The migration agent wrote by hand a "find by slug → update, else create" script with an `ids.json`
state file, a manual `pay find … --where 'slug eq x'` for every relationship id, and a manual
`diff <(jq -S …)` to see whether a re-run would change anything — and published pages by accident,
because an edited export carries `_status: "published"`. §9.15 replaces all of it with declarative,
idempotent writes. `internal/manifest` (reading, `$ref` grammar, ordering) is pure apart from file
reads; resolution and writes live in `internal/cli/{refs,upsert,plan,sync}.go`.

#### 9.15.1 References: `{"$ref": …}` in every write body

| Form | Replaced with |
|---|---|
| `{"$ref":"pages","slug":"about"}` | the id: `12` |
| `{"$ref":"pages","slug":"about","$as":"polymorphic"}` | `{"relationTo":"pages","value":12}` |
| `{"$ref":"pages","slug":"about","locale":"de"}` | several keys = several equals-matches (AND) |
| `{"$ref":"posts","where":{"title":{"like":"Launch"}}}` | any Payload where object (ANDed with equals keys) |
| `{"$ref":"@home"}` / `{"$ref":"@home","$as":"polymorphic"}` | the id of plan item `home` — **plan/sync only** |

Grammar (`manifest.Parse`): `$ref` a non-empty string; `$as` ∈ `id` (default) | `polymorphic`;
`where` a non-empty object; every other key a field name (dotted keys reach groups) whose value is a
string, number or boolean; any other `$`-key is an error; at least one criterion; `@key` takes only
`$as`. Malformed → `bad_request_body` naming the path (`$ref at layout[2].links[0].link.reference: …`).

* Resolved **before anything is previewed or sent**, so `--dry-run` shows the real ids, by the shared
  bounded lookup `Deps.lookupDocs` (also used by §9.17): `GET /{coll}?where=…&limit=2&depth=0&select[id]=true`,
  plus `draft=true` where drafts exist (a draft-only document is a valid target) and the write's
  `locale`. Field names are checked against the shard first (`unknown_field`, 0 requests); a `where`
  goes through `query.ValidateWhere`; an unknown collection is `collection_unknown`.
* 0 matches → **`ref_unresolved`** (exit 4) with a runnable `pay find` hint; 2+ → **`ref_ambiguous`**
  (exit 5) in the `slug_ambiguous` shape (total in the message, ids in `did_you_mean`). Answers are
  cached per invocation; a body without `$ref` costs nothing (`manifest.HasRefs`).
* Wired into `create`, `update` (by id and `--where`), `globals update`, `upload` metadata, the body
  form of `pay diff` (in the locale the document is read in), `upsert`, every sync item, and
  `pay apply` — all through `Deps.prepareWriteBody` (with §9.15.2's mask handling). A `@key` reference
  outside a plan is `bad_request_body`. A `$ref` inside a `json` field is still resolved (no escape form).
* **relationTo check** (`refScope`, server-resolved and plan-local references alike). Postgres ids are
  small serials, so a posts id written into a categories relationship usually IS some category's id:
  Payload stores it and the link points at an unrelated document. So the collection a reference names
  must be one its field accepts: a reference at `X.value` whose sibling `X.relationTo` is a string (a
  written-out polymorphic value, a Lexical link's `fields.doc`, an upload/relationship node) must name
  that collection; otherwise the field at the reference's path — `Shard.Field` with indexes stripped, or,
  below a block row, `Shard.BlockSchemaFor(blockType)` — must list it in `relation_to`. A mismatch against
  an authoritative `relation_to` (graphql, configured, project-source) is **`bad_request_body`** naming the
  path, the field and its relationTo, before anything is sent. Where `relation_to` is unknown or only
  observed (REST-only discovery: stage's `pages.parent` is typed `number`), one **`ref_target_unchecked`**
  warning lists the paths (per plan: the item keys). References inside rich text without a sibling
  `relationTo` are not checked.
* Markdown names documents by slug with `[t](pay:COLLECTION/FIELD=VALUE)`: `doc.value` becomes
  `{"$ref": COLLECTION, FIELD: VALUE}` (§9.12), resolved like any other reference. A `{relationTo,
  value}` whose value is a non-numeric string for a collection with `id_type: number` (the dead link
  `[t](pay:pages/about)` writes) warns **`relation_id_not_numeric`** (0 requests: the id type comes from
  the discovery manifest).

#### 9.15.2 `pay upsert <collection>` — create or update one document (L1)

1. The body is built exactly like `create`/`update` (§9.10.2, envelope unwrap, §9.7 block warnings).
   Server-owned top-level `id`, `createdAt`, `updatedAt`, `globalType` are never sent; **`_status` is
   dropped** with `body_status_ignored` (§9.15.4). Values equal to the redaction mask `"<redacted>"`
   (an export of a secret-looking key) are never sent (writing the mask back would destroy the stored
   value), and how depends on how Payload stores what surrounds them (`internal/cli/masked.go`): in a
   **merged** place — a top-level field, a group key, a key of a row — the key is left out with
   **`redacted_not_sent`** (a PATCH keeps the stored value; a new document or row gets the default).
   Inside a **wholesale** value — a Lexical editor state anywhere, or a `json` field of the shard —
   leaving a key out would DELETE it (Payload stores the value whole), so the masked value is **put back
   from the matched document** at the same place (rows paired by id, else index and block type; Lexical
   nodes by index and only between nodes of the same `type`). Only when there is nothing to restore
   from (a create, a node the stored value lacks) is the key removed, with **`redacted_removed`**. Stage
   page 35 stores a literal `"<redacted>"` at `layout[16].content…fields.media.ogQuelle.hash`: its
   unmodified export restores that literal and plans `unchanged`. The same handling runs in every write
   verb (`create`, `update`, `apply`, `globals update`, `upload`, the body form of `pay diff`) via
   `Deps.prepareWriteBody`, which reads the document being written (newest version, depth 0) only when
   a wholesale value holds a mask; a bulk `update --where` has no single document to restore from.
   A null is never masked on read (§5.3): masking it made a get → edit → write loop store the string.
   A body `id` that is not the matched id warns `body_id_mismatch`.
2. References are resolved (§9.15.1).
3. `--match` (default `slug`; comma-separated or repeated; dotted allowed; `--match id` reads the
   body's `id`) names fields whose **values come from the body**. A missing/null value →
   `invalid_args`; non-scalar → `invalid_args`; unknown field → `unknown_field` (0 requests). A match
   on `id` that finds nothing is `doc_not_found` (exit 4), never a create (the copy would get another id).
4. One lookup (`lookupDocs`, AND of equals, newest draft where drafts exist, whole documents): 2+ →
   **`match_ambiguous`** (exit 5, ids in `did_you_mean`); 0 → create; 1 → that document at depth 0 is
   the diff base.
5. Decision: `sent = alignToStored(current, body)` (§9.15.5), `docdiff.DiffPatch(current, sent)` —
   identical and no status change → **unchanged: nothing written, no backup, no audit record**; else
   update, sending the top-level fields that change (`Summary.Fields`) of `sent`, **plus every
   top-level string, number or boolean field the body declares whose value equals the stored one**
   (`unchangedScalars`; never upload fields Payload derives from the file — `url` + `filename` would
   make it fetch the file again), plus the status body. The re-sent scalars are what keep presence-based
   hooks from overwriting them: the website template's `populatePublishedAt` sets `publishedAt` to now
   whenever `req.data` has none, so an edit that sent only the changed label moved the public date and
   needed a second sync. `fields` still lists only the changed fields. `--force` sends the whole body
   anyway (`meta.upsert.forced: true`); `--ignore PATH` removes paths from the unchanged test, and an
   ignored field that differs is not re-sent. After an update at `--depth 0`, the echo is compared with
   the declared fields the write did not send; a difference (a hook rewriting a group or array) warns
   **`hook_rewrote_unsent`** — running the upsert again writes the declared value back.
6. A create in an upload collection is refused before any write (`file_missing`, hint `pay upload …`).
7. The write goes through `runCreateOne`/`runUpdateOne` with command `upsert`: backups (update half),
   audit (`command: upsert`, `action: create|update`), echo-diff, `created_as_draft`, the §12.2 dry run
   and L1 confirmation are the create/update ones.
8. After the write, each match field of the echo is compared with the value used; a difference (a
   slug-formatting hook) warns **`match_value_normalized`** naming the stored value — otherwise the next
   upsert would not find the document and create another. A create failing `validation_failed` on a
   match field gets a `pay find <c> --where '<f> like <word>'` hint.
9. `--publish`/`--draft` on a collection without drafts → `feature_unavailable` (exit 10).

Output: `data_kind: doc` (for `unchanged` the current newest version, `changed` all zero) and
`meta.upsert = {action: created|updated|unchanged, matched_by: {field: value}, id, forced?}`.
`--dry-run` → `op_result` with `action` (plan vocabulary `create|update|unchanged`), `matched_by`,
`id`, `write_mode`, `fields`, `diff`, `would_affect`, `request` and `would_backup`; an unchanged dry
run has `would_affect: 0` and no `request`.

#### 9.15.3 The manifest format

A manifest **file** holds one item object or a JSON array of items. Arguments: a file; a **directory**
(every `*.json` below it, recursively, lexical path order, names starting with `.` skipped); or `-`
(stdin, once). A saved PayCLI envelope is read as items too: `pay get` (`doc`) → one item, `pay find`
(`doc_list`) → one per document, `pay globals get` (`global`) → one global item; each with status
`keep`, collection from `target.slug`, match `slug` — or `id` when the document has no slug (stage
`ctas`, `media`). Any other envelope, an error envelope, an empty file, two JSON values in one file,
an empty array, or an envelope **read above `--depth 0`** (an expanded relationship outside rich text;
up to 5 paths named, hint `pay get <c> <id> --depth 0 --draft`) is `manifest_invalid`.

```jsonc
{
  "type": "object",
  "additionalProperties": false,        // except "$"-keys ("$schema", "$comment"): ignored
  "properties": {
    "key":        {"type": "string", "pattern": "^[^@\\s][^\\s]*$"},
    "collection": {"type": "string"},   // exactly one of collection | global
    "global":     {"type": "string"},
    "match":      {"oneOf": [{"type": "string"},
                             {"type": "array", "items": {"type": "string"}, "minItems": 1, "uniqueItems": true}],
                   "default": ["slug"]},                      // collections only
    "status":     {"enum": ["keep", "draft", "published"], "default": "keep"},
    "locale":     {"type": "string"},
    "data":       {"type": "object"}    // required; may hold {"$ref": …} anywhere
  },
  "required": ["data"]
}
```

* **Keys** default to `<collection>/<match value…>` (`pages/about`, `@<locale>` appended when the
  item sets one; globals `globals/<slug>`). Two items naming the same document or global are
  `manifest_invalid`. Unknown keys are rejected with did_you_mean.
* **Exported ids** (`body_id_mismatch`): an item's `data.id` is never sent but is compared with the
  match result — a different id, or no match (a create), warns ("… so this CREATES a new one"; hint
  `pay update <c> <id> --set slug=<new> --dry-run`). This is the rename trap: editing an export's slug
  used to plan a silent full copy next to the original.
* **Every problem is reported at once** — parse, key/duplicate and per-item planning problems — as
  `error.failures[] {id: "<key> (<file>#<i>)", code, message}`; the error code is the common code when
  all agree, else `manifest_invalid`.
* **Linking:** a reference names a plan item when it is `@key`, **or** an equals-reference whose
  collection, field set and values are exactly another item's collection and match. A linked item that
  exists resolves with no request; one that does not exist yet is a **dependency** created first. A
  reference to a global item is refused (globals have no id).

#### 9.15.4 Publication state: `status`, write modes, and `_status` in data

`_status` is **never** taken from the body in upsert and sync (warning `body_status_ignored`, one per
invocation listing the item keys), because an exported published page written "as a draft" would
publish (§2.9). The state is the item's `status` (upsert: `--publish` / `--draft` / neither = keep):

| status | document exists, newest version | write mode | request |
|---|---|---|---|
| `published` | any | `publish` | body `_status: "published"`, no `?draft` — live, full validation |
| `draft` | any | `draft` | `?draft=true` — a draft version; **never unpublishes** |
| `keep` (default) | `published` | `save` | plain save, no `_status` — stays published |
| `keep` | `draft` | `draft` | `?draft=true`; on a **published** document with a pending draft the live version does not change: warning `saved_as_draft` |
| `keep` / `draft` | does not exist | `draft` | `POST ?draft=true` (`created_as_draft` says what publishing will need) |
| `published` | does not exist | `publish` | `POST` with `_status: "published"` |
| any | entity without drafts | `save` | plain; `draft`/`published` there is `feature_unavailable` at plan time |

Drafts support unknown: the document's own `_status` decides. A status-only change is an `update` with
`fields: []` and body `{"_status":"published"}`. `keep` on a published document with a pending draft
is a `draft` write (the newest version is a draft), so a sync never trips §10.2.3's refusal — and it
never takes the change live either. Because the match reads only the newest version, an update item
whose newest version is a draft costs **one more read**, the stored row's `_status`
(`select=_status`, no `?draft`), so the envelope can say what that write does to a live document:

* `keep` over a published row → **`saved_as_draft`** ("… saves this change into that draft: the live
  document does not change"; hint `--publish` / `"status": "published"` and
  `pay diff <c> <id> --draft-vs-published`) — on `plan`, `sync --dry-run`, the real sync and
  `upsert` (dry run and real run). A draft write never reaches §10.2.3's guard, so nothing else says it.
* `published` over a published row → **`publishes_pending_draft`** (§10.2.3's warning) on `plan` and
  `sync --dry-run`, which never reach the update path; the real sync and `upsert --publish` get it
  from `runUpdateOne`'s guard, once.

The plan item then also carries `stored_status: "published"` next to `current_status: "draft"`.
A failed read adds no warning (a publish still runs the guard; a draft save cannot take anything
offline).

#### 9.15.5 The plan (`buildPlan`)

1. **Load** (`manifest.Load`): files, envelopes, keys, duplicates.
2. **Target**: entity lookup, status vs drafts, data hygiene (server-owned keys, `_status`, the mask),
   locale (the item's or the root one, validated), match values.
3. **Read**: collections — one `lookupDocs` per item (limit 2, depth 0, newest draft, whole documents):
   0 → create, 1 → current + id, 2+ → `match_ambiguous`; globals — `GET /globals/{slug}?depth=0[&draft=true]`.
4. **Link** references to plan items; an edge only towards items that do not exist yet.
5. **Order** (`manifest.Order`): Kahn's algorithm, lowest load index first among ready items, so an
   independent manifest keeps load order. A cycle among new documents → **`ref_cycle`** (exit 5) naming
   it (`pages/a → pages/b → pages/a`). A cycle between existing documents is fine.
6. **Resolve and decide**: references replaced (a pending create by a placeholder string, so a
   dependent's diff still shows the change); create → `fields` = every body key; existing →
   `sent = alignToStored(current, body)`, `DiffPatch(current, sent)`, unchanged or update with
   `fields = Summary.Fields` — **the write sends those fields of `sent`** (plus the unchanged declared
   scalars, §9.15.2 step 5), so the diff is exactly the write. Masked values inside rich text or json
   are restored from the stored document before the diff (§9.15.2 step 1).
   * **`alignToStored` — row ids.** A body row without an id is a NEW row to Payload, so every key it
     leaves out falls back to its `defaultValue` and every row id changes (§2.9). A manifest is normally
     written without row ids, so every id-less body row **adopts the id of a stored row**: the first
     stored row, in stored order, not claimed by a body row's own id or an earlier adoption, with the
     same `blockType`. Payload then merges it (unsent keys keep their stored value, the id is stable),
     recursively. An id-less row with no partner stays new. Consequences: a key **removed** from a
     manifest row is not a change (write `null` to clear it); deleting a row from an id-less manifest
     shifts later rows onto their predecessors' ids; an export plus an appended id-less row converges
     after one sync.
   * **`alignToStored` — dates.** A body date that names the same instant as a stored
     `Date.toISOString` string (`"2026-01-01"` = UTC midnight, or a date-time with a zone) is replaced
     by the stored form; a date-time without a zone is never equated.
   * **`alignToStored` — numeric ids.** A body string that spells the stored number (`"4"` against
     `4`, element-wise in `["66"]` against `[66]`: relationship/upload ids from a manifest, which `--set`
     would have coerced) is replaced by the stored number, so the item converges.
   * `opts`: `--ignore`, `--full`, the entity's top-level `json` fields as `LeafPaths`.
   * Warnings for create/update items only (advice about a write that will not happen is noise), lifted
     to the envelope as `"[<key>] <message>"`: §9.7 block warnings (computed on what the write sends,
     so an update that leaves `layout` alone has none), `field_not_on_document`, `data_redacted`,
     `relation_id_not_numeric`; envelope-level `redacted_not_sent`, `redacted_removed` and
     `ref_target_unchecked` (item keys in `paths`); for update items **`orphan_risk`**
     (§10.2.1.4) from the state already read (0 requests; reported by `plan` and `sync --dry-run` only —
     a real sync's writes run their own checks); for update items over a pending draft `saved_as_draft`
     / `publishes_pending_draft` (§9.15.4).
7. Items that failed any stage are skipped later; items referencing them are `not_planned` with a
   `reason`. If anything failed, the plan is an **error envelope** whose `data` still carries the plan —
   nothing is written.

Cost: one request per collection item and per global, plus one per distinct server-resolved reference,
plus one per update item whose newest version is a draft (its stored `_status`, §9.15.4).
Plans are sequential (a 500-item manifest costs ~500 reads). Match lookups exclude trashed documents.

#### 9.15.6 `pay plan <DIR|FILE|->...` (L0)

`op_result` `{summary: {items, create, update, unchanged}, items: [{key, source, collection|global,
match, action, id, status, current_status, stored_status?, write_mode, depends_on, fields, diff?}]}` in **execution
order**. `write_mode` is absent for `unchanged`; `diff` only for existing documents (`--brief` drops
`changes`, keeps `summary`); `current_status` is the newest version's `_status`; `stored_status` (only
when it was read, §9.15.4) is the stored row's — `published` there with `current_status: "draft"` is a
live document with a pending draft. `--output table` →
`ACTION KEY TARGET MODE FIELDS`. Verified read-only on stage: 41 pages as one `pay find … --depth 0
--draft` envelope → 41 × unchanged (41 requests); page 47's export with one nested title edited →
exactly that change, `fields: [layout]`, mode `save`.

#### 9.15.7 `pay sync <DIR|FILE|->...` (L2)

1. Plan exactly as §9.15.5 (any problem → the plan error, nothing written).
2. `--dry-run` → the plan plus `dry_run: true` and an aggregated `would_backup`; 0 writes, no audit.
3. Every item unchanged → success, **no confirmation needed**, nothing written.
4. Otherwise **one confirmation** for the whole sync (L2: exit 11 in a non-TTY without `--yes`),
   "sync will create N and update M document(s) and K global(s) …". The per-item writes then run
   without asking again.
5. Items are written in plan order through `runCreateOne` / `runUpdateOne` / `runGlobalUpdateOne` with
   command `sync`: each update item is backed up (`…--sync.json`), audited (`command: sync`,
   `action: create|update`), echo-checked; warnings are lifted as `[key] …` (a write's warnings that
   repeat the plan's are not added twice). An update item also gets `hook_rewrote_unsent` (§9.15.2).
   An item with dependencies is re-resolved with the real ids just before its write and re-decided.
   A failed item's `validation_failed` hint is rewritten in manifest terms: set `"status": "draft"` on
   the item (sync has no `--draft`), `pay describe <slug> --required-only`.
6. A failed item does not stop the others; items that reference it are `not_attempted` with a
   `reason`. `--fail-fast` makes every later item `not_attempted`.
7. Result `op_result` `{summary(+created, updated, failed, not_attempted), items (result:
   created|updated|unchanged|failed|not_attempted), succeeded, failed, not_attempted}`, `changed`,
   `next: {reason: verify_write, cmd: "pay plan <args> --brief"}`. Failures:
   * ≥ 1 write committed → **`partial_failure` (exit 7)**, hint "… Re-running `pay sync <args> --yes`
     is SAFE: a sync re-plans against the server, skips every item that is already in place and writes
     only what is still missing or different", `next.reason: retry_failed_subset`. **Unlike §12.5's bulk
     update, re-running a sync is idempotent.**
   * nothing committed → the first failed item's own error (e.g. `validation_failed`, exit 5), message
     "sync wrote nothing" — unless a failure is `server_error`/`timeout`, whose write may have committed:
     then "no write of this sync was confirmed" and a hint to run `pay plan <args> --brief` first.
   * nothing committed and nothing failed (interrupted) → `timeout` (exit 6).

Verified on localhost (6-item manifest: category parent/child by `@key`, posts by equals-reference, a
page with a polymorphic reference, the `header` global): plan → 5 create + 1 update in dependency
order, 6 requests; `sync --yes` → 13 requests, 1 backup; plan again → 6 × unchanged; a one-label edit
→ exactly that change, the write sends only `hero`.

### 9.16 Frontend URLs and draft preview — `pay url`

The migration agent **published pages to stage on every iteration only to screenshot them**, because a
draft could not be viewed. The website template already ships the answer
(`src/app/(frontend)/next/preview/route.ts`: checks `previewSecret`, authenticates the caller with
`payload.auth({headers})`, `draftMode().enable()`, `redirect(path)`); what was missing is a client that
performs that GET with the profile's credential **without** following the redirect and keeps the
`Set-Cookie`.

```
pay url <collection> <id> | <collection> --slug SLUG [--draft] [--locale CODE]
        [--preview [--cookie-file FILE]]
```

**L0** (the handshake changes no content). `op_result`:

```json
{"url":"https://stage.example.com/skills/data-ai-engineering/dashboards",
 "path":"/skills/data-ai-engineering/dashboards","path_source":"breadcrumbs",
 "site_url":"https://stage.example.com","site_url_source":"derived:base_url",
 "collection":"pages","id":47,"slug":"dashboards",
 "status":"published","has_unpublished_changes":false,"draft":false,
 "preview":{"url":"…","cookie_file":"…/state/preview/<profile>.cookies.json",
   "cookies":[{"name":"__prerender_bypass","domain":"localhost","path":"/","expires":null,
               "http_only":true,"secure":false,"same_site":"Lax"}],
   "handshake":{"url":"…/next/preview?path=%2Fx&previewSecret=<redacted>","status":307,
                "location":"/x","credential_sent":true},
   "key_source":"env:PAY_PREVIEW_SECRET","key_fingerprint":"f6ea7f7a3a58a0d6",
   "load":{"agent_browser":"agent-browser cookies set --curl FILE --domain HOST && agent-browser open URL",
           "curl":"curl -sS -H \"Cookie: …\" URL"}}}
```

**9.16.1 Status.** For a drafts collection: GET `?draft=true` first; if the newest version is
`published` it IS the stored row (1 request, `has_unpublished_changes: false`); otherwise a bare GET
supplies the stored state (`status` = its `_status`, `has_unpublished_changes: true`). No drafts: 1
request, `status: null`. `--draft`/`--preview` resolve the path from the newest version (`draft: true`)
and report `published_path` when it differs.

**9.16.2 Path ladder** (`resolveFrontendPath`, pure): the last `breadcrumbs[].url` starting with `/`
(nested-docs plugin) → the profile's `url_paths[collection]` template → `"/" + PathEscape(slug)` →
otherwise `feature_unavailable` (exit 10) with a `pay config set profiles.<p>.url_paths.<c> '/<c>/{slug}'`
hint. A resulting `/home` becomes `/` (`path_source: home`). Template placeholders: any document field
or dotted scalar path (`{slug}`, `{category.slug}`, `{id}`) and `{locale}`; path-escaped before the
first `?`, query-escaped after it; a placeholder without a value is `invalid_option` (no URL with a
hole). Warnings: `url_path_guessed` (`/{slug}` on a collection other than `pages`), `url_not_live` (no
published version and not `--draft`/`--preview`; hint `pay url <c> <id> --preview`).

**9.16.3 `--preview`.** Resolve the secret first (§5.5; `preview_secret_missing`, exit 9, before any
request when `{secret}` is needed); expand `preview_path` (`{path}`, `{secret}`, `{collection}`,
`{id}`, `{slug}`); `SiteGet` (§6.3); map 401/403 → **`preview_denied`** (exit 8; the message says
whether the credential was withheld cross-origin), 404 → **`preview_failed`** (exit 10, "no preview
route"), no usable cookie → `preview_failed`. A cross-origin handshake that still succeeded warns
`preview_credential_withheld`. Cookies deleted by the response are dropped. The cookie file is written
through `fsatomic` at 0600 (dirs 0700): a JSON array in the Playwright/agent-browser shape
`{name, value, domain, path, expires, httpOnly, secure, sameSite}` (host-only cookies get the site host
as `domain`, `expires` is unix seconds or `-1`, `sameSite` defaults to `Lax`); default
`<state_dir>/preview/<profile>.cookies.json`. **Cookie values never appear in the envelope.** The
emitted `agent-browser` hint carries `--domain` because agent-browser's JSON import fails without it
(`CDP error (Network.setCookies): Invalid cookie fields`).

Verified on localhost: never-published page → 404 without a cookie; `pay url pages 46 --preview` →
`307`, one `__prerender_bypass` cookie written 0600; `curl` with it → 200 with the draft's title; the
secret appeared in no output, log, audit record or file under `PAY_HOME`. Stage (read-only):
`pay url pages 47` → the breadcrumbs URL, 1 request; page 1 (`home`) → `/`. `--output id` prints the
document id; the URL is `--path .url --output id`.

### 9.17 Addressing one document by slug

`(*Deps).ResolveDocBySlug(ctx, t, slug, SlugLookup{Field, Draft, Trash, Locale})` in
`internal/cli/docresolve.go` is the one implementation (`get --slug`, `url --slug`, `outline --slug`),
built on the same bounded `lookupDocs` as §9.15: one request
`GET /{c}?where[slug][equals]=S&limit=2&depth=0&select[slug]=true` (+ `draft=true`, `trash=true`,
`locale` as asked). 0 → `doc_not_found` (exit 4; without `--draft` on a drafts collection the hint says
a slug changed in a draft is found only with `--draft`); > 1 → **`slug_ambiguous`** (exit 5,
`did_you_mean` = the ids, the message carries the total); 1 → `DocRef{ID, IDString, Slug}`. A slug field
the shard does not know is `unknown_field` before any request. `pay get <c> --slug S` = resolve + the
normal GET (2 requests); exactly one of `<id>` / `--slug` (`invalid_args`).

---

## 10. Output

### 10.1 Success envelope

One object on **stdout**, keys in this order.

```json
{
  "ok": true,
  "v": 1,
  "command": "find",
  "data_kind": "doc_list",
  "target": {"kind": "collection", "slug": "pages", "singular": "Page", "id_type": "number"},
  "data": [{"id": 11, "title": "Home", "slug": "home", "_status": "published"}],
  "page": {
    "limit": 3, "page": 1, "total_pages": 4, "total_docs": 11, "returned": 3,
    "has_next_page": true, "has_prev_page": false, "next_page": 2, "prev_page": null,
    "truncated": true
  },
  "next": {
    "reason": "more_pages",
    "cmd": "pay find pages --where '_status = published' --limit 3 --page 2 --profile dev",
    "args": {"page": 2},
    "alternatives": [
      {"why": "stream every page without looping", "cmd": "pay find pages --where '_status = published' --all --output jsonl"},
      {"why": "just the total, 1 cheap request",   "cmd": "pay count pages --where '_status = published'"}
    ]
  },
  "meta": {
    "request_id": "01K5Q7TZ4V3B8C",
    "cli_version": "0.1.0",
    "profile": "dev",
    "base_url": "http://localhost:3900",
    "api_path": "/api",
    "auth_mode": "api-key",
    "payload_version": "3.86.0",
    "payload_version_source": "project-package-json",
    "locale": {"requested": null, "fallback": null},
    "date_field": null, "since": null, "until": null,
    "duration_ms": 41,
    "http_requests": 1,
    "retries": 0,
    "bytes": 612,
    "cache": {"discovery": "hit", "age_s": 412, "ttl_s": 600, "fingerprint": "0df77681", "revalidated": false},
    "discovery_revision": "2026-09-16T17:00:00Z",
    "dry_run": false
  },
  "warnings": []
}
```

Rules:

* `ok` is the **one** field to branch on. Never require presence-checking `error`.
* `v` is the envelope schema version (integer).
* `data_kind` ∈ `doc` | `doc_list` | `count` | `global` | `version` | `version_list` | `bulk_result` |
  `capabilities` | `schema` | `command_spec` | `op_result` | `raw` | `error`. `data`'s shape is implied
  by `data_kind` and never has to be guessed.
* `data` is the natural shape: a bare array for `doc_list`, a bare object for `doc`, an integer for
  `count`. Payload's `{docs:…}` wrapper is unwrapped into `data` + `page`; a global's
  `{result, message}` is normalised to `data` so globals and collections look identical to a caller.
* `page` is present iff `data_kind == "doc_list"`. `truncated` is one boolean requiring no arithmetic:
  `true` means more matching documents exist than this envelope holds — another page on a paginated
  read (it equals `has_next_page`), or `--all` stopped at `--max`. Following `next.cmd` until it is
  `false` collects the whole set.
* `edits` is present **only** on §9.11's edit pipeline — the `pay blocks` verbs and `pay apply`. It is
  `{fields, ops}`: `fields` are the document paths the pipeline changed, in first-touched order and
  deduplicated, and `ops` is every transform applied, oldest first. `pay apply` builds its PATCH body
  from `fields` alone, which is what keeps a one-block reorder from also rewriting `_status` and
  every expanded relationship. A command that talks to Payload never sets it.
* `next` is present iff there is a useful follow-up. `next.cmd` is a **literally runnable string with
  the current flags and profile already baked in** — the agent copies it rather than reconstructing it.
  `next.reason` ∈ `more_pages` | `retry_failed_subset` | `needs_auth` | `verify_write` |
  `discover_first` | `refresh_discovery`. `discover_first` appears **only** alongside exit 10
  `discovery_failed`; a cold cache runs discovery inline instead (§8.5).
* `warnings` is always an array of `{code, message, paths?, hint}`; warnings never change `ok` or the
  exit code.
* `meta.cache.discovery` ∈ `hit` | `stale-served` | `revalidated` | `miss` | `bypassed` | `disabled`.
* `meta.payload_version` is **`null`** when it could not be determined (§7.11); it is never a
  fabricated constant, and `meta.payload_version_source` always says which it is.
* `meta.locale` is `{requested, fallback}` with the **resolved** values PayCLI actually sent, so an
  agent can see that `fallback: "none"` (the default on a localised project) is why a field came back
  null. Both are `null` on a non-localised project.
* `meta.date_field` / `meta.since` / `meta.until` are non-null exactly when `--since`/`--until` were
  used, and carry the **resolved** field name and absolute bounds (§9.4).
* `meta.auth_mode` ∈ `api-key` | `jwt` | `anonymous`.
* **The agent-ergonomics release adds no `data_kind`** (a new one is a breaking change): every new
  command reports `op_result` (`diff`, `outline`, `plan`, `sync`, `url`, `lexical *`, the block catalog,
  `backups list|show|prune`, every dry run), `doc` (`upsert`) or `doc`/`global` (`backups restore`), and
  the several-ids publish reports `bulk_result`. New information travels in `meta` keys that are
  **absent unless the command produced them** (all `omitempty`):

| key | set by | shape |
|---|---|---|
| `meta.backups` | any write that took a §12.8 backup — on success **and** on a failure after the backup | `["/abs/…/pages/42/20260928T134135.519Z--update.json", …]` |
| `meta.restored_from` | `pay backups restore` | the backup file written back |
| `meta.richtext` | a read with `--richtext md\|text` (§9.12.8.2) | `{format: "md"\|"text", rendered, paths[≤100], paths_truncated?}` — its presence is what the write paths refuse |
| `meta.reset_nested` | a `--reset-nested` write (§10.2.1.6) | `{mode, phases, writes, cleared_block_types, carriers, at_risk, empty_rows_before, history_versions, steps, verified, remaining_empty_rows?, failed_phase?, unresolved?}` |
| `meta.upsert` | `pay upsert` (§9.15.2) | `{action: created\|updated\|unchanged (dry run: create\|update\|unchanged), matched_by: {field: value}, id, forced?}` |

  Every catalog verb (§7.10c) carries its cost report in `data.census`, and every dry run its preview
  in `data.diff` / `data.would_backup` / `data.reset_nested` / `data.steps` (§12.2).

### 10.2 Write envelope and the echo-diff check

Writes add a `changed` block and compare what was sent against what Payload echoed back.

```json
{
  "ok": true, "v": 1, "command": "create", "data_kind": "doc",
  "target": {"kind": "collection", "slug": "pages", "id": 16},
  "data": {"id": 16, "title": "t3", "slug": "s3", "_status": "draft", "layout": []},
  "changed": {"created": 1, "updated": 0, "deleted": 0, "trashed": 0, "ids": [16]},
  "warnings": [
    {"code": "input_silently_dropped",
     "message": "1 value you sent is absent from the document Payload returned. Payload discards unknown fields and unknown block types without an error.",
     "paths": ["layout[0]"],
     "sent": {"layout[0].blockType": "nope"},
     "hint": "pay describe pages --field layout prints this project's allowed blockType values, or states in plain words that they are not determinable from the API and where to read them from instead."},
    {"code": "created_as_draft",
     "message": "pages has drafts enabled and the new document is _status=draft, so it is not publicly visible. --draft skipped required-field validation; publishing re-runs exactly that validation.",
     "still_missing": ["layout"],
     "hint": "Still missing: layout (blocks, >=1 item; blockType values UNKNOWN for this project - read them from payload.config.ts / src/blocks/*/config.ts). `pay publish pages 16` will fail until layout is set."}
  ],
  "next": {"reason": "verify_write", "cmd": "pay get pages 16 --depth 0"}
}
```

**Echo-diff algorithm.** After a write, walk the request body's leaf paths and compare each against
the response `doc`. The residue splits into **two classes, which are not the same warning**:

| Residue | Warning | Severity |
|---|---|---|
| the leaf path is **absent** from the echoed doc | `input_silently_dropped` | the real Payload footgun — unknown fields and unknown `blockType` are discarded with a 201 |
| the leaf path is **present with a different value** | `value_normalized_by_server` | informational; names `sent` vs `returned` |

Skipped in both classes: `id`, `createdAt`, `updatedAt`, `_status`, array item `id`s, `hasMany`
reordering, every path listed in the profile's `echo_check_ignore`, and **every field whose manifest
`localized` is `null`** (§7.9d — a `locale=all` write echoes `{code: value}` maps and would otherwise
flag every localized field). `--no-echo-check` skips the whole check.

The original algorithm ended with "…and any field the schema marks as hook-mutated", which was
unimplementable: §7.8's field entries define no such marker, and `beforeChange`/`beforeValidate` hooks
are not introspectable over REST or GraphQL. Its absence was not benign — the official Payload website
template (the shape this CLI meets most often) runs a `formatSlug` hook on `slug` and normalises
`email`, so `pay create pages --set slug='My Page'` returns `slug: "my-page"` and the old single-class
algorithm would flag it `input_silently_dropped`. A false positive on the one warning an agent is told
to act on trains the agent to ignore it. Splitting the classes fixes that by construction: `slug` is
*present with a different value*, which is `value_normalized_by_server`, not a dropped input.

`fields[].hook_mutated` exists in the manifest but is **observation-only**: `"unknown"` until a write
in this scope proves a path differed, then `"observed"` (recorded in the scope cache, never inferred
from the schema). `HOOK_MUTATION_UNKNOWN` (§7.6) declares the gap, and `echo_check_ignore` is the
documented manual answer.

This check exists because Payload returns 201 `"Page successfully created."` while having thrown away
an entire `layout` array for one wrong `blockType`. No other Payload client catches this, and an agent
will believe the success message.

**Three refinements of this release.**

* **`stored_as_string`.** Where the body sent a non-empty object or array and the echo holds a string
  that is JSON text (starts with `{`/`[`), Payload stored the value in a text column as JSON (§2.9:
  an editor state sent to `meta.description`). Those paths are reported once as `stored_as_string`
  (hint: `pay describe <c>` and `--set KEY=…` to write the text back), and their leaves are **not**
  reported as `input_silently_dropped`, whose "Payload discards unknown fields" names the wrong cause.
  A string echo that is not JSON text (a populated relationship echoing as its id) is not this case.
  Every caller of the echo-diff raises it (create, update, apply, globals update, upsert, sync).
* **`{}` rows never pair with sent rows.** The leaf echo-diff runs on the echo **without** its `{}`
  rows (`orphans.WithoutEmptyRows`), so an orphan (§10.2.1) is never matched with the body's row at the
  same index and misreported as `input_silently_dropped`; §10.2.1.3 reports it as `orphan_rows`.
* **A global update answers `{"message", "result": {…}}`** on Payload 3.87 (§2.9); `payload.writeDoc`
  unwraps `result`. Before, the wrapper itself was diffed as the document and every field sent to
  `pay globals update` was reported `input_silently_dropped`.

`--select` narrows the echo; when it hides paths the body sent, the check says so
(`echo_check_narrowed`) rather than reporting them dropped.

#### 10.2.1 Orphan rows — nested block restructures on Postgres

**The failure.** On a Postgres project, changing the TYPE of a nested block (a `bento` inside a `group`
becomes a `grid`) leaves the old nested rows (the bento's `bentoItem`s) in the database. They read back
at their old path: as `{}` rows when the field now at that path does not accept their type, as real
rows when it does, invisibly when no field has that path. Top-level swaps in `layout` are clean. The
only fix before this section was deleting and re-creating the document, which changes its id and drops
its versions.

**10.2.1.1 The adapter's write path** (`@payloadcms/drizzle/dist` 3.87.1 unless marked `payload/`):

* **Storage.** One table per block TYPE per collection (`pages_blocks_bento_item`), whatever depth the
  rows sit at. A block row's `_parentID` is the **document id** (`upsertRow/index.js` l.257), `_path`
  records where it sits (`layout.0.blocks.0.blocks`), `_order` its position. An ARRAY row's `_parentID`
  is the row that holds it, with `ON DELETE CASCADE`.
* **Which tables an update clears** (`upsertRow/index.js`, `operation === 'update'`):

  | child table | cleared by an update | rule |
  |---|---|---|
  | `_locales` | always, for the parent | l.270-285 |
  | `_rels`, `_texts`, `_numbers` | rows at the paths being written | `deleteExistingRowsByPath` l.289-306, 463-511 |
  | **block tables** | every table in `blocksToDelete`, **all paths** | l.516-525 `DELETE WHERE _parentID = <document id>` |
  | top-level array tables (and arrays in top-level groups/tabs) | always | l.572-581 |
  | nested array tables | only by CASCADE from a deleted parent row | schema FK |

* **`blocksToDelete`** is filled by `transform/write/traverseFields.js` l.130-134 when the traversal of
  the data being written REACHES a blocks field — every block table that field accepts, before the value
  is looked at. Every top-level field is always reached; a blocks field inside a block (or array row) is
  reached only through a row of that parent type. **Rule:** an update clears block type T's table iff the
  new document still reaches a blocks field that accepts T — a top-level one always, a nested one only
  if the new document contains at least one row of the field's parent type. Rows of every other type
  survive.
* **Read** (`utilities/createBlocksMap.js`, `transform/read/traverseFields.js`): rows of all block
  tables are grouped by `_path` and sorted by `_order` (stale rows interleave with new ones); a row whose
  type the field at that path does not know is pushed as `{}`.
* **Which write touches which row** (`payload/…/utilities/update.js`, `payload/…/versions/saveVersion.js`):

  | write | stored (main) row | version tables |
  |---|---|---|
  | create | insert | new version (insert) |
  | update `?draft=true` | untouched (l.253) | new version (insert only) |
  | update `?draft=true&autosave=true` onto an autosave | untouched | latest version updated IN PLACE (the rule applies to the version tables) |
  | non-draft update, `publish`, `unpublish`, `versions restore`, a non-draft `backups restore` | **updateOne — the rule applies** | new version built from the stored row READ BACK, in which `{}` rows have no blockType and are dropped |
  | non-draft update with `?unpublishAllLocales=true` (no per-locale status) | updateOne — the rule applies, **validation skipped** (l.154-157) | latest version updated IN PLACE |

  Consequence: on a versioned collection the write's ECHO is the new version and is clean, the draft read
  is clean, and only the stored row — `pay get` without `--draft`, what the site renders — has the `{}`
  rows. A plain draft save can neither create nor remove orphans; the write that creates them after a
  draft restructure is the **publish**.

**10.2.1.2 Verified live** on the klixpert stage, on the one approved scratch page (`paywt-orphan-test`,
id 71, draft only, never published, permanently deleted afterwards — `find --draft` → 0, `get` →
`doc_not_found`, `versions list --id 71` → 0): `layout: [group{blocks:[bento{blocks:[bentoItem×3]}]}]`
restructured by a plain update to `[group{blocks:[grid{blocks:[text A, text B]}]}]` → echo clean,
`pay get --draft` clean, `pay get` → `layout[0].blocks[0].blocks = [{}, text A, {}, text B, {}]` — exactly
the rule (`group` still reached, so `bento`'s table was cleared; `bento.blocks` not, so `bentoItem`'s was
not). A first remedy attempt sent as a normal validated write was rejected (`bento.blocks` requires rows)
— hence the validation-free phases. The shipped remedy then: `verified: true`, same id, no `{}` in the
stored or the draft read, every version in the history with exactly one layout row. On localhost
`?unpublishAllLocales=true` accepted a required-field-empty body and kept `_status` and the version count
(§2.9); the template has no nested blocks, so the orphan itself was reproduced on stage only.

**10.2.1.3 Detection after a write — `orphan_rows`.** After `update <id>`, `apply`, `globals update`,
`publish`/`unpublish <id>`, `backups restore` and `versions restore`, PayCLI compares every row list of
the request body with the same list in (1) the echo and (2) for a write that overwrote the stored row of
a versioned entity, **the stored row read back once** (`GET …?depth=0`, no draft; the write's locale and
trash) — when the echo or the body has a row holding a list, or when `--select` projected the echo so it
cannot tell (`pay publish <c> <id> --select title` sends only `{_status}` — the typical orphaning write).
One extra request; `--no-echo-check` skips it. Symptoms (`orphans.EchoCheck`): `empty_row`, `extra_row`,
`no_block_type`, `type_sequence`; a list returned SHORTER than sent is `input_silently_dropped`, not an
orphan. The warning:

```json
{"code":"orphan_rows",
 "message":"the stored row (read back without draft) holds rows this write did not send (3 empty_row): orphan rows that Payload's Postgres adapter left in a nested block table — when this write or an EARLIER one changed or removed the blocks around them …",
 "paths":["layout[0].blocks[0].blocks[0]","layout[0].blocks[0].blocks[2]","layout[0].blocks[0].blocks[4]"],
 "hint":"pay update pages 71 --reset-nested --dry-run   # removes them and keeps the document id; …"}
```

The message does not blame the current write: rows left by REMOVING a nested container sit at a path no
field reads and surface only when a later write puts a field there again. A read-back that fails is
`orphan_check_incomplete` (never a silent pass). Not checked: `create` (inserts only), the bulk `--where`
forms and several-ids publish (one read per document would be needed — a documented gap).
`get`/`globals get` warn **`empty_rows`** when the returned document has `{}` rows (paths; which row was
read; hint = the repair dry run).

**10.2.1.4 Prediction — `orphan_risk`.** `orphans.Analyze(stored, next, history, accepts)` applies the
rule of 10.2.1.1 to `stored` (the row the write overwrites) and `next` (`docdiff.ApplyPatch(newest,
body)`). Which types a list accepts is learnt from every row seen in that list definition (container =
parent row kind + field path: `root>layout`, `block:group>blocks`) in stored, next and history, widened
by the **cached** census (never built). Unknown acceptance counts as NOT cleared — the safe direction.
A block type with rows at depth ≥ 1 in `stored` that `next` does not clear is a risk; the cause names
the outermost changed ancestor (`layout[0].blocks[0]: bento → grid`, or `… removed`). Raised on every
`--dry-run` that reads the pre-image (§12.2; for a plain draft save the message says the save is safe
and the PUBLISH will leave the rows, hint `pay publish <c> <id> --reset-nested --dry-run`), on a real
write when the §12.8 backup captured the pre-image ("the pre-write backup predicted …"; costs nothing),
and by `pay plan`/`sync --dry-run` (§9.15.5). Without a dry run or a backup a removing write's orphans go
undetected until a later write surfaces them (documented gap; no extra pre-read on real writes).

**10.2.1.5 The remedy — `--reset-nested`.**

*Carriers.* For each block type to clear, PayCLI takes a real occurrence of it (stored row first, then
history) and copies its ancestor rows into a chain appended to the top-level list: every id removed,
every nested blocks list emptied except the one link that leads down, arrays without block rows kept.
The adapter walks the chain, so it clears every table the chain's lists accept and inserts nothing below
it. Carrier rows whose own type the final write would not clear are peeled by further phases; chains
that are strict prefixes of another are dropped; at most **6** phases (`reset_nested_incomplete`
beyond). klixpert's case needs one phase: `group{blocks:[bento{blocks:[]}]}`.

*Validation.* Emptied carriers are not valid content, so carrier phases and the restatement are sent
with **`?unpublishAllLocales=true`**, which on a project WITHOUT per-locale status skips validation on a
non-draft write and changes nothing else except that the newest version is updated in place. A
per-locale `_status` is refused (`feature_unavailable`) — there the switch unpublishes every locale — and
because a single-locale read cannot see it, the remedy reads `?locale=all&select[_status]=true&depth=0`
once and refuses when `_status` is an object (skipped when discovery knows localization is off).

*Sequence* (every request audited as its own step, 10.2.1.7):

1. **the requested write**, exactly as without the flag (validated; a rejection writes nothing).
   Skipped in repair mode.
2. **carrier phases**: every top-level list that receives carriers, restated as stored by 1, plus that
   phase's carriers; `depth=0`, no draft, `unpublishAllLocales=true`.
3. **restatement**: the same lists without carriers, with the write's own params plus
   `unpublishAllLocales=true`. It removes the carriers and leaves the newest version exactly as 1 made
   it — no carrier ever reaches the version history. Its echo is the command's `data`.

*Failures.* A repair's phase 1 rejected with a non-retriable code → error, nothing written. A transient
failure of any phase may have been applied, so the restatement is still sent. The restatement is
**retried** after 1 s and 3 s on a retriable code; when it still fails, carriers are stored and a re-run
of the repair cannot see them, so the restatement body is saved to
`<state_dir>/reset-nested/<profile>/<collection>-<id>|<global>-<UTC time>.json` (0600) and the error (the
upstream code) names the lists and the recovery command
`pay update <c> <id> --data-file <file> [--locale L] [--trash] --yes` (`pay globals update <slug> …`),
plus `pay backups restore <file>` only when this invocation wrote a backup.

*Modes.* **restructure** (a body): the sequence above, `writes` = phases + 2. **repair** (no body:
`pay update <c> <id> --reset-nested`, `pay globals update <slug> --reset-nested`): no step 1; the
restatement is the newest version's lists; `writes` = phases + 1; `{}` rows in the stored row are traced
to a type through the document's last 50 versions, untraceable ones are `reset_nested_unresolved` and
never guessed. A repair of a published document with a **pending draft** is refused (`invalid_args`: it
would publish the draft; hint `pay publish <c> <id> --reset-nested --dry-run`) — where "pending draft"
compares the stored row **without its `{}` rows** with the newest version, because the `{}` rows are only
ever in the stored row. **not needed**: nothing at risk → one ordinary request +
`reset_nested_not_needed`; a plain draft save (`--draft`, no `--autosave`, body not publishing) likewise,
with the hint to publish with the flag. `--autosave` → `invalid_args`. Afterwards the stored row is
ALWAYS read back (10.2.1.3) and `meta.reset_nested.verified` says whether it is clean.

**10.2.1.6 Surface.**

| command | `--reset-nested` |
|---|---|
| `pay update <c> <id>` | restructure; with no body: repair. Bulk `--where` → `invalid_args` |
| `pay apply` (collection or global) | restructure (the body is the piped edits) |
| `pay globals update <slug>` | restructure; with no body: repair |
| `pay publish <c> <id>`, `pay unpublish <c> <id>` | restructure with body `{_status}`. Several ids / `--where` → `invalid_args` |

`meta.reset_nested` (and `data.reset_nested` on a dry run, with `phase_bodies`):

```json
{"mode":"restructure","phases":1,"writes":3,"cleared_block_types":["bentoItem"],
 "carriers":[{"phase":1,"list":"layout","chain":["group","bento"],"clears":"bentoItem"}],
 "at_risk":[{"block_type":"bentoItem","paths":["layout[0].blocks[0].blocks[0]","…"],"causes":["layout[0].blocks[0]: bento → grid"]}],
 "empty_rows_before":[],"history_versions":0,
 "steps":["requested write (validated)","carrier phase 1/1 (unpublishAllLocales=true: no validation)","restate the lists without carriers (unpublishAllLocales=true)"],
 "verified":true}
```

`mode` ∈ `restructure | repair | not_needed`. A repair dry run with nothing to do has `would_affect: 0`,
a null request body and `reset_nested_not_needed`.

**10.2.1.7 Audit and backups.** Each request is its own writeOp: pre/post records with `Event.step`
(`reset-nested requested write`, `reset-nested phase N/M`, `reset-nested restate`). The §12.8 backup is
taken once, by the first request (idempotent per invocation), and every record names the same file.
Confirmation (§12.1) runs once, before the reads.

**10.2.1.8 Package API** (`internal/orphans`, pure; imports only `internal/lexical`):

```go
func Rows(doc map[string]any) []Row              // every row of every row list; kinds block:<slug> / array:<container>
func EmptyRows(doc map[string]any) []string      // paths of {} rows
func WithoutEmptyRows(doc map[string]any) map[string]any
func EchoCheck(sent, stored map[string]any) []Finding   // empty_row | extra_row | no_block_type | type_sequence
func Analyze(stored, next map[string]any, history []map[string]any, accepts Accepts) *Analysis
type Accepts map[Container][]string              // extra acceptance knowledge (the census)
type Analysis struct{ Risks []Risk; Empty, Unresolved []string; Phases [][]Carrier; Incomplete bool }
func (a *Analysis) NeedsReset() bool; func (a *Analysis) ClearedTypes() []string
const MaxPhases = 6
```

#### 10.2.2 A body that publishes — `body_publishes`

`isSavingDraft = draftArg && drafts && data._status !== 'published'` (§2.9): a body with
`_status: "published"` — every export of a published document carries it — publishes even with
`?draft=true`. For `update` (by id and bulk), `apply` and `globals update`, when the entity has drafts
(or unknown):

| flags | body `_status: "published"` | behaviour |
|---|---|---|
| `--draft` | yes | **`--draft` wins**: `_status` is REMOVED from the body, the write saves a draft, `body_publishes` says so |
| none | yes | sent as is — it publishes — `body_publishes` warns loudly ("this write PUBLISHES the document") |
| `--publish` / `--unpublish` | any | the flag sets `_status`; no warning |
| `--draft --publish` | — | `invalid_args` before anything is read or sent (`--publish --unpublish` likewise) |
| `--draft --unpublish` | — | `invalid_args` before anything is read or sent: with `?draft=true` Payload only saves a draft version and the live document stays published (verified on localhost: the echo said `draft`, the page stayed live). One rule, `checkStatusFlags`, for update, apply, globals update and versions restore |

`upsert` and `sync` never take `_status` from the body at all (§9.15.4); `pay diff`'s body form warns.
Verified on localhost (published page 54): an edited export written with `--draft` → `body_publishes`,
the published read kept the old title, the draft read had the new one.

#### 10.2.3 A non-draft write never unpublishes by accident

**The failure** (§2.9, localhost pages 63/67/68): an update-by-id merges into the NEWEST version — the
pending draft — and without `?draft` stores the merge as the main row; the global update does the same.
So on a published page with a pending draft, `pay update pages 63 --set slug=x` wrote the whole draft
AND its `_status: "draft"` into the live row: the page went offline, and neither the dry run (its diff
was against the newest version: one change) nor the write warned. A body carrying `_status: "draft"`
(every `pay get --draft` export of a draft) did the same to a published page without a pending draft.

**The rule** (`internal/cli/pubguard.go`) — for `update <id>`, `apply` (collection or global),
`globals update`, and the update half of `upsert`/`sync`, on an entity with drafts (or unknown), for a
**non-draft write with a body and without `--unpublish`**:

| stored row | newest version | body `_status` | outcome |
|---|---|---|---|
| published | draft (pending draft) | none | **refused** `invalid_args` (exit 5): it would publish the draft AND unpublish |
| published | any | `"draft"` | **refused** `invalid_args`: it would unpublish |
| published | draft | `"published"` (`--publish`, or a publishing body) | sent; warning **`publishes_pending_draft`** (visitors get the draft's changes too; hint `pay diff <c> <id> --draft-vs-published`) |
| anything else | | | sent as before |

`upsert` / `sync` never reach the first row: their status `keep` resolves to a `?draft=true` write when
the newest version is a draft (§9.15.4), which saves into the pending draft and warns `saved_as_draft`.
For them only the `publishes_pending_draft` row applies (status `published`), and `pay plan` /
`sync --dry-run` report it from their own stored-status read.

The refusal names the three explicit choices — `--draft` (edit the draft; the live document is
untouched), `--publish` (publish the draft with this change), `--unpublish` (take it offline
deliberately; no check). `pay globals update` has `--publish`/`--unpublish` for this, and
`pay apply --publish/--unpublish` apply to a global too. PayCLI does **not** silently re-route such a
write into the draft (§1 conflict 44).

**Cost.** The dry run decides from the pre-image it already reads (a preview never promises what the
write refuses). The real write reads the state first — `GET …?depth=0&select[_status]=true` without
draft and, only when that is `published`, with `draft=true` — **before** the backup and the PATCH, so a
refused write takes no backup. `--draft`, `--unpublish`, an entity without drafts and an empty body skip
it. A failed read: `doc_not_found` → the write proceeds (and fails the same way); a publishing body →
proceeds; otherwise the error (hint: state the outcome with a flag). Payload's draft read returns the
stored row itself when no newer draft exists, so "pending draft" is exactly: stored `published`, draft
read `draft` (stage: 22 published pages, `pay diff --draft-vs-published` identical on all).

**Preview.** A non-draft write whose stored row differs from the newest version adds
**`data.diff.published`** — `{base: "published", …}` = stored row (without `{}` rows) →
`ApplyPatch(newest, body)`: what the live page will actually change by (§12.2).

**`pay versions restore`.** payload@3.87.1 `collections/operations/restoreVersion.js` l.206-213 writes
the version into the stored row unless `?draft`, keeping the VERSION's `_status`. Restoring a draft
version without `--draft` therefore unpublishes a published document (verified on localhost:3900:
`pay get --published-only` answered `not_published` afterwards). For a collection with drafts and
without `--draft`, PayCLI reads the version record (shared with the backup's parent lookup) and, when
the version is a draft, the stored row's `_status` (`select=_status`); a published stored row is
**refused** `invalid_args` (dry run too) naming `--draft` (restore as a new draft on top; the live
document stays) and **`--unpublish`** (new flag: restore into the live row and take it offline
deliberately). `--unpublish` on a published version is refused (the restore stores it published);
`--draft --unpublish` is refused (§10.2.2); on a global `--unpublish` is refused — globals are not
checked: `globals/operations/restoreVersion.js` always rewrites the stored global. A version read that
fails with `doc_not_found` lets the restore answer; another failure refuses with the flag hint.

**Bulk.** Payload's bulk update merges into the STORED rows unless `?draft=true` (§2.9). So
`pay update --where` without `--draft` never takes a pending draft's `_status` (no refusal), but it
**supersedes** pending drafts: its dry-run `data.diff` is based on the stored row (`base: "published"`)
and warns **`pending_draft_superseded`** for sampled documents with one. **`pay publish --where` sends
`?draft=true`**, as the admin panel's bulk Publish does, so the newest versions are published (before,
it re-published the stored content and superseded the drafts). `pay unpublish --where` keeps the plain
PATCH. Several explicit ids are one PATCH per id (update-by-id, newest version).

### 10.3 Formats

| Format | Audience | Shape |
|---|---|---|
| `json` | **agents, DEFAULT** | one pretty envelope object |
| `jsonl` | agents streaming | one **bare document** per line on stdout, no envelope; the envelope (`page`/`next`/`meta`/`warnings`) is one JSON line on **stderr**. `pay find pages --all --output jsonl > out.jsonl 2> summary.json` yields a clean data file and a clean summary |
| `id` | shell pipelines | one id per line, nothing else |
| `raw` | escape hatch | Payload's body, no envelope — **after redaction** unless `--no-redact`; see below |
| `csv` | humans | RFC 4180; nested objects flattened to dotted columns, arrays JSON-encoded in-cell; default columns = discovered top-level **scalar** fields only, never richText blobs |
| `table` | humans | aligned, truncated to terminal width; never recommended to agents |

**`--output raw` is redacted by default.** The original definition ("Payload's body verbatim, no
envelope") bypassed §5.3's redaction by construction, and the leak is concrete:
`pay get users 66 --output raw` and `pay raw GET users/me --output raw` print the API key in
plaintext (verified §2.6). So: `raw` routes through `internal/redact` like every other format; when
redaction actually altered the bytes, PayCLI emits the redacted body on stdout **and** a one-line
stderr notice naming `--no-redact`; `--no-redact` produces the true bytes. The escape hatch survives —
it is one flag away, and the flag is named in the notice — but it is no longer the *default* way to
exfiltrate a credential into an agent transcript.

**`--path EXPR` (replaces `--jq`).** A closed, three-form grammar, evaluated in-process with no
dependency:

| Form | Meaning |
|---|---|
| `.a.b` | field access, arbitrarily nested |
| `.a[0]` | index into an array (negative indices are an error) |
| `.a[]` | iterate a whole array |

**Nothing else** — no filters, no pipes, no functions, no `select`, no `map`. The result **replaces
`data`**; `ok`, `v`, `command`, `data_kind`, `target`, `page`, `next`, `meta` and `warnings` are
preserved unchanged, so an agent can always still branch on `.ok`. Use `--output raw` for a bare
scalar with no envelope. A malformed or unsupported expression fails with `invalid_path_expr`
(exit 5, §11.4) whose message lists the three supported forms and ends with the literal sentence
*"This is not jq; pipe the envelope to jq for full expressions."* The flag was renamed because
advertising jq's name for a hand-rolled evaluator guarantees an agent sends
`.[] | select(._status=="published") | .title` and gets an unspecified failure.

**No YAML.** It adds a dependency, no agent parses it better than JSON, and its implicit typing is a
real correctness hazard for CMS content (`no` → false, unquoted `on`/`off`, the Norway problem on a
`country: NO` field).

Format/command compatibility is **explicit**: `--output csv` on `pay explain` fails with exit 5
`format_unsupported` naming the supported formats, rather than silently falling back to JSON.

### 10.4 Machine-readable help

`pay <any command> --help --output json` emits `data_kind: "command_spec"`:

```json
{"ok": true, "v": 1, "data_kind": "command_spec",
 "data": {"path": ["find"], "short": "List documents in a collection (GET /api/{collection}).",
   "args": [{"name": "collection", "required": true, "type": "enum",
             "values_from": "discovery.collections", "example": "pages"}],
   "flags": [{"name": "where", "type": "string", "repeatable": true, "default": null,
              "grammar": "PATH OP VALUE",
              "operators": ["=","!=","~","gt","gte","lt","lte","in","nin","exists","near"]},
             {"name": "depth", "type": "int", "min": 0, "max": 10, "default": 0}],
   "output": {"data_kind": "doc_list", "paginated": true},
   "exit_codes": [0,2,3,4,5,6,8,9,10],
   "examples": [{"why": "published pages, newest first",
                 "cmd": "pay find pages --where '_status = published' --sort -publishedAt --select id,title,slug"}]}}
```

### 10.5 Help-text doctrine

Every command's help contains these sections, in order, with **real discovered values** substituted
wherever one exists:

1. One-line `Short` naming the HTTP operation (`List documents in a collection (GET /api/{collection}).`)
2. `SYNOPSIS` with explicit metavariables
3. `DISCOVERED IN THIS PROJECT` — live, cache-backed, grouped, with doc counts, plus the escape hatch
   when truncated. Never a placeholder.
4. `WHERE SYNTAX` — the full operator table taught inline
5. `SORT` with the silent-ignore warning
6. `FLAGS` — type, default, enumerated valid values; flags that are no-ops for this project say so
7. `OUTPUT` — the `data_kind` and a one-line JSON skeleton
8. `EXIT CODES` used by this command
9. `EXAMPLES` — 4–8, copy-pasteable, using this project's real slugs and fields
10. `COMMON MISTAKES` — 3–6 numbered anti-patterns each with its correction
11. `SEE ALSO`

Hard rules: every "you cannot X" is followed by "do Y instead"; help renders from cache only and
**never blocks on the network** (a cold cache prints `run 'pay discover' to populate` for sections 3
and 5); help is deterministic for a given `discovery_revision`; and every help block containing
discovered values carries a header line stamping `profile`, `base_url` and `discovery_revision`, so an
agent that cached help from project A cannot mistake it for project B.

---

## 11. Errors

### 11.1 Error envelope

Same envelope, `ok:false`, `data_kind:"error"`, on **stdout**. A one-line human summary
(`pay: validation_failed (exit 5): 3 fields are invalid on "pages" — hint: …`) goes to stderr.
`PAY_ERRORS_TO=stderr` / `--errors-to stderr` moves the envelope to stderr for people who script
around the Unix convention.

```json
{
  "ok": false, "v": 1, "command": "create", "data_kind": "error",
  "target": {"kind": "collection", "slug": "pages"},
  "error": {
    "code": "validation_failed",
    "exit": 5,
    "message": "3 fields are invalid on collection \"pages\".",
    "hint": "Fix every path in error.fields and retry. `pay describe pages --required-only` lists all required fields with their types. To store an incomplete document instead, add --draft: Payload skips required-field validation for drafts, and `pay publish` will re-run it later.",
    "retriable": false,
    "confidence": "certain",
    "fields": [
      {"path": "title",  "label": "Title",            "message": "This field is required.",           "sent": true},
      {"path": "layout", "label": "Content > Layout", "message": "This field requires at least 1 Row.", "sent": false},
      {"path": "slug",   "label": "Slug",             "message": "This field is required.",           "sent": false}
    ],
    "did_you_mean": [],
    "docs": "pay explain --section exit_codes",
    "http": {"status": 400, "method": "POST", "url": "http://localhost:3900/api/pages",
             "payload_error_name": "ValidationError", "attempts": 1},
    "raw": {"errors": [{"name": "ValidationError", "data": {"collection": "pages", "errors": [
              {"label": "Title", "message": "This field is required.", "path": "title"}]},
              "message": "The following fields are invalid: Title, Content > Layout, Slug"}]}
  },
  "meta": {"request_id": "01K5Q7V0…", "duration_ms": 22, "http_requests": 1, "retries": 0, "profile": "dev"}
}
```

**`error.raw` is byte-faithful *after redaction*.** The original "always included and byte-faithful"
rule could not coexist with §5.3's "`internal/redact` scrubs `apiKey` … from **all** output", and the
leak was real: §12.5's `partial_failure` envelope embeds `raw: {docs:[…]}`, so
`pay update users --where … --set …` against the auth collection emitted every touched user document
verbatim, `apiKey` in plaintext (verified §2.6) — into stdout, into an agent transcript, and into the
§17.3 golden files committed to a public repo.

So: `raw` is redacted like everything else; when redaction **modified** it, the envelope carries
`warnings[] {"code":"raw_redacted","paths":["docs[0].apiKey","docs[1].apiKey"]}` so the agent knows the
bytes differ from the wire; `--no-redact` produces the true bytes. `--no-raw` drops it entirely for
token economy.

**`error.confidence`** ∈ `certain` | `probable`. `certain` means the classification rests on
`errors[].name`, JSON structure, or an HTTP status — signals that cannot be changed by the project's
i18n config. `probable` means it rests on a structural heuristic (§11.5's `file_missing` row is the
only one) and the confirming English string was absent. It is always present and never absent.

**`error.fields[].sent`** is `true` iff the path is a leaf of the request body PayCLI actually sent.
Entries are sorted **sent-first**. PayCLI already computes the request body's leaf-path set for the
§10.2 echo-diff, so this costs nothing.

It exists because Payload validates the **whole document** on `PATCH` and therefore reports fields the
caller never touched (verified: `PATCH /api/posts/1 {"heroImage":4}` → 400 listing `title`, `slug` and
`content`). The original hint — "Fix every path in error.fields and retry" — instructed an agent to
invent a title, a slug and a body for someone else's post. The hint is therefore **branched**:

* **any entry has `sent: true`** → the existing text.
* **every entry has `sent: false`** →
  `"The stored document is already invalid on 3 field(s) you did not send - Payload validates the whole document on update, so your change was rejected by pre-existing state, not by your input. Retry with --draft to store the change without validation, or repair the listed fields in the same request."`
* **every entry has `sent: false` on a create** (`POST` to the collection root, not `/globals/<slug>`
  or `/<slug>/<id>/duplicate`) → there is no stored document; the fields are missing from the caller's
  body: `"The new document is missing 1 required field(s) you did not send: a create must supply every required field. Add them to the body (--set, --set-json, --data), or pass --draft on a collection with drafts to store an incomplete draft without validation. `pay describe pages` lists the required fields."`

`error.fields` is `[]` rather than absent when there are no field-level details.

### 11.2 Normalisation (six server shapes → one)

Derived in this priority order:

**Classification never reads a human-readable message.** `followingFieldsInvalid`,
`noFilesUploaded`, `notAllowedToPerformAction` and `deletedCountSuccessfully` all go through `req.t`
(verified in `payload/dist`), so their text changes with the project's `i18n.supportedLanguages`,
`i18n.fallbackLanguage`, a `payload-lng` cookie, or a proxy-injected `Accept-Language`. Only
`errors[].name`, the JSON structure, and the HTTP status are trusted. The two Payload strings that are
**hardcoded English** — `Route not found "…"` and `Cannot <METHOD> …` — are the sole exceptions, and
they are named explicitly wherever they are used.

1. body has `docs` **and** a non-empty `errors` → `partial_failure`
2. `errors[0].name == "ValidationError"` and `errors[0].data.errors` is an array → `validation_failed`,
   `fields` = that array verbatim (the **`name`** is the signal; the `message` text is carried into
   `error.raw` and the per-field `message`, never matched against)
3. `errors[0].name == "QueryError"` and `errors[0].data` is an **array** → `query_path_invalid`,
   `fields` = `data.map(path → {path, message:"This path cannot be queried."})`
   (note the type difference from case 2: `data` is an array here and an object there — code that does
   `err.data.errors` on a QueryError crashes)
4. `errors[*]` carry a `field` key (the Mongoose branch in `formatErrors.js`) → `validation_failed`,
   map `field` → `path`
5. body has `message` and **no** `errors` key → `route_not_found` (404) / `operation_unsupported` (501)
6. otherwise → map by HTTP status, `fields: []`

The normaliser must never assume `errors[i]` is an object with a `message`: `formatErrors` contains
`if (Array.isArray(incoming.message)) return { errors: incoming.message }`, which lets a project author
return anything, and `afterError` hooks can rewrite both the body and the status. Unparseable bodies
degrade to `code: "unknown"` with `raw` preserved — never a panic.

Response bodies are environment-dependent: when the server runs with `config.debug: true`,
`isErrorPublic` returns true for everything and the body gains a `stack` field. Prefer the server's
message when it is present and non-generic; demote PayCLI's own diagnosis to a secondary hint.

**Regression guard.** `testdata/fixtures/error_validation_de.json` holds a real German
`ValidationError` body (`"Die folgenden Felder sind ungültig: …"`, per-field `message`s translated).
A unit test asserts the normaliser still produces `code: "validation_failed"`, exit 5, and the correct
per-field `path` list from it. That test is what keeps the i18n independence from regressing the first
time someone "simplifies" a matcher into a string compare.

### 11.3 Diagnosing opaque 500s

Payload masks non-public errors as `{"errors":[{"message":"Something went wrong."}]}` unless
`config.debug` is on. Three client mistakes produce this, all verified; PayCLI **pre-empts the first
two client-side** and diagnoses the third:

| Condition | `likely_causes[0]` | PayCLI action |
|---|---|---|
| path is `/{coll}/{id}` and `id` does not parse as the collection's `id_type` | `id_type_mismatch` (high) | rejected before the call: `invalid_id`, exit 5 |
| path ends `/versions` and the collection has no versions | `feature_unavailable` (high) | rejected before the call: exit 10 |
| body contains a relationship whose target id does not exist | `relationship_target_missing` (medium) | diagnosed after the fact, fix: `pay get <relTo> <id>` |

### 11.4 Error codes and exit codes

| Exit | Class | Auto-retry? | Codes |
|---|---|---|---|
| 0 | success | — | — |
| 1 | generic / internal | no | `internal`, `unknown`, `cache_corrupt`, `update_verification_failed`, `audit_write_failed`, `backup_failed` |
| 2 | auth — *your credentials* | no | `auth_missing`, `auth_invalid`, `auth_required`, `auth_locked`, `auth_unverified_email`, `auth_insecure_permissions`, `auth_helper_failed` |
| 3 | throttled / temporarily unavailable | **yes** | `rate_limited` (429), `doc_locked` (423), `server_busy` (503 with `Retry-After`) |
| 4 | not found | no | `doc_not_found`, `route_not_found`, `version_not_found`, `selector_no_match`, `not_published`, `ref_unresolved` |
| 5 | validation / bad input | no | `validation_failed`, `query_path_invalid`, `invalid_args`, `invalid_where_syntax`, `invalid_sort_field`, `unknown_field`, `invalid_option`, `invalid_id`, `bad_request_body`, `where_required`, `file_missing`, `not_upload_collection`, `unsupported_operator`, `bulk_limit_exceeded`, `request_too_large`, `format_unsupported`, `invalid_path_expr`, `selector_ambiguous`, `field_ambiguous`, `no_input`, `no_edits`, `not_rich_text`, `richtext_rendered`, `backup_target_mismatch`, `slug_ambiguous`, `match_ambiguous`, `ref_ambiguous`, `ref_cycle`, `manifest_invalid` |
| 6 | network / server | **yes**, except 500 | `network_unreachable`, `dns_failure`, `tls_error`, `timeout`, `server_error` (500, **not retried**), `server_unavailable` (502/504), `non_json_response` |
| 7 | **partial failure** | **never** | `partial_failure` |
| 8 | **access denied** — identity valid, permission not | no | `access_denied`, `admin_access_denied`, `preview_denied` |
| 9 | **config / profile** | no | `config_missing`, `config_secret_in_plaintext`, `profile_unknown`, `base_url_invalid`, `endpoint_not_payload`, `auth_collection_unknown`, `preview_secret_missing` |
| 10 | **capability / discovery** | no | `collection_unknown`, `global_unknown`, `feature_unavailable`, `operation_unsupported` (501), `discovery_failed`, `graphql_disabled`, `schema_stale`, `block_type_unknown`, `preview_failed` |
| 11 | **confirmation required** | no | `confirmation_required` |

Never uses 125–128 or 130 (shell/signal territory). A unit test asserts the `Code → exit` map is
**total** — gsc-cli's `default:` branch silently returns 1 for any new code.

**The codes of the agent-ergonomics release**, with where each is raised:

| code | exit | when |
|---|---|---|
| `backup_failed` | 1 | a §12.8 backup could not be taken (read or file write); the write is **aborted**, nothing sent, no audit pre-record (hint names `--no-backup`) |
| `not_published` | 4 | `pay get --published-only` on a document with no published state (§9.6.2) |
| `ref_unresolved` | 4 | a `{"$ref": …}` matched no document, or `@key` names no plan item (§9.15.1) |
| `not_rich_text` | 5 | a `pay lexical` input holds no editor state where one was expected; `did_you_mean` lists the rich-text paths (§9.12.7) |
| `richtext_rendered` | 5 | a write path was handed a document rendered by `--richtext md\|text`, a bare body whose declared richText field holds a string, or a body with a string where the stored document holds an editor state (§9.12.8.3) |
| `backup_target_mismatch` | 5 | `pay backups restore` of a backup taken from another profile or server; hint `--profile <theirs>` / `--ignore-target` (§12.8.8) |
| `slug_ambiguous` | 5 | `--slug` matched more than one document; `did_you_mean` = the ids (§9.17) |
| `match_ambiguous` | 5 | `pay upsert --match` / a manifest item matched more than one document (§9.15.2) |
| `ref_ambiguous` | 5 | a `{"$ref": …}` matched more than one document (§9.15.1) |
| `ref_cycle` | 5 | plan items reference each other in a cycle that contains a document still to be created (§9.15.5) |
| `manifest_invalid` | 5 | a manifest cannot be planned; `error.failures[]` names every item (§9.15.3) |
| `preview_denied` | 8 | the site's preview route answered 401/403 (§9.16.3) |
| `preview_secret_missing` | 9 | `pay url --preview` with `{secret}` in `preview_path` and nothing resolved; raised before any request (§5.5) |
| `preview_failed` | 10 | 404 at `preview_path`, or no usable cookie issued (§9.16.3) |

Existing codes gained new triggers: `invalid_args` (the §10.2.3 refusal, `--draft --publish`,
`--draft --unpublish`, a draft version restored over a published document without `--draft`/`--unpublish`,
`--reset-nested` on a bulk/several-ids write or with `--autosave`, a repair over a pending draft, a
`--set-md` key through an array or onto a non-rich-text field, an unresolvable nested `--field`);
`feature_unavailable` (`--reset-nested` on a per-locale `_status`, `pay url` with no path source,
`--publish`/`--draft` on upsert/sync of an entity without drafts); `validation_failed` (a
`pay blocks validate` report with an error-severity issue; a publish whose every id is blocked by the
pre-check, §9.10.4); `selector_no_match`/`selector_ambiguous`/`unknown_field` (nested `--field` paths,
§9.11.4). `payload.BulkFailure` gained `code`: a per-document write (`--per-doc`, several ids) reports
each failure's own code (a missing id is `doc_not_found`), not a blanket `validation_failed`.

**Warning codes of the release** (warnings never change `ok` or the exit code):

| area | codes |
|---|---|
| block census & writes (§7.10c, §9.7) | `blocks_unchecked`, `census_entity_failed`, `census_truncated`, `census_unavailable`, `census_scope_wide`, `example_moved`, `block_type_shapes_differ`, and the §7.10c.4 issue codes `empty_row`, `missing_block_type`, `invalid_row`, `unknown_block_type`, `block_type_not_observed_here`, `unknown_field`, `value_not_in_enum`, `required_field_missing`, `likely_required` |
| edit pipeline (§9.11) | `stale_index`, `block_type_omitted`, `apply_all_fields` (plus the existing `field_absent`, `blocks_field_inferred`, `local_validation_skipped`, `populated_relationship`, `envelope_unwrapped`) |
| rich text (§9.12) | `frontmatter_stripped`, `lexical_<code>` (`lexical_placeholder`, `lexical_normalized`, `lexical_lossy_alignment`, `lexical_lossy_style`, `lexical_lossy_format`, `lexical_lossy_state_keys`, `lexical_html_as_text`, `lexical_comment_dropped`, `lexical_image_as_text`, `lexical_code_block_flattened`, `lexical_table_as_text`, `lexical_list_item_paragraphs_joined`, `lexical_list_item_block_flattened`, `lexical_list_became_checklist`, `lexical_nesting_capped`, `lexical_link_reference_undefined`, `lexical_link_url_invalid`, `lexical_link_url_relative`), `set_md_not_richtext`, `set_md_field_unchecked`, `set_md_literal_newline`, `set_md_empty`, `richtext_as_string`, `array_as_object` (pay diff) |
| diffs & dry runs (§9.14, §12.2) | `never_published`, `field_not_on_document`, `body_id_mismatch`, `data_redacted`, `diff_unavailable` |
| echo & publication (§10.2) | `stored_as_string`, `orphan_rows`, `orphan_risk`, `empty_rows`, `orphan_check_incomplete`, `reset_nested_not_needed`, `reset_nested_incomplete`, `reset_nested_unresolved`, `body_publishes`, `publishes_pending_draft`, `pending_draft_superseded` |
| idempotent writes (§9.15) | `body_status_ignored`, `match_value_normalized`, `redacted_not_sent`, `redacted_removed`, `hook_rewrote_unsent`, `ref_target_unchecked`, `relation_id_not_numeric`, `saved_as_draft` |
| URLs & preview (§9.16, §5.5) | `url_path_guessed`, `url_not_live`, `preview_credential_withheld`, `preview_secret_shadowed` |
| publish (§9.10.4) | `publish_blocked` (a dry run's per-id pre-check block) |
| backups (§12.8) | `backup_skipped`, `backup_redacted`, `backups_disabled`, `backup_fields_not_restored`, `restored_as_draft`, `published_state_not_restored`, `backup_recreated_new_id`, `backup_recreated_in_trash`, `recreate_not_needed`, `restore_two_steps`, `config_path_anchored` |

### 11.5 HTTP → code mapping

| HTTP | Body signal | Code | Exit |
|---|---|---|---|
| 400 | `name:"ValidationError"` | `validation_failed` | 5 |
| 400 | `name:"QueryError"` | `query_path_invalid` | 5 |
| 400 | `"Invalid JSON"` | `bad_request_body` | 5 |
| 400 | `"Missing 'where' query…"` | `where_required` | 5 |
| 400 | `POST`/`PATCH` to a collection whose `flags.upload` is `true`, **and** no `errors[0].name` | `file_missing` | 5 |
| 400 | has `docs` **and** `errors[]` | `partial_failure` | **7** |
| 401 | any | `auth_invalid` | 2 |
| 403 | `auth_mode` ∈ {`api-key`,`jwt`} **and** `identity.verified == true` | `access_denied` | **8** |
| 403 | `auth_mode == "anonymous"`, or identity is null / unverified | `auth_required` | 2 |
| 404 | `{"errors":[{"message":"Not Found"}]}` | `doc_not_found` | 4 |
| 404 | `{"message":"Route not found …"}` | `route_not_found` / `collection_unknown` | 4 / 10 |
| 404 + `text/html` | `id="__next_error__"` in the body | `non_json_response` | 6 |
| 413 / 414 / 431 | — | `request_too_large` | 5 (after the override fallback) |
| 423 | `Locked` | `doc_locked` | 3 |
| 429 | proxy/WAF only | `rate_limited` | 3 |
| 500 | `"Something went wrong."` | `server_error` + `likely_causes[]` | 6 |
| 501 | `{"message":"Cannot <M> …"}` | `operation_unsupported` | 10 |
| 502 / 503 / 504 | — | `server_unavailable` | 6 |

The 401-vs-403 split is the one the server cannot make for us: a 403 body is **byte-identical** whether
the caller is unauthenticated or merely unpermitted (verified), and its message is translated on top of
that. PayCLI disambiguates using its own `auth_mode` and verified identity, which is why exit 2 and
exit 8 are separate codes with completely different remediations ("fix your key" vs "ask for
permission"). In `anonymous` mode a 403 is **always** exit 2 `auth_required` with the
`pay auth login …` hint, because "you are not logged in" is the true and actionable answer.

The `file_missing` row no longer matches `"No files were uploaded."`, because that string is
`t('error:noFilesUploaded')` (verified, `errors/MissingFile.js`) and vanishes on a German project,
silently degrading the upload diagnosis to a generic 400. The structural signal (upload collection +
400 + no `ValidationError` name) is adapter- and language-independent; the English string is kept only
as a **confirming** signal that raises `error.confidence` from `probable` to `certain`, never as the
discriminator.

Non-JSON handling: before parsing any body, check `Content-Type`. An `/api/*` response that is not
`application/json` never reaches `json.Unmarshal` (which would produce a useless
`invalid character '<'`); it becomes `non_json_response` with a ≤200-char `body_excerpt` and, when the
body contains `id="__next_error__"`, an explicit note that Next.js's error boundary was hit rather
than Payload.

---

## 12. Write safety

### 12.1 Risk levels

| Level | Commands | Behaviour |
|---|---|---|
| **L0** read | `find` `get` `count` `versions list/get/diff` `describe` `collections` `explain` `access` `can` `whoami` `download` `discover` `doctor` `cache *` `globals list/get`, the `pay blocks` edit verbs (`ls mv rm add cp set` — they rewrite stdin, §9.11.5) and catalog verbs (`types schema example new validate learn`, §7.10c), `lexical from-md/to-md/text`, `diff`, `outline`, `plan`, `url` (its preview handshake changes no content) | never prompts |
| **L1** scoped write | `create`, `update <id>`, `apply` (collection), `upload`, `duplicate`, `publish <id>` (also several explicit ids, each its own write), `restore`, `upsert`, `delete <id>` to the trash | no prompt by default (single doc, recoverable via versions where enabled); `defaults.confirm_writes = true` opts in |
| **L2** scoped destructive | `delete <id> --permanent` (or on a collection without trash), `versions restore`, `globals update` (and `apply` to a global), `unpublish <id>` (also several explicit ids), `backups restore`, `backups prune` (it deletes the safety net), `sync` (one confirmation for the whole manifest) | prompts on a TTY; **requires `--yes` in non-TTY** or exits 11 |
| **L3** bulk | `update --where`, `delete --where`, `publish/unpublish --where` | **always** requires `--yes` in non-TTY, always prints the resolved match count before acting, and is capped by `--max-docs` (default `defaults.max_bulk` = 100) unless `--all` is passed. `--limit` on these verbs is `invalid_args` (exit 5) |

**Explicit ids are not bulk.** `pay publish pages 38 39 40` / `pay unpublish pages 38 39 40 --yes`
(`safety.SelectorIDs`) take the per-document level — publish L1, unpublish L2 — **not** L3: the blast
radius is exactly the typed ids, never what a filter happens to match. The confirmation line reads
"unpublish will affect 2 explicitly named document(s) in "pages", one write each". `--where`/filters,
`--max-docs`, `--all` and `--limit` together with ids are `invalid_args`. `pay backups list|show` and
`pay auth preview-secret` touch only local files and never prompt.

### 12.2 `--dry-run`

Available on every write. Performs the read half only and emits, with **exit 0**:

```json
{"ok": true, "v": 1, "data_kind": "op_result",
 "data": {"would_affect": 12, "sample_ids": [224,223,222,221,220], "truncated": true,
          "request": {"method": "DELETE", "url": "/api/crm-contacts?where=%7B…%7D", "body": null}},
 "meta": {"dry_run": true, "http_requests": 2}}
```

`request.url` passes through `redact.URL` (§5.3) like every other URL that crosses a boundary;
otherwise it is printed verbatim so an agent can hand it to `pay raw` or curl.

The op_result may carry four more keys (`safety.DryRunResult`, all omitempty — no new `data_kind`):

* **`would_backup`** `{dir, documents, paths[≤5], truncated}` — the §12.8 files the real run would write,
  stamped with the dry run's clock. A dry run reads and writes nothing for the backup.
* **`diff`** — what the write changes in each document it targets, read now and computed by
  `internal/docdiff` (the "manual `jq -S` diff to prove a dry run touched one block" of the agent session):

  | command | reads | `data.diff` |
  |---|---|---|
  | `update <id>`, `apply` (collection) | newest version (+ stored row where drafts exist) | `{base:"latest", id, status, updated_at, identical, summary, changes}` = `docdiff.Diff(newest, ApplyPatch(newest, body))` |
  | `update --where` | the first `safety.SampleLimit` (5) resolved ids, one `where id in` read per side | `{sampled, of, truncated, base, documents:[{id, …}]}`; `base: "published"` (stored row → stored + body) without `--draft` — Payload's bulk update merges into the stored row — `"latest"` with it |
  | `publish/unpublish <id>` | stored row + newest version | `{base:"published", …}` = stored row vs `ApplyPatch(newest, {_status})` — what visitors will see change |
  | `publish/unpublish --where`, several ids | sampled, as `update --where` | `{sampled, …, base:"published", documents}`; a sampled id the read did not return is listed in `missing` with `diff_unavailable` (and, for several ids, left out of `would_backup`) |
  | `globals update`, `apply` (global) | the global (newest + stored where drafts exist) | `{base:"latest", identical, summary, changes}` |
  | `backups restore` (in place) | newest (+ stored) | `{base:"latest", …}` of the restore body; a **two-step** restore (backup with `published_doc`) adds `published: {base:"published", …}` = stored row → `ApplyPatch(newest, published body)` (step 1, omitted when step 1 is skipped) and `data.diff` becomes newest → `ApplyPatch(step-1 result, draft body)`; warning `restore_two_steps` says what stays live |
  | `versions restore <id>` | the version + the document's newest/stored rows | `{base:"published", …}` = stored row → the version's content (`--draft`: `base:"latest"`) |
  | any non-draft row above whose stored row differs from the newest version | (same reads) | adds `published: {base:"published", …}` = stored row (no `{}` rows) → result (§10.2.3) |

  Cost: 1 GET without drafts, 2 with. The same reads feed `orphan_risk`/`empty_rows` (§10.2.1.4), the
  §10.2.3 publication verdict (a preview refuses what the write would refuse) and the census overlay
  (§9.7). A read that fails is the warning `diff_unavailable` (upstream code; hint the `pay get` that
  shows it) and the preview still succeeds. Secret paths are masked (§9.14.6).
* **`reset_nested`** — the `--reset-nested` plan with `phase_bodies` (§10.2.1.6).
* **`steps`** `[{step, method, url, body, skipped?}]` — every request of a write sent as several (the
  two-step `pay backups restore`, §12.8.8); `request` is then the last one.

### 12.3 Bulk delete — the three-phase algorithm

**Bulk `DELETE` ignores `limit` and deletes every match** (verified: `limit=1` deleted all 4). PayCLI
therefore never passes `--where` straight through to `DELETE`:

1. `GET /{coll}/count?where=…` → N.
2. If `N > --max-docs` (default `defaults.max_bulk` = 100) and `--all` was not passed →
   `bulk_limit_exceeded` (exit 5) with the hint
   `"N=1200 exceeds --max-docs 100; pass --all to delete everything matching, or narrow --where"`.
3. `GET /{coll}?select[id]=true&limit=N&where=…` to resolve exact ids, then
   `DELETE ?where={"id":{"in":[…]}}` in chunks of 100.

**`--limit` and `--max-docs` are different flags with different jobs.** The original spec used
`--limit` for both page size (default 20, §4.2/§9.2) and blast radius (default `max_bulk` = 100,
§12.3 step 2), so for `pay delete pages --where '…'` matching 50 documents one correct implementation
refuses and another deletes all 50. Resolved:

* **`--limit N` is page size, on read commands only.** On `update --where`, `delete --where` and
  `publish/unpublish --where` it is an **error** (exit 5 `invalid_args`, hint
  `"--limit is page size and has no meaning on a bulk write; use --max-docs N to cap the blast radius, or --all"`).
* **`--max-docs N` (default `defaults.max_bulk` = 100, lifted by `--all`) is the sole blast-radius
  cap** and the only thing §12.3 step 2 consults.
* **Neither verb ever sends a server-side `limit` on a bulk write.** PayCLI always resolves ids
  client-side first — for `PATCH` exactly as for `DELETE` — so the blast radius equals what
  `--dry-run` printed, on both verbs, regardless of the verified asymmetry that bulk `PATCH` honours
  `limit` while bulk `DELETE` ignores it entirely (§2.4). Relying on that asymmetry would make the two
  verbs behave differently for the same flags. A golden test asserts the emitted query string for
  `pay update c --where x --max-docs 2` contains **no** `limit=` parameter.

The blast radius is then exactly what `--dry-run` showed, and every failure maps to a specific id.
`--unsafe-passthrough-where` restores the raw semantics for anyone who wants them.

TOCTOU is bounded and documented: documents created between the count and the delete are **not**
included (correct); documents that start matching after resolution are missed (acceptable); documents
deleted in the interim surface as per-id failures and are **not** treated as hard errors.

### 12.4 Soft delete by default

If the manifest says the collection has trash, `pay delete <id>` issues
`PATCH {"deletedAt":"<now ISO>"}` and reports `"trashed": true`. `--permanent` issues the real
`DELETE`, automatically adding `?trash=true` when the document is already trashed (otherwise it 404s —
verified). `pay restore <coll> <id>` issues `PATCH ?trash=true {"deletedAt":null}`.

If the collection has no trash, `pay delete` warns on stderr that the operation is irreversible and
follows the L2 rules.

`deletedAt` is only Payload's **default** trash field name, so it is read from the manifest, never
hardcoded, and the trash capability is re-probed on any `delete` whose cache entry is older than
`access_ttl`.

### 12.5 Partial failure

Bulk verbs return HTTP 400 whenever `errors[]` is non-empty, **even when some documents succeeded and
committed** — the outer transaction commits after the per-document loop. Never trust the status code;
always parse the body.

```json
{
  "ok": false, "v": 1, "command": "update", "data_kind": "bulk_result", "partial": true,
  "target": {"kind": "collection", "slug": "pages"},
  "data": {"succeeded": [{"id": 11, "title": "Bulk Test"}], "failed": [{"id": 10}],
           "not_attempted": []},
  "changed": {"created": 0, "updated": 1, "deleted": 0, "ids": [11]},
  "error": {
    "code": "partial_failure", "exit": 7,
    "message": "Updated 1 of 2 documents in \"pages\"; 1 failed.",
    "hint": "THE 1 SUCCESSFUL WRITE IS ALREADY COMMITTED. Payload bulk operations are NOT transactional over the REST API - do not re-run this command or you will re-apply it. Retry only the failed ids using next.cmd.",
    "retriable": false,
    "failures": [{"id": 10, "code": "validation_failed",
                  "message": "The following field is invalid: Content > Layout",
                  "fields": [{"path": "layout", "label": "Content > Layout",
                              "message": "This field requires at least 1 Row."}]}],
    "http": {"status": 400, "method": "PATCH", "url": "…"},
    "raw": {"docs": ["…"], "errors": [{"id": 10, "isPublic": true, "message": "…"}],
            "message": "Unable to update 1 out of 2 Pages."}
  },
  "next": {"reason": "retry_failed_subset",
           "cmd": "pay update pages --where 'id in 10' --set title='Bulk Test'",
           "args": {"ids": [10]}}
}
```

`failures[].fields` is best-effort: Payload gives only a message string for bulk sub-errors, so PayCLI
re-parses it against the collection's field labels and may leave `fields` empty.
Entries with `isPublic: false` — whose message Payload rewrote to the literal `"Something went wrong."`
— map to `code: "server_error"` with an explicit note that the real message was withheld and that
`debug: true` in `payload.config.ts` would reveal it.

**Exit 7 is documented everywhere as "never auto-retry."** This is the only failure mode where the
default agent heuristic (`if exit != 0: retry`) causes real damage.

`--per-doc` on bulk update loops individual `PATCH /{id}` calls instead of one bulk call. This is the
antidote to a single bad document poisoning the batch and its innocent batch-mates coming back as
opaque `"Something went wrong."` It multiplies request count by N, so it is opt-in.

**Several explicit ids** (`pay publish|unpublish <c> <id> <id>…`, §12.1). Every id is validated
(`id_type`) before anything is sent; exact duplicates are written once; publish pre-validation runs once
for all ids (§9.10.4). Writes are one PATCH per id (`perDocUpdate`), sequential, fail-soft;
`--fail-fast` stops at the first failure and lists the rest in `data.not_attempted`. Result
`bulk_result`; any failure → `partial_failure` (exit 7) with `next.cmd` =
`pay publish <c> <failed…> <not_attempted…>` (plus ` --yes` for unpublish). Failures with code
`doc_not_found` are left out of `next.cmd` and `next.args.ids` — a missing id fails the same way on
every retry — and with nothing left to retry there is no `next`. `error.hint` uses per-document wording
("N of the listed document(s) were written and stay written; each id is its own request …") instead of
the bulk "NOT transactional … do not re-run" text. `--dry-run` previews `PATCH …/{id}` with
`would_affect` = the ids after pre-validation and a `publish_blocked` warning per blocked id (known gap:
without pre-validation a nonexistent id is still counted). Audit: one pre/post pair with all ids.

**`pay sync` inverts the exit-7 advice.** A sync with ≥ 1 committed write and ≥ 1 failure is
`partial_failure` (exit 7) too, but because a sync re-plans against the server and skips every item
already in place, **re-running it is safe** — its hint and `next.cmd` (`pay sync <args> --yes`) say so
(§9.15.7). Exit 7 remains "never auto-retry *blindly*"; for sync the documented recovery is the re-run.

### 12.6 Client-side fan-out

`pay update pages --ids 1,2,3` uses the same envelope: a bounded worker pool, fail-soft per item, a
shared retry budget, and it never aborts the whole run on one item's failure unless `--fail-fast`. On
SIGINT it stops scheduling, lets in-flight items finish, and emits the partial envelope with the
remainder under `data.not_attempted` so the agent can resume.

### 12.7 Audit log

`$CONFIG/audit.log`, JSONL, 0600, 10 MB rotation with 3 generations. Logs **L1–L3 only**, never reads,
**before and after** the call so an interrupted destructive op still leaves a trace.

```go
type Event struct {
    Time         time.Time       `json:"time"`
    Phase        Phase           `json:"phase"`    // pre | post
    Command      string          `json:"command"`  // e.g. update, apply, upsert, sync, "backups restore"
    Profile      string          `json:"profile"`
    BaseURL      string          `json:"base_url"`
    Collection   string          `json:"collection,omitempty"`
    Global       string          `json:"global,omitempty"`
    Action       string          `json:"action"`   // create|update|delete|trash|restore|publish|unpublish|
                                                    // upload|restore_version|raw|restore_backup
    Method       string          `json:"method"`
    Path         string          `json:"path"`
    Where        json.RawMessage `json:"where,omitempty"`
    IDs          []string        `json:"ids,omitempty"`        // capped at 200 + IDsTruncated
    IDsTruncated bool            `json:"ids_truncated,omitempty"`
    Affected     int             `json:"affected"`
    Failed       int             `json:"failed,omitempty"`
    DryRun       bool            `json:"dry_run,omitempty"`
    Status       int             `json:"http_status"`
    OK           bool            `json:"ok"`
    Err          string          `json:"error,omitempty"`
    DurationMS   int64           `json:"duration_ms"`
    RequestID    string          `json:"request_id"`
    Backups          []string    `json:"backups,omitempty"`           // §12.8 files taken before this write, capped at 200
    BackupsTruncated bool        `json:"backups_truncated,omitempty"`
    Step         string          `json:"step,omitempty"`          // one request of a multi-request write
    RestoredFrom string          `json:"restored_from,omitempty"` // the backup `pay backups restore` wrote back
}
```

**Multi-request writes** record one pre/post pair **per request**, told apart by `step`:
`reset-nested requested write` / `reset-nested phase N/M` / `reset-nested restate` (§10.2.1.7) and
`backups restore 1/2: the published state` / `backups restore 2/2: the draft on top` (§12.8.8). `upsert`
and `sync` records carry `command: upsert|sync` and the write's own `action` (`create`/`update`).
`backups` and `restored_from` are local paths, never bodies.

`BaseURL` and `Path` are written only after `redact.URL` (§5.3). Never logs the API key, a JWT, an
`Authorization` header in any form, or full document bodies. `--no-audit` / `PAY_NO_AUDIT=1` disables
it.

**Audit-write failure policy** (read-only `$CONFIG`, disk full, permission change). This is a policy
decision, not an implementation detail, because §12.7's stated purpose is that an interrupted
destructive op still leaves a trace:

* **The pre-write record failing aborts the operation** with exit 1 `audit_write_failed`, naming the
  path and errno, with the hint `pass --no-audit to proceed without a trace`. The user must be able to
  opt out of auditing **deliberately**, never by accident.
* **The post-write record failing emits a warning only** (`{"code":"audit_write_failed"}`) and does
  not change `ok` or the exit code — the mutation already happened and failing the command would
  report a false negative on a committed write.
* `--no-audit` suppresses both, and `pay doctor` reports audit-log writability.
* **Order with backups:** the §12.8 backup runs **first** inside `writeOp.pre`, then the pre-record,
  then the request. A backup that fails aborts before any audit record is written.

### 12.8 Automatic pre-write backups

The migration agent took "manual backups before every risky write" (`pay get … > before.json`, a
hand-written restore via `pay update --data-file`) and cleared orphan rows by permanent delete +
re-create, which changed page ids with no record of what was deleted. Payload's versions do not cover
this: `--permanent` removes the document AND its versions, a global without `versions` has no history,
and a collection without drafts/versions has nothing to restore from. `internal/backup` owns the
on-disk format (no HTTP, never imports `internal/cli`); `internal/cli/backup.go` the hook;
`internal/cli/backups.go` the commands.

#### 12.8.1 Enabling

Backups are **off** until a directory is configured (§1 conflict 47): `--backup-dir DIR` →
`PAY_BACKUP_DIR` → `./pay.toml backup_dir` → `config.toml backup_dir` → off, with the anchoring rules of
§4.5. `--no-backup` / `PAY_NO_BACKUP=1` disables **writing** for one invocation without forgetting the
directory, so `pay backups list|show|restore` still work. `pay config explain` reports `backup_dir`
(absolute, with its source) and `no_backup`; `pay config paths` adds `backup_dir` (`""` when off).

#### 12.8.2 Which writes are backed up

Every write that **changes or removes an existing document**:

| Command | Backed up |
|---|---|
| `update <id>`, `apply` (collection) | the document |
| `update --where` (`--per-doc` too) | every id §12.3 resolved |
| `publish`/`unpublish` `<id>`, several ids, `--where` | every targeted id |
| `delete <id>` (soft, `--permanent`), `delete --where` | every targeted id |
| `delete --where --unsafe-passthrough-where` | every id matching at resolve time (one extra read, only when backups are on) |
| `restore` (un-trash) | the document |
| `versions restore <coll> <versionId>` | the **parent** document (`GET /{coll}/versions/{id}` → `parent`, only when backups are on) |
| `versions restore --global`, `globals update`, `apply` (global) | the global |
| `upload --replace ID` | the document (metadata only — the old file bytes are NOT backed up) |
| `upsert` (update half), every `sync` update item, every `--reset-nested` sequence (once) | the document / global |
| `backups restore` | the document or global it overwrites |

Not backed up: `create`, `duplicate`, `upload` without `--replace` (they change no existing document)
and `pay raw` (its body and target are opaque to PayCLI).

#### 12.8.3 The read

Before the §12.7 pre-audit record and before the request:

* Collections: `GET /{coll}?where={"id":{"in":[…]}}&depth=0&limit=100[&draft=true][&trash=true]
  [&locale=…&fallback-locale=…]` in chunks of 100 ids — one read per 100 documents (verified: a
  3-document bulk update with backups = 4 requests: count, resolve, **one** backup read, PATCH).
  `draft=true` exactly when the collection has drafts (the capture is the newest version);
  `trash=true` exactly when it has trash (it returns trashed **and** live documents, §2.9); the
  `locale`/`fallback-locale` are the **write's own**.
* **The published state under a newer draft.** When a chunk holds documents whose newest version is a
  draft, one more read of the same ids **without** `draft=true` returns their main rows; every main row
  that is `published` is stored as **`published_doc`**. That is exactly the state a non-draft write over
  a published document with a pending draft (publish, `update --publish`, …) overwrites, and no other file
  held it. A published document without a pending draft costs nothing extra.
* Globals: `GET /globals/{slug}?depth=0[&draft=true][&locale=…]`, with the same `published_doc` rule.
* Returned documents are paired with the requested ids by the server's id, numerically for number ids
  (`0108` ↔ `108`). An id the batched read does not return is read once more on its own
  (`GET /{coll}/{id}` with the same params): a document found there is backed up; only a
  `doc_not_found` answer makes it **skipped** with `backup_skipped` (hint: the server answered
  doc_not_found, the write fails for it too); any other failure is `backup_failed`.

#### 12.8.4 The file

```
<dir>/<profile>/<collection>/<id>/<UTC-timestamp>--<command>.json
<dir>/<profile>/_globals/<slug>/<UTC-timestamp>--<command>.json
```

Timestamp `20060102T150405.000Z` (UTC, milliseconds, no `:` so the name is Windows-legal); a command
with spaces uses `-` (`globals-update`, `backups-restore`); a second file for the same document in the
same millisecond gets `.2`, `.3`… — nothing is ever overwritten. Path elements are sanitised
(`[A-Za-z0-9._-]`). Written through `fsatomic`: files **0600**, directories **0700**.

```json
{"pay_backup": 1, "time": "2026-09-28T13:41:35.519655665Z", "profile": "local",
 "base_url": "http://localhost:3900", "collection": "pages", "id": 42, "command": "update",
 "request_id": "01M3M3ZR2E56J8D73VKZXT6M6Q", "draft": true, "trash": false,
 "locale": "de", "fallback_locale": "none", "redacted_paths": ["createdBy.apiKey"], "cli_version": "0.2.0",
 "doc": {"id": 42, "title": "…", "_status": "draft", "layout": […]},
 "published_doc": {"id": 42, "title": "…live…", "_status": "published", "layout": […]}}
```

`pay_backup` is the format marker (a reader refuses any other value); `global` replaces
`collection`+`id`; `base_url` passes `redact.URL`; `request_id` is the read that captured `doc`; numbers
round-trip exactly (`UseNumber`); `published_doc` is present only per §12.8.3.

**Redaction is deliberately narrower than §5.3's output rules**, because every masked value is one a
restore can never bring back (a JWT-shaped `description` masked by the output rules could not be
restored). Unless `--no-redact`, `doc`/`published_doc` are scrubbed **for credentials only**
(`redact.Scrubber.StoredDoc`): `apiKey`, `apiKeyIndex`, `resetPasswordToken`,
`resetPasswordExpiration`, `_verificationToken` at any depth; `hash`, `salt`, `password`, `sessions`
only inside an **auth document** (the top-level document of an auth collection, or a nested object
carrying `email`/`username` — a populated user); and any string containing a resolved literal secret.
§5.3's `(?i)token|secret` key rule and the JWT-shape rule are **not** applied (they match content —
`designTokens`, embed tokens — far more often than credentials). Masked paths go to `redacted_paths`
(a restore never writes them back) and the write warns `backup_redacted` up front.

#### 12.8.5 Failure policy (mirrors §12.7's pre-record)

A backup that cannot be taken — the read fails, or the file cannot be written — **aborts the write**:
exit 1, `backup_failed`, naming the step and the OS error, hint
`… or pass --no-backup to write without a backup, deliberately`. Nothing is sent and **no audit
pre-record** is written. Verified on localhost with `PAY_BACKUP_DIR` pointing at a regular file: exit 1,
the document unchanged. The user opts out deliberately, never by accident.

#### 12.8.6 Reporting

`meta.backups` lists every file this invocation wrote (on success **and** on a failure after the
backup; absent when nothing was backed up); both audit records carry `backups` (§12.7); a dry run
reports `would_backup` (§12.2) and reads and writes nothing for it.

#### 12.8.7 The hook

```go
// internal/cli/backup.go
type BackupTarget struct {
    Collection *collTarget   // exactly one of Collection / Global
    Global     *globalTarget
    IDs        []any         // nil on a writeOp = the ids given to withIDs
    Params     query.Params  // the WRITE's params: Locale, FallbackLocale (and Trash) are honoured
}
type BackupResult struct{ Paths []string; Skipped []any }

func (w *writeOp) withBackup(ctx context.Context, bt BackupTarget) *writeOp       // normal path
func (d *Deps) BackupBeforeWrite(ctx context.Context, command string, bt BackupTarget) (*BackupResult, error)
func (d *Deps) BackupPreview(command string, bt BackupTarget) *backupPreview   // nil when off
func (d *Deps) BackupsEnabled() bool
```

A write through `writeOp` adds `.withBackup(…)`; `pre()` backs up, `emitDryRun` previews. One logical
change made of several requests (upsert's update half, sync items, the `--reset-nested` phases) calls
`BackupBeforeWrite` once before its first request and aborts on error. The hook is **idempotent per
invocation**: a (kind, slug, id, locale) already backed up is not read or written again, so a two-phase
write backs up the PRE-image once and never its own phase 1. Off, or under `--dry-run`, it does nothing.

#### 12.8.8 `pay backups`

`list`/`show`/`prune` need no server. `list` and `show` are `op_result`; `restore` is `doc`/`global`.

* **`pay backups list [-n N] [--since D] [--collection C] [--id ID] [--global G] [--command C]
  [--all-profiles]`** — newest first, current profile unless `--all-profiles`; `-n` default 50, `0` =
  all; read from file names only. `data: {dir, profile, writing, returned, total, backups: [{path,
  profile, collection|global, id, command, time, bytes}]}`; `next.cmd` = `pay backups show <newest>`. No
  directory → `ok:true`, empty list, `backups_disabled` (also named when `--no-backup` is set).
* **`pay backups show <file>`** — the record plus `path`. `<file>` may be absolute, relative to the
  working directory, to the backup directory, or to the profile's directory under it. Missing →
  `invalid_args`; not a `pay_backup: 1` record → `bad_request_body`. `next.cmd` =
  `pay backups restore <file> --dry-run`.
* **`pay backups restore <file> [--draft|--publish] [--recreate] [--ignore-target] [--depth N]
  [--no-echo-check] [--dry-run] [--yes]`** — L2, audit action `restore_backup`.
  1. The backup's `profile` and (redacted) `base_url` must equal the current ones, else
     `backup_target_mismatch` (exit 5; hint `--profile <theirs>` / `--ignore-target`) — before any request.
  2. Body = `doc` minus server-owned `id, createdAt, updatedAt, sizes, thumbnailURL, globalType`, `url` on
     upload collections, join fields, top-level read-only fields (except `_status`, `deletedAt`) and every
     masked value; masked and other read-only fields are listed in `backup_fields_not_restored`.
  3. Live read (main row): exists → `PATCH` (with `trash=true` when the collection has trash), the
     current state backed up first; 404 → `doc_not_found` with hint
     `pay backups restore <file> --recreate --dry-run`.
  4. **Drafts** (`restoreDraftMode`): `--draft` → saved as a draft (with a `published_doc` over a live
     row that is not published: `published_state_not_restored`); `--publish` → the backup's `doc`
     content is published. A backup **with `published_doc`** restores in **two steps**: (1) a plain
     PATCH of `published_doc`'s body with `_status: "published"` — skipped when the live row is already
     published and identical — then (2) `doc` with `draft=true`: the exact pre-write state, live again
     with the old published content and the old draft pending. Each step is its own audited request
     (`step`), one confirmation, one backup of the current state; the dry run adds `data.steps`. A
     failure of step 1 writes nothing; a failure of step 2 says the published state is back and hints
     `--draft --yes`. The dry run's `data.diff.published` shows step 1's change to the live row and
`data.diff` the newest version after both steps; `restore_two_steps` warns that two versions are
written and the document stays published. A backup whose `_status` is `draft` is saved with **`draft=true`** (a draft save
     skips required-field validation as the original did; verified: pages with `layout: []` restore);
     over a live published row it warns `restored_as_draft`. Otherwise a plain PATCH. A restore never
     unpublishes.
  5. `--recreate` when the document is gone: `POST /{coll}` (draft=true for a draft backup; with a
     `published_doc`, POST the published state then PATCH the draft onto the new id). **Payload assigns a
     new id** (§2.9): `backup_recreated_new_id` names old and new id and says relationships to the old id
     were NOT updated; a backup taken in the trash is re-created in the trash
     (`backup_recreated_in_trash`, `pay restore` hint); `--recreate` on an existing document restores in
     place (`recreate_not_needed`); on a global `invalid_args`.
  6. Globals: `POST /globals/{slug}` with the same body and draft rules.
  7. The §10.2 echo check (and §10.2.1.3's orphan check) runs on the result.
  8. Every audit record carries `restored_from`; the envelope `meta.restored_from`.
* **`pay backups prune --older-than D [--collection C] [--global G] [--id ID] [--all-profiles]
  [--dry-run] [--yes]`** — L2 (the backups are the safety net); `--older-than` is required (`30d`, `12h`,
  a date). Removes matching files, then every directory left empty up to (never including) the root.
  `data: {dir, cutoff, dry_run, removed, would_remove, bytes, sample[≤20], truncated}`.

#### 12.8.9 `pay config set backup_dir <relative>`

`pay config set` writes the **user** config, where a relative value would anchor to the config directory
— never what an agent in a project means. A relative value is therefore stored as
`<working directory>/<value>` with `config_path_anchored` (hint: put `backup_dir = "<value>"` in
`./pay.toml` for a directory next to the project); `data.value` is the stored absolute path.

#### 12.8.10 Known gaps

* Localized collections: a backup holds the write's locale only; a `--permanent` delete + `--recreate`
  restores one locale.
* The global two-step restore is exercised by code review and the in-memory store only (no
  drafts-enabled global on the test sites).

---

## 13. Uploads

`pay upload <collection> <file|-|URL>` builds `multipart/form-data` with exactly:

* part **`file`** — the bytes. The name must be literally `file` (verified: `-F upload=@…` → 400
  `"No files were uploaded."`). The part's `Content-Type` is sniffed or taken from `--content-type`.
* part **`_payload`** — a JSON string of all sibling fields, present only if any were supplied. Built
  by the **§9.10.2 merge**, deep-merged with later winning:
  `--alt` → `--data`/`--data-file` → `--set` (typed + coerced per §9.10.1) → `--set-json` →
  `--set-md` (§9.12.8.1; e.g. a media `caption` rich-text field).
  So `--set alt=B --alt A` yields `alt: "B"`. The flags are **combinable**, not alternatives.
  `{"$ref": …}` references in the metadata are resolved first (§9.15.1).

Go: `multipart.Writer`, `CreatePart` with an explicit
`Content-Disposition: form-data; name="file"; filename="…"` plus `Content-Type`, `io.Copy` the file
(never buffer it whole), and `WriteField("_payload", string(jsonBytes))`.

Behaviours surfaced in help and in `warnings[]`:

* **Filename collisions are auto-renamed server-side** (`paycli-probe.png` → `paycli-probe-1.png`).
  PayCLI echoes the returned filename and emits a `filename_changed` warning when it differs, because
  an agent will otherwise assume its own name.
* `--replace <id>` does a `PATCH /{coll}/{id}` with the same multipart body. This also **mints a new
  filename** rather than overwriting in place, so any hardcoded URL to the old file breaks. Stated in
  the flag help. With backups on (§12.8) the document's metadata is backed up first — the old file
  bytes are not.
* A URL argument is downloaded **client-side** and re-sent as multipart. A JSON body with a `url` key
  does **not** work over REST (verified: `"No files were uploaded."`) — that is an admin-client
  feature. `--max-size` (default 100 MB) is enforced, and `--allow-remote` is required for non-HTTPS
  or private-range hosts.
* `-` reads stdin; `--filename` is then required.
* **Preflight:** if the manifest says the target is not an upload collection, fail with exit 5
  `not_upload_collection` rather than letting Payload return an opaque 500.

`pay download` hits `GET /{coll}/file/{filename}` (raw bytes; served unauthenticated in this project;
returns 500, not 404, for a missing file) and resolves `--size thumbnail` via
`sizes.<name>.filename` on the document first.

---

## 14. Skills

The canonical source lives **only** at the repository root, under `skills/pay/`. That directory is
*itself* a Go package — `skills/embed.go`, `package skills`, `//go:embed all:pay` — which is what
makes "root-level" and "exactly one copy" compatible: `go:embed` cannot reach a *parent* directory,
but it reaches a *child* freely, so the package that embeds the tree lives in the tree's own
directory. `internal/skills` imports it (`root "github.com/KLIXPERT-io/pay-cli/skills"`) and owns
installation, manifests and `references/PROJECT.md`; it embeds nothing itself.

The bytes compiled into `pay` are therefore read from the files a reader of the repository sees.
There is no sync step, no generated mirror, and nothing to drift — see §1 conflict 18, whose original
embedded-only decision this supersedes without giving up its guarantee.

```
skills/                           ROOT level (not under internal/)
├── embed.go                      package skills — //go:embed all:pay, exports FS()
├── embed_test.go                 asserts embed root == this directory, and that the
│                                 repository contains exactly one SKILL.md
└── pay/                          <- `--skill pay`
    ├── SKILL.md                  static, hand-written, git-reviewed
    └── references/
        ├── query-syntax.md
        ├── errors.md             the taxonomy, exit-code table, and the six body shapes
        ├── recipes.md            paginate, bulk safely, upload, versions, drafts
        └── gotchas.md
```

The layout is the `skills` convention (github.com/anthropics/skills), the same one
`KLIXPERT-io/gsc-cli` ships as `skills/gsc-cli/SKILL.md`, so there are two install routes and they
read the *same* files:

```sh
pay skills install                                                            # offline, from the binary
npx skills add https://github.com/KLIXPERT-io/pay-cli/skills --skill pay       # from the repository
```

`pay skills install` remains the primary route and is fully offline: it needs no network, no Node
and no `git`, it writes `.pay-skill.json` so reinstalls can tell an upgrade from a user edit, and it
is the only route that can also generate `references/PROJECT.md` for the project at hand.

`SKILL.md` frontmatter:

```yaml
---
name: payload-cli
description: Read and write any Payload CMS 3.x project from the command line with the `pay` CLI - discover collections, query documents, create/update/delete, manage globals, versions and uploads. Use when the user asks to inspect or change Payload CMS content, or mentions payload.config.ts, a Payload collection, or a Payload admin panel.
---
```

Body, in order: (1) **"Run `pay explain` first"** within the first five lines, non-negotiable;
(2) the envelope and `data_kind` table; (3) the exit-code table with **exit 7 in bold**; (4) the
where/sort/select/depth mini-DSL; (5) the verified gotchas; (6) ten recipes; (7) the closing rule —
*branch on `.ok` and the exit code, never on `error.message`*.

Four statements are **mandatory verbatim** in `SKILL.md` and `references/gotchas.md`, because each one
is a place where the obvious agent behaviour produces a confidently wrong answer:

* the draft-read rule in §9.6.2 ("a read without `--draft` does **not** filter out unpublished
  documents … always pass `--published-only`");
* the whole-document-validation rule (§9.10.3) and what `error.fields[].sent` means;
* the localisation rule (§7.9): `--locale de` on an untranslated field returns the **default locale's**
  text unless `fallback-locale=none`, which PayCLI sends by default and reports in `meta.locale`;
* the closing rule — *never branch on `error.message`; it is translated* (§11.2).

`--with-project-context` additionally runs discovery and writes `references/PROJECT.md`:

```
<!-- generated by pay 0.1.0 on 2026-09-16T17:12:00Z from profile "dev"
     base_url=http://localhost:3900 discovery_revision=2026-09-16T17:00:00Z -->
This is a SNAPSHOT and may be stale. `pay explain` is always live - prefer it
when anything here does not match reality. Refresh: `pay skills update --project-only`
```

…then the collection table (slug, ops, features, key fields), the globals, the auth collection slug
and exact header form, and three examples written against real slugs. It degrades gracefully (skips
the file and warns) when the server is unreachable.

**Install mechanics.** Default scope is `project` when a `payload.config.ts`, `.git` or `package.json`
is found walking upward, else `user`; the chosen scope is always printed in the envelope.
Targets are `<scope>/.claude/skills/pay/` and the equivalents for `codex`, `cursor`, `gemini`,
`gemini/antigravity`, `opencode`, `windsurf`, `continue`, `crush`, `kiro`, `qwen`, `qoder`. Default
behaviour installs into every agent whose parent directory **already exists** and never creates one
for an absent agent; skipped agents are reported with reasons. The list is a data table in the binary,
extensible via `--dir PATH` and a `skills.extra_dirs` config key, so new agents do not require a
release.

Files are **copied, not symlinked** (a symlink into a binary-relative path breaks on update).
A `.pay-skill.json` manifest records `{cli_version, installed_at, scope, agents, profile, base_url,
discovery_revision, files: {path: sha256}}`. On reinstall: matching hashes are overwritten silently
and reported `unchanged`/`written`; a file whose hash **differs from the manifest** was edited by the
user and is **skipped with a warning** unless `--force`. Writes are atomic; dirs 0755, files 0644.

`pay skills status` reports every install location, its `cli_version` versus the running binary, hash
drift per file, and whether `PROJECT.md`'s `discovery_revision` is older than the current cache.
`pay doctor` surfaces whether the skill is installed and stale.

**Privacy:** `PROJECT.md` is typically committed, so it never contains credentials, redacts any
userinfo in `base_url`, and `pay skills install` warns when the detected project root's `.gitignore`
does not cover the agent skill directory.

---

## 15. Self-update

Reuses gsc-cli's pipeline (`resolveBinary`/`EvalSymlinks`, `lookupChecksum`, `extractTarGz`/
`extractZip`, `atomicSwap`, flock, state file, 24 h throttle) with six changes:

1. **Fix the race, and specify the apply path exactly.** gsc-cli starts `update.Background()` as a
   goroutine and immediately calls `os.Exit(cmd.Execute(...))`, killing the download for any fast
   command. PayCLI: `pay update-self --check` writes `{"pending_version":"v1.2.3"}` to
   `update-state.json` with a bounded 2 s HTTP call. The **apply** then has exactly two shapes, and
   no third:

   * **Implicit (`update.auto = true`, pending version recorded).** At the top of `main()` PayCLI
     spawns the **detached child** (`Setsid` on unix, `CREATE_NEW_PROCESS_GROUP|DETACHED_PROCESS` on
     Windows, then `Process.Release()`) and **returns immediately**: bounded at the fork, zero network
     in the foreground, no `syscall.Exec`, no fall-through. The swapped binary takes effect on the
     **next** invocation. This is the only semantics compatible with §1 conflict 20: an in-process
     swap leaves the running image executing the *old* command surface while reporting the *new*
     version, and the download would otherwise block `pay get pages 16` for tens of seconds with no
     ceiling, indistinguishable to an agent from a hung server.
   * **Explicit (`pay update-self --apply`).** Runs in the foreground under a **total deadline of
     120 s** (`--timeout` overrides). On success the command exits **0 having done only the update** —
     it does **not** `syscall.Exec` the new binary and does not continue to any other work.

   **Windows rename-self dance** (`os.Rename` over a running `.exe` fails, §19.13), specified so it is
   not reinvented: `MoveFileEx(current → pay.exe.old, MOVEFILE_REPLACE_EXISTING)` → move the new
   binary into the original path → best-effort `Remove("pay.exe.old")` on the *next* start, ignoring
   `ERROR_SHARING_VIOLATION`. `update.auto = true` on Windows is therefore functional, not a silent
   no-op. The windows-latest CI job named in §16.2 exercises it against a `t.TempDir()`.
2. **`auto = false` by default** (§1 conflict 20).
3. **Verify more than a checksum, with three outcomes — not two.** `checksums.txt` comes from the
   same origin as the artifact, so it proves nothing against a compromised release. Download
   `checksums.txt.sig` + `.pem` and, when `cosign` is on PATH, `cosign verify-blob`; otherwise verify
   the GitHub attestation via the API. The result is one of:

   | Outcome | Meaning | Behaviour |
   |---|---|---|
   | `verified` | signature/attestation present and matches | proceed |
   | `unverifiable` | **no evidence either way** — cosign absent and the attestation API returned non-200 or timed out | warn loudly on stderr naming the reason; proceed **unless** `PAY_UPDATE_STRICT=1` |
   | `verification_failed` | a signature, attestation or checksum **is present and does not match** | **always abort**: exit 1 `update_verification_failed`, delete the downloaded artefact, print expected vs computed digest. `PAY_UPDATE_STRICT` is irrelevant here and **must not** be able to relax it |

   `pay update-self --check` prints the same three-state outcome. The distinction is the whole point
   of signing: a mismatching signature is positive evidence of exactly the compromised release this
   section exists to defend against, and installing anyway is strictly worse than not checking.
   §16.1 signs `artifacts: checksum`, so the chain is artifact → `checksums.txt` → signature and a
   mismatch anywhere in it is the same signal.

   A unit test against item 6's `httptest.Server` serves a tar.gz with a deliberately wrong checksum
   and asserts that **no swap occurred** and the exit code is non-zero **with `PAY_UPDATE_STRICT`
   unset**.
4. `GH_TOKEN` support and a `PAY_UPDATE_URL` override for private mirrors and air-gapped installs.
5. **Managed-install detection** extended beyond gsc's list: `go env GOPATH`/`GOBIN` (tell the user to
   `go install …@latest`), `/nix/store`, `~/.asdf`, `~/.local/share/mise`, `/opt/homebrew`,
   `/usr/local/Cellar`, `/snap`, `/var/lib/flatpak`, `C:\Program Files`, chocolatey, scoop.
6. **Testability.** The release API base URL and the HTTP client are fields on an `Updater` struct, not
   package-level constants, so tests point at an `httptest.Server` serving a tar.gz built in-test and
   assert the swap happened in a `t.TempDir()`.

**Frozen forever:** the archive name template `pay_{{ .Version }}_{{ .Os }}_{{ .Arch }}` is duplicated
in three places (goreleaser `name_template`, `internal/update.archiveAssetName`,
`install.sh`/`install.ps1`). Changing it after v0.1.0 permanently breaks self-update for every
already-installed binary, because old binaries construct the old name. A unit test asserts
`update.archiveAssetName("v1.2.3")` equals exactly what the template produces.

---

## 16. Release engineering

### 16.1 `.goreleaser.yaml`

```yaml
version: 2
project_name: pay
before: {hooks: [go mod tidy, go mod verify]}
gomod: {proxy: true}                                   # reproducible: fetch via proxy + sumdb
builds:
  - id: pay
    main: ./cmd/pay
    binary: pay
    env: [CGO_ENABLED=0]
    goos: [darwin, linux, windows]
    goarch: [amd64, arm64]                             # keep windows/arm64; gsc-cli drops it
    flags: [-trimpath]
    mod_timestamp: "{{ .CommitTimestamp }}"
    ldflags:
      - -s -w
      - -X main.version={{.Version}} -X main.commit={{.Commit}} -X main.date={{.CommitDate}}
universal_binaries: [{replace: false}]
archives:
  - id: default
    name_template: "pay_{{ .Version }}_{{ .Os }}_{{ .Arch }}"     # FROZEN, see §15
    format_overrides: [{goos: windows, formats: [zip]}]
    files: [README.md, INSTALL.md, LICENSE]
checksum: {name_template: checksums.txt}
sboms: [{artifacts: archive}]
signs:
  - cmd: cosign
    certificate: "${artifact}.pem"
    args: [sign-blob, "--output-certificate=${certificate}", "--output-signature=${signature}", "${artifact}", "--yes"]
    artifacts: checksum
    output: true
homebrew_casks: [...]          # NOT `brews` - fully deprecated as of GoReleaser v2.16
nfpms: [{formats: [deb, rpm, apk]}]
snapshot: {version_template: "{{ incpatch .Version }}-next"}
changelog: {use: github, sort: asc, groups: [Features, Fixes, Others],
            filters: {exclude: ['^docs:', '^test:', '^chore:']}}
release: {github: {owner: KLIXPERT-io, name: pay-cli}, draft: false, prerelease: auto}
```

gsc-cli's config omits `-trimpath`, `gomod.proxy`, SBOMs, signing, `main.commit`/`main.date`, and
windows/arm64. All are one-line fixes with real downstream value.

### 16.2 Workflows

Current versions verified today: `actions/checkout@v7.0.1`, `actions/setup-go@v7.0.0`,
`goreleaser/goreleaser-action@v7.2.3`, `golangci/golangci-lint-action@v9.3.0`,
`actions/attest-build-provenance@v4.2.2`, `sigstore/cosign-installer@v4.1.2`,
`anchore/sbom-action@v0.24.2`. gsc-cli pins checkout@v4, setup-go@v5 and goreleaser-action@v6 — all a
major behind.

* **`ci.yml`** (gsc-cli has **no test workflow at all** — this is the largest gap). On PR and push to
  main, `permissions: {contents: read}`. Jobs:
  * `lint` — `gofmt -l` must be empty, `go vet`, `go tool staticcheck`, golangci-lint,
    `go mod tidy && git diff --exit-code`, plus the `os.Stdout`/`os.Getenv`/`os.Exit`/`time.Now` grep
    from §3.1.
  * `test` — matrix over ubuntu / macos / **windows**, `go test -race -shuffle=on -count=1 ./...`.
    Windows matters because `lock_windows.go`, `platform_windows.go` and the `%LocalAppData%` paths are
    otherwise never exercised.
  * `build` — `goreleaser check` plus `release --snapshot --clean --skip=publish,sign,sbom`, so a
    release config that does not compile fails on the PR rather than on tag day.
* **`live.yml`** — nightly + `workflow_dispatch`, Postgres + a Payload 3.x fixture app as service
  containers, runs `make test-live` and `make test-contract`. Guarded with
  `if: github.repository == 'KLIXPERT-io/pay-cli'` so forks do not fail.
* **`release.yml`** — tag push + `workflow_call`, `permissions: {contents: write, id-token: write,
  attestations: write}`, cosign + SBOM + provenance attestation over `dist/*.tar.gz`, `dist/*.zip`,
  `dist/checksums.txt`.
* **`tag-and-release.yml`** — keeps gsc-cli's VERSION-file-drives-the-tag design (one file, one commit,
  one release) but **replaces `butlerlogic/action-autotag@1.1.2` with ~10 lines of inline shell**
  (read VERSION, `git rev-parse -q --verify refs/tags/v$V`, `git tag -a`, `git push origin`), removing
  a third-party action with write access from the supply chain.
* **`codeql.yml`**, **`dependabot.yml`** (gomod + github-actions, weekly, grouped).

All non-`actions/*` actions are pinned by full commit SHA with a `# vX.Y.Z` comment.

### 16.3 Installers

`install.sh` reuses gsc-cli's sh-only, jq-free tag parse and `checksums.txt` verification, with:
`PAY_VERSION` / `INSTALL_DIR` env names; **`$HOME/.local/bin` as the default install dir** (agents
rarely have sudo, and a user-writable install is a prerequisite for self-update to work at all);
`--version`, `--dir`, `--dry-run`, `--with-skills` flags; `GH_TOKEN` honoured for the
`api.github.com` call (anonymous GitHub API is 60 req/h per IP and shared CI IPs hit that constantly);
optional `cosign verify-blob` when cosign is present; and a final JSON line
`{"version":…,"path":…}` when `PAY_INSTALL_JSON=1`. It also prints §18's `binary_on_path` advice
when the install directory is not on the PATH of a non-interactive shell, and warns when another
`pay` earlier on PATH shadows the installed one.

`install.ps1` does real architecture detection (`[Environment]::Is64BitOperatingSystem` /
`$env:PROCESSOR_ARCHITECTURE`) rather than gsc-cli's hardcoded `windows_amd64`, because
windows/arm64 now ships.

### 16.4 Version embedding

`-X main.version/main.commit/main.date`, read in `cmd/pay/main.go` and passed to `internal/buildinfo`.
`buildinfo.Version()` falls back to `debug.ReadBuildInfo()`'s `vcs.revision` + `vcs.modified` when
ldflags are absent, so `go install …/cmd/pay@latest` produces something better than `dev`.
`pay version --output json` emits
`{"version","commit","date","go","os","arch","install_path","managed"}`.

---

## 17. Testing

### 17.1 Three tiers

**Tier 1 — hermetic unit tests.** `make test`, zero network, passes on a fresh clone with no
credentials. A `TestMain` sets `PAY_HOME` to `t.TempDir()`, sets `PAY_NO_UPDATE=1`, and replaces
`http.DefaultTransport` with a RoundTripper that **panics** on any call.

Covers: the `config.Resolve` precedence matrix (the highest-value table test in the repo); the secret
chain with fake env/fs/exec; the `where` JSON encoder and the bracket encoder (~60 golden query
strings including `where[or][0][and][0][x][equals]=y` and `sort=-createdAt`); `apierr` mapping for all
six body shapes; exit-code map **totality**; retry/backoff with an injected clock; cache TTL,
stale-on-error and the `cacheable` predicate with a fake clock; output goldens; discovery parsing from
fixtures; and **golden tests of the generated help text** for a fixed schema fixture, so a discovery
change that degrades agent-facing help fails CI.

Plus, one test per blocker this spec closes — each one fails loudly if the corresponding rule is ever
softened:

| Test | Asserts |
|---|---|
| `TestScopeEqualityTable` | scope equality/inequality over header set, `graphql_path`, `api_path`, key, profile name, and `auto` vs explicit `auth_collection` (§8.1) |
| `TestFingerprintLengthIsSixteen` | every producer of a key fingerprint emits the identical 16 hex chars (§4.4) |
| `TestGetChecksTheIDType`, `TestIDTypeUnknownIsAnnounced` | an unknown id type never rejects `pay get c 66f1a2b3…` locally: the request is made (no exit 5) and `id_type_unknown` says the check was skipped (§9.3) |
| `TestClassifyWiring` (case "german validation body still classifies") | a German validation body still normalises to `validation_failed` with per-field paths (§11.2) |
| `TestNoSecretInAnyOutput` | repo-wide grep of golden `.out`/`.err`, audit lines and log sinks for the fixture key (§3.1) |
| `TestReactiveIsOptIn` | the reactive invalidator never fires on a request without `WithReactiveInvalidation`, which discovery never sets — exempt by construction (§8.4a) |
| `TestBulkWriteSendsNoLimit`, `TestBulkWritesAreIDScopedAndNeverSendLimit` | `pay update c --where x --max-docs 2` emits no `limit=` parameter (§12.3) |
| `TestUpdateSelfAbortsOnBadChecksum` | wrong checksum ⇒ no swap, non-zero exit, `PAY_UPDATE_STRICT` unset (§15.3) |
| `TestEveryHintIsAnswerable` | every `pay …` command the shipped code names — a backticked span in any string, a `WithHint` argument, an `Example.Cmd`, a `Synopsis` line — resolves against the real command tree: each word a subcommand, each `--flag` defined on it, each value of an enumerated flag one of its `Values` (§7.10, A15) |
| `TestFieldKeySetIsExactAndTotal` | every field entry has exactly the §7.8.2 key set, nulls included |
| `BenchmarkWarmPath` | index + one shard resolution stays under **5 ms** for a synthetic 150 × 200 manifest; CI-enforced (§8.2) |

The agent-ergonomics release adds three **in-memory Payload models**, each encoding the verified server
behaviour its feature depends on, so the rules are tested without a live server:

* **`pgSim`** (`internal/cli/orphans_test.go`) — the drizzle storage of §10.2.1.1: tables per block
  type, `_path`/`_order`, clearing by reachability, `{}` on read, versions built from the read-back, the
  `unpublishAllLocales` and draft branches, a required `bento.blocks`, a PATCH failure injector and a
  per-locale `_status` switch. Mutation-checked: dropping the switch or keeping nested rows in carriers
  fails the tests.
* **`docStore`** (`internal/cli/backups_paths_test.go`) — 3.87's draft model (main row vs newest
  version, draft saves, plain saves merging into the newest) for backups and restores.
* **`memCMS`** (`internal/cli/sync_test.go`, `sync_align_test.go`) — an in-memory Payload with drafts for plan/sync/upsert.

| Test | Asserts |
|---|---|
| `FuzzMarkdownRoundTrip` | md → lexical → md → lexical is stable (§9.12.4); the 7 regressions it found are seeds |
| `TestIrreversibleWritePathsBackUpFirst` | every write path of §12.8.2 writes one file per id, named after the command, BEFORE its first write request, into `meta.backups` and the audit pre-record; fails when a call site drops `.withBackup` |
| `TestBackupsRestoreBringsBackThePublishedStateAndTheDraft` | the two-step restore of §12.8.8 restores the exact pre-write state |
| `TestBackupKeepsContentAndMasksOnlyCredentials` | §12.8.4's narrow rule set + `backup_redacted` |
| `TestOrphanHintsNameRealCommands`, `TestCensusHintsNameRealCommands`, `TestPlanHintsAndUpsertPublishHint` | the new hints (`--reset-nested --dry-run`, `pay blocks learn`, `pay publish … --reset-nested`, …) name real commands and flags (§7.10's rule) |
| `TestOutlineFieldsAreUsableWithPayBlocks` | every outline row's `field` + `selector` is listed by `pay blocks ls` (§9.13) |
| `TestRenderedDocumentsAreRefused` | every write path refuses a `--richtext md\|text` envelope with nothing sent (§9.12.8.3) |
| `TestStoredDocMasksCredentialsOnly` | `redact.Scrubber.StoredDoc` masks credentials, keeps content |

Pure packages carry their own table tests (`internal/{lexical,docdiff,orphans,manifest,rows}/*_test.go`,
`internal/discovery/census*_test.go`, `internal/cache/census*_test.go`). Goldens touched by the release:
`help_root.out` (new commands), `explain_slim.out` (code totals), `explain_section_gotchas.out`.

**Tier 2 — live integration.** Build tag `//go:build live`, skipped unless `PAY_TEST_BASE_URL` and
`PAY_TEST_API_KEY` are set. Asserts what fixtures cannot: that a bad key yields 200 + `user:null`; that
a create → update → trash → restore → permanent-delete round trip works; that bulk `DELETE` ignores
`limit`; that a 400 validation error carries per-field paths; that `sort` on an unknown field is
silently ignored; that pagination/depth/select behave.

**Tier 3 — contract tests.** Run the same request specs against both the live server and the recorded
fixtures and compare **shape** (the recursive set of JSON key paths + value kinds), not values. A
mismatch means Payload drifted and the fixtures are lying; the failure message says
`run "make fixtures"`. This is the only thing that catches an envelope change in Payload 3.87 before
users do.

### 17.2 Fixtures

`internal/payloadtest.NewServer(t)` returns an `httptest.Server` routing `METHOD + path +
canonicalised query` to `testdata/fixtures/<name>.json` via `manifest.json`. Fixtures are recorded by
`go run ./tools/recordfixtures -base $PAY_TEST_BASE_URL -key $PAY_TEST_API_KEY`, which hits a live
instance for a fixed request list, runs every body through `internal/redact`, and writes
pretty-printed, key-sorted JSON plus a manifest recording `{payload_version, recorded_at,
request_spec}`.

**go-vcr is explicitly rejected**: its YAML cassettes are opaque to both humans and LLMs, and we need
project-specific transforms (redaction, key sorting, trimming). Plain readable JSON an agent can `cat`
and diff is worth more than the library.

Measured sizes make plain committed JSON fine: `/api/access` = 30,585 B, a 1-doc pages list = 497 B, a
targeted GraphQL `__type` = 237 B.

### 17.3 CLI-level golden tests

Because `cli.Run(ctx, App{Args, Env, Stdin, Stdout, Stderr, Now, HTTP, Dirs}) int` takes injected IO,
whole-command tests run **in-process** against the fixture server: set `Env` `PAY_BASE_URL` to
`srv.URL`, capture stdout/stderr bytes and the int return, compare against
`testdata/golden/<case>.{out,err,exit}`. `UPDATE_GOLDEN=1` rewrites them. No subprocess, so coverage
works and Windows is free.

### 17.4 Makefile

```make
check:         fmt-check vet lint test         ## what CI runs
build:         go build -trimpath -ldflags '$(LDFLAGS)' -o pay ./cmd/pay
install:       go install -trimpath -ldflags '$(LDFLAGS)' ./cmd/pay
fmt:           gofmt -w . && go tool goimports -w .
fmt-check:     test -z "$$(gofmt -l .)"
vet:           go vet ./...
lint:          go tool staticcheck ./... && ./scripts/arch-lint.sh
tidy-check:    go mod tidy && git diff --exit-code go.mod go.sum
test:          PAY_HOME=$$(mktemp -d) PAY_NO_UPDATE=1 go test -race -shuffle=on -count=1 ./...
test-cover:    ... -coverprofile=coverage.out ./... && go tool cover -func=coverage.out
test-live:     @[ -n "$$PAY_TEST_BASE_URL" ] || { echo 'set PAY_TEST_BASE_URL and PAY_TEST_API_KEY'; exit 2; }; \
               go test -tags live -count=1 -race ./internal/payload/... ./internal/discovery/... ./internal/cli/...
test-contract: go test -tags live -run TestContract -count=1 ./internal/payloadtest/...
test-all:      test test-live test-contract
fixtures:      go run ./tools/recordfixtures -base $$PAY_TEST_BASE_URL -key $$PAY_TEST_API_KEY -out testdata/fixtures
golden:        UPDATE_GOLDEN=1 go test ./... && git diff --stat testdata/golden
snapshot:      goreleaser release --snapshot --clean --skip=publish,sign,sbom
release-check: goreleaser check
clean:         rm -rf pay dist coverage.out
```

`make test-live` **fails with exit 2** when the env vars are missing rather than silently skipping —
the usual failure mode is integration tests that skip forever while everyone believes they ran.

A guard at the top of the Makefile detects `go env GOTOOLCHAIN == local` with a system Go older than
1.26 and explains the fix. This is a real hazard on the current development machine, where the system
Go is 1.22.2 and everything works only because `GOTOOLCHAIN=auto` fetches go1.26.x transparently.

---

## 18. `pay doctor` and `pay explain`

`pay doctor` is the single command an agent runs when confused, and its help text says so. It reports,
in one JSON document: base-URL reachability; the `X-Powered-By` check; the resolved `auth_mode` and,
in `api-key`/`jwt` mode, `/api/{auth}/me` identity verification (checking `user != null`, **not** the
status code, with `apiKey` redacted); **which auth collections on this project actually have
`useAPIKey`** and, when none does, the JWT login command instead of `pay auth login --api-key`
(§5.4); the resolved `auth_collection` and its source; GraphQL availability and mode; the resolved
`api_path` and whether `graphql_path` was derived or overridden; measured latency for `/api/access`;
the current topology and schema fingerprints; `payload_version` / `db_adapter` and their sources
(explicitly `null` / `"unknown"` when undetermined, never invented); the cache directory path, size,
per-scope ages, and **whether any cache write has been failing** (§8.7); audit-log writability
(§12.7); whether any cached scope's fingerprint currently mismatches the server; whether the skill is
installed and stale; and whether a newer release is pending, with its verification outcome (§15.3).

**Checks added by the agent-ergonomics release** (`data.checks[]` in this order on a healthy run —
verified on localhost: `binary_on_path, base_url, site_url, url_paths, preview, credential,
reachability, powered_by, discovery, graphql_path, graphql, payload_version, db_adapter,
auth_collection, api_key_support, identity, cache, cache_writes, cache_fingerprint, block_census,
audit_log, backups, skill, update`; `data` gains `binary`, `site` and `backups`):

* **`binary_on_path`** (always, first; `data.binary = {executable, dir, on_path, first_on_path}`): looks
  for `pay` (`pay.exe`) in each `$PATH` entry, resolving symlinks, and compares with the running binary
  (`App.Executable`, `os.Executable()` taken in `Main`). `ok` — `pay` on PATH is this binary; `warn` —
  not on PATH (agents' non-interactive shells do not read `~/.bashrc`, §2.9) or another `pay` shadows it;
  `unknown` — executable not known. Hint: `ln -sf <exe> <dir>/pay` into a directory already on PATH, or
  export PATH in the agent's own environment (Claude Code: `"env"` in settings.json) and in
  `~/.profile` + `~/.zshenv`; until then use the absolute path.
* **`site_url`, `url_paths`, `preview`** (local, no request; `data.site = {site_url, site_url_source,
  url_paths, preview_path, preview_path_source, preview_env_vars, preview_profile_env_var,
  preview_key_source, preview_key_fingerprint, cross_origin}`): `preview` warns when `{secret}` is needed
  and nothing resolves (naming the three ways to configure it) or when `site_url` is cross-origin. The
  secret value is never printed (tested).
* **`graphql`** with mode `introspection_disabled` is **`warn`** (was `unknown`) and says exactly what
  degrades: discovery is REST-observed, field lists come from three sampled documents, required flags are
  unknown, block slugs and schemas are not published, and the block census stands in (`observed-census`).
  Hint: set `graphQL: { disableIntrospectionInProduction: false }` in `buildConfig()` and redeploy (the
  Payload 3.x default is `true`, applied only under `NODE_ENV=production`); until then `pay blocks types`.
* **`block_census`**: none cached → `ok` ("built on first use of `pay blocks types|schema|new|validate`"),
  or `warn` when GraphQL is not `ok` (writes cannot check block rows); cached → entities, oldest age,
  entities with blocks ("ctas (2 types), forms (9 types), pages (33 types)"), `warn` + hint
  `pay blocks learn` when older than 24 h or an entity read failed; `value` = the census path.
* **`backups`** (`data.backups = {dir, source, enabled, no_backup, profile_dir, writable, files}`):
  `skipped` (no directory; hint: set `PAY_BACKUP_DIR`), `warn` (configured but `--no-backup`), `fail`
  (not writable — "every write would abort with backup_failed"; the probe creates the profile directory
  and a temp file it removes), `ok`.
* **Local checks always run.** `audit_log`, `backups`, `skill` and `update` need no server and run on
  every path, including the early returns for a missing `base_url`, an unusable client and failed
  discovery (before this release doctor stopped at the first of those, so an agent with no `base_url`
  never learned its backup directory was unwritable). Server-dependent checks still do not run there.

`pay explain` is the "what can I do here?" call, cache-backed and offline after the first run. It is
**size-tiered**, because a 400 KB capability dump is functionally the same as no capability dump — the
agent truncates it arbitrarily:

| Tier | Content | Target size |
|---|---|---|
| `--slim` | connection + **every** slug + exit codes | < 1.5 KB at 49 collections |
| default | as above plus per-collection ops/features/key fields/gotchas, **detail** truncated at 40 entries | < 15 KB |
| `--full` | every field of every entity; refuses above `--max-bytes` (default 400 KB) with exit 5 | — |
| `--section X` | one slice, with `--offset` / `--limit` / `--grep` | — |
| `--collection X` | one entity, full detail | — |

**The slug list is never truncated.** `data.collection_slugs` always contains **every** slug — 49 of
them is about 1 KB, and it is the single most load-bearing thing `pay explain` returns. Only the
per-collection *detail* array is capped.

**Detail ordering is specified, not incidental:** non-internal before internal, then
`stats.total_docs` descending, ties alphabetical. The original spec truncated at 40 with no defined
order, and the natural choice given §8.2's key-sorted-JSON rule is alphabetical — under which the 9
entries past the cap on the live project are `payload-folders, payload-jobs, payload-locked-documents,
payload-migrations, payload-preferences, `**`posts`**`, redirects, search, `**`users`** (verified).
An agent asking "what can I do here?" was told about 30 `crm-*` collections and never saw `posts`, and
would conclude blog posts do not exist on a blog.

**The truncation warning carries a literally runnable recovery**, which the original could not: its
documented recovery was "a `next.cmd` to page by group", but §9.2's signature for `pay explain` had no
offset, page, group or filter flag anywhere. Those flags now exist (§9.2):

```json
{"code": "collections_truncated",
 "message": "detail shown for 40 of 49 collections; all 49 slugs are in data.collection_slugs",
 "hint": "pay explain --section collections --offset 40"}
```

`pay explain --section collections [--offset N] [--limit M] [--grep PATTERN]` and
`pay collections --grep PATTERN` are the paging and filtering surface. `--grep` matches the slug and
both labels, case-insensitively, as a plain substring (not a regex — an agent-supplied regex is an
error surface with no upside here).

`meta.bytes` is always set so an agent can budget its own context before asking for more.
The `gotchas` array travels **in the payload**, not only in the skill file, so the warnings reach
agents that never installed the skill.

The release adds three gotchas, each with a runnable `do` (a unit test resolves every gotcha's `do` to a
real command and its flags):

* `unknown_fields_dropped` (always): Payload silently drops unknown fields, unknown keys inside block
  rows and rows whose blockType it does not know, and still answers 200 →
  `pay blocks validate --collection <collection> --data-file body.json`.
* `introspection_disabled` (when `capabilities.graphql.mode = introspection_disabled`): describe knows
  only REST-observed fields; the census learns blocks from real documents → `pay blocks types <collection>`.
* `nested_block_type_change_orphans` (Postgres): a blockType change (or removing a row holding nested
  blocks) at a NESTED path leaves `{}` orphan rows; `pay outline` shows them; `--reset-nested` avoids or
  repairs them keeping the id → `pay update <collection> <id> --data-file body.json --reset-nested`.

A body `_status` that publishes and a `--richtext` write-back are deliberately **not** gotchas: PayCLI
guards both at runtime (`body_publishes`, `richtext_rendered`).

---

## 19. Known risks carried into implementation

These are accepted, not solved. Each has a mitigation that must be implemented.

1. **Adapter-dependent behaviour, on an adapter the API does not reveal.** `all` 500s on Postgres but
   **works** on Mongo; `like` word-splitting and `like`-on-relationship-becoming-`equals` are
   drizzle-specific; `formatErrors` has a Mongoose branch emitting `field` instead of `path`;
   `id_type` is numeric on Postgres and a 24-hex string on Mongo; an invalid enum value 500s on
   Postgres but returns 0 docs on Mongo. *Mitigation:* `db_adapter` is **nullable with provenance**
   and inferred only from observed id shapes (§7.11); `unsupported_operators` starts **empty** and is
   learned reactively, with a client-side block only under `db_adapter_source == "configured"`;
   adapter-independent positive signals (`docs` key present, `Route not found` absent) are preferred
   over 500-based discrimination; both validation shapes are handled.
2. **`/api/access` permission values may be `{permission, where}`.** Unverified but structurally
   possible. *Mitigation:* the parser accepts `bool | object`; `pay can` reports `conditional` if it
   ever sees the object form, rather than a flat yes.
3. **The 16 KB URL cliff is Node's default, not a Payload constant.** nginx (8 KB
   `large_client_header_buffers`), Cloudflare (~16 KB total / 8 KB per header) and a Node started with
   `--max-http-header-size=65536` all differ, and some proxies return 414 rather than 431.
   *Mitigation:* the 8,000-byte auto-promotion threshold is deliberately conservative and the fallback
   triggers on 413, 414 and 431.
4. **The method-override `Content-Type` check is a Payload-version implementation detail.** A proxy
   that rewrites or appends to `Content-Type` silently unscopes the request. *Mitigation:* never use
   the override for `PATCH`/`DELETE` (§6.2).
5. **Bulk partial-commit granularity depends on `db.bulkOperationsSingleTransaction`.** It defaults to
   false, but adapters may set it true, and MongoDB without transactions behaves differently again.
   *Mitigation:* the client contract is *assume a partial commit happened*, unconditionally.
6. **Exit 7's hint assumes non-transactional bulk semantics.** If a future Payload wraps bulk writes in
   one transaction, "the successful writes are already committed" becomes actively wrong.
   *Mitigation:* gate the hint on `payload_version` **when it is known**; when it is `null` (§7.11)
   the hint uses its unconditional wording ("assume the successful writes committed"), which is the
   safe direction — it costs a redundant verification read, whereas the opposite error costs a
   duplicated write. Re-verify per minor release via the contract tests.
7. **Neither fingerprint detects a field added to an existing collection.** *Mitigation:* Level-3
   reactive invalidation on `QueryError` (§8.4), which is the only mechanism that catches field drift.
8. **`pay explain` size, and manifest decode cost, scale with the project.** 49 collections here; a
   large install can have 150+ with 500+ fields each. *Mitigation:* the §18 tiering truncates
   **detail** only (never the slug list) with an explicit order and real paging flags, and §8.2's
   index/shard split keeps the per-invocation decode cost flat — measured 1.44 ms at 150 collections
   versus 282 ms for the consolidated layout, with a CI benchmark holding the line at 5 ms.
9. **Help text embeds discovered values and is therefore project- and cache-dependent.** An agent that
   caches help from project A and reuses it against project B will hallucinate slugs. *Mitigation:*
   the `profile` / `base_url` / `discovery_revision` stamp on every help block containing discovered
   data (§10.5).
10. **A developer who adds a collection and does not see it will conclude PayCLI is broken.**
    *Mitigation:* the 10-minute topology TTL plus the 14 ms Level-1 probe, `pay discover --refresh`
    prominent in root help, and it named explicitly in the `collection_unknown` hint.
11. **Writing the error envelope to stdout diverges from Unix convention.** Anyone running
    `pay find … | jq '.data[]'` gets a parse-friendly but semantically empty result on failure.
    *Mitigation:* stated loudly in README and SKILL.md, plus `PAY_ERRORS_TO=stderr`.
12. **Fixtures drift as Payload ships minor releases.** Without the scheduled contract tier, unit tests
    keep passing against stale fixtures while the CLI is broken against current Payload — the worst
    possible failure mode for a tool whose premise is adapting to any project. *Mitigation:*
    `live.yml` nightly.
13. **Windows is the highest-risk platform** (no flock, `os.Rename` over a running `.exe`, unlinking an
    open file fails outright, `%AppData%` vs `%LocalAppData%`). *Mitigation:* windows-latest in the CI
    test matrix from day one; the §15.1 `MoveFileEx` rename-self dance; and §8.7's
    rename-to-`.trash-<ulid>`-before-unlink GC, which is what makes lock-free cache reads safe there.
14. **The shared dev instance is not exclusively owned.** During this spec's verification, probe
    documents from a sibling session were visible, and one page was deleted mid-test by another
    session. *Mitigation:* live tests namespace their fixtures and must not assume exclusive ownership
    of ids. All documents this spec's verification created (crm-contacts 225–230) were deleted; the
    `pages` collection was already in a degraded seed state (null titles and slugs) before any
    PayCLI work began.
15. **An i18n-configured project changes every user-facing server string.** PayCLI pins
    `Accept-Language: en` (§6), but a project may not support `en` at all, a `payload-lng` cookie or a
    proxy may override the header, and a custom `translations` map can reword anything. *Mitigation:*
    no classification depends on a translated string (§11.2); the only English matches permitted are
    the two hardcoded ones (`Route not found`, `Cannot <METHOD>`); label harvesting is fail-soft to a
    slug-derived label; and `error_validation_de.json` is a committed regression test.
16. **Project-source scanning (§7.10, §7.11) reads files PayCLI does not own.** A monorepo may contain
    several Payload configs, and a `slug:` literal may live in a file that is not a block. *Mitigation:*
    the scan is bounded (200 files, 2 MB), never imports or evaluates anything, requires a sibling
    `fields:` key, records every source file in `diagnostics.block_slug_files[]`, and is always
    overridable by the profile's `blocks` map. A wrong slug produces the same
    `input_silently_dropped` warning an unknown slug already does — it never makes things worse than
    the `unknown` state it replaces.
17. **`anonymous` mode discovers a truncated project and looks like a broken one.** Verified: 10
    collections anonymous versus 49 authenticated. *Mitigation:* the `anonymous_session` warning on
    **every** envelope (§5.0), `keyFP = "anon"` keeping the two scopes apart (§8.1), and 403 → exit 2
    `auth_required` with a login hint rather than exit 8 `access_denied` (§11.5).
18. **The block census is observation, and observation lags the site.** A deploy adds block types, a
    migration moves rows, a sampled collection is truncated at `census_max_docs`. *Mitigation:* an
    observation only ever warns (§7.10c.4); writes overlay their pre-image and fold their echo (§9.7);
    `validate` rebuilds reactively with a 10-minute cooldown; every observation-based warning states the
    census age and names `pay blocks learn`; `census_truncated` is explicit.
19. **`--reset-nested` depends on drizzle-adapter internals** (§10.2.1.1: which tables an update clears,
    `unpublishAllLocales` skipping validation). A future `@payloadcms/drizzle` could clear nested tables
    itself (making the remedy unnecessary) or change the switch. *Mitigation:* the remedy is opt-in, its
    dry run shows every phase body, the stored row is always read back afterwards
    (`meta.reset_nested.verified`), a per-locale `_status` is refused, a failed restatement is saved with
    its recovery command, and `pgSim` pins the modelled behaviour. Re-verify per Payload minor release.
20. **Publication-state rules assume 3.87's draft model** (update-by-id merges into the newest version;
    bulk update into the stored row; `isSavingDraft`). *Mitigation:* the rules are guards that refuse or
    warn, never silent re-routing (§1 conflict 44); each is backed by a verified source line (§2.9) and
    by `docStore`/`pgSim` tests.
21. **Backups hold document content in plaintext on disk**, credentials masked but content kept by
    design (§12.8.4). *Mitigation:* off until a directory is chosen; files 0600, directories 0700;
    `backups prune` is L2; `pay doctor` reports the directory. Localized collections back up one locale.
22. **The preview handshake is website-template-specific** (`/next/preview?path=&previewSecret=`).
    *Mitigation:* `preview_path` is configurable with placeholders; a missing route is `preview_failed`
    (exit 10), not a Payload error; credentials never cross an origin (§6.3).
23. **A manifest written without row ids pairs rows by blockType in stored order** (§9.15.5), so deleting
    a row from an id-less manifest shifts later rows onto their predecessors' ids. *Mitigation:* the plan
    diff shows it before any write; exporting with `pay get --depth 0 --draft` keeps the ids.


---

## 20. Open questions

Only items genuinely blocked on the product owner. Everything else is decided above.

1. **Does PayCLI ship a Homebrew tap and Linux packages at v0.1.0, or binaries + `install.sh` only?**
   `.goreleaser.yaml` contains `homebrew_casks` and `nfpms` blocks; they need a tap repository
   (`KLIXPERT-io/homebrew-tap`) and a decision on whether to maintain deb/rpm/apk. Blocking only the
   release workflow, not any code.
2. **Is a non-GitHub / air-gapped distribution channel required?** `PAY_UPDATE_URL` exists as a hook,
   but whether to build and test a mirror path is a product call.
3. **Should `pay` ever write to a Payload project's `payload.config.ts`, or is it strictly an API
   client?** This spec assumes strictly an API client. If config-file editing is in scope, it is a
   separate subsystem (TypeScript AST manipulation) that changes the dependency footprint entirely.
4. **Is an MCP server mode (`pay mcp serve`) planned?** `internal/cache.Session` is designed so a
   long-lived process could hold a bounded LRU, but no persistent document cache is shipped. Knowing
   now prevents a later refactor of the session lifetime.
5. **Should the advisory Payload version floor ever become a hard floor?** *Decided for v0.1.0:*
   **no.** `payload_version` is unobtainable from the API (§7.11), so a hard floor would refuse to run
   against every project PayCLI cannot introspect — the opposite of §21's contract. PayCLI declares an
   *advisory* floor of **3.0**, emits `warning{code:"payload_version_unknown"}` or
   `warning{code:"payload_version_below_floor"}`, and proceeds. The only open question left for the
   owner is whether a **future** major version should gain a hard floor and a refusal path; nothing in
   this spec depends on the answer.

---

## 21. Portability contract

This is the complete list of what PayCLI assumes about a Payload project, what it does when the
assumption fails, and where that behaviour is specified. **Only the four in §21.1 can stop PayCLI;
every assumption in §21.2 fails into a degraded but working CLI.** Nothing in this table is a guess:
each assumption is either verified against the live instance or read out of `payload/dist`.

### 21.1 Hard requirements (four, and only four)

| # | Assumption | If it fails |
|---|---|---|
| H1 | The base URL serves a Payload REST API: `GET {base}{api_path}/access` returns **200**, `Content-Type: application/json`, and a decoded object with a `collections` key | `endpoint_not_payload` (exit 9), naming the resolved URL and, when §4.3 found a `payload.config.ts` with a `routes:` literal, the `--api-path` value to try. `api_path` is autodiscovered first over `[configured, /api, /cms-api, /payload-api, ""]` (§7.2) |
| H2 | It is Payload **3.x** REST shape: `{docs, totalDocs, limit, page, …}` for lists, `{errors:[…]}` or `{message}` for errors | Bodies that do not parse into the expected shape are never cached (§8.3 rule 8) and become `non_json_response` (exit 6) with a ≤200-char excerpt |
| H3 | If the operator supplied a credential, it is valid | `auth_invalid` (exit 2). A *missing* credential is **not** a failure — see A1 |
| H4 | The cache directory, or the ability to run without one | A read-only/full `$CACHE` yields `cache_write_failed` warnings and a per-invocation discovery, never a failed command (§8.3, §8.7). `rm -rf $(pay cache path)` is always safe |

### 21.2 Everything else degrades

| # | Assumption | Verified reality when it fails | PayCLI behaviour | Spec |
|---|---|---|---|---|
| A1 | A credential is available | Many projects allow public reads; `useAPIKey` is **opt-in per collection** and most projects have none | `auth_mode: "anonymous"`: no `Authorization` header, `/me` skipped, `identity.verified = false`, permissions from the anonymous `/access`, `anonymous_session` warning on every envelope, 403 → exit 2 `auth_required` | §5.0, §11.5 |
| A2 | The auth collection supports API keys | `apiKey` fields exist only `if (authConfig.useAPIKey)` | `auth_mode: "jwt"` via `POST {api}/{auth}/login`, `Authorization: JWT <t>`; token + expiry stored, never the password; one silent re-login on a read 403, **never** on a write. `pay doctor` reports which collections have `useAPIKey` and prints the JWT command when none does | §5.0, §5.4 |
| A3 | `auth_collection` is known | The slug is in the header, and a **wrong** slug returns 200 with the anonymous view (10 vs 49 collections) — indistinguishable from a low-privilege key | Stage -1 bootstrap: anonymous `/access` for candidates (never cached, never an inventory) → GraphQL `me{S}`/`initialized{S}` → project source → `/{slug}/init` filter → first `/{slug}/me` with `user != null` wins, deterministic order. Resolved slug persisted in `auth-resolution.json`. Zero matches ⇒ `auth_collection_unknown` (exit 9), never a guess at `users` | §7.0 |
| A4 | GraphQL is reachable and introspectable | `graphQL.disable: true`; `disableIntrospectionInProduction` (fires only on a literal `__schema`/`__type` field, only under `NODE_ENV=production`); custom `validationRules`; `maxComplexity` | Mode probe **contains `__type`** so the guard actually fires. Any 200 with non-empty `errors[]` or a null expected alias is a **degrade trigger**, not a failure: record the mode, append `diagnostics.degraded[]`, fall through to REST-only, emit a usable manifest. Block knowledge then comes from the block census (§7.10c), and `pay doctor` names the config switch. `discovery_failed` (exit 10) only if REST-only *also* fails | §7.6, §7.10c |
| A5 | The GraphQL batch fits under `maxComplexity` | 450 aliases / 37 KB passed live (200, 529 KB, 0.47 s), but the ceiling is configurable | Halve the alias batch and retry on a complexity error, a null alias, or a >8 MB body; ≤4 batches; then degrade to REST-only for the unresolved entities | §7.4 |
| A6 | `id_type` is knowable | GraphQL off **and** MongoDB ⇒ no `Query.{S}(id:)` arg | Ladder: `select[id]=true` sample → `/versions` `docs[0].parent` → profile pin → **`"unknown"`**. `"unknown"` ⇒ the client-side `invalid_id` check is **skipped** and the server answers, with `id_type_unknown` warning. Never defaulted | §7.6, §9.3 |
| A7 | Capability flags are knowable | `duplicate` is GraphQL-only; `use_api_key` is GraphQL-only | Flags are `true \| false \| null` with per-flag `flags_source`. `null` ⇒ **attempt the operation** and classify the server's 404/501 as `feature_unavailable` after the fact, with `pay discover --refresh`. Never block locally on `null` | §7.6 |
| A8 | The collection has documents to sample | 10 of 49 live collections are empty | `FIELDS_UNAVAILABLE`; fall back to `/versions?limit=1`, then `--deep --allow-write-probes`, then declare the gap | §7.6 |
| A9 | Localisation is off, or its codes are enumerable | `fallback` defaults to **true**, so `--locale de` silently returns English; unknown codes are coerced to the default with 200; GraphQL enum names are `formatName`-mangled | `fallback-locale=none` on every read by default; codes from `locale=all` key sets, cross-validated against the enum count; `--locale` validated only when `locales_source` ∈ {configured, locale-all}, else passed through with `locale_unverified`; `meta.locale` always echoed | §7.9 |
| A10 | Per-field `localized` is knowable | Neither REST nor GraphQL exposes it | `localized: null`, `LOCALIZATION_PER_FIELD_UNKNOWN`, and §10.2's echo-diff **skips** those fields so a `locale=all` write produces no bogus `input_silently_dropped` | §7.9d, §10.2 |
| A11 | Server strings are English | `deletedCountSuccessfully`, `followingFieldsInvalid`, `noFilesUploaded`, `notAllowedToPerformAction` all go through `req.t` | `Accept-Language: en` is sent, **and** no classification depends on a translated string: `errors[].name` + status only. Label harvest is fail-soft to a slug-derived label + `LABELS_UNAVAILABLE`. `error_validation_de.json` is a committed regression test | §6, §7.5, §11.2 |
| A12 | `routes.api` is `/api` and GraphQL is at `/api/graphql` | `routes.graphQL` is **nested under** `routes.api` | `graphql_path = api_path + graphql_route`, derived unless explicitly overridden, shown as `source: "derived:api_path+graphql_route"`; `api_path` autodiscovered; a `route_missing` GraphQL probe with a non-default `api_path` names the derived candidate in its hint | §4.2, §7.2, §7.6 |
| A13 | The DB adapter is known | Not discoverable from the API; `all` 500s on Postgres and **works** on Mongo | `db_adapter` nullable with provenance, inferred only from observed id shapes. `unsupported_operators` starts **empty** and is learned reactively; the client-side block applies only under `db_adapter_source == "configured"` | §7.11 |
| A14 | The Payload version is known | No header, no endpoint carries it | From the profile pin, else the project's `package.json` (local FS, no network), else `null` + `PAYLOAD_VERSION_UNKNOWN`. `meta.payload_version` is honestly `null`; version-gated hints use their unconditional wording; the version floor is **advisory only** | §7.11, §20.5 |
| A15 | Block `blockType` slugs are recoverable from the API | They are not when `interfaceName` is set, and the observed-values fallback yields nothing when every `layout` is `[]` | Profile `blocks` map → project-source scan (`slug:` literals near `fields:`, bounded, never evaluated) → observed values → `blocks_source: "unknown"`, collection `publishable: false` with a plain-words reason and the file to read. The block census (§7.10c) then learns slugs, nested placements and row schemas from real documents, labelled `observed-census`, and never gates on them. **No hint may name a command that cannot answer it** — unit-tested (`TestEveryHintIsAnswerable`) | §7.10, §7.10c |
| A16 | Hooks do not rewrite submitted values | The official website template's `formatSlug` rewrites `slug`; `email` is normalised | Echo-diff splits into `input_silently_dropped` (path **absent**) and `value_normalized_by_server` (path **present, different**); `hook_mutated` is observation-only; profile `echo_check_ignore` is the manual answer | §10.2 |
| A17 | Custom endpoints do not shadow built-in routes | `payload-preferences` registers `GET /:key`, which shadows `/versions` and returns a 200 | Every probe validates the **response shape**, never the status alone; the `docs`-is-an-array check on `/versions` is mandatory; `CUSTOM_ENDPOINTS_NOT_ENUMERABLE` declares the gap; `pay raw` reaches anything unmodelled | §7.5, §9.8 |
| A18 | The project has ~50 collections | A large install has 150+ with 500+ fields | Index + lazy field shards; index decode 1.44 ms at 150 and 4.79 ms at 500; `pay explain` never truncates the slug list and pages detail with `--offset`/`--limit`/`--grep`; a CI benchmark holds warm-path resolution under 5 ms at 150 × 200 | §8.2, §18 |
| A19 | `endpoints` are enabled on every collection | `payload-migrations` returns 501 `Cannot GET` | The 501 guard runs **first** and skips every other probe for that collection; the entity is recorded in `unreachable[]` with `reason: "endpoints-disabled"` and remains reachable through `pay raw` | §7.5 |
| A20 | `/api/access` is a complete inventory | It omits `payload-kv` (zero permissions); GraphQL omits `payload-migrations` | The inventory is the **union** of both sources, with `reachability` recording which one saw each entity | §7.3 |
| A21 | The network and server are reliable | — | Classified retries with decorrelated jitter, a shared retry budget, a wall-clock deadline that outranks the attempt count, and **never** a retried write once a response byte has been read | §6.1 |
| A22 | A write's echo shows what was stored | On Postgres, a nested block restructure leaves orphan rows visible only in the stored row; the echo and the draft read are clean | The stored row is read back after overwriting writes of versioned entities (`orphan_rows`); dry runs and backups predict (`orphan_risk`); `get` warns `empty_rows`; `--reset-nested` removes them keeping the id | §10.2.1 |
| A23 | A plain save leaves the publication state alone | Update-by-id merges into the pending draft and stores its `_status: "draft"` in the live row; a body `_status: "published"` publishes even with `?draft=true` | The write is refused until `--draft`/`--publish`/`--unpublish` states the outcome; `--draft` beats a publishing body; `pay publish --where` sends `?draft=true` | §10.2.2, §10.2.3 |
| A24 | Rich text round-trips through text | Payload stores a string in a richText field and an editor state in a text field as JSON text | `--richtext` renderings are refused on every write path; `--set-md` checks the target field's type (schema, then stored values); `stored_as_string` after the fact | §9.12.8, §10.2 |
| A25 | Drafts can be viewed without publishing | Only through the site's preview route with its secret | `pay url --preview`: the handshake without redirect, cookies saved 0600 in the agent-browser shape; secret from §5.5, never printed; credentials never cross an origin | §9.16, §6.3 |

### 21.3 What PayCLI never does

Stated as prohibitions because each one was a real defect in an earlier draft of this document:

1. **Never defaults a discovered fact.** `unknown` / `null` is the honest value; a defaulted `id_type`
   or capability flag makes PayCLI the author of a silent wrong answer.
2. **Never rejects client-side on a fact it does not know.** Every §9.3 row is gated on provenance.
3. **Never classifies on a translated string.** Only `errors[].name`, JSON structure, HTTP status, and
   the two hardcoded English strings.
4. **Never emits a hint naming a command that cannot answer it.** Unit-tested over every command line the shipped code names (`TestEveryHintIsAnswerable`).
5. **Never prints a credential.** Not in `error.raw`, not in `--output raw`, not in a debug log, not in
   a URL, not in a golden file. Repo-wide test.
6. **Never retries a write after a response byte has been read** — including on the Level-3
   reactive-invalidation path and the `jwt` re-login path.
7. **Never fails a command because of a cache problem.** Every cache error is a miss plus a warning.
8. **Never requires a credential to run.** Anonymous is a supported mode, not an error state.
9. **Never blocks a write on an observation.** Census facts (`observed-census`) warn; only declared
   facts and Payload's wire format produce errors (§7.10c.4).
10. **Never takes a published document offline without `--unpublish`**, and never lets a body's
    `_status` override the `--draft` the caller typed (§10.2.2, §10.2.3).
11. **Never writes rendered rich text back** (`richtext_rendered`, §9.12.8.3).
12. **Never writes without the backup the user asked for** — with a backup directory configured, a
    backup that cannot be taken aborts the write (§12.8.5).
