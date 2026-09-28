package lexical

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// golden compares got with testdata/golden/<name>, or rewrites it under
// UPDATE_GOLDEN=1.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if payloadtest.Updating() {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (run with UPDATE_GOLDEN=1 to create it)", path, err)
	}
	if string(want) != got {
		t.Errorf("%s differs (UPDATE_GOLDEN=1 rewrites it)\n--- want ---\n%s\n--- got ---\n%s", path, want, got)
	}
}

func loadState(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "golden", name))
	if err != nil {
		t.Fatal(err)
	}
	return decodeState(t, string(b))
}

// TestToMarkdownGolden renders real-world-shaped editor states (the node
// shapes and attribute mixes found on the klixpert stage, with neutral text)
// and pins both the Markdown and the reported findings. Every fixture must
// also survive lexical→md→lexical.
func TestToMarkdownGolden(t *testing.T) {
	for _, name := range []string{"article", "formats", "links", "lists", "lossy"} {
		t.Run(name, func(t *testing.T) {
			state := loadState(t, name+".json")
			md, issues, err := ToMarkdownIssues(state)
			if err != nil {
				t.Fatal(err)
			}
			golden(t, name+".md", md)
			var sb strings.Builder
			for _, is := range issues {
				sb.WriteString(is.Code + "\t" + is.Path + "\t" + is.Message + "\n")
			}
			golden(t, name+".issues", sb.String())
			assertLexicalRoundTrip(t, state)
		})
	}
}

func TestToMarkdownRejectsNonState(t *testing.T) {
	for _, v := range []any{nil, "x", map[string]any{}, map[string]any{"root": map[string]any{"type": "paragraph", "children": []any{}}},
		map[string]any{"root": map[string]any{"type": "root"}}} {
		if _, err := ToMarkdown(v); !errors.Is(err, ErrNotEditorState) {
			t.Errorf("ToMarkdown(%v) = %v, want ErrNotEditorState", v, err)
		}
	}
	md, err := ToMarkdown(map[string]any{"root": map[string]any{"type": "root", "children": []any{}}})
	if err != nil || md != "" {
		t.Errorf("empty state = %q, %v", md, err)
	}
}

// TestToMarkdownSpellings pins the Markdown chosen for individual shapes.
func TestToMarkdownSpellings(t *testing.T) {
	st := func(children ...any) map[string]any { return map[string]any{"root": newRoot(children)} }
	cases := []struct {
		name  string
		state map[string]any
		want  string
	}{
		{"simple upload uses Payload's own form", st(Node{"fields": nil, "format": "", "relationTo": "media", "type": "upload", "value": 40, "version": 3}),
			"![media:40]()\n"},
		{"populated upload value is reduced to its id", st(Node{"fields": nil, "format": "", "relationTo": "media", "type": "upload", "value": map[string]any{"id": 9, "url": "/x.png"}, "version": 3}),
			"![media:9]()\n"},
		{"relationship placeholder", st(Node{"format": "", "relationTo": "posts", "type": "relationship", "value": 3, "version": 2}),
			"<!-- lexical:relationship {\"relationTo\":\"posts\",\"value\":3} -->\n"},
		{"empty paragraph placeholder", st(newParagraph(nil)),
			"<!-- lexical:paragraph -->\n"},
		{"html-looking text in a placeholder cannot close the comment", st(Node{"type": "block", "version": 2, "format": "", "fields": map[string]any{"blockType": "x", "note": "a --> b <c>"}}),
			"<!-- lexical:block {\"fields\":{\"blockType\":\"x\",\"note\":\"a --\\u003e b \\u003cc\\u003e\"}} -->\n"},
		{"empty heading", st(newHeading(3, nil)), "###\n"},
		{"empty quote", st(newQuote(nil)), ">\n"},
		{"adjacent bullet and check lists alternate markers",
			st(newList("bullet", 1, []any{newListItem(1, nil, 0, []any{newText("a", 0)})}),
				newList("check", 1, []any{newListItem(1, boolPtr(true), 0, []any{newText("b", 0)})})),
			"- a\n\n* [x] b\n"},
		{"adjacent ordered lists alternate delimiters",
			st(newList("number", 1, []any{newListItem(1, nil, 0, []any{newText("a", 0)})}),
				newList("number", 1, []any{newListItem(1, nil, 0, []any{newText("b", 0)})})),
			"1. a\n\n1) b\n"},
		{"bold ending in punctuation before a letter falls back to a tag, only for that run",
			st(newParagraph([]any{newText("Tier 1 (", 1), newText("0,4 %", 0), newText("):", 1), newText(" rest", 0)})),
			"<strong>Tier 1 (</strong>0,4 %**):** rest\n"},
		{"whitespace moves outside bold but stays inside underline",
			st(newParagraph([]any{newText("a", 0), newText(" b ", 1), newText("c", 0), newText(" d ", 8), newText("e", 0)})),
			"a **b** c<u> d </u>e\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md, err := ToMarkdown(tc.state)
			if err != nil {
				t.Fatal(err)
			}
			if md != tc.want {
				t.Errorf("got  %q\nwant %q", md, tc.want)
			}
			assertLexicalRoundTrip(t, tc.state)
		})
	}
}

func boolPtr(b bool) *bool { return &b }

// TestToMarkdownStageShapes runs the non-native shapes found in the stage's
// imported content through the writer: a quote holding a paragraph and loose
// inline nodes, a paragraph holding an upload with a pending import, a list
// item holding text and a nested list.
func TestToMarkdownStageShapes(t *testing.T) {
	mixedQuote := newQuote([]any{newParagraph([]any{newText("Quote", 0)}), newParagraph(nil), newText("– Author", 0)})
	pending := Node{"format": "", "pending": map[string]any{"formID": "6aa2", "src": "https://lh7.example/img"}, "type": "upload", "version": 3}
	uploadPara := newParagraph([]any{pending, newText("Caption", 1)})
	item := newListItem(1, nil, 0, []any{newText("parent", 0), newList("bullet", 1, []any{newListItem(1, nil, 1, []any{newText("child", 0)})})})
	state := map[string]any{"root": newRoot([]any{mixedQuote, uploadPara, newList("bullet", 1, []any{item})})}
	md, issues, err := ToMarkdownIssues(state)
	if err != nil {
		t.Fatal(err)
	}
	back, err := FromMarkdown(md)
	if err != nil {
		t.Fatal(err)
	}
	// The mixed quote and the upload paragraph are placeholders and come
	// back exactly; the list item comes back in Lexical's wrapper shape.
	root, _ := rootOf(back)
	kids := childrenOf(root)
	if mustJSONString(kids[0]) != mustJSONString(mixedQuote) {
		t.Errorf("mixed quote changed:\n%s\n%s", mustJSONString(mixedQuote), mustJSONString(kids[0]))
	}
	if mustJSONString(kids[1]) != mustJSONString(uploadPara) {
		t.Errorf("upload paragraph changed:\n%s\n%s", mustJSONString(uploadPara), mustJSONString(kids[1]))
	}
	items := childrenOf(node(t, kids[2]))
	if len(items) != 2 || inlineSig(childrenOf(node(t, items[0]))) != "0:parent" {
		t.Errorf("list item = %s", mustJSONString(items))
	}
	if !hasIssue(issues, IssueNormalized) || !hasIssue(issues, IssuePlaceholder) {
		t.Errorf("issues = %v", issues)
	}
}
