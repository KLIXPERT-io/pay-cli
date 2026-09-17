package cli

import (
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// ---------------------------------------------------------------------------
// versions list --sort (finding 4)
//
// Payload answers an unknown sort field with HTTP 200 and no sorting at all —
// the gotcha `pay explain` documents and `pay find` rejects locally. The
// versions route needs the same guard, against a VERSION-RECORD schema: the row
// is {id, parent, createdAt, updatedAt, autosave, latest, publishedLocale,
// version:{…}}, so `--sort title` is silently ignored while
// `--sort version.title` really sorts.
// ---------------------------------------------------------------------------

func versionsScope(shard *discovery.Shard) versionScope {
	t := testTarget()
	t.Shard = shard
	return versionScope{target: payload.CollectionTarget("pages"), coll: t}
}

func TestValidateVersionSort(t *testing.T) {
	tests := []struct {
		name     string
		sort     []string
		wantErr  bool
		wantMean string
	}{
		{name: "version record column", sort: []string{"-updatedAt"}},
		{name: "parent", sort: []string{"parent"}},
		{name: "id", sort: []string{"id"}},
		{name: "autosave", sort: []string{"autosave"}},
		{name: "prefixed document field", sort: []string{"-version.publishedAt"}},
		{name: "prefixed nested document path", sort: []string{"version.tags.name"}},
		{name: "empty is not a field", sort: []string{""}},
		{name: "unknown record field", sort: []string{"-bogusfield"}, wantErr: true},
		{
			// The discoverability half: `title` IS in .sortable_paths, which is
			// exactly what the documented gotcha tells an agent to consult.
			name: "document field without the prefix", sort: []string{"title"},
			wantErr: true, wantMean: "version.title",
		},
		{name: "unknown prefixed field", sort: []string{"-version.bogusfield"}, wantErr: true},
		{name: "bare prefix", sort: []string{"version."}, wantErr: true},
		{name: "one bad field among good ones", sort: []string{"-updatedAt", "bogus"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateVersionSort(versionsScope(pagesShard()), tc.sort)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("unexpected rejection: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected invalid_sort_field")
			}
			if got := apierr.CodeOf(err); got != apierr.CodeInvalidSortField {
				t.Fatalf("code = %s, want invalid_sort_field", got)
			}
			if got := apierr.ExitCode(err); got != apierr.ExitValidation {
				t.Errorf("exit = %d, want %d", got, apierr.ExitValidation)
			}
			e, _ := apierr.As(err)
			if !strings.Contains(e.Hint, "version.") {
				t.Errorf("the hint never mentions the version. prefix: %q", e.Hint)
			}
			if tc.wantMean != "" {
				found := false
				for _, m := range e.DidYouMean {
					if m == tc.wantMean {
						found = true
					}
				}
				if !found {
					t.Errorf("did_you_mean = %v, want %q in it", e.DidYouMean, tc.wantMean)
				}
			}
		})
	}
}

// TestValidateVersionSortIsTriState is §9.3's rule: with no field shard nothing
// was learned, so nothing may be rejected.
func TestValidateVersionSortIsTriState(t *testing.T) {
	if err := validateVersionSort(versionsScope(nil), []string{"-bogusfield", "title"}); err != nil {
		t.Fatalf("an unknown schema must not reject: %v", err)
	}
	empty := discovery.NewShard("gen", "pages")
	empty.Finalize()
	if err := validateVersionSort(versionsScope(empty), []string{"-bogusfield"}); err != nil {
		t.Fatalf("an empty schema must not reject: %v", err)
	}
}

func TestVersionsListRejectsAnUnknownSortField(t *testing.T) {
	srv := payloadtest.NewServer(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)

	res := cliRun(t, invocation{
		Home: home,
		Args: []string{"versions", "list", "pages", "--sort", "-bogusfield", "--limit", "4"},
		Env:  seededEnv(srv.URL),
	})
	if res.Code != apierr.ExitValidation {
		t.Fatalf("exit = %d, want %d\n%s", res.Code, apierr.ExitValidation, res.Stdout)
	}
	if got := res.code(t); got != string(apierr.CodeInvalidSortField) {
		t.Errorf("code = %q, want invalid_sort_field\n%s", got, res.Stdout)
	}
	// The request must never leave: the whole point is that the server would
	// have answered it with HTTP 200 and unsorted rows.
	if n := srv.Count("GET", "/api/pages/versions"); n != 0 {
		t.Errorf("the unsorted request was sent anyway (%d times)", n)
	}
}

func TestVersionsListAcceptsAPrefixedSortField(t *testing.T) {
	srv := payloadtest.NewServer(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)

	res := cliRun(t, invocation{
		Home: home,
		Args: []string{"versions", "list", "pages", "--sort", "-version.publishedAt", "--limit", "2"},
		Env:  seededEnv(srv.URL),
	})
	if res.Code != 0 {
		t.Fatalf("exit = %d, want 0\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}
	if n := srv.Count("GET", "/api/pages/versions"); n != 1 {
		t.Errorf("versions route hit %d times, want 1", n)
	}
}

// TestVersionsListSortEscapeHatch mirrors find's --no-validate-sort: the
// rejection is derived, so there has to be a way past it.
func TestVersionsListSortEscapeHatch(t *testing.T) {
	srv := payloadtest.NewServer(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)

	res := cliRun(t, invocation{
		Home: home,
		Args: []string{"versions", "list", "pages", "--sort", "-bogusfield",
			"--no-validate-sort", "--limit", "2"},
		Env: seededEnv(srv.URL),
	})
	if res.Code != 0 {
		t.Fatalf("exit = %d, want 0\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}
	if n := srv.Count("GET", "/api/pages/versions"); n != 1 {
		t.Errorf("versions route hit %d times, want 1", n)
	}
}
