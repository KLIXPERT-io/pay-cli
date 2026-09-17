package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/audit"
	"github.com/KLIXPERT-io/pay-cli/internal/config"
)

// writeAuditFixture seeds an audit log under $PAY_HOME so `pay audit tail` has
// something to read. It writes through the real logger so the on-disk shape is
// the one production produces.
func writeAuditFixture(t *testing.T, home string, events ...audit.Event) string {
	t.Helper()
	paths := config.PathsFor(config.NewEnv([]string{"PAY_HOME=" + home}), runtime.GOOS, home)
	path := paths.AuditFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	logger := audit.New(audit.Options{Path: path, Now: func() time.Time { return opsClock }})
	for _, e := range events {
		if e.Phase == audit.PhasePost {
			if w := logger.Post(e); w != nil {
				t.Fatalf("audit post: %s", w.Message)
			}
			continue
		}
		if err := logger.Pre(e); err != nil {
			t.Fatalf("audit pre: %v", err)
		}
	}
	return path
}

func TestAuditTailOnAnEmptyLog(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	res := opsRun(t, home, "audit", "tail")
	if res.Code != 0 {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", res.Code, res.Stdout, res.Stderr)
	}
	data := res.data(t)
	events, ok := data["events"].([]any)
	if !ok {
		t.Fatalf("events = %#v, want []", data["events"])
	}
	if len(events) != 0 {
		t.Fatalf("events = %v, want empty", events)
	}
	if enabled, _ := data["enabled"].(bool); !enabled {
		t.Fatalf("enabled = false; an agent cannot tell an empty log from a disabled one")
	}
}

func TestAuditTailReturnsRecords(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	writeAuditFixture(t, home,
		audit.Event{Command: "delete", Collection: "pages", Action: "delete", Method: "DELETE", Path: "/api/pages/1", IDs: []string{"1"}},
		audit.Event{Command: "delete", Collection: "pages", Action: "delete", Method: "DELETE", Path: "/api/pages/1", Phase: audit.PhasePost, OK: true, Affected: 1, Status: 200},
		audit.Event{Command: "create", Collection: "posts", Action: "create", Method: "POST", Path: "/api/posts"},
		audit.Event{Command: "create", Collection: "posts", Action: "create", Method: "POST", Path: "/api/posts", Phase: audit.PhasePost, OK: true, Affected: 1, Status: 201},
	)

	res := opsRun(t, home, "audit", "tail")
	if res.Code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr: %s", res.Code, res.Stderr)
	}
	data := res.data(t)
	events, _ := data["events"].([]any)
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4\n%s", len(events), res.Stdout)
	}
	returned, _ := data["returned"].(json.Number)
	if returned.String() != "4" {
		t.Fatalf("returned = %s, want 4", returned)
	}
}

func TestAuditTailFilters(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	writeAuditFixture(t, home,
		audit.Event{Command: "delete", Collection: "pages", Action: "delete", Method: "DELETE"},
		audit.Event{Command: "create", Collection: "posts", Action: "create", Method: "POST"},
	)

	cases := []struct {
		name string
		args []string
		want int
	}{
		{"by action", []string{"--action", "delete"}, 1},
		{"by collection", []string{"--collection", "posts"}, 1},
		{"by command", []string{"--command", "create"}, 1},
		{"by phase", []string{"--phase", "pre"}, 2},
		{"no filter", nil, 2},
		{"n caps", []string{"-n", "1"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"audit", "tail"}, tc.args...)
			res := opsRun(t, home, args...)
			if res.Code != 0 {
				t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
			}
			events, _ := res.data(t)["events"].([]any)
			if len(events) != tc.want {
				t.Fatalf("events = %d, want %d\n%s", len(events), tc.want, res.Stdout)
			}
		})
	}
}

func TestAuditTailRejectsAnUnknownAction(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	res := opsRun(t, home, "audit", "tail", "--action", "deleted")
	if code := res.errorCode(t); code != "invalid_option" {
		t.Fatalf("error.code = %q, want invalid_option\n%s", code, res.Stdout)
	}
	if res.Code != 5 {
		t.Fatalf("exit = %d, want 5", res.Code)
	}
}

func TestAuditTailRejectsANegativeN(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	res := opsRun(t, home, "audit", "tail", "-n", "-3")
	if code := res.errorCode(t); code != "invalid_args" {
		t.Fatalf("error.code = %q, want invalid_args\n%s", code, res.Stdout)
	}
}

func TestAuditTailWarnsWhenAuditingIsDisabled(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	res := opsRun(t, home, "audit", "tail", "--no-audit")
	if res.Code != 0 {
		t.Fatalf("exit = %d, want 0\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}
	warnings, _ := res.Envelope["warnings"].([]any)
	found := false
	for _, w := range warnings {
		m, _ := w.(map[string]any)
		if code, _ := m["code"].(string); code == "audit_disabled" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no audit_disabled warning under --no-audit\n%s", res.Stdout)
	}
}

func TestAuditNextPointsAtAnInterruptedWrite(t *testing.T) {
	t.Parallel()
	base := opsClock
	cases := []struct {
		name    string
		events  []audit.Event
		wantCmd string
	}{
		{
			name: "pre with no post",
			events: []audit.Event{
				{Phase: audit.PhasePre, Command: "delete", Collection: "pages", Method: "DELETE", IDs: []string{"16"}, Time: base},
			},
			wantCmd: "pay get pages 16",
		},
		{
			name: "pre with a matching post",
			events: []audit.Event{
				{Phase: audit.PhasePre, Command: "delete", Collection: "pages", Method: "DELETE", IDs: []string{"16"}, Time: base},
				{Phase: audit.PhasePost, Command: "delete", Collection: "pages", Method: "DELETE", Time: base.Add(time.Second)},
			},
			wantCmd: "",
		},
		{
			name:    "no events",
			events:  nil,
			wantCmd: "",
		},
		{
			name: "pre with no ids",
			events: []audit.Event{
				{Phase: audit.PhasePre, Command: "delete", Collection: "pages", Method: "DELETE", Time: base},
			},
			wantCmd: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			next := auditNext(auditTailData{Events: tc.events})
			if tc.wantCmd == "" {
				if next != nil {
					t.Fatalf("next = %+v, want nil", next)
				}
				return
			}
			if next == nil {
				t.Fatalf("next = nil, want %q", tc.wantCmd)
			}
			if next.Cmd != tc.wantCmd {
				t.Fatalf("next.cmd = %q, want %q", next.Cmd, tc.wantCmd)
			}
		})
	}
}

func TestAuditPathIsAlwaysAnswerable(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	res := opsRun(t, home, "audit", "path")
	if res.Code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr: %s", res.Code, res.Stderr)
	}
	if p, _ := res.data(t)["path"].(string); filepath.Base(p) != audit.FileName {
		t.Fatalf("path = %q, want it to end in %s", p, audit.FileName)
	}
}

// The repo-wide secret assertion of §3.1, applied to this command group: no
// byte an operational command writes may contain a credential.
func TestAuditTailNeverEchoesACredential(t *testing.T) {
	t.Parallel()
	const fixtureKey = "paycli-dev-key-deadbeefdeadbeef"
	home := t.TempDir()
	writeAuditFixture(t, home, audit.Event{
		Command: "raw", Action: "raw", Method: "GET",
		Path: "http://localhost:3900/api/users/me?apiKey=" + fixtureKey,
	})

	res := opsRunEnv(t, home, []string{"PAY_API_KEY=" + fixtureKey}, "audit", "tail")
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
	}
	for name, stream := range map[string]string{"stdout": res.Stdout, "stderr": res.Stderr} {
		if strings.Contains(stream, fixtureKey) {
			t.Fatalf("%s leaked the fixture API key:\n%s", name, stream)
		}
	}
	onDisk, err := os.ReadFile(writeAuditFixture(t, home))
	if err == nil && strings.Contains(string(onDisk), fixtureKey) {
		t.Fatalf("the audit log on disk contains the fixture API key")
	}
}
