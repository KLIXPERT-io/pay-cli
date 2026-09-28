package cli

import (
	"context"
	"fmt"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/docdiff"
	"github.com/KLIXPERT-io/pay-cli/internal/orphans"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
)

// ---------------------------------------------------------------------------
// §10.2.3 a non-draft write never unpublishes by accident
// ---------------------------------------------------------------------------
//
// payload@3.87.1 collections/operations/updateByID.js l.79 merges the body of
// an update-by-id into getLatestCollectionVersion — the NEWEST version, which
// is the pending draft when there is one — and, without ?draft
// (utilities/update.js l.29 isSavingDraft=false), stores the merge as the main
// row (l.253-262 db.updateOne). The global update does the same
// (globals/operations/update.js getLatestGlobalVersion). So on a published
// document with a pending draft, `pay update <c> <id> --set x=y` without a
// flag writes the whole draft into the live row AND its `_status: "draft"`:
// the page goes offline. A body that carries `_status: "draft"` (every
// `pay get --draft` export of a draft does) does the same on a published
// document without a pending draft. Verified on localhost:3900 (pages 63, 67,
// 68 of the wave-B review): the stored row went from published to draft and
// the site answered 404.
//
// PayCLI therefore reads the publication state (the stored row's and the
// newest version's `_status`, two `select=_status` reads; a dry run reuses its
// pre-image) before a non-draft write that states no outcome, and refuses a
// write that would turn a published row into a draft unless the caller said so
// (--unpublish). --draft (edit the draft) and --publish (publish the draft with
// the change) are the two other explicit choices. A write that publishes on
// top of a pending draft is allowed and warned about (publishes_pending_draft):
// visitors get the draft's changes too.
//
// Bulk `update --where` is not affected: payload's bulk update merges into the
// stored row when ?draft is absent (collections/operations/update.js l.100-127
// db.find), so it never takes a pending draft's _status.

// WarnPublishesPendingDraft says a write that publishes also publishes the
// document's pending draft (whose changes the caller may not have made).
const WarnPublishesPendingDraft = "publishes_pending_draft"

// pubState is the publication state of one document before a write: the
// `_status` of the stored (main) row and of the newest version ("" when the
// entity has no _status).
type pubState struct {
	stored, latest string
}

func pubStateOf(pre *preimage) pubState {
	if pre == nil {
		return pubState{}
	}
	return pubState{stored: statusOfDoc(pre.stored), latest: statusOfDoc(pre.latest)}
}

// pendingDraft is whether a published document has a newer draft: the newest
// version is a draft. Payload's draft read answers with the stored row itself
// when no draft newer than it exists (versions/drafts/
// replaceWithDraftIfAvailable.js), so a published document without a pending
// draft reads back published both ways (verified on klixpert stage: 22
// published pages, `pay diff --draft-vs-published` identical).
func (s pubState) pendingDraft() bool {
	return s.stored == "published" && s.latest == "draft"
}

// needsPubCheck is whether a write's effect on the publication state has to be
// checked: a non-draft write with a body, on an entity that has (or may have)
// drafts, that does not deliberately unpublish.
func needsPubCheck(o orphanTarget, body map[string]any, p query.Params, unpublish bool) bool {
	if dr := o.drafts(); dr != nil && !*dr {
		return false
	}
	if p.Draft != nil && *p.Draft {
		return false
	}
	return !unpublish && len(body) > 0
}

// readPubState reads the two `_status` values (depth 0, select _status).
func (d *Deps) readPubState(ctx context.Context, client *payload.Client, o orphanTarget, p query.Params) (pubState, error) {
	read := func(draft bool) (string, error) {
		rp := query.Params{Depth: query.IntPtr(0), Locale: p.Locale, FallbackLocale: p.FallbackLocale,
			Trash: p.Trash, Select: []string{"_status"}}
		if draft {
			rp.Draft = query.BoolPtr(true)
		}
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
			return "", err
		}
		return statusOfDoc(doc), nil
	}
	stored, err := read(false)
	if err != nil || stored != "published" {
		// Only a published row can be taken offline; the newest version
		// is not needed otherwise.
		return pubState{stored: stored}, err
	}
	latest, err := read(true)
	return pubState{stored: stored, latest: latest}, err
}

// checkPublication is the guard of a write about to be sent (or previewed):
// an error when the write would unpublish a published document the caller did
// not ask to unpublish, a warning when it publishes a pending draft. pre is
// the dry run's (or the remedy's) pre-image; nil reads the state.
func (d *Deps) checkPublication(ctx context.Context, client *payload.Client, o orphanTarget, body map[string]any,
	p query.Params, unpublish bool, pre *preimage) (*output.Warning, error) {
	if !needsPubCheck(o, body, p, unpublish) {
		return nil, nil
	}
	bodyStatus, _ := body["_status"].(string)
	var st pubState
	if pre != nil {
		st = pubStateOf(pre)
	} else {
		var err error
		st, err = d.readPubState(ctx, client, o, p)
		switch {
		case err == nil:
		case apierr.HasCode(err, apierr.CodeDocNotFound), bodyStatus == "published":
			// The write fails the same way; or it publishes, which
			// cannot take anything offline — the check is only a hint.
			return nil, nil
		default:
			return nil, apierr.Wrap(err, apierr.CodeOf(err),
				"could not read the publication state of %s before writing it: %s", o.what(), errMessage(err)).
				WithHint("a non-draft write can unpublish a published document; state the outcome with --draft, --publish or --unpublish and retry")
		}
	}
	return publicationVerdict(o, bodyStatus, st)
}

// publicationVerdict is the pure rule of checkPublication.
func publicationVerdict(o orphanTarget, bodyStatus string, st pubState) (*output.Warning, error) {
	if st.stored != "published" {
		return nil, nil
	}
	switch bodyStatus {
	case "published":
		if !st.pendingDraft() {
			return nil, nil
		}
		return &output.Warning{
			Code: WarnPublishesPendingDraft,
			Message: fmt.Sprintf("%s has a pending draft: Payload merges this write into the newest version (the draft), "+
				"so publishing also publishes every change of that draft, not only the fields this write sends", o.what()),
			Hint: o.draftVsPublishedCmd() + "   # what the pending draft changes",
		}, nil
	case "draft":
		return nil, apierr.New(apierr.CodeInvalidArgs,
			"the body sets _status \"draft\" and %s is published: written without --draft, that body replaces the live "+
				"row and UNPUBLISHES it (a `pay get --draft` export carries _status \"draft\")", o.what()).
			WithHint("%s", o.unpublishChoices())
	case "":
		if !st.pendingDraft() {
			return nil, nil
		}
		return nil, apierr.New(apierr.CodeInvalidArgs,
			"%s is published and has a pending draft: a write without --draft is merged into that draft and stored as the "+
				"live row, which publishes the draft's changes AND sets _status \"draft\" — it would take the published "+
				"document offline", o.what()).
			WithHint("%s; %s shows the pending draft", o.unpublishChoices(), o.draftVsPublishedCmd())
	}
	return nil, nil
}

// unpublishChoices names the three explicit outcomes.
func (o orphanTarget) unpublishChoices() string {
	return "say what you want: --draft edits the draft (the live document stays as it is), --publish publishes the " +
		"draft together with this change, --unpublish takes the document offline deliberately"
}

// draftVsPublishedCmd previews a pending draft.
func (o orphanTarget) draftVsPublishedCmd() string {
	if o.g != nil {
		return "pay diff --global " + o.g.Slug + " --draft-vs-published"
	}
	return "pay diff " + o.t.Slug + " " + shellQuote(o.id) + " --draft-vs-published"
}

// storedDiffers is whether the stored row holds something the newest version
// does not — a pending draft, in content or in status. The stored row's {}
// rows do not count: Payload builds every version from the stored row read
// back, in which they have no blockType and are dropped (§10.2.1.1), so they
// are never in the newest version and are no sign of a draft.
func storedDiffers(pre *preimage) bool {
	if pre == nil {
		return false
	}
	if statusOfDoc(pre.stored) != statusOfDoc(pre.latest) {
		return true
	}
	return !docdiff.Diff(orphans.WithoutEmptyRows(map[string]any(pre.stored)), map[string]any(pre.latest),
		docdiff.Options{IgnorePaths: []string{"_status"}}).Identical
}

// checkStatusFlags refuses contradictory publication flags before anything is
// read or sent — the ONE rule `pay update`, `pay apply` and
// `pay globals update` share, so the three cannot drift apart.
//
// --draft with --unpublish is refused like --draft with --publish: the write
// goes out with ?draft=true, and Payload then saves a draft VERSION and
// leaves the stored row — the live, published document — untouched
// (verified on localhost:3900: the echo said `_status: "draft"` while the
// page stayed published). An agent that asked to take a page offline must
// not be told it did.
func checkStatusFlags(draft, publish, unpublish bool) error {
	if publish && unpublish {
		return apierr.New(apierr.CodeInvalidArgs, "--publish and --unpublish are mutually exclusive")
	}
	if draft && publish {
		return apierr.New(apierr.CodeInvalidArgs,
			"--draft and --publish contradict each other: a write with _status \"published\" is never saved as a draft").
			WithHint("pick one: --draft saves a draft, --publish stores and publishes")
	}
	if draft && unpublish {
		return apierr.New(apierr.CodeInvalidArgs,
			"--draft and --unpublish contradict each other: with --draft the write only saves a draft version and "+
				"the published document stays live").
			WithHint("pick one: --unpublish takes the live document offline (it stores _status \"draft\"); " +
				"--draft only saves a draft version and leaves the live one as it is")
	}
	return nil
}

// checkVersionRestore is §10.2.3 for `pay versions restore` on a collection.
//
// payload@3.87.1 collections/operations/restoreVersion.js l.206-213 writes the
// version's content into the stored (main) row unless ?draft is set, and it
// keeps the VERSION's `_status` (`result._status = draftArg ? 'draft' :
// result._status`). Restoring a draft version without --draft therefore
// stores `_status: "draft"` in the live row: a published document goes
// offline (verified on localhost:3900 — `pay get --published-only` answered
// not_published after the restore). The guard refuses that unless the caller
// says so with --unpublish; --draft restores the content as a new draft on
// top and leaves the live document as it is.
//
// --unpublish on a PUBLISHED version is refused too: the restore stores that
// version as published, so the flag would promise something the write does
// not do.
//
// Globals are not checked: globals/operations/restoreVersion.js always
// rewrites the stored global (with --draft too), and a global has no
// "offline" — its stored row is what every read returns.
func (d *Deps) checkVersionRestore(ctx context.Context, client *payload.Client, scope versionScope, versionID string,
	draft, unpublish bool, readVersion func() (payload.Doc, error)) error {
	if scope.global != nil || scope.coll == nil || draft {
		return nil
	}
	if dr := scope.coll.flags().Drafts; dr != nil && !*dr {
		if unpublish {
			return apierr.New(apierr.CodeInvalidArgs,
				"--unpublish has nothing to do: %q has no drafts, so a restore never changes a publication state", scope.coll.Slug).
				WithHint("drop --unpublish")
		}
		return nil
	}
	ver, err := readVersion()
	if err != nil {
		if apierr.HasCode(err, apierr.CodeDocNotFound) {
			return nil // the restore itself answers doc_not_found
		}
		return apierr.Wrap(err, apierr.CodeOf(err),
			"could not read version %s before restoring it: %s", versionID, errMessage(err)).
			WithHint("restoring a draft version without --draft unpublishes a published document; pass --draft " +
				"(restore as a new draft, the live document stays) or --unpublish (take it offline deliberately) and retry")
	}
	content, _ := ver["version"].(map[string]any)
	verStatus := statusOfDoc(content)
	parent := ver["parent"]
	if obj, ok := parent.(map[string]any); ok {
		parent = obj["id"]
	}
	o := orphanTarget{t: scope.coll, id: fmt.Sprint(parent)}
	if verStatus != "draft" {
		if unpublish && verStatus == "published" {
			return apierr.New(apierr.CodeInvalidArgs,
				"--unpublish contradicts version %s: it is a published version, and restoring it stores %s as published",
				versionID, o.what()).
				WithHint("drop --unpublish; to take the document offline after the restore, run pay unpublish %s %s",
					scope.coll.Slug, shellQuote(o.id))
		}
		return nil
	}
	if unpublish || parent == nil {
		return nil
	}
	st, err := d.readPubState(ctx, client, o, query.Params{})
	switch {
	case err != nil && apierr.HasCode(err, apierr.CodeDocNotFound):
		return nil
	case err != nil:
		return apierr.Wrap(err, apierr.CodeOf(err),
			"could not read the publication state of %s before restoring version %s: %s", o.what(), versionID, errMessage(err)).
			WithHint("pass --draft (restore as a new draft, the live document stays) or --unpublish (take it offline deliberately) and retry")
	}
	if st.stored != "published" {
		return nil
	}
	return apierr.New(apierr.CodeInvalidArgs,
		"version %s is a draft and %s is published: restored without --draft, Payload writes the version with its "+
			"_status \"draft\" into the live row and UNPUBLISHES the document", versionID, o.what()).
		WithHint("say what you want: --draft restores it as a new draft on top (the live document stays as it is; "+
			"pay publish %s %s publishes it later), --unpublish restores it into the live row and takes the document offline deliberately",
			scope.coll.Slug, shellQuote(o.id))
}
