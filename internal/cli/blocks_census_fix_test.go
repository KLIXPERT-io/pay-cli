package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// questionsDocs is the census the stage agent had: six pages with a questions
// block at the TOP level of layout, intro filled in.
func questionsDocs() []string {
	var out []string
	for i := 46; i <= 51; i++ {
		out = append(out, fmt.Sprintf(`{"id":%d,"layout":[{"id":"t%d","blockType":"text","heading":"H%d","textSize":"regular"},`+
			`{"id":"g%d","blockType":"group","blocks":[{"id":"gt%d","blockType":"text","heading":"G","textSize":"large"}]},`+
			`{"id":"q%d","blockType":"questions","intro":"Intro %d","items":[{"id":"i%d","q":"Q"}]}]}`, i, i, i, i, i, i, i, i))
	}
	return out
}

// restructuredLayout: questions moved into the group, intro left empty — what
// the pages hold after the restructure the census never saw.
func restructuredLayout(heading string) string {
	return `[{"blockType":"text","heading":"` + heading + `","textSize":"regular"},` +
		`{"blockType":"group","blocks":[{"blockType":"text","heading":"G","textSize":"large"},` +
		`{"blockType":"questions","intro":null,"items":[{"q":"Q"}]}]}]`
}

func decodeLayout(t *testing.T, s string) []any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v []any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// The root cause of the stage `questions` false positive: writes validated
// against a stale census and ignored the document they had just fetched, and
// PayCLI's own writes never reached the census.
func TestWritesTrustThePreImageAndTheirOwnEchoOverAStaleCensus(t *testing.T) {
	cms := newFakeCMS()
	cms.pages["16"]["layout"] = decodeLayout(t, restructuredLayout("Old heading"))
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
	body := restructuredLayout("New heading")

	// Without a pre-image (a create), the stale census still speaks — and its
	// hint names the command that renews it first.
	r := run("create", "pages", "--set", "title=x", "--set-json", "layout="+body, "--dry-run")
	if !r.hasWarning(t, discovery.IssueBlockTypeNotObserved) || !r.hasWarning(t, discovery.IssueLikelyRequired) {
		t.Fatalf("precondition: the stale census warns on create: %v", warningCodes(r))
	}
	for _, w := range r.warnings(t) {
		if w["code"] == discovery.IssueBlockTypeNotObserved && !strings.HasPrefix(fmt.Sprint(w["hint"]), "`pay blocks learn pages` first") {
			t.Errorf("hint must lead with learn: %v", w["hint"])
		}
	}

	// update --dry-run fetched page 16, which holds exactly these rows.
	r = run("update", "pages", "16", "--set-json", "layout="+body, "--dry-run")
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	for _, code := range []string{discovery.IssueBlockTypeNotObserved, discovery.IssueLikelyRequired, warnBlocksUnchecked} {
		if r.hasWarning(t, code) {
			t.Errorf("the pre-image contradicts %s: %v", code, r.warnings(t))
		}
	}

	// A real write folds its echo into the census on disk ...
	cms.pages["17"]["layout"] = []any{}
	r = run("update", "pages", "17", "--set-json", "layout="+body)
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	raw, ok, _ := store.ReadCensus(sc)
	if !ok {
		t.Fatal("census gone")
	}
	c, _ := discovery.DecodeCensus(raw)
	e, _ := c.Entity(discovery.EntityKindCollection, "pages")
	if e.FoldedDocs != 1 || e.Paths["layout[].blocks"].Types["questions"] != 1 || !e.BuiltAt.Equal(buildPagesCensus(t, sc.Key, nil).Collections["pages"].BuiltAt) {
		t.Fatalf("echo not folded: folded=%d paths=%+v", e.FoldedDocs, e.Paths["layout[].blocks"])
	}
	// ... so the next write of the same shape is clean even without a pre-image.
	r = run("create", "pages", "--set", "title=y", "--set-json", "layout="+body, "--dry-run")
	for _, code := range []string{discovery.IssueBlockTypeNotObserved, discovery.IssueLikelyRequired} {
		if r.hasWarning(t, code) {
			t.Errorf("after the fold %s still fires: %v", code, r.warnings(t))
		}
	}
	// A dry run writes nothing, and folds nothing.
	raw2, _, _ := store.ReadCensus(sc)
	if string(raw2) != string(raw) {
		t.Error("a dry run changed the census")
	}
}

// A census that holds ANOTHER entity only (forms, whose `text` is the
// form-builder's field block) must not be applied to a pages write.
func TestWritesIgnoreAnotherEntitysCensus(t *testing.T) {
	home := t.TempDir()
	sc := seedDiscovery(t, home, testBaseURL)
	fb := discovery.NewCensusBuilder(discovery.EntityKindCollection, "forms")
	for i := 1; i <= 7; i++ {
		var d map[string]any
		dec := json.NewDecoder(strings.NewReader(fmt.Sprintf(`{"id":%d,"fields":[{"blockType":"text","name":"n","label":"L%d","width":50,"required":true},`+
			`{"blockType":"text","name":"m","label":"M%d","width":100,"required":false}]}`, i, i, i)))
		dec.UseNumber()
		if err := dec.Decode(&d); err != nil {
			t.Fatal(err)
		}
		fb.AddDoc(d["id"], d)
	}
	c := discovery.NewCensus(sc.Key)
	c.Put(fb.Finish())
	if ok, w := cache.New(filepath.Join(home, "cache")).WriteCensus(sc, c); !ok {
		t.Fatalf("seed: %v", w)
	}
	r := censusRun(t, home, "", "create", "pages", "--set", "title=x", "--set-json",
		`layout=[{"blockType":"text","heading":"H","content":null}]`, "--dry-run")
	got := warningCodes(r)
	if !r.hasWarning(t, warnBlocksUnchecked) {
		t.Errorf("pages was never censused: blocks_unchecked expected, got %v", got)
	}
	for _, code := range []string{discovery.IssueUnknownField, discovery.IssueLikelyRequired, discovery.IssueBlockTypeNotObserved} {
		if r.hasWarning(t, code) {
			t.Errorf("forms' shape leaked into a pages write (%s): %v", code, r.warnings(t))
		}
	}
}

// `pay blocks validate --field` accepts every grammar `--field` advertises and
// rejects a field the entity does not have.
func TestBlocksValidateFieldGrammar(t *testing.T) {
	home := t.TempDir()
	seedCensus(t, home, seedDiscovery(t, home, testBaseURL))
	row := `{"blockType":"text","heading":"H","textSize":"regular"}`
	dir := t.TempDir()
	file := filepath.Join(dir, "row.json")
	if err := os.WriteFile(file, []byte(row), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"layout[].blocks", "layout.1.blocks", "layout[1].blocks", "layout[type:group[1]].blocks",
		"group.blocks", "layout[].blocks[].blocks", "grid.blocks"} {
		r := censusRun(t, home, "", "blocks", "validate", "--data-file", file, "--collection", "pages", "--field", field)
		d := r.data(t)
		if r.Code != 0 || fmt.Sprint(d["warnings"]) != "0" || fmt.Sprint(d["checked_rows"]) != "1" {
			t.Errorf("--field %s: exit %d %v", field, r.Code, d["issues"])
		}
	}
	r := censusRun(t, home, "", "blocks", "validate", "--data-file", file, "--collection", "pages", "--field", "nosuchfield")
	if r.Code != 5 || r.code(t) != "invalid_option" {
		t.Errorf("unknown --field: exit %d %s", r.Code, r.Stdout)
	}
	// The row really is placed: a hero where only text and grid were seen.
	hero := filepath.Join(dir, "hero.json")
	_ = os.WriteFile(hero, []byte(`{"blockType":"hero","title":"t"}`), 0o600)
	r = censusRun(t, home, "", "blocks", "validate", "--data-file", hero, "--collection", "pages", "--field", "layout.1.blocks")
	if !strings.Contains(r.Stdout, "observed there: text, grid") && !strings.Contains(r.Stdout, "observed there: grid, text") {
		t.Errorf("placement not checked at layout[].blocks: %s", r.Stdout)
	}
}

// plan (and so sync) checks each item against the document it fetched to
// decide the action: the stage `questions` warnings came from exactly this.
func TestPlanChecksBlocksAgainstTheFetchedDocument(t *testing.T) {
	m := newMemCMS(t)
	m.seed("pages", 16, map[string]any{"title": "Home", "slug": "home", "_status": "published",
		"layout": decodeLayout(t, restructuredLayout("Old heading"))})
	home := t.TempDir()
	sc := seedDiscovery(t, home, m.srv.URL)
	if ok, w := cache.New(filepath.Join(home, "cache")).WriteCensus(sc, buildPagesCensus(t, sc.Key, questionsDocs())); !ok {
		t.Fatalf("seed: %v", w)
	}
	dir := t.TempDir()
	writeManifest(t, dir, "home.json", `{"collection":"pages","match":["slug"],"data":{"slug":"home","layout":`+
		restructuredLayout("New heading")+`}}`)
	res := m.run(t, home, nil, "plan", dir)
	if res.Code != 0 {
		t.Fatalf("exit %d\n%s", res.Code, res.Stdout)
	}
	it := itemByKey(t, planItems(t, res), "pages/home")
	if it["action"] != "update" {
		t.Fatalf("action = %v", it["action"])
	}
	ws, _ := it["warnings"].([]any)
	for _, w := range ws {
		code := fmt.Sprint(w.(map[string]any)["code"])
		if code == discovery.IssueBlockTypeNotObserved || code == discovery.IssueLikelyRequired {
			t.Errorf("the fetched page holds these rows, yet plan warned: %v", w)
		}
	}
}

// `blocks new bentoItem` used to copy another card's link into the template
// (link was taken for a select) and did not ask for one.
func TestNewRowAsksForALinkInsteadOfCopyingOne(t *testing.T) {
	b := discovery.NewCensusBuilder(discovery.EntityKindCollection, "pages")
	links := []string{"/skills/a", "/skills/a", "/skills/b", "/skills/b", "/x/c", "/x/c", "/skills/a", "/x/c"}
	for i, l := range links {
		b.AddDoc(i+1, map[string]any{"layout": []any{map[string]any{"blockType": "bentoItem", "link": l,
			"heading": fmt.Sprint("H", i), "size": []string{"default", "higher"}[i%2]}}})
	}
	e := b.Finish()
	stats := e.Types["bentoItem"]
	row, chosen := discovery.NewBlockRow("bentoItem", stats, nil)
	view := discovery.BuildSchemaView("bentoItem", stats, nil, nil)
	if row["link"] != nil || strings.Contains(strings.Join(chosen, ","), "link") {
		t.Errorf("row = %v chosen = %v", row, chosen)
	}
	got := fillIn(view, row)
	if !strings.Contains(strings.Join(got, ","), "link") {
		t.Errorf("fill_in = %v, want link", got)
	}
}

// Parallel `pay blocks learn ctas & learn forms & learn pages` kept only the
// last writer's entity: each process wrote back the copy it loaded before the
// others finished.
func TestCensusWritesMergeWithWhatIsOnDisk(t *testing.T) {
	home := t.TempDir()
	sc := seedDiscovery(t, home, testBaseURL)
	store := cache.New(filepath.Join(home, "cache"))
	older := discovery.NewCensus(sc.Key)
	forms := discovery.NewCensusEntity(discovery.EntityKindCollection, "forms")
	forms.BuiltAt = buildPagesCensus(t, sc.Key, nil).Collections["pages"].BuiltAt
	older.Put(forms)

	// This process loaded an empty census, then built pages; meanwhile
	// another process wrote forms.
	mine := buildPagesCensus(t, sc.Key, censusPageDocs())
	pages, _ := mine.Entity(discovery.EntityKindCollection, "pages")
	if ok, w := store.WriteCensus(sc, older); !ok {
		t.Fatal(w)
	}
	var merged *discovery.Census
	ok, w := store.UpdateCensus(sc, func(raw []byte) (any, error) {
		merged = mergeBuiltCensus(raw, mine, []*discovery.CensusEntity{pages}, false)
		return merged, nil
	})
	if !ok {
		t.Fatal(w)
	}
	raw, _, _ := store.ReadCensus(sc)
	got, _ := discovery.DecodeCensus(raw)
	if _, ok := got.Entity(discovery.EntityKindCollection, "forms"); !ok {
		t.Error("the other process's entity was dropped")
	}
	if _, ok := got.Entity(discovery.EntityKindCollection, "pages"); !ok {
		t.Error("this process's entity was not written")
	}
	// --refresh replaces the whole census.
	if r := mergeBuiltCensus(raw, mine, []*discovery.CensusEntity{pages}, true); r != mine {
		t.Error("--refresh must discard the rest")
	}
	// A newer read by another process wins over an older one of ours.
	newer := discovery.NewCensus(sc.Key)
	np := *pages
	np.BuiltAt = pages.BuiltAt.Add(1)
	np.BlockRows = 999
	newer.Put(&np)
	b, _ := json.Marshal(newer)
	if r := mergeBuiltCensus(b, mine, []*discovery.CensusEntity{pages}, false); r.Collections["pages"].BlockRows != 999 {
		t.Error("an older read overwrote a newer one")
	}
}

// A read cut short by the budget before it found a block row is the only sign
// that "no blocks here" may be wrong; it must say so.
func TestCensusTruncatedWarnsEvenWithoutBlockRows(t *testing.T) {
	home := t.TempDir()
	var docs []string
	for i := 1; i <= 30; i++ {
		docs = append(docs, fmt.Sprintf(`{"id":%d,"layout":[]}`, i))
	}
	srv := pagesServer(t, docs)
	seedDiscovery(t, home, srv.URL)
	r := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), HTTP: srv.Client().Transport,
		Args: []string{"blocks", "learn", "pages", "--census-max-docs", "10"}})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	if !r.hasWarning(t, warnCensusTruncated) {
		t.Errorf("no census_truncated for a budget-truncated blockless read: %v", warningCodes(r))
	}
}

// describe --block with a typo on a site without block slugs in its schema
// suggests from the census and names the command that lists them.
func TestDescribeBlockTypoSuggestsFromTheCensus(t *testing.T) {
	home := t.TempDir()
	seedCensus(t, home, seedDiscovery(t, home, testBaseURL))
	r := censusRun(t, home, "", "describe", "pages", "--block", "heroo")
	if r.code(t) != "invalid_option" || !strings.Contains(r.Stdout, `"hero"`) || !strings.Contains(r.Stdout, "pay blocks types pages") {
		t.Errorf("describe --block heroo: %s", r.Stdout)
	}
}

// Every cached reference of a type moved (a restructure): example re-reads the
// holders once instead of failing with feature_unavailable.
func TestBlocksExampleRelearnsWhenEveryReferenceMoved(t *testing.T) {
	home := t.TempDir()
	moved := `{"id":1,"layout":[{"id":"g","blockType":"group","blocks":[{"id":"t","blockType":"text","heading":"H","textSize":"regular"}]},` +
		`{"id":"h","blockType":"hero","title":"One","variant":"mid","textSize":"regular","content":` + censusLexical + `,"buttons":[],"image":{"file":null,"aspectRatio":"1/1"}}]}`
	list := `{"docs":[` + moved + `],"totalDocs":1,"limit":100,"page":1,"hasNextPage":false}`
	srv := payloadtest.NewServer(t,
		payloadtest.WithHandler("GET", "/api/pages", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(list))
		}),
		payloadtest.WithHandler("GET", "/api/pages/1", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(moved))
		}))
	seedCensus(t, home, seedDiscovery(t, home, srv.URL)) // the census saw the group at layout[1]
	r := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), HTTP: srv.Client().Transport,
		Args: []string{"blocks", "example", "group", "--collection", "pages"}})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	d := r.data(t)
	if d["census"].(map[string]any)["revalidated"] != true {
		t.Errorf("no relearn: %v", d["census"])
	}
	ex := d["examples"].([]any)[0].(map[string]any)
	if ex["ref"].(map[string]any)["path"] != "layout[0]" {
		t.Errorf("example = %v", ex)
	}
}
