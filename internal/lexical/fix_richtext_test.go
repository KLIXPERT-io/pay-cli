package lexical

import (
	"strings"
	"testing"
)

func blockTypes(kids []any) string {
	var out []string
	for _, k := range kids {
		if n, ok := k.(map[string]any); ok {
			out = append(out, typeOf(n))
		}
	}
	return strings.Join(out, " ")
}

// A state that starts with a rule and has a second one later must survive
// `pay get --richtext md` → `--set-md` (which strips front matter): the rule
// was written as `---`, so the text between the two rules was taken for
// front matter and deleted.
func TestLeadingRuleRoundTripsThroughFrontmatterStrip(t *testing.T) {
	state := map[string]any{"root": newRoot([]any{
		newHorizontalRule(),
		newParagraph([]any{newText("Wichtiger Absatz", 0)}),
		newHorizontalRule(),
		newParagraph([]any{newText("Ende", 0)}),
	})}
	md := assertLexicalRoundTrip(t, state)
	if strings.HasPrefix(md, "---") {
		t.Fatalf("ToMarkdown starts with the front matter fence:\n%s", md)
	}
	body, _, stripped := StripFrontmatter(md)
	if stripped || body != md {
		t.Fatalf("StripFrontmatter took content for front matter: stripped=%v body=%q", stripped, body)
	}
	back, err := FromMarkdown(body)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := mustJSON(t, canonState(state)), mustJSON(t, canonState(back)); a != b {
		t.Fatalf("content changed\nwant %s\ngot  %s", a, b)
	}
}

func TestStripFrontmatterOnlyStripsYAML(t *testing.T) {
	notFM := []string{
		// pay's own output before the fix, and hand-written thematic breaks
		"---\n\nWichtiger Absatz\n\n---\n\nEnde\n",
		"---\n\nText\n\n...\n\nRest\n",
		"---\nWichtiger Absatz\n---\n\nEnde",
		"---\n# Heading\n---\n",
		"---\n  indented: x\n---\n",
		"---\nkey: value\nA sentence without a colon\n---\n",
		"---\nhttps://example.com\n---\n",
	}
	for _, md := range notFM {
		if body, _, ok := StripFrontmatter(md); ok || body != md {
			t.Errorf("%q: stripped as front matter (body %q)", md, body)
		}
	}
	yes := []struct{ md, fm, body string }{
		{"---\ntitle: T\ntags: [a]\n---\n\n# Body\n", "title: T\ntags: [a]", "\n# Body\n"},
		{"---\ntitle: T\nknowledge:\n    - a.md\n  # note\n\ndescription: 'x: y'\nog:image: /x.png\n\"quoted key\": 1\n...\nBody", "", "Body"},
		{"\uFEFF---\r\ntitle: T\r\n---\r\nBody", "title: T", "Body"},
		{"---\nempty:\n- item\n---\n", "empty:\n- item", ""},
	}
	for _, c := range yes {
		body, fm, ok := StripFrontmatter(c.md)
		if !ok || body != c.body || (c.fm != "" && fm != c.fm) {
			t.Errorf("%q: ok=%v fm=%q body=%q", c.md, ok, fm, body)
		}
	}
}

func TestReferenceLinks(t *testing.T) {
	md := "See [the docs][1], [Foo], [foo][] and [Bar] [x](https://inline.io).\n\n" +
		"[1]: https://example.com/docs \"Title\"\n" +
		"[FOO]:   <https://foo.de/a b>\n" +
		"[bar]:\n  pay:pages/47\n  'Titel'\n"
	kids, issues := fromMD(t, md)
	if len(kids) != 1 {
		t.Fatalf("definitions must not become a paragraph: %s", mustJSONString(kids))
	}
	got := inlineSig(childrenOf(node(t, kids[0])))
	for _, want := range []string{
		`link({"linkType":"custom","newTab":false,"url":"https://example.com/docs"})[0:the docs]`,
		`link({"linkType":"custom","newTab":false,"url":"https://foo.de/a b"})[0:Foo]`,
		`link({"linkType":"custom","newTab":false,"url":"https://foo.de/a b"})[0:foo]`,
		`[0:Bar]`,
		`"linkType":"internal"`,
		`link({"linkType":"custom","newTab":false,"url":"https://inline.io"})[0:x]`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in\n%s", want, got)
		}
	}
	if strings.Contains(got, "[1]") || strings.Contains(got, "]:") {
		t.Errorf("reference syntax left as text: %s", got)
	}
	if hasIssue(issues, IssueLinkRefUndefined) {
		t.Errorf("unexpected issue: %v", issues)
	}
}

func TestReferenceLinkEdgeCases(t *testing.T) {
	// A definition inside a later paragraph position is text, not a definition.
	kids, _ := fromMD(t, "Text\n[1]: https://x.io")
	if got := inlineSig(childrenOf(node(t, kids[0]))); !strings.Contains(got, "[1]: https://x.io") {
		t.Errorf("mid-paragraph definition consumed: %s", got)
	}
	// Undefined full reference: text plus an issue; an index is not reported.
	kids, issues := fromMD(t, "see [docs][nope] and m[i][j]")
	if got := inlineSig(childrenOf(node(t, kids[0]))); got != "0:see [docs][nope] and m[i][j]" {
		t.Errorf("got %s", got)
	}
	n := 0
	for _, is := range issues {
		if is.Code == IssueLinkRefUndefined {
			n++
		}
	}
	if n != 1 {
		t.Errorf("want one %s issue, got %v", IssueLinkRefUndefined, issues)
	}
	// Definitions only → no block; a definition followed by a setext line
	// does not make an empty heading.
	kids, _ = fromMD(t, "[a]: /x\n\nText [a]")
	if blockTypes(kids) != "paragraph" {
		t.Errorf("got %s", blockTypes(kids))
	}
	kids, _ = fromMD(t, "[a]: /x\n===")
	if blockTypes(kids) != "paragraph" || inlineSig(childrenOf(node(t, kids[0]))) != "0:===" {
		t.Errorf("got %s", mustJSONString(kids))
	}
	// Definitions inside a quote apply to the whole document.
	kids, _ = fromMD(t, "> [q]: https://q.io\n\n[q]")
	if got := inlineSig(childrenOf(node(t, kids[len(kids)-1]))); !strings.Contains(got, "https://q.io") {
		t.Errorf("quote definition not used: %s", got)
	}
	// "[x]: two words" is not a definition (the destination has no title form).
	kids, _ = fromMD(t, "[Hinweis]: bitte beachten")
	if got := inlineSig(childrenOf(node(t, kids[0]))); got != "0:[Hinweis]: bitte beachten" {
		t.Errorf("got %s", got)
	}
}

func TestListItemBlocksFlattenedAreReported(t *testing.T) {
	kids, issues := fromMD(t, "- ## heading in list\n- > quote in list\n- plain")
	if blockTypes(kids) != "list" {
		t.Fatalf("got %s", blockTypes(kids))
	}
	n := 0
	for _, is := range issues {
		if is.Code == IssueListItemBlockFlattened {
			n++
			if is.Line == 0 {
				t.Errorf("issue without a line: %+v", is)
			}
		}
	}
	if n != 2 {
		t.Errorf("want 2 %s issues, got %v", IssueListItemBlockFlattened, issues)
	}
}

func TestToMarkdownReportsStateKeysBesideRoot(t *testing.T) {
	root := newRoot([]any{newParagraph([]any{newText("x", 0)})})
	_, issues, err := ToMarkdownIssues(map[string]any{"root": root, "media": nil})
	if err != nil || hasIssue(issues, IssueLossyStateKeys) {
		t.Errorf("a null sibling is not a loss: %v %v", err, issues)
	}
	_, issues, err = ToMarkdownIssues(map[string]any{"root": root, "media": 12, "extra": "x"})
	if err != nil || !hasIssue(issues, IssueLossyStateKeys) {
		t.Fatalf("want %s: %v %v", IssueLossyStateKeys, err, issues)
	}
	for _, is := range issues {
		if is.Code == IssueLossyStateKeys && !strings.Contains(is.Message, "extra, media") {
			t.Errorf("message does not name the keys: %s", is.Message)
		}
	}
}

// A quote inside a list item that holds block content (a heading, several
// paragraphs, a nested list, a code block) is flattened recursively: the
// listitem gets inline children only, a nested list becomes a wrapper item,
// and the result survives the round trip unchanged. Before the fix the
// quote's block children were copied into the listitem as they were.
func TestListItemQuoteFlattensRecursively(t *testing.T) {
	cases := []struct {
		md    string
		items []string // blockTypes of each listitem's children
	}{
		{"- > # Titel\n", []string{"text"}},
		{"- > a\n  >\n  > b\n", []string{"text linebreak text"}},
		{"- > a\n  > - b\n", []string{"text", "list"}},
		{"0) > \t0", []string{"text"}},
		{"- > > deep\n  > >\n  > > er\n", []string{"text linebreak text"}},
	}
	for _, c := range cases {
		state, issues, err := FromMarkdownIssues(c.md)
		if err != nil {
			t.Fatalf("%q: %v", c.md, err)
		}
		root := state["root"].(map[string]any)
		list := childrenOf(root["children"].([]any)[0].(map[string]any))
		var got []string
		for _, it := range list {
			got = append(got, blockTypes(childrenOf(it.(map[string]any))))
		}
		if strings.Join(got, " | ") != strings.Join(c.items, " | ") {
			t.Errorf("%q: listitem children %q, want %q", c.md, got, c.items)
		}
		flagged := false
		for _, is := range issues {
			flagged = flagged || is.Code == IssueListItemBlockFlattened
		}
		if !flagged {
			t.Errorf("%q: no %s finding", c.md, IssueListItemBlockFlattened)
		}
		md := assertLexicalRoundTrip(t, state)
		if strings.Contains(md, "<!-- lexical:list") {
			t.Errorf("%q: the list fell back to a placeholder:\n%s", c.md, md)
		}
	}
}

// A byte order mark without front matter used to make the first block a
// paragraph starting with U+FEFF.
func TestByteOrderMarkIsNotContent(t *testing.T) {
	state, err := FromMarkdown("\uFEFF# Title\n\nText\n")
	if err != nil {
		t.Fatal(err)
	}
	kids := state["root"].(map[string]any)["children"].([]any)
	if got := blockTypes(kids); got != "heading paragraph" {
		t.Fatalf("blocks = %q", got)
	}
	if txt := PlainText(state); strings.ContainsRune(txt, '\uFEFF') {
		t.Errorf("text keeps the BOM: %q", txt)
	}
}

// A bullet list with one task item becomes a check list (Lexical has one
// listType per list), so the plain items turn into unchecked checkboxes —
// now with a finding naming the task item's line.
func TestMixedTaskListWarns(t *testing.T) {
	_, issues, err := FromMarkdownIssues("- plain item\n- [x] done\n- another plain\n")
	if err != nil {
		t.Fatal(err)
	}
	var got []Issue
	for _, is := range issues {
		if is.Code == IssueListBecameChecklist {
			got = append(got, is)
		}
	}
	if len(got) != 1 || got[0].Line != 2 || !strings.Contains(got[0].Message, "2 plain item(s)") {
		t.Fatalf("issues = %+v", issues)
	}
	// A pure task list, and a pure bullet list, say nothing.
	for _, md := range []string{"- [ ] a\n- [x] b\n", "- a\n- b\n"} {
		_, issues, _ := FromMarkdownIssues(md)
		for _, is := range issues {
			if is.Code == IssueListBecameChecklist {
				t.Errorf("%q: %+v", md, is)
			}
		}
	}
}

// Quotes and lists nest at most maxContainerDepth levels; deeper lines are
// paragraph text with one finding. 20 000 `>` took 39 s and 400 MB before.
func TestNestingIsCapped(t *testing.T) {
	var list strings.Builder
	for i := 0; i < 300; i++ {
		list.WriteString(strings.Repeat("  ", i) + "- x\n")
	}
	for _, md := range []string{strings.Repeat(">", 20000) + " tief\n", list.String()} {
		state, issues, err := FromMarkdownIssues(md)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, is := range issues {
			if is.Code == IssueNestingCapped {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%.20q…: %d nesting_capped findings", md, n)
		}
		assertLexicalRoundTrip(t, state)
	}
	// Real nesting stays exact.
	_, issues, _ := FromMarkdownIssues("> > > a\n\n- a\n  - b\n    - c\n")
	for _, is := range issues {
		if is.Code == IssueNestingCapped {
			t.Errorf("shallow nesting capped: %+v", is)
		}
	}
}

// Reference images were mangled (`![x][r]` became "![x]" plus a link "r" to
// the image URL) or silently lost their URL. They are kept verbatim as text
// and reported like inline images.
func TestReferenceImagesAreKeptAsText(t *testing.T) {
	for _, c := range []struct{ md, text string }{
		{"![x][r]\n\n[r]: http://img\n", "![x][r]"},
		{"![x][]\n\n[x]: http://img\n", "![x][]"},
		{"![x]\n\n[x]: http://img\n", "![x]"},
	} {
		state, issues, err := FromMarkdownIssues(c.md)
		if err != nil {
			t.Fatal(err)
		}
		if got := PlainText(state); strings.TrimSpace(got) != c.text {
			t.Errorf("%q: text %q, want %q", c.md, got, c.text)
		}
		if strings.Contains(mustJSONString(state), `"link"`) {
			t.Errorf("%q: became a link: %s", c.md, mustJSONString(state))
		}
		found := false
		for _, is := range issues {
			found = found || is.Code == IssueImageAsText
		}
		if !found {
			t.Errorf("%q: no %s finding (%+v)", c.md, IssueImageAsText, issues)
		}
		assertLexicalRoundTrip(t, state)
	}
	// A reference LINK still resolves.
	state, _ := FromMarkdown("[x][r]\n\n[r]: http://a.b\n")
	if !strings.Contains(mustJSONString(state), `"url":"http://a.b"`) {
		t.Errorf("reference link: %s", mustJSONString(state))
	}
}
