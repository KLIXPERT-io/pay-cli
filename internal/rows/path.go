package rows

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// field paths
// ---------------------------------------------------------------------------
//
// A Payload page is rarely one flat array. The klixpert pages this package was
// grounded on keep a `group` block in `layout` whose own `blocks` field holds
// six processStep rows, and a `grid` inside a group inside `layout` holding
// three more. Reaching those rows means walking through TWO kinds of hop:
// object keys (`layout`, `blocks`, a group's `settings`) and array rows
// (`layout[2]`). A dotted path alone cannot say which is which — `layout.2`
// could be a key called "2" — so the grammar below lets a row hop be written
// either way and decides by what the document holds at that point: an array
// is always entered by a row selector, an object always by a key.
//
//	layout                     the top-level field
//	layout.2.blocks            row 2 of layout, then its blocks
//	layout[2].blocks           the same, bracketed
//	layout[id:6ab8…].blocks    by id — survives another stage reordering layout
//	layout[type:group[1]].blocks
//	                           the second group; the §9.11.3 selector grammar
//	                           is reused verbatim inside the brackets
//
// Nothing here reads a schema. A path resolves against the document's own
// shape, which is what makes it work on a project whose GraphQL introspection
// is disabled and whose block schemas PayCLI therefore never saw.

// PathGrammar is the one-line statement of the field-path grammar, shared by
// every help text that documents --field.
const PathGrammar = `KEY ( .KEY | .N | [SELECTOR] )...   e.g. layout[2].blocks, layout[id:6ab8…].blocks`

// Step is one segment of a parsed path, exactly as the caller wrote it. Whether
// it is an object key or a row selector is decided only while walking, by what
// the document holds at that point.
type Step struct {
	// Text is the segment without its brackets.
	Text string
	// Bracket reports `[…]`. A bracketed step is always a row selector, and its
	// selector is parsed eagerly so a malformed one fails before any document
	// is read.
	Bracket bool
	// Sel is the pre-parsed selector of a bracketed step.
	Sel *Selector
}

func (s Step) String() string {
	if s.Bracket {
		return "[" + s.Text + "]"
	}
	return s.Text
}

// looksLikeRow reports a step that can only mean "a row": bracketed, or a bare
// integer. It is what distinguishes "this absent key would have been an array"
// from "this absent key is a group that can be created".
func (s Step) looksLikeRow() bool {
	if s.Bracket {
		return true
	}
	_, err := strconv.Atoi(s.Text)
	return err == nil
}

// selector returns the step read as a row selector. A bare step is parsed here,
// lazily, because until the walk reached an array it was just as likely a key.
func (s Step) selector() (Selector, error) {
	if s.Sel != nil {
		return *s.Sel, nil
	}
	return ParseSelector(s.Text)
}

// Path is a parsed field path.
type Path struct {
	Raw   string
	Steps []Step
}

// ParsePath parses a field path. It never consults a document.
func ParsePath(raw string) (Path, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Path{}, fmt.Errorf("the path is empty (%s)", PathGrammar)
	}
	p := Path{Raw: s}
	// key reads one dotted segment starting at i and returns where it ended.
	key := func(i int) (int, error) {
		j := i
		for j < len(s) && s[j] != '.' && s[j] != '[' && s[j] != ']' {
			j++
		}
		if j == i {
			if i == 0 && i < len(s) && s[i] == '[' {
				return 0, fmt.Errorf("%q starts with a row selector; a path starts with a field name", s)
			}
			return 0, fmt.Errorf("%q has an empty segment at offset %d", s, i)
		}
		p.Steps = append(p.Steps, Step{Text: s[i:j]})
		return j, nil
	}
	i, err := key(0)
	if err != nil {
		return Path{}, err
	}
	for i < len(s) {
		switch s[i] {
		case '.':
			if i, err = key(i + 1); err != nil {
				return Path{}, err
			}
		case '[':
			end, err := matchBracket(s, i)
			if err != nil {
				return Path{}, err
			}
			inner := s[i+1 : end]
			// `layout.type:group[1].blocks`: the dotted spelling of the
			// bracketed `layout[type:group[1]].blocks`. A key never contains a
			// ':', so a bare `type:SLUG` step directly followed by `[N]` can
			// only mean the n-th row of that type; read it that way instead
			// of splitting it into an ambiguous `type:SLUG` and a row [N].
			if n := len(p.Steps); n > 1 && s[i-1] != ']' && !p.Steps[n-1].Bracket &&
				strings.HasPrefix(p.Steps[n-1].Text, "type:") && isInt(inner) {
				joined := p.Steps[n-1].Text + "[" + inner + "]"
				if sel, err := ParseSelector(joined); err == nil {
					p.Steps[n-1] = Step{Text: sel.Raw, Bracket: true, Sel: &sel}
					i = end + 1
					if i < len(s) && s[i] != '.' && s[i] != '[' {
						return Path{}, fmt.Errorf("%q: the ] at offset %d must be followed by '.', '[' or the end", s, end)
					}
					continue
				}
			}
			sel, err := ParseSelector(inner)
			if err != nil {
				return Path{}, fmt.Errorf("%q: [%s] is not a row selector: %w", s, inner, err)
			}
			p.Steps = append(p.Steps, Step{Text: sel.Raw, Bracket: true, Sel: &sel})
			i = end + 1
			if i < len(s) && s[i] != '.' && s[i] != '[' {
				return Path{}, fmt.Errorf("%q: the ] at offset %d must be followed by '.', '[' or the end", s, end)
			}
		default: // ']'
			return Path{}, fmt.Errorf("%q has a ] with no [ before it at offset %d", s, i)
		}
	}
	return p, nil
}

func isInt(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}

// matchBracket returns the index of the ] that closes the [ at open, counting
// nested pairs so `[type:group[1]]` is one selector.
func matchBracket(s string, open int) (int, error) {
	depth := 0
	for j := open; j < len(s); j++ {
		switch s[j] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				if j == open+1 {
					return 0, fmt.Errorf("%q has an empty [] at offset %d", s, open)
				}
				return j, nil
			}
		}
	}
	return 0, fmt.Errorf("%q has a [ at offset %d with no closing ]", s, open)
}

// ---------------------------------------------------------------------------
// locations
// ---------------------------------------------------------------------------

// Hop is one resolved step: an object key, or a row of an array.
type Hop struct {
	// Key is set for an object hop.
	Key string
	// Row reports an array hop; Index is then the row's position NOW.
	Row   bool
	Index int
	// Selector is the shortest selector that addresses this row among its
	// siblings — an `id:` whenever it has one — so a location can be printed in
	// a form that survives another stage renumbering the array.
	Selector string
	// BlockType is the row's blockType, "" when it has none.
	BlockType string
}

// Location is a resolved path: the concrete hops from the document root.
type Location struct {
	Hops []Hop
}

func (l Location) render(row func(Hop) string) string {
	var b strings.Builder
	for i, h := range l.Hops {
		if h.Row {
			b.WriteString("[" + row(h) + "]")
			continue
		}
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(h.Key)
	}
	return b.String()
}

// String is the location with row positions: `layout[2].blocks`. It is exact
// for the document it was resolved against and stale after the next edit.
func (l Location) String() string {
	return l.render(func(h Hop) string { return strconv.Itoa(h.Index) })
}

// Stable is the location with each row's shortest selector:
// `layout[id:6ab8…].blocks`. It is what a later stage should be given, because
// an index is stale the moment another stage inserts or removes a row.
func (l Location) Stable() string {
	return l.render(func(h Hop) string {
		if h.Selector != "" {
			return h.Selector
		}
		return strconv.Itoa(h.Index)
	})
}

// Pattern is the location with every row erased: `layout[].blocks`. Every
// group on the page shares it, which is what a per-field schema is keyed by.
func (l Location) Pattern() string {
	return l.render(func(Hop) string { return "" })
}

// Nested reports whether the location passes through an array row.
func (l Location) Nested() bool {
	for _, h := range l.Hops {
		if h.Row {
			return true
		}
	}
	return false
}

// Root is the unit Payload's REST API can write: the object-key prefix up to
// the first array row. `layout[2].blocks` is written as the whole of `layout`,
// because Payload has no per-row endpoint — a PATCH replaces the array.
func (l Location) Root() string {
	var keys []string
	for _, h := range l.Hops {
		if h.Row {
			break
		}
		keys = append(keys, h.Key)
	}
	return strings.Join(keys, ".")
}

// Parent returns the location of the innermost row l passes through (ending
// in that row's hop) and the key path from that row to the end of l
// ("blocks", "settings.items"). ok is false for a location through no row.
func (l Location) Parent() (row Location, rel string, ok bool) {
	last := -1
	for i, h := range l.Hops {
		if h.Row {
			last = i
		}
	}
	if last < 0 {
		return Location{}, "", false
	}
	keys := make([]string, 0, len(l.Hops)-last-1)
	for _, h := range l.Hops[last+1:] {
		keys = append(keys, h.Key)
	}
	return Location{Hops: l.Hops[: last+1 : last+1]}, strings.Join(keys, "."), true
}

// Last returns the final hop, the zero Hop for an empty location.
func (l Location) Last() Hop {
	if len(l.Hops) == 0 {
		return Hop{}
	}
	return l.Hops[len(l.Hops)-1]
}

// With returns a copy of l with one more hop.
func (l Location) With(h Hop) Location {
	hops := make([]Hop, 0, len(l.Hops)+1)
	hops = append(hops, l.Hops...)
	return Location{Hops: append(hops, h)}
}

// KeyLocation is the location of a plain dotted object path, for a field that
// was chosen rather than parsed.
func KeyLocation(dotted string) Location {
	var l Location
	for _, k := range strings.Split(dotted, ".") {
		l.Hops = append(l.Hops, Hop{Key: k})
	}
	return l
}

// ---------------------------------------------------------------------------
// resolving
// ---------------------------------------------------------------------------

// PathErrorKind names why a path did not resolve.
type PathErrorKind string

const (
	// PathAbsent is a key that is not there (or is null) on the object the
	// walk reached.
	PathAbsent PathErrorKind = "absent"
	// PathNotContainer is a walk that reached a scalar and had steps left.
	PathNotContainer PathErrorKind = "not_container"
	// PathKeyOnArray is a bare step on an array that is not a row selector.
	PathKeyOnArray PathErrorKind = "key_on_array"
	// PathRowOnObject is a [selector] step on an object.
	PathRowOnObject PathErrorKind = "row_on_object"
	// PathNotArray is a path that resolved to something other than an array.
	PathNotArray PathErrorKind = "not_array"
)

// PathError is a path that did not resolve, with enough context to say what IS
// there instead.
type PathError struct {
	Kind PathErrorKind
	// Path is the path as the caller wrote it.
	Path string
	// At is where the walk stopped. For PathAbsent it INCLUDES the absent key,
	// so At.String() names the field that is missing.
	At Location
	// Rest are the steps after At, not walked.
	Rest []Step
	// InRow reports that At passes through an array row: the absent key was
	// looked up on a block row, not on the document.
	InRow bool
	// Null reports a key that is present with a null value.
	Null bool
	// Container is the object the missing key was looked up on (PathAbsent) or
	// the value found (PathNotArray), for the caller to describe.
	Container any
	// Have lists what IS there: the object's keys for PathAbsent, a row's
	// array-valued keys for PathNotArray. Sorted.
	Have []string
	// Detail is a one-clause explanation for the message.
	Detail string
}

func (e *PathError) Error() string {
	where := e.At.String()
	switch e.Kind {
	case PathAbsent:
		if e.Null {
			return fmt.Sprintf("%s is null, so the path %q cannot continue into it", where, e.Path)
		}
		return fmt.Sprintf("%s is absent (path %q)", where, e.Path)
	case PathNotArray:
		return fmt.Sprintf("%s is %s, not an array of rows%s", where, e.Detail, suffix(e.Have))
	default:
		return fmt.Sprintf("path %q stops at %s: %s", e.Path, where, e.Detail)
	}
}

func suffix(have []string) string {
	if len(have) == 0 {
		return ""
	}
	return "; the arrays inside it are: " + strings.Join(have, ", ")
}

// NextLooksLikeRow reports whether the first unwalked step can only address a
// row — which turns "the key is absent" into "the parent row does not exist".
func (e *PathError) NextLooksLikeRow() bool {
	return len(e.Rest) > 0 && e.Rest[0].looksLikeRow()
}

// RestAreKeys reports whether every unwalked step is a plain key, so the whole
// remainder can be created as nested objects.
func (e *PathError) RestAreKeys() bool {
	for _, s := range e.Rest {
		if s.looksLikeRow() {
			return false
		}
	}
	return true
}

// Resolved is a path that reached its end.
type Resolved struct {
	At Location
	// Value is what the path addresses. It is nil for a key that is present
	// and null.
	Value any
}

// Resolve walks p through doc. It returns the location and the value there, or
// a typed error: *NoMatchError / *AmbiguousError when a row selector did not
// address exactly one row (their Field is the array's location), *PathError
// for everything else. It never requires the end value to be an array; the
// caller decides what a non-array means.
func Resolve(doc map[string]any, p Path) (Resolved, error) {
	var at Location
	var cur any = doc
	inRow := false
	for i, step := range p.Steps {
		switch c := cur.(type) {
		case map[string]any:
			if step.Bracket {
				return Resolved{}, &PathError{Kind: PathRowOnObject, Path: p.Raw, At: at, Rest: p.Steps[i:],
					InRow: inRow, Container: c, Have: sortedKeysOf(c),
					Detail: fmt.Sprintf("it is an object, and %s addresses a row of an array", step)}
			}
			v, ok := c[step.Text]
			next := at.With(Hop{Key: step.Text})
			if !ok || (v == nil && i < len(p.Steps)-1) {
				return Resolved{}, &PathError{Kind: PathAbsent, Path: p.Raw, At: next, Rest: p.Steps[i+1:],
					InRow: inRow, Null: ok, Container: c, Have: sortedKeysOf(c)}
			}
			at, cur = next, v
		case []any:
			sel, err := step.selector()
			if err != nil {
				return Resolved{}, &PathError{Kind: PathKeyOnArray, Path: p.Raw, At: at, Rest: p.Steps[i:],
					InRow: inRow, Container: c,
					Detail: fmt.Sprintf("it is an array, so %q must be a row selector (%s)", step.Text, SelectorGrammar)}
			}
			idx, err := ResolveOne(c, sel, at.String())
			if err != nil {
				return Resolved{}, err
			}
			sum := Summarize(c)[idx]
			at = at.With(Hop{Row: true, Index: idx, Selector: sum.Selector, BlockType: sum.BlockType})
			cur, inRow = c[idx], true
		default:
			return Resolved{}, &PathError{Kind: PathNotContainer, Path: p.Raw, At: at, Rest: p.Steps[i:],
				InRow: inRow, Container: cur,
				Detail: fmt.Sprintf("it is %s and has nothing inside it", jsonKind(cur))}
		}
	}
	return Resolved{At: at, Value: cur}, nil
}

// Lookup returns the value at a resolved location, false when the document no
// longer has it.
func Lookup(doc map[string]any, loc Location) (any, bool) {
	var cur any = doc
	for _, h := range loc.Hops {
		if h.Row {
			list, ok := cur.([]any)
			if !ok || h.Index < 0 || h.Index >= len(list) {
				return nil, false
			}
			cur = list[h.Index]
			continue
		}
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok := obj[h.Key]
		if !ok {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

// NotArrayError builds the error for a resolved value that is not an array,
// listing the arrays inside it when it is an object — `layout[2]` is a row, and
// the caller almost certainly meant one of its nested fields.
func NotArrayError(p Path, r Resolved) *PathError {
	e := &PathError{Kind: PathNotArray, Path: p.Raw, At: r.At, Container: r.Value, Detail: jsonKind(r.Value)}
	if obj, ok := r.Value.(map[string]any); ok {
		if bt := scalarString(obj[KeyBlockType]); bt != "" {
			e.Detail = "a " + bt + " row"
		} else if len(r.At.Hops) > 0 && r.At.Hops[len(r.At.Hops)-1].Row {
			e.Detail = "a row"
		}
		for _, k := range sortedKeysOf(obj) {
			if isRowArray(obj[k]) {
				e.Have = append(e.Have, k)
			}
		}
	}
	return e
}

func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	default:
		return "a number"
	}
}

func sortedKeysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// writing back
// ---------------------------------------------------------------------------

// Replace returns a copy of doc with value stored at loc. Every object and
// array on the way is copied, never mutated: the decoded input is still what
// the error paths print. An object hop that is absent is created (a top-level
// layout that is null has to be fillable); a row hop must exist, because a
// location is only ever built by resolving it.
func Replace(doc map[string]any, loc Location, value any) (map[string]any, error) {
	if len(loc.Hops) == 0 {
		return nil, errors.New("an empty location cannot be written")
	}
	out, err := replaceIn(doc, loc.Hops, value, Location{})
	if err != nil {
		return nil, err
	}
	m, _ := out.(map[string]any)
	return m, nil
}

func replaceIn(cur any, hops []Hop, value any, trail Location) (any, error) {
	if len(hops) == 0 {
		return value, nil
	}
	h := hops[0]
	if h.Row {
		list, ok := cur.([]any)
		if !ok || h.Index < 0 || h.Index >= len(list) {
			return nil, fmt.Errorf("%s no longer has a row %d", trail, h.Index)
		}
		sub, err := replaceIn(list[h.Index], hops[1:], value, trail.With(h))
		if err != nil {
			return nil, err
		}
		cp := clone(list)
		cp[h.Index] = sub
		return cp, nil
	}
	obj, _ := cur.(map[string]any)
	cp := make(map[string]any, len(obj)+1)
	for k, v := range obj {
		cp[k] = v
	}
	sub, err := replaceIn(cp[h.Key], hops[1:], value, trail.With(h))
	if err != nil {
		return nil, err
	}
	cp[h.Key] = sub
	return cp, nil
}

// ---------------------------------------------------------------------------
// shape knowledge from the document itself
// ---------------------------------------------------------------------------

// ObservedArrayField reports whether any row anywhere in doc whose blockType is
// blockType carries an array at rel (a dotted key path inside the row). It is
// the schema-free answer to "does a group have a `blocks` field?": on a site
// whose GraphQL introspection is off, the other groups on the page are the
// only evidence there is.
func ObservedArrayField(doc any, blockType, rel string) bool {
	if blockType == "" || rel == "" {
		return false
	}
	found := false
	var walk func(v any)
	walk = func(v any) {
		if found {
			return
		}
		switch t := v.(type) {
		case map[string]any:
			if IsEditorState(t) {
				return
			}
			if scalarString(t[KeyBlockType]) == blockType {
				if _, ok := dotted(t, rel).([]any); ok {
					found = true
					return
				}
			}
			for _, sub := range t {
				walk(sub)
			}
		case []any:
			for _, sub := range t {
				walk(sub)
			}
		}
	}
	walk(doc)
	return found
}

// Row kinds, as ArrayRowKind reports them.
const (
	// RowKindBlocks is an array whose rows carry a blockType.
	RowKindBlocks = "blocks"
	// RowKindArray is an array whose rows carry none: a plain array field.
	RowKindArray = "array"
)

// ArrayRowKind reads what kind of field list is from its rows: RowKindBlocks
// when every row carries a blockType, RowKindArray when none does, "" when the
// list is empty, mixed, or holds only `{}` rows (an orphan left behind by a
// nested restructure says nothing either way).
func ArrayRowKind(list []any) string {
	typed, untyped := 0, 0
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			return ""
		}
		if len(m) == 0 {
			continue
		}
		if scalarString(m[KeyBlockType]) != "" {
			typed++
		} else {
			untyped++
		}
	}
	switch {
	case typed > 0 && untyped == 0:
		return RowKindBlocks
	case untyped > 0 && typed == 0:
		return RowKindArray
	}
	return ""
}

// ObservedRowKind is ArrayRowKind over the arrays at rel of every row of
// blockType anywhere in doc, "" when they disagree or none has a row.
func ObservedRowKind(doc any, blockType, rel string) string {
	if blockType == "" || rel == "" {
		return ""
	}
	seen := ""
	conflict := false
	var walk func(v any)
	walk = func(v any) {
		if conflict {
			return
		}
		switch t := v.(type) {
		case map[string]any:
			if IsEditorState(t) || isPopulatedDoc(t) {
				return
			}
			if scalarString(t[KeyBlockType]) == blockType {
				if list, ok := dotted(t, rel).([]any); ok {
					if k := ArrayRowKind(list); k != "" {
						if seen != "" && seen != k {
							conflict = true
							return
						}
						seen = k
					}
				}
			}
			for _, sub := range t {
				walk(sub)
			}
		case []any:
			for _, sub := range t {
				walk(sub)
			}
		}
	}
	// The document itself has an id and timestamps: start below it, or it
	// would be mistaken for an expanded relationship.
	if m, ok := doc.(map[string]any); ok {
		for _, sub := range m {
			walk(sub)
		}
	} else {
		walk(doc)
	}
	if conflict {
		return ""
	}
	return seen
}

func dotted(obj map[string]any, rel string) any {
	var cur any = obj
	for _, k := range strings.Split(rel, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

// IsEditorState reports a Lexical rich-text value: an object whose `root` is a
// node of type "root". Its `children` are arrays of objects that look exactly
// like rows, so every walk over a document must stop at one.
func IsEditorState(obj map[string]any) bool {
	root, ok := obj["root"].(map[string]any)
	if !ok {
		return false
	}
	return scalarString(root["type"]) == "root"
}

// isPopulatedDoc reports a relationship a read above depth 0 expanded into a
// whole document. Its arrays (an upload's `sizes`, a page's own layout) are
// not rows of THIS document.
func isPopulatedDoc(obj map[string]any) bool {
	if _, ok := obj[KeyID]; !ok {
		return false
	}
	_, created := obj["createdAt"]
	_, updated := obj["updatedAt"]
	return created || updated
}

// isRowArray reports an array that can hold rows: empty, or objects only. An
// array of ids (a hasMany relationship) or strings (a hasMany select) cannot.
func isRowArray(v any) bool {
	list, ok := v.([]any)
	if !ok {
		return false
	}
	for _, e := range list {
		if _, ok := e.(map[string]any); !ok {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// recursive listing
// ---------------------------------------------------------------------------

// Node is one row of a recursive listing.
type Node struct {
	// Depth is 0 for a row of the listed field, 1 for a row nested in one of
	// those, and so on.
	Depth int `json:"depth"`
	// Path is the row's own location with positions: `layout[2].blocks[1]`.
	Path string `json:"path"`
	// Field is the exact --field value that addresses the array this row is
	// in, written with stable selectors: `layout[id:6ab8…].blocks`. It is
	// empty (and omitted) for rows that arrived bare, with no document to
	// address them in.
	Field string `json:"field,omitempty"`
	Summary
	// Nested maps each array inside the row (a key path relative to it) to its
	// row count. An empty array is listed too: an empty nested blocks field is
	// exactly where a caller wants to `pay blocks add`.
	Nested map[string]int `json:"nested,omitempty"`
	// Arrays describes each array in Nested (same keys) as far as anything
	// knows: whether it is a blocks or a plain array field, and which
	// blockTypes it takes. Filled by the caller, which holds the schema and
	// the census; an array nothing knows about has no entry.
	Arrays map[string]ArrayInfo `json:"arrays,omitempty"`
	// Value is the row itself, for a caller that fills Summary.Row (--long).
	// It is never serialised on its own.
	Value any `json:"-"`
	// At is the row's own location, for a caller that resolves its nested
	// arrays.
	At Location `json:"-"`
}

// ArrayInfo is what is known about one nested array of a listed row.
type ArrayInfo struct {
	// Kind is "blocks" (rows carry a blockType) or "array" (a plain array
	// field: rows carry none, and `pay blocks add -` adds one).
	Kind string `json:"kind,omitempty"`
	// Accepts are the blockTypes the field takes, with where that list came
	// from; an observed list is not everything the field allows.
	Accepts              []string `json:"accepts,omitempty"`
	AcceptsSource        string   `json:"accepts_source,omitempty"`
	AcceptsAuthoritative *bool    `json:"accepts_authoritative,omitempty"`
}

// Tree lists every row of list and, recursively, every row of every array
// nested inside those rows, in document order (a row, then its children).
// at is the location of list itself.
func Tree(list []any, at Location) []Node {
	var out []Node
	walkTree(list, at, 0, &out)
	return out
}

func walkTree(list []any, at Location, depth int, out *[]Node) {
	sums := Summarize(list)
	field := at.Stable()
	for i, row := range list {
		s := sums[i]
		hop := Hop{Row: true, Index: i, Selector: s.Selector, BlockType: s.BlockType}
		rowAt := at.With(hop)
		n := Node{Depth: depth, Path: rowAt.String(), Field: field, Summary: s, Value: row, At: rowAt}
		obj, ok := row.(map[string]any)
		if !ok {
			*out = append(*out, n)
			continue
		}
		nested := nestedArrays(obj, "", 0)
		if len(nested) > 0 {
			n.Nested = map[string]int{}
			for _, na := range nested {
				n.Nested[na.rel] = len(na.list)
			}
		}
		*out = append(*out, n)
		for _, na := range nested {
			sub := rowAt
			for _, k := range strings.Split(na.rel, ".") {
				sub = sub.With(Hop{Key: k})
			}
			walkTree(na.list, sub, depth+1, out)
		}
	}
}

// FindRow looks for the row whose id is id anywhere in doc (every row array
// at every depth, never inside rich text or an expanded relationship) and
// returns the node describing it: its positional Path and the stable Field of
// the array it is in. ok is false when no row has that id.
func FindRow(doc map[string]any, id string) (Node, bool) {
	if id == "" {
		return Node{}, false
	}
	for _, na := range nestedArrays(doc, "", 0) {
		for _, n := range Tree(na.list, KeyLocation(na.rel)) {
			if n.ID == id {
				return n, true
			}
		}
	}
	return Node{}, false
}

type nestedArray struct {
	rel  string
	list []any
}

// maxGroupDepth bounds the walk into a row's plain objects (group fields). The
// deepest real shape seen is two levels (`link` inside a `links` row); eight
// leaves room without letting a pathological JSON value run away.
const maxGroupDepth = 8

// nestedArrays finds the row arrays directly inside obj, descending through
// group objects but never through rich text or an expanded relationship, and
// never into the arrays themselves (their rows are walked by walkTree).
func nestedArrays(obj map[string]any, prefix string, depth int) []nestedArray {
	var out []nestedArray
	for _, k := range sortedKeysOf(obj) {
		if prefix == "" && (k == KeyID || k == KeyBlockType || k == KeyBlockName) {
			continue
		}
		rel := k
		if prefix != "" {
			rel = prefix + "." + k
		}
		switch v := obj[k].(type) {
		case []any:
			if isRowArray(v) {
				out = append(out, nestedArray{rel: rel, list: v})
			}
		case map[string]any:
			if depth >= maxGroupDepth || IsEditorState(v) || isPopulatedDoc(v) {
				continue
			}
			out = append(out, nestedArrays(v, rel, depth+1)...)
		}
	}
	return out
}
