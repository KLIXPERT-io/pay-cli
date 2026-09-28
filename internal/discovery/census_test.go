package discovery

import (
	"encoding/json"
	"strings"
	"testing"
)

// decodeDoc parses a JSON document the way the REST client does (numbers as
// json.Number), so the census sees exactly what it sees live.
func decodeDoc(t *testing.T, s string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return m
}

const lexicalHello = `{"root":{"type":"root","children":[{"type":"paragraph","children":[{"type":"text","text":"Hello","format":0,"detail":0,"mode":"normal","style":"","version":1}],"direction":"ltr","format":"","indent":0,"version":1}],"direction":"ltr","format":"","indent":0,"version":1}}`

// censusDocs is shaped like the klixpert stage pages: a hero, a group whose
// blocks hold a text and a grid (whose blocks hold another text), a columns
// block whose columns hold image blocks, an orphan {} row and an untyped row.
func censusDocs(t *testing.T) []map[string]any {
	t.Helper()
	hero := func(id, title, size string, file int) string {
		return `{"id":"` + id + `","blockType":"hero","blockName":null,"title":"` + title + `","variant":"mid",` +
			`"textSize":"` + size + `","firstHero":true,"content":` + lexicalHello + `,` +
			`"image":{"file":` + itoa(file) + `,"alt":null,"aspectRatio":"1/1"},` +
			`"buttons":[{"id":"b` + id + `","label":"Go","style":"primary"}]}`
	}
	return []map[string]any{
		decodeDoc(t, `{"id":1,"title":"one","meta":{"title":"x"},"breadcrumbs":[{"id":"bc","doc":1,"url":"/one"}],
		  "layout":[`+hero("h1", "One", "regular", 119)+`,
		    {"id":"g1","blockType":"group","blockName":null,"contentWidth":"inhalt","blocks":[
		      {"id":"t1","blockType":"text","heading":"H","textSize":"regular","section":{"contentWidth":"inhalt","backgroundColor":null}},
		      {"id":"gr","blockType":"grid","columnsDesktop":3,"splitDesktop":"equal","blocks":[
		        {"id":"t2","blockType":"text","heading":null,"textSize":"large","section":{"contentWidth":"inhalt","backgroundColor":"white"}}]},
		      {}]},
		    {"id":"c1","blockType":"columns","columns":[{"id":"col","blockType":"column","blocks":[{"id":"im","blockType":"image","image":7}]}]}]}`),
		decodeDoc(t, `{"id":2,"title":"two","layout":[`+hero("h2", "Two", "regular", 120)+`,
		    {"id":"g2","blockType":"group","blockName":null,"contentWidth":"voll","blocks":[
		      {"id":"t3","blockType":"text","heading":"H3","textSize":"regular","section":{"contentWidth":"inhalt","backgroundColor":null}}]},
		    {"id":"x","title":"no type"}]}`),
		decodeDoc(t, `{"id":3,"title":"three","layout":[`+hero("h3", "Three", "large", 121)+`]}`),
		decodeDoc(t, `{"id":4,"title":"four","layout":[`+hero("h4", "Four", "regular", 122)+`]}`),
		decodeDoc(t, `{"id":5,"title":"five","layout":[]}`),
	}
}

func buildTestCensus(t *testing.T) (*CensusBuilder, *CensusEntity) {
	t.Helper()
	b := NewCensusBuilder(EntityKindCollection, "pages")
	for _, d := range censusDocs(t) {
		b.AddDoc(d["id"], d)
	}
	return b, b.Finish()
}

func TestCensusRecordsEveryBlocksPathPattern(t *testing.T) {
	_, e := buildTestCensus(t)
	want := []string{"layout", "layout[].blocks", "layout[].columns", "layout[].blocks[].blocks", "layout[].columns[].blocks"}
	if got := e.PathPatterns(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("patterns = %v, want %v", got, want)
	}
	top := e.Paths["layout"]
	if top.Types["hero"] != 4 || top.Types["group"] != 2 || top.Types["columns"] != 1 {
		t.Errorf("layout types = %v", top.Types)
	}
	if top.UntypedRows != 1 || top.Docs != 4 {
		t.Errorf("layout untyped=%d docs=%d, want 1 and 4 (doc 5 has an empty layout)", top.UntypedRows, top.Docs)
	}
	nested := e.Paths["layout[].blocks"]
	if nested.EmptyRows != 1 {
		t.Errorf("the orphan {} row was not counted: %+v", nested)
	}
	if nested.ParentTypes["group"] != 2 || nested.Parent != "layout" || nested.Field != "blocks" {
		t.Errorf("nested parent bookkeeping = %+v", nested)
	}
	if deep := e.Paths["layout[].columns[].blocks"]; deep.Types["image"] != 1 || deep.Parent != "layout[].columns" {
		t.Errorf("deep pattern = %+v", deep)
	}
	if e.Types["text"].UsedIn["layout[].blocks"] != 2 || e.Types["text"].UsedIn["layout[].blocks[].blocks"] != 1 {
		t.Errorf("text used_in = %v", e.Types["text"].UsedIn)
	}
	if e.Types["text"].Docs != 2 {
		t.Errorf("text docs = %d, want 2", e.Types["text"].Docs)
	}
	if e.DocsScanned != 5 || e.BlockRows != 13 {
		t.Errorf("docs=%d rows=%d", e.DocsScanned, e.BlockRows)
	}
}

func TestCensusProfilesFields(t *testing.T) {
	_, e := buildTestCensus(t)
	hero := e.Types["hero"]
	if _, ok := hero.Fields["id"]; ok {
		t.Error("id is plumbing and must not be profiled")
	}
	if _, ok := hero.Fields["blockType"]; ok {
		t.Error("blockType must not be profiled as a field")
	}
	cases := map[string]string{
		"content": CensusKindRichText, "image": CensusKindGroup, "buttons": CensusKindArray,
		"title": CensusKindScalar, "blockName": CensusKindUnknown,
	}
	for name, kind := range cases {
		if got := hero.Fields[name].Kind; got != kind {
			t.Errorf("hero.%s kind = %s, want %s", name, got, kind)
		}
	}
	if !hero.Fields["textSize"].EnumLike {
		t.Errorf("textSize (regular x3, large x1) should be enum-like: %+v", hero.Fields["textSize"])
	}
	if hero.Fields["variant"].EnumLike {
		t.Error("a value that never varies is a default, not a vocabulary")
	}
	if hero.Fields["title"].EnumLike || len(hero.Fields["title"].Values) != 0 {
		t.Errorf("distinct titles are content: %+v", hero.Fields["title"])
	}
	if got := hero.Fields["buttons"].Fields["style"].Values; len(got) != 1 || got[0].Value != "primary" || got[0].Count != 4 {
		t.Errorf("buttons[].style values = %+v", got)
	}
	if hero.Fields["buttons"].Items != 4 {
		t.Errorf("buttons items = %d", hero.Fields["buttons"].Items)
	}
	group := e.Types["group"]
	if f := group.Fields["blocks"]; f.Kind != CensusKindBlocks || f.Accepts["text"] != 2 || f.Accepts["grid"] != 1 {
		t.Errorf("group.blocks = %+v", f)
	}
	if got := e.Types["text"].Fields["section"].Fields["backgroundColor"]; got.NonNull != 1 || got.Present != 3 {
		t.Errorf("section.backgroundColor presence = %+v", got)
	}
}

func TestCensusKeepsTheMostCompleteInstancesFirst(t *testing.T) {
	_, e := buildTestCensus(t)
	text := e.Types["text"]
	if len(text.Instances) != 3 {
		t.Fatalf("instances = %+v", text.Instances)
	}
	if text.Instances[0].Path != "layout[1].blocks[0]" || text.Instances[0].Score < text.Instances[2].Score {
		t.Errorf("instances not ordered by completeness: %+v", text.Instances)
	}
	grid := e.Types["grid"].Instances[0]
	if grid.Slug != "pages" || grid.Kind != EntityKindCollection || grid.Path != "layout[1].blocks[1]" {
		t.Errorf("grid ref = %+v", grid)
	}
}

func TestEnumLikeIsConservative(t *testing.T) {
	mk := func(values ...any) *CensusField {
		f := newCensusField()
		for _, v := range values {
			f.Present++
			f.NonNull++
			f.Types[jsonTypeName(v)]++
			f.kinds[CensusKindScalar]++
			f.track(v)
		}
		f.finish(false)
		return f
	}
	if !mk("left", "left", "center", "left").EnumLike {
		t.Error("token vocabulary should be enum-like")
	}
	if mk("Termin buchen", "Termin buchen", "Mehr erfahren", "Termin buchen").EnumLike {
		t.Error("repeated human copy is not a select")
	}
	if mk("a", "b", "c").EnumLike {
		t.Error("three observations are not enough")
	}
	if mk("x", "x", "x", "x").EnumLike {
		t.Error("a constant is not a vocabulary")
	}
	if mk(true, false, true, true).EnumLike {
		t.Error("booleans are never enum-like")
	}
	if got := mk(true, false, true, true).Values; len(got) != 2 || got[0].Value != true {
		t.Errorf("boolean values are kept for defaults: %+v", got)
	}
}

func TestCensusDepth1ProbeSettlesRelationships(t *testing.T) {
	b, e := buildTestCensus(t)
	ids := b.ProbeCandidates(3)
	if len(ids) == 0 {
		t.Fatal("no probe candidates although image ids were observed")
	}
	depth1 := decodeDoc(t, `{"id":1,"layout":[
	  {"blockType":"hero","image":{"file":{"id":119,"filename":"a.jpg","mimeType":"image/jpeg"}}},
	  {"blockType":"columns","columns":[{"blockType":"column","blocks":[{"blockType":"image","image":{"id":7,"title":"x"}}]}]}]}`)
	b.ApplyDepth1(depth1)
	if f := e.Types["hero"].Fields["image"].Fields["file"]; f.Kind != CensusKindUpload || f.KindSource != SourceObservedDepth1 || f.EnumLike {
		t.Errorf("hero.image.file = %+v", f)
	}
	if f := e.Types["image"].Fields["image"]; f.Kind != CensusKindRelationship {
		t.Errorf("image.image kind = %s", f.Kind)
	}
}

func TestNormalizeBlockPath(t *testing.T) {
	cases := map[string]string{
		"layout":                       "layout",
		"layout.2.blocks":              "layout[].blocks",
		"layout[2].blocks":             "layout[].blocks",
		"layout[id:6ab9].blocks":       "layout[].blocks",
		"layout[type:group[1]].blocks": "layout[].blocks",
		"layout[].blocks[].blocks":     "layout[].blocks[].blocks",
		"layout.0.columns.1.blocks":    "layout[].columns[].blocks",
		"group.blocks":                 "group.blocks",
		"layout[-1].blocks":            "layout[].blocks",
		"hero.items":                   "hero.items",
	}
	cases["\tgroup.blocks "] = "group.blocks" // surrounding whitespace is trimmed
	for in, want := range cases {
		if got := NormalizeBlockPath(in); got != want {
			t.Errorf("NormalizeBlockPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValueAtRowPath(t *testing.T) {
	doc := censusDocs(t)[0]
	v, ok := ValueAtRowPath(doc, "layout[1].blocks[1].blocks[0]")
	row, isRow := v.(map[string]any)
	if !ok || !isRow || row["id"] != "t2" {
		t.Fatalf("got %v %v", v, ok)
	}
	if _, ok := ValueAtRowPath(doc, "layout[9]"); ok {
		t.Error("an index past the end resolved")
	}
	if _, ok := ValueAtRowPath(doc, "layout[x]"); ok {
		t.Error("a non-numeric index resolved")
	}
	if v, ok := ValueAtRowPath(doc, "meta.title"); !ok || v != "x" {
		t.Errorf("meta.title = %v", v)
	}
}

func TestContainsBlockRows(t *testing.T) {
	if !ContainsBlockRows(map[string]any{"hero": map[string]any{"items": []any{map[string]any{"blockType": "x"}}}}) {
		t.Error("nested blocks array not found")
	}
	if ContainsBlockRows(map[string]any{"title": "x", "tags": []any{json.Number("1")}}) {
		t.Error("a body without block rows reported rows")
	}
	lexical := decodeDoc(t, `{"root":{"children":[{"type":"block","fields":{"blockType":"x"}}]}}`)
	if ContainsBlockRows(map[string]any{"content": lexical}) {
		t.Error("rich text internals are not block rows")
	}
}

func TestCensusRoundTripsThroughJSON(t *testing.T) {
	_, e := buildTestCensus(t)
	c := NewCensus("0123456789abcdef")
	c.Put(e)
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	back, ok := DecodeCensus(raw)
	if !ok {
		t.Fatal("DecodeCensus rejected its own output")
	}
	got, ok := back.Entity(EntityKindCollection, "pages")
	if !ok || got.Types["hero"].Count != 4 || !got.Types["hero"].Fields["textSize"].EnumLike {
		t.Fatalf("round trip lost facts: %+v", got)
	}
	if _, ok := DecodeCensus([]byte(`{"census_version":99}`)); ok {
		t.Error("a foreign census_version decoded")
	}
}

// Select options are identifiers. URL paths, colours and numbers repeat
// because content repeats, and treating them as a vocabulary made every new
// link a value_not_in_enum warning and `blocks new` copy another card's link.
func TestEnumLikeRejectsLinksColoursAndNumbers(t *testing.T) {
	b := NewCensusBuilder(EntityKindCollection, "pages")
	links := []string{"/skills/a", "/skills/a", "/skills/b", "/skills/b", "/skills/c", "/skills/c", "/skills/a", "/skills/b"}
	for i, link := range links {
		size, ratio, colour, width := "default", "1/1", "#0047FF", 50
		if i%2 == 1 {
			size, ratio, colour, width = "higher", "2/3", "#BCC9EB", 100
		}
		row := map[string]any{"blockType": "bentoItem", "link": link, "size": size, "aspectRatio": ratio,
			"color": colour, "width": json.Number(itoa(width)), "mail": []string{"a@x.de", "b@x.de"}[i%2]}
		b.AddDoc(i+1, map[string]any{"layout": []any{row}})
	}
	e := b.Finish()
	f := e.Types["bentoItem"].Fields
	for _, name := range []string{"link", "color", "width", "mail"} {
		if f[name].EnumLike || f[name].IsVocabulary() {
			t.Errorf("%s must not be enum-like: %+v", name, f[name])
		}
	}
	for _, name := range []string{"size", "aspectRatio"} {
		if !f[name].EnumLike || !f[name].IsVocabulary() {
			t.Errorf("%s is a select: %+v", name, f[name])
		}
	}
	if likelyRequired(f["width"], e.Types["bentoItem"].Count) {
		t.Error("a number set to one of a few values is a layout knob, not likely required")
	}
	row, chosen := NewBlockRow("bentoItem", e.Types["bentoItem"], nil)
	if row["link"] != nil || row["color"] != nil || row["size"] != "default" {
		t.Errorf("row = %v", row)
	}
	for _, c := range chosen {
		if c == "link" {
			t.Errorf("a link copied from another row is not a default: %v", chosen)
		}
	}

	// A census written by an older build (enum_like: true on a link field)
	// is re-checked on use.
	old := &CensusField{Kind: CensusKindScalar, EnumLike: true, Types: map[string]int{"string": 8},
		Values: []CensusValue{{Value: "/skills/a", Count: 3}, {Value: "/skills/b", Count: 3}}}
	if old.IsVocabulary() {
		t.Error("an old cache's URL vocabulary must not be trusted")
	}
	k := &BlockKnowledge{Kind: EntityKindCollection, Slug: "pages"}
	v := &validator{k: k, res: BlockValidation{Issues: []BlockIssue{}}}
	v.checkValue("bentoItem", "link", "/skills/new", old, BlockFieldSchema{}, false, "layout[0].link")
	if len(v.res.Issues) != 0 {
		t.Errorf("value_not_in_enum on a link: %+v", v.res.Issues)
	}
	for _, s := range []string{"/x", "https://x.de", "#fff", "a@b.de", "logo.svg", "example.com", "mailto:x", "rgb(0,0,0)"} {
		if !LinkLikeValue(s) {
			t.Errorf("LinkLikeValue(%q) = false", s)
		}
	}
	for _, s := range []string{"1/1", "2/3", "50%", "grey-400", "fill-icon", "2,2,2", "inhalt"} {
		if LinkLikeValue(s) {
			t.Errorf("LinkLikeValue(%q) = true", s)
		}
	}
}

// The probe covers every id-like field at least once, not just the fields of
// the documents that hold the most id-like values.
func TestProbeCandidatesCoverEveryField(t *testing.T) {
	b := NewCensusBuilder(EntityKindCollection, "pages")
	logos := func() []any {
		return []any{map[string]any{"blockType": "logos", "logos": []any{
			map[string]any{"image": json.Number("1"), "width": json.Number("440")},
			map[string]any{"image": json.Number("2"), "width": json.Number("168")}}, "count": json.Number("2")}}
	}
	b.AddDoc(1, map[string]any{"layout": logos()})
	b.AddDoc(2, map[string]any{"layout": logos()})
	b.AddDoc(3, map[string]any{"layout": []any{map[string]any{"blockType": "image", "image": json.Number("7")}}})
	b.AddDoc(4, map[string]any{"layout": []any{map[string]any{"blockType": "videoClaim", "video": json.Number("9")}}})
	b.AddDoc(5, map[string]any{"layout": []any{map[string]any{"blockType": "userMeta", "person": json.Number("3")}}})
	b.Finish()
	ids := b.ProbeCandidates(relationProbeDocs)
	if len(ids) != 4 || ids[0] != 1 || ids[1] != 3 || ids[2] != 4 || ids[3] != 5 {
		t.Fatalf("probe documents = %v, want [1 3 4 5] (doc 2 adds no field)", ids)
	}
}
