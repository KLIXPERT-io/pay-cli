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
	failFast     bool
	// resetNested is §10.2.1's --reset-nested: publishing writes the newest
	// version over the stored row, which is exactly the write that leaves
	// nested rows behind after a draft restructure.
	resetNested bool
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
	name, short := "publish", "Set _status=published on one or more documents, or every match"
	long := `Publish documents.

Publishing re-runs EXACTLY the required-field validation that --draft skipped,
so a draft is a deferral, not an escape. PayCLI checks the document against the
collection's required paths BEFORE the call and names every missing one, instead
of relaying an opaque server 400.

When a collection's required fields include a blocks field whose blockType slugs
are not determinable from the API, the collection is publishable: false and this
command refuses locally — no amount of retrying will make those slugs knowable.

Several ids (` + "`pay publish pages 38 39 40`" + `) are written one document at a time:
each id is its own PATCH, a failure never poisons the others, and the result is a
bulk_result — exit 7 (partial_failure) when some failed, with next.cmd naming only
the failed ids. The blast radius is exactly the ids you typed, so this is not an L3
bulk write and needs no --max-docs.

  --limit is page size and has no meaning here. Use --max-docs N, or --all.`
	if !publish {
		name, short = "unpublish", "Set _status=draft on one or more documents, or every match"
		long = `Unpublish documents: set _status back to draft.

The document keeps its content and its published version history; it simply
stops being served as published content. This is an L2 operation and prompts on
a TTY, because nothing else in the system announces that a page went offline.

Several ids (` + "`pay unpublish pages 38 39 40 --yes`" + `) are written one document at a
time; a partial failure is exit 7 with next.cmd naming only the failed ids.

  --limit is page size and has no meaning here. Use --max-docs N, or --all.`
	}

	command := safety.CmdPublish
	if !publish {
		command = safety.CmdUnpublish
	}
	cmd := &cobra.Command{
		GroupID: GroupWrite,
		Use:     name + " <collection> [id...]",
		Short:   short,
		Long:    long,
		Args:    cobra.MinimumNArgs(1),
	}
	cmd.RunE = runData(rt, command, func(ctx context.Context, cmd *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		if f.resetNested && len(args) != 2 {
			return nil, apierr.New(apierr.CodeInvalidArgs,
				"--reset-nested works on one document: it reads it, plans the carrier phases for it and verifies it").
				WithHint("run it per document: pay %s %s <id> --reset-nested --dry-run", command, args[0])
		}
		if len(args) > 2 {
			return runPublishIDs(ctx, cmd, d, f, publish, args[0], args[1:])
		}
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
	fl.BoolVar(&f.failFast, "fail-fast", false, "with several ids: stop at the first failure (the rest are reported as not_attempted)")
	if publish {
		fl.BoolVar(&f.skipValidate, "no-pre-validate", false, "skip the client-side required-field check and let the server answer")
	}
	fl.BoolVar(&f.resetNested, "reset-nested", false,
		"one id only: remove nested block rows the write would leave behind in the stored row (orphans), keeping the id")

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
			"where":     {Grammar: "PATH OP VALUE", Operators: WhereOperatorAliases(), Repeatable: true},
			"or":        {Grammar: "PATH OP VALUE", Operators: WhereOperatorAliases(), Repeatable: true},
			"depth":     {Min: intPtr(0), Max: intPtr(10)},
			"max-docs":  {Min: intPtr(1), Note: "blast-radius cap; default 100, --all lifts it"},
			"limit":     {Note: "NOT valid here — it is page size; use --max-docs"},
			"fail-fast": {Note: "only with several ids; the rest land in data.not_attempted"},
			"reset-nested": {Note: "one id only; removes nested block rows the write leaves behind in the stored row " +
				"(orphans) with carrier phases; see meta.reset_nested"},
		},
		ExitCodes: BulkExitCodes,
	}
	if publish {
		h.Synopsis = []string{
			"pay publish <collection> <id> [--no-pre-validate] [--depth N] [--select a,b] [--reset-nested]",
			"pay publish <collection> <id> <id>... [--fail-fast]     # one PATCH per id, bulk_result",
			"pay publish <collection> --where 'PATH OP VALUE' [--max-docs N|--all] --yes",
		}
		h.Output = OutputSpec{Kind: output.KindDoc,
			Skeleton: `{"id":16,"_status":"published","publishedAt":"…"}  · bulk: data_kind bulk_result`}
		h.Examples = []Example{
			{Why: "will it pass validation? the check runs before anything is sent",
				Cmd: "pay publish pages 16 --dry-run"},
			{Why: "publish one document",
				Cmd: "pay publish pages 16"},
			{Why: "publish exactly these documents, one write each (exit 7 if some fail)",
				Cmd: "pay publish pages 38 39 40"},
			{Why: "the draft workflow end to end",
				Cmd: "pay create pages --set title='Pricing' --set slug=pricing --draft"},
			{Why: "preview a bulk publish",
				Cmd: "pay publish posts --where '_status eq draft' --max-docs 20 --dry-run"},
			{Why: "run it; --yes is required in a non-TTY (else exit 11)",
				Cmd: "pay publish posts --where '_status eq draft' --max-docs 20 --yes"},
			{Why: "skip PayCLI's own required-field check and let the server answer",
				Cmd: "pay publish pages 16 --no-pre-validate"},
			{Why: "the draft restructured nested blocks (orphan_risk in the dry run): publish without leaving {} rows",
				Cmd: "pay publish pages 16 --reset-nested"},
		}
		h.Mistakes = []Mistake{
			{Wrong: "Treating --draft as a way to avoid validation forever.",
				Right: "Publishing re-runs EXACTLY the validation --draft skipped. Read error.fields, set those paths with `pay update`, then publish."},
			{Wrong: "Retrying a publish that failed with publishable:false.",
				Right: "The collection has a required blocks field whose blockType slugs are not discoverable over the API; no retry makes them knowable. Write the blocks explicitly, or publish from the admin UI."},
			{Wrong: "`--limit 20` to cap a bulk publish.",
				Right: "--limit is page size and is an error here. Use --max-docs 20, or --all."},
			{Wrong: "Checking only `pay get --draft` after publishing a nested block restructure.",
				Right: "The draft (the new version) is clean; the published row can hold {} orphan rows. publish reads it back and warns orphan_rows; --reset-nested prevents them."},
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
		"pay unpublish <collection> <id> <id>... --yes [--fail-fast]     # one PATCH per id, bulk_result",
		"pay unpublish <collection> --where 'PATH OP VALUE' [--max-docs N|--all] --yes",
	}
	h.Output = OutputSpec{Kind: output.KindDoc,
		Skeleton: `{"id":16,"_status":"draft"}  · bulk: data_kind bulk_result`}
	h.Examples = []Example{
		{Why: "see exactly what would go offline",
			Cmd: "pay unpublish pages 16 --dry-run"},
		{Why: "take one document offline; it is L2, so --yes is required in a non-TTY",
			Cmd: "pay unpublish pages 16 --yes"},
		{Why: "take exactly these documents offline, one write each",
			Cmd: "pay unpublish pages 38 39 40 --yes"},
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
		canon, e := d.canonicalID(t, t.Slug, id)
		if e != nil {
			return nil, e
		}
		id = canon
		if publish && !f.skipValidate {
			if err := preValidatePublish(ctx, client, t, id, classify); err != nil {
				return nil, err
			}
		}
		op := safety.Op{Command: command, Selector: safety.SelectorID}
		newOp := func() *writeOp {
			return d.newWriteOp(op, t.Slug, "PATCH", "/"+t.Slug+"/"+id).withIDs([]any{id}).
				withBackup(ctx, BackupTarget{Collection: t, Params: p})
		}
		w := newOp()
		o := orphanTarget{t: t, id: id}
		fixCmd := fmt.Sprintf("pay %s %s %s --reset-nested --dry-run   # plans the id-preserving remedy", command, t.Slug, shellQuote(id))
		spec := resetSpec{o: o, command: command, body: body, params: p,
			op: func(step string) *writeOp {
				nw := newOp()
				nw.step = step
				return nw
			},
			send: func(ctx context.Context, b map[string]any, p query.Params) (*payload.WriteResult, error) {
				return client.Update(ctx, t.Slug, id, b, p,
					payload.WithClassify(classifyFor(t, cfg, payload.SentPaths(b))), payload.WithKnownRoute())
			},
		}
		var warnings []output.Warning
		if cfg.DryRun {
			// F7 stage B: what visitors will see change — the stored
			// (published) row against the newest version with the new status.
			diff, pre, dw := d.dryRunDiff(ctx, client, o, body, p, diffBasePublished, fixCmd)
			warnings = append(warnings, dw...)
			if diff != nil {
				w.dryDiff = diff
			}
			sent := body
			if f.resetNested {
				plan, pw, err := d.planReset(ctx, client, spec, pre)
				if err != nil {
					return nil, err
				}
				warnings = append(warnings, pw...)
				w.dryReset = plan.report(true)
			}
			q, _ := p.Encode()
			return emitDryRun(d, w, command, "PATCH",
				client.URLFor(&payload.Request{Method: "PATCH", Path: "/" + t.Slug + "/" + id, Query: q}),
				sent, 1, []any{id}, warnings...)
		}
		if err := w.confirm(1); err != nil {
			return nil, err
		}
		var plan *resetPlan
		if f.resetNested {
			var pw []output.Warning
			var err error
			plan, pw, err = d.planReset(ctx, client, spec, nil)
			if err != nil {
				return nil, err
			}
			warnings = append(warnings, pw...)
		}
		var (
			res *payload.WriteResult
			rep *resetReport
		)
		if plan != nil && !plan.skip {
			var rw []output.Warning
			var err error
			res, rep, rw, err = d.execReset(ctx, spec, plan, w)
			warnings = append(warnings, rw...)
			if err != nil {
				return nil, err
			}
		} else {
			if err := w.pre(1); err != nil {
				return nil, err
			}
			warnings = append(warnings, d.riskFromBackup(o, body, p, fixCmd)...)
			var err error
			res, err = client.Update(ctx, t.Slug, id, body, p, classify, payload.WithKnownRoute())
			if err != nil {
				w.post(false, statusOf(res), 0, 1, err.Error(), "")
				return nil, err
			}
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
		for _, warn := range warnings {
			env.AddWarning(warn)
		}
		// Publishing writes the newest version over the stored row: the one
		// write that leaves nested rows behind after a draft restructure,
		// while its echo (the new version) shows none of them.
		if rep != nil {
			for _, warn := range d.finishReset(ctx, client, spec, body, res, rep) {
				env.AddWarning(warn)
			}
			env.Meta.ResetNested = rep
		} else {
			if plan != nil {
				env.Meta.ResetNested = plan.report(false)
			}
			warns, _ := d.orphanCheckAfterWrite(ctx, client, o, body, res.Doc, p, false, false)
			for _, warn := range warns {
				env.AddWarning(warn)
			}
		}
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
	w := d.newWriteOp(op, t.Slug, "PATCH", "/"+t.Slug).withWhere(where).
		withBackup(ctx, BackupTarget{Collection: t, Params: p})

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

	// The bulk PATCH of a PUBLISH carries ?draft=true, as the admin panel's
	// "Publish" bulk action does (@payloadcms/ui PublishMany): without it
	// payload@3.87.1's bulk update (collections/operations/update.js) reads
	// the STORED rows (db.find), so a pending draft was not published but
	// superseded — verified on localhost:3900 (pages 79: the draft title was
	// neither published nor kept as the newest version). With it the
	// documents come from the newest versions (queryDrafts) and, because the
	// body says _status "published", isSavingDraft is false and the main row
	// is written. An unpublish keeps the plain PATCH: with ?draft=true it
	// would only save a draft version.
	wp := p
	if publish {
		wp.Draft = query.BoolPtr(true)
	}

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
		q, _ := bulkScope(where, wp).Encode()
		diff, dw := d.sampleDiffs(ctx, client, t, ids, body, wp, diffBasePublished, true)
		if diff != nil {
			w.dryDiff = diff
		}
		env, err := emitDryRun(d, w.withIDs(ids), command, "PATCH",
			client.URLFor(&payload.Request{Method: "PATCH", Path: "/" + t.Slug, Query: q}), body, len(ids), ids, dw...)
		if err != nil {
			return nil, err
		}
		if gap != nil {
			env.AddWarning(*gap)
		}
		return env, nil
	}
	if len(ids) == 0 && len(blocked) > 0 {
		return nil, allBlockedError(t.Slug, blocked)
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
		res, callErr = client.UpdateByIDs(ctx, t.Slug, ids, body, wp, classify, payload.WithKnownRoute())
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
	sel := requiredSelect(t.Shard.RequiredPaths)
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

	index := newDocsByID()
	for _, doc := range docs {
		index.add(doc)
	}
	keep := make([]any, 0, len(ids))
	var blocked []payload.BulkFailure
	for _, id := range ids {
		doc, _, found := index.lookup(id)
		if !found {
			keep = append(keep, id)
			continue
		}
		if m := stillMissing(t.Shard, doc); len(m) > 0 {
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

// requiredSelect is the projection the publish pre-check reads: `id` plus the
// TOP-LEVEL field of every required path, never the paths themselves.
//
// Selecting a parent next to its own sub-paths (`hero`, `hero.type`,
// `hero.links.link.label`) makes Payload return the parent narrowed to the
// scalar sub-paths — `hero: {type}` with `links` dropped — so stillMissing
// saw no `links` array at all and reported `hero.links.link.label` missing on
// a page whose `hero.links` is `[]` (verified on localhost:3900). Reading the
// whole top-level field is what the single-id check (a full get) sees.
func requiredSelect(paths []string) []string {
	out := []string{"id"}
	seen := map[string]bool{"id": true}
	for _, p := range paths {
		top, _, _ := strings.Cut(p, ".")
		if top == "" || seen[top] {
			continue
		}
		seen[top] = true
		out = append(out, top)
	}
	return out
}

// allBlockedError answers a publish in which the pre-validation blocked EVERY
// document: nothing was sent, so it is a validation failure (exit 5) with the
// per-document reasons — not a partial failure whose hint warns that
// "successful writes are already committed".
func allBlockedError(slug string, blocked []payload.BulkFailure) *apierr.Error {
	failures := make([]apierr.Failure, 0, len(blocked))
	for _, b := range blocked {
		failures = append(failures, apierr.Failure{ID: b.ID, Code: apierr.CodeValidationFailed, Message: b.Message})
	}
	return apierr.New(apierr.CodeValidationFailed,
		"none of the %d %s document(s) can be published: each is missing required field(s); nothing was sent",
		len(blocked), slug).
		WithFailures(failures...).
		WithHint("set the missing fields first (pay describe %s --required-only lists them), "+
			"or pass --no-pre-validate to let the server decide", slug)
}

func failureIDs(in []payload.BulkFailure) []any {
	out := make([]any, 0, len(in))
	for _, f := range in {
		out = append(out, f.ID)
	}
	return out
}

// runPublishIDs is `pay publish|unpublish <collection> <id> <id>...`: explicit
// ids, one PATCH per document (§12.5's --per-doc semantics, so one bad
// document never poisons its batch-mates), reported as a bulk_result with exit
// 7 on a partial failure. Each document is a scoped write, so the risk level is
// the single-id one — publish L1, unpublish L2 — confirmed once for the set.
func runPublishIDs(ctx context.Context, cmd *cobra.Command, d *Deps, f *publishFlags, publish bool,
	slug string, rawIDs []string) (*output.Envelope, error) {
	command, status, verb := safety.CmdPublish, "published", "Published"
	if !publish {
		command, status, verb = safety.CmdUnpublish, "draft", "Unpublished"
	}
	if publishHasFilter(&f.filter) {
		return nil, apierr.New(apierr.CodeInvalidArgs,
			"`pay %s` takes explicit ids OR --where, not both", command).
			WithHint("drop the ids to act on every match, or drop --where to act on exactly the ids you named")
	}
	if cmd.Flags().Changed("max-docs") || f.all {
		return nil, apierr.New(apierr.CodeInvalidArgs,
			"--max-docs and --all cap a --where match; explicit ids are already the exact blast radius").
			WithHint("drop --max-docs/--all")
	}
	if err := safety.CheckLimitFlag(command, safety.SelectorBulk, cmd.Flags().Changed("limit")); err != nil {
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

	// Validate every id before anything is sent, and drop exact duplicates: a
	// typo'd id list must fail whole, not after half of it was written.
	ids := make([]any, 0, len(rawIDs))
	seen := map[string]bool{}
	for _, raw := range rawIDs {
		raw, e := d.canonicalID(t, t.Slug, raw)
		if e != nil {
			return nil, e
		}
		if seen[raw] {
			continue
		}
		seen[raw] = true
		ids = append(ids, raw)
	}

	body := map[string]any{"_status": status}
	p := query.Params{Depth: query.IntPtr(f.depth), Select: f.selectF}
	if warn, err := applyWriteLocale(d, t, f.locale, &p); err != nil {
		return nil, err
	} else if warn != nil {
		d.RT.Warn(*warn)
	}
	if err := query.ValidateSelect(f.selectF, t.schema(cfg)); err != nil {
		return nil, err
	}
	classify := payload.WithClassify(classifyFor(t, cfg, payload.SentPaths(body)))

	// §9.10.4 for every id at once: one find, not N gets.
	var blocked []payload.BulkFailure
	if publish && !f.skipValidate {
		ids, blocked, err = splitPublishable(ctx, client, t, ids, classify)
		if err != nil {
			return nil, err
		}
	}

	op := safety.Op{Command: command, Selector: safety.SelectorIDs}
	// One batched pre-image read for every id that will actually be sent
	// (§12.8); ids the pre-validation blocked are never written, so they are
	// not backed up either.
	w := d.newWriteOp(op, t.Slug, "PATCH", "/"+t.Slug+"/{id}").withIDs(ids).
		withBackup(ctx, BackupTarget{Collection: t, Params: p})
	if cfg.DryRun {
		q, _ := p.Encode()
		diff, dw := d.sampleDiffs(ctx, client, t, ids, body, p, diffBasePublished, false)
		if diff != nil {
			w.dryDiff = diff
			// would_backup counts only documents the preview read found: an
			// id that does not exist is never backed up by the real run.
			if missing, _ := diff["missing"].([]string); len(missing) > 0 && w.bk != nil {
				w.bk.IDs = withoutIDs(ids, missing)
			}
		}
		env, err := emitDryRun(d, w, command, "PATCH",
			client.URLFor(&payload.Request{Method: "PATCH", Path: "/" + t.Slug + "/{id}", Query: q}),
			body, len(ids), ids, dw...)
		if err != nil {
			return nil, err
		}
		for _, b := range blocked {
			env.AddWarning(output.Warning{
				Code:    "publish_blocked",
				Message: fmt.Sprintf("%s %v would not be sent: %s", t.Slug, b.ID, b.Message),
				Hint:    fmt.Sprintf("pay describe %s --required-only", t.Slug),
			})
		}
		return env, nil
	}
	if len(ids) == 0 && len(blocked) > 0 {
		return nil, allBlockedError(t.Slug, blocked)
	}
	if err := w.confirm(len(ids)); err != nil {
		return nil, err
	}
	if err := w.pre(len(ids)); err != nil {
		return nil, err
	}

	res := &payload.BulkResult{}
	var notAttempted []any
	var callErr error
	if len(ids) > 0 {
		res, notAttempted, callErr = perDocUpdate(ctx, client, t, ids, body, p, f.failFast, classify, payload.WithKnownRoute())
		if res == nil {
			res = &payload.BulkResult{}
		}
	}
	res.Errors = append(res.Errors, blocked...)

	// next.cmd retries what a retry can fix: failures other than a missing
	// document, plus the ids never attempted. An id that does not exist fails
	// the same way on every retry, so it is named in the hint instead.
	var retryIDs, missing []any
	for _, e := range res.Errors {
		if e.Code == apierr.CodeDocNotFound {
			missing = append(missing, e.ID)
			continue
		}
		retryIDs = append(retryIDs, e.ID)
	}
	retryIDs = append(retryIDs, notAttempted...)
	retry := ""
	if len(retryIDs) > 0 {
		retry = fmt.Sprintf("pay %s %s %s%s%s", command, t.Slug, joinIDsSpaced(retryIDs), yesFlag(!publish), d.profileFlag())
	}
	attempted := append(append([]any{}, ids...), failureIDs(blocked)...)
	env := bulkEnvelope(command, verb, t, res, attempted, notAttempted, retry).
		WithMeta(withLocaleMeta(w.run.meta(), p))
	if env.Error != nil {
		// Explicit ids are one PATCH each, not a bulk operation: say what is
		// committed and what next.cmd covers, per document.
		hint := fmt.Sprintf("%d of the listed document(s) were written and stay written; each id is its own request.", len(res.Docs))
		if len(res.Docs) == 0 {
			hint = "None of the listed documents was written; each id is its own request."
		}
		if retry != "" {
			hint += " next.cmd retries only the ids a retry can fix (fix a validation failure first)."
		}
		if len(missing) > 0 {
			hint += fmt.Sprintf(" %s %s do(es) not exist and is left out of next.cmd; `pay find %s --limit 5` lists real ids.",
				t.Slug, joinIDs(missing), t.Slug)
		}
		env.Error.Hint = hint
	}
	if env.Next != nil {
		env.Next.Args = map[string]any{"ids": retryIDs}
	}
	errMsg := ""
	if callErr != nil {
		errMsg = callErr.Error()
	}
	return w.finish(env, res.HTTP, len(res.Docs), len(res.Errors)+len(notAttempted), errMsg)
}

// withoutIDs is ids minus the ones named in drop (compared as strings). The
// result is never nil, so an explicit empty list stays explicit.
func withoutIDs(ids []any, drop []string) []any {
	skip := make(map[string]bool, len(drop))
	for _, s := range drop {
		skip[s] = true
	}
	out := make([]any, 0, len(ids))
	for _, id := range ids {
		if !skip[fmt.Sprint(id)] {
			out = append(out, id)
		}
	}
	return out
}

func joinIDsSpaced(ids []any) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, shellQuote(fmt.Sprintf("%v", id)))
	}
	return strings.Join(parts, " ")
}

// yesFlag re-emits --yes on a retry of an L2 verb, so next.cmd stays literally
// runnable in the non-TTY shell that produced it.
func yesFlag(needed bool) string {
	if needed {
		return " --yes"
	}
	return ""
}

// publishHasFilter reports whether any --where-family flag was given.
func publishHasFilter(f *readFlags) bool {
	return len(f.where) > 0 || len(f.or) > 0 || f.whereJSON != "" || f.whereRaw != "" ||
		f.q != "" || len(f.ids) > 0 || f.idsCSV != "" || f.draftOnly || f.publishedOnly ||
		f.since != "" || f.until != ""
}
