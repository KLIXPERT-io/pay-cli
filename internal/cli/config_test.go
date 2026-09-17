package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigPathsAreAlwaysAnswerable(t *testing.T) {
	home := t.TempDir()
	res := cliRun(t, invocation{Home: home, Args: []string{"config", "paths"}})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
	}
	d := res.data(t)
	for _, key := range []string{
		"config_dir", "cache_dir", "state_dir", "config_file",
		"credentials_file", "audit_file", "sources",
	} {
		if _, ok := d[key]; !ok {
			t.Errorf("config paths is missing %q: %v", key, d)
		}
	}
	if !strings.HasPrefix(d["config_dir"].(string), home) {
		t.Errorf("PAY_HOME was ignored: %v", d["config_dir"])
	}
}

func TestConfigExplainCarriesProvenance(t *testing.T) {
	res := cliRun(t, invocation{
		Args: []string{"config", "explain"},
		Env:  []string{"PAY_BASE_URL=" + testBaseURL, "PAY_LIMIT=7"},
	})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
	}
	d := res.data(t)
	base, ok := d["base_url"].(map[string]any)
	if !ok {
		t.Fatalf("no base_url setting: %v", d)
	}
	if base["source"] != "env:PAY_BASE_URL" {
		t.Errorf("base_url source = %v, want env:PAY_BASE_URL", base["source"])
	}
	limit, _ := d["limit"].(map[string]any)
	if limit["source"] != "env:PAY_LIMIT" {
		t.Errorf("limit source = %v", limit["source"])
	}
	depth, _ := d["depth"].(map[string]any)
	if depth["source"] != "default" {
		t.Errorf("depth source = %v, want default", depth["source"])
	}
}

func TestConfigExplainNeverPrintsASecret(t *testing.T) {
	const key = "super-secret-key-value-1234"
	res := cliRun(t, invocation{
		Args: []string{"config", "explain"},
		Env: []string{
			"PAY_BASE_URL=" + testBaseURL,
			"PAY_API_KEY=" + key,
		},
	})
	if strings.Contains(res.Stdout, key) || strings.Contains(res.Stderr, key) {
		t.Fatalf("the API key reached the output:\n%s\n%s", res.Stdout, res.Stderr)
	}
}

func TestConfigSetGetUnsetRoundTrip(t *testing.T) {
	home := t.TempDir()
	steps := []struct {
		name string
		args []string
		want string
	}{
		{"set a string", []string{"config", "set", "defaults.output", "table"}, ""},
		{"set an int", []string{"config", "set", "defaults.limit", "50"}, ""},
		{"set a profile field", []string{"config", "set", "profiles.staging.base_url", "https://staging.example.com"}, ""},
		{"set the default profile", []string{"config", "set", "default_profile", "staging"}, ""},
	}
	for _, s := range steps {
		res := cliRun(t, invocation{Home: home, Args: s.args})
		if res.Code != 0 {
			t.Fatalf("%s: exit = %d\n%s", s.name, res.Code, res.Stdout)
		}
	}

	// defaults.output = table is now in the file, so a command with no --output
	// must render a table rather than an envelope. That is the check that the
	// setting took effect; everything after it asks for JSON explicitly.
	tabular := cliRun(t, invocation{Home: home, Args: []string{"config", "get", "limit"}})
	if tabular.Env != nil {
		t.Errorf("defaults.output = table did not take effect:\n%s", tabular.Stdout)
	}

	got := cliRun(t, invocation{Home: home, Args: []string{"config", "get", "limit", "--output", "json"}})
	if v := got.data(t)["value"]; v.(interface{ String() string }).String() != "50" {
		t.Errorf("limit = %v, want 50", v)
	}
	got = cliRun(t, invocation{Home: home, Args: []string{"config", "get", "profile", "--output", "json"}})
	if v := got.data(t)["value"]; v != "staging" {
		t.Errorf("profile = %v, want staging", v)
	}

	// The file PayCLI wrote must be one PayCLI can read back.
	listed := cliRun(t, invocation{Home: home, Args: []string{"config", "list", "--output", "json"}})
	if listed.Code != 0 {
		t.Fatalf("config list failed: %s", listed.Stdout)
	}
	profiles, _ := listed.data(t)["profiles"].([]any)
	found := false
	for _, p := range profiles {
		if p == "staging" {
			found = true
		}
	}
	if !found {
		t.Errorf("profiles = %v, want staging", profiles)
	}

	unset := cliRun(t, invocation{Home: home, Args: []string{"config", "unset", "defaults.output", "--output", "json"}})
	if unset.data(t)["removed"] != true {
		t.Errorf("unset did not report a removal: %v", unset.data(t))
	}
	again := cliRun(t, invocation{Home: home, Args: []string{"config", "unset", "defaults.output", "--output", "json"}})
	if again.Code != 0 || again.data(t)["removed"] != false {
		t.Errorf("unset is not idempotent: exit %d, %v", again.Code, again.data(t))
	}
	back := cliRun(t, invocation{Home: home, Args: []string{"config", "get", "output"}})
	if back.Env == nil {
		t.Fatalf("after unset, output is still not json:\n%s", back.Stdout)
	}
	if back.data(t)["source"] != "default" {
		t.Errorf("after unset, output source = %v, want default", back.data(t)["source"])
	}
}

// §4.2: an api_key at any depth is a hard error, both on read and on write.
func TestConfigRefusesToWriteAnAPIKey(t *testing.T) {
	home := t.TempDir()
	res := cliRun(t, invocation{
		Home: home,
		Args: []string{"config", "set", "profiles.default.api_key", "sk-live-nope"},
	})
	if got := res.code(t); got != "config_secret_in_plaintext" {
		t.Fatalf("code = %q, want config_secret_in_plaintext\n%s", got, res.Stdout)
	}
	if res.Code != 9 {
		t.Errorf("exit = %d, want 9", res.Code)
	}
	if strings.Contains(res.Stdout, "sk-live-nope") {
		t.Errorf("the rejected key was echoed back:\n%s", res.Stdout)
	}
	if data, err := os.ReadFile(filepath.Join(home, "config.toml")); err == nil {
		if strings.Contains(string(data), "api_key") {
			t.Errorf("api_key reached the config file")
		}
	}
}

func TestConfigRefusesToLoadAnAPIKey(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, []byte("[profiles.default]\napi_key = \"sk-live-nope\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := cliRun(t, invocation{Home: home, Args: []string{"config", "paths"}})
	if got := res.code(t); got != "config_secret_in_plaintext" {
		t.Fatalf("code = %q, want config_secret_in_plaintext\n%s", got, res.Stdout)
	}
	if strings.Contains(res.Stdout, "sk-live-nope") {
		t.Errorf("the plaintext key was echoed back:\n%s", res.Stdout)
	}
}

func TestConfigGetSuggestsOnAnUnknownKey(t *testing.T) {
	res := cliRun(t, invocation{Args: []string{"config", "get", "outpt"}})
	if got := res.code(t); got != "invalid_args" {
		t.Fatalf("code = %q, want invalid_args", got)
	}
	e, _ := res.Env["error"].(map[string]any)
	dym, _ := e["did_you_mean"].([]any)
	for _, s := range dym {
		if s == "output" {
			return
		}
	}
	t.Errorf("did_you_mean = %v, want it to contain \"output\"", dym)
}

func TestConfigListRedactsHeaderValues(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	body := "[profiles.default]\nbase_url = \"http://localhost:3900\"\n\n" +
		"[profiles.default.headers]\nx-bypass-token = \"a-very-secret-header-value\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	res := cliRun(t, invocation{Home: home, Args: []string{"config", "list"}})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
	}
	if strings.Contains(res.Stdout, "a-very-secret-header-value") {
		t.Fatalf("a configured header value reached the output:\n%s", res.Stdout)
	}
}

func TestConfigSetRejectsAMalformedKey(t *testing.T) {
	for _, key := range []string{"", "a..b", "defaults."} {
		res := cliRun(t, invocation{Args: []string{"config", "set", key, "x"}})
		if res.Code == 0 {
			t.Errorf("key %q was accepted", key)
		}
	}
}

// TestConfigNeverPrintsAHeaderValue is the §5.3 regression guard for
// finding 16: header values are secret because of WHERE they sit, not because
// of what they are named. The fixture deliberately uses names no key-name rule
// matches (the older x-bypass-token fixture is caught by the generic
// (?i)token rule and so passed before the headers rule existed).
func TestConfigNeverPrintsAHeaderValue(t *testing.T) {
	const (
		tenant = "tenant-live-9d1f4c2ab7e6"
		client = "cf-id-abc123-not-a-token"
		custom = "super-sekret-proxy-cred"
	)
	home := t.TempDir()
	body := "[profiles.default]\nbase_url = \"http://localhost:3900\"\n\n" +
		"[profiles.default.headers]\n" +
		"X-Tenant = \"" + tenant + "\"\n" +
		"CF-Access-Client-Id = \"" + client + "\"\n" +
		"X-Custom-Thing = \"" + custom + "\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := [][]string{
		{"config", "list"},
		{"config", "list", "--no-redact"},
		{"config", "explain"},
		{"config", "get", "profiles"},
		{"config", "get", "profiles.default"},
		{"config", "get", "profiles.default.headers"},
		{"config", "get", "profiles.default.headers.X-Tenant"},
		{"config", "get", "profiles.default.headers.X-Tenant", "--no-redact"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args[1:], " "), func(t *testing.T) {
			res := cliRun(t, invocation{Home: home, Args: args})
			if res.Code != 0 {
				t.Fatalf("exit = %d\n%s\n%s", res.Code, res.Stdout, res.Stderr)
			}
			for _, secret := range []string{tenant, client, custom} {
				if strings.Contains(res.Stdout, secret) || strings.Contains(res.Stderr, secret) {
					t.Errorf("header value %q reached the output of %v:\n%s", secret, args, res.Stdout)
				}
			}
		})
	}
}

// TestConfigStillPrintsNonHeaderValues keeps the headers rule from turning into
// "mask everything": only values under a headers table are secret.
func TestConfigStillPrintsNonHeaderValues(t *testing.T) {
	home := t.TempDir()
	body := "[profiles.default]\nbase_url = \"http://localhost:3900\"\n" +
		"x-headers-hint = \"keep-me-visible\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	res := cliRun(t, invocation{Home: home, Args: []string{"config", "list"}})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
	}
	for _, want := range []string{"http://localhost:3900", "keep-me-visible"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("config list hid %q:\n%s", want, res.Stdout)
		}
	}
}

// TestConfigSetAcceptsCommaListsForListKeys closes a §7.10 violation: PayCLI
// PRINTS `pay config set profiles.<p>.locales en,de` as a hint (the string is
// mandated verbatim by the spec and by discovery/locales.go's
// LocaleUnverifiedHint), but the generic value typer produced the STRING
// "en,de", which config.Parse then rejected with "TOML value has type string;
// destination has type slice". The advice PayCLI gives could not be followed.
func TestConfigSetAcceptsCommaListsForListKeys(t *testing.T) {
	tests := []struct {
		name     string
		segments []string
		value    string
		want     any
	}{
		{"locales", []string{"profiles", "dummy", "locales"}, "en,de", []string{"en", "de"}},
		{"echo_check_ignore", []string{"defaults", "echo_check_ignore"}, "a,b", []string{"a", "b"}},
		{"custom_endpoints", []string{"profiles", "p", "custom_endpoints"}, "/x", []string{"/x"}},
		{"blocks.<field> is a list too", []string{"profiles", "p", "blocks", "layout"}, "hero,cta", []string{"hero", "cta"}},
		{"whitespace is trimmed", []string{"defaults", "echo_check_ignore"}, " a , b ", []string{"a", "b"}},
		{"the explicit array form still wins", []string{"profiles", "p", "locales"}, `["en","de"]`, []any{"en", "de"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := typedConfigValueFor(tc.segments, tc.value)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("typedConfigValueFor(%v, %q) = %#v, want %#v",
					tc.segments, tc.value, got, tc.want)
			}
		})
	}

	// A scalar key with a comma in it must NOT become a list.
	for _, tc := range []struct {
		segments []string
		value    string
	}{
		{[]string{"profiles", "p", "base_url"}, "http://a.test/x,y"},
		{[]string{"defaults", "output"}, "table"},
		{[]string{"defaults", "limit"}, "100"},
	} {
		if _, isList := typedConfigValueFor(tc.segments, tc.value).([]string); isList {
			t.Errorf("%v with value %q was turned into a list", tc.segments, tc.value)
		}
	}
}
