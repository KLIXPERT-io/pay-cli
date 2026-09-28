package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// diffRich is a Payload lexical editor state with one paragraph.
func diffRich(text string) string {
	b, _ := json.Marshal(text)
	return `{"root":{"type":"root","version":1,"direction":"ltr","format":"","indent":0,"children":[` +
		`{"type":"paragraph","version":1,"direction":"ltr","format":"","indent":0,"textFormat":0,"children":[` +
		`{"type":"text","version":1,"detail":0,"format":0,"mode":"normal","style":"","text":` + string(b) + `}]}]}}`
}

// diffStored is the newest draft of pages 12: top-level blocks, a group with
// nested blocks, a named group inside a row, and rich text — the shapes a
// real page on klixpert stage has.
func diffStored() string {
	return `{"id":12,"title":"Prozess","slug":"prozess","_status":"draft",
	  "createdAt":"2026-01-01T00:00:00.000Z","updatedAt":"2026-09-01T00:00:00.000Z",
	  "meta":{"title":"Prozess","description":"d","image":4},
	  "layout":[
	    {"id":"h","blockType":"hero","title":"T","image":{"file":4,"alt":""}},
	    {"id":"x","blockType":"text","content":` + diffRich("Fünf Phasen.") + `},
	    {"id":"g","blockType":"group","blocks":[
	      {"id":"s1","blockType":"processStep","question":"Eins?"},
	      {"id":"s2","blockType":"processStep","question":"Zwei?"}]}]}`
}

// diffPublished is the published version of the same page.
func diffPublished() string {
	return strings.NewReplacer(`"_status":"draft"`, `"_status":"published"`,
		`"Zwei?"`, `"Zwo?"`, "Fünf Phasen.", "Vier Phasen.").Replace(diffStored())
}

// diffServer answers GET /api/pages/12 and /api/pages/13 (published without
// ?draft=true, the draft with it) and GET /api/globals/header, and records
// every request so a test can prove nothing but reads were sent.
func diffServer(t *testing.T, published, draft string) *payloadtest.Server {
	t.Helper()
	serve := func(pub, drf string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("X-Powered-By", "Payload")
			if r.URL.Query().Get("draft") == "true" {
				_, _ = io.WriteString(w, drf)
				return
			}
			_, _ = io.WriteString(w, pub)
		}
	}
	other := strings.NewReplacer(`"id":12`, `"id":13`, `"slug":"prozess"`, `"slug":"prozess-2"`).Replace(published)
	header := `{"id":1,"navItems":[{"id":"n1","link":{"label":"Home","url":"/"}}],"updatedAt":"2026-01-01T00:00:00.000Z"}`
	return payloadtest.NewServer(t,
		payloadtest.WithHandler("GET", "/api/pages/12", serve(published, draft)),
		payloadtest.WithHandler("GET", "/api/pages/13", serve(other, other)),
		payloadtest.WithHandler("GET", "/api/globals/header", serve(header, header)),
	)
}

func writeDiffFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// diffChanges returns data.changes as "op path" strings.
func diffChanges(t *testing.T, r cliResult) []string {
	t.Helper()
	raw, _ := r.data(t)["changes"].([]any)
	out := make([]string, 0, len(raw))
	for _, c := range raw {
		m := c.(map[string]any)
		out = append(out, m["op"].(string)+" "+m["path"].(string))
	}
	return out
}

func assertOnlyReads(t *testing.T, srv *payloadtest.Server) {
	t.Helper()
	for _, rec := range srv.Requests() {
		if rec.Method != http.MethodGet && rec.Method != http.MethodPost {
			t.Fatalf("pay diff sent %s %s", rec.Method, rec.Path)
		}
		if rec.Method == http.MethodPost && !strings.HasSuffix(rec.Path, "/graphql") {
			t.Fatalf("pay diff sent POST %s", rec.Path)
		}
	}
}

// ---------------------------------------------------------------------------
// --data-file: server vs the result of the PATCH
// ---------------------------------------------------------------------------

// The acceptance case: a whole edited copy of a page with ONE nested field
// changed reports exactly that one change — and writes nothing.
func TestDiffDataFileReportsExactlyTheEditedField(t *testing.T) {
	srv := diffServer(t, diffPublished(), diffStored())
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)

	edited := strings.Replace(diffStored(), `"Zwei?"`, `"Zwei, geändert?"`, 1)
	edited = strings.Replace(edited, `"updatedAt":"2026-09-01T00:00:00.000Z"`, `"updatedAt":"2030-01-01T00:00:00.000Z"`, 1)
	file := writeDiffFile(t, home, "page.json", edited)

	r := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), Args: []string{"diff", "pages", "12", "--data-file", file}})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	if got := strings.Join(diffChanges(t, r), ","); got != "change layout[2].blocks[1].question" {
		t.Fatalf("changes = %s\n%s", got, r.Stdout)
	}
	d := r.data(t)
	if d["identical"] != false {
		t.Errorf("identical = %v", d["identical"])
	}
	left := d["left"].(map[string]any)
	right := d["right"].(map[string]any)
	if left["source"] != "server" || left["draft"] != true || right["source"] != "server+patch" || right["semantics"] != "payload-patch" {
		t.Errorf("sides = %v / %v", left, right)
	}
	// The base is the newest draft (what Payload patches), read at depth 0.
	gets := 0
	for _, rec := range srv.Requests() {
		if rec.Path == "/api/pages/12" {
			gets++
			if !strings.Contains(rec.Query, "draft=true") || !strings.Contains(rec.Query, "depth=0") {
				t.Errorf("query = %q, want draft=true and depth=0", rec.Query)
			}
		}
	}
	if gets != 1 {
		t.Errorf("GET /api/pages/12 x%d, want 1", gets)
	}
	assertOnlyReads(t, srv)
	if tgt, _ := r.Env["target"].(map[string]any); tgt["slug"] != "pages" {
		t.Errorf("target = %v", r.Env["target"])
	}
	if r.Env["data_kind"] != "op_result" {
		t.Errorf("data_kind = %v", r.Env["data_kind"])
	}
}

// A partial body is diffed with Payload's PATCH semantics: omitted keys keep
// their value, a row with a known id keeps its unsent keys, omitted rows go.
func TestDiffPartialBodyUsesPatchSemantics(t *testing.T) {
	srv := diffServer(t, diffPublished(), diffStored())
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	env := seededEnv(srv.URL)

	r := cliRun(t, invocation{Home: home, Env: env, Args: []string{"diff", "pages", "12", "--set", "title=Neu"}})
	if got := strings.Join(diffChanges(t, r), ","); got != "change title" {
		t.Fatalf("--set: changes = %s\n%s", got, r.Stdout)
	}

	r = cliRun(t, invocation{Home: home, Env: env, Args: []string{"diff", "pages", "12",
		"--data", `{"meta":{"description":"neu"},"layout":[{"id":"g","blocks":[{"id":"s2"},{"id":"s1","question":"1?"}]}]}`}})
	want := "remove layout[0],remove layout[1],move layout[0].blocks[0],change layout[0].blocks[1].question,change meta.description"
	if got := strings.Join(diffChanges(t, r), ","); got != want {
		t.Fatalf("--data: changes =\n%s\nwant\n%s", got, want)
	}
	body := r.data(t)["right"].(map[string]any)["body_keys"].([]any)
	if len(body) != 2 {
		t.Errorf("body_keys = %v", body)
	}
}

func TestDiffBodyWarnings(t *testing.T) {
	srv := diffServer(t, diffPublished(), diffStored())
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)

	r := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), Args: []string{"diff", "pages", "12",
		"--data", `{"id":99,"titel":"typo"}`}})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	if !r.hasWarning(t, WarnFieldNotOnDocument) || !r.hasWarning(t, WarnBodyIDMismatch) {
		t.Fatalf("warnings = %v", r.warnings(t))
	}
	// The body's id never reaches the result: the URL decides.
	if got := strings.Join(diffChanges(t, r), ","); got != "add titel" {
		t.Fatalf("changes = %s", got)
	}

	// A whole export of a published page carries _status "published", which
	// makes even `update --draft` publish.
	r = cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), Args: []string{"diff", "pages", "12",
		"--data", `{"_status":"published","title":"x"}`}})
	if !r.hasWarning(t, WarnBodyPublishes) {
		t.Fatalf("warnings = %v", r.warnings(t))
	}
	r = cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), Args: []string{"diff", "pages", "12", "--set", "title=x"}})
	if r.hasWarning(t, WarnBodyPublishes) || r.hasWarning(t, WarnFieldNotOnDocument) {
		t.Fatalf("a clean body must not warn: %v", r.warnings(t))
	}

	r = cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), Args: []string{"diff", "pages", "12", "--data", `{}`}})
	if r.code(t) != "bad_request_body" || r.Code != 5 {
		t.Fatalf("empty body: code=%s exit=%d", r.code(t), r.Code)
	}
}

// --data-file accepts a whole `pay get` envelope, like update does.
func TestDiffDataFileAcceptsAnEnvelope(t *testing.T) {
	srv := diffServer(t, diffPublished(), diffStored())
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	file := writeDiffFile(t, home, "env.json", pageEnvelope(t, diffStored()))

	r := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), Args: []string{"diff", "pages", "12", "--data-file", file}})
	if r.Code != 0 || r.data(t)["identical"] != true {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	if !r.hasWarning(t, WarnEnvelopeUnwrapped) {
		t.Error("the envelope unwrap must be announced")
	}
}

// ---------------------------------------------------------------------------
// --draft-vs-published
// ---------------------------------------------------------------------------

func TestDiffDraftVsPublished(t *testing.T) {
	srv := diffServer(t, diffPublished(), diffStored())
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)

	r := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), Args: []string{"diff", "pages", "12", "--draft-vs-published"}})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	want := "change _status,change layout[1].content,change layout[2].blocks[1].question"
	if got := strings.Join(diffChanges(t, r), ","); got != want {
		t.Fatalf("changes = %s\nwant %s", got, want)
	}
	rich := r.data(t)["changes"].([]any)[1].(map[string]any)
	if rich["kind"] != "richtext" || rich["from_text"] != "Vier Phasen." || rich["to_text"] != "Fünf Phasen." {
		t.Errorf("rich-text change = %v", rich)
	}
	if _, ok := rich["from"]; ok {
		t.Errorf("a rich-text change carries the editor JSON: %v", rich)
	}
	if r.hasWarning(t, WarnNeverPublished) {
		t.Error("a published page must not warn never_published")
	}
	// --full keeps the editor states.
	r = cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), Args: []string{"diff", "pages", "12", "--draft-vs-published", "--full"}})
	if !strings.Contains(r.Stdout, `"root"`) {
		t.Errorf("--full dropped the editor state:\n%s", r.Stdout)
	}
}

// A page that was never published has no published state: the plain read is
// the stored record, and the diff says so.
func TestDiffDraftVsPublishedNeverPublished(t *testing.T) {
	srv := diffServer(t, diffStored(), diffStored())
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)

	r := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), Args: []string{"diff", "pages", "12", "--draft-vs-published"}})
	if r.Code != 0 || r.data(t)["identical"] != true {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	if !r.hasWarning(t, WarnNeverPublished) {
		t.Fatalf("warnings = %v", r.warnings(t))
	}
}

// A collection known to have no drafts is refused locally, before any read.
func TestDiffDraftVsPublishedNeedsDrafts(t *testing.T) {
	srv := diffServer(t, diffPublished(), diffStored())
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)

	r := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), Args: []string{"diff", "media", "4", "--draft-vs-published"}})
	if r.code(t) != "feature_unavailable" || r.Code != 10 {
		t.Fatalf("code=%s exit=%d: %s", r.code(t), r.Code, r.Stdout)
	}
	if srv.Count("GET", "/api/media") != 0 {
		t.Error("the refusal must happen before the read")
	}
}

// ---------------------------------------------------------------------------
// two ids, globals
// ---------------------------------------------------------------------------

func TestDiffTwoDocuments(t *testing.T) {
	srv := diffServer(t, diffPublished(), diffStored())
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)

	r := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), Args: []string{"diff", "pages", "12", "13"}})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	// The documents' own ids always differ and are not reported.
	if got := strings.Join(diffChanges(t, r), ","); got != "change slug" {
		t.Fatalf("changes = %s", got)
	}
	for _, rec := range srv.Requests() {
		if strings.HasPrefix(rec.Path, "/api/pages/") && strings.Contains(rec.Query, "draft=true") {
			t.Errorf("the two-id form reads stored documents unless --draft: %s?%s", rec.Path, rec.Query)
		}
	}
	r = cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), Args: []string{"diff", "pages", "12", "13", "--draft"}})
	if got := strings.Join(diffChanges(t, r), ","); got != "change _status,change layout[1].content,change layout[2].blocks[1].question,change slug" {
		t.Fatalf("--draft changes = %s", got)
	}
}

func TestDiffGlobal(t *testing.T) {
	srv := diffServer(t, diffPublished(), diffStored())
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	file := writeDiffFile(t, home, "header.json", `{"navItems":[{"id":"n1","link":{"label":"Start"}}]}`)

	r := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), Args: []string{"diff", "--global", "header", "--data-file", file}})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	// The row keeps its unsent url: its id matches the stored row.
	if got := strings.Join(diffChanges(t, r), ","); got != "change navItems[0].link.label" {
		t.Fatalf("changes = %s", got)
	}
	if tgt, _ := r.Env["target"].(map[string]any); tgt["kind"] != "global" || tgt["slug"] != "header" {
		t.Errorf("target = %v", r.Env["target"])
	}
	assertOnlyReads(t, srv)

	// header has no drafts in the recorded manifest.
	r = cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), Args: []string{"diff", "--global", "header", "--draft-vs-published"}})
	if r.code(t) != "feature_unavailable" {
		t.Fatalf("code = %s", r.code(t))
	}
}

// ---------------------------------------------------------------------------
// --left / --right: local, no network, no profile
// ---------------------------------------------------------------------------

func TestDiffLocalFilesNeedNothing(t *testing.T) {
	home := t.TempDir()
	a := writeDiffFile(t, home, "a.json", pageEnvelope(t, diffStored()))
	b := writeDiffFile(t, home, "b.json", strings.Replace(diffStored(), `"T"`, `"Titel"`, 1))

	// No base URL, no credential, and a transport that panics on use.
	r := cliRun(t, invocation{Home: home, HTTP: panicTransport{}, Args: []string{"diff", "--left", a, "--right", b}})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s\n%s", r.Code, r.Stdout, r.Stderr)
	}
	if got := strings.Join(diffChanges(t, r), ","); got != "change layout[0].title" {
		t.Fatalf("changes = %s", got)
	}
	left := r.data(t)["left"].(map[string]any)
	if left["source"] != "file" || left["data_kind"] != "doc" || left["collection"] != "pages" {
		t.Errorf("left = %v", left)
	}
	meta := r.Env["meta"].(map[string]any)
	if n, _ := meta["http_requests"].(json.Number); n.String() != "0" {
		t.Errorf("http_requests = %v", meta["http_requests"])
	}

	// One side from stdin.
	r = cliRun(t, invocation{Home: home, HTTP: panicTransport{}, Stdin: diffStored(),
		Args: []string{"diff", "--left", "-", "--right", b}})
	if r.Code != 0 || strings.Join(diffChanges(t, r), ",") != "change layout[0].title" {
		t.Fatalf("stdin: exit %d: %s", r.Code, r.Stdout)
	}
	if r.data(t)["left"].(map[string]any)["source"] != "stdin" {
		t.Errorf("left = %v", r.data(t)["left"])
	}
}

// A `pay versions get` envelope is compared by its version sub-document.
func TestDiffLocalFilesUnwrapVersions(t *testing.T) {
	home := t.TempDir()
	var doc any
	if err := json.Unmarshal([]byte(diffStored()), &doc); err != nil {
		t.Fatal(err)
	}
	env, _ := json.Marshal(map[string]any{
		"ok": true, "v": 1, "command": "versions get", "data_kind": "version",
		"data": map[string]any{"id": 88, "parent": 12, "version": doc},
		"meta": map[string]any{},
	})
	a := writeDiffFile(t, home, "v.json", string(env))
	b := writeDiffFile(t, home, "b.json", diffStored())
	r := cliRun(t, invocation{Home: home, Args: []string{"diff", "--left", a, "--right", b}})
	if r.Code != 0 || r.data(t)["identical"] != true {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
}

func TestDiffLocalFileErrors(t *testing.T) {
	home := t.TempDir()
	good := writeDiffFile(t, home, "good.json", `{"a":1}`)
	bad := writeDiffFile(t, home, "bad.json", `{"a":`)
	empty := writeDiffFile(t, home, "empty.json", "  ")
	failed := writeDiffFile(t, home, "failed.json",
		`{"ok":false,"v":1,"command":"get","data_kind":"error","meta":{},"error":{"code":"doc_not_found","message":"no such page"}}`)
	cases := []struct {
		name string
		args []string
		code string
		exit int
	}{
		{"missing file", []string{"--left", filepath.Join(home, "nope.json"), "--right", good}, "file_missing", 5},
		{"invalid json", []string{"--left", bad, "--right", good}, "bad_request_body", 5},
		{"empty file", []string{"--left", good, "--right", empty}, "no_input", 5},
		{"upstream error keeps its code", []string{"--left", failed, "--right", good}, "doc_not_found", 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := cliRun(t, invocation{Home: home, Args: append([]string{"diff"}, tc.args...)})
			if r.code(t) != tc.code {
				t.Fatalf("code = %q, want %q\n%s", r.code(t), tc.code, r.Stdout)
			}
			if tc.exit != 0 && r.Code != tc.exit {
				t.Errorf("exit = %d, want %d", r.Code, tc.exit)
			}
		})
	}
}

func TestDiffRejectsImpossibleForms(t *testing.T) {
	home := t.TempDir()
	f := writeDiffFile(t, home, "f.json", `{}`)
	cases := map[string][]string{
		"nothing":                     {"diff"},
		"id without comparison":       {"diff", "pages", "12"},
		"left alone":                  {"diff", "--left", f},
		"files and a collection":      {"diff", "pages", "--left", f, "--right", f},
		"files and a body":            {"diff", "--left", f, "--right", f, "--set", "a=1"},
		"both sides stdin":            {"diff", "--left", "-", "--right", "-"},
		"body and draft-vs-published": {"diff", "pages", "12", "--data-file", f, "--draft-vs-published"},
		"two ids and a body":          {"diff", "pages", "12", "13", "--data-file", f},
		"draft with a body":           {"diff", "pages", "12", "--data-file", f, "--draft"},
		"global with an id":           {"diff", "pages", "--global", "header", "--data-file", f},
		"global without comparison":   {"diff", "--global", "header"},
		"global with --draft":         {"diff", "--global", "header", "--draft-vs-published", "--draft"},
		"one argument":                {"diff", "pages"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			r := cliRun(t, invocation{Home: home, HTTP: panicTransport{}, Env: seededEnv(testBaseURL), Args: args})
			if r.code(t) != "invalid_args" || r.Code != 5 {
				t.Fatalf("code=%q exit=%d\n%s", r.code(t), r.Code, r.Stdout)
			}
			e := r.Env["error"].(map[string]any)
			if hint, _ := e["hint"].(string); !strings.Contains(hint, "pay diff --left a.json --right b.json") {
				t.Errorf("the hint must list the forms: %q", hint)
			}
		})
	}
}

// --output table is a readable change list, one line per change.
func TestDiffTableOutput(t *testing.T) {
	home := t.TempDir()
	a := writeDiffFile(t, home, "a.json", `{"t":"alt","c":`+diffRich("Hallo")+`,"layout":[{"id":"a","blockType":"hero"},{"id":"b","blockType":"cta"}]}`)
	b := writeDiffFile(t, home, "b.json", `{"t":"neu","c":`+diffRich("Hallo Welt")+`,"layout":[{"id":"b","blockType":"cta"},{"blockType":"text"}]}`)
	r := cliRun(t, invocation{Home: home, Args: []string{"diff", "--left", a, "--right", b, "--output", "table"}})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	lines := strings.Split(strings.TrimSpace(r.Stdout), "\n")
	want := []string{"OP", "change  c", "remove  layout[0]", "add     layout[1]", "change  t"}
	if len(lines) != len(want) {
		t.Fatalf("table:\n%s", r.Stdout)
	}
	for i, prefix := range want {
		if !strings.HasPrefix(lines[i], prefix) {
			t.Errorf("line %d = %q, want prefix %q", i, lines[i], prefix)
		}
	}
	for _, s := range []string{"Hallo Welt", "<hero> id:a", "<text>"} {
		if !strings.Contains(r.Stdout, s) {
			t.Errorf("table misses %q:\n%s", s, r.Stdout)
		}
	}

	same := cliRun(t, invocation{Home: home, Args: []string{"diff", "--left", a, "--right", a, "--output", "table"}})
	if !strings.Contains(same.Stdout, "(no differences)") {
		t.Errorf("identical table:\n%s", same.Stdout)
	}
}

func TestDiffIgnoreAndTimestampFlags(t *testing.T) {
	home := t.TempDir()
	a := writeDiffFile(t, home, "a.json", `{"updatedAt":"1","meta":{"image":1},"layout":[{"id":"a","blockName":"x"}]}`)
	b := writeDiffFile(t, home, "b.json", `{"updatedAt":"2","meta":{"image":2},"layout":[{"id":"a","blockName":"y"}]}`)
	r := cliRun(t, invocation{Home: home, Args: []string{"diff", "--left", a, "--right", b,
		"--ignore", "meta.image", "--ignore", "layout[].blockName"}})
	if r.data(t)["identical"] != true {
		t.Fatalf("changes = %v", diffChanges(t, r))
	}
	r = cliRun(t, invocation{Home: home, Args: []string{"diff", "--left", a, "--right", b,
		"--ignore", "meta", "--ignore", "layout", "--include-timestamps"}})
	if got := strings.Join(diffChanges(t, r), ","); got != "change updatedAt" {
		t.Fatalf("changes = %s", got)
	}
}

// §5.3: in a diff the secret's field name is only in "path", so the
// envelope's key-based pass cannot see it. `pay diff users 1 2` must not
// print two API keys.
func TestDiffRedactsSecretsWhoseNameIsOnlyInThePath(t *testing.T) {
	home := t.TempDir()
	a := writeDiffFile(t, home, "a.json", `{"email":"a@b.c","apiKey":"4f9a1c2e-aaaa-bbbb-cccc-1234567890ab","sessions":[{"id":"s1"}]}`)
	b := writeDiffFile(t, home, "b.json", `{"email":"a@b.c","apiKey":"5f9a1c2e-aaaa-bbbb-cccc-1234567890ab","rows":[{"password":"hunter22"}]}`)
	r := cliRun(t, invocation{Home: home, Args: []string{"diff", "--left", a, "--right", b}})
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	for _, secret := range []string{"4f9a1c2e", "5f9a1c2e", "hunter22"} {
		if strings.Contains(r.Stdout, secret) {
			t.Fatalf("a secret leaked into the diff:\n%s", r.Stdout)
		}
	}
	if got := strings.Join(diffChanges(t, r), ","); got != "change apiKey,add rows,remove sessions" {
		t.Fatalf("the change itself must still be reported: %s", got)
	}
	if !r.hasWarning(t, "data_redacted") {
		t.Error("masking must be announced")
	}
	// --no-redact is the documented escape hatch.
	r = cliRun(t, invocation{Home: home, Args: []string{"diff", "--left", a, "--right", b, "--no-redact"}})
	if !strings.Contains(r.Stdout, "4f9a1c2e") {
		t.Errorf("--no-redact must show the true values:\n%s", r.Stdout)
	}
}

func TestLastPathKey(t *testing.T) {
	for in, want := range map[string]string{
		"apiKey": "apiKey", "users[2].apiKey": "apiKey", "sessions[0]": "sessions",
		"a.b[1][2]": "b", "": "", "[3]": "",
	} {
		if got := lastPathKey(in); got != want {
			t.Errorf("lastPathKey(%q) = %q, want %q", in, got, want)
		}
	}
}
