package lexical

import (
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrNotEditorState is returned by ToMarkdown for a value that is not a
// Lexical editor state.
var ErrNotEditorState = errors.New("not a Lexical editor state (expected an object with root.type \"root\" and root.children)")

// ToMarkdown renders a Lexical editor state as Markdown in this package's
// dialect. Every node survives FromMarkdown(ToMarkdown(x)): what Markdown can
// spell is spelled, and what it cannot is written as a lossless
// `<!-- lexical:TYPE {…} -->` placeholder. The only losses are presentation
// attributes Markdown has no syntax for (alignment, indent, inline CSS), which
// ToMarkdownIssues reports.
func ToMarkdown(state any) (string, error) {
	md, _, err := ToMarkdownIssues(state)
	return md, err
}

// ToMarkdownIssues is ToMarkdown plus the findings: lossy attributes,
// placeholders written, and shapes normalised to Lexical's native form.
func ToMarkdownIssues(state any) (string, []Issue, error) {
	root, ok := rootOf(state)
	if !ok {
		return "", nil, ErrNotEditorState
	}
	w := &mdWriter{}
	if m, ok := state.(map[string]any); ok {
		w.stateKeys(m)
	}
	parts := w.blocks(childrenOf(root), "root")
	if w.err != nil {
		return "", w.issues, w.err
	}
	if len(parts) == 0 {
		return "", w.issues, nil
	}
	return strings.Join(parts, "\n\n") + "\n", w.issues, nil
}

type mdWriter struct {
	issues []Issue
	err    error
}

func (w *mdWriter) issue(code, path, msg string) {
	w.issues = append(w.issues, Issue{Code: code, Path: path, Message: msg})
}

// stateKeys reports keys stored next to `root` that carry a value: Markdown
// holds the editor content only, so they do not survive a round trip. A null
// sibling (`{"media": null, "root": …}` on some projects) is dropped quietly —
// the state written back without it reads the same.
func (w *mdWriter) stateKeys(state map[string]any) {
	var keys []string
	for k, v := range state {
		if k != "root" && v != nil {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return
	}
	sort.Strings(keys)
	w.issue(IssueLossyStateKeys, "", "the editor state holds "+strings.Join(keys, ", ")+
		" next to root; Markdown carries the root only, so a write of this Markdown drops them")
}

// placeholder writes a node as a placeholder comment.
func (w *mdWriter) placeholder(n Node, path, why string) string {
	s, err := encodePlaceholder(n)
	if err != nil {
		w.err = err
		return ""
	}
	w.issue(IssuePlaceholder, path, why)
	return s
}

// checkElement reports the element attributes Markdown cannot spell.
func (w *mdWriter) checkElement(n Node, path string) {
	if f := strOf(n, "format"); f != "" && f != "left" && f != "start" {
		w.issue(IssueLossyAlignment, path, typeOf(n)+" alignment "+strconv.Quote(f)+" has no Markdown spelling and was dropped")
	}
	if typeOf(n) != "listitem" {
		if ind, _ := intOf(n, "indent"); ind > 0 {
			w.issue(IssueLossyAlignment, path, typeOf(n)+" indent "+strconv.Itoa(ind)+" has no Markdown spelling and was dropped")
		}
	}
	if s := strOf(n, "textStyle"); s != "" && typeOf(n) != "paragraph" {
		w.issue(IssueLossyStyle, path, typeOf(n)+" textStyle "+strconv.Quote(s)+" was dropped")
	}
}

// listState remembers the previous sibling list, so two adjacent lists that
// would use the same marker get different ones ('-' then '*', '.' then ')')
// and are not read back as one list. Bullet and check lists share the bullet
// marker, so they alternate with each other too.
type listState struct {
	kind string
	alt  bool
}

// markerFamily is the Markdown marker a list type is written with.
func markerFamily(listType string) string {
	if listType == "number" {
		return "ordered"
	}
	return "bullet"
}

func (w *mdWriter) blocks(children []any, path string) []string {
	var out []string
	var prev listState
	for i, c := range children {
		p := path + ".children[" + strconv.Itoa(i) + "]"
		n, ok := c.(map[string]any)
		if !ok {
			continue
		}
		t := typeOf(n)
		if t != "list" {
			prev = listState{}
		}
		switch t {
		case "paragraph":
			out = append(out, w.paragraph(n, p))
		case "heading":
			out = append(out, w.heading(n, p))
		case "quote":
			out = append(out, w.quote(n, p))
		case "list":
			kind := markerFamily(strOf(n, "listType"))
			alt := prev.kind == kind && !prev.alt
			out = append(out, w.list(n, p, alt))
			prev = listState{kind: kind, alt: alt}
		case "horizontalrule":
			// `***`, never `---`: a document that starts with a rule would
			// otherwise open with `---`, the front matter fence, and a
			// second rule later would close it (StripFrontmatter is strict
			// about the body, but the output should not depend on that).
			out = append(out, "***")
		case "upload":
			out = append(out, w.upload(n, p))
		default:
			out = append(out, w.placeholder(n, p, t+" node has no Markdown spelling; written as a lossless placeholder"))
		}
	}
	return out
}

// upload uses Payload's own Markdown form `![media:40]()` when the node is
// nothing more than a reference, and a placeholder otherwise.
func (w *mdWriter) upload(n Node, path string) string {
	rel := strOf(n, "relationTo")
	id := idString(reduceRelationValue("value", n["value"]))
	simple := rel != "" && id != "" && !strings.ContainsAny(rel+id, ":] \t\n") && strOf(n, "format") == ""
	for k, v := range n {
		switch k {
		case "type", "version", "format", "relationTo", "value":
		case "fields":
			if v != nil {
				if m, ok := v.(map[string]any); !ok || len(m) > 0 {
					simple = false
				}
			}
		default:
			simple = false
		}
	}
	if v, _ := intOf(n, "version"); v != 3 {
		simple = false
	}
	if simple && sameJSON(idValue(id), reduceRelationValue("value", n["value"])) {
		return "![" + rel + ":" + id + "]()"
	}
	return w.placeholder(n, path, "upload node carries more than a reference; written as a lossless placeholder")
}

// idString renders a document id for Markdown ("" when it is not an id).
// refDest renders a {"$ref": rel, field: value} doc value (FromMarkdown's
// pay:COLLECTION/FIELD=VALUE) back as FIELD=VALUE.
func refDest(rel string, v any) (string, bool) {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 2 || m[RefKey] != rel {
		return "", false
	}
	for k, val := range m {
		if k == RefKey {
			continue
		}
		s, ok := val.(string)
		if !ok || s == "" || strings.ContainsAny(k+s, " \t\n/()") || strings.Contains(k, "=") {
			return "", false
		}
		return k + "=" + s, true
	}
	return "", false
}

func idString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	}
	if i, ok := toInt(v); ok {
		return strconv.Itoa(i)
	}
	return ""
}

func (w *mdWriter) paragraph(n Node, path string) string {
	w.checkElement(n, path)
	kids := childrenOf(n)
	if len(kids) == 0 {
		// Markdown has no empty paragraph; the placeholder keeps the spacer
		// paragraphs editors leave between blocks.
		s, err := encodePlaceholder(n)
		if err != nil {
			w.err = err
		}
		return s
	}
	for _, k := range kids {
		if kn, ok := k.(map[string]any); ok && !isInlineType(typeOf(kn)) && len(kids) == 1 {
			return w.placeholder(n, path, "paragraph holds only a "+typeOf(kn)+" node; written as a lossless placeholder")
		}
	}
	if s, ok := w.inline(kids, path, ctxParagraph); ok {
		return s
	}
	return w.placeholder(n, path, "paragraph content could not be written as Markdown that reads back identically; written as a lossless placeholder")
}

func (w *mdWriter) heading(n Node, path string) string {
	w.checkElement(n, path)
	tag := strOf(n, "tag")
	level := 0
	if len(tag) == 2 && tag[0] == 'h' && tag[1] >= '1' && tag[1] <= '6' {
		level = int(tag[1] - '0')
	}
	if level == 0 {
		return w.placeholder(n, path, "heading tag "+strconv.Quote(tag)+" is not h1–h6; written as a lossless placeholder")
	}
	hashes := strings.Repeat("#", level)
	kids := childrenOf(n)
	if len(kids) == 0 {
		return hashes
	}
	if s, ok := w.inline(kids, path, ctxHeading); ok {
		return hashes + " " + s
	}
	return w.placeholder(n, path, "heading content could not be written as Markdown that reads back identically; written as a lossless placeholder")
}

func (w *mdWriter) quote(n Node, path string) string {
	w.checkElement(n, path)
	kids := childrenOf(n)
	if len(kids) == 0 {
		return ">"
	}
	inline, block := 0, 0
	for _, k := range kids {
		if kn, ok := k.(map[string]any); ok && isInlineType(typeOf(kn)) {
			inline++
		} else {
			block++
		}
	}
	var body string
	switch {
	case block == 0:
		s, ok := w.inline(kids, path, ctxParagraph)
		if !ok {
			return w.placeholder(n, path, "quote content could not be written as Markdown that reads back identically; written as a lossless placeholder")
		}
		body = s
	case inline == 0:
		if len(kids) == 1 {
			if kn, ok := kids[0].(map[string]any); ok && typeOf(kn) == "paragraph" {
				w.issue(IssueNormalized, path, "a quote holding one paragraph reads back as Lexical's native quote (inline children)")
			}
		}
		body = strings.Join(w.blocks(kids, path), "\n\n")
	default:
		return w.placeholder(n, path, "quote mixes inline and block children; written as a lossless placeholder")
	}
	return prefixLines(body, "> ", ">")
}

// prefixLines prefixes every line; blank lines get blankPrefix.
func prefixLines(s, prefix, blankPrefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l == "" {
			lines[i] = blankPrefix
		} else {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n")
}

// indentLines indents every line but the first by n spaces (blank lines stay
// empty).
func indentLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	pad := strings.Repeat(" ", n)
	for i := 1; i < len(lines); i++ {
		if lines[i] != "" {
			lines[i] = pad + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

// list renders a list.
func (w *mdWriter) list(n Node, path string, alt bool) string {
	w.checkElement(n, path)
	listType := strOf(n, "listType")
	start, ok := intOf(n, "start")
	if !ok || start < 0 {
		start = 1
	}
	switch listType {
	case "bullet", "number", "check":
	default:
		return w.placeholder(n, path, "list type "+strconv.Quote(listType)+" is unknown; written as a lossless placeholder")
	}
	kids := childrenOf(n)
	var items []string
	num := start
	var nestedPrev listState
	for i, c := range kids {
		p := path + ".children[" + strconv.Itoa(i) + "]"
		it, ok := c.(map[string]any)
		if !ok || typeOf(it) != "listitem" {
			return w.placeholder(n, path, "list holds a non-listitem child; written as a lossless placeholder")
		}
		w.checkElement(it, p)
		// Lexical's nested-list shape: a wrapper item whose only child is
		// the nested list. It renders under the previous item.
		if sub, ok := wrappedList(it); ok {
			kind := markerFamily(strOf(sub, "listType"))
			nalt := nestedPrev.kind == kind && !nestedPrev.alt
			nestedPrev = listState{kind: kind, alt: nalt}
			nestedMD := w.list(sub, p+".children[0]", nalt)
			if len(items) == 0 {
				// No item to hang it under: a bare marker holds it, which
				// is exactly how the wrapper reads back.
				marker := bulletMarker(alt)
				if listType == "number" {
					marker = strconv.Itoa(num) + orderedDelim(alt)
				}
				items = append(items, marker+"\n"+indentAll(nestedMD, len(marker)+1))
				continue
			}
			last := items[len(items)-1]
			items[len(items)-1] = last + nestedSep(sub) + indentAll(nestedMD, markerWidth(last))
			continue
		}
		nestedPrev = listState{}
		var marker string
		switch listType {
		case "number":
			marker = strconv.Itoa(num) + orderedDelim(alt) + " "
			num++
		case "check":
			box := "[ ] "
			if b, _ := it["checked"].(bool); b {
				box = "[x] "
			}
			marker = bulletMarker(alt) + " " + box
		default:
			marker = bulletMarker(alt) + " "
		}
		width := len(marker)
		if listType == "check" {
			width = 2
		}
		content, ok := w.itemContent(it, p)
		if !ok {
			return w.placeholder(n, path, "a list item could not be written as Markdown that reads back identically; the list is written as a lossless placeholder")
		}
		if content == "" {
			// An empty item followed by a wrapper needs a body, or the
			// nested list would read back as belonging to a bare marker
			// (the wrapper shape) and the empty item would vanish.
			if listType != "check" && i+1 < len(kids) && isWrapper(kids[i+1]) {
				items = append(items, marker+emptyBody)
				continue
			}
			items = append(items, strings.TrimRight(marker, " "))
			continue
		}
		items = append(items, marker+indentLines(content, width))
	}
	if len(items) == 0 {
		return w.placeholder(n, path, "empty list; written as a lossless placeholder")
	}
	return strings.Join(items, "\n")
}

// emptyBody is the body of an item that is empty but must not be bare. The
// reader drops the comment and keeps the item.
const emptyBody = "<!-- -->"

// wrappedList returns the nested list of a wrapper item.
func wrappedList(it Node) (Node, bool) {
	kids := childrenOf(it)
	if len(kids) != 1 {
		return nil, false
	}
	kn, ok := kids[0].(map[string]any)
	if !ok || typeOf(kn) != "list" {
		return nil, false
	}
	return kn, true
}

func isWrapper(v any) bool {
	it, ok := v.(map[string]any)
	if !ok || typeOf(it) != "listitem" {
		return false
	}
	_, ok = wrappedList(it)
	return ok
}

// nestedSep separates an item's text from the list nested under it. Two
// kinds of nested list cannot interrupt the item's paragraph (CommonMark
// 5.3) and get a blank line: an ordered list not starting at 1, and a list
// whose first line is a bare marker (which would read as a setext
// underline). Anything else follows directly, keeping the list tight.
func nestedSep(list Node) string {
	if start, _ := intOf(list, "start"); strOf(list, "listType") == "number" && start != 1 {
		return "\n\n"
	}
	kids := childrenOf(list)
	if len(kids) > 0 {
		if first, ok := kids[0].(map[string]any); ok && strOf(list, "listType") != "check" {
			if isWrapper(first) {
				return "\n\n"
			}
			if len(childrenOf(first)) == 0 && (len(kids) < 2 || !isWrapper(kids[1])) {
				return "\n\n"
			}
		}
	}
	return "\n"
}

func orderedDelim(alt bool) string {
	if alt {
		return ")"
	}
	return "."
}

func bulletMarker(alt bool) string {
	if alt {
		return "*"
	}
	return "-"
}

// markerWidth is the content offset of an item line: the marker and the
// space after it (a task box is content, not marker).
func markerWidth(item string) int {
	first := item
	if i := strings.IndexByte(item, '\n'); i >= 0 {
		first = item[:i]
	}
	if m := itemMarkerRe.FindString(first); m != "" {
		if strings.HasSuffix(m, " ") {
			return len(m)
		}
		return len(m) + 1
	}
	return 2
}

// indentAll indents every non-blank line by n spaces.
func indentAll(s string, n int) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}

// itemContent renders a list item's children. The native shape is inline
// content only; a nested list after inline content (a non-native shape some
// importers write) renders beneath it and reads back as Lexical's wrapper
// shape, which is reported.
func (w *mdWriter) itemContent(it Node, path string) (string, bool) {
	kids := childrenOf(it)
	var inline []any
	var tail []string
	var prev listState
	for i, k := range kids {
		kn, ok := k.(map[string]any)
		if !ok {
			continue
		}
		switch t := typeOf(kn); {
		case isInlineType(t):
			if len(tail) > 0 {
				return "", false
			}
			inline = append(inline, kn)
		case t == "list":
			w.issue(IssueNormalized, path, "a list item holding text and a nested list reads back as Lexical's wrapper-item shape")
			kind := markerFamily(strOf(kn, "listType"))
			alt := prev.kind == kind && !prev.alt
			prev = listState{kind: kind, alt: alt}
			tail = append(tail, nestedSep(kn)+w.list(kn, path+".children["+strconv.Itoa(i)+"]", alt))
		case t == "paragraph":
			if len(tail) > 0 {
				return "", false
			}
			if len(inline) > 0 {
				inline = append(inline, newLinebreak())
			}
			inline = append(inline, childrenOf(kn)...)
			w.issue(IssueNormalized, path, "a paragraph inside a list item reads back as inline content of the item")
		default:
			return "", false
		}
	}
	var head string
	if len(inline) > 0 {
		s, ok := w.inline(inline, path, ctxParagraph)
		if !ok {
			return "", false
		}
		head = s
	}
	if len(tail) == 0 {
		return head, true
	}
	return head + strings.Join(tail, ""), true
}

// ---------------------------------------------------------------------------
// inline rendering
// ---------------------------------------------------------------------------

type inlineCtx int

const (
	ctxParagraph inlineCtx = iota
	ctxHeading
)

type renderStyle int

const (
	styleMarkdown renderStyle = iota
	styleHTML
)

// inline renders inline nodes. It tries plain Markdown delimiters first and
// verifies the result reads back identically; when CommonMark's flanking
// rules defeat a delimiter (bold text ending in punctuation followed by a
// letter, adjacent code spans, …) it re-renders with HTML format tags, which
// have no flanking rules; if even that fails the caller writes a placeholder.
func (w *mdWriter) inline(kids []any, path string, ctx inlineCtx) (string, bool) {
	w.checkInline(kids, path)
	want := canonInline(kids)
	accept := func(s string) bool {
		got, ok := readBackInline(s, ctx)
		return ok && sameJSON(canonInline(got), want)
	}
	// Markdown delimiters first; every delimiter run CommonMark cannot flank
	// in its actual context is switched to its HTML tag and the paragraph is
	// re-rendered, so only the runs that need it fall back.
	shared := &renderShared{forced: map[int]bool{}}
	for attempt := 0; attempt < 8; attempt++ {
		shared.seq, shared.failed = 0, nil
		r := &inlineRenderer{style: styleMarkdown, ctx: ctx, shared: shared}
		s, ok := r.render(kids)
		if ok && accept(s) {
			return s, true
		}
		grew := false
		for _, run := range shared.failed {
			if !shared.forced[run] {
				shared.forced[run] = true
				grew = true
			}
		}
		if !grew {
			break
		}
	}
	r := &inlineRenderer{style: styleHTML, ctx: ctx, shared: &renderShared{forced: map[int]bool{}}}
	if s, ok := r.render(kids); ok && accept(s) {
		return s, true
	}
	return "", false
}

// renderShared is the state one paragraph's renderers share across
// attempts: the delimiter runs forced to HTML, and the runs that failed the
// flanking check in the last attempt. Run ids are assigned in opening order,
// which does not depend on the delimiter spelling, so they are stable across
// attempts.
type renderShared struct {
	forced map[int]bool
	seq    int
	failed []int
}

// delimMark is one emitted Markdown delimiter, checked for flanking once
// the output is assembled.
type delimMark struct {
	run     int
	opening bool
}

// checkInline reports lossy text attributes.
func (w *mdWriter) checkInline(kids []any, path string) {
	for i, k := range kids {
		kn, ok := k.(map[string]any)
		if !ok {
			continue
		}
		p := path + ".children[" + strconv.Itoa(i) + "]"
		switch typeOf(kn) {
		case "text", "tab":
			if s := strOf(kn, "style"); s != "" {
				w.issue(IssueLossyStyle, p, "text style "+strconv.Quote(s)+" was dropped")
			}
			if f, _ := intOf(kn, "format"); f&^knownFormats != 0 {
				w.issue(IssueLossyFormat, p, "text format bits "+strconv.Itoa(f&^knownFormats)+" (lowercase/uppercase/capitalize) have no Markdown spelling and were dropped")
			}
		case "link", "autolink":
			w.checkInline(childrenOf(kn), p)
		}
	}
}

// readBackInline parses a rendered paragraph or heading and returns its
// inline children, exactly as FromMarkdown would build them.
func readBackInline(s string, ctx inlineCtx) ([]any, bool) {
	src := s
	if ctx == ctxHeading {
		src = "# " + s
	}
	state, _, err := FromMarkdownIssues(src)
	if err != nil {
		return nil, false
	}
	root, _ := rootOf(state)
	kids := childrenOf(root)
	if len(kids) != 1 {
		return nil, false
	}
	n, ok := kids[0].(map[string]any)
	if !ok {
		return nil, false
	}
	want := "paragraph"
	if ctx == ctxHeading {
		want = "heading"
	}
	if typeOf(n) != want {
		return nil, false
	}
	return childrenOf(n), true
}

// canonTok is the comparison form of inline content: formats only as far as
// they are visible (bold/italic/sub/sup on whitespace are not), text merged
// across node boundaries, links reduced to what they mean.
type canonTok struct {
	K string     `json:"k"`
	T string     `json:"t,omitempty"`
	F int        `json:"f,omitempty"`
	L any        `json:"l,omitempty"`
	C []canonTok `json:"c,omitempty"`
}

const (
	invisibleOnSpace = FormatBold | FormatItalic | FormatSubscript | FormatSuperscript
	visibleOnSpace   = FormatUnderline | FormatStrikethrough | FormatHighlight
)

func canonInline(kids []any) []canonTok {
	var out []canonTok
	addText := func(s string, f int) {
		for _, r := range s {
			rf := f & knownFormats
			if unicode.IsSpace(r) {
				rf &^= invisibleOnSpace
			}
			if len(out) > 0 && out[len(out)-1].K == "text" && out[len(out)-1].F == rf {
				out[len(out)-1].T += string(r)
				continue
			}
			out = append(out, canonTok{K: "text", T: string(r), F: rf})
		}
	}
	for _, k := range kids {
		kn, ok := k.(map[string]any)
		if !ok {
			continue
		}
		switch t := typeOf(kn); t {
		case "text":
			f, _ := intOf(kn, "format")
			addText(strOf(kn, "text"), f)
		case "tab":
			out = append(out, canonTok{K: "tab"})
		case "linebreak":
			out = append(out, canonTok{K: "br"})
		case "link", "autolink":
			out = append(out, canonTok{K: t, L: canonLink(kn), C: canonInline(childrenOf(kn))})
		default:
			s, err := encodePlaceholder(kn)
			if err != nil {
				out = append(out, canonTok{K: "bad"})
				continue
			}
			back, _, _ := decodePlaceholder(s)
			out = append(out, canonTok{K: "node", L: back})
		}
	}
	return out
}

// canonLink is what a link means: where it goes and how it opens.
func canonLink(n Node) map[string]any {
	fields, _ := n["fields"].(map[string]any)
	out := map[string]any{}
	lt, _ := fields["linkType"].(string)
	if lt == "" {
		lt = "custom"
	}
	out["linkType"] = lt
	if typeOf(n) == "link" {
		nt, _ := fields["newTab"].(bool)
		out["newTab"] = nt
	} else if nt, _ := fields["newTab"].(bool); nt {
		out["newTab"] = true
	}
	if lt == "internal" {
		out["doc"] = canonDoc(fields["doc"])
	} else {
		u, _ := fields["url"].(string)
		out["url"] = u
	}
	for k, v := range fields {
		switch k {
		case "linkType", "newTab", "url", "doc":
		default:
			out["x:"+k] = v
		}
	}
	return out
}

func canonDoc(v any) any {
	doc, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return map[string]any{"relationTo": doc["relationTo"], "value": reduceRelationValue("value", doc["value"])}
}

// inlineRenderer emits one run of inline nodes in one style.
//
// It renders into tokens rather than straight into a string: syntax tokens
// (delimiters, links, code spans, breaks, placeholders) and text tokens.
// Adjacent text tokens are merged before anything is escaped, and each text
// run is escaped knowing the syntax on either side of it. That makes the
// output independent of how the text happened to be split into nodes, which
// is what keeps ToMarkdown a fixed point: FromMarkdown merges adjacent
// equal-format text, so a split the original had cannot be allowed to change
// the escaping.
type inlineRenderer struct {
	style  renderStyle
	ctx    inlineCtx
	shared *renderShared

	toks      []outTok
	open      []int // open format bits, outermost first
	openRuns  []int // the run id of each open format
	marks     []delimMark
	pendingWS string
}

// outTok is one rendered piece. A tokText piece is unescaped text, escaped
// at assembly; every other kind is written verbatim.
type outTok struct {
	s    string
	kind tokKind
	mark int // index into marks, -1 when none
}

type tokKind int

const (
	tokSyntax tokKind = iota
	tokText
	tokBreak   // a hard break: the next text starts a line
	tokLinkEnd // a link: `{` right after it would read as attributes
)

// formatOrder is the tie-break order for opening formats.
var formatOrder = []int{FormatBold, FormatItalic, FormatStrikethrough, FormatUnderline,
	FormatHighlight, FormatSubscript, FormatSuperscript}

func (r *inlineRenderer) delims(bit, run int) (string, string) {
	if r.style == styleHTML || r.shared.forced[run] {
		switch bit {
		case FormatBold:
			return "<strong>", "</strong>"
		case FormatItalic:
			return "<em>", "</em>"
		case FormatStrikethrough:
			return "<s>", "</s>"
		}
	} else {
		switch bit {
		case FormatBold:
			return "**", "**"
		case FormatItalic:
			return "*", "*"
		case FormatStrikethrough:
			return "~~", "~~"
		}
	}
	switch bit {
	case FormatUnderline:
		return "<u>", "</u>"
	case FormatHighlight:
		return "<mark>", "</mark>"
	case FormatSubscript:
		return "<sub>", "</sub>"
	case FormatSuperscript:
		return "<sup>", "</sup>"
	case FormatCode:
		return "<code>", "</code>"
	}
	return "", ""
}

func (r *inlineRenderer) syntax(s string, kind tokKind) {
	r.toks = append(r.toks, outTok{s: s, kind: kind, mark: -1})
}

func (r *inlineRenderer) text(s string) {
	if s == "" {
		return
	}
	if n := len(r.toks); n > 0 && r.toks[n-1].kind == tokText {
		r.toks[n-1].s += s
		return
	}
	r.toks = append(r.toks, outTok{s: s, kind: tokText, mark: -1})
}

// mark writes a format delimiter, remembering Markdown ones for the
// flanking check.
func (r *inlineRenderer) mark(run int, opening bool, d string) {
	idx := -1
	if d != "" && (d[0] == '*' || d[0] == '~') {
		r.marks = append(r.marks, delimMark{run: run, opening: opening})
		idx = len(r.marks) - 1
	}
	r.toks = append(r.toks, outTok{s: d, kind: tokSyntax, mark: idx})
}

// leaf is one inline node flattened for emission.
type leaf struct {
	node Node
	kind string // text, tab, br, link, autolink, node
	text string
	fmt  int
}

func (r *inlineRenderer) render(kids []any) (string, bool) {
	var leaves []leaf
	for _, k := range kids {
		kn, ok := k.(map[string]any)
		if !ok {
			continue
		}
		f, _ := intOf(kn, "format")
		switch t := typeOf(kn); t {
		case "text":
			s := strOf(kn, "text")
			if s == "" {
				continue
			}
			// Adjacent text of one format is one run, as FromMarkdown will
			// read it back; rendering it as one keeps the output a fixed
			// point (two adjacent code spans, for one, cannot be written).
			if n := len(leaves); n > 0 && leaves[n-1].kind == "text" && leaves[n-1].fmt == f&knownFormats {
				leaves[n-1].text += s
				continue
			}
			leaves = append(leaves, leaf{node: kn, kind: "text", text: s, fmt: f & knownFormats})
		case "tab":
			leaves = append(leaves, leaf{node: kn, kind: "tab", fmt: f & knownFormats})
		case "linebreak":
			leaves = append(leaves, leaf{node: kn, kind: "br"})
		case "link", "autolink":
			leaves = append(leaves, leaf{node: kn, kind: t})
		default:
			leaves = append(leaves, leaf{node: kn, kind: "node"})
		}
	}
	for i, lf := range leaves {
		switch lf.kind {
		case "text", "tab":
			r.leafText(lf, leaves[i+1:])
		case "br":
			r.closeTo(0)
			r.flushWS()
			first := i == 0
			last := i == len(leaves)-1
			adjacent := (i > 0 && leaves[i-1].kind == "br") || (!last && leaves[i+1].kind == "br")
			if r.ctx == ctxHeading || first || last || adjacent {
				r.syntax("<br>", tokSyntax)
			} else {
				r.syntax("\\\n", tokBreak)
			}
		case "link", "autolink":
			r.closeTo(0)
			r.flushWS()
			s, ok := r.link(lf.node)
			if !ok {
				return "", false
			}
			r.syntax(s, tokLinkEnd)
		case "node":
			r.closeTo(0)
			r.flushWS()
			s, err := encodePlaceholder(lf.node)
			if err != nil {
				return "", false
			}
			r.syntax(s, tokSyntax)
		}
	}
	r.closeTo(0)
	r.flushWS()
	return r.assemble(), true
}

// assemble escapes every text token in its final context, joins the tokens,
// and runs the flanking check on the delimiters where they finally landed.
func (r *inlineRenderer) assemble() string {
	var sb strings.Builder
	offsets := make([][2]int, len(r.marks))
	for i, t := range r.toks {
		if t.kind != tokText {
			start := sb.Len()
			sb.WriteString(t.s)
			if t.mark >= 0 {
				offsets[t.mark] = [2]int{start, sb.Len()}
			}
			continue
		}
		atLineStart := i == 0 || r.toks[i-1].kind == tokBreak
		afterLink := i > 0 && r.toks[i-1].kind == tokLinkEnd
		next := ""
		lineEnd := i == len(r.toks)-1
		if !lineEnd {
			next = r.toks[i+1].s
			lineEnd = r.toks[i+1].kind == tokBreak
		}
		prev, _ := utf8.DecodeLastRuneInString(sb.String())
		if sb.Len() == 0 {
			prev = ' '
		}
		sb.WriteString(escapeText(t.s, textCtx{
			heading: r.ctx == ctxHeading, atLineStart: atLineStart, lineEnd: lineEnd,
			afterLink: afterLink, prev: prev, next: next,
		}))
	}
	out := sb.String()
	for i, m := range r.marks {
		if !flanks(out, offsets[i][0], offsets[i][1], m.opening) {
			r.shared.failed = append(r.shared.failed, m.run)
		}
	}
	return out
}

// flanks applies CommonMark's flanking rule to the delimiter out[s:e].
func flanks(out string, s, e int, opening bool) bool {
	prev, next := ' ', ' '
	if s > 0 {
		prev, _ = utf8.DecodeLastRuneInString(out[:s])
	}
	if e < len(out) {
		next, _ = utf8.DecodeRuneInString(out[e:])
	}
	prevWS, nextWS := unicode.IsSpace(prev), unicode.IsSpace(next)
	prevP, nextP := isPunct(prev), isPunct(next)
	if opening {
		return !nextWS && (!nextP || prevWS || prevP)
	}
	return !prevWS && (!prevP || nextWS || nextP)
}

// closeTo closes open formats down to depth d.
func (r *inlineRenderer) closeTo(d int) {
	for len(r.open) > d {
		bit := r.open[len(r.open)-1]
		run := r.openRuns[len(r.openRuns)-1]
		_, cl := r.delims(bit, run)
		r.mark(run, false, cl)
		r.open = r.open[:len(r.open)-1]
		r.openRuns = r.openRuns[:len(r.openRuns)-1]
	}
}

// flushWS writes whitespace held back from the end of a formatted run, so it
// lands outside the closing delimiter (a delimiter after a space cannot
// close).
func (r *inlineRenderer) flushWS() {
	r.text(r.pendingWS)
	r.pendingWS = ""
}

func (r *inlineRenderer) leafText(lf leaf, rest []leaf) {
	f := lf.fmt &^ FormatCode
	// Close every open format this leaf does not carry, and everything
	// opened after it (reopened below).
	for i, bit := range r.open {
		if f&bit == 0 {
			r.closeTo(i)
			break
		}
	}
	if lf.kind == "tab" {
		r.flushWS()
		r.openFormats(f, rest)
		r.syntax("&#9;", tokSyntax)
		return
	}
	code := lf.fmt&FormatCode != 0
	lead, core, trail := splitWS(lf.text)
	// Edge whitespace moves outside the delimiters only where that changes
	// nothing visible: bold/italic/sub/sup on a space do not show, while
	// underline, strikethrough, highlight and code do. HTML tags have no
	// flanking rules, so in the HTML style nothing needs to move.
	if code || r.style == styleHTML || lf.fmt&visibleOnSpace != 0 {
		lead, core, trail = "", lf.text, ""
	}
	if core == "" {
		// Whitespace only: bold/italic on a space is invisible, anything
		// else fails verification and the HTML style takes over.
		r.pendingWS += lead
		return
	}
	r.flushWS()
	r.text(lead)
	r.openFormats(f, rest)
	if code {
		r.codeSpan(core)
	} else {
		r.text(core)
	}
	r.pendingWS = trail
}

// openFormats opens the bits of f that are not open, longest-lasting first,
// so a format that continues over the next runs is not closed and reopened.
func (r *inlineRenderer) openFormats(f int, rest []leaf) {
	isOpen := 0
	for _, b := range r.open {
		isOpen |= b
	}
	var want []int
	for _, b := range formatOrder {
		if f&b != 0 && isOpen&b == 0 {
			want = append(want, b)
		}
	}
	runLen := func(b int) int {
		n := 0
		for _, lf := range rest {
			if (lf.kind != "text" && lf.kind != "tab") || lf.fmt&b == 0 {
				break
			}
			n++
		}
		return n
	}
	sort.SliceStable(want, func(i, j int) bool { return runLen(want[i]) > runLen(want[j]) })
	for _, b := range want {
		r.shared.seq++
		run := r.shared.seq
		op, _ := r.delims(b, run)
		r.mark(run, true, op)
		r.open = append(r.open, b)
		r.openRuns = append(r.openRuns, run)
	}
}

func (r *inlineRenderer) codeSpan(s string) {
	if r.style == styleHTML {
		op, cl := r.delims(FormatCode, 0)
		r.syntax(op, tokSyntax)
		r.text(s)
		r.syntax(cl, tokSyntax)
		return
	}
	longest, run := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	pad := ""
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") ||
		(strings.HasPrefix(s, " ") && strings.HasSuffix(s, " ") && strings.Trim(s, " ") != "") {
		pad = " "
	}
	r.syntax(fence+pad+s+pad+fence, tokSyntax)
}

// splitWS splits leading and trailing whitespace off a text.
func splitWS(s string) (lead, core, trail string) {
	core = strings.TrimLeftFunc(s, unicode.IsSpace)
	lead = s[:len(s)-len(core)]
	c2 := strings.TrimRightFunc(core, unicode.IsSpace)
	trail = core[len(c2):]
	return lead, c2, trail
}

var (
	itemMarkerRe       = regexp.MustCompile(`^(\d{1,9}[.)]|[-*+])( |$)`)
	lineStartOrderedRe = regexp.MustCompile(`^\d{1,9}[.)]([ \t]|$)`)
	entityLikeRe       = regexp.MustCompile(`^&(?:#[0-9]{1,7}|#[xX][0-9a-fA-F]{1,6}|[A-Za-z][A-Za-z0-9]{1,31});`)
)

// textCtx is what escaping needs to know about a text run's surroundings.
type textCtx struct {
	heading     bool
	atLineStart bool   // first thing on a line: block syntax must be escaped
	lineEnd     bool   // last thing on a line: a final space would be trimmed
	afterLink   bool   // directly after a link: `{` would read as attributes
	prev        rune   // the output character before the run
	next        string // the output after the run (the next token)
}

// escapeText escapes a text run so the reader takes every character as
// text. It escapes only what is ambiguous in context, so ordinary prose
// ("5 < 6", "R&D", "snake_case", "~5 min") stays readable.
func escapeText(s string, c textCtx) string {
	var sb strings.Builder
	if c.atLineStart {
		// Leading whitespace of a line is not content to the reader.
		for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
			if s[0] == ' ' {
				sb.WriteString("&#32;")
			} else {
				sb.WriteString("&#9;")
			}
			s = s[1:]
		}
		switch {
		case s == "":
		case s[0] == '#' || s[0] == '>' || s[0] == '=':
			sb.WriteByte('\\')
		case (s[0] == '-' || s[0] == '+') && (len(s) == 1 || s[1] == ' ' || s[1] == '\t' || s[1] == s[0]):
			sb.WriteByte('\\')
		case lineStartOrderedRe.MatchString(s):
			k := strings.IndexAny(s, ".)")
			sb.WriteString(s[:k])
			sb.WriteByte('\\')
			s = s[k:]
		}
	}
	if c.afterLink && strings.HasPrefix(s, "{") {
		sb.WriteByte('\\')
	}
	nextRune, _ := utf8.DecodeRuneInString(c.next)
	if c.next == "" {
		nextRune = ' '
	}
	trailingSpace := -1
	if c.lineEnd && strings.HasSuffix(s, " ") {
		trailingSpace = len(s) - 1
	}
	for i := 0; i < len(s); {
		ch, size := utf8.DecodeRuneInString(s[i:])
		prev := c.prev
		if i > 0 {
			prev, _ = utf8.DecodeLastRuneInString(s[:i])
		}
		next := nextRune
		if i+size < len(s) {
			next, _ = utf8.DecodeRuneInString(s[i+size:])
		}
		if i == trailingSpace {
			sb.WriteString("&#32;")
			i += size
			continue
		}
		switch ch {
		case '\\', '*', '`', '[', ']':
			sb.WriteByte('\\')
			sb.WriteRune(ch)
		case '\n':
			sb.WriteString("&#10;")
		case '\t':
			sb.WriteString("&#9;")
		case '_':
			if isAlnum(prev) && isAlnum(next) {
				sb.WriteRune(ch)
			} else {
				sb.WriteString("\\_")
			}
		case '~':
			if prev == '~' || next == '~' {
				sb.WriteString("\\~")
			} else {
				sb.WriteRune(ch)
			}
		case '<':
			if isAlnum(next) || next == '/' || next == '!' || next == '?' {
				sb.WriteString("\\<")
			} else {
				sb.WriteRune(ch)
			}
		case '&':
			if entityLikeRe.MatchString(s[i:]) {
				sb.WriteString("\\&")
			} else {
				sb.WriteRune(ch)
			}
		case '!':
			if next == '[' {
				sb.WriteString("\\!")
			} else {
				sb.WriteRune(ch)
			}
		case '#':
			if c.heading && (next == ' ' || i+size == len(s) && c.next == "") {
				sb.WriteString("\\#")
			} else {
				sb.WriteRune(ch)
			}
		default:
			sb.WriteRune(ch)
		}
		i += size
	}
	return sb.String()
}

func isAlnum(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// link renders a link or autolink with its children and attributes.
func (r *inlineRenderer) link(n Node) (string, bool) {
	fields, _ := n["fields"].(map[string]any)
	lt, _ := fields["linkType"].(string)
	url, _ := fields["url"].(string)
	newTab, _ := fields["newTab"].(bool)
	kids := childrenOf(n)
	autolink := typeOf(n) == "autolink"

	var extra []string
	for k, v := range fields {
		switch k {
		case "linkType", "newTab", "url", "doc":
			continue
		}
		if !attrKeyOK(k) {
			return "", false
		}
		extra = append(extra, k+"="+attrValue(v))
	}
	sort.Strings(extra)

	if autolink && !newTab && len(extra) == 0 && lt != "internal" {
		if txt, ok := plainTextOnly(kids); ok {
			switch {
			case txt == url && uriAutoRe.MatchString("<"+url+">"):
				return "<" + url + ">", true
			case "mailto:"+txt == url && emailAutoRe.MatchString("<"+txt+">"):
				return "<" + txt + ">", true
			}
		}
	}

	inner := &inlineRenderer{style: r.style, ctx: r.ctx, shared: r.shared}
	text, ok := inner.render(kids)
	if !ok || strings.Contains(text, "\n") {
		return "", false
	}
	var dest string
	if lt == "internal" {
		dest = "pay:"
		if doc, ok := fields["doc"].(map[string]any); ok {
			rel, _ := doc["relationTo"].(string)
			id := idString(reduceRelationValue("value", doc["value"]))
			if ref, ok := refDest(rel, doc["value"]); ok {
				id = ref
			}
			if rel == "" || id == "" {
				return "", false
			}
			dest += rel + "/" + id
		}
	} else {
		if strings.HasPrefix(url, "pay:") {
			return "", false
		}
		dest = linkDest(url)
	}
	var attrs []string
	if autolink {
		attrs = append(attrs, "autolink")
	}
	if newTab {
		attrs = append(attrs, "target=_blank")
	}
	attrs = append(attrs, extra...)
	out := "[" + text + "](" + dest + ")"
	if len(attrs) > 0 {
		out += "{" + strings.Join(attrs, " ") + "}"
	}
	return out, true
}

func attrKeyOK(k string) bool {
	if k == "" || k == "target" || k == "autolink" {
		return false
	}
	for i := 0; i < len(k); i++ {
		if !isAttrKeyChar(k[i]) {
			return false
		}
	}
	return true
}

// attrValue writes a link-field value: strings as JSON strings, everything
// else as single-quoted raw JSON.
func attrValue(v any) string {
	if s, ok := v.(string); ok {
		return strconv.Quote(s)
	}
	b, err := jsonCompact(v)
	if err != nil || strings.ContainsRune(b, '\'') {
		return strconv.Quote(b)
	}
	return "'" + b + "'"
}

func plainTextOnly(kids []any) (string, bool) {
	if len(kids) != 1 {
		return "", false
	}
	n, ok := kids[0].(map[string]any)
	if !ok || typeOf(n) != "text" {
		return "", false
	}
	if f, _ := intOf(n, "format"); f != 0 {
		return "", false
	}
	return strOf(n, "text"), true
}

// linkDest writes a link destination, in <…> form when it has spaces or
// parentheses, escaping what the reader would otherwise interpret.
func linkDest(u string) string {
	var sb strings.Builder
	pointy := u == "" || strings.ContainsAny(u, " ()<>\t")
	if pointy && strings.ContainsAny(u, "\n") {
		pointy = false
	}
	if pointy {
		sb.WriteByte('<')
	}
	for i := 0; i < len(u); i++ {
		c := u[i]
		switch {
		case c == '\\' || c == '<' || c == '>':
			sb.WriteByte('\\')
			sb.WriteByte(c)
		case !pointy && (c == '(' || c == ')'):
			sb.WriteByte('\\')
			sb.WriteByte(c)
		case c == '&' && entityLikeRe.MatchString(u[i:]):
			sb.WriteString("\\&")
		default:
			sb.WriteByte(c)
		}
	}
	if pointy {
		sb.WriteByte('>')
	}
	if u == "" {
		return ""
	}
	return sb.String()
}

func jsonCompact(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}
