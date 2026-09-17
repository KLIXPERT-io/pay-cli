package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

type publishFlags struct {
	filter readFlags

	depth   int
	selectF []string
	locale  string

	maxDocs      int
	all          bool
	limit        int
	skipValidate bool
}

func init() {
	Register(newPublishCmd)
	Register(newUnpublishCmd)
}

// newPublishCmd builds `pay publish` (§9.2, §9.10.4).
func newPublishCmd(rt *Runtime) *cobra.Command {
	return newPublishLikeCmd(rt, true)
}

// newUnpublishCmd builds `pay unpublish`. Taking a page offline is an L2
// operation: it is a content-visibility change nobody else can see coming.
func newUnpublishCmd(rt *Runtime) *cobra.Command {
	return newPublishLikeCmd(rt, false)
}

func newPublishLikeCmd(rt *Runtime, publish bool) *cobra.Command {
	f := &publishFlags{}
	name, short := "publish", "Set _status=published on one document or every match"
	long := `Publish documents.

Publishing re-runs EXACTLY the required-field validation that --draft skipped,
so a draft is a deferral, not an escape. PayCLI checks the document against the
collection's required paths BEFORE the call and names every missing one, instead
of relaying an opaque server 400.

When a collection's required fields include a blocks field whose blockType slugs
are not determinable from the API, the collection is publishable: false and this
command refuses locally — no amount of retrying will make those slugs knowable.

  --limit is page size and has no meaning here. Use --max-docs N, or --all.`
	if !publish {
		name, short = "unpublish", "Set _status=draft on one document or every match"
		long = `Unpublish documents: set _status back to draft.

The document keeps its content and its published version history; it simply
stops being served as published content. This is an L2 operation and prompts on
a TTY, because nothing else in the system announces that a page went offline.

  --limit is page size and has no meaning here. Use --max-docs N, or --all.`
	}

	command := safety.CmdPublish
	if !publish {
		command = safety.CmdUnpublish
	}
	cmd := &cobra.Command{
		GroupID: GroupWrite,
		Use:     name + " <collection> [id]",
		Short:   short,
		Long:    long,
		Args:    cobra.RangeArgs(1, 2),
	}
	cmd.RunE = runData(rt, command, func(ctx context.Context, cmd *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		id := ""
		if len(args) == 2 {
			id = args[1]
		}
		return runPublish(ctx, cmd, d, f, publish, args[0], id)
	})
	f.filter.registerFilterFlags(cmd)
	fl := cmd.Flags()
	fl.IntVar(&f.depth, "depth", 0, "relationship expansion depth of the echoed document")
	fl.StringSliceVar(&f.selectF, "select", nil, "return only these fields")
	fl.StringVar(&f.locale, "locale", "", "locale to operate in")
	fl.IntVar(&f.maxDocs, "max-docs", 0, "blast-radius cap for the bulk form (default defaults.max_bulk = 100)")
	fl.BoolVar(&f.all, "all", false, "lift the --max-docs cap")
	fl.IntVar(&f.limit, "limit", 0, "not valid on a bulk write — use --max-docs")
	if publish {
		fl.BoolVar(&f.skipValidate, "no-pre-validate", false, "skip the client-side required-field check and let the server answer")
	}

	SetHelp(cmd, publishHelp(publish))
	return cmd
}

// publishHelp is §10.5's model for `pay publish` and `pay unpublish`.
func publishHelp(publish bool) *Help {
	h := &Help{
		Collections: true,
		Where:       true,
		Args: []ArgSpec{
			{Name: "collection", Required: true, Type: "enum",
				ValuesFrom: "discovery.collections", Example: "pages"},
			{Name: "id", Required: false, Type: "string", Example: "16"},
		},
		FlagInfo: map[string]FlagInfo{
			"where":    {Grammar: "PATH OP VALUE", Operators: WhereOperatorAliases(), Repeatable: true},
			"or":       {Grammar: "PATH OP VALUE", Operators: WhereOperatorAliases(), Repeatable: true},
			"depth":    {Min: intPtr(0), Max: intPtr(10)},
			"max-docs": {Min: intPtr(1), Note: "blast-radius cap; default 100, --all lifts it"},
			"limit":    {Note: "NOT valid here — it is page size; use --max-docs"},
		},
		ExitCodes: BulkExitCodes,
	}
	if publish {
		h.Synopsis = []string{
			"pay publish <collection> <id> [--no-pre-validate] [--depth N] [--select a,b]",
			"pay publish <collection> --where 'PATH OP VALUE' [--max-docs N|--all] --yes",
		}
		h.Output = OutputSpec{Kind: output.KindDoc,
			Skeleton: `{"id":16,"_status":"published","publishedAt":"…"}  · bulk: data_kind bulk_result`}
		h.Examples = []Example{
			{Why: "will it pass validation? the check runs before anything is sent",
				Cmd: "pay publish pages 16 --dry-run"},
			{Why: "publish one document",
				Cmd: "pay publish pages 16"},
			{Why: "the draft workflow end to end",
				Cmd: "pay create pages --set title='Pricing' --set slug=pricing --draft"},
			{Why: "preview a bulk publish",
				Cmd: "pay publish posts --where '_status eq draft' --max-docs 20 --dry-run"},
			{Why: "run it; --yes is required in a non-TTY (else exit 11)",
				Cmd: "pay publish posts --where '_status eq draft' --max-docs 20 --yes"},
			{Why: "skip PayCLI's own required-field check and let the server answer",
				Cmd: "pay publish pages 16 --no-pre-validate"},
		}
		h.Mistakes = []Mistake{
			{Wrong: "Treating --draft as a way to avoid validation forever.",
				Right: "Publishing re-runs EXACTLY the validation --draft skipped. Read error.fields, set those paths with `pay update`, then publish."},
			{Wrong: "Retrying a publish that failed with publishable:false.",
				Right: "The collection has a required blocks field whose blockType slugs are not discoverable over the API; no retry makes them knowable. Write the blocks explicitly, or publish from the admin UI."},
			{Wrong: "`--limit 20` to cap a bulk publish.",
				Right: "--limit is page size and is an error here. Use --max-docs 20, or --all."},
			{Wrong: "Auto-retrying exit 7 on a bulk publish.",
				Right: "Some documents are already published. Re-run only the failed ids with `next.cmd`."},
		}
		h.SeeAlso = []string{
			"pay unpublish <collection> <id>   # the inverse",
			"pay describe <collection> --required-only   # what publishing will demand",
			"pay update <collection> <id> --publish   # write and publish in one call",
			"pay find <collection> --draft-only   # what is waiting to be published",
		}
		return h
	}
	h.Synopsis = []string{
		"pay unpublish <collection> <id> --yes [--depth N] [--select a,b]",
		"pay unpublish <collection> --where 'PATH OP VALUE' [--max-docs N|--all] --yes",
	}
	h.Output = OutputSpec{Kind: output.KindDoc,
		Skeleton: `{"id":16,"_status":"draft"}  · bulk: data_kind bulk_result`}
	h.Examples = []Example{
		{Why: "see exactly what would go offline",
			Cmd: "pay unpublish pages 16 --dry-run"},
		{Why: "take one document offline; it is L2, so --yes is required in a non-TTY",
			Cmd: "pay unpublish pages 16 --yes"},
		{Why: "what is currently live, before you unpublish any of it",
			Cmd: "pay find posts --published-only --select id,title"},
		{Why: "preview a bulk unpublish",
			Cmd: "pay unpublish posts --where '_status eq published' --max-docs 20 --dry-run"},
		{Why: "run it",
			Cmd: "pay unpublish posts --where '_status eq published' --max-docs 20 --yes"},
	}
	h.Mistakes = []Mistake{
		{Wrong: "Treating exit 11 as a failure.",
			Right: "11 is \"confirmation required\": unpublishing takes content offline and nothing else announces it. Re-run with --yes after --dry-run."},
		{Wrong: "Expecting unpublish to delete the document.",
			Right: "It only sets _status back to draft. The content and the version history stay; use `pay delete` to remove it."},
		{Wrong: "`--limit 20` to cap a bulk unpublish.",
			Right: "--limit is page size and is an error here. Use --max-docs 20, or --all."},
		{Wrong: "Auto-retrying exit 7 on a bulk unpublish.",
			Right: "Some documents are already offline. Re-run only the failed ids with `next.cmd`."},
	}
	h.SeeAlso = []string{
		"pay publish <collection> <id>   # the inverse",
		"pay find <collection> --published-only   # what is live right now",
		"pay versions list <collection> --id <id>",
	}
	return h
}

func runPublish(ctx context.Context, cmd *cobra.Command, d *Deps, f *publishFlags, publish bool, slug, id string) (*output.Envelope, error) {
	// Validated before anything is resolved or sent, and on the single-id form
	// too: an explicit `--max-docs 0` used to fall through to the config
	// default, so the error then quoted a cap the caller never typed.
	if err := safety.ValidateMaxDocs(f.maxDocs, cmd.Flags().Changed("max-docs")); err != nil {
		return nil, err
	}
	cfg := d.cfg()
	client, err := d.requireClient()
	if err != nil {
		return nil, err
	}
	t, err := d.collection(ctx, slug)
	if err != nil {
		return nil, err
	}
	if err := checkDrafts(t.Slug, t.flags().Drafts); err != nil {
		return nil, err
	}
	if publish {
		if err := checkPublishable(t); err != nil {
			return nil, err
		}
	}

	command := safety.CmdPublish
	status := "published"
	if !publish {
		command, status = safety.CmdUnpublish, "draft"
	}
	bulk := id == ""
	selector := safety.SelectorID
	if bulk {
		selector = safety.SelectorBulk
	}
	if err := safety.CheckLimitFlag(command, selector, cmd.Flags().Changed("limit")); err != nil {
		return nil, err
	}

	body := map[string]any{"_status": status}
	p := query.Params{Depth: query.IntPtr(f.depth), Select: f.selectF}
	// §7.9a: publish echoes the document, so it needs fallback-locale=none
	// exactly as much as update does. See applyWriteLocale in update.go.
	if warn, err := applyWriteLocale(d, t, f.locale, &p); err != nil {
		return nil, err
	} else if warn != nil {
		d.RT.Warn(*warn)
	}
	if err := query.ValidateSelect(f.selectF, t.schema(cfg)); err != nil {
		return nil, err
	}
	classify := payload.WithClassify(classifyFor(t, cfg, payload.SentPaths(body)))

	if !bulk {
		if e := d.checkID(t, t.Slug, id); e != nil {
			return nil, e
		}
		if publish && !f.skipValidate {
			if err := preValidatePublish(ctx, client, t, id, classify); err != nil {
				return nil, err
			}
		}
		op := safety.Op{Command: command, Selector: safety.SelectorID}
		w := d.newWriteOp(op, t.Slug, "PATCH", "/"+t.Slug+"/"+id).withIDs([]any{id})
		if cfg.DryRun {
			q, _ := p.Encode()
			return emitDryRun(d, w, command, "PATCH",
				client.URLFor(&payload.Request{Method: "PATCH", Path: "/" + t.Slug + "/" + id, Query: q}),
				body, 1, []any{id})
		}
		if err := w.confirm(1); err != nil {
			return nil, err
		}
		if err := w.pre(1); err != nil {
			return nil, err
		}
		res, err := client.Update(ctx, t.Slug, id, body, p, classify, payload.WithKnownRoute())
		if err != nil {
			w.post(false, statusOf(res), 0, 1, err.Error(), "")
			return nil, err
		}
		env := output.New(command, output.KindDoc, res.Doc).
			WithTarget(t.envTarget(res.Doc.ID())).
			WithChanged(&output.Changed{Updated: 1, IDs: []any{res.Doc.ID()}}).
			WithMeta(withLocaleMeta(w.run.meta(), p)).
			WithRawBody(res.Raw, !cfg.Redact).
			WithNext(&output.Next{
				Reason: output.ReasonVerifyWrite,
				Cmd:    fmt.Sprintf("pay get %s %s --depth 0%s", t.Slug, res.Doc.IDString(), d.profileFlag()),
			})
		return w.finish(env, res.HTTP, 1, 0, "")
	}

	// --- bulk --------------------------------------------------------------
	built, err := f.filter.build(nil, d, t)
	if err != nil {
		return nil, err
	}
	where := built.Params.Where
	if where.IsEmpty() {
		return nil, apierr.New(apierr.CodeWhereRequired,
			"`pay %s <collection>` without an id needs --where", command).
			WithHint("pass --where 'field op value', or name a document: pay %s %s <id>", command, t.Slug)
	}

	op := safety.Op{Command: command, Selector: safety.SelectorBulk}
	w := d.newWriteOp(op, t.Slug, "PATCH", "/"+t.Slug).withWhere(where)

	// All three §12.3 phases share ONE scope. Counting on a bare --where while
	// the write carries --locale sizes the cap against one population and
	// publishes another.
	scope := bulkScope(where, p)

	n, _, err := client.Count(ctx, t.Slug, scope, classify, payload.WithKnownRoute())
	if err != nil {
		return nil, err
	}
	maxDocs := safety.ResolveMaxDocs(f.maxDocs, cmd.Flags().Changed("max-docs"), cfg.MaxBulk, f.all)
	plan := safety.Plan{Command: command, Collection: t.Slug, Matched: n, MaxDocs: maxDocs, All: f.all}
	if err := plan.Check(); err != nil {
		return nil, err
	}
	ids, err := client.ResolveIDsScoped(ctx, t.Slug, scope, n, classify, payload.WithKnownRoute())
	if err != nil {
		return nil, err
	}
	gap := resolveGapWarning(n, len(ids))

	// §9.10.4 extended to the bulk form: pre-validate every resolved document
	// so one incomplete draft does not poison the batch with an opaque 400.
	var blocked []payload.BulkFailure
	if publish && !f.skipValidate {
		ids, blocked, err = splitPublishable(ctx, client, t, ids, classify)
		if err != nil {
			return nil, err
		}
	}

	if cfg.DryRun {
		q, _ := scope.Encode()
		env, err := emitDryRun(d, w.withIDs(ids), command, "PATCH",
			client.URLFor(&payload.Request{Method: "PATCH", Path: "/" + t.Slug, Query: q}), body, len(ids), ids)
		if err != nil {
			return nil, err
		}
		if gap != nil {
			env.AddWarning(*gap)
		}
		return env, nil
	}
	if err := w.confirm(len(ids)); err != nil {
		return nil, err
	}
	if err := w.withIDs(ids).pre(len(ids)); err != nil {
		return nil, err
	}

	res := &payload.BulkResult{}
	var callErr error
	if len(ids) > 0 {
		res, callErr = client.UpdateByIDs(ctx, t.Slug, ids, body, p, classify, payload.WithKnownRoute())
		if res == nil {
			if callErr != nil {
				w.post(false, 0, 0, len(ids), callErr.Error(), "")
				return nil, callErr
			}
			res = &payload.BulkResult{}
		}
	}
	res.Errors = append(res.Errors, blocked...)

	verb := "Published"
	if !publish {
		verb = "Unpublished"
	}
	retry := fmt.Sprintf("pay %s %s --where 'id in %s'%s", command, t.Slug, joinIDs(failedIDsOf(res)), d.profileFlag())
	attempted := append(append([]any{}, ids...), failureIDs(blocked)...)
	env := bulkEnvelope(command, verb, t, res, attempted, nil, retry).
		WithMeta(withLocaleMeta(w.run.meta(), p)).
		WithRawBody(res.Raw, !cfg.Redact)
	if gap != nil {
		env.AddWarning(*gap)
	}
	errMsg := ""
	if callErr != nil {
		errMsg = callErr.Error()
	}
	return w.finish(env, res.HTTP, len(res.Docs), len(res.Errors), errMsg)
}

// checkPublishable is §9.10.4's local refusal.
func checkPublishable(t *collTarget) error {
	if t == nil || t.Coll == nil || t.Coll.Publishable {
		return nil
	}
	reason := "a required field's accepted values are not determinable from the API"
	if t.Coll.PublishableReason != nil && *t.Coll.PublishableReason != "" {
		reason = *t.Coll.PublishableReason
	}
	return apierr.New(apierr.CodeFeatureUnavailable,
		"%q cannot be published by PayCLI: %s", t.Slug, reason).
		WithHint("read the block slugs from payload.config.ts / src/blocks/*/config.ts (look for `slug:`), " +
			"then pin them with `pay config set profiles.<profile>.blocks.<field> a,b,c`")
}

// preValidatePublish reads the document and checks it against the collection's
// required paths BEFORE issuing the publishing PATCH.
func preValidatePublish(ctx context.Context, client *payload.Client, t *collTarget, id string, opts ...payload.Option) error {
	if t.Shard == nil || len(t.Shard.RequiredPaths) == 0 {
		return nil
	}
	doc, _, err := client.Get(ctx, t.Slug, id, query.Params{Depth: query.IntPtr(0), Draft: query.BoolPtr(true)},
		append(opts, payload.WithKnownRoute())...)
	if err != nil {
		// A read failure is the server's answer to the id, not a validation
		// verdict: let the caller see it rather than inventing one.
		return err
	}
	missing := stillMissing(t.Shard, doc)
	if len(missing) == 0 {
		return nil
	}
	fields := make([]apierr.Field, 0, len(missing))
	for _, path := range missing {
		fields = append(fields, apierr.Field{Path: path, Message: "required to publish, and currently empty"})
	}
	return apierr.New(apierr.CodeValidationFailed,
		"%s %s cannot be published: %d required field(s) are missing (%s)",
		t.Slug, id, len(missing), strings.Join(missing, ", ")).
		WithFields(fields...).
		WithHint("set them first: pay update %s %s --set <field>=<value>", t.Slug, id)
}

// splitPublishable partitions resolved ids into the publishable ones and the
// blocked ones, so an incomplete draft never poisons its batch-mates.
func splitPublishable(ctx context.Context, client *payload.Client, t *collTarget, ids []any,
	opts ...payload.Option) ([]any, []payload.BulkFailure, error) {
	if t.Shard == nil || len(t.Shard.RequiredPaths) == 0 || len(ids) == 0 {
		return ids, nil, nil //nolint:nilerr // documented above: a failed pre-check must not block the write
	}
	sel := append([]string{"id"}, t.Shard.RequiredPaths...)
	var docs []payload.Doc
	err := client.FindPages(ctx, t.Slug, query.Params{
		Where:  safety.WhereIDsIn(ids),
		Select: sel,
		Depth:  query.IntPtr(0),
		Draft:  query.BoolPtr(true),
		Limit:  query.IntPtr(100),
	}, len(ids), func(l *payload.ListResult) error {
		docs = append(docs, l.Docs...)
		return nil
	}, append(opts, payload.WithKnownRoute())...)
	if err != nil {
		// The pre-check is an optimisation, not a gate: if it cannot run, send
		// the write and let the server answer.
		return ids, nil, nil //nolint:nilerr // deliberate: a failed pre-check must not block the write
	}

	missingByID := map[string][]string{}
	for _, doc := range docs {
		if m := stillMissing(t.Shard, doc); len(m) > 0 {
			missingByID[doc.IDString()] = m
		}
	}
	if len(missingByID) == 0 {
		return ids, nil, nil
	}
	keep := make([]any, 0, len(ids))
	var blocked []payload.BulkFailure
	for _, id := range ids {
		key := fmt.Sprintf("%v", id)
		if m, bad := missingByID[key]; bad {
			blocked = append(blocked, payload.BulkFailure{
				ID:      id,
				Message: "cannot be published: missing required field(s) " + strings.Join(m, ", "),
			})
			continue
		}
		keep = append(keep, id)
	}
	return keep, blocked, nil
}

func failureIDs(in []payload.BulkFailure) []any {
	out := make([]any, 0, len(in))
	for _, f := range in {
		out = append(out, f.ID)
	}
	return out
}
