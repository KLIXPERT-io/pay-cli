package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

// defaultTrashField is Payload's default name for the soft-delete timestamp.
// §12.4 requires it to come from the manifest rather than being hardcoded, so
// it is only the fallback for a project whose shard PayCLI could not read.
const defaultTrashField = "deletedAt"

// trashField resolves the collection's soft-delete field from its shard.
func trashField(shard *discovery.Shard) string {
	if shard == nil {
		return defaultTrashField
	}
	if f, ok := shard.Field(defaultTrashField); ok && f.PayloadType == discovery.TypeDate {
		return f.Path
	}
	for _, f := range shard.Fields {
		if f.PayloadType == discovery.TypeDate && f.Path == defaultTrashField {
			return f.Path
		}
	}
	return defaultTrashField
}

// deleteDecision is §12.4's soft-delete-by-default rule, isolated so it is a
// table test rather than a branch buried in a command body.
type deleteDecision struct {
	// Soft is true when the delete is the recoverable PATCH.
	Soft bool
	// Method is the HTTP verb that will be used.
	Method string
	// SendTrashParam is true when ?trash=true must be added. A hard delete of
	// an ALREADY-trashed document 404s without it (verified), and PayCLI
	// cannot know whether this document is trashed without spending an extra
	// request; trash=true is harmless on a live document, so it is set
	// whenever the collection has trash at all.
	SendTrashParam bool
}

func deletePlan(trashEnabled, permanent bool) deleteDecision {
	switch {
	case trashEnabled && !permanent:
		return deleteDecision{Soft: true, Method: "PATCH"}
	case trashEnabled && permanent:
		return deleteDecision{Method: "DELETE", SendTrashParam: true}
	default:
		return deleteDecision{Method: "DELETE"}
	}
}

type deleteFlags struct {
	filter readFlags

	permanent    bool
	overrideLock bool
	unsafeWhere  bool
	maxDocs      int
	all          bool
	limit        int
	locale       string
}

func init() { Register(newDeleteCmd) }

func newDeleteCmd(rt *Runtime) *cobra.Command {
	f := &deleteFlags{}
	cmd := &cobra.Command{
		GroupID: GroupWrite,
		Use:     "delete <collection> [id]",
		Short:   "Delete one document by id, or every document matching --where",
		Long: `Delete documents.

On a trash-enabled collection a delete is a SOFT delete by default: PayCLI
issues PATCH {"deletedAt": "<now>"} and reports "trashed". --permanent issues
the real DELETE, which cannot be undone. On a collection without trash every
delete is irreversible and PayCLI says so on stderr before acting.

Bulk DELETE on the server IGNORES limit and removes every match (verified:
limit=1 deleted all 4). PayCLI therefore never passes --where through to
DELETE. It counts, enforces --max-docs, resolves the exact ids and deletes them
in chunks of 100, so the blast radius is exactly what --dry-run printed.
--unsafe-passthrough-where restores the raw server semantics for anyone who
wants them.

  --limit is page size and has no meaning here. Use --max-docs N, or --all.`,
		Args: cobra.RangeArgs(1, 2),
	}
	cmd.RunE = runData(rt, safety.CmdDelete, func(ctx context.Context, cmd *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		id := ""
		if len(args) == 2 {
			id = args[1]
		}
		return runDelete(ctx, cmd, d, f, args[0], id)
	})
	f.filter.registerFilterFlags(cmd)
	fl := cmd.Flags()
	fl.BoolVar(&f.permanent, "permanent", false, "hard DELETE instead of the soft-delete PATCH — irreversible")
	fl.BoolVar(&f.overrideLock, "override-lock", false, "ignore an existing document lock")
	fl.BoolVar(&f.unsafeWhere, "unsafe-passthrough-where", false, "send --where straight to DELETE; the server then ignores every cap")
	fl.IntVar(&f.maxDocs, "max-docs", 0, "blast-radius cap for the bulk form (default defaults.max_bulk = 100)")
	fl.BoolVar(&f.all, "all", false, "lift the --max-docs cap")
	fl.IntVar(&f.limit, "limit", 0, "not valid on a bulk write — use --max-docs")
	fl.StringVar(&f.locale, "locale", "", "locale to operate in")

	SetHelp(cmd, deleteHelp())
	return cmd
}

// deleteHelp is §10.5's model for `pay delete`.
func deleteHelp() *Help {
	return &Help{
		Synopsis: []string{
			"pay delete <collection> <id> [--permanent] [--yes]",
			"pay delete <collection> --where 'PATH OP VALUE' [--max-docs N|--all] --yes",
		},
		Collections: true,
		Where:       true,
		Args: []ArgSpec{
			{Name: "collection", Required: true, Type: "enum",
				ValuesFrom: "discovery.collections", Example: "crm-contacts"},
			{Name: "id", Required: false, Type: "string", Example: "224"},
		},
		FlagInfo: map[string]FlagInfo{
			"where":                    {Grammar: "PATH OP VALUE", Operators: WhereOperatorAliases(), Repeatable: true},
			"or":                       {Grammar: "PATH OP VALUE", Operators: WhereOperatorAliases(), Repeatable: true},
			"permanent":                {Note: "the real DELETE; irreversible even where trash exists"},
			"max-docs":                 {Min: intPtr(1), Note: "blast-radius cap; default 100, --all lifts it"},
			"limit":                    {Note: "NOT valid on a bulk write — it is page size; use --max-docs"},
			"unsafe-passthrough-where": {Note: "restores the server's own semantics, where limit is IGNORED and every match is deleted"},
		},
		Output: OutputSpec{
			Kind:     output.KindDoc,
			Skeleton: `{"id":224,"deletedAt":"…","trashed":true}  · bulk: data_kind bulk_result {"succeeded":[…],"failed":[…]}`,
		},
		ExitCodes: BulkExitCodes,
		Examples: []Example{
			{Why: "always first: exactly which documents, and the exact request",
				Cmd: "pay delete crm-contacts 174 --dry-run"},
			{Why: "soft delete on a trash-enabled collection (PATCH deletedAt, reversible)",
				Cmd: "pay delete crm-contacts 174 --yes"},
			{Why: "see what you soft-deleted",
				Cmd: "pay find crm-contacts --trash --where 'id eq 174' --select id,name,deletedAt"},
			{Why: "the real DELETE — irreversible; note pages has no trash, so every delete there is this",
				Cmd: "pay delete pages 27 --permanent --yes"},
			{Why: "preview a bulk delete before it exists",
				Cmd: "pay delete pages --where 'slug contains tmp-' --dry-run"},
			{Why: "run it, capped at the documents you just previewed",
				Cmd: "pay delete pages --where 'slug contains tmp-' --max-docs 20 --yes"},
		},
		Mistakes: []Mistake{
			{Wrong: "Believing `--limit 1` limits a bulk delete.",
				Right: "Payload's DELETE IGNORES limit and removes EVERY match (verified: limit=1 deleted all 4). PayCLI never passes --where through; --max-docs N is the only cap, and --limit is an error here."},
			{Wrong: "Assuming a delete is reversible because another collection has trash.",
				Right: "Trash is per-collection. `pay collections` lists `trash` under features; without it every delete is permanent, and PayCLI says so on stderr before acting."},
			{Wrong: "Auto-retrying exit 7 (partial_failure) on a bulk delete.",
				Right: "Some documents are already gone. Re-run only the failed ids from `next.cmd`."},
			{Wrong: "Expecting `pay restore` to undo a --permanent delete.",
				Right: "It cannot: that DELETE is real. Restore only clears the soft-delete timestamp. Recover from a version or a backup instead."},
			{Wrong: "Running the bulk form without --yes in a script and treating exit 11 as a failure.",
				Right: "11 means \"confirmation required\", not \"error\". Re-run with --yes once --dry-run showed you the blast radius."},
		},
		SeeAlso: []string{
			"pay restore <collection> <id>   # un-trash a soft delete",
			"pay count <collection> --where …   # size the filter first",
			"pay find <collection> --trash   # what is already soft-deleted",
			"pay audit tail --action delete   # what did I already change?",
		},
	}
}

func runDelete(ctx context.Context, cmd *cobra.Command, d *Deps, f *deleteFlags, slug, id string) (*output.Envelope, error) {
	// The blast-radius cap is validated before anything is resolved or sent:
	// an explicit `--max-docs 0` is an error on the scoped form too, where it
	// used to be dropped in silence.
	if err := safety.ValidateMaxDocs(f.maxDocs, cmd.Flags().Changed("max-docs")); err != nil {
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

	bulk := id == ""
	selector := safety.SelectorID
	if bulk {
		selector = safety.SelectorBulk
	}
	if err := safety.CheckLimitFlag(safety.CmdDelete, selector, cmd.Flags().Changed("limit")); err != nil {
		return nil, err
	}

	trash := t.flags().Trash
	trashEnabled := trash != nil && *trash
	plan := deletePlan(trashEnabled, f.permanent)
	softDelete := plan.Soft

	p := query.Params{}
	// §7.9a: DELETE echoes the document it removed, and that echo is the agent's
	// only remaining copy of it — so it must not be a fabricated translation.
	if warn, err := applyWriteLocale(d, t, f.locale, &p); err != nil {
		return nil, err
	} else if warn != nil {
		d.RT.Warn(*warn)
	}
	if plan.SendTrashParam {
		p.Trash = query.BoolPtr(true)
	}
	if f.overrideLock {
		p.Extra = urlValues("overrideLock", "true")
	}

	if bulk {
		return runBulkDelete(ctx, cmd, d, client, f, t, p, softDelete, trashEnabled)
	}
	return runDeleteOne(ctx, d, client, f, t, id, p, softDelete, trashEnabled)
}

func runDeleteOne(ctx context.Context, d *Deps, client *payload.Client, f *deleteFlags,
	t *collTarget, id string, p query.Params, softDelete, trashEnabled bool) (*output.Envelope, error) {
	cfg := d.cfg()
	if e := d.checkID(t, t.Slug, id); e != nil {
		return nil, e
	}
	op := safety.Op{
		Command:      safety.CmdDelete,
		Selector:     safety.SelectorID,
		Permanent:    f.permanent,
		TrashEnabled: trashEnabled,
	}
	method, path := "DELETE", "/"+t.Slug+"/"+id
	var body map[string]any
	if softDelete {
		method = "PATCH"
		body = map[string]any{trashField(t.Shard): d.now().UTC().Format(time.RFC3339)}
	}
	w := d.newWriteOp(op, t.Slug, method, path).withIDs([]any{id})

	if cfg.DryRun {
		q, _ := p.Encode()
		return emitDryRun(d, w, safety.CmdDelete, method,
			client.URLFor(&payload.Request{Method: method, Path: path, Query: q}), body, 1, []any{id})
	}
	if err := w.confirm(1); err != nil {
		return nil, err
	}
	if err := w.pre(1); err != nil {
		return nil, err
	}

	classify := payload.WithClassify(classifyFor(t, cfg, payload.SentPaths(body)))
	var (
		res *payload.WriteResult
		err error
	)
	if softDelete {
		res, err = client.Update(ctx, t.Slug, id, body, p, classify, payload.WithKnownRoute())
	} else {
		res, err = client.Delete(ctx, t.Slug, id, p, classify, payload.WithKnownRoute())
	}
	if err != nil {
		w.post(false, statusOf(res), 0, 1, err.Error(), "")
		return nil, err
	}

	changed := &output.Changed{IDs: []any{res.Doc.ID()}}
	next := &output.Next{
		Reason: output.ReasonVerifyWrite,
		Cmd:    fmt.Sprintf("pay get %s %s --trash%s", t.Slug, res.Doc.IDString(), d.profileFlag()),
	}
	if softDelete {
		changed.Trashed = 1
		next.Alternatives = []output.Alternative{{
			Why: "undo the soft delete",
			Cmd: fmt.Sprintf("pay restore %s %s%s", t.Slug, res.Doc.IDString(), d.profileFlag()),
		}}
	} else {
		changed.Deleted = 1
		next = &output.Next{
			Reason: output.ReasonVerifyWrite,
			Cmd:    fmt.Sprintf("pay count %s --where 'id eq %s'%s", t.Slug, res.Doc.IDString(), d.profileFlag()),
		}
	}
	if changed.IDs[0] == nil {
		changed.IDs = []any{id}
	}

	env := output.New(safety.CmdDelete, output.KindDoc, res.Doc).
		WithTarget(t.envTarget(res.Doc.ID())).
		WithChanged(changed).
		WithMeta(withLocaleMeta(w.run.meta(), p)).
		WithRawBody(res.Raw, !cfg.Redact).
		WithNext(next)
	return w.finish(env, res.HTTP, 1, 0, "")
}

func runBulkDelete(ctx context.Context, cmd *cobra.Command, d *Deps, client *payload.Client,
	f *deleteFlags, t *collTarget, p query.Params, softDelete, trashEnabled bool) (*output.Envelope, error) {
	cfg := d.cfg()
	built, err := f.filter.build(nil, d, t)
	if err != nil {
		return nil, err
	}
	where := built.Params.Where
	if where.IsEmpty() {
		return nil, apierr.New(apierr.CodeWhereRequired,
			"`pay delete <collection>` without an id needs --where").
			WithHint("pass --where 'field op value', or name a document: pay delete %s <id>", t.Slug)
	}

	// --unsafe-passthrough-where hands the filter to the server, which answers
	// it with a real DELETE and no concept of trash. The op and the method
	// therefore have to describe a hard delete on that path whatever the
	// collection's trash setting says: the op drives the L3 confirmation line,
	// the §12.4 IRREVERSIBLE stderr warning and the audit `action`, and the
	// method argument below is what the audit record's `method` is taken from.
	// Deriving them from `softDelete` used to promise a reversible PATCH while
	// purging the collection.
	if f.unsafeWhere {
		softDelete = false
	}
	op := safety.Op{
		Command:      safety.CmdDelete,
		Selector:     safety.SelectorBulk,
		Permanent:    f.permanent,
		HardDelete:   f.unsafeWhere,
		TrashEnabled: trashEnabled,
	}
	method := "DELETE"
	var body map[string]any
	if softDelete {
		method = "PATCH"
		body = map[string]any{trashField(t.Shard): d.now().UTC().Format(time.RFC3339)}
	}
	w := d.newWriteOp(op, t.Slug, method, "/"+t.Slug).withWhere(where)
	classify := payload.WithClassify(classifyFor(t, cfg, payload.SentPaths(body)))

	// All three §12.3 phases share ONE scope. `--permanent` sends trash=true,
	// so counting live-only (or resolving live-only) would compute the cap on
	// one population and delete another: the already-trashed documents a purge
	// is aimed at were counted and then never resolved.
	scope := bulkScope(where, p)

	// §12.3 phase 1: count. The passthrough path counts too — skipping this is
	// what reported its blast radius as 0 documents.
	n, _, err := client.Count(ctx, t.Slug, scope, classify, payload.WithKnownRoute())
	if err != nil {
		return nil, err
	}

	if f.unsafeWhere {
		return runUnsafeDelete(ctx, d, client, w, t, where, p, scope, n, classify)
	}

	// phase 2: the blast-radius cap.
	maxDocs := safety.ResolveMaxDocs(f.maxDocs, cmd.Flags().Changed("max-docs"), cfg.MaxBulk, f.all)
	plan := safety.Plan{Command: safety.CmdDelete, Collection: t.Slug, Matched: n, MaxDocs: maxDocs, All: f.all}
	if err := plan.Check(); err != nil {
		return nil, err
	}
	// phase 3: resolve the exact ids, in the same scope that was counted.
	ids, err := client.ResolveIDsScoped(ctx, t.Slug, scope, n, classify, payload.WithKnownRoute())
	if err != nil {
		return nil, err
	}
	gap := resolveGapWarning(n, len(ids))

	if cfg.DryRun {
		q, _ := scope.Encode()
		env, err := emitDryRun(d, w.withIDs(ids), safety.CmdDelete, method,
			client.URLFor(&payload.Request{Method: method, Path: "/" + t.Slug, Query: q}), body, len(ids), ids)
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
	if len(ids) == 0 {
		env := output.New(safety.CmdDelete, output.KindBulkResult,
			bulkData{Succeeded: []payload.Doc{}, Failed: []map[string]any{}, NotAttempted: []any{}}).
			WithTarget(t.envTarget(nil)).
			WithChanged(&output.Changed{IDs: []any{}}).
			WithMeta(withLocaleMeta(w.run.meta(), p))
		if gap != nil {
			env.AddWarning(*gap)
		}
		return w.finish(env, nil, 0, 0, "")
	}

	var (
		res     *payload.BulkResult
		callErr error
	)
	if softDelete {
		res, callErr = client.UpdateByIDs(ctx, t.Slug, ids, body, p, classify, payload.WithKnownRoute())
	} else {
		res, callErr = client.DeleteByIDs(ctx, t.Slug, ids, p, classify, payload.WithKnownRoute())
	}
	if res == nil {
		if callErr != nil {
			w.post(false, 0, 0, len(ids), callErr.Error(), "")
			return nil, callErr
		}
		res = &payload.BulkResult{}
	}

	verb := "Deleted"
	if softDelete {
		verb = "Trashed"
	}
	retry := fmt.Sprintf("pay delete %s --where 'id in %s'%s", t.Slug, joinIDs(failedIDsOf(res)), d.profileFlag())
	if f.permanent {
		retry += " --permanent"
	}
	env := bulkEnvelope(safety.CmdDelete, verb, t, res, ids, nil, retry).
		WithMeta(withLocaleMeta(w.run.meta(), p)).
		WithRawBody(res.Raw, !cfg.Redact)
	if gap != nil {
		env.AddWarning(*gap)
	}
	if softDelete && env.Changed != nil {
		env.Changed.Trashed = len(res.Docs)
		env.Changed.Deleted = 0
	}
	errMsg := ""
	if callErr != nil {
		errMsg = callErr.Error()
	}
	return w.finish(env, res.HTTP, len(res.Docs), len(res.Errors), errMsg)
}

// unsafePassthroughWarning describes the passthrough path on BOTH the dry-run
// and the executed envelope. It used to be attached only after the delete had
// happened, so the preview an agent is told to read first carried no trace of
// it. n is the advisory phase-1 count.
func unsafePassthroughWarning(n int) output.Warning {
	return output.Warning{
		Code: "unsafe_passthrough_where",
		Message: fmt.Sprintf(
			"--unsafe-passthrough-where sends the filter straight to DELETE: the server ignores --max-docs, ignores trash (this is a hard delete even where `pay restore` normally works), and removes every match. %d document(s) matched when PayCLI counted, but the server re-evaluates the filter at delete time, so the real total may differ.",
			n),
		Hint: "drop the flag to get the counted, id-resolved, capped, chunked behaviour back",
	}
}

// runUnsafeDelete is the documented escape hatch: the raw server semantics, in
// which DELETE ignores every cap and removes every match.
// n is the phase-1 count for the same scope. It is advisory, not the
// exact-blast-radius guarantee §12.3 gives the id-resolved path: the server
// re-evaluates the filter when the DELETE lands, so the real total may differ.
func runUnsafeDelete(ctx context.Context, d *Deps, client *payload.Client, w *writeOp,
	t *collTarget, where query.Where, p query.Params, scope query.Params, n int,
	classify payload.Option) (*output.Envelope, error) {
	cfg := d.cfg()
	warn := unsafePassthroughWarning(n)
	if cfg.DryRun {
		// The URL mirrors what DeleteWhereUnsafe actually puts on the wire —
		// p (trash, locale, overrideLock) with the filter attached — rather
		// than the where/trash pair the preview used to show.
		req := p.Clone()
		req.Where = where
		req.Limit = nil
		q, _ := req.Encode()
		env, err := emitDryRun(d, w, safety.CmdDelete, "DELETE",
			client.URLFor(&payload.Request{Method: "DELETE", Path: "/" + t.Slug, Query: q}), nil, n, nil)
		if err != nil {
			return nil, err
		}
		env.AddWarning(warn)
		return env, nil
	}
	if err := w.confirm(n); err != nil {
		return nil, err
	}
	if err := w.pre(n); err != nil {
		return nil, err
	}
	res, callErr := client.DeleteWhereUnsafe(ctx, t.Slug, where, p, classify, payload.WithKnownRoute())
	if res == nil {
		if callErr != nil {
			w.post(false, 0, 0, n, callErr.Error(), "")
			return nil, callErr
		}
		res = &payload.BulkResult{}
	}
	retry := fmt.Sprintf("pay delete %s --where 'id in %s' --permanent%s", t.Slug, joinIDs(failedIDsOf(res)), d.profileFlag())
	env := bulkEnvelope(safety.CmdDelete, "Deleted", t, res, res.IDs(), nil, retry).
		WithMeta(withLocaleMeta(w.run.meta(), p)).
		WithRawBody(res.Raw, !cfg.Redact)
	env.AddWarning(warn)
	errMsg := ""
	if callErr != nil {
		errMsg = callErr.Error()
	}
	return w.finish(env, res.HTTP, len(res.Docs), len(res.Errors), errMsg)
}
