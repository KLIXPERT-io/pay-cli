package cli

import (
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

func TestRestoreCommandWiring(t *testing.T) {
	cmd := newRestoreCmd(nil)
	if cmd.Name() != "restore" {
		t.Fatalf("name = %q", cmd.Name())
	}
	for _, name := range []string{"depth", "select", "locale"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("pay restore is missing --%s", name)
		}
	}
	if !strings.Contains(cmd.Long, "cannot bring back a document removed with --permanent") {
		t.Fatal("restore help must say what it cannot undo")
	}
}

// TestRestoreIsRecoverable: un-trashing is the inverse of a soft delete, so it
// is L1 and never prompts by default.
func TestRestoreIsRecoverable(t *testing.T) {
	op := safety.Op{Command: safety.CmdRestore, Selector: safety.SelectorID, TrashEnabled: true}
	if got := op.Level(); got != safety.L1 {
		t.Fatalf("level = %v, want L1", got)
	}
	if !op.Level().Audited() {
		t.Fatal("every write is audited (§12.7)")
	}
}
