package audit

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

const fixtureKey = "paycli-dev-key-deadbeefdeadbeef"

func fixedNow() time.Time { return time.Date(2026, 9, 16, 17, 12, 0, 0, time.UTC) }

func newTestLogger(t *testing.T, opts Options) (*Logger, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg", FileName)
	opts.Path = path
	if opts.Now == nil {
		opts.Now = fixedNow
	}
	return New(opts), path
}

func readLines(t *testing.T, path string) []Event {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []Event
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %q is not JSON: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

func TestPreAndPostRecords(t *testing.T) {
	l, path := newTestLogger(t, Options{})

	ev := Event{
		Command:    "delete",
		Profile:    "dev",
		BaseURL:    "http://localhost:3900",
		Collection: "crm-contacts",
		Action:     "trash",
		Method:     "PATCH",
		Path:       "/api/crm-contacts/224",
		RequestID:  "01K5Q7V0",
	}
	if err := l.Pre(ev); err != nil {
		t.Fatalf("Pre() = %v", err)
	}
	ev.OK = true
	ev.Status = 200
	ev.Affected = 1
	ev.DurationMS = 22
	if w := l.Post(ev); w != nil {
		t.Fatalf("Post() = %+v, want nil", w)
	}

	events := readLines(t, path)
	if len(events) != 2 {
		t.Fatalf("got %d records, want 2", len(events))
	}
	if events[0].Phase != PhasePre || events[0].OK {
		t.Errorf("pre record = %+v", events[0])
	}
	if events[1].Phase != PhasePost || !events[1].OK || events[1].Affected != 1 {
		t.Errorf("post record = %+v", events[1])
	}
	if !events[0].Time.Equal(fixedNow()) {
		t.Errorf("time = %v, want the injected clock", events[0].Time)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != FilePerm {
		t.Errorf("mode = %v, want %v", info.Mode().Perm(), FilePerm)
	}
	if runtime.GOOS != "windows" {
		di, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if di.Mode().Perm() != 0o700 {
			t.Errorf("dir mode = %v, want 0700", di.Mode().Perm())
		}
	}
}

func TestDisabledLoggerWritesNothing(t *testing.T) {
	tests := []struct {
		name string
		opts Options
	}{
		{"--no-audit", Options{Disabled: true}},
		{"no path", Options{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			opts := tc.opts
			if tc.name == "--no-audit" {
				opts.Path = filepath.Join(dir, FileName)
			}
			opts.Now = fixedNow
			l := New(opts)
			if l.Enabled() {
				t.Fatal("logger should be disabled")
			}
			if err := l.Pre(Event{Command: "delete"}); err != nil {
				t.Fatalf("Pre() = %v", err)
			}
			if w := l.Post(Event{Command: "delete"}); w != nil {
				t.Fatalf("Post() = %+v", w)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Fatalf("disabled logger created %v", entries)
			}
		})
	}
}

func TestPreFailureAbortsAndPostFailureWarns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission semantics")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })

	l := New(Options{Path: filepath.Join(locked, "sub", FileName), Now: fixedNow})

	err := l.Pre(Event{Command: "delete"})
	if err == nil {
		t.Fatal("Pre() = nil, want audit_write_failed")
	}
	if got := apierr.CodeOf(err); got != apierr.CodeAuditWriteFailed {
		t.Fatalf("code = %s, want audit_write_failed", got)
	}
	if got := apierr.ExitCode(err); got != apierr.ExitInternal {
		t.Fatalf("exit = %d, want 1", got)
	}
	e, _ := apierr.As(err)
	if !strings.Contains(e.Hint, "--no-audit") {
		t.Errorf("hint = %q, want the --no-audit escape hatch", e.Hint)
	}

	w := l.Post(Event{Command: "delete", OK: true})
	if w == nil {
		t.Fatal("Post() = nil, want a warning")
	}
	if w.Code != WarnCode {
		t.Fatalf("warning code = %q, want %q", w.Code, WarnCode)
	}
}

func TestSanitize(t *testing.T) {
	l, path := newTestLogger(t, Options{Secrets: []string{fixtureKey}})

	ev := Event{
		Command:    "update",
		Profile:    "dev",
		BaseURL:    "https://admin:hunter2@example.com",
		Collection: "users",
		Action:     "update",
		Method:     "PATCH",
		Path:       "/api/users?where=%7B%7D&api_key=" + fixtureKey,
		Where:      json.RawMessage(`{"apiKey":{"equals":"` + fixtureKey + `"}}`),
		IDs:        []string{"66"},
		Err:        "Authorization: users API-Key " + fixtureKey,
	}
	if err := l.Pre(ev); err != nil {
		t.Fatalf("Pre() = %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), fixtureKey) {
		t.Fatalf("audit line leaked the credential:\n%s", raw)
	}
	if strings.Contains(string(raw), "hunter2") {
		t.Fatalf("audit line leaked base_url userinfo:\n%s", raw)
	}
	got := readLines(t, path)[0]
	if got.BaseURL != "https://example.com" {
		t.Errorf("base_url = %q, want the userinfo stripped", got.BaseURL)
	}
}

func TestIDsAreCappedAt200(t *testing.T) {
	l, path := newTestLogger(t, Options{})
	ids := make([]string, 250)
	for i := range ids {
		ids[i] = "id-" + string(rune('a'+i%26))
	}
	if err := l.Pre(Event{Command: "delete", IDs: ids}); err != nil {
		t.Fatal(err)
	}
	got := readLines(t, path)[0]
	if len(got.IDs) != MaxIDs {
		t.Fatalf("ids = %d, want %d", len(got.IDs), MaxIDs)
	}
	if !got.IDsTruncated {
		t.Fatal("ids_truncated = false, want true")
	}
}

func TestWritable(t *testing.T) {
	l, path := newTestLogger(t, Options{})
	if err := l.Writable(); err != nil {
		t.Fatalf("Writable() = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Writable() did not create the log: %v", err)
	}
	if err := New(Options{Disabled: true}).Writable(); err != nil {
		t.Fatalf("disabled Writable() = %v", err)
	}
}

func TestTail(t *testing.T) {
	l, _ := newTestLogger(t, Options{})
	base := fixedNow()
	for i, action := range []string{"create", "update", "delete", "trash"} {
		ev := Event{
			Time:       base.Add(time.Duration(i) * time.Minute),
			Command:    action,
			Action:     action,
			Profile:    "dev",
			Collection: "pages",
			OK:         true,
		}
		if w := l.Post(ev); w != nil {
			t.Fatalf("Post() = %+v", w)
		}
	}

	tests := []struct {
		name string
		opts TailOptions
		want []string
	}{
		{"default order", TailOptions{}, []string{"create", "update", "delete", "trash"}},
		{"-n 2 keeps the newest", TailOptions{N: 2}, []string{"delete", "trash"}},
		{"--action delete", TailOptions{Action: "delete"}, []string{"delete"}},
		{"--since", TailOptions{Since: base.Add(2 * time.Minute)}, []string{"delete", "trash"}},
		{"unknown profile", TailOptions{Profile: "prod"}, nil},
		{"phase filter", TailOptions{Phase: PhasePre}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := l.Tail(tc.opts)
			if err != nil {
				t.Fatalf("Tail() = %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d events, want %d (%v)", len(got), len(tc.want), tc.want)
			}
			for i, want := range tc.want {
				if got[i].Action != want {
					t.Errorf("event %d = %q, want %q", i, got[i].Action, want)
				}
			}
		})
	}
}

func TestTailSkipsCorruptLinesAndMissingFile(t *testing.T) {
	l, path := newTestLogger(t, Options{})
	if err := l.Post(Event{Command: "create", Action: "create"}); err != nil {
		t.Fatalf("Post() = %+v", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, FilePerm)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"time\": truncated\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	got, err := l.Tail(TailOptions{})
	if err != nil {
		t.Fatalf("Tail() = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}

	missing := New(Options{Path: filepath.Join(t.TempDir(), FileName), Now: fixedNow})
	events, err := missing.Tail(TailOptions{IncludeRotated: true})
	if err != nil {
		t.Fatalf("Tail() on a missing log = %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("got %d events, want 0", len(events))
	}
	if !errors.Is(func() error { _, err := os.Stat(missing.Path()); return err }(), fs.ErrNotExist) {
		t.Fatal("Tail must not create the log")
	}
}

func TestRotation(t *testing.T) {
	l, path := newTestLogger(t, Options{MaxBytes: 400, Generations: 3})

	// Each record is well over 100 bytes, so a handful of writes forces
	// several rotations.
	for i := 0; i < 24; i++ {
		ev := Event{
			Command:    "update",
			Profile:    "dev",
			BaseURL:    "http://localhost:3900",
			Collection: "pages",
			Action:     "update",
			Method:     "PATCH",
			Path:       "/api/pages/" + string(rune('a'+i%26)),
			RequestID:  "req-0000000000",
		}
		if w := l.Post(ev); w != nil {
			t.Fatalf("Post() = %+v", w)
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 400 {
		t.Fatalf("live log is %d bytes, want <= 400", info.Size())
	}
	gens := l.Generations()
	if len(gens) != 3 {
		t.Fatalf("generations = %v, want 3", gens)
	}
	if _, err := os.Stat(path + ".4"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a 4th generation survived rotation")
	}
	// Rotated content is still readable and chronological.
	events, err := l.Tail(TailOptions{N: 100, IncludeRotated: true})
	if err != nil {
		t.Fatalf("Tail() = %v", err)
	}
	if len(events) < 4 {
		t.Fatalf("got %d events across generations, want several", len(events))
	}
}

func TestRotationKeepsAnOversizedRecord(t *testing.T) {
	l, path := newTestLogger(t, Options{MaxBytes: 1, Generations: 2})
	if w := l.Post(Event{Command: "create", Action: "create"}); w != nil {
		t.Fatalf("Post() = %+v", w)
	}
	if w := l.Post(Event{Command: "update", Action: "update"}); w != nil {
		t.Fatalf("Post() = %+v", w)
	}
	if got := readLines(t, path); len(got) != 1 || got[0].Action != "update" {
		t.Fatalf("live log = %+v, want only the newest record", got)
	}
	if got := readLines(t, path+".1"); len(got) != 1 || got[0].Action != "create" {
		t.Fatalf("generation 1 = %+v, want the rotated record", got)
	}
}

func TestRotatedNames(t *testing.T) {
	for i, want := range map[int]string{1: "/tmp/audit.log.1", 3: "/tmp/audit.log.3"} {
		if got := rotatedName("/tmp/audit.log", i); got != want {
			t.Errorf("rotatedName(%d) = %q, want %q", i, got, want)
		}
	}
}
