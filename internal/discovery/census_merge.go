package discovery

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// This file merges the census with everything else PayCLI knows about blocks —
// the profile pin, the project source and GraphQL introspection, all of which
// arrive through the entity's Shard — into the views the `pay blocks` catalog
// verbs print and the validator checks against. Every merged fact keeps the
// list of sources that support it.

// BlockKnowledge is the merged block knowledge for ONE entity.
type BlockKnowledge struct {
	Kind string
	Slug string
	// Entity is this entity's census, nil when none was read.
	Entity *CensusEntity
	// Census is the whole census, used for cross-entity facts about a block
	// type (a block used on pages and posts is the same block).
	Census *Census
	// Shard is the entity's field schema, nil when discovery had none.
	Shard *Shard

	accepted map[string]*AcceptedSet
	// aliases maps a census slug to the shard slug it was matched to
	// (cta -> callToActionBlock), which is how an observed slug reaches the
	// GraphQL block schema published under the union member.
	aliases map[string]string
}

// NewBlockKnowledge builds the merged view for one entity.
func NewBlockKnowledge(kind, slug string, census *Census, shard *Shard) *BlockKnowledge {
	k := &BlockKnowledge{Kind: kind, Slug: slug, Census: census, Shard: shard,
		accepted: map[string]*AcceptedSet{}, aliases: map[string]string{}}
	if e, ok := census.Entity(kind, slug); ok {
		k.Entity = e
	}
	// Reconcile every top-level blocks field up front: the aliases it
	// produces are needed before any block schema can be looked up.
	for _, p := range k.TopLevelPaths() {
		k.Accepted(p)
	}
	return k
}

// TopLevelPaths lists the entity's blocks fields: every path the schema
// declares plus every top-level pattern the census observed.
func (k *BlockKnowledge) TopLevelPaths() []string {
	set := map[string]bool{}
	if k.Shard != nil {
		for p := range k.Shard.BlockFields {
			set[p] = true
		}
		for p := range k.Shard.Blocks {
			set[p] = true
		}
	}
	if k.Entity != nil {
		for p, cp := range k.Entity.Paths {
			if cp.Parent == "" && !strings.Contains(p, "[]") {
				set[p] = true
			}
		}
	}
	return sortedBoolKeys(set)
}

// AcceptedType is one blockType a blocks field takes.
type AcceptedType struct {
	Slug string `json:"slug"`
	// Count is how many rows of this type the census saw at this path.
	Count         int      `json:"count"`
	Sources       []string `json:"sources"`
	InterfaceName string   `json:"interface_name,omitempty"`
	Label         *string  `json:"label,omitempty"`
	Description   *string  `json:"description,omitempty"`
}

// AcceptedSet is what one blocks-field path pattern takes.
type AcceptedSet struct {
	Path string `json:"path"`
	// Parent is the owning blockType for a parent-scoped pattern
	// (`group.blocks`), empty otherwise.
	Parent string         `json:"parent,omitempty"`
	Types  []AcceptedType `json:"types"`
	// Authoritative is true only when a declared source enumerates the whole
	// set: a profile pin, or a GraphQL union whose every member resolved to a
	// confirmed slug. Only then is "not in the set" an error rather than
	// "never observed here".
	Authoritative bool   `json:"authoritative"`
	Source        string `json:"source"`
	// InterfaceNames and Unconfirmed carry the GraphQL half when there is one.
	InterfaceNames []string `json:"interface_names,omitempty"`
	Unconfirmed    []string `json:"unconfirmed,omitempty"`
	Reason         string   `json:"reason,omitempty"`
	// Rows/Docs/EmptyRows/UntypedRows are the census counts at this path.
	Rows        int `json:"rows"`
	Docs        int `json:"docs"`
	EmptyRows   int `json:"empty_rows"`
	UntypedRows int `json:"untyped_rows"`
}

// Has reports whether slug is in the set.
func (a *AcceptedSet) Has(slug string) bool {
	if a == nil {
		return false
	}
	for _, t := range a.Types {
		if t.Slug == slug {
			return true
		}
	}
	return false
}

// Slugs returns the set's slugs in order.
func (a *AcceptedSet) Slugs() []string {
	if a == nil {
		return nil
	}
	out := make([]string, 0, len(a.Types))
	for _, t := range a.Types {
		out = append(out, t.Slug)
	}
	return out
}

// Accepted answers "what may go at this pattern". pattern is canonical
// (NormalizeBlockPath) or parent-scoped (`group.blocks`).
func (k *BlockKnowledge) Accepted(pattern string) *AcceptedSet {
	if k == nil {
		return &AcceptedSet{Path: pattern, Types: []AcceptedType{}, Source: SourceUnknown}
	}
	if a, ok := k.accepted[pattern]; ok {
		return a
	}
	var a *AcceptedSet
	switch {
	case strings.Contains(pattern, "[]"):
		a = k.observedAt(pattern)
	default:
		if parent, field, ok := k.parentScoped(pattern); ok {
			a = k.parentAccepts(parent, field)
		} else {
			a = k.reconcile(pattern)
		}
	}
	k.accepted[pattern] = a
	return a
}

// parentScoped recognises `group.blocks`: a first segment that is an observed
// blockType carrying a blocks field of that name, and not a top-level field.
func (k *BlockKnowledge) parentScoped(pattern string) (string, string, bool) {
	head, rest, ok := strings.Cut(pattern, ".")
	if !ok || strings.Contains(rest, ".") {
		return "", "", false
	}
	if k.Shard != nil {
		if _, isField := k.Shard.Field(head); isField {
			return "", "", false
		}
	}
	t := k.TypeStats(head)
	if t == nil {
		return "", "", false
	}
	if f, ok := t.Fields[rest]; ok && f.Kind == CensusKindBlocks {
		return head, rest, true
	}
	return "", "", false
}

func (k *BlockKnowledge) parentAccepts(parent, field string) *AcceptedSet {
	a := &AcceptedSet{Path: parent + "." + field, Parent: parent, Types: []AcceptedType{}, Source: SourceObservedCensus}
	t := k.TypeStats(parent)
	if t == nil {
		return a
	}
	f := t.Fields[field]
	if f == nil {
		return a
	}
	for _, s := range sortedCountKeys(f.Accepts) {
		a.Types = append(a.Types, AcceptedType{Slug: s, Count: f.Accepts[s], Sources: []string{SourceObservedCensus}})
		a.Rows += f.Accepts[s]
	}
	a.Reason = "observed only: the census lists what real " + parent + " rows contain, not everything the field allows"
	return a
}

// observedAt answers a nested pattern from the census alone. GraphQL never
// reaches below a top-level blocks field (§7.10a walks a block's own fields,
// not its nested unions), so a nested set is always observation.
func (k *BlockKnowledge) observedAt(pattern string) *AcceptedSet {
	a := &AcceptedSet{Path: pattern, Types: []AcceptedType{}, Source: SourceObservedCensus}
	if k.Entity == nil {
		a.Source = SourceUnknown
		return a
	}
	cp := k.Entity.Paths[pattern]
	if cp == nil {
		a.Source = SourceUnknown
		return a
	}
	for _, s := range sortedCountKeys(cp.Types) {
		a.Types = append(a.Types, AcceptedType{Slug: s, Count: cp.Types[s], Sources: []string{SourceObservedCensus}})
	}
	a.Rows, a.Docs, a.EmptyRows, a.UntypedRows = cp.Rows, cp.Docs, cp.EmptyRows, cp.UntypedRows
	a.Reason = "observed only: nested blocks fields are not published by any declared source this project exposes"
	return a
}

// reconcile merges a top-level field's declared slugs with the census.
func (k *BlockKnowledge) reconcile(path string) *AcceptedSet {
	a := &AcceptedSet{Path: path, Types: []AcceptedType{}}
	var observed map[string]int
	if k.Entity != nil {
		if cp := k.Entity.Paths[path]; cp != nil {
			observed = cp.Types
			a.Rows, a.Docs, a.EmptyRows, a.UntypedRows = cp.Rows, cp.Docs, cp.EmptyRows, cp.UntypedRows
		}
	}
	bf, haveBF := DeclaredBlockField(k.Shard, path)
	if !haveBF || len(bf.Slugs) == 0 {
		for _, s := range sortedCountKeys(observed) {
			a.Types = append(a.Types, AcceptedType{Slug: s, Count: observed[s], Sources: []string{SourceObservedCensus}})
		}
		a.Source = SourceObservedCensus
		if len(a.Types) == 0 {
			a.Source = SourceUnknown
		}
		if haveBF {
			a.InterfaceNames = bf.InterfaceNames
		}
		a.Reason = "observed only: no declared source (profile pin, project source or GraphQL introspection) " +
			"enumerates this field, so a blockType not listed here may still be valid"
		return a
	}

	a.InterfaceNames = bf.InterfaceNames
	added := map[string]bool{}
	add := func(t AcceptedType) {
		if added[t.Slug] {
			return
		}
		added[t.Slug] = true
		a.Types = append(a.Types, t)
	}
	declared := map[string]bool{}
	for _, s := range bf.Slugs {
		declared[s] = true
	}
	var unconfirmed []string
	for _, s := range bf.Slugs {
		src := bf.SlugSources[s]
		if src == "" {
			src = bf.Source
		}
		iface := bf.SlugInterfaces[s]
		if observed[s] > 0 {
			add(AcceptedType{Slug: s, Count: observed[s], Sources: []string{src, SourceObservedCensus}, InterfaceName: iface})
			continue
		}
		if src == SourceUnionInferred && iface != "" {
			var cands []string
			for _, o := range sortedCountKeys(observed) {
				if !declared[o] && !added[o] && slugMatchesInterface(o, iface) {
					cands = append(cands, o)
				}
			}
			if len(cands) == 1 {
				o := cands[0]
				add(AcceptedType{Slug: o, Count: observed[o], Sources: []string{SourceCensusMatched, SourceObservedCensus}, InterfaceName: iface})
				k.aliases[o] = s
				continue
			}
			unconfirmed = append(unconfirmed, s)
		}
		add(AcceptedType{Slug: s, Sources: []string{src}, InterfaceName: iface})
	}
	var extra []string
	for _, o := range sortedCountKeys(observed) {
		if !added[o] {
			add(AcceptedType{Slug: o, Count: observed[o], Sources: []string{SourceObservedCensus}})
			extra = append(extra, o)
		}
	}
	a.Unconfirmed = unconfirmed
	sort.SliceStable(a.Types, func(i, j int) bool { return a.Types[i].Slug < a.Types[j].Slug })

	complete := len(bf.Unresolved) == 0 && len(unconfirmed) == 0 && len(extra) == 0
	switch {
	case bf.Source == SourceConfigured:
		a.Authoritative, a.Source = true, SourceConfigured
	case bf.Source == SourceProjectSource && complete:
		a.Authoritative, a.Source = true, SourceProjectSource
	case len(bf.InterfaceNames) > 0 && complete:
		a.Authoritative = true
		a.Source = SourceGraphQL
		if bf.Source == SourceProjectSource {
			a.Source = SourceProjectSource
		}
	default:
		a.Source = SourceMixed
	}
	switch {
	case len(unconfirmed) > 0:
		a.Reason = fmt.Sprintf("%s %s inferred from a GraphQL interfaceName and never observed in a document, "+
			"so the real slug may differ; a blockType outside this list is reported as a warning, not an error",
			strings.Join(unconfirmed, ", "), plural(len(unconfirmed), "was", "were"))
	case len(extra) > 0:
		a.Reason = fmt.Sprintf("%s %s observed in documents but not named by the declared source",
			strings.Join(extra, ", "), plural(len(extra), "was", "were"))
	case len(bf.Unresolved) > 0:
		a.Reason = bf.Reason
	}
	return a
}

// DeclaredBlockField returns a top-level blocks field's declared answer from a
// shard: its block_fields entry, or — for a shard written before block_fields
// existed — the legacy blocks/blocks_source pair.
func DeclaredBlockField(shard *Shard, path string) (BlockField, bool) {
	if shard == nil {
		return BlockField{}, false
	}
	if bf, ok := shard.BlockFieldFor(path); ok {
		return bf, true
	}
	slugs, ok := shard.Blocks[path]
	if !ok || len(slugs) == 0 {
		return BlockField{}, false
	}
	source := orUnknownSource(shard.BlocksSource)
	return BlockField{Path: path, Slugs: slugs, Source: source, SlugSources: uniformSources(slugs, source)}, true
}

// slugMatchesInterface decides whether an observed slug is the real name of a
// GraphQL union member: ContentBlock ↔ content, ArchiveBlock ↔ archive,
// CallToActionBlock ↔ cta (initials), MediaBlock ↔ mediaBlock. A join is only
// ever made when exactly one observed slug matches one unconfirmed member.
func slugMatchesInterface(slug, iface string) bool {
	s := strings.ToLower(slug)
	i := strings.ToLower(iface)
	if s == "" || i == "" {
		return false
	}
	stem := strings.TrimSuffix(i, "block")
	if s == i || s == stem || strings.TrimSuffix(s, "block") == stem {
		return true
	}
	// Initials are how an acronym slug is named after a long interface
	// (cta ↔ CallToAction); a mere prefix is not evidence (text ↔ Textarea).
	return len(s) >= 2 && initials(strings.TrimSuffix(iface, "Block")) == s
}

func initials(pascal string) string {
	var b strings.Builder
	for i, r := range pascal {
		if i == 0 || unicode.IsUpper(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// ShardSlug returns the slug the shard knows a census slug by.
func (k *BlockKnowledge) ShardSlug(slug string) string {
	if k == nil {
		return slug
	}
	if s, ok := k.aliases[slug]; ok {
		return s
	}
	return slug
}

// GraphQLSchema returns the declared block schema for a (census) slug.
func (k *BlockKnowledge) GraphQLSchema(slug string) (BlockTypeSchema, bool) {
	if k == nil || k.Shard == nil {
		return BlockTypeSchema{}, false
	}
	return k.Shard.BlockSchemaFor(k.ShardSlug(slug))
}

// TypeStats returns what the census observed about a blockType in THIS
// entity. Only when this entity has never used the type does it fall back to
// other entities — and then only to the ones whose rows have the same shape,
// because a slug is not globally unique: the form-builder's `text` field block
// (name, label, width, required) and a page's `text` content block share a
// slug and nothing else.
func (k *BlockKnowledge) TypeStats(slug string) *CensusType {
	if k == nil {
		return nil
	}
	if k.Entity != nil {
		if t := k.Entity.Types[slug]; t != nil {
			return t
		}
	}
	var parts []*CensusType
	for _, e := range k.Census.Entities() {
		if t := e.Types[slug]; t != nil {
			parts = append(parts, t)
		}
	}
	return MergeCompatibleTypes(parts...)
}

// MergeCompatibleTypes merges the parts that share the shape of the largest
// one and drops the rest.
func MergeCompatibleTypes(parts ...*CensusType) *CensusType {
	var in []*CensusType
	for _, p := range parts {
		if p != nil {
			in = append(in, p)
		}
	}
	if len(in) == 0 {
		return nil
	}
	sort.SliceStable(in, func(i, j int) bool { return in[i].Count > in[j].Count })
	keep := []*CensusType{in[0]}
	for _, t := range in[1:] {
		if CompatibleTypes(in[0], t) {
			keep = append(keep, t)
		}
	}
	return MergeCensusTypes(keep...)
}

// CompatibleTypes reports whether two observations of one slug describe the
// same block: at least 80% of the union of their row keys is shared.
func CompatibleTypes(a, b *CensusType) bool {
	if a == nil || b == nil {
		return false
	}
	union, shared := 0, 0
	for k := range a.Fields {
		union++
		if _, ok := b.Fields[k]; ok {
			shared++
		}
	}
	for k := range b.Fields {
		if _, ok := a.Fields[k]; !ok {
			union++
		}
	}
	if union == 0 {
		return true
	}
	return shared*5 >= union*4
}

// KnownTypes lists every blockType this entity can name: census, then shard.
func (k *BlockKnowledge) KnownTypes() []string {
	set := map[string]bool{}
	if k.Entity != nil {
		for s := range k.Entity.Types {
			set[s] = true
		}
	}
	for _, p := range k.TopLevelPaths() {
		for _, s := range k.Accepted(p).Slugs() {
			set[s] = true
		}
	}
	return sortedBoolKeys(set)
}

// HasObservedBlocks reports whether this entity's census saw at least one
// block row. Without that, "never observed" says nothing about the entity: it
// was not read, or the read stopped on documents that hold no blocks.
func (k *BlockKnowledge) HasObservedBlocks() bool {
	return k != nil && k.Entity != nil && (k.Entity.BlockRows > 0 || len(k.Entity.Types) > 0)
}

// ownType reports whether this entity's own rows include slug.
func (k *BlockKnowledge) ownType(slug string) bool {
	return k != nil && k.Entity != nil && k.Entity.Types[slug] != nil
}

// ProjectTypes lists every blockType observed anywhere in the census.
func (k *BlockKnowledge) ProjectTypes() map[string]bool {
	set := map[string]bool{}
	for _, e := range k.Census.Entities() {
		for s := range e.Types {
			set[s] = true
		}
	}
	return set
}

// ---------------------------------------------------------------------------
// merging several entities' observations of one type
// ---------------------------------------------------------------------------

// MergeCensusTypes combines observations of one blockType from several
// entities. With one input it returns that input unchanged.
func MergeCensusTypes(parts ...*CensusType) *CensusType {
	var in []*CensusType
	for _, p := range parts {
		if p != nil {
			in = append(in, p)
		}
	}
	switch len(in) {
	case 0:
		return nil
	case 1:
		return in[0]
	}
	out := &CensusType{Slug: in[0].Slug, UsedIn: map[string]int{}, Fields: map[string]*CensusField{}}
	for _, t := range in {
		out.Count += t.Count
		out.Docs += t.Docs
		for p, n := range t.UsedIn {
			out.UsedIn[p] += n
		}
		for name, f := range t.Fields {
			out.Fields[name] = mergeCensusField(out.Fields[name], f)
		}
		out.Instances = append(out.Instances, t.Instances...)
	}
	sort.SliceStable(out.Instances, func(i, j int) bool { return out.Instances[i].Score > out.Instances[j].Score })
	if len(out.Instances) > maxCensusRefs {
		out.Instances = out.Instances[:maxCensusRefs]
	}
	return out
}

func mergeCensusField(a, b *CensusField) *CensusField {
	if a == nil {
		c := *b
		c.Types = copyCounts(b.Types)
		c.Accepts = copyCounts(b.Accepts)
		c.Values = append([]CensusValue{}, b.Values...)
		if b.Fields != nil {
			c.Fields = map[string]*CensusField{}
			for k, v := range b.Fields {
				c.Fields[k] = mergeCensusField(nil, v)
			}
		}
		return &c
	}
	a.Kind = pickKind(map[string]int{a.Kind: 1, b.Kind: 1})
	if b.KindSource == SourceObservedDepth1 {
		a.KindSource = b.KindSource
		a.Kind = b.Kind
	}
	for k, n := range b.Types {
		a.Types[k] += n
	}
	a.Present += b.Present
	a.NonNull += b.NonNull
	a.NonEmpty += b.NonEmpty
	a.Items += b.Items
	if len(b.Accepts) > 0 && a.Accepts == nil {
		a.Accepts = map[string]int{}
	}
	for k, n := range b.Accepts {
		a.Accepts[k] += n
	}
	a.Values = mergeValues(a.Values, b.Values)
	a.DistinctCapped = a.DistinctCapped || b.DistinctCapped
	a.Optional = a.Optional || b.Optional
	if b.Distinct > a.Distinct {
		a.Distinct = b.Distinct
	}
	// Enum-like only if every part that saw a value was: a part that was not
	// kept only its repeated values, so the merged list could be missing the
	// very value it would then flag.
	switch {
	case b.NonNull == 0:
	case a.NonNull-b.NonNull == 0:
		a.EnumLike = b.EnumLike
	default:
		a.EnumLike = a.EnumLike && b.EnumLike
	}
	a.EnumLike = a.EnumLike && !a.DistinctCapped && len(a.Values) <= maxCensusValues
	for k, v := range b.Fields {
		if a.Fields == nil {
			a.Fields = map[string]*CensusField{}
		}
		a.Fields[k] = mergeCensusField(a.Fields[k], v)
	}
	return a
}

func mergeValues(a, b []CensusValue) []CensusValue {
	idx := map[string]int{}
	out := append([]CensusValue{}, a...)
	for i, v := range out {
		key, _ := valueKey(v.Value)
		idx[key] = i
	}
	for _, v := range b {
		key, _ := valueKey(v.Value)
		if i, ok := idx[key]; ok {
			out[i].Count += v.Count
			continue
		}
		idx[key] = len(out)
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	if len(out) > maxCensusValues {
		out = out[:maxCensusValues]
	}
	return out
}

func copyCounts(m map[string]int) map[string]int {
	if m == nil {
		return nil
	}
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// sortedCountKeys sorts a count map's keys by count descending, then name.
func sortedCountKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if m[out[i]] != m[out[j]] {
			return m[out[i]] > m[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

// ---------------------------------------------------------------------------
// path patterns
// ---------------------------------------------------------------------------

// NormalizeBlockPath turns any concrete or addressed blocks path into the
// census's canonical PATTERN: every index or selector becomes `[]`.
//
//	layout                      -> layout
//	layout.2.blocks             -> layout[].blocks
//	layout[2].blocks            -> layout[].blocks
//	layout[id:6ab…].blocks      -> layout[].blocks
//	layout[type:group[1]].blocks-> layout[].blocks
//	layout[].blocks[].blocks    -> layout[].blocks[].blocks
//
// A path the census itself printed is returned unchanged.
func NormalizeBlockPath(p string) string {
	p = strings.TrimSpace(p)
	var out strings.Builder
	depth := 0
	seg := strings.Builder{}
	flush := func() {
		s := seg.String()
		seg.Reset()
		if s == "" {
			return
		}
		if isAllDigits(s) || (strings.HasPrefix(s, "-") && isAllDigits(s[1:])) {
			out.WriteString("[]")
			return
		}
		if out.Len() > 0 {
			out.WriteByte('.')
		}
		out.WriteString(s)
	}
	for _, r := range p {
		switch {
		case r == '[':
			if depth == 0 {
				flush()
				out.WriteString("[]")
			}
			depth++
		case r == ']':
			if depth > 0 {
				depth--
			}
		case depth > 0:
			// inside a selector: dropped
		case r == '.':
			flush()
		default:
			seg.WriteRune(r)
		}
	}
	flush()
	return out.String()
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// the schema view (`pay blocks schema`)
// ---------------------------------------------------------------------------

// BlockUse is one place a blockType is used.
type BlockUse struct {
	Kind  string `json:"kind"`
	Slug  string `json:"slug"`
	Path  string `json:"path"`
	Count int    `json:"count"`
}

// BlockFieldView is one row of a block's field table.
type BlockFieldView struct {
	// Path is the field's path inside the row: `title`, `section.contentWidth`,
	// `buttons[].label`.
	Path       string   `json:"path"`
	Name       string   `json:"name"`
	Depth      int      `json:"depth"`
	Kind       string   `json:"kind"`
	KindSource string   `json:"kind_source"`
	JSONTypes  []string `json:"json_types"`
	// PayloadType is the declared type when GraphQL or project source has one.
	PayloadType *string `json:"payload_type"`
	// Observed is how many rows (or group/array entries) this field's
	// percentages are relative to.
	Observed      int           `json:"observed"`
	PresencePct   int           `json:"presence_pct"`
	NonNullPct    int           `json:"non_null_pct"`
	Values        []CensusValue `json:"values"`
	EnumLike      bool          `json:"enum_like"`
	Options       []string      `json:"options"`
	OptionsSource string        `json:"options_source"`
	// Required is "true", "false", "likely" or "unknown".
	Required       string   `json:"required"`
	RequiredSource string   `json:"required_source"`
	Accepts        []string `json:"accepts,omitempty"`
	RelationTo     []string `json:"relation_to,omitempty"`
	Plumbing       bool     `json:"plumbing"`
	Description    *string  `json:"description"`
	Sources        []string `json:"sources"`
}

// BlockSchemaView is `pay blocks schema <type>`.
type BlockSchemaView struct {
	BlockType     string           `json:"block_type"`
	InterfaceName string           `json:"interface_name,omitempty"`
	Label         *string          `json:"label"`
	Description   *string          `json:"description"`
	Count         int              `json:"count"`
	Docs          int              `json:"docs"`
	UsedIn        []BlockUse       `json:"used_in"`
	Sources       []string         `json:"sources"`
	Fields        []BlockFieldView `json:"fields"`
	Required      []string         `json:"required_fields"`
	Likely        []string         `json:"likely_required"`
	BestInstance  *CensusRef       `json:"best_instance"`
}

// likelyRequiredMin is the smallest number of observations "every row filled
// this in" is allowed to mean anything.
const likelyRequiredMin = 3

// BuildSchemaView merges a type's census profile with its declared schema.
// stats or gql may be missing, never both.
func BuildSchemaView(slug string, stats *CensusType, gql *BlockTypeSchema, uses []BlockUse) BlockSchemaView {
	v := BlockSchemaView{BlockType: slug, UsedIn: uses, Fields: []BlockFieldView{}, Required: []string{}, Likely: []string{}, Sources: []string{}}
	if v.UsedIn == nil {
		v.UsedIn = []BlockUse{}
	}
	declared := map[string]BlockFieldSchema{}
	if gql != nil {
		v.InterfaceName = gql.InterfaceName
		v.Label, v.Description = gql.Label, gql.Description
		v.Sources = append(v.Sources, orUnknownSource(gql.FieldsSource))
		for _, f := range gql.Fields {
			declared[f.Path] = f
		}
	}
	if stats != nil {
		v.Count, v.Docs = stats.Count, stats.Docs
		v.Sources = append(v.Sources, SourceObservedCensus)
		if len(stats.Instances) > 0 {
			ref := stats.Instances[0]
			v.BestInstance = &ref
		}
		walkViewFields(&v, stats.Fields, "", "", 0, stats.Count, declared)
	}
	// Declared fields the census never saw. GraphQL paths carry no array
	// markers (links.link.url), so each is re-rooted under the census path of
	// its nearest observed ancestor (links[].link.url).
	viewPath := map[string]string{}
	for _, f := range v.Fields {
		viewPath[strings.ReplaceAll(f.Path, "[]", "")] = f.Path
	}
	if gql != nil {
		for _, f := range gql.Fields {
			if _, seen := viewPath[f.Path]; seen || f.Name == "id" || f.Name == "blockType" {
				continue
			}
			path := f.Path
			if head, _, ok := cutLast(f.Path); ok {
				if parent, known := viewPath[head]; known {
					path = parent + "." + f.Name
					if parentIsArray(v.Fields, parent) {
						path = parent + "[]." + f.Name
					}
				}
			}
			fv := BlockFieldView{
				Path: path, Name: f.Name, Depth: strings.Count(f.Path, "."),
				Kind: censusKindForPayload(f.PayloadType), KindSource: orUnknownSource(f.PayloadTypeSource),
				JSONTypes: []string{}, Values: []CensusValue{},
				OptionsSource: SourceUnknown, Required: "unknown", RequiredSource: SourceUnknown,
				PayloadType: strPtr(f.PayloadType), Plumbing: f.Plumbing, Description: f.Description,
				Sources: []string{orUnknownSource(f.PayloadTypeSource)},
			}
			applyDeclared(&fv, f)
			v.Fields = append(v.Fields, fv)
		}
	}
	for _, f := range v.Fields {
		if f.Depth > 0 {
			continue
		}
		switch f.Required {
		case "true":
			v.Required = append(v.Required, f.Path)
		case "likely":
			v.Likely = append(v.Likely, f.Path)
		}
	}
	return v
}

// cutLast splits a dotted path at its last dot.
func cutLast(p string) (string, string, bool) {
	i := strings.LastIndexByte(p, '.')
	if i < 0 {
		return "", p, false
	}
	return p[:i], p[i+1:], true
}

func parentIsArray(fields []BlockFieldView, path string) bool {
	for _, f := range fields {
		if f.Path == path {
			return f.Kind == CensusKindArray
		}
	}
	return false
}

func walkViewFields(v *BlockSchemaView, fields map[string]*CensusField, prefix, joinPrefix string,
	depth, base int, declared map[string]BlockFieldSchema) {
	names := make([]string, 0, len(fields))
	for n := range fields {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		f := fields[name]
		path, join := name, name
		if prefix != "" {
			path, join = prefix+"."+name, joinPrefix+"."+name
		}
		fv := BlockFieldView{
			Path: path, Name: name, Depth: depth, Kind: f.Kind, KindSource: f.KindSource,
			JSONTypes: sortedCountKeys(f.Types), Observed: base,
			PresencePct: pct(f.Present, base), NonNullPct: pct(f.NonNull, base),
			Values: f.Values, EnumLike: f.IsVocabulary(), OptionsSource: SourceUnknown,
			Required: "unknown", RequiredSource: SourceUnknown,
			Plumbing: name == "blockName" && depth == 0,
			Sources:  []string{SourceObservedCensus},
		}
		if fv.Values == nil {
			fv.Values = []CensusValue{}
		}
		if len(f.Accepts) > 0 {
			fv.Accepts = sortedCountKeys(f.Accepts)
		}
		if d, ok := declared[join]; ok {
			applyDeclared(&fv, d)
		}
		if fv.Required == "unknown" && likelyRequired(f, base) && !fv.Plumbing {
			fv.Required, fv.RequiredSource = "likely", SourceObservedCensus
		}
		if fv.Plumbing {
			fv.Required, fv.RequiredSource = "false", SourcePayloadProtocol
		}
		v.Fields = append(v.Fields, fv)
		switch f.Kind {
		case CensusKindGroup:
			walkViewFields(v, f.Fields, path, join, depth+1, f.NonNull, declared)
		case CensusKindArray:
			walkViewFields(v, f.Fields, path+"[]", join, depth+1, f.Items, declared)
		}
	}
}

// applyDeclared overlays a declared (GraphQL / project-source) field onto a
// view row. A declared fact is never overwritten by an observation.
func applyDeclared(fv *BlockFieldView, d BlockFieldSchema) {
	if d.PayloadType != "" && d.PayloadType != TypeUnknown {
		fv.PayloadType = strPtr(d.PayloadType)
		fv.Sources = appendUnique(fv.Sources, orUnknownSource(d.PayloadTypeSource))
	}
	if len(d.Options) > 0 {
		fv.Options, fv.OptionsSource = d.Options, d.OptionsSource
	}
	if d.Required != nil {
		fv.Required = strconv.FormatBool(*d.Required)
		fv.RequiredSource = d.RequiredSource
	}
	if len(d.RelationTo) > 0 {
		fv.RelationTo = d.RelationTo
	}
	if d.Description != nil {
		fv.Description = d.Description
	}
	if d.Plumbing {
		fv.Plumbing = true
	}
}

// likelyRequired: filled in on every one of at least three observed rows, for
// a kind where "always filled" means something. Checkboxes, select-like
// fields and scalars that hold the same value everywhere are excluded,
// because Payload's defaultValue fills those on every row whether or not the
// field is required.
func likelyRequired(f *CensusField, base int) bool {
	if f.Optional || base < likelyRequiredMin || f.NonNull < base || f.NonEmpty < base {
		return false
	}
	switch f.Kind {
	case CensusKindScalar:
		// A value that never varies is a defaultValue at work, not an author
		// filling the field in; a select-like vocabulary is the same.
		varied := f.DistinctCapped || f.Distinct >= 2
		if f.Types["number"] > 0 && !f.DistinctCapped && f.Distinct <= maxCensusValues && f.Distinct*2 <= f.NonNull {
			// A number set to one of a few values on every row (a width of
			// 50 or 100) is a layout knob with a default at work.
			return false
		}
		return varied && !f.IsVocabulary() && f.Types["boolean"] == 0
	case CensusKindRichText, CensusKindRelationship, CensusKindUpload:
		return true
	}
	return false
}

func pct(n, base int) int {
	if base <= 0 {
		return 0
	}
	return (n*100 + base/2) / base
}

func appendUnique(ss []string, s string) []string {
	for _, have := range ss {
		if have == s {
			return ss
		}
	}
	return append(ss, s)
}

func censusKindForPayload(t string) string {
	switch t {
	case TypeBlocks:
		return CensusKindBlocks
	case TypeArray:
		return CensusKindArray
	case TypeRichText:
		return CensusKindRichText
	case TypeGroup:
		return CensusKindGroup
	case TypeRelationship:
		return CensusKindRelationship
	case TypeUpload:
		return CensusKindUpload
	case TypeUnknown, "":
		return CensusKindUnknown
	}
	return CensusKindScalar
}

// ---------------------------------------------------------------------------
// a new row (`pay blocks new`)
// ---------------------------------------------------------------------------

// DefaultRichTextValue returns a fresh copy of Payload's own empty rich-text
// value — @payloadcms/richtext-lexical's defaultRichTextValue (3.87,
// dist/populateGraphQL/defaultValue.js): one paragraph holding one empty text
// node. Payload's hasText() reports it as empty, so a required rich-text field
// still fails validation until it is filled in.
func DefaultRichTextValue() map[string]any {
	return map[string]any{
		"root": map[string]any{
			"type": "root",
			"children": []any{map[string]any{
				"type": "paragraph",
				"children": []any{map[string]any{
					"type": "text", "detail": json.Number("0"), "format": json.Number("0"),
					"mode": "normal", "style": "", "text": "", "version": json.Number("1"),
				}},
				"direction": nil, "format": "", "indent": json.Number("0"),
				"textFormat": json.Number("0"), "textStyle": "", "version": json.Number("1"),
			}},
			"direction": nil, "format": "", "indent": json.Number("0"), "version": json.Number("1"),
		},
	}
}

// newRowPresence is the share of observed rows a field must appear in before
// `pay blocks new` includes it. Payload returns every declared field at
// depth 0, so a real field is present in ~100% of rows; the threshold only
// filters keys that belong to a retired version of the block.
const newRowPresence = 50

// NewBlockRow builds a ready-to-edit row for a blockType: every commonly
// present field, the most common value of an enum-like or boolean field, an
// empty rich-text value, [] for nested blocks and arrays, null for everything
// else. stats may be nil (GraphQL-only knowledge); gql may be nil (census
// only). It returns the fields it filled from an observed value, so the
// caller can say which values are defaults it chose.
func NewBlockRow(slug string, stats *CensusType, gql *BlockTypeSchema) (map[string]any, []string) {
	row := map[string]any{"blockType": slug}
	var chosen []string
	if stats != nil && stats.Count > 0 {
		fillRow(row, stats.Fields, stats.Count, "", &chosen)
	}
	if gql != nil {
		for _, f := range gql.Fields {
			if f.Parent != nil || f.Name == "id" || f.Name == "blockType" {
				continue
			}
			if _, have := row[f.Name]; have {
				continue
			}
			if stats != nil && stats.Count > 0 && (f.Required == nil || !*f.Required) {
				// Observed rows never carried it and nothing says it is required.
				continue
			}
			row[f.Name] = declaredDefault(f.PayloadType)
		}
	}
	sort.Strings(chosen)
	return row, chosen
}

func fillRow(row map[string]any, fields map[string]*CensusField, base int, prefix string, chosen *[]string) {
	for name, f := range fields {
		if pct(f.Present, base) < newRowPresence {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		row[name] = templateValue(f, path, chosen)
	}
}

func templateValue(f *CensusField, path string, chosen *[]string) any {
	switch f.Kind {
	case CensusKindBlocks, CensusKindArray, CensusKindList:
		return []any{}
	case CensusKindRichText:
		return DefaultRichTextValue()
	case CensusKindGroup:
		obj := map[string]any{}
		if f.NonNull > 0 {
			fillRow(obj, f.Fields, f.NonNull, path, chosen)
		}
		return obj
	case CensusKindScalar:
		boolOnly := f.Types["boolean"] > 0 && f.Types["string"] == 0 && f.Types["number"] == 0
		constant := len(f.Values) == 1 && f.Values[0].Count == f.NonNull && f.NonNull >= 2 && f.NonNull == f.Present
		if (f.IsVocabulary() || boolOnly || constant) && len(f.Values) > 0 {
			if v, isString := f.Values[0].Value.(string); isString && (v == "" || LinkLikeValue(v)) {
				// An empty string is an unfilled text field, not a choice; and
				// a link copied from another row would silently point the new
				// one at the wrong page.
				return nil
			}
			*chosen = append(*chosen, path)
			return f.Values[0].Value
		}
	}
	return nil
}

func declaredDefault(payloadType string) any {
	switch payloadType {
	case TypeBlocks, TypeArray:
		return []any{}
	case TypeRichText:
		return DefaultRichTextValue()
	case TypeGroup:
		return map[string]any{}
	}
	return nil
}
