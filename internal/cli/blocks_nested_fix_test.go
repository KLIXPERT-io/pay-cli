package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// cp / add keep reference ids (relationship, upload, Lexical nodes)
// ---------------------------------------------------------------------------

// richRowDoc has the shape of klixpert stage pages 35 read with --depth 0: a
// content row whose rich text holds a mediaBlock node with its own fields.id
// and an upload that Payload populates even at depth 0, plus a populated
// upload field directly on the row.
const richRowDoc = `{"id":35,"layout":[
 {"id":"r0","blockType":"content","media":{"id":848,"filename":"a.png","createdAt":"x","updatedAt":"y"},
  "columns":[{"id":"c0","richText":{"root":{"type":"root","children":[
    {"type":"block","version":2,"fields":{"id":"6aba78573fd56967ea7d3aec","blockType":"mediaBlock","blockName":"",
      "media":{"id":848,"filename":"a.png","createdAt":"x","folder":{"id":7,"createdAt":"x"}}}},
    {"type":"upload","id":"6ab000000000000000000001","relationTo":"media","value":{"id":12,"createdAt":"x"}}]}}}]}]}`

func assertReferencesKept(t *testing.T, row any) {
	t.Helper()
	if id := at(t, row, "id"); id != nil {
		t.Errorf("the new row kept its row id %v", id)
	}
	if id := at(t, row, "columns", 0, "id"); id != nil {
		t.Errorf("the new row kept a nested row id %v", id)
	}
	if got := at(t, row, "media", "id"); got == nil || got.(json.Number).String() != "848" {
		t.Errorf("the populated upload lost its id: %v", at(t, row, "media"))
	}
	children := at(t, row, "columns", 0, "richText", "root", "children")
	if got := at(t, children, 0, "fields", "id"); got != "6aba78573fd56967ea7d3aec" {
		t.Errorf("the lexical block node lost fields.id: %v", got)
	}
	if got := at(t, children, 0, "fields", "media", "id"); got == nil || got.(json.Number).String() != "848" {
		t.Errorf("the lexical block's upload lost its id: %v", got)
	}
	if got := at(t, children, 1, "id"); got != "6ab000000000000000000001" {
		t.Errorf("the lexical upload node lost its id: %v", got)
	}
}

func TestBlocksCpKeepsReferenceAndLexicalIDs(t *testing.T) {
	r := cliRun(t, invocation{Args: []string{"blocks", "cp", "id:r0", "--last", "--field", "layout"},
		Stdin: pageEnvelope(t, richRowDoc)})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	assertReferencesKept(t, at(t, docOf(t, r), "layout", 1))
	if at(t, docOf(t, r), "layout", 0, "id") != "r0" {
		t.Error("the original row lost its id")
	}
}

func TestBlocksAddDataKeepsReferenceAndLexicalIDs(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(richRowDoc), &doc); err != nil {
		t.Fatal(err)
	}
	row, _ := json.Marshal(doc["layout"].([]any)[0])
	file := filepath.Join(t.TempDir(), "row.json")
	if err := os.WriteFile(file, row, 0o600); err != nil {
		t.Fatal(err)
	}
	r := cliRun(t, invocation{Args: []string{"blocks", "add", "content", "--data", "@" + file, "--field", "layout"},
		Stdin: pageEnvelope(t, richRowDoc)})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	assertReferencesKept(t, at(t, docOf(t, r), "layout", 1))
}

// ---------------------------------------------------------------------------
// a null at the end of a nested path
// ---------------------------------------------------------------------------

// `layout[2].blocks[0].heading` is null: a text field that is not set, not an
// empty blocks field. Before the fix `ls` answered count 0 and `add`
// replaced the scalar with an array of rows, silently.
func TestBlocksNullNestedScalarIsNotAnArray(t *testing.T) {
	for _, args := range [][]string{
		{"blocks", "ls", "--field", "layout[2].blocks[0].heading"},
		{"blocks", "add", "note", "--set", "text=hi", "--field", "layout[2].blocks[0].heading"},
	} {
		r := cliRun(t, invocation{Args: args, Stdin: nestedEnvelope(t)})
		if r.Code != 5 || r.code(t) != "invalid_args" {
			t.Errorf("%v: exit %d code %q, want invalid_args exit 5\n%s", args, r.Code, r.code(t), r.Stdout)
			continue
		}
		e := errOf(t, r)
		if msg, _ := e["message"].(string); !strings.Contains(msg, "is null") {
			t.Errorf("%v: message = %q", args, msg)
		}
		if dym := toStrings(e["did_you_mean"]); !contains(dym, "layout[id:g2].blocks[id:x0].buttons") {
			t.Errorf("%v: did_you_mean = %v, want the row's arrays", args, dym)
		}
	}
}

// A null key that other rows of the same blockType carry as an array IS a
// known, empty field: it stays fillable, with the field_absent warning.
func TestBlocksNullNestedKnownArrayIsEmpty(t *testing.T) {
	doc := `{"id":2,"layout":[
	  {"id":"t0","blockType":"text","buttons":[{"id":"b0","label":"Go"}]},
	  {"id":"t1","blockType":"text","buttons":null}]}`
	r := cliRun(t, invocation{Args: []string{"blocks", "ls", "--field", "layout[1].buttons"}, Stdin: pageEnvelope(t, doc)})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	if !r.hasWarning(t, WarnFieldAbsent) {
		t.Error("a null known array must say it is being treated as empty")
	}
}

// ---------------------------------------------------------------------------
// an absent key inside a present group
// ---------------------------------------------------------------------------

func TestBlocksGroupKeyTypoWithSchema(t *testing.T) {
	home := t.TempDir()
	seedNestedSchema(t, home, testBaseURL)
	env := seededEnv(testBaseURL)
	doc := `{"id":2,"hero":{"type":"lowImpact","links":[{"id":"l0","link":{"url":"/a"}}],"media":null},"layout":[]}`

	r := cliRun(t, invocation{Home: home, Env: env, Args: []string{"blocks", "ls", "--field", "hero.linsk"},
		Stdin: pageEnvelope(t, doc)})
	if r.Code != 5 || r.code(t) != "unknown_field" {
		t.Fatalf("hero.linsk: exit %d code %q, want unknown_field\n%s", r.Code, r.code(t), r.Stdout)
	}
	if dym := toStrings(errOf(t, r)["did_you_mean"]); !contains(dym, "hero.links") {
		t.Errorf("did_you_mean = %v, want hero.links", dym)
	}

	// A sub-field the schema knows as something other than an array.
	r = cliRun(t, invocation{Home: home, Env: env, Args: []string{"blocks", "ls", "--field", "hero.media"},
		Stdin: pageEnvelope(t, doc)})
	if r.Code != 5 || r.code(t) != "invalid_args" {
		t.Errorf("hero.media: exit %d code %q, want invalid_args", r.Code, r.code(t))
	}

	// A known array the read left out (e.g. --select hero.type) stays fillable.
	r = cliRun(t, invocation{Home: home, Env: env, Args: []string{"blocks", "ls", "--field", "hero.links"},
		Stdin: pageEnvelope(t, `{"id":2,"hero":{"type":"lowImpact"}}`)})
	if r.Code != 0 || !r.hasWarning(t, WarnFieldAbsent) {
		t.Errorf("hero.links absent: exit %d, want 0 with field_absent\n%s", r.Code, r.Stdout)
	}
}

// Without a schema the legacy answer stands, but the hint names the arrays the
// group does carry instead of only blaming a --select.
func TestBlocksGroupKeyAbsentWithoutSchema(t *testing.T) {
	doc := `{"id":2,"hero":{"type":"lowImpact","links":[]}}`
	r := cliRun(t, invocation{Args: []string{"blocks", "ls", "--field", "hero.linsk"}, Stdin: pageEnvelope(t, doc)})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	var hint string
	for _, w := range r.warnings(t) {
		if w["code"] == WarnFieldAbsent {
			hint, _ = w["hint"].(string)
		}
	}
	if !strings.Contains(hint, "hero.links") {
		t.Errorf("field_absent hint = %q, want it to name hero.links", hint)
	}
}

// ---------------------------------------------------------------------------
// add on a plain array field
// ---------------------------------------------------------------------------

const cardButtons = "layout[id:g2].blocks[id:r1].blocks[id:c0].buttons"

func TestBlocksAddOnPlainArrayOmitsBlockType(t *testing.T) {
	r := cliRun(t, invocation{Args: []string{"blocks", "add", "button", "--set", "label=Los", "--field", cardButtons},
		Stdin: nestedEnvelope(t)})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	if !r.hasWarning(t, WarnBlockTypeOmitted) {
		t.Error("a blockType given for a plain array field must be reported as omitted")
	}
	row := at(t, docOf(t, r), "layout", 2, "blocks", 1, "blocks", 0, "buttons", 1)
	if bt := at(t, row, "blockType"); bt != nil {
		t.Errorf("the array row got blockType %v", bt)
	}
	if at(t, row, "label") != "Los" {
		t.Errorf("row = %v", row)
	}
}

// `-` adds an array row; on an EMPTY array (the first button) the kind comes
// from other rows of the same blockType in the document.
func TestBlocksAddDashOnEmptyArray(t *testing.T) {
	r := cliRun(t, invocation{Args: []string{"blocks", "add", "-", "--set", "label=Erster", "--set", "url=/x",
		"--field", "layout[id:g2].blocks[id:r1].blocks[id:c1].buttons"}, Stdin: nestedEnvelope(t)})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	if r.hasWarning(t, WarnBlockTypeOmitted) {
		t.Error("`-` must not warn")
	}
	row := at(t, docOf(t, r), "layout", 2, "blocks", 1, "blocks", 1, "buttons", 0)
	if bt := at(t, row, "blockType"); bt != nil {
		t.Errorf("the array row got blockType %v", bt)
	}
	if at(t, row, "label") != "Erster" || at(t, row, "url") != "/x" {
		t.Errorf("row = %v", row)
	}
}

func TestBlocksAddDashRefusedOnBlocksField(t *testing.T) {
	r := cliRun(t, invocation{Args: []string{"blocks", "add", "-", "--field", "layout[id:g1].blocks"}, Stdin: nestedEnvelope(t)})
	if r.Code != 5 || r.code(t) != "invalid_args" {
		t.Errorf("exit %d code %q, want invalid_args exit 5\n%s", r.Code, r.code(t), r.Stdout)
	}
	r = cliRun(t, invocation{Args: []string{"blocks", "add", "-", "--template", "--field", cardButtons}, Stdin: nestedEnvelope(t)})
	if r.Code != 5 || r.code(t) != "invalid_args" {
		t.Errorf("--template on an array: exit %d code %q, want invalid_args", r.Code, r.code(t))
	}
}

// ---------------------------------------------------------------------------
// ls --recursive: per-array kind
// ---------------------------------------------------------------------------

func TestBlocksLsRecursiveArrayKinds(t *testing.T) {
	r := cliRun(t, invocation{Args: []string{"blocks", "ls", "--recursive"}, Stdin: nestedEnvelope(t)})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	kinds := map[string]string{}
	for _, n := range r.data(t)["rows"].([]any) {
		arrays, _ := at(t, n, "arrays").(map[string]any)
		for rel, info := range arrays {
			kinds[at(t, n, "path").(string)+"."+rel] = at(t, info, "kind").(string)
		}
	}
	want := map[string]string{
		"layout[1].blocks":                      "blocks",
		"layout[2].blocks[1].blocks":            "blocks",
		"layout[2].blocks[1].blocks[0].buttons": "array",
		// empty, but other text rows carry buttons without a blockType
		"layout[2].blocks[1].blocks[1].buttons": "array",
	}
	for k, v := range want {
		if kinds[k] != v {
			t.Errorf("%s kind = %q, want %q (all: %v)", k, kinds[k], v, kinds)
		}
	}
}

// ---------------------------------------------------------------------------
// LOW: error messages and stale positions
// ---------------------------------------------------------------------------

func TestBlocksNestedErrorMessages(t *testing.T) {
	// (a) an anchor miss names the nested array.
	r := cliRun(t, invocation{Args: []string{"blocks", "mv", "first", "--after", "id:nope", "--field", "layout[id:g1].blocks"},
		Stdin: nestedEnvelope(t)})
	if msg, _ := errOf(t, r)["message"].(string); !strings.Contains(msg, "layout[1].blocks has no row") {
		t.Errorf("anchor miss message = %q", msg)
	}

	// (b) an id that lives in another array points at its --field.
	r = cliRun(t, invocation{Args: []string{"blocks", "rm", "id:s1", "--field", "layout"}, Stdin: nestedEnvelope(t)})
	if hint, _ := errOf(t, r)["hint"].(string); !strings.Contains(hint, "layout[id:g1].blocks") {
		t.Errorf("id in another array: hint = %q", hint)
	}

	// (c) type:X[n] out of range suggests the n that exist, never the ambiguous type:X.
	r = cliRun(t, invocation{Args: []string{"blocks", "ls", "--field", "layout[type:group[9]].blocks"}, Stdin: nestedEnvelope(t)})
	dym := toStrings(errOf(t, r)["did_you_mean"])
	if contains(dym, "type:group") || !contains(dym, "type:group[2]") {
		t.Errorf("type:group[9]: did_you_mean = %v", dym)
	}

	// (d) the dotted type:SLUG[N] form works.
	r = cliRun(t, invocation{Args: []string{"blocks", "ls", "--field", "layout.type:group[1].blocks"}, Stdin: nestedEnvelope(t)})
	if r.Code != 0 || r.data(t)["field"] != "layout[id:g2].blocks" {
		t.Errorf("layout.type:group[1].blocks: exit %d %s", r.Code, r.Stdout)
	}

	// (e) a typo in a middle key is unknown_field with a suggestion.
	r = cliRun(t, invocation{Args: []string{"blocks", "ls", "--field", "layout[2].blokcs[1].blocks"}, Stdin: nestedEnvelope(t)})
	if r.code(t) != "unknown_field" || !contains(toStrings(errOf(t, r)["did_you_mean"]), "layout[id:g2].blocks") {
		t.Errorf("middle-key typo: code %q %s", r.code(t), r.Stdout)
	}
}

func TestBlocksLsRecursiveBareRowsHaveNoField(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(nestedDoc), &doc); err != nil {
		t.Fatal(err)
	}
	bare, _ := json.Marshal(doc["layout"])
	r := cliRun(t, invocation{Args: []string{"blocks", "ls", "--recursive"}, Stdin: string(bare)})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	for _, n := range r.data(t)["rows"].([]any) {
		if f, ok := n.(map[string]any)["field"]; ok {
			t.Errorf("bare row %v carries field %v, which no --field accepts", at(t, n, "path"), f)
		}
	}
}

func TestBlocksStaleIndexWarning(t *testing.T) {
	up := cliRun(t, invocation{Args: []string{"blocks", "rm", "0", "--field", "layout"}, Stdin: nestedEnvelope(t)})
	if up.Code != 0 {
		t.Fatalf("rm: exit %d: %s", up.Code, up.Stdout)
	}
	r := cliRun(t, invocation{Args: []string{"blocks", "set", "first", "--set", "phase=X", "--field", "layout.0.blocks"}, Stdin: up.Stdout})
	if r.Code != 0 {
		t.Fatalf("set: exit %d: %s", r.Code, r.Stdout)
	}
	if !r.hasWarning(t, WarnStaleIndex) {
		t.Error("a positional nested --field after an upstream rm on the same array must warn stale_index")
	}
	// The id-based form does not warn.
	r = cliRun(t, invocation{Args: []string{"blocks", "set", "first", "--set", "phase=X", "--field", "layout[id:g1].blocks"}, Stdin: up.Stdout})
	if r.Code != 0 || r.hasWarning(t, WarnStaleIndex) {
		t.Errorf("id-based field: exit %d, stale_index %v", r.Code, r.hasWarning(t, WarnStaleIndex))
	}
}

func TestBlocksAddEmptyArrayRowRefused(t *testing.T) {
	for _, typ := range []string{"-", "bento"} {
		r := cliRun(t, invocation{Args: []string{"blocks", "add", typ, "--field", cardButtons}, Stdin: nestedEnvelope(t)})
		if r.Code != 5 || r.code(t) != "invalid_args" {
			t.Errorf("add %s with no fields: exit %d code %q, want invalid_args", typ, r.Code, r.code(t))
		}
	}
}
