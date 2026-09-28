package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
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

const (
	// arrayRowArg is the `pay blocks add` type argument for a row of a plain
	// array field, which has no blockType.
	arrayRowArg = "-"
	// WarnBlockTypeOmitted marks a `pay blocks add <blockType>` whose target
	// is a plain array field: the new row was added without the blockType.
	WarnBlockTypeOmitted = "block_type_omitted"
)

// orThisField names rf for a message and a pasteable --field: its stable path,
// or "this field" for bare rows.
func orThisField(rf *rowField) string {
	if rf == nil || rf.Stable == "" {
		return "this field"
	}
	return rf.Stable
}

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
		Short:   "Edit a blocks field on a document read from stdin; learn the site's block types (types/schema/new)",
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
	// §7.10c's catalog verbs: they learn what blocks exist from the live site.
	cmd.AddCommand(blocksCatalogCmds(rt)...)
	SetHelp(cmd, withCatalogHelp(blocksFamilyHelp()))
	return cmd
}

func blocksFamilyHelp() *Help {
	return &Help{
		Synopsis: []string{
			"pay blocks ls   [--field PATH] [--recursive] [--long]",
			"pay blocks mv   <selector> (--at N | --before SEL | --after SEL | --first | --last) [--field PATH]",
			"pay blocks rm   <selector>... [--all] [--field PATH]",
			"pay blocks add  <blockType|-> [--set k=v] [--set-json k=JSON] [--name NAME] [WHERE] [--field PATH]",
			"pay blocks cp   <selector> [WHERE] [--field PATH]",
			"pay blocks set  <selector> [--set k=v] [--set-json k=JSON] [--unset KEY] [--field PATH]",
			"PATH:           " + rows.PathGrammar,
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
			{Why: "every row at every depth, with the --field that reaches each nested array",
				Cmd: "pay get pages 12 --depth 0 | pay blocks ls --recursive"},
			{Why: "reorder the blocks INSIDE a group; apply writes the whole of layout",
				Cmd: "pay get pages 12 --draft --depth 0 | pay blocks mv last --first --field 'layout[type:group].blocks' | pay apply --draft --yes"},
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
			{Wrong: "Editing a group's inner blocks with jq and `pay update --set-json layout=…`.",
				Right: "Every verb takes a nested --field (`layout[id:…].blocks`, `layout[2].blocks[0].buttons`). edits.fields records the top-level field, so `pay apply` PATCHes the whole of layout with the nested change inside it."},
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
		"the blocks (or array) field to edit, at any depth (default: the document's only top-level blocks field)")
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
		if in.RichText != "" {
			// The edited document goes on to `pay apply`; refuse it here,
			// where the caller can still re-read, not three stages later.
			return nil, renderedError("pay "+name, in.RichText)
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
		return emitStage(name, in, rf, op, warns)
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
		"field":  fieldFlagInfo(),
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
// It matches with errors.As rather than a type switch: the rows package returns
// these bare today, but a future caller wrapping one with %w must not silently
// downgrade a precise selector_no_match into a generic invalid_args.
func resolveErr(err error, rf *rowField) error {
	var miss *rows.NoMatchError
	if errors.As(err, &miss) {
		// An anchor (--before/--after) is resolved inside the rows package,
		// which does not know where the array is; name it here.
		anchor := miss.Field == "" && rf != nil && rf.Path != ""
		if anchor {
			miss.Field = rf.Path
		}
		// `pay blocks rm id:<a nested row>` without --field: the id exists,
		// just in another array. Say which, ready to paste.
		if miss.Sel.Kind == rows.KindID && rf != nil && rf.Doc != nil {
			if n, ok := rows.FindRow(rf.Doc, miss.Sel.Value); ok && n.Field != rf.Stable {
				if anchor {
					return apierr.New(apierr.CodeSelectorNoMatch,
						"%s; the row id:%s is at %s, in another array (--field %s), and a destination must be a row of the SAME array",
						miss.Error(), miss.Sel.Value, n.Path, n.Field).
						WithHint("no verb moves a row between arrays: copy it (`pay blocks ls --long`, then `pay blocks add - --data @row.json` or `pay blocks add <blockType> --data @row.json` with --field on the target) and `pay blocks rm` it from the source")
				}
				return apierr.New(apierr.CodeSelectorNoMatch,
					"%s; the row id:%s is at %s, in the array --field %s", miss.Error(), miss.Sel.Value, n.Path, n.Field).
					WithHint("repeat the command with --field '%s'", n.Field)
			}
		}
		return apierr.New(apierr.CodeSelectorNoMatch, "%s", miss.Error()).
			WithDidYouMean(selectorSuggestions(miss.Sel, miss.Have)...).
			WithHint("%s", haveHint(miss.Have))
	}
	var amb *rows.AmbiguousError
	if errors.As(err, &amb) {
		if amb.Field == "" && rf != nil {
			amb.Field = rf.Path
		}
		return apierr.New(apierr.CodeSelectorAmbiguous, "%s", amb.Error()).
			WithDidYouMean(matchedSelectors(amb.Matched, amb.Have)...).
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
			pool = near
		}
	}
	if sel.Kind == rows.KindType {
		return expandTypeSelectors(pool, have)
	}
	return pool
}

// expandTypeSelectors turns a `type:X` suggestion that would itself be
// ambiguous (the array holds several X rows) into the `type:X[n]` forms that
// exist, so every suggestion is ready to paste: `type:group[9]` on a page with
// four groups suggests type:group[0]…[3], never the ambiguous type:group.
func expandTypeSelectors(pool []string, have []rows.Summary) []string {
	count := map[string]int{}
	for _, s := range have {
		if s.BlockType != "" {
			count[s.BlockType]++
		}
	}
	var out []string
	for _, p := range pool {
		slug := strings.TrimPrefix(p, "type:")
		n := count[slug]
		if n <= 1 || slug == p {
			out = append(out, p)
			continue
		}
		for i := 0; i < n; i++ {
			out = append(out, fmt.Sprintf("type:%s[%d]", slug, i))
		}
	}
	return out
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
	var long, recursive bool
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List the rows of a blocks field, with a selector for each",
		Long: `Reads ONE document from stdin and lists its blocks. Nothing is edited and
nothing is sent to Payload.

Each row comes back with a ` + "`selector`" + ` that addresses it and no other row, so the
next command in the pipe can be written by pasting rather than by counting.

--recursive also lists every row nested inside those rows — a group's blocks, a
grid inside that group, a button list — each with the exact ` + "`--field`" + ` value
that addresses its array, so an edit at any depth is written by pasting too.`,
		Args: cobra.NoArgs,
	}
	cmd.Flags().BoolVar(&long, "long", false, "include each row's full JSON in .rows[].row")
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false,
		"also list the rows of every array nested inside the listed rows, at every depth")
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
		data := map[string]any{
			// field is the form to paste into the next --field: it names each
			// row on the way by a selector that survives a reorder of the
			// parent array. path is the positional form, for reading.
			"field": rf.Stable,
			"count": len(rf.Rows),
		}
		if rf.Nested {
			data["path"] = rf.Path
		}
		if recursive {
			nodes := rows.Tree(rf.Rows, rf.Loc)
			describeNestedArrays(in.Doc, rf, nodes)
			if rf.Path == "" {
				// Bare rows (`--path .layout` on the read): no --field can
				// address them, so none is printed — `[id:…].blocks` is not
				// a path any verb accepts.
				for i := range nodes {
					nodes[i].Field = ""
				}
			}
			if long {
				for i := range nodes {
					nodes[i].Row = nodes[i].Value
				}
			}
			data["total"] = len(nodes)
			data["rows"] = nodes
		} else {
			list := rows.Summarize(rf.Rows)
			if long {
				for i := range list {
					list[i].Row = rf.Rows[i]
				}
			}
			data["rows"] = list
		}
		if acc := acceptsFor(rf); acc != nil {
			// accepts_source says whether the list is declared (and complete
			// when accepts_authoritative) or merely what real documents
			// contain (observed-census) — a slug outside an observed list may
			// still be valid.
			data["accepts"] = acc.Slugs
			data["accepts_source"] = acc.Source
			data["accepts_authoritative"] = acc.Authoritative
			data["accepts_pattern"] = acc.Pattern
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
		Synopsis: []string{"… | pay blocks ls [--field PATH] [--recursive] [--long]"},
		FlagInfo: map[string]FlagInfo{
			"field": {Grammar: "PATH", Note: "needed only for a nested array, or when the document has more than one top-level blocks field; " + fieldFlagNote},
			"recursive": {Note: "every row at every depth, parents before children; each carries depth, path (positional), " +
				"field (the --field to paste for its array), nested{} (each array inside the row → its row count) and arrays{} " +
				"(each of those → kind blocks|array and, when known, accepts/accepts_source/accepts_authoritative)"},
		},
		Output: OutputSpec{
			Kind: output.KindOpResult,
			Skeleton: `{"field":"layout","count":3,"accepts":["cta","content","mediaBlock"],
 "accepts_source":"graphql","accepts_authoritative":true,"accepts_pattern":"layout",
 "rows":[{"index":0,"id":"67f3a1","block_type":"cta","block_name":"Top CTA",
          "selector":"id:67f3a1","fields":["richText","links"]}]}
with --recursive:
{"field":"layout","count":6,"total":13,
 "rows":[{"depth":0,"path":"layout[2]","field":"layout","index":2,"id":"6ab8…cd2",
          "block_type":"group","selector":"id:6ab8…cd2","nested":{"blocks":6},
          "arrays":{"blocks":{"kind":"blocks","accepts":["processStep"],"accepts_source":"observed-census","accepts_authoritative":false}}},
         {"depth":1,"path":"layout[2].blocks[0]","field":"layout[id:6ab8…cd2].blocks","index":0,
          "id":"6ab8…ccc","block_type":"processStep","selector":"id:6ab8…ccc"}]}`,
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
			{Why: "every row at every depth, each with the --field that reaches its array",
				Cmd: "pay get pages 12 --depth 0 | pay blocks ls --recursive"},
			{Why: "the rows inside the page's only group block",
				Cmd: "pay get pages 12 --depth 0 | pay blocks ls --field 'layout[type:group].blocks'"},
		},
		Mistakes: []Mistake{
			{Wrong: "Piping `pay blocks ls` into `pay apply`.",
				Right: "`ls` emits a listing (data_kind op_result), not a document. Put the edit verbs in the pipe and apply that."},
			{Wrong: "Reading an index out of `ls` and using it two stages later.",
				Right: "Every insert or remove before it shifts that index. Use the `selector` field, which is an `id:` whenever the row has one."},
			{Wrong: "Expecting `.accepts` on a project that has never been discovered, or reading an observed list as complete.",
				Right: "It is absent when neither a declared schema nor a cached block census knows the field (`pay blocks learn <collection>` builds the census). `accepts_source: observed-census` with `accepts_authoritative: false` is what real documents contain, not everything the field allows."},
			{Wrong: "`--field layout.blocks` for the blocks inside a group.",
				Right: "layout is an array, so the path must name the ROW first: `layout[2].blocks` or `layout[id:…].blocks` (did_you_mean lists them). A path that does not resolve is an error, never an empty list."},
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
			{Why: "reorder the blocks INSIDE the page's group block",
				Cmd: "pay get pages 12 --depth 0 | pay blocks mv last --first --field 'layout[type:group].blocks' | pay apply --yes"},
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
			"all":   {Note: "without it, a selector matching several rows is selector_ambiguous (exit 5) rather than a wider delete than intended"},
			"field": fieldFlagInfo(),
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
			{Why: "drop a row nested in a grid inside a group (paste the field from `ls --recursive`)",
				Cmd: "pay get pages 12 --depth 0 | pay blocks rm id:6ab9…c15 --field 'layout[id:6ab9…c1f].blocks[id:6ab9…c1e].blocks' | pay apply --yes"},
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
		// The piped document's collection is unknown at TAB time, so every
		// slug the cached block census saw is offered.
		ValidArgsFunction: CompleteCensusBlockTypes(rt),
		Short:             "Insert a new block row",
		Long: blocksIntro + `

blockType must be the block's SLUG, not its GraphQL interface name. It is
checked against what this field accepts before anything is written, because
Payload DROPS a row whose blockType it does not recognise and still answers 201:
the mistake is otherwise invisible until someone notices the block is missing.
A DECLARED list (profile pin, project source, GraphQL) refuses a slug it does
not name (block_type_unknown, exit 10); the cached block census only WARNS
(unknown_block_type with a did-you-mean, or block_type_not_observed_here), and
without either nothing is checked here.

A plain ARRAY field (a cta's links, a card's buttons) holds rows without a
blockType: pass ` + "`-`" + ` as the type. A blockType given for a field known to be a
plain array (schema, block census, or its rows) is dropped with a
block_type_omitted warning.`,
		Args: exactArgs(1, "pay blocks add <blockType> --set k=v   (or `pay blocks add - --set k=v` for an array field)"),
	}
	a.register(cmd)
	b.register(cmd)
	var template bool
	cmd.Flags().BoolVar(&template, "template", false,
		"start from a row shaped like the real ones (block census, as `pay blocks new`); flags then override it")
	bindStage(rt, cmd, safety.CmdBlocksAdd, f, func(d *Deps, in *pipeInput, rf *rowField, args []string) (output.EditOp, error) {
		slug := strings.TrimSpace(args[0])
		// A plain array field (cta links, a card's buttons) holds rows with
		// NO blockType. `-` says so explicitly; a slug given for such a field
		// is dropped with a warning rather than stored, because a stray
		// blockType makes every validator read the array as a blocks field
		// and blame its correct rows.
		kind, kindSource := rowArrayKind(in.Doc, rf)
		arrayRow := slug == arrayRowArg || kind == discovery.TypeArray
		if slug == arrayRowArg {
			if kind == discovery.TypeBlocks {
				return output.EditOp{}, apierr.New(apierr.CodeInvalidArgs,
					"%s is a blocks field (%s), so a new row needs a blockType; `-` adds a row of a plain array field", orThisField(rf), kindSource).
					WithHint("pay blocks ls --field %s shows the blockTypes its rows use", orThisField(rf))
			}
		} else if err := checkBlockType(rf, slug); err != nil {
			return output.EditOp{}, err
		} else if !arrayRow {
			if w := observedPlacementWarning(rf, in.Target, slug); w != nil {
				in.Warnings = append(in.Warnings, *w)
			}
		}
		if arrayRow && template {
			return output.EditOp{}, apierr.New(apierr.CodeInvalidArgs,
				"--template builds a BLOCK row from the census, and %s is a plain array field whose rows have no blockType", orThisField(rf)).
				WithHint("copy an existing row with `pay blocks cp <selector> --field %s`, or build one with `pay blocks add - --set k=v --field %s`",
					orThisField(rf), orThisField(rf))
		}
		if slug != arrayRowArg && kind == discovery.TypeArray {
			in.Warnings = append(in.Warnings, output.Warning{
				Code: WarnBlockTypeOmitted,
				Message: fmt.Sprintf("%s is a plain array field (%s), not a blocks field: its rows carry no blockType, "+
					"so %q was not stored on the new row", orThisField(rf), kindSource, slug),
				Paths: []string{rf.Path},
				Hint:  "pass `-` instead of a blockType to add a row of an array field: pay blocks add - --set k=v --field " + orThisField(rf),
			})
		}
		anchor, err := a.build(cmd, false)
		if err != nil {
			return output.EditOp{}, err
		}
		row, mdWarns, err := b.build(d)
		if err != nil {
			return output.EditOp{}, err
		}
		in.Warnings = append(in.Warnings, mdWarns...)
		if row, err = expandRowPaths(row); err != nil {
			return output.EditOp{}, err
		}
		if template {
			base, warns, err := templateRow(cmd.Context(), d, in.Target, rf.Path, slug)
			if err != nil {
				return output.EditOp{}, err
			}
			in.Warnings = append(in.Warnings, warns...)
			deepMerge(base, row)
			// A converted editor state replaces the template's empty one
			// outright; deepMerge would keep keys only the template has.
			for _, key := range b.mdKeys() {
				if v := rowValueAt(row, key); v != nil {
					if err := rows.SetPath(base, key, v); err != nil {
						return output.EditOp{}, apierr.New(apierr.CodeInvalidArgs, "--set-md %s: %s", key, err.Error())
					}
				}
			}
			row = base
		}
		if len(b.setMD) > 0 {
			stats := cachedTypeStats(d, in.Target, rf.Shard, slug)
			for _, key := range b.mdKeys() {
				if w := mdKindWarning(stats, slug, key, key); w != nil {
					in.Warnings = append(in.Warnings, *w)
				}
			}
		}
		// blockType is set last so no --data or --set-json can shadow the
		// positional argument the caller and PayCLI both validated. A row of
		// a plain array field carries none, whatever --data held.
		what := slug
		switch {
		case kind == discovery.TypeArray:
			delete(row, rows.KeyBlockType)
			what = "array"
		case slug == arrayRowArg:
			what = "array"
		default:
			row[rows.KeyBlockType] = slug
		}
		rows.StripIDs(row)
		if what == "array" && len(row) == 0 {
			// `{}` is what an orphan row looks like, and Payload stores it
			// as a row of nulls: an array row needs at least one field.
			return output.EditOp{}, apierr.New(apierr.CodeInvalidArgs,
				"the new row of the array field %s would be empty ({}): an array row has no blockType, so it needs at least one field", orThisField(rf)).
				WithHint("set its fields: pay blocks add - --set k=v --field %s", orThisField(rf))
		}

		out, at, err := rows.Insert(rf.Rows, row, anchor)
		if err != nil {
			return output.EditOp{}, resolveErr(err, rf)
		}
		rf.Rows = out
		return output.EditOp{Detail: fmt.Sprintf("added one %s row at index %d%s", what, at, because(anchor))}, nil
	})

	SetHelp(cmd, &Help{
		Synopsis: []string{
			"… | pay blocks add <blockType> [--set k=v ...] [--set-json k=JSON ...] [--set-md k=@file.md ...] [--name NAME]",
			"                              [--at N | --before SEL | --after SEL | --first | --last]",
		},
		Args: []ArgSpec{{Name: "blockType", Required: true, Type: "enum",
			ValuesFrom: "discovery.blocks", Example: "cta"}},
		FlagInfo: mergeFlagInfo(a.helpFlags(), map[string]FlagInfo{
			"set":      {Grammar: "KEY=VALUE", Repeatable: true},
			"set-json": {Grammar: "KEY=JSON", Repeatable: true},
			"set-md":   {Grammar: setMDRowGrammar, Repeatable: true, Note: setMDRowNote},
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
			"`-` as the blockType adds a row of a plain ARRAY field (links, buttons), which carries no blockType; on a field known to be a blocks field it is invalid_args.",
			"Every ROW id in the new row is stripped (its own and those of the rows nested in it): row ids are server-generated, and reusing one rewrites an existing row. Relationship/upload ids and everything inside rich text are kept.",
		}, selectorNotes()...),
		Examples: []Example{
			{Why: "append an empty block of a type this field accepts",
				Cmd: "pay get pages 12 --depth 0 | pay blocks add mediaBlock | pay apply --yes"},
			{Why: "with a field set, placed under the hero",
				Cmd: "pay get pages 12 --depth 0 | pay blocks add cta --set-json richText=\"$(cat rt.json)\" --after 0 | pay apply --yes"},
			{Why: "a text block whose rich text comes from a Markdown file",
				Cmd: "pay get pages 12 --depth 0 --draft | pay blocks add text --set heading=Ablauf --set-md content=@ablauf.md --after 0 | pay apply --draft --yes"},
			{Why: "a whole row prepared in a file",
				Cmd: "pay get pages 12 --depth 0 | pay blocks add cta --data @cta.json --first | pay apply --yes"},
			{Why: "a new step inside the group block, after its first one",
				Cmd: "pay get pages 12 --depth 0 | pay blocks add processStep --set phase=Plan --after 0 --field 'layout[type:group].blocks' | pay apply --yes"},
			{Why: "a row of a plain array field (no blockType): a link in the cta block's links",
				Cmd: "pay get pages 12 --depth 0 | pay blocks add - --set link.url=/kontakt --set link.label=Kontakt --field 'layout[type:cta].links' | pay apply --yes"},
			{Why: "what may go in this field, and what is inside one",
				Cmd: "pay describe pages --field layout --path .block_types && pay describe pages --block cta"},
		},
		Mistakes: []Mistake{
			{Wrong: "Passing the GraphQL interface name (`CallToActionBlock`).",
				Right: "blockType is the slug from the block's config.ts (`cta`). `pay describe <collection> --field <path> --path .block_types` lists the slugs."},
			{Wrong: "`--data @-` to read the row from stdin.",
				Right: "stdin already carries the document being edited. Use `--data @file`, or `--set-json` with a shell substitution."},
			{Wrong: "Copying a row out of another document, ids and all.",
				Right: "add strips every ROW id at every depth (a kept row id makes Payload rewrite the row that already has it instead of adding one). Upload/relationship ids and Lexical node ids are references and are kept."},
			{Wrong: "`pay blocks add link --field 'layout[…].links'` on a plain array field.",
				Right: "Array rows have no blockType: `pay blocks add - --set link.url=/x --field 'layout[…].links'`. A slug given for a known array field is dropped (warning block_type_omitted)."},
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

The copy is stripped of every ROW ` + "`id`" + `: its own and those of the rows nested in it,
at every depth. That is not optional: Payload matches a row by its id, so a
duplicate that kept its ids does not add a block — it overwrites the original and
the array loses a row. References are kept: an upload or relationship value keeps
its id, and nothing inside rich text (Lexical node ids, a block node's upload) is
touched, exactly as the admin UI's "duplicate" does.`,
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
			"Every ROW id is stripped from the copy, at every depth; upload/relationship ids and rich-text node ids are kept.",
		}, selectorNotes()...),
		Examples: []Example{
			{Why: "a second CTA just like the first, right after it",
				Cmd: "pay get pages 12 --depth 0 | pay blocks cp type:cta[0] --after type:cta[0] | pay apply --yes"},
			{Why: "duplicate and then edit the copy in the same pipe",
				Cmd: "pay get pages 12 --depth 0 | pay blocks cp first --last | pay blocks set last --set blockName='Repeat CTA' | pay apply --yes"},
			{Why: "duplicate a card inside a nested grid",
				Cmd: "pay get pages 12 --depth 0 | pay blocks cp first --after first --field 'layout[type:group].blocks[type:grid].blocks' | pay apply --yes"},
		},
		Mistakes: []Mistake{
			{Wrong: "Expecting the copy to keep the original's id so it can be addressed by it.",
				Right: "It has no id until Payload assigns one. Address it by position in the same pipe (`last`, `--at`), then re-read to get its id."},
			{Wrong: "Duplicating with jq instead, keeping the ids.",
				Right: "Payload matches a row by id, so the \"copy\" overwrites its original and the array loses an entry. `cp` strips every row id at every depth (never the id of an upload, a relationship or a Lexical node)."},
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
and ` + "`--set links.0.label=Buy`" + `, never ` + "`--set layout.0.blockName=Hero`" + `.

` + "`--unset KEY`" + ` clears a field; it does not delete the key. Payload matches a row by
its id and KEEPS the stored value of any key the row omits, so a deleted key
would be written back unchanged with a 2xx. --unset therefore writes null, []
for an array, and clears every sub-field of a group (null there is a 500).`,
		Args: exactArgs(1, "pay blocks set <selector> --set k=v"),
	}
	b.register(cmd)
	cmd.Flags().StringArrayVar(&unset, "unset", nil, "clear this field of the row: null, [] for an array, every sub-field for a group (repeatable)")
	bindStage(rt, cmd, safety.CmdBlocksSet, f, func(d *Deps, in *pipeInput, rf *rowField, args []string) (output.EditOp, error) {
		sel, err := parseSelector(args[0], "the selector")
		if err != nil {
			return output.EditOp{}, err
		}
		patch, mdWarns, err := b.build(d)
		if err != nil {
			return output.EditOp{}, err
		}
		in.Warnings = append(in.Warnings, mdWarns...)
		if len(patch) == 0 && len(unset) == 0 {
			return output.EditOp{}, apierr.New(apierr.CodeInvalidArgs,
				"`pay blocks set` was given nothing to change").
				WithHint("pass --set k=v, --set-json k=JSON, --set-md k=@file.md or --unset KEY")
		}
		if _, ok := patch[rows.KeyBlockType]; ok {
			if err := checkBlockType(rf, fmt.Sprint(patch[rows.KeyBlockType])); err != nil {
				return output.EditOp{}, err
			}
			if w := observedPlacementWarning(rf, in.Target, fmt.Sprint(patch[rows.KeyBlockType])); w != nil {
				in.Warnings = append(in.Warnings, *w)
			}
		}
		for _, key := range unset {
			if k := strings.TrimSpace(key); k == rows.KeyBlockType || k == rows.KeyID {
				return output.EditOp{}, apierr.New(apierr.CodeInvalidArgs,
					"`--unset %s` would clear the key Payload identifies the row by", k).
					WithHint("remove the row with `pay blocks rm`, or change its type with `pay blocks set <selector> --set blockType=<slug>`")
			}
		}
		idx, err := rows.ResolveOne(rf.Rows, sel, rf.Path)
		if err != nil {
			return output.EditOp{}, resolveErr(err, rf)
		}
		target := rows.Summarize(rf.Rows)[idx]
		if obj, ok := rf.Rows[idx].(map[string]any); ok && len(b.setMD) > 0 {
			stats := cachedTypeStats(d, in.Target, rf.Shard, target.BlockType)
			for _, key := range b.mdKeys() {
				where := joinLoc(rf.Loc.With(rows.Hop{Row: true, Index: idx}), key)
				w := mdTargetWarning(where, key, rowValueAt(obj, key))
				if w == nil {
					w = mdKindWarning(stats, target.BlockType, key, where)
				}
				if w != nil {
					in.Warnings = append(in.Warnings, *w)
				}
			}
		}
		out, err := rows.Set(rf.Rows, idx, patch, unset)
		if err != nil {
			return output.EditOp{}, setPathErr(err, rf, idx)
		}
		rf.Rows = out
		return output.EditOp{
			Detail:  fmt.Sprintf("set %s on %s", strings.Join(changedKeys(patch, unset), ", "), describeRow(target)),
			Matched: []int{idx},
		}, nil
	})

	SetHelp(cmd, &Help{
		Synopsis: []string{"… | pay blocks set <selector> [--set k=v ...] [--set-json k=JSON ...] [--set-md k=@file.md ...] [--unset KEY ...]"},
		Args:     []ArgSpec{selectorArg("selector", true)},
		FlagInfo: map[string]FlagInfo{
			"set":      {Grammar: "KEY=VALUE", Repeatable: true, Note: "KEY is a path inside the ROW (links.0.label, links[id:…].label — an array is entered by a row, never replaced); values are typed (true, 12, null stay scalars)"},
			"field":    fieldFlagInfo(),
			"set-json": {Grammar: "KEY=JSON", Repeatable: true, Note: "the escape hatch for an object or array"},
			"set-md":   {Grammar: setMDRowGrammar, Repeatable: true, Note: setMDRowNote},
			"unset":    {Grammar: "KEY", Repeatable: true, Note: "the key is CLEARED, not deleted: Payload keeps the stored value of a key a row omits, so it writes null ([] for an array, each sub-field cleared for a group); a key the row does not have is not an error"},
			"data":     {Grammar: "JSON | @FILE", Note: "@- is not accepted here: stdin already holds the document"},
		},
		Output: OutputSpec{
			Kind:     output.KindDoc,
			Skeleton: `the whole document, with the row's fields changed`,
		},
		ExitCodes: blocksExitCodes,
		Notes: append([]string{
			"A lexical rich-text field is JSON, never a string: write it from Markdown with --set-md, or pass the editorState object through --set-json.",
		}, selectorNotes()...),
		Examples: []Example{
			{Why: "relabel a block in the admin UI",
				Cmd: "pay get pages 12 --depth 0 | pay blocks set id:67f3a1 --set blockName='Hero CTA' | pay apply --yes"},
			{Why: "replace a lexical rich-text value from Markdown",
				Cmd: "pay get pages 12 --depth 0 | pay blocks set type:cta[0] --set-md richText=@cta.md | pay apply --yes"},
			{Why: "rich text of a row nested in a group (field and selector from `pay outline`)",
				Cmd: "pay get pages 12 --depth 0 --draft | pay blocks set id:6ab8… --set-md content=@step.md --field 'layout[type:group].blocks' | pay apply --draft --yes"},
			{Why: "point a media block at another upload",
				Cmd: "pay get pages 12 --depth 0 | pay blocks set type:mediaBlock --set media=7 | pay apply --yes"},
			{Why: "clear an optional field (writes null: Payload keeps a key the row omits)",
				Cmd: "pay get pages 12 --depth 0 | pay blocks set last --unset blockName | pay apply --yes"},
			{Why: "a field of a row nested inside a group",
				Cmd: "pay get pages 12 --depth 0 | pay blocks set type:processStep[2] --set phase=Convert --field 'layout[type:group].blocks' | pay apply --yes"},
			{Why: "one entry of an array inside the row (entered by row, never replaced)",
				Cmd: "pay get pages 12 --depth 0 | pay blocks set type:cta --set 'buttons[0].label=Buy now' | pay apply --yes"},
			{Why: "what fields does this block have?",
				Cmd: "pay describe pages --block cta --path '.block.fields[].path'"},
		},
		Mistakes: []Mistake{
			{Wrong: "`--set layout.0.blockName=Hero`.",
				Right: "The selector already chose the row, and --field chooses the array it is in (at any depth). The key is a path inside the row: `--set blockName=Hero`."},
			{Wrong: "`--set richText='<p>hi</p>'` on a lexical field.",
				Right: "Lexical stores JSON, and a string is stored as nonsense the editor cannot open. Write Markdown with --set-md richText=@file.md (or pass an editorState with --set-json)."},
			{Wrong: "`jq 'del(.layout[0].blockName)' | pay apply` to clear a field.",
				Right: "Payload keeps the stored value of a key a row omits (the row is matched by its id), so the write answers 2xx and changes nothing. Send null: `pay blocks set … --unset blockName`, or `--set-json blockName=null`."},
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
		out = append(out, k+" (cleared)")
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
	setMD   []string
	name    string
}

func (b *blockData) register(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVar(&b.data, "data", "", "the row as a JSON object, or @FILE (never @-: stdin holds the document)")
	fl.StringArrayVar(&b.set, "set", nil, "k=v inside the row (repeatable, typed)")
	fl.StringArrayVar(&b.setJSON, "set-json", nil, "k=JSON inside the row (repeatable, no coercion)")
	fl.StringArrayVar(&b.setMD, "set-md", nil, "k=@file.md or k=markdown inside the row: a richText field from Markdown (repeatable; applied last; never @-: stdin holds the document)")
	fl.StringVar(&b.name, "name", "", "set blockName, the admin-UI label")
}

// build merges the flags into one row patch, later flags winning:
// --data → --set → --set-json → --set-md (§9.12.8), then --name.
func (b *blockData) build(d *Deps) (map[string]any, []output.Warning, error) {
	row, err := b.buildScalars()
	if err != nil {
		return nil, nil, err
	}
	values, warns, err := convertSetMD(d, b.setMD, mdSource{
		flag: "--set-md", stdinTakenBy: "stdin already carries the document being edited",
	})
	if err != nil {
		return nil, nil, err
	}
	for _, mv := range values {
		row[mv.Key] = mv.State
	}
	if b.name != "" {
		row[rows.KeyBlockName] = b.name
	}
	return row, warns, nil
}

// mdKeys are the --set-md keys, for the checks a verb runs against the row.
func (b *blockData) mdKeys() []string {
	out := make([]string, 0, len(b.setMD))
	for _, pair := range b.setMD {
		if key, _, ok := strings.Cut(pair, "="); ok && strings.TrimSpace(key) != "" {
			out = append(out, strings.TrimSpace(key))
		}
	}
	return out
}

// buildScalars is --data, --set and --set-json.
func (b *blockData) buildScalars() (map[string]any, error) {
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
	return row, nil
}

// expandRowPaths turns the path keys of a built row (`link.url`,
// `buttons[0].label`) into the nested values they name. `pay blocks set` hands
// its patch to rows.Set, which walks each key; a NEW row has no Set step, and
// without this `add --set link.url=/x` stored a literal "link.url" key that
// Payload silently drops.
func expandRowPaths(row map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(row))
	var paths []string
	for k, v := range row {
		if strings.ContainsAny(k, ".[") {
			paths = append(paths, k)
			continue
		}
		out[k] = v
	}
	sortStrings(paths)
	for _, k := range paths {
		if err := rows.SetPath(out, k, row[k]); err != nil {
			return nil, apierr.New(apierr.CodeInvalidArgs, "--set %s: %s", k, err.Error()).
				WithHint("a key is a path inside the new row: %s", rows.PathGrammar)
		}
	}
	return out, nil
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
