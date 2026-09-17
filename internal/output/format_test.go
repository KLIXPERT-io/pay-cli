package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/google/go-cmp/cmp"
)

func TestParseFormat(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Format
		wantErr bool
	}{
		{"empty defaults to json", "", FormatJSON, false},
		{"json", "json", FormatJSON, false},
		{"jsonl", "jsonl", FormatJSONL, false},
		{"id", "id", FormatID, false},
		{"raw", "raw", FormatRaw, false},
		{"csv", "csv", FormatCSV, false},
		{"table", "table", FormatTable, false},
		{"upper case", "JSON", FormatJSON, false},
		{"padded", "  csv  ", FormatCSV, false},
		{"yaml is rejected", "yaml", "", true},
		{"typo", "jsonl2", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseFormat(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				e, _ := apierr.As(err)
				if e.Code != apierr.CodeInvalidOption || e.Exit != 5 {
					t.Errorf("got %s/%d, want invalid_option/5", e.Code, e.Exit)
				}
				if !strings.Contains(e.Message, "json") {
					t.Errorf("the error must name the valid values: %q", e.Message)
				}
				return
			}
			if got != tc.want {
				t.Errorf("ParseFormat(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCheckFormat pins §10.3's "format/command compatibility is EXPLICIT":
// csv on a schema fails with format_unsupported rather than silently falling
// back to JSON.
func TestCheckFormat(t *testing.T) {
	tests := []struct {
		format  Format
		kind    DataKind
		wantErr bool
	}{
		{FormatJSON, KindDocList, false},
		{FormatJSON, KindSchema, false},
		{FormatJSON, KindCommandSpec, false},
		{FormatRaw, KindSchema, false},
		{FormatJSONL, KindDocList, false},
		{FormatJSONL, KindCount, true},
		{FormatJSONL, KindSchema, true},
		{FormatID, KindDocList, false},
		{FormatID, KindSchema, true},
		{FormatCSV, KindDocList, false},
		{FormatCSV, KindSchema, true},
		{FormatCSV, KindCommandSpec, true},
		{FormatTable, KindDocList, false},
		{FormatTable, KindSchema, true},
	}
	for _, tc := range tests {
		t.Run(string(tc.format)+"/"+string(tc.kind), func(t *testing.T) {
			err := CheckFormat(tc.format, tc.kind)
			if (err != nil) != tc.wantErr {
				t.Fatalf("CheckFormat(%q,%q) = %v, wantErr %v", tc.format, tc.kind, err, tc.wantErr)
			}
			if err == nil {
				return
			}
			e, _ := apierr.As(err)
			if e.Code != apierr.CodeFormatUnsupported || e.Exit != 5 {
				t.Errorf("got %s/%d, want format_unsupported/5", e.Code, e.Exit)
			}
			if !strings.Contains(e.Hint, "json") {
				t.Errorf("the hint must name the supported formats: %q", e.Hint)
			}
		})
	}
}

func TestEveryDataKindHasAFormat(t *testing.T) {
	for _, k := range DataKinds {
		if !FormatJSON.Supports(k) {
			t.Errorf("json must render every data_kind, but not %q", k)
		}
	}
}

// TestParsePath covers §10.3's closed three-form grammar.
func TestParsePath(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		wantErr bool
	}{
		{"identity dot", ".", false},
		{"empty", "", false},
		{"field", ".title", false},
		{"nested field", ".a.b.c", false},
		{"index", ".docs[0]", false},
		{"index at depth", ".a.b[2].c", false},
		{"iterate", ".layout[]", false},
		{"iterate then field", ".layout[].blockType", false},
		{"double index", ".a[0][1]", false},
		// A root index/iterate is legal: .data IS the array for a doc_list.
		{"root index", "[0]", false},
		{"root index, jq spelling", ".[0]", false},
		{"root iterate", ".[]", false},
		{"no leading dot", "title", true},
		{"negative index", ".a[-1]", true},
		{"non-numeric index", ".a[foo]", true},
		{"unclosed bracket", ".a[0", true},
		{"empty field name", ".a..b", true},
		{"trailing dot", ".a.", true},
		{"jq pipe", `.[] | select(._status=="published") | .title`, true},
		{"jq map", ".docs | map(.id)", true},
		{"jq select", ".docs[] | select(.id > 1)", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParsePath(tc.expr)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParsePath(%q) err = %v, wantErr %v", tc.expr, err, tc.wantErr)
			}
			if err == nil {
				return
			}
			e, _ := apierr.As(err)
			if e.Code != apierr.CodeInvalidPathExpr || e.Exit != 5 {
				t.Errorf("got %s/%d, want invalid_path_expr/5", e.Code, e.Exit)
			}
			// §10.3: the message lists the three supported forms and ends with
			// the literal not-jq sentence.
			for _, form := range []string{`".a.b"`, `".a[0]"`, `".a[]"`} {
				if !strings.Contains(e.Message, form) {
					t.Errorf("message must list %s: %q", form, e.Message)
				}
			}
			if !strings.HasSuffix(e.Message, pathNotJQ) {
				t.Errorf("message must end with the literal not-jq sentence, got %q", e.Message)
			}
		})
	}
}

func TestEvalPath(t *testing.T) {
	doc := map[string]any{
		"id":    float64(11),
		"title": "Home",
		"meta":  map[string]any{"seo": map[string]any{"title": "Home | Site"}},
		"layout": []any{
			map[string]any{"blockType": "hero", "id": "a"},
			map[string]any{"blockType": "cta", "id": "b"},
		},
		"tags": []any{"x", "y"},
	}
	tests := []struct {
		name    string
		expr    string
		data    any
		want    any
		wantErr bool
	}{
		{name: "identity", expr: ".", data: doc, want: doc},
		{name: "field", expr: ".title", data: doc, want: "Home"},
		{name: "nested", expr: ".meta.seo.title", data: doc, want: "Home | Site"},
		{name: "missing field is null", expr: ".nope", data: doc, want: nil},
		{name: "missing nested is null", expr: ".nope.deeper", data: doc, want: nil},
		{name: "index", expr: ".layout[0].blockType", data: doc, want: "hero"},
		{name: "index out of range is null", expr: ".layout[9]", data: doc, want: nil},
		{name: "iterate", expr: ".tags[]", data: doc, want: []any{"x", "y"}},
		{name: "iterate then field", expr: ".layout[].blockType", data: doc, want: []any{"hero", "cta"}},
		{name: "iterate an empty array", expr: ".empty[]", data: map[string]any{"empty": []any{}}, want: []any{}},
		// .data IS the array for a doc_list, so a root index addresses an
		// element of it.
		{name: "list index", expr: ".[1]", data: []any{"a", "b"}, want: "b"},
		{name: "bare list index", expr: "[0]", data: []any{"a", "b"}, want: "a"},
		{name: "field on an array is an error", expr: ".title", data: []any{1, 2}, wantErr: true},
		{name: "index into a string is an error", expr: ".title[0]", data: doc, wantErr: true},
		{name: "iterate an object is an error", expr: ".meta[]", data: doc, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			steps, err := ParsePath(tc.expr)
			if err != nil {
				if !tc.wantErr {
					t.Fatalf("parse: %v", err)
				}
				return
			}
			got, err := EvalPath(tc.data, steps)
			if (err != nil) != tc.wantErr {
				t.Fatalf("EvalPath err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				e, _ := apierr.As(err)
				if e.Code != apierr.CodeInvalidPathExpr {
					t.Errorf("code = %q, want invalid_path_expr", e.Code)
				}
				return
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
		})
	}
}

// TestApplyPathPreservesTheEnvelope is §10.3's guarantee: the result replaces
// data, and every other key survives so an agent can always still branch on
// .ok.
func TestApplyPathPreservesTheEnvelope(t *testing.T) {
	// .data IS the array for a doc_list, so a root iterate addresses the
	// documents themselves.
	env := fullSuccessEnvelope()
	if err := env.ApplyPath(".[].title"); err != nil {
		t.Fatalf("root iterate over a doc_list: %v", err)
	}
	if diff := cmp.Diff([]any{"Home"}, env.Data); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	if env.Page == nil || env.Target == nil {
		t.Error("the envelope did not survive --path")
	}
	// Iterating an object is still an error.
	env = fullSuccessEnvelope()
	env.Data = map[string]any{"title": "Home"}
	if err := env.ApplyPath(".[].title"); err == nil {
		t.Fatal("iterating an object must fail")
	}
	env = fullSuccessEnvelope()
	env.Data = map[string]any{"docs": []any{
		map[string]any{"title": "Home"}, map[string]any{"title": "About"},
	}}
	if err := env.ApplyPath(".docs[].title"); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]any{"Home", "About"}, env.Data); diff != "" {
		t.Errorf("data (-want +got):\n%s", diff)
	}
	if !env.OK || env.V != SchemaVersion || env.Command != "find" || env.DataKind != KindDocList {
		t.Error("ok, v, command and data_kind must survive --path")
	}
	if env.Target == nil || env.Page == nil || env.Next == nil || env.Meta.RequestID == "" {
		t.Error("target, page, next and meta must survive --path")
	}
	if env.Warnings == nil {
		t.Error("warnings must survive --path")
	}
}

func TestApplyPathEmptyIsANoop(t *testing.T) {
	env := New("get", KindDoc, map[string]any{"id": 1})
	if err := env.ApplyPath(""); err != nil {
		t.Fatal(err)
	}
	if m, ok := env.Data.(map[string]any); !ok || m["id"] != 1 {
		t.Errorf("data = %v", env.Data)
	}
}

func TestParseErrorsTo(t *testing.T) {
	tests := []struct {
		in      string
		want    ErrorsTo
		wantErr bool
	}{
		{"", ErrorsToStdout, false},
		{"stdout", ErrorsToStdout, false},
		{"stderr", ErrorsToStderr, false},
		{"both", "", true},
	}
	for _, tc := range tests {
		got, err := ParseErrorsTo(tc.in)
		if (err != nil) != tc.wantErr {
			t.Fatalf("ParseErrorsTo(%q) err = %v", tc.in, err)
		}
		if err == nil && got != tc.want {
			t.Errorf("ParseErrorsTo(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestPathReachesRootArrayElements covers doc_list, where .data is itself the
// array: both the bare "[0]" form and the jq spelling ".[]" must address the
// documents.
func TestPathReachesRootArrayElements(t *testing.T) {
	data := []any{
		map[string]any{"id": json.Number("16"), "title": "A"},
		map[string]any{"id": json.Number("10"), "title": "B"},
	}
	cases := []struct {
		expr string
		want string
	}{
		{expr: "[0].id", want: `16`},
		{expr: ".[0].id", want: `16`},
		{expr: "[].id", want: `[16,10]`},
		{expr: ".[].id", want: `[16,10]`},
		{expr: ".[1].title", want: `"B"`},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			steps, err := ParsePath(tc.expr)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got, err := EvalPath(data, steps)
			if err != nil {
				t.Fatalf("eval: %v", err)
			}
			b, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(b) != tc.want {
				t.Errorf("%s = %s, want %s", tc.expr, b, tc.want)
			}
		})
	}
	if _, err := ParsePath("a.b"); err == nil {
		t.Error("an expression that starts with a bare field name must still fail")
	}
	if _, err := ParsePath(".a..b"); err == nil {
		t.Error("an empty field name must still fail")
	}
}

// TestPathNarrowsRawOutput pins §10.3's "use --output raw for a bare scalar":
// once --path has narrowed .data, the recorded wire body no longer describes
// it and must not be what raw prints.
func TestPathNarrowsRawOutput(t *testing.T) {
	env := New("get", KindDoc, map[string]any{"id": json.Number("66"), "title": "Home"})
	env.WithRawBody([]byte(`{"id":66,"title":"Home"}`), false)
	if err := env.ApplyPath(".title"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if env.RawBody != nil {
		t.Errorf("RawBody survived --path: %s", env.RawBody)
	}
	var out, errBuf strings.Builder
	w := &Writer{Stdout: &out, Stderr: &errBuf, Format: FormatRaw}
	if _, err := w.Render(env); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.TrimSpace(out.String()) != `"Home"` {
		t.Errorf("raw output = %q, want the selected value", out.String())
	}
}

// capabilityData is the shape EVERY capability/schema command builds by hand:
// a map[string]any whose values are typed Go values, not wire-decoded generics
// (internal/cli/explain.go, collections.go, describe.go, access.go, auth.go,
// config.go all do this). Before the deep normalisation in generic(), .data was
// handed to the path evaluator untouched because the TOP level already
// satisfied `map[string]any`, so `.a[]`, `.a[0]` and any field access through a
// nested struct failed with invalid_path_expr on data the envelope had just
// rendered as an object.
type capLocalization struct {
	Enabled bool     `json:"enabled"`
	Locales []string `json:"locales"`
}

type capConnection struct {
	Localization capLocalization `json:"localization"`
}

type capRow struct {
	Slug string   `json:"slug"`
	Ops  []string `json:"ops"`
}

type capNamedMap map[string]string

func capabilityData() map[string]any {
	return map[string]any{
		"collection_slugs": []string{"pages", "posts", "media"},
		"connection":       capConnection{Localization: capLocalization{Enabled: true, Locales: []string{"en", "de"}}},
		"collections":      []capRow{{Slug: "pages", Ops: []string{"read"}}, {Slug: "posts", Ops: []string{"read", "create"}}},
		"named":            capNamedMap{"key": "value"},
		"count":            3,
		"empty_slugs":      []string{},
	}
}

// TestPathFormsOverMapShapedData walks every --path form §10.3 defines against
// the map-shaped, typed .data of a capability command. Each case failed with
// invalid_path_expr before generic() normalised depth-first.
func TestPathFormsOverMapShapedData(t *testing.T) {
	tests := []struct {
		name string
		expr string
		want any
	}{
		{"field returning a typed slice", ".collection_slugs", []any{"pages", "posts", "media"}},
		{"iterate a typed slice", ".collection_slugs[]", []any{"pages", "posts", "media"}},
		{"index a typed slice", ".collection_slugs[0]", "pages"},
		{"index past the end of a typed slice", ".collection_slugs[9]", nil},
		{"iterate an empty typed slice", ".empty_slugs[]", []any{}},
		{"field through a nested struct", ".connection.localization.locales", []any{"en", "de"}},
		{"iterate through a nested struct", ".connection.localization.locales[]", []any{"en", "de"}},
		{"index through a nested struct", ".connection.localization.locales[1]", "de"},
		{"scalar through a nested struct", ".connection.localization.enabled", true},
		{"iterate a slice of structs then a field", ".collections[].slug", []any{"pages", "posts"}},
		{"index a slice of structs then a field", ".collections[0].slug", "pages"},
		{"iterate a field of an iterated struct", ".collections[].ops[]", []any{"read", "read", "create"}},
		{"field of a named map type", ".named.key", "value"},
		{"number survives as json.Number", ".count", json.Number("3")},
		{"missing field is still null", ".nope", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := New("explain", KindCapabilities, capabilityData())
			if err := env.ApplyPath(tc.expr); err != nil {
				t.Fatalf("ApplyPath(%q): %v", tc.expr, err)
			}
			if diff := cmp.Diff(tc.want, env.Data); diff != "" {
				t.Errorf("--path %s (-want +got):\n%s", tc.expr, diff)
			}
		})
	}
}

// TestPathTypeErrorsOverMapShapedDataStayTruthful: the forms that genuinely do
// not apply must still fail — and must name the shape they actually found.
func TestPathTypeErrorsOverMapShapedDataStayTruthful(t *testing.T) {
	tests := []struct {
		name string
		expr string
		says string
	}{
		{"iterate an object", ".connection[]", "into an object"},
		{"iterate a struct-backed object", ".connection.localization[]", "into an object"},
		{"index a boolean", ".connection.localization.enabled[0]", "into a boolean"},
		{"field on a typed slice", ".collection_slugs.slug", "from an array"},
		{"field on a number", ".count.nope", "from a number"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := New("explain", KindCapabilities, capabilityData())
			err := env.ApplyPath(tc.expr)
			if err == nil {
				t.Fatalf("ApplyPath(%q) = nil error, want invalid_path_expr", tc.expr)
			}
			e, ok := apierr.As(err)
			if !ok || e.Code != apierr.CodeInvalidPathExpr {
				t.Fatalf("code = %v, want invalid_path_expr", err)
			}
			if !strings.Contains(e.Message, tc.says) {
				t.Errorf("message %q does not contain %q", e.Message, tc.says)
			}
		})
	}
}

// TestTypeNameNeverLies guards the message itself. The old default branch
// returned "a number" for every value it did not recognise, so an agent that
// ran `--path .queryable_paths[]` was told its array was a number — the CLI
// inventing a wrong explanation for its own bug.
func TestTypeNameNeverLies(t *testing.T) {
	type payload struct{ A int }
	var nilPtr *payload
	var nilSlice []string
	var nilMap map[string]int
	type myString string
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"typed slice", []string{"a"}, "an array"},
		{"typed array", [2]int{1, 2}, "an array"},
		{"slice of structs", []payload{{A: 1}}, "an array"},
		{"struct", payload{A: 1}, "an object"},
		{"pointer to struct", &payload{A: 1}, "an object"},
		{"named map", capNamedMap{"k": "v"}, "an object"},
		{"named string", myString("x"), "a string"},
		{"int", 3, "a number"},
		{"float", 3.5, "a number"},
		{"json.Number", json.Number("19"), "a number"},
		{"nil pointer", nilPtr, "null"},
		{"nil slice", nilSlice, "null"},
		{"nil map", nilMap, "null"},
		{"generic object", map[string]any{}, "an object"},
		{"generic array", []any{}, "an array"},
		{"string", "s", "a string"},
		{"bool", true, "a boolean"},
		{"nil", nil, "null"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := typeName(tc.in); got != tc.want {
				t.Errorf("typeName(%#v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestGenericNormalisesDepthFirst is the root-cause test: a map that is already
// map[string]any at the top but typed underneath must still come back fully
// generic, while data that is ALREADY generic is returned untouched (no marshal
// round trip for a large doc_list).
func TestGenericNormalisesDepthFirst(t *testing.T) {
	got := generic(map[string]any{"xs": []string{"a", "b"}})
	want := map[string]any{"xs": []any{"a", "b"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}

	alreadyGeneric := map[string]any{
		"id":   json.Number("6712345678901234567890"),
		"docs": []any{map[string]any{"title": "Home"}, nil, true, "x"},
	}
	out := generic(alreadyGeneric)
	if !reflect.DeepEqual(out, any(alreadyGeneric)) {
		t.Errorf("already-generic data was rewritten: %#v", out)
	}
	if fmt.Sprintf("%p", out.(map[string]any)) != fmt.Sprintf("%p", alreadyGeneric) {
		t.Errorf("already-generic data was copied; the fast path should return it as is")
	}
}

// TestPathNarrowedScalarRendersAsID is the §10.3 matrix / --path interaction.
//
// The matrix is keyed on data_kind, and data_kind describes what the COMMAND
// produced. After `--path .ok` there is nothing capabilities-shaped left in
// data — it is the bare value `true` — but the check still saw
// KindCapabilities, so `pay doctor --path .ok --output id`, an example the help
// registry itself prints, failed with format_unsupported (exit 5).
func TestPathNarrowedScalarRendersAsID(t *testing.T) {
	newEnv := func() *Envelope {
		return New("doctor", KindCapabilities, map[string]any{
			"ok":     true,
			"checks": []any{map[string]any{"name": "reachability", "ok": true}},
		})
	}

	t.Run("a scalar selection renders", func(t *testing.T) {
		env := newEnv()
		if err := env.ApplyPath(".ok"); err != nil {
			t.Fatalf("ApplyPath: %v", err)
		}
		if !env.NarrowedToScalar() {
			t.Fatal("NarrowedToScalar() = false for a bare bool")
		}
		var out, errBuf bytes.Buffer
		w := &Writer{Stdout: &out, Stderr: &errBuf, Format: FormatID}
		code, err := w.Render(env)
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		if code != 0 {
			t.Errorf("exit = %d, want 0; stdout=%q", code, out.String())
		}
		if got := strings.TrimSpace(out.String()); got != "true" {
			t.Errorf("stdout = %q, want \"true\"", got)
		}
	})

	t.Run("a STRUCTURED selection is still refused", func(t *testing.T) {
		env := newEnv()
		if err := env.ApplyPath(".checks"); err != nil {
			t.Fatalf("ApplyPath: %v", err)
		}
		if env.NarrowedToScalar() {
			t.Fatal("NarrowedToScalar() = true for a list of objects")
		}
	})

	t.Run("no --path at all is still refused", func(t *testing.T) {
		if newEnv().NarrowedToScalar() {
			t.Fatal("NarrowedToScalar() = true without --path")
		}
		if err := CheckFormat(FormatID, KindCapabilities); err == nil {
			t.Fatal("the matrix stopped rejecting `--output id` on capabilities")
		}
	})
}
