package cli

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
	"github.com/KLIXPERT-io/pay-cli/internal/rows"
)

// outlineDoc is nestedDoc plus the rows `pay outline` must flag: a `{}`
// orphan and a row that lost its blockType inside a group's blocks, rich text
// as the only content of a row, an upload-only row, and plain array rows
// (buttons, breadcrumbs).
func outlineDoc(t *testing.T) map[string]any {
	t.Helper()
	doc := decodeDocT(t, nestedDoc)
	layout := doc["layout"].([]any)
	g1 := layout[1].(map[string]any)
	g1["blocks"] = append(g1["blocks"].([]any),
		map[string]any{},
		map[string]any{"id": "u1", "phase": "Lost"},
	)
	layout[0].(map[string]any)["title"] = "  Die   Überschrift  "
	x0 := layout[2].(map[string]any)["blocks"].([]any)[0].(map[string]any)
	x0["content"] = mdState(t, "## Kennzahlen\n\n"+strings.Repeat("sehr lang ", 20))
	layout = append(layout, map[string]any{"id": "m1", "blockType": "mediaBlock", "media": json.Number("5")})
	doc["layout"] = layout
	doc["breadcrumbs"] = []any{map[string]any{"id": "bc", "label": "Prozess", "url": "/prozess"}}
	return doc
}

func outlineRows(t *testing.T, r cliResult) []map[string]any {
	t.Helper()
	list, _ := r.data(t)["rows"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, x := range list {
		out = append(out, x.(map[string]any))
	}
	return out
}

func TestBuildOutline(t *testing.T) {
	list, counts := buildOutline(outlineDoc(t), nil)
	var paths []string
	byPath := map[string]outlineRow{}
	for _, r := range list {
		paths = append(paths, r.Path)
		byPath[r.Path] = r
	}
	want := []string{
		"layout[0]",
		"layout[1]", "layout[1].blocks[0]", "layout[1].blocks[1]", "layout[1].blocks[2]", "layout[1].blocks[3]", "layout[1].blocks[4]",
		"layout[2]", "layout[2].blocks[0]", "layout[2].blocks[1]", "layout[2].blocks[1].blocks[0]",
		"layout[2].blocks[1].blocks[0].buttons[0]", "layout[2].blocks[1].blocks[1]",
		"layout[3]", "layout[4]",
		"breadcrumbs[0]", // blocks arrays first, then plain arrays
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths =\n%v\nwant\n%v", paths, want)
	}

	check := func(path, field, selector, blockType, summary string, children int, flags ...string) {
		t.Helper()
		r := byPath[path]
		if flags == nil {
			flags = []string{}
		}
		if r.Field != field || r.Selector != selector || r.BlockType != blockType || r.Summary != summary ||
			r.Children != children || !reflect.DeepEqual(r.Flags, flags) {
			t.Errorf("%s = %+v\nwant field=%s selector=%s type=%s summary=%q children=%d flags=%v",
				path, r, field, selector, blockType, summary, children, flags)
		}
	}
	check("layout[0]", "layout", "id:h0", "hero", "Die Überschrift", 0)
	check("layout[1]", "layout", "id:g1", "group", "", 5)
	check("layout[1].blocks[3]", "layout[id:g1].blocks", "3", "", "", 0, outlineFlagEmptyRow)
	check("layout[1].blocks[4]", "layout[id:g1].blocks", "id:u1", "", "Lost", 0, outlineFlagNoBlockType)
	check("layout[2].blocks[1].blocks[0].buttons[0]", "layout[id:g2].blocks[id:r1].blocks[id:c0].buttons", "id:b0", "", "Go", 0)
	check("layout[4]", "layout", "id:m1", "mediaBlock", "[media 5]", 0)
	check("breadcrumbs[0]", "breadcrumbs", "id:bc", "", "Prozess", 0)

	// Rich text: paragraphs joined with " · ", cut at 80 characters.
	s := byPath["layout[2].blocks[0]"].Summary
	if !strings.HasPrefix(s, "Kennzahlen · sehr lang sehr lang") || len([]rune(s)) != outlineSummaryMax || !strings.HasSuffix(s, "…") {
		t.Errorf("rich-text summary = %q (%d)", s, len([]rune(s)))
	}

	if counts.Rows != len(want) || counts.Blocks != 12 || counts.ByType["text"] != 3 || counts.ByType["processStep"] != 3 ||
		counts.Flagged[outlineFlagEmptyRow] != 1 || counts.Flagged[outlineFlagNoBlockType] != 1 {
		t.Errorf("counts = %+v", counts)
	}
}

// unknown_type and label need knowledge; without it nothing is flagged.
func TestBuildOutlineWithKnowledge(t *testing.T) {
	doc := outlineDoc(t)
	cb := discovery.NewCensusBuilder(discovery.EntityKindCollection, "pages")
	cb.AddDoc(2, decodeDocT(t, nestedDoc))
	census := discovery.NewCensus("scope")
	census.Put(cb.Finish())
	k := discovery.NewBlockKnowledge(discovery.EntityKindCollection, "pages", census, nil)

	list, counts := buildOutline(doc, k)
	for _, r := range list {
		want := r.BlockType == "mediaBlock" // never observed by this census
		if got := contains(r.Flags, outlineFlagUnknownType); got != want {
			t.Errorf("%s (%s) unknown_type = %v", r.Path, r.BlockType, got)
		}
	}
	if counts.Flagged[outlineFlagUnknownType] != 1 {
		t.Errorf("flagged = %v", counts.Flagged)
	}
	// An array whose rows are all untyped is a blocks array when the census
	// knows its pattern.
	if !knownBlocksArray(k, "layout[1].blocks") || knownBlocksArray(k, "layout[1].buttons") || knownBlocksArray(nil, "layout") {
		t.Error("knownBlocksArray")
	}
}

// Every row's field + selector addresses exactly that row in `pay blocks`.
func TestOutlineFieldsAreUsableWithPayBlocks(t *testing.T) {
	home := t.TempDir()
	env := []string{"PAY_HOME=" + home}
	doc := outlineDoc(t)
	in := pageEnvelope(t, mustJSON(t, doc))
	r := cliRun(t, invocation{Home: home, Env: env, Stdin: in, Args: []string{"outline"}})
	if r.Code != 0 || r.Env["data_kind"] != "op_result" || r.Env["command"] != "outline" {
		t.Fatalf("outline: exit %d %s", r.Code, r.Stdout)
	}
	if tgt, _ := r.Env["target"].(map[string]any); tgt["slug"] != "pages" {
		t.Errorf("target = %v", r.Env["target"])
	}
	if !contains(warningCodes(r), "upstream_thing") {
		t.Errorf("upstream warnings were not carried: %v", warningCodes(r))
	}
	for _, row := range outlineRows(t, r) {
		field, sel, path := dataString(row, "field"), dataString(row, "selector"), dataString(row, "path")
		ls := cliRun(t, invocation{Home: home, Env: env, Stdin: in, Args: []string{"blocks", "ls", "--field", field}})
		if ls.Code != 0 {
			t.Errorf("%s: blocks ls --field %s: exit %d %s", path, field, ls.Code, ls.Stdout)
			continue
		}
		found := false
		for _, x := range ls.data(t)["rows"].([]any) {
			if dataString(x.(map[string]any), "selector") == sel {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: selector %s is not listed at --field %s", path, sel, field)
		}
	}
	// And the pair edits that row: set a field on the nested button.
	set := cliRun(t, invocation{Home: home, Env: env, Stdin: in, Args: []string{"blocks", "set", "id:b0",
		"--set", "label=Los", "--field", "layout[id:g2].blocks[id:r1].blocks[id:c0].buttons"}})
	if set.Code != 0 || at(t, docOf(t, set), "layout", 2, "blocks", 1, "blocks", 0, "buttons", 0, "label") != "Los" {
		t.Fatalf("set via outline's field: exit %d %s", set.Code, set.Stdout)
	}
}

func TestOutlineTable(t *testing.T) {
	home := t.TempDir()
	r := cliRun(t, invocation{Home: home, Env: []string{"PAY_HOME=" + home}, Stdin: mustJSON(t, outlineDoc(t)),
		Args: []string{"outline", "--output", "table"}})
	if r.Code != 0 {
		t.Fatalf("exit %d %s", r.Code, r.Stdout)
	}
	lines := strings.Split(strings.TrimRight(r.Stdout, "\n"), "\n")
	if !strings.HasPrefix(lines[0], "OUTLINE") || !strings.Contains(lines[0], "SUMMARY") || !strings.Contains(lines[0], "FLAGS") {
		t.Fatalf("header = %q", lines[0])
	}
	for _, want := range []string{
		"layout[0] hero",
		"· blocks[3] ",
		"· · blocks[0] text",
		"· · · buttons[0]",
		"breadcrumbs[0]",
	} {
		if !strings.Contains(r.Stdout, want) {
			t.Errorf("table lacks %q:\n%s", want, r.Stdout)
		}
	}
	if !strings.Contains(r.Stdout, "empty_row") || !strings.Contains(r.Stdout, "no_block_type") {
		t.Errorf("flags missing:\n%s", r.Stdout)
	}
	if got := outlineTable(nil); got[0]["outline"] != "(no rows)" {
		t.Errorf("empty table = %v", got)
	}
}

func TestOutlineRemote(t *testing.T) {
	home := t.TempDir()
	doc := outlineDoc(t)
	var queries []string
	srv := payloadtest.NewServer(t,
		payloadtest.WithHandler("GET", "/api/pages/2", func(w http.ResponseWriter, r *http.Request) {
			queries = append(queries, r.URL.RawQuery)
			writeJSON(w, 200, doc)
		}),
		payloadtest.WithHandler("GET", "/api/pages", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, map[string]any{"docs": []any{map[string]any{"id": 2, "slug": "prozess"}},
				"totalDocs": 1, "limit": 2, "page": 1, "totalPages": 1})
		}),
		payloadtest.WithHandler("GET", "/api/globals/footer", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, map[string]any{"id": 1, "navItems": []any{
				map[string]any{"id": "n1", "link": map[string]any{"label": "Impressum", "url": "/impressum"}}}})
		}),
	)
	seedDiscovery(t, home, srv.URL)

	r := richRun(t, srv, home, "", "outline", "pages", "2", "--draft")
	if r.Code != 0 || len(outlineRows(t, r)) != 16 {
		t.Fatalf("outline pages 2: exit %d %s", r.Code, r.Stdout)
	}
	if len(queries) != 1 || !strings.Contains(queries[0], "depth=0") || !strings.Contains(queries[0], "draft=true") {
		t.Errorf("queries = %v", queries)
	}
	if tgt, _ := r.Env["target"].(map[string]any); tgt["id"] != json.Number("2") {
		t.Errorf("target = %v", r.Env["target"])
	}
	if n := metaOf(t, r)["http_requests"]; n != json.Number("1") {
		t.Errorf("http_requests = %v", n)
	}

	r = richRun(t, srv, home, "", "outline", "pages", "--slug", "prozess")
	if r.Code != 0 || len(outlineRows(t, r)) != 16 {
		t.Fatalf("--slug: exit %d %s", r.Code, r.Stdout)
	}

	r = richRun(t, srv, home, "", "outline", "--global", "footer")
	rowsOut := outlineRows(t, r)
	if r.Code != 0 || len(rowsOut) != 1 || rowsOut[0]["summary"] != "Impressum" || rowsOut[0]["field"] != "navItems" {
		t.Fatalf("--global: exit %d %s", r.Code, r.Stdout)
	}

	for _, tc := range []struct {
		name string
		args []string
		code string
	}{
		{"collection without id", []string{"outline", "pages"}, "invalid_args"},
		{"id and slug", []string{"outline", "pages", "2", "--slug", "x"}, "invalid_args"},
		{"global and args", []string{"outline", "pages", "--global", "footer"}, "invalid_args"},
		{"global and slug", []string{"outline", "--global", "footer", "--slug", "x"}, "invalid_args"},
		{"piped with --draft", []string{"outline", "--draft"}, "invalid_args"},
		{"nothing at all", []string{"outline"}, "no_input"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := richRun(t, srv, home, "", tc.args...)
			if r.Code != 5 || r.code(t) != tc.code {
				t.Fatalf("exit %d code %s, want 5 %s: %s", r.Code, r.code(t), tc.code, r.Stdout)
			}
		})
	}
	// A bare row array is not a document.
	if r := richRun(t, srv, home, `[{"blockType":"cta"}]`, "outline"); r.code(t) != "bad_request_body" {
		t.Errorf("array input: %s", r.Stdout)
	}
}

func TestDocTreeOrdersBlocksFirst(t *testing.T) {
	doc := map[string]any{
		"a":      []any{map[string]any{"id": "1"}},
		"layout": []any{map[string]any{}, map[string]any{"blockType": "cta"}},
		"meta":   map[string]any{"links": []any{map[string]any{"label": "x"}}},
		"tags":   []any{"x", "y"},
		"rt":     mdStateFor("x"),
	}
	var paths []string
	for _, n := range rows.DocTree(doc) {
		paths = append(paths, n.Path)
	}
	if want := []string{"layout[0]", "layout[1]", "a[0]", "meta.links[0]"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	if !rows.HasBlockRow([]any{map[string]any{}, map[string]any{"blockType": "x"}}) || rows.HasBlockRow([]any{map[string]any{}}) {
		t.Error("HasBlockRow")
	}
}

func mdStateFor(text string) map[string]any {
	return map[string]any{"root": map[string]any{"type": "root", "children": []any{
		map[string]any{"type": "paragraph", "children": []any{map[string]any{"type": "text", "text": text}}}}}}
}

func TestRowSummary(t *testing.T) {
	for _, tc := range []struct {
		row  map[string]any
		want string
	}{
		{map[string]any{}, ""},
		{map[string]any{"heading": " ", "title": "T"}, "T"},
		{map[string]any{"title": mdStateFor("Rich title")}, "Rich title"},
		{map[string]any{"link": map[string]any{"label": "Mehr"}}, "Mehr"},
		{map[string]any{"richText": mdStateFor("Body")}, "Body"},
		{map[string]any{"zzz": mdStateFor("Any rich text")}, "Any rich text"},
		{map[string]any{"content": "## Rendered already"}, "## Rendered already"},
		{map[string]any{"content": map[string]any{"root": map[string]any{"type": "root", "children": []any{
			map[string]any{"type": "upload", "relationTo": "media", "value": 7}}}}}, "[upload media:7]"},
		{map[string]any{"image": map[string]any{"id": 9, "url": "/x.png"}}, "[image 9]"},
		{map[string]any{"url": "/kontakt"}, "/kontakt"},
		{map[string]any{"blockName": "Admin label only"}, ""},
	} {
		if got := rowSummary(tc.row); got != tc.want {
			t.Errorf("rowSummary(%v) = %q, want %q", tc.row, got, tc.want)
		}
	}
	if relativeRowPath("layout[3].blocks[1].rows[0]") != "rows[0]" || relativeRowPath("hero.links[0]") != "hero.links[0]" ||
		relativeRowPath("layout[1].settings.items[2]") != "settings.items[2]" {
		t.Error("relativeRowPath")
	}
}
