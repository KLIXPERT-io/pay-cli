package cli

import (
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// doctorChecks indexes a report by check name.
func doctorChecks(t *testing.T, res cliResult) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	raw, _ := res.data(t)["checks"].([]any)
	for _, c := range raw {
		m := c.(map[string]any)
		out[m["name"].(string)] = m
	}
	return out
}

func TestDoctorWithNoBaseURLStillReports(t *testing.T) {
	res := cliRun(t, invocation{Args: []string{"doctor"}})
	if res.Code != 0 {
		t.Fatalf("doctor must report rather than fail: exit = %d\n%s", res.Code, res.Stdout)
	}
	checks := doctorChecks(t, res)
	base, ok := checks["base_url"]
	if !ok {
		t.Fatalf("no base_url check: %v", res.data(t))
	}
	if base["status"] != CheckFail {
		t.Errorf("base_url status = %v, want fail", base["status"])
	}
	if !strings.Contains(base["hint"].(string), "pay auth login") {
		t.Errorf("hint is not actionable: %v", base["hint"])
	}
	if res.data(t)["ok"] != false {
		t.Errorf("data.ok = %v, want false", res.data(t)["ok"])
	}
}

func TestDoctorReportsEveryMandatedFact(t *testing.T) {
	srv := payloadtest.NewServer(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	res := cliRun(t, invocation{
		Home: home, Args: []string{"doctor"},
		Env: []string{"PAY_BASE_URL=" + srv.URL, "PAY_AUTH_MODE=anonymous"},
	})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}
	checks := doctorChecks(t, res)
	// §18's mandated report lines.
	for _, name := range []string{
		"base_url", "credential", "reachability", "powered_by", "discovery",
		"graphql", "graphql_path", "payload_version", "db_adapter",
		"auth_collection", "api_key_support", "identity",
		"cache", "cache_writes", "cache_fingerprint", "audit_log", "skill", "update",
	} {
		if _, ok := checks[name]; !ok {
			t.Errorf("doctor has no %q check", name)
		}
	}
	for name, c := range checks {
		switch c["status"] {
		case CheckOK, CheckWarn, CheckFail, CheckUnknown, CheckSkipped:
		default:
			t.Errorf("check %q has an unknown status %v", name, c["status"])
		}
		if c["detail"] == "" {
			t.Errorf("check %q has no detail", name)
		}
	}

	d := res.data(t)
	conn, _ := d["connection"].(map[string]any)
	for _, key := range []string{
		"api_path", "api_path_source", "graphql_path", "graphql_path_source",
		"topology_sha256", "schema_sha256", "payload_version", "payload_version_source",
		"db_adapter", "db_adapter_source", "auth_collections", "api_key_collections",
		"access_latency_ms",
	} {
		if _, ok := conn[key]; !ok {
			t.Errorf("connection.%s missing", key)
		}
	}
	cache, _ := d["cache"].(map[string]any)
	if _, ok := cache["scopes"]; !ok {
		t.Errorf("cache.scopes missing")
	}
	if _, ok := d["audit"].(map[string]any)["path"]; !ok {
		t.Errorf("audit.path missing")
	}
}

// §5.4: when no auth collection has useAPIKey, every hint must print the JWT
// command instead of the API-key one, because the latter can never succeed.
func TestDoctorNamesTheAPIKeyCollections(t *testing.T) {
	srv := payloadtest.NewServer(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	res := cliRun(t, invocation{
		Home: home, Args: []string{"doctor"},
		Env: []string{"PAY_BASE_URL=" + srv.URL, "PAY_AUTH_MODE=anonymous"},
	})
	check := doctorChecks(t, res)["api_key_support"]
	switch check["status"] {
	case CheckOK:
		conn := res.data(t)["connection"].(map[string]any)
		list, _ := conn["api_key_collections"].([]any)
		if len(list) == 0 {
			t.Errorf("api_key_support is ok but no collection was named")
		}
	case CheckWarn, CheckUnknown:
		hint, _ := check["hint"].(string)
		if !strings.Contains(hint, "--jwt") {
			t.Errorf("without API-key support the hint must print the JWT command, got %q", hint)
		}
	default:
		t.Errorf("unexpected status %v", check["status"])
	}
}

func TestDoctorNeverPrintsACredential(t *testing.T) {
	srv := payloadtest.NewServer(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	res := cliRun(t, invocation{
		Home: home, Args: []string{"doctor"},
		Env: []string{"PAY_BASE_URL=" + srv.URL, "PAY_API_KEY=" + fixtureKey},
	})
	if strings.Contains(res.Stdout, fixtureKey) || strings.Contains(res.Stderr, fixtureKey) {
		t.Fatalf("doctor leaked the API key")
	}
}

func TestAccessReturnsTheWholeMatrix(t *testing.T) {
	srv := payloadtest.NewServer(t)
	res := cliRun(t, invocation{
		Args: []string{"access", "--base-url", srv.URL, "--auth-mode", "anonymous"},
	})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}
	d := res.data(t)
	slugs, _ := d["collection_slugs"].([]any)
	if len(slugs) < 10 {
		t.Fatalf("only %d collection slugs", len(slugs))
	}
	if _, ok := d["can_access_admin"]; !ok {
		t.Errorf("can_access_admin missing")
	}
	if strings.Contains(res.Stdout, fixtureKey) {
		t.Errorf("access leaked the recorded key")
	}
}

func TestAccessOneCollectionAndUnknownSuggestion(t *testing.T) {
	srv := payloadtest.NewServer(t)
	one := cliRun(t, invocation{
		Args: []string{"access", "pages", "--base-url", srv.URL, "--auth-mode", "anonymous"},
	})
	if one.Code != 0 {
		t.Fatalf("exit = %d\n%s", one.Code, one.Stdout)
	}
	if one.data(t)["collection"] != "pages" {
		t.Errorf("collection = %v", one.data(t)["collection"])
	}
	miss := cliRun(t, invocation{
		Args: []string{"access", "page", "--base-url", srv.URL, "--auth-mode", "anonymous"},
	})
	if got := miss.code(t); got != "collection_unknown" {
		t.Fatalf("code = %q, want collection_unknown", got)
	}
	e, _ := miss.Env["error"].(map[string]any)
	dym, _ := e["did_you_mean"].([]any)
	if len(dym) == 0 {
		t.Errorf("no did_you_mean for a near-miss slug")
	}
}

func TestCanUsesExitCodesAsTheAnswer(t *testing.T) {
	srv := payloadtest.NewServer(t)
	run := func(args ...string) cliResult {
		return cliRun(t, invocation{
			Args: append(args, "--base-url", srv.URL, "--auth-mode", "anonymous"),
		})
	}
	read := run("can", "read", "pages")
	switch read.Code {
	case 0:
		d := read.data(t)
		// Exit 0 means either "permitted" or "the server never said" — §7.6's
		// tri-state rule forbids turning an unknown into a denial.
		if d["permitted"] != true && d["known"] != false {
			t.Errorf("exit 0 with permitted=%v known=%v", d["permitted"], d["known"])
		}
	case 8:
		if read.code(t) != "access_denied" {
			t.Errorf("exit 8 with code %q", read.code(t))
		}
	default:
		t.Fatalf("unexpected exit %d\n%s", read.Code, read.Stdout)
	}

	if got := run("can", "frobnicate", "pages").code(t); got != "invalid_option" {
		t.Errorf("bad operation = %q, want invalid_option", got)
	}
	if got := run("can", "read", "no-such-collection").code(t); got != "collection_unknown" {
		t.Errorf("bad collection = %q, want collection_unknown", got)
	}
	if got := run("can", "read").code(t); got != "invalid_args" {
		t.Errorf("missing collection = %q, want invalid_args", got)
	}
}

func TestDiscoverServesAndRefreshesTheCache(t *testing.T) {
	srv := payloadtest.NewServer(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	env := []string{"PAY_BASE_URL=" + srv.URL, "PAY_AUTH_MODE=anonymous"}

	// A warm cache serves `pay explain` with no discovery run at all.
	warm := cliRun(t, invocation{Home: home, Args: []string{"explain", "--slim"}, Env: env})
	if warm.Code != 0 {
		t.Fatalf("warm explain: exit %d\n%s", warm.Code, warm.Stdout)
	}
	if n := srv.Count("GET", "/api/access"); n != 0 {
		t.Errorf("a warm cache still issued %d /api/access requests", n)
	}
	meta := warm.Env["meta"].(map[string]any)
	cacheMeta, _ := meta["cache"].(map[string]any)
	if cacheMeta == nil || cacheMeta["discovery"] != "hit" {
		t.Errorf("meta.cache.discovery = %v, want hit", cacheMeta)
	}
}

func TestDiscoverRejectsBadArguments(t *testing.T) {
	res := cliRun(t, invocation{Args: []string{"discover", "extra-arg"}})
	if got := res.code(t); got != "invalid_args" {
		t.Errorf("code = %q, want invalid_args", got)
	}
}
