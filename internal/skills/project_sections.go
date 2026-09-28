package skills

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

// maxTypesPerCell caps the block types listed in one PROJECT.md table cell;
// the rest are counted, so a wide field cannot turn the table unreadable.
const maxTypesPerCell = 30

// GraphQL modes as discovery reports them (internal/discovery provenance.go).
// Duplicated as strings because this package renders data and must not
// import discovery.
const (
	graphQLModeOK                    = "ok"
	graphQLModeIntrospectionDisabled = "introspection_disabled"
	graphQLModeDisabled              = "disabled"
	graphQLModeRouteMissing          = "route_missing"
)

// writeSchemaDiscovery says what discovery could learn and therefore what
// `pay describe` can be trusted for. An agent that does not know
// introspection is off reads `required: null` as "optional" and goes looking
// for block schemas in the app's source; this section is the antidote.
func writeSchemaDiscovery(b *strings.Builder, ctx ProjectContext) {
	g := ctx.GraphQL
	if g == nil {
		return
	}
	b.WriteString("## Schema discovery\n\n")
	path := orValue(g.Path, "unknown")
	switch g.Mode {
	case graphQLModeOK:
		fmt.Fprintf(b, "GraphQL introspection is **available** at `%s`: `pay describe` knows field\n", path)
		b.WriteString("types, required flags, relationship targets and the block slugs every blocks field\n")
		b.WriteString("accepts. The block census (`pay blocks types|schema`) adds observed values and counts.\n\n")
	case graphQLModeIntrospectionDisabled:
		fmt.Fprintf(b, "GraphQL answers queries at `%s`, but **introspection is disabled** (a production\n", path)
		b.WriteString("build). Discovery is REST-observed, which degrades:\n\n")
		writeDegradations(b)
		b.WriteString("To enable it, the app sets `graphQL: { disableIntrospectionInProduction: false }` in\n")
		b.WriteString("`buildConfig()` and redeploys. `pay doctor` reports the current state.\n\n")
	case graphQLModeDisabled, graphQLModeRouteMissing:
		fmt.Fprintf(b, "GraphQL is **not available** here (`%s`). Discovery is REST-observed, which degrades:\n\n", g.Mode)
		writeDegradations(b)
	default:
		fmt.Fprintf(b, "GraphQL could not be probed (`%s`), so discovery may be REST-observed only.\n", orValue(g.Mode, "unknown"))
		b.WriteString("Run `pay doctor` for the reason. Until then treat `required: null` as unknown and use\n")
		b.WriteString("`pay blocks types|schema` for anything about blocks.\n\n")
	}
}

func writeDegradations(b *strings.Builder) {
	b.WriteString("* field lists come from sampled documents: a field that was `null` there has no type;\n")
	b.WriteString("* required flags are unknown — `required: null` means unknown, never \"optional\";\n")
	b.WriteString("* block slugs and block schemas are not published at all: use the block census\n")
	b.WriteString("  (`pay blocks types | schema | example | new | validate`, catalog below), not\n")
	b.WriteString("  `pay describe`, and never the app's source code, to learn block shapes.\n\n")
}

// writeBlocks renders the block catalog from the cached census: per blocks
// field path the observed block types with row counts, and per block type the
// nested patterns (`group.blocks`) and what they hold.
func writeBlocks(b *strings.Builder, ctx ProjectContext) {
	c := ctx.Blocks
	if c == nil {
		return
	}
	b.WriteString("## Blocks (block census)\n\n")
	if !c.Cached {
		b.WriteString("No block census is cached for this profile, so this snapshot has no block catalog.\n")
		b.WriteString("Run `pay blocks types` once (it reads real documents; the cost is shown in\n")
		b.WriteString("`data.census.requests`), then `pay skills update --project-only` to include it here.\n\n")
		return
	}
	built := "unknown"
	if !c.Oldest.IsZero() {
		built = c.Oldest.UTC().Format(time.RFC3339)
	}
	fmt.Fprintf(b, "A snapshot of the cached block census (oldest entity read %s): what real documents\n", built)
	b.WriteString("contain, **not** everything the fields allow. Live: `pay blocks types <collection>`;\n")
	b.WriteString("after a deploy: `pay blocks learn`. How to edit blocks: `references/blocks.md`.\n\n")

	ents := append([]BlockEntityInfo(nil), c.Entities...)
	sort.SliceStable(ents, func(i, j int) bool {
		if ents[i].Kind != ents[j].Kind {
			return ents[i].Kind == "collection"
		}
		return ents[i].Slug < ents[j].Slug
	})
	if len(ents) == 0 {
		fmt.Fprintf(b, "The census read %d collection(s)/global(s) and found no block rows in any of them.\n\n", c.Censused)
		return
	}
	for _, e := range ents {
		writeBlockEntity(b, e)
	}
	if rest := c.Censused - len(ents); rest > 0 {
		fmt.Fprintf(b, "%d other censused collection(s)/global(s) hold no block rows.\n\n", rest)
	}
}

func writeBlockEntity(b *strings.Builder, e BlockEntityInfo) {
	kind := orValue(e.Kind, "collection")
	fmt.Fprintf(b, "### `%s` (%s)\n\n", redact.Text(e.Slug), kind)
	switch {
	case e.Error != "":
		fmt.Fprintf(b, "The census read failed part-way (%s); the catalog below is incomplete.\n\n", redact.Text(e.Error))
	case e.Truncated && e.TotalDocs >= 0:
		fmt.Fprintf(b, "%d of %d documents read (truncated: raise `--census-max-docs`).\n\n", e.DocsScanned, e.TotalDocs)
	case e.Truncated:
		fmt.Fprintf(b, "%d documents read (truncated: raise `--census-max-docs`).\n\n", e.DocsScanned)
	case e.TotalDocs >= 0:
		fmt.Fprintf(b, "%d of %d documents read.\n\n", e.DocsScanned, e.TotalDocs)
	default:
		fmt.Fprintf(b, "%d documents read.\n\n", e.DocsScanned)
	}

	b.WriteString("| field path | rows | block types (rows) |\n|---|---|---|\n")
	var orphans []string
	for _, f := range e.Fields {
		cell := typeCell(f.Types)
		if f.Authoritative {
			cell += " — complete declared list (" + orValue(f.Source, "declared") + ")"
		}
		fmt.Fprintf(b, "| `%s` | %d | %s |\n", redact.Text(f.Path), f.Rows, cell)
		if f.EmptyRows > 0 || f.UntypedRows > 0 {
			orphans = append(orphans, fmt.Sprintf("`%s` (%d empty, %d without blockType)",
				redact.Text(f.Path), f.EmptyRows, f.UntypedRows))
		}
	}
	b.WriteString("\n")
	if len(e.Nested) > 0 {
		b.WriteString("Nested blocks fields by parent type (use as `pay blocks types " + redact.Text(e.Slug) +
			" --field PATTERN`):\n\n")
		b.WriteString("| pattern | block types (rows) |\n|---|---|\n")
		for _, n := range e.Nested {
			fmt.Fprintf(b, "| `%s` | %s |\n", redact.Text(n.Pattern), typeCell(n.Types))
		}
		b.WriteString("\n")
	}
	if len(orphans) > 0 {
		b.WriteString("Orphan rows observed at " + strings.Join(orphans, ", ") + ": leftovers of nested block\n")
		b.WriteString("restructures. `pay outline <collection> <id>` flags them per document; repair with\n")
		b.WriteString("`pay update <collection> <id> --reset-nested` (keeps the id).\n\n")
	}
}

func typeCell(types []BlockTypeCount) string {
	if len(types) == 0 {
		return "—"
	}
	sorted := append([]BlockTypeCount(nil), types...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Count != sorted[j].Count {
			return sorted[i].Count > sorted[j].Count
		}
		return sorted[i].Slug < sorted[j].Slug
	})
	parts := make([]string, 0, len(sorted))
	for i, t := range sorted {
		if i == maxTypesPerCell {
			parts = append(parts, fmt.Sprintf("… +%d more", len(sorted)-maxTypesPerCell))
			break
		}
		parts = append(parts, fmt.Sprintf("`%s` %d", redact.Text(t.Slug), t.Count))
	}
	return strings.Join(parts, ", ")
}

// writeSite renders the frontend URL, draft-preview and backup configuration
// the profile resolves to. It names where the preview secret comes from and
// never the secret itself.
func writeSite(b *strings.Builder, ctx ProjectContext) {
	s := ctx.Site
	if s == nil {
		return
	}
	b.WriteString("## URLs, draft preview and backups\n\n")
	b.WriteString("| | |\n|---|---|\n")
	if s.SiteURL == "" {
		b.WriteString("| site URL | not derivable: `pay url` cannot build URLs until `site_url` is set |\n")
	} else {
		fmt.Fprintf(b, "| site URL | `%s` (%s) |\n", redact.URL(s.SiteURL), orValue(s.SiteURLSource, "unknown"))
	}
	if len(s.URLPaths) == 0 {
		b.WriteString("| URL templates | none: `pay url` uses breadcrumbs, else `/{slug}` (a guess outside `pages`) |\n")
	} else {
		keys := make([]string, 0, len(s.URLPaths))
		for k := range s.URLPaths {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("`%s` → `%s`", redact.Text(k), redact.Text(s.URLPaths[k])))
		}
		fmt.Fprintf(b, "| URL templates | %s |\n", strings.Join(parts, ", "))
	}
	if s.PreviewPath != "" {
		fmt.Fprintf(b, "| preview route | `%s` (%s) |\n", previewPathForDisplay(s.PreviewPath), orValue(s.PreviewPathSource, "default"))
	}
	switch {
	case !s.PreviewNeedsSecret:
		b.WriteString("| preview secret | not needed by this route |\n")
	case s.PreviewSecretSource != "":
		fmt.Fprintf(b, "| preview secret | configured (resolves from `%s`) |\n", redact.Text(s.PreviewSecretSource))
	case s.PreviewSecretEnv != "":
		fmt.Fprintf(b, "| preview secret | **missing**: `preview_secret_env` names `%s`, which is not set in this environment |\n",
			redact.Text(s.PreviewSecretEnv))
	default:
		b.WriteString("| preview secret | **not configured**: `pay url --preview` fails with `preview_secret_missing` |\n")
	}
	switch {
	case s.NoBackup:
		b.WriteString("| backups | **off** (`no_backup` / `PAY_NO_BACKUP` is set) |\n")
	case s.BackupDir != "":
		fmt.Fprintf(b, "| backups | on: `%s` (%s) |\n", redact.Text(s.BackupDir), orValue(s.BackupDirSource, "unknown"))
	default:
		b.WriteString("| backups | **off**: no `PAY_BACKUP_DIR` / `backup_dir` configured |\n")
	}
	b.WriteString("\n")
	b.WriteString("`pay url <collection> <id>` prints a document's site URL; `--preview` mints draft-preview\n")
	b.WriteString("cookies so a browser shows unpublished drafts (see `references/migration.md` §6).\n")
	if s.BackupDir == "" || s.NoBackup {
		b.WriteString("Turn backups on before migration writes: `backup_dir = \".pay-backups\"` in `./pay.toml`\n")
		b.WriteString("(or `export PAY_BACKUP_DIR=…`); every write then saves what it replaces, restorable with\n")
		b.WriteString("`pay backups restore <file>`.\n")
	} else {
		b.WriteString("Every write that changes an existing document saves it first (`meta.backups`);\n")
		b.WriteString("undo with `pay backups restore <file>`.\n")
	}
	b.WriteString("\n")
}

// previewPathForDisplay redacts a preview route template without masking its
// `{secret}` placeholder, which is documentation, not a secret: each part
// around the placeholder is redacted on its own, so a literal secret someone
// pasted into the template is still masked.
func previewPathForDisplay(path string) string {
	parts := strings.Split(path, "{secret}")
	for i, p := range parts {
		parts[i] = redact.Text(p)
	}
	return strings.Join(parts, "{secret}")
}

// blockExamples adds block commands built from the catalog's real slugs: the
// busiest entity and its most frequent top-level block type.
func blockExamples(c *BlockCatalog) []string {
	if c == nil || !c.Cached || len(c.Entities) == 0 {
		return nil
	}
	var best *BlockEntityInfo
	bestRows := -1
	for i := range c.Entities {
		rows := 0
		for _, f := range c.Entities[i].Fields {
			rows += f.Rows
		}
		if rows > bestRows {
			best, bestRows = &c.Entities[i], rows
		}
	}
	out := []string{fmt.Sprintf("pay blocks types %s   # block types per field path, live", best.Slug)}
	// Fields are ordered shallowest first, so Fields[0] is a top-level field.
	var top BlockTypeCount
	if len(best.Fields) > 0 {
		for _, t := range best.Fields[0].Types {
			if top.Slug == "" || t.Count > top.Count || (t.Count == top.Count && t.Slug < top.Slug) {
				top = t
			}
		}
	}
	if top.Slug != "" {
		out = append(out, fmt.Sprintf("pay blocks schema %s --collection %s   # what is inside one", top.Slug, best.Slug))
	}
	return out
}
