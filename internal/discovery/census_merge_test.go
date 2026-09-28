package discovery

import (
	"strings"
	"testing"
)

// unionShard is a pages shard whose layout union GraphQL published but whose
// slugs could only be inferred from interfaceNames — the localhost:3900
// website template with no project source on disk.
func unionShard() *Shard {
	s := NewShard("gen", "pages")
	s.Fields = []Field{NewField("layout", "layout")}
	s.Fields[0].PayloadType = TypeBlocks
	s.SetBlockField(BlockField{
		Path:           "layout",
		Slugs:          []string{"callToActionBlock", "contentBlock", "mediaBlock"},
		Source:         SourceUnionInferred,
		SlugSources:    map[string]string{"callToActionBlock": SourceUnionInferred, "contentBlock": SourceUnionInferred, "mediaBlock": SourceUnionInferred},
		SlugInterfaces: map[string]string{"callToActionBlock": "CallToActionBlock", "contentBlock": "ContentBlock", "mediaBlock": "MediaBlock"},
		InterfaceNames: []string{"CallToActionBlock", "ContentBlock", "MediaBlock"},
	})
	s.BlockSchemas = map[string]BlockTypeSchema{
		"callToActionBlock": {Slug: "callToActionBlock", InterfaceName: "CallToActionBlock", FieldsSource: SourceGraphQL,
			Fields: []BlockFieldSchema{
				{Name: "richText", Path: "richText", PayloadType: TypeRichText, PayloadTypeSource: SourceGraphQL, RequiredSource: SourceUnknown},
				{Name: "links", Path: "links", PayloadType: TypeArray, PayloadTypeSource: SourceGraphQL, RequiredSource: SourceUnknown},
				{Name: "link", Path: "links.link", Parent: strPtr("links"), PayloadType: TypeGroup, PayloadTypeSource: SourceGraphQL, RequiredSource: SourceUnknown},
				{Name: "appearance", Path: "links.link.appearance", Parent: strPtr("links.link"), PayloadType: TypeSelect,
					PayloadTypeSource: SourceGraphQL, Options: []string{"default", "outline"}, OptionsSource: SourceGraphQLEnum, RequiredSource: SourceUnknown},
				{Name: "reference", Path: "links.link.reference", Parent: strPtr("links.link"), PayloadType: TypeRelationship, PayloadTypeSource: SourceGraphQL, RequiredSource: SourceUnknown},
				{Name: "headline", Path: "headline", PayloadType: TypeText, PayloadTypeSource: SourceGraphQL, Required: boolPtr(true), RequiredSource: SourceGraphQL},
			}},
	}
	return s
}

func unionCensus(t *testing.T) *Census {
	t.Helper()
	b := NewCensusBuilder(EntityKindCollection, "pages")
	for i, d := range []string{
		`{"id":1,"layout":[{"blockType":"cta","richText":` + lexicalHello + `,"headline":"A","links":[{"id":"l","link":{"type":"custom","appearance":"default","label":"Go","url":"/a"}}]},{"blockType":"content","columns":[]}]}`,
		`{"id":2,"layout":[{"blockType":"cta","richText":` + lexicalHello + `,"headline":"B","links":[]},{"blockType":"mediaBlock","media":4}]}`,
		`{"id":3,"layout":[{"blockType":"cta","richText":` + lexicalHello + `,"headline":"C","links":[]}]}`,
	} {
		doc := decodeDoc(t, d)
		b.AddDoc(i+1, doc)
	}
	c := NewCensus("0123456789abcdef")
	c.Put(b.Finish())
	return c
}

func TestReconcileJoinsObservedSlugsToUnionMembers(t *testing.T) {
	k := NewBlockKnowledge(EntityKindCollection, "pages", unionCensus(t), unionShard())
	a := k.Accepted("layout")
	if !a.Authoritative || a.Source != SourceGraphQL {
		t.Fatalf("a fully matched union must be authoritative: %+v", a)
	}
	got := map[string]AcceptedType{}
	for _, at := range a.Types {
		got[at.Slug] = at
	}
	if len(got) != 3 {
		t.Fatalf("types = %v", a.Slugs())
	}
	if at := got["cta"]; at.InterfaceName != "CallToActionBlock" || at.Sources[0] != SourceCensusMatched {
		t.Errorf("cta = %+v", at)
	}
	if at := got["mediaBlock"]; at.Sources[0] != SourceUnionInferred || at.Sources[1] != SourceObservedCensus {
		t.Errorf("an inferred slug confirmed by a document keeps both sources: %+v", at)
	}
	if _, stale := got["callToActionBlock"]; stale {
		t.Error("the inferred slug the API rejects is still offered")
	}
	// The observed slug now reaches the GraphQL schema of its union member.
	if s, ok := k.GraphQLSchema("cta"); !ok || s.InterfaceName != "CallToActionBlock" {
		t.Errorf("cta did not reach its declared schema: %+v %v", s, ok)
	}
}

func TestReconcileLeavesUnmatchedInferredSlugsUnconfirmed(t *testing.T) {
	c := unionCensus(t)
	e, _ := c.Entity(EntityKindCollection, "pages")
	delete(e.Paths["layout"].Types, "content")
	k := NewBlockKnowledge(EntityKindCollection, "pages", c, unionShard())
	a := k.Accepted("layout")
	if a.Authoritative {
		t.Fatal("contentBlock was never confirmed, so the set cannot be authoritative")
	}
	if len(a.Unconfirmed) != 1 || a.Unconfirmed[0] != "contentBlock" || !strings.Contains(a.Reason, "contentBlock") {
		t.Errorf("unconfirmed = %v reason = %q", a.Unconfirmed, a.Reason)
	}
}

func TestReconcileWithoutAnyDeclaredSource(t *testing.T) {
	k := NewBlockKnowledge(EntityKindCollection, "pages", unionCensus(t), nil)
	a := k.Accepted("layout")
	if a.Authoritative || a.Source != SourceObservedCensus || len(a.Types) != 3 {
		t.Fatalf("observed-only = %+v", a)
	}
	if a.Types[0].Slug != "cta" || a.Types[0].Count != 3 {
		t.Errorf("most-used first: %+v", a.Types)
	}
}

func TestReconcileLegacyShardAndPins(t *testing.T) {
	legacy := NewShard("gen", "pages")
	legacy.Blocks = map[string][]string{"layout": {"cta", "content"}}
	legacy.BlocksSource = SourceProjectSource
	k := NewBlockKnowledge(EntityKindCollection, "pages", nil, legacy)
	if a := k.Accepted("layout"); !a.Authoritative || !a.Has("cta") {
		t.Errorf("a pre-block_fields shard's project-source list is declared: %+v", a)
	}

	pinned := NewShard("gen", "pages")
	pinned.SetBlockField(BlockField{Path: "layout", Slugs: []string{"hero"}, Source: SourceConfigured,
		SlugSources: map[string]string{"hero": SourceConfigured}})
	k = NewBlockKnowledge(EntityKindCollection, "pages", unionCensus(t), pinned)
	a := k.Accepted("layout")
	if !a.Authoritative || a.Source != SourceConfigured {
		t.Errorf("a pin is authoritative even when documents disagree: %+v", a)
	}
}

func TestSlugMatchesInterface(t *testing.T) {
	yes := [][2]string{{"cta", "CallToActionBlock"}, {"content", "ContentBlock"}, {"archive", "ArchiveBlock"},
		{"mediaBlock", "MediaBlock"}, {"media", "MediaBlock"}, {"formBlock", "FormBlock"}, {"hero", "Hero"}}
	for _, c := range yes {
		if !slugMatchesInterface(c[0], c[1]) {
			t.Errorf("%s should match %s", c[0], c[1])
		}
	}
	no := [][2]string{{"cta", "ContentBlock"}, {"text", "Textarea"}, {"x", "Xylophone"}, {"c", "Content"}}
	for _, c := range no {
		if slugMatchesInterface(c[0], c[1]) {
			t.Errorf("%s must not match %s", c[0], c[1])
		}
	}
}

func TestNestedAndParentScopedPatterns(t *testing.T) {
	_, e := buildTestCensus(t)
	c := NewCensus("0123456789abcdef")
	c.Put(e)
	k := NewBlockKnowledge(EntityKindCollection, "pages", c, nil)
	if a := k.Accepted("layout[].blocks"); !a.Has("text") || !a.Has("grid") || a.EmptyRows != 1 || a.Authoritative {
		t.Errorf("layout[].blocks = %+v", a)
	}
	if a := k.Accepted("group.blocks"); a.Parent != "group" || !a.Has("grid") || a.Has("image") {
		t.Errorf("group.blocks = %+v", a)
	}
	if a := k.Accepted("grid.blocks"); len(a.Types) != 1 || a.Types[0].Slug != "text" {
		t.Errorf("grid.blocks = %+v", a)
	}
	if a := k.Accepted("layout[].nothing"); len(a.Types) != 0 || a.Source != SourceUnknown {
		t.Errorf("unknown pattern = %+v", a)
	}
}

func TestMergeCompatibleTypesRefusesSameSlugDifferentBlock(t *testing.T) {
	page := &CensusType{Slug: "text", Count: 10, Fields: map[string]*CensusField{
		"heading": {Kind: CensusKindScalar}, "content": {Kind: CensusKindRichText}, "textSize": {Kind: CensusKindScalar}, "blockName": {}}}
	form := &CensusType{Slug: "text", Count: 3, Fields: map[string]*CensusField{
		"name": {Kind: CensusKindScalar}, "label": {Kind: CensusKindScalar}, "width": {Kind: CensusKindScalar}, "blockName": {}}}
	if CompatibleTypes(page, form) {
		t.Fatal("the form-builder text and a page text are different blocks")
	}
	if got := MergeCompatibleTypes(form, page); got != page {
		t.Errorf("the most-used shape must win alone, got %+v", got)
	}
	same := &CensusType{Slug: "text", Count: 2, Fields: map[string]*CensusField{
		"heading": {Kind: CensusKindScalar, Types: map[string]int{}}, "content": {Kind: CensusKindRichText, Types: map[string]int{}},
		"textSize": {Kind: CensusKindScalar, Types: map[string]int{}}, "blockName": {Types: map[string]int{}}}}
	for _, f := range page.Fields {
		f.Types = map[string]int{}
	}
	if got := MergeCompatibleTypes(page, same); got.Count != 12 {
		t.Errorf("compatible shapes merge: count = %d", got.Count)
	}
}

func TestBuildSchemaViewMergesDeclaredAndObserved(t *testing.T) {
	c := unionCensus(t)
	k := NewBlockKnowledge(EntityKindCollection, "pages", c, unionShard())
	gql, _ := k.GraphQLSchema("cta")
	v := BuildSchemaView("cta", k.TypeStats("cta"), &gql, nil)
	byPath := map[string]BlockFieldView{}
	for _, f := range v.Fields {
		byPath[f.Path] = f
	}
	if f := byPath["links[].link.appearance"]; f.OptionsSource != SourceGraphQLEnum || len(f.Options) != 2 {
		t.Errorf("declared options not merged: %+v", f)
	}
	if f := byPath["links[].link.reference"]; f.Kind != CensusKindRelationship || f.Required != "unknown" || f.KindSource != SourceGraphQL {
		t.Errorf("a declared-only field is re-rooted under its observed array parent: %+v", f)
	}
	if f := byPath["headline"]; f.Required != "true" || f.RequiredSource != SourceGraphQL {
		t.Errorf("NON_NULL wins over observation: %+v", f)
	}
	if f := byPath["richText"]; f.Required != "likely" || f.RequiredSource != SourceObservedCensus {
		t.Errorf("richText filled in on every row is likely required: %+v", f)
	}
	if strings.Join(v.Required, ",") != "headline" || strings.Join(v.Likely, ",") != "richText" {
		t.Errorf("required=%v likely=%v", v.Required, v.Likely)
	}
	if v.InterfaceName != "CallToActionBlock" || len(v.Sources) != 2 {
		t.Errorf("header = %+v", v)
	}
}

func TestNewBlockRowIsShapedLikeRealRows(t *testing.T) {
	_, e := buildTestCensus(t)
	row, chosen := NewBlockRow("hero", e.Types["hero"], nil)
	want := []string{"blockName", "blockType", "buttons", "content", "firstHero", "image", "textSize", "title", "variant"}
	got := sortedAnyKeys(row)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	if row["textSize"] != "regular" || row["variant"] != "mid" || row["firstHero"] != true {
		t.Errorf("defaults = %v %v %v", row["textSize"], row["variant"], row["firstHero"])
	}
	if row["title"] != nil {
		t.Errorf("free text must stay null, got %v", row["title"])
	}
	if b, _ := row["buttons"].([]any); b == nil || len(b) != 0 {
		t.Errorf("buttons = %#v", row["buttons"])
	}
	img, _ := row["image"].(map[string]any)
	if img == nil || img["aspectRatio"] != "1/1" || img["file"] != nil {
		t.Errorf("group sub-row = %#v", row["image"])
	}
	if !isLexicalRoot(row["content"].(map[string]any)) || ContainsContent(row["content"]) {
		t.Errorf("rich text must be Payload's empty value: %#v", row["content"])
	}
	if strings.Join(chosen, ",") != "firstHero,image.aspectRatio,textSize,variant" {
		t.Errorf("chosen = %v", chosen)
	}
	// GraphQL-only knowledge still yields a row.
	gql := unionShard().BlockSchemas["callToActionBlock"]
	row, _ = NewBlockRow("cta", nil, &gql)
	if _, ok := row["links"].([]any); !ok || row["headline"] != nil || row["blockType"] != "cta" {
		t.Errorf("graphql-only row = %#v", row)
	}
}

func TestDiffCensusEntity(t *testing.T) {
	_, prev := buildTestCensus(t)
	b := NewCensusBuilder(EntityKindCollection, "pages")
	for _, d := range censusDocs(t) {
		b.AddDoc(d["id"], d)
	}
	b.AddDoc(9, decodeDoc(t, `{"id":9,"layout":[{"blockType":"quote","text":"q"},
	  {"blockType":"group","blocks":[{"blockType":"grid","columnsDesktop":2,"splitDesktop":"1-2","blocks":[],"rowGap":"16px"}]}]}`))
	next := b.Finish()
	msgs := []string{}
	for _, ch := range DiffCensusEntity(prev, next) {
		msgs = append(msgs, ch.Message)
	}
	joined := strings.Join(msgs, "\n")
	for _, want := range []string{`pages: new blockType "quote"`, "grid gained rowGap"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	if len(DiffCensusEntity(nil, next)) != 0 {
		t.Error("a first census has nothing to diff")
	}
	back := DiffCensusEntity(next, prev)
	found := false
	for _, ch := range back {
		if ch.Change == "removed" && ch.BlockType == "quote" && ch.What == "block_type" {
			found = true
		}
	}
	if !found {
		t.Errorf("removal not reported: %+v", back)
	}

	// Regression: a truncated rebuild (`learn --census-max-docs 7`) reported
	// every type it did not happen to read as "no longer used".
	partial := *prev
	partial.Truncated = true
	for _, ch := range DiffCensusEntity(next, &partial) {
		if ch.Change == "removed" {
			t.Errorf("a truncated read cannot prove a removal: %+v", ch)
		}
		if ch.BlockType == "quote" && ch.What == "block_type" &&
			(ch.Change != "not_seen" || !strings.Contains(ch.Message, "partial read")) {
			t.Errorf("unread type must be not_seen: %+v", ch)
		}
	}
}
