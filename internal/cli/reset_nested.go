package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/docdiff"
	"github.com/KLIXPERT-io/pay-cli/internal/fsatomic"
	"github.com/KLIXPERT-io/pay-cli/internal/orphans"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
)

// ---------------------------------------------------------------------------
// §10.2.1 --reset-nested: remove orphan rows and keep the document id
// ---------------------------------------------------------------------------
//
// The adapter clears a nested block table only when the written document
// reaches a list that accepts the table's type (internal/orphans). So the
// remedy writes CARRIERS: copies of the old ancestor rows, appended to the
// top-level list, with their nested block lists emptied — the adapter walks
// them, deletes every row of the tables their lists accept (WHERE _parentID =
// the document id, at every path), and inserts nothing below them. A later
// request without the carriers then removes the carriers themselves (a
// top-level table is always cleared).
//
// Carriers are not valid content: a nested blocks field that requires rows
// (klixpert's bento.blocks, "Kacheln") rejects an emptied carrier with a 400
// — verified on stage. The phases are therefore sent with
// `?unpublishAllLocales=true`, the one REST switch with which Payload 3.87
// skips validation on a NON-draft write
// (collections/operations/utilities/update.js l.154-157; globals l.153). On a
// project without per-locale status (versions.drafts.localizeStatus) it
// changes nothing else about the write: the stored row is written, `_status`
// keeps its merged value, and the version bookkeeping updates the NEWEST
// VERSION IN PLACE (saveVersion → updateLatestVersion) instead of adding one.
// Verified on localhost:3900: a published page stayed published, the version
// count stayed at 6, and a body with an empty required `layout` was accepted.
// A document whose `_status` is a per-locale map is refused.
//
// So that no unvalidated content is ever left behind, the sequence is:
//
//  1. the requested write, sent exactly as it would be without the flag
//     (validated; a rejection here writes nothing);
//  2. the carrier phases, each restating the top-level lists as stored by 1
//     plus the carriers;
//  3. a restatement of those lists without carriers — content that passed
//     validation in 1 — sent the same way, which also leaves the newest
//     version exactly as 1 made it (no carrier ever survives in the history).
//
// A repair (no body) skips 1: it restates what is stored.

const unpublishAllLocalesParam = "unpublishAllLocales"

// resetSpec describes the write --reset-nested wraps.
type resetSpec struct {
	o       orphanTarget
	command string
	// body is the requested body; empty means repair mode.
	body   map[string]any
	params query.Params
	// op builds the writeOp one request of the sequence is audited under.
	op func(step string) *writeOp
	// send performs one request.
	send func(ctx context.Context, body map[string]any, p query.Params) (*payload.WriteResult, error)
}

// resetPlan is what --reset-nested will do.
type resetPlan struct {
	analysis *orphans.Analysis
	pre      *preimage
	// lists are the top-level lists the carriers go into, sorted.
	lists []string
	// base is each list's intended content before the requested write runs
	// (the body's value, else the newest version's).
	base    map[string][]any
	repair  bool
	history int
	// skip: nothing needs a carrier; the write runs as an ordinary one.
	skip bool
}

// resetReport is meta.reset_nested (and, on a dry run, data.reset_nested).
type resetReport struct {
	Mode            string           `json:"mode"`
	Phases          int              `json:"phases"`
	Writes          int              `json:"writes"`
	Cleared         []string         `json:"cleared_block_types"`
	Carriers        []resetCarrier   `json:"carriers"`
	AtRisk          []orphans.Risk   `json:"at_risk"`
	EmptyBefore     []string         `json:"empty_rows_before"`
	Unresolved      []string         `json:"unresolved,omitempty"`
	HistoryVersions int              `json:"history_versions"`
	Steps           []string         `json:"steps"`
	Verified        *bool            `json:"verified,omitempty"`
	RemainingEmpty  []string         `json:"remaining_empty_rows,omitempty"`
	FailedPhase     int              `json:"failed_phase,omitempty"`
	PhaseBodies     []map[string]any `json:"phase_bodies,omitempty"`
}

type resetCarrier struct {
	Phase  int      `json:"phase"`
	List   string   `json:"list"`
	Chain  []string `json:"chain"`
	Clears string   `json:"clears"`
}

func (p *resetPlan) report(dry bool) *resetReport {
	r := &resetReport{Mode: "restructure", Cleared: []string{}, Carriers: []resetCarrier{}, AtRisk: []orphans.Risk{},
		EmptyBefore: []string{}, HistoryVersions: p.history, Steps: []string{}}
	switch {
	case p.skip:
		r.Mode = "not_needed"
	case p.repair:
		r.Mode = "repair"
	}
	if a := p.analysis; a != nil {
		r.Cleared = orEmptyStrings(a.ClearedTypes())
		if a.Risks != nil {
			r.AtRisk = a.Risks
		}
		if a.Empty != nil {
			r.EmptyBefore = a.Empty
		}
		r.Unresolved = a.Unresolved
		for i, ph := range a.Phases {
			for _, c := range ph {
				r.Carriers = append(r.Carriers, resetCarrier{Phase: i + 1, List: c.List, Chain: c.Chain, Clears: c.Clears})
			}
		}
		if !p.skip {
			r.Phases = len(a.Phases)
		}
	}
	switch {
	case p.skip && p.repair:
		r.Writes = 0
	case p.skip:
		r.Writes = 1
		r.Steps = append(r.Steps, "requested write")
	default:
		if !p.repair {
			r.Steps = append(r.Steps, "requested write (validated)")
		}
		for i := 0; i < r.Phases; i++ {
			r.Steps = append(r.Steps, fmt.Sprintf("carrier phase %d/%d (unpublishAllLocales=true: no validation)", i+1, r.Phases))
		}
		r.Steps = append(r.Steps, "restate the lists without carriers (unpublishAllLocales=true)")
		r.Writes = len(r.Steps)
	}
	if dry && !p.skip {
		r.PhaseBodies = p.phaseBodies(p.base)
	}
	return r
}

// phaseBodies builds each phase's body: every carrier list restated (lists)
// with that phase's carriers appended.
func (p *resetPlan) phaseBodies(lists map[string][]any) []map[string]any {
	if p.analysis == nil {
		return nil
	}
	out := make([]map[string]any, 0, len(p.analysis.Phases))
	for _, ph := range p.analysis.Phases {
		body := map[string]any{}
		for _, list := range p.lists {
			rows := cloneList(lists[list])
			for _, c := range ph {
				if c.List == list {
					rows = append(rows, orphans.CloneDoc(c.Row))
				}
			}
			orphans.SetAt(body, list, rows)
		}
		out = append(out, body)
	}
	return out
}

// restatement is the last request: the carrier lists as the document holds
// them, without carriers.
func (p *resetPlan) restatement(lists map[string][]any) map[string]any {
	body := map[string]any{}
	for _, list := range p.lists {
		orphans.SetAt(body, list, cloneList(lists[list]))
	}
	return body
}

func cloneList(l []any) []any {
	if l == nil {
		return []any{}
	}
	out, _ := orphans.CloneDoc(map[string]any{"l": l})["l"].([]any)
	return out
}

// listValue reads a top-level list (through named groups) out of a document.
func listValue(doc map[string]any, path string) ([]any, bool) {
	if doc == nil {
		return nil, false
	}
	v, ok := orphans.Lookup(doc, path)
	if !ok {
		return nil, false
	}
	l, ok := v.([]any)
	return l, ok
}

// planReset reads what the remedy needs and plans it. pre may carry the
// reads a dry run already made; nil reads them.
func (d *Deps) planReset(ctx context.Context, client *payload.Client, s resetSpec, pre *preimage) (*resetPlan, []output.Warning, error) {
	plan := &resetPlan{repair: len(s.body) == 0}
	if s.params.Extra.Get("autosave") == "true" {
		return nil, nil, apierr.New(apierr.CodeInvalidArgs,
			"--reset-nested does not combine with --autosave: an autosave rewrites the newest version, not the stored row").
			WithHint("save the draft without --reset-nested, then publish with it: %s", publishResetCmd(s.o))
	}
	if isPlainDraftSave(s.params, s.body) {
		if plan.repair {
			return nil, nil, apierr.New(apierr.CodeInvalidArgs,
				"--reset-nested with --draft and no body has nothing to repair: a draft save never touches the stored row").
				WithHint("drop --draft to repair the stored row: %s", s.o.resetCmd(" --dry-run"))
		}
		plan.skip = true
		return plan, []output.Warning{{
			Code: WarnResetNestedNotNeeded,
			Message: "--reset-nested did nothing: a draft save only inserts a new version, which never inherits old " +
				"rows. Orphan rows live in the stored (published) row and are made by the NON-draft write that replaces it.",
			Hint: "publish this way instead: " + publishResetCmd(s.o),
		}}, nil
	}
	if pre == nil {
		loaded, err := d.loadPreimage(ctx, client, s.o, s.params)
		if err != nil {
			return nil, nil, err
		}
		pre = loaded
	}
	plan.pre = pre
	perLocale, err := d.perLocaleStatus(ctx, client, s.o, s.params, pre)
	if err != nil {
		return nil, nil, err
	}
	if perLocale {
		return nil, nil, apierr.New(apierr.CodeFeatureUnavailable,
			"%s has a per-locale _status (versions.drafts.localizeStatus); --reset-nested's validation-free phases would unpublish every locale",
			s.o.what()).
			WithHint("rebuild the document instead: `pay backups restore <file> --recreate` (a NEW id), or restructure the nested blocks in the admin UI")
	}
	if plan.repair {
		if err := refusePendingDraft(s.o, pre); err != nil {
			return nil, nil, err
		}
	}

	var history []map[string]any
	if len(orphans.EmptyRows(pre.stored)) > 0 && s.o.versioned() {
		history = d.readHistory(ctx, client, s.o)
		plan.history = len(history)
	}
	next := docdiff.ApplyPatch(map[string]any(pre.latest), s.body, docdiff.Options{})
	a := orphans.Analyze(pre.stored, next, history, d.censusAccepts(s.o))
	plan.analysis = a

	var warns []output.Warning
	if len(a.Unresolved) > 0 {
		warns = append(warns, output.Warning{
			Code: WarnResetNestedUnresolved,
			Message: fmt.Sprintf("%d {} row(s) could not be traced to a block type in the last %d version(s) of %s, so no "+
				"carrier can make the adapter clear them", len(a.Unresolved), resetHistoryLimit, s.o.what()),
			Paths: capPaths(a.Unresolved),
			Hint:  "`pay versions list` shows older states; the last resort is `pay backups restore <file> --recreate` (a NEW id)",
		})
	}
	if a.Incomplete {
		warns = append(warns, output.Warning{
			Code:    WarnResetNestedIncomplete,
			Message: fmt.Sprintf("the nesting needs more than %d carrier phases; the plan stops there", orphans.MaxPhases),
		})
	}
	if len(a.Phases) == 0 {
		plan.skip = true
		if !plan.repair {
			warns = append(warns, output.Warning{
				Code:    WarnResetNestedNotNeeded,
				Message: "--reset-nested found nothing this write would leave behind, so it is sent as one ordinary request",
			})
		}
		return plan, warns, nil
	}

	seen := map[string]bool{}
	for _, ph := range a.Phases {
		for _, c := range ph {
			if !seen[c.List] {
				seen[c.List] = true
				plan.lists = append(plan.lists, c.List)
			}
		}
	}
	plan.lists = sortedKeysOf(seen)
	plan.base = map[string][]any{}
	for _, list := range plan.lists {
		if l, ok := listValue(s.body, list); ok {
			plan.base[list] = l
			continue
		}
		l, _ := listValue(map[string]any(pre.latest), list)
		plan.base[list] = l
	}
	return plan, warns, nil
}

// perLocaleStatus is whether the document's _status is per locale
// (versions.drafts.localizeStatus). A read in one locale cannot tell: Payload
// flattens a localized field to the requested locale (fields/hooks/afterRead/
// promise.js shouldHoistLocalizedValue), so _status reads back as a plain
// string unless the locale is "all". The stored row is therefore read once
// more with locale=all (select _status) — unless the read already was, the
// entity has no _status at all, or discovery knows the project is not
// localized. A failed read is an error: on such a project the
// validation-free phases would unpublish every locale
// (collections/operations/utilities/update.js l.167-182).
func (d *Deps) perLocaleStatus(ctx context.Context, client *payload.Client, o orphanTarget, p query.Params, pre *preimage) (bool, error) {
	st, has := pre.stored["_status"]
	if !has {
		return false, nil
	}
	if _, ok := st.(map[string]any); ok {
		return true, nil
	}
	if d.Manifest != nil {
		if en := d.Manifest.Capabilities.Localization.Enabled; en != nil && !*en {
			return false, nil
		}
	}
	rp := query.Params{Depth: query.IntPtr(0), Locale: "all", Select: []string{"_status"}, Trash: p.Trash}
	var (
		doc payload.Doc
		err error
	)
	if o.g != nil {
		rp.Trash = nil
		doc, _, err = client.GlobalGet(ctx, o.g.Slug, rp, payload.WithKnownRoute())
	} else {
		doc, _, err = client.Get(ctx, o.t.Slug, o.id, rp,
			payload.WithClassify(classifyFor(o.t, d.cfg(), nil)), payload.WithKnownRoute())
	}
	if err != nil {
		return false, apierr.Wrap(err, apierr.CodeOf(err),
			"--reset-nested could not read the _status of %s in every locale (locale=all), which it needs to rule out a "+
				"per-locale status: %s", o.what(), errMessage(err)).
			WithHint("%s   # nothing was written", o.getCmd())
	}
	_, perLocale := doc["_status"].(map[string]any)
	return perLocale, nil
}

// publishResetCmd is the publish that removes the rows. A global has no
// `pay publish`: `pay globals update <slug> --publish` is its publish (a
// restructure with body {_status: "published"}, never a repair).
func publishResetCmd(o orphanTarget) string {
	if o.g != nil {
		return "pay globals update " + o.g.Slug + " --publish --reset-nested --dry-run"
	}
	return "pay publish " + o.t.Slug + " " + shellQuote(o.id) + " --reset-nested --dry-run"
}

// refusePendingDraft stops a repair from publishing unpublished work: the
// repair restates the NEWEST version's lists into the stored row (and Payload
// takes the newest version's _status with them), so on a published document
// with a pending draft it would publish the draft's lists and unpublish the
// row. The stored row's {} rows are not a draft (storedDiffers): they are
// what the repair removes, and they are never in a version.
func refusePendingDraft(o orphanTarget, pre *preimage) error {
	if st, _ := pre.stored["_status"].(string); st != "published" {
		return nil
	}
	if !storedDiffers(pre) {
		return nil
	}
	return apierr.New(apierr.CodeInvalidArgs,
		"%s has unpublished draft changes; a --reset-nested repair restates the newest version into the published row and would publish them",
		o.what()).WithHint("publish the draft with the repair built in: %s — or review it first with `%s`",
		publishResetCmd(o), o.draftVsPublishedCmd())
}

// readHistory reads the document's recent versions (depth 0), newest first.
// It only names the type of {} rows; without it they are reported
// unresolved, never guessed, so a failed read is not an error.
func (d *Deps) readHistory(ctx context.Context, client *payload.Client, o orphanTarget) []map[string]any {
	p := query.Params{Depth: query.IntPtr(0), Limit: query.IntPtr(resetHistoryLimit), Sort: []string{"-updatedAt"}}
	var target payload.VersionTarget
	if o.g != nil {
		target = payload.GlobalTarget(o.g.Slug)
	} else {
		target = payload.CollectionTarget(o.t.Slug)
		id, err := query.TypedID(o.id, o.t.idType())
		if err != nil {
			return nil
		}
		p.Where = query.Term("parent", query.OpEquals, id)
	}
	list, hasDocs, err := client.VersionsList(ctx, target, p, payload.WithKnownRoute())
	if err != nil || !hasDocs {
		return nil
	}
	out := make([]map[string]any, 0, len(list.Docs))
	for _, v := range list.Docs {
		if doc, ok := v["version"].(map[string]any); ok {
			out = append(out, doc)
		}
	}
	return out
}

// unvalidated is params for the validation-free requests: no draft, the
// switch, and — for the phases — a depth-0 echo nobody reads.
func unvalidated(p query.Params, echo bool) query.Params {
	out := p
	out.Draft = nil
	extra := url.Values{}
	for k, v := range p.Extra {
		if k == "autosave" {
			continue
		}
		extra[k] = append([]string(nil), v...)
	}
	extra.Set(unpublishAllLocalesParam, "true")
	out.Extra = extra
	if !echo {
		out.Select = nil
		out.Depth = query.IntPtr(0)
	}
	return out
}

// resetRestateBackoff is the wait before each retry of the restatement. The
// restatement is idempotent (it restates lists; a second copy changes
// nothing), so unlike an ordinary write it is re-sent after a transient
// failure — leaving the carriers stored is the worst outcome of the sequence.
// A variable so tests do not wait.
var resetRestateBackoff = []time.Duration{time.Second, 3 * time.Second}

// execReset runs the sequence. final is the writeOp of the last request; its
// confirm() has already run. Every request is its own audited step; the
// §12.8 backup is taken by the first one (idempotent, so the rest reuse it).
func (d *Deps) execReset(ctx context.Context, s resetSpec, plan *resetPlan, final *writeOp) (*payload.WriteResult, *resetReport, []output.Warning, error) {
	rep := plan.report(false)
	var warns []output.Warning
	lists := map[string][]any{}
	for k, v := range plan.base {
		lists[k] = v
	}
	sent := 0 // requests the server accepted so far

	// 1. The requested write, exactly as it would be sent without the flag.
	if !plan.repair {
		w := s.op("reset-nested requested write")
		if err := w.pre(1); err != nil {
			return nil, rep, warns, err
		}
		res, err := s.send(ctx, s.body, s.params)
		if err != nil {
			w.post(false, statusOf(res), 0, 1, err.Error(), "")
			return nil, rep, warns, err
		}
		w.post(true, statusOf(res), 1, 0, "", requestIDOf(res))
		sent++
		// Restate what was stored (rows keep their new ids) when the echo
		// holds it verbatim: depth 0 and no projection.
		if res != nil && res.Doc != nil && (s.params.Depth == nil || *s.params.Depth == 0) && len(s.params.Select) == 0 {
			for _, list := range plan.lists {
				if l, ok := listValue(map[string]any(res.Doc), list); ok {
					lists[list] = l
				}
			}
		}
	}

	// 2. The carrier phases.
	phases := plan.phaseBodies(lists)
	n := len(phases)
	carriers := false // a phase was written: carriers may be stored
	for i, body := range phases {
		w := s.op(fmt.Sprintf("reset-nested phase %d/%d", i+1, n))
		if err := w.pre(1); err != nil {
			if !carriers {
				return nil, rep, warns, d.resetStoppedEarly(s, err, sent)
			}
			return nil, rep, warns, d.resetCarriersLeft(s, plan, lists, err, fmt.Sprintf("the audit record of phase %d could not be written", i+1))
		}
		res, err := s.send(ctx, body, unvalidated(s.params, false))
		if err != nil {
			w.post(false, statusOf(res), 0, 1, err.Error(), "")
			rep.FailedPhase = i + 1
			// A rejection (4xx, a deterministic 500) wrote nothing; a
			// transient failure may have been applied after all.
			if i == 0 && plan.repair && !apierr.CodeOf(err).Retriable() {
				return nil, rep, warns, apierr.Wrap(err, apierr.CodeOf(err),
					"--reset-nested phase 1 of %d was rejected, so nothing was written: %s", n, errMessage(err)).
					WithHint("the last resort is `pay backups restore <file> --recreate` (a NEW id); `%s` shows the stored rows", s.o.getCmd())
			}
			warns = append(warns, output.Warning{
				Code: WarnResetNestedIncomplete,
				Message: fmt.Sprintf("--reset-nested phase %d of %d failed (%s); the lists are restated anyway so no carrier "+
					"row stays, but some orphan rows may remain", i+1, n, errMessage(err)),
				Hint: s.o.getCmd() + "   # check the stored row",
			})
			// A failed request may still have been applied (a timeout after
			// the server wrote): restate as if it had.
			carriers = true
			break
		}
		w.post(true, statusOf(res), 1, 0, "", requestIDOf(res))
		sent++
		carriers = true
	}

	// 3. The restatement, without carriers — retried, it is idempotent.
	final.step = "reset-nested restate"
	if err := final.pre(1); err != nil {
		if !carriers {
			return nil, rep, warns, d.resetStoppedEarly(s, err, sent)
		}
		return nil, rep, warns, d.resetCarriersLeft(s, plan, lists, err, "the audit record of the restatement could not be written")
	}
	body := plan.restatement(lists)
	var (
		res *payload.WriteResult
		err error
	)
	for attempt := 0; ; attempt++ {
		res, err = s.send(ctx, body, unvalidated(s.params, true))
		if err == nil || attempt >= len(resetRestateBackoff) || !apierr.CodeOf(err).Retriable() {
			break
		}
		if werr := waitCtx(ctx, resetRestateBackoff[attempt]); werr != nil {
			break
		}
	}
	if err != nil {
		final.post(false, statusOf(res), 0, 1, err.Error(), "")
		return nil, rep, warns, d.resetCarriersLeft(s, plan, lists, err, "the final restatement failed")
	}
	return res, rep, warns, nil
}

// waitCtx waits d, or until ctx is done.
func waitCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// resetStoppedEarly is a failure before any carrier was written: the
// requested write (restructure) may have been sent, nothing else was.
func (d *Deps) resetStoppedEarly(s resetSpec, err error, sent int) error {
	if sent == 0 {
		return err
	}
	return apierr.Wrap(err, apierr.CodeOf(err),
		"--reset-nested stopped after the requested write was sent (it is stored) and before any carrier phase: %s",
		errMessage(err)).
		WithHint("%s   # the stored row may hold orphan rows; `%s` plans their removal", s.o.getCmd(), s.o.resetCmd(" --dry-run"))
}

// resetCarriersLeft is a failure after carrier rows were (or may have been)
// stored: the stored row — and, because the phases ran with
// unpublishAllLocales, the newest version — end in carrier rows that are not
// valid content, so every later validated write is rejected until the lists
// are restated. The restatement body is the only copy of the lists as they
// must be (the newest version was overwritten in place), so it is saved to a
// file and the hint names the command that sends it. A plain re-run would
// not do: the repair plans from the newest version, which now holds the
// carriers and no {} rows, and reports nothing to do.
func (d *Deps) resetCarriersLeft(s resetSpec, plan *resetPlan, lists map[string][]any, err error, what string) error {
	file, saveErr := d.saveRestatement(s, plan.restatement(lists))
	msg := fmt.Sprintf("--reset-nested wrote carrier rows but %s: %s. The stored row and the newest version (updated "+
		"in place) end in carrier rows at the end of %v, which are not valid content — later validated writes are "+
		"rejected until the lists are restated. Re-running the repair does not help: it would find no {} rows", what,
		errMessage(err), plan.lists)
	var hint string
	if saveErr == nil {
		msg += "; the restatement was saved to " + file
		hint = s.o.restateCmd(file, s.params) + "   # sends the lists without the carriers (validated); do not re-run the repair itself"
	} else {
		msg += "; the restatement could not be saved (" + saveErr.Error() + ")"
		hint = s.o.getCmd() + "   # rewrite the listed lists without their trailing carrier rows"
	}
	if files := d.RT.backupsWritten(); len(files) > 0 {
		hint += fmt.Sprintf("; the pre-write backup is %s (`pay backups restore %s`)", files[0], shellQuote(files[0]))
	}
	return apierr.Wrap(err, apierr.CodeOf(err), "%s", msg).WithHint("%s", hint)
}

// saveRestatement writes the restatement body to
// <state_dir>/reset-nested/<profile>/<entity>-<time>.json (0600).
func (d *Deps) saveRestatement(s resetSpec, body map[string]any) (string, error) {
	if d.RT == nil || d.RT.Paths.StateDir == "" {
		return "", errors.New("no state directory is known")
	}
	name := s.o.slug()
	if s.o.g == nil {
		name += "-" + s.o.id
	}
	name = safeFileName(name) + "-" + d.RT.Now().UTC().Format("20060102T150405.000Z") + ".json"
	path := filepath.Join(d.RT.Paths.StateDir, "reset-nested", safeFileName(d.cfg().Profile), name)
	data, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return "", err
	}
	if err := fsatomic.Write(path, append(data, '\n'), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// restateCmd sends a saved restatement: an ordinary (validated) write of the
// lists, with the write's locale and trash scope.
func (o orphanTarget) restateCmd(file string, p query.Params) string {
	var b strings.Builder
	if o.g != nil {
		b.WriteString("pay globals update " + o.g.Slug)
	} else {
		b.WriteString("pay update " + o.t.Slug + " " + shellQuote(o.id))
	}
	b.WriteString(" --data-file " + shellQuote(file))
	if p.Locale != "" {
		b.WriteString(" --locale " + shellQuote(p.Locale))
	}
	if o.g == nil && p.Trash != nil && *p.Trash {
		b.WriteString(" --trash")
	}
	b.WriteString(" --yes")
	return b.String()
}

func errMessage(err error) string {
	if e, ok := apierr.As(err); ok {
		return e.Message
	}
	return err.Error()
}

func requestIDOf(res *payload.WriteResult) string {
	if res == nil || res.HTTP == nil {
		return ""
	}
	return res.HTTP.RequestID
}

// finishReset verifies the stored row after the sequence and completes
// meta.reset_nested. body is what the caller reports as sent (the requested
// body, or the repair's restatement).
func (d *Deps) finishReset(ctx context.Context, client *payload.Client, s resetSpec, body map[string]any,
	res *payload.WriteResult, rep *resetReport) []output.Warning {
	params := s.params
	params.Draft = nil
	warns, remaining := d.orphanCheckAfterWrite(ctx, client, s.o, body, res.Doc, params, false, true)
	ok := len(remaining) == 0 && len(warns) == 0
	rep.Verified = &ok
	rep.RemainingEmpty = remaining
	return warns
}

// resetNothingToDo answers a --reset-nested repair that found nothing to
// remove: no request is sent, and the stored row is returned as read.
func resetNothingToDo(command string, o orphanTarget, plan *resetPlan, meta output.Meta, warnings []output.Warning) *output.Envelope {
	var doc payload.Doc
	if plan.pre != nil {
		doc = plan.pre.stored
	}
	var env *output.Envelope
	if o.g != nil {
		env = output.New(command, output.KindGlobal, doc).WithTarget(o.g.envTarget())
	} else {
		env = output.New(command, output.KindDoc, doc).WithTarget(o.t.envTarget(doc.ID()))
	}
	env.WithChanged(&output.Changed{IDs: []any{}})
	meta.ResetNested = plan.report(false)
	env.WithMeta(meta)
	for _, w := range warnings {
		env.AddWarning(w)
	}
	env.AddWarning(repairNotNeededWarning(o, false))
	return env
}

// repairNotNeededWarning is a repair that has nothing to remove — the real run
// (nothing was written) and its dry run (nothing would be) say the same.
// Rows left by REMOVING a nested container sit at a path no field reads, so
// no read shows them and the repair cannot see them (§10.2.1.3): the message
// says what was checked instead of promising there is nothing.
func repairNotNeededWarning(o orphanTarget, dry bool) output.Warning {
	did := "so nothing was written"
	if dry {
		did = "so the real run sends nothing"
	}
	return output.Warning{
		Code: WarnResetNestedNotNeeded,
		Message: fmt.Sprintf("the stored row of %s has no {} rows and restating its newest version would leave no "+
			"nested rows behind, %s. Rows left by removing a nested container are invisible to every read "+
			"until a field at their old path exists again; a --dry-run of that later write predicts them (orphan_risk)",
			o.what(), did),
	}
}
