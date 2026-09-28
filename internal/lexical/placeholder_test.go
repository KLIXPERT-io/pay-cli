package lexical

import "testing"

func TestPlaceholderCodec(t *testing.T) {
	cases := []struct {
		node Node
		want string
	}{
		{Node{"fields": nil, "format": "", "relationTo": "media", "type": "upload", "value": 40, "version": 3},
			`<!-- lexical:upload {"relationTo":"media","value":40} -->`},
		{Node{"format": "center", "relationTo": "media", "type": "upload", "value": 40, "version": 3},
			// An absent `fields` reads back as null, the value the editor stores.
			`<!-- lexical:upload {"format":"center","relationTo":"media","value":40} -->`},
		{Node{"format": "", "relationTo": "posts", "type": "relationship", "value": map[string]any{"id": 3, "title": "T"}, "version": 2},
			`<!-- lexical:relationship {"relationTo":"posts","value":3} -->`},
		{Node{"fields": map[string]any{"blockType": "cta"}, "type": "inlineBlock", "version": 1},
			`<!-- lexical:inlineBlock {"fields":{"blockType":"cta"}} -->`},
		{newParagraph(nil), `<!-- lexical:paragraph -->`},
		{Node{"type": "mystery", "version": 1, "x": "<!-- -->"},
			`<!-- lexical:mystery {"version":1,"x":"\u003c!-- --\u003e"} -->`},
	}
	for _, tc := range cases {
		got, err := encodePlaceholder(tc.node)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("encode %s\n got  %s\n want %s", mustJSONString(tc.node), got, tc.want)
		}
		back, ok, err := decodePlaceholder(got)
		if err != nil || !ok {
			t.Fatalf("decode %s: ok=%v err=%v", got, ok, err)
		}
		// Encoding again is stable: defaults restored on decode are
		// stripped again on encode.
		again, _ := encodePlaceholder(back)
		if again != got {
			t.Errorf("not stable: %s -> %s", got, again)
		}
	}
	if _, ok, _ := decodePlaceholder("<!-- a plain comment -->"); ok {
		t.Error("a plain comment decoded as a placeholder")
	}
}
