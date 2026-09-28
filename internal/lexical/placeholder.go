package lexical

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Placeholders are how a node Markdown cannot spell survives a round trip:
//
//	<!-- lexical:upload {"fields":null,"relationTo":"media","value":40} -->
//
// ToMarkdown writes one for every upload, relationship, block, inlineBlock,
// table and unknown node (and for any block it cannot render losslessly), and
// FromMarkdown turns it back into the exact node. The JSON is the node minus
// its `type` (which is in the tag) and minus the keys whose value is the
// type's export default — so a hand-written `<!-- lexical:upload
// {"relationTo":"media","value":12} -->` is a valid, complete upload node.
//
// The JSON is marshalled with HTML escaping ON, so `<`, `>` and `&` inside a
// value are written as JSON unicode escapes (backslash-u003c and so on), so
// a value can never close the comment early.

// placeholderRe matches one placeholder comment, JSON optional. (?s) lets a
// hand-written placeholder spread its JSON over several lines.
var placeholderRe = regexp.MustCompile(`(?s)^<!--\s*lexical:([A-Za-z][A-Za-z0-9_-]*)(?:\s+(.*?))?\s*-->$`)

// placeholderDefaults are the keys stripped from (and restored into) a
// placeholder, per type: exactly the values each node's exportJSON writes when
// nothing was set. A type absent here keeps every key.
var placeholderDefaults = map[string]map[string]any{
	"upload":       {"version": 3, "format": "", "fields": nil},
	"relationship": {"version": 2, "format": ""},
	"block":        {"version": 2, "format": ""},
	"inlineBlock":  {"version": 1},
	"paragraph": {
		"version": 1, "format": "", "indent": 0, "textFormat": 0, "textStyle": "",
		"children": []any{},
	},
}

// placeholderRecomputed are keys dropped from a placeholder and recomputed on
// the way back, because they are derived state (Lexical's reconciler rewrites
// direction on every edit).
var placeholderRecomputed = map[string][]string{
	"paragraph": {"direction"},
}

// encodePlaceholder renders a node as a placeholder comment.
func encodePlaceholder(n Node) (string, error) {
	typ := typeOf(n)
	body := map[string]any{}
	defaults := placeholderDefaults[typ]
	drop := map[string]bool{"type": true}
	for _, k := range placeholderRecomputed[typ] {
		drop[k] = true
	}
	for k, v := range n {
		if drop[k] {
			continue
		}
		if dv, ok := defaults[k]; ok && sameJSON(dv, v) {
			continue
		}
		body[k] = reduceRelationValue(k, v)
	}
	if len(body) == 0 {
		return "<!-- lexical:" + typ + " -->", nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(true)
	if err := enc.Encode(body); err != nil {
		return "", fmt.Errorf("placeholder for %s node: %w", typ, err)
	}
	return "<!-- lexical:" + typ + " " + strings.TrimSpace(buf.String()) + " -->", nil
}

// reduceRelationValue turns a populated relationship value (a read at depth > 0
// returns the whole related document) back into its id, which is the only
// shape a write accepts without surprises. It applies to the top-level `value`
// of upload/relationship nodes.
func reduceRelationValue(key string, v any) any {
	if key != "value" {
		return v
	}
	if obj, ok := v.(map[string]any); ok {
		if id, ok := obj["id"]; ok {
			return id
		}
	}
	return v
}

// decodePlaceholder parses a placeholder comment. ok is false when s is not a
// placeholder at all; err is set when it is one whose JSON is broken — which
// is fatal, because silently turning a node into text would lose it.
func decodePlaceholder(s string) (Node, bool, error) {
	m := placeholderRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return nil, false, nil
	}
	typ := m[1]
	n := Node{}
	if m[2] != "" {
		dec := json.NewDecoder(strings.NewReader(m[2]))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, true, fmt.Errorf("the lexical:%s placeholder's JSON is invalid: %w", typ, err)
		}
		obj, isObj := v.(map[string]any)
		if !isObj {
			return nil, true, fmt.Errorf("the lexical:%s placeholder must hold a JSON object", typ)
		}
		n = obj
	}
	for k, dv := range placeholderDefaults[typ] {
		if _, ok := n[k]; !ok {
			n[k] = cloneJSON(dv)
		}
	}
	n["type"] = typ
	if typ == "paragraph" {
		n["direction"] = directionOf(childrenOf(n))
	}
	return n, true, nil
}

// sameJSON compares two JSON-ish values by their encoding, which makes 0,
// int(0), float64(0) and json.Number("0") equal.
func sameJSON(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(ab, bb)
}

func cloneJSON(v any) any {
	switch t := v.(type) {
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = cloneJSON(t[i])
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = cloneJSON(x)
		}
		return out
	}
	return v
}

// decodeJSONValue parses one JSON value, keeping numbers exact.
func decodeJSONValue(s string) (any, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if dec.More() {
		return nil, false
	}
	return v, true
}
