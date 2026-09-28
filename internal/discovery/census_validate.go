package discovery

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Block-row validation (§7.10c): every block row at every depth of a document
// or write body, checked against the merged block knowledge.
//
// The severity rule is the whole point. PayCLI's old check warned about every
// blockType it could not confirm — on a project with introspection disabled
// that was every blockType on every write, so agents grepped the warnings away
// and the real ones drowned. Here an ERROR needs a DECLARED fact (a profile
// pin, the project source, a GraphQL union or enum, a NON_NULL); a fact the
// census merely observed can only ever produce a WARNING, and a fact nobody
// has produces nothing at all.

// Issue severities.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
)

// Issue codes. They double as write-path warning codes.
const (
	IssueEmptyRow             = "empty_row"
	IssueMissingBlockType     = "missing_block_type"
	IssueInvalidRow           = "invalid_row"
	IssueUnknownBlockType     = "unknown_block_type"
	IssueBlockTypeNotObserved = "block_type_not_observed_here"
	IssueUnknownField         = "unknown_field"
	IssueValueNotInEnum       = "value_not_in_enum"
	IssueLikelyRequired       = "likely_required"
	IssueRequiredMissing      = "required_field_missing"
)

// BlockIssue is one finding.
type BlockIssue struct {
	Severity  string `json:"severity"`
	Code      string `json:"code"`
	Path      string `json:"path"`
	BlockType string `json:"block_type,omitempty"`
	Message   string `json:"message"`
	Hint      string `json:"hint,omitempty"`
	// Source names the fact that produced the issue: a declared source
	// (configured, project-source, graphql) or observed-census.
	Source string `json:"source"`
}

// BlockValidation is the result of one validation pass.
type BlockValidation struct {
	CheckedRows int          `json:"checked_rows"`
	Errors      int          `json:"errors"`
	Warnings    int          `json:"warnings"`
	Issues      []BlockIssue `json:"issues"`
}

// ValidateBlocks checks every block row in doc. k may carry no facts at all;
// the structural checks (`{}` rows, rows without a blockType) still run.
func ValidateBlocks(doc map[string]any, k *BlockKnowledge) BlockValidation {
	v := &validator{k: k, res: BlockValidation{Issues: []BlockIssue{}}}
	if k != nil {
		v.projectTypes = k.ProjectTypes()
	}
	for _, key := range sortedAnyKeys(doc) {
		if key == "id" {
			continue
		}
		v.walk(doc[key], key, key, 0)
	}
	for _, is := range v.res.Issues {
		if is.Severity == SeverityError {
			v.res.Errors++
		} else {
			v.res.Warnings++
		}
	}
	return v.res
}

// TouchesBlocks reports whether a document or write body holds anything the
// block check must look at: a block row anywhere (ContainsBlockRows), or a
// non-empty array at a position k knows is a blocks field — a census path
// with rows, or a top-level field the schema declares as blocks. The second
// half is what catches `layout=[{}]` and `layout=[{"heading":"x"}]`: no row
// carries a blockType, so Payload drops them all with a 2xx and the field
// becomes [], wiping the layout.
func TouchesBlocks(doc map[string]any, k *BlockKnowledge) bool {
	if ContainsBlockRows(doc) {
		return true
	}
	v := &validator{k: k}
	for _, key := range sortedAnyKeys(doc) {
		if key != "id" && v.touches(doc[key], key, 0) {
			return true
		}
	}
	return false
}

func (v *validator) touches(val any, pattern string, depth int) bool {
	if depth > maxCensusDepth {
		return false
	}
	switch t := val.(type) {
	case []any:
		if len(t) > 0 && v.isBlocksPattern(pattern) {
			return true
		}
		for _, item := range t {
			if row, ok := item.(map[string]any); ok {
				for _, key := range sortedAnyKeys(row) {
					if key != "id" && v.touches(row[key], pattern+"[]."+key, depth+1) {
						return true
					}
				}
			}
		}
	case map[string]any:
		if isLexicalRoot(t) || isPolymorphicRef(t) {
			return false
		}
		for _, key := range sortedAnyKeys(t) {
			if v.touches(t[key], pattern+"."+key, depth+1) {
				return true
			}
		}
	}
	return false
}

// ContainsArray reports whether a value holds a non-empty array anywhere
// outside rich text: the cheap test for "could this body touch a blocks
// field at all", made before any block knowledge is loaded.
func ContainsArray(v any) bool {
	return containsArray(v, 0)
}

func containsArray(v any, depth int) bool {
	if depth > maxCensusDepth {
		return false
	}
	switch t := v.(type) {
	case []any:
		return len(t) > 0
	case map[string]any:
		if isLexicalRoot(t) {
			return false
		}
		for _, sub := range t {
			if containsArray(sub, depth+1) {
				return true
			}
		}
	}
	return false
}

// ValidateRowsAt checks bare block rows as if they sat in the blocks field at
// field: a top-level field (`layout`), a nested position in any grammar
// NormalizeBlockPath accepts (`layout[].blocks`, `layout.2.blocks`,
// `layout[type:group[1]].blocks`), or a parent-scoped field (`group.blocks`).
// A `type:` selector on the last parent names the parent row's blockType, so
// what that type's field accepts (wherever such a row sits) is the observed
// set, exactly as for a row nested in a document. Issue paths start at the
// normalised pattern (`layout[].blocks[0]`).
func ValidateRowsAt(rows []any, field string, k *BlockKnowledge) BlockValidation {
	v := &validator{k: k, res: BlockValidation{Issues: []BlockIssue{}}}
	if k != nil {
		v.projectTypes = k.ProjectTypes()
	}
	pattern := NormalizeBlockPath(field)
	parentType, fieldKey := selectorParentType(field), ""
	if i := strings.LastIndex(pattern, "."); i >= 0 {
		fieldKey = pattern[i+1:]
	}
	if parentType == "" {
		fieldKey = ""
	}
	v.rows(rows, pattern, pattern, parentType, fieldKey, strings.Count(pattern, "[]"))
	for _, is := range v.res.Issues {
		if is.Severity == SeverityError {
			v.res.Errors++
		} else {
			v.res.Warnings++
		}
	}
	return v.res
}

// selectorTypeRe finds the blockType of a `[type:group]` or `[type:group[1]]`
// selector.
var selectorTypeRe = regexp.MustCompile(`\[type:([^\[\]]+)(?:\[[^\]]*\])?\][^\[]*$`)

// selectorParentType returns the blockType a path's LAST selector names, if
// that selector is a `type:` one.
func selectorParentType(field string) string {
	m := selectorTypeRe.FindStringSubmatch(field)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

type validator struct {
	k            *BlockKnowledge
	res          BlockValidation
	projectTypes map[string]bool
}

func (v *validator) add(is BlockIssue) { v.res.Issues = append(v.res.Issues, is) }

// walk looks for blocks arrays anywhere in a value.
func (v *validator) walk(val any, pattern, concrete string, depth int) {
	if depth > maxCensusDepth {
		return
	}
	switch t := val.(type) {
	case []any:
		if isBlocksArray(t) || v.isBlocksPattern(pattern) {
			v.rows(t, pattern, concrete, "", "", depth)
			return
		}
		for i, item := range t {
			row, ok := item.(map[string]any)
			if !ok {
				continue
			}
			for _, key := range sortedAnyKeys(row) {
				if key == "id" {
					continue
				}
				v.walk(row[key], pattern+"[]."+key, fmt.Sprintf("%s[%d].%s", concrete, i, key), depth+1)
			}
		}
	case map[string]any:
		if isLexicalRoot(t) || isPolymorphicRef(t) {
			return
		}
		for _, key := range sortedAnyKeys(t) {
			v.walk(t[key], pattern+"."+key, concrete+"."+key, depth+1)
		}
	}
}

// isBlocksPattern reports a path the knowledge says is a blocks field, which
// is what lets an array of `{}` rows be recognised as orphaned blocks rather
// than as an unremarkable empty array of objects.
func (v *validator) isBlocksPattern(pattern string) bool {
	if v.k == nil {
		return false
	}
	if v.k.Entity != nil {
		if cp := v.k.Entity.Paths[pattern]; cp != nil && (cp.Rows > 0 || cp.EmptyRows > 0) {
			return true
		}
	}
	if v.k.Shard != nil && !strings.Contains(pattern, "[]") {
		if f, ok := v.k.Shard.Field(pattern); ok && f.PayloadType == TypeBlocks {
			return true
		}
	}
	return false
}

// rows validates one blocks array.
func (v *validator) rows(items []any, pattern, concrete, parentType, fieldKey string, depth int) {
	for i, item := range items {
		path := fmt.Sprintf("%s[%d]", concrete, i)
		row, ok := item.(map[string]any)
		if !ok {
			v.add(BlockIssue{Severity: SeverityError, Code: IssueInvalidRow, Path: path, Source: "payload-protocol",
				Message: fmt.Sprintf("%s is %s; every entry of a blocks field must be an object with a blockType", path, describeJSON(item))})
			continue
		}
		if len(row) == 0 {
			v.add(BlockIssue{Severity: SeverityError, Code: IssueEmptyRow, Path: path, Source: "payload-protocol",
				Message: fmt.Sprintf("%s is an empty object {}. On a read it is an orphan row the database adapter left behind "+
					"after a nested block restructure; in a write it is a row with no blockType, which Payload cannot store", path),
				Hint: "remove it (`pay blocks rm`) before writing the document back"})
			continue
		}
		bt, _ := row["blockType"].(string)
		if bt == "" {
			v.add(BlockIssue{Severity: SeverityError, Code: IssueMissingBlockType, Path: path, Source: "payload-protocol",
				Message: path + " has no blockType; Payload identifies a block row only by its blockType slug",
				Hint:    v.typesHint(pattern)})
			continue
		}
		v.res.CheckedRows++
		v.checkAccepted(bt, pattern, path, parentType, fieldKey)
		v.checkRow(bt, row, pattern, path, depth)
	}
}

func (v *validator) typesHint(pattern string) string {
	if v.k == nil || v.k.Slug == "" {
		return ""
	}
	return fmt.Sprintf("pay blocks types %s --field %s lists the blockTypes used there", v.k.Slug, pattern)
}

// checkAccepted decides whether bt may go at pattern.
func (v *validator) checkAccepted(bt, pattern, path, parentType, fieldKey string) {
	k := v.k
	if k == nil {
		return
	}
	a := k.Accepted(pattern)
	if !a.Authoritative && parentType != "" && fieldKey != "" {
		// `group.blocks` answers for every group row, wherever it sits, and
		// it answers INSTEAD of the depth pattern: `layout[].blocks` is the
		// union of every parent's field at that depth (a group's blocks and a
		// bento's items), so it would admit a bentoItem into a group. Only
		// when no row of the parent type was ever observed with this field
		// does the depth pattern speak (the same order `pay blocks ls` uses).
		if ps := k.Accepted(parentType + "." + fieldKey); ps.Parent != "" && len(ps.Types) > 0 {
			a = ps
			pattern = ps.Path
		}
	}
	if a.Has(bt) {
		return
	}
	slugs := a.Slugs()
	if a.Authoritative {
		v.add(BlockIssue{Severity: SeverityError, Code: IssueUnknownBlockType, Path: path, BlockType: bt, Source: a.Source,
			Message: fmt.Sprintf("%s: blockType %q is not accepted by %s (%s: %s). Payload silently DROPS a row whose "+
				"blockType it does not recognise and still answers 2xx", path, bt, pattern, a.Source, strings.Join(slugs, ", ")),
			Hint: suggestHint(bt, slugs, fmt.Sprintf("pay blocks types %s --field %s", k.Slug, pattern))})
		return
	}
	if !k.HasObservedBlocks() {
		// This entity was never censused, or its census saw no block row at
		// all (a read that stopped on a page of empty stubs): "never observed
		// here" and "never observed anywhere" would be guesses. Say nothing;
		// the caller reports that the check could not run.
		return
	}
	if v.projectTypes[bt] {
		v.add(BlockIssue{Severity: SeverityWarning, Code: IssueBlockTypeNotObserved, Path: path, BlockType: bt, Source: SourceObservedCensus,
			Message: fmt.Sprintf("%s: blockType %q exists in this project but was never observed at %s (observed there: %s); "+
				"nothing declares whether this field accepts it", path, bt, pattern, orNone(slugs)),
			Hint: fmt.Sprintf("pay blocks types %s --field %s", k.Slug, pattern)})
		return
	}
	v.add(BlockIssue{Severity: SeverityWarning, Code: IssueUnknownBlockType, Path: path, BlockType: bt, Source: SourceObservedCensus,
		Message: fmt.Sprintf("%s: blockType %q was never observed anywhere in this project and no declared source names it. "+
			"Payload silently DROPS a row whose blockType it does not recognise and still answers 2xx", path, bt),
		Hint: suggestHint(bt, sortedBoolKeys(v.projectTypes), "pay blocks types "+k.Slug)})
}

// checkRow validates one row's keys and values, and recurses into its nested
// blocks and arrays.
func (v *validator) checkRow(bt string, row map[string]any, pattern, path string, depth int) {
	var stats *CensusType
	var gql *BlockTypeSchema
	if v.k != nil {
		stats = v.k.TypeStats(bt)
		if stats != nil && !v.k.ownType(bt) && !rowFitsShape(row, stats) {
			// The statistics come from ANOTHER entity's rows of this slug,
			// and this row does not look like them: a slug is not globally
			// unique (forms' `text` field block vs a page's `text` block).
			// Comparing the row with a foreign shape would call its real
			// fields unknown and demand the foreign ones.
			stats = nil
		}
		if s, ok := v.k.GraphQLSchema(bt); ok {
			gql = &s
		}
	}
	declared := map[string]BlockFieldSchema{}
	if gql != nil {
		for _, f := range gql.Fields {
			declared[f.Path] = f
		}
	}
	var fields map[string]*CensusField
	if stats != nil {
		fields = stats.Fields
	}
	v.checkObject(bt, row, fields, declared, "", pattern+"[]", path, depth, true, statsCount(stats))

	// Missing fields: declared-required is an error, observed-always-filled a
	// warning.
	for _, name := range sortedDeclared(declared) {
		d := declared[name]
		if d.Parent != nil || d.Plumbing || d.Required == nil || !*d.Required {
			continue
		}
		if val, ok := row[name]; !ok || val == nil {
			v.add(BlockIssue{Severity: SeverityError, Code: IssueRequiredMissing, Path: path + "." + name, BlockType: bt,
				Source:  d.RequiredSource,
				Message: fmt.Sprintf("%s.%s is required on %s (%s) and is missing or null", path, name, bt, d.RequiredSource),
				Hint:    "pay blocks schema " + bt})
		}
	}
	if stats == nil {
		return
	}
	for _, name := range sortedFieldNames(fields) {
		f := fields[name]
		if d, ok := declared[name]; ok && d.Required != nil {
			continue
		}
		if name == "blockName" || !likelyRequired(f, stats.Count) {
			continue
		}
		if val, ok := row[name]; !ok || !nonEmptyValue(val) {
			v.add(BlockIssue{Severity: SeverityWarning, Code: IssueLikelyRequired, Path: path + "." + name, BlockType: bt,
				Source: SourceObservedCensus,
				Message: fmt.Sprintf("%s.%s is empty, but every one of the %d observed %s rows fills it in",
					path, name, stats.Count, bt),
				Hint: "pay blocks example " + bt})
		}
	}
}

// rowFitsShape reports whether a row's own keys are mostly keys the observed
// type carries: at least 80% of them, the threshold CompatibleTypes uses
// between two observations. A row with no keys besides blockType shows no
// shape at all and fits nothing.
func rowFitsShape(row map[string]any, t *CensusType) bool {
	keys, known := 0, 0
	for k := range row {
		if k == "id" || k == "blockType" || k == "blockName" {
			continue
		}
		keys++
		if _, ok := t.Fields[k]; ok {
			known++
		}
	}
	return keys > 0 && known*5 >= keys*4
}

func statsCount(t *CensusType) int {
	if t == nil {
		return 0
	}
	return t.Count
}

// checkObject validates the keys of a row, a group or an array entry.
// observedBase is how many times the enclosing object was observed; an unknown
// key is only reported when there is at least one observation to compare to.
func (v *validator) checkObject(bt string, obj map[string]any, fields map[string]*CensusField,
	declared map[string]BlockFieldSchema, joinPrefix, pattern, path string, depth int, isBlock bool, observedBase int) {
	if depth > maxCensusDepth {
		return
	}
	for _, key := range sortedAnyKeys(obj) {
		if key == "id" || (isBlock && (key == "blockType" || key == "blockName")) {
			continue
		}
		join := key
		if joinPrefix != "" {
			join = joinPrefix + "." + key
		}
		val := obj[key]
		fpath := path + "." + key
		var f *CensusField
		if fields != nil {
			f = fields[key]
		}
		d, isDeclared := declared[join]
		if f == nil && !isDeclared {
			// A block row is compared with every observed row and the declared
			// schema; a nested object only with observed siblings, because the
			// declared walk stops three levels down (§7.10a).
			report := observedBase > 0 || len(declared) > 0
			if !isBlock {
				report = observedBase > 0 && fields != nil
			}
			if report {
				src := SourceObservedCensus
				if len(declared) > 0 {
					src = SourceGraphQL
				}
				v.add(BlockIssue{Severity: SeverityWarning, Code: IssueUnknownField, Path: fpath, BlockType: bt, Source: src,
					Message: fmt.Sprintf("%s: %q is not a field of %s — never observed on %d real row(s) and absent from every "+
						"declared schema. Payload silently drops unknown fields", fpath, key, bt, observedBase),
					Hint: "pay blocks schema " + bt})
			}
			// Still look inside: a mistyped wrapper can hold real blocks.
			v.walkNested(bt, val, pattern+"."+key, fpath, key, nil, declared, join, depth)
			continue
		}
		v.checkValue(bt, key, val, f, d, isDeclared, fpath)
		v.walkNested(bt, val, pattern+"."+key, fpath, key, f, declared, join, depth)
	}
}

// walkNested recurses into a field's value.
func (v *validator) walkNested(bt string, val any, pattern, path, key string, f *CensusField,
	declared map[string]BlockFieldSchema, join string, depth int) {
	switch t := val.(type) {
	case []any:
		if nestedIsBlocks(t, f, declared, join) {
			v.rows(t, pattern, path, bt, key, depth+1)
			return
		}
		if !allObjects(t) {
			return
		}
		var sub map[string]*CensusField
		base := 0
		if f != nil {
			sub, base = f.Fields, f.Items
		}
		for i, item := range t {
			if obj, ok := item.(map[string]any); ok {
				v.checkObject(bt, obj, sub, declared, join, pattern+"[]",
					fmt.Sprintf("%s[%d]", path, i), depth+1, false, base)
			}
		}
	case map[string]any:
		if isLexicalRoot(t) || isPolymorphicRef(t) {
			return
		}
		if f != nil && f.Kind != CensusKindGroup {
			return
		}
		var sub map[string]*CensusField
		base := 0
		if f != nil {
			sub, base = f.Fields, f.NonNull
		}
		v.checkObject(bt, t, sub, declared, join, pattern, path, depth+1, false, base)
	}
}

// nestedIsBlocks decides whether an array inside a row is a blocks field. What
// the field IS outranks what one of its rows carries: a declared kind first,
// then the census's kind when real rows were observed. Only an unknown field
// is judged by its content. A stray blockType on a row of a plain array
// (hero.buttons) is then reported as the unknown key it is — Payload drops
// the key, not the row — instead of blaming every untouched real row of the
// array for lacking one.
func nestedIsBlocks(items []any, f *CensusField, declared map[string]BlockFieldSchema, join string) bool {
	if d, ok := declared[join]; ok {
		switch d.PayloadType {
		case TypeBlocks:
			return true
		case TypeArray:
			return false
		}
	}
	if f != nil {
		switch {
		case f.Kind == CensusKindBlocks:
			return true
		case f.Kind == CensusKindArray && f.Items > 0:
			return false
		}
	}
	return isBlocksArray(items)
}

// checkValue checks a scalar against a declared enum (error) or an observed
// enum-like vocabulary (warning).
func (v *validator) checkValue(bt, key string, val any, f *CensusField, d BlockFieldSchema, isDeclared bool, path string) {
	vk, ok := valueKey(val)
	if !ok {
		return
	}
	if isDeclared && len(d.Options) > 0 && d.OptionsSource != SourceUnknown {
		for _, o := range d.Options {
			if ok2, _ := valueKey(o); ok2 == vk {
				return
			}
		}
		v.add(BlockIssue{Severity: SeverityError, Code: IssueValueNotInEnum, Path: path, BlockType: bt, Source: d.OptionsSource,
			Message: fmt.Sprintf("%s: %s is not one of %s's declared options (%s)", path, describeJSON(val), key, strings.Join(d.Options, ", ")),
			Hint:    "pay blocks schema " + bt})
		return
	}
	if f == nil || !f.IsVocabulary() || f.Kind != CensusKindScalar {
		return
	}
	var seen []string
	for _, cv := range f.Values {
		ck, _ := valueKey(cv.Value)
		if ck == vk {
			return
		}
		seen = append(seen, fmt.Sprint(cv.Value))
	}
	v.add(BlockIssue{Severity: SeverityWarning, Code: IssueValueNotInEnum, Path: path, BlockType: bt, Source: SourceObservedCensus,
		Message: fmt.Sprintf("%s: %s was never observed for %s.%s, which behaves like a select (observed: %s)",
			path, describeJSON(val), bt, key, strings.Join(seen, ", ")),
		Hint: "pay blocks schema " + bt})
}

func suggestHint(bt string, pool []string, cmd string) string {
	near := closest(bt, pool)
	if near != "" {
		return fmt.Sprintf("did you mean %q? %s lists the accepted values", near, cmd)
	}
	return cmd + " lists the accepted values"
}

// ClosestSlug is the pool entry a mistyped slug most plausibly meant (the
// validator's did-you-mean), or "" when none is close enough.
func ClosestSlug(s string, pool []string) string { return closest(s, pool) }

// closest returns the pool entry with the smallest edit distance to s, when it
// is close enough to be a plausible typo.
func closest(s string, pool []string) string {
	best, bestD := "", 1<<30
	for _, p := range pool {
		d := editDistance(strings.ToLower(s), strings.ToLower(p))
		if d < bestD {
			best, bestD = p, d
		}
	}
	if best == "" || bestD > 2+len(s)/4 {
		return ""
	}
	return best
}

func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = minInt(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

func minInt(vals ...int) int {
	m := vals[0]
	for _, v := range vals[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

func orNone(ss []string) string {
	if len(ss) == 0 {
		return "none"
	}
	return strings.Join(ss, ", ")
}

func describeJSON(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return fmt.Sprintf("%q", t)
	case bool:
		return strconv.FormatBool(t)
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	}
	return fmt.Sprint(v)
}

func sortedDeclared(m map[string]BlockFieldSchema) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedFieldNames(m map[string]*CensusField) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
