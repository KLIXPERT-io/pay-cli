package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/fsatomic"
	"github.com/KLIXPERT-io/pay-cli/internal/lexical"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/rows"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

// `pay lexical` converts Payload rich text (a Lexical editor state) to and
// from Markdown, and extracts its plain text.
//
// It exists because the measured agent workflow for "put this Markdown into a
// richText field" was a hand-written Python builder per project, and the one
// for "what does this page say" was a jq program over nested `children`
// arrays. Both got the node shapes subtly wrong (a missing `textFormat`, a
// `doc` key on a custom link, a numbered list without `start`). The
// conversion lives in internal/lexical, against the shapes Payload 3.87
// stores; this file is only the CLI around it.
//
// Every verb is L0 and runs with no profile, no credential and no network:
// it reads a file or stdin and prints the result.

func init() { Register(newLexicalCmd) }

func newLexicalCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		GroupID: GroupEdit,
		Use:     "lexical",
		Short:   "Convert rich text between Lexical JSON and Markdown (local, no network)",
		Long: `Payload stores a richText field as a Lexical editor state: {"root":{"type":"root",
"children":[…]}}. These verbs translate it to and from Markdown and extract its
plain text, locally — no profile, no credential, nothing sent to Payload.

    pay lexical from-md body.md --as content | pay update posts 12 --data @- --draft
    pay get pages 47 --draft --depth 0 | pay lexical to-md -o -`,
	}
	cmd.AddCommand(newLexicalFromMDCmd(rt), newLexicalToMDCmd(rt), newLexicalTextCmd(rt))
	SetHelp(cmd, lexicalFamilyHelp())
	return cmd
}

// lexicalExitCodes: a conversion fails only on its input — unreadable,
// not JSON, not rich text, or a broken placeholder.
var lexicalExitCodes = []int{apierr.ExitOK, apierr.ExitInternal, apierr.ExitValidation}

func lexicalFamilyHelp() *Help {
	return &Help{
		Synopsis: []string{
			"pay lexical from-md [FILE|-] [--as FIELD] [--keep-frontmatter]",
			"pay lexical to-md   [FILE|-] [--field PATH] [-o FILE|-]",
			"pay lexical text    [FILE|-] [--field PATH] [-o FILE|-]",
		},
		Output: OutputSpec{
			Kind:     output.KindOpResult,
			Skeleton: `from-md: {"root":{…}}   to-md: {"markdown":"…","fields":[…]}   text: {"text":"…","fields":[…]}`,
		},
		ExitCodes: lexicalExitCodes,
		Examples: []Example{
			{Why: "write a Markdown file into a post's richText field (as a draft)",
				Cmd: "pay lexical from-md body.md --as content | pay update posts 12 --data @- --draft"},
			{Why: "read a page's rich text as Markdown",
				Cmd: "pay get pages 47 --draft --depth 0 | pay lexical to-md -o -"},
			{Why: "just the words, e.g. to check a migrated page against its source",
				Cmd: "pay get posts 3 --depth 0 | pay lexical text --field content -o -"},
			{Why: "see exactly which JSON a piece of Markdown becomes",
				Cmd: "echo '**Hello** [docs](pay:pages/47)' | pay lexical from-md"},
		},
		Mistakes: []Mistake{
			{Wrong: "Hand-building Lexical JSON (or a Python helper) for a richText field.",
				Right: "`pay lexical from-md` writes the exact shape Payload stores: textFormat/textStyle on paragraphs, `start` on lists, custom vs internal link fields."},
			{Wrong: "Deleting the `<!-- lexical:… -->` comments from Markdown produced by to-md before writing it back.",
				Right: "They carry the nodes Markdown cannot spell (uploads with captions, blocks, tables). Keep them; from-md restores each node exactly."},
			{Wrong: "Linking to another page of the site by its URL path.",
				Right: "Write [text](pay:pages/47): it becomes an internal link that follows the page when its slug changes."},
		},
		SeeAlso: []string{
			"pay describe <collection>            # which fields are richText",
			"pay update <collection> <id> --data @-   # write a --as body",
		},
	}
}

// ---------------------------------------------------------------------------
// shared plumbing
// ---------------------------------------------------------------------------

// runOffline is runLocalData without the optional client and discovery: a
// conversion needs neither, and resolving them could reach the network (a
// cold discovery cache on a configured profile) for nothing.
func runOffline(rt *Runtime, name string, h dataHandler) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		rt.Command = name
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		d := &Deps{RT: rt}
		env, err := h(ctx, cmd, d, args)
		if err != nil {
			env = output.NewError(name, err)
		} else if env == nil {
			env = output.NewError(name, apierr.New(apierr.CodeInternal,
				"command %q produced no result", name))
		}
		rt.emit(env)
		return nil
	}
}

// readInputArg reads FILE, or stdin for "" and "-".
func readInputArg(d *Deps, command string, args []string) ([]byte, error) {
	if len(args) == 0 || args[0] == "-" {
		b, err := io.ReadAll(d.inStream())
		if err != nil {
			return nil, apierr.Wrap(err, apierr.CodeNoInput, "stdin could not be read")
		}
		if strings.TrimSpace(string(b)) == "" {
			return nil, apierr.New(apierr.CodeNoInput,
				"`pay %s` reads a FILE argument or stdin, and stdin was empty", command).
				WithHint("pass a file: `pay %s <file>`, or pipe one in", command)
		}
		return b, nil
	}
	b, err := os.ReadFile(args[0])
	if err != nil {
		return nil, apierr.Wrap(err, apierr.CodeFileMissing, "%s could not be read", args[0]).
			WithHint("pass an existing file, or - to read stdin")
	}
	return b, nil
}

// issueHints is the remedy for each conversion finding.
var issueHints = map[string]string{
	lexical.IssuePlaceholder:          "each <!-- lexical:TYPE {…} --> comment carries a node Markdown cannot spell; keep it intact and `pay lexical from-md` restores the node exactly",
	lexical.IssueLossyAlignment:       "alignment and indent have no Markdown spelling; writing this Markdown back drops them, so set them in the admin editor afterwards",
	lexical.IssueLossyStyle:           "inline CSS has no Markdown spelling; writing this Markdown back drops it",
	lexical.IssueLossyFormat:          "lowercase/uppercase/capitalize formatting has no Markdown spelling; writing this Markdown back drops it",
	lexical.IssueNormalized:           "no content is lost: the Markdown reads back as the shape Payload's editor itself produces",
	lexical.IssueHTMLAsText:           "the dialect reads **bold**, *italic*, ~~strike~~, `code`, <u>, <sub>, <sup>, <mark> and <br>; other HTML has no Lexical node and stays literal text",
	lexical.IssueCommentDropped:       "the only comment that survives is a <!-- lexical:TYPE {…} --> placeholder",
	lexical.IssueImageAsText:          "an image must be an upload: write ![media:ID]() with the id from `pay find media --select id,filename`",
	lexical.IssueCodeBlockFlattened:   "Payload's default editor has no code block; if the project has a code block type, add it as a block instead",
	lexical.IssueTableAsText:          "tables are not converted; build them in the admin editor or as a block",
	lexical.IssueNestingCapped:        "no editor nests content that deep; flatten the quotes/lists in the source",
	lexical.IssueListBecameChecklist:  "mark every item as a task (- [ ] / - [x]), or split the list with a blank line and a paragraph so the plain items stay bullets",
	lexical.IssueItemParagraphsJoined: "a Lexical list item holds one line of inline content; the paragraphs were joined with line breaks",
	lexical.IssueLinkURLInvalid:       "give the link a URL without spaces (percent-encode them as %20), or make it internal: [text](pay:pages/ID)",
	lexical.IssueLinkURLRelative:      "point it at a site path (/prozess), an absolute URL, or an internal link [text](pay:pages/ID) that follows the page when its slug changes",
}

// issueWarnings groups conversion findings into one warning per code, with
// the places they occurred (capped, so a long document cannot flood the
// envelope).
func issueWarnings(issues []lexical.Issue, where func(lexical.Issue) string) []output.Warning {
	const maxPaths = 20
	byCode := map[string][]lexical.Issue{}
	var order []string
	for _, is := range issues {
		if _, seen := byCode[is.Code]; !seen {
			order = append(order, is.Code)
		}
		byCode[is.Code] = append(byCode[is.Code], is)
	}
	out := make([]output.Warning, 0, len(order))
	for _, code := range order {
		list := byCode[code]
		msg := list[0].Message
		if len(list) > 1 {
			msg = fmt.Sprintf("%d occurrences; first: %s", len(list), msg)
		}
		var paths []string
		for i, is := range list {
			if i == maxPaths {
				paths = append(paths, fmt.Sprintf("… and %d more", len(list)-maxPaths))
				break
			}
			if p := where(is); p != "" {
				paths = append(paths, p)
			}
		}
		out = append(out, output.Warning{
			Code:    "lexical_" + code,
			Message: msg,
			Paths:   dedupe(paths),
			Hint:    issueHints[code],
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// pay lexical from-md
// ---------------------------------------------------------------------------

func newLexicalFromMDCmd(rt *Runtime) *cobra.Command {
	var as string
	var keepFM bool
	cmd := &cobra.Command{
		Use:   "from-md [FILE|-]",
		Short: "Convert Markdown to a Lexical editor state",
		Long: `Reads Markdown from FILE (or stdin) and prints the Lexical editor state Payload
stores for it. With --as FIELD the state is wrapped as {FIELD: state}, a request
body that pipes straight into ` + "`--data @-`" + ` of create/update/globals update.

The dialect is CommonMark (headings, paragraphs, emphasis, links, autolinks,
nested bullet/ordered/task lists, block quotes, rules, hard breaks, escapes)
plus GFM ~~strikethrough~~, <u>, <sub>, <sup>, <mark>, internal links
[text](pay:COLLECTION/ID) or by slug [text](pay:COLLECTION/slug=VALUE) (a
{"$ref"} every write verb resolves to the id), link attributes {target=_blank}, uploads
![media:ID]() and <!-- lexical:TYPE {…} --> placeholders for any other node.`,
		Args: maxArgs(1, "pay lexical from-md [FILE|-]"),
	}
	cmd.Flags().StringVar(&as, "as", "", "wrap the state in the write body {FIELD: state} for --data @- (dotted for a group: meta.body)")
	cmd.Flags().BoolVar(&keepFM, "keep-frontmatter", false, "do not strip a leading YAML front matter block")

	cmd.RunE = runOffline(rt, safety.CmdLexicalFromMD, func(_ context.Context, _ *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		raw, err := readInputArg(d, safety.CmdLexicalFromMD, args)
		if err != nil {
			return nil, err
		}
		md := string(raw)
		var warns []output.Warning
		lineOffset := 0
		if !keepFM {
			if body, fm, ok := lexical.StripFrontmatter(md); ok {
				// Findings name lines of the FILE, not of the stripped body.
				lineOffset = strings.Count(md[:len(md)-len(body)], "\n")
				md = body
				warns = append(warns, frontmatterWarning("", fm, "pass --keep-frontmatter to convert it as Markdown"))
			}
		}
		state, issues, err := lexical.FromMarkdownIssues(md)
		if err != nil {
			return nil, apierr.New(apierr.CodeBadRequestBody, "%s", err.Error()).
				WithHint("a <!-- lexical:TYPE {…} --> placeholder must hold one JSON object; copy it unchanged from `pay lexical to-md`")
		}
		warns = append(warns, issueWarnings(issues, func(is lexical.Issue) string {
			if is.Line > 0 {
				return "line " + strconv.Itoa(is.Line+lineOffset)
			}
			return ""
		})...)

		var data any = state
		if as != "" {
			root, hop, err := bodyKeyArrayHop(as)
			if err != nil || strings.Trim(as, ".") != as || strings.Contains(as, "..") {
				return nil, apierr.New(apierr.CodeInvalidArgs, "--as %q is not a field path", as).
					WithHint("name the richText field, e.g. --as content or --as meta.body")
			}
			if hop {
				// {"layout":{"0":…}} would reach Payload as an object where
				// the blocks array belongs, and the write would empty it.
				return nil, apierr.New(apierr.CodeInvalidArgs,
					"--as %s addresses a row of an array, and a write body can only replace the whole array %s", as, root).
					WithHint("%s", rowPipeHint("<collection>"))
			}
			body := map[string]any{}
			setPath(body, as, state)
			data = body
		}
		env := output.New(safety.CmdLexicalFromMD, output.KindOpResult, data)
		for _, w := range warns {
			env.AddWarning(w)
		}
		return env, nil
	})

	SetHelp(cmd, &Help{
		Synopsis: []string{"pay lexical from-md [FILE|-] [--as FIELD] [--keep-frontmatter]"},
		Args: []ArgSpec{
			{Name: "file", Required: false, Type: "path", Example: "body.md  (omit it, or pass -, to read stdin)"},
		},
		FlagInfo: map[string]FlagInfo{
			"as":               {Grammar: "FIELD | GROUP.FIELD", Note: "the output then is a write body, not a bare editor state; an array row (layout.0.content) is refused — a body replaces the whole array, so edit a row with `pay blocks set --set-md`"},
			"keep-frontmatter": {Note: "front matter (a leading ---/--- block of YAML key: lines) is stripped by default and reported as frontmatter_stripped with its line count and first line; a leading rule followed by prose is never front matter"},
		},
		Output: OutputSpec{
			Kind: output.KindOpResult,
			Skeleton: `{"root":{"type":"root","version":1,"direction":"ltr","format":"","indent":0,
  "children":[{"type":"paragraph","version":1,"textFormat":0,"textStyle":"", …,
    "children":[{"type":"text","version":1,"text":"Hello","format":1,"detail":0,"mode":"normal","style":""}]}]}}`,
		},
		ExitCodes: lexicalExitCodes,
		Notes: []string{
			"Formats: **bold** *italic* ~~strike~~ `code` <u>underline</u> <sub>x</sub> <sup>x</sup> <mark>x</mark>.",
			"Links: [text](https://…) custom, [text](pay:pages/47) internal, {target=_blank} after the link opens a new tab.",
			"By slug: [text](pay:pages/slug=about) writes doc.value {\"$ref\":\"pages\",\"slug\":\"about\"}; create/update/upsert/sync resolve it to the id (pay:pages/about would store the string \"about\" as the id — a dead link on a numeric-id site).",
			"Uploads: ![media:40]() on its own line. Any other node: <!-- lexical:TYPE {json} --> (what to-md writes).",
			"Findings (dropped HTML, flattened code blocks, link URLs Payload rejects or that are relative to the source file) are warnings named lexical_<code>.",
		},
		Examples: []Example{
			{Why: "convert a file and look at the JSON",
				Cmd: "pay lexical from-md body.md"},
			{Why: "write it into a draft in one pipe (the envelope's data is the request body)",
				Cmd: "pay lexical from-md body.md --as content | pay update posts 12 --data @- --draft"},
			{Why: "create a new post from Markdown",
				Cmd: "pay lexical from-md body.md --as content | pay create posts --data @- --set title='Hello' --set slug=hello --draft"},
			{Why: "keep the state in a file for --set-json",
				Cmd: "pay lexical from-md body.md --output raw > content.json"},
			{Why: "stdin works too",
				Cmd: "printf '## Title\\n\\n- one\\n- two\\n' | pay lexical from-md"},
		},
		Mistakes: []Mistake{
			{Wrong: "Piping `from-md` without --as into `--data @-`.",
				Right: "Without --as the data is the bare editor state, which is not a document body. Add --as <field>."},
			{Wrong: "Relative links to other Markdown files (../brief.md) in migrated content.",
				Right: "On the site a relative URL resolves against the page's own URL (lexical_link_url_relative warns). Use site paths (/prozess) or [text](pay:pages/ID)."},
			{Wrong: "Expecting ![alt](https://…/img.png) to become an image.",
				Right: "Rich-text images are uploads: upload the file (`pay upload media`) and write ![media:ID]()."},
			{Wrong: "Writing the Markdown of a whole page (all blocks) into one richText field.",
				Right: "A page's layout is blocks; convert the Markdown of each block's rich text separately and set it on that block (`pay blocks set`)."},
		},
		SeeAlso: []string{"pay lexical to-md", "pay update <collection> <id> --data @-"},
	})
	return cmd
}

// ---------------------------------------------------------------------------
// input: an editor state, a document, or an envelope
// ---------------------------------------------------------------------------

// richField is one editor state found in the input, with its document path.
type richField struct {
	Path  string
	State map[string]any
}

// richInput is a decoded `pay lexical to-md|text` input.
type richInput struct {
	// Root is the value the editor states were found in (the envelope's data
	// for an envelope), which --field resolves against.
	Root     any
	Found    []richField
	Warnings []output.Warning
	Target   *output.Target
}

// readRichInput decodes the input and finds its editor states.
func readRichInput(d *Deps, command string, args []string) (*richInput, error) {
	raw, err := readInputArg(d, command, args)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, apierr.New(apierr.CodeBadRequestBody, "the input is not valid JSON").
			WithHint("pass an editor state, a document, or the envelope of `pay get`; to convert Markdown use `pay lexical from-md`")
	}
	var warns []output.Warning
	var target *output.Target
	if obj, ok := v.(map[string]any); ok {
		if env, isEnv := asEnvelope(obj); isEnv {
			if okv, _ := env["ok"].(bool); !okv {
				return nil, upstreamError(env)
			}
			if format := envelopeRichText(env); format != "" {
				return nil, apierr.New(apierr.CodeRichTextRendered,
					"the input was read with --richtext %s: its rich text is already %s, and no Lexical editor state is left to convert", format, formatNoun(format)).
					WithHint("read it with the default --richtext json and pipe that: pay get <collection> <id> --depth 0 | pay lexical to-md")
			}
			target = decodeInto[output.Target](env["target"])
			if ws := decodeInto[[]output.Warning](env["warnings"]); ws != nil {
				warns = *ws
			}
			v = env["data"]
		}
	}
	var found []richField
	collectRich(v, "", &found)
	return &richInput{Root: v, Found: found, Warnings: warns, Target: target}, nil
}

// collectRich walks a JSON value and records every editor state, with paths
// in the `layout[2].blocks[0].richText` form other commands print.
func collectRich(v any, path string, out *[]richField) {
	switch t := v.(type) {
	case map[string]any:
		if lexical.IsEditorState(t) {
			*out = append(*out, richField{Path: path, State: t})
			return
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
			collectRich(t[k], p, out)
		}
	case []any:
		for i, x := range t {
			collectRich(x, path+"["+strconv.Itoa(i)+"]", out)
		}
	}
}

// numericSegment matches `.2` in `layout.2.blocks`, the dotted index form.
var numericSegment = regexp.MustCompile(`\.(\d+)(\.|$)`)

// normalizeFieldPath turns `layout.2.richText` into `layout[2].richText`.
func normalizeFieldPath(p string) string {
	p = strings.TrimPrefix(strings.TrimSpace(p), ".")
	for {
		next := numericSegment.ReplaceAllString(p, "[$1]$2")
		if next == p {
			break
		}
		p = next
	}
	return p
}

// selectRich applies --field. The path is resolved against the input with the
// nested --field grammar `pay blocks` uses (§9.11.3): positions
// (`layout[2].richText`, `layout.2.richText`), ids (`layout[id:6ab8…].content`)
// and types (`layout[type:group[1]].blocks[0].content`) all name the same
// value. A path into a `pay find` list (`[1].content`) is matched as written.
func selectRich(in *richInput, field string) ([]richField, error) {
	found := in.Found
	paths := make([]string, 0, len(found))
	for _, f := range found {
		paths = append(paths, f.Path)
	}
	if field == "" {
		if len(found) == 0 {
			return nil, apierr.New(apierr.CodeNotRichText,
				"the input holds no Lexical editor state").
				WithHint("pipe a document with a richText field (`pay get <collection> <id> --depth 0`), or an editor state; `pay describe <collection>` shows which fields are richText")
		}
		return found, nil
	}
	want := normalizeFieldPath(field)
	for _, f := range found {
		if f.Path == want {
			return []richField{f}, nil
		}
	}
	notRich := func(detail string) error {
		e := apierr.New(apierr.CodeNotRichText, "--field %s is not a rich-text value in the input%s", field, detail)
		if len(paths) > 0 {
			return e.WithDidYouMean(paths...).
				WithHint("the rich-text paths in this input are: %s", strings.Join(paths, ", "))
		}
		return e.WithHint("the input holds no rich text at all; read the document with `pay get <collection> <id> --depth 0` (a --select that omits the field removes it)")
	}
	doc, isDoc := in.Root.(map[string]any)
	if !isDoc {
		return nil, notRich("")
	}
	p, err := rows.ParsePath(field)
	if err != nil {
		return nil, apierr.New(apierr.CodeInvalidArgs, "--field: %s", err.Error()).
			WithHint("the grammar is: %s", rows.PathGrammar)
	}
	res, err := rows.Resolve(doc, p)
	if err != nil {
		var miss *rows.NoMatchError
		var amb *rows.AmbiguousError
		if errors.As(err, &miss) || errors.As(err, &amb) {
			// A selector that addresses no row (or several) is the same
			// not-found / ambiguous answer `pay blocks` gives.
			return nil, resolveErr(err, nil)
		}
		var pe *rows.PathError
		if errors.As(err, &pe) {
			return nil, notRich(" (" + pe.Error() + ")")
		}
		return nil, notRich("")
	}
	state, ok := res.Value.(map[string]any)
	if !ok || !lexical.IsEditorState(state) {
		return nil, notRich(fmt.Sprintf(" (%s holds %s)", res.At.String(), jsonKindOf(res.Value)))
	}
	return []richField{{Path: res.At.String(), State: state}}, nil
}

// outFlag is -o/--out: write the text to a file, or to stdout with the
// envelope moved to stderr.
type outFlag struct{ path string }

func (o *outFlag) register(cmd *cobra.Command) {
	cmd.Flags().StringVarP(&o.path, "out", "o", "", "also write the text to FILE, or - for stdout (the envelope then goes to stderr)")
}

// write puts text where -o says and returns the value for data.path.
func (o *outFlag) write(d *Deps, text string) (string, error) {
	switch o.path {
	case "":
		return "", nil
	case "-":
		if d == nil || d.RT == nil || d.RT.App.Stdout == nil {
			return "", apierr.New(apierr.CodeInternal, "stdout is not available")
		}
		if _, err := io.WriteString(d.RT.App.Stdout, text); err != nil {
			return "", apierr.Wrap(err, apierr.CodeInternal, "stdout could not be written")
		}
		if d.RT.Out != nil {
			d.RT.Out.StdoutIsData = true
		}
		return "-", nil
	default:
		if err := fsatomic.Write(o.path, []byte(text), 0o644); err != nil {
			return "", apierr.Wrap(err, apierr.CodeInternal, "%s could not be written", o.path).
				WithHint("check that the directory exists and is writable")
		}
		return o.path, nil
	}
}

// combine joins per-field texts; with several fields each is introduced by a
// `<!-- pay:field PATH -->` comment so the reader can tell them apart.
func combine(parts []map[string]any, key string) string {
	if len(parts) == 1 {
		s, _ := parts[0][key].(string)
		return s
	}
	var sb strings.Builder
	for i, p := range parts {
		if i > 0 {
			sb.WriteString("\n")
		}
		path, _ := p["path"].(string)
		s, _ := p[key].(string)
		sb.WriteString("<!-- pay:field " + path + " -->\n\n")
		sb.WriteString(strings.TrimRight(s, "\n"))
		sb.WriteString("\n")
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// pay lexical to-md
// ---------------------------------------------------------------------------

func newLexicalToMDCmd(rt *Runtime) *cobra.Command {
	var field string
	var out outFlag
	cmd := &cobra.Command{
		Use:   "to-md [FILE|-]",
		Short: "Render Lexical rich text as Markdown",
		Long: `Reads an editor state, a document, or a PayCLI envelope (FILE or stdin) and
renders every rich-text value in it as Markdown — or only the one --field names.

The Markdown reads back through ` + "`pay lexical from-md`" + ` to the same content. Nodes
Markdown cannot spell (blocks, relationships, uploads with extra data, tables)
are written as <!-- lexical:TYPE {…} --> placeholders that from-md restores
exactly; the only losses are alignment, indent and inline CSS, reported as
lexical_lossy_* warnings.`,
		Args: maxArgs(1, "pay lexical to-md [FILE|-] [--field PATH]"),
	}
	cmd.Flags().StringVar(&field, "field", "", "convert only this rich-text path (content, layout[2].richText, layout[id:6ab8…].content, layout[type:group].blocks[0].content)")
	out.register(cmd)

	cmd.RunE = runOffline(rt, safety.CmdLexicalToMD, func(_ context.Context, _ *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		in, err := readRichInput(d, safety.CmdLexicalToMD, args)
		if err != nil {
			return nil, err
		}
		upstream, target := in.Warnings, in.Target
		sel, err := selectRich(in, field)
		if err != nil {
			return nil, err
		}
		var parts []map[string]any
		var issues []lexical.Issue
		for _, f := range sel {
			md, iss, err := lexical.ToMarkdownIssues(f.State)
			if err != nil {
				if errors.Is(err, lexical.ErrNotEditorState) {
					return nil, apierr.New(apierr.CodeNotRichText, "%s: %s", f.Path, err.Error())
				}
				return nil, apierr.Wrap(err, apierr.CodeInternal, "%s could not be rendered", f.Path)
			}
			for _, is := range iss {
				is.Path = joinFieldPath(f.Path, is.Path)
				issues = append(issues, is)
			}
			parts = append(parts, map[string]any{"path": f.Path, "markdown": md})
		}
		combined := combine(parts, "markdown")
		written, err := out.write(d, combined)
		if err != nil {
			return nil, err
		}
		data := map[string]any{
			"markdown": combined,
			"fields":   parts,
			"count":    len(parts),
		}
		if written != "" {
			data["out"] = written
		}
		env := output.New(safety.CmdLexicalToMD, output.KindOpResult, data)
		if target != nil {
			env.WithTarget(target)
		}
		for _, w := range upstream {
			env.AddWarning(w)
		}
		for _, w := range issueWarnings(issues, func(is lexical.Issue) string { return is.Path }) {
			env.AddWarning(w)
		}
		return env, nil
	})

	SetHelp(cmd, &Help{
		Synopsis: []string{
			"pay lexical to-md [FILE|-] [--field PATH] [-o FILE|-]",
			"pay get <collection> <id> --depth 0 | pay lexical to-md [--field PATH]",
		},
		Args: []ArgSpec{
			{Name: "file", Required: false, Type: "path", Example: "page.json  (omit it, or pass -, to read stdin)"},
		},
		FlagInfo: map[string]FlagInfo{
			"field": {Grammar: "PATH", Note: "paths are printed in data.fields[].path; the nested --field grammar of `pay blocks` works too: layout.2.richText, layout[id:6ab8…].content, layout[type:group[1]].blocks[0].content"},
			"out":   {Grammar: "FILE | -", Note: "- writes the bare Markdown to stdout and moves the envelope to stderr"},
		},
		Output: OutputSpec{
			Kind: output.KindOpResult,
			Skeleton: `{"markdown":"## Title\n\nText with **bold** and a [link](pay:pages/47).\n",
 "fields":[{"path":"content","markdown":"…"}],"count":1}`,
		},
		ExitCodes: lexicalExitCodes,
		Notes: []string{
			"With several rich-text values and no --field, data.markdown joins them, each introduced by <!-- pay:field PATH -->; data.fields has them one by one.",
			"Links: internal links render as [text](pay:COLLECTION/ID), a new-tab link gets {target=_blank}.",
		},
		Examples: []Example{
			{Why: "a post's body as Markdown",
				Cmd: "pay get posts 3 --depth 0 | pay lexical to-md --field content -o -"},
			{Why: "every rich-text value of a page (blocks included), one per entry",
				Cmd: "pay get pages 47 --draft --depth 0 | pay lexical to-md --path '.fields[].path'"},
			{Why: "one block's rich text, straight to a file",
				Cmd: "pay get pages 47 --draft --depth 0 | pay lexical to-md --field 'layout[1].richText' -o block.md"},
			{Why: "edit and write back: to-md, edit the file, from-md",
				Cmd: "pay lexical from-md block.md --output raw > block.json"},
			{Why: "a saved editor state or document on disk",
				Cmd: "pay lexical to-md state.json -o -"},
		},
		Mistakes: []Mistake{
			{Wrong: "Reading with --depth 1 or more before converting.",
				Right: "Above depth 0 internal links and uploads carry whole documents; to-md reduces them to ids, but read with --depth 0 when you intend to write back."},
			{Wrong: "Writing data.markdown of a multi-field document back into one field.",
				Right: "It joins every rich-text value of the document. Pass --field to get exactly one."},
			{Wrong: "Removing <!-- lexical:… --> comments to make the Markdown look cleaner before writing it back.",
				Right: "Each one is a node (a block, an upload with data, a table). Removing it deletes that node on write."},
		},
		SeeAlso: []string{"pay lexical from-md", "pay lexical text"},
	})
	return cmd
}

// joinFieldPath prefixes a node path with the document field it lives in.
func joinFieldPath(field, node string) string {
	switch {
	case field == "":
		return node
	case node == "":
		return field
	}
	return field + " > " + node
}

// ---------------------------------------------------------------------------
// pay lexical text
// ---------------------------------------------------------------------------

func newLexicalTextCmd(rt *Runtime) *cobra.Command {
	var field string
	var out outFlag
	cmd := &cobra.Command{
		Use:   "text [FILE|-]",
		Short: "Extract the plain text of Lexical rich text",
		Long: `Reads an editor state, a document, or a PayCLI envelope (FILE or stdin) and
prints the plain text of every rich-text value in it — or only the one --field
names. Blocks are separated by a blank line, list items and table rows by a
newline; uploads, blocks and relationships contribute no text.`,
		Args: maxArgs(1, "pay lexical text [FILE|-] [--field PATH]"),
	}
	cmd.Flags().StringVar(&field, "field", "", "extract only this rich-text path (content, layout[2].richText, layout[id:6ab8…].content, layout[type:group].blocks[0].content)")
	out.register(cmd)

	cmd.RunE = runOffline(rt, safety.CmdLexicalText, func(_ context.Context, _ *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		in, err := readRichInput(d, safety.CmdLexicalText, args)
		if err != nil {
			return nil, err
		}
		upstream, target := in.Warnings, in.Target
		sel, err := selectRich(in, field)
		if err != nil {
			return nil, err
		}
		parts := make([]map[string]any, 0, len(sel))
		for _, f := range sel {
			text := lexical.PlainText(f.State)
			parts = append(parts, map[string]any{"path": f.Path, "text": text, "chars": len([]rune(text))})
		}
		combined := ""
		if len(parts) == 1 {
			combined, _ = parts[0]["text"].(string)
		} else {
			texts := make([]string, 0, len(parts))
			for _, p := range parts {
				if s, _ := p["text"].(string); s != "" {
					texts = append(texts, s)
				}
			}
			combined = strings.Join(texts, "\n\n")
		}
		toWrite := combined
		if toWrite != "" && !strings.HasSuffix(toWrite, "\n") {
			toWrite += "\n"
		}
		written, err := out.write(d, toWrite)
		if err != nil {
			return nil, err
		}
		data := map[string]any{"text": combined, "fields": parts, "count": len(parts)}
		if written != "" {
			data["out"] = written
		}
		env := output.New(safety.CmdLexicalText, output.KindOpResult, data)
		if target != nil {
			env.WithTarget(target)
		}
		for _, w := range upstream {
			env.AddWarning(w)
		}
		return env, nil
	})

	SetHelp(cmd, &Help{
		Synopsis: []string{
			"pay lexical text [FILE|-] [--field PATH] [-o FILE|-]",
			"pay get <collection> <id> --depth 0 | pay lexical text [--field PATH]",
		},
		Args: []ArgSpec{
			{Name: "file", Required: false, Type: "path", Example: "page.json  (omit it, or pass -, to read stdin)"},
		},
		FlagInfo: map[string]FlagInfo{
			"field": {Grammar: "PATH", Note: "paths are printed in data.fields[].path; the nested --field grammar of `pay blocks` works too (layout[id:6ab8…].content)"},
			"out":   {Grammar: "FILE | -", Note: "- writes the bare text to stdout and moves the envelope to stderr"},
		},
		Output: OutputSpec{
			Kind:     output.KindOpResult,
			Skeleton: `{"text":"Title\n\nFirst paragraph.\n\none\ntwo","fields":[{"path":"content","text":"…","chars":41}],"count":1}`,
		},
		ExitCodes: lexicalExitCodes,
		Examples: []Example{
			{Why: "the words of a post",
				Cmd: "pay get posts 3 --depth 0 | pay lexical text --field content -o -"},
			{Why: "all text of a page, blocks included, to compare with the migration source",
				Cmd: "pay get pages 47 --draft --depth 0 | pay lexical text -o page.txt"},
			{Why: "which rich-text fields have how much text",
				Cmd: "pay get pages 47 --draft --depth 0 | pay lexical text --path '.fields[]'"},
			{Why: "every post's text from a list read",
				Cmd: "pay find posts --limit 5 --depth 0 --select title,content | pay lexical text --path '.fields[]'"},
		},
		Mistakes: []Mistake{
			{Wrong: "Parsing `children` arrays with jq to get the text of a page.",
				Right: "`pay lexical text` walks every node type (links, list nesting, tables, line breaks) the same way Payload's own plain-text converter does."},
			{Wrong: "Expecting captions or block content in the text.",
				Right: "Uploads, relationships and blocks inside rich text carry no text of their own; use `pay lexical to-md` to see them as placeholders."},
			{Wrong: "Using `text` output to write content back.",
				Right: "Plain text drops all formatting and links. Round-trip through `pay lexical to-md` and `pay lexical from-md` instead."},
		},
		SeeAlso: []string{"pay lexical to-md"},
	})
	return cmd
}
