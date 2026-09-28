package docdiff

import (
	"encoding/json"
	"strings"
	"testing"
)

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// stored is a trimmed copy of the shape klixpert stage's pages 2 has:
// top-level blocks, a group with nested blocks, named groups inside rows,
// and rich text.
const stored = `{
  "id": 2, "title": "Prozess", "slug": "prozess", "_status": "draft",
  "meta": {"title": "Prozess", "description": "d", "image": 4},
  "layout": [
    {"id": "h", "blockType": "hero", "title": "T", "image": {"file": 4, "alt": ""}, "kpi": {"label": null, "value": null}},
    {"id": "g", "blockType": "group", "blockGap": "standard", "blocks": [
      {"id": "s1", "blockType": "processStep", "title": "One", "content": ` + "%s" + `},
      {"id": "s2", "blockType": "processStep", "title": "Two"}
    ]}
  ],
  "updatedAt": "2026-09-01T00:00:00Z"
}`

func storedDoc(t *testing.T) map[string]any {
	t.Helper()
	return obj(t, strings.Replace(stored, "%s", richText("Erster Schritt"), 1))
}

// A body carrying only the keys it changes changes only those keys.
func TestPatchAbsentKeysKeepTheirValue(t *testing.T) {
	cur := storedDoc(t)
	res := DiffPatch(cur, obj(t, `{"title":"Neu"}`), Options{})
	wantPaths(t, res, "change title")
}

// Named groups merge key by key (beforeValidate recurses with the stored
// group as siblingDoc).
func TestPatchGroupsMerge(t *testing.T) {
	cur := storedDoc(t)
	res := DiffPatch(cur, obj(t, `{"meta":{"description":"neu"}}`), Options{})
	wantPaths(t, res, "change meta.description")
}

// A row whose id matches a stored row keeps its unsent keys
// (getExistingRowDoc); only the sent key changes, at any depth.
func TestPatchRowsMatchedByIDMerge(t *testing.T) {
	cur := storedDoc(t)
	body := obj(t, `{"layout":[
	  {"id":"h"},
	  {"id":"g","blocks":[{"id":"s1"},{"id":"s2","title":"Zwei"}]}]}`)
	res := DiffPatch(cur, body, Options{})
	wantPaths(t, res, "change layout[1].blocks[1].title")

	patched := ApplyPatch(cur, body, Options{})
	row := patched["layout"].([]any)[0].(map[string]any)
	if row["blockType"] != "hero" || row["title"] != "T" {
		t.Fatalf("merged row lost its stored keys: %v", row)
	}
}

// Rows the body omits are deleted; rows without an id are new.
func TestPatchArraysReplaceRows(t *testing.T) {
	cur := storedDoc(t)
	body := obj(t, `{"layout":[{"id":"g"},{"blockType":"cta","title":"Jetzt"}]}`)
	res := DiffPatch(cur, body, Options{})
	wantPaths(t, res, "remove layout[0]", "add layout[1]")
	if res.Changes[0].ID != "h" || res.Changes[0].BlockType != "hero" {
		t.Fatalf("remove = %+v", res.Changes[0])
	}
}

// The whole stored document sent back with one nested change is exactly
// one change — the `pay diff --data-file` round trip.
func TestPatchWithAWholeEditedCopy(t *testing.T) {
	cur := storedDoc(t)
	edited := storedDoc(t)
	edited["layout"].([]any)[1].(map[string]any)["blocks"].([]any)[0].(map[string]any)["title"] = "Eins"
	edited["updatedAt"] = "2030-01-01T00:00:00Z"
	res := DiffPatch(cur, edited, Options{})
	wantPaths(t, res, "change layout[1].blocks[0].title")
}

func TestPatchRichTextAndScalarListsReplace(t *testing.T) {
	cur := obj(t, `{"c":`+richText("a", "b")+`,"tags":[1,2,3]}`)
	body := obj(t, `{"c":`+richText("a")+`,"tags":[3]}`)
	patched := ApplyPatch(cur, body, Options{})
	if jsonOf(t, patched["c"]) != jsonOf(t, body["c"]) {
		t.Fatalf("rich text must be replaced, not merged: %s", jsonOf(t, patched["c"]))
	}
	if jsonOf(t, patched["tags"]) != `[3]` {
		t.Fatalf("tags = %s", jsonOf(t, patched["tags"]))
	}
}

// stripNullRows: non-object rows vanish from an array/blocks value.
func TestPatchStripsNonObjectRows(t *testing.T) {
	cur := obj(t, `{"layout":[{"id":"a","blockType":"x"}]}`)
	patched := ApplyPatch(cur, obj(t, `{"layout":[null,{"id":"a"},7]}`), Options{})
	if jsonOf(t, patched["layout"]) != `[{"blockType":"x","id":"a"}]` {
		t.Fatalf("layout = %s", jsonOf(t, patched["layout"]))
	}
}

// A changed blockType at the same id is not merged: the old block's keys
// must not appear in a row Payload stores as the new type.
func TestPatchDoesNotMergeAcrossBlockTypes(t *testing.T) {
	cur := obj(t, `{"layout":[{"id":"a","blockType":"bento","items":[1]}]}`)
	patched := ApplyPatch(cur, obj(t, `{"layout":[{"id":"a","blockType":"grid"}]}`), Options{})
	if jsonOf(t, patched["layout"]) != `[{"blockType":"grid","id":"a"}]` {
		t.Fatalf("layout = %s", jsonOf(t, patched["layout"]))
	}
}

func TestPatchLeafPathsReplaceObjects(t *testing.T) {
	cur := obj(t, `{"settings":{"a":1,"b":2}}`)
	body := obj(t, `{"settings":{"a":5}}`)
	if got := jsonOf(t, ApplyPatch(cur, body, Options{})["settings"]); got != `{"a":5,"b":2}` {
		t.Fatalf("group merge = %s", got)
	}
	if got := jsonOf(t, ApplyPatch(cur, body, Options{LeafPaths: []string{"settings"}})["settings"]); got != `{"a":5}` {
		t.Fatalf("leaf replace = %s", got)
	}
}

func TestPatchIgnoresTheBodyIDAndDoesNotMutate(t *testing.T) {
	cur := obj(t, `{"id":2,"t":"a","g":{"x":1}}`)
	body := obj(t, `{"id":3,"t":"b","g":{"y":2}}`)
	curBefore, bodyBefore := jsonOf(t, cur), jsonOf(t, body)
	patched := ApplyPatch(cur, body, Options{})
	if jsonOf(t, patched) != `{"g":{"x":1,"y":2},"id":2,"t":"b"}` {
		t.Fatalf("patched = %s", jsonOf(t, patched))
	}
	if jsonOf(t, cur) != curBefore || jsonOf(t, body) != bodyBefore {
		t.Fatal("ApplyPatch mutated an input")
	}
	// Mutating the result must not reach the inputs either.
	patched["g"].(map[string]any)["x"] = 99
	if jsonOf(t, cur) != curBefore {
		t.Fatal("the result aliases the stored document")
	}
	if got := ApplyPatch(nil, obj(t, `{"a":1}`), Options{}); jsonOf(t, got) != `{"a":1}` {
		t.Fatalf("nil current = %s", jsonOf(t, got))
	}
}

// New rows are cleaned at every depth, like Payload's beforeValidate does.
func TestPatchCleansNestedRowsOfNewRows(t *testing.T) {
	patched := ApplyPatch(obj(t, `{}`), obj(t, `{"layout":[{"blockType":"group","blocks":[null,{"blockType":"cta"}],"g":{"x":1}}]}`), Options{})
	if got := jsonOf(t, patched["layout"]); got != `[{"blockType":"group","blocks":[{"blockType":"cta"}],"g":{"x":1}}]` {
		t.Fatalf("layout = %s", got)
	}
}
