package discovery

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// liveBlockSchema is the live project's GraphQL, verbatim, for the two blocks
// that exercise both halves of the required-ness ladder:
//
//	__type(name:"MediaBlock").fields -> media: Media, id, blockName, blockType
//	  (media is `required: true` in src/blocks/MediaBlock/config.ts and STILL
//	   nullable here, because Pages has drafts and drafts force-nullable every
//	   field beneath them — so only project source can prove it.)
//	__type(name:"Textarea").fields   -> name: String!, label, width: Float,
//	                                    defaultValue, required, id, blockName, blockType
//	  (the form-builder plugin lives in node_modules, which is never scanned,
//	   so only the NON_NULL can prove name is required.)
func liveBlockSchema() *Schema {
	return &Schema{
		SlugBySingular: map[string]string{"Media": "media", "Page": "pages"},
		Types: map[string]*IntroType{
			"Page_Layout": {Kind: KindUnion, Name: "Page_Layout",
				PossibleTypes: namesOf([]string{"MediaBlock", "CallToActionBlock"})},
			"Form_Fields": {Kind: KindUnion, Name: "Form_Fields", PossibleTypes: namesOf([]string{"Textarea"})},
			"Media": {Kind: KindObject, Name: "Media", Fields: []IntroField{
				{Name: "id", Type: scalarRef("Int")},
				{Name: "filename", Type: scalarRef("String")},
				{Name: "mimeType", Type: scalarRef("String")},
				{Name: "filesize", Type: scalarRef("Float")},
				{Name: "url", Type: scalarRef("String")},
			}},
			"MediaBlock": {Kind: KindObject, Name: "MediaBlock", Fields: []IntroField{
				{Name: "media", Type: &TypeRef{Kind: KindObject, Name: "Media"}},
				{Name: "id", Type: scalarRef("String")},
				{Name: "blockName", Type: scalarRef("String")},
				{Name: "blockType", Type: scalarRef("String")},
			}},
			"CallToActionBlock": {Kind: KindObject, Name: "CallToActionBlock", Fields: []IntroField{
				{Name: "richText", Type: scalarRef("JSON")},
				{Name: "links", Type: &TypeRef{Kind: KindList, OfType: &TypeRef{
					Kind: KindNonNull, OfType: &TypeRef{Kind: KindObject, Name: "CallToActionBlock_Links"}}}},
				{Name: "id", Type: scalarRef("String")},
				{Name: "blockName", Type: scalarRef("String")},
				{Name: "blockType", Type: scalarRef("String")},
			}},
			"CallToActionBlock_Links": {Kind: KindObject, Name: "CallToActionBlock_Links", Fields: []IntroField{
				{Name: "link", Type: &TypeRef{Kind: KindObject, Name: "CallToActionBlock_Links_Link"}},
				{Name: "id", Type: scalarRef("String")},
			}},
			"CallToActionBlock_Links_Link": {Kind: KindObject, Name: "CallToActionBlock_Links_Link",
				Fields: []IntroField{
					{Name: "url", Type: scalarRef("String")},
					{Name: "appearance", Type: &TypeRef{Kind: KindEnum, Name: "CallToActionBlock_Links_Link_appearance"}},
				}},
			"CallToActionBlock_Links_Link_appearance": {Kind: KindEnum,
				Name:       "CallToActionBlock_Links_Link_appearance",
				EnumValues: []IntroEnumValue{{Name: "default"}, {Name: "outline"}}},
			"Textarea": {Kind: KindObject, Name: "Textarea", Fields: []IntroField{
				{Name: "name", Type: &TypeRef{Kind: KindNonNull, OfType: scalarRef("String")}},
				{Name: "label", Type: scalarRef("String")},
				{Name: "width", Type: scalarRef("Float")},
				{Name: "required", Type: scalarRef("Boolean")},
				{Name: "id", Type: scalarRef("String")},
				{Name: "blockName", Type: scalarRef("String")},
				{Name: "blockType", Type: scalarRef("String")},
			}},
		},
	}
}

func scalarRef(name string) *TypeRef { return &TypeRef{Kind: KindScalar, Name: name} }

// liveBlockSources mirrors what config.Scan reads off /home/flo/payload-dummy:
// mediaBlock fully parsed, cta only partly (linkGroup() contributes `links`),
// textarea absent entirely because it is a plugin block in node_modules.
func liveBlockSources() BlockSources {
	req := true
	return BlockSources{
		ProjectSource: []string{"mediaBlock", "cta"},
		SlugByInterface: map[string]string{
			"MediaBlock":        "mediaBlock",
			"CallToActionBlock": "cta",
		},
		SourceDecls: map[string]BlockSourceDecl{
			"mediaBlock": {
				File:     "/p/src/blocks/MediaBlock/config.ts",
				Fields:   []BlockSourceField{{Name: "media", Type: "upload", Required: &req}},
				Complete: true,
			},
			"cta": {
				File:     "/p/src/blocks/CallToAction/config.ts",
				Fields:   []BlockSourceField{{Name: "richText", Type: "richText"}},
				Complete: false,
			},
		},
	}
}

func fieldByPath(t *testing.T, s BlockTypeSchema, path string) BlockFieldSchema {
	t.Helper()
	for _, f := range s.Fields {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("%s has no field %q; got %v", s.Slug, path, blockPaths(s))
	return BlockFieldSchema{}
}

func blockPaths(s BlockTypeSchema) []string {
	out := []string{}
	for _, f := range s.Fields {
		out = append(out, f.Path)
	}
	return out
}

// TestBlockSchemaRequiredFromProjectSource: MediaBlock.media is `required:
// true` on disk and nullable in GraphQL (drafts force-nullable it). Only the
// project source can answer, and the answer must be labelled as coming from
// there.
func TestBlockSchemaRequiredFromProjectSource(t *testing.T) {
	src := liveBlockSources()
	got := BuildBlockSchema("mediaBlock", "MediaBlock", liveBlockSchema(), src.SourceDecls["mediaBlock"], true)

	media := fieldByPath(t, got, "media")
	if media.Required == nil || !*media.Required {
		t.Fatalf("media.required = %v, want true — src/blocks/MediaBlock/config.ts says required: true", media.Required)
	}
	if media.RequiredSource != SourceProjectSource {
		t.Errorf("media.required_source = %q, want %q", media.RequiredSource, SourceProjectSource)
	}
	// It is an upload pointing at the media collection: without the target an
	// agent cannot know what id to put there.
	if media.PayloadType != TypeUpload || !reflect.DeepEqual(media.RelationTo, []string{"media"}) {
		t.Errorf("media = %s relation_to %v, want upload -> [media]", media.PayloadType, media.RelationTo)
	}
	if media.WriteShape == nil || *media.WriteShape != WriteShapeID {
		t.Errorf("media.write_shape = %v, want %q", media.WriteShape, WriteShapeID)
	}
	if got.ConfigFile != "/p/src/blocks/MediaBlock/config.ts" {
		t.Errorf("config_file = %q, want the file required-ness was read from", got.ConfigFile)
	}
	if len(got.RequiredUnknown) != 0 {
		t.Errorf("required_unknown = %v, want empty for a fully declared block", got.RequiredUnknown)
	}
	if got.Reason != "" {
		t.Errorf("a block with no gaps must carry no reason, got %q", got.Reason)
	}
	if !reflect.DeepEqual(got.RequiredFields(), []string{"media"}) {
		t.Errorf("required_fields = %v, want [media]", got.RequiredFields())
	}
}

// TestBlockSchemaRequiredFromGraphQLNonNull: the form-builder's Textarea has
// no config on disk at all (node_modules is never scanned), so the NON_NULL on
// the object type is the only proof of required there is — and the fields it
// does not mark are unknown, never false.
func TestBlockSchemaRequiredFromGraphQLNonNull(t *testing.T) {
	got := BuildBlockSchema("textarea", "Textarea", liveBlockSchema(), BlockSourceDecl{}, false)

	name := fieldByPath(t, got, "name")
	if name.Required == nil || !*name.Required {
		t.Fatalf("name.required = %v, want true — Textarea.name is String! in the live schema", name.Required)
	}
	if name.RequiredSource != SourceGraphQL {
		t.Errorf("name.required_source = %q, want %q", name.RequiredSource, SourceGraphQL)
	}
	for _, path := range []string{"label", "width", "required"} {
		f := fieldByPath(t, got, path)
		if f.Required != nil {
			t.Errorf("%s.required = %v, want null: a nullable block field proves nothing, because a "+
				"draft-enabled parent force-nullables required fields", path, *f.Required)
		}
		if f.RequiredSource != SourceUnknown {
			t.Errorf("%s.required_source = %q, want %q", path, f.RequiredSource, SourceUnknown)
		}
	}
	if got.ConfigFile != "" {
		t.Errorf("config_file = %q, want empty for a plugin block", got.ConfigFile)
	}
	if got.RequiredSource != SourceMixed {
		t.Errorf("required_source = %q, want %q — one proved field beside three unknowns may never "+
			"report as fully answered", got.RequiredSource, SourceMixed)
	}
	if !strings.Contains(got.Reason, "node_modules") {
		t.Errorf("reason %q does not say why the gaps exist", got.Reason)
	}
}

// TestBlockSchemaNeverGuessesRequiredness is the tri-state guard: a field the
// scanner never saw must come back null even though its block IS declared on
// disk, because the declaration was incomplete.
func TestBlockSchemaNeverGuessesRequiredness(t *testing.T) {
	src := liveBlockSources()
	got := BuildBlockSchema("cta", "CallToActionBlock", liveBlockSchema(), src.SourceDecls["cta"], true)

	rich := fieldByPath(t, got, "richText")
	if rich.Required == nil || *rich.Required {
		t.Fatalf("richText.required = %v, want false: it IS declared and carries no `required:`", rich.Required)
	}
	if rich.RequiredSource != SourceProjectSource {
		t.Errorf("richText.required_source = %q, want %q", rich.RequiredSource, SourceProjectSource)
	}
	// The `type: 'richText'` literal is the only thing that can tell lexical
	// richText from a json field: both are the same JSON scalar.
	if rich.PayloadType != TypeRichText || rich.PayloadTypeSource != SourceProjectSource {
		t.Errorf("richText = %s (%s), want richText confirmed from project source",
			rich.PayloadType, rich.PayloadTypeSource)
	}

	links := fieldByPath(t, got, "links")
	if links.Required != nil {
		t.Fatalf("links.required = %v, want null: linkGroup() contributed it and the scanner never "+
			"saw its declaration", *links.Required)
	}
	if !containsString(got.RequiredUnknown, "links") {
		t.Errorf("required_unknown = %v, want links named", got.RequiredUnknown)
	}
	if !strings.Contains(got.Reason, "linkGroup()") {
		t.Errorf("reason %q does not name the helper that made the list incomplete", got.Reason)
	}
}

// TestBlockSchemaWalksNestedInteriors: `links.link.url` is what an agent
// actually writes, and a flat one-level answer would leave it invisible.
func TestBlockSchemaWalksNestedInteriors(t *testing.T) {
	src := liveBlockSources()
	got := BuildBlockSchema("cta", "CallToActionBlock", liveBlockSchema(), src.SourceDecls["cta"], true)

	url := fieldByPath(t, got, "links.link.url")
	if url.PayloadType != TypeText {
		t.Errorf("links.link.url = %s, want text", url.PayloadType)
	}
	if url.Parent == nil || *url.Parent != "links.link" {
		t.Errorf("links.link.url parent = %v, want links.link", url.Parent)
	}
	appearance := fieldByPath(t, got, "links.link.appearance")
	if !reflect.DeepEqual(appearance.Options, []string{"default", "outline"}) {
		t.Errorf("links.link.appearance options = %v, want the enum values", appearance.Options)
	}
	// A nested array row's id is Payload's, not the author's.
	rowID := fieldByPath(t, got, "links.id")
	if !rowID.Plumbing {
		t.Error("links.id is a server-generated array-row id and must be marked plumbing")
	}
}

// TestBlockSchemaMarksPlumbing pins the three keys that are protocol rather
// than content — and that blockType is the one an agent MUST send.
func TestBlockSchemaMarksPlumbing(t *testing.T) {
	got := BuildBlockSchema("mediaBlock", "MediaBlock", liveBlockSchema(), BlockSourceDecl{}, false)

	for _, name := range []string{"id", "blockName", "blockType"} {
		f := fieldByPath(t, got, name)
		if !f.Plumbing {
			t.Errorf("%s.plumbing = false, want true", name)
		}
		if f.RequiredSource != SourcePayloadProtocol {
			t.Errorf("%s.required_source = %q, want %q", name, f.RequiredSource, SourcePayloadProtocol)
		}
	}
	if bt := fieldByPath(t, got, "blockType"); bt.Required == nil || !*bt.Required {
		t.Error("blockType.required must be true: Payload silently drops a row without a known blockType")
	}
	if bn := fieldByPath(t, got, "blockName"); bn.Required == nil || *bn.Required {
		t.Error("blockName.required must be false: it is an optional admin label")
	}
	for _, f := range got.ContentFields() {
		if f.Plumbing {
			t.Errorf("ContentFields() returned the plumbing field %q", f.Name)
		}
	}
	if got.PlumbingNote == "" {
		t.Error("every block schema must carry the plumbing note")
	}
}

// TestBlockSchemaUnreadableTypeIsUnknownNotEmpty: a union member whose object
// type never resolved must say so. "No fields" and "not discovered" are the
// two answers an agent must not confuse.
func TestBlockSchemaUnreadableTypeIsUnknownNotEmpty(t *testing.T) {
	got := BuildBlockSchema("ghost", "GhostBlock", liveBlockSchema(), BlockSourceDecl{}, false)
	if got.FieldsSource != SourceUnknown {
		t.Errorf("fields_source = %q, want %q", got.FieldsSource, SourceUnknown)
	}
	if len(got.Fields) != 0 {
		t.Errorf("fields = %v, want none", blockPaths(got))
	}
	if !strings.Contains(got.Reason, "GhostBlock") {
		t.Errorf("reason %q does not name the type that could not be read", got.Reason)
	}
}

// TestResolveShardBlocksAttachesSchemasPerSlug is the wiring test: resolving a
// shard's blocks must also attach each slug's interior, keyed by the slug an
// agent writes rather than by the interfaceName it must never write.
func TestResolveShardBlocksAttachesSchemasPerSlug(t *testing.T) {
	schema := liveBlockSchema()
	shard := NewShard("g1", "pages")
	layout := NewField("layout", "layout")
	layout.PayloadType = TypeBlocks
	layout.GraphQLType = strPtr("Page_Layout")
	shard.Fields = append(shard.Fields, layout)

	ResolveShardBlocks(shard, schema, liveBlockSources())
	shard.Finalize()

	slugs := []string{}
	for s := range shard.BlockSchemas {
		slugs = append(slugs, s)
	}
	sort.Strings(slugs)
	if !reflect.DeepEqual(slugs, []string{"cta", "mediaBlock"}) {
		t.Fatalf("block_schemas keys = %v, want the two slugs of Page_Layout", slugs)
	}
	if _, wrong := shard.BlockSchemas["MediaBlock"]; wrong {
		t.Error("block_schemas is keyed by interfaceName; it must be keyed by the writable slug")
	}
	bs, ok := shard.BlockSchemaFor("mediaBlock")
	if !ok || bs.InterfaceName != "MediaBlock" {
		t.Fatalf("mediaBlock schema = %+v", bs)
	}
	// The pairing itself is published, because that is the only way back from
	// a union member to the slug.
	bf, _ := shard.BlockFieldFor("layout")
	if bf.SlugInterfaces["mediaBlock"] != "MediaBlock" {
		t.Errorf("slug_interface_names = %v, want mediaBlock -> MediaBlock", bf.SlugInterfaces)
	}
	if !reflect.DeepEqual(shard.BlockSlugsFor(), []string{"cta", "mediaBlock"}) {
		t.Errorf("BlockSlugsFor() = %v", shard.BlockSlugsFor())
	}
}

// TestShardHashCoversBlockSchemas: §8.2 verifies a cached shard against the
// index's fields_sha256. A block interior that changed without changing the
// hash would be served from cache forever.
func TestShardHashCoversBlockSchemas(t *testing.T) {
	base := NewShard("g1", "pages")
	base.Finalize()
	before := base.SHA256

	base.BlockSchemas = map[string]BlockTypeSchema{
		"cta": {Slug: "cta", InterfaceName: "CallToActionBlock"},
	}
	base.Finalize()
	if base.SHA256 == before {
		t.Fatal("adding a block schema did not change the shard hash")
	}
}

// TestShardWithoutBlockSchemasStaysReadable is the backward-compatibility
// guard: a shard written before this key existed must still decode, and must
// report "not discovered" rather than "this block has no fields".
func TestShardWithoutBlockSchemasStaysReadable(t *testing.T) {
	old := `{"generation":"g1","slug":"pages","sha256":"x","fields":[],"join_fields":[],` +
		`"blocks":{"layout":["cta"]},"blocks_source":"project-source",` +
		`"block_fields":{"layout":{"path":"layout","slugs":["cta"],"source":"project-source"}},` +
		`"required_paths":[]}`
	var shard Shard
	if err := json.Unmarshal([]byte(old), &shard); err != nil {
		t.Fatalf("a pre-block_schemas shard no longer decodes: %v", err)
	}
	if shard.BlockSchemas != nil {
		t.Errorf("block_schemas = %v, want nil on an old shard", shard.BlockSchemas)
	}
	if _, ok := shard.BlockSchemaFor("cta"); ok {
		t.Error("BlockSchemaFor reported a schema that is not in the shard")
	}
	if got, ok := shard.BlockTypesFor("layout"); !ok || !reflect.DeepEqual(got, []string{"cta"}) {
		t.Errorf("the old shard's slugs stopped working: %v %v", got, ok)
	}
}

// TestBlockFieldSchemaKeySetIsExactlyDeclared mirrors §7.8.2's contract for
// the field entries: an agent parses one without presence checks, so the key
// set may not drift from the documented list.
func TestBlockFieldSchemaKeySetIsExactlyDeclared(t *testing.T) {
	got := BuildBlockSchema("mediaBlock", "MediaBlock", liveBlockSchema(), BlockSourceDecl{}, false)
	if len(got.Fields) == 0 {
		t.Fatal("no fields to check")
	}
	raw, err := json.Marshal(got.Fields[0])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	want := append([]string(nil), BlockFieldSchemaKeys...)
	sort.Strings(keys)
	sort.Strings(want)
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("block field keys = %v, want %v", keys, want)
	}
}

// TestPinnedSlugsStillReachTheirInterior: a profile pin outranks every other
// source for the SLUG, but the interior is published under a GraphQL union
// member, so the two still have to be joined — and joined only when the join
// is unambiguous.
func TestPinnedSlugsStillReachTheirInterior(t *testing.T) {
	src := liveBlockSources()
	src.Configured = map[string][]string{"layout": {"mediaBlock", "housePinned"}}

	got := ResolveBlocks("layout", []string{"MediaBlock", "CallToActionBlock"}, src)
	if got.Source != SourceConfigured {
		t.Fatalf("source = %q, want the pin to win", got.Source)
	}
	if got.SlugInterfaces["mediaBlock"] != "MediaBlock" {
		t.Errorf("slug_interface_names = %v, want mediaBlock -> MediaBlock", got.SlugInterfaces)
	}
	// housePinned matches no union member. Attaching it to CallToActionBlock
	// because that member happened to be spare would show one block's fields
	// under another block's name.
	if iface, joined := got.SlugInterfaces["housePinned"]; joined {
		t.Errorf("housePinned was joined to %q, but no union member resolves to it", iface)
	}
}
