package cli

import (
	"strings"
	"testing"
)

// §10.1 / SKILL.md §1: "the shape never changes between commands". `command` is
// part of that shape, and an agent that keys its retry bookkeeping on it must
// not get a missing key precisely on the failure path.
//
// Regression for the argument-parse path: cobra rejects an unknown flag or a
// bad Args count BEFORE PersistentPreRunE binds a command, so Runtime.fail()
// used to render the envelope with the ROOT command and emit no `command` key
// at all.
func TestErrorEnvelopeAlwaysCarriesTheCommandKey(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"unknown flag on a data command", []string{"get", "pages", "--published-only"}, "get"},
		{"missing positional", []string{"find"}, "find"},
		{"bad flag value", []string{"find", "pages", "--limit", "notanumber"}, "find"},
		{"unknown flag on a subcommand", []string{"auth", "status", "--nope"}, "auth status"},
		{"unknown flag on a meta command", []string{"version", "--nope"}, "version"},
		{"unknown command", []string{"frobnicate"}, ""},
		{"no command at all", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := cliRun(t, invocation{Args: tc.args})
			if res.Env == nil {
				t.Fatalf("stdout is not an envelope:\n%s", res.Stdout)
			}
			got, ok := res.Env["command"]
			if !ok {
				t.Fatalf("`command` key is absent from the error envelope:\n%s", res.Stdout)
			}
			if got != tc.want {
				t.Errorf("command = %q, want %q\n%s", got, tc.want, res.Stdout)
			}
			// The raw bytes must carry it too: omitempty on an empty string
			// would satisfy neither an agent nor `jq -e '.command'`.
			if !strings.Contains(res.Stdout, `"command":`) &&
				!strings.Contains(res.Stdout, `"command": `) {
				t.Errorf("`command` missing from the rendered JSON:\n%s", res.Stdout)
			}
		})
	}
}

// An unknown command must NOT invent one: writing "frobnicate" into `command`
// would feed unvalidated argv into a field the audit log also consumes (§21.3).
func TestUnknownCommandDoesNotInventACommandName(t *testing.T) {
	res := cliRun(t, invocation{Args: []string{"frobnicate", "--profile", "x"}})
	if got := res.Env["command"]; got != "" {
		t.Fatalf("command = %q, want the empty string", got)
	}
}
