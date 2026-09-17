package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// complete runs cobra's hidden __complete command the way a shell does.
func complete(t *testing.T, home string, env []string, args ...string) cliResult {
	t.Helper()
	return cliRun(t, invocation{
		Home: home,
		Args: append([]string{"__complete"}, args...),
		Env:  env,
	})
}

func suggestions(out string) []string {
	var got []string
	for _, line := range strings.Split(out, "\n") {
		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "Completion ended") {
			continue
		}
		name, _, _ := strings.Cut(line, "\t")
		got = append(got, name)
	}
	return got
}

func TestCompletionNeverPrintsAnEnvelope(t *testing.T) {
	res := complete(t, "", nil, "describe", "")
	if strings.Contains(res.Stdout, `"data_kind"`) {
		t.Fatalf("__complete emitted an envelope:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, ":") {
		t.Fatalf("__complete did not emit a shell directive:\n%s", res.Stdout)
	}
}

// TestCompletionNeverTouchesTheNetwork is §9.9's hard rule. TestMain installs a
// RoundTripper that panics, and there is no cache here, so a completion that
// triggered discovery would take the process down.
func TestCompletionOnAColdCacheOffersNothing(t *testing.T) {
	cases := [][]string{
		{"describe", ""},
		{"collections", "--kind", ""},
		{"access", ""},
		{"can", "read", ""},
		{"explain", "--collection", ""},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			res := complete(t, "", []string{"PAY_BASE_URL=" + testBaseURL}, args...)
			if res.Code != 0 {
				t.Fatalf("exit = %d\n%s", res.Code, res.Stderr)
			}
			for _, s := range suggestions(res.Stdout) {
				// A closed enum (--kind) is answerable offline; a slug is not.
				if strings.Contains(s, "pages") || strings.Contains(s, "crm-") {
					t.Errorf("cold cache produced a discovered suggestion: %q", s)
				}
			}
		})
	}
}

func TestCompletionOnAWarmCacheOffersSlugs(t *testing.T) {
	home := t.TempDir()
	seedDiscovery(t, home, testBaseURL)
	res := complete(t, home, seededEnv(testBaseURL), "describe", "pag")
	got := suggestions(res.Stdout)
	found := false
	for _, s := range got {
		if s == "pages" {
			found = true
		}
		if !strings.HasPrefix(s, "pag") {
			t.Errorf("suggestion %q does not match the prefix", s)
		}
	}
	if !found {
		t.Errorf("warm cache did not offer \"pages\": %v", got)
	}
}

func TestCompletionOffersFieldPathsForTheCollectionOnTheLine(t *testing.T) {
	home := t.TempDir()
	seedDiscovery(t, home, testBaseURL)
	res := complete(t, home, seededEnv(testBaseURL), "describe", "pages", "--field", "")
	got := suggestions(res.Stdout)
	if len(got) == 0 {
		t.Fatalf("no field paths offered:\n%s", res.Stdout)
	}
	hasID := false
	for _, s := range got {
		if s == "id" {
			hasID = true
		}
	}
	if !hasID {
		t.Errorf("field completion did not offer \"id\": %v", got)
	}
}

func TestCompletionEnumsAreAlwaysAnswerable(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"collections", "--kind", ""}, "content"},
		{[]string{"explain", "--section", ""}, "query_syntax"},
		{[]string{"version", "--output", ""}, "jsonl"},
		{[]string{"can", ""}, "create"},
		{[]string{"completion", ""}, "zsh"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			res := complete(t, "", nil, tc.args...)
			got := suggestions(res.Stdout)
			for _, s := range got {
				if s == tc.want {
					return
				}
			}
			t.Errorf("completion offered %v, want it to contain %q", got, tc.want)
		})
	}
}

func TestCompletionRespectsTheBudget(t *testing.T) {
	home := t.TempDir()
	seedDiscovery(t, home, testBaseURL)
	// A clock that jumps a full second per read makes every completion look
	// over budget, which must degrade to zero suggestions rather than block.
	clock := payloadtest.NewClock(time.Second)
	res := cliRun(t, invocation{
		Home:  home,
		Args:  []string{"__complete", "describe", ""},
		Env:   seededEnv(testBaseURL),
		Clock: clock.Now,
	})
	for _, s := range suggestions(res.Stdout) {
		if s == "pages" {
			t.Errorf("an over-budget completion still produced suggestions: %v", suggestions(res.Stdout))
		}
	}
}

func TestCompletionOfProfiles(t *testing.T) {
	home := t.TempDir()
	setup := cliRun(t, invocation{
		Home: home,
		Args: []string{"config", "set", "profiles.staging.base_url", "https://staging.example.com"},
	})
	if setup.Code != 0 {
		t.Fatalf("setup failed: %s", setup.Stdout)
	}
	res := complete(t, home, nil, "--profile", "")
	got := suggestions(res.Stdout)
	for _, s := range got {
		if s == "staging" {
			return
		}
	}
	t.Errorf("profile completion offered %v, want it to contain \"staging\"", got)
}
