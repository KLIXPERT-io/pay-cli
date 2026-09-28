package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
)

// ---------------------------------------------------------------------------
// §10.2.2 body_publishes
// ---------------------------------------------------------------------------

func lastPatch(t *testing.T, cms *fakeCMS, path string) (map[string]any, string) {
	t.Helper()
	reqs := cms.srv.Requests()
	for i := len(reqs) - 1; i >= 0; i-- {
		if reqs[i].Method == http.MethodPatch && reqs[i].Path == path {
			var body map[string]any
			_ = json.Unmarshal([]byte(reqs[i].Body), &body)
			return body, reqs[i].Query
		}
	}
	t.Fatalf("no PATCH %s", path)
	return nil, ""
}

// A body carrying _status "published" publishes in Payload even with
// ?draft=true. --draft wins: the key is removed and the write saves a draft.
func TestDraftWinsOverABodyThatPublishes(t *testing.T) {
	cms := newFakeCMS()
	srv := cms.server(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	file := writeBody(t, home, `{"title":"Edited export","_status":"published"}`)

	r := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL),
		Args: []string{"update", "pages", "16", "--data-file", file, "--draft"}})
	if r.Code != 0 || !r.hasWarning(t, WarnBodyPublishes) {
		t.Fatalf("exit %d warnings %v", r.Code, r.warnings(t))
	}
	body, q := lastPatch(t, cms, "/api/pages/16")
	if _, ok := body["_status"]; ok || !strings.Contains(q, "draft=true") || body["title"] != "Edited export" {
		t.Errorf("sent %v ?%s — _status must be gone and draft=true kept", body, q)
	}

	// No flag: the write publishes, loudly; the body is sent as given.
	r = cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL),
		Args: []string{"update", "pages", "16", "--data-file", file}})
	if r.Code != 0 || !r.hasWarning(t, WarnBodyPublishes) {
		t.Fatalf("exit %d warnings %v", r.Code, r.warnings(t))
	}
	if body, _ := lastPatch(t, cms, "/api/pages/16"); body["_status"] != "published" {
		t.Errorf("sent %v", body)
	}

	// --publish makes it deliberate: no warning.
	r = cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL),
		Args: []string{"update", "pages", "16", "--data-file", file, "--publish"}})
	if r.Code != 0 || r.hasWarning(t, WarnBodyPublishes) {
		t.Fatalf("exit %d warnings %v", r.Code, r.warnings(t))
	}

	// The contradiction is refused before anything is sent.
	n := len(cms.srv.Requests())
	r = cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL),
		Args: []string{"update", "pages", "16", "--set", "title=x", "--draft", "--publish"}})
	if r.code(t) != "invalid_args" || len(cms.srv.Requests()) != n {
		t.Errorf("--draft --publish: %s", r.Stdout)
	}
}

func TestApplyAllFieldsDraftStripsStatus(t *testing.T) {
	cms := newFakeCMS()
	srv := cms.server(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	doc := `{"id":16,"title":"Home","slug":"home","_status":"published","createdAt":"x","updatedAt":"y"}`
	env := strings.Replace(pageEnvelope(t, doc), `"id":12`, `"id":16`, 1)
	r := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), Stdin: env,
		Args: []string{"apply", "--all-fields", "--draft"}})
	if r.Code != 0 || !r.hasWarning(t, WarnBodyPublishes) {
		t.Fatalf("exit %d warnings %v\n%s", r.Code, r.warnings(t), r.Stdout)
	}
	if body, _ := lastPatch(t, cms, "/api/pages/16"); body["_status"] != nil {
		t.Errorf("apply --all-fields --draft sent _status: %v", body)
	}
}

func TestApplyBodyStatusRule(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name          string
		body          map[string]any
		draft, flag   bool
		drafts        *bool
		warn, removed bool
	}{
		{"draft wins", map[string]any{"_status": "published"}, true, false, &yes, true, true},
		{"publishes loudly", map[string]any{"_status": "published"}, false, false, &yes, true, false},
		{"flagged", map[string]any{"_status": "published"}, false, true, &yes, false, false},
		{"a draft status is harmless", map[string]any{"_status": "draft"}, true, false, &yes, false, false},
		{"no drafts at all", map[string]any{"_status": "published"}, true, false, &no, false, false},
		{"unknown drafts is treated as drafts", map[string]any{"_status": "published"}, true, false, nil, true, true},
	}
	for _, c := range cases {
		w := applyBodyStatus(c.body, c.draft, c.flag, c.drafts)
		_, present := c.body["_status"]
		if (w != nil) != c.warn || present == c.removed {
			t.Errorf("%s: warning %v, _status present %v", c.name, w != nil, present)
		}
	}
}

// ---------------------------------------------------------------------------
// publish pre-validation: required paths through an array
// ---------------------------------------------------------------------------

func TestStillMissingThroughArrays(t *testing.T) {
	shard := &discovery.Shard{RequiredPaths: []string{"hero", "hero.links.link.label", "hero.type", "slug", "title"}}
	doc := func(s string) payload.Doc {
		var d payload.Doc
		if err := json.Unmarshal([]byte(s), &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	cases := []struct {
		doc  string
		want []string
	}{
		// The reported false positive: a fresh website-template page.
		{`{"title":"t","slug":"s","hero":{"type":"lowImpact","links":[]}}`, nil},
		{`{"title":"t","slug":"s","hero":{"type":"lowImpact","links":[{"link":{"label":"a"}},{"link":{"label":""}},{"link":{}}]}}`,
			[]string{"hero.links[1].link.label", "hero.links[2].link.label"}},
		{`{"title":"","slug":"s","hero":null}`, []string{"hero", "hero.links.link.label", "hero.type", "title"}},
		{`{"slug":"s","hero":{"type":"x","links":null}}`, []string{"hero.links.link.label", "title"}},
	}
	for _, c := range cases {
		if got := stillMissing(shard, doc(c.doc)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: missing = %v, want %v", c.doc, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// F7 stage B: data.diff on every dry run of a write to existing documents
// ---------------------------------------------------------------------------

func TestDryRunsCarryADiff(t *testing.T) {
	cms := newFakeCMS()
	srv := cms.server(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	run := func(args ...string) cliResult {
		r := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), Args: append(args, "--dry-run")})
		if r.Code != 0 {
			t.Fatalf("%v: exit %d\n%s", args, r.Code, r.Stdout)
		}
		return r
	}
	patches := func() int { return cms.srv.Count("PATCH", "/api/pages") + cms.srv.Count("POST", "/api/globals") }

	// update by id: base = the newest version, changes by PATCH semantics.
	r := run("update", "pages", "16", "--set", "title=Changed")
	diff := r.data(t)["diff"].(map[string]any)
	changes := diff["changes"].([]any)
	if diff["base"] != diffBaseLatest || fmt.Sprint(diff["id"]) != "16" || len(changes) != 1 ||
		changes[0].(map[string]any)["path"] != "title" || changes[0].(map[string]any)["to"] != "Changed" {
		t.Errorf("update diff = %v", diff)
	}

	// publish/unpublish: base = the stored (published) row.
	r = run("unpublish", "pages", "16", "--yes")
	diff = r.data(t)["diff"].(map[string]any)
	if diff["base"] != diffBasePublished || diff["status"] != "published" {
		t.Errorf("unpublish diff = %v", diff)
	}

	// bulk: the sampled documents, one batched read per side.
	r = run("update", "pages", "--where", "id in 16,17", "--set", "slug=x", "--yes")
	diff = r.data(t)["diff"].(map[string]any)
	docs := diff["documents"].([]any)
	if fmt.Sprint(diff["sampled"]) != "2" || len(docs) != 2 {
		t.Errorf("bulk diff = %v", diff)
	}

	// several explicit ids.
	r = run("publish", "pages", "16", "17", "--no-pre-validate")
	if docs := r.data(t)["diff"].(map[string]any)["documents"].([]any); len(docs) != 2 {
		t.Errorf("publish ids diff = %v", r.data(t)["diff"])
	}

	// a global.
	r = run("globals", "update", "header", "--set-json", `navItems=[{"link":{"label":"x"}}]`, "--yes")
	diff = r.data(t)["diff"].(map[string]any)
	if diff["identical"] != false || fmt.Sprint(diff["summary"].(map[string]any)["fields"]) != "[navItems]" {
		t.Errorf("globals diff = %v", diff)
	}

	// nothing was written by any of them.
	if n := patches(); n != 0 {
		t.Errorf("dry runs sent %d writes", n)
	}

	// a document that does not exist: the preview says so instead of failing.
	r = run("update", "pages", "18", "--set", "title=x")
	if _, ok := r.data(t)["diff"]; ok || !r.hasWarning(t, "diff_unavailable") {
		t.Errorf("missing document: %v %v", r.data(t)["diff"], r.warnings(t))
	}
}

func TestBackupsRestoreDryRunCarriesADiff(t *testing.T) {
	cms := newFakeCMS()
	srv := cms.server(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	dir := filepath.Join(t.TempDir(), "bk")
	env := backupEnv(srv.URL, dir)
	if r := cliRun(t, invocation{Home: home, Env: env, Args: []string{"update", "pages", "16", "--set", "title=Changed"}}); r.Code != 0 {
		t.Fatalf("update: %s", r.Stdout)
	}
	files := backupFiles(t, dir)
	r := cliRun(t, invocation{Home: home, Env: env, Args: []string{"backups", "restore", files[0], "--dry-run"}})
	if r.Code != 0 {
		t.Fatalf("exit %d\n%s", r.Code, r.Stdout)
	}
	diff := r.data(t)["diff"].(map[string]any)
	found := false
	for _, c := range diff["changes"].([]any) {
		m := c.(map[string]any)
		if m["path"] == "title" && m["from"] == "Changed" && m["to"] == "Home" {
			found = true
		}
	}
	if !found {
		t.Errorf("restore diff = %v", diff)
	}
}
