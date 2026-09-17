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
| 15 | Output format auto-switching to `table` on a TTY (gsc, commands, gopkg) | **Never.** `json` always, unless `--output` / `PAY_OUTPUT` / `defaults.output` says otherwise; `PAY_HUMAN=1` opts into TTY switching | Agent harnesses frequently allocate a PTY, so TTY-dependent output makes the same command emit different bytes in different harnesses. |
| 16 | Secret storage: OS keychain with silent file fallback (gsc) vs file-first with opt-in keychain (gopkg) | **File-first** (`credentials.json`, 0600); keychain only when explicitly requested | Re-verified on this machine: `go-keyring` fails even with a live D-Bus session bus (`org.freedesktop.secrets was not provided by any .service files`), and a silent fallback means the secret lands in a different place depending on invisible environment state. |
| 17 | Config dir on macOS: `os.UserConfigDir()` (gsc) vs `~/.config/pay` everywhere (gopkg) | **`~/.config/pay` on both Linux and macOS** | gsc's split between `DataDir()` and `Path()` puts config and cache in different trees on macOS; one guessable path is worth more to an agent than platform idiom. |
| 18 | Skills: shell out to `npx skills add` (agentux considered it) vs native Go + `go:embed` | **Native, embedded, offline**; a top-level `skills/` directory is **not** created | `go:embed` cannot reach a parent directory, so a second copy would drift from the binary it documents, and the whole point is that the skill describes the installed binary. |
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
├── install.sh  install.ps1
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
└── internal/
    ├── buildinfo/      buildinfo.go  buildinfo_test.go
    ├── apierr/         code.go  error.go  exit.go  payload.go  diagnose.go  *_test.go
    ├── redact/         redact.go  url.go  headers.go  redact_test.go
    ├── config/         paths.go  file.go  profile.go  resolve.go  project.go  interpolate.go
    │                   projectscan.go    # reads package.json + scans block slugs (§7.10) — local FS only
    │                   *_test.go
    ├── secret/         store.go  file.go  env.go  keyring.go  helper.go  chain.go  fingerprint.go
    │                   jwt.go            # JWT credential record: {token, exp}; never the password
    │                   *_test.go
    ├── payload/        client.go  transport.go  retry.go  errors.go  find.go  mutate.go  globals.go
    │                   versions.go  upload.go  auth.go  login.go  graphql.go  override.go
    │                   reactive.go       # WithReactiveInvalidation(ctx) — §8.4 Level-3 opt-in
    │                   *_test.go
    │   └── query/      json.go  brackets.go  dsl.go  operators.go  validate.go  *_test.go
    ├── discovery/      manifest.go  index.go  shard.go  authboot.go  stage0.go  stage1.go  stage2.go
    │                   stage3.go  degrade.go  fieldkind.go  labels.go  idtype.go  locales.go
    │                   blocks.go  provenance.go  fingerprint.go  *_test.go
    ├── cache/          store.go  key.go  scope.go  ttl.go  gc.go  generation.go
    │                   lock_unix.go  lock_windows.go  *_test.go
    ├── output/         envelope.go  json.go  jsonl.go  table.go  csv.go  id.go  format.go  writer.go  *_test.go
    ├── logging/        logging.go  logging_test.go
    ├── audit/          audit.go  rotate.go  audit_test.go
    ├── safety/         risk.go  confirm.go  dryrun.go  blastradius.go  *_test.go
    ├── skills/         embed.go  install.go  manifest.go  project.go  *_test.go
    │   └── assets/pay/ SKILL.md
    │                   references/{query-syntax,errors,recipes,gotchas}.md
    ├── update/         update.go  state.go  detach.go  managed.go  verify.go
    │                   lock_unix.go  lock_windows.go  platform_unix.go  platform_windows.go  *_test.go
    ├── payloadtest/    server.go  fixture.go  golden.go  clock.go      # imported only by _test.go
    └── cli/            app.go  root.go  help.go  complete.go
                        auth.go  profile.go  config.go  whoami.go  access.go  can.go  doctor.go
                        discover.go  explain.go  collections.go  describe.go
                        find.go  get.go  count.go  create.go  update.go  delete.go  restore.go
                        duplicate.go  publish.go
                        upload.go  download.go  globals.go  versions.go
                        raw.go  cache.go  skills.go  selfupdate.go  completion.go  version.go  audit.go
                        *_test.go
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
`v1/<scope>/…` in cache dir; `update-state.json` in state dir. All directories 0700.

`rm -rf $(pay cache path)` must never break the CLI, only slow it down.
`pay config paths --output json` prints all of them so an agent never guesses.

### 4.2 `~/.config/pay/config.toml`

```toml
version = 1
default_profile = "local"

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

### 4.6 Environment variables

```
PAY_PROFILE  PAY_BASE_URL  PAY_API_PATH  PAY_GRAPHQL_PATH  PAY_GRAPHQL_ROUTE  PAY_AUTH_COLLECTION
PAY_AUTH_MODE                # auto|api-key|jwt|anonymous
PAY_API_KEY  PAY_API_KEY_<PROFILE_UPPER_SNAKE>  PAY_JWT  PAY_JWT_<PROFILE_UPPER_SNAKE>
PAY_HOME  PAY_CONFIG  PAY_CONFIG_DIR  PAY_CACHE_DIR  PAY_STATE_DIR
PAY_OUTPUT  PAY_HUMAN  PAY_ERRORS_TO  PAY_NO_COLOR  NO_COLOR
PAY_DEPTH  PAY_LIMIT  PAY_MAX_DOCS  PAY_TIMEOUT  PAY_DEADLINE  PAY_MAX_RETRIES  PAY_CONCURRENCY
PAY_NO_CACHE  PAY_CACHE_TTL  PAY_REFRESH
PAY_LOG_LEVEL  PAY_LOG_FORMAT  PAY_VERBOSE  PAY_QUIET
PAY_NO_REDACT  PAY_INSECURE_SKIP_VERIFY  PAY_KEYRING (auto|off|force)
PAY_NO_AUDIT  PAY_NO_UPDATE  PAY_NO_UPDATE_NOTICE  PAY_UPDATE_URL  PAY_UPDATE_STRICT
PAY_YES                     # equivalent to --yes; for non-interactive automation
PAY_TEST_BASE_URL  PAY_TEST_API_KEY   # live tests only
```

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
  output, logs, audit records and cache writes. Disabled only by `--no-redact`.
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
production build with `graphQL.disableIntrospectionInProduction: true`, a documented hardening
option — it returns a happy 200 and PayCLI would record `mode: "ok"`, then watch Stages 1 and 2 both
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
| `BLOCK_SLUGS_UNKNOWN` | the GraphQL union names are `interfaceName`s, not `blockType` slugs | resolve from project source (§7.10), sample documents, or the profile's `blocks` map |
| `CUSTOM_ENDPOINTS_NOT_ENUMERABLE` | `config.endpoints` is not introspectable | profile `custom_endpoints`, or `pay raw` |
| `SORTABILITY_HEURISTIC` | sortability inferred, not measured | `pay discover --deep --verify-sort` runs the asc/desc differential probe |
| `LOCALIZATION_UNKNOWN` | whether the project is localised at all is not detectable over REST with certainty (`locale` is accepted and ignored when it is not) | `?locale=all` differential heuristic (§7.9b); until then no locale parameters are sent |
| `RICHTEXT_SHAPE_UNKNOWN` | lexical maps to the `JSON` scalar | `pay describe <c> --field <f> --sample` prints a real document's value |
| `ID_TYPE_UNKNOWN` | no GraphQL, zero documents, no versions row, no profile pin | pin `id_type` in the profile; until then ids are passed through unchecked |
| `CAPABILITY_UNKNOWN` | one or more `flags.*` are `null` (GraphQL-only signals with no REST probe) | the operation is attempted and the server's answer is classified after the fact |
| `LABELS_UNAVAILABLE` | probe 8's message did not match the English pattern (translated or custom) | `labels.source: "derived"` from the slug; nothing else depends on labels |
| `LOCALES_UNKNOWN` | localisation is enabled but the true locale codes could not be enumerated (§7.9) | `--locale` is passed through unvalidated with `locale_unverified`; pin `locales` in the profile |
| `LOCALIZATION_PER_FIELD_UNKNOWN` | per-field `localized` is not introspectable over REST **or** GraphQL | `fields[].localized: null`; §10.2's echo-diff skips those fields |
| `BLOCK_SLUGS_UNKNOWN_NO_SOURCE` | block slugs unresolvable from the API **and** no project source found (§7.10) | the collection is marked `publishable: false` with that reason |
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
   mutable by definition, and stale content served to an agent causes wrong edits.
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
pay describe <collection|global> [--field PATH] [--required-only] [--queryable] [--examples] [--sample]
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
pay get   <collection> <id>   --depth --select --populate --draft --trash --locale
pay count <collection>        --where/--or/--where-json --trash        (NOTE: --draft is a no-op)
# --data / --data-file / --set / --set-json are COMBINABLE and deep-merged, later wins. Value
# typing, relationship-id coercion and the full precedence order are §9.10 — identical for
# create, update, upload and globals update.
pay create <collection>
    [--data JSON] [--data-file F] [--data @-] [--set k=v]... [--set-json k=JSON]...
    --draft --autosave --depth --select --file PATH --no-echo-check --dry-run --yes
pay update <collection> <id>            (same data flags)
    --draft --autosave --publish --unpublish --trash --override-lock
    --depth --select --no-echo-check --dry-run --yes
pay update <collection> --where ... [--max-docs N | --all] [--per-doc] --dry-run --yes
pay delete <collection> <id>            --permanent --override-lock --dry-run --yes
pay delete <collection> --where ... [--max-docs N | --all] --permanent
                                        --unsafe-passthrough-where --dry-run --yes
pay restore   <collection> <id>                       # un-trash
pay duplicate <collection> <id> [--no-draft]
pay publish|unpublish <collection> <id> | --where ... [--max-docs N | --all]

pay upload   <collection> <file|-|URL> [--alt TEXT] [--set k=v]... [--data JSON]
             [--filename NAME] [--replace ID] [--content-type MIME] [--max-size 100MB] [--allow-remote]
pay download <collection> <id> | --filename NAME [-o PATH|-] [--size thumbnail]

pay globals list
pay globals get    <slug> [--draft] [--depth] [--select] [--locale]
pay globals update <slug> (--data|--data-file|--set) [--draft] [--dry-run] [--yes]

pay versions list    <collection>|--global SLUG [--id DOC] [--autosave] [--latest]
                     [--limit] [--page] [--sort]
pay versions get     <collection>|--global SLUG <versionId> [--depth]
pay versions restore <collection>|--global SLUG <versionId> [--draft] [--dry-run] [--yes]
pay versions diff    <collection> <vA> <vB>          # client-side JSON diff

pay raw <GET|POST|PATCH|DELETE> <path> [--query k=v]... [--data JSON] [--file F] [--raw-body]
pay config get|set|unset|list|paths|explain
pay skills install [--global] [--dir PATH] [--agent a,b|all] [--force] [--with-project-context]
pay skills list | status | print [NAME] | update [--project-only] | uninstall
pay completion bash|zsh|fish|powershell
pay version [--check]
pay update-self [--check] [--apply] [--force]
pay audit tail [-n N] [--since D] [--action delete]
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

`--no-validate-sort` / `--no-validate-where` bypass the check for escape-hatch cases.

**The tri-state rule governs this entire table** (§7.6). Every row above is gated on the manifest
actually *knowing* the fact. When the relevant value is `"unknown"` or `null`, PayCLI **sends the
request** and attaches the matching `*_unknown` warning, rather than rejecting locally. A client-side
rejection built on a fact PayCLI never learned is a fabricated error, and it is worse than the
server's own bad message because it is confidently wrong: `invalid_id` on a valid 24-hex ObjectId says
the id is malformed when the truth is that PayCLI could not determine the id type.

A unit test loads `testdata/fixtures/manifest_id_unknown.json` (`id_type: "unknown"`) and asserts that
`pay get <coll> 66f1a2b3c4d5e6f708192a3b` **issues an HTTP request** rather than exiting 5.

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
   escape.

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

When the value being written cannot be resolved, PayCLI emits an `unknown_block_type` **warning**
before sending and an `input_silently_dropped` warning after (§10.2). When the collection's *required*
paths include an unresolvable blocks field, the collection is `publishable: false` and
`pay publish` refuses locally (§9.10.4) rather than relaying a 400 the agent cannot act on.

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

### 9.10 Write input semantics

Referenced identically from `create`, `update`, `upload` and `globals update`. The original spec
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
```

Arrays are **replaced**, never merged element-wise. The `|` in §9.2's `create`/`update` grammar that
implied mutual exclusion is removed. `pay upload` uses the identical order with `--alt` slotted in as
the lowest-precedence contributor (`--alt` → `--data`/`--data-file` → `--set` → `--set-json`), which
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
knowable from the API.

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
* `page` is present iff `data_kind == "doc_list"`. `truncated` is one boolean requiring no arithmetic.
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
| 1 | generic / internal | no | `internal`, `unknown`, `cache_corrupt`, `update_verification_failed`, `audit_write_failed` |
| 2 | auth — *your credentials* | no | `auth_missing`, `auth_invalid`, `auth_required`, `auth_locked`, `auth_unverified_email`, `auth_insecure_permissions`, `auth_helper_failed` |
| 3 | throttled / temporarily unavailable | **yes** | `rate_limited` (429), `doc_locked` (423), `server_busy` (503 with `Retry-After`) |
| 4 | not found | no | `doc_not_found`, `route_not_found`, `version_not_found` |
| 5 | validation / bad input | no | `validation_failed`, `query_path_invalid`, `invalid_args`, `invalid_where_syntax`, `invalid_sort_field`, `unknown_field`, `invalid_option`, `invalid_id`, `bad_request_body`, `where_required`, `file_missing`, `not_upload_collection`, `unsupported_operator`, `bulk_limit_exceeded`, `request_too_large`, `format_unsupported`, `invalid_path_expr` |
| 6 | network / server | **yes**, except 500 | `network_unreachable`, `dns_failure`, `tls_error`, `timeout`, `server_error` (500, **not retried**), `server_unavailable` (502/504), `non_json_response` |
| 7 | **partial failure** | **never** | `partial_failure` |
| 8 | **access denied** — identity valid, permission not | no | `access_denied`, `admin_access_denied` |
| 9 | **config / profile** | no | `config_missing`, `config_secret_in_plaintext`, `profile_unknown`, `base_url_invalid`, `endpoint_not_payload`, `auth_collection_unknown` |
| 10 | **capability / discovery** | no | `collection_unknown`, `global_unknown`, `feature_unavailable`, `operation_unsupported` (501), `discovery_failed`, `graphql_disabled`, `schema_stale` |
| 11 | **confirmation required** | no | `confirmation_required` |

Never uses 125–128 or 130 (shell/signal territory). A unit test asserts the `Code → exit` map is
**total** — gsc-cli's `default:` branch silently returns 1 for any new code.

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
| **L0** read | `find` `get` `count` `versions list/get/diff` `describe` `collections` `explain` `access` `can` `whoami` `download` `discover` `doctor` `cache *` | never prompts |
| **L1** scoped write | `create`, `update <id>`, `upload`, `duplicate`, `publish <id>` | no prompt by default (single doc, recoverable via versions where enabled); `defaults.confirm_writes = true` opts in |
| **L2** scoped destructive | `delete <id> --permanent`, `versions restore`, `globals update`, `unpublish <id>` | prompts on a TTY; **requires `--yes` in non-TTY** or exits 11 |
| **L3** bulk | `update --where`, `delete --where`, `publish/unpublish --where` | **always** requires `--yes` in non-TTY, always prints the resolved match count before acting, and is capped by `--max-docs` (default `defaults.max_bulk` = 100) unless `--all` is passed. `--limit` on these verbs is `invalid_args` (exit 5) |

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
    Time       time.Time       `json:"time"`
    Command    string          `json:"command"`
    Profile    string          `json:"profile"`
    BaseURL    string          `json:"base_url"`
    Collection string          `json:"collection,omitempty"`
    Global     string          `json:"global,omitempty"`
    Action     string          `json:"action"`   // create|update|delete|trash|restore|publish|upload|restore_version
    Method     string          `json:"method"`
    Path       string          `json:"path"`
    Where      json.RawMessage `json:"where,omitempty"`
    IDs        []string        `json:"ids,omitempty"`        // capped at 200 + IDsTruncated
    Affected   int             `json:"affected"`
    Failed     int             `json:"failed,omitempty"`
    DryRun     bool            `json:"dry_run,omitempty"`
    Status     int             `json:"http_status"`
    OK         bool            `json:"ok"`
    Err        string          `json:"error,omitempty"`
    DurationMS int64           `json:"duration_ms"`
    RequestID  string          `json:"request_id"`
}
```

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

---

## 13. Uploads

`pay upload <collection> <file|-|URL>` builds `multipart/form-data` with exactly:

* part **`file`** — the bytes. The name must be literally `file` (verified: `-F upload=@…` → 400
  `"No files were uploaded."`). The part's `Content-Type` is sniffed or taken from `--content-type`.
* part **`_payload`** — a JSON string of all sibling fields, present only if any were supplied. Built
  by the **§9.10.2 merge**, deep-merged with later winning:
  `--alt` → `--data`/`--data-file` → `--set` (typed + coerced per §9.10.1) → `--set-json`.
  So `--set alt=B --alt A` yields `alt: "B"`. The flags are **combinable**, not alternatives.

Go: `multipart.Writer`, `CreatePart` with an explicit
`Content-Disposition: form-data; name="file"; filename="…"` plus `Content-Type`, `io.Copy` the file
(never buffer it whole), and `WriteField("_payload", string(jsonBytes))`.

Behaviours surfaced in help and in `warnings[]`:

* **Filename collisions are auto-renamed server-side** (`paycli-probe.png` → `paycli-probe-1.png`).
  PayCLI echoes the returned filename and emits a `filename_changed` warning when it differs, because
  an agent will otherwise assume its own name.
* `--replace <id>` does a `PATCH /{coll}/{id}` with the same multipart body. This also **mints a new
  filename** rather than overwriting in place, so any hardcoded URL to the old file breaks. Stated in
  the flag help.
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

The canonical source lives **only** at `internal/skills/assets/pay/`, compiled in with
`//go:embed all:assets`. No top-level `skills/` directory is created — `go:embed` cannot reach a
parent directory, so a second copy would inevitably drift from the binary it documents, and
`pay skills print` makes it redundant anyway.

```
internal/skills/assets/pay/
├── SKILL.md                      static, hand-written, git-reviewed
└── references/
    ├── query-syntax.md
    ├── errors.md                 the taxonomy, exit-code table, and the six body shapes
    ├── recipes.md                paginate, bulk safely, upload, versions, drafts
    └── gotchas.md
```

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
`{"version":…,"path":…}` when `PAY_INSTALL_JSON=1`.

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
| `TestScopeKeyTable` | scope equality/inequality over header set, `graphql_path`, `api_path`, key, profile name, and `auto` vs explicit `auth_collection` (§8.1) |
| `TestFingerprintLengthIsSixteen` | every producer of a key fingerprint emits the identical 16 hex chars (§4.4) |
| `TestIDTypeUnknownIssuesRequest` | `manifest_id_unknown.json` + `pay get c 66f1a2b3…` makes an HTTP request, does not exit 5 (§9.3) |
| `TestGermanValidationError` | `error_validation_de.json` still normalises to `validation_failed` with per-field paths (§11.2) |
| `TestNoSecretInAnyOutput` | repo-wide grep of golden `.out`/`.err`, audit lines and log sinks for the fixture key (§3.1) |
| `TestDiscoveryNeverSetsReactiveFlag` | a full discovery run issues zero `WithReactiveInvalidation` requests (§8.4a) |
| `TestBulkUpdateSendsNoLimit` | `pay update c --where x --max-docs 2` emits no `limit=` parameter (§12.3) |
| `TestUpdateSelfAbortsOnBadChecksum` | wrong checksum ⇒ no swap, non-zero exit, `PAY_UPDATE_STRICT` unset (§15.3) |
| `TestEveryHintIsAnswerable` | every `hint` containing a `pay …` command produces a non-empty answer against the fixture manifest (§7.10) |
| `TestManifestFieldKeySetIsTotal` | every field entry has exactly the §7.8.2 key set, nulls included |
| `BenchmarkWarmPath` | index + one shard resolution stays under **5 ms** for a synthetic 150 × 200 manifest; CI-enforced (§8.2) |

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
| A4 | GraphQL is reachable and introspectable | `graphQL.disable: true`; `disableIntrospectionInProduction` (fires only on a literal `__schema`/`__type` field, only under `NODE_ENV=production`); custom `validationRules`; `maxComplexity` | Mode probe **contains `__type`** so the guard actually fires. Any 200 with non-empty `errors[]` or a null expected alias is a **degrade trigger**, not a failure: record the mode, append `diagnostics.degraded[]`, fall through to REST-only, emit a usable manifest. `discovery_failed` (exit 10) only if REST-only *also* fails | §7.6 |
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
| A15 | Block `blockType` slugs are recoverable from the API | They are not when `interfaceName` is set, and the observed-values fallback yields nothing when every `layout` is `[]` | Profile `blocks` map → project-source scan (`slug:` literals near `fields:`, bounded, never evaluated) → observed values → `blocks_source: "unknown"`, collection `publishable: false` with a plain-words reason and the file to read. **No hint may name a command that cannot answer it** — unit-tested | §7.10 |
| A16 | Hooks do not rewrite submitted values | The official website template's `formatSlug` rewrites `slug`; `email` is normalised | Echo-diff splits into `input_silently_dropped` (path **absent**) and `value_normalized_by_server` (path **present, different**); `hook_mutated` is observation-only; profile `echo_check_ignore` is the manual answer | §10.2 |
| A17 | Custom endpoints do not shadow built-in routes | `payload-preferences` registers `GET /:key`, which shadows `/versions` and returns a 200 | Every probe validates the **response shape**, never the status alone; the `docs`-is-an-array check on `/versions` is mandatory; `CUSTOM_ENDPOINTS_NOT_ENUMERABLE` declares the gap; `pay raw` reaches anything unmodelled | §7.5, §9.8 |
| A18 | The project has ~50 collections | A large install has 150+ with 500+ fields | Index + lazy field shards; index decode 1.44 ms at 150 and 4.79 ms at 500; `pay explain` never truncates the slug list and pages detail with `--offset`/`--limit`/`--grep`; a CI benchmark holds warm-path resolution under 5 ms at 150 × 200 | §8.2, §18 |
| A19 | `endpoints` are enabled on every collection | `payload-migrations` returns 501 `Cannot GET` | The 501 guard runs **first** and skips every other probe for that collection; the entity is recorded in `unreachable[]` with `reason: "endpoints-disabled"` and remains reachable through `pay raw` | §7.5 |
| A20 | `/api/access` is a complete inventory | It omits `payload-kv` (zero permissions); GraphQL omits `payload-migrations` | The inventory is the **union** of both sources, with `reachability` recording which one saw each entity | §7.3 |
| A21 | The network and server are reliable | — | Classified retries with decorrelated jitter, a shared retry budget, a wall-clock deadline that outranks the attempt count, and **never** a retried write once a response byte has been read | §6.1 |

### 21.3 What PayCLI never does

Stated as prohibitions because each one was a real defect in an earlier draft of this document:

1. **Never defaults a discovered fact.** `unknown` / `null` is the honest value; a defaulted `id_type`
   or capability flag makes PayCLI the author of a silent wrong answer.
2. **Never rejects client-side on a fact it does not know.** Every §9.3 row is gated on provenance.
3. **Never classifies on a translated string.** Only `errors[].name`, JSON structure, HTTP status, and
   the two hardcoded English strings.
4. **Never emits a hint naming a command that cannot answer it.** Unit-tested over every hint.
5. **Never prints a credential.** Not in `error.raw`, not in `--output raw`, not in a debug log, not in
   a URL, not in a golden file. Repo-wide test.
6. **Never retries a write after a response byte has been read** — including on the Level-3
   reactive-invalidation path and the `jwt` re-login path.
7. **Never fails a command because of a cache problem.** Every cache error is a miss plus a warning.
8. **Never requires a credential to run.** Anonymous is a supported mode, not an error state.
