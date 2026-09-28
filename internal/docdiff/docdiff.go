// Package docdiff is PayCLI's structural diff of two Payload documents.
//
// It answers "what exactly would change?" — before a write (`--dry-run`),
// between the draft and the published version of a page, between two
// documents, or between two exported files — without the caller piping both
// sides through `jq -S` and `diff`, which reports reformatted JSON and shifted
// array indices instead of the one field that changed.
//
// Three properties separate it from a line diff:
//
//  1. **Rows are matched by `id`.** Payload gives every array and blocks row a
//     server-generated `id`, so a reordered layout is reported as the moves it
//     is, and an edit inside a moved row is reported at the row's NEW path,
//     not as "every row after the insert changed".
//  2. **Rich text is compared by structure but reported as text.** A Lexical
//     editor state is a deep JSON tree; a one-word edit in it is reported once,
//     at the field, with `from_text`/`to_text`, instead of as a leaf path six
//     levels into `root.children`.
//  3. **PATCH semantics are modelled.** [ApplyPatch] computes the document
//     Payload would store after `PATCH` with a body — keys absent from the body
//     keep their stored value, named groups merge key by key, and an array row
//     whose `id` matches a stored row keeps that row's unsent keys — so
//     [DiffPatch] reports what a write would really change, not the difference
//     between a partial body and a whole document.
//
// Nothing here performs I/O, reads the clock or knows about the CLI. Values are
// the generic shapes encoding/json produces (map[string]any, []any, string,
// bool, nil, and json.Number or float64 for numbers); inputs are never mutated.
package docdiff

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// Op is the kind of one change.
type Op string

const (
	// OpAdd is a key or row present only on the right.
	OpAdd Op = "add"
	// OpRemove is a key or row present only on the left.
	OpRemove Op = "remove"
	// OpChange is a value that differs between the two sides.
	OpChange Op = "change"
	// OpMove is a row (matched by id) whose position changed relative to the
	// other matched rows.
	OpMove Op = "move"
)

// Kinds carried in [Change.Kind].
const (
	// KindRichText marks a change to a Lexical editor state. From/To are
	// replaced by FromText/ToText unless [Options.Full] is set.
	KindRichText = "richtext"
	// KindRow marks the addition, removal or move of a whole array row.
	KindRow = "row"
)

// DefaultIgnoreKeys are ignored at every depth unless
// [Options.IncludeTimestamps] is set: Payload rewrites them on every save, so
// a diff that reports them reports nothing but the fact that a save happened.
var DefaultIgnoreKeys = []string{"createdAt", "updatedAt"}

// Change is one difference. Path is written in the §10.2 echo-diff notation
// (`layout[2].blocks[1].title`). Every index in it addresses the RIGHT
// document — the one a write produces, and the one the next `pay blocks`
// edit runs against — except the last index of a row removal, which is the
// row's position on the LEFT (it has no position on the right).
type Change struct {
	Op   Op     `json:"op"`
	Path string `json:"path"`
	// Kind is "" for a plain value, KindRichText or KindRow.
	Kind string `json:"kind,omitempty"`
	// ID and BlockType identify a row for row-level changes, so the caller can
	// build a `pay blocks` selector (`id:<ID>`) without re-reading the doc.
	ID        any    `json:"id,omitempty"`
	BlockType string `json:"block_type,omitempty"`
	// FromIndex and ToIndex are set on a move.
	FromIndex *int `json:"from_index,omitempty"`
	ToIndex   *int `json:"to_index,omitempty"`
	// From and To are the values. Which of them is rendered depends on Op:
	// add renders `to`, remove renders `from`, change renders both (either may
	// be null), move renders neither.
	From any `json:"from"`
	To   any `json:"to"`
	// FromText and ToText are the plain text of a rich-text value.
	FromText *string `json:"from_text,omitempty"`
	ToText   *string `json:"to_text,omitempty"`
	// Detail qualifies a change: "formatting" is a rich-text change whose
	// plain text is identical (bold, a link, a heading level…).
	Detail string `json:"detail,omitempty"`

	// abbreviated records that From/To were dropped in favour of the text
	// fields, so MarshalJSON omits them instead of printing two nulls.
	abbreviated bool
}

// MarshalJSON renders only the fields that mean something for the op, in a
// fixed order. A plain struct tag cannot express "from is present and null"
// (a change from null to a value) versus "from is absent" (an add).
func (c Change) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	first := true
	put := func(key string, v any) error {
		enc, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		kb, _ := json.Marshal(key)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(enc)
		return nil
	}
	steps := []struct {
		key  string
		v    any
		emit bool
	}{
		{"op", c.Op, true},
		{"path", c.Path, true},
		{"kind", c.Kind, c.Kind != ""},
		{"id", c.ID, c.ID != nil},
		{"block_type", c.BlockType, c.BlockType != ""},
		{"from_index", c.FromIndex, c.FromIndex != nil},
		{"to_index", c.ToIndex, c.ToIndex != nil},
		{"from", c.From, !c.abbreviated && (c.Op == OpRemove || c.Op == OpChange)},
		{"to", c.To, !c.abbreviated && (c.Op == OpAdd || c.Op == OpChange)},
		{"from_text", c.FromText, c.FromText != nil},
		{"to_text", c.ToText, c.ToText != nil},
		{"detail", c.Detail, c.Detail != ""},
	}
	for _, s := range steps {
		if !s.emit {
			continue
		}
		if err := put(s.key, s.v); err != nil {
			return nil, err
		}
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

// Summary counts the changes.
type Summary struct {
	Total   int `json:"total"`
	Added   int `json:"added"`
	Removed int `json:"removed"`
	Changed int `json:"changed"`
	Moved   int `json:"moved"`
	// RichText counts the changes whose Kind is KindRichText (they are also
	// counted under their op).
	RichText int `json:"rich_text"`
	// Fields are the top-level document fields at least one change touches,
	// sorted — the keys a minimal PATCH would have to carry.
	Fields []string `json:"fields"`
}

// Result is one diff.
type Result struct {
	Identical bool     `json:"identical"`
	Summary   Summary  `json:"summary"`
	Changes   []Change `json:"changes"`
}

// Options tunes a diff. The zero value is the documented default.
type Options struct {
	// IncludeTimestamps stops ignoring DefaultIgnoreKeys.
	IncludeTimestamps bool
	// IgnoreKeys are additional object keys ignored at every depth.
	IgnoreKeys []string
	// IgnorePaths are path patterns whose value — and everything below it —
	// is ignored. A pattern is a path in change notation; `[]` or `[*]`
	// matches any index: "meta.image", "layout[].blockName", "layout[3]".
	IgnorePaths []string
	// LeafPaths are path patterns (same grammar) whose value is compared and
	// replaced as one unit rather than descended into — a `json` field, whose
	// object value Payload stores wholesale. A caller with a schema passes
	// them; without one, every non-rich-text object is treated as a group.
	LeafPaths []string
	// NullDistinct reports null versus an absent key as a change. By default
	// the two are equal: Payload returns null for an unset field, and a
	// hand-written file or a PATCH body usually omits it.
	NullDistinct bool
	// Full keeps every value lossless: rich-text changes carry the editor
	// states in from/to, and editor states nested inside added or removed
	// values are not abbreviated.
	Full bool
}

// Diff compares two documents (or any two JSON values) and returns every
// difference in a deterministic order: object keys sorted, and within an
// array the removed rows first (left order), then the right rows in order.
func Diff(left, right any, opts Options) Result {
	d := newDiffer(opts)
	d.value("", left, right, true, true)
	return d.result()
}

type differ struct {
	opts    Options
	ignore  map[string]bool
	ignored []pattern
	leaves  []pattern
	changes []Change
}

func newDiffer(opts Options) *differ {
	d := &differ{opts: opts, ignore: map[string]bool{}}
	if !opts.IncludeTimestamps {
		for _, k := range DefaultIgnoreKeys {
			d.ignore[k] = true
		}
	}
	for _, k := range opts.IgnoreKeys {
		d.ignore[k] = true
	}
	d.ignored = compilePatterns(opts.IgnorePaths)
	d.leaves = compilePatterns(opts.LeafPaths)
	return d
}

func (d *differ) result() Result {
	res := Result{Changes: d.changes}
	if res.Changes == nil {
		res.Changes = []Change{}
	}
	fields := map[string]bool{}
	for _, c := range res.Changes {
		res.Summary.Total++
		switch c.Op {
		case OpAdd:
			res.Summary.Added++
		case OpRemove:
			res.Summary.Removed++
		case OpChange:
			res.Summary.Changed++
		case OpMove:
			res.Summary.Moved++
		}
		if c.Kind == KindRichText {
			res.Summary.RichText++
		}
		if root := RootField(c.Path); root != "" {
			fields[root] = true
		}
	}
	res.Summary.Fields = make([]string, 0, len(fields))
	for f := range fields {
		res.Summary.Fields = append(res.Summary.Fields, f)
	}
	sort.Strings(res.Summary.Fields)
	res.Identical = res.Summary.Total == 0
	return res
}

// value compares one pair. hasL/hasR are key presence: an absent key and a
// null value are the same unless Options.NullDistinct.
func (d *differ) value(path string, l, r any, hasL, hasR bool) {
	if path != "" && matchAny(d.ignored, path) {
		return
	}
	// Null and absent are the same VALUE unless NullDistinct, but the op
	// still follows key presence: a key that exists on both sides and goes
	// from null to a value is a change, not an add.
	if !d.opts.NullDistinct && l == nil && r == nil {
		return
	}
	switch {
	case !hasL && !hasR:
		return
	case hasL && !hasR:
		d.emitRemove(path, l)
		return
	case !hasL && hasR:
		d.emitAdd(path, r)
		return
	}

	// Rich text first: an editor state is an object, and descending into it
	// would report `root.children[0].children[2].text` for a one-word edit.
	if isEditorState(l) || isEditorState(r) {
		d.richText(path, l, r)
		return
	}
	if path != "" && matchAny(d.leaves, path) {
		if !Equal(l, r) {
			d.emitChange(path, l, r)
		}
		return
	}

	switch lv := l.(type) {
	case map[string]any:
		rv, ok := r.(map[string]any)
		if !ok {
			d.emitChange(path, l, r)
			return
		}
		d.object(path, lv, rv, false)
	case []any:
		rv, ok := r.([]any)
		if !ok {
			d.emitChange(path, l, r)
			return
		}
		d.array(path, lv, rv)
	default:
		if !Equal(l, r) {
			d.emitChange(path, l, r)
		}
	}
}

// object compares two objects key by key, in sorted key order. rowIDsIgnored
// drops the `id` key of an array row whose identity was already decided by
// position (index mode), where two independently generated ids are noise.
func (d *differ) object(path string, l, r map[string]any, rowIDsIgnored bool) {
	keys := make([]string, 0, len(l)+len(r))
	seen := map[string]bool{}
	for k := range l {
		keys = append(keys, k)
		seen[k] = true
	}
	for k := range r {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if d.ignore[k] || (rowIDsIgnored && k == "id") {
			continue
		}
		lv, hasL := l[k]
		rv, hasR := r[k]
		d.value(JoinKey(path, k), lv, rv, hasL, hasR)
	}
}

// array compares two arrays. Arrays of scalars (a hasMany relationship, a
// multi-select) are one value; arrays of rows are matched by id when both
// sides share at least one row id, by position otherwise.
func (d *differ) array(path string, l, r []any) {
	if allScalar(l) && allScalar(r) {
		if !Equal(l, r) {
			d.emitChange(path, l, r)
		}
		return
	}
	if lIdx, rIDs, ok := idIndex(l, r); ok {
		d.arrayByID(path, l, r, lIdx, rIDs)
		return
	}
	n := len(l)
	if len(r) > n {
		n = len(r)
	}
	for i := 0; i < n; i++ {
		p := JoinIndex(path, i)
		switch {
		case i >= len(r):
			d.emitRowRemove(p, l[i])
		case i >= len(l):
			d.emitRowAdd(p, r[i])
		default:
			lo, lok := l[i].(map[string]any)
			ro, rok := r[i].(map[string]any)
			if lok && rok && !isEditorState(lo) && !isEditorState(ro) {
				if matchAny(d.ignored, p) || d.rowTypeChanged(p, lo, ro) {
					continue
				}
				d.object(p, lo, ro, true)
				continue
			}
			d.value(p, l[i], r[i], true, true)
		}
	}
}

// arrayByID matches rows by id. Rows present on both sides are compared at
// their right-hand path; the ones whose relative order changed are reported
// as moves (everything outside the longest run of rows that kept their
// order), so inserting one row reports one add, not a move of every row
// below it.
func (d *differ) arrayByID(path string, l, r []any, lIdx map[string]int, rIDs []string) {
	matchedLeft := make([]bool, len(l))
	type pair struct{ li, ri int }
	var pairs []pair
	for ri, id := range rIDs {
		if id == "" {
			continue
		}
		if li, ok := lIdx[id]; ok {
			matchedLeft[li] = true
			pairs = append(pairs, pair{li, ri})
		}
	}

	// Removals address the left document, so they are reported first and in
	// left order.
	for li, row := range l {
		if !matchedLeft[li] {
			d.emitRowRemove(JoinIndex(path, li), row)
		}
	}

	seq := make([]int, len(pairs))
	for i, p := range pairs {
		seq[i] = p.li
	}
	stable := longestIncreasing(seq)
	moved := map[int]bool{}
	for i, p := range pairs {
		if !stable[i] {
			moved[p.ri] = true
		}
	}
	byRight := map[int]int{}
	for _, p := range pairs {
		byRight[p.ri] = p.li
	}

	for ri, row := range r {
		p := JoinIndex(path, ri)
		li, matched := byRight[ri]
		if !matched {
			d.emitRowAdd(p, row)
			continue
		}
		if matchAny(d.ignored, p) {
			continue
		}
		if moved[ri] {
			from, to := li, ri
			c := Change{Op: OpMove, Path: p, Kind: KindRow, FromIndex: &from, ToIndex: &to}
			c.ID, c.BlockType = rowIdentity(row)
			d.changes = append(d.changes, c)
		}
		// idIndex only indexes object rows, so both assertions hold.
		lo, _ := l[li].(map[string]any)
		ro, _ := row.(map[string]any)
		if d.rowTypeChanged(p, lo, ro) {
			continue
		}
		d.object(p, lo, ro, false)
	}
}

// richText compares two values at least one of which is an editor state.
func (d *differ) richText(path string, l, r any) {
	if Equal(l, r) {
		return
	}
	c := Change{Op: OpChange, Path: path, Kind: KindRichText}
	lState, rState := isEditorState(l), isEditorState(r)
	// A null side of a rich-text field is an empty editor: its text is "".
	lText, rText := lState || l == nil, rState || r == nil
	if lText {
		t := plainText(l)
		c.FromText = &t
	}
	if rText {
		t := plainText(r)
		c.ToText = &t
	}
	if lState && rState && *c.FromText == *c.ToText {
		c.Detail = "formatting"
	}
	switch {
	case d.opts.Full:
		c.From, c.To = d.strip(l), d.strip(r)
	case lText && rText:
		c.abbreviated = true
	default:
		// One side is a plain value (a string where rich text used to be).
		// It is small and it is the most useful thing to show, so only the
		// editor side is abbreviated.
		c.From, c.To = d.present(l), d.present(r)
	}
	d.changes = append(d.changes, c)
}

// rowTypeChanged reports two rows whose blockType differs. They are reported
// as one whole-row change: comparing a hero's keys with a cta's keys produces
// a dozen add/remove lines that all say "this is a different block".
func (d *differ) rowTypeChanged(path string, l, r map[string]any) bool {
	lt, lok := l["blockType"].(string)
	rt, rok := r["blockType"].(string)
	if !lok || !rok || lt == rt {
		return false
	}
	c := Change{Op: OpChange, Path: path, Kind: KindRow, From: d.present(l), To: d.present(r)}
	c.ID, c.BlockType = rowIdentity(r)
	d.changes = append(d.changes, c)
	return true
}

func (d *differ) emitAdd(path string, v any) {
	c := Change{Op: OpAdd, Path: path, To: d.present(v)}
	if isEditorState(v) {
		c.Kind = KindRichText
		t := plainText(v)
		c.ToText = &t
		if !d.opts.Full {
			c.abbreviated = true
		}
	}
	d.changes = append(d.changes, c)
}

func (d *differ) emitRemove(path string, v any) {
	c := Change{Op: OpRemove, Path: path, From: d.present(v)}
	if isEditorState(v) {
		c.Kind = KindRichText
		t := plainText(v)
		c.FromText = &t
		if !d.opts.Full {
			c.abbreviated = true
		}
	}
	d.changes = append(d.changes, c)
}

func (d *differ) emitRowAdd(path string, row any) {
	c := Change{Op: OpAdd, Path: path, To: d.present(row)}
	if _, ok := row.(map[string]any); ok && !isEditorState(row) {
		c.Kind = KindRow
		c.ID, c.BlockType = rowIdentity(row)
	}
	d.changes = append(d.changes, c)
}

func (d *differ) emitRowRemove(path string, row any) {
	c := Change{Op: OpRemove, Path: path, From: d.present(row)}
	if _, ok := row.(map[string]any); ok && !isEditorState(row) {
		c.Kind = KindRow
		c.ID, c.BlockType = rowIdentity(row)
	}
	d.changes = append(d.changes, c)
}

func (d *differ) emitChange(path string, l, r any) {
	d.changes = append(d.changes, Change{Op: OpChange, Path: path, From: d.present(l), To: d.present(r)})
}

// present prepares a value for a change record: a deep copy (the caller may
// mutate its documents afterwards) with ignored keys dropped and, unless
// Options.Full, every nested editor state abbreviated to its text.
func (d *differ) present(v any) any {
	if d.opts.Full {
		return d.strip(v)
	}
	return abbreviateValue(d.strip(v))
}

// strip drops ignored keys (timestamps) from a value about to be reported, so
// an added row does not carry a createdAt the diff otherwise hides.
func (d *differ) strip(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if isEditorState(t) {
			return clone(t)
		}
		out := make(map[string]any, len(t))
		for k, val := range t {
			if d.ignore[k] {
				continue
			}
			out[k] = d.strip(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = d.strip(val)
		}
		return out
	default:
		return v
	}
}

// RichTextMarker is the key of the object that stands in for an abbreviated
// editor state inside a reported value: {"$richtext": "plain text"}.
const RichTextMarker = "$richtext"

// abbreviateValue replaces every editor state inside v with a
// {"$richtext": "<plain text>"} marker. The marker is an object with a key no
// Payload field can have, so it cannot be mistaken for a real string value.
func abbreviateValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if isEditorState(t) {
			return map[string]any{RichTextMarker: plainText(t)}
		}
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = abbreviateValue(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = abbreviateValue(val)
		}
		return out
	default:
		return v
	}
}

// idIndex decides whether an array pair is matched by id: every object row
// that carries an id must carry a unique one, and at least one id must be
// shared between the sides. It returns the left index by id and the right
// row ids ("" for a row without one).
func idIndex(l, r []any) (map[string]int, []string, bool) {
	lIdx := map[string]int{}
	for i, row := range l {
		id, ok := rowID(row)
		if !ok {
			continue
		}
		if _, dup := lIdx[id]; dup {
			return nil, nil, false
		}
		lIdx[id] = i
	}
	if len(lIdx) == 0 {
		return nil, nil, false
	}
	rIDs := make([]string, len(r))
	seen := map[string]bool{}
	shared := false
	for i, row := range r {
		id, ok := rowID(row)
		if !ok {
			continue
		}
		if seen[id] {
			return nil, nil, false
		}
		seen[id] = true
		rIDs[i] = id
		if _, ok := lIdx[id]; ok {
			shared = true
		}
	}
	if !shared {
		return nil, nil, false
	}
	return lIdx, rIDs, true
}

// rowID is a row's id as a comparable string. Payload row ids are strings
// (ObjectId-shaped hex on every adapter); numbers are accepted for symmetry.
func rowID(row any) (string, bool) {
	obj, ok := row.(map[string]any)
	if !ok || isEditorState(obj) {
		return "", false
	}
	switch id := obj["id"].(type) {
	case string:
		if id == "" {
			return "", false
		}
		return "s:" + id, true
	case json.Number:
		return "n:" + id.String(), true
	case float64:
		return "n:" + fmt.Sprint(id), true
	}
	return "", false
}

func rowIdentity(row any) (id any, blockType string) {
	obj, ok := row.(map[string]any)
	if !ok {
		return nil, ""
	}
	bt, _ := obj["blockType"].(string)
	return obj["id"], bt
}

// longestIncreasing marks the elements of seq that belong to one longest
// strictly increasing subsequence (O(n log n)). Rows on it kept their relative
// order; every other matched row moved.
func longestIncreasing(seq []int) []bool {
	n := len(seq)
	out := make([]bool, n)
	if n == 0 {
		return out
	}
	tails := []int{}       // index into seq of the smallest tail per length
	prev := make([]int, n) // predecessor index for reconstruction
	for i, v := range seq {
		lo, hi := 0, len(tails)
		for lo < hi {
			mid := (lo + hi) / 2
			if seq[tails[mid]] < v {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		if lo > 0 {
			prev[i] = tails[lo-1]
		} else {
			prev[i] = -1
		}
		if lo == len(tails) {
			tails = append(tails, i)
		} else {
			tails[lo] = i
		}
	}
	for k := tails[len(tails)-1]; k >= 0; k = prev[k] {
		out[k] = true
	}
	return out
}

func allScalar(list []any) bool {
	for _, v := range list {
		switch v.(type) {
		case map[string]any, []any:
			return false
		}
	}
	return true
}

// Equal is JSON equality: numbers compare by value whatever their Go type
// (json.Number from a UseNumber decoder, float64 from a plain one), objects by
// key set and values, arrays element-wise.
func Equal(a, b any) bool {
	switch x := a.(type) {
	case nil:
		return b == nil
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	case json.Number, float64, float32, int, int64:
		return numbersEqual(a, b)
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !Equal(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !Equal(x[i], y[i]) {
				return false
			}
		}
		return true
	}
	// Any other Go type (a typed struct a caller passed in) falls back to its
	// JSON encoding, which is what Payload would see.
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ab) == string(bb)
}

func numbersEqual(a, b any) bool {
	x, ok1 := toRat(a)
	y, ok2 := toRat(b)
	return ok1 && ok2 && x.Cmp(y) == 0
}

func toRat(v any) (*big.Rat, bool) {
	r := new(big.Rat)
	switch n := v.(type) {
	case json.Number:
		if _, ok := r.SetString(n.String()); ok {
			return r, true
		}
	case float64:
		if r.SetFloat64(n) != nil {
			return r, true
		}
	case float32:
		if r.SetFloat64(float64(n)) != nil {
			return r, true
		}
	case int:
		return r.SetInt64(int64(n)), true
	case int64:
		return r.SetInt64(n), true
	}
	return nil, false
}

// clone deep-copies a generic JSON value.
func clone(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = clone(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = clone(val)
		}
		return out
	default:
		return v
	}
}
