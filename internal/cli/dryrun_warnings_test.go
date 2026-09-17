package cli

import (
	"strings"
	"testing"
)

// §12.2 + §9.7 — a --dry-run preview must carry every warning the real write
// would have printed.
//
// Regression test for a preview that was QUIETER than the write it previews:
// every write verb built its body warnings and then returned an envelope that
// never received them, because only the post-response path looped over
// `warnings`. Verified live against the dummy project before the fix:
//
//	pay create pages --set title=zzz \
//	  --set-json 'layout=[{"blockType":"nonsense"}]' --dry-run
//	  -> "warnings": []
//
// while the same command without --dry-run warns. That is the worst possible
// place to lose §9.7's warning: --dry-run is what an agent is told to run
// BEFORE a write, and Payload answers 201 and silently discards the unknown
// block, so the preview was the last chance to catch it.

func TestDryRunCreateWarnsAboutAnUnknownBlockType(t *testing.T) {
	home := t.TempDir()
	seedTwoBlockFields(t, home, testBaseURL)

	res := cliRun(t, invocation{
		Home: home,
		Args: []string{
			"create", "pages",
			"--set", "title=preview",
			"--set-json", `layout=[{"blockType":"nonsense"}]`,
			"--dry-run",
		},
		Env: seededEnv(testBaseURL),
	})

	if res.Env == nil {
		t.Fatalf("no envelope\nstdout:\n%s\nstderr:\n%s", res.Stdout, res.Stderr)
	}
	if !res.hasWarning(t, warnUnknownBlockType) {
		t.Fatalf("--dry-run dropped %s; warnings = %v", warnUnknownBlockType, res.warnings(t))
	}
	var msg string
	for _, w := range res.warnings(t) {
		if w["code"] == warnUnknownBlockType {
			msg, _ = w["message"].(string)
		}
	}
	if !strings.Contains(msg, `"nonsense"`) {
		t.Errorf("warning must name the offending blockType, got %q", msg)
	}
}

// A blockType the field really accepts must stay silent, so the fix above does
// not turn every preview into noise.
func TestDryRunCreateIsSilentForAKnownBlockType(t *testing.T) {
	home := t.TempDir()
	seedTwoBlockFields(t, home, testBaseURL)

	res := cliRun(t, invocation{
		Home: home,
		Args: []string{
			"create", "pages",
			"--set", "title=preview",
			"--set-json", `layout=[{"blockType":"cta"}]`,
			"--dry-run",
		},
		Env: seededEnv(testBaseURL),
	})
	if res.hasWarning(t, warnUnknownBlockType) {
		t.Errorf("cta is in Page_Layout; warnings = %v", res.warnings(t))
	}
}

// The same hole existed in `pay update`, which has two dry-run paths.
func TestDryRunUpdateWarnsAboutAnUnknownBlockType(t *testing.T) {
	home := t.TempDir()
	seedTwoBlockFields(t, home, testBaseURL)

	res := cliRun(t, invocation{
		Home: home,
		Args: []string{
			"update", "pages", "1",
			"--set-json", `layout=[{"blockType":"nonsense"}]`,
			"--dry-run", "--yes",
		},
		Env: seededEnv(testBaseURL),
	})
	if res.Env == nil {
		t.Fatalf("no envelope\nstdout:\n%s\nstderr:\n%s", res.Stdout, res.Stderr)
	}
	if !res.hasWarning(t, warnUnknownBlockType) {
		t.Fatalf("--dry-run dropped %s; warnings = %v", warnUnknownBlockType, res.warnings(t))
	}
}

// A preview must not say the same thing twice: emitDryRun dedupes, so the bulk
// update path that already attached its resolve-gap warning by hand still
// reports it exactly once.
func TestDryRunWarningsAreNotDuplicated(t *testing.T) {
	home := t.TempDir()
	seedTwoBlockFields(t, home, testBaseURL)

	res := cliRun(t, invocation{
		Home: home,
		Args: []string{
			"create", "pages",
			"--set", "title=preview",
			"--set-json", `layout=[{"blockType":"nonsense"},{"blockType":"nonsense"}]`,
			"--dry-run",
		},
		Env: seededEnv(testBaseURL),
	})
	n := 0
	for _, w := range res.warnings(t) {
		if w["code"] == warnUnknownBlockType {
			n++
		}
	}
	if n != 1 {
		t.Errorf("unknown_block_type appeared %d times, want exactly 1: %v", n, res.warnings(t))
	}
}
