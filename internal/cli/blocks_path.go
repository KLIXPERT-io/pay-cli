package cli

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/rows"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

// WarnStaleIndex marks a nested --field that enters a row BY POSITION of an
// array an earlier stage of the same pipe reordered or resized.
const WarnStaleIndex = "stale_index"

// Nested --field paths (§9.11.4).
//
// A real page nests: a `group` row in `layout` has its own `blocks`, and a
// `grid` inside that group has blocks of its own. `--field layout.2.blocks`
// used to be read as a chain of OBJECT keys, found no key "2" on the layout
// array, and answered with a field_absent warning and an empty list — so
// `pay blocks ls` said the group was empty and `pay blocks add` would have
// replaced its six rows with one. Every failure to resolve a path is now loud
// and names what IS there; only the one case the pipeline has always treated
// as "empty" (a top-level field the read left out) still is.

// fieldFlagNote is the --field documentation every verb shares.
const fieldFlagNote = "a top-level field (layout) or a nested one through rows: layout[2].blocks, " +
	"layout[id:6ab8…].blocks, layout[type:group[1]].blocks, layout.2.blocks.0.blocks; " +
	"`pay blocks ls --recursive` prints the exact value for every nested array"

// fieldFlagInfo is the FlagInfo entry for --field.
func fieldFlagInfo() FlagInfo {
	return FlagInfo{Grammar: "PATH", Note: fieldFlagNote}
}

// locateRows resolves a --field path against the piped document.
func locateRows(doc map[string]any, field string, shard *discovery.Shard, census rowArrayKnowledge) (*rowField, []output.Warning, error) {
	p, err := rows.ParsePath(field)
	if err != nil {
		return nil, nil, apierr.New(apierr.CodeInvalidArgs, "--field: %s", err.Error()).
			WithHint("the grammar is: %s", rows.PathGrammar)
	}
	res, err := rows.Resolve(doc, p)
	if err != nil {
		var pe *rows.PathError
		if errors.As(err, &pe) {
			if pe.Kind == rows.PathAbsent {
				return absentField(doc, pe, shard, census)
			}
			return nil, nil, pathError(pe)
		}
		return nil, nil, resolveErr(err, nil)
	}

	rf := newRowField(res.At, shard)
	switch v := res.Value.(type) {
	case []any:
		rf.Rows = v
		return rf, nil, nil
	case nil:
		// Present and null. On Postgres an empty blocks or array field reads
		// back as [], so a null at the end of a path is almost always a
		// scalar, relationship or group field (blockName, heading, anchor):
		// it is decided exactly like an absent key, never assumed empty.
		if len(res.At.Hops) == 1 {
			// A top-level null keeps the warning it has always had: a null
			// layout is what `pay blocks add` must be able to fill.
			return rf, []output.Warning{absentWarning(rf.Path)}, nil
		}
		container, _ := rows.Lookup(doc, rows.Location{Hops: res.At.Hops[:len(res.At.Hops)-1]})
		obj, _ := container.(map[string]any)
		return absentField(doc, &rows.PathError{Kind: rows.PathAbsent, Path: p.Raw, At: res.At,
			InRow: res.At.Nested(), Null: true, Container: obj}, shard, census)
	default:
		return nil, nil, pathError(rows.NotArrayError(p, res))
	}
}

// newRowField fills everything a rowField derives from its location.
func newRowField(at rows.Location, shard *discovery.Shard) *rowField {
	rf := &rowField{
		Path:   at.String(),
		Stable: at.Stable(),
		Root:   at.Root(),
		Loc:    at,
		Nested: at.Nested(),
		Shard:  shard,
	}
	// A nested field is looked up by its pattern (`layout[].blocks`). Only a
	// DECLARED, complete slug list may gate a local edit (§7.10c): a list
	// inferred from GraphQL interfaceNames or observed in sampled documents
	// (the census included) is not proof that anything else is invalid, so
	// such a field has no accepted set and add/set do not block on one.
	if slugs, ok := declaredAccepts(shard, at.Pattern()); ok {
		rf.Accepts = slugs
	}
	return rf
}

// absentWarning is the top-level field_absent warning, unchanged in wording
// from before nested paths existed.
func absentWarning(path string) output.Warning {
	return output.Warning{
		Code:    WarnFieldAbsent,
		Message: fmt.Sprintf("the piped document has no %q, so it is being treated as an empty array", path),
		Paths:   []string{path},
		Hint:    "a --select on the read that produced this document drops the fields it does not name; re-read without it",
	}
}

// absentField decides what an absent key — or a null at the end of a path —
// means.
//
// A top-level field the document does not carry (or carries as null) is empty,
// exactly as before: `pay blocks add` must be able to fill a layout that is
// null, and a --select on the read is the usual reason for it.
//
// Below that, "empty" needs evidence. In a present group (`hero.linsk`) the
// collection schema decides when it lists the group's sub-fields. On a ROW
// the discovered block schema, the block census, or another row of the same
// blockType in this document that carries the key as an array decides.
// Otherwise it is a typo or a scalar, and `layout[2].blokcs` or
// `layout[1].blockName` answering "0 rows" is precisely the silent failure
// this file exists to end.
func absentField(doc map[string]any, pe *rows.PathError, shard *discovery.Shard, census rowArrayKnowledge) (*rowField, []output.Warning, error) {
	if !pe.InRow {
		if !pe.RestAreKeys() {
			return nil, nil, missingParentRow(pe)
		}
		at := pe.At
		for _, s := range pe.Rest {
			at = at.With(rows.Hop{Key: s.Text})
		}
		if len(pe.At.Hops) > 1 {
			return groupKeyField(pe, at, shard)
		}
		rf := newRowField(at, shard)
		return rf, []output.Warning{absentWarning(rf.Path)}, nil
	}

	if len(pe.Rest) > 0 {
		switch {
		case pe.NextLooksLikeRow() && !pe.Null:
			// `layout[3].blokcs[1].rows`: a key the row does not carry,
			// with a row step after it. When nothing knows the key it is a
			// typo (unknown_field, with the arrays the row DOES carry);
			// when the row's type is known to have it, the array is just
			// absent here and has no row to enter.
			parent, rel, _ := pe.At.Parent()
			if knownRowArray(doc, shard, census, parent.Last().BlockType, rel) == "" {
				return nil, nil, unknownRowField(pe)
			}
			return nil, nil, missingParentRow(pe)
		case pe.NextLooksLikeRow():
			return nil, nil, missingParentRow(pe)
		case pe.Null:
			// The field exists and is null: not a typo, just nothing inside.
			return nil, nil, apierr.New(apierr.CodeInvalidArgs, "--field: %s", pe.Error()).
				WithHint("`pay blocks ls --recursive` lists every nested array and the --field that addresses it")
		}
		return nil, nil, unknownRowField(pe)
	}
	parent, rel, _ := pe.At.Parent()
	source := knownRowArray(doc, shard, census, parent.Last().BlockType, rel)
	if source == "" {
		if pe.Null {
			return nil, nil, nullRowField(pe)
		}
		return nil, nil, unknownRowField(pe)
	}
	rf := newRowField(pe.At, shard)
	state := fmt.Sprintf("carries no %q", rel)
	if pe.Null {
		state = fmt.Sprintf("has %q: null", rel)
	}
	return rf, []output.Warning{{
		Code: WarnFieldAbsent,
		Message: fmt.Sprintf("%s %s; %s, so it is being treated as an empty array",
			rowLabel(parent), state, source),
		Paths: []string{rf.Path},
		Hint:  "`pay blocks add <blockType> --field " + rf.Stable + "` puts the first row in it",
	}}, nil
}

// groupKeyField decides an absent or null key inside a PRESENT object that is
// not a row — a top-level group such as `hero`. A --select on the read can
// leave a group half-filled (`--select hero.type`), so without a schema the
// legacy "empty, with a warning" answer stands; with one that lists the
// group's sub-fields, a key it does not know is a typo (`hero.linsk`) and a
// key it knows as something other than an array is not a list of rows.
func groupKeyField(pe *rows.PathError, at rows.Location, shard *discovery.Shard) (*rowField, []output.Warning, error) {
	full := at.String()
	missing := pe.At.String()
	parentPath := rows.Location{Hops: pe.At.Hops[:len(pe.At.Hops)-1]}.String()
	state := "has no"
	if pe.Null && len(pe.Rest) == 0 {
		state = "has a null"
	}
	if f, ok := shard.Field(full); ok && f.PayloadType != "" && f.PayloadType != discovery.TypeUnknown {
		if f.PayloadType != discovery.TypeBlocks && f.PayloadType != discovery.TypeArray {
			return nil, nil, apierr.New(apierr.CodeInvalidArgs,
				"--field %s is the %s field %q in the collection schema, not an array of rows", full, f.PayloadType, at.Last().Key).
				WithDidYouMean(schemaArrays(shard, parentPath)...).
				WithHint("--field names an ARRAY whose rows are edited; `pay describe <collection> --field %s` shows what it is", parentPath)
		}
		rf := newRowField(at, shard)
		return rf, []output.Warning{{
			Code: WarnFieldAbsent,
			Message: fmt.Sprintf("the piped document %s %q; the collection schema declares it (%s), so it is being treated as an empty array",
				state, full, f.PayloadType),
			Paths: []string{full},
			Hint:  "`pay blocks add <blockType> --field " + full + "` puts the first row in it; a --select on the read also drops the fields it does not name",
		}}, nil
	}
	if schemaKnowsChildren(shard, parentPath) {
		if _, ok := shard.Field(missing); !ok {
			key := pe.At.Last().Key
			var names []string
			for _, a := range schemaArrays(shard, parentPath) {
				names = append(names, a[len(parentPath)+1:])
			}
			var dym []string
			for _, k := range apierr.DidYouMean(key, names) {
				dym = append(dym, parentPath+"."+k)
			}
			e := apierr.New(apierr.CodeUnknownField,
				"%s has no field %q (path %q), and the collection schema does not declare one", parentPath, key, pe.Path).
				WithDidYouMean(dym...)
			if arrays := schemaArrays(shard, parentPath); len(arrays) > 0 {
				return nil, nil, e.WithHint("the arrays %s carries are: %s", parentPath, strings.Join(arrays, ", "))
			}
			return nil, nil, e.WithHint("`pay describe <collection> --field %s` lists what it carries", parentPath)
		}
	}
	rf := newRowField(at, shard)
	hint := fmt.Sprintf("%s is in the document but %s %q: if that is a typo, `pay blocks ls --recursive` lists every array; "+
		"a --select on the read also drops the fields it does not name", parentPath, state, pe.At.Last().Key)
	if arrays := objectArrays(pe.Container, parentPath); len(arrays) > 0 {
		hint = fmt.Sprintf("%s is in the document but %s %q; the arrays it carries are: %s (a --select on the read also drops the fields it does not name)",
			parentPath, state, pe.At.Last().Key, strings.Join(arrays, ", "))
	}
	return rf, []output.Warning{{
		Code:    WarnFieldAbsent,
		Message: fmt.Sprintf("the piped document %s %q, so it is being treated as an empty array", state, full),
		Paths:   []string{full},
		Hint:    hint,
	}}, nil
}

// schemaKnowsChildren reports a collection schema that lists the sub-fields of
// the object at path — only then is a key it does not name evidence of a typo.
func schemaKnowsChildren(shard *discovery.Shard, path string) bool {
	if shard == nil {
		return false
	}
	for _, f := range shard.Fields {
		if f.Parent != nil && *f.Parent == path {
			return true
		}
	}
	return false
}

// schemaArrays lists the full paths of the array and blocks fields directly
// under the object at path, sorted.
func schemaArrays(shard *discovery.Shard, path string) []string {
	if shard == nil {
		return nil
	}
	var out []string
	for _, f := range shard.Fields {
		if f.Parent != nil && *f.Parent == path && (f.PayloadType == discovery.TypeArray || f.PayloadType == discovery.TypeBlocks) {
			out = append(out, f.Path)
		}
	}
	sort.Strings(out)
	return out
}

// objectArrays lists the full paths of the arrays of rows directly in obj.
func objectArrays(container any, path string) []string {
	obj, ok := container.(map[string]any)
	if !ok {
		return nil
	}
	var out []string
	for _, k := range sortedMapKeys(obj) {
		if _, isList := obj[k].([]any); isList {
			out = append(out, path+"."+k)
		}
	}
	return out
}

// nullRowField is a key a row carries as null that nothing says is an array.
// An empty blocks or array field reads back as [], so a null is a scalar,
// relationship or group field that is not set (blockName, heading, anchor),
// and `ls` answering "0 rows" — or `add` turning it into a list — would be
// wrong.
func nullRowField(pe *rows.PathError) error {
	parent, rel, _ := pe.At.Parent()
	base := pe.At.Hops[:len(pe.At.Hops)-1]
	stable := rows.Location{Hops: base}.Stable()
	arrays := objectArrays(pe.Container, stable)
	why := "nothing in the schema or the document says it is an array"
	if bt := parent.Last().BlockType; bt != "" {
		why = fmt.Sprintf("neither the %s block's schema, the block census nor any other %s row in the document carries %q as one", bt, bt, rel)
	}
	e := apierr.New(apierr.CodeInvalidArgs,
		"--field %s is null, and nothing says it is an array: %s. An empty array reads back as [], so a null is a scalar, "+
			"relationship or group field that is not set", pe.At.String(), why).
		WithDidYouMean(arrays...)
	if len(arrays) == 0 {
		return e.WithHint("%s carries no arrays; to set a field of the row use `pay blocks set <selector> --set %s=… --field <its array>`",
			rowLabel(parent), rel)
	}
	return e.WithHint("--field names an ARRAY of rows; the arrays %s carries are listed in did_you_mean", rowLabel(parent))
}

// rowArrayKnowledge answers, from knowledge outside the document (the cached
// block census), why rel is an array field of blockType, or "" when it does not
// know. nil means there is no such knowledge.
type rowArrayKnowledge func(blockType, rel string) string

// knownRowArray says why rel is believed to be an array field of blockType, or
// "" when nothing does.
func knownRowArray(doc map[string]any, shard *discovery.Shard, census rowArrayKnowledge, blockType, rel string) string {
	if blockType == "" {
		return ""
	}
	if bs, ok := shard.BlockSchemaFor(blockType); ok {
		for _, f := range bs.Fields {
			if f.Path == rel && (f.PayloadType == discovery.TypeBlocks || f.PayloadType == discovery.TypeArray) {
				return fmt.Sprintf("the %s block's schema declares it (%s)", blockType, f.PayloadType)
			}
		}
	}
	if rows.ObservedArrayField(doc, blockType, rel) {
		return fmt.Sprintf("other %s rows in this document carry it as an array", blockType)
	}
	if census != nil {
		return census(blockType, rel)
	}
	return ""
}

// pipeCensus is the merged block knowledge from the CACHED block census
// (§7.10c) of the piped document's collection or global, or nil when no
// census is cached. It never builds the census: a pipe stage must not
// start paging through a collection.
func pipeCensus(d *Deps, target *output.Target, shard *discovery.Shard) *discovery.BlockKnowledge {
	if d == nil || d.RT == nil || target == nil || target.Slug == "" {
		return nil
	}
	census, ok := d.RT.cachedCensus()
	if !ok || census == nil {
		return nil
	}
	kind := discovery.EntityKindCollection
	if target.Kind == "global" {
		kind = discovery.EntityKindGlobal
	}
	// Kept even when the census has no entry for this entity: TypeStats
	// still answers from the other entities' rows of the same blockType.
	return discovery.NewBlockKnowledge(kind, target.Slug, census, shard)
}

// censusTopLevelFields are the top-level blocks fields the census saw rows
// in, sorted.
func censusTopLevelFields(k *discovery.BlockKnowledge) []string {
	if k == nil || k.Entity == nil {
		return nil
	}
	var out []string
	for _, p := range k.Entity.PathPatterns() {
		cp := k.Entity.Paths[p]
		if cp == nil || cp.Parent != "" || strings.Contains(p, "[]") || (cp.Rows == 0 && len(cp.Types) == 0) {
			continue
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// censusRowArrays is rowArrayKnowledge from the cached census knowledge.
func censusRowArrays(k *discovery.BlockKnowledge) rowArrayKnowledge {
	if k == nil {
		return nil
	}
	return func(blockType, rel string) string {
		kind := censusFieldKind(k, blockType, rel)
		if kind == discovery.CensusKindBlocks || kind == discovery.CensusKindArray {
			return fmt.Sprintf("the block census saw it on other %s rows (%s)", blockType, kind)
		}
		return ""
	}
}

// censusFieldKind is the census kind (blocks, array, scalar, …) of rel — a
// dotted key path inside a row — on rows of blockType, "" when the census
// never saw it.
func censusFieldKind(k *discovery.BlockKnowledge, blockType, rel string) string {
	if k == nil || blockType == "" || rel == "" {
		return ""
	}
	stats := k.TypeStats(blockType)
	if stats == nil {
		return ""
	}
	fields := stats.Fields
	parts := strings.Split(rel, ".")
	for i, key := range parts {
		f := fields[key]
		if f == nil {
			return ""
		}
		if i == len(parts)-1 {
			return f.Kind
		}
		if f.Kind != discovery.CensusKindGroup {
			return ""
		}
		fields = f.Fields
	}
	return ""
}

// rowArrayKind says whether the array rf addresses is a BLOCKS field (every
// row carries a blockType) or a plain ARRAY field (links, buttons: rows carry
// none), and what says so. kind is discovery.TypeBlocks, discovery.TypeArray
// or "" when nothing knows. The evidence, strongest first: a declared
// accepted set, the collection or block schema, the cached block census, the
// array's own rows, and other rows of the same blockType in the document.
func rowArrayKind(doc map[string]any, rf *rowField) (kind, source string) {
	if rf == nil {
		return "", ""
	}
	if len(rf.Accepts) > 0 {
		return discovery.TypeBlocks, "its accepted blockTypes are declared"
	}
	parent, rel, nested := rf.Loc.Parent()
	bt := ""
	if nested {
		bt = parent.Last().BlockType
	}
	if bt != "" {
		if bs, ok := rf.Shard.BlockSchemaFor(bt); ok {
			for _, f := range bs.Fields {
				if f.Path == rel && (f.PayloadType == discovery.TypeBlocks || f.PayloadType == discovery.TypeArray) {
					return f.PayloadType, fmt.Sprintf("the %s block's schema declares it", bt)
				}
			}
		}
	} else if rf.Path != "" {
		dotted := strings.ReplaceAll(rf.Loc.Pattern(), "[]", "")
		if f, ok := rf.Shard.Field(dotted); ok && (f.PayloadType == discovery.TypeBlocks || f.PayloadType == discovery.TypeArray) {
			return f.PayloadType, "the collection schema declares it"
		}
	}
	if bt != "" {
		switch censusFieldKind(rf.Census, bt, rel) {
		case discovery.CensusKindBlocks:
			return discovery.TypeBlocks, fmt.Sprintf("the block census saw it on %s rows", bt)
		case discovery.CensusKindArray:
			return discovery.TypeArray, fmt.Sprintf("the block census saw it on %s rows", bt)
		}
	}
	switch rows.ArrayRowKind(rf.Rows) {
	case rows.RowKindBlocks:
		return discovery.TypeBlocks, "its rows carry a blockType"
	case rows.RowKindArray:
		return discovery.TypeArray, "its rows carry no blockType"
	}
	if bt != "" {
		switch rows.ObservedRowKind(doc, bt, rel) {
		case rows.RowKindBlocks:
			return discovery.TypeBlocks, fmt.Sprintf("the %s of other %s rows in this document carry a blockType", rel, bt)
		case rows.RowKindArray:
			return discovery.TypeArray, fmt.Sprintf("the %s of other %s rows in this document carry no blockType", rel, bt)
		}
	}
	return "", ""
}

// rowLabel renders "layout[2] (group)" for a row location.
func rowLabel(row rows.Location) string {
	if bt := row.Last().BlockType; bt != "" {
		return fmt.Sprintf("%s (%s)", row.String(), bt)
	}
	return row.String()
}

// missingParentRow is a path whose parent row cannot exist because the array
// it would be in is absent or null. It is the not-found class, like any other
// selector that addresses no row.
func missingParentRow(pe *rows.PathError) error {
	state := "absent"
	if pe.Null {
		state = "null"
	}
	next := ""
	if len(pe.Rest) > 0 {
		next = pe.Rest[0].String()
	}
	e := apierr.New(apierr.CodeSelectorNoMatch,
		"%s is %s in the piped document, so it has no row %s (path %q)", pe.At.String(), state, next, pe.Path)
	if pe.InRow {
		return e.WithHint("`pay blocks ls --recursive` lists every nested array and the --field that addresses it")
	}
	return e.WithHint("a --select on the read drops the fields it does not name; re-read the whole document with --depth 0")
}

// unknownRowField is a key a block row does not carry and nothing says it
// should. It names the row and the arrays it DOES carry.
func unknownRowField(pe *rows.PathError) error {
	parent, rel, _ := pe.At.Parent()
	label := rowLabel(parent)
	key := rel
	if i := strings.LastIndexByte(rel, '.'); i >= 0 {
		key = rel[i+1:]
	}
	var arrays []string
	if obj, ok := pe.Container.(map[string]any); ok {
		for _, k := range sortedMapKeys(obj) {
			if _, isList := obj[k].([]any); isList {
				arrays = append(arrays, k)
			}
		}
	}
	base := pe.At.Hops[:len(pe.At.Hops)-1]
	stable := rows.Location{Hops: base}.Stable()
	var suggestions []string
	for _, k := range apierr.DidYouMean(key, arrays) {
		suggestions = append(suggestions, stable+"."+k)
	}
	e := apierr.New(apierr.CodeUnknownField,
		"%s has no field %q (path %q), and neither this block's schema nor any other %s row in the document has one",
		label, rel, pe.Path, orUntyped(parent.Last().BlockType)).
		WithDidYouMean(suggestions...)
	if len(arrays) == 0 {
		return e.WithHint("this row carries no arrays at all; `pay blocks ls --recursive` lists every nested array and the --field that addresses it")
	}
	full := make([]string, len(arrays))
	for i, k := range arrays {
		full[i] = stable + "." + k
	}
	return e.WithHint("the arrays this row carries are: %s", strings.Join(full, ", "))
}

func orUntyped(s string) string {
	if s == "" {
		return "untyped"
	}
	return s
}

// pathError maps every other resolution failure to invalid_args, with the
// suggestion that fixes the most common one: `layout.blocks`, a key applied
// to an array whose ROW was never named.
func pathError(pe *rows.PathError) error {
	e := apierr.New(apierr.CodeInvalidArgs, "--field: %s", pe.Error())
	switch pe.Kind {
	case rows.PathKeyOnArray:
		list, _ := pe.Container.([]any)
		key := ""
		if len(pe.Rest) > 0 {
			key = pe.Rest[0].Text
		}
		var fixes []string
		for _, s := range rows.Summarize(list) {
			obj, _ := list[s.Index].(map[string]any)
			if _, ok := obj[key]; ok {
				fixes = append(fixes, fmt.Sprintf("%s[%s].%s", pe.At.Stable(), s.Selector, joinSteps(pe.Rest)))
			}
		}
		e = e.WithDidYouMean(fixes...)
		return e.WithHint("%s is an array: name a row before the key, e.g. %s[0].%s", pe.At.String(), pe.At.String(), key)
	case rows.PathNotArray:
		fixes := make([]string, 0, len(pe.Have))
		for _, k := range pe.Have {
			fixes = append(fixes, pe.At.Stable()+"."+k)
		}
		e = e.WithDidYouMean(fixes...)
		if len(fixes) == 0 {
			return e.WithHint("--field names the ARRAY whose rows are edited; to change fields of one row use `pay blocks set <selector> --field <its array>`")
		}
		return e.WithHint("--field names the ARRAY whose rows are edited; the arrays inside it are listed in did_you_mean")
	}
	return e.WithHint("the grammar is: %s; `pay blocks ls --recursive` prints a ready-made --field for every nested array", rows.PathGrammar)
}

func joinSteps(steps []rows.Step) string {
	var b strings.Builder
	for i, s := range steps {
		if !s.Bracket && i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(s.String())
	}
	return b.String()
}

// setPathErr gives a *rows.PathError raised INSIDE a row by `pay blocks set`
// the row's own location, so the message names `layout[2].blocks[0].links`
// rather than a bare `links`.
func setPathErr(err error, rf *rowField, idx int) error {
	row := rf.Loc.With(rows.Hop{Row: true, Index: idx})
	var miss *rows.NoMatchError
	if errors.As(err, &miss) {
		miss.Field = joinLoc(row, miss.Field)
		return resolveErr(miss, rf)
	}
	var amb *rows.AmbiguousError
	if errors.As(err, &amb) {
		amb.Field = joinLoc(row, amb.Field)
		return resolveErr(amb, rf)
	}
	var pe *rows.PathError
	if errors.As(err, &pe) {
		where := joinLoc(row, pe.At.String())
		if pe.Kind == rows.PathAbsent {
			return apierr.New(apierr.CodeSelectorNoMatch,
				"%s is absent or null, so the --set/--unset key %q cannot address a row inside it", where, pe.Path).
				WithHint("set the whole array instead: --set-json '%s=[…]'", pe.At.String())
		}
		return apierr.New(apierr.CodeInvalidArgs, "%s: %s", where, pe.Detail).
			WithHint("a --set key is a path INSIDE the row: %s", rows.PathGrammar)
	}
	return resolveErr(err, rf)
}

func joinLoc(row rows.Location, rel string) string {
	base := row.String()
	switch {
	case base == "":
		return rel
	case rel == "":
		return base
	case strings.HasPrefix(rel, "["):
		return base + rel
	default:
		return base + "." + rel
	}
}

// fieldAccepts is what `pay blocks ls` reports about the blockTypes the
// listed field takes, and where that list came from.
type fieldAccepts struct {
	Slugs []string
	// Source is the list's provenance: configured, project-source, graphql,
	// mixed, or observed-census (what real documents contain).
	Source        string
	Authoritative bool
	// Pattern is the key the list was found under: `layout`,
	// `layout[].blocks`, or a parent-scoped `group.blocks`.
	Pattern string
}

// acceptsFor answers "what may go here" for a resolved field.
//
// A DECLARED complete list (rf.Accepts, the one that gates `add`) wins. Without
// one, the cached block census answers — for a nested field by the owning
// row's type first (`group.blocks`: what real group rows contain), then by
// the position pattern (`layout[].blocks`). A census answer is labelled with
// its source and never gates an edit (§9.11.4). nil means nothing is known,
// which is not "accepts nothing".
func acceptsFor(rf *rowField) *fieldAccepts {
	if rf == nil || rf.Path == "" {
		return nil
	}
	pattern := rf.Loc.Pattern()
	if rf.Accepts != nil {
		src := discovery.SourceUnknown
		if bf, ok := discovery.DeclaredBlockField(rf.Shard, pattern); ok && bf.Source != "" {
			src = bf.Source
		}
		return &fieldAccepts{Slugs: rf.Accepts, Source: src, Authoritative: true, Pattern: pattern}
	}
	k := rf.Census
	if k == nil {
		return nil
	}
	var a *discovery.AcceptedSet
	if parent, rel, ok := rf.Loc.Parent(); ok && rel != "" && !strings.Contains(rel, ".") {
		if bt := parent.Last().BlockType; bt != "" {
			if ps := k.Accepted(bt + "." + rel); ps.Parent != "" && len(ps.Types) > 0 {
				a = ps
			}
		}
	}
	if a == nil {
		a = k.Accepted(pattern)
	}
	if len(a.Types) == 0 {
		return nil
	}
	return &fieldAccepts{Slugs: a.Slugs(), Source: a.Source, Authoritative: a.Authoritative, Pattern: a.Path}
}

// observedPlacementWarning is the census's say on a blockType an edit puts
// into rf: a WARNING (never a refusal — "A census list never gates an
// edit") when the cached census never saw that type at this position, or
// anywhere in the project. It answers from the same set `pay blocks ls`
// reports (acceptsFor), so a typo (bentoItme) or a misplaced type (bentoItem
// in a group) is reported by the stage that made it, not three stages later
// by `pay apply`. A declared complete list is checkBlockType's job; nothing
// is said when no census is cached or it saw no blocks.
func observedPlacementWarning(rf *rowField, target *output.Target, slug string) *output.Warning {
	if rf == nil || rf.Census == nil || len(rf.Accepts) > 0 || !rf.Census.HasObservedBlocks() {
		return nil
	}
	acc := acceptsFor(rf)
	if acc == nil || acc.Authoritative {
		return nil
	}
	for _, s := range acc.Slugs {
		if s == slug {
			return nil
		}
	}
	hint := "pay blocks ls --field " + rf.Path
	if target != nil && target.Slug != "" {
		hint = fmt.Sprintf("pay blocks types %s --field %s", target.Slug, acc.Pattern)
	}
	project := rf.Census.ProjectTypes()
	if !project[slug] {
		pool := make([]string, 0, len(project))
		for s := range project {
			pool = append(pool, s)
		}
		sortStrings(pool)
		w := output.Warning{Code: discovery.IssueUnknownBlockType, Paths: []string{rf.Path},
			Message: fmt.Sprintf("blockType %q was never observed anywhere in this project (cached block census) and no declared "+
				"source names it. Payload silently DROPS a row whose blockType it does not recognise and still answers 2xx", slug),
			Hint: hint}
		if near := discovery.ClosestSlug(slug, pool); near != "" {
			w.Hint = fmt.Sprintf("did you mean %q? %s lists what is used there", near, hint)
		}
		return &w
	}
	return &output.Warning{Code: discovery.IssueBlockTypeNotObserved, Paths: []string{rf.Path},
		Message: fmt.Sprintf("blockType %q exists in this project but was never observed at %s (observed there: %s); "+
			"nothing declares whether this field accepts it", slug, acc.Pattern, strings.Join(acc.Slugs, ", ")),
		Hint: hint}
}

// describeNestedArrays fills each recursive-listing node's Arrays: for every
// array inside the row, whether it is a blocks or a plain array field and
// what it accepts — the same answer `pay blocks ls --field <that array>`
// gives, so the listing alone says where `add <blockType>` and where
// `add -` belongs.
func describeNestedArrays(doc map[string]any, rf *rowField, nodes []rows.Node) {
	if rf == nil || rf.Path == "" {
		// Bare rows: there is no document to resolve a location against.
		return
	}
	for i := range nodes {
		n := &nodes[i]
		for rel := range n.Nested {
			loc := n.At
			for _, k := range strings.Split(rel, ".") {
				loc = loc.With(rows.Hop{Key: k})
			}
			sub := newRowField(loc, rf.Shard)
			sub.Census = rf.Census
			if v, ok := rows.Lookup(doc, loc); ok {
				sub.Rows, _ = v.([]any)
			}
			var info rows.ArrayInfo
			info.Kind, _ = rowArrayKind(doc, sub)
			if acc := acceptsFor(sub); acc != nil {
				auth := acc.Authoritative
				info.Accepts, info.AcceptsSource, info.AcceptsAuthoritative = acc.Slugs, acc.Source, &auth
				if info.Kind == "" {
					info.Kind = discovery.TypeBlocks
				}
			}
			if info.Kind == "" {
				continue
			}
			if n.Arrays == nil {
				n.Arrays = map[string]rows.ArrayInfo{}
			}
			n.Arrays[rel] = info
		}
	}
}

// staleIndexWarning is stale_index: field enters a row by position (N, first,
// last, -N, type:SLUG[N]) of an array that an earlier stage of this pipe
// moved, removed, added or copied rows in. The position is resolved against
// the renumbered array, so it may name a different row than the listing the
// caller read it from — `pay blocks rm 2 | pay blocks set … --field
// layout.3.blocks` edits what WAS layout[4]. nil when nothing upstream touched
// such an array.
func staleIndexWarning(field string, rf *rowField, edits *output.Edits) *output.Warning {
	if edits == nil || len(edits.Ops) == 0 || rf == nil || !rf.Nested {
		return nil
	}
	reshaped := map[string]bool{}
	for _, op := range edits.Ops {
		switch op.Command {
		case safety.CmdBlocksMove, safety.CmdBlocksRemove, safety.CmdBlocksAdd, safety.CmdBlocksCopy:
			reshaped[op.Field] = true
		}
	}
	if len(reshaped) == 0 {
		return nil
	}
	p, err := rows.ParsePath(field)
	if err != nil || len(p.Steps) != len(rf.Loc.Hops) {
		return nil
	}
	var stale []string
	for i, h := range rf.Loc.Hops {
		if !h.Row {
			continue
		}
		sel, err := rows.ParseSelector(p.Steps[i].Text)
		if err != nil {
			continue
		}
		positional := sel.Kind == rows.KindIndex || (sel.Kind == rows.KindType && sel.Nth != nil)
		array := rows.Location{Hops: rf.Loc.Hops[:i]}.String()
		if positional && reshaped[array] {
			stale = append(stale, fmt.Sprintf("%s[%s]", array, p.Steps[i].Text))
		}
	}
	if len(stale) == 0 {
		return nil
	}
	return &output.Warning{
		Code: WarnStaleIndex,
		Message: fmt.Sprintf("--field %s enters %s by position, and an earlier stage of this pipe moved, added or removed rows there; "+
			"the position was resolved against the renumbered array and now names %s", field, strings.Join(stale, ", "), rf.Stable),
		Paths: []string{rf.Path},
		Hint: "address rows on the way by id so the path survives earlier stages: take the field from " +
			"`pay blocks ls --recursive` on the document BEFORE the pipe's edits (it prints id-based fields)",
	}
}
