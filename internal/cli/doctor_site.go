package cli

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/secret"
)

// ---------------------------------------------------------------------------
// pay doctor: is this binary reachable as `pay`? (F9 small item)
// ---------------------------------------------------------------------------

// pathProbe is the file-system surface binaryPathCheck needs, injected so the
// check is a table test.
type pathProbe struct {
	// exists reports whether a regular, executable-looking file is there.
	exists func(path string) bool
	// resolve follows symlinks (filepath.EvalSymlinks).
	resolve func(path string) (string, error)
}

func osPathProbe() pathProbe {
	return pathProbe{
		exists: func(p string) bool {
			fi, err := os.Stat(p)
			return err == nil && fi.Mode().IsRegular()
		},
		resolve: filepath.EvalSymlinks,
	}
}

// binaryPathCheck answers: would a shell that types `pay` run THIS binary?
//
// Agents drive PayCLI from non-interactive shells, which usually read neither
// ~/.bashrc nor ~/.profile — so an install whose directory was added to PATH
// only there works in the user's terminal and fails with "command not found"
// in the agent's. That was observed on the machine this check was written on:
// ~/.local/bin/pay existed, ~/.profile and ~/.bashrc both exported
// ~/.local/bin, and the agent's $PATH did not contain it.
func binaryPathCheck(exe, pathEnv, home, goos string, probe pathProbe) (Check, map[string]any) {
	info := map[string]any{"executable": exe, "on_path": nil}
	if exe == "" {
		return Check{Name: "binary_on_path", Status: CheckUnknown,
			Detail: "the running binary's path could not be determined"}, info
	}
	name := "pay"
	if goos == "windows" {
		name = "pay.exe"
	}
	dir := filepath.Dir(exe)
	info["dir"] = dir
	self := exe
	if r, err := probe.resolve(exe); err == nil {
		self = r
	}
	entries := filepath.SplitList(pathEnv)
	for _, d := range entries {
		if d == "" {
			continue
		}
		candidate := filepath.Join(d, name)
		if !probe.exists(candidate) {
			continue
		}
		target := candidate
		if r, err := probe.resolve(candidate); err == nil {
			target = r
		}
		info["first_on_path"] = candidate
		if target == self {
			info["on_path"] = true
			return Check{Name: "binary_on_path", Status: CheckOK,
				Detail: fmt.Sprintf("`%s` on $PATH is this binary (%s)", name, candidate), Value: candidate}, info
		}
		info["on_path"] = false
		return Check{Name: "binary_on_path", Status: CheckWarn,
			Detail: fmt.Sprintf("the first `%s` on $PATH is %s, a different binary than this one (%s); "+
				"a shell that types `pay` runs the other one", name, candidate, self),
			Hint: fmt.Sprintf("remove or update %s, or run this binary by its absolute path: %s", candidate, exe)}, info
	}
	info["on_path"] = false
	return Check{Name: "binary_on_path", Status: CheckWarn,
		Detail: fmt.Sprintf("%s is not on $PATH, so `%s` is \"command not found\" in this shell. Agent shells are "+
			"usually non-interactive and do not read ~/.bashrc or ~/.profile, where installers add it", dir, name),
		Hint: PathAdvice(exe, entries, home)}, info
}

// PathAdvice is the one remedy text for "the binary's directory is not on
// $PATH", shared by `pay doctor` and install.sh (which prints the same
// sentences). The robust fix is a link from a directory the shell ALREADY
// searches; editing rc files helps only the shells that read them.
func PathAdvice(exe string, pathEntries []string, home string) string {
	// Prefer a conventional per-user bin directory the shell already
	// searches (no sudo); never an arbitrary directory under $HOME, which on
	// an agent's PATH is often a tool's private cache.
	link := "/usr/local/bin"
	if home != "" {
		on := map[string]bool{}
		for _, d := range pathEntries {
			on[filepath.Clean(d)] = true
		}
		for _, d := range []string{filepath.Join(home, ".local", "bin"), filepath.Join(home, "bin")} {
			if on[d] && d != filepath.Dir(exe) {
				link = d
				break
			}
		}
	}
	dir := filepath.Dir(exe)
	return fmt.Sprintf("link it into a directory that is already on $PATH: ln -sf %s %s "+
		"(sudo if needed); or export PATH=\"%s:$PATH\" in the agent's own environment (for Claude Code: "+
		"\"env\" in settings.json) and in ~/.profile plus ~/.zshenv; until then call %s by its absolute path",
		exe, filepath.Join(link, filepath.Base(exe)), dir, exe)
}

// ---------------------------------------------------------------------------
// pay doctor: frontend URLs and draft preview (F9)
// ---------------------------------------------------------------------------

// siteChecks reports site_url, url_paths and the preview configuration. It is
// local-only: nothing here sends a request, so it runs even when the server is
// unreachable, and it never prints the preview secret.
func (rt *Runtime) siteChecks(r *doctorReport, add func(Check)) {
	cfg := rt.Cfg
	r.Site["site_url"] = cfg.SiteURL
	r.Site["site_url_source"] = cfg.Sources["site_url"]
	r.Site["preview_path"] = cfg.PreviewPath
	r.Site["preview_path_source"] = cfg.Sources["preview_path"]
	paths := cfg.URLPaths
	if paths == nil {
		paths = map[string]string{}
	}
	r.Site["url_paths"] = paths

	if cfg.SiteURL == "" {
		add(Check{Name: "site_url", Status: CheckWarn, Detail: "no site_url could be derived; `pay url` cannot build URLs",
			Hint: "pay config set profiles." + cfg.Profile + ".site_url https://www.example.com"})
		return
	}
	crossOrigin := false
	if su, err := url.Parse(cfg.SiteURL); err == nil {
		if bu, err := url.Parse(cfg.BaseURL); err == nil {
			crossOrigin = !payload.SameOrigin(su, bu)
		}
	}
	r.Site["cross_origin"] = crossOrigin
	add(Check{Name: "site_url", Status: CheckOK,
		Detail: cfg.SiteURL + " (" + cfg.Sources["site_url"] + ")", Value: cfg.SiteURL})

	if len(paths) == 0 {
		add(Check{Name: "url_paths", Status: CheckOK,
			Detail: "no url_paths templates: `pay url` uses breadcrumbs, else /{slug}, and flags /{slug} as a guess outside pages",
			Hint:   "pay config set profiles." + cfg.Profile + ".url_paths.posts '/posts/{slug}'"})
	} else {
		keys := make([]string, 0, len(paths))
		for k := range paths {
			keys = append(keys, k+"="+paths[k])
		}
		sort.Strings(keys)
		add(Check{Name: "url_paths", Status: CheckOK, Detail: strings.Join(keys, ", "), Value: paths})
	}

	needsKey := strings.Contains(cfg.PreviewPath, "{secret}")
	prev, err := rt.PreviewSecret()
	r.Site["preview_env_vars"] = secret.PreviewSecretEnvNames(cfg.Profile)
	r.Site["preview_profile_env_var"] = cfg.PreviewSecretEnv
	r.Site["preview_key_source"] = secret.SourceNone
	r.Site["preview_key_fingerprint"] = nil
	switch {
	case err != nil:
		add(Check{Name: "preview", Status: CheckFail, Detail: "the preview secret could not be read: " + err.Error(),
			Hint: "pay auth fix-perms"})
	case prev == nil && needsKey:
		add(Check{Name: "preview", Status: CheckWarn,
			Detail: "no preview secret is configured, so `pay url --preview` fails with preview_secret_missing (exit 9); " +
				"`pay url` without --preview works",
			Hint: "export PAY_PREVIEW_SECRET, or pay config set profiles." + cfg.Profile +
				".preview_secret_env PREVIEW_SECRET, or printf %s \"$PREVIEW_SECRET\" | pay auth preview-secret --stdin"})
	case crossOrigin:
		if prev != nil {
			r.Site["preview_key_source"] = prev.Source
			r.Site["preview_key_fingerprint"] = prev.Fingerprint
		}
		add(Check{Name: "preview", Status: CheckWarn,
			Detail: "site_url's origin differs from base_url's, so the preview handshake is sent WITHOUT the profile's " +
				"credential; a preview route that requires a logged-in user (the website template's does) answers 403",
			Hint: "leave site_url unset when the site and the API share an origin: pay config unset profiles." + cfg.Profile + ".site_url"})
	default:
		detail := "preview_path needs no secret"
		if prev != nil {
			r.Site["preview_key_source"] = prev.Source
			r.Site["preview_key_fingerprint"] = prev.Fingerprint
			detail = "preview secret from " + prev.Source + " (fingerprint " + prev.Fingerprint + ")"
		}
		add(Check{Name: "preview", Status: CheckOK, Detail: detail + "; handshake route " + cfg.PreviewPath})
	}
}

// doctorGOOS is runtime.GOOS, named so the binary check reads as a parameter.
var doctorGOOS = runtime.GOOS

// pathEnvName is the variable binaryPathCheck reads.
const pathEnvName = "PATH"
