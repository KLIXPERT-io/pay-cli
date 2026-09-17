package query

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// The 16 operators Payload implements (§2.3, fixed list).
const (
	OpEquals           = "equals"
	OpNotEquals        = "not_equals"
	OpGreaterThan      = "greater_than"
	OpGreaterThanEqual = "greater_than_equal"
	OpLessThan         = "less_than"
	OpLessThanEqual    = "less_than_equal"
	OpLike             = "like"
	OpNotLike          = "not_like"
	OpContains         = "contains"
	OpIn               = "in"
	OpNotIn            = "not_in"
	OpAll              = "all"
	OpExists           = "exists"
	OpNear             = "near"
	OpWithin           = "within"
	OpIntersects       = "intersects"
)

// Operators is the canonical operator list, sorted.
var Operators = []string{
	OpAll, OpContains, OpEquals, OpExists, OpGreaterThan, OpGreaterThanEqual,
	OpIn, OpIntersects, OpLessThan, OpLessThanEqual, OpLike, OpNear,
	OpNotEquals, OpNotIn, OpNotLike, OpWithin,
}

// valueMode says how the right-hand side of a term is turned into JSON.
type valueMode int

const (
	modeScalar   valueMode = iota // §9.4 value typing
	modeNumeric                   // number or date, still §9.4-typed
	modeEscaped                   // contains: % _ \ are escaped
	modeLikePass                  // like: passed through verbatim
	modeLikeWrap                  // not_like: auto-wrapped in %…%
	modeList                      // in / not_in / all
	modeBool                      // exists
	modeNear                      // "lng,lat,maxMeters[,minMeters]"
	modeGeo                       // GeoJSON literal or @file.geojson
)

type opSpec struct {
	canonical string
	mode      valueMode
	aliases   []string
}

var opSpecs = []opSpec{
	{OpEquals, modeScalar, []string{"eq", "=", "is"}},
	{OpNotEquals, modeScalar, []string{"ne", "!=", "not"}},
	{OpGreaterThan, modeNumeric, []string{"gt", ">"}},
	{OpGreaterThanEqual, modeNumeric, []string{"gte", ">="}},
	{OpLessThan, modeNumeric, []string{"lt", "<"}},
	{OpLessThanEqual, modeNumeric, []string{"lte", "<="}},
	{OpContains, modeEscaped, []string{"~"}},
	{OpLike, modeLikePass, nil},
	{OpNotLike, modeLikeWrap, []string{"nlike", "!~"}},
	{OpIn, modeList, nil},
	{OpNotIn, modeList, []string{"nin"}},
	{OpAll, modeList, nil},
	{OpExists, modeBool, nil},
	{OpNear, modeNear, nil},
	{OpWithin, modeGeo, nil},
	{OpIntersects, modeGeo, nil},
}

var (
	byAlias  = map[string]opSpec{}
	aliasSet []string
)

func init() {
	for _, s := range opSpecs {
		byAlias[s.canonical] = s
		for _, a := range s.aliases {
			byAlias[a] = s
		}
	}
	for a := range byAlias {
		aliasSet = append(aliasSet, a)
	}
	sort.Strings(aliasSet)
}

// KnownAliases lists every accepted spelling of every operator, sorted.
func KnownAliases() []string { return append([]string(nil), aliasSet...) }

// Canonical resolves an alias to the Payload operator it means.
func Canonical(alias string) (string, bool) {
	s, ok := byAlias[strings.ToLower(strings.TrimSpace(alias))]
	if !ok {
		return "", false
	}
	return s.canonical, true
}

// IsOperator reports whether s is a canonical Payload operator.
func IsOperator(s string) bool {
	spec, ok := byAlias[s]
	return ok && spec.canonical == s
}

// adapterGated are the operators verified to return HTTP 500 on the Postgres
// adapter (§2.3, §9.3). `all` works on Mongo, so the block is adapter-specific
// and only fires when the adapter is actually known.
var adapterGated = map[string]bool{
	OpAll:        true,
	OpNear:       true,
	OpWithin:     true,
	OpIntersects: true,
}

// CheckOperator implements §9.3's tri-state rule for adapter-specific
// operators: reject locally only when PayCLI actually knows the adapter is
// Postgres. "unknown" means send it and learn.
func CheckOperator(op, dbAdapter, adapterSource string) error {
	if !adapterGated[op] {
		return nil
	}
	if dbAdapter != "postgres" || adapterSource != "configured" {
		return nil
	}
	return apierr.New(apierr.CodeUnsupportedOperator,
		"the %q operator is not implemented by this project's Postgres adapter (it returns HTTP 500)", op).
		WithHint("use `in` for a subset match, or query the relationship from the other side")
}

// ParseOptions tunes term parsing. The zero value is the safe default.
type ParseOptions struct {
	// ReadFile resolves an @file.geojson argument. Nil means @file is refused
	// rather than silently treated as a string — the query package performs no
	// I/O of its own.
	ReadFile func(path string) ([]byte, error)
	// NoEscapeContains disables the `contains` escaping for a caller that
	// genuinely wants LIKE wildcards.
	NoEscapeContains bool
	// NoWrapNotLike disables the not_like auto-wrap.
	NoWrapNotLike bool
}

// BuildTerm compiles one path/operator/value triple into a Where clause.
func BuildTerm(path, alias, raw string, opts ParseOptions) (Where, error) {
	spec, ok := byAlias[strings.ToLower(strings.TrimSpace(alias))]
	if !ok {
		return nil, apierr.New(apierr.CodeUnsupportedOperator, "unknown operator %q in --where %q", alias, path+" "+alias+" "+raw).
			WithDidYouMean(apierr.DidYouMean(alias, aliasSet)...).
			WithHint("%s", "operators: "+strings.Join(Operators, " ")+" (aliases: eq = is, ne != not, gt >, gte >=, lt <, lte <=, contains ~, nlike !~, nin)")
	}
	v, err := operandFor(spec, path, raw, opts)
	if err != nil {
		return nil, err
	}
	return Term(path, spec.canonical, v), nil
}

func operandFor(spec opSpec, path, raw string, opts ParseOptions) (any, error) {
	switch spec.mode {
	case modeBool:
		if strings.TrimSpace(raw) == "" {
			return true, nil // `exists` with no value means true (§9.4)
		}
		b, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return nil, apierr.New(apierr.CodeInvalidWhereSyntax,
				"the `exists` operator takes true or false; got %q", raw).
				WithHint("%s", "write `--where '"+path+" exists false'` (the value may also be omitted, which means true)")
		}
		return b, nil

	case modeList:
		return listOperand(path, raw)

	case modeEscaped:
		v, err := TypeValue(raw)
		if err != nil {
			return nil, err
		}
		s, isStr := v.(string)
		if !isStr || opts.NoEscapeContains {
			return v, nil
		}
		// Verified: contains=% matched all 104 rows; contains=\% matched 0.
		return EscapeLike(s), nil

	case modeLikeWrap:
		v, err := TypeValue(raw)
		if err != nil {
			return nil, err
		}
		s, isStr := v.(string)
		if !isStr || opts.NoWrapNotLike {
			return v, nil
		}
		// Verified: not_like=hopp excluded nothing (104/104); not_like=%hopp%
		// excluded the row (103/104).
		return WrapLike(s), nil

	case modeNear:
		return nearOperand(path, raw)

	case modeGeo:
		return geoOperand(path, raw, opts)

	default: // modeScalar, modeNumeric, modeLikePass
		return TypeValue(raw)
	}
}

func listOperand(path, raw string) (any, error) {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, jsonPrefix) {
		v, err := decodeJSON(strings.TrimPrefix(trimmed, jsonPrefix))
		if err != nil {
			return nil, err
		}
		return v, nil
	}
	parts := SplitList(raw)
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		v, err := TypeValue(p)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, apierr.New(apierr.CodeInvalidWhereSyntax,
			"the list operator on %q needs at least one value", path).
			WithHint("comma-separate the values (escape a literal comma as \\,) or pass json:[…]")
	}
	return out, nil
}

func nearOperand(path, raw string) (any, error) {
	parts := strings.Split(strings.TrimSpace(raw), ",")
	if len(parts) < 3 || len(parts) > 4 {
		return nil, nearSyntaxError(path, raw)
	}
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if _, err := strconv.ParseFloat(p, 64); err != nil {
			return nil, nearSyntaxError(path, raw)
		}
		parts[i] = p
	}
	// Payload documents `near` as a comma-separated string and parses it with
	// String.split, so the string form is sent verbatim rather than an array.
	return strings.Join(parts, ","), nil
}

func nearSyntaxError(path, raw string) error {
	return apierr.New(apierr.CodeInvalidWhereSyntax,
		"the `near` operator on %q takes lng,lat,maxMeters[,minMeters]; got %q", path, raw).
		WithHint("example: --where 'location near -73.99,40.73,5000'")
}

func geoOperand(path, raw string, opts ParseOptions) (any, error) {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "@") {
		if opts.ReadFile == nil {
			return nil, apierr.New(apierr.CodeInvalidArgs,
				"@file GeoJSON is not available here; pass the GeoJSON literal for %q instead", path).
				WithHint("%s", "--where '"+path+" within {\"type\":\"Polygon\",\"coordinates\":[…]}'")
		}
		b, err := opts.ReadFile(strings.TrimPrefix(trimmed, "@"))
		if err != nil {
			return nil, apierr.Wrap(err, apierr.CodeFileMissing, "cannot read the GeoJSON file for %q", path)
		}
		trimmed = string(b)
	}
	trimmed = strings.TrimPrefix(trimmed, jsonPrefix)
	v, err := decodeJSON(trimmed)
	if err != nil {
		return nil, apierr.New(apierr.CodeInvalidWhereSyntax,
			"the geo operator on %q needs a GeoJSON object or @file.geojson", path).
			WithHint("example: --where 'area within {\"type\":\"Polygon\",\"coordinates\":[[[0,0],[0,1],[1,1],[0,0]]]}'")
	}
	return v, nil
}

// EscapeLike escapes the SQL LIKE metacharacters so a user's literal `%` or
// `_` is matched as itself. Verified live on the Postgres adapter.
func EscapeLike(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 4)
	for _, r := range s {
		switch r {
		case '\\', '%', '_':
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// WrapLike wraps a value in %…% unless the caller already did.
func WrapLike(s string) string {
	if strings.HasPrefix(s, "%") && strings.HasSuffix(s, "%") && len(s) > 1 {
		return s
	}
	return "%" + s + "%"
}

// SplitList splits a comma-separated list, honouring `\,` as a literal comma
// and `\\` as a literal backslash.
func SplitList(s string) []string {
	var (
		out  []string
		cur  strings.Builder
		esc  bool
		seen bool
	)
	for _, r := range s {
		switch {
		case esc:
			if r != ',' && r != '\\' {
				cur.WriteRune('\\') // keep an unknown escape verbatim
			}
			cur.WriteRune(r)
			esc = false
		case r == '\\':
			esc = true
		case r == ',':
			out = append(out, cur.String())
			cur.Reset()
			seen = true
		default:
			cur.WriteRune(r)
		}
	}
	if esc {
		cur.WriteRune('\\')
	}
	last := cur.String()
	if last != "" || seen || len(out) == 0 {
		out = append(out, last)
	}
	// Drop empty trailing segments produced by "a,b," but keep "" for a
	// genuinely empty input so the caller can report it.
	cleaned := out[:0]
	for _, v := range out {
		if strings.TrimSpace(v) == "" {
			continue
		}
		cleaned = append(cleaned, v)
	}
	return cleaned
}

func decodeJSON(s string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, apierr.Wrap(err, apierr.CodeInvalidWhereSyntax, "the json: value is not valid JSON")
	}
	if dec.More() {
		return nil, apierr.New(apierr.CodeInvalidWhereSyntax, "the json: value has trailing content after the JSON value")
	}
	return v, nil
}
