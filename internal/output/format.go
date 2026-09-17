package output

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// Format is the value of --output (§10.3).
type Format string

const (
	// FormatJSON is the DEFAULT: one pretty envelope object, for agents.
	FormatJSON Format = "json"
	// FormatJSONL streams bare documents on stdout, envelope on stderr.
	FormatJSONL Format = "jsonl"
	// FormatID prints one id per line and nothing else.
	FormatID Format = "id"
	// FormatRaw prints Payload's body with no envelope — after redaction,
	// unless --no-redact.
	FormatRaw Format = "raw"
	// FormatCSV is RFC 4180, for humans.
	FormatCSV Format = "csv"
	// FormatTable is aligned and width-truncated, for humans. Never
	// recommended to agents.
	FormatTable Format = "table"
)

// Formats is the closed set, in help order.
var Formats = []Format{FormatJSON, FormatJSONL, FormatID, FormatRaw, FormatCSV, FormatTable}

// ParseFormat validates --output. An unknown value is invalid_option (exit 5)
// with the valid list attached, never a silent fallback to json.
func ParseFormat(s string) (Format, error) {
	f := Format(strings.ToLower(strings.TrimSpace(s)))
	if f == "" {
		return FormatJSON, nil
	}
	for _, known := range Formats {
		if f == known {
			return f, nil
		}
	}
	names := formatNames(Formats)
	return "", apierr.New(apierr.CodeInvalidOption,
		"%q is not a valid --output format. Valid values: %s.", s, strings.Join(names, ", ")).
		WithDidYouMean(apierr.DidYouMean(s, names)...).
		WithHint("The default is json, which is the format agents should use. `pay explain --section output` describes each one.")
}

// supportedKinds is the explicit format/command compatibility matrix (§10.3).
// It is explicit on purpose: `--output csv` on a command whose data is a schema
// fails loudly rather than silently falling back to JSON.
var supportedKinds = map[Format][]DataKind{
	FormatJSON:  DataKinds,
	FormatRaw:   DataKinds,
	FormatJSONL: {KindDoc, KindDocList, KindGlobal, KindVersion, KindVersionList, KindBulkResult},
	FormatID:    {KindDoc, KindDocList, KindVersion, KindVersionList, KindBulkResult, KindOpResult},
	FormatCSV:   {KindDoc, KindDocList, KindVersion, KindVersionList},
	FormatTable: {KindDoc, KindDocList, KindVersion, KindVersionList, KindCount, KindCapabilities, KindOpResult},
}

// Supports reports whether the format can render this data_kind.
func (f Format) Supports(kind DataKind) bool {
	for _, k := range supportedKinds[f] {
		if k == kind {
			return true
		}
	}
	return false
}

// CheckFormat returns format_unsupported (exit 5) naming the formats that DO work,
// so the caller's next attempt succeeds.
func CheckFormat(f Format, kind DataKind) error {
	if f.Supports(kind) {
		return nil
	}
	var ok []Format
	for _, candidate := range Formats {
		if candidate.Supports(kind) {
			ok = append(ok, candidate)
		}
	}
	return apierr.New(apierr.CodeFormatUnsupported,
		"--output %s cannot render %s output.", f, kind).
		WithHint("Supported formats for %s: %s.", kind, strings.Join(formatNames(ok), ", "))
}

func formatNames(fs []Format) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, string(f))
	}
	return out
}

// --- the --path evaluator (§10.3) ---------------------------------------

// pathForms is the whole grammar. It is quoted verbatim in every
// invalid_path_expr message, because the failure mode being designed against is
// an agent sending a jq program.
const pathForms = `--path supports exactly three forms: ".a.b" (field access, arbitrarily nested), ".a[0]" (index into an array; negative indices are an error) and ".a[]" (iterate a whole array). Nothing else - no filters, no pipes, no functions, no select, no map.`

// pathNotJQ is the literal closing sentence §10.3 requires.
const pathNotJQ = "This is not jq; pipe the envelope to jq for full expressions."

type stepKind int

const (
	stepField stepKind = iota
	stepIndex
	stepIterate
)

type pathStep struct {
	kind  stepKind
	key   string
	index int
}

// ParsePath compiles a --path expression. The returned steps are applied by
// EvalPath; an empty or "." expression is the identity.
func ParsePath(expr string) ([]pathStep, error) {
	e := strings.TrimSpace(expr)
	if e == "" || e == "." {
		return nil, nil
	}
	// A step may also start with '[' because .data IS the array for every
	// doc_list (§10.1): `pay find pages --path '[0].id'` and the jq spelling
	// `.[].id` address the documents themselves. Without this the three forms
	// could not reach a single document of the CLI's most-used command.
	if !strings.HasPrefix(e, ".") && !strings.HasPrefix(e, "[") {
		return nil, pathErr(expr, "an expression must start with '.' or '['")
	}
	var steps []pathStep
	i := 0
	for i < len(e) {
		switch e[i] {
		case '.':
			i++
			if i < len(e) && e[i] == '[' {
				// ".[0]" / ".[]": the dot is a separator before an index, not
				// the start of a field name.
				continue
			}
			start := i
			for i < len(e) && e[i] != '.' && e[i] != '[' {
				i++
			}
			key := e[start:i]
			if key == "" {
				return nil, pathErr(expr, "empty field name after '.'")
			}
			if bad := badFieldChar(key); bad != "" {
				return nil, pathErr(expr, fmt.Sprintf("%q is not a field name (it contains %q)", key, bad))
			}
			steps = append(steps, pathStep{kind: stepField, key: key})
		case '[':
			close := strings.IndexByte(e[i:], ']')
			if close < 0 {
				return nil, pathErr(expr, "unclosed '['")
			}
			inner := e[i+1 : i+close]
			i += close + 1
			if inner == "" {
				steps = append(steps, pathStep{kind: stepIterate})
				continue
			}
			n, err := strconv.Atoi(inner)
			if err != nil {
				return nil, pathErr(expr, fmt.Sprintf("%q is not an array index; only a non-negative integer or an empty [] is allowed", inner))
			}
			if n < 0 {
				return nil, pathErr(expr, "negative indices are an error")
			}
			steps = append(steps, pathStep{kind: stepIndex, index: n})
		default:
			return nil, pathErr(expr, fmt.Sprintf("unexpected %q; a step must begin with '.' or '['", string(e[i])))
		}
	}
	return steps, nil
}

// badFieldChar returns the first character of key that cannot appear in a
// Payload field name. Without this check ".docs | map(.id)" parses as two
// field accesses instead of failing, and the agent gets an unspecified result
// from a jq program rather than the invalid_path_expr §10.3 promises.
func badFieldChar(key string) string {
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return string(r)
		}
	}
	return ""
}

func pathErr(expr, why string) error {
	return apierr.New(apierr.CodeInvalidPathExpr,
		"Invalid --path expression %q: %s. %s %s", expr, why, pathForms, pathNotJQ).
		WithHint("%s %s", pathForms, pathNotJQ)
}

// EvalPath applies a compiled expression to a value. The result REPLACES
// envelope.data; ok, v, command, data_kind, target, page, next, meta and
// warnings are preserved unchanged, so an agent can always still branch on .ok.
func EvalPath(data any, steps []pathStep) (any, error) {
	if len(steps) == 0 {
		return data, nil
	}
	values := []any{data}
	multi := false
	for _, step := range steps {
		next := make([]any, 0, len(values))
		for _, v := range values {
			switch step.kind {
			case stepField:
				got, err := fieldOf(v, step.key)
				if err != nil {
					return nil, err
				}
				next = append(next, got)
			case stepIndex:
				arr, err := arrayOf(v, step.index, false)
				if err != nil {
					return nil, err
				}
				if step.index < len(arr) {
					next = append(next, arr[step.index])
				} else {
					next = append(next, nil)
				}
			case stepIterate:
				arr, err := arrayOf(v, 0, true)
				if err != nil {
					return nil, err
				}
				next = append(next, arr...)
				multi = true
			}
		}
		values = next
	}
	if multi {
		return values, nil
	}
	if len(values) == 0 {
		return nil, nil
	}
	return values[0], nil
}

func fieldOf(v any, key string) (any, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		return t[key], nil
	default:
		return nil, pathTypeErr(fmt.Sprintf("cannot read field %q from %s; field access needs an object", key, typeName(v)))
	}
}

func arrayOf(v any, index int, iterate bool) ([]any, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case []any:
		return t, nil
	default:
		what := fmt.Sprintf("index [%d]", index)
		if iterate {
			what = "iterate []"
		}
		return nil, pathTypeErr(fmt.Sprintf("cannot %s into %s; that form needs an array", what, typeName(v)))
	}
}

func pathTypeErr(why string) error {
	return apierr.New(apierr.CodeInvalidPathExpr, "--path: %s. %s %s", why, pathForms, pathNotJQ).
		WithHint("%s %s", pathForms, pathNotJQ)
}

// typeName names the JSON shape of v for an invalid_path_expr message. It must
// never guess: the old `default: return "a number"` told an agent that a
// []string was a number, which sends it debugging the wrong thing. Anything
// generic() could not normalise (its marshal failed) is described by its Go
// kind instead, so the message is still true.
func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case json.Number, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, float32, float64:
		return "a number"
	}
	return kindName(reflect.ValueOf(v))
}

func kindName(rv reflect.Value) string {
	switch rv.Kind() {
	case reflect.Invalid:
		return "null"
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return "null"
		}
		return kindName(rv.Elem())
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return "null"
		}
		return "an array"
	case reflect.Map:
		if rv.IsNil() {
			return "null"
		}
		return "an object"
	case reflect.Struct:
		return "an object"
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "a boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Uintptr, reflect.Float32, reflect.Float64:
		return "a number"
	default:
		return "a " + rv.Kind().String()
	}
}

// ApplyPath compiles and applies expr, replacing envelope.data in place.
func (e *Envelope) ApplyPath(expr string) error {
	steps, err := ParsePath(expr)
	if err != nil {
		return err
	}
	if len(steps) == 0 {
		return nil
	}
	got, err := EvalPath(generic(e.Data), steps)
	if err != nil {
		return err
	}
	e.Data = got
	e.pathNarrowed = true
	// The recorded wire body is no longer what .data says, so it is dropped:
	// `--path .title --output raw` must print the selected value (§10.3's "use
	// --output raw for a bare scalar with no envelope"), not the whole
	// document the path was evaluated against. renderRaw falls back to the
	// data value when there is no body.
	e.RawBody = nil
	e.RawRedacted = false
	return nil
}

// NarrowedToScalar reports that --path reduced the envelope to a plain value
// (or a list of plain values) with no structure left.
//
// It exists so `--output id` can print a path-selected scalar. The §10.3 matrix
// is keyed on data_kind, and data_kind describes what the COMMAND produced, so
// `pay doctor --path .ok --output id` — an example the help registry itself
// prints — failed with format_unsupported "--output id cannot render
// capabilities output", even though by then .data is just `true`. Nothing about
// a bare scalar is capabilities-shaped.
func (e *Envelope) NarrowedToScalar() bool {
	if !e.pathNarrowed {
		return false
	}
	switch v := generic(e.Data).(type) {
	case nil:
		return true
	case map[string]any:
		return false
	case []any:
		for _, item := range v {
			switch item.(type) {
			case map[string]any, []any:
				return false
			}
		}
		return true
	default:
		return true
	}
}

// SortedKeys is a small helper shared by the csv and table renderers.
func SortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
