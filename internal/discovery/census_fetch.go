package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/logging"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
)

// CensusTarget is one entity the census should read.
type CensusTarget struct {
	Kind string
	Slug string
	// Draft asks for the newest draft of every document. It is set unless the
	// entity is known NOT to have drafts; Payload ignores draft=true on an
	// entity without versions, so an unknown answer costs nothing.
	Draft bool
	// Select narrows the read to the top-level fields that can hold blocks.
	// Empty means "read everything" — the answer when no field list is known.
	Select []string
}

// CensusOptions configures one census run.
type CensusOptions struct {
	Client  *payload.Client
	Now     func() time.Time
	MaxDocs int
	// Concurrency bounds how many entities are read at once.
	Concurrency int
	// NoRelationProbe skips the depth-1 re-read of up to three documents per
	// entity that turns an id-looking number into a relationship or upload.
	NoRelationProbe bool
	Logger          *slog.Logger
}

// censusEarlyStop is how many documents without a single block row end an
// entity's read early.
const censusEarlyStop = censusPageSize

// relationProbeDocs bounds how many documents per entity the depth-1 probe
// reads; ProbeCandidates stops earlier once every id-like field is covered.
const relationProbeDocs = 6

// RunCensus reads every target and returns one entity census per target, in
// target order. A failing entity is returned with Error set rather than
// failing the run: one unreadable collection must not hide what the others
// say.
func RunCensus(ctx context.Context, opt CensusOptions, targets []CensusTarget) []*CensusEntity {
	if opt.MaxDocs <= 0 {
		opt.MaxDocs = DefaultCensusMaxDocs
	}
	if opt.Logger == nil {
		opt.Logger = logging.Discard()
	}
	conc := opt.Concurrency
	if conc < 1 {
		conc = 1
	}
	if conc > 4 {
		// The census is background knowledge, never the command's main work,
		// so it stays well under §9.1's request budget.
		conc = 4
	}
	out := make([]*CensusEntity, len(targets))
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, t CensusTarget) {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = censusEntity(ctx, opt, t)
		}(i, t)
	}
	wg.Wait()
	return out
}

// censusEntity reads one entity.
func censusEntity(ctx context.Context, opt CensusOptions, t CensusTarget) *CensusEntity {
	b := NewCensusBuilder(t.Kind, t.Slug)
	ent := b.Entity()
	ent.Draft = t.Draft
	ent.MaxDocs = opt.MaxDocs
	if len(t.Select) > 0 {
		ent.Select = append([]string{}, t.Select...)
	}
	if opt.Now != nil {
		ent.BuiltAt = opt.Now().UTC().Truncate(time.Second)
	}

	var err error
	if t.Kind == EntityKindGlobal {
		err = readGlobal(ctx, opt, t, b)
	} else {
		err = readCollection(ctx, opt, t, b)
	}
	if err != nil {
		ent.Error = err.Error()
		opt.Logger.Debug("census read failed", "slug", t.Slug, "err", err.Error())
	}
	b.Finish()

	if err == nil && !opt.NoRelationProbe {
		probeRelations(ctx, opt, t, b)
	}
	return ent
}

func censusParams(t CensusTarget, depth int) query.Params {
	p := query.Params{Depth: query.IntPtr(depth)}
	if t.Draft {
		p.Draft = query.BoolPtr(true)
	}
	return p
}

// readCollection pages through a collection, depth 0, up to MaxDocs.
//
// The page size is the same on every request: Payload computes a page's
// offset as (page-1)*limit, so shrinking the limit for the last page would
// re-read documents already counted and skip the ones after them. The budget
// is enforced by consuming only as many documents of a page as it has left.
// Documents are counted once by id whatever page brought them.
func readCollection(ctx context.Context, opt CensusOptions, t CensusTarget, b *CensusBuilder) error {
	ent := b.Entity()
	limit := censusPageSize
	if opt.MaxDocs < limit {
		limit = opt.MaxDocs
	}
	seen := map[string]bool{}
	var total int
	consume := func(docs []payload.Doc) {
		for _, doc := range docs {
			if ent.DocsScanned >= opt.MaxDocs {
				return
			}
			if id := doc.ID(); id != nil {
				key := fmt.Sprint(id)
				if seen[key] {
					continue
				}
				seen[key] = true
			}
			b.AddDoc(doc.ID(), doc)
		}
	}
	read := func(page int, sort []string) (*payload.ListResult, error) {
		p := censusParams(t, 0)
		p.Limit = query.IntPtr(limit)
		p.Page = page
		p.Select = t.Select
		p.Sort = sort
		res, err := opt.Client.Find(ctx, t.Slug, p)
		ent.Requests++
		return res, err
	}
	sampledOldest := false
	page := 1
	for {
		res, err := read(page, nil)
		if err != nil {
			return err
		}
		total = res.TotalDocs
		ent.TotalDocs = &total
		consume(res.Docs)
		if !res.HasNextPage || res.NextPage == nil || len(res.Docs) == 0 {
			ent.Truncated = total > ent.DocsScanned
			return nil
		}
		if ent.DocsScanned >= opt.MaxDocs {
			ent.Truncated = total > ent.DocsScanned
			return nil
		}
		if ent.BlockRows == 0 && len(ent.Paths) == 0 && ent.DocsScanned >= censusEarlyStop && !sampledOldest {
			// A full page of the NEWEST documents (Payload's default order)
			// without a single block row. Before concluding that this entity
			// does not use blocks, look at the other end: a migration that
			// creates empty stub pages first makes the newest page of a
			// blocks collection blockless, and a wrong "no blocks here" would
			// be reused for days. One request; ids already counted are skipped.
			sampledOldest = true
			if old, oerr := read(1, []string{"id"}); oerr == nil {
				consume(old.Docs)
			}
			if ent.BlockRows == 0 && len(ent.Paths) == 0 {
				// Both ends hold no blocks: this entity does not use them, and
				// reading hundreds of media documents to prove it again would
				// be the census's whole cost.
				ent.Truncated = total > ent.DocsScanned
				// The two samples may already have covered a small
				// collection; only an unread remainder is an early stop.
				ent.StoppedEarly = ent.Truncated
				return nil
			}
			if ent.DocsScanned >= opt.MaxDocs {
				ent.Truncated = total > ent.DocsScanned
				return nil
			}
		}
		page = *res.NextPage
	}
}

// readGlobal reads a global's one document.
func readGlobal(ctx context.Context, opt CensusOptions, t CensusTarget, b *CensusBuilder) error {
	ent := b.Entity()
	doc, _, err := opt.Client.GlobalGet(ctx, t.Slug, censusParams(t, 0))
	ent.Requests++
	if err != nil {
		return err
	}
	one := 1
	ent.TotalDocs = &one
	b.AddDoc(nil, doc)
	return nil
}

// probeRelations re-reads the few documents that carry the most id-like block
// field values at depth 1. A relationship id comes back as a whole document
// there, which is the only REST-visible difference between "a number field"
// and "a relationship to media" — the fact `pay blocks new` needs to write
// null instead of 0 and validation needs to say "pass the id".
func probeRelations(ctx context.Context, opt CensusOptions, t CensusTarget, b *CensusBuilder) {
	ent := b.Entity()
	var ids []any
	if t.Kind == EntityKindGlobal {
		if len(b.candidate) > 0 {
			ids = []any{nil}
		}
	} else {
		ids = b.ProbeCandidates(relationProbeDocs)
	}
	if len(ids) == 0 {
		ent.RelationProbe = "none"
		return
	}
	failed := 0
	for _, id := range ids {
		var doc payload.Doc
		var err error
		if t.Kind == EntityKindGlobal {
			doc, _, err = opt.Client.GlobalGet(ctx, t.Slug, censusParams(t, 1))
		} else {
			doc, _, err = opt.Client.Get(ctx, t.Slug, fmt.Sprint(id), censusParams(t, 1))
		}
		ent.Requests++
		if err != nil {
			failed++
			continue
		}
		b.ApplyDepth1(doc)
	}
	ent.RelationProbe = "ok"
	if failed == len(ids) {
		ent.RelationProbe = "failed"
	}
}

// ---------------------------------------------------------------------------
// choosing what to read
// ---------------------------------------------------------------------------

// ShardLookup returns an entity's field shard, or nil.
type ShardLookup func(kind, slug string) *Shard

// CensusTargets decides which entities of a manifest can hold blocks and how
// to read each one cheaply.
//
// A GraphQL-sourced shard is authoritative about field kinds, so an entity
// whose schema has no blocks field anywhere is skipped outright. Anything
// else — a REST-observed shard (introspection disabled), or no shard at all —
// is read, narrowed with `select` to the top-level fields that could contain a
// blocks array: arrays, groups, and fields whose kind the three-document
// sample could not settle (a `layout` that was [] in every sample).
//
// only restricts the result to the named slugs (collections or globals).
func CensusTargets(m *Manifest, shards ShardLookup, only []string) []CensusTarget {
	if m == nil {
		return nil
	}
	want := map[string]bool{}
	for _, s := range only {
		want[s] = true
	}
	var out []CensusTarget
	for _, c := range m.Collections {
		if c == nil || (len(want) > 0 && !want[c.Slug]) {
			continue
		}
		if len(want) == 0 && (c.Internal || c.Reachability == ReachabilityAccessDenied) {
			continue
		}
		if c.Flags.EndpointsDisabled != nil && *c.Flags.EndpointsDisabled {
			continue
		}
		var shard *Shard
		if shards != nil {
			shard = shards(EntityKindCollection, c.Slug)
		}
		sel, ok := censusSelect(shard, c.FieldsSource)
		if !ok && len(want) == 0 {
			continue
		}
		out = append(out, CensusTarget{
			Kind: EntityKindCollection, Slug: c.Slug, Select: sel,
			Draft: c.Flags.Drafts == nil || *c.Flags.Drafts,
		})
	}
	for _, g := range m.Globals {
		if g == nil || (len(want) > 0 && !want[g.Slug]) {
			continue
		}
		if len(want) == 0 && (g.Internal || g.Reachability == ReachabilityAccessDenied) {
			continue
		}
		var shard *Shard
		if shards != nil {
			shard = shards(EntityKindGlobal, g.Slug)
		}
		if _, ok := censusSelect(shard, g.FieldsSource); !ok && len(want) == 0 {
			continue
		}
		// A global is one document; a projection would save nothing.
		out = append(out, CensusTarget{
			Kind: EntityKindGlobal, Slug: g.Slug,
			Draft: g.Flags.Drafts == nil || *g.Flags.Drafts,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind == EntityKindCollection
		}
		return out[i].Slug < out[j].Slug
	})
	return out
}

// censusSelect returns the projection for one entity and whether it can hold
// blocks at all.
func censusSelect(shard *Shard, fieldsSource string) ([]string, bool) {
	if shard == nil || len(shard.Fields) == 0 {
		// Nothing known: read whole documents.
		return nil, true
	}
	graphQL := fieldsSource == SourceGraphQL
	if graphQL {
		roots := map[string]bool{}
		for _, f := range shard.Fields {
			if f.PayloadType == TypeBlocks {
				root, _, _ := strings.Cut(f.Path, ".")
				roots[root] = true
			}
		}
		if len(roots) == 0 {
			return nil, false
		}
		return sortedBoolKeys(roots), true
	}
	roots := map[string]bool{}
	for _, f := range shard.Fields {
		if strings.Contains(f.Path, ".") {
			continue
		}
		switch f.PayloadType {
		case TypeBlocks, TypeArray, TypeGroup, TypeUnknown:
			roots[f.Path] = true
		default:
			if f.JSONType == "array" || f.JSONType == "object" || f.JSONType == "unknown" {
				if f.PayloadType != TypeRichText && f.PayloadType != TypeRelationship && f.PayloadType != TypeUpload {
					roots[f.Path] = true
				}
			}
		}
	}
	for _, j := range shard.JoinFields {
		delete(roots, j)
	}
	if len(roots) == 0 {
		return nil, false
	}
	return sortedBoolKeys(roots), true
}

func sortedBoolKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
