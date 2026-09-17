package cli

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/skills"
)

// The installed skill is the agent's primary contract (§14), and it drifts
// silently: a flag is renamed in a command file and nothing tells SKILL.md.
// These tests validate every `pay …` line in the embedded documents against the
// real cobra tree and the real --path parser. They need no server, so they
// cannot rot the way an execution harness against live-only slugs would.

var (
	skillCmdLine  = regexp.MustCompile(`(?m)^\s*(pay\s.*)$`)
	skillComment  = regexp.MustCompile(`\s+#.*$`)
	skillRedirect = regexp.MustCompile(`[<>|].*$`)
)

// skillCommands returns every `pay …` invocation in the embedded skill files,
// keyed by "file:line" so a failure names the line to edit.
func skillCommands(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := fs.WalkDir(skills.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return err
		}
		data, err := fs.ReadFile(skills.FS(), p)
		if err != nil {
			return err
		}
		lines := strings.Split(string(data), "\n")
		for i := 0; i < len(lines); i++ {
			line := strings.TrimSpace(lines[i])
			if !strings.HasPrefix(line, "pay ") {
				continue
			}
			start := i
			// Join shell continuations so a wrapped example is checked whole.
			for strings.HasSuffix(line, `\`) && i+1 < len(lines) {
				i++
				line = strings.TrimSuffix(line, `\`) + " " + strings.TrimSpace(lines[i])
			}
			out[p+":"+itoa(start+1)] = line
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the embedded skill: %v", err)
	}
	if len(out) < 50 {
		t.Fatalf("only %d skill commands found; the scanner is broken", len(out))
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// skillTokens splits one documented invocation into argv, dropping the trailing
// comment, any shell redirection or pipe, and the leading "pay".
func skillTokens(line string) []string {
	line = skillComment.ReplaceAllString(line, "")
	line = skillRedirect.ReplaceAllString(line, "")
	fields := splitShell(line)
	if len(fields) == 0 || fields[0] != "pay" {
		return nil
	}
	return fields[1:]
}

// splitShell is a minimal, quote-aware tokeniser — enough for documentation.
func splitShell(s string) []string {
	var (
		out   []string
		cur   strings.Builder
		quote rune
		has   bool
	)
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote, has = r, true
		case r == ' ' || r == '\t':
			if cur.Len() > 0 || has {
				out = append(out, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 || has {
		out = append(out, cur.String())
	}
	return out
}

// TestSkillCommandsResolveAgainstTheCommandTree is the regression test for the
// four documented commands that did not run: every command chain must exist and
// every --flag must be declared on that command or on root.
func TestSkillCommandsResolveAgainstTheCommandTree(t *testing.T) {
	root := newRootCommand(&Runtime{})
	for loc, line := range skillCommands(t) {
		args := skillTokens(line)
		if len(args) == 0 {
			continue
		}
		cmd, rest, err := root.Find(args)
		if err != nil || cmd == nil {
			t.Errorf("%s: `%s` does not resolve to a command: %v", loc, line, err)
			continue
		}
		if cmd == root {
			t.Errorf("%s: `%s` names no subcommand", loc, line)
			continue
		}
		skip := false
		for _, tok := range rest {
			if skip {
				skip = false
				continue
			}
			name, ok := strings.CutPrefix(tok, "--")
			if !ok {
				continue
			}
			name, hasValue := cutFlagValue(name)
			if name == "" {
				continue
			}
			flag := cmd.Flags().Lookup(name)
			if flag == nil {
				flag = root.PersistentFlags().Lookup(name)
			}
			if flag == nil {
				t.Errorf("%s: `%s` has no --%s (documented at `%s`)",
					loc, cmd.CommandPath(), name, line)
				continue
			}
			// A non-boolean flag consumes the next token, which must not then
			// be mistaken for a flag of its own (`--sort -publishedAt`).
			if flag.Value.Type() != "bool" && !hasValue {
				skip = true
			}
		}
	}
}

func cutFlagValue(s string) (string, bool) {
	if name, _, ok := strings.Cut(s, "="); ok {
		return name, true
	}
	return s, false
}

// Every --path expression the skill teaches must parse with the real evaluator.
// This is what caught `[*].field`, a form the parser has never accepted.
func TestSkillPathExpressionsParse(t *testing.T) {
	root := newRootCommand(&Runtime{})
	seen := 0
	for loc, line := range skillCommands(t) {
		args := skillTokens(line)
		_, rest, err := root.Find(args)
		if err != nil {
			continue
		}
		for i, tok := range rest {
			expr := ""
			switch {
			case tok == "--path" && i+1 < len(rest):
				expr = rest[i+1]
			case strings.HasPrefix(tok, "--path="):
				expr = strings.TrimPrefix(tok, "--path=")
			default:
				continue
			}
			seen++
			if _, err := output.ParsePath(expr); err != nil {
				t.Errorf("%s: --path %q is not valid: %v (documented at `%s`)", loc, expr, err, line)
			}
		}
	}
	if seen == 0 {
		t.Fatal("the skill documents no --path expression; the scanner is broken")
	}
}

// §9.8: a `pay raw` path with a leading slash is sent VERBATIM, so a documented
// `pay raw GET /pages` reaches the Next.js app rather than the REST API and
// fails with non_json_response. Only /api… and /admin… are deliberate.
func TestSkillRawExamplesUseTheRightPathForm(t *testing.T) {
	root := newRootCommand(&Runtime{})
	for loc, line := range skillCommands(t) {
		args := skillTokens(line)
		cmd, rest, err := root.Find(args)
		if err != nil || cmd == nil || cmd.Name() != "raw" || len(rest) < 2 {
			continue
		}
		path := rest[1]
		if !strings.HasPrefix(path, "/") {
			continue
		}
		if strings.HasPrefix(path, "/api") || strings.HasPrefix(path, "/admin") {
			continue
		}
		t.Errorf("%s: `pay raw %s %s` is sent verbatim and will not reach the REST API; "+
			"write %q or %q", loc, rest[0], path, strings.TrimPrefix(path, "/"), "/api"+path)
	}
}

// A sanity check that the scanner is actually looking at the shipped tree.
func TestSkillScannerSeesTheRealDocuments(t *testing.T) {
	cmds := skillCommands(t)
	var files []string
	for loc := range cmds {
		files = append(files, strings.SplitN(loc, ":", 2)[0])
	}
	for _, want := range []string{"SKILL.md", "references/recipes.md", "references/query-syntax.md"} {
		found := false
		for _, f := range files {
			if f == want {
				found = true
			}
		}
		if !found {
			t.Errorf("no `pay …` command was scanned out of %s", want)
		}
	}
}

var _ = cobra.Command{}
