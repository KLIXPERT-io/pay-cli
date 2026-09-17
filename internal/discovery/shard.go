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
	// know" (§7.10).
	Blocks       map[string][]string `json:"blocks"`
	BlocksSource string              `json:"blocks_source"`
	// RequiredPaths is the flattened list of paths whose required is true.
	RequiredPaths []string `json:"required_paths"`
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
		Slug          string              `json:"slug"`
		Fields        []Field             `json:"fields"`
		JoinFields    []string            `json:"join_fields"`
		Blocks        map[string][]string `json:"blocks"`
		BlocksSource  string              `json:"blocks_source"`
		RequiredPaths []string            `json:"required_paths"`
	}{s.Slug, s.Fields, s.JoinFields, s.Blocks, s.BlocksSource, s.RequiredPaths}
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
