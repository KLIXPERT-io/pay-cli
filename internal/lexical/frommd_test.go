package lexical

import (
	"sort"
	"strconv"
	"strings"
	"testing"
)

// fromMD converts and returns the root's children, failing the test on error.
func fromMD(t *testing.T, md string) ([]any, []Issue) {
	t.Helper()
	state, issues, err := FromMarkdownIssues(md)
	if err != nil {
		t.Fatalf("FromMarkdown(%q): %v", md, err)
	}
	root, ok := rootOf(state)
	if !ok {
		t.Fatalf("FromMarkdown(%q) returned no root: %v", md, state)
	}
	return childrenOf(root), issues
}

func node(t *testing.T, v any) Node {
	t.Helper()
	n, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not a node: %#v", v)
	}
	return n
}

// inlineSig renders inline children compactly: text as "fmt:text", other
// nodes as their type, links with their target.
func inlineSig(kids []any) string {
	var parts []string
	for _, k := range kids {
		n, ok := k.(map[string]any)
		if !ok {
			continue
		}
		switch typeOf(n) {
		case "text":
			f, _ := intOf(n, "format")
			parts = append(parts, strconv.Itoa(f)+":"+strOf(n, "text"))
		case "link", "autolink":
			fields, _ := n["fields"].(map[string]any)
			parts = append(parts, typeOf(n)+"("+mustJSONString(fields)+")["+inlineSig(childrenOf(n))+"]")
		default:
			parts = append(parts, typeOf(n))
		}
	}
	return strings.Join(parts, " | ")
}

// TestFromMarkdownNodeShapes pins the exact keys Payload 3.87 / Lexical 0.41
// export for every node FromMarkdown builds (grounded on 351 editor states read
// from the klixpert stage and on each node's exportJSON).
func TestFromMarkdownNodeShapes(t *testing.T) {
	kids, _ := fromMD(t, "# H\n\nA **b** [l](https://x.io) [i](pay:pages/47){target=_blank} <https://a.io>\\\nc\n\n- x\n\n1. y\n\n- [x] z\n\n> q\n\n---\n\n![media:40]()")
	want := []string{
		`heading:children,direction,format,indent,tag,type,version`,
		`paragraph:children,direction,format,indent,textFormat,textStyle,type,version`,
		`list:children,direction,format,indent,listType,start,tag,type,version`,
		`list:children,direction,format,indent,listType,start,tag,type,version`,
		`list:children,direction,format,indent,listType,start,tag,type,version`,
		`quote:children,direction,format,indent,type,version`,
		`horizontalrule:type,version`,
		`upload:fields,format,relationTo,type,value,version`,
	}
	if len(kids) != len(want) {
		t.Fatalf("got %d blocks, want %d: %s", len(kids), len(want), mustJSONString(kids))
	}
	for i, k := range kids {
		if got := keySig(node(t, k)); got != want[i] {
			t.Errorf("block %d = %s, want %s", i, got, want[i])
		}
	}
	para := node(t, kids[1])
	shapes := map[string]string{}
	for _, c := range childrenOf(para) {
		n := node(t, c)
		shapes[typeOf(n)] = keySig(n)
	}
	for typ, want := range map[string]string{
		"text":      "text:detail,format,mode,style,text,type,version",
		"link":      "link:children,direction,fields,format,indent,type,version",
		"autolink":  "autolink:children,direction,fields,format,indent,type,version",
		"linebreak": "linebreak:type,version",
	} {
		if shapes[typ] != want {
			t.Errorf("%s = %s, want %s", typ, shapes[typ], want)
		}
	}
	item := node(t, childrenOf(node(t, kids[2]))[0])
	if got := keySig(item); got != "listitem:children,direction,format,indent,type,value,version" {
		t.Errorf("bullet listitem = %s (no `checked` outside a check list)", got)
	}
	check := node(t, childrenOf(node(t, kids[4]))[0])
	if got := keySig(check); got != "listitem:checked,children,direction,format,indent,type,value,version" {
		t.Errorf("check listitem = %s", got)
	}
}

func keySig(n Node) string {
	keys := make([]string, 0, len(n))
	for k := range n {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return typeOf(n) + ":" + strings.Join(keys, ",")
}

func TestFromMarkdownValues(t *testing.T) {
	kids, _ := fromMD(t, "Hello **world**")
	p := node(t, kids[0])
	for k, want := range map[string]any{"direction": "ltr", "format": "", "indent": 0, "textFormat": 0, "textStyle": "", "version": 1} {
		if !sameJSON(p[k], want) {
			t.Errorf("paragraph.%s = %v, want %v", k, p[k], want)
		}
	}
	// textFormat mirrors the first text child, as Lexical 0.41 exports it.
	kids, _ = fromMD(t, "**Bold** start")
	if tf := node(t, kids[0])["textFormat"]; !sameJSON(tf, 1) {
		t.Errorf("textFormat = %v, want 1", tf)
	}
	root, _ := rootOf(mustFrom(t, "x"))
	if root["direction"] != "ltr" || !sameJSON(root["indent"], 0) || root["format"] != "" || !sameJSON(root["version"], 1) {
		t.Errorf("root = %s", mustJSONString(root))
	}
	if d := node(t, childrenOf(mustRoot(t, "שלום עולם"))[0])["direction"]; d != "rtl" {
		t.Errorf("RTL paragraph direction = %v", d)
	}
	if d := node(t, childrenOf(mustRoot(t, "123 !?"))[0])["direction"]; d != nil {
		t.Errorf("a paragraph with no strong characters has direction %v, want null", d)
	}
}

func mustFrom(t *testing.T, md string) map[string]any {
	t.Helper()
	s, err := FromMarkdown(md)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustRoot(t *testing.T, md string) Node {
	t.Helper()
	r, _ := rootOf(mustFrom(t, md))
	return r
}

func TestFromMarkdownInline(t *testing.T) {
	cases := []struct{ md, want string }{
		{"**b** *i* _i_ __b__ ***bi***", "1:b | 0:  | 2:i | 0:  | 2:i | 0:  | 1:b | 0:  | 3:bi"},
		{"~~s~~ `c` <u>u</u> <sub>d</sub> <sup>p</sup> <mark>m</mark>", "4:s | 0:  | 16:c | 0:  | 8:u | 0:  | 32:d | 0:  | 64:p | 0:  | 128:m"},
		{"<strong>b</strong><em>i</em><s>s</s><del>d</del><b>B</b><i>I</i><ins>u</ins><code>c</code>", "1:b | 2:i | 4:sd | 1:B | 2:I | 8:u | 16:c"},
		{"**bold *nested* bold**", "1:bold  | 3:nested | 1: bold"},
		{"`**not bold**`", "16:**not bold**"},
		{"`` a ` b ``", "16:a ` b"},
		{"snake_case_word and 2*3*4", "0:snake_case_word and 2 | 2:3 | 0:4"},
		{"\\*lit\\* \\_x\\_ \\# \\[y\\]", "0:*lit* _x_ # [y]"},
		{"&amp; &copy; &#228; &#x41; &nope;", "0:& © ä A &nope;"},
		{"~one~ and ~~~three~~~", "0:~one~ and ~~~three~~~"},
		{"**unclosed and *also", "0:**unclosed and *also"},
		{"soft\nbreak", "0:soft break"},
		{"hard  \nbreak", "0:hard | linebreak | 0:break"},
		{"hard\\\nbreak", "0:hard | linebreak | 0:break"},
		{"a<br>b<br/>c<br />d", "0:a | linebreak | 0:b | linebreak | 0:c | linebreak | 0:d"},
		{"tab\there", "0:tab | tab | 0:here"},
		{"x&#9;y", "0:x | tab | 0:y"},
		{"lead&#32;&#32;", "0:lead  "},
		// CommonMark nests these: <strong>Tier 1 (<strong>0,4 %</strong>):</strong>.
		{"**Tier 1 (**0,4 %**):** Text", "1:Tier 1 (0,4 %): | 0: Text"},
		{"<strong>Tier 1 (</strong>0,4 %**):** Text", "1:Tier 1 ( | 0:0,4 % | 1:): | 0: Text"},
	}
	for _, tc := range cases {
		kids, _ := fromMD(t, tc.md)
		if len(kids) != 1 {
			t.Errorf("%q: %d blocks", tc.md, len(kids))
			continue
		}
		if got := inlineSig(childrenOf(node(t, kids[0]))); got != tc.want {
			t.Errorf("%q\n got: %s\nwant: %s", tc.md, got, tc.want)
		}
	}
}

func TestFromMarkdownLinks(t *testing.T) {
	cases := []struct{ md, want string }{
		{"[a](https://x.io/p?q=1&r=2)", `link({"linkType":"custom","newTab":false,"url":"https://x.io/p?q=1\u0026r=2"})[0:a]`},
		{"[a](<https://x.io/with space>)", `link({"linkType":"custom","newTab":false,"url":"https://x.io/with space"})[0:a]`},
		{`[a](/p "a title")`, `link({"linkType":"custom","newTab":false,"url":"/p"})[0:a]`},
		{"[a](https://x.io){target=_blank}", `link({"linkType":"custom","newTab":true,"url":"https://x.io"})[0:a]`},
		{`[a](https://x.io){target="_blank" rel=nofollow n=3 o='{"k":[1]}'}`, `link({"linkType":"custom","n":3,"newTab":true,"o":{"k":[1]},"rel":"nofollow","url":"https://x.io"})[0:a]`},
		{"[a](pay:pages/47)", `link({"doc":{"relationTo":"pages","value":47},"linkType":"internal","newTab":false})[0:a]`},
		{"[a](pay:posts/6671b2c0e4b0a1a2b3c4d5e6){target=_blank}", `link({"doc":{"relationTo":"posts","value":"6671b2c0e4b0a1a2b3c4d5e6"},"linkType":"internal","newTab":true})[0:a]`},
		{"[a](pay:)", `link({"doc":null,"linkType":"internal","newTab":false})[0:a]`},
		// By a field instead of an id: a {"$ref"} the write verbs resolve.
		{"[a](pay:pages/slug=about)", `link({"doc":{"relationTo":"pages","value":{"$ref":"pages","slug":"about"}},"linkType":"internal","newTab":false})[0:a]`},
		{"[**b** and *i*](/x)", `link({"linkType":"custom","newTab":false,"url":"/x"})[1:b | 0: and  | 2:i]`},
		{"<https://x.io/a>", `autolink({"linkType":"custom","url":"https://x.io/a"})[0:https://x.io/a]`},
		{"<hi@x.io>", `autolink({"linkType":"custom","url":"mailto:hi@x.io"})[0:hi@x.io]`},
		{"[t](https://x.io){autolink}", `autolink({"linkType":"custom","url":"https://x.io"})[0:t]`},
		{"[a [b] c](/u)", `link({"linkType":"custom","newTab":false,"url":"/u"})[0:a [b] c]`},
		{"[no link] (/u)", `0:[no link] (/u)`},
		{"[x](/a\\)b)", `link({"linkType":"custom","newTab":false,"url":"/a)b"})[0:x]`},
		{"[x](/a(b))", `link({"linkType":"custom","newTab":false,"url":"/a(b)"})[0:x]`},
		{"[x](/\\&amp;)", `link({"linkType":"custom","newTab":false,"url":"/\u0026amp;"})[0:x]`},
		{"[x](/&amp;)", `link({"linkType":"custom","newTab":false,"url":"/\u0026"})[0:x]`},
	}
	for _, tc := range cases {
		kids, _ := fromMD(t, tc.md)
		if got := inlineSig(childrenOf(node(t, kids[0]))); got != tc.want {
			t.Errorf("%q\n got: %s\nwant: %s", tc.md, got, tc.want)
		}
	}
}

func TestFromMarkdownBlocks(t *testing.T) {
	cases := []struct{ md, want string }{
		{"# 1\n## 2\n### 3\n#### 4\n##### 5\n###### 6", "h1 h2 h3 h4 h5 h6"},
		{"## closed ##\n\nSetext 1\n===\n\nSetext 2\n---", "h2 h1 h2"},
		{"#hashtag", "paragraph"},
		{"***\n\n- - -\n\n___", "horizontalrule horizontalrule horizontalrule"},
		{"para\n---\n\npara\n\n---", "h2 paragraph horizontalrule"},
		{"> q", "quote"},
		{"> a\nlazy", "quote"},
		{"- a\n- b\n\n* c", "list:bullet list:bullet"},
		{"1. a\n2) b", "list:number list:number"},
		{"text\n- item", "paragraph list:bullet"},
		{"text\n2. not a list", "paragraph"},
		{"```js\ncode\n```", "paragraph"},
		{"    indented", "paragraph"},
		{"<!-- a comment -->\n\nx", "paragraph"},
		{"<!-- lexical:upload {\"relationTo\":\"media\",\"value\":3} -->", "upload"},
		{"<!-- lexical:inlineBlock {\"fields\":{\"blockType\":\"b\"}} -->", "paragraph"},
		{"<!-- lexical:paragraph -->", "paragraph"},
		{"a ![media:7]() b", "paragraph upload paragraph"},
		{"line one\r\nline two\r\n\r\n# h", "paragraph h1"},
	}
	for _, tc := range cases {
		kids, _ := fromMD(t, tc.md)
		var got []string
		for _, k := range kids {
			n := node(t, k)
			switch typeOf(n) {
			case "heading":
				got = append(got, strOf(n, "tag"))
			case "list":
				got = append(got, "list:"+strOf(n, "listType"))
			default:
				got = append(got, typeOf(n))
			}
		}
		if s := strings.Join(got, " "); s != tc.want {
			t.Errorf("%q: got %q, want %q", tc.md, s, tc.want)
		}
	}
}

func TestFromMarkdownLists(t *testing.T) {
	kids, _ := fromMD(t, "- a\n- b\n  - c\n  - d\n    1. e\n- f")
	top := node(t, kids[0])
	items := childrenOf(top)
	// a(1) b(2) wrapper(3) f(3): the wrapper takes the next value without
	// advancing it, as lexical's $updateChildrenListItemValue does.
	wantValues := []int{1, 2, 3, 3}
	if len(items) != 4 {
		t.Fatalf("top items = %d: %s", len(items), mustJSONString(items))
	}
	for i, it := range items {
		v, _ := intOf(node(t, it), "value")
		if v != wantValues[i] {
			t.Errorf("item %d value = %d, want %d", i, v, wantValues[i])
		}
	}
	wrapper := node(t, items[2])
	nested := node(t, childrenOf(wrapper)[0])
	if typeOf(nested) != "list" || strOf(nested, "listType") != "bullet" {
		t.Fatalf("wrapper holds %s", mustJSONString(wrapper))
	}
	if ind, _ := intOf(node(t, childrenOf(nested)[0]), "indent"); ind != 1 {
		t.Errorf("nested item indent = %d, want 1", ind)
	}
	if ind, _ := intOf(nested, "indent"); ind != 0 {
		t.Errorf("nested list indent = %d, want 0 (the stage stores 0)", ind)
	}
	deep := node(t, childrenOf(node(t, childrenOf(nested)[2]))[0])
	if strOf(deep, "listType") != "number" || strOf(deep, "tag") != "ol" {
		t.Errorf("deep list = %s", mustJSONString(deep))
	}
	if ind, _ := intOf(node(t, childrenOf(deep)[0]), "indent"); ind != 2 {
		t.Errorf("deep item indent = %d, want 2", ind)
	}

	kids, _ = fromMD(t, "3. c\n4. d")
	l := node(t, kids[0])
	if s, _ := intOf(l, "start"); s != 3 {
		t.Errorf("start = %d, want 3", s)
	}
	if v, _ := intOf(node(t, childrenOf(l)[1]), "value"); v != 4 {
		t.Errorf("second value = %d, want 4", v)
	}

	kids, _ = fromMD(t, "- [ ] open\n- [x] done\n- [X] upper")
	l = node(t, kids[0])
	if strOf(l, "listType") != "check" || strOf(l, "tag") != "ul" {
		t.Fatalf("check list = %s", mustJSONString(l))
	}
	for i, want := range []bool{false, true, true} {
		if got, _ := node(t, childrenOf(l)[i])["checked"].(bool); got != want {
			t.Errorf("item %d checked = %v", i, got)
		}
	}

	kids, issues := fromMD(t, "- first para\n\n  second para\n- next")
	if got := inlineSig(childrenOf(node(t, childrenOf(node(t, kids[0]))[0]))); got != "0:first para | linebreak | 0:second para" {
		t.Errorf("joined item = %s", got)
	}
	if !hasIssue(issues, IssueItemParagraphsJoined) {
		t.Errorf("no %s issue: %v", IssueItemParagraphsJoined, issues)
	}
}

func TestFromMarkdownQuotes(t *testing.T) {
	kids, _ := fromMD(t, "> one *two*\\\n> three")
	q := node(t, kids[0])
	if got := inlineSig(childrenOf(q)); got != "0:one  | 2:two | linebreak | 0:three" {
		t.Errorf("native quote = %s", got)
	}
	kids, _ = fromMD(t, "> a\n>\n> b")
	q = node(t, kids[0])
	if len(childrenOf(q)) != 2 || typeOf(node(t, childrenOf(q)[0])) != "paragraph" {
		t.Errorf("multi-paragraph quote = %s", mustJSONString(q))
	}
}

func TestFromMarkdownIssues(t *testing.T) {
	cases := []struct {
		md   string
		code string
	}{
		{"<div>x</div>", IssueHTMLAsText},
		{"a <span class=\"c\">b</span>", IssueHTMLAsText},
		{"<!-- note -->", IssueCommentDropped},
		{"a <!-- inline note --> b", IssueCommentDropped},
		{"![alt](https://x.io/a.png)", IssueImageAsText},
		{"```\ncode\n```", IssueCodeBlockFlattened},
		{"| a | b |\n| --- | --- |\n| 1 | 2 |", IssueTableAsText},
		{"[x](../brief.md)", IssueLinkURLRelative},
		{"[x](www.example.com)", IssueLinkURLRelative},
		{"[x]()", IssueLinkURLInvalid},
		{"[x](<a b>)", IssueLinkURLInvalid},
	}
	for _, tc := range cases {
		_, issues := fromMD(t, tc.md)
		if !hasIssue(issues, tc.code) {
			t.Errorf("%q: want issue %s, got %v", tc.md, tc.code, issues)
		}
	}
	for _, md := range []string{"<!-- -->", "<!-- pay:field layout[0].richText -->\n\nx", "[x](/über-uns)", "[x](?q=1)", "[x](https://klixpert.io/prozess/)", "[x](pay:pages/1)", "[x](mailto:a@b.co)", "[x](#anchor)"} {
		if _, issues := fromMD(t, md); len(issues) > 0 {
			t.Errorf("%q raised %v", md, issues)
		}
	}
	_, issues := fromMD(t, "\n\n<div>\n")
	if len(issues) != 1 || issues[0].Line != 3 {
		t.Errorf("issue line = %v, want line 3", issues)
	}
}

func hasIssue(issues []Issue, code string) bool {
	for _, is := range issues {
		if is.Code == code {
			return true
		}
	}
	return false
}

func TestFromMarkdownPlaceholders(t *testing.T) {
	kids, _ := fromMD(t, `<!-- lexical:upload {"relationTo":"media","value":12} -->`)
	up := node(t, kids[0])
	if mustJSONString(up) != `{"fields":null,"format":"","relationTo":"media","type":"upload","value":12,"version":3}` {
		t.Errorf("upload defaults = %s", mustJSONString(up))
	}
	kids, _ = fromMD(t, "<!-- lexical:block {\"fields\":{\"blockType\":\"cta\",\n  \"id\":\"a1\"}} -->")
	if mustJSONString(node(t, kids[0])) != `{"fields":{"blockType":"cta","id":"a1"},"format":"","type":"block","version":2}` {
		t.Errorf("multi-line block placeholder = %s", mustJSONString(kids[0]))
	}
	kids, _ = fromMD(t, `before <!-- lexical:inlineBlock {"fields":{"blockType":"x"}} --> after`)
	if got := inlineSig(childrenOf(node(t, kids[0]))); got != "0:before  | inlineBlock | 0: after" {
		t.Errorf("inline placeholder = %s", got)
	}
	kids, _ = fromMD(t, `<!-- lexical:mystery {"a":1,"version":7} -->`)
	if mustJSONString(node(t, kids[0])) != `{"a":1,"type":"mystery","version":7}` {
		t.Errorf("unknown type = %s", mustJSONString(kids[0]))
	}
	for _, bad := range []string{`<!-- lexical:upload {"relationTo": -->`, `<!-- lexical:block [1] -->`, `x <!-- lexical:text {bad} --> y`} {
		if _, err := FromMarkdown(bad); err == nil {
			t.Errorf("%q: want an error for a broken placeholder", bad)
		}
	}
}

func TestStripFrontmatter(t *testing.T) {
	body, fm, ok := StripFrontmatter("---\ntitle: T\ntags: [a]\n---\n\n# Body\n")
	if !ok || fm != "title: T\ntags: [a]" || body != "\n# Body\n" {
		t.Errorf("got ok=%v fm=%q body=%q", ok, fm, body)
	}
	if _, _, ok := StripFrontmatter("# no front matter\n---\n"); ok {
		t.Error("front matter found where there is none")
	}
	if _, _, ok := StripFrontmatter("---\n\nnot closed"); ok {
		t.Error("an unclosed block is not front matter")
	}
}

func TestInvalidUTF8IsReplaced(t *testing.T) {
	kids, _ := fromMD(t, "a\xffb")
	if got := inlineSig(childrenOf(node(t, kids[0]))); got != "0:a�b" {
		t.Errorf("got %q", got)
	}
}

// pay:COLLECTION/FIELD=VALUE survives the round trip, and a bare "=" is not
// a reference.
func TestRefLinkRoundTrip(t *testing.T) {
	md := "See [about](pay:pages/slug=about) and [x](pay:pages/41).\n"
	state, err := FromMarkdown(md)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ToMarkdown(state)
	if err != nil {
		t.Fatal(err)
	}
	if back != md {
		t.Errorf("round trip:\n got %q\nwant %q", back, md)
	}
	kids, _ := fromMD(t, "[a](pay:pages/=about)")
	if got := inlineSig(childrenOf(node(t, kids[0]))); strings.Contains(got, "$ref") {
		t.Errorf("an empty field must not become a reference: %s", got)
	}
}
