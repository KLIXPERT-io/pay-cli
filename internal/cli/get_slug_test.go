package cli

import (
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

func TestGetBySlug(t *testing.T) {
	fs := newFakeSite(t)
	home := t.TempDir()
	seedDiscovery(t, home, fs.URL)

	res := urlRun(t, fs, home, nil, "get", "pages", "--slug", "about")
	if res.Code != 0 || res.Env["data_kind"] != "doc" {
		t.Fatalf("exit %d\n%s", res.Code, res.Stdout)
	}
	if d := res.data(t); d["slug"] != "about" || d["id"].(interface{ String() string }).String() != "1" {
		t.Fatalf("doc = %v", d)
	}
	// --draft finds a slug that only exists in the newest draft, and reads that draft.
	res = urlRun(t, fs, home, nil, "get", "pages", "--slug", "changed-new", "--draft")
	if res.Code != 0 || res.data(t)["slug"] != "changed-new" {
		t.Fatalf("--draft --slug: exit %d\n%s", res.Code, res.Stdout)
	}
	for _, tc := range []struct {
		name string
		args []string
		code apierr.Code
	}{
		{"ambiguous", []string{"get", "pages", "--slug", "dup"}, apierr.CodeSlugAmbiguous},
		{"missing", []string{"get", "pages", "--slug", "nope"}, apierr.CodeDocNotFound},
		{"neither id nor slug", []string{"get", "pages"}, apierr.CodeInvalidArgs},
		{"both id and slug", []string{"get", "pages", "1", "--slug", "about"}, apierr.CodeInvalidArgs},
		{"--draft with --published-only", []string{"get", "pages", "1", "--draft", "--published-only"}, apierr.CodeInvalidArgs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := urlRun(t, fs, home, nil, tc.args...)
			if res.code(t) != string(tc.code) || res.Code != tc.code.Exit() {
				t.Fatalf("code %s exit %d, want %s\n%s", res.code(t), res.Code, tc.code, res.Stdout)
			}
		})
	}
}

// --published-only on get is an assertion: the stored state or not_published.
func TestGetPublishedOnly(t *testing.T) {
	fs := newFakeSite(t)
	home := t.TempDir()
	seedDiscovery(t, home, fs.URL)

	res := urlRun(t, fs, home, nil, "get", "pages", "3", "--published-only")
	if res.Code != 0 || res.data(t)["slug"] != "changed" {
		t.Fatalf("a published doc with a newer draft must return its PUBLISHED state: exit %d\n%s", res.Code, res.Stdout)
	}
	for _, q := range fs.Requests() {
		if strings.HasPrefix(q.Path, "/api/pages/3") && strings.Contains(q.Query, "draft=true") {
			t.Fatalf("--published-only sent draft=true: %s", q.Query)
		}
	}

	res = urlRun(t, fs, home, nil, "get", "pages", "--slug", "draft-only", "--published-only")
	if res.code(t) != string(apierr.CodeNotPublished) || res.Code != apierr.ExitNotFound {
		t.Fatalf("never-published: code %s exit %d\n%s", res.code(t), res.Code, res.Stdout)
	}
	if hint := res.Env["error"].(map[string]any)["hint"].(string); !strings.Contains(hint, "pay get pages 2 --draft") {
		t.Errorf("hint = %q", hint)
	}

	// A --select without _status still asserts correctly.
	res = urlRun(t, fs, home, nil, "get", "pages", "1", "--published-only", "--select", "title")
	if res.Code != 0 {
		t.Fatalf("--select title --published-only: exit %d\n%s", res.Code, res.Stdout)
	}
	last := fs.Requests()[len(fs.Requests())-1]
	if !strings.Contains(last.Query, "_status") {
		t.Errorf("the select did not ask for _status: %s", last.Query)
	}
}
