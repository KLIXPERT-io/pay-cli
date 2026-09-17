package query

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

func TestBrackets(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"bool leaf", true, "p=true"},
		{"string leaf", "a b", "p=a+b"},
		{"number leaf", json.Number("5"), "p=5"},
		{"int leaf", 7, "p=7"},
		{"float leaf", 1.5, "p=1.5"},
		{"nested map sorted", map[string]any{"b": true, "a": false}, "p[a]=false&p[b]=true"},
		{"array indexed", []any{"x", "y"}, "p[0]=x&p[1]=y"},
		{"string slice", []string{"x"}, "p[0]=x"},
		{
			"joins shape",
			map[string]any{"activities": map[string]any{"limit": 5, "sort": "-createdAt"}},
			"p[activities][limit]=5&p[activities][sort]=-createdAt",
		},
		{"key is escaped", map[string]any{"a b": true}, "p[a+b]=true"},
		{"map[string]string normalises", map[string]string{"k": "v"}, "p[k]=v"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pairs, err := Brackets("p", tc.in)
			if err != nil {
				t.Fatalf("Brackets: %v", err)
			}
			if got := EncodePairs(pairs); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestBracketsRejectsNull(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
	}{
		{"bare null", nil},
		{"null in a map", map[string]any{"jobTitle": map[string]any{"equals": nil}}},
		{"null in an array", []any{"a", nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Brackets("where", tc.in)
			if !apierr.HasCode(err, apierr.CodeInvalidWhereSyntax) {
				t.Fatalf("err = %v, want invalid_where_syntax", err)
			}
			if !strings.Contains(err.Error(), "cannot express null") {
				t.Fatalf("message does not explain the limitation: %v", err)
			}
		})
	}
}

func TestWhereBrackets(t *testing.T) {
	w := Where{"and": []any{
		map[string]any{"status": map[string]any{"equals": "lead"}},
		map[string]any{"or": []any{
			map[string]any{"a": map[string]any{"equals": 1}},
			map[string]any{"b": map[string]any{"exists": true}},
		}},
	}}
	pairs, err := WhereBrackets(w)
	if err != nil {
		t.Fatalf("WhereBrackets: %v", err)
	}
	want := "where[and][0][status][equals]=lead&" +
		"where[and][1][or][0][a][equals]=1&" +
		"where[and][1][or][1][b][exists]=true"
	if got := EncodePairs(pairs); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestWhereBracketsEmpty(t *testing.T) {
	pairs, err := WhereBrackets(nil)
	if err != nil || len(pairs) != 0 {
		t.Fatalf("WhereBrackets(nil) = %v, %v", pairs, err)
	}
}
