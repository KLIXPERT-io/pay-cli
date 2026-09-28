package cli

import (
	"context"
	"fmt"

	"github.com/KLIXPERT-io/pay-cli/internal/manifest"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
)

// ---------------------------------------------------------------------------
// §10.2.3 upsert / plan / sync over a published document with a pending draft
// ---------------------------------------------------------------------------
//
// upsert and sync decide the write mode from the NEWEST version (decideMode):
// the match reads with draft=true, so a published document with a pending
// draft reads back as a draft. Two consequences the envelope has to say out
// loud, because the newest version alone cannot show them:
//
//   - status keep saves the change INTO the pending draft (?draft=true). That
//     is the safe choice — the live document is untouched — but an agent that
//     migrates content into live pages sees ok / "updated" and assumes the
//     site changed. It is warned saved_as_draft, on the preview and on the
//     real write (a draft write never reaches the §10.2.3 guard).
//   - status published publishes the pending draft together with the change.
//     The real write warns publishes_pending_draft through the ordinary
//     update path; `pay plan` and `pay sync --dry-run` never reach that path,
//     so the plan warns it itself — the review step must show what the write
//     will do.
//
// Both need the stored (main) row's `_status`, one extra `select=_status`
// read, made only for an update whose newest version is a draft.

// WarnSavedAsDraft says a write with status keep was saved into the pending
// draft of a published document: the live document did not change.
const WarnSavedAsDraft = "saved_as_draft"

// readStoredStatus reads the stored (main) row's `_status` (depth 0, select
// _status, no draft): the published state the newest version hides.
func (d *Deps) readStoredStatus(ctx context.Context, client *payload.Client, o orphanTarget, p query.Params) (string, error) {
	rp := query.Params{Depth: query.IntPtr(0), Locale: p.Locale, FallbackLocale: p.FallbackLocale,
		Select: []string{"_status"}}
	var (
		doc payload.Doc
		err error
	)
	if o.g != nil {
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

// pendingDraftCheck is what an update decided from current (the newest
// version) needs to know about the stored row. It returns the stored row's
// status ("" when it was not read), the warnings of the write itself
// (saved_as_draft) and the warnings only a preview that bypasses the update
// path must add (publishes_pending_draft). A failed read is no warning: the
// real write's own guard still runs for a publish, and a draft save cannot
// take anything offline.
func (d *Deps) pendingDraftCheck(ctx context.Context, o orphanTarget, current map[string]any, dc decision,
	status manifest.Status, p query.Params) (stored string, warns, preview []output.Warning) {
	if dc.action != actionUpdate || current == nil || statusOfDoc(current) != "draft" {
		return "", nil, nil
	}
	if dr := o.drafts(); dr != nil && !*dr {
		return "", nil, nil
	}
	keepDraft := status == manifest.StatusKeep && dc.mode == modeDraft
	if !keepDraft && dc.mode != modePublish {
		return "", nil, nil
	}
	client, err := d.requireClient()
	if err != nil {
		return "", nil, nil
	}
	stored, err = d.readStoredStatus(ctx, client, o, p)
	if err != nil || stored != "published" {
		return stored, nil, nil
	}
	if keepDraft {
		return stored, []output.Warning{savedAsDraftWarning(o)}, nil
	}
	w, _ := publicationVerdict(o, "published", pubState{stored: stored, latest: "draft"})
	if w != nil {
		preview = append(preview, *w)
	}
	return stored, nil, preview
}

// savedAsDraftWarning is saved_as_draft for o.
func savedAsDraftWarning(o orphanTarget) output.Warning {
	return output.Warning{
		Code: WarnSavedAsDraft,
		Message: fmt.Sprintf("%s is published and has a pending draft, so the default status \"keep\" saves this change "+
			"into that draft (?draft=true): the live document does not change", o.what()),
		Hint: "to take it live, write with --publish (a manifest item: \"status\": \"published\"), which also publishes " +
			"every other change of the pending draft; " + o.draftVsPublishedCmd() + " shows them",
	}
}
