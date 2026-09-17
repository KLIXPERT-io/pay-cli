package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

// newGetCmd builds `pay get <collection> <id>` (§9.2).
func init() { Register(newGetCmd) }

func newGetCmd(rt *Runtime) *cobra.Command {
	f := &readFlags{}
	cmd := &cobra.Command{
		GroupID: GroupRead,
		Use:     "get <collection> <id>",
		Short:   "Fetch one document by id (GET /api/{collection}/{id})",
		Long: `Fetch a single document.

` + draftTrapGetHelp + `

The id is checked against this collection's id_type before the call, because
Payload answers a non-castable id with an opaque HTTP 500. When the id type was
never discovered the request is sent unchecked — a client-side rejection built
on a fact PayCLI never learned would be confidently wrong.`,
		Args: cobra.ExactArgs(2),
	}
	cmd.RunE = runData(rt, safety.CmdGet, func(ctx context.Context, cmd *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		return runGet(ctx, cmd, d, f, args[0], args[1])
	})
	f.registerSelectionFlags(cmd, 0)
	cmd.Flags().BoolVar(&f.trash, "trash", false, "look in the trash as well")

	SetHelp(cmd, getHelp())
	return cmd
}

// draftTrapGetHelp is the draft trap stated for a command whose filter is the
// id itself. `pay get` has NO --published-only — it fetches exactly the document
// you named — so repeating find's "always pass --published-only" here sends an
// agent to a flag that does not exist (verified: exit 5, unknown flag). The
// verbatim §9.6.2 wording lives on find, count and globals get, which do have
// the flag.
const draftTrapGetHelp = `A read without --draft does NOT filter out unpublished documents, and ` + "`get`" + ` cannot
filter at all: the id IS the whole query. A bare get therefore returns a never-published
document with _status:"draft" and reports success. READ _status ON THE DOCUMENT before you
treat it as live content. --draft swaps in the newest draft for a document that does have a
published version. To ask "this id, but only if it is published", use the command that can
filter: pay find <collection> --published-only --where 'id eq <id>' --limit 1.`

// getHelp is §10.5's model for `pay get`.
func getHelp() *Help {
	return &Help{
		Synopsis: []string{
			"pay get <collection> <id> [--depth N] [--select a,b] [--populate 'coll:f1,f2']",
			"                          [--joins 'field:limit=5'] [--draft] [--trash] [--locale CODE]",
		},
		Collections: true,
		Args: []ArgSpec{
			{Name: "collection", Required: true, Type: "enum",
				ValuesFrom: "discovery.collections", Example: "pages"},
			{Name: "id", Required: true, Type: "string",
				Example: "16"},
		},
		FlagInfo: map[string]FlagInfo{
			"depth":    {Min: intPtr(0), Max: intPtr(10)},
			"select":   {Grammar: "field,field.sub", Repeatable: true},
			"populate": {Grammar: "collection:field1,field2", Repeatable: true},
			"joins":    {Grammar: "field:limit=5,sort=-createdAt", Repeatable: true},
			"draft":    {Note: "swaps in the newest draft; it does NOT exclude drafts"},
		},
		Output: OutputSpec{
			Kind:     output.KindDoc,
			Skeleton: `{"id":16,"title":"Home","slug":"home","_status":"draft","updatedAt":"…"}`,
		},
		ExitCodes: ReadExitCodes,
		Examples: []Example{
			{Why: "one document, every field", Cmd: "pay get pages 16"},
			{Why: "embed relationships one level and keep the response small",
				Cmd: "pay get pages 16 --depth 1 --select id,title,hero"},
			{Why: "the newest draft of a document that also has a published version",
				Cmd: "pay get posts 1 --draft"},
			{Why: "an upload document: the stored filename, URL and generated sizes",
				Cmd: "pay get media 4 --select filename,url,sizes"},
			{Why: "check whether what you got back is actually live",
				Cmd: "pay get pages 16 --path '._status'"},
			{Why: "\"this id, but only if it is published\" — get cannot filter, find can",
				Cmd: "pay find pages --published-only --where 'id eq 16' --limit 1"},
		},
		Mistakes: []Mistake{
			{Wrong: "Passing --published-only to `pay get`.",
				Right: "It does not exist here and fails with invalid_args (exit 5): the id is the whole filter. Read _status on the document, or use `pay find --published-only --where 'id eq <id>'`."},
			{Wrong: "Reading a returned document as live content because ok was true.",
				Right: "ok only means the request succeeded. A never-published document comes back with _status \"draft\"."},
			{Wrong: "Using --select on a join field and getting nothing.",
				Right: "Join fields are read-only and are requested with --joins 'field:limit=5'; `pay describe <collection>` lists them under join_fields."},
			{Wrong: "Retrying a 4 (doc_not_found) hoping it is a race.",
				Right: "It is not retriable. Check the slug and the id with `pay find <collection> --limit 5 --select id`, or add --trash if it was soft-deleted."},
		},
		SeeAlso: []string{
			"pay find <collection> --where 'id eq <id>'   # the filtering form",
			"pay describe <collection>   # what the fields mean",
			"pay download <collection> <id>   # the bytes of an upload document",
			"pay versions list <collection> --id <id>   # this document's history",
		},
	}
}

func runGet(ctx context.Context, cmd *cobra.Command, d *Deps, f *readFlags, slug, id string) (*output.Envelope, error) {
	cfg := d.cfg()
	if !cmd.Flags().Changed("depth") && cfg.Depth > 0 {
		f.depth = cfg.Depth
	}

	client, err := d.requireClient()
	if err != nil {
		return nil, err
	}
	t, err := d.collection(ctx, slug)
	if err != nil {
		return nil, err
	}
	if e := d.checkID(t, t.Slug, id); e != nil {
		return nil, e
	}
	built, err := f.build(cmd, d, t)
	if err != nil {
		return nil, err
	}

	run := d.beginRun()
	doc, resp, err := client.Get(ctx, t.Slug, id, built.Params,
		payload.WithClassify(classifyFor(t, cfg, nil)), payload.WithKnownRoute())
	if err != nil {
		return nil, err
	}

	var raw []byte
	if resp != nil {
		raw = resp.Body
	}
	env := output.New(safety.CmdGet, output.KindDoc, doc).
		WithTarget(t.envTarget(doc.ID())).
		WithMeta(built.applyMeta(run.meta())).
		WithRawBody(raw, !cfg.Redact)
	for _, w := range built.Warnings {
		env.AddWarning(w)
	}
	if w := mixedStatusWarning(t.Slug, []payload.Doc{doc}, t.flags().Drafts, f.draft); w != nil {
		env.AddWarning(*w)
	}
	return env, nil
}
