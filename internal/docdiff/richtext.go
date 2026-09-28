package docdiff

import "github.com/KLIXPERT-io/pay-cli/internal/lexical"

// Editor-state detection and the plain-text rendering are internal/lexical's,
// so the diff and every other rich-text feature agree on what counts as rich
// text and on how it reads. The diff uses the placeholder variant: a reader of
// a change list must see a swapped image or block, which lexical.PlainText
// (reading text) drops.

// isEditorState reports a Lexical editor state as Payload's richText field
// stores it (lexical.IsEditorState).
func isEditorState(v any) bool { return lexical.IsEditorState(v) }

// plainText renders an editor state for from_text/to_text and the
// {"$richtext": …} marker: lexical.PlainTextWithPlaceholders — blocks
// separated by a blank line, list items by a newline, and every node without
// text of its own as a bracketed placeholder ([upload media:12], [block cta],
// --- for a rule). Anything that is not an editor state renders as "".
func plainText(v any) string { return lexical.PlainTextWithPlaceholders(v) }
