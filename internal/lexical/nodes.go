package lexical

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Text format bits, exactly Lexical's TextNode format bitmask
// (lexical@0.41 IS_BOLD … IS_HIGHLIGHT). Bits 256 (lowercase), 512
// (uppercase) and 1024 (capitalize) exist in Lexical but have no Payload
// feature and no Markdown spelling; ToMarkdown reports them as lossy.
const (
	FormatBold          = 1
	FormatItalic        = 2
	FormatStrikethrough = 4
	FormatUnderline     = 8
	FormatCode          = 16
	FormatSubscript     = 32
	FormatSuperscript   = 64
	FormatHighlight     = 128

	// knownFormats is every bit this package can spell in Markdown.
	knownFormats = FormatBold | FormatItalic | FormatStrikethrough | FormatUnderline |
		FormatCode | FormatSubscript | FormatSuperscript | FormatHighlight
)

// Node is one serialized Lexical node.
type Node = map[string]any

// Issue is a non-fatal finding of a conversion: something that was dropped,
// normalised or could not be expressed. Conversions never fail on content they
// can represent some other way; they report it here instead so the caller can
// decide whether the loss matters.
type Issue struct {
	// Code is a stable identifier (see the Issue* constants).
	Code string `json:"code"`
	// Path addresses the node in the editor state ("root.children[3]") for
	// ToMarkdown, and is empty for FromMarkdown.
	Path string `json:"path,omitempty"`
	// Line is the 1-based Markdown source line for FromMarkdown, 0 otherwise.
	Line int `json:"line,omitempty"`
	// Message says what happened in plain words.
	Message string `json:"message"`
}

// Issue codes. FromMarkdown raises the first group, ToMarkdown the second.
const (
	// IssueHTMLAsText: raw HTML this dialect does not model was kept as
	// literal text (Lexical has no HTML node).
	IssueHTMLAsText = "html_as_text"
	// IssueCommentDropped: an HTML comment that is not a lexical placeholder
	// was removed.
	IssueCommentDropped = "comment_dropped"
	// IssueImageAsText: a Markdown image that is not the `![collection:id]()`
	// upload form was kept as literal text (an upload node needs a media id).
	IssueImageAsText = "image_as_text"
	// IssueCodeBlockFlattened: a fenced or indented code block became a
	// paragraph of inline-code text (Payload's default editor has no code
	// block node; projects model code as a block).
	IssueCodeBlockFlattened = "code_block_flattened"
	// IssueTableAsText: a GFM pipe table was kept as paragraph text.
	IssueTableAsText = "table_as_text"
	// IssueItemParagraphsJoined: a list item with several paragraphs was
	// joined with line breaks (a Lexical list item holds inline content only).
	IssueItemParagraphsJoined = "list_item_paragraphs_joined"
	// IssueListItemBlockFlattened: a heading or block quote inside a list
	// item lost its block type and became the item's inline text (a Lexical
	// list item holds inline content only).
	IssueListItemBlockFlattened = "list_item_block_flattened"
	// IssueListBecameChecklist: a bullet list mixing task items (`- [ ]`)
	// and plain items became a check list; the plain items are unchecked
	// checkboxes now (a Lexical list has one listType).
	IssueListBecameChecklist = "list_became_checklist"
	// IssueNestingCapped: quotes/lists nested deeper than the parser's bound;
	// the deeper lines were kept as paragraph text.
	IssueNestingCapped = "nesting_capped"
	// IssueLinkRefUndefined: a full or collapsed reference link
	// (`[text][label]`, `[label][]`) names a label no `[label]: url`
	// definition declares; it was kept as text.
	IssueLinkRefUndefined = "link_reference_undefined"
	// IssueLinkURLInvalid: a custom link whose URL is empty or contains a
	// space. Payload's default link validation (url required, and
	// validateUrlMinimal: no spaces) rejects the document on any write that
	// validates — a publish or a write without --draft. Verified live for the
	// empty URL: "link node failed to validate".
	IssueLinkURLInvalid = "link_url_invalid"
	// IssueLinkURLRelative: a link URL that is relative to the Markdown file
	// (`../brief.md`, `page.html`, `www.example.com`) rather than a site path
	// (`/…`), an anchor (`#…`), a query (`?…`) or an absolute URL. On the site
	// it resolves against the page's URL, which is almost never what the
	// source meant.
	IssueLinkURLRelative = "link_url_relative"

	// IssueLossyStateKeys: the editor state carries non-null keys next to
	// `root`; Markdown holds the root only.
	IssueLossyStateKeys = "lossy_state_keys"
	// IssueLossyAlignment: an element's format (alignment) or indent has no
	// Markdown spelling and is lost.
	IssueLossyAlignment = "lossy_alignment"
	// IssueLossyStyle: inline CSS (text `style`, element `textStyle`) is lost.
	IssueLossyStyle = "lossy_style"
	// IssueLossyFormat: a text format bit with no Markdown spelling
	// (lowercase/uppercase/capitalize) is lost.
	IssueLossyFormat = "lossy_format"
	// IssuePlaceholder: a node was written as a lossless
	// `<!-- lexical:TYPE {…} -->` placeholder because Markdown cannot express
	// it. Informational: FromMarkdown restores it exactly.
	IssuePlaceholder = "placeholder"
	// IssueNormalized: a shape Lexical's editor never produces (a quote
	// holding one paragraph, a list item holding a paragraph or text followed
	// by a nested list) was written as ordinary Markdown and reads back as
	// the editor's native shape. The content is unchanged.
	IssueNormalized = "normalized"
)

// IsEditorState reports whether v is a Lexical editor state: an object whose
// "root" is an object of type "root" with a children array. It is the test
// every "find the rich text in this document" walk uses, so it is strict about
// the root and says nothing about the children.
func IsEditorState(v any) bool {
	_, ok := rootOf(v)
	return ok
}

// rootOf returns the root node of an editor state.
func rootOf(v any) (Node, bool) {
	state, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	root, ok := state["root"].(map[string]any)
	if !ok {
		return nil, false
	}
	if t, _ := root["type"].(string); t != "root" {
		return nil, false
	}
	if _, ok := root["children"].([]any); !ok {
		return nil, false
	}
	return root, true
}

// Walk visits every node of an editor state depth-first, parents before
// children. path is the node's address ("root", "root.children[2]",
// "root.children[2].children[0]"). Returning false from fn skips that node's
// children. Walk does nothing when state is not an editor state.
func Walk(state any, fn func(n Node, path string) bool) {
	root, ok := rootOf(state)
	if !ok {
		return
	}
	walkNode(root, "root", fn)
}

func walkNode(n Node, path string, fn func(Node, string) bool) {
	if !fn(n, path) {
		return
	}
	for i, c := range childrenOf(n) {
		if cn, ok := c.(map[string]any); ok {
			walkNode(cn, path+".children["+strconv.Itoa(i)+"]", fn)
		}
	}
}

// NodeTypes counts the node types of an editor state, root included. It is
// what a caller prints to show what a conversion produced.
func NodeTypes(state any) map[string]int {
	out := map[string]int{}
	Walk(state, func(n Node, _ string) bool {
		out[typeOf(n)]++
		return true
	})
	return out
}

// ---------------------------------------------------------------------------
// accessors — tolerant of float64 and json.Number, because a state decoded by
// PayCLI keeps numbers exact (UseNumber) and one decoded elsewhere does not.
// ---------------------------------------------------------------------------

func typeOf(n Node) string {
	s, _ := n["type"].(string)
	return s
}

func childrenOf(n Node) []any {
	c, _ := n["children"].([]any)
	return c
}

func strOf(n Node, key string) string {
	s, _ := n[key].(string)
	return s
}

// intOf reads a numeric key; ok is false when it is absent or not a number.
func intOf(n Node, key string) (int, bool) {
	return toInt(n[key])
}

func toInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case float64:
		return int(t), true
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return int(i), true
		}
		if f, err := t.Float64(); err == nil {
			return int(f), true
		}
	}
	return 0, false
}

// isInlineType reports whether a node type lives inside a paragraph rather
// than at block level. The set is Lexical's and Payload's inline nodes; an
// unknown type is treated as block-level, which is what every decorator
// Payload ships (upload, relationship, block) is.
func isInlineType(t string) bool {
	switch t {
	case "text", "linebreak", "tab", "link", "autolink", "inlineBlock":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// constructors — the exact shapes Payload 3.87 / Lexical 0.41 export.
//
// Grounding (read-only, 2026-09-28): 351 editor states on the klixpert stage
// (pages, posts, customer-stories, team-blog, jobs, lead-magnets, draft=true)
// and the local website-template instance; exportJSON() of ElementNode,
// ParagraphNode, TextNode, TabNode (lexical@0.41.0), HeadingNode, QuoteNode
// (@lexical/rich-text), ListNode/ListItemNode (@lexical/list), and Payload's
// LinkNode (version 3), AutoLinkNode (version 2), UploadServerNode (version 3),
// RelationshipServerNode (version 2), BlockNode (version 2), InlineBlockNode
// (version 1), HorizontalRuleServerNode (version 1).
// ---------------------------------------------------------------------------

func newRoot(children []any) Node {
	n := element("root", children)
	return n
}

// element is ElementNode.exportJSON: children, direction, format, indent,
// type, version. direction is what Lexical's reconciler would compute from
// the text ("ltr"/"rtl"), or null for an element without text — the value an
// empty paragraph has on the stage.
func element(typ string, children []any) Node {
	if children == nil {
		children = []any{}
	}
	return Node{
		"children":  children,
		"direction": directionOf(children),
		"format":    "",
		"indent":    0,
		"type":      typ,
		"version":   1,
	}
}

// newParagraph is ParagraphNode.exportJSON: an element plus textFormat and
// textStyle, which Lexical 0.41 always writes and derives from the first
// direct text child (0 and "" when there is none).
func newParagraph(children []any) Node {
	n := element("paragraph", children)
	tf := 0
	for _, c := range children {
		if cn, ok := c.(map[string]any); ok && typeOf(cn) == "text" {
			tf, _ = intOf(cn, "format")
			break
		}
	}
	n["textFormat"] = tf
	n["textStyle"] = ""
	return n
}

func newHeading(level int, children []any) Node {
	n := element("heading", children)
	n["tag"] = "h" + strconv.Itoa(level)
	return n
}

func newQuote(children []any) Node { return element("quote", children) }

// newList is ListNode.exportJSON: listType bullet|number|check, start, and
// tag ul|ol (a check list is a ul).
func newList(listType string, start int, items []any) Node {
	n := element("list", items)
	n["listType"] = listType
	n["start"] = start
	if listType == "number" {
		n["tag"] = "ol"
	} else {
		n["tag"] = "ul"
	}
	return n
}

// newListItem is ListItemNode.exportJSON: value, and checked ONLY for a check
// list (Lexical leaves it undefined, i.e. absent, otherwise). indent is the
// nesting depth, which is what the editor stores for a nested item.
func newListItem(value int, checked *bool, indent int, children []any) Node {
	n := element("listitem", children)
	n["value"] = value
	n["indent"] = indent
	if checked != nil {
		n["checked"] = *checked
	}
	return n
}

func newText(text string, format int) Node {
	return Node{
		"detail":  0,
		"format":  format,
		"mode":    "normal",
		"style":   "",
		"text":    text,
		"type":    "text",
		"version": 1,
	}
}

// newTab is TabNode.exportJSON: a text node whose text is "\t" and whose
// detail is IS_UNMERGEABLE (2).
func newTab(format int) Node {
	n := newText("\t", format)
	n["type"] = "tab"
	n["detail"] = 2
	return n
}

func newLinebreak() Node { return Node{"type": "linebreak", "version": 1} }

func newHorizontalRule() Node { return Node{"type": "horizontalrule", "version": 1} }

// linkTarget is what a Markdown link destination resolved to.
type linkTarget struct {
	url      string
	internal bool
	relation string
	docID    any // number or string; nil for `pay:` with no document
	newTab   bool
	autolink bool
	extra    map[string]any // further link fields, from the {…} attributes
}

// newLink is Payload's LinkNode.exportJSON (version 3). A custom link carries
// {linkType, newTab, url}; an internal one {doc:{relationTo,value}, linkType,
// newTab} — exportJSON deletes the other key, so neither shape has both.
// AutoLinkNode is the same element with type "autolink", version 2, and the
// fields the editor's AutoLinkPlugin writes: {linkType:"custom", url}.
func newLink(t linkTarget, children []any) Node {
	n := element("link", children)
	fields := map[string]any{}
	switch {
	case t.autolink:
		n["type"] = "autolink"
		n["version"] = 2
		fields["linkType"] = "custom"
		fields["url"] = t.url
	case t.internal:
		n["version"] = 3
		fields["linkType"] = "internal"
		fields["newTab"] = t.newTab
		if t.relation == "" {
			fields["doc"] = nil
		} else {
			fields["doc"] = map[string]any{"relationTo": t.relation, "value": t.docID}
		}
	default:
		n["version"] = 3
		fields["linkType"] = "custom"
		fields["newTab"] = t.newTab
		fields["url"] = t.url
	}
	for k, v := range t.extra {
		fields[k] = v
	}
	n["fields"] = fields
	return n
}

// ---------------------------------------------------------------------------
// direction — Lexical's getTextDirection: the first strong character decides.
// ---------------------------------------------------------------------------

func directionOf(children []any) any {
	var sb strings.Builder
	collectText(children, &sb)
	switch textDirection(sb.String()) {
	case "rtl":
		return "rtl"
	case "ltr":
		return "ltr"
	}
	return nil
}

func collectText(children []any, sb *strings.Builder) {
	for _, c := range children {
		cn, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if s, ok := cn["text"].(string); ok {
			sb.WriteString(s)
		}
		collectText(childrenOf(cn), sb)
	}
}

// textDirection mirrors lexical's RTL_REGEX / LTR_REGEX: whichever class of
// strong character appears first wins; text with neither has no direction.
func textDirection(s string) string {
	for _, r := range s {
		switch {
		case isRTL(r):
			return "rtl"
		case isLTR(r):
			return "ltr"
		}
	}
	return ""
}

func isRTL(r rune) bool {
	return (r >= 0x0591 && r <= 0x07FF) || (r >= 0xFB1D && r <= 0xFDFD) || (r >= 0xFE70 && r <= 0xFEFC)
}

func isLTR(r rune) bool {
	switch {
	case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		return true
	case r >= 0x00C0 && r <= 0x00D6, r >= 0x00D8 && r <= 0x00F6, r >= 0x00F8 && r <= 0x02B8:
		return true
	case r >= 0x0300 && r <= 0x0590, r >= 0x0800 && r <= 0x1FFF, r == 0x200E:
		return true
	case r >= 0x2C00 && r <= 0xFB1C, r >= 0xFE00 && r <= 0xFE6F, r >= 0xFEFD && r <= 0xFFFF:
		return true
	}
	return false
}
