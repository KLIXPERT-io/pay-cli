package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func upsertMeta(t *testing.T, r cliResult) map[string]any {
	t.Helper()
	meta, _ := r.Env["meta"].(map[string]any)
	u, _ := meta["upsert"].(map[string]any)
	if u == nil {
		t.Fatalf("no meta.upsert:\n%s", r.Stdout)
	}
	return u
}

func TestUpsertCreatesThenIsUnchangedThenUpdates(t *testing.T) {
	m := newMemCMS(t)
	m.seed("categories", 7, map[string]any{"title": "News", "slug": "news"})
	home := m.home(t)
	bk := filepath.Join(t.TempDir(), "bk")
	env := []string{"PAY_BACKUP_DIR=" + bk}
	args := []string{"upsert", "posts", "--set", "slug=launch", "--set", "title=Launch",
		"--set-json", `categories=[{"$ref":"categories","slug":"news"}]`}

	res := m.run(t, home, env, args...)
	if res.Code != 0 {
		t.Fatalf("exit %d\n%s", res.Code, res.Stdout)
	}
	u := upsertMeta(t, res)
	if u["action"] != "created" || fmt.Sprint(u["id"]) != "101" || fmt.Sprint(u["matched_by"]) != "map[slug:launch]" {
		t.Errorf("meta.upsert = %v", u)
	}
	if res.Env["command"] != "upsert" || res.Env["data_kind"] != "doc" {
		t.Errorf("envelope = %v %v", res.Env["command"], res.Env["data_kind"])
	}
	if len(m.writes) != 1 || m.writes[0] != "POST /api/posts?depth=0&draft=true" || fmt.Sprint(m.bodies[0]["categories"]) != "[7]" {
		t.Fatalf("writes = %v %v", m.writes, m.bodies)
	}
	if !res.hasWarning(t, "created_as_draft") {
		t.Errorf("the create half keeps create's warnings")
	}

	// Same body again: one lookup per ref and one match, no write.
	m.writes, m.bodies = nil, nil
	res = m.run(t, home, env, args...)
	if u := upsertMeta(t, res); res.Code != 0 || u["action"] != "unchanged" || fmt.Sprint(u["id"]) != "101" {
		t.Fatalf("exit %d meta %v", res.Code, u)
	}
	if len(m.writes) != 0 {
		t.Errorf("unchanged upsert wrote %v", m.writes)
	}
	if ch := res.Env["changed"].(map[string]any); fmt.Sprint(ch["created"], ch["updated"]) != "00" {
		t.Errorf("changed = %v", ch)
	}

	// A changed title: dry-run first (the diff, the request, would_backup),
	// then the PATCH carries only that field.
	dry := m.run(t, home, env, "upsert", "posts", "--set", "slug=launch", "--set", "title=Launch 2", "--dry-run")
	if dry.Code != 0 {
		t.Fatalf("dry exit %d\n%s", dry.Code, dry.Stdout)
	}
	d := dry.data(t)
	if d["action"] != "update" || fmt.Sprint(d["fields"]) != "[title]" || d["would_backup"] == nil {
		t.Errorf("dry-run data = %v", d)
	}
	req := d["request"].(map[string]any)
	if req["method"] != "PATCH" || !strings.Contains(req["url"].(string), "/api/posts/101") {
		t.Errorf("request = %v", req)
	}
	if changes := d["diff"].(map[string]any)["changes"].([]any); len(changes) != 1 {
		t.Errorf("diff = %v", d["diff"])
	}
	if upsertMeta(t, dry)["action"] != "update" || len(m.writes) != 0 {
		t.Errorf("dry-run meta/writes: %v %v", upsertMeta(t, dry), m.writes)
	}
	res = m.run(t, home, env, "upsert", "posts", "--set", "slug=launch", "--set", "title=Launch 2")
	if u := upsertMeta(t, res); u["action"] != "updated" {
		t.Errorf("meta = %v", u)
	}
	if len(m.writes) != 1 || m.writes[0] != "PATCH /api/posts/101?depth=0&draft=true" || fmt.Sprint(m.bodies[0]) != "map[slug:launch title:Launch 2]" {
		t.Errorf("writes = %v %v", m.writes, m.bodies)
	}
	if files := backupFiles(t, bk); len(files) != 1 || !strings.HasSuffix(files[0], "--upsert.json") {
		t.Errorf("backups = %v", files)
	}
	events := auditEvents(t, home)
	last := events[len(events)-1]
	if last.Command != "upsert" || last.Action != "update" {
		t.Errorf("audit = %+v", last)
	}

	// --force writes the whole body although nothing changed.
	m.writes, m.bodies = nil, nil
	res = m.run(t, home, env, "upsert", "posts", "--set", "slug=launch", "--set", "title=Launch 2", "--force")
	if u := upsertMeta(t, res); u["action"] != "updated" || u["forced"] != true || len(m.writes) != 1 {
		t.Errorf("force: meta %v writes %v", u, m.writes)
	}
}

func TestUpsertStatusIsFlagsOnly(t *testing.T) {
	m := newMemCMS(t)
	m.seed("pages", 40, map[string]any{"title": "Old", "slug": "s", "_status": "published"})
	home := m.home(t)
	// An edited export: _status published in the body must not publish a
	// --draft write (the body_publishes footgun).
	res := m.run(t, home, nil, "upsert", "pages", "--data", `{"id":40,"slug":"s","title":"New","_status":"published","updatedAt":"x"}`, "--draft")
	if res.Code != 0 {
		t.Fatalf("exit %d\n%s", res.Code, res.Stdout)
	}
	if !res.hasWarning(t, WarnBodyStatusIgnored) {
		t.Errorf("warnings = %v", res.warnings(t))
	}
	if len(m.writes) != 1 || m.writes[0] != "PATCH /api/pages/40?depth=0&draft=true" {
		t.Fatalf("writes = %v", m.writes)
	}
	if fmt.Sprint(m.bodies[0]) != "map[slug:s title:New]" {
		t.Errorf("body = %v (no _status, no id, no timestamps; the unchanged slug re-sent)", m.bodies[0])
	}
	if got := m.colls["pages"]["40"].main["title"]; got != "Old" {
		t.Errorf("the live page changed: %v", got)
	}
	// --publish on the same, now draft-on-top, page publishes it.
	m.writes, m.bodies = nil, nil
	res = m.run(t, home, nil, "upsert", "pages", "--set", "slug=s", "--set", "title=New", "--publish")
	if res.Code != 0 || len(m.writes) != 1 || m.writes[0] != "PATCH /api/pages/40?depth=0" || m.bodies[0]["_status"] != "published" {
		t.Errorf("publish: exit %d writes %v bodies %v", res.Code, m.writes, m.bodies)
	}
	if res := m.run(t, home, nil, "upsert", "pages", "--set", "slug=s", "--draft", "--publish"); res.Code != 5 {
		t.Errorf("--draft --publish: exit %d", res.Code)
	}
	if res := m.run(t, home, nil, "upsert", "categories", "--set", "slug=s", "--draft"); res.Code != 10 {
		t.Errorf("--draft without drafts: exit %d", res.Code)
	}
}

func TestUpsertMatchErrors(t *testing.T) {
	m := newMemCMS(t)
	m.seed("categories", 8, map[string]any{"title": "Dup", "slug": "dup-1"})
	m.seed("categories", 9, map[string]any{"title": "Dup", "slug": "dup-2"})
	home := m.home(t)

	res := m.run(t, home, nil, "upsert", "categories", "--match", "title", "--set", "title=Dup", "--set", "slug=x")
	if res.Code != 5 || res.code(t) != "match_ambiguous" {
		t.Fatalf("exit %d code %s", res.Code, res.code(t))
	}
	e := res.Env["error"].(map[string]any)
	if fmt.Sprint(e["did_you_mean"]) != "[8 9]" || !strings.Contains(e["hint"].(string), "pay find categories --where 'title eq Dup'") {
		t.Errorf("error = %v", e)
	}
	res = m.run(t, home, nil, "upsert", "categories", "--set", "title=No slug")
	if res.Code != 5 || res.code(t) != "invalid_args" || !strings.Contains(res.Stdout, "has no value for") {
		t.Errorf("missing match value: exit %d\n%s", res.Code, res.Stdout)
	}
	// pages has a field shard: an unknown match field is caught before the
	// lookup.
	m.nFinds = 0
	res = m.run(t, home, nil, "upsert", "pages", "--match", "slugg", "--set", "slugg=x")
	if res.Code != 5 || res.code(t) != "unknown_field" || m.nFinds != 0 {
		t.Errorf("unknown match field: exit %d code %s finds %d", res.Code, res.code(t), m.nFinds)
	}
	if len(m.writes) != 0 {
		t.Errorf("writes = %v", m.writes)
	}
}

// ---------------------------------------------------------------------------
// $ref in the ordinary write verbs
// ---------------------------------------------------------------------------

func TestRefsInCreateAndUpdateBodies(t *testing.T) {
	m := newMemCMS(t)
	m.seed("pages", 16, map[string]any{"title": "Home", "slug": "home", "_status": "published"})
	m.seed("categories", 8, map[string]any{"title": "Dup", "slug": "dup-1"})
	m.seed("categories", 9, map[string]any{"title": "Dup", "slug": "dup-2"})
	home := m.home(t)

	res := m.run(t, home, nil, "create", "posts", "--set", "title=T", "--set-json",
		`link={"$ref":"pages","slug":"home","$as":"polymorphic"}`, "--set-json",
		`cats=[{"$ref":"categories","slug":"dup-2"},{"$ref":"categories","where":{"slug":{"equals":"dup-1"}}}]`)
	if res.Code != 0 {
		t.Fatalf("exit %d\n%s", res.Code, res.Stdout)
	}
	b, _ := json.Marshal(m.bodies[0])
	if !strings.Contains(string(b), `"link":{"relationTo":"pages","value":16}`) || !strings.Contains(string(b), `"cats":[9,8]`) {
		t.Errorf("body = %s", b)
	}

	cases := []struct {
		name, set, code string
		exit            int
	}{
		{"unresolved", `x={"$ref":"pages","slug":"nope"}`, "ref_unresolved", 4},
		{"ambiguous", `x={"$ref":"categories","title":"Dup"}`, "ref_ambiguous", 5},
		{"plan-local outside a plan", `x={"$ref":"@home"}`, "bad_request_body", 5},
		{"malformed", `x={"$ref":"pages"}`, "bad_request_body", 5},
		{"unknown collection", `x={"$ref":"nopes","slug":"a"}`, "collection_unknown", 10},
		{"unknown field on a known shard", `x={"$ref":"pages","slugg":"a"}`, "unknown_field", 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m.writes, m.bodies = nil, nil
			res := m.run(t, home, nil, "update", "pages", "16", "--set-json", tc.set)
			if res.Code != tc.exit || res.code(t) != tc.code {
				t.Errorf("exit %d code %s, want %d %s\n%s", res.Code, res.code(t), tc.exit, tc.code, res.Stdout)
			}
			if len(m.writes) != 0 {
				t.Errorf("a failed reference must stop the write: %v", m.writes)
			}
		})
	}
	amb := m.run(t, home, nil, "update", "pages", "16", "--set-json", `x={"$ref":"categories","title":"Dup"}`)
	if e := amb.Env["error"].(map[string]any); fmt.Sprint(e["did_you_mean"]) != "[8 9]" {
		t.Errorf("ref_ambiguous lists the ids like slug_ambiguous: %v", e)
	}
	// A dry run resolves references too: the previewed body is the real one.
	dry := m.run(t, home, nil, "update", "pages", "16", "--set-json", `x={"$ref":"categories","slug":"dup-1"}`, "--dry-run")
	req := dry.data(t)["request"].(map[string]any)
	if fmt.Sprint(req["body"]) != "map[x:8]" {
		t.Errorf("dry-run body = %v", req["body"])
	}
	// globals update resolves them as well.
	m.writes, m.bodies = nil, nil
	g := m.run(t, home, nil, "globals", "update", "header", "--yes", "--set-json",
		`navItems=[{"link":{"reference":{"$ref":"pages","slug":"home","$as":"polymorphic"}}}]`)
	if g.Code != 0 {
		t.Fatalf("globals update exit %d\n%s", g.Code, g.Stdout)
	}
	nb, _ := json.Marshal(m.bodies[0])
	if !strings.Contains(string(nb), `{"relationTo":"pages","value":16}`) {
		t.Errorf("header body = %s", nb)
	}
}
