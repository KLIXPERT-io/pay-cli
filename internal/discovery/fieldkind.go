package discovery

import "strings"

// GraphQL introspection kinds.
const (
	KindScalar      = "SCALAR"
	KindObject      = "OBJECT"
	KindInterface   = "INTERFACE"
	KindUnion       = "UNION"
	KindEnum        = "ENUM"
	KindInputObject = "INPUT_OBJECT"
	KindList        = "LIST"
	KindNonNull     = "NON_NULL"
)

// TypeRef is a GraphQL type reference as introspection returns it: a chain of
// NON_NULL / LIST wrappers around one named type.
type TypeRef struct {
	Kind   string   `json:"kind"`
	Name   string   `json:"name"`
	OfType *TypeRef `json:"ofType"`
}

// Named walks the wrapper chain and returns the innermost named type together
// with the two facts the wrappers carry: whether the outermost position is
// NON_NULL (required) and whether a LIST appeared anywhere (has_many).
func (t *TypeRef) Named() (name, kind string, nonNull, list bool) {
	if t == nil {
		return "", "", false, false
	}
	cur := t
	nonNull = cur.Kind == KindNonNull
	for cur != nil {
		switch cur.Kind {
		case KindNonNull:
			cur = cur.OfType
		case KindList:
			list = true
			cur = cur.OfType
		default:
			return cur.Name, cur.Kind, nonNull, list
		}
	}
	return "", "", nonNull, list
}

// IntroField is one field of an OBJECT type.
type IntroField struct {
	Name string     `json:"name"`
	Args []IntroArg `json:"args"`
	Type *TypeRef   `json:"type"`
}

// Arg returns a named argument.
func (f *IntroField) Arg(name string) (IntroArg, bool) {
	if f == nil {
		return IntroArg{}, false
	}
	for _, a := range f.Args {
		if a.Name == name {
			return a, true
		}
	}
	return IntroArg{}, false
}

// HasArg reports whether a named argument exists.
func (f *IntroField) HasArg(name string) bool {
	_, ok := f.Arg(name)
	return ok
}

// IntroArg is one field argument.
type IntroArg struct {
	Name string   `json:"name"`
	Type *TypeRef `json:"type"`
}

// IntroInputField is one field of an INPUT_OBJECT type.
type IntroInputField struct {
	Name string   `json:"name"`
	Type *TypeRef `json:"type"`
}

// IntroEnumValue is one ENUM value.
type IntroEnumValue struct {
	Name string `json:"name"`
}

// IntroTypeName is a bare type reference in possibleTypes.
type IntroTypeName struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// IntroType is one __type(name:) result.
type IntroType struct {
	Kind          string            `json:"kind"`
	Name          string            `json:"name"`
	Fields        []IntroField      `json:"fields"`
	InputFields   []IntroInputField `json:"inputFields"`
	EnumValues    []IntroEnumValue  `json:"enumValues"`
	PossibleTypes []IntroTypeName   `json:"possibleTypes"`
}

// FieldNames returns the OBJECT field names.
func (t *IntroType) FieldNames() []string {
	if t == nil {
		return nil
	}
	out := make([]string, 0, len(t.Fields))
	for _, f := range t.Fields {
		out = append(out, f.Name)
	}
	return out
}

// InputFieldNames returns the INPUT_OBJECT field names.
func (t *IntroType) InputFieldNames() []string {
	if t == nil {
		return nil
	}
	out := make([]string, 0, len(t.InputFields))
	for _, f := range t.InputFields {
		out = append(out, f.Name)
	}
	return out
}

// EnumNames returns the ENUM value names.
func (t *IntroType) EnumNames() []string {
	if t == nil {
		return nil
	}
	out := make([]string, 0, len(t.EnumValues))
	for _, v := range t.EnumValues {
		out = append(out, v.Name)
	}
	return out
}

// PossibleTypeNames returns a UNION's member names, preserving schema order —
// the *_RelationTo enum and its matching union list their members in the same
// order, and the polymorphic target mapping depends on that.
func (t *IntroType) PossibleTypeNames() []string {
	if t == nil {
		return nil
	}
	out := make([]string, 0, len(t.PossibleTypes))
	for _, p := range t.PossibleTypes {
		out = append(out, p.Name)
	}
	return out
}

// uploadMarkers are the four fields Payload adds to an upload collection's
// object type. Their joint presence is what distinguishes an upload target
// from an ordinary relationship target (§7.4).
var uploadMarkers = []string{"filename", "mimeType", "filesize", "url"}

// IsUploadType reports whether an OBJECT type carries all four upload markers.
func IsUploadType(t *IntroType) bool {
	if t == nil || len(t.Fields) == 0 {
		return false
	}
	have := make(map[string]bool, len(t.Fields))
	for _, f := range t.Fields {
		have[f.Name] = true
	}
	for _, m := range uploadMarkers {
		if !have[m] {
			return false
		}
	}
	return true
}

// joinMarkers are the three fields of Payload's paginated join object.
var joinMarkers = []string{"docs", "hasNextPage", "totalDocs"}

// IsJoinType reports §7.4's "OBJECT with exactly {docs, hasNextPage,
// totalDocs}". The check is on the key set, not on the type name, because the
// name is {T}_{field} and a group field can share that shape of name.
func IsJoinType(t *IntroType) bool {
	if t == nil || len(t.Fields) != len(joinMarkers) {
		return false
	}
	have := make(map[string]bool, len(t.Fields))
	for _, f := range t.Fields {
		have[f.Name] = true
	}
	for _, m := range joinMarkers {
		if !have[m] {
			return false
		}
	}
	return true
}

// IsRelationshipType reports §7.4's polymorphic wrapper: an OBJECT whose only
// fields are relationTo and value.
func IsRelationshipType(t *IntroType) bool {
	if t == nil || len(t.Fields) != 2 {
		return false
	}
	have := make(map[string]bool, 2)
	for _, f := range t.Fields {
		have[f.Name] = true
	}
	return have["relationTo"] && have["value"]
}

// Schema is everything the field-kind inference needs to resolve a named type
// without another round trip.
type Schema struct {
	// Types are the resolved __type results, keyed by type name.
	Types map[string]*IntroType
	// SlugBySingular maps a GraphQL singular type name back to its slug, so a
	// monomorphic relationship target is reported as a slug, never as a type
	// name an agent cannot pass on the command line.
	SlugBySingular map[string]string
}

// Type looks up a resolved type.
func (s *Schema) Type(name string) *IntroType {
	if s == nil || name == "" {
		return nil
	}
	return s.Types[name]
}

// Kind is the result of §7.4's field-kind inference table.
type Kind struct {
	PayloadType string
	Confidence  string
	JSONType    string
	HasMany     bool
	ReadOnly    bool
	Polymorphic bool

	Options       []string
	OptionsSource string

	RelationTo       []string
	RelationToSource string

	// Blocks is the UNION member list for a blocks field. These are
	// interfaceNames, NOT blockType slugs — §7.10 resolves the real slugs from
	// project source.
	BlockInterfaces []string

	// Children is the named type whose fields should be walked as nested
	// group/array entries, or "" when there are none.
	Children string
	// Leaves are type names this inference needs resolved to be complete.
	Leaves []string
}

// InferKind implements §7.4's table. It is pure: everything it needs is either
// in the TypeRef or already in the Schema, and anything missing is reported
// through Leaves so the caller can batch one more __type round.
//
// ownerSingular is the entity's GraphQL type name, used only to recognise the
// {Owner}_{field}* naming convention.
func InferKind(ref *TypeRef, ownerSingular, fieldName string, s *Schema) Kind {
	name, kind, _, list := ref.Named()
	k := Kind{
		PayloadType:      TypeUnknown,
		Confidence:       ConfidenceUnknown,
		JSONType:         "unknown",
		HasMany:          list,
		OptionsSource:    SourceNA,
		RelationToSource: SourceNA,
	}
	switch kind {
	case KindEnum:
		// ENUM {T}_{field} is a select or a radio; introspection cannot tell
		// the two apart (both compile to the same enum), so PayCLI reports the
		// one that behaves identically on the wire.
		k.PayloadType = TypeSelect
		k.Confidence = ConfidenceInferred
		k.JSONType = "string"
		if t := s.Type(name); t != nil {
			k.Options = t.EnumNames()
			k.OptionsSource = SourceGraphQLEnum
		} else {
			k.Leaves = append(k.Leaves, name)
			k.OptionsSource = SourceUnknown
		}
	case KindUnion:
		k.PayloadType = TypeBlocks
		k.Confidence = ConfidenceGraphQL
		k.JSONType = "array"
		k.HasMany = true
		if t := s.Type(name); t != nil {
			k.BlockInterfaces = t.PossibleTypeNames()
		} else {
			k.Leaves = append(k.Leaves, name)
		}
	case KindScalar:
		k.PayloadType, k.JSONType = scalarKind(name, fieldName)
		k.Confidence = ConfidenceGraphQL
		if k.PayloadType == TypeRichText || k.PayloadType == TypeJSON {
			// The JSON scalar is the one place GraphQL genuinely cannot tell
			// lexical richText from a json field (RICHTEXT_SHAPE_UNKNOWN).
			k.Confidence = ConfidenceInferred
		}
	case KindObject, KindInterface:
		k = inferObjectKind(k, name, ownerSingular, fieldName, s)
	case "":
		// A field with no resolvable type reference. Honest unknown.
	}
	if k.PayloadType == TypeID {
		k.ReadOnly = true
	}
	return k
}

func inferObjectKind(k Kind, name, ownerSingular, fieldName string, s *Schema) Kind {
	t := s.Type(name)
	if t == nil {
		k.Leaves = append(k.Leaves, name)
		// Name-shape fallbacks so a missing leaf still produces a usable
		// answer rather than "unknown".
		switch {
		case strings.HasSuffix(name, "_Relationship"):
			k.PayloadType = TypeRelationship
			k.Polymorphic = true
			k.Confidence = ConfidenceInferred
			k.JSONType = "object"
			k.RelationToSource = SourceUnknown
		case slugFor(s, name) != "":
			k.PayloadType = TypeRelationship
			k.Confidence = ConfidenceGraphQL
			k.JSONType = numberOrObject
			k.RelationTo = []string{slugFor(s, name)}
			k.RelationToSource = SourceGraphQL
		default:
			k.PayloadType = TypeGroup
			k.Confidence = ConfidenceInferred
			k.JSONType = "object"
			k.Children = name
		}
		return k
	}

	switch {
	case IsJoinType(t):
		// A join is read-only and is requested with joins[field][limit], not
		// selected like a normal field.
		k.PayloadType = TypeJoin
		k.Confidence = ConfidenceGraphQL
		k.JSONType = "object"
		k.ReadOnly = true
		k.HasMany = true
	case IsRelationshipType(t):
		k.Polymorphic = true
		k.Confidence = ConfidenceGraphQL
		k.JSONType = "object"
		k.PayloadType = TypeRelationship
		targets, src, leaves := polymorphicTargets(t, s)
		k.RelationTo, k.RelationToSource = targets, src
		k.Leaves = append(k.Leaves, leaves...)
		if allUploads(targets, s) {
			k.PayloadType = TypeUpload
		}
	case slugFor(s, name) != "":
		k.Confidence = ConfidenceGraphQL
		k.JSONType = numberOrObject
		k.RelationTo = []string{slugFor(s, name)}
		k.RelationToSource = SourceGraphQL
		if IsUploadType(t) {
			k.PayloadType = TypeUpload
		} else {
			k.PayloadType = TypeRelationship
		}
	case isPointType(t):
		k.PayloadType = TypePoint
		k.Confidence = ConfidenceInferred
		k.JSONType = "array"
	default:
		// A nested group (or an array's row type — GraphQL models both as an
		// OBJECT named {Owner}_{field}, and the LIST wrapper is what tells
		// them apart).
		if k.HasMany {
			k.PayloadType = TypeArray
			k.JSONType = "array"
		} else {
			k.PayloadType = TypeGroup
			k.JSONType = "object"
		}
		k.Confidence = ConfidenceInferred
		k.Children = name
		_ = ownerSingular
		_ = fieldName
	}
	return k
}

// numberOrObject is the json_type of a relationship: a bare id at depth 0 and
// the populated document at depth > 0. Writing it as one string keeps the key
// total without lying about either shape.
const numberOrObject = "number|string|object"

// polymorphicTargets reads the {T}_{field}_RelationTo enum and its matching
// union, which list their members in the same order.
func polymorphicTargets(t *IntroType, s *Schema) (targets []string, source string, leaves []string) {
	var relTo, value *TypeRef
	for i := range t.Fields {
		switch t.Fields[i].Name {
		case "relationTo":
			relTo = t.Fields[i].Type
		case "value":
			value = t.Fields[i].Type
		}
	}
	enumName, _, _, _ := relTo.Named()
	unionName, _, _, _ := value.Named()

	enum := s.Type(enumName)
	union := s.Type(unionName)
	if enum == nil && enumName != "" {
		leaves = append(leaves, enumName)
	}
	if union == nil && unionName != "" {
		leaves = append(leaves, unionName)
	}
	if union != nil {
		// The union names the target *types*; mapping them back through
		// SlugBySingular yields the slugs an agent can actually pass.
		out := []string{}
		for _, m := range union.PossibleTypeNames() {
			if slug := slugFor(s, m); slug != "" {
				out = append(out, slug)
			}
		}
		if len(out) > 0 {
			return out, SourceGraphQL, leaves
		}
	}
	if enum != nil {
		// The enum values are formatName()-mangled slugs. They are usable only
		// when they round-trip to a slug PayCLI already knows; anything else
		// would be a guess.
		out := []string{}
		for _, v := range enum.EnumNames() {
			if slug := slugForMangled(s, v); slug != "" {
				out = append(out, slug)
			}
		}
		if len(out) > 0 {
			return out, SourceGraphQL, leaves
		}
	}
	return nil, SourceUnknown, leaves
}

func allUploads(slugs []string, s *Schema) bool {
	if len(slugs) == 0 || s == nil {
		return false
	}
	for _, slug := range slugs {
		singular := ""
		for sing, sl := range s.SlugBySingular {
			if sl == slug {
				singular = sing
				break
			}
		}
		if !IsUploadType(s.Type(singular)) {
			return false
		}
	}
	return true
}

func slugFor(s *Schema, typeName string) string {
	if s == nil || typeName == "" {
		return ""
	}
	return s.SlugBySingular[typeName]
}

func slugForMangled(s *Schema, mangled string) string {
	if s == nil || mangled == "" {
		return ""
	}
	for _, slug := range s.SlugBySingular {
		if FormatName(slug) == mangled {
			return slug
		}
	}
	return ""
}

// isPointType recognises Payload's point field, which GraphQL models as an
// OBJECT with only coordinates.
func isPointType(t *IntroType) bool {
	if t == nil || len(t.Fields) == 0 || len(t.Fields) > 2 {
		return false
	}
	for _, f := range t.Fields {
		if f.Name != "coordinates" && f.Name != "type" {
			return false
		}
	}
	for _, f := range t.Fields {
		if f.Name == "coordinates" {
			return true
		}
	}
	return false
}

// scalarKind is §7.4's scalar row. The JSON scalar is the one genuinely
// ambiguous case: lexical richText and a json field are the same scalar, so the
// field name is used as a tie-break and the confidence drops to "inferred".
func scalarKind(name, fieldName string) (payloadType, jsonType string) {
	if fieldName == "id" {
		// The primary key is reported as `id` whatever scalar carries it, so a
		// consumer branches on id_type rather than on whether this project
		// happens to use Int or String keys.
		switch name {
		case "Int", "Float":
			return TypeID, "number"
		default:
			return TypeID, "string"
		}
	}
	switch name {
	case "DateTime":
		return TypeDate, "string"
	case "EmailAddress":
		return TypeEmail, "string"
	case "Int":
		return TypeNumber, "number"
	case "Float":
		return TypeNumber, "number"
	case "Boolean":
		return TypeCheckbox, "boolean"
	case "JSON":
		if looksRichText(fieldName) {
			return TypeRichText, "object"
		}
		return TypeJSON, "object"
	case "ID":
		return TypeID, "number|string"
	case "String":
		return TypeText, "string"
	default:
		return TypeText, "string"
	}
}

// richTextNames are the conventional lexical field names. This only moves the
// payload_type label between two values that are written identically (both are
// sent as an object, never a string), so a miss is cosmetic.
var richTextNames = []string{"richtext", "content", "body", "caption", "intro", "description"}

func looksRichText(fieldName string) bool {
	lower := strings.ToLower(fieldName)
	for _, n := range richTextNames {
		if lower == n || strings.HasSuffix(lower, "richtext") {
			return true
		}
	}
	return false
}

// SortableKind reports §7.5's sortability heuristic. It is a heuristic, not a
// measurement, which is exactly why every field records
// sortable_confidence: "heuristic" and the manifest carries
// SORTABILITY_HEURISTIC.
func SortableKind(payloadType string) bool {
	switch payloadType {
	case TypeJoin, TypeBlocks, TypeRichText, TypeJSON, TypeGroup, TypeArray, TypePoint:
		return false
	default:
		return true
	}
}
