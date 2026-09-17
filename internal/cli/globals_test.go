package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

func TestGlobalUnknownSuggests(t *testing.T) {
	d := testDeps()
	if _, err := d.global(context.Background(), "header"); err != nil {
		t.Fatalf("a known global must resolve: %v", err)
	}
	_, err := d.global(context.Background(), "headr")
	if err == nil {
		t.Fatal("expected global_unknown")
	}
	if apierr.CodeOf(err) != apierr.CodeGlobalUnknown {
		t.Fatalf("code = %s", apierr.CodeOf(err))
	}
	e, _ := apierr.As(err)
	if len(e.DidYouMean) == 0 || e.DidYouMean[0] != "header" {
		t.Fatalf("did_you_mean = %v", e.DidYouMean)
	}
}

func TestGlobalsNilManifestNeverRejects(t *testing.T) {
	d := &Deps{}
	g, err := d.global(context.Background(), "anything")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if g.Global != nil {
		t.Fatal("expected an empty target")
	}
}

func TestGlobalsCommandTree(t *testing.T) {
	cmd := newGlobalsCmd(nil)
	names := map[string]bool{}
	for _, sub := range cmd.Commands() {
		names[sub.Name()] = true
	}
	for _, want := range []string{"list", "get", "update"} {
		if !names[want] {
			t.Fatalf("pay globals is missing %q (have %v)", want, names)
		}
	}
	if !strings.Contains(cmd.Long, "POST, not a PATCH") {
		t.Fatal("globals help must state that an update is a POST")
	}
}

func TestGlobalTargetEnvelope(t *testing.T) {
	d := testDeps()
	g, err := d.global(context.Background(), "header")
	if err != nil {
		t.Fatal(err)
	}
	tgt := g.envTarget()
	if tgt.Kind != "global" || tgt.Slug != "header" || tgt.Singular != "Header" {
		t.Fatalf("target = %+v", tgt)
	}
	if tgt.ID != nil {
		t.Fatal("a global has no id")
	}
}
