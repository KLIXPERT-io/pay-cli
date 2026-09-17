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
				if got[i] != tc.want[i] {
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
