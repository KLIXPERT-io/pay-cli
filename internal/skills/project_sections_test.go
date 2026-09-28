package skills

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

func boolp(b bool) *bool { return &b }

// fullContext is liveContext plus every optional section, shaped like the
// klixpert stage: introspection disabled, a nested block catalog with an
// orphan, no preview secret, backups on.
func fullContext() ProjectContext {
	ctx := liveContext()
	ctx.GraphQL = &GraphQLInfo{Mode: "introspection_disabled", Introspection: boolp(false), Path: "/api/graphql"}
	ctx.Blocks = &BlockCatalog{
		Cached:   true,
		Censused: 5,
		Oldest:   time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC),
		Entities: []BlockEntityInfo{
			{Kind: "global", Slug: "footer", DocsScanned: 1, TotalDocs: 1,
				Fields: []BlockFieldInfo{{Path: "columns", Rows: 3, Types: []BlockTypeCount{{"linkList", 3}}}}},
			{Kind: "collection", Slug: "pages", DocsScanned: 41, TotalDocs: 41,
				Fields: []BlockFieldInfo{
					{Path: "layout", Rows: 112, Types: []BlockTypeCount{{"hero", 17}, {"group", 75}, {"text", 20}}},
					{Path: "layout[].blocks", Rows: 80, EmptyRows: 2,
						Types: []BlockTypeCount{{"text", 59}, {"grid", 19}, {"bento", 2}}},
					{Path: "layout[].blocks[].blocks", Rows: 126, Types: []BlockTypeCount{{"text", 105}, {"bentoItem", 21}}},
				},
				Nested: []BlockNestedInfo{
					{Pattern: "bento.blocks", Types: []BlockTypeCount{{"bentoItem", 21}}},
					{Pattern: "group.blocks", Types: []BlockTypeCount{{"text", 59}, {"grid", 19}, {"bento", 2}}},
				}},
		},
	}
	ctx.Site = &SiteInfo{
		SiteURL: "http://localhost:3900", SiteURLSource: "derived:base_url",
		PreviewPath: "/next/preview?path={path}&previewSecret={secret}", PreviewPathSource: "default",
		PreviewNeedsSecret: true,
		BackupDir:          "/work/.pay-backups", BackupDirSource: "./pay.toml",
	}
	return ctx
}

// TestRenderProjectGolden pins the whole document an agent reads. Regenerate
// with UPDATE_GOLDEN=1 after a deliberate change and review the diff.
func TestRenderProjectGolden(t *testing.T) {
	got := RenderProject(fullContext())
	path := filepath.Join("testdata", "project_full.golden.md")
	if payloadtest.Updating() {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with UPDATE_GOLDEN=1 to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("PROJECT.md differs from %s (UPDATE_GOLDEN=1 to accept):\n%s", path, got)
	}
}

func TestRenderProjectOmitsSectionsItWasNotGiven(t *testing.T) {
	got := string(RenderProject(liveContext()))
	for _, absent := range []string{"## Schema discovery", "## Blocks", "## URLs, draft preview and backups"} {
		if strings.Contains(got, absent) {
			t.Errorf("a context without that data rendered %q:\n%s", absent, got)
		}
	}
}

func TestRenderSchemaDiscoveryModes(t *testing.T) {
	tests := []struct {
		mode string
		want []string
	}{
		{"ok", []string{"introspection is **available** at `/api/graphql`"}},
		{"introspection_disabled", []string{"**introspection is disabled**", "`required: null` means unknown",
			"disableIntrospectionInProduction: false", "pay blocks types | schema"}},
		{"route_missing", []string{"GraphQL is **not available** here (`route_missing`)", "block slugs and block schemas"}},
		{"unreachable", []string{"GraphQL could not be probed (`unreachable`)", "pay doctor"}},
	}
	for _, tc := range tests {
		t.Run(tc.mode, func(t *testing.T) {
			ctx := ProjectContext{GraphQL: &GraphQLInfo{Mode: tc.mode, Path: "/api/graphql"}}
			got := string(RenderProject(ctx))
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q:\n%s", w, got)
				}
			}
		})
	}
}

func TestRenderBlocksWithoutCensus(t *testing.T) {
	got := string(RenderProject(ProjectContext{Blocks: &BlockCatalog{}}))
	for _, w := range []string{"## Blocks (block census)", "No block census is cached",
		"pay blocks types", "pay skills update --project-only"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q:\n%s", w, got)
		}
	}
}

func TestRenderBlocksCatalog(t *testing.T) {
	got := string(RenderProject(fullContext()))
	for _, w := range []string{
		"oldest entity read 2026-09-16T09:00:00Z",
		"### `pages` (collection)",
		"41 of 41 documents read.",
		"| `layout` | 112 | `group` 75, `text` 20, `hero` 17 |", // sorted by count
		"| `group.blocks` | `text` 59, `grid` 19, `bento` 2 |",
		"pay blocks types pages --field PATTERN",
		"Orphan rows observed at `layout[].blocks` (2 empty, 0 without blockType)",
		"--reset-nested",
		"### `footer` (global)",
		"3 other censused collection(s)/global(s) hold no block rows.",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q:\n%s", w, got)
		}
	}
	// Collections before globals.
	if strings.Index(got, "### `pages`") > strings.Index(got, "### `footer`") {
		t.Error("collections must be listed before globals")
	}
}

func TestRenderBlocksTruncationAndCap(t *testing.T) {
	var many []BlockTypeCount
	for i := 0; i < maxTypesPerCell+5; i++ {
		many = append(many, BlockTypeCount{Slug: "t" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Count: 1})
	}
	ctx := ProjectContext{Blocks: &BlockCatalog{Cached: true, Censused: 1, Entities: []BlockEntityInfo{
		{Kind: "collection", Slug: "pages", DocsScanned: 500, TotalDocs: 1200, Truncated: true,
			Fields: []BlockFieldInfo{{Path: "layout", Rows: 35, Types: many, Authoritative: true, Source: "configured"}}},
	}}}
	got := string(RenderProject(ctx))
	for _, w := range []string{"500 of 1200 documents read (truncated: raise `--census-max-docs`)",
		"… +5 more", "complete declared list (configured)"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q:\n%s", w, got)
		}
	}
}

func TestRenderSiteStates(t *testing.T) {
	tests := []struct {
		name string
		site SiteInfo
		want []string
	}{
		{"no secret, no backups", SiteInfo{SiteURL: "https://x.dev", PreviewPath: "/p?s={secret}", PreviewNeedsSecret: true},
			[]string{"**not configured**: `pay url --preview` fails with `preview_secret_missing`",
				"| backups | **off**", "backup_dir = \".pay-backups\"", "none: `pay url` uses breadcrumbs"}},
		{"secret env named but unset", SiteInfo{SiteURL: "https://x.dev", PreviewNeedsSecret: true, PreviewSecretEnv: "PREVIEW_SECRET"},
			[]string{"`preview_secret_env` names `PREVIEW_SECRET`, which is not set"}},
		{"no secret needed", SiteInfo{SiteURL: "https://x.dev", PreviewPath: "/api/preview?path={path}"},
			[]string{"| preview secret | not needed by this route |"}},
		{"backups suppressed", SiteInfo{SiteURL: "https://x.dev", BackupDir: "/b", NoBackup: true},
			[]string{"| backups | **off** (`no_backup`"}},
		{"no site url", SiteInfo{}, []string{"not derivable"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			site := tc.site
			got := string(RenderProject(ProjectContext{Site: &site}))
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q:\n%s", w, got)
				}
			}
		})
	}
}

func TestPreviewPathForDisplayKeepsThePlaceholderOnly(t *testing.T) {
	if got := previewPathForDisplay("/next/preview?path={path}&previewSecret={secret}"); got !=
		"/next/preview?path={path}&previewSecret={secret}" {
		t.Errorf("placeholder masked: %q", got)
	}
	literal := "/next/preview?path={path}&previewSecret=hunter2-literal-value"
	if got := previewPathForDisplay(literal); strings.Contains(got, "hunter2-literal-value") {
		t.Errorf("a literal secret in the template was not redacted: %q", got)
	}
}

func TestExamplesUseTheBusiestDraftsCollectionAndBlocks(t *testing.T) {
	ctx := fullContext()
	ctx.Collections = append(ctx.Collections,
		CollectionInfo{Slug: "posts", Features: []string{"drafts"}, TotalDocs: 5})
	got := strings.Join(examples(ctx), "\n")
	for _, w := range []string{
		"pay describe pages --required-only",
		"pay find pages --published-only --limit 5",
		"pay blocks types pages",
		"pay blocks schema group --collection pages",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("examples missing %q:\n%s", w, got)
		}
	}
}
