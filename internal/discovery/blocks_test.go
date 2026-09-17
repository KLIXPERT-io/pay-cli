package discovery

import (
	"reflect"
	"strings"
	"testing"
)

func TestResolveBlocksOrder(t *testing.T) {
	interfaces := []string{"CallToActionBlock", "ContentBlock", "MediaBlock"}
	src := BlockSources{
		Configured:    map[string][]string{"layout": {"pinned"}},
		ProjectSource: []string{"cta", "content", "mediaBlock"},
		Observed:      map[string][]string{"layout": {"observedBlock"}},
	}
	got := ResolveBlocks("layout", interfaces, src)
	if got.Source != SourceConfigured || !reflect.DeepEqual(got.Slugs, []string{"pinned"}) {
		t.Fatalf("configured must win: %+v", got)
	}

	src.Configured = nil
	got = ResolveBlocks("layout", interfaces, src)
	if got.Source != SourceProjectSource || !reflect.DeepEqual(got.Slugs, []string{"content", "cta", "mediaBlock"}) {
		t.Fatalf("project source must come second: %+v", got)
	}

	src.ProjectSource = nil
	got = ResolveBlocks("layout", interfaces, src)
	if got.Source != SourceObserved {
		t.Fatalf("observed must come third: %+v", got)
	}

	src.Observed = nil
	got = ResolveBlocks("layout", interfaces, src)
	if got.Slugs != nil || got.Source != SourceUnknown {
		t.Fatalf("unresolved must be nil/unknown: %+v", got)
	}
	// The interfaceNames must never leak out as slugs: feeding
	// CallToActionBlock back to the API is exactly the silent wrong answer
	// this function exists to prevent.
	for _, s := range got.Slugs {
		for _, iface := range interfaces {
			if s == iface {
				t.Fatalf("interfaceName %q was returned as a blockType slug", s)
			}
		}
	}
	if !strings.Contains(got.Reason, "cannot be determined from the Payload API") {
		t.Errorf("reason does not say so in plain words: %q", got.Reason)
	}
}

func TestDescribeBlocksHelpNamesOnlySourcesThatCanAnswer(t *testing.T) {
	help := DescribeBlocksHelp([]string{"CallToActionBlock", "ContentBlock", "MediaBlock"}, "local", "layout")
	for _, want := range []string{
		"payload.config.ts",
		"src/blocks/*/config.ts",
		"slug:",
		"pay config set profiles.local.blocks.layout",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("help is missing %q:\n%s", want, help)
		}
	}
	// The help must not tell the reader to look in a document sample: §7.10
	// verified every live page has an empty layout.
	if strings.Contains(help, "pay find") {
		t.Error("the help points at a command that cannot answer it")
	}
}

func TestUnresolvedBlocksReasonQuotesTheField(t *testing.T) {
	got := UnresolvedBlocksReason("layout")
	if !strings.Contains(got, `"layout"`) {
		t.Errorf("reason = %q", got)
	}
}
