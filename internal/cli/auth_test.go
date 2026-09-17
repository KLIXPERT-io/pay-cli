package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
	"github.com/KLIXPERT-io/pay-cli/internal/secret"
)

// fixtureKey is the credential the recorded instance was probed with. It must
// never appear in any output (§3.1's repo-wide assertion covers the files; this
// covers the live streams).
const fixtureKey = "paycli-dev-key-deadbeefdeadbeef"

func TestAuthStatusIsOfflineAndSecretFree(t *testing.T) {
	res := cliRun(t, invocation{Args: []string{"auth", "status"}})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
	}
	d := res.data(t)
	for _, key := range []string{
		"profile", "base_url", "auth_mode", "auth_collection", "auth_collection_source",
		"key_fingerprint", "key_source", "token_exp", "authenticated",
	} {
		if _, ok := d[key]; !ok {
			t.Errorf("auth status is missing %q (§5.3)", key)
		}
	}
	if d["authenticated"] != false {
		t.Errorf("a fresh home reported authenticated = %v", d["authenticated"])
	}
	if d["key_fingerprint"] != nil {
		t.Errorf("key_fingerprint = %v on a fresh home", d["key_fingerprint"])
	}
}

func TestAuthLoginVerifiesStoresAndNeverEchoesTheKey(t *testing.T) {
	srv := payloadtest.NewServer(t)
	home := t.TempDir()

	res := cliRun(t, invocation{
		Home:  home,
		Args:  []string{"auth", "login", "--base-url", srv.URL, "--auth-collection", "users", "--api-key-stdin"},
		Stdin: fixtureKey + "\n",
	})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}
	d := res.data(t)
	if d["verified"] != true {
		t.Errorf("login did not verify: %v", d)
	}
	if d["auth_collection"] != "users" {
		t.Errorf("auth_collection = %v", d["auth_collection"])
	}
	fp, _ := d["key_fingerprint"].(string)
	if len(fp) != 16 {
		t.Errorf("key_fingerprint = %q, want 16 hex characters", fp)
	}
	if strings.Contains(res.Stdout, fixtureKey) || strings.Contains(res.Stderr, fixtureKey) {
		t.Fatalf("the API key reached the output")
	}

	// The credential file exists, is 0600, and the config file has no secret.
	credPath := filepath.Join(home, "credentials.json")
	st, err := os.Stat(credPath)
	if err != nil {
		t.Fatalf("no credentials.json: %v", err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("credentials.json mode = %v, want 0600", st.Mode().Perm())
	}
	cfg, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatalf("no config.toml: %v", err)
	}
	if strings.Contains(string(cfg), fixtureKey) || strings.Contains(string(cfg), "api_key") {
		t.Fatalf("config.toml carries a secret:\n%s", cfg)
	}
	if !strings.Contains(string(cfg), srv.URL) {
		t.Errorf("config.toml did not record the base URL:\n%s", cfg)
	}

	// And the stored credential works on the next invocation.
	test := cliRun(t, invocation{Home: home, Args: []string{"auth", "test"}})
	if test.Code != 0 {
		t.Fatalf("auth test after login: exit = %d\n%s", test.Code, test.Stdout)
	}
	if test.data(t)["verified"] != true {
		t.Errorf("auth test did not verify: %v", test.data(t))
	}
}

func TestAuthTestFailsOnANullUser(t *testing.T) {
	// §5.4's whole point: a WRONG key answers HTTP 200 with {"user":null}.
	// The handler replays exactly that, because a status code cannot express it.
	srv := payloadtest.NewServer(t, payloadtest.WithHandler(
		"GET", "/api/users/me", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("X-Powered-By", "Payload")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payloadtest.Load(t, "users_me_anon"))
		}))
	home := t.TempDir()
	res := cliRun(t, invocation{
		Home: home,
		Args: []string{"auth", "test", "--base-url", srv.URL, "--auth-collection", "users",
			"--auth-mode", "api-key", "--api-key", "wrong-key-value"},
	})
	if res.Code != 2 {
		t.Fatalf("exit = %d, want 2\n%s", res.Code, res.Stdout)
	}
	if got := res.code(t); got != "auth_invalid" {
		t.Errorf("code = %q, want auth_invalid", got)
	}
	if strings.Contains(res.Stdout, "wrong-key-value") {
		t.Errorf("the rejected key was echoed:\n%s", res.Stdout)
	}
}

func TestAuthTestRefusesAnonymousMode(t *testing.T) {
	srv := payloadtest.NewServer(t)
	res := cliRun(t, invocation{
		Args: []string{"auth", "test", "--base-url", srv.URL, "--auth-mode", "anonymous"},
	})
	if got := res.code(t); got != "auth_missing" {
		t.Fatalf("code = %q, want auth_missing\n%s", got, res.Stdout)
	}
	if res.Code != 2 {
		t.Errorf("exit = %d, want 2", res.Code)
	}
}

func TestWhoamiRedactsTheIdentityDocument(t *testing.T) {
	srv := payloadtest.NewServer(t)
	res := cliRun(t, invocation{
		Args: []string{"whoami", "--base-url", srv.URL, "--auth-collection", "users",
			"--auth-mode", "api-key", "--api-key", fixtureKey},
	})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}
	d := res.data(t)
	user, _ := d["user"].(map[string]any)
	if user["apiKey"] != "<redacted>" {
		t.Errorf("apiKey = %v, want <redacted>", user["apiKey"])
	}
	if strings.Contains(res.Stdout, fixtureKey) {
		t.Fatalf("the API key reached stdout")
	}
	if d["verified"] != true {
		t.Errorf("verified = %v", d["verified"])
	}
	if fmtID(d["user_id"]) != "66" {
		t.Errorf("user_id = %v, want 66", d["user_id"])
	}
}

func TestAuthLoginJWTStoresAToken(t *testing.T) {
	srv := payloadtest.NewServer(t)
	home := t.TempDir()
	res := cliRun(t, invocation{
		Home: home,
		Args: []string{"auth", "login", "--jwt", "--base-url", srv.URL,
			"--auth-collection", "users", "--username", "paycli-test", "--password-stdin"},
		Stdin: "paycli-fixture-pw\n",
	})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}
	d := res.data(t)
	if d["auth_mode"] != "jwt" {
		t.Errorf("auth_mode = %v, want jwt", d["auth_mode"])
	}
	if d["login_field"] != "username" {
		t.Errorf("login_field = %v", d["login_field"])
	}
	cred, err := os.ReadFile(filepath.Join(home, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cred), "paycli-fixture-pw") {
		t.Fatalf("the password was stored")
	}
	var parsed map[string]any
	if err := json.Unmarshal(cred, &parsed); err != nil {
		t.Fatalf("credentials.json is not valid JSON: %v", err)
	}
}

func TestAuthLoginJWTRequiresPasswordStdin(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no identifier", []string{"auth", "login", "--jwt", "--base-url", testBaseURL, "--password-stdin"}, "invalid_args"},
		{"both identifiers", []string{"auth", "login", "--jwt", "--base-url", testBaseURL,
			"--email", "a@b.c", "--username", "x", "--password-stdin"}, "invalid_args"},
		{"no --password-stdin", []string{"auth", "login", "--jwt", "--base-url", testBaseURL, "--email", "a@b.c"}, "invalid_args"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := cliRun(t, invocation{Args: tc.args})
			if got := res.code(t); got != tc.want {
				t.Errorf("code = %q, want %q\n%s", got, tc.want, res.Stdout)
			}
		})
	}
}

func TestAuthProfileLifecycle(t *testing.T) {
	srv := payloadtest.NewServer(t)
	home := t.TempDir()

	login := cliRun(t, invocation{
		Home:  home,
		Args:  []string{"auth", "login", "--profile", "staging", "--base-url", srv.URL, "--auth-collection", "users", "--api-key-stdin"},
		Stdin: fixtureKey + "\n",
	})
	if login.Code != 0 {
		t.Fatalf("login: exit %d\n%s", login.Code, login.Stdout)
	}

	list := cliRun(t, invocation{Home: home, Args: []string{"auth", "list"}})
	rows, _ := list.data(t)["profiles"].([]any)
	var staging map[string]any
	for _, r := range rows {
		row := r.(map[string]any)
		if row["name"] == "staging" {
			staging = row
		}
	}
	if staging == nil {
		t.Fatalf("staging not listed: %v", rows)
	}
	if staging["credential"] != "api-key" {
		t.Errorf("credential = %v, want api-key", staging["credential"])
	}
	if strings.Contains(list.Stdout, fixtureKey) {
		t.Fatalf("auth list leaked the key")
	}

	use := cliRun(t, invocation{Home: home, Args: []string{"auth", "use", "staging"}})
	if use.Code != 0 || use.data(t)["default_profile"] != "staging" {
		t.Fatalf("auth use: %d %v", use.Code, use.data(t))
	}

	rename := cliRun(t, invocation{Home: home, Args: []string{"auth", "rename", "staging", "prod"}})
	if rename.Code != 0 {
		t.Fatalf("auth rename: exit %d\n%s", rename.Code, rename.Stdout)
	}
	if rename.data(t)["profile_moved"] != true || rename.data(t)["credential_moved"] != true {
		t.Errorf("rename moved: %v", rename.data(t))
	}

	logout := cliRun(t, invocation{Home: home, Args: []string{"auth", "logout", "prod"}})
	if logout.Code != 0 || logout.data(t)["removed"] != true {
		t.Fatalf("auth logout: %d %v", logout.Code, logout.data(t))
	}
	after := cliRun(t, invocation{Home: home, Args: []string{"auth", "status", "--profile", "prod"}})
	if after.data(t)["authenticated"] != false {
		t.Errorf("still authenticated after logout: %v", after.data(t))
	}
}

func TestAuthUseRejectsAnUnknownProfile(t *testing.T) {
	res := cliRun(t, invocation{Args: []string{"auth", "use", "nope"}})
	if got := res.code(t); got != "profile_unknown" {
		t.Fatalf("code = %q, want profile_unknown\n%s", got, res.Stdout)
	}
	if res.Code != 9 {
		t.Errorf("exit = %d, want 9", res.Code)
	}
}

func TestAuthFixPermsRestoresTheMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes only")
	}
	srv := payloadtest.NewServer(t)
	home := t.TempDir()
	login := cliRun(t, invocation{
		Home:  home,
		Args:  []string{"auth", "login", "--base-url", srv.URL, "--auth-collection", "users", "--api-key-stdin"},
		Stdin: fixtureKey + "\n",
	})
	if login.Code != 0 {
		t.Fatalf("login failed: %s", login.Stdout)
	}
	path := filepath.Join(home, "credentials.json")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	// A world-readable credential file is fatal until it is fixed.
	broken := cliRun(t, invocation{Home: home, Args: []string{"auth", "test"}})
	if got := broken.code(t); got != "auth_insecure_permissions" {
		t.Fatalf("code = %q, want auth_insecure_permissions\n%s", got, broken.Stdout)
	}
	fix := cliRun(t, invocation{Home: home, Args: []string{"auth", "fix-perms"}})
	if fix.Code != 0 {
		t.Fatalf("fix-perms: exit %d\n%s", fix.Code, fix.Stdout)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v after fix-perms", st.Mode().Perm())
	}
	fixed := cliRun(t, invocation{Home: home, Args: []string{"auth", "test"}})
	if fixed.Code != 0 {
		t.Errorf("auth test still fails after fix-perms: %s", fixed.Stdout)
	}
}

func fmtID(v any) string {
	switch t := v.(type) {
	case json.Number:
		return t.String()
	case string:
		return t
	default:
		return ""
	}
}

// TestEveryFingerprintProducerAgrees is §4.4's cross-package assertion: the
// credential fingerprint written to credentials.json, published in the
// manifest, keyed on by the §8.1 cache scope and printed in the transport's
// redacted Authorization line must be the same 16 characters, or a profile can
// no longer be matched to a cache scope by string equality.
func TestEveryFingerprintProducerAgrees(t *testing.T) {
	const credential = "paycli-fixture-credential-0123456789"

	want := redact.Fingerprint(credential)
	if len(want) != 16 {
		t.Fatalf("fingerprint length = %d, want 16", len(want))
	}
	producers := map[string]string{
		"redact.Fingerprint":     want,
		"secret.Fingerprint":     secret.Fingerprint(credential),
		"secret.FingerprintFor":  secret.FingerprintFor(secret.ModeAPIKey, credential),
		"cache.KeyFingerprint":   cache.KeyFingerprint(credential),
		"redact.MaskSecret(fp=)": strings.TrimSuffix(strings.TrimPrefix(redact.MaskSecret(credential), "<redacted:fp="), ">"),
	}
	for name, got := range producers {
		if got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	// The anonymous sentinel is a literal, not a hash, in every producer.
	if secret.Fingerprint("") != "anon" || cache.KeyFingerprint("") != "anon" {
		t.Errorf("anonymous fingerprint = %q / %q, want \"anon\" from both",
			secret.Fingerprint(""), cache.KeyFingerprint(""))
	}
}

// TestAutoAuthCollectionNeverReachesTheWire is the §7.0 seam: auth_collection
// defaults to the literal "auto", which is a placeholder and not a slug. A
// request sent as `Authorization: auto API-Key <key>` returns HTTP 200 with the
// ANONYMOUS view (§7.0), so every authenticated command would silently degrade
// instead of failing. The client must therefore carry the slug Stage -1
// resolved before it issues anything.
func TestAutoAuthCollectionNeverReachesTheWire(t *testing.T) {
	// /{slug}/init is Stage -1 step 3's filter and is not one of the recorded
	// bodies, so it is served here: `users` is an auth collection, every other
	// candidate is not.
	srv := payloadtest.NewServer(t, payloadtest.WithHandler(
		"GET", "/api/users/init", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("X-Powered-By", "Payload")
			_, _ = w.Write([]byte(`{"initialized":true}`))
		}))
	home := t.TempDir()

	login := cliRun(t, invocation{
		Home:  home,
		Args:  []string{"auth", "login", "--base-url", srv.URL, "--api-key-stdin"},
		Stdin: fixtureKey + "\n",
	})
	if login.Code != 0 {
		t.Fatalf("login with auth_collection=auto: exit = %d\n%s\n%s", login.Code, login.Stdout, login.Stderr)
	}
	d := login.data(t)
	if d["auth_collection"] != "users" {
		t.Errorf("auth_collection = %v, want the slug Stage -1 resolved", d["auth_collection"])
	}
	if d["verified"] != true {
		t.Errorf("login did not verify: %v", d)
	}

	srv.Reset()
	who := cliRun(t, invocation{Home: home, Args: []string{"whoami"}})
	if who.Code != 0 {
		t.Fatalf("whoami: exit = %d\n%s\n%s", who.Code, who.Stdout, who.Stderr)
	}

	authenticated := 0
	for _, rec := range srv.Requests() {
		got := rec.Header.Get("Authorization")
		if got == "" {
			continue
		}
		authenticated++
		if strings.HasPrefix(got, "auto ") {
			t.Fatalf("%s %s carried the unresolved placeholder: %q", rec.Method, rec.Path, got)
		}
		if !strings.HasPrefix(got, "users API-Key ") {
			t.Errorf("%s %s Authorization = %q, want the resolved slug", rec.Method, rec.Path, got)
		}
	}
	if authenticated == 0 {
		t.Fatal("whoami sent no authenticated request")
	}
}
