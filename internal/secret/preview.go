package secret

import (
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// EnvPreviewSecret is F9's environment variable for the site's draft-preview
// secret. Like PAY_API_KEY it has a per-profile form, PAY_PREVIEW_SECRET_<P>.
//
//nolint:gosec // G101: an environment variable NAME, not a secret value.
const EnvPreviewSecret = "PAY_PREVIEW_SECRET"

// Preview-secret source strings. They name a location, never a value.
const (
	SourcePreviewEnvVar   = "env:preview_secret_env"
	SourcePreviewCredFile = "file:credentials.json (preview_secret)"
)

// PreviewSecretEnvNames lists the variables in resolution order: the
// profile-specific one first, then the general one (the §5.1 steps 4-5 order).
func PreviewSecretEnvNames(profile string) []string {
	if suffix := EnvSuffix(profile); suffix != "" {
		return []string{EnvPreviewSecret + "_" + suffix, EnvPreviewSecret}
	}
	return []string{EnvPreviewSecret}
}

// PreviewInput is everything the preview-secret chain may consider.
type PreviewInput struct {
	Profile string
	Env     Env
	// SecretEnv is the profile's preview_secret_env: the NAME of the variable
	// the site itself reads (e.g. PREVIEW_SECRET), so an agent can reuse the
	// app's own .env without copying the value anywhere.
	SecretEnv string
	Store     *Store
}

// PreviewSecret is a resolved preview secret. Value must never be printed,
// logged or cached; Fingerprint (the §4.4 formula) is the only identity that
// may leave the process.
type PreviewSecret struct {
	Value       string
	Source      string
	Fingerprint string
}

// ResolvePreviewSecret walks F9's chain, first hit wins:
//
//  1. $PAY_PREVIEW_SECRET_<PROFILE_UPPER_SNAKE>
//  2. $PAY_PREVIEW_SECRET
//  3. the variable named by the profile's preview_secret_env
//  4. the profile's credentials.json record (`pay auth preview-secret --stdin`)
//
// Nothing resolved is (nil, nil): whether that is an error depends on whether
// the preview_path template needs a {secret} at all, which only the caller
// knows. The OS keychain is not consulted — the preview secret is stored in
// credentials.json (0600) only.
func ResolvePreviewSecret(in PreviewInput) (*PreviewSecret, error) {
	for _, name := range PreviewSecretEnvNames(in.Profile) {
		if v, ok := lookup(in.Env, name); ok {
			return previewSecret(v, "env:"+name), nil
		}
	}
	if in.SecretEnv != "" {
		if v, ok := lookup(in.Env, in.SecretEnv); ok {
			return previewSecret(v, "env:"+in.SecretEnv), nil
		}
	}
	if in.Store == nil || in.Store.File == nil {
		return nil, nil
	}
	rec, ok, err := in.Store.File.Get(in.Profile)
	if err != nil {
		return nil, err
	}
	if ok && rec != nil && rec.PreviewSecret != "" {
		return previewSecret(rec.PreviewSecret, SourcePreviewCredFile), nil
	}
	return nil, nil
}

func previewSecret(value, source string) *PreviewSecret {
	return &PreviewSecret{Value: value, Source: source, Fingerprint: Fingerprint(value)}
}

// SetPreviewSecret stores a profile's preview secret in credentials.json,
// creating the record when the profile has no API credential yet. The API
// credential, if any, is left untouched.
func (s *FileStore) SetPreviewSecret(profile, value string, now time.Time) error {
	if s == nil || s.Path == "" {
		return apierr.New(apierr.CodeInternal, "secret: no credentials path configured")
	}
	if value == "" {
		return apierr.New(apierr.CodeInvalidArgs, "the preview secret is empty").
			WithHint("pipe it in: printf %%s \"$PREVIEW_SECRET\" | pay auth preview-secret --stdin")
	}
	creds, err := s.Load()
	if err != nil {
		return err
	}
	rec := creds.Profiles[profile]
	if rec == nil {
		rec = &Record{}
		creds.Profiles[profile] = rec
	}
	rec.PreviewSecret = value
	stamp := now.UTC()
	rec.UpdatedAt = &stamp
	return s.save(creds)
}

// ClearPreviewSecret removes a profile's stored preview secret and reports
// whether there was one. A record left with no credential at all is removed
// entirely, so `pay auth list` does not show an empty entry.
func (s *FileStore) ClearPreviewSecret(profile string) (bool, error) {
	creds, err := s.Load()
	if err != nil {
		return false, err
	}
	rec := creds.Profiles[profile]
	if rec == nil || rec.PreviewSecret == "" {
		return false, nil
	}
	rec.PreviewSecret = ""
	if !rec.HasAPICredential() && rec.AuthMode != ModeAnonymous {
		delete(creds.Profiles, profile)
	}
	return true, s.save(creds)
}
