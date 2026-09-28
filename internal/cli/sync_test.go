package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/docdiff"
	"github.com/KLIXPERT-io/pay-cli/internal/manifest"
)

// ---------------------------------------------------------------------------
// memCMS: an in-memory Payload with drafts, just enough for F6
// ---------------------------------------------------------------------------
//
// pages and posts have drafts (the fixture manifest says so), categories do
// not; header is a global. A drafts document keeps two states — main (what a
// plain read returns) and latest (what ?draft=true returns) — and a write
// follows payload@3.87.1's update.js: ?draft=true with a body _status other
// than "published" saves the latest version only; anything else saves both.
// A body whose title is "FAIL" is rejected with a ValidationError.

type memDoc struct {
	main, latest map[string]any
}

type memCMS struct {
	mu     sync.Mutex
	colls  map[string]map[string]*memDoc
	header map[string]any
	next   int
	writes []string
	bodies []map[string]any
	srv    *httptest.Server
	nFinds int
}

var memDrafts = map[string]bool{"pages": true, "posts": true}

// memNow is what memCMS's populatePublishedAt hook writes.
const memNow = "2099-01-01T00:00:00.000Z"

func newMemCMS(t *testing.T) *memCMS {
	t.Helper()
	m := &memCMS{colls: map[string]map[string]*memDoc{"pages": {}, "posts": {}, "categories": {}, "media": {}, "users": {}},
		header: map[string]any{"id": json.Number("1"), "globalType": "header", "navItems": []any{}},
		next:   100}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.srv.Close)
	return m
}

// seed stores a document in both states.
func (m *memCMS) seed(coll string, id int, doc map[string]any) {
	doc["id"] = json.Number(strconv.Itoa(id))
	cp := cloneAny(doc).(map[string]any)
	m.colls[coll][strconv.Itoa(id)] = &memDoc{main: doc, latest: cp}
}

func cloneAny(v any) any {
	b, _ := json.Marshal(v)
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	var out any
	_ = dec.Decode(&out)
	return out
}

func memJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Powered-By", "Payload")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// matchWhere evaluates the subset of Payload's where grammar the CLI sends.
func matchWhere(doc map[string]any, w map[string]any) bool {
	for k, v := range w {
		switch k {
		case "and":
			for _, sub := range v.([]any) {
				if !matchWhere(doc, sub.(map[string]any)) {
					return false
				}
			}
		case "or":
			ok := false
			for _, sub := range v.([]any) {
				if matchWhere(doc, sub.(map[string]any)) {
					ok = true
				}
			}
			if !ok {
				return false
			}
		default:
			got, _ := manifest.Lookup(doc, k)
			for op, want := range v.(map[string]any) {
				switch op {
				case "equals":
					if fmt.Sprint(got) != fmt.Sprint(want) {
						return false
					}
				case "in":
					found := false
					for _, x := range want.([]any) {
						if fmt.Sprint(got) == fmt.Sprint(x) {
							found = true
						}
					}
					if !found {
						return false
					}
				case "like":
					if !strings.Contains(fmt.Sprint(got), fmt.Sprint(want)) {
						return false
					}
				default:
					return false
				}
			}
		}
	}
	return true
}

func (m *memCMS) serve(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	if len(raw) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.UseNumber()
		_ = dec.Decode(&body)
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/"), "/")
	q := r.URL.Query()
	draft := q.Get("draft") == "true"
	if r.Method != http.MethodGet {
		m.writes = append(m.writes, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		recorded, _ := cloneAny(body).(map[string]any)
		m.bodies = append(m.bodies, recorded)
	}
	if parts[0] == "globals" && len(parts) == 2 && parts[1] == "header" {
		switch r.Method {
		case http.MethodGet:
			memJSON(w, 200, m.header)
		case http.MethodPost:
			for k, v := range body {
				m.header[k] = v
			}
			memJSON(w, 200, map[string]any{"message": "Updated successfully.", "result": m.header})
		}
		return
	}
	docs, ok := m.colls[parts[0]]
	if !ok {
		memJSON(w, 404, map[string]any{"message": "Route not found \"" + r.URL.Path + "\""})
		return
	}
	view := func(d *memDoc) map[string]any {
		if draft {
			return d.latest
		}
		return d.main
	}
	fail := func() {
		memJSON(w, 400, map[string]any{"errors": []map[string]any{{
			"name": "ValidationError", "message": "The following field is invalid: Title",
			"data": map[string]any{"errors": []map[string]any{{"path": "title", "message": "no FAIL please"}}},
		}}})
	}
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		m.nFinds++
		var where map[string]any
		if s := q.Get("where"); s != "" {
			_ = json.Unmarshal([]byte(s), &where)
		}
		ids := make([]int, 0, len(docs))
		for id := range docs {
			n, _ := strconv.Atoi(id)
			ids = append(ids, n)
		}
		sort.Ints(ids)
		out := []any{}
		for _, n := range ids {
			d := view(docs[strconv.Itoa(n)])
			if where == nil || matchWhere(d, where) {
				out = append(out, d)
			}
		}
		total := len(out)
		if lim, err := strconv.Atoi(q.Get("limit")); err == nil && lim > 0 && len(out) > lim {
			out = out[:lim]
		}
		memJSON(w, 200, map[string]any{"docs": out, "totalDocs": total, "limit": 2, "page": 1,
			"totalPages": 1, "hasNextPage": false})
	case len(parts) == 1 && r.Method == http.MethodPost:
		if body["title"] == "FAIL" {
			fail()
			return
		}
		if body["title"] == "BOOM" {
			memJSON(w, 500, map[string]any{"errors": []map[string]any{{"message": "Something went wrong."}}})
			return
		}
		if parts[0] == "categories" {
			// The website template's formatSlug hook, and a unique index.
			if s, ok := body["slug"].(string); ok {
				body["slug"] = strings.ReplaceAll(strings.ToLower(s), " ", "-")
			}
			for _, d := range docs {
				if d.main["slug"] == body["slug"] {
					memJSON(w, 400, map[string]any{"errors": []map[string]any{{
						"name": "ValidationError", "message": "The following field is invalid: slug",
						"data": map[string]any{"errors": []map[string]any{{"path": "slug", "message": "Value must be unique"}}},
					}}})
					return
				}
			}
		}
		m.newRows(body)
		m.next++
		id := strconv.Itoa(m.next)
		body["id"] = json.Number(id)
		if memDrafts[parts[0]] {
			if draft || body["_status"] == nil {
				body["_status"] = "draft"
			}
		}
		docs[id] = &memDoc{main: body, latest: cloneAny(body).(map[string]any)}
		memJSON(w, 201, map[string]any{"doc": body, "message": "created"})
	case len(parts) == 2:
		d, ok := docs[parts[1]]
		if !ok {
			memJSON(w, 404, map[string]any{"errors": []map[string]any{{"message": "Not Found"}}})
			return
		}
		switch r.Method {
		case http.MethodGet:
			memJSON(w, 200, view(d))
		case http.MethodPatch:
			if body["title"] == "FAIL" {
				fail()
				return
			}
			// Payload's PATCH semantics (docdiff.ApplyPatch models them):
			// a row whose id matches is merged, any other row is new — it
			// gets a fresh id and its unsent keys their defaults.
			next := docdiff.ApplyPatch(d.latest, body, docdiff.Options{})
			m.newRows(next)
			// The website template's populatePublishedAt: a request
			// without publishedAt gets "now" (req.data decides, not the
			// stored value).
			if _, sent := body["publishedAt"]; !sent && parts[0] == "pages" && d.latest["publishedAt"] != nil {
				next["publishedAt"] = memNow
			}
			if memDrafts[parts[0]] && draft && body["_status"] != "published" {
				next["_status"] = "draft"
				d.latest = next
			} else {
				d.latest = next
				d.main = cloneAny(next).(map[string]any)
			}
			memJSON(w, 200, map[string]any{"doc": next, "message": "Updated successfully."})
		}
	}
}

// newRows gives every array row without an id a fresh one, and a link in
// such a row the defaultValue Payload's website template declares
// (appearance "default"): what Payload does to a row it creates.
func (m *memCMS) newRows(v any) {
	switch t := v.(type) {
	case map[string]any:
		for _, c := range t {
			m.newRows(c)
		}
	case []any:
		for _, r := range t {
			row, ok := r.(map[string]any)
			if !ok {
				continue
			}
			if row["id"] == nil {
				m.next++
				row["id"] = fmt.Sprintf("row%d", m.next)
				if link, ok := row["link"].(map[string]any); ok {
					if _, has := link["appearance"]; !has {
						link["appearance"] = "default"
					}
				}
			}
			m.newRows(row)
		}
	}
}

// memRun runs pay against the memCMS with a seeded discovery cache.
func (m *memCMS) run(t *testing.T, home string, env []string, args ...string) cliResult {
	t.Helper()
	return cliRun(t, invocation{Home: home, Env: append(seededEnv(m.srv.URL), env...), Args: args})
}

func (m *memCMS) home(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	seedDiscovery(t, home, m.srv.URL)
	return home
}

func writeManifest(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func planItems(t *testing.T, r cliResult) []map[string]any {
	t.Helper()
	d := r.data(t)
	raw, _ := d["items"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		out = append(out, it.(map[string]any))
	}
	return out
}

func itemByKey(t *testing.T, items []map[string]any, key string) map[string]any {
	t.Helper()
	for _, it := range items {
		if it["key"] == key {
			return it
		}
	}
	t.Fatalf("no item %q in %v", key, items)
	return nil
}

// siteManifest is a small site: a new page that another item points at, a
// post referencing it and an existing category, an existing page with one
// changed field, an existing category that is already in place, and the
// header global pointing at the new page polymorphically.
func siteManifest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeManifest(t, dir, "a-post.json", `{"collection":"posts","data":{"title":"Launch","slug":"launch",
		"categories":[{"$ref":"categories","slug":"news"}],
		"relatedPage":{"$ref":"@pages/about"}}}`)
	writeManifest(t, dir, "pages/about.json", `{"collection":"pages","data":{"title":"About","slug":"about","_status":"published"}}`)
	writeManifest(t, dir, "pages/home.json", `{"collection":"pages","data":{"id":16,"title":"Home v2","slug":"home","createdAt":"x"}}`)
	writeManifest(t, dir, "cats.json", `[{"collection":"categories","data":{"title":"News","slug":"news"}}]`)
	writeManifest(t, dir, "z-header.json", `{"global":"header","data":{"navItems":[{"link":{"label":"About",
		"reference":{"$ref":"@pages/about","$as":"polymorphic"}}}]}}`)
	writeManifest(t, dir, ".hidden/ignored.json", `not json at all`)
	return dir
}

func seedSite(m *memCMS) {
	m.seed("pages", 16, map[string]any{"title": "Home", "slug": "home", "_status": "published"})
	m.seed("categories", 7, map[string]any{"title": "News", "slug": "news"})
}

// ---------------------------------------------------------------------------
// plan
// ---------------------------------------------------------------------------

func TestPlanDecidesEveryItemAndWritesNothing(t *testing.T) {
	m := newMemCMS(t)
	seedSite(m)
	home := m.home(t)
	dir := siteManifest(t)

	res := m.run(t, home, nil, "plan", dir)
	if res.Code != 0 {
		t.Fatalf("exit %d\n%s", res.Code, res.Stdout)
	}
	if len(m.writes) != 0 {
		t.Fatalf("plan wrote: %v", m.writes)
	}
	items := planItems(t, res)
	var order []string
	for _, it := range items {
		order = append(order, it["key"].(string))
	}
	// The new page is created before the post and the global that need its
	// id; everything else keeps load order.
	want := []string{"categories/news", "pages/about", "posts/launch", "pages/home", "globals/header"}
	if strings.Join(order, " ") != strings.Join(want, " ") {
		t.Errorf("order = %v, want %v", order, want)
	}
	about := itemByKey(t, items, "pages/about")
	if about["action"] != "create" || about["write_mode"] != "draft" || about["id"] != nil {
		t.Errorf("about = %v", about)
	}
	homeItem := itemByKey(t, items, "pages/home")
	if homeItem["action"] != "update" || fmt.Sprint(homeItem["fields"]) != "[title]" || homeItem["write_mode"] != "save" {
		t.Errorf("home = %v", homeItem)
	}
	changes := homeItem["diff"].(map[string]any)["changes"].([]any)
	if len(changes) != 1 || changes[0].(map[string]any)["path"] != "title" {
		t.Errorf("home changes = %v (id and createdAt from the export must not count)", changes)
	}
	if c := itemByKey(t, items, "categories/news"); c["action"] != "unchanged" || fmt.Sprint(c["id"]) != "7" {
		t.Errorf("news = %v", c)
	}
	post := itemByKey(t, items, "posts/launch")
	if fmt.Sprint(post["depends_on"]) != "[categories/news pages/about]" {
		t.Errorf("post depends_on = %v", post["depends_on"])
	}
	hdr := itemByKey(t, items, "globals/header")
	if hdr["action"] != "update" {
		t.Errorf("header = %v", hdr)
	}
	// The header's reference to a page that does not exist yet is shown as
	// a placeholder, polymorphic shape intact.
	b, _ := json.Marshal(hdr["diff"])
	if !strings.Contains(string(b), `"relationTo":"pages"`) || !strings.Contains(string(b), "created by this sync") {
		t.Errorf("header diff = %s", b)
	}
	sum := res.data(t)["summary"].(map[string]any)
	if got := fmt.Sprint(sum["create"], " ", sum["update"], " ", sum["unchanged"], " ", sum["items"]); got != "2 2 1 5" {
		t.Errorf("summary = %v", sum)
	}
	if !res.hasWarning(t, WarnBodyStatusIgnored) {
		t.Errorf("about's data._status must be reported as ignored: %v", res.warnings(t))
	}
}

func TestPlanTableAndBrief(t *testing.T) {
	m := newMemCMS(t)
	seedSite(m)
	home := m.home(t)
	dir := siteManifest(t)
	res := m.run(t, home, nil, "plan", dir, "--brief")
	for _, it := range planItems(t, res) {
		if d, ok := it["diff"].(map[string]any); ok {
			if _, has := d["changes"]; has {
				t.Errorf("--brief kept changes: %v", d)
			}
		}
	}
	tbl := m.run(t, home, nil, "plan", dir, "--output", "table")
	if tbl.Code != 0 || !strings.Contains(tbl.Stdout, "ACTION") || !strings.Contains(tbl.Stdout, "pages/about") {
		t.Errorf("table:\n%s", tbl.Stdout)
	}
}

func TestPlanReportsEveryProblemBeforeAnyWrite(t *testing.T) {
	m := newMemCMS(t)
	seedSite(m)
	m.seed("categories", 8, map[string]any{"title": "Dup", "slug": "dup-1"})
	m.seed("categories", 9, map[string]any{"title": "Dup", "slug": "dup-2"})
	home := m.home(t)
	dir := t.TempDir()
	writeManifest(t, dir, "x.json", `[
	 {"collection":"categories","status":"published","data":{"slug":"a"}},
	 {"collection":"categories","match":"title","data":{"title":"Dup","slug":"q"}},
	 {"collection":"pages","data":{"slug":"b","title":"B","x":{"$ref":"@nope"}}},
	 {"collection":"pages","data":{"slug":"c","title":"C","x":{"$ref":"categories","slug":"missing"}}},
	 {"collection":"pages","data":{"slug":"d","title":"D","x":{"$ref":"@pages/b"}}}
	]`)
	res := m.run(t, home, nil, "plan", dir)
	if res.Code != 5 || res.code(t) != "manifest_invalid" {
		t.Fatalf("exit %d code %s\n%s", res.Code, res.code(t), res.Stdout)
	}
	fails, _ := res.Env["error"].(map[string]any)["failures"].([]any)
	codes := map[string]bool{}
	for _, f := range fails {
		codes[f.(map[string]any)["code"].(string)] = true
	}
	for _, want := range []string{"feature_unavailable", "match_ambiguous", "ref_unresolved"} {
		if !codes[want] {
			t.Errorf("failures lack %s: %v", want, fails)
		}
	}
	items := planItems(t, res)
	if it := itemByKey(t, items, "pages/d"); it["action"] != "not_planned" || !strings.Contains(fmt.Sprint(it["reason"]), "pages/b") {
		t.Errorf("an item referencing a broken one = %v", it)
	}
	if it := itemByKey(t, items, "pages/c"); it["action"] != "error" {
		t.Errorf("ref_unresolved item = %v", it)
	}
	if len(m.writes) != 0 {
		t.Errorf("writes: %v", m.writes)
	}
}

func TestPlanRejectsACycleOfNewDocuments(t *testing.T) {
	m := newMemCMS(t)
	home := m.home(t)
	dir := t.TempDir()
	writeManifest(t, dir, "c.json", `[
	 {"collection":"pages","data":{"slug":"a","title":"A","x":{"$ref":"@pages/b"}}},
	 {"collection":"pages","data":{"slug":"b","title":"B","x":{"$ref":"pages","slug":"a"}}}]`)
	res := m.run(t, home, nil, "plan", dir)
	if res.Code != 5 || res.code(t) != "ref_cycle" {
		t.Fatalf("exit %d code %s\n%s", res.Code, res.code(t), res.Stdout)
	}
	msg := res.Env["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "pages/a → pages/b → pages/a") {
		t.Errorf("message = %q", msg)
	}
	// The same cycle between two documents that already exist is fine: their
	// ids are known before anything is written.
	m.seed("pages", 30, map[string]any{"title": "A", "slug": "a", "_status": "draft"})
	m.seed("pages", 31, map[string]any{"title": "B", "slug": "b", "_status": "draft"})
	res = m.run(t, home, nil, "plan", dir)
	if res.Code != 0 {
		t.Fatalf("existing cycle: exit %d\n%s", res.Code, res.Stdout)
	}
	a := itemByKey(t, planItems(t, res), "pages/a")
	b, _ := json.Marshal(a["diff"])
	if !strings.Contains(string(b), `"to":31`) {
		t.Errorf("a's reference should resolve to 31: %s", b)
	}
}

func TestPlanReadsStdinAndEnvelopes(t *testing.T) {
	m := newMemCMS(t)
	seedSite(m)
	home := m.home(t)
	get := m.run(t, home, nil, "get", "pages", "16", "--draft")
	res := cliRun(t, invocation{Home: home, Env: seededEnv(m.srv.URL), Args: []string{"plan", "-"}, Stdin: get.Stdout})
	if res.Code != 0 {
		t.Fatalf("exit %d\n%s", res.Code, res.Stdout)
	}
	it := planItems(t, res)[0]
	if it["key"] != "pages/home" || it["action"] != "unchanged" {
		t.Errorf("an unedited `pay get` envelope must plan as unchanged: %v", it)
	}
	list := m.run(t, home, nil, "find", "pages", "--draft")
	res = cliRun(t, invocation{Home: home, Env: seededEnv(m.srv.URL), Args: []string{"plan", "-"}, Stdin: list.Stdout})
	if res.Code != 0 || len(planItems(t, res)) != 1 {
		t.Fatalf("a find envelope: exit %d\n%s", res.Code, res.Stdout)
	}
}

// ---------------------------------------------------------------------------
// sync
// ---------------------------------------------------------------------------

func TestSyncWritesInOrderAndIsIdempotent(t *testing.T) {
	m := newMemCMS(t)
	seedSite(m)
	home := m.home(t)
	dir := siteManifest(t)
	bk := filepath.Join(t.TempDir(), "bk")

	res := m.run(t, home, []string{"PAY_BACKUP_DIR=" + bk}, "sync", dir, "--yes")
	if res.Code != 0 {
		t.Fatalf("exit %d\n%s", res.Code, res.Stdout)
	}
	want := []string{
		"POST /api/pages?depth=0&draft=true",
		"POST /api/posts?depth=0&draft=true",
		"PATCH /api/pages/16?depth=0",
		"POST /api/globals/header?depth=0",
	}
	if strings.Join(m.writes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("writes =\n%s\nwant\n%s", strings.Join(m.writes, "\n"), strings.Join(want, "\n"))
	}
	// The page body carries no _status (removed) and no id; the home PATCH
	// the field that changed plus the unchanged scalars the item declares
	// (re-sent so a presence-based hook cannot overwrite them); the post
	// the new page's real id.
	if _, has := m.bodies[0]["_status"]; has {
		t.Errorf("create body kept _status: %v", m.bodies[0])
	}
	if fmt.Sprint(m.bodies[2]) != "map[slug:home title:Home v2]" {
		t.Errorf("home PATCH = %v", m.bodies[2])
	}
	if fmt.Sprint(m.bodies[1]["relatedPage"]) != "101" || fmt.Sprint(m.bodies[1]["categories"]) != "[7]" {
		t.Errorf("post body = %v", m.bodies[1])
	}
	nav, _ := json.Marshal(m.bodies[3]["navItems"])
	if string(nav) != `[{"link":{"label":"About","reference":{"relationTo":"pages","value":101}}}]` {
		t.Errorf("header body = %s", nav)
	}
	d := res.data(t)
	if s := d["summary"].(map[string]any); fmt.Sprint(s["created"], " ", s["updated"], " ", s["failed"], " ", s["not_attempted"]) != "2 2 0 0" {
		t.Errorf("summary = %v", s)
	}
	if n, _ := res.Env["next"].(map[string]any); n == nil || !strings.HasPrefix(n["cmd"].(string), "pay plan ") {
		t.Errorf("next = %v", res.Env["next"])
	}
	// Backups: the two documents that existed (home, header), not the creates.
	if got := metaBackups(t, res); len(got) != 2 {
		t.Errorf("meta.backups = %v", got)
	}
	// Audit: one pre+post pair per write, command sync, the right action.
	events := auditEvents(t, home)
	if len(events) != 8 {
		t.Fatalf("audit events = %d", len(events))
	}
	var actions []string
	for _, e := range events {
		if e.Command != "sync" {
			t.Errorf("audit command = %q", e.Command)
		}
		if e.Phase == "post" {
			actions = append(actions, e.Action)
		}
	}
	if strings.Join(actions, ",") != "create,create,update,update" {
		t.Errorf("audit actions = %v", actions)
	}

	// Re-run: everything is in place, nothing is written, no confirmation
	// is needed.
	m.writes, m.bodies = nil, nil
	again := m.run(t, home, nil, "sync", dir)
	if again.Code != 0 {
		t.Fatalf("re-run exit %d\n%s", again.Code, again.Stdout)
	}
	if len(m.writes) != 0 {
		t.Errorf("re-run wrote %v", m.writes)
	}
	for _, it := range planItems(t, again) {
		if it["action"] != "unchanged" || it["result"] != "unchanged" {
			t.Errorf("after a sync every item is unchanged: %v", it)
		}
	}
}

func TestSyncNeedsConfirmationAndDryRunWritesNothing(t *testing.T) {
	m := newMemCMS(t)
	seedSite(m)
	home := m.home(t)
	dir := siteManifest(t)
	res := m.run(t, home, nil, "sync", dir)
	if res.Code != 11 || res.code(t) != "confirmation_required" {
		t.Fatalf("exit %d code %s", res.Code, res.code(t))
	}
	if !strings.Contains(res.Env["error"].(map[string]any)["message"].(string), "create 2 and update 1 document(s) and 1 global(s)") {
		t.Errorf("message = %v", res.Env["error"])
	}
	bk := filepath.Join(t.TempDir(), "bk")
	dry := m.run(t, home, []string{"PAY_BACKUP_DIR=" + bk}, "sync", dir, "--dry-run")
	if dry.Code != 0 {
		t.Fatalf("dry-run exit %d\n%s", dry.Code, dry.Stdout)
	}
	d := dry.data(t)
	if d["dry_run"] != true {
		t.Errorf("dry_run = %v", d["dry_run"])
	}
	wb, _ := d["would_backup"].(map[string]any)
	if wb == nil || fmt.Sprint(wb["documents"]) != "2" {
		t.Errorf("would_backup = %v", d["would_backup"])
	}
	if len(m.writes) != 0 {
		t.Errorf("writes: %v", m.writes)
	}
	if files := backupFiles(t, bk); len(files) != 0 {
		t.Errorf("dry run wrote backups %v", files)
	}
}

func TestSyncPartialFailure(t *testing.T) {
	m := newMemCMS(t)
	seedSite(m)
	home := m.home(t)
	dir := t.TempDir()
	writeManifest(t, dir, "a.json", `[
	 {"key":"bad","collection":"pages","data":{"slug":"bad","title":"FAIL"}},
	 {"collection":"pages","data":{"slug":"home","title":"Home v3"}},
	 {"collection":"posts","data":{"slug":"p","title":"P","page":{"$ref":"@bad"}}}]`)
	res := m.run(t, home, nil, "sync", dir, "--yes")
	if res.Code != 7 || res.code(t) != "partial_failure" {
		t.Fatalf("exit %d code %s\n%s", res.Code, res.code(t), res.Stdout)
	}
	if res.Env["partial"] != true {
		t.Errorf("partial = %v", res.Env["partial"])
	}
	e := res.Env["error"].(map[string]any)
	if !strings.Contains(e["hint"].(string), "SAFE") {
		t.Errorf("hint = %v", e["hint"])
	}
	d := res.Env["data"].(map[string]any)
	if fmt.Sprint(len(d["succeeded"].([]any)), " ", len(d["failed"].([]any)), " ", len(d["not_attempted"].([]any))) != "1 1 1" {
		t.Errorf("data = %v", d)
	}
	na := d["not_attempted"].([]any)[0].(map[string]any)
	if na["key"] != "posts/p" || !strings.Contains(na["reason"].(string), "bad") {
		t.Errorf("not_attempted = %v", na)
	}
	if n := res.Env["next"].(map[string]any); n["reason"] != "retry_failed_subset" || !strings.Contains(n["cmd"].(string), "pay sync ") {
		t.Errorf("next = %v", n)
	}
	if strings.Join(m.writes, ",") != "POST /api/pages?depth=0&draft=true,PATCH /api/pages/16?depth=0" {
		t.Errorf("writes = %v", m.writes)
	}

	// Nothing committed: the failure is the item's own (validation, exit 5).
	m.writes, m.bodies = nil, nil
	only := t.TempDir()
	writeManifest(t, only, "a.json", `{"collection":"pages","data":{"slug":"bad","title":"FAIL"}}`)
	res = m.run(t, home, nil, "sync", only, "--yes")
	if res.Code != 5 || res.code(t) != "validation_failed" || res.Env["partial"] == true {
		t.Errorf("exit %d code %s partial %v", res.Code, res.code(t), res.Env["partial"])
	}
	if _, ok := res.Env["data"].(map[string]any); !ok {
		t.Errorf("the sync result rides along in data: %v", res.Env["data"])
	}
}

func TestSyncFailFast(t *testing.T) {
	m := newMemCMS(t)
	seedSite(m)
	home := m.home(t)
	dir := t.TempDir()
	writeManifest(t, dir, "a.json", `[
	 {"collection":"pages","data":{"slug":"bad","title":"FAIL"}},
	 {"collection":"pages","data":{"slug":"home","title":"Home v3"}}]`)
	res := m.run(t, home, nil, "sync", dir, "--yes", "--fail-fast")
	if res.Code != 5 {
		t.Fatalf("exit %d\n%s", res.Code, res.Stdout)
	}
	na := res.Env["data"].(map[string]any)["not_attempted"].([]any)
	if len(na) != 1 || !strings.Contains(na[0].(map[string]any)["reason"].(string), "--fail-fast") {
		t.Errorf("not_attempted = %v", na)
	}
	if len(m.writes) != 1 {
		t.Errorf("writes = %v", m.writes)
	}
}

func TestSyncStatusModes(t *testing.T) {
	cases := []struct {
		name, status, current string
		exists                bool
		wantWrite             string
		wantStatus            any
	}{
		{"published publishes", "published", "draft", true, "PATCH /api/pages/40?depth=0", "published"},
		{"draft never unpublishes", "draft", "published", true, "PATCH /api/pages/40?depth=0&draft=true", nil},
		{"keep on published saves live", "keep", "published", true, "PATCH /api/pages/40?depth=0", nil},
		{"keep on a draft saves a draft", "keep", "draft", true, "PATCH /api/pages/40?depth=0&draft=true", nil},
		{"keep creates a draft", "keep", "", false, "POST /api/pages?depth=0&draft=true", nil},
		{"published creates published", "published", "", false, "POST /api/pages?depth=0", "published"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMemCMS(t)
			if tc.exists {
				m.seed("pages", 40, map[string]any{"title": "Old", "slug": "s", "_status": tc.current})
			}
			home := m.home(t)
			dir := t.TempDir()
			writeManifest(t, dir, "a.json", fmt.Sprintf(`{"collection":"pages","status":%q,"data":{"slug":"s","title":"New","_status":"published"}}`, tc.status))
			res := m.run(t, home, nil, "sync", dir, "--yes")
			if res.Code != 0 {
				t.Fatalf("exit %d\n%s", res.Code, res.Stdout)
			}
			if len(m.writes) != 1 || m.writes[0] != tc.wantWrite {
				t.Fatalf("writes = %v, want %s", m.writes, tc.wantWrite)
			}
			if got := m.bodies[0]["_status"]; fmt.Sprint(got) != fmt.Sprint(tc.wantStatus) {
				t.Errorf("body _status = %v, want %v", got, tc.wantStatus)
			}
		})
	}
	// A status-only change is still a write: publishing an unchanged draft.
	m := newMemCMS(t)
	m.seed("pages", 40, map[string]any{"title": "Same", "slug": "s", "_status": "draft"})
	home := m.home(t)
	dir := t.TempDir()
	writeManifest(t, dir, "a.json", `{"collection":"pages","status":"published","data":{"slug":"s","title":"Same"}}`)
	res := m.run(t, home, nil, "plan", dir)
	it := planItems(t, res)[0]
	if it["action"] != "update" || fmt.Sprint(it["fields"]) != "[]" || it["write_mode"] != "publish" {
		t.Errorf("status-only item = %v", it)
	}
	// And a status on a collection without drafts is refused before any read.
	writeManifest(t, dir, "a.json", `{"collection":"categories","status":"draft","data":{"slug":"s"}}`)
	res = m.run(t, home, nil, "plan", dir)
	if res.Code != 10 || res.code(t) != "feature_unavailable" {
		t.Errorf("exit %d code %s", res.Code, res.code(t))
	}
}

func TestPlanRefusesUploadCreatesAndRedactsSecrets(t *testing.T) {
	m := newMemCMS(t)
	m.seed("users", 5, map[string]any{"email": "a@example.com", "apiKey": "old-secret-value"})
	m.seed("media", 6, map[string]any{"filename": "a.png", "alt": "A"})
	home := m.home(t)
	dir := t.TempDir()
	writeManifest(t, dir, "m.json", `[{"collection":"media","match":"filename","data":{"filename":"new.png","alt":"N"}},
		{"collection":"media","match":"filename","data":{"filename":"a.png","alt":"A2"}}]`)
	res := m.run(t, home, nil, "plan", dir)
	if res.Code != 5 || res.code(t) != "file_missing" || !strings.Contains(res.Stdout, "pay upload media") {
		t.Errorf("upload create: exit %d code %s\n%s", res.Code, res.code(t), res.Stdout)
	}
	if it := itemByKey(t, planItems(t, res), "media/a.png"); it["action"] != "update" {
		t.Errorf("updating an upload document's metadata is fine: %v", it)
	}
	writeManifest(t, dir, "m.json", `{"collection":"users","match":"email","data":{"email":"a@example.com","apiKey":"new-secret-value"}}`)
	res = m.run(t, home, nil, "plan", dir)
	if res.Code != 0 {
		t.Fatalf("exit %d\n%s", res.Code, res.Stdout)
	}
	if strings.Contains(res.Stdout, "secret-value") {
		t.Errorf("a plan printed a secret:\n%s", res.Stdout)
	}
	if !res.hasWarning(t, "data_redacted") {
		t.Errorf("warnings = %v", res.warnings(t))
	}
}
