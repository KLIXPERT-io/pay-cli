package cli

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/orphans"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

// urlValues is a tiny helper for the odd query parameter query.Params does not
// model (autosave, overrideLock).
func urlValues(pairs ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		v.Set(pairs[i], pairs[i+1])
	}
	return v
}

// ---------------------------------------------------------------------------
// bulk write scope (§12.3)
// ---------------------------------------------------------------------------

// bulkScope is the read scope §12.3's three phases share. Count, id resolution
// and the write itself must address the SAME population, so the phases that
// read are derived from the write's own params instead of a bare where clause:
// trash, draft and locale decide which documents exist for this command at all.
//
// The projection/paging fields are deliberately absent — ResolveIDsScoped owns
// those — as are the write-only extras (autosave, overrideLock).
func bulkScope(where query.Where, p query.Params) query.Params {
	return query.Params{
		Where:      where,
		WhereStyle: p.WhereStyle,
		WhereRaw:   p.WhereRaw,
		Trash:      p.Trash,
		Draft:      p.Draft,
		Locale:     p.Locale,
	}
}

// warnBulkResolveGap is raised when phase 1 and phase 3 disagree about how many
// documents the write covers. The write is capped and executed on the resolved
// ids, so a gap means the caller is about to touch fewer documents than the
// count they were shown — the silent-no-op failure mode made visible.
const warnBulkResolveGap = "bulk_resolve_gap"

func resolveGapWarning(counted, resolved int) *output.Warning {
	if counted == resolved {
		return nil
	}
	return &output.Warning{
		Code: warnBulkResolveGap,
		Message: fmt.Sprintf(
			"--where counted %d document(s) but only %d could be resolved by id; the write covers the %d resolved one(s).",
			counted, resolved, resolved),
		Hint: "documents may have changed between the two requests; re-run `pay find` with the same --where (and --trash/--draft) to see the current set",
	}
}

// localeMetaOf is §10.1's locale disclosure for the write verbs, derived from
// the params that were actually encoded. meta.locale on a write used to be
// {null, null} however many locale parameters went out, so an agent could not
// see that `fallback-locale=none` is why a field in the echo came back null —
// nor that it was ABSENT, which is why finding 19 was invisible from the
// envelope alone.
func localeMetaOf(p query.Params) output.Locale {
	var out output.Locale
	if p.Locale != "" {
		l := p.Locale
		out.Requested = &l
	}
	if p.FallbackLocale != "" {
		f := p.FallbackLocale
		out.Fallback = &f
	}
	return out
}

// withLocaleMeta stamps that disclosure onto a meta block.
func withLocaleMeta(m output.Meta, p query.Params) output.Meta {
	m.Locale = localeMetaOf(p)
	return m
}

// applyWriteLocale is §7.9a / conflict 35 for every verb that echoes a document
// back. A write is also a read: `data` is what the agent believes it stored, so
// `locale=de` sent WITHOUT `fallback-locale` lets Payload apply its default
// fallback:true and return default-locale text that is byte-identical to a real
// translation — the read-modify-write loop that copies English into `de`.
//
// It exists as one helper because the bug was one line duplicated eight times:
// update, create, publish, unpublish, duplicate, restore, delete and upload
// each hand-built `query.Params{Locale: locale}`, and fixing two of them left
// the other six answering the same question wrongly. Routing them all through
// resolveLocale also picks up the profile's `locale` (which the write verbs
// ignored entirely) and validates the code against discovery BEFORE the request
// is sent, so `--locale dee` fails instead of writing into a locale that does
// not exist.
//
// The fallback is deliberately not exposed as a flag on the write verbs: there
// is no honest reason for a write to ask the server to lie about what it holds.
func applyWriteLocale(d *Deps, t *collTarget, locale string, p *query.Params) (*output.Warning, error) {
	loc, warn, err := resolveLocale(d, t, locale, "")
	if err != nil {
		return nil, err
	}
	if loc.Requested != nil {
		p.Locale = *loc.Requested
	}
	if loc.Fallback != nil {
		p.FallbackLocale = *loc.Fallback
	}
	return warn, nil
}

// appendNewWarnings appends src to dst, skipping any entry dst already carries
// verbatim. The bulk form resolves the query twice — once for the write params
// and once, through readFlags.build, for §12.3's count/resolve phases — so a
// profile-level `locale` PayCLI cannot verify would otherwise be reported to
// the agent twice, reading like two separate problems.
func appendNewWarnings(dst []output.Warning, src ...output.Warning) []output.Warning {
	key := func(w output.Warning) string {
		return w.Code + "\x00" + w.Message + "\x00" + strings.Join(w.Paths, ",")
	}
	seen := make(map[string]bool, len(dst))
	for _, w := range dst {
		seen[key(w)] = true
	}
	for _, w := range src {
		if seen[key(w)] {
			continue
		}
		seen[key(w)] = true
		dst = append(dst, w)
	}
	return dst
}

// ---------------------------------------------------------------------------
// bulk result rendering (§12.5)
// ---------------------------------------------------------------------------

// bulkData is the §12.5 data block.
type bulkData struct {
	Succeeded    []payload.Doc    `json:"succeeded"`
	Failed       []map[string]any `json:"failed"`
	NotAttempted []any            `json:"not_attempted"`
}

// serverErrorMessage is what Payload substitutes for a sub-error whose
// isPublic is false. The real message is one `debug: true` away.
const serverErrorMessage = "Something went wrong."

// bulkEnvelope builds §12.5's envelope. Exit 7 is emitted whenever any
// document failed, even though committed documents are in the same body: the
// outer transaction commits after the per-document loop, so the successes are
// real and must never be re-run.
func bulkEnvelope(command, verbPast string, t *collTarget, res *payload.BulkResult,
	attempted []any, notAttempted []any, retryCmd string) *output.Envelope {
	succeeded := res.Docs
	if succeeded == nil {
		succeeded = []payload.Doc{}
	}
	failed := make([]map[string]any, 0, len(res.Errors))
	failures := make([]apierr.Failure, 0, len(res.Errors))
	failedIDs := make([]any, 0, len(res.Errors))
	for _, e := range res.Errors {
		failed = append(failed, map[string]any{"id": e.ID})
		failedIDs = append(failedIDs, e.ID)
		f := apierr.Failure{ID: e.ID, Code: apierr.CodeValidationFailed, Message: e.Message}
		if e.Code != "" && e.Code.Known() {
			// A per-document write carries its own classified code: a 404
			// is doc_not_found, not a validation failure.
			f.Code = e.Code
		}
		if strings.TrimSpace(e.Message) == serverErrorMessage {
			f.Code = apierr.CodeServerError
			f.Message = serverErrorMessage + " (Payload withheld the real message because the sub-error's isPublic is false; set debug: true in payload.config.ts to see it)"
		}
		failures = append(failures, f)
	}
	if notAttempted == nil {
		notAttempted = []any{}
	}

	data := bulkData{Succeeded: succeeded, Failed: failed, NotAttempted: notAttempted}
	changed := &output.Changed{IDs: res.IDs()}
	switch command {
	case safety.CmdDelete:
		changed.Deleted = len(succeeded)
	default:
		changed.Updated = len(succeeded)
	}

	if len(res.Errors) == 0 && len(notAttempted) == 0 {
		env := output.New(command, output.KindBulkResult, data).
			WithTarget(t.envTarget(nil)).
			WithChanged(changed)
		if warn := chunkedRawWarning(res); warn != nil {
			env.AddWarning(*warn)
		}
		return env
	}

	total := len(attempted)
	if total == 0 {
		total = len(succeeded) + len(res.Errors) + len(notAttempted)
	}
	e := apierr.New(apierr.CodePartialFailure,
		"%s %d of %d documents in %q; %d failed.", verbPast, len(succeeded), total, t.Slug, len(res.Errors)).
		WithHint("THE %d SUCCESSFUL WRITE(S) ARE ALREADY COMMITTED. Payload bulk operations are NOT transactional "+
			"over the REST API - do not re-run this command or you will re-apply it. Retry only the failed ids using next.cmd.",
			len(succeeded)).
		WithFailures(failures...)
	if res.Chunks > 1 {
		// error.http and error.raw can only hold ONE response. §12.3 writes the
		// id set in chunks of 100, so say which chunk they came from rather
		// than letting an agent debug a request that is not the failing one.
		e = e.WithHint("%s error.http and error.raw are the response of chunk %d of %d "+
			"(ids are written in chunks of %d); data.failed lists every failed id across all chunks.",
			e.Hint, failedChunkOf(res), res.Chunks, payload.BulkChunk)
	}
	if res.HTTP != nil {
		e = e.WithHTTP(&apierr.HTTP{
			Status: res.HTTP.Status, Method: res.HTTP.Method, URL: res.HTTP.URL, Attempts: res.HTTP.Attempts,
		})
	}
	if len(res.Raw) > 0 {
		e = e.WithRaw(res.Raw, false)
	}

	env := output.NewError(command, e).
		WithTarget(t.envTarget(nil)).
		WithChanged(changed)
	env.Partial = true
	env.Data = data
	if retryCmd != "" {
		env.WithNext(&output.Next{
			Reason: output.ReasonRetryFailedSubset,
			Cmd:    retryCmd,
			Args:   map[string]any{"ids": failedIDs},
		})
	}
	return env
}

// warnBulkChunked says data.raw is one chunk's response, not the whole write.
const warnBulkChunked = "bulk_response_is_one_chunk"

// failedChunkOf is the 1-based chunk the pinned response belongs to. A result
// with no failing chunk pinned nothing, so it is the last one.
func failedChunkOf(res *payload.BulkResult) int {
	if res.FailedChunk > 0 {
		return res.FailedChunk
	}
	return res.Chunks
}

// chunkedRawWarning is the successful-write half of the same problem: `raw` is
// the body of ONE of the requests this command issued, and an agent reading
// "Updated 50 Pages successfully" after a 150-document write would otherwise
// conclude the other 100 did not happen.
func chunkedRawWarning(res *payload.BulkResult) *output.Warning {
	if res.Chunks <= 1 {
		return nil
	}
	return &output.Warning{
		Code: warnBulkChunked,
		Message: fmt.Sprintf("this write was sent as %d requests of up to %d ids; `raw` is chunk %d's response only. "+
			"changed.ids covers every chunk.", res.Chunks, payload.BulkChunk, failedChunkOf(res)),
		Hint: "read changed.ids and data.succeeded for the whole write; `raw` is one request's body",
	}
}

// ---------------------------------------------------------------------------
// §10.2 echo-diff under a projection
// ---------------------------------------------------------------------------

// warnEchoCheckNarrowed says the echo-diff could only cover part of what was
// sent. It is never silent: suppressing the check without a word would hide a
// genuinely dropped field just as effectively as the false positive it fixes.
//
// It reports an ABSENCE OF EVIDENCE and must never be worded as evidence of
// absence. PayCLI did not see those paths come back, so it knows nothing about
// them: the write may have stored them, or Payload may have discarded them
// silently — the very case §10.2 exists for. Saying "they were still written"
// would be the confidently-wrong answer this whole product is built to stop,
// and it is strictly worse than the false positive it replaced, because a
// false positive makes an agent look while a false assurance stops it looking.
const warnEchoCheckNarrowed = "echo_check_narrowed"

// narrowEchoBody splits a request body by the --select projection: the fields
// the server was asked to echo, and the roots it was told to leave out.
//
// Payload honours `select` on POST/PATCH, so a non-selected field is absent
// from the echoed document BECAUSE OF THE PROJECTION, not because Payload
// discarded it. Diffing it would produce `input_silently_dropped` for a write
// that fully succeeded — the exact false positive §10.2 exists to avoid.
//
// Matching is at root granularity: `--select meta.title` is encoded as
// select[meta][title]=true, so comparing whole sub-paths would be brittle,
// while skipping the root risks at worst a missed warning, never a false one.
func narrowEchoBody(body map[string]any, sel []string) (map[string]any, []string) {
	keep := map[string]bool{}
	for _, s := range sel {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		keep[rootOf(strings.SplitN(s, "[", 2)[0])] = true
	}
	if len(keep) == 0 || len(body) == 0 {
		return body, nil
	}
	narrowed := make(map[string]any, len(body))
	var unchecked []string
	for k, v := range body {
		if keep[rootOf(strings.SplitN(k, "[", 2)[0])] {
			narrowed[k] = v
			continue
		}
		unchecked = append(unchecked, k)
	}
	sort.Strings(unchecked)
	return narrowed, unchecked
}

// echoWarnings is §10.2's check, projection-aware. It runs the diff over the
// part of the body the echo could possibly contain and reports what it could
// not check, so `--select` narrows the check instead of falsifying it.
func echoWarnings(slug string, body map[string]any, doc payload.Doc, sel []string, cfg echoConfig) []output.Warning {
	if cfg.Disabled || cfg.LocaleAll || len(body) == 0 || doc == nil {
		return nil
	}
	narrowed, unchecked := narrowEchoBody(body, sel)
	// {} rows are orphans (§10.2.1, reported as orphan_rows by the caller);
	// left in, the leaf diff pairs the body's rows with them by index and
	// reports the real rows as dropped input.
	out := echoDiff(slug, narrowed, payload.Doc(orphans.WithoutEmptyRows(map[string]any(doc))), cfg)
	if len(unchecked) > 0 {
		out = append(out, output.Warning{
			Code: warnEchoCheckNarrowed,
			Message: fmt.Sprintf("--select kept %d field(s) you sent out of the document Payload echoed back, "+
				"so PayCLI could NOT verify them: their fate is UNKNOWN, not confirmed. Payload discards an "+
				"unknown field or an unknown block type without an error, and the projection hides exactly "+
				"that, so do not assume the write kept them.", len(unchecked)),
			Paths: unchecked,
			Hint: fmt.Sprintf("re-read the document WITHOUT --select — `pay get %s <id> --depth 0` (add --draft "+
				"if you wrote a draft) — and check these paths hold the values you sent", slug),
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// pay update
// ---------------------------------------------------------------------------

type updateFlags struct {
	data   writeData
	filter readFlags

	draft        bool
	autosave     bool
	publish      bool
	unpublish    bool
	overrideLock bool

	depth       int
	selectF     []string
	locale      string
	noEchoCheck bool

	maxDocs  int
	all      bool
	perDoc   bool
	failFast bool
	limit    int

	// resetNested is §10.2.1's --reset-nested: carrier phases before the
	// write so the Postgres adapter clears every nested block table the
	// write itself would skip.
	resetNested bool
}

func init() { Register(newUpdateCmd) }

func newUpdateCmd(rt *Runtime) *cobra.Command {
	f := &updateFlags{}
	cmd := &cobra.Command{
		GroupID: GroupWrite,
		Use:     "update <collection> [id]",
		Short:   "Update one document by id, or every document matching --where (PATCH)",
		Long: `Update documents.

Payload validates the WHOLE document on a PATCH: a one-field update can come
back 400 naming fields you never sent. error.fields[].sent tells you which of
the named fields were actually in your request.

The bulk form never passes --where straight through: PayCLI counts the matches,
enforces --max-docs, resolves the exact ids and then writes them in chunks of
100, so the blast radius is exactly what --dry-run printed.`,
		Args: cobra.RangeArgs(1, 2),
	}
	cmd.RunE = runData(rt, safety.CmdUpdate, func(ctx context.Context, cmd *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		id := ""
		if len(args) == 2 {
			id = args[1]
		}
		return runUpdate(ctx, cmd, d, f, args[0], id)
	})
	f.data.register(cmd)
	f.filter.registerFilterFlags(cmd)
	fl := cmd.Flags()
	fl.BoolVar(&f.draft, "draft", false, "save as a draft, skipping required-field validation")
	fl.BoolVar(&f.autosave, "autosave", false, "mark the write as an autosave")
	fl.BoolVar(&f.publish, "publish", false, "also set _status=published")
	fl.BoolVar(&f.unpublish, "unpublish", false, "also set _status=draft, taking the live document offline (not with --draft)")
	fl.BoolVar(&f.overrideLock, "override-lock", false, "ignore an existing document lock")
	fl.IntVar(&f.depth, "depth", 0, "relationship expansion depth of the echoed document")
	fl.StringSliceVar(&f.selectF, "select", nil, "return only these fields")
	fl.StringVar(&f.locale, "locale", "", "write into this locale")
	fl.BoolVar(&f.noEchoCheck, "no-echo-check", false, "skip the §10.2 echo-diff")
	fl.IntVar(&f.maxDocs, "max-docs", 0, "blast-radius cap for the bulk form (default defaults.max_bulk = 100)")
	fl.BoolVar(&f.all, "all", false, "lift the --max-docs cap")
	fl.BoolVar(&f.perDoc, "per-doc", false, "issue one PATCH per document instead of one bulk call")
	fl.BoolVar(&f.failFast, "fail-fast", false, "stop at the first per-document failure")
	fl.IntVar(&f.limit, "limit", 0, "not valid on a bulk write — use --max-docs")
	fl.BoolVar(&f.resetNested, "reset-nested", false,
		"remove nested block rows the write would leave behind (orphans), keeping the document id; with no body it repairs the stored row")

	SetHelp(cmd, updateHelp())
	return cmd
}

// updateHelp is §10.5's model for `pay update`.
func updateHelp() *Help {
	return &Help{
		Synopsis: []string{
			"pay update <collection> <id>  [--set k=v ...] [--set-json k=JSON ...] [--set-md k=@file.md ...] [--publish|--unpublish|--draft] [--reset-nested]",
			"pay update <collection> <id> --reset-nested [--dry-run]     # repair {} rows (orphans), same id",
			"pay update <collection> --where 'PATH OP VALUE' [--max-docs N|--all] --set k=v --yes",
		},
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
			"set":      {Grammar: "key=value  (dotted keys nest: meta.title=…)", Repeatable: true},
			"set-json": {Grammar: "key=JSON", Repeatable: true},
			"set-md":   {Grammar: setMDGrammar, Repeatable: true, Note: setMDBodyNote},
			"depth":    {Min: intPtr(0), Max: intPtr(10)},
			"max-docs": {Min: intPtr(1), Note: "blast-radius cap; default 100, --all lifts it"},
			"limit":    {Note: "NOT valid on a bulk write — it is page size; use --max-docs"},
			"per-doc":  {Note: "one PATCH per id; slower, but a failure names the exact document"},
			"reset-nested": {Note: "one document only; carrier phases so Payload's Postgres adapter clears nested block tables " +
				"the write would skip; sends 2+ requests (see meta.reset_nested)"},
		},
		Output: OutputSpec{
			Kind:     output.KindDoc,
			Skeleton: `{"id":16,"title":"New title",…}  · bulk: data_kind bulk_result {"succeeded":[…],"failed":[…],"not_attempted":[…]}`,
		},
		ExitCodes: BulkExitCodes,
		Examples: []Example{
			{Why: "the safe first move on any write: see the exact request",
				Cmd: "pay update pages 16 --set title='Launch day' --dry-run"},
			{Why: "one field of one document",
				Cmd: "pay update pages 16 --set title='Launch day'"},
			{Why: "nested JSON rather than a dotted key",
				Cmd: "pay update posts 1 --set-json meta='{\"title\":\"SEO title\"}'"},
			{Why: "replace a richText field with a Markdown file's content, as a draft",
				Cmd: "pay update posts 3 --set-md content=@body.md --draft --dry-run"},
			{Why: "store the change without publishing it (--draft alone changes nothing: exit 5)",
				Cmd: "pay update pages 16 --set title='Launch day' --draft"},
			{Why: "store and publish in one call",
				Cmd: "pay update pages 16 --publish"},
			{Why: "preview a bulk change — would_affect and sample_ids are the blast radius",
				Cmd: "pay update crm-contacts --where 'lifecycleStage eq lead' --set status=customer --dry-run"},
			{Why: "then run it, capped; --yes is required in a non-TTY (else exit 11)",
				Cmd: "pay update crm-contacts --where 'lifecycleStage eq lead' --set status=customer --max-docs 100 --yes"},
			{Why: "one PATCH per document, so a failure names the exact id",
				Cmd: "pay update crm-contacts --where 'lifecycleStage eq lead' --set status=customer --all --per-doc --yes"},
			{Why: "a nested block restructure (bento → grid inside a group): data.diff + orphan_risk first, then the id-preserving write",
				Cmd: "pay update pages 16 --data-file ./layout.json --reset-nested --dry-run"},
			{Why: "remove {} rows an earlier restructure left in the stored row (the same id; nothing else changes)",
				Cmd: "pay update pages 16 --reset-nested"},
		},
		Mistakes: []Mistake{
			{Wrong: "Auto-retrying exit 7 (partial_failure).",
				Right: "NEVER. Payload's bulk write is not transactional over REST: some documents are already written. Re-run only the failed ids — `next.cmd` gives you that command verbatim."},
			{Wrong: "Inventing values for fields a 400 named but you never sent.",
				Right: "Payload re-validates the whole stored document. Check error.fields[].sent: all-false means the stored document was already invalid. Repair it deliberately, or write with --draft."},
			{Wrong: "`--limit 50` to cap a bulk update.",
				Right: "--limit is page size and is an error here. Use --max-docs 50 — and when more documents match than the cap allows you get bulk_limit_exceeded (exit 5), not a truncated write."},
			{Wrong: "Running a bulk write without --dry-run first.",
				Right: "--dry-run prints would_affect, sample_ids and the exact request. The blast radius of the real run is identical, because PayCLI resolves ids client-side."},
			{Wrong: "`pay update <coll> <id> --draft` on its own, expecting it to \"save a draft\".",
				Right: "An update with no field to change is rejected (exit 5, bad_request_body). --draft is a modifier: pass it alongside --set/--data."},
			{Wrong: "`--set-md layout.2.content=@x.md` for rich text inside a block.",
				Right: "A write body replaces the whole layout array, so a row cannot be addressed here (invalid_args). Edit the row in a pipe: `pay get … | pay blocks set <selector> --set-md content=@x.md --field layout | pay apply`."},
			{Wrong: "Writing back a document read with --richtext md.",
				Right: "Its rich text is Markdown strings; Payload would store them as-is. --data refuses it (richtext_rendered, exit 5). Re-read without --richtext, or use --set-md."},
			{Wrong: "Assuming a 200 means every field you sent was stored.",
				Right: "Read warnings[]: input_silently_dropped means Payload discarded part of your write and still answered success."},
			{Wrong: "Changing a NESTED block's type (bento → grid inside a group) with a plain write, or fixing the {} rows by deleting and re-creating the page.",
				Right: "Payload's Postgres adapter leaves the old nested rows behind (orphan_risk / orphan_rows warn; `pay get` shows {} rows). Add --reset-nested: same id, no delete."},
			{Wrong: "Exporting a published document, editing it and writing it back with --draft, expecting a draft.",
				Right: "A body with _status \"published\" publishes even with --draft in Payload. PayCLI removes _status from the body when --draft is given and warns body_publishes; --draft with --publish is refused."},
			{Wrong: "A plain update (no --draft/--publish) of a published page that has a pending draft, or writing a `pay get --draft` export (_status \"draft\") back without --draft.",
				Right: "Payload would store the draft as the live row with _status \"draft\" — the page goes offline. PayCLI refuses it (invalid_args): pass --draft (edit the draft), --publish (publish it with the change) or --unpublish (deliberately)."},
		},
		SeeAlso: []string{
			"pay describe <collection> --field <path>   # what this field accepts",
			"pay count <collection> --where …   # size the filter first",
			"pay publish <collection> <id>",
			"pay versions list <collection> --id <id>   # what it looked like before",
		},
	}
}

func runUpdate(ctx context.Context, cmd *cobra.Command, d *Deps, f *updateFlags, slug, id string) (*output.Envelope, error) {
	// The blast-radius cap is validated before anything is resolved or sent:
	// an explicit `--max-docs 0` is an error on the scoped form too, where it
	// used to be dropped in silence.
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
	if id != "" {
		// Canonical before anything reads or writes it (see canonicalID).
		canon, e := d.canonicalID(t, t.Slug, id)
		if e != nil {
			return nil, e
		}
		id = canon
	}
	if err := checkStatusFlags(f.draft, f.publish, f.unpublish); err != nil {
		return nil, err
	}
	if f.draft || f.publish || f.unpublish {
		if err := checkDrafts(t.Slug, t.flags().Drafts); err != nil {
			return nil, err
		}
	}
	bulk := id == ""
	if bulk && f.resetNested {
		return nil, apierr.New(apierr.CodeInvalidArgs,
			"--reset-nested works on one document: it reads it, plans the carrier phases for it and verifies it").
			WithHint("run it per document: pay update %s <id> … --reset-nested --dry-run", t.Slug)
	}

	body, warnings, err := f.data.build(d, t, t.Shard)
	if err != nil {
		return nil, err
	}
	if warn := applyBodyStatus(body, f.draft, f.publish || f.unpublish, t.flags().Drafts); warn != nil {
		warnings = append(warnings, *warn)
	}
	if f.publish {
		body["_status"] = "published"
	}
	if f.unpublish {
		body["_status"] = "draft"
	}
	if len(body) == 0 && !f.resetNested {
		return nil, apierr.New(apierr.CodeBadRequestBody, "update needs at least one field to change").
			WithHint("pass --set k=v, --data '{...}', --publish or --unpublish")
	}

	selector := safety.SelectorID
	if bulk {
		selector = safety.SelectorBulk
	}
	if err := safety.CheckLimitFlag(safety.CmdUpdate, selector, cmd.Flags().Changed("limit")); err != nil {
		return nil, err
	}

	// §7.9a / conflict 35. A write is also a read: Payload echoes the document
	// back, and `data` is what the agent then believes it stored. Building the
	// locale by hand here sent `locale=de` with NO fallback-locale, so Payload
	// applied its default fallback:true and every untranslated field came back
	// in the DEFAULT locale — byte-identical to a real translation. That is the
	// read-modify-write loop that copies English into `de`. --locale therefore
	// goes through the same resolver the read commands use, which also picks up
	// the profile's `locale` (previously ignored by every write) and validates
	// the code against discovery before the PATCH leaves.
	p := query.Params{Select: f.selectF, Depth: query.IntPtr(f.depth)}
	locWarn, err := applyWriteLocale(d, t, f.locale, &p)
	if err != nil {
		return nil, err
	}
	if locWarn != nil {
		warnings = append(warnings, *locWarn)
	}
	if f.draft {
		p.Draft = query.BoolPtr(true)
	}
	// --trash comes from the shared filter flag set: it is the same
	// "operate on soft-deleted documents" switch the read commands use.
	if f.filter.trash {
		p.Trash = query.BoolPtr(true)
	}
	extra := url.Values{}
	if f.autosave {
		extra.Set("autosave", "true")
	}
	if f.overrideLock {
		extra.Set("overrideLock", "true")
	}
	if len(extra) > 0 {
		p.Extra = extra
	}
	if err := query.ValidateSelect(f.selectF, t.schema(cfg)); err != nil {
		return nil, err
	}
	// F6: {"$ref": …} objects become ids before anything is previewed or
	// sent; §9.15.2: masked values from an export are handled first.
	sc := writeScope{shard: t.Shard}
	if !bulk {
		sc.preimage = func(ctx context.Context) (map[string]any, error) {
			doc, err := d.readOrphanTarget(ctx, client, orphanTarget{t: t, id: id}, p, true)
			return map[string]any(doc), err
		}
	}
	body, prepWarns, err := d.prepareWriteBody(ctx, sc, body, p.Locale)
	if err != nil {
		return nil, err
	}
	warnings = append(warnings, prepWarns...)
	// --set-md into a field the schema cannot type: checked against the
	// stored value (the document itself; for --where, other documents).
	mdWarns, err := f.data.guardMDTargets(ctx, d, client, orphanTarget{t: t, id: id}, p)
	if err != nil {
		return nil, err
	}
	warnings = append(warnings, mdWarns...)

	if bulk {
		return runBulkUpdate(ctx, cmd, d, client, f, t, body, p, warnings)
	}
	return runUpdateOne(ctx, d, client, safety.CmdUpdate, f, t, id, body, p, warnings)
}

// runUpdateOne is the single-document PATCH, shared by `pay update <id>` and
// `pay apply`.
//
// command is a parameter rather than a constant because the two verbs must be
// distinguishable everywhere the operation is recorded — the envelope, the
// audit log, the dry-run preview — while being IDENTICAL in every safety
// decision. `pay apply` is `pay update` with a body assembled from a pipe, and
// giving it its own copy of this function is how the two would drift apart on
// confirmation or echo-checking.
func runUpdateOne(ctx context.Context, d *Deps, client *payload.Client, command string, f *updateFlags,
	t *collTarget, id string, body map[string]any, p query.Params, warnings []output.Warning) (*output.Envelope, error) {
	cfg := d.cfg()
	id, e := d.canonicalID(t, t.Slug, id)
	if e != nil {
		return nil, e
	}
	op := safety.Op{Command: command, Selector: safety.SelectorID}
	newOp := func() *writeOp {
		return d.newWriteOp(op, t.Slug, "PATCH", "/"+t.Slug+"/"+id).withIDs([]any{id}).
			withBackup(ctx, BackupTarget{Collection: t, Params: p})
	}
	w := newOp()
	o := orphanTarget{t: t, id: id}
	fixCmd := fmt.Sprintf("re-run this command with --reset-nested added (`pay %s %s %s … --reset-nested --dry-run` shows the plan)",
		command, t.Slug, shellQuote(id))
	switch command {
	case safety.CmdApply:
		fixCmd = "end the same pipeline in `pay apply --reset-nested --dry-run` to see the plan, then without --dry-run"
	case safety.CmdUpsert, safety.CmdSync:
		// Neither verb has --reset-nested: the repair is an update by id.
		fixCmd = fmt.Sprintf("after the write, `pay update %s %s --reset-nested --dry-run` plans the id-preserving repair",
			t.Slug, shellQuote(id))
	}
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

	if cfg.DryRun {
		// F7 stage B: the preview shows what the write changes (data.diff),
		// read from the newest version with Payload's PATCH semantics, and
		// the §10.2.1 orphan prediction the same reads allow.
		diff, pre, dw := d.dryRunDiff(ctx, client, o, body, p, diffBaseLatest, fixCmd)
		warnings = append(warnings, dw...)
		if diff != nil {
			w.dryDiff = diff
		}
		if pre != nil {
			// §9.7: the document as it is now outranks an older census.
			warnings = refineBlockWarnings(d, t, t.Shard, body, pre.latest, warnings)
			// §9.12.8.3: a string over an editor state, an object over an array.
			if err := d.checkStoredShapes(ctx, client, o, p, body, pre.latest); err != nil {
				return nil, err
			}
		}
		// §10.2.3: the preview refuses what the write would refuse.
		if pre != nil {
			pw, err := d.checkPublication(ctx, client, o, body, p, f.unpublish, pre)
			if err != nil {
				return nil, err
			}
			if pw != nil {
				warnings = append(warnings, *pw)
			}
		}
		sent, reqParams, affected, ids := body, p, 1, []any{id}
		if f.resetNested {
			plan, pw, err := d.planReset(ctx, client, spec, pre)
			if err != nil {
				return nil, err
			}
			warnings = append(warnings, pw...)
			w.dryReset = plan.report(true)
			switch {
			case plan.repair && !plan.skip:
				// A repair's request is the restatement, sent without
				// validation (see reset_nested.go).
				sent, reqParams = plan.restatement(plan.base), unvalidated(p, true)
			case plan.repair:
				// Nothing to repair: the real run sends nothing.
				affected = 0
				warnings = append(warnings, repairNotNeededWarning(o, true))
			}
		}
		q, _ := reqParams.Encode()
		env, err := emitDryRun(d, w, command, "PATCH",
			client.URLFor(&payload.Request{Method: "PATCH", Path: "/" + t.Slug + "/" + id, Query: q}),
			sent, affected, ids[:affected], warnings...)
		if err != nil {
			return nil, err
		}
		// The preview discloses the same locale facts the real write will send,
		// so --dry-run answers "which locale, and what fallback" too.
		env.Meta.Locale = localeMetaOf(p)
		return env, nil
	}
	// §9.12.8.3: refused before the prompt; one read when the body has block
	// rows or an index-keyed object.
	if err := d.checkStoredShapes(ctx, client, o, p, body, nil); err != nil {
		return nil, err
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
		if plan.skip && plan.repair {
			return resetNothingToDo(command, o, plan, withLocaleMeta(w.run.meta(), p), warnings), nil
		}
	}
	// §10.2.3: never unpublish by accident. The remedy already read the
	// pre-image; otherwise two select=_status reads.
	var pre *preimage
	if plan != nil {
		pre = plan.pre
	}
	if pw, err := d.checkPublication(ctx, client, o, body, p, f.unpublish, pre); err != nil {
		return nil, err
	} else if pw != nil {
		warnings = append(warnings, *pw)
	}

	var (
		res *payload.WriteResult
		rep *resetReport
		err error
	)
	if plan != nil && !plan.skip {
		var rw []output.Warning
		res, rep, rw, err = d.execReset(ctx, spec, plan, w)
		warnings = append(warnings, rw...)
		if err != nil {
			return nil, err
		}
		if plan.repair {
			body = plan.restatement(plan.base)
		}
	} else {
		if err := w.pre(1); err != nil {
			return nil, err
		}
		warnings = append(warnings, d.riskFromBackup(o, body, p, fixCmd)...)
		warnings = refineBlockWarnings(d, t, t.Shard, body, d.backedUpDoc("collection", t.Slug, id, p.Locale), warnings)
		res, err = client.Update(ctx, t.Slug, id, body, p,
			payload.WithClassify(classifyFor(t, cfg, payload.SentPaths(body))), payload.WithKnownRoute())
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
	for _, warn := range echoWarnings(t.Slug, body, res.Doc, f.selectF, echoConfig{
		Shard:     t.Shard,
		Ignore:    cfg.EchoCheckIgnore,
		LocaleAll: p.Locale == "all",
		Disabled:  f.noEchoCheck,
	}) {
		env.AddWarning(warn)
	}
	if rep != nil {
		for _, warn := range d.finishReset(ctx, client, spec, body, res, rep) {
			env.AddWarning(warn)
		}
		env.Meta.ResetNested = rep
	} else {
		if plan != nil {
			env.Meta.ResetNested = plan.report(false)
		}
		warns, _ := d.orphanCheckAfterWrite(ctx, client, o, body, res.Doc, p, f.noEchoCheck, false)
		for _, warn := range warns {
			env.AddWarning(warn)
		}
	}
	d.foldIntoCensus(discovery.EntityKindCollection, t.Slug, p, res.Doc)
	return w.finish(env, res.HTTP, 1, 0, "")
}

func runBulkUpdate(ctx context.Context, cmd *cobra.Command, d *Deps, client *payload.Client,
	f *updateFlags, t *collTarget, body map[string]any, p query.Params, warnings []output.Warning) (*output.Envelope, error) {
	cfg := d.cfg()
	built, err := f.filter.build(nil, d, t)
	if err != nil {
		return nil, err
	}
	warnings = appendNewWarnings(warnings, built.Warnings...)
	where := built.Params.Where
	if where.IsEmpty() {
		return nil, apierr.New(apierr.CodeWhereRequired,
			"`pay update <collection>` without an id needs --where").
			WithHint("pass --where 'field op value', or name a document: pay update %s <id> --set …", t.Slug)
	}

	maxDocs := safety.ResolveMaxDocs(f.maxDocs, cmd.Flags().Changed("max-docs"), cfg.MaxBulk, f.all)
	op := safety.Op{Command: safety.CmdUpdate, Selector: safety.SelectorBulk}
	// §12.8: the ids §12.3 resolves below are the ones backed up (withIDs).
	w := d.newWriteOp(op, t.Slug, "PATCH", "/"+t.Slug).withWhere(where).
		withBackup(ctx, BackupTarget{Collection: t, Params: p})
	classify := payload.WithClassify(classifyFor(t, cfg, payload.SentPaths(body)))

	// §12.3 phases 1-3: count, cap, resolve ids. Never pass --where through.
	//
	// All three phases share ONE scope. Counting live documents and then
	// writing with trash=true (or the reverse) makes the cap and the blast
	// radius describe different populations: `--trash` used to reach only the
	// PATCH, so a bulk update aimed at soft-deleted documents resolved zero
	// ids and reported success having changed nothing.
	scope := bulkScope(where, p)
	n, _, err := client.Count(ctx, t.Slug, scope, classify, payload.WithKnownRoute())
	if err != nil {
		return nil, err
	}
	plan := safety.Plan{Command: safety.CmdUpdate, Collection: t.Slug, Matched: n, MaxDocs: maxDocs, All: f.all}
	if err := plan.Check(); err != nil {
		return nil, err
	}
	ids, err := client.ResolveIDsScoped(ctx, t.Slug, scope, n, classify, payload.WithKnownRoute())
	if err != nil {
		return nil, err
	}
	gap := resolveGapWarning(n, len(ids))
	if gap != nil {
		warnings = append(warnings, *gap)
	}

	if cfg.DryRun {
		q, _ := scope.Encode()
		// F7 stage B: the sampled documents' change lists.
		diff, dw := d.sampleDiffs(ctx, client, t, ids, body, p, diffBaseLatest, true)
		if diff != nil {
			w.dryDiff = diff
		}
		warnings = append(warnings, dw...)
		// gap is already in warnings; emitDryRun dedupes, so the preview
		// carries the blockType and write-shape warnings too instead of the
		// resolve gap alone.
		env, err := emitDryRun(d, w.withIDs(ids), safety.CmdUpdate, "PATCH",
			client.URLFor(&payload.Request{Method: "PATCH", Path: "/" + t.Slug, Query: q}),
			body, len(ids), ids, warnings...)
		if err != nil {
			return nil, err
		}
		if gap != nil {
			env.Warnings = appendNewWarnings(env.Warnings, *gap)
		}
		env.Meta.Locale = localeMetaOf(p)
		return env, nil
	}
	if err := w.confirm(len(ids)); err != nil {
		return nil, err
	}
	if err := w.withIDs(ids).pre(len(ids)); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		env := output.New(safety.CmdUpdate, output.KindBulkResult,
			bulkData{Succeeded: []payload.Doc{}, Failed: []map[string]any{}, NotAttempted: []any{}}).
			WithTarget(t.envTarget(nil)).
			WithChanged(&output.Changed{IDs: []any{}}).
			WithMeta(withLocaleMeta(w.run.meta(), p))
		for _, warn := range warnings {
			env.AddWarning(warn)
		}
		return w.finish(env, nil, 0, 0, "")
	}

	var (
		res          *payload.BulkResult
		notAttempted []any
		callErr      error
	)
	if f.perDoc {
		res, notAttempted, callErr = perDocUpdate(ctx, client, t, ids, body, p, f.failFast, classify)
	} else {
		res, callErr = client.UpdateByIDs(ctx, t.Slug, ids, body, p, classify, payload.WithKnownRoute())
	}
	if res == nil {
		if callErr != nil {
			w.post(false, 0, 0, len(ids), callErr.Error(), "")
			return nil, callErr
		}
		res = &payload.BulkResult{}
	}

	retry := fmt.Sprintf("pay update %s --where 'id in %s' %s%s", t.Slug, joinIDs(failedIDsOf(res)), rerunFlags(f), d.profileFlag())
	env := bulkEnvelope(safety.CmdUpdate, "Updated", t, res, ids, notAttempted, retry).
		WithMeta(withLocaleMeta(w.run.meta(), p)).
		WithRawBody(res.Raw, !cfg.Redact)
	for _, warn := range warnings {
		env.AddWarning(warn)
	}
	// The echo-diff runs against the first committed document: the same body
	// was sent to every id, so a dropped field is dropped for all of them and
	// N copies of one warning would only bury it.
	if len(res.Docs) > 0 {
		for _, warn := range echoWarnings(t.Slug, body, res.Docs[0], f.selectF, echoConfig{
			Shard:     t.Shard,
			Ignore:    cfg.EchoCheckIgnore,
			LocaleAll: p.Locale == "all",
			Disabled:  f.noEchoCheck,
		}) {
			env.AddWarning(warn)
		}
	}
	errMsg := ""
	if callErr != nil {
		errMsg = callErr.Error()
	}
	return w.finish(env, res.HTTP, len(res.Docs), len(res.Errors)+len(notAttempted), errMsg)
}

// perDocUpdate is §12.5's --per-doc antidote: one bad document no longer
// poisons its innocent batch-mates with an opaque "Something went wrong."
func perDocUpdate(ctx context.Context, client *payload.Client, t *collTarget, ids []any,
	body map[string]any, p query.Params, failFast bool, opts ...payload.Option) (*payload.BulkResult, []any, error) {
	out := &payload.BulkResult{}
	for i, id := range ids {
		if ctx.Err() != nil {
			return out, ids[i:], ctx.Err()
		}
		res, err := client.Update(ctx, t.Slug, fmt.Sprintf("%v", id), body, p, opts...)
		if err != nil {
			out.Errors = append(out.Errors, payload.BulkFailure{ID: id, Message: err.Error(), Code: apierr.CodeOf(err)})
			// Latch the response that belongs to the FIRST failure. Overwriting
			// out.HTTP on every success meant error.http pointed at the last
			// document that worked, so the diagnostic an agent is told to read
			// described a different, successful request.
			if out.HTTP == nil {
				out.HTTP = httpOf(res)
			}
			if failFast {
				return out, ids[i+1:], nil
			}
			continue
		}
		out.Docs = append(out.Docs, res.Doc)
		if len(out.Errors) == 0 {
			out.HTTP = res.HTTP
		}
	}
	return out, nil, nil
}

func failedIDsOf(res *payload.BulkResult) []any {
	out := make([]any, 0, len(res.Errors))
	for _, e := range res.Errors {
		out = append(out, e.ID)
	}
	return out
}

func joinIDs(ids []any) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("%v", id))
	}
	return strings.Join(parts, ",")
}

// rerunFlags renders the data flags of the failed subset so next.cmd is
// literally runnable. Only --set is reproduced: a --data body may be large and
// is already in the agent's own history.
func rerunFlags(f *updateFlags) string {
	parts := make([]string, 0, len(f.data.set))
	for _, s := range f.data.set {
		parts = append(parts, "--set "+quoteArg(s))
	}
	if f.publish {
		parts = append(parts, "--publish")
	}
	if f.unpublish {
		parts = append(parts, "--unpublish")
	}
	return strings.Join(parts, " ")
}

// quoteArg renders one argument for a literally runnable next.cmd.
//
// It delegates to shellQuote's character ALLOWLIST rather than testing for a
// space, tab or quote: a retry string like --set 'slug=a*b' or a value holding
// $VAR, ;, & or a backtick contains none of those three characters and used to
// be emitted bare, where the agent's shell would then expand or split it.
func quoteArg(s string) string {
	return shellQuote(s)
}

// httpOf returns the response carried by a WriteResult, tolerating a nil result
// (a transport-level failure produces an error and no result at all).
func httpOf(res *payload.WriteResult) *payload.Response {
	if res == nil {
		return nil
	}
	return res.HTTP
}
