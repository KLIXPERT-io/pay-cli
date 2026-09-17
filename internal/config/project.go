package config

import (
	"os"
	"path/filepath"
	"strings"
)

// maxWalkDepth bounds the upward walk of §4.3 so a pathological symlink farm
// or a very deep checkout cannot turn config loading into a filesystem crawl.
const maxWalkDepth = 64

// Project is what the upward walk of §4.3 found around the working directory.
// Every field is optional: PayCLI is usable from any directory, including one
// that has nothing to do with a Payload project.
type Project struct {
	// Dir is the directory that anchored the discovery — the first one going
	// up that contained any marker at all.
	Dir string
	// ConfigPath is ./pay.toml, the committed project config (§4.3).
	ConfigPath string
	// PackageJSON is the nearest package.json (§7.11 source 2).
	PackageJSON string
	// PayloadConfig is the nearest payload.config.ts / .js / .mjs, at the root
	// or under src/ (§7.10).
	PayloadConfig string
	// SrcDir is the payload config's directory when it lives in src/.
	SrcDir string
	// GitRoot is the directory holding .git; the walk stops after it.
	GitRoot string
	// Start is the directory the walk began in.
	Start string

	// firstMarker is the deepest directory that carried any marker; it is the
	// fallback root when no stronger marker exists.
	firstMarker string
}

// payloadConfigNames are the file names Payload projects actually use.
var payloadConfigNames = []string{
	"payload.config.ts", "payload.config.js", "payload.config.mjs", "payload.config.mts",
}

// FindProject walks up from start looking for a project (§4.3). It stops at
// the git root, at the filesystem root, or after maxWalkDepth directories.
//
// The walk never leaves the filesystem and never executes anything it finds.
func FindProject(start string) *Project {
	p := &Project{Start: start}
	if start == "" {
		return p
	}
	dir, err := filepath.Abs(start)
	if err != nil {
		return p
	}

	for depth := 0; depth < maxWalkDepth; depth++ {
		if p.ConfigPath == "" {
			if path := existing(filepath.Join(dir, ProjectFileName)); path != "" {
				p.ConfigPath = path
				p.note(dir)
			}
		}
		if p.PackageJSON == "" {
			if path := existing(filepath.Join(dir, "package.json")); path != "" {
				p.PackageJSON = path
				p.note(dir)
			}
		}
		if p.PayloadConfig == "" {
			for _, name := range payloadConfigNames {
				if path := existing(filepath.Join(dir, name)); path != "" {
					p.PayloadConfig = path
					p.note(dir)
					break
				}
				if path := existing(filepath.Join(dir, "src", name)); path != "" {
					p.PayloadConfig = path
					p.SrcDir = filepath.Join(dir, "src")
					p.note(dir)
					break
				}
			}
		}
		if isDir(filepath.Join(dir, ".git")) || existing(filepath.Join(dir, ".git")) != "" {
			p.GitRoot = dir
			p.note(dir)
			break
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	p.resolveRoot()
	if p.SrcDir == "" && p.Dir != "" && isDir(filepath.Join(p.Dir, "src")) {
		p.SrcDir = filepath.Join(p.Dir, "src")
	}
	return p
}

// note records the first (deepest) directory that had any marker at all. It
// is only a fallback: resolveRoot picks the strongest marker.
func (p *Project) note(dir string) {
	if p.firstMarker == "" {
		p.firstMarker = dir
	}
}

// resolveRoot picks the project root from the markers found, strongest first:
// the git root, then the directory holding pay.toml, then package.json, then
// the payload config (or its parent when it lives in src/).
//
// The git root wins because §7.10's scan wants the whole repository — a
// monorepo keeps blocks in packages/ as well as src/ — while the caps in
// projectscan.go keep that bounded.
func (p *Project) resolveRoot() {
	switch {
	case p.GitRoot != "":
		p.Dir = p.GitRoot
	case p.ConfigPath != "":
		p.Dir = filepath.Dir(p.ConfigPath)
	case p.PackageJSON != "":
		p.Dir = filepath.Dir(p.PackageJSON)
	case p.PayloadConfig != "":
		dir := filepath.Dir(p.PayloadConfig)
		if filepath.Base(dir) == "src" {
			dir = filepath.Dir(dir)
		}
		p.Dir = dir
	default:
		p.Dir = p.firstMarker
	}
}

// Found reports whether the walk saw anything project-shaped.
func (p *Project) Found() bool { return p != nil && p.Dir != "" }

// IsPayload reports whether this looks like a Payload project specifically —
// the precondition for §7.10's block-slug scan and §7.11's version read.
func (p *Project) IsPayload() bool { return p != nil && p.PayloadConfig != "" }

// LoadConfig loads the project pay.toml, if the walk found one. A project
// without pay.toml is normal and yields an empty, non-existent File.
func (p *Project) LoadConfig() (*File, error) {
	if p == nil || p.ConfigPath == "" {
		return &File{Kind: KindProject, Profiles: map[string]Profile{}}, nil
	}
	return Load(p.ConfigPath, KindProject)
}

// GitignoreCovers reports whether the project's .gitignore mentions pattern.
// `pay auth login` uses it to warn when a skill directory or a credential file
// would be committed (§5.3). A missing .gitignore reports false.
func (p *Project) GitignoreCovers(pattern string) bool {
	if p == nil || p.Dir == "" {
		return false
	}
	root := p.GitRoot
	if root == "" {
		root = p.Dir
	}
	data, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		return false
	}
	pattern = strings.TrimSuffix(strings.TrimPrefix(pattern, "/"), "/")
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSuffix(strings.TrimPrefix(line, "/"), "/")
		if line == pattern {
			return true
		}
	}
	return false
}

func existing(path string) string {
	fi, err := os.Lstat(path)
	if err != nil || fi.IsDir() {
		return ""
	}
	return path
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
