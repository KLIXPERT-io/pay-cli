package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// pageDoc is a bare Payload document: no envelope around it, so the field has
// to be found from the document's own shape.
const pageDoc = `{"id":12,"title":"Home","slug":"home","layout":[
  {"id":"a1","blockType":"cta","blockName":"Top CTA"},
  {"id":"b2","blockType":"content"},
  {"id":"c3","blockType":"mediaBlock","media":7}
]}`

// pageEnvelope is what `pay get pages 12` actually writes: the document inside
// an envelope that names the collection and the id, which is what makes
// `pay apply` need no arguments.
func pageEnvelope(t *testing.T, doc string) string {
	t.Helper()
	var d any
	if err := json.Unmarshal([]byte(doc), &d); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	env := map[string]any{
		"ok": true, "v": 1, "command": "get", "data_kind": "doc",
		"target":   map[string]any{"kind": "collection", "slug": "pages", "id": 12},
		"data":     d,
		"meta":     map[string]any{"request_id": "t", "dry_run": false},
		"warnings": []any{map[string]any{"code": "upstream_thing", "message": "carried forward"}},
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return string(b)
}

// stageOut re-serialises one stage's envelope so it can be piped into the next,
// which is what the shell does between two `pay` processes.
func stageOut(t *testing.T, r cliResult) string {
	t.Helper()
	if r.Env == nil {
		t.Fatalf("stage produced no envelope (exit %d):\n%s\n%s", r.Code, r.Stdout, r.Stderr)
	}
	return r.Stdout
}

// seedBlocksDiscovery seeds the cache like seedDiscovery, but declares
// `pages.layout` as a blocks field. The recorded fixture has `blocks: null` —
// the live project's slugs were unresolvable when it was captured — and the
// schema-backed half of this family cannot be tested without one.
func seedBlocksDiscovery(t *testing.T, home, baseURL string, slugs []string) cache.Scope {
	t.Helper()
	sc, err := cache.NewScope(cache.ScopeInput{
		BaseURL:        baseURL,
		APIPath:        "/api",
		GraphQLPath:    "/api/graphql",
		KeyFingerprint: cache.AnonKeyFingerprint,
	})
	if err != nil {
		t.Fatalf("scope: %v", err)
	}

	var m discovery.Manifest
	payloadtest.LoadJSON(t, "manifest", &m)
	var shard discovery.Shard
	payloadtest.LoadJSON(t, "fields_pages", &shard)

	shard.SetBlockField(discovery.BlockField{
		Path:   "layout",
		Slugs:  slugs,
		Source: discovery.SourcePayloadProtocol,
	})

	now := payloadtest.Epoch
	generation := cache.NewGeneration(now)
	m.Generation = generation
	m.Meta.Scope = sc.Key
	m.GeneratedAt = now
	m.ExpiresAt = now.Add(24 * time.Hour)
	shard.Generation = generation

	store := cache.New(filepath.Join(home, "cache"))
	ok, warns := store.WriteSet(sc, cache.Set{
		Generation: generation,
		Manifest:   &m,
		Shards:     map[string]any{cache.ShardName(shard.Slug, cache.KindCollection): &shard},
	}, now)
	if !ok {
		t.Fatalf("seed cache write failed: %v", warns)
	}
	return sc
}

// ---------------------------------------------------------------------------
// ls
// ---------------------------------------------------------------------------

func TestBlocksLsFindsTheFieldFromTheDocument(t *testing.T) {
	r := cliRun(t, invocation{Args: []string{"blocks", "ls"}, Stdin: pageDoc})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	d := r.data(t)
	if d["field"] != "layout" {
		t.Errorf("field = %v, want layout", d["field"])
	}
	if got, _ := d["count"].(json.Number); got.String() != "3" {
		t.Errorf("count = %v", d["count"])
	}
	if r.Env["data_kind"] != "op_result" {
		t.Errorf("data_kind = %v; ls is a listing, not a document to apply", r.Env["data_kind"])
	}
	// The field was found by looking at the document, not at a schema, and that
	// has to be said out loud.
	if !r.hasWarning(t, WarnBlocksFieldInferred) {
		t.Error("an inferred field must warn")
	}

	rows, _ := d["rows"].([]any)
	if len(rows) != 3 {
		t.Fatalf("rows = %d", len(rows))
	}
	first, _ := rows[0].(map[string]any)
	if first["selector"] != "id:a1" {
		t.Errorf("selector = %v, want id:a1 — an index does not survive the next stage", first["selector"])
	}
	if first["block_type"] != "cta" || first["block_name"] != "Top CTA" {
		t.Errorf("row 0 = %v", first)
	}
}

// Two blocks fields in one document is the case PayCLI must refuse rather than
// guess: choosing the wrong one edits a field the caller never named.
func TestBlocksLsRefusesToGuessBetweenTwoFields(t *testing.T) {
	doc := `{"id":1,
      "layout":[{"blockType":"cta"}],
      "hero":[{"blockType":"mediaBlock"}]}`
	r := cliRun(t, invocation{Args: []string{"blocks", "ls"}, Stdin: doc})
	if got := r.code(t); got != "field_ambiguous" {
		t.Fatalf("code = %q, want field_ambiguous", got)
	}
	if r.Code != 5 {
		t.Errorf("exit = %d, want 5", r.Code)
	}
	e, _ := r.Env["error"].(map[string]any)
	dym, _ := e["did_you_mean"].([]any)
	if len(dym) != 2 {
		t.Errorf("did_you_mean = %v, want both candidates", dym)
	}

	// Naming one resolves it.
	r = cliRun(t, invocation{Args: []string{"blocks", "ls", "--field", "hero"}, Stdin: doc})
	if r.Code != 0 {
		t.Fatalf("--field did not resolve it: exit %d %s", r.Code, r.Stdout)
	}
	if r.data(t)["field"] != "hero" {
		t.Errorf("field = %v", r.data(t)["field"])
	}
}

// ---------------------------------------------------------------------------
// mv / rm and the pipeline contract
// ---------------------------------------------------------------------------

func TestBlocksMvRecordsTheEditAndKeepsTheTarget(t *testing.T) {
	r := cliRun(t, invocation{
		Args:  []string{"blocks", "mv", "type:cta", "--after", "type:mediaBlock"},
		Stdin: pageEnvelope(t, pageDoc),
	})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	d := r.data(t)
	if got := blockTypes(t, d["layout"]); strings.Join(got, ",") != "content,mediaBlock,cta" {
		t.Errorf("order = %v", got)
	}
	// Everything else about the document survives untouched: the next stage's
	// input is this stage's output.
	if d["title"] != "Home" {
		t.Errorf("the rest of the document was not passed through: %v", d)
	}

	target, _ := r.Env["target"].(map[string]any)
	if target == nil || target["slug"] != "pages" {
		t.Fatalf("target = %v; `pay apply` reads the destination out of it", target)
	}
	edits, _ := r.Env["edits"].(map[string]any)
	if edits == nil {
		t.Fatal("no edits block: `pay apply` would have nothing to narrow the write to")
	}
	if fields, _ := edits["fields"].([]any); len(fields) != 1 || fields[0] != "layout" {
		t.Errorf("edits.fields = %v", edits["fields"])
	}
	// An upstream warning is about the document the LAST stage will write, so
	// it has to survive every stage in between.
	if !r.hasWarning(t, "upstream_thing") {
		t.Error("an upstream warning was dropped by the pipe")
	}
}

// Three stages, one document: the ops accumulate and the field list stays
// deduplicated.
func TestBlocksStagesCompose(t *testing.T) {
	r := cliRun(t, invocation{
		Args:  []string{"blocks", "rm", "type:content"},
		Stdin: pageEnvelope(t, pageDoc),
	})
	r = cliRun(t, invocation{Args: []string{"blocks", "mv", "last", "--first"}, Stdin: stageOut(t, r)})
	r = cliRun(t, invocation{Args: []string{"blocks", "cp", "first", "--last"}, Stdin: stageOut(t, r)})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}

	if got := blockTypes(t, r.data(t)["layout"]); strings.Join(got, ",") != "mediaBlock,cta,mediaBlock" {
		t.Fatalf("order = %v", got)
	}
	edits, _ := r.Env["edits"].(map[string]any)
	ops, _ := edits["ops"].([]any)
	if len(ops) != 3 {
		t.Fatalf("ops = %d, want 3", len(ops))
	}
	if fields, _ := edits["fields"].([]any); len(fields) != 1 {
		t.Errorf("edits.fields = %v; three edits to one field is still one field", fields)
	}
	for i, want := range []string{"blocks rm", "blocks mv", "blocks cp"} {
		op, _ := ops[i].(map[string]any)
		if op["command"] != want {
			t.Errorf("ops[%d].command = %v, want %v", i, op["command"], want)
		}
	}

	// The copy must have no id, or Payload matches it to the row it was copied
	// from and the array loses an entry instead of gaining one.
	list, _ := r.data(t)["layout"].([]any)
	last, _ := list[len(list)-1].(map[string]any)
	if _, ok := last["id"]; ok {
		t.Error("the copied row kept its id")
	}
}

// A failure upstream must arrive with ITS code and ITS exit status. Reporting
// it as a local input error sends the caller to fix a selector that was right.
func TestBlocksPropagatesAnUpstreamError(t *testing.T) {
	upstream := `{"ok":false,"v":1,"command":"get","data_kind":"error",
	  "error":{"code":"doc_not_found","exit":4,"message":"no document 99","retriable":false,
	           "confidence":"certain","fields":[],"did_you_mean":[]},
	  "meta":{"request_id":"t"},"warnings":[]}`
	r := cliRun(t, invocation{Args: []string{"blocks", "rm", "0"}, Stdin: upstream})
	if got := r.code(t); got != "doc_not_found" {
		t.Fatalf("code = %q, want the upstream's doc_not_found", got)
	}
	if r.Code != 4 {
		t.Fatalf("exit = %d, want the upstream's 4", r.Code)
	}
	if r.Env["command"] != "blocks rm" {
		t.Errorf("command = %v; the failing STAGE is still this one", r.Env["command"])
	}
}

func TestBlocksRefusesAnEmptyStdin(t *testing.T) {
	r := cliRun(t, invocation{Args: []string{"blocks", "ls"}})
	if got := r.code(t); got != "no_input" {
		t.Fatalf("code = %q, want no_input", got)
	}
	if r.Code != 5 {
		t.Errorf("exit = %d, want 5", r.Code)
	}
}

func TestBlocksRefusesADocList(t *testing.T) {
	list := `{"ok":true,"v":1,"command":"find","data_kind":"doc_list",
	  "data":[{"id":1},{"id":2}],"meta":{"request_id":"t"},"warnings":[]}`
	r := cliRun(t, invocation{Args: []string{"blocks", "mv", "0", "--last"}, Stdin: list})
	if r.Code == 0 {
		t.Fatal("a doc_list must not be editable as one document")
	}
}

// ---------------------------------------------------------------------------
// selector failures
// ---------------------------------------------------------------------------

func TestBlocksSelectorFailures(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantCode string
		wantExit int
		wantDYM  string
	}{
		{"no match is a not-found", []string{"blocks", "rm", "id:nope"}, "selector_no_match", 4, "id:a1"},
		{"ambiguous is a validation failure", []string{"blocks", "mv", "type:cta", "--first"}, "", 0, ""},
		{"bad grammar", []string{"blocks", "rm", "slug:cta"}, "invalid_args", 5, ""},
		{"a move with no destination", []string{"blocks", "mv", "0"}, "invalid_args", 5, ""},
		{"two destinations", []string{"blocks", "mv", "0", "--first", "--last"}, "invalid_args", 5, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.wantCode == "" {
				t.Skip("covered by TestBlocksAmbiguousSelector")
			}
			r := cliRun(t, invocation{Args: tc.args, Stdin: pageDoc})
			if got := r.code(t); got != tc.wantCode {
				t.Fatalf("code = %q, want %q (%s)", got, tc.wantCode, r.Stdout)
			}
			if r.Code != tc.wantExit {
				t.Errorf("exit = %d, want %d", r.Code, tc.wantExit)
			}
			if tc.wantDYM != "" {
				e, _ := r.Env["error"].(map[string]any)
				if !strings.Contains(strings.Join(toStrings(e["did_you_mean"]), ","), tc.wantDYM) {
					t.Errorf("did_you_mean = %v, want it to offer %s", e["did_you_mean"], tc.wantDYM)
				}
			}
		})
	}
}

// A selector matching two rows must fail a single-row verb rather than pick
// one: picking one silently moves a block the caller did not name.
func TestBlocksAmbiguousSelector(t *testing.T) {
	doc := `{"layout":[{"id":"a","blockType":"cta"},{"id":"b","blockType":"content"},{"id":"c","blockType":"cta"}]}`
	r := cliRun(t, invocation{Args: []string{"blocks", "mv", "type:cta", "--first"}, Stdin: doc})
	if got := r.code(t); got != "selector_ambiguous" {
		t.Fatalf("code = %q, want selector_ambiguous", got)
	}
	if r.Code != 5 {
		t.Errorf("exit = %d, want 5", r.Code)
	}
	e, _ := r.Env["error"].(map[string]any)
	dym := toStrings(e["did_you_mean"])
	if len(dym) != 2 || dym[0] != "id:a" || dym[1] != "id:c" {
		t.Errorf("did_you_mean = %v, want both matches as ready-to-paste selectors", dym)
	}

	// --all is how the caller says they meant every match.
	r = cliRun(t, invocation{Args: []string{"blocks", "rm", "type:cta", "--all"}, Stdin: doc})
	if r.Code != 0 {
		t.Fatalf("--all did not allow the multi-row remove: %s", r.Stdout)
	}
	if got := blockTypes(t, r.data(t)["layout"]); strings.Join(got, ",") != "content" {
		t.Errorf("remaining = %v", got)
	}
}

// ---------------------------------------------------------------------------
// schema-backed validation
// ---------------------------------------------------------------------------

// Payload DROPS a block row whose blockType it does not recognise and still
// answers 201, so this has to fail locally or not at all.
func TestBlocksAddRejectsAnUnknownBlockType(t *testing.T) {
	home := t.TempDir()
	srv := payloadtest.NewServer(t)
	seedBlocksDiscovery(t, home, srv.URL, []string{"cta", "content", "mediaBlock"})

	r := cliRun(t, invocation{
		Home: home, Env: seededEnv(srv.URL), HTTP: srv.Client().Transport,
		Args:  []string{"blocks", "add", "ctaa"},
		Stdin: pageEnvelope(t, pageDoc),
	})
	if got := r.code(t); got != "block_type_unknown" {
		t.Fatalf("code = %q, want block_type_unknown (%s)", got, r.Stdout)
	}
	if r.Code != 10 {
		t.Errorf("exit = %d, want 10 — it is a fact about the project, not the command line", r.Code)
	}
	e, _ := r.Env["error"].(map[string]any)
	if dym := toStrings(e["did_you_mean"]); len(dym) == 0 || dym[0] != "cta" {
		t.Errorf("did_you_mean = %v, want cta", dym)
	}

	// A slug the field does accept goes through, and the row gets no id.
	r = cliRun(t, invocation{
		Home: home, Env: seededEnv(srv.URL), HTTP: srv.Client().Transport,
		Args:  []string{"blocks", "add", "content", "--name", "Body", "--first"},
		Stdin: pageEnvelope(t, pageDoc),
	})
	if r.Code != 0 {
		t.Fatalf("a valid blockType was rejected: %s", r.Stdout)
	}
	list, _ := r.data(t)["layout"].([]any)
	first, _ := list[0].(map[string]any)
	if first["blockType"] != "content" || first["blockName"] != "Body" {
		t.Errorf("new row = %v", first)
	}
	if _, ok := first["id"]; ok {
		t.Error("a new row must carry no id: ids are server-generated")
	}
}

// With no schema to check against, an unknown slug must NOT be rejected — a
// local refusal based on a fact PayCLI merely failed to discover is a wrong
// answer PayCLI produced itself (§7.10, conflict 37).
func TestBlocksAddWithoutASchemaDoesNotBlock(t *testing.T) {
	r := cliRun(t, invocation{Args: []string{"blocks", "add", "somethingNew"}, Stdin: pageDoc})
	if r.Code != 0 {
		t.Fatalf("exit %d: an undiscovered slug must not be a local rejection\n%s", r.Code, r.Stdout)
	}
}

// ---------------------------------------------------------------------------
// set
// ---------------------------------------------------------------------------

func TestBlocksSet(t *testing.T) {
	r := cliRun(t, invocation{
		Args: []string{"blocks", "set", "id:c3",
			"--set", "media=9", "--set-json", `links=[{"label":"Buy"}]`, "--unset", "blockName"},
		Stdin: pageEnvelope(t, pageDoc),
	})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	list, _ := r.data(t)["layout"].([]any)
	row, _ := list[2].(map[string]any)
	if got, _ := row["media"].(json.Number); got.String() != "9" {
		t.Errorf("media = %v (%T); --set must keep a number a number", row["media"], row["media"])
	}
	if links, _ := row["links"].([]any); len(links) != 1 {
		t.Errorf("links = %v", row["links"])
	}
	if _, ok := row["blockName"]; ok {
		t.Error("--unset left the key behind")
	}
	// The other rows are untouched.
	other, _ := list[0].(map[string]any)
	if other["blockName"] != "Top CTA" {
		t.Errorf("set touched another row: %v", other)
	}
}

func TestBlocksSetNeedsSomethingToChange(t *testing.T) {
	r := cliRun(t, invocation{Args: []string{"blocks", "set", "0"}, Stdin: pageDoc})
	if got := r.code(t); got != "invalid_args" {
		t.Fatalf("code = %q", got)
	}
}

// stdin is the document. Reading --data from it too would swallow the
// pipeline's input and fail three stages later with "stdin was empty".
func TestBlocksAddRefusesDataFromStdin(t *testing.T) {
	r := cliRun(t, invocation{Args: []string{"blocks", "add", "cta", "--data", "@-"}, Stdin: pageDoc})
	if got := r.code(t); got != "invalid_args" {
		t.Fatalf("code = %q, want invalid_args", got)
	}
	if !strings.Contains(r.Stdout, "stdin already carries the document") {
		t.Errorf("the message does not explain why: %s", r.Stdout)
	}
}

// ---------------------------------------------------------------------------
// depth
// ---------------------------------------------------------------------------

// A read above --depth 0 expands a relationship into a whole document. Writing
// that back stores the expansion instead of the id, and PayCLI is the only
// thing in the pipe that can see it coming.
func TestBlocksWarnsAboutAPopulatedRelationship(t *testing.T) {
	doc := `{"id":1,"layout":[
	  {"id":"a","blockType":"mediaBlock","media":{"id":7,"filename":"x.png","createdAt":"2026-01-01T00:00:00Z"}}]}`
	r := cliRun(t, invocation{Args: []string{"blocks", "mv", "0", "--last"}, Stdin: doc})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	if !r.hasWarning(t, WarnPopulatedRelationship) {
		t.Fatalf("no populated_relationship warning: %v", r.warnings(t))
	}
	for _, w := range r.warnings(t) {
		if w["code"] == WarnPopulatedRelationship {
			if paths := toStrings(w["paths"]); len(paths) != 1 || paths[0] != "layout[0].media" {
				t.Errorf("paths = %v", w["paths"])
			}
		}
	}
}

// A lexical richText value is an object too, and it is not a relationship. The
// warning must not fire on one, or it fires on every page with rich text.
func TestBlocksDoesNotWarnAboutLexical(t *testing.T) {
	doc := `{"id":1,"layout":[{"id":"a","blockType":"cta","richText":{"root":{"children":[],"type":"root"}}}]}`
	r := cliRun(t, invocation{Args: []string{"blocks", "mv", "0", "--last"}, Stdin: doc})
	if r.hasWarning(t, WarnPopulatedRelationship) {
		t.Errorf("a lexical value was reported as an expanded relationship: %v", r.warnings(t))
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func blockTypes(t *testing.T, v any) []string {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("not an array: %v", v)
	}
	out := make([]string, 0, len(list))
	for _, row := range list {
		obj, _ := row.(map[string]any)
		s, _ := obj["blockType"].(string)
		out = append(out, s)
	}
	return out
}

func toStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
