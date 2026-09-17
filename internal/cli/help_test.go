package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// helpSections is §10.5's mandatory order. Every command's help renders all of
// them, so an agent can rely on the shape instead of pattern-matching prose.
var helpSections = []string{
	"SYNOPSIS", "FLAGS", "OUTPUT", "EXIT CODES", "EXAMPLES", "COMMON MISTAKES", "SEE ALSO",
}

// helpOwned is every command §10.5's doctrine covers. It used to exclude all
// ten data commands — the ones an agent actually runs — which is exactly
// backwards: `pay find --help` needs worked examples far more than `pay version`
// does. The data commands are listed here now, so a new EXAMPLES-less command
// cannot ship.
var helpOwned = [][]string{
	// Data commands (§9.2). These are the ones agents use.
	{"find"}, {"get"}, {"count"}, {"create"}, {"update"}, {"delete"},
	{"restore"}, {"duplicate"}, {"publish"}, {"unpublish"},
	{"upload"}, {"download"},
	{"globals"}, {"globals", "list"}, {"globals", "get"}, {"globals", "update"},
	{"versions"}, {"versions", "list"}, {"versions", "get"},
	{"versions", "restore"}, {"versions", "diff"},
	// Meta commands.
	{"explain"}, {"collections"}, {"describe"}, {"doctor"}, {"discover"},
	{"whoami"}, {"access"}, {"can"}, {"version"}, {"completion"},
	{"auth"}, {"auth", "login"}, {"auth", "list"}, {"auth", "status"},
	{"auth", "test"}, {"auth", "logout"}, {"auth", "use"}, {"auth", "rename"},
	{"auth", "fix-perms"},
	{"config"}, {"config", "get"}, {"config", "set"}, {"config", "unset"},
	{"config", "list"}, {"config", "paths"}, {"config", "explain"},
	// The last four trees to be brought under the doctrine. They rendered
	// "(none recorded for this command)" under EXAMPLES and COMMON MISTAKES
	// for every one of their sixteen commands.
	{"raw"},
	{"audit"}, {"audit", "tail"}, {"audit", "path"},
	{"cache"}, {"cache", "info"}, {"cache", "ls"}, {"cache", "show"},
	{"cache", "path"}, {"cache", "clear"}, {"cache", "warm"},
	{"skills"}, {"skills", "install"}, {"skills", "update"},
	{"skills", "uninstall"}, {"skills", "status"},
	{"skills", "list"}, {"skills", "print"},
	// update-self was the last command in the tree with no Help at all: it
	// rendered both placeholders and the renderer's {0, 1} exit-code fallback,
	// which is a lie for a command whose every failure mode is remote
	// (measured: 2 on a bad GH_TOKEN, 4 with no release published, 5 on
	// --check --apply, 10 under PAY_NO_UPDATE).
	{"update-self"},
}

// helpOwnedCoversTheWholeTree is the guard that made `helpOwned` stop being a
// hand-curated subset. §10.5's doctrine is "every command", so the list above
// must name every command cobra can reach; a new command ships with real
// examples or it does not ship.
func TestHelpOwnedNamesEveryCommand(t *testing.T) {
	listed := map[string]bool{}
	for _, path := range helpOwned {
		listed[strings.Join(path, " ")] = true
	}
	var missing []string
	var walk func(cmd *cobra.Command, path []string)
	walk = func(cmd *cobra.Command, path []string) {
		for _, sub := range cmd.Commands() {
			if !sub.IsAvailableCommand() {
				continue
			}
			p := append(append([]string{}, path...), sub.Name())
			if !listed[strings.Join(p, " ")] {
				missing = append(missing, strings.Join(p, " "))
			}
			walk(sub, p)
		}
	}
	walk(newRootCommand(&Runtime{}), nil)
	if len(missing) > 0 {
		t.Fatalf("helpOwned does not cover %v; §10.5 applies to every command", missing)
	}
}

// §10.5 section 8, for the whole tree rather than the data commands alone.
// The renderer's fallback is {0, 1} and buildCommandSpec's used to be {0}, so a
// command that declared nothing published two different wrong tables for
// itself. This keeps the fallback unreachable in the shipped binary: every
// command states what it can actually return.
func TestEveryCommandDeclaresItsExitCodes(t *testing.T) {
	root := newRootCommand(&Runtime{})
	for _, path := range helpOwned {
		t.Run(strings.Join(path, " "), func(t *testing.T) {
			cmd, _, err := root.Find(path)
			if err != nil || cmd == nil {
				t.Fatalf("%v does not resolve: %v", path, err)
			}
			codes := HelpOf(cmd).ExitCodes
			if len(codes) == 0 {
				t.Fatalf("`%s` declares no ExitCodes, so help falls back to %v",
					cmd.CommandPath(), fallbackExitCodes)
			}
			if codes[0] != 0 {
				t.Errorf("`%s` exit codes do not start at 0: %v", cmd.CommandPath(), codes)
			}
		})
	}
}

// dataCommands is helpOwned's §9.2 half, asserted separately for the two rules
// that are specific to them: a real exit-code table and a machine-readable spec
// that is not empty.
var dataCommands = [][]string{
	{"find"}, {"get"}, {"count"}, {"create"}, {"update"}, {"delete"},
	{"restore"}, {"duplicate"}, {"publish"}, {"unpublish"},
	{"upload"}, {"download"},
	{"globals", "list"}, {"globals", "get"}, {"globals", "update"},
	{"versions", "list"}, {"versions", "get"},
	{"versions", "restore"}, {"versions", "diff"},
}

// §10.5 section 8. The renderer's fallback prints {0, 1}, which is a lie for
// every command that talks to a server: an agent branching on `pay find --help`
// would believe find cannot answer 5, 9 or 10. All three are reproducible
// against a live instance (unknown collection -> 10, --limit 0 -> 5, an
// unreachable base URL -> 9).
func TestDataCommandsPublishTheirRealExitCodes(t *testing.T) {
	for _, path := range dataCommands {
		t.Run(strings.Join(path, " "), func(t *testing.T) {
			res := helpOf(t, path...)
			for _, want := range []string{"\n    5   ", "\n    9   ", "\n    10  "} {
				if !strings.Contains(res.Stdout, want) {
					t.Errorf("EXIT CODES omits %q:\n%s", strings.TrimSpace(want), res.Stdout)
				}
			}
			if strings.Contains(res.Stdout, "unknown\n") {
				t.Errorf("an exit code rendered as \"unknown\":\n%s", res.Stdout)
			}
		})
	}
}

// §10.4. `pay <cmd> --help --output json` is the form an agent parses, and it
// used to report exit_codes:[0], examples:[] and args:[] for every data command
// while the prose carried real content.
func TestDataCommandSpecsAreNotEmpty(t *testing.T) {
	for _, path := range dataCommands {
		t.Run(strings.Join(path, " "), func(t *testing.T) {
			args := append(append([]string{}, path...), "--help", "--output", "json")
			res := cliRun(t, invocation{Args: args})
			if res.Code != 0 {
				t.Fatalf("exit = %d\n%s", res.Code, res.Stderr)
			}
			d := res.data(t)
			if codes, _ := d["exit_codes"].([]any); len(codes) < 5 {
				t.Errorf("exit_codes = %v; a data command can fail in more ways than that", d["exit_codes"])
			}
			if ex, _ := d["examples"].([]any); len(ex) < 4 {
				t.Errorf("examples = %d; §10.5 asks for 4-8", len(ex))
			}
			if m, _ := d["common_mistakes"].([]any); len(m) < 3 {
				t.Errorf("common_mistakes = %d; §10.5 asks for 3-6", len(m))
			}
			// §10.4: a command whose Use line declares a positional must
			// document it, or `--help --output json` tells an agent the command
			// takes no arguments at all.
			root := newRootCommand(&Runtime{})
			cmd, _, _ := root.Find(path)
			if cmd != nil && strings.Contains(cmd.Use, "<") {
				if args2, _ := d["args"].([]any); len(args2) == 0 {
					t.Errorf("`%s` declares %q but documents no args: %v",
						cmd.CommandPath(), cmd.Use, d["args"])
				}
			}
		})
	}
}

// §10.5 sections 4 and 5 are unreachable unless a command opts in, so the two
// verified footguns (contains escaping, nlike auto-wrapping) and the silent-sort
// warning appeared in NO help output at all.
func TestCommandsThatTakeWhereTeachTheWhereSyntax(t *testing.T) {
	for _, path := range [][]string{{"find"}, {"count"}, {"update"}, {"delete"}, {"publish"}, {"unpublish"}} {
		t.Run(strings.Join(path, " "), func(t *testing.T) {
			res := helpOf(t, path...)
			if !strings.Contains(res.Stdout, "\nWHERE SYNTAX\n") {
				t.Fatalf("a command with --where does not teach it:\n%s", res.Stdout)
			}
			for _, want := range []string{"contains escapes", "nlike auto-wraps", "greater_than_equal"} {
				if !strings.Contains(res.Stdout, want) {
					t.Errorf("WHERE SYNTAX is missing %q", want)
				}
			}
		})
	}
	for _, path := range [][]string{{"find"}, {"versions", "list"}} {
		t.Run(strings.Join(path, " ")+" sort", func(t *testing.T) {
			res := helpOf(t, path...)
			if !strings.Contains(res.Stdout, "\nSORT\n") {
				t.Fatalf("a command with --sort does not teach it:\n%s", res.Stdout)
			}
			if !strings.Contains(res.Stdout, "SILENTLY IGNORES") {
				t.Errorf("SORT does not carry the silent-ignore warning:\n%s", res.Stdout)
			}
		})
	}
}

// An example that only exists in cmd.Long never reaches CommandSpec.Examples,
// and printing it in both places renders it twice. Keep them in Help only.
func TestDataCommandProseCarriesNoExampleBlock(t *testing.T) {
	root := newRootCommand(&Runtime{})
	for _, path := range dataCommands {
		cmd, _, err := root.Find(path)
		if err != nil || cmd == nil {
			t.Fatalf("%v does not resolve: %v", path, err)
		}
		if strings.Contains(cmd.Long, "\nExamples:\n") {
			t.Errorf("`%s` keeps an Examples: block in cmd.Long; move it to Help.Examples", cmd.CommandPath())
		}
		if strings.Contains(cmd.Long, "\nCOMMON MISTAKES\n") {
			t.Errorf("`%s` keeps a COMMON MISTAKES block in cmd.Long; move it to Help.Mistakes", cmd.CommandPath())
		}
	}
}

func helpOf(t *testing.T, path ...string) cliResult {
	t.Helper()
	return cliRun(t, invocation{Args: append(append([]string{}, path...), "--help")})
}

func TestHelpRendersEverySection(t *testing.T) {
	for _, path := range helpOwned {
		t.Run(strings.Join(path, " "), func(t *testing.T) {
			res := helpOf(t, path...)
			if res.Code != 0 {
				t.Fatalf("exit = %d\n%s", res.Code, res.Stderr)
			}
			at := -1
			for _, section := range helpSections {
				idx := strings.Index(res.Stdout, "\n"+section+"\n")
				if idx < 0 {
					t.Fatalf("section %q missing from help:\n%s", section, res.Stdout)
				}
				if idx < at {
					t.Errorf("section %q is out of §10.5 order", section)
				}
				at = idx
			}
		})
	}
}

// §10.5's hard rule: never a placeholder. Four to eight copy-pasteable
// examples and three to six numbered anti-patterns, each with its correction.
func TestHelpCarriesRealExamplesAndMistakes(t *testing.T) {
	for _, path := range helpOwned {
		t.Run(strings.Join(path, " "), func(t *testing.T) {
			res := helpOf(t, path...)
			if strings.Contains(res.Stdout, "(none recorded for this command)") {
				t.Fatalf("help has an empty section:\n%s", res.Stdout)
			}
			examples := strings.Count(res.Stdout, "\n  pay ")
			if examples < 3 {
				t.Errorf("only %d example lines; §10.5 asks for 4-8", examples)
			}
			corrections := strings.Count(res.Stdout, "Instead: ")
			if corrections < 3 {
				t.Errorf("only %d corrections; §10.5 asks for 3-6", corrections)
			}
			// Every "you cannot X" must be followed by "do Y instead".
			mistakes := strings.Count(res.Stdout, "\n  1. ") + strings.Count(res.Stdout, "\n  2. ") +
				strings.Count(res.Stdout, "\n  3. ")
			if mistakes < 3 {
				t.Errorf("mistakes are not numbered 1..n: %d", mistakes)
			}
		})
	}
}

func TestHelpNeverBlocksOnAColdCache(t *testing.T) {
	// No base URL, no cache, and http.DefaultTransport panics (TestMain): if
	// help touched the network this test would not merely fail, it would panic.
	res := helpOf(t, "describe")
	if !strings.Contains(res.Stdout, "DISCOVERED IN THIS PROJECT") {
		t.Fatalf("section 3 missing:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "run 'pay discover' to populate") {
		t.Errorf("cold cache must name the command that populates it:\n%s", res.Stdout)
	}
}

func TestHelpStampsProfileBaseURLAndRevision(t *testing.T) {
	home := t.TempDir()
	seedDiscovery(t, home, testBaseURL)
	res := cliRun(t, invocation{
		Home: home, Args: []string{"describe", "--help"}, Env: seededEnv(testBaseURL),
	})
	if !strings.Contains(res.Stdout, "profile=default") {
		t.Errorf("stamp missing the profile:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "base_url="+testBaseURL) {
		t.Errorf("stamp missing the base_url:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "discovery_revision=") {
		t.Errorf("stamp missing the discovery revision:\n%s", res.Stdout)
	}
	if strings.Contains(res.Stdout, "discovery_revision=-") {
		t.Errorf("warm cache still reported an unknown revision:\n%s", res.Stdout)
	}
}

func TestHelpShowsRealDiscoveredValues(t *testing.T) {
	home := t.TempDir()
	seedDiscovery(t, home, testBaseURL)
	res := cliRun(t, invocation{
		Home: home, Args: []string{"collections", "--help"}, Env: seededEnv(testBaseURL),
	})
	if !strings.Contains(res.Stdout, "pages") {
		t.Errorf("help did not name a real slug from this project:\n%s", res.Stdout)
	}
	if strings.Contains(res.Stdout, "<collection>\n") && !strings.Contains(res.Stdout, "content (") {
		t.Errorf("section 3 rendered a placeholder instead of the inventory:\n%s", res.Stdout)
	}
}

func TestHelpTeachesTheWhereSyntax(t *testing.T) {
	// §10.5 section 4 must carry the full operator table inline.
	var sb strings.Builder
	for _, row := range whereOperators {
		sb.WriteString(row[1])
	}
	res := cliRun(t, invocation{Args: []string{"describe", "--help"}})
	_ = res
	// describe has no --where; the table belongs to commands that do. Assert it
	// on the shared renderer instead, which is what every such command uses.
	lines := strings.Join(whereSyntaxLines(), "\n")
	for _, row := range whereOperators {
		if !strings.Contains(lines, row[1]) {
			t.Errorf("operator %q is not taught in the WHERE SYNTAX block", row[1])
		}
	}
	for _, want := range []string{"contains escapes", "nlike auto-wraps", "json:", "ANDed"} {
		if !strings.Contains(lines, want) {
			t.Errorf("WHERE SYNTAX does not mention %q", want)
		}
	}
	sortText := strings.Join(sortLines(), "\n")
	if !strings.Contains(sortText, "SILENTLY IGNORES") {
		t.Errorf("SORT section does not warn about silent ignoring: %s", sortText)
	}
}

func TestMachineReadableHelp(t *testing.T) {
	res := cliRun(t, invocation{Args: []string{"describe", "--help", "--output", "json"}})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}
	if res.Env == nil {
		t.Fatalf("not an envelope:\n%s", res.Stdout)
	}
	if res.Env["data_kind"] != "command_spec" {
		t.Fatalf("data_kind = %v, want command_spec", res.Env["data_kind"])
	}
	d := res.data(t)
	path, _ := d["path"].([]any)
	if len(path) != 1 || path[0] != "describe" {
		t.Errorf("path = %v, want [describe]", path)
	}
	for _, key := range []string{"short", "args", "flags", "output", "exit_codes", "examples"} {
		if _, ok := d[key]; !ok {
			t.Errorf("command_spec.%s missing", key)
		}
	}
	flags, _ := d["flags"].([]any)
	if len(flags) == 0 {
		t.Fatal("no flags in the command spec")
	}
	first := flags[0].(map[string]any)
	for _, key := range []string{"name", "type", "repeatable", "default", "usage"} {
		if _, ok := first[key]; !ok {
			t.Errorf("flag spec is missing %q: %v", key, first)
		}
	}
	args, _ := d["args"].([]any)
	if len(args) == 0 {
		t.Fatal("describe declares no positional argument")
	}
	arg := args[0].(map[string]any)
	if arg["values_from"] != "discovery.collections" {
		t.Errorf("arg.values_from = %v", arg["values_from"])
	}
}

func TestMachineReadableHelpFillsValuesFromTheCache(t *testing.T) {
	home := t.TempDir()
	seedDiscovery(t, home, testBaseURL)
	res := cliRun(t, invocation{
		Home: home, Args: []string{"describe", "--help", "--output", "json"}, Env: seededEnv(testBaseURL),
	})
	args, _ := res.data(t)["args"].([]any)
	values, _ := args[0].(map[string]any)["values"].([]any)
	if len(values) == 0 {
		t.Fatalf("a warm cache must fill the enum: %v", args[0])
	}
}

func TestHelpRejectsAnImpossibleFormat(t *testing.T) {
	res := cliRun(t, invocation{Args: []string{"describe", "--help", "--output", "csv"}})
	if got := res.code(t); got != "format_unsupported" {
		t.Fatalf("code = %q, want format_unsupported\n%s", got, res.Stdout)
	}
	if res.Code != 5 {
		t.Errorf("exit = %d, want 5", res.Code)
	}
}

func TestRootHelpListsTheGroups(t *testing.T) {
	res := cliRun(t, invocation{Args: []string{"--help"}})
	if !strings.Contains(res.Stdout, "Subcommands:") {
		t.Fatalf("root help does not list subcommands:\n%s", res.Stdout)
	}
	for _, want := range []string{"explain", "doctor", "collections", "describe"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("root help does not mention %q", want)
		}
	}
}

func TestExitCodeHelpIsAnswerable(t *testing.T) {
	// Every exit code named in a help block must have a meaning, and the block
	// must point at the full table.
	res := helpOf(t, "explain")
	if !strings.Contains(res.Stdout, "pay explain --section exit_codes") {
		t.Errorf("EXIT CODES does not point at the full table:\n%s", res.Stdout)
	}
	if strings.Contains(res.Stdout, "unknown\n") {
		t.Errorf("an exit code rendered as \"unknown\":\n%s", res.Stdout)
	}
}

func TestCommandSpecIsValidJSON(t *testing.T) {
	res := cliRun(t, invocation{Args: []string{"version", "--help", "--output", "json"}})
	var v any
	if err := json.Unmarshal([]byte(res.Stdout), &v); err != nil {
		t.Fatalf("command spec is not valid JSON: %v\n%s", err, res.Stdout)
	}
}
