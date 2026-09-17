package rows

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// decode builds a []any the way the pipeline does — through encoding/json with
// UseNumber — so the tests exercise the same value types production sees.
func decode(t *testing.T, s string) []any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v []any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return v
}

// layout is the shape of a real Payload blocks field: mongo-style string ids on
// three rows, two of which share a blockType.
const layout = `[
  {"id":"a1","blockType":"cta","blockName":"Top CTA","richText":{"root":{}}},
  {"id":"b2","blockType":"content","columns":[]},
  {"id":"c3","blockType":"cta","blockName":"Bottom CTA"},
  {"id":"d4","blockType":"mediaBlock","media":7}
]`

func ids(t *testing.T, list []any) []string {
	t.Helper()
	out := make([]string, len(list))
	for i, row := range list {
		obj, ok := row.(map[string]any)
		if !ok {
			t.Fatalf("row %d is not an object", i)
		}
		out[i], _ = obj["id"].(string)
	}
	return out
}

func TestParseSelector(t *testing.T) {
	two := 2
	cases := []struct {
		in   string
		want Selector
	}{
		{"3", Selector{Raw: "3", Kind: KindIndex, Index: 3}},
		{"-1", Selector{Raw: "-1", Kind: KindIndex, Index: -1}},
		{"first", Selector{Raw: "first", Kind: KindIndex, Index: 0}},
		{"last", Selector{Raw: "last", Kind: KindIndex, Index: -1}},
		{"LAST", Selector{Raw: "LAST", Kind: KindIndex, Index: -1}},
		{"id:67f3", Selector{Raw: "id:67f3", Kind: KindID, Value: "67f3"}},
		{"name:Hero CTA", Selector{Raw: "name:Hero CTA", Kind: KindName, Value: "Hero CTA"}},
		{"type:cta", Selector{Raw: "type:cta", Kind: KindType, Value: "cta"}},
		{"type:cta[2]", Selector{Raw: "type:cta[2]", Kind: KindType, Value: "cta", Nth: &two}},
		// A colon inside a value is content, not a second prefix: a blockName
		// of "Hero: the sequel" has to survive the round trip.
		{"name:Hero: the sequel", Selector{Raw: "name:Hero: the sequel", Kind: KindName, Value: "Hero: the sequel"}},
	}
	for _, tc := range cases {
		got, err := ParseSelector(tc.in)
		if err != nil {
			t.Errorf("ParseSelector(%q) errored: %v", tc.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseSelector(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestParseSelectorRejects(t *testing.T) {
	for _, in := range []string{
		"", "   ", "cta", "slug:cta", "type:", "id:", "name:",
		"type:cta[", "type:cta]", "type:cta[x]", "type:cta[-1]",
	} {
		if got, err := ParseSelector(in); err == nil {
			t.Errorf("ParseSelector(%q) = %+v, want an error", in, got)
		}
	}
}

func TestMatch(t *testing.T) {
	list := decode(t, layout)
	cases := []struct {
		sel  string
		want []int
	}{
		{"0", []int{0}},
		{"-1", []int{3}},
		{"last", []int{3}},
		{"9", nil},
		{"-9", nil},
		{"id:c3", []int{2}},
		{"id:nope", nil},
		{"name:Bottom CTA", []int{2}},
		{"name:bottom cta", nil}, // exact, never case-folded
		{"type:cta", []int{0, 2}},
		{"type:cta[1]", []int{2}},
		{"type:cta[2]", nil},
	}
	for _, tc := range cases {
		sel, err := ParseSelector(tc.sel)
		if err != nil {
			t.Fatalf("ParseSelector(%q): %v", tc.sel, err)
		}
		if got := sel.Match(list); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q matched %v, want %v", tc.sel, got, tc.want)
		}
	}
}

// A numeric id arrives as a json.Number on a Postgres project and as a string
// on a Mongo one. `id:42` has to find both, or the same selector would work
// against one project and silently match nothing against another.
func TestMatchIDAcrossIDTypes(t *testing.T) {
	list := decode(t, `[{"id":42,"blockType":"cta"},{"id":"42b","blockType":"cta"}]`)
	sel, _ := ParseSelector("id:42")
	if got := sel.Match(list); !reflect.DeepEqual(got, []int{0}) {
		t.Errorf("id:42 matched %v, want [0]", got)
	}
}

func TestRemove(t *testing.T) {
	list := decode(t, layout)
	got := Remove(list, []int{2, 0})
	if want := []string{"b2", "d4"}; !reflect.DeepEqual(ids(t, got), want) {
		t.Errorf("Remove = %v, want %v", ids(t, got), want)
	}
	if want := []string{"a1", "b2", "c3", "d4"}; !reflect.DeepEqual(ids(t, list), want) {
		t.Errorf("Remove mutated its input: %v", ids(t, list))
	}
}

// The anchor arithmetic is the whole point of the package, so every direction
// of "move X relative to Y" is pinned.
func TestMoveAnchors(t *testing.T) {
	cases := []struct {
		name   string
		src    string
		anchor string
		mode   Mode
		at     int
		want   []string
		wantAt int
	}{
		{name: "forwards, after", src: "id:a1", anchor: "id:c3", mode: ModeAfter,
			want: []string{"b2", "c3", "a1", "d4"}, wantAt: 2},
		{name: "forwards, before", src: "id:a1", anchor: "id:c3", mode: ModeBefore,
			want: []string{"b2", "a1", "c3", "d4"}, wantAt: 1},
		{name: "backwards, after", src: "id:d4", anchor: "id:a1", mode: ModeAfter,
			want: []string{"a1", "d4", "b2", "c3"}, wantAt: 1},
		{name: "backwards, before", src: "id:d4", anchor: "id:a1", mode: ModeBefore,
			want: []string{"d4", "a1", "b2", "c3"}, wantAt: 0},
		{name: "to an index", src: "id:a1", mode: ModeIndex, at: 2,
			want: []string{"b2", "c3", "a1", "d4"}, wantAt: 2},
		{name: "to the end with a negative index", src: "id:a1", mode: ModeIndex, at: -1,
			want: []string{"b2", "c3", "d4", "a1"}, wantAt: 3},
		{name: "append", src: "id:b2", mode: ModeAppend,
			want: []string{"a1", "c3", "d4", "b2"}, wantAt: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			list := decode(t, layout)
			srcSel, _ := ParseSelector(tc.src)
			src, err := ResolveOne(list, srcSel, "layout")
			if err != nil {
				t.Fatalf("resolve src: %v", err)
			}
			a := Anchor{Mode: tc.mode, Index: tc.at}
			if tc.anchor != "" {
				a.Sel, _ = ParseSelector(tc.anchor)
			}
			got, at, err := Move(list, src, a)
			if err != nil {
				t.Fatalf("Move: %v", err)
			}
			if !reflect.DeepEqual(ids(t, got), tc.want) {
				t.Errorf("Move = %v, want %v", ids(t, got), tc.want)
			}
			if at != tc.wantAt {
				t.Errorf("landed at %d, want %d", at, tc.wantAt)
			}
			if len(got) != len(list) {
				t.Errorf("Move changed the row count: %d -> %d", len(list), len(got))
			}
		})
	}
}

// "move it after itself" has no meaning and must not silently no-op: a no-op
// that reports success is how a pipeline ends up pushing an unchanged document.
func TestMoveRelativeToItself(t *testing.T) {
	list := decode(t, layout)
	sel, _ := ParseSelector("id:a1")
	anchor, _ := ParseSelector("id:a1")
	if _, _, err := Move(list, 0, Anchor{Mode: ModeAfter, Sel: anchor}); err == nil {
		t.Fatal("Move onto itself returned no error")
	}
	_ = sel
}

func TestInsert(t *testing.T) {
	list := decode(t, layout)
	row := map[string]any{"blockType": "cta"}

	got, at, err := Insert(list, row, Anchor{Mode: ModeAppend})
	if err != nil || at != 4 || len(got) != 5 {
		t.Fatalf("append: at=%d len=%d err=%v", at, len(got), err)
	}
	got, at, err = Insert(list, row, Anchor{Mode: ModeIndex, Index: 0})
	if err != nil || at != 0 || len(got) != 5 {
		t.Fatalf("at 0: at=%d len=%d err=%v", at, len(got), err)
	}
	// A 4-row array has 5 insert positions; index 4 is the append slot and is
	// legal, index 5 is not.
	if _, _, err := Insert(list, row, Anchor{Mode: ModeIndex, Index: 4}); err != nil {
		t.Errorf("insert at len should be legal: %v", err)
	}
	if _, _, err := Insert(list, row, Anchor{Mode: ModeIndex, Index: 5}); err == nil {
		t.Error("insert past len should fail")
	}
}

// A copied row that keeps its id is not a new block: Payload matches the id to
// the existing row and the array loses one entry instead of gaining one.
func TestCopyStripsIDsAtEveryDepth(t *testing.T) {
	list := decode(t, `[{"id":"a1","blockType":"cta","links":[{"id":"l1","url":"/x"}]}]`)
	got, at, err := Copy(list, 0, Anchor{Mode: ModeAppend})
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if at != 1 || len(got) != 2 {
		t.Fatalf("Copy landed at %d with %d rows", at, len(got))
	}
	dup := got[1].(map[string]any)
	if _, ok := dup["id"]; ok {
		t.Error("the copy kept its top-level id")
	}
	nested := dup["links"].([]any)[0].(map[string]any)
	if _, ok := nested["id"]; ok {
		t.Error("the copy kept a nested row id")
	}
	// The original is untouched.
	if orig := list[0].(map[string]any); orig["id"] != "a1" {
		t.Error("Copy mutated the source row")
	}
}

func TestSetAndUnset(t *testing.T) {
	list := decode(t, layout)
	got, err := Set(list, 0, map[string]any{"blockName": "Renamed", "links.0.url": "/pricing"}, []string{"richText"})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	row := got[0].(map[string]any)
	if row["blockName"] != "Renamed" {
		t.Errorf("blockName = %v", row["blockName"])
	}
	if _, ok := row["richText"]; ok {
		t.Error("--unset richText left the key behind")
	}
	if links, ok := row["links"].(map[string]any); !ok || links["0"].(map[string]any)["url"] != "/pricing" {
		t.Errorf("dotted set produced %#v", row["links"])
	}
	if orig := list[0].(map[string]any); orig["blockName"] != "Top CTA" {
		t.Error("Set mutated the source row")
	}
}

func TestResolveOneErrors(t *testing.T) {
	list := decode(t, layout)

	sel, _ := ParseSelector("type:cta")
	_, err := ResolveOne(list, sel, "layout")
	var amb *AmbiguousError
	if !asErr(err, &amb) {
		t.Fatalf("type:cta on two rows returned %v, want AmbiguousError", err)
	}
	if !reflect.DeepEqual(amb.Matched, []int{0, 2}) {
		t.Errorf("matched %v, want [0 2]", amb.Matched)
	}
	if len(amb.Have) != 4 {
		t.Errorf("the error carries %d summaries, want 4", len(amb.Have))
	}

	sel, _ = ParseSelector("id:zz")
	_, err = ResolveOne(list, sel, "layout")
	var miss *NoMatchError
	if !asErr(err, &miss) {
		t.Fatalf("id:zz returned %v, want NoMatchError", err)
	}
	if !strings.Contains(miss.Error(), "layout") {
		t.Errorf("the message does not name the field: %s", miss.Error())
	}
}

func asErr[T error](err error, target *T) bool {
	t, ok := err.(T)
	if ok {
		*target = t
	}
	return ok
}

// The emitted selector has to survive the next edit in the pipe, which an index
// does not. id wins, then a unique blockType, then a subscripted one.
func TestSummarizeSelectors(t *testing.T) {
	got := Summarize(decode(t, layout))
	want := []string{"id:a1", "id:b2", "id:c3", "id:d4"}
	for i, s := range got {
		if s.Selector != want[i] {
			t.Errorf("row %d selector = %q, want %q", i, s.Selector, want[i])
		}
	}

	// Without ids — a document built locally and not yet written — the
	// blockType has to carry it, subscripted when it repeats.
	got = Summarize(decode(t, `[{"blockType":"cta"},{"blockType":"content"},{"blockType":"cta"}]`))
	want = []string{"type:cta[0]", "type:content", "type:cta[1]"}
	for i, s := range got {
		if s.Selector != want[i] {
			t.Errorf("row %d selector = %q, want %q", i, s.Selector, want[i])
		}
	}
}

func TestSummarizeFieldsExcludePlumbing(t *testing.T) {
	got := Summarize(decode(t, layout))
	if !reflect.DeepEqual(got[0].Fields, []string{"richText"}) {
		t.Errorf("fields = %v, want [richText]", got[0].Fields)
	}
	if got[2].Fields != nil {
		t.Errorf("a row with only plumbing reported fields %v", got[2].Fields)
	}
}

func TestLooksLikeBlocks(t *testing.T) {
	cases := []struct {
		json string
		want bool
	}{
		{layout, true},
		{`[]`, false},                                  // nothing to read it from
		{`[{"title":"a"},{"title":"b"}]`, false},       // a plain array field
		{`[{"blockType":"cta"},{"title":"x"}]`, false}, // mixed is not blocks
		{`["a","b"]`, false},                           // an array of scalars
	}
	for _, tc := range cases {
		if got := LooksLikeBlocks(decode(t, tc.json)); got != tc.want {
			t.Errorf("LooksLikeBlocks(%s) = %v, want %v", tc.json, got, tc.want)
		}
	}
}

func TestTypes(t *testing.T) {
	if got := Types(decode(t, layout)); !reflect.DeepEqual(got, []string{"content", "cta", "mediaBlock"}) {
		t.Errorf("Types = %v", got)
	}
}
