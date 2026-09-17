package update

import (
	"path"
	"path/filepath"
	"strings"
)

// Managed says whether the running binary was installed by a package manager,
// in which case PayCLI must not swap it (§15.5).
type Managed struct {
	// Managed is true when a manager owns this binary.
	Managed bool `json:"managed"`
	// Manager names it ("go install", "homebrew", "nix", …).
	Manager string `json:"manager,omitempty"`
	// Hint is the command the user should run instead.
	Hint string `json:"hint,omitempty"`
}

// Env is the small slice of the environment managed-install detection needs.
// It is passed in rather than read, so the detection is a pure function.
type Env struct {
	GOPATH string
	GOBIN  string
	Home   string
	GOOS   string
}

// DetectManaged classifies the install path. The path should already be
// symlink-resolved (ResolveBinary does that).
func DetectManaged(binPath string, env Env) Managed {
	p := normalisePath(binPath)
	if p == "" {
		return Managed{}
	}

	type rule struct {
		match   string
		manager string
		hint    string
	}

	var rules []rule
	if env.GOBIN != "" {
		rules = append(rules, rule{normalisePath(env.GOBIN), "go install", goInstallHint})
	}
	for _, gopath := range strings.Split(env.GOPATH, string(filepath.ListSeparator)) {
		if gopath = strings.TrimSpace(gopath); gopath != "" {
			rules = append(rules, rule{normalisePath(filepath.Join(gopath, "bin")), "go install", goInstallHint})
		}
	}
	if env.Home != "" {
		home := normalisePath(env.Home)
		rules = append(rules,
			rule{home + "/go/bin", "go install", goInstallHint},
			rule{home + "/.asdf", "asdf", "asdf install pay latest && asdf global pay latest"},
			rule{home + "/.local/share/mise", "mise", "mise upgrade pay"},
			rule{home + "/scoop", "scoop", "scoop update pay"},
		)
	}
	rules = append(rules,
		rule{"/nix/store", "nix", "update your flake or nix profile: nix profile upgrade pay"},
		rule{"/opt/homebrew", "homebrew", "brew upgrade pay"},
		rule{"/usr/local/cellar", "homebrew", "brew upgrade pay"},
		rule{"/home/linuxbrew/.linuxbrew", "homebrew", "brew upgrade pay"},
		rule{"/snap", "snap", "sudo snap refresh pay"},
		rule{"/var/lib/flatpak", "flatpak", "flatpak update"},
		rule{"/var/lib/snapd", "snap", "sudo snap refresh pay"},
		rule{"c:/program files", "system install", "reinstall from the release archive with administrator rights"},
		rule{"c:/program files (x86)", "system install", "reinstall from the release archive with administrator rights"},
		rule{"c:/programdata/chocolatey", "chocolatey", "choco upgrade pay"},
	)

	for _, r := range rules {
		if r.match == "" {
			continue
		}
		if p == r.match || strings.HasPrefix(p, strings.TrimSuffix(r.match, "/")+"/") {
			return Managed{Managed: true, Manager: r.manager, Hint: r.hint}
		}
	}
	return Managed{}
}

const goInstallHint = "go install github.com/KLIXPERT-io/pay-cli/cmd/pay@latest"

// normalisePath lower-cases and slash-normalises a path so the table above can
// be compared case-insensitively on Windows and macOS.
func normalisePath(p string) string {
	if p == "" {
		return ""
	}
	// Backslashes are converted on every host, not just Windows, so the
	// Windows rules below are testable from a Linux CI runner.
	return strings.ToLower(path.Clean(strings.ReplaceAll(filepath.ToSlash(p), "\\", "/")))
}
