package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/docdiff"
	"github.com/KLIXPERT-io/pay-cli/internal/lexical"
	"github.com/KLIXPERT-io/pay-cli/internal/manifest"
	"github.com/KLIXPERT-io/pay-cli/internal/orphans"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
)

// ---------------------------------------------------------------------------
// F6 — the plan engine shared by `pay plan`, `pay sync` and `pay upsert`
// ---------------------------------------------------------------------------
//
// A migration agent used to write "find by slug → update, else create" by
// hand, with an ids.json state file so the next run would not create
// duplicates, and a manual `diff <(jq -S …)` to see whether a re-run would
// change anything. The plan engine does all of it from the server's current
// state:
//
//  1. every manifest item names one document by its match fields (or one
//     global); one bounded lookup finds it (newest draft, depth 0);
//  2. references between items are ordered so a document is created before
//     the items that point at it;
//  3. each body is diffed against the document with Payload's PATCH
//     semantics (docdiff.DiffPatch): identical → unchanged, no write at all.
//
// Execution (sync, upsert) goes through the ordinary write paths —
// runCreateOne, runUpdateOne, runGlobalUpdateOne — so backups, the audit
// log, the echo-diff and created_as_draft apply to every item exactly as they
// do to `pay create`/`pay update`/`pay globals update`.

// Plan actions.
const (
	actionCreate    = "create"
	actionUpdate    = "update"
	actionUnchanged = "unchanged"
	// actionError and actionNotPlanned only appear in a plan that failed:
	// the item could not be planned, or it references one that could not.
	actionError      = "error"
	actionNotPlanned = "not_planned"
)

// Write modes: how an item's write treats publication state.
const (
	// modePublish sends _status "published" (no ?draft): the result is live.
	modePublish = "publish"
	// modeDraft sends ?draft=true: a draft version, the live one untouched.
	modeDraft = "draft"
	// modeSave is a plain save: no ?draft, no _status. On a published
	// document Payload keeps it published (the base is its newest version);
	// on an entity without drafts it is the only kind of write there is.
	modeSave = "save"
)

// WarnBodyStatusIgnored says a body's _status was removed: in upsert and
// sync the publication state is set by --draft/--publish (a manifest's
// "status") and nothing else. Leaving it in would reopen the body_publishes
// footgun — Payload publishes any write whose body says _status "published",
// ?draft=true notwithstanding (payload@3.87.1 collections/operations/
// utilities/update.js, isSavingDraft).
const WarnBodyStatusIgnored = "body_status_ignored"

// serverOwnedKeys are top-level keys a document export carries and a write
// must not: the id (the URL or the match decides), the timestamps (Payload
// sets them), a global's globalType. _status is handled separately because
// it is reported.
var serverOwnedKeys = []string{"id", "createdAt", "updatedAt", "globalType"}

// cleanBody returns a copy of body without server-owned keys and without
// _status; status is the removed _status value (nil when there was none).
func cleanBody(body map[string]any) (out map[string]any, status any) {
	out = make(map[string]any, len(body))
	for k, v := range body {
		out[k] = v
	}
	for _, k := range serverOwnedKeys {
		delete(out, k)
	}
	if st, ok := out["_status"]; ok {
		status = st
		if status == nil {
			status = "null"
		}
		delete(out, "_status")
	}
	return out, status
}

// statusIgnoredWarning is the one warning for every body whose _status was
// dropped. keys names the manifest items (sync, plan); nil means upsert.
func statusIgnoredWarning(keys []string) output.Warning {
	if len(keys) == 0 {
		return output.Warning{
			Code: WarnBodyStatusIgnored,
			Message: "the body carried _status, which was removed: upsert sets the publication state from --draft/--publish only " +
				"(a body _status \"published\" would publish even a --draft write)",
			Paths: []string{"_status"},
			Hint:  "pass --publish to publish, --draft to save a draft; with neither the document keeps its state (a new one is created as a draft)",
		}
	}
	return output.Warning{
		Code: WarnBodyStatusIgnored,
		Message: fmt.Sprintf("%d item(s) carried _status in \"data\", which was removed: a manifest sets the publication state "+
			"with the item's \"status\" only (a body _status \"published\" would publish even a draft write)", len(keys)),
		Paths: keys,
		Hint:  "set \"status\": \"published\" on an item to publish it, \"draft\" to save a draft; the default \"keep\" leaves its state alone",
	}
}

// statusOfDoc is a document's _status, "" when it has none.
func statusOfDoc(doc map[string]any) string {
	s, _ := doc["_status"].(string)
	return s
}

// decideMode maps a requested status onto a write mode. drafts is the
// entity's drafts flag (nil = never learned); current is the document's
// newest version, nil for a document that does not exist yet.
func decideMode(drafts *bool, status manifest.Status, current map[string]any) string {
	enabled := flagOn(drafts)
	if drafts == nil && current != nil {
		// Tri-state: never learned, but the document itself says it has a
		// publication state.
		_, enabled = current["_status"]
	}
	if drafts == nil && status != manifest.StatusKeep {
		// Asked for explicitly on an entity whose drafts support is unknown:
		// attempt it (§9.3 tri-state) rather than refuse.
		enabled = true
	}
	if !enabled {
		return modeSave
	}
	switch status {
	case manifest.StatusPublished:
		return modePublish
	case manifest.StatusDraft:
		return modeDraft
	}
	if current != nil && statusOfDoc(current) == "published" {
		return modeSave
	}
	return modeDraft
}

// diffOptions is how plans diff: the default ignore set plus --ignore, with
// the entity's top-level json fields treated as leaves (Payload stores them
// wholesale, they do not merge like groups).
func diffOptions(shard *discovery.Shard, ignore []string, full bool) docdiff.Options {
	opts := docdiff.Options{IgnorePaths: append([]string(nil), ignore...), Full: full}
	if shard != nil {
		for _, f := range shard.Fields {
			if f.PayloadType == discovery.TypeJSON && f.Parent == nil && f.Path != "" {
				opts.LeafPaths = append(opts.LeafPaths, f.Path)
			}
		}
	}
	return opts
}

// uploadCreateError refuses a create in an upload collection: its POST
// needs the file as multipart, which a JSON body cannot carry. Updating an
// existing upload document's metadata is fine.
func uploadCreateError(t *collTarget, what string) error {
	if t == nil || !flagOn(t.flags().Upload) {
		return nil
	}
	return apierr.New(apierr.CodeFileMissing,
		"%s would create a %s document, and %s is an upload collection: creating one needs the file itself", what, t.Slug, t.Slug).
		WithHint("upload the file first (pay upload %s <file> --set …), then re-run: the existing document is matched and its fields updated", t.Slug)
}

// redactPlanDiff masks secret values in a diff (§5.3, as `pay diff` does):
// the envelope's redaction is key-based and a diff carries a secret's name
// only in the change path. It returns the masked paths.
func redactPlanDiff(d *Deps, res *docdiff.Result) []string {
	if res == nil || !d.cfg().Redact {
		return nil
	}
	return redactChanges(res.Changes)
}

// decision is what one document's write needs: the action, the mode, the
// diff, and the exact body and params to send.
type decision struct {
	action string
	mode   string
	diff   *docdiff.Result
	// fields are the top-level body keys the write sends (a create sends the
	// whole body; an update only the fields whose value changes).
	fields []string
	// resent are the top-level scalar fields an update sends although their
	// value is already stored (see unchangedScalars); not part of fields,
	// which says what changes.
	resent []string
	// body is what the write takes those fields from: the caller's body,
	// aligned with the stored document (alignToStored) for an update — row
	// ids adopted, equal instants in their stored form.
	body map[string]any
}

// decide compares body with current (nil = the document does not exist).
// force writes the whole body even when nothing would change. leaves are the
// entity's top-level json fields (stored wholesale, never aligned).
func decide(current, body map[string]any, drafts *bool, status manifest.Status, opts docdiff.Options, force bool) decision {
	dc := decision{mode: decideMode(drafts, status, current), body: body}
	if current == nil {
		dc.action = actionCreate
		dc.fields = sortedMapKeys(body)
		return dc
	}
	dc.body = alignToStored(current, body, opts.LeafPaths)
	res := docdiff.DiffPatch(current, dc.body, opts)
	dc.diff = &res
	statusChange := dc.mode == modePublish && statusOfDoc(current) != "published"
	switch {
	case force:
		dc.action = actionUpdate
		dc.fields = sortedMapKeys(dc.body)
	case res.Identical && !statusChange:
		dc.action = actionUnchanged
		dc.fields = []string{}
	default:
		dc.action = actionUpdate
		dc.fields = append([]string{}, res.Summary.Fields...)
		dc.resent = unchangedScalars(current, dc.body, dc.fields)
	}
	return dc
}

// noResend are upload fields an update never re-sends: Payload derives them
// from the file, and filename + url together make it fetch the file again
// (uploads/generateFileData.js).
var noResend = map[string]bool{
	"url": true, "thumbnailURL": true, "filename": true, "mimeType": true, "filesize": true,
	"width": true, "height": true, "sizes": true, "focalX": true, "focalY": true,
}

// unchangedScalars are the top-level fields of body, other than changed,
// whose value is a string, number or boolean equal to the stored one. An
// update sends them along with the changes: a beforeChange hook that fills a
// field the REQUEST leaves out (the website template's populatePublishedAt
// sets publishedAt to now whenever req.data has none) would otherwise
// overwrite the value the body declares on every write that does not change
// it. Sending an equal value changes nothing else (PATCH semantics); a field
// whose value differs but is --ignore'd is not sent.
func unchangedScalars(current, body map[string]any, changed []string) []string {
	out := []string{}
	for _, k := range sortedMapKeys(body) {
		v := body[k]
		if containsString(changed, k) || noResend[k] || k == "_status" || !manifest.IsScalar(v) || v == nil {
			continue
		}
		if sv, ok := current[k]; ok && docdiff.Equal(sv, v) {
			out = append(out, k)
		}
	}
	return out
}

// alignToStored is the body an update of current really sends.
//
// ROW IDS. A body row without an id is a NEW row to Payload
// (getExistingRowDoc.js: no id match → an empty sibling document), so every
// key the body leaves out falls back to its defaultValue on save — a value an
// editor set in the admin UI (`appearance: "outline"` on a link) is reset,
// nested rows are recreated, and every row gets a fresh id. A manifest is
// normally written without row ids (nobody copies server-generated ids into
// content files), and an export plus one appended row mixes both. So each
// id-less body row adopts the id of a stored row: the first stored row, in
// order, that no body row claims by its own id and no earlier id-less row
// adopted, with the same blockType (rows without a blockType pair with rows
// without one). Payload then merges the row (getFallbackValue: an unsent key
// keeps its stored value), the row keeps its id, and the diff — computed by
// docdiff.DiffPatch with those same merge rules — is exactly what the write
// does. An id-less row with no such partner stays new. Rows with an id of
// their own are left as they are (an unknown id is a new row, as Payload
// treats it). The rule recurses into matched rows and groups: their nested
// rows pair the same way.
//
// DATES. Payload stores a date as an ISO instant ("2026-01-01T00:00:00.000Z")
// whatever form was written ("2026-01-01"). A body string that names the same
// instant as a stored string of that form is replaced by the stored form, so
// a manifest date does not make its item "update" forever.
//
// Rich text, top-level json fields (leaves) and scalars other than such dates
// are sent as written. Neither argument is modified; the result shares
// unchanged subtrees with body.
func alignToStored(current, body map[string]any, leaves []string) map[string]any {
	out, copied := body, false
	for k, bv := range body {
		if k == "id" || containsString(leaves, k) {
			continue
		}
		sv, ok := current[k]
		if !ok {
			continue
		}
		nv, changed := alignValue(sv, bv)
		if !changed {
			continue
		}
		if !copied {
			out, copied = cloneShallow(body), true
		}
		out[k] = nv
	}
	return out
}

// alignValue aligns one value with its stored counterpart; changed reports a
// new value (body itself is never modified).
func alignValue(stored, body any) (any, bool) {
	switch b := body.(type) {
	case string:
		if s, ok := stored.(string); ok && s != b && sameInstant(s, b) {
			return s, true
		}
		if sameNumber(stored, b) {
			return stored, true
		}
	case map[string]any:
		s, ok := stored.(map[string]any)
		if !ok || lexicalState(s) || lexicalState(b) {
			return body, false
		}
		return alignObject(s, b)
	case []any:
		s, ok := stored.([]any)
		if !ok {
			return body, false
		}
		return alignRows(s, b)
	}
	return body, false
}

// alignObject aligns a group or a row key by key.
func alignObject(stored, body map[string]any) (map[string]any, bool) {
	out, changed := body, false
	for k, bv := range body {
		if k == "id" {
			continue
		}
		sv, ok := stored[k]
		if !ok {
			continue
		}
		nv, ch := alignValue(sv, bv)
		if !ch {
			continue
		}
		if !changed {
			out, changed = cloneShallow(body), true
		}
		out[k] = nv
	}
	return out, changed
}

// alignRows pairs the body's rows with stored rows (see alignToStored).
func alignRows(stored, body []any) ([]any, bool) {
	byID := map[string]int{}
	for j, r := range stored {
		if id, ok := rowIDOf(r); ok {
			byID[id] = j
		}
	}
	claimed := map[int]bool{}
	for _, r := range body {
		if id, ok := rowIDOf(r); ok {
			if j, ok := byID[id]; ok {
				claimed[j] = true
			}
		}
	}
	var out []any
	for i, r := range body {
		b, ok := r.(map[string]any)
		if !ok {
			// A hasMany relationship written with string ids ["66"]
			// against stored [66]: element by element.
			if str, isStr := r.(string); isStr && i < len(stored) && sameNumber(stored[i], str) {
				if out == nil {
					out = append([]any(nil), body...)
				}
				out[i] = stored[i]
			}
			continue
		}
		if lexicalState(b) {
			continue
		}
		var partner map[string]any
		adopt := false
		if id, has := rowIDOf(b); has {
			if j, ok := byID[id]; ok && sameBlockType(stored[j], b) {
				partner, _ = stored[j].(map[string]any)
			}
		} else {
			for j, sr := range stored {
				if _, ok := rowIDOf(sr); !ok || claimed[j] || !sameBlockType(sr, b) {
					continue
				}
				claimed[j] = true
				partner, _ = sr.(map[string]any)
				adopt = true
				break
			}
		}
		if partner == nil {
			continue
		}
		nb, changed := alignObject(partner, b)
		if adopt {
			if !changed {
				nb = cloneShallow(b)
			}
			nb["id"] = partner["id"]
			changed = true
		}
		if !changed {
			continue
		}
		if out == nil {
			out = append([]any(nil), body...)
		}
		out[i] = nb
	}
	if out == nil {
		return body, false
	}
	return out, true
}

// rowIDOf is a row's non-null id, rendered for comparison.
func rowIDOf(row any) (string, bool) {
	m, ok := row.(map[string]any)
	if !ok || m["id"] == nil {
		return "", false
	}
	return manifest.ScalarString(m["id"]), true
}

// sameBlockType pairs rows of the same block type (or two rows without one).
func sameBlockType(stored any, body map[string]any) bool {
	s, ok := stored.(map[string]any)
	if !ok {
		return false
	}
	return fmt.Sprint(s["blockType"]) == fmt.Sprint(body["blockType"])
}

// sameNumber reports a stored number and a body string that spell the same
// number: a relationship or upload id written as "4" where Payload stores 4
// (--set coerces through the schema; a manifest or --data body does not).
// The stored form is sent, so the item does not plan "update" forever.
func sameNumber(stored any, body string) bool {
	var sf float64
	switch n := stored.(type) {
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return false
		}
		sf = f
	case float64:
		sf = n
	case int:
		sf = float64(n)
	case int64:
		sf = float64(n)
	default:
		return false
	}
	bf, err := strconv.ParseFloat(strings.TrimSpace(body), 64)
	return err == nil && body != "" && bf == sf
}

// payloadInstant is how Payload serialises every date it stores
// (Date.prototype.toISOString).
const payloadInstant = "2006-01-02T15:04:05.000Z07:00"

// sameInstant reports a stored ISO instant and a body string that name the
// same moment. The body must say which moment it means: a date alone
// ("2026-01-01", which Payload reads as UTC midnight, as JavaScript's Date
// does) or a date-time with a zone; a local time without a zone depends on
// the server's clock and is never equated.
func sameInstant(stored, body string) bool {
	if len(stored) != len("2006-01-02T15:04:05.000Z") || !strings.HasSuffix(stored, "Z") {
		return false
	}
	st, err := time.Parse(payloadInstant, stored)
	if err != nil {
		return false
	}
	for _, layout := range []string{"2006-01-02", time.RFC3339Nano} {
		if bt, err := time.Parse(layout, body); err == nil {
			return bt.Equal(st)
		}
	}
	return false
}

func cloneShallow(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

func lexicalState(v any) bool { return lexical.IsEditorState(v) }

// writeBody is what the write sends: the chosen fields of the decision's
// body (the changed ones and the re-sent scalars), plus _status "published"
// in publish mode.
func (dc decision) writeBody() map[string]any {
	out := make(map[string]any, len(dc.fields)+len(dc.resent)+1)
	for _, f := range append(append([]string(nil), dc.fields...), dc.resent...) {
		if v, ok := dc.body[f]; ok {
			out[f] = v
		}
	}
	if dc.mode == modePublish {
		out["_status"] = "published"
	}
	return out
}

// WarnHookRewroteUnsent says a write changed fields the body declares but
// did not send (their value was already stored): a server hook fills
// fields a request leaves out.
const WarnHookRewroteUnsent = "hook_rewrote_unsent"

// hookRewroteWarning compares the echo of an update with the fields the body
// declares and the write did not send. Scalars are re-sent (unchangedScalars),
// so this catches the rest: a hook that resets a group, an array or a
// relationship whenever the request lacks it. echo must be depth 0.
func hookRewroteWarning(echo map[string]any, dc decision, opts docdiff.Options, rerun string) *output.Warning {
	unsent := map[string]any{}
	for k, v := range dc.body {
		if !containsString(dc.fields, k) && !containsString(dc.resent, k) && k != "_status" {
			unsent[k] = v
		}
	}
	if len(unsent) == 0 || echo == nil {
		return nil
	}
	res := docdiff.DiffPatch(echo, unsent, opts)
	if res.Identical {
		return nil
	}
	return &output.Warning{
		Code: WarnHookRewroteUnsent,
		Message: fmt.Sprintf("the write changed %s, which the body declares and the write did not send (the stored value was "+
			"already equal): a server hook rewrites a field the request leaves out, so the document no longer matches the body",
			strings.Join(res.Summary.Fields, ", ")),
		Paths: res.Summary.Fields,
		Hint:  rerun,
	}
}

// writeParams adds the mode's ?draft to the item's read/write params.
func (dc decision) writeParams(p query.Params) query.Params {
	out := p.Clone()
	out.Draft = nil
	if dc.mode == modeDraft {
		out.Draft = query.BoolPtr(true)
	}
	return out
}

// ---------------------------------------------------------------------------
// the plan
// ---------------------------------------------------------------------------

// planOptions are the flags plan and sync share.
type planOptions struct {
	ignore   []string
	full     bool
	brief    bool
	noEcho   bool
	failFast bool
}

// planItem is one manifest item on its way through the plan.
type planItem struct {
	it  *manifest.Item
	idx int
	t   *collTarget   // collection items
	g   *globalTarget // global items

	params query.Params // depth 0 + the item's locale: the reads and the write
	data   map[string]any
	// maskedWhole: data holds "<redacted>" inside rich text or json, put
	// back from the stored document once it is known (restoreMasked).
	maskedWhole bool
	// bodyID is the id the item's data carried (an export's own id); the
	// body never sends it, but a match that finds another document (or
	// none) is reported (body_id_mismatch).
	bodyID    any
	matchedBy map[string]any
	exists    bool
	current   map[string]any
	id        any
	idString  string
	// storedStatus is the stored (main) row's _status, read only for an
	// update whose newest version is a draft (pendingDraftCheck); "" else.
	storedStatus string

	// links are the plan items this item's references point at; deps the
	// subset that does not exist yet (they must be created first).
	links []int
	deps  []int

	body     map[string]any
	dc       decision
	warnings []output.Warning
	// preview are warnings about the write a plan previews (orphan_risk);
	// they are reported by plan and sync --dry-run, not after a real write,
	// whose own checks speak then.
	preview []output.Warning

	// execution (sync)
	// matchRejected marks a create that failed on one of its match fields.
	matchRejected bool
	result        string
	err           *apierr.Error
	reason        string
}

func (pi *planItem) drafts() *bool {
	if pi.g != nil {
		return pi.g.flags().Drafts
	}
	return pi.t.flags().Drafts
}

func (pi *planItem) shard() *discovery.Shard {
	if pi.g != nil {
		return pi.g.Shard
	}
	return pi.t.Shard
}

func (pi *planItem) slug() string {
	if pi.g != nil {
		return pi.g.Slug
	}
	return pi.t.Slug
}

// syncPlan is a whole manifest set, planned.
type syncPlan struct {
	items    []*planItem
	order    []int
	byKey    map[string]int
	resolver *refResolver
	opts     planOptions
	// statusIgnored are the keys whose data._status was removed.
	statusIgnored []string
	// masked are the keys whose data held "<redacted>" values in merged
	// fields (not sent), maskedPaths those values' paths; maskRemoved the
	// keys whose masked rich text / json keys had no stored value to take
	// the real one from (removed), maskRemovedPaths their paths.
	masked, maskedPaths           []string
	maskRemoved, maskRemovedPaths []string
	// refUnchecked are the keys whose references sit in fields of unknown
	// relationTo, refUncheckedPaths those references' paths.
	refUnchecked, refUncheckedPaths []string
	// problems is everything that stops the plan from being executed.
	problems manifest.Problems
	// executed is set once sync starts writing.
	executed bool
}

// fail records why pi cannot be planned; later stages skip it.
func (sp *syncPlan) fail(pi *planItem, err error) {
	e := apierr.From(err)
	if pi.err == nil {
		pi.err = e
	}
	where := pi.it.Label()
	if pi.it.Key != "" {
		where = pi.it.Key + " (" + pi.it.Label() + ")"
	}
	sp.problems = append(sp.problems, manifest.Problem{Where: where, Code: e.Code, Message: e.Message, Hint: e.Hint})
}

// placeholder stands in for the id of an item a sync would create, so a plan
// can still diff the items that point at it.
func placeholder(key string) string {
	return fmt.Sprintf("<id of @%s, created by this sync>", key)
}

// linked finds the plan item a reference names: "@key", or an
// equals-reference whose collection, fields and values are exactly another
// item's collection and match.
func (sp *syncPlan) linked(r *manifest.Ref) (int, bool) {
	if r.IsLocal() {
		j, ok := sp.byKey[r.Local]
		return j, ok
	}
	if r.Where != nil || len(r.Equals) == 0 {
		return 0, false
	}
	for j, other := range sp.items {
		if other.t == nil || other.t.Slug != r.Collection || len(other.it.Match) != len(r.Equals) {
			continue
		}
		same := true
		for _, term := range r.Equals {
			v, ok := other.matchedBy[term.Field]
			if !ok || manifest.ScalarString(v) != manifest.ScalarString(term.Value) {
				same = false
				break
			}
		}
		if same {
			return j, true
		}
	}
	return 0, false
}

// local is the plan's half of reference resolution (refResolver.local).
func (sp *syncPlan) local(r *manifest.Ref) (any, string, bool, error) {
	j, ok := sp.linked(r)
	if !ok {
		if r.IsLocal() {
			return nil, "", true, apierr.New(apierr.CodeRefUnresolved,
				"$ref at %s names plan item %q, and no item has that key", refAt(r), manifest.LocalPrefix+r.Local).
				WithHint("keys: %s", strings.Join(sp.keys(), ", "))
		}
		return nil, "", false, nil
	}
	target := sp.items[j]
	if target.g != nil {
		return nil, "", true, apierr.New(apierr.CodeBadRequestBody,
			"$ref at %s names %q, a global; a reference resolves to a document id and globals have none", refAt(r), target.it.Key)
	}
	if target.id != nil {
		return r.Value(target.t.Slug, target.id), target.t.Slug, true, nil
	}
	return r.Value(target.t.Slug, placeholder(target.it.Key)), target.t.Slug, true, nil
}

func (sp *syncPlan) keys() []string {
	out := make([]string, 0, len(sp.items))
	for _, pi := range sp.items {
		out = append(out, pi.it.Key)
	}
	return out
}

// buildPlan loads the manifests and plans every item against the server.
// The returned plan is non-nil whenever the manifests could be read; err is
// set when anything stops it from being executed, and then sp.problems
// names every item concerned.
func buildPlan(ctx context.Context, d *Deps, paths []string, opts planOptions) (*syncPlan, error) {
	loaded, err := manifest.Load(paths, d.inStream())
	if err != nil {
		return nil, err
	}
	sp := &syncPlan{byKey: map[string]int{}, opts: opts, resolver: d.newRefResolver()}
	sp.resolver.local = sp.local
	for i, it := range loaded {
		sp.items = append(sp.items, &planItem{it: it, idx: i})
		sp.byKey[it.Key] = i
	}
	// Every stage runs over the items the earlier stages accepted, so one
	// run reports every problem it can find, not only the first.
	for _, pi := range sp.items {
		sp.target(ctx, d, pi)
	}
	for _, pi := range sp.items {
		if pi.err == nil {
			sp.read(ctx, d, pi)
		}
	}
	sp.checkDuplicateTargets()
	sp.link()
	if err := sp.sort(); err != nil {
		return sp, err
	}
	for _, i := range sp.order {
		pi := sp.items[i]
		if pi.err != nil {
			continue
		}
		if bad := sp.unplannedDep(pi); bad != "" {
			pi.reason = "not planned: it references " + bad + ", which cannot be planned"
			continue
		}
		sp.plan(ctx, d, pi)
	}
	if len(sp.problems) > 0 {
		return sp, sp.problems.Err()
	}
	return sp, nil
}

// unplannedDep names the first item pi references that failed to plan (or
// was itself skipped): pi's body cannot be resolved without it.
func (sp *syncPlan) unplannedDep(pi *planItem) string {
	for _, j := range pi.links {
		dep := sp.items[j]
		if dep.err != nil || (dep.dc.action == "" && dep.reason != "") {
			return dep.it.Key
		}
	}
	return ""
}

// target resolves the item's collection or global and prepares its body.
func (sp *syncPlan) target(ctx context.Context, d *Deps, pi *planItem) {
	it := pi.it
	var locTarget *collTarget
	if it.Global != "" {
		g, err := d.global(ctx, it.Global)
		if err != nil {
			sp.fail(pi, err)
			return
		}
		pi.g = g
		locTarget = g.asCollTarget()
	} else {
		t, err := d.collection(ctx, it.Collection)
		if err != nil {
			sp.fail(pi, err)
			return
		}
		pi.t = t
		locTarget = t
	}
	if it.Status != manifest.StatusKeep {
		if drafts := pi.drafts(); drafts != nil && !*drafts {
			sp.fail(pi, apierr.New(apierr.CodeFeatureUnavailable,
				"\"status\": %q needs drafts, and %s has none (every write to it is live)", it.Status, it.Target()).
				WithHint("drop \"status\" (or set \"keep\")"))
			return
		}
	}
	if id, ok := it.Data["id"]; ok && pi.t != nil && manifest.IsScalar(id) {
		pi.bodyID = id
	}
	data, status := cleanBody(it.Data)
	if status != nil {
		sp.statusIgnored = append(sp.statusIgnored, it.Key)
	}
	// §9.15.2: masked values in merged fields are left out now; those
	// inside rich text or json wait for the stored document (decideItem).
	data, masked, inWhole := withoutMasked(data, wholesaleValues(locTarget.Shard))
	pi.maskedWhole = inWhole
	if len(masked) > 0 {
		sp.masked = append(sp.masked, it.Key)
		sp.maskedPaths = appendMissing(sp.maskedPaths, masked...)
	}
	// §9.12.8: a document read with --richtext md|text (its envelope already
	// refused by the manifest loader, or a bare export of one) has strings
	// where the schema declares Lexical editor states.
	if err := checkRenderedBody("the item's \"data\"", locTarget, data, locTarget.Shard); err != nil {
		// A manifest has no --set-md: the Markdown route is the converter.
		sp.fail(pi, apierr.From(err).WithHint("re-export the document without --richtext (the default is json), "+
			"or convert Markdown with `pay lexical from-md file.md` and put the editor state it prints into \"data\""))
		return
	}
	pi.data = data
	pi.params = query.Params{Depth: query.IntPtr(0)}
	warn, err := applyWriteLocale(d, locTarget, it.Locale, &pi.params)
	if err != nil {
		sp.fail(pi, err)
		return
	}
	if warn != nil {
		pi.warnings = append(pi.warnings, *warn)
	}
	if pi.t != nil {
		vals, _, err := it.MatchValues()
		if err != nil {
			sp.fail(pi, apierr.New(apierr.CodeManifestInvalid, "%s", err.Error()))
			return
		}
		pi.matchedBy = vals
	}
}

// read fetches the item's current state: the one document its match finds
// (newest draft, depth 0), or the global.
func (sp *syncPlan) read(ctx context.Context, d *Deps, pi *planItem) {
	if pi.g != nil {
		client, err := d.requireClient()
		if err != nil {
			sp.fail(pi, err)
			return
		}
		p := pi.params.Clone()
		if flagOn(pi.g.flags().Drafts) {
			p.Draft = query.BoolPtr(true)
		}
		doc, _, err := client.GlobalGet(ctx, pi.g.Slug, p, payload.WithKnownRoute())
		if err != nil {
			sp.fail(pi, err)
			return
		}
		pi.exists, pi.current = true, map[string]any(doc)
		return
	}
	spec := matchSpec(pi.t, pi.it.Match, pi.matchedBy, pi.params.Locale)
	list, err := d.lookupMatches(ctx, spec, true)
	if err != nil {
		sp.fail(pi, err)
		return
	}
	switch len(list.Docs) {
	case 0:
		if err := idMatchCreate(pi.t, pi.it.Match, pi.matchedBy, "the item"); err != nil {
			sp.fail(pi, err)
			return
		}
	case 1:
		doc := list.Docs[0]
		pi.exists, pi.current = true, map[string]any(doc)
		pi.id, pi.idString = doc.ID(), doc.IDString()
	default:
		sp.fail(pi, matchAmbiguous(pi.t, list, spec, "\"match\""))
		return
	}
	if w := bodyIDMismatch(pi.t, pi.bodyID, pi.id, pi.matchedBy, "the item's data"); w != nil {
		pi.warnings = append(pi.warnings, *w)
	}
}

// idMatchCreate refuses the create a match on "id" would lead to: the new
// document gets a server-assigned id, so the next run would not find it by
// that id and would create another one.
func idMatchCreate(t *collTarget, fields []string, values map[string]any, what string) error {
	if !containsString(fields, "id") {
		return nil
	}
	return apierr.New(apierr.CodeDocNotFound,
		"%s matches on id %s, and no %s document has it; a match on id can only update the document with that id "+
			"(a create would get a new id, and every later run would create another copy)",
		what, manifest.ScalarString(values["id"]), t.Slug).
		WithHint("match on a field the body carries that identifies the document on this server (\"match\": [\"title\"], --match title), "+
			"or create it once with `pay create %s --data-file <file>`", t.Slug)
}

// bodyIDMismatch warns when a body carries an id (a `pay get` export does)
// that is not the document its match found: an export whose slug was edited
// matches nothing and would CREATE a copy of the page it was read from, with
// every row id duplicated. The id is never sent (the URL or the match
// decides), so this is advice, not a refusal: an export from another server
// legitimately has another id.
func bodyIDMismatch(t *collTarget, bodyID, id any, matched map[string]any, what string) *output.Warning {
	if bodyID == nil || t == nil {
		return nil
	}
	b := manifest.ScalarString(bodyID)
	if id != nil && manifest.ScalarString(id) == b {
		return nil
	}
	by := make([]string, 0, len(matched))
	for _, k := range sortedMapKeys(matched) {
		by = append(by, fmt.Sprintf("%s %q", k, manifest.ScalarString(matched[k])))
	}
	found := "finds no document, so this CREATES a new one"
	if id != nil {
		found = fmt.Sprintf("finds %s %s, another document", t.Slug, manifest.ScalarString(id))
	}
	return &output.Warning{
		Code: WarnBodyIDMismatch,
		Message: fmt.Sprintf("%s carries id %s, but the match on %s %s (the body's id is never sent). "+
			"Harmless for an export from another server; for an export of THIS server it usually means a match value "+
			"(the slug) was edited, and the result is a second document, not a rename.",
			what, b, strings.Join(by, ", "), found),
		Paths: []string{"id"},
		Hint: fmt.Sprintf("to change the slug of %s %s, update it by id: pay update %s %s --set slug=<new> --dry-run",
			t.Slug, b, t.Slug, shellQuote(b)),
	}
}

// matchSpec is the lookup for match fields whose values come from a body.
func matchSpec(t *collTarget, fields []string, values map[string]any, locale string) lookupSpec {
	s := lookupSpec{t: t, locale: locale}
	for _, f := range fields {
		s.terms = append(s.terms, manifest.Term{Field: f, Value: values[f]})
	}
	return s
}

// matchAmbiguous is match_ambiguous in the shared ambiguity shape.
func matchAmbiguous(t *collTarget, list *payload.ListResult, spec lookupSpec, what string) *apierr.Error {
	return ambiguousLookup(apierr.CodeMatchAmbiguous, t, list, flagOn(t.flags().Drafts),
		fmt.Sprintf("have %s; %s must identify exactly one", spec.describe(), what),
		"add a field to the match that makes it unique, or write that document by id",
		spec.filterArgs(), spec.selectField())
}

// checkDuplicateTargets rejects two items that resolved to the same stored
// document through different match fields.
func (sp *syncPlan) checkDuplicateTargets() {
	seen := map[string]*planItem{}
	for _, pi := range sp.items {
		if pi.t == nil || !pi.exists || pi.err != nil {
			continue
		}
		k := pi.t.Slug + "\x00" + pi.idString + "\x00" + pi.params.Locale
		if prev, ok := seen[k]; ok {
			sp.fail(pi, apierr.New(apierr.CodeManifestInvalid,
				"matches %s %s, the same document as %s; one document is described by one item", pi.t.Slug, pi.idString, prev.it.Key))
			continue
		}
		seen[k] = pi
	}
}

// link records, for every item, the plan items its references point at.
func (sp *syncPlan) link() {
	for i, pi := range sp.items {
		if pi.err != nil {
			continue
		}
		refs, err := manifest.Collect(pi.data)
		if err != nil {
			sp.fail(pi, err)
			continue
		}
		seen := map[int]bool{}
		for _, r := range refs {
			j, ok := sp.linked(r)
			if !ok {
				if r.IsLocal() {
					if _, _, _, err := sp.local(r); err != nil {
						sp.fail(pi, err)
					}
				}
				continue
			}
			if sp.items[j].g != nil {
				_, _, _, err := sp.local(r)
				sp.fail(pi, err)
				continue
			}
			if seen[j] {
				continue
			}
			seen[j] = true
			pi.links = append(pi.links, j)
			if !sp.items[j].exists {
				if j == i {
					sp.fail(pi, apierr.New(apierr.CodeRefCycle,
						"references itself (the $ref at %s), and it does not exist yet, so it has no id to point at", refAt(r)).
						WithHint("create it first without the reference, then add the reference"))
					continue
				}
				pi.deps = append(pi.deps, j)
			}
		}
		sort.Ints(pi.links)
	}
}

// sort orders the items so that every document is created before the items
// whose references need its id.
func (sp *syncPlan) sort() error {
	deps := map[int][]int{}
	for i, pi := range sp.items {
		deps[i] = pi.deps
	}
	order, cycle, ok := manifest.Order(len(sp.items), deps)
	if ok {
		sp.order = order
		return nil
	}
	names := make([]string, 0, len(cycle))
	for _, i := range cycle {
		names = append(names, sp.items[i].it.Key)
	}
	e := apierr.New(apierr.CodeRefCycle,
		"the items reference each other in a cycle, and a document in it does not exist yet: %s",
		strings.Join(names, " → ")).
		WithHint("no order gives every item the id it needs. Create one side first without the reference (a separate manifest), sync, then add the reference.")
	for _, i := range cycle {
		if sp.items[i].err == nil {
			sp.items[i].err = e
		}
	}
	sp.problems = append(sp.problems, manifest.Problem{Where: "plan order", Code: e.Code, Message: e.Message, Hint: e.Hint})
	return sp.problems.Err()
}

// decideItem resolves the item's references (checked against relationTo),
// puts masked rich text / json values back from the stored document, and
// decides the item's action. Plan runs it once; sync again, with real ids,
// for an item whose references point at documents it just created.
func (sp *syncPlan) decideItem(ctx context.Context, pi *planItem) error {
	scope := newRefScope(pi.shard())
	body, err := sp.resolver.replace(ctx, pi.data, pi.params.Locale, scope)
	if err != nil {
		return err
	}
	if len(scope.unchecked) > 0 {
		sp.refUnchecked = appendMissing(sp.refUnchecked, pi.it.Key)
		sp.refUncheckedPaths = appendMissing(sp.refUncheckedPaths, scope.unchecked...)
	}
	var current map[string]any
	if pi.exists {
		current = pi.current
	}
	if pi.maskedWhole {
		var removed []string
		body, _, removed = restoreMasked(current, body, wholesaleValues(pi.shard()))
		if len(removed) > 0 {
			sp.maskRemoved = appendMissing(sp.maskRemoved, pi.it.Key)
			sp.maskRemovedPaths = appendMissing(sp.maskRemovedPaths, removed...)
		}
	}
	pi.body = body
	pi.dc = decide(current, body, pi.drafts(), pi.it.Status, diffOptions(pi.shard(), sp.opts.ignore, sp.opts.full), false)
	return nil
}

// appendMissing appends the values dst does not hold yet.
func appendMissing(dst []string, values ...string) []string {
	for _, v := range values {
		if !containsString(dst, v) {
			dst = append(dst, v)
		}
	}
	return dst
}

// plan resolves the item's references and decides its action.
func (sp *syncPlan) plan(ctx context.Context, d *Deps, pi *planItem) {
	if err := sp.decideItem(ctx, pi); err != nil {
		sp.fail(pi, err)
		return
	}
	body := pi.body
	var current map[string]any
	if pi.exists {
		current = pi.current
	}
	if pi.dc.action == actionCreate {
		if err := uploadCreateError(pi.t, "the item"); err != nil {
			sp.fail(pi, err)
			return
		}
	}
	if paths := redactPlanDiff(d, pi.dc.diff); len(paths) > 0 {
		pi.warnings = append(pi.warnings, output.DataRedactedWarning(paths))
	}
	if pi.dc.action == actionUnchanged {
		// Nothing will be written: advice about the body (block types,
		// unknown fields) would be about a write that does not happen.
		return
	}
	t := pi.t
	if pi.g != nil {
		t = pi.g.asCollTarget()
	}
	if pi.exists {
		// §9.12.8.3: a string over an editor state, an object over an array.
		o := orphanTarget{t: pi.t, g: pi.g, id: pi.idString}
		if err := storedShapeError("the item's \"data\"", o, body, current); err != nil {
			sp.fail(pi, err)
			return
		}
	}
	// Advice about the rows the write SENDS: an update that leaves the
	// blocks field alone has nothing to check there.
	pi.warnings = append(pi.warnings, blockTypeWarningsWith(d, t, pi.shard(), pi.dc.writeBody(), current)...)
	// Checked on the data, where a reference is still a {"$ref"} object
	// and a placeholder id cannot pass for a slug.
	if w := d.relationIDWarning(pi.data); w != nil {
		pi.warnings = append(pi.warnings, *w)
	}
	if pi.exists {
		for _, w := range bodyWarnings(current, body, nil, pi.slug(), pi.g != nil) {
			if w.Code == WarnFieldNotOnDocument {
				pi.warnings = append(pi.warnings, w)
			}
		}
		pi.preview = d.planOrphanRisk(pi)
		stored, warns, preview := d.pendingDraftCheck(ctx, orphanTarget{t: pi.t, g: pi.g, id: pi.idString},
			current, pi.dc, pi.it.Status, pi.params)
		pi.storedStatus = stored
		pi.warnings = append(pi.warnings, warns...)
		pi.preview = append(pi.preview, preview...)
	}
}

// planOrphanRisk is §10.2.1's pre-write prediction for an update item, from
// the state the plan already read (0 requests): the newest version, which is
// also what a document without a pending draft stores — the same
// approximation the backup-based check of a real write makes.
func (d *Deps) planOrphanRisk(pi *planItem) []output.Warning {
	if pi.dc.action != actionUpdate || pi.current == nil {
		return nil
	}
	o := orphanTarget{t: pi.t, g: pi.g, id: pi.idString}
	body, p := pi.dc.writeBody(), pi.dc.writeParams(pi.params)
	a := orphans.Analyze(pi.current, docdiff.ApplyPatch(pi.current, body, docdiff.Options{}), nil, d.censusAccepts(o))
	if len(a.Risks) == 0 {
		return nil
	}
	draftSave := isPlainDraftSave(p, body)
	fix := "after the sync, `" + o.resetCmd(" --dry-run") + "` plans the id-preserving repair"
	if draftSave && pi.g == nil {
		fix = "pay publish " + pi.t.Slug + " " + shellQuote(pi.idString) + " --reset-nested --dry-run   # publish this way to remove them"
	}
	return []output.Warning{orphanRiskWarning(o, a, draftSave, fix)}
}

// ---------------------------------------------------------------------------
// rendering
// ---------------------------------------------------------------------------

// planItemOut is one item of data.items.
type planItemOut struct {
	Key           string           `json:"key"`
	Source        string           `json:"source"`
	Collection    string           `json:"collection,omitempty"`
	Global        string           `json:"global,omitempty"`
	Locale        string           `json:"locale,omitempty"`
	Match         map[string]any   `json:"match,omitempty"`
	Action        string           `json:"action"`
	ID            any              `json:"id"`
	Status        string           `json:"status"`
	CurrentStatus any              `json:"current_status"`
	StoredStatus  string           `json:"stored_status,omitempty"`
	WriteMode     string           `json:"write_mode,omitempty"`
	DependsOn     []string         `json:"depends_on"`
	Fields        []string         `json:"fields"`
	Diff          map[string]any   `json:"diff,omitempty"`
	Result        string           `json:"result,omitempty"`
	Error         *itemErrorOut    `json:"error,omitempty"`
	Reason        string           `json:"reason,omitempty"`
	Warnings      []output.Warning `json:"warnings,omitempty"`
}

// itemErrorOut is a failed item's error, compact.
type itemErrorOut struct {
	Code    apierr.Code    `json:"code"`
	Exit    int            `json:"exit"`
	Message string         `json:"message"`
	Hint    string         `json:"hint,omitempty"`
	Fields  []apierr.Field `json:"fields,omitempty"`
}

// planSummary counts the items by action (and, after a sync, by result).
type planSummary struct {
	Items        int  `json:"items"`
	Create       int  `json:"create"`
	Update       int  `json:"update"`
	Unchanged    int  `json:"unchanged"`
	Created      *int `json:"created,omitempty"`
	Updated      *int `json:"updated,omitempty"`
	Failed       *int `json:"failed,omitempty"`
	NotAttempted *int `json:"not_attempted,omitempty"`
}

// planData is `pay plan`'s data (and `pay sync --dry-run`'s).
type planData struct {
	DryRun      *bool          `json:"dry_run,omitempty"`
	Summary     planSummary    `json:"summary"`
	Items       []planItemOut  `json:"items"`
	WouldBackup *backupPreview `json:"would_backup,omitempty"`
}

func (sp *syncPlan) itemOut(pi *planItem) planItemOut {
	out := planItemOut{
		Key: pi.it.Key, Source: pi.it.Label(), Locale: pi.it.Locale,
		Collection: pi.it.Collection, Global: pi.it.Global,
		Match: pi.matchedBy, Action: pi.dc.action, ID: pi.id,
		Status: string(pi.it.Status), DependsOn: []string{}, Fields: pi.dc.fields,
		Result: pi.result, Reason: pi.reason, Warnings: pi.warnings,
	}
	if out.Fields == nil {
		out.Fields = []string{}
	}
	if out.Action == "" {
		// The item never got a decision: it failed to plan (error) or
		// references one that did (reason).
		out.Action = actionNotPlanned
		if pi.err != nil {
			out.Action = actionError
		}
	}
	if pi.current != nil {
		if st, ok := pi.current["_status"]; ok {
			out.CurrentStatus = st
		}
	}
	out.StoredStatus = pi.storedStatus
	if pi.dc.action != actionUnchanged && pi.dc.action != "" {
		out.WriteMode = pi.dc.mode
	}
	for _, j := range pi.links {
		out.DependsOn = append(out.DependsOn, sp.items[j].it.Key)
	}
	if pi.dc.diff != nil {
		// The same data.diff shape as every other dry run (orphans.go),
		// with the change list dropped under --brief.
		out.Diff = docDiffData(diffBaseLatest, *pi.dc.diff, pi.current, pi.g != nil, sp.opts.brief)
	}
	if pi.err != nil {
		out.Error = &itemErrorOut{Code: pi.err.Code, Exit: pi.err.Exit, Message: pi.err.Message, Hint: pi.err.Hint, Fields: pi.err.Fields}
	}
	return out
}

// sequence is the items in execution order, or in load order when the plan
// never got as far as ordering them.
func (sp *syncPlan) sequence() []*planItem {
	if len(sp.order) == len(sp.items) {
		out := make([]*planItem, 0, len(sp.order))
		for _, i := range sp.order {
			out = append(out, sp.items[i])
		}
		return out
	}
	return sp.items
}

func (sp *syncPlan) data() planData {
	out := planData{Items: []planItemOut{}}
	for _, pi := range sp.sequence() {
		out.Items = append(out.Items, sp.itemOut(pi))
		out.Summary.Items++
		switch pi.dc.action {
		case actionCreate:
			out.Summary.Create++
		case actionUpdate:
			out.Summary.Update++
		case actionUnchanged:
			out.Summary.Unchanged++
		}
	}
	return out
}

// warnings lifts the item warnings to the envelope, each prefixed with its
// item key, plus the one body_status_ignored summary.
func (sp *syncPlan) warnings() []output.Warning {
	var out []output.Warning
	if len(sp.statusIgnored) > 0 {
		out = append(out, statusIgnoredWarning(sp.statusIgnored))
	}
	if len(sp.masked) > 0 {
		out = append(out, maskedWarning(sp.maskedPaths, sp.masked))
	}
	if len(sp.maskRemoved) > 0 {
		out = append(out, maskRemovedWarning(sp.maskRemovedPaths, sp.maskRemoved))
	}
	if len(sp.refUnchecked) > 0 {
		out = append(out, *refUncheckedWarning(sp.refUncheckedPaths, sp.refUnchecked))
	}
	for _, pi := range sp.sequence() {
		ws := pi.warnings
		if !sp.executed {
			ws = append(append([]output.Warning(nil), ws...), pi.preview...)
		}
		for _, w := range ws {
			w.Message = "[" + pi.it.Key + "] " + w.Message
			out = append(out, w)
		}
	}
	return out
}

// tableRows is the --output table rendering: one line per item.
func (sp *syncPlan) tableRows(withResult bool) ([]map[string]any, []string) {
	rows := []map[string]any{}
	for _, pi := range sp.sequence() {
		target := pi.it.Target()
		if pi.id != nil {
			target = fmt.Sprintf("%s %v", target, pi.id)
		}
		row := map[string]any{
			"action": pi.dc.action, "key": pi.it.Key, "target": target,
			"mode": pi.dc.mode, "fields": strings.Join(pi.dc.fields, ","),
		}
		if pi.dc.action == actionUnchanged {
			row["mode"] = ""
		}
		if withResult {
			res := pi.result
			if pi.err != nil {
				res += " (" + string(pi.err.Code) + ")"
			}
			row["result"] = res
		}
		rows = append(rows, row)
	}
	cols := []string{"action", "key", "target", "mode", "fields"}
	if withResult {
		cols = append(cols, "result")
	}
	return rows, cols
}

// planArgs renders the manifest arguments for a next.cmd.
func planArgs(paths []string) string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, shellQuote(p))
	}
	return strings.Join(out, " ")
}
