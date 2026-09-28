package manifest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestParseRefs(t *testing.T) {
	ok := []struct {
		in, want string
		as       string
	}{
		{`{"$ref":"pages","slug":"about"}`, "pages{slug=about}", AsID},
		{`{"$ref":"pages","slug":"about","locale":"de","$as":"polymorphic"}`, "pages{locale=de,slug=about}", AsPolymorphic},
		{`{"$ref":"posts","where":{"title":{"like":"x"}}}`, "posts{where}", AsID},
		{`{"$ref":"@home"}`, "@home", AsID},
		{`{"$ref":"@home","$as":"polymorphic"}`, "@home", AsPolymorphic},
		{`{"$ref":"media","filename":"a.png","n":3,"b":true}`, "media{b=true,filename=a.png,n=3}", AsID},
	}
	for _, tc := range ok {
		var m map[string]any
		dec := json.NewDecoder(strings.NewReader(tc.in))
		dec.UseNumber()
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		r, err := Parse(m, "x")
		if err != nil {
			t.Errorf("%s: %v", tc.in, err)
			continue
		}
		if r.String() != tc.want || r.As != tc.as || r.Path != "x" {
			t.Errorf("%s: %s %s", tc.in, r.String(), r.As)
		}
	}
	bad := map[string]string{
		`{"$ref":""}`:                                "must name a collection",
		`{"$ref":7}`:                                 "must name a collection",
		`{"$ref":"pages","$as":"object"}`:            `"$as" must be`,
		`{"$ref":"@"}`:                               "must be followed",
		`{"$ref":"@k","slug":"x"}`:                   "takes only",
		`{"$ref":"pages"}`:                           "names no document",
		`{"$ref":"pages","$where":{}}`:               "unknown key",
		`{"$ref":"pages","slug":{"a":1}}`:            "must be a string, number or boolean",
		`{"$ref":"pages","slug":null}`:               "must be a string, number or boolean",
		`{"$ref":"pages","where":[]}`:                "non-empty Payload where object",
		`{"$ref":"pages","where":{},"slug":"x"}`:     "non-empty Payload where object",
		`{"$ref":"pages","slug":["a","b"]}`:          "must be a string, number or boolean",
		`{"$ref":"pages","$as":"polymorphic","x":1}`: "",
	}
	for in, want := range bad {
		var m map[string]any
		_ = json.Unmarshal([]byte(in), &m)
		_, err := Parse(m, "p")
		if want == "" {
			if err != nil {
				t.Errorf("%s: %v", in, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v (want %q)", in, err, want)
		}
	}
}

func TestCollectReplaceHasRefs(t *testing.T) {
	var body map[string]any
	_ = json.Unmarshal([]byte(`{"title":"t","parent":{"$ref":"pages","slug":"a"},
		"layout":[{"blockType":"cta","links":[{"link":{"reference":{"$ref":"@k","$as":"polymorphic"}}}]}],
		"cats":[{"$ref":"categories","slug":"x"},{"$ref":"categories","slug":"y"}]}`), &body)
	refs, err := Collect(body)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, r := range refs {
		paths = append(paths, r.Path)
	}
	if strings.Join(paths, " ") != "cats[0] cats[1] layout[0].links[0].link.reference parent" {
		t.Errorf("paths = %v", paths)
	}
	if !HasRefs(body) || HasRefs(map[string]any{"a": []any{map[string]any{"b": 1}}}) {
		t.Error("HasRefs")
	}
	before, _ := json.Marshal(body)
	out, err := Replace(body, func(r *Ref) (any, error) {
		if r.IsLocal() {
			return r.Value("pages", 9), nil
		}
		return r.Value(r.Collection, r.Equals[0].Value), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(body)
	if string(before) != string(after) {
		t.Error("Replace mutated its input")
	}
	got, _ := json.Marshal(out)
	for _, want := range []string{`"parent":"a"`, `"cats":["x","y"]`, `"reference":{"relationTo":"pages","value":9}`, `"title":"t"`} {
		if !strings.Contains(string(got), want) {
			t.Errorf("%s lacks %s", got, want)
		}
	}
	if _, err := Replace(body, func(*Ref) (any, error) { return nil, fmt.Errorf("boom") }); err == nil {
		t.Error("resolver error swallowed")
	}
	if _, err := Collect(map[string]any{"x": map[string]any{"$ref": ""}}); err == nil {
		t.Error("bad ref collected")
	}
}
