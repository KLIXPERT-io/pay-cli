package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/payload"
)

// censusAPI is a minimal Payload REST fake for the census reader.
type censusAPI struct {
	mu   sync.Mutex
	seen []*http.Request
	// collections maps a slug to all of its documents.
	collections map[string][]map[string]any
	// depth1 maps "slug/id" to the document as read at depth 1.
	depth1  map[string]map[string]any
	globals map[string]map[string]any
	broken  map[string]bool
}

func (f *censusAPI) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.seen = append(f.seen, r)
	f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api/")
	q := r.URL.Query()
	respond := func(status int, v any) (*http.Response, error) {
		b, _ := json.Marshal(v)
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(string(b))), Request: r}, nil
	}
	if strings.HasPrefix(path, "globals/") {
		return respond(200, f.globals[strings.TrimPrefix(path, "globals/")])
	}
	slug, id, _ := strings.Cut(path, "/")
	if f.broken[slug] {
		return respond(500, map[string]any{"errors": []any{map[string]any{"message": "Something went wrong."}}})
	}
	if id != "" {
		return respond(200, f.depth1[slug+"/"+id])
	}
	docs := f.collections[slug]
	if q.Get("sort") == "id" {
		// The fixture lists documents newest first (Payload's default
		// order); sort=id reads them oldest first.
		docs = append([]map[string]any{}, docs...)
		sort.SliceStable(docs, func(i, j int) bool { return fmt.Sprint(docs[i]["id"]) < fmt.Sprint(docs[j]["id"]) })
		if allIntIDs(docs) {
			sort.SliceStable(docs, func(i, j int) bool { return docs[i]["id"].(int) < docs[j]["id"].(int) })
		}
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit < 1 {
		limit = 10 // Payload's default page size
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	// Payload's offset is (page-1)*limit: a later page asked for with a
	// smaller limit overlaps the rows already read.
	start := (page - 1) * limit
	if start > len(docs) {
		start = len(docs)
	}
	end := start + limit
	if end > len(docs) {
		end = len(docs)
	}
	out := map[string]any{"docs": docs[start:end], "totalDocs": len(docs), "limit": limit, "page": page,
		"hasNextPage": end < len(docs)}
	if end < len(docs) {
		out["nextPage"] = page + 1
	}
	return respond(200, out)
}

func allIntIDs(docs []map[string]any) bool {
	for _, d := range docs {
		if _, ok := d["id"].(int); !ok {
			return false
		}
	}
	return true
}

func (f *censusAPI) requests(prefix string) []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*http.Request
	for _, r := range f.seen {
		if strings.HasPrefix(r.URL.Path, prefix) {
			out = append(out, r)
		}
	}
	return out
}

func censusClient(t *testing.T, f *censusAPI) *payload.Client {
	t.Helper()
	cli, err := payload.New(payload.Config{
		BaseURL: "http://payload.test", APIPath: "/api",
		AuthMode: payload.AuthModeAPIKey, AuthCollection: "users", Credential: testAPIKey,
		RoundTripper: f, Now: fixedClock(), Concurrency: 4,
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return cli
}

func TestRunCensusReadsPagesGlobalsAndSurvivesFailures(t *testing.T) {
	var pages []map[string]any
	for i := 1; i <= 120; i++ {
		pages = append(pages, map[string]any{"id": i, "layout": []any{
			map[string]any{"id": fmt.Sprint("h", i), "blockType": "hero", "title": fmt.Sprint("T", i), "image": i % 7},
		}})
	}
	var media []map[string]any
	for i := 1; i <= 250; i++ {
		media = append(media, map[string]any{"id": i, "sizes": map[string]any{"thumb": map[string]any{"url": "/x"}}})
	}
	f := &censusAPI{
		collections: map[string][]map[string]any{"pages": pages, "media": media},
		depth1: map[string]map[string]any{"pages/1": {"id": 1, "layout": []any{
			map[string]any{"blockType": "hero", "image": map[string]any{"id": 1, "filename": "a.png", "mimeType": "image/png"}}}}},
		globals: map[string]map[string]any{"header": {"nav": []any{map[string]any{"blockType": "link", "label": "Home"}}}},
		broken:  map[string]bool{"broken": true},
	}
	targets := []CensusTarget{
		{Kind: EntityKindCollection, Slug: "pages", Draft: true, Select: []string{"layout"}},
		{Kind: EntityKindCollection, Slug: "media", Select: []string{"sizes"}},
		{Kind: EntityKindCollection, Slug: "broken", Draft: true},
		{Kind: EntityKindGlobal, Slug: "header", Draft: true},
	}
	got := RunCensus(context.Background(), CensusOptions{Client: censusClient(t, f), Now: fixedClock(), MaxDocs: 150, Concurrency: 2}, targets)

	p := got[0]
	if p.DocsScanned != 120 || p.Truncated || p.Types["hero"].Count != 120 || p.Error != "" {
		t.Fatalf("pages = %+v", p)
	}
	if p.RelationProbe != "ok" || p.Types["hero"].Fields["image"].Kind != CensusKindUpload {
		t.Errorf("depth-1 probe: %s %+v", p.RelationProbe, p.Types["hero"].Fields["image"])
	}
	reqs := f.requests("/api/pages")
	if len(reqs) < 3 || p.Requests != len(reqs) {
		t.Fatalf("pages requests = %d (entity says %d)", len(reqs), p.Requests)
	}
	first := reqs[0].URL.Query()
	if first.Get("depth") != "0" || first.Get("draft") != "true" || first.Get("limit") != "100" || first.Get("select[layout]") != "true" {
		t.Errorf("list query = %s", reqs[0].URL.RawQuery)
	}
	var depth1 int
	for _, r := range reqs {
		if r.URL.Query().Get("depth") == "1" {
			depth1++
		}
	}
	if depth1 == 0 || depth1 > relationProbeDocs {
		t.Errorf("depth-1 reads = %d, want 1..%d", depth1, relationProbeDocs)
	}

	m := got[1]
	if !m.StoppedEarly || !m.Truncated || m.DocsScanned != 100 || m.Requests != 2 || m.Draft {
		t.Errorf("a collection without blocks must stop after one page plus the oldest-first sample: %+v", m)
	}
	if b := got[2]; b.Error == "" || b.DocsScanned != 0 {
		t.Errorf("broken entity = %+v", b)
	}
	if h := got[3]; h.Kind != EntityKindGlobal || h.Types["link"] == nil || h.Types["link"].Instances[0].ID != nil {
		t.Errorf("global = %+v", h)
	}
}

func TestRunCensusHonoursMaxDocs(t *testing.T) {
	var pages []map[string]any
	for i := 1; i <= 30; i++ {
		pages = append(pages, map[string]any{"id": i, "layout": []any{map[string]any{"blockType": "text"}}})
	}
	f := &censusAPI{collections: map[string][]map[string]any{"pages": pages}}
	got := RunCensus(context.Background(), CensusOptions{Client: censusClient(t, f), MaxDocs: 10},
		[]CensusTarget{{Kind: EntityKindCollection, Slug: "pages"}})
	if got[0].DocsScanned != 10 || !got[0].Truncated || got[0].MaxDocs != 10 {
		t.Fatalf("got %+v", got[0])
	}
	if q := f.requests("/api/pages")[0].URL.Query(); q.Get("limit") != "10" || q.Has("draft") {
		t.Errorf("query = %v", q)
	}
}

func TestCensusTargets(t *testing.T) {
	m := NewManifest()
	yes, no := true, false
	m.Collections = []*Collection{
		{Slug: "pages", FieldsSource: SourceObserved, Flags: Flags{Drafts: &yes}},
		{Slug: "posts", FieldsSource: SourceGraphQL, Flags: Flags{Drafts: &yes}},
		{Slug: "forms", FieldsSource: SourceGraphQL, Flags: Flags{Drafts: &no}},
		{Slug: "payload-preferences", Internal: true},
		{Slug: "gone", Flags: Flags{EndpointsDisabled: &yes}},
		{Slug: "tags", FieldsSource: SourceObserved},
	}
	m.Globals = []*Global{{Slug: "header"}}
	shards := map[string]*Shard{}
	pages := NewShard("g", "pages")
	pages.Fields = []Field{
		{Path: "title", PayloadType: TypeText, JSONType: "string"},
		{Path: "layout", PayloadType: TypeUnknown, JSONType: "array"},
		{Path: "meta", PayloadType: TypeGroup, JSONType: "object"},
		{Path: "content", PayloadType: TypeRichText, JSONType: "object"},
		{Path: "parent", PayloadType: TypeNumber, JSONType: "number"},
	}
	shards["pages"] = pages
	posts := NewShard("g", "posts")
	posts.Fields = []Field{{Path: "content", PayloadType: TypeRichText}, {Path: "meta.image", PayloadType: TypeUpload}}
	shards["posts"] = posts
	forms := NewShard("g", "forms")
	forms.Fields = []Field{{Path: "fields", PayloadType: TypeBlocks}, {Path: "title", PayloadType: TypeText}}
	shards["forms"] = forms
	tags := NewShard("g", "tags")
	tags.Fields = []Field{{Path: "name", PayloadType: TypeText, JSONType: "string"}}
	shards["tags"] = tags

	lookup := func(kind, slug string) *Shard { return shards[slug] }
	got := CensusTargets(m, lookup, nil)
	var names []string
	for _, t := range got {
		names = append(names, t.Kind+":"+t.Slug+"="+strings.Join(t.Select, "+")+fmt.Sprint("/draft=", t.Draft))
	}
	want := "collection:forms=fields/draft=false collection:pages=layout+meta/draft=true global:header=/draft=true"
	if strings.Join(names, " ") != want {
		t.Fatalf("targets = %v\nwant    %s", names, want)
	}
	only := CensusTargets(m, lookup, []string{"tags"})
	if len(only) != 1 || only[0].Slug != "tags" {
		t.Errorf("an explicitly named entity is read even without candidate fields: %+v", only)
	}
}

// A budget that is not a multiple of the page size must neither re-read nor
// skip documents: Payload's offset is (page-1)*limit.
func TestRunCensusPagingWithOddBudget(t *testing.T) {
	var pages []map[string]any
	for i := 250; i >= 1; i-- {
		pages = append(pages, map[string]any{"id": i, "layout": []any{
			map[string]any{"blockType": "hero", "title": fmt.Sprint("T", i)}}})
	}
	f := &censusAPI{collections: map[string][]map[string]any{"pages": pages}}
	got := RunCensus(context.Background(), CensusOptions{Client: censusClient(t, f), MaxDocs: 150, NoRelationProbe: true},
		[]CensusTarget{{Kind: EntityKindCollection, Slug: "pages"}})[0]
	if got.DocsScanned != 150 || !got.Truncated || got.Types["hero"].Count != 150 || got.BlockRows != 150 {
		t.Fatalf("150 of 250 documents: %+v", got)
	}
	for _, r := range f.requests("/api/pages") {
		if r.URL.Query().Get("limit") != "100" {
			t.Errorf("every page must keep limit=100 so offsets line up: %s", r.URL.RawQuery)
		}
	}

	// 120 documents under a 150 budget: complete, each counted once.
	f2 := &censusAPI{collections: map[string][]map[string]any{"pages": pages[:120]}}
	got = RunCensus(context.Background(), CensusOptions{Client: censusClient(t, f2), MaxDocs: 150, NoRelationProbe: true},
		[]CensusTarget{{Kind: EntityKindCollection, Slug: "pages"}})[0]
	if got.DocsScanned != 120 || got.Truncated || got.Types["hero"].Count != 120 {
		t.Fatalf("120 of 120 documents: %+v", got)
	}
}

// Stub-first migrations leave the NEWEST page of a blocks collection empty.
// The early stop must look at the oldest documents before calling the
// collection blockless.
func TestRunCensusEarlyStopSamplesOldest(t *testing.T) {
	var pages []map[string]any
	for i := 150; i >= 1; i-- { // newest first
		doc := map[string]any{"id": i, "layout": []any{}}
		if i <= 50 {
			doc["layout"] = []any{map[string]any{"blockType": "hero", "title": fmt.Sprint("T", i)}}
		}
		pages = append(pages, doc)
	}
	f := &censusAPI{collections: map[string][]map[string]any{"pages": pages}}
	got := RunCensus(context.Background(), CensusOptions{Client: censusClient(t, f), MaxDocs: 500, NoRelationProbe: true},
		[]CensusTarget{{Kind: EntityKindCollection, Slug: "pages"}})[0]
	if got.StoppedEarly || got.Types["hero"] == nil || got.Types["hero"].Count != 50 || got.DocsScanned != 150 || got.Truncated {
		t.Fatalf("a blocks collection whose newest 100 documents are stubs: %+v", got)
	}
	sorted := 0
	for _, r := range f.requests("/api/pages") {
		if r.URL.Query().Get("sort") == "id" {
			sorted++
		}
	}
	if sorted != 1 {
		t.Errorf("oldest-first samples = %d, want 1", sorted)
	}
}
