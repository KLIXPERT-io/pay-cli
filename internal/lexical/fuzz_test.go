package lexical

import (
	"math/rand/v2"
	"strings"
	"testing"
)

// The property the whole package is built on: whatever FromMarkdown reads,
// ToMarkdown writes back to Markdown that reads to the same content, and that
// Markdown is a fixed point. `go test -fuzz FuzzMarkdownRoundTrip` explores
// beyond the seeds; the seeds run in every `go test`.
func FuzzMarkdownRoundTrip(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, md string) {
		state, err := FromMarkdown(md)
		if err != nil {
			return // a malformed placeholder is the one documented failure
		}
		assertLexicalRoundTrip(t, state)
	})
}

var fuzzSeeds = []string{
	"",
	"plain",
	"**bold** *it* ***both*** ~~s~~ `c` <u>u</u> <sub>s</sub> <sup>p</sup> <mark>m</mark>",
	"**Tier 1 (**0,4 %**):** text",
	"*a***b**",
	"foo*bar*baz and snake_case_word and _under_ and __strong__",
	"[a](b) [c](<d e>) [f](g \"title\") [h](pay:pages/1){target=_blank} <https://x.y> <a@b.co>",
	"1. a\n2. b\n\n* c\n* d\n\n- [ ] e\n- [x] f",
	"> q\n>\n> r",
	"# h1\n## h2 ##\nSetext\n===",
	"a  \nb\\\nc<br>d",
	"&amp; &copy; &#9; &#32;lead",
	"<!-- lexical:upload {\"relationTo\":\"media\",\"value\":1} -->",
	"text ![media:2]() more",
	"```\ncode\n```\n\n    indented",
	"- a\n  - b\n    - c\n- d",
	"\\# not heading\n\n1\\. not list\n\n\\- not bullet",
	"`` a ` b ``",
	"x\n---\ny",
	"~single~ ~~double~~ ~~~triple~~~",
	"[link *with* `code` **and bold**](http://a.b/(c))",
	"<div>html</div> <span a=\"b\">x</span>",
	"!\\[not image\\] and ![img](x.png)",
	"a\tb\t\tc",
	"* * *\n- - -\n___",
	"text {not attrs} [l](u){rel=nofollow data-x='{\"a\":1}' autolink}",
	// list items holding quotes with block content (fix2-richtext)
	"0) > \t0",
	"- > # Titel\n",
	"- > a\n  >\n  > b\n",
	"- > a\n  > - b\n",
}

// TestRandomInlineRoundTrip builds random paragraphs from the characters and
// formats that make Markdown hard — delimiters, punctuation next to
// formatting, leading/trailing spaces, line breaks, links — and requires every
// one to survive lexical→md→lexical. It is deterministic (fixed seed).
func TestRandomInlineRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(20260928, 1))
	pieces := []string{"a", "b", "Wort", " ", "  ", "*", "**", "_", "~", "~~", "`", "``", "[", "]", "(", ")",
		"<", ">", "&", "&amp;", "#", "-", "+", "1.", "!", "\\", "{", "}", "|", ".", ",", ":", "„", "“", "ä", "–",
		" ", "https://x.y", "<u>", "</b>", "=", "\"", "'"}
	formats := []int{0, 0, 0, FormatBold, FormatItalic, FormatBold | FormatItalic, FormatStrikethrough,
		FormatCode, FormatUnderline, FormatSubscript, FormatSuperscript, FormatHighlight, FormatBold | FormatCode,
		FormatItalic | FormatUnderline}
	randText := func() string {
		var sb strings.Builder
		for n := 1 + rng.IntN(4); n > 0; n-- {
			sb.WriteString(pieces[rng.IntN(len(pieces))])
		}
		return sb.String()
	}
	for i := 0; i < 3000; i++ {
		var kids []any
		for n := 1 + rng.IntN(6); n > 0; n-- {
			switch r := rng.IntN(10); {
			case r < 7:
				kids = append(kids, newText(randText(), formats[rng.IntN(len(formats))]))
			case r == 7:
				kids = append(kids, newLinebreak())
			case r == 8:
				kids = append(kids, newLink(linkTarget{url: "https://ex.com/" + randText(), newTab: rng.IntN(2) == 0},
					[]any{newText(randText(), formats[rng.IntN(len(formats))])}))
			default:
				kids = append(kids, newTab(0))
			}
		}
		state := map[string]any{"root": newRoot([]any{newParagraph(kids)})}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on %s: %v", mustJSON(t, state), r)
				}
			}()
			md := assertLexicalRoundTrip(t, state)
			if strings.Contains(md, "<!-- lexical:paragraph") && !hasEmptyText(kids) {
				// Not a failure — the placeholder is lossless — but it is the
				// last resort and should be rare; count it.
				placeholderFallbacks++
			}
		}()
	}
	if placeholderFallbacks > 3 {
		t.Errorf("%d of 3000 random paragraphs needed the placeholder fallback", placeholderFallbacks)
	}
}

var placeholderFallbacks int

func hasEmptyText(kids []any) bool {
	for _, k := range kids {
		if n, ok := k.(map[string]any); ok && typeOf(n) == "text" && strOf(n, "text") == "" {
			return true
		}
	}
	return false
}
