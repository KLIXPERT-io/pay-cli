package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/manifest"
)

// ---------------------------------------------------------------------------
// alignToStored: what an update of an id-less manifest really sends
// ---------------------------------------------------------------------------

func TestAlignToStoredAdoptsRowIDs(t *testing.T) {
	stored := map[string]any{
		"layout": []any{
			map[string]any{"id": "b1", "blockType": "cta", "links": []any{
				map[string]any{"id": "l1", "link": map[string]any{"label": "Go", "appearance": "outline"}}}},
			map[string]any{"id": "b2", "blockType": "content"},
			map[string]any{"id": "b3", "blockType": "cta"},
		},
		"when": "2026-01-01T00:00:00.000Z",
		"data": []any{map[string]any{"id": "j1"}},
	}
	body := map[string]any{
		"layout": []any{
			// id-less: pairs with the first unclaimed cta (b1), and its
			// nested id-less link with l1.
			map[string]any{"blockType": "cta", "links": []any{map[string]any{"link": map[string]any{"label": "Go"}}}},
			// its own id claims b3 …
			map[string]any{"id": "b3", "blockType": "cta"},
			// … so this id-less cta has no partner left: a new row.
			map[string]any{"blockType": "cta"},
			// a different type pairs with the content row.
			map[string]any{"blockType": "content"},
		},
		"when": "2026-01-01",
		"data": []any{map[string]any{"x": 1}},
	}
	got := alignToStored(stored, body, []string{"data"})
	ids := []string{}
	for _, r := range got["layout"].([]any) {
		ids = append(ids, fmt.Sprint(r.(map[string]any)["id"]))
	}
	if strings.Join(ids, ",") != "b1,b3,<nil>,b2" {
		t.Errorf("adopted ids = %v", ids)
	}
	link := got["layout"].([]any)[0].(map[string]any)["links"].([]any)[0].(map[string]any)
	if link["id"] != "l1" {
		t.Errorf("a nested id-less row must adopt too: %v", link)
	}
	if got["when"] != "2026-01-01T00:00:00.000Z" {
		t.Errorf("an equal instant keeps its stored form: %v", got["when"])
	}
	if _, has := got["data"].([]any)[0].(map[string]any)["id"]; has {
		t.Errorf("a json leaf is sent as written: %v", got["data"])
	}
	// The body itself is never modified.
	if _, has := body["layout"].([]any)[0].(map[string]any)["id"]; has || body["when"] != "2026-01-01" {
		t.Errorf("alignToStored mutated the body: %v", body)
	}
}

func TestSameInstant(t *testing.T) {
	cases := []struct {
		stored, body string
		want         bool
	}{
		{"2026-01-01T00:00:00.000Z", "2026-01-01", true},
		{"2026-01-01T00:00:00.000Z", "2026-01-01T00:00:00Z", true},
		{"2026-01-01T00:00:00.000Z", "2026-01-01T01:00:00+01:00", true},
		{"2026-01-01T00:00:00.000Z", "2026-01-02", false},
		{"2026-01-01T00:00:00.000Z", "2026-01-01T00:00:00", false}, // no zone: the server's clock decides
		{"2026-01-01", "2026-01-01T00:00:00.000Z", false},          // a stored text value, not an instant
		{"2026-01-01T00:00:00.000Z", "hello", false},
	}
	for _, c := range cases {
		if got := sameInstant(c.stored, c.body); got != c.want {
			t.Errorf("sameInstant(%q, %q) = %v, want %v", c.stored, c.body, got, c.want)
		}
	}
}

func TestDecideOnIDLessRows(t *testing.T) {
	stored := map[string]any{
		"hero": map[string]any{"type": "low", "links": []any{
			map[string]any{"id": "r1", "link": map[string]any{"label": "Read", "appearance": "default", "newTab": nil}},
		}},
		"layout": []any{map[string]any{"id": "b1", "blockType": "cta", "title": "T", "style": "dark"}},
	}
	opts := diffOptions(nil, nil, false)
	// id-less rows: the stored keys they leave out are kept by Payload (the
	// row adopts r1/b1), so they are no change.
	body := map[string]any{
		"hero":   map[string]any{"links": []any{map[string]any{"link": map[string]any{"label": "Read"}}}},
		"layout": []any{map[string]any{"blockType": "cta", "title": "T"}},
	}
	if dc := decide(stored, body, nil, manifest.StatusKeep, opts, false); dc.action != actionUnchanged {
		t.Errorf("unsent keys of matched rows reported as changes: %+v", dc.diff.Changes)
	}
	// A key the body DOES mention is compared, and the write sends the row
	// with its stored id.
	body["layout"] = []any{map[string]any{"blockType": "cta", "title": "T2"}}
	dc := decide(stored, body, nil, manifest.StatusKeep, opts, false)
	if dc.action != actionUpdate || fmt.Sprint(dc.fields) != "[layout]" || len(dc.diff.Changes) != 1 {
		t.Fatalf("a real change was hidden or widened: %+v %+v", dc, dc.diff)
	}
	if row := dc.writeBody()["layout"].([]any)[0].(map[string]any); row["id"] != "b1" {
		t.Errorf("the write must carry the stored row id: %v", row)
	}
	// A different blockType is a new row: a change.
	body["layout"] = []any{map[string]any{"blockType": "hero", "title": "T"}}
	if dc = decide(stored, body, nil, manifest.StatusKeep, opts, false); dc.action != actionUpdate {
		t.Errorf("blockType change hidden")
	}
	// Clearing a value takes an explicit null.
	body = map[string]any{"layout": []any{map[string]any{"blockType": "cta", "style": nil}}}
	if dc = decide(stored, body, nil, manifest.StatusKeep, opts, false); dc.action != actionUpdate {
		t.Errorf("clearing a value with null must be a change")
	}
	// Fewer rows is a removal.
	body = map[string]any{"layout": []any{}}
	if dc = decide(stored, body, nil, manifest.StatusKeep, opts, false); dc.action != actionUpdate {
		t.Errorf("row removal hidden")
	}
}

// ---------------------------------------------------------------------------
// sync against memCMS (Payload's row-merge semantics)
// ---------------------------------------------------------------------------

func rowAt(t *testing.T, doc map[string]any, path ...any) map[string]any {
	t.Helper()
	var cur any = doc
	for _, p := range path {
		switch k := p.(type) {
		case string:
			cur = cur.(map[string]any)[k]
		case int:
			cur = cur.([]any)[k]
		}
	}
	m, ok := cur.(map[string]any)
	if !ok {
		t.Fatalf("%v is not an object: %v", path, cur)
	}
	return m
}

// An editor set a link's appearance in the admin UI; the id-less manifest
// never mentions it. An unrelated edit in the same top-level field must not
// reset it, and plan/--dry-run must show exactly that one edit.
func TestSyncKeepsWhatTheManifestDoesNotMention(t *testing.T) {
	m := newMemCMS(t)
	m.seed("pages", 40, map[string]any{"title": "P", "slug": "p", "_status": "draft",
		"hero": map[string]any{"type": "low", "links": []any{
			map[string]any{"id": "h1", "link": map[string]any{"label": "Read", "appearance": "outline"}}}},
		"layout": []any{
			map[string]any{"id": "b1", "blockType": "cta", "title": "T", "links": []any{
				map[string]any{"id": "l1", "link": map[string]any{"label": "Go", "appearance": "outline"}}}},
			map[string]any{"id": "b2", "blockType": "archive", "limit": json.Number("3")},
		}})
	home := m.home(t)
	dir := t.TempDir()
	item := `{"collection":"pages","data":{"slug":"p","title":"P",
		"hero":{"links":[{"link":{"label":"Read"}}]},
		"layout":[{"blockType":"cta","title":"T","links":[{"link":{"label":"Go"}}]},{"blockType":"archive","limit":%d}]}}`
	writeManifest(t, dir, "p.json", fmt.Sprintf(item, 3))
	if it := planItems(t, m.run(t, home, nil, "plan", dir))[0]; it["action"] != "unchanged" {
		t.Fatalf("values the manifest does not mention are no change: %v", it)
	}

	writeManifest(t, dir, "p.json", fmt.Sprintf(item, 4))
	dry := m.run(t, home, nil, "sync", dir, "--dry-run")
	it := planItems(t, dry)[0]
	changes := it["diff"].(map[string]any)["changes"].([]any)
	if len(changes) != 1 || changes[0].(map[string]any)["path"] != "layout[1].limit" {
		t.Fatalf("the preview must show exactly the one edit: %v", changes)
	}

	res := m.run(t, home, nil, "sync", dir, "--yes")
	if res.Code != 0 {
		t.Fatalf("exit %d\n%s", res.Code, res.Stdout)
	}
	sent := m.bodies[0]
	if rowAt(t, sent, "layout", 0)["id"] != "b1" || rowAt(t, sent, "layout", 1)["id"] != "b2" ||
		rowAt(t, sent, "layout", 0, "links", 0)["id"] != "l1" {
		t.Errorf("the write must carry the stored row ids: %v", sent)
	}
	stored := m.colls["pages"]["40"].latest
	link := rowAt(t, stored, "layout", 0, "links", 0)
	if link["id"] != "l1" || rowAt(t, link, "link")["appearance"] != "outline" {
		t.Errorf("the admin value (or the row id) was reset: %v", link)
	}
	if fmt.Sprint(rowAt(t, stored, "layout", 1)["limit"]) != "4" {
		t.Errorf("the edit was not written: %v", stored["layout"])
	}
	if it := planItems(t, m.run(t, home, nil, "plan", dir))[0]; it["action"] != "unchanged" {
		t.Errorf("after the sync the plan is unchanged: %v", it)
	}
}

// Export → append one block without an id → sync → the next plan is
// unchanged (the appended row adopts the id the server gave it).
func TestSyncConvergesOnAnExportWithAnAppendedRow(t *testing.T) {
	m := newMemCMS(t)
	m.seed("pages", 41, map[string]any{"title": "E", "slug": "e", "_status": "draft",
		"layout": []any{map[string]any{"id": "b1", "blockType": "cta", "title": "T"}}})
	home := m.home(t)
	get := m.run(t, home, nil, "get", "pages", "41", "--draft", "--depth", "0")
	env := get.Env
	data := env["data"].(map[string]any)
	data["layout"] = append(data["layout"].([]any), map[string]any{"blockType": "content", "columns": []any{}})
	raw, _ := json.Marshal(env)
	dir := t.TempDir()
	writeManifest(t, dir, "e.json", string(raw))

	it := planItems(t, m.run(t, home, nil, "plan", dir))[0]
	if it["action"] != "update" {
		t.Fatalf("plan = %v", it)
	}
	if res := m.run(t, home, nil, "sync", dir, "--yes"); res.Code != 0 {
		t.Fatalf("exit %d\n%s", res.Code, res.Stdout)
	}
	m.writes, m.bodies = nil, nil
	again := m.run(t, home, nil, "sync", dir, "--yes")
	if it := planItems(t, again)[0]; it["action"] != "unchanged" || len(m.writes) != 0 {
		t.Errorf("an export plus an appended row must converge: %v, writes %v", it, m.writes)
	}
}

func TestPlanEqualDatesAreUnchanged(t *testing.T) {
	m := newMemCMS(t)
	m.seed("pages", 42, map[string]any{"title": "D", "slug": "d", "_status": "draft", "publishedAt": "2026-01-01T00:00:00.000Z"})
	home := m.home(t)
	dir := t.TempDir()
	writeManifest(t, dir, "d.json", `{"collection":"pages","data":{"slug":"d","publishedAt":"2026-01-01"}}`)
	if it := planItems(t, m.run(t, home, nil, "plan", dir))[0]; it["action"] != "unchanged" {
		t.Errorf("a date equal to the stored instant is unchanged: %v", it)
	}
	writeManifest(t, dir, "d.json", `{"collection":"pages","data":{"slug":"d","publishedAt":"2026-01-02"}}`)
	if it := planItems(t, m.run(t, home, nil, "plan", dir))[0]; it["action"] != "update" {
		t.Errorf("another date is a change: %v", it)
	}
}

// ---------------------------------------------------------------------------
// exports: ids, slug-less collections, depth
// ---------------------------------------------------------------------------

func TestPlanExportIDs(t *testing.T) {
	m := newMemCMS(t)
	seedSite(m)
	m.seed("media", 6, map[string]any{"filename": "a.png", "alt": "A"})
	home := m.home(t)

	// A renamed export would create a copy: said, with the fix.
	get := m.run(t, home, nil, "get", "pages", "16", "--draft", "--depth", "0")
	get.Env["data"].(map[string]any)["slug"] = "home-renamed"
	raw, _ := json.Marshal(get.Env)
	res := cliRun(t, invocation{Home: home, Env: seededEnv(m.srv.URL), Args: []string{"plan", "-"}, Stdin: string(raw)})
	if it := planItems(t, res)[0]; it["action"] != "create" || !res.hasWarning(t, WarnBodyIDMismatch) {
		t.Fatalf("a renamed export: %v warnings %v", it, res.warnings(t))
	}
	if !strings.Contains(res.Stdout, "pay update pages 16 --set slug=") {
		t.Errorf("the hint names the rename by id:\n%s", res.Stdout)
	}

	// A collection without slugs: the export matches on its id.
	get = m.run(t, home, nil, "get", "media", "6", "--depth", "0")
	res = cliRun(t, invocation{Home: home, Env: seededEnv(m.srv.URL), Args: []string{"plan", "-"}, Stdin: get.Stdout})
	if res.Code != 0 {
		t.Fatalf("exit %d\n%s", res.Code, res.Stdout)
	}
	if it := planItems(t, res)[0]; it["key"] != "media/6" || it["action"] != "unchanged" {
		t.Errorf("a slug-less export: %v", it)
	}
	// … and never creates: a new document would get another id.
	get.Env["data"].(map[string]any)["id"] = 99
	raw, _ = json.Marshal(get.Env)
	res = cliRun(t, invocation{Home: home, Env: seededEnv(m.srv.URL), Args: []string{"plan", "-"}, Stdin: string(raw)})
	if res.Code != 4 || res.code(t) != "doc_not_found" {
		t.Errorf("an id match that finds nothing: exit %d code %s", res.Code, res.code(t))
	}

	// upsert warns the same way.
	res = m.run(t, home, nil, "upsert", "pages", "--data", `{"id":16,"slug":"home-2","title":"x"}`, "--dry-run")
	if !res.hasWarning(t, WarnBodyIDMismatch) {
		t.Errorf("upsert: %v", res.warnings(t))
	}
}

func TestManifestRefusesPopulatedExports(t *testing.T) {
	env := `{"ok":true,"v":1,"data_kind":"doc","meta":{},"target":{"slug":"pages"},
		"data":{"id":1,"slug":"x","hero":{"media":{"id":7,"filename":"a.png","createdAt":"2026"}},"layout":[{"id":"r","blockType":"cta"}]}}`
	dir := t.TempDir()
	writeManifest(t, dir, "e.json", env)
	_, err := manifest.Load([]string{dir}, nil)
	if err == nil || !strings.Contains(err.Error(), "hero.media") {
		t.Fatalf("err = %v", err)
	}
	// Rows have ids but no timestamps: a depth-0 export is fine.
	writeManifest(t, dir, "e.json", strings.Replace(env, `{"id":7,"filename":"a.png","createdAt":"2026"}`, "7", 1))
	if _, err := manifest.Load([]string{dir}, nil); err != nil {
		t.Fatalf("a depth-0 export: %v", err)
	}
}

// ---------------------------------------------------------------------------
// normalized match values, failures, hints
// ---------------------------------------------------------------------------

func TestSyncWarnsWhenAHookRewritesTheMatchValue(t *testing.T) {
	m := newMemCMS(t)
	home := m.home(t)
	dir := t.TempDir()
	writeManifest(t, dir, "c.json", `{"collection":"categories","data":{"title":"C","slug":"My Cat"}}`)
	res := m.run(t, home, nil, "sync", dir, "--yes")
	if res.Code != 0 || !res.hasWarning(t, WarnMatchValueNormalized) {
		t.Fatalf("exit %d warnings %v", res.Code, res.warnings(t))
	}
	if !strings.Contains(res.Stdout, `\"my-cat\"`) {
		t.Errorf("the warning names the stored value:\n%s", res.Stdout)
	}
	// The next run cannot find it; the unique index refuses the create, and
	// the hint says why.
	res = m.run(t, home, nil, "sync", dir, "--yes")
	if res.Code != 5 || res.code(t) != "validation_failed" {
		t.Fatalf("exit %d code %s", res.Code, res.code(t))
	}
	hint := res.Env["error"].(map[string]any)["hint"].(string)
	if !strings.Contains(hint, "normalized form") || !strings.Contains(hint, "pay find categories --where 'slug like cat'") {
		t.Errorf("hint = %s", hint)
	}
	// upsert says the same after its write.
	res = m.run(t, home, nil, "upsert", "categories", "--data", `{"title":"O","slug":"Other Cat"}`)
	if res.Code != 0 || !res.hasWarning(t, WarnMatchValueNormalized) {
		t.Errorf("upsert: exit %d warnings %v", res.Code, res.warnings(t))
	}
}

func TestSyncServerErrorIsNotWroteNothing(t *testing.T) {
	m := newMemCMS(t)
	home := m.home(t)
	dir := t.TempDir()
	writeManifest(t, dir, "p.json", `{"collection":"posts","data":{"slug":"x","title":"BOOM"}}`)
	res := m.run(t, home, nil, "sync", dir, "--yes")
	if res.Code != 6 {
		t.Fatalf("exit %d\n%s", res.Code, res.Stdout)
	}
	e := res.Env["error"].(map[string]any)
	if msg := e["message"].(string); strings.Contains(msg, "wrote nothing") || !strings.Contains(msg, "no write of this sync was confirmed") {
		t.Errorf("message = %s", msg)
	}
	if !strings.Contains(e["hint"].(string), "MAY HAVE COMMITTED") {
		t.Errorf("hint = %s", e["hint"])
	}
}

func TestPlanPredictsOrphanRows(t *testing.T) {
	m := newMemCMS(t)
	m.seed("pages", 43, map[string]any{"title": "O", "slug": "o", "_status": "published", "layout": []any{
		map[string]any{"id": "g1", "blockType": "group", "blocks": []any{
			map[string]any{"id": "b1", "blockType": "bento", "blocks": []any{
				map[string]any{"id": "i1", "blockType": "bentoItem", "heading": "one"}}}}}}})
	home := m.home(t)
	dir := t.TempDir()
	writeManifest(t, dir, "o.json", `{"collection":"pages","data":{"slug":"o","layout":[
		{"blockType":"group","blocks":[{"blockType":"grid","blocks":[{"blockType":"text","heading":"A"}]}]}]}}`)
	for _, args := range [][]string{{"plan", dir}, {"sync", dir, "--dry-run"}} {
		res := m.run(t, home, nil, args...)
		if res.Code != 0 || !res.hasWarning(t, WarnOrphanRisk) {
			t.Errorf("%v: exit %d warnings %v", args, res.Code, res.warnings(t))
		}
		if !strings.Contains(res.Stdout, "pay update pages 43 --reset-nested --dry-run") {
			t.Errorf("%v: the fix is named:\n%s", args, res.Stdout)
		}
	}
	// A top-level-only change carries no risk.
	writeManifest(t, dir, "o.json", `{"collection":"pages","data":{"slug":"o","title":"O2"}}`)
	if res := m.run(t, home, nil, "plan", dir); res.hasWarning(t, WarnOrphanRisk) {
		t.Errorf("false orphan_risk: %v", res.warnings(t))
	}
}

func TestPlanHintsAndUpsertPublishHint(t *testing.T) {
	m := newMemCMS(t)
	home := m.home(t)
	dir := t.TempDir()
	writeManifest(t, dir, "p.json", `{"collection":"pages","data":{"slug":"p","hero":{"richText":"# Hi"}}}`)
	res := m.run(t, home, nil, "plan", dir)
	if res.code(t) != "richtext_rendered" || strings.Contains(res.Stdout, "--set-md") || !strings.Contains(res.Stdout, "pay lexical from-md") {
		t.Errorf("code %s\n%s", res.code(t), res.Stdout)
	}
	res = m.run(t, home, nil, "upsert", "categories", "--data", `{"slug":"s","title":"S"}`, "--publish")
	if res.code(t) != "feature_unavailable" || !strings.Contains(res.Stdout, "--publish") {
		t.Errorf("code %s\n%s", res.code(t), res.Stdout)
	}
}

// pay apply resolves {"$ref"} like every other write (a `pay blocks
// --set-json` stage can put one into the piped document).
func TestApplyResolvesRefs(t *testing.T) {
	m := newMemCMS(t)
	seedSite(m)
	m.seed("categories", 8, map[string]any{"title": "Child", "slug": "child"})
	home := m.home(t)
	get := m.run(t, home, nil, "get", "categories", "8", "--depth", "0")
	get.Env["data"].(map[string]any)["parent"] = map[string]any{"$ref": "categories", "slug": "news"}
	raw, _ := json.Marshal(get.Env)
	res := cliRun(t, invocation{Home: home, Env: seededEnv(m.srv.URL), Args: []string{"apply", "--field", "parent", "--yes"}, Stdin: string(raw)})
	if res.Code != 0 {
		t.Fatalf("exit %d\n%s", res.Code, res.Stdout)
	}
	if len(m.bodies) != 1 || fmt.Sprint(m.bodies[0]["parent"]) != "7" {
		t.Errorf("bodies = %v", m.bodies)
	}
}

// An export carries "<redacted>" where a value is secret; syncing it back must
// not write the mask over the real value.
func TestPlanLeavesMaskedValuesOut(t *testing.T) {
	m := newMemCMS(t)
	m.seed("users", 5, map[string]any{"email": "a@example.com", "apiKey": "real-secret-value", "name": "A"})
	home := m.home(t)
	get := m.run(t, home, nil, "get", "users", "5", "--depth", "0")
	if !strings.Contains(get.Stdout, "<redacted>") {
		t.Fatalf("the fixture must be masked: %s", get.Stdout)
	}
	res := cliRun(t, invocation{Home: home, Env: seededEnv(m.srv.URL), Args: []string{"plan", "-"}, Stdin: get.Stdout})
	if it := planItems(t, res)[0]; it["action"] != "unchanged" || !res.hasWarning(t, WarnRedactedNotSent) {
		t.Fatalf("plan: %v warnings %v", it, res.warnings(t))
	}
	get.Env["data"].(map[string]any)["name"] = "B"
	raw, _ := json.Marshal(get.Env)
	res = cliRun(t, invocation{Home: home, Env: seededEnv(m.srv.URL), Args: []string{"sync", "-", "--yes"}, Stdin: string(raw)})
	if res.Code != 0 || len(m.bodies) != 1 {
		t.Fatalf("exit %d bodies %v\n%s", res.Code, m.bodies, res.Stdout)
	}
	if _, sent := m.bodies[0]["apiKey"]; sent || m.colls["users"]["5"].latest["apiKey"] != "real-secret-value" {
		t.Errorf("the mask was written: %v", m.bodies[0])
	}
	// upsert drops them the same way.
	res = m.run(t, home, nil, "upsert", "users", "--match", "email", "--data", `{"email":"a@example.com","apiKey":"<redacted>","name":"B"}`)
	if res.Code != 0 || !res.hasWarning(t, WarnRedactedNotSent) || len(m.bodies) != 1 {
		t.Errorf("upsert: exit %d warnings %v bodies %v", res.Code, res.warnings(t), m.bodies)
	}
}

func TestApplyResolvesRefsInAGlobal(t *testing.T) {
	m := newMemCMS(t)
	seedSite(m)
	home := m.home(t)
	get := m.run(t, home, nil, "globals", "get", "header", "--depth", "0")
	get.Env["data"].(map[string]any)["navItems"] = []any{map[string]any{"link": map[string]any{"label": "Home",
		"reference": map[string]any{"$ref": "pages", "slug": "home", "$as": "polymorphic"}}}}
	raw, _ := json.Marshal(get.Env)
	res := cliRun(t, invocation{Home: home, Env: seededEnv(m.srv.URL), Args: []string{"apply", "--field", "navItems", "--yes"}, Stdin: string(raw)})
	if res.Code != 0 {
		t.Fatalf("exit %d\n%s", res.Code, res.Stdout)
	}
	nav, _ := json.Marshal(m.bodies[0]["navItems"])
	if string(nav) != `[{"link":{"label":"Home","reference":{"relationTo":"pages","value":16}}}]` {
		t.Errorf("navItems = %s", nav)
	}
}
