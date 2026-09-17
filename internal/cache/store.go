package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/buildinfo"
	"github.com/KLIXPERT-io/pay-cli/internal/fsatomic"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

// ManifestVersion is the §7.8.1 layout version this build writes and reads.
// A file carrying any other value is a cache miss, not a parse error.
const ManifestVersion = 2

// DefaultLockTimeout is §8.7's 5 s flock acquisition timeout. On timeout the
// write is skipped and the command continues: a cache failure must never fail a
// command, and a stale lock from a crashed process must never wedge the CLI.
const DefaultLockTimeout = 5 * time.Second

// ErrLockTimeout is returned by the locker when the acquisition budget expires.
var ErrLockTimeout = errors.New("cache: lock acquisition timed out")

// Store is the on-disk cache. A Store with an empty root is a valid, permanently
// disabled store: every read is a miss and every write is a silent no-op, which
// is exactly what `--no-cache` needs.
type Store struct {
	root string

	// LockTimeout overrides DefaultLockTimeout (tests use a shorter one).
	LockTimeout time.Duration
	// GCDivisor is the 1/N write-path GC probability from §8.7 (default 50).
	GCDivisor int
	// DisableGC turns the opportunistic sweep off entirely.
	DisableGC bool
}

// New returns a Store rooted at the cache directory ($CACHE from §4.1).
func New(root string) *Store { return &Store{root: root} }

// Enabled reports whether this store may touch the filesystem.
func (s *Store) Enabled() bool { return s != nil && s.root != "" }

func (s *Store) lockTimeout() time.Duration {
	if s == nil || s.LockTimeout <= 0 {
		return DefaultLockTimeout
	}
	return s.LockTimeout
}

// ---------------------------------------------------------------------------
// Manifest (§7.8.1) — the index.
//
// This package deliberately decodes only the *header* fields it must verify:
// the layout version, the generation, the identity block and the per-entity
// shard references. internal/discovery owns the full typed manifest; duplicating
// it here would make the cache a second source of truth for the schema, and
// would force this package to be updated for every field discovery adds.
// ---------------------------------------------------------------------------

// Manifest is the verified index. Raw is the exact bytes read from disk, which
// is what discovery decodes into its own richer struct.
type Manifest struct {
	Raw  []byte `json:"-"`
	Path string `json:"-"`

	ManifestVersion int                 `json:"manifest_version"`
	Generation      string              `json:"generation"`
	CLIVersion      string              `json:"cli_version"`
	GeneratedAt     time.Time           `json:"generated_at"`
	ExpiresAt       time.Time           `json:"expires_at"`
	Meta            ManifestMeta        `json:"meta"`
	Fingerprint     ManifestFingerprint `json:"fingerprint"`
	Collections     []EntityRef         `json:"collections"`
	Globals         []EntityRef         `json:"globals"`
}

// ManifestMeta is manifest.json -> meta. Header *values* are never present:
// only the sorted names, so `pay cache ls` can explain why two profiles do or
// do not share a scope (§8.1).
type ManifestMeta struct {
	Scope          string    `json:"scope"`
	Profiles       []string  `json:"profiles"`
	BaseURL        string    `json:"base_url"`
	APIPath        string    `json:"api_path"`
	GraphQLPath    string    `json:"graphql_path"`
	HeaderNames    []string  `json:"header_names"`
	KeyFingerprint string    `json:"key_fingerprint"`
	CreatedAt      time.Time `json:"created_at"`
	ConfirmedAt    time.Time `json:"confirmed_at"`
}

// ManifestFingerprint is manifest.json -> fingerprint (§8.4 levels 1 and 2).
type ManifestFingerprint struct {
	TopologySHA256 string    `json:"topology_sha256"`
	SchemaSHA256   string    `json:"schema_sha256"`
	CheckedAt      time.Time `json:"checked_at"`
	ServerIdentity string    `json:"server_identity"`
}

// EntityRef is the subset of a collection/global entry the cache must verify:
// which shard holds its fields and what that shard must hash to.
type EntityRef struct {
	Slug         string `json:"slug"`
	FieldsCount  int    `json:"fields_count"`
	FieldsSHA256 string `json:"fields_sha256"`
	FieldsShard  string `json:"fields_shard"`
}

// Entity finds a collection or global by slug.
func (m *Manifest) Entity(slug string, kind EntityKind) (EntityRef, bool) {
	if m == nil {
		return EntityRef{}, false
	}
	list := m.Collections
	if kind == KindGlobal {
		list = m.Globals
	}
	for _, e := range list {
		if e.Slug == slug {
			return e, true
		}
	}
	return EntityRef{}, false
}

// Age is the manifest's age against meta.confirmed_at, which §8.2 names as the
// single authoritative timestamp (created_at and generated_at record when the
// data was *produced*; confirmed_at records when it was last known good).
func (m *Manifest) Age(now time.Time) time.Duration {
	if m == nil {
		return 0
	}
	ref := m.Meta.ConfirmedAt
	if ref.IsZero() {
		ref = m.GeneratedAt
	}
	age := now.Sub(ref)
	if age < 0 {
		return 0
	}
	return age
}

// Freshness places the manifest on the §8.5 ladder.
func (m *Manifest) Freshness(now time.Time) (Freshness, time.Duration) {
	if m == nil {
		return FreshnessExpired, 0
	}
	ref := m.Meta.ConfirmedAt
	if ref.IsZero() {
		ref = m.GeneratedAt
	}
	return Evaluate(ClassDiscoveryManifest, ref, now)
}

// ReadManifest loads and verifies the index for a scope.
//
// It never returns an error. Per §8.3 every failure on the read path — ENOENT,
// EACCES, a short read, malformed JSON, an unknown manifest_version, a scope
// mismatch — is a miss, optionally carrying a cache_unreadable warning. A plain
// "not cached yet" is a miss with no warning: a cold start is not an anomaly.
func (s *Store) ReadManifest(sc Scope) (*Manifest, bool, *output.Warning) {
	if !s.Enabled() || !sc.Valid() {
		return nil, false, nil
	}
	path := s.ManifestPath(sc)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, readWarning(path, err)
	}
	m := &Manifest{Raw: data, Path: path}
	if err := json.Unmarshal(data, m); err != nil {
		return nil, false, warning(WarnCacheUnreadable, path+": "+err.Error())
	}
	if m.ManifestVersion != ManifestVersion {
		return nil, false, warning(WarnCacheUnreadable, path+": unsupported manifest_version "+
			strconv.Itoa(m.ManifestVersion)+" (this build reads "+strconv.Itoa(ManifestVersion)+")")
	}
	if !ValidGeneration(m.Generation) {
		return nil, false, warning(WarnCacheUnreadable, path+": malformed generation")
	}
	if m.Meta.Scope != sc.Key {
		// The index does not belong to the directory it was found in, so
		// something rewrote it out of band. Not deleted: a concurrent writer
		// publishing a new set must never be raced by a reader.
		return nil, false, warning(WarnCacheUnreadable, path+": scope mismatch")
	}
	return m, true, nil
}

// Shard is a verified §7.8.2 field shard.
type Shard struct {
	Raw  []byte `json:"-"`
	Path string `json:"-"`

	Generation string `json:"generation"`
	Slug       string `json:"slug"`
	SHA256     string `json:"sha256"`
}

// ReadShard loads one entity's field shard and verifies it against the index.
//
// A shard whose generation or sha256 does not match the index's
// fields_shard/fields_sha256 entry is a miss **for that entity only** (§8.2):
// PayCLI re-discovers rather than mixing two runs. The file is not deleted —
// it may well be a *newer* generation whose index has not been renamed into
// place yet, and deleting it would corrupt a concurrent writer's set.
func (s *Store) ReadShard(sc Scope, m *Manifest, slug string, kind EntityKind) (*Shard, bool, *output.Warning) {
	if !s.Enabled() || m == nil {
		return nil, false, nil
	}
	ref, ok := m.Entity(slug, kind)
	if !ok {
		return nil, false, nil
	}
	shardRef := ref.FieldsShard
	if shardRef == "" {
		shardRef = ShardName(slug, kind)
	}
	path, ok := s.shardPath(sc, shardRef)
	if !ok {
		return nil, false, warning(WarnCacheUnreadable, s.ScopeDir(sc)+": refusing unsafe shard path "+shardRef)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, readWarning(path, err)
	}
	sh := &Shard{Raw: data, Path: path}
	if err := json.Unmarshal(data, sh); err != nil {
		return nil, false, warning(WarnCacheUnreadable, path+": "+err.Error())
	}
	switch {
	case sh.Generation != m.Generation:
		return nil, false, warning(WarnCacheUnreadable, path+": generation mismatch (torn read)")
	case ref.FieldsSHA256 != "" && sh.SHA256 != ref.FieldsSHA256:
		return nil, false, warning(WarnCacheUnreadable, path+": sha256 mismatch (torn read)")
	case sh.Slug != "" && sh.Slug != slug:
		return nil, false, warning(WarnCacheUnreadable, path+": slug mismatch")
	}
	return sh, true, nil
}

// ---------------------------------------------------------------------------
// Entries (§8.3) — self-describing derived blobs, currently graphql/<Type>.json.
// ---------------------------------------------------------------------------

// Entry is §8.3's mandatory self-describing header plus the payload. Every
// field of the header is verified on read; a mismatch is a miss and the entry
// is deleted, because unlike a shard an entry is never part of an atomically
// published set.
type Entry struct {
	Scope        string          `json:"scope"`
	Generation   string          `json:"generation"`
	Method       string          `json:"method"`
	URL          string          `json:"url"`
	Status       int             `json:"status"`
	ContentType  string          `json:"content_type"`
	FetchedAt    time.Time       `json:"fetched_at"`
	TTL          int64           `json:"ttl"`
	CLIMajor     int             `json:"cli_major"`
	Class        Class           `json:"class"`
	SchemaSHA256 string          `json:"schema_sha256,omitempty"`
	Body         json.RawMessage `json:"body"`
}

// NewEntry builds an entry header. url is stored only after redact.URL (§8.3).
func NewEntry(sc Scope, generation string, class Class, req Request, resp Response, now time.Time) Entry {
	ttl, _ := TTLFor(class)
	return Entry{
		Scope:       sc.Key,
		Generation:  generation,
		Method:      req.Method,
		URL:         redactedURL(req.URL),
		Status:      resp.Status,
		ContentType: resp.ContentType,
		FetchedAt:   now.UTC().Truncate(time.Second),
		TTL:         int64(ttl.Fresh / time.Second),
		CLIMajor:    cliMajor(),
		Class:       class,
		Body:        json.RawMessage(resp.Body),
	}
}

// ReadGraphQLType returns the cached introspection blob for one GraphQL type.
// schemaSHA256 is the §8.2 key that prevents a graphql blob from disagreeing
// with the manifest that references it; pass "" to skip that check.
func (s *Store) ReadGraphQLType(sc Scope, typeName, schemaSHA256 string, now time.Time) (*Entry, bool, *output.Warning) {
	path, ok := s.graphQLPath(sc, typeName)
	if !s.Enabled() || !ok {
		return nil, false, nil
	}
	e, hit, warn := s.readEntry(path, sc, now)
	if !hit {
		return nil, false, warn
	}
	if schemaSHA256 != "" && e.SchemaSHA256 != schemaSHA256 {
		s.discard(path)
		return nil, false, warning(WarnCacheUnreadable, path+": schema_sha256 mismatch")
	}
	return e, true, nil
}

func (s *Store) readEntry(path string, sc Scope, now time.Time) (*Entry, bool, *output.Warning) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, readWarning(path, err)
	}
	var e Entry
	if err := json.Unmarshal(data, &e); err != nil {
		s.discard(path)
		return nil, false, warning(WarnCacheUnreadable, path+": "+err.Error())
	}
	if reason, ok := e.verify(sc); !ok {
		s.discard(path)
		return nil, false, warning(WarnCacheUnreadable, path+": "+reason)
	}
	if fresh, _ := Evaluate(e.Class, e.FetchedAt, now); fresh == FreshnessExpired {
		return nil, false, nil
	}
	return &e, true, nil
}

func (e Entry) verify(sc Scope) (string, bool) {
	switch {
	case e.Scope != sc.Key:
		return "scope mismatch", false
	case !ValidGeneration(e.Generation):
		return "malformed generation", false
	case e.Status != 200:
		return "cached status is not 200", false
	case !isJSONContentType(e.ContentType):
		return "cached content_type is not application/json", false
	case e.CLIMajor != cliMajor():
		return "written by a different CLI major", false
	case !e.Class.Persistable():
		return "class " + string(e.Class) + " may not be cached", false
	case len(e.Body) == 0:
		return "empty body", false
	}
	return "", true
}

// discard removes a provably invalid entry. Best effort: a failure here is
// itself a cache problem, and cache problems never fail commands.
func (s *Store) discard(path string) { _ = os.Remove(path) }

// ---------------------------------------------------------------------------
// §8.3 — the one predicate authorised to write to disk.
// ---------------------------------------------------------------------------

// Request is the request half of the Cacheable predicate.
type Request struct {
	Method string
	URL    string
	// Class is what the response carries. Call sites never decide whether to
	// cache; they only say what kind of thing they fetched.
	Class Class
	// IdempotencySafe is §6.1's predicate: GET, POST /api/graphql carrying a
	// read-only query, or POST with X-Payload-HTTP-Method-Override: GET.
	IdempotencySafe bool
	// Anonymous is true when the request carried no credential.
	Anonymous bool
	// ScopeKeyFingerprint is the scope the answer would be filed under.
	ScopeKeyFingerprint string
	// NoCache is --no-cache / PAY_NO_CACHE=1: discover in-process, write
	// nothing to disk (§8.5).
	NoCache bool
}

// Response is the response half of the Cacheable predicate.
type Response struct {
	Status      int
	ContentType string
	Body        []byte
	// RedirectedHost is true when a redirect changed host (§8.3 rule 7).
	RedirectedHost bool
	// ShapeErr is the caller's strict json.Unmarshal error, if any. Bodies
	// arrive Transfer-Encoding: chunked with no Content-Length, so a truncated
	// body can still look like valid JSON to a permissive decoder (§8.3 rule 8).
	ShapeErr error
}

// Reasons returned by Cacheable. ReasonCacheable is the only one that permits a
// disk write.
const (
	ReasonCacheable           = "cacheable"
	ReasonNoCache             = "cache_disabled"
	ReasonNonIdempotent       = "non_idempotent_method"
	ReasonStatusNot200        = "status_not_200"
	ReasonContentTypeNotJSON  = "content_type_not_json"
	ReasonClassNotPersistable = "class_not_persistable"
	ReasonIdentityResponse    = "identity_response"
	ReasonAnonymousIntoScope  = "anonymous_into_authenticated_scope"
	ReasonRedirectHostChanged = "redirect_changed_host"
	ReasonBodyIncomplete      = "body_incomplete"
	ReasonUnknownClass        = "unknown_class"
)

// Cacheable is §8.3: the only function authorised to permit a disk write.
//
// Rule 1 reads "any non-GET response" in §8.3, but §8.2 caches
// graphql/<Type>.json, which can only be obtained by POSTing an introspection
// query. The two are reconciled the way §6.1 already reconciles them for
// retries: the gate is the *idempotency-safe* predicate (GET, read-only
// POST /api/graphql, or POST with the method-override header), not the literal
// method. A real write is never cacheable under either reading.
func Cacheable(req Request, resp Response) (bool, string) {
	if req.NoCache {
		return false, ReasonNoCache
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method != "GET" && !req.IdempotencySafe {
		return false, ReasonNonIdempotent
	}
	// Rule 2. 501 and 500 included: a cached 501 would permanently hide a
	// collection the project later re-enables, so negative results live only in
	// the in-memory negative memo (§8.6).
	if resp.Status != 200 {
		return false, ReasonStatusNot200
	}
	// Rule 3. A wrong base URL returns 200 text/html; a 22 KB Next.js error
	// page must never be stored as a discovery result.
	if !isJSONContentType(resp.ContentType) {
		return false, ReasonContentTypeNotJSON
	}
	// Rules 4 and 5, by class.
	if _, known := TTLFor(req.Class); !known {
		return false, ReasonUnknownClass
	}
	if req.Class == ClassIdentity || isIdentityURL(req.URL) {
		return false, ReasonIdentityResponse
	}
	if !req.Class.Persistable() {
		return false, ReasonClassNotPersistable
	}
	// Rule 6. Structurally impossible given the "anon" sentinel, but asserted.
	if req.Anonymous && req.ScopeKeyFingerprint != "" && req.ScopeKeyFingerprint != AnonKeyFingerprint {
		return false, ReasonAnonymousIntoScope
	}
	// Rule 7.
	if resp.RedirectedHost {
		return false, ReasonRedirectHostChanged
	}
	// Rule 8.
	if resp.ShapeErr != nil || !completeJSON(resp.Body) {
		return false, ReasonBodyIncomplete
	}
	return true, ReasonCacheable
}

// isIdentityURL matches /{auth-collection}/me under any api_path. §8.3 rule 5
// is enforced by class *and* by URL, because a /me body contains a plaintext
// API key (verified) and one mislabelled call site must not be enough to leak it.
func isIdentityURL(raw string) bool {
	path := raw
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	path = strings.TrimRight(path, "/")
	return strings.HasSuffix(path, "/me")
}

func isJSONContentType(ct string) bool {
	if ct == "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		// Tolerate a bare, unparseable "application/json" with stray params.
		mt = strings.TrimSpace(strings.ToLower(strings.SplitN(ct, ";", 2)[0]))
	}
	return strings.ToLower(mt) == "application/json"
}

// completeJSON is the structural half of §8.3 rule 8: the body must decode as
// exactly one complete JSON value with nothing after it. json.Valid alone would
// accept a truncated object no better than a permissive decoder does.
func completeJSON(body []byte) bool {
	if len(bytes.TrimSpace(body)) == 0 {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return false
	}
	return !dec.More()
}

// ---------------------------------------------------------------------------
// Writing a discovery set (§8.7).
// ---------------------------------------------------------------------------

// Set is everything one discovery run publishes. Shards and GraphQL blobs are
// written first and manifest.json is renamed last, so the index — which names
// and checksums every shard — only becomes visible after everything it points
// at exists (§8.7).
type Set struct {
	// Generation is the run's ULID. It must match the manifest's own.
	Generation string
	// Manifest is the §7.8.1 index: a struct, a map, or raw JSON bytes.
	Manifest any
	// Shards maps a manifest-relative shard path ("fields/pages.json") to its
	// §7.8.2 body.
	Shards map[string]any
	// GraphQL maps a GraphQL type name to its introspection Entry body.
	GraphQL map[string]Entry
}

// WriteSet publishes a discovery run atomically.
//
// It returns ok=false plus a cache_write_failed warning for every failure mode,
// and never an error: §8.7 requires that a read-only or full $CACHE degrade to
// "slower", never to "broken", and requires that the degradation be visible
// rather than silent.
func (s *Store) WriteSet(sc Scope, set Set, now time.Time) (bool, []output.Warning) {
	if !s.Enabled() {
		return false, nil
	}
	if !sc.Valid() {
		return false, []output.Warning{*warning(WarnCacheWriteFailed, "refusing to write a malformed scope key")}
	}
	if !ValidGeneration(set.Generation) {
		return false, []output.Warning{*warning(WarnCacheWriteFailed, s.ScopeDir(sc)+": malformed generation")}
	}

	manifestData, err := canonicalJSON(set.Manifest)
	if err != nil {
		return false, []output.Warning{*warning(WarnCacheWriteFailed, s.ManifestPath(sc)+": "+err.Error())}
	}
	if reason, ok := verifyManifestBytes(manifestData, sc, set.Generation); !ok {
		return false, []output.Warning{*warning(WarnCacheWriteFailed, s.ManifestPath(sc)+": "+reason)}
	}

	dir := s.ScopeDir(sc)
	if err := os.MkdirAll(dir, DirPerm); err != nil {
		return false, []output.Warning{*warning(WarnCacheWriteFailed, dir+": "+err.Error())}
	}

	lk, err := acquireLock(s.LockPath(sc), s.lockTimeout())
	if err != nil {
		return false, []output.Warning{*warning(WarnCacheWriteFailed, s.LockPath(sc)+": "+err.Error())}
	}
	defer lk.release()

	// 1. Shards.
	for ref, body := range set.Shards {
		path, ok := s.shardPath(sc, ref)
		if !ok {
			return false, []output.Warning{*warning(WarnCacheWriteFailed, dir+": refusing unsafe shard path "+ref)}
		}
		data, err := canonicalJSON(body)
		if err != nil {
			return false, []output.Warning{*warning(WarnCacheWriteFailed, path+": "+err.Error())}
		}
		if err := fsatomic.Write(path, data, FilePerm); err != nil {
			return false, []output.Warning{*warning(WarnCacheWriteFailed, path+": "+err.Error())}
		}
	}

	// 2. GraphQL blobs.
	for typeName, entry := range set.GraphQL {
		path, ok := s.graphQLPath(sc, typeName)
		if !ok {
			return false, []output.Warning{*warning(WarnCacheWriteFailed, dir+": refusing unsafe graphql type name "+typeName)}
		}
		entry.Scope = sc.Key
		entry.Generation = set.Generation
		if entry.Class == "" {
			entry.Class = ClassGraphQLType
		}
		if entry.FetchedAt.IsZero() {
			entry.FetchedAt = now.UTC().Truncate(time.Second)
		}
		if entry.CLIMajor == 0 {
			entry.CLIMajor = cliMajor()
		}
		if entry.TTL == 0 {
			ttl, _ := TTLFor(entry.Class)
			entry.TTL = int64(ttl.Fresh / time.Second)
		}
		entry.URL = redactedURL(entry.URL)
		if reason, ok := entry.verify(sc); !ok {
			return false, []output.Warning{*warning(WarnCacheWriteFailed, path+": "+reason)}
		}
		data, err := canonicalJSON(entry)
		if err != nil {
			return false, []output.Warning{*warning(WarnCacheWriteFailed, path+": "+err.Error())}
		}
		if err := fsatomic.Write(path, data, FilePerm); err != nil {
			return false, []output.Warning{*warning(WarnCacheWriteFailed, path+": "+err.Error())}
		}
	}

	// 3. The index, last.
	if err := fsatomic.Write(s.ManifestPath(sc), manifestData, FilePerm); err != nil {
		return false, []output.Warning{*warning(WarnCacheWriteFailed, s.ManifestPath(sc)+": "+err.Error())}
	}

	// Release before sweeping: a lock is never held across work that may touch
	// other scopes, and release is idempotent so the defer above stays correct.
	lk.release()

	return true, s.MaybeGC(now)
}

// Touch bumps meta.confirmed_at after a successful §8.5 step-3 revalidation.
// It rewrites the index in place, atomically, under the same lock the writer
// uses; nothing else in the file changes.
func (s *Store) Touch(sc Scope, now time.Time) (bool, *output.Warning) {
	if !s.Enabled() || !sc.Valid() {
		return false, nil
	}
	path := s.ManifestPath(sc)
	data, err := os.ReadFile(path)
	if err != nil {
		return false, readWarning(path, err)
	}
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return false, warning(WarnCacheUnreadable, path+": "+err.Error())
	}
	meta, _ := doc["meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		doc["meta"] = meta
	}
	meta["confirmed_at"] = now.UTC().Truncate(time.Second).Format(time.RFC3339)

	out, err := canonicalJSON(doc)
	if err != nil {
		return false, warning(WarnCacheWriteFailed, path+": "+err.Error())
	}
	lk, err := acquireLock(s.LockPath(sc), s.lockTimeout())
	if err != nil {
		return false, warning(WarnCacheWriteFailed, s.LockPath(sc)+": "+err.Error())
	}
	defer lk.release()
	if err := fsatomic.Write(path, out, FilePerm); err != nil {
		return false, warning(WarnCacheWriteFailed, path+": "+err.Error())
	}
	return true, nil
}

// Clear removes one scope, using the same rename-then-remove dance as GC so a
// concurrent reader sees a clean ENOENT rather than a half-deleted tree (§8.7).
func (s *Store) Clear(scope string, now time.Time) error {
	if !s.Enabled() {
		return nil
	}
	if !ValidScopeKey(scope) {
		return errors.New("cache: " + scope + " is not a scope key")
	}
	dir := filepath.Join(s.EpochDir(), scope)
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return s.trash(dir, now)
}

// ClearAll removes the whole cache root, including auth-resolution.json
// (§7.0(e): `pay cache clear --all` removes it).
func (s *Store) ClearAll() error {
	if !s.Enabled() {
		return nil
	}
	return os.RemoveAll(s.root)
}

// ---------------------------------------------------------------------------
// Locking (§8.7). The syscall shims live in lock_unix.go / lock_windows.go.
// ---------------------------------------------------------------------------

// procLocks serialises writers *within* one process before they contend on the
// advisory file lock. flock and LockFileEx both work per open handle, so this is
// not required for correctness — it is what keeps a fan-out of goroutines from
// burning the 5 s budget spinning against itself.
var procLocks sync.Map // lock path -> chan struct{} (capacity 1)

type fileLock struct {
	f   *os.File
	sem chan struct{}
}

func acquireLock(path string, timeout time.Duration) (*fileLock, error) {
	v, _ := procLocks.LoadOrStore(path, make(chan struct{}, 1))
	sem, _ := v.(chan struct{})

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	select {
	case sem <- struct{}{}:
	case <-deadline.C:
		return nil, ErrLockTimeout
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, FilePerm)
	if err != nil {
		<-sem
		return nil, err
	}

	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	for {
		ok, err := lockFile(f)
		if err != nil {
			_ = f.Close()
			<-sem
			return nil, err
		}
		if ok {
			return &fileLock{f: f, sem: sem}, nil
		}
		select {
		case <-deadline.C:
			_ = f.Close()
			<-sem
			return nil, ErrLockTimeout
		case <-ticker.C:
		}
	}
}

func (l *fileLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = unlockFile(l.f)
	_ = l.f.Close()
	l.f = nil
	if l.sem != nil {
		<-l.sem
		l.sem = nil
	}
}

// ---------------------------------------------------------------------------
// Shared helpers.
// ---------------------------------------------------------------------------

// canonicalJSON renders a value as §8.2's "plain, pretty-printed, key-sorted
// JSON". Everything written by this package goes through it, so a file written
// by discovery and a file rewritten by Touch are byte-comparable.
//
// The round-trip through a decoder is what produces the key ordering (Go sorts
// map keys on encode) and UseNumber is what keeps a 19-digit document id from
// being mangled by float64. redact.JSON is applied last as defence in depth:
// §7.8.3 forbids apiKey/hash/salt/sessions from ever reaching a manifest, and
// this is the single choke point where that can be enforced.
func canonicalJSON(v any) ([]byte, error) {
	var raw []byte
	switch t := v.(type) {
	case nil:
		return nil, errors.New("cache: refusing to write a nil value")
	case []byte:
		raw = t
	case json.RawMessage:
		raw = t
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		raw = b
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("cache: trailing data after JSON value")
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(decoded); err != nil {
		return nil, err
	}
	return redact.JSON(buf.Bytes()).Data, nil
}

// verifyManifestBytes asserts the index we are about to publish describes the
// scope directory it is going into and the run that produced it. A mismatch is
// a programming error in discovery; it is reported as a skipped write rather
// than a failed command, because a wrong index on disk is worse than no index.
func verifyManifestBytes(data []byte, sc Scope, generation string) (string, bool) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return err.Error(), false
	}
	switch {
	case m.ManifestVersion != ManifestVersion:
		return "manifest_version " + strconv.Itoa(m.ManifestVersion) + " != " + strconv.Itoa(ManifestVersion), false
	case m.Generation != generation:
		return "manifest generation does not match the set generation", false
	case m.Meta.Scope != sc.Key:
		return "manifest meta.scope does not match the target scope", false
	}
	return "", true
}

// readWarning turns a read error into §8.3's miss-plus-warning. A missing file
// is a plain miss: a cold cache is the normal first run, not a fault.
func readWarning(path string, err error) *output.Warning {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return warning(WarnCacheUnreadable, path+": "+err.Error())
}

// cliMajor is the major version stamped into every entry header so a v1 binary
// never reads a v2 binary's derived blobs.
func cliMajor() int {
	v := strings.TrimPrefix(buildinfo.Version(), "v")
	if i := strings.IndexByte(v, '.'); i >= 0 {
		v = v[:i]
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

// BodyHash is the §8.6 singleflight key component for a request body.
func BodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// §8.6 — the in-process layer.
// ---------------------------------------------------------------------------

// SessionOptions carries the per-invocation cache flags (§8.5).
type SessionOptions struct {
	// NoCache is --no-cache / PAY_NO_CACHE=1: discovery still runs in-process,
	// nothing is persisted. It never disables client-side validation.
	NoCache bool
	// Refresh is --refresh: force a re-discovery, and do write it.
	Refresh bool
}

// Session is the in-process cache built once in PersistentPreRun and threaded
// on the context (§8.6). It holds the decoded manifest, the resolved identity
// (memory only), the Level-1 probe result, a singleflight group, a negative
// memo for 404/501 paths, a document memo, and the §8.4(b) re-entrancy guard.
//
// The manifest is immutable after construction and shared by read-only pointer;
// only the mutable memos are guarded.
type Session struct {
	store *Store
	scope Scope
	opts  SessionOptions

	mu            sync.Mutex
	manifest      *Manifest
	identity      any
	identityOK    bool
	topology      string
	topologyOK    bool
	discoveryMode string
	rediscovered  bool
	negative      map[string]bool
	docs          map[string]map[string]any
	flights       map[string]*flight
	warnings      []output.Warning
}

// NewSession builds the in-process layer for one invocation.
func NewSession(store *Store, sc Scope, opts SessionOptions) *Session {
	mode := output.CacheMiss
	if opts.NoCache {
		mode = output.CacheDisabled
	}
	return &Session{
		store:         store,
		scope:         sc,
		opts:          opts,
		discoveryMode: mode,
		negative:      map[string]bool{},
		docs:          map[string]map[string]any{},
		flights:       map[string]*flight{},
	}
}

// Store returns the disk layer, which is nil-safe and may be disabled.
func (s *Session) Store() *Store { return s.store }

// Scope returns the session's scope.
func (s *Session) Scope() Scope { return s.scope }

// Options returns the per-invocation cache flags.
func (s *Session) Options() SessionOptions { return s.opts }

// PersistWrites reports whether this session may write to disk. --no-cache
// discovers in-process but persists nothing; --refresh does write.
func (s *Session) PersistWrites() bool {
	return s != nil && !s.opts.NoCache && s.store.Enabled()
}

// Manifest returns the shared read-only manifest, or nil.
func (s *Session) Manifest() *Manifest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.manifest
}

// SetManifest publishes the manifest for the rest of the process.
func (s *Session) SetManifest(m *Manifest) {
	s.mu.Lock()
	s.manifest = m
	s.mu.Unlock()
}

// LoadManifest reads the manifest from disk once per process, collecting any
// cache_unreadable warning. --refresh skips the read entirely so a forced
// refresh never races its own stale copy.
func (s *Session) LoadManifest() (*Manifest, bool) {
	if m := s.Manifest(); m != nil {
		return m, true
	}
	if s.opts.Refresh {
		return nil, false
	}
	m, ok, warn := s.store.ReadManifest(s.scope)
	if warn != nil {
		s.AddWarnings(*warn)
	}
	if !ok {
		return nil, false
	}
	s.SetManifest(m)
	return m, true
}

// Identity is the resolved /me result. Memory only, for the whole process
// lifetime: §8.3 rule 5 forbids it ever reaching disk.
func (s *Session) Identity() (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.identity, s.identityOK
}

// SetIdentity records the resolved identity in memory.
func (s *Session) SetIdentity(v any) {
	s.mu.Lock()
	s.identity, s.identityOK = v, true
	s.mu.Unlock()
}

// Topology returns the memoised Level-1 probe result, so a command touching
// three collections issues one /api/access, not three.
func (s *Session) Topology() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.topology, s.topologyOK
}

// SetTopology records the Level-1 probe result.
func (s *Session) SetTopology(fingerprint string) {
	s.mu.Lock()
	s.topology, s.topologyOK = fingerprint, true
	s.mu.Unlock()
}

// DiscoveryMode returns the meta.cache.discovery value for this invocation.
func (s *Session) DiscoveryMode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.discoveryMode
}

// SetDiscoveryMode records how the manifest was obtained (output.Cache*).
func (s *Session) SetDiscoveryMode(mode string) {
	s.mu.Lock()
	// --no-cache is sticky: an in-process discovery under --no-cache is
	// reported as "disabled", never as "miss", so an agent can tell the two
	// apart in meta.cache.discovery.
	if !s.opts.NoCache {
		s.discoveryMode = mode
	}
	s.mu.Unlock()
}

// AllowRediscovery is §8.4(b): at most one Level-3 re-discovery per scope per
// process. The first call returns true; every later call returns false, and the
// caller must fail with schema_stale (exit 10) rather than re-discover again —
// no ping-pong between a genuinely stale scope and a genuinely invalid path.
func (s *Session) AllowRediscovery() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rediscovered {
		return false
	}
	s.rediscovered = true
	return true
}

// Rediscovered reports whether the Level-3 guard has already fired.
func (s *Session) Rediscovered() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rediscovered
}

// MarkNegative records a 404/501 path so a fan-out over 49 collections hits
// payload-migrations's 501 once rather than 49 times. Negative results are
// memory-only by §8.3 rule 2.
func (s *Session) MarkNegative(key string) {
	s.mu.Lock()
	s.negative[key] = true
	s.mu.Unlock()
}

// Negative reports whether key already produced a 404/501 in this process.
func (s *Session) Negative(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.negative[key]
}

// Doc reads the RAM document memo. The RAM layer may hold document data; the
// disk layer may not (§8.6).
func (s *Session) Doc(collection, key string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	byColl, ok := s.docs[collection]
	if !ok {
		return nil, false
	}
	v, ok := byColl[key]
	return v, ok
}

// PutDoc memoises a document for the rest of the process.
func (s *Session) PutDoc(collection, key string, v any) {
	s.mu.Lock()
	if s.docs[collection] == nil {
		s.docs[collection] = map[string]any{}
	}
	s.docs[collection][key] = v
	s.mu.Unlock()
}

// FlushCollection drops the RAM memo for a collection. Any successful write
// must call this immediately (§8.6).
func (s *Session) FlushCollection(collection string) {
	s.mu.Lock()
	delete(s.docs, collection)
	s.mu.Unlock()
}

// AddWarnings collects cache warnings for the envelope. Safe from any goroutine.
func (s *Session) AddWarnings(w ...output.Warning) {
	if len(w) == 0 {
		return
	}
	s.mu.Lock()
	s.warnings = append(s.warnings, w...)
	s.mu.Unlock()
}

// Warnings returns a copy of the collected warnings, de-duplicated by
// code+message so a fan-out over 49 collections cannot emit 49 identical
// cache_unreadable lines.
func (s *Session) Warnings() []output.Warning {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[string]struct{}, len(s.warnings))
	out := make([]output.Warning, 0, len(s.warnings))
	for _, w := range s.warnings {
		k := w.Code + "\x00" + w.Message
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, w)
	}
	return out
}

// FlightKey is §8.6's singleflight key: method + url + sha256(body).
func FlightKey(method, url string, body []byte) string {
	return strings.ToUpper(method) + " " + url + " " + BodyHash(body)
}

type flight struct {
	wg  sync.WaitGroup
	val any
	err error
}

// Do runs fn once per key for the lifetime of the session, sharing the result
// with every concurrent caller. shared reports whether the result came from
// another caller's in-flight call.
//
// A tiny singleflight rather than golang.org/x/sync: §3.2 pins the dependency
// list at seven direct modules, and this is twenty lines.
func (s *Session) Do(key string, fn func() (any, error)) (val any, shared bool, err error) {
	s.mu.Lock()
	if f, ok := s.flights[key]; ok {
		s.mu.Unlock()
		f.wg.Wait()
		return f.val, true, f.err
	}
	f := &flight{}
	f.wg.Add(1)
	s.flights[key] = f
	s.mu.Unlock()

	defer func() {
		// A panic in fn must not leave every other caller blocked on wg.
		if r := recover(); r != nil {
			f.err = errors.New("cache: singleflight call panicked")
			f.wg.Done()
			s.mu.Lock()
			delete(s.flights, key)
			s.mu.Unlock()
			panic(r)
		}
	}()

	f.val, f.err = fn()
	f.wg.Done()

	// The result stays memoised for the rest of the process: a successful
	// discovery must not be re-run, and a failed one must not be retried in a
	// loop by 49 parallel callers.
	return f.val, false, f.err
}

// RevalidateRead runs the §8.5 staleness ladder for a stale manifest on the
// read path: the caller is holding its computed response unwritten, this call
// spends at most RevalidationBudget deciding what to do with it, and the
// resulting discovery mode, confirmed_at bump and warning are all applied to
// the session.
func (s *Session) RevalidateRead(ctx context.Context, probe Probe, now time.Time) Revalidation {
	r := Revalidate(ctx, s.cachedTopology(), probe, RevalidationBudget)
	s.applyRevalidation(r, now)
	return r
}

// RevalidateWrite is §8.5's "serve-stale is read-path only": a write whose
// client-side validation depends on cached field names waits for the Level-1
// probe rather than capping it at 250 ms. The wait is still bounded, by the
// command's own --timeout/--deadline on ctx.
//
// A probe that fails here forces a re-discovery rather than a stale serve: for
// a write, "I could not confirm the schema" and "the schema changed" must have
// the same consequence.
func (s *Session) RevalidateWrite(ctx context.Context, probe Probe, now time.Time) Revalidation {
	cached := s.cachedTopology()
	if probe == nil {
		r := Revalidation{Discovery: output.CacheRevalidated, Refresh: true}
		s.applyRevalidation(r, now)
		return r
	}
	fp, err := probe(ctx)
	r := Revalidation{Fingerprint: fp, Err: err}
	switch {
	case err != nil:
		r.Discovery = output.CacheRevalidated
		r.Refresh = true
	case fp == cached && fp != "":
		r.Discovery = output.CacheStaleServed
		r.Confirm = true
	default:
		r.Discovery = output.CacheRevalidated
		r.Refresh = true
	}
	s.applyRevalidation(r, now)
	return r
}

func (s *Session) cachedTopology() string {
	if fp, ok := s.Topology(); ok {
		return fp
	}
	if m := s.Manifest(); m != nil {
		return m.Fingerprint.TopologySHA256
	}
	return ""
}

func (s *Session) applyRevalidation(r Revalidation, now time.Time) {
	if r.Fingerprint != "" {
		s.SetTopology(r.Fingerprint)
	}
	s.SetDiscoveryMode(r.Discovery)
	if r.Warning != nil {
		s.AddWarnings(*r.Warning)
	}
	if r.Confirm && s.PersistWrites() {
		// Ladder step 3: the stale entry was proven still good, so its
		// freshness window restarts from now.
		if _, warn := s.store.Touch(s.scope, now); warn != nil {
			s.AddWarnings(*warn)
		}
	}
}
