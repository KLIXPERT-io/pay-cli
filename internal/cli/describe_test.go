package cli

import (
	"path/filepath"
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
