package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/lexical"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// mdState is the editor state `pay lexical from-md` builds for md.
func mdState(t *testing.T, md string) map[string]any {
	t.Helper()
	st, err := lexical.FromMarkdown(md)
	if err != nil {
		t.Fatal(err)
	}
	return roundTripJSON(t, st)
}

// roundTripJSON re-decodes a value the way a CLI stage would see it.
func roundTripJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return decodeDocT(t, string(b))
}

// richDoc is a page shaped like the local website template: rich text in the
// hero group and inside blocks at two depths.
func richDoc(t *testing.T) map[string]any {
	t.Helper()
	return map[string]any{
		"id": 12, "title": "Home", "slug": "home", "_status": "draft",
		"hero": map[string]any{"type": "lowImpact", "richText": mdState(t, "# Willkommen\n\nEin **Satz**.")},
		"layout": []any{
			map[string]any{"id": "c1", "blockType": "cta", "richText": mdState(t, "Jetzt [los](pay:pages/3)")},
			map[string]any{"id": "g1", "blockType": "content", "columns": []any{
				map[string]any{"id": "k1", "size": "full", "richText": mdState(t, "- eins\n- zwei")},
			}},
		},
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// richServer answers the reads --richtext is wired into.
func richServer(t *testing.T) *payloadtest.Server {
	t.Helper()
	doc := richDoc(t)
	return payloadtest.NewServer(t,
		payloadtest.WithHandler("GET", "/api/pages/12", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, 200, doc)
		}),
		payloadtest.WithHandler("GET", "/api/pages", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, 200, map[string]any{"docs": []any{doc, doc}, "totalDocs": 2, "limit": 10, "page": 1,
				"totalPages": 1, "hasNextPage": false, "hasPrevPage": false})
		}),
		payloadtest.WithHandler("GET", "/api/pages/versions/77", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, 200, map[string]any{"id": 77, "parent": 12, "latest": true, "version": doc})
		}),
		payloadtest.WithHandler("GET", "/api/globals/footer", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, 200, map[string]any{"id": 1, "globalType": "footer",
				"note": mdState(t, "Impressum *kurz*"), "navItems": []any{}})
		}),
	)
}

func richRun(t *testing.T, srv *payloadtest.Server, home, stdin string, args ...string) cliResult {
	t.Helper()
	return cliRun(t, invocation{Home: home, Args: args, Stdin: stdin, Env: seededEnv(srv.URL)})
}

func metaOf(t *testing.T, r cliResult) map[string]any {
	t.Helper()
	m, _ := r.Env["meta"].(map[string]any)
	return m
}

func warningCodes(r cliResult) []string {
	var out []string
	ws, _ := r.Env["warnings"].([]any)
	for _, w := range ws {
		if m, ok := w.(map[string]any); ok {
			out = append(out, dataString(m, "code"))
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// --set-md on the write commands
// ---------------------------------------------------------------------------

func TestSetMDCreateBuildsTheEditorState(t *testing.T) {
	home := t.TempDir()
	srv := payloadtest.NewServer(t)
	seedDiscovery(t, home, srv.URL)
	md := "---\ntitle: front matter\n---\n# Hallo\n\nText mit <u>Unterstrich</u> und ![bild](x.png).\n"
	file := writeFile(t, home, "hero.md", md)

	r := richRun(t, srv, home, "", "create", "pages", "--set", "title=Neu",
		"--data", `{"hero":{"type":"lowImpact"}}`,
		"--set-json", `hero.richText={"root":{"type":"root","children":[]}}`,
		"--set-md", "hero.richText=@"+file, "--draft", "--dry-run")
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	body := dryRunBody(t, r)
	hero, _ := body["hero"].(map[string]any)
	if hero["type"] != "lowImpact" {
		t.Errorf("--data's hero.type was lost: %v", hero)
	}
	// --set-md is applied last and wins over --set-json on the same key.
	want := mdState(t, "# Hallo\n\nText mit <u>Unterstrich</u> und ![bild](x.png).\n")
	if !reflect.DeepEqual(hero["richText"], any(want)) {
		t.Fatalf("hero.richText =\n%v\nwant\n%v", hero["richText"], want)
	}
	codes := warningCodes(r)
	for _, c := range []string{"frontmatter_stripped", "lexical_image_as_text"} {
		if !contains(codes, c) {
			t.Errorf("warnings %v lack %s", codes, c)
		}
	}
	// The finding names the key and the FILE's line (front matter counted).
	for _, w := range r.Env["warnings"].([]any) {
		m := w.(map[string]any)
		if m["code"] == "lexical_image_as_text" {
			if paths, _ := m["paths"].([]any); len(paths) != 1 || paths[0] != "hero.richText line 6" {
				t.Errorf("paths = %v", m["paths"])
			}
		}
	}
	if n := srv.Count("POST", "/api/pages"); n != 0 {
		t.Errorf("a dry run sent %d POSTs", n)
	}
}

func TestSetMDInlineAndStdin(t *testing.T) {
	home := t.TempDir()
	srv := payloadtest.NewServer(t)
	seedDiscovery(t, home, srv.URL)

	r := richRun(t, srv, home, "## Aus stdin\n", "update", "pages", "12",
		"--set-md", "hero.richText=@-", "--draft", "--dry-run")
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	if got := at(t, dryRunBody(t, r), "hero", "richText"); !reflect.DeepEqual(got, any(mdState(t, "## Aus stdin\n"))) {
		t.Fatalf("stdin value = %v", got)
	}
	// Inline Markdown; \@ is Markdown's escape for a literal @.
	r = richRun(t, srv, home, "", "update", "pages", "12", "--set-md", `hero.richText=\@team *hallo*`, "--draft", "--dry-run")
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	if got := lexical.PlainText(at(t, dryRunBody(t, r), "hero", "richText")); got != "@team hallo" {
		t.Fatalf("inline text = %q", got)
	}
}

func TestSetMDRefusals(t *testing.T) {
	home := t.TempDir()
	srv := payloadtest.NewServer(t)
	seedDiscovery(t, home, srv.URL)
	bad := writeFile(t, home, "bad.md", "<!-- lexical:block {broken -->\n")

	cases := []struct {
		name  string
		stdin string
		args  []string
		code  string
		in    string
	}{
		{"array row by index", "", []string{"update", "pages", "12", "--set-md", "layout.0.richText=x", "--dry-run"}, "invalid_args", "row of an array"},
		{"array row by selector", "", []string{"update", "pages", "12", "--set-md", "layout[id:a].richText=x", "--dry-run"}, "invalid_args", "pay blocks set"},
		{"a text field", "", []string{"update", "pages", "12", "--set-md", "title=x", "--dry-run"}, "invalid_args", "type text"},
		{"stdin taken by --data", "{}", []string{"update", "pages", "12", "--data", "@-", "--set-md", "hero.richText=@-", "--dry-run"}, "invalid_args", "--data @-"},
		{"two stdin values", "x", []string{"update", "pages", "12", "--set-md", "hero.richText=@-", "--set-md", "meta.x=@-", "--dry-run"}, "invalid_args", "only one"},
		{"missing file", "", []string{"update", "pages", "12", "--set-md", "hero.richText=@nope.md", "--dry-run"}, "file_missing", "nope.md"},
		{"broken placeholder", "", []string{"update", "pages", "12", "--set-md", "hero.richText=@" + bad, "--dry-run"}, "bad_request_body", "hero.richText"},
		{"no key", "", []string{"update", "pages", "12", "--set-md", "=x", "--dry-run"}, "invalid_args", "not key="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := richRun(t, srv, home, tc.stdin, tc.args...)
			if r.Code != 5 || r.code(t) != tc.code {
				t.Fatalf("exit %d code %s, want 5 %s: %s", r.Code, r.code(t), tc.code, r.Stdout)
			}
			e := errOf(t, r)
			if msg := dataString(e, "message") + " " + dataString(e, "hint"); !strings.Contains(msg, tc.in) {
				t.Errorf("error %q lacks %q", msg, tc.in)
			}
		})
	}
	if n := srv.Count("PATCH", "/api/pages"); n != 0 {
		t.Errorf("a refused --set-md sent %d PATCHes", n)
	}
}

// ---------------------------------------------------------------------------
// --set-md on pay blocks
// ---------------------------------------------------------------------------

func TestBlocksSetMD(t *testing.T) {
	home := t.TempDir()
	file := writeFile(t, home, "step.md", "Schritt **zwei**\n")
	env := []string{"PAY_HOME=" + home}

	// add: the key is relative to the new row; later flags win.
	r := cliRun(t, invocation{Home: home, Env: env, Stdin: nestedEnvelope(t), Args: []string{
		"blocks", "add", "text", "--set-json", `content={"root":{"type":"root","children":[]}}`,
		"--set-md", "content=@" + file, "--set-md", "section.intro=kurz", "--field", "layout[id:g2].blocks"}})
	if r.Code != 0 {
		t.Fatalf("add: exit %d: %s", r.Code, r.Stdout)
	}
	row := at(t, docOf(t, r), "layout", 2, "blocks", 2).(map[string]any)
	if !reflect.DeepEqual(row["content"], any(mdState(t, "Schritt **zwei**\n"))) {
		t.Errorf("content = %v", row["content"])
	}
	if !reflect.DeepEqual(at(t, row, "section", "intro"), any(mdState(t, "kurz"))) {
		t.Errorf("section.intro = %v", row["section"])
	}

	// set: nested row by id; a key that held a plain string warns.
	r = cliRun(t, invocation{Home: home, Env: env, Stdin: pageEnvelope(t, strings.Replace(nestedDoc,
		`{"id":"x0","blockType":"text","heading":null`, `{"id":"x0","blockType":"text","heading":"Hallo"`, 1)),
		Args: []string{"blocks", "set", "id:x0", "--set-md", "content=@" + file, "--set-md", "heading=Neu",
			"--field", "layout[type:group[1]].blocks"}})
	if r.Code != 0 {
		t.Fatalf("set: exit %d: %s", r.Code, r.Stdout)
	}
	row = at(t, docOf(t, r), "layout", 2, "blocks", 0).(map[string]any)
	if !lexical.IsEditorState(row["content"]) || !lexical.IsEditorState(row["heading"]) {
		t.Fatalf("row = %v", row)
	}
	if codes := warningCodes(r); !contains(codes, warnSetMDNotRichText) {
		t.Errorf("warnings %v lack %s", codes, warnSetMDNotRichText)
	}

	// stdin holds the document: @- is refused.
	r = cliRun(t, invocation{Home: home, Env: env, Stdin: nestedEnvelope(t),
		Args: []string{"blocks", "set", "first", "--set-md", "content=@-"}})
	if r.Code != 5 || r.code(t) != "invalid_args" || !strings.Contains(dataString(errOf(t, r), "message"), "stdin") {
		t.Fatalf("@-: exit %d %s", r.Code, r.Stdout)
	}
	// --set-md alone is something to change.
	r = cliRun(t, invocation{Home: home, Env: env, Stdin: nestedEnvelope(t),
		Args: []string{"blocks", "set", "first", "--set-md", "richText=x"}})
	if r.Code != 0 {
		t.Fatalf("set with only --set-md: exit %d %s", r.Code, r.Stdout)
	}
}

func TestApplyRowMarkdownWarnsFromTheCensus(t *testing.T) {
	stats := &discovery.CensusType{Slug: "text", Fields: map[string]*discovery.CensusField{
		"heading": {Kind: discovery.CensusKindScalar, Types: map[string]int{"string": 4, "null": 1}},
		"content": {Kind: discovery.CensusKindRichText},
		"section": {Kind: discovery.CensusKindGroup, Fields: map[string]*discovery.CensusField{
			"body": {Kind: discovery.CensusKindRichText}}},
	}}
	row := map[string]any{"heading": nil, "content": discovery.DefaultRichTextValue(), "section": map[string]any{}}
	warns, err := applyRowMarkdown(&Deps{}, row, []string{"heading=x", "content=**y**", "section.body=z"}, stats, "text")
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 1 || warns[0].Code != warnSetMDNotRichText || !strings.Contains(warns[0].Message, "string") {
		t.Fatalf("warnings = %+v", warns)
	}
	if lexical.PlainText(row["content"]) != "y" || lexical.PlainText(at(t, row, "section", "body")) != "z" {
		t.Fatalf("row = %v", row)
	}
	if censusFieldAt(stats, "section.body") == nil || censusFieldAt(stats, "items[0].x") != nil || censusFieldAt(nil, "x") != nil {
		t.Error("censusFieldAt")
	}
}

// ---------------------------------------------------------------------------
// --richtext on the reads
// ---------------------------------------------------------------------------

func TestRenderRichText(t *testing.T) {
	doc := richDoc(t)
	centered := mdState(t, "Mitte")
	centered["root"].(map[string]any)["children"].([]any)[0].(map[string]any)["format"] = "center"
	doc["footer"] = centered
	doc["upload"] = mdState(t, "![media:4]()")

	data, meta, issues := renderRichText(any(doc), richTextMD)
	got := data.(map[string]any)
	if at(t, got, "hero", "richText") != "# Willkommen\n\nEin **Satz**.\n" {
		t.Errorf("hero = %q", at(t, got, "hero", "richText"))
	}
	if at(t, got, "layout", 0, "richText") != "Jetzt [los](pay:pages/3)\n" {
		t.Errorf("cta = %q", at(t, got, "layout", 0, "richText"))
	}
	want := []string{"footer", "hero.richText", "layout[0].richText", "layout[1].columns[0].richText", "upload"}
	if meta.Format != "md" || meta.Rendered != 5 || !reflect.DeepEqual(meta.Paths, want) {
		t.Errorf("meta = %+v", meta)
	}
	// Lossy findings are reported with the field path; the informational
	// placeholder finding (the upload) is not.
	if len(issues) != 1 || issues[0].Code != lexical.IssueLossyAlignment || !strings.HasPrefix(issues[0].Path, "footer > ") {
		t.Errorf("issues = %+v", issues)
	}

	list, meta, _ := renderRichText([]any{richDoc(t), richDoc(t)}, richTextText)
	if at(t, list, 1, "hero", "richText") != "Willkommen\n\nEin Satz." || meta.Paths[3] != "[1].hero.richText" {
		t.Errorf("list = %v / %v", at(t, list, 1, "hero", "richText"), meta.Paths)
	}

	// The cap on meta.richtext.paths.
	many := make([]any, output.MaxRichTextPaths+5)
	for i := range many {
		many[i] = mdState(t, "x")
	}
	_, meta, _ = renderRichText(many, richTextText)
	if meta.Rendered != len(many) || len(meta.Paths) != output.MaxRichTextPaths || !meta.PathsTruncated {
		t.Errorf("capped meta = %d %d %v", meta.Rendered, len(meta.Paths), meta.PathsTruncated)
	}
}

func TestReadCommandsRenderRichText(t *testing.T) {
	home := t.TempDir()
	srv := richServer(t)
	seedDiscovery(t, home, srv.URL)

	r := richRun(t, srv, home, "", "get", "pages", "12", "--richtext", "md")
	if r.Code != 0 {
		t.Fatalf("get: exit %d: %s", r.Code, r.Stdout)
	}
	if at(t, r.data(t), "layout", 1, "columns", 0, "richText") != "- eins\n- zwei\n" {
		t.Errorf("nested value = %v", at(t, r.data(t), "layout", 1, "columns", 0, "richText"))
	}
	rt, _ := metaOf(t, r)["richtext"].(map[string]any)
	if rt["format"] != "md" || rt["rendered"] != json.Number("3") {
		t.Errorf("meta.richtext = %v", rt)
	}

	// json (the default) leaves the stored state and records nothing.
	r = richRun(t, srv, home, "", "get", "pages", "12")
	if _, ok := metaOf(t, r)["richtext"]; ok || !lexical.IsEditorState(at(t, r.data(t), "hero", "richText")) {
		t.Errorf("default read changed: %s", r.Stdout)
	}

	// --output raw prints the RENDERED data, not the stored body.
	raw := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL),
		Args: []string{"get", "pages", "12", "--richtext", "text", "--output", "raw"}})
	if raw.Code != 0 || !strings.Contains(raw.Stdout, `"richText": "Willkommen\n\nEin Satz."`) {
		t.Errorf("raw: exit %d\n%s", raw.Code, raw.Stdout)
	}

	// find, jsonl: bare documents on stdout, meta on the stderr summary.
	jl := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL),
		Args: []string{"find", "pages", "--richtext", "text", "--output", "jsonl"}})
	lines := strings.Split(strings.TrimSpace(jl.Stdout), "\n")
	if jl.Code != 0 || len(lines) != 2 || !strings.Contains(lines[0], `"richText":"Jetzt los"`) {
		t.Fatalf("jsonl: exit %d\n%s", jl.Code, jl.Stdout)
	}
	if !strings.Contains(jl.Stderr, `"richtext":{"format":"text","rendered":6`) {
		t.Errorf("jsonl summary lacks meta.richtext: %s", jl.Stderr)
	}

	r = richRun(t, srv, home, "", "versions", "get", "pages", "77", "--richtext", "md")
	if r.Code != 0 || at(t, r.data(t), "version", "hero", "richText") != "# Willkommen\n\nEin **Satz**.\n" {
		t.Fatalf("versions get: exit %d %s", r.Code, r.Stdout)
	}
	r = richRun(t, srv, home, "", "globals", "get", "footer", "--richtext", "markdown")
	if r.Code != 0 || r.data(t)["note"] != "Impressum *kurz*\n" {
		t.Fatalf("globals get: exit %d %s", r.Code, r.Stdout)
	}

	// A bad value fails before any request.
	srv.Reset()
	r = richRun(t, srv, home, "", "get", "pages", "12", "--richtext", "html")
	if r.Code != 5 || r.code(t) != "invalid_option" || srv.Count("GET", "/api/pages") != 0 {
		t.Fatalf("--richtext html: exit %d %s", r.Code, r.Stdout)
	}
}

// ---------------------------------------------------------------------------
// rendered documents are refused by every write path
// ---------------------------------------------------------------------------

func renderedEnvelope(t *testing.T, format string) string {
	t.Helper()
	data, meta, _ := renderRichText(any(richDoc(t)), format)
	env := map[string]any{
		"ok": true, "v": 1, "command": "get", "data_kind": "doc",
		"target": map[string]any{"kind": "collection", "slug": "pages", "id": 12},
		"data":   data,
		"meta":   map[string]any{"request_id": "t", "richtext": meta},
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRenderedDocumentsAreRefused(t *testing.T) {
	home := t.TempDir()
	srv := payloadtest.NewServer(t)
	seedBlocksDiscovery(t, home, srv.URL, []string{"cta", "content"})
	md := renderedEnvelope(t, richTextMD)

	for _, args := range [][]string{
		{"apply", "--draft", "--dry-run"},
		{"update", "pages", "12", "--data", "@-", "--draft", "--dry-run"},
		{"create", "pages", "--data", "@-", "--draft", "--dry-run"},
		{"blocks", "set", "first", "--set", "blockName=x"},
		{"blocks", "mv", "first", "--last"},
		{"lexical", "to-md"},
	} {
		r := richRun(t, srv, home, md, args...)
		if r.Code != 5 || r.code(t) != "richtext_rendered" {
			t.Errorf("%v: exit %d code %s: %s", args, r.Code, r.code(t), r.Stdout)
		}
	}
	// A file holding the envelope is the same thing.
	f := writeFile(t, home, "page.json", md)
	if r := richRun(t, srv, home, "", "update", "pages", "12", "--data-file", f, "--dry-run"); r.code(t) != "richtext_rendered" {
		t.Errorf("--data-file: %s", r.Stdout)
	}
	// Reading verbs still accept it.
	for _, args := range [][]string{{"blocks", "ls"}, {"outline"}} {
		if r := richRun(t, srv, home, md, args...); r.Code != 0 {
			t.Errorf("%v refused a rendered document: %s", args, r.Stdout)
		}
	}
	// --richtext json is the stored form and is not refused.
	env := strings.Replace(renderedEnvelope(t, richTextMD), `"format":"md"`, `"format":"json"`, 1)
	if r := richRun(t, srv, home, env, "blocks", "ls"); r.Code != 0 {
		t.Errorf("json-format envelope refused: %s", r.Stdout)
	}
	if n := srv.Count("PATCH", "/api/pages") + srv.Count("POST", "/api/pages"); n != 0 {
		t.Errorf("%d writes were sent", n)
	}
}

// A bare document (jsonl output, a jq extract) has no envelope to say it was
// rendered: a declared richText field holding a string is refused, and a
// census-known one inside a block row warns.
func TestRenderedBareDocuments(t *testing.T) {
	home := t.TempDir()
	srv := payloadtest.NewServer(t)
	seedDiscovery(t, home, srv.URL)
	data, _, _ := renderRichText(any(richDoc(t)), richTextMD)
	b, _ := json.Marshal(data)

	r := richRun(t, srv, home, string(b), "update", "pages", "12", "--data", "@-", "--draft", "--dry-run")
	if r.Code != 5 || r.code(t) != "richtext_rendered" || !strings.Contains(dataString(errOf(t, r), "message"), "hero.richText") {
		t.Fatalf("bare: exit %d %s", r.Code, r.Stdout)
	}
	// An empty string (a cleared field) and a proper state are fine.
	r = richRun(t, srv, home, "", "update", "pages", "12", "--data", `{"hero":{"richText":null}}`, "--draft", "--dry-run")
	if r.Code != 0 {
		t.Fatalf("null: exit %d %s", r.Code, r.Stdout)
	}

	// Block rows: the census knows cta.richText and columns[].richText.
	doc := richDoc(t)
	cb := discovery.NewCensusBuilder(discovery.EntityKindCollection, "pages")
	cb.AddDoc(12, roundTripJSON(t, doc))
	census := discovery.NewCensus("scope")
	census.Put(cb.Finish())
	k := discovery.NewBlockKnowledge(discovery.EntityKindCollection, "pages", census, nil)
	body := roundTripJSON(t, map[string]any{"layout": data.(map[string]any)["layout"]})
	w := rowRichTextStrings(body, k)
	if w == nil || !reflect.DeepEqual(w.Paths, []string{"layout[0].richText", "layout[1].columns[0].richText"}) {
		t.Fatalf("warning = %+v", w)
	}
	if rowRichTextStrings(roundTripJSON(t, doc), k) != nil {
		t.Error("stored states must not warn")
	}
	if rowRichTextStrings(body, nil) != nil {
		t.Error("no knowledge, no warning")
	}
}

func TestEnvelopeRichText(t *testing.T) {
	for _, tc := range []struct {
		meta any
		want string
	}{
		{nil, ""},
		{map[string]any{}, ""},
		{map[string]any{"richtext": map[string]any{"format": "json"}}, ""},
		{map[string]any{"richtext": map[string]any{"format": "md"}}, "md"},
		{map[string]any{"richtext": map[string]any{"format": "text"}}, "text"},
	} {
		if got := envelopeRichText(map[string]any{"meta": tc.meta}); got != tc.want {
			t.Errorf("envelopeRichText(%v) = %q", tc.meta, got)
		}
	}
	f := richTextFlag{format: " Markdown "}
	if f.validate() != nil || f.normalized() != richTextMD || !f.active() {
		t.Error("markdown alias")
	}
	if (&richTextFlag{format: "plain"}).normalized() != richTextText || (&richTextFlag{}).active() {
		t.Error("aliases")
	}
}

// ---------------------------------------------------------------------------
// pay lexical --field: the nested selector grammar
// ---------------------------------------------------------------------------

func TestLexicalFieldNestedGrammar(t *testing.T) {
	doc := richDoc(t)
	doc["layout"] = append(doc["layout"].([]any), map[string]any{"id": "c2", "blockType": "cta", "richText": mdState(t, "Zweite")})
	env := pageEnvelope(t, mustJSON(t, doc))
	for _, field := range []string{
		"layout[1].columns[0].richText", "layout.1.columns.0.richText",
		"layout[id:g1].columns[id:k1].richText", "layout[type:content].columns[0].richText",
	} {
		r := lexRun(t, env, "text", "--field", field)
		if r.Code != 0 || r.data(t)["text"] != "eins\nzwei" {
			t.Errorf("--field %s: exit %d %s", field, r.Code, r.Stdout)
			continue
		}
		if p := at(t, r.data(t), "fields", 0, "path"); p != "layout[1].columns[0].richText" {
			t.Errorf("--field %s: path %v", field, p)
		}
	}
	if r := lexRun(t, env, "to-md", "--field", "layout[type:cta[1]].richText"); r.Code != 0 || r.data(t)["markdown"] != "Zweite\n" {
		t.Errorf("type:cta[1]: %s", r.Stdout)
	}
	for _, tc := range []struct {
		field, code string
		exit        int
	}{
		{"layout[type:cta].richText", "selector_ambiguous", 5},
		{"layout[id:nope].richText", "selector_no_match", 4},
		{"layout[id:g1].columns", "not_rich_text", 5},
		{"layout[0].nope", "not_rich_text", 5},
		{"layout[0", "invalid_args", 5},
	} {
		r := lexRun(t, env, "to-md", "--field", tc.field)
		if r.Code != tc.exit || r.code(t) != tc.code {
			t.Errorf("--field %s: exit %d code %s, want %d %s", tc.field, r.Code, r.code(t), tc.exit, tc.code)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
