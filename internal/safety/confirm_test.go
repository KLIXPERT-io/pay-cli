package safety

import (
	"bytes"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

func TestConfirm(t *testing.T) {
	tests := []struct {
		name      string
		conf      Confirmer
		req       Request
		input     string
		wantErr   bool
		wantCode  apierr.Code
		wantOut   []string
		wantNoOut []string
	}{
		{
			name: "L0 never prompts",
			conf: Confirmer{},
			req:  Request{Op: Op{Command: CmdFind}, Target: "pages"},
		},
		{
			name: "L1 does not prompt by default",
			conf: Confirmer{TTY: true},
			req:  Request{Op: Op{Command: CmdCreate}, Target: "pages"},
		},
		{
			name:     "L1 prompts when confirm_writes is on",
			conf:     Confirmer{TTY: false, ConfirmWrites: true},
			req:      Request{Op: Op{Command: CmdCreate}, Target: "pages"},
			wantErr:  true,
			wantCode: apierr.CodeConfirmationRequired,
		},
		{
			name:     "L2 in a non-TTY without --yes is exit 11",
			conf:     Confirmer{},
			req:      Request{Op: Op{Command: CmdDelete, Selector: SelectorID, Permanent: true}, Target: "pages"},
			wantErr:  true,
			wantCode: apierr.CodeConfirmationRequired,
			wantOut:  []string{"IRREVERSIBLE"},
		},
		{
			name: "L2 in a non-TTY with --yes proceeds",
			conf: Confirmer{AssumeYes: true},
			req:  Request{Op: Op{Command: CmdGlobalsUpdate, Global: true}, Target: "header"},
		},
		{
			name:    "L2 on a TTY accepts y",
			conf:    Confirmer{TTY: true},
			req:     Request{Op: Op{Command: CmdVersionsRestore, Selector: SelectorID}, Target: "pages"},
			input:   "y\n",
			wantOut: []string{"Proceed? [y/N]"},
		},
		{
			name:     "L2 on a TTY refuses on anything else",
			conf:     Confirmer{TTY: true},
			req:      Request{Op: Op{Command: CmdVersionsRestore, Selector: SelectorID}, Target: "pages"},
			input:    "nope\n",
			wantErr:  true,
			wantCode: apierr.CodeConfirmationRequired,
		},
		{
			name:     "L3 without --yes in a non-TTY is exit 11 and still prints the count",
			conf:     Confirmer{},
			req:      Request{Op: Op{Command: CmdDelete, Selector: SelectorBulk, TrashEnabled: true}, Target: "crm-contacts", Affected: 12},
			wantErr:  true,
			wantCode: apierr.CodeConfirmationRequired,
			wantOut:  []string{"12 document(s)", "crm-contacts"},
		},
		{
			name:    "L3 with --yes still prints the resolved match count",
			conf:    Confirmer{AssumeYes: true},
			req:     Request{Op: Op{Command: CmdUpdate, Selector: SelectorBulk}, Target: "pages", Affected: 3},
			wantOut: []string{"3 document(s)"},
		},
		{
			name:      "--dry-run short-circuits every prompt",
			conf:      Confirmer{DryRun: true},
			req:       Request{Op: Op{Command: CmdDelete, Selector: SelectorBulk}, Target: "pages", Affected: 1200},
			wantNoOut: []string{"Proceed"},
		},
		{
			// §12.4's irreversibility line states WHY there is no restore, and
			// "this collection has no trash" is a claim about the schema: on a
			// trash-enabled collection --permanent is the reason, and printing
			// the other sentence is simply false.
			name:      "--permanent on a trash-enabled collection blames --permanent, not the schema",
			conf:      Confirmer{},
			req:       Request{Op: Op{Command: CmdDelete, Selector: SelectorID, Permanent: true, TrashEnabled: true}, Target: "crm-tags"},
			wantErr:   true,
			wantCode:  apierr.CodeConfirmationRequired,
			wantOut:   []string{"IRREVERSIBLE", "--permanent bypasses this collection's trash"},
			wantNoOut: []string{"no trash"},
		},
		{
			name:      "a collection with no trash still says so",
			conf:      Confirmer{},
			req:       Request{Op: Op{Command: CmdDelete, Selector: SelectorID}, Target: "pages"},
			wantErr:   true,
			wantCode:  apierr.CodeConfirmationRequired,
			wantOut:   []string{"IRREVERSIBLE", "this collection has no trash, so there is no restore"},
			wantNoOut: []string{"--permanent"},
		},
		{
			name:      "--quiet suppresses the notice but not the decision",
			conf:      Confirmer{Quiet: true, AssumeYes: true},
			req:       Request{Op: Op{Command: CmdUpdate, Selector: SelectorBulk}, Target: "pages", Affected: 3},
			wantNoOut: []string{"3 document(s)"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			c := tc.conf
			c.Out = &out
			c.In = strings.NewReader(tc.input)

			err := c.Confirm(tc.req)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Confirm() = nil, want %s", tc.wantCode)
				}
				if got := apierr.CodeOf(err); got != tc.wantCode {
					t.Fatalf("code = %s, want %s", got, tc.wantCode)
				}
				if got := apierr.ExitCode(err); got != apierr.ExitConfirmationRequired {
					t.Fatalf("exit = %d, want %d", got, apierr.ExitConfirmationRequired)
				}
			} else if err != nil {
				t.Fatalf("Confirm() = %v, want nil", err)
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output %q missing %q", out.String(), want)
				}
			}
			for _, unwanted := range tc.wantNoOut {
				if strings.Contains(out.String(), unwanted) {
					t.Errorf("output %q unexpectedly contains %q", out.String(), unwanted)
				}
			}
		})
	}
}

func TestRequestLine(t *testing.T) {
	tests := []struct {
		name string
		req  Request
		want string
	}{
		{
			name: "explicit summary wins",
			req:  Request{Op: Op{Command: CmdDelete}, Summary: "custom"},
			want: "custom",
		},
		{
			name: "soft delete reads as trash",
			req:  Request{Op: Op{Command: CmdDelete, Selector: SelectorID, TrashEnabled: true}, Target: "pages"},
			want: `trash will affect 1 document in "pages"`,
		},
		{
			name: "bulk with an unresolved count",
			req:  Request{Op: Op{Command: CmdUpdate, Selector: SelectorBulk}, Target: "pages", Affected: -1},
			want: `update will affect every document matching --where in "pages"`,
		},
		{
			name: "global",
			req:  Request{Op: Op{Command: CmdGlobalsUpdate, Global: true}, Target: "header"},
			want: `globals update will modify the global "header"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.req.Line(); got != tc.want {
				t.Fatalf("Line() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestConfirmPromptWithNoInputIsExit11(t *testing.T) {
	var out bytes.Buffer
	c := Confirmer{Out: &out, TTY: true} // In is nil: EOF immediately
	err := c.Confirm(Request{Op: Op{Command: CmdDelete, Selector: SelectorID, Permanent: true}, Target: "pages"})
	if apierr.CodeOf(err) != apierr.CodeConfirmationRequired {
		t.Fatalf("Confirm() = %v, want confirmation_required", err)
	}
}
