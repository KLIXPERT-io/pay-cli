package secret

import (
	"context"
	"strings"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// Mode is §5.0's resolved auth mode. The string values match
// config.AuthMode and apierr's AuthMode* constants exactly; the type is
// re-declared here so internal/secret depends on neither package for its own
// vocabulary.
type Mode string

// Auth modes.
const (
	ModeAuto      Mode = "auto"
	ModeAPIKey    Mode = "api-key"
	ModeJWT       Mode = "jwt"
	ModeAnonymous Mode = "anonymous"
)

// ParseMode normalises a mode string; anything unrecognised becomes auto,
// because the configuration layer has already rejected invalid values.
func ParseMode(s string) Mode {
	switch Mode(strings.ToLower(strings.TrimSpace(s))) {
	case ModeAPIKey:
		return ModeAPIKey
	case ModeJWT:
		return ModeJWT
	case ModeAnonymous:
		return ModeAnonymous
	default:
		return ModeAuto
	}
}

// Storage targets reported by SaveResult.
const (
	TargetFile    = "file"
	TargetKeyring = "keyring"
)

// SaveResult says where a credential actually landed. The caller prints it:
// §5.2's rule is that the user must always know where the secret went.
type SaveResult struct {
	Target      string
	Path        string
	Fingerprint string
	// Warning is set when the keychain was tried and the file was used
	// instead (PAY_KEYRING=auto only).
	Warning string
}

// Store is §5.2's storage policy: credentials.json first, the OS keychain only
// on explicit opt-in.
type Store struct {
	File    *FileStore
	Keyring *Keyring
}

// NewStore binds the default file store and a keychain in the given mode.
func NewStore(credentialsPath string, mode KeyringMode) *Store {
	return &Store{File: NewFileStore(credentialsPath), Keyring: NewKeyring(mode)}
}

// Path is the credentials file path, for messages.
func (s *Store) Path() string {
	if s == nil || s.File == nil {
		return ""
	}
	return s.File.Path
}

// KeyringMode reports the configured keychain policy (§5.2).
func (s *Store) KeyringMode() KeyringMode {
	if s == nil || s.Keyring == nil {
		return KeyringOff
	}
	return s.Keyring.Mode
}

// Lookup returns a profile's stored record together with the secret it points
// at, resolving a "keyring": true record through the keychain (§5.1 steps 7
// and 8).
//
// The keychain is consulted only when the record opted in or the mode is
// force, exactly as §5.1 step 8 requires.
func (s *Store) Lookup(ctx context.Context, profile string) (*Record, string, error) {
	if s == nil || s.File == nil {
		return nil, "", nil
	}
	rec, ok, err := s.File.Get(profile)
	if err != nil || !ok {
		return nil, "", err
	}
	forced := s.Keyring != nil && s.Keyring.Mode == KeyringForce
	if rec.Keyring || forced {
		value, found, kerr := s.Keyring.Get(ctx, profile)
		if kerr != nil {
			if forced {
				return rec, "", kerr
			}
			return rec, localSecret(rec), kerr
		}
		if found {
			return rec, value, nil
		}
	}
	return rec, localSecret(rec), nil
}

// KeyringOnly reads a keychain entry for a profile that has no file record at
// all, which is what PAY_KEYRING=force means for a machine that never wrote
// credentials.json.
func (s *Store) KeyringOnly(ctx context.Context, profile string) (string, bool, error) {
	if s == nil || s.Keyring == nil || s.Keyring.Mode != KeyringForce {
		return "", false, nil
	}
	return s.Keyring.Get(ctx, profile)
}

func localSecret(rec *Record) string {
	if rec == nil {
		return ""
	}
	if rec.APIKey != "" {
		return rec.APIKey
	}
	return rec.Token
}

// SaveAPIKey stores an API key for a profile (§5.2).
func (s *Store) SaveAPIKey(ctx context.Context, profile, key string, useKeyring bool, now time.Time) (SaveResult, error) {
	rec := &Record{AuthMode: ModeAPIKey, APIKey: key, Fingerprint: Fingerprint(key)}
	return s.save(ctx, profile, rec, key, useKeyring, now)
}

// SaveJWT stores a minted token and its expiry — never the password (§5.0).
func (s *Store) SaveJWT(ctx context.Context, profile string, token JWT, identifier, field string,
	useKeyring bool, now time.Time) (SaveResult, error) {
	rec := &Record{
		AuthMode:        ModeJWT,
		Token:           token.Token,
		LoginIdentifier: identifier,
		LoginField:      field,
		Fingerprint:     Fingerprint(token.Token),
	}
	if !token.Exp.IsZero() {
		exp := token.Exp.UTC()
		rec.TokenExp = &exp
	}
	return s.save(ctx, profile, rec, token.Token, useKeyring, now)
}

// save applies §5.2's policy:
//
//	--keyring or PAY_KEYRING=force  -> keychain, and a failure is fatal
//	PAY_KEYRING=auto                -> keychain, falling back to the file with
//	                                   a warning naming the path
//	otherwise                       -> credentials.json (0600)
func (s *Store) save(ctx context.Context, profile string, rec *Record, secret string,
	useKeyring bool, now time.Time) (SaveResult, error) {
	if s == nil || s.File == nil {
		return SaveResult{}, apierr.New(apierr.CodeInternal, "secret: no store configured")
	}
	mode := KeyringOff
	if s.Keyring != nil {
		mode = s.Keyring.Mode
	}
	wantKeyring := useKeyring || mode == KeyringForce || mode == KeyringAuto
	mandatory := useKeyring || mode == KeyringForce

	result := SaveResult{Target: TargetFile, Path: s.File.Path, Fingerprint: Fingerprint(secret)}

	if wantKeyring {
		kr := s.Keyring
		if kr == nil || kr.Mode == KeyringOff {
			kr = NewKeyring(KeyringAuto)
		}
		err := kr.Set(ctx, profile, secret)
		switch {
		case err == nil:
			pointer := *rec
			pointer.Keyring = true
			pointer.APIKey = ""
			pointer.Token = ""
			result.Target = TargetKeyring
			if perr := s.File.Put(profile, &pointer, now); perr != nil {
				return result, perr
			}
			return result, nil
		case mandatory:
			// Never a silent fallback: the user asked for the keychain.
			return result, err
		default:
			result.Warning = "OS keychain unavailable; the credential was written to " +
				s.File.Path + " (mode 0600) instead"
		}
	}

	if err := s.File.Put(profile, rec, now); err != nil {
		return result, err
	}
	return result, nil
}

// Logout removes a profile's credential from both backends.
func (s *Store) Logout(ctx context.Context, profile string) (bool, error) {
	if s == nil || s.File == nil {
		return false, nil
	}
	rec, ok, err := s.File.Get(profile)
	if err != nil {
		return false, err
	}
	var kerr error
	if ok && rec.Keyring || (s.Keyring != nil && s.Keyring.Mode == KeyringForce) {
		kerr = s.Keyring.Delete(ctx, profile)
	}
	removed, derr := s.File.Delete(profile)
	if derr != nil {
		return removed, derr
	}
	return removed, kerr
}
