package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/lexical"
	"github.com/KLIXPERT-io/pay-cli/internal/manifest"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
)

// ---------------------------------------------------------------------------
// F6 — {"$ref": …} in write bodies
// ---------------------------------------------------------------------------
//
// A migration writes documents that point at each other: a page's parent, a
// post's categories, a CTA link to another page. Before references, the agent
// had to look every id up by hand (`pay find … --where 'slug eq x'`), keep an
// ids.json state file, and hope nothing was recreated in between. Every write
// body — create, update, globals update, upload, upsert, a sync manifest and
// the body form of `pay diff` — may now name a document instead:
//
//	{"$ref": "pages", "slug": "about"}                        → 12
//	{"$ref": "pages", "slug": "about", "$as": "polymorphic"}  → {"relationTo":"pages","value":12}
//	{"$ref": "posts", "where": {"title": {"equals": "x"}}}    → 7
//	{"$ref": "@home"}                                         → the id of plan item "home" (plan/sync only)
//
// The grammar lives in internal/manifest (pure); this file resolves it
// against the server with the same bounded lookup `--slug` uses
// (lookupDocs): exactly one match becomes the id; none is ref_unresolved
// (exit 4); several is ref_ambiguous (exit 5, ids in did_you_mean — the
// slug_ambiguous shape). Lookups read the newest draft on a drafts-enabled
// collection (a draft-only document is a valid relationship target) in the
// write's locale, and are cached for the invocation.

// lookupSpec is "the document of t whose fields equal these values (and that
// matches where)".
type lookupSpec struct {
	t      *collTarget
	terms  []manifest.Term
	where  map[string]any
	locale string
}

// queryWhere is the spec as one Payload where object.
func (s lookupSpec) queryWhere() query.Where {
	parts := make([]query.Where, 0, len(s.terms)+1)
	for _, term := range s.terms {
		parts = append(parts, query.Term(term.Field, "equals", term.Value))
	}
	if s.where != nil {
		parts = append(parts, query.Where(s.where))
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return query.And(parts...)
}

// describe renders the criteria for a message: slug="about", locale="de".
func (s lookupSpec) describe() string {
	parts := make([]string, 0, len(s.terms)+1)
	for _, term := range s.terms {
		if str, ok := term.Value.(string); ok {
			parts = append(parts, fmt.Sprintf("%s=%q", term.Field, str))
			continue
		}
		parts = append(parts, term.Field+"="+manifest.ScalarString(term.Value))
	}
	if s.where != nil {
		b, _ := json.Marshal(s.where)
		parts = append(parts, "where "+string(b))
	}
	return strings.Join(parts, ", ")
}

// filterArgs renders the spec as `pay find` filter flags, ready to paste.
func (s lookupSpec) filterArgs() string {
	if s.where != nil {
		b, _ := json.Marshal(s.queryWhere())
		return "--where-json " + shellQuote(string(b))
	}
	parts := make([]string, 0, len(s.terms))
	for _, term := range s.terms {
		parts = append(parts, "--where "+shellQuote(term.Field+" eq "+manifest.ScalarString(term.Value)))
	}
	return strings.Join(parts, " ")
}

// selectField is the column a listing of candidates shows besides id.
func (s lookupSpec) selectField() string {
	if len(s.terms) > 0 {
		return s.terms[0].Field
	}
	return "updatedAt"
}

// flagOn reads a tri-state capability flag as "known to be enabled".
func flagOn(b *bool) bool { return b != nil && *b }

// lookupMatches runs the bounded lookup for spec: at most two documents at
// depth 0, the newest draft where the collection has drafts. full returns
// whole documents (upsert and sync diff against them); otherwise only ids.
// Field names are checked against the collection's schema first, because a
// where on an unknown field is a 400 at best and a silent match-all at worst.
func (d *Deps) lookupMatches(ctx context.Context, s lookupSpec, full bool) (*payload.ListResult, error) {
	if s.t.Shard != nil && len(s.t.Shard.Fields) > 0 {
		paths := s.t.Shard.Paths()
		for _, term := range s.terms {
			if term.Field == "id" || containsString(paths, term.Field) {
				continue
			}
			return nil, apierr.New(apierr.CodeUnknownField,
				"%q is not a field of %s, so no document can be matched on it", term.Field, s.t.Slug).
				WithDidYouMean(apierr.DidYouMean(term.Field, paths)...).
				WithHint("`pay describe %s` lists its fields", s.t.Slug)
		}
	}
	if s.where != nil {
		if err := query.ValidateWhere(query.Where(s.where), s.t.schema(d.cfg())); err != nil {
			return nil, err
		}
	}
	var sel []string
	if !full {
		sel = []string{"id"}
	}
	return d.lookupDocs(ctx, s.t, s.queryWhere(), SlugLookup{Draft: flagOn(s.t.flags().Drafts), Locale: s.locale}, sel)
}

// refResolver turns references into values for one invocation, caching every
// server lookup: a body that links to the same page ten times costs one read.
type refResolver struct {
	d     *Deps
	cache map[string]any
	// local answers the references a plan resolves itself ("@key", and
	// equals-references that name another item of the plan): the value and
	// the collection of the document it names. handled=false sends the
	// reference to the server. nil outside a plan.
	local func(r *manifest.Ref) (value any, collection string, handled bool, err error)
}

func (d *Deps) newRefResolver() *refResolver {
	return &refResolver{d: d, cache: map[string]any{}}
}

// resolve is one reference's replacement value and the collection of the
// document it names.
func (rr *refResolver) resolve(ctx context.Context, r *manifest.Ref, locale string) (any, string, error) {
	if rr.local != nil {
		if v, coll, handled, err := rr.local(r); handled || err != nil {
			return v, coll, err
		}
	}
	if r.IsLocal() {
		return nil, "", apierr.New(apierr.CodeBadRequestBody,
			"$ref at %s is %q, a plan-local reference; those only work between the items of a `pay plan`/`pay sync` manifest",
			r.Path, manifest.LocalPrefix+r.Local).
			WithHint(`name the document instead: {"$ref":"<collection>","slug":"<slug>"}`)
	}
	t, err := rr.d.collection(ctx, r.Collection)
	if err != nil {
		return nil, "", err
	}
	spec := lookupSpec{t: t, terms: r.Equals, where: r.Where, locale: locale}
	whereJSON, _ := json.Marshal(spec.queryWhere())
	key := t.Slug + "\x00" + locale + "\x00" + string(whereJSON)
	if id, ok := rr.cache[key]; ok {
		return r.Value(t.Slug, id), t.Slug, nil
	}
	list, err := rr.d.lookupMatches(ctx, spec, false)
	if err != nil {
		return nil, "", err
	}
	switch len(list.Docs) {
	case 0:
		return nil, "", apierr.New(apierr.CodeRefUnresolved,
			"$ref at %s: no %s document has %s", refAt(r), t.Slug, spec.describe()).
			WithHint("check that it exists: pay find %s %s%s --select id,%s",
				t.Slug, spec.filterArgs(), draftFlag(flagOn(t.flags().Drafts)), spec.selectField())
	case 1:
		id := list.Docs[0].ID()
		rr.cache[key] = id
		return r.Value(t.Slug, id), t.Slug, nil
	}
	return nil, "", ambiguousLookup(apierr.CodeRefAmbiguous, t, list, flagOn(t.flags().Drafts),
		fmt.Sprintf("match the $ref at %s (%s); a reference must name exactly one", refAt(r), spec.describe()),
		"make the reference unique (another field, or a \"where\"), or write the id itself",
		spec.filterArgs(), spec.selectField())
}

// replace resolves every reference in body into a new body. scope, when
// set, checks each reference against the relationTo of the field it sits in
// (refScope.check) and collects the ones no schema could check.
func (rr *refResolver) replace(ctx context.Context, body map[string]any, locale string, scope *refScope) (map[string]any, error) {
	if scope != nil {
		scope.body = body
	}
	return manifest.Replace(body, func(r *manifest.Ref) (any, error) {
		v, coll, err := rr.resolve(ctx, r, locale)
		if err != nil {
			return nil, err
		}
		if err := scope.check(r, coll); err != nil {
			return nil, err
		}
		return v, nil
	})
}

func refAt(r *manifest.Ref) string {
	if r.Path == "" {
		return "(root)"
	}
	return r.Path
}

// resolveBodyRefs is the write verbs' one call: a body without any "$ref"
// comes back untouched and costs nothing; otherwise every reference is
// replaced (in a copy) or the write fails before anything is sent. shard is
// the written entity's schema (nil: nothing to check references against);
// the warning, when set, names the references it could not check.
func (d *Deps) resolveBodyRefs(ctx context.Context, shard *discovery.Shard, body map[string]any, locale string) (map[string]any, *output.Warning, error) {
	if !manifest.HasRefs(body) {
		return body, nil, nil
	}
	scope := newRefScope(shard)
	out, err := d.newRefResolver().replace(ctx, body, locale, scope)
	if err != nil {
		return nil, nil, err
	}
	return out, scope.warning(nil), nil
}

// ---------------------------------------------------------------------------
// the relationTo check
// ---------------------------------------------------------------------------

// WarnRefTargetUnchecked says references sit in fields whose relationTo no
// schema source knows, so a reference naming the wrong collection could not
// be caught.
const WarnRefTargetUnchecked = "ref_target_unchecked"

// refScope checks that a reference names a collection its field accepts.
// Postgres ids are small serial numbers, so the id of a posts document
// written into a categories relationship usually IS the id of some category:
// Payload stores it without complaint and the link points at an unrelated
// document. The field's relationTo says which collections it takes:
//
//   - a reference at X.value whose sibling X.relationTo is a string (a
//     polymorphic value written out, a Lexical link's fields.doc, an upload
//     or relationship node) must name that collection;
//   - otherwise the field at the reference's path — a collection field
//     (Shard.Fields), or a field of the block row it sits in
//     (Shard.BlockSchemaFor) — must list it in relation_to when that is
//     known (graphql, project source, configuration).
//
// A mismatch is bad_request_body before anything is sent. A reference inside
// rich text without a sibling relationTo, or in a field whose relationTo is
// unknown (a REST-only discovery), is collected in unchecked.
type refScope struct {
	shard     *discovery.Shard
	body      map[string]any
	unchecked []string
}

func newRefScope(shard *discovery.Shard) *refScope { return &refScope{shard: shard} }

// check is nil-safe: no scope, no check.
func (s *refScope) check(r *manifest.Ref, coll string) error {
	if s == nil || coll == "" {
		return nil
	}
	f := s.fieldAt(r.Path)
	switch {
	case f.skip:
		return nil
	case len(f.relationTo) == 0:
		if !containsString(s.unchecked, r.Path) {
			s.unchecked = append(s.unchecked, r.Path)
		}
		return nil
	case containsString(f.relationTo, coll):
		return nil
	case !f.authoritative:
		// Observed targets are the collections seen so far, not all the
		// field accepts: no proof of a mistake.
		if !containsString(s.unchecked, r.Path) {
			s.unchecked = append(s.unchecked, r.Path)
		}
		return nil
	}
	return apierr.New(apierr.CodeBadRequestBody,
		"$ref at %s names a %s document, and %s accepts only %s (%s): its id would link a document of another collection "+
			"(Postgres ids are small numbers, so it usually exists there)",
		refAt(r), coll, f.what, strings.Join(f.relationTo, ", "), f.source).
		WithHint("name a %s document in the $ref (\"$ref\":%q), or check the field: %s", strings.Join(f.relationTo, " or "), f.relationTo[0], f.describe)
}

// warning is the ref_target_unchecked warning, nil when every reference was
// checked. keys names plan items (nil for one body).
func (s *refScope) warning(keys []string) *output.Warning {
	if s == nil || len(s.unchecked) == 0 {
		return nil
	}
	return refUncheckedWarning(s.unchecked, keys)
}

func refUncheckedWarning(paths, keys []string) *output.Warning {
	w := &output.Warning{
		Code: WarnRefTargetUnchecked,
		Message: fmt.Sprintf("%d $ref(s) sit in fields whose relationTo the schema does not know (%s), so PayCLI could not check "+
			"that they name a collection the field accepts; a reference to the wrong collection links whatever document has that id there",
			len(paths), strings.Join(capPaths(paths), ", ")),
		Paths: capPaths(paths),
		Hint:  "double-check the collection named in each \"$ref\"; the check is automatic where discovery knows relationTo (GraphQL introspection)",
	}
	if len(keys) > 0 {
		w.Paths = capPaths(keys)
		w.Message = fmt.Sprintf("%d item(s) hold $refs in fields whose relationTo the schema does not know (%s), so PayCLI could not check "+
			"that they name a collection the field accepts; a reference to the wrong collection links whatever document has that id there",
			len(keys), strings.Join(capPaths(paths), ", "))
	}
	return w
}

// refField is what the schema says about the field a reference sits in.
type refField struct {
	// skip: nothing to check (inside rich text without a sibling relationTo).
	skip       bool
	relationTo []string
	// authoritative: relationTo comes from the configuration, GraphQL or
	// the project source, not from observed values.
	authoritative bool
	source        string
	what          string // "pages.parent", "the categories block's link.doc"
	describe      string // the command that shows the field
}

// fieldAt finds the field a reference at path sits in.
func (s *refScope) fieldAt(path string) refField {
	segs := splitDocPath(path)
	if len(segs) == 0 {
		return refField{skip: true}
	}
	// Walk the body along the path: the innermost block row names the
	// schema the rest of the path is relative to.
	var cur any = s.body
	var parent map[string]any
	blockType, blockAt, rich := "", -1, false
	for i, seg := range segs {
		m, isMap := cur.(map[string]any)
		if isMap {
			if lexical.IsEditorState(m) {
				rich = true
			}
			if bt, ok := m["blockType"].(string); ok && bt != "" && i > 0 && !rich {
				blockType, blockAt = bt, i
			}
		}
		if i == len(segs)-1 && isMap {
			parent = m
		}
		switch {
		case seg.index >= 0:
			arr, ok := cur.([]any)
			if !ok || seg.index >= len(arr) {
				return refField{skip: true}
			}
			cur = arr[seg.index]
		case isMap:
			cur = m[seg.key]
		default:
			return refField{skip: true}
		}
	}
	last := segs[len(segs)-1]
	if parent != nil && last.key == "value" {
		if rel, ok := parent["relationTo"].(string); ok && rel != "" {
			at := strings.TrimSuffix(path, ".value")
			return refField{relationTo: []string{rel}, authoritative: true, source: "the relationTo next to it", what: at,
				describe: "the \"relationTo\" at " + at}
		}
	}
	if rich {
		return refField{skip: true}
	}
	var keys []string
	start := 0
	if blockAt >= 0 {
		start = blockAt
	}
	for _, seg := range segs[start:] {
		if seg.index < 0 {
			keys = append(keys, seg.key)
		}
	}
	rel := strings.Join(keys, ".")
	return s.schemaField(blockType, rel)
}

// schemaField looks the field up in the shard: a collection field path, or
// a path within a block type.
func (s *refScope) schemaField(blockType, rel string) refField {
	if s.shard == nil {
		return refField{}
	}
	lookup := func(p string) ([]string, string, bool) {
		if blockType != "" {
			schema, ok := s.shard.BlockSchemaFor(blockType)
			if !ok {
				return nil, "", false
			}
			for _, f := range schema.Fields {
				if f.Path == p {
					return f.RelationTo, f.RelationToSource, true
				}
			}
			return nil, "", false
		}
		f, ok := s.shard.Field(p)
		return f.RelationTo, f.RelationToSource, ok
	}
	relTo, src, ok := lookup(rel)
	if !ok && strings.HasSuffix(rel, ".value") {
		rel = strings.TrimSuffix(rel, ".value")
		relTo, src, ok = lookup(rel)
	}
	if !ok || len(relTo) == 0 || src == discovery.SourceUnknown || src == discovery.SourceNA {
		return refField{}
	}
	out := refField{relationTo: relTo, source: "relationTo from " + src,
		authoritative: src == discovery.SourceGraphQL || src == discovery.SourceConfigured || src == discovery.SourceProjectSource}
	if blockType != "" {
		out.what = fmt.Sprintf("the %s block's %s", blockType, rel)
		out.describe = fmt.Sprintf("pay describe %s --block %s", s.shard.Slug, blockType)
	} else {
		out.what = s.shard.Slug + "." + rel
		out.describe = fmt.Sprintf("pay describe %s --field %s", s.shard.Slug, rel)
	}
	return out
}

// docPathSeg is one step of a docdiff path: a key, or an index (index >= 0).
type docPathSeg struct {
	key   string
	index int
}

// splitDocPath parses docdiff notation: "layout[2].blocks[0].link.doc".
func splitDocPath(p string) []docPathSeg {
	var out []docPathSeg
	for _, part := range strings.Split(p, ".") {
		for part != "" {
			i := strings.IndexByte(part, '[')
			if i != 0 {
				key := part
				if i > 0 {
					key = part[:i]
				}
				out = append(out, docPathSeg{key: key, index: -1})
				if i < 0 {
					break
				}
				part = part[i:]
				continue
			}
			j := strings.IndexByte(part, ']')
			if j < 0 {
				return nil
			}
			n, err := strconv.Atoi(part[1:j])
			if err != nil {
				return nil
			}
			out = append(out, docPathSeg{index: n})
			part = part[j+1:]
		}
	}
	return out
}
