package docdiff

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// decode parses a JSON literal the way PayCLI decodes documents: UseNumber,
// so every number is a json.Number.
func decode(t *testing.T, s string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("fixture: %v\n%s", err, s)
	}
	return v
}

func obj(t *testing.T, s string) map[string]any {
	t.Helper()
	m, ok := decode(t, s).(map[string]any)
	if !ok {
		t.Fatalf("fixture is not an object: %s", s)
	}
	return m
}

// richText builds a Payload lexical editor state with one paragraph per text.
func richText(paragraphs ...string) string {
	var ps []string
	for _, p := range paragraphs {
		ps = append(ps, `{"type":"paragraph","version":1,"direction":"ltr","format":"","indent":0,"textFormat":0,
		  "children":[{"type":"text","version":1,"detail":0,"format":0,"mode":"normal","style":"","text":`+quote(p)+`}]}`)
	}
	return `{"root":{"type":"root","version":1,"direction":"ltr","format":"","indent":0,"children":[` + strings.Join(ps, ",") + `]}}`
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func paths(res Result) []string {
	out := make([]string, 0, len(res.Changes))
	for _, c := range res.Changes {
		out = append(out, string(c.Op)+" "+c.Path)
	}
	return out
}

func wantPaths(t *testing.T, res Result, want ...string) {
	t.Helper()
	got := paths(res)
	if want == nil {
		want = []string{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestDiffScalarsAndKeys(t *testing.T) {
	l := obj(t, `{"id":2,"title":"Old","slug":"s","gone":"x","meta":{"title":"a","description":"d"},"n":1}`)
	r := obj(t, `{"id":2,"title":"New","slug":"s","added":true,"meta":{"title":"b","description":"d"},"n":1.0}`)
	res := Diff(l, r, Options{})
	wantPaths(t, res, "add added", "remove gone", "change meta.title", "change title")
	if res.Identical {
		t.Fatal("identical must be false")
	}
	s := res.Summary
	if s.Total != 4 || s.Added != 1 || s.Removed != 1 || s.Changed != 2 || s.Moved != 0 {
		t.Fatalf("summary = %+v", s)
	}
	if strings.Join(s.Fields, ",") != "added,gone,meta,title" {
		t.Fatalf("fields = %v", s.Fields)
	}
	c := res.Changes[3]
	if c.From != "Old" || c.To != "New" {
		t.Fatalf("title change = %+v", c)
	}
}

// 1 and 1.0 are the same JSON number; a float64 from a plain decoder equals a
// json.Number from a UseNumber one.
func TestNumbersCompareByValue(t *testing.T) {
	if !Equal(json.Number("1"), json.Number("1.0")) || !Equal(json.Number("12"), float64(12)) {
		t.Fatal("numbers must compare by value")
	}
	if Equal(json.Number("1"), "1") {
		t.Fatal("a number is not a string")
	}
}

func TestTimestampsAreIgnoredByDefault(t *testing.T) {
	l := obj(t, `{"updatedAt":"2026-01-01","createdAt":"x","layout":[{"id":"a","updatedAt":"1"}]}`)
	r := obj(t, `{"updatedAt":"2026-02-02","createdAt":"x","layout":[{"id":"a","updatedAt":"2"}]}`)
	wantPaths(t, Diff(l, r, Options{}))
	wantPaths(t, Diff(l, r, Options{IncludeTimestamps: true}), "change layout[0].updatedAt", "change updatedAt")
}

func TestNullEqualsAbsentUnlessDistinct(t *testing.T) {
	l := obj(t, `{"a":null,"b":"x"}`)
	r := obj(t, `{"b":"x","c":null}`)
	wantPaths(t, Diff(l, r, Options{}))
	wantPaths(t, Diff(l, r, Options{NullDistinct: true}), "remove a", "add c")
	// A real value to null is always a change; the op follows key presence.
	wantPaths(t, Diff(obj(t, `{"a":"x"}`), obj(t, `{"a":null}`), Options{}), "change a")
	wantPaths(t, Diff(obj(t, `{"a":null}`), obj(t, `{"a":"x"}`), Options{}), "change a")
	wantPaths(t, Diff(obj(t, `{"a":"x"}`), obj(t, `{}`), Options{}), "remove a")
	wantPaths(t, Diff(obj(t, `{}`), obj(t, `{"a":"x"}`), Options{}), "add a")
}

func TestIgnorePathsAndKeys(t *testing.T) {
	l := obj(t, `{"meta":{"image":1,"title":"a"},"layout":[{"id":"a","blockName":"x","t":1},{"id":"b","blockName":"y","t":2}]}`)
	r := obj(t, `{"meta":{"image":2,"title":"b"},"layout":[{"id":"a","blockName":"z","t":1},{"id":"b","blockName":"w","t":3}]}`)
	res := Diff(l, r, Options{IgnorePaths: []string{"meta.image", "layout[].blockName"}})
	wantPaths(t, res, "change layout[1].t", "change meta.title")
	res = Diff(l, r, Options{IgnorePaths: []string{"layout[1]"}, IgnoreKeys: []string{"title"}})
	wantPaths(t, res, "change layout[0].blockName", "change meta.image")
	// A pattern matches its subtree but not a sibling that shares a prefix.
	res = Diff(obj(t, `{"meta":1,"metaX":1}`), obj(t, `{"meta":2,"metaX":2}`), Options{IgnorePaths: []string{"meta"}})
	wantPaths(t, res, "change metaX")
}

// The core claim: an edit inside a row is reported ONCE, at the row's path,
// no matter how deep it is.
func TestNestedBlockEditIsOneChange(t *testing.T) {
	l := obj(t, `{"layout":[
	  {"id":"h","blockType":"hero","title":"T"},
	  {"id":"g","blockType":"group","blocks":[
	    {"id":"s1","blockType":"processStep","title":"One","image":{"file":4,"alt":""}},
	    {"id":"s2","blockType":"processStep","title":"Two"}]}]}`)
	r := obj(t, `{"layout":[
	  {"id":"h","blockType":"hero","title":"T"},
	  {"id":"g","blockType":"group","blocks":[
	    {"id":"s1","blockType":"processStep","title":"One","image":{"file":4,"alt":"Neu"}},
	    {"id":"s2","blockType":"processStep","title":"Two"}]}]}`)
	res := Diff(l, r, Options{})
	wantPaths(t, res, "change layout[1].blocks[0].image.alt")
	if res.Changes[0].From != "" || res.Changes[0].To != "Neu" {
		t.Fatalf("change = %+v", res.Changes[0])
	}
}

func TestRowsMatchedByIDReportMoves(t *testing.T) {
	l := obj(t, `{"layout":[{"id":"a","blockType":"hero"},{"id":"b","blockType":"text","x":1},{"id":"c","blockType":"cta"}]}`)
	r := obj(t, `{"layout":[{"id":"c","blockType":"cta"},{"id":"a","blockType":"hero"},{"id":"b","blockType":"text","x":2}]}`)
	res := Diff(l, r, Options{})
	wantPaths(t, res, "move layout[0]", "change layout[2].x")
	mv := res.Changes[0]
	if mv.Kind != KindRow || *mv.FromIndex != 2 || *mv.ToIndex != 0 || mv.ID != "c" || mv.BlockType != "cta" {
		t.Fatalf("move = %+v", mv)
	}
	if res.Summary.Moved != 1 {
		t.Fatalf("summary = %+v", res.Summary)
	}
}

// Inserting a row shifts every row below it; only the insert is a change.
func TestInsertIsOneAddNotAShift(t *testing.T) {
	l := obj(t, `{"layout":[{"id":"a","blockType":"hero"},{"id":"b","blockType":"text"},{"id":"c","blockType":"cta"}]}`)
	r := obj(t, `{"layout":[{"id":"a","blockType":"hero"},{"blockType":"image","media":7},{"id":"b","blockType":"text"},{"id":"c","blockType":"cta"}]}`)
	res := Diff(l, r, Options{})
	wantPaths(t, res, "add layout[1]")
	if c := res.Changes[0]; c.Kind != KindRow || c.BlockType != "image" {
		t.Fatalf("add = %+v", c)
	}
	// Removal addresses the LEFT document's index.
	res = Diff(r, l, Options{})
	wantPaths(t, res, "remove layout[1]")
}

// Without shared ids (two different documents, or fresh rows with no ids),
// rows pair up by position and their generated ids are not reported.
func TestIndexModeIgnoresRowIDs(t *testing.T) {
	l := obj(t, `{"layout":[{"id":"a1","blockType":"hero","title":"x"},{"id":"a2","blockType":"text"}]}`)
	r := obj(t, `{"layout":[{"id":"b1","blockType":"hero","title":"y"}]}`)
	wantPaths(t, Diff(l, r, Options{}), "change layout[0].title", "remove layout[1]")
	fresh := obj(t, `{"layout":[{"blockType":"hero","title":"x"},{"blockType":"text"},{"blockType":"cta"}]}`)
	wantPaths(t, Diff(l, fresh, Options{}), "add layout[2]")
}

func TestBlockTypeSwapIsOneRowChange(t *testing.T) {
	l := obj(t, `{"layout":[{"id":"a","blockType":"group","blocks":[{"id":"n","blockType":"bento","items":[1]}]}]}`)
	r := obj(t, `{"layout":[{"id":"a","blockType":"group","blocks":[{"id":"n","blockType":"grid","columns":3}]}]}`)
	res := Diff(l, r, Options{})
	wantPaths(t, res, "change layout[0].blocks[0]")
	if c := res.Changes[0]; c.Kind != KindRow || c.BlockType != "grid" {
		t.Fatalf("change = %+v", c)
	}
}

func TestScalarListsAreOneValue(t *testing.T) {
	res := Diff(obj(t, `{"tags":[1,2,3]}`), obj(t, `{"tags":[1,3]}`), Options{})
	wantPaths(t, res, "change tags")
}

func TestDuplicateRowIDsFallBackToPosition(t *testing.T) {
	l := obj(t, `{"a":[{"id":"x","v":1},{"id":"x","v":2}]}`)
	r := obj(t, `{"a":[{"id":"x","v":1},{"id":"x","v":3}]}`)
	wantPaths(t, Diff(l, r, Options{}), "change a[1].v")
}

func TestTypeMismatchIsAChange(t *testing.T) {
	wantPaths(t, Diff(obj(t, `{"a":{"b":1},"c":[1],"d":"x"}`), obj(t, `{"a":[1],"c":"x","d":{"e":1}}`), Options{}),
		"change a", "change c", "change d")
}

func TestLeafPathsCompareWholesale(t *testing.T) {
	l := obj(t, `{"settings":{"a":1,"b":2}}`)
	r := obj(t, `{"settings":{"a":1,"b":3}}`)
	wantPaths(t, Diff(l, r, Options{}), "change settings.b")
	wantPaths(t, Diff(l, r, Options{LeafPaths: []string{"settings"}}), "change settings")
}

func TestRichTextIsReportedAsText(t *testing.T) {
	l := obj(t, `{"layout":[{"id":"a","blockType":"text","content":`+richText("Hallo Welt", "Zweiter Absatz")+`}]}`)
	r := obj(t, `{"layout":[{"id":"a","blockType":"text","content":`+richText("Hallo Payload", "Zweiter Absatz")+`}]}`)
	res := Diff(l, r, Options{})
	wantPaths(t, res, "change layout[0].content")
	c := res.Changes[0]
	if c.Kind != KindRichText || c.FromText == nil || c.ToText == nil {
		t.Fatalf("change = %+v", c)
	}
	if *c.FromText != "Hallo Welt\n\nZweiter Absatz" || *c.ToText != "Hallo Payload\n\nZweiter Absatz" {
		t.Fatalf("texts = %q / %q", *c.FromText, *c.ToText)
	}
	if res.Summary.RichText != 1 {
		t.Fatalf("summary = %+v", res.Summary)
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"from":`) || strings.Contains(string(b), `"root"`) {
		t.Fatalf("an abbreviated rich-text change must not carry the editor state: %s", b)
	}

	// --full keeps the states.
	full := Diff(l, r, Options{Full: true})
	b, _ = json.Marshal(full.Changes[0])
	if !strings.Contains(string(b), `"from":{"root"`) || !strings.Contains(string(b), `"from_text"`) {
		t.Fatalf("full change = %s", b)
	}
}

func TestRichTextFormattingOnly(t *testing.T) {
	l := obj(t, `{"c":`+richText("Hallo")+`}`)
	bold := strings.Replace(richText("Hallo"), `"format":0`, `"format":1`, 1)
	res := Diff(l, obj(t, `{"c":`+bold+`}`), Options{})
	wantPaths(t, res, "change c")
	if res.Changes[0].Detail != "formatting" {
		t.Fatalf("detail = %q", res.Changes[0].Detail)
	}
	// Structurally identical states are no change at all.
	wantPaths(t, Diff(l, obj(t, `{"c":`+richText("Hallo")+`}`), Options{}))
}

func TestRichTextAddedAndReplacedByAString(t *testing.T) {
	res := Diff(obj(t, `{}`), obj(t, `{"c":`+richText("Neu")+`}`), Options{})
	wantPaths(t, res, "add c")
	c := res.Changes[0]
	b, _ := json.Marshal(c)
	if c.Kind != KindRichText || *c.ToText != "Neu" || strings.Contains(string(b), `"to":`) {
		t.Fatalf("add = %s", b)
	}
	// null -> state: the key exists on both sides, so it is a change from "".
	res = Diff(obj(t, `{"c":null}`), obj(t, `{"c":`+richText("Neu")+`}`), Options{})
	b, _ = json.Marshal(res.Changes[0])
	if string(b) != `{"op":"change","path":"c","kind":"richtext","from_text":"","to_text":"Neu"}` {
		t.Fatalf("null -> state = %s", b)
	}

	res = Diff(obj(t, `{"c":`+richText("Alt")+`}`), obj(t, `{"c":"plain"}`), Options{})
	b, _ = json.Marshal(res.Changes[0])
	if !strings.Contains(string(b), `"from":{"$richtext":"Alt"}`) || !strings.Contains(string(b), `"to":"plain"`) {
		t.Fatalf("mixed change = %s", b)
	}
}

// An added row carrying rich text shows the text, not the editor tree.
func TestAddedRowsAbbreviateNestedRichText(t *testing.T) {
	l := obj(t, `{"layout":[{"id":"a","blockType":"hero"}]}`)
	r := obj(t, `{"layout":[{"id":"a","blockType":"hero"},{"blockType":"text","content":`+richText("Neu hier")+`,"createdAt":"x"}]}`)
	res := Diff(l, r, Options{})
	wantPaths(t, res, "add layout[1]")
	b, _ := json.Marshal(res.Changes[0])
	if !strings.Contains(string(b), `"content":{"$richtext":"Neu hier"}`) {
		t.Fatalf("added row = %s", b)
	}
	if strings.Contains(string(b), "createdAt") {
		t.Fatalf("ignored keys must not appear inside a reported value: %s", b)
	}
	full := Diff(l, r, Options{Full: true})
	b, _ = json.Marshal(full.Changes[0])
	if !strings.Contains(string(b), `"root"`) {
		t.Fatalf("--full must keep the editor state: %s", b)
	}
}

func TestChangeJSONShape(t *testing.T) {
	res := Diff(obj(t, `{"a":null,"b":1,"c":2}`), obj(t, `{"a":"x","b":null,"c":3}`), Options{NullDistinct: true})
	var got []string
	for _, c := range res.Changes {
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, string(b))
	}
	want := []string{
		`{"op":"change","path":"a","from":null,"to":"x"}`,
		`{"op":"change","path":"b","from":1,"to":null}`,
		`{"op":"change","path":"c","from":2,"to":3}`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("json =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	add := Diff(obj(t, `{}`), obj(t, `{"a":1}`), Options{}).Changes[0]
	if b, _ := json.Marshal(add); string(b) != `{"op":"add","path":"a","to":1}` {
		t.Fatalf("add = %s", b)
	}
	rm := Diff(obj(t, `{"a":1}`), obj(t, `{}`), Options{}).Changes[0]
	if b, _ := json.Marshal(rm); string(b) != `{"op":"remove","path":"a","from":1}` {
		t.Fatalf("remove = %s", b)
	}
}

func TestIdenticalAndEmpty(t *testing.T) {
	res := Diff(obj(t, `{"a":[{"id":"x","b":{"c":[1,2]}}]}`), obj(t, `{"a":[{"id":"x","b":{"c":[1,2]}}]}`), Options{})
	if !res.Identical || res.Changes == nil || len(res.Changes) != 0 || res.Summary.Fields == nil {
		t.Fatalf("result = %+v", res)
	}
	b, _ := json.Marshal(res)
	if string(b) != `{"identical":true,"summary":{"total":0,"added":0,"removed":0,"changed":0,"moved":0,"rich_text":0,"fields":[]},"changes":[]}` {
		t.Fatalf("json = %s", b)
	}
	if !Diff(nil, nil, Options{}).Identical {
		t.Fatal("nil vs nil")
	}
}

func TestDiffDoesNotMutateItsInputs(t *testing.T) {
	src := `{"layout":[{"id":"a","blockType":"text","content":` + richText("x") + `,"createdAt":"1"}],"t":"x"}`
	l := obj(t, src)
	r := obj(t, `{"layout":[],"t":"y"}`)
	before, _ := json.Marshal(l)
	res := Diff(l, r, Options{})
	// Mutating a reported value must not reach the input.
	if from, ok := res.Changes[0].From.(map[string]any); ok {
		from["blockType"] = "mutated"
	}
	after, _ := json.Marshal(l)
	if string(before) != string(after) {
		t.Fatal("Diff mutated its input")
	}
}

func TestLongestIncreasing(t *testing.T) {
	cases := []struct {
		seq  []int
		kept int
	}{
		{nil, 0}, {[]int{0, 1, 2}, 3}, {[]int{2, 0, 1}, 2}, {[]int{3, 2, 1, 0}, 1}, {[]int{1, 0, 3, 2, 4}, 3},
	}
	for _, tc := range cases {
		marks := longestIncreasing(tc.seq)
		n, last := 0, -1
		for i, m := range marks {
			if !m {
				continue
			}
			if tc.seq[i] <= last {
				t.Fatalf("%v: marked subsequence is not increasing: %v", tc.seq, marks)
			}
			last = tc.seq[i]
			n++
		}
		if n != tc.kept {
			t.Errorf("%v: kept %d, want %d", tc.seq, n, tc.kept)
		}
	}
}

func TestPathHelpers(t *testing.T) {
	if JoinKey("", "a") != "a" || JoinKey("a", "b") != "a.b" || JoinIndex("a", 2) != "a[2]" {
		t.Fatal("join")
	}
	for in, want := range map[string]string{"layout[2].title": "layout", "meta.title": "meta", "slug": "slug", "": ""} {
		if got := RootField(in); got != want {
			t.Errorf("RootField(%q) = %q", in, got)
		}
	}
	ps := compilePatterns([]string{"layout[].blocks[*].title", ".meta", "a[3]", "  "})
	for path, want := range map[string]bool{
		"layout[0].blocks[12].title":     true,
		"layout[0].blocks[12].title.sub": true,
		"layout[0].blocks[12].titles":    false,
		"meta":                           true,
		"meta.image":                     true,
		"metadata":                       false,
		"a[3]":                           true,
		"a[3].b":                         true,
		"a[30]":                          false,
	} {
		if got := matchAny(ps, path); got != want {
			t.Errorf("match(%q) = %v, want %v", path, got, want)
		}
	}
}
