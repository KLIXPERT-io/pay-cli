package discovery

import (
	"encoding/json"
	"regexp"
	"strings"
)

// IDTypeFromArg reads §7.3's authoritative signal: the `id` argument of the
// singular Query field. Int means a relational adapter's integer primary key,
// String means Mongo ObjectIds or UUIDs.
//
// This matters beyond validation: passing a non-castable id causes 500s, and on
// /duplicate it caused unintended writes.
func IDTypeFromArg(f *IntroField) (string, bool) {
	arg, ok := f.Arg("id")
	if !ok {
		return IDTypeUnknown, false
	}
	name, kind, _, _ := arg.Type.Named()
	if kind != KindScalar && kind != "" {
		return IDTypeUnknown, false
	}
	switch name {
	case "Int", "Float":
		return IDTypeNumber, true
	case "String", "ID":
		return IDTypeString, true
	default:
		return IDTypeUnknown, false
	}
}

// IDTypeOfJSON types a decoded JSON id value. json.Number and float64 are both
// accepted because a decoder configured with UseNumber and one without must
// agree.
func IDTypeOfJSON(v any) (string, bool) {
	switch v.(type) {
	case json.Number, float64, int, int64:
		return IDTypeNumber, true
	case string:
		return IDTypeString, true
	default:
		return IDTypeUnknown, false
	}
}

// IDLadderInput is §7.6's REST id_type ladder, expressed as already-fetched
// samples so the ladder itself is pure and table-testable.
type IDLadderInput struct {
	// GraphQL is the Stage-1 answer, "" when GraphQL was unavailable.
	GraphQL string
	// SampleID is docs[0].id from GET /{slug}?limit=1&depth=0&select[id]=true.
	SampleID any
	// SampleIDPresent distinguishes "the collection is empty" from "id was
	// null".
	SampleIDPresent bool
	// VersionParentID is docs[0].parent from /{slug}/versions, which is the
	// *document* id while the version's own id is a different row.
	VersionParentID      any
	VersionParentPresent bool
	// Configured is the profile's id_type pin.
	Configured string
}

// ResolveIDType walks §7.6's ladder, stopping at the first hit. The last rung
// is "unknown" — reachable on purpose, because a defaulted "number" would make
// PayCLI reject every valid 24-hex ObjectId with an error message that is a
// lie, and a defaulted "string" would silently lose the Postgres protection the
// check exists for.
func ResolveIDType(in IDLadderInput) (idType, source string) {
	if in.GraphQL == IDTypeNumber || in.GraphQL == IDTypeString {
		return in.GraphQL, SourceGraphQL
	}
	if in.SampleIDPresent {
		if t, ok := IDTypeOfJSON(in.SampleID); ok {
			return t, SourceObserved
		}
	}
	if in.VersionParentPresent {
		if t, ok := IDTypeOfJSON(in.VersionParentID); ok {
			return t, SourceObserved
		}
	}
	if in.Configured == IDTypeNumber || in.Configured == IDTypeString {
		return in.Configured, SourceConfigured
	}
	return IDTypeUnknown, SourceUnknown
}

var (
	objectIDRe = regexp.MustCompile(`^[0-9a-fA-F]{24}$`)
	uuidRe     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// DB adapter values (§7.11).
const (
	DBPostgres = "postgres"
	DBMongoDB  = "mongodb"
	DBSQLite   = "sqlite"
	DBUnknown  = "unknown"
)

// InferDBAdapter is §7.11 source 3: the only inference the API supports, and
// it is deliberately weak. It never unlocks §9.3's client-side
// unsupported_operator block, which requires db_adapter_source ==
// "configured" — because on MongoDB `all` maps to $all and works, so a guessed
// adapter would make PayCLI refuse a query the server would have answered.
//
// idTypes are the resolved id_types of every collection; sampleIDs are any
// observed string ids.
func InferDBAdapter(idTypes []string, sampleIDs []string) (adapter, source string) {
	if len(idTypes) == 0 {
		return DBUnknown, SourceUnknown
	}
	numbers, strs := 0, 0
	for _, t := range idTypes {
		switch t {
		case IDTypeNumber:
			numbers++
		case IDTypeString:
			strs++
		}
	}
	if numbers > 0 && strs == 0 {
		return DBPostgres, SourceInferred
	}
	if strs > 0 && numbers == 0 {
		objectIDs, uuids := 0, 0
		for _, id := range sampleIDs {
			switch {
			case objectIDRe.MatchString(id):
				objectIDs++
			case uuidRe.MatchString(id):
				uuids++
			}
		}
		switch {
		case objectIDs > 0 && uuids == 0:
			return DBMongoDB, SourceInferred
		case uuids > 0 && objectIDs == 0:
			return DBPostgres, SourceInferred
		}
	}
	return DBUnknown, SourceUnknown
}

// IDString renders an observed id for the adapter inference without going
// through fmt, so a json.Number keeps its exact digits.
func IDString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	default:
		return ""
	}
}

// LooksNumeric reports whether a raw command-line id is all digits, used by
// callers that must decide whether a string id is safe to send to a
// number-typed collection.
func LooksNumeric(s string) bool {
	if s == "" {
		return false
	}
	s = strings.TrimPrefix(s, "-")
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
