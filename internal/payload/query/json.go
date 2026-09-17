// Package query builds Payload REST query strings.
//
// The split is load-bearing and verified live (§2.3, §9.5): `where` and `data`
// are URL-encoded JSON strings, while `select`, `populate`, `joins`, `sort`,
// `depth`, `limit`, `page`, `draft` and `trash` use qs bracket notation.
// Sending `select` as a JSON string is silently dropped by the server, and
// bracket notation cannot express JSON null — `where[jobTitle][equals]=null`
// matched 0 documents where the JSON form matched 23.
package query

import (
	"bytes"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// Where is a Payload where-clause tree: {"and":[…]}, {"or":[…]} or
// {"field":{"operator":value}}.
type Where map[string]any

// IsEmpty reports whether the clause would contribute nothing to a request.
func (w Where) IsEmpty() bool { return len(w) == 0 }

// JSON marshals the clause. Go's encoder sorts map keys, so the output is
// deterministic and therefore golden-testable.
func (w Where) JSON() (string, error) {
	if w.IsEmpty() {
		return "", nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // a `&` or `<` in a value must survive verbatim
	if err := enc.Encode(map[string]any(w)); err != nil {
		return "", apierr.Wrap(err, apierr.CodeInvalidWhereSyntax, "the where clause cannot be encoded as JSON")
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

// Encode returns the URL-encoded JSON form used as the `where=` value.
func (w Where) Encode() (string, error) {
	s, err := w.JSON()
	if err != nil || s == "" {
		return "", err
	}
	return url.QueryEscape(s), nil
}

// Term builds {path: {op: value}}.
func Term(path, op string, value any) Where {
	return Where{path: map[string]any{op: value}}
}

// And ANDs the non-empty terms. A single term is returned unwrapped so the
// simplest query stays the simplest string.
func And(terms ...Where) Where { return group("and", terms) }

// Or ORs the non-empty terms.
func Or(terms ...Where) Where { return group("or", terms) }

func group(key string, terms []Where) Where {
	kept := make([]any, 0, len(terms))
	for _, t := range terms {
		if !t.IsEmpty() {
			kept = append(kept, map[string]any(t))
		}
	}
	switch len(kept) {
	case 0:
		return nil
	case 1:
		one, ok := kept[0].(map[string]any)
		if !ok {
			return Where{key: kept}
		}
		return Where(one)
	default:
		return Where{key: kept}
	}
}

// Combine implements §9.4's composition rule: --where terms are ANDed, and the
// repeatable --or terms form exactly one OR group which is ANDed with them.
func Combine(and []Where, or []Where) Where {
	terms := make([]Where, 0, len(and)+1)
	terms = append(terms, and...)
	if g := Or(or...); !g.IsEmpty() {
		terms = append(terms, g)
	}
	return And(terms...)
}

// WhereStyle selects the encoding of the `where` parameter.
type WhereStyle string

const (
	// StyleJSON is the default and the only style that can express null.
	StyleJSON WhereStyle = "json"
	// StyleQS emits bracket notation for debugging and URL-pasting
	// (--where-style qs). It fails loudly on a null comparison rather than
	// silently returning the wrong documents.
	StyleQS WhereStyle = "qs"
)

// ParseWhereStyle validates a --where-style value.
func ParseWhereStyle(s string) (WhereStyle, error) {
	switch s {
	case "", string(StyleJSON):
		return StyleJSON, nil
	case string(StyleQS):
		return StyleQS, nil
	default:
		return "", apierr.New(apierr.CodeInvalidOption, "unknown --where-style %q", s).
			WithDidYouMean(apierr.DidYouMean(s, []string{string(StyleJSON), string(StyleQS)})...).
			WithHint("valid styles: json, qs")
	}
}

// Params is every query parameter PayCLI knows how to send. Zero values are
// omitted, so one type serves find, get, count, create, update and upload.
type Params struct {
	Where      Where
	WhereStyle WhereStyle
	// WhereRaw is --where-raw: a literal query string appended verbatim.
	WhereRaw string
	// Data is the rare `data=` query parameter (URL-encoded JSON, like where).
	Data any

	Sort  []string
	Limit *int
	Page  int
	Depth *int

	Select        []string
	SelectExclude []string
	// Populate is populate[collection][field]=true.
	Populate map[string][]string
	// Joins is joins[field][limit]=5.
	Joins map[string]map[string]string

	Draft *bool
	Trash *bool

	Locale         string
	FallbackLocale string

	// Extra carries anything a caller needs that this struct does not model
	// (pay raw --query k=v). Keys are emitted in sorted order.
	Extra url.Values
}

// Clone returns a shallow-enough copy that callers can adjust one field
// without mutating a shared Params.
func (p Params) Clone() Params {
	c := p
	c.Sort = append([]string(nil), p.Sort...)
	c.Select = append([]string(nil), p.Select...)
	c.SelectExclude = append([]string(nil), p.SelectExclude...)
	if p.Where != nil {
		c.Where = Where{}
		for k, v := range p.Where {
			c.Where[k] = v
		}
	}
	if p.Populate != nil {
		c.Populate = map[string][]string{}
		for k, v := range p.Populate {
			c.Populate[k] = append([]string(nil), v...)
		}
	}
	if p.Joins != nil {
		c.Joins = map[string]map[string]string{}
		for k, v := range p.Joins {
			m := map[string]string{}
			for kk, vv := range v {
				m[kk] = vv
			}
			c.Joins[k] = m
		}
	}
	if p.Extra != nil {
		c.Extra = url.Values{}
		for k, v := range p.Extra {
			c.Extra[k] = append([]string(nil), v...)
		}
	}
	return c
}

func intPtr(n int) *int    { return &n }
func boolPtr(b bool) *bool { return &b }

// IntPtr and BoolPtr are helpers for building Params literals.
func IntPtr(n int) *int    { return intPtr(n) }
func BoolPtr(b bool) *bool { return boolPtr(b) }

// Encode renders the full query string. Parameter order is fixed so that a
// golden test can assert on the exact bytes: where, data, select, populate,
// joins, sort, depth, limit, page, draft, trash, locale, fallback-locale,
// extras (sorted), then --where-raw verbatim.
func (p Params) Encode() (string, error) {
	var parts []string
	add := func(k, v string) { parts = append(parts, k+"="+v) }

	if !p.Where.IsEmpty() {
		switch p.WhereStyle {
		case StyleQS:
			pairs, err := WhereBrackets(p.Where)
			if err != nil {
				return "", err
			}
			for _, pr := range pairs {
				add(pr.Key, pr.Value)
			}
		default:
			enc, err := p.Where.Encode()
			if err != nil {
				return "", err
			}
			add("where", enc)
		}
	}

	if p.Data != nil {
		b, err := marshalNoHTML(p.Data)
		if err != nil {
			return "", apierr.Wrap(err, apierr.CodeBadRequestBody, "the data parameter cannot be encoded as JSON")
		}
		add("data", url.QueryEscape(string(b)))
	}

	// select[f]=true, or select[f]=false for --select-exclude. Payload reads
	// both forms; mixing them in one request is the caller's choice.
	for _, f := range dedupe(p.Select) {
		pairs, err := Brackets("select", fieldTree(f, true))
		if err != nil {
			return "", err
		}
		appendPairs(&parts, pairs)
	}
	for _, f := range dedupe(p.SelectExclude) {
		pairs, err := Brackets("select", fieldTree(f, false))
		if err != nil {
			return "", err
		}
		appendPairs(&parts, pairs)
	}

	for _, coll := range sortedKeys(p.Populate) {
		for _, f := range dedupe(p.Populate[coll]) {
			pairs, err := Brackets("populate["+coll+"]", fieldTree(f, true))
			if err != nil {
				return "", err
			}
			appendPairs(&parts, pairs)
		}
	}

	for _, field := range sortedKeys(p.Joins) {
		opts := p.Joins[field]
		for _, k := range sortedKeys(opts) {
			add("joins["+escape(field)+"]["+escape(k)+"]", escape(opts[k]))
		}
	}

	if len(p.Sort) > 0 {
		add("sort", escape(strings.Join(p.Sort, ",")))
	}
	if p.Depth != nil {
		add("depth", strconv.Itoa(*p.Depth))
	}
	if p.Limit != nil {
		add("limit", strconv.Itoa(*p.Limit))
	}
	if p.Page > 0 {
		add("page", strconv.Itoa(p.Page))
	}
	if p.Draft != nil {
		add("draft", strconv.FormatBool(*p.Draft))
	}
	if p.Trash != nil {
		add("trash", strconv.FormatBool(*p.Trash))
	}
	if p.Locale != "" {
		add("locale", escape(p.Locale))
	}
	if p.FallbackLocale != "" {
		add("fallback-locale", escape(p.FallbackLocale))
	}
	for _, k := range sortedKeys(p.Extra) {
		for _, v := range p.Extra[k] {
			add(escape(k), escape(v))
		}
	}

	out := strings.Join(parts, "&")
	if raw := strings.TrimPrefix(strings.TrimSpace(p.WhereRaw), "?"); raw != "" {
		if out == "" {
			out = raw
		} else {
			out += "&" + raw
		}
	}
	return out, nil
}

func appendPairs(parts *[]string, pairs []Pair) {
	for _, pr := range pairs {
		*parts = append(*parts, pr.Key+"="+pr.Value)
	}
}

// fieldTree turns a dotted select path into the nested map bracket notation
// expects: "hero.media" -> {"hero":{"media":true}}.
func fieldTree(path string, leaf bool) any {
	segs := strings.Split(path, ".")
	var v any = leaf
	for i := len(segs) - 1; i >= 0; i-- {
		v = map[string]any{segs[i]: v}
	}
	return v
}

func marshalNoHTML(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// escape is url.QueryEscape: the server decodes both '+' and %20 as a space,
// and Go's decoder on the other end of our own tests does too.
func escape(s string) string { return url.QueryEscape(s) }
