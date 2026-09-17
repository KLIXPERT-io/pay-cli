package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
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
		"title":     "old",
		"slug":      "same",
		"removed":   "gone",
		"updatedAt": "2026-01-01",
		"layout":    []any{map[string]any{"blockType": "cta"}},
	}
	b := map[string]any{
		"title":     "new",
		"slug":      "same",
		"added":     "here",
		"updatedAt": "2026-02-02",
		"layout":    []any{map[string]any{"blockType": "content"}},
	}
	got := diffVersions(a, b)

	byPath := map[string]versionChange{}
	for _, c := range got {
		byPath[c.Path] = c
	}
	if c, ok := byPath["title"]; !ok || c.Kind != "changed" || c.A != "old" || c.B != "new" {
		t.Fatalf("title = %+v", c)
	}
	if c, ok := byPath["removed"]; !ok || c.Kind != "removed" {
		t.Fatalf("removed = %+v", c)
	}
	if c, ok := byPath["added"]; !ok || c.Kind != "added" {
		t.Fatalf("added = %+v", c)
	}
	if _, ok := byPath["slug"]; ok {
		t.Fatal("an unchanged field must not be reported")
	}
	if _, ok := byPath["updatedAt"]; ok {
		t.Fatal("volatile bookkeeping must be excluded")
	}
	if c, ok := byPath["layout[0].blockType"]; !ok || c.Kind != "changed" {
		t.Fatalf("nested change = %+v", c)
	}
	// The result is deterministic: paths come back sorted.
	for i := 1; i < len(got); i++ {
		if got[i-1].Path > got[i].Path {
			t.Fatalf("changes are not sorted: %v", got)
		}
	}
	if len(diffVersions(a, a)) != 0 {
		t.Fatal("identical versions must diff to nothing")
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
