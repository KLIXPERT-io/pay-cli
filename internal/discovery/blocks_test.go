package discovery

import (
	"reflect"
	"strings"
	"testing"
)

// dummyProject mirrors what config.Scan harvests from /home/flo/payload-dummy:
// every src/blocks/*/config.ts declares a slug beside an interfaceName.
var dummyProject = BlockSources{
	ProjectSource: []string{"cta", "content", "mediaBlock", "archive", "formBlock", "banner", "code"},
	SlugByInterface: map[string]string{
		"CallToActionBlock": "cta",
		"ContentBlock":      "content",
		"MediaBlock":        "mediaBlock",
		"ArchiveBlock":      "archive",
		"FormBlock":         "formBlock",
		"BannerBlock":       "banner",
		"CodeBlock":         "code",
	},
}

// pageLayoutUnion and formFieldsUnion are the live possibleTypes, verbatim:
//
//	__type(name:"Page_Layout").possibleTypes
//	  = CallToActionBlock, ContentBlock, MediaBlock, ArchiveBlock, FormBlock
//	__type(name:"Form_Fields").possibleTypes
//	  = Checkbox, Country, Email, Message, Number, Select, State, Text, Textarea
var (
	pageLayoutUnion = []string{"CallToActionBlock", "ContentBlock", "MediaBlock", "ArchiveBlock", "FormBlock"}
	formFieldsUnion = []string{"Checkbox", "Country", "Email", "Message", "Number", "Select", "State", "Text", "Textarea"}
)

// TestResolveBlocksIsPerFieldNotGlobal is the regression test for the bug this
// file exists to prevent: the project source scan harvests one project-wide
// bag of slugs, and attaching that bag to every blocks field told an agent
// that `forms.fields` accepts `cta` — a blockType Payload discards silently.
func TestResolveBlocksIsPerFieldNotGlobal(t *testing.T) {
	layout := ResolveBlocks("layout", pageLayoutUnion, dummyProject)
	wantLayout := []string{"archive", "content", "cta", "formBlock", "mediaBlock"}
	if !reflect.DeepEqual(layout.Slugs, wantLayout) {
		t.Fatalf("pages.layout = %v, want exactly Page_Layout's 5 members %v", layout.Slugs, wantLayout)
	}
	// banner and code are real project blocks that Page_Layout does NOT
	// accept. Offering them is over-broad, which is the same class of silent
	// wrong answer as offering too few.
	for _, forbidden := range []string{"banner", "code"} {
		if containsString(layout.Slugs, forbidden) {
			t.Errorf("pages.layout offers %q, which is not in Page_Layout's union", forbidden)
		}
	}
	if layout.Source != SourceProjectSource {
		t.Errorf("layout source = %q, want every slug confirmed from project source", layout.Source)
	}
	if layout.Reason != "" {
		t.Errorf("a fully confirmed field must carry no caveat, got %q", layout.Reason)
	}

	fields := ResolveBlocks("fields", formFieldsUnion, dummyProject)
	wantFields := []string{"checkbox", "country", "email", "message", "number", "select", "state", "text", "textarea"}
	if !reflect.DeepEqual(fields.Slugs, wantFields) {
		t.Fatalf("forms.fields = %v, want the 9 form field types %v", fields.Slugs, wantFields)
	}
	// The two fields were resolved from the same BlockSources. If one bag were
	// still being shared, these would be equal.
	for _, pageBlock := range wantLayout {
		if containsString(fields.Slugs, pageBlock) {
			t.Errorf("forms.fields offers the page layout block %q", pageBlock)
		}
	}
}

// TestResolveBlocksPluginBlocksAreMarkedInferred pins the provenance rule:
// Form_Fields' members come from the form-builder plugin in node_modules,
// which §7.10 never scans, so every one of them is resolved by heuristic and
// must SAY so.
func TestResolveBlocksPluginBlocksAreMarkedInferred(t *testing.T) {
	got := ResolveBlocks("fields", formFieldsUnion, dummyProject)
	if got.Source != SourceUnionInferred {
		t.Fatalf("source = %q, want %q", got.Source, SourceUnionInferred)
	}
	for _, slug := range got.Slugs {
		if got.SlugSources[slug] != SourceUnionInferred {
			t.Errorf("%q provenance = %q, want %q", slug, got.SlugSources[slug], SourceUnionInferred)
		}
	}
	if got.Reason == "" {
		t.Error("an inferred list must carry a reason an agent can read")
	}
	if !reflect.DeepEqual(got.Interfaces, dedupeSorted(formFieldsUnion)) {
		t.Errorf("interfaces = %v, want the union reported verbatim", got.Interfaces)
	}
}

// TestResolveBlocksMixedProvenance covers a union that is half project source
// and half plugin: the field-level source must not claim either one.
func TestResolveBlocksMixedProvenance(t *testing.T) {
	got := ResolveBlocks("layout", []string{"CallToActionBlock", "Text"}, dummyProject)
	if !reflect.DeepEqual(got.Slugs, []string{"cta", "text"}) {
		t.Fatalf("slugs = %v", got.Slugs)
	}
	if got.Source != SourceMixed {
		t.Errorf("source = %q, want %q", got.Source, SourceMixed)
	}
	if got.SlugSources["cta"] != SourceProjectSource {
		t.Errorf("cta came from project source, got %q", got.SlugSources["cta"])
	}
	if got.SlugSources["text"] != SourceUnionInferred {
		t.Errorf("text was inferred, got %q", got.SlugSources["text"])
	}
}

// TestResolveBlocksObservedUpgradesAnInference: a blockType seen in a real
// document is evidence, not a guess, so it outranks the heuristic that
// produced the same string.
func TestResolveBlocksObservedUpgradesAnInference(t *testing.T) {
	src := dummyProject
	src.Observed = map[string][]string{"fields": {"text", "email"}}
	got := ResolveBlocks("fields", []string{"Text", "Email", "Select"}, src)
	if got.SlugSources["text"] != SourceObserved || got.SlugSources["email"] != SourceObserved {
		t.Errorf("observed slugs keep the inferred label: %v", got.SlugSources)
	}
	if got.SlugSources["select"] != SourceUnionInferred {
		t.Errorf("select was never observed, got %q", got.SlugSources["select"])
	}
}

func TestResolveBlocksOrder(t *testing.T) {
	src := dummyProject
	src.Configured = map[string][]string{"layout": {"pinned"}}
	src.Observed = map[string][]string{"layout": {"observedBlock"}}

	got := ResolveBlocks("layout", pageLayoutUnion, src)
	if got.Source != SourceConfigured || !reflect.DeepEqual(got.Slugs, []string{"pinned"}) {
		t.Fatalf("a pin must win: %+v", got)
	}

	src.Configured = nil
	got = ResolveBlocks("layout", pageLayoutUnion, src)
	if got.Source != SourceProjectSource {
		t.Fatalf("the field's own union must come second: %+v", got)
	}

	// No union at all — the REST-only path. The project-wide bag is NOT an
	// acceptable substitute; only this field's own observed values are.
	got = ResolveBlocks("layout", nil, src)
	if got.Source != SourceObserved || !reflect.DeepEqual(got.Slugs, []string{"observedBlock"}) {
		t.Fatalf("observed must come third: %+v", got)
	}

	src.Observed = nil
	got = ResolveBlocks("layout", nil, src)
	if got.Slugs != nil || got.Source != SourceUnknown {
		t.Fatalf("unresolved must be nil/unknown, never the project-wide bag: %+v", got)
	}
	if !strings.Contains(got.Reason, "cannot be determined from the Payload API") {
		t.Errorf("reason does not say so in plain words: %q", got.Reason)
	}
}

// TestResolveBlocksNeverReturnsAnInterfaceName is the invariant that makes the
// answer safe to send: CallToActionBlock is a GraphQL type name and the REST
// API silently discards it.
func TestResolveBlocksNeverReturnsAnInterfaceName(t *testing.T) {
	for _, src := range []BlockSources{dummyProject, {}} {
		for _, union := range [][]string{pageLayoutUnion, formFieldsUnion} {
			got := ResolveBlocks("layout", union, src)
			for _, slug := range got.Slugs {
				if containsString(union, slug) {
					t.Errorf("interfaceName %q was returned as a blockType slug", slug)
				}
			}
		}
	}
}

// TestResolveBlocksWithoutProjectPairsStaysHonest: when nothing paired the
// interfaceNames the answer is still produced, but every slug is labelled
// inferred — including the ones that are wrong (CallToActionBlock's slug is
// cta, not callToActionBlock).
func TestResolveBlocksWithoutProjectPairsStaysHonest(t *testing.T) {
	got := ResolveBlocks("layout", pageLayoutUnion, BlockSources{})
	if got.Source != SourceUnionInferred {
		t.Fatalf("source = %q, want %q", got.Source, SourceUnionInferred)
	}
	if got.Reason == "" {
		t.Fatal("an entirely inferred list must carry a reason")
	}
	for _, slug := range got.Slugs {
		if got.SlugSources[slug] == SourceProjectSource {
			t.Errorf("%q claims project source with no project source at all", slug)
		}
	}
}

func TestSlugFromInterfaceName(t *testing.T) {
	// Verified against @payloadcms/plugin-form-builder, which declares exactly
	// these slugs, and against live form documents whose fields[].blockType
	// values are text, email and textarea.
	for name, want := range map[string]string{
		"Checkbox": "checkbox", "Country": "country", "Email": "email",
		"Message": "message", "Number": "number", "Select": "select",
		"State": "state", "Text": "text", "Textarea": "textarea",
		"MediaBlock": "mediaBlock", "FormBlock": "formBlock",
	} {
		if got := SlugFromInterfaceName(name); got != want {
			t.Errorf("SlugFromInterfaceName(%q) = %q, want %q", name, got, want)
		}
	}
	// Cases the rule cannot invert must refuse rather than guess: FAQBlock
	// would become fAQBlock, which no Payload project would accept.
	for _, name := range []string{"", "FAQBlock", "APIKeyBlock", "cta"} {
		if got := SlugFromInterfaceName(name); got != "" {
			t.Errorf("SlugFromInterfaceName(%q) = %q, want a refusal", name, got)
		}
	}
}

// TestResolveBlocksReportsUninvertibleMembers: a union member whose slug
// cannot be recovered must be named, not dropped in silence — the field then
// accepts more than PayCLI can list.
func TestResolveBlocksReportsUninvertibleMembers(t *testing.T) {
	got := ResolveBlocks("layout", []string{"CallToActionBlock", "FAQBlock"}, dummyProject)
	if !reflect.DeepEqual(got.Slugs, []string{"cta"}) {
		t.Fatalf("slugs = %v, want only the one that could be resolved", got.Slugs)
	}
	if !reflect.DeepEqual(got.Unresolved, []string{"FAQBlock"}) {
		t.Fatalf("unresolved = %v, want FAQBlock named", got.Unresolved)
	}
	if !strings.Contains(got.Reason, "FAQBlock") {
		t.Errorf("reason does not name the gap: %q", got.Reason)
	}
}

func TestResolveShardBlocksPerField(t *testing.T) {
	schema := &Schema{Types: map[string]*IntroType{
		"Page_Layout": {Kind: KindUnion, Name: "Page_Layout", PossibleTypes: namesOf(pageLayoutUnion)},
		"Form_Fields": {Kind: KindUnion, Name: "Form_Fields", PossibleTypes: namesOf(formFieldsUnion)},
	}}
	shard := NewShard("g1", "mixed")
	layout := NewField("layout", "layout")
	layout.PayloadType = TypeBlocks
	layout.GraphQLType = strPtr("Page_Layout")
	fields := NewField("fields", "fields")
	fields.PayloadType = TypeBlocks
	fields.GraphQLType = strPtr("Form_Fields")
	title := NewField("title", "title")
	title.PayloadType = TypeText
	shard.Fields = []Field{layout, fields, title}

	ResolveShardBlocks(shard, schema, dummyProject)

	if len(shard.Blocks["layout"]) != 5 || len(shard.Blocks["fields"]) != 9 {
		t.Fatalf("blocks = %v, want 5 layout blocks and 9 form field types", shard.Blocks)
	}
	if _, ok := shard.Blocks["title"]; ok {
		t.Error("a non-blocks field must not get a blockType list")
	}
	// blocks_source is per field, and these two fields genuinely differ.
	if shard.BlockFields["layout"].Source != SourceProjectSource {
		t.Errorf("layout source = %q", shard.BlockFields["layout"].Source)
	}
	if shard.BlockFields["fields"].Source != SourceUnionInferred {
		t.Errorf("fields source = %q", shard.BlockFields["fields"].Source)
	}
	if shard.BlocksSource != SourceMixed {
		t.Errorf("entity summary = %q, want %q when the fields disagree", shard.BlocksSource, SourceMixed)
	}
}

// TestResolveShardBlocksNoBlocksField: a collection without a blocks field
// reports nothing at all, which is different from reporting an empty list.
func TestResolveShardBlocksNoBlocksField(t *testing.T) {
	shard := NewShard("g1", "media")
	f := NewField("filename", "filename")
	f.PayloadType = TypeText
	shard.Fields = []Field{f}

	ResolveShardBlocks(shard, nil, dummyProject)

	if shard.Blocks != nil {
		t.Errorf("blocks = %v, want null", shard.Blocks)
	}
	if shard.BlockFields != nil {
		t.Errorf("block_fields = %v, want null", shard.BlockFields)
	}
	if shard.BlocksSource != SourceUnknown {
		t.Errorf("blocks_source = %q, want %q", shard.BlocksSource, SourceUnknown)
	}
}

// TestResolveShardBlocksUnresolvedKeepsBlocksNull pins the tri-state: a blocks
// field nothing could resolve leaves .blocks null rather than writing an empty
// list that reads as "accepts nothing".
func TestResolveShardBlocksUnresolvedKeepsBlocksNull(t *testing.T) {
	shard := NewShard("g1", "pages")
	f := NewField("layout", "layout")
	f.PayloadType = TypeBlocks
	shard.Fields = []Field{f}

	ResolveShardBlocks(shard, nil, dummyProject)

	if shard.Blocks != nil {
		t.Errorf("blocks = %v, want null when nothing resolved", shard.Blocks)
	}
	bf, ok := shard.BlockFieldFor("layout")
	if !ok {
		t.Fatal("the field must still be recorded, with its reason")
	}
	if bf.Source != SourceUnknown || bf.Reason == "" {
		t.Errorf("unresolved field = %+v, want unknown with a reason", bf)
	}
	if _, known := shard.BlockTypesFor("layout"); known {
		t.Error("BlockTypesFor must report unknown, not an empty allow-list")
	}
}

func TestUnionMembers(t *testing.T) {
	schema := &Schema{Types: map[string]*IntroType{
		"Page_Layout": {Kind: KindUnion, PossibleTypes: namesOf(pageLayoutUnion)},
		"Page_Meta":   {Kind: "OBJECT"},
	}}
	if got := UnionMembers(schema, strPtr("Page_Layout")); !reflect.DeepEqual(got, pageLayoutUnion) {
		t.Errorf("UnionMembers = %v", got)
	}
	// A non-union type is not a union of one; it is no union at all.
	if got := UnionMembers(schema, strPtr("Page_Meta")); got != nil {
		t.Errorf("UnionMembers(OBJECT) = %v, want nil", got)
	}
	if got := UnionMembers(schema, nil); got != nil {
		t.Errorf("UnionMembers(nil) = %v, want nil", got)
	}
	if got := UnionMembers(nil, strPtr("Page_Layout")); got != nil {
		t.Errorf("UnionMembers(no schema) = %v, want nil", got)
	}
}

func TestObservedBlockTypes(t *testing.T) {
	docs := []map[string]any{
		{
			"layout": []any{
				map[string]any{"blockType": "cta"},
				map[string]any{"blockType": "content"},
				map[string]any{"blockName": "no block type here"},
			},
			"hero": map[string]any{
				"panels": []any{map[string]any{"blockType": "banner"}},
			},
			"title": "not a blocks field",
		},
		{"layout": []any{map[string]any{"blockType": "cta"}, map[string]any{"blockType": "mediaBlock"}}},
	}
	got := ObservedBlockTypes(docs)
	if !reflect.DeepEqual(got["layout"], []string{"cta", "content", "mediaBlock"}) {
		t.Errorf("layout = %v", got["layout"])
	}
	if !reflect.DeepEqual(got["hero.panels"], []string{"banner"}) {
		t.Errorf("hero.panels = %v, want the nested path keyed dotted", got["hero.panels"])
	}
	if _, ok := got["title"]; ok {
		t.Error("a scalar field must not be reported as a blocks field")
	}
	if ObservedBlockTypes(nil) != nil {
		t.Error("no documents must yield null, not an empty map")
	}
}

func TestDescribeBlocksHelpNamesOnlySourcesThatCanAnswer(t *testing.T) {
	help := DescribeBlocksHelp([]string{"CallToActionBlock", "ContentBlock", "MediaBlock"}, "local", "layout")
	for _, want := range []string{
		"payload.config.ts",
		"src/blocks/*/config.ts",
		"slug:",
		"pay config set profiles.local.blocks.layout",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("help is missing %q:\n%s", want, help)
		}
	}
	// The help must not tell the reader to look in a document sample: §7.10
	// verified every live page has an empty layout.
	if strings.Contains(help, "pay find") {
		t.Error("the help points at a command that cannot answer it")
	}
}

func TestUnresolvedBlocksReasonQuotesTheField(t *testing.T) {
	got := UnresolvedBlocksReason("layout")
	if !strings.Contains(got, `"layout"`) {
		t.Errorf("reason = %q", got)
	}
}

func namesOf(ss []string) []IntroTypeName {
	out := make([]IntroTypeName, 0, len(ss))
	for _, s := range ss {
		out = append(out, IntroTypeName{Name: s, Kind: "OBJECT"})
	}
	return out
}
