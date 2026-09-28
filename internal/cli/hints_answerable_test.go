package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// ---------------------------------------------------------------------------
// A15: no hint may name a command that cannot answer it
// ---------------------------------------------------------------------------
//
// An agent runs a hint verbatim. A hint naming `pay collections --globals`
// (no such flag), `pay config init` (no such subcommand) or
// `pay explain --section where` (no such section) costs it a round-trip and
// teaches it to distrust every other hint. This test reads every string
// literal of the shipped code (internal/…, cmd/…, tests excluded) and checks
// each `pay …` command it names against the real command tree:
//
//   - every backticked span that starts with "pay " (the house style of hints
//     and messages: "… `pay describe <collection>` lists …"), and
//   - every literal that IS a command line: it starts with "pay " (WithHint
//     arguments, Example.Cmd, next[] strings).
//
// A command must resolve word by word to a real subcommand, every --flag must
// exist on it (inherited persistent flags count), and a flag value must be one
// of the flag's documented Values when it has them. Placeholders (<id>, %s,
// FILE, …) stand for arguments and are accepted wherever an argument is; a
// template whose command itself is a placeholder (`pay <command> --help`) is
// skipped.

var hintSpanRE = regexp.MustCompile("`(pay [^`]+)`")

// hintPlaceholder is an argument stand-in: <x>, %s / %q / %d, {x}, …, X|Y
// alternations, or an all-caps word (FILE, ID, COLLECTION).
func hintPlaceholder(tok string) bool {
	if strings.ContainsAny(tok, "<>%{}…[]|") || strings.Contains(tok, "...") {
		return true
	}
	letters := strings.Trim(tok, "_-0123456789")
	return letters != "" && strings.ToUpper(letters) == letters && strings.ToLower(letters) != letters
}

// hintTokens splits a command line like a shell would, closely enough for
// hints: quotes group, a trailing "# comment" ends it.
func hintTokens(s string) []string {
	var (
		out   []string
		cur   strings.Builder
		quote rune
		have  bool
	)
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, have = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if have || cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteRune(r)
		}
	}
	if have || cur.Len() > 0 {
		out = append(out, cur.String())
	}
	for i, tok := range out {
		if strings.HasPrefix(tok, "#") {
			return out[:i]
		}
	}
	return out
}

// lookupHintFlag finds a flag on cmd or inherited from its parents.
func lookupHintFlag(cmd *cobra.Command, name string, short bool) *pflag.Flag {
	for c := cmd; c != nil; c = c.Parent() {
		for _, fs := range []*pflag.FlagSet{c.Flags(), c.PersistentFlags()} {
			var f *pflag.Flag
			if short {
				f = fs.ShorthandLookup(name)
			} else {
				f = fs.Lookup(name)
			}
			if f != nil && (c == cmd || fs == c.PersistentFlags() || f.Name == "help") {
				return f
			}
		}
	}
	return nil
}

// checkHintCommand returns why line (one command, starting with "pay ")
// cannot be run, "" when it can, and "skip" for a template.
func checkHintCommand(root *cobra.Command, line string) string {
	toks := hintTokens(line)
	if len(toks) == 0 || toks[0] != "pay" {
		return ""
	}
	cmd := root
	positional, end := false, false
	for i := 1; i < len(toks) && !end; i++ {
		tok := toks[i]
		// "… --draft, or pass …": a comma ends the command, prose follows.
		if strings.HasSuffix(tok, ",") {
			tok, end = strings.TrimRight(tok, ","), true
		}
		switch {
		case tok == "-" || tok == "--" || tok == "…" || tok == "...":
			continue
		case strings.HasPrefix(tok, "-"):
			short := !strings.HasPrefix(tok, "--")
			name := strings.TrimLeft(tok, "-")
			value, hasValue := "", false
			if k, v, ok := strings.Cut(name, "="); ok {
				name, value, hasValue = k, v, true
			}
			if hintPlaceholder(name) {
				continue
			}
			if short && len(name) > 1 {
				return "combined short flags " + tok
			}
			if name == "help" || name == "h" || (name == "version" && cmd == root) {
				continue // cobra adds these lazily
			}
			f := lookupHintFlag(cmd, name, short)
			if f == nil {
				return "`" + cmd.CommandPath() + "` has no flag " + tok
			}
			if f.Value.Type() == "bool" || f.NoOptDefVal != "" {
				continue
			}
			if !hasValue {
				if i+1 >= len(toks) {
					continue // the value is elided in the prose
				}
				i++
				value = toks[i]
			}
			if vals := HelpOf(cmd).FlagInfo[f.Name].Values; len(vals) > 0 && !hintPlaceholder(value) {
				ok := false
				for _, v := range vals {
					ok = ok || v == value
				}
				if !ok {
					return "`" + cmd.CommandPath() + " --" + f.Name + "` does not accept " + strconv.Quote(value) +
						" (valid: " + strings.Join(vals, ", ") + ")"
				}
			}
		case positional || !cmd.HasSubCommands():
			positional = true
		default:
			if alts := strings.Split(strings.Trim(tok, "<>[]"), "|"); len(alts) > 1 {
				// A synopsis: `pay audit tail|path`. Every alternative must
				// be a subcommand; what follows applies to several of them.
				for _, a := range alts {
					if subcommand(cmd, a) == nil {
						return "`" + cmd.CommandPath() + "` has no subcommand " + strconv.Quote(a)
					}
				}
				return ""
			}
			if hintPlaceholder(tok) {
				if cmd == root {
					return "skip"
				}
				return "`" + cmd.CommandPath() + "` takes a subcommand, not " + tok
			}
			next := subcommand(cmd, tok)
			if next == nil {
				return "`" + cmd.CommandPath() + "` has no subcommand " + strconv.Quote(tok)
			}
			cmd = next
		}
	}
	return ""
}

// subcommand is cmd's child named (or aliased) name; cobra's lazily added
// help command counts.
func subcommand(cmd *cobra.Command, name string) *cobra.Command {
	if name == "help" && !cmd.HasParent() {
		return cmd
	}
	for _, c := range cmd.Commands() {
		if c.Name() == name || c.HasAlias(name) {
			return c
		}
	}
	return nil
}

var hintJoinRE = regexp.MustCompile(`\s(?:\|\||\||&&)\s|;\s+|,?\s+or:?\s+`)

// hintCommands are the command lines a string literal names. whole says the
// literal is itself a command line (a hint or an example), not prose.
func hintCommands(s string, whole bool) []string {
	var lines []string
	if whole && strings.HasPrefix(s, "pay ") {
		lines = append(lines, s)
	}
	for _, m := range hintSpanRE.FindAllStringSubmatch(s, -1) {
		lines = append(lines, m[1])
	}
	var out []string
	for _, l := range lines {
		// A pipe or a && joins commands; each `pay` part is checked.
		for _, part := range hintJoinRE.Split(l, -1) {
			if part = strings.TrimSpace(part); strings.HasPrefix(part, "pay ") {
				out = append(out, part)
			}
		}
	}
	return out
}

func TestEveryHintIsAnswerable(t *testing.T) {
	root := newRootCommand(&Runtime{})
	checked := 0
	for _, dir := range []string{"../../internal", "../../cmd"} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			// Literals that are command lines by position: WithHint's
			// arguments and Hint / Cmd / Synopsis values.
			whole := map[*ast.BasicLit]bool{}
			markAll := func(n ast.Node) {
				ast.Inspect(n, func(n ast.Node) bool {
					if lit, ok := n.(*ast.BasicLit); ok {
						whole[lit] = true
					}
					return true
				})
			}
			ast.Inspect(file, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CallExpr:
					if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "WithHint" {
						for _, a := range n.Args {
							markAll(a)
						}
					}
				case *ast.KeyValueExpr:
					if id, ok := n.Key.(*ast.Ident); ok && (id.Name == "Hint" || id.Name == "Cmd" || id.Name == "Synopsis") {
						markAll(n.Value)
					}
				}
				return true
			})
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					return true
				}
				for _, line := range hintCommands(s, whole[lit]) {
					checked++
					if why := checkHintCommand(root, line); why != "" && why != "skip" {
						t.Errorf("%s: %q: %s", fset.Position(lit.Pos()), line, why)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("checked %d command lines", checked)
	if checked < 200 {
		t.Fatalf("only %d command lines found: the scan is not reading the sources", checked)
	}
}

// The checker itself: the hints the review found broken fail it, their
// replacements pass.
func TestHintCheckerCatchesBrokenCommands(t *testing.T) {
	root := newRootCommand(&Runtime{})
	for _, bad := range []string{
		"pay collections --globals", "pay collections --uploads", "pay explain --section where",
		"pay explain --section endpoints", "pay config init", "pay versions <collection> <id>",
	} {
		if why := checkHintCommand(root, bad); why == "" || why == "skip" {
			t.Errorf("%q passed the checker", bad)
		}
	}
	for _, good := range []string{
		"pay globals list", "pay collections --kind upload", "pay explain --section query_syntax",
		"pay find <collection> --limit 5", "pay config explain", "pay versions list <collection> --id <id>",
		"pay get <collection> <id> --depth 0", "pay blocks ls --field layout[2].blocks",
	} {
		if why := checkHintCommand(root, good); why != "" {
			t.Errorf("%q failed the checker: %s", good, why)
		}
	}
}
