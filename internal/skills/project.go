package skills

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

// CollectionInfo is one collection's row in PROJECT.md.
type CollectionInfo struct {
	Slug      string   `json:"slug"`
	Label     string   `json:"label,omitempty"`
	Singular  string   `json:"singular,omitempty"`
	IDType    string   `json:"id_type,omitempty"`
	Ops       []string `json:"ops,omitempty"`      // create read update delete
	Features  []string `json:"features,omitempty"` // drafts versions upload trash auth folders
	KeyFields []string `json:"key_fields,omitempty"`
	Internal  bool     `json:"internal,omitempty"`
	// TotalDocs is negative when unknown.
	TotalDocs int `json:"total_docs,omitempty"`
}

// HasFeature reports whether the collection advertises a capability.
func (c CollectionInfo) HasFeature(name string) bool {
	for _, f := range c.Features {
		if strings.EqualFold(f, name) {
			return true
		}
	}
	return false
}

// GlobalInfo is one global's row in PROJECT.md.
type GlobalInfo struct {
	Slug     string   `json:"slug"`
	Label    string   `json:"label,omitempty"`
	Features []string `json:"features,omitempty"`
}

// GraphQLInfo is discovery's GraphQL ladder result (§7.6), as PROJECT.md
// reports it: whether introspection answered, and so what `pay describe` can
// and cannot know on this project.
type GraphQLInfo struct {
	// Mode is discovery's mode string: "ok", "introspection_disabled",
	// "disabled", "route_missing", "unreachable" or "errors".
	Mode string
	// Introspection is tri-state: nil when discovery learned nothing.
	Introspection *bool
	// Path is the GraphQL endpoint path discovery probed.
	Path string
}

// BlockTypeCount is one blockType and how many rows of it the census saw.
type BlockTypeCount struct {
	Slug  string
	Count int
}

// BlockFieldInfo is one blocks-field path pattern of an entity (`layout`,
// `layout[].blocks`) with the types observed there.
type BlockFieldInfo struct {
	Path  string
	Types []BlockTypeCount
	Rows  int
	Docs  int
	// EmptyRows are `{}` rows (orphans of a nested restructure); UntypedRows
	// are non-empty rows without a blockType.
	EmptyRows   int
	UntypedRows int
	// Authoritative is true only when a declared source enumerates the list.
	Authoritative bool
	Source        string
}

// BlockNestedInfo is a parent-scoped pattern (`group.blocks`): what rows of
// one block type hold in one of their blocks fields.
type BlockNestedInfo struct {
	Pattern string
	Types   []BlockTypeCount
}

// BlockEntityInfo is one collection's or global's block catalog.
type BlockEntityInfo struct {
	Kind        string // "collection" | "global"
	Slug        string
	BuiltAt     time.Time
	DocsScanned int
	// TotalDocs is negative when unknown.
	TotalDocs int
	Truncated bool
	Error     string
	Fields    []BlockFieldInfo
	Nested    []BlockNestedInfo
}

// BlockCatalog is PROJECT.md's view of the CACHED block census (§7.10c). It is
// never built for PROJECT.md: a nil catalog means the caller did not look, a
// catalog with Cached false means no census is cached for this profile.
type BlockCatalog struct {
	Cached bool
	// Entities holds only entities whose documents contain block rows.
	Entities []BlockEntityInfo
	// Censused counts every entity the census read, with or without blocks.
	Censused int
	// Oldest is the build time of the oldest entity census.
	Oldest time.Time
}

// SiteInfo is the profile's frontend-URL, draft-preview and backup state as
// `pay doctor` reports it. It never carries the preview secret: only where it
// resolves from.
type SiteInfo struct {
	SiteURL           string
	SiteURLSource     string
	URLPaths          map[string]string
	PreviewPath       string
	PreviewPathSource string
	// PreviewNeedsSecret is true when PreviewPath contains {secret}.
	PreviewNeedsSecret bool
	// PreviewSecretSource is where the secret resolves from ("" = none).
	PreviewSecretSource string
	// PreviewSecretEnv is the profile's preview_secret_env (a variable NAME).
	PreviewSecretEnv string
	BackupDir        string
	BackupDirSource  string
	NoBackup         bool
}

// ProjectContext is everything PROJECT.md is rendered from. It carries no
// credential of any kind: the auth header is documented as a form, never with
// a value, and BaseURL is redacted on render (§14 Privacy).
type ProjectContext struct {
	CLIVersion        string
	GeneratedAt       time.Time
	Profile           string
	BaseURL           string
	APIPath           string
	DiscoveryRevision string
	PayloadVersion    string
	AuthCollection    string
	AuthMode          string
	Locales           []string
	DefaultLocale     string
	Collections       []CollectionInfo
	Globals           []GlobalInfo
	// GraphQL, Blocks and Site are optional: nil omits their section.
	GraphQL *GraphQLInfo
	Blocks  *BlockCatalog
	Site    *SiteInfo
	// Examples overrides the generated example commands.
	Examples []string
}

// RenderProject renders references/PROJECT.md.
//
// §14 requires the header comment, the snapshot disclaimer, the collection
// table, the globals, the auth collection slug with the exact header form, and
// three examples written against real slugs.
func RenderProject(ctx ProjectContext) []byte {
	var b strings.Builder

	base := redact.URL(strings.TrimSpace(ctx.BaseURL))
	profile := redact.Text(ctx.Profile)

	fmt.Fprintf(&b, "<!-- generated by pay %s on %s from profile %q\n",
		version(ctx.CLIVersion), ctx.GeneratedAt.UTC().Format(time.RFC3339), profile)
	fmt.Fprintf(&b, "     base_url=%s discovery_revision=%s -->\n", base, orDash(ctx.DiscoveryRevision))
	b.WriteString("This is a SNAPSHOT and may be stale. `pay explain` is always live - prefer it\n")
	b.WriteString("when anything here does not match reality. Refresh: `pay skills update --project-only`\n\n")

	b.WriteString("# This Payload project\n\n")
	b.WriteString("| | |\n|---|---|\n")
	fmt.Fprintf(&b, "| base URL | `%s` |\n", base)
	fmt.Fprintf(&b, "| API path | `%s` |\n", orValue(ctx.APIPath, "/api"))
	fmt.Fprintf(&b, "| profile | `%s` |\n", orDash(profile))
	fmt.Fprintf(&b, "| Payload version | %s |\n", orValue(ctx.PayloadVersion, "unknown"))
	fmt.Fprintf(&b, "| auth collection | `%s` |\n", orDash(redact.Text(ctx.AuthCollection)))
	fmt.Fprintf(&b, "| auth mode | `%s` |\n", orDash(ctx.AuthMode))
	if len(ctx.Locales) > 0 {
		fmt.Fprintf(&b, "| locales | %s |\n", "`"+strings.Join(ctx.Locales, "`, `")+"`")
		if ctx.DefaultLocale != "" {
			fmt.Fprintf(&b, "| default locale | `%s` |\n", ctx.DefaultLocale)
		}
	}
	b.WriteString("\n")

	writeAuth(&b, ctx)
	writeSchemaDiscovery(&b, ctx)
	writeCollections(&b, ctx)
	writeGlobals(&b, ctx)
	writeBlocks(&b, ctx)
	writeSite(&b, ctx)
	writeExamples(&b, ctx)

	return []byte(b.String())
}

func writeAuth(b *strings.Builder, ctx ProjectContext) {
	auth := strings.TrimSpace(ctx.AuthCollection)
	b.WriteString("## Authentication\n\n")
	if auth == "" {
		b.WriteString("The auth collection for this project was not determined. Run `pay doctor` to\n")
		b.WriteString("find it; the header form below needs its slug.\n\n")
		auth = "<authCollection>"
	} else {
		fmt.Fprintf(b, "The auth collection is **`%s`**. The API-key header embeds that slug, and a\n", redact.Text(auth))
		b.WriteString("wrong slug silently degrades you to the anonymous view instead of failing:\n\n")
	}
	fmt.Fprintf(b, "```\nAuthorization: %s API-Key <your key>\n```\n\n", redact.Text(auth))
	b.WriteString("`pay` builds this header for you from the profile's stored credential. The key\n")
	b.WriteString("itself is never written to this file, to the audit log, or to any output.\n")
	b.WriteString("Verify identity with `pay whoami` — a wrong key returns HTTP 200 with a null user.\n\n")
}

func writeCollections(b *strings.Builder, ctx ProjectContext) {
	cols := append([]CollectionInfo(nil), ctx.Collections...)
	// Non-internal first, then most documents, then alphabetical: the order an
	// agent asking "what can I do here?" needs.
	sort.SliceStable(cols, func(i, j int) bool {
		if cols[i].Internal != cols[j].Internal {
			return !cols[i].Internal
		}
		if cols[i].TotalDocs != cols[j].TotalDocs {
			return cols[i].TotalDocs > cols[j].TotalDocs
		}
		return cols[i].Slug < cols[j].Slug
	})

	fmt.Fprintf(b, "## Collections (%d)\n\n", len(cols))
	if len(cols) == 0 {
		b.WriteString("Discovery returned no collections. Run `pay doctor`.\n\n")
		return
	}
	b.WriteString("| slug | ops | features | key fields | id |\n|---|---|---|---|---|\n")
	for _, c := range cols {
		slug := "`" + redact.Text(c.Slug) + "`"
		if c.Internal {
			slug += " *(internal)*"
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s |\n",
			slug,
			orDash(strings.Join(c.Ops, " ")),
			orDash(strings.Join(c.Features, " ")),
			codeList(c.KeyFields),
			orValue(c.IDType, "unknown"))
	}
	b.WriteString("\nFeatures decide what is legal: `drafts` means `--draft`/`--published-only` apply,\n")
	b.WriteString("`versions` means `pay versions` works, `trash` means `pay delete` is a soft delete\n")
	b.WriteString("and `pay restore` exists, `upload` means `pay upload`/`pay download` work.\n")
	b.WriteString("Collections marked *(internal)* are Payload's own bookkeeping; they are reachable\n")
	b.WriteString("but rarely what you want.\n\n")
}

func writeGlobals(b *strings.Builder, ctx ProjectContext) {
	fmt.Fprintf(b, "## Globals (%d)\n\n", len(ctx.Globals))
	if len(ctx.Globals) == 0 {
		b.WriteString("This project has no globals.\n\n")
		return
	}
	globals := append([]GlobalInfo(nil), ctx.Globals...)
	sort.Slice(globals, func(i, j int) bool { return globals[i].Slug < globals[j].Slug })
	b.WriteString("| slug | features |\n|---|---|\n")
	for _, g := range globals {
		fmt.Fprintf(b, "| `%s` | %s |\n", redact.Text(g.Slug), orDash(strings.Join(g.Features, " ")))
	}
	b.WriteString("\nA global is a single document: `pay globals get <slug>` and\n")
	b.WriteString("`pay globals update <slug>`. Updating one is a destructive (L2) operation and\n")
	b.WriteString("needs `--yes` in a non-interactive shell.\n\n")
}

func writeExamples(b *strings.Builder, ctx ProjectContext) {
	b.WriteString("## Examples against this project\n\n```bash\n")
	for _, ex := range examples(ctx) {
		b.WriteString(ex + "\n")
	}
	b.WriteString("```\n")
}

// examples produces runnable commands built from this project's real slugs,
// so the snapshot is never illustrated with invented ones: three about
// collections and globals, plus two about blocks when a census is cached.
func examples(ctx ProjectContext) []string {
	if len(ctx.Examples) > 0 {
		return ctx.Examples
	}
	var content, drafts, upload string
	draftsDocs := -2
	for _, c := range ctx.Collections {
		if c.Internal {
			continue
		}
		if content == "" {
			content = c.Slug
		}
		// The drafts collection with the most documents is the one an agent
		// is most likely to work on (pages, posts), and the one where
		// forgetting --published-only hurts most.
		if c.HasFeature("drafts") && c.TotalDocs > draftsDocs {
			drafts, draftsDocs = c.Slug, c.TotalDocs
		}
		if upload == "" && c.HasFeature("upload") {
			upload = c.Slug
		}
	}
	if drafts != "" {
		content = drafts
	}
	if content == "" {
		content = "<collection>"
	}

	out := []string{
		fmt.Sprintf("pay describe %s --required-only        # what must be set to create one", content),
	}
	if drafts != "" {
		out = append(out, fmt.Sprintf("pay find %s --published-only --limit 5   # never omit --published-only here", drafts))
	} else {
		out = append(out, fmt.Sprintf("pay find %s --limit 5 --select id", content))
	}
	switch {
	case len(ctx.Globals) > 0:
		out = append(out, fmt.Sprintf("pay globals get %s --depth 1", ctx.Globals[0].Slug))
	case upload != "":
		out = append(out, fmt.Sprintf("pay upload %s ./file.png --alt 'Alt text'", upload))
	default:
		out = append(out, fmt.Sprintf("pay count %s --where 'updatedAt gt 2026-01-01'", content))
	}
	return append(out, blockExamples(ctx.Blocks)...)
}

func codeList(items []string) string {
	if len(items) == 0 {
		return "—"
	}
	safe := make([]string, 0, len(items))
	for _, s := range items {
		safe = append(safe, "`"+redact.Text(s)+"`")
	}
	return strings.Join(safe, ", ")
}

func orDash(s string) string { return orValue(s, "—") }

func orValue(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func version(v string) string { return orValue(v, "dev") }
