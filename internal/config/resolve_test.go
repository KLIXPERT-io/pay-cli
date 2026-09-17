package config

import (
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// fullStack builds an Input in which *every* layer of §4.5 supplies a value
// for both a profile-level field (base_url) and a [defaults]-level field
// (limit). Peeling one layer off at a time is what proves the ordering.
func fullStack() Input {
	limit20, limit30 := 20, 30
	return Input{
		Flags: Flags{
			Profile: "local",
			BaseURL: "https://flag.example.com",
			Limit:   intp(5),
		},
		Env: Env{
			"PAY_BASE_URL": "https://env.example.com",
			"PAY_LIMIT":    "10",
		},
		Project: &File{
			Path: "/repo/pay.toml", Kind: KindProject, Exists: true,
			Defaults: Defaults{Limit: &limit20},
			Profiles: map[string]Profile{
				"local": {BaseURL: "https://project.example.com"},
			},
		},
		User: &File{
			Path: "/home/u/.config/pay/config.toml", Kind: KindUser, Exists: true,
			Defaults: Defaults{Limit: &limit30},
			Profiles: map[string]Profile{
				"local": {BaseURL: "https://user.example.com"},
			},
		},
	}
}

func intp(v int) *int    { return &v }
func boolp(v bool) *bool { return &v }

// TestResolvePrecedence is §4.5's matrix: flag > env > project profile >
// user profile > project defaults > user defaults > built-in default.
func TestResolvePrecedence(t *testing.T) {
	tests := []struct {
		name       string
		peel       func(*Input)
		wantURL    string
		wantURLSrc string
		wantLimit  int
		wantLimSrc string
	}{
		{
			name:       "flag wins everything",
			peel:       func(*Input) {},
			wantURL:    "https://flag.example.com",
			wantURLSrc: SourceFlag,
			wantLimit:  5,
			wantLimSrc: SourceFlag,
		},
		{
			name: "env beats every file",
			peel: func(in *Input) {
				in.Flags.BaseURL = ""
				in.Flags.Limit = nil
			},
			wantURL:    "https://env.example.com",
			wantURLSrc: "env:PAY_BASE_URL",
			wantLimit:  10,
			wantLimSrc: "env:PAY_LIMIT",
		},
		{
			name: "set-but-empty env counts as unset",
			peel: func(in *Input) {
				in.Flags.BaseURL = ""
				in.Flags.Limit = nil
				in.Env["PAY_BASE_URL"] = ""
				in.Env["PAY_LIMIT"] = ""
			},
			wantURL:    "https://project.example.com",
			wantURLSrc: "project:/repo/pay.toml",
			wantLimit:  20,
			wantLimSrc: "project:/repo/pay.toml",
		},
		{
			name: "project profile beats user profile",
			peel: func(in *Input) {
				in.Flags.BaseURL = ""
				in.Flags.Limit = nil
				delete(in.Env, "PAY_BASE_URL")
				delete(in.Env, "PAY_LIMIT")
			},
			wantURL:    "https://project.example.com",
			wantURLSrc: "project:/repo/pay.toml",
			wantLimit:  20,
			wantLimSrc: "project:/repo/pay.toml",
		},
		{
			name: "user profile when the project has none",
			peel: func(in *Input) {
				in.Flags.BaseURL = ""
				in.Flags.Limit = nil
				delete(in.Env, "PAY_BASE_URL")
				delete(in.Env, "PAY_LIMIT")
				in.Project.Profiles = map[string]Profile{}
				in.Project.Defaults = Defaults{}
			},
			wantURL:    "https://user.example.com",
			wantURLSrc: "user:/home/u/.config/pay/config.toml",
			wantLimit:  30,
			wantLimSrc: "user:/home/u/.config/pay/config.toml",
		},
		{
			name: "built-in default is the floor",
			peel: func(in *Input) {
				in.Flags = Flags{}
				in.Env = Env{}
				in.Project = nil
				in.User = nil
			},
			wantURL:    "",
			wantURLSrc: SourceDefault,
			wantLimit:  DefaultLimit,
			wantLimSrc: SourceDefault,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := fullStack()
			tc.peel(&in)
			got, err := Resolve(in)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.BaseURL != tc.wantURL {
				t.Errorf("base_url = %q, want %q", got.BaseURL, tc.wantURL)
			}
			if src := got.Sources["base_url"]; src != tc.wantURLSrc {
				t.Errorf("base_url source = %q, want %q", src, tc.wantURLSrc)
			}
			if got.Limit != tc.wantLimit {
				t.Errorf("limit = %d, want %d", got.Limit, tc.wantLimit)
			}
			if src := got.Sources["limit"]; src != tc.wantLimSrc {
				t.Errorf("limit source = %q, want %q", src, tc.wantLimSrc)
			}
		})
	}
}

// TestProfileSelection covers the other chain of §4.5.
func TestProfileSelection(t *testing.T) {
	project := &File{Path: "/repo/pay.toml", Kind: KindProject, DefaultProfile: "proj",
		Profiles: map[string]Profile{"proj": {}, "flagged": {}, "envd": {}}}
	user := &File{Path: "/u/config.toml", Kind: KindUser, DefaultProfile: "usr",
		Profiles: map[string]Profile{"usr": {}, "proj": {}}}

	tests := []struct {
		name       string
		in         Input
		want       string
		wantSource string
	}{
		{
			name:       "flag",
			in:         Input{Flags: Flags{Profile: "flagged"}, Env: Env{"PAY_PROFILE": "envd"}, Project: project, User: user},
			want:       "flagged",
			wantSource: SourceFlag,
		},
		{
			name:       "env",
			in:         Input{Env: Env{"PAY_PROFILE": "envd"}, Project: project, User: user},
			want:       "envd",
			wantSource: "env:PAY_PROFILE",
		},
		{
			name:       "project default_profile",
			in:         Input{Project: project, User: user},
			want:       "proj",
			wantSource: "project:/repo/pay.toml",
		},
		{
			name:       "user default_profile",
			in:         Input{User: user},
			want:       "usr",
			wantSource: "user:/u/config.toml",
		},
		{
			name: "sole profile",
			in: Input{User: &File{Path: "/u/config.toml", Kind: KindUser,
				Profiles: map[string]Profile{"only": {BaseURL: "http://x"}}}},
			want:       "only",
			wantSource: "sole-profile",
		},
		{
			name:       "literal default",
			in:         Input{},
			want:       DefaultProfileName,
			wantSource: SourceDefault,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(tc.in)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.Profile != tc.want {
				t.Errorf("profile = %q, want %q", got.Profile, tc.want)
			}
			if src := got.Sources["profile"]; src != tc.wantSource {
				t.Errorf("profile source = %q, want %q", src, tc.wantSource)
			}
		})
	}
}

func TestUnknownProfile(t *testing.T) {
	user := &File{Path: "/u/config.toml", Kind: KindUser,
		Profiles: map[string]Profile{"local": {BaseURL: "http://localhost:3900"}, "staging": {}}}

	_, err := Resolve(Input{Flags: Flags{Profile: "locl"}, User: user})
	if !apierr.HasCode(err, apierr.CodeProfileUnknown) {
		t.Fatalf("err = %v, want profile_unknown", err)
	}
	e := apierr.From(err)
	if len(e.DidYouMean) == 0 || e.DidYouMean[0] != "local" {
		t.Errorf("did_you_mean = %v, want [local …]", e.DidYouMean)
	}

	// An unknown profile plus an explicit base_url is `pay auth login` and
	// must not fail.
	got, err := Resolve(Input{Flags: Flags{Profile: "brand-new", BaseURL: "http://x.test"}, User: user})
	if err != nil {
		t.Fatalf("Resolve with explicit base_url: %v", err)
	}
	if got.ProfileDefined {
		t.Error("ProfileDefined = true for a profile that exists nowhere")
	}
}

func TestGraphQLPathDerivation(t *testing.T) {
	tests := []struct {
		name       string
		profile    Profile
		flags      Flags
		want       string
		wantSource string
	}{
		{
			name:       "derived from defaults",
			profile:    Profile{BaseURL: "http://x.test"},
			want:       "/api/graphql",
			wantSource: SourceDerivedGraphQL,
		},
		{
			name:       "derived from a custom api_path",
			profile:    Profile{BaseURL: "http://x.test", APIPath: "/cms-api"},
			want:       "/cms-api/graphql",
			wantSource: SourceDerivedGraphQL,
		},
		{
			name:       "derived from a custom graphql_route",
			profile:    Profile{BaseURL: "http://x.test", APIPath: "/cms-api", GraphQLRoute: "/gql"},
			want:       "/cms-api/gql",
			wantSource: SourceDerivedGraphQL,
		},
		{
			name:       "explicit profile key overrides",
			profile:    Profile{BaseURL: "http://x.test", APIPath: "/cms-api", GraphQLPath: "/graphql"},
			want:       "/graphql",
			wantSource: "user:/u/config.toml",
		},
		{
			name:       "flag overrides",
			profile:    Profile{BaseURL: "http://x.test"},
			flags:      Flags{GraphQLPath: "/elsewhere"},
			want:       "/elsewhere",
			wantSource: SourceFlag,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := Input{
				Flags: tc.flags,
				User: &File{Path: "/u/config.toml", Kind: KindUser,
					Profiles: map[string]Profile{"default": tc.profile}},
			}
			got, err := Resolve(in)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.GraphQLPath != tc.want {
				t.Errorf("graphql_path = %q, want %q", got.GraphQLPath, tc.want)
			}
			if src := got.Sources["graphql_path"]; src != tc.wantSource {
				t.Errorf("source = %q, want %q", src, tc.wantSource)
			}
		})
	}
}

func TestBaseURLUserinfoIsLifted(t *testing.T) {
	got, err := Resolve(Input{Flags: Flags{BaseURL: "https://user:pa55@cms.example.com/base/"}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.BaseURL != "https://cms.example.com/base" {
		t.Errorf("base_url = %q, want the userinfo-free form", got.BaseURL)
	}
	user, pass, ok := got.BasicAuth()
	if !ok || user != "user" || pass != "pa55" {
		t.Errorf("BasicAuth() = %q,%q,%v", user, pass, ok)
	}
	if len(got.Warnings) == 0 {
		t.Error("want a warning naming where the credential moved")
	}
	for _, w := range got.Warnings {
		if strings.Contains(w, "pa55") {
			t.Fatal("the password leaked into a warning")
		}
	}
	// Explain must not carry the secret either.
	for key, setting := range got.Explain() {
		if s, ok := setting.Value.(string); ok && strings.Contains(s, "pa55") {
			t.Fatalf("Explain()[%q] leaked the password", key)
		}
	}
}

func TestBaseURLInvalid(t *testing.T) {
	for _, raw := range []string{"cms.example.com", "ftp://cms.example.com", "http://", "http://x/?a=b"} {
		if _, err := Resolve(Input{Flags: Flags{BaseURL: raw}}); !apierr.HasCode(err, apierr.CodeBaseURLInvalid) {
			t.Errorf("Resolve(%q) err = %v, want base_url_invalid", raw, err)
		}
	}
}

func TestNormURL(t *testing.T) {
	// api_path "/" means "the API is at the site root" and normalises to "",
	// which is §7.2's empty candidate.
	tests := []struct{ base, apiPath, want string }{
		{"http://LocalHost:3900", "/api", "http://localhost:3900/api"},
		{"https://cms.example.com:443", "/api", "https://cms.example.com/api"},
		{"http://cms.example.com:80", "/", "http://cms.example.com"},
		{"https://cms.example.com/base", "cms-api/", "https://cms.example.com/base/cms-api"},
	}
	for _, tc := range tests {
		r, err := Resolve(Input{Flags: Flags{BaseURL: tc.base, APIPath: tc.apiPath}})
		if err != nil {
			t.Fatalf("Resolve(%q): %v", tc.base, err)
		}
		if got := r.NormURL(); got != tc.want {
			t.Errorf("NormURL(%q,%q) = %q, want %q", tc.base, tc.apiPath, got, tc.want)
		}
	}
}

func TestDefaultsAreTheSpecDefaults(t *testing.T) {
	got, err := Resolve(Input{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"api_path", got.APIPath, DefaultAPIPath},
		{"graphql_route", got.GraphQLRoute, DefaultGraphQLRoute},
		{"auth_collection", got.AuthCollection, DefaultAuthCollection},
		{"auth_mode", got.AuthMode, AuthModeAuto},
		{"auth_header_scheme", got.AuthHeaderScheme, "JWT"},
		{"output", got.Output, DefaultOutput},
		{"depth", got.Depth, 0},
		{"limit", got.Limit, 20},
		{"timeout", got.Timeout, 30 * time.Second},
		{"deadline", got.Deadline, 120 * time.Second},
		{"max_retries", got.MaxRetries, 3},
		{"concurrency", got.Concurrency, 8},
		{"redact", got.Redact, true},
		{"accept_language", got.AcceptLanguage, "en"},
		{"max_bulk", got.MaxBulk, 100},
		{"max_docs", got.MaxDocs, 100},
		{"confirm_writes", got.ConfirmWrites, false},
		{"discovery_ttl", got.DiscoveryTTL, 24 * time.Hour},
		{"access_ttl", got.AccessTTL, 10 * time.Minute},
		{"schema_ttl", got.SchemaTTL, 24 * time.Hour},
		{"identity_ttl", got.IdentityTTL, 60 * time.Second},
		{"skills_ttl", got.SkillsTTL, 6 * time.Hour},
		{"log_level", got.LogLevel, "info"},
		{"log_format", got.LogFormat, "text"},
		{"update_channel", got.UpdateChannel, "stable"},
		{"keyring", got.KeyringMode, KeyringOff},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("default %s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestRedactToggleAndNegatedEnv(t *testing.T) {
	got, err := Resolve(Input{Env: Env{"PAY_NO_REDACT": "1"}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Redact {
		t.Error("PAY_NO_REDACT=1 must disable redaction")
	}
	if src := got.Sources["redact"]; src != "env:PAY_NO_REDACT" {
		t.Errorf("source = %q", src)
	}

	got, err = Resolve(Input{Flags: Flags{Redact: boolp(true)}, Env: Env{"PAY_NO_REDACT": "1"}})
	if err != nil || !got.Redact {
		t.Errorf("an explicit flag must beat the env var: redact=%v err=%v", got.Redact, err)
	}
}

func TestConcurrencyIsClamped(t *testing.T) {
	got, err := Resolve(Input{Flags: Flags{Concurrency: intp(500)}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Concurrency != MaxConcurrency || !got.ConcurrencyClamped {
		t.Errorf("concurrency = %d clamped=%v, want %d/true", got.Concurrency, got.ConcurrencyClamped, MaxConcurrency)
	}
	if len(got.Warnings) == 0 {
		t.Error("clamping must warn")
	}
}

func TestInvalidValues(t *testing.T) {
	tests := []struct {
		name string
		in   Input
		code apierr.Code
	}{
		{"auth mode", Input{Flags: Flags{AuthMode: "apikey"}}, apierr.CodeInvalidOption},
		{"auth header scheme", Input{Flags: Flags{AuthHeaderScheme: "Token"}}, apierr.CodeInvalidOption},
		{"timeout", Input{Flags: Flags{Timeout: "30 seconds"}}, apierr.CodeInvalidOption},
		{"limit env", Input{Env: Env{"PAY_LIMIT": "many"}}, apierr.CodeInvalidOption},
		{"bool env", Input{Env: Env{"PAY_NO_CACHE": "perhaps"}}, apierr.CodeInvalidOption},
		{"keyring", Input{Env: Env{"PAY_KEYRING": "sometimes"}}, apierr.CodeInvalidOption},
		{"id_type", Input{User: &File{Path: "/u", Kind: KindUser,
			Profiles: map[string]Profile{"default": {IDType: "uuid"}}}}, apierr.CodeInvalidOption},
		{"db_adapter", Input{User: &File{Path: "/u", Kind: KindUser,
			Profiles: map[string]Profile{"default": {DBAdapter: "mysql"}}}}, apierr.CodeInvalidOption},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Resolve(tc.in); !apierr.HasCode(err, tc.code) {
				t.Errorf("err = %v, want %s", err, tc.code)
			}
		})
	}
}

func TestHeadersMergeAndInterpolate(t *testing.T) {
	in := Input{
		Env: Env{"VERCEL_BYPASS": "s3cr3t"},
		Project: &File{Path: "/repo/pay.toml", Kind: KindProject,
			Profiles: map[string]Profile{"default": {Headers: map[string]string{"X-A": "project"}}}},
		User: &File{Path: "/u/config.toml", Kind: KindUser,
			Profiles: map[string]Profile{"default": {Headers: map[string]string{
				"X-A": "user", "X-Bypass": "${VERCEL_BYPASS}", "X-Missing": "${NOPE}"}}}},
	}
	got, err := Resolve(in)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Headers["X-A"] != "project" {
		t.Errorf("project layer must win: %q", got.Headers["X-A"])
	}
	if got.Headers["X-Bypass"] != "s3cr3t" {
		t.Errorf("interpolation failed: %q", got.Headers["X-Bypass"])
	}
	if got.Headers["X-Missing"] != "" {
		t.Errorf("an unset variable must expand to empty, got %q", got.Headers["X-Missing"])
	}
	if len(got.Warnings) == 0 {
		t.Error("an unset ${NOPE} must warn")
	}
	names := got.HeaderNames()
	if len(names) != 3 || names[0] != "X-A" {
		t.Errorf("HeaderNames() = %v", names)
	}
	// Explain must mask the values but keep the names.
	explained, ok := got.Explain()["headers"].Value.(map[string]string)
	if !ok {
		t.Fatal("Explain() has no headers entry")
	}
	for name, value := range explained {
		if value == "s3cr3t" {
			t.Fatalf("Explain leaked the value of %s", name)
		}
	}
}

func TestRequireBaseURL(t *testing.T) {
	got, _ := Resolve(Input{})
	if err := got.RequireBaseURL(); !apierr.HasCode(err, apierr.CodeConfigMissing) {
		t.Errorf("err = %v, want config_missing", err)
	}
	got, _ = Resolve(Input{Flags: Flags{BaseURL: "http://x.test"}})
	if err := got.RequireBaseURL(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestResolveIsPure(t *testing.T) {
	in := fullStack()
	first, err := Resolve(in)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	second, err := Resolve(in)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if first.BaseURL != second.BaseURL || first.Limit != second.Limit {
		t.Error("Resolve is not deterministic")
	}
	if len(first.Sources) != len(second.Sources) {
		t.Error("source map differs between runs")
	}
}

// TestZeroBlastRadiusCapsAreRejected is finding 11's config half: max_bulk = 0
// used to be accepted and then silently became DefaultMaxBulk (100) — the
// opposite of what was written, on the one setting whose whole job is to be a
// limit.
func TestZeroBlastRadiusCapsAreRejected(t *testing.T) {
	// max_bulk is the only settable cap; the resolved max_docs is derived from
	// it, so rejecting max_bulk covers both.
	zero := 0
	if _, err := Resolve(Input{
		Flags: Flags{Profile: "local"},
		User: &File{Path: "/u/config.toml", Kind: KindUser,
			Defaults: Defaults{MaxBulk: &zero},
			Profiles: map[string]Profile{"local": {BaseURL: "http://localhost:3900"}}},
	}); !apierr.HasCode(err, apierr.CodeInvalidOption) {
		t.Fatalf("max_bulk = 0 err = %v, want invalid_option: a zero cap is "+
			"silently reinterpreted as the default 100", err)
	}

	// A POSITIVE cap still resolves, so the new rule cannot swallow a valid one.
	one := 1
	got, err := Resolve(Input{
		Flags: Flags{Profile: "local"},
		User: &File{Path: "/u/config.toml", Kind: KindUser,
			Defaults: Defaults{MaxBulk: &one},
			Profiles: map[string]Profile{"local": {BaseURL: "http://localhost:3900"}}},
	})
	if err != nil {
		t.Fatalf("max_bulk = 1: %v", err)
	}
	if got.MaxBulk != 1 {
		t.Errorf("MaxBulk = %d, want 1", got.MaxBulk)
	}
}
