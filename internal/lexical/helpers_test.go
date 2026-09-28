package lexical

import (
	"encoding/json"
	"strings"
	"testing"
)

// canonState reduces an editor state to what a round trip must preserve:
// everything except derived bookkeeping (direction, paragraph textFormat /
// textStyle, list item value/indent, link ids) and the presentation
// attributes ToMarkdown reports as lossy.
func canonState(state any) any {
	root, ok := rootOf(state)
	if !ok {
		return nil
	}
	return canonBlocks(childrenOf(root))
}

func canonBlocks(kids []any) []any {
	out := []any{}
	for _, k := range kids {
		n, ok := k.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, canonBlock(n))
	}
	return out
}

func canonBlock(n Node) any {
	switch t := typeOf(n); t {
	case "paragraph":
		return map[string]any{"t": t, "c": canonInline(childrenOf(n))}
	case "heading":
		return map[string]any{"t": t, "tag": n["tag"], "c": canonInline(childrenOf(n))}
	case "quote":
		kids := childrenOf(n)
		// A quote holding exactly one paragraph is the same quote as one
		// holding the paragraph's inline children (IssueNormalized).
		if len(kids) == 1 {
			if p, ok := kids[0].(map[string]any); ok && typeOf(p) == "paragraph" {
				kids = childrenOf(p)
			}
		}
		for _, k := range kids {
			if kn, ok := k.(map[string]any); ok && !isInlineType(typeOf(kn)) {
				return map[string]any{"t": t, "b": canonBlocks(kids)}
			}
		}
		return map[string]any{"t": t, "c": canonInline(kids)}
	case "list":
		start := any(nil)
		if strOf(n, "listType") == "number" {
			s, _ := intOf(n, "start")
			start = s
		}
		var items []any
		for _, k := range childrenOf(n) {
			it, ok := k.(map[string]any)
			if !ok {
				continue
			}
			kids := childrenOf(it)
			if len(kids) == 1 {
				if l, ok := kids[0].(map[string]any); ok && typeOf(l) == "list" {
					items = append(items, map[string]any{"nested": canonBlock(l)})
					continue
				}
			}
			ck := any(nil)
			if strOf(n, "listType") == "check" {
				b, _ := it["checked"].(bool)
				ck = b
			}
			items = append(items, map[string]any{"checked": ck, "c": canonInline(kids)})
		}
		return map[string]any{"t": t, "listType": n["listType"], "start": start, "items": items}
	case "horizontalrule":
		return map[string]any{"t": t}
	default:
		s, _ := encodePlaceholder(n)
		back, _, _ := decodePlaceholder(s)
		return map[string]any{"t": "node", "n": back}
	}
}

// mustJSON marshals for comparisons and failure messages.
func mustJSON(t testing.TB, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// decodeState parses JSON the way PayCLI does (exact numbers).
func decodeState(t testing.TB, s string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode: %v\n%s", err, s)
	}
	return v
}

// assertLexicalRoundTrip checks lexical→md→lexical: the Markdown reads back
// to the same content, and rendering that again gives the same Markdown.
func assertLexicalRoundTrip(t testing.TB, state any) string {
	t.Helper()
	md, err := ToMarkdown(state)
	if err != nil {
		t.Fatalf("ToMarkdown: %v", err)
	}
	back, err := FromMarkdown(md)
	if err != nil {
		t.Fatalf("FromMarkdown(ToMarkdown(x)): %v\n--- md ---\n%s", err, md)
	}
	if a, b := mustJSON(t, canonState(state)), mustJSON(t, canonState(back)); a != b {
		t.Fatalf("lexical→md→lexical changed the content\n--- md ---\n%s\n--- want ---\n%s\n--- got ---\n%s", md, a, b)
	}
	md2, err := ToMarkdown(back)
	if err != nil || md2 != md {
		t.Fatalf("Markdown not stable\n--- first ---\n%s\n--- second ---\n%s", md, md2)
	}
	return md
}

// mustJSONString marshals for messages and exact-shape comparisons.
func mustJSONString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "<unmarshalable: " + err.Error() + ">"
	}
	return string(b)
}
