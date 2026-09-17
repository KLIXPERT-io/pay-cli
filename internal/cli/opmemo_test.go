package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// §7.11's reactive operator memo.
//
// Before this, output.OperatorPreviouslyFailedWarning was called from nowhere
// and discovery.LearnedOperatorFailures was initialised empty at NewManifest
// and never appended to by anything in the repository — so the whole "learn
// reactively instead of assuming an adapter" decision (§1 conflict 36) existed
// only as two dead declarations and a paragraph of spec.

// serverErrorOn answers one route with the HTTP 500 Payload produces for an
// operator its adapter does not implement. Verified against the live instance:
// `pay raw GET crm-contacts --query 'where[tags][all]=a'` -> 500.
func serverErrorOn(path string) payloadtest.Option {
	return payloadtest.WithHandler(http.MethodGet, path, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"errors":[{"message":"Something went wrong."}]}`))
	})
}

// learnedFailures reads the memo back off disk, which is the only proof that it
// survives the process that learned it.
func learnedFailures(t *testing.T, home string, sc cache.Scope) []discovery.LearnedFailure {
	t.Helper()
	path := cache.New(filepath.Join(home, "cache")).ManifestPath(sc)
	data, err := os.ReadFile(path) //nolint:gosec // a path this test just wrote
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc struct {
		Learned []discovery.LearnedFailure `json:"learned_operator_failures"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("manifest is not JSON after the memo write: %v", err)
	}
	return doc.Learned
}

func TestOperatorFailureIsLearnedAndWarnedOnReuse(t *testing.T) {
	srv := payloadtest.NewServer(t, serverErrorOn("/api/pages"))
	home := t.TempDir()
	sc := seedDiscovery(t, home, srv.URL)

	args := []string{"raw", "GET", "pages", "--query", "where[tags][all]=a"}

	first := cliRun(t, invocation{Home: home, Args: args, Env: seededEnv(srv.URL)})
	if first.Code != 6 {
		t.Fatalf("first run exit = %d, want 6 (server_error)\n%s", first.Code, first.Stdout)
	}
	if first.hasWarning(t, output.WarnOperatorPreviouslyFailed) {
		t.Fatalf("the FIRST use warned; there was nothing learned yet:\n%s", first.Stdout)
	}

	learned := learnedFailures(t, home, sc)
	if len(learned) != 1 {
		t.Fatalf("learned_operator_failures = %v; §7.11 requires the failure to be recorded", learned)
	}
	got := learned[0]
	if got.Operator != "all" || got.Collection != "pages" {
		t.Errorf("recorded %q on %q, want all on pages", got.Operator, got.Collection)
	}
	if got.Evidence == "" {
		t.Error("the record carries no evidence, so the next warning cannot say why")
	}
	if got.LearnedAt.IsZero() {
		t.Error("the record carries no timestamp")
	}

	// The whole point: the NEXT use of that operator on that collection warns
	// before sending.
	second := cliRun(t, invocation{Home: home, Args: args, Env: seededEnv(srv.URL)})
	var warn map[string]any
	for _, w := range second.warnings(t) {
		if w["code"] == output.WarnOperatorPreviouslyFailed {
			warn = w
		}
	}
	if warn == nil {
		t.Fatalf("no %s warning on reuse; the memo is write-only.\nwarnings: %v",
			output.WarnOperatorPreviouslyFailed, second.warnings(t))
	}
	msg, _ := warn["message"].(string)
	for _, want := range []string{`"all"`, `"pages"`, got.Evidence} {
		if !strings.Contains(msg, want) {
			t.Errorf("warning message omits %q: %q", want, msg)
		}
	}
	if hint, _ := warn["hint"].(string); !strings.Contains(hint, "pay describe pages") {
		t.Errorf("warning hint does not name the command that lists the supported operators: %q", hint)
	}
	// A warning is not a block: §7.11 says PayCLI sends it anyway rather than
	// inventing a refusal.
	if second.Code != 6 {
		t.Errorf("second run exit = %d, want 6 — the memo must warn, never block", second.Code)
	}
}

// The memo must not fire on a collection it never failed on, or it becomes the
// same "assume, do not learn" mistake §1 conflict 36 exists to correct.
func TestOperatorMemoIsScopedToTheCollectionThatFailed(t *testing.T) {
	srv := payloadtest.NewServer(t, serverErrorOn("/api/pages"))
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)

	cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL),
		Args: []string{"raw", "GET", "pages", "--query", "where[tags][all]=a"}})

	other := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL),
		Args: []string{"raw", "GET", "media", "--query", "where[tags][all]=a"}})
	if other.hasWarning(t, output.WarnOperatorPreviouslyFailed) {
		t.Fatalf("a failure on pages warned about media:\n%s", other.Stdout)
	}
}

// §7.11 says "provably caused by operator O". These are the cases where nothing
// is proven, and recording them would teach PayCLI that `equals` is broken.
func TestOperatorFailureIsNotLearnedWithoutProof(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query []string
	}{
		{"two operators leave neither proven", []string{
			"where[tags][all]=a", "where[slug][equals]=home"}},
		{"a core operator is never blamed for a 500", []string{
			"where[slug][equals]=home"}},
		{"no operator at all", []string{"limit=1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := payloadtest.NewServer(t, serverErrorOn("/api/pages"))
			home := t.TempDir()
			sc := seedDiscovery(t, home, srv.URL)

			args := []string{"raw", "GET", "pages"}
			for _, q := range tc.query {
				args = append(args, "--query", q)
			}
			res := cliRun(t, invocation{Home: home, Args: args, Env: seededEnv(srv.URL)})
			if res.Code != 6 {
				t.Fatalf("exit = %d, want 6\n%s", res.Code, res.Stdout)
			}
			if learned := learnedFailures(t, home, sc); len(learned) != 0 {
				t.Fatalf("learned %v from an unattributable failure", learned)
			}
		})
	}
}

// A 4xx means the server understood the operator and rejected the VALUE. That
// is the caller's bug, not the adapter's, and learning from it would silence
// nothing and warn forever.
func TestOperatorFailureIsNotLearnedFromA4xx(t *testing.T) {
	srv := payloadtest.NewServer(t, payloadtest.WithHandler(
		http.MethodGet, "/api/pages", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":[{"message":"The following field is invalid: tags"}]}`))
		}))
	home := t.TempDir()
	sc := seedDiscovery(t, home, srv.URL)

	cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL),
		Args: []string{"raw", "GET", "pages", "--query", "where[tags][all]=a"}})
	if learned := learnedFailures(t, home, sc); len(learned) != 0 {
		t.Fatalf("a 400 was recorded as an operator failure: %v", learned)
	}
}

// --no-cache discovers in process and persists nothing (§8.5). The memo is a
// cache write and obeys the same rule rather than inventing an exception.
func TestOperatorMemoHonoursNoCache(t *testing.T) {
	srv := payloadtest.NewServer(t, serverErrorOn("/api/pages"))
	home := t.TempDir()
	sc := seedDiscovery(t, home, srv.URL)

	cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL),
		Args: []string{"raw", "GET", "pages", "--query", "where[tags][all]=a", "--no-cache"}})
	if learned := learnedFailures(t, home, sc); len(learned) != 0 {
		t.Fatalf("--no-cache still wrote to the scope cache: %v", learned)
	}
}

func TestWhereOperatorsInQuery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   string
		want []string
	}{
		{"none", "limit=1&depth=0", nil},
		{"simple", "where%5Btags%5D%5Ball%5D=a", []string{"all"}},
		{"nested or/and", "where%5Bor%5D%5B0%5D%5Band%5D%5B0%5D%5Bx%5D%5Bnear%5D=1%2C2%2C3", []string{"near"}},
		{"a field named like an operator is not one", "where%5Ball%5D=1", nil},
		{"unknown trailing segment", "where%5Bslug%5D%5Bnope%5D=x", nil},
		{"malformed", "%zz", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := whereOperatorsInQuery(tc.in)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("whereOperatorsInQuery(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestRawCollectionSlug(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		path     string
		absolute bool
		want     string
	}{
		{"relative collection", "/pages", false, "pages"},
		{"relative document", "/pages/11", false, "pages"},
		{"absolute with the api prefix", "/api/pages", true, "pages"},
		{"absolute outside the api mount", "/admin/collections/pages", true, ""},
		{"absolute at the root", "/pages", true, ""},
		{"empty", "/", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := rawCollectionSlug(tc.path, "/api", tc.absolute); got != tc.want {
				t.Fatalf("rawCollectionSlug(%q, /api, %v) = %q, want %q",
					tc.path, tc.absolute, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// §7.11 on the path an agent actually uses
// ---------------------------------------------------------------------------

// TestOperatorMemoCoversTheWhereFlag is the regression for the memo's real
// blind spot. Both halves were wired only into `pay raw`, which is the escape
// hatch — the operators §7.11 is about (`all`, `near`, `within`, `intersects`)
// reach the server through `--where` on find/count/update/delete/publish, and
// none of those learned or warned. The feature existed and did not apply to
// anything an agent would type.
func TestOperatorMemoCoversTheWhereFlag(t *testing.T) {
	srv := payloadtest.NewServer(t, serverErrorOn("/api/pages"))
	home := t.TempDir()
	sc := seedDiscovery(t, home, srv.URL)

	args := []string{"find", "pages", "--where", "tags all a", "--no-validate-where"}

	first := cliRun(t, invocation{Home: home, Args: args, Env: seededEnv(srv.URL)})
	if first.Code != 6 {
		t.Fatalf("first run exit = %d, want 6 (server_error)\n%s\n%s", first.Code, first.Stdout, first.Stderr)
	}
	if first.hasWarning(t, output.WarnOperatorPreviouslyFailed) {
		t.Fatalf("the FIRST use warned; nothing was learned yet:\n%s", first.Stdout)
	}

	learned := learnedFailures(t, home, sc)
	if len(learned) != 1 || learned[0].Operator != "all" || learned[0].Collection != "pages" {
		t.Fatalf("learned_operator_failures = %v; `find --where 'tags all a'` must teach the memo", learned)
	}

	// The next --where use of that operator warns BEFORE sending...
	second := cliRun(t, invocation{Home: home, Args: args, Env: seededEnv(srv.URL)})
	if !second.hasWarning(t, output.WarnOperatorPreviouslyFailed) {
		t.Fatalf("no %s on reuse via --where: %v", output.WarnOperatorPreviouslyFailed, second.warnings(t))
	}
	if second.Code != 6 {
		t.Errorf("second run exit = %d; the memo warns, it never blocks", second.Code)
	}

	// ...and so does every other command that shares the --where builder: the
	// memo is a property of the query, not of the verb that carried it.
	count := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL),
		Args: []string{"count", "pages", "--where", "tags all a", "--no-validate-where"}})
	if !count.hasWarning(t, output.WarnOperatorPreviouslyFailed) {
		t.Errorf("`count --where` did not consult the memo: %v", count.warnings(t))
	}
}

// The high attribution bar of §7.11 must hold on the --where path too: a 500
// carrying two operators proves nothing about either of them.
func TestWhereOperatorMemoKeepsTheAttributionBar(t *testing.T) {
	srv := payloadtest.NewServer(t, serverErrorOn("/api/pages"))
	home := t.TempDir()
	sc := seedDiscovery(t, home, srv.URL)

	res := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), Args: []string{
		"find", "pages", "--where", "tags all a", "--where", "slug equals home", "--no-validate-where"}})
	if res.Code != 6 {
		t.Fatalf("exit = %d, want 6\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}
	if learned := learnedFailures(t, home, sc); len(learned) != 0 {
		t.Fatalf("learned %v from a two-operator query", learned)
	}
}

// A query that never reaches the server must not be recorded: `--where` that
// fails client-side validation produced no evidence about any operator.
func TestWhereOperatorMemoIgnoresASuccessfulQuery(t *testing.T) {
	srv := payloadtest.NewServer(t, payloadtest.WithHandler(
		http.MethodGet, "/api/pages", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"docs":[],"totalDocs":0,"limit":20,"page":1,"totalPages":1,` +
				`"hasNextPage":false,"hasPrevPage":false}`))
		}))
	home := t.TempDir()
	sc := seedDiscovery(t, home, srv.URL)

	res := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL),
		Args: []string{"find", "pages", "--where", "tags all a", "--no-validate-where"}})
	if res.Code != 0 {
		t.Fatalf("exit = %d, want 0\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}
	if learned := learnedFailures(t, home, sc); len(learned) != 0 {
		t.Fatalf("a query that SUCCEEDED was recorded as a failure: %v", learned)
	}
}
