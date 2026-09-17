package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPathsFor(t *testing.T) {
	const home = "/home/u"
	tests := []struct {
		name                     string
		goos                     string
		env                      Env
		wantConfig, wantCache    string
		wantState, wantConfigSrc string
	}{
		{
			name:          "linux defaults",
			goos:          "linux",
			env:           Env{},
			wantConfig:    "/home/u/.config/pay",
			wantCache:     "/home/u/.cache/pay",
			wantState:     "/home/u/.local/state/pay",
			wantConfigSrc: "default",
		},
		{
			name:          "macos uses the same xdg layout",
			goos:          "darwin",
			env:           Env{},
			wantConfig:    "/home/u/.config/pay",
			wantCache:     "/home/u/.cache/pay",
			wantState:     "/home/u/.local/state/pay",
			wantConfigSrc: "default",
		},
		{
			name:          "xdg overrides",
			goos:          "linux",
			env:           Env{"XDG_CONFIG_HOME": "/x/cfg", "XDG_CACHE_HOME": "/x/cache", "XDG_STATE_HOME": "/x/state"},
			wantConfig:    "/x/cfg/pay",
			wantCache:     "/x/cache/pay",
			wantState:     "/x/state/pay",
			wantConfigSrc: "env:XDG_CONFIG_HOME",
		},
		{
			name:          "PAY_HOME overrides the defaults",
			goos:          "linux",
			env:           Env{"PAY_HOME": "/tmp/payhome", "XDG_CONFIG_HOME": "/x/cfg"},
			wantConfig:    "/tmp/payhome",
			wantCache:     "/tmp/payhome/cache",
			wantState:     "/tmp/payhome/state",
			wantConfigSrc: "env:PAY_HOME",
		},
		{
			name:          "the specific variable beats PAY_HOME",
			goos:          "linux",
			env:           Env{"PAY_HOME": "/tmp/payhome", "PAY_CACHE_DIR": "/var/cache/pay"},
			wantConfig:    "/tmp/payhome",
			wantCache:     "/var/cache/pay",
			wantState:     "/tmp/payhome/state",
			wantConfigSrc: "env:PAY_HOME",
		},
		{
			name:          "windows",
			goos:          "windows",
			env:           Env{"AppData": `C:\Users\u\AppData\Roaming`, "LocalAppData": `C:\Users\u\AppData\Local`},
			wantConfig:    filepath.Clean(`C:\Users\u\AppData\Roaming/pay`),
			wantCache:     filepath.Clean(`C:\Users\u\AppData\Local/pay/cache`),
			wantState:     filepath.Clean(`C:\Users\u\AppData\Local/pay/state`),
			wantConfigSrc: "windows:%AppData%",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := PathsFor(tc.env, tc.goos, home)
			if p.ConfigDir != tc.wantConfig {
				t.Errorf("config dir = %q, want %q", p.ConfigDir, tc.wantConfig)
			}
			if p.CacheDir != tc.wantCache {
				t.Errorf("cache dir = %q, want %q", p.CacheDir, tc.wantCache)
			}
			if p.StateDir != tc.wantState {
				t.Errorf("state dir = %q, want %q", p.StateDir, tc.wantState)
			}
			if p.Sources["config_dir"] != tc.wantConfigSrc {
				t.Errorf("config dir source = %q, want %q", p.Sources["config_dir"], tc.wantConfigSrc)
			}
			if want := joinFor(tc.goos, p.ConfigDir, ConfigFileName); p.ConfigFile != want {
				t.Errorf("config file = %q, want %q", p.ConfigFile, want)
			}
		})
	}
}

func TestPathsOverrides(t *testing.T) {
	p := PathsFor(Env{"PAY_CONFIG": "/etc/pay/custom.toml"}, "linux", "/home/u")
	if p.ConfigFile != "/etc/pay/custom.toml" {
		t.Errorf("PAY_CONFIG ignored: %q", p.ConfigFile)
	}
	if p.Sources["config_file"] != "env:PAY_CONFIG" {
		t.Errorf("source = %q", p.Sources["config_file"])
	}

	flagged := p.WithConfigFile("/tmp/flag.toml")
	if flagged.ConfigFile != "/tmp/flag.toml" || flagged.Sources["config_file"] != SourceFlag {
		t.Errorf("--config did not win: %+v", flagged.Sources)
	}
	if p.ConfigFile != "/etc/pay/custom.toml" {
		t.Error("WithConfigFile mutated the receiver")
	}

	moved := p.WithCacheDir("/var/tmp/paycache")
	if moved.CacheDir != "/var/tmp/paycache" {
		t.Errorf("cache dir = %q", moved.CacheDir)
	}
	if p.CacheDir == moved.CacheDir {
		t.Error("WithCacheDir mutated the receiver")
	}
}

func TestPathsDerivedFiles(t *testing.T) {
	p := PathsFor(Env{"PAY_HOME": "/h"}, "linux", "/home/u")
	want := map[string]string{
		"credentials":  "/h/" + CredentialsFileName,
		"audit":        "/h/" + AuditFileName,
		"cache root":   "/h/cache/" + CacheGeneration,
		"scope":        "/h/cache/" + CacheGeneration + "/abcd",
		"update state": "/h/state/" + UpdateStateFileName,
	}
	got := map[string]string{
		"credentials":  p.CredentialsFile(),
		"audit":        p.AuditFile(),
		"cache root":   p.CacheRoot(),
		"scope":        p.ScopeDir("abcd"),
		"update state": p.UpdateStateFile(),
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if m := p.Map(); len(m) != 8 {
		t.Errorf("Map() has %d entries, want 8", len(m))
	}
}

func TestEnvLookupTreatsEmptyAsUnset(t *testing.T) {
	env := NewEnv([]string{"A=1", "B=", "MALFORMED", "=novalue", "C=x=y"})
	if v, ok := env.Lookup("A"); !ok || v != "1" {
		t.Errorf("A = %q,%v", v, ok)
	}
	if _, ok := env.Lookup("B"); ok {
		t.Error("an empty value must count as unset (§4.5)")
	}
	if !env.Has("B") {
		t.Error("Has must still see the variable")
	}
	if v := env.Get("C"); v != "x=y" {
		t.Errorf("C = %q, want x=y", v)
	}
	if _, ok := env.Lookup("MALFORMED"); ok {
		t.Error("a pair with no '=' must be dropped")
	}
	var nilEnv Env
	if _, ok := nilEnv.Lookup("A"); ok {
		t.Error("nil Env must be usable")
	}
}

// TestPathsForIsHostIndependent pins the property that made CI red on Windows:
// PathsFor takes goos as an argument, so its output must depend on that
// argument and not on the separator of the machine running the test. Before
// joinFor/cleanFor it used filepath, and every POSIX row of TestPathsFor
// produced `\home\u\.config\pay` on the windows-latest runner.
func TestPathsForIsHostIndependent(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			p := PathsFor(Env{"PAY_HOME": "/h"}, goos, "/home/u")
			paths := map[string]string{
				"ConfigDir":       p.ConfigDir,
				"CacheDir":        p.CacheDir,
				"StateDir":        p.StateDir,
				"ConfigFile":      p.ConfigFile,
				"CredentialsFile": p.CredentialsFile(),
				"AuditFile":       p.AuditFile(),
				"CacheRoot":       p.CacheRoot(),
				"ScopeDir":        p.ScopeDir("abcd"),
				"UpdateStateFile": p.UpdateStateFile(),
			}
			for name, got := range paths {
				if strings.Contains(got, `\`) {
					t.Errorf("%s = %q contains a backslash; goos=%q paths must be POSIX on every host", name, got, goos)
				}
				if !strings.HasPrefix(got, "/") {
					t.Errorf("%s = %q is not rooted", name, got)
				}
			}
		})
	}

	// And the windows rules must hold regardless of host too.
	w := PathsFor(Env{"AppData": `C:\Users\u\AppData\Roaming`, "LocalAppData": `C:\Users\u\AppData\Local`}, "windows", `C:\Users\u`)
	if !strings.HasPrefix(w.ConfigDir, `C:`) {
		t.Errorf("windows ConfigDir = %q, want it under C:", w.ConfigDir)
	}
}
