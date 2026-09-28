package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/orphans"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

// newGetCmd builds `pay get <collection> <id>` (§9.2).
func init() { Register(newGetCmd) }

// getFlags is readFlags plus the two addressing flags only `pay get` has.
type getFlags struct {
	readFlags
	slug string
	// requirePublished is --published-only. It is not readFlags.publishedOnly
	// on purpose: that one compiles to a `where` filter, and a GET by id has
	// nothing to filter.
	requirePublished bool
}

func newGetCmd(rt *Runtime) *cobra.Command {
	f := &getFlags{}
	cmd := &cobra.Command{
		GroupID: GroupRead,
		Use:     "get <collection> <id> | get <collection> --slug SLUG",
		Short:   "Fetch one document by id or slug (GET /api/{collection}/{id})",
		Long: `Fetch a single document, addressed by id or by --slug.

` + draftTrapGetHelp + `

--slug resolves the slug to an id first (one extra request, limit 2): no match is
doc_not_found (exit 4), more than one match is slug_ambiguous (exit 5). With --draft
the slug is matched against each document's newest draft, so a slug that was
changed in an unpublished draft is found too.

The id is checked against this collection's id_type before the call, because
Payload answers a non-castable id with an opaque HTTP 500. When the id type was
never discovered the request is sent unchecked — a client-side rejection built
on a fact PayCLI never learned would be confidently wrong.`,
		Args: cobra.RangeArgs(1, 2),
	}
	cmd.RunE = runData(rt, safety.CmdGet, func(ctx context.Context, cmd *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		id := ""
		if len(args) == 2 {
			id = args[1]
		}
		return runGet(ctx, cmd, d, f, args[0], id)
	})
	f.registerSelectionFlags(cmd, 0)
	cmd.Flags().BoolVar(&f.trash, "trash", false, "look in the trash as well")
	cmd.Flags().StringVar(&f.slug, "slug", "", "address the document by its slug instead of its id")
	cmd.Flags().BoolVar(&f.requirePublished, "published-only", false,
		"return the document only in its published state; not_published (exit 4) when it has none")

	SetHelp(cmd, getHelp())
	return cmd
}

// draftTrapGetHelp is the draft trap stated for a command whose filter is the
// id itself. `pay get` cannot FILTER — it fetches exactly the document you
// named — so its --published-only is an assertion ("this document, but only
// in its published state") that fails with not_published instead of
// filtering. The verbatim §9.6.2 wording lives on find, count and globals get.
const draftTrapGetHelp = `A read without --draft does NOT filter out unpublished documents, and ` + "`get`" + ` cannot
filter at all: the id IS the whole query. A bare get therefore returns a never-published
document with _status:"draft" and reports success. READ _status ON THE DOCUMENT before you
treat it as live content, or pass --published-only: the document is then returned only in
its published state, and a document that has none (never published, or unpublished) is
not_published (exit 4). --draft swaps in the newest draft for a document that does have a
published version; it cannot be combined with --published-only.`

// getHelp is §10.5's model for `pay get`.
func getHelp() *Help {
	return &Help{
		Synopsis: []string{
			"pay get <collection> <id> [--depth N] [--select a,b] [--populate 'coll:f1,f2']",
			"                          [--joins 'field:limit=5'] [--draft | --published-only] [--trash] [--locale CODE]",
			"pay get <collection> --slug SLUG [same flags]",
		},
		Collections: true,
		Args: []ArgSpec{
			{Name: "collection", Required: true, Type: "enum",
				ValuesFrom: "discovery.collections", Example: "pages"},
			{Name: "id", Required: false, Type: "string",
				Example: "16"},
		},
		FlagInfo: map[string]FlagInfo{
			"depth":          {Min: intPtr(0), Max: intPtr(10)},
			"select":         {Grammar: "field,field.sub", Repeatable: true},
			"populate":       {Grammar: "collection:field1,field2", Repeatable: true},
			"joins":          {Grammar: "field:limit=5,sort=-createdAt", Repeatable: true},
			"draft":          {Note: "swaps in the newest draft; it does NOT exclude drafts"},
			"slug":           {Note: "replaces <id>; 0 matches = doc_not_found (4), >1 = slug_ambiguous (5); honours --draft/--trash/--locale"},
			"published-only": {Note: "an assertion, not a filter: not_published (4) when the document has no published state"},
			"richtext":       richTextFlagInfo,
		},
		Output: OutputSpec{
			Kind:     output.KindDoc,
			Skeleton: `{"id":16,"title":"Home","slug":"home","_status":"draft","updatedAt":"…"}`,
		},
		ExitCodes: ReadExitCodes,
		Examples: []Example{
			{Why: "one document, every field", Cmd: "pay get pages 16"},
			{Why: "the same document by its slug (one extra lookup request)",
				Cmd: "pay get pages --slug about"},
			{Why: "embed relationships one level and keep the response small",
				Cmd: "pay get pages 16 --depth 1 --select id,title,hero"},
			{Why: "the newest draft of a document that also has a published version",
				Cmd: "pay get posts 1 --draft"},
			{Why: "what visitors see right now — fails with not_published if nothing is live",
				Cmd: "pay get pages --slug about --published-only"},
			{Why: "an upload document: the stored filename, URL and generated sizes",
				Cmd: "pay get media 4 --select filename,url,sizes"},
			{Why: "what the page says: every rich-text value (blocks included) as Markdown",
				Cmd: "pay get pages 47 --draft --richtext md"},
			{Why: "check whether what you got back is actually live",
				Cmd: "pay get pages 16 --path '._status'"},
		},
		Mistakes: []Mistake{
			{Wrong: "Expecting --published-only to filter like it does on `pay find`.",
				Right: "get names ONE document, so there is nothing to filter: the flag returns it in its published state or fails with not_published (exit 4)."},
			{Wrong: "Reading a returned document as live content because ok was true.",
				Right: "ok only means the request succeeded. A never-published document comes back with _status \"draft\" unless you pass --published-only."},
			{Wrong: "Looking up a slug with `pay find … --where 'slug eq x'` and piping the id back into get.",
				Right: "pay get <collection> --slug x does the lookup, and refuses an ambiguous slug (slug_ambiguous, exit 5) instead of silently taking the first match."},
			{Wrong: "Using --select on a join field and getting nothing.",
				Right: "Join fields are read-only and are requested with --joins 'field:limit=5'; `pay describe <collection>` lists them under join_fields."},
			{Wrong: "Editing a document read with --richtext md and piping it into `pay apply` or `--data @-`.",
				Right: "Its rich text is Markdown strings; the write paths refuse it (richtext_rendered, exit 5). Read with the default --richtext json to edit; write Markdown with --set-md."},
			{Wrong: "Retrying a 4 (doc_not_found) hoping it is a race.",
				Right: "It is not retriable. Check the slug and the id with `pay find <collection> --limit 5 --select id,slug`, add --draft if the slug only exists in a draft, or --trash if it was soft-deleted."},
		},
		SeeAlso: []string{
			"pay find <collection> --where 'id eq <id>'   # the filtering form",
			"pay url <collection> <id>   # where the document lives on the site",
			"pay describe <collection>   # what the fields mean",
			"pay download <collection> <id>   # the bytes of an upload document",
			"pay versions list <collection> --id <id>   # this document's history",
		},
	}
}

func runGet(ctx context.Context, cmd *cobra.Command, d *Deps, f *getFlags, slug, id string) (*output.Envelope, error) {
	cfg := d.cfg()
	if !cmd.Flags().Changed("depth") && cfg.Depth > 0 {
		f.depth = cfg.Depth
	}
	if err := checkGetAddressing(id, f); err != nil {
		return nil, err
	}
	if err := f.richtext.validate(); err != nil {
		return nil, err
	}

	client, err := d.requireClient()
	if err != nil {
		return nil, err
	}
	t, err := d.collection(ctx, slug)
	if err != nil {
		return nil, err
	}
	if f.requirePublished {
		if err := checkDrafts(t.Slug, t.flags().Drafts); err != nil {
			return nil, err
		}
	}
	built, err := f.build(cmd, d, t)
	if err != nil {
		return nil, err
	}
	if f.slug != "" {
		ref, err := d.ResolveDocBySlug(ctx, t, f.slug, SlugLookup{
			Draft: f.draft, Trash: f.trash, Locale: built.Params.Locale,
		})
		if err != nil {
			return nil, err
		}
		id = ref.IDString
	} else if e := d.checkID(t, t.Slug, id); e != nil {
		return nil, e
	}
	if f.requirePublished && len(built.Params.Select) > 0 && !containsString(built.Params.Select, "_status") {
		// The assertion reads _status; a --select without it would make every
		// document look unpublished.
		built.Params.Select = append(built.Params.Select, "_status")
	}

	run := d.beginRun()
	doc, resp, err := client.Get(ctx, t.Slug, id, built.Params,
		payload.WithClassify(classifyFor(t, cfg, nil)), payload.WithKnownRoute())
	if err != nil {
		return nil, err
	}
	if f.requirePublished {
		if err := assertPublished(t.Slug, id, doc); err != nil {
			return nil, err
		}
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
	// §10.2.1: a {} row is an orphan the Postgres adapter left behind.
	if paths := orphans.EmptyRows(map[string]any(doc)); len(paths) > 0 {
		env.AddWarning(emptyRowsWarning(orphanTarget{t: t, id: id}, paths, f.draft))
	}
	f.richtext.apply(env)
	return env, nil
}

// checkGetAddressing enforces "exactly one of <id> and --slug", and that
// --published-only and --draft are not both asked for.
func checkGetAddressing(id string, f *getFlags) error {
	switch {
	case id == "" && f.slug == "":
		return apierr.New(apierr.CodeInvalidArgs, "`pay get` needs a document: pass <id> or --slug SLUG").
			WithHint("pay get <collection> <id>, or pay get <collection> --slug <slug>")
	case id != "" && f.slug != "":
		return apierr.New(apierr.CodeInvalidArgs, "pass <id> or --slug, not both").
			WithHint("drop one of them: the id and the slug each address the document on their own")
	case f.requirePublished && f.draft:
		return apierr.New(apierr.CodeInvalidArgs,
			"--published-only reads the published state and --draft swaps in the newest draft; pick one").
			WithHint("drop --draft to read what visitors see, or drop --published-only to read the working copy")
	}
	return nil
}

// assertPublished is `pay get --published-only`. A read without draft returns
// the document's stored state, which Payload only overwrites on a published
// save: saving a draft writes a version and leaves the main row alone
// (payload/dist/collections/operations/utilities/update.js, `if
// (!isSavingDraft) db.updateOne(...)`, Payload 3.87). So a stored _status of
// "published" IS the live document, and anything else has no live state.
func assertPublished(slug, id string, doc payload.Doc) error {
	status, _ := doc["_status"].(string)
	if status == "published" {
		return nil
	}
	shown := status
	if shown == "" {
		shown = "absent"
	}
	return apierr.New(apierr.CodeNotPublished,
		"%s %s has no published state (_status is %s): it was never published, or it was unpublished", slug, id, shown).
		WithHint("read it as it is: pay get %s %s — or its newest draft: pay get %s %s --draft", slug, id, slug, id)
}
