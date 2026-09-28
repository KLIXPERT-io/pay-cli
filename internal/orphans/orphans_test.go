package orphans

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func doc(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	return m
}

// The klixpert stage shapes: group.blocks accepts bento and grid, bento.blocks
// holds bentoItem, grid.blocks holds text.
const bentoPage = `{"id":71,"title":"t","layout":[
 {"id":"g1","blockType":"group","blockGap":"standard","blocks":[
   {"id":"b1","blockType":"bento","grid":"2,2,2","section":{"contentWidth":"inhalt"},"blocks":[
     {"id":"i1","blockType":"bentoItem","heading":"one","content":{"root":{"type":"root","children":[]}}},
     {"id":"i2","blockType":"bentoItem","heading":"two"},
     {"id":"i3","blockType":"bentoItem","heading":"three"}]}]}]}`

const gridPage = `{"id":71,"title":"t","layout":[
 {"blockType":"group","blockGap":"standard","blocks":[
   {"blockType":"grid","columnsDesktop":3,"blocks":[
     {"blockType":"text","heading":"A","buttons":[]},
     {"blockType":"text","heading":"B","buttons":[]}]}]}]}`

// What the stage main row read back after the plain restructure: the three
// bentoItem rows reappear as {} inside the grid, interleaved by _order.
const orphanedMain = `{"id":71,"title":"t","layout":[
 {"id":"g2","blockType":"group","blockGap":"standard","blocks":[
   {"id":"r1","blockType":"grid","columnsDesktop":3,"blocks":[
     {},{"id":"t1","blockType":"text","heading":"A","buttons":[]},{},
     {"id":"t2","blockType":"text","heading":"B","buttons":[]},{}]}]}]}`

func TestRowsKindsContainersAndAncestors(t *testing.T) {
	rows := Rows(doc(t, bentoPage))
	if len(rows) != 5 {
		t.Fatalf("rows = %d, want 5 (group, bento, 3 items)", len(rows))
	}
	item := rows[2]
	if item.Path != "layout[0].blocks[0].blocks[0]" || item.Kind != "block:bentoItem" || item.Depth != 2 {
		t.Fatalf("item row = %+v", item)
	}
	if item.Container != (Container{Parent: "block:bento", Field: "blocks"}) {
		t.Errorf("container = %v", item.Container)
	}
	if len(item.Anc) != 2 || item.Anc[0].Kind != "block:group" || item.Anc[0].Field != "blocks" ||
		item.Anc[1].Kind != "block:bento" || item.Anc[1].List != "layout[0].blocks" {
		t.Errorf("ancestors = %+v", item.Anc)
	}
	if rows[0].Container != (Container{Parent: RootKind, Field: "layout"}) || rows[0].Depth != 0 {
		t.Errorf("root row = %+v", rows[0])
	}
}

func TestRowsSkipRichTextRelationRefsAndScalars(t *testing.T) {
	d := doc(t, `{"content":{"root":{"type":"root","children":[{"type":"paragraph","children":[]}]}},
		"tags":[1,2],"rel":[{"relationTo":"pages","value":3}],
		"hero":{"links":[{"id":"l1","link":{"label":"x"}}]}}`)
	rows := Rows(d)
	if len(rows) != 1 || rows[0].Path != "hero.links[0]" || !strings.HasPrefix(rows[0].Kind, "array:root>hero.links") {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestEmptyRows(t *testing.T) {
	got := EmptyRows(doc(t, orphanedMain))
	want := []string{"layout[0].blocks[0].blocks[0]", "layout[0].blocks[0].blocks[2]", "layout[0].blocks[0].blocks[4]"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("EmptyRows = %v, want %v", got, want)
	}
	if got := EmptyRows(doc(t, bentoPage)); len(got) != 0 {
		t.Errorf("clean page reported %v", got)
	}
}

func TestEchoCheckFindsInterleavedEmptyRows(t *testing.T) {
	sent := doc(t, gridPage)
	got := EchoCheck(sent, doc(t, orphanedMain))
	if len(got) != 3 {
		t.Fatalf("findings = %+v", got)
	}
	for _, f := range got {
		if f.Kind != FindEmptyRow {
			t.Errorf("finding %+v, want empty_row", f)
		}
	}
	// The clean echo (the version Payload returns) reports nothing.
	if got := EchoCheck(sent, doc(t, gridPage)); len(got) != 0 {
		t.Errorf("clean echo reported %+v", got)
	}
}

func TestEchoCheckExtraRowsAndTypeSequence(t *testing.T) {
	sent := doc(t, `{"layout":[{"blockType":"group","blocks":[{"blockType":"grid","blocks":[{"blockType":"text"}]}]}]}`)
	extra := doc(t, `{"layout":[{"id":"a","blockType":"group","blocks":[{"id":"b","blockType":"grid","blocks":[
		{"id":"c","blockType":"text"},{"id":"d","blockType":"bentoItem"}]}]}]}`)
	got := EchoCheck(sent, extra)
	if len(got) != 1 || got[0].Kind != FindExtraRow || got[0].BlockType != "bentoItem" ||
		got[0].Path != "layout[0].blocks[0].blocks[1]" {
		t.Fatalf("extra = %+v", got)
	}
	swapped := doc(t, `{"layout":[{"id":"a","blockType":"group","blocks":[{"id":"b","blockType":"bento","blocks":[]}]}]}`)
	got = EchoCheck(sent, swapped)
	if len(got) != 1 || got[0].Kind != FindTypeSequence || got[0].Path != "layout[0].blocks" {
		t.Fatalf("sequence = %+v", got)
	}
	lost := doc(t, `{"layout":[{"id":"a","blockType":"group","blocks":[{"id":"b","foo":1}]}]}`)
	got = EchoCheck(sent, lost)
	if len(got) != 1 || got[0].Kind != FindNoBlockType {
		t.Fatalf("lost type = %+v", got)
	}
}

func TestEchoCheckIgnoresDroppedRows(t *testing.T) {
	// Payload drops a row with an unknown blockType: that is §10.2's
	// input_silently_dropped, not an orphan, and must not be reported twice.
	sent := doc(t, `{"layout":[{"blockType":"nope"},{"blockType":"text"}]}`)
	echo := doc(t, `{"layout":[{"id":"x","blockType":"text"}]}`)
	if got := EchoCheck(sent, echo); len(got) != 0 {
		t.Fatalf("findings = %+v", got)
	}
}

func TestAnalyzeNestedRestructure(t *testing.T) {
	a := Analyze(doc(t, bentoPage), doc(t, gridPage), nil, nil)
	if len(a.Risks) != 1 || a.Risks[0].BlockType != "bentoItem" || len(a.Risks[0].Paths) != 3 {
		t.Fatalf("risks = %+v", a.Risks)
	}
	if want := []string{"layout[0].blocks[0]: bento → grid"}; !reflect.DeepEqual(a.Risks[0].Causes, want) {
		t.Errorf("causes = %v, want %v", a.Risks[0].Causes, want)
	}
	if len(a.Phases) != 1 || len(a.Phases[0]) != 1 {
		t.Fatalf("phases = %+v", a.Phases)
	}
	c := a.Phases[0][0]
	if c.List != "layout" || c.Clears != "bentoItem" || !reflect.DeepEqual(c.Chain, []string{"group", "bento"}) {
		t.Errorf("carrier = %+v", c)
	}
	// group{blocks:[bento{blocks:[]}]}, no ids, scalars kept.
	if _, ok := c.Row["id"]; ok {
		t.Error("carrier kept an id")
	}
	inner := c.Row["blocks"].([]any)
	if len(inner) != 1 {
		t.Fatalf("carrier group blocks = %v", inner)
	}
	bento := inner[0].(map[string]any)
	if bento["blockType"] != "bento" || len(bento["blocks"].([]any)) != 0 || bento["grid"] != "2,2,2" {
		t.Errorf("carrier bento = %v", bento)
	}
	if _, ok := bento["id"]; ok {
		t.Error("carrier bento kept its id")
	}
	// The input is untouched.
	orig := doc(t, bentoPage)
	if got := orig["layout"].([]any)[0].(map[string]any)["blocks"].([]any)[0].(map[string]any)["blocks"].([]any); len(got) != 3 {
		t.Error("fixture mutated")
	}
}

func TestAnalyzeTopLevelSwapIsClean(t *testing.T) {
	next := doc(t, `{"layout":[{"blockType":"text","heading":"x"}]}`)
	a := Analyze(doc(t, bentoPage), next, nil, nil)
	// group left the document: its nested lists are NOT reached any more, so
	// bento (group.blocks) and bentoItem (bento.blocks) both stay behind.
	if len(a.Risks) != 2 {
		t.Fatalf("risks = %+v", a.Risks)
	}
	if len(a.Phases) != 2 {
		t.Fatalf("phases = %d, want 2 (bentoItem first, then the bento carrier itself)", len(a.Phases))
	}
	// Top-level only: nothing nested, nothing at risk.
	flat := doc(t, `{"layout":[{"id":"x","blockType":"text"},{"id":"y","blockType":"cta","links":[{"id":"l","label":"a"}]}]}`)
	if a := Analyze(flat, next, nil, nil); a.NeedsReset() || len(a.Phases) != 0 {
		t.Fatalf("flat = %+v", a)
	}
}

func TestAnalyzeKeepsTypesTheWriteStillReaches(t *testing.T) {
	// bento stays somewhere in the new document, so bento.blocks is reached
	// and its bentoItem rows are cleared by the plain write.
	next := doc(t, `{"layout":[{"blockType":"group","blocks":[{"blockType":"grid","blocks":[]}]},
		{"blockType":"bento","blocks":[{"blockType":"bentoItem"}]}]}`)
	if a := Analyze(doc(t, bentoPage), next, nil, nil); a.NeedsReset() {
		t.Fatalf("risks = %+v", a.Risks)
	}
}

func TestAnalyzeRepairsEmptyRowsFromHistory(t *testing.T) {
	stored := doc(t, orphanedMain)
	// The plain restructure again: without history the {} rows are unnamed.
	a := Analyze(stored, doc(t, gridPage), nil, nil)
	if len(a.Empty) != 3 || len(a.Unresolved) != 3 || len(a.Phases) != 0 {
		t.Fatalf("without history: %+v", a)
	}
	a = Analyze(stored, doc(t, gridPage), []map[string]any{doc(t, bentoPage)}, nil)
	if len(a.Empty) != 3 || len(a.Unresolved) != 0 || len(a.Phases) != 1 || a.Phases[0][0].Clears != "bentoItem" {
		t.Fatalf("with history: %+v", a)
	}
	if got := a.ClearedTypes(); !reflect.DeepEqual(got, []string{"bentoItem"}) {
		t.Errorf("cleared = %v", got)
	}
}

func TestAnalyzeDeepChainPeelsLevels(t *testing.T) {
	stored := doc(t, `{"layout":[{"id":"1","blockType":"group","blocks":[{"id":"2","blockType":"tabs","panels":[
		{"id":"3","blockType":"tabPanel","blocks":[{"id":"4","blockType":"chart","data":1}]}]}]}]}`)
	next := doc(t, `{"layout":[{"blockType":"text"}]}`)
	a := Analyze(stored, next, nil, nil)
	if len(a.Risks) != 3 {
		t.Fatalf("risks = %+v", a.Risks)
	}
	if len(a.Phases) != 3 {
		t.Fatalf("phases = %d", len(a.Phases))
	}
	wantClears := []string{"chart", "tabPanel", "tabs"}
	for i, ph := range a.Phases {
		if len(ph) != 1 || ph[0].Clears != wantClears[i] {
			t.Errorf("phase %d = %+v, want clears %s", i, ph, wantClears[i])
		}
	}
	if !reflect.DeepEqual(a.Phases[0][0].Chain, []string{"group", "tabs", "tabPanel"}) {
		t.Errorf("chain = %v", a.Phases[0][0].Chain)
	}
}

func TestHollowKeepsArraysWithoutBlocks(t *testing.T) {
	row := doc(t, `{"id":"r","blockType":"cta","links":[{"id":"l","link":{"label":"x"}}],
		"blocks":[{"id":"b","blockType":"text"}],"section":{"items":[{"id":"s","blockType":"x"}]}}`)
	h := hollow(row, "", nil)
	links := h["links"].([]any)
	if len(links) != 1 {
		t.Fatalf("links = %v", links)
	}
	if _, ok := links[0].(map[string]any)["id"]; ok {
		t.Error("array row id kept")
	}
	if len(h["blocks"].([]any)) != 0 || len(h["section"].(map[string]any)["items"].([]any)) != 0 {
		t.Errorf("block lists not emptied: %v", h)
	}
	if _, ok := row["links"].([]any)[0].(map[string]any)["id"]; !ok {
		t.Error("input mutated")
	}
}

func TestAnalyzeCensusAcceptsRemovesFalseRisk(t *testing.T) {
	// grid → bento inside the group: the grid's text rows are cleared by the
	// plain write only because group.blocks ALSO accepts text — which this
	// document never shows, but the census does.
	stored := doc(t, `{"layout":[{"id":"g","blockType":"group","blocks":[{"id":"r","blockType":"grid","blocks":[
		{"id":"t","blockType":"text"}]}]}]}`)
	next := doc(t, `{"layout":[{"blockType":"group","blocks":[{"blockType":"bento","blocks":[{"blockType":"bentoItem"}]}]}]}`)
	if a := Analyze(stored, next, nil, nil); len(a.Risks) != 1 || a.Risks[0].BlockType != "text" {
		t.Fatalf("without the census: %+v", a.Risks)
	}
	acc := Accepts{{Parent: BlockKind("group"), Field: "blocks"}: {"text", "grid", "bento"}}
	if a := Analyze(stored, next, nil, acc); a.NeedsReset() || len(a.Phases) != 0 {
		t.Fatalf("with the census: %+v", a)
	}
}

func TestWithoutEmptyRows(t *testing.T) {
	in := doc(t, orphanedMain)
	out := WithoutEmptyRows(in)
	if got := EmptyRows(out); len(got) != 0 {
		t.Fatalf("still %v", got)
	}
	inner := out["layout"].([]any)[0].(map[string]any)["blocks"].([]any)[0].(map[string]any)["blocks"].([]any)
	if len(inner) != 2 {
		t.Errorf("inner = %v", inner)
	}
	if len(EmptyRows(in)) != 3 {
		t.Error("input mutated")
	}
	clean := doc(t, bentoPage)
	if got := WithoutEmptyRows(clean); !reflect.DeepEqual(got, clean) {
		t.Error("a clean document changed")
	}
}
