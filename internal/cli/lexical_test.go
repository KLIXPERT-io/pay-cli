package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lexNoNetwork fails the test on any HTTP request: the `pay lexical` family must
// never reach a server, even with a profile configured.
type lexNoNetwork struct{ t *testing.T }

func (n lexNoNetwork) RoundTrip(r *http.Request) (*http.Response, error) {
	n.t.Errorf("pay lexical made an HTTP request: %s %s", r.Method, r.URL)
	return nil, http.ErrHandlerTimeout
}

func lexRun(t *testing.T, stdin string, args ...string) cliResult {
	t.Helper()
	return cliRun(t, invocation{
		Args:  append([]string{"lexical"}, args...),
		Stdin: stdin,
		HTTP:  lexNoNetwork{t},
		// A configured profile must not tempt the command into discovery.
		Env: []string{"PAY_BASE_URL=http://127.0.0.1:9", "PAY_API_KEY=paycli-dev-key-lexical"},
	})
}

func TestLexicalFromMarkdownStdin(t *testing.T) {
	r := lexRun(t, "## Hello\n\nA **bold** [page](pay:pages/47){target=_blank}.\n", "from-md")
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	if r.Env["data_kind"] != "op_result" || r.Env["command"] != "lexical from-md" {
		t.Errorf("envelope = %v / %v", r.Env["data_kind"], r.Env["command"])
	}
	root, ok := r.data(t)["root"].(map[string]any)
	if !ok || root["type"] != "root" {
		t.Fatalf("data is not an editor state: %v", r.data(t))
	}
	kids, _ := root["children"].([]any)
	if len(kids) != 2 {
		t.Fatalf("children = %d", len(kids))
	}
	if meta, _ := r.Env["meta"].(map[string]any); meta["http_requests"] != json.Number("0") {
		t.Errorf("http_requests = %v", meta["http_requests"])
	}
}

func TestLexicalFromMarkdownAs(t *testing.T) {
	r := lexRun(t, "x", "from-md", "--as", "meta.body")
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	meta, _ := r.data(t)["meta"].(map[string]any)
	body, _ := meta["body"].(map[string]any)
	if _, ok := body["root"]; !ok {
		t.Fatalf("--as meta.body = %v", r.data(t))
	}
	// The envelope IS a write body: `--data @-` unwraps it to {meta:{body:…}}.
	var env map[string]any
	dec := json.NewDecoder(strings.NewReader(r.Stdout))
	dec.UseNumber()
	if err := dec.Decode(&env); err != nil {
		t.Fatal(err)
	}
	got, warn, err := unwrapEnvelopeBody("--data", env)
	if err != nil || warn == nil {
		t.Fatalf("unwrap: %v %v", err, warn)
	}
	if _, ok := got["meta"].(map[string]any); !ok || len(got) != 1 {
		t.Errorf("unwrapped body = %v", got)
	}
	for _, bad := range []string{".x", "x.", "a..b"} {
		if r := lexRun(t, "x", "from-md", "--as", bad); r.Code != 5 || r.code(t) != "invalid_args" {
			t.Errorf("--as %q: exit %d %s", bad, r.Code, r.code(t))
		}
	}
}

func TestLexicalFromMarkdownFileAndWarnings(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "brief.md")
	src := "---\ntitle: T\n---\n\n# Brief\n\n<div>raw</div>\n\n[x](../other.md)\n"
	if err := os.WriteFile(f, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	r := lexRun(t, "", "from-md", f)
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	for _, code := range []string{"frontmatter_stripped", "lexical_html_as_text", "lexical_link_url_relative"} {
		if !r.hasWarning(t, code) {
			t.Errorf("no %s warning: %v", code, r.warnings(t))
		}
	}
	// Line numbers name the FILE's lines, front matter included.
	for _, w := range r.warnings(t) {
		if w["code"] == "lexical_html_as_text" {
			paths, _ := w["paths"].([]any)
			if len(paths) != 1 || paths[0] != "line 7" {
				t.Errorf("html_as_text paths = %v, want [line 7]", paths)
			}
		}
	}
	r = lexRun(t, "", "from-md", f, "--keep-frontmatter")
	if r.hasWarning(t, "frontmatter_stripped") {
		t.Error("--keep-frontmatter still stripped")
	}
}

func TestLexicalFromMarkdownErrors(t *testing.T) {
	cases := []struct {
		name  string
		stdin string
		args  []string
		exit  int
		code  string
	}{
		{"missing file", "", []string{"from-md", "/nonexistent/x.md"}, 5, "file_missing"},
		{"empty stdin", "  \n", []string{"from-md"}, 5, "no_input"},
		{"broken placeholder", "<!-- lexical:upload {\"relationTo\": -->", []string{"from-md"}, 5, "bad_request_body"},
		{"too many args", "", []string{"from-md", "a", "b"}, 5, "invalid_args"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := lexRun(t, tc.stdin, tc.args...)
			if r.Code != tc.exit || r.code(t) != tc.code {
				t.Errorf("exit %d code %q, want %d %q\n%s", r.Code, r.code(t), tc.exit, tc.code, r.Stdout)
			}
		})
	}
}

// richPage is `pay get pages 12 --depth 0` output with rich text at two
// depths, the shape a real page has (layout[].richText, layout[].blocks[].content).
func richPage(t *testing.T) string {
	t.Helper()
	para := func(text string) map[string]any {
		return map[string]any{"root": map[string]any{"type": "root", "version": 1, "direction": "ltr", "format": "", "indent": 0,
			"children": []any{map[string]any{"type": "paragraph", "version": 1, "direction": "ltr", "format": "", "indent": 0,
				"textFormat": 0, "textStyle": "", "children": []any{map[string]any{"type": "text", "version": 1, "text": text,
					"format": 1, "detail": 0, "mode": "normal", "style": ""}}}}}}
	}
	doc := map[string]any{"id": 12, "title": "Home", "layout": []any{
		map[string]any{"id": "a1", "blockType": "text", "richText": para("First")},
		map[string]any{"id": "b2", "blockType": "group", "blocks": []any{
			map[string]any{"id": "c3", "blockType": "text", "content": para("Nested")},
		}},
	}}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return pageEnvelope(t, string(b))
}

func TestLexicalToMarkdownDocument(t *testing.T) {
	r := lexRun(t, richPage(t), "to-md")
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	d := r.data(t)
	fields, _ := d["fields"].([]any)
	if len(fields) != 2 || d["count"] != json.Number("2") {
		t.Fatalf("fields = %v", fields)
	}
	var paths []string
	for _, f := range fields {
		m, _ := f.(map[string]any)
		paths = append(paths, m["path"].(string))
	}
	if strings.Join(paths, ",") != "layout[0].richText,layout[1].blocks[0].content" {
		t.Errorf("paths = %v", paths)
	}
	md, _ := d["markdown"].(string)
	if !strings.Contains(md, "<!-- pay:field layout[0].richText -->\n\n**First**\n") || !strings.Contains(md, "**Nested**") {
		t.Errorf("markdown = %q", md)
	}
	if tgt, _ := r.Env["target"].(map[string]any); tgt["slug"] != "pages" {
		t.Errorf("target not carried: %v", r.Env["target"])
	}
	if !r.hasWarning(t, "upstream_thing") {
		t.Error("upstream warnings are not carried forward")
	}

	for _, field := range []string{"layout[1].blocks[0].content", "layout.1.blocks.0.content"} {
		r = lexRun(t, richPage(t), "to-md", "--field", field)
		if r.Code != 0 {
			t.Fatalf("--field %s: exit %d: %s", field, r.Code, r.Stdout)
		}
		if md, _ := r.data(t)["markdown"].(string); md != "**Nested**\n" {
			t.Errorf("--field %s markdown = %q", field, md)
		}
	}
}

func TestLexicalToMarkdownErrors(t *testing.T) {
	r := lexRun(t, richPage(t), "to-md", "--field", "layout[0].nope")
	if r.Code != 5 || r.code(t) != "not_rich_text" {
		t.Fatalf("exit %d code %s", r.Code, r.code(t))
	}
	e, _ := r.Env["error"].(map[string]any)
	dym, _ := e["did_you_mean"].([]any)
	if len(dym) != 2 {
		t.Errorf("did_you_mean = %v, want the two rich-text paths", dym)
	}

	r = lexRun(t, `{"id":1,"title":"no rich text"}`, "to-md")
	if r.Code != 5 || r.code(t) != "not_rich_text" {
		t.Errorf("doc without rich text: exit %d code %s", r.Code, r.code(t))
	}
	r = lexRun(t, "# not json", "to-md")
	if r.Code != 5 || r.code(t) != "bad_request_body" {
		t.Errorf("markdown fed to to-md: exit %d code %s", r.Code, r.code(t))
	}
	upstream := `{"ok":false,"v":1,"command":"get","data_kind":"error","meta":{},"error":{"code":"doc_not_found","message":"no such page"}}`
	r = lexRun(t, upstream, "to-md")
	if r.Code != 4 || r.code(t) != "doc_not_found" {
		t.Errorf("upstream error: exit %d code %s, want 4 doc_not_found", r.Code, r.code(t))
	}
}

func TestLexicalToMarkdownOut(t *testing.T) {
	state := `{"root":{"type":"root","children":[{"type":"heading","tag":"h2","children":[{"type":"text","text":"T","format":0}]}]}}`
	r := lexRun(t, state, "to-md", "-o", "-")
	if r.Code != 0 {
		t.Fatalf("exit %d: %s %s", r.Code, r.Stdout, r.Stderr)
	}
	if r.Stdout != "## T\n" {
		t.Errorf("stdout = %q, want the bare Markdown", r.Stdout)
	}
	if !strings.Contains(r.Stderr, `"command": "lexical to-md"`) {
		t.Errorf("the envelope did not move to stderr: %q", r.Stderr)
	}

	out := filepath.Join(t.TempDir(), "x.md")
	r = lexRun(t, state, "to-md", "--out", out)
	if r.Code != 0 || r.data(t)["out"] != out {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	if b, err := os.ReadFile(out); err != nil || string(b) != "## T\n" {
		t.Errorf("file = %q, %v", b, err)
	}
}

func TestLexicalToMarkdownIssueWarnings(t *testing.T) {
	state := `{"root":{"type":"root","children":[
	  {"type":"paragraph","format":"center","children":[{"type":"text","text":"c","format":0}]},
	  {"type":"block","version":2,"format":"","fields":{"blockType":"cta"}}]}}`
	r := lexRun(t, state, "to-md")
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	for _, code := range []string{"lexical_lossy_alignment", "lexical_placeholder"} {
		if !r.hasWarning(t, code) {
			t.Errorf("no %s: %v", code, r.warnings(t))
		}
	}
	for _, w := range r.warnings(t) {
		if w["code"] == "lexical_lossy_alignment" {
			if paths, _ := w["paths"].([]any); len(paths) != 1 || paths[0] != "root.children[0]" {
				t.Errorf("paths = %v", paths)
			}
		}
	}
}

func TestLexicalText(t *testing.T) {
	r := lexRun(t, richPage(t), "text")
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	if got := r.data(t)["text"]; got != "First\n\nNested" {
		t.Errorf("text = %q", got)
	}
	r = lexRun(t, richPage(t), "text", "--field", "layout[0].richText", "-o", "-")
	if r.Stdout != "First\n" {
		t.Errorf("-o - stdout = %q", r.Stdout)
	}
	// A doc_list works too: every document's rich text, addressed by index.
	list := `{"ok":true,"v":1,"command":"find","data_kind":"doc_list","meta":{},"data":[` +
		`{"id":1,"content":{"root":{"type":"root","children":[{"type":"paragraph","children":[{"type":"text","text":"one"}]}]}}},` +
		`{"id":2,"content":{"root":{"type":"root","children":[{"type":"paragraph","children":[{"type":"text","text":"two"}]}]}}}]}`
	r = lexRun(t, list, "text")
	if r.Code != 0 || r.data(t)["text"] != "one\n\ntwo" {
		t.Errorf("doc_list: exit %d data %v", r.Code, r.Env["data"])
	}
	fields, _ := r.data(t)["fields"].([]any)
	if len(fields) != 2 || fields[1].(map[string]any)["path"] != "[1].content" {
		t.Errorf("fields = %v", fields)
	}
}

// TestLexicalPipeline is the round trip an agent runs: to-md, edit, from-md.
func TestLexicalPipeline(t *testing.T) {
	r := lexRun(t, richPage(t), "to-md", "--field", "layout[0].richText", "-o", "-")
	edited := strings.Replace(r.Stdout, "First", "First *edited*", 1)
	r = lexRun(t, edited, "from-md", "--as", "richText")
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	r = lexRun(t, r.Stdout, "to-md", "-o", "-")
	if r.Stdout != "***First*** ***edited***\n" && r.Stdout != "**First *edited***\n" {
		t.Errorf("round trip = %q", r.Stdout)
	}
}

func TestLexicalHelp(t *testing.T) {
	for _, sub := range []string{"from-md", "to-md", "text"} {
		res := helpOf(t, "lexical", sub)
		if res.Code != 0 || !strings.Contains(res.Stdout, "EXAMPLES") {
			t.Errorf("pay lexical %s --help: exit %d\n%s", sub, res.Code, res.Stdout)
		}
	}
}

// `--as layout.0.content` built {"layout":{"0":{"content":…}}}: an object
// where Payload expects the blocks array, which empties the page's layout
// when piped into `pay update --data @-`. An array hop is refused like
// `--set-md layout.2.content` is, with the row-editing pipe as the hint.
func TestLexicalFromMarkdownAsRefusesArrayRows(t *testing.T) {
	for _, bad := range []string{"layout.0.content", "layout[0].content", "layout[id:6aba].columns", "hero.links.1.text"} {
		r := lexRun(t, "x", "from-md", "--as", bad)
		if r.Code != 5 || r.code(t) != "invalid_args" {
			t.Errorf("--as %q: exit %d %s", bad, r.Code, r.code(t))
			continue
		}
		e, _ := r.Env["error"].(map[string]any)
		if msg, _ := e["message"].(string); !strings.Contains(msg, "addresses a row of an array") {
			t.Errorf("--as %q: message %q", bad, msg)
		}
		if hint, _ := e["hint"].(string); !strings.Contains(hint, "pay blocks set <selector> --set-md") {
			t.Errorf("--as %q: hint %q", bad, hint)
		}
	}
	if r := lexRun(t, "x", "from-md", "--as", "hero.richText"); r.Code != 0 {
		t.Errorf("--as hero.richText: exit %d %s", r.Code, r.Stdout)
	}
}
