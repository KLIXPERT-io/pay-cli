package rows

// DocTree lists every row of every row array in a whole document, at every
// depth, in document order (a row, then the rows nested in it) — what
// `pay outline` prints. Tree lists one array; DocTree finds the arrays itself:
// every top-level field (and every group field, through nested objects) that
// holds an array of objects, walked the way Tree walks a row's nested arrays
// (never into rich text or an expanded relationship).
//
// Arrays whose rows carry a blockType (blocks fields: `layout`) come first, in
// key order, then the other row arrays (`breadcrumbs`, `hero.links`), so a
// page's outline starts with its content.
func DocTree(doc map[string]any) []Node {
	arrays := nestedArrays(doc, "", 0)
	var blocks, plain []nestedArray
	for _, na := range arrays {
		if hasBlockRow(na.list) {
			blocks = append(blocks, na)
		} else {
			plain = append(plain, na)
		}
	}
	var out []Node
	for _, na := range append(blocks, plain...) {
		walkTree(na.list, KeyLocation(na.rel), 0, &out)
	}
	return out
}

// hasBlockRow reports an array with at least one row carrying a blockType.
// One is enough: a blocks field that also holds a `{}` orphan row is still a
// blocks field, and that orphan is exactly what an outline must show.
func hasBlockRow(list []any) bool {
	for _, row := range list {
		if obj, ok := row.(map[string]any); ok && scalarString(obj[KeyBlockType]) != "" {
			return true
		}
	}
	return false
}

// HasBlockRow is hasBlockRow for callers outside the package.
func HasBlockRow(list []any) bool { return hasBlockRow(list) }
