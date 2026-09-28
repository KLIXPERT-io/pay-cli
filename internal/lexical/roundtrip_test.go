package lexical

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMarkdownCorpus converts hand-written Markdown (a migration-source style
// brief with front matter, and a kitchen sink of every construct) and pins
// the exact editor state, the findings, and the normalised Markdown ToMarkdown
// writes back. md→lexical→md→lexical must be stable.
func TestMarkdownCorpus(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "md", "*.md"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no corpus: %v", err)
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".md")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			body, _, _ := StripFrontmatter(string(raw))
			state, issues, err := FromMarkdownIssues(body)
			if err != nil {
				t.Fatal(err)
			}
			b, err := json.MarshalIndent(state, "", " ")
			if err != nil {
				t.Fatal(err)
			}
			golden(t, "md-"+name+".state.json", string(b)+"\n")
			var sb strings.Builder
			for _, is := range issues {
				sb.WriteString(is.Code + "\tline " + intStr(is.Line) + "\t" + is.Message + "\n")
			}
			golden(t, "md-"+name+".issues", sb.String())
			md := assertLexicalRoundTrip(t, state)
			golden(t, "md-"+name+".roundtrip.md", md)
			// The normalised Markdown is itself a fixed point.
			again, err := FromMarkdown(md)
			if err != nil {
				t.Fatal(err)
			}
			if mustJSONString(canonState(again)) != mustJSONString(canonState(state)) {
				t.Error("the round-tripped Markdown reads to different content")
			}
		})
	}
}

func intStr(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

// TestFromMarkdownExactShape pins one complete editor state byte for byte:
// the shape Payload accepted and stored unchanged in the live check (a post
// created on the local instance from FromMarkdown output read back equal).
func TestFromMarkdownExactShape(t *testing.T) {
	state := mustFrom(t, "## Hi\n\nA **b** [l](pay:pages/4).")
	want := `{"root":{"children":[` +
		`{"children":[{"detail":0,"format":0,"mode":"normal","style":"","text":"Hi","type":"text","version":1}],"direction":"ltr","format":"","indent":0,"tag":"h2","type":"heading","version":1},` +
		`{"children":[{"detail":0,"format":0,"mode":"normal","style":"","text":"A ","type":"text","version":1},` +
		`{"detail":0,"format":1,"mode":"normal","style":"","text":"b","type":"text","version":1},` +
		`{"detail":0,"format":0,"mode":"normal","style":"","text":" ","type":"text","version":1},` +
		`{"children":[{"detail":0,"format":0,"mode":"normal","style":"","text":"l","type":"text","version":1}],"direction":"ltr","fields":{"doc":{"relationTo":"pages","value":4},"linkType":"internal","newTab":false},"format":"","indent":0,"type":"link","version":3},` +
		`{"detail":0,"format":0,"mode":"normal","style":"","text":".","type":"text","version":1}],` +
		`"direction":"ltr","format":"","indent":0,"textFormat":0,"textStyle":"","type":"paragraph","version":1}],` +
		`"direction":"ltr","format":"","indent":0,"type":"root","version":1}}`
	if got := mustJSONString(state); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}
