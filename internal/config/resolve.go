package config

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

// Built-in defaults (§4.2). These are the last link of §4.5's chain.
const (
	DefaultProfileName      = "default"
	DefaultAPIPath          = "/api"
	DefaultGraphQLRoute     = "/graphql"
	DefaultAuthCollection   = "auto"
	DefaultAuthHeaderScheme = "JWT"
	DefaultOutput           = "json"
	DefaultErrorsTo         = "stdout"
	DefaultDepth            = 0
	DefaultLimit            = 20
	DefaultMaxRetries       = 3
	DefaultConcurrency      = 8
	DefaultMaxBulk          = 100
	DefaultAcceptLanguage   = "en"
	DefaultLogLevel         = "info"
	DefaultLogFormat        = "text"
	DefaultUpdateChannel    = "stable"

	// MaxConcurrency is §9.1's hard cap. PayCLI is the only backpressure a
	// Payload 3.x server has.
	MaxConcurrency = 32
)

// Default durations (§4.2).
const (
	DefaultTimeout      = 30 * time.Second
	DefaultDeadline     = 120 * time.Second
	DefaultDiscoveryTTL = 24 * time.Hour
	DefaultAccessTTL    = 10 * time.Minute
	DefaultSchemaTTL    = 24 * time.Hour
	DefaultIdentityTTL  = 60 * time.Second
	DefaultSkillsTTL    = 6 * time.Hour
)

// Source strings that are not a layer name.
const (
	SourceFlag    = "flag"
	SourceDefault = "default"
	// SourceDerivedGraphQL is §4.2's visible coupling between api_path and
	// graphql_route.
	SourceDerivedGraphQL = "derived:api_path+graphql_route"
)

// Flags carries §9.1's root persistent flags. Every field is empty/nil unless
// the flag was actually changed, which is how "flag" wins §4.5's first slot
// without a spurious zero value beating a config file.
type Flags struct {
	Profile          string
	BaseURL          string
	APIPath          string
	GraphQLPath      string
	GraphQLRoute     string
	AuthCollection   string
	AuthMode         string
	AuthHeaderScheme string

	Output         string
	ErrorsTo       string
	Path           string
	Locale         string
	FallbackLocale string
	AcceptLanguage string

	Timeout  string
	Deadline string
	CacheTTL string

	Depth       *int
	Limit       *int
	MaxDocs     *int
	MaxRetries  *int
	Concurrency *int

	Redact             *bool
	InsecureSkipVerify *bool
	NoCache            *bool
	Refresh            *bool
	Yes                *bool
	DryRun             *bool
	NoAudit            *bool
	Quiet              *bool
	Verbose            *bool

	LogLevel  string
	LogFormat string
}

// Input is everything Resolve is allowed to see. No file handles, no clock, no
// environment access: the caller loads the layers, Resolve combines them
// (§3.1).
type Input struct {
	Flags   Flags
	Env     Env
	Project *File
	User    *File
}

// Resolved is the effective configuration for one command invocation.
//
// basicAuth is deliberately unexported and has no JSON/TOML tag anywhere: it
// holds a credential lifted out of base_url (§4.2) and must never be
// serialised with the rest of the struct.
type Resolved struct {
	Profile        string
	ProfileDefined bool
	Label          string

	BaseURL          string
	APIPath          string
	GraphQLRoute     string
	GraphQLPath      string
	AuthCollection   string
	AuthMode         AuthMode
	AuthHeaderScheme string
	APIKeyEnv        string
	CredentialHelper string
	Headers          map[string]string
	KeyringMode      KeyringMode

	Output         string
	ErrorsTo       string
	Path           string
	Locale         string
	FallbackLocale string
	AcceptLanguage string

	Timeout     time.Duration
	Deadline    time.Duration
	Depth       int
	Limit       int
	MaxDocs     int
	MaxRetries  int
	Concurrency int
	MaxBulk     int

	Redact             bool
	ConfirmWrites      bool
	InsecureSkipVerify bool
	NoCache            bool
	Refresh            bool
	CacheTTL           time.Duration
	Yes                bool
	DryRun             bool
	NoAudit            bool
	Quiet              bool
	Verbose            bool

	CacheDir     string
	DiscoveryTTL time.Duration
	AccessTTL    time.Duration
	SchemaTTL    time.Duration
	IdentityTTL  time.Duration
	SkillsTTL    time.Duration

	LogLevel  string
	LogFormat string

	UpdateAuto    bool
	UpdateChannel string

	// Optional pins (§4.2). An empty value means "not configured"; the
	// matching *Source entry says so explicitly.
	IDType          string
	Locales         []string
	PayloadVersion  string
	DBAdapter       string
	EchoCheckIgnore []string
	CustomEndpoints []string
	Blocks          map[string][]string

	// ConcurrencyClamped is true when the requested concurrency exceeded
	// MaxConcurrency and was lowered.
	ConcurrencyClamped bool
	// Warnings are non-fatal configuration observations for the caller to put
	// in the envelope (§10): a moved base_url credential, an unset ${ENV} in a
	// header, an unknown key in a config file.
	Warnings []string
	// Sources maps a setting name to its §4.5 provenance string.
	Sources map[string]string

	basicAuth *url.Userinfo
}

// BasicAuth returns the credential that was carried in base_url, if any
// (§4.2). The transport applies it as an Authorization: Basic header, where it
// is redacted like every other credential.
func (r *Resolved) BasicAuth() (username, password string, ok bool) {
	if r == nil || r.basicAuth == nil {
		return "", "", false
	}
	pw, _ := r.basicAuth.Password()
	return r.basicAuth.Username(), pw, true
}

// Setting is one row of `pay config explain --output json` (§4.5).
type Setting struct {
	Value  any    `json:"value"`
	Source string `json:"source"`
}

// Resolve combines the layers of §4.5. It is pure: given the same Input it
// returns the same Resolved, and it never touches the filesystem, the clock or
// the environment beyond the supplied snapshot.
func Resolve(in Input) (*Resolved, error) {
	r := &res{in: in, out: &Resolved{Sources: map[string]string{}}}
	r.resolveProfile()
	r.resolveConnection()
	r.resolveAuth()
	r.resolveOutput()
	r.resolveLimits()
	r.resolveToggles()
	r.resolveCache()
	r.resolveLoggingAndUpdate()
	r.resolvePins()
	r.collectFileWarnings()
	if r.err != nil {
		return nil, r.err
	}
	return r.out, nil
}

// res is the accumulator Resolve threads through the per-group helpers. The
// first error wins and later groups become no-ops, so a bad --timeout does not
// hide behind a bad --limit.
type res struct {
	in  Input
	out *Resolved
	err error

	projectProfile *Profile
	userProfile    *Profile
}

func (r *res) fail(err error) {
	if r.err == nil && err != nil {
		r.err = err
	}
}

// ---------------------------------------------------------------- profile ---

// resolveProfile implements §4.5's profile-selection chain:
// --profile -> PAY_PROFILE -> project default_profile -> user default_profile
// -> the sole profile if exactly one exists -> "default".
func (r *res) resolveProfile() {
	name, source := "", ""
	explicit := false
	switch {
	case r.in.Flags.Profile != "":
		name, source, explicit = r.in.Flags.Profile, SourceFlag, true
	default:
		if v, ok := r.in.Env.Lookup("PAY_PROFILE"); ok {
			name, source, explicit = v, "env:PAY_PROFILE", true
		} else if p := r.in.Project; p != nil && p.DefaultProfile != "" {
			name, source = p.DefaultProfile, p.Source()
		} else if u := r.in.User; u != nil && u.DefaultProfile != "" {
			name, source = u.DefaultProfile, u.Source()
		} else if names := ProfileNames(r.in.Project, r.in.User); len(names) == 1 {
			name, source = names[0], "sole-profile"
		} else {
			name, source = DefaultProfileName, SourceDefault
		}
	}

	r.out.Profile = name
	r.out.Sources["profile"] = source

	pp, inProject := r.in.Project.Profile(name)
	up, inUser := r.in.User.Profile(name)
	if inProject {
		r.projectProfile = &pp
	}
	if inUser {
		r.userProfile = &up
	}
	r.out.ProfileDefined = inProject || inUser

	// An explicitly named profile that exists nowhere is a typo *unless* the
	// caller supplied a base_url out of band — which is exactly what
	// `pay auth login --profile new --base-url …` does, and failing that
	// would make a profile impossible to create.
	if explicit && !r.out.ProfileDefined && !r.hasExplicitBaseURL() {
		r.fail(unknownProfile(name, ProfileNames(r.in.Project, r.in.User)))
	}

	r.out.Label = r.profileString("label", nil, func(p *Profile) string { return p.Label }, "")
}

func (r *res) hasExplicitBaseURL() bool {
	if r.in.Flags.BaseURL != "" {
		return true
	}
	_, ok := r.in.Env.Lookup("PAY_BASE_URL")
	return ok
}

// ------------------------------------------------------------- connection ---

func (r *res) resolveConnection() {
	raw := r.profileString("base_url", []string{"PAY_BASE_URL"},
		func(p *Profile) string { return p.BaseURL }, "")
	if raw != "" {
		normalised, userinfo, err := splitBaseURL(raw)
		if err != nil {
			r.fail(err)
		} else {
			r.out.BaseURL = normalised
			r.out.basicAuth = userinfo
			if userinfo != nil {
				r.out.Warnings = append(r.out.Warnings, fmt.Sprintf(
					"base_url for profile %q carried embedded credentials; they were moved out of the URL "+
						"and will be sent as an Authorization: Basic header (§4.2)", r.out.Profile))
			}
		}
	}

	r.out.APIPath = normalisePath(r.profileString("api_path", []string{"PAY_API_PATH"},
		func(p *Profile) string { return p.APIPath }, DefaultAPIPath))
	r.out.GraphQLRoute = normalisePath(r.profileString("graphql_route", []string{"PAY_GRAPHQL_ROUTE"},
		func(p *Profile) string { return p.GraphQLRoute }, DefaultGraphQLRoute))

	// graphql_path is derived, not defaulted (§4.2). Only an explicit setting
	// overrides {api_path}{graphql_route}.
	explicit := r.profileString("graphql_path", []string{"PAY_GRAPHQL_PATH"},
		func(p *Profile) string { return p.GraphQLPath }, "")
	if explicit != "" {
		r.out.GraphQLPath = normalisePath(explicit)
	} else {
		r.out.GraphQLPath = r.out.APIPath + r.out.GraphQLRoute
		r.out.Sources["graphql_path"] = SourceDerivedGraphQL
	}

	headers, missing := r.resolveHeaders()
	r.out.Headers = headers
	for _, name := range missing {
		r.out.Warnings = append(r.out.Warnings, fmt.Sprintf(
			"header interpolation: ${%s} is unset, the header was sent empty", name))
	}
}

// resolveHeaders merges [profiles.X.headers] across layers (project wins per
// key, matching §4.5's ordering) and interpolates ${ENV} (§4.2, §8.1).
func (r *res) resolveHeaders() (map[string]string, []string) {
	merged := map[string]string{}
	for _, p := range []*Profile{r.userProfile, r.projectProfile} { // later wins
		if p == nil {
			continue
		}
		for k, v := range p.Headers {
			merged[k] = v
		}
	}
	if len(merged) == 0 {
		return nil, nil
	}
	out, missing := InterpolateMap(merged, r.in.Env)
	if len(out) > 0 {
		r.out.Sources["headers"] = r.profileLayerSource(func(p *Profile) bool { return len(p.Headers) > 0 })
	}
	return out, missing
}

// ------------------------------------------------------------------- auth ---

func (r *res) resolveAuth() {
	r.out.AuthCollection = r.profileString("auth_collection", []string{"PAY_AUTH_COLLECTION"},
		func(p *Profile) string { return p.AuthCollection }, DefaultAuthCollection)

	modeStr := r.profileString("auth_mode", []string{"PAY_AUTH_MODE"},
		func(p *Profile) string { return p.AuthMode }, string(AuthModeAuto))
	mode, err := ParseAuthMode(modeStr)
	if err != nil {
		r.fail(err)
	}
	r.out.AuthMode = mode

	scheme := r.profileString("auth_header_scheme", nil,
		func(p *Profile) string { return p.AuthHeaderScheme }, DefaultAuthHeaderScheme)
	switch strings.ToLower(scheme) {
	case "jwt":
		r.out.AuthHeaderScheme = "JWT"
	case "bearer":
		r.out.AuthHeaderScheme = "Bearer"
	default:
		r.fail(apierr.New(apierr.CodeInvalidOption,
			"unknown auth header scheme %q; valid schemes are JWT and Bearer", scheme).
			WithHint("Payload accepts both; PayCLI sends JWT by default because reverse proxies " +
				"often consume Bearer (§5.0)"))
	}

	r.out.APIKeyEnv = r.profileString("api_key_env", nil,
		func(p *Profile) string { return p.APIKeyEnv }, "")
	r.out.CredentialHelper = r.profileString("credential_helper", nil,
		func(p *Profile) string { return p.CredentialHelper }, "")

	keyring, err := ParseKeyringMode(r.in.Env.Get("PAY_KEYRING"))
	if err != nil {
		r.fail(err)
	}
	r.out.KeyringMode = keyring
	if r.in.Env.Get("PAY_KEYRING") != "" {
		r.out.Sources["keyring"] = "env:PAY_KEYRING"
	} else {
		r.out.Sources["keyring"] = SourceDefault
	}
}

// ----------------------------------------------------------------- output ---

func (r *res) resolveOutput() {
	// The format string is validated by internal/output, which owns the list;
	// duplicating it here would let the two drift.
	r.out.Output = r.defaultsString("output", []string{"PAY_OUTPUT"},
		r.in.Flags.Output, func(d *Defaults) string { return d.Output }, DefaultOutput)
	r.out.ErrorsTo = r.defaultsString("errors_to", []string{"PAY_ERRORS_TO"},
		r.in.Flags.ErrorsTo, func(d *Defaults) string { return d.ErrorsTo }, DefaultErrorsTo)
	r.out.Path = r.in.Flags.Path
	if r.out.Path != "" {
		r.out.Sources["path"] = SourceFlag
	}
	r.out.Locale = r.defaultsString("locale", nil, r.in.Flags.Locale,
		func(d *Defaults) string { return d.Locale }, "")
	r.out.FallbackLocale = r.defaultsString("fallback_locale", nil, r.in.Flags.FallbackLocale,
		func(d *Defaults) string { return d.FallbackLocale }, "")
	r.out.AcceptLanguage = r.defaultsString("accept_language", nil, r.in.Flags.AcceptLanguage,
		func(d *Defaults) string { return d.AcceptLanguage }, DefaultAcceptLanguage)
}

// ----------------------------------------------------------------- limits ---

func (r *res) resolveLimits() {
	r.out.Depth = r.defaultsInt("depth", []string{"PAY_DEPTH"}, r.in.Flags.Depth,
		func(d *Defaults) *int { return d.Depth }, DefaultDepth)
	r.out.Limit = r.defaultsInt("limit", []string{"PAY_LIMIT"}, r.in.Flags.Limit,
		func(d *Defaults) *int { return d.Limit }, DefaultLimit)
	r.out.MaxBulk = r.defaultsInt("max_bulk", nil, nil,
		func(d *Defaults) *int { return d.MaxBulk }, DefaultMaxBulk)
	// --max-docs defaults to max_bulk: §4.2 calls max_bulk "the default for
	// --max-docs", so the two are one setting with two names.
	r.out.MaxDocs = r.defaultsInt("max_docs", []string{"PAY_MAX_DOCS"}, r.in.Flags.MaxDocs,
		func(d *Defaults) *int { return d.MaxBulk }, r.out.MaxBulk)
	r.out.MaxRetries = r.defaultsInt("max_retries", []string{"PAY_MAX_RETRIES"}, r.in.Flags.MaxRetries,
		func(d *Defaults) *int { return d.MaxRetries }, DefaultMaxRetries)
	r.out.Concurrency = r.defaultsInt("concurrency", []string{"PAY_CONCURRENCY"}, r.in.Flags.Concurrency,
		func(d *Defaults) *int { return d.Concurrency }, DefaultConcurrency)

	if r.out.Concurrency > MaxConcurrency {
		r.out.ConcurrencyClamped = true
		r.out.Warnings = append(r.out.Warnings, fmt.Sprintf(
			"concurrency %d exceeds the hard cap of %d and was lowered (§9.1)",
			r.out.Concurrency, MaxConcurrency))
		r.out.Concurrency = MaxConcurrency
	}
	if r.out.Concurrency < 1 {
		r.out.Concurrency = 1
	}
	for key, v := range map[string]int{"depth": r.out.Depth, "limit": r.out.Limit,
		"max_retries": r.out.MaxRetries, "max_docs": r.out.MaxDocs, "max_bulk": r.out.MaxBulk} {
		if v < 0 {
			r.fail(apierr.New(apierr.CodeInvalidOption, "%s must not be negative, got %d", key, v))
		}
	}
	// The blast-radius caps are the config half of --max-docs' rule: both keys
	// are *int, so a resolved 0 can only come from someone explicitly writing
	// `max_bulk = 0`. That used to fall through to DefaultMaxBulk, quietly
	// turning "touch nothing" into "touch up to 100" — the opposite of what
	// was asked for, on the one setting whose entire job is to be a limit.
	for key, v := range map[string]int{"max_docs": r.out.MaxDocs, "max_bulk": r.out.MaxBulk} {
		if v == 0 {
			r.fail(apierr.New(apierr.CodeInvalidOption,
				"%s must be a positive number, got 0", key).
				WithHint("%s caps how many documents a bulk write may touch and must be >= 1; "+
					"there is no config value meaning \"no cap\" — pass --all on the command instead", key))
		}
	}

	r.out.Timeout = r.defaultsDuration("timeout", []string{"PAY_TIMEOUT"}, r.in.Flags.Timeout,
		func(d *Defaults) string { return d.Timeout }, DefaultTimeout)
	r.out.Deadline = r.defaultsDuration("deadline", []string{"PAY_DEADLINE"}, r.in.Flags.Deadline,
		func(d *Defaults) string { return d.Deadline }, DefaultDeadline)
}

// ---------------------------------------------------------------- toggles ---

func (r *res) resolveToggles() {
	r.out.Redact = r.defaultsBool("redact", []string{"PAY_NO_REDACT"}, r.in.Flags.Redact,
		func(d *Defaults) *bool { return d.Redact }, true, true)
	r.out.ConfirmWrites = r.defaultsBool("confirm_writes", nil, nil,
		func(d *Defaults) *bool { return d.ConfirmWrites }, false, false)

	r.out.InsecureSkipVerify = r.boolChain("insecure_skip_verify", []string{"PAY_INSECURE_SKIP_VERIFY"},
		r.in.Flags.InsecureSkipVerify, func(p *Profile) *bool { return p.InsecureSkipVerify }, false, false)

	r.out.NoCache = r.simpleBool("no_cache", "PAY_NO_CACHE", r.in.Flags.NoCache, false)
	r.out.Refresh = r.simpleBool("refresh", "PAY_REFRESH", r.in.Flags.Refresh, false)
	r.out.Yes = r.simpleBool("yes", "PAY_YES", r.in.Flags.Yes, false)
	r.out.NoAudit = r.simpleBool("no_audit", "PAY_NO_AUDIT", r.in.Flags.NoAudit, false)
	r.out.Quiet = r.simpleBool("quiet", "PAY_QUIET", r.in.Flags.Quiet, false)
	r.out.Verbose = r.simpleBool("verbose", "PAY_VERBOSE", r.in.Flags.Verbose, false)
	r.out.DryRun = r.simpleBool("dry_run", "", r.in.Flags.DryRun, false)
}

// ------------------------------------------------------------------ cache ---

func (r *res) resolveCache() {
	r.out.CacheDir = r.fileString("cache_dir", nil, "",
		func(f *File) string { return f.Cache.Dir }, "")
	r.out.DiscoveryTTL = r.fileDuration("discovery_ttl", nil, "",
		func(f *File) string { return f.Cache.DiscoveryTTL }, DefaultDiscoveryTTL)
	r.out.AccessTTL = r.fileDuration("access_ttl", nil, "",
		func(f *File) string { return f.Cache.AccessTTL }, DefaultAccessTTL)
	r.out.SchemaTTL = r.fileDuration("schema_ttl", nil, "",
		func(f *File) string { return f.Cache.SchemaTTL }, DefaultSchemaTTL)
	r.out.IdentityTTL = r.fileDuration("identity_ttl", nil, "",
		func(f *File) string { return f.Cache.IdentityTTL }, DefaultIdentityTTL)
	r.out.SkillsTTL = r.fileDuration("skills_ttl", nil, "",
		func(f *File) string { return f.Cache.SkillsTTL }, DefaultSkillsTTL)

	// --cache-ttl / PAY_CACHE_TTL overrides the discovery TTL for this run.
	if ttl := r.durationValue("cache_ttl", []string{"PAY_CACHE_TTL"}, r.in.Flags.CacheTTL, nil, 0); ttl > 0 {
		r.out.CacheTTL = ttl
		r.out.DiscoveryTTL = ttl
	}
}

func (r *res) resolveLoggingAndUpdate() {
	r.out.LogLevel = r.fileString("log_level", []string{"PAY_LOG_LEVEL"}, r.in.Flags.LogLevel,
		func(f *File) string { return f.Logging.Level }, DefaultLogLevel)
	r.out.LogFormat = r.fileString("log_format", []string{"PAY_LOG_FORMAT"}, r.in.Flags.LogFormat,
		func(f *File) string { return f.Logging.Format }, DefaultLogFormat)
	if r.out.Verbose && r.out.LogLevel == DefaultLogLevel && r.out.Sources["log_level"] == SourceDefault {
		r.out.LogLevel = "debug"
		r.out.Sources["log_level"] = "flag:--verbose"
	}

	auto := false
	source := SourceDefault
	for _, f := range r.fileLayers() {
		if f.Update.Auto != nil {
			auto, source = *f.Update.Auto, f.Source()
			break
		}
	}
	// §4.5: set but empty counts as unset.
	if r.in.Env.Get("PAY_NO_UPDATE") != "" {
		auto, source = false, "env:PAY_NO_UPDATE"
	}
	r.out.UpdateAuto = auto
	r.out.Sources["update_auto"] = source

	r.out.UpdateChannel = r.fileString("update_channel", nil, "",
		func(f *File) string { return f.Update.Channel }, DefaultUpdateChannel)
}

// ------------------------------------------------------------------- pins ---

func (r *res) resolvePins() {
	r.out.IDType = r.profileString("id_type", nil, func(p *Profile) string { return p.IDType }, "")
	if r.out.IDType != "" && r.out.IDType != "string" && r.out.IDType != "number" {
		r.fail(apierr.New(apierr.CodeInvalidOption,
			"profiles.%s.id_type must be \"string\" or \"number\", got %q", r.out.Profile, r.out.IDType))
	}
	r.out.PayloadVersion = r.profileString("payload_version", nil,
		func(p *Profile) string { return p.PayloadVersion }, "")
	r.out.DBAdapter = r.profileString("db_adapter", nil, func(p *Profile) string { return p.DBAdapter }, "")
	switch r.out.DBAdapter {
	case "", DBPostgres, DBMongoDB, DBSQLite:
	default:
		r.fail(apierr.New(apierr.CodeInvalidOption,
			"profiles.%s.db_adapter must be one of postgres, mongodb, sqlite; got %q",
			r.out.Profile, r.out.DBAdapter).
			WithDidYouMean(apierr.DidYouMean(r.out.DBAdapter, []string{DBPostgres, DBMongoDB, DBSQLite})...))
	}

	r.out.Locales = r.profileStrings("locales", func(p *Profile) []string { return p.Locales })
	r.out.EchoCheckIgnore = r.profileStrings("echo_check_ignore",
		func(p *Profile) []string { return p.EchoCheckIgnore })
	r.out.CustomEndpoints = r.profileStrings("custom_endpoints",
		func(p *Profile) []string { return p.CustomEndpoints })

	blocks := map[string][]string{}
	for _, p := range []*Profile{r.userProfile, r.projectProfile} { // later wins
		if p == nil {
			continue
		}
		for field, slugs := range p.Blocks {
			blocks[field] = append([]string(nil), slugs...)
		}
	}
	if len(blocks) > 0 {
		r.out.Blocks = blocks
		r.out.Sources["blocks"] = r.profileLayerSource(func(p *Profile) bool { return len(p.Blocks) > 0 })
	}
}

func (r *res) collectFileWarnings() {
	for _, f := range r.fileLayers() {
		for _, key := range f.UnknownKeys {
			r.out.Warnings = append(r.out.Warnings, fmt.Sprintf(
				"%s: unknown key %q was ignored", f.Path, key))
		}
	}
}

// ------------------------------------------------------------ chain steps ---

func (r *res) profileLayers() []*Profile {
	return []*Profile{r.projectProfile, r.userProfile}
}

func (r *res) profileLayerSource(match func(*Profile) bool) string {
	if r.projectProfile != nil && match(r.projectProfile) {
		return r.in.Project.Source()
	}
	if r.userProfile != nil && match(r.userProfile) {
		return r.in.User.Source()
	}
	return SourceDefault
}

func (r *res) layerSourceFor(p *Profile) string {
	if p == r.projectProfile {
		return r.in.Project.Source()
	}
	return r.in.User.Source()
}

func (r *res) fileLayers() []*File {
	out := make([]*File, 0, 2)
	if r.in.Project != nil {
		out = append(out, r.in.Project)
	}
	if r.in.User != nil {
		out = append(out, r.in.User)
	}
	return out
}

func (r *res) flagFor(key string) string {
	switch key {
	case "base_url":
		return r.in.Flags.BaseURL
	case "api_path":
		return r.in.Flags.APIPath
	case "graphql_path":
		return r.in.Flags.GraphQLPath
	case "graphql_route":
		return r.in.Flags.GraphQLRoute
	case "auth_collection":
		return r.in.Flags.AuthCollection
	case "auth_mode":
		return r.in.Flags.AuthMode
	case "auth_header_scheme":
		return r.in.Flags.AuthHeaderScheme
	}
	return ""
}

// profileString resolves a profile-level string through the full §4.5 chain.
func (r *res) profileString(key string, envNames []string, get func(*Profile) string, def string) string {
	if v := r.flagFor(key); v != "" {
		r.out.Sources[key] = SourceFlag
		return v
	}
	for _, name := range envNames {
		if v, ok := r.in.Env.Lookup(name); ok {
			r.out.Sources[key] = "env:" + name
			return v
		}
	}
	for _, p := range r.profileLayers() {
		if p == nil {
			continue
		}
		if v := get(p); v != "" {
			r.out.Sources[key] = r.layerSourceFor(p)
			return v
		}
	}
	r.out.Sources[key] = SourceDefault
	return def
}

func (r *res) profileStrings(key string, get func(*Profile) []string) []string {
	for _, p := range r.profileLayers() {
		if p == nil {
			continue
		}
		if v := get(p); len(v) > 0 {
			r.out.Sources[key] = r.layerSourceFor(p)
			return append([]string(nil), v...)
		}
	}
	r.out.Sources[key] = SourceDefault
	return nil
}

// defaultsString resolves a [defaults]-level string: flag -> env -> project
// [defaults] -> user [defaults] -> built-in.
func (r *res) defaultsString(key string, envNames []string, flagVal string,
	get func(*Defaults) string, def string) string {
	if flagVal != "" {
		r.out.Sources[key] = SourceFlag
		return flagVal
	}
	for _, name := range envNames {
		if v, ok := r.in.Env.Lookup(name); ok {
			r.out.Sources[key] = "env:" + name
			return v
		}
	}
	for _, f := range r.fileLayers() {
		if v := get(&f.Defaults); v != "" {
			r.out.Sources[key] = f.Source()
			return v
		}
	}
	r.out.Sources[key] = SourceDefault
	return def
}

func (r *res) defaultsInt(key string, envNames []string, flagVal *int,
	get func(*Defaults) *int, def int) int {
	if flagVal != nil {
		r.out.Sources[key] = SourceFlag
		return *flagVal
	}
	for _, name := range envNames {
		if v, ok := r.in.Env.Lookup(name); ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				r.fail(apierr.New(apierr.CodeInvalidOption, "%s=%q is not an integer", name, v))
				return def
			}
			r.out.Sources[key] = "env:" + name
			return n
		}
	}
	for _, f := range r.fileLayers() {
		if v := get(&f.Defaults); v != nil {
			r.out.Sources[key] = f.Source()
			return *v
		}
	}
	r.out.Sources[key] = SourceDefault
	return def
}

// defaultsBool resolves a tri-state bool. negatedEnv says the environment
// variable is a NO_ form (PAY_NO_REDACT=1 means redact=false).
func (r *res) defaultsBool(key string, envNames []string, flagVal *bool,
	get func(*Defaults) *bool, def bool, negatedEnv bool) bool {
	if flagVal != nil {
		r.out.Sources[key] = SourceFlag
		return *flagVal
	}
	for _, name := range envNames {
		if v, ok := r.in.Env.Lookup(name); ok {
			b, err := parseBool(name, v)
			if err != nil {
				r.fail(err)
				return def
			}
			r.out.Sources[key] = "env:" + name
			if negatedEnv {
				return !b
			}
			return b
		}
	}
	for _, f := range r.fileLayers() {
		if v := get(&f.Defaults); v != nil {
			r.out.Sources[key] = f.Source()
			return *v
		}
	}
	r.out.Sources[key] = SourceDefault
	return def
}

// boolChain is defaultsBool for a profile-level bool.
func (r *res) boolChain(key string, envNames []string, flagVal *bool,
	get func(*Profile) *bool, def bool, negatedEnv bool) bool {
	if flagVal != nil {
		r.out.Sources[key] = SourceFlag
		return *flagVal
	}
	for _, name := range envNames {
		if v, ok := r.in.Env.Lookup(name); ok {
			b, err := parseBool(name, v)
			if err != nil {
				r.fail(err)
				return def
			}
			r.out.Sources[key] = "env:" + name
			if negatedEnv {
				return !b
			}
			return b
		}
	}
	for _, p := range r.profileLayers() {
		if p == nil {
			continue
		}
		if v := get(p); v != nil {
			r.out.Sources[key] = r.layerSourceFor(p)
			return *v
		}
	}
	r.out.Sources[key] = SourceDefault
	return def
}

// simpleBool is a flag/env-only switch with no config-file home.
func (r *res) simpleBool(key, envName string, flagVal *bool, def bool) bool {
	if flagVal != nil {
		r.out.Sources[key] = SourceFlag
		return *flagVal
	}
	if envName != "" {
		if v, ok := r.in.Env.Lookup(envName); ok {
			b, err := parseBool(envName, v)
			if err != nil {
				r.fail(err)
				return def
			}
			r.out.Sources[key] = "env:" + envName
			return b
		}
	}
	r.out.Sources[key] = SourceDefault
	return def
}

func (r *res) fileString(key string, envNames []string, flagVal string,
	get func(*File) string, def string) string {
	if flagVal != "" {
		r.out.Sources[key] = SourceFlag
		return flagVal
	}
	for _, name := range envNames {
		if v, ok := r.in.Env.Lookup(name); ok {
			r.out.Sources[key] = "env:" + name
			return v
		}
	}
	for _, f := range r.fileLayers() {
		if v := get(f); v != "" {
			r.out.Sources[key] = f.Source()
			return v
		}
	}
	r.out.Sources[key] = SourceDefault
	return def
}

func (r *res) fileDuration(key string, envNames []string, flagVal string,
	get func(*File) string, def time.Duration) time.Duration {
	raw := r.fileString(key, envNames, flagVal, get, "")
	if raw == "" {
		r.out.Sources[key] = SourceDefault
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		r.fail(durationError(key, raw))
		return def
	}
	return d
}

func (r *res) defaultsDuration(key string, envNames []string, flagVal string,
	get func(*Defaults) string, def time.Duration) time.Duration {
	raw := r.defaultsString(key, envNames, flagVal, get, "")
	if raw == "" {
		r.out.Sources[key] = SourceDefault
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		r.fail(durationError(key, raw))
		return def
	}
	if d <= 0 {
		r.fail(apierr.New(apierr.CodeInvalidOption, "%s must be positive, got %q", key, raw))
		return def
	}
	return d
}

func (r *res) durationValue(key string, envNames []string, flagVal string,
	get func(*Defaults) string, def time.Duration) time.Duration {
	if get == nil {
		get = func(*Defaults) string { return "" }
	}
	return r.defaultsDuration(key, envNames, flagVal, get, def)
}

func durationError(key, raw string) error {
	return apierr.New(apierr.CodeInvalidOption,
		"%s=%q is not a duration; use Go syntax such as 30s, 2m, 1h30m", key, raw)
}

func parseBool(name, value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "t", "true", "y", "yes", "on":
		return true, nil
	case "0", "f", "false", "n", "no", "off":
		return false, nil
	}
	return false, apierr.New(apierr.CodeInvalidOption,
		"%s=%q is not a boolean; use 1/0, true/false, yes/no or on/off", name, value)
}

// ------------------------------------------------------------ url helpers ---

// splitBaseURL validates base_url and lifts any userinfo out of it (§4.2).
func splitBaseURL(raw string) (string, *url.Userinfo, error) {
	trimmed := strings.TrimSpace(raw)
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", nil, apierr.Wrap(err, apierr.CodeBaseURLInvalid,
			"base_url %q cannot be parsed as a URL", redact.URL(trimmed))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", nil, apierr.New(apierr.CodeBaseURLInvalid,
			"base_url %q must start with http:// or https://", redact.URL(trimmed))
	}
	if u.Host == "" {
		return "", nil, apierr.New(apierr.CodeBaseURLInvalid,
			"base_url %q has no host", redact.URL(trimmed))
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", nil, apierr.New(apierr.CodeBaseURLInvalid,
			"base_url %q must not carry a query string or fragment; use api_path for the route prefix",
			redact.URL(trimmed))
	}
	userinfo := u.User
	u.User = nil
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u.String(), userinfo, nil
}

// normalisePath forces a leading slash and drops a trailing one. An empty
// api_path is legal — §7.2 probes "" as a candidate — and stays empty.
func normalisePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimSuffix(p, "/")
}

// NormURL is §8.1's normalised URL: lowercase scheme and host, the port only
// when it is non-default, plus api_path. Userinfo is already gone by
// construction. It lives here because it is a pure function of resolved
// config, and internal/cache must not re-derive it differently.
func (r *Resolved) NormURL() string {
	u, err := url.Parse(r.BaseURL)
	if err != nil {
		return strings.ToLower(r.BaseURL) + r.APIPath
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	out := scheme + "://" + host
	if port != "" {
		out += ":" + port
	}
	out += strings.TrimSuffix(u.Path, "/")
	return out + r.APIPath
}

// HeaderNames returns the configured extra header names, sorted. Only names
// ever leave this process; values are secret (§5.3, §8.1).
func (r *Resolved) HeaderNames() []string {
	if len(r.Headers) == 0 {
		return []string{}
	}
	names := make([]string, 0, len(r.Headers))
	for name := range r.Headers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// RequireBaseURL is the check every command that talks to a server performs.
// Resolve itself tolerates an absent base_url so that `pay version`,
// `pay config paths` and `pay explain` still work on a bare machine.
func (r *Resolved) RequireBaseURL() error {
	if r.BaseURL != "" {
		return nil
	}
	return apierr.New(apierr.CodeConfigMissing,
		"no base_url is configured for profile %q", r.Profile).
		WithHint("pay auth login --profile %s --base-url http://localhost:3000, or set PAY_BASE_URL",
			r.Profile)
}

// Explain renders `pay config explain --output json` (§4.5). Header values are
// masked: the names are the only part that may be shown (§5.3).
func (r *Resolved) Explain() map[string]Setting {
	out := map[string]Setting{}
	add := func(key string, value any) {
		out[key] = Setting{Value: value, Source: r.source(key)}
	}
	add("profile", r.Profile)
	add("base_url", redact.URL(r.BaseURL))
	add("api_path", r.APIPath)
	add("graphql_route", r.GraphQLRoute)
	add("graphql_path", r.GraphQLPath)
	add("auth_collection", r.AuthCollection)
	add("auth_mode", string(r.AuthMode))
	add("auth_header_scheme", r.AuthHeaderScheme)
	add("api_key_env", r.APIKeyEnv)
	add("credential_helper", r.CredentialHelper)
	add("keyring", string(r.KeyringMode))
	add("label", r.Label)
	add("output", r.Output)
	add("errors_to", r.ErrorsTo)
	add("locale", r.Locale)
	add("fallback_locale", r.FallbackLocale)
	add("accept_language", r.AcceptLanguage)
	add("depth", r.Depth)
	add("limit", r.Limit)
	add("max_docs", r.MaxDocs)
	add("max_bulk", r.MaxBulk)
	add("max_retries", r.MaxRetries)
	add("concurrency", r.Concurrency)
	add("timeout", r.Timeout.String())
	add("deadline", r.Deadline.String())
	add("redact", r.Redact)
	add("confirm_writes", r.ConfirmWrites)
	add("insecure_skip_verify", r.InsecureSkipVerify)
	add("no_cache", r.NoCache)
	add("refresh", r.Refresh)
	add("cache_dir", r.CacheDir)
	add("discovery_ttl", r.DiscoveryTTL.String())
	add("access_ttl", r.AccessTTL.String())
	add("schema_ttl", r.SchemaTTL.String())
	add("identity_ttl", r.IdentityTTL.String())
	add("skills_ttl", r.SkillsTTL.String())
	add("log_level", r.LogLevel)
	add("log_format", r.LogFormat)
	add("update_auto", r.UpdateAuto)
	add("update_channel", r.UpdateChannel)
	add("id_type", r.IDType)
	add("locales", r.Locales)
	add("payload_version", r.PayloadVersion)
	add("db_adapter", r.DBAdapter)
	add("echo_check_ignore", r.EchoCheckIgnore)
	add("custom_endpoints", r.CustomEndpoints)
	add("blocks", r.Blocks)

	if len(r.Headers) > 0 {
		masked := make(map[string]string, len(r.Headers))
		for name := range r.Headers {
			masked[name] = redact.Mask
		}
		add("headers", masked)
	}
	if _, _, ok := r.BasicAuth(); ok {
		out["basic_auth"] = Setting{Value: redact.Mask, Source: "moved-from:base_url"}
	}
	return out
}

func (r *Resolved) source(key string) string {
	if s, ok := r.Sources[key]; ok && s != "" {
		return s
	}
	return SourceDefault
}
