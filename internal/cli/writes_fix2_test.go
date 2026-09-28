package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// ---------------------------------------------------------------------------
// non-canonical ids (`016`, ` 16`) — the pre-write backup must not be skipped
// ---------------------------------------------------------------------------

// Payload casts `016` to 16 in the URL path and in `where id in`, so the write
// reached the document while the backup — pairing returned documents with
// the requested ids by string — recorded it as skipped (verified on
// localhost:3900: `pay delete pages 0108 --yes` deleted page 108 with
// backup_skipped and no backup). The id is now canonical from validation on.
func TestNonCanonicalIDIsCanonicalizedAndBackedUp(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		path string
	}{
		{"update with a leading zero", []string{"update", "pages", "016", "--set", "title=Changed"}, "PATCH /api/pages/16"},
		{"update with a leading space", []string{"update", "pages", " 16", "--set", "title=Changed"}, "PATCH /api/pages/16"},
		{"delete --permanent", []string{"delete", "pages", "016", "--permanent", "--yes"}, "DELETE /api/pages/16"},
		{"unpublish", []string{"unpublish", "pages", "016", "--yes"}, "PATCH /api/pages/16"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cms := newFakeCMS()
			srv := cms.server(t)
			home := t.TempDir()
			seedDiscovery(t, home, srv.URL)
			dir := filepath.Join(t.TempDir(), "bk")
			res := cliRun(t, invocation{Home: home, Env: backupEnv(srv.URL, dir), Args: tc.args})
			if res.Code != 0 {
				t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
			}
			if res.hasWarning(t, WarnBackupSkipped) {
				t.Errorf("backup_skipped on an existing document: %v", res.warnings(t))
			}
			files := backupFiles(t, dir)
			if len(files) != 1 || !strings.Contains(files[0], filepath.Join("pages", "16")) {
				t.Fatalf("backup files = %v, want the pre-image of pages 16", files)
			}
			var sent []string
			for _, r := range srv.Requests() {
				if r.Method != http.MethodGet {
					sent = append(sent, r.Method+" "+r.Path)
				}
			}
			if len(sent) != 1 || sent[0] != tc.path {
				t.Errorf("writes = %v, want [%s]", sent, tc.path)
			}
		})
	}
}

// Several ids: `016` and `16` are one document — one write, one backup.
func TestPublishSeveralNonCanonicalIDs(t *testing.T) {
	cms := newFakeCMS()
	srv := cms.server(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	dir := filepath.Join(t.TempDir(), "bk")
	res := cliRun(t, invocation{Home: home, Env: backupEnv(srv.URL, dir),
		Args: []string{"unpublish", "pages", "016", "16", "017", "--yes"}})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
	}
	if res.hasWarning(t, WarnBackupSkipped) {
		t.Errorf("backup_skipped: %v", res.warnings(t))
	}
	if files := backupFiles(t, dir); len(files) != 2 {
		t.Errorf("backup files = %v, want one each for 16 and 17", files)
	}
	var paths []string
	for _, r := range srv.Requests() {
		if r.Method == http.MethodPatch {
			paths = append(paths, r.Path)
		}
	}
	if strings.Join(paths, ",") != "/api/pages/16,/api/pages/17" {
		t.Errorf("PATCHes = %v", paths)
	}
}

// Defense in depth for a collection whose id type PayCLI never learned (no
// canonicalization possible): an id the batched read did not return is read
// on its own before it is skipped, and backed up when the server has it.
func TestBackupRereadsAnIDTheBatchedReadDidNotReturn(t *testing.T) {
	cms := newFakeCMS()
	srv := cms.server(t, payloadtest.WithHandler("GET", "/api/pages/016", func(w http.ResponseWriter, _ *http.Request) {
		cms.mu.Lock()
		defer cms.mu.Unlock()
		writeJSON(w, 200, cms.pages["16"]) // Payload's cast: 016 is 16
	}))
	home := t.TempDir()
	dir := filepath.Join(t.TempDir(), "bk")
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
	res, err := d.BackupBeforeWrite(t.Context(), "delete", BackupTarget{Collection: &collTarget{Slug: "pages"}, IDs: []any{"016", "404"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Paths) != 1 || !strings.Contains(res.Paths[0], filepath.Join("pages", "16")) {
		t.Errorf("paths = %v, want the backup of pages 16", res.Paths)
	}
	if len(res.Skipped) != 1 || fmt.Sprint(res.Skipped[0]) != "404" {
		t.Errorf("skipped = %v, want only the id the server does not have", res.Skipped)
	}
	for _, w := range rt.Warnings() {
		if w.Code == WarnBackupSkipped && !strings.Contains(w.Hint, "doc_not_found") {
			t.Errorf("backup_skipped hint = %q: it must say the server answered doc_not_found", w.Hint)
		}
	}
}

func TestDocsByIDMatchesNumericIDsOnly(t *testing.T) {
	x := newDocsByID()
	x.add(payload.Doc{"id": json.Number("108"), "title": "num"})
	x.add(payload.Doc{"id": "0042", "title": "str"})
	cases := []struct {
		id   any
		want string
		ok   bool
	}{
		{"108", "108", true},
		{"0108", "108", true},
		{" 108", "108", true},
		{108, "108", true},
		{"0042", "0042", true},
		{"42", "", false}, // a string id is matched exactly: 42 is another document
		{"109", "", false},
	}
	for _, c := range cases {
		_, key, ok := x.lookup(c.id)
		if ok != c.ok || key != c.want {
			t.Errorf("lookup(%q) = %q %v, want %q %v", c.id, key, ok, c.want, c.ok)
		}
	}
}

// A multi-id dry run names an id it could not read instead of silently
// leaving it out of data.diff, and would_backup counts only what exists.
func TestSeveralIDsDryRunNamesMissingIDs(t *testing.T) {
	cms := newFakeCMS()
	srv := cms.server(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	dir := filepath.Join(t.TempDir(), "bk")
	res := cliRun(t, invocation{Home: home, Env: backupEnv(srv.URL, dir),
		Args: []string{"publish", "pages", "016", "404", "--no-pre-validate", "--dry-run"}})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
	}
	diff, _ := res.data(t)["diff"].(map[string]any)
	docs, _ := diff["documents"].([]any)
	if len(docs) != 1 {
		t.Errorf("diff.documents = %v, want page 16's entry", docs)
	}
	if fmt.Sprint(diff["missing"]) != "[404]" || !res.hasWarning(t, "diff_unavailable") {
		t.Errorf("missing = %v, warnings %v", diff["missing"], res.warnings(t))
	}
	wb, _ := res.data(t)["would_backup"].(map[string]any)
	if wb == nil || fmt.Sprint(wb["documents"]) != "1" {
		t.Errorf("would_backup = %v, want 1 document", res.data(t)["would_backup"])
	}
	if len(backupFiles(t, dir)) != 0 {
		t.Error("the dry run wrote a backup")
	}
}

// ---------------------------------------------------------------------------
// --draft --unpublish is refused everywhere
// ---------------------------------------------------------------------------

// `--draft --unpublish` sent ?draft=true with _status "draft": Payload saved a
// draft version and the page stayed live, while the echo said "draft"
// (verified on localhost:3900).
func TestDraftWithUnpublishIsRefused(t *testing.T) {
	cms := newFakeCMS()
	srv := cms.server(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	doc := `{"id":16,"title":"x","_status":"published"}`
	for _, tc := range []struct {
		name  string
		args  []string
		stdin string
	}{
		{"update", []string{"update", "pages", "16", "--set", "title=y", "--draft", "--unpublish", "--yes"}, ""},
		{"update dry run", []string{"update", "pages", "16", "--set", "title=y", "--draft", "--unpublish", "--dry-run"}, ""},
		{"apply", []string{"apply", "pages", "16", "--draft", "--unpublish", "--yes"}, doc},
		{"globals update", []string{"globals", "update", "header", "--set-json", "navItems=[]", "--draft", "--unpublish", "--yes"}, ""},
		{"versions restore", []string{"versions", "restore", "pages", "80", "--draft", "--unpublish", "--yes"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(srv.Requests())
			res := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), Args: tc.args, Stdin: tc.stdin})
			if res.Code != apierr.ExitValidation || res.code(t) != string(apierr.CodeInvalidArgs) {
				t.Fatalf("exit %d code %s, want 5 invalid_args\n%s", res.Code, res.code(t), res.Stdout)
			}
			errObj, _ := res.Env["error"].(map[string]any)
			if !strings.Contains(fmt.Sprint(errObj["message"]), "--draft and --unpublish") {
				t.Errorf("error = %v", errObj)
			}
			for _, r := range srv.Requests()[before:] {
				if r.Method != http.MethodGet {
					t.Errorf("a refused write sent %s %s", r.Method, r.Path)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// versions restore of a draft version over a published document
// ---------------------------------------------------------------------------

func versionsRestoreServer(t *testing.T) (*fakeCMS, *payloadtest.Server) {
	t.Helper()
	cms := newFakeCMS()
	version := func(id, parent int, status string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, 200, map[string]any{"id": id, "parent": parent,
				"version": map[string]any{"title": "Old", "_status": status}})
		}
	}
	restored := func(parent int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, 200, map[string]any{"doc": map[string]any{"id": parent, "title": "Old"}})
		}
	}
	srv := cms.server(t,
		payloadtest.WithHandler("GET", "/api/pages/versions/80", version(80, 16, "draft")),
		payloadtest.WithHandler("POST", "/api/pages/versions/80", restored(16)),
		payloadtest.WithHandler("GET", "/api/pages/versions/81", version(81, 16, "published")),
		payloadtest.WithHandler("POST", "/api/pages/versions/81", restored(16)),
		payloadtest.WithHandler("GET", "/api/pages/versions/82", version(82, 17, "draft")),
		payloadtest.WithHandler("POST", "/api/pages/versions/82", restored(17)),
	)
	return cms, srv
}

// payload@3.87.1 restoreVersion writes the version's own _status into the
// live row without ?draft: restoring a draft version unpublished a published
// page with exit 0 and no warning (verified on localhost:3900).
func TestVersionsRestoreOfADraftVersionOverAPublishedDocument(t *testing.T) {
	_, srv := versionsRestoreServer(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	env := seededEnv(srv.URL)
	restores := func() []payloadtest.Recorded {
		var out []payloadtest.Recorded
		for _, r := range srv.Requests() {
			if r.Method == http.MethodPost {
				out = append(out, r)
			}
		}
		return out
	}

	for _, extra := range []string{"--yes", "--dry-run"} {
		res := cliRun(t, invocation{Home: home, Env: env, Args: []string{"versions", "restore", "pages", "80", extra}})
		if res.code(t) != string(apierr.CodeInvalidArgs) || len(restores()) != 0 {
			t.Fatalf("%s: code %s, %d restores\n%s", extra, res.code(t), len(restores()), res.Stdout)
		}
		errObj, _ := res.Env["error"].(map[string]any)
		if !strings.Contains(fmt.Sprint(errObj["message"]), "UNPUBLISHES") ||
			!strings.Contains(fmt.Sprint(errObj["hint"]), "--draft") || !strings.Contains(fmt.Sprint(errObj["hint"]), "--unpublish") {
			t.Errorf("%s: error = %v", extra, errObj)
		}
	}

	// --draft: a new draft on top.
	res := cliRun(t, invocation{Home: home, Env: env, Args: []string{"versions", "restore", "pages", "80", "--draft", "--yes"}})
	if res.Code != 0 {
		t.Fatalf("--draft: exit %d\n%s", res.Code, res.Stdout)
	}
	if r := restores(); len(r) != 1 || !strings.Contains(r[0].Query, "draft=true") {
		t.Fatalf("restores = %+v", r)
	}
	// --unpublish: deliberate.
	res = cliRun(t, invocation{Home: home, Env: env, Args: []string{"versions", "restore", "pages", "80", "--unpublish", "--yes"}})
	if res.Code != 0 {
		t.Fatalf("--unpublish: exit %d\n%s", res.Code, res.Stdout)
	}
	if r := restores(); len(r) != 2 || strings.Contains(r[1].Query, "draft=true") {
		t.Fatalf("restores = %+v", r)
	}
	// A published version restores as before; --unpublish on it is refused.
	if res := cliRun(t, invocation{Home: home, Env: env, Args: []string{"versions", "restore", "pages", "81", "--yes"}}); res.Code != 0 {
		t.Fatalf("published version: exit %d\n%s", res.Code, res.Stdout)
	}
	res = cliRun(t, invocation{Home: home, Env: env, Args: []string{"versions", "restore", "pages", "81", "--unpublish", "--yes"}})
	if res.code(t) != string(apierr.CodeInvalidArgs) {
		t.Fatalf("--unpublish on a published version: code %s\n%s", res.code(t), res.Stdout)
	}
	// A draft version of a document that is not published: nothing to guard.
	if res := cliRun(t, invocation{Home: home, Env: env, Args: []string{"versions", "restore", "pages", "82", "--yes"}}); res.Code != 0 {
		t.Fatalf("draft document: exit %d\n%s", res.Code, res.Stdout)
	}
	if n := len(restores()); n != 4 {
		t.Errorf("restores = %d, want 4", n)
	}
}

// ---------------------------------------------------------------------------
// two-step backups restore: the dry run previews both steps
// ---------------------------------------------------------------------------

// The dry run used to diff only the draft body against the current newest
// version: the live row's change (step 1) was in no diff and `_status
// published → draft` read like an unpublish.
func TestTwoStepBackupsRestoreDryRunShowsThePublishedChange(t *testing.T) {
	s := pendingDraftStore(t)
	home := t.TempDir()
	seedDiscovery(t, home, s.srv.URL)
	dir := filepath.Join(t.TempDir(), "bk")
	env := backupEnv(s.srv.URL, dir)

	// A publish backs up both states (DRAFT-B on top of PUB-A), then goes
	// live with DRAFT-B.
	if res := cliRun(t, invocation{Home: home, Env: env, Args: []string{"publish", "pages", "1", "--no-pre-validate", "--yes"}}); res.Code != 0 {
		t.Fatalf("publish: exit %d\n%s", res.Code, res.Stdout)
	}
	file := backupFiles(t, dir)[0]
	if s.main["pages"]["1"]["title"] != "DRAFT-B" || s.main["pages"]["1"]["_status"] != "published" {
		t.Fatalf("main row = %v", s.main["pages"]["1"])
	}

	dry := cliRun(t, invocation{Home: home, Env: env, Args: []string{"backups", "restore", file, "--dry-run"}})
	if dry.Code != 0 {
		t.Fatalf("dry run exit = %d\n%s", dry.Code, dry.Stdout)
	}
	diff, _ := dry.data(t)["diff"].(map[string]any)
	pub, _ := diff["published"].(map[string]any)
	if pub == nil || pub["base"] != diffBasePublished {
		t.Fatalf("data.diff.published = %v, want the live row's change (step 1)", diff)
	}
	changes := fmt.Sprint(pub["changes"])
	if !strings.Contains(changes, "DRAFT-B") || !strings.Contains(changes, "PUB-A") {
		t.Errorf("data.diff.published changes = %s, want title DRAFT-B -> PUB-A", changes)
	}
	if diff["base"] != diffBaseLatest {
		t.Errorf("data.diff.base = %v", diff["base"])
	}
	if !dry.hasWarning(t, WarnRestoreTwoSteps) {
		t.Errorf("no %s warning: %v", WarnRestoreTwoSteps, dry.warnings(t))
	}
	if s.main["pages"]["1"]["title"] != "DRAFT-B" {
		t.Error("the dry run wrote")
	}
}
