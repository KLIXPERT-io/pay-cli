package discovery

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The block census (§7.10c) answers, from the live site alone, the questions
// §7.10 and §7.10a can only answer with GraphQL introspection or the project's
// source on disk: which blockTypes are used WHERE, what fields each one
// carries, which values those fields take, and what a real, complete row looks
// like.
//
// Neither prerequisite is dependable. GraphQL introspection is OFF by default
// in production — Payload 3.87's `graphQL.disableIntrospectionInProduction`
// defaults to true and rejects every __schema/__type query when NODE_ENV is
// production (verified in @payloadcms/graphql's NoProductionIntrospection
// rule) — and the app's source is not on the machine running PayCLI in
// general. Real documents are always there: a Payload REST read at depth 0
// returns every field of every block row, nulls included, so the rows ARE a
// schema, measured rather than declared.
//
// Everything here is observation, and every fact the census publishes is
// labelled "observed-census" so it is never mistaken for a declaration. The
// census is an AGGREGATE: it stores counts, JSON types, a bounded set of short
// scalar values and document REFERENCES (collection, id, path) — never a
// document. §8.3 rule 4 (no document data on disk) therefore still holds; an
// example row is fetched live when it is asked for.

// CensusVersion is the on-disk layout version. A file carrying any other value
// is a cache miss.
const CensusVersion = 1

// DefaultCensusMaxDocs bounds how many documents one entity's census reads.
// Pages are paged 100 at a time, so the default costs at most five requests
// per collection.
const DefaultCensusMaxDocs = 500

// censusPageSize is the REST page size the census asks for.
const censusPageSize = 100

// Bounds that keep one census small no matter what a project stores.
const (
	// maxCensusValues is how many distinct scalar values a field reports.
	maxCensusValues = 12
	// maxCensusTracked is how many distinct values are counted before the
	// field is declared high-cardinality (distinct_capped).
	maxCensusTracked = 64
	// maxCensusValueLen is the longest string recorded as a value. Longer
	// strings are prose, and prose is never an enum.
	maxCensusValueLen = 120
	// maxCensusRefs is how many instance references a block type keeps.
	maxCensusRefs = 8
	// maxCensusDepth guards the recursive walk against pathological nesting.
	maxCensusDepth = 16
)

// SourceObservedCensus labels every fact the census measured. It is distinct
// from SourceObserved (the three-document REST field sample) because the two
// differ by two orders of magnitude in evidence.
const SourceObservedCensus = "observed-census"

// SourceCensusMatched labels a blockType slug the census joined to a GraphQL
// union member by name because introspection published only the member's
// interfaceName (CallToActionBlock) and a real document carries the slug (cta).
const SourceCensusMatched = "census-matched"

// SourceObservedDepth1 labels a relationship/upload kind learned by reading one
// document at depth 1, where a relationship id comes back as a whole document.
const SourceObservedDepth1 = "observed-depth-1"

// Census kinds: what a field's values LOOK like across every observed row.
const (
	CensusKindBlocks       = "blocks"
	CensusKindArray        = "array"
	CensusKindRichText     = "richText"
	CensusKindGroup        = "group"
	CensusKindRelationship = "relationship"
	CensusKindUpload       = "upload"
	CensusKindList         = "list"
	CensusKindScalar       = "scalar"
	CensusKindUnknown      = "unknown"
)

// Census is the persisted artefact: one entry per entity that was read.
type Census struct {
	CensusVersion int       `json:"census_version"`
	Scope         string    `json:"scope"`
	CLIVersion    string    `json:"cli_version"`
	UpdatedAt     time.Time `json:"updated_at"`
	// Collections and Globals are keyed by slug. Entities are built and
	// refreshed independently, so each carries its own built_at.
	Collections map[string]*CensusEntity `json:"collections"`
	Globals     map[string]*CensusEntity `json:"globals"`
}

// NewCensus returns an empty census for a scope.
func NewCensus(scope string) *Census {
	return &Census{
		CensusVersion: CensusVersion,
		Scope:         scope,
		Collections:   map[string]*CensusEntity{},
		Globals:       map[string]*CensusEntity{},
	}
}

// DecodeCensus parses a persisted census, reporting false for anything this
// build cannot read — which the caller treats as a miss, never as an error.
func DecodeCensus(raw []byte) (*Census, bool) {
	c, err := DecodeCensusErr(raw)
	return c, err == nil
}

// DecodeCensusErr is DecodeCensus with the reason, for the cache_unreadable
// warning a caller owes when a census that passed the header check still
// cannot be used (§8.3: a miss plus a warning, never a silent miss).
func DecodeCensusErr(raw []byte) (*Census, error) {
	var c Census
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	if c.CensusVersion != CensusVersion {
		return nil, fmt.Errorf("census_version %d, this build reads %d", c.CensusVersion, CensusVersion)
	}
	if c.Collections == nil {
		c.Collections = map[string]*CensusEntity{}
	}
	if c.Globals == nil {
		c.Globals = map[string]*CensusEntity{}
	}
	return &c, nil
}

// Entity returns one entity's census.
func (c *Census) Entity(kind, slug string) (*CensusEntity, bool) {
	if c == nil {
		return nil, false
	}
	m := c.Collections
	if kind == EntityKindGlobal {
		m = c.Globals
	}
	e, ok := m[slug]
	return e, ok && e != nil
}

// Put stores (or replaces) one entity's census.
func (c *Census) Put(e *CensusEntity) {
	if c == nil || e == nil {
		return
	}
	if e.Kind == EntityKindGlobal {
		c.Globals[e.Slug] = e
		return
	}
	c.Collections[e.Slug] = e
}

// Entities returns every entity, collections first, each group sorted.
func (c *Census) Entities() []*CensusEntity {
	if c == nil {
		return nil
	}
	var out []*CensusEntity
	for _, m := range []map[string]*CensusEntity{c.Collections, c.Globals} {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if m[k] != nil {
				out = append(out, m[k])
			}
		}
	}
	return out
}

// Entity kinds, as the census and its consumers name them.
const (
	EntityKindCollection = "collection"
	EntityKindGlobal     = "global"
)

// CensusEntity is one collection's or global's census.
type CensusEntity struct {
	Slug    string    `json:"slug"`
	Kind    string    `json:"kind"`
	BuiltAt time.Time `json:"built_at"`
	// Draft reports that documents were read with draft=true, i.e. the census
	// describes the newest draft of every document, which is what an editor
	// is about to overwrite.
	Draft bool `json:"draft"`
	// Select is the top-level field projection the read used; empty means the
	// whole document was read.
	Select      []string `json:"select"`
	DocsScanned int      `json:"docs_scanned"`
	TotalDocs   *int     `json:"total_docs"`
	// Truncated is true when the entity has more documents than were read.
	Truncated bool `json:"truncated"`
	// StoppedEarly is true when the read stopped after a full page with no
	// block row at all: the entity does not use blocks.
	StoppedEarly bool `json:"stopped_early,omitempty"`
	MaxDocs      int  `json:"max_docs"`
	Requests     int  `json:"requests"`
	// RelationProbe says whether the depth-1 relationship probe ran: "ok",
	// "none" (nothing to probe), "failed" or "skipped".
	RelationProbe string `json:"relation_probe"`
	// Error is set when the read failed; the entity then carries whatever was
	// learned before the failure.
	Error string `json:"error,omitempty"`
	// FoldedDocs counts the distinct documents PayCLI's own successful writes
	// folded into this entity after it was read (OverlayDocs): the census
	// then also knows the shapes those writes created, without another read.
	FoldedDocs int `json:"folded_docs,omitempty"`
	// DocKeys are the ids of the documents the read counted ("_global" for a
	// global) and FoldedKeys those of the documents folded in since. They are
	// references, like Instances, never content; OverlayDocs uses them to
	// count every document once however often it is written (§7.10c).
	DocKeys    []string               `json:"doc_keys,omitempty"`
	FoldedKeys []string               `json:"folded_keys,omitempty"`
	BlockRows  int                    `json:"block_rows"`
	Paths      map[string]*CensusPath `json:"paths"`
	Types      map[string]*CensusType `json:"types"`
}

// NewCensusEntity returns an empty entity census.
func NewCensusEntity(kind, slug string) *CensusEntity {
	return &CensusEntity{
		Slug:          slug,
		Kind:          kind,
		Select:        []string{},
		RelationProbe: "skipped",
		Paths:         map[string]*CensusPath{},
		Types:         map[string]*CensusType{},
	}
}

// TypeSlugs lists the entity's observed blockTypes, sorted.
func (e *CensusEntity) TypeSlugs() []string {
	if e == nil {
		return nil
	}
	out := make([]string, 0, len(e.Types))
	for s := range e.Types {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// PathPatterns lists the entity's blocks-field path patterns, shallowest first.
func (e *CensusEntity) PathPatterns() []string {
	if e == nil {
		return nil
	}
	out := make([]string, 0, len(e.Paths))
	for p := range e.Paths {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		di, dj := strings.Count(out[i], "[]"), strings.Count(out[j], "[]")
		if di != dj {
			return di < dj
		}
		return out[i] < out[j]
	})
	return out
}

// CensusPath is one blocks-field path PATTERN: `layout`, `layout[].blocks`,
// `layout[].blocks[].blocks`. Indices are erased because the question is
// "what may go at this position", not "what is at row 3".
type CensusPath struct {
	Path string `json:"path"`
	// Parent is the enclosing blocks pattern ("" at the top level).
	Parent string `json:"parent"`
	// ParentTypes counts the blockType of the row that owns this field, so a
	// nested pattern knows it is `group.blocks` in 30 rows and `grid.blocks`
	// in 12.
	ParentTypes map[string]int `json:"parent_types,omitempty"`
	// Field is the key inside the parent row (`blocks`), or the top-level
	// field path for a top-level pattern.
	Field string `json:"field"`
	// Types counts every blockType seen at this pattern.
	Types map[string]int `json:"types"`
	Rows  int            `json:"rows"`
	Docs  int            `json:"docs"`
	// EmptyRows are `{}` entries — the orphan rows Payload's Postgres adapter
	// leaves behind after a nested block restructure.
	EmptyRows int `json:"empty_rows"`
	// UntypedRows are non-empty rows with no blockType.
	UntypedRows int `json:"untyped_rows"`
}

// CensusType is everything observed about one blockType.
type CensusType struct {
	Slug  string `json:"slug"`
	Count int    `json:"count"`
	Docs  int    `json:"docs"`
	// UsedIn counts rows per path pattern.
	UsedIn map[string]int `json:"used_in"`
	// Fields are the row's keys, excluding id and blockType.
	Fields map[string]*CensusField `json:"fields"`
	// Instances reference the most complete real rows, most complete first.
	Instances []CensusRef `json:"instances"`
}

// CensusRef points at one real row. It is a reference, never a copy.
type CensusRef struct {
	Kind string `json:"kind"`
	Slug string `json:"slug"`
	ID   any    `json:"id,omitempty"`
	// Path is the concrete row path, e.g. layout[1].blocks[0].
	Path string `json:"path"`
	// Score is the number of non-empty fields on the row.
	Score int `json:"score"`
}

// CensusField is the observed profile of one key.
type CensusField struct {
	Kind string `json:"kind"`
	// KindSource is observed-census, or observed-depth-1 when the kind was
	// settled by the relationship probe.
	KindSource string `json:"kind_source"`
	// Types counts JSON types: string, number, boolean, object, array, null.
	Types    map[string]int `json:"types"`
	Present  int            `json:"present"`
	NonNull  int            `json:"non_null"`
	NonEmpty int            `json:"non_empty"`
	// Values are the most common scalar values with their counts. For a field
	// that is not enum-like only REPEATED values are kept: a value seen once is
	// content, not vocabulary.
	Values         []CensusValue `json:"values,omitempty"`
	Distinct       int           `json:"distinct"`
	DistinctCapped bool          `json:"distinct_capped,omitempty"`
	EnumLike       bool          `json:"enum_like"`
	// Accepts counts blockTypes for a nested blocks field.
	Accepts map[string]int `json:"accepts,omitempty"`
	// Items is the total number of array / blocks rows observed.
	Items int `json:"items,omitempty"`
	// Fields are a group's keys or an array row's keys.
	Fields map[string]*CensusField `json:"fields,omitempty"`
	// Optional is set when a document the census already counted was folded
	// in (OverlayDocs) holding this field empty, or holding it at all for the
	// first time: the server keeps such a row, so the field is never reported
	// likely_required, whatever the counts say.
	Optional bool `json:"optional,omitempty"`

	kinds   map[string]int
	tracked map[string]*valueAcc
	// seenIn is the first document whose value was a probe candidate.
	seenIn string
}

// CensusValue is one observed scalar value.
type CensusValue struct {
	Value any `json:"value"`
	Count int `json:"count"`
}

type valueAcc struct {
	value any
	count int
	order int
}

// ---------------------------------------------------------------------------
// building
// ---------------------------------------------------------------------------

// CensusBuilder accumulates one entity's census document by document.
type CensusBuilder struct {
	ent *CensusEntity

	docKey    string
	docID     any
	typeDocs  map[string]map[string]bool
	pathDocs  map[string]map[string]bool
	refOrder  int
	refs      map[string][]scoredRef
	docOrder  []string
	docIDs    map[string]any
	candidate map[string]int
	// candFields records, per document, which id-like fields it carries, so
	// the relationship probe can pick documents that cover every field.
	candFields map[string]map[*CensusField]bool
	// keepValues keeps every observed value, not only repeated ones. Only an
	// in-memory overlay (OverlayDocs) sets it: a value seen once is content,
	// and content never reaches the disk.
	keepValues bool
}

type scoredRef struct {
	ref   CensusRef
	order int
}

// NewCensusBuilder starts a census for one entity.
func NewCensusBuilder(kind, slug string) *CensusBuilder {
	return &CensusBuilder{
		ent:        NewCensusEntity(kind, slug),
		typeDocs:   map[string]map[string]bool{},
		pathDocs:   map[string]map[string]bool{},
		refs:       map[string][]scoredRef{},
		docIDs:     map[string]any{},
		candidate:  map[string]int{},
		candFields: map[string]map[*CensusField]bool{},
	}
}

// Entity returns the entity under construction. Call Finish first.
func (b *CensusBuilder) Entity() *CensusEntity { return b.ent }

// AddDoc walks one document. id may be nil for a global.
func (b *CensusBuilder) AddDoc(id any, doc map[string]any) {
	if doc == nil {
		return
	}
	b.docKey = CensusDocKey(id)
	b.docID = id
	if _, seen := b.docIDs[b.docKey]; !seen {
		b.docOrder = append(b.docOrder, b.docKey)
		b.docIDs[b.docKey] = id
		b.ent.DocKeys = append(b.ent.DocKeys, b.docKey)
	}
	b.ent.DocsScanned++
	for _, k := range sortedAnyKeys(doc) {
		if k == "id" {
			continue
		}
		b.walkTop(doc[k], k, k, 0)
	}
}

// CensusDocKey is the census's key for a document id: its decimal or string
// form, and "_global" for a global's one document (id nil).
func CensusDocKey(id any) string {
	switch t := id.(type) {
	case nil:
		return "_global"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	}
	return fmt.Sprint(id)
}

// walkTop walks a document-level value looking for blocks arrays. It never
// profiles plain document fields: the census is about blocks, and the REST
// field sample already covers the top level.
func (b *CensusBuilder) walkTop(v any, pattern, concrete string, depth int) {
	if depth > maxCensusDepth {
		return
	}
	switch t := v.(type) {
	case []any:
		if isBlocksArray(t) {
			b.blocksField(t, pattern, concrete, "", "", pattern, depth)
			return
		}
		for i, item := range t {
			row, ok := item.(map[string]any)
			if !ok {
				continue
			}
			for _, k := range sortedAnyKeys(row) {
				if k == "id" {
					continue
				}
				b.walkTop(row[k], pattern+"[]."+k, fmt.Sprintf("%s[%d].%s", concrete, i, k), depth+1)
			}
		}
	case map[string]any:
		if isLexicalRoot(t) || isPolymorphicRef(t) {
			return
		}
		for _, k := range sortedAnyKeys(t) {
			b.walkTop(t[k], pattern+"."+k, concrete+"."+k, depth+1)
		}
	}
}

// blocksField records one blocks array at a path pattern.
func (b *CensusBuilder) blocksField(rows []any, pattern, concrete, parentPattern, parentType, field string, depth int) {
	p := b.ent.Paths[pattern]
	if p == nil {
		p = &CensusPath{Path: pattern, Parent: parentPattern, Field: field, Types: map[string]int{}}
		b.ent.Paths[pattern] = p
	}
	if parentType != "" {
		if p.ParentTypes == nil {
			p.ParentTypes = map[string]int{}
		}
		p.ParentTypes[parentType]++
	}
	if markDoc(b.pathDocs, pattern, b.docKey) {
		p.Docs++
	}
	for i, item := range rows {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if len(row) == 0 {
			p.EmptyRows++
			continue
		}
		bt, _ := row["blockType"].(string)
		if bt == "" {
			p.UntypedRows++
			continue
		}
		p.Types[bt]++
		p.Rows++
		b.ent.BlockRows++

		t := b.ent.Types[bt]
		if t == nil {
			t = &CensusType{Slug: bt, UsedIn: map[string]int{}, Fields: map[string]*CensusField{}}
			b.ent.Types[bt] = t
		}
		t.Count++
		t.UsedIn[pattern]++
		if markDoc(b.typeDocs, bt, b.docKey) {
			t.Docs++
		}
		rowPath := fmt.Sprintf("%s[%d]", concrete, i)
		score := b.observeRow(t.Fields, row, bt, pattern+"[]", rowPath, depth+1, true)
		b.refOrder++
		b.refs[bt] = append(b.refs[bt], scoredRef{order: b.refOrder, ref: CensusRef{
			Kind: b.ent.Kind, Slug: b.ent.Slug, ID: b.docID, Path: rowPath, Score: score,
		}})
	}
}

// observeRow profiles every key of one row into fields and returns the row's
// completeness score (its number of non-empty keys).
func (b *CensusBuilder) observeRow(fields map[string]*CensusField, row map[string]any,
	ownerType, rowPattern, rowPath string, depth int, isBlock bool) int {
	score := 0
	for _, k := range sortedAnyKeys(row) {
		if k == "id" || (isBlock && k == "blockType") {
			continue
		}
		f := fields[k]
		if f == nil {
			f = newCensusField()
			fields[k] = f
		}
		v := row[k]
		b.observeField(f, v, ownerType, rowPattern+"."+k, rowPath+"."+k, k, depth+1)
		if nonEmptyValue(v) {
			score++
		}
	}
	return score
}

func newCensusField() *CensusField {
	return &CensusField{Types: map[string]int{}, kinds: map[string]int{}, KindSource: SourceObservedCensus}
}

// observeField records one value of one key.
func (b *CensusBuilder) observeField(f *CensusField, v any, ownerType, pattern, concrete, key string, depth int) {
	if depth > maxCensusDepth {
		return
	}
	f.Present++
	f.Types[jsonTypeName(v)]++
	if v == nil {
		return
	}
	f.NonNull++
	if nonEmptyValue(v) {
		f.NonEmpty++
	}
	if withholdValue(key, v) {
		f.kinds[CensusKindScalar]++
		return
	}
	switch t := v.(type) {
	case []any:
		f.Items += len(t)
		switch {
		case len(t) == 0:
			f.kinds[CensusKindArray] += 0
		case isBlocksArray(t):
			f.kinds[CensusKindBlocks]++
			if f.Accepts == nil {
				f.Accepts = map[string]int{}
			}
			for _, item := range t {
				if row, ok := item.(map[string]any); ok {
					if bt, _ := row["blockType"].(string); bt != "" {
						f.Accepts[bt]++
					}
				}
			}
			parent := strings.TrimSuffix(pattern, "."+key)
			parent = strings.TrimSuffix(parent, "[]")
			b.blocksField(t, pattern, concrete, parent, ownerType, key, depth)
		case allObjects(t):
			f.kinds[CensusKindArray]++
			if f.Fields == nil {
				f.Fields = map[string]*CensusField{}
			}
			for i, item := range t {
				if row, ok := item.(map[string]any); ok {
					b.observeRow(f.Fields, row, ownerType, pattern+"[]", fmt.Sprintf("%s[%d]", concrete, i), depth, false)
				}
			}
		default:
			f.kinds[CensusKindList]++
			if allIDLike(t) {
				b.markCandidate(f)
			}
		}
	case map[string]any:
		switch {
		case isLexicalRoot(t):
			f.kinds[CensusKindRichText]++
		case isPolymorphicRef(t):
			f.kinds[CensusKindRelationship]++
		default:
			f.kinds[CensusKindGroup]++
			if f.Fields == nil {
				f.Fields = map[string]*CensusField{}
			}
			for _, k := range sortedAnyKeys(t) {
				sub := f.Fields[k]
				if sub == nil {
					sub = newCensusField()
					f.Fields[k] = sub
				}
				b.observeField(sub, t[k], ownerType, pattern+"."+k, concrete+"."+k, k, depth+1)
			}
		}
	default:
		f.kinds[CensusKindScalar]++
		f.track(v)
		if idLike(v) {
			b.markCandidate(f)
		}
	}
}

// markCandidate records that the current document holds an id-like value of
// f, which only a depth-1 read can tell from a plain number.
func (b *CensusBuilder) markCandidate(f *CensusField) {
	if f.seenIn == "" {
		f.seenIn = b.docKey
		b.candidate[b.docKey]++
	}
	set := b.candFields[b.docKey]
	if set == nil {
		set = map[*CensusField]bool{}
		b.candFields[b.docKey] = set
	}
	set[f] = true
}

// track counts one scalar value.
func (f *CensusField) track(v any) {
	key, ok := valueKey(v)
	if !ok {
		f.DistinctCapped = true
		return
	}
	if f.tracked == nil {
		f.tracked = map[string]*valueAcc{}
	}
	if acc, seen := f.tracked[key]; seen {
		acc.count++
		return
	}
	if len(f.tracked) >= maxCensusTracked {
		f.DistinctCapped = true
		return
	}
	f.tracked[key] = &valueAcc{value: v, count: 1, order: len(f.tracked)}
}

// Finish finalises the entity: kinds, values, enum detection, references.
func (b *CensusBuilder) Finish() *CensusEntity {
	for slug, t := range b.ent.Types {
		for _, f := range t.Fields {
			f.finish(b.keepValues)
		}
		refs := b.refs[slug]
		sort.SliceStable(refs, func(i, j int) bool {
			if refs[i].ref.Score != refs[j].ref.Score {
				return refs[i].ref.Score > refs[j].ref.Score
			}
			return refs[i].order < refs[j].order
		})
		t.Instances = []CensusRef{}
		for i := 0; i < len(refs) && i < maxCensusRefs; i++ {
			t.Instances = append(t.Instances, refs[i].ref)
		}
	}
	return b.ent
}

// finish settles one field (recursively): its kind, its values and whether
// it is enum-like.
func (f *CensusField) finish(keepValues bool) {
	f.Kind = pickKind(f.kinds)
	if f.Kind == CensusKindUnknown && f.Items == 0 && f.Types["array"] > 0 {
		// Only ever seen as []: an array or a blocks field, never filled in.
		f.Kind = CensusKindArray
	}
	for _, sub := range f.Fields {
		sub.finish(keepValues)
	}
	f.Distinct = len(f.tracked)
	accs := make([]*valueAcc, 0, len(f.tracked))
	for _, a := range f.tracked {
		accs = append(accs, a)
	}
	sort.Slice(accs, func(i, j int) bool {
		if accs[i].count != accs[j].count {
			return accs[i].count > accs[j].count
		}
		return accs[i].order < accs[j].order
	})
	f.EnumLike = enumLike(f)
	f.Values = nil
	for _, a := range accs {
		if len(f.Values) >= maxCensusValues {
			break
		}
		_, isBool := a.value.(bool)
		if !f.EnumLike && !isBool && a.count < 2 && !keepValues {
			continue
		}
		f.Values = append(f.Values, CensusValue{Value: a.value, Count: a.count})
	}
	f.tracked = nil
	f.kinds = nil
}

// enumLike decides whether a scalar field behaves like a select: enough
// observations, few distinct values, and each value used repeatedly. It is
// deliberately conservative — a false "enum" turns every new value into a
// warning.
func enumLike(f *CensusField) bool {
	if f.Kind != CensusKindScalar || f.DistinctCapped || f.NonNull < 4 {
		return false
	}
	if f.Types["boolean"] > 0 || f.Types["number"] > 0 {
		// Payload stores select and radio values as strings, so a number
		// field is never a select however few values it takes (the
		// form-builder's `width` is a free number that happens to be 50 or
		// 100 on every observed row).
		return false
	}
	d := len(f.tracked)
	// One value everywhere is a default, not a vocabulary: there is no
	// evidence that a second value would be wrong.
	if d < 2 || d > maxCensusValues {
		return false
	}
	if d*2 > f.NonNull {
		return false
	}
	// Select options are identifiers ("regular", "1-2", "inhalt", "36px").
	// Repeated human text ("Termin buchen", "Mehr erfahren") is copy that
	// happens to recur, and flagging a new label as off-vocabulary would be
	// exactly the noise this census exists to remove.
	for _, acc := range f.tracked {
		if !vocabularyValue(acc.value) {
			return false
		}
	}
	return true
}

// tokenLike reports a short string with no whitespace.
func tokenLike(s string) bool {
	if s == "" || len(s) > 40 {
		return false
	}
	return !strings.ContainsAny(s, " \t\n\r")
}

// vocabularyValue reports a value that can be a select option: a short
// identifier-like string. Payload stores select and radio values as strings,
// so a number never is one; and an option is an identifier ("regular",
// "grey-400", "1/1", "50%"), never a URL or path (/skills/x), an anchor or hex
// colour (#ff0000), an e-mail address, a file name or domain (logo.svg), or a
// CSS function (rgb(…)). Such values repeat because content repeats — eight
// cards linking to five pages — not because a field restricts them.
func vocabularyValue(v any) bool {
	s, ok := v.(string)
	if !ok || !tokenLike(s) {
		return false
	}
	return !LinkLikeValue(s)
}

// linkSchemeRe matches a URI scheme prefix (https:, mailto:, tel:).
var linkSchemeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

// fileOrDomainRe matches a trailing file extension or top-level domain
// (logo.svg, example.com).
var fileOrDomainRe = regexp.MustCompile(`\.[A-Za-z][A-Za-z0-9]{1,5}$`)

// LinkLikeValue reports a string that looks like a URL, path, anchor, colour,
// e-mail address or file/domain name rather than an identifier. A ratio such
// as "2/3" is an identifier: only a LEADING slash makes a path.
func LinkLikeValue(s string) bool {
	switch {
	case strings.HasPrefix(s, "/"), strings.HasPrefix(s, "./"), strings.HasPrefix(s, "../"),
		strings.HasPrefix(s, "#"), strings.HasPrefix(s, "?"):
		return true
	case strings.Contains(s, "://"), strings.ContainsAny(s, "@()"):
		return true
	case linkSchemeRe.MatchString(s), fileOrDomainRe.MatchString(s):
		return true
	}
	return false
}

// IsVocabulary reports whether the field behaves like a select: it was
// judged enum-like and every recorded value is an identifier. The second half
// re-checks a census written by a build whose rule accepted URLs, paths and
// numbers as vocabulary, so an old cache file cannot keep producing false
// value_not_in_enum warnings or `blocks new` defaults.
func (f *CensusField) IsVocabulary() bool {
	if f == nil || !f.EnumLike || f.Types["number"] > 0 {
		return false
	}
	for _, v := range f.Values {
		if !vocabularyValue(v.Value) {
			return false
		}
	}
	return true
}

// pickKind chooses the most informative kind seen. Mixed observations favour
// structure: one filled-in blocks array outweighs any number of empty ones.
func pickKind(kinds map[string]int) string {
	for _, k := range []string{CensusKindBlocks, CensusKindRichText, CensusKindUpload, CensusKindRelationship,
		CensusKindGroup, CensusKindArray, CensusKindList, CensusKindScalar} {
		if kinds[k] > 0 {
			return k
		}
	}
	if _, ok := kinds[CensusKindArray]; ok {
		return CensusKindArray
	}
	return CensusKindUnknown
}

// ProbeCandidates returns up to n documents worth re-reading at depth 1 to
// learn which id-like values are relationship or upload ids. They are chosen
// greedily so that, between them, they cover as many distinct id-like fields
// as possible — every field at least once when n allows. Ranking documents by
// how many id-like values they hold picked the three logo-heavy pages and
// left the image, gallery and person fields of other block types unprobed.
func (b *CensusBuilder) ProbeCandidates(n int) []any {
	uncovered := map[*CensusField]bool{}
	for _, set := range b.candFields {
		for f := range set {
			uncovered[f] = true
		}
	}
	chosen := map[string]bool{}
	var out []any
	for len(out) < n && len(uncovered) > 0 {
		best, bestN := "", 0
		for _, k := range b.docOrder {
			if chosen[k] {
				continue
			}
			c := 0
			for f := range b.candFields[k] {
				if uncovered[f] {
					c++
				}
			}
			if c > bestN {
				best, bestN = k, c
			}
		}
		if bestN == 0 {
			break
		}
		chosen[best] = true
		for f := range b.candFields[best] {
			delete(uncovered, f)
		}
		out = append(out, b.docIDs[best])
	}
	return out
}

// ApplyDepth1 walks a document read at depth 1 and marks every field whose
// depth-0 value was an id and whose depth-1 value is a whole document: that is
// a relationship, and an upload when the document is a file.
func (b *CensusBuilder) ApplyDepth1(doc map[string]any) {
	for _, k := range sortedAnyKeys(doc) {
		b.probeTop(doc[k], 0)
	}
}

func (b *CensusBuilder) probeTop(v any, depth int) {
	if depth > maxCensusDepth {
		return
	}
	switch t := v.(type) {
	case []any:
		if isBlocksArray(t) {
			b.probeRows(t, depth)
			return
		}
		for _, item := range t {
			if row, ok := item.(map[string]any); ok {
				for _, k := range sortedAnyKeys(row) {
					b.probeTop(row[k], depth+1)
				}
			}
		}
	case map[string]any:
		if isLexicalRoot(t) {
			return
		}
		for _, k := range sortedAnyKeys(t) {
			b.probeTop(t[k], depth+1)
		}
	}
}

func (b *CensusBuilder) probeRows(rows []any, depth int) {
	for _, item := range rows {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		bt, _ := row["blockType"].(string)
		t := b.ent.Types[bt]
		if t == nil {
			continue
		}
		probeFields(b, t.Fields, row, depth+1)
	}
}

func probeFields(b *CensusBuilder, fields map[string]*CensusField, row map[string]any, depth int) {
	for k, v := range row {
		f := fields[k]
		if f == nil {
			continue
		}
		probeField(b, f, v, depth)
	}
}

func probeField(b *CensusBuilder, f *CensusField, v any, depth int) {
	if depth > maxCensusDepth || v == nil {
		return
	}
	switch f.Kind {
	case CensusKindScalar:
		if doc, ok := v.(map[string]any); ok {
			if _, hasID := doc["id"]; hasID {
				f.Kind, f.KindSource = relationKindOf(doc), SourceObservedDepth1
				f.EnumLike = false
			}
		}
	case CensusKindList:
		if items, ok := v.([]any); ok {
			for _, it := range items {
				if doc, ok := it.(map[string]any); ok {
					if _, hasID := doc["id"]; hasID {
						f.Kind, f.KindSource = relationKindOf(doc), SourceObservedDepth1
						break
					}
				}
			}
		}
	case CensusKindGroup:
		if obj, ok := v.(map[string]any); ok {
			probeFields(b, f.Fields, obj, depth+1)
		}
	case CensusKindArray:
		if items, ok := v.([]any); ok {
			for _, it := range items {
				if obj, ok := it.(map[string]any); ok {
					probeFields(b, f.Fields, obj, depth+1)
				}
			}
		}
	case CensusKindBlocks:
		if items, ok := v.([]any); ok {
			b.probeRows(items, depth)
		}
	}
}

// relationKindOf tells an upload document from any other: Payload's upload
// collections carry filename and mimeType on every document.
func relationKindOf(doc map[string]any) string {
	_, hasFilename := doc["filename"]
	_, hasMime := doc["mimeType"]
	if hasFilename || hasMime {
		return CensusKindUpload
	}
	return CensusKindRelationship
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// isBlocksArray reports whether an array holds block rows: at least one object
// with a string blockType. A `{}` row inside such an array is still a block
// row — an orphan one.
func isBlocksArray(items []any) bool {
	for _, it := range items {
		if row, ok := it.(map[string]any); ok {
			if bt, ok := row["blockType"].(string); ok && bt != "" {
				return true
			}
		}
	}
	return false
}

func allObjects(items []any) bool {
	for _, it := range items {
		if _, ok := it.(map[string]any); !ok {
			return false
		}
	}
	return len(items) > 0
}

// isPolymorphicRef recognises a polymorphic relationship value at depth 0.
func isPolymorphicRef(m map[string]any) bool {
	if _, ok := m["relationTo"].(string); !ok {
		return false
	}
	_, ok := m["value"]
	return ok && len(m) <= 3
}

func allIDLike(items []any) bool {
	for _, it := range items {
		if !idLike(it) {
			return false
		}
	}
	return len(items) > 0
}

// idLike reports a value that could be a relationship id at depth 0: an
// integer, a 24-hex ObjectID or a UUID.
func idLike(v any) bool {
	switch t := v.(type) {
	case json.Number:
		_, err := strconv.ParseInt(t.String(), 10, 64)
		return err == nil
	case float64:
		return t == float64(int64(t))
	case string:
		return objectIDRe.MatchString(t) || uuidRe.MatchString(t)
	}
	return false
}

func markDoc(m map[string]map[string]bool, key, doc string) bool {
	set := m[key]
	if set == nil {
		set = map[string]bool{}
		m[key] = set
	}
	if set[doc] {
		return false
	}
	set[doc] = true
	return true
}

// nonEmptyValue is "has content": not null, not "", not [], not {}.
func nonEmptyValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		if isLexicalRoot(t) {
			return lexicalHasContent(t)
		}
		return len(t) > 0
	}
	return true
}

// lexicalHasContent reports whether a Lexical root has any text or non-text
// node below an empty paragraph.
func lexicalHasContent(m map[string]any) bool {
	root, _ := m["root"].(map[string]any)
	children, _ := root["children"].([]any)
	for _, c := range children {
		node, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if node["type"] != "paragraph" {
			return true
		}
		kids, _ := node["children"].([]any)
		for _, k := range kids {
			kn, _ := k.(map[string]any)
			if s, _ := kn["text"].(string); s != "" || kn["type"] != "text" {
				return true
			}
		}
	}
	return false
}

// jsonTypeName names a decoded JSON value's type.
func jsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number, float64, int, int64:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

// valueKey is a stable identity for a scalar value; false for values that are
// too long to be vocabulary.
func valueKey(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		if len(t) > maxCensusValueLen {
			return "", false
		}
		return "s:" + t, true
	case bool:
		return "b:" + strconv.FormatBool(t), true
	case json.Number:
		return "n:" + t.String(), true
	case float64:
		return "n:" + strconv.FormatFloat(t, 'f', -1, 64), true
	}
	return "", false
}

func sortedAnyKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ContainsBlockRows reports whether a value holds a blocks array anywhere —
// the trigger for block validation on a write body.
func ContainsBlockRows(v any) bool {
	return containsBlockRows(v, 0)
}

func containsBlockRows(v any, depth int) bool {
	if depth > maxCensusDepth {
		return false
	}
	switch t := v.(type) {
	case []any:
		if isBlocksArray(t) {
			return true
		}
		for _, it := range t {
			if containsBlockRows(it, depth+1) {
				return true
			}
		}
	case map[string]any:
		if isLexicalRoot(t) {
			return false
		}
		for _, sub := range t {
			if containsBlockRows(sub, depth+1) {
				return true
			}
		}
	}
	return false
}

// ValueAtRowPath reads a concrete census path (`layout[1].blocks[0]`,
// `hero.items[2]`) out of a document.
func ValueAtRowPath(doc map[string]any, path string) (any, bool) {
	var cur any = doc
	i := 0
	for i < len(path) {
		switch path[i] {
		case '.':
			i++
		case '[':
			end := strings.IndexByte(path[i:], ']')
			if end < 0 {
				return nil, false
			}
			n, err := strconv.Atoi(path[i+1 : i+end])
			if err != nil {
				return nil, false
			}
			list, ok := cur.([]any)
			if !ok || n < 0 || n >= len(list) {
				return nil, false
			}
			cur = list[n]
			i += end + 1
		default:
			end := strings.IndexAny(path[i:], ".[")
			if end < 0 {
				end = len(path) - i
			}
			obj, ok := cur.(map[string]any)
			if !ok {
				return nil, false
			}
			cur, ok = obj[path[i:i+end]]
			if !ok {
				return nil, false
			}
			i += end
		}
	}
	return cur, true
}

// ContainsContent reports whether a value has content: not null, not "", not
// [], not {}, and not an empty rich-text value.
func ContainsContent(v any) bool { return nonEmptyValue(v) }
