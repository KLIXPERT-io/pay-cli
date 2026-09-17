package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTree materialises a map of relative path -> contents under a temp dir.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestFindProjectWalksUp(t *testing.T) {
	root := writeTree(t, map[string]string{
		"package.json":             `{"dependencies":{"payload":"3.86.0"}}`,
		"pay.toml":                 "default_profile = \"local\"\n",
		"src/payload.config.ts":    "export default buildConfig({})",
		".git/HEAD":                "ref: refs/heads/main\n",
		"src/collections/Pages.ts": "export const Pages = {}",
	})

	p := FindProject(filepath.Join(root, "src", "collections"))
	if p.Dir != root {
		t.Errorf("Dir = %q, want %q", p.Dir, root)
	}
	if p.ConfigPath != filepath.Join(root, "pay.toml") {
		t.Errorf("ConfigPath = %q", p.ConfigPath)
	}
	if p.PackageJSON != filepath.Join(root, "package.json") {
		t.Errorf("PackageJSON = %q", p.PackageJSON)
	}
	if p.PayloadConfig != filepath.Join(root, "src", "payload.config.ts") {
		t.Errorf("PayloadConfig = %q", p.PayloadConfig)
	}
	if p.SrcDir != filepath.Join(root, "src") {
		t.Errorf("SrcDir = %q", p.SrcDir)
	}
	if p.GitRoot != root {
		t.Errorf("GitRoot = %q", p.GitRoot)
	}
	if !p.Found() || !p.IsPayload() {
		t.Error("Found/IsPayload = false")
	}
}

func TestFindProjectStopsAtGitRoot(t *testing.T) {
	root := writeTree(t, map[string]string{
		"pay.toml":          "",
		"repo/.git/HEAD":    "ref: refs/heads/main\n",
		"repo/package.json": `{"name":"inner"}`,
		"repo/src/index.ts": "",
	})
	p := FindProject(filepath.Join(root, "repo", "src"))
	if p.GitRoot != filepath.Join(root, "repo") {
		t.Fatalf("GitRoot = %q", p.GitRoot)
	}
	if p.ConfigPath != "" {
		t.Errorf("the walk must stop at the git root, but it found %q", p.ConfigPath)
	}
}

func TestFindProjectFindsNothing(t *testing.T) {
	p := FindProject(t.TempDir())
	if p.Found() || p.IsPayload() {
		t.Errorf("empty dir looks like a project: %+v", p)
	}
	f, err := p.LoadConfig()
	if err != nil || f.Exists {
		t.Errorf("LoadConfig on a bare dir = %+v, %v", f, err)
	}
	if FindProject("").Found() {
		t.Error("an empty start must be handled")
	}
}

func TestProjectLoadConfig(t *testing.T) {
	root := writeTree(t, map[string]string{
		"pay.toml": "default_profile = \"proj\"\n[profiles.proj]\nbase_url = \"http://p.test\"\n",
	})
	p := FindProject(root)
	f, err := p.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if f.Kind != KindProject || f.Source() != "project:"+filepath.Join(root, "pay.toml") {
		t.Errorf("Source() = %q", f.Source())
	}
	if f.DefaultProfile != "proj" {
		t.Errorf("default_profile = %q", f.DefaultProfile)
	}
}

func TestGitignoreCovers(t *testing.T) {
	root := writeTree(t, map[string]string{
		".git/HEAD":  "ref: refs/heads/main\n",
		".gitignore": "# comment\nnode_modules\n/.claude/skills/\n",
	})
	p := FindProject(root)
	if !p.GitignoreCovers("node_modules") {
		t.Error("node_modules should be covered")
	}
	if !p.GitignoreCovers(".claude/skills") {
		t.Error("a rooted directory pattern should be covered")
	}
	if p.GitignoreCovers("credentials.json") {
		t.Error("false positive")
	}
}
