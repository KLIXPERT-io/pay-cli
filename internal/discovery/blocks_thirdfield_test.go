package discovery

import (
	"reflect"
	"strings"
	"testing"
)

// The live dummy project has exactly TWO blocks fields (verified: a scan of
// every cached field shard finds only pages.layout -> Page_Layout and
// forms.fields -> Form_Fields), so "it works on both of them" is not evidence
// that the resolver generalises rather than happening to suit two shapes.
//
// These tests supply a THIRD field the project does not have: a different
// union, under a NESTED path, in a shard that also carries the two real ones.
// Nothing in the resolver may key off a collection name, a field name or a
// union name.

// thirdUnion is a blocks field none of the live collections has: it mixes a
// member the project source can confirm (MediaBlock -> mediaBlock), one only
// the interfaceName heuristic can name (PricingTableBlock -> plugin-style),
// and one that cannot be inverted at all (an acronym run).
var thirdUnion = []string{"MediaBlock", "PricingTable", "FAQEntry"}

func TestResolveBlocksGeneralisesToAThirdField(t *testing.T) {
	got := ResolveBlocks("hero.panels", thirdUnion, dummyProject)

	want := []string{"mediaBlock", "pricingTable"}
	if !reflect.DeepEqual(got.Slugs, want) {
		t.Fatalf("hero.panels = %v, want %v", got.Slugs, want)
	}
	// The confirmed member keeps its provenance; the inferred one keeps its
	// own. A field that mixes them is "mixed", never silently "project-source".
	if got.SlugSources["mediaBlock"] != SourceProjectSource {
		t.Errorf("mediaBlock source = %q, want %q", got.SlugSources["mediaBlock"], SourceProjectSource)
	}
	if got.SlugSources["pricingTable"] != SourceUnionInferred {
		t.Errorf("pricingTable source = %q, want %q", got.SlugSources["pricingTable"], SourceUnionInferred)
	}
	if got.Source != SourceMixed {
		t.Errorf("field source = %q, want %q", got.Source, SourceMixed)
	}
	// The uninvertible member is reported, not guessed at and not hidden.
	if !reflect.DeepEqual(got.Unresolved, []string{"FAQEntry"}) {
		t.Errorf("unresolved = %v, want [FAQEntry]", got.Unresolved)
	}
	if got.Reason == "" {
		t.Error("an incomplete answer must carry a reason")
	}
	// The other collections' blocks must not leak in, in either direction.
	for _, alien := range []string{"cta", "content", "archive", "formBlock", "checkbox", "text"} {
		if containsString(got.Slugs, alien) {
			t.Errorf("hero.panels offers %q, which is not in its own union", alien)
		}
	}
	// And the nested path must be quoted verbatim in the caveat, so an agent
	// can tell which field the caveat is about.
	if !strings.Contains(got.Reason, `"hero.panels"`) {
		t.Errorf("reason %q does not name the field", got.Reason)
	}
}

// TestResolveShardBlocksThreeFieldsStayIndependent runs the same third field
// through the shard-level entry point beside the two real ones: three blocks
// fields, three different unions, three different answers, in one shard.
func TestResolveShardBlocksThreeFieldsStayIndependent(t *testing.T) {
	schema := &Schema{Types: map[string]*IntroType{
		"Page_Layout":  {Kind: KindUnion, Name: "Page_Layout", PossibleTypes: namesOf(pageLayoutUnion)},
		"Form_Fields":  {Kind: KindUnion, Name: "Form_Fields", PossibleTypes: namesOf(formFieldsUnion)},
		"Hero_Panels":  {Kind: KindUnion, Name: "Hero_Panels", PossibleTypes: namesOf(thirdUnion)},
		"Page_Content": {Kind: KindObject, Name: "Page_Content"},
	}}

	shard := NewShard("g1", "three")
	mk := func(path, gqlType string) Field {
		f := NewField(path, path)
		f.PayloadType = TypeBlocks
		f.GraphQLType = strPtr(gqlType)
		return f
	}
	// content is a richText field whose GraphQL type is an OBJECT, not a
	// union: it must get no blockType list at all. This is the shape that made
	// pages.layout advertise banner and code, which live only in the lexical
	// editor's BlocksFeature on posts.content.
	richText := NewField("content", "content")
	richText.PayloadType = TypeRichText
	richText.GraphQLType = strPtr("Page_Content")

	shard.Fields = []Field{
		mk("layout", "Page_Layout"),
		mk("fields", "Form_Fields"),
		mk("hero.panels", "Hero_Panels"),
		richText,
	}

	ResolveShardBlocks(shard, schema, dummyProject)

	cases := []struct {
		path  string
		slugs []string
		src   string
	}{
		{"layout", []string{"archive", "content", "cta", "formBlock", "mediaBlock"}, SourceProjectSource},
		{"fields", []string{"checkbox", "country", "email", "message", "number", "select",
			"state", "text", "textarea"}, SourceUnionInferred},
		{"hero.panels", []string{"mediaBlock", "pricingTable"}, SourceMixed},
	}
	for _, c := range cases {
		if !reflect.DeepEqual(shard.Blocks[c.path], c.slugs) {
			t.Errorf("%s = %v, want %v", c.path, shard.Blocks[c.path], c.slugs)
		}
		if got := shard.BlockFields[c.path].Source; got != c.src {
			t.Errorf("%s source = %q, want %q", c.path, got, c.src)
		}
	}
	if _, ok := shard.Blocks["content"]; ok {
		t.Errorf("a richText field got a blockType list: %v", shard.Blocks["content"])
	}
	if len(shard.Blocks) != 3 {
		t.Errorf("blocks = %v, want exactly the three blocks fields", shard.Blocks)
	}
}
