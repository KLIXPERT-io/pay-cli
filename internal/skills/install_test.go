package skills

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

func testNow() time.Time { return time.Date(2026, 9, 16, 17, 12, 0, 0, time.UTC) }

func baseOptions(t *testing.T, root string) Options {
	t.Helper()
	return Options{
		Scope:             ScopeProject,
		ProjectRoot:       root,
		Home:              root,
		StartDir:          root,
		CLIVersion:        "0.1.0",
		Now:               testNow(),
		Profile:           "dev",
		BaseURL:           "http://localhost:3900",
		DiscoveryRevision: "2026-09-16T17:00:00Z",
	}
}

func TestDetectProjectRoot(t *testing.T) {
	tests := []struct {
		name   string
		marker string
	}{
		{"payload config", "payload.config.ts"},
		{"git", ".git"},
		{"package.json", "package.json"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, tc.marker), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			nested := filepath.Join(root, "src", "app")
			if err := os.MkdirAll(nested, 0o755); err != nil {
				t.Fatal(err)
			}
			got, ok := DetectProjectRoot(nested)
			if !ok {
				t.Fatal("DetectProjectRoot found nothing")
			}
			// t.TempDir can be a symlinked path (/var → /private/var on macOS).
			wantResolved, _ := filepath.EvalSymlinks(root)
			gotResolved, _ := filepath.EvalSymlinks(got)
			if gotResolved != wantResolved {
				t.Fatalf("root = %q, want %q", got, root)
			}
		})
	}
}

func TestResolveScope(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	bare := t.TempDir()

	tests := []struct {
		name      string
		opts      Options
		wantScope Scope
		wantErr   bool
	}{
		{"auto picks project when a marker exists", Options{StartDir: root, Home: home}, ScopeProject, false},
		{"auto falls back to user", Options{StartDir: filepath.Join(bare, "nope"), Home: home}, ScopeUser, false},
		{"explicit user", Options{Scope: ScopeUser, StartDir: root, Home: home}, ScopeUser, false},
		{"explicit project with no root", Options{Scope: ScopeProject, StartDir: string(filepath.Separator)}, "", true},
		{"unknown scope", Options{Scope: "global", Home: home}, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scope, _, err := tc.opts.resolveScope()
			if tc.wantErr {
				if err == nil {
					t.Fatal("resolveScope() = nil error, want one")
				}
				if apierr.ExitCode(err) != apierr.ExitValidation {
					t.Fatalf("exit = %d, want 5", apierr.ExitCode(err))
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveScope() = %v", err)
			}
			if scope != tc.wantScope {
				t.Fatalf("scope = %q, want %q", scope, tc.wantScope)
			}
		})
	}
}

func TestInstallOnlyTouchesExistingAgents(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := Install(baseOptions(t, root))
	if err != nil {
		t.Fatalf("Install() = %v", err)
	}
	if len(res.Targets) != 1 || res.Targets[0].Agent != "claude" {
		t.Fatalf("targets = %+v, want only claude", res.Targets)
	}
	if len(res.Skipped) != len(Agents)-1 {
		t.Fatalf("skipped %d agents, want %d", len(res.Skipped), len(Agents)-1)
	}
	for _, a := range Agents[1:] {
		if _, err := os.Stat(a.HomeDir(root)); err == nil {
			t.Fatalf("install created %s for an agent that is not used here", a.HomeDir(root))
		}
	}

	dir := filepath.Join(root, ".claude", "skills", "pay")
	for _, name := range wantFiles {
		p := filepath.Join(dir, filepath.FromSlash(name))
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != FilePerm {
			t.Errorf("%s mode = %v, want %v", name, info.Mode().Perm(), FilePerm)
		}
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != DirPerm {
			t.Errorf("dir mode = %v, want %v", info.Mode().Perm(), DirPerm)
		}
	}

	m, err := LoadManifest(dir)
	if err != nil || m == nil {
		t.Fatalf("LoadManifest() = %v, %v", m, err)
	}
	if m.CLIVersion != "0.1.0" || m.Scope != string(ScopeProject) || m.Profile != "dev" {
		t.Fatalf("manifest = %+v", m)
	}
	if len(m.Files) != len(wantFiles) {
		t.Fatalf("manifest files = %v", m.Files)
	}
	if m.InstalledAt != "2026-09-16T17:12:00Z" {
		t.Fatalf("installed_at = %q", m.InstalledAt)
	}
}

func TestInstallReinstallSkipsAndForces(t *testing.T) {
	root := t.TempDir()
	opts := baseOptions(t, root)
	opts.Agents = []string{"claude"}

	if _, err := Install(opts); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".claude", "skills", "pay")

	// An untouched reinstall reports everything unchanged and rewrites nothing.
	res, err := Install(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Targets[0].Status != StatusUnchanged {
		t.Fatalf("status = %q, want unchanged", res.Targets[0].Status)
	}

	// A user edit is preserved, with a warning.
	edited := filepath.Join(dir, "references", "gotchas.md")
	if err := os.WriteFile(edited, []byte("my own notes\n"), FilePerm); err != nil {
		t.Fatal(err)
	}
	res, err = Install(opts)
	if err != nil {
		t.Fatal(err)
	}
	var got FileResult
	for _, f := range res.Targets[0].Files {
		if f.Path == "references/gotchas.md" {
			got = f
		}
	}
	if got.Status != StatusSkipped {
		t.Fatalf("edited file status = %q, want skipped", got.Status)
	}
	if b, _ := os.ReadFile(edited); string(b) != "my own notes\n" {
		t.Fatal("the user's edit was overwritten")
	}
	if len(res.Warnings) == 0 || res.Warnings[0].Code != WarnFileModified {
		t.Fatalf("warnings = %+v, want %s", res.Warnings, WarnFileModified)
	}

	// --force overwrites it.
	opts.Force = true
	if _, err := Install(opts); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(edited)
	want, _ := Read("references/gotchas.md")
	if string(b) != string(want) {
		t.Fatal("--force did not restore the embedded content")
	}
}

func TestInstallUpgradesAFileItWrote(t *testing.T) {
	root := t.TempDir()
	opts := baseOptions(t, root)
	opts.Agents = []string{"claude"}
	if _, err := Install(opts); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".claude", "skills", "pay")

	// Simulate a previous CLI version having written different bytes: the
	// manifest records the hash of what is on disk, so it is not a user edit.
	stale := []byte("an older release's SKILL.md\n")
	target := filepath.Join(dir, SkillFile)
	if err := os.WriteFile(target, stale, FilePerm); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	m.Files[SkillFile] = Sum(stale)
	if err := m.Save(dir); err != nil {
		t.Fatal(err)
	}

	res, err := Install(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Targets[0].Status != StatusWritten {
		t.Fatalf("status = %q, want written", res.Targets[0].Status)
	}
	b, _ := os.ReadFile(target)
	want, _ := Read(SkillFile)
	if string(b) != string(want) {
		t.Fatal("a pay-written file was not upgraded")
	}
}

func TestInstallDryRunTouchesNothing(t *testing.T) {
	root := t.TempDir()
	opts := baseOptions(t, root)
	opts.Agents = []string{"claude"}
	opts.DryRun = true

	res, err := Install(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || len(res.Targets) != 1 {
		t.Fatalf("result = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, ".claude")); err == nil {
		t.Fatal("--dry-run created files")
	}
}

func TestInstallWithProjectContextAndExtraDir(t *testing.T) {
	root := t.TempDir()
	extra := filepath.Join(t.TempDir(), "agents")
	opts := baseOptions(t, root)
	opts.Agents = []string{"claude"}
	opts.ExtraDirs = []string{extra}
	opts.Project = []byte("# PROJECT\n")

	res, err := Install(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Targets) != 2 {
		t.Fatalf("targets = %+v, want claude + the extra dir", res.Targets)
	}
	for _, dir := range []string{
		filepath.Join(root, ".claude", "skills", "pay"),
		filepath.Join(extra, "pay"),
	} {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ProjectFile)))
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if string(b) != "# PROJECT\n" {
			t.Errorf("%s PROJECT.md = %q", dir, b)
		}
	}
}

func TestInstallUnknownAgent(t *testing.T) {
	root := t.TempDir()
	opts := baseOptions(t, root)
	opts.Agents = []string{"claud"}
	_, err := Install(opts)
	if err == nil {
		t.Fatal("Install() = nil, want invalid_option")
	}
	if apierr.CodeOf(err) != apierr.CodeInvalidOption {
		t.Fatalf("code = %s, want invalid_option", apierr.CodeOf(err))
	}
	e, _ := apierr.As(err)
	if len(e.DidYouMean) == 0 || e.DidYouMean[0] != "claude" {
		t.Fatalf("did_you_mean = %v, want claude", e.DidYouMean)
	}
}

func TestInstallAllAgentsCreatesDirectories(t *testing.T) {
	root := t.TempDir()
	opts := baseOptions(t, root)
	opts.AllAgents = true
	res, err := Install(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Targets) != len(Agents) {
		t.Fatalf("targets = %d, want %d", len(res.Targets), len(Agents))
	}
	for _, a := range Agents {
		if _, err := os.Stat(filepath.Join(a.Dir(root), SkillFile)); err != nil {
			t.Errorf("%s: %v", a.Name, err)
		}
	}
}

func TestUninstall(t *testing.T) {
	root := t.TempDir()
	opts := baseOptions(t, root)
	opts.Agents = []string{"claude"}
	if _, err := Install(opts); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".claude", "skills", "pay")

	res, err := Uninstall(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Targets[0].Status != StatusRemoved {
		t.Fatalf("status = %q, want removed", res.Targets[0].Status)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("the skill directory survived uninstall")
	}

	// A second uninstall is a no-op, not an error.
	res, err = Uninstall(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Status != StatusMissing {
		t.Fatalf("second uninstall = %+v", res)
	}
}

func TestUninstallKeepsEditedFiles(t *testing.T) {
	root := t.TempDir()
	opts := baseOptions(t, root)
	opts.Agents = []string{"claude"}
	if _, err := Install(opts); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".claude", "skills", "pay")
	edited := filepath.Join(dir, SkillFile)
	if err := os.WriteFile(edited, []byte("mine\n"), FilePerm); err != nil {
		t.Fatal(err)
	}

	if _, err := Uninstall(opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(edited); err != nil {
		t.Fatal("uninstall removed a file the user had edited")
	}
}

func TestStatus(t *testing.T) {
	root := t.TempDir()
	opts := baseOptions(t, root)
	opts.Agents = []string{"claude"}
	if _, err := Install(opts); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".claude", "skills", "pay")

	// A newer binary against an older install.
	statusOpts := baseOptions(t, root)
	statusOpts.CLIVersion = "0.2.0"
	statusOpts.DiscoveryRevision = "2026-10-01T00:00:00Z"

	rep, err := Status(statusOpts)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Targets) != 1 {
		t.Fatalf("targets = %+v, want only the installed one", rep.Targets)
	}
	ts := rep.Targets[0]
	if !ts.Installed || !ts.Stale {
		t.Fatalf("target = %+v, want installed and stale", ts)
	}
	if !ts.ProjectDocStale {
		t.Fatal("project doc should be reported stale")
	}
	for _, f := range ts.Files {
		if f.State != "current" {
			t.Errorf("%s = %q, want current", f.Path, f.State)
		}
	}

	if err := os.WriteFile(filepath.Join(dir, SkillFile), []byte("mine\n"), FilePerm); err != nil {
		t.Fatal(err)
	}
	rep, err = Status(statusOpts)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range rep.Targets[0].Files {
		if f.Path == SkillFile && f.State != "modified" {
			t.Fatalf("%s = %q, want modified", f.Path, f.State)
		}
	}
}

func TestGitignoreCovers(t *testing.T) {
	tests := []struct {
		name        string
		gitignore   string
		rel         string
		wantCovered bool
		wantKnown   bool
	}{
		{"no gitignore", "", ".claude/skills/pay", false, false},
		{"exact directory", ".claude/\n", ".claude/skills/pay", true, true},
		{"exact path", ".claude/skills/pay\n", ".claude/skills/pay", true, true},
		{"bare name at any depth", "skills\n", ".claude/skills/pay", true, true},
		{"unrelated", "node_modules\ndist\n", ".claude/skills/pay", false, true},
		{"comments and negations ignored", "# .claude\n!.claude\n", ".claude/skills/pay", false, true},
		{"catch-all", "*\n", ".claude/skills/pay", true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.gitignore != "" {
				if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(tc.gitignore), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			covered, known := GitignoreCovers(root, tc.rel)
			if covered != tc.wantCovered || known != tc.wantKnown {
				t.Fatalf("GitignoreCovers() = (%v, %v), want (%v, %v)", covered, known, tc.wantCovered, tc.wantKnown)
			}
		})
	}
}

func TestInstallWarnsWhenNotGitignored(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("node_modules\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := baseOptions(t, root)
	opts.Agents = []string{"claude"}
	res, err := Install(opts)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, w := range res.Warnings {
		if w.Code == WarnNotIgnored {
			found = true
			if strings.Contains(w.Message, "API-Key") {
				t.Errorf("warning leaks a credential form: %q", w.Message)
			}
		}
	}
	if !found {
		t.Fatalf("warnings = %+v, want %s", res.Warnings, WarnNotIgnored)
	}
}

func TestManifestRedactsBaseURL(t *testing.T) {
	dir := t.TempDir()
	m := &Manifest{
		CLIVersion: "0.1.0",
		BaseURL:    "https://admin:hunter2@example.com",
		Files:      map[string]string{"SKILL.md": "abc"},
	}
	if err := m.Save(dir); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(ManifestPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "hunter2") {
		t.Fatalf("manifest leaked userinfo:\n%s", b)
	}
	got, err := LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseURL != "https://example.com" {
		t.Fatalf("base_url = %q", got.BaseURL)
	}
}

func TestLoadManifestCorrupt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(ManifestPath(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadManifest(dir)
	if apierr.CodeOf(err) != apierr.CodeCacheCorrupt {
		t.Fatalf("code = %s, want cache_corrupt", apierr.CodeOf(err))
	}
}
