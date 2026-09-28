package rows

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// page is the shape of a real klixpert page (pages 2 and 47 on stage, read
// with --draft --depth 0): a `group` row whose own `blocks` hold processStep
// rows, a second group holding a `grid` whose `blocks` hold text rows with a
// `buttons` array each, a lexical `content` value whose `children` look
// exactly like rows, and a text row with an empty `buttons` array.
const page = `{"id":2,"title":"Prozess","layout":[
  {"id":"h0","blockType":"hero","buttons":[],"image":7},
  {"id":"t1","blockType":"text","heading":null,"buttons":[],
   "section":{"backgroundColor":null,"contentWidth":"inhalt"},
   "content":{"root":{"type":"root","children":[{"type":"paragraph","children":[{"type":"text","text":"Hi"}]}]}}},
  {"id":"g2","blockType":"group","blocks":[
     {"id":"s0","blockType":"processStep","phase":"Plan"},
     {"id":"s1","blockType":"processStep","phase":"Build"},
     {"id":"s2","blockType":"processStep","phase":"Run"}]},
  {"id":"g3","blockType":"group","blocks":[
     {"id":"x0","blockType":"text","buttons":[]},
     {"id":"r1","blockType":"grid","columnsDesktop":3,"blocks":[
        {"id":"c0","blockType":"text","buttons":[{"id":"b0","label":"Go","url":"/go"}]},
        {"id":"c1","blockType":"text","buttons":[]}]}]},
  {"blockType":"logos","logos":[{"logo":3},{"logo":4}],"media":{"id":9,"createdAt":"x","sizes":[{"w":1}]}}
]}`

func decodeDoc(t *testing.T, s string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return v
}

func mustPath(t *testing.T, s string) Path {
	t.Helper()
	p, err := ParsePath(s)
	if err != nil {
		t.Fatalf("ParsePath(%q): %v", s, err)
	}
	return p
}

func TestParsePath(t *testing.T) {
	cases := []struct {
		in   string
		want []string // Step.String() of each step
	}{
		{"layout", []string{"layout"}},
		{"hero.items", []string{"hero", "items"}},
		{"layout.2.blocks", []string{"layout", "2", "blocks"}},
		{"layout[2].blocks", []string{"layout", "[2]", "blocks"}},
		{"layout[-1]", []string{"layout", "[-1]"}},
		{"layout[id:6ab8ed4d].blocks", []string{"layout", "[id:6ab8ed4d]", "blocks"}},
		{"layout[type:group[1]].blocks", []string{"layout", "[type:group[1]]", "blocks"}},
		{"layout[name:A.B].blocks", []string{"layout", "[name:A.B]", "blocks"}},
		{"layout[2][0]", []string{"layout", "[2]", "[0]"}},
		{"layout[first].blocks[last].buttons", []string{"layout", "[first]", "blocks", "[last]", "buttons"}},
		{"  layout[2].blocks  ", []string{"layout", "[2]", "blocks"}},
	}
	for _, c := range cases {
		p, err := ParsePath(c.in)
		if err != nil {
			t.Errorf("ParsePath(%q): %v", c.in, err)
			continue
		}
		got := make([]string, len(p.Steps))
		for i, s := range p.Steps {
			got[i] = s.String()
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParsePath(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParsePathRejects(t *testing.T) {
	for _, in := range []string{
		"", ".layout", "layout.", "layout..blocks", "[2].blocks", "layout[", "layout[]",
		"layout[2]x", "layout]", "layout.[2]", "layout[type:group[1]", "layout[bogus:1]",
		"layout[type:cta[x]]",
	} {
		if _, err := ParsePath(in); err == nil {
			t.Errorf("ParsePath(%q) succeeded; it must be rejected", in)
		}
	}
}

func TestResolveEveryFormReachesTheSameArray(t *testing.T) {
	doc := decodeDoc(t, page)
	for _, in := range []string{
		"layout.2.blocks", "layout[2].blocks", "layout[id:g2].blocks",
		"layout[type:group[0]].blocks", "layout.id:g2.blocks", "layout[-3].blocks",
	} {
		r, err := Resolve(doc, mustPath(t, in))
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		list, ok := r.Value.([]any)
		if !ok || len(list) != 3 {
			t.Errorf("%s resolved to %#v", in, r.Value)
		}
		if r.At.String() != "layout[2].blocks" {
			t.Errorf("%s: String() = %q", in, r.At.String())
		}
		if r.At.Stable() != "layout[id:g2].blocks" {
			t.Errorf("%s: Stable() = %q", in, r.At.Stable())
		}
		if r.At.Pattern() != "layout[].blocks" || r.At.Root() != "layout" || !r.At.Nested() {
			t.Errorf("%s: Pattern/Root/Nested = %q %q %v", in, r.At.Pattern(), r.At.Root(), r.At.Nested())
		}
	}

	r, err := Resolve(doc, mustPath(t, "layout[id:g3].blocks[type:grid].blocks[0].buttons"))
	if err != nil {
		t.Fatal(err)
	}
	if r.At.String() != "layout[3].blocks[1].blocks[0].buttons" {
		t.Errorf("String() = %q", r.At.String())
	}
	if r.At.Stable() != "layout[id:g3].blocks[id:r1].blocks[id:c0].buttons" {
		t.Errorf("Stable() = %q", r.At.Stable())
	}
	row, rel, ok := r.At.Parent()
	if !ok || rel != "buttons" || row.Last().BlockType != "text" || row.String() != "layout[3].blocks[1].blocks[0]" {
		t.Errorf("Parent() = %q %q %v", row.String(), rel, ok)
	}

	// A top-level field keeps its old, flat identity.
	r, err = Resolve(doc, mustPath(t, "layout"))
	if err != nil {
		t.Fatal(err)
	}
	if r.At.String() != "layout" || r.At.Stable() != "layout" || r.At.Root() != "layout" || r.At.Nested() {
		t.Errorf("top-level location = %+v", r.At)
	}
}

func TestResolveFailuresAreTyped(t *testing.T) {
	doc := decodeDoc(t, page)

	// A parent row that does not exist is a not-found on THAT array.
	_, err := Resolve(doc, mustPath(t, "layout.9.blocks"))
	var miss *NoMatchError
	if !asErr(err, &miss) || miss.Field != "layout" || len(miss.Have) != 5 {
		t.Errorf("layout.9.blocks = %v", err)
	}
	_, err = Resolve(doc, mustPath(t, "layout[2].blocks[id:zz]"))
	if !asErr(err, &miss) || miss.Field != "layout[2].blocks" {
		t.Errorf("nested miss = %v", err)
	}

	var amb *AmbiguousError
	if _, err = Resolve(doc, mustPath(t, "layout[type:group].blocks")); !asErr(err, &amb) {
		t.Errorf("type:group on two groups = %v, want AmbiguousError", err)
	}

	var pe *PathError
	cases := []struct {
		path  string
		kind  PathErrorKind
		inRow bool
		null  bool
		at    string
	}{
		{"layout[2].blokcs", PathAbsent, true, false, "layout[2].blokcs"},
		{"layout[1].heading.x", PathAbsent, true, true, "layout[1].heading"},
		{"layuot.2.blocks", PathAbsent, false, false, "layuot"},
		{"layout.blocks", PathKeyOnArray, false, false, "layout"},
		{"title[0]", PathNotContainer, false, false, "title"},
		{"layout[2][0]", PathRowOnObject, true, false, "layout[2]"},
		{"layout[0].image.x", PathNotContainer, true, false, "layout[0].image"},
	}
	for _, c := range cases {
		_, err := Resolve(doc, mustPath(t, c.path))
		if !asErr(err, &pe) {
			t.Errorf("%s: %v, want PathError", c.path, err)
			continue
		}
		if pe.Kind != c.kind || pe.InRow != c.inRow || pe.Null != c.null || pe.At.String() != c.at {
			t.Errorf("%s: kind=%s inRow=%v null=%v at=%q", c.path, pe.Kind, pe.InRow, pe.Null, pe.At.String())
		}
	}

	// The absent-parent case knows the next step can only be a row.
	_, err = Resolve(doc, mustPath(t, "layuot.2.blocks"))
	if asErr(err, &pe) && !pe.NextLooksLikeRow() {
		t.Error("layuot.2 must be recognised as addressing a row")
	}
	_, err = Resolve(doc, mustPath(t, "hero.items"))
	if asErr(err, &pe) && (!pe.RestAreKeys() || pe.NextLooksLikeRow()) {
		t.Error("hero.items is a plain group path")
	}
}

func TestNotArrayErrorNamesTheArraysInsideARow(t *testing.T) {
	doc := decodeDoc(t, page)
	p := mustPath(t, "layout[3]")
	r, err := Resolve(doc, p)
	if err != nil {
		t.Fatal(err)
	}
	e := NotArrayError(p, r)
	if !reflect.DeepEqual(e.Have, []string{"blocks"}) {
		t.Errorf("Have = %v", e.Have)
	}
	if !strings.Contains(e.Error(), "a group row") {
		t.Errorf("message = %s", e.Error())
	}
}

func TestReplaceIsCopyOnWrite(t *testing.T) {
	doc := decodeDoc(t, page)
	before, _ := json.Marshal(doc)

	r, err := Resolve(doc, mustPath(t, "layout[id:g3].blocks[id:r1].blocks"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Replace(doc, r.At, []any{map[string]any{"blockType": "text"}})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(doc)
	if string(before) != string(after) {
		t.Fatal("Replace mutated its input")
	}
	grid := out["layout"].([]any)[3].(map[string]any)["blocks"].([]any)[1].(map[string]any)
	if n := len(grid["blocks"].([]any)); n != 1 {
		t.Errorf("grid has %d rows after Replace", n)
	}
	// Everything beside the edited subtree is the same value.
	if !reflect.DeepEqual(out["layout"].([]any)[2], doc["layout"].([]any)[2]) {
		t.Error("a sibling row changed")
	}

	// An absent object hop is created, which is what fills a null layout.
	fresh, err := Replace(map[string]any{"id": 1}, KeyLocation("hero.items"), []any{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fresh["hero"].(map[string]any)["items"]; !ok {
		t.Errorf("Replace did not create hero.items: %v", fresh)
	}
}

func TestTreeListsEveryDepthWithAPasteableField(t *testing.T) {
	doc := decodeDoc(t, page)
	nodes := Tree(doc["layout"].([]any), KeyLocation("layout"))

	var got []string
	for _, n := range nodes {
		got = append(got, strings.Repeat(" ", n.Depth)+n.Path+" "+n.Selector)
	}
	want := []string{
		"layout[0] id:h0",
		"layout[1] id:t1",
		"layout[2] id:g2",
		" layout[2].blocks[0] id:s0",
		" layout[2].blocks[1] id:s1",
		" layout[2].blocks[2] id:s2",
		"layout[3] id:g3",
		" layout[3].blocks[0] id:x0",
		" layout[3].blocks[1] id:r1",
		"  layout[3].blocks[1].blocks[0] id:c0",
		"   layout[3].blocks[1].blocks[0].buttons[0] id:b0",
		"  layout[3].blocks[1].blocks[1] id:c1",
		"layout[4] type:logos",
		" layout[4].logos[0] 0",
		" layout[4].logos[1] 1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tree =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// Every node's field resolves, and its selector finds the node's row in it.
	for _, n := range nodes {
		r, err := Resolve(doc, mustPath(t, n.Field))
		if err != nil {
			t.Errorf("%s: its field %q does not resolve: %v", n.Path, n.Field, err)
			continue
		}
		sel, _ := ParseSelector(n.Selector)
		if idx, err := ResolveOne(r.Value.([]any), sel, n.Field); err != nil || idx != n.Index {
			t.Errorf("%s: selector %q in %q = %d, %v", n.Path, n.Selector, n.Field, idx, err)
		}
	}

	byPath := map[string]Node{}
	for _, n := range nodes {
		byPath[n.Path] = n
	}
	// Empty arrays are listed (they are where `add` goes); rich text and an
	// expanded relationship are not walked.
	if got := byPath["layout[1]"].Nested; !reflect.DeepEqual(got, map[string]int{"buttons": 0}) {
		t.Errorf("text row nested = %v; lexical children must not count, empty buttons must", got)
	}
	if got := byPath["layout[4]"].Nested; !reflect.DeepEqual(got, map[string]int{"logos": 2}) {
		t.Errorf("logos row nested = %v; an expanded upload's sizes are not rows of this document", got)
	}
	if got := byPath["layout[3].blocks[1]"].Field; got != "layout[id:g3].blocks" {
		t.Errorf("grid field = %q", got)
	}
}

func TestObservedArrayField(t *testing.T) {
	doc := decodeDoc(t, page)
	if !ObservedArrayField(doc, "group", "blocks") {
		t.Error("groups carry blocks")
	}
	if !ObservedArrayField(doc, "text", "buttons") {
		t.Error("text rows carry buttons")
	}
	if ObservedArrayField(doc, "processStep", "buttons") {
		t.Error("no processStep carries buttons")
	}
	if ObservedArrayField(doc, "text", "section") {
		t.Error("section is a group, not an array")
	}
	// A paragraph inside rich text has `children`, but it is not a row.
	if ObservedArrayField(doc, "paragraph", "children") {
		t.Error("the walk entered rich text")
	}
}

func TestSetPathWalksArraysByRow(t *testing.T) {
	row := decodeDoc(t, `{"blockType":"cta","links":[{"id":"l0","link":{"url":"/a"}},{"id":"l1","link":{"url":"/b"}}],"title":"x"}`)

	for _, key := range []string{"links.0.link.url", "links[1].link.url", "links[id:l1].link.label"} {
		if err := SetPath(row, key, "/new"); err != nil {
			t.Errorf("SetPath(%s): %v", key, err)
		}
	}
	links := row["links"].([]any)
	if len(links) != 2 {
		t.Fatalf("the links array was replaced: %#v", row["links"])
	}
	l0 := links[0].(map[string]any)["link"].(map[string]any)
	l1 := links[1].(map[string]any)["link"].(map[string]any)
	if l0["url"] != "/new" || l1["url"] != "/new" || l1["label"] != "/new" {
		t.Errorf("links = %#v", links)
	}

	// Groups that do not exist yet are created; a scalar is replaced.
	if err := SetPath(row, "settings.theme.dark", true); err != nil {
		t.Fatal(err)
	}
	if err := SetPath(row, "title.text", "y"); err != nil {
		t.Fatal(err)
	}
	if row["settings"].(map[string]any)["theme"].(map[string]any)["dark"] != true {
		t.Errorf("settings = %#v", row["settings"])
	}

	// A whole array entry can be replaced by addressing it last.
	if err := SetPath(row, "links[0]", map[string]any{"link": map[string]any{"url": "/z"}}); err != nil {
		t.Fatal(err)
	}

	var miss *NoMatchError
	if err := SetPath(row, "links.5.link.url", "/x"); !asErr(err, &miss) || miss.Field != "links" {
		t.Errorf("a row that does not exist = %v, want NoMatchError on links", err)
	}
	var pe *PathError
	if err := SetPath(row, "buttons.0.label", "x"); !asErr(err, &pe) || pe.Kind != PathAbsent {
		t.Errorf("an absent array addressed by row = %v, want PathAbsent", err)
	}
	if err := SetPath(row, "links.link", "x"); !asErr(err, &pe) || pe.Kind != PathKeyOnArray {
		t.Errorf("a key on an array = %v, want PathKeyOnArray", err)
	}
}

func TestClearPath(t *testing.T) {
	row := decodeDoc(t, `{"links":[{"id":"l0","label":"a","link":{"url":"/a","newTab":true}}],"x":1,
	  "rt":{"root":{"type":"root","children":[]}},"media":{"id":7,"createdAt":"c","filename":"f.png"},
	  "rel":{"relationTo":"pages","value":3},"tags":[1,2],"blockName":"B"}`)
	if err := ClearPath(row, "links[id:l0].link.url"); err != nil {
		t.Fatal(err)
	}
	if err := ClearPath(row, "missing.deep.key"); err != nil {
		t.Errorf("clearing an absent path must be a no-op: %v", err)
	}
	if _, ok := row["missing"]; ok {
		t.Error("clearing an absent key created it")
	}
	link := row["links"].([]any)[0].(map[string]any)["link"].(map[string]any)
	if v, ok := link["url"]; !ok || v != nil {
		t.Errorf("url = %#v (present %v), want null: Payload keeps a key the row omits", v, ok)
	}
	// Each kind gets the value Payload clears on.
	for _, key := range []string{"rt", "media", "rel", "x", "blockName", "tags"} {
		if err := ClearPath(row, key); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"rt", "media", "rel", "x", "blockName"} {
		if v, ok := row[key]; !ok || v != nil {
			t.Errorf("%s = %#v, want null", key, v)
		}
	}
	if tags, ok := row["tags"].([]any); !ok || len(tags) != 0 {
		t.Errorf("tags = %#v, want []", row["tags"])
	}
	// A group is cleared field by field (null for the group itself is a 500,
	// {} keeps every stored sub-field).
	if err := ClearPath(row, "links[0].link"); err != nil {
		t.Fatal(err)
	}
	link = row["links"].([]any)[0].(map[string]any)["link"].(map[string]any)
	if len(link) != 2 || link["url"] != nil || link["newTab"] != nil {
		t.Errorf("group = %#v, want every sub-field null", link)
	}
	if err := ClearPath(row, "links"); err != nil {
		t.Fatal(err)
	}
	if links, ok := row["links"].([]any); !ok || len(links) != 0 {
		t.Errorf("links = %#v, want []", row["links"])
	}
	var miss *NoMatchError
	row2 := decodeDoc(t, `{"links":[{"id":"l0","label":"a"}]}`)
	if err := ClearPath(row2, "links[id:zz].label"); !asErr(err, &miss) {
		t.Errorf("a typo'd row selector = %v, want NoMatchError", err)
	}
	var pe *PathError
	if err := ClearPath(row2, "links[0]"); !asErr(err, &pe) {
		t.Errorf("clearing a whole row = %v, want a PathError pointing at `pay blocks rm`", err)
	}
}

func TestParsePathDottedTypeNth(t *testing.T) {
	p, err := ParsePath("layout.type:group[1].blocks")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Steps) != 3 || !p.Steps[1].Bracket || p.Steps[1].Text != "type:group[1]" {
		t.Fatalf("steps = %+v", p.Steps)
	}
	res, err := Resolve(decodeDoc(t, page), p)
	if err != nil {
		t.Fatal(err)
	}
	if res.At.Stable() != "layout[id:g3].blocks" {
		t.Errorf("resolved to %s", res.At.Stable())
	}
	// A bracketed row after a bare index is still two steps.
	if p, _ := ParsePath("layout.2[0]"); len(p.Steps) != 3 {
		t.Errorf("layout.2[0] = %+v", p.Steps)
	}
}

func TestArrayRowKind(t *testing.T) {
	cases := map[string]string{
		`[]`:                                    "",
		`[{"blockType":"a"},{"blockType":"b"}]`: RowKindBlocks,
		`[{"id":"x","label":"y"}]`:              RowKindArray,
		`[{"blockType":"a"},{"label":"y"}]`:     "",
		`[{},{"label":"y"}]`:                    RowKindArray,
		`[{}]`:                                  "",
		`[1,2]`:                                 "",
	}
	for in, want := range cases {
		var list []any
		if err := json.Unmarshal([]byte(in), &list); err != nil {
			t.Fatal(err)
		}
		if got := ArrayRowKind(list); got != want {
			t.Errorf("%s = %q, want %q", in, got, want)
		}
	}
}

func TestObservedRowKindAndFindRow(t *testing.T) {
	doc := decodeDoc(t, page)
	doc["createdAt"] = "x" // a real document has timestamps; it is not a relationship
	if got := ObservedRowKind(doc, "text", "buttons"); got != RowKindArray {
		t.Errorf("text.buttons = %q, want array", got)
	}
	if got := ObservedRowKind(doc, "group", "blocks"); got != RowKindBlocks {
		t.Errorf("group.blocks = %q, want blocks", got)
	}
	n, ok := FindRow(doc, "c1")
	if !ok || n.Path != "layout[3].blocks[1].blocks[1]" || n.Field != "layout[id:g3].blocks[id:r1].blocks" {
		t.Errorf("FindRow(c1) = %+v, %v", n, ok)
	}
	if _, ok := FindRow(doc, "nope"); ok {
		t.Error("FindRow found a row that does not exist")
	}
	loc := Location{Hops: []Hop{{Key: "layout"}, {Row: true, Index: 3}, {Key: "blocks"}, {Row: true, Index: 0}}}
	if v, ok := Lookup(doc, loc); !ok || v.(map[string]any)["id"] != "x0" {
		t.Errorf("Lookup = %v, %v", v, ok)
	}
}
