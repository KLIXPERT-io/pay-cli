package secret

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"runtime"
	"sort"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/fsatomic"
)

// FilePerm is credentials.json's mandatory mode (§4.4).
const FilePerm fs.FileMode = 0o600

// CredentialsVersion is the schema version of the file.
const CredentialsVersion = 1

// Record is one profile's stored credential (§4.4).
//
// There is deliberately no password field and never will be: `pay auth login
// --jwt` keeps only the minted token and its expiry (§5.0).
type Record struct {
	AuthMode Mode   `json:"auth_mode"`
	APIKey   string `json:"api_key,omitempty"`
	// Keyring is true when the real secret lives in the OS keychain and this
	// record is only the pointer to it (§5.1 step 8, §5.2).
	Keyring         bool       `json:"keyring,omitempty"`
	Token           string     `json:"token,omitempty"`
	TokenExp        *time.Time `json:"token_exp,omitempty"`
	LoginIdentifier string     `json:"login_identifier,omitempty"`
	LoginField      string     `json:"login_field,omitempty"`
	Fingerprint     string     `json:"fingerprint,omitempty"`
	UpdatedAt       *time.Time `json:"updated_at,omitempty"`
}

// JWT returns the record's token as a JWT record.
func (r Record) JWT() JWT {
	j := JWT{Token: r.Token}
	if r.TokenExp != nil {
		j.Exp = *r.TokenExp
	}
	return j
}

// Credentials is the whole file.
type Credentials struct {
	Version  int                `json:"version"`
	Profiles map[string]*Record `json:"profiles"`
}

// FileStore is §5.2's default storage target: credentials.json, 0600, written
// atomically.
type FileStore struct {
	Path string
}

// NewFileStore binds a store to a path (config.Paths.CredentialsFile()).
func NewFileStore(path string) *FileStore { return &FileStore{Path: path} }

// Load reads the file. A missing file is not an error — it is the state of
// every fresh install.
//
// Before reading a single byte it enforces §5.1 step 7: a group- or
// world-readable credentials file is refused, not quietly used.
func (s *FileStore) Load() (*Credentials, error) {
	empty := &Credentials{Version: CredentialsVersion, Profiles: map[string]*Record{}}
	if s == nil || s.Path == "" {
		return empty, nil
	}
	if err := CheckPerm(s.Path); err != nil {
		return empty, err
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return empty, nil
		}
		return empty, apierr.Wrap(err, apierr.CodeAuthMissing, "cannot read %s", s.Path)
	}
	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return empty, apierr.Wrap(err, apierr.CodeAuthInvalid,
			"%s is not valid JSON", s.Path).
			WithHint("delete it and run pay auth login again")
	}
	if creds.Profiles == nil {
		creds.Profiles = map[string]*Record{}
	}
	if creds.Version == 0 {
		creds.Version = CredentialsVersion
	}
	return &creds, nil
}

// Get returns the record for one profile.
func (s *FileStore) Get(profile string) (*Record, bool, error) {
	creds, err := s.Load()
	if err != nil {
		return nil, false, err
	}
	rec, ok := creds.Profiles[profile]
	if !ok || rec == nil {
		return nil, false, nil
	}
	return rec, true, nil
}

// Put stores (or replaces) a profile's record, stamping the fingerprint and
// updated_at. now is a parameter because §3.1 forbids time.Now outside app.go.
func (s *FileStore) Put(profile string, rec *Record, now time.Time) error {
	if s == nil || s.Path == "" {
		return apierr.New(apierr.CodeInternal, "secret: no credentials path configured")
	}
	if rec == nil {
		return apierr.New(apierr.CodeInternal, "secret: nil credential record")
	}
	creds, err := s.Load()
	if err != nil {
		return err
	}
	stamped := *rec
	stamped.Fingerprint = recordFingerprint(&stamped)
	stamp := now.UTC()
	stamped.UpdatedAt = &stamp
	creds.Profiles[profile] = &stamped
	return s.save(creds)
}

// Delete removes a profile's record. It reports whether anything was removed
// so `pay auth logout` can say so.
func (s *FileStore) Delete(profile string) (bool, error) {
	creds, err := s.Load()
	if err != nil {
		return false, err
	}
	if _, ok := creds.Profiles[profile]; !ok {
		return false, nil
	}
	delete(creds.Profiles, profile)
	return true, s.save(creds)
}

// Rename moves a record between profile names (`pay auth rename`).
func (s *FileStore) Rename(oldName, newName string) (bool, error) {
	creds, err := s.Load()
	if err != nil {
		return false, err
	}
	rec, ok := creds.Profiles[oldName]
	if !ok {
		return false, nil
	}
	delete(creds.Profiles, oldName)
	creds.Profiles[newName] = rec
	return true, s.save(creds)
}

// Profiles lists the stored profile names, sorted.
func (s *FileStore) Profiles() ([]string, error) {
	creds, err := s.Load()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(creds.Profiles))
	for name := range creds.Profiles {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func (s *FileStore) save(creds *Credentials) error {
	creds.Version = CredentialsVersion
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return apierr.Wrap(err, apierr.CodeInternal, "cannot encode credentials")
	}
	data = append(data, '\n')
	if err := fsatomic.Write(s.Path, data, FilePerm); err != nil {
		return apierr.Wrap(err, apierr.CodeInternal, "cannot write %s", s.Path)
	}
	return nil
}

// FixPerms implements `pay auth fix-perms`: chmod 0600 the credentials file
// and 0700 its directory.
func (s *FileStore) FixPerms() error {
	if s == nil || s.Path == "" {
		return apierr.New(apierr.CodeInternal, "secret: no credentials path configured")
	}
	if _, err := os.Stat(s.Path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return apierr.Wrap(err, apierr.CodeAuthMissing, "cannot stat %s", s.Path)
	}
	if err := os.Chmod(s.Path, FilePerm); err != nil {
		return apierr.Wrap(err, apierr.CodeAuthInsecurePermissions, "cannot chmod %s", s.Path)
	}
	return nil
}

// CheckPerm enforces §5.1's "refuses to read if the file mode is group/world
// readable" for credentials.json and for --api-key-file.
//
// The check is skipped on Windows, where Unix permission bits are synthesised
// by the Go runtime and would report a false positive on every file.
func CheckPerm(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return apierr.Wrap(err, apierr.CodeAuthMissing, "cannot stat %s", path)
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		return apierr.New(apierr.CodeAuthInsecurePermissions,
			"%s is mode %04o; a credential file must not be readable by group or others", path, mode).
			WithHint("pay auth fix-perms")
	}
	return nil
}

// recordFingerprint derives §4.4's fingerprint from whichever credential the
// record actually carries. A keyring-backed record has no local secret to
// hash, so its fingerprint is filled in by the caller that knows the value.
func recordFingerprint(rec *Record) string {
	switch {
	case rec.AuthMode == ModeAnonymous:
		return Anon
	case rec.APIKey != "":
		return Fingerprint(rec.APIKey)
	case rec.Token != "":
		return Fingerprint(rec.Token)
	default:
		return rec.Fingerprint
	}
}
