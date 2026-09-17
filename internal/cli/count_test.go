package cli

import (
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

// TestCountEnvelopeShape pins §10.1's count envelope: data is the bare number,
// which stays in the JSON even at zero.
func TestCountEnvelopeShape(t *testing.T) {
	env := output.New(safety.CmdCount, output.KindCount, 0).WithTarget(testTarget().envTarget(nil))
	b, err := env.Compact()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"data":0`) {
		t.Fatalf("a zero count must still be emitted: %s", b)
	}
	if !strings.Contains(string(b), `"data_kind":"count"`) {
		t.Fatalf("data_kind = %s", b)
	}
}

// TestCountSendsOnlyWhereAndTrash mirrors Client.Count's contract: the count
// route honours nothing else, so nothing else is built for it.
func TestCountSendsOnlyWhereAndTrash(t *testing.T) {
	cmd, f := readCmd()
	f.limit = 20
	f.where = []string{"title contains launch"}
	built, err := f.build(cmd, testDeps(), testTarget())
	if err != nil {
		t.Fatal(err)
	}
	if built.Params.Where.IsEmpty() {
		t.Fatal("the where clause was lost")
	}
	q, err := query.Params{Where: built.Params.Where}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(q, "draft=") || strings.Contains(q, "limit=") {
		t.Fatalf("count query carries parameters the route ignores: %s", q)
	}
}
