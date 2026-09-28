// Package rows is PayCLI's local editor for an array of objects: the rows of a
// Payload `blocks` field, or of a plain `array` field.
//
// It exists because the one edit a content agent makes most often — "move the
// CTA above the media block", "drop the third block" — has no API of its own.
// Payload's REST surface can only replace the whole array, so the edit is a
// read-modify-write round trip that every caller re-invents with jq and a
// temp file, and gets wrong in the same two ways: the anchor index shifts once
// the moved row is lifted out, and a row is addressed by a position the caller
// read from a stale listing.
//
// Nothing here performs I/O, reads the clock or consults a schema. A [Selector]
// is parsed from a string and matched against a []any; a [Path] (path.go)
// reaches an array at any depth of a decoded document — through a group's
// rows, a grid inside the group — using only the document's own shape; every
// mutation returns a NEW value and never aliases the input. That makes each
// rule below a table test, and it keeps the index arithmetic — the part that is
// actually hard — in one place instead of in six command files.
package rows

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Payload's own keys on a blocks row. They are protocol, not content:
// `blockType` decides which block the row IS, `id` is server-generated, and
// `blockName` is the admin-UI label. They are named here because the selector
// grammar addresses rows BY them.
const (
	KeyID        = "id"
	KeyBlockType = "blockType"
	KeyBlockName = "blockName"
)

// ---------------------------------------------------------------------------
// selectors
// ---------------------------------------------------------------------------

// Kind is the form a [Selector] took. The grammar is closed — five forms, no
// escapes, no wildcards — for the same reason `--path` is not jq: an open
// grammar guarantees a caller sends something plausible that this package does
// not implement, and gets an unspecified failure instead of a named one.
type Kind string

const (
	// KindIndex is a bare integer: `0`, `3`, `-1`. Negative counts from the
	// end, so `-1` is the last row.
	KindIndex Kind = "index"
	// KindID is `id:<value>` — the row's server-generated `id`.
	KindID Kind = "id"
	// KindName is `name:<value>` — an exact `blockName` match.
	KindName Kind = "name"
	// KindType is `type:<slug>` — every row with that `blockType`, or the
	// n-th one with `type:<slug>[n]`.
	KindType Kind = "type"
)

// Selector addresses one or more rows. The zero value is unusable; build one
// with [ParseSelector].
type Selector struct {
	// Raw is the string the caller wrote, kept verbatim for error messages.
	Raw string
	// Kind is which of the five forms was used.
	Kind Kind
	// Index is set for KindIndex and may be negative.
	Index int
	// Value is the id, blockName or blockType being matched.
	Value string
	// Nth is the `[n]` subscript of `type:cta[1]`, nil when absent. It is
	// separate from Index so that "the second cta" and "row 2" cannot be
	// confused by a reader of this struct.
	Nth *int
}

// SelectorGrammar is the one-paragraph description of the grammar, shared by
// every command's help so the five forms are documented identically in all of
// them.
const SelectorGrammar = `N | -N | first | last | id:VALUE | name:VALUE | type:SLUG | type:SLUG[N]`

// SelectorForms is the per-form explanation for help output.
var SelectorForms = []string{
	"3           the row at index 3 (0-based)",
	"-1          the last row; -2 the second to last",
	"first,last  sugar for 0 and -1",
	"id:67f3a1   the row whose `id` is 67f3a1 — the only stable handle across edits",
	"name:Hero   the row whose `blockName` is exactly Hero (never a substring)",
	"type:cta    EVERY row whose `blockType` is cta",
	"type:cta[1] the second cta row (0-based), which is always exactly one row",
}

// ParseSelector parses one selector. It never consults the rows, so a selector
// can be validated before a document has been read.
func ParseSelector(s string) (Selector, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Selector{}, fmt.Errorf("a selector is required (%s)", SelectorGrammar)
	}
	sel := Selector{Raw: raw}

	switch strings.ToLower(raw) {
	case "first":
		sel.Kind, sel.Index = KindIndex, 0
		return sel, nil
	case "last":
		sel.Kind, sel.Index = KindIndex, -1
		return sel, nil
	}

	prefix, rest, hasPrefix := strings.Cut(raw, ":")
	if !hasPrefix {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return Selector{}, fmt.Errorf(
				"%q is not a selector: an unprefixed selector must be an integer index", raw)
		}
		sel.Kind, sel.Index = KindIndex, n
		return sel, nil
	}

	switch strings.ToLower(prefix) {
	case "id":
		if rest == "" {
			return Selector{}, fmt.Errorf("%q has an empty id", raw)
		}
		sel.Kind, sel.Value = KindID, rest
	case "name":
		if rest == "" {
			return Selector{}, fmt.Errorf("%q has an empty blockName", raw)
		}
		sel.Kind, sel.Value = KindName, rest
	case "type":
		value, nth, err := parseSubscript(raw, rest)
		if err != nil {
			return Selector{}, err
		}
		if value == "" {
			return Selector{}, fmt.Errorf("%q has an empty blockType", raw)
		}
		sel.Kind, sel.Value, sel.Nth = KindType, value, nth
	default:
		return Selector{}, fmt.Errorf(
			"%q uses an unknown selector prefix %q; the three prefixes are id:, name: and type:, in that spelling", raw, prefix)
	}
	return sel, nil
}

// parseSubscript splits `cta[1]` into ("cta", 1). A missing subscript is not an
// error; a malformed one is, because silently treating `cta[1` as a blockType
// would match nothing and read as "that block does not exist".
func parseSubscript(raw, s string) (string, *int, error) {
	open := strings.IndexByte(s, '[')
	if open < 0 {
		if strings.ContainsAny(s, "]") {
			return "", nil, fmt.Errorf("%q has a closing ] with no opening [ before it", raw)
		}
		return s, nil, nil
	}
	if !strings.HasSuffix(s, "]") {
		return "", nil, fmt.Errorf("%q has an opening [ with no closing ]", raw)
	}
	inner := s[open+1 : len(s)-1]
	n, err := strconv.Atoi(inner)
	if err != nil {
		return "", nil, fmt.Errorf("%q: the subscript %q is not an integer", raw, inner)
	}
	if n < 0 {
		return "", nil, fmt.Errorf("%q: a type subscript counts forwards and cannot be negative", raw)
	}
	return s[:open], &n, nil
}

// Match returns the indices sel addresses, ascending. An empty result is not an
// error here: the caller decides whether "no match" is fatal, because `rm` on
// an already-absent row and `mv` of a row that must exist want opposite
// answers.
func (s Selector) Match(list []any) []int {
	switch s.Kind {
	case KindIndex:
		i := s.Index
		if i < 0 {
			i += len(list)
		}
		if i < 0 || i >= len(list) {
			return nil
		}
		return []int{i}
	case KindID:
		return matchKey(list, KeyID, s.Value)
	case KindName:
		return matchKey(list, KeyBlockName, s.Value)
	case KindType:
		hits := matchKey(list, KeyBlockType, s.Value)
		if s.Nth == nil {
			return hits
		}
		if *s.Nth >= len(hits) {
			return nil
		}
		return []int{hits[*s.Nth]}
	}
	return nil
}

// matchKey collects the rows whose key equals want. Comparison is on the
// rendered scalar rather than the Go value, because an id is a string on a
// Mongo project and a json.Number on a Postgres one and `id:42` has to find
// both — this is the same tri-state id problem §7.8 handles for documents.
func matchKey(list []any, key, want string) []int {
	var out []int
	for i, row := range list {
		obj, ok := row.(map[string]any)
		if !ok {
			continue
		}
		if scalarString(obj[key]) == want {
			out = append(out, i)
		}
	}
	return out
}

// scalarString renders a JSON scalar the way a caller would have typed it.
// Anything that is not a scalar returns "", which never equals a selector
// value, so an object-valued `id` simply does not match.
func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case bool:
		return strconv.FormatBool(t)
	}
	return ""
}

// ---------------------------------------------------------------------------
// positions
// ---------------------------------------------------------------------------

// Mode is how an [Anchor] names a destination.
type Mode string

const (
	// ModeAppend puts the row after every existing row. It is the default for
	// an insert, and the only mode that needs no argument.
	ModeAppend Mode = "append"
	// ModeIndex is `--at N` / `--to N`, a literal destination index.
	ModeIndex Mode = "index"
	// ModeBefore is `--before SEL`: end up immediately above that row.
	ModeBefore Mode = "before"
	// ModeAfter is `--after SEL`: end up immediately below that row.
	ModeAfter Mode = "after"
)

// Anchor is a destination. Before/After are expressed relative to another ROW
// rather than to an index on purpose: an index read out of a listing is stale
// the moment anything else in the pipeline edits the array, while "after the
// media block" stays true.
type Anchor struct {
	Mode  Mode
	Index int
	Sel   Selector
	// Label is how the caller SPELLED the destination, when that is clearer
	// than the mode it compiled to. `--last` compiles to index -1, and an op
	// log reading "(index -1)" makes a reader work out what the caller wrote;
	// "(last)" does not. Empty means "describe the mode".
	Label string
}

// Describe renders the anchor for an error message or an op log. It returns ""
// when the destination is already stated by the sentence around it — an index
// the caller can read off the op's own text.
func (a Anchor) Describe() string {
	if a.Label != "" {
		return a.Label
	}
	switch a.Mode {
	case ModeBefore:
		return "before " + a.Sel.Raw
	case ModeAfter:
		return "after " + a.Sel.Raw
	}
	return ""
}

// ---------------------------------------------------------------------------
// errors
// ---------------------------------------------------------------------------

// NoMatchError is returned when a selector that had to address a row addressed
// none. It carries the rows' summary so the caller can print what DOES exist
// instead of only what does not.
type NoMatchError struct {
	Sel   Selector
	Field string
	Have  []Summary
}

func (e *NoMatchError) Error() string {
	where := "the array"
	if e.Field != "" {
		where = e.Field
	}
	return fmt.Sprintf("%s has no row matching %q", where, e.Sel.Raw)
}

// AmbiguousError is returned when a selector addressed several rows but the
// operation acts on exactly one.
type AmbiguousError struct {
	Sel     Selector
	Field   string
	Matched []int
	Have    []Summary
}

func (e *AmbiguousError) Error() string {
	where := "the array"
	if e.Field != "" {
		where = e.Field
	}
	return fmt.Sprintf("%q matches %d rows in %s (indices %s), but this operation acts on exactly one",
		e.Sel.Raw, len(e.Matched), where, joinInts(e.Matched))
}

func joinInts(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.Itoa(x)
	}
	return strings.Join(parts, ", ")
}

// ResolveOne matches sel and insists on exactly one row.
func ResolveOne(list []any, sel Selector, field string) (int, error) {
	hits := sel.Match(list)
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return 0, &NoMatchError{Sel: sel, Field: field, Have: Summarize(list)}
	default:
		return 0, &AmbiguousError{Sel: sel, Field: field, Matched: hits, Have: Summarize(list)}
	}
}

// ResolveMany matches sel and insists on at least one row. Several is fine.
func ResolveMany(list []any, sel Selector, field string) ([]int, error) {
	hits := sel.Match(list)
	if len(hits) == 0 {
		return nil, &NoMatchError{Sel: sel, Field: field, Have: Summarize(list)}
	}
	return hits, nil
}

// ---------------------------------------------------------------------------
// mutations
// ---------------------------------------------------------------------------

// clone copies the slice header's contents. Every mutation below goes through
// it so that an input array the caller still holds — the document it read off
// the pipe — is never modified underneath them.
func clone(list []any) []any {
	out := make([]any, len(list))
	copy(out, list)
	return out
}

// Remove deletes the rows at idx and returns the new array. Indices may arrive
// in any order and may repeat.
func Remove(list []any, idx []int) []any {
	drop := make(map[int]bool, len(idx))
	for _, i := range idx {
		drop[i] = true
	}
	out := make([]any, 0, len(list))
	for i, row := range list {
		if !drop[i] {
			out = append(out, row)
		}
	}
	return out
}

// Insert puts row at the destination a names and returns the new array plus the
// index it landed at.
//
// The destination space has len+1 slots (0 .. len), not len: appending to a
// 3-row array is index 3. A negative `--at` counts back through those slots, so
// `--at -1` is the last position — consistent with a negative selector meaning
// the last row.
func Insert(list []any, row any, a Anchor) ([]any, int, error) {
	dest, err := insertPoint(list, a)
	if err != nil {
		return nil, 0, err
	}
	out := make([]any, 0, len(list)+1)
	out = append(out, list[:dest]...)
	out = append(out, row)
	out = append(out, list[dest:]...)
	return out, dest, nil
}

// insertPoint resolves an anchor against list, for an insert into list.
func insertPoint(list []any, a Anchor) (int, error) {
	switch a.Mode {
	case ModeAppend, "":
		return len(list), nil
	case ModeIndex:
		i := a.Index
		if i < 0 {
			i += len(list) + 1
		}
		if i < 0 || i > len(list) {
			return 0, fmt.Errorf(
				"index %d is outside 0..%d — an array of %d rows has %d insert positions",
				a.Index, len(list), len(list), len(list)+1)
		}
		return i, nil
	case ModeBefore:
		at, err := ResolveOne(list, a.Sel, "")
		if err != nil {
			return 0, err
		}
		return at, nil
	case ModeAfter:
		at, err := ResolveOne(list, a.Sel, "")
		if err != nil {
			return 0, err
		}
		return at + 1, nil
	}
	return 0, fmt.Errorf("unknown destination mode %q", a.Mode)
}

// Move relocates the row at src to the destination a names and returns the new
// array plus the index the row ended at.
//
// The anchor is resolved against the array WITH the moved row still in it, and
// the destination is then recomputed against the array WITHOUT it. That is the
// whole reason this function exists: "move row 0 after row 3" naively becomes
// insert-at-4 on a 3-row remainder and lands the row past its anchor. Every
// hand-written version of this loop gets it wrong once.
func Move(list []any, src int, a Anchor) ([]any, int, error) {
	if src < 0 || src >= len(list) {
		return nil, 0, fmt.Errorf("index %d is outside 0..%d", src, len(list)-1)
	}

	// Resolve the anchor first, while the indices still mean what the caller
	// saw in `pay blocks ls`.
	var anchor int
	switch a.Mode {
	case ModeBefore, ModeAfter:
		at, err := ResolveOne(list, a.Sel, "")
		if err != nil {
			return nil, 0, err
		}
		if at == src {
			return nil, 0, fmt.Errorf(
				"%q selects the row being moved, so the move has no destination", a.Sel.Raw)
		}
		anchor = at
	}

	rest := Remove(list, []int{src})

	var dest int
	switch a.Mode {
	case ModeBefore:
		dest = shiftForRemoval(anchor, src)
	case ModeAfter:
		dest = shiftForRemoval(anchor, src) + 1
	default:
		d, err := insertPoint(rest, a)
		if err != nil {
			return nil, 0, err
		}
		dest = d
	}

	out := make([]any, 0, len(list))
	out = append(out, rest[:dest]...)
	out = append(out, list[src])
	out = append(out, rest[dest:]...)
	return out, dest, nil
}

// shiftForRemoval maps an index in the original array to its index after the
// row at removed was taken out.
func shiftForRemoval(i, removed int) int {
	if i > removed {
		return i - 1
	}
	return i
}

// Copy duplicates the row at src into the destination a names, stripping every
// server-generated `id` on the way — at the top level and at every depth.
//
// The strip is not optional and not a flag. Payload treats a row whose `id`
// matches an existing row as THAT row: a duplicate that kept its ids does not
// add a block, it silently rewrites the original and drops one of the two.
func Copy(list []any, src int, a Anchor) ([]any, int, error) {
	if src < 0 || src >= len(list) {
		return nil, 0, fmt.Errorf("index %d is outside 0..%d", src, len(list)-1)
	}
	dup := StripIDs(deepCopy(list[src]))
	return Insert(list, dup, a)
}

// Set applies a field patch to the row at idx and returns the new array. Keys
// are paths into the row in the [ParsePath] grammar — `blockName`,
// `link.url`, `links.0.label`, `links[id:6ab8…].label`. A value in patch is
// written as given (nil writes null); every key in unset is cleared
// ([ClearPath]), never deleted.
//
// A path that passes through an array addresses one of its rows and never
// replaces the array: before this rule `--set links.0.label=Buy` turned the
// `links` array into an object with a key "0", which Payload then dropped.
func Set(list []any, idx int, patch map[string]any, unset []string) ([]any, error) {
	if idx < 0 || idx >= len(list) {
		return nil, fmt.Errorf("index %d is outside 0..%d", idx, len(list)-1)
	}
	row, ok := deepCopy(list[idx]).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("row %d is not an object, so it has no fields to set", idx)
	}
	for _, key := range sortedKeys(patch) {
		if err := SetPath(row, key, patch[key]); err != nil {
			return nil, err
		}
	}
	for _, key := range unset {
		if err := ClearPath(row, key); err != nil {
			return nil, err
		}
	}
	out := clone(list)
	out[idx] = row
	return out, nil
}

// sortedKeys keeps a multi-key patch deterministic: two `--set` flags that
// touch the same subtree must compose the same way on every run.
func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SetPath assigns a path inside obj, in place. obj must be the caller's own
// copy.
//
// An object key that is absent or null is created as an object (a group the
// row has not filled yet). An intermediate scalar is replaced by an object,
// because the alternative is to fail on a path the caller can see is right in
// the schema. An ARRAY is never replaced: the next step must address one of
// its rows, and a selector that addresses none is a *NoMatchError — creating
// an array row out of thin air is `pay blocks add --field …`'s job.
func SetPath(obj map[string]any, key string, value any) error {
	p, err := ParsePath(key)
	if err != nil {
		return err
	}
	return setSteps(obj, p, 0, value, Location{})
}

func setSteps(obj map[string]any, p Path, i int, value any, trail Location) error {
	step := p.Steps[i]
	if step.Bracket {
		return &PathError{Kind: PathRowOnObject, Path: p.Raw, At: trail, Rest: p.Steps[i:], Container: obj,
			Have:   sortedKeysOf(obj),
			Detail: fmt.Sprintf("it is an object, and %s addresses a row of an array", step)}
	}
	here := trail.With(Hop{Key: step.Text})
	if i == len(p.Steps)-1 {
		obj[step.Text] = value
		return nil
	}
	next := p.Steps[i+1]
	switch cur := obj[step.Text].(type) {
	case map[string]any:
		return setSteps(cur, p, i+1, value, here)
	case []any:
		return setInArray(cur, p, i+1, value, here)
	case nil:
		if next.looksLikeRow() {
			return &PathError{Kind: PathAbsent, Path: p.Raw, At: here, Rest: p.Steps[i+1:],
				Null: obj[step.Text] == nil && hasKey(obj, step.Text), Container: obj, Have: sortedKeysOf(obj)}
		}
	default:
		if next.looksLikeRow() {
			return &PathError{Kind: PathNotContainer, Path: p.Raw, At: here, Rest: p.Steps[i+1:], Container: cur,
				Detail: fmt.Sprintf("it is %s and has no rows", jsonKind(cur))}
		}
	}
	sub := map[string]any{}
	obj[step.Text] = sub
	return setSteps(sub, p, i+1, value, here)
}

// setInArray handles a step that addresses a row of list. The row is edited in
// place: the whole row was deep-copied by Set before any step ran.
func setInArray(list []any, p Path, i int, value any, trail Location) error {
	sel, err := p.Steps[i].selector()
	if err != nil {
		return &PathError{Kind: PathKeyOnArray, Path: p.Raw, At: trail, Rest: p.Steps[i:], Container: list,
			Detail: fmt.Sprintf("it is an array, so %q must be a row selector (%s)", p.Steps[i].Text, SelectorGrammar)}
	}
	idx, err := ResolveOne(list, sel, trail.String())
	if err != nil {
		return err
	}
	here := trail.With(Hop{Row: true, Index: idx})
	if i == len(p.Steps)-1 {
		list[idx] = value
		return nil
	}
	row, ok := list[idx].(map[string]any)
	if !ok {
		return &PathError{Kind: PathNotContainer, Path: p.Raw, At: here, Rest: p.Steps[i+1:], Container: list[idx],
			Detail: fmt.Sprintf("it is %s and has no fields", jsonKind(list[idx]))}
	}
	return setSteps(row, p, i+1, value, here)
}

func hasKey(obj map[string]any, k string) bool {
	_, ok := obj[k]
	return ok
}

// ClearPath clears a path inside obj, in place: the value becomes what makes
// Payload store "empty" (ClearedValue). It does NOT delete the key. Payload
// matches a row by its id and keeps the stored value of every key the row
// omits, so a deleted key is written back unchanged — `pay blocks set --unset
// blockName | pay apply` used to answer 2xx and change nothing.
//
// A key that does not exist is not an error: `--unset` states a desired end
// state, and it is already true. A row selector that addresses no row IS an
// error, because that is a typo in the path rather than a state, and a row
// cannot be cleared — removing one is `pay blocks rm --field …`.
func ClearPath(obj map[string]any, key string) error {
	p, err := ParsePath(key)
	if err != nil {
		return err
	}
	var cur any = obj
	var trail Location
	for i, step := range p.Steps {
		last := i == len(p.Steps)-1
		switch c := cur.(type) {
		case map[string]any:
			if step.Bracket {
				return &PathError{Kind: PathRowOnObject, Path: p.Raw, At: trail, Rest: p.Steps[i:], Container: c,
					Have:   sortedKeysOf(c),
					Detail: fmt.Sprintf("it is an object, and %s addresses a row of an array", step)}
			}
			if last {
				if v, ok := c[step.Text]; ok {
					c[step.Text] = ClearedValue(v)
				}
				return nil
			}
			trail = trail.With(Hop{Key: step.Text})
			cur = c[step.Text]
		case []any:
			sel, err := step.selector()
			if err != nil {
				return &PathError{Kind: PathKeyOnArray, Path: p.Raw, At: trail, Rest: p.Steps[i:], Container: c,
					Detail: fmt.Sprintf("it is an array, so %q must be a row selector (%s)", step.Text, SelectorGrammar)}
			}
			idx, err := ResolveOne(c, sel, trail.String())
			if err != nil {
				return err
			}
			if last {
				return &PathError{Kind: PathNotContainer, Path: p.Raw, At: trail, Container: c,
					Detail: "--unset clears a field, and " + step.String() + " is a whole row; remove rows with `pay blocks rm`"}
			}
			trail = trail.With(Hop{Row: true, Index: idx})
			cur = c[idx]
		default:
			// Absent, null or a scalar: nothing below it exists to delete.
			return nil
		}
	}
	return nil
}

// ClearedValue is the value that clears a field holding v, verified against
// Payload 3.87 (Postgres) on localhost:
//
//   - an array, blocks or hasMany field: [] (null answers 500);
//   - a named group (a plain object): every sub-field cleared, because null
//     answers 500 and {} keeps every stored sub-field;
//   - anything else — a scalar, a rich-text editor state, a relationship or
//     upload value (an id, a populated document, a {relationTo, value}): null.
func ClearedValue(v any) any {
	switch t := v.(type) {
	case []any:
		return []any{}
	case map[string]any:
		if IsEditorState(t) || isPopulatedDoc(t) || isRelationValue(t) {
			return nil
		}
		out := make(map[string]any, len(t))
		for k, sub := range t {
			out[k] = ClearedValue(sub)
		}
		return out
	}
	return nil
}

// StripIDs removes the server-generated row ids from a row: the row's own
// `id` and the `id` of every row of every array nested inside it, at every
// depth. Exported because `pay blocks add` needs it for a row pasted out of
// another document, and `pay blocks new --from-example` for a real instance.
//
// Only ROW ids are stripped — the ids Payload matches rows by, which a copy
// must not reuse. Two other kinds of `id` are references and are kept, exactly
// as the admin UI's "duplicate row" keeps them:
//
//   - a relationship or upload value (an expanded document: `id` plus
//     `createdAt`/`updatedAt`, or a polymorphic `{relationTo, value}`), whose
//     `id` IS the reference — on klixpert stage a Lexical block's upload field
//     reads back populated even at depth 0, and stripping its `id` left an
//     object that names no upload at all;
//   - anything inside a Lexical editor state: a block node's `fields.id`, a
//     link or upload node's `id`. Payload's lexical hooks key their node map by
//     those ids and skip a node that has none, and the upload validator reads
//     `value.id`. They are scoped to their own rich-text value, so a copy that
//     keeps them does not collide with the original.
//
// A group object (a map under a key) has no id of its own; its arrays are
// walked like the row's.
func StripIDs(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if IsEditorState(t) || isPopulatedDoc(t) || isRelationValue(t) {
			return v
		}
		delete(t, KeyID)
		stripNested(t)
	case []any:
		stripRowList(t)
	}
	return v
}

// stripNested strips the row ids below a row or group object, without
// touching the object's own keys.
func stripNested(obj map[string]any) {
	for _, sub := range obj {
		switch c := sub.(type) {
		case []any:
			stripRowList(c)
		case map[string]any:
			if IsEditorState(c) || isPopulatedDoc(c) || isRelationValue(c) {
				continue
			}
			// A group: it has no id, and an `id` key on a plain object that
			// is not an array element is not a row id either.
			stripNested(c)
		}
	}
}

// stripRowList strips the id of every row of list. An element that is a
// relationship value (a hasMany relationship read at depth > 0, or a
// polymorphic ref) is a reference, not a row, and is left alone.
func stripRowList(list []any) {
	for _, el := range list {
		m, ok := el.(map[string]any)
		if !ok || IsEditorState(m) || isPopulatedDoc(m) || isRelationValue(m) {
			continue
		}
		delete(m, KeyID)
		stripNested(m)
	}
}

// isRelationValue reports a polymorphic relationship value
// `{relationTo, value}` (its value may be an id or an expanded document).
func isRelationValue(m map[string]any) bool {
	if _, ok := m["relationTo"].(string); !ok {
		return false
	}
	_, ok := m["value"]
	return ok && len(m) == 2
}

// deepCopy clones a decoded-JSON value. Scalars are immutable and are returned
// as they are.
func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, sub := range t {
			out[k] = deepCopy(sub)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, sub := range t {
			out[i] = deepCopy(sub)
		}
		return out
	}
	return v
}

// ---------------------------------------------------------------------------
// listing
// ---------------------------------------------------------------------------

// Summary is one row as `pay blocks ls` reports it: enough to choose a row and
// to write a selector for it, and nothing else.
type Summary struct {
	Index     int    `json:"index"`
	ID        string `json:"id,omitempty"`
	BlockType string `json:"block_type,omitempty"`
	BlockName string `json:"block_name,omitempty"`
	// Selector is the shortest selector that addresses THIS row and no other.
	// It is emitted rather than left to the caller because the shortest one is
	// not the obvious one: an index is stale after the next edit in the pipe,
	// so an `id:` is preferred whenever the row has an id.
	Selector string `json:"selector"`
	// Fields is the row's own keys, minus Payload's plumbing, so a caller can
	// see what `pay blocks set` could address without printing the whole row.
	Fields []string `json:"fields,omitempty"`
	// Row is the complete row. It is only filled in for `--long`.
	Row any `json:"row,omitempty"`
}

// Summarize renders every row. It never fails: a row that is not an object
// still gets an entry, because a listing that silently skips rows would make
// the printed indices disagree with the real ones.
func Summarize(list []any) []Summary {
	out := make([]Summary, 0, len(list))
	typeCounts := map[string]int{}
	typeTotals := map[string]int{}
	for _, row := range list {
		if obj, ok := row.(map[string]any); ok {
			typeTotals[scalarString(obj[KeyBlockType])]++
		}
	}
	for i, row := range list {
		s := Summary{Index: i}
		obj, ok := row.(map[string]any)
		if !ok {
			s.Selector = strconv.Itoa(i)
			out = append(out, s)
			continue
		}
		s.ID = scalarString(obj[KeyID])
		s.BlockType = scalarString(obj[KeyBlockType])
		s.BlockName = scalarString(obj[KeyBlockName])
		s.Fields = contentFields(obj)

		nth := typeCounts[s.BlockType]
		typeCounts[s.BlockType]++
		s.Selector = shortestSelector(s, nth, typeTotals[s.BlockType])
		out = append(out, s)
	}
	return out
}

// shortestSelector picks the stablest unambiguous handle for a row: its id if
// it has one, then a unique blockType, then `type:slug[n]`, then the index.
func shortestSelector(s Summary, nth, total int) string {
	switch {
	case s.ID != "":
		return "id:" + s.ID
	case s.BlockType != "" && total == 1:
		return "type:" + s.BlockType
	case s.BlockType != "":
		return fmt.Sprintf("type:%s[%d]", s.BlockType, nth)
	default:
		return strconv.Itoa(s.Index)
	}
}

// contentFields is a row's keys minus Payload's three plumbing keys, sorted.
func contentFields(obj map[string]any) []string {
	out := make([]string, 0, len(obj))
	for k := range obj {
		if k == KeyID || k == KeyBlockType || k == KeyBlockName {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// Types returns the distinct blockTypes present, sorted. Used to tell a caller
// what a failed `type:` selector could have matched.
func Types(list []any) []string {
	seen := map[string]bool{}
	var out []string
	for _, row := range list {
		obj, ok := row.(map[string]any)
		if !ok {
			continue
		}
		t := scalarString(obj[KeyBlockType])
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// LooksLikeBlocks reports whether every object row carries a `blockType`, which
// is what distinguishes a blocks field from a plain `array` field in a document
// PayCLI has no schema for. An empty array is not blocks: there is nothing to
// read it from, and guessing would name a field the caller never asked for.
func LooksLikeBlocks(list []any) bool {
	if len(list) == 0 {
		return false
	}
	for _, row := range list {
		obj, ok := row.(map[string]any)
		if !ok {
			return false
		}
		if scalarString(obj[KeyBlockType]) == "" {
			return false
		}
	}
	return true
}
