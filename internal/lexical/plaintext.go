package lexical

import "strings"

// PlainText extracts the readable text of an editor state: blocks separated
// by a blank line, list items and table rows by a newline, table cells by
// " | ", a linebreak as "\n" and a tab as "\t". Uploads, relationships,
// blocks, inline blocks and rules contribute nothing — they carry no text of
// their own. It returns "" for a value that is not an editor state.
//
// The separators follow Payload's own convertLexicalToPlaintext heuristic
// (@payloadcms/richtext-lexical 3.87), except that a quote is a block here
// too, so quoted text does not run into the paragraph before it.
func PlainText(state any) string {
	return plainText(state, textWalker{})
}

// PlainTextWithPlaceholders is PlainText for a reader who must SEE every
// node, text or not: a diff that has to show a swapped image, an outline that
// summarises a block holding only an upload. Each node without text of its
// own is rendered as a short bracketed placeholder where PlainText renders
// nothing:
//
//	upload          [upload media:12]       (relationTo:id; a populated value by its id)
//	relationship    [relationship pages:47]
//	block           [block cta]             (fields.blockType; [block] without one)
//	inlineBlock     [inlineBlock banner]
//	horizontalrule  ---
//	any other node type without children    [TYPE]
//
// Separators are PlainText's. The placeholders are for reading only: they are
// not Markdown and FromMarkdown does not turn them back into nodes (the
// lossless form is ToMarkdown's <!-- lexical:… --> placeholder).
func PlainTextWithPlaceholders(state any) string {
	return plainText(state, textWalker{placeholders: true})
}

// textWalker is the one walk behind both plain-text renderings.
type textWalker struct {
	// placeholders renders text-less nodes as [bracketed] placeholders.
	placeholders bool
}

func plainText(state any, w textWalker) string {
	root, ok := rootOf(state)
	if !ok {
		return ""
	}
	return strings.TrimSpace(w.joinBlocks(childrenOf(root)))
}

func (w textWalker) joinBlocks(kids []any) string {
	var parts []string
	for _, k := range kids {
		n, ok := k.(map[string]any)
		if !ok {
			continue
		}
		var s string
		if isInlineType(typeOf(n)) {
			s = w.inlineText([]any{n})
		} else {
			s = w.blockText(n)
		}
		if strings.TrimSpace(s) != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n\n")
}

func (w textWalker) blockText(n Node) string {
	switch typeOf(n) {
	case "paragraph", "heading":
		return w.inlineText(childrenOf(n))
	case "list":
		var items []string
		for _, c := range childrenOf(n) {
			it, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if s := w.itemText(it); strings.TrimSpace(s) != "" {
				items = append(items, s)
			}
		}
		return strings.Join(items, "\n")
	case "table":
		var rows []string
		for _, c := range childrenOf(n) {
			row, ok := c.(map[string]any)
			if !ok {
				continue
			}
			var cells []string
			for _, cc := range childrenOf(row) {
				cell, ok := cc.(map[string]any)
				if !ok {
					continue
				}
				cells = append(cells, strings.ReplaceAll(w.joinBlocks(childrenOf(cell)), "\n\n", " "))
			}
			rows = append(rows, strings.Join(cells, " | "))
		}
		return strings.Join(rows, "\n")
	}
	if s, ok := w.placeholder(n); ok {
		return s
	}
	kids := childrenOf(n)
	for _, k := range kids {
		if kn, ok := k.(map[string]any); ok && isInlineType(typeOf(kn)) {
			// An element with inline children (a native quote, or an
			// unknown element): its text as one block.
			return w.inlineText(kids)
		}
	}
	return w.joinBlocks(kids)
}

// itemText is one list item: its inline text, then any nested list on the
// following lines.
func (w textWalker) itemText(it Node) string {
	var inline []any
	var nested []string
	for _, k := range childrenOf(it) {
		kn, ok := k.(map[string]any)
		if !ok {
			continue
		}
		if isInlineType(typeOf(kn)) {
			inline = append(inline, kn)
			continue
		}
		if s := w.blockText(kn); s != "" {
			nested = append(nested, s)
		}
	}
	parts := make([]string, 0, 1+len(nested))
	if s := w.inlineText(inline); s != "" {
		parts = append(parts, s)
	}
	parts = append(parts, nested...)
	return strings.Join(parts, "\n")
}

func (w textWalker) inlineText(kids []any) string {
	var sb strings.Builder
	for _, k := range kids {
		n, ok := k.(map[string]any)
		if !ok {
			continue
		}
		switch typeOf(n) {
		case "text":
			sb.WriteString(strOf(n, "text"))
		case "tab":
			sb.WriteByte('\t')
		case "linebreak":
			sb.WriteByte('\n')
		case "link", "autolink":
			sb.WriteString(w.inlineText(childrenOf(n)))
		default:
			if s, ok := w.placeholder(n); ok {
				sb.WriteString(s)
				continue
			}
			if kids := childrenOf(n); len(kids) > 0 {
				sb.WriteString(w.inlineText(kids))
			}
		}
	}
	return sb.String()
}

// placeholder renders a node that has no text of its own. ok is false when
// placeholders are off, and for an element (a node with a children key, even
// an empty one: an empty paragraph is not a decorator).
func (w textWalker) placeholder(n Node) (string, bool) {
	if !w.placeholders {
		return "", false
	}
	t := typeOf(n)
	switch t {
	case "horizontalrule":
		return "---", true
	case "upload", "relationship":
		return "[" + t + " " + relationRef(n) + "]", true
	case "block", "inlineBlock":
		fields, _ := n["fields"].(map[string]any)
		if bt, _ := fields["blockType"].(string); bt != "" {
			return "[" + t + " " + bt + "]", true
		}
		return "[" + t + "]", true
	case "text", "tab", "linebreak", "":
		return "", false
	}
	if _, isElement := n["children"]; isElement {
		return "", false
	}
	return "[" + t + "]", true
}

// relationRef renders an upload/relationship node's target as
// "collection:id", accepting both the depth-0 id and a populated document.
func relationRef(n Node) string {
	rel, _ := n["relationTo"].(string)
	v := n["value"]
	if doc, ok := v.(map[string]any); ok {
		v = doc["id"]
	}
	id := idString(v)
	switch {
	case rel == "" && id == "":
		return "?"
	case rel == "":
		return id
	case id == "":
		return rel
	}
	return rel + ":" + id
}
