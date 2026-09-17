package skills

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/fsatomic"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

// Scope is where the skill is installed (§14).
type Scope string

const (
	// ScopeProject installs into the detected project root.
	ScopeProject Scope = "project"
	// ScopeUser installs into the user's home directory.
	ScopeUser Scope = "user"
)

// Agent is one coding agent's on-disk skill layout. The table is data in the
// binary rather than code so a new agent does not require a release; --dir PATH
// and the skills.extra_dirs config key extend it at runtime.
type Agent struct {
	// Name is what --agent matches, case-insensitively.
	Name string `json:"name"`
	// Home is the agent's directory relative to the scope root (".claude").
	// It is the directory whose EXISTENCE decides whether the default install
	// touches this agent at all.
	Home string `json:"home"`
	// Skills is the skill container inside Home ("skills").
	Skills string `json:"skills"`
}

// Agents is the §14 target list, in install order.
var Agents = []Agent{
	{Name: "claude", Home: ".claude", Skills: "skills"},
	{Name: "codex", Home: ".codex", Skills: "skills"},
	{Name: "cursor", Home: ".cursor", Skills: "skills"},
	{Name: "gemini", Home: ".gemini", Skills: "skills"},
	{Name: "gemini/antigravity", Home: ".antigravity", Skills: "skills"},
	{Name: "opencode", Home: ".opencode", Skills: "skills"},
	{Name: "windsurf", Home: ".windsurf", Skills: "skills"},
	{Name: "continue", Home: ".continue", Skills: "skills"},
	{Name: "crush", Home: ".crush", Skills: "skills"},
	{Name: "kiro", Home: ".kiro", Skills: "skills"},
	{Name: "qwen", Home: ".qwen", Skills: "skills"},
	{Name: "qoder", Home: ".qoder", Skills: "skills"},
}

// AgentNames lists the known agents, for help text and completions.
func AgentNames() []string {
	out := make([]string, 0, len(Agents))
	for _, a := range Agents {
		out = append(out, a.Name)
	}
	return out
}

// FindAgent looks an agent up by name, case-insensitively.
func FindAgent(name string) (Agent, bool) {
	for _, a := range Agents {
		if strings.EqualFold(a.Name, name) {
			return a, true
		}
	}
	return Agent{}, false
}

// HomeDir is the agent's own directory under root. Its existence is the signal
// "this agent is used here".
func (a Agent) HomeDir(root string) string { return filepath.Join(root, filepath.FromSlash(a.Home)) }

// SkillsDir is where all of this agent's skills live.
func (a Agent) SkillsDir(root string) string { return filepath.Join(a.HomeDir(root), a.Skills) }

// Dir is this skill's directory for the agent: <root>/<home>/skills/pay.
func (a Agent) Dir(root string) string { return filepath.Join(a.SkillsDir(root), Name) }

// ProjectMarkers identify a project root when walking upward from the working
// directory (§14).
var ProjectMarkers = []string{
	"payload.config.ts", "payload.config.js", "payload.config.mjs", "payload.config.mts",
	".git", "package.json",
}

// DetectProjectRoot walks upward from start looking for a project marker. The
// deepest match wins, so a nested package.json beats the repository root.
func DetectProjectRoot(start string) (string, bool) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", false
	}
	for {
		for _, marker := range ProjectMarkers {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return dir, true
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// File and target statuses.
const (
	// StatusWritten — the file was created or overwritten.
	StatusWritten = "written"
	// StatusUnchanged — the file already had the embedded content.
	StatusUnchanged = "unchanged"
	// StatusSkipped — the file (or agent) was deliberately left alone.
	StatusSkipped = "skipped"
	// StatusRemoved — the file was deleted by `pay skills uninstall`.
	StatusRemoved = "removed"
	// StatusMissing — nothing was installed there.
	StatusMissing = "missing"
)

// Warning codes this package emits.
const (
	// WarnFileModified — a user-edited file was left alone.
	WarnFileModified = "skill_file_modified"
	// WarnNotIgnored — the project's .gitignore does not cover the skill dir.
	WarnNotIgnored = "skill_dir_not_ignored"
	// WarnAgentSkipped — an agent was not installed into, and why.
	WarnAgentSkipped = "skill_agent_skipped"
	// WarnProjectContextUnavailable — discovery failed, so PROJECT.md was not
	// written (§14: it degrades gracefully).
	WarnProjectContextUnavailable = "skill_project_context_unavailable"
)

// FileResult is what happened to one file.
type FileResult struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// TargetResult is what happened at one install location.
type TargetResult struct {
	Agent  string       `json:"agent"`
	Dir    string       `json:"dir"`
	Status string       `json:"status"`
	Reason string       `json:"reason,omitempty"`
	Files  []FileResult `json:"files,omitempty"`
}

// Result is the envelope payload of `pay skills install` / `uninstall`.
type Result struct {
	Scope    Scope            `json:"scope"`
	Root     string           `json:"root"`
	Targets  []TargetResult   `json:"targets"`
	Skipped  []TargetResult   `json:"skipped"`
	Warnings []output.Warning `json:"-"`
	DryRun   bool             `json:"dry_run,omitempty"`
}

// Options drives Install, Uninstall and Status.
type Options struct {
	// Scope is "" for the §14 default (project when a marker is found by
	// walking up from StartDir, else user).
	Scope Scope
	// StartDir is the working directory used for project detection.
	StartDir string
	// ProjectRoot overrides detection.
	ProjectRoot string
	// Home is the user's home directory. It is injected rather than looked up
	// so the whole package is testable and env-free.
	Home string
	// Agents selects agents by name. Empty means the §14 default: every agent
	// whose Home directory already exists.
	Agents []string
	// AllAgents is --agent all: install into every known agent, creating the
	// directories.
	AllAgents bool
	// ExtraDirs are --dir PATH and skills.extra_dirs. Each is an agent's
	// skills directory; the skill lands in <dir>/pay.
	ExtraDirs []string
	// Force overwrites files the user edited.
	Force bool
	// DryRun computes the plan without touching the filesystem.
	DryRun bool

	// Project is references/PROJECT.md's content from --with-project-context.
	// Nil leaves any existing PROJECT.md alone.
	Project []byte

	// Manifest metadata.
	CLIVersion        string
	Now               time.Time
	Profile           string
	BaseURL           string
	DiscoveryRevision string
}

// resolveScope applies the §14 default: project when a marker is found walking
// upward, else user. The chosen scope is always reported in the envelope.
func (o Options) resolveScope() (Scope, string, error) {
	root := o.ProjectRoot
	if root == "" && o.StartDir != "" {
		if detected, ok := DetectProjectRoot(o.StartDir); ok {
			root = detected
		}
	}
	switch o.Scope {
	case ScopeProject:
		if root == "" {
			return "", "", apierr.New(apierr.CodeInvalidArgs,
				"no project root found: walked up from %q without finding %s.",
				o.StartDir, strings.Join(ProjectMarkers, ", ")).
				WithHint("run from inside the project, pass --dir PATH, or use --global to install for the current user")
		}
		return ScopeProject, root, nil
	case ScopeUser:
		if o.Home == "" {
			return "", "", apierr.New(apierr.CodeInvalidArgs, "the user's home directory is unknown.").
				WithHint("pass --dir PATH to name the skill directory explicitly")
		}
		return ScopeUser, o.Home, nil
	case "":
		if root != "" {
			return ScopeProject, root, nil
		}
		if o.Home == "" {
			return "", "", apierr.New(apierr.CodeInvalidArgs,
				"no project root found and the user's home directory is unknown.").
				WithHint("pass --dir PATH to name the skill directory explicitly")
		}
		return ScopeUser, o.Home, nil
	default:
		return "", "", apierr.New(apierr.CodeInvalidOption, "unknown scope %q.", o.Scope).
			WithDidYouMean(string(ScopeProject), string(ScopeUser))
	}
}

// target is one resolved install location.
type target struct {
	agent  string
	dir    string
	create bool // create the agent's home directory even when it is absent
}

// targets resolves the install locations, and returns the agents that were
// deliberately skipped with the reason (§14: skipped agents are reported).
func (o Options) targets(root string) ([]target, []TargetResult, error) {
	var out []target
	var skipped []TargetResult

	switch {
	case len(o.Agents) > 0:
		for _, name := range o.Agents {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if strings.EqualFold(name, "all") {
				for _, a := range Agents {
					out = append(out, target{agent: a.Name, dir: a.Dir(root), create: true})
				}
				continue
			}
			a, ok := FindAgent(name)
			if !ok {
				return nil, nil, apierr.New(apierr.CodeInvalidOption, "unknown agent %q.", name).
					WithDidYouMean(apierr.DidYouMean(name, AgentNames())...).
					WithHint("known agents: %s. Use --dir PATH for anything else.", strings.Join(AgentNames(), ", "))
			}
			// An explicitly named agent is installed into whether or not its
			// directory exists: the user asked for it.
			out = append(out, target{agent: a.Name, dir: a.Dir(root), create: true})
		}
	case o.AllAgents:
		for _, a := range Agents {
			out = append(out, target{agent: a.Name, dir: a.Dir(root), create: true})
		}
	default:
		for _, a := range Agents {
			if _, err := os.Stat(a.HomeDir(root)); err != nil {
				skipped = append(skipped, TargetResult{
					Agent:  a.Name,
					Dir:    a.Dir(root),
					Status: StatusSkipped,
					Reason: a.HomeDir(root) + " does not exist; PayCLI never creates a directory for an agent that is not used here",
				})
				continue
			}
			out = append(out, target{agent: a.Name, dir: a.Dir(root)})
		}
	}

	for _, dir := range o.ExtraDirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, nil, apierr.Wrap(err, apierr.CodeInvalidArgs, "--dir %q: %v", dir, err)
		}
		out = append(out, target{agent: "dir:" + abs, dir: filepath.Join(abs, Name), create: true})
	}

	if len(out) == 0 && len(skipped) == 0 {
		return nil, nil, apierr.New(apierr.CodeInvalidArgs, "no install targets were selected.").
			WithHint("pass --agent %s, --agent all, or --dir PATH", strings.Join(AgentNames(), "|"))
	}
	return out, skipped, nil
}

// payload is the full file set to install: the embedded documents plus, when
// requested, the generated PROJECT.md.
func (o Options) payload() []File {
	files := Files()
	if len(o.Project) > 0 {
		files = append(files, File{Path: ProjectFile, Data: o.Project, SHA256: Sum(o.Project)})
		sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	}
	return files
}

// Install copies the skill into every selected target. Files are copied, never
// symlinked: a symlink into a binary-relative path breaks on the next update.
func Install(opts Options) (*Result, error) {
	scope, root, err := opts.resolveScope()
	if err != nil {
		return nil, err
	}
	targets, skipped, err := opts.targets(root)
	if err != nil {
		return nil, err
	}

	res := &Result{Scope: scope, Root: root, Skipped: skipped, DryRun: opts.DryRun}
	for _, s := range skipped {
		res.Warnings = append(res.Warnings, output.Warning{
			Code:    WarnAgentSkipped,
			Message: fmt.Sprintf("skipped %s: %s", s.Agent, s.Reason),
			Hint:    fmt.Sprintf("pay skills install --agent %s installs it anyway", s.Agent),
		})
	}

	files := opts.payload()
	for _, t := range targets {
		tr, warns, err := opts.installTo(t, scope, files)
		if err != nil {
			return nil, err
		}
		res.Targets = append(res.Targets, tr)
		res.Warnings = append(res.Warnings, warns...)
	}

	if scope == ScopeProject {
		for _, t := range targets {
			if rel, err := filepath.Rel(root, t.dir); err == nil && !strings.HasPrefix(rel, "..") {
				covered, known := GitignoreCovers(root, filepath.ToSlash(rel))
				if known && !covered {
					res.Warnings = append(res.Warnings, output.Warning{
						Code: WarnNotIgnored,
						Message: fmt.Sprintf(
							"%s is not covered by %s, so the skill will be committed.", filepath.ToSlash(rel), filepath.Join(root, ".gitignore")),
						Hint: "that is usually fine and shares the skill with the team; add it to .gitignore if you would rather not commit it. PROJECT.md never contains credentials.",
					})
				}
			}
		}
	}
	return res, nil
}

// installTo writes the file set into one target directory.
func (o Options) installTo(t target, scope Scope, files []File) (TargetResult, []output.Warning, error) {
	tr := TargetResult{Agent: t.agent, Dir: t.dir, Status: StatusWritten}
	var warns []output.Warning

	manifest, err := LoadManifest(t.dir)
	if err != nil {
		return tr, nil, err
	}

	if !o.DryRun {
		if err := os.MkdirAll(t.dir, DirPerm); err != nil {
			return tr, nil, apierr.Wrap(err, apierr.CodeInternal, "cannot create %s: %v", t.dir, err)
		}
	}

	next := &Manifest{
		CLIVersion:        o.CLIVersion,
		InstalledAt:       o.Now.UTC().Format(time.RFC3339),
		Scope:             string(scope),
		Agents:            []string{t.agent},
		Profile:           o.Profile,
		BaseURL:           o.BaseURL,
		DiscoveryRevision: o.DiscoveryRevision,
		Files:             map[string]string{},
	}

	wroteAny, skippedAny := false, false
	for _, f := range files {
		dest := filepath.Join(t.dir, filepath.FromSlash(f.Path))
		action, status, reason := decide(dest, f.Data, manifest, f.Path, o.Force)

		switch status {
		case StatusWritten:
			wroteAny = true
		case StatusSkipped:
			skippedAny = true
			warns = append(warns, output.Warning{
				Code:    WarnFileModified,
				Message: dest + " was edited since it was installed and was left alone.",
				Paths:   []string{dest},
				Hint:    "pass --force to overwrite it, or move your edits into a separate file.",
			})
		}

		if action && !o.DryRun {
			if err := os.MkdirAll(filepath.Dir(dest), DirPerm); err != nil {
				return tr, nil, apierr.Wrap(err, apierr.CodeInternal, "cannot create %s: %v", filepath.Dir(dest), err)
			}
			if err := fsatomic.Write(dest, f.Data, FilePerm); err != nil {
				return tr, nil, apierr.Wrap(err, apierr.CodeInternal, "cannot write %s: %v", dest, err)
			}
		}
		// The manifest records what PayCLI believes is on disk. A skipped file
		// keeps its previously recorded hash so the next run still sees the
		// drift; a file with no record and no write is recorded as-is.
		switch {
		case action:
			next.Files[f.Path] = f.SHA256
		case status == StatusUnchanged:
			next.Files[f.Path] = f.SHA256
		default:
			if sum, ok := manifest.Recorded(f.Path); ok {
				next.Files[f.Path] = sum
			}
		}
		tr.Files = append(tr.Files, FileResult{Path: f.Path, Status: status, Reason: reason})
	}

	switch {
	case wroteAny:
		tr.Status = StatusWritten
	case skippedAny:
		tr.Status = StatusSkipped
		tr.Reason = "every file was locally modified; pass --force to overwrite"
	default:
		tr.Status = StatusUnchanged
	}

	// Carry forward every agent that previously shared this directory (an
	// extra-dir target can be the same directory as an agent's).
	if manifest != nil {
		next.Agents = mergeAgents(manifest.Agents, t.agent)
	}
	if !o.DryRun {
		if err := next.Save(t.dir); err != nil {
			return tr, nil, err
		}
	}
	return tr, warns, nil
}

// decide applies §14's reinstall rules to one file.
//
//	absent                        -> write   "written"
//	identical to the embedded copy -> no-op  "unchanged"
//	matches the manifest's hash    -> write  "written"   (a genuine upgrade)
//	differs from the manifest      -> skip   "skipped"   (the user edited it)
//	no manifest and differs        -> skip   "skipped"   (unknown provenance)
func decide(dest string, want []byte, m *Manifest, rel string, force bool) (write bool, status, reason string) {
	have, err := os.ReadFile(dest)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return true, StatusWritten, ""
		}
		// Unreadable: try to write over it and let the write report the real
		// error rather than guessing here.
		return true, StatusWritten, "existing file was unreadable"
	}
	haveSum := Sum(have)
	if haveSum == Sum(want) {
		return false, StatusUnchanged, ""
	}
	if recorded, ok := m.Recorded(rel); ok && recorded == haveSum {
		return true, StatusWritten, "updated from " + shortSum(recorded)
	}
	if force {
		return true, StatusWritten, "overwritten by --force"
	}
	if m == nil {
		return false, StatusSkipped, "not installed by pay (no " + ManifestName + "); pass --force to overwrite"
	}
	return false, StatusSkipped, "edited since install; pass --force to overwrite"
}

func shortSum(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func mergeAgents(existing []string, add string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(existing)+1)
	for _, a := range append(append([]string{}, existing...), add) {
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// Uninstall removes the skill from every selected target. Only files PayCLI
// recorded are removed, and only when they still match what it wrote — an
// edited file is left behind unless --force, exactly like install.
func Uninstall(opts Options) (*Result, error) {
	scope, root, err := opts.resolveScope()
	if err != nil {
		return nil, err
	}
	targets, _, err := opts.targets(root)
	if err != nil {
		return nil, err
	}

	res := &Result{Scope: scope, Root: root, DryRun: opts.DryRun}
	for _, t := range targets {
		tr := TargetResult{Agent: t.agent, Dir: t.dir, Status: StatusRemoved}
		manifest, err := LoadManifest(t.dir)
		if err != nil {
			return nil, err
		}
		if manifest == nil {
			if _, statErr := os.Stat(t.dir); statErr != nil {
				tr.Status = StatusMissing
				tr.Reason = "nothing installed here"
				res.Skipped = append(res.Skipped, tr)
				continue
			}
			if !opts.Force {
				tr.Status = StatusSkipped
				tr.Reason = "no " + ManifestName + "; pass --force to remove it anyway"
				res.Skipped = append(res.Skipped, tr)
				res.Warnings = append(res.Warnings, output.Warning{
					Code:    WarnFileModified,
					Message: t.dir + " was not installed by pay and was left alone.",
					Paths:   []string{t.dir},
					Hint:    "pass --force to remove it anyway.",
				})
				continue
			}
		}

		removedAny := false
		for rel, sum := range manifestFiles(manifest) {
			dest := filepath.Join(t.dir, filepath.FromSlash(rel))
			have, readErr := os.ReadFile(dest)
			if readErr != nil {
				tr.Files = append(tr.Files, FileResult{Path: rel, Status: StatusMissing})
				continue
			}
			if Sum(have) != sum && !opts.Force {
				tr.Files = append(tr.Files, FileResult{Path: rel, Status: StatusSkipped, Reason: "edited since install"})
				res.Warnings = append(res.Warnings, output.Warning{
					Code:    WarnFileModified,
					Message: dest + " was edited since it was installed and was left in place.",
					Paths:   []string{dest},
					Hint:    "pass --force to remove it anyway.",
				})
				continue
			}
			if !opts.DryRun {
				if err := os.Remove(dest); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return nil, apierr.Wrap(err, apierr.CodeInternal, "cannot remove %s: %v", dest, err)
				}
			}
			removedAny = true
			tr.Files = append(tr.Files, FileResult{Path: rel, Status: StatusRemoved})
		}
		sort.Slice(tr.Files, func(i, j int) bool { return tr.Files[i].Path < tr.Files[j].Path })

		if !opts.DryRun {
			if err := os.Remove(ManifestPath(t.dir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, apierr.Wrap(err, apierr.CodeInternal, "cannot remove %s: %v", ManifestPath(t.dir), err)
			}
			pruneEmpty(t.dir)
		}
		if !removedAny {
			tr.Status = StatusSkipped
		}
		res.Targets = append(res.Targets, tr)
	}
	return res, nil
}

func manifestFiles(m *Manifest) map[string]string {
	if m != nil && len(m.Files) > 0 {
		return m.Files
	}
	// No manifest (--force) or an empty one: fall back to the embedded set.
	out := map[string]string{}
	for _, f := range Files() {
		out[f.Path] = f.SHA256
	}
	out[ProjectFile] = ""
	return out
}

// pruneEmpty removes the skill directory and its now-empty subdirectories,
// leaving anything the user added untouched.
func pruneEmpty(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			pruneEmpty(filepath.Join(dir, e.Name()))
		}
	}
	if entries, err = os.ReadDir(dir); err == nil && len(entries) == 0 {
		_ = os.Remove(dir)
	}
}

// FileStatus is one file's drift state in `pay skills status`.
type FileStatus struct {
	Path string `json:"path"`
	// State is "current" | "outdated" | "modified" | "missing" | "unknown".
	State string `json:"state"`
}

// TargetStatus reports one install location.
type TargetStatus struct {
	Agent             string       `json:"agent"`
	Dir               string       `json:"dir"`
	Installed         bool         `json:"installed"`
	CLIVersion        string       `json:"cli_version,omitempty"`
	Stale             bool         `json:"stale"`
	Profile           string       `json:"profile,omitempty"`
	BaseURL           string       `json:"base_url,omitempty"`
	DiscoveryRevision string       `json:"discovery_revision,omitempty"`
	ProjectDocStale   bool         `json:"project_doc_stale"`
	Files             []FileStatus `json:"files,omitempty"`
}

// StatusReport is the payload of `pay skills status`. `pay doctor` uses the
// same data to say whether the skill is installed and stale.
type StatusReport struct {
	Scope   Scope          `json:"scope"`
	Root    string         `json:"root"`
	Targets []TargetStatus `json:"targets"`
}

// Status reports every install location, its recorded CLI version against the
// running binary, per-file drift, and whether PROJECT.md's discovery revision
// is older than the current one.
func Status(opts Options) (*StatusReport, error) {
	scope, root, err := opts.resolveScope()
	if err != nil {
		return nil, err
	}
	// Status inspects every known agent, not just the ones that would be
	// installed into: an agent that was installed into and then abandoned still
	// has a stale copy of the skill.
	all := opts

	all.Agents = nil
	all.AllAgents = true
	targets, _, err := all.targets(root)
	if err != nil {
		return nil, err
	}

	rep := &StatusReport{Scope: scope, Root: root}
	embedded := map[string]string{}
	for _, f := range Files() {
		embedded[f.Path] = f.SHA256
	}

	for _, t := range targets {
		ts := TargetStatus{Agent: t.agent, Dir: t.dir}
		manifest, err := LoadManifest(t.dir)
		if err != nil {
			return nil, err
		}
		if manifest == nil {
			if _, statErr := os.Stat(t.dir); statErr != nil {
				continue // nothing here at all: not worth reporting
			}
		} else {
			ts.Installed = true
			ts.CLIVersion = manifest.CLIVersion
			ts.Profile = manifest.Profile
			ts.BaseURL = redact.URL(manifest.BaseURL)
			ts.DiscoveryRevision = manifest.DiscoveryRevision
			ts.Stale = opts.CLIVersion != "" && manifest.CLIVersion != opts.CLIVersion
			ts.ProjectDocStale = opts.DiscoveryRevision != "" &&
				manifest.DiscoveryRevision != "" &&
				manifest.DiscoveryRevision < opts.DiscoveryRevision
		}

		for _, rel := range sortedKeys(embedded) {
			ts.Files = append(ts.Files, FileStatus{Path: rel, State: fileState(t.dir, rel, embedded[rel], manifest)})
		}
		if _, err := os.Stat(filepath.Join(t.dir, filepath.FromSlash(ProjectFile))); err == nil {
			ts.Files = append(ts.Files, FileStatus{Path: ProjectFile, State: projectState(ts)})
		}
		rep.Targets = append(rep.Targets, ts)
	}
	return rep, nil
}

func projectState(ts TargetStatus) string {
	if ts.ProjectDocStale {
		return "outdated"
	}
	return "current"
}

func fileState(dir, rel, want string, m *Manifest) string {
	have, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		return StatusMissing
	}
	sum := Sum(have)
	switch {
	case sum == want:
		return "current"
	case m == nil:
		return "unknown"
	default:
		if recorded, ok := m.Recorded(rel); ok && recorded == sum {
			return "outdated"
		}
		return "modified"
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// GitignoreCovers reports whether the project's .gitignore covers rel (a
// slash-separated path relative to the project root). The second return value
// is false when there is no .gitignore to consult, in which case no warning is
// warranted.
//
// This is a deliberately simple prefix/segment matcher, not a gitignore engine:
// it decides whether to print an informational warning, and a false negative
// there costs nothing.
func GitignoreCovers(root, rel string) (covered, known bool) {
	f, err := os.Open(filepath.Join(root, ".gitignore"))
	if err != nil {
		return false, false
	}
	defer f.Close()

	rel = strings.TrimPrefix(filepath.ToSlash(rel), "./")
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		pattern := strings.Trim(line, "/")
		if pattern == "" {
			continue
		}
		if pattern == "*" || rel == pattern || strings.HasPrefix(rel, pattern+"/") {
			return true, true
		}
		// A bare name with no slash matches at any depth, like git does.
		if !strings.Contains(pattern, "/") {
			for _, seg := range strings.Split(rel, "/") {
				if seg == pattern {
					return true, true
				}
			}
		}
	}
	return false, true
}
