package query

import (
	"reflect"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

func TestExtractPathsAndOperators(t *testing.T) {
	w := Where{"and": []any{
		map[string]any{"status": map[string]any{"equals": "lead"}},
		map[string]any{"or": []any{
			map[string]any{"company.name": map[string]any{"contains": "acme"}},
			map[string]any{"jobTitle": map[string]any{"exists": false}},
		}},
	}}
	if got, want := ExtractPaths(w), []string{"company.name", "jobTitle", "status"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractPaths = %v, want %v", got, want)
	}
	if got, want := ExtractOperators(w), []string{"contains", "equals", "exists"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractOperators = %v, want %v", got, want)
	}
}

func TestValidateWhere(t *testing.T) {
	schema := Schema{
		Collection: "crm-contacts",
		Queryable:  []string{"id", "status", "company", "jobTitle"},
	}
	tests := []struct {
		name string
		w    Where
		s    Schema
		code apierr.Code
	}{
		{"known path", Term("status", OpEquals, "lead"), schema, ""},
		{"relational hop via the root segment", Term("company.name", OpContains, "a"), schema, ""},
		{"unknown path", Term("nosuch", OpEquals, "1"), schema, apierr.CodeQueryPathInvalid},
		{"unknown path but schema unknown", Term("nosuch", OpEquals, "1"), Schema{}, ""},
		{"empty where", nil, schema, ""},
		{
			"bogus operator from --where-json",
			Where{"status": map[string]any{"matches": "lead"}}, schema, apierr.CodeUnsupportedOperator,
		},
		{
			"adapter-gated operator on postgres",
			Term("tags", OpAll, []any{1}),
			Schema{Queryable: []string{"tags"}, DBAdapter: "postgres", DBAdapterSource: "configured"},
			apierr.CodeUnsupportedOperator,
		},
		{
			"adapter-gated operator with an unknown adapter",
			Term("tags", OpAll, []any{1}),
			Schema{Queryable: []string{"tags"}},
			"",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateWhere(tc.w, tc.s)
			if tc.code == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !apierr.HasCode(err, tc.code) {
				t.Fatalf("err = %v, want %s", err, tc.code)
			}
		})
	}
}

func TestValidateWhereSuggests(t *testing.T) {
	err := ValidateWhere(Term("statuss", OpEquals, "lead"), Schema{Queryable: []string{"status", "id"}})
	e, ok := apierr.As(err)
	if !ok {
		t.Fatalf("err = %v", err)
	}
	if len(e.DidYouMean) == 0 || e.DidYouMean[0] != "status" {
		t.Fatalf("did_you_mean = %v, want status first", e.DidYouMean)
	}
}

func TestValidateSort(t *testing.T) {
	s := Schema{Collection: "pages", Sortable: []string{"id", "createdAt", "title"}}
	for _, ok := range []string{"id", "-createdAt", ""} {
		if err := ValidateSort([]string{ok}, s); err != nil {
			t.Fatalf("ValidateSort(%q) = %v", ok, err)
		}
	}
	if err := ValidateSort([]string{"bogusField"}, s); !apierr.HasCode(err, apierr.CodeInvalidSortField) {
		t.Fatalf("err = %v, want invalid_sort_field", err)
	}
	// Tri-state: an unknown sortable set never rejects.
	if err := ValidateSort([]string{"bogusField"}, Schema{}); err != nil {
		t.Fatalf("unknown schema rejected: %v", err)
	}
}

func TestValidateSelect(t *testing.T) {
	s := Schema{Collection: "pages", Selectable: []string{"id", "title", "hero"}}
	if err := ValidateSelect([]string{"title", "hero.media"}, s); err != nil {
		t.Fatalf("ValidateSelect: %v", err)
	}
	if err := ValidateSelect([]string{"nope"}, s); !apierr.HasCode(err, apierr.CodeUnknownField) {
		t.Fatalf("err = %v, want unknown_field", err)
	}
	if err := ValidateSelect([]string{"nope"}, Schema{}); err != nil {
		t.Fatalf("unknown schema rejected: %v", err)
	}
}

func TestValidateDateField(t *testing.T) {
	s := Schema{Collection: "posts", DateFields: []string{"createdAt", "updatedAt", "publishedAt"}}
	if err := ValidateDateField("publishedAt", s); err != nil {
		t.Fatalf("ValidateDateField: %v", err)
	}
	if err := ValidateDateField("title", s); !apierr.HasCode(err, apierr.CodeUnknownField) {
		t.Fatalf("err = %v, want unknown_field", err)
	}
	if err := ValidateDateField("title", Schema{}); err != nil {
		t.Fatalf("unknown schema rejected: %v", err)
	}
	if err := ValidateDateField("", s); err != nil {
		t.Fatalf("empty date field rejected: %v", err)
	}
}

func TestValidateLimit(t *testing.T) {
	if err := ValidateLimit(nil); err != nil {
		t.Fatalf("nil limit: %v", err)
	}
	if err := ValidateLimit(IntPtr(20)); err != nil {
		t.Fatalf("limit 20: %v", err)
	}
	for _, bad := range []int{0, -1} {
		if err := ValidateLimit(IntPtr(bad)); !apierr.HasCode(err, apierr.CodeInvalidArgs) {
			t.Fatalf("ValidateLimit(%d) = %v, want invalid_args", bad, err)
		}
	}
}
