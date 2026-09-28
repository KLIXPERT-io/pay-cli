package backup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

var t0 = time.Date(2026, 9, 28, 12, 0, 0, 123_000_000, time.UTC)

func record(profile, collection string, id any, command string, at time.Time) *Record {
	return &Record{
		Time: at, Profile: profile, BaseURL: "http://localhost:3900",
		Collection: collection, ID: id, Command: command,
		Doc: map[string]any{"id": id, "title": "before"},
	}
}

func TestComponent(t *testing.T) {
	cases := map[string]string{
		"pages":            "pages",
		"crm-contacts":     "crm-contacts",
		"64f1c0ffee00a1b2": "64f1c0ffee00a1b2",
		"a/b":              "a_b",
		`..`:               "_..",
		".":                "_.",
		"":                 "_",
		"klixpert stage":   "klixpert_stage",
		`C:\evil`:          "C__evil",
	}
	for in, want := range cases {
		if got := Component(in); got != want {
			t.Errorf("Component(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFileNameRoundTrip(t *testing.T) {
	name := FileName(t0, "globals update")
	if name != "20260928T120000.123Z--globals-update.json" {
		t.Fatalf("FileName = %q", name)
	}
	for _, n := range []string{name, "20260928T120000.123Z--globals-update.2.json"} {
		at, cmd, ok := ParseFileName(n)
		if !ok || !at.Equal(t0) || cmd != "globals-update" {
			t.Errorf("ParseFileName(%q) = %v %q %v", n, at, cmd, ok)
		}
	}
	for _, bad := range []string{"notes.txt", "x.json", ".pay-probe-1.json", "20260928T120000.123Z--.json"} {
		if _, _, ok := ParseFileName(bad); ok {
			t.Errorf("ParseFileName(%q) accepted a non-backup name", bad)
		}
	}
}

func TestWriteLayoutModesAndContent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "backups")
	s := New(root)
	path, err := s.Write(record("local", "pages", json.Number("16"), "update", t0))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "local", "pages", "16", "20260928T120000.123Z--update.json")
	if path != want {
		t.Fatalf("path = %s, want %s", path, want)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("file mode = %v, want 0600", info.Mode().Perm())
		}
		dir, _ := os.Stat(filepath.Dir(path))
		if dir.Mode().Perm() != 0o700 {
			t.Errorf("dir mode = %v, want 0700", dir.Mode().Perm())
		}
	}
	rec, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if rec.PayBackup != FormatVersion || rec.Collection != "pages" || rec.IDString() != "16" {
		t.Fatalf("record = %+v", rec)
	}
	// Numbers survive exactly: the id is a JSON number, not a string.
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"id": 16`) {
		t.Errorf("the id did not stay a number:\n%s", raw)
	}
	if rec.Doc["title"] != "before" {
		t.Errorf("doc = %v", rec.Doc)
	}
}

func TestWriteGlobalLayout(t *testing.T) {
	root := t.TempDir()
	rec := &Record{Time: t0, Profile: "stage", Global: "header", Command: "globals update",
		Doc: map[string]any{"navItems": []any{}}}
	path, err := New(root).Write(rec)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "stage", GlobalsDir, "header", "20260928T120000.123Z--globals-update.json"); path != want {
		t.Fatalf("path = %s, want %s", path, want)
	}
}

func TestWriteNeverOverwrites(t *testing.T) {
	s := New(t.TempDir())
	a, err := s.Write(record("p", "pages", 1, "update", t0))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Write(record("p", "pages", 1, "update", t0))
	if err != nil {
		t.Fatal(err)
	}
	if a == b || !strings.HasSuffix(b, "--update.2.json") {
		t.Fatalf("second backup in the same millisecond: %s then %s", a, b)
	}
	entries, _ := s.List(Filter{})
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want both files", len(entries))
	}
}

// A root that is a FILE must fail fast. It used to spin forever in unique():
// Lstat answered ENOTDIR, which is not "not exist", for every candidate name.
func TestWriteIntoAFileFailsFast(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := New(file).Write(record("p", "pages", 1, "update", t0))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("writing below a file succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Write did not return: unique() is probing forever")
	}
	if err := New(file).Writable("p"); err == nil {
		t.Fatal("Writable reported a file as a writable directory")
	}
}

func TestListFiltersAndOrder(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	must := func(r *Record) {
		t.Helper()
		if _, err := s.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	must(record("local", "pages", 1, "update", t0))
	must(record("local", "pages", 1, "delete", t0.Add(time.Hour)))
	must(record("local", "posts", 7, "publish", t0.Add(2*time.Hour)))
	must(&Record{Time: t0.Add(3 * time.Hour), Profile: "local", Global: "header", Command: "globals update", Doc: map[string]any{}})
	must(record("stage", "pages", 1, "update", t0))
	// Noise the walk must skip.
	_ = os.WriteFile(filepath.Join(root, "README.txt"), []byte("x"), 0o600)
	_ = os.WriteFile(filepath.Join(root, "local", "pages", "1", "notes.json"), []byte("{}"), 0o600)

	all, err := s.List(Filter{Profile: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("local entries = %d, want 4: %+v", len(all), all)
	}
	if all[0].Global != "header" || all[3].Command != "update" {
		t.Errorf("not newest first: %+v", all)
	}
	for _, tc := range []struct {
		name string
		f    Filter
		want int
	}{
		{"every profile", Filter{}, 5},
		{"collection", Filter{Profile: "local", Collection: "pages"}, 2},
		{"id", Filter{Profile: "local", Collection: "pages", ID: "1"}, 2},
		{"command with a space", Filter{Profile: "local", Command: "globals update"}, 1},
		{"global", Filter{Profile: "local", Global: "header"}, 1},
		{"since", Filter{Profile: "local", Since: t0.Add(90 * time.Minute)}, 2},
		{"before", Filter{Profile: "local", Before: t0.Add(30 * time.Minute)}, 1},
		{"unknown profile", Filter{Profile: "nope"}, 0},
	} {
		got, err := s.List(tc.f)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got) != tc.want {
			t.Errorf("%s: %d entries, want %d: %+v", tc.name, len(got), tc.want, got)
		}
	}
	if got, err := New(filepath.Join(root, "missing")).List(Filter{}); err != nil || len(got) != 0 {
		t.Errorf("a missing root must list as empty, got %v %v", got, err)
	}
}

func TestPruneRemovesFilesAndEmptyDirs(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	old, _ := s.Write(record("local", "pages", 1, "update", t0))
	keep, _ := s.Write(record("local", "pages", 2, "update", t0.Add(48*time.Hour)))

	cut := Filter{Profile: "local", Before: t0.Add(24 * time.Hour)}
	dry, err := s.Prune(cut, true)
	if err != nil || len(dry) != 1 {
		t.Fatalf("dry run = %v %v", dry, err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("a dry run removed a file")
	}
	gone, err := s.Prune(cut, false)
	if err != nil || len(gone) != 1 || gone[0].Path != old {
		t.Fatalf("prune = %v %v", gone, err)
	}
	if _, err := os.Stat(filepath.Dir(old)); !os.IsNotExist(err) {
		t.Errorf("the emptied document directory was left behind")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("prune removed a newer backup: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("prune removed the root itself: %v", err)
	}
}

func TestLoadRefusesForeignFiles(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"notjson.json":  "{",
		"version.json":  `{"pay_backup": 2, "collection": "pages", "doc": {}}`,
		"nodoc.json":    `{"pay_backup": 1, "collection": "pages"}`,
		"notarget.json": `{"pay_backup": 1, "doc": {}}`,
	}
	for name, body := range cases {
		p := filepath.Join(dir, name)
		_ = os.WriteFile(p, []byte(body), 0o600)
		_, err := Load(p)
		if !apierr.HasCode(err, apierr.CodeBadRequestBody) {
			t.Errorf("%s: err = %v, want bad_request_body", name, err)
		}
	}
	if _, err := Load(filepath.Join(dir, "absent.json")); !apierr.HasCode(err, apierr.CodeFileMissing) {
		t.Errorf("missing file: err = %v", err)
	}
}
