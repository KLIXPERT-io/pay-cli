package lexical

import (
	"strings"
	"testing"
)

func TestPlainText(t *testing.T) {
	cases := []struct{ md, want string }{
		{"# Title\n\nFirst **bold** [link](/x).\n\nSecond", "Title\n\nFirst bold link.\n\nSecond"},
		{"- a\n- b\n  - c\n- d", "a\nb\nc\nd"},
		{"line\\\nbreak and\ttab", "line\nbreak and\ttab"},
		{"> quoted\n\nafter", "quoted\n\nafter"},
		{"before\n\n---\n\n![media:1]()\n\n<!-- lexical:block {\"fields\":{\"blockType\":\"x\"}} -->\n\nafter", "before\n\nafter"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := PlainText(mustFrom(t, tc.md)); got != tc.want {
			t.Errorf("PlainText(%q) = %q, want %q", tc.md, got, tc.want)
		}
	}
	if PlainText("not a state") != "" || PlainText(nil) != "" {
		t.Error("a non-state must give no text")
	}
}

func TestPlainTextTable(t *testing.T) {
	state := loadState(t, "article.json")
	got := PlainText(state)
	for _, want := range []string{"Kennzahl | Richtwert", "Ziel festlegen\nWelche Kennzahl beschreibt Erfolg?\nDatenquellen prüfen\nCRM-Status\nFormular-Events\nReporting aufsetzen"} {
		if !strings.Contains(got, want) {
			t.Errorf("PlainText lacks %q:\n%s", want, got)
		}
	}
}

func TestIsEditorStateAndWalk(t *testing.T) {
	state := mustFrom(t, "# H\n\n- a **b**\n\n![media:1]()")
	if !IsEditorState(state) {
		t.Fatal("FromMarkdown output is not an editor state")
	}
	for _, v := range []any{nil, 1, "x", []any{}, map[string]any{"root": "x"},
		map[string]any{"root": map[string]any{"type": "root"}},
		map[string]any{"root": map[string]any{"type": "paragraph", "children": []any{}}}} {
		if IsEditorState(v) {
			t.Errorf("IsEditorState(%v) = true", v)
		}
	}
	var paths []string
	Walk(state, func(n Node, path string) bool {
		paths = append(paths, typeOf(n)+"@"+path)
		return typeOf(n) != "list" // skip list children
	})
	want := []string{"root@root", "heading@root.children[0]", "text@root.children[0].children[0]",
		"list@root.children[1]", "upload@root.children[2]"}
	if mustJSONString(paths) != mustJSONString(want) {
		t.Errorf("Walk = %v\nwant   %v", paths, want)
	}
	counts := NodeTypes(state)
	if counts["text"] != 3 || counts["listitem"] != 1 || counts["root"] != 1 {
		t.Errorf("NodeTypes = %v", counts)
	}
}

// PlainTextWithPlaceholders renders every text-less node so a reader (a diff,
// an outline) sees a swapped image or block; separators are PlainText's.
func TestPlainTextWithPlaceholders(t *testing.T) {
	state := decodeState(t, `{"root":{"type":"root","version":1,"children":[
	  {"type":"heading","tag":"h2","children":[{"type":"text","text":"Titel","format":1}]},
	  {"type":"paragraph","children":[
	    {"type":"text","text":"Ein "},
	    {"type":"link","fields":{"url":"https://x","linkType":"custom"},"children":[{"type":"text","text":"Link"}]},
	    {"type":"text","text":" und"},{"type":"linebreak"},{"type":"text","text":"Umbruch"},{"type":"tab"},{"type":"text","text":"."}]},
	  {"type":"list","listType":"bullet","children":[
	    {"type":"listitem","value":1,"children":[{"type":"text","text":"eins"}]},
	    {"type":"listitem","value":2,"children":[{"type":"text","text":"zwei"}]}]},
	  {"type":"quote","children":[{"type":"text","text":"Zitat"}]},
	  {"type":"horizontalrule"},
	  {"type":"upload","relationTo":"media","value":12,"fields":null},
	  {"type":"relationship","relationTo":"pages","value":{"id":47,"title":"x"}},
	  {"type":"block","fields":{"blockType":"cta","id":"b"}},
	  {"type":"block","fields":{}},
	  {"type":"paragraph","children":[{"type":"text","text":"vor "},{"type":"inlineBlock","fields":{"blockType":"badge"}},{"type":"text","text":" nach"}]},
	  {"type":"paragraph","children":[{"type":"inlineBlock","fields":{}}]},
	  {"type":"paragraph","children":[{"type":"mention","version":1}]},
	  {"type":"paragraph","children":[]}
	]}}`)
	want := "Titel\n\nEin Link und\nUmbruch\t.\n\neins\nzwei\n\nZitat\n\n---\n\n[upload media:12]\n\n[relationship pages:47]\n\n[block cta]\n\n[block]\n\nvor [inlineBlock badge] nach\n\n[inlineBlock]\n\n[mention]"
	if got := PlainTextWithPlaceholders(state); got != want {
		t.Fatalf("PlainTextWithPlaceholders =\n%q\nwant\n%q", got, want)
	}
	// PlainText of the same state has none of the placeholders.
	if got := PlainText(state); strings.Contains(got, "[") || strings.Contains(got, "---") {
		t.Fatalf("PlainText rendered a placeholder: %q", got)
	}
	if PlainTextWithPlaceholders("not a state") != "" || PlainTextWithPlaceholders(nil) != "" {
		t.Fatal("a non-state must give no text")
	}
	for _, tc := range []struct {
		node Node
		want string
	}{
		{Node{}, "?"},
		{Node{"value": "abc"}, "abc"},
		{Node{"relationTo": "media"}, "media"},
		{Node{"relationTo": "media", "value": "6ab8"}, "media:6ab8"},
	} {
		if got := relationRef(tc.node); got != tc.want {
			t.Errorf("relationRef(%v) = %q, want %q", tc.node, got, tc.want)
		}
	}
	// Markdown in, placeholders out: an upload and a block from from-md.
	md := mustFrom(t, "Text\n\n![media:4]()\n\n<!-- lexical:block {\"fields\":{\"blockType\":\"banner\"}} -->")
	if got := PlainTextWithPlaceholders(md); got != "Text\n\n[upload media:4]\n\n[block banner]" {
		t.Fatalf("from-md state = %q", got)
	}
}
