package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/skills"
)

func TestSkillsListNamesEveryEmbeddedFile(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	res := opsRun(t, home, "skills", "list")
	if res.Code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr: %s", res.Code, res.Stderr)
	}
	data := res.data(t)
	files, ok := data["files"].([]any)
	if !ok {
		t.Fatalf("files = %#v, want a list", data["files"])
	}
	if len(files) != len(skills.Files()) {
		t.Fatalf("listed %d files, embedded %d", len(files), len(skills.Files()))
	}
	seen := map[string]bool{}
	for _, f := range files {
		m, ok := f.(map[string]any)
		if !ok {
			t.Fatalf("file entry = %T, want an object", f)
		}
		path, _ := m["path"].(string)
		sha, _ := m["sha256"].(string)
		if path == "" || len(sha) != 64 {
			t.Fatalf("file entry %#v is missing a path or a sha256", m)
		}
		seen[path] = true
	}
	if !seen[skills.SkillFile] {
		t.Fatalf("SKILL.md is not listed: %v", seen)
	}
}

func TestSkillsPrintRawIsTheFileVerbatim(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	want, err := skills.Read(skills.SkillFile)
	if err != nil {
		t.Fatalf("skills.Read: %v", err)
	}
	res := opsRun(t, home, "skills", "print", "--output", "raw")
	if res.Code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr: %s", res.Code, res.Stderr)
	}
	// The renderer guarantees the stream ends in a newline; the file itself may
	// not. Everything before that must be byte-identical.
	if strings.TrimRight(res.Stdout, "\n") != strings.TrimRight(string(want), "\n") {
		t.Fatalf("`pay skills print --output raw` did not reproduce SKILL.md byte for byte\n"+
			"got %d bytes, want %d", len(res.Stdout), len(want))
	}
}

func TestSkillsPrintUnknownNameSuggests(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	res := opsRun(t, home, "skills", "print", "references/gotcha.md")
	if res.Code == 0 {
		t.Fatalf("exit = 0, want a failure\n%s", res.Stdout)
	}
	e, ok := res.Envelope["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error object in the envelope\n%s", res.Stdout)
	}
	suggestions, _ := e["did_you_mean"].([]any)
	if len(suggestions) == 0 {
		t.Fatalf("did_you_mean is empty; an agent has no way back\n%s", res.Stdout)
	}
}

func TestSkillsInstallIntoAProjectScope(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	// A .claude directory that already exists is what the default install
	// targets; an agent whose directory is absent must be left alone.
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	res := opsRun(t, home, "skills", "install", "--agent", "claude")
	if res.Code != 0 {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", res.Code, res.Stdout, res.Stderr)
	}
	installed := filepath.Join(home, ".claude", "skills", "pay", skills.SkillFile)
	if _, err := os.Stat(installed); err != nil {
		t.Fatalf("SKILL.md was not installed at %s: %v\n%s", installed, err, res.Stdout)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "skills", "pay")); !os.IsNotExist(err) {
		t.Fatalf("an agent with no home directory was created anyway")
	}

	// A second run is a no-op, not a rewrite.
	again := opsRun(t, home, "skills", "install", "--agent", "claude")
	if again.Code != 0 {
		t.Fatalf("reinstall exit = %d, want 0\n%s", again.Code, again.Stdout)
	}
	if !strings.Contains(again.Stdout, skills.StatusUnchanged) {
		t.Fatalf("reinstall did not report any file as %q\n%s", skills.StatusUnchanged, again.Stdout)
	}
}

func TestSkillsInstallSkipsAnEditedFile(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if res := opsRun(t, home, "skills", "install", "--agent", "claude"); res.Code != 0 {
		t.Fatalf("install exit = %d\n%s", res.Code, res.Stdout)
	}

	edited := filepath.Join(home, ".claude", "skills", "pay", skills.SkillFile)
	if err := os.WriteFile(edited, []byte("# my own notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := opsRun(t, home, "skills", "install", "--agent", "claude")
	if res.Code != 0 {
		t.Fatalf("exit = %d, want 0 (a skipped file is not a failure)\n%s", res.Code, res.Stdout)
	}
	got, err := os.ReadFile(edited)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "# my own notes\n" {
		t.Fatalf("an edited SKILL.md was overwritten without --force")
	}

	forced := opsRun(t, home, "skills", "install", "--agent", "claude", "--force")
	if forced.Code != 0 {
		t.Fatalf("--force exit = %d\n%s", forced.Code, forced.Stdout)
	}
	got, err = os.ReadFile(edited)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) == "# my own notes\n" {
		t.Fatalf("--force did not restore the embedded SKILL.md")
	}
}

func TestSkillsStatusOnAnEmptyTree(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	res := opsRun(t, home, "skills", "status")
	if res.Code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr: %s", res.Code, res.Stderr)
	}
	data := res.data(t)
	n, _ := data["installed_count"].(json.Number)
	if n.String() != "0" {
		t.Fatalf("installed_count = %s, want 0", n)
	}
}

func TestSkillsUninstallRemovesOnlyWhatWasInstalled(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if res := opsRun(t, home, "skills", "install", "--agent", "claude"); res.Code != 0 {
		t.Fatalf("install exit = %d\n%s", res.Code, res.Stdout)
	}
	dir := filepath.Join(home, ".claude", "skills", "pay")
	mine := filepath.Join(dir, "NOTES.md")
	if err := os.WriteFile(mine, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := opsRun(t, home, "skills", "uninstall", "--agent", "claude")
	if res.Code != 0 {
		t.Fatalf("uninstall exit = %d\n%s", res.Code, res.Stdout)
	}
	if _, err := os.Stat(filepath.Join(dir, skills.SkillFile)); !os.IsNotExist(err) {
		t.Fatalf("SKILL.md survived uninstall")
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatalf("uninstall deleted a file PayCLI did not install: %v", err)
	}
}

func TestSkillsInstallRejectsAnUnknownAgent(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	res := opsRun(t, home, "skills", "install", "--agent", "claud")
	if code := res.errorCode(t); code != "invalid_option" {
		t.Fatalf("error.code = %q, want invalid_option\n%s", code, res.Stdout)
	}
	if res.Code != 5 {
		t.Fatalf("exit = %d, want 5", res.Code)
	}
}

func TestSkillsFlagsOptionsScope(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		flags   skillsFlags
		want    skills.Scope
		wantErr bool
	}{
		{"default is detection", skillsFlags{}, "", false},
		{"--global is user", skillsFlags{global: true}, skills.ScopeUser, false},
		{"--scope project wins", skillsFlags{global: true, scope: "project"}, skills.ScopeProject, false},
		{"--scope user", skillsFlags{scope: "USER"}, skills.ScopeUser, false},
		{"--scope garbage", skillsFlags{scope: "global"}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := tc.flags
			opts, err := f.options(&Runtime{App: App{Now: func() time.Time { return opsClock }}})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("options() = %+v, want an error", opts)
				}
				return
			}
			if err != nil {
				t.Fatalf("options() = %v", err)
			}
			if opts.Scope != tc.want {
				t.Fatalf("scope = %q, want %q", opts.Scope, tc.want)
			}
		})
	}
}

func TestCollectionFeaturesOnlyReportsKnownTrue(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	c := &discovery.Collection{
		Slug: "pages",
		Flags: discovery.Flags{
			Upload:   &no,  // known false -> not listed
			Drafts:   &yes, // known true  -> listed
			Versions: nil,  // never learned -> not listed, and not denied
		},
	}
	got := collectionFeatures(c)
	if len(got) != 1 || got[0] != "drafts" {
		t.Fatalf("collectionFeatures = %v, want [drafts]", got)
	}
}

func TestCollectionOps(t *testing.T) {
	t.Parallel()
	c := &discovery.Collection{
		Permissions: discovery.Permissions{Create: true, Read: true, Update: false, Delete: true},
	}
	got := collectionOps(c)
	want := []string{"create", "read", "delete"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("collectionOps = %v, want %v", got, want)
	}
}

func TestKeyFieldsOf(t *testing.T) {
	t.Parallel()
	title := "title"
	shard := &discovery.Shard{
		Slug:          "pages",
		RequiredPaths: []string{"title", "slug", "hero.type", "publishedAt", "a", "b", "c", "d"},
	}
	got := keyFieldsOf(shard, &title)
	if len(got) > 6 {
		t.Fatalf("keyFieldsOf returned %d fields, want at most 6: %v", len(got), got)
	}
	if got[0] != "title" {
		t.Fatalf("keyFieldsOf[0] = %q, want the title field first", got[0])
	}
	for _, f := range got {
		if strings.Contains(f, ".") {
			t.Fatalf("keyFieldsOf returned a nested path %q", f)
		}
	}
	// No duplicates: "title" is both the title field and a required path.
	seen := map[string]bool{}
	for _, f := range got {
		if seen[f] {
			t.Fatalf("keyFieldsOf duplicated %q: %v", f, got)
		}
		seen[f] = true
	}
}
