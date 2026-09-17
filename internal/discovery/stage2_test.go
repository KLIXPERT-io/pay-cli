package discovery

import (
	"reflect"
	"strings"
	"testing"
)

// pageSchema mirrors the live Page shapes: the object type's title is nullable
// while mutationPageInput.title is NON_NULL, because drafts force-nullable the
// object type.
func pageSchema() (*entity, *Schema) {
	s := testSchema()
	s.SlugBySingular["Page"] = "pages"
	s.Types["Page"] = &IntroType{Kind: KindObject, Name: "Page", Fields: []IntroField{
		{Name: "id", Type: nonNull(scalar("Int"))},
		{Name: "title", Type: scalar("String")},
		{Name: "meta", Type: object("Page_Meta")},
		{Name: "layout", Type: nonNull(listOf(nonNull(&TypeRef{Kind: KindUnion, Name: "Page_Layout"})))},
		{Name: "_status", Type: enumRef("Page__status")},
		{Name: "publishedAt", Type: scalar("DateTime")},
		{Name: "activities", Type: object("CrmContact_Activities")},
	}}
	s.Types["mutationPageInput"] = &IntroType{Kind: KindInputObject, InputFields: []IntroInputField{
		{Name: "title", Type: nonNull(scalar("String"))},
		{Name: "meta", Type: &TypeRef{Kind: KindInputObject, Name: "mutationPage_MetaInput"}},
		{Name: "layout", Type: scalar("JSON")},
		{Name: "_status", Type: enumRef("Page__status_MutationInput")},
		{Name: "publishedAt", Type: scalar("String")},
	}}
	s.Types["mutationPage_MetaInput"] = &IntroType{Kind: KindInputObject, InputFields: []IntroInputField{
		{Name: "title", Type: nonNull(scalar("String"))},
	}}
	s.Types["Page_where"] = &IntroType{Kind: KindInputObject, InputFields: []IntroInputField{
		{Name: "title", Type: &TypeRef{Kind: KindInputObject, Name: "Page_title_operator"}},
		{Name: "meta__title", Type: &TypeRef{Kind: KindInputObject, Name: "Page_meta__title_operator"}},
		{Name: "publishedAt", Type: &TypeRef{Kind: KindInputObject, Name: "Page_publishedAt_operator"}},
		{Name: "AND", Type: nil},
		{Name: "OR", Type: nil},
	}}
	return &entity{Slug: "pages", Singular: "Page", Plural: "Pages", Kind: kindCollection}, s
}

func TestBuildShardRequiredComesFromTheInputType(t *testing.T) {
	e, s := pageSchema()
	shard := buildShard(e, s, buildShardOptions{Generation: "gen"})
	byPath := map[string]Field{}
	for _, f := range shard.Fields {
		byPath[f.Path] = f
	}

	title, ok := byPath["title"]
	if !ok {
		t.Fatal("no title field")
	}
	if title.Required == nil || !*title.Required {
		t.Fatalf("title.required = %v; it must come from mutationPageInput (NON_NULL), not from Page (nullable)",
			show(title.Required))
	}
	if title.RequiredSource != SourceGraphQLInput {
		t.Errorf("required_source = %q", title.RequiredSource)
	}
	// Present on the object type and absent from the input type is positive
	// evidence of a read-only field.
	if id := byPath["id"]; !id.ReadOnly || id.Required == nil || *id.Required {
		t.Errorf("id should be read-only and not required: %+v", id)
	}
	if join := byPath["activities"]; !join.ReadOnly || join.PayloadType != TypeJoin {
		t.Errorf("activities should be a read-only join: %+v", join)
	}
	if !reflect.DeepEqual(shard.JoinFields, []string{"activities"}) {
		t.Errorf("join_fields = %v", shard.JoinFields)
	}
	// Nested required-ness is read from the nested input type.
	if nested := byPath["meta.title"]; nested.Required == nil || !*nested.Required {
		t.Errorf("meta.title.required = %v, want true", show(nested.Required))
	}
	if got := byPath["meta.title"].GraphQLPath; got != "meta__title" {
		t.Errorf("graphql_path = %q", got)
	}
	if !reflect.DeepEqual(shard.RequiredPaths, []string{"meta.title", "title"}) {
		t.Errorf("required_paths = %v", shard.RequiredPaths)
	}
}

func TestBuildShardQueryabilityAndOperators(t *testing.T) {
	e, s := pageSchema()
	shard := buildShard(e, s, buildShardOptions{Generation: "gen"})
	byPath := map[string]Field{}
	for _, f := range shard.Fields {
		byPath[f.Path] = f
	}
	if !byPath["title"].Queryable {
		t.Error("title is in Page_where and must be queryable")
	}
	if byPath["_status"].Queryable {
		t.Error("_status is absent from Page_where here and must not be queryable")
	}
	if !byPath["meta.title"].Queryable {
		t.Error("meta__title is in Page_where, so meta.title must be queryable")
	}
	wantText := []string{"equals", "not_equals", "like", "contains", "in", "not_in", "all"}
	if !reflect.DeepEqual(byPath["title"].Operators, wantText) {
		t.Errorf("title operators = %v, want %v", byPath["title"].Operators, wantText)
	}
	wantDate := []string{"equals", "not_equals", "greater_than_equal", "greater_than", "less_than_equal", "less_than", "like", "exists"}
	if !reflect.DeepEqual(byPath["publishedAt"].Operators, wantDate) {
		t.Errorf("publishedAt operators = %v", byPath["publishedAt"].Operators)
	}
	if byPath["layout"].PayloadType != TypeBlocks {
		t.Errorf("layout payload_type = %q", byPath["layout"].PayloadType)
	}
	if ws := byPath["layout"].WriteShape; ws == nil || *ws != WriteShapeJSON {
		t.Errorf("layout write_shape = %v", ws)
	}
}

func TestOperatorsForRelationInputObject(t *testing.T) {
	// A polymorphic relationship's where entry is a {T}_{f}_Relation input
	// object taking relationTo and value, not an operator set.
	got := OperatorsFor(TypeRelationship, "Page_hero__links__link__reference_Relation")
	if len(got) == 0 {
		t.Fatal("a _Relation entry must still advertise usable operators")
	}
	fallback := OperatorsFor("some-unmodelled-kind", "")
	if !reflect.DeepEqual(fallback, []string{"equals", "not_equals", "exists"}) {
		t.Errorf("fallback operators = %v", fallback)
	}
	// The returned slice must be a copy: a caller that sorts it in place must
	// not corrupt every other field of the same kind.
	a := OperatorsFor(TypeText, "")
	a[0] = "mutated"
	if OperatorsFor(TypeText, "")[0] != "equals" {
		t.Fatal("OperatorsFor returned a shared slice")
	}
}

func TestUseAPIKeyNeedsBothFields(t *testing.T) {
	// Verified live on mutationUserInput.
	both := &IntroType{InputFields: []IntroInputField{{Name: "apiKey"}, {Name: "enableAPIKey"}, {Name: "email"}}}
	if v := UseAPIKey(both); v == nil || !*v {
		t.Errorf("use_api_key = %v, want true", show(v))
	}
	one := &IntroType{InputFields: []IntroInputField{{Name: "apiKey"}}}
	if v := UseAPIKey(one); v == nil || *v {
		t.Errorf("use_api_key = %v, want false", show(v))
	}
	if UseAPIKey(nil) != nil {
		t.Error("with no input type the answer is unknown, not false")
	}
}

func TestFlagsFromObject(t *testing.T) {
	// Verified live: Page has _status and not deletedAt, CrmContact has
	// deletedAt and not _status, Media has folder.
	page := &IntroType{Fields: []IntroField{{Name: "_status"}, {Name: "title"}}}
	drafts, trash, folders := FlagsFromObject(page)
	if drafts == nil || !*drafts {
		t.Errorf("Page drafts = %v", show(drafts))
	}
	if trash == nil || *trash {
		t.Errorf("Page trash = %v", show(trash))
	}
	if folders == nil || *folders {
		t.Errorf("Page folders = %v", show(folders))
	}

	contact := &IntroType{Fields: []IntroField{{Name: "deletedAt"}}}
	_, trash, _ = FlagsFromObject(contact)
	if trash == nil || !*trash {
		t.Errorf("CrmContact trash = %v", show(trash))
	}

	media := &IntroType{Fields: []IntroField{{Name: "folder"}}}
	_, _, folders = FlagsFromObject(media)
	if folders == nil || !*folders {
		t.Errorf("Media folders = %v", show(folders))
	}

	d, tr, f := FlagsFromObject(nil)
	if d != nil || tr != nil || f != nil {
		t.Error("with no object type every answer is unknown, not false")
	}
}

func TestUploadFromObject(t *testing.T) {
	if v := UploadFromObject(testSchema().Type("Media")); v == nil || !*v {
		t.Errorf("media upload = %v", show(v))
	}
	if v := UploadFromObject(testSchema().Type("CrmCompany")); v == nil || *v {
		t.Errorf("crm-companies upload = %v", show(v))
	}
	if UploadFromObject(nil) != nil {
		t.Error("unknown must stay unknown")
	}
}

func TestBuildTypeQueryEscapesAndAliases(t *testing.T) {
	q := buildTypeQuery([]string{"Page", `Weird"Name`})
	if !strings.Contains(q, `t0: __type(name: "Page")`) {
		t.Errorf("missing alias t0:\n%s", q)
	}
	if !strings.Contains(q, `t1: __type(name: "Weird\"Name")`) {
		t.Errorf("type name was not escaped:\n%s", q)
	}
	// A GraphQL alias must be a NAME, which is why the aliases are positional.
	if strings.Contains(q, "Weird\"Name:") {
		t.Error("a type name leaked into an alias position")
	}
}

func TestAbsorbTypesHalvesOnTheDocumentedTriggers(t *testing.T) {
	d := &Discoverer{}
	names := []string{"A", "B"}
	tests := []struct {
		name string
		res  *gqlResult
		want bool
	}{
		{"all resolved", gqlOK(`{"data":{"t0":{"kind":"OBJECT","name":"A"},"t1":{"kind":"OBJECT","name":"B"}}}`), true},
		// A type that genuinely does not exist (a global has no {S}_where) is
		// a legitimate null, not a batch failure.
		{"legit null", gqlOK(`{"data":{"t0":{"kind":"OBJECT","name":"A"},"t1":null}}`), true},
		{"silently dropped alias", gqlOK(`{"data":{"t0":{"kind":"OBJECT","name":"A"}}}`), false},
		{"complexity", gqlOK(`{"data":{"t0":{"kind":"OBJECT"},"t1":{"kind":"OBJECT"}},"errors":[{"message":"Query is too complex"}]}`), false},
		{"no data", gqlOK(`{"errors":[{"message":"nope"}]}`), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schema := &Schema{Types: map[string]*IntroType{}}
			if got := d.absorbTypes(schema, names, tt.res); got != tt.want {
				t.Fatalf("absorbTypes = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAbsorbTypesRejectsOversizedBody(t *testing.T) {
	d := &Discoverer{}
	res := gqlOK(`{"data":{"t0":{"kind":"OBJECT","name":"A"}}}`)
	res.Body = make([]byte, maxGraphQLBody+1)
	if d.absorbTypes(&Schema{Types: map[string]*IntroType{}}, []string{"A"}, res) {
		t.Fatal("a body over 8 MB must halve the batch")
	}
}

func TestPruneKnownSkipsResolvedAndNullTypes(t *testing.T) {
	schema := &Schema{Types: map[string]*IntroType{"A": {}, "B": nil}}
	got := pruneKnown(schema, []string{"A", "B", "C", "C", ""})
	if !reflect.DeepEqual(got, []string{"C"}) {
		t.Fatalf("pruneKnown = %v, want [C]", got)
	}
}

func TestTitleFieldPrefersAText(t *testing.T) {
	s := NewShard("g", "pages")
	s.Fields = []Field{
		func() Field { f := NewField("id", "id"); f.PayloadType = TypeID; return f }(),
		func() Field { f := NewField("slug", "slug"); f.PayloadType = TypeText; return f }(),
		func() Field { f := NewField("title", "title"); f.PayloadType = TypeText; return f }(),
	}
	if got := TitleField(s); got == nil || *got != "title" {
		t.Fatalf("TitleField = %v, want title", got)
	}
	if TitleField(NewShard("g", "x")) != nil {
		t.Error("an empty shard has no title field")
	}
}

// TestMissingTypesAsksForBlockObjectTypes is the cost-shaped regression: a
// blocks field's union members are OBJECT types carrying the block's own
// fields, and nothing else in the schema describes what goes inside a block
// (Payload publishes no input type for one).
//
// They must be requested through the SAME batch the other leaves ride, which
// is what missingTypes returning them proves: fetchTypes issues one aliased
// request per round, so one more name is one more alias, not one more request.
func TestMissingTypesAsksForBlockObjectTypes(t *testing.T) {
	e, s := pageSchema()
	s.Types["Page_Layout"] = &IntroType{Kind: KindUnion, Name: "Page_Layout",
		PossibleTypes: namesOf([]string{"CallToActionBlock", "MediaBlock"})}

	entities := map[string]*entity{"pages": e}
	want := missingTypes(entities, s)
	for _, name := range []string{"CallToActionBlock", "MediaBlock"} {
		if !containsString(want, name) {
			t.Errorf("missingTypes() = %v, want it to ask for the block object type %q", want, name)
		}
	}
	// A block has no mutation input type — mutationCallToActionBlockInput does
	// not exist — so asking for one would spend an alias per block to learn
	// nothing.
	for _, name := range want {
		if strings.HasPrefix(name, "mutationCallToActionBlock") || strings.HasPrefix(name, "mutationMediaBlock") {
			t.Errorf("missingTypes() asks for %q, which Payload never generates for a block", name)
		}
	}

	// Once the block types resolve, their own nested interiors become the next
	// round's names — again in one batch, and again without input types.
	s.Types["CallToActionBlock"] = &IntroType{Kind: KindObject, Name: "CallToActionBlock", Fields: []IntroField{
		{Name: "links", Type: listOf(nonNull(object("CallToActionBlock_Links")))},
	}}
	s.Types["MediaBlock"] = &IntroType{Kind: KindObject, Name: "MediaBlock"}
	next := missingTypes(entities, s)
	if !containsString(next, "CallToActionBlock_Links") {
		t.Errorf("missingTypes() = %v, want the block's nested row type", next)
	}
	if containsString(next, "mutationCallToActionBlock_LinksInput") {
		t.Error("missingTypes() asked for a nested block input type, which does not exist")
	}
}
