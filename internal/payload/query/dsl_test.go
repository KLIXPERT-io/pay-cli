package query

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

func TestTypeValue(t *testing.T) {
	tests := []struct {
		in   string
		want any
	}{
		{"null", nil},
		{"true", true},
		{"false", false},
		{"3", json.Number("3")},
		{"-2.5", json.Number("-2.5")},
		{"1e6", json.Number("1e6")},
		{`"123"`, "123"},
		{`'123'`, "123"},
		{"lead", "lead"},
		{"Grace Hopper", "Grace Hopper"},
		{"json:[1,2]", []any{json.Number("1"), json.Number("2")}},
		{"0x10", "0x10"},
		{"NaN", "NaN"},
		{"Inf", "Inf"},
		{"1_000", "1_000"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := TypeValue(tc.in)
			if err != nil {
				t.Fatalf("TypeValue: %v", err)
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tc.want)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("TypeValue(%q) = %s, want %s", tc.in, gotJSON, wantJSON)
			}
		})
	}
}

func TestParseTerm(t *testing.T) {
	tests := []struct {
		name string
		term string
		want string
	}{
		{"simple", "status eq lead", `{"status":{"equals":"lead"}}`},
		{"dotted path", "company.name contains acme", `{"company.name":{"contains":"acme"}}`},
		{"symbolic alias", "id >= 10", `{"id":{"greater_than_equal":10}}`},
		{"value with spaces", "name eq Grace Hopper", `{"name":{"equals":"Grace Hopper"}}`},
		{"extra whitespace", "  status   eq   lead  ", `{"status":{"equals":"lead"}}`},
		{"underscore path", "_status eq published", `{"_status":{"equals":"published"}}`},
		{"two tokens for exists", "jobTitle exists", `{"jobTitle":{"exists":true}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, err := ParseTerm(tc.term, ParseOptions{})
			if err != nil {
				t.Fatalf("ParseTerm: %v", err)
			}
			got, _ := w.JSON()
			if got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestParseTermErrors(t *testing.T) {
	tests := []struct {
		name string
		term string
		code apierr.Code
	}{
		{"one token", "status", apierr.CodeInvalidWhereSyntax},
		{"empty", "   ", apierr.CodeInvalidWhereSyntax},
		{"path with a pipe", "docs|map eq 1", apierr.CodeInvalidWhereSyntax},
		{"path with a bracket", "a[0] eq 1", apierr.CodeInvalidWhereSyntax},
		{"leading dot", ".a eq 1", apierr.CodeInvalidWhereSyntax},
		{"double dot", "a..b eq 1", apierr.CodeInvalidWhereSyntax},
		{"unknown operator", "status matches lead", apierr.CodeUnsupportedOperator},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseTerm(tc.term, ParseOptions{})
			if !apierr.HasCode(err, tc.code) {
				t.Fatalf("err = %v, want %s", err, tc.code)
			}
		})
	}
}

func TestBuild(t *testing.T) {
	tests := []struct {
		name string
		in   Input
		want string
	}{
		{"empty", Input{}, ""},
		{
			"where and or",
			Input{Where: []string{"status eq lead"}, Or: []string{"a eq 1", "b eq 2"}},
			`{"and":[{"status":{"equals":"lead"}},{"or":[{"a":{"equals":1}},{"b":{"equals":2}}]}]}`,
		},
		{
			"where-json replaces everything",
			Input{Where: []string{"status eq lead"}, WhereJSON: `{"id":{"equals":1}}`},
			`{"id":{"equals":1}}`,
		},
		{
			"ids sugar with a number id_type",
			Input{IDs: []string{"1,2"}, IDType: "number"},
			`{"id":{"in":[1,2]}}`,
		},
		{
			"ids sugar with a string id_type",
			Input{IDs: []string{"66f1a2b3c4d5e6f708192a3b"}, IDType: "string"},
			`{"id":{"in":["66f1a2b3c4d5e6f708192a3b"]}}`,
		},
		{
			"published-only sugar",
			Input{PublishedOnly: true},
			`{"_status":{"equals":"published"}}`,
		},
		{
			"draft-only sugar",
			Input{DraftOnly: true},
			`{"_status":{"equals":"draft"}}`,
		},
		{
			"q fans out over the text fields",
			Input{Q: "ada", QFields: []string{"name", "email"}},
			`{"or":[{"name":{"contains":"ada"}},{"email":{"contains":"ada"}}]}`,
		},
		{
			"extra terms are ANDed",
			Input{Where: []string{"a eq 1"}, Extra: []Where{Term("updatedAt", OpGreaterThanEqual, "2026-01-01T00:00:00Z")}},
			`{"and":[{"a":{"equals":1}},{"updatedAt":{"greater_than_equal":"2026-01-01T00:00:00Z"}}]}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, err := Build(tc.in)
			if err != nil {
				t.Fatalf("Build: %v", err)
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

func TestBuildErrors(t *testing.T) {
	if _, err := Build(Input{DraftOnly: true, PublishedOnly: true}); !apierr.HasCode(err, apierr.CodeInvalidArgs) {
		t.Fatalf("mutually exclusive status flags: %v", err)
	}
	if _, err := Build(Input{WhereJSON: "[1]"}); !apierr.HasCode(err, apierr.CodeInvalidWhereSyntax) {
		t.Fatalf("array where-json: %v", err)
	}
	if _, err := Build(Input{WhereJSON: "{oops"}); !apierr.HasCode(err, apierr.CodeInvalidWhereSyntax) {
		t.Fatalf("malformed where-json: %v", err)
	}
	if _, err := Build(Input{Q: "x"}); !apierr.HasCode(err, apierr.CodeInvalidArgs) {
		t.Fatalf("--q with no fields: %v", err)
	}
	if _, err := Build(Input{IDs: []string{"abc"}, IDType: "number"}); !apierr.HasCode(err, apierr.CodeInvalidID) {
		t.Fatalf("non-castable id: %v", err)
	}
}

func TestTypedIDUnknownTypeNeverRejects(t *testing.T) {
	// §9.3's tri-state rule: an unknown id_type must never produce invalid_id.
	for _, id := range []string{"66f1a2b3c4d5e6f708192a3b", "17", "weird id"} {
		if _, err := TypedID(id, "unknown"); err != nil {
			t.Fatalf("TypedID(%q, unknown) = %v, want no rejection", id, err)
		}
	}
}

func TestQTermCaps(t *testing.T) {
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = string(rune('a' + i))
	}
	w, err := QTerm("x", fields)
	if err != nil {
		t.Fatalf("QTerm: %v", err)
	}
	group, ok := w["or"].([]any)
	if !ok {
		t.Fatalf("QTerm did not build an or group: %v", w)
	}
	if len(group) != MaxQFields {
		t.Fatalf("QTerm fanned out to %d fields, want %d", len(group), MaxQFields)
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Time
	}{
		{"12h", now.Add(-12 * time.Hour)},
		{"30d", time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)},
		{"4w", time.Date(2026, 3, 3, 12, 0, 0, 0, time.UTC)},
		{"1mo", time.Date(2026, 2, 28, 12, 0, 0, 0, time.UTC)},
		{"720h", now.Add(-720 * time.Hour)},
		{"36h30m", now.Add(-(36*time.Hour + 30*time.Minute))},
		{"2026-08-01", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)},
		{"2026-08-01T09:30:00Z", time.Date(2026, 8, 1, 9, 30, 0, 0, time.UTC)},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseSince(tc.in, now)
			if err != nil {
				t.Fatalf("ParseSince: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("ParseSince(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
	for _, bad := range []string{"", "yesterday", "7", "3x", "-1d"} {
		if _, err := ParseSince(bad, now); !apierr.HasCode(err, apierr.CodeInvalidArgs) {
			t.Fatalf("ParseSince(%q) = %v, want invalid_args", bad, err)
		}
	}
}

func TestDateTerm(t *testing.T) {
	since := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	got, _ := DateTerm("publishedAt", &since, &until).JSON()
	want := `{"and":[{"publishedAt":{"greater_than_equal":"2026-08-17T00:00:00Z"}},` +
		`{"publishedAt":{"less_than_equal":"2026-09-01T00:00:00Z"}}]}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if w := DateTerm("updatedAt", &since, nil); len(w) != 1 {
		t.Fatalf("one bound must not be wrapped in and: %v", w)
	}
}
