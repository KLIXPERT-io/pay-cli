package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/lexical"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// storedPageServer serves richDoc as pages 12 and the footer global, and
// echoes a PATCH/POST body back, recording what was sent.
type storedPageServer struct {
	*payloadtest.Server
	mu   sync.Mutex
	sent []map[string]any
}

func newStoredPageServer(t *testing.T, doc map[string]any) *storedPageServer {
	t.Helper()
	s := &storedPageServer{}
	echo := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		s.mu.Lock()
		s.sent = append(s.sent, body)
		s.mu.Unlock()
		out := map[string]any{}
		for k, v := range doc {
			out[k] = v
		}
		for k, v := range body {
			out[k] = v
		}
		writeJSON(w, 200, map[string]any{"doc": out, "message": "Updated successfully."})
	}
	s.Server = payloadtest.NewServer(t,
		payloadtest.WithHandler("GET", "/api/pages/12", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, 200, doc)
		}),
		payloadtest.WithHandler("PATCH", "/api/pages/12", echo),
		payloadtest.WithHandler("GET", "/api/pages", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, 200, map[string]any{"docs": []any{doc}, "totalDocs": 1, "limit": 10, "page": 1,
				"totalPages": 1, "hasNextPage": false, "hasPrevPage": false})
		}),
		payloadtest.WithHandler("GET", "/api/globals/footer", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, 200, map[string]any{"id": 1, "globalType": "footer",
				"note": doc["hero"].(map[string]any)["richText"], "navItems": []any{}})
		}),
		payloadtest.WithHandler("POST", "/api/globals/footer", echo),
	)
	return s
}

func (s *storedPageServer) writes() int {
	return s.Count("PATCH", "/api/pages") + s.Count("POST", "/api/pages") + s.Count("POST", "/api/globals")
}

// renderedLayout is the layout of richDoc as `pay get --richtext md --output
// raw` prints it: every block rich-text field is a Markdown string.
func renderedLayout(t *testing.T) map[string]any {
	t.Helper()
	data, _, _ := renderRichText(any(richDoc(t)), richTextMD)
	return roundTripJSON(t, map[string]any{"layout": data.(map[string]any)["layout"]})
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// the comparison itself
// ---------------------------------------------------------------------------

func TestStoredShapeProblems(t *testing.T) {
	doc := roundTripJSON(t, richDoc(t))
	data, _, _ := renderRichText(any(richDoc(t)), richTextMD)
	rendered := roundTripJSON(t, data)

	strs, objs := storedShapeProblems(rendered, doc)
	want := []string{"hero.richText", "layout[0].richText", "layout[1].columns[0].richText"}
	if !reflect.DeepEqual(strs, want) || len(objs) != 0 {
		t.Fatalf("strs %v objs %v, want %v", strs, objs, want)
	}
	// The stored document itself is clean.
	if s, o := storedShapeProblems(doc, doc); len(s)+len(o) != 0 {
		t.Fatalf("stored doc flagged: %v %v", s, o)
	}
	// Rows are matched by id: reordered rows compare with their own stored row.
	lay := rendered["layout"].([]any)
	swapped := map[string]any{"layout": []any{lay[1], lay[0]}}
	if s, _ := storedShapeProblems(swapped, doc); !reflect.DeepEqual(s, []string{"layout[0].columns[0].richText", "layout[1].richText"}) {
		t.Errorf("swapped rows: %v", s)
	}
	// Without ids, a row is compared only with a stored row of the same type.
	noID := map[string]any{"layout": []any{map[string]any{"blockType": "content", "richText": "text"}}}
	if s, _ := storedShapeProblems(noID, doc); len(s) != 0 {
		t.Errorf("a row of another type was compared: %v", s)
	}
	sameType := map[string]any{"layout": []any{map[string]any{"blockType": "cta", "richText": "text"}}}
	if s, _ := storedShapeProblems(sameType, doc); !reflect.DeepEqual(s, []string{"layout[0].richText"}) {
		t.Errorf("same-type row: %v", s)
	}
	// An empty string is how Payload stores empty rich text: not flagged.
	if s, _ := storedShapeProblems(map[string]any{"hero": map[string]any{"richText": ""}}, doc); len(s) != 0 {
		t.Errorf("empty string flagged: %v", s)
	}
	// An object where the document holds an array.
	_, o := storedShapeProblems(map[string]any{"layout": map[string]any{"0": map[string]any{"richText": "x"}}}, doc)
	if !reflect.DeepEqual(o, []string{"layout"}) {
		t.Errorf("objs = %v", o)
	}
	_, o = storedShapeProblems(map[string]any{"layout": []any{map[string]any{"id": "g1", "blockType": "content",
		"columns": map[string]any{"0": map[string]any{}}}}}, doc)
	if !reflect.DeepEqual(o, []string{"layout[0].columns"}) {
		t.Errorf("nested objs = %v", o)
	}
}

// ---------------------------------------------------------------------------
// rendered block rich text, no census
// ---------------------------------------------------------------------------

// `pay get pages 12 --richtext md --output raw > page.json` then
// `pay update pages 12 --data-file page.json` stored Markdown strings in every
// block richText field when no census was cached: only blocks_unchecked
// warned. The stored document shows the editor states; the write is refused.
func TestRenderedBlockRichTextRefusedWithoutCensus(t *testing.T) {
	home := t.TempDir()
	srv := newStoredPageServer(t, richDoc(t))
	seedDiscovery(t, home, srv.URL) // a shard, no census
	body := mustMarshal(t, renderedLayout(t))
	f := writeFile(t, home, "page.json", body)

	for _, args := range [][]string{
		{"update", "pages", "12", "--data-file", f, "--draft"},
		{"update", "pages", "12", "--data-file", f, "--draft", "--dry-run"},
		{"update", "pages", "12", "--data", "@-", "--draft"},
		{"upsert", "pages", "--match", "id", "--set", "id=12", "--data", "@-", "--draft", "--dry-run"},
	} {
		r := richRun(t, srv.Server, home, body, args...)
		if r.Code != 5 || r.code(t) != "richtext_rendered" {
			t.Errorf("%v: exit %d code %s: %s", args, r.Code, r.code(t), r.Stdout)
			continue
		}
		msg := dataString(errOf(t, r), "message")
		if !strings.Contains(msg, "layout[0].richText") || !strings.Contains(msg, "layout[1].columns[0].richText") {
			t.Errorf("%v: message %q", args, msg)
		}
	}
	if n := srv.writes(); n != 0 {
		t.Errorf("%d writes were sent", n)
	}

	// The stored states themselves pass, and the real write costs one read.
	srv.Reset()
	good := mustMarshal(t, map[string]any{"layout": richDoc(t)["layout"]})
	r := richRun(t, srv.Server, home, good, "update", "pages", "12", "--data", "@-", "--draft")
	if r.Code != 0 {
		t.Fatalf("stored states: exit %d %s", r.Code, r.Stdout)
	}
	if n := srv.Count("GET", "/api/pages/12"); n != 1 {
		t.Errorf("GET /api/pages/12 = %d, want 1", n)
	}
	// A body without block rows reads nothing extra.
	srv.Reset()
	r = richRun(t, srv.Server, home, "", "update", "pages", "12", "--set", "title=Neu", "--draft")
	if r.Code != 0 {
		t.Fatalf("title: exit %d %s", r.Code, r.Stdout)
	}
	if n := srv.Count("GET", "/api/pages/12"); n != 0 {
		t.Errorf("a title-only update read the document %d time(s)", n)
	}
}

func TestRenderedRichTextRefusedOnAGlobalDryRun(t *testing.T) {
	home := t.TempDir()
	srv := newStoredPageServer(t, richDoc(t))
	seedDiscovery(t, home, srv.URL)
	r := richRun(t, srv.Server, home, "", "globals", "update", "footer", "--data", `{"note":"# Willkommen"}`, "--dry-run")
	if r.Code != 5 || r.code(t) != "richtext_rendered" || !strings.Contains(dataString(errOf(t, r), "hint"), "pay globals get footer") {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
}

// ---------------------------------------------------------------------------
// an object where the array belongs
// ---------------------------------------------------------------------------

// `pay lexical from-md x.md --as layout.0.content | pay update … --data @-`
// sent {"layout":{"0":…}} and Payload emptied the layout. The schema knows
// layout is blocks: refused before anything is read or sent.
func TestObjectOverArrayRefused(t *testing.T) {
	home := t.TempDir()
	srv := newStoredPageServer(t, richDoc(t))
	seedDiscovery(t, home, srv.URL)
	state := mustMarshal(t, mdState(t, "neu"))

	cases := []struct {
		name  string
		stdin string
		args  []string
		path  string
	}{
		{"--data index object", `{"layout":{"0":{"richText":` + state + `}}}`,
			[]string{"update", "pages", "12", "--data", "@-", "--draft"}, "layout"},
		{"--set index path", "",
			[]string{"update", "pages", "12", "--set", "layout.0.blockName=x", "--draft"}, "layout"},
		{"--set-json a row", "",
			[]string{"create", "pages", "--set-json", `layout={"blockType":"cta"}`, "--draft", "--dry-run"}, "layout"},
		{"group array", "",
			[]string{"update", "pages", "12", "--set", "hero.links.0.link.label=x", "--draft", "--dry-run"}, "hero.links"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv.Reset()
			r := richRun(t, srv.Server, home, tc.stdin, tc.args...)
			if r.Code != 5 || r.code(t) != "bad_request_body" {
				t.Fatalf("exit %d code %s: %s", r.Code, r.code(t), r.Stdout)
			}
			e := errOf(t, r)
			if msg := dataString(e, "message"); !strings.Contains(msg, "object at "+tc.path+",") {
				t.Errorf("message %q", msg)
			}
			if !strings.Contains(dataString(e, "hint"), "pay blocks set") {
				t.Errorf("hint %q", dataString(e, "hint"))
			}
			if n := srv.writes(); n != 0 {
				t.Errorf("%d writes were sent", n)
			}
		})
	}
	// A key the schema does not know: the stored document shows the array.
	doc := richDoc(t)
	doc["sections"] = []any{map[string]any{"id": "s1", "heading": "a"}}
	srv2 := newStoredPageServer(t, doc)
	home2 := t.TempDir()
	seedDiscovery(t, home2, srv2.URL)
	r := richRun(t, srv2.Server, home2, "", "update", "pages", "12", "--set", "sections.0.heading=b", "--draft")
	if r.Code != 5 || r.code(t) != "bad_request_body" || !strings.Contains(dataString(errOf(t, r), "message"), "pages 12 stores an array") {
		t.Fatalf("unknown key: exit %d %s", r.Code, r.Stdout)
	}
	if n := srv2.writes(); n != 0 {
		t.Errorf("%d writes were sent", n)
	}
	// A localized array read with --locale all is an object keyed by locale:
	// not a row list, not refused by the schema check.
	if err := checkKnownArrayShapes(nil, nil, nil, map[string]any{"layout": map[string]any{"en": []any{}}}); err != nil {
		t.Errorf("locale-keyed object refused: %v", err)
	}
}

// ---------------------------------------------------------------------------
// --set-md KEY=@- with nothing on stdin
// ---------------------------------------------------------------------------

func TestSetMDEmptyStdinAndSecondStdinReader(t *testing.T) {
	home := t.TempDir()
	srv := newStoredPageServer(t, richDoc(t))
	seedDiscovery(t, home, srv.URL)

	for _, stdin := range []string{"", "  \n\t\n"} {
		r := richRun(t, srv.Server, home, stdin, "update", "pages", "12", "--set-md", "hero.richText=@-", "--draft", "--dry-run")
		if r.Code != 5 || r.code(t) != "no_input" {
			t.Errorf("stdin %q: exit %d code %s: %s", stdin, r.Code, r.code(t), r.Stdout)
			continue
		}
		if h := dataString(errOf(t, r), "hint"); !strings.Contains(h, "hero.richText=") {
			t.Errorf("hint %q", h)
		}
	}
	// --data-file - holds stdin: a --set-md @- after it would read nothing.
	for _, df := range []string{"-", "/dev/stdin"} {
		r := richRun(t, srv.Server, home, `{"title":"x"}`, "update", "pages", "12",
			"--data-file", df, "--set-md", "hero.richText=@-", "--draft", "--dry-run")
		if r.Code != 5 || r.code(t) != "invalid_args" || !strings.Contains(dataString(errOf(t, r), "message"), "--data-file "+df) {
			t.Errorf("--data-file %s: exit %d %s", df, r.Code, r.Stdout)
		}
	}
	// --data-file - is stdin, as the help says.
	r := richRun(t, srv.Server, home, `{"title":"aus stdin"}`, "update", "pages", "12", "--data-file", "-", "--draft", "--dry-run")
	if r.Code != 0 || dryRunBody(t, r)["title"] != "aus stdin" {
		t.Fatalf("--data-file -: exit %d %s", r.Code, r.Stdout)
	}
	r = richRun(t, srv.Server, home, `{}`, "update", "pages", "12", "--data", "@-", "--data-file", "-", "--dry-run")
	if r.Code != 5 || r.code(t) != "invalid_args" {
		t.Errorf("two stdin bodies: exit %d %s", r.Code, r.Stdout)
	}
	// An empty file converts (a deliberate clear is possible) but warns.
	empty := writeFile(t, home, "empty.md", "\n")
	fmOnly := writeFile(t, home, "fm.md", "---\ntitle: T\n---\n")
	for _, f := range []string{empty, fmOnly} {
		r = richRun(t, srv.Server, home, "", "update", "pages", "12", "--set-md", "hero.richText=@"+f, "--draft", "--dry-run")
		if r.Code != 0 || !contains(warningCodes(r), warnSetMDEmpty) {
			t.Errorf("%s: exit %d warnings %v", f, r.Code, warningCodes(r))
		}
	}
	// Inline empty Markdown is the deliberate clear: no warning.
	r = richRun(t, srv.Server, home, "", "update", "pages", "12", "--set-md", "hero.richText=", "--draft", "--dry-run")
	if r.Code != 0 || contains(warningCodes(r), warnSetMDEmpty) {
		t.Errorf("inline empty: exit %d warnings %v", r.Code, warningCodes(r))
	}
	if n := srv.writes(); n != 0 {
		t.Errorf("%d writes were sent", n)
	}
	if got := lexical.PlainText(at(t, dryRunBody(t, r), "hero", "richText")); got != "" {
		t.Errorf("inline empty converted to %q", got)
	}
}

func TestReadsStdin(t *testing.T) {
	for _, c := range []struct {
		flag, value string
		want        bool
	}{
		{"--data", "@-", true}, {"--data", "@/dev/stdin", true}, {"--data", "-", false}, {"--data", "{}", false},
		{"--data", "@body.json", false}, {"--data-file", "-", true}, {"--data-file", "/dev/stdin", true},
		{"--data-file", "/dev/fd/0", true}, {"--data-file", "body.json", false},
	} {
		if got := readsStdin(c.flag, c.value); got != c.want {
			t.Errorf("readsStdin(%s %s) = %v", c.flag, c.value, got)
		}
	}
}

// ---------------------------------------------------------------------------
// LOW findings
// ---------------------------------------------------------------------------

// The bypass hint of a refused --set-md keeps --draft (and --locale): without
// it a draft edit would go live.
func TestSetMDRefusalHintKeepsDraft(t *testing.T) {
	home := t.TempDir()
	srv := guardServer(t, "Kennzahlen", nil)
	seedDiscovery(t, home, srv.URL)
	r := richRun(t, srv, home, "", "update", "pages", "12", "--set-md", "meta.summary=x", "--draft", "--dry-run")
	if r.Code != 5 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	if h := dataString(errOf(t, r), "hint"); !strings.Contains(h, "pay update pages 12 --data @- --draft") {
		t.Errorf("hint = %q", h)
	}
}

// `pay diff --set-md` samples other documents when the target holds nothing
// there, like `pay update` does, instead of previewing a write update refuses.
func TestDiffSetMDSamplesLikeUpdate(t *testing.T) {
	home := t.TempDir()
	srv := guardServer(t, "", "Andere Seite")
	seedDiscovery(t, home, srv.URL)
	r := richRun(t, srv, home, "", "diff", "pages", "12", "--set-md", "meta.summary=x")
	if r.Code != 0 || !contains(warningCodes(r), warnSetMDNotRichText) {
		t.Fatalf("exit %d warnings %v: %s", r.Code, warningCodes(r), r.Stdout)
	}
	if !strings.Contains(r.Stdout, "pages 3 holds a string there") {
		t.Errorf("the warning does not name the sampled document: %s", r.Stdout)
	}
}

func TestJSONKindOfObject(t *testing.T) {
	for _, v := range []any{map[string]any{"a": 1}, payload.Doc{"a": 1}} {
		if got := jsonKindOf(v); got != "an object" {
			t.Errorf("jsonKindOf(%T) = %q", v, got)
		}
	}
	if w := mdTargetWarning("section", "section", map[string]any{"backgroundColor": "x"}); w == nil || !strings.Contains(w.Message, "held an object") {
		t.Errorf("warning = %+v", w)
	}
}
