package cli

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// censusWarning returns the first warning with code, nil when absent.
func censusWarning(t *testing.T, r cliResult, code string) map[string]any {
	t.Helper()
	for _, w := range r.warnings(t) {
		if w["code"] == code {
			return w
		}
	}
	return nil
}

// allEntitiesServer answers every collection list and global read of the
// fixture manifest: pages with the census fixture documents, everything else
// with no block rows. It counts reads per entity.
func allEntitiesServer(t *testing.T) *payloadtest.Server {
	t.Helper()
	var m discovery.Manifest
	payloadtest.LoadJSON(t, "manifest", &m)
	pages := `{"docs":[` + strings.Join(censusPageDocs(), ",") + `],"totalDocs":4,"limit":100,"page":1,"hasNextPage":false}`
	empty := `{"docs":[{"id":1,"title":"x"}],"totalDocs":1,"limit":100,"page":1,"hasNextPage":false}`
	var opts []payloadtest.Option
	for _, c := range m.Collections {
		body := empty
		if c.Slug == "pages" {
			body = pages
		}
		opts = append(opts, payloadtest.WithHandler("GET", "/api/"+c.Slug, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
	}
	// `pay blocks example` fetches the referenced documents live.
	for _, doc := range censusPageDocs() {
		var head struct {
			ID json.Number `json:"id"`
		}
		_ = json.Unmarshal([]byte(doc), &head)
		body := doc
		opts = append(opts, payloadtest.WithHandler("GET", "/api/pages/"+head.ID.String(), func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
	}
	for _, g := range m.Globals {
		opts = append(opts, payloadtest.WithHandler("GET", "/api/globals/"+g.Slug, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":1}`))
		}))
	}
	return payloadtest.NewServer(t, opts...)
}

// Without --collection, a per-type verb used to read EVERY entity that can
// hold blocks. With a cached census that saw the type, it consults only the
// holders; the first, uncached run states what the wide read cost.
func TestPerTypeVerbsConsultTheCachedCensusFirst(t *testing.T) {
	srv := allEntitiesServer(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	run := func(args ...string) cliResult {
		t.Helper()
		r := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), Args: args})
		if r.Code != 0 {
			t.Fatalf("%v: exit %d\n%s", args, r.Code, r.Stdout)
		}
		return r
	}

	// 1. Cold: a wide read, and the warning says what it cost and how to
	//    read one entity instead.
	r := run("blocks", "schema", "hero")
	census, _ := r.data(t)["census"].(map[string]any)
	if census["scope"] != wideScopeNote {
		t.Errorf("cold scope = %v", census["scope"])
	}
	w := censusWarning(t, r, warnCensusScopeWide)
	if w == nil {
		t.Fatalf("a wide read must state its cost: %v", r.warnings(t))
	}
	if hint, _ := w["hint"].(string); hint != "pay blocks schema hero --collection pages reads one entity" {
		t.Errorf("hint = %q", hint)
	}
	if !strings.Contains(w["message"].(string), "requests") {
		t.Errorf("message = %v", w["message"])
	}
	cold := len(srv.Requests())

	// 2. Warm: the cached census knows hero lives in pages, so nothing else
	//    is consulted and nothing is read.
	srv.Reset()
	r = run("blocks", "schema", "hero")
	census, _ = r.data(t)["census"].(map[string]any)
	scope, _ := census["scope"].(string)
	if !strings.HasPrefix(scope, "narrowed:") || !strings.Contains(scope, "pages") {
		t.Errorf("warm scope = %q", scope)
	}
	if census["requests"] != json.Number("0") || len(census["entities"].([]any)) != 1 {
		t.Errorf("warm census = %v", census)
	}
	if n := srv.Count("GET", "/api/"); n > 0 {
		t.Errorf("a warm narrowed answer read %d times (cold run read %d)", n, cold)
	}
	if censusWarning(t, r, warnCensusScopeWide) != nil {
		t.Error("a narrowed answer is not a wide read")
	}
	// The other per-type verbs narrow the same way.
	for _, verb := range []string{"new", "example"} {
		srv.Reset()
		r = run("blocks", verb, "text")
		census, _ = r.data(t)["census"].(map[string]any)
		if n := srv.Count("GET", "/api/posts"); n > 0 {
			t.Errorf("%s read posts", verb)
		}
		if s, _ := census["scope"].(string); !strings.HasPrefix(s, "narrowed:") {
			t.Errorf("%s scope = %q", verb, s)
		}
	}

	// 3. An unknown type still answers block_type_unknown after the wide
	//    fallback, from the (fresh) cache, without a network read.
	srv.Reset()
	bad := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), Args: []string{"blocks", "schema", "heroo"}})
	if bad.code(t) != "block_type_unknown" {
		t.Errorf("unknown type: %s", bad.Stdout)
	}
	if n := srv.Count("GET", "/api/"); n > 0 {
		t.Errorf("an unknown type against a fresh census read %d times", n)
	}
}

// A wide scan reuses an entity whose (stale but unexpired) census held no
// blocks; naming the entity, or `learn`, re-reads it.
func TestWideScanReusesBlocklessEntities(t *testing.T) {
	srv := allEntitiesServer(t)
	home := t.TempDir()
	sc := seedDiscovery(t, home, srv.URL)
	run := func(args ...string) cliResult {
		t.Helper()
		r := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), Args: args})
		if r.Code != 0 {
			t.Fatalf("%v: exit %d\n%s", args, r.Code, r.Stdout)
		}
		return r
	}
	run("blocks", "types") // cold: builds every target
	store := cache.New(filepath.Join(home, "cache"))
	raw, ok, _ := store.ReadCensus(sc)
	if !ok {
		t.Fatal("no census written")
	}
	c, _ := discovery.DecodeCensus(raw)
	posts, ok := c.Entity(discovery.EntityKindCollection, "posts")
	if !ok || posts.BlockRows != 0 {
		t.Fatalf("posts census = %+v", posts)
	}
	// Two days old: past the 24 h freshness, inside the 7-day hard max.
	for _, e := range c.Entities() {
		e.BuiltAt = payloadtest.Epoch.Add(-48 * time.Hour)
	}
	if ok, w := store.WriteCensus(sc, c); !ok {
		t.Fatalf("rewrite census: %v", w)
	}

	srv.Reset()
	r := run("blocks", "types")
	if n := srv.Count("GET", "/api/posts"); n != 0 {
		t.Errorf("a stale blockless entity was re-read %d times by a wide scan", n)
	}
	if n := srv.Count("GET", "/api/pages"); n == 0 {
		t.Error("the stale entity WITH blocks must be re-read")
	}
	if censusWarning(t, r, warnCensusScopeWide) == nil {
		t.Error("the rebuild of pages cost requests; the warning must say so")
	}

	srv.Reset()
	run("blocks", "types", "posts")
	if n := srv.Count("GET", "/api/posts"); n == 0 {
		t.Error("naming the entity must re-read a stale census")
	}
}

func TestBlocklessReusable(t *testing.T) {
	now := payloadtest.Epoch
	fresh := func() *discovery.CensusEntity {
		e := discovery.NewCensusEntity(discovery.EntityKindCollection, "posts")
		e.BuiltAt = now.Add(-48 * time.Hour)
		return e
	}
	if !blocklessReusable(fresh(), nil, 500, now) {
		t.Error("a 2-day-old blockless entity is reusable by a wide scan")
	}
	e := fresh()
	e.BuiltAt = now.Add(-8 * 24 * time.Hour)
	if blocklessReusable(e, nil, 500, now) {
		t.Error("past the hard max it is not")
	}
	e = fresh()
	e.BlockRows = 1
	if blocklessReusable(e, nil, 500, now) {
		t.Error("an entity with block rows is not")
	}
	e = fresh()
	e.Error = "boom"
	if blocklessReusable(e, nil, 500, now) {
		t.Error("a failed read is not")
	}
	e = fresh()
	e.Truncated, e.MaxDocs = true, 100
	if blocklessReusable(e, nil, 500, now) {
		t.Error("a truncated read below the requested budget is not")
	}
	e.StoppedEarly = true
	if !blocklessReusable(e, nil, 500, now) {
		t.Error("an early stop after a blockless page is")
	}
	shard := &discovery.Shard{Fields: []discovery.Field{{Path: "layout", PayloadType: discovery.TypeBlocks}}}
	if blocklessReusable(fresh(), shard, 500, now) {
		t.Error("a schema that declares a blocks field always re-reads")
	}
}

func TestMergeCensusReports(t *testing.T) {
	first := &censusReport{Requests: 4, Entities: []censusEntityReport{{Kind: "collection", Slug: "pages", Requests: 4}}}
	second := &censusReport{Requests: 3, Entities: []censusEntityReport{
		{Kind: "collection", Slug: "pages", FromCache: true},
		{Kind: "collection", Slug: "posts", Requests: 3},
	}}
	got := mergeCensusReports(first, second)
	if got.Requests != 7 || got.Source != "built" {
		t.Errorf("merged = %+v", got)
	}
	if got.Entities[0].FromCache || got.Entities[0].Requests != 4 {
		t.Errorf("pages was read in this invocation: %+v", got.Entities[0])
	}
}
