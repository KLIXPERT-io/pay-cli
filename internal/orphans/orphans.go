// Package orphans models how Payload's Postgres adapter clears NESTED rows on
// an update, so PayCLI can predict orphan rows before a write, detect them
// after one, and remove them without deleting the document.
//
// # The adapter's write path (@payloadcms/drizzle 3.87.1, verified)
//
// Every block TYPE has ONE table per collection (`pages_blocks_bento_item`),
// whatever depth its rows sit at. A block row's `_parentID` is the DOCUMENT
// id, not the id of the row it is nested in, and `_path` records where it
// sits (`layout.0.blocks.0.blocks`). An array row's `_parentID` is the id of
// the row that holds it, with ON DELETE CASCADE.
//
// On update, upsertRow (dist/upsertRow/index.js ~l.516) deletes, for every
// table in `blocksToDelete`, ALL rows `WHERE _parentID = <document id>` and
// then inserts the new rows. transform/write/traverseFields.js ~l.130 adds a
// blocks field's tables to that set when the TRAVERSAL of the new data
// reaches the field — before looking at the field's value. The traversal
// always reaches every top-level field (Payload hands the adapter the whole
// merged document), but it reaches a blocks field inside a block (or an array
// row) only by walking a row of that parent type. So:
//
//   - top-level blocks tables are always cleared (top-level swaps are clean);
//   - a nested block type's table is cleared only when the new document still
//     contains a row of a type whose nested blocks field accepts it;
//   - nested arrays are removed by the cascade from their parent row, never on
//     their own.
//
// A restructure such as group → bento → bentoItem ⇒ group → grid → text leaves
// every bentoItem row in place. On read (transform/read, createBlocksMap) rows
// are grouped by `_path` across ALL block tables, so the stale rows reappear
// inside whatever row now sits at their old path: as `{}` when that field does
// not accept their type, as real rows when it does, or invisibly when no field
// has that path.
//
// Nothing here performs I/O; values are the generic shapes encoding/json
// produces and inputs are never mutated.
package orphans

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/lexical"
)

// RootKind is the parent kind of a list that is not inside any row.
const RootKind = "root"

// blockPrefix/arrayPrefix start a row kind: "block:<slug>" for a block row,
// "array:<container>" for an array row.
const (
	blockPrefix = "block:"
	arrayPrefix = "array:"
)

// BlockKind is the row kind of a block row of type slug.
func BlockKind(slug string) string { return blockPrefix + slug }

// isBlockKind reports whether kind names a block type (it has a table).
func isBlockKind(kind string) bool { return strings.HasPrefix(kind, blockPrefix) }

// SlugOf is the block type of a block kind ("" for any other kind).
func SlugOf(kind string) string {
	if !isBlockKind(kind) {
		return ""
	}
	return strings.TrimPrefix(kind, blockPrefix)
}

// Container identifies a list by what holds it: the kind of the enclosing row
// (RootKind outside every row) and the list's path inside that row, through
// named groups ("blocks", "section.items", or "layout" / "hero.links" at the
// root). Two lists with the same Container are the same field definition.
type Container struct {
	Parent string
	Field  string
}

// String renders "parent>field".
func (c Container) String() string { return c.Parent + ">" + c.Field }

// Ancestor is one row on the way down to a nested row.
type Ancestor struct {
	// Path is the ancestor row's own path, List the list it sits in.
	Path, List string
	// Kind is the ancestor's row kind.
	Kind string
	// Field is the list inside the ancestor that leads to the next row.
	Field string
	// Row is the ancestor row itself (not copied; never mutated here).
	Row map[string]any
}

// Row is one element of a row list (a blocks or array field value).
type Row struct {
	// Path is the row's path (`layout[0].blocks[1]`), List its list's path.
	Path, List string
	// Kind is BlockKind(blockType) for a block row, "array:<container>" for an
	// array row, and "" for an empty object.
	Kind string
	// BlockType is the row's blockType ("" for array rows and {} rows).
	BlockType string
	// Container is the field definition the row belongs to.
	Container Container
	// Depth is the number of rows enclosing this one (0 = a top-level list).
	Depth int
	// Empty marks a row that is `{}` — how Payload reads back an orphan row
	// whose type the field at its old path does not accept.
	Empty bool
	// Anc are the enclosing rows, outermost first.
	Anc []Ancestor
	// Value is the row itself (not copied).
	Value map[string]any
}

// Rows lists every row of every row list in doc, depth first, keys sorted.
// Rich-text editor states and relationship references are not rows.
func Rows(doc map[string]any) []Row {
	w := &walker{}
	w.object(doc, "", RootKind, "", 0, nil, nil)
	return w.out
}

type walker struct{ out []Row }

// object walks a document or a group inside one. parentKind is the kind of
// the row the object belongs to; rel is the object's path inside that row;
// self is that row as an ancestor-to-be (nil at the root).
func (w *walker) object(obj map[string]any, path, parentKind, rel string, depth int, anc []Ancestor, self *Ancestor) {
	for _, k := range sortedKeys(obj) {
		cpath, crel := join(path, k), join(rel, k)
		switch t := obj[k].(type) {
		case map[string]any:
			if lexical.IsEditorState(t) {
				continue
			}
			w.object(t, cpath, parentKind, crel, depth, anc, self)
		case []any:
			if !isRowList(t) {
				continue
			}
			next := anc
			if self != nil {
				a := *self
				a.Field = crel
				next = append(append([]Ancestor(nil), anc...), a)
			}
			w.list(t, cpath, Container{Parent: parentKind, Field: crel}, depth, next)
		}
	}
}

func (w *walker) list(list []any, path string, c Container, depth int, anc []Ancestor) {
	for i, el := range list {
		m, ok := el.(map[string]any)
		if !ok {
			continue
		}
		r := Row{Path: path + "[" + strconv.Itoa(i) + "]", List: path, Container: c, Depth: depth, Anc: anc, Value: m}
		switch bt, _ := m["blockType"].(string); {
		case bt != "":
			r.BlockType, r.Kind = bt, BlockKind(bt)
		case len(m) == 0:
			r.Empty = true
		default:
			r.Kind = arrayPrefix + c.String()
		}
		w.out = append(w.out, r)
		if r.Empty {
			continue
		}
		self := &Ancestor{Path: r.Path, List: path, Kind: r.Kind, Row: m}
		w.object(m, r.Path, r.Kind, "", depth+1, anc, self)
	}
}

// isRowList reports whether a list holds rows: at least one element is an
// object that is not a relationship reference ({relationTo, value}).
func isRowList(list []any) bool {
	for _, el := range list {
		m, ok := el.(map[string]any)
		if !ok {
			continue
		}
		if isRelationRef(m) {
			return false
		}
		return true
	}
	return false
}

func isRelationRef(m map[string]any) bool {
	if len(m) != 2 {
		return false
	}
	_, a := m["relationTo"]
	_, b := m["value"]
	return a && b
}

// EmptyRows lists the paths of every `{}` row in doc. Payload never returns an
// empty object for a real row (every row carries at least its id), so each one
// is a row the adapter read from a table the field at that path does not
// accept — an orphan.
func EmptyRows(doc map[string]any) []string {
	var out []string
	for _, r := range Rows(doc) {
		if r.Empty {
			out = append(out, r.Path)
		}
	}
	return out
}

// WithoutEmptyRows is a copy of doc with every `{}` row removed from its
// list — the document as the body described it, for the §10.2 leaf diff,
// which would otherwise pair the body's rows with the orphans by index and
// report them as dropped input. doc itself is returned when it has none.
func WithoutEmptyRows(doc map[string]any) map[string]any {
	if len(EmptyRows(doc)) == 0 {
		return doc
	}
	out, _ := stripEmpty(cloneValue(doc)).(map[string]any)
	return out
}

func stripEmpty(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if lexical.IsEditorState(t) {
			return t
		}
		for k, c := range t {
			t[k] = stripEmpty(c)
		}
		return t
	case []any:
		if !isRowList(t) {
			return t
		}
		out := make([]any, 0, len(t))
		for _, el := range t {
			if isEmptyObject(el) {
				continue
			}
			out = append(out, stripEmpty(el))
		}
		return out
	}
	return v
}

// ---------------------------------------------------------------------------
// echo check
// ---------------------------------------------------------------------------

// Finding kinds.
const (
	// FindEmptyRow is a `{}` row where the body sent a real row (or none).
	FindEmptyRow = "empty_row"
	// FindExtraRow is a row the body did not send.
	FindExtraRow = "extra_row"
	// FindNoBlockType is a row that lost the blockType the body gave it.
	FindNoBlockType = "no_block_type"
	// FindTypeSequence is a list whose blockType sequence differs from the
	// body's.
	FindTypeSequence = "type_sequence"
)

// Finding is one orphan symptom.
type Finding struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
	// BlockType is the extra row's type, when it has one.
	BlockType string `json:"block_type,omitempty"`
	// Sent/Stored are the blockType sequences of a type_sequence finding.
	Sent   []string `json:"sent,omitempty"`
	Stored []string `json:"stored,omitempty"`
}

// EchoCheck compares every row list in the request body with the same list
// in a stored document (the echo, or a read-back) and reports orphan
// symptoms: `{}` rows, rows the body did not send, rows that lost their
// blockType, and blockType sequences that differ. A list the server returned
// SHORTER than sent is not reported here — that is a dropped input (§10.2),
// not an orphan — and neither is anything below it.
func EchoCheck(sent, stored map[string]any) []Finding {
	var out []Finding
	echoObject(sent, stored, "", &out)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func echoObject(sent, stored map[string]any, path string, out *[]Finding) {
	for _, k := range sortedKeys(sent) {
		got, ok := stored[k]
		if !ok {
			continue
		}
		p := join(path, k)
		switch s := sent[k].(type) {
		case map[string]any:
			if lexical.IsEditorState(s) {
				continue
			}
			if g, ok := got.(map[string]any); ok {
				echoObject(s, g, p, out)
			}
		case []any:
			g, ok := got.([]any)
			if !ok {
				continue
			}
			echoList(s, g, p, out)
		}
	}
}

func echoList(sent, stored []any, path string, out *[]Finding) {
	if !isRowList(stored) && !isRowList(sent) {
		return
	}
	at := func(i int) string { return path + "[" + strconv.Itoa(i) + "]" }
	// The stored list without its {} rows is what should line up with the
	// body; the {} rows themselves are the first symptom.
	var kept []int
	for i, el := range stored {
		m, ok := el.(map[string]any)
		if !ok {
			continue
		}
		if len(m) == 0 && (i >= len(sent) || !isEmptyObject(sent[i])) {
			*out = append(*out, Finding{Kind: FindEmptyRow, Path: at(i)})
			continue
		}
		kept = append(kept, i)
	}
	var sentRows []map[string]any
	for _, el := range sent {
		if m, ok := el.(map[string]any); ok {
			sentRows = append(sentRows, m)
		}
	}
	if len(kept) < len(sentRows) {
		return // dropped input, reported by the §10.2 echo-diff
	}
	row := func(j int) map[string]any {
		m, _ := stored[kept[j]].(map[string]any)
		return m
	}
	for j := len(sentRows); j < len(kept); j++ {
		bt, _ := row(j)["blockType"].(string)
		*out = append(*out, Finding{Kind: FindExtraRow, Path: at(kept[j]), BlockType: bt})
	}
	var sentTypes, storedTypes []string
	typed, differ := false, false
	for j := range sentRows {
		st, _ := sentRows[j]["blockType"].(string)
		gt, _ := row(j)["blockType"].(string)
		sentTypes = append(sentTypes, st)
		storedTypes = append(storedTypes, gt)
		if st != "" {
			typed = true
		}
		if st != "" && gt == "" {
			*out = append(*out, Finding{Kind: FindNoBlockType, Path: at(kept[j])})
			continue
		}
		if st != gt {
			differ = true
		}
	}
	if typed && differ {
		*out = append(*out, Finding{Kind: FindTypeSequence, Path: path, Sent: sentTypes, Stored: storedTypes})
		return
	}
	for j := range sentRows {
		echoObject(sentRows[j], row(j), at(kept[j]), out)
	}
}

func isEmptyObject(v any) bool {
	m, ok := v.(map[string]any)
	return ok && len(m) == 0
}

// ---------------------------------------------------------------------------
// prediction and remedy
// ---------------------------------------------------------------------------

// MaxPhases caps the carrier phases a remedy may plan. Each phase peels one
// level of nesting; real documents need one or two.
const MaxPhases = 6

// Risk is one block type whose rows in the stored document the write will
// leave behind.
type Risk struct {
	BlockType string `json:"block_type"`
	// Paths are the rows at risk (in the stored document).
	Paths []string `json:"paths"`
	// Causes name the nested rows whose change makes the adapter skip the
	// table ("layout[0].blocks[0]: bento → grid").
	Causes []string `json:"causes,omitempty"`
}

// Carrier is one row appended to a top-level list for one remedy phase.
type Carrier struct {
	// List is the top-level list the carrier is appended to ("layout").
	List string `json:"list"`
	// Chain is the carrier's block types from the top-level row down.
	Chain []string `json:"chain"`
	// Clears is the block type the carrier exists to make the adapter clear.
	Clears string `json:"clears"`
	// Row is the carrier row itself: a copy of real rows with every id
	// removed and every nested blocks list emptied except the one link that
	// leads down the chain.
	Row map[string]any `json:"-"`
}

// Analysis is Analyze's answer.
type Analysis struct {
	// Risks are the block types the write leaves behind, sorted.
	Risks []Risk
	// Empty are `{}` rows already present in the stored document.
	Empty []string
	// Unresolved are `{}` rows whose block type no source could name, so no
	// carrier can clear them.
	Unresolved []string
	// Phases are the carrier sets of the remedy, in order. Empty when the
	// plain write leaves nothing behind.
	Phases [][]Carrier
	// Incomplete is set when MaxPhases was reached with carriers still
	// needing a table cleared.
	Incomplete bool
}

// NeedsReset reports whether a plain write would leave rows behind (or
// already has).
func (a *Analysis) NeedsReset() bool {
	return a != nil && (len(a.Risks) > 0 || len(a.Empty) > 0)
}

// ClearedTypes is every block type the remedy's carriers clear, sorted.
func (a *Analysis) ClearedTypes() []string {
	set := map[string]bool{}
	for _, ph := range a.Phases {
		for _, c := range ph {
			set[c.Clears] = true
		}
	}
	return sortedKeys(set)
}

// Analyze predicts which rows of `stored` — the row the write overwrites, as
// it is stored now — survive writing `next` — the whole document the write
// will store (Payload's merge of the body into the newest version) — and
// plans the carrier phases that remove them. history holds older states of
// the same document (versions); they only serve to name the type of a `{}`
// row and to find rows to copy as carriers.
//
// A block type's table is cleared by the write when some list it was seen in
// is reached by the traversal of `next`: a top-level list always is, a nested
// list only when `next` still has a row of the list's parent kind. Which
// types a list accepts is learnt from every row seen in it (stored, next,
// history), so an accepted-but-never-seen type is treated as not cleared —
// the safe direction: it costs a carrier, never an orphan.
//
// accepts adds what is known about list definitions from elsewhere (PayCLI's
// block census: which block types were observed in each nested blocks field of
// each block type, across every document of the collection). It only ever
// widens the accepted sets, so it can only remove false risks.
func Analyze(stored, next map[string]any, history []map[string]any, accepts Accepts) *Analysis {
	a := &Analysis{}
	storedRows := Rows(stored)
	nextRows := Rows(next)
	var histRows [][]Row
	for _, h := range history {
		histRows = append(histRows, Rows(h))
	}

	k := knowledge{allowed: map[Container]map[string]bool{}, present: map[string]bool{}, occ: map[string]Row{}}
	for c, slugs := range accepts {
		set := k.allowed[c]
		if set == nil {
			set = map[string]bool{}
			k.allowed[c] = set
		}
		for _, s := range slugs {
			set[BlockKind(s)] = true
		}
	}
	k.observe(storedRows, true)
	for _, hr := range histRows {
		k.observe(hr, true)
	}
	k.observe(nextRows, false)
	for _, r := range nextRows {
		if !r.Empty {
			k.present[r.Kind] = true
		}
	}
	nextKind := map[string]string{}
	for _, r := range nextRows {
		nextKind[r.Path] = r.Kind
	}

	risks := map[string]*Risk{}
	need := map[string]bool{}
	for _, r := range storedRows {
		if r.Empty {
			a.Empty = append(a.Empty, r.Path)
			continue
		}
		if r.Depth == 0 || !isBlockKind(r.Kind) || k.cleared(r.Kind) {
			continue
		}
		rk := risks[r.BlockType]
		if rk == nil {
			rk = &Risk{BlockType: r.BlockType}
			risks[r.BlockType] = rk
		}
		rk.Paths = append(rk.Paths, r.Path)
		if c := cause(r, nextKind, k.present); c != "" && !contains(rk.Causes, c) {
			rk.Causes = append(rk.Causes, c)
		}
		need[r.Kind] = true
	}
	for _, slug := range sortedKeys(risks) {
		a.Risks = append(a.Risks, *risks[slug])
	}

	// {} rows: the older states of the document name what used to sit in
	// that list. Types the write clears anyway need nothing.
	for _, r := range storedRows {
		if !r.Empty {
			continue
		}
		cands := map[string]bool{}
		for _, hr := range histRows {
			for _, h := range hr {
				if h.List == r.List && isBlockKind(h.Kind) && h.Depth > 0 {
					cands[h.Kind] = true
				}
			}
		}
		if len(cands) == 0 {
			a.Unresolved = append(a.Unresolved, r.Path)
			continue
		}
		for kind := range cands {
			if !k.cleared(kind) {
				need[kind] = true
			}
		}
	}

	pending := []chain{}
	seen := map[string]bool{}
	for _, kind := range sortedKeys(need) {
		occ, ok := k.occ[kind]
		if !ok || len(occ.Anc) == 0 {
			continue
		}
		c := chain{anc: occ.Anc, clears: SlugOf(kind)}
		if key := c.key(); !seen[key] {
			seen[key] = true
			pending = append(pending, c)
		}
	}
	for len(pending) > 0 {
		pending = dropSubsumed(pending)
		if len(a.Phases) == MaxPhases {
			a.Incomplete = true
			break
		}
		phase := make([]Carrier, 0, len(pending))
		for _, c := range pending {
			phase = append(phase, c.carrier())
		}
		a.Phases = append(a.Phases, phase)
		// The carrier rows below the top level are inserted by this phase;
		// the deepest one the final write would not clear becomes the next
		// phase's target, carried by the chain above it.
		var nextPending []chain
		seen = map[string]bool{}
		for _, c := range pending {
			for i := len(c.anc) - 1; i >= 1; i-- {
				if isBlockKind(c.anc[i].Kind) && !k.cleared(c.anc[i].Kind) {
					nc := chain{anc: c.anc[:i], clears: SlugOf(c.anc[i].Kind)}
					if key := nc.key(); !seen[key] {
						seen[key] = true
						nextPending = append(nextPending, nc)
					}
					break
				}
			}
		}
		pending = nextPending
	}
	return a
}

// dropSubsumed removes every chain whose rows are a strict prefix of another
// chain's: the longer carrier reaches every list the shorter one would (each
// row on the way keeps its lists, emptied), so it clears everything the
// shorter one clears while the shorter one would only add rows.
func dropSubsumed(in []chain) []chain {
	prefix := func(a, b chain) bool {
		if len(a.anc) >= len(b.anc) {
			return false
		}
		for i := range a.anc {
			if a.anc[i].Path != b.anc[i].Path || a.anc[i].Kind != b.anc[i].Kind {
				return false
			}
		}
		return true
	}
	out := make([]chain, 0, len(in))
	for i, c := range in {
		subsumed := false
		for j, o := range in {
			if i != j && prefix(c, o) {
				subsumed = true
				break
			}
		}
		if !subsumed {
			out = append(out, c)
		}
	}
	return out
}

// Accepts maps a list definition to block types known to be accepted in it.
// Use Container{Parent: RootKind, Field: "layout"} for a top-level field and
// Container{Parent: BlockKind("group"), Field: "blocks"} for a nested one.
type Accepts map[Container][]string

// knowledge is what the rows seen so far say about the schema.
type knowledge struct {
	// allowed are the block kinds seen in each list definition.
	allowed map[Container]map[string]bool
	// present are the kinds `next` has rows of.
	present map[string]bool
	// occ is the first nested occurrence of each block kind (stored document
	// first, then history), whose ancestors become carriers.
	occ map[string]Row
}

func (k *knowledge) observe(rows []Row, carriers bool) {
	for _, r := range rows {
		if r.Empty || !isBlockKind(r.Kind) {
			continue
		}
		set := k.allowed[r.Container]
		if set == nil {
			set = map[string]bool{}
			k.allowed[r.Container] = set
		}
		set[r.Kind] = true
		if _, ok := k.occ[r.Kind]; carriers && !ok && r.Depth > 0 {
			k.occ[r.Kind] = r
		}
	}
}

// cleared reports whether the write clears kind's table: some list that
// accepts it is reached by the traversal of the new document.
func (k *knowledge) cleared(kind string) bool {
	for c, set := range k.allowed {
		if !set[kind] {
			continue
		}
		if c.Parent == RootKind || k.present[c.Parent] {
			return true
		}
	}
	return false
}

// cause names the outermost enclosing row whose kind the new document no
// longer has at all.
func cause(r Row, nextKind map[string]string, present map[string]bool) string {
	for _, a := range r.Anc {
		if present[a.Kind] {
			continue
		}
		from := describeKind(a.Kind)
		if to, ok := nextKind[a.Path]; ok && to != "" {
			return fmt.Sprintf("%s: %s → %s", a.Path, from, describeKind(to))
		}
		return fmt.Sprintf("%s: %s removed", a.Path, from)
	}
	return ""
}

func describeKind(kind string) string {
	if s := SlugOf(kind); s != "" {
		return s
	}
	return "array row"
}

// chain is a carrier to build: ancestors from the top-level row down to the
// row whose lists must be reached.
type chain struct {
	anc    []Ancestor
	clears string
}

func (c chain) key() string {
	parts := make([]string, 0, len(c.anc)+1)
	for _, a := range c.anc {
		parts = append(parts, a.Path+"="+a.Kind)
	}
	return strings.Join(parts, "|") + "#" + c.clears
}

// carrier builds the chain's rows bottom-up: the last ancestor with every
// nested blocks list emptied (so the adapter reaches them and clears their
// tables while inserting nothing), each one above it holding only the next.
func (c chain) carrier() Carrier {
	var next map[string]any
	types := make([]string, 0, len(c.anc))
	for i := len(c.anc) - 1; i >= 0; i-- {
		keep := ""
		if i < len(c.anc)-1 {
			keep = c.anc[i].Field
		}
		next = hollow(c.anc[i].Row, keep, next)
	}
	for _, a := range c.anc {
		types = append(types, describeKind(a.Kind))
	}
	return Carrier{List: c.anc[0].List, Chain: types, Clears: c.clears, Row: next}
}

// hollow copies a row for use as a carrier: ids removed at every depth, every
// list holding block rows emptied, except keep, which holds exactly next.
// Arrays without block rows are kept (ids removed): their rows hang off the
// carrier and go with it by cascade.
func hollow(row map[string]any, keep string, next map[string]any) map[string]any {
	out := hollowObject(row, "", keep, next)
	if keep != "" {
		if _, ok := lookup(out, keep); !ok {
			setAt(out, keep, []any{next})
		}
	}
	return out
}

func hollowObject(obj map[string]any, rel, keep string, next map[string]any) map[string]any {
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		if k == "id" && rel == "" {
			continue
		}
		crel := join(rel, k)
		switch t := v.(type) {
		case map[string]any:
			if lexical.IsEditorState(t) {
				out[k] = cloneValue(t)
				continue
			}
			out[k] = hollowObject(t, crel, keep, next)
		case []any:
			switch {
			case crel == keep:
				out[k] = []any{next}
			case len(t) == 0:
				out[k] = []any{}
			case isRowList(t) && hasBlockRows(t):
				out[k] = []any{}
			case isRowList(t):
				out[k] = stripIDs(cloneValue(t))
			default:
				out[k] = cloneValue(t)
			}
		default:
			out[k] = v
		}
	}
	return out
}

// hasBlockRows reports whether any row at any depth below list is a block row.
func hasBlockRows(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		if lexical.IsEditorState(t) {
			return false
		}
		if bt, ok := t["blockType"].(string); ok && bt != "" {
			return true
		}
		for _, c := range t {
			if hasBlockRows(c) {
				return true
			}
		}
	case []any:
		for _, c := range t {
			if hasBlockRows(c) {
				return true
			}
		}
	}
	return false
}

// stripIDs removes the id of every row in v, at every depth.
func stripIDs(v any) any {
	switch t := v.(type) {
	case []any:
		for i, el := range t {
			if m, ok := el.(map[string]any); ok && !isRelationRef(m) {
				delete(m, "id")
			}
			t[i] = stripIDs(el)
		}
	case map[string]any:
		if lexical.IsEditorState(t) {
			return t
		}
		for k, c := range t {
			t[k] = stripIDs(c)
		}
	}
	return v
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// CloneDoc deep-copies a document.
func CloneDoc(doc map[string]any) map[string]any {
	if doc == nil {
		return nil
	}
	out, _ := cloneValue(doc).(map[string]any)
	return out
}

func cloneValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, c := range t {
			out[k] = cloneValue(c)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, c := range t {
			out[i] = cloneValue(c)
		}
		return out
	default:
		return v
	}
}

// Lookup resolves a dotted path of object keys ("layout", "hero.links").
func Lookup(doc map[string]any, path string) (any, bool) { return lookup(doc, path) }

func lookup(doc map[string]any, path string) (any, bool) {
	var cur any = doc
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// SetAt sets a dotted path of object keys, creating objects on the way.
func SetAt(doc map[string]any, path string, v any) { setAt(doc, path, v) }

func setAt(doc map[string]any, path string, v any) {
	parts := strings.Split(path, ".")
	cur := doc
	for _, part := range parts[:len(parts)-1] {
		next, ok := cur[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[part] = next
		}
		cur = next
	}
	cur[parts[len(parts)-1]] = v
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
