package manifest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func problemsOf(t *testing.T, err error) (apierr.Code, []apierr.Failure) {
	t.Helper()
	var e *apierr.Error
	if !errors.As(err, &e) {
		t.Fatalf("not an apierr: %v", err)
	}
	return e.Code, e.Failures
}

func TestLoadFormsDefaultsAndOrder(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "b/two.json", `[{"collection":"pages","data":{"slug":"b1"}},
		{"key":"k","collection":"pages","match":"slug, locale","status":"published","locale":"de","data":{"slug":"b2","locale":"x"}}]`)
	write(t, dir, "a.json", `{"$comment":"documented","collection":"posts","data":{"slug":"a","title":"A"}}`)
	write(t, dir, "c.json", `{"global":"footer","status":"draft","data":{"navItems":[]}}`)
	write(t, dir, ".git/x.json", `garbage`)
	write(t, dir, "notes.txt", `garbage`)
	write(t, dir, "env.json", `{"ok":true,"v":1,"data_kind":"doc","meta":{},"target":{"kind":"collection","slug":"pages"},"data":{"id":3,"slug":"env"}}`)
	items, err := Load([]string{dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range items {
		got = append(got, fmt.Sprintf("%s|%s|%s|%s|%s", it.Key, it.Label()[len(dir)+1:], it.Status, strings.Join(it.Match, ","), it.Locale))
	}
	want := []string{
		"posts/a|a.json|keep|slug|",
		"pages/b1|b/two.json#0|keep|slug|",
		"k|b/two.json#1|published|slug,locale|de",
		"globals/footer|c.json|draft||",
		"pages/env|env.json|keep|slug|",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !items[4].FromEnvelope || items[4].Data["id"] == nil {
		t.Errorf("envelope item = %+v", items[4])
	}
}

func TestLoadStdinAndEnvelopes(t *testing.T) {
	items, err := Load([]string{"-"}, strings.NewReader(`{"ok":true,"v":1,"data_kind":"doc_list","meta":{},
		"target":{"kind":"collection","slug":"posts"},"data":[{"slug":"x"},{"slug":"y"}]}`))
	if err != nil || len(items) != 2 || items[1].Key != "posts/y" || items[1].Label() != "-#1" {
		t.Fatalf("doc_list: %v %+v", err, items)
	}
	items, err = Load([]string{"-"}, strings.NewReader(`{"ok":true,"v":1,"data_kind":"global","meta":{},
		"target":{"kind":"global","slug":"header"},"data":{"navItems":[]}}`))
	if err != nil || items[0].Key != "globals/header" || items[0].Global != "header" {
		t.Fatalf("global: %v %+v", err, items)
	}
	for _, bad := range []string{
		`{"ok":false,"v":1,"data_kind":"error","meta":{}}`,
		`{"ok":true,"v":1,"data_kind":"count","meta":{},"target":{"slug":"pages"},"data":3}`,
		`{"ok":true,"v":1,"data_kind":"doc","meta":{},"data":{}}`,
	} {
		if _, err := Load([]string{"-"}, strings.NewReader(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, err := Load([]string{"-", "-"}, strings.NewReader(`{"collection":"a","data":{"slug":"s"}}`)); err == nil {
		t.Error("stdin read twice")
	}
}

func TestLoadCollectsEveryProblem(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.json", `[
		{"collection":"pages","stauts":"draft","data":{"slug":"a"}},
		{"collection":"pages","global":"x","data":{}},
		{"data":{}},
		{"collection":"pages","status":"live","data":{"slug":"b"}},
		{"global":"footer","match":["slug"],"data":{}},
		{"collection":"pages"},
		{"collection":"pages","data":null},
		{"collection":"pages","data":[]},
		{"collection":"pages","match":[],"data":{"slug":"c"}},
		{"collection":"pages","match":["slug","slug"],"data":{"slug":"d"}},
		{"key":"@x","collection":"pages","data":{"slug":"e"}},
		{"key":"a b","collection":"pages","data":{"slug":"f"}},
		{"collection":"pages","data":{"title":"no slug"}},
		{"collection":"pages","data":{"slug":{"$ref":"pages","slug":"x"}}},
		{"collection":"pages","data":{"slug":"dup"}},
		{"collection":"pages","data":{"slug":"dup"}},
		{"key":"k1","collection":"pages","data":{"slug":"same"}},
		{"key":"k2","collection":"pages","data":{"slug":"same"}},
		{"collection":"pages","collection2":1,"data":{"slug":"g"}},
		{"collection":7,"data":{"slug":"h"}},
		"not an object"
	]`)
	write(t, dir, "b.json", `{nope`)
	write(t, dir, "c.json", ``)
	write(t, dir, "d.json", `[]`)
	write(t, dir, "e.json", `{"collection":"pages","data":{"slug":"x"}} {"second":1}`)
	write(t, dir, "f.json", `42`)
	_, err := Load([]string{dir, filepath.Join(dir, "missing.json")}, nil)
	code, fails := problemsOf(t, err)
	if code != apierr.CodeManifestInvalid {
		t.Errorf("code = %s", code)
	}
	var msgs []string
	for _, f := range fails {
		msgs = append(msgs, fmt.Sprint(f.ID)+": "+f.Message)
	}
	joined := strings.Join(msgs, "\n")
	for _, want := range []string{
		`a.json#0: unknown key "stauts"`, `did you mean "status"?`,
		`a.json#1: sets both "collection" and "global"`,
		`a.json#2: names no target`,
		`a.json#3: "status" "live" is not draft, published or keep`,
		`a.json#4: "match" is for collection items`,
		`a.json#5: has no "data"`,
		`a.json#6: "data" is null`,
		`a.json#7: "data" must be an object, not an array`,
		`a.json#8: "match" is empty`,
		`a.json#9: "match" names "slug" twice`,
		`a.json#10: "key" "@x" must not start with @`,
		`a.json#11: "key" "a b" must not contain whitespace`,
		`a.json#12: match field "slug" has no value`,
		`a.json#13: match field "slug" must be a string, number or boolean`,
		`a.json#15: key "pages/dup" is already used by`,
		`a.json#17: writes the same document as`,
		`a.json#18: unknown key "collection2"`,
		`a.json#19: "collection" must be a string, not a scalar`,
		`a.json#20: an item must be a JSON object`,
		`b.json: not valid JSON`,
		`c.json: the file is empty`,
		`d.json: the array holds no items`,
		`e.json: holds more than one JSON value`,
		`f.json: a manifest file holds one item object or an array of them`,
		`missing.json: no such file or directory`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing problem %q in\n%s", want, joined)
		}
	}
}

func TestProblemsErr(t *testing.T) {
	if (Problems{}).Err() != nil {
		t.Error("no problems must be no error")
	}
	one := Problems{{Where: "a", Code: apierr.CodeRefUnresolved, Message: "m", Hint: "h"}}.Err()
	var e *apierr.Error
	if !errors.As(one, &e) || e.Code != apierr.CodeRefUnresolved || e.Hint != "h" || e.Message != "a: m" || e.Exit != 4 {
		t.Errorf("one = %+v", e)
	}
	mixed := Problems{{Where: "a", Code: apierr.CodeRefUnresolved, Message: "m"}, {Where: "b", Code: apierr.CodeFileMissing, Message: "n"}}.Err()
	if !errors.As(mixed, &e) || e.Code != apierr.CodeManifestInvalid || len(e.Failures) != 2 ||
		!strings.HasPrefix(e.Message, "2 manifest problems; the first: a: m") || !strings.Contains(e.Hint, "error.failures") {
		t.Errorf("mixed = %+v", e)
	}
	if _, err := Load(nil, nil); err == nil {
		t.Error("no paths must fail")
	}
	dir := t.TempDir()
	if _, err := Load([]string{dir}, nil); err == nil || !strings.Contains(err.Error(), "no *.json") {
		t.Errorf("empty dir: %v", err)
	}
}

func TestMatchValuesAndLookup(t *testing.T) {
	it := &Item{Collection: "c", Match: []string{"meta.title", "n"}, Data: map[string]any{"meta": map[string]any{"title": "T"}, "n": 3.0}}
	vals, fields, err := it.MatchValues()
	if err != nil || vals["meta.title"] != "T" || fmt.Sprint(fields) != "[meta.title n]" {
		t.Errorf("%v %v %v", vals, fields, err)
	}
	if _, ok := Lookup(map[string]any{"a": 1}, "a.b"); ok {
		t.Error("lookup through a scalar")
	}
	if ScalarString(3.0) != "3" || ScalarString("x") != "x" {
		t.Error("ScalarString")
	}
}
