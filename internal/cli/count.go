package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

// newCountCmd builds `pay count <collection>` (§9.2).
//
// Payload's /count route accepts only `where` and `trash`; it ignores `draft`
// entirely (§9.6.3), so this command deliberately offers no --draft flag and
// says why in its help rather than sending a parameter that does nothing.
func init() { Register(newCountCmd) }

func newCountCmd(rt *Runtime) *cobra.Command {
	f := &readFlags{}
	cmd := &cobra.Command{
		GroupID: GroupRead,
		Use:     "count <collection>",
		Short:   "Count the documents matching a filter (GET /api/{collection}/count)",
		Long: `Count documents without transferring them.

Payload's count route IGNORES the draft parameter, so there is no --draft flag
here. Use --published-only or --draft-only, which compile to a _status filter
the count route does honour.`,
		Args: cobra.ExactArgs(1),
	}
	cmd.RunE = runData(rt, safety.CmdCount, func(ctx context.Context, cmd *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		return runCount(ctx, cmd, d, f, args[0])
	})
	f.registerFilterFlags(cmd)

	SetHelp(cmd, countHelp())
	return cmd
}

// countHelp is §10.5's model for `pay count`.
func countHelp() *Help {
	return &Help{
		Synopsis: []string{
			"pay count <collection> [--where 'PATH OP VALUE' ...] [--or 'PATH OP VALUE' ...]",
			"                       [--published-only|--draft-only] [--since 30d] [--trash]",
		},
		Collections: true,
		Where:       true,
		Args: []ArgSpec{
			{Name: "collection", Required: true, Type: "enum",
				ValuesFrom: "discovery.collections", Example: "pages"},
		},
		FlagInfo: map[string]FlagInfo{
			"where": {Grammar: "PATH OP VALUE", Operators: WhereOperatorAliases(), Repeatable: true},
			"or":    {Grammar: "PATH OP VALUE", Operators: WhereOperatorAliases(), Repeatable: true},
			"since": {Grammar: "30d | 2026-01-01 | 2026-01-01T00:00:00Z"},
			"until": {Grammar: "30d | 2026-01-01 | 2026-01-01T00:00:00Z"},
			"id":    {Repeatable: true},
		},
		Output: OutputSpec{
			Kind:     output.KindCount,
			Skeleton: `104   (a bare integer, not an object)`,
		},
		ExitCodes: ReadExitCodes,
		Examples: []Example{
			{Why: "how big is this collection?", Cmd: "pay count pages"},
			{Why: "how much of it is actually live?", Cmd: "pay count posts --published-only"},
			{Why: "size a filter before you run it as a bulk write",
				Cmd: "pay count crm-contacts --where 'lifecycleStage eq lead'"},
			{Why: "a date window, resolved client-side to an absolute bound",
				Cmd: "pay count crm-contacts --where 'createdAt gte 2026-01-01'"},
			{Why: "the same substring search `pay find --q` runs",
				Cmd: "pay count crm-contacts --q Ada"},
			{Why: "a substring filter on one field",
				Cmd: "pay count crm-contacts --where 'email contains @navy.test'"},
		},
		Mistakes: []Mistake{
			{Wrong: "Passing --draft to exclude drafts from a count.",
				Right: "The count route ignores draft entirely, so the flag does not exist here. Use --published-only or --draft-only, which compile to a _status filter."},
			{Wrong: "Assuming count and find with the same --where disagree.",
				Right: "They send the identical filter. If the numbers differ, --limit is capping find; use `pay find --count-only` or --all."},
			{Wrong: "Counting to decide whether a bulk write is safe, then running it unfiltered.",
				Right: "Run the write itself with --dry-run: it reports would_affect and sample_ids for the filter you are actually about to send."},
			{Wrong: "Reading .data as an object.",
				Right: "data_kind is count and .data is a bare integer — no .totalDocs, no .count. (--output raw prints Payload's own body, which IS {\"totalDocs\":N}.)"},
		},
		SeeAlso: []string{
			"pay find <collection> --count-only   # same number, from the find command",
			"pay describe <collection> --queryable   # what can appear in --where",
			"pay explain --section gotchas",
		},
	}
}

// countHandoffLimit is the page size the count -> find hand-off suggests.
const countHandoffLimit = 20

func runCount(ctx context.Context, cmd *cobra.Command, d *Deps, f *readFlags, slug string) (*output.Envelope, error) {
	cfg := d.cfg()
	client, err := d.requireClient()
	if err != nil {
		return nil, err
	}
	t, err := d.collection(ctx, slug)
	if err != nil {
		return nil, err
	}
	built, err := f.build(cmd, d, t)
	if err != nil {
		return nil, err
	}

	run := d.beginRun()
	n, resp, err := client.Count(ctx, t.Slug, built.Params,
		payload.WithClassify(classifyFor(t, cfg, nil)), payload.WithKnownRoute())
	if err != nil {
		return nil, err
	}
	var raw []byte
	if resp != nil {
		raw = resp.Body
	}
	env := output.New(safety.CmdCount, output.KindCount, n).
		WithTarget(t.envTarget(nil)).
		WithMeta(built.applyMeta(run.meta())).
		WithRawBody(raw, !cfg.Redact)
	for _, w := range built.Warnings {
		env.AddWarning(w)
	}
	if n > 0 {
		// The hand-off to `pay find` has to carry the SAME filter. Re-emitting
		// `pay find <slug> --limit 20` alone turns "69 leads" into a first page
		// of all 104 contacts, with nothing in either envelope to say so.
		nextCmd, args := f.reinvocation(d, t, built, reinvokeOpts{Limit: countHandoffLimit})
		env.WithNext(&output.Next{
			Reason: output.ReasonMorePages,
			Cmd:    nextCmd,
			Args:   args,
		})
	}
	return env, nil
}
