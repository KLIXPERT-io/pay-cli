package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// AuthMode is §5.0's three-way auth selector plus "auto".
type AuthMode string

// Auth modes. "auto" is a *configuration* value only: it never survives
// credential resolution, which always lands on one of the other three (§5.1).
const (
	AuthModeAuto      AuthMode = "auto"
	AuthModeAPIKey    AuthMode = "api-key"
	AuthModeJWT       AuthMode = "jwt"
	AuthModeAnonymous AuthMode = "anonymous"
)

// AuthModes is the accepted set, in help order.
var AuthModes = []AuthMode{AuthModeAuto, AuthModeAPIKey, AuthModeJWT, AuthModeAnonymous}

// ParseAuthMode validates --auth-mode / PAY_AUTH_MODE / the profile key.
func ParseAuthMode(s string) (AuthMode, error) {
	switch AuthMode(strings.ToLower(strings.TrimSpace(s))) {
	case "":
		return AuthModeAuto, nil
	case AuthModeAuto:
		return AuthModeAuto, nil
	case AuthModeAPIKey:
		return AuthModeAPIKey, nil
	case AuthModeJWT:
		return AuthModeJWT, nil
	case AuthModeAnonymous:
		return AuthModeAnonymous, nil
	}
	names := make([]string, 0, len(AuthModes))
	for _, m := range AuthModes {
		names = append(names, string(m))
	}
	return "", apierr.New(apierr.CodeInvalidOption,
		"unknown auth mode %q; valid modes are %s", s, strings.Join(names, ", ")).
		WithDidYouMean(apierr.DidYouMean(s, names)...)
}

// String satisfies fmt.Stringer.
func (m AuthMode) String() string { return string(m) }

// KeyringMode is PAY_KEYRING (§5.2).
type KeyringMode string

// Keyring modes.
const (
	KeyringAuto  KeyringMode = "auto"
	KeyringOff   KeyringMode = "off"
	KeyringForce KeyringMode = "force"
)

// ParseKeyringMode validates PAY_KEYRING. The default is "off": §5.1 step 8
// reads the keychain only when the profile opted in or PAY_KEYRING=force, so
// an unset variable must not make the chain touch dbus/wincred. "auto" only
// affects *writes* (§5.2).
func ParseKeyringMode(s string) (KeyringMode, error) {
	switch KeyringMode(strings.ToLower(strings.TrimSpace(s))) {
	case "":
		return KeyringOff, nil
	case KeyringAuto:
		return KeyringAuto, nil
	case KeyringOff:
		return KeyringOff, nil
	case KeyringForce:
		return KeyringForce, nil
	}
	return "", apierr.New(apierr.CodeInvalidOption,
		"unknown PAY_KEYRING value %q; valid values are auto, off, force", s).
		WithDidYouMean(apierr.DidYouMean(s, []string{"auto", "off", "force"})...)
}

// DBAdapter values (§7.11).
const (
	DBPostgres = "postgres"
	DBMongoDB  = "mongodb"
	DBSQLite   = "sqlite"
	DBUnknown  = "unknown"
)

// Provenance strings shared by §7.10/§7.11 and `pay config explain`.
const (
	SourceConfigured    = "configured"
	SourceProjectPkg    = "project-package-json"
	SourceProjectSource = "project-source"
	SourceInferred      = "inferred"
	SourceObserved      = "observed"
	SourceUnknown       = "unknown"
)

// Profile is one [profiles.X] table (§4.2).
//
// Every optional scalar is a pointer or an empty-able string so that "unset"
// and "set to the zero value" stay distinguishable through §4.5's chain. The
// APIKey field exists only so that a config.toml carrying the forbidden key
// decodes instead of landing in the undecoded set: its presence is a hard
// error (config_secret_in_plaintext).
type Profile struct {
	BaseURL            string              `toml:"base_url,omitempty"`
	APIPath            string              `toml:"api_path,omitempty"`
	GraphQLRoute       string              `toml:"graphql_route,omitempty"`
	GraphQLPath        string              `toml:"graphql_path,omitempty"`
	AuthCollection     string              `toml:"auth_collection,omitempty"`
	AuthMode           string              `toml:"auth_mode,omitempty"`
	AuthHeaderScheme   string              `toml:"auth_header_scheme,omitempty"`
	Label              string              `toml:"label,omitempty"`
	APIKeyEnv          string              `toml:"api_key_env,omitempty"`
	CredentialHelper   string              `toml:"credential_helper,omitempty"`
	InsecureSkipVerify *bool               `toml:"insecure_skip_verify,omitempty"`
	Headers            map[string]string   `toml:"headers,omitempty"`
	Blocks             map[string][]string `toml:"blocks,omitempty"`

	// Optional pins. Each one turns a discovered-or-unknown value into a
	// configured one and flips the matching *_source to "configured" (§4.2).
	IDType          string   `toml:"id_type,omitempty"`
	Locales         []string `toml:"locales,omitempty"`
	PayloadVersion  string   `toml:"payload_version,omitempty"`
	DBAdapter       string   `toml:"db_adapter,omitempty"`
	EchoCheckIgnore []string `toml:"echo_check_ignore,omitempty"`
	CustomEndpoints []string `toml:"custom_endpoints,omitempty"`

	// APIKey is never a legal key (§4.2); see File.Validate.
	APIKey string `toml:"api_key,omitempty"`
}

// Defaults is the [defaults] table (§4.2). Pointers everywhere: `depth = 0`
// and `redact = false` are meaningful settings, not absence.
type Defaults struct {
	Output         string `toml:"output,omitempty"`
	Depth          *int   `toml:"depth,omitempty"`
	Limit          *int   `toml:"limit,omitempty"`
	Timeout        string `toml:"timeout,omitempty"`
	Deadline       string `toml:"deadline,omitempty"`
	MaxRetries     *int   `toml:"max_retries,omitempty"`
	Concurrency    *int   `toml:"concurrency,omitempty"`
	Redact         *bool  `toml:"redact,omitempty"`
	AcceptLanguage string `toml:"accept_language,omitempty"`
	MaxBulk        *int   `toml:"max_bulk,omitempty"`
	ConfirmWrites  *bool  `toml:"confirm_writes,omitempty"`
	Locale         string `toml:"locale,omitempty"`
	FallbackLocale string `toml:"fallback_locale,omitempty"`
	ErrorsTo       string `toml:"errors_to,omitempty"`
}

// CacheSection is the [cache] table (§4.2).
type CacheSection struct {
	Dir          string `toml:"dir,omitempty"`
	DiscoveryTTL string `toml:"discovery_ttl,omitempty"`
	AccessTTL    string `toml:"access_ttl,omitempty"`
	SchemaTTL    string `toml:"schema_ttl,omitempty"`
	IdentityTTL  string `toml:"identity_ttl,omitempty"`
	SkillsTTL    string `toml:"skills_ttl,omitempty"`
}

// LoggingSection is the [logging] table (§4.2).
type LoggingSection struct {
	Level  string `toml:"level,omitempty"`
	Format string `toml:"format,omitempty"`
}

// UpdateSection is the [update] table (§4.2).
type UpdateSection struct {
	Auto    *bool  `toml:"auto,omitempty"`
	Channel string `toml:"channel,omitempty"`
}

// ProfileNames returns the union of profile names across the layers, sorted,
// for did-you-mean and `pay auth list`.
func ProfileNames(files ...*File) []string {
	seen := map[string]bool{}
	for _, f := range files {
		if f == nil {
			continue
		}
		for name := range f.Profiles {
			seen[name] = true
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// unknownProfile builds the profile_unknown error with the candidate list.
func unknownProfile(name string, candidates []string) *apierr.Error {
	msg := fmt.Sprintf("profile %q is not defined in any config file", name)
	if len(candidates) == 0 {
		return apierr.New(apierr.CodeProfileUnknown, "%s", msg).
			WithHint("no profiles exist yet: pay auth login --profile %s --base-url <url>", name)
	}
	return apierr.New(apierr.CodeProfileUnknown, "%s; known profiles: %s", msg, strings.Join(candidates, ", ")).
		WithDidYouMean(apierr.DidYouMean(name, candidates)...)
}
