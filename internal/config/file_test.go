package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

const sampleConfig = `
version = 1
default_profile = "local"

[defaults]
output      = "json"
depth       = 0
limit       = 20
timeout     = "30s"
max_retries = 3
redact      = true

[cache]
discovery_ttl = "24h"
identity_ttl  = "60s"

[logging]
level = "debug"

[update]
auto = false

[profiles.local]
base_url        = "http://localhost:3900"
api_path        = "/api"
graphql_route   = "/graphql"
auth_collection = "auto"
auth_mode       = "auto"
label           = "payload-dummy dev"
api_key_env     = "DUMMY_KEY"
locales         = ["en", "de"]
payload_version = "3.86.0"

[profiles.local.headers]
"X-Bypass" = "${VERCEL_BYPASS}"

[profiles.local.blocks]
layout = ["cta", "content", "mediaBlock"]
`

func TestParse(t *testing.T) {
	f, err := Parse([]byte(sampleConfig), "/u/config.toml", KindUser)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.DefaultProfile != "local" {
		t.Errorf("default_profile = %q", f.DefaultProfile)
	}
	p, ok := f.Profile("local")
	if !ok {
		t.Fatal("profile local missing")
	}
	if p.BaseURL != "http://localhost:3900" || p.APIKeyEnv != "DUMMY_KEY" {
		t.Errorf("profile = %+v", p)
	}
	if got := p.Blocks["layout"]; len(got) != 3 || got[0] != "cta" {
		t.Errorf("blocks.layout = %v", got)
	}
	if len(p.Locales) != 2 || p.Locales[1] != "de" {
		t.Errorf("locales = %v", p.Locales)
	}
	if p.Headers["X-Bypass"] != "${VERCEL_BYPASS}" {
		t.Errorf("headers are interpolated at Resolve time, not parse time: %v", p.Headers)
	}
	if f.Logging.Level != "debug" || f.Cache.IdentityTTL != "60s" {
		t.Errorf("sections = %+v %+v", f.Logging, f.Cache)
	}
	if f.Update.Auto == nil || *f.Update.Auto {
		t.Errorf("update.auto = %v", f.Update.Auto)
	}
	if f.Defaults.Depth == nil || *f.Defaults.Depth != 0 {
		t.Error("depth = 0 must survive as a set value, not as absence")
	}
	if len(f.UnknownKeys) != 0 {
		t.Errorf("unexpected unknown keys: %v", f.UnknownKeys)
	}
	if f.Source() != "user:/u/config.toml" {
		t.Errorf("Source() = %q", f.Source())
	}
}

// TestAPIKeyInConfigIsFatal is §4.2's hard rule and arch-lint's (a) clause.
func TestAPIKeyInConfigIsFatal(t *testing.T) {
	bodies := []string{
		"api_key = \"paycli-dev-key\"\n",
		"[profiles.local]\napi_key = \"paycli-dev-key\"\n",
		"[defaults]\napi_key = \"paycli-dev-key\"\n",
		"[profiles.local.nested]\napi_key = \"paycli-dev-key\"\n",
	}
	for _, body := range bodies {
		_, err := Parse([]byte(body), "/u/config.toml", KindUser)
		if !apierr.HasCode(err, apierr.CodeConfigSecretInPlaintext) {
			t.Errorf("Parse(%q) err = %v, want config_secret_in_plaintext", body, err)
		}
		if err != nil && strings.Contains(err.Error(), "paycli-dev-key") {
			t.Errorf("the error message leaked the key: %v", err)
		}
	}
}

func TestParseErrors(t *testing.T) {
	if _, err := Parse([]byte("version = 2\n"), "/u/config.toml", KindUser); err == nil {
		t.Error("an unknown schema version must be rejected")
	}
	if _, err := Parse([]byte("this is not toml"), "/u/config.toml", KindUser); err == nil {
		t.Error("invalid TOML must be rejected")
	}
	_, err := Parse([]byte("[profiles.local]\nauth_mode = \"apikey\"\n"), "/u/c.toml", KindUser)
	if !apierr.HasCode(err, apierr.CodeInvalidOption) {
		t.Errorf("err = %v, want invalid_option", err)
	}
}

func TestUnknownKeysAreWarningsNotErrors(t *testing.T) {
	f, err := Parse([]byte("[profiles.local]\nbase_ur1 = \"http://x\"\n"), "/u/config.toml", KindUser)
	if err != nil {
		t.Fatalf("an unknown key must not be fatal: %v", err)
	}
	if len(f.UnknownKeys) != 1 || !strings.Contains(f.UnknownKeys[0], "base_ur1") {
		t.Errorf("UnknownKeys = %v", f.UnknownKeys)
	}
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	f, err := Load(filepath.Join(t.TempDir(), "nope.toml"), KindUser)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if f.Exists {
		t.Error("Exists must be false")
	}
	if _, ok := f.Profile("anything"); ok {
		t.Error("no profiles expected")
	}
}

func TestSaveIsAtomicAnd0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", ConfigFileName)
	f := &File{Path: path, Kind: KindUser, DefaultProfile: "local",
		Profiles: map[string]Profile{"local": {BaseURL: "http://localhost:3900"}}}
	if err := f.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Windows has no Unix mode bits: os.Chmod only toggles the read-only
	// attribute, so Perm() reports 0666 there. Confidentiality on Windows comes
	// from the file living under %AppData%, not from the mode.
	if runtime.GOOS != "windows" {
		if perm := fi.Mode().Perm(); perm != FilePerm {
			t.Errorf("mode = %04o, want %04o", perm, FilePerm)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("a temp file was left behind: %d entries", len(entries))
	}

	reloaded, err := Load(path, KindUser)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded.Version != 1 {
		t.Errorf("version = %d, want 1", reloaded.Version)
	}
	p, ok := reloaded.Profile("local")
	if !ok || p.BaseURL != "http://localhost:3900" {
		t.Errorf("round trip lost the profile: %+v", reloaded.Profiles)
	}
}

func TestProfileNames(t *testing.T) {
	a := &File{Profiles: map[string]Profile{"b": {}, "a": {}}}
	b := &File{Profiles: map[string]Profile{"c": {}, "a": {}}}
	got := ProfileNames(a, b, nil)
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("ProfileNames() = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ProfileNames() = %v, want %v", got, want)
		}
	}
}

// TestEncodeNeverEmitsAPIKey pins the other half of §4.2's rule. Load rejects a
// config that *contains* api_key; this asserts PayCLI can never *write* one.
// The File.APIKey field exists solely so a forbidden key decodes and can be
// named in the error, and a toml:"api_key" tag on a populated field would
// otherwise round-trip a credential straight into config.toml.
func TestEncodeNeverEmitsAPIKey(t *testing.T) {
	f := &File{
		Path: "x", Kind: KindUser, DefaultProfile: "local",
		APIKey:   "top-level-secret",
		Profiles: map[string]Profile{"local": {BaseURL: "http://localhost:3900"}},
	}
	out, err := f.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if strings.Contains(string(out), "api_key") {
		t.Errorf("Encode emitted an api_key key:\n%s", out)
	}
	if strings.Contains(string(out), "top-level-secret") {
		t.Errorf("Encode leaked the credential value:\n%s", out)
	}
	// The receiver must not have been mutated.
	if f.APIKey != "top-level-secret" {
		t.Errorf("Encode mutated the receiver: APIKey = %q", f.APIKey)
	}
}
