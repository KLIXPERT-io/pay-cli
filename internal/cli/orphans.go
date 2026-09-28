package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/docdiff"
	"github.com/KLIXPERT-io/pay-cli/internal/lexical"
	"github.com/KLIXPERT-io/pay-cli/internal/orphans"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

// ---------------------------------------------------------------------------
// §10.2.1 orphan rows: detect, predict, remove (F5)
// ---------------------------------------------------------------------------
//
// Payload's Postgres adapter clears a nested block type's table only when the
// new document still reaches a list that accepts that type (the model and the
// evidence are in internal/orphans). A nested restructure therefore leaves
// the old rows in the database, and they read back as `{}` rows inside
// whatever now sits at their old path. On a collection with versions the
// write's echo is the NEW VERSION, which Payload builds from the stored row
// minus its `{}` rows — so the echo is clean while the published row (what
// the site renders, what `pay get` without --draft returns) is not. Verified
// on klixpert stage (pages 71): the update echoed a clean grid, `pay get
// --draft` was clean, `pay get` showed `{}, text, {}, text, {}`.
//
// PayCLI therefore:
//
//   - warns `orphan_rows` when the echo, or the stored row read back after a
//     non-draft write, has `{}` rows, rows the body did not send, rows that
//     lost their blockType or a different blockType sequence;
//   - warns `orphan_risk` before a write when the pre-image is at hand
//     (--dry-run, or the §12.8 backup) and the model says rows will survive;
//   - warns `empty_rows` on `pay get` / `pay globals get` of such a document;
//   - offers `--reset-nested` on update/apply/globals update/publish/unpublish:
//     carrier phases that make the adapter reach — and so clear — every table
//     the requested write would skip, then the requested body. The document
//     keeps its id; no delete, no recreate.

// Warning codes of §10.2.1.
const (
	// WarnOrphanRows reports orphan symptoms observed after a write.
	WarnOrphanRows = "orphan_rows"
	// WarnOrphanRisk predicts them before a write.
	WarnOrphanRisk = "orphan_risk"
	// WarnEmptyRows reports `{}` rows in a document that was read.
	WarnEmptyRows = "empty_rows"
	// WarnOrphanCheckIncomplete says the stored row could not be read back
	// after the write, so orphan rows were neither confirmed nor ruled out.
	WarnOrphanCheckIncomplete = "orphan_check_incomplete"
	// WarnResetNestedNotNeeded says --reset-nested had nothing to do.
	WarnResetNestedNotNeeded = "reset_nested_not_needed"
	// WarnResetNestedIncomplete says a carrier phase failed after an earlier
	// one was written; the requested body was still written.
	WarnResetNestedIncomplete = "reset_nested_incomplete"
	// WarnPendingDraftSuperseded says a bulk non-draft write replaces
	// pending drafts instead of building on them (§10.2.3).
	WarnPendingDraftSuperseded = "pending_draft_superseded"
	// WarnResetNestedUnresolved names `{}` rows whose block type no version
	// of the document could name, so no carrier could clear them.
	WarnResetNestedUnresolved = "reset_nested_unresolved"
)

// resetHistoryLimit caps the versions read to name the type of `{}` rows.
const resetHistoryLimit = 50

// maxWarnPaths caps the paths one warning lists.
const maxWarnPaths = 20

// orphanTarget is the one document a check or a remedy is about.
type orphanTarget struct {
	t  *collTarget
	g  *globalTarget
	id string
}

func (o orphanTarget) slug() string {
	if o.g != nil {
		return o.g.Slug
	}
	return o.t.Slug
}

func (o orphanTarget) drafts() *bool {
	if o.g != nil {
		return o.g.flags().Drafts
	}
	return o.t.flags().Drafts
}

// versioned is whether a write's echo is a version (Payload returns the new
// version whenever versions are enabled; drafts imply versions). Unknown is
// treated as versioned: the check then costs a read instead of missing rows.
func (o orphanTarget) versioned() bool {
	var v, dr *bool
	if o.g != nil {
		f := o.g.flags()
		v, dr = f.Versions, f.Drafts
	} else {
		f := o.t.flags()
		v, dr = f.Versions, f.Drafts
	}
	if (v != nil && *v) || (dr != nil && *dr) {
		return true
	}
	return v == nil
}

// what renders the target for messages.
func (o orphanTarget) what() string {
	if o.g != nil {
		return "the global " + o.g.Slug
	}
	return o.t.Slug + " " + o.id
}

// resetCmd is the command that removes the rows (dry-run form).
func (o orphanTarget) resetCmd(suffix string) string {
	if o.g != nil {
		return "pay globals update " + o.g.Slug + " --reset-nested" + suffix
	}
	return "pay update " + o.t.Slug + " " + shellQuote(o.id) + " --reset-nested" + suffix
}

// read fetches the document at depth 0: the newest version when latest is
// set (draft=true, where drafts exist), else the stored main row.
func (d *Deps) readOrphanTarget(ctx context.Context, client *payload.Client, o orphanTarget, p query.Params, latest bool) (payload.Doc, error) {
	rp := query.Params{Depth: query.IntPtr(0), Locale: p.Locale, FallbackLocale: p.FallbackLocale, Trash: p.Trash}
	if latest {
		if dr := o.drafts(); dr == nil || *dr {
			rp.Draft = query.BoolPtr(true)
		}
	}
	if o.g != nil {
		rp.Trash = nil
		doc, _, err := client.GlobalGet(ctx, o.g.Slug, rp, payload.WithKnownRoute())
		return doc, err
	}
	doc, _, err := client.Get(ctx, o.t.Slug, o.id, rp,
		payload.WithClassify(classifyFor(o.t, d.cfg(), nil)), payload.WithKnownRoute())
	return doc, err
}

// preimage is what a dry run (or the remedy) knows about the document before
// the write: the newest version, which Payload merges the body into, and the
// stored main row, which a non-draft write overwrites.
type preimage struct {
	latest payload.Doc
	stored payload.Doc
}

// loadPreimage reads both. Without drafts they are the same row (one read).
func (d *Deps) loadPreimage(ctx context.Context, client *payload.Client, o orphanTarget, p query.Params) (*preimage, error) {
	if dr := o.drafts(); dr != nil && !*dr {
		doc, err := d.readOrphanTarget(ctx, client, o, p, false)
		if err != nil {
			return nil, err
		}
		return &preimage{latest: doc, stored: doc}, nil
	}
	latest, err := d.readOrphanTarget(ctx, client, o, p, true)
	if err != nil {
		return nil, err
	}
	stored, err := d.readOrphanTarget(ctx, client, o, p, false)
	if err != nil {
		return nil, err
	}
	return &preimage{latest: latest, stored: stored}, nil
}

// isPlainDraftSave is a write that only inserts a new version: Payload's
// update skips db.updateOne when it saves a draft, and createVersion inserts
// fresh rows, so it can neither create nor remove orphan rows. An autosave
// updates the newest version IN PLACE (updateLatestVersion) and is not one.
func isPlainDraftSave(p query.Params, body map[string]any) bool {
	if p.Draft == nil || !*p.Draft {
		return false
	}
	if st, _ := body["_status"].(string); st == "published" {
		return false
	}
	return p.Extra.Get("autosave") != "true"
}

// ---------------------------------------------------------------------------
// warnings
// ---------------------------------------------------------------------------

func capPaths(in []string) []string {
	if len(in) <= maxWarnPaths {
		return in
	}
	return append(append([]string(nil), in[:maxWarnPaths]...), fmt.Sprintf("… %d more", len(in)-maxWarnPaths))
}

// emptyRowsWarning is `pay get`'s warning for a document with `{}` rows.
func emptyRowsWarning(o orphanTarget, paths []string, draft bool) output.Warning {
	where := "the stored row — what a read without --draft returns, the published document when there is one"
	if draft {
		where = "the newest version"
	}
	return output.Warning{
		Code: WarnEmptyRows,
		Message: fmt.Sprintf("%d row(s) of %s read back as {} (%s): these are orphan rows the Postgres adapter left in a "+
			"nested block table after a restructure. The site renders them as empty blocks; a --draft read may look clean.",
			len(paths), o.what(), where),
		Paths: capPaths(paths),
		Hint: o.resetCmd(" --dry-run") + "   # plans an id-preserving repair; run it without --dry-run to remove them " +
			"(it writes the document as it is, so publish pending drafts first)",
	}
}

// orphanRowsWarning reports symptoms observed after a write.
func orphanRowsWarning(o orphanTarget, findings []orphans.Finding, where string) output.Warning {
	paths := make([]string, 0, len(findings))
	kinds := map[string]int{}
	for _, f := range findings {
		paths = append(paths, f.Path)
		kinds[f.Kind]++
	}
	parts := make([]string, 0, len(kinds))
	for _, k := range sortedKeysOf(kinds) {
		parts = append(parts, fmt.Sprintf("%d %s", kinds[k], k))
	}
	return output.Warning{
		Code: WarnOrphanRows,
		Message: fmt.Sprintf("%s holds rows this write did not send (%s): orphan rows that Payload's Postgres adapter "+
			"left in a nested block table — when this write or an EARLIER one changed or removed the blocks around "+
			"them (rows left by a removal stay invisible until a field at their path exists again, as this write may "+
			"have made it). They read back at their old path, as {} where the field there does not accept their type.",
			where, strings.Join(parts, ", ")),
		Paths: capPaths(paths),
		Hint:  o.resetCmd(" --dry-run") + "   # removes them and keeps the document id; `pay get` without --draft shows the stored rows",
	}
}

// orphanRiskWarning predicts the rows a write leaves behind. draftSave marks
// a plain draft save, which is safe itself: the rows are left behind by the
// write that later publishes it.
func orphanRiskWarning(o orphanTarget, a *orphans.Analysis, draftSave bool, fixCmd string) output.Warning {
	return riskWarning(o, a, draftSave, false, fixCmd)
}

// riskWarning words the prediction for a preview (after=false) or for a write
// that was already sent, predicted from its backup (after=true).
func riskWarning(o orphanTarget, a *orphans.Analysis, draftSave, after bool, fixCmd string) output.Warning {
	var paths, parts, causes []string
	for _, r := range a.Risks {
		paths = append(paths, r.Paths...)
		parts = append(parts, fmt.Sprintf("%d %s", len(r.Paths), r.BlockType))
		for _, c := range r.Causes {
			if !containsString(causes, c) {
				causes = append(causes, c)
			}
		}
	}
	lead := "this write leaves"
	switch {
	case draftSave:
		lead = "this draft save is safe (it only inserts a new version), but publishing it leaves"
	case after:
		lead = "the pre-write backup predicted that this write leaves"
	}
	msg := fmt.Sprintf("%s nested rows behind in %s: %s row(s) sit in block tables the Postgres adapter only clears when "+
		"the new document still has their parent block type", lead, o.what(), strings.Join(parts, ", "))
	if len(causes) > 0 {
		msg += " (" + strings.Join(causes, "; ") + ")"
	}
	msg += ". They would read back as {} rows in the published document."
	return output.Warning{Code: WarnOrphanRisk, Message: msg, Paths: capPaths(paths), Hint: fixCmd}
}

// mergeFindings dedupes findings by path.
func mergeFindings(sets ...[]orphans.Finding) []orphans.Finding {
	seen := map[string]bool{}
	var out []orphans.Finding
	for _, set := range sets {
		for _, f := range set {
			if seen[f.Path] {
				continue
			}
			seen[f.Path] = true
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func emptyFindings(doc map[string]any) []orphans.Finding {
	var out []orphans.Finding
	for _, p := range orphans.EmptyRows(doc) {
		out = append(out, orphans.Finding{Kind: orphans.FindEmptyRow, Path: p})
	}
	return out
}

// mayHoldNestedRows is whether a stored document could carry orphans at all:
// orphans only ever sit in a list INSIDE a row, so a document none of whose
// rows holds a list cannot have any.
func mayHoldNestedRows(doc map[string]any) bool {
	for _, r := range orphans.Rows(doc) {
		if !r.Empty && holdsList(r.Value) {
			return true
		}
	}
	return false
}

func holdsList(v any) bool {
	t, ok := v.(map[string]any)
	if !ok || lexical.IsEditorState(t) {
		// A rich-text editor state holds lists (root.children) but no rows.
		return false
	}
	for _, c := range t {
		if _, ok := c.([]any); ok {
			return true
		}
		if m, ok := c.(map[string]any); ok && holdsList(m) {
			return true
		}
	}
	return false
}

// orphanCheckAfterWrite is the §10.2.1 check of a finished write. The echo is
// compared with the body first; then, for a write that overwrote the stored
// row of a versioned entity (whose echo is the new version, built WITHOUT the
// {} rows), the stored row itself is read back once — when the echo or the
// body has rows that hold lists (the only place orphans can be), or when
// --select projected the echo so it cannot tell (a `pay publish --select …`
// sends only {_status}). force reads it back regardless (after
// --reset-nested).
func (d *Deps) orphanCheckAfterWrite(ctx context.Context, client *payload.Client, o orphanTarget, body map[string]any,
	echo payload.Doc, p query.Params, disabled, force bool) ([]output.Warning, []string) {
	if disabled || echo == nil {
		return nil, nil
	}
	echoed := map[string]any(echo)
	found := mergeFindings(orphans.EchoCheck(body, echoed), emptyFindings(echoed))
	where := "the document Payload echoed back"
	var remaining []string
	projected := len(p.Select) > 0
	if !isPlainDraftSave(p, body) && o.versioned() && (force || projected || mayHoldNestedRows(echoed) || mayHoldNestedRows(body)) {
		autosave := p.Extra.Get("autosave") == "true"
		stored, err := d.readOrphanTarget(ctx, client, o, p, autosave)
		if err != nil {
			d.RT.Warn(output.Warning{
				Code:    WarnOrphanCheckIncomplete,
				Message: fmt.Sprintf("the stored row of %s could not be read back to check for orphan rows: %s", o.what(), err.Error()),
				Hint:    "check it yourself: " + o.getCmd(),
			})
		} else {
			storedFound := mergeFindings(orphans.EchoCheck(body, stored), emptyFindings(stored))
			remaining = orphans.EmptyRows(stored)
			if len(storedFound) > 0 {
				found = mergeFindings(found, storedFound)
				where = "the stored row (read back without draft)"
				if autosave {
					where = "the autosaved version (read back)"
				}
			}
		}
	}
	if len(found) == 0 {
		return nil, remaining
	}
	return []output.Warning{orphanRowsWarning(o, found, where)}, remaining
}

// getCmd reads the stored row.
func (o orphanTarget) getCmd() string {
	if o.g != nil {
		return "pay globals get " + o.g.Slug + " --depth 0"
	}
	return "pay get " + o.t.Slug + " " + shellQuote(o.id) + " --depth 0"
}

// riskFromBackup is the pre-write prediction on a real write: the §12.8 backup
// already holds the pre-image, so the check costs nothing. It is the NEWEST
// version, which is also what a published document without a pending draft
// stores. A plain draft save leaves nothing behind itself and is skipped.
func (d *Deps) riskFromBackup(o orphanTarget, body map[string]any, p query.Params, fixCmd string) []output.Warning {
	if isPlainDraftSave(p, body) {
		return nil
	}
	kind, id := "collection", o.id
	if o.g != nil {
		kind, id = "global", ""
	}
	pre := d.backedUpDoc(kind, o.slug(), id, p.Locale)
	if pre == nil {
		return nil
	}
	a := orphans.Analyze(pre, docdiff.ApplyPatch(pre, body, docdiff.Options{}), nil, d.censusAccepts(o))
	if len(a.Risks) == 0 {
		return nil
	}
	return []output.Warning{riskWarning(o, a, false, true, fixCmd)}
}

// ---------------------------------------------------------------------------
// the dry-run diff (F7 stage B)
// ---------------------------------------------------------------------------

// Diff bases.
const (
	diffBaseLatest    = "latest"
	diffBasePublished = "published"
)

// dryRunDiff is data.diff for a write that targets one existing document:
// what the write changes, computed with Payload's PATCH semantics
// (docdiff.DiffPatch). base picks the left side: the newest version for an
// update (the state Payload merges the body into), the stored main row for
// publish/unpublish (what visitors see now). It also returns the §10.2.1
// warnings the pre-image allows: `{}` rows already present, and the rows the
// write (or, for a draft save, the later publish) would leave behind.
func (d *Deps) dryRunDiff(ctx context.Context, client *payload.Client, o orphanTarget, body map[string]any,
	p query.Params, base string, fixCmd string) (map[string]any, *preimage, []output.Warning) {
	pre, err := d.loadPreimage(ctx, client, o, p)
	if err != nil {
		return nil, nil, []output.Warning{diffUnavailableWarning(o, err)}
	}
	diff := d.diffFromPreimage(o, pre, body, base)
	// §10.2.3: a non-draft write REPLACES the stored row with the merge of
	// the body into the newest version. When the two differ (a pending
	// draft), the change list against the newest version is not what
	// visitors will see change: data.diff.published is (stored row → result).
	if base == diffBaseLatest && !isDraftWrite(p, body) && storedDiffers(pre) {
		left := orphans.WithoutEmptyRows(map[string]any(pre.stored))
		diff["published"] = d.diffData(o, diffBasePublished, left,
			docdiff.ApplyPatch(map[string]any(pre.latest), body, docdiff.Options{}))
	}
	return diff, pre, d.preimageWarnings(o, pre, body, p, fixCmd)
}

// isDraftWrite is a write that saves a version only (?draft=true, the body not
// publishing): it never touches the stored row. An autosave included.
func isDraftWrite(p query.Params, body map[string]any) bool {
	if p.Draft == nil || !*p.Draft {
		return false
	}
	st, _ := body["_status"].(string)
	return st != "published"
}

func (d *Deps) diffFromPreimage(o orphanTarget, pre *preimage, body map[string]any, base string) map[string]any {
	left := pre.latest
	if base == diffBasePublished {
		left = pre.stored
	}
	return d.diffData(o, base, map[string]any(left), docdiff.ApplyPatch(map[string]any(pre.latest), body, docdiff.Options{}))
}

// diffData diffs left → right (secrets masked) into the data.diff shape.
func (d *Deps) diffData(o orphanTarget, base string, left, right map[string]any) map[string]any {
	res := docdiff.Diff(left, right, docdiff.Options{})
	if d.cfg().Redact {
		if paths := redactChanges(res.Changes); len(paths) > 0 {
			d.RT.Warn(output.DataRedactedWarning(paths))
		}
	}
	return docDiffData(base, res, left, o.g != nil, false)
}

// docDiffData is data.diff for one document — the shape every dry run that
// previews a write shares (update, apply, globals update, publish, upsert,
// and each item of plan/sync): the docdiff result plus the base it was taken
// from and the id, _status and updatedAt of that base. global omits the id;
// brief omits the change list (plan/sync --brief).
func docDiffData(base string, res docdiff.Result, left map[string]any, global, brief bool) map[string]any {
	out := map[string]any{
		"base":      base,
		"identical": res.Identical,
		"summary":   res.Summary,
	}
	if !brief {
		out["changes"] = res.Changes
	}
	if !global {
		out["id"] = payload.Doc(left).ID()
	}
	if st, ok := left["_status"]; ok {
		out["status"] = st
	}
	if u, ok := left["updatedAt"]; ok {
		out["updated_at"] = u
	}
	return out
}

// preimageWarnings are the orphan warnings a pre-image supports.
func (d *Deps) preimageWarnings(o orphanTarget, pre *preimage, body map[string]any, p query.Params, fixCmd string) []output.Warning {
	var out []output.Warning
	if paths := orphans.EmptyRows(pre.stored); len(paths) > 0 {
		out = append(out, emptyRowsWarning(o, paths, false))
	}
	next := docdiff.ApplyPatch(map[string]any(pre.latest), body, docdiff.Options{})
	a := orphans.Analyze(pre.stored, next, nil, d.censusAccepts(o))
	if len(a.Risks) > 0 {
		draftSave := isPlainDraftSave(p, body)
		cmd := fixCmd
		if draftSave && o.g == nil {
			cmd = "pay publish " + o.t.Slug + " " + shellQuote(o.id) + " --reset-nested --dry-run   # publish this way to remove them"
		} else if draftSave {
			cmd = "pay globals update " + o.g.Slug + " --reset-nested --dry-run   # a non-draft write this way removes them"
		}
		out = append(out, orphanRiskWarning(o, a, draftSave, cmd))
	}
	return out
}

// diffUnavailableWarning says the dry run could not read the current state.
func diffUnavailableWarning(o orphanTarget, err error) output.Warning {
	code := string(apierr.CodeOf(err))
	return output.Warning{
		Code:    "diff_unavailable",
		Message: fmt.Sprintf("the current state of %s could not be read (%s), so data.diff is absent: %s", o.what(), code, err.Error()),
		Hint:    o.getCmd() + "   # the real write will fail the same way if the document does not exist",
	}
}

// sampleDiffs is data.diff for a write over several documents: the first
// safety.SampleLimit targeted ids, each with its own change list. One batched
// read per side. whereWrite is a bulk PATCH /{collection}?where (Payload's
// bulk update), as opposed to one PATCH /{collection}/{id} per document.
func (d *Deps) sampleDiffs(ctx context.Context, client *payload.Client, t *collTarget, ids []any, body map[string]any,
	p query.Params, base string, whereWrite bool) (map[string]any, []output.Warning) {
	sample := ids
	if len(sample) > safety.SampleLimit {
		sample = sample[:safety.SampleLimit]
	}
	out := map[string]any{"sampled": len(sample), "of": len(ids), "truncated": len(ids) > len(sample),
		"base": base, "documents": []any{}}
	if len(sample) == 0 {
		return out, nil
	}
	read := func(draft bool) (*docsByID, error) {
		rp := query.Params{Where: safety.WhereIDsIn(sample), Depth: query.IntPtr(0), Limit: query.IntPtr(len(sample)),
			Locale: p.Locale, FallbackLocale: p.FallbackLocale, Trash: p.Trash}
		if draft {
			rp.Draft = query.BoolPtr(true)
		}
		got := newDocsByID()
		err := client.FindPages(ctx, t.Slug, rp, len(sample), func(l *payload.ListResult) error {
			for _, doc := range l.Docs {
				got.add(doc)
			}
			return nil
		}, payload.WithKnownRoute())
		return got, err
	}
	hasDrafts := t.flags().Drafts == nil || *t.flags().Drafts
	latest, err := read(hasDrafts)
	if err != nil {
		return nil, []output.Warning{diffUnavailableWarning(orphanTarget{t: t, id: joinIDs(sample)}, err)}
	}
	stored := latest
	if hasDrafts {
		if stored, err = read(false); err != nil {
			return nil, []output.Warning{diffUnavailableWarning(orphanTarget{t: t, id: joinIDs(sample)}, err)}
		}
	}
	// A bulk write WITHOUT ?draft is merged into the STORED row, not the
	// newest version: payload@3.87.1 collections/operations/update.js reads
	// its documents with db.find (the main table) unless draft is set
	// (queryDrafts). So the preview's base is the stored row, and a pending
	// draft is not part of the result — it is superseded (the write's new
	// version becomes the newest one). One PATCH per id merges into the
	// newest version, like `pay update <id>`.
	nonDraft := whereWrite && (p.Draft == nil || !*p.Draft)
	if nonDraft {
		base = diffBasePublished
		out["base"] = base
	}
	var warns []output.Warning
	var riskPaths, superseded, missing []string
	docs := make([]any, 0, len(sample))
	for _, id := range sample {
		l, key, ok := latest.lookup(id)
		if !ok {
			missing = append(missing, fmt.Sprint(id))
			continue
		}
		s, _, _ := stored.lookup(id)
		if s == nil {
			s = l
		}
		o := orphanTarget{t: t, id: key}
		pre := &preimage{latest: l, stored: s}
		if nonDraft {
			if storedDiffers(pre) {
				superseded = append(superseded, key)
			}
			pre.latest = s
		}
		entry := d.diffFromPreimage(o, pre, body, base)
		docs = append(docs, entry)
		a := orphans.Analyze(s, docdiff.ApplyPatch(map[string]any(pre.latest), body, docdiff.Options{}), nil, d.censusAccepts(o))
		for _, r := range a.Risks {
			for _, path := range r.Paths {
				riskPaths = append(riskPaths, key+": "+path)
			}
		}
	}
	out["documents"] = docs
	if len(missing) > 0 {
		// A targeted id the read did not return is not silently left out of
		// the preview: the agent must see that it has no diff, and why.
		out["missing"] = missing
		warns = append(warns, output.Warning{
			Code: "diff_unavailable",
			Message: fmt.Sprintf("%d of the sampled id(s) (%s) were not returned by the read, so they have no diff: "+
				"they do not exist (or are not readable) and the real write will fail for them", len(missing), strings.Join(missing, ", ")),
			Hint: fmt.Sprintf("pay get %s %s   # confirms whether the document exists", t.Slug, missing[0]),
		})
	}
	if len(superseded) > 0 {
		warns = append(warns, output.Warning{
			Code: WarnPendingDraftSuperseded,
			Message: fmt.Sprintf("%d sampled document(s) have a pending draft (ids %s): a bulk write without --draft is merged "+
				"into the STORED row, so the draft's changes are not part of the result and the write's new version "+
				"replaces the draft as the newest one (it stays in the version history)", len(superseded), strings.Join(superseded, ", ")),
			Hint: fmt.Sprintf("pay diff %s %s --draft-vs-published   # what the draft holds; publish drafts one at a time with pay publish %s <id>",
				t.Slug, superseded[0], t.Slug),
		})
	}
	if len(riskPaths) > 0 {
		warns = append(warns, output.Warning{
			Code: WarnOrphanRisk,
			Message: fmt.Sprintf("in the sampled documents this write leaves %d nested row(s) behind in block tables the "+
				"Postgres adapter only clears when the new document still has their parent block type; they would read "+
				"back as {} rows", len(riskPaths)),
			Paths: capPaths(riskPaths),
			Hint:  fmt.Sprintf("write those documents one at a time: pay update %s <id> --reset-nested --dry-run", t.Slug),
		})
	}
	return out, warns
}

// ---------------------------------------------------------------------------
// §10.2.2 body_publishes
// ---------------------------------------------------------------------------

// applyBodyStatus is the body_publishes rule for update/apply/globals update.
// payload@3.87.1 collections/operations/utilities/update.js l.29 (and
// globals/operations/update.js l.42):
//
//	isSavingDraft = draftArg && drafts && data._status !== 'published'
//
// so a body carrying _status "published" — every export of a published page
// does — publishes even when the write is sent with ?draft=true. Verified on
// localhost:3900 (pages 47, F7 stage A). PayCLI makes the explicit flag win:
//
//   - with --draft, `_status` is REMOVED from the body (the write then saves a
//     draft, as asked) and the warning says so;
//   - with neither --draft nor --publish/--unpublish, the write publishes and
//     the warning says that loudly; --publish makes it deliberate and silent.
//
// It mutates body. flagged is --publish or --unpublish (their own _status wins
// anyway). drafts is the entity's tri-state drafts flag; without drafts there
// is no _status to speak of.
func applyBodyStatus(body map[string]any, draft, flagged bool, drafts *bool) *output.Warning {
	st, _ := body["_status"].(string)
	if st != "published" || flagged || (drafts != nil && !*drafts) {
		return nil
	}
	if draft {
		delete(body, "_status")
		return &output.Warning{
			Code: WarnBodyPublishes,
			Message: "the body sets _status \"published\", which makes Payload publish even with --draft; --draft wins: " +
				"_status was removed from the body, so this saves a draft",
			Paths: []string{"_status"},
			Hint:  "to publish, send it with --publish instead of --draft, or review the draft and run `pay publish <collection> <id>`",
		}
	}
	return &output.Warning{
		Code: WarnBodyPublishes,
		Message: "the body sets _status \"published\": this write PUBLISHES the document (a whole exported document " +
			"carries its _status). Pass --publish to make that deliberate, or --draft to save a draft instead",
		Paths: []string{"_status"},
		Hint:  "with --draft PayCLI removes _status from the body; `pay diff <collection> <id> --data-file F` previews the write",
	}
}

// censusAccepts is what the cached block census (F1) observed about the
// entity's list definitions: the block types seen in each top-level blocks
// field and in each nested blocks field of each block type. It is read from
// the cache only — an orphan check never builds a census — and it only widens
// what the model believes a list accepts, so it can only remove false risks.
func (d *Deps) censusAccepts(o orphanTarget) orphans.Accepts {
	if d == nil || d.RT == nil {
		return nil
	}
	c, ok := d.RT.cachedCensus()
	if !ok || c == nil {
		return nil
	}
	kind := discovery.EntityKindCollection
	if o.g != nil {
		kind = discovery.EntityKindGlobal
	}
	e, ok := c.Entity(kind, o.slug())
	if !ok || e == nil {
		return nil
	}
	out := orphans.Accepts{}
	for _, p := range e.Paths {
		if p == nil || p.Parent != "" {
			continue
		}
		c := orphans.Container{Parent: orphans.RootKind, Field: p.Field}
		out[c] = append(out[c], sortedKeysOf(p.Types)...)
	}
	for slug, ty := range e.Types {
		if ty == nil {
			continue
		}
		collectAccepts(out, orphans.BlockKind(slug), "", ty.Fields)
	}
	return out
}

func collectAccepts(out orphans.Accepts, parent, prefix string, fields map[string]*discovery.CensusField) {
	for name, f := range fields {
		if f == nil {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		if len(f.Accepts) > 0 {
			c := orphans.Container{Parent: parent, Field: path}
			out[c] = append(out[c], sortedKeysOf(f.Accepts)...)
		}
		if f.Kind == "group" && len(f.Fields) > 0 {
			collectAccepts(out, parent, path, f.Fields)
		}
	}
}

// versionRestorePreview is data.diff (and the §10.2.1 warnings) of a
// `pay versions restore --dry-run`. payload@3.87.1 collections/operations/
// restoreVersion.js builds the result from the VERSION's content (beforeChange
// with docWithLocales = the version), keeps its _status unless ?draft, and
// without ?draft writes it over the stored row (db.updateOne) — so the diff
// is stored row → version content (base "published"); with --draft only a
// new draft version is saved, and the diff is newest version → content.
func (d *Deps) versionRestorePreview(ctx context.Context, client *payload.Client, scope versionScope, versionID string,
	p query.Params) (map[string]any, []output.Warning) {
	o := orphanTarget{t: scope.coll, g: scope.global}
	ver, _, err := client.VersionGet(ctx, scope.target, versionID, query.Params{Depth: query.IntPtr(0)}, payload.WithKnownRoute())
	if err != nil {
		o.id = versionID
		return nil, []output.Warning{diffUnavailableWarning(o, err)}
	}
	content, _ := ver["version"].(map[string]any)
	if scope.global == nil {
		parent := ver["parent"]
		if obj, ok := parent.(map[string]any); ok {
			parent = obj["id"]
		}
		if parent == nil || content == nil {
			return nil, nil
		}
		o.id = fmt.Sprint(parent)
	}
	pre, err := d.loadPreimage(ctx, client, o, p)
	if err != nil {
		return nil, []output.Warning{diffUnavailableWarning(o, err)}
	}
	right := orphans.CloneDoc(content)
	// A version's content carries no id (the result keeps the document's).
	if id, ok := pre.stored["id"]; ok {
		if _, has := right["id"]; !has {
			right["id"] = id
		}
	}
	draft := p.Draft != nil && *p.Draft
	if draft {
		right["_status"] = "draft"
		return d.diffData(o, diffBaseLatest, map[string]any(pre.latest), right), nil
	}
	out := d.diffData(o, diffBasePublished, map[string]any(pre.stored), right)
	var warns []output.Warning
	if paths := orphans.EmptyRows(pre.stored); len(paths) > 0 {
		warns = append(warns, emptyRowsWarning(o, paths, false))
	}
	if a := orphans.Analyze(pre.stored, right, nil, d.censusAccepts(o)); len(a.Risks) > 0 {
		warns = append(warns, orphanRiskWarning(o, a, false,
			o.resetCmd(" --dry-run")+"   # after the restore: the id-preserving repair of the rows it leaves"))
	}
	return out, warns
}
