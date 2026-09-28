package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/audit"
	"github.com/KLIXPERT-io/pay-cli/internal/backup"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// ---------------------------------------------------------------------------
// docStore: a small in-memory Payload for several collections, with the
// drafts model of payload@3.87.1 (collections/operations/utilities/update.js):
//
//   - the main row is what a read WITHOUT draft=true returns;
//   - a newer draft on top of it (newest) is what draft=true returns;
//   - a save with draft=true and a body _status other than "published" only
//     writes a new newest version (the main row is untouched);
//   - any other save merges the body into the NEWEST version and writes the
//     result to the main row — which is how a plain save over a published
//     document with a pending draft stores _status "draft" and unpublishes.
// ---------------------------------------------------------------------------

type docStore struct {
	mu     sync.Mutex
	main   map[string]map[string]map[string]any // collection → id → doc
	newest map[string]map[string]map[string]any // collection → id → newer draft
	srv    *payloadtest.Server
}

func cloneDoc(d map[string]any) map[string]any {
	b, _ := json.Marshal(d)
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	var out map[string]any
	_ = dec.Decode(&out)
	return out
}

func (s *docStore) current(coll, id string, draft bool) (map[string]any, bool) {
	if draft {
		if d, ok := s.newest[coll][id]; ok {
			return d, true
		}
	}
	d, ok := s.main[coll][id]
	return d, ok
}

// save applies one write with Payload's draft semantics. An update by id
// merges into the newest version; a bulk update merges into the stored row
// unless it is a draft save (payload 3.87 collections/operations/update.js:
// db.find without ?draft, queryDrafts with it).
func (s *docStore) save(coll, id string, body map[string]any, draft, bulk bool) map[string]any {
	st, _ := body["_status"].(string)
	base, _ := s.current(coll, id, !bulk || draft)
	merged := cloneDoc(base)
	for k, v := range body {
		merged[k] = v
	}
	if draft && st != "published" {
		if _, ok := merged["_status"]; ok || st != "" {
			merged["_status"] = "draft"
		}
		if s.newest[coll] == nil {
			s.newest[coll] = map[string]map[string]any{}
		}
		s.newest[coll][id] = merged
		return merged
	}
	s.main[coll][id] = merged
	delete(s.newest[coll], id)
	return merged
}

func (s *docStore) bodyOf(r *http.Request) map[string]any {
	seen := s.srv.Requests()
	raw := seen[len(seen)-1].Body
	body := map[string]any{}
	if ct, params, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil && strings.HasPrefix(ct, "multipart/") {
		mr := multipart.NewReader(strings.NewReader(raw), params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			if p.FormName() == "_payload" {
				b, _ := io.ReadAll(p)
				_ = json.Unmarshal(b, &body)
			}
		}
		return body
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	_ = dec.Decode(&body)
	return body
}

func newDocStore(t *testing.T, docs map[string]map[string]map[string]any, extra ...payloadtest.Option) *docStore {
	t.Helper()
	s := &docStore{main: docs, newest: map[string]map[string]map[string]any{}}
	var opts []payloadtest.Option
	for coll := range docs {
		opts = append(opts,
			payloadtest.WithHandler("GET", "/api/"+coll, func(w http.ResponseWriter, r *http.Request) {
				s.mu.Lock()
				defer s.mu.Unlock()
				draft := r.URL.Query().Get("draft") == "true"
				ids := whereIDs(r)
				if len(ids) == 0 {
					for id := range s.main[coll] {
						ids = append(ids, id)
					}
					sort.Strings(ids)
				}
				docs := []any{}
				for _, id := range ids {
					if d, ok := s.current(coll, id, draft); ok {
						docs = append(docs, d)
					}
				}
				writeJSON(w, 200, map[string]any{"docs": docs, "totalDocs": len(docs), "limit": 100, "page": 1,
					"totalPages": 1, "hasNextPage": false})
			}),
			payloadtest.WithHandler("GET", "/api/"+coll+"/count", func(w http.ResponseWriter, _ *http.Request) {
				s.mu.Lock()
				defer s.mu.Unlock()
				writeJSON(w, 200, map[string]any{"totalDocs": len(s.main[coll])})
			}),
			payloadtest.WithHandler("PATCH", "/api/"+coll, func(w http.ResponseWriter, r *http.Request) {
				s.mu.Lock()
				defer s.mu.Unlock()
				body := s.bodyOf(r)
				docs := []any{}
				for _, id := range whereIDs(r) {
					if _, ok := s.main[coll][id]; ok {
						docs = append(docs, s.save(coll, id, body, r.URL.Query().Get("draft") == "true", true))
					}
				}
				writeJSON(w, 200, map[string]any{"docs": docs, "errors": []any{}})
			}),
			payloadtest.WithHandler("DELETE", "/api/"+coll, func(w http.ResponseWriter, r *http.Request) {
				s.mu.Lock()
				defer s.mu.Unlock()
				ids := whereIDs(r)
				if len(ids) == 0 { // the passthrough form: every document
					for id := range s.main[coll] {
						ids = append(ids, id)
					}
				}
				docs := []any{}
				for _, id := range ids {
					if d, ok := s.main[coll][id]; ok {
						docs = append(docs, d)
						delete(s.main[coll], id)
					}
				}
				writeJSON(w, 200, map[string]any{"docs": docs, "errors": []any{}})
			}),
		)
		for id := range docs[coll] {
			path := "/api/" + coll + "/" + id
			opts = append(opts,
				payloadtest.WithHandler("GET", path, func(w http.ResponseWriter, r *http.Request) {
					s.mu.Lock()
					defer s.mu.Unlock()
					if d, ok := s.current(coll, id, r.URL.Query().Get("draft") == "true"); ok {
						writeJSON(w, 200, d)
						return
					}
					notFound(w)
				}),
				payloadtest.WithHandler("PATCH", path, func(w http.ResponseWriter, r *http.Request) {
					s.mu.Lock()
					defer s.mu.Unlock()
					if _, ok := s.main[coll][id]; !ok {
						notFound(w)
						return
					}
					d := s.save(coll, id, s.bodyOf(r), r.URL.Query().Get("draft") == "true", false)
					writeJSON(w, 200, map[string]any{"doc": d, "message": "Updated successfully."})
				}),
				payloadtest.WithHandler("DELETE", path, func(w http.ResponseWriter, _ *http.Request) {
					s.mu.Lock()
					defer s.mu.Unlock()
					d, ok := s.main[coll][id]
					if !ok {
						notFound(w)
						return
					}
					delete(s.main[coll], id)
					writeJSON(w, 200, map[string]any{"doc": d, "message": "Deleted successfully."})
				}),
			)
		}
	}
	s.srv = payloadtest.NewServer(t, append(opts, extra...)...)
	return s
}

func storeDoc(id int, kv ...any) map[string]any {
	d := map[string]any{"id": json.Number(fmt.Sprint(id)),
		"createdAt": "2026-01-01T00:00:00.000Z", "updatedAt": "2026-01-02T00:00:00.000Z"}
	for i := 0; i+1 < len(kv); i += 2 {
		d[kv[i].(string)] = kv[i+1]
	}
	return d
}

func standardStore(t *testing.T) *docStore {
	t.Helper()
	return newDocStore(t, map[string]map[string]map[string]any{
		"pages": {
			"1": storeDoc(1, "title", "One", "slug", "one", "_status", "published"),
			"2": storeDoc(2, "title", "Two", "slug", "two", "_status", "published"),
		},
		"crm-companies": {
			"1": storeDoc(1, "name", "Acme", "deletedAt", nil),
			"2": storeDoc(2, "name", "Beta", "deletedAt", "2026-02-01T00:00:00.000Z"),
			"3": storeDoc(3, "name", "Gamma", "deletedAt", "2026-02-01T00:00:00.000Z"),
		},
		"media": {
			"1": storeDoc(1, "alt", "logo", "filename", "logo.png", "mimeType", "image/png"),
		},
	})
}

// ---------------------------------------------------------------------------
// every irreversible write path backs up first
// ---------------------------------------------------------------------------

// TestIrreversibleWritePathsBackUpFirst drives every write verb that changes
// or removes an existing document — the ones not covered one by one in
// backups_test.go — with a backup directory set, and asserts for each that
// the backup read reaches the server before the first write request, that
// one file per targeted document is written, and that meta.backups and the
// audit pre-record carry the files. Each of these paths only backs up because
// its call site remembered `.withBackup(...)`: this test is what fails when
// one forgets.
func TestIrreversibleWritePathsBackUpFirst(t *testing.T) {
	upload := filepath.Join(t.TempDir(), "logo.png")
	if err := os.WriteFile(upload, []byte("\x89PNG\r\n\x1a\nfake"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		args  []string
		coll  string
		ids   []string
		label string // the command in the file name
	}{
		{"delete by id", []string{"delete", "pages", "1", "--yes"}, "pages", []string{"1"}, "delete"},
		{"delete to the trash", []string{"delete", "crm-companies", "1", "--yes"}, "crm-companies", []string{"1"}, "delete"},
		{"delete --permanent", []string{"delete", "crm-companies", "2", "--permanent", "--trash", "--yes"}, "crm-companies", []string{"2"}, "delete"},
		{"delete --where", []string{"delete", "pages", "--where", "slug exists true", "--yes"}, "pages", []string{"1", "2"}, "delete"},
		{"delete --where --unsafe-passthrough-where",
			[]string{"delete", "pages", "--where", "slug exists true", "--unsafe-passthrough-where", "--yes"}, "pages", []string{"1", "2"}, "delete"},
		{"restore (un-trash)", []string{"restore", "crm-companies", "3", "--yes"}, "crm-companies", []string{"3"}, "restore"},
		{"upload --replace", []string{"upload", "media", upload, "--replace", "1", "--yes"}, "media", []string{"1"}, "upload"},
		{"publish --where", []string{"publish", "pages", "--where", "slug exists true", "--no-pre-validate", "--yes"}, "pages", []string{"1", "2"}, "publish"},
		{"unpublish --where", []string{"unpublish", "pages", "--where", "slug exists true", "--yes"}, "pages", []string{"1", "2"}, "unpublish"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := standardStore(t)
			home := t.TempDir()
			seedDiscovery(t, home, s.srv.URL)
			dir := filepath.Join(t.TempDir(), "bk")

			res := cliRun(t, invocation{Home: home, Env: backupEnv(s.srv.URL, dir), Args: tc.args})
			if res.Code != 0 {
				t.Fatalf("exit = %d\n%s\n%s", res.Code, res.Stdout, res.Stderr)
			}
			files := backupFiles(t, dir)
			if len(files) != len(tc.ids) {
				t.Fatalf("backup files = %v, want one per id %v", files, tc.ids)
			}
			for i, f := range files {
				want := filepath.Join("default", tc.coll, tc.ids[i]) + string(filepath.Separator)
				if !strings.Contains(f, want) || !strings.HasSuffix(f, "--"+tc.label+".json") {
					t.Errorf("backup %s: want %s…--%s.json", f, want, tc.label)
				}
				rec, err := backup.Load(f)
				if err != nil {
					t.Fatal(err)
				}
				if rec.IDString() != tc.ids[i] || rec.Collection != tc.coll {
					t.Errorf("record %s names %s %s", f, rec.Collection, rec.IDString())
				}
			}
			if got := metaBackups(t, res); strings.Join(got, ",") != strings.Join(files, ",") {
				t.Errorf("meta.backups = %v, want %v", got, files)
			}

			// On the wire: the backup read (a depth=0 list read by id) before
			// the first request that writes.
			firstWrite, backupRead := -1, -1
			for i, r := range s.srv.Requests() {
				if !strings.HasPrefix(r.Path, "/api/"+tc.coll) {
					continue
				}
				if r.Method != http.MethodGet {
					if firstWrite < 0 {
						firstWrite = i
					}
					continue
				}
				if backupRead < 0 && r.Path == "/api/"+tc.coll && strings.Contains(r.Query, "depth=0") &&
					strings.Contains(r.Query, "%22in%22") && !strings.Contains(r.Query, "select") {
					backupRead = i
				}
			}
			if backupRead < 0 || firstWrite < 0 || backupRead > firstWrite {
				t.Errorf("backup read at %d, first write at %d: the backup must come first", backupRead, firstWrite)
			}

			// The audit pre-record names the files.
			var pre *audit.Event
			for _, e := range auditEvents(t, home) {
				if e.Phase == audit.PhasePre {
					e := e
					pre = &e
					break
				}
			}
			if pre == nil || strings.Join(pre.Backups, ",") != strings.Join(files, ",") {
				t.Errorf("audit pre-record backups = %v, want %v", pre, files)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// the published state under a newer draft
// ---------------------------------------------------------------------------

// pendingDraftStore is pages 1 published as "PUB-A" with a newer draft
// "DRAFT-B" on top — the state the reviewer reproduced on localhost (61).
func pendingDraftStore(t *testing.T) *docStore {
	t.Helper()
	s := newDocStore(t, map[string]map[string]map[string]any{
		"pages": {
			"1": storeDoc(1, "title", "PUB-A", "slug", "one", "_status", "published"),
			"2": storeDoc(2, "title", "Two", "slug", "two", "_status", "published"),
		},
	})
	s.newest["pages"] = map[string]map[string]any{
		"1": storeDoc(1, "title", "DRAFT-B", "slug", "one", "_status", "draft"),
	}
	return s
}

func patchesTo(s *docStore, path string) []payloadtest.Recorded {
	var out []payloadtest.Recorded
	for _, r := range s.srv.Requests() {
		if r.Method == http.MethodPatch && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func hasWarning(t *testing.T, r cliResult, code string) map[string]any {
	t.Helper()
	for _, w := range r.warnings(t) {
		if w["code"] == code {
			return w
		}
	}
	return nil
}

// The backup of a published document with a pending draft holds BOTH states:
// doc (the newest version) and published_doc (the main row the write
// overwrites).
func TestBackupCapturesThePublishedStateUnderADraft(t *testing.T) {
	s := pendingDraftStore(t)
	home := t.TempDir()
	seedDiscovery(t, home, s.srv.URL)
	dir := filepath.Join(t.TempDir(), "bk")

	res := cliRun(t, invocation{Home: home, Env: backupEnv(s.srv.URL, dir),
		Args: []string{"publish", "pages", "1", "--no-pre-validate", "--yes"}})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
	}
	files := backupFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
	rec, err := backup.Load(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if rec.Doc["title"] != "DRAFT-B" || rec.Doc["_status"] != "draft" {
		t.Errorf("doc = %v, want the newest version", rec.Doc)
	}
	if rec.PublishedDoc == nil || rec.PublishedDoc["title"] != "PUB-A" || rec.PublishedDoc["_status"] != "published" {
		t.Errorf("published_doc = %v, want the published main row", rec.PublishedDoc)
	}

	// A document without a pending draft costs no second read and has no
	// published_doc.
	dir2 := filepath.Join(t.TempDir(), "bk2")
	res = cliRun(t, invocation{Home: home, Env: backupEnv(s.srv.URL, dir2),
		Args: []string{"publish", "pages", "2", "--no-pre-validate", "--yes"}})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
	}
	rec2, err := backup.Load(backupFiles(t, dir2)[0])
	if err != nil {
		t.Fatal(err)
	}
	if rec2.PublishedDoc != nil {
		t.Errorf("published_doc = %v on a document without a pending draft", rec2.PublishedDoc)
	}
}

// The reviewer's repro, end to end: a plain update over a published page with
// a pending draft is refused (§10.2.3; the page stays live), --draft saves into
// the draft, and even the deliberate --unpublish is undone exactly by `pay
// backups restore`: the published state back live, the draft back on top.
func TestBackupsRestoreBringsBackThePublishedStateAndTheDraft(t *testing.T) {
	s := pendingDraftStore(t)
	home := t.TempDir()
	seedDiscovery(t, home, s.srv.URL)
	dir := filepath.Join(t.TempDir(), "bk")
	env := backupEnv(s.srv.URL, dir)

	// 1. A plain update is refused: it would take the live page offline.
	res := cliRun(t, invocation{Home: home, Env: env, Args: []string{"update", "pages", "1", "--set", "title=C"}})
	if res.Code != 5 {
		t.Fatalf("plain update over a pending draft: exit = %d, want 5\n%s", res.Code, res.Stdout)
	}
	if p := patchesTo(s, "/api/pages/1"); len(p) != 0 {
		t.Fatalf("the refused update sent PATCHes: %+v", p)
	}
	// --draft saves into the draft, the live page untouched.
	res = cliRun(t, invocation{Home: home, Env: env, Args: []string{"update", "pages", "1", "--set", "title=C", "--draft"}})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
	}
	if p := patchesTo(s, "/api/pages/1"); len(p) != 1 || !strings.Contains(p[0].Query, "draft=true") {
		t.Fatalf("PATCHes = %+v, want one with draft=true", p)
	}
	if s.main["pages"]["1"]["_status"] != "published" || s.main["pages"]["1"]["title"] != "PUB-A" {
		t.Fatalf("main row = %v: the plain update took the page offline", s.main["pages"]["1"])
	}
	if s.newest["pages"]["1"]["title"] != "C" {
		t.Fatalf("newest = %v", s.newest["pages"]["1"])
	}

	firstBackup := backupFiles(t, dir)
	// 2. Deliberately take it offline with a new title.
	res = cliRun(t, invocation{Home: home, Env: env,
		Args: []string{"update", "pages", "1", "--set", "title=D", "--unpublish", "--yes"}})
	if res.Code != 0 {
		t.Fatalf("--unpublish: exit %d, warnings %v", res.Code, res.warnings(t))
	}
	if s.main["pages"]["1"]["_status"] != "draft" {
		t.Fatalf("--unpublish did not unpublish: %v", s.main["pages"]["1"])
	}
	files := backupFiles(t, dir)
	if len(files) != 2 || len(firstBackup) != 1 {
		t.Fatalf("files = %v", files)
	}
	// The clock is frozen, so the second file is the ".2" one.
	last := files[0]
	if last == firstBackup[0] {
		last = files[1]
	}
	rec, _ := backup.Load(last)
	if rec.PublishedDoc["title"] != "PUB-A" || rec.Doc["title"] != "C" {
		t.Fatalf("the --unpublish backup holds doc %v / published_doc %v", rec.Doc["title"], rec.PublishedDoc)
	}

	// 3. The dry run shows both steps.
	dry := cliRun(t, invocation{Home: home, Env: env, Args: []string{"backups", "restore", last, "--dry-run"}})
	if dry.Code != 0 {
		t.Fatalf("dry run exit = %d\n%s", dry.Code, dry.Stdout)
	}
	steps, _ := dry.data(t)["steps"].([]any)
	if len(steps) != 2 {
		t.Fatalf("data.steps = %v", dry.data(t)["steps"])
	}
	first, _ := steps[0].(map[string]any)
	second, _ := steps[1].(map[string]any)
	if strings.Contains(fmt.Sprint(first["url"]), "draft=true") || !strings.Contains(fmt.Sprint(second["url"]), "draft=true") {
		t.Errorf("steps = %v", steps)
	}

	// 4. The real restore: two PATCHes, published then draft.
	before := len(patchesTo(s, "/api/pages/1"))
	res = cliRun(t, invocation{Home: home, Env: env, Args: []string{"backups", "restore", last, "--yes"}})
	if res.Code != 0 {
		t.Fatalf("restore exit = %d\n%s", res.Code, res.Stdout)
	}
	p := patchesTo(s, "/api/pages/1")[before:]
	if len(p) != 2 || strings.Contains(p[0].Query, "draft=true") || !strings.Contains(p[1].Query, "draft=true") {
		t.Fatalf("restore PATCHes = %+v", p)
	}
	if !strings.Contains(p[0].Body, `"PUB-A"`) || !strings.Contains(p[0].Body, `"published"`) {
		t.Errorf("step 1 body = %s", p[0].Body)
	}
	if got := s.main["pages"]["1"]; got["_status"] != "published" || got["title"] != "PUB-A" {
		t.Errorf("live after restore = %v, want PUB-A published", got)
	}
	if got := s.newest["pages"]["1"]; got["title"] != "C" || got["_status"] != "draft" {
		t.Errorf("newest after restore = %v, want the draft C on top", got)
	}
	meta, _ := res.Env["meta"].(map[string]any)
	if meta["restored_from"] != last {
		t.Errorf("meta.restored_from = %v", meta["restored_from"])
	}
	// Both steps are audited, each naming the file it restored from.
	var steps2 []string
	for _, e := range auditEvents(t, home) {
		if e.Command == "backups restore" && e.Phase == audit.PhasePost && !e.DryRun {
			if e.RestoredFrom != last {
				t.Errorf("audit restored_from = %q", e.RestoredFrom)
			}
			steps2 = append(steps2, e.Step)
		}
	}
	if len(steps2) != 2 {
		t.Errorf("audited restore steps = %v", steps2)
	}

	// 5. Restoring again: the live row already holds the published state, so
	// only the draft is written.
	before = len(patchesTo(s, "/api/pages/1"))
	res = cliRun(t, invocation{Home: home, Env: env, Args: []string{"backups", "restore", last, "--yes"}})
	if res.Code != 0 {
		t.Fatalf("second restore exit = %d\n%s", res.Code, res.Stdout)
	}
	if p := patchesTo(s, "/api/pages/1")[before:]; len(p) != 1 || !strings.Contains(p[0].Query, "draft=true") {
		t.Errorf("second restore PATCHes = %+v, want the draft only", p)
	}
}

// A draft backup is restored with draft=true: the save skips required-field
// validation like the original draft save did, so an incomplete draft can be
// put back (on 3.87 a plain save with _status draft still validates).
func TestBackupsRestoreOfADraftSavesADraft(t *testing.T) {
	s := newDocStore(t, map[string]map[string]map[string]any{
		"pages": {"1": storeDoc(1, "title", "WIP v2", "slug", "wip", "_status", "draft", "layout", []any{})},
	})
	home := t.TempDir()
	seedDiscovery(t, home, s.srv.URL)
	dir := t.TempDir()
	file, err := backup.New(dir).Write(&backup.Record{Time: payloadtest.Frozen().Now(), Profile: "default",
		BaseURL: s.srv.URL, Collection: "pages", ID: json.Number("1"), Command: "update", Draft: true,
		Doc: storeDoc(1, "title", "WIP v1", "slug", "wip", "_status", "draft", "layout", []any{})})
	if err != nil {
		t.Fatal(err)
	}
	env := backupEnv(s.srv.URL, dir)
	dry := cliRun(t, invocation{Home: home, Env: env, Args: []string{"backups", "restore", file, "--dry-run"}})
	req, _ := dry.data(t)["request"].(map[string]any)
	if !strings.Contains(fmt.Sprint(req["url"]), "draft=true") {
		t.Errorf("dry-run request = %v, want draft=true", req)
	}
	res := cliRun(t, invocation{Home: home, Env: env, Args: []string{"backups", "restore", file, "--yes"}})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
	}
	p := patchesTo(s, "/api/pages/1")
	if len(p) != 1 || !strings.Contains(p[0].Query, "draft=true") {
		t.Fatalf("PATCHes = %+v", p)
	}
	if hasWarning(t, res, WarnRestoredAsDraft) != nil {
		t.Errorf("restored_as_draft on a document that was never live")
	}
	if !strings.Contains(fmt.Sprint(res.Env["next"]), "--draft") {
		t.Errorf("next = %v, want a --draft read", res.Env["next"])
	}
}

// A bulk update never unpublishes: without ?draft Payload merges it into the
// STORED rows, so a published page with a pending draft stays published (its
// draft is superseded, which the dry run warns about).
func TestBulkUpdateNeverUnpublishes(t *testing.T) {
	s := pendingDraftStore(t)
	home := t.TempDir()
	seedDiscovery(t, home, s.srv.URL)
	res := cliRun(t, invocation{Home: home, Env: seededEnv(s.srv.URL),
		Args: []string{"update", "pages", "--where", "slug exists true", "--set", "title=Bulk", "--dry-run", "--yes"}})
	if res.Code != 0 {
		t.Fatalf("dry run: exit = %d\n%s", res.Code, res.Stdout)
	}
	if hasWarning(t, res, WarnPendingDraftSuperseded) == nil {
		t.Errorf("dry run: no %s warning: %v", WarnPendingDraftSuperseded, res.warnings(t))
	}
	res = cliRun(t, invocation{Home: home, Env: seededEnv(s.srv.URL),
		Args: []string{"update", "pages", "--where", "slug exists true", "--set", "title=Bulk", "--yes"}})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
	}
	patches := patchesTo(s, "/api/pages")
	if len(patches) != 1 || strings.Contains(patches[0].Query, "draft=true") {
		t.Errorf("PATCHes = %+v, want one plain bulk PATCH", patches)
	}
	for _, id := range []string{"1", "2"} {
		if got := s.main["pages"][id]; got["_status"] != "published" || got["title"] != "Bulk" {
			t.Errorf("page %s live = %v", id, got)
		}
	}
	d := res.data(t)
	if ok, _ := d["succeeded"].([]any); len(ok) != 2 {
		t.Errorf("succeeded = %v", d["succeeded"])
	}
}

// ---------------------------------------------------------------------------
// backups keep content that merely looks secret-ish
// ---------------------------------------------------------------------------

func TestBackupKeepsContentAndMasksOnlyCredentials(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJwYWdlIjoiZGVtbyJ9.c2lnbmF0dXJlLWRlbW8"
	s := newDocStore(t, map[string]map[string]map[string]any{
		"crm-companies": {"1": storeDoc(1, "name", "Acme", "description", jwt,
			"designTokens", map[string]any{"primary": "#123456"},
			"sessions", []any{map[string]any{"title": "Keynote"}},
			"owner", map[string]any{"id": json.Number("5"), "email": "u@example.com", "apiKey": "0f1e2d3c-aaaa-bbbb-cccc-111122223333",
				"sessions": []any{map[string]any{"id": "sess-1"}}},
		)},
	})
	home := t.TempDir()
	seedDiscovery(t, home, s.srv.URL)
	dir := filepath.Join(t.TempDir(), "bk")
	res := cliRun(t, invocation{Home: home, Env: backupEnv(s.srv.URL, dir),
		Args: []string{"update", "crm-companies", "1", "--set", "name=Changed"}})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
	}
	rec, err := backup.Load(backupFiles(t, dir)[0])
	if err != nil {
		t.Fatal(err)
	}
	if rec.Doc["description"] != jwt {
		t.Errorf("a JWT-shaped content value was masked: %v", rec.Doc["description"])
	}
	if tokens, _ := rec.Doc["designTokens"].(map[string]any); tokens["primary"] != "#123456" {
		t.Errorf("designTokens = %v", rec.Doc["designTokens"])
	}
	if sessions, _ := rec.Doc["sessions"].([]any); len(sessions) != 1 {
		t.Errorf("a content field named sessions was masked: %v", rec.Doc["sessions"])
	}
	owner, _ := rec.Doc["owner"].(map[string]any)
	if owner["apiKey"] != "<redacted>" || owner["sessions"] != "<redacted>" || owner["email"] != "u@example.com" {
		t.Errorf("populated user = %v, want apiKey and sessions masked", owner)
	}
	if strings.Join(rec.RedactedPaths, ",") != "owner.apiKey,owner.sessions" {
		t.Errorf("redacted_paths = %v", rec.RedactedPaths)
	}
	// The write says, before anyone needs the backup, that it is incomplete.
	w := hasWarning(t, res, WarnBackupRedacted)
	if w == nil || !strings.Contains(fmt.Sprint(w["paths"]), "crm-companies 1: owner.apiKey") ||
		!strings.Contains(fmt.Sprint(w["hint"]), "--no-redact") {
		t.Errorf("backup_redacted warning = %v", w)
	}
}

// `pay config set backup_dir <relative>` stores the directory the agent
// means — relative to where it runs — not one next to the user config file.
func TestConfigSetBackupDirAnchorsToTheWorkingDirectory(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(t.TempDir(), "proj", "sub")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	res := cliRun(t, invocation{Home: home, WorkDir: work, Args: []string{"config", "set", "backup_dir", "my-bk"}})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
	}
	want := filepath.Join(work, "my-bk")
	if res.data(t)["value"] != want {
		t.Errorf("stored value = %v, want %s", res.data(t)["value"], want)
	}
	if hasWarning(t, res, WarnConfigPathAnchored) == nil {
		t.Errorf("no %s warning", WarnConfigPathAnchored)
	}
	res = cliRun(t, invocation{Home: home, WorkDir: t.TempDir(), Args: []string{"config", "explain", "--path", ".backup_dir.value", "--output", "id"}})
	if got := strings.TrimSpace(res.Stdout); got != want {
		t.Errorf("explain backup_dir = %q, want %s", got, want)
	}
	// Absolute and ~ values are stored as given.
	res = cliRun(t, invocation{Home: home, WorkDir: work, Args: []string{"config", "set", "backup_dir", "~/pay-backups"}})
	if res.data(t)["value"] != "~/pay-backups" || hasWarning(t, res, WarnConfigPathAnchored) != nil {
		t.Errorf("~ value = %v", res.data(t))
	}
}
