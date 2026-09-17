package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/google/go-cmp/cmp"
)

func newWriter(f Format) (*Writer, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return &Writer{Stdout: &out, Stderr: &errb, Format: f}, &out, &errb
}

func docList() *Envelope {
	return New("find", KindDocList, []any{
		map[string]any{"id": float64(11), "title": "Home", "slug": "home", "_status": "published"},
		map[string]any{"id": float64(12), "title": "About, Inc.", "slug": "about", "_status": "draft"},
	}).WithMeta(Meta{RequestID: "r1", Profile: "dev", BaseURL: "http://localhost:3900"})
}

func TestRenderJSON(t *testing.T) {
	w, out, errb := newWriter(FormatJSON)
	exit, err := w.Render(docList())
	if err != nil {
		t.Fatal(err)
	}
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
	if errb.Len() != 0 {
		t.Errorf("a successful json render must not write to stderr: %q", errb)
	}
	var env map[string]any
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, out)
	}
	if env["ok"] != true {
		t.Error("ok must be true")
	}
	if !strings.HasSuffix(out.String(), "}\n") {
		t.Errorf("output must end with a single newline: %q", out.String()[out.Len()-5:])
	}
}

// TestRenderJSONL is §10.3's split: bare documents on stdout, envelope on
// stderr, so `> out.jsonl 2> summary.json` produces two clean files.
func TestRenderJSONL(t *testing.T) {
	w, out, errb := newWriter(FormatJSONL)
	if _, err := w.Render(docList()); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout has %d lines, want 2:\n%s", len(lines), out)
	}
	for i, line := range lines {
		var doc map[string]any
		if err := json.Unmarshal([]byte(line), &doc); err != nil {
			t.Fatalf("line %d is not a bare document: %v", i, err)
		}
		if _, hasEnvelope := doc["ok"]; hasEnvelope {
			t.Errorf("line %d carries an envelope; jsonl documents are bare", i)
		}
		if doc["id"] == nil {
			t.Errorf("line %d has no id", i)
		}
	}
	summaryLines := strings.Split(strings.TrimRight(errb.String(), "\n"), "\n")
	if len(summaryLines) != 1 {
		t.Fatalf("stderr must be exactly one summary line, got %d", len(summaryLines))
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(summaryLines[0]), &summary); err != nil {
		t.Fatalf("summary is not JSON: %v", err)
	}
	if summary["ok"] != true {
		t.Error("the summary carries the envelope")
	}
	if _, hasData := summary["data"]; hasData {
		t.Error("the summary must not repeat data")
	}
}

func TestRenderID(t *testing.T) {
	tests := []struct {
		name string
		env  *Envelope
		want string
	}{
		{name: "doc list", env: docList(), want: "11\n12\n"},
		{
			name: "a write prints changed.ids",
			env:  New("create", KindDoc, map[string]any{"id": float64(16)}).WithChanged(&Changed{Created: 1, IDs: []any{16}}),
			want: "16\n",
		},
		{
			name: "string ids",
			env:  New("find", KindDocList, []any{map[string]any{"id": "66f1a2b3c4d5e6f708192a3b"}}),
			want: "66f1a2b3c4d5e6f708192a3b\n",
		},
		{
			name: "large numeric ids never use scientific notation",
			env:  New("find", KindDocList, []any{map[string]any{"id": float64(1234567890123)}}),
			want: "1234567890123\n",
		},
		{name: "empty list", env: New("find", KindDocList, []any{}), want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, out, _ := newWriter(FormatID)
			if _, err := w.Render(tc.env); err != nil {
				t.Fatal(err)
			}
			if out.String() != tc.want {
				t.Errorf("stdout = %q, want %q", out, tc.want)
			}
		})
	}
}

func TestRenderCSV(t *testing.T) {
	w, out, _ := newWriter(FormatCSV)
	if _, err := w.Render(docList()); err != nil {
		t.Fatal(err)
	}
	want := "id,_status,slug,title\r\n11,published,home,Home\r\n12,draft,about,\"About, Inc.\"\r\n"
	if out.String() != want {
		t.Errorf("csv =\n%q\nwant\n%q", out, want)
	}
}

// TestCSVDefaultColumnsSkipRichText is §10.3's "default columns = discovered
// top-level scalar fields only, never richText blobs".
func TestCSVDefaultColumnsSkipRichText(t *testing.T) {
	env := New("find", KindDocList, []any{map[string]any{
		"id":      float64(1),
		"title":   "Home",
		"content": map[string]any{"root": map[string]any{"children": []any{"a huge lexical tree"}}},
		"layout":  []any{map[string]any{"blockType": "hero"}},
	}})
	w, out, _ := newWriter(FormatCSV)
	if _, err := w.Render(env); err != nil {
		t.Fatal(err)
	}
	header := strings.Split(out.String(), "\r\n")[0]
	if header != "id,title" {
		t.Errorf("header = %q, want the scalar columns only", header)
	}
}

func TestCSVExplicitDottedColumns(t *testing.T) {
	env := New("find", KindDocList, []any{map[string]any{
		"id":   float64(1),
		"meta": map[string]any{"seo": map[string]any{"title": "Home | Site"}},
		"tags": []any{"a", "b"},
	}})
	var out, errb bytes.Buffer
	w := &Writer{Stdout: &out, Stderr: &errb, Format: FormatCSV, Columns: []string{"id", "meta.seo.title", "tags", "nope"}}
	if _, err := w.Render(env); err != nil {
		t.Fatal(err)
	}
	want := "id,meta.seo.title,tags,nope\r\n1,Home | Site,\"[\"\"a\"\",\"\"b\"\"]\",\r\n"
	if out.String() != want {
		t.Errorf("csv =\n%q\nwant\n%q", out.String(), want)
	}
}

func TestRenderTable(t *testing.T) {
	var out, errb bytes.Buffer
	w := &Writer{Stdout: &out, Stderr: &errb, Format: FormatTable, Width: 60}
	if _, err := w.Render(docList()); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("table has %d lines, want header + 2 rows:\n%s", len(lines), out.String())
	}
	if !strings.HasPrefix(lines[0], "ID") {
		t.Errorf("header = %q", lines[0])
	}
	for i, l := range lines {
		if len([]rune(l)) > 60 {
			t.Errorf("line %d is %d runes wide, want <= 60: %q", i, len([]rune(l)), l)
		}
	}
}

func TestTableTruncatesToWidth(t *testing.T) {
	env := New("find", KindDocList, []any{map[string]any{
		"id":    float64(1),
		"title": strings.Repeat("very long title ", 20),
	}})
	var out, errb bytes.Buffer
	w := &Writer{Stdout: &out, Stderr: &errb, Format: FormatTable, Width: 40}
	if _, err := w.Render(env); err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if len([]rune(l)) > 40 {
			t.Errorf("line is %d runes wide: %q", len([]rune(l)), l)
		}
	}
	if !strings.Contains(out.String(), "…") {
		t.Error("a truncated cell must be marked, or a reader mistakes it for a complete value")
	}
}

func TestTableCollapsesNewlines(t *testing.T) {
	env := New("find", KindDocList, []any{map[string]any{"id": float64(1), "title": "a\nb\tc"}})
	var out, errb bytes.Buffer
	w := &Writer{Stdout: &out, Stderr: &errb, Format: FormatTable, Width: 80}
	if _, err := w.Render(env); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out.String(), "\n"); n != 2 {
		t.Errorf("a multi-line cell broke the alignment: %d lines\n%s", n, out.String())
	}
}

func TestTableEmpty(t *testing.T) {
	w, out, _ := newWriter(FormatTable)
	if _, err := w.Render(New("find", KindDocList, []any{})); err != nil {
		t.Fatal(err)
	}
	if out.String() != "(no rows)\n" {
		t.Errorf("out = %q", out)
	}
}

// TestRenderRaw is §10.3's central fix: raw is redacted by default, and the
// notice names the escape hatch.
func TestRenderRaw(t *testing.T) {
	body := `{"id":66,"email":"a@b.c","apiKey":"` + fixtureKey + `"}`
	t.Run("redacted by default with a stderr notice", func(t *testing.T) {
		w, out, errb := newWriter(FormatRaw)
		env := New("get", KindDoc, nil).WithRawBody([]byte(body), false)
		if _, err := w.Render(env); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), fixtureKey) {
			t.Fatalf("--output raw leaked the API key: %s", out)
		}
		if !strings.Contains(errb.String(), "--no-redact") {
			t.Errorf("the notice must name --no-redact: %q", errb)
		}
	})
	t.Run("no-redact prints the true bytes with no notice", func(t *testing.T) {
		w, out, errb := newWriter(FormatRaw)
		env := New("get", KindDoc, nil).WithRawBody([]byte(body), true)
		if _, err := w.Render(env); err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(out.String()) != body {
			t.Errorf("out = %q", out)
		}
		if errb.Len() != 0 {
			t.Errorf("no redaction happened, so there is nothing to notice: %q", errb)
		}
	})
	t.Run("a clean body prints no notice", func(t *testing.T) {
		w, out, errb := newWriter(FormatRaw)
		env := New("get", KindDoc, nil).WithRawBody([]byte(`{"id":1}`), false)
		if _, err := w.Render(env); err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(out.String()) != `{"id":1}` {
			t.Errorf("out = %q", out)
		}
		if errb.Len() != 0 {
			t.Errorf("stderr = %q", errb)
		}
	})
	t.Run("no wire body falls back to data", func(t *testing.T) {
		w, out, _ := newWriter(FormatRaw)
		if _, err := w.Render(New("count", KindCount, 11)); err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(out.String()) != "11" {
			t.Errorf("out = %q", out)
		}
	})
}

// TestRenderErrorEnvelope pins §11.1: the envelope on stdout, a one-line human
// summary on stderr, and the exit code from the code.
func TestRenderErrorEnvelope(t *testing.T) {
	e := apierr.New(apierr.CodeValidationFailed, "3 field(s) are invalid on %q.", "pages")
	env := NewError("create", e).WithTarget(&Target{Kind: "collection", Slug: "pages"})

	w, out, errb := newWriter(FormatJSON)
	exit, err := w.Render(env)
	if err != nil {
		t.Fatal(err)
	}
	if exit != 5 {
		t.Errorf("exit = %d, want 5", exit)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("stdout is not the envelope: %v\n%s", err, out)
	}
	if decoded["ok"] != false || decoded["data_kind"] != "error" {
		t.Errorf("envelope = %v", decoded)
	}
	if !strings.HasPrefix(errb.String(), "pay: validation_failed (exit 5):") {
		t.Errorf("stderr summary = %q", errb)
	}
}

func TestErrorsToStderr(t *testing.T) {
	var out, errb bytes.Buffer
	w := &Writer{Stdout: &out, Stderr: &errb, Format: FormatJSON, ErrorsTo: ErrorsToStderr}
	env := NewError("find", apierr.New(apierr.CodeDocNotFound, "nope"))
	exit, err := w.Render(env)
	if err != nil {
		t.Fatal(err)
	}
	if exit != 4 {
		t.Errorf("exit = %d, want 4", exit)
	}
	if out.Len() != 0 {
		t.Errorf("stdout must be empty with --errors-to stderr: %q", out.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(errb.Bytes(), &decoded); err != nil {
		t.Fatalf("stderr is not the envelope: %v\n%s", err, errb.String())
	}
	if strings.Contains(errb.String(), "pay: doc_not_found") {
		t.Error("the human line must not be mixed into the envelope stream")
	}
}

func TestQuietSuppressesTheHumanLine(t *testing.T) {
	var out, errb bytes.Buffer
	w := &Writer{Stdout: &out, Stderr: &errb, Format: FormatJSON, Quiet: true}
	if _, err := w.Render(NewError("find", apierr.New(apierr.CodeDocNotFound, "nope"))); err != nil {
		t.Fatal(err)
	}
	if errb.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", errb.String())
	}
}

// TestUnsupportedFormatFailsLoudly is §10.3's explicit compatibility rule.
func TestUnsupportedFormatFailsLoudly(t *testing.T) {
	w, out, errb := newWriter(FormatCSV)
	env := New("explain", KindSchema, map[string]any{"sections": []any{}})
	exit, err := w.Render(env)
	if err != nil {
		t.Fatal(err)
	}
	if exit != 5 {
		t.Errorf("exit = %d, want 5", exit)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("the failure must still render as JSON: %v\n%s", err, out)
	}
	errObj, _ := decoded["error"].(map[string]any)
	if errObj["code"] != "format_unsupported" {
		t.Errorf("code = %v", errObj["code"])
	}
	if !strings.Contains(errb.String(), "format_unsupported") {
		t.Errorf("stderr = %q", errb)
	}
}

func TestErrorEnvelopeNeverRendersAsCSV(t *testing.T) {
	w, out, _ := newWriter(FormatCSV)
	env := NewError("find", apierr.New(apierr.CodeDocNotFound, "nope"))
	if _, err := w.Render(env); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("an error envelope is always JSON-shaped: %v\n%s", err, out)
	}
}

func TestErrorInJSONLStaysOnStderr(t *testing.T) {
	w, out, errb := newWriter(FormatJSONL)
	env := NewError("find", apierr.New(apierr.CodeDocNotFound, "nope"))
	exit, err := w.Render(env)
	if err != nil {
		t.Fatal(err)
	}
	if exit != 4 {
		t.Errorf("exit = %d", exit)
	}
	if out.Len() != 0 {
		t.Errorf("the jsonl data file must stay clean on failure: %q", out)
	}
	var decoded map[string]any
	if err := json.Unmarshal(errb.Bytes(), &decoded); err != nil {
		t.Fatalf("stderr is not the envelope: %v\n%s", err, errb)
	}
}

func TestRenderNilEnvelope(t *testing.T) {
	w, _, _ := newWriter(FormatJSON)
	exit, err := w.Render(nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if exit != 1 {
		t.Errorf("exit = %d, want 1", exit)
	}
}

// TestNoSecretReachesAnyStream is the package-local half of §3.1's repo-wide
// secret assertion: every format, for an envelope stuffed with credentials.
func TestNoSecretReachesAnyStream(t *testing.T) {
	body := `{"docs":[{"id":1,"apiKey":"` + fixtureKey + `","hash":"h","salt":"s"}]}`
	for _, f := range Formats {
		t.Run(string(f), func(t *testing.T) {
			env := New("find", KindDocList, []any{
				map[string]any{"id": float64(1), "apiKey": fixtureKey, "hash": "h"},
			}).
				WithMeta(Meta{BaseURL: "http://admin:" + fixtureKey + "@localhost:3900"}).
				WithRawBody([]byte(body), false)
			// Redaction of `data` is the caller's job (the transport redacts the
			// decoded body); what this asserts is that no renderer re-introduces
			// a secret from the parts output owns: meta.base_url and raw.
			redacted, _ := json.Marshal(env.Meta)
			if strings.Contains(string(redacted), fixtureKey) {
				t.Errorf("meta leaked the key: %s", redacted)
			}
			if strings.Contains(string(env.RawBody), fixtureKey) {
				t.Errorf("raw body leaked the key: %s", env.RawBody)
			}
			if !f.Supports(KindDocList) {
				return
			}
			var out, errb bytes.Buffer
			w := &Writer{Stdout: &out, Stderr: &errb, Format: f, Width: 80}
			if _, err := w.Render(env); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(errb.String(), fixtureKey) {
				t.Errorf("stderr leaked the key: %s", errb.String())
			}
		})
	}
}

func TestDocsOf(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want int
	}{
		{"nil", nil, 0},
		{"slice", []any{1, 2, 3}, 3},
		{"map slice", []map[string]any{{"id": 1}}, 1},
		{"bare object", map[string]any{"id": 1}, 1},
		{"scalar", 11, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(docsOf(tc.in)); got != tc.want {
				t.Errorf("docsOf() = %d docs, want %d", got, tc.want)
			}
		})
	}
}

func TestFitWidths(t *testing.T) {
	got := fitWidths([]int{4, 60, 8}, 40)
	if sum(got)+columnGap*2 > 40 {
		t.Errorf("widths %v do not fit 40 columns", got)
	}
	if got[0] != 4 {
		t.Errorf("a narrow column must survive shrinking: %v", got)
	}
	// Already fitting: unchanged.
	if diff := cmp.Diff([]int{4, 6, 8}, fitWidths([]int{4, 6, 8}, 100)); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}
