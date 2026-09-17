package config

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Scan caps (§7.10). They exist so that pointing PayCLI at a monorepo cannot
// turn a cache miss into a filesystem crawl.
const (
	MaxScanFiles = 200
	MaxScanBytes = 2 << 20 // 2 MB total
	maxFileBytes = 512 << 10
)

// skipDirs are never descended into. node_modules is the load-bearing one: it
// contains thousands of .ts files, including Payload's own block definitions,
// which would swamp the project's real slugs.
var skipDirs = map[string]bool{
	"node_modules": true, ".git": true, ".next": true, ".turbo": true, ".cache": true,
	"dist": true, "build": true, "out": true, "coverage": true, "tmp": true,
	"playwright-report": true, "test-results": true, ".vercel": true, ".yarn": true,
}

// BlockDecl is one block definition found in project source: the blockType
// slug the REST API accepts, together with the interfaceName Payload turns
// into that block's GraphQL union member.
//
// Verified in /home/flo/payload-dummy: src/blocks/CallToAction/config.ts
// declares slug: 'cta' beside interfaceName: 'CallToActionBlock', and
// __type(name:"Page_Layout").possibleTypes names CallToActionBlock. Without
// the pair there is no way back from the union to the slug.
type BlockDecl struct {
	Slug string `json:"slug"`
	// InterfaceName is "" for a block that declares none; Payload then derives
	// the GraphQL type name from the slug itself.
	InterfaceName string `json:"interface_name,omitempty"`
}

// ScanResult is everything §7.10 and §7.11 can learn from the project on disk.
// Every fact carries its provenance; nothing here is ever guessed.
type ScanResult struct {
	Root string `json:"root,omitempty"`

	PayloadVersion       string `json:"payload_version,omitempty"`
	PayloadVersionSource string `json:"payload_version_source"`
	PackageJSON          string `json:"package_json,omitempty"`

	// DBAdapter is inferred from the project's @payloadcms/db-* dependency.
	// §7.11 only defines "configured" and "inferred" for this field, so a
	// package.json match reports "inferred" — which deliberately does NOT
	// unlock §9.3's client-side operator block, since that requires
	// "configured".
	DBAdapter       string `json:"db_adapter"`
	DBAdapterSource string `json:"db_adapter_source"`

	BlockSlugs     []string          `json:"blocks,omitempty"`
	BlocksSource   string            `json:"blocks_source"`
	BlockSlugFiles []string          `json:"block_slug_files,omitempty"`
	SlugFile       map[string]string `json:"-"`

	// BlockDecls are the (slug, interfaceName) pairs §7.10 harvests, in scan
	// order. The pair is the load-bearing fact: GraphQL publishes a blocks
	// field's union as interfaceNames (CallToActionBlock) while the REST API
	// only ever accepts the slug (cta), so neither half alone can turn the
	// per-field union into something writable.
	BlockDecls []BlockDecl `json:"block_decls,omitempty"`
	// SlugByInterface is BlockDecls indexed by interfaceName. Blocks that
	// declare no interfaceName are absent: there is nothing to key them by.
	SlugByInterface map[string]string `json:"-"`

	FilesScanned int   `json:"files_scanned"`
	BytesScanned int64 `json:"bytes_scanned"`
	Truncated    bool  `json:"truncated,omitempty"`
}

// Scan reads what the local filesystem knows about the project: the Payload
// version from package.json (§7.11) and the block slugs from project source
// (§7.10).
//
// It is local-filesystem only. It never makes a network call, never imports or
// evaluates project code, and never writes anything.
func Scan(p *Project) *ScanResult {
	out := &ScanResult{
		PayloadVersionSource: SourceUnknown,
		DBAdapter:            DBUnknown,
		DBAdapterSource:      SourceUnknown,
		BlocksSource:         SourceUnknown,
		SlugFile:             map[string]string{},
	}
	if p == nil || !p.Found() {
		return out
	}
	out.Root = p.Dir

	if p.PackageJSON != "" {
		out.PackageJSON = p.PackageJSON
		if data, err := os.ReadFile(p.PackageJSON); err == nil {
			deps := dependenciesOf(data)
			if v := payloadVersion(deps, filepath.Dir(p.PackageJSON)); v != "" {
				out.PayloadVersion = v
				out.PayloadVersionSource = SourceProjectPkg
			}
			if adapter := dbAdapterFromDeps(deps); adapter != DBUnknown {
				out.DBAdapter = adapter
				out.DBAdapterSource = SourceInferred
			}
		}
	}

	for _, file := range blockCandidates(p) {
		if out.FilesScanned >= MaxScanFiles || out.BytesScanned >= MaxScanBytes {
			out.Truncated = true
			break
		}
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		if len(data) > maxFileBytes {
			data = data[:maxFileBytes]
			out.Truncated = true
		}
		out.FilesScanned++
		out.BytesScanned += int64(len(data))
		for _, decl := range BlockDeclsFromSource(string(data)) {
			if _, seen := out.SlugFile[decl.Slug]; seen {
				continue
			}
			out.SlugFile[decl.Slug] = file
			out.BlockSlugs = append(out.BlockSlugs, decl.Slug)
			out.BlockDecls = append(out.BlockDecls, decl)
			if decl.InterfaceName != "" {
				if out.SlugByInterface == nil {
					out.SlugByInterface = map[string]string{}
				}
				// First declaration wins, matching SlugFile: a second block
				// claiming an interfaceName already taken is a project bug and
				// overwriting would make the answer depend on walk order.
				if _, taken := out.SlugByInterface[decl.InterfaceName]; !taken {
					out.SlugByInterface[decl.InterfaceName] = decl.Slug
				}
			}
		}
	}

	if len(out.BlockSlugs) > 0 {
		out.BlocksSource = SourceProjectSource
		files := map[string]bool{}
		for _, f := range out.SlugFile {
			files[f] = true
		}
		out.BlockSlugFiles = make([]string, 0, len(files))
		for f := range files {
			out.BlockSlugFiles = append(out.BlockSlugFiles, f)
		}
		sort.Strings(out.BlockSlugFiles)
	}
	return out
}

// blockCandidates lists the files §7.10 allows: **/blocks/**/*.ts,
// **/*.block.ts and payload.config.ts. The order is the walk order, which is
// lexical, so the result is deterministic.
func blockCandidates(p *Project) []string {
	roots := []string{p.Dir}
	if p.SrcDir != "" && !strings.HasPrefix(p.SrcDir, p.Dir+string(filepath.Separator)) && p.SrcDir != p.Dir {
		roots = append(roots, p.SrcDir)
	}

	seen := map[string]bool{}
	var out []string
	add := func(path string) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		out = append(out, path)
	}

	if p.PayloadConfig != "" {
		add(p.PayloadConfig)
	}

	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				name := d.Name()
				if path != root && (skipDirs[name] || strings.HasPrefix(name, ".")) {
					return fs.SkipDir
				}
				return nil
			}
			if len(out) >= MaxScanFiles {
				return fs.SkipAll
			}
			if isBlockCandidate(path, root) {
				add(path)
			}
			return nil
		})
	}
	return out
}

func isBlockCandidate(path, root string) bool {
	ext := filepath.Ext(path)
	if ext != ".ts" && ext != ".tsx" && ext != ".js" && ext != ".mjs" {
		return false
	}
	base := filepath.Base(path)
	if strings.HasSuffix(base, ".block"+ext) {
		return true
	}
	if strings.HasPrefix(base, "payload.config.") {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	for _, segment := range strings.Split(filepath.ToSlash(filepath.Dir(rel)), "/") {
		if strings.EqualFold(segment, "blocks") {
			return true
		}
	}
	return false
}

// BlockSlugsFromSource extracts the `slug: '…'` half of BlockDeclsFromSource,
// in the same order and with the same duplicate handling.
func BlockSlugsFromSource(src string) []string {
	decls := BlockDeclsFromSource(src)
	if len(decls) == 0 {
		return nil
	}
	out := make([]string, 0, len(decls))
	for _, d := range decls {
		out = append(out, d.Slug)
	}
	return out
}

// BlockDeclsFromSource extracts the `slug: '…'` and `interfaceName: '…'`
// literals of every object literal that also has a `fields:` key (§7.10).
//
// The two are harvested TOGETHER, from the same object literal, because the
// pair is the only bridge between what GraphQL publishes for a blocks field
// (possibleTypes: CallToActionBlock) and what the REST API accepts for it
// (blockType: cta). A bare slug list cannot say which field a block belongs
// to, which is exactly how one global bag ended up attached to every blocks
// field.
//
// This is a byte scanner, not a TypeScript parser: PayCLI must never import or
// evaluate project code. It understands strings, template literals and both
// comment forms well enough that a slug inside one of them cannot be mistaken
// for a declaration, and it pairs slug/fields by brace nesting, so a nested
// block definition is picked up while an unrelated `slug:` on a collection
// export without `fields:` is not.
//
// Order is source order; duplicate slugs are removed.
func BlockDeclsFromSource(src string) []BlockDecl {
	type object struct {
		slug      string
		iface     string
		hasSlug   bool
		hasFields bool
	}
	var stack []*object
	var out []BlockDecl
	seen := map[string]bool{}

	emit := func(o *object) {
		if o == nil || !o.hasSlug || !o.hasFields || o.slug == "" || seen[o.slug] {
			return
		}
		seen[o.slug] = true
		out = append(out, BlockDecl{Slug: o.slug, InterfaceName: o.iface})
	}

	i := 0
	n := len(src)
	// pendingKey holds the most recent identifier or quoted key so that the
	// following ':' can be attributed to it.
	pendingKey := ""

	for i < n {
		c := src[i]
		switch {
		case c == '/' && i+1 < n && src[i+1] == '/':
			for i < n && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && src[i+1] == '*':
			i += 2
			for i+1 < n && (src[i] != '*' || src[i+1] != '/') {
				i++
			}
			i += 2
		case c == '\'' || c == '"' || c == '`':
			value, next := readString(src, i)
			i = next
			pendingKey = ""
			// A string immediately followed by ':' is a quoted key.
			j := skipSpace(src, i)
			if j < n && src[j] == ':' {
				pendingKey = value
			}
		case c == '{':
			stack = append(stack, &object{})
			pendingKey = ""
			i++
		case c == '}':
			if len(stack) > 0 {
				emit(stack[len(stack)-1])
				stack = stack[:len(stack)-1]
			}
			pendingKey = ""
			i++
		case c == ':':
			key := pendingKey
			pendingKey = ""
			i++
			if len(stack) == 0 {
				continue
			}
			top := stack[len(stack)-1]
			switch key {
			case "fields":
				top.hasFields = true
			case "slug":
				j := skipSpace(src, i)
				if j < n && (src[j] == '\'' || src[j] == '"' || src[j] == '`') {
					value, next := readString(src, j)
					if value != "" && !strings.Contains(value, "${") {
						top.slug = value
						top.hasSlug = true
					}
					i = next
				}
			case "interfaceName":
				j := skipSpace(src, i)
				if j < n && (src[j] == '\'' || src[j] == '"' || src[j] == '`') {
					value, next := readString(src, j)
					if value != "" && !strings.Contains(value, "${") {
						top.iface = value
					}
					i = next
				}
			}
		case isIdentStart(c):
			start := i
			for i < n && isIdentPart(src[i]) {
				i++
			}
			pendingKey = src[start:i]
		default:
			if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
				pendingKey = ""
			}
			i++
		}
	}
	// Unbalanced braces (a truncated file) still yield what was complete.
	for k := len(stack) - 1; k >= 0; k-- {
		emit(stack[k])
	}
	return out
}

// readString returns the contents of the string literal starting at i (which
// must point at a quote) and the index just past it.
func readString(src string, i int) (string, int) {
	quote := src[i]
	i++
	var b strings.Builder
	for i < len(src) {
		c := src[i]
		if c == '\\' {
			if i+1 < len(src) {
				b.WriteByte(src[i+1])
			}
			i += 2
			continue
		}
		if c == quote {
			return b.String(), i + 1
		}
		if quote != '`' && c == '\n' {
			// Unterminated single-line string; stop here rather than eating
			// the rest of the file.
			return b.String(), i
		}
		b.WriteByte(c)
		i++
	}
	return b.String(), i
}

func skipSpace(src string, i int) int {
	for i < len(src) {
		switch src[i] {
		case ' ', '\t', '\r', '\n':
			i++
		case '/':
			if i+1 < len(src) && src[i+1] == '/' {
				for i < len(src) && src[i] != '\n' {
					i++
				}
				continue
			}
			if i+1 < len(src) && src[i+1] == '*' {
				i += 2
				for i+1 < len(src) && (src[i] != '*' || src[i+1] != '/') {
					i++
				}
				i += 2
				continue
			}
			return i
		default:
			return i
		}
	}
	return i
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

// ---------------------------------------------------------- package.json ---

type packageJSON struct {
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	PeerNames       map[string]string `json:"peerDependencies"`
	Version         string            `json:"version"`
}

// dependenciesOf merges the dependency maps, dependencies winning.
func dependenciesOf(data []byte) map[string]string {
	var pkg packageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil
	}
	out := map[string]string{}
	for _, m := range []map[string]string{pkg.PeerNames, pkg.DevDependencies, pkg.Dependencies} {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// payloadVersion resolves the project's Payload version (§7.11 source 2).
//
// An exact specifier ("3.86.0") is the answer. A range ("^3.86.0") is not a
// resolved version, so the installed node_modules/payload/package.json is
// consulted instead; if that is absent the version stays unknown rather than
// being invented from the range.
func payloadVersion(deps map[string]string, dir string) string {
	spec := deps["payload"]
	if isExactVersion(spec) {
		return spec
	}
	installed := filepath.Join(dir, "node_modules", "payload", "package.json")
	if data, err := os.ReadFile(installed); err == nil {
		var pkg packageJSON
		if json.Unmarshal(data, &pkg) == nil && isExactVersion(pkg.Version) {
			return pkg.Version
		}
	}
	return ""
}

// isExactVersion reports whether spec is a bare semver such as "3.86.0" or
// "3.0.0-beta.12" — not a range, tag, workspace or git specifier.
func isExactVersion(spec string) bool {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return false
	}
	if strings.ContainsAny(spec, "^~*<>|= ") || strings.Contains(spec, "x") {
		return false
	}
	if strings.Contains(spec, ":") || strings.HasPrefix(spec, "file") {
		return false
	}
	dots := 0
	for i := 0; i < len(spec); i++ {
		c := spec[i]
		switch {
		case c >= '0' && c <= '9':
		case c == '.':
			dots++
		case c == '-' || c == '+':
			return dots >= 2 // prerelease/build metadata after major.minor.patch
		default:
			return false
		}
	}
	return dots >= 2
}

// dbAdapterFromDeps maps the @payloadcms/db-* package to §7.11's vocabulary.
func dbAdapterFromDeps(deps map[string]string) string {
	for name := range deps {
		switch name {
		case "@payloadcms/db-postgres", "@payloadcms/db-vercel-postgres":
			return DBPostgres
		case "@payloadcms/db-mongodb":
			return DBMongoDB
		case "@payloadcms/db-sqlite":
			return DBSQLite
		}
	}
	return DBUnknown
}
