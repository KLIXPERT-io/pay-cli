package discovery

import (
	"encoding/json"
	"fmt"
	"testing"
)

// restructureCensus is the stage situation behind the `questions` false
// positive: six pages held a questions block at the top level of layout, with
// intro filled in, when the census was read.
func restructureCensus(t *testing.T) *CensusEntity {
	t.Helper()
	b := NewCensusBuilder(EntityKindCollection, "pages")
	for i := 46; i <= 51; i++ {
		b.AddDoc(i, decodeDoc(t, fmt.Sprintf(`{"id":%d,"layout":[
		  {"blockType":"text","heading":"H%d","textSize":"regular"},
		  {"blockType":"group","blocks":[{"blockType":"text","heading":"G","textSize":"large"}]},
		  {"blockType":"questions","intro":"Intro %d","items":[{"q":"Q%d"}]}]}`, i, i, i, i)))
	}
	return b.Finish()
}

// after the restructure: questions moved into the group, intro left empty.
func restructuredPage(t *testing.T, heading string) map[string]any {
	t.Helper()
	return decodeDoc(t, `{"id":50,"layout":[
	  {"blockType":"text","heading":"`+heading+`","textSize":"regular"},
	  {"blockType":"group","blocks":[{"blockType":"text","heading":"G","textSize":"large"},
	    {"blockType":"questions","intro":null,"items":[{"q":"Q"}]}]}]}`)
}

func TestOverlayDocsClearsFindingsTheServerContradicts(t *testing.T) {
	base := restructureCensus(t)
	before, _ := json.Marshal(base)
	c := NewCensus("0123456789abcdef")
	c.Put(base)
	body := restructuredPage(t, "changed heading")

	stale := ValidateBlocks(body, NewBlockKnowledge(EntityKindCollection, "pages", c, nil))
	got := issueCodes(stale)
	if _, ok := got[IssueBlockTypeNotObserved+"@layout[1].blocks[1]"]; !ok {
		t.Fatalf("precondition: the stale census reports the moved row: %+v", stale.Issues)
	}
	if _, ok := got[IssueLikelyRequired+"@layout[1].blocks[1].intro"]; !ok {
		t.Fatalf("precondition: the stale census demands intro: %+v", stale.Issues)
	}

	current := restructuredPage(t, "old heading")
	ov := OverlayDocs(base, false, current)
	fresh := ValidateBlocks(body, NewBlockKnowledge(EntityKindCollection, "pages", c.WithEntity(ov), nil))
	if len(fresh.Issues) != 0 {
		t.Fatalf("the pre-image holds exactly this row at this position: %+v", fresh.Issues)
	}
	after, _ := json.Marshal(base)
	if string(before) != string(after) {
		t.Error("OverlayDocs modified the cached entity")
	}
	if e, _ := c.Entity(EntityKindCollection, "pages"); e != base {
		t.Error("WithEntity modified the receiver")
	}

	// Evidence is only added: a value nobody used is still off-vocabulary,
	// and a type no document carries is still unknown.
	odd := restructuredPage(t, "x")
	odd["layout"].([]any)[0].(map[string]any)["textSize"] = "huge"
	odd["layout"] = append(odd["layout"].([]any), map[string]any{"blockType": "heroo"})
	res := issueCodes(ValidateBlocks(odd, NewBlockKnowledge(EntityKindCollection, "pages", c.WithEntity(ov), nil)))
	if _, ok := res[IssueValueNotInEnum+"@layout[0].textSize"]; !ok {
		t.Errorf("an overlay must not widen a vocabulary beyond what the document holds: %+v", res)
	}
	if _, ok := res[IssueUnknownBlockType+"@layout[2]"]; !ok {
		t.Errorf("unknown type lost: %+v", res)
	}

	if OverlayDocs(nil, false, current) != nil {
		t.Error("one document is not a census")
	}
}

func TestOverlayDocsPersistKeepsNoContent(t *testing.T) {
	base := restructureCensus(t)
	doc := restructuredPage(t, "A unique heading that is content")
	ov := OverlayDocs(base, true, doc)
	if ov.FoldedDocs != 1 {
		t.Errorf("folded_docs = %d", ov.FoldedDocs)
	}
	for _, v := range ov.Types["text"].Fields["heading"].Values {
		if v.Value == "A unique heading that is content" {
			t.Errorf("a value seen once reached the persisted census: %+v", ov.Types["text"].Fields["heading"].Values)
		}
	}
	if ov.Paths["layout[].blocks"].Types["questions"] != 1 {
		t.Errorf("the new position was not folded in: %+v", ov.Paths["layout[].blocks"])
	}
}

// formPage is a page holding one formBlock whose rich-text intro is filled in.
func formPage(t *testing.T, id int, title string) map[string]any {
	t.Helper()
	return decodeDoc(t, fmt.Sprintf(`{"id":%d,"title":%q,"layout":[{"blockType":"formBlock","form":1,"enableIntro":true,
	  "introContent":{"root":{"type":"root","children":[{"type":"paragraph","children":[{"type":"text","text":"Hi"}]}]}}}]}`, id, title))
}

func formStats(e *CensusEntity) (count, docs, present, nonNull int) {
	ft := e.Types["formBlock"]
	f := ft.Fields["introContent"]
	return ft.Count, ft.Docs, f.Present, f.NonNull
}

// Regression: every echo fold of the same document used to add its rows
// again, so iterating on one page made its fields "likely required" for every
// future row of that type (localhost: count 2 → 5 after three title-only
// updates; a later formBlock without introContent warned likely_required).
func TestOverlayDocsCountsEachDocumentOnce(t *testing.T) {
	b := NewCensusBuilder(EntityKindCollection, "pages")
	b.AddDoc(40, formPage(t, 40, "real"))
	base := b.Finish()
	if got := base.DocKeys; len(got) != 1 || got[0] != "40" {
		t.Fatalf("doc_keys = %v", got)
	}

	// A new document (the page a create just made) is new evidence.
	e := OverlayDocs(base, true, formPage(t, 106, "created"))
	if c, d, p, n := formStats(e); c != 2 || d != 2 || p != 2 || n != 2 {
		t.Fatalf("after the create fold: count=%d docs=%d present=%d non_null=%d, want 2 each", c, d, p, n)
	}
	// Three title-only updates of the same page echo the same layout.
	for i := 0; i < 3; i++ {
		e = OverlayDocs(e, true, formPage(t, 106, fmt.Sprintf("title %d", i)))
	}
	if c, d, p, n := formStats(e); c != 2 || d != 2 || p != 2 || n != 2 {
		t.Errorf("after three re-folds of page 106: count=%d docs=%d present=%d non_null=%d, want 2 each", c, d, p, n)
	}
	if e.FoldedDocs != 1 || len(e.FoldedKeys) != 1 || e.FoldedKeys[0] != "106" {
		t.Errorf("folded_docs=%d folded_keys=%v, want 1 [106]", e.FoldedDocs, e.FoldedKeys)
	}
	if got := e.Paths["layout"].Types["formBlock"]; got != 2 {
		t.Errorf("layout formBlock count = %d, want 2", got)
	}

	// Even with a third document the field is filled on every row, the
	// re-folds must not be what pushes it over the likely_required line.
	c := NewCensus("0123456789abcdef")
	c.Put(e)
	body := decodeDoc(t, `{"layout":[{"blockType":"formBlock","form":1,"enableIntro":false,"introContent":null}]}`)
	// The dry-run pre-image of page 106 is folded in memory once more.
	pre := OverlayDocs(e, false, formPage(t, 106, "pre"))
	if cnt, _, _, _ := formStats(pre); cnt != 2 {
		t.Errorf("the pre-image of a counted document raised the count to %d", cnt)
	}
	res := issueCodes(ValidateBlocks(body, NewBlockKnowledge(EntityKindCollection, "pages", c.WithEntity(pre), nil)))
	if _, ok := res[IssueLikelyRequired+"@layout[0].introContent"]; ok {
		t.Errorf("two real formBlock rows are not evidence that introContent is required: %v", res)
	}

	// The pre-image of a document the census itself READ is not new either.
	pre40 := OverlayDocs(base, false, formPage(t, 40, "pre"))
	if cnt, _, _, _ := formStats(pre40); cnt != 1 {
		t.Errorf("the pre-image of a document the census read raised the count to %d", cnt)
	}
}

// A counted document that now holds a field empty proves the field optional,
// without counting the document again.
func TestOverlayDocsRefreshMarksEmptiedFieldOptional(t *testing.T) {
	b := NewCensusBuilder(EntityKindCollection, "pages")
	for id := 1; id <= 4; id++ {
		b.AddDoc(id, formPage(t, id, "p"))
	}
	base := b.Finish()
	f := base.Types["formBlock"].Fields["introContent"]
	if !likelyRequired(f, base.Types["formBlock"].Count) {
		t.Fatal("precondition: introContent filled on 4/4 rows is likely required")
	}
	emptied := formPage(t, 2, "p")
	emptied["layout"].([]any)[0].(map[string]any)["introContent"] = nil
	e := OverlayDocs(base, true, emptied)
	ft := e.Types["formBlock"]
	if ft.Count != 4 || ft.Fields["introContent"].NonNull != 4 {
		t.Errorf("a refresh changed the counts: count=%d non_null=%d", ft.Count, ft.Fields["introContent"].NonNull)
	}
	if !ft.Fields["introContent"].Optional || likelyRequired(ft.Fields["introContent"], ft.Count) {
		t.Error("the server holds a formBlock without introContent; it must no longer be likely required")
	}
	raw, _ := json.Marshal(e)
	c, ok := DecodeCensus([]byte(`{"census_version":1,"collections":{"pages":` + string(raw) + `}}`))
	if !ok || !c.Collections["pages"].Types["formBlock"].Fields["introContent"].Optional {
		t.Error("optional did not survive the cache round trip")
	}
}

// A census written before doc_keys existed cannot tell a new document from a
// counted one, so every fold only refreshes.
func TestOverlayDocsLegacyCensusNeverInflates(t *testing.T) {
	b := NewCensusBuilder(EntityKindCollection, "pages")
	b.AddDoc(40, formPage(t, 40, "real"))
	base := b.Finish()
	base.DocKeys = nil
	e := OverlayDocs(base, true, formPage(t, 106, "x"))
	e = OverlayDocs(e, true, formPage(t, 106, "y"))
	if c, _, _, _ := formStats(e); c != 1 {
		t.Errorf("legacy census count = %d, want 1", c)
	}
}
