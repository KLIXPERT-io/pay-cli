package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/rows"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

// `pay blocks` (§9.11) is the local half of the edit pipeline.
//
// The command it replaces is a three-step ritual every caller writes by hand:
// read the document to a file, edit the JSON with jq, write the array back with
// --set-json. That ritual has four failure modes PayCLI can see and jq cannot —
// the anchor index shifts once the moved row is lifted out, a duplicated row
// keeps its `id` and overwrites its original, an unknown `blockType` is dropped
// by Payload with a 201, and a document read at depth > 0 writes expanded
// relationships back as objects — so the edit belongs in the tool that knows
// the schema.
//
// Every verb here is L0: it reads one document from stdin, edits it, and writes
// it to stdout. Nothing reaches the network until `pay apply`.

func init() { Register(newBlocksCmd) }

// blocksIntro is the paragraph every verb's Long text opens with. It is shared
// so the pipeline contract is stated identically in all six.
const blocksIntro = `Reads ONE document from stdin, edits a blocks (or array) field, and writes the
document to stdout. Nothing is sent to Payload: pipe the result to ` + "`pay apply`" + ` to
write it back.

    pay get pages 12 --depth 0 | pay blocks mv type:cta --after type:mediaBlock | pay apply --yes`

func newBlocksCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		GroupID: GroupEdit,
		Use:     "blocks",
		Short:   "Edit a blocks field on a document read from stdin (local, no network)",
		Long: blocksIntro + `

Every verb addresses rows with the same closed selector grammar, and ` + "`pay blocks ls`" + `
prints a ready-made selector for each row. Prefer ` + "`id:`" + ` over an index: an index is
stale the moment another stage in the pipe inserts or removes a row.`,
	}
	cmd.AddCommand(
		newBlocksLsCmd(rt),
		newBlocksMvCmd(rt),
		newBlocksRmCmd(rt),
		newBlocksAddCmd(rt),
		newBlocksCpCmd(rt),
		newBlocksSetCmd(rt),
	)
	SetHelp(cmd, blocksFamilyHelp())
	return cmd
}

func blocksFamilyHelp() *Help {
	return &Help{
		Synopsis: []string{
			"pay blocks ls   [--field PATH] [--long]",
			"pay blocks mv   <selector> (--at N | --before SEL | --after SEL | --first | --last)",
			"pay blocks rm   <selector>... [--all]",
			"pay blocks add  <blockType> [--set k=v] [--set-json k=JSON] [--name NAME] [WHERE]",
			"pay blocks cp   <selector> [WHERE]",
			"pay blocks set  <selector> [--set k=v] [--set-json k=JSON] [--unset KEY]",
		},
		Output: OutputSpec{
			Kind:     output.KindDoc,
			Skeleton: `{"id": 12, "title": "Home", "layout": [ …the edited rows… ]}`,
		},
		ExitCodes: blocksExitCodes,
		Examples: []Example{
			{Why: "see what is on the page and how to address each row",
				Cmd: "pay get pages 12 --depth 0 | pay blocks ls"},
			{Why: "move the CTA below the media block",
				Cmd: "pay get pages 12 --depth 0 | pay blocks mv type:cta --after type:mediaBlock | pay apply --yes"},
			{Why: "drop one block, by the id `pay blocks ls` printed",
				Cmd: "pay get pages 12 --depth 0 | pay blocks rm id:67f3a1 | pay apply --yes"},
			{Why: "several edits in one write",
				Cmd: "pay get pages 12 --depth 0 | pay blocks rm type:content | pay blocks mv last --first | pay apply --yes"},
		},
		Mistakes: []Mistake{
			{Wrong: "Expecting a `pay blocks` verb to change anything in Payload.",
				Right: "The whole family is local: it reads a document from stdin and writes one to stdout. `pay apply` is the only stage that talks to the server."},
			{Wrong: "Reading the document without --depth 0.",
				Right: "Above depth 0 a relationship comes back as a whole document and is written back that way. Read with --depth 0; the pipeline warns with populated_relationship when it sees one."},
			{Wrong: "Reading without --draft and applying with it (or the reverse).",
				Right: "A read without --draft returns the PUBLISHED document. Use --draft on both ends of the pipe, or on neither, or you save published content over the draft."},
			{Wrong: "Addressing rows by index across several stages.",
				Right: "Each stage renumbers the array. `pay blocks ls` prints an `id:` selector per row that survives every later edit."},
		},
		SeeAlso: []string{
			"pay apply                                    # write the piped document back",
			"pay describe pages --field layout            # which blockTypes this field accepts",
			"pay describe pages --block cta               # what is INSIDE one block type",
		},
	}
}

// blocksExitCodes is the closed set the family can produce. There is no network
// exit code in it, which is the point: a `pay blocks` failure is always local
// and always the caller's input.
var blocksExitCodes = []int{
	apierr.ExitOK, apierr.ExitInternal, apierr.ExitNotFound,
	apierr.ExitValidation, apierr.ExitCapability,
}

// selectorHelp is the FlagInfo/ArgSpec detail shared by every verb.
func selectorArg(name string, required bool) ArgSpec {
	return ArgSpec{Name: name, Required: required, Type: "selector", Example: "id:67f3a1"}
}

// selectorNotes explains the grammar once, in the FLAGS block of every verb.
func selectorNotes() []string {
	out := []string{"A selector is one of:"}
	for _, form := range rows.SelectorForms {
		out = append(out, "  "+form)
	}
	return append(out,
		"`pay blocks ls` prints the shortest unambiguous selector for every row in .selector.",
		"An index is stale after the next stage edits the array; an `id:` is not.")
}

// ---------------------------------------------------------------------------
// shared plumbing
// ---------------------------------------------------------------------------

// stageFlags is the one flag every verb in the family shares.
type stageFlags struct {
	field string
}

func (f *stageFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.field, "field", "",
		"the blocks field to edit (default: the collection's only one)")
}

// stageHandler is a transform: it edits rf.Rows and describes what it did.
type stageHandler func(d *Deps, in *pipeInput, rf *rowField, args []string) (output.EditOp, error)

// runStage is runData for a local transform. It reads stdin, resolves the
// field, runs the handler and emits the edited document.
//
// It uses localDeps rather than dataDeps because the family must work with no
// profile at all: a caller editing a JSON file on disk is doing nothing that
// needs a Payload connection, and failing on auth_missing would make the tool
// useless exactly when it is most convenient.
func runStage(rt *Runtime, name string, h stageHandler) func(*cobra.Command, []string) error {
	return runLocalData(rt, name, func(ctx context.Context, cmd *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		f, _ := cmd.Context().Value(stageFlagsKey{}).(*stageFlags)
		in, err := readPipeInput(d, name)
		if err != nil {
			return nil, err
		}
		field := ""
		if f != nil {
			field = f.field
		}
		rf, warns, err := resolveRowField(ctx, d, in, field)
		if err != nil {
			return nil, err
		}
		op, err := h(d, in, rf, args)
		if err != nil {
			return nil, err
		}
		return emitStage(name, in, rf, op, warns), nil
	})
}

// stageFlagsKey carries the shared --field value to runStage without making
// every handler take it as a parameter.
type stageFlagsKey struct{}

// bindStage wires a verb's RunE, putting its stageFlags where runStage can see
// them.
func bindStage(rt *Runtime, cmd *cobra.Command, name string, f *stageFlags, h stageHandler) {
	f.register(cmd)
	run := runStage(rt, name, h)
	cmd.RunE = func(c *cobra.Command, args []string) error {
		ctx := c.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		c.SetContext(contextWithStageFlags(ctx, f))
		return run(c, args)
	}
}

// ---------------------------------------------------------------------------
// anchors
// ---------------------------------------------------------------------------

// anchorFlags is §9.11's destination grammar: exactly one of five flags.
//
// --before/--after take a SELECTOR rather than an index on purpose. "after the
// media block" survives another stage editing the array; "at index 3" does not,
// and an agent that computed 3 from an earlier listing is the single most
// common way a scripted reorder puts a block in the wrong place.
type anchorFlags struct {
	at     int
	before string
	after  string
	first  bool
	last   bool
}

func (a *anchorFlags) register(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.IntVar(&a.at, "at", 0, "destination index (0-based; -1 is the last position)")
	fl.StringVar(&a.before, "before", "", "put it immediately above the row this selector matches")
	fl.StringVar(&a.after, "after", "", "put it immediately below the row this selector matches")
	fl.BoolVar(&a.first, "first", false, "put it first")
	fl.BoolVar(&a.last, "last", false, "put it last")
}

// build resolves the flags to one anchor. required is true for `mv`, where
// "no destination" is not a sensible default — a move with no destination is
// a no-op, and a no-op that reports success is how a pipeline writes an
// unchanged document.
func (a *anchorFlags) build(cmd *cobra.Command, required bool) (rows.Anchor, error) {
	var given []string
	if cmd.Flags().Changed("at") {
		given = append(given, "--at")
	}
	for name, set := range map[string]bool{"--before": a.before != "", "--after": a.after != "", "--first": a.first, "--last": a.last} {
		if set {
			given = append(given, name)
		}
	}
	if len(given) > 1 {
		sortStrings(given)
		return rows.Anchor{}, apierr.New(apierr.CodeInvalidArgs,
			"%s were given together, and a row has one destination", strings.Join(given, ", "))
	}
	switch {
	case a.before != "":
		sel, err := parseSelector(a.before, "--before")
		return rows.Anchor{Mode: rows.ModeBefore, Sel: sel}, err
	case a.after != "":
		sel, err := parseSelector(a.after, "--after")
		return rows.Anchor{Mode: rows.ModeAfter, Sel: sel}, err
	case a.first:
		return rows.Anchor{Mode: rows.ModeIndex, Index: 0, Label: "first"}, nil
	case a.last:
		return rows.Anchor{Mode: rows.ModeIndex, Index: -1, Label: "last"}, nil
	case cmd.Flags().Changed("at"):
		return rows.Anchor{Mode: rows.ModeIndex, Index: a.at}, nil
	case required:
		return rows.Anchor{}, apierr.New(apierr.CodeInvalidArgs,
			"a move needs a destination").
			WithHint("pass one of --at N, --before SELECTOR, --after SELECTOR, --first or --last")
	default:
		return rows.Anchor{Mode: rows.ModeAppend}, nil
	}
}

func (a *anchorFlags) helpFlags() map[string]FlagInfo {
	return map[string]FlagInfo{
		"before": {Grammar: rows.SelectorGrammar},
		"after":  {Grammar: rows.SelectorGrammar},
		"at":     {Grammar: "N | -N", Note: "an array of n rows has n+1 insert positions; --at n appends"},
	}
}

// parseSelector wraps the grammar's error in PayCLI's.
func parseSelector(raw, where string) (rows.Selector, error) {
	sel, err := rows.ParseSelector(raw)
	if err != nil {
		return rows.Selector{}, apierr.New(apierr.CodeInvalidArgs, "%s: %s", where, err.Error()).
			WithHint("the grammar is: %s", rows.SelectorGrammar)
	}
	return sel, nil
}

// resolveErr converts the rows package's typed misses into PayCLI errors, with
// the rows that DO exist attached — a "no such block" that does not say what
// is there costs the caller another round trip to find out.
func resolveErr(err error, rf *rowField) error {
	switch e := err.(type) {
	case *rows.NoMatchError:
		return apierr.New(apierr.CodeSelectorNoMatch, "%s", e.Error()).
			WithDidYouMean(selectorSuggestions(e.Sel, e.Have)...).
			WithHint("%s", haveHint(e.Have))
	case *rows.AmbiguousError:
		return apierr.New(apierr.CodeSelectorAmbiguous, "%s", e.Error()).
			WithDidYouMean(matchedSelectors(e.Matched, e.Have)...).
			WithHint("address one row (the did_you_mean list is ready to paste), or pass --all where the verb accepts it")
	}
	if err != nil {
		return apierr.New(apierr.CodeInvalidArgs, "%s", err.Error())
	}
	return nil
}

// selectorSuggestions offers the real selectors closest to what was asked for.
func selectorSuggestions(sel rows.Selector, have []rows.Summary) []string {
	var pool []string
	for _, s := range have {
		switch sel.Kind {
		case rows.KindType:
			if s.BlockType != "" {
				pool = append(pool, "type:"+s.BlockType)
			}
		case rows.KindName:
			if s.BlockName != "" {
				pool = append(pool, "name:"+s.BlockName)
			}
		default:
			pool = append(pool, s.Selector)
		}
	}
	pool = dedupe(pool)
	if sel.Kind == rows.KindType || sel.Kind == rows.KindName {
		if near := apierr.DidYouMean(sel.Raw, pool); len(near) > 0 {
			return near
		}
	}
	return pool
}

func matchedSelectors(matched []int, have []rows.Summary) []string {
	out := make([]string, 0, len(matched))
	for _, i := range matched {
		if i >= 0 && i < len(have) {
			out = append(out, have[i].Selector)
		}
	}
	return out
}

func haveHint(have []rows.Summary) string {
	if len(have) == 0 {
		return "the field is empty; `pay blocks add <blockType>` puts the first row in it"
	}
	parts := make([]string, 0, len(have))
	for _, s := range have {
		label := s.Selector
		if s.BlockType != "" {
			label += " (" + s.BlockType + ")"
		}
		parts = append(parts, label)
	}
	return "the rows that exist are: " + strings.Join(parts, ", ")
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// pay blocks ls
// ---------------------------------------------------------------------------

func newBlocksLsCmd(rt *Runtime) *cobra.Command {
	f := &stageFlags{}
	var long bool
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List the rows of a blocks field, with a selector for each",
		Long: `Reads ONE document from stdin and lists its blocks. Nothing is edited and
nothing is sent to Payload.

Each row comes back with a ` + "`selector`" + ` that addresses it and no other row, so the
next command in the pipe can be written by pasting rather than by counting.`,
		Args: cobra.NoArgs,
	}
	cmd.Flags().BoolVar(&long, "long", false, "include each row's full JSON in .rows[].row")
	f.register(cmd)

	cmd.RunE = runLocalData(rt, safety.CmdBlocksList, func(ctx context.Context, c *cobra.Command, d *Deps, _ []string) (*output.Envelope, error) {
		in, err := readPipeInput(d, safety.CmdBlocksList)
		if err != nil {
			return nil, err
		}
		rf, warns, err := resolveRowField(ctx, d, in, f.field)
		if err != nil {
			return nil, err
		}
		list := rows.Summarize(rf.Rows)
		if long {
			for i := range list {
				list[i].Row = rf.Rows[i]
			}
		}
		data := map[string]any{
			"field": rf.Path,
			"count": len(list),
			"rows":  list,
		}
		if rf.Accepts != nil {
			data["accepts"] = rf.Accepts
		}
		// `ls` is the one verb that does not pass the document on: its data is
		// a listing, so it ends the pipe. It reports op_result for exactly that
		// reason — an agent must not mistake it for a document it can apply.
		env := output.New(safety.CmdBlocksList, output.KindOpResult, data)
		if in.Target != nil {
			env.WithTarget(in.Target)
		}
		for _, w := range append(in.Warnings, warns...) {
			env.AddWarning(w)
		}
		return env, nil
	})

	SetHelp(cmd, &Help{
		Synopsis: []string{"… | pay blocks ls [--field PATH] [--long]"},
		FlagInfo: map[string]FlagInfo{
			"field": {Grammar: "PATH", Note: "required only when the collection has more than one blocks field"},
		},
		Output: OutputSpec{
			Kind: output.KindOpResult,
			Skeleton: `{"field":"layout","count":3,"accepts":["cta","content","mediaBlock"],
 "rows":[{"index":0,"id":"67f3a1","block_type":"cta","block_name":"Top CTA",
          "selector":"id:67f3a1","fields":["richText","links"]}]}`,
		},
		ExitCodes: blocksExitCodes,
		Notes: append([]string{
			"`ls` ends a pipe: its data is a listing, not the document, so it cannot be piped into `pay apply`.",
		}, selectorNotes()...),
		Examples: []Example{
			{Why: "what is on this page, and how do I address each row?",
				Cmd: "pay get pages 12 --depth 0 | pay blocks ls"},
			{Why: "just the selectors, ready to paste",
				Cmd: "pay get pages 12 --depth 0 | pay blocks ls --path '.rows[].selector'"},
			{Why: "the whole of one block, to see its fields before editing them",
				Cmd: "pay get pages 12 --depth 0 | pay blocks ls --long --path '.rows[0].row'"},
			{Why: "a collection with two blocks fields",
				Cmd: "pay get pages 12 --depth 0 | pay blocks ls --field hero.items"},
		},
		Mistakes: []Mistake{
			{Wrong: "Piping `pay blocks ls` into `pay apply`.",
				Right: "`ls` emits a listing (data_kind op_result), not a document. Put the edit verbs in the pipe and apply that."},
			{Wrong: "Reading an index out of `ls` and using it two stages later.",
				Right: "Every insert or remove before it shifts that index. Use the `selector` field, which is an `id:` whenever the row has one."},
			{Wrong: "Expecting `.accepts` on a project that has never been discovered.",
				Right: "It is absent when no schema was available, and a blocks_field_inferred warning says so. Run `pay discover --refresh` once."},
			{Wrong: "Using `ls` to check a change before applying, then re-running the edit from the original.",
				Right: "Put `ls` at the END of the same pipe — `… | pay blocks mv … | pay blocks ls` — so it lists the edited rows, not the ones on the server."},
		},
		SeeAlso: []string{"pay describe <collection> --field <path> --blocks-detail"},
	})
	return cmd
}

// ---------------------------------------------------------------------------
// pay blocks mv
// ---------------------------------------------------------------------------

func newBlocksMvCmd(rt *Runtime) *cobra.Command {
	f := &stageFlags{}
	a := &anchorFlags{}
	cmd := &cobra.Command{
		Use:     "mv <selector>",
		Aliases: []string{"move"},
		Short:   "Move one row to another position",
		Long: blocksIntro + `

The destination is resolved against the array as it is NOW, then recomputed
after the row is lifted out. That is the arithmetic a hand-written reorder gets
wrong: moving row 0 after row 3 is an insert at index 2 of the remaining three
rows, not at index 4.`,
		Args: exactArgs(1, "pay blocks mv <selector> --after <selector>"),
	}
	a.register(cmd)
	bindStage(rt, cmd, safety.CmdBlocksMove, f, func(d *Deps, in *pipeInput, rf *rowField, args []string) (output.EditOp, error) {
		sel, err := parseSelector(args[0], "the selector")
		if err != nil {
			return output.EditOp{}, err
		}
		anchor, err := a.build(cmd, true)
		if err != nil {
			return output.EditOp{}, err
		}
		src, err := rows.ResolveOne(rf.Rows, sel, rf.Path)
		if err != nil {
			return output.EditOp{}, resolveErr(err, rf)
		}
		moved := rows.Summarize(rf.Rows)[src]
		out, at, err := rows.Move(rf.Rows, src, anchor)
		if err != nil {
			return output.EditOp{}, resolveErr(err, rf)
		}
		rf.Rows = out
		return output.EditOp{
			Detail:  fmt.Sprintf("moved %s from index %d to index %d%s", describeRow(moved), src, at, because(anchor)),
			Matched: []int{src},
		}, nil
	})

	SetHelp(cmd, &Help{
		Synopsis: []string{"… | pay blocks mv <selector> (--at N | --before SEL | --after SEL | --first | --last) [--field PATH]"},
		Args:     []ArgSpec{selectorArg("selector", true)},
		FlagInfo: a.helpFlags(),
		Output: OutputSpec{
			Kind:     output.KindDoc,
			Skeleton: `the whole document, with the field's rows reordered`,
		},
		ExitCodes: blocksExitCodes,
		Notes:     selectorNotes(),
		Examples: []Example{
			{Why: "the CTA belongs under the media block",
				Cmd: "pay get pages 12 --depth 0 | pay blocks mv type:cta --after type:mediaBlock | pay apply --yes"},
			{Why: "promote the last block to the top",
				Cmd: "pay get pages 12 --depth 0 | pay blocks mv last --first | pay apply --yes"},
			{Why: "an exact destination index",
				Cmd: "pay get pages 12 --depth 0 | pay blocks mv id:67f3a1 --at 2 | pay apply --yes"},
			{Why: "check the new order before writing anything",
				Cmd: "pay get pages 12 --depth 0 | pay blocks mv first --last | pay blocks ls"},
		},
		Mistakes: []Mistake{
			{Wrong: "`--after 3`, meaning \"after the row now at index 3\".",
				Right: "--before/--after take a SELECTOR. `--after 3` is the selector `3`, which is that row — correct here, but write `--after id:…` so it survives the next stage."},
			{Wrong: "Using `type:cta` when the page has two CTAs.",
				Right: "A move acts on exactly one row, so that is selector_ambiguous (exit 5) with both selectors listed. Use `type:cta[0]` or the row's `id:`."},
			{Wrong: "Expecting mv to write the change.",
				Right: "It writes the edited document to stdout. Pipe it to `pay apply` — that is the only stage that talks to Payload."},
		},
		SeeAlso: []string{"pay blocks ls", "pay apply --dry-run"},
	})
	return cmd
}

// ---------------------------------------------------------------------------
// pay blocks rm
// ---------------------------------------------------------------------------

func newBlocksRmCmd(rt *Runtime) *cobra.Command {
	f := &stageFlags{}
	var all bool
	cmd := &cobra.Command{
		Use:     "rm <selector>...",
		Aliases: []string{"remove", "del"},
		Short:   "Remove one or more rows",
		Long: blocksIntro + `

Several selectors may be given; they are all resolved against the array as it is
now, so the indices in a multi-row remove cannot shift under each other.`,
		Args: minArgs(1, "pay blocks rm <selector>..."),
	}
	cmd.Flags().BoolVar(&all, "all", false, "allow a selector that matches several rows to remove all of them")
	bindStage(rt, cmd, safety.CmdBlocksRemove, f, func(d *Deps, in *pipeInput, rf *rowField, args []string) (output.EditOp, error) {
		summaries := rows.Summarize(rf.Rows)
		var hit []int
		var labels []string
		for _, raw := range args {
			sel, err := parseSelector(raw, "the selector")
			if err != nil {
				return output.EditOp{}, err
			}
			var idx []int
			if all {
				idx, err = rows.ResolveMany(rf.Rows, sel, rf.Path)
			} else {
				var one int
				one, err = rows.ResolveOne(rf.Rows, sel, rf.Path)
				idx = []int{one}
			}
			if err != nil {
				return output.EditOp{}, resolveErr(err, rf)
			}
			for _, i := range idx {
				hit = append(hit, i)
				labels = append(labels, describeRow(summaries[i]))
			}
		}
		hit = dedupeInts(hit)
		rf.Rows = rows.Remove(rf.Rows, hit)
		return output.EditOp{
			Detail:  fmt.Sprintf("removed %d row(s): %s", len(hit), strings.Join(dedupe(labels), ", ")),
			Matched: hit,
		}, nil
	})

	SetHelp(cmd, &Help{
		Synopsis: []string{"… | pay blocks rm <selector>... [--all] [--field PATH]"},
		Args:     []ArgSpec{selectorArg("selector", true)},
		FlagInfo: map[string]FlagInfo{
			"all": {Note: "without it, a selector matching several rows is selector_ambiguous (exit 5) rather than a wider delete than intended"},
		},
		Output: OutputSpec{
			Kind:     output.KindDoc,
			Skeleton: `the whole document, with the named rows gone`,
		},
		ExitCodes: blocksExitCodes,
		Notes:     selectorNotes(),
		Examples: []Example{
			{Why: "drop one block by its id",
				Cmd: "pay get pages 12 --depth 0 | pay blocks rm id:67f3a1 | pay apply --yes"},
			{Why: "drop two in one pass",
				Cmd: "pay get pages 12 --depth 0 | pay blocks rm id:67f3a1 id:67f3b2 | pay apply --yes"},
			{Why: "drop every block of one type",
				Cmd: "pay get pages 12 --depth 0 | pay blocks rm type:content --all | pay apply --yes"},
			{Why: "see what would be left, without writing",
				Cmd: "pay get pages 12 --depth 0 | pay blocks rm last | pay blocks ls"},
		},
		Mistakes: []Mistake{
			{Wrong: "`rm type:cta` on a page with two CTAs, expecting both to go.",
				Right: "That is selector_ambiguous (exit 5). Pass --all to mean every match, which makes the blast radius explicit in the command."},
			{Wrong: "Removing by index in a pipeline that already removed something.",
				Right: "Each stage renumbers the array. Address rows by `id:`, or do both removes in ONE `rm` — its selectors are resolved together."},
			{Wrong: "Expecting `rm` of a row that is already gone to be a no-op.",
				Right: "It is selector_no_match (exit 4), with the rows that DO exist listed in the hint. A remove that silently matched nothing would report success having changed nothing."},
			{Wrong: "Treating `rm` as a delete against Payload.",
				Right: "It removes a row from the array in the piped document. Nothing is written until `pay apply`, and nothing is written at all if you drop the pipe."},
		},
		SeeAlso: []string{"pay blocks ls", "pay apply --dry-run"},
	})
	return cmd
}

func dedupeInts(in []int) []int {
	seen := map[int]bool{}
	out := in[:0:0]
	for _, i := range in {
		if !seen[i] {
			seen[i] = true
			out = append(out, i)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// pay blocks add
// ---------------------------------------------------------------------------

func newBlocksAddCmd(rt *Runtime) *cobra.Command {
	f := &stageFlags{}
	a := &anchorFlags{}
	b := &blockData{}
	cmd := &cobra.Command{
		Use:     "add <blockType>",
		Aliases: []string{"insert"},
		Short:   "Insert a new block row",
		Long: blocksIntro + `

blockType must be the block's SLUG, not its GraphQL interface name. It is
checked against what this field accepts before anything is written, because
Payload DROPS a row whose blockType it does not recognise and still answers 201:
the mistake is otherwise invisible until someone notices the block is missing.`,
		Args: exactArgs(1, "pay blocks add <blockType> --set k=v"),
	}
	a.register(cmd)
	b.register(cmd)
	bindStage(rt, cmd, safety.CmdBlocksAdd, f, func(d *Deps, in *pipeInput, rf *rowField, args []string) (output.EditOp, error) {
		slug := strings.TrimSpace(args[0])
		if err := checkBlockType(rf, slug); err != nil {
			return output.EditOp{}, err
		}
		anchor, err := a.build(cmd, false)
		if err != nil {
			return output.EditOp{}, err
		}
		row, err := b.build()
		if err != nil {
			return output.EditOp{}, err
		}
		// blockType is set last so no --data or --set-json can shadow the
		// positional argument the caller and PayCLI both validated.
		row[rows.KeyBlockType] = slug
		rows.StripIDs(row)

		out, at, err := rows.Insert(rf.Rows, row, anchor)
		if err != nil {
			return output.EditOp{}, resolveErr(err, rf)
		}
		rf.Rows = out
		return output.EditOp{Detail: fmt.Sprintf("added one %s row at index %d%s", slug, at, because(anchor))}, nil
	})

	SetHelp(cmd, &Help{
		Synopsis: []string{
			"… | pay blocks add <blockType> [--set k=v ...] [--set-json k=JSON ...] [--name NAME]",
			"                              [--at N | --before SEL | --after SEL | --first | --last]",
		},
		Args: []ArgSpec{{Name: "blockType", Required: true, Type: "enum",
			ValuesFrom: "discovery.blocks", Example: "cta"}},
		FlagInfo: mergeFlagInfo(a.helpFlags(), map[string]FlagInfo{
			"set":      {Grammar: "KEY=VALUE", Repeatable: true},
			"set-json": {Grammar: "KEY=JSON", Repeatable: true},
			"data":     {Grammar: "JSON | @FILE", Note: "@- is not accepted here: stdin already holds the document"},
			"name":     {Grammar: "NAME", Note: "sets blockName, the admin-UI label; it is not content"},
		}),
		Output: OutputSpec{
			Kind:     output.KindDoc,
			Skeleton: `the whole document, with the new row inserted`,
		},
		ExitCodes: blocksExitCodes,
		Notes: append([]string{
			"The new row is inserted at the END unless a destination flag says otherwise.",
			"Every `id` in the row is stripped: ids are server-generated, and reusing one rewrites an existing row.",
		}, selectorNotes()...),
		Examples: []Example{
			{Why: "append an empty block of a type this field accepts",
				Cmd: "pay get pages 12 --depth 0 | pay blocks add mediaBlock | pay apply --yes"},
			{Why: "with a field set, placed under the hero",
				Cmd: "pay get pages 12 --depth 0 | pay blocks add cta --set-json richText=\"$(cat rt.json)\" --after 0 | pay apply --yes"},
			{Why: "a whole row prepared in a file",
				Cmd: "pay get pages 12 --depth 0 | pay blocks add cta --data @cta.json --first | pay apply --yes"},
			{Why: "what may go in this field, and what is inside one",
				Cmd: "pay describe pages --field layout --path .block_types && pay describe pages --block cta"},
		},
		Mistakes: []Mistake{
			{Wrong: "Passing the GraphQL interface name (`CallToActionBlock`).",
				Right: "blockType is the slug from the block's config.ts (`cta`). `pay describe <collection> --field <path> --path .block_types` lists the slugs."},
			{Wrong: "`--data @-` to read the row from stdin.",
				Right: "stdin already carries the document being edited. Use `--data @file`, or `--set-json` with a shell substitution."},
			{Wrong: "Copying a row out of another document, ids and all.",
				Right: "add strips every `id` at every depth. A kept id makes Payload rewrite the row that already has it instead of adding one."},
		},
		SeeAlso: []string{"pay describe <collection> --block <slug>", "pay blocks cp"},
	})
	return cmd
}

// ---------------------------------------------------------------------------
// pay blocks cp
// ---------------------------------------------------------------------------

func newBlocksCpCmd(rt *Runtime) *cobra.Command {
	f := &stageFlags{}
	a := &anchorFlags{}
	cmd := &cobra.Command{
		Use:     "cp <selector>",
		Aliases: []string{"duplicate"},
		Short:   "Duplicate a row",
		Long: blocksIntro + `

The copy is stripped of every ` + "`id`" + `, at the top level and at every depth. That is
not optional: Payload matches a row by its id, so a duplicate that kept its ids
does not add a block — it overwrites the original and the array loses a row.`,
		Args: exactArgs(1, "pay blocks cp <selector> --after <selector>"),
	}
	a.register(cmd)
	bindStage(rt, cmd, safety.CmdBlocksCopy, f, func(d *Deps, in *pipeInput, rf *rowField, args []string) (output.EditOp, error) {
		sel, err := parseSelector(args[0], "the selector")
		if err != nil {
			return output.EditOp{}, err
		}
		anchor, err := a.build(cmd, false)
		if err != nil {
			return output.EditOp{}, err
		}
		src, err := rows.ResolveOne(rf.Rows, sel, rf.Path)
		if err != nil {
			return output.EditOp{}, resolveErr(err, rf)
		}
		copied := rows.Summarize(rf.Rows)[src]
		out, at, err := rows.Copy(rf.Rows, src, anchor)
		if err != nil {
			return output.EditOp{}, resolveErr(err, rf)
		}
		rf.Rows = out
		return output.EditOp{
			Detail:  fmt.Sprintf("copied %s to index %d%s, without its ids", describeRow(copied), at, because(anchor)),
			Matched: []int{src},
		}, nil
	})

	SetHelp(cmd, &Help{
		Synopsis: []string{"… | pay blocks cp <selector> [--at N | --before SEL | --after SEL | --first | --last]"},
		Args:     []ArgSpec{selectorArg("selector", true)},
		FlagInfo: a.helpFlags(),
		Output: OutputSpec{
			Kind:     output.KindDoc,
			Skeleton: `the whole document, with a copy of the row inserted`,
		},
		ExitCodes: blocksExitCodes,
		Notes: append([]string{
			"The copy is appended unless a destination flag says otherwise.",
			"Every `id` is stripped from the copy, at every depth.",
		}, selectorNotes()...),
		Examples: []Example{
			{Why: "a second CTA just like the first, right after it",
				Cmd: "pay get pages 12 --depth 0 | pay blocks cp type:cta[0] --after type:cta[0] | pay apply --yes"},
			{Why: "duplicate and then edit the copy in the same pipe",
				Cmd: "pay get pages 12 --depth 0 | pay blocks cp first --last | pay blocks set last --set blockName='Repeat CTA' | pay apply --yes"},
		},
		Mistakes: []Mistake{
			{Wrong: "Expecting the copy to keep the original's id so it can be addressed by it.",
				Right: "It has no id until Payload assigns one. Address it by position in the same pipe (`last`, `--at`), then re-read to get its id."},
			{Wrong: "Duplicating with jq instead, keeping the ids.",
				Right: "Payload matches a row by id, so the \"copy\" overwrites its original and the array loses an entry. `cp` strips every id at every depth."},
			{Wrong: "`cp type:cta` on a page with two CTAs.",
				Right: "A copy has exactly one source, so that is selector_ambiguous (exit 5). Use `type:cta[0]` or the row's `id:`."},
		},
		SeeAlso: []string{"pay blocks add", "pay blocks set"},
	})
	return cmd
}

// ---------------------------------------------------------------------------
// pay blocks set
// ---------------------------------------------------------------------------

func newBlocksSetCmd(rt *Runtime) *cobra.Command {
	f := &stageFlags{}
	b := &blockData{}
	var unset []string
	cmd := &cobra.Command{
		Use:   "set <selector>",
		Short: "Set or clear fields inside one row",
		Long: blocksIntro + `

Keys are dotted paths INTO the row, not into the document: ` + "`--set blockName=Hero`" + `
and ` + "`--set links.0.label=Buy`" + `, never ` + "`--set layout.0.blockName=Hero`" + `.`,
		Args: exactArgs(1, "pay blocks set <selector> --set k=v"),
	}
	b.register(cmd)
	cmd.Flags().StringArrayVar(&unset, "unset", nil, "delete this key from the row (repeatable)")
	bindStage(rt, cmd, safety.CmdBlocksSet, f, func(d *Deps, in *pipeInput, rf *rowField, args []string) (output.EditOp, error) {
		sel, err := parseSelector(args[0], "the selector")
		if err != nil {
			return output.EditOp{}, err
		}
		patch, err := b.build()
		if err != nil {
			return output.EditOp{}, err
		}
		if len(patch) == 0 && len(unset) == 0 {
			return output.EditOp{}, apierr.New(apierr.CodeInvalidArgs,
				"`pay blocks set` was given nothing to change").
				WithHint("pass --set k=v, --set-json k=JSON or --unset KEY")
		}
		if _, ok := patch[rows.KeyBlockType]; ok {
			if err := checkBlockType(rf, fmt.Sprint(patch[rows.KeyBlockType])); err != nil {
				return output.EditOp{}, err
			}
		}
		idx, err := rows.ResolveOne(rf.Rows, sel, rf.Path)
		if err != nil {
			return output.EditOp{}, resolveErr(err, rf)
		}
		target := rows.Summarize(rf.Rows)[idx]
		out, err := rows.Set(rf.Rows, idx, patch, unset)
		if err != nil {
			return output.EditOp{}, resolveErr(err, rf)
		}
		rf.Rows = out
		return output.EditOp{
			Detail:  fmt.Sprintf("set %s on %s", strings.Join(changedKeys(patch, unset), ", "), describeRow(target)),
			Matched: []int{idx},
		}, nil
	})

	SetHelp(cmd, &Help{
		Synopsis: []string{"… | pay blocks set <selector> [--set k=v ...] [--set-json k=JSON ...] [--unset KEY ...]"},
		Args:     []ArgSpec{selectorArg("selector", true)},
		FlagInfo: map[string]FlagInfo{
			"set":      {Grammar: "KEY=VALUE", Repeatable: true, Note: "KEY is a path inside the ROW; values are typed (true, 12, null stay scalars)"},
			"set-json": {Grammar: "KEY=JSON", Repeatable: true, Note: "the escape hatch for an object or array — a lexical richText value goes here"},
			"unset":    {Grammar: "KEY", Repeatable: true, Note: "a key that is already absent is not an error"},
			"data":     {Grammar: "JSON | @FILE", Note: "@- is not accepted here: stdin already holds the document"},
		},
		Output: OutputSpec{
			Kind:     output.KindDoc,
			Skeleton: `the whole document, with the row's fields changed`,
		},
		ExitCodes: blocksExitCodes,
		Notes: append([]string{
			"A lexical rich-text field is JSON, never a string: pass the editorState object through --set-json.",
		}, selectorNotes()...),
		Examples: []Example{
			{Why: "relabel a block in the admin UI",
				Cmd: "pay get pages 12 --depth 0 | pay blocks set id:67f3a1 --set blockName='Hero CTA' | pay apply --yes"},
			{Why: "replace a lexical rich-text value",
				Cmd: "pay get pages 12 --depth 0 | pay blocks set type:cta[0] --set-json richText=\"$(cat rt.json)\" | pay apply --yes"},
			{Why: "point a media block at another upload",
				Cmd: "pay get pages 12 --depth 0 | pay blocks set type:mediaBlock --set media=7 | pay apply --yes"},
			{Why: "clear an optional field",
				Cmd: "pay get pages 12 --depth 0 | pay blocks set last --unset blockName | pay apply --yes"},
			{Why: "what fields does this block have?",
				Cmd: "pay describe pages --block cta --path '.block.fields[].path'"},
		},
		Mistakes: []Mistake{
			{Wrong: "`--set layout.0.blockName=Hero`.",
				Right: "The selector already chose the row. The key is a path inside it: `--set blockName=Hero`."},
			{Wrong: "`--set richText='<p>hi</p>'` on a lexical field.",
				Right: "Lexical stores JSON, and a string is rejected or stored as nonsense. Build the editorState object and pass it with --set-json."},
			{Wrong: "`--set media='{\"id\":7}'` for a relationship.",
				Right: "Send the id: `--set media=7`. An object here is how a relationship gets rewritten as an embedded copy."},
		},
		SeeAlso: []string{"pay describe <collection> --block <slug>", "pay blocks ls --long"},
	})
	return cmd
}

func changedKeys(patch map[string]any, unset []string) []string {
	out := make([]string, 0, len(patch)+len(unset))
	for k := range patch {
		out = append(out, k)
	}
	sortStrings(out)
	for _, k := range unset {
		out = append(out, k+" (unset)")
	}
	return out
}

// ---------------------------------------------------------------------------
// row bodies
// ---------------------------------------------------------------------------

// blockData is the row-level equivalent of §9.10.2's writeData. It is separate
// from it because the keys address a BLOCK's fields, not a collection's, and
// the collection-level coercion writeData performs would type them against the
// wrong schema.
type blockData struct {
	data    string
	set     []string
	setJSON []string
	name    string
}

func (b *blockData) register(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVar(&b.data, "data", "", "the row as a JSON object, or @FILE (never @-: stdin holds the document)")
	fl.StringArrayVar(&b.set, "set", nil, "k=v inside the row (repeatable, typed)")
	fl.StringArrayVar(&b.setJSON, "set-json", nil, "k=JSON inside the row (repeatable, no coercion)")
	fl.StringVar(&b.name, "name", "", "set blockName, the admin-UI label")
}

// build merges the flags into one row patch, later flags winning.
func (b *blockData) build() (map[string]any, error) {
	row := map[string]any{}

	if b.data != "" {
		raw, err := b.readData()
		if err != nil {
			return nil, err
		}
		obj, err := decodeObject("--data", raw)
		if err != nil {
			return nil, err
		}
		deepMerge(row, obj)
	}
	for _, pair := range b.set {
		key, raw, ok := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, apierr.New(apierr.CodeInvalidArgs, "--set %q is not key=value", pair)
		}
		v, err := query.TypeValue(raw)
		if err != nil {
			return nil, err
		}
		row[key] = v
	}
	for _, pair := range b.setJSON {
		key, raw, ok := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, apierr.New(apierr.CodeInvalidArgs, "--set-json %q is not key=JSON", pair)
		}
		var v any
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			return nil, apierr.New(apierr.CodeInvalidArgs,
				"--set-json %s: the value is not valid JSON", key).
				WithHint(`quote it for the shell: --set-json 'links=[{"label":"Buy"}]'`)
		}
		row[key] = v
	}
	if b.name != "" {
		row[rows.KeyBlockName] = b.name
	}
	return row, nil
}

// readData resolves --data. `@-` is refused rather than silently read: stdin is
// the document being edited, so consuming it here would swallow the pipeline's
// input and leave a confusing "stdin was empty" three stages later.
func (b *blockData) readData() ([]byte, error) {
	switch {
	case b.data == "@-":
		return nil, apierr.New(apierr.CodeInvalidArgs,
			"--data @- cannot be used here: stdin already carries the document being edited").
			WithHint("use --data @FILE, or --set-json with a shell substitution: --set-json 'x=\"$(cat x.json)\"'")
	case strings.HasPrefix(b.data, "@"):
		name := strings.TrimPrefix(b.data, "@")
		raw, err := os.ReadFile(name)
		if err != nil {
			return nil, apierr.Wrap(err, apierr.CodeFileMissing, "--data @%s could not be read", name)
		}
		return raw, nil
	default:
		return []byte(b.data), nil
	}
}

// because renders the destination the caller asked for, when it says more than
// the resulting index already does. `--at 2` landing at index 2 needs no
// parenthetical; `--after type:mediaBlock` landing at index 2 does.
func because(a rows.Anchor) string {
	if d := a.Describe(); d != "" {
		return " (" + d + ")"
	}
	return ""
}

// describeRow labels a row for an op log or an error: its selector plus its
// blockType, which together identify it to a human reading the edit list.
func describeRow(s rows.Summary) string {
	if s.BlockType == "" {
		return s.Selector
	}
	return fmt.Sprintf("%s (%s)", s.Selector, s.BlockType)
}

func mergeFlagInfo(maps ...map[string]FlagInfo) map[string]FlagInfo {
	out := map[string]FlagInfo{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}
