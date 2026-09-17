package cli

import (
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// TestDuplicateRejectsANonCastableID: POST /{coll}/{id}/duplicate with an id
// that does not cast CREATES a document (verified), so the id check is not
// optional here.
func TestDuplicateRejectsANonCastableID(t *testing.T) {
	if err := apierr.CheckID("pages", "not-a-number", "number"); err == nil {
		t.Fatal("a non-castable id must be refused before /duplicate")
	}
	if err := apierr.CheckID("pages", "not-a-number", "unknown"); err != nil {
		t.Fatalf("an unknown id type must still send: %v", err)
	}
}

func TestDuplicateCommandWiring(t *testing.T) {
	cmd := newDuplicateCmd(nil)
	for _, name := range []string{"no-draft", "depth", "select", "locale"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("pay duplicate is missing --%s", name)
		}
	}
	if !strings.Contains(cmd.Long, "re-runs the collection's full") {
		t.Fatal("duplicate help must say what --no-draft costs")
	}
}
