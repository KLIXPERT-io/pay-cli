package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// seedTwoBlockFields caches a pages shard carrying TWO blocks fields that
// resolved differently, which is the shape the live project has across
// collections: Page_Layout's five project-source blocks next to Form_Fields'
// nine plugin-provided ones.
func seedTwoBlockFields(t *testing.T, home, baseURL string) cache.Scope {
	t.Helper()
	sc, err := cache.NewScope(cache.ScopeInput{
		BaseURL:        baseURL,
		APIPath:        "/api",
		GraphQLPath:    "/api/graphql",
		KeyFingerprint: cache.AnonKeyFingerprint,
	})
	if err != nil {
		t.Fatalf("scope: %v", err)
	}

	var m discovery.Manifest
	payloadtest.LoadJSON(t, "manifest", &m)
	var shard discovery.Shard
	payloadtest.LoadJSON(t, "fields_pages", &shard)

	extra := discovery.NewField("fields", "fields")
	extra.PayloadType = discovery.TypeBlocks
	shard.Fields = append(shard.Fields, extra)

	shard.Blocks, shard.BlockFields, shard.BlocksSource = nil, nil, discovery.SourceUnknown
	shard.SetBlockField(discovery.BlockField{
		Path:           "layout",
		Slugs:          []string{"archive", "content", "cta", "formBlock", "mediaBlock"},
		Source:         discovery.SourceProjectSource,
		InterfaceNames: []string{"ArchiveBlock", "CallToActionBlock", "ContentBlock", "FormBlock", "MediaBlock"},
		SlugSources: map[string]string{
			"archive": discovery.SourceProjectSource, "content": discovery.SourceProjectSource,
			"cta": discovery.SourceProjectSource, "formBlock": discovery.SourceProjectSource,
			"mediaBlock": discovery.SourceProjectSource,
		},
	})
	shard.SetBlockField(discovery.BlockField{
		Path:           "fields",
		Slugs:          []string{"email", "text"},
		Source:         discovery.SourceUnionInferred,
		InterfaceNames: []string{"Email", "Text"},
		SlugSources: map[string]string{
			"email": discovery.SourceUnionInferred, "text": discovery.SourceUnionInferred,
		},
		Reason: "email, text in \"fields\" were inferred from the GraphQL interfaceName",
	})

	now := payloadtest.Epoch
	generation := cache.NewGeneration(now)
	m.Generation = generation
	m.Meta.Scope = sc.Key
	m.GeneratedAt = now
	m.ExpiresAt = now.Add(24 * time.Hour)
	shard.Generation = generation

	store := cache.New(filepath.Join(home, "cache"))
	ok, warns := store.WriteSet(sc, cache.Set{
		Generation: generation,
		Manifest:   &m,
		Shards:     map[string]any{cache.ShardName(shard.Slug, cache.KindCollection): &shard},
	}, now)
	if !ok {
		t.Fatalf("seed cache write failed: %v", warns)
	}
	return sc
}

// TestDescribeBlocksSourceIsPerField is the regression test for the reported
// bug: `pay describe pages` published ONE blocks_source string for the whole
// entity, so a field resolved by heuristic was reported with the provenance of
// an unrelated field that happened to be resolved from project source.
func TestDescribeBlocksSourceIsPerField(t *testing.T) {
	home := t.TempDir()
	seedTwoBlockFields(t, home, testBaseURL)

	res := cliRun(t, invocation{
		Home: home,
		Args: []string{"describe", "pages"},
		Env:  seededEnv(testBaseURL),
	})
	data := res.data(t)

	sources, ok := data["blocks_source"].(map[string]any)
	if !ok {
		t.Fatalf("blocks_source = %#v, want an object keyed by field path", data["blocks_source"])
	}
	if sources["layout"] != discovery.SourceProjectSource {
		t.Errorf("blocks_source.layout = %v, want %q", sources["layout"], discovery.SourceProjectSource)
	}
	if sources["fields"] != discovery.SourceUnionInferred {
		t.Errorf("blocks_source.fields = %v, want %q", sources["fields"], discovery.SourceUnionInferred)
	}

	blocks, ok := data["blocks"].(map[string]any)
	if !ok {
		t.Fatalf("blocks = %#v", data["blocks"])
	}
	layout, _ := blocks["layout"].([]any)
	fields, _ := blocks["fields"].([]any)
	if len(layout) != 5 || len(fields) != 2 {
		t.Fatalf("blocks = %v, want each field its own list", blocks)
	}
	for _, got := range fields {
		for _, pageBlock := range layout {
			if got == pageBlock {
				t.Errorf("the second blocks field inherited %v from the first", got)
			}
		}
	}
	if _, ok := data["block_fields"].(map[string]any); !ok {
		t.Errorf("block_fields = %#v, want the full per-field record", data["block_fields"])
	}
}

// TestDescribeFieldReportsThatFieldsOwnBlockTypes pins the --field view: it
// must answer about the field named on the command line and nothing else.
func TestDescribeFieldReportsThatFieldsOwnBlockTypes(t *testing.T) {
	home := t.TempDir()
	seedTwoBlockFields(t, home, testBaseURL)

	layout := cliRun(t, invocation{
		Home: home,
		Args: []string{"describe", "pages", "--field", "layout"},
		Env:  seededEnv(testBaseURL),
	}).data(t)
	if got := layout["blocks_source"]; got != discovery.SourceProjectSource {
		t.Errorf("layout blocks_source = %v, want %q", got, discovery.SourceProjectSource)
	}
	if got, _ := layout["block_types"].([]any); len(got) != 5 {
		t.Errorf("layout block_types = %v", layout["block_types"])
	}
	if got := layout["blocks_reason"]; got != "" {
		t.Errorf("a confirmed field must carry no caveat, got %v", got)
	}

	fields := cliRun(t, invocation{
		Home: home,
		Args: []string{"describe", "pages", "--field", "fields"},
		Env:  seededEnv(testBaseURL),
	})
	data := fields.data(t)
	if got := data["blocks_source"]; got != discovery.SourceUnionInferred {
		t.Errorf("fields blocks_source = %v, want %q — not the neighbouring field's provenance",
			got, discovery.SourceUnionInferred)
	}
	got, _ := data["block_types"].([]any)
	if len(got) != 2 {
		t.Fatalf("fields block_types = %v", data["block_types"])
	}
	for _, v := range got {
		if v == "cta" || v == "mediaBlock" {
			t.Errorf("fields was told it accepts %v, which belongs to layout", v)
		}
	}
	if data["blocks_reason"] == "" {
		t.Error("an inferred list must state why it is not confirmed")
	}
	// The caveat must reach the agent as a warning, not only as a data key.
	warnings, _ := fields.Env["warnings"].([]any)
	if len(warnings) == 0 {
		t.Error("an inferred blockType list produced no warning")
	}
}

// TestDescribeWithoutBlocksFieldReportsNone: a collection with no blocks field
// must say null, which is different from an empty list.
func TestDescribeWithoutBlocksFieldReportsNone(t *testing.T) {
	home := t.TempDir()
	seedDiscovery(t, home, testBaseURL)

	// The stock pages fixture has a layout field but no resolved blocks.
	data := cliRun(t, invocation{
		Home: home,
		Args: []string{"describe", "pages"},
		Env:  seededEnv(testBaseURL),
	}).data(t)

	if data["blocks"] != nil {
		t.Errorf("blocks = %#v, want null when nothing resolved", data["blocks"])
	}
	if data["blocks_source"] != nil {
		t.Errorf("blocks_source = %#v, want null rather than a confident source", data["blocks_source"])
	}
	if data["blocks_source_summary"] != discovery.SourceUnknown {
		t.Errorf("blocks_source_summary = %#v, want %q", data["blocks_source_summary"], discovery.SourceUnknown)
	}
}

// seedBlockSchemas caches a pages shard whose layout field resolves two block
// types AND carries their interiors: mediaBlock, whose required-ness came from
// the project's own config.ts, and textarea, a plugin block with one field
// proved required by a GraphQL NON_NULL and the rest unknown.
func seedBlockSchemas(t *testing.T, home, baseURL string) cache.Scope {
	t.Helper()
	sc := seedTwoBlockFields(t, home, baseURL)

	store := cache.New(filepath.Join(home, "cache"))
	cm, ok, _ := store.ReadManifest(sc)
	if !ok {
		t.Fatal("seeded manifest is unreadable")
	}
	var m discovery.Manifest
	if err := json.Unmarshal(cm.Raw, &m); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	raw, ok, _ := store.ReadShard(sc, cm, "pages", cache.KindCollection)
	if !ok {
		t.Fatal("seeded shard is unreadable")
	}
	var shard discovery.Shard
	if err := json.Unmarshal(raw.Raw, &shard); err != nil {
		t.Fatalf("shard: %v", err)
	}

	yes, no := true, false
	shard.BlockSchemas = map[string]discovery.BlockTypeSchema{
		"mediaBlock": {
			Slug: "mediaBlock", InterfaceName: "MediaBlock",
			FieldsSource: discovery.SourceGraphQL, RequiredSource: discovery.SourceProjectSource,
			ConfigFile: "/p/src/blocks/MediaBlock/config.ts", RequiredUnknown: []string{},
			PlumbingNote: discovery.BlockPlumbingNote,
			Fields: []discovery.BlockFieldSchema{
				{Name: "media", Path: "media", PayloadType: discovery.TypeUpload,
					Required: &yes, RequiredSource: discovery.SourceProjectSource,
					RelationTo: []string{"media"}, RelationToSource: discovery.SourceGraphQL,
					WriteShape: strPtrTest(discovery.WriteShapeID)},
				{Name: "blockName", Path: "blockName", PayloadType: discovery.TypeText,
					Required: &no, RequiredSource: discovery.SourcePayloadProtocol, Plumbing: true},
				{Name: "blockType", Path: "blockType", PayloadType: discovery.TypeText,
					Required: &yes, RequiredSource: discovery.SourcePayloadProtocol, Plumbing: true},
			},
		},
		"text": {
			Slug: "text", InterfaceName: "Text",
			FieldsSource: discovery.SourceGraphQL, RequiredSource: discovery.SourceMixed,
			RequiredUnknown: []string{"label"}, PlumbingNote: discovery.BlockPlumbingNote,
			Reason: "required-ness for label is unknown: no project source declares blockType \"text\" " +
				"(a plugin-provided block lives in node_modules, which is never scanned)",
			Fields: []discovery.BlockFieldSchema{
				{Name: "name", Path: "name", PayloadType: discovery.TypeText,
					Required: &yes, RequiredSource: discovery.SourceGraphQL},
				{Name: "label", Path: "label", PayloadType: discovery.TypeText,
					RequiredSource: discovery.SourceUnknown},
			},
		},
	}
	shard.Finalize()

	for i := range m.Collections {
		if m.Collections[i].Slug == "pages" {
			m.Collections[i].FieldsSHA256 = shard.SHA256
		}
	}
	gen := m.Generation
	shard.Generation = gen
	ok, warns := store.WriteSet(sc, cache.Set{
		Generation: gen,
		Manifest:   &m,
		Shards:     map[string]any{cache.ShardName("pages", cache.KindCollection): &shard},
	}, payloadtest.Epoch)
	if !ok {
		t.Fatalf("re-seed failed: %v", warns)
	}
	return sc
}

func strPtrTest(s string) *string { return &s }

// TestDescribeBlockPrintsOneBlocksInterior is the whole point of --block: a
// slug tells an agent a block EXISTS, and only its interior lets the agent
// construct one.
func TestDescribeBlockPrintsOneBlocksInterior(t *testing.T) {
	home := t.TempDir()
	seedBlockSchemas(t, home, testBaseURL)

	data := cliRun(t, invocation{
		Home: home,
		Args: []string{"describe", "pages", "--block", "mediaBlock"},
		Env:  seededEnv(testBaseURL),
	}).data(t)

	block, ok := data["block"].(map[string]any)
	if !ok {
		t.Fatalf("block = %#v, want the block's schema", data["block"])
	}
	if block["slug"] != "mediaBlock" || block["interface_name"] != "MediaBlock" {
		t.Errorf("block = %v, want the slug to write and the interfaceName it came from", block)
	}
	fields, _ := block["fields"].([]any)
	if len(fields) != 3 {
		t.Fatalf("block.fields = %v", fields)
	}
	media, _ := fields[0].(map[string]any)
	if media["required"] != true || media["required_source"] != discovery.SourceProjectSource {
		t.Errorf("media = %v, want required true from project source", media)
	}
	rel, _ := media["relation_to"].([]any)
	if len(rel) != 1 || rel[0] != "media" {
		t.Errorf("media.relation_to = %v, want [media]: without the target an agent cannot know what id to send", rel)
	}
	// Which field(s) of this entity actually accept the block.
	if paths, _ := data["block_fields"].([]any); len(paths) != 1 || paths[0] != "layout" {
		t.Errorf("block_fields = %v, want [layout]", data["block_fields"])
	}
	if req, _ := data["required_fields"].([]any); len(req) != 1 || req[0] != "media" {
		t.Errorf("required_fields = %v, want [media]", data["required_fields"])
	}
	// The generated write must carry the blockType and the required field, and
	// must be a dry run.
	examples, _ := data["examples"].([]any)
	joined := ""
	for _, e := range examples {
		joined += e.(string) + "\n"
	}
	for _, want := range []string{`"blockType":"mediaBlock"`, `"media":"<media id>"`, "--dry-run"} {
		if !strings.Contains(joined, want) {
			t.Errorf("examples %q do not contain %q", joined, want)
		}
	}
}

// TestDescribeBlockMarksPlumbing: blockName/blockType are Payload's keys, and
// an agent that treats them as content writes nonsense.
func TestDescribeBlockMarksPlumbing(t *testing.T) {
	home := t.TempDir()
	seedBlockSchemas(t, home, testBaseURL)

	data := cliRun(t, invocation{
		Home: home,
		Args: []string{"describe", "pages", "--block", "mediaBlock"},
		Env:  seededEnv(testBaseURL),
	}).data(t)

	block, _ := data["block"].(map[string]any)
	fields, _ := block["fields"].([]any)
	for _, raw := range fields {
		f, _ := raw.(map[string]any)
		switch f["name"] {
		case "blockName", "blockType":
			if f["plumbing"] != true {
				t.Errorf("%v.plumbing = %v, want true", f["name"], f["plumbing"])
			}
		case "media":
			if f["plumbing"] != false {
				t.Errorf("media.plumbing = %v, want false — it is the block's content", f["plumbing"])
			}
		}
	}
	if note, _ := data["plumbing_note"].(string); !strings.Contains(note, "blockType is MANDATORY") {
		t.Errorf("plumbing_note = %q, want it to say blockType must be sent", note)
	}
	// content_fields is the plumbing-free list an agent fills in.
	content, _ := data["content_fields"].([]any)
	if len(content) != 1 || content[0] != "media" {
		t.Errorf("content_fields = %v, want [media]", content)
	}
}

// TestDescribeBlockWarnsWhenRequirednessIsUnknown: "not listed as required"
// and "not required" are the two readings an agent must not confuse, so the
// gap is a warning and not only a data key.
func TestDescribeBlockWarnsWhenRequirednessIsUnknown(t *testing.T) {
	home := t.TempDir()
	seedBlockSchemas(t, home, testBaseURL)

	res := cliRun(t, invocation{
		Home: home,
		Args: []string{"describe", "pages", "--block", "text"},
		Env:  seededEnv(testBaseURL),
	})
	data := res.data(t)
	block, _ := data["block"].(map[string]any)
	unknown, _ := block["required_unknown"].([]any)
	if len(unknown) != 1 || unknown[0] != "label" {
		t.Errorf("required_unknown = %v, want [label]", unknown)
	}
	if block["reason"] == "" {
		t.Error("a block with unknown required-ness must say why")
	}
	warnings, _ := res.Env["warnings"].([]any)
	found := false
	for _, w := range warnings {
		if m, _ := w.(map[string]any); m["code"] == "block_required_unknown" {
			found = true
		}
	}
	if !found {
		t.Errorf("no block_required_unknown warning in %v", warnings)
	}
}

// TestDescribeBlockRejectsASlugThisEntityDoesNotAccept: a blockType is only
// writable where a blocks field accepts it, and answering for the wrong entity
// would produce a document Payload silently drops (§9.7).
func TestDescribeBlockRejectsASlugThisEntityDoesNotAccept(t *testing.T) {
	home := t.TempDir()
	seedBlockSchemas(t, home, testBaseURL)

	res := cliRun(t, invocation{
		Home: home,
		Args: []string{"describe", "pages", "--block", "mediaBlok"},
		Env:  seededEnv(testBaseURL),
	})
	if res.Code == 0 {
		t.Fatal("an unknown blockType exited 0")
	}
	errObj, _ := res.Env["error"].(map[string]any)
	if errObj["code"] != "invalid_option" {
		t.Errorf("error code = %v, want invalid_option", errObj["code"])
	}
	dym, _ := errObj["did_you_mean"].([]any)
	if len(dym) == 0 || dym[0] != "mediaBlock" {
		t.Errorf("did_you_mean = %v, want mediaBlock", dym)
	}
}

// TestDescribeBlockScopedToOneField: the same slug can be absent from a
// sibling blocks field, and --field narrows the question to the field being
// written.
func TestDescribeBlockScopedToOneField(t *testing.T) {
	home := t.TempDir()
	seedBlockSchemas(t, home, testBaseURL)

	res := cliRun(t, invocation{
		Home: home,
		Args: []string{"describe", "pages", "--block", "mediaBlock", "--field", "fields"},
		Env:  seededEnv(testBaseURL),
	})
	if res.Code == 0 {
		t.Fatal("pages.fields does not accept mediaBlock, but --block answered anyway")
	}
	ok := cliRun(t, invocation{
		Home: home,
		Args: []string{"describe", "pages", "--block", "mediaBlock", "--field", "layout"},
		Env:  seededEnv(testBaseURL),
	})
	if ok.Code != 0 {
		t.Fatalf("pages.layout accepts mediaBlock but --field layout failed: %s", ok.Stderr)
	}
}

// TestDescribeOmitsBlockInteriorsByDefault is the size contract: the interiors
// are cached and reachable, but inlining them would multiply the output of the
// most-run discovery command.
func TestDescribeOmitsBlockInteriorsByDefault(t *testing.T) {
	home := t.TempDir()
	seedBlockSchemas(t, home, testBaseURL)

	plain := cliRun(t, invocation{
		Home: home,
		Args: []string{"describe", "pages"},
		Env:  seededEnv(testBaseURL),
	}).data(t)
	if plain["block_schemas"] != nil {
		t.Errorf("block_schemas = %#v, want null without --blocks-detail", plain["block_schemas"])
	}
	available, _ := plain["block_schemas_available"].([]any)
	if len(available) != 2 {
		t.Errorf("block_schemas_available = %v, want both cached block types named", available)
	}
	hint, _ := plain["block_schemas_hint"].(string)
	if !strings.Contains(hint, "--blocks-detail") || !strings.Contains(hint, "--block") {
		t.Errorf("hint = %q, want it to name both ways to get the interiors", hint)
	}

	detailed := cliRun(t, invocation{
		Home: home,
		Args: []string{"describe", "pages", "--blocks-detail"},
		Env:  seededEnv(testBaseURL),
	}).data(t)
	schemas, ok := detailed["block_schemas"].(map[string]any)
	if !ok || len(schemas) != 2 {
		t.Fatalf("block_schemas = %#v, want both interiors inlined", detailed["block_schemas"])
	}
	if _, ok := schemas["mediaBlock"]; !ok {
		t.Error("block_schemas is not keyed by the writable slug")
	}
	if detailed["block_schemas_hint"] != nil {
		t.Error("--blocks-detail still printed the hint for getting the detail")
	}
}

// TestDescribeFieldBlocksDetailIsScopedToThatField: pages.layout and
// pages.fields accept different blocks, so --field + --blocks-detail must not
// inline the neighbour's.
func TestDescribeFieldBlocksDetailIsScopedToThatField(t *testing.T) {
	home := t.TempDir()
	seedBlockSchemas(t, home, testBaseURL)

	data := cliRun(t, invocation{
		Home: home,
		Args: []string{"describe", "pages", "--field", "fields", "--blocks-detail"},
		Env:  seededEnv(testBaseURL),
	}).data(t)

	schemas, ok := data["block_schemas"].(map[string]any)
	if !ok {
		t.Fatalf("block_schemas = %#v", data["block_schemas"])
	}
	if _, wrong := schemas["mediaBlock"]; wrong {
		t.Error("pages.fields was given layout's mediaBlock interior")
	}
	if _, want := schemas["text"]; !want {
		t.Errorf("block_schemas = %v, want this field's own text block", schemas)
	}
}
