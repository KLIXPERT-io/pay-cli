package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// noNetwork fails a test that reaches the wire: every census answer below must
// come from the cache the test seeded.
type noNetwork struct{ t *testing.T }

func (n noNetwork) RoundTrip(r *http.Request) (*http.Response, error) {
	n.t.Errorf("unexpected request: %s %s", r.Method, r.URL)
	return nil, fmt.Errorf("no network in this test")
}

const censusLexical = `{"root":{"type":"root","children":[{"type":"paragraph","children":[{"type":"text","text":"Hi","format":0,"detail":0,"mode":"normal","style":"","version":1}],"direction":"ltr","format":"","indent":0,"version":1}],"direction":"ltr","format":"","indent":0,"version":1}}`

// censusPageDocs are what a klixpert-like site's pages look like at depth 0.
func censusPageDocs() []string {
	hero := func(title, size string) string {
		return `{"id":"h` + title + `","blockType":"hero","blockName":null,"title":"` + title + `","variant":"mid","textSize":"` + size +
			`","content":` + censusLexical + `,"buttons":[],"image":{"file":null,"aspectRatio":"1/1"}}`
	}
	return []string{
		`{"id":1,"layout":[` + hero("One", "regular") + `,{"id":"g","blockType":"group","blocks":[{"id":"t","blockType":"text","heading":"H","textSize":"regular"},{"id":"gr","blockType":"grid","columnsDesktop":3,"blocks":[{"id":"t2","blockType":"text","heading":"I","textSize":"large"}]}]}]}`,
		`{"id":2,"layout":[` + hero("Two", "regular") + `]}`,
		`{"id":3,"layout":[` + hero("Three", "large") + `]}`,
		`{"id":4,"layout":[` + hero("Four", "regular") + `]}`,
	}
}

func buildPagesCensus(t *testing.T, scope string, docs []string) *discovery.Census {
	t.Helper()
	b := discovery.NewCensusBuilder(discovery.EntityKindCollection, "pages")
	for _, raw := range docs {
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.UseNumber()
		var d map[string]any
		if err := dec.Decode(&d); err != nil {
			t.Fatalf("fixture: %v", err)
		}
		b.AddDoc(d["id"], d)
	}
	e := b.Finish()
	e.BuiltAt = payloadtest.Epoch
	e.Draft = true
	e.MaxDocs = discovery.DefaultCensusMaxDocs
	c := discovery.NewCensus(scope)
	c.Put(e)
	return c
}

// seedCensus writes a pages census for the scope seedDiscovery used.
func seedCensus(t *testing.T, home string, sc cache.Scope) {
	t.Helper()
	store := cache.New(filepath.Join(home, "cache"))
	if ok, w := store.WriteCensus(sc, buildPagesCensus(t, sc.Key, censusPageDocs())); !ok {
		t.Fatalf("seed census: %v", w)
	}
}

// pagesServer answers GET /api/pages with docs, the way a census reads them.
func pagesServer(t *testing.T, docs []string) *payloadtest.Server {
	t.Helper()
	body := fmt.Sprintf(`{"docs":[%s],"totalDocs":%d,"limit":100,"page":1,"hasNextPage":false}`, strings.Join(docs, ","), len(docs))
	return payloadtest.NewServer(t, payloadtest.WithHandler("GET", "/api/pages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func censusRun(t *testing.T, home string, stdin string, args ...string) cliResult {
	t.Helper()
	return cliRun(t, invocation{Home: home, Env: seededEnv(testBaseURL), HTTP: noNetwork{t}, Args: args, Stdin: stdin})
}

func TestBlocksTypesAnswersFromTheCachedCensus(t *testing.T) {
	home := t.TempDir()
	seedCensus(t, home, seedDiscovery(t, home, testBaseURL))

	r := censusRun(t, home, "", "blocks", "types", "pages")
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	if r.Env["data_kind"] != "op_result" {
		t.Errorf("data_kind = %v", r.Env["data_kind"])
	}
	d := r.data(t)
	census, _ := d["census"].(map[string]any)
	if census["source"] != "cache" || census["requests"] != json.Number("0") {
		t.Errorf("census = %v", census)
	}
	ents, _ := d["entities"].([]any)
	ent, _ := ents[0].(map[string]any)
	fields, _ := ent["fields"].([]any)
	paths := []string{}
	for _, f := range fields {
		fm := f.(map[string]any)
		paths = append(paths, fm["path"].(string))
		if fm["path"] == "layout" && (fm["source"] != discovery.SourceObservedCensus || fm["authoritative"] != false) {
			t.Errorf("layout = %v", fm)
		}
	}
	if strings.Join(paths, ",") != "layout,layout[].blocks,layout[].blocks[].blocks" {
		t.Errorf("paths = %v", paths)
	}
	if got := toStrings(d["block_types"]); strings.Join(got, ",") != "grid,group,hero,text" {
		t.Errorf("block_types = %v", got)
	}

	r = censusRun(t, home, "", "blocks", "types", "pages", "--field", "group.blocks")
	ent = r.data(t)["entities"].([]any)[0].(map[string]any)
	f := ent["fields"].([]any)[0].(map[string]any)
	if f["path"] != "group.blocks" || f["parent"] != "group" {
		t.Errorf("parent-scoped field = %v", f)
	}

	r = censusRun(t, home, "", "blocks", "types", "pages", "--field", "layout[].nope")
	if r.code(t) != "invalid_option" || r.Code != 5 {
		t.Errorf("unknown field: code=%s exit=%d", r.code(t), r.Code)
	}
}

func TestBlocksSchemaAndNew(t *testing.T) {
	home := t.TempDir()
	seedCensus(t, home, seedDiscovery(t, home, testBaseURL))

	r := censusRun(t, home, "", "blocks", "schema", "hero", "--collection", "pages")
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	schema := r.data(t)["schema"].(map[string]any)
	if schema["count"] != json.Number("4") || strings.Join(toStrings(schema["likely_required"]), ",") != "content,title" {
		t.Errorf("schema = %v", schema)
	}
	var textSize map[string]any
	for _, f := range schema["fields"].([]any) {
		if f.(map[string]any)["path"] == "textSize" {
			textSize = f.(map[string]any)
		}
	}
	if textSize["enum_like"] != true || textSize["presence_pct"] != json.Number("100") {
		t.Errorf("textSize = %v", textSize)
	}

	r = censusRun(t, home, "", "blocks", "schema", "heroo", "--collection", "pages")
	if r.code(t) != "block_type_unknown" || r.Code != 10 {
		t.Fatalf("typo: code=%s exit=%d", r.code(t), r.Code)
	}
	if dym := toStrings(r.Env["error"].(map[string]any)["did_you_mean"]); len(dym) == 0 || dym[0] != "hero" {
		t.Errorf("did_you_mean = %v", dym)
	}

	r = censusRun(t, home, "", "blocks", "new", "hero", "--collection", "pages",
		"--set", "title=Hallo", "--set", "image.alt=Bild", "--set-json", `buttons=[{"label":"Go"}]`)
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	d := r.data(t)
	row := d["row"].(map[string]any)
	if row["blockType"] != "hero" || row["title"] != "Hallo" || row["textSize"] != "regular" || row["variant"] != "mid" {
		t.Errorf("row = %v", row)
	}
	if img := row["image"].(map[string]any); img["alt"] != "Bild" || img["aspectRatio"] != "1/1" {
		t.Errorf("dotted --set did not reach the group: %v", img)
	}
	if _, hasID := row["id"]; hasID {
		t.Error("a new row must carry no id")
	}
	if got := toStrings(d["fill_in"]); strings.Join(got, ",") != "content" {
		t.Errorf("fill_in = %v (title was set; content is Payload's empty value)", got)
	}
}

func TestBlocksValidateSeverityAndExit(t *testing.T) {
	home := t.TempDir()
	// Findings the census only observed make validate rebuild it once, so
	// this server answers with the very documents the census was built from:
	// the rebuild must confirm, not erase, every finding below.
	srv := pagesServer(t, censusPageDocs())
	seedCensus(t, home, seedDiscovery(t, home, srv.URL))
	censusRun := func(t *testing.T, home, stdin string, args ...string) cliResult {
		t.Helper()
		return cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), HTTP: srv.Client().Transport, Args: args, Stdin: stdin})
	}

	bad := pageEnvelope(t, `{"id":12,"layout":[{"blockType":"heroo"},{"blockType":"group","blocks":[{}]}]}`)
	r := censusRun(t, home, bad, "blocks", "validate")
	if r.Code != 5 || r.code(t) != "validation_failed" {
		t.Fatalf("errors must exit 5: exit=%d %s", r.Code, r.Stdout)
	}
	d := r.data(t)
	if d["errors"] != json.Number("1") || d["warnings"] != json.Number("1") || d["collection"] != "pages" {
		t.Errorf("report = %v", d)
	}
	codes := map[string]string{}
	for _, is := range d["issues"].([]any) {
		m := is.(map[string]any)
		codes[m["code"].(string)] = m["severity"].(string)
	}
	if codes["empty_row"] != "error" || codes["unknown_block_type"] != "warning" {
		t.Errorf("issues = %v", codes)
	}

	good := pageEnvelope(t, `{"id":12,"layout":[{"blockType":"hero","title":"x","content":`+censusLexical+`}]}`)
	r = censusRun(t, home, good, "blocks", "validate")
	if r.Code != 0 || r.data(t)["checked_rows"] != json.Number("1") {
		t.Fatalf("a clean document: exit=%d %s", r.Code, r.Stdout)
	}

	// No collection: structural checks only, and it says so.
	r = censusRun(t, home, `{"layout":[{"blockType":"x"},{}]}`, "blocks", "validate")
	if r.Code != 5 || !r.hasWarning(t, warnCensusUnavailable) {
		t.Errorf("structural-only run: exit=%d warnings=%v", r.Code, r.warnings(t))
	}

	// `pay blocks new` output validates as the row it carries.
	newEnv := `{"ok":true,"v":1,"command":"blocks new","data_kind":"op_result","meta":{},"data":{"row":{"blockType":"hero","titel":"x"}}}`
	r = censusRun(t, home, newEnv, "blocks", "validate", "--collection", "pages", "--field", "layout")
	if r.Code != 0 || r.data(t)["checked_rows"] != json.Number("1") {
		t.Fatalf("blocks new output: exit=%d %s", r.Code, r.Stdout)
	}
	found := false
	for _, is := range r.data(t)["issues"].([]any) {
		if is.(map[string]any)["code"] == "unknown_field" {
			found = true
		}
	}
	if !found {
		t.Errorf("the misspelt key was not reported: %v", r.data(t)["issues"])
	}
}

func TestBlocksAddTemplate(t *testing.T) {
	home := t.TempDir()
	seedCensus(t, home, seedDiscovery(t, home, testBaseURL))
	r := censusRun(t, home, pageEnvelope(t, pageDoc), "blocks", "add", "hero", "--template", "--set", "title=T", "--first")
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	first := r.data(t)["layout"].([]any)[0].(map[string]any)
	if first["blockType"] != "hero" || first["title"] != "T" || first["textSize"] != "regular" {
		t.Errorf("templated row = %v", first)
	}
	if _, ok := first["content"].(map[string]any); !ok {
		t.Errorf("rich text missing from the template: %v", first)
	}
	r = censusRun(t, home, pageEnvelope(t, pageDoc), "blocks", "add", "nope", "--template")
	if r.code(t) != "block_type_unknown" {
		t.Errorf("unknown template type: %s", r.Stdout)
	}
}

func TestWritesUseTheCensusInsteadOfWarningOnEveryBlock(t *testing.T) {
	home := t.TempDir()
	sc := seedDiscovery(t, home, testBaseURL)

	create := func(layout string) cliResult {
		return censusRun(t, home, "", "create", "pages", "--set", "title=x", "--set-json", "layout="+layout, "--dry-run")
	}
	// No census, no declared list: one blocks_unchecked, never a claim that a
	// real block type is unknown.
	r := create(`[{"blockType":"hero"},{"blockType":"group","blocks":[{"blockType":"text"}]}]`)
	if !r.hasWarning(t, warnBlocksUnchecked) || r.hasWarning(t, warnUnknownBlockType) {
		t.Fatalf("no knowledge: %v", r.warnings(t))
	}

	seedCensus(t, home, sc)
	r = create(`[{"blockType":"hero","title":"x","content":` + censusLexical + `},{"blockType":"group","blocks":[{"blockType":"text"}]}]`)
	for _, w := range r.warnings(t) {
		if w["code"] != "anonymous_session" {
			t.Errorf("observed block types must write silently: %v", w)
		}
	}
	r = create(`[{"blockType":"heroo"},{"blockType":"group","blocks":[{"blockType":"grid","colums":3}]}]`)
	for _, code := range []string{warnUnknownBlockType, "unknown_field"} {
		if !r.hasWarning(t, code) {
			t.Errorf("missing %s: %v", code, r.warnings(t))
		}
	}
	if r.hasWarning(t, warnBlocksUnchecked) {
		t.Error("blocks_unchecked with a census cached")
	}
}

func TestDescribeFallsBackToTheCensus(t *testing.T) {
	home := t.TempDir()
	seedCensus(t, home, seedDiscovery(t, home, testBaseURL))

	r := censusRun(t, home, "", "describe", "pages", "--field", "layout[0].blocks")
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	d := r.data(t)
	if d["blocks_source"] != discovery.SourceObservedCensus || strings.Join(toStrings(d["block_types"]), ",") != "grid,text" {
		t.Errorf("nested field = %v %v", d["blocks_source"], d["block_types"])
	}
	r = censusRun(t, home, "", "describe", "pages", "--field", "layout")
	if strings.Join(toStrings(r.data(t)["block_types"]), ",") != "hero,group" {
		t.Errorf("a blocks field with no declared slugs = %v", r.data(t)["block_types"])
	}
	r = censusRun(t, home, "", "describe", "pages", "--block", "grid")
	if r.Code != 0 || r.data(t)["block_source"] != discovery.SourceObservedCensus {
		t.Fatalf("--block fallback: %s", r.Stdout)
	}
}

func TestBlocksLearnBuildsPersistsAndDiffs(t *testing.T) {
	home := t.TempDir()
	docs := `{"docs":[` + strings.Join(censusPageDocs(), ",") + `,{"id":5,"layout":[{"id":"q","blockType":"quote","text":"q"}]}],"totalDocs":5,"limit":100,"page":1,"hasNextPage":false}`
	srv := payloadtest.NewServer(t, payloadtest.WithHandler("GET", "/api/pages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(docs))
	}))
	sc := seedDiscovery(t, home, srv.URL)
	store := cache.New(filepath.Join(home, "cache"))
	if ok, w := store.WriteCensus(sc, buildPagesCensus(t, sc.Key, censusPageDocs()[:1])); !ok {
		t.Fatalf("seed: %v", w)
	}
	r := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), HTTP: srv.Client().Transport,
		Args: []string{"blocks", "learn", "pages"}})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	d := r.data(t)
	msgs := []string{}
	for _, c := range d["changes"].([]any) {
		msgs = append(msgs, c.(map[string]any)["message"].(string))
	}
	if !strings.Contains(strings.Join(msgs, "\n"), `new blockType "quote"`) {
		t.Errorf("changes = %v", msgs)
	}
	census := d["census"].(map[string]any)
	if census["source"] != "built" || census["persisted"] != true {
		t.Errorf("census = %v", census)
	}
	if n := srv.Count("GET", "/api/pages"); n < 1 {
		t.Errorf("learn did not read pages (%d requests)", n)
	}
	raw, ok, _ := store.ReadCensus(sc)
	c, decoded := discovery.DecodeCensus(raw)
	if !ok || !decoded {
		t.Fatal("learn did not persist the census")
	}
	if e, _ := c.Entity(discovery.EntityKindCollection, "pages"); e.Types["quote"] == nil || !e.Draft {
		t.Errorf("persisted census = %+v", e)
	}
}

func TestIntrospectionDisabledDoctorCheckNamesTheRealOption(t *testing.T) {
	c := introspectionDisabledCheck(nil)
	if c.Status != CheckWarn || !strings.Contains(c.Hint, "disableIntrospectionInProduction: false") {
		t.Fatalf("check = %+v", c)
	}
	for _, want := range []string{"required flags", "block slugs", "pay blocks types"} {
		if !strings.Contains(c.Detail+c.Hint, want) {
			t.Errorf("doctor does not say %q: %+v", want, c)
		}
	}
}

// Every `pay …` command a census hint names must exist (§7.10's hint rule).
func TestCensusHintsNameRealCommands(t *testing.T) {
	root := newRootCommand(&Runtime{})
	for _, path := range [][]string{
		{"blocks", "types"}, {"blocks", "schema"}, {"blocks", "example"}, {"blocks", "new"},
		{"blocks", "validate"}, {"blocks", "learn"}, {"blocks", "rm"}, {"blocks", "add"}, {"discover"},
	} {
		cmd, _, err := root.Find(path)
		if err != nil || cmd == nil || cmd.Name() != path[len(path)-1] {
			t.Errorf("hint command %v does not resolve", path)
		}
	}
	if f := root.Flags().Lookup("census-max-docs"); f != nil {
		t.Error("--census-max-docs must be a catalog flag, not a root flag")
	}
	cmd, _, _ := root.Find([]string{"blocks", "add"})
	if cmd.Flags().Lookup("template") == nil {
		t.Error("blocks add has no --template")
	}
}

// A block type deployed after the census reads as unknown until the census is
// rebuilt. validate rebuilds once when — and only when — an observed-only
// finding appears, so a stale census cannot produce a false warning there.
func TestBlocksValidateRebuildsAStaleCensusOnAnAnomaly(t *testing.T) {
	home := t.TempDir()
	srv := pagesServer(t, append(censusPageDocs(), `{"id":5,"layout":[{"id":"q","blockType":"quote","text":"q"}]}`))
	seedCensus(t, home, seedDiscovery(t, home, srv.URL))
	// An hour after the census was read: fresh (24 h), but past the reactive
	// rebuild's cooldown.
	later := func() time.Time { return payloadtest.Epoch.Add(time.Hour) }
	run := func(doc string) cliResult {
		return cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), HTTP: srv.Client().Transport,
			Args: []string{"blocks", "validate"}, Stdin: pageEnvelope(t, doc), Clock: later})
	}

	// A clean document never triggers a rebuild.
	r := run(`{"id":1,"layout":[{"blockType":"hero","title":"x","content":` + censusLexical + `}]}`)
	if r.Code != 0 || srv.Count("GET", "/api/pages") != 0 {
		t.Fatalf("clean doc: exit=%d requests=%d", r.Code, srv.Count("GET", "/api/pages"))
	}
	// The new type: rebuilt once, then known.
	r = run(`{"id":5,"layout":[{"blockType":"quote","text":"q"}]}`)
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	d := r.data(t)
	if d["warnings"] != json.Number("0") || d["census"].(map[string]any)["revalidated"] != true {
		t.Errorf("stale census was not rebuilt: %v", d)
	}
	if srv.Count("GET", "/api/pages") == 0 {
		t.Error("no rebuild request was made")
	}
}

// TestNestedFieldAbsentKnownFromTheCensus is F4 over F1: on a site without
// block schemas (introspection disabled), the cached census is what says a row
// type has a nested blocks field, so an absent one is empty, not a typo. The
// census is read from the cache only; a pipe stage never builds it.
func TestNestedFieldAbsentKnownFromTheCensus(t *testing.T) {
	home := t.TempDir()
	seedCensus(t, home, seedDiscovery(t, home, testBaseURL))
	doc := `{"id":5,"layout":[{"id":"g","blockType":"grid","columnsDesktop":3}]}`

	r := censusRun(t, home, pageEnvelope(t, doc), "blocks", "ls", "--field", "layout[0].blocks")
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	var msg string
	for _, w := range r.warnings(t) {
		if w["code"] == WarnFieldAbsent {
			msg, _ = w["message"].(string)
		}
	}
	if !strings.Contains(msg, "block census") {
		t.Errorf("field_absent message = %q; it must name the census as its source", msg)
	}

	r = censusRun(t, home, pageEnvelope(t, doc), "blocks", "ls", "--field", "layout[0].blokcs")
	if r.code(t) != "unknown_field" {
		t.Errorf("a key the census never saw = %q, want unknown_field", r.code(t))
	}

	// Without a census the same absent key is not known.
	bare := t.TempDir()
	seedDiscovery(t, bare, testBaseURL)
	r = censusRun(t, bare, pageEnvelope(t, doc), "blocks", "ls", "--field", "layout[0].blocks")
	if r.code(t) != "unknown_field" {
		t.Errorf("without a census = %q, want unknown_field", r.code(t))
	}
}

// In an edit-and-validate loop the anomaly is usually the edit itself (a
// typo'd key), which no rebuild clears; a census read minutes ago is therefore
// not rebuilt again, and data.census says so.
func TestBlocksValidateSkipsTheRebuildRightAfterARead(t *testing.T) {
	home := t.TempDir()
	srv := pagesServer(t, censusPageDocs())
	seedCensus(t, home, seedDiscovery(t, home, srv.URL))
	r := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), HTTP: srv.Client().Transport,
		Args:  []string{"blocks", "validate"},
		Stdin: pageEnvelope(t, `{"id":1,"layout":[{"blockType":"hero","titel":"x","title":"x","content":`+censusLexical+`}]}`),
		Clock: func() time.Time { return payloadtest.Epoch.Add(5 * time.Minute) }})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	c := r.data(t)["census"].(map[string]any)
	if srv.Count("GET", "/api/pages") != 0 || c["revalidated"] == true {
		t.Errorf("a census read 5 minutes ago was rebuilt: %v", c)
	}
	if s, _ := c["revalidate_skipped"].(string); !strings.Contains(s, "pay blocks learn pages") {
		t.Errorf("revalidate_skipped = %q", s)
	}
}
