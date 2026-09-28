package docdiff

import "testing"

func TestIsEditorState(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{richText("x"), true},
		{`{"root":{"type":"root","children":[]}}`, true},
		{`{"root":{"type":"paragraph"}}`, false},
		{`{"root":"root"}`, false},
		{`{"type":"root"}`, false},
		{`"root"`, false},
	} {
		if got := isEditorState(decode(t, tc.in)); got != tc.want {
			t.Errorf("isEditorState(%s) = %v", tc.in, got)
		}
	}
}

// The node shapes are Payload 3.87's lexical editor as stored on klixpert
// stage and localhost:3900 (paragraph/heading/list/listitem/quote/link/
// linebreak/upload/block). The rendering is lexical.PlainTextWithPlaceholders
// (it replaced a private walker that put blocks on single lines and prefixed
// list items with "- "): blocks are separated by a blank line, and every node
// without text of its own is still visible as a placeholder.
func TestPlainText(t *testing.T) {
	state := `{"root":{"type":"root","version":1,"children":[
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
	  {"type":"paragraph","children":[{"type":"inlineBlock","fields":{}}]},
	  {"type":"paragraph","children":[]}
	]}}`
	want := "Titel\n\nEin Link und\nUmbruch\t.\n\neins\nzwei\n\nZitat\n\n---\n\n[upload media:12]\n\n[relationship pages:47]\n\n[block cta]\n\n[inlineBlock]"
	if got := plainText(decode(t, state)); got != want {
		t.Fatalf("plainText =\n%q\nwant\n%q", got, want)
	}
	if plainText("not a state") != "" || plainText(nil) != "" {
		t.Fatal("non-states render empty")
	}
}
