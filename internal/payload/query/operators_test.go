package query

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

func TestCanonicalAliases(t *testing.T) {
	tests := map[string]string{
		"eq": OpEquals, "=": OpEquals, "is": OpEquals, "equals": OpEquals,
		"ne": OpNotEquals, "!=": OpNotEquals, "not": OpNotEquals,
		"gt": OpGreaterThan, ">": OpGreaterThan,
		"gte": OpGreaterThanEqual, ">=": OpGreaterThanEqual,
		"lt": OpLessThan, "<": OpLessThan,
		"lte": OpLessThanEqual, "<=": OpLessThanEqual,
		"contains": OpContains, "~": OpContains,
		"like":  OpLike,
		"nlike": OpNotLike, "!~": OpNotLike, "not_like": OpNotLike,
		"in": OpIn, "nin": OpNotIn, "not_in": OpNotIn,
		"all": OpAll, "exists": OpExists, "near": OpNear,
		"within": OpWithin, "intersects": OpIntersects,
	}
	for alias, want := range tests {
		got, ok := Canonical(alias)
		if !ok || got != want {
			t.Errorf("Canonical(%q) = %q, %v; want %q", alias, got, ok, want)
		}
	}
	if _, ok := Canonical("nope"); ok {
		t.Error("Canonical accepted an unknown alias")
	}
}

func TestOperatorsListIsThe16(t *testing.T) {
	if len(Operators) != 16 {
		t.Fatalf("Operators has %d entries, want the 16 Payload implements", len(Operators))
	}
	for _, op := range Operators {
		if !IsOperator(op) {
			t.Errorf("%q is listed but not recognised", op)
		}
	}
	if IsOperator("eq") {
		t.Error("an alias must not pass as a canonical operator")
	}
}

func TestBuildTermValueHandling(t *testing.T) {
	tests := []struct {
		name string
		path string
		op   string
		val  string
		want string
	}{
		{"typed number", "id", "eq", "3", `{"id":{"equals":3}}`},
		{"typed null", "jobTitle", "eq", "null", `{"jobTitle":{"equals":null}}`},
		{"typed bool", "emailOptOut", "eq", "true", `{"emailOptOut":{"equals":true}}`},
		{"quoted forces string", "code", "eq", `"123"`, `{"code":{"equals":"123"}}`},
		{"value with spaces", "name", "eq", "Grace Hopper", `{"name":{"equals":"Grace Hopper"}}`},
		{"contains escapes the wildcards", "firstName", "contains", "50%_x", `{"firstName":{"contains":"50\\%\\_x"}}`},
		{"like is passed through", "name", "like", "grace hopper", `{"name":{"like":"grace hopper"}}`},
		{"not_like auto-wraps", "lastName", "nlike", "hopper", `{"lastName":{"not_like":"%hopper%"}}`},
		{"not_like keeps an explicit wrap", "lastName", "nlike", "%hopper%", `{"lastName":{"not_like":"%hopper%"}}`},
		{"in comma splits and types", "tags", "in", "1,2,x", `{"tags":{"in":[1,2,"x"]}}`},
		{"in escaped comma", "name", "in", `a\,b,c`, `{"name":{"in":["a,b","c"]}}`},
		{"in json array", "tags", "in", "json:[1,2]", `{"tags":{"in":[1,2]}}`},
		{"nin maps to not_in", "tags", "nin", "1", `{"tags":{"not_in":[1]}}`},
		{"exists defaults to true", "jobTitle", "exists", "", `{"jobTitle":{"exists":true}}`},
		{"exists false", "jobTitle", "exists", "false", `{"jobTitle":{"exists":false}}`},
		{"near string form", "location", "near", "-73.99,40.73,5000", `{"location":{"near":"-73.99,40.73,5000"}}`},
		{"near with minMeters", "location", "near", "-73.99,40.73,5000,10", `{"location":{"near":"-73.99,40.73,5000,10"}}`},
		{
			"within geojson literal", "area", "within",
			`{"type":"Polygon","coordinates":[[[0,0],[0,1],[1,1],[0,0]]]}`,
			`{"area":{"within":{"coordinates":[[[0,0],[0,1],[1,1],[0,0]]],"type":"Polygon"}}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, err := BuildTerm(tc.path, tc.op, tc.val, ParseOptions{})
			if err != nil {
				t.Fatalf("BuildTerm: %v", err)
			}
			got, err := w.JSON()
			if err != nil {
				t.Fatalf("JSON: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestBuildTermErrors(t *testing.T) {
	tests := []struct {
		name string
		op   string
		val  string
		code apierr.Code
	}{
		{"unknown operator", "matches", "x", apierr.CodeUnsupportedOperator},
		{"exists non-bool", "exists", "maybe", apierr.CodeInvalidWhereSyntax},
		{"near too few parts", "near", "1,2", apierr.CodeInvalidWhereSyntax},
		{"near non-numeric", "near", "a,b,c", apierr.CodeInvalidWhereSyntax},
		{"near too many parts", "near", "1,2,3,4,5", apierr.CodeInvalidWhereSyntax},
		{"geo not json", "within", "somewhere", apierr.CodeInvalidWhereSyntax},
		{"empty list", "in", " ", apierr.CodeInvalidWhereSyntax},
		{"bad json value", "eq", "json:{oops", apierr.CodeInvalidWhereSyntax},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildTerm("f", tc.op, tc.val, ParseOptions{})
			if !apierr.HasCode(err, tc.code) {
				t.Fatalf("err = %v, want %s", err, tc.code)
			}
		})
	}
}

func TestGeoFromFile(t *testing.T) {
	opts := ParseOptions{ReadFile: func(path string) ([]byte, error) {
		if path != "area.geojson" {
			t.Fatalf("ReadFile got %q", path)
		}
		return []byte(`{"type":"Point","coordinates":[1,2]}`), nil
	}}
	w, err := BuildTerm("area", "intersects", "@area.geojson", opts)
	if err != nil {
		t.Fatalf("BuildTerm: %v", err)
	}
	got, _ := w.JSON()
	want := `{"area":{"intersects":{"coordinates":[1,2],"type":"Point"}}}`
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	if _, err := BuildTerm("area", "within", "@area.geojson", ParseOptions{}); !apierr.HasCode(err, apierr.CodeInvalidArgs) {
		t.Fatalf("without a ReadFile the @file form must be refused, got %v", err)
	}
}

func TestEscapeLikeAndWrapLike(t *testing.T) {
	if got := EscapeLike(`100%_a\b`); got != `100\%\_a\\b` {
		t.Fatalf("EscapeLike = %q", got)
	}
	if got := WrapLike("x"); got != "%x%" {
		t.Fatalf("WrapLike = %q", got)
	}
	if got := WrapLike("%x%"); got != "%x%" {
		t.Fatalf("WrapLike double-wrapped: %q", got)
	}
	if got := WrapLike("%"); got != "%%%" {
		t.Fatalf("a lone %% must still be wrapped, got %q", got)
	}
}

func TestSplitList(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"a,b,c", []string{"a", "b", "c"}},
		{`a\,b,c`, []string{"a,b", "c"}},
		{`a\\b`, []string{`a\b`}},
		{"a,,b", []string{"a", "b"}},
		{"a,b,", []string{"a", "b"}},
		{"", nil},
	}
	for _, tc := range tests {
		got := SplitList(tc.in)
		if len(got) == 0 && len(tc.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("SplitList(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

func TestCheckOperatorIsTriState(t *testing.T) {
	tests := []struct {
		name          string
		op            string
		adapter       string
		source        string
		wantRejection bool
	}{
		{"all on configured postgres", OpAll, "postgres", "configured", true},
		{"near on configured postgres", OpNear, "postgres", "configured", true},
		{"all on mongo", OpAll, "mongodb", "configured", false},
		{"all on an inferred postgres", OpAll, "postgres", "inferred", false},
		{"all with an unknown adapter", OpAll, "unknown", "unknown", false},
		{"equals is never gated", OpEquals, "postgres", "configured", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckOperator(tc.op, tc.adapter, tc.source)
			if tc.wantRejection != (err != nil) {
				t.Fatalf("err = %v, wantRejection=%v", err, tc.wantRejection)
			}
			if err != nil && !apierr.HasCode(err, apierr.CodeUnsupportedOperator) {
				t.Fatalf("err = %v, want unsupported_operator", err)
			}
		})
	}
}

func TestListOperandKeepsNumberFormatting(t *testing.T) {
	w, err := BuildTerm("v", "in", "1.50,2", ParseOptions{})
	if err != nil {
		t.Fatalf("BuildTerm: %v", err)
	}
	got, _ := w.JSON()
	if got != `{"v":{"in":[1.50,2]}}` {
		t.Fatalf("number formatting was not preserved: %s", got)
	}
	var probe any
	if err := json.Unmarshal([]byte(got), &probe); err != nil {
		t.Fatalf("emitted invalid JSON: %v", err)
	}
}
