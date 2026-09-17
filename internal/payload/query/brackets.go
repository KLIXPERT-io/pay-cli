package query

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// Pair is one already-escaped query-string parameter.
type Pair struct {
	Key   string
	Value string
}

// String renders "key=value".
func (p Pair) String() string { return p.Key + "=" + p.Value }

// Brackets renders a nested value into qs bracket notation under prefix:
//
//	Brackets("select", map[string]any{"hero": map[string]any{"media": true}})
//	  -> select[hero][media]=true
//
// Map keys are emitted in sorted order so the output is deterministic, and
// slices become [0], [1], … in index order.
//
// A JSON null anywhere in the tree is a hard error: bracket notation has no
// syntax for it and the server reads the four characters "null" as the string
// "null", which silently matches nothing (verified: where[jobTitle][equals]=null
// returned 0 documents where the JSON form returned 23).
func Brackets(prefix string, v any) ([]Pair, error) {
	var out []Pair
	if err := brackets(prefix, v, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func brackets(prefix string, v any, out *[]Pair) error {
	switch t := v.(type) {
	case nil:
		return nullNotExpressible(prefix)
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := brackets(prefix+"["+escape(k)+"]", t[k], out); err != nil {
				return err
			}
		}
		return nil
	case Where:
		return brackets(prefix, map[string]any(t), out)
	case []any:
		for i, item := range t {
			if err := brackets(prefix+"["+strconv.Itoa(i)+"]", item, out); err != nil {
				return err
			}
		}
		return nil
	case []string:
		for i, item := range t {
			if err := brackets(prefix+"["+strconv.Itoa(i)+"]", item, out); err != nil {
				return err
			}
		}
		return nil
	case string:
		*out = append(*out, Pair{Key: prefix, Value: escape(t)})
		return nil
	case bool:
		*out = append(*out, Pair{Key: prefix, Value: strconv.FormatBool(t)})
		return nil
	case json.Number:
		*out = append(*out, Pair{Key: prefix, Value: escape(t.String())})
		return nil
	case int:
		*out = append(*out, Pair{Key: prefix, Value: strconv.Itoa(t)})
		return nil
	case int64:
		*out = append(*out, Pair{Key: prefix, Value: strconv.FormatInt(t, 10)})
		return nil
	case float64:
		*out = append(*out, Pair{Key: prefix, Value: escape(strconv.FormatFloat(t, 'f', -1, 64))})
		return nil
	default:
		// Anything else is normalised through JSON so a caller-supplied struct
		// or a map[string]string behaves like the shapes above.
		b, err := marshalNoHTML(v)
		if err != nil {
			return apierr.Wrap(err, apierr.CodeInvalidWhereSyntax,
				"%s cannot be expressed in bracket notation", unescapePrefix(prefix))
		}
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.UseNumber()
		var generic any
		if err := dec.Decode(&generic); err != nil {
			return apierr.Wrap(err, apierr.CodeInvalidWhereSyntax,
				"%s cannot be expressed in bracket notation", unescapePrefix(prefix))
		}
		if _, again := generic.(map[string]any); !again {
			if _, arr := generic.([]any); !arr {
				// A scalar that round-tripped to itself would loop forever.
				if generic == nil {
					return nullNotExpressible(prefix)
				}
			}
		}
		return brackets(prefix, generic, out)
	}
}

func nullNotExpressible(prefix string) error {
	return apierr.New(apierr.CodeInvalidWhereSyntax,
		"%s is null, and qs bracket notation cannot express null", unescapePrefix(prefix)).
		WithHint("drop --where-style qs (the default JSON encoding sends a real null), " +
			"or use the `exists` operator: --where 'field exists false'")
}

// unescapePrefix makes an escaped bracket path readable again in an error
// message. It is cosmetic only and never feeds a request.
func unescapePrefix(prefix string) string {
	s := strings.ReplaceAll(prefix, "%20", " ")
	return strings.ReplaceAll(s, "+", " ")
}

// WhereBrackets renders a where tree in bracket notation for --where-style qs.
// It is a debugging aid; StyleJSON is what PayCLI sends by default.
func WhereBrackets(w Where) ([]Pair, error) {
	if w.IsEmpty() {
		return nil, nil
	}
	return Brackets("where", map[string]any(w))
}

// EncodePairs joins pairs with '&'.
func EncodePairs(pairs []Pair) string {
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p.String()
	}
	return strings.Join(parts, "&")
}
