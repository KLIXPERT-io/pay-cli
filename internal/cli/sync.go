package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/manifest"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

// ---------------------------------------------------------------------------
// F6 — `pay plan` and `pay sync`
// ---------------------------------------------------------------------------

// manifestFormatHelp is the item format, shared by both commands' help.
const manifestFormatHelp = `A manifest file holds one item or a JSON array of items; a directory argument
contributes every *.json file below it (lexical order, dot-files skipped), and
- reads one file from stdin. A saved ` + "`pay get`" + `, ` + "`pay find`" + ` or ` + "`pay globals get`" + `
envelope is read as items too (collection from target, status keep, match slug —
or id for a document without one; read it with --depth 0 --draft).

An update keeps what the body does not mention: a row without an id takes the id
of the stored row it replaces (same blockType, in order), so values an editor set
and nested rows the body leaves out stay, and the diff shows exactly the write.

  {"key": "home",                   optional; default "<collection>/<match values>"
   "collection": "pages",           or "global": "footer" (no match)
   "match": ["slug"],               fields whose values in data identify the document; default slug
   "status": "keep",                keep (default) | draft | published — the ONLY status control;
                                    a "_status" inside data is removed (warning body_status_ignored);
                                    keep saves a published document with a pending draft into
                                    that draft, the live version unchanged (warning saved_as_draft)
   "locale": "de",                  optional
   "data": { … the fields …, "parent": {"$ref": "@home"} }}

References: {"$ref":"pages","slug":"about"} → the id; add "$as":"polymorphic" for
{relationTo,value}; "where":{…} for any Payload filter; {"$ref":"@key"} → the id of
another item of the plan (created first when it does not exist yet).`

type planFlags struct {
	opts planOptions
}

func (f *planFlags) register(cmd *cobra.Command, sync bool) {
	fl := cmd.Flags()
	fl.StringArrayVar(&f.opts.ignore, "ignore", nil, "path pattern the diff ignores, e.g. meta.image or layout[].blockName (repeatable)")
	fl.BoolVar(&f.opts.full, "full", false, "keep rich-text editor states in the diff's from/to instead of text")
	fl.BoolVar(&f.opts.brief, "brief", false, "omit each item's change list (the summary and fields stay)")
	if sync {
		fl.BoolVar(&f.opts.failFast, "fail-fast", false, "stop at the first item that fails; the rest are not_attempted")
		fl.BoolVar(&f.opts.noEcho, "no-echo-check", false, "skip the §10.2 echo-diff on every write")
	}
}

func init() {
	Register(newPlanCmd)
	Register(newSyncCmd)
}

func newPlanCmd(rt *Runtime) *cobra.Command {
	f := &planFlags{}
	cmd := &cobra.Command{
		GroupID: GroupWrite,
		Use:     "plan <DIR|FILE|->...",
		Short:   "Show what `pay sync` would do with a set of manifest files (read-only)",
		Long: `Read manifest files and the server, and report per item what a sync would do:
create, update (with the structural diff of exactly what changes) or unchanged.
Nothing is written. Items are listed in execution order: a document another item
references is created first.

` + manifestFormatHelp,
		Args: minArgs(1, "pay plan <DIR|FILE|->..."),
	}
	cmd.RunE = runData(rt, safety.CmdPlan, func(ctx context.Context, cmd *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		return runPlan(ctx, d, f, args)
	})
	f.register(cmd, false)
	SetHelp(cmd, planHelp())
	return cmd
}

func newSyncCmd(rt *Runtime) *cobra.Command {
	f := &planFlags{}
	cmd := &cobra.Command{
		GroupID: GroupWrite,
		Use:     "sync <DIR|FILE|->...",
		Short:   "Make the server match a set of manifest files: create, update or leave each item",
		Long: `Plan the manifests exactly like ` + "`pay plan`" + `, then write every item that is not
unchanged, in dependency order, through the ordinary create/update paths: backups,
the audit log and the echo-diff apply to each item. Re-running a sync is safe: an
item already in place is unchanged and is not written again.

It is an L2 operation: it prompts on a TTY and needs --yes in a non-TTY (exit 11).
A failed item does not stop the others unless --fail-fast; items that reference a
failed item are not attempted. Some writes committed and some failed = exit 7.

` + manifestFormatHelp,
		Args: minArgs(1, "pay sync <DIR|FILE|->... [--dry-run] [--yes]"),
	}
	cmd.RunE = runData(rt, safety.CmdSync, func(ctx context.Context, cmd *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		return runSync(ctx, d, f, args)
	})
	f.register(cmd, true)
	SetHelp(cmd, syncHelp())
	return cmd
}

func planHelp() *Help {
	return &Help{
		Synopsis: []string{
			"pay plan <DIR|FILE|->... [--brief] [--ignore PATH]... [--full]",
		},
		Collections: true,
		Args: []ArgSpec{
			{Name: "manifest", Required: true, Type: "path", Example: "./content"},
		},
		FlagInfo: map[string]FlagInfo{
			"ignore": {Grammar: "a change path; [] matches any index: layout[].blockName", Repeatable: true},
		},
		Output: OutputSpec{
			Kind: output.KindOpResult,
			Skeleton: `{"summary":{"items":3,"create":1,"update":1,"unchanged":1},` +
				`"items":[{"key":"pages/about","action":"update","id":12,"write_mode":"save","depends_on":[],"fields":["title"],"diff":{…}},…]}`,
		},
		ExitCodes: ReadExitCodes,
		Examples: []Example{
			{Why: "what would a sync of this directory change?", Cmd: "pay plan ./content"},
			{Why: "the one-line-per-item view", Cmd: "pay plan ./content --output table"},
			{Why: "only the counts and the touched fields", Cmd: "pay plan ./content --brief --path .summary"},
			{Why: "a manifest from stdin: plan one exported page", Cmd: "pay get pages 12 --depth 0 --draft | pay plan -"},
			{Why: "ignore a field a hook recomputes on every save", Cmd: "pay plan ./content --ignore 'breadcrumbs'"},
		},
		Mistakes: []Mistake{
			{Wrong: "Putting \"_status\":\"published\" in data to publish.",
				Right: "It is removed (body_status_ignored). Set the item's \"status\": \"published\" — status is controlled there and nowhere else."},
			{Wrong: "Hand-maintaining an ids.json of what was already created.",
				Right: "Each item is matched on the server (default: its slug) every run; `pay plan` says create/update/unchanged from the server's real state."},
			{Wrong: "Writing a relationship id that differs between servers.",
				Right: "Name the document: {\"$ref\":\"pages\",\"slug\":\"about\"}, or {\"$ref\":\"@key\"} for another item of the same plan."},
			{Wrong: "Expecting an item that is never 'unchanged' to be a bug in pay.",
				Right: "A value a hook rewrites on save (a formatted slug, computed breadcrumbs) differs every time: write the stored form, or --ignore that path."},
		},
		SeeAlso: []string{
			"pay sync <DIR|FILE>...   # execute the plan",
			"pay upsert <collection> --match slug --data-file F   # one document, no manifest",
			"pay diff <collection> <id> --data-file F   # one body against one document",
		},
	}
}

func syncHelp() *Help {
	return &Help{
		Synopsis: []string{
			"pay sync <DIR|FILE|->... [--dry-run] [--yes] [--fail-fast] [--brief] [--ignore PATH]...",
		},
		Collections: true,
		Args: []ArgSpec{
			{Name: "manifest", Required: true, Type: "path", Example: "./content"},
		},
		FlagInfo: map[string]FlagInfo{
			"ignore":    {Grammar: "a change path; [] matches any index: layout[].blockName", Repeatable: true},
			"fail-fast": {Note: "stop at the first failed item; the rest are reported not_attempted"},
		},
		Output: OutputSpec{
			Kind: output.KindOpResult,
			Skeleton: `{"summary":{"items":3,"create":1,"update":1,"unchanged":1,"created":1,"updated":1,"failed":0,"not_attempted":0},` +
				`"items":[…,"result":"created"],"succeeded":[{"key":"pages/about","action":"update","id":12}],"failed":[],"not_attempted":[]}`,
		},
		ExitCodes: BulkExitCodes,
		Examples: []Example{
			{Why: "always first: the plan, and which backups would be written", Cmd: "pay sync ./content --dry-run"},
			{Why: "run it; --yes is required in a non-TTY (else exit 11)", Cmd: "pay sync ./content --yes"},
			{Why: "prove it is done: every item is now unchanged", Cmd: "pay plan ./content --brief --path .summary"},
			{Why: "stop at the first failure instead of continuing", Cmd: "pay sync ./content --yes --fail-fast"},
			{Why: "a few files rather than a directory", Cmd: "pay sync home.json about.json footer.json --yes"},
		},
		Mistakes: []Mistake{
			{Wrong: "Being afraid to re-run after exit 7.",
				Right: "Unlike a bulk update, a sync is idempotent: fix the failed items (error.failures), re-run the same command, and only what is still missing is written."},
			{Wrong: "\"_status\":\"published\" in data to publish a page.",
				Right: "It is removed (body_status_ignored). Use the item's \"status\": \"published\"; \"draft\" saves a draft and never unpublishes."},
			{Wrong: "Two pages that reference each other and do not exist yet.",
				Right: "That is a cycle (ref_cycle): create one without the reference first, sync, then add it."},
			{Wrong: "Syncing without --dry-run first.",
				Right: "--dry-run prints the whole plan (create/update/unchanged, the diff of every update) and would_backup, and writes nothing."},
		},
		SeeAlso: []string{
			"pay plan <DIR|FILE>...   # the same plan, never writes",
			"pay backups list   # the pre-images sync saved (when backups are on)",
			"pay audit tail --action update   # what was written",
		},
	}
}

func runPlan(ctx context.Context, d *Deps, f *planFlags, args []string) (*output.Envelope, error) {
	run := d.beginRun()
	sp, err := buildPlan(ctx, d, args, f.opts)
	if err != nil {
		return planError(d, sp, err), nil
	}
	return sp.envelope(d, safety.CmdPlan, run.meta(), false), nil
}

// planError is a plan that cannot be executed. When the manifests could be
// read, the partial plan rides along in data so the failing items can be
// seen next to the rest.
func planError(d *Deps, sp *syncPlan, err error) *output.Envelope {
	env := output.NewError(d.RT.Command, err)
	if sp != nil {
		env.Data = sp.data()
		for _, w := range sp.warnings() {
			env.AddWarning(w)
		}
	}
	return env
}

// envelope renders a plan (or a dry-run sync) as op_result.
func (sp *syncPlan) envelope(d *Deps, command string, meta output.Meta, dryRun bool) *output.Envelope {
	var data any = sp.data()
	if dryRun {
		pd := sp.data()
		pd.DryRun = &dryRun
		pd.WouldBackup = sp.wouldBackup(d)
		data = pd
	}
	if d.RT != nil && d.RT.Out != nil && d.RT.Out.Format == output.FormatTable && d.cfg().Path == "" {
		rows, cols := sp.tableRows(false)
		data = rows
		d.RT.Out.Columns = cols
	}
	env := output.New(command, output.KindOpResult, data).WithMeta(meta)
	for _, w := range sp.warnings() {
		env.AddWarning(w)
	}
	return env
}

// wouldBackup aggregates §12.8's preview over every item the sync would
// write to an existing document.
func (sp *syncPlan) wouldBackup(d *Deps) *backupPreview {
	var out *backupPreview
	for _, pi := range sp.sequence() {
		if pi.dc.action != actionUpdate {
			continue
		}
		bt := BackupTarget{Global: pi.g, Params: pi.params}
		if pi.g == nil {
			bt = BackupTarget{Collection: pi.t, IDs: []any{pi.id}, Params: pi.params}
		}
		prev := d.BackupPreview(safety.CmdSync, bt)
		if prev == nil {
			return nil
		}
		if out == nil {
			out = &backupPreview{Dir: prev.Dir, Paths: []string{}}
		}
		out.Documents += prev.Documents
		for _, p := range prev.Paths {
			if len(out.Paths) >= safety.SampleLimit {
				out.Truncated = true
				break
			}
			out.Paths = append(out.Paths, p)
		}
	}
	return out
}

// syncData is `pay sync`'s data after a real run.
type syncData struct {
	planData
	Succeeded    []syncDone     `json:"succeeded"`
	Failed       []syncFailed   `json:"failed"`
	NotAttempted []syncNotTried `json:"not_attempted"`
}

type syncDone struct {
	Key        string `json:"key"`
	Action     string `json:"action"`
	Collection string `json:"collection,omitempty"`
	Global     string `json:"global,omitempty"`
	ID         any    `json:"id"`
}

type syncFailed struct {
	Key     string      `json:"key"`
	Code    apierr.Code `json:"code"`
	Message string      `json:"message"`
}

type syncNotTried struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// Sync results per item.
const (
	resultCreated      = "created"
	resultUpdated      = "updated"
	resultUnchanged    = "unchanged"
	resultFailed       = "failed"
	resultNotAttempted = "not_attempted"
)

func runSync(ctx context.Context, d *Deps, f *planFlags, args []string) (*output.Envelope, error) {
	run := d.beginRun()
	cfg := d.cfg()
	sp, err := buildPlan(ctx, d, args, f.opts)
	if err != nil {
		return planError(d, sp, err), nil
	}
	if cfg.DryRun {
		return sp.envelope(d, safety.CmdSync, run.meta(), true), nil
	}
	var creates, updates, globals int
	for _, pi := range sp.items {
		switch {
		case pi.dc.action == actionCreate:
			creates++
		case pi.dc.action == actionUpdate && pi.g != nil:
			globals++
		case pi.dc.action == actionUpdate:
			updates++
		}
	}
	if creates+updates+globals > 0 {
		summary := fmt.Sprintf("sync will create %d and update %d document(s)", creates, updates)
		if globals > 0 {
			summary += fmt.Sprintf(" and %d global(s)", globals)
		}
		summary += fmt.Sprintf(" (%d of %d manifest item(s); the rest are unchanged)", creates+updates+globals, len(sp.items))
		if err := d.confirmer().Confirm(safety.Request{
			Op: safety.Op{Command: safety.CmdSync}, Affected: creates + updates + globals, Summary: summary,
		}); err != nil {
			return nil, err
		}
		// One confirmation covers every item: the per-item writes run with
		// --yes, so an L2 global item or confirm_writes do not ask again.
		if d.RT != nil && d.RT.Cfg != nil && !d.RT.Cfg.Yes {
			confirmed := *d.RT.Cfg
			confirmed.Yes = true
			d.RT.Cfg = &confirmed
		}
	}
	client, err := d.requireClient()
	if err != nil {
		return nil, err
	}
	sp.execute(ctx, d, client)
	return sp.syncEnvelope(d, run.meta(), args), nil
}

// execute writes every item in order.
func (sp *syncPlan) execute(ctx context.Context, d *Deps, client *payload.Client) {
	sp.executed = true
	stopped := ""
	for _, i := range sp.order {
		pi := sp.items[i]
		if pi.dc.action == actionUnchanged {
			pi.result = resultUnchanged
			continue
		}
		if stopped != "" {
			pi.result, pi.reason = resultNotAttempted, "--fail-fast stopped the sync after "+stopped+" failed"
			continue
		}
		if ctx.Err() != nil {
			pi.result, pi.reason = resultNotAttempted, "the sync was interrupted before this item"
			continue
		}
		if blocked := sp.blockedBy(pi); blocked != "" {
			pi.result, pi.reason = resultNotAttempted, "it references "+blocked+", which was not written"
			continue
		}
		if len(pi.deps) > 0 {
			// The items it references exist now: resolve again with their
			// real ids and re-decide (the placeholder diff was a preview).
			if err := sp.decideItem(ctx, pi); err != nil {
				sp.failed(pi, err)
				if sp.opts.failFast {
					stopped = pi.it.Key
				}
				continue
			}
			if pi.dc.action == actionUnchanged {
				pi.result = resultUnchanged
				continue
			}
		}
		env, err := sp.write(ctx, d, client, pi)
		if err != nil {
			if pi.dc.action == actionCreate {
				pi.matchRejected, err = matchFieldRejected(err, pi.t, pi.it.Match, pi.matchedBy)
			}
			sp.failed(pi, err)
			if sp.opts.failFast {
				stopped = pi.it.Key
			}
			continue
		}
		if doc, ok := env.Data.(payload.Doc); ok && pi.g == nil && doc.ID() != nil {
			pi.id, pi.idString = doc.ID(), doc.IDString()
		}
		pi.warnings = appendNewWarnings(pi.warnings, env.Warnings...)
		if doc, ok := env.Data.(payload.Doc); ok && pi.dc.action == actionUpdate {
			if w := hookRewroteWarning(doc, pi.dc, diffOptions(pi.shard(), sp.opts.ignore, sp.opts.full),
				"run the same sync again: it writes the declared value back (`pay plan` shows it first)"); w != nil {
				pi.warnings = append(pi.warnings, *w)
			}
		}
		if doc, ok := env.Data.(payload.Doc); ok && pi.g == nil {
			if w := matchNormalized(pi.t, pi.it.Match, pi.matchedBy, doc, "the manifest item"); w != nil {
				pi.warnings = append(pi.warnings, *w)
			}
		}
		if pi.dc.action == actionCreate {
			pi.result = resultCreated
		} else {
			pi.result = resultUpdated
		}
	}
}

func (sp *syncPlan) failed(pi *planItem, err error) {
	pi.result = resultFailed
	pi.err = manifestHint(pi, apierr.From(err))
}

// manifestHint rewrites validation_failed's generic hint, which offers
// --draft and a <collection> placeholder, in manifest terms: sync has no
// --draft; an item is saved as a draft with its "status".
func manifestHint(pi *planItem, e *apierr.Error) *apierr.Error {
	if e == nil || e.Code != apierr.CodeValidationFailed || !strings.Contains(e.Hint, "add --draft") {
		return e
	}
	describe := "pay describe " + pi.slug() + " --required-only"
	hint := fmt.Sprintf("fix every path in error.fields in the item's \"data\" (%s) and re-run the sync; `%s` lists the required fields",
		pi.it.Label(), describe)
	if pi.t != nil && flagOn(pi.t.flags().Drafts) {
		hint += ". To store it incomplete, set \"status\": \"draft\" on the item: Payload skips required-field validation for drafts"
	}
	return e.WithHint("%s", hint)
}

// blockedBy names the first item pi references that a sync was supposed to
// create but did not.
func (sp *syncPlan) blockedBy(pi *planItem) string {
	for _, j := range pi.deps {
		dep := sp.items[j]
		if dep.result == resultFailed || dep.result == resultNotAttempted || dep.id == nil {
			return dep.it.Key
		}
	}
	return ""
}

// write sends one item through the ordinary write path.
func (sp *syncPlan) write(ctx context.Context, d *Deps, client *payload.Client, pi *planItem) (*output.Envelope, error) {
	body := pi.dc.writeBody()
	p := pi.dc.writeParams(pi.params)
	switch {
	case pi.g != nil:
		return runGlobalUpdateOne(ctx, d, client, safety.CmdSync, pi.g, body, p, nil, sp.opts.noEcho, nil)
	case pi.dc.action == actionCreate:
		return runCreateOne(ctx, d, client, safety.CmdSync, &createFlags{noEchoCheck: sp.opts.noEcho}, pi.t, body, p, nil)
	default:
		return runUpdateOne(ctx, d, client, safety.CmdSync, &updateFlags{noEchoCheck: sp.opts.noEcho}, pi.t, pi.idString, body, p, nil)
	}
}

// syncEnvelope renders the executed sync. All items fine → op_result. Some
// failed → partial_failure (exit 7) when anything was committed, else the
// first failure's own error; data carries the whole result either way.
func (sp *syncPlan) syncEnvelope(d *Deps, meta output.Meta, args []string) *output.Envelope {
	out := syncData{planData: sp.data(), Succeeded: []syncDone{}, Failed: []syncFailed{}, NotAttempted: []syncNotTried{}}
	var created, updated, failed, notTried int
	changed := &output.Changed{IDs: []any{}}
	var failures []apierr.Failure
	var first *apierr.Error
	var firstItem *planItem
	for _, pi := range sp.sequence() {
		switch pi.result {
		case resultCreated, resultUpdated:
			out.Succeeded = append(out.Succeeded, syncDone{Key: pi.it.Key, Action: pi.dc.action,
				Collection: pi.it.Collection, Global: pi.it.Global, ID: pi.id})
			if pi.result == resultCreated {
				created++
				changed.Created++
			} else {
				updated++
				changed.Updated++
			}
			if pi.id != nil {
				changed.IDs = append(changed.IDs, pi.id)
			}
		case resultFailed:
			failed++
			out.Failed = append(out.Failed, syncFailed{Key: pi.it.Key, Code: pi.err.Code, Message: pi.err.Message})
			failures = append(failures, apierr.Failure{ID: pi.it.Key, Code: pi.err.Code, Message: pi.err.Message, Fields: pi.err.Fields})
			if first == nil {
				first, firstItem = pi.err, pi
			}
		case resultNotAttempted:
			notTried++
			out.NotAttempted = append(out.NotAttempted, syncNotTried{Key: pi.it.Key, Reason: pi.reason})
		}
	}
	out.Summary.Created, out.Summary.Updated = &created, &updated
	out.Summary.Failed, out.Summary.NotAttempted = &failed, &notTried

	var data any = out
	if failed+notTried == 0 && d.RT != nil && d.RT.Out != nil && d.RT.Out.Format == output.FormatTable && d.cfg().Path == "" {
		rows, cols := sp.tableRows(true)
		data = rows
		d.RT.Out.Columns = cols
	}
	rerun := "pay sync " + planArgs(args) + " --yes" + d.profileFlag()
	replan := "pay plan " + planArgs(args) + " --brief" + d.profileFlag()
	warnings := sp.warnings()

	if failed == 0 && notTried == 0 {
		env := output.New(safety.CmdSync, output.KindOpResult, data).WithMeta(meta).WithChanged(changed)
		if created+updated > 0 {
			env.WithNext(&output.Next{Reason: output.ReasonVerifyWrite, Cmd: replan})
		}
		for _, w := range warnings {
			env.AddWarning(w)
		}
		return env
	}

	safe := "Re-running `" + rerun + "` is SAFE: a sync re-plans against the server, skips every item that is already " +
		"in place (unchanged) and writes only what is still missing or different. Fix the failed items' manifests " +
		"(error.failures) first — re-running without a fix fails the same way."
	var e *apierr.Error
	switch {
	case created+updated > 0:
		e = apierr.New(apierr.CodePartialFailure,
			"sync wrote %d of %d item(s) that needed a write; %d failed, %d not attempted.",
			created+updated, created+updated+failed+notTried, failed, notTried).
			WithHint("%d WRITE(S) ARE COMMITTED (data.succeeded). %s", created+updated, safe)
	case first != nil:
		// No write was confirmed, so there is nothing partial about it: the
		// failure is the first item's own, with its own exit status. A 5xx
		// or a timeout on a write may still have committed it (§11.4), and
		// pay cannot know — say so instead of "wrote nothing".
		lead, hint := "sync wrote nothing", safe
		if firstItem != nil && firstItem.matchRejected {
			hint = first.Hint + ". " + safe
		}
		if uncertainWrite(failures) {
			lead = "no write of this sync was confirmed"
			hint = "A server error or timeout on a write MAY HAVE COMMITTED it: `" + replan + "` shows the real state " +
				"before anything is re-run. " + safe
		}
		e = apierr.New(first.Code, "%s: %d item(s) failed, %d not attempted; the first: %s",
			lead, failed, notTried, first.Message).
			WithHint("%s", hint)
	default:
		// Only an interruption leaves items unattempted without a failure.
		e = apierr.New(apierr.CodeTimeout, "the sync was interrupted before it wrote anything: %d item(s) not attempted", notTried).
			WithHint("%s", safe)
	}
	e = e.WithFailures(failures...)
	env := output.NewError(safety.CmdSync, e).WithMeta(meta).WithChanged(changed)
	env.Partial = created+updated > 0
	env.Data = data
	env.WithNext(&output.Next{
		Reason: output.ReasonRetryFailedSubset, Cmd: rerun,
		Alternatives: []output.Alternative{{Why: "see what is still pending first (writes nothing)", Cmd: replan}},
	})
	for _, w := range warnings {
		env.AddWarning(w)
	}
	return env
}

// uncertainWrite reports a failure whose write may have committed anyway:
// Payload answered 5xx, or nothing answered in time.
func uncertainWrite(failures []apierr.Failure) bool {
	for _, f := range failures {
		if f.Code == apierr.CodeServerError || f.Code == apierr.CodeTimeout {
			return true
		}
	}
	return false
}

// WarnMatchValueNormalized says the server stored a match field in another
// form than the body sent (a slug-formatting hook): the next plan or upsert,
// which matches on the body's value, will not find the document.
const WarnMatchValueNormalized = "match_value_normalized"

// matchNormalized compares the written document's match fields with the
// values the match used.
func matchNormalized(t *collTarget, fields []string, matched map[string]any, doc payload.Doc, what string) *output.Warning {
	var paths, parts, fix []string
	for _, f := range fields {
		want, ok := matched[f]
		if !ok {
			continue
		}
		got, ok := manifest.Lookup(map[string]any(doc), f)
		if !ok || got == nil || manifest.ScalarString(got) == manifest.ScalarString(want) {
			continue
		}
		paths = append(paths, f)
		parts = append(parts, fmt.Sprintf("%s %q (sent %q)", f, manifest.ScalarString(got), manifest.ScalarString(want)))
		fix = append(fix, fmt.Sprintf("%q: %q", f, manifest.ScalarString(got)))
	}
	if len(paths) == 0 {
		return nil
	}
	return &output.Warning{
		Code: WarnMatchValueNormalized,
		Message: fmt.Sprintf("%s %s was stored with %s: a hook rewrote the value the match looks for, so the next plan or upsert "+
			"will not find this document and will try to CREATE it again (a unique field then fails validation, any other "+
			"field gets a duplicate)", t.Slug, doc.IDString(), strings.Join(parts, ", ")),
		Paths: paths,
		Hint:  fmt.Sprintf("write the stored value in %s (%s); the next plan then finds the document", what, strings.Join(fix, ", ")),
	}
}

// matchFieldRejected rewrites the hint of a create that failed on one of its
// match fields: the usual cause is that the document already exists with a
// normalized form of that value (a slug hook on an earlier run), so the match
// did not find it.
func matchFieldRejected(err error, t *collTarget, fields []string, matched map[string]any) (bool, error) {
	e := apierr.From(err)
	if t == nil || e.Code != apierr.CodeValidationFailed {
		return false, err
	}
	for _, f := range e.Fields {
		if !containsString(fields, f.Path) {
			continue
		}
		v := manifest.ScalarString(matched[f.Path])
		return true, e.WithHint("the match found no document with %s %q, and Payload rejected that value for a new one (%s): "+
			"a document with a normalized form of it probably exists already (a hook formats it on save). "+
			"Find it (pay find %s --where '%s like %s' --select id,%s%s) and write its stored value in the manifest",
			f.Path, v, f.Message, t.Slug, f.Path, likeTerm(v), f.Path, draftFlag(flagOn(t.flags().Drafts)))
	}
	return false, err
}

// likeTerm is the longest word of a value, for a `like` search that survives
// the reformatting.
func likeTerm(v string) string {
	best := ""
	for _, w := range strings.FieldsFunc(strings.ToLower(v), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) {
		if len(w) > len(best) {
			best = w
		}
	}
	if best == "" {
		best = "…"
	}
	return best
}
