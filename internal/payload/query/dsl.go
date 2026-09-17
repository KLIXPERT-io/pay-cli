package query

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// jsonPrefix forces the raw-JSON escape hatch on any value (§9.4).
const jsonPrefix = "json:"

// MaxQFields caps the fan-out of --q (§9.4).
const MaxQFields = 12

// TypeValue applies §9.4 value typing to one raw token:
//
//	null            -> JSON null
//	true / false    -> bool
//	1, -2, 3.5, 1e6 -> number (kept as json.Number so it round-trips verbatim)
//	"123" / '123'   -> string (quotes force a string)
//	json:…          -> raw JSON
//	anything else   -> string
//
// This typing is the entire reason PayCLI emits JSON rather than bracket
// notation: the bracket form stringifies everything, and `equals=null` then
// silently matches nothing.
func TypeValue(raw string) (any, error) {
	s := strings.TrimSpace(raw)
	switch {
	case strings.HasPrefix(s, jsonPrefix):
		return decodeJSON(strings.TrimPrefix(s, jsonPrefix))
	case len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"':
		var out string
		if err := json.Unmarshal([]byte(s), &out); err == nil {
			return out, nil
		}
		return strings.Trim(s, `"`), nil
	case len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'':
		return s[1 : len(s)-1], nil
	case s == "null":
		return nil, nil
	case s == "true":
		return true, nil
	case s == "false":
		return false, nil
	}
	if isNumeric(s) {
		return json.Number(s), nil
	}
	return raw, nil
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	if _, err := strconv.ParseFloat(s, 64); err != nil {
		return false
	}
	// Reject the forms ParseFloat accepts but JSON does not, so a value that
	// types as a number here is always a legal JSON number.
	lower := strings.ToLower(s)
	if strings.ContainsAny(lower, "xp_") || strings.Contains(lower, "inf") || strings.Contains(lower, "nan") {
		return false
	}
	var n = json.Number(s)
	if _, err := n.Float64(); err != nil {
		return false
	}
	return true
}

// ParseTerm parses one `--where` / `--or` term: at most three
// whitespace-separated tokens — path, operator, rest-of-string-as-value — so a
// value containing spaces needs no quoting.
func ParseTerm(term string, opts ParseOptions) (Where, error) {
	path, rest, ok := cutField(strings.TrimSpace(term))
	if !ok {
		return nil, termSyntaxError(term, "it needs a path, an operator and (for most operators) a value")
	}
	op, value, hasValue := cutField(rest)
	if !hasValue {
		op = strings.TrimSpace(rest)
		value = ""
	}
	if op == "" {
		return nil, termSyntaxError(term, "it names a path but no operator")
	}
	if err := CheckPathSyntax(path); err != nil {
		return nil, err
	}
	return BuildTerm(path, op, value, opts)
}

// cutField splits off the first whitespace-delimited token.
func cutField(s string) (head, rest string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false
	}
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, "", false
	}
	return s[:i], strings.TrimLeft(s[i:], " \t"), true
}

func termSyntaxError(term, why string) error {
	return apierr.New(apierr.CodeInvalidWhereSyntax, "cannot parse --where %q: %s", term, why).
		WithHint(`the grammar is 'PATH OP VALUE' — e.g. 'status eq lead', 'company.name contains acme', ` +
			`'jobTitle exists false', 'tags in 1,2,3'. Values with spaces need no quoting.`)
}

// CheckPathSyntax rejects a path that cannot be a field path before it reaches
// the server, where an unknown path is a 400 QueryError at best and silently
// ignored at worst.
func CheckPathSyntax(path string) error {
	if path == "" {
		return apierr.New(apierr.CodeInvalidWhereSyntax, "the where term has an empty path")
	}
	for _, r := range path {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.':
		default:
			return apierr.New(apierr.CodeInvalidWhereSyntax,
				"%q is not a valid field path (%q is not allowed in a path)", path, string(r)).
				WithHint("paths are dotted field names such as `company.name` or `_status`")
		}
	}
	if strings.HasPrefix(path, ".") || strings.HasSuffix(path, ".") || strings.Contains(path, "..") {
		return apierr.New(apierr.CodeInvalidWhereSyntax, "%q is not a valid field path", path).
			WithHint("paths are dotted field names such as `company.name` or `_status`")
	}
	return nil
}

// Input is everything the filter flags contribute to one where clause.
type Input struct {
	Where []string // --where, ANDed
	Or    []string // --or, one OR group ANDed with the rest

	// WhereJSON is --where-json, which replaces everything else (§9.4).
	WhereJSON string

	// IDs is --id/--ids sugar, compiled to {"id":{"in":[…]}}.
	IDs []string
	// IDType is the manifest's id_type: "number", "string" or "unknown".
	// Unknown means §9.4 typing decides, never a fabricated rejection.
	IDType string

	DraftOnly     bool
	PublishedOnly bool

	// Q is --q: an OR of `contains` across QFields.
	Q       string
	QFields []string

	// Extra terms already compiled by the caller (e.g. --since/--until).
	Extra []Where

	Options ParseOptions
}

// Build compiles the whole filter into one Where clause.
func Build(in Input) (Where, error) {
	if strings.TrimSpace(in.WhereJSON) != "" {
		w, err := ParseWhereJSON(in.WhereJSON)
		if err != nil {
			return nil, err
		}
		return w, nil
	}

	var and []Where
	for _, t := range in.Where {
		if strings.TrimSpace(t) == "" {
			continue
		}
		w, err := ParseTerm(t, in.Options)
		if err != nil {
			return nil, err
		}
		and = append(and, w)
	}

	var or []Where
	for _, t := range in.Or {
		if strings.TrimSpace(t) == "" {
			continue
		}
		w, err := ParseTerm(t, in.Options)
		if err != nil {
			return nil, err
		}
		or = append(or, w)
	}

	if len(in.IDs) > 0 {
		w, err := IDTerm(in.IDs, in.IDType)
		if err != nil {
			return nil, err
		}
		and = append(and, w)
	}
	switch {
	case in.DraftOnly && in.PublishedOnly:
		return nil, apierr.New(apierr.CodeInvalidArgs, "--draft-only and --published-only are mutually exclusive")
	case in.DraftOnly:
		and = append(and, Term("_status", OpEquals, "draft"))
	case in.PublishedOnly:
		and = append(and, Term("_status", OpEquals, "published"))
	}
	if strings.TrimSpace(in.Q) != "" {
		w, err := QTerm(in.Q, in.QFields)
		if err != nil {
			return nil, err
		}
		and = append(and, w)
	}
	and = append(and, in.Extra...)

	return Combine(and, or), nil
}

// ParseWhereJSON decodes --where-json, rejecting anything that is not a JSON
// object so `--where-json '[…]'` fails here rather than as an opaque server 400.
func ParseWhereJSON(s string) (Where, error) {
	v, err := decodeJSON(strings.TrimSpace(s))
	if err != nil {
		return nil, apierr.New(apierr.CodeInvalidWhereSyntax, "--where-json is not valid JSON").
			WithHint(`example: --where-json '{"and":[{"status":{"equals":"lead"}},{"jobTitle":{"equals":null}}]}'`)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, apierr.New(apierr.CodeInvalidWhereSyntax, "--where-json must be a JSON object, not %s", jsonKind(v)).
			WithHint(`example: --where-json '{"status":{"equals":"lead"}}'`)
	}
	return Where(m), nil
}

func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case json.Number:
		return "a number"
	case string:
		return "a string"
	case []any:
		return "an array"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// IDTerm compiles --id/--ids into {"id":{"in":[…]}}, typing each id against
// the collection's id_type when it is known.
func IDTerm(ids []string, idType string) (Where, error) {
	vals := make([]any, 0, len(ids))
	for _, raw := range ids {
		for _, id := range SplitList(raw) {
			v, err := TypedID(id, idType)
			if err != nil {
				return nil, err
			}
			vals = append(vals, v)
		}
	}
	if len(vals) == 0 {
		return nil, apierr.New(apierr.CodeInvalidArgs, "--id/--ids was given no values")
	}
	return Term("id", OpIn, vals), nil
}

// TypedID coerces one id to the collection's id_type. An unknown id_type is
// the tri-state "send it and learn" case: §9.4 typing applies and nothing is
// rejected locally.
func TypedID(id, idType string) (any, error) {
	s := strings.TrimSpace(id)
	switch idType {
	case "number":
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, apierr.New(apierr.CodeInvalidID,
				"%q is not a valid id: this collection's ids are numbers", id).
				WithHint("Payload answers a non-castable id with an opaque HTTP 500, so PayCLI rejects it here")
		}
		return json.Number(strconv.FormatInt(n, 10)), nil
	case "string":
		return s, nil
	default:
		return TypeValue(s)
	}
}

// QTerm expands --q into an OR of `contains` across up to MaxQFields fields.
func QTerm(text string, fields []string) (Where, error) {
	fields = dedupe(fields)
	if len(fields) == 0 {
		return nil, apierr.New(apierr.CodeInvalidArgs,
			"--q needs at least one text field to search, and none is known for this collection").
			WithHint("use --where 'field contains TEXT' with a field from `pay describe <collection>`")
	}
	if len(fields) > MaxQFields {
		fields = fields[:MaxQFields]
	}
	terms := make([]Where, 0, len(fields))
	for _, f := range fields {
		terms = append(terms, Term(f, OpContains, EscapeLike(text)))
	}
	return Or(terms...), nil
}

// SinceForms documents the accepted --since/--until spellings; it is quoted
// verbatim in the error message so an agent never has to guess.
const SinceForms = "N[h|d|w|mo] (12h 30d 4w 1mo) · a Go duration (720h 90m 36h30m) · YYYY-MM-DD · full RFC 3339"

// ParseSince resolves a --since/--until expression to an absolute instant.
// `now` is passed in (never read from the clock here) so the resolution is
// deterministic under test, per §3.1 and §9.4.
func ParseSince(expr string, now time.Time) (time.Time, error) {
	s := strings.TrimSpace(expr)
	if s == "" {
		return time.Time{}, apierr.New(apierr.CodeInvalidArgs, "empty date expression").
			WithHint("forms: " + SinceForms)
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	if n, unit, ok := cutRelative(s); ok {
		switch unit {
		case "h":
			return now.Add(-time.Duration(n) * time.Hour), nil
		case "d":
			return now.AddDate(0, 0, -n), nil
		case "w":
			return now.AddDate(0, 0, -7*n), nil
		case "mo":
			// Calendar-aware, clamping the day: 2026-03-31 minus 1mo is
			// 2026-02-28, not 2026-03-03 as AddDate alone would give.
			return addMonthsClamped(now, -n), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	return time.Time{}, apierr.New(apierr.CodeInvalidArgs, "cannot parse the date expression %q", expr).
		WithHint("forms: " + SinceForms)
}

func cutRelative(s string) (n int, unit string, ok bool) {
	for _, u := range []string{"mo", "h", "d", "w"} {
		if !strings.HasSuffix(s, u) {
			continue
		}
		head := strings.TrimSuffix(s, u)
		if head == "" {
			continue
		}
		v, err := strconv.Atoi(head)
		if err != nil || v < 0 {
			continue
		}
		return v, u, true
	}
	return 0, "", false
}

func addMonthsClamped(t time.Time, months int) time.Time {
	y, m, d := t.Date()
	first := time.Date(y, m, 1, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
	shifted := first.AddDate(0, months, 0)
	last := daysInMonth(shifted.Year(), shifted.Month())
	if d > last {
		d = last
	}
	return time.Date(shifted.Year(), shifted.Month(), d, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
}

func daysInMonth(y int, m time.Month) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// DateTerm compiles a resolved --since/--until bound against a date field.
func DateTerm(field string, since, until *time.Time) Where {
	var terms []Where
	if since != nil {
		terms = append(terms, Term(field, OpGreaterThanEqual, since.UTC().Format(time.RFC3339)))
	}
	if until != nil {
		terms = append(terms, Term(field, OpLessThanEqual, until.UTC().Format(time.RFC3339)))
	}
	return And(terms...)
}
