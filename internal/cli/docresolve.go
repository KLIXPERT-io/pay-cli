package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
)

// ---------------------------------------------------------------------------
// Resolving a document by slug (F9 — shared by `pay get --slug` and
// `pay url --slug`; any command that addresses ONE document may reuse it).
// ---------------------------------------------------------------------------

// DefaultSlugField is the field ResolveDocBySlug matches when SlugLookup.Field
// is empty: the Payload website template's (and the slug field plugin's) name.
const DefaultSlugField = "slug"

// SlugLookup says how ResolveDocBySlug searches.
type SlugLookup struct {
	// Field is the field to match; "" means DefaultSlugField.
	Field string
	// Draft matches against each document's NEWEST draft (?draft=true). A
	// slug that was changed in an unpublished draft is found only this way;
	// without it the match runs against the stored (published, or
	// never-published) state, exactly like a bare `pay find`.
	Draft bool
	// Trash includes soft-deleted documents.
	Trash bool
	// Locale matches a localized slug in this locale ("" = the default).
	Locale string
}

// DocRef is the one document a slug resolved to.
type DocRef struct {
	// ID is the id exactly as Payload returned it (a JSON number on Postgres,
	// a string on MongoDB), for envelopes.
	ID any
	// IDString is the same id rendered for a URL path or a next.cmd.
	IDString string
	// Slug is the value that matched.
	Slug string
}

// ResolveDocBySlug finds exactly one document of collection t whose slug field
// equals slug, in one bounded request (limit 2, id-only select):
//
//   - no match      -> doc_not_found (exit 4), with a hint that names --draft
//     when the lookup did not use it (the slug may exist only in a draft);
//   - several       -> slug_ambiguous (exit 5), with the matching ids in
//     error.did_you_mean;
//   - exactly one   -> its DocRef.
//
// It validates the slug field against the collection's field shard when one is
// known (a Payload `where` on an unknown field is a 400 QueryError, or worse a
// silent match-all on some adapters), and never guesses a field that is not
// there. Callers pass the same --draft/--trash/--locale they will use for the
// follow-up read, so the id they get is the document they will then read.
func (d *Deps) ResolveDocBySlug(ctx context.Context, t *collTarget, slug string, o SlugLookup) (*DocRef, error) {
	field := o.Field
	if field == "" {
		field = DefaultSlugField
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, apierr.New(apierr.CodeInvalidArgs, "--slug needs a value").
			WithHint("pass the document's slug, e.g. --slug about; list them with: pay find %s --select id,%s --limit 20", t.Slug, field)
	}
	if t.Shard != nil && len(t.Shard.Fields) > 0 && !containsString(t.Shard.Paths(), field) {
		return nil, apierr.New(apierr.CodeUnknownField,
			"%q has no %q field, so it cannot be addressed by slug", t.Slug, field).
			WithHint("address the document by id instead: pay find %s --select id --limit 20", t.Slug)
	}
	if o.Draft {
		if err := checkDrafts(t.Slug, t.flags().Drafts); err != nil {
			return nil, err
		}
	}
	if o.Trash {
		if err := checkTrash(t.Slug, t.flags().Trash); err != nil {
			return nil, err
		}
	}
	list, err := d.lookupDocs(ctx, t, query.Term(field, "equals", slug), o, []string{field})
	if err != nil {
		return nil, err
	}
	switch {
	case len(list.Docs) == 0:
		e := apierr.New(apierr.CodeDocNotFound, "no %s document has %s %q", t.Slug, field, slug)
		switch {
		case !o.Draft && t.flags().Drafts != nil && *t.flags().Drafts:
			e = e.WithHint("a slug changed in an unpublished draft is only found with --draft; "+
				"list the stored slugs with: pay find %s --select id,%s --limit 50", t.Slug, field)
		default:
			e = e.WithHint("list the slugs that exist: pay find %s --select id,%s --limit 50", t.Slug, field)
		}
		return nil, e
	case len(list.Docs) > 1:
		return nil, ambiguousLookup(apierr.CodeSlugAmbiguous, t, list, o.Draft,
			fmt.Sprintf("have %s %q; this command acts on exactly one", field, slug),
			"address one by id (candidates in error.did_you_mean), or see them all",
			"--where "+shellQuote(fmt.Sprintf("%s eq %s", field, slug)), field)
	}
	doc := list.Docs[0]
	return &DocRef{ID: doc.ID(), IDString: doc.IDString(), Slug: slug}, nil
}

// lookupDocs is the one bounded lookup behind every "find THE document that
// …" in PayCLI: ResolveDocBySlug, `pay upsert --match`, a sync manifest's
// match and every {"$ref": …} (F6). It asks for at most two documents at depth
// 0 — enough to tell none, one and several apart — with the lookup's draft,
// trash and locale. sel narrows the returned documents (nil = whole
// documents, which upsert and sync diff against).
func (d *Deps) lookupDocs(ctx context.Context, t *collTarget, where query.Where, o SlugLookup, sel []string) (*payload.ListResult, error) {
	client, err := d.requireClient()
	if err != nil {
		return nil, err
	}
	p := query.Params{
		Where:  where,
		Limit:  query.IntPtr(2),
		Depth:  query.IntPtr(0),
		Select: sel,
		Locale: o.Locale,
	}
	if o.Draft {
		p.Draft = query.BoolPtr(true)
	}
	if o.Trash {
		p.Trash = query.BoolPtr(true)
	}
	return client.Find(ctx, t.Slug, p,
		payload.WithClassify(classifyFor(t, d.cfg(), nil)), payload.WithKnownRoute())
}

// ambiguousLookup is the error every exactly-one lookup raises when it found
// several documents — slug_ambiguous, match_ambiguous and ref_ambiguous share
// one shape so an agent handles them alike: the total in the message, the
// ids in error.did_you_mean, and a runnable `pay find` that lists them all.
// what completes "<n> <collection> documents …"; filter is the ready-quoted
// filter flags (`--where 'slug eq x'`), field the column to select besides
// id, and draft whether the lookup read newest drafts (the listing then must
// too).
func ambiguousLookup(code apierr.Code, t *collTarget, list *payload.ListResult, draft bool, what, advice, filter, field string) *apierr.Error {
	total := list.TotalDocs
	if total < len(list.Docs) {
		total = len(list.Docs)
	}
	ids := make([]string, 0, len(list.Docs))
	for _, doc := range list.Docs {
		ids = append(ids, doc.IDString())
	}
	find := fmt.Sprintf("pay find %s %s --select id,%s", t.Slug, filter, field)
	if draft {
		find += " --draft"
	}
	return apierr.New(code, "%d %s documents %s", total, t.Slug, what).
		WithDidYouMean(ids...).
		WithHint("%s: %s", advice, find)
}
