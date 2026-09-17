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
	// Fields are the entries of this block's own `fields:` array literal, in
	// source order. They exist for ONE fact GraphQL cannot supply: Payload
	// generates no INPUT_OBJECT for a block type (the blocks mutation argument
	// is a JSON scalar), so the NON_NULL trick that recovers required-ness for
	// a collection field has nothing to read. The config on disk is the only
	// place `required: true` is written down.
	Fields []BlockFieldDecl `json:"fields,omitempty"`
	// FieldsComplete is true only when EVERY element of the `fields:` array
	// was an object literal carrying a `name:` string literal. A helper call
	// (`linkGroup({…})`), a spread, or a computed field makes it false, and a
	// false here is what keeps a missing entry reading as "unknown" instead of
	// "not required" — the exact guess this whole type exists to refuse.
	FieldsComplete bool `json:"fields_complete,omitempty"`

	// LabelSingular and LabelPlural are the block's own `labels:` literals
	// ({ singular: 'Call to Action', plural: 'Calls to Action' }). They are
	// the human name of the block and, like every other fact here, exist only
	// when the project's authors wrote them; "" is "not declared", never a
	// title-cased guess at the slug.
	LabelSingular string `json:"label_singular,omitempty"`
	LabelPlural   string `json:"label_plural,omitempty"`

	// Description is the one-sentence statement of what this block is FOR.
	//
	// Payload blocks have NO standard field for it — verified: a Block's
	// `admin` accepts only components/custom/disableBlockName/group/images/jsx
	// and `tsc --noEmit` rejects `admin: { description }` on a Block — so the
	// text lives in `custom`, Payload's sanctioned arbitrary-metadata escape
	// hatch, under whatever key the project chose. DescriptionKey records
	// which one it was so the output is honest about where the words came from
	// instead of implying a standard field exists.
	Description string `json:"description,omitempty"`
	// DescriptionKey is the literal config path the text was read from:
	// "custom.description", "custom.docs", "custom.summary" or
	// "admin.description". Empty exactly when Description is empty.
	DescriptionKey string `json:"description_key,omitempty"`
}

// BlockFieldDecl is one field read out of a block's `fields:` array.
//
// Required is a POINTER: nil is "the object literal had no `required:` key at
// all", which for Payload means not required, while a non-literal value
// (`required: isProd`) is also recorded as nil because the scanner refuses to
// evaluate project code. The caller decides what to do with each, and only a
// declaration that was fully parsed is allowed to answer false.
type BlockFieldDecl struct {
	Name string `json:"name"`
	// Type is the `type:` literal ('richText', 'upload', 'select', …). It is
	// the only source that can tell lexical richText from a json field and a
	// select from a radio, both of which compile to the SAME GraphQL shape.
	Type string `json:"type,omitempty"`
	// Required is the literal `required: true` / `required: false`, or nil
	// when the key was absent or not a boolean literal.
	Required *bool `json:"required,omitempty"`

	// Description is the field's own human instruction, read from
	// `admin: { description: '…' }` — which, unlike a Block's admin, Payload
	// DOES support on a field and is exactly where a per-field "pass a media
	// document id" belongs. `custom.description` is accepted as well, for the
	// same reason it is on a block.
	Description string `json:"description,omitempty"`
	// DescriptionKey is the literal config path the text was read from
	// ("admin.description", "custom.description", …), empty when Description
	// is empty.
	DescriptionKey string `json:"description_key,omitempty"`
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

	// FieldDocs are the per-field `admin: { description: '…' }` instructions
	// declared by the project's COLLECTIONS and GLOBALS, keyed by entity slug
	// and then by field name.
	//
	// They cover a collection's TOP-LEVEL `fields:` array only. A field nested
	// inside a group, an array or a tab is not reached — the scanner adopts
	// only the direct elements of the array it can attribute — and is reported
	// as undocumented rather than being given a path this scanner would have
	// had to guess.
	//
	// An entity whose config was read but documents nothing is present with an
	// EMPTY map. The key's presence is the fact "PayCLI read this entity's
	// config"; its absence is "there is none on disk", which is what a
	// plugin-provided collection looks like.
	FieldDocs map[string]map[string]FieldDoc `json:"field_docs,omitempty"`
	// FieldDocFiles are the absolute paths FieldDocs were read from, keyed by
	// entity slug, so the answer can always name its own source file.
	FieldDocFiles map[string]string `json:"field_doc_files,omitempty"`

	FilesScanned int   `json:"files_scanned"`
	BytesScanned int64 `json:"bytes_scanned"`
	Truncated    bool  `json:"truncated,omitempty"`
}

// FieldDoc is one field's human documentation as the project wrote it: the
// text and the config key it came from, never one without the other.
type FieldDoc struct {
	Description string `json:"description"`
	// Key is the literal config path — "admin.description" in the normal case.
	Key string `json:"key"`
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

	for _, cand := range scanCandidates(p) {
		file := cand.path
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
			if cand.kind == candidateCollection {
				// A collection's own config. Its slug is a COLLECTION slug and
				// must never enter the block vocabulary; the only thing taken
				// from it is per-field documentation.
				out.addFieldDocs(decl, file)
				continue
			}
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

// addFieldDocs files one entity declaration's documented fields under its slug.
//
// An entity whose config declares NO description is still recorded, with an
// empty map. That is the whole tri-state: the presence of the key means "this
// entity's config was read off disk", so an empty map says "read it, nobody
// documented anything" while an absent key says "never saw a config" — which
// is what a plugin-provided collection in node_modules looks like. Collapsing
// the two would turn a project's deliberate silence into a PayCLI failure.
func (r *ScanResult) addFieldDocs(decl BlockDecl, file string) {
	if decl.Slug == "" {
		return
	}
	docs := map[string]FieldDoc{}
	for _, f := range decl.Fields {
		if f.Name == "" || f.Description == "" {
			continue
		}
		if _, taken := docs[f.Name]; taken {
			// First declaration wins, matching SlugFile: two fields with the
			// same name in one array is a project bug, and letting the later
			// one win would make the answer depend on walk order.
			continue
		}
		docs[f.Name] = FieldDoc{Description: f.Description, Key: f.DescriptionKey}
	}
	if r.FieldDocs == nil {
		r.FieldDocs = map[string]map[string]FieldDoc{}
		r.FieldDocFiles = map[string]string{}
	}
	if _, taken := r.FieldDocs[decl.Slug]; taken {
		return
	}
	r.FieldDocs[decl.Slug] = docs
	r.FieldDocFiles[decl.Slug] = file
}

// candidate is one file the scan is allowed to read, together with WHAT it is
// expected to declare.
//
// The kind is decided by the path alone and is load-bearing: a CollectionConfig
// and a Block are the same object literal to a byte scanner — both carry
// `slug:` and `fields:` — so without it `pages` would join the project's list
// of block slugs and `pay create pages --set layout=…` would offer it as a
// blockType. The directory is the only signal available without evaluating
// project code, which §7.10 forbids.
type candidate struct {
	path string
	kind string
}

const (
	// candidateBlock is a file under a `blocks/` directory, a *.block.ts, or
	// payload.config.ts — the §7.10 block sources.
	candidateBlock = "block"
	// candidateCollection is a file under a `collections/` or `globals/`
	// directory. Its decls are read for FIELD DOCUMENTATION only
	// (admin.description); its slugs are never treated as blockTypes.
	candidateCollection = "collection"
)

// scanCandidates lists every file §7.10 allows the scan to read, classified.
//
// Block sources are **/blocks/**/*.ts, **/*.block.ts and payload.config.ts.
// Entity sources — read for field documentation only — are
// **/collections/**/*.ts and **/globals/**/*.ts. The order is the walk order,
// which is lexical, so the result is deterministic.
func scanCandidates(p *Project) []candidate {
	roots := []string{p.Dir}
	if p.SrcDir != "" && !strings.HasPrefix(p.SrcDir, p.Dir+string(filepath.Separator)) && p.SrcDir != p.Dir {
		roots = append(roots, p.SrcDir)
	}

	seen := map[string]bool{}
	var out []candidate
	add := func(path, kind string) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		out = append(out, candidate{path: path, kind: kind})
	}

	if p.PayloadConfig != "" {
		add(p.PayloadConfig, candidateBlock)
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
			switch {
			case isBlockCandidate(path, root):
				add(path, candidateBlock)
			case isCollectionCandidate(path, root):
				add(path, candidateCollection)
			}
			return nil
		})
	}
	return out
}

// isCollectionCandidate reports the files a collection's or global's own
// config lives in: **/collections/**/*.ts and **/globals/**/*.ts.
//
// They are read for ONE thing — `admin: { description: '…' }` on a field, the
// per-field instruction an agent needs while filling that field in — and never
// for block slugs. Payload supports admin.description on a field of any
// entity, so the same mechanism that documents a block's fields documents a
// collection's; the only difference is which list the decl is filed under.
func isCollectionCandidate(path, root string) bool {
	ext := filepath.Ext(path)
	if ext != ".ts" && ext != ".tsx" && ext != ".js" && ext != ".mjs" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	for _, segment := range strings.Split(filepath.ToSlash(filepath.Dir(rel)), "/") {
		if strings.EqualFold(segment, "collections") || strings.EqualFold(segment, "globals") {
			return true
		}
	}
	return false
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

// DescriptionKeys are the config paths §7.10 accepts a human description from,
// in preference order, and the exact strings reported back as
// description_key.
//
// `admin.description` is Payload's own documented FIELD-level key and is both
// the right place for per-field docs and the first choice here. A BLOCK has no
// such key: a Block's `admin` accepts only components / custom /
// disableBlockName / group / images / jsx, and `tsc --noEmit` rejects
// `admin: { description }` on one. So a block's description has to live in
// `custom`, Payload's sanctioned arbitrary-metadata escape hatch — which is
// free-form, meaning every project spells it differently. The obvious
// neighbours are therefore all accepted, and whichever one answered is always
// reported next to the text rather than being flattened into a "description"
// that implies Payload defines one.
var DescriptionKeys = []string{
	"admin.description", "custom.description", "custom.docs", "custom.summary",
}

// descriptionRank orders DescriptionKeys; a key that is not one of them is
// never recorded at all.
func descriptionRank(key string) int {
	for i, k := range DescriptionKeys {
		if k == key {
			return i
		}
	}
	return len(DescriptionKeys)
}

// docHosts are the child objects whose contents document their parent:
// `admin: {…}` and `custom: {…}`. Everything else nested in a block or a field
// is ignored, which is what keeps an unrelated `description:` on some other
// sub-object from being read as documentation.
var docHosts = map[string]bool{"admin": true, "custom": true}

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

		// name/typ/required are read only so that this object can be reported
		// as one entry of its PARENT's fields array; a block never uses them.
		name     string
		typ      string
		required *bool

		// labelSingular/labelPlural are this object's `labels: { singular,
		// plural }` — a block's human name. They are filled when the child
		// `labels` object closes.
		labelSingular string
		labelPlural   string

		// description/descriptionKey are the human sentence found under this
		// object's `admin:` or `custom:` child, together with the literal key
		// path it came from. Both are set at once or neither is: a description
		// whose origin was lost is exactly the half-fact §7.8.3 forbids.
		description    string
		descriptionKey string

		// role is "admin", "custom" or "labels" when this object IS the child
		// of that name, and "" for every other object; host is the object it
		// documents. Together they are what lets a `description:` five braces
		// deep be attributed to the right block or field — or, far more often,
		// be ignored.
		role string
		host *object

		// pendingChild is set between a `labels`/`admin`/`custom` key and the
		// `{` that follows it, exactly as pendingFields is for `fields` — so
		// that only the object literally assigned to that key adopts the role.
		pendingChild string

		// owner is the block object whose `fields:` array this object is a
		// direct element of, and nil for every other object (a nested
		// `admin: {}`, the block itself, an unrelated literal).
		owner *object

		// pendingFields is set between `fields` and the `[` that follows it,
		// so that only the fields array — not some other array on the same
		// object — adopts the elements inside it.
		pendingFields bool
		fields        []BlockFieldDecl
		// fieldsClosed records that the fields array was balanced; a truncated
		// file leaves it false.
		fieldsClosed bool
		// fieldsPartial records an element the scanner could not read as a
		// named object literal: a helper call, a spread, a conditional. It is
		// the difference between "this block has no `required:` on foo" and
		// "PayCLI never saw foo's declaration".
		fieldsPartial bool
	}
	// bracket is one `[` on the stack, remembering whether it opened a block's
	// fields array and which object owns it.
	type bracket struct {
		owner *object
	}
	var stack []*object
	var brackets []bracket
	var out []BlockDecl
	seen := map[string]bool{}

	emit := func(o *object) {
		if o == nil || !o.hasSlug || !o.hasFields || o.slug == "" || seen[o.slug] {
			return
		}
		seen[o.slug] = true
		out = append(out, BlockDecl{
			Slug:          o.slug,
			InterfaceName: o.iface,
			Fields:        o.fields,
			// Complete means every element was read AND the array was
			// balanced. Both halves matter: a truncated file (§7.10 caps reads
			// at 512 KB) can end mid-array with everything so far parsed.
			FieldsComplete: o.fieldsClosed && !o.fieldsPartial,
			LabelSingular:  o.labelSingular,
			LabelPlural:    o.labelPlural,
			Description:    o.description,
			DescriptionKey: o.descriptionKey,
		})
	}

	// document copies a closed `admin:`/`custom:` child's text onto the object
	// it describes, keeping the strongest key when a project wrote more than
	// one. Preference is DescriptionKeys' order, so the answer cannot depend on
	// which key the file happened to list first.
	document := func(o *object) {
		if o.host == nil || o.description == "" {
			return
		}
		if o.host.description != "" &&
			descriptionRank(o.host.descriptionKey) <= descriptionRank(o.descriptionKey) {
			return
		}
		o.host.description, o.host.descriptionKey = o.description, o.descriptionKey
	}

	i := 0
	n := len(src)
	// pendingKey holds the most recent identifier or quoted key so that the
	// following ':' can be attributed to it.
	pendingKey := ""

	// inFieldsArray reports that the very next `{` is an element of some
	// block's fields array: the innermost bracket must be that array and the
	// innermost object must be the block that owns it, which is what keeps a
	// nested `admin: {}` (one object deeper) from being read as a field.
	inFieldsArray := func() *object {
		if len(brackets) == 0 || len(stack) == 0 {
			return nil
		}
		b := brackets[len(brackets)-1]
		if b.owner != nil && b.owner == stack[len(stack)-1] {
			return b.owner
		}
		return nil
	}

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
		case c == '[':
			// Only the `[` that directly follows `fields:` adopts elements.
			var owner *object
			if len(stack) > 0 {
				top := stack[len(stack)-1]
				// An array is not an `admin:`/`custom:`/`labels:` object, so
				// nothing inside it may claim that role.
				top.pendingChild = ""
				if top.pendingFields {
					owner = top
					owner.pendingFields = false
				}
			}
			brackets = append(brackets, bracket{owner: owner})
			pendingKey = ""
			i++
		case c == ']':
			if len(brackets) > 0 {
				b := brackets[len(brackets)-1]
				brackets = brackets[:len(brackets)-1]
				if b.owner != nil {
					b.owner.fieldsClosed = true
				}
			}
			pendingKey = ""
			i++
		case c == '{':
			child := &object{owner: inFieldsArray()}
			if len(stack) > 0 {
				if parent := stack[len(stack)-1]; parent.pendingChild != "" {
					child.role, child.host = parent.pendingChild, parent
					parent.pendingChild = ""
				}
			}
			stack = append(stack, child)
			pendingKey = ""
			i++
		case c == '}':
			if len(stack) > 0 {
				o := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				emit(o)
				switch o.role {
				case "labels":
					// A block's human name. Only the two literals Payload
					// defines are copied up; an absent one stays "".
					if o.host != nil {
						if o.labelSingular != "" {
							o.host.labelSingular = o.labelSingular
						}
						if o.labelPlural != "" {
							o.host.labelPlural = o.labelPlural
						}
					}
				case "admin", "custom":
					document(o)
				}
				if o.owner != nil {
					if o.name == "" {
						// An element with no `name:` is a row, a collapsible,
						// a UI field or something the scanner does not
						// understand. Either way its children are not named
						// here, so the block's field list is incomplete.
						o.owner.fieldsPartial = true
					} else {
						o.owner.fields = append(o.owner.fields, BlockFieldDecl{
							Name: o.name, Type: o.typ, Required: o.required,
							Description: o.description, DescriptionKey: o.descriptionKey,
						})
					}
				}
			}
			pendingKey = ""
			i++
		case c == '.' && i+2 < n && src[i+1] == '.' && src[i+2] == '.':
			// A spread inside a fields array contributes fields the scanner
			// cannot name.
			if o := inFieldsArray(); o != nil {
				o.fieldsPartial = true
			}
			pendingKey = ""
			i += 3
		case c == ':':
			key := pendingKey
			pendingKey = ""
			i++
			if len(stack) == 0 {
				continue
			}
			top := stack[len(stack)-1]
			// A new key ends any unclaimed `admin:`/`custom:`/`labels:`
			// expectation: only the object literally assigned to that key may
			// adopt its role.
			top.pendingChild = ""
			switch key {
			case "fields":
				top.hasFields = true
				top.pendingFields = true
			case "admin", "custom", "labels":
				top.pendingChild = key
			case "singular", "plural":
				// Only inside a `labels: {…}` object. Anywhere else these are
				// somebody else's keys.
				if top.role != "labels" {
					break
				}
				if value, next, ok := readKeyString(src, i); ok {
					if key == "singular" {
						top.labelSingular = value
					} else {
						top.labelPlural = value
					}
					i = next
				} else {
					i = next
				}
			case "description", "docs", "summary":
				// Only inside an `admin: {…}` or `custom: {…}` object, and
				// only for a key DescriptionKeys names — so `admin.docs` and a
				// stray `description:` on an unrelated literal are both
				// ignored rather than attributed to a block.
				if !docHosts[top.role] {
					break
				}
				path := top.role + "." + key
				if descriptionRank(path) == len(DescriptionKeys) {
					break
				}
				value, next, ok := readKeyString(src, i)
				i = next
				if !ok || value == "" {
					break
				}
				if top.description == "" || descriptionRank(path) < descriptionRank(top.descriptionKey) {
					top.description, top.descriptionKey = value, path
				}
			case "slug":
				if value, next, ok := readKeyString(src, i); ok {
					top.slug, top.hasSlug, i = value, true, next
				} else {
					i = next
				}
			case "interfaceName":
				if value, next, ok := readKeyString(src, i); ok {
					top.iface, i = value, next
				} else {
					i = next
				}
			case "name":
				if value, next, ok := readKeyString(src, i); ok {
					top.name, i = value, next
				} else {
					i = next
				}
			case "type":
				if value, next, ok := readKeyString(src, i); ok {
					top.typ, i = value, next
				} else {
					i = next
				}
			case "required":
				// Literal booleans only. `required: isProd` is deliberately
				// left nil: evaluating project code is forbidden, and a guess
				// here is the one thing an agent cannot recover from.
				j := skipSpace(src, i)
				switch {
				case strings.HasPrefix(src[j:], "true") && !isIdentPart(byteAt(src, j+4)):
					v := true
					top.required = &v
					i = j + 4
				case strings.HasPrefix(src[j:], "false") && !isIdentPart(byteAt(src, j+5)):
					v := false
					top.required = &v
					i = j + 5
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

// readKeyString reads the string literal a key's ':' is followed by. It
// reports false for anything that is not a plain literal — a template with a
// ${} substitution, an identifier, a call — because the scanner never
// evaluates project code.
func readKeyString(src string, i int) (value string, next int, ok bool) {
	j := skipSpace(src, i)
	if j >= len(src) || (src[j] != '\'' && src[j] != '"' && src[j] != '`') {
		return "", i, false
	}
	v, end := readString(src, j)
	if v == "" || strings.Contains(v, "${") {
		return "", end, false
	}
	return v, end, true
}

// byteAt is src[i] with an out-of-range read reported as a byte that ends an
// identifier, so a literal at end-of-file is still recognised.
func byteAt(src string, i int) byte {
	if i < 0 || i >= len(src) {
		return 0
	}
	return src[i]
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
