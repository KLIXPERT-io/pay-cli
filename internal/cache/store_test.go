package cache

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/output"
)

var testNow = time.Date(2026, 9, 16, 17, 0, 0, 0, time.UTC)

func newTestStore(t *testing.T) (*Store, Scope) {
	t.Helper()
	s := New(t.TempDir())
	s.LockTimeout = 2 * time.Second
	s.DisableGC = true
	return s, mustScope(t, base())
}

// manifestDoc builds a §7.8.1-shaped index for a scope.
func manifestDoc(sc Scope, generation string, collections ...string) map[string]any {
	colls := make([]any, 0, len(collections))
	for _, slug := range collections {
		colls = append(colls, map[string]any{
			"slug":          slug,
			"fields_count":  2,
			"fields_sha256": "sha-" + slug,
			"fields_shard":  ShardName(slug, KindCollection),
		})
	}
	return map[string]any{
		"manifest_version": ManifestVersion,
		"generation":       generation,
		"cli_version":      "0.1.0",
		"generated_at":     testNow.Format(time.RFC3339),
		"expires_at":       testNow.Add(24 * time.Hour).Format(time.RFC3339),
		"meta": map[string]any{
			"scope":           sc.Key,
			"profiles":        []any{"local"},
			"base_url":        "http://localhost:3900",
			"api_path":        "/api",
			"graphql_path":    "/api/graphql",
			"header_names":    []any{},
			"key_fingerprint": sc.KeyFingerprint,
			"created_at":      testNow.Format(time.RFC3339),
			"confirmed_at":    testNow.Format(time.RFC3339),
		},
		"fingerprint": map[string]any{
			"topology_sha256": "top-1",
			"schema_sha256":   "schema-1",
			"checked_at":      testNow.Format(time.RFC3339),
			"server_identity": "Next.js, Payload",
		},
		"collections": colls,
		"globals":     []any{},
	}
}

func shardDoc(slug, generation string) map[string]any {
	return map[string]any{
		"generation": generation,
		"slug":       slug,
		"sha256":     "sha-" + slug,
		"fields":     []any{map[string]any{"name": "title", "path": "title"}},
	}
}

func writeFixture(t *testing.T, s *Store, sc Scope, generation string, slugs ...string) {
	t.Helper()
	set := Set{
		Generation: generation,
		Manifest:   manifestDoc(sc, generation, slugs...),
		Shards:     map[string]any{},
	}
	for _, slug := range slugs {
		set.Shards[ShardName(slug, KindCollection)] = shardDoc(slug, generation)
	}
	ok, warns := s.WriteSet(sc, set, testNow)
	if !ok {
		t.Fatalf("WriteSet failed: %+v", warns)
	}
}

func TestWriteSetRoundTrip(t *testing.T) {
	s, sc := newTestStore(t)
	gen := NewGeneration(testNow)
	writeFixture(t, s, sc, gen, "pages", "crm-contacts")

	m, ok, warn := s.ReadManifest(sc)
	if !ok {
		t.Fatalf("manifest miss, warning=%+v", warn)
	}
	if m.Generation != gen || m.Meta.Scope != sc.Key || len(m.Collections) != 2 {
		t.Fatalf("unexpected manifest: %+v", m)
	}
	sh, ok, warn := s.ReadShard(sc, m, "pages", KindCollection)
	if !ok {
		t.Fatalf("shard miss, warning=%+v", warn)
	}
	if sh.Slug != "pages" || sh.Generation != gen {
		t.Fatalf("unexpected shard: %+v", sh)
	}

	// §4.1: files 0600, directories 0700.
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(s.ManifestPath(sc))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("manifest mode = %v, want 0600", fi.Mode().Perm())
		}
		di, err := os.Stat(s.ScopeDir(sc))
		if err != nil {
			t.Fatal(err)
		}
		if di.Mode().Perm() != 0o700 {
			t.Fatalf("scope dir mode = %v, want 0700", di.Mode().Perm())
		}
	}
}

func TestManifestIsPrettyKeySortedJSON(t *testing.T) {
	s, sc := newTestStore(t)
	writeFixture(t, s, sc, NewGeneration(testNow), "pages")

	data, err := os.ReadFile(s.ManifestPath(sc))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "\n  \"collections\": [") {
		t.Fatalf("manifest is not pretty-printed:\n%s", text)
	}
	// Key-sorted: "cli_version" precedes "collections" precedes "expires_at".
	if i, j := strings.Index(text, `"cli_version"`), strings.Index(text, `"collections"`); i > j {
		t.Fatalf("keys are not sorted:\n%s", text)
	}
	var round any
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
}

// TestReadPathFailuresAreMisses is §8.3's "any I/O error on the cache read path
// is a miss, never a command failure".
func TestReadPathFailuresAreMisses(t *testing.T) {
	gen := NewGeneration(testNow)

	tests := []struct {
		name        string
		mutate      func(t *testing.T, s *Store, sc Scope)
		wantWarning bool
	}{
		{
			name:   "cold cache",
			mutate: func(*testing.T, *Store, Scope) {},
			// A first run is not an anomaly and must not produce a warning.
			wantWarning: false,
		},
		{
			name: "malformed json",
			mutate: func(t *testing.T, s *Store, sc Scope) {
				writeFixture(t, s, sc, gen, "pages")
				if err := os.WriteFile(s.ManifestPath(sc), []byte(`{"manifest_version":`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantWarning: true,
		},
		{
			name: "unknown manifest_version",
			mutate: func(t *testing.T, s *Store, sc Scope) {
				doc := manifestDoc(sc, gen, "pages")
				doc["manifest_version"] = 1
				data, _ := json.Marshal(doc)
				mustWriteFile(t, s.ManifestPath(sc), data)
			},
			wantWarning: true,
		},
		{
			name: "scope mismatch",
			mutate: func(t *testing.T, s *Store, sc Scope) {
				doc := manifestDoc(sc, gen, "pages")
				doc["meta"].(map[string]any)["scope"] = "ffffffffffffffff"
				data, _ := json.Marshal(doc)
				mustWriteFile(t, s.ManifestPath(sc), data)
			},
			wantWarning: true,
		},
		{
			name: "malformed generation",
			mutate: func(t *testing.T, s *Store, sc Scope) {
				doc := manifestDoc(sc, "not-a-ulid", "pages")
				data, _ := json.Marshal(doc)
				mustWriteFile(t, s.ManifestPath(sc), data)
			},
			wantWarning: true,
		},
		{
			name: "truncated file",
			mutate: func(t *testing.T, s *Store, sc Scope) {
				writeFixture(t, s, sc, gen, "pages")
				data, err := os.ReadFile(s.ManifestPath(sc))
				if err != nil {
					t.Fatal(err)
				}
				mustWriteFile(t, s.ManifestPath(sc), data[:len(data)/2])
			},
			wantWarning: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, sc := newTestStore(t)
			tc.mutate(t, s, sc)
			m, ok, warn := s.ReadManifest(sc)
			if ok || m != nil {
				t.Fatal("expected a miss")
			}
			if (warn != nil) != tc.wantWarning {
				t.Fatalf("warning=%+v, wantWarning=%v", warn, tc.wantWarning)
			}
			if warn != nil {
				if warn.Code != WarnCacheUnreadable || warn.Hint != hintCachePath {
					t.Fatalf("unexpected warning: %+v", warn)
				}
			}
		})
	}
}

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestTornReadDetection is §8.7: an index from writer B against a shard from
// writer A must be a miss for that entity, not a mixed answer.
func TestTornReadDetection(t *testing.T) {
	s, sc := newTestStore(t)
	genA := NewGeneration(testNow)
	writeFixture(t, s, sc, genA, "pages")

	// Publish a new index with a new generation, leaving the old shard behind.
	genB := NewGeneration(testNow.Add(time.Second))
	doc := manifestDoc(sc, genB, "pages")
	data, _ := json.Marshal(doc)
	mustWriteFile(t, s.ManifestPath(sc), data)

	m, ok, _ := s.ReadManifest(sc)
	if !ok {
		t.Fatal("manifest miss")
	}
	sh, ok, warn := s.ReadShard(sc, m, "pages", KindCollection)
	if ok || sh != nil {
		t.Fatal("a stale-generation shard was served")
	}
	if warn == nil || !strings.Contains(warn.Message, "generation mismatch") {
		t.Fatalf("warning = %+v, want a generation mismatch", warn)
	}
	// The shard is NOT deleted: it may belong to a writer whose index has not
	// been renamed into place yet.
	if _, err := os.Stat(filepath.Join(s.ScopeDir(sc), "fields", "pages.json")); err != nil {
		t.Fatalf("a mismatched shard was deleted: %v", err)
	}
}

func TestShardSHAMismatchIsAMiss(t *testing.T) {
	s, sc := newTestStore(t)
	gen := NewGeneration(testNow)
	writeFixture(t, s, sc, gen, "pages")

	shard := shardDoc("pages", gen)
	shard["sha256"] = "sha-something-else"
	data, _ := json.Marshal(shard)
	mustWriteFile(t, filepath.Join(s.ScopeDir(sc), "fields", "pages.json"), data)

	m, _, _ := s.ReadManifest(sc)
	if _, ok, warn := s.ReadShard(sc, m, "pages", KindCollection); ok {
		t.Fatal("a shard with the wrong sha256 was served")
	} else if warn == nil || !strings.Contains(warn.Message, "sha256 mismatch") {
		t.Fatalf("warning = %+v", warn)
	}
}

func TestReadShardUnknownEntity(t *testing.T) {
	s, sc := newTestStore(t)
	writeFixture(t, s, sc, NewGeneration(testNow), "pages")
	m, _, _ := s.ReadManifest(sc)
	if _, ok, warn := s.ReadShard(sc, m, "nope", KindCollection); ok || warn != nil {
		t.Fatalf("ok=%v warn=%+v, want a silent miss", ok, warn)
	}
}

func TestWriteSetRefusesUnsafePaths(t *testing.T) {
	s, sc := newTestStore(t)
	gen := NewGeneration(testNow)
	for _, ref := range []string{"../escape.json", "fields/../../escape.json", "/etc/passwd", "graphql/x.json"} {
		t.Run(ref, func(t *testing.T) {
			ok, warns := s.WriteSet(sc, Set{
				Generation: gen,
				Manifest:   manifestDoc(sc, gen),
				Shards:     map[string]any{ref: shardDoc("x", gen)},
			}, testNow)
			if ok {
				t.Fatalf("WriteSet accepted shard path %q", ref)
			}
			if len(warns) != 1 || warns[0].Code != WarnCacheWriteFailed {
				t.Fatalf("warnings = %+v", warns)
			}
		})
	}
}

func TestWriteSetVerifiesManifestIdentity(t *testing.T) {
	s, sc := newTestStore(t)
	gen := NewGeneration(testNow)

	other := manifestDoc(sc, gen)
	other["meta"].(map[string]any)["scope"] = "ffffffffffffffff"
	if ok, warns := s.WriteSet(sc, Set{Generation: gen, Manifest: other}, testNow); ok {
		t.Fatal("WriteSet published an index belonging to another scope")
	} else if len(warns) == 0 {
		t.Fatal("a skipped write must never be silent")
	}

	mismatched := manifestDoc(sc, NewGeneration(testNow.Add(time.Hour)))
	if ok, _ := s.WriteSet(sc, Set{Generation: gen, Manifest: mismatched}, testNow); ok {
		t.Fatal("WriteSet published an index from a different run")
	}
}

// TestWriteSetPublishesIndexLast is §8.7's ordering requirement.
func TestWriteSetPublishesIndexLast(t *testing.T) {
	s, sc := newTestStore(t)
	gen := NewGeneration(testNow)

	// A shard path that cannot be written aborts the whole set before the
	// index appears, so the index never names a shard that does not exist.
	ok, warns := s.WriteSet(sc, Set{
		Generation: gen,
		Manifest:   manifestDoc(sc, gen, "pages"),
		Shards:     map[string]any{ShardName("pages", KindCollection): make(chan int)},
	}, testNow)
	if ok {
		t.Fatal("WriteSet succeeded with an unencodable shard")
	}
	if len(warns) == 0 {
		t.Fatal("no warning for a failed write")
	}
	if _, err := os.Stat(s.ManifestPath(sc)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the index was published despite a failed shard: %v", err)
	}
}

func TestReadOnlyCacheDirWarnsAndNeverFails(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission semantics differ")
	}
	s, sc := newTestStore(t)
	if err := os.MkdirAll(s.EpochDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.EpochDir(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(s.EpochDir(), 0o700) })

	gen := NewGeneration(testNow)
	ok, warns := s.WriteSet(sc, Set{Generation: gen, Manifest: manifestDoc(sc, gen)}, testNow)
	if ok {
		t.Fatal("WriteSet reported success on a read-only cache")
	}
	if len(warns) != 1 || warns[0].Code != WarnCacheWriteFailed || warns[0].Hint != hintCachePath {
		t.Fatalf("warnings = %+v, want exactly one cache_write_failed", warns)
	}
	// And the read path still degrades to a plain miss.
	if _, hit, _ := s.ReadManifest(sc); hit {
		t.Fatal("read reported a hit")
	}
}

func TestDisabledStoreIsSilent(t *testing.T) {
	s := New("")
	sc := mustScope(t, base())
	if s.Enabled() {
		t.Fatal("an empty root is not a disabled store")
	}
	if ok, warns := s.WriteSet(sc, Set{Generation: NewGeneration(testNow), Manifest: manifestDoc(sc, NewGeneration(testNow))}, testNow); ok || warns != nil {
		t.Fatalf("ok=%v warns=%+v, want a silent no-op", ok, warns)
	}
	if _, ok, warn := s.ReadManifest(sc); ok || warn != nil {
		t.Fatalf("ok=%v warn=%+v, want a silent miss", ok, warn)
	}
}

func TestTouchBumpsConfirmedAt(t *testing.T) {
	s, sc := newTestStore(t)
	gen := NewGeneration(testNow)
	writeFixture(t, s, sc, gen, "pages")

	later := testNow.Add(20 * time.Minute)
	if ok, warn := s.Touch(sc, later); !ok {
		t.Fatalf("Touch failed: %+v", warn)
	}
	m, ok, _ := s.ReadManifest(sc)
	if !ok {
		t.Fatal("manifest miss after Touch")
	}
	if !m.Meta.ConfirmedAt.Equal(later) {
		t.Fatalf("confirmed_at = %s, want %s", m.Meta.ConfirmedAt, later)
	}
	// Nothing else moved.
	if m.Generation != gen || !m.Meta.CreatedAt.Equal(testNow) || len(m.Collections) != 1 {
		t.Fatalf("Touch changed more than confirmed_at: %+v", m)
	}
}

func TestClearAndClearAll(t *testing.T) {
	s, sc := newTestStore(t)
	writeFixture(t, s, sc, NewGeneration(testNow), "pages")

	if err := s.Clear("not-a-scope", testNow); err == nil {
		t.Fatal("Clear accepted a non-scope name")
	}
	if err := s.Clear(sc.Key, testNow); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.ReadManifest(sc); ok {
		t.Fatal("scope survived Clear")
	}
	if err := s.Clear(sc.Key, testNow); err != nil {
		t.Fatalf("Clear of a missing scope must be a no-op: %v", err)
	}

	writeFixture(t, s, sc, NewGeneration(testNow), "pages")
	if ok, _ := s.PutAuthResolution(sc, "users", testNow); !ok {
		t.Fatal("PutAuthResolution failed")
	}
	if err := s.ClearAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Root()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache root survived ClearAll: %v", err)
	}
}

func TestAuthResolutionRoundTrip(t *testing.T) {
	s, sc := newTestStore(t)
	if _, ok, warn := s.LookupAuthResolution(sc); ok || warn != nil {
		t.Fatalf("cold lookup: ok=%v warn=%+v", ok, warn)
	}
	if ok, warn := s.PutAuthResolution(sc, "admins", testNow); !ok {
		t.Fatalf("put failed: %+v", warn)
	}
	rec, ok, _ := s.LookupAuthResolution(sc)
	if !ok || rec.AuthCollection != "admins" {
		t.Fatalf("lookup = %+v, ok=%v", rec, ok)
	}
	// A different credential against the same server must not inherit it.
	other := base()
	other.Credential = "a-completely-different-key"
	if _, ok, _ := s.LookupAuthResolution(mustScope(t, other)); ok {
		t.Fatal("auth resolution leaked across credentials")
	}
	// It must survive scope GC (it lives beside the scope directories).
	if err := s.Clear(sc.Key, testNow); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.LookupAuthResolution(sc); !ok {
		t.Fatal("auth resolution did not survive scope removal")
	}
	// The file names neither the server nor the credential.
	data, err := os.ReadFile(s.AuthResolutionPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), base().Credential) || strings.Contains(string(data), "localhost") {
		t.Fatalf("auth-resolution.json leaked identifying data:\n%s", data)
	}
}

// TestCacheable is §8.3's whole table.
func TestCacheable(t *testing.T) {
	okReq := Request{
		Method:              "GET",
		URL:                 "http://localhost:3900/api/access",
		Class:               ClassDiscoveryManifest,
		ScopeKeyFingerprint: "a1b2c3d4e5f60718",
	}
	okResp := Response{Status: 200, ContentType: "application/json; charset=utf-8", Body: []byte(`{"canAccessAdmin":true}`)}

	tests := []struct {
		name   string
		req    Request
		resp   Response
		want   bool
		reason string
	}{
		{"discovery GET", okReq, okResp, true, ReasonCacheable},
		{"POST write", with(okReq, func(r *Request) { r.Method = "POST" }), okResp, false, ReasonNonIdempotent},
		{"PATCH", with(okReq, func(r *Request) { r.Method = "PATCH" }), okResp, false, ReasonNonIdempotent},
		{"read-only graphql POST", with(okReq, func(r *Request) {
			r.Method = "POST"
			r.IdempotencySafe = true
			r.URL = "http://localhost:3900/api/graphql"
			r.Class = ClassGraphQLType
		}), okResp, true, ReasonCacheable},
		{"501 endpoints disabled", okReq, with2(okResp, func(r *Response) { r.Status = 501 }), false, ReasonStatusNot200},
		{"500", okReq, with2(okResp, func(r *Response) { r.Status = 500 }), false, ReasonStatusNot200},
		{"404", okReq, with2(okResp, func(r *Response) { r.Status = 404 }), false, ReasonStatusNot200},
		{"html error page", okReq, with2(okResp, func(r *Response) {
			r.ContentType = "text/html; charset=utf-8"
			r.Body = []byte("<!DOCTYPE html>")
		}), false, ReasonContentTypeNotJSON},
		{"no content type", okReq, with2(okResp, func(r *Response) { r.ContentType = "" }), false, ReasonContentTypeNotJSON},
		{"document data", with(okReq, func(r *Request) { r.Class = ClassDocument }), okResp, false, ReasonClassNotPersistable},
		{"identity by class", with(okReq, func(r *Request) { r.Class = ClassIdentity }), okResp, false, ReasonIdentityResponse},
		{"identity by url", with(okReq, func(r *Request) { r.URL = "http://localhost:3900/api/users/me" }), okResp, false, ReasonIdentityResponse},
		{"identity by url with query", with(okReq, func(r *Request) { r.URL = "http://localhost:3900/api/users/me?depth=0" }), okResp, false, ReasonIdentityResponse},
		{"unknown class", with(okReq, func(r *Request) { r.Class = "invented" }), okResp, false, ReasonUnknownClass},
		{"anonymous into authenticated scope", with(okReq, func(r *Request) { r.Anonymous = true }), okResp, false, ReasonAnonymousIntoScope},
		{"anonymous into anonymous scope", with(okReq, func(r *Request) {
			r.Anonymous = true
			r.ScopeKeyFingerprint = AnonKeyFingerprint
		}), okResp, true, ReasonCacheable},
		{"redirect changed host", okReq, with2(okResp, func(r *Response) { r.RedirectedHost = true }), false, ReasonRedirectHostChanged},
		{"truncated body", okReq, with2(okResp, func(r *Response) { r.Body = []byte(`{"canAccessAdmin":`) }), false, ReasonBodyIncomplete},
		{"empty body", okReq, with2(okResp, func(r *Response) { r.Body = nil }), false, ReasonBodyIncomplete},
		{"trailing garbage", okReq, with2(okResp, func(r *Response) { r.Body = []byte(`{"a":1}{"b":2}`) }), false, ReasonBodyIncomplete},
		{"caller shape error", okReq, with2(okResp, func(r *Response) { r.ShapeErr = errors.New("unexpected shape") }), false, ReasonBodyIncomplete},
		{"no-cache", with(okReq, func(r *Request) { r.NoCache = true }), okResp, false, ReasonNoCache},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := Cacheable(tc.req, tc.resp)
			if got != tc.want || reason != tc.reason {
				t.Fatalf("Cacheable = (%v, %q), want (%v, %q)", got, reason, tc.want, tc.reason)
			}
		})
	}
}

func with(r Request, f func(*Request)) Request     { f(&r); return r }
func with2(r Response, f func(*Response)) Response { f(&r); return r }

func TestGraphQLEntryRoundTripAndVerification(t *testing.T) {
	s, sc := newTestStore(t)
	gen := NewGeneration(testNow)
	body := []byte(`{"name":"Page","fields":[]}`)
	set := Set{
		Generation: gen,
		Manifest:   manifestDoc(sc, gen),
		GraphQL: map[string]Entry{
			"Page": {
				Method:       "POST",
				URL:          "http://localhost:3900/api/graphql?token=secret",
				Status:       200,
				ContentType:  "application/json",
				Class:        ClassGraphQLType,
				SchemaSHA256: "schema-1",
				Body:         body,
			},
		},
	}
	if ok, warns := s.WriteSet(sc, set, testNow); !ok {
		t.Fatalf("WriteSet: %+v", warns)
	}

	e, ok, warn := s.ReadGraphQLType(sc, "Page", "schema-1", testNow)
	if !ok {
		t.Fatalf("graphql miss: %+v", warn)
	}
	if e.Generation != gen || e.CLIMajor != cliMajor() || e.TTL != int64(24*time.Hour/time.Second) {
		t.Fatalf("unexpected entry header: %+v", e)
	}
	// The body is re-emitted as §8.2's pretty, key-sorted JSON, so compare it
	// semantically rather than byte for byte.
	var gotBody, wantBody any
	if err := json.Unmarshal(e.Body, &gotBody); err != nil {
		t.Fatalf("entry body is not JSON: %v", err)
	}
	if err := json.Unmarshal(body, &wantBody); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotBody, wantBody) {
		t.Fatalf("body = %s, want %s", e.Body, body)
	}
	if strings.Contains(e.URL, "secret") {
		t.Fatalf("entry URL was not redacted: %s", e.URL)
	}

	// A schema_sha256 mismatch is a miss AND deletes the entry (§8.3).
	if _, ok, warn := s.ReadGraphQLType(sc, "Page", "schema-2", testNow); ok {
		t.Fatal("a stale-schema entry was served")
	} else if warn == nil {
		t.Fatal("no warning for a schema mismatch")
	}
	path, _ := s.graphQLPath(sc, "Page")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an invalid entry was not deleted: %v", err)
	}
}

func TestGraphQLEntryExpiresByTTL(t *testing.T) {
	s, sc := newTestStore(t)
	gen := NewGeneration(testNow)
	set := Set{
		Generation: gen,
		Manifest:   manifestDoc(sc, gen),
		GraphQL: map[string]Entry{
			"Page": {Method: "POST", URL: "/api/graphql", Status: 200, ContentType: "application/json",
				Class: ClassGraphQLType, Body: []byte(`{}`)},
		},
	}
	if ok, warns := s.WriteSet(sc, set, testNow); !ok {
		t.Fatalf("WriteSet: %+v", warns)
	}
	// 8 days: past the 7-day hard max for graphql_type.
	if _, ok, warn := s.ReadGraphQLType(sc, "Page", "", testNow.Add(8*24*time.Hour)); ok {
		t.Fatal("an expired graphql entry was served")
	} else if warn != nil {
		t.Fatalf("an expiry is a plain miss, not a warning: %+v", warn)
	}
}

func TestWriteSetRefusesNonPersistableEntry(t *testing.T) {
	s, sc := newTestStore(t)
	gen := NewGeneration(testNow)
	ok, warns := s.WriteSet(sc, Set{
		Generation: gen,
		Manifest:   manifestDoc(sc, gen),
		GraphQL: map[string]Entry{
			"Page": {Method: "GET", URL: "/api/users/me", Status: 200, ContentType: "application/json",
				Class: ClassIdentity, Body: []byte(`{"user":{"apiKey":"x"}}`)},
		},
	}, testNow)
	if ok {
		t.Fatal("an identity-class entry was written to disk")
	}
	if len(warns) != 1 || !strings.Contains(warns[0].Message, "may not be cached") {
		t.Fatalf("warnings = %+v", warns)
	}
}

// TestCanonicalJSONRedactsSecrets is the §5.3 backstop: nothing written by this
// package may contain a secret, even if a caller hands one over.
func TestCanonicalJSONRedactsSecrets(t *testing.T) {
	data, err := canonicalJSON(map[string]any{
		"user": map[string]any{"id": 66, "apiKey": "paycli-dev-key-deadbeefdeadbeef", "email": "a@b.c"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "paycli-dev-key") {
		t.Fatalf("canonicalJSON leaked a secret:\n%s", data)
	}
	if !strings.Contains(string(data), "a@b.c") {
		t.Fatalf("canonicalJSON dropped non-secret data:\n%s", data)
	}
}

func TestCanonicalJSONPreservesLargeIntegers(t *testing.T) {
	const big = "1234567890123456789"
	data, err := canonicalJSON(json.RawMessage(`{"id":` + big + `}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), big) {
		t.Fatalf("large integer was mangled:\n%s", data)
	}
}

func TestCanonicalJSONRejectsJunk(t *testing.T) {
	for _, in := range []any{nil, json.RawMessage(`{"a":1}{"b":2}`), json.RawMessage(`{`)} {
		if _, err := canonicalJSON(in); err == nil {
			t.Fatalf("canonicalJSON accepted %v", in)
		}
	}
}

// TestConcurrentStoreHammer runs N goroutines writing and reading the same
// scope. It asserts §8.7's central claim: readers are lock-free and never
// observe a mixed set, and writers never corrupt each other.
func TestConcurrentStoreHammer(t *testing.T) {
	s, sc := newTestStore(t)
	const (
		writers    = 8
		readers    = 8
		iterations = 25
	)
	slugs := []string{"pages", "crm-contacts", "media"}

	// Seed so readers have something to find from the first instant.
	writeFixture(t, s, sc, NewGeneration(testNow), slugs...)

	var wg sync.WaitGroup
	errCh := make(chan string, (writers+readers)*iterations)

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				gen := NewGeneration(testNow.Add(time.Duration(w*iterations+i) * time.Millisecond))
				set := Set{Generation: gen, Manifest: manifestDoc(sc, gen, slugs...), Shards: map[string]any{}}
				for _, slug := range slugs {
					set.Shards[ShardName(slug, KindCollection)] = shardDoc(slug, gen)
				}
				if ok, warns := s.WriteSet(sc, set, testNow); !ok {
					errCh <- "write failed: " + warnText(warns)
					return
				}
			}
		}(w)
	}

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				m, ok, _ := s.ReadManifest(sc)
				if !ok {
					// A miss is always legal; a wrong answer is not.
					continue
				}
				if m.Meta.Scope != sc.Key || len(m.Collections) != len(slugs) {
					errCh <- "torn index observed"
					return
				}
				for _, slug := range slugs {
					sh, ok, _ := s.ReadShard(sc, m, slug, KindCollection)
					if !ok {
						continue // a generation mismatch is a legal miss
					}
					if sh.Generation != m.Generation || sh.Slug != slug {
						errCh <- "mixed generations served"
						return
					}
				}
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for msg := range errCh {
		t.Fatal(msg)
	}

	// The cache is still consistent afterwards.
	m, ok, warn := s.ReadManifest(sc)
	if !ok {
		t.Fatalf("manifest unreadable after the hammer: %+v", warn)
	}
	for _, slug := range slugs {
		if _, ok, warn := s.ReadShard(sc, m, slug, KindCollection); !ok {
			t.Fatalf("shard %s unreadable after the hammer: %+v", slug, warn)
		}
	}
}

func warnText(warns []output.Warning) string {
	parts := make([]string, 0, len(warns))
	for _, w := range warns {
		parts = append(parts, w.Code+": "+w.Message)
	}
	return strings.Join(parts, "; ")
}

// ---------------------------------------------------------------------------
// §8.6 — the in-process layer.
// ---------------------------------------------------------------------------

func TestSessionManifestMemo(t *testing.T) {
	s, sc := newTestStore(t)
	gen := NewGeneration(testNow)
	writeFixture(t, s, sc, gen, "pages")

	sess := NewSession(s, sc, SessionOptions{})
	m, ok := sess.LoadManifest()
	if !ok || m.Generation != gen {
		t.Fatalf("LoadManifest = %+v, ok=%v", m, ok)
	}
	// The manifest is shared by read-only pointer, not re-decoded.
	again, _ := sess.LoadManifest()
	if again != m {
		t.Fatal("the manifest was re-decoded instead of shared")
	}

	// --refresh never serves the copy on disk.
	fresh := NewSession(s, sc, SessionOptions{Refresh: true})
	if _, ok := fresh.LoadManifest(); ok {
		t.Fatal("--refresh served a cached manifest")
	}
}

func TestSessionDiscoveryMode(t *testing.T) {
	s, sc := newTestStore(t)

	sess := NewSession(s, sc, SessionOptions{})
	if got := sess.DiscoveryMode(); got != "miss" {
		t.Fatalf("cold mode = %q, want miss", got)
	}
	sess.SetDiscoveryMode("hit")
	if got := sess.DiscoveryMode(); got != "hit" {
		t.Fatalf("mode = %q, want hit", got)
	}

	// --no-cache is sticky: an in-process discovery is reported as "disabled".
	off := NewSession(s, sc, SessionOptions{NoCache: true})
	if got := off.DiscoveryMode(); got != "disabled" {
		t.Fatalf("--no-cache mode = %q, want disabled", got)
	}
	off.SetDiscoveryMode("hit")
	if got := off.DiscoveryMode(); got != "disabled" {
		t.Fatalf("--no-cache mode became %q", got)
	}
	if off.PersistWrites() {
		t.Fatal("--no-cache persisted writes")
	}
	if !NewSession(s, sc, SessionOptions{Refresh: true}).PersistWrites() {
		t.Fatal("--refresh must still write")
	}
	if NewSession(New(""), sc, SessionOptions{}).PersistWrites() {
		t.Fatal("a disabled store persisted writes")
	}
}

// TestSessionRediscoveryGuard is §8.4(b): at most one Level-3 re-discovery per
// scope per process.
func TestSessionRediscoveryGuard(t *testing.T) {
	sess := NewSession(New(""), mustScope(t, base()), SessionOptions{})
	if sess.Rediscovered() {
		t.Fatal("the guard fired before any re-discovery")
	}
	if !sess.AllowRediscovery() {
		t.Fatal("the first Level-3 trigger was refused")
	}
	if sess.AllowRediscovery() {
		t.Fatal("a second Level-3 re-discovery was allowed; §8.4(b) forbids the ping-pong")
	}
	if !sess.Rediscovered() {
		t.Fatal("Rediscovered did not latch")
	}
}

func TestSessionMemos(t *testing.T) {
	sess := NewSession(New(""), mustScope(t, base()), SessionOptions{})

	// Negative memo: payload-migrations's 501 is learned once, not 49 times.
	if sess.Negative("GET /api/payload-migrations") {
		t.Fatal("an unseen path was already negative")
	}
	sess.MarkNegative("GET /api/payload-migrations")
	if !sess.Negative("GET /api/payload-migrations") {
		t.Fatal("the negative memo did not stick")
	}

	// Identity is memory only.
	if _, ok := sess.Identity(); ok {
		t.Fatal("identity was set before resolution")
	}
	sess.SetIdentity(map[string]any{"id": 66})
	if v, ok := sess.Identity(); !ok || v.(map[string]any)["id"] != 66 {
		t.Fatalf("identity = %v, ok=%v", v, ok)
	}

	// Level-1 probe memo.
	if _, ok := sess.Topology(); ok {
		t.Fatal("topology was set before probing")
	}
	sess.SetTopology("top-1")
	if v, ok := sess.Topology(); !ok || v != "top-1" {
		t.Fatalf("topology = %q ok=%v", v, ok)
	}

	// Document memo, flushed per collection on write.
	sess.PutDoc("pages", "1", map[string]any{"title": "Home"})
	sess.PutDoc("media", "9", map[string]any{"filename": "a.png"})
	if _, ok := sess.Doc("pages", "1"); !ok {
		t.Fatal("document memo miss")
	}
	sess.FlushCollection("pages")
	if _, ok := sess.Doc("pages", "1"); ok {
		t.Fatal("a write did not flush the collection's memo")
	}
	if _, ok := sess.Doc("media", "9"); !ok {
		t.Fatal("flushing one collection dropped another")
	}
}

func TestSessionWarningsAreDeduplicated(t *testing.T) {
	sess := NewSession(New(""), mustScope(t, base()), SessionOptions{})
	w := output.Warning{Code: WarnCacheUnreadable, Message: "/tmp/x: permission denied"}
	sess.AddWarnings(w, w, w)
	sess.AddWarnings(output.Warning{Code: WarnCacheWriteFailed, Message: "/tmp/y: read-only"})
	if got := sess.Warnings(); len(got) != 2 {
		t.Fatalf("warnings = %+v, want 2 after de-duplication", got)
	}
}

func TestSessionCollectsReadWarnings(t *testing.T) {
	s, sc := newTestStore(t)
	mustWriteFile(t, s.ManifestPath(sc), []byte("{"))
	sess := NewSession(s, sc, SessionOptions{})
	if _, ok := sess.LoadManifest(); ok {
		t.Fatal("a corrupt manifest was served")
	}
	warns := sess.Warnings()
	if len(warns) != 1 || warns[0].Code != WarnCacheUnreadable {
		t.Fatalf("warnings = %+v", warns)
	}
}

// TestSessionSingleflight is §8.6: one /api/access for a command touching three
// collections, not three.
func TestSessionSingleflight(t *testing.T) {
	sess := NewSession(New(""), mustScope(t, base()), SessionOptions{})
	key := FlightKey("get", "http://localhost:3900/api/access", nil)

	var calls, sharedCount atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]any, 16)

	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, shared, err := sess.Do(key, func() (any, error) {
				calls.Add(1)
				<-release
				return "answer", nil
			})
			if err != nil {
				t.Errorf("Do: %v", err)
			}
			results[i] = v
			if shared {
				sharedCount.Add(1)
			}
		}(i)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("fn ran %d times, want 1", got)
	}
	if got := sharedCount.Load(); got != 15 {
		t.Fatalf("%d callers reported a shared result, want 15", got)
	}
	for i, v := range results {
		if v != "answer" {
			t.Fatalf("caller %d got %v", i, v)
		}
	}

	// The memo persists for the rest of the process.
	v, shared, err := sess.Do(key, func() (any, error) { return "recomputed", nil })
	if err != nil || v != "answer" || !shared {
		t.Fatalf("Do after completion = (%v, %v, %v)", v, shared, err)
	}

	// A different key is a different flight.
	other, _, _ := sess.Do(FlightKey("GET", "http://localhost:3900/api/pages", []byte(`{"a":1}`)), func() (any, error) {
		return "other", nil
	})
	if other != "other" {
		t.Fatalf("a different key reused a result: %v", other)
	}
}

func TestFlightKey(t *testing.T) {
	a := FlightKey("get", "http://x/api/pages", []byte(`{"a":1}`))
	b := FlightKey("GET", "http://x/api/pages", []byte(`{"a":1}`))
	c := FlightKey("GET", "http://x/api/pages", []byte(`{"a":2}`))
	if a != b {
		t.Fatal("the method case changed the flight key")
	}
	if a == c {
		t.Fatal("the body did not change the flight key")
	}
	if strings.Contains(a, `{"a":1}`) {
		t.Fatalf("the flight key embeds the raw body: %s", a)
	}
}

// TestSessionIsGoroutineSafe hammers every memo at once under -race.
func TestSessionIsGoroutineSafe(t *testing.T) {
	s, sc := newTestStore(t)
	writeFixture(t, s, sc, NewGeneration(testNow), "pages")
	sess := NewSession(s, sc, SessionOptions{})

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sess.LoadManifest()
			sess.MarkNegative("k")
			sess.Negative("k")
			sess.SetIdentity(i)
			sess.Identity()
			sess.SetTopology("t")
			sess.Topology()
			sess.PutDoc("pages", "1", i)
			sess.Doc("pages", "1")
			sess.FlushCollection("pages")
			sess.AddWarnings(output.Warning{Code: WarnCacheUnreadable, Message: "x"})
			sess.Warnings()
			sess.SetDiscoveryMode("hit")
			sess.DiscoveryMode()
			sess.AllowRediscovery()
			sess.Do("k", func() (any, error) { return i, nil })
		}(i)
	}
	wg.Wait()
}

// syntheticManifest builds the §8.2 benchmark fixture: collections × fields.
func syntheticManifest(t testing.TB, s *Store, sc Scope, collections, fields int) string {
	t.Helper()
	gen := NewGeneration(testNow)
	colls := make([]any, 0, collections)
	shards := map[string]any{}
	for c := 0; c < collections; c++ {
		slug := "collection-" + strconv.Itoa(c)
		colls = append(colls, map[string]any{
			"slug":          slug,
			"labels":        map[string]any{"singular": slug, "plural": slug + "s", "source": "synthetic"},
			"graphql":       map[string]any{"singular": slug, "plural": slug + "s", "count": "count"},
			"id_type":       "number",
			"reachability":  "ok",
			"internal":      false,
			"publishable":   true,
			"flags":         map[string]any{"upload": false, "auth": false, "versions": true, "drafts": true},
			"permissions":   map[string]any{"create": true, "read": true, "update": true, "delete": true},
			"fields_count":  fields,
			"fields_sha256": "sha-" + slug,
			"fields_shard":  ShardName(slug, KindCollection),
			"title_field":   "title",
			"date_fields":   []any{"createdAt", "updatedAt"},
			"warnings":      []any{},
		})
		entries := make([]any, 0, fields)
		for f := 0; f < fields; f++ {
			name := "field" + strconv.Itoa(f)
			entries = append(entries, map[string]any{
				"name": name, "path": name, "graphql_path": name, "parent": nil,
				"payload_type": "text", "payload_type_confidence": "graphql",
				"graphql_type": "String", "json_type": "string",
				"required": false, "required_source": "graphql-input",
				"has_many": false, "localized": nil, "localized_source": "unknown",
				"read_only": false, "options": nil, "options_source": "n/a",
				"relation_to": nil, "relation_to_source": "n/a", "polymorphic": false,
				"write_shape": nil, "queryable": true,
				"operators": []any{"equals", "not_equals", "in", "not_in", "exists"},
				"sortable":  true, "sortable_confidence": "heuristic",
				"hook_mutated": "unknown", "label": name,
			})
		}
		shards[ShardName(slug, KindCollection)] = map[string]any{
			"generation": gen, "slug": slug, "sha256": "sha-" + slug,
			"fields": entries, "join_fields": []any{}, "blocks": nil,
			"blocks_source": "unknown", "required_paths": []any{},
		}
	}
	doc := manifestDoc(sc, gen)
	doc["collections"] = colls
	if ok, warns := s.WriteSet(sc, Set{Generation: gen, Manifest: doc, Shards: shards}, testNow); !ok {
		t.Fatalf("WriteSet: %+v", warnText(warns))
	}
	return gen
}

func TestLargeManifestRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a 150-collection fixture")
	}
	s, sc := newTestStore(t)
	gen := syntheticManifest(t, s, sc, 150, 20)

	m, ok, warn := s.ReadManifest(sc)
	if !ok {
		t.Fatalf("miss: %+v", warn)
	}
	if len(m.Collections) != 150 || m.Generation != gen {
		t.Fatalf("unexpected manifest: %d collections", len(m.Collections))
	}
	ref, ok := m.Entity("collection-149", KindCollection)
	if !ok || ref.FieldsShard != "fields/collection-149.json" {
		t.Fatalf("target resolution failed: %+v", ref)
	}
	if _, ok, warn := s.ReadShard(sc, m, "collection-149", KindCollection); !ok {
		t.Fatalf("shard miss: %+v", warn)
	}
}

// BenchmarkWarmPathResolution is §8.2's CI benchmark: index decode + one shard
// decode + target resolution on a synthetic 150-collection × 200-field manifest
// must stay under 5 ms. It fails the build rather than filing a report, which is
// why the assertion lives in the benchmark body.
func BenchmarkWarmPathResolution(b *testing.B) {
	s := New(b.TempDir())
	s.DisableGC = true
	sc, err := NewScope(base())
	if err != nil {
		b.Fatal(err)
	}
	syntheticManifest(b, s, sc, 150, 200)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m, ok, _ := s.ReadManifest(sc)
		if !ok {
			b.Fatal("manifest miss")
		}
		if _, ok := m.Entity("collection-75", KindCollection); !ok {
			b.Fatal("target resolution miss")
		}
		if _, ok, _ := s.ReadShard(sc, m, "collection-75", KindCollection); !ok {
			b.Fatal("shard miss")
		}
	}
	b.StopTimer()

	if per := b.Elapsed() / time.Duration(b.N); per > 5*time.Millisecond {
		b.Fatalf("warm-path resolution took %s per operation, budget is 5ms (§8.2)", per)
	}
}
