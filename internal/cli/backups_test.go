package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/audit"
	"github.com/KLIXPERT-io/pay-cli/internal/backup"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

// ---------------------------------------------------------------------------
// a tiny in-memory Payload: just enough of pages + the header global for the
// §12.8 write paths to run end to end
// ---------------------------------------------------------------------------

type fakeCMS struct {
	mu     sync.Mutex
	pages  map[string]map[string]any
	header map[string]any
	nextID int
	srv    *payloadtest.Server
	// finds records the query of every GET /api/pages list read.
	finds []string
}

func newFakeCMS() *fakeCMS {
	return &fakeCMS{
		pages: map[string]map[string]any{
			"16": {"id": json.Number("16"), "title": "Home", "slug": "home", "_status": "published",
				"createdAt": "2026-01-01T00:00:00.000Z", "updatedAt": "2026-01-02T00:00:00.000Z"},
			"17": {"id": json.Number("17"), "title": "About", "slug": "about", "_status": "draft",
				"createdAt": "2026-01-01T00:00:00.000Z", "updatedAt": "2026-01-02T00:00:00.000Z"},
		},
		header: map[string]any{"id": json.Number("1"), "globalType": "header", "navItems": []any{},
			"createdAt": "2026-01-01T00:00:00.000Z", "updatedAt": "2026-01-01T00:00:00.000Z"},
		nextID: 100,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Powered-By", "Payload")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func notFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]any{"errors": []map[string]any{{"message": "Not Found"}}})
}

// body returns the JSON body of the request being served. payloadtest has
// already drained r.Body into its recording, so it is read from there: the
// last recorded request IS the one in flight (the CLI never overlaps writes).
func (f *fakeCMS) body() map[string]any {
	seen := f.srv.Requests()
	body := map[string]any{}
	if len(seen) == 0 {
		return body
	}
	dec := json.NewDecoder(strings.NewReader(seen[len(seen)-1].Body))
	dec.UseNumber()
	_ = dec.Decode(&body)
	return body
}

func whereIDs(r *http.Request) []string {
	var where struct {
		ID struct {
			In []any `json:"in"`
		} `json:"id"`
	}
	_ = json.Unmarshal([]byte(r.URL.Query().Get("where")), &where)
	out := make([]string, 0, len(where.ID.In))
	for _, id := range where.ID.In {
		out = append(out, fmt.Sprint(id))
	}
	return out
}

func (f *fakeCMS) server(t *testing.T, extra ...payloadtest.Option) *payloadtest.Server {
	t.Helper()
	opts := []payloadtest.Option{
		payloadtest.WithHandler("GET", "/api/pages", func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.finds = append(f.finds, r.URL.RawQuery)
			ids := whereIDs(r)
			docs := []any{}
			for _, id := range ids {
				if d, ok := f.pages[id]; ok {
					docs = append(docs, d)
				}
			}
			if len(ids) == 0 {
				keys := make([]string, 0, len(f.pages))
				for k := range f.pages {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					docs = append(docs, map[string]any{"id": f.pages[k]["id"]})
				}
			}
			writeJSON(w, 200, map[string]any{"docs": docs, "totalDocs": len(docs), "limit": 100, "page": 1,
				"totalPages": 1, "hasNextPage": false})
		}),
		payloadtest.WithHandler("GET", "/api/pages/count", func(w http.ResponseWriter, _ *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			writeJSON(w, 200, map[string]any{"totalDocs": len(f.pages)})
		}),
		payloadtest.WithHandler("PATCH", "/api/pages", func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			body := f.body()
			docs := []any{}
			for _, id := range whereIDs(r) {
				if d, ok := f.pages[id]; ok {
					for k, v := range body {
						d[k] = v
					}
					docs = append(docs, d)
				}
			}
			writeJSON(w, 200, map[string]any{"docs": docs, "errors": []any{}})
		}),
		payloadtest.WithHandler("POST", "/api/pages", func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			body := f.body()
			f.nextID++
			id := strconv.Itoa(f.nextID)
			body["id"] = json.Number(id)
			f.pages[id] = body
			writeJSON(w, 201, map[string]any{"doc": body, "message": "created"})
		}),
		payloadtest.WithHandler("GET", "/api/globals/header", func(w http.ResponseWriter, _ *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			writeJSON(w, 200, f.header)
		}),
		payloadtest.WithHandler("POST", "/api/globals/header", func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			for k, v := range f.body() {
				f.header[k] = v
			}
			// The real 3.87 shape: the document under `result`.
			writeJSON(w, 200, map[string]any{"message": "Updated successfully.", "result": f.header})
		}),
	}
	for _, id := range []string{"16", "17", "18", "101", "102"} {
		path := "/api/pages/" + id
		opts = append(opts,
			payloadtest.WithHandler("GET", path, func(w http.ResponseWriter, _ *http.Request) {
				f.mu.Lock()
				defer f.mu.Unlock()
				if d, ok := f.pages[id]; ok {
					writeJSON(w, 200, d)
					return
				}
				notFound(w)
			}),
			payloadtest.WithHandler("PATCH", path, func(w http.ResponseWriter, r *http.Request) {
				f.mu.Lock()
				defer f.mu.Unlock()
				d, ok := f.pages[id]
				if !ok {
					notFound(w)
					return
				}
				for k, v := range f.body() {
					d[k] = v
				}
				writeJSON(w, 200, map[string]any{"doc": d, "message": "Updated successfully."})
			}),
			payloadtest.WithHandler("DELETE", path, func(w http.ResponseWriter, _ *http.Request) {
				f.mu.Lock()
				defer f.mu.Unlock()
				d, ok := f.pages[id]
				if !ok {
					notFound(w)
					return
				}
				delete(f.pages, id)
				writeJSON(w, 200, map[string]any{"doc": d, "message": "Deleted successfully."})
			}),
		)
	}
	f.srv = payloadtest.NewServer(t, append(opts, extra...)...)
	return f.srv
}

// backupNewestReads are the backup's batched newest-version reads among the
// recorded list reads: draft=true and no _status projection (that is the
// unpublish guard's read).
func backupNewestReads(finds []string) []string {
	var out []string
	for _, q := range finds {
		if strings.Contains(q, "draft=true") && !strings.Contains(q, "select") {
			out = append(out, q)
		}
	}
	return out
}

// backupEnv is seededEnv plus a backup directory.
func backupEnv(baseURL, dir string) []string {
	return append(seededEnv(baseURL), "PAY_BACKUP_DIR="+dir)
}

func backupFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".json") {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func metaBackups(t *testing.T, r cliResult) []string {
	t.Helper()
	meta, _ := r.Env["meta"].(map[string]any)
	raw, _ := meta["backups"].([]any)
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		out = append(out, fmt.Sprint(p))
	}
	return out
}

func auditEvents(t *testing.T, home string) []audit.Event {
	t.Helper()
	events, err := audit.New(audit.Options{Path: filepath.Join(home, "audit.log")}).Tail(audit.TailOptions{N: 100})
	if err != nil {
		t.Fatalf("audit tail: %v", err)
	}
	return events
}

// ---------------------------------------------------------------------------
// the write paths
// ---------------------------------------------------------------------------

func TestUpdateBacksUpTheDocumentFirst(t *testing.T) {
	cms := newFakeCMS()
	srv := cms.server(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	dir := filepath.Join(t.TempDir(), "bk")

	res := cliRun(t, invocation{
		Home: home, Env: backupEnv(srv.URL, dir),
		Args: []string{"update", "pages", "16", "--set", "title=Changed"},
	})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}
	files := backupFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("backup files = %v, want exactly one", files)
	}
	if got := metaBackups(t, res); len(got) != 1 || got[0] != files[0] {
		t.Errorf("meta.backups = %v, want %v", got, files)
	}
	if !strings.Contains(files[0], filepath.Join("default", "pages", "16")) || !strings.HasSuffix(files[0], "--update.json") {
		t.Errorf("layout: %s", files[0])
	}
	rec, err := backup.Load(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if rec.Doc["title"] != "Home" {
		t.Errorf("the backup holds %v, not the PRE-image", rec.Doc["title"])
	}
	if rec.Collection != "pages" || rec.IDString() != "16" || rec.Command != "update" || !rec.Draft {
		t.Errorf("record = %+v", rec)
	}
	// The read that captured it: depth=0, draft=true (pages has drafts), one id.
	if len(cms.finds) != 1 {
		t.Fatalf("backup reads = %v", cms.finds)
	}
	for _, want := range []string{"depth=0", "draft=true"} {
		if !strings.Contains(cms.finds[0], want) {
			t.Errorf("backup read %q lacks %s", cms.finds[0], want)
		}
	}
	// Order on the wire: the backup read strictly before the PATCH.
	var order []string
	for _, r := range srv.Requests() {
		if r.Path == "/api/pages" || r.Path == "/api/pages/16" {
			order = append(order, r.Method+" "+r.Path)
		}
	}
	// (The two GET /api/pages/16 before it are §10.2.3's select=_status
	// reads, which run before the backup so a refused write takes none.)
	backupAt := -1
	for i, r := range order {
		if r == "GET /api/pages" {
			backupAt = i
			break
		}
	}
	if backupAt < 0 || order[len(order)-1] != "PATCH /api/pages/16" || backupAt != len(order)-2 {
		t.Errorf("request order = %v", order)
	}
	// Both audit records name the file.
	events := auditEvents(t, home)
	if len(events) != 2 {
		t.Fatalf("audit events = %d", len(events))
	}
	for _, e := range events {
		if len(e.Backups) != 1 || e.Backups[0] != files[0] {
			t.Errorf("%s record backups = %v", e.Phase, e.Backups)
		}
	}
}

// A write the server rejects still leaves its backup behind, and the error
// envelope says where: the pre-image was captured before the request.
func TestFailedWriteStillReportsItsBackup(t *testing.T) {
	cms := newFakeCMS()
	srv := cms.server(t, payloadtest.WithHandler("PATCH", "/api/pages/16", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"errors": []map[string]any{{
			"name": "ValidationError", "message": "The following field is invalid: Layout",
			"data": map[string]any{"errors": []map[string]any{{"path": "layout", "message": "required"}}},
		}}})
	}))
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	dir := filepath.Join(t.TempDir(), "bk")
	res := cliRun(t, invocation{Home: home, Env: backupEnv(srv.URL, dir),
		Args: []string{"update", "pages", "16", "--set", "title=x"}})
	if res.Code == 0 {
		t.Fatalf("the rejected write exited 0:\n%s", res.Stdout)
	}
	files := backupFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
	if got := metaBackups(t, res); len(got) != 1 || got[0] != files[0] {
		t.Errorf("error envelope meta.backups = %v, want %v", got, files)
	}
}

func TestDryRunReportsWouldBackupAndWritesNothing(t *testing.T) {
	cms := newFakeCMS()
	srv := cms.server(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	dir := filepath.Join(t.TempDir(), "bk")

	res := cliRun(t, invocation{
		Home: home, Env: backupEnv(srv.URL, dir),
		Args: []string{"update", "pages", "16", "--set", "title=Changed", "--dry-run"},
	})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
	}
	wb, _ := res.data(t)["would_backup"].(map[string]any)
	if wb == nil {
		t.Fatalf("no would_backup: %s", res.Stdout)
	}
	paths, _ := wb["paths"].([]any)
	if fmt.Sprint(wb["documents"]) != "1" || len(paths) != 1 ||
		!strings.Contains(fmt.Sprint(paths[0]), filepath.Join("default", "pages", "16")) {
		t.Errorf("would_backup = %v", wb)
	}
	if files := backupFiles(t, dir); len(files) != 0 {
		t.Errorf("a dry run wrote %v", files)
	}
	if len(cms.finds) != 0 {
		t.Errorf("a dry run read documents to back up: %v", cms.finds)
	}
	if len(metaBackups(t, res)) != 0 {
		t.Error("a dry run reported meta.backups")
	}
}

func TestBackupsOffMeansNoReadAndNoKey(t *testing.T) {
	cms := newFakeCMS()
	srv := cms.server(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	dir := filepath.Join(t.TempDir(), "bk")

	for _, tc := range []struct {
		name string
		env  []string
		args []string
	}{
		{"no directory configured", seededEnv(srv.URL), nil},
		{"--no-backup", backupEnv(srv.URL, dir), []string{"--no-backup"}},
		{"PAY_NO_BACKUP", append(backupEnv(srv.URL, dir), "PAY_NO_BACKUP=1"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cms.finds = nil
			args := append([]string{"update", "pages", "16", "--set", "title=x"}, tc.args...)
			res := cliRun(t, invocation{Home: home, Env: tc.env, Args: args})
			if res.Code != 0 {
				t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
			}
			if len(cms.finds) != 0 || len(backupFiles(t, dir)) != 0 || len(metaBackups(t, res)) != 0 {
				t.Errorf("backups ran: finds=%v files=%v", cms.finds, backupFiles(t, dir))
			}
			if strings.Contains(res.Stdout, `"backups"`) {
				t.Errorf("meta.backups must be absent when nothing was backed up:\n%s", res.Stdout)
			}
		})
	}
}

func TestBackupFailureAbortsTheWrite(t *testing.T) {
	cms := newFakeCMS()
	srv := cms.server(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	res := cliRun(t, invocation{
		Home: home, Env: backupEnv(srv.URL, file),
		Args: []string{"update", "pages", "16", "--set", "title=Changed"},
	})
	if res.Code != apierr.ExitInternal || res.code(t) != string(apierr.CodeBackupFailed) {
		t.Fatalf("exit = %d code = %s, want 1 backup_failed\n%s", res.Code, res.code(t), res.Stdout)
	}
	errObj, _ := res.Env["error"].(map[string]any)
	if !strings.Contains(fmt.Sprint(errObj["hint"]), "--no-backup") {
		t.Errorf("hint does not name --no-backup: %v", errObj["hint"])
	}
	if srv.Count("PATCH", "/api/pages") != 0 {
		t.Fatal("the write was sent although its backup failed")
	}
	if cms.pages["16"]["title"] != "Home" {
		t.Fatal("the document changed")
	}
	// Like a failed pre-audit record: no record claims the write started.
	for _, e := range auditEvents(t, home) {
		if e.Command == "update" {
			t.Errorf("audit has a %s record for an aborted write", e.Phase)
		}
	}
}

func TestBulkUpdateBacksUpEveryResolvedID(t *testing.T) {
	cms := newFakeCMS()
	srv := cms.server(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	dir := filepath.Join(t.TempDir(), "bk")

	res := cliRun(t, invocation{
		Home: home, Env: backupEnv(srv.URL, dir),
		Args: []string{"update", "pages", "--where", "slug exists true", "--set", "title=Bulk", "--yes"},
	})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}
	files := backupFiles(t, dir)
	if len(files) != 2 || len(metaBackups(t, res)) != 2 {
		t.Fatalf("files = %v meta = %v", files, metaBackups(t, res))
	}
	// One batched read for both ids, after the id resolution.
	if backupReads := backupNewestReads(cms.finds); len(backupReads) != 1 {
		t.Errorf("backup reads = %v (%v), want one batched read", backupReads, cms.finds)
	}
	for _, f := range files {
		rec, err := backup.Load(f)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Doc["title"] == "Bulk" {
			t.Errorf("%s holds the POST-image", f)
		}
	}
}

func TestGlobalsUpdateBackupAndExactRestore(t *testing.T) {
	cms := newFakeCMS()
	srv := cms.server(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	dir := filepath.Join(t.TempDir(), "bk")
	env := backupEnv(srv.URL, dir)

	res := cliRun(t, invocation{Home: home, Env: env,
		Args: []string{"globals", "update", "header", "--set-json", `navItems=[{"link":{"label":"x"}}]`, "--yes"}})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
	}
	// The unwrapped `result` makes the echo check quiet on a clean write.
	if res.hasWarning(t, "input_silently_dropped") {
		t.Errorf("false input_silently_dropped on a global update: %v", res.warnings(t))
	}
	files := backupFiles(t, dir)
	if len(files) != 1 || !strings.Contains(files[0], filepath.Join("default", backup.GlobalsDir, "header")) {
		t.Fatalf("files = %v", files)
	}

	srv.Reset()
	res = cliRun(t, invocation{Home: home, Env: env, Args: []string{"backups", "restore", files[0], "--yes"}})
	if res.Code != 0 {
		t.Fatalf("restore exit = %d\n%s", res.Code, res.Stdout)
	}
	var sent map[string]any
	for _, r := range srv.Requests() {
		if r.Method == http.MethodPost && r.Path == "/api/globals/header" {
			_ = json.Unmarshal([]byte(r.Body), &sent)
		}
	}
	if sent == nil {
		t.Fatal("the restore sent no POST")
	}
	for _, owned := range []string{"id", "createdAt", "updatedAt", "globalType"} {
		if _, ok := sent[owned]; ok {
			t.Errorf("the restore sent the server-owned %q", owned)
		}
	}
	if nav, _ := cms.header["navItems"].([]any); len(nav) != 0 {
		t.Errorf("header navItems = %v, want the backed-up []", cms.header["navItems"])
	}
	// The restore itself backed up the state it overwrote.
	if got := backupFiles(t, dir); len(got) != 2 {
		t.Errorf("files after restore = %v, want the restore's own backup too", got)
	}
}

func TestBackupsRestoreDocument(t *testing.T) {
	cms := newFakeCMS()
	srv := cms.server(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	dir := filepath.Join(t.TempDir(), "bk")
	env := backupEnv(srv.URL, dir)

	cliRun(t, invocation{Home: home, Env: env, Args: []string{"update", "pages", "16", "--set", "title=Broken"}})
	file := backupFiles(t, dir)[0]

	t.Run("dry run previews the PATCH and its own backup", func(t *testing.T) {
		res := cliRun(t, invocation{Home: home, Env: env, Args: []string{"backups", "restore", file, "--dry-run"}})
		if res.Code != 0 {
			t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
		}
		d := res.data(t)
		req, _ := d["request"].(map[string]any)
		body, _ := req["body"].(map[string]any)
		if req["method"] != "PATCH" || body["title"] != "Home" || body["id"] != nil || body["createdAt"] != nil {
			t.Errorf("request = %v", req)
		}
		if d["would_backup"] == nil {
			t.Error("no would_backup on the restore preview")
		}
		if cms.pages["16"]["title"] != "Broken" {
			t.Error("the dry run wrote")
		}
	})
	t.Run("non-TTY without --yes is exit 11", func(t *testing.T) {
		res := cliRun(t, invocation{Home: home, Env: env, Args: []string{"backups", "restore", file}})
		if res.Code != apierr.ExitConfirmationRequired {
			t.Fatalf("exit = %d, want 11", res.Code)
		}
	})
	t.Run("restores the stored state", func(t *testing.T) {
		res := cliRun(t, invocation{Home: home, Env: env, Args: []string{"backups", "restore", file, "--yes"}})
		if res.Code != 0 {
			t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
		}
		if cms.pages["16"]["title"] != "Home" {
			t.Errorf("title = %v", cms.pages["16"]["title"])
		}
		events := auditEvents(t, home)
		last := events[len(events)-1]
		if last.Action != "restore_backup" || len(last.Backups) != 1 {
			t.Errorf("audit = %+v", last)
		}
	})
	t.Run("relative to the backup directory", func(t *testing.T) {
		rel, _ := filepath.Rel(dir, file)
		res := cliRun(t, invocation{Home: home, Env: env, Args: []string{"backups", "show", rel}})
		if res.Code != 0 || res.data(t)["path"] != file {
			t.Fatalf("show %s: exit %d %v", rel, res.Code, res.Stdout)
		}
	})
	t.Run("another server is refused", func(t *testing.T) {
		other := append(seededEnv("http://127.0.0.1:1"), "PAY_BACKUP_DIR="+dir)
		res := cliRun(t, invocation{Home: home, Env: other, Args: []string{"backups", "restore", file, "--yes"}})
		if res.code(t) != string(apierr.CodeBackupTargetMismatch) || res.Code != apierr.ExitValidation {
			t.Fatalf("exit %d code %s, want 5 backup_target_mismatch\n%s", res.Code, res.code(t), res.Stdout)
		}
	})
	t.Run("a deleted document needs --recreate, and gets a new id", func(t *testing.T) {
		delete(cms.pages, "16")
		res := cliRun(t, invocation{Home: home, Env: env, Args: []string{"backups", "restore", file, "--yes"}})
		if res.code(t) != string(apierr.CodeDocNotFound) {
			t.Fatalf("code = %s\n%s", res.code(t), res.Stdout)
		}
		errObj, _ := res.Env["error"].(map[string]any)
		if !strings.Contains(fmt.Sprint(errObj["hint"]), "--recreate") {
			t.Errorf("hint = %v", errObj["hint"])
		}
		res = cliRun(t, invocation{Home: home, Env: env, Args: []string{"backups", "restore", file, "--recreate", "--yes"}})
		if res.Code != 0 {
			t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
		}
		if !res.hasWarning(t, WarnRecreatedNewID) {
			t.Errorf("no %s warning: %v", WarnRecreatedNewID, res.warnings(t))
		}
		newID := fmt.Sprint(res.data(t)["id"])
		if newID == "16" || cms.pages[newID]["title"] != "Home" {
			t.Errorf("recreated as %s: %v", newID, cms.pages[newID])
		}
	})
}

func TestVersionsRestoreBacksUpTheParentDocument(t *testing.T) {
	cms := newFakeCMS()
	home := t.TempDir()
	dir := filepath.Join(t.TempDir(), "bk")
	var versionReads int
	// The version record answers with its parent; the restore itself is a POST.
	extra := payloadtest.NewServer(t,
		payloadtest.WithHandler("GET", "/api/pages/versions/77", func(w http.ResponseWriter, _ *http.Request) {
			versionReads++
			writeJSON(w, 200, map[string]any{"id": 77, "parent": 16, "version": map[string]any{"title": "Old"}})
		}),
		payloadtest.WithHandler("POST", "/api/pages/versions/77", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, 200, map[string]any{"doc": map[string]any{"id": 16, "title": "Old"}})
		}),
		payloadtest.WithHandler("GET", "/api/pages", func(w http.ResponseWriter, r *http.Request) {
			cms.mu.Lock()
			defer cms.mu.Unlock()
			cms.finds = append(cms.finds, r.URL.RawQuery)
			writeJSON(w, 200, map[string]any{"docs": []any{cms.pages["16"]}, "totalDocs": 1, "hasNextPage": false})
		}),
	)
	seedDiscovery(t, home, extra.URL)
	res := cliRun(t, invocation{Home: home, Env: backupEnv(extra.URL, dir),
		Args: []string{"versions", "restore", "pages", "77", "--yes"}})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
	}
	files := backupFiles(t, dir)
	if versionReads != 1 || len(files) != 1 || !strings.Contains(files[0], filepath.Join("pages", "16")) {
		t.Fatalf("version reads %d, files %v: the PARENT document must be backed up", versionReads, files)
	}
}

func TestBackupHookIsIdempotentPerInvocation(t *testing.T) {
	cms := newFakeCMS()
	srv := cms.server(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	dir := t.TempDir()

	rt := newRuntime(App{Env: append([]string{"PAY_HOME=" + home, "PAY_NO_UPDATE=1"}, backupEnv(srv.URL, dir)...),
		Now: payloadtest.Frozen().Now, WorkDir: home})
	root := newRootCommand(rt)
	if err := rt.setup(root); err != nil {
		t.Fatal(err)
	}
	client, err := rt.Client(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	d := &Deps{RT: rt, Client: client}
	target := BackupTarget{Collection: &collTarget{Slug: "pages"}, IDs: []any{16}}
	a, err := d.BackupBeforeWrite(t.Context(), "update", target)
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.BackupBeforeWrite(t.Context(), "update", target)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Paths) != 1 || len(b.Paths) != 1 || a.Paths[0] != b.Paths[0] {
		t.Fatalf("paths %v then %v, want the same file twice", a.Paths, b.Paths)
	}
	if len(cms.finds) != 1 || len(backupFiles(t, dir)) != 1 {
		t.Fatalf("reads %v, files %v: a second call must not re-read or re-write", cms.finds, backupFiles(t, dir))
	}
	if got := rt.backupsWritten(); len(got) != 1 {
		t.Errorf("meta.backups = %v, want the file once", got)
	}
	// A missing id is skipped with a warning, not an error.
	res, err := d.BackupBeforeWrite(t.Context(), "update", BackupTarget{Collection: &collTarget{Slug: "pages"}, IDs: []any{404}})
	if err != nil || len(res.Skipped) != 1 {
		t.Fatalf("skipped = %v err = %v", res, err)
	}
	var warned bool
	for _, w := range rt.Warnings() {
		warned = warned || w.Code == WarnBackupSkipped
	}
	if !warned {
		t.Error("no backup_skipped warning")
	}
}

// ---------------------------------------------------------------------------
// restore body and draft handling
// ---------------------------------------------------------------------------

func TestRestoreBodyStripsWhatTheServerOwns(t *testing.T) {
	shard := discovery.NewShard("g", "crm-companies")
	shard.Fields = []discovery.Field{
		testField("name", discovery.TypeText),
		testField("contacts", discovery.TypeJoin),
		testField("computed", discovery.TypeText),
		testField("_status", discovery.TypeSelect),
	}
	shard.Fields[1].ReadOnly = true
	shard.Fields[2].ReadOnly = true
	shard.Fields[3].ReadOnly = true
	shard.JoinFields = []string{"contacts"}
	rec := &backup.Record{Doc: map[string]any{
		"id": 1, "createdAt": "x", "updatedAt": "y", "name": "Acme", "_status": "draft",
		"contacts": map[string]any{"docs": []any{}}, "computed": 3, "sizes": map[string]any{},
		"owner": map[string]any{"apiKey": redact.Mask, "name": "u"},
	}}
	body, notRestored := restoreBody(rec, shard, false)
	for _, gone := range []string{"id", "createdAt", "updatedAt", "contacts", "computed", "sizes"} {
		if _, ok := body[gone]; ok {
			t.Errorf("body still carries %q", gone)
		}
	}
	if body["name"] != "Acme" || body["_status"] != "draft" {
		t.Errorf("body = %v", body)
	}
	owner, _ := body["owner"].(map[string]any)
	if _, ok := owner["apiKey"]; ok {
		t.Error("the redaction mask would be written back")
	}
	if strings.Join(notRestored, ",") != "computed,owner.apiKey" {
		t.Errorf("notRestored = %v (joins and server-owned fields are silent)", notRestored)
	}
	// The backup record itself is untouched.
	if _, ok := rec.Doc["id"]; !ok {
		t.Error("restoreBody mutated the record")
	}
}

func TestRestoreDraftMode(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name         string
		f            backupsRestoreFlags
		drafts       *bool
		backup, live string
		published    bool
		want         restoreMode
	}{
		{"no drafts: plain write", backupsRestoreFlags{}, &no, "", "", false, restoreMode{}},
		{"published backup: restore published", backupsRestoreFlags{}, &yes, "published", "published", false, restoreMode{}},
		// A draft is restored as the draft it was: draft=true skips
		// required-field validation like the original draft save, so an
		// incomplete work-in-progress draft can be put back.
		{"draft backup over a draft: draft save", backupsRestoreFlags{}, &yes, "draft", "draft", false,
			restoreMode{draft: true, status: "draft"}},
		{"draft backup over a LIVE page: new draft, never unpublish", backupsRestoreFlags{}, &yes, "draft", "published", false,
			restoreMode{draft: true, status: "draft", asDraftWarn: true}},
		// The backup also holds the published state the write overwrote.
		{"published state under a draft: two steps", backupsRestoreFlags{}, &yes, "draft", "draft", true,
			restoreMode{draft: true, status: "draft", twoStep: true}},
		{"published state under a draft, live published: two steps", backupsRestoreFlags{}, &yes, "draft", "published", true,
			restoreMode{draft: true, status: "draft", twoStep: true}},
		{"--draft forces a draft", backupsRestoreFlags{asDraft: true}, &yes, "published", "published", false,
			restoreMode{draft: true, status: "draft"}},
		{"--draft wins over the published state", backupsRestoreFlags{asDraft: true}, &yes, "draft", "draft", true,
			restoreMode{draft: true, status: "draft"}},
		{"--publish forces published", backupsRestoreFlags{publish: true}, &yes, "draft", "published", true,
			restoreMode{status: "published"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := restoreDraftMode(&tc.f, tc.drafts, tc.backup, tc.live, tc.published)
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// list / show / prune and the surfaces that report the directory
// ---------------------------------------------------------------------------

func TestBackupsListShowPrune(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	store := backup.New(dir)
	now := payloadtest.Frozen().Now()
	old, _ := store.Write(&backup.Record{Time: now.AddDate(0, 0, -40), Profile: "default", Collection: "pages",
		ID: 16, Command: "update", Doc: map[string]any{"title": "old"}})
	fresh, _ := store.Write(&backup.Record{Time: now.Add(-1e9), Profile: "default", Collection: "pages",
		ID: 16, Command: "delete", Doc: map[string]any{"title": "fresh"}})
	_, _ = store.Write(&backup.Record{Time: now, Profile: "other", Collection: "pages",
		ID: 1, Command: "update", Doc: map[string]any{}})
	env := []string{"PAY_BACKUP_DIR=" + dir}

	res := cliRun(t, invocation{Home: home, Env: env, Args: []string{"backups", "list"}})
	d := res.data(t)
	if fmt.Sprint(d["total"]) != "2" || d["dir"] != dir {
		t.Fatalf("list = %v", d)
	}
	list, _ := d["backups"].([]any)
	if first, _ := list[0].(map[string]any); first["path"] != fresh {
		t.Errorf("not newest first: %v", list)
	}
	res = cliRun(t, invocation{Home: home, Env: env, Args: []string{"backups", "list", "--all-profiles", "--command", "update"}})
	if fmt.Sprint(res.data(t)["total"]) != "2" {
		t.Errorf("--all-profiles --command update = %v", res.data(t))
	}

	res = cliRun(t, invocation{Home: home, Env: env, Args: []string{"backups", "show", old}})
	if doc, _ := res.data(t)["doc"].(map[string]any); doc["title"] != "old" {
		t.Errorf("show = %s", res.Stdout)
	}
	res = cliRun(t, invocation{Home: home, Env: env, Args: []string{"backups", "show", filepath.Join(dir, "nope.json")}})
	if res.code(t) != string(apierr.CodeInvalidArgs) {
		t.Errorf("show of a missing file: %s", res.Stdout)
	}

	res = cliRun(t, invocation{Home: home, Env: env, Args: []string{"backups", "prune", "--older-than", "30d"}})
	if res.Code != apierr.ExitConfirmationRequired {
		t.Fatalf("prune without --yes: exit %d", res.Code)
	}
	res = cliRun(t, invocation{Home: home, Env: env, Args: []string{"backups", "prune", "--older-than", "30d", "--dry-run"}})
	if fmt.Sprint(res.data(t)["would_remove"]) != "1" {
		t.Errorf("dry prune = %v", res.data(t))
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("a dry prune deleted a file")
	}
	res = cliRun(t, invocation{Home: home, Env: env, Args: []string{"backups", "prune", "--older-than", "30d", "--yes"}})
	if fmt.Sprint(res.data(t)["removed"]) != "1" {
		t.Errorf("prune = %v", res.data(t))
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("prune kept the old backup")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("prune deleted a fresh backup")
	}
	res = cliRun(t, invocation{Home: home, Env: env, Args: []string{"backups", "prune"}})
	if res.code(t) != string(apierr.CodeInvalidArgs) {
		t.Errorf("prune without --older-than: %s", res.Stdout)
	}
}

func TestBackupsListWithoutADirectoryWarns(t *testing.T) {
	res := cliRun(t, invocation{Args: []string{"backups", "list"}})
	if res.Code != 0 || !res.hasWarning(t, WarnBackupsOff) {
		t.Fatalf("exit %d warnings %v", res.Code, res.warnings(t))
	}
}

func TestConfigSurfacesReportTheBackupDir(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "pay.toml"), []byte("backup_dir = \".pay-backups\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(work, "a", "b")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	res := cliRun(t, invocation{WorkDir: sub, Args: []string{"config", "explain", "--path", ".backup_dir"}})
	d := res.data(t)
	if d["value"] != filepath.Join(work, ".pay-backups") || !strings.HasPrefix(fmt.Sprint(d["source"]), "project:") {
		t.Errorf("explain backup_dir = %v", d)
	}
	res = cliRun(t, invocation{WorkDir: sub, Args: []string{"config", "paths"}})
	if res.data(t)["backup_dir"] != filepath.Join(work, ".pay-backups") {
		t.Errorf("config paths backup_dir = %v", res.data(t)["backup_dir"])
	}
	res = cliRun(t, invocation{WorkDir: sub, Env: []string{"PAY_BACKUP_DIR=rel"},
		Args: []string{"config", "explain", "--path", ".backup_dir.value", "--output", "id"}})
	if got := strings.TrimSpace(res.Stdout); got != filepath.Join(sub, "rel") {
		t.Errorf("PAY_BACKUP_DIR=rel resolved to %q, want it relative to the working directory", got)
	}
}

// TestPublishSeveralIDsBacksUpEveryDocument is §12.8 over §9.x's explicit-id
// publish: one batched pre-image read for the ids that are sent, one file per
// document, before the first PATCH.
func TestPublishSeveralIDsBacksUpEveryDocument(t *testing.T) {
	cms := newFakeCMS()
	srv := cms.server(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	dir := filepath.Join(t.TempDir(), "bk")

	res := cliRun(t, invocation{
		Home: home, Env: backupEnv(srv.URL, dir),
		Args: []string{"publish", "pages", "16", "17", "--no-pre-validate"},
	})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}
	files := backupFiles(t, dir)
	if len(files) != 2 {
		t.Fatalf("backup files = %v, want one per id", files)
	}
	for _, f := range files {
		if !strings.HasSuffix(f, "--publish.json") {
			t.Errorf("backup %s is not named after the command", f)
		}
	}
	if got := metaBackups(t, res); len(got) != 2 {
		t.Errorf("meta.backups = %v", got)
	}
	// One batched newest-version read; page 17's newest version is a draft,
	// so its main row is read too (is it published under that draft?).
	if len(backupNewestReads(cms.finds)) != 1 || len(cms.finds) != 2 {
		t.Errorf("backup reads = %v, want one batched read plus 17's main row", cms.finds)
	}
	var order []string
	for _, r := range srv.Requests() {
		if strings.HasPrefix(r.Path, "/api/pages") {
			order = append(order, r.Method)
		}
	}
	if len(order) == 0 || order[0] != "GET" {
		t.Errorf("request order = %v, want the backup read first", order)
	}

	// The dry run reports what it would back up and writes nothing.
	dry := cliRun(t, invocation{
		Home: home, Env: backupEnv(srv.URL, filepath.Join(t.TempDir(), "dry")),
		Args: []string{"publish", "pages", "16", "17", "--no-pre-validate", "--dry-run"},
	})
	if dry.Code != 0 {
		t.Fatalf("dry-run exit = %d\n%s", dry.Code, dry.Stdout)
	}
	wb, _ := dry.data(t)["would_backup"].(map[string]any)
	if wb == nil || fmt.Sprint(wb["documents"]) != "2" {
		t.Errorf("would_backup = %v", dry.data(t)["would_backup"])
	}
}
