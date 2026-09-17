package skills

import (
	"strings"
	"testing"
)

// wantFiles is the §14 asset tree. A missing file is a shipping bug: the skill
// links to each of these by name.
var wantFiles = []string{
	"SKILL.md",
	"references/errors.md",
	"references/gotchas.md",
	"references/query-syntax.md",
	"references/recipes.md",
}

func TestEmbeddedFiles(t *testing.T) {
	got := Names()
	if len(got) != len(wantFiles) {
		t.Fatalf("embedded files = %v, want %v", got, wantFiles)
	}
	for i, want := range wantFiles {
		if got[i] != want {
			t.Errorf("file %d = %q, want %q", i, got[i], want)
		}
	}
	for _, f := range Files() {
		if len(f.Data) == 0 {
			t.Errorf("%s is empty", f.Path)
		}
		if len(f.SHA256) != 64 {
			t.Errorf("%s has digest %q, want 64 hex chars", f.Path, f.SHA256)
		}
	}
}

func TestSkillFrontmatter(t *testing.T) {
	b, err := Read(SkillFile)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.HasPrefix(s, "---\n") {
		t.Fatal("SKILL.md must start with YAML frontmatter")
	}
	end := strings.Index(s[4:], "\n---\n")
	if end < 0 {
		t.Fatal("SKILL.md frontmatter is not terminated")
	}
	front := s[4 : 4+end]
	if !strings.Contains(front, "name: payload-cli") {
		t.Errorf("frontmatter name is wrong:\n%s", front)
	}
	if !strings.Contains(front, "description: Read and write any Payload CMS 3.x project") {
		t.Errorf("frontmatter description is wrong:\n%s", front)
	}
	for _, want := range []string{"payload.config.ts", "Payload collection", "Payload admin panel"} {
		if !strings.Contains(front, want) {
			t.Errorf("frontmatter description is missing the trigger %q", want)
		}
	}
}

func TestExplainFirstIsInTheFirstFiveLines(t *testing.T) {
	b, err := Read(SkillFile)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	// Skip the frontmatter: "first five lines" means of the body (§14).
	if i := strings.Index(s[4:], "\n---\n"); i >= 0 {
		s = s[4+i+5:]
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 5 {
		lines = lines[:5]
	}
	if !strings.Contains(strings.Join(lines, "\n"), "pay explain") {
		t.Fatalf("`pay explain` must appear in the first five body lines, got:\n%s", strings.Join(lines, "\n"))
	}
}

func TestMandatoryStatementsAreVerbatim(t *testing.T) {
	targets := []string{SkillFile, "references/gotchas.md"}
	for _, name := range targets {
		b, err := Read(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		body := NormalizeProse(string(b))
		for i, stmt := range MandatoryStatements {
			if !strings.Contains(body, NormalizeProse(stmt)) {
				t.Errorf("%s is missing mandatory statement %d:\n%s", name, i+1, stmt)
			}
		}
	}
}

func TestSkillTeachesTheLoadBearingFacts(t *testing.T) {
	skill, err := Read(SkillFile)
	if err != nil {
		t.Fatal(err)
	}
	body := string(skill)
	tests := []struct {
		name string
		want string
	}{
		{"the ok field", "`ok`"},
		{"data_kind", "data_kind"},
		{"page.truncated", "page.truncated"},
		{"next.cmd", "next.cmd"},
		{"errors go to stdout", "Errors are printed on STDOUT"},
		{"exit 7 is bold", "**Exit 7"},
		{"exit 11", "| 11 |"},
		{"limit 0 is unlimited and rejected", "`--limit 0` means \"unlimited\""},
		{"bulk delete resolves ids client-side", "ids client-side"},
		{"max-docs is the only cap", "--max-docs"},
		{"the draft trap", "_status"},
		{"dry run", "--dry-run"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(body, tc.want) {
				t.Errorf("SKILL.md never mentions %q", tc.want)
			}
		})
	}

	// The exit-code table must be total: 0 through 11.
	for i := 0; i <= 11; i++ {
		if !strings.Contains(body, "| "+itoa(i)+" |") && !strings.Contains(body, "| **"+itoa(i)+"**") {
			t.Errorf("SKILL.md's exit-code table is missing exit %d", i)
		}
	}
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}

func TestReferencesCoverTheirTopics(t *testing.T) {
	tests := []struct {
		file string
		want []string
	}{
		{"references/query-syntax.md", []string{"--where", "not_like", "greater_than_equal", "--since", "--max-docs", "json:"}},
		{"references/errors.md", []string{"partial_failure", "Exit 7", "validation_failed", "QueryError", "server_busy"}},
		{"references/recipes.md", []string{"--dry-run", "--output jsonl", "pay upload", "pay versions", "--published-only"}},
		{"references/gotchas.md", []string{"limit=0", "Bulk `DELETE` ignores `limit`", "deletedAt", "API-Key"}},
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			b, err := Read(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.Contains(string(b), want) {
					t.Errorf("%s never mentions %q", tc.file, want)
				}
			}
		})
	}
}

func TestNoSecretInAssets(t *testing.T) {
	// §3.1's repo-wide assertion, applied to the bytes that ship in the binary.
	forbidden := []string{"paycli-dev-key-", "API-Key paycli", "eyJhbGciOi"}
	for _, f := range Files() {
		for _, bad := range forbidden {
			if strings.Contains(string(f.Data), bad) {
				t.Errorf("%s contains %q", f.Path, bad)
			}
		}
	}
}

func TestResolve(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", SkillFile},
		{"skill", SkillFile},
		{"SKILL.md", SkillFile},
		{"errors", "references/errors.md"},
		{"errors.md", "references/errors.md"},
		{"references/errors.md", "references/errors.md"},
		{"gotchas", "references/gotchas.md"},
		{"../../etc/passwd", "references/passwd.md"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := Resolve(tc.in); got != tc.want {
				t.Fatalf("Resolve(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	if _, err := Read("nope"); err == nil {
		t.Fatal("Read of an unknown document must fail")
	}
}

func TestNormalizeProse(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"blockquote", "> a\n> b", "a b"},
		{"indented blockquote", "  >  a\n  > b", " a b"},
		{"plain wrapping", "a\n   b", "a b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeProse(tc.in); got != tc.want {
				t.Fatalf("NormalizeProse(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
