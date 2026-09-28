package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/config"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/skills"
)

// A census shaped like the klixpert stage: a top-level layout, a group whose
// blocks hold a text and a grid, the grid's own blocks, and one `{}` orphan.
func projectCensus(t *testing.T) *discovery.Census {
	t.Helper()
	b := discovery.NewCensusBuilder(discovery.EntityKindCollection, "pages")
	b.AddDoc(1, map[string]any{"id": 1, "layout": []any{
		map[string]any{"id": "a", "blockType": "hero", "title": "Hi"},
		map[string]any{"id": "b", "blockType": "group", "blocks": []any{
			map[string]any{"id": "c", "blockType": "text", "heading": "T"},
			map[string]any{"id": "d", "blockType": "grid", "blocks": []any{
				map[string]any{"id": "e", "blockType": "text", "heading": "U"},
			}},
			map[string]any{},
		}},
	}})
	b.AddDoc(2, map[string]any{"id": 2, "layout": []any{
		map[string]any{"id": "f", "blockType": "hero", "title": "Ho"},
	}})
	pages := b.Finish()
	pages.BuiltAt = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	total := 2
	pages.TotalDocs = &total

	posts := discovery.NewCensusEntity(discovery.EntityKindCollection, "posts")
	posts.BuiltAt = time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)

	c := discovery.NewCensus("test")
	c.Put(pages)
	c.Put(posts)
	return c
}

func TestSkillsBlockCatalogFromCachedCensus(t *testing.T) {
	rt := &Runtime{}
	rt.rememberCensus(projectCensus(t))
	cat := skillsBlockCatalog(context.Background(), rt)
	if cat == nil || !cat.Cached {
		t.Fatalf("catalog = %+v, want a cached catalog", cat)
	}
	if cat.Censused != 2 || len(cat.Entities) != 1 {
		t.Fatalf("censused=%d entities=%d, want 2 censused and only pages listed", cat.Censused, len(cat.Entities))
	}
	if want := time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC); !cat.Oldest.Equal(want) {
		t.Errorf("oldest = %v, want %v (the blockless entity counts too)", cat.Oldest, want)
	}
	e := cat.Entities[0]
	if e.Slug != "pages" || e.DocsScanned != 2 || e.TotalDocs != 2 {
		t.Errorf("entity = %+v", e)
	}
	if len(e.Fields) == 0 || e.Fields[0].Path != "layout" {
		t.Fatalf("fields = %+v, want layout first (shallowest first)", e.Fields)
	}
	counts := map[string]map[string]int{}
	empty := map[string]int{}
	for _, f := range e.Fields {
		counts[f.Path] = map[string]int{}
		for _, ty := range f.Types {
			counts[f.Path][ty.Slug] = ty.Count
		}
		empty[f.Path] = f.EmptyRows
	}
	if counts["layout"]["hero"] != 2 || counts["layout"]["group"] != 1 {
		t.Errorf("layout counts = %v", counts["layout"])
	}
	if counts["layout[].blocks"]["text"] != 1 || counts["layout[].blocks"]["grid"] != 1 {
		t.Errorf("layout[].blocks counts = %v", counts["layout[].blocks"])
	}
	if counts["layout[].blocks[].blocks"]["text"] != 1 {
		t.Errorf("layout[].blocks[].blocks counts = %v", counts["layout[].blocks[].blocks"])
	}
	if empty["layout[].blocks"] != 1 {
		t.Errorf("empty rows = %v, want the {} orphan at layout[].blocks", empty)
	}
	nested := map[string]bool{}
	for _, n := range e.Nested {
		nested[n.Pattern] = true
	}
	if !nested["group.blocks"] || !nested["grid.blocks"] {
		t.Errorf("nested = %+v, want group.blocks and grid.blocks", e.Nested)
	}

	md := string(skills.RenderProject(skills.ProjectContext{Blocks: cat}))
	for _, want := range []string{"### `pages` (collection)", "| `layout` | 3 | `hero` 2, `group` 1 |",
		"| `group.blocks` |", "Orphan rows observed at `layout[].blocks` (1 empty", "1 other censused"} {
		if !strings.Contains(md, want) {
			t.Errorf("PROJECT.md is missing %q:\n%s", want, md)
		}
	}
}

func TestSkillsBlockCatalogWithoutCensus(t *testing.T) {
	cat := skillsBlockCatalog(context.Background(), &Runtime{})
	if cat == nil || cat.Cached {
		t.Fatalf("catalog = %+v, want a non-nil, not-cached catalog", cat)
	}
}

func TestSkillsSiteInfoNamesTheSecretSourceOnly(t *testing.T) {
	const secret = "paywt-preview-secret-value-1234"
	rt := &Runtime{
		Cfg: &config.Resolved{
			Profile:     "stage",
			SiteURL:     "https://www.example.com",
			PreviewPath: config.DefaultPreviewPath,
			BackupDir:   "/tmp/pay-backups",
			URLPaths:    map[string]string{"posts": "/posts/{slug}"},
			Sources: map[string]string{"site_url": "derived:base_url", "preview_path": "default",
				"backup_dir": "env:PAY_BACKUP_DIR"},
		},
		Env: config.Env{"PAY_PREVIEW_SECRET": secret},
	}
	si := skillsSiteInfo(rt)
	if si == nil || !si.PreviewNeedsSecret || si.PreviewSecretSource == "" {
		t.Fatalf("site info = %+v, want a resolved secret source", si)
	}
	md := string(skills.RenderProject(skills.ProjectContext{Site: si}))
	if strings.Contains(md, secret) {
		t.Fatalf("PROJECT.md leaked the preview secret:\n%s", md)
	}
	for _, want := range []string{"| preview secret | configured (resolves from `env:PAY_PREVIEW_SECRET`) |",
		"`posts` → `/posts/{slug}`", "| backups | on: `/tmp/pay-backups` (env:PAY_BACKUP_DIR) |",
		"previewSecret={secret}"} {
		if !strings.Contains(md, want) {
			t.Errorf("PROJECT.md is missing %q:\n%s", want, md)
		}
	}
}
