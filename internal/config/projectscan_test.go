package config

import (
	"path/filepath"
	"strings"
	"testing"
)

const ctaBlock = `import type { Block } from 'payload'

export const CallToAction: Block = {
  slug: 'cta',
  interfaceName: 'CallToActionBlock',
  fields: [
    {
      name: 'richText',
      type: 'richText',
    },
  ],
  labels: { plural: 'Calls to Action', singular: 'Call to Action' },
}
`

const mediaBlock = `import type { Block } from 'payload'

export const MediaBlock: Block = {
  slug: 'mediaBlock',
  interfaceName: 'MediaBlock',
  fields: [{ name: 'media', type: 'upload', relationTo: 'media', required: true }],
}
`

func TestBlockSlugsFromSource(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{name: "block with fields", src: ctaBlock, want: []string{"cta"}},
		{name: "single line", src: mediaBlock, want: []string{"mediaBlock"}},
		{
			name: "double quotes and a quoted key",
			src:  `export const B = { "slug": "quoted", fields: [] }`,
			want: []string{"quoted"},
		},
		{
			name: "template literal slug",
			src:  "export const B = { slug: `templated`, fields: [] }",
			want: []string{"templated"},
		},
		{
			name: "no fields key means it is not a block",
			src:  `export const Collection = { slug: 'pages', admin: {} }`,
			want: nil,
		},
		{
			name: "nested blocks are found too",
			src: `export const Outer = { slug: 'outer', fields: [
                     { name: 'layout', type: 'blocks', blocks: [ { slug: 'inner', fields: [] } ] } ] }`,
			want: []string{"inner", "outer"},
		},
		{
			name: "a slug inside a comment is ignored",
			src:  "// slug: 'commented',\n/* slug: 'blocked', fields: [] */\nexport const B = { slug: 'real', fields: [] }",
			want: []string{"real"},
		},
		{
			name: "a slug inside a string is ignored",
			src:  `const doc = "slug: 'stringy', fields: []"; export const B = { slug: 'real', fields: [] }`,
			want: []string{"real"},
		},
		{
			name: "an interpolated slug is not a literal",
			src:  "export const B = { slug: `${name}-block`, fields: [] }",
			want: nil,
		},
		{
			name: "a computed slug is skipped, not guessed",
			src:  `export const B = { slug: SLUG_CONST, fields: [] }`,
			want: nil,
		},
		{
			name: "duplicates collapse",
			src:  `const a = { slug: 'x', fields: [] }; const b = { slug: 'x', fields: [] }`,
			want: []string{"x"},
		},
		{
			name: "escaped quote inside the slug",
			src:  `export const B = { slug: 'it\'s', fields: [] }`,
			want: []string{"it's"},
		},
		{name: "empty source", src: "", want: nil},
		{name: "unbalanced braces still yield what closed", src: `{ slug: 'a', fields: [] `, want: []string{"a"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := BlockSlugsFromSource(tc.src)
			if len(got) != len(tc.want) {
				t.Fatalf("BlockSlugsFromSource() = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("BlockSlugsFromSource() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestScanProject mirrors the real layout of /home/flo/payload-dummy: payload
// 3.86.0 in package.json, @payloadcms/db-postgres, and cta/content/mediaBlock
// under src/blocks/*/config.ts (§7.10, §7.11).
func TestScanProject(t *testing.T) {
	root := writeTree(t, map[string]string{
		".git/HEAD":                            "ref: refs/heads/main\n",
		"package.json":                         `{"name":"payload-dummy","dependencies":{"payload":"3.86.0","@payloadcms/db-postgres":"3.86.0","next":"15.0.0"}}`,
		"src/payload.config.ts":                "export default buildConfig({ collections: [Pages] })",
		"src/blocks/CallToAction/config.ts":    ctaBlock,
		"src/blocks/MediaBlock/config.ts":      mediaBlock,
		"src/blocks/Content/config.ts":         `export const Content = { slug: 'content', fields: [] }`,
		"src/collections/Pages.ts":             `export const Pages = { slug: 'pages', fields: [] }`,
		"node_modules/payload/src/blocks/x.ts": `export const X = { slug: 'should-not-appear', fields: [] }`,
	})

	p := FindProject(filepath.Join(root, "src", "collections"))
	got := Scan(p)

	if got.PayloadVersion != "3.86.0" || got.PayloadVersionSource != SourceProjectPkg {
		t.Errorf("payload version = %q/%q, want 3.86.0/project-package-json",
			got.PayloadVersion, got.PayloadVersionSource)
	}
	if got.DBAdapter != DBPostgres || got.DBAdapterSource != SourceInferred {
		t.Errorf("db adapter = %q/%q", got.DBAdapter, got.DBAdapterSource)
	}
	want := map[string]bool{"cta": true, "content": true, "mediaBlock": true}
	if len(got.BlockSlugs) != len(want) {
		t.Fatalf("blocks = %v, want %v", got.BlockSlugs, want)
	}
	for _, slug := range got.BlockSlugs {
		if !want[slug] {
			t.Errorf("unexpected slug %q (node_modules or a collection leaked in)", slug)
		}
		if got.SlugFile[slug] == "" {
			t.Errorf("slug %q has no source file recorded", slug)
		}
	}
	if got.BlocksSource != SourceProjectSource {
		t.Errorf("blocks_source = %q", got.BlocksSource)
	}
	if len(got.BlockSlugFiles) != 3 {
		t.Errorf("block_slug_files = %v", got.BlockSlugFiles)
	}
	for _, f := range got.BlockSlugFiles {
		if !filepath.IsAbs(f) {
			t.Errorf("block_slug_files must be absolute: %q", f)
		}
		if strings.Contains(f, "node_modules") {
			t.Errorf("node_modules was scanned: %q", f)
		}
	}
}

func TestScanWithoutProject(t *testing.T) {
	got := Scan(FindProject(t.TempDir()))
	if got.PayloadVersion != "" || got.PayloadVersionSource != SourceUnknown {
		t.Errorf("version = %q/%q, want empty/unknown", got.PayloadVersion, got.PayloadVersionSource)
	}
	if got.DBAdapter != DBUnknown || got.BlocksSource != SourceUnknown {
		t.Errorf("want unknown/unknown, got %q/%q", got.DBAdapter, got.BlocksSource)
	}
	if len(got.BlockSlugs) != 0 {
		t.Errorf("blocks = %v", got.BlockSlugs)
	}
	if Scan(nil) == nil {
		t.Error("Scan(nil) must not return nil")
	}
}

func TestScanRangeVersionFallsBackToNodeModules(t *testing.T) {
	root := writeTree(t, map[string]string{
		"package.json":                      `{"dependencies":{"payload":"^3.80.0"}}`,
		"node_modules/payload/package.json": `{"name":"payload","version":"3.86.1"}`,
		"src/payload.config.ts":             "export default {}",
	})
	got := Scan(FindProject(root))
	if got.PayloadVersion != "3.86.1" {
		t.Errorf("version = %q, want the installed 3.86.1 rather than the range", got.PayloadVersion)
	}

	// Without node_modules the range must NOT be reported as a version.
	bare := writeTree(t, map[string]string{
		"package.json":          `{"dependencies":{"payload":"^3.80.0"}}`,
		"src/payload.config.ts": "export default {}",
	})
	got = Scan(FindProject(bare))
	if got.PayloadVersion != "" || got.PayloadVersionSource != SourceUnknown {
		t.Errorf("version = %q/%q, want empty/unknown", got.PayloadVersion, got.PayloadVersionSource)
	}
}

func TestIsExactVersion(t *testing.T) {
	tests := map[string]bool{
		"3.86.0":        true,
		"3.0.0-beta.12": true,
		"^3.86.0":       false,
		"~3.86.0":       false,
		">=3.0.0":       false,
		"3.x":           false,
		"latest":        false,
		"workspace:*":   false,
		"link:./plugin": false,
		"3.86":          false,
		"":              false,
	}
	for spec, want := range tests {
		if got := isExactVersion(spec); got != want {
			t.Errorf("isExactVersion(%q) = %v, want %v", spec, got, want)
		}
	}
}

func TestDBAdapterFromDeps(t *testing.T) {
	tests := []struct {
		deps map[string]string
		want string
	}{
		{map[string]string{"@payloadcms/db-postgres": "3.86.0"}, DBPostgres},
		{map[string]string{"@payloadcms/db-mongodb": "3.86.0"}, DBMongoDB},
		{map[string]string{"@payloadcms/db-sqlite": "3.86.0"}, DBSQLite},
		{map[string]string{"@payloadcms/db-vercel-postgres": "3.86.0"}, DBPostgres},
		{map[string]string{"next": "15.0.0"}, DBUnknown},
		{nil, DBUnknown},
	}
	for _, tc := range tests {
		if got := dbAdapterFromDeps(tc.deps); got != tc.want {
			t.Errorf("dbAdapterFromDeps(%v) = %q, want %q", tc.deps, got, tc.want)
		}
	}
}

func TestScanCapsFileCount(t *testing.T) {
	files := map[string]string{"package.json": `{}`, ".git/HEAD": "ref: x"}
	for i := 0; i < MaxScanFiles+50; i++ {
		files["src/blocks/b"+itoa(i)+"/config.ts"] = `export const B = { slug: 'b` + itoa(i) + `', fields: [] }`
	}
	got := Scan(FindProject(writeTree(t, files)))
	if got.FilesScanned > MaxScanFiles {
		t.Errorf("scanned %d files, cap is %d", got.FilesScanned, MaxScanFiles)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [8]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

// TestBlockDeclsFromSourcePairsSlugWithInterfaceName is the regression test
// for §7.10's per-field resolution: GraphQL publishes a blocks field's union
// as interfaceNames (CallToActionBlock) and the REST API only accepts slugs
// (cta), so harvesting bare slugs leaves no way back from one to the other.
func TestBlockDeclsFromSourcePairsSlugWithInterfaceName(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []BlockDecl
	}{
		{
			name: "interfaceName before fields",
			src:  ctaBlock,
			want: []BlockDecl{{Slug: "cta", InterfaceName: "CallToActionBlock"}},
		},
		{
			name: "interfaceName after fields, as Banner/config.ts writes it",
			src:  `export const Banner = { slug: 'banner', fields: [{ name: 'style' }], interfaceName: 'BannerBlock' }`,
			want: []BlockDecl{{Slug: "banner", InterfaceName: "BannerBlock"}},
		},
		{
			name: "no interfaceName leaves the pair half empty rather than guessing",
			src:  `export const Content = { slug: 'content', fields: [] }`,
			want: []BlockDecl{{Slug: "content"}},
		},
		{
			name: "an interfaceName one object down does not attach to the parent",
			src: `export const Outer = { slug: 'outer', fields: [
                    { name: 'l', type: 'blocks', blocks: [ { slug: 'inner', interfaceName: 'InnerBlock', fields: [] } ] } ] }`,
			want: []BlockDecl{{Slug: "inner", InterfaceName: "InnerBlock"}, {Slug: "outer"}},
		},
		{
			name: "an interfaceName inside a comment or string is ignored",
			src: "// interfaceName: 'Commented',\nconst s = \"interfaceName: 'Stringy'\";" +
				"export const B = { slug: 'real', interfaceName: 'RealBlock', fields: [] }",
			want: []BlockDecl{{Slug: "real", InterfaceName: "RealBlock"}},
		},
		{
			name: "an interpolated interfaceName is not a literal",
			src:  "export const B = { slug: 'b', interfaceName: `${N}Block`, fields: [] }",
			want: []BlockDecl{{Slug: "b"}},
		},
		{
			name: "an object without fields is not a block at all",
			src:  `export const Pages = { slug: 'pages', interfaceName: 'Page', admin: {} }`,
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := BlockDeclsFromSource(tc.src)
			if len(got) != len(tc.want) {
				t.Fatalf("BlockDeclsFromSource() = %+v, want %+v", got, tc.want)
			}
			for i := range tc.want {
				// Compared field by field rather than with ==: BlockDecl now
				// carries the block's own field declarations, which makes it
				// uncomparable. The pair is still asserted exactly.
				if got[i].Slug != tc.want[i].Slug || got[i].InterfaceName != tc.want[i].InterfaceName {
					t.Fatalf("BlockDeclsFromSource() = %+v, want %+v", got, tc.want)
				}
			}
		})
	}
}

// TestBlockSlugsFromSourceStillReturnsOnlySlugs pins that the pair harvest did
// not change the slug half's contract.
func TestBlockSlugsFromSourceStillReturnsOnlySlugs(t *testing.T) {
	got := BlockSlugsFromSource(ctaBlock + mediaBlock)
	want := []string{"cta", "mediaBlock"}
	if len(got) != len(want) {
		t.Fatalf("BlockSlugsFromSource() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("BlockSlugsFromSource() = %v, want %v", got, want)
		}
	}
}

// TestScanIndexesBlocksByInterfaceName covers the scan-level half: without
// SlugByInterface a blocks field's union cannot be turned into blockTypes.
func TestScanIndexesBlocksByInterfaceName(t *testing.T) {
	root := writeTree(t, map[string]string{
		".git/HEAD":                         "ref: refs/heads/main\n",
		"package.json":                      `{"name":"d","dependencies":{"payload":"3.86.0"}}`,
		"src/payload.config.ts":             "export default buildConfig({})",
		"src/blocks/CallToAction/config.ts": ctaBlock,
		"src/blocks/MediaBlock/config.ts":   mediaBlock,
		"src/blocks/Content/config.ts":      `export const Content = { slug: 'content', fields: [] }`,
	})

	got := Scan(FindProject(filepath.Join(root, "src")))

	if got.SlugByInterface["CallToActionBlock"] != "cta" {
		t.Errorf("CallToActionBlock -> %q, want cta", got.SlugByInterface["CallToActionBlock"])
	}
	if got.SlugByInterface["MediaBlock"] != "mediaBlock" {
		t.Errorf("MediaBlock -> %q, want mediaBlock", got.SlugByInterface["MediaBlock"])
	}
	// A block that declares no interfaceName has nothing to key it by and must
	// not be invented into the map under its slug.
	if _, ok := got.SlugByInterface["Content"]; ok {
		t.Error("a block without an interfaceName was given one")
	}
	if len(got.BlockDecls) != 3 {
		t.Fatalf("block_decls = %+v, want one per block", got.BlockDecls)
	}
	for _, d := range got.BlockDecls {
		if d.Slug == "" {
			t.Errorf("a declaration with no slug was recorded: %+v", d)
		}
	}
}

// ctaBlockWithHelper is CallToAction/config.ts as the live project actually
// writes it: one literal field beside a `linkGroup({…})` helper call that
// contributes a field the scanner cannot name.
const ctaBlockWithHelper = `import type { Block } from 'payload'
import { linkGroup } from '../../fields/linkGroup'

export const CallToAction: Block = {
  slug: 'cta',
  interfaceName: 'CallToActionBlock',
  fields: [
    { name: 'richText', type: 'richText', label: false },
    linkGroup({ appearances: ['default', 'outline'], overrides: { maxRows: 2 } }),
  ],
}
`

// TestBlockDeclFieldsCarryRequiredness pins the ONE fact GraphQL cannot supply
// for a block: Payload publishes no input type for a block type, so
// `required: true` exists only in the project's own source. Without the field
// harvest every block field's required-ness is unknowable.
func TestBlockDeclFieldsCarryRequiredness(t *testing.T) {
	decls := BlockDeclsFromSource(mediaBlock)
	if len(decls) != 1 {
		t.Fatalf("BlockDeclsFromSource() = %+v, want one declaration", decls)
	}
	d := decls[0]
	if !d.FieldsComplete {
		t.Errorf("FieldsComplete = false; every element of this fields array is a named object literal")
	}
	if len(d.Fields) != 1 {
		t.Fatalf("Fields = %+v, want exactly media", d.Fields)
	}
	f := d.Fields[0]
	if f.Name != "media" || f.Type != "upload" {
		t.Errorf("Fields[0] = %+v, want name=media type=upload", f)
	}
	if f.Required == nil || !*f.Required {
		t.Errorf("media.Required = %v, want true — it is `required: true` in the config", f.Required)
	}
}

// TestBlockDeclFieldsAreIncompleteWhenAHelperContributesFields is the guard
// against the worst available answer: reporting a block's field list as
// authoritative when a helper call added fields the scanner never saw would
// make `links` read as "declared, not required" instead of "never seen".
func TestBlockDeclFieldsAreIncompleteWhenAHelperContributesFields(t *testing.T) {
	decls := BlockDeclsFromSource(ctaBlockWithHelper)
	if len(decls) != 1 || decls[0].Slug != "cta" {
		t.Fatalf("BlockDeclsFromSource() = %+v", decls)
	}
	d := decls[0]
	if d.FieldsComplete {
		t.Error("FieldsComplete = true, but linkGroup({…}) contributes a field the scanner cannot name")
	}
	if len(d.Fields) != 1 || d.Fields[0].Name != "richText" {
		t.Fatalf("Fields = %+v, want only the literal richText entry", d.Fields)
	}
	if d.Fields[0].Required != nil {
		t.Errorf("richText.Required = %v, want nil: the config declares no `required:` key", d.Fields[0].Required)
	}
	if d.Fields[0].Type != "richText" {
		t.Errorf("richText.Type = %q, want richText — the only source that tells lexical from json", d.Fields[0].Type)
	}
}

// TestBlockDeclFieldParsing covers the shapes a byte scanner must not get
// wrong, each of which would otherwise become a confident wrong answer.
func TestBlockDeclFieldParsing(t *testing.T) {
	tests := []struct {
		name         string
		src          string
		wantFields   []BlockFieldDecl
		wantComplete bool
	}{
		{
			name:         "a non-literal required is nil, never evaluated",
			src:          `const B = { slug: 'b', fields: [{ name: 'x', type: 'text', required: isProd }] }`,
			wantFields:   []BlockFieldDecl{{Name: "x", Type: "text"}},
			wantComplete: true,
		},
		{
			name:         "required: false is recorded as false, not as absent",
			src:          `const B = { slug: 'b', fields: [{ name: 'x', type: 'text', required: false }] }`,
			wantFields:   []BlockFieldDecl{{Name: "x", Type: "text", Required: boolPtrTest(false)}},
			wantComplete: true,
		},
		{
			name:         "a nested admin object is not a field of the block",
			src:          `const B = { slug: 'b', fields: [{ name: 'x', type: 'text', admin: { width: '50%' } }] }`,
			wantFields:   []BlockFieldDecl{{Name: "x", Type: "text"}},
			wantComplete: true,
		},
		{
			name:         "a spread makes the list incomplete",
			src:          `const B = { slug: 'b', fields: [...shared, { name: 'x', type: 'text' }] }`,
			wantFields:   []BlockFieldDecl{{Name: "x", Type: "text"}},
			wantComplete: false,
		},
		{
			name:         "an unnamed element (a row) makes the list incomplete",
			src:          `const B = { slug: 'b', fields: [{ type: 'row', fields: [] }] }`,
			wantFields:   nil,
			wantComplete: false,
		},
		{
			name:         "a sibling array is not the fields array",
			src:          `const B = { slug: 'b', labels: [{ name: 'nope' }], fields: [{ name: 'x', type: 'text' }] }`,
			wantFields:   []BlockFieldDecl{{Name: "x", Type: "text"}},
			wantComplete: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			decls := BlockDeclsFromSource(tc.src)
			if len(decls) != 1 {
				t.Fatalf("BlockDeclsFromSource() = %+v, want one declaration", decls)
			}
			got := decls[0]
			if got.FieldsComplete != tc.wantComplete {
				t.Errorf("FieldsComplete = %v, want %v", got.FieldsComplete, tc.wantComplete)
			}
			if len(got.Fields) != len(tc.wantFields) {
				t.Fatalf("Fields = %+v, want %+v", got.Fields, tc.wantFields)
			}
			for i, w := range tc.wantFields {
				g := got.Fields[i]
				if g.Name != w.Name || g.Type != w.Type {
					t.Fatalf("Fields[%d] = %+v, want %+v", i, g, w)
				}
				switch {
				case w.Required == nil && g.Required != nil:
					t.Errorf("Fields[%d].Required = %v, want nil", i, *g.Required)
				case w.Required != nil && g.Required == nil:
					t.Errorf("Fields[%d].Required = nil, want %v", i, *w.Required)
				case w.Required != nil && *g.Required != *w.Required:
					t.Errorf("Fields[%d].Required = %v, want %v", i, *g.Required, *w.Required)
				}
			}
		})
	}
}

func boolPtrTest(b bool) *bool { return &b }

// ---------------------------------------------------------------------------
// §7.10 human documentation: labels, descriptions and per-field instructions.
//
// A block slug tells an agent that `cta` exists. It does not say what a cta is
// FOR, which is the one thing needed to choose between cta, content and
// mediaBlock. Payload publishes neither a block's labels nor any description
// over REST or GraphQL, and defines NO description field for a block at all,
// so the project's own source is the only place the answer can come from.
// ---------------------------------------------------------------------------

// TestBlockDeclHarvestsLabelsAndDescription is the shape every block in
// /home/flo/payload-dummy now has.
func TestBlockDeclHarvestsLabelsAndDescription(t *testing.T) {
	src := `
export const CallToAction: Block = {
  slug: 'cta',
  custom: {
    description:
      'A prompt with rich text and one or more buttons, used to push the reader to a next step.',
  },
  interfaceName: 'CallToActionBlock',
  fields: [
    { name: 'richText', type: 'richText', label: false },
  ],
  labels: {
    plural: 'Calls to Action',
    singular: 'Call to Action',
  },
}`
	decls := BlockDeclsFromSource(src)
	if len(decls) != 1 {
		t.Fatalf("decls = %+v, want exactly one block", decls)
	}
	d := decls[0]
	if d.Slug != "cta" || d.InterfaceName != "CallToActionBlock" {
		t.Fatalf("decl = %+v, want the slug/interfaceName pair intact", d)
	}
	if d.LabelSingular != "Call to Action" || d.LabelPlural != "Calls to Action" {
		t.Errorf("labels = %q/%q, want the block's own singular and plural", d.LabelSingular, d.LabelPlural)
	}
	if !strings.HasPrefix(d.Description, "A prompt with rich text") {
		t.Errorf("description = %q, want the custom.description text", d.Description)
	}
	// The key is as load-bearing as the text: Payload defines no description
	// for a block, so an answer that does not say where the words came from
	// implies a standard field that does not exist.
	if d.DescriptionKey != "custom.description" {
		t.Errorf("description_key = %q, want custom.description", d.DescriptionKey)
	}
}

// TestBlockDescriptionAcceptsCustomNeighbours: `custom` is free-form, so every
// project spells this differently. The obvious neighbours are all accepted and
// each reports the key it actually came from.
func TestBlockDescriptionAcceptsCustomNeighbours(t *testing.T) {
	cases := []struct{ key, want string }{
		{"description", "custom.description"},
		{"docs", "custom.docs"},
		{"summary", "custom.summary"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			src := "export const B = { slug: 'b', custom: { " + tc.key +
				": 'what this block is for' }, fields: [{ name: 'x', type: 'text' }] }"
			decls := BlockDeclsFromSource(src)
			if len(decls) != 1 {
				t.Fatalf("decls = %+v", decls)
			}
			if decls[0].Description != "what this block is for" {
				t.Errorf("description = %q", decls[0].Description)
			}
			if decls[0].DescriptionKey != tc.want {
				t.Errorf("description_key = %q, want %q", decls[0].DescriptionKey, tc.want)
			}
		})
	}
}

// TestBlockDescriptionPrefersTheCanonicalKey: a project that wrote two of them
// gets a deterministic answer that does not depend on source order.
func TestBlockDescriptionPrefersTheCanonicalKey(t *testing.T) {
	for _, src := range []string{
		"export const B = { slug: 'b', custom: { summary: 'S', description: 'D' }, fields: [] }",
		"export const B = { slug: 'b', custom: { description: 'D', summary: 'S' }, fields: [] }",
	} {
		decls := BlockDeclsFromSource(src)
		if len(decls) != 1 || decls[0].Description != "D" || decls[0].DescriptionKey != "custom.description" {
			t.Errorf("%s -> %+v, want custom.description to win in either order", src, decls)
		}
	}
}

// TestBlockFieldAdminDescription: unlike a Block, a FIELD does support
// admin.description, and it is exactly the per-field instruction an agent
// needs while filling that field in.
func TestBlockFieldAdminDescription(t *testing.T) {
	src := `
export const MediaBlock: Block = {
  slug: 'mediaBlock',
  labels: { plural: 'Media', singular: 'Media' },
  custom: { description: 'A single image or video from the media library.' },
  interfaceName: 'MediaBlock',
  fields: [
    {
      name: 'media',
      admin: {
        description: 'The image or video to display. Pass a media document id.',
      },
      type: 'upload',
      relationTo: 'media',
      required: true,
    },
    { name: 'caption', type: 'text' },
  ],
}`
	decls := BlockDeclsFromSource(src)
	if len(decls) != 1 || len(decls[0].Fields) != 2 {
		t.Fatalf("decls = %+v", decls)
	}
	media := decls[0].Fields[0]
	if media.Description != "The image or video to display. Pass a media document id." {
		t.Errorf("media.description = %q", media.Description)
	}
	if media.DescriptionKey != "admin.description" {
		t.Errorf("media.description_key = %q, want admin.description", media.DescriptionKey)
	}
	// The field's own `admin` block must not leak onto the block, and an
	// undocumented sibling must stay undocumented.
	if decls[0].DescriptionKey != "custom.description" {
		t.Errorf("block description_key = %q, want the block's own custom.description", decls[0].DescriptionKey)
	}
	if decls[0].Fields[1].Description != "" {
		t.Errorf("caption.description = %q, want empty: nobody wrote one", decls[0].Fields[1].Description)
	}
	// Harvesting documentation must not disturb the facts that were already
	// there.
	if media.Required == nil || !*media.Required || !decls[0].FieldsComplete {
		t.Errorf("decl = %+v, want media required and the fields array complete", decls[0])
	}
}

// TestDescriptionIsNotHarvestedFromUnrelatedObjects: a `description:` that is
// not under `admin:`/`custom:` belongs to somebody else. Attributing it to the
// block would publish an arbitrary string as the project's documentation.
func TestDescriptionIsNotHarvestedFromUnrelatedObjects(t *testing.T) {
	src := `
export const B = {
  slug: 'b',
  graphQL: { description: 'a GraphQL schema description, not ours' },
  meta: { labels: { singular: 'Nope' } },
  fields: [{ name: 'x', type: 'text', validate: { description: 'nope' } }],
}`
	decls := BlockDeclsFromSource(src)
	if len(decls) != 1 {
		t.Fatalf("decls = %+v", decls)
	}
	if decls[0].Description != "" || decls[0].DescriptionKey != "" {
		t.Errorf("description = %q (%q), want nothing: neither key is admin/custom",
			decls[0].Description, decls[0].DescriptionKey)
	}
	// `labels` nested inside an unrelated object is that object's, not the
	// block's — the role is only adopted by the literal assigned to the key.
	if decls[0].LabelSingular != "" {
		t.Errorf("label_singular = %q, want nothing", decls[0].LabelSingular)
	}
	if decls[0].Fields[0].Description != "" {
		t.Errorf("field description = %q, want nothing", decls[0].Fields[0].Description)
	}
}

// TestBlockDocsAbsentStayEmpty: a block with neither labels nor a description
// reports nothing rather than a title-cased guess at its slug.
func TestBlockDocsAbsentStayEmpty(t *testing.T) {
	decls := BlockDeclsFromSource("export const B = { slug: 'mediaBlock', fields: [] }")
	if len(decls) != 1 {
		t.Fatalf("decls = %+v", decls)
	}
	if decls[0].LabelSingular != "" || decls[0].LabelPlural != "" || decls[0].Description != "" {
		t.Errorf("decl = %+v, want every documentation field empty", decls[0])
	}
}

// TestScanHarvestsCollectionFieldDocs: `admin.description` documents a
// collection's fields with exactly the same mechanism it documents a block's,
// so a collection's config is scanned for it — and its SLUG is never allowed
// into the block vocabulary, because a CollectionConfig and a Block are the
// same object literal to a byte scanner.
func TestScanHarvestsCollectionFieldDocs(t *testing.T) {
	root := writeTree(t, map[string]string{
		"package.json":          `{"dependencies":{"payload":"3.86.0"}}`,
		"src/payload.config.ts": "export default buildConfig({})",
		"src/collections/Things.ts": `
export const Things: CollectionConfig = {
  slug: 'things',
  fields: [
    { name: 'title', type: 'text', required: true,
      admin: { description: 'Shown in listings. Keep it under 60 characters.' } },
    { name: 'body', type: 'richText' },
  ],
}`,
		"src/blocks/Cta/config.ts": "export const Cta = { slug: 'cta', fields: [{ name: 'x', type: 'text' }] }",
	})
	got := Scan(FindProject(root))

	docs, scanned := got.FieldDocs["things"]
	if !scanned {
		t.Fatalf("FieldDocs = %+v, want an entry for things", got.FieldDocs)
	}
	if docs["title"].Description != "Shown in listings. Keep it under 60 characters." {
		t.Errorf("title doc = %+v", docs["title"])
	}
	if docs["title"].Key != "admin.description" {
		t.Errorf("title key = %q, want admin.description", docs["title"].Key)
	}
	if _, documented := docs["body"]; documented {
		t.Errorf("body is documented (%+v) but nobody wrote one", docs["body"])
	}
	if got.FieldDocFiles["things"] == "" {
		t.Error("FieldDocFiles has no file for things: an answer must be able to name its source")
	}
	// The collection slug must NOT become a blockType. `pay create pages
	// --set-json layout=[{"blockType":"things"}]` would be silently dropped by
	// Payload, and offering it is how that happens.
	for _, slug := range got.BlockSlugs {
		if slug == "things" {
			t.Fatalf("BlockSlugs = %v: a collection slug leaked into the block vocabulary", got.BlockSlugs)
		}
	}
	if len(got.BlockSlugs) != 1 || got.BlockSlugs[0] != "cta" {
		t.Errorf("BlockSlugs = %v, want exactly [cta]", got.BlockSlugs)
	}
}

// TestScanRecordsEntitiesThatDocumentNothing is the tri-state: an entity whose
// config was READ and documents nothing must be distinguishable from one whose
// config PayCLI never saw (a plugin's collection, in node_modules).
func TestScanRecordsEntitiesThatDocumentNothing(t *testing.T) {
	root := writeTree(t, map[string]string{
		"package.json":          `{"dependencies":{"payload":"3.86.0"}}`,
		"src/payload.config.ts": "export default buildConfig({})",
		"src/collections/Plain.ts": "export const Plain = " +
			"{ slug: 'plain', fields: [{ name: 'title', type: 'text' }] }",
	})
	got := Scan(FindProject(root))

	docs, scanned := got.FieldDocs["plain"]
	if !scanned {
		t.Fatalf("FieldDocs = %+v, want plain present with an empty map", got.FieldDocs)
	}
	if len(docs) != 0 {
		t.Errorf("docs = %+v, want empty", docs)
	}
	if _, scanned := got.FieldDocs["forms"]; scanned {
		t.Error("a collection with no config on disk must be absent, not empty")
	}
}
