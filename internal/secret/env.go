package secret

import "strings"

// Env is the environment snapshot the chain reads. config.Env satisfies it,
// and so does any test map: the interface exists so internal/secret does not
// depend on internal/config, and so §4.5's "set but empty counts as unset"
// rule is the caller's single implementation.
type Env interface {
	Lookup(name string) (string, bool)
}

// mapEnv is a trivial Env for callers that have a plain map (tests, and
// `pay doctor`'s hypotheticals).
type mapEnv map[string]string

func (m mapEnv) Lookup(name string) (string, bool) {
	v, ok := m[name]
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

// MapEnv adapts a plain map to Env.
func MapEnv(m map[string]string) Env { return mapEnv(m) }

// EnvSuffix renders a profile name as the UPPER_SNAKE suffix of §4.6's
// PAY_API_KEY_<PROFILE> and PAY_JWT_<PROFILE> variables. Every character that
// cannot appear in an environment variable name becomes an underscore, and a
// leading digit is prefixed with one, so any profile name a user can type maps
// to a legal variable.
func EnvSuffix(profile string) string {
	if profile == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(profile))
	for i := 0; i < len(profile); i++ {
		c := profile[i]
		switch {
		case c >= 'a' && c <= 'z':
			b.WriteByte(c - 32)
		case (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9'):
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out[0] >= '0' && out[0] <= '9' {
		return "_" + out
	}
	return out
}

// Environment variable names of §4.6.
//
//nolint:gosec // G101: these are environment variable NAMES, not secret values.
const (
	EnvAPIKey = "PAY_API_KEY"
	EnvJWT    = "PAY_JWT"
)

// APIKeyEnvNames lists the API-key variables in §5.1 order: the
// profile-specific one first, then the general one.
func APIKeyEnvNames(profile string) []string {
	if suffix := EnvSuffix(profile); suffix != "" {
		return []string{EnvAPIKey + "_" + suffix, EnvAPIKey}
	}
	return []string{EnvAPIKey}
}

// JWTEnvNames is APIKeyEnvNames for PAY_JWT.
func JWTEnvNames(profile string) []string {
	if suffix := EnvSuffix(profile); suffix != "" {
		return []string{EnvJWT + "_" + suffix, EnvJWT}
	}
	return []string{EnvJWT}
}
