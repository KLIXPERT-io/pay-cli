// Package config owns everything PayCLI knows before it talks to a server:
// where its files live (§4.1), what is in them (§4.2, §4.3), how the layers
// combine (§4.5) and what can be learned about the surrounding Payload project
// from the local filesystem alone (§7.10, §7.11).
//
// Two rules shape this package:
//
//   - Resolve is a pure function. It performs no I/O whatsoever, so §4.5's
//     precedence matrix is a table test (§3.1).
//   - Nothing here reads the process environment directly. It enters as an Env
//     snapshot built once by internal/cli/app.go, which is the only file
//     allowed to touch it (§3.1).
package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Env is a snapshot of the process environment.
//
// Lookup implements §4.5's "set but empty counts as unset" rule, so
// `PAY_API_KEY= pay …` cannot silently blank a credential.
type Env map[string]string

// NewEnv builds an Env from os.Environ()-shaped "NAME=value" pairs.
func NewEnv(pairs []string) Env {
	e := make(Env, len(pairs))
	for _, kv := range pairs {
		if i := strings.IndexByte(kv, '='); i > 0 {
			e[kv[:i]] = kv[i+1:]
		}
	}
	return e
}

// Lookup returns the value of name, treating a set-but-empty variable as unset
// (§4.5).
func (e Env) Lookup(name string) (string, bool) {
	if e == nil {
		return "", false
	}
	v, ok := e[name]
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

// Get returns the value of name or "".
func (e Env) Get(name string) string {
	v, _ := e.Lookup(name)
	return v
}

// Has reports whether name is present at all, empty value included. Only the
// handful of "presence is the signal" variables should use this; everything in
// §4.5's precedence chain goes through Lookup.
func (e Env) Has(name string) bool {
	if e == nil {
		return false
	}
	_, ok := e[name]
	return ok
}

// File names inside the resolved directories (§4.1).
const (
	ConfigFileName      = "config.toml"
	CredentialsFileName = "credentials.json"
	AuditFileName       = "audit.log"
	UpdateStateFileName = "update-state.json"
	ProjectFileName     = "pay.toml"

	// CacheGeneration is the versioned sub-directory of the cache dir; the
	// whole tree below it is disposable (§4.1, §8).
	CacheGeneration = "v1"
)

// Paths is the one source of truth for where PayCLI keeps its files (§4.1).
// Sources explains, per directory, which rule produced it, so
// `pay config paths --output json` never has to guess.
type Paths struct {
	ConfigDir  string
	CacheDir   string
	StateDir   string
	ConfigFile string

	Sources map[string]string

	// goos is the platform these paths were resolved FOR, captured so the
	// derived-path helpers below join with the same separator rules PathsFor
	// used. Unexported: it is an implementation detail, not part of the
	// documented Paths surface, and Map() must stay at 8 entries.
	goos string
}

// PathsFor is the pure form of path resolution: everything it needs is an
// argument, so the Windows and XDG branches are both testable on any host.
//
// goos is a runtime.GOOS value; home is the user's home directory (may be
// empty, in which case relative fallbacks are used rather than failing — a
// missing $HOME must never make `pay version` impossible).
//
// Precedence per directory: the specific PAY_*_DIR variable, then $PAY_HOME,
// then the platform rule. PAY_HOME "overrides all three" defaults (§4.1) but
// the more specific variable still wins over it — that is the conventional
// reading, and it lets a test pin PAY_HOME while redirecting just the cache.
func PathsFor(env Env, goos, home string) Paths {
	p := Paths{Sources: map[string]string{}, goos: goos}
	payHome := env.Get("PAY_HOME")

	set := func(field *string, source *string, value, src string) bool {
		if value == "" {
			return false
		}
		*field = cleanFor(goos, value)
		*source = src
		return true
	}

	var cfgSrc, cacheSrc, stateSrc string

	// config dir
	switch {
	case set(&p.ConfigDir, &cfgSrc, env.Get("PAY_CONFIG_DIR"), "env:PAY_CONFIG_DIR"):
	case set(&p.ConfigDir, &cfgSrc, payHome, "env:PAY_HOME"):
	case goos == "windows":
		set(&p.ConfigDir, &cfgSrc, joinFor(goos, appData(env, home), "pay"), "windows:%AppData%")
	default:
		if xdg := env.Get("XDG_CONFIG_HOME"); xdg != "" {
			set(&p.ConfigDir, &cfgSrc, joinFor(goos, xdg, "pay"), "env:XDG_CONFIG_HOME")
		} else {
			set(&p.ConfigDir, &cfgSrc, joinFor(goos, home, ".config", "pay"), "default")
		}
	}

	// cache dir
	switch {
	case set(&p.CacheDir, &cacheSrc, env.Get("PAY_CACHE_DIR"), "env:PAY_CACHE_DIR"):
	case set(&p.CacheDir, &cacheSrc, joinUnder(goos, payHome, "cache"), "env:PAY_HOME"):
	case goos == "windows":
		set(&p.CacheDir, &cacheSrc, joinFor(goos, localAppData(env, home), "pay", "cache"), "windows:%LocalAppData%")
	default:
		if xdg := env.Get("XDG_CACHE_HOME"); xdg != "" {
			set(&p.CacheDir, &cacheSrc, joinFor(goos, xdg, "pay"), "env:XDG_CACHE_HOME")
		} else {
			set(&p.CacheDir, &cacheSrc, joinFor(goos, home, ".cache", "pay"), "default")
		}
	}

	// state dir
	switch {
	case set(&p.StateDir, &stateSrc, env.Get("PAY_STATE_DIR"), "env:PAY_STATE_DIR"):
	case set(&p.StateDir, &stateSrc, joinUnder(goos, payHome, "state"), "env:PAY_HOME"):
	case goos == "windows":
		set(&p.StateDir, &stateSrc, joinFor(goos, localAppData(env, home), "pay", "state"), "windows:%LocalAppData%")
	default:
		if xdg := env.Get("XDG_STATE_HOME"); xdg != "" {
			set(&p.StateDir, &stateSrc, joinFor(goos, xdg, "pay"), "env:XDG_STATE_HOME")
		} else {
			set(&p.StateDir, &stateSrc, joinFor(goos, home, ".local", "state", "pay"), "default")
		}
	}

	p.ConfigFile = joinFor(goos, p.ConfigDir, ConfigFileName)
	fileSrc := "default"
	if v, ok := env.Lookup("PAY_CONFIG"); ok {
		p.ConfigFile = cleanFor(goos, v)
		fileSrc = "env:PAY_CONFIG"
	}

	p.Sources["config_dir"] = cfgSrc
	p.Sources["cache_dir"] = cacheSrc
	p.Sources["state_dir"] = stateSrc
	p.Sources["config_file"] = fileSrc
	return p
}

// DefaultPaths is PathsFor bound to this host. It is the only function in the
// package that touches the OS, and it never fails: an unknown home directory
// degrades to the process working directory rather than aborting the CLI.
func DefaultPaths(env Env) Paths {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = env.Get("HOME")
	}
	return PathsFor(env, runtime.GOOS, home)
}

// WithConfigFile applies the --config flag, which points at the config file
// itself and therefore also moves nothing else (§9.1).
func (p Paths) WithConfigFile(path string) Paths {
	if path == "" {
		return p
	}
	out := p.clone()
	out.ConfigFile = cleanFor(p.goos, path)
	out.Sources["config_file"] = "flag"
	return out
}

// WithCacheDir applies config.toml's [cache] dir override (§4.2).
func (p Paths) WithCacheDir(dir string) Paths {
	if dir == "" {
		return p
	}
	out := p.clone()
	out.CacheDir = cleanFor(p.goos, dir)
	out.Sources["cache_dir"] = "config:[cache].dir"
	return out
}

func (p Paths) clone() Paths {
	out := p
	out.Sources = make(map[string]string, len(p.Sources))
	for k, v := range p.Sources {
		out.Sources[k] = v
	}
	return out
}

// CredentialsFile is §4.4's 0600 credential store.
func (p Paths) CredentialsFile() string { return joinFor(p.goos, p.ConfigDir, CredentialsFileName) }

// AuditFile is §14's 0600 append-only audit log.
func (p Paths) AuditFile() string { return joinFor(p.goos, p.ConfigDir, AuditFileName) }

// CacheRoot is the generation directory; `rm -rf` on it must only ever cost
// time (§4.1).
func (p Paths) CacheRoot() string { return joinFor(p.goos, p.CacheDir, CacheGeneration) }

// ScopeDir is the cache directory for one §8.1 scope key.
func (p Paths) ScopeDir(scope string) string { return joinFor(p.goos, p.CacheRoot(), scope) }

// UpdateStateFile is §15's self-update bookkeeping.
func (p Paths) UpdateStateFile() string { return joinFor(p.goos, p.StateDir, UpdateStateFileName) }

// Map renders the paths for `pay config paths --output json`.
func (p Paths) Map() map[string]string {
	return map[string]string{
		"config_dir":        p.ConfigDir,
		"config_file":       p.ConfigFile,
		"credentials_file":  p.CredentialsFile(),
		"audit_file":        p.AuditFile(),
		"cache_dir":         p.CacheDir,
		"cache_root":        p.CacheRoot(),
		"state_dir":         p.StateDir,
		"update_state_file": p.UpdateStateFile(),
	}
}

func appData(env Env, home string) string {
	if v := env.Get("AppData"); v != "" {
		return v
	}
	if v := env.Get("APPDATA"); v != "" {
		return v
	}
	return filepath.Join(home, "AppData", "Roaming")
}

func localAppData(env Env, home string) string {
	if v := env.Get("LocalAppData"); v != "" {
		return v
	}
	if v := env.Get("LOCALAPPDATA"); v != "" {
		return v
	}
	return filepath.Join(home, "AppData", "Local")
}
