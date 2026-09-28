package cli

import (
	"net/http"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// guardServer answers the reads the --set-md target guard makes: the target
// document (GET /api/pages/12), the sample of other documents (GET
// /api/pages) and the footer global. meta.summary is a key the seeded pages
// schema does not know — the shape of every group sub-field on a site with
// GraphQL introspection disabled.
func guardServer(t *testing.T, current, sample any) *payloadtest.Server {
	t.Helper()
	return payloadtest.NewServer(t,
		payloadtest.WithHandler("GET", "/api/pages/12", func(w http.ResponseWriter, _ *http.Request) {
			doc := map[string]any{"id": 12, "title": "Home", "_status": "draft"}
			if current != nil {
				doc["meta"] = map[string]any{"summary": current}
			}
			writeJSON(w, 200, doc)
		}),
		payloadtest.WithHandler("GET", "/api/pages", func(w http.ResponseWriter, r *http.Request) {
			var docs []any
			q := r.URL.Query()
			asked := q.Get("where[meta.summary][exists]") == "true" ||
				strings.ReplaceAll(q.Get("where"), " ", "") == `{"meta.summary":{"exists":true}}`
			if sample != nil && asked && q.Get("select[meta][summary]") == "true" {
				docs = append(docs, map[string]any{"id": 3, "meta": map[string]any{"summary": sample}})
			}
			writeJSON(w, 200, map[string]any{"docs": docs, "totalDocs": len(docs), "limit": 10, "page": 1,
				"totalPages": 1, "hasNextPage": false, "hasPrevPage": false})
		}),
		payloadtest.WithHandler("GET", "/api/globals/footer", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, 200, map[string]any{"id": 1, "globalType": "footer", "copyright": "© 2026 Beispiel"})
		}),
	)
}

// KNOWN/CONFIRMED: `--set-md meta.description=…` on a site whose schema does
// not know the sub-field passed without a word and stored the editor state as
// a JSON string in the text column. The stored value is the evidence.
func TestSetMDUnknownFieldIsCheckedAgainstStoredValues(t *testing.T) {
	cases := []struct {
		name             string
		current, sample  any
		args             []string
		wantCode         int
		wantErr, wantMsg string
		wantWarn         string
	}{
		{name: "target holds a string", current: "Kennzahlen mit Herkunft", sample: nil,
			args:     []string{"update", "pages", "12", "--set-md", "meta.summary=Neue **Beschreibung**", "--draft", "--dry-run"},
			wantCode: 5, wantErr: "invalid_args", wantMsg: `pages 12 holds a string there ("Kennzahlen mit Herkunft")`},
		{name: "target empty, other documents hold strings", current: "", sample: "Andere Seite",
			args:     []string{"update", "pages", "12", "--set-md", "meta.summary=x", "--draft", "--dry-run"},
			wantCode: 5, wantErr: "invalid_args", wantMsg: "pages 3 holds a string"},
		{name: "create: other documents hold strings", sample: "Andere Seite",
			args:     []string{"create", "pages", "--set", "title=T", "--set-md", "meta.summary=x", "--draft", "--dry-run"},
			wantCode: 5, wantErr: "invalid_args", wantMsg: "pages 3 holds a string"},
		{name: "a number", current: 42,
			args:     []string{"update", "pages", "12", "--set-md", "meta.summary=x", "--draft", "--dry-run"},
			wantCode: 5, wantErr: "invalid_args", wantMsg: "holds a number"},
		{name: "stored rich text passes silently", current: map[string]any{"root": map[string]any{"type": "root", "children": []any{}}},
			args:     []string{"update", "pages", "12", "--set-md", "meta.summary=x", "--draft", "--dry-run"},
			wantCode: 0},
		{name: "sampled rich text passes silently", sample: map[string]any{"root": map[string]any{"type": "root", "children": []any{}}},
			args:     []string{"create", "pages", "--set", "title=T", "--set-md", "meta.summary=x", "--draft", "--dry-run"},
			wantCode: 0},
		{name: "a non-editor object warns", current: map[string]any{"a": 1},
			args:     []string{"update", "pages", "12", "--set-md", "meta.summary=x", "--draft", "--dry-run"},
			wantCode: 0, wantWarn: warnSetMDNotRichText},
		{name: "no evidence warns", current: nil, sample: nil,
			args:     []string{"update", "pages", "12", "--set-md", "meta.summary=x", "--draft", "--dry-run"},
			wantCode: 0, wantWarn: warnSetMDUnchecked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			srv := guardServer(t, tc.current, tc.sample)
			seedDiscovery(t, home, srv.URL)
			r := richRun(t, srv, home, "", tc.args...)
			if r.Code != tc.wantCode {
				t.Fatalf("exit %d, want %d: %s", r.Code, tc.wantCode, r.Stdout)
			}
			if tc.wantErr != "" {
				e := errOf(t, r)
				if r.code(t) != tc.wantErr || !strings.Contains(dataString(e, "message"), tc.wantMsg) {
					t.Fatalf("error = %v", e)
				}
				if h := dataString(e, "hint"); !strings.Contains(h, "--set meta.summary=") || !strings.Contains(h, "pay lexical from-md") {
					t.Errorf("hint = %q", h)
				}
				return
			}
			codes := warningCodes(r)
			for _, c := range []string{warnSetMDNotRichText, warnSetMDUnchecked} {
				if contains(codes, c) != (c == tc.wantWarn) {
					t.Errorf("warnings %v (want %q)", codes, tc.wantWarn)
				}
			}
		})
	}
}

func TestSetMDUnknownFieldOnAGlobal(t *testing.T) {
	home := t.TempDir()
	srv := guardServer(t, nil, nil)
	seedDiscovery(t, home, srv.URL)
	r := richRun(t, srv, home, "", "globals", "update", "footer", "--set-md", "copyright=**neu**", "--dry-run")
	if r.Code != 5 || r.code(t) != "invalid_args" {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	if h := dataString(errOf(t, r), "hint"); !strings.Contains(h, "pay globals update footer --data @-") {
		t.Errorf("hint = %q", h)
	}
}

// A known richText field costs no extra read.
func TestSetMDKnownFieldReadsNothing(t *testing.T) {
	home := t.TempDir()
	srv := guardServer(t, nil, nil)
	seedDiscovery(t, home, srv.URL)
	r := richRun(t, srv, home, "", "create", "pages", "--set", "title=T", "--set-md", "hero.richText=x", "--draft", "--dry-run")
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	if n := srv.Count("GET", "/api/pages"); n != 0 {
		t.Errorf("a known field triggered %d sample reads", n)
	}
}

func TestSetMDLiteralBackslashN(t *testing.T) {
	home := t.TempDir()
	srv := payloadtest.NewServer(t)
	seedDiscovery(t, home, srv.URL)
	r := richRun(t, srv, home, "", "update", "pages", "12", "--set-md", `hero.richText=# Titel\n\nText`, "--draft", "--dry-run")
	if r.Code != 0 || !contains(warningCodes(r), warnSetMDLiteralNewline) {
		t.Fatalf("exit %d, warnings %v", r.Code, warningCodes(r))
	}
	// Real line breaks, or a \n inside a file, are not flagged.
	r = richRun(t, srv, home, "", "update", "pages", "12", "--set-md", "hero.richText=# Titel\n\nText mit \\n", "--draft", "--dry-run")
	if contains(warningCodes(r), warnSetMDLiteralNewline) {
		t.Errorf("flagged Markdown with real line breaks: %v", warningCodes(r))
	}
}

// CONFIRMED: a Markdown file that starts with a rule and has a second rule
// later lost everything between them to front matter stripping — including
// the Markdown `pay get --richtext md` writes for such a state.
func TestSetMDLeadingRuleIsContent(t *testing.T) {
	home := t.TempDir()
	srv := payloadtest.NewServer(t)
	seedDiscovery(t, home, srv.URL)
	for _, md := range []string{"---\n\nWichtiger Absatz\n\n---\n\nEnde\n", "***\n\nWichtiger Absatz\n\n***\n\nEnde\n"} {
		file := writeFile(t, home, "hr.md", md)
		r := richRun(t, srv, home, "", "update", "pages", "12", "--set-md", "hero.richText=@"+file, "--draft", "--dry-run")
		if r.Code != 0 {
			t.Fatalf("exit %d: %s", r.Code, r.Stdout)
		}
		if contains(warningCodes(r), warnFrontmatterStripped) {
			t.Errorf("%q: a rule was taken for front matter", md)
		}
		state := at(t, dryRunBody(t, r), "hero", "richText")
		kids, _ := at(t, state, "root", "children").([]any)
		if len(kids) != 4 {
			t.Errorf("%q: %d blocks, want 4 (rule, paragraph, rule, paragraph)", md, len(kids))
		}
	}
	// Real front matter is still stripped, and the warning shows what went.
	file := writeFile(t, home, "fm.md", "---\ntitle: Seite\ntags: [a]\n---\n\nText\n")
	r := richRun(t, srv, home, "", "update", "pages", "12", "--set-md", "hero.richText=@"+file, "--draft", "--dry-run")
	var msg string
	for _, w := range r.Env["warnings"].([]any) {
		if m := w.(map[string]any); m["code"] == warnFrontmatterStripped {
			msg = dataString(m, "message")
		}
	}
	if !strings.Contains(msg, `2 line(s)`) || !strings.Contains(msg, `"title: Seite"`) {
		t.Errorf("frontmatter_stripped message = %q", msg)
	}
}

// CONFIRMED: an object Payload stored as a JSON string was reported as
// input_silently_dropped ("Payload discards unknown fields"), pointing the
// agent at the wrong cause.
func TestEchoDiffReportsStoredAsString(t *testing.T) {
	state := map[string]any{"root": map[string]any{"type": "root", "children": []any{
		map[string]any{"type": "paragraph", "children": []any{}},
	}}}
	sent := map[string]any{"meta": map[string]any{"description": state}, "title": "T"}
	echo := payload.Doc{"id": 1, "title": "T",
		"meta": map[string]any{"description": `{"root":{"type":"root","children":[{"type":"paragraph","children":[]}]}}`}}
	ws := echoDiff("pages", sent, echo, echoConfig{})
	var got *output.Warning
	for i := range ws {
		switch ws[i].Code {
		case warnStoredAsString:
			got = &ws[i]
		case output.WarnInputSilentlyDropped:
			t.Errorf("stringified value reported as dropped: %+v", ws[i])
		}
	}
	if got == nil || len(got.Paths) != 1 || got.Paths[0] != "meta.description" {
		t.Fatalf("warnings = %+v", ws)
	}
	// A populated relationship sent back as an object echoes as its string
	// id: that is not JSON text.
	var paths []string
	stringifiedPaths("", map[string]any{"rel": map[string]any{"id": "abc"}}, map[string]any{"rel": "abc"}, &paths)
	if len(paths) != 0 {
		t.Errorf("string id taken for JSON text: %v", paths)
	}
}
