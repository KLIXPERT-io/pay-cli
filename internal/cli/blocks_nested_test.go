package cli

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// nestedDoc has the shape of klixpert stage pages 2 and 47 (read with
// --draft --depth 0): a `group` whose `blocks` hold processStep rows, and a
// second group holding a `grid` whose `blocks` hold text rows with `buttons`.
// Stage publishes no block schema (GraphQL introspection is off), so every
// nested path here must resolve from the document's own shape.
const nestedDoc = `{"id":2,"title":"Prozess","slug":"prozess","_status":"draft",
 "createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-02T00:00:00Z",
 "layout":[
  {"id":"h0","blockType":"hero","buttons":[]},
  {"id":"g1","blockType":"group","blocks":[
     {"id":"s0","blockType":"processStep","phase":"Plan"},
     {"id":"s1","blockType":"processStep","phase":"Build"},
     {"id":"s2","blockType":"processStep","phase":"Run"}]},
  {"id":"g2","blockType":"group","blocks":[
     {"id":"x0","blockType":"text","heading":null,"buttons":[]},
     {"id":"r1","blockType":"grid","blocks":[
        {"id":"c0","blockType":"text","buttons":[{"id":"b0","label":"Go","url":"/go"}]},
        {"id":"c1","blockType":"text","buttons":[]}]}]},
  {"id":"g3","blockType":"group"}
 ]}`

func nestedEnvelope(t *testing.T) string {
	t.Helper()
	return pageEnvelope(t, nestedDoc)
}

// docOf decodes a stage's document back into a value, for structural checks.
func docOf(t *testing.T, r cliResult) map[string]any {
	t.Helper()
	return r.data(t)
}

// at walks a decoded document by a list of keys and indices.
func at(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("at %v: %T is not an object", path, v)
			}
			v = m[k]
		case int:
			l, ok := v.([]any)
			if !ok || k >= len(l) {
				t.Fatalf("at %v: no index %d in %T", path, k, v)
			}
			v = l[k]
		}
	}
	return v
}

func rowIDs(t *testing.T, v any) []string {
	t.Helper()
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, r := range list {
		m, _ := r.(map[string]any)
		id, _ := m["id"].(string)
		out = append(out, id)
	}
	return out
}

func errOf(t *testing.T, r cliResult) map[string]any {
	t.Helper()
	e, _ := r.Env["error"].(map[string]any)
	if e == nil {
		t.Fatalf("expected an error envelope, got exit %d: %s", r.Code, r.Stdout)
	}
	return e
}

// ---------------------------------------------------------------------------
// ls
// ---------------------------------------------------------------------------

func TestBlocksLsNestedFieldEveryForm(t *testing.T) {
	for _, field := range []string{
		"layout.1.blocks", "layout[1].blocks", "layout[id:g1].blocks",
		"layout[type:group[0]].blocks", "layout[name:nope].blocks",
	} {
		r := cliRun(t, invocation{Args: []string{"blocks", "ls", "--field", field}, Stdin: nestedEnvelope(t)})
		if field == "layout[name:nope].blocks" {
			if r.Code != 4 || r.code(t) != "selector_no_match" {
				t.Errorf("%s: exit %d code %q, want selector_no_match exit 4", field, r.Code, r.code(t))
			}
			continue
		}
		if r.Code != 0 {
			t.Fatalf("%s: exit %d: %s", field, r.Code, r.Stdout)
		}
		d := r.data(t)
		if d["field"] != "layout[id:g1].blocks" || d["path"] != "layout[1].blocks" {
			t.Errorf("%s: field=%v path=%v", field, d["field"], d["path"])
		}
		if got, _ := d["count"].(json.Number); got.String() != "3" {
			t.Errorf("%s: count = %v", field, d["count"])
		}
		if r.hasWarning(t, WarnFieldAbsent) {
			t.Errorf("%s: a resolved nested field must not warn field_absent", field)
		}
	}
}

// The reported bug: `--field layout.2.blocks` answered field_absent + an empty
// list, so an agent believed the group was empty.
func TestBlocksLsNestedNeverSilentlyEmpty(t *testing.T) {
	cases := []struct {
		field string
		exit  int
		code  string
		dym   string // a did_you_mean entry that must be offered
	}{
		{"layout.9.blocks", 4, "selector_no_match", ""},
		{"layout[1].blokcs", 5, "unknown_field", "layout[id:g1].blocks"},
		{"layout.blocks", 5, "invalid_args", "layout[id:g1].blocks"},
		{"layout[1]", 5, "invalid_args", "layout[id:g1].blocks"},
		{"layuot.1.blocks", 4, "selector_no_match", ""},
		{"layout[type:group].blocks", 5, "selector_ambiguous", "id:g1"},
		{"layout[2].blocks[0].heading.x", 5, "invalid_args", ""},
		{"layout[1].blocks[0].buttons", 5, "unknown_field", ""},
		{"layout[1", 5, "invalid_args", ""},
	}
	for _, c := range cases {
		r := cliRun(t, invocation{Args: []string{"blocks", "ls", "--field", c.field}, Stdin: nestedEnvelope(t)})
		if r.Code != c.exit || r.code(t) != c.code {
			t.Errorf("%s: exit %d code %q, want exit %d %s\n%s", c.field, r.Code, r.code(t), c.exit, c.code, r.Stdout)
			continue
		}
		if c.dym != "" {
			dym := toStrings(errOf(t, r)["did_you_mean"])
			if !contains(dym, c.dym) {
				t.Errorf("%s: did_you_mean = %v, want it to offer %s", c.field, dym, c.dym)
			}
		}
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// A key that is absent on ONE row but carried as an array by another row of
// the same blockType is a known field that is merely empty: the one group with
// no `blocks` key can still be filled.
func TestBlocksAbsentNestedFieldKnownFromSiblings(t *testing.T) {
	r := cliRun(t, invocation{Args: []string{"blocks", "add", "text", "--set", "heading=Neu",
		"--field", "layout[id:g3].blocks"}, Stdin: nestedEnvelope(t)})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	if !r.hasWarning(t, WarnFieldAbsent) {
		t.Error("filling an absent nested field must say it was absent")
	}
	got := at(t, docOf(t, r), "layout", 3, "blocks")
	if list, _ := got.([]any); len(list) != 1 {
		t.Fatalf("layout[3].blocks = %v", got)
	}
	if at(t, got, 0, "heading") != "Neu" || at(t, got, 0, "blockType") != "text" {
		t.Errorf("row = %v", at(t, got, 0))
	}
}

// With a discovered block schema the same absent key is known from it, and an
// unknown one is still an error.
func TestBlocksAbsentNestedFieldKnownFromSchema(t *testing.T) {
	home := t.TempDir()
	seedNestedSchema(t, home, testBaseURL)
	doc := `{"id":2,"layout":[{"id":"g1","blockType":"section"}]}`
	env := seededEnv(testBaseURL)

	r := cliRun(t, invocation{Home: home, Env: env, Args: []string{"blocks", "ls", "--field", "layout[0].items"},
		Stdin: pageEnvelope(t, doc)})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	var msg string
	for _, w := range r.warnings(t) {
		if w["code"] == WarnFieldAbsent {
			msg, _ = w["message"].(string)
		}
	}
	if !strings.Contains(msg, "schema declares it") {
		t.Errorf("field_absent message = %q; it must name the schema as its source", msg)
	}

	r = cliRun(t, invocation{Home: home, Env: env, Args: []string{"blocks", "ls", "--field", "layout[0].itmes"},
		Stdin: pageEnvelope(t, doc)})
	if r.code(t) != "unknown_field" {
		t.Errorf("a key the schema does not know = %q, want unknown_field", r.code(t))
	}
}

// seedNestedSchema caches a pages shard with a `section` block whose schema
// declares an `items` blocks field.
func seedNestedSchema(t *testing.T, home, baseURL string) {
	t.Helper()
	sc, err := cache.NewScope(cache.ScopeInput{
		BaseURL: baseURL, APIPath: "/api", GraphQLPath: "/api/graphql", KeyFingerprint: cache.AnonKeyFingerprint,
	})
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	var m discovery.Manifest
	payloadtest.LoadJSON(t, "manifest", &m)
	var shard discovery.Shard
	payloadtest.LoadJSON(t, "fields_pages", &shard)
	shard.SetBlockField(discovery.BlockField{Path: "layout", Slugs: []string{"section"}, Source: discovery.SourcePayloadProtocol})
	shard.BlockSchemas = map[string]discovery.BlockTypeSchema{
		"section": {Slug: "section", Fields: []discovery.BlockFieldSchema{
			{Name: "items", Path: "items", PayloadType: discovery.TypeBlocks},
		}},
	}
	now := payloadtest.Epoch
	generation := cache.NewGeneration(now)
	m.Generation, m.Meta.Scope, m.GeneratedAt, m.ExpiresAt = generation, sc.Key, now, now.Add(24*time.Hour)
	shard.Generation = generation
	store := cache.New(filepath.Join(home, "cache"))
	if ok, warns := store.WriteSet(sc, cache.Set{
		Generation: generation, Manifest: &m,
		Shards: map[string]any{cache.ShardName(shard.Slug, cache.KindCollection): &shard},
	}, now); !ok {
		t.Fatalf("seed: %v", warns)
	}
}

func TestBlocksLsRecursive(t *testing.T) {
	r := cliRun(t, invocation{Args: []string{"blocks", "ls", "--recursive"}, Stdin: nestedEnvelope(t)})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	d := r.data(t)
	if got, _ := d["total"].(json.Number); got.String() != "12" {
		t.Errorf("total = %v, want 12 rows at every depth", d["total"])
	}
	nodes, _ := d["rows"].([]any)
	var paths []string
	for _, n := range nodes {
		paths = append(paths, at(t, n, "path").(string))
	}
	want := []string{
		"layout[0]", "layout[1]", "layout[1].blocks[0]", "layout[1].blocks[1]", "layout[1].blocks[2]",
		"layout[2]", "layout[2].blocks[0]", "layout[2].blocks[1]", "layout[2].blocks[1].blocks[0]",
		"layout[2].blocks[1].blocks[0].buttons[0]", "layout[2].blocks[1].blocks[1]", "layout[3]",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("paths =\n%v\nwant\n%v", paths, want)
	}

	// Every listed (field, selector) pair is pasteable into the next stage.
	for _, n := range nodes {
		field, sel := at(t, n, "field").(string), at(t, n, "selector").(string)
		out := cliRun(t, invocation{Args: []string{"blocks", "set", sel, "--set", "blockName=seen", "--field", field},
			Stdin: nestedEnvelope(t)})
		if out.Code != 0 {
			t.Errorf("set %s --field %s: exit %d %s", sel, field, out.Code, out.Stdout)
		}
	}
	btn := nodes[9]
	if at(t, btn, "field") != "layout[id:g2].blocks[id:r1].blocks[id:c0].buttons" || at(t, btn, "depth") != json.Number("3") {
		t.Errorf("button node = %v", btn)
	}
	if nested := at(t, nodes[1], "nested"); !reflect.DeepEqual(nested, map[string]any{"blocks": json.Number("3")}) {
		t.Errorf("group nested = %v", nested)
	}

	// --long carries each row, and --recursive composes with a nested --field.
	r = cliRun(t, invocation{Args: []string{"blocks", "ls", "-r", "--long", "--field", "layout[id:g2].blocks"}, Stdin: nestedEnvelope(t)})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	nodes, _ = r.data(t)["rows"].([]any)
	if len(nodes) != 5 || at(t, nodes[0], "row", "id") != "x0" {
		t.Errorf("nested recursive listing = %v", nodes)
	}
}

// ---------------------------------------------------------------------------
// edits at depth
// ---------------------------------------------------------------------------

// Every verb edits exactly the nested array, records the concrete path in
// edits.ops and the TOP-LEVEL field in edits.fields — the unit `pay apply`
// writes.
func TestBlocksVerbsAtDepth(t *testing.T) {
	grid := "layout[id:g2].blocks[type:grid].blocks"
	cases := []struct {
		name string
		args []string
		want []string // row ids of the grid afterwards ("" for a new row)
	}{
		{"mv", []string{"blocks", "mv", "last", "--first", "--field", grid}, []string{"c1", "c0"}},
		{"rm", []string{"blocks", "rm", "id:c0", "--field", grid}, []string{"c1"}},
		{"add", []string{"blocks", "add", "text", "--after", "id:c0", "--field", grid}, []string{"c0", "", "c1"}},
		{"cp", []string{"blocks", "cp", "id:c0", "--last", "--field", grid}, []string{"c0", "c1", ""}},
		{"set", []string{"blocks", "set", "id:c1", "--set", "heading=Hi", "--field", grid}, []string{"c0", "c1"}},
	}
	orig := decodeDocT(t, nestedDoc)
	for _, c := range cases {
		r := cliRun(t, invocation{Args: c.args, Stdin: nestedEnvelope(t)})
		if r.Code != 0 {
			t.Fatalf("%s: exit %d: %s", c.name, r.Code, r.Stdout)
		}
		doc := docOf(t, r)
		if got := rowIDs(t, at(t, doc, "layout", 2, "blocks", 1, "blocks")); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: grid ids = %v, want %v", c.name, got, c.want)
		}
		// Nothing outside the grid moved.
		stripped := func(d map[string]any) any {
			cp := deepCopyJSON(t, d)
			delete(at(t, cp, "layout", 2, "blocks", 1).(map[string]any), "blocks")
			return cp
		}
		if !reflect.DeepEqual(stripped(doc), stripped(orig)) {
			t.Errorf("%s: something outside the grid changed", c.name)
		}
		edits, _ := r.Env["edits"].(map[string]any)
		if got := toStrings(edits["fields"]); !reflect.DeepEqual(got, []string{"layout"}) {
			t.Errorf("%s: edits.fields = %v, want [layout]", c.name, got)
		}
		ops, _ := edits["ops"].([]any)
		if len(ops) != 1 || at(t, ops[0], "field") != "layout[2].blocks[1].blocks" {
			t.Errorf("%s: ops = %v", c.name, ops)
		}
	}
	// cp strips ids at depth too.
	r := cliRun(t, invocation{Args: []string{"blocks", "cp", "id:c0", "--field", grid}, Stdin: nestedEnvelope(t)})
	cp := at(t, docOf(t, r), "layout", 2, "blocks", 1, "blocks", 2)
	if _, ok := at(t, cp, "buttons", 0).(map[string]any)["id"]; ok {
		t.Error("the copy kept a nested button id")
	}
}

// The stable field printed by ls survives an earlier stage reordering the
// PARENT array, which is the whole reason it is printed with id: selectors.
func TestBlocksNestedPipelineSurvivesAParentReorder(t *testing.T) {
	ls := cliRun(t, invocation{Args: []string{"blocks", "ls", "--field", "layout[1].blocks"}, Stdin: nestedEnvelope(t)})
	field := ls.data(t)["field"].(string)

	s1 := cliRun(t, invocation{Args: []string{"blocks", "mv", "id:g1", "--last"}, Stdin: nestedEnvelope(t)})
	s2 := cliRun(t, invocation{Args: []string{"blocks", "rm", "first", "--field", field}, Stdin: stageOut(t, s1)})
	s3 := cliRun(t, invocation{Args: []string{"blocks", "set", "first", "--set", "phase=Neu", "--field", field}, Stdin: stageOut(t, s2)})
	if s3.Code != 0 {
		t.Fatalf("exit %d: %s", s3.Code, s3.Stdout)
	}
	doc := docOf(t, s3)
	if got := rowIDs(t, at(t, doc, "layout", 3, "blocks")); !reflect.DeepEqual(got, []string{"s1", "s2"}) {
		t.Errorf("group rows = %v", got)
	}
	if at(t, doc, "layout", 3, "blocks", 0, "phase") != "Neu" {
		t.Error("the set did not land on the moved group's first row")
	}
	edits, _ := s3.Env["edits"].(map[string]any)
	if got := toStrings(edits["fields"]); !reflect.DeepEqual(got, []string{"layout"}) {
		t.Errorf("edits.fields = %v", got)
	}
	if ops, _ := edits["ops"].([]any); len(ops) != 3 {
		t.Errorf("ops = %v", ops)
	}
}

// --set keys walk arrays by row and never replace them.
func TestBlocksSetKeysThroughArrays(t *testing.T) {
	grid := "layout[id:g2].blocks[type:grid].blocks"
	r := cliRun(t, invocation{Args: []string{"blocks", "set", "id:c0", "--set", "buttons[id:b0].label=Los",
		"--set", "buttons.0.style=fill", "--field", grid}, Stdin: nestedEnvelope(t)})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	btn := at(t, docOf(t, r), "layout", 2, "blocks", 1, "blocks", 0, "buttons", 0)
	if at(t, btn, "label") != "Los" || at(t, btn, "style") != "fill" || at(t, btn, "id") != "b0" {
		t.Errorf("button = %v", btn)
	}

	r = cliRun(t, invocation{Args: []string{"blocks", "set", "id:c0", "--set", "buttons.3.label=x", "--field", grid},
		Stdin: nestedEnvelope(t)})
	if r.Code != 4 || r.code(t) != "selector_no_match" {
		t.Errorf("a button that does not exist: exit %d %q", r.Code, r.code(t))
	}
	if msg, _ := errOf(t, r)["message"].(string); !strings.Contains(msg, "layout[2].blocks[1].blocks[0].buttons") {
		t.Errorf("the message must name the full location: %s", msg)
	}

	r = cliRun(t, invocation{Args: []string{"blocks", "set", "id:s0", "--set", "links.0.url=/x", "--field", "layout[1].blocks"},
		Stdin: nestedEnvelope(t)})
	if r.code(t) != "selector_no_match" {
		t.Errorf("a row into an absent array = %q, want selector_no_match", r.code(t))
	}
}

// `add --set link.url=…` used to store a literal "link.url" key, which Payload
// drops.
func TestBlocksAddExpandsPathKeys(t *testing.T) {
	r := cliRun(t, invocation{Args: []string{"blocks", "add", "cta", "--set", "link.url=/x",
		"--set-json", `buttons=[{"label":"a"}]`, "--set", "buttons.0.style=fill"}, Stdin: pageDoc})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	row := at(t, docOf(t, r), "layout", 3).(map[string]any)
	if _, literal := row["link.url"]; literal {
		t.Fatal("a dotted --set key was stored literally")
	}
	if at(t, row, "link", "url") != "/x" || at(t, row, "buttons", 0, "style") != "fill" || at(t, row, "buttons", 0, "label") != "a" {
		t.Errorf("row = %v", row)
	}
}

// ---------------------------------------------------------------------------
// apply
// ---------------------------------------------------------------------------

// A nested edit is written as the whole top-level array, and nothing else.
func TestApplyWritesANestedEditAsTheTopLevelField(t *testing.T) {
	home := t.TempDir()
	srv := payloadtest.NewServer(t)
	seedBlocksDiscovery(t, home, srv.URL, []string{"hero", "group"})

	r := applyPipe(t, home, srv.URL,
		[]string{"blocks", "mv", "last", "--first", "--field", "layout[id:g1].blocks"},
		[]string{"apply", "--dry-run"},
		nestedEnvelope(t))
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	body := dryRunBody(t, r)
	if keys := sortedMapKeys(body); !reflect.DeepEqual(keys, []string{"layout"}) {
		t.Fatalf("the PATCH carries %v, want exactly layout", keys)
	}
	if got := rowIDs(t, at(t, body, "layout", 1, "blocks")); !reflect.DeepEqual(got, []string{"s2", "s0", "s1"}) {
		t.Errorf("nested order in the body = %v", got)
	}
	if got := rowIDs(t, body["layout"]); !reflect.DeepEqual(got, []string{"h0", "g1", "g2", "g3"}) {
		t.Errorf("top-level rows in the body = %v", got)
	}
}

func TestApplyRefusesANestedFieldFlag(t *testing.T) {
	home := t.TempDir()
	srv := payloadtest.NewServer(t)
	seedBlocksDiscovery(t, home, srv.URL, []string{"hero", "group"})

	r := applyPipe(t, home, srv.URL,
		[]string{"blocks", "rm", "first", "--field", "layout[1].blocks"},
		[]string{"apply", "--dry-run", "--field", "layout[1].blocks"},
		nestedEnvelope(t))
	if r.code(t) != "invalid_args" {
		t.Fatalf("code = %q: %s", r.code(t), r.Stdout)
	}
	if hint, _ := errOf(t, r)["hint"].(string); !strings.Contains(hint, "--field layout") {
		t.Errorf("hint = %q; it must name the top-level field to write", hint)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func decodeDocT(t *testing.T, s string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return v
}

func deepCopyJSON(t *testing.T, v map[string]any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return decodeDocT(t, string(b))
}
