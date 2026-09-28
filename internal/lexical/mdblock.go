package lexical

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The block half of the Markdown reader. It is a line-based, recursive
// rendition of CommonMark's container algorithm, restricted to the constructs
// Lexical can hold: ATX and setext headings, paragraphs, block quotes (with
// lazy continuation), bullet/ordered/task lists (nested by indentation),
// thematic breaks, fenced and indented code, and HTML-comment blocks (which
// carry lexical placeholders). Everything else is paragraph text.

// srcLine is one input line and its 1-based number in the original source,
// kept through container stripping so issues can name a line.
type srcLine struct {
	text string
	no   int
}

type blockKind int

const (
	bParagraph blockKind = iota
	bHeading
	bQuote
	bList
	bRule
	bCode
	bComment
)

// mdBlock is one parsed block.
type mdBlock struct {
	kind  blockKind
	line  int
	level int // heading level

	// text is the raw inline source of a paragraph or heading, lines joined
	// with "\n"; the code content of a code block; the comment of bComment.
	text string

	children []*mdBlock // quote

	// list
	ordered bool
	marker  byte // '-', '+', '*' for bullets; '.' or ')' for ordered
	start   int
	items   []*mdItem
}

type mdItem struct {
	blocks  []*mdBlock
	task    bool
	checked bool
	line    int
	// bare is an item whose marker line had no content at all. A bare item
	// holding a nested list is Lexical's wrapper shape; an item with any
	// body (even a dropped comment) is a real item of its own.
	bare bool
}

var (
	atxRe       = regexp.MustCompile(`^(#{1,6})(?:[ \t]+(.*?))?(?:[ \t]+#+)?[ \t]*$`)
	atxClosedRe = regexp.MustCompile(`^#+$`)
	ruleRe      = regexp.MustCompile(`^(?:(?:\*[ \t]*){3,}|(?:-[ \t]*){3,}|(?:_[ \t]*){3,})$`)
	setextRe    = regexp.MustCompile(`^(=+|-+)[ \t]*$`)
	fenceRe     = regexp.MustCompile("^(`{3,}|~{3,})(.*)$")
	bulletRe    = regexp.MustCompile(`^([-+*])([ \t]+|$)`)
	orderedRe   = regexp.MustCompile(`^(\d{1,9})([.)])([ \t]+|$)`)
	tableRowRe  = regexp.MustCompile(`^\|?\s*:?-{3,}:?\s*(\|\s*:?-{3,}:?\s*)+\|?\s*$`)
)

// splitLines turns source into numbered lines, normalising CRLF and CR.
func splitLines(src string) []srcLine {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\r", "\n")
	raw := strings.Split(src, "\n")
	out := make([]srcLine, len(raw))
	for i, l := range raw {
		out[i] = srcLine{text: l, no: i + 1}
	}
	return out
}

// indentOf measures a line's leading whitespace in columns (tabs to the next
// multiple of 4) and returns the rest of the line.
func indentOf(s string) (int, string) {
	col := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ':
			col++
		case '\t':
			col += 4 - col%4
		default:
			return col, s[i:]
		}
	}
	return col, ""
}

// stripIndent removes up to n columns of leading whitespace, expanding a tab
// that straddles the boundary into the spaces left over.
func stripIndent(s string, n int) string {
	col := 0
	for i := 0; i < len(s); i++ {
		if col >= n {
			return s[i:]
		}
		switch s[i] {
		case ' ':
			col++
		case '\t':
			w := 4 - col%4
			if col+w > n {
				return strings.Repeat(" ", col+w-n) + s[i+1:]
			}
			col += w
		default:
			return s[i:]
		}
	}
	return ""
}

// isBlank is CommonMark's blank line: spaces and tabs only. A no-break space
// is content.
func isBlank(s string) bool { return trimMD(s) == "" }

// trimMD trims what CommonMark trims: spaces and tabs (and newlines between
// joined lines), never Unicode spaces such as U+00A0.
func trimMD(s string) string { return strings.Trim(s, " \t\n") }

// listMarker recognises a list item start on a line with indent < 4 already
// stripped. width is the marker plus the spaces after it that belong to the
// item's content offset (CommonMark's W rule: 1–4 spaces; 5+ means 1).
type listMarker struct {
	ordered bool
	marker  byte
	start   int
	width   int
	rest    string
	empty   bool
}

func parseListMarker(s string) (listMarker, bool) {
	if m := bulletRe.FindStringSubmatch(s); m != nil {
		return finishMarker(listMarker{marker: m[1][0]}, len(m[1]), m[2], s[len(m[0]):]), true
	}
	if m := orderedRe.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		return finishMarker(listMarker{ordered: true, marker: m[2][0], start: n},
			len(m[1])+1, m[3], s[len(m[0]):]), true
	}
	return listMarker{}, false
}

func finishMarker(lm listMarker, markerLen int, spaces, rest string) listMarker {
	lm.rest = rest
	sp, _ := indentOf(spaces)
	switch {
	case isBlank(rest):
		lm.empty = true
		lm.width = markerLen + 1
		lm.rest = ""
	case sp >= 5:
		// Content starting with 5+ spaces is indented code inside the item;
		// the content offset is marker + 1 and the rest keeps its spaces.
		lm.width = markerLen + 1
		lm.rest = strings.Repeat(" ", sp-1) + rest
	default:
		lm.width = markerLen + sp
	}
	return lm
}

// blockParser parses one container's lines.
type blockParser struct {
	issues *[]Issue
	// refs are the link reference definitions (`[label]: url "title"`) of
	// the whole document, by normalised label; the first definition of a
	// label wins. The inline pass resolves `[text][label]`, `[label][]` and
	// `[label]` against them once every block has been read.
	refs map[string]string
	// depth is the current container (quote, list item) nesting; capped
	// records that maxContainerDepth was reached (reported once).
	depth  int
	capped bool
}

// maxContainerDepth bounds quote/list nesting. Every level re-reads its lines,
// so without a bound a few KB of `>` or nested `- ` cost seconds and hundreds
// of MB; real content nests a handful of levels (CommonMark implementations
// cap similarly).
const maxContainerDepth = 100

// nested parses a container's lines one level deeper. Beyond
// maxContainerDepth the lines are kept as one paragraph of text.
func (p *blockParser) nested(inner []srcLine, line int) []*mdBlock {
	if p.depth >= maxContainerDepth {
		if !p.capped {
			p.capped = true
			p.issue(IssueNestingCapped, line, fmt.Sprintf(
				"quotes/lists nested deeper than %d levels: the deeper lines were kept as paragraph text", maxContainerDepth))
		}
		var parts []string
		first := line
		for _, l := range inner {
			if t := strings.TrimLeft(l.text, " \t"); t != "" {
				if len(parts) == 0 {
					first = l.no
				}
				parts = append(parts, t)
			}
		}
		if len(parts) == 0 {
			return nil
		}
		return []*mdBlock{{kind: bParagraph, line: first, text: strings.Join(parts, "\n")}}
	}
	p.depth++
	defer func() { p.depth-- }()
	return p.parse(inner)
}

func (p *blockParser) issue(code string, line int, msg string) {
	*p.issues = append(*p.issues, Issue{Code: code, Line: line, Message: msg})
}

// startsBlock reports whether a line (indent < 4) begins a block that may
// interrupt a paragraph. It is the test for both paragraph termination and
// lazy continuation.
func startsBlock(line string) bool {
	ind, rest := indentOf(line)
	if ind >= 4 || rest == "" {
		return false
	}
	switch {
	case atxRe.MatchString(rest), ruleRe.MatchString(rest), fenceRe.MatchString(rest):
		return true
	case strings.HasPrefix(rest, ">"), strings.HasPrefix(rest, "<!--"):
		return true
	}
	if lm, ok := parseListMarker(rest); ok {
		// An empty item, or an ordered item not starting at 1, cannot
		// interrupt a paragraph (CommonMark 5.3).
		if lm.empty || (lm.ordered && lm.start != 1) {
			return false
		}
		return true
	}
	return false
}

// parse turns lines into blocks.
func (p *blockParser) parse(lines []srcLine) []*mdBlock {
	var out []*mdBlock
	i := 0
	for i < len(lines) {
		l := lines[i]
		if isBlank(l.text) {
			i++
			continue
		}
		ind, rest := indentOf(l.text)

		if ind >= 4 {
			// Indented code (it cannot interrupt a paragraph, and a paragraph
			// is never open here: paragraphs consume their own lines).
			j := i
			var code []string
			for j < len(lines) {
				if isBlank(lines[j].text) {
					code = append(code, "")
					j++
					continue
				}
				if li, _ := indentOf(lines[j].text); li < 4 {
					break
				}
				code = append(code, stripIndent(lines[j].text, 4))
				j++
			}
			for len(code) > 0 && code[len(code)-1] == "" {
				code = code[:len(code)-1]
			}
			out = append(out, &mdBlock{kind: bCode, line: l.no, text: strings.Join(code, "\n")})
			i = j
			continue
		}

		switch {
		case fenceRe.MatchString(rest):
			b, next := p.fenced(lines, i, ind)
			out = append(out, b)
			i = next
			continue
		case atxRe.MatchString(rest):
			m := atxRe.FindStringSubmatch(rest)
			content := trimMD(m[2])
			if atxClosedRe.MatchString(content) {
				content = ""
			}
			out = append(out, &mdBlock{kind: bHeading, line: l.no, level: len(m[1]), text: content})
			i++
			continue
		case ruleRe.MatchString(rest):
			out = append(out, &mdBlock{kind: bRule, line: l.no})
			i++
			continue
		case strings.HasPrefix(rest, ">"):
			b, next := p.quote(lines, i)
			out = append(out, b)
			i = next
			continue
		case strings.HasPrefix(rest, "<!--"):
			if b, next, ok := p.comment(lines, i); ok {
				if b != nil {
					out = append(out, b)
				}
				i = next
				continue
			}
		}
		if lm, ok := parseListMarker(rest); ok {
			b, next := p.list(lines, i, lm)
			out = append(out, b)
			i = next
			continue
		}

		b, next := p.paragraph(lines, i)
		if b != nil {
			out = append(out, b)
		}
		i = next
	}
	return out
}

// fenced reads a fenced code block starting at lines[i].
func (p *blockParser) fenced(lines []srcLine, i, ind int) (*mdBlock, int) {
	_, rest := indentOf(lines[i].text)
	m := fenceRe.FindStringSubmatch(rest)
	fence := m[1]
	var code []string
	j := i + 1
	for ; j < len(lines); j++ {
		li, lrest := indentOf(lines[j].text)
		if li < 4 && strings.HasPrefix(lrest, fence[:1]) {
			run := len(lrest) - len(strings.TrimLeft(lrest, fence[:1]))
			if run >= len(fence) && isBlank(lrest[run:]) {
				j++
				break
			}
		}
		code = append(code, stripIndent(lines[j].text, ind))
	}
	return &mdBlock{kind: bCode, line: lines[i].no, text: strings.Join(code, "\n")}, j
}

// quote collects a block quote: `>` lines with the marker (and one optional
// space) stripped, plus lazy paragraph-continuation lines.
func (p *blockParser) quote(lines []srcLine, i int) (*mdBlock, int) {
	var inner []srcLine
	j := i
	lastWasText := false
	for j < len(lines) {
		ind, rest := indentOf(lines[j].text)
		if ind < 4 && strings.HasPrefix(rest, ">") {
			rest = rest[1:]
			switch {
			case strings.HasPrefix(rest, " "):
				rest = rest[1:]
			case strings.HasPrefix(rest, "\t"):
				rest = stripIndent(rest, 1)
			}
			inner = append(inner, srcLine{text: rest, no: lines[j].no})
			lastWasText = !isBlank(rest) && !startsBlock(rest)
			j++
			continue
		}
		if lastWasText && !isBlank(lines[j].text) && !startsBlock(lines[j].text) {
			inner = append(inner, lines[j])
			j++
			continue
		}
		break
	}
	return &mdBlock{kind: bQuote, line: lines[i].no, children: p.nested(inner, lines[i].no)}, j
}

// comment reads an HTML comment block. A lone lexical placeholder becomes a
// bComment carrying it; any other comment is dropped with an issue; a comment
// followed by text on its closing line is NOT a comment block (ok=false) and
// is read as a paragraph, so an inline placeholder can start a paragraph.
func (p *blockParser) comment(lines []srcLine, i int) (*mdBlock, int, bool) {
	var sb strings.Builder
	for j := i; j < len(lines); j++ {
		if j > i {
			sb.WriteByte('\n')
		}
		sb.WriteString(lines[j].text)
		// On the opening line the terminator is searched after `<!`, so
		// CommonMark's degenerate comments `<!-->` and `<!--->` close too.
		from := 0
		if j == i {
			from = strings.Index(lines[j].text, "<!--") + 2
		}
		if k := strings.Index(lines[j].text[from:], "-->"); k >= 0 {
			k += from
			if !isBlank(lines[j].text[k+3:]) {
				return nil, 0, false
			}
			text := strings.TrimSpace(sb.String())
			if placeholderRe.MatchString(text) {
				return &mdBlock{kind: bComment, line: lines[i].no, text: text}, j + 1, true
			}
			if !isSilentComment(text) {
				p.issue(IssueCommentDropped, lines[i].no, "an HTML comment was removed; Lexical has no comment node")
			}
			return nil, j + 1, true
		}
	}
	// Unterminated: CommonMark runs the comment to the end of the container.
	p.issue(IssueCommentDropped, lines[i].no, "an unterminated HTML comment swallowed the rest of its container")
	return nil, len(lines), true
}

// isSilentComment is a comment dropped without an issue: `<!-- -->` (the
// conventional list separator, which ToMarkdown also writes as the body of
// an empty item) and `<!-- pay:… -->` (the field headers `pay lexical to-md`
// writes between fields). Dropping them is their whole purpose.
func isSilentComment(s string) bool {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "<!--") && strings.HasPrefix(strings.TrimSpace(t[4:]), "pay:") && strings.HasSuffix(t, "-->") {
		return true
	}
	return isEmptyComment(s)
}

// isEmptyComment is a comment with nothing in it.
func isEmptyComment(s string) bool {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "<!") || !strings.HasSuffix(s, "-->") {
		return false
	}
	if len(s) < 7 { // <!--> and <!--->
		return true
	}
	return strings.TrimSpace(s[4:len(s)-3]) == ""
}

// paragraph collects paragraph lines until a blank line or a block start, and
// turns a setext underline into a heading.
func (p *blockParser) paragraph(lines []srcLine, i int) (*mdBlock, int) {
	var parts []string
	j := i
	for j < len(lines) {
		t := lines[j].text
		if isBlank(t) {
			break
		}
		if j > i {
			ind, rest := indentOf(t)
			if ind < 4 && setextRe.MatchString(rest) {
				text := p.takeRefDefs(strings.Join(parts, "\n"))
				if trimMD(text) == "" {
					// Only definitions: the underline is not a heading's
					// (CommonMark reads it as a rule or a paragraph).
					return nil, j
				}
				level := 1
				if rest[0] == '-' {
					level = 2
				}
				return &mdBlock{kind: bHeading, line: lines[i].no, level: level,
					text: trimMD(text)}, j + 1
			}
			if startsBlock(t) {
				break
			}
		}
		_, rest := indentOf(t)
		parts = append(parts, rest)
		j++
	}
	text := p.takeRefDefs(strings.Join(parts, "\n"))
	text = strings.TrimRight(text, " \t")
	if trimMD(text) == "" {
		return nil, j
	}
	if len(parts) > 1 && tableRowRe.MatchString(strings.TrimSpace(parts[1])) {
		p.issue(IssueTableAsText, lines[i].no, "a pipe table was kept as paragraph text; write tables in the admin editor or as a block")
	}
	return &mdBlock{kind: bParagraph, line: lines[i].no, text: text}, j
}

// list collects consecutive items of one list.
func (p *blockParser) list(lines []srcLine, i int, first listMarker) (*mdBlock, int) {
	b := &mdBlock{kind: bList, line: lines[i].no, ordered: first.ordered, marker: first.marker, start: first.start}
	if !first.ordered {
		b.start = 1
	}
	j := i
	for j < len(lines) {
		ind, rest := indentOf(lines[j].text)
		if ind >= 4 {
			break
		}
		lm, ok := parseListMarker(rest)
		if !ok || lm.ordered != first.ordered || lm.marker != first.marker {
			break
		}
		// An ruled line like `- - -` or `* * *` is a thematic break, not an item.
		if ruleRe.MatchString(rest) {
			break
		}
		item, next := p.item(lines, j, ind, lm)
		b.items = append(b.items, item)
		j = next
		// Blank lines between items keep the list open; anything else that is
		// not a sibling marker ends it (checked at the top of the loop).
		k := j
		for k < len(lines) && isBlank(lines[k].text) {
			k++
		}
		if k < len(lines) {
			ki, krest := indentOf(lines[k].text)
			if nlm, ok := parseListMarker(krest); ki < 4 && ok && nlm.ordered == first.ordered && nlm.marker == first.marker && !ruleRe.MatchString(krest) {
				j = k
				continue
			}
		}
		break
	}
	return b, j
}

// item collects one list item's lines and parses them as a container.
func (p *blockParser) item(lines []srcLine, i, ind int, lm listMarker) (*mdItem, int) {
	w := ind + lm.width
	inner := []srcLine{{text: lm.rest, no: lines[i].no}}
	j := i + 1
	lastWasText := !lm.empty && !startsBlock(lm.rest)
	for j < len(lines) {
		t := lines[j].text
		if isBlank(t) {
			// An item that starts empty ends at its first blank line.
			if lm.empty && len(inner) == 1 {
				break
			}
			inner = append(inner, srcLine{text: "", no: lines[j].no})
			lastWasText = false
			j++
			continue
		}
		li, _ := indentOf(t)
		if li >= w {
			stripped := stripIndent(t, w)
			inner = append(inner, srcLine{text: stripped, no: lines[j].no})
			lastWasText = !startsBlock(stripped)
			j++
			continue
		}
		// A list marker that is not indented into this item is a sibling (or
		// a new list); it is never lazy paragraph continuation, even where it
		// could not interrupt a paragraph (`3. x` / `4. y`).
		if _, isMarker := parseListMarker(stripIndent(t, li)); isMarker {
			break
		}
		if lastWasText && !startsBlock(t) {
			inner = append(inner, lines[j])
			j++
			continue
		}
		break
	}
	// Trailing blank lines belong to whatever follows the item.
	for len(inner) > 1 && isBlank(inner[len(inner)-1].text) {
		inner = inner[:len(inner)-1]
		j--
	}
	it := &mdItem{line: lines[i].no, blocks: p.nested(inner, lines[i].no), bare: lm.empty}
	// GFM task item: the first paragraph opens with [ ] or [x].
	if len(it.blocks) > 0 && it.blocks[0].kind == bParagraph {
		t := it.blocks[0].text
		if len(t) >= 3 && t[0] == '[' && t[2] == ']' && (t[1] == ' ' || t[1] == 'x' || t[1] == 'X') &&
			(len(t) == 3 || t[3] == ' ' || t[3] == '\t' || t[3] == '\n') {
			it.task = true
			it.checked = t[1] != ' '
			it.blocks[0].text = strings.TrimLeft(t[3:], " \t")
		}
	}
	return it, j
}

// takeRefDefs removes the link reference definitions at the start of a
// paragraph's text, records them, and returns the rest. CommonMark only
// recognises a definition where a paragraph starts, and a paragraph of
// definitions only produces no block at all.
func (p *blockParser) takeRefDefs(text string) string {
	for strings.HasPrefix(text, "[") {
		label, dest, n, ok := parseRefDef(text)
		if !ok {
			break
		}
		if p.refs == nil {
			p.refs = map[string]string{}
		}
		if _, dup := p.refs[label]; !dup {
			p.refs[label] = dest
		}
		text = text[n:]
	}
	return text
}

// normalizeRefLabel is CommonMark's label matching: case-insensitive, inner
// whitespace collapsed, outer whitespace ignored.
func normalizeRefLabel(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// scanRefLabel reads `[label]` at src[0] and returns the raw label and the
// offset after `]`. A label holds no unescaped brackets, is at most 999
// characters long and is not blank.
func scanRefLabel(src string) (string, int, bool) {
	if src == "" || src[0] != '[' {
		return "", 0, false
	}
	for i := 1; i < len(src) && i <= 1000; i++ {
		switch src[i] {
		case '\\':
			i++
		case '[':
			return "", 0, false
		case ']':
			label := src[1:i]
			if strings.TrimSpace(label) == "" {
				return "", 0, false
			}
			return label, i + 1, true
		}
	}
	return "", 0, false
}

// parseRefDef reads one link reference definition at the start of text:
// `[label]: destination "optional title"`, where the destination may be in
// <angle brackets> and destination and title may each move to the next line.
// It returns the normalised label, the decoded destination and the number of
// bytes consumed (through the end of the definition's last line). The title
// is read and dropped: Lexical links have none.
func parseRefDef(text string) (string, string, int, bool) {
	label, i, ok := scanRefLabel(text)
	if !ok || i >= len(text) || text[i] != ':' {
		return "", "", 0, false
	}
	i = skipSpaceNL(text, i+1)
	dest, end, ok := scanLinkDest(text, i)
	if !ok || end == i {
		return "", "", 0, false
	}
	// With a title: whitespace, the title, then nothing but spaces to EOL.
	if j := skipSpaceNL(text, end); j > end && j < len(text) {
		if k, ok := scanLinkTitle(text, j); ok {
			if e, ok := restOfLineBlank(text, k); ok {
				return normalizeRefLabel(label), dest, e, true
			}
		}
	}
	// Without: the destination ends its line.
	if e, ok := restOfLineBlank(text, end); ok {
		return normalizeRefLabel(label), dest, e, true
	}
	return "", "", 0, false
}

// restOfLineBlank reports whether only spaces and tabs follow src[i] up to
// the end of the line, and returns the offset after that line's newline.
func restOfLineBlank(src string, i int) (int, bool) {
	for ; i < len(src); i++ {
		switch src[i] {
		case ' ', '\t':
		case '\n':
			return i + 1, true
		default:
			return 0, false
		}
	}
	return i, true
}
