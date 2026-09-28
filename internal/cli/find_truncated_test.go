package cli

import (
	"testing"
)

// envTruncated reads page.truncated from a doc_list envelope.
func envTruncated(t *testing.T, res cliResult) bool {
	t.Helper()
	page, ok := res.Env["page"].(map[string]any)
	if !ok {
		t.Fatalf("envelope has no page block:\n%s", res.Stdout)
	}
	v, ok := page["truncated"].(bool)
	if !ok {
		t.Fatalf("page.truncated is not a boolean: %#v", page["truncated"])
	}
	return v
}

// TestPaginatedFindReportsTruncated pins §10.1's promise that page.truncated
// alone answers "did I get everything?". A plain --limit read with more pages
// used to report truncated:false, so an agent following the skill ("run
// next.cmd until page.truncated is false") stopped after page 1 and treated
// a partial result as the full set.
func TestPaginatedFindReportsTruncated(t *testing.T) {
	srv := reinvokeServer(t)
	home := t.TempDir()
	seedContactsDiscovery(t, home, srv.URL)
	run := func(args []string) cliResult {
		t.Helper()
		res := cliRun(t, invocation{Home: home, Args: args, Env: seededEnv(srv.URL)})
		if res.Code != 0 {
			t.Fatalf("%v exited %d\n%s\n%s", args, res.Code, res.Stdout, res.Stderr)
		}
		return res
	}

	t.Run("a page with more pages after it is truncated", func(t *testing.T) {
		res := run([]string{"find", "crm-contacts", "--limit", "50"})
		if !envTruncated(t, res) {
			t.Fatalf("--limit 50 of %d reported truncated:false:\n%s", reinvokeTotal, res.Stdout)
		}
	})

	t.Run("following next.cmd until truncated is false collects everything", func(t *testing.T) {
		res := run([]string{"find", "crm-contacts", "--limit", "50"})
		seen := map[int]bool{}
		for i := 0; ; i++ {
			if i > 10 {
				t.Fatal("next.cmd loop did not terminate")
			}
			for _, id := range envIDs(t, res) {
				seen[id] = true
			}
			if !envTruncated(t, res) {
				if _, has := res.Env["next"]; has {
					t.Fatalf("truncated:false but next is still present:\n%s", res.Stdout)
				}
				break
			}
			res = run(reinvokeArgs(t, nextCmdOf(t, res, false)))
		}
		if len(seen) != reinvokeTotal {
			t.Fatalf("collected %d documents, want %d", len(seen), reinvokeTotal)
		}
	})

	t.Run("a read that holds every match is not truncated", func(t *testing.T) {
		if envTruncated(t, run([]string{"find", "crm-contacts", "--limit", "200"})) {
			t.Fatal("a single complete page reported truncated:true")
		}
		if envTruncated(t, run([]string{"find", "crm-contacts", "--all"})) {
			t.Fatal("a complete --all walk reported truncated:true")
		}
	})

	t.Run("--all stopped by --max stays truncated", func(t *testing.T) {
		if !envTruncated(t, run([]string{"find", "crm-contacts", "--all", "--max", "5"})) {
			t.Fatal("--all --max 5 of 104 reported truncated:false")
		}
	})
}
