// Package lexical converts between Payload's Lexical rich-text JSON and
// Markdown, and extracts plain text from it.
//
// Payload 3 stores a richText field as a Lexical editor state:
//
//	{"root": {"type": "root", "children": [ …block nodes… ], "direction": "ltr",
//	          "format": "", "indent": 0, "version": 1}}
//
// Writing that shape by hand is the most error-prone thing an agent does
// against a Payload site: the node vocabulary is large, every element carries
// five bookkeeping keys, text formatting is a bitmask, and a link's target
// lives in a nested `fields` object whose shape depends on linkType. Agents
// measured doing a content migration wrote their own Markdown→Lexical builders
// and a Lexical→text parser before they could do anything else. This package
// is that code, written once, against the shapes Payload 3.87 and Lexical 0.41
// store (see the grounding notes on the constructors in nodes.go).
//
// Everything here is pure: no I/O, no clock, no environment. Inputs are the
// generic values encoding/json produces (map[string]any, []any, string,
// float64 or json.Number, bool, nil); outputs are the same kinds, so a result
// marshals straight into a request body.
//
// # API
//
//   - FromMarkdown / FromMarkdownIssues: Markdown → editor state.
//   - ToMarkdown / ToMarkdownIssues: editor state → Markdown.
//   - PlainText: editor state → text.
//   - IsEditorState, Walk, NodeTypes: recognise and traverse a state.
//   - StripFrontmatter: drop a leading YAML block before converting (only a
//     block whose lines look like YAML; a leading rule is content).
//
// # Dialect
//
// CommonMark, restricted to what Lexical can hold, plus a few extensions:
//
//	# … ######            heading h1–h6 (ATX, closing #s allowed; setext = and - too)
//	paragraph             soft line breaks become a space
//	**b** __b__           bold (1)           *i* _i_      italic (2)
//	~~s~~                 strikethrough (4)  (exactly two tildes; "~5 min" stays text)
//	<u>u</u>              underline (8)      `c`          code (16)
//	<sub>x</sub>          subscript (32)     <sup>x</sup> superscript (64)
//	<mark>x</mark>        highlight (128)
//	<strong> <b> <em> <i> <s> <del> <ins> <code> are accepted as well
//	\ at end of line, two trailing spaces, <br>      hard break → linebreak node
//	&#9; or a tab         tab node           \* \_ \[ …  escapes; &amp; &#228; entities
//	[t](https://…)        link, linkType custom
//	[t](pay:pages/47)     link, linkType internal, doc {relationTo:"pages", value:47}
//	                      (all-digit ids are numbers, anything else a string id)
//	[t](pay:pages/slug=about)  the page whose slug is "about": doc.value is
//	                      {"$ref":"pages","slug":"about"}, which a write resolves to its id
//	[t](…){target=_blank} newTab; further `key=value` attributes become link fields,
//	                      `autolink` makes an autolink node
//	[t][ref] [ref][] [ref]  reference links, resolved against `[ref]: url "title"`
//	                      definitions anywhere in the document (the lines vanish)
//	<https://…> <a@b.c>   autolink node (mailto: for an address)
//	- * +  /  1. 1)       bullet / numbered list (start = first number), nested by
//	                      indentation into Lexical's wrapper items
//	- [ ] / - [x]         check list (checked false / true)
//	> quote               one paragraph → native quote (inline children);
//	                      several blocks → quote holding them
//	--- *** ___           horizontal rule (ToMarkdown writes ***, so its output
//	                      never opens with the front matter fence)
//	![media:40]()         upload node (Payload's own Markdown form)
//	<!-- lexical:TYPE {json} -->
//	                      any node, restored exactly: the JSON is the node minus
//	                      `type` and minus the keys at their export default
//
// Code blocks become a paragraph of code-formatted lines, pipe tables stay
// paragraph text, other HTML stays literal text, other comments are dropped,
// and images other than the upload form stay text — each reported as an
// Issue. ToMarkdown writes exactly this dialect; FromMarkdown(ToMarkdown(x))
// equals x for every node except the presentation attributes Markdown cannot
// spell (alignment, indent, inline CSS, three text-transform bits), which are
// reported, and derived bookkeeping (direction, paragraph textFormat, list
// item value/indent, link ids), which Lexical recomputes.
package lexical
