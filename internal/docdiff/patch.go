package docdiff

// PATCH semantics, as Payload 3 implements them (verified against
// payload@3.87.1, dist/fields/hooks/beforeValidate/promise.js and
// dist/fields/hooks/beforeChange/getExistingRowDoc.js):
//
//   - A field ABSENT from the body keeps its stored value: beforeValidate
//     fills every undefined field from the stored document (getFallbackValue →
//     cloneDataFromOriginalDoc). This is why a one-key PATCH only changes one
//     key.
//   - A named GROUP recurses with the stored group as its sibling document, so
//     the same rule applies inside it: sub-keys the body omits keep their
//     stored value. Groups merge key by key.
//   - An ARRAY or BLOCKS value replaces the stored array. Rows the body omits
//     are deleted. A body row whose `id` matches a stored row is merged with
//     that row (getExistingRowDoc), so its unsent keys keep their stored
//     values; a row without an id, or with an unknown one, is a new row whose
//     unsent keys take their defaults.
//   - Rows that are not plain objects (null, a scalar, a nested array) are
//     stripped from an array or blocks value (stripNullRows).
//   - RICH TEXT, json, scalars and scalar lists (hasMany) are replaced
//     wholesale.
//
// Two things cannot be modelled without the project's schema, and both are
// stated where the result is presented: a `json` field holding an object is
// merged here like a group unless the caller names it in Options.LeafPaths,
// and a new row's unsent keys are absent here where Payload would fill in the
// field's defaultValue.

// ApplyPatch returns the document Payload would store after a PATCH of
// current with body. Neither argument is modified. The body's own top-level
// `id` is ignored: the id in the URL decides which document is written.
func ApplyPatch(current, body map[string]any, opts Options) map[string]any {
	leaves := compilePatterns(opts.LeafPaths)
	out, _ := clone(current).(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	for k, v := range body {
		if k == "id" {
			continue
		}
		var cur any
		if current != nil {
			cur = current[k]
		}
		out[k] = mergeValue(k, cur, v, leaves)
	}
	return out
}

// DiffPatch is Diff(current, ApplyPatch(current, body)): exactly what a PATCH
// with body would change on current.
func DiffPatch(current, body map[string]any, opts Options) Result {
	return Diff(current, ApplyPatch(current, body, opts), opts)
}

func mergeValue(path string, cur, v any, leaves []pattern) any {
	if matchAny(leaves, path) {
		return clone(v)
	}
	switch nv := v.(type) {
	case map[string]any:
		if isEditorState(nv) {
			return clone(nv)
		}
		// A stored value that is not a mergeable object (absent, null, rich
		// text, a scalar) contributes nothing; the body's object is still
		// walked so nested rows are cleaned the way Payload cleans them.
		cm, ok := cur.(map[string]any)
		if !ok || isEditorState(cm) {
			cm = nil
		}
		out, _ := clone(cm).(map[string]any)
		if out == nil {
			out = make(map[string]any, len(nv))
		}
		for k, val := range nv {
			out[k] = mergeValue(JoinKey(path, k), cm[k], val, leaves)
		}
		return out
	case []any:
		if !hasObjectRow(nv) {
			return clone(nv)
		}
		stored, _ := cur.([]any)
		out := make([]any, 0, len(nv))
		for _, row := range nv {
			ro, ok := row.(map[string]any)
			if !ok {
				continue // stripNullRows
			}
			if existing := storedRow(stored, ro); existing != nil {
				out = append(out, mergeValue(JoinIndex(path, len(out)), existing, ro, leaves))
				continue
			}
			out = append(out, mergeValue(JoinIndex(path, len(out)), nil, ro, leaves))
		}
		return out
	default:
		return v
	}
}

// storedRow is getExistingRowDoc: the stored row with the body row's id. A
// row whose blockType changed is NOT merged — Payload would carry over only
// the keys the new block type also declares, which cannot be known without
// the schema, and keeping the old block's keys would show a row Payload never
// stores.
func storedRow(stored []any, row map[string]any) map[string]any {
	id, ok := rowID(row)
	if !ok {
		return nil
	}
	for _, s := range stored {
		so, ok := s.(map[string]any)
		if !ok {
			continue
		}
		if sid, ok := rowID(so); !ok || sid != id {
			continue
		}
		lt, lok := so["blockType"].(string)
		rt, rok := row["blockType"].(string)
		if lok && rok && lt != rt {
			return nil
		}
		return so
	}
	return nil
}

// hasObjectRow reports an array that is an array/blocks value rather than a
// scalar list.
func hasObjectRow(list []any) bool {
	for _, v := range list {
		if m, ok := v.(map[string]any); ok && !isEditorState(m) {
			return true
		}
	}
	return false
}
