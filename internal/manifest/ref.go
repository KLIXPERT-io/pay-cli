package manifest

import (
	"fmt"
	"sort"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/docdiff"
)

// The reference grammar. A reference is any JSON object with a "$ref" key,
// anywhere in a write body:
//
//	{"$ref": "pages", "slug": "about"}                   → 12
//	{"$ref": "pages", "slug": "about", "$as": "polymorphic"}
//	                                                     → {"relationTo":"pages","value":12}
//	{"$ref": "posts", "where": {"title": {"like": "x"}}} → a Payload where object
//	{"$ref": "@home"}                                    → the id of plan item "home"
//
// Every key other than $ref, $as and where is an equals-match on that field
// (dotted keys reach group fields: "meta.title"). The value must be a scalar.
const (
	// RefKey marks an object as a reference.
	RefKey = "$ref"
	// AsKey selects the replacement's shape.
	AsKey = "$as"
	// WhereKey carries a Payload where object.
	WhereKey = "where"
	// AsID (the default) replaces the reference with the bare id.
	AsID = "id"
	// AsPolymorphic replaces it with {relationTo, value}, the shape of a
	// relationship whose relationTo lists several collections.
	AsPolymorphic = "polymorphic"
	// LocalPrefix starts a plan-local reference: "@key".
	LocalPrefix = "@"
)

// Term is one equals-match of a reference.
type Term struct {
	Field string
	Value any
}

// Ref is one parsed reference.
type Ref struct {
	// Path is where in the body the reference sits (docdiff notation).
	Path string
	// Collection is the target collection; empty for a local reference.
	Collection string
	// Local is the plan item key of a "@key" reference (without the @).
	Local string
	// As is AsID or AsPolymorphic.
	As string
	// Equals are the equals-matches, sorted by field.
	Equals []Term
	// Where is the reference's own Payload where object, if any.
	Where map[string]any
}

// IsLocal reports a plan-local "@key" reference.
func (r *Ref) IsLocal() bool { return r.Local != "" }

// String renders the reference compactly for messages:
// `pages{slug=about}`, `@home`, `posts{where}`.
func (r *Ref) String() string {
	if r.IsLocal() {
		return LocalPrefix + r.Local
	}
	parts := make([]string, 0, len(r.Equals)+1)
	for _, t := range r.Equals {
		parts = append(parts, t.Field+"="+ScalarString(t.Value))
	}
	if r.Where != nil {
		parts = append(parts, "where")
	}
	return r.Collection + "{" + strings.Join(parts, ",") + "}"
}

// Value is what the reference is replaced with, once collection and id are
// known.
func (r *Ref) Value(collection string, id any) any {
	if r.As == AsPolymorphic {
		return map[string]any{"relationTo": collection, "value": id}
	}
	return id
}

// IsRef reports whether v is a reference object.
func IsRef(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	_, ok = m[RefKey]
	return ok
}

// Parse validates one reference object found at path.
func Parse(m map[string]any, path string) (*Ref, error) {
	bad := func(format string, args ...any) error {
		at := path
		if at == "" {
			at = "(root)"
		}
		return apierr.New(apierr.CodeBadRequestBody, "$ref at %s: %s", at, fmt.Sprintf(format, args...)).
			WithHint(`a reference is {"$ref":"<collection>","<field>":<value>…} (optionally "where":{…} and "$as":"polymorphic"), or {"$ref":"@<plan item key>"} inside a plan`)
	}
	target, ok := m[RefKey].(string)
	target = strings.TrimSpace(target)
	if !ok || target == "" {
		return nil, bad("\"$ref\" must name a collection (or \"@key\" of a plan item)")
	}
	r := &Ref{Path: path, As: AsID}
	if as, ok := m[AsKey]; ok {
		s, _ := as.(string)
		switch s {
		case AsID, AsPolymorphic:
			r.As = s
		default:
			return nil, bad("\"$as\" must be %q or %q, not %v", AsID, AsPolymorphic, as)
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.HasPrefix(target, LocalPrefix) {
		r.Local = strings.TrimSpace(strings.TrimPrefix(target, LocalPrefix))
		if r.Local == "" {
			return nil, bad("\"@\" must be followed by a plan item key")
		}
		for _, k := range keys {
			if k != RefKey && k != AsKey {
				return nil, bad("a plan-local reference takes only \"$as\" besides \"$ref\"; %q is not allowed", k)
			}
		}
		return r, nil
	}
	r.Collection = target
	for _, k := range keys {
		switch k {
		case RefKey, AsKey:
			continue
		case WhereKey:
			w, ok := m[k].(map[string]any)
			if !ok || len(w) == 0 {
				return nil, bad("\"where\" must be a non-empty Payload where object")
			}
			r.Where = w
			continue
		}
		if strings.HasPrefix(k, "$") {
			return nil, bad("unknown key %q (only $ref and $as start with $)", k)
		}
		v := m[k]
		if !IsScalar(v) {
			return nil, bad("%q must be a string, number or boolean to match on, not %s", k, kindOf(v))
		}
		r.Equals = append(r.Equals, Term{Field: k, Value: v})
	}
	if len(r.Equals) == 0 && r.Where == nil {
		return nil, bad("names no document: add a field to match (e.g. \"slug\":\"about\") or a \"where\"")
	}
	return r, nil
}

// Collect returns every reference in v, in document order (object keys
// sorted). A reference inside a reference is not looked for: its values are
// scalars by construction.
func Collect(v any) ([]*Ref, error) {
	var out []*Ref
	var walk func(path string, v any) error
	walk = func(path string, v any) error {
		switch t := v.(type) {
		case map[string]any:
			if _, ok := t[RefKey]; ok {
				r, err := Parse(t, path)
				if err != nil {
					return err
				}
				out = append(out, r)
				return nil
			}
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if err := walk(docdiff.JoinKey(path, k), t[k]); err != nil {
					return err
				}
			}
		case []any:
			for i, e := range t {
				if err := walk(docdiff.JoinIndex(path, i), e); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk("", v); err != nil {
		return nil, err
	}
	return out, nil
}

// HasRefs is a cheap test: does v contain any reference at all?
func HasRefs(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		if _, ok := t[RefKey]; ok {
			return true
		}
		for _, e := range t {
			if HasRefs(e) {
				return true
			}
		}
	case []any:
		for _, e := range t {
			if HasRefs(e) {
				return true
			}
		}
	}
	return false
}

// Replace returns a deep copy of body with every reference replaced by
// resolve's answer. body is never mutated. The first error aborts.
func Replace(body map[string]any, resolve func(*Ref) (any, error)) (map[string]any, error) {
	var walk func(path string, v any) (any, error)
	walk = func(path string, v any) (any, error) {
		switch t := v.(type) {
		case map[string]any:
			if _, ok := t[RefKey]; ok {
				r, err := Parse(t, path)
				if err != nil {
					return nil, err
				}
				return resolve(r)
			}
			out := make(map[string]any, len(t))
			for k, e := range t {
				nv, err := walk(docdiff.JoinKey(path, k), e)
				if err != nil {
					return nil, err
				}
				out[k] = nv
			}
			return out, nil
		case []any:
			out := make([]any, len(t))
			for i, e := range t {
				nv, err := walk(docdiff.JoinIndex(path, i), e)
				if err != nil {
					return nil, err
				}
				out[i] = nv
			}
			return out, nil
		}
		return v, nil
	}
	out, err := walk("", body)
	if err != nil {
		return nil, err
	}
	m, _ := out.(map[string]any)
	return m, nil
}
