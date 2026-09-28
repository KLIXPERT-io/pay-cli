package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/lexical"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/rows"
)

// Body-shape checks against the document as it is stored (§9.12.8.3).
//
// Two write-body mistakes lose content without an error from Payload, and
// neither needs a schema or a census to be recognised once the document's
// current state is known:
//
//   - a STRING where the document holds a Lexical editor state — a document
//     read with `--richtext md|text --output raw` (no envelope left to say so)
//     written back. Payload stores the string in every block richText field,
//     and the admin editor can no longer open them. checkRenderedBody only
//     sees top-level/group fields the shard declares richText, and the census
//     warning (richtext_as_string) needs a cached census.
//   - an OBJECT where the document holds an array — `{"layout":{"0":…}}`, what
//     `--set layout.0.x=…` or a hand-written body builds. Payload replaces the
//     whole array with nothing: the page loses every row (verified on
//     localhost, Payload 3.87 / Postgres: layout 2 rows → 0, still 200).
//
// Both refuse the write. The pre-image is the one a dry run, a backup, an
// upsert match or a plan already read; a real update by id (and apply,
// globals update) whose body could hold either mistake reads it once.

// storedShapeProblems compares a write body with the document it is merged
// into. strs are paths where the body holds a non-empty string and the
// document an editor state; objs are paths where the body holds an object and
// the document an array. Array rows are matched by id, else by index when the
// blockType agrees (a moved or new row is not compared).
func storedShapeProblems(body, current map[string]any) (strs, objs []string) {
	var walk func(b, c any, path string, depth int)
	walk = func(b, c any, path string, depth int) {
		if depth > 64 || c == nil {
			return
		}
		switch bv := b.(type) {
		case string:
			if bv != "" && lexical.IsEditorState(c) {
				strs = append(strs, path)
			}
		case map[string]any:
			if _, isList := c.([]any); isList {
				objs = append(objs, path)
				return
			}
			cm, ok := c.(map[string]any)
			if !ok || lexical.IsEditorState(bv) || lexical.IsEditorState(cm) {
				return
			}
			for _, k := range sortedMapKeys(bv) {
				walk(bv[k], cm[k], dotJoin(path, k), depth+1)
			}
		case []any:
			cl, ok := c.([]any)
			if !ok {
				return
			}
			for i, row := range bv {
				if match := matchStoredRow(row, i, cl); match != nil {
					walk(row, match, path+"["+strconv.Itoa(i)+"]", depth+1)
				}
			}
		}
	}
	walk(body, current, "", 0)
	return strs, objs
}

// matchStoredRow finds the stored row a body row is compared with: the row
// with the same id, else the row at the same index when neither carries an id
// and their blockTypes agree.
func matchStoredRow(row any, i int, stored []any) any {
	rm, ok := row.(map[string]any)
	if !ok {
		if i < len(stored) {
			return stored[i]
		}
		return nil
	}
	if id, has := rm[rows.KeyID]; has && id != nil {
		want := fmt.Sprint(id)
		for _, s := range stored {
			if sm, ok := s.(map[string]any); ok && sm[rows.KeyID] != nil && fmt.Sprint(sm[rows.KeyID]) == want {
				return sm
			}
		}
		return nil
	}
	if i >= len(stored) {
		return nil
	}
	sm, ok := stored[i].(map[string]any)
	if !ok {
		return nil
	}
	if fmt.Sprint(rm[rows.KeyBlockType]) != fmt.Sprint(sm[rows.KeyBlockType]) {
		return nil
	}
	return sm
}

// needsStoredShapeCheck reports whether a body could hold either mistake in
// a place only the stored document can reveal: block rows (whose rich text
// the shard never declares), or an object with an array-index key.
func needsStoredShapeCheck(body map[string]any) bool {
	return discovery.ContainsBlockRows(body) || hasIndexKeyedObject(body, 0)
}

// hasIndexKeyedObject finds an object with an all-digit key — the shape a
// dotted index path (`layout.0.content`) turns into.
func hasIndexKeyedObject(v any, depth int) bool {
	if depth > 64 {
		return false
	}
	switch t := v.(type) {
	case map[string]any:
		if lexical.IsEditorState(t) {
			return false
		}
		for k, sub := range t {
			if isAllDigitsString(k) || hasIndexKeyedObject(sub, depth+1) {
				return true
			}
		}
	case []any:
		for _, sub := range t {
			if hasIndexKeyedObject(sub, depth+1) {
				return true
			}
		}
	}
	return false
}

// checkStoredShapes refuses a body that would replace an editor state with a
// string or an array with an object, judged against current (the pre-image a
// command already holds). With current nil and a body that could hold either
// mistake, it reads the newest version once; a failed read never blocks.
func (d *Deps) checkStoredShapes(ctx context.Context, client *payload.Client, o orphanTarget, p query.Params,
	body, current map[string]any) error {
	if len(body) == 0 {
		return nil
	}
	if current == nil {
		if client == nil || !needsStoredShapeCheck(body) {
			return nil
		}
		// A failed read never blocks the write: the check is advice the
		// document can give, and the write reports its own errors.
		if doc, rerr := d.readOrphanTarget(ctx, client, o, p, true); rerr == nil && doc != nil {
			current = map[string]any(doc)
		}
		if current == nil {
			return nil
		}
	}
	return storedShapeError(d.commandName(), o, body, current)
}

// storedShapeError is checkStoredShapes' verdict without the read.
func storedShapeError(who string, o orphanTarget, body, current map[string]any) error {
	strs, objs := storedShapeProblems(body, current)
	if len(objs) > 0 {
		return apierr.New(apierr.CodeBadRequestBody,
			"%s: the body holds an object at %s, where %s stores an array; Payload would replace the whole array with it and the document would lose every row (a key like layout.0.content builds such an object)",
			who, pathList(objs), o.what()).
			WithHint("a write body replaces a whole array: send the complete array, or edit one row in a pipe: %s --depth 0 --draft | pay blocks set <selector> … --field <array> | pay apply --draft",
				o.readCmd())
	}
	if len(strs) > 0 {
		return apierr.New(apierr.CodeRichTextRendered,
			"%s: %d field(s) hold a string where %s stores a Lexical editor state (%s); Payload would store the string and the admin editor could no longer open the field (a document read with --richtext md|text, or a hand-written value)",
			who, len(strs), o.what(), pathList(strs)).
			WithHint("re-read the document without --richtext (%s --depth 0 --draft) and edit that, or set a field from Markdown: pay blocks set <selector> --set-md <field>=@file.md --field <array>",
				o.readCmd())
	}
	return nil
}

// readCmd is the command that reads the target at depth 0, for hints.
func (o orphanTarget) readCmd() string {
	if o.g != nil {
		return "pay globals get " + o.g.Slug
	}
	return "pay get " + o.t.Slug + " " + shellQuote(o.id)
}

// checkKnownArrayShapes is the check without a pre-image (create, and every
// write before its read): an object at a field the schema types array/blocks,
// or the cached census knows as a blocks field, is refused when it can only
// be a row list gone wrong — an all-digit key (`layout.0.content` turned into
// {"0":…}) or a row's own keys (blockType, id). Any other object is left
// alone: with `--locale all` a localized array is an object keyed by locale.
func checkKnownArrayShapes(d *Deps, t *collTarget, shard *discovery.Shard, body map[string]any) error {
	known := knownArrayPaths(d, t, shard)
	if len(known) == 0 {
		return nil
	}
	var bad []string
	var walk func(obj map[string]any, prefix string, depth int)
	walk = func(obj map[string]any, prefix string, depth int) {
		for _, k := range sortedMapKeys(obj) {
			m, ok := obj[k].(map[string]any)
			if !ok || lexical.IsEditorState(m) {
				continue
			}
			p := dotJoin(prefix, k)
			if known[p] {
				if rowListGoneWrong(m) {
					bad = append(bad, p)
				}
				continue
			}
			if depth < 8 {
				walk(m, p, depth+1)
			}
		}
	}
	walk(body, "", 0)
	if len(bad) == 0 {
		return nil
	}
	slug := targetSlug(t)
	return apierr.New(apierr.CodeBadRequestBody,
		"%s: the body holds an object at %s, which is an array field of %s; Payload would replace the whole array with it and the document would lose every row (a key like layout.0.content builds such an object)",
		d.commandName(), pathList(bad), slug).
		WithHint("a write body replaces a whole array: send the complete array, or edit one row in a pipe: pay get %s <id> --depth 0 --draft | pay blocks set <selector> … --field <array> | pay apply --draft",
			slug)
}

// rowListGoneWrong is an object that can only have been meant as array rows.
func rowListGoneWrong(m map[string]any) bool {
	for k := range m {
		if isAllDigitsString(k) || k == rows.KeyBlockType || k == rows.KeyID {
			return true
		}
	}
	return false
}

// knownArrayPaths are the dotted paths the schema types array or blocks, plus
// the blocks fields of the cached census (never built here).
func knownArrayPaths(d *Deps, t *collTarget, shard *discovery.Shard) map[string]bool {
	out := map[string]bool{}
	if shard != nil {
		for _, f := range shard.Fields {
			if f.PayloadType == discovery.TypeArray || f.PayloadType == discovery.TypeBlocks {
				out[f.Path] = true
			}
		}
	}
	if t == nil || d == nil || d.RT == nil {
		return out
	}
	c, ok := d.RT.cachedCensus()
	if !ok {
		return out
	}
	kind := discovery.EntityKindCollection
	if t.Coll == nil && d.Manifest != nil {
		if _, isGlobal := d.Manifest.Global(t.Slug); isGlobal {
			kind = discovery.EntityKindGlobal
		}
	}
	for _, p := range discovery.NewBlockKnowledge(kind, t.Slug, c, shard).TopLevelPaths() {
		out[p] = true
	}
	return out
}

// dotJoin extends a dotted field path.
func dotJoin(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// warnArrayAsObject: a preview's body holds an object where the document
// holds an array (`pay update` refuses it).
const warnArrayAsObject = "array_as_object"

// storedShapeWarnings is storedShapeProblems as warnings, for `pay diff`,
// which previews and never refuses.
func storedShapeWarnings(body, current map[string]any) []output.Warning {
	strs, objs := storedShapeProblems(body, current)
	var out []output.Warning
	if len(strs) > 0 {
		out = append(out, output.Warning{
			Code: warnRichTextAsString,
			Message: fmt.Sprintf("%d field(s) hold a string where the document stores a Lexical editor state; `pay update` refuses this write (richtext_rendered)",
				len(strs)),
			Paths: capPaths(strs),
			Hint:  "re-read the document without --richtext, or set a field from Markdown: pay blocks set <selector> --set-md <field>=@file.md --field <array>",
		})
	}
	if len(objs) > 0 {
		out = append(out, output.Warning{
			Code: warnArrayAsObject,
			Message: fmt.Sprintf("%d field(s) hold an object where the document stores an array; Payload would replace the whole array and `pay update` refuses this write",
				len(objs)),
			Paths: capPaths(objs),
			Hint:  "a write body replaces a whole array: send the complete array, or edit one row with pay blocks set … --field <array> | pay apply",
		})
	}
	return out
}

// pathList renders paths for a message, at most maxWarnPaths of them.
func pathList(paths []string) string {
	if len(paths) <= maxWarnPaths {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(paths[:maxWarnPaths], ", "), len(paths)-maxWarnPaths)
}
