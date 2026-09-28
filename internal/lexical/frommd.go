package lexical

import (
	"fmt"
	"regexp"
	"strings"
)

// FromMarkdown converts Markdown to a Lexical editor state
// ({"root": {...}}) in the exact shape Payload 3.87's lexical editor stores.
// It fails only on a malformed `<!-- lexical:… -->` placeholder, because
// turning one into text would silently lose the node it carries; everything
// else converts, and what could not be represented is reported by
// FromMarkdownIssues.
func FromMarkdown(md string) (map[string]any, error) {
	state, _, err := FromMarkdownIssues(md)
	return state, err
}

// FromMarkdownIssues is FromMarkdown plus the list of non-fatal findings
// (dropped comments, HTML kept as text, flattened code blocks, link URLs
// Payload rejects or that are relative to the source file, …), in source
// order.
func FromMarkdownIssues(md string) (map[string]any, []Issue, error) {
	var issues []Issue
	// Lexical text is JSON text: invalid UTF-8 would be replaced on the way
	// to the server anyway, so it is replaced here, once, where the
	// conversion can still be consistent about it.
	md = strings.ToValidUTF8(md, "\uFFFD")
	// A UTF-8 byte order mark (Windows editors write one) is not content: left
	// in, it turns the first heading, list or quote into a paragraph whose
	// text starts with an invisible U+FEFF.
	md = strings.TrimPrefix(md, "\uFEFF")
	bp := &blockParser{issues: &issues}
	blocks := bp.parse(splitLines(md))
	c := &converter{issues: &issues, refs: bp.refs}
	children, err := c.blocks(blocks, 0)
	if err != nil {
		return nil, issues, err
	}
	return map[string]any{"root": newRoot(children)}, issues, nil
}

// frontmatterRe matches a candidate YAML front matter block at the very
// start: a `---` line, a body, and a closing `---` or `...` line.
var frontmatterRe = regexp.MustCompile(`^(?:\x{FEFF})?---[ \t]*\r?\n(?s:(.*?))\r?\n(?:---|\.\.\.)[ \t]*(?:\r?\n|$)`)

// yamlKeyRe is a YAML mapping key line: `key:` followed by a space, a tab or
// the end of the line (so `og:image: x` is a key and `https://x` is not),
// plain or quoted.
var yamlKeyRe = regexp.MustCompile(`^(?:[^\s#'"\[\]{}&*!|>%@` + "`" + `-][^#]*?|"[^"]*"|'[^']*')[ \t]*:(?:[ \t]|$)`)

// StripFrontmatter removes a leading YAML front matter block (`---` … `---`),
// which Markdown files written for static-site generators and knowledge bases
// carry and which CommonMark would otherwise read as a rule and a heading.
// ok reports whether one was found; fm is its body.
//
// The block must look like YAML: its first line directly follows the opening
// `---` (no blank line) and every line is a `key:` line, a list item, a
// comment, a blank line or an indented continuation. Anything else is
// Markdown that happens to start with a thematic break — a rule, a paragraph,
// a second rule — and is left alone: stripping it would delete content.
func StripFrontmatter(md string) (body, fm string, ok bool) {
	m := frontmatterRe.FindStringSubmatchIndex(md)
	if m == nil {
		return md, "", false
	}
	fm = md[m[2]:m[3]]
	if !looksLikeYAML(fm) {
		return md, "", false
	}
	return md[m[1]:], fm, true
}

// looksLikeYAML is the front matter test: a first line that is a key or a
// list item, and no line that could only be Markdown prose.
func looksLikeYAML(fm string) bool {
	lines := strings.Split(strings.ReplaceAll(fm, "\r\n", "\n"), "\n")
	for i, l := range lines {
		t := strings.TrimRight(l, " \t\r")
		switch {
		case t == "":
			if i == 0 {
				return false
			}
		case t[0] == ' ' || t[0] == '\t':
			if i == 0 {
				return false
			}
		case t[0] == '#':
			// A YAML comment; also what a Markdown heading looks like, so it
			// cannot be the first line.
			if i == 0 {
				return false
			}
		case t == "-" || strings.HasPrefix(t, "- "):
		case yamlKeyRe.MatchString(t):
		default:
			return false
		}
	}
	return true
}

type converter struct {
	issues *[]Issue
	refs   map[string]string
}

func (c *converter) issue(code string, line int, msg string) {
	*c.issues = append(*c.issues, Issue{Code: code, Line: line, Message: msg})
}

// blocks converts parsed blocks to Lexical block nodes. depth is the list
// nesting depth (for list item indent).
func (c *converter) blocks(blocks []*mdBlock, depth int) ([]any, error) {
	var out []any
	for _, b := range blocks {
		switch b.kind {
		case bParagraph:
			nodes, err := c.paragraph(b)
			if err != nil {
				return nil, err
			}
			out = append(out, nodes...)
		case bHeading:
			kids, err := c.inlineFlat(b.text, b.line)
			if err != nil {
				return nil, err
			}
			out = append(out, newHeading(b.level, kids))
		case bQuote:
			q, err := c.quote(b, depth)
			if err != nil {
				return nil, err
			}
			out = append(out, q)
		case bList:
			l, err := c.list(b, depth)
			if err != nil {
				return nil, err
			}
			out = append(out, l)
		case bRule:
			out = append(out, newHorizontalRule())
		case bCode:
			c.issue(IssueCodeBlockFlattened, b.line,
				"a code block became a paragraph of inline code; Payload's default editor has no code block node (projects use a Code block)")
			out = append(out, codeParagraph(b.text))
		case bComment:
			n, _, err := decodePlaceholder(b.text)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", b.line, err)
			}
			if isInlineType(typeOf(n)) {
				n = newParagraph([]any{n})
			}
			out = append(out, n)
		}
	}
	return out, nil
}

// codeParagraph renders a code block as one paragraph of code-formatted text
// with line breaks, which is what an editor paste of code produces.
func codeParagraph(code string) Node {
	var kids []any
	for i, line := range strings.Split(code, "\n") {
		if i > 0 {
			kids = append(kids, newLinebreak())
		}
		kids = append(kids, splitTabs(line, FormatCode)...)
	}
	return newParagraph(kids)
}

// paragraph converts one paragraph. It may yield several nodes: an upload or
// a block-level placeholder inside the text splits the paragraph around it,
// because Lexical's decorator blocks cannot live inside a paragraph.
func (c *converter) paragraph(b *mdBlock) ([]any, error) {
	list, err := parseInline(b.text, b.line, c.issues, c.refs)
	if err != nil {
		return nil, fmt.Errorf("line %d: %w", b.line, err)
	}
	items := c.inlines(list, 0)
	var out []any
	var run []any
	// Whitespace is trimmed only where the paragraph was split: the block
	// parser already trimmed the paragraph's own edges, and a space written
	// as &#32; at an edge is content that must survive.
	afterSplit := false
	flush := func(beforeSplit bool) {
		run = trimRun(run, afterSplit, beforeSplit)
		if len(run) > 0 {
			out = append(out, newParagraph(run))
		}
		run = nil
	}
	for _, it := range items {
		if bb, ok := it.(blockBreak); ok {
			flush(true)
			out = append(out, bb.node)
			afterSplit = true
			continue
		}
		run = append(run, it)
	}
	flush(false)
	return out, nil
}

// inlineFlat converts inline source where a block break is not allowed
// (headings): an upload or block placeholder stays inline.
func (c *converter) inlineFlat(src string, line int) ([]any, error) {
	list, err := parseInline(src, line, c.issues, c.refs)
	if err != nil {
		return nil, fmt.Errorf("line %d: %w", line, err)
	}
	items := c.inlines(list, 0)
	out := make([]any, 0, len(items))
	for _, it := range items {
		if bb, ok := it.(blockBreak); ok {
			out = append(out, bb.node)
			continue
		}
		out = append(out, it)
	}
	return mergeText(out), nil
}

// blockBreak marks a block-level node met in inline content.
type blockBreak struct{ node Node }

// inlines flattens a parsed inline list into Lexical inline nodes, applying
// the accumulated format bits.
func (c *converter) inlines(l *inlList, format int) []any {
	var out []any
	if l == nil {
		return out
	}
	for n := l.head; n != nil; n = n.next {
		switch n.kind {
		case inText, inHTMLOpen:
			if n.text != "" {
				out = append(out, splitTabs(n.text, format)...)
			}
		case inSoft:
			out = append(out, newText(" ", format))
		case inCode:
			out = append(out, splitTabs(n.text, format|FormatCode)...)
		case inBreak:
			out = append(out, newLinebreak())
		case inFmt:
			out = append(out, c.inlines(n.kids, format|n.fmt)...)
		case inLink:
			kids := mergeText(c.inlines(n.kids, format))
			out = append(out, newLink(n.target, stripBreaks(kids)))
		case inPlaceholder:
			if isInlineType(typeOf(n.node)) {
				out = append(out, n.node)
			} else {
				out = append(out, blockBreak{node: n.node})
			}
		case inUpload:
			out = append(out, blockBreak{node: n.node})
		}
	}
	return out
}

// stripBreaks keeps block breaks out of a link: a link holds inline nodes
// only, so an upload inside link text is kept as its own inline node.
func stripBreaks(in []any) []any {
	out := make([]any, 0, len(in))
	for _, it := range in {
		if bb, ok := it.(blockBreak); ok {
			out = append(out, bb.node)
			continue
		}
		out = append(out, it)
	}
	return out
}

// splitTabs turns a text run into text and tab nodes, which is what the
// editor stores for a tab character.
func splitTabs(s string, format int) []any {
	if !strings.Contains(s, "\t") {
		return []any{newText(s, format)}
	}
	var out []any
	for i, part := range strings.Split(s, "\t") {
		if i > 0 {
			out = append(out, newTab(format))
		}
		if part != "" {
			out = append(out, newText(part, format))
		}
	}
	return out
}

// mergeText joins adjacent text nodes of equal format, as Lexical's
// normalisation does, and drops empty ones.
func mergeText(in []any) []any {
	out := make([]any, 0, len(in))
	for _, it := range in {
		n, ok := it.(map[string]any)
		if !ok {
			out = append(out, it)
			continue
		}
		if typeOf(n) == "text" {
			if strOf(n, "text") == "" {
				continue
			}
			if len(out) > 0 {
				if prev, ok := out[len(out)-1].(map[string]any); ok && typeOf(prev) == "text" {
					pf, _ := intOf(prev, "format")
					nf, _ := intOf(n, "format")
					if pf == nf {
						prev["text"] = strOf(prev, "text") + strOf(n, "text")
						continue
					}
				}
			}
		}
		out = append(out, n)
	}
	return out
}

// trimRun merges a paragraph's text and removes the whitespace next to a
// split point (left: the run follows a split; right: one follows it).
func trimRun(in []any, left, right bool) []any {
	in = mergeText(in)
	for left && len(in) > 0 {
		n, ok := in[0].(map[string]any)
		if !ok || typeOf(n) != "text" {
			break
		}
		t := strings.TrimLeft(strOf(n, "text"), " \n")
		if t != "" {
			n["text"] = t
			break
		}
		in = in[1:]
	}
	for right && len(in) > 0 {
		n, ok := in[len(in)-1].(map[string]any)
		if !ok || typeOf(n) != "text" {
			break
		}
		t := strings.TrimRight(strOf(n, "text"), " \n")
		if t != "" {
			n["text"] = t
			break
		}
		in = in[:len(in)-1]
	}
	return in
}

// quote converts a block quote. One paragraph becomes Lexical's native quote
// (inline children, which is what the editor and Payload's Markdown import
// produce); anything richer keeps its block children.
func (c *converter) quote(b *mdBlock, depth int) (Node, error) {
	if len(b.children) == 1 && b.children[0].kind == bParagraph {
		kids, err := c.inlineFlat(b.children[0].text, b.children[0].line)
		if err != nil {
			return nil, err
		}
		return newQuote(kids), nil
	}
	if len(b.children) == 0 {
		return newQuote(nil), nil
	}
	kids, err := c.blocks(b.children, depth)
	if err != nil {
		return nil, err
	}
	return newQuote(kids), nil
}

// list converts a list. Nested lists become Lexical's wrapper items: a
// listitem whose only child is the nested list, carrying the value of the
// item after it (lexical's $updateChildrenListItemValue does not advance the
// counter for a wrapper). Nested items carry indent = depth.
func (c *converter) list(b *mdBlock, depth int) (Node, error) {
	// A bullet list with any task item is a check list: Lexical types the
	// whole list, and the first item may be a wrapper for a nested list.
	listType := "bullet"
	if b.ordered {
		listType = "number"
	} else {
		taskLine, plain := 0, 0
		for _, it := range b.items {
			switch {
			case it.task && taskLine == 0:
				taskLine = it.line
			case !it.task && !it.bare:
				plain++
			}
		}
		if taskLine > 0 {
			listType = "check"
			if plain > 0 {
				c.issue(IssueListBecameChecklist, taskLine, fmt.Sprintf(
					"the task item on this line made the whole list a check list: its %d plain item(s) became unchecked checkboxes (a Lexical list has one type)", plain))
			}
		}
	}
	start := b.start
	if !b.ordered {
		start = 1
	}
	var items []any
	value := start
	for _, it := range b.items {
		var checked *bool
		if listType == "check" {
			v := it.checked
			checked = &v
		}
		var inline []any
		var nested []any
		paras := 0
		for _, blk := range it.blocks {
			switch blk.kind {
			case bList:
				l, err := c.list(blk, depth+1)
				if err != nil {
					return nil, err
				}
				nested = append(nested, l)
			case bParagraph, bHeading:
				if blk.kind == bHeading {
					c.issue(IssueListItemBlockFlattened, blk.line,
						"a heading inside a list item became the item's text; a Lexical list item holds inline content only")
				}
				kids, err := c.inlineFlat(blk.text, blk.line)
				if err != nil {
					return nil, err
				}
				if paras > 0 {
					inline = append(inline, newLinebreak())
				}
				paras++
				inline = append(inline, kids...)
			default:
				// A quote, rule, code block or placeholder inside an item has
				// no native Lexical spelling; keep the content as inline text
				// where possible, else as a following block-level node in a
				// wrapper (the same shape a nested list takes).
				sub, err := c.blocks([]*mdBlock{blk}, depth+1)
				if err != nil {
					return nil, err
				}
				if blk.kind == bQuote {
					c.issue(IssueListItemBlockFlattened, blk.line,
						"a block quote inside a list item became the item's text; a Lexical list item holds inline content only")
				}
				lines, blocks := flattenItemBlocks(sub, 0)
				for _, ln := range lines {
					if paras > 0 {
						inline = append(inline, newLinebreak())
					}
					paras++
					inline = append(inline, ln...)
				}
				nested = append(nested, blocks...)
			}
		}
		if paras > 1 {
			c.issue(IssueItemParagraphsJoined, it.line,
				"a list item's paragraphs were joined with a line break; a Lexical list item holds inline content only")
		}
		if len(inline) > 0 || len(nested) == 0 || !it.bare {
			items = append(items, newListItem(value, checked, depth, mergeText(inline)))
			value++
		}
		for _, n := range nested {
			items = append(items, newListItem(value, nil, depth, []any{n}))
		}
	}
	return newList(listType, start, items), nil
}

// flattenItemBlocks turns the block nodes converted from something inside a
// list item (a quote, a code block, a placeholder) into what a Lexical list
// item can hold: lines of inline content (a paragraph's or heading's
// children, and — recursively — every such line inside a quote, however
// deeply nested) and block nodes that follow the item as wrapper items (a
// nested list, a rule, an upload or block placeholder). A listitem never gets
// a block child: that is not a shape Lexical's editor produces, and it does
// not survive the round trip through Markdown.
func flattenItemBlocks(nodes []any, depth int) (lines [][]any, blocks []any) {
	for _, s := range nodes {
		sn, ok := s.(map[string]any)
		if !ok {
			continue
		}
		switch typeOf(sn) {
		case "paragraph", "heading":
			lines = append(lines, childrenOf(sn))
		case "quote":
			kids := childrenOf(sn)
			if depth >= maxFlattenDepth || !hasBlockChild(kids) {
				// Lexical's native quote: its children are inline.
				lines = append(lines, kids)
				continue
			}
			l, b := flattenItemBlocks(kids, depth+1)
			lines = append(lines, l...)
			blocks = append(blocks, b...)
		default:
			blocks = append(blocks, s)
		}
	}
	return lines, blocks
}

// maxFlattenDepth bounds flattenItemBlocks' recursion into quotes.
const maxFlattenDepth = 64

// hasBlockChild reports whether a node list holds block-level nodes (a quote
// that holds more than one paragraph, a heading or a list) rather than inline
// content.
func hasBlockChild(kids []any) bool {
	for _, k := range kids {
		if n, ok := k.(map[string]any); ok && !isInlineType(typeOf(n)) {
			return true
		}
	}
	return false
}
