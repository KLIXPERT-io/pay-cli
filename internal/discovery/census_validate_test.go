package discovery

import (
	"fmt"
	"strings"
	"testing"
)

func issueCodes(v BlockValidation) map[string]BlockIssue {
	out := map[string]BlockIssue{}
	for _, is := range v.Issues {
		out[is.Code+"@"+is.Path] = is
	}
	return out
}

func stageKnowledge(t *testing.T) *BlockKnowledge {
	t.Helper()
	_, e := buildTestCensus(t)
	c := NewCensus("0123456789abcdef")
	c.Put(e)
	return NewBlockKnowledge(EntityKindCollection, "pages", c, nil)
}

// The regression the census exists for: every real document of the project
// validates with no findings at all.
func TestValidateRealDocumentsHasNoFalseFindings(t *testing.T) {
	k := stageKnowledge(t)
	for _, d := range censusDocs(t)[2:] {
		if v := ValidateBlocks(d, k); len(v.Issues) != 0 {
			t.Errorf("doc %v: %+v", d["id"], v.Issues)
		}
	}
	v := ValidateBlocks(censusDocs(t)[0], k)
	// Doc 1 carries the orphan {} row, which is a real error.
	if v.Errors != 1 || v.Warnings != 0 || v.CheckedRows != 8 {
		t.Fatalf("doc 1 = %+v", v)
	}
	if _, ok := issueCodes(v)[IssueEmptyRow+"@layout[1].blocks[2]"]; !ok {
		t.Errorf("orphan row not reported at its path: %+v", v.Issues)
	}
}

func TestValidateEveryObservedFinding(t *testing.T) {
	k := stageKnowledge(t)
	doc := decodeDoc(t, `{"layout":[
	  {"blockType":"heroo"},
	  {"blockType":"hero","title":"t","textSize":"huge","headline":"x","content":`+lexicalHello+`},
	  {"blockType":"group","blocks":[{"blockType":"hero","title":"h","content":`+lexicalHello+`},{"blockType":"text","section":{"contentWidth":"inhalt","colour":"red"}}]},
	  {"title":"untyped"}]}`)
	v := ValidateBlocks(doc, k)
	got := issueCodes(v)
	want := map[string]string{
		IssueUnknownBlockType + "@layout[0]":                      SeverityWarning,
		IssueValueNotInEnum + "@layout[1].textSize":               SeverityWarning,
		IssueUnknownField + "@layout[1].headline":                 SeverityWarning,
		IssueBlockTypeNotObserved + "@layout[2].blocks[0]":        SeverityWarning,
		IssueUnknownField + "@layout[2].blocks[1].section.colour": SeverityWarning,
		IssueMissingBlockType + "@layout[3]":                      SeverityError,
	}
	for key, sev := range want {
		is, ok := got[key]
		if !ok {
			t.Errorf("missing %s in %+v", key, v.Issues)
			continue
		}
		if is.Severity != sev {
			t.Errorf("%s severity = %s, want %s", key, is.Severity, sev)
		}
		if is.Source == "" {
			t.Errorf("%s carries no source", key)
		}
	}
	if is := got[IssueUnknownBlockType+"@layout[0]"]; !strings.Contains(is.Hint, `"hero"`) {
		t.Errorf("typo hint = %q", is.Hint)
	}
	// Observed-only knowledge may never produce an error for a block choice.
	for _, is := range v.Issues {
		if is.Severity == SeverityError && is.Code != IssueMissingBlockType {
			t.Errorf("observation produced an error: %+v", is)
		}
	}
}

func TestValidateDeclaredFactsAreErrors(t *testing.T) {
	k := NewBlockKnowledge(EntityKindCollection, "pages", unionCensus(t), unionShard())
	doc := decodeDoc(t, `{"layout":[
	  {"blockType":"callToActionBlock"},
	  {"blockType":"cta","richText":`+lexicalHello+`,"links":[{"link":{"type":"custom","appearance":"loud","label":"x","url":"/"}}]}]}`)
	v := ValidateBlocks(doc, k)
	got := issueCodes(v)
	if is := got[IssueUnknownBlockType+"@layout[0]"]; is.Severity != SeverityError || is.Source != SourceGraphQL {
		t.Errorf("an inferred slug outside the reconciled union is a declared error: %+v", is)
	}
	if is := got[IssueValueNotInEnum+"@layout[1].links[0].link.appearance"]; is.Severity != SeverityError || is.Source != SourceGraphQLEnum {
		t.Errorf("a GraphQL enum violation is an error: %+v", v.Issues)
	}
	if is := got[IssueRequiredMissing+"@layout[1].headline"]; is.Severity != SeverityError {
		t.Errorf("NON_NULL headline missing = %+v", v.Issues)
	}
}

func TestValidateLikelyRequired(t *testing.T) {
	k := stageKnowledge(t)
	doc := decodeDoc(t, `{"layout":[{"blockType":"hero","title":"","textSize":"regular"}]}`)
	got := issueCodes(ValidateBlocks(doc, k))
	for _, path := range []string{"layout[0].title", "layout[0].content"} {
		if is, ok := got[IssueLikelyRequired+"@"+path]; !ok || is.Severity != SeverityWarning {
			t.Errorf("likely_required %s missing: %+v", path, got)
		}
	}
	if _, ok := got[IssueLikelyRequired+"@layout[0].variant"]; ok {
		t.Error("a constant (default) value is never likely-required")
	}
}

func TestValidateWithNoKnowledgeIsStructuralOnly(t *testing.T) {
	doc := decodeDoc(t, `{"layout":[{"blockType":"anything","x":1},{},{"a":1}]}`)
	v := ValidateBlocks(doc, NewBlockKnowledge(EntityKindCollection, "pages", nil, nil))
	if v.CheckedRows != 1 || v.Errors != 2 || v.Warnings != 0 {
		t.Fatalf("got %+v", v)
	}
	v = ValidateBlocks(doc, nil)
	if v.Errors != 2 {
		t.Fatalf("nil knowledge = %+v", v)
	}
}

func TestValidateRecognisesOrphanOnlyArrays(t *testing.T) {
	k := stageKnowledge(t)
	// Every row is {}: only the census knows this is a blocks field.
	doc := decodeDoc(t, `{"layout":[{"blockType":"group","blocks":[{},{}]}]}`)
	v := ValidateBlocks(doc, k)
	if v.Errors != 2 {
		t.Fatalf("orphan-only nested array = %+v", v)
	}
}

// forms' `text` field block and a page's `text` block share a slug and
// nothing else. With pages never censused, or never having used `text`, a
// page row must not be compared with the form-builder's shape.
func TestValidateIgnoresAForeignShapeOfTheSameSlug(t *testing.T) {
	fb := NewCensusBuilder(EntityKindCollection, "forms")
	for i := 1; i <= 7; i++ {
		fb.AddDoc(i, decodeDoc(t, fmt.Sprintf(`{"id":%d,"fields":[
		  {"blockType":"text","name":"n%d","label":"L%d","width":50,"required":true,"defaultValue":null},
		  {"blockType":"text","name":"m%d","label":"M%d","width":100,"required":false,"defaultValue":null}]}`, i, i, i, i, i)))
	}
	c := NewCensus("0123456789abcdef")
	c.Put(fb.Finish())
	page := decodeDoc(t, `{"layout":[{"blockType":"text","heading":"H","content":null}]}`)

	// 1. pages has no census entry at all.
	v := ValidateBlocks(page, NewBlockKnowledge(EntityKindCollection, "pages", c, nil))
	if len(v.Issues) != 0 {
		t.Errorf("no census for pages: nothing may be said, got %+v", v.Issues)
	}

	// 2. pages was censused, but never used `text`.
	pb := NewCensusBuilder(EntityKindCollection, "pages")
	for i := 1; i <= 4; i++ {
		pb.AddDoc(i, decodeDoc(t, fmt.Sprintf(`{"id":%d,"layout":[{"blockType":"hero","title":"T%d"}]}`, i, i)))
	}
	c.Put(pb.Finish())
	v = ValidateBlocks(page, NewBlockKnowledge(EntityKindCollection, "pages", c, nil))
	for _, is := range v.Issues {
		if is.Code == IssueUnknownField || is.Code == IssueLikelyRequired {
			t.Errorf("a foreign shape was applied: %+v", is)
		}
	}
	// A row that does look like the other entity's rows is still checked.
	k := NewBlockKnowledge(EntityKindCollection, "pages", c, nil)
	v = ValidateBlocks(decodeDoc(t, `{"layout":[{"blockType":"text","name":"x","label":"y","width":50,"required":false,"defaultValue":null,"colour":"red"}]}`), k)
	if _, ok := issueCodes(v)[IssueUnknownField+"@layout[0].colour"]; !ok {
		t.Errorf("a compatible foreign shape still informs the check: %+v", v.Issues)
	}
}

// A census that saw no block row for the entity (it stopped early on a page
// of empty stubs) is no evidence that a real type is unknown.
func TestValidateBlocklessEntityStaysSilent(t *testing.T) {
	c := NewCensus("0123456789abcdef")
	stub := NewCensusEntity(EntityKindCollection, "pages")
	stub.StoppedEarly, stub.Truncated, stub.DocsScanned = true, true, 100
	c.Put(stub)
	other := NewCensusBuilder(EntityKindCollection, "ctas")
	other.AddDoc(1, decodeDoc(t, `{"id":1,"blocks":[{"blockType":"text","heading":"x"}]}`))
	c.Put(other.Finish())
	k := NewBlockKnowledge(EntityKindCollection, "pages", c, nil)
	if k.HasObservedBlocks() {
		t.Fatal("a blockless entity has no observed blocks")
	}
	v := ValidateBlocks(decodeDoc(t, `{"layout":[{"blockType":"hero","title":"t"},{"blockType":"text","heading":"h"}]}`), k)
	if len(v.Issues) != 0 {
		t.Errorf("a blockless census produced findings: %+v", v.Issues)
	}
}

// `pay blocks validate --field` places a bare row at the position named, in
// every grammar the flag accepts.
func TestValidateRowsAtNestedPositions(t *testing.T) {
	k := stageKnowledge(t)
	text := func() []any {
		return []any{decodeDoc(t, `{"blockType":"text","heading":"H","textSize":"regular","section":{"contentWidth":"inhalt","backgroundColor":null}}`)}
	}
	for _, field := range []string{"layout[].blocks", "layout.2.blocks", "layout[1].blocks", "layout[type:group[1]].blocks",
		"layout[id:g1].blocks", "group.blocks", "layout[].blocks[].blocks", "grid.blocks"} {
		v := ValidateRowsAt(text(), field, k)
		if v.CheckedRows != 1 || len(v.Issues) != 0 {
			t.Errorf("--field %s: %+v", field, v)
		}
	}
	v := ValidateRowsAt([]any{decodeDoc(t, `{"blockType":"hero","title":"t","content":`+lexicalHello+`}`)}, "layout.1.blocks", k)
	is, ok := issueCodes(v)[IssueBlockTypeNotObserved+"@layout[].blocks[0]"]
	if !ok || !strings.Contains(is.Message, "observed there: text, grid") {
		t.Errorf("a misplaced row is reported with what IS observed there: %+v", v.Issues)
	}
	// A type: selector names the parent, whose own nested field counts.
	if v := ValidateRowsAt(text(), "layout[].blocks[type:grid].blocks", k); len(v.Issues) != 0 {
		t.Errorf("parent-scoped via selector: %+v", v.Issues)
	}
	if got := selectorParentType("layout[type:group[1]].blocks"); got != "group" {
		t.Errorf("selectorParentType = %q", got)
	}
	if got := selectorParentType("layout[2].blocks"); got != "" {
		t.Errorf("selectorParentType = %q", got)
	}
}

// bentoCensus: a group holding text rows and a top-level bento holding
// bentoItems. Both nested fields share the depth pattern `layout[].blocks`.
func bentoCensus(t *testing.T) *BlockKnowledge {
	t.Helper()
	b := NewCensusBuilder(EntityKindCollection, "pages")
	for i := 1; i <= 3; i++ {
		b.AddDoc(i, decodeDoc(t, fmt.Sprintf(`{"id":%d,"layout":[
		  {"blockType":"group","blocks":[{"blockType":"text","heading":"G%d"}]},
		  {"blockType":"bento","blocks":[{"blockType":"bentoItem","heading":"B%d"}]}]}`, i, i, i)))
	}
	c := NewCensus("0123456789abcdef")
	c.Put(b.Finish())
	return NewBlockKnowledge(EntityKindCollection, "pages", c, nil)
}

// Regression: a type observed only in bento.blocks placed in a group's blocks
// passed silently, because the depth pattern is the union of both parents.
func TestValidateNestedUsesTheParentScopedSet(t *testing.T) {
	k := bentoCensus(t)
	doc := decodeDoc(t, `{"layout":[{"blockType":"group","blocks":[{"blockType":"text","heading":"x"},
	  {"blockType":"bentoItem","heading":"misplaced"}]},{"blockType":"bento","blocks":[{"blockType":"bentoItem","heading":"ok"}]}]}`)
	got := issueCodes(ValidateBlocks(doc, k))
	is, ok := got[IssueBlockTypeNotObserved+"@layout[0].blocks[1]"]
	if !ok || !strings.Contains(is.Message, "group.blocks") || !strings.Contains(is.Message, "observed there: text") {
		t.Errorf("bentoItem in group.blocks must be reported against group.blocks: %+v", got)
	}
	if len(got) != 1 {
		t.Errorf("only the misplaced row is a finding: %+v", got)
	}
	// Every way of naming the position gives the same answer.
	row := func() []any { return []any{decodeDoc(t, `{"blockType":"bentoItem","heading":"h"}`)} }
	for _, field := range []string{"group.blocks", "layout[type:group[0]].blocks", "layout[type:group].blocks"} {
		v := ValidateRowsAt(row(), field, k)
		if len(v.Issues) != 1 || v.Issues[0].Code != IssueBlockTypeNotObserved {
			t.Errorf("--field %s: %+v", field, v.Issues)
		}
	}
	// A bento's own field still accepts it.
	if v := ValidateRowsAt(row(), "layout[type:bento].blocks", k); len(v.Issues) != 0 {
		t.Errorf("bento.blocks: %+v", v.Issues)
	}
}

// Regression: one stray blockType on a row of a plain array (hero.buttons)
// made the validator treat the whole array as blocks and fail the untouched
// real rows with missing_block_type (exit 5 on `pay blocks validate`).
func TestValidatePlainArrayWithAStrayBlockType(t *testing.T) {
	b := NewCensusBuilder(EntityKindCollection, "pages")
	for i := 1; i <= 3; i++ {
		b.AddDoc(i, decodeDoc(t, fmt.Sprintf(`{"id":%d,"layout":[{"blockType":"hero","title":"T%d",
		  "buttons":[{"id":"b%d","label":"Go","url":"/x"}]}]}`, i, i, i)))
	}
	c := NewCensus("0123456789abcdef")
	c.Put(b.Finish())
	k := NewBlockKnowledge(EntityKindCollection, "pages", c, nil)
	doc := decodeDoc(t, `{"layout":[{"blockType":"hero","title":"T","buttons":[
	  {"id":"b1","label":"Go","url":"/x"},{"blockType":"button","label":"x"}]}]}`)
	v := ValidateBlocks(doc, k)
	got := issueCodes(v)
	if v.Errors != 0 {
		t.Errorf("the real button row is fine; no error may be reported: %+v", v.Issues)
	}
	if _, ok := got[IssueUnknownField+"@layout[0].buttons[1].blockType"]; !ok {
		t.Errorf("the stray key is the finding: %+v", v.Issues)
	}
	// A field the census never saw filled is still judged by its content.
	fresh := decodeDoc(t, `{"layout":[{"blockType":"hero","title":"T","buttons":[],"items":[{"blockType":"text"},{}]}]}`)
	if _, ok := issueCodes(ValidateBlocks(fresh, k))[IssueEmptyRow+"@layout[0].items[1]"]; !ok {
		t.Errorf("an unknown array with typed rows is still validated as blocks: %+v", ValidateBlocks(fresh, k).Issues)
	}
}
