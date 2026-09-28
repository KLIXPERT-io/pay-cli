package lexical

import (
	"html"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The inline half of the Markdown reader: CommonMark's inline algorithm
// (delimiter stack, process_emphasis with the rule of 3, bracket stack for
// links) over a doubly linked list of nodes, plus GFM `~~strikethrough~~`,
// a closed set of HTML format tags, `<br>`, lexical placeholders, and link
// attributes `{target=_blank}`.

type inlKind int

const (
	inText inlKind = iota
	inCode
	inBreak
	inSoft
	inLink
	inPlaceholder
	inUpload
	inFmt
	inHTMLOpen
)

type inl struct {
	kind   inlKind
	text   string
	fmt    int
	tag    string
	target linkTarget
	node   Node
	kids   *inlList
	seq    int

	prev, next *inl
}

type inlList struct{ head, tail *inl }

func (l *inlList) push(n *inl) {
	n.prev, n.next = l.tail, nil
	if l.tail != nil {
		l.tail.next = n
	} else {
		l.head = n
	}
	l.tail = n
}

func (l *inlList) remove(n *inl) {
	if n.prev != nil {
		n.prev.next = n.next
	} else {
		l.head = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else {
		l.tail = n.prev
	}
	n.prev, n.next = nil, nil
}

// insertAfter puts n right after at.
func (l *inlList) insertAfter(at, n *inl) {
	n.prev, n.next = at, at.next
	if at.next != nil {
		at.next.prev = n
	} else {
		l.tail = n
	}
	at.next = n
}

// cut detaches the nodes strictly between a and b (b may be nil: to the end)
// and returns them as a new list.
func (l *inlList) cut(a, b *inl) *inlList {
	out := &inlList{}
	first := a.next
	if first == b {
		return out
	}
	last := l.tail
	if b != nil {
		last = b.prev
	}
	a.next = b
	if b != nil {
		b.prev = a
	} else {
		l.tail = a
	}
	first.prev, last.next = nil, nil
	out.head, out.tail = first, last
	return out
}

type delim struct {
	node     *inl
	ch       byte
	num      int
	orig     int
	canOpen  bool
	canClose bool

	prev, next *delim
}

type bracket struct {
	node      *inl
	image     bool
	active    bool
	srcPos    int // source offset just after `[` / `![`
	prevDelim *delim
	prev      *bracket
}

type htmlOpen struct {
	node      *inl
	fmt       int
	prevDelim *delim
	prev      *htmlOpen
}

type inlineParser struct {
	src      string
	pos      int
	list     *inlList
	delims   *delim
	brackets *bracket
	htmls    *htmlOpen
	seq      int
	line     int
	issues   *[]Issue
	err      error
	// refs are the document's link reference definitions (blockParser.refs).
	refs map[string]string
}

// htmlFormatTags are the tags the dialect reads as formatting. <u>, <sub>,
// <sup> and <mark> are the ONLY spelling of underline, subscript, superscript
// and highlight; the rest are accepted because agents and converters emit
// them.
var htmlFormatTags = map[string]int{
	"u": FormatUnderline, "ins": FormatUnderline,
	"sub": FormatSubscript, "sup": FormatSuperscript, "mark": FormatHighlight,
	"b": FormatBold, "strong": FormatBold,
	"i": FormatItalic, "em": FormatItalic,
	"s": FormatStrikethrough, "del": FormatStrikethrough, "strike": FormatStrikethrough,
	"code": FormatCode,
}

var (
	simpleTagRe  = regexp.MustCompile(`^<(/?)([A-Za-z][A-Za-z0-9]*)\s*(/?)>`)
	anyTagRe     = regexp.MustCompile(`^</?[A-Za-z][A-Za-z0-9-]*(?:\s+[^<>]*)?/?>`)
	uriAutoRe    = regexp.MustCompile(`^<([A-Za-z][A-Za-z0-9+.-]{1,31}:[^\x00-\x20<>]*)>`)
	emailAutoRe  = regexp.MustCompile("^<([a-zA-Z0-9.!#$%&'*+/=?^_`{|}~-]+@[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*)>")
	entityRe     = regexp.MustCompile(`^&(?:#[0-9]{1,7}|#[xX][0-9a-fA-F]{1,6}|[A-Za-z][A-Za-z0-9]{1,31});`)
	uploadLabel  = regexp.MustCompile(`^([^:\]\s]+):([^\]\s]+)$`)
	internalDest = regexp.MustCompile(`^pay:(?:([A-Za-z0-9_-]+)/([^/\s]+))?$`)
)

// parseInline parses one paragraph's (or heading's) inline source.
func parseInline(src string, line int, issues *[]Issue, refs map[string]string) (*inlList, error) {
	p := &inlineParser{src: src, list: &inlList{}, line: line, issues: issues, refs: refs}
	for p.pos < len(p.src) && p.err == nil {
		p.step()
	}
	if p.err != nil {
		return nil, p.err
	}
	p.processEmphasis(nil)
	// Unmatched HTML open tags stay literal.
	for h := p.htmls; h != nil; h = h.prev {
		h.node.kind = inText
		p.issue(IssueHTMLAsText, "an unclosed <"+h.node.tag+"> was kept as text")
	}
	return p.list, nil
}

func (p *inlineParser) issue(code, msg string) {
	*p.issues = append(*p.issues, Issue{Code: code, Line: p.line, Message: msg})
}

func (p *inlineParser) add(n *inl) *inl {
	p.seq++
	n.seq = p.seq
	p.list.push(n)
	return n
}

func (p *inlineParser) addText(s string) {
	if s == "" {
		return
	}
	if t := p.list.tail; t != nil && t.kind == inText && !p.isDelimNode(t) && !p.isBracketNode(t) {
		t.text += s
		return
	}
	p.add(&inl{kind: inText, text: s})
}

func (p *inlineParser) isDelimNode(n *inl) bool {
	for d := p.delims; d != nil; d = d.prev {
		if d.node == n {
			return true
		}
	}
	return false
}

func (p *inlineParser) isBracketNode(n *inl) bool {
	for b := p.brackets; b != nil; b = b.prev {
		if b.node == n {
			return true
		}
	}
	return false
}

func (p *inlineParser) step() {
	c := p.src[p.pos]
	switch c {
	case '\n':
		p.newline()
	case '\\':
		p.backslash()
	case '`':
		p.codeSpan()
	case '*', '_':
		p.delimRun(c)
	case '~':
		p.tildes()
	case '[':
		p.openBracket(false)
	case '!':
		if p.pos+1 < len(p.src) && p.src[p.pos+1] == '[' {
			p.openBracket(true)
			return
		}
		p.addText("!")
		p.pos++
	case ']':
		p.closeBracket()
	case '<':
		p.angle()
	case '&':
		p.entity()
	default:
		end := p.pos + 1
		for end < len(p.src) && !strings.ContainsRune("\n\\`*_~[]!<&", rune(p.src[end])) {
			end++
		}
		p.addText(p.src[p.pos:end])
		p.pos = end
	}
}

// newline is a soft break, or a hard break when the line ended in two or
// more spaces. Leading spaces of the next line are not content.
func (p *inlineParser) newline() {
	hard := false
	if t := p.list.tail; t != nil && t.kind == inText && !p.isDelimNode(t) {
		trimmed := strings.TrimRight(t.text, " ")
		hard = len(t.text)-len(trimmed) >= 2
		t.text = trimmed
		if t.text == "" {
			p.list.remove(t)
		}
	}
	if hard {
		p.add(&inl{kind: inBreak})
	} else {
		p.add(&inl{kind: inSoft})
	}
	p.pos++
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		p.pos++
	}
}

func isASCIIPunct(c byte) bool {
	return strings.IndexByte("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", c) >= 0
}

func (p *inlineParser) backslash() {
	if p.pos+1 < len(p.src) {
		n := p.src[p.pos+1]
		if n == '\n' {
			p.add(&inl{kind: inBreak})
			p.pos += 2
			for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
				p.pos++
			}
			return
		}
		if isASCIIPunct(n) {
			p.addText(string(n))
			p.pos += 2
			return
		}
	}
	p.addText("\\")
	p.pos++
}

func (p *inlineParser) codeSpan() {
	start := p.pos
	n := 0
	for p.pos < len(p.src) && p.src[p.pos] == '`' {
		n++
		p.pos++
	}
	// Find a closing run of exactly n backticks.
	for i := p.pos; i < len(p.src); {
		if p.src[i] != '`' {
			i++
			continue
		}
		j := i
		for j < len(p.src) && p.src[j] == '`' {
			j++
		}
		if j-i == n {
			content := strings.ReplaceAll(p.src[p.pos:i], "\n", " ")
			if len(content) >= 2 && content[0] == ' ' && content[len(content)-1] == ' ' && strings.Trim(content, " ") != "" {
				content = content[1 : len(content)-1]
			}
			p.add(&inl{kind: inCode, text: content})
			p.pos = j
			return
		}
		i = j
	}
	p.addText(p.src[start:p.pos])
}

// flanking computes CommonMark's left/right-flanking for the run src[s:e].
func (p *inlineParser) flanking(s, e int) (left, right, prevPunct, nextPunct bool) {
	prev, next := ' ', ' '
	if s > 0 {
		prev, _ = utf8.DecodeLastRuneInString(p.src[:s])
	}
	if e < len(p.src) {
		next, _ = utf8.DecodeRuneInString(p.src[e:])
	}
	prevWS, nextWS := unicode.IsSpace(prev), unicode.IsSpace(next)
	prevPunct, nextPunct = isPunct(prev), isPunct(next)
	left = !nextWS && (!nextPunct || prevWS || prevPunct)
	right = !prevWS && (!prevPunct || nextWS || nextPunct)
	return left, right, prevPunct, nextPunct
}

func isPunct(r rune) bool { return unicode.IsPunct(r) || unicode.IsSymbol(r) }

func (p *inlineParser) delimRun(c byte) {
	s := p.pos
	for p.pos < len(p.src) && p.src[p.pos] == c {
		p.pos++
	}
	left, right, prevPunct, nextPunct := p.flanking(s, p.pos)
	var canOpen, canClose bool
	if c == '*' {
		canOpen, canClose = left, right
	} else {
		canOpen = left && (!right || prevPunct)
		canClose = right && (!left || nextPunct)
	}
	p.pushDelim(c, p.src[s:p.pos], canOpen, canClose)
}

// tildes: only a run of exactly two is a strikethrough delimiter. A single
// `~` ("~5 min") stays text, which is safer for prose than GFM's rule.
func (p *inlineParser) tildes() {
	s := p.pos
	for p.pos < len(p.src) && p.src[p.pos] == '~' {
		p.pos++
	}
	if p.pos-s != 2 {
		p.addText(p.src[s:p.pos])
		return
	}
	left, right, _, _ := p.flanking(s, p.pos)
	p.pushDelim('~', "~~", left, right)
}

func (p *inlineParser) pushDelim(c byte, run string, canOpen, canClose bool) {
	n := p.add(&inl{kind: inText, text: run})
	if !canOpen && !canClose {
		return
	}
	d := &delim{node: n, ch: c, num: len(run), orig: len(run), canOpen: canOpen, canClose: canClose, prev: p.delims}
	if p.delims != nil {
		p.delims.next = d
	}
	p.delims = d
}

func (p *inlineParser) removeDelim(d *delim) {
	if d.prev != nil {
		d.prev.next = d.next
	}
	if d.next != nil {
		d.next.prev = d.prev
	} else {
		p.delims = d.prev
	}
}

// processEmphasis is CommonMark's process_emphasis over the delimiters above
// bottom (nil = all of them).
func (p *inlineParser) processEmphasis(bottom *delim) {
	openersBottom := map[[3]int]*delim{}
	// Find the first delimiter above bottom.
	var closer *delim
	for d := p.delims; d != nil && d != bottom; d = d.prev {
		closer = d
	}
	for closer != nil {
		if !closer.canClose {
			closer = closer.next
			continue
		}
		key := [3]int{int(closer.ch), boolInt(closer.canOpen), closer.orig % 3}
		obot, seen := openersBottom[key]
		opener := closer.prev
		found := false
		for opener != nil && opener != bottom && (!seen || opener != obot) {
			odd := (closer.canOpen || opener.canClose) && closer.orig%3 != 0 && (opener.orig+closer.orig)%3 == 0
			if opener.ch == closer.ch && opener.canOpen && (!odd || closer.ch == '~') {
				found = true
				break
			}
			opener = opener.prev
		}
		old := closer
		if !found {
			closer = closer.next
			openersBottom[key] = old.prev
			if !old.canOpen {
				p.removeDelim(old)
			}
			continue
		}
		use := 1
		format := FormatItalic
		switch {
		case closer.ch == '~':
			use, format = 2, FormatStrikethrough
		case closer.num >= 2 && opener.num >= 2:
			use, format = 2, FormatBold
		}
		opener.num -= use
		closer.num -= use
		opener.node.text = opener.node.text[:len(opener.node.text)-use]
		closer.node.text = closer.node.text[:len(closer.node.text)-use]
		// Wrap everything between the two delimiter nodes.
		kids := p.list.cut(opener.node, closer.node)
		p.seq++
		cont := &inl{kind: inFmt, fmt: format, kids: kids, seq: opener.node.seq}
		p.list.insertAfter(opener.node, cont)
		// Delimiters strictly between opener and closer are gone.
		for d := closer.prev; d != nil && d != opener; {
			prev := d.prev
			p.removeDelim(d)
			d = prev
		}
		if opener.num == 0 {
			p.list.remove(opener.node)
			p.removeDelim(opener)
		}
		if closer.num == 0 {
			next := closer.next
			p.list.remove(closer.node)
			p.removeDelim(closer)
			closer = next
		}
	}
	// Remove every delimiter above bottom; their text stays literal.
	for p.delims != nil && p.delims != bottom {
		p.removeDelim(p.delims)
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (p *inlineParser) openBracket(image bool) {
	text := "["
	if image {
		text = "!["
	}
	n := p.add(&inl{kind: inText, text: text})
	p.pos += len(text)
	p.brackets = &bracket{node: n, image: image, active: true, srcPos: p.pos, prevDelim: p.delims, prev: p.brackets}
}

func (p *inlineParser) closeBracket() {
	b := p.brackets
	if b == nil {
		p.addText("]")
		p.pos++
		return
	}
	if !b.active {
		p.brackets = b.prev
		p.addText("]")
		p.pos++
		return
	}
	labelEnd := p.pos
	after := p.pos + 1
	dest, destEnd, ok := parseLinkDest(p.src, after)
	if !ok && !b.image {
		dest, destEnd, ok = p.refLink(p.src[b.srcPos:labelEnd], after, b.srcPos-1)
	}
	if !ok && b.image {
		if end, isRef := p.refImage(p.src[b.srcPos:labelEnd], after); isRef {
			// A reference image (`![x][r]`, `![x][]`, `![x]` with a
			// definition) is an image like the inline form: kept verbatim as
			// text and reported, never turned into a link to the image URL.
			p.issue(IssueImageAsText, "an image was kept as text; only the upload form ![collection:id]() becomes a node")
			p.brackets = b.prev
			p.addText("]")
			p.pos++
			if end > p.pos {
				p.addText(p.src[p.pos:end])
				p.pos = end
			}
			return
		}
	}
	if !ok {
		p.brackets = b.prev
		p.addText("]")
		p.pos++
		return
	}
	if b.image {
		label := p.src[b.srcPos:labelEnd]
		if m := uploadLabel.FindStringSubmatch(label); m != nil && dest == "" {
			// Payload's own upload Markdown form: ![media:40]().
			p.list.cut(b.node, nil)
			b.node.kind = inUpload
			b.node.text = ""
			b.node.node = Node{
				"fields": nil, "format": "", "relationTo": m[1], "value": idValue(m[2]),
				"type": "upload", "version": 3,
			}
			p.dropDelimsAfter(b.prevDelim)
			p.brackets = b.prev
			p.pos = destEnd
			return
		}
		p.issue(IssueImageAsText, "an image was kept as text; only the upload form ![collection:id]() becomes a node")
		p.brackets = b.prev
		p.addText("]")
		p.pos++
		return
	}

	target, terr := resolveDest(dest)
	if terr != "" {
		p.brackets = b.prev
		p.addText("]")
		p.pos++
		return
	}
	p.pos = destEnd
	if attrs, end, ok := parseAttrs(p.src, p.pos); ok {
		applyAttrs(&target, attrs)
		p.pos = end
	}
	p.checkLinkURL(target)
	p.processEmphasis(b.prevDelim)
	kids := p.list.cut(b.node, nil)
	b.node.kind = inLink
	b.node.text = ""
	b.node.target = target
	b.node.kids = kids
	p.brackets = b.prev
	// Links cannot contain links: deactivate every earlier `[`.
	for q := p.brackets; q != nil; q = q.prev {
		if !q.image {
			q.active = false
		}
	}
}

// refLink resolves a reference link whose text is text and whose `]` is
// just before src[after]: full `[text][label]`, collapsed `[text][]` and
// shortcut `[text]` (CommonMark 6.3). It returns the destination and the
// offset after the construct. A full or collapsed reference to a label no
// definition declares stays text and is reported — unless the `[` (at open)
// follows a letter or digit: `m[i][j]` is an index, not a link.
func (p *inlineParser) refLink(text string, after, open int) (string, int, bool) {
	report := func(msg string) {
		if open > 0 {
			if r, _ := utf8.DecodeLastRuneInString(p.src[:open]); unicode.IsLetter(r) || unicode.IsDigit(r) {
				return
			}
		}
		p.issue(IssueLinkRefUndefined, msg)
	}
	// A label is at most 999 characters (CommonMark 4.7); the text is only
	// a label in the collapsed and shortcut forms.
	textIsLabel := strings.TrimSpace(text) != "" && len(text) <= 999
	if after < len(p.src) && p.src[after] == '[' {
		if after+1 < len(p.src) && p.src[after+1] == ']' {
			if !textIsLabel {
				return "", 0, false
			}
			if dest, ok := p.refs[normalizeRefLabel(text)]; ok {
				return dest, after + 2, true
			}
			report("the reference link [" + text + "][] has no [label]: URL definition and was kept as text")
			return "", 0, false
		}
		if label, end, ok := scanRefLabel(p.src[after:]); ok {
			if dest, ok := p.refs[normalizeRefLabel(label)]; ok {
				return dest, after + end, true
			}
			report("the reference link [" + text + "][" + label + "] has no [" + label + "]: URL definition and was kept as text")
			return "", 0, false
		}
	}
	if !textIsLabel || len(p.refs) == 0 {
		return "", 0, false
	}
	if dest, ok := p.refs[normalizeRefLabel(text)]; ok {
		return dest, after, true
	}
	return "", 0, false
}

// refImage reports whether `![text]` whose `]` is just before src[after]
// is a reference image whose label a definition declares — full
// `![text][label]`, collapsed `![text][]` or shortcut `![text]` — and the
// offset after it.
func (p *inlineParser) refImage(text string, after int) (int, bool) {
	has := func(label string) bool {
		_, ok := p.refs[normalizeRefLabel(label)]
		return strings.TrimSpace(label) != "" && ok
	}
	if after < len(p.src) && p.src[after] == '[' {
		if after+1 < len(p.src) && p.src[after+1] == ']' {
			return after + 2, has(text)
		}
		if label, end, ok := scanRefLabel(p.src[after:]); ok {
			return after + end, has(label)
		}
	}
	return after, has(text)
}

// checkLinkURL reports custom link URLs that Payload rejects or that will
// not point where the Markdown source meant.
func (p *inlineParser) checkLinkURL(t linkTarget) {
	if t.internal || t.autolink {
		return
	}
	switch {
	case t.url == "" || strings.Contains(t.url, " "):
		p.issue(IssueLinkURLInvalid, "the link URL "+strconv.Quote(t.url)+
			" is empty or contains a space; Payload's link validation rejects it when the document is published or saved without --draft")
	case isRelativeURL(t.url):
		p.issue(IssueLinkURLRelative, "the link URL "+strconv.Quote(t.url)+
			" is relative to the Markdown file; on the site it resolves against the page's own URL")
	}
}

// isRelativeURL: not a site path, an anchor, a query, or a URL with a scheme.
func isRelativeURL(u string) bool {
	switch u[0] {
	case '/', '#', '?':
		return false
	}
	if i := strings.IndexAny(u, ":/?#"); i > 0 && u[i] == ':' && schemeRe.MatchString(u[:i]) {
		return false
	}
	return true
}

var schemeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*$`)

// dropDelimsAfter removes the delimiters above bottom without processing
// them (their nodes were cut away with an upload's alt text).
func (p *inlineParser) dropDelimsAfter(bottom *delim) {
	for p.delims != nil && p.delims != bottom {
		p.removeDelim(p.delims)
	}
}

// idValue turns a document id from Markdown into JSON: all digits → number
// (Postgres/SQLite serial ids), anything else → string (Mongo ObjectIDs,
// UUIDs, custom text ids).
func idValue(s string) any {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && strconv.FormatInt(n, 10) == s {
		return n
	}
	return s
}

// RefKey marks a document reference in a link's doc value — the "$ref" of
// PayCLI's write bodies (internal/manifest); lexical only produces and
// renders it.
const RefKey = "$ref"

// resolveDest turns a link destination into a target. `pay:COLLECTION/ID` is
// an internal (document) link, `pay:COLLECTION/FIELD=VALUE` one to the
// document whose FIELD is VALUE (a {"$ref"} a write resolves), `pay:` alone
// an internal link with no document.
func resolveDest(dest string) (linkTarget, string) {
	if strings.HasPrefix(dest, "pay:") {
		m := internalDest.FindStringSubmatch(dest)
		if m == nil {
			return linkTarget{}, "bad internal link"
		}
		t := linkTarget{internal: true}
		if m[1] != "" {
			t.relation = m[1]
			t.docID = idValue(m[2])
			if field, value, ok := strings.Cut(m[2], "="); ok {
				if field == "" || value == "" {
					return linkTarget{}, "bad internal link"
				}
				// pay:pages/slug=about names the document by a field: a
				// {"$ref"} the write verbs (create, update, upsert, sync …)
				// replace with its id, so Markdown needs no id lookups.
				t.docID = map[string]any{RefKey: m[1], field: value}
			}
		}
		return t, ""
	}
	return linkTarget{url: dest}, ""
}

// parseLinkDest reads `(dest "title")` starting at src[i]; it returns the
// decoded destination and the offset after `)`.
func parseLinkDest(src string, i int) (string, int, bool) {
	if i >= len(src) || src[i] != '(' {
		return "", 0, false
	}
	i++
	i = skipSpaceNL(src, i)
	dest, i, ok := scanLinkDest(src, i)
	if !ok {
		return "", 0, false
	}
	// Optional title: Lexical links have none, so it is parsed and dropped.
	if j := skipSpaceNL(src, i); j > i {
		if k, ok := scanLinkTitle(src, j); ok {
			i = k
		} else if j < len(src) && (src[j] == '"' || src[j] == '\'' || src[j] == '(') {
			return "", 0, false
		}
	}
	i = skipSpaceNL(src, i)
	if i >= len(src) || src[i] != ')' {
		return "", 0, false
	}
	return dest, i + 1, true
}

// scanLinkDest reads a link destination at src[i]: `<…>` (no line break, no
// `<`), or a run of non-space characters with balanced parentheses. It
// decodes backslash escapes and entities and returns the offset after it. An
// empty plain destination is allowed (end == i); `(` … `)` links use it.
func scanLinkDest(src string, i int) (string, int, bool) {
	if i < len(src) && src[i] == '<' {
		j := i + 1
		var sb strings.Builder
		for ; j < len(src); j++ {
			c := src[j]
			if c == '\n' || c == '<' {
				return "", 0, false
			}
			if c == '\\' && j+1 < len(src) && isASCIIPunct(src[j+1]) {
				sb.WriteByte(src[j+1])
				j++
				continue
			}
			if c == '&' {
				if dec, n := entityAt(src[j:]); n > 0 {
					sb.WriteString(dec)
					j += n - 1
					continue
				}
			}
			if c == '>' {
				break
			}
			sb.WriteByte(c)
		}
		if j >= len(src) {
			return "", 0, false
		}
		return sb.String(), j + 1, true
	}
	depth := 0
	var sb strings.Builder
	j := i
	for ; j < len(src); j++ {
		c := src[j]
		if c <= ' ' {
			break
		}
		if c == '\\' && j+1 < len(src) && isASCIIPunct(src[j+1]) {
			sb.WriteByte(src[j+1])
			j++
			continue
		}
		if c == '&' {
			if dec, n := entityAt(src[j:]); n > 0 {
				sb.WriteString(dec)
				j += n - 1
				continue
			}
		}
		if c == '(' {
			depth++
		} else if c == ')' {
			if depth == 0 {
				break
			}
			depth--
		}
		sb.WriteByte(c)
	}
	if depth != 0 {
		return "", 0, false
	}
	return sb.String(), j, true
}

// scanLinkTitle reads a link title ("…", '…' or (…)) at src[j] and returns
// the offset after its closing character.
func scanLinkTitle(src string, j int) (int, bool) {
	if j >= len(src) || (src[j] != '"' && src[j] != '\'' && src[j] != '(') {
		return 0, false
	}
	closeCh := src[j]
	if closeCh == '(' {
		closeCh = ')'
	}
	k := j + 1
	for ; k < len(src) && src[k] != closeCh; k++ {
		if src[k] == '\\' {
			k++
		}
	}
	if k >= len(src) {
		return 0, false
	}
	return k + 1, true
}

func skipSpaceNL(src string, i int) int {
	nl := false
	for i < len(src) {
		switch src[i] {
		case ' ', '\t':
			i++
		case '\n':
			if nl {
				return i
			}
			nl = true
			i++
		default:
			return i
		}
	}
	return i
}

// linkAttr is one `key=value` (or bare `key`) of a `{…}` attribute block.
type linkAttr struct {
	key   string
	value any
	bare  bool
}

// parseAttrs reads a pandoc-style attribute block `{target=_blank rel=x}`
// directly after a link. Values: "JSON string", 'raw JSON', or a bare token
// (a JSON literal when it parses as one, else a string).
func parseAttrs(src string, i int) ([]linkAttr, int, bool) {
	if i >= len(src) || src[i] != '{' {
		return nil, 0, false
	}
	var out []linkAttr
	j := i + 1
	for {
		for j < len(src) && (src[j] == ' ' || src[j] == '\t') {
			j++
		}
		if j >= len(src) {
			return nil, 0, false
		}
		if src[j] == '}' {
			return out, j + 1, len(out) > 0
		}
		k := j
		for k < len(src) && (isAttrKeyChar(src[k])) {
			k++
		}
		if k == j {
			return nil, 0, false
		}
		key := src[j:k]
		if k >= len(src) || src[k] != '=' {
			out = append(out, linkAttr{key: key, bare: true})
			j = k
			continue
		}
		k++
		if k >= len(src) {
			return nil, 0, false
		}
		switch src[k] {
		case '"':
			end := k + 1
			for end < len(src) && src[end] != '"' {
				if src[end] == '\\' {
					end++
				}
				end++
			}
			if end >= len(src) {
				return nil, 0, false
			}
			s, err := strconv.Unquote(src[k : end+1])
			if err != nil {
				return nil, 0, false
			}
			out = append(out, linkAttr{key: key, value: s})
			j = end + 1
		case '\'':
			end := strings.IndexByte(src[k+1:], '\'')
			if end < 0 {
				return nil, 0, false
			}
			v, ok := decodeJSONValue(src[k+1 : k+1+end])
			if !ok {
				return nil, 0, false
			}
			out = append(out, linkAttr{key: key, value: v})
			j = k + 1 + end + 1
		default:
			end := k
			for end < len(src) && src[end] != ' ' && src[end] != '\t' && src[end] != '}' && src[end] != '\n' {
				end++
			}
			tok := src[k:end]
			if v, ok := decodeJSONValue(tok); ok {
				out = append(out, linkAttr{key: key, value: v})
			} else {
				out = append(out, linkAttr{key: key, value: tok})
			}
			j = end
		}
	}
}

func isAttrKeyChar(c byte) bool {
	return c == '_' || c == '-' || c == '.' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// applyAttrs maps attributes onto a link: target=_blank is newTab, `autolink`
// makes an autolink node, anything else is a further link field.
func applyAttrs(t *linkTarget, attrs []linkAttr) {
	for _, a := range attrs {
		switch {
		case a.bare && a.key == "autolink":
			t.autolink = true
		case a.key == "target":
			s, _ := a.value.(string)
			t.newTab = s == "_blank"
		case a.bare:
			if t.extra == nil {
				t.extra = map[string]any{}
			}
			t.extra[a.key] = true
		default:
			if t.extra == nil {
				t.extra = map[string]any{}
			}
			t.extra[a.key] = a.value
		}
	}
}

// angle handles `<`: autolinks, comments/placeholders, format tags, <br>.
func (p *inlineParser) angle() {
	rest := p.src[p.pos:]
	if strings.HasPrefix(rest, "<!--") {
		if end := strings.Index(rest[2:], "-->"); end >= 0 {
			full := rest[:2+end+3]
			node, isPH, err := decodePlaceholder(full)
			switch {
			case err != nil:
				p.err = err
			case isPH:
				p.add(&inl{kind: inPlaceholder, node: node})
			case !isSilentComment(full):
				p.issue(IssueCommentDropped, "an inline HTML comment was removed")
			}
			p.pos += len(full)
			return
		}
	}
	if m := uriAutoRe.FindStringSubmatch(rest); m != nil {
		u := decodeEntities(m[1])
		p.add(&inl{kind: inLink, target: linkTarget{url: u, autolink: true},
			kids: textList(u)})
		p.pos += len(m[0])
		return
	}
	if m := emailAutoRe.FindStringSubmatch(rest); m != nil {
		p.add(&inl{kind: inLink, target: linkTarget{url: "mailto:" + m[1], autolink: true},
			kids: textList(m[1])})
		p.pos += len(m[0])
		return
	}
	if m := simpleTagRe.FindStringSubmatch(rest); m != nil {
		name := strings.ToLower(m[2])
		if name == "br" && m[1] == "" {
			p.add(&inl{kind: inBreak})
			p.pos += len(m[0])
			return
		}
		if f, ok := htmlFormatTags[name]; ok && m[3] == "" {
			p.pos += len(m[0])
			if m[1] == "" {
				n := p.add(&inl{kind: inHTMLOpen, text: m[0], tag: name, fmt: f})
				p.htmls = &htmlOpen{node: n, fmt: f, prevDelim: p.delims, prev: p.htmls}
				return
			}
			p.closeHTML(f, m[0])
			return
		}
	}
	if m := anyTagRe.FindString(rest); m != "" {
		p.issue(IssueHTMLAsText, "the HTML tag "+strconv.Quote(m)+" was kept as text; Lexical has no HTML node")
		p.addText(m)
		p.pos += len(m)
		return
	}
	p.addText("<")
	p.pos++
}

func textList(s string) *inlList {
	l := &inlList{}
	l.push(&inl{kind: inText, text: s})
	return l
}

// closeHTML matches a closing format tag with the nearest open one of the
// same format and wraps what lies between.
func (p *inlineParser) closeHTML(f int, raw string) {
	var h *htmlOpen
	for q := p.htmls; q != nil; q = q.prev {
		if q.fmt == f {
			h = q
			break
		}
	}
	if h == nil {
		p.issue(IssueHTMLAsText, "the closing tag "+strconv.Quote(raw)+" has no opening tag and was kept as text")
		p.addText(raw)
		return
	}
	p.processEmphasis(h.prevDelim)
	kids := p.list.cut(h.node, nil)
	h.node.kind = inFmt
	h.node.fmt = f
	h.node.text = ""
	h.node.kids = kids
	// Open tags nested inside are unmatched now: they stay literal.
	for q := p.htmls; q != h; q = q.prev {
		q.node.kind = inText
		p.issue(IssueHTMLAsText, "an unclosed <"+q.node.tag+"> was kept as text")
	}
	p.htmls = h.prev
	// A `[` inside the wrapped range can no longer become a link.
	for p.brackets != nil && p.brackets.node.seq > h.node.seq {
		p.brackets = p.brackets.prev
	}
}

func (p *inlineParser) entity() {
	if m := entityRe.FindString(p.src[p.pos:]); m != "" {
		if dec := html.UnescapeString(m); dec != m {
			p.addText(dec)
			p.pos += len(m)
			return
		}
	}
	p.addText("&")
	p.pos++
}

// entityAt decodes an entity reference at the start of s; n is its length,
// 0 when s does not start with one. Escapes and entities are resolved in ONE
// pass (CommonMark): `\&amp;` is a literal "&amp;", not "&".
func entityAt(s string) (string, int) {
	m := entityRe.FindString(s)
	if m == "" {
		return "", 0
	}
	if dec := html.UnescapeString(m); dec != m {
		return dec, len(m)
	}
	return "", 0
}

// decodeEntities resolves entity references in an autolink.
func decodeEntities(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '&' {
			if m := entityRe.FindString(s[i:]); m != "" {
				if dec := html.UnescapeString(m); dec != m {
					sb.WriteString(dec)
					i += len(m)
					continue
				}
			}
		}
		sb.WriteByte(s[i])
		i++
	}
	return sb.String()
}
