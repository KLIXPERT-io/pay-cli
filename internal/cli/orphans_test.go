package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/orphans"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// ---------------------------------------------------------------------------
// pgSim: @payloadcms/drizzle 3.87.1's block storage, in miniature
// ---------------------------------------------------------------------------
//
// One table per block TYPE; every block row carries the document id as its
// parent and its `_path`. An update deletes every row of every table whose
// blocks field the traversal of the new data REACHES (top-level fields
// always; a nested field only through a row of its parent type), then inserts
// the new rows. A read groups rows by `_path` across all tables; a row whose
// type the field at that path does not accept reads back as {}. Versions are
// written from the stored row minus its {} rows; ?unpublishAllLocales=true
// skips validation and updates the newest version in place; a draft save
// only appends a version. That is the behaviour reproduced on klixpert stage
// (pages 71) — see internal/orphans for the source references.

type simRow struct {
	id, typ, path string
	order         int
	data          map[string]any
}

type pgSim struct {
	mu       sync.Mutex
	accepts  map[string][]string // "root.layout" / "<type>.<field>"
	required map[string]bool     // "<type>.<field>" needs >= 1 row
	nested   map[string][]string // type -> nested blocks fields
	rows     []simRow
	scalars  map[string]any
	versions []map[string]any
	nextRow  int
	srv      *payloadtest.Server
	// npatch counts PATCHes; fail, when set, answers PATCH number n with
	// that status instead of serving it (0 = serve).
	npatch int
	fail   func(n int) int
	// perLocaleStatus models versions.drafts.localizeStatus: a locale=all
	// read returns _status per locale (any other read flattens it).
	perLocaleStatus bool
}

func newPGSim() *pgSim {
	return &pgSim{
		accepts: map[string][]string{
			"root.layout":  {"group", "text", "bento"},
			"group.blocks": {"grid", "bento", "text"},
			"bento.blocks": {"bentoItem"},
			"grid.blocks":  {"text"},
		},
		required: map[string]bool{"bento.blocks": true},
		nested:   map[string][]string{"group": {"blocks"}, "bento": {"blocks"}, "grid": {"blocks"}},
		scalars: map[string]any{"id": json.Number("71"), "title": "paywt orphan test", "slug": "paywt-orphan-test",
			"_status": "draft", "createdAt": "2026-09-28T00:00:00.000Z", "updatedAt": "2026-09-28T00:00:00.000Z"},
	}
}

// write stores doc's layout the way upsertRow does.
func (s *pgSim) write(doc map[string]any) {
	visited := map[string]bool{}
	var insert []simRow
	var walk func(list []any, path, acceptKey string)
	walk = func(list []any, path, acceptKey string) {
		for _, t := range s.accepts[acceptKey] {
			visited[t] = true
		}
		for i, el := range list {
			row, ok := el.(map[string]any)
			if !ok {
				continue
			}
			bt, _ := row["blockType"].(string)
			if bt == "" || !containsString(s.accepts[acceptKey], bt) {
				continue // transformBlocks skips it
			}
			id, _ := row["id"].(string)
			if id == "" {
				s.nextRow++
				id = "r" + strconv.Itoa(s.nextRow)
			}
			data := map[string]any{}
			for k, v := range row {
				if k == "id" || k == "blockType" || containsString(s.nested[bt], k) {
					continue
				}
				data[k] = v
			}
			insert = append(insert, simRow{id: id, typ: bt, path: path, order: i + 1, data: data})
			for _, f := range s.nested[bt] {
				sub, _ := row[f].([]any)
				walk(sub, path+"."+strconv.Itoa(i)+"."+f, bt+"."+f)
			}
		}
	}
	layout, _ := doc["layout"].([]any)
	walk(layout, "layout", "root.layout")
	var kept []simRow
	for _, r := range s.rows {
		if !visited[r.typ] {
			kept = append(kept, r)
		}
	}
	kept = append(kept, insert...)
	s.rows = kept
	for k, v := range doc {
		if k != "layout" {
			s.scalars[k] = v
		}
	}
}

func (s *pgSim) readList(path, acceptKey string) []any {
	var at []simRow
	for _, r := range s.rows {
		if r.path == path {
			at = append(at, r)
		}
	}
	sort.SliceStable(at, func(i, j int) bool { return at[i].order < at[j].order })
	out := []any{}
	for i, r := range at {
		if !containsString(s.accepts[acceptKey], r.typ) {
			out = append(out, map[string]any{})
			continue
		}
		obj := map[string]any{"id": r.id, "blockType": r.typ}
		for k, v := range r.data {
			obj[k] = v
		}
		for _, f := range s.nested[r.typ] {
			obj[f] = s.readList(path+"."+strconv.Itoa(i)+"."+f, r.typ+"."+f)
		}
		out = append(out, obj)
	}
	return out
}

func (s *pgSim) readMain() map[string]any {
	doc := map[string]any{}
	for k, v := range s.scalars {
		doc[k] = v
	}
	doc["layout"] = s.readList("layout", "root.layout")
	return doc
}

func (s *pgSim) validate(doc map[string]any) error {
	var check func(list []any, owner string) error
	check = func(list []any, owner string) error {
		for _, el := range list {
			row, _ := el.(map[string]any)
			bt, _ := row["blockType"].(string)
			for _, f := range s.nested[bt] {
				sub, _ := row[f].([]any)
				if s.required[bt+"."+f] && len(sub) == 0 {
					return fmt.Errorf("The following field is invalid: %s > %s", bt, f)
				}
				if err := check(sub, bt); err != nil {
					return err
				}
			}
		}
		return nil
	}
	layout, _ := doc["layout"].([]any)
	return check(layout, "root")
}

func (s *pgSim) latest() map[string]any {
	return orphans.CloneDoc(s.versions[len(s.versions)-1])
}

// seed stores doc as a plain (non-draft) write would.
func (s *pgSim) seed(doc map[string]any, status string) {
	doc["_status"] = status
	s.write(doc)
	s.versions = append(s.versions, orphans.WithoutEmptyRows(orphans.CloneDoc(s.readMain())))
}

func (s *pgSim) server(t *testing.T) *payloadtest.Server {
	t.Helper()
	body := func() map[string]any {
		seen := s.srv.Requests()
		out := map[string]any{}
		dec := json.NewDecoder(strings.NewReader(seen[len(seen)-1].Body))
		dec.UseNumber()
		_ = dec.Decode(&out)
		return out
	}
	s.srv = payloadtest.NewServer(t,
		payloadtest.WithHandler("GET", "/api/pages/71", func(w http.ResponseWriter, r *http.Request) {
			s.mu.Lock()
			defer s.mu.Unlock()
			var doc map[string]any
			if r.URL.Query().Get("draft") == "true" {
				doc = s.latest()
			} else {
				doc = s.readMain()
			}
			if s.perLocaleStatus && r.URL.Query().Get("locale") == "all" {
				doc["_status"] = map[string]any{"en": doc["_status"], "de": doc["_status"]}
			}
			writeJSON(w, 200, doc)
		}),
		payloadtest.WithHandler("GET", "/api/pages", func(w http.ResponseWriter, r *http.Request) {
			// The §12.8 backup's batched read (where id in 71).
			s.mu.Lock()
			defer s.mu.Unlock()
			doc := s.readMain()
			if r.URL.Query().Get("draft") == "true" {
				doc = s.latest()
			}
			writeJSON(w, 200, map[string]any{"docs": []any{doc}, "totalDocs": 1, "limit": 10, "page": 1,
				"totalPages": 1, "hasNextPage": false})
		}),
		payloadtest.WithHandler("GET", "/api/pages/versions/700", func(w http.ResponseWriter, _ *http.Request) {
			s.mu.Lock()
			defer s.mu.Unlock()
			writeJSON(w, 200, map[string]any{"id": 700, "parent": 71, "version": orphans.CloneDoc(s.versions[0])})
		}),
		payloadtest.WithHandler("GET", "/api/pages/versions", func(w http.ResponseWriter, _ *http.Request) {
			s.mu.Lock()
			defer s.mu.Unlock()
			docs := []any{}
			for i := len(s.versions) - 1; i >= 0; i-- {
				docs = append(docs, map[string]any{"id": 700 + i, "parent": 71, "version": s.versions[i]})
			}
			writeJSON(w, 200, map[string]any{"docs": docs, "totalDocs": len(docs), "limit": 50, "page": 1,
				"totalPages": 1, "hasNextPage": false})
		}),
		payloadtest.WithHandler("PATCH", "/api/pages/71", func(w http.ResponseWriter, r *http.Request) {
			s.mu.Lock()
			defer s.mu.Unlock()
			q := r.URL.Query()
			s.npatch++
			if s.fail != nil {
				if st := s.fail(s.npatch); st != 0 {
					writeJSON(w, st, map[string]any{"errors": []map[string]any{{"message": "injected failure"}}})
					return
				}
			}
			in := body()
			merged := s.latest()
			for k, v := range in {
				merged[k] = v
			}
			st, _ := in["_status"].(string)
			if q.Get("draft") == "true" && st != "published" {
				merged["_status"] = "draft"
				s.versions = append(s.versions, merged)
				writeJSON(w, 200, map[string]any{"doc": merged, "message": "Updated successfully."})
				return
			}
			unpub := q.Get("unpublishAllLocales") == "true"
			if !unpub {
				if err := s.validate(merged); err != nil {
					writeJSON(w, 400, map[string]any{"errors": []map[string]any{{"name": "ValidationError",
						"message": err.Error(), "data": map[string]any{"errors": []map[string]any{{"path": "layout", "message": err.Error()}}}}}})
					return
				}
			}
			s.write(merged)
			version := orphans.WithoutEmptyRows(orphans.CloneDoc(s.readMain()))
			if unpub {
				s.versions[len(s.versions)-1] = version
			} else {
				s.versions = append(s.versions, version)
			}
			writeJSON(w, 200, map[string]any{"doc": version, "message": "Updated successfully."})
		}),
	)
	return s.srv
}

func (s *pgSim) patches() []payloadtest.Recorded {
	var out []payloadtest.Recorded
	for _, r := range s.srv.Requests() {
		if r.Method == http.MethodPatch {
			out = append(out, r)
		}
	}
	return out
}

func simDoc(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return m
}

const simBento = `{"layout":[{"id":"g1","blockType":"group","blockGap":"standard","blocks":[
 {"id":"b1","blockType":"bento","grid":"2,2,2","blocks":[
  {"id":"i1","blockType":"bentoItem","heading":"one"},{"id":"i2","blockType":"bentoItem","heading":"two"},
  {"id":"i3","blockType":"bentoItem","heading":"three"}]}]}]}`

const simGridBody = `{"layout":[{"blockType":"group","blockGap":"standard","blocks":[
 {"blockType":"grid","columnsDesktop":3,"blocks":[{"blockType":"text","heading":"A"},{"blockType":"text","heading":"B"}]}]}]}`

func simRun(t *testing.T, s *pgSim, home string, args ...string) cliResult {
	t.Helper()
	return cliRun(t, invocation{Home: home, Env: seededEnv(s.srv.URL), Args: args})
}

func writeBody(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "body.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func emptyCount(doc map[string]any) int { return len(orphans.EmptyRows(doc)) }

func setupSim(t *testing.T, seed string, status string) (*pgSim, string) {
	t.Helper()
	s := newPGSim()
	s.server(t)
	s.seed(simDoc(t, seed), status)
	home := t.TempDir()
	seedDiscovery(t, home, s.srv.URL)
	return s, home
}

// ---------------------------------------------------------------------------
// the model reproduces the stage observation
// ---------------------------------------------------------------------------

func TestPlainNestedRestructureLeavesEmptyRowsAndIsReported(t *testing.T) {
	s, home := setupSim(t, simBento, "draft")
	r := simRun(t, s, home, "update", "pages", "71", "--data-file", writeBody(t, home, simGridBody))
	if r.Code != 0 {
		t.Fatalf("exit %d\n%s", r.Code, r.Stdout)
	}
	// The echo (the new version) is clean — the reason a read-back is needed.
	if n := emptyCount(r.data(t)); n != 0 {
		t.Fatalf("echo has %d {} rows", n)
	}
	var found map[string]any
	for _, w := range r.warnings(t) {
		if w["code"] == WarnOrphanRows {
			found = w
		}
	}
	if found == nil {
		t.Fatalf("no orphan_rows warning: %v", r.warnings(t))
	}
	if paths, _ := found["paths"].([]any); len(paths) != 3 {
		t.Errorf("orphan_rows paths = %v", found["paths"])
	}
	if !strings.Contains(fmt.Sprint(found["hint"]), "pay update pages 71 --reset-nested --dry-run") {
		t.Errorf("hint = %v", found["hint"])
	}
	// The stored row holds three {} rows, the draft read none.
	if n := emptyCount(s.readMain()); n != 3 {
		t.Fatalf("stored {} rows = %d, want 3", n)
	}
	g := simRun(t, s, home, "get", "pages", "71", "--depth", "0")
	if !g.hasWarning(t, WarnEmptyRows) {
		t.Errorf("get does not warn empty_rows: %v", g.warnings(t))
	}
	gd := simRun(t, s, home, "get", "pages", "71", "--depth", "0", "--draft")
	if gd.hasWarning(t, WarnEmptyRows) {
		t.Errorf("the draft read is clean but warned: %v", gd.warnings(t))
	}
}

// ---------------------------------------------------------------------------
// --reset-nested
// ---------------------------------------------------------------------------

func TestResetNestedRestructureKeepsTheIDAndLeavesNothing(t *testing.T) {
	s, home := setupSim(t, simBento, "draft")
	versionsBefore := len(s.versions)
	r := simRun(t, s, home, "update", "pages", "71", "--data-file", writeBody(t, home, simGridBody), "--reset-nested")
	if r.Code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.Code, r.Stdout, r.Stderr)
	}
	if n := emptyCount(s.readMain()); n != 0 {
		t.Fatalf("stored row still has %d {} rows: %v", n, s.readMain()["layout"])
	}
	for _, row := range s.rows {
		if row.typ == "bentoItem" || row.typ == "bento" {
			t.Errorf("a %s row survived: %+v", row.typ, row)
		}
	}
	if r.data(t)["id"] != json.Number("71") {
		t.Errorf("id = %v", r.data(t)["id"])
	}
	meta := r.Env["meta"].(map[string]any)
	rn, _ := meta["reset_nested"].(map[string]any)
	if rn == nil || rn["verified"] != true || rn["mode"] != "restructure" || rn["writes"] != json.Number("3") {
		t.Fatalf("meta.reset_nested = %v", rn)
	}
	// The wire: the requested write validated, then the phase and the
	// restatement without validation.
	ps := s.patches()
	if len(ps) != 3 {
		t.Fatalf("PATCHes = %d", len(ps))
	}
	if strings.Contains(ps[0].Query, "unpublishAllLocales") {
		t.Errorf("the requested write skipped validation: %s", ps[0].Query)
	}
	for _, p := range ps[1:] {
		if !strings.Contains(p.Query, "unpublishAllLocales=true") {
			t.Errorf("phase/restatement %s lacks unpublishAllLocales=true", p.Query)
		}
	}
	var phase map[string]any
	_ = json.Unmarshal([]byte(ps[1].Body), &phase)
	layout := phase["layout"].([]any)
	if len(layout) != 2 {
		t.Fatalf("phase layout = %v", layout)
	}
	carrier := layout[1].(map[string]any)
	inner := carrier["blocks"].([]any)[0].(map[string]any)
	if carrier["blockType"] != "group" || inner["blockType"] != "bento" || len(inner["blocks"].([]any)) != 0 {
		t.Errorf("carrier = %v", carrier)
	}
	if _, ok := carrier["id"]; ok {
		t.Error("the carrier kept an id")
	}
	// History: one new version (the requested write), no carrier anywhere.
	if got := len(s.versions); got != versionsBefore+1 {
		t.Errorf("versions %d -> %d, want +1", versionsBefore, got)
	}
	for i, v := range s.versions {
		if l := v["layout"].([]any); len(l) != 1 {
			t.Errorf("version %d has %d layout rows (a carrier leaked into the history)", i, len(l))
		}
	}
	// Every request is audited as its own step.
	var steps []string
	for _, e := range auditEvents(t, home) {
		if e.Phase == "post" {
			steps = append(steps, e.Step)
		}
	}
	want := []string{"reset-nested requested write", "reset-nested phase 1/1", "reset-nested restate"}
	if strings.Join(steps, "|") != strings.Join(want, "|") {
		t.Errorf("audit steps = %v, want %v", steps, want)
	}
}

func TestResetNestedRepairsAnOrphanedDocument(t *testing.T) {
	s, home := setupSim(t, simBento, "draft")
	// A plain restructure orphans the bentoItem rows.
	grid := simDoc(t, simGridBody)
	merged := s.latest()
	merged["layout"] = grid["layout"]
	s.write(merged)
	s.versions = append(s.versions, orphans.WithoutEmptyRows(orphans.CloneDoc(s.readMain())))
	if emptyCount(s.readMain()) != 3 {
		t.Fatal("setup: no orphans")
	}
	versionsBefore := len(s.versions)

	dry := simRun(t, s, home, "update", "pages", "71", "--reset-nested", "--dry-run")
	if dry.Code != 0 || len(s.patches()) != 0 {
		t.Fatalf("dry run: exit %d, %d PATCHes\n%s", dry.Code, len(s.patches()), dry.Stdout)
	}
	plan := dry.data(t)["reset_nested"].(map[string]any)
	if plan["mode"] != "repair" || plan["history_versions"] == json.Number("0") {
		t.Fatalf("plan = %v", plan)
	}
	if !strings.Contains(fmt.Sprint(dry.data(t)["request"]), "unpublishAllLocales=true") {
		t.Errorf("the previewed request is not the unvalidated restatement: %v", dry.data(t)["request"])
	}

	r := simRun(t, s, home, "update", "pages", "71", "--reset-nested")
	if r.Code != 0 {
		t.Fatalf("exit %d\n%s", r.Code, r.Stdout)
	}
	if n := emptyCount(s.readMain()); n != 0 {
		t.Fatalf("still %d {} rows", n)
	}
	if len(s.patches()) != 2 {
		t.Errorf("PATCHes = %d, want 2 (phase + restatement)", len(s.patches()))
	}
	if got := len(s.versions); got != versionsBefore {
		t.Errorf("a repair added versions: %d -> %d", versionsBefore, got)
	}
	rn := r.Env["meta"].(map[string]any)["reset_nested"].(map[string]any)
	if rn["verified"] != true || len(rn["empty_rows_before"].([]any)) != 3 {
		t.Errorf("report = %v", rn)
	}
	// Nothing left to do: no request at all.
	before := len(s.patches())
	again := simRun(t, s, home, "update", "pages", "71", "--reset-nested")
	if again.Code != 0 || len(s.patches()) != before || !again.hasWarning(t, WarnResetNestedNotNeeded) {
		t.Errorf("second repair: exit %d, patches %d->%d, warnings %v", again.Code, before, len(s.patches()), again.warnings(t))
	}
}

func TestResetNestedDryRunPlansAndPredicts(t *testing.T) {
	s, home := setupSim(t, simBento, "draft")
	r := simRun(t, s, home, "update", "pages", "71", "--data-file", writeBody(t, home, simGridBody), "--dry-run")
	if r.Code != 0 || len(s.patches()) != 0 {
		t.Fatalf("exit %d patches %d", r.Code, len(s.patches()))
	}
	if !r.hasWarning(t, WarnOrphanRisk) {
		t.Errorf("no orphan_risk: %v", r.warnings(t))
	}
	diff, _ := r.data(t)["diff"].(map[string]any)
	if diff == nil || diff["base"] != diffBaseLatest || diff["identical"] != false {
		t.Errorf("data.diff = %v", diff)
	}
	r = simRun(t, s, home, "update", "pages", "71", "--data-file", writeBody(t, home, simGridBody), "--dry-run", "--reset-nested")
	plan, _ := r.data(t)["reset_nested"].(map[string]any)
	if plan == nil || plan["phases"] != json.Number("1") || len(plan["phase_bodies"].([]any)) != 1 {
		t.Fatalf("plan = %v", plan)
	}
	if len(s.patches()) != 0 {
		t.Error("a dry run wrote")
	}
}

func TestResetNestedRefusalsAndNoOps(t *testing.T) {
	s, home := setupSim(t, simBento, "published")
	// A pending draft on a published document: a repair would publish it.
	d := s.latest()
	d["title"] = "pending"
	d["_status"] = "draft"
	s.versions = append(s.versions, d)
	r := simRun(t, s, home, "update", "pages", "71", "--reset-nested")
	if r.code(t) != "invalid_args" || len(s.patches()) != 0 {
		t.Fatalf("pending draft repair: %s", r.Stdout)
	}
	// A draft save never needs it: one ordinary request.
	r = simRun(t, s, home, "update", "pages", "71", "--set", "title=x", "--draft", "--reset-nested")
	if r.Code != 0 || !r.hasWarning(t, WarnResetNestedNotNeeded) || len(s.patches()) != 1 {
		t.Fatalf("draft: exit %d warnings %v patches %d", r.Code, r.warnings(t), len(s.patches()))
	}
	for _, args := range [][]string{
		{"update", "pages", "--where", "id equals 71", "--set", "title=x", "--reset-nested", "--yes"},
		{"update", "pages", "71", "--set", "title=x", "--autosave", "--draft", "--reset-nested"},
		{"publish", "pages", "71", "72", "--reset-nested"},
	} {
		if r := simRun(t, s, home, args...); r.code(t) != "invalid_args" {
			t.Errorf("%v: code %q\n%s", args, r.code(t), r.Stdout)
		}
	}
}

func TestPublishLeavesOrphansAndResetNestedDoesNot(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(fmt.Sprintf("reset=%v", reset), func(t *testing.T) {
			s, home := setupSim(t, simBento, "published")
			// A draft restructured the nested blocks.
			d := s.latest()
			d["layout"] = simDoc(t, simGridBody)["layout"]
			d["_status"] = "draft"
			s.versions = append(s.versions, d)

			args := []string{"publish", "pages", "71", "--no-pre-validate"}
			if reset {
				args = append(args, "--reset-nested")
			}
			dry := simRun(t, s, home, append(args, "--dry-run")...)
			if !dry.hasWarning(t, WarnOrphanRisk) {
				t.Errorf("publish dry run does not predict the orphans: %v", dry.warnings(t))
			}
			if diff := dry.data(t)["diff"].(map[string]any); diff["base"] != diffBasePublished {
				t.Errorf("publish diff base = %v", diff["base"])
			}
			r := simRun(t, s, home, args...)
			if r.Code != 0 {
				t.Fatalf("exit %d\n%s", r.Code, r.Stdout)
			}
			if st := s.readMain()["_status"]; st != "published" {
				t.Errorf("status = %v", st)
			}
			n := emptyCount(s.readMain())
			if reset && (n != 0 || !strings.Contains(r.Stdout, `"verified": true`)) {
				t.Errorf("--reset-nested left %d {} rows\n%s", n, r.Stdout)
			}
			if !reset && (n != 3 || !r.hasWarning(t, WarnOrphanRows)) {
				t.Errorf("plain publish: %d {} rows, warnings %v", n, r.warnings(t))
			}
		})
	}
}

// The {} rows are orphans, reported as such — never paired with the body's
// rows by index and reported as dropped input.
func TestEchoWarningsIgnoreEmptyRows(t *testing.T) {
	sent := simDoc(t, `{"layout":[{"blockType":"text","heading":"A"},{"blockType":"text","heading":"B"}]}`)
	echo := simDoc(t, `{"layout":[{},{"id":"1","blockType":"text","heading":"A"},{},{"id":"2","blockType":"text","heading":"B"}]}`)
	for _, w := range echoWarnings("pages", sent, echo, nil, echoConfig{}) {
		if w.Code == "input_silently_dropped" {
			t.Fatalf("false dropped-input warning: %+v", w)
		}
	}
}

// Every command an orphan warning names must exist with the flags it uses.
func TestOrphanHintsNameRealCommands(t *testing.T) {
	root := newRootCommand(&Runtime{})
	for _, c := range []struct {
		path []string
		flag string
	}{
		{[]string{"update"}, "reset-nested"},
		{[]string{"publish"}, "reset-nested"},
		{[]string{"unpublish"}, "reset-nested"},
		{[]string{"apply"}, "reset-nested"},
		{[]string{"globals", "update"}, "reset-nested"},
		{[]string{"backups", "restore"}, "recreate"},
		{[]string{"diff"}, "draft-vs-published"},
		{[]string{"versions", "list"}, "id"},
		{[]string{"get"}, "draft"},
	} {
		cmd, _, err := root.Find(c.path)
		if err != nil || cmd == nil || cmd.Name() != c.path[len(c.path)-1] {
			t.Errorf("%v does not resolve", c.path)
			continue
		}
		if cmd.Flags().Lookup(c.flag) == nil {
			t.Errorf("%v has no --%s", c.path, c.flag)
		}
	}
	o := orphanTarget{t: &collTarget{Slug: "pages"}, id: "71"}
	for _, h := range []string{
		emptyRowsWarning(o, []string{"x"}, false).Hint,
		o.resetCmd(" --dry-run"), publishResetCmd(o), o.getCmd(),
	} {
		if !strings.HasPrefix(h, "pay update pages 71 --reset-nested") && !strings.HasPrefix(h, "pay publish pages 71 --reset-nested") &&
			!strings.HasPrefix(h, "pay get pages 71") {
			t.Errorf("hint %q", h)
		}
	}
}

func TestUnvalidatedParams(t *testing.T) {
	in := query.Params{Draft: query.BoolPtr(true), Depth: query.IntPtr(2), Select: []string{"title"},
		Extra: url.Values{"autosave": {"true"}}}
	p := unvalidated(in, false)
	q, _ := p.Encode()
	v, _ := url.ParseQuery(q)
	if v.Get("unpublishAllLocales") != "true" || v.Get("draft") != "" || v.Get("autosave") != "" || v.Get("depth") != "0" {
		t.Errorf("phase params = %s", q)
	}
	if in.Extra.Get("unpublishAllLocales") != "" {
		t.Error("the write's own params were mutated")
	}
}
