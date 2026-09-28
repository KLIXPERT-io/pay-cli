package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

func TestExplicitIDsKeepThePerDocumentRiskLevel(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want safety.Level
	}{{safety.CmdPublish, safety.L1}, {safety.CmdUnpublish, safety.L2}} {
		if got := (safety.Op{Command: tc.cmd, Selector: safety.SelectorIDs}).Level(); got != tc.want {
			t.Errorf("%s with explicit ids = %v, want %v", tc.cmd, got, tc.want)
		}
	}
	line := safety.Request{Op: safety.Op{Command: "unpublish", Selector: safety.SelectorIDs}, Target: "pages", Affected: 3}.Line()
	if !strings.Contains(line, "3 explicitly named document(s)") {
		t.Errorf("confirmation line = %q", line)
	}
}

func TestPublishSeveralIDs(t *testing.T) {
	fs := newFakeSite(t)
	home := t.TempDir()
	seedDiscovery(t, home, fs.URL)

	t.Run("all succeed: bulk_result, exit 0, one PATCH per id", func(t *testing.T) {
		res := urlRun(t, fs, home, nil, "publish", "pages", "2", "3", "2")
		if res.Code != 0 || res.Env["data_kind"] != "bulk_result" {
			t.Fatalf("exit %d\n%s", res.Code, res.Stdout)
		}
		d := res.data(t)
		if n := len(d["succeeded"].([]any)); n != 2 {
			t.Errorf("succeeded = %d, want 2 (the duplicate id is written once)", n)
		}
		if n := fs.Count("PATCH", "/api/pages/"); n != 2 {
			t.Errorf("PATCH count = %d, want 2", n)
		}
	})

	t.Run("partial failure: exit 7 and a retry of only the failed id", func(t *testing.T) {
		fs.Reset()
		res := urlRun(t, fs, home, nil, "publish", "pages", "1", "7", "--no-pre-validate")
		if res.Code != apierr.ExitPartial || res.code(t) != string(apierr.CodePartialFailure) {
			t.Fatalf("exit %d code %s\n%s", res.Code, res.code(t), res.Stdout)
		}
		d := res.Env["data"].(map[string]any)
		if len(d["succeeded"].([]any)) != 1 || len(d["failed"].([]any)) != 1 {
			t.Fatalf("data = %v", d)
		}
		next := res.Env["next"].(map[string]any)
		if next["cmd"] != "pay publish pages 7" {
			t.Errorf("next.cmd = %v", next["cmd"])
		}
	})

	t.Run("each failure keeps its own code", func(t *testing.T) {
		res := urlRun(t, fs, home, nil, "publish", "pages", "1", "99", "--no-pre-validate")
		failures := res.Env["error"].(map[string]any)["failures"].([]any)
		if len(failures) != 1 || failures[0].(map[string]any)["code"] != string(apierr.CodeDocNotFound) {
			t.Fatalf("failures = %v, want doc_not_found for the missing id", failures)
		}
		// A missing id fails the same way on every retry: it is not in
		// next.cmd (here nothing is left to retry), and the hint names it
		// instead of telling the agent not to re-run a per-id write.
		if next, ok := res.Env["next"].(map[string]any); ok && next != nil {
			t.Errorf("next = %v, want none: the only failure is a missing document", next)
		}
		hint := res.Env["error"].(map[string]any)["hint"].(string)
		if !strings.Contains(hint, "pages 99 do(es) not exist") || strings.Contains(hint, "NOT transactional") {
			t.Errorf("hint = %q", hint)
		}
	})

	t.Run("a missing id is left out of the retry of the others", func(t *testing.T) {
		res := urlRun(t, fs, home, nil, "publish", "pages", "99", "7", "1", "--no-pre-validate")
		next := res.Env["next"].(map[string]any)
		if next["cmd"] != "pay publish pages 7" {
			t.Errorf("next.cmd = %v, want only the id a retry can fix", next["cmd"])
		}
		if ids := fmt.Sprint(next["args"].(map[string]any)["ids"]); ids != "[7]" {
			t.Errorf("next.args.ids = %v, want the ids of next.cmd", ids)
		}
	})

	t.Run("--fail-fast leaves the rest not_attempted", func(t *testing.T) {
		res := urlRun(t, fs, home, nil, "publish", "pages", "7", "1", "--no-pre-validate", "--fail-fast")
		d := res.Env["data"].(map[string]any)
		if na := d["not_attempted"].([]any); len(na) != 1 {
			t.Fatalf("not_attempted = %v", na)
		}
		if next := res.Env["next"].(map[string]any)["cmd"]; next != "pay publish pages 7 1" {
			t.Errorf("next.cmd = %v", next)
		}
	})

	t.Run("unpublish several is L2: exit 11 without --yes, then per-id", func(t *testing.T) {
		res := urlRun(t, fs, home, nil, "unpublish", "pages", "1", "3")
		if res.Code != apierr.ExitConfirmationRequired {
			t.Fatalf("exit %d, want 11\n%s", res.Code, res.Stdout)
		}
		fs.Reset()
		res = urlRun(t, fs, home, nil, "unpublish", "pages", "1", "3", "--yes")
		if res.Code != 0 || fs.Count("PATCH", "/api/pages/") != 2 {
			t.Fatalf("exit %d, %d PATCHes\n%s", res.Code, fs.Count("PATCH", "/api/pages/"), res.Stdout)
		}
	})

	t.Run("dry run sends nothing", func(t *testing.T) {
		fs.Reset()
		res := urlRun(t, fs, home, nil, "publish", "pages", "1", "3", "--dry-run")
		if res.Code != 0 || fs.Count("PATCH", "/api/pages/") != 0 {
			t.Fatalf("exit %d, PATCHes %d\n%s", res.Code, fs.Count("PATCH", "/api/pages/"), res.Stdout)
		}
		if d := res.data(t); d["would_affect"].(interface{ String() string }).String() != "2" {
			t.Errorf("would_affect = %v", d["would_affect"])
		}
	})

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"ids and --where", []string{"publish", "pages", "1", "2", "--where", "_status eq draft"}},
		{"ids and --max-docs", []string{"publish", "pages", "1", "2", "--max-docs", "5"}},
		{"a malformed id fails the whole list first", []string{"publish", "pages", "1", "abc"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs.Reset()
			res := urlRun(t, fs, home, nil, tc.args...)
			if res.Code != apierr.ExitValidation {
				t.Fatalf("exit %d, want 5\n%s", res.Code, res.Stdout)
			}
			if n := fs.Count("PATCH", "/api/pages/"); n != 0 {
				t.Fatalf("%d PATCH(es) were sent before the refusal", n)
			}
		})
	}
}
