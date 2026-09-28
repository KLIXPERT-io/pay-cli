package discovery

import (
	"encoding/json"
	"sort"
	"strings"
)

// A census is a snapshot. During a migration the site keeps changing under
// it — often through PayCLI itself — and every restructure made the next write
// of the same pages warn about content that is live on the server: a
// blockType "never observed" at the position it now holds, a field "always
// filled" that the moved rows leave empty. The document a write command
// already has in hand (the pre-image a plan, a dry-run diff or an upsert
// fetched; the echo of a write that just succeeded) is the freshest evidence
// there is, and it costs no request. OverlayDocs folds such documents into an
// entity's census as additional observations.
//
// An overlay only ADDS evidence. It never makes a field select-like (one
// document is not a vocabulary), and a field's existing select-like verdict is
// kept: a value the document carries joins that vocabulary, so an agent may
// write it back, but a value nobody has used is still reported.
//
// A document is COUNTED once. The census remembers which documents it read
// (DocKeys) and which ones were folded in since (FoldedKeys). A document that
// is neither is new evidence and is added in full: its rows raise the counts
// and presence ratios like any row the census read. A document the census
// already counted — the page being iterated on, re-written and re-echoed on
// every run, or the pre-image of a document the census itself read — only
// REFRESHES what is known: a position, blockType, field or value it shows for
// the first time is recorded, and a field it leaves empty is marked Optional
// (never likely_required again), but no count grows. Adding it again would
// make PayCLI's own write history outweigh the site: three title-only updates
// of one page turned a field a single real row fills into "likely required"
// for every row of that type.

// OverlayDocs returns a copy of base that also observes docs; base itself is
// never modified. It returns nil when base is nil: one document is not a
// census, and "never observed" against it would be a guess.
//
// persist is true when the result will be written to the cache. Values seen
// once are then dropped exactly as a census read drops them (a value seen once
// is content, and content never reaches the disk), except for fields the
// census already treats as a vocabulary; and the folded documents are
// recorded in FoldedKeys so a later fold of the same document refreshes
// instead of counting it again.
func OverlayDocs(base *CensusEntity, persist bool, docs ...map[string]any) *CensusEntity {
	if base == nil {
		return nil
	}
	out := cloneEntity(base)
	known := out.countedDocs()
	for _, d := range docs {
		if d == nil {
			continue
		}
		id, key := d["id"], CensusDocKey(d["id"])
		if base.Kind == EntityKindGlobal {
			id, key = nil, CensusDocKey(nil)
		}
		b := NewCensusBuilder(base.Kind, base.Slug)
		b.keepValues = true
		b.AddDoc(id, d)
		ov := b.Finish()
		if ov.BlockRows == 0 && len(ov.Paths) == 0 {
			continue
		}
		if known != nil && !known[key] {
			addObservations(out, ov, persist)
		} else {
			refreshObservations(out, ov, persist)
		}
		if known != nil {
			known[key] = true
		}
		if persist && !containsString(out.FoldedKeys, key) {
			out.FoldedKeys = append(out.FoldedKeys, key)
			out.FoldedDocs = len(out.FoldedKeys)
		}
	}
	return out
}

// countedDocs is the set of documents this entity already counts: the ones
// the census read and the ones folded in since. It returns nil for an entity
// written by a build that did not record them (it read documents but lists
// none); every document is then treated as counted, because inflating the
// counts is the failure this set exists to prevent.
func (e *CensusEntity) countedDocs() map[string]bool {
	if e.DocKeys == nil && e.DocsScanned > 0 {
		return nil
	}
	set := make(map[string]bool, len(e.DocKeys)+len(e.FoldedKeys))
	for _, k := range e.DocKeys {
		set[k] = true
	}
	for _, k := range e.FoldedKeys {
		set[k] = true
	}
	return set
}

// addObservations adds a new document's observations to out in full.
func addObservations(out, ov *CensusEntity, persist bool) {
	out.BlockRows += ov.BlockRows
	for p, op := range ov.Paths {
		dp := out.Paths[p]
		if dp == nil {
			out.Paths[p] = op
			continue
		}
		addCounts(&dp.Types, op.Types)
		addCounts(&dp.ParentTypes, op.ParentTypes)
		dp.Rows += op.Rows
		dp.Docs += op.Docs
		dp.EmptyRows += op.EmptyRows
		dp.UntypedRows += op.UntypedRows
	}
	for slug, ot := range ov.Types {
		dt := out.Types[slug]
		if dt == nil {
			insertType(out, ot, persist)
			continue
		}
		dt.Count += ot.Count
		dt.Docs += ot.Docs
		addCounts(&dt.UsedIn, ot.UsedIn)
		if dt.Fields == nil {
			dt.Fields = map[string]*CensusField{}
		}
		for name, of := range ot.Fields {
			dt.Fields[name] = overlayField(dt.Fields[name], of, persist)
		}
	}
}

// refreshObservations records what a document the census already counts
// shows for the first time, without counting any of it again.
func refreshObservations(out, ov *CensusEntity, persist bool) {
	for p, op := range ov.Paths {
		dp := out.Paths[p]
		if dp == nil {
			// A position nobody saw before: this document is its only
			// evidence, so its counts are the position's counts.
			out.Paths[p] = op
			continue
		}
		for t, n := range op.Types {
			if _, seen := dp.Types[t]; !seen {
				if dp.Types == nil {
					dp.Types = map[string]int{}
				}
				dp.Types[t] = n
				dp.Rows += n
			}
		}
		addNewKeys(&dp.ParentTypes, op.ParentTypes)
	}
	for slug, ot := range ov.Types {
		dt := out.Types[slug]
		if dt == nil {
			insertType(out, ot, persist)
			continue
		}
		addNewKeys(&dt.UsedIn, ot.UsedIn)
		if dt.Fields == nil {
			dt.Fields = map[string]*CensusField{}
		}
		markOptional(dt.Fields, ot.Fields, ot.Count)
		for name, of := range ot.Fields {
			dt.Fields[name] = refreshField(dt.Fields[name], of, dt.Count, persist)
		}
	}
}

// insertType adds a blockType only the overlaid document carries.
func insertType(out *CensusEntity, ot *CensusType, persist bool) {
	for _, f := range ot.Fields {
		demoteField(f, persist)
	}
	if persist {
		// References point at rows the census read; an overlaid document's
		// rows may move again before anyone follows them.
		ot.Instances = []CensusRef{}
	}
	out.Types[ot.Slug] = ot
}

// markOptional marks every known field that rows of an already counted
// document leave empty (null, "", [], an empty rich text, or absent): the
// server holds such a row, so the field is not required whatever the counts
// say. base is how many objects src describes.
func markOptional(dst, src map[string]*CensusField, base int) {
	if base <= 0 {
		return
	}
	for name, df := range dst {
		sf := src[name]
		if sf == nil || sf.NonNull < base || sf.NonEmpty < base {
			df.Optional = true
		}
		if sf == nil {
			continue
		}
		switch df.Kind {
		case CensusKindGroup:
			markOptional(df.Fields, sf.Fields, sf.NonNull)
		case CensusKindArray:
			markOptional(df.Fields, sf.Fields, sf.Items)
		}
	}
}

// refreshField records what b shows about a field for the first time. base is
// how many objects a describes; a field first seen here gets counts no larger
// than that and is Optional, because the objects the census read did not
// carry it.
func refreshField(a, b *CensusField, base int, persist bool) *CensusField {
	if a == nil {
		demoteField(b, persist)
		clampField(b, base)
		b.Optional = true
		return b
	}
	if a.KindSource != SourceObservedDepth1 {
		a.Kind = pickKind(map[string]int{a.Kind: 1, b.Kind: 1})
	}
	addNewKeys(&a.Types, b.Types)
	addNewKeys(&a.Accepts, b.Accepts)
	a.DistinctCapped = a.DistinctCapped || b.DistinctCapped
	joinValues(a, b.Values, persist, false)
	subBase := a.NonNull
	if a.Kind == CensusKindArray {
		subBase = a.Items
	}
	for name, sub := range b.Fields {
		if a.Fields == nil {
			a.Fields = map[string]*CensusField{}
		}
		a.Fields[name] = refreshField(a.Fields[name], sub, subBase, persist)
	}
	return a
}

// clampField caps a field's counts at base, recursively for its sub-fields.
func clampField(f *CensusField, base int) {
	f.Present = min(f.Present, base)
	f.NonNull = min(f.NonNull, base)
	f.NonEmpty = min(f.NonEmpty, base)
	subBase := f.NonNull
	if f.Kind == CensusKindArray {
		f.Items = min(f.Items, base)
		subBase = f.Items
	}
	for _, sub := range f.Fields {
		clampField(sub, subBase)
		sub.Optional = true
	}
}

// overlayField adds b's observations to a (a copy the overlay owns).
func overlayField(a, b *CensusField, persist bool) *CensusField {
	if a == nil {
		demoteField(b, persist)
		return b
	}
	if a.KindSource != SourceObservedDepth1 {
		a.Kind = pickKind(map[string]int{a.Kind: 1, b.Kind: 1})
	}
	addCounts(&a.Types, b.Types)
	a.Present += b.Present
	a.NonNull += b.NonNull
	a.NonEmpty += b.NonEmpty
	a.Items += b.Items
	addCounts(&a.Accepts, b.Accepts)
	a.DistinctCapped = a.DistinctCapped || b.DistinctCapped
	joinValues(a, b.Values, persist, true)
	for name, sub := range b.Fields {
		if a.Fields == nil {
			a.Fields = map[string]*CensusField{}
		}
		a.Fields[name] = overlayField(a.Fields[name], sub, persist)
	}
	return a
}

// joinValues adds observed values to a field's list. count says whether a
// value already listed has its count raised (new evidence) or is left alone
// (a refresh of a document already counted).
func joinValues(a *CensusField, values []CensusValue, persist, count bool) {
	idx := map[string]int{}
	for i, v := range a.Values {
		key, _ := valueKey(v.Value)
		idx[key] = i
	}
	for _, v := range values {
		key, ok := valueKey(v.Value)
		if !ok {
			continue
		}
		if i, seen := idx[key]; seen {
			if count {
				a.Values[i].Count += v.Count
			}
			continue
		}
		if persist && !a.EnumLike && !keepOnDisk(v) {
			continue
		}
		idx[key] = len(a.Values)
		a.Values = append(a.Values, v)
	}
	if len(a.Values) > a.Distinct {
		a.Distinct = len(a.Values)
	}
	sort.SliceStable(a.Values, func(i, j int) bool { return a.Values[i].Count > a.Values[j].Count })
	if len(a.Values) > maxCensusValues {
		// More distinct values than a select-like field may have.
		a.EnumLike = false
		a.Values = a.Values[:maxCensusValues]
	}
}

// demoteField prepares a field only the overlay observed: never select-like,
// and on disk only repeated values.
func demoteField(f *CensusField, persist bool) {
	if f == nil {
		return
	}
	f.EnumLike = false
	if persist {
		kept := f.Values[:0]
		for _, v := range f.Values {
			if keepOnDisk(v) {
				kept = append(kept, v)
			}
		}
		f.Values = kept
	}
	for _, sub := range f.Fields {
		demoteField(sub, persist)
	}
}

// keepOnDisk is the census's rule for a non-vocabulary value: repeated, or a
// boolean (kept for defaults).
func keepOnDisk(v CensusValue) bool {
	if _, isBool := v.Value.(bool); isBool {
		return true
	}
	return v.Count >= 2
}

// addNewKeys records the keys of src that dst does not have yet, with their
// counts; a key dst already counts is left alone.
func addNewKeys(dst *map[string]int, src map[string]int) {
	for k, n := range src {
		if *dst == nil {
			*dst = map[string]int{}
		}
		if _, seen := (*dst)[k]; !seen {
			(*dst)[k] = n
		}
	}
}

func addCounts(dst *map[string]int, src map[string]int) {
	if len(src) == 0 {
		return
	}
	if *dst == nil {
		*dst = map[string]int{}
	}
	for k, n := range src {
		(*dst)[k] += n
	}
}

// cloneEntity deep-copies an entity through its JSON form, which is exactly
// what the cache stores.
func cloneEntity(e *CensusEntity) *CensusEntity {
	raw, err := json.Marshal(e)
	if err != nil {
		return e
	}
	var out CensusEntity
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return e
	}
	if out.Paths == nil {
		out.Paths = map[string]*CensusPath{}
	}
	if out.Types == nil {
		out.Types = map[string]*CensusType{}
	}
	return &out
}

// WithEntity returns a shallow copy of the census in which e replaces the
// entity of its kind and slug. The receiver is not modified.
func (c *Census) WithEntity(e *CensusEntity) *Census {
	if c == nil || e == nil {
		return c
	}
	out := *c
	out.Collections = make(map[string]*CensusEntity, len(c.Collections)+1)
	for k, v := range c.Collections {
		out.Collections[k] = v
	}
	out.Globals = make(map[string]*CensusEntity, len(c.Globals)+1)
	for k, v := range c.Globals {
		out.Globals[k] = v
	}
	out.Put(e)
	return &out
}
