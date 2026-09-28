package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/lexical"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/rows"
)

// Rich text in the everyday commands (§9.12.8, F2 stage B).
//
// `pay lexical` converts rich text on its own; this file wires the same
// conversion into the commands an agent already uses, in both directions:
//
//   - WRITE: `--set-md FIELD=@file.md` (or `FIELD=markdown`) on create,
//     update, globals update, upload and `pay blocks add|set|new` — the
//     Markdown becomes the exact editor state `pay lexical from-md` produces,
//     with no intermediate file or pipe.
//   - READ: `--richtext md|text` on get, find, globals get and versions get
//     replaces every editor state in the data with its Markdown or plain text,
//     so "what does this page say" is one read instead of a read and a jq walk.
//
// A rendered document is for reading only. A string in a richText field is
// not rejected by Payload — richTextValidate only walks `value.root.children`
// when it exists — so writing one back would silently replace the field with
// text the admin editor cannot open. Every write path therefore refuses a
// document whose envelope says it was rendered (meta.richtext), and a bare
// document whose declared richText field holds a string.

// Rich-text read formats.
const (
	richTextJSON = "json"
	richTextMD   = "md"
	richTextText = "text"
)

// Warning codes this file raises.
const (
	// warnSetMDNotRichText: --set-md targets a key that currently holds a
	// value that is not rich text.
	warnSetMDNotRichText = "set_md_not_richtext"
	// warnRichTextAsString: a block row's field the census knows as rich text
	// holds a string (observed fact → warning, never an error; §7.10c).
	warnRichTextAsString = "richtext_as_string"
)

// ---------------------------------------------------------------------------
// --set-md
// ---------------------------------------------------------------------------

// setMDFlagUsage is the one-line flag description every command shares.
const setMDFlagUsage = "k=@file.md or k=markdown: a richText field from Markdown (repeatable; applied last, after --set-json)"

// setMDGrammar documents --set-md in help.
const setMDGrammar = "KEY=@FILE.md | KEY=@- | KEY=MARKDOWN"

// markdownValue is one --set-md flag, converted.
type markdownValue struct {
	Key   string
	State map[string]any
	// Source is "inline", "stdin" or the file name, for messages.
	Source string
}

// mdSource says where --set-md values may be read from.
type mdSource struct {
	// flag is the flag name used in messages ("--set-md").
	flag string
	// stdinFree is false when stdin already carries something else — the
	// document of a `pay blocks` stage, or `--data @-` of the same command.
	stdinFree bool
	// stdinTakenBy names what holds stdin, for the refusal message.
	stdinTakenBy string
}

// convertSetMD reads and converts every --set-md pair. Findings of the
// conversion become lexical_* warnings whose paths name the key and the
// Markdown line ("content line 12").
func convertSetMD(d *Deps, pairs []string, src mdSource) ([]markdownValue, []output.Warning, error) {
	var out []markdownValue
	var warns []output.Warning
	stdinUsed := false
	for _, pair := range pairs {
		key, raw, ok := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, nil, apierr.New(apierr.CodeInvalidArgs, "%s %q is not key=@file.md or key=markdown", src.flag, pair).
				WithHint("e.g. %s content=@body.md, or %s 'content=## Title'", src.flag, src.flag)
		}
		md, source, err := readMarkdownArg(d, src, key, raw, &stdinUsed)
		if err != nil {
			return nil, nil, err
		}
		if source == "inline" {
			if w := literalNewlineWarning(src, key, md); w != nil {
				warns = append(warns, *w)
			}
		}
		lineOffset := 0
		if body, fm, stripped := lexical.StripFrontmatter(md); stripped {
			lineOffset = strings.Count(md[:len(md)-len(body)], "\n")
			md = body
			w := frontmatterWarning(src.flag+" "+key+": ", fm,
				"if those lines are content, not metadata: `pay lexical from-md FILE.md --keep-frontmatter` converts everything; pass its data with --set-json "+key+"=…")
			w.Paths = []string{key}
			warns = append(warns, w)
		}
		if source != "inline" && strings.TrimSpace(md) == "" {
			warns = append(warns, output.Warning{
				Code: warnSetMDEmpty,
				Message: fmt.Sprintf("%s %s: %s holds no Markdown, so the field becomes an empty editor state and its current content is replaced",
					src.flag, key, source),
				Paths: []string{key},
				Hint:  "check the file; to clear the field on purpose, pass " + src.flag + " " + key + "= (empty inline Markdown), which does not warn",
			})
		}
		state, issues, err := lexical.FromMarkdownIssues(md)
		if err != nil {
			return nil, nil, apierr.New(apierr.CodeBadRequestBody, "%s %s (%s): %s", src.flag, key, source, err.Error()).
				WithHint("a <!-- lexical:TYPE {…} --> placeholder must hold one JSON object; copy it unchanged from `pay lexical to-md`")
		}
		warns = append(warns, issueWarnings(issues, func(is lexical.Issue) string {
			if is.Line > 0 {
				return key + " line " + strconv.Itoa(is.Line+lineOffset)
			}
			return key
		})...)
		out = append(out, markdownValue{Key: key, State: state, Source: source})
	}
	return out, warns, nil
}

// warnSetMDEmpty: a --set-md file (or stdin that held only front matter)
// has no Markdown in it, so the field is cleared.
const warnSetMDEmpty = "set_md_empty"

// isStdinFile names a path that reads the process's stdin.
func isStdinFile(name string) bool {
	switch name {
	case "/dev/stdin", "/dev/fd/0", "/proc/self/fd/0":
		return true
	}
	return false
}

// warnFrontmatterStripped: a leading YAML front matter block was not
// converted.
const warnFrontmatterStripped = "frontmatter_stripped"

// frontmatterWarning says what was stripped — how many lines and the first
// one — so a block that was content after all is visible in the warning
// itself, not only in the missing text of the written field.
func frontmatterWarning(prefix, fm, hint string) output.Warning {
	lines := strings.Split(strings.ReplaceAll(fm, "\r\n", "\n"), "\n")
	first := strings.TrimSpace(lines[0])
	if r := []rune(first); len(r) > 60 {
		first = string(r[:60]) + "…"
	}
	return output.Warning{
		Code: warnFrontmatterStripped,
		Message: fmt.Sprintf("%sa leading YAML front matter block (%d line(s) between the --- fences, starting %q) was not converted",
			prefix, len(lines), first),
		Hint: hint,
	}
}

// warnSetMDLiteralNewline: inline --set-md Markdown holds the two characters
// `\n` and no line break.
const warnSetMDLiteralNewline = "set_md_literal_newline"

// literalNewlineWarning catches `--set-md 'content=# Titel\n\nText'`: in
// single quotes the shell passes a backslash and an n, which Markdown keeps
// as text, so the field becomes one heading reading "Titel\n\nText".
func literalNewlineWarning(src mdSource, key, md string) *output.Warning {
	if strings.Contains(md, "\n") || !strings.Contains(md, `\n`) {
		return nil
	}
	flag := src.flag
	hint := fmt.Sprintf("pass real line breaks: %s %s=@file.md or bash $'…' quoting", flag, key)
	if src.stdinFree {
		hint = fmt.Sprintf("pass real line breaks: %s %s=@file.md, a heredoc with %s %s=@-, or bash $'…' quoting", flag, key, flag, key)
	}
	return &output.Warning{
		Code: warnSetMDLiteralNewline,
		Message: fmt.Sprintf("%s %s: the inline Markdown contains the characters \\n but no line break; "+
			"Markdown keeps \\n as text, so the field reads as one block", flag, key),
		Paths: []string{key},
		Hint:  hint,
	}
}

// readMarkdownArg resolves the value half of a --set-md pair: @FILE, @- or
// the Markdown itself. A literal leading @ is written as \@ — Markdown's own
// escape, which the conversion turns back into "@".
func readMarkdownArg(d *Deps, src mdSource, key, raw string, stdinUsed *bool) (string, string, error) {
	switch {
	case raw == "@-" || (strings.HasPrefix(raw, "@") && isStdinFile(strings.TrimPrefix(raw, "@"))):
		if !src.stdinFree {
			return "", "", apierr.New(apierr.CodeInvalidArgs,
				"%s %s=@- cannot read stdin: %s", src.flag, key, src.stdinTakenBy).
				WithHint("put the Markdown in a file and pass %s %s=@file.md", src.flag, key)
		}
		if *stdinUsed {
			return "", "", apierr.New(apierr.CodeInvalidArgs,
				"only one %s value can be read from stdin (@-)", src.flag).
				WithHint("pass the others as files: %s %s=@file.md", src.flag, key)
		}
		*stdinUsed = true
		b, err := io.ReadAll(d.inStream())
		if err != nil {
			return "", "", apierr.Wrap(err, apierr.CodeNoInput, "%s %s=@- could not read stdin", src.flag, key)
		}
		if strings.TrimSpace(string(b)) == "" {
			// A closed or empty stdin (a non-interactive shell, a broken
			// heredoc or pipe) would convert to an empty editor state and
			// silently clear the field — `pay lexical from-md` refuses the
			// same input.
			return "", "", apierr.New(apierr.CodeNoInput,
				"%s %s=@- read nothing from stdin; an empty value would clear the field", src.flag, key).
				WithHint("pipe the Markdown in (%s %s=@- <<'EOF' … EOF) or pass a file: %s %s=@file.md; to clear the field on purpose pass %s %s= (empty inline Markdown)",
					src.flag, key, src.flag, key, src.flag, key)
		}
		return string(b), "stdin", nil
	case strings.HasPrefix(raw, "@"):
		name := strings.TrimPrefix(raw, "@")
		b, err := os.ReadFile(name)
		if err != nil {
			return "", "", apierr.Wrap(err, apierr.CodeFileMissing, "%s %s=@%s could not be read", src.flag, key, name).
				WithHint("pass an existing Markdown file; inline Markdown that starts with @ is written \\@")
		}
		return string(b), name, nil
	}
	return raw, "inline", nil
}

// checkBodyMDKey enforces the write-body key rule for --set-md on create,
// update, globals update and upload: like --set, a dotted key reaches a group
// field (meta.body); an array row cannot be addressed, because a write body
// replaces a whole array and PayCLI does not have the array to edit.
func checkBodyMDKey(t *collTarget, key string) error {
	root, hop, err := bodyKeyArrayHop(key)
	if err != nil {
		return apierr.New(apierr.CodeInvalidArgs, "--set-md %s: %s", key, err.Error()).
			WithHint("name a richText field: content, or meta.body for one inside a group")
	}
	if hop {
		return apierr.New(apierr.CodeInvalidArgs,
			"--set-md %s addresses a row of an array, and a write body can only replace the whole array %s", key, root).
			WithHint("%s", rowPipeHint(targetSlug(t)))
	}
	return nil
}

// bodyKeyArrayHop parses a write-body key (`content`, `meta.body`) and
// reports whether it steps into an array row — a bracket (`layout[0]`,
// `layout[id:…]`) or an all-digit segment (`layout.0`). root is the key's
// first segment. Such a key cannot be turned into a body: setPath would build
// {"layout":{"0":…}}, an object where Payload expects the whole array, and
// Payload empties the array (verified on localhost, Payload 3.87).
func bodyKeyArrayHop(key string) (root string, hop bool, err error) {
	p, err := rows.ParsePath(key)
	if err != nil {
		return "", false, err
	}
	for _, s := range p.Steps {
		if s.Bracket || isAllDigitsString(s.Text) {
			return p.Steps[0].Text, true, nil
		}
	}
	return p.Steps[0].Text, false, nil
}

// rowPipeHint names the pipe that edits rich text inside an array row.
func rowPipeHint(slug string) string {
	return fmt.Sprintf("edit the row in a pipe, where the key is relative to the row: pay get %s <id> --depth 0 --draft | pay blocks set <selector> --set-md <field>=@file.md --field <array> | pay apply --draft; `pay outline %s <id>` prints the --field and selector of every row",
		slug, slug)
}

func isAllDigitsString(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// checkMDField refuses --set-md on a field the schema knows is not rich text:
// an editor state in a text field is either rejected with an opaque message or
// stored as a JSON string. known reports whether the schema could type the
// field at all; an unknown one (a group sub-field on an introspection-disabled
// site, a field no observed document holds) is checked against real values
// by guardMDTargets before anything is sent.
func checkMDField(t *collTarget, shard *discovery.Shard, key string) (known bool, err error) {
	fld, ok := fieldFor(shard, key)
	if !ok {
		return false, nil
	}
	switch fld.PayloadType {
	case discovery.TypeRichText, discovery.TypeJSON:
		return true, nil
	case discovery.TypeUnknown, "":
		return false, nil
	}
	return true, apierr.New(apierr.CodeInvalidArgs,
		"--set-md %s: %s.%s is a field of type %s, and --set-md writes a Lexical editor state, which only a richText field holds", key, targetSlug(t), key, fld.PayloadType).
		WithHint("use --set %s=… for a %s field; `pay describe %s` lists the richText fields", key, fld.PayloadType, targetSlug(t))
}

// warnSetMDUnchecked: --set-md targets a field whose type neither the schema
// nor any stored value could establish.
const warnSetMDUnchecked = "set_md_field_unchecked"

// mdSampleLimit bounds the documents read to type an unknown --set-md field.
const mdSampleLimit = 10

// guardMDTargets checks the --set-md keys the schema could not type
// (writeData.mdUnchecked) against what is stored, because Payload does not
// reject an editor state sent to a text field — it stores the JSON as a
// string (verified on Payload 3.87 / Postgres: meta.description read back as
// "{\"root\":…"), and the site then shows that JSON. The evidence, in order:
//
//  1. the target document's current value (o.id set, or a global): one GET;
//  2. otherwise, or when that value is empty, up to mdSampleLimit other
//     documents of the collection that hold the key (one GET).
//
// A non-empty value that is not an editor state (a string, number, boolean or
// array) refuses the write: the field is not rich text. A non-editor object
// warns (set_md_not_richtext): it may be a json field. No evidence at all
// warns set_md_field_unchecked. A failed read never blocks the write; it
// warns like no evidence.
func (w *writeData) guardMDTargets(ctx context.Context, d *Deps, client *payload.Client, o orphanTarget, p query.Params) ([]output.Warning, error) {
	if len(w.mdUnchecked) == 0 || client == nil {
		return nil, nil
	}
	var doc payload.Doc
	if o.id != "" || o.g != nil {
		doc, _ = d.readOrphanTarget(ctx, client, o, p, true)
	}
	var out []output.Warning
	for _, key := range w.mdUnchecked {
		var v any
		var where string
		if doc != nil {
			if cur, ok := lookupPath(map[string]any(doc), key); ok && !emptyMDValue(cur) {
				v, where = cur, o.what()
			}
		}
		if v == nil && o.t != nil && o.g == nil {
			v, where = d.sampleMDField(ctx, client, o, p, key)
		}
		switch {
		case v == nil:
			out = append(out, output.Warning{
				Code: warnSetMDUnchecked,
				Message: fmt.Sprintf("--set-md %s: the schema PayCLI discovered does not know %s.%s, and no stored value shows its type; "+
					"if it is not a richText field, Payload stores the editor state as a JSON string", key, o.slug(), key),
				Paths: []string{key},
				Hint:  "check the key before writing: `pay describe " + o.slug() + "` lists the known fields; a plain text field takes --set",
			})
		case lexical.IsEditorState(v):
		case isMap(v):
			out = append(out, output.Warning{
				Code: warnSetMDNotRichText,
				Message: fmt.Sprintf("--set-md %s: %s holds an object there that is not rich text; the write replaces it with a Lexical editor state",
					key, where),
				Paths: []string{key},
				Hint:  "check the key: `pay describe " + o.slug() + "` lists the known fields",
			})
		default:
			return nil, mdNotRichTextError(o, key, where, v, p)
		}
	}
	return out, nil
}

func isMap(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

// emptyMDValue is a value that says nothing about a field's type: absent,
// null, or an empty string (Payload's empty text and empty rich text alike).
func emptyMDValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && s == ""
}

// sampleMDField reads up to mdSampleLimit documents that hold key and returns
// the first non-empty value, with where it was found.
func (d *Deps) sampleMDField(ctx context.Context, client *payload.Client, o orphanTarget, p query.Params, key string) (any, string) {
	lim := mdSampleLimit
	sp := query.Params{
		Where:  query.Term(key, "exists", true),
		Limit:  &lim,
		Depth:  query.IntPtr(0),
		Select: []string{key},
		Locale: p.Locale, FallbackLocale: p.FallbackLocale,
	}
	if dr := o.drafts(); dr == nil || *dr {
		sp.Draft = query.BoolPtr(true)
	}
	res, err := client.Find(ctx, o.t.Slug, sp,
		payload.WithClassify(classifyFor(o.t, d.cfg(), nil)), payload.WithKnownRoute())
	if err != nil || res == nil {
		return nil, ""
	}
	for _, doc := range res.Docs {
		if v, ok := lookupPath(map[string]any(doc), key); ok && !emptyMDValue(v) {
			return v, o.t.Slug + " " + doc.IDString()
		}
	}
	return nil, ""
}

// mdNotRichTextError refuses --set-md into a field whose stored value shows
// it is not rich text.
func mdNotRichTextError(o orphanTarget, key, where string, v any, p query.Params) error {
	excerpt := ""
	if s, ok := v.(string); ok {
		r := []rune(s)
		if len(r) > 40 {
			s = string(r[:40]) + "…"
		}
		excerpt = fmt.Sprintf(" (%q)", s)
	}
	fix := "pay lexical from-md FILE.md --as " + key + " | pay create " + o.slug() + " --data @-"
	switch {
	case o.g != nil:
		fix = "pay lexical from-md FILE.md --as " + key + " | pay globals update " + o.slug() + " --data @-"
	case o.id != "":
		fix = "pay lexical from-md FILE.md --as " + key + " | pay update " + o.slug() + " " + shellQuote(o.id) + " --data @-"
	}
	// The bypass must write the same version and locale the refused command
	// named, or a --draft edit would go live.
	if p.Draft != nil && *p.Draft {
		fix += " --draft"
	}
	if p.Locale != "" {
		fix += " --locale " + shellQuote(p.Locale)
	}
	return apierr.New(apierr.CodeInvalidArgs,
		"--set-md %s: %s holds %s there%s, not rich text; the field is not a richText field (the discovered schema does not know it), "+
			"and Payload would store the editor state as a JSON string", key, where, jsonKindOf(v), excerpt).
		WithHint("use --set %s=… for a text field; if %s really is rich text holding a stray value, write the state on purpose: %s",
			key, key, fix)
}

// setLeaf stores value at a dotted object path, creating groups on the way.
// Unlike setPath it never merges: an editor state replaces whatever an earlier
// flag put there, key for key.
func setLeaf(body map[string]any, key string, value any) {
	parts := strings.Split(key, ".")
	cur := body
	for _, p := range parts[:len(parts)-1] {
		next, ok := cur[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
	cur[parts[len(parts)-1]] = value
}

// mdTargetWarning warns when --set-md replaces a value that is not rich text
// — a string heading, a number — which is almost always the wrong key. It is
// a warning, not an error: in a row, the only evidence is the row itself.
func mdTargetWarning(where, key string, existing any) *output.Warning {
	if existing == nil || lexical.IsEditorState(existing) {
		return nil
	}
	if s, ok := existing.(string); ok && s == "" {
		return nil
	}
	return &output.Warning{
		Code: warnSetMDNotRichText,
		Message: fmt.Sprintf("--set-md %s replaced %s, which held %s, not rich text; the key now holds a Lexical editor state",
			key, where, jsonKindOf(existing)),
		Paths: []string{where},
		Hint:  "check the key: `pay blocks schema <blockType>` shows which fields are richText (kind richText); a plain text field takes --set",
	}
}

// ---------------------------------------------------------------------------
// --richtext json|md|text
// ---------------------------------------------------------------------------

// richTextFlag is --richtext on the read commands.
type richTextFlag struct{ format string }

func (f *richTextFlag) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.format, "richtext", richTextJSON,
		"render every rich-text value: json (stored Lexical JSON), md (Markdown) or text (plain text) — md/text output is for reading and cannot be written back")
}

// richTextFlagInfo is the FlagInfo entry for --richtext.
var richTextFlagInfo = FlagInfo{Grammar: "json | md | text",
	Note: "md/text replace every Lexical editor state (at any depth) with a string and record meta.richtext {format, rendered, paths}; " +
		"for reading only: pay apply, pay blocks edits and --data refuse such a document (richtext_rendered, exit 5)"}

// validate checks the value before anything is sent.
func (f *richTextFlag) validate() error {
	switch f.normalized() {
	case richTextJSON, richTextMD, richTextText:
		return nil
	}
	return apierr.New(apierr.CodeInvalidOption, "--richtext %q is not one of json, md, text", f.format).
		WithDidYouMean(apierr.DidYouMean(f.format, []string{richTextJSON, richTextMD, richTextText})...).
		WithHint("json keeps the stored editor state, md renders Markdown, text renders plain text")
}

func (f *richTextFlag) normalized() string {
	s := strings.ToLower(strings.TrimSpace(f.format))
	switch s {
	case "", "lexical":
		return richTextJSON
	case "markdown":
		return richTextMD
	case "plain", "plaintext":
		return richTextText
	}
	return s
}

// active reports a rendering format other than json.
func (f *richTextFlag) active() bool {
	return f != nil && f.normalized() != richTextJSON
}

// apply renders env.Data in place when --richtext asks for it. The raw body
// is dropped so `--output raw` prints the rendered data, not the stored JSON.
func (f *richTextFlag) apply(env *output.Envelope) {
	if env == nil || !f.active() {
		return
	}
	format := f.normalized()
	data, meta, issues := renderRichText(env.Data, format)
	env.Data = data
	env.RawBody = nil
	env.RawRedacted = false
	env.Meta.RichText = meta
	for _, w := range issueWarnings(issues, func(is lexical.Issue) string { return is.Path }) {
		env.AddWarning(w)
	}
}

// renderRichText replaces every editor state in v (at any depth) with its
// Markdown or plain text. Findings that lose information on the way to
// Markdown (lossy_*) are returned; the informational ones (placeholder,
// normalized) are not — on a read they say nothing the reader must act on.
func renderRichText(v any, format string) (any, *output.RichTextMeta, []lexical.Issue) {
	meta := &output.RichTextMeta{Format: format, Paths: []string{}}
	var issues []lexical.Issue
	var walk func(v any, path string) any
	walk = func(v any, path string) any {
		switch t := v.(type) {
		case map[string]any:
			if lexical.IsEditorState(t) {
				meta.Rendered++
				if len(meta.Paths) < output.MaxRichTextPaths {
					meta.Paths = append(meta.Paths, path)
				} else {
					meta.PathsTruncated = true
				}
				if format == richTextText {
					return lexical.PlainText(t)
				}
				md, iss, err := lexical.ToMarkdownIssues(t)
				if err != nil {
					// Not reachable for a value IsEditorState accepted; keep
					// the stored value rather than lose it.
					return t
				}
				for _, is := range iss {
					switch is.Code {
					case lexical.IssuePlaceholder, lexical.IssueNormalized:
						continue
					}
					is.Path = joinFieldPath(path, is.Path)
					issues = append(issues, is)
				}
				return md
			}
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				p := k
				if path != "" {
					p = path + "." + k
				}
				t[k] = walk(t[k], p)
			}
			return t
		case []any:
			for i := range t {
				t[i] = walk(t[i], path+"["+strconv.Itoa(i)+"]")
			}
			return t
		}
		return v
	}
	return walk(genericData(v), ""), meta, issues
}

// genericData turns the typed containers commands put in env.Data (a
// payload.Doc, a []payload.Doc) into plain JSON containers the walk can edit.
// The documents themselves are not copied: a read owns its decoded data.
func genericData(v any) any {
	switch t := v.(type) {
	case nil, map[string]any, []any:
		return v
	case payload.Doc:
		return map[string]any(t)
	case []payload.Doc:
		out := make([]any, len(t))
		for i := range t {
			out[i] = map[string]any(t[i])
		}
		return out
	case []map[string]any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = t[i]
		}
		return out
	}
	// Anything else (a struct) goes through JSON, numbers kept exact.
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return v
	}
	return out
}

// ---------------------------------------------------------------------------
// refusing rendered documents
// ---------------------------------------------------------------------------

// envelopeRichText returns meta.richtext.format of a decoded envelope, "" when
// the envelope does not record a rendering (or records json).
func envelopeRichText(env map[string]any) string {
	meta, _ := env["meta"].(map[string]any)
	rt, _ := meta["richtext"].(map[string]any)
	format, _ := rt["format"].(string)
	if format == richTextJSON {
		return ""
	}
	return format
}

// renderedError is the refusal for a document whose envelope says its rich
// text was rendered. who names the receiver: "pay apply", "--data @-".
func renderedError(who, format string) error {
	return apierr.New(apierr.CodeRichTextRendered,
		"%s was given a document read with --richtext %s: its rich text is %s strings, not Lexical editor states, and writing it back would replace every richText field with text the admin editor cannot open",
		who, format, formatNoun(format)).
		WithHint("re-read it without --richtext (the default is json) and pipe that: pay get <collection> <id> --depth 0 --draft | pay blocks … | pay apply; to write Markdown into a field use --set-md FIELD=@file.md")
}

func formatNoun(format string) string {
	if format == richTextText {
		return "plain-text"
	}
	return "Markdown"
}

// renderedStringFields finds top-level (and group) fields the schema declares
// richText that hold a string in a bare document or body — what a document
// read with --richtext looks like once its envelope is gone (`--output jsonl`,
// a jq extract). Payload would store the string.
func renderedStringFields(body map[string]any, shard *discovery.Shard) []string {
	if shard == nil || len(shard.Fields) == 0 {
		return nil
	}
	var out []string
	var walk func(obj map[string]any, prefix string, depth int)
	walk = func(obj map[string]any, prefix string, depth int) {
		for _, k := range sortedMapKeys(obj) {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			switch v := obj[k].(type) {
			case string:
				if fld, ok := shard.Field(p); ok && fld.PayloadType == discovery.TypeRichText {
					out = append(out, p)
				}
			case map[string]any:
				if depth < 8 && !lexical.IsEditorState(v) {
					walk(v, p, depth+1)
				}
			}
		}
	}
	walk(body, "", 0)
	return out
}

// checkRenderedBody refuses a body whose declared richText fields hold
// strings. who names the command ("pay update").
func checkRenderedBody(who string, t *collTarget, body map[string]any, shard *discovery.Shard) error {
	paths := renderedStringFields(body, shard)
	if len(paths) == 0 {
		return nil
	}
	return apierr.New(apierr.CodeRichTextRendered,
		"%s: %s is a richText field of %s and holds a string; Payload stores a Lexical editor state there (a document read with --richtext md|text, or a hand-written value)",
		who, strings.Join(paths, ", "), targetSlug(t)).
		WithHint("re-read the document without --richtext, or convert Markdown on purpose: --set-md %s=@file.md", paths[0])
}

// rowRichTextStrings warns about block rows (and the array rows and groups
// inside them) whose census-known richText fields hold strings. The census is
// an observation, so this never blocks (§7.10c).
func rowRichTextStrings(body map[string]any, k *discovery.BlockKnowledge) *output.Warning {
	if k == nil || k.Census == nil {
		return nil
	}
	var paths []string
	// withFields checks obj against a known field profile.
	var withFields func(obj map[string]any, fields map[string]*discovery.CensusField, path string, depth int)
	// find walks a value that has no profile, looking for block rows.
	var find func(v any, path string, depth int)
	join := func(path, key string) string {
		if path == "" {
			return key
		}
		return path + "." + key
	}
	blockRow := func(obj map[string]any, path string, depth int) bool {
		bt, _ := obj[rows.KeyBlockType].(string)
		if bt == "" {
			return false
		}
		if stats := k.TypeStats(bt); stats != nil {
			withFields(obj, stats.Fields, path, depth+1)
			return true
		}
		return false
	}
	withFields = func(obj map[string]any, fields map[string]*discovery.CensusField, path string, depth int) {
		if depth > 32 {
			return
		}
		for _, key := range sortedMapKeys(obj) {
			p := join(path, key)
			f := fields[key]
			switch v := obj[key].(type) {
			case string:
				if v != "" && f != nil && f.Kind == discovery.CensusKindRichText {
					paths = append(paths, p)
				}
			case map[string]any:
				if lexical.IsEditorState(v) {
					continue
				}
				if f != nil && len(f.Fields) > 0 {
					withFields(v, f.Fields, p, depth+1)
				} else {
					find(v, p, depth+1)
				}
			case []any:
				for i, x := range v {
					rp := p + "[" + strconv.Itoa(i) + "]"
					row, ok := x.(map[string]any)
					if !ok {
						continue
					}
					if blockRow(row, rp, depth) {
						continue
					}
					if f != nil && len(f.Fields) > 0 {
						withFields(row, f.Fields, rp, depth+1)
					} else {
						find(row, rp, depth+1)
					}
				}
			}
		}
	}
	find = func(v any, path string, depth int) {
		if depth > 32 {
			return
		}
		switch t := v.(type) {
		case map[string]any:
			if lexical.IsEditorState(t) || blockRow(t, path, depth) {
				return
			}
			for _, key := range sortedMapKeys(t) {
				find(t[key], join(path, key), depth+1)
			}
		case []any:
			for i, x := range t {
				find(x, path+"["+strconv.Itoa(i)+"]", depth+1)
			}
		}
	}
	find(body, "", 0)
	if len(paths) == 0 {
		return nil
	}
	return &output.Warning{
		Code: warnRichTextAsString,
		Message: fmt.Sprintf("%d block field(s) that hold rich text on the observed rows hold a string here; Payload stores the string and the admin editor cannot open it",
			len(paths)),
		Paths: paths,
		Hint:  "re-read the document without --richtext, or set the field from Markdown: pay blocks set <selector> --set-md <field>=@file.md --field <array>",
	}
}

// commandName is "pay <command>" for messages, "pay" when unknown.
func (d *Deps) commandName() string {
	if d == nil || d.RT == nil || d.RT.Command == "" {
		return "pay"
	}
	return "pay " + d.RT.Command
}

// rowValueAt reads a row-relative key (`content`, `section.body`,
// `items[0].answer`) from a row, nil when it does not resolve.
func rowValueAt(row map[string]any, key string) any {
	p, err := rows.ParsePath(key)
	if err != nil {
		return nil
	}
	res, err := rows.Resolve(row, p)
	if err != nil {
		return nil
	}
	return res.Value
}

// Help notes for --set-md.
const (
	setMDBodyNote = "Markdown → the Lexical editor state `pay lexical from-md` builds; applied after --set-json and wins; " +
		"dotted keys reach group fields (meta.body); an array row (layout.2.content) cannot be addressed — use `pay blocks set --set-md`; " +
		"a leading YAML front matter block (`---` then key: lines) is stripped, a leading rule is not; " +
		"a field the schema does not know is checked against stored values (a text value refuses the write); " +
		"\\@ starts inline Markdown with a literal @"
	setMDRowGrammar = "KEY=@FILE.md | KEY=MARKDOWN"
	setMDRowNote    = "KEY is a path inside the ROW (content, section.body, items[0].answer); @- is refused: stdin holds the document"
)

// censusFieldAt looks a row-relative dotted key up in a block type's census
// profile (`content`, `section.body`); nil when the census does not know it.
// Keys through array rows are not looked up.
func censusFieldAt(stats *discovery.CensusType, key string) *discovery.CensusField {
	if stats == nil || strings.Contains(key, "[") {
		return nil
	}
	fields := stats.Fields
	var f *discovery.CensusField
	for _, part := range strings.Split(key, ".") {
		if fields == nil {
			return nil
		}
		if f = fields[part]; f == nil {
			return nil
		}
		fields = f.Fields
	}
	return f
}

// mdKindWarning warns when the census observed a --set-md key of a block type
// as something other than rich text (a plain string heading, say). Observed
// facts only ever warn (§7.10c).
func mdKindWarning(stats *discovery.CensusType, blockType, key, where string) *output.Warning {
	f := censusFieldAt(stats, key)
	if f == nil {
		return nil
	}
	switch f.Kind {
	case discovery.CensusKindRichText, discovery.CensusKindUnknown, "":
		return nil
	}
	return &output.Warning{
		Code: warnSetMDNotRichText,
		Message: fmt.Sprintf("--set-md %s: the observed %s rows hold %s values there (census kind %s), not rich text; the key now holds a Lexical editor state",
			key, blockType, describeKinds(f), f.Kind),
		Paths: []string{where},
		Hint:  "check the key with `pay blocks schema " + blockType + "` (rich-text fields have kind richText); a plain text field takes --set",
	}
}

func describeKinds(f *discovery.CensusField) string {
	types := make([]string, 0, len(f.Types))
	for t := range f.Types {
		if t != "null" {
			types = append(types, t)
		}
	}
	sort.Strings(types)
	if len(types) == 0 {
		return "null"
	}
	return strings.Join(types, "/")
}

// cachedTypeStats is the census profile of a block type from the CACHED
// census (never built here), nil when there is none.
func cachedTypeStats(d *Deps, target *output.Target, shard *discovery.Shard, blockType string) *discovery.CensusType {
	if d == nil || d.RT == nil || blockType == "" {
		return nil
	}
	c, ok := d.RT.cachedCensus()
	if !ok {
		return nil
	}
	kind, slug := discovery.EntityKindCollection, ""
	if target != nil {
		slug = target.Slug
		if target.Kind == "global" {
			kind = discovery.EntityKindGlobal
		}
	}
	return discovery.NewBlockKnowledge(kind, slug, c, shard).TypeStats(blockType)
}
