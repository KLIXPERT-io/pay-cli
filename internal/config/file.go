package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/fsatomic"
)

// FilePerm is the mode of config.toml (§4.1). It is 0600 like everything else
// PayCLI writes: the file holds header values that §5.3 treats as secret.
const FilePerm fs.FileMode = 0o600

// File is a decoded config.toml (§4.2) or pay.toml (§4.3). The same schema
// serves both; a project file simply must not carry secrets.
type File struct {
	Version        int                `toml:"version,omitempty"`
	DefaultProfile string             `toml:"default_profile,omitempty"`
	Defaults       Defaults           `toml:"defaults,omitempty"`
	Cache          CacheSection       `toml:"cache,omitempty"`
	Logging        LoggingSection     `toml:"logging,omitempty"`
	Update         UpdateSection      `toml:"update,omitempty"`
	Profiles       map[string]Profile `toml:"profiles,omitempty"`

	// APIKey is not a legal key anywhere (§4.2); it is decoded only so the
	// hard error can name it.
	//nolint:gosec // G117: decode-only. Encode() clears it; see
	// TestEncodeNeverEmitsAPIKey and TestAPIKeyInConfigIsFatal.
	APIKey string `toml:"api_key,omitempty"`

	// Path is where the file came from; it is what `pay config explain`
	// prints as "user:/abs/config.toml" or "project:/abs/pay.toml" (§4.5).
	Path string `toml:"-"`
	// Exists is false for a file that was absent, which is not an error.
	Exists bool `toml:"-"`
	// Kind is "user" or "project"; it prefixes the source string.
	Kind string `toml:"-"`
	// UnknownKeys are decoded-but-unrecognised keys, surfaced as warnings so a
	// typo like `base_ur1` is visible without being fatal.
	UnknownKeys []string `toml:"-"`
}

// Layer kinds for the source strings of §4.5.
const (
	KindUser    = "user"
	KindProject = "project"
)

// Load reads and validates a config file. A missing file yields a usable empty
// File with Exists == false and no error: PayCLI must work with no config at
// all, driven entirely by flags and environment (§4.5, §21).
func Load(path, kind string) (*File, error) {
	f := &File{Path: path, Kind: kind, Profiles: map[string]Profile{}}
	if path == "" {
		return f, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return f, nil
		}
		return f, apierr.Wrap(err, apierr.CodeConfigMissing, "cannot read config file %s", path)
	}
	return Parse(data, path, kind)
}

// Parse decodes TOML bytes. It is the I/O-free half of Load, which is what the
// tests use.
func Parse(data []byte, path, kind string) (*File, error) {
	f := &File{Path: path, Kind: kind, Exists: true, Profiles: map[string]Profile{}}
	md, err := toml.Decode(string(data), f)
	if err != nil {
		return f, apierr.Wrap(err, apierr.CodeConfigMissing, "cannot parse %s: %v", displayPath(path), err)
	}
	if f.Profiles == nil {
		f.Profiles = map[string]Profile{}
	}
	if err := checkForbiddenKeys(md, path); err != nil {
		return f, err
	}
	for _, key := range md.Undecoded() {
		f.UnknownKeys = append(f.UnknownKeys, key.String())
	}
	sort.Strings(f.UnknownKeys)
	if err := f.Validate(); err != nil {
		return f, err
	}
	return f, nil
}

// checkForbiddenKeys implements §4.2's hard error: `api_key` is not a valid
// field anywhere in a config file, at any nesting depth. The check runs over
// every key the decoder *saw*, decoded or not, so it cannot be dodged by
// putting it in a table PayCLI does not model.
func checkForbiddenKeys(md toml.MetaData, path string) error {
	for _, key := range md.Keys() {
		parts := []string(key)
		if len(parts) == 0 {
			continue
		}
		if strings.EqualFold(parts[len(parts)-1], "api_key") {
			return apierr.New(apierr.CodeConfigSecretInPlaintext,
				"%s sets %s: an API key may never be stored in a config file", displayPath(path), key.String()).
				WithHint("remove it and store the key instead: pay auth login --api-key-stdin, " +
					"or point api_key_env at an environment variable")
		}
	}
	return nil
}

// Validate applies the schema rules that do not need any other layer.
func (f *File) Validate() error {
	if f.APIKey != "" {
		return apierr.New(apierr.CodeConfigSecretInPlaintext,
			"%s sets api_key: an API key may never be stored in a config file", displayPath(f.Path))
	}
	for name, p := range f.Profiles {
		if p.APIKey != "" {
			return apierr.New(apierr.CodeConfigSecretInPlaintext,
				"%s sets profiles.%s.api_key: an API key may never be stored in a config file",
				displayPath(f.Path), name)
		}
		if p.AuthMode != "" {
			if _, err := ParseAuthMode(p.AuthMode); err != nil {
				return apierr.Wrap(err, apierr.CodeInvalidOption,
					"%s: profiles.%s.auth_mode: %v", displayPath(f.Path), name, err)
			}
		}
	}
	if f.Version != 0 && f.Version != 1 {
		return apierr.New(apierr.CodeConfigMissing,
			"%s declares version = %d; this build understands version 1", displayPath(f.Path), f.Version)
	}
	return nil
}

// Profile returns the named profile and whether it is defined here.
func (f *File) Profile(name string) (Profile, bool) {
	if f == nil || f.Profiles == nil {
		return Profile{}, false
	}
	p, ok := f.Profiles[name]
	return p, ok
}

// Source renders this layer's §4.5 source string, e.g.
// "user:/home/u/.config/pay/config.toml".
func (f *File) Source() string {
	if f == nil || f.Path == "" {
		return ""
	}
	kind := f.Kind
	if kind == "" {
		kind = KindUser
	}
	return kind + ":" + f.Path
}

// Save writes the file atomically at 0600 (§4.1). Version is forced to 1 so a
// hand-written file that omitted it does not round-trip as 0.
func (f *File) Save() error {
	if f.Path == "" {
		return apierr.New(apierr.CodeInternal, "config: Save called with no path")
	}
	if f.Version == 0 {
		f.Version = 1
	}
	if err := f.Validate(); err != nil {
		return err
	}
	data, err := f.Encode()
	if err != nil {
		return err
	}
	if err := fsatomic.Write(f.Path, data, FilePerm); err != nil {
		return apierr.Wrap(err, apierr.CodeInternal, "cannot write %s", displayPath(f.Path))
	}
	f.Exists = true
	return nil
}

// Encode renders the file as TOML.
//
// APIKey is cleared first. The field exists only so a config.toml carrying the
// forbidden `api_key` decodes and can be rejected by name (§4.2); it must never
// travel in the other direction. Without this, any code path that set it would
// make Encode write a credential into config.toml — exactly what §5.2 forbids
// and what scripts/arch-lint.sh greps for. Encode operates on a copy, so the
// caller's File is untouched.
func (f *File) Encode() ([]byte, error) {
	safe := *f
	safe.APIKey = ""
	for name, p := range safe.Profiles {
		if p.APIKey != "" {
			p.APIKey = ""
			safe.Profiles[name] = p
		}
	}
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	//nolint:gosec // G117: safe.APIKey was just cleared above; see TestEncodeNeverEmitsAPIKey.
	if err := enc.Encode(&safe); err != nil {
		return nil, apierr.Wrap(err, apierr.CodeInternal, "cannot encode config: %v", err)
	}
	return buf.Bytes(), nil
}

// SetProfile inserts or replaces a profile.
func (f *File) SetProfile(name string, p Profile) {
	if f.Profiles == nil {
		f.Profiles = map[string]Profile{}
	}
	f.Profiles[name] = p
}

// displayPath keeps error messages readable when a path is empty (parsing from
// memory in tests and in `pay config explain --dry-run`).
func displayPath(path string) string {
	if path == "" {
		return "config"
	}
	return path
}

// String makes a File printable in test failures without dumping every field.
func (f *File) String() string {
	if f == nil {
		return "<nil config>"
	}
	return fmt.Sprintf("config{path:%s exists:%t profiles:%v}", f.Path, f.Exists, ProfileNames(f))
}
