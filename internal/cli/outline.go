package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/lexical"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/rows"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

// `pay outline` (§9.13, F3) prints the structure of one document: one line per
// block or array row at every depth, each with the exact --field and selector
// `pay blocks` needs to edit it, a one-line summary of what the row says, and
// the flags that mark rows Payload silently mangled (`{}` orphans, rows that
// lost their blockType).
//
// It exists because the measured way to answer "what is on this page, and how
// do I address the third card in the grid" was `pay get` piped through a jq
// program an agent wrote per page shape — 54 of 77 calls in one session went
// through jq. `pay blocks ls --recursive` answers the addressing half for one
// blocks field; outline answers it for the whole document and adds what each
// row is about.

func init() { Register(newOutlineCmd) }

// Outline flags.
const (
	outlineFlagEmptyRow    = "empty_row"
	outlineFlagNoBlockType = "no_block_type"
	outlineFlagUnknownType = "unknown_type"
)

// outlineSummaryMax is the summary length in characters.
const outlineSummaryMax = 80

// outlineFlags are the command's own flags.
type outlineFlags struct {
	slug   string
	global string
	draft  bool
}

// outlineRow is one row of the outline.
type outlineRow struct {
	Depth int `json:"depth"`
	// Path is the row's location with positions: layout[4].blocks[1].
	Path string `json:"path"`
	// Field is the --field value that addresses the array the row is in,
	// with stable selectors: layout[id:6ab8…].blocks. Paste it into
	// `pay blocks … --field`.
	Field string `json:"field"`
	// Selector addresses the row within that array: id:6ab8… .
	Selector  string `json:"selector"`
	Index     int    `json:"index"`
	ID        string `json:"id,omitempty"`
	BlockType string `json:"block_type,omitempty"`
	BlockName string `json:"block_name,omitempty"`
	// Label is the block type's admin label when a declared schema in the
	// cached discovery/census knows it; absent otherwise.
	Label string `json:"label,omitempty"`
	// Summary is what the row says, in one line.
	Summary string `json:"summary"`
	// Children counts the rows directly nested in this one.
	Children int `json:"children"`
	// Nested maps each array inside the row to its row count (empty arrays
	// too: that is where `pay blocks add --field` goes).
	Nested map[string]int `json:"nested,omitempty"`
	Flags  []string       `json:"flags"`
}

// outlineCounts is data.counts.
type outlineCounts struct {
	Rows   int            `json:"rows"`
	Blocks int            `json:"blocks"`
	ByType map[string]int `json:"by_type"`
	// Flagged counts rows per flag.
	Flagged map[string]int `json:"flagged"`
}

func newOutlineCmd(rt *Runtime) *cobra.Command {
	f := &outlineFlags{}
	cmd := &cobra.Command{
		GroupID: GroupEdit,
		Use:     "outline [<collection> <id> | <collection> --slug SLUG | --global SLUG]",
		Short:   "Print a document's block structure: every row, its --field, selector and summary",
		Long: `Prints the structure of one document: one entry per block or array row at every
depth (a group's blocks, a grid inside it, a card's buttons), in document order.
Each entry carries the exact --field and selector that address the row in
` + "`pay blocks`" + `, the blockType, a one-line summary (a heading/title-like field, else
the start of the row's rich text), how many rows are nested in it, and flags:

  empty_row      the row is {} — an orphan Payload left after a nested restructure
  no_block_type  a row without blockType in an array whose other rows have one
  unknown_type   a blockType no cached schema or block census knows

Read the document from the server (<collection> <id>, --slug, --global; always
at depth 0), or pipe one in: ` + "`pay get pages 47 --draft --depth 0 | pay outline`" + `.
Labels and unknown_type come from the CACHED discovery and block census only;
outline never builds a census (` + "`pay blocks learn`" + ` does).`,
		Args: cobra.MaximumNArgs(2),
	}
	cmd.Flags().StringVar(&f.slug, "slug", "", "address the document by its slug instead of its id")
	cmd.Flags().StringVar(&f.global, "global", "", "outline this global instead of a collection document")
	cmd.Flags().BoolVar(&f.draft, "draft", false, "read the newest draft (what `pay blocks … | pay apply --draft` edits)")

	remote := runData(rt, safety.CmdOutline, func(ctx context.Context, c *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		return runOutlineRemote(ctx, d, f, args)
	})
	local := runLocalData(rt, safety.CmdOutline, func(ctx context.Context, c *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		return runOutlinePiped(ctx, d, f)
	})
	cmd.RunE = func(c *cobra.Command, args []string) error {
		if len(args) > 0 || f.global != "" {
			return remote(c, args)
		}
		return local(c, args)
	}
	SetHelp(cmd, outlineHelp())
	return cmd
}

func outlineHelp() *Help {
	return &Help{
		Synopsis: []string{
			"pay outline <collection> <id> [--draft]",
			"pay outline <collection> --slug SLUG [--draft]",
			"pay outline --global SLUG [--draft]",
			"pay get <collection> <id> --depth 0 [--draft] | pay outline",
		},
		Collections: true,
		Args: []ArgSpec{
			{Name: "collection", Required: false, Type: "enum", ValuesFrom: "discovery.collections",
				Example: "pages  (omit everything to read a document from stdin)"},
			{Name: "id", Required: false, Type: "string", Example: "47"},
		},
		FlagInfo: map[string]FlagInfo{
			"slug":   {Note: "replaces <id>; 0 matches = doc_not_found (4), >1 = slug_ambiguous (5); honours --draft"},
			"global": {Grammar: "SLUG", Note: "no <collection>/<id> with it"},
			"draft":  {Note: "read the newest draft; use it when you will edit with `pay apply --draft`"},
		},
		Output: OutputSpec{
			Kind: output.KindOpResult,
			Skeleton: `{"rows":[{"depth":0,"path":"layout[4]","field":"layout","selector":"id:6ab8…c1f","index":4,
   "id":"6ab8…c1f","block_type":"group","summary":"","children":2,"nested":{"blocks":2},"flags":[]},
  {"depth":1,"path":"layout[4].blocks[1]","field":"layout[id:6ab8…c1f].blocks","selector":"id:6ab8…c1e",
   "index":1,"block_type":"grid","summary":"","children":3,"flags":[]},
  {"depth":2,"path":"layout[4].blocks[1].blocks[0]","field":"layout[id:6ab8…c1f].blocks[id:6ab8…c1e].blocks",
   "selector":"id:6ab8…c15","index":0,"block_type":"text","summary":"Kennzahlen, die zählen","children":1,"flags":[]}],
 "counts":{"rows":3,"blocks":3,"by_type":{"grid":1,"group":1,"text":1},"flagged":{}}}
--output table: an indented tree (OUTLINE, SUMMARY, FLAGS), one line per row`,
		},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitInternal, apierr.ExitAuth, apierr.ExitThrottled,
			apierr.ExitNotFound, apierr.ExitValidation, apierr.ExitNetwork, apierr.ExitAccessDenied,
			apierr.ExitConfig, apierr.ExitCapability},
		Notes: []string{
			"Every row's `field` + `selector` pair is ready to paste: `pay blocks set <selector> --field '<field>'`.",
			"`summary` is the first of heading/title/headline/eyebrow/name/label/question/phase/caption/tag found on the row (one group level deep too), else the first ~80 characters of its first rich-text field.",
			"Flags: empty_row ({} rows Payload left behind), no_block_type, unknown_type (only when a cached schema or census knows the project's blocks).",
			"outline is L0: it only reads (one GET, plus one lookup for --slug), and a piped document needs no profile at all.",
		},
		Examples: []Example{
			{Why: "what is on this page and how to address each row",
				Cmd: "pay outline pages 47 --draft"},
			{Why: "the same, for a person",
				Cmd: "pay outline pages --slug home --draft --output table"},
			{Why: "find the orphan {} rows a nested restructure left",
				Cmd: "pay outline pages 47 --draft --path '.counts.flagged'"},
			{Why: "then edit one row by pasting its field and selector",
				Cmd: "pay get pages 47 --draft --depth 0 | pay blocks set id:6ab8… --set-md content=@card.md --field 'layout[id:6ab8…].blocks' | pay apply --draft --dry-run"},
			{Why: "a global's structure",
				Cmd: "pay outline --global footer"},
			{Why: "a document from a file or a pipe",
				Cmd: "pay outline < page.json"},
		},
		Mistakes: []Mistake{
			{Wrong: "Counting positions in `pay get` output with jq to find the row to edit.",
				Right: "outline prints each row's --field and selector; the id-based forms survive earlier stages of the same pipe."},
			{Wrong: "Reading the outline without --draft and editing with `pay apply --draft`.",
				Right: "The draft can differ from the published document (rows added, moved). Outline the state you are about to edit."},
			{Wrong: "Expecting labels and unknown_type on a project that was never learned.",
				Right: "They come from the cached discovery/census only. Run `pay blocks learn <collection>` once; outline never triggers it."},
		},
		SeeAlso: []string{
			"pay blocks ls --recursive   # the same addressing for one blocks field, from a pipe",
			"pay blocks schema <blockType>   # what is inside one block type",
			"pay get <collection> <id> --richtext md   # every rich-text value as Markdown",
		},
	}
}

// runOutlineRemote reads the document from the server.
func runOutlineRemote(ctx context.Context, d *Deps, f *outlineFlags, args []string) (*output.Envelope, error) {
	switch {
	case f.global != "" && len(args) > 0:
		return nil, apierr.New(apierr.CodeInvalidArgs, "--global names the document on its own; drop the positional arguments").
			WithHint("pay outline --global <slug>, or pay outline <collection> <id>")
	case f.global != "" && f.slug != "":
		return nil, apierr.New(apierr.CodeInvalidArgs, "--slug addresses a collection document; a global has no slug").
			WithHint("pay outline --global <slug>")
	case f.global == "" && len(args) == 1 && f.slug == "":
		return nil, apierr.New(apierr.CodeInvalidArgs, "`pay outline %s` needs a document: pass <id> or --slug SLUG", args[0]).
			WithHint("pay outline %s <id>, or pay outline %s --slug <slug>", args[0], args[0])
	case len(args) == 2 && f.slug != "":
		return nil, apierr.New(apierr.CodeInvalidArgs, "pass <id> or --slug, not both").
			WithHint("drop one of them: the id and the slug each address the document on their own")
	}
	client, err := d.requireClient()
	if err != nil {
		return nil, err
	}
	run := d.beginRun()
	p := query.Params{Depth: query.IntPtr(0)}
	if f.draft {
		p.Draft = query.BoolPtr(true)
	}

	if f.global != "" {
		g, err := d.global(ctx, f.global)
		if err != nil {
			return nil, err
		}
		if f.draft {
			if err := checkDrafts(g.Slug, g.flags().Drafts); err != nil {
				return nil, err
			}
		}
		doc, _, err := client.GlobalGet(ctx, g.Slug, p, payload.WithKnownRoute())
		if err != nil {
			return nil, err
		}
		k := outlineKnowledge(d, discovery.EntityKindGlobal, g.Slug, g.Shard)
		env := output.New(safety.CmdOutline, output.KindOpResult, outlineData(d, doc, k)).
			WithTarget(g.envTarget()).
			WithMeta(run.meta())
		return env, nil
	}

	t, err := d.collection(ctx, args[0])
	if err != nil {
		return nil, err
	}
	if f.draft {
		if err := checkDrafts(t.Slug, t.flags().Drafts); err != nil {
			return nil, err
		}
	}
	id := ""
	if len(args) == 2 {
		id = args[1]
		if e := d.checkID(t, t.Slug, id); e != nil {
			return nil, e
		}
	} else {
		ref, err := d.ResolveDocBySlug(ctx, t, f.slug, SlugLookup{Draft: f.draft})
		if err != nil {
			return nil, err
		}
		id = ref.IDString
	}
	doc, _, err := client.Get(ctx, t.Slug, id, p,
		payload.WithClassify(classifyFor(t, d.cfg(), nil)), payload.WithKnownRoute())
	if err != nil {
		return nil, err
	}
	k := outlineKnowledge(d, discovery.EntityKindCollection, t.Slug, t.Shard)
	env := output.New(safety.CmdOutline, output.KindOpResult, outlineData(d, doc, k)).
		WithTarget(t.envTarget(doc.ID())).
		WithMeta(run.meta())
	return env, nil
}

// runOutlinePiped outlines the document on stdin (a bare document or any
// envelope whose data is one).
func runOutlinePiped(ctx context.Context, d *Deps, f *outlineFlags) (*output.Envelope, error) {
	if f.slug != "" || f.draft {
		return nil, apierr.New(apierr.CodeInvalidArgs, "--slug and --draft choose what to READ, and a piped document was already read").
			WithHint("pay outline <collection> --slug <slug> --draft, or pass --draft to the `pay get` in front of the pipe")
	}
	in, err := readPipeInput(d, safety.CmdOutline)
	if err != nil {
		var e *apierr.Error
		if errors.As(err, &e) && e.Code == apierr.CodeNoInput {
			return nil, apierr.New(apierr.CodeNoInput,
				"`pay outline` needs a document: name one, or pipe one in").
				WithHint("pay outline <collection> <id> [--draft], or: pay get <collection> <id> --depth 0 | pay outline")
		}
		return nil, err
	}
	if in.Bare {
		return nil, apierr.New(apierr.CodeBadRequestBody,
			"stdin holds an array, and `pay outline` outlines ONE document").
			WithHint("pipe the whole document: `pay get <collection> <id> --depth 0 | pay outline` (a --path narrowing to one field loses the rest)")
	}
	var k *discovery.BlockKnowledge
	if in.Target != nil && in.Target.Slug != "" {
		kind := discovery.EntityKindCollection
		if in.Target.Kind == "global" {
			kind = discovery.EntityKindGlobal
		}
		k = outlineKnowledge(d, kind, in.Target.Slug, pipeShard(ctx, d, in.Target))
	}
	env := output.New(safety.CmdOutline, output.KindOpResult, outlineData(d, in.Doc, k))
	if in.Target != nil {
		env.WithTarget(in.Target)
	}
	for _, w := range in.Warnings {
		env.AddWarning(w)
	}
	return env, nil
}

// outlineKnowledge is the block knowledge outline may use: the cached census
// (never built here) merged with the entity's shard. nil when neither exists.
func outlineKnowledge(d *Deps, kind, slug string, shard *discovery.Shard) *discovery.BlockKnowledge {
	var census *discovery.Census
	if d != nil && d.RT != nil {
		if c, ok := d.RT.cachedCensus(); ok {
			census = c
		}
	}
	if census == nil && shard == nil {
		return nil
	}
	return discovery.NewBlockKnowledge(kind, slug, census, shard)
}

// outlineData builds data (or, for --output table, the tree rows).
func outlineData(d *Deps, doc map[string]any, k *discovery.BlockKnowledge) any {
	list, counts := buildOutline(doc, k)
	if d != nil && d.RT != nil && d.RT.Out != nil && d.RT.Out.Format == output.FormatTable && d.cfg().Path == "" {
		// --output table is a person reading a tree; every other format keeps
		// the lossless object.
		d.RT.Out.Columns = []string{"outline", "summary", "flags"}
		return outlineTable(list)
	}
	return map[string]any{"rows": list, "counts": counts}
}

// buildOutline walks the document.
func buildOutline(doc map[string]any, k *discovery.BlockKnowledge) ([]outlineRow, outlineCounts) {
	nodes := rows.DocTree(doc)
	counts := outlineCounts{ByType: map[string]int{}, Flagged: map[string]int{}}
	known, haveKnowledge := knownBlockTypes(k)

	// An array is a blocks array when any of its rows carries a blockType, or
	// when the knowledge names its pattern as a blocks field.
	typed := map[string]bool{}
	for _, n := range nodes {
		if n.BlockType != "" {
			typed[arrayOf(n.Path)] = true
		}
	}

	out := make([]outlineRow, 0, len(nodes))
	for _, n := range nodes {
		r := outlineRow{
			Depth: n.Depth, Path: n.Path, Field: n.Field, Selector: n.Selector, Index: n.Index,
			ID: n.ID, BlockType: n.BlockType, BlockName: n.BlockName, Nested: n.Nested, Flags: []string{},
		}
		for _, c := range n.Nested {
			r.Children += c
		}
		obj, _ := n.Value.(map[string]any)
		switch {
		case len(obj) == 0:
			r.Flags = append(r.Flags, outlineFlagEmptyRow)
		case n.BlockType == "" && (typed[arrayOf(n.Path)] || knownBlocksArray(k, arrayOf(n.Path))):
			r.Flags = append(r.Flags, outlineFlagNoBlockType)
		case n.BlockType != "" && haveKnowledge && !known[n.BlockType]:
			r.Flags = append(r.Flags, outlineFlagUnknownType)
		}
		if n.BlockType != "" {
			counts.Blocks++
			counts.ByType[n.BlockType]++
			if k != nil {
				if s, ok := k.GraphQLSchema(n.BlockType); ok && s.Label != nil {
					r.Label = *s.Label
				}
			}
		}
		r.Summary = rowSummary(obj)
		for _, fl := range r.Flags {
			counts.Flagged[fl]++
		}
		out = append(out, r)
	}
	counts.Rows = len(out)
	return out, counts
}

// arrayOf is the positional path of the array a row is in:
// layout[4].blocks[1] → layout[4].blocks.
func arrayOf(rowPath string) string {
	if i := strings.LastIndexByte(rowPath, '['); i > 0 {
		return rowPath[:i]
	}
	return rowPath
}

// knownBlockTypes is every blockType the knowledge can name: the entity's
// census and accepted sets, every other entity's census, and the shard's
// declared block schemas. ok is false when there is nothing to judge by.
func knownBlockTypes(k *discovery.BlockKnowledge) (map[string]bool, bool) {
	if k == nil {
		return nil, false
	}
	set := map[string]bool{}
	for _, s := range k.KnownTypes() {
		set[s] = true
	}
	if k.Census != nil {
		for s := range k.ProjectTypes() {
			set[s] = true
		}
	}
	if k.Shard != nil {
		for _, list := range k.Shard.Blocks {
			for _, s := range list {
				set[s] = true
			}
		}
	}
	if len(set) == 0 {
		return nil, false
	}
	return set, true
}

// knownBlocksArray reports an array whose pattern the knowledge knows as a
// blocks field — for an array whose rows are ALL untyped or empty.
func knownBlocksArray(k *discovery.BlockKnowledge, arrayPath string) bool {
	if k == nil {
		return false
	}
	pattern := discovery.NormalizeBlockPath(arrayPath)
	for _, p := range k.TopLevelPaths() {
		if p == pattern {
			return true
		}
	}
	if k.Entity != nil {
		if _, ok := k.Entity.Paths[pattern]; ok {
			return true
		}
	}
	return false
}

// summaryKeys are the heading-like fields a row's summary is taken from, in
// order of preference.
var summaryKeys = []string{"heading", "title", "headline", "eyebrow", "name", "label", "question", "phase", "caption", "tag", "alt"}

// mediaSummaryKeys are the upload/relationship fields a row without any text
// is summarised by ("[image 5]"), and url the last resort.
var mediaSummaryKeys = []string{"image", "media", "logo", "icon", "video", "file", "doc", "url"}

// richSummaryKeys are the rich-text fields tried first when no heading-like
// field has a value.
var richSummaryKeys = []string{"content", "richText", "text", "body", "intro", "description"}

// rowSummary is one line saying what a row is about.
func rowSummary(obj map[string]any) string {
	if len(obj) == 0 {
		return ""
	}
	if s := headingLike(obj); s != "" {
		return clip(s)
	}
	// One group level down: `link.label`, `section.heading`.
	for _, k := range sortedMapKeys(obj) {
		if sub, ok := obj[k].(map[string]any); ok && !lexical.IsEditorState(sub) {
			if s := headingLike(sub); s != "" {
				return clip(s)
			}
		}
	}
	for _, k := range richSummaryKeys {
		if s := richSummary(obj[k]); s != "" {
			return clip(s)
		}
	}
	for _, k := range sortedMapKeys(obj) {
		if st, ok := obj[k].(map[string]any); ok && lexical.IsEditorState(st) {
			if s := richSummary(st); s != "" {
				return clip(s)
			}
		}
	}
	for _, k := range mediaSummaryKeys {
		switch v := obj[k].(type) {
		case string:
			if v = strings.TrimSpace(v); v != "" {
				if k == "url" {
					return clip(v)
				}
				return clip("[" + k + " " + v + "]")
			}
		case json.Number:
			return "[" + k + " " + v.String() + "]"
		case map[string]any:
			if id, ok := v["id"]; ok && id != nil {
				return clip(fmt.Sprintf("[%s %v]", k, id))
			}
		}
	}
	return ""
}

func headingLike(obj map[string]any) string {
	for _, k := range summaryKeys {
		switch v := obj[k].(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		case map[string]any:
			if s := richSummary(v); s != "" {
				return s
			}
		}
	}
	return ""
}

// richSummary is the text of a rich-text value: its plain text, or — for a
// value holding only uploads/blocks — their placeholders. A string (a
// document read with --richtext md|text) is used as it is.
func richSummary(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case map[string]any:
		if !lexical.IsEditorState(t) {
			return ""
		}
		if s := lexical.PlainText(t); s != "" {
			return s
		}
		return lexical.PlainTextWithPlaceholders(t)
	}
	return ""
}

// clip joins paragraphs with " · ", collapses whitespace and cuts to
// outlineSummaryMax characters.
func clip(s string) string {
	parts := strings.Split(s, "\n\n")
	for i, p := range parts {
		parts[i] = strings.Join(strings.Fields(p), " ")
	}
	s = strings.Join(parts, " · ")
	if utf8.RuneCountInString(s) <= outlineSummaryMax {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:outlineSummaryMax-1])) + "…"
}

// outlineTable is the --output table rendering: one line per row, indented by
// depth with "· " (the table renderer trims leading spaces).
func outlineTable(list []outlineRow) []map[string]any {
	if len(list) == 0 {
		return []map[string]any{{"outline": "(no rows)", "summary": "", "flags": ""}}
	}
	out := make([]map[string]any, 0, len(list))
	for _, r := range list {
		label := relativeRowPath(r.Path)
		if r.BlockType != "" {
			label += " " + r.BlockType
		}
		flags := append([]string(nil), r.Flags...)
		sort.Strings(flags)
		out = append(out, map[string]any{
			"outline": strings.Repeat("· ", r.Depth) + label,
			"summary": r.Summary,
			"flags":   strings.Join(flags, ","),
		})
	}
	return out
}

// relativeRowPath is a row's path relative to its parent row:
// layout[4].blocks[1] → blocks[1]; a top-level row keeps its whole path.
func relativeRowPath(p string) string {
	cut := arrayOf(p)
	if i := strings.LastIndex(cut, "]."); i >= 0 {
		return p[i+2:]
	}
	return p
}
