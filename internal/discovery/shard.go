package discovery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

// Field is one entry of a field shard.
//
// **Every key below is mandatory on every entry**, using null (or the
// documented sentinels "n/a" / "unknown") where it does not apply. An agent
// parses a field entry without presence checks, so `jq '.fields[] |
// select(.required)'` is total. A unit test asserts the exact key set.
type Field struct {
	Name        string  `json:"name"`
	Path        string  `json:"path"`
	GraphQLPath string  `json:"graphql_path"`
	Parent      *string `json:"parent"`

	PayloadType           string `json:"payload_type"`
	PayloadTypeConfidence string `json:"payload_type_confidence"`

	GraphQLType *string `json:"graphql_type"`
	JSONType    string  `json:"json_type"`

	Required       *bool  `json:"required"`
	RequiredSource string `json:"required_source"`

	HasMany         bool   `json:"has_many"`
	Localized       *bool  `json:"localized"`
	LocalizedSource string `json:"localized_source"`

	ReadOnly bool `json:"read_only"`

	Options       []string `json:"options"`
	OptionsSource string   `json:"options_source"`

	RelationTo       []string `json:"relation_to"`
	RelationToSource string   `json:"relation_to_source"`
	Polymorphic      bool     `json:"polymorphic"`

	// WriteShape is mandatory and non-null on every relationship, upload and
	// polymorphic field and null on every other kind. §9.10's --set coercion
	// reads exactly this key.
	WriteShape *string `json:"write_shape"`

	Queryable bool     `json:"queryable"`
	Operators []string `json:"operators"`

	Sortable           bool   `json:"sortable"`
	SortableConfidence string `json:"sortable_confidence"`

	HookMutated string `json:"hook_mutated"`

	Label string `json:"label"`
}

// FieldKeys is the exact, ordered key set every field entry carries. It is
// exported so a consumer can assert the contract instead of re-deriving it.
var FieldKeys = []string{
	"name", "path", "graphql_path", "parent",
	"payload_type", "payload_type_confidence",
	"graphql_type", "json_type",
	"required", "required_source",
	"has_many", "localized", "localized_source",
	"read_only",
	"options", "options_source",
	"relation_to", "relation_to_source", "polymorphic",
	"write_shape",
	"queryable", "operators",
	"sortable", "sortable_confidence",
	"hook_mutated",
	"label",
}

// NewField returns a field with every tri-state key already set to its honest
// unknown value, so a producer can never emit a half-populated entry.
func NewField(name, path string) Field {
	return Field{
		Name:                  name,
		Path:                  path,
		GraphQLPath:           strings.ReplaceAll(path, ".", "__"),
		PayloadType:           TypeUnknown,
		PayloadTypeConfidence: ConfidenceUnknown,
		JSONType:              "unknown",
		RequiredSource:        SourceUnknown,
		LocalizedSource:       SourceUnknown,
		OptionsSource:         SourceNA,
		RelationToSource:      SourceNA,
		Queryable:             false,
		Operators:             nil,
		SortableConfidence:    ConfidenceUnknown,
		HookMutated:           HookMutatedUnknown,
		Label:                 HumanizeField(name),
	}
}

// BlockField is one blocks field's resolved answer, recorded per field
// because there is no such thing as a project-wide blockType list: verified
// live that Page.layout accepts 5 block types and Form.fields accepts 9
// entirely different ones.
type BlockField struct {
	Path string `json:"path"`
	// Slugs are the blockType values the REST API accepts here, or null when
	// none could be resolved.
	Slugs []string `json:"slugs"`
	// Source is the weakest provenance among Slugs, or "mixed".
	Source string `json:"source"`
	// SlugSources gives every slug its own provenance so a confirmed slug is
	// distinguishable from one inferred from an interfaceName.
	SlugSources map[string]string `json:"slug_sources"`
	// SlugInterfaces pairs each slug with the GraphQL union member it came
	// from, which is how a slug is joined to the block's field schema in
	// Shard.BlockSchemas.
	SlugInterfaces map[string]string `json:"slug_interface_names"`
	// InterfaceNames is the field's GraphQL union possibleTypes, verbatim.
	// They are NOT blockType slugs and must never be sent to the API.
	InterfaceNames []string `json:"interface_names"`
	// Unresolved lists union members no source could name; the field accepts
	// them but PayCLI cannot say what to call them.
	Unresolved []string `json:"unresolved_interface_names"`
	// Reason is empty exactly when every slug came from a confirmed source.
	Reason string `json:"reason"`
}

// Shard is one entity's field schema — the artefact decoded only for the
// collection named on the command line (§8.2).
type Shard struct {
	Generation string  `json:"generation"`
	Slug       string  `json:"slug"`
	SHA256     string  `json:"sha256"`
	Fields     []Field `json:"fields"`
	// JoinFields lists read-only join fields, which must be requested with
	// joins[field][limit] rather than selected like a normal field.
	JoinFields []string `json:"join_fields"`
	// Blocks maps a blocks field's path to its resolved blockType slugs. It is
	// null — not an empty map — when no source could resolve them, which is
	// the difference between "this project has no blocks" and "PayCLI does not
	// know" (§7.10). Every entry is resolved from THAT FIELD's own GraphQL
	// union, so two blocks fields on the same entity can and do carry
	// different lists.
	Blocks map[string][]string `json:"blocks"`
	// BlocksSource is the entity-wide summary: the one source when every
	// blocks field resolved the same way, "mixed" when they did not, "unknown"
	// when the entity has no resolved blocks field at all. It is a SUMMARY —
	// BlockFields[path].Source is the per-field answer and the one an agent
	// should read.
	BlocksSource string `json:"blocks_source"`
	// BlockFields carries the full per-field answer for every blocks field,
	// including each slug's own provenance and the GraphQL union the slugs
	// were derived from. It is null when the entity has no blocks field.
	BlockFields map[string]BlockField `json:"block_fields"`
	// BlockSchemas is every reachable block type's OWN field schema, keyed by
	// blockType slug — what is inside a cta, not merely that cta exists. It is
	// null on a shard written before the key existed and on a REST-only shard
	// (there is no union to read), which is why every consumer treats null as
	// "not discovered" and says so rather than as "this block has no fields".
	BlockSchemas map[string]BlockTypeSchema `json:"block_schemas"`
	// RequiredPaths is the flattened list of paths whose required is true.
	RequiredPaths []string `json:"required_paths"`

	// FieldDocs are the per-field human instructions this entity's own config
	// declares — `admin: { description: '…' }` — keyed by field path.
	//
	// It is a SEPARATE map rather than three more keys on every field entry,
	// and that is a size decision with a measured reason: `pay describe pages`
	// carries ~90 field entries, so three keys each would add ~16 KB to the
	// most frequently run command in order to publish null 90 times. Here the
	// cost is proportional to what the project actually documented, and is
	// nothing at all on a project that documented nothing.
	//
	// FieldDocsSource is what distinguishes "scanned, found none" from "never
	// scanned": a producer records no map rather than an empty one.
	FieldDocs map[string]FieldDoc `json:"field_docs,omitempty"`
	// FieldDocsSource is "project-source" when this entity's config was read
	// off disk and "unknown" when it was not — the normal case for a
	// plugin-provided collection, whose config lives in node_modules.
	FieldDocsSource string `json:"field_docs_source,omitempty"`
	// FieldDocsFile is the absolute path the docs were read from, "" when
	// there is none.
	FieldDocsFile string `json:"field_docs_file,omitempty"`
}

// FieldDoc is one field's human documentation as the project wrote it.
//
// Key is always reported with Description: `admin.description` is Payload's
// own field-level key, but PayCLI also accepts a `custom.*` neighbour, and an
// agent is entitled to know which one a sentence came from.
type FieldDoc struct {
	Description string `json:"description"`
	Key         string `json:"key"`
	// Source is "project-source"; it exists so that a single entry is
	// self-describing when it is lifted out of the map.
	Source string `json:"source"`
}

// NewShard returns an empty shard for a slug.
func NewShard(generation, slug string) *Shard {
	return &Shard{
		Generation:    generation,
		Slug:          slug,
		Fields:        []Field{},
		JoinFields:    []string{},
		Blocks:        nil,
		BlocksSource:  SourceUnknown,
		BlockFields:   nil,
		BlockSchemas:  nil,
		RequiredPaths: []string{},
	}
}

// Field returns the entry for a dotted path.
func (s *Shard) Field(path string) (Field, bool) {
	if s == nil {
		return Field{}, false
	}
	for _, f := range s.Fields {
		if f.Path == path {
			return f, true
		}
	}
	return Field{}, false
}

// SetBlockField records ONE blocks field's resolution and keeps every derived
// view of it consistent: block_fields[path] (the full answer), blocks[path]
// (the slug list §9.7 validates a --set blockType against) and blocks_source
// (the entity-wide summary).
//
// It is the only writer of those three keys, so they cannot drift apart and a
// producer cannot record slugs without also recording where they came from
// (§7.8.3).
func (s *Shard) SetBlockField(bf BlockField) {
	if s == nil || bf.Path == "" {
		return
	}
	if s.BlockFields == nil {
		s.BlockFields = map[string]BlockField{}
	}
	s.BlockFields[bf.Path] = bf
	if len(bf.Slugs) > 0 {
		if s.Blocks == nil {
			s.Blocks = map[string][]string{}
		}
		s.Blocks[bf.Path] = bf.Slugs
	} else {
		delete(s.Blocks, bf.Path)
		if len(s.Blocks) == 0 {
			// nil, not an empty map: §7.10's tri-state.
			s.Blocks = nil
		}
	}
	s.BlocksSource = s.blocksSourceSummary()
}

// blocksSourceSummary collapses the per-field sources into the one entity-wide
// string blocks_source has always been. It reports "mixed" rather than picking
// a winner when the fields disagree, because naming one source and hiding the
// other is how a global answer looked trustworthy while being wrong.
func (s *Shard) blocksSourceSummary() string {
	sources := map[string]bool{}
	for _, bf := range s.BlockFields {
		if len(bf.Slugs) == 0 {
			continue
		}
		sources[bf.Source] = true
	}
	switch len(sources) {
	case 0:
		return SourceUnknown
	case 1:
		for k := range sources {
			return k
		}
	}
	return SourceMixed
}

// SetFieldDocs records this entity's per-field documentation and the file it
// was read from, keeping the three keys that describe it consistent.
//
// It is the only writer of them, so a producer cannot record documentation
// without also recording where it came from (§7.8.3). Calling it with an empty
// map still records the SOURCE: "the config was read and documents nothing" is
// a different, and more useful, answer than "no config was read".
func (s *Shard) SetFieldDocs(docs map[string]FieldDoc, file, source string) {
	if s == nil {
		return
	}
	s.FieldDocsSource = orUnknownSource(source)
	s.FieldDocsFile = file
	if len(docs) == 0 {
		s.FieldDocs = nil
		return
	}
	s.FieldDocs = docs
}

// FieldDocFor returns one field path's documentation and whether the project
// declared any. The bool is the tri-state: false is "nobody wrote one", never
// an empty sentence.
func (s *Shard) FieldDocFor(path string) (FieldDoc, bool) {
	if s == nil || len(s.FieldDocs) == 0 {
		return FieldDoc{}, false
	}
	d, ok := s.FieldDocs[path]
	return d, ok && d.Description != ""
}

// DocumentedPaths lists every field path carrying a description, sorted.
func (s *Shard) DocumentedPaths() []string {
	out := []string{}
	if s == nil {
		return out
	}
	for path, d := range s.FieldDocs {
		if d.Description != "" {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// BlockTypesFor returns the blockType slugs one field accepts, and whether
// PayCLI knows. The bool is the §7.10 tri-state: false means unknown, not
// "accepts nothing".
func (s *Shard) BlockTypesFor(path string) ([]string, bool) {
	if s == nil {
		return nil, false
	}
	slugs, ok := s.Blocks[path]
	return slugs, ok && len(slugs) > 0
}

// BlockSchemaFor returns one blockType's field schema and whether the shard
// carries it. The bool is the tri-state: false is "PayCLI did not discover
// this block's interior", never "the block has no fields".
func (s *Shard) BlockSchemaFor(slug string) (BlockTypeSchema, bool) {
	if s == nil {
		return BlockTypeSchema{}, false
	}
	bs, ok := s.BlockSchemas[slug]
	if ok {
		bs.normalizeDocs()
	}
	return bs, ok
}

// BlockSlugsFor returns every blockType slug reachable from any blocks field
// of this entity, sorted and deduplicated.
func (s *Shard) BlockSlugsFor() []string {
	if s == nil {
		return nil
	}
	seen := map[string]bool{}
	out := []string{}
	for _, bf := range s.BlockFields {
		for _, slug := range bf.Slugs {
			if !seen[slug] {
				seen[slug] = true
				out = append(out, slug)
			}
		}
	}
	sort.Strings(out)
	return out
}

// BlockFieldFor returns the full per-field record for a blocks field.
func (s *Shard) BlockFieldFor(path string) (BlockField, bool) {
	if s == nil {
		return BlockField{}, false
	}
	bf, ok := s.BlockFields[path]
	return bf, ok
}

// Paths returns every field path in shard order.
func (s *Shard) Paths() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.Fields))
	for _, f := range s.Fields {
		out = append(out, f.Path)
	}
	return out
}

// QueryablePaths returns the subset that Payload's where parser accepts. It
// feeds query.Schema, whose nil-means-unknown contract this preserves: an
// empty shard yields nil, never an empty non-nil slice that would reject
// everything.
func (s *Shard) QueryablePaths() []string {
	if s == nil || len(s.Fields) == 0 {
		return nil
	}
	out := []string{}
	for _, f := range s.Fields {
		if f.Queryable {
			out = append(out, f.Path)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// SortablePaths returns the sortable subset, with the same nil contract.
func (s *Shard) SortablePaths() []string {
	if s == nil || len(s.Fields) == 0 {
		return nil
	}
	out := []string{}
	for _, f := range s.Fields {
		if f.Sortable {
			out = append(out, f.Path)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// DateFields returns every field whose payload_type is date, in shard order.
func (s *Shard) DateFields() []string {
	if s == nil {
		return nil
	}
	out := []string{}
	for _, f := range s.Fields {
		if f.PayloadType == TypeDate {
			out = append(out, f.Path)
		}
	}
	return out
}

// Finalize sorts the derived lists, recomputes required_paths and stamps the
// content hash. It is the single place a shard becomes publishable, so a
// caller cannot forget the hash the index refers to.
func (s *Shard) Finalize() {
	if s == nil {
		return
	}
	if s.Fields == nil {
		s.Fields = []Field{}
	}
	if s.JoinFields == nil {
		s.JoinFields = []string{}
	}
	required := []string{}
	for _, f := range s.Fields {
		if f.Required != nil && *f.Required {
			required = append(required, f.Path)
		}
	}
	sort.Strings(required)
	s.RequiredPaths = required
	sort.Strings(s.JoinFields)
	s.SHA256 = ""
	s.SHA256 = HashShard(s)
}

// HashShard is the content hash written to fields_sha256. The hash covers the
// fields and their provenance but not the generation or the hash slot itself,
// so an unchanged schema keeps a stable hash across runs and §8.4's staleness
// check cannot be defeated by a new generation id.
func HashShard(s *Shard) string {
	if s == nil {
		return ""
	}
	payload := struct {
		Slug          string                     `json:"slug"`
		Fields        []Field                    `json:"fields"`
		JoinFields    []string                   `json:"join_fields"`
		Blocks        map[string][]string        `json:"blocks"`
		BlocksSource  string                     `json:"blocks_source"`
		BlockFields   map[string]BlockField      `json:"block_fields"`
		BlockSchemas  map[string]BlockTypeSchema `json:"block_schemas"`
		RequiredPaths []string                   `json:"required_paths"`
		FieldDocs     map[string]FieldDoc        `json:"field_docs"`
	}{s.Slug, s.Fields, s.JoinFields, s.Blocks, s.BlocksSource, s.BlockFields, s.BlockSchemas,
		s.RequiredPaths, s.FieldDocs}
	b, err := json.Marshal(payload)
	if err != nil {
		// Field contains only JSON-safe types, so this cannot happen; hashing
		// the slug keeps the function total rather than panicking.
		b = []byte(s.Slug)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// WriteShapeFor implements §7.8.2's closed enum. It is the only producer of
// the key, so a relationship can never be written without one.
func WriteShapeFor(payloadType string, polymorphic, hasMany bool) *string {
	switch payloadType {
	case TypeRelationship, TypeUpload:
		switch {
		case polymorphic && hasMany:
			return strPtr(WriteShapeRelList)
		case polymorphic:
			return strPtr(WriteShapeRelValue)
		case hasMany:
			return strPtr(WriteShapeIDArray)
		default:
			return strPtr(WriteShapeID)
		}
	case TypeRichText, TypeJSON, TypeBlocks:
		return strPtr(WriteShapeJSON)
	default:
		return nil
	}
}

// HumanizeField turns a camelCase or snake_case field name into an admin-ish
// label. It is a display aid only: Payload's real labels are not introspectable
// per field, and nothing behavioural keys off this string.
func HumanizeField(name string) string {
	if name == "" {
		return ""
	}
	s := strings.NewReplacer("_", " ", "-", " ").Replace(name)
	s = splitCamel(s)
	parts := strings.Fields(s)
	for i, p := range parts {
		parts[i] = titleWord(p)
	}
	return strings.Join(parts, " ")
}

// splitCamel inserts spaces at lower->upper boundaries and before the last
// capital of an acronym run followed by a lowercase letter (APIKey -> API Key).
func splitCamel(s string) string {
	var b strings.Builder
	r := []rune(s)
	for i, c := range r {
		if i > 0 && isUpper(c) {
			prev := r[i-1]
			switch {
			case !isUpper(prev) && prev != ' ':
				b.WriteRune(' ')
			case isUpper(prev) && i+1 < len(r) && isLower(r[i+1]):
				b.WriteRune(' ')
			}
		}
		b.WriteRune(c)
	}
	return b.String()
}

func isUpper(r rune) bool { return r >= 'A' && r <= 'Z' }
func isLower(r rune) bool { return r >= 'a' && r <= 'z' }

func titleWord(w string) string {
	if w == "" {
		return w
	}
	r := []rune(w)
	if isLower(r[0]) {
		r[0] = r[0] - 'a' + 'A'
	}
	return string(r)
}
