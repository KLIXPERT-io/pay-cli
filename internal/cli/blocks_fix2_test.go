package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
)

// Regression: every echo fold added the whole document again, so writing one
// page N times counted its rows N more times and turned fields a single real
// row fills into "likely required" (localhost: formBlock count 2 → 5 after
// three title-only updates of one page).
func TestEchoFoldsCountEachDocumentOnce(t *testing.T) {
	cms := newFakeCMS()
	srv := cms.server(t)
	home := t.TempDir()
	sc := seedDiscovery(t, home, srv.URL)
	store := cache.New(filepath.Join(home, "cache"))
	if ok, w := store.WriteCensus(sc, buildPagesCensus(t, sc.Key, questionsDocs())); !ok {
		t.Fatalf("seed: %v", w)
	}
	run := func(args ...string) cliResult {
		return cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), HTTP: srv.Client().Transport, Args: args})
	}
	census := func() *discovery.CensusEntity {
		t.Helper()
		raw, ok, _ := store.ReadCensus(sc)
		if !ok {
			t.Fatal("census gone")
		}
		c, _ := discovery.DecodeCensus(raw)
		e, _ := c.Entity(discovery.EntityKindCollection, "pages")
		return e
	}

	cms.pages["17"]["layout"] = []any{}
	if r := run("update", "pages", "17", "--set-json", "layout="+restructuredLayout("New")); r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	first := census()
	if first.Types["questions"].Count != 7 || first.Types["text"].Count != 14 || first.FoldedDocs != 1 {
		t.Fatalf("page 17 is new evidence: questions=%d text=%d folded=%d",
			first.Types["questions"].Count, first.Types["text"].Count, first.FoldedDocs)
	}
	// Three title-only updates: the echo carries the same layout each time.
	for _, title := range []string{"a", "b", "c"} {
		if r := run("update", "pages", "17", "--set", "title="+title); r.Code != 0 {
			t.Fatalf("exit %d: %s", r.Code, r.Stdout)
		}
	}
	after := census()
	for _, slug := range []string{"questions", "text", "group"} {
		if a, b := first.Types[slug], after.Types[slug]; a.Count != b.Count || a.Docs != b.Docs {
			t.Errorf("%s: count %d→%d docs %d→%d; re-writing one page must not re-count it", slug, a.Count, b.Count, a.Docs, b.Docs)
		}
	}
	if a, b := first.Paths["layout[].blocks"], after.Paths["layout[].blocks"]; a.Rows != b.Rows || a.Docs != b.Docs {
		t.Errorf("layout[].blocks rows %d→%d docs %d→%d", a.Rows, b.Rows, a.Docs, b.Docs)
	}
	if after.FoldedDocs != 1 {
		t.Errorf("folded_docs = %d, want 1 distinct document", after.FoldedDocs)
	}
}

// Regression: the write-path block check only ran when some row carried a
// blockType, so a body whose rows ALL lack one — which Payload drops with a
// 2xx, leaving layout [] — passed every dry run without a warning.
func TestWriteCheckRunsForUntypedRowsInABlocksField(t *testing.T) {
	cms := newFakeCMS()
	srv := cms.server(t)
	home := t.TempDir()
	sc := seedDiscovery(t, home, srv.URL)
	store := cache.New(filepath.Join(home, "cache"))
	if ok, w := store.WriteCensus(sc, buildPagesCensus(t, sc.Key, questionsDocs())); !ok {
		t.Fatalf("seed: %v", w)
	}
	run := func(args ...string) cliResult {
		return cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), HTTP: srv.Client().Transport, Args: args})
	}
	for _, tc := range []struct{ layout, code string }{
		{`[{}]`, discovery.IssueEmptyRow},
		{`[{"heading":"x"}]`, discovery.IssueMissingBlockType},
	} {
		for _, args := range [][]string{
			{"update", "pages", "16", "--set-json", "layout=" + tc.layout, "--dry-run"},
			{"create", "pages", "--set", "title=x", "--set-json", "layout=" + tc.layout, "--dry-run"},
		} {
			r := run(args...)
			if r.Code != 0 {
				t.Fatalf("%v: exit %d: %s", args, r.Code, r.Stdout)
			}
			if !r.hasWarning(t, tc.code) {
				t.Errorf("%v: want %s, got %v", args, tc.code, warningCodes(r))
			}
		}
	}
	// An empty layout and a plain array elsewhere are not block writes.
	r := run("update", "pages", "16", "--set-json", "layout=[]", "--set-json", `tags=[{"name":"x"}]`, "--dry-run")
	for _, code := range []string{discovery.IssueEmptyRow, discovery.IssueMissingBlockType, warnBlocksUnchecked} {
		if r.hasWarning(t, code) {
			t.Errorf("layout=[] / a plain array must not warn %s: %v", code, warningCodes(r))
		}
	}
}

// Regression: `pay blocks new … | pay blocks validate` ran structural checks
// only (the new envelope had no target) and, given --collection, checked the
// row at `layout` although the type only ever sits in nested fields.
func TestBlocksNewPipesIntoValidate(t *testing.T) {
	home := t.TempDir()
	seedCensus(t, home, seedDiscovery(t, home, testBaseURL))
	r := censusRun(t, home, "", "blocks", "new", "text", "--collection", "pages")
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	target, _ := r.Env["target"].(map[string]any)
	if target["slug"] != "pages" || target["kind"] != "collection" {
		t.Errorf("blocks new target = %v, want the collection the row was shaped for", r.Env["target"])
	}
	v := censusRun(t, home, r.Stdout, "blocks", "validate")
	if v.Code != 0 || v.hasWarning(t, warnCensusUnavailable) {
		t.Fatalf("validate of blocks new output: exit=%d warnings=%v\n%s", v.Code, warningCodes(v), v.Stdout)
	}
	d := v.data(t)
	if d["collection"] != "pages" || len(d["issues"].([]any)) != 0 {
		t.Errorf("a fresh text row checked where text is used must be clean: %v", d)
	}
	// Without --collection on `new`, used_in still names the entity.
	r = censusRun(t, home, "", "blocks", "new", "text")
	v = censusRun(t, home, r.Stdout, "blocks", "validate")
	if v.Code != 0 || v.hasWarning(t, warnCensusUnavailable) || v.data(t)["collection"] != "pages" {
		t.Errorf("validate without --collection: exit=%d warnings=%v data=%v", v.Code, warningCodes(v), v.data(t))
	}
}

// Regression: on a site where nothing is declared, `pay blocks add` accepted a
// typo or a type never seen at the position silently, even with a census
// cached; the mistake surfaced only at `pay apply`.
func TestBlocksAddWarnsFromTheCachedCensus(t *testing.T) {
	home := t.TempDir()
	seedCensus(t, home, seedDiscovery(t, home, testBaseURL))
	doc := pageEnvelope(t, `{"id":12,"layout":[{"id":"h","blockType":"hero","title":"x"},
	  {"id":"g","blockType":"group","blocks":[{"id":"t","blockType":"text","heading":"H"}]}]}`)
	warn := func(r cliResult, code string) map[string]any {
		t.Helper()
		if r.Code != 0 {
			t.Fatalf("exit %d: a census never gates an edit\n%s", r.Code, r.Stdout)
		}
		for _, w := range r.warnings(t) {
			if w["code"] == code {
				return w
			}
		}
		return nil
	}
	r := censusRun(t, home, doc, "blocks", "add", "heroo")
	if w := warn(r, discovery.IssueUnknownBlockType); w == nil || !strings.Contains(w["hint"].(string), `"hero"`) {
		t.Errorf("typo: want unknown_block_type with did-you-mean hero, got %v", r.warnings(t))
	}
	r = censusRun(t, home, doc, "blocks", "add", "hero", "--field", "layout[type:group].blocks")
	if w := warn(r, discovery.IssueBlockTypeNotObserved); w == nil || !strings.Contains(w["message"].(string), "group.blocks") {
		t.Errorf("misplaced: want block_type_not_observed_here against group.blocks, got %v", r.warnings(t))
	}
	for _, args := range [][]string{
		{"blocks", "add", "text", "--field", "layout[type:group].blocks"},
		{"blocks", "add", "hero"},
	} {
		r = censusRun(t, home, doc, args...)
		if warn(r, discovery.IssueBlockTypeNotObserved) != nil || warn(r, discovery.IssueUnknownBlockType) != nil {
			t.Errorf("%v: an observed placement must not warn: %v", args, r.warnings(t))
		}
	}
	r = censusRun(t, home, doc, "blocks", "set", "id:t", "--set", "blockType=textt", "--field", "layout[type:group].blocks")
	if warn(r, discovery.IssueUnknownBlockType) == nil {
		t.Errorf("set blockType typo: %v", r.warnings(t))
	}
}
