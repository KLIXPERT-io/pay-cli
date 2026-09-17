package cli

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

// TestCheckPublishable is §9.10.4's local refusal: when a required blocks
// field's slugs are not determinable from the API, no amount of retrying will
// make them knowable, so PayCLI says so instead of relaying a 400.
func TestCheckPublishable(t *testing.T) {
	ok := testTarget()
	if err := checkPublishable(ok); err != nil {
		t.Fatalf("a publishable collection must pass: %v", err)
	}

	blocked := testTarget()
	blocked.Coll.Publishable = false
	blocked.Coll.PublishableReason = strp("layout's blockType values are not determinable from the API")
	err := checkPublishable(blocked)
	if err == nil {
		t.Fatal("expected feature_unavailable")
	}
	if apierr.CodeOf(err) != apierr.CodeFeatureUnavailable {
		t.Fatalf("code = %s", apierr.CodeOf(err))
	}
	if apierr.ExitCode(err) != apierr.ExitCapability {
		t.Fatalf("exit = %d, want 10", apierr.ExitCode(err))
	}
	if !strings.Contains(err.Error(), "not determinable from the API") {
		t.Fatalf("message = %q — it must carry publishable_reason", err.Error())
	}
	e, _ := apierr.As(err)
	if !strings.Contains(e.Hint, "payload.config.ts") {
		t.Fatalf("hint = %q — it must say where to read the slugs from", e.Hint)
	}

	// An unknown manifest never refuses.
	if err := checkPublishable(&collTarget{Slug: "pages"}); err != nil {
		t.Fatalf("an unknown collection must not refuse: %v", err)
	}
}

// TestPublishRiskLevels: publishing one document is recoverable (L1), taking
// one offline is not announced anywhere else (L2), and the bulk forms are L3.
func TestPublishRiskLevels(t *testing.T) {
	tests := []struct {
		op   safety.Op
		want safety.Level
	}{
		{safety.Op{Command: safety.CmdPublish, Selector: safety.SelectorID}, safety.L1},
		{safety.Op{Command: safety.CmdUnpublish, Selector: safety.SelectorID}, safety.L2},
		{safety.Op{Command: safety.CmdPublish, Selector: safety.SelectorBulk}, safety.L3},
		{safety.Op{Command: safety.CmdUnpublish, Selector: safety.SelectorBulk}, safety.L3},
		{safety.Op{Command: safety.CmdGlobalsUpdate, Selector: safety.SelectorNone, Global: true}, safety.L2},
		{safety.Op{Command: safety.CmdVersionsRestore, Selector: safety.SelectorID}, safety.L2},
	}
	for _, tc := range tests {
		if got := tc.op.Level(); got != tc.want {
			t.Fatalf("%s/%v level = %v, want %v", tc.op.Command, tc.op.Selector, got, tc.want)
		}
	}
}

func TestPublishHelpStatesTheDraftDeferral(t *testing.T) {
	cmd := newPublishCmd(nil)
	for _, phrase := range []string{
		"re-runs EXACTLY the required-field validation that --draft skipped",
		"a draft is a deferral, not an escape",
		"publishable: false",
	} {
		if !strings.Contains(cmd.Long, phrase) {
			t.Fatalf("publish help is missing %q", phrase)
		}
	}
	un := newUnpublishCmd(nil)
	if un.Name() != "unpublish" {
		t.Fatalf("name = %q", un.Name())
	}
	if !strings.Contains(un.Long, "L2 operation") {
		t.Fatal("unpublish help must say it is an L2 operation")
	}
	for _, name := range []string{"where", "max-docs", "all", "limit"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("pay publish is missing --%s", name)
		}
	}
}

// TestPublishBulkCountsInTheWriteScope is the publish half of findings 7/8/11.
//
// runPublish counted and resolved on a BARE --where while the write itself
// carried --locale, so phase 1 sized the blast-radius cap against one
// population and phase 3 resolved another. It also never validated --max-docs,
// so `--max-docs 0` was silently ignored and the cap fell back to 100.
func TestPublishBulkCountsInTheWriteScope(t *testing.T) {
	var countQueries, resolveQueries []string
	srv := payloadtest.NewServer(t,
		payloadtest.WithHandler("GET", "/api/pages/count", func(w http.ResponseWriter, r *http.Request) {
			countQueries = append(countQueries, r.URL.RawQuery)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("X-Powered-By", "Payload")
			_, _ = io.WriteString(w, `{"totalDocs":2}`)
		}),
		payloadtest.WithHandler("GET", "/api/pages", func(w http.ResponseWriter, r *http.Request) {
			resolveQueries = append(resolveQueries, r.URL.RawQuery)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("X-Powered-By", "Payload")
			_, _ = io.WriteString(w, `{"docs":[{"id":1},{"id":2}],"totalDocs":2,"limit":200,"page":1,"totalPages":1}`)
		}))
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)

	// Phase 3 must resolve ids in the write's OWN scope. It used to be called
	// with a bare --where (client.ResolveIDs), so `publish --locale de`
	// resolved the default-locale population and published that instead.
	// (Phase 1 legitimately sends only where+trash: ARCHITECTURE:128 measured
	// that /count ignores everything else.)
	t.Run("phase 3 resolves in the write's locale", func(t *testing.T) {
		countQueries, resolveQueries = nil, nil
		res := cliRun(t, invocation{
			Home: home,
			Args: []string{"publish", "pages", "--where", "_status eq draft",
				"--locale", "de", "--dry-run"},
			Env: seededEnv(srv.URL),
		})
		if res.Code != 0 {
			t.Fatalf("exit = %d: %s\n%s", res.Code, res.Stdout, res.Stderr)
		}
		if len(countQueries) == 0 {
			t.Fatal("phase 1 never ran")
		}
		if len(resolveQueries) == 0 {
			t.Fatal("phase 3 never ran")
		}
		if !strings.Contains(resolveQueries[0], "locale=de") {
			t.Errorf("resolve query = %q, want locale=de: the ids published are "+
				"resolved from a different population than the write targets",
				resolveQueries[0])
		}
	})

	t.Run("--max-docs 0 is an error, not a silent 100", func(t *testing.T) {
		res := cliRun(t, invocation{
			Home: home,
			Args: []string{"publish", "pages", "--where", "_status eq draft",
				"--max-docs", "0", "--dry-run"},
			Env: seededEnv(srv.URL),
		})
		if res.Code == 0 {
			t.Fatalf("`--max-docs 0` was accepted (exit 0); the cap the caller "+
				"typed was discarded.\n%s", res.Stdout)
		}
		if !strings.Contains(res.Stdout, "max-docs") {
			t.Errorf("the refusal does not name --max-docs: %s", res.Stdout)
		}
	})
}
