package secret

import (
	"bufio"
	"context"
	"io"
	"os"
	"strings"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// Source strings recorded by each step of §5.1, for `pay config explain` and
// `pay auth status`. They name a location, never a value.
const (
	SourceStdin       = "flag:--api-key-stdin"
	SourceKeyFile     = "flag:--api-key-file"
	SourceFlag        = "flag:--api-key"
	SourceProfileEnv  = "env:api_key_env"
	SourceCredFile    = "file:credentials.json"
	SourceKeyringName = "keyring"
	SourceHelper      = "credential_helper"
	SourceStoredJWT   = "file:credentials.json (jwt)"
	SourceNone        = "none"
)

// Flags are the credential-bearing root flags of §9.1.
type Flags struct {
	APIKey      string
	APIKeyStdin bool
	APIKeyFile  string
	// Stdin is where --api-key-stdin reads from. A nil reader means standard
	// input is not available, which makes the flag an error rather than a hang.
	Stdin io.Reader
	// IsTTY drives the §5.1 step-3 warning.
	IsTTY bool
}

// Input is everything the chain may consider. Everything that would otherwise
// be ambient — the environment, the clock, stdin — is a field, so §5.1 is a
// table test.
type Input struct {
	Profile string
	BaseURL string
	Env     Env
	Flags   Flags
	Mode    Mode
	// APIKeyEnv is the profile's api_key_env (§5.1 step 6).
	APIKeyEnv string
	// Helper is the profile's credential_helper (§5.1 step 9).
	Helper *Helper
	Store  *Store
	Now    time.Time
}

// Credential is the outcome of the chain. Value is the secret itself and must
// never be logged, printed, cached or put in an error; Fingerprint is the only
// identity that may leave the process (§5.3).
type Credential struct {
	Mode        Mode
	Value       string
	Source      string
	Fingerprint string

	// Exp, LoginIdentifier and LoginField are set in jwt mode (§5.0).
	Exp             time.Time
	LoginIdentifier string
	LoginField      string

	// Warnings are non-fatal observations: an --api-key typed on a TTY, a
	// keychain that could not be read while a usable fallback existed.
	Warnings []string
}

// Anonymous reports whether the command will run without an Authorization
// header (§5.0).
func (c *Credential) Anonymous() bool { return c == nil || c.Mode == ModeAnonymous }

// Resolve walks §5.1's ten steps, first hit wins.
//
// Two shortcuts precede the walk, both from §5.1's closing paragraph:
// --auth-mode anonymous short-circuits the whole chain and sends no credential
// even when one is available; an explicit api-key or jwt mode restricts the
// chain to that mode's sources, so a stale token cannot satisfy --auth-mode
// api-key.
//
// Step 11 is the important one: when nothing resolves and the mode is auto or
// anonymous, this returns an anonymous credential and no error. Refusing to
// run without a credential would make PayCLI useless against a project with
// public read access.
func Resolve(ctx context.Context, in Input) (*Credential, error) {
	if in.Mode == ModeAnonymous {
		return anonymous(SourceNone), nil
	}
	wantKey := in.Mode == ModeAuto || in.Mode == ModeAPIKey || in.Mode == ""
	wantJWT := in.Mode == ModeAuto || in.Mode == ModeJWT || in.Mode == ""

	var warnings []string

	if wantKey {
		// 1. --api-key-stdin
		if in.Flags.APIKeyStdin {
			key, err := readStdinLine(in.Flags.Stdin)
			if err != nil {
				return nil, err
			}
			if key != "" {
				return apiKey(key, SourceStdin, warnings), nil
			}
			return nil, apierr.New(apierr.CodeAuthMissing,
				"--api-key-stdin was given but stdin was empty").
				WithHint("pipe the key: printf %%s \"$KEY\" | pay … --api-key-stdin")
		}

		// 2. --api-key-file
		if path := in.Flags.APIKeyFile; path != "" {
			key, err := readKeyFile(path)
			if err != nil {
				return nil, err
			}
			if key != "" {
				return apiKey(key, SourceKeyFile+" "+path, warnings), nil
			}
			return nil, apierr.New(apierr.CodeAuthMissing, "%s is empty", path)
		}

		// 3. --api-key
		if in.Flags.APIKey != "" {
			if in.Flags.IsTTY {
				warnings = append(warnings,
					"--api-key was passed on the command line; it is visible in ps output and shell "+
						"history. Prefer --api-key-stdin or PAY_API_KEY.")
			}
			return apiKey(in.Flags.APIKey, SourceFlag, warnings), nil
		}

		// 4 + 5. $PAY_API_KEY_<PROFILE> then $PAY_API_KEY
		for _, name := range APIKeyEnvNames(in.Profile) {
			if v, ok := lookup(in.Env, name); ok {
				return apiKey(v, "env:"+name, warnings), nil
			}
		}

		// 6. the env var named by the profile's api_key_env
		if in.APIKeyEnv != "" {
			if v, ok := lookup(in.Env, in.APIKeyEnv); ok {
				return apiKey(v, "env:"+in.APIKeyEnv, warnings), nil
			}
		}
	}

	// PAY_JWT / PAY_JWT_<PROFILE> force jwt mode (§5.1). They sit after the
	// API-key environment variables and before stored credentials, so an
	// exported token beats a stale file entry but never beats an explicit key.
	if wantJWT {
		for _, name := range JWTEnvNames(in.Profile) {
			if v, ok := lookup(in.Env, name); ok {
				token, _ := ParseJWT(v)
				return &Credential{
					Mode:        ModeJWT,
					Value:       v,
					Source:      "env:" + name,
					Fingerprint: Fingerprint(v),
					Exp:         token.Exp,
					Warnings:    warnings,
				}, nil
			}
		}
	}

	// 7 + 8. credentials.json, then the OS keychain when the record opted in.
	rec, stored, err := in.Store.Lookup(ctx, in.Profile)
	if err != nil {
		// An unreadable or insecure credentials file is fatal: the user asked
		// for a stored credential and PayCLI must not pretend it is absent.
		// A keychain that cannot be reached is only a warning, unless
		// PAY_KEYRING=force made the keychain the sole source of truth (§5.2).
		if apierr.HasCode(err, apierr.CodeAuthInsecurePermissions) ||
			apierr.HasCode(err, apierr.CodeAuthInvalid) ||
			in.Store.KeyringMode() == KeyringForce {
			return nil, err
		}
		warnings = append(warnings, apierr.From(err).Message)
	}
	if rec != nil && stored != "" && wantKey && rec.AuthMode == ModeAPIKey {
		source := SourceCredFile
		if rec.Keyring {
			source = SourceKeyringName
		}
		return apiKey(stored, source, warnings), nil
	}
	if wantKey {
		if value, found, kerr := in.Store.KeyringOnly(ctx, in.Profile); kerr == nil && found {
			return apiKey(value, SourceKeyringName, warnings), nil
		}
	}

	// 9. credential_helper
	if wantKey && in.Helper.Configured() {
		value, herr := in.Helper.Resolve(ctx)
		if herr != nil {
			return nil, herr
		}
		if value != "" {
			return apiKey(value, SourceHelper, warnings), nil
		}
	}

	// 10. a stored, unexpired JWT
	if wantJWT && rec != nil && rec.AuthMode == ModeJWT {
		token := rec.JWT()
		if stored != "" {
			token.Token = stored
		}
		if token.Usable(in.Now) {
			source := SourceStoredJWT
			if rec.Keyring {
				source = SourceKeyringName
			}
			return &Credential{
				Mode:            ModeJWT,
				Value:           token.Token,
				Source:          source,
				Fingerprint:     Fingerprint(token.Token),
				Exp:             token.Exp,
				LoginIdentifier: rec.LoginIdentifier,
				LoginField:      rec.LoginField,
				Warnings:        warnings,
			}, nil
		}
		if token.Token != "" {
			warnings = append(warnings,
				"the stored token for profile \""+in.Profile+"\" has expired (or expires within 60s)")
		}
	}

	// 11. nothing resolved.
	switch in.Mode {
	case ModeAPIKey:
		return nil, apierr.New(apierr.CodeAuthMissing,
			"no API key resolved for profile %q", in.Profile).
			WithHint("pay auth login --profile %s --base-url %s", in.Profile, loginURL(in.BaseURL))
	case ModeJWT:
		expired := rec != nil && rec.AuthMode == ModeJWT && rec.Token != ""
		e := apierr.New(apierr.CodeAuthMissing,
			"no usable token resolved for profile %q", in.Profile)
		if expired {
			e = e.Recode(apierr.CodeAuthInvalid)
		}
		return nil, e.WithHint(
			"pay auth login --jwt --profile %s --base-url %s --email you@example.com --password-stdin",
			in.Profile, loginURL(in.BaseURL))
	default:
		cred := anonymous(SourceNone)
		cred.Warnings = warnings
		return cred, nil
	}
}

func loginURL(base string) string {
	if base == "" {
		return "<url>"
	}
	return base
}

func apiKey(value, source string, warnings []string) *Credential {
	return &Credential{
		Mode:        ModeAPIKey,
		Value:       value,
		Source:      source,
		Fingerprint: Fingerprint(value),
		Warnings:    warnings,
	}
}

func anonymous(source string) *Credential {
	return &Credential{Mode: ModeAnonymous, Source: source, Fingerprint: Anon}
}

func lookup(env Env, name string) (string, bool) {
	if env == nil || name == "" {
		return "", false
	}
	v, ok := env.Lookup(name)
	if !ok {
		return "", false
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return "", false
	}
	return v, true
}

// readStdinLine implements §5.1 step 1: one line, trimmed. It reads a single
// line rather than draining stdin, so a command that also streams a body from
// stdin is not broken by the flag.
func readStdinLine(r io.Reader) (string, error) {
	if r == nil {
		return "", apierr.New(apierr.CodeAuthMissing,
			"--api-key-stdin was given but no stdin is attached")
	}
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", apierr.Wrap(err, apierr.CodeAuthMissing, "cannot read the key from stdin")
	}
	return strings.TrimSpace(line), nil
}

// readKeyFile implements §5.1 step 2, including its refusal to read a
// group/world-readable file.
func readKeyFile(path string) (string, error) {
	if _, err := os.Stat(path); err != nil {
		return "", apierr.Wrap(err, apierr.CodeFileMissing, "cannot read --api-key-file %s", path)
	}
	if err := CheckPerm(path); err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", apierr.Wrap(err, apierr.CodeFileMissing, "cannot read --api-key-file %s", path)
	}
	text := string(data)
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	return strings.TrimSpace(text), nil
}
