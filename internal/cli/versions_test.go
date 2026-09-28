package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/docdiff"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

func TestSplitVersionArgs(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		global    string
		wantScope []string
		wantID    string
		wantErr   bool
	}{
		{name: "collection and version", args: []string{"pages", "412"}, wantScope: []string{"pages"}, wantID: "412"},
		{name: "global and version", args: []string{"88"}, global: "header", wantID: "88"},
		{name: "both is an error", args: []string{"pages", "412"}, global: "header", wantErr: true},
		{name: "version without a scope", args: []string{"412"}, wantErr: true},
		{name: "no args", args: nil, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scope, id, err := splitVersionArgs(tc.args, tc.global)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected invalid_args")
				}
				if apierr.CodeOf(err) != apierr.CodeInvalidArgs {
					t.Fatalf("code = %s", apierr.CodeOf(err))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if id != tc.wantID || strings.Join(scope, ",") != strings.Join(tc.wantScope, ",") {
				t.Fatalf("scope=%v id=%q", scope, id)
			}
		})
	}
}

// TestVersionScopeChecksVersions pre-empts §11.3's opaque 500: /versions on a
// collection without versions.
func TestVersionScopeChecksVersions(t *testing.T) {
	d := testDeps()
	for _, c := range d.Manifest.Collections {
		if c.Slug == "media" {
			c.Flags.Versions = boolp(false)
		}
	}
	if _, err := resolveVersionScope(context.Background(), d, []string{"media"}, ""); err == nil {
		t.Fatal("expected feature_unavailable")
	} else if apierr.CodeOf(err) != apierr.CodeFeatureUnavailable {
		t.Fatalf("code = %s", apierr.CodeOf(err))
	}

	// A collection whose versions support was never learned must be attempted.
	for _, c := range d.Manifest.Collections {
		if c.Slug == "media" {
			c.Flags.Versions = nil
		}
	}
	if _, err := resolveVersionScope(context.Background(), d, []string{"media"}, ""); err != nil {
		t.Fatalf("unknown versions support must not reject: %v", err)
	}

	if _, err := resolveVersionScope(context.Background(), d, []string{"pages"}, "header"); err == nil {
		t.Fatal("collection + --global must be rejected")
	}
	if _, err := resolveVersionScope(context.Background(), d, nil, ""); err == nil {
		t.Fatal("no target must be rejected")
	}
}

func TestVersionBody(t *testing.T) {
	wrapped := payload.Doc{"id": 412, "version": map[string]any{"title": "t"}}
	if got := versionBody(wrapped); got["title"] != "t" {
		t.Fatalf("versionBody = %v", got)
	}
	flat := payload.Doc{"title": "t"}
	if got := versionBody(flat); got["title"] != "t" {
		t.Fatalf("versionBody = %v", got)
	}
	if got := versionBody(nil); len(got) != 0 {
		t.Fatalf("versionBody(nil) = %v", got)
	}
}

func TestDiffVersions(t *testing.T) {
	a := map[string]any{
		"id":        "v1",
		"title":     "old",
		"slug":      "same",
		"removed":   "gone",
		"updatedAt": "2026-01-01",
		"layout": []any{
			map[string]any{"id": "r1", "blockType": "cta", "updatedAt": "x"},
			map[string]any{"id": "r2", "blockType": "text", "heading": "A"},
		},
	}
	b := map[string]any{
		"id":        "v2",
		"title":     "new",
		"slug":      "same",
		"added":     "here",
		"updatedAt": "2026-02-02",
		"layout": []any{
			map[string]any{"id": "r2", "blockType": "text", "heading": "B"},
			map[string]any{"id": "r1", "blockType": "cta", "updatedAt": "y"},
		},
	}
	res := docdiff.Diff(a, b, versionDiffOptions(nil, false, false))
	got := versionChanges(res, false)

	byPath := map[string]versionChange{}
	for _, c := range got {
		byPath[c.Path+"/"+string(c.Op)] = c
	}
	// The v0.1.0 keys keep their meaning.
	if c, ok := byPath["title/change"]; !ok || c.Kind != "changed" || c.A != "old" || c.B != "new" {
		t.Fatalf("title = %+v", c)
	}
	if c, ok := byPath["removed/remove"]; !ok || c.Kind != "removed" || c.A != "gone" || c.B != nil {
		t.Fatalf("removed = %+v", c)
	}
	if c, ok := byPath["added/add"]; !ok || c.Kind != "added" || c.B != "here" {
		t.Fatalf("added = %+v", c)
	}
	for _, p := range []string{"slug/change", "updatedAt/change", "id/change"} {
		if _, ok := byPath[p]; ok {
			t.Fatalf("%s must not be reported: %+v", p, got)
		}
	}
	// Rows are matched by id: one move, and the edit inside the moved row is
	// reported at its new path — not every row "changed".
	if c, ok := byPath["layout[0].heading/change"]; !ok || c.A != "A" || c.B != "B" {
		t.Fatalf("nested change = %+v (all: %+v)", c, got)
	}
	moves := 0
	for _, c := range got {
		if c.Op == docdiff.OpMove {
			moves++
			if c.Kind != "moved" || c.ValueKind != docdiff.KindRow || c.FromIndex == nil || c.ToIndex == nil {
				t.Errorf("move = %+v", c)
			}
		}
	}
	if moves != 1 {
		t.Errorf("moves = %d, want 1: %+v", moves, got)
	}
	if res.Identical || res.Summary.Total != len(got) {
		t.Errorf("summary = %+v", res.Summary)
	}
	if again := docdiff.Diff(a, a, versionDiffOptions(nil, false, false)); !again.Identical || len(again.Changes) != 0 {
		t.Fatal("identical versions must diff to nothing")
	}

	// --ignore drops a subtree; --include-timestamps brings updatedAt back.
	ign := docdiff.Diff(a, b, versionDiffOptions([]string{"layout"}, false, false))
	for _, c := range ign.Changes {
		if strings.HasPrefix(c.Path, "layout") {
			t.Errorf("--ignore layout left %s", c.Path)
		}
	}
	ts := docdiff.Diff(a, b, versionDiffOptions(nil, true, false))
	found := false
	for _, c := range ts.Changes {
		found = found || c.Path == "updatedAt"
	}
	if !found {
		t.Error("--include-timestamps must compare updatedAt")
	}
}

// A block whose type changed is one row change, and rich text is reported
// once per field as text — in a/b too, so a v0.1.0 caller sees words.
func TestDiffVersionsRowTypeAndRichText(t *testing.T) {
	rt := func(text string) map[string]any {
		return map[string]any{"root": map[string]any{"type": "root", "children": []any{
			map[string]any{"type": "paragraph", "children": []any{map[string]any{"type": "text", "text": text}}},
		}}}
	}
	a := map[string]any{"body": rt("Alter Text"), "layout": []any{map[string]any{"id": "r", "blockType": "bento"}}}
	b := map[string]any{"body": rt("Neuer Text"), "layout": []any{map[string]any{"id": "r", "blockType": "grid"}}}
	got := versionChanges(docdiff.Diff(a, b, versionDiffOptions(nil, false, false)), false)
	if len(got) != 2 {
		t.Fatalf("changes = %+v", got)
	}
	byPath := map[string]versionChange{}
	for _, c := range got {
		byPath[c.Path] = c
	}
	body := byPath["body"]
	if body.ValueKind != docdiff.KindRichText || body.A != "Alter Text" || body.B != "Neuer Text" ||
		body.AText == nil || *body.AText != "Alter Text" {
		t.Errorf("body = %+v", body)
	}
	row := byPath["layout[0]"]
	if row.ValueKind != docdiff.KindRow || row.Kind != "changed" {
		t.Errorf("row = %+v", row)
	}

	full := versionChanges(docdiff.Diff(a, b, versionDiffOptions(nil, false, true)), true)
	for _, c := range full {
		if c.Path == "body" {
			if _, ok := c.A.(map[string]any); !ok {
				t.Errorf("--full must keep the editor state in a: %+v", c.A)
			}
		}
	}
}

func TestVersionsCommandTree(t *testing.T) {
	cmd := newVersionsCmd(nil)
	names := map[string]bool{}
	for _, sub := range cmd.Commands() {
		names[sub.Name()] = true
	}
	for _, want := range []string{"list", "get", "restore", "diff"} {
		if !names[want] {
			t.Fatalf("pay versions is missing %q (have %v)", want, names)
		}
	}
}

// The whole command: two GETs, the v0.1.0 keys, the docdiff summary, and the
// table rendering shared with `pay diff`.
func TestVersionsDiffCommand(t *testing.T) {
	home := t.TempDir()
	srv := payloadtest.NewServer(t,
		payloadtest.WithHandler("GET", "/api/pages/versions/20", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, 200, map[string]any{"id": 20, "parent": 16, "updatedAt": "a", "version": map[string]any{
				"title": "Alt", "layout": []any{map[string]any{"id": "r1", "blockType": "cta"}, map[string]any{"id": "r2", "blockType": "text"}}}})
		}),
		payloadtest.WithHandler("GET", "/api/pages/versions/21", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, 200, map[string]any{"id": 21, "parent": 16, "updatedAt": "b", "version": map[string]any{
				"title": "Neu", "layout": []any{map[string]any{"id": "r2", "blockType": "text"}, map[string]any{"id": "r1", "blockType": "cta"}}}})
		}),
	)
	seedDiscovery(t, home, srv.URL)
	env := []string{"PAY_BASE_URL=" + srv.URL, "PAY_AUTH_MODE=anonymous"}
	res := cliRun(t, invocation{Home: home, Env: env, Args: []string{"versions", "diff", "pages", "20", "21"}})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
	}
	d := res.data(t)
	if d["a"] != "20" || d["b"] != "21" || d["identical"] != false || d["changed"] != json.Number("2") {
		t.Fatalf("data = %v", d)
	}
	sum, _ := d["summary"].(map[string]any)
	if sum["moved"] != json.Number("1") || sum["changed"] != json.Number("1") {
		t.Errorf("summary = %v", sum)
	}
	changes, _ := d["changes"].([]any)
	var title map[string]any
	for _, c := range changes {
		if m := c.(map[string]any); m["path"] == "title" {
			title = m
		}
	}
	if title == nil || title["kind"] != "changed" || title["a"] != "Alt" || title["b"] != "Neu" || title["op"] != "change" {
		t.Errorf("title change = %v", title)
	}

	tab := cliRun(t, invocation{Home: home, Env: env, Args: []string{"versions", "diff", "pages", "20", "21", "--output", "table"}})
	if tab.Code != 0 || !strings.Contains(tab.Stdout, "title") || !strings.Contains(tab.Stdout, "move") {
		t.Errorf("table = %d\n%s", tab.Code, tab.Stdout)
	}
}
