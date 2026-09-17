package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// panicTransport is §17.1's tier-1 guarantee: a unit test that reaches the
// network fails loudly instead of depending on a machine with connectivity.
type panicTransport struct{}

func (panicTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	panic("unit test attempted a real network request: " + r.URL.Host)
}

func TestMain(m *testing.M) {
	http.DefaultTransport = panicTransport{}
	http.DefaultClient = &http.Client{Transport: panicTransport{}}
	_ = os.Setenv("PAY_NO_UPDATE", "1")
	os.Exit(m.Run())
}

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

// cliResult is one whole-command run.
type cliResult struct {
	Stdout string
	Stderr string
	Code   int
	Env    map[string]any
}

// invocation describes one `pay …` run in-process.
type invocation struct {
	Home    string
	Args    []string
	Env     []string
	Stdin   string
	HTTP    http.RoundTripper
	WorkDir string
	TTY     bool
	Clock   func() time.Time
}

func cliRun(t *testing.T, in invocation) cliResult {
	t.Helper()
	if in.Home == "" {
		in.Home = t.TempDir()
	}
	if in.WorkDir == "" {
		in.WorkDir = in.Home
	}
	if in.Clock == nil {
		in.Clock = payloadtest.Frozen().Now
	}
	env := append([]string{
		"PAY_HOME=" + in.Home,
		"HOME=" + in.Home,
		"USERPROFILE=" + in.Home,
		"PAY_NO_UPDATE=1",
	}, in.Env...)

	var out, errBuf bytes.Buffer
	code := Run(context.Background(), App{
		Args:       in.Args,
		Env:        env,
		Stdin:      strings.NewReader(in.Stdin),
		Stdout:     &out,
		Stderr:     &errBuf,
		Now:        in.Clock,
		HTTP:       in.HTTP,
		WorkDir:    in.WorkDir,
		StdinIsTTY: in.TTY,
		NoSignals:  true,
	})
	res := cliResult{Stdout: out.String(), Stderr: errBuf.String(), Code: code}
	if trimmed := strings.TrimSpace(res.Stdout); strings.HasPrefix(trimmed, "{") {
		dec := json.NewDecoder(strings.NewReader(trimmed))
		dec.UseNumber()
		if err := dec.Decode(&res.Env); err != nil {
			res.Env = nil
		}
	}
	return res
}

func (r cliResult) data(t *testing.T) map[string]any {
	t.Helper()
	if r.Env == nil {
		t.Fatalf("stdout is not a JSON envelope:\n%s\nstderr:\n%s", r.Stdout, r.Stderr)
	}
	d, ok := r.Env["data"].(map[string]any)
	if !ok {
		t.Fatalf("envelope has no object data: %v", r.Env["data"])
	}
	return d
}

func (r cliResult) code(t *testing.T) string {
	t.Helper()
	if r.Env == nil {
		return ""
	}
	e, ok := r.Env["error"].(map[string]any)
	if !ok {
		return ""
	}
	s, _ := e["code"].(string)
	return s
}

func (r cliResult) warnings(t *testing.T) []map[string]any {
	t.Helper()
	raw, _ := r.Env["warnings"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, w := range raw {
		if m, ok := w.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func (r cliResult) hasWarning(t *testing.T, code string) bool {
	t.Helper()
	for _, w := range r.warnings(t) {
		if w["code"] == code {
			return true
		}
	}
	return false
}

// seedDiscovery writes the recorded manifest and the pages shard into the cache
// for the scope a given base URL resolves to, so a command that needs a schema
// runs offline. The generation and meta.scope are rewritten because the cache
// verifies both on read (§8.2).
func seedDiscovery(t *testing.T, home, baseURL string) cache.Scope {
	t.Helper()
	sc, err := cache.NewScope(cache.ScopeInput{
		BaseURL:        baseURL,
		APIPath:        "/api",
		GraphQLPath:    "/api/graphql",
		KeyFingerprint: cache.AnonKeyFingerprint,
	})
	if err != nil {
		t.Fatalf("scope: %v", err)
	}

	var m discovery.Manifest
	payloadtest.LoadJSON(t, "manifest", &m)
	var shard discovery.Shard
	payloadtest.LoadJSON(t, "fields_pages", &shard)

	now := payloadtest.Epoch
	generation := cache.NewGeneration(now)
	m.Generation = generation
	m.Meta.Scope = sc.Key
	m.GeneratedAt = now
	m.ExpiresAt = now.Add(24 * time.Hour)
	shard.Generation = generation

	store := cache.New(filepath.Join(home, "cache"))
	ok, warns := store.WriteSet(sc, cache.Set{
		Generation: generation,
		Manifest:   &m,
		Shards:     map[string]any{cache.ShardName(shard.Slug, cache.KindCollection): &shard},
	}, now)
	if !ok {
		t.Fatalf("seed cache write failed: %v", warns)
	}
	return sc
}

// seededEnv is the environment a cache-backed, offline command needs.
func seededEnv(baseURL string) []string {
	return []string{"PAY_BASE_URL=" + baseURL, "PAY_AUTH_MODE=anonymous"}
}

const testBaseURL = "http://localhost:3900"

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

func TestRunAlwaysEmitsAnEnvelope(t *testing.T) {
	cases := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"no command", nil, 5, "invalid_args"},
		{"unknown command", []string{"frobnicate"}, 5, "invalid_args"},
		{"unknown flag", []string{"version", "--nope"}, 5, "invalid_args"},
		{"unknown subcommand", []string{"auth", "frobnicate"}, 5, "invalid_args"},
		{"too many args", []string{"version", "a", "b"}, 5, "invalid_args"},
		{"bad output format", []string{"version", "--output", "yaml"}, 5, "invalid_option"},
		{"bad errors-to", []string{"version", "--errors-to", "syslog"}, 5, "invalid_option"},
		{"bad path expression", []string{"version", "--path", ".a | .b"}, 5, "invalid_path_expr"},
		{"bad log level", []string{"version", "--log-level", "screaming"}, 5, "invalid_option"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := cliRun(t, invocation{Args: tc.args})
			if res.Code != tc.code {
				t.Errorf("exit = %d, want %d\nstdout: %s", res.Code, tc.code, res.Stdout)
			}
			if got := res.code(t); got != tc.want {
				t.Errorf("error code = %q, want %q\nstdout: %s", got, tc.want, res.Stdout)
			}
			if res.Env == nil {
				t.Fatalf("no envelope on stdout: %q", res.Stdout)
			}
			if ok, _ := res.Env["ok"].(bool); ok {
				t.Errorf("ok = true on a failure")
			}
			if res.Env["data_kind"] != "error" {
				t.Errorf("data_kind = %v, want error", res.Env["data_kind"])
			}
			if v, _ := res.Env["v"].(json.Number); v.String() != "1" {
				t.Errorf("v = %v, want 1", res.Env["v"])
			}
		})
	}
}

func TestErrorEnvelopeGoesToStdoutByDefault(t *testing.T) {
	res := cliRun(t, invocation{Args: []string{"frobnicate"}})
	if !strings.Contains(res.Stdout, `"code": "invalid_args"`) {
		t.Fatalf("envelope not on stdout: %q", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "pay: invalid_args (exit 5)") {
		t.Errorf("human summary missing from stderr: %q", res.Stderr)
	}
}

func TestErrorsToStderrMovesTheEnvelope(t *testing.T) {
	for _, via := range []invocation{
		{Args: []string{"frobnicate", "--errors-to", "stderr"}},
		{Args: []string{"frobnicate"}, Env: []string{"PAY_ERRORS_TO=stderr"}},
	} {
		res := cliRun(t, via)
		if strings.Contains(res.Stdout, `"code"`) {
			t.Errorf("envelope leaked to stdout: %q", res.Stdout)
		}
		if !strings.Contains(res.Stderr, `"code": "invalid_args"`) {
			t.Errorf("envelope not on stderr: %q", res.Stderr)
		}
	}
}

func TestQuietSuppressesTheHumanSummary(t *testing.T) {
	res := cliRun(t, invocation{Args: []string{"frobnicate", "--quiet"}})
	if strings.Contains(res.Stderr, "pay:") {
		t.Errorf("--quiet still printed a summary: %q", res.Stderr)
	}
	if !strings.Contains(res.Stdout, `"invalid_args"`) {
		t.Errorf("--quiet suppressed the envelope too: %q", res.Stdout)
	}
}

func TestExitCodeMatchesTheEnvelope(t *testing.T) {
	res := cliRun(t, invocation{Args: []string{"whoami"}})
	// No base URL configured anywhere: config_missing, exit 9.
	if res.Code != 9 {
		t.Fatalf("exit = %d, want 9\n%s", res.Code, res.Stdout)
	}
	if got := res.code(t); got != "config_missing" {
		t.Errorf("code = %q, want config_missing", got)
	}
}

func TestVersionWorksWithNoConfigAtAll(t *testing.T) {
	res := cliRun(t, invocation{Args: []string{"version"}})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}
	d := res.data(t)
	if d["version"] == nil || d["os"] == nil || d["arch"] == nil {
		t.Errorf("build stamp is incomplete: %v", d)
	}
}

func TestAppNormaliseSubstitutesInertDefaults(t *testing.T) {
	// A zero App must not panic: Run substitutes io.Discard and a fixed clock.
	code := Run(context.Background(), App{Args: []string{"version"}, NoSignals: true})
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
}

func TestMetaCarriesTheInvariantFields(t *testing.T) {
	res := cliRun(t, invocation{Args: []string{"version"}})
	meta, ok := res.Env["meta"].(map[string]any)
	if !ok {
		t.Fatalf("no meta: %v", res.Env)
	}
	for _, key := range []string{"cli_version", "profile", "duration_ms"} {
		if _, ok := meta[key]; !ok {
			t.Errorf("meta.%s missing", key)
		}
	}
	if _, ok := res.Env["warnings"].([]any); !ok {
		t.Errorf("warnings must always be an array, got %T", res.Env["warnings"])
	}
}

func TestPathExpressionAppliesToDataOnly(t *testing.T) {
	res := cliRun(t, invocation{Args: []string{"version", "--path", ".version"}})
	if res.Code != 0 {
		t.Fatalf("exit = %d: %s", res.Code, res.Stdout)
	}
	if _, ok := res.Env["meta"]; !ok {
		t.Errorf("--path must replace data only, meta survived? %v", res.Env)
	}
	if _, ok := res.Env["data"].(string); !ok {
		t.Errorf("data = %T, want the bare version string", res.Env["data"])
	}
}

// TestNoSecretInAnyOutput is §3.1's repo-wide assertion: no golden file and no
// fixture may contain the fixture key.
func TestNoSecretInAnyOutput(t *testing.T) {
	const fixtureKey = "paycli-dev-key-deadbeefdeadbeef"
	roots := []string{payloadtest.FixtureDir(t), payloadtest.GoldenDir(t)}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if d.IsDir() {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if bytes.Contains(data, []byte(fixtureKey)) {
				t.Errorf("%s contains the fixture API key", path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
}

// ---------------------------------------------------------------------------
// §17.3 CLI-level golden tests
//
// Because Run takes injected IO and returns the exit code, a whole command runs
// in-process and its two streams plus its status are compared byte-for-byte.
// UPDATE_GOLDEN=1 rewrites them.
// ---------------------------------------------------------------------------

func TestGoldenCommands(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		seeded bool
		env    []string
	}{
		{name: "error_unknown_command", args: []string{"frobnicate"}},
		{name: "error_unknown_flag", args: []string{"version", "--nope"}},
		{name: "error_bad_output", args: []string{"version", "--output", "yaml"}},
		{name: "error_no_base_url", args: []string{"whoami"}},
		{name: "error_bad_path_expr", args: []string{"version", "--path", ".a|.b"}},
		{name: "help_root", args: []string{"--help"}},
		{name: "help_explain", args: []string{"explain", "--help"}},
		{name: "help_describe_cold", args: []string{"describe", "--help"}},
		{name: "explain_slim", args: []string{"explain", "--slim"}, seeded: true},
		{name: "explain_section_gotchas", args: []string{"explain", "--section", "gotchas"}, seeded: true},
		{name: "collections_grep_pages", args: []string{"collections", "--grep", "pages"}, seeded: true},
		{name: "describe_field_title", args: []string{"describe", "pages", "--field", "title"}, seeded: true},
		{name: "command_spec_describe", args: []string{"describe", "--help", "--output", "json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			env := tc.env
			if tc.seeded {
				seedDiscovery(t, home, testBaseURL)
				env = append(env, seededEnv(testBaseURL)...)
			}
			res := cliRun(t, invocation{Home: home, Args: tc.args, Env: env})
			payloadtest.Golden(t, tc.name, payloadtest.Result{
				Stdout: []byte(res.Stdout), Stderr: []byte(res.Stderr), Exit: res.Code,
			})
		})
	}
}
