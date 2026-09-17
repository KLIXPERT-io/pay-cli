# PayCLI — verified grounding facts (probed live, 2026-09-16)

## Decisions (from product owner)
- Language: **Go + Cobra**, mirroring `KLIXPERT-io/gsc-cli` conventions.
- Binary name: **`pay`**
- Repo: **KLIXPERT-io/pay-cli**, **public**.
- Module path: `github.com/KLIXPERT-io/pay-cli`
- Auth: **Payload API keys**, **multi-profile**.
- Primary consumer: **LLM agents**. JSON-first, stable envelope, machine-readable error codes.

## Live test target
- Payload **3.86.0**, Next 16.2.6, Postgres. Base URL `http://localhost:3900`, API at `/api`.
- Repo: `/home/flo/payload-dummy` (dev server running in tmux session `payload-dummy`, log `/tmp/payload-dummy.log`).
- Auth header (case-sensitive): `Authorization: users API-Key <LOCAL_DEV_API_KEY>`
  - Format is `Authorization: {authCollectionSlug} API-Key {key}` — the collection slug is REQUIRED and varies per project.
- Test user: `paycli-test` (id 66), `useAPIKey: true` was enabled on the `users` collection.
- Re-mint key: `cd /home/flo/payload-dummy && ./node_modules/.bin/tsx scripts/paycli-mint-key.ts`

## Verified discovery surface

### 1. `GET /api/access` — PRIMARY discovery source (works with API key)
Returns `{ canAccessAdmin, collections: {...}, globals: {...} }`.
Per collection: `{ fields, create, read, update, delete, readVersions? }`.
`readVersions` present => that collection has versions/drafts enabled.
Live result: 49 collections, 5 globals.
Collections include: pages, posts, media, categories, users, forms, form-submissions,
redirects, search, 30+ `crm-*` (plugin-provided), and Payload internals:
payload-folders, payload-jobs, payload-locked-documents, payload-migrations, payload-preferences.
Globals: header, footer, crm-brand, crm-features, payload-jobs-stats.

### 2. `POST /api/graphql` introspection — FIELD SCHEMA source
Works with the same API key. 17,361 types in this project, so **never** dump the whole schema;
use targeted `__type(name:"X")` queries and cache per-type.
- `__type(name:"Page"){ fields { name type { name kind ofType { name kind } } } }` => field names + types.
- `__type(name:"Page_where"){ inputFields { name type { name } } }` => queryable paths,
  nested paths use `__` separator (e.g. `hero__links__link__url`), plus `AND`/`OR`.
- Type naming: collection slug -> singular PascalCase (`pages`->`Page`, `crm-contacts`->`CrmContact` — VERIFY per project, do not assume).
- **GraphQL can be disabled** per project (`graphQL: { disable: true }`) — discovery MUST degrade gracefully.

### 3. Probing fallback
`GET /api/{slug}?limit=1&depth=0` returns a doc whose keys reveal fields.
List response envelope: `{ docs, totalDocs, limit, totalPages, page, pagingCounter, hasPrevPage, hasNextPage, prevPage, nextPage }`.

## Verified REST surface (Payload 3.x)
```
GET    /api/{collection}                 find (where/sort/limit/page/depth/select/populate/locale/draft/trash)
GET    /api/{collection}/{id}            findByID
GET    /api/{collection}/count           count
POST   /api/{collection}                 create   (multipart for upload collections)
PATCH  /api/{collection}/{id}            update by id
PATCH  /api/{collection}?where=...       bulk update
DELETE /api/{collection}/{id}            delete by id
DELETE /api/{collection}?where=...       bulk delete
GET    /api/globals/{slug}               get global
POST   /api/globals/{slug}               update global
GET    /api/{collection}/versions        find versions
GET    /api/{collection}/versions/{id}   version by id
POST   /api/{collection}/versions/{id}   restore version
GET    /api/globals/{slug}/versions      global versions
GET    /api/access                       permissions
GET    /api/{authCollection}/me          current identity
```
Query string style is **qs / bracket notation**: `where[title][equals]=Foo`,
`where[or][0][and][0][x][equals]=y`, `sort=-createdAt`, `select[title]=true`, `populate[...]`.
`trash=true` includes soft-deleted docs (Payload 3.x trash feature).

## Gotchas confirmed / to handle
- Unauthenticated `GET /api/pages` returned **200** in this project (public read access). So a 200 does
  not prove auth works — verify auth via `/api/{authCollection}/me` returning a user.
- `/api/access` unauthenticated still returns 200 with a reduced permission set — compare against
  authenticated result rather than relying on status code.
- Auth collection slug is project-specific (`users` here, could be `admins`, `api-keys`, ...).
  Discovery must find auth-enabled collections rather than hardcoding `users`.
- Payload's own internal collections (payload-*) should be de-emphasized in default listings but reachable.
