package query

import (
	"net/url"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

func TestWhereEncodeIsJSON(t *testing.T) {
	// The live-verified case: bracket notation cannot express null
	// (where[jobTitle][equals]=null matched 0 of 104 rows), the JSON string can
	// (23 rows).
	w := Term("jobTitle", OpEquals, nil)
	got, err := w.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if want := `{"jobTitle":{"equals":null}}`; got != want {
		t.Fatalf("JSON = %s, want %s", got, want)
	}
	enc, err := w.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	back, err := url.QueryUnescape(enc)
	if err != nil {
		t.Fatalf("unescape: %v", err)
	}
	if back != got {
		t.Fatalf("round trip = %s, want %s", back, got)
	}
}

func TestWhereJSONDoesNotEscapeHTML(t *testing.T) {
	got, err := Term("title", OpEquals, "a & b <c>").JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if strings.Contains(got, `\u0026`) || strings.Contains(got, `\u003c`) {
		t.Fatalf("value was HTML-escaped: %s", got)
	}
	if want := `{"title":{"equals":"a & b <c>"}}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestCombine(t *testing.T) {
	tests := []struct {
		name string
		and  []Where
		or   []Where
		want string
	}{
		{"empty", nil, nil, ""},
		{"single and unwrapped", []Where{Term("a", OpEquals, "1")}, nil, `{"a":{"equals":"1"}}`},
		{
			"two ands",
			[]Where{Term("a", OpEquals, "1"), Term("b", OpEquals, "2")}, nil,
			`{"and":[{"a":{"equals":"1"}},{"b":{"equals":"2"}}]}`,
		},
		{
			"single or unwrapped",
			nil, []Where{Term("a", OpEquals, "1")},
			`{"a":{"equals":"1"}}`,
		},
		{
			"or group is ANDed with the where terms",
			[]Where{Term("a", OpEquals, "1")},
			[]Where{Term("b", OpEquals, "2"), Term("c", OpEquals, "3")},
			`{"and":[{"a":{"equals":"1"}},{"or":[{"b":{"equals":"2"}},{"c":{"equals":"3"}}]}]}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Combine(tc.and, tc.or).JSON()
			if err != nil {
				t.Fatalf("JSON: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestParamsEncode(t *testing.T) {
	tests := []struct {
		name string
		p    Params
		want string
	}{
		{"zero value", Params{}, ""},
		{
			"select stays bracket notation",
			Params{Select: []string{"title", "id"}, Depth: IntPtr(0), Limit: IntPtr(2)},
			"select[title]=true&select[id]=true&depth=0&limit=2",
		},
		{
			"nested select path",
			Params{Select: []string{"hero.media"}},
			"select[hero][media]=true",
		},
		{
			"select exclude",
			Params{SelectExclude: []string{"content"}},
			"select[content]=false",
		},
		{
			"populate and joins",
			Params{
				Populate: map[string][]string{"crm-companies": {"name"}},
				Joins:    map[string]map[string]string{"activities": {"limit": "5", "sort": "-createdAt"}},
			},
			"populate[crm-companies][name]=true&joins[activities][limit]=5&joins[activities][sort]=-createdAt",
		},
		{
			"where is a URL-encoded JSON string",
			Params{Where: Term("jobTitle", OpEquals, nil), Limit: IntPtr(0)},
			"where=%7B%22jobTitle%22%3A%7B%22equals%22%3Anull%7D%7D&limit=0",
		},
		{
			"sort depth draft trash locale",
			Params{
				Sort: []string{"-createdAt", "id"}, Depth: IntPtr(1), Page: 2,
				Draft: BoolPtr(true), Trash: BoolPtr(false), Locale: "en", FallbackLocale: "none",
			},
			"sort=-createdAt%2Cid&depth=1&page=2&draft=true&trash=false&locale=en&fallback-locale=none",
		},
		{
			"where-raw is appended verbatim",
			Params{Limit: IntPtr(1), WhereRaw: "?where[id][equals]=3"},
			"limit=1&where[id][equals]=3",
		},
		{
			"extras are sorted",
			Params{Extra: url.Values{"b": {"2"}, "a": {"1"}}},
			"a=1&b=2",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.p.Encode()
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestParamsEncodeQSStyleRejectsNull(t *testing.T) {
	p := Params{Where: Term("jobTitle", OpEquals, nil), WhereStyle: StyleQS}
	_, err := p.Encode()
	if !apierr.HasCode(err, apierr.CodeInvalidWhereSyntax) {
		t.Fatalf("err = %v, want invalid_where_syntax", err)
	}
}

func TestParamsEncodeQSStyle(t *testing.T) {
	p := Params{Where: Term("id", OpEquals, 3), WhereStyle: StyleQS}
	got, err := p.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if want := "where[id][equals]=3"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestParseWhereStyle(t *testing.T) {
	for _, in := range []string{"", "json"} {
		if got, err := ParseWhereStyle(in); err != nil || got != StyleJSON {
			t.Fatalf("ParseWhereStyle(%q) = %v, %v", in, got, err)
		}
	}
	if got, err := ParseWhereStyle("qs"); err != nil || got != StyleQS {
		t.Fatalf("ParseWhereStyle(qs) = %v, %v", got, err)
	}
	if _, err := ParseWhereStyle("bracket"); !apierr.HasCode(err, apierr.CodeInvalidOption) {
		t.Fatalf("err = %v, want invalid_option", err)
	}
}

func TestParamsCloneIsDeep(t *testing.T) {
	p := Params{
		Where:    Term("a", OpEquals, "1"),
		Select:   []string{"id"},
		Populate: map[string][]string{"c": {"f"}},
		Joins:    map[string]map[string]string{"j": {"limit": "1"}},
		Extra:    url.Values{"x": {"1"}},
	}
	c := p.Clone()
	c.Where["b"] = map[string]any{"equals": "2"}
	c.Select[0] = "title"
	c.Populate["c"][0] = "other"
	c.Joins["j"]["limit"] = "9"
	c.Extra.Set("x", "9")
	if _, ok := p.Where["b"]; ok {
		t.Error("Where was shared")
	}
	if p.Select[0] != "id" {
		t.Error("Select was shared")
	}
	if p.Populate["c"][0] != "f" {
		t.Error("Populate was shared")
	}
	if p.Joins["j"]["limit"] != "1" {
		t.Error("Joins was shared")
	}
	if p.Extra.Get("x") != "1" {
		t.Error("Extra was shared")
	}
}
