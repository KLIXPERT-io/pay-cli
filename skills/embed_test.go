package skills

import (
	"bytes"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestEmbeddedTreeIsTheOnDiskTree is the structural guarantee that replaced
// §1 conflict 18's "never create a top-level skills/ directory" rule.
//
// The old rule existed because a second copy of the skill would drift from the
// binary it documents. The copy is gone instead of the directory: this package
// lives *inside* skills/, so //go:embed all:pay embeds the very directory a
// reader of the repository (and `npx skills add`) sees. This test asserts that
// relationship rather than trusting it — if someone ever repoints the embed at
// a vendored or generated copy, the two trees stop being the same bytes and
// this fails.
func TestEmbeddedTreeIsTheOnDiskTree(t *testing.T) {
	embedded := map[string][]byte{}
	if err := fs.WalkDir(FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, readErr := fs.ReadFile(FS(), p)
		if readErr != nil {
			return readErr
		}
		embedded[p] = b
		return nil
	}); err != nil {
		t.Fatalf("walking the embedded tree: %v", err)
	}
	if len(embedded) == 0 {
		t.Fatal("the embedded skill tree is empty")
	}

	// Dir is a plain relative path from this package's directory, which is what
	// `go test` runs in. No embed involved: this is the real filesystem.
	onDisk := map[string][]byte{}
	if err := filepath.WalkDir(Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(Dir, p)
		if relErr != nil {
			return relErr
		}
		b, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		onDisk[filepath.ToSlash(rel)] = b
		return nil
	}); err != nil {
		t.Fatalf("walking %s on disk: %v", Dir, err)
	}

	for _, p := range sortedKeys(embedded) {
		want, ok := onDisk[p]
		if !ok {
			t.Errorf("%s is embedded but does not exist at skills/%s/%s", p, Dir, p)
			continue
		}
		if !bytes.Equal(embedded[p], want) {
			t.Errorf("skills/%s/%s differs from the copy compiled into the binary (%d vs %d bytes)",
				Dir, p, len(want), len(embedded[p]))
		}
	}
	for _, p := range sortedKeys(onDisk) {
		if _, ok := embedded[p]; !ok {
			t.Errorf("skills/%s/%s exists on disk but is not embedded; `pay skills install` would not ship it", Dir, p)
		}
	}
}

// TestExactlyOneCopyOfTheSkill walks the whole repository and fails if a second
// SKILL.md appears anywhere. "One copy" is the invariant; a duplicated tree
// under internal/, a generated mirror or a stale assets/ directory would all
// reintroduce exactly the drift conflict 18 was protecting against.
func TestExactlyOneCopyOfTheSkill(t *testing.T) {
	repoRoot := ".."
	if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err != nil {
		t.Skipf("not running from the source tree: %v", err)
	}

	var found []string
	err := filepath.WalkDir(repoRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable directory is not a duplicate
		}
		if d.IsDir() {
			// Hidden directories are skipped wholesale, and that is not
			// cosmetic: `pay skills install` run inside this repository writes
			// .claude/skills/pay/SKILL.md, which is a legitimate *install*, not
			// a second source copy. .gitignore excludes them for the same
			// reason. node_modules/dist/vendor are build output.
			if p != repoRoot && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			switch d.Name() {
			case "node_modules", "dist", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "SKILL.md" {
			return nil
		}
		rel, relErr := filepath.Rel(repoRoot, p)
		if relErr != nil {
			rel = p
		}
		found = append(found, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}

	want := path.Join("skills", Dir, "SKILL.md")
	sort.Strings(found)
	if len(found) != 1 || found[0] != want {
		t.Fatalf("the repository must contain exactly one SKILL.md (%s), found:\n  %s",
			want, strings.Join(found, "\n  "))
	}
}

func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
