package discovery

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestArchitecturalRules enforces §3.1's greps locally, so the package cannot
// drift between full `make lint` runs.
func TestArchitecturalRules(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	banned := []struct {
		pattern *regexp.Regexp
		why     string
	}{
		{regexp.MustCompile(`\btime\.Now\b`), "§3.1: only app.go reads the wall clock; Options.Now is injected"},
		{regexp.MustCompile(`\bos\.(Getenv|Stdout|Stderr|Stdin|Exit|Args)\b`), "§3.1: only app.go touches the process environment"},
		{regexp.MustCompile(`http\.NewRequest`), "§3.1: every *http.Request is built in internal/payload/transport.go"},
		{regexp.MustCompile(`WithReactiveInvalidation`), "§8.4(a): discovery deliberately generates every Level-3 trigger, so it must never arm one"},
		{regexp.MustCompile(`internal/cli`), "§3.1: internal/discovery never imports internal/cli"},
		{regexp.MustCompile(`spf13/cobra`), "§3.1: internal/discovery never imports cobra"},
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range banned {
			if loc := b.pattern.FindIndex(src); loc != nil {
				t.Errorf("%s contains %q — %s", name, src[loc[0]:loc[1]], b.why)
			}
		}
	}
}

func TestProvenanceConstantsAreDistinct(t *testing.T) {
	// A duplicated source string would make two different stories
	// indistinguishable in the manifest.
	sources := map[string]string{
		"SourceConfigured": SourceConfigured, "SourceProbed": SourceProbed,
		"SourceGraphQL": SourceGraphQL, "SourceGraphQLInput": SourceGraphQLInput,
		"SourceGraphQLEnum": SourceGraphQLEnum, "SourceProbe": SourceProbe,
		"SourceAccess": SourceAccess, "SourceObserved": SourceObserved,
		"SourceLocaleAll": SourceLocaleAll, "SourceLocaleAllUnverified": SourceLocaleAllUnverified,
		"SourceProjectSource": SourceProjectSource, "SourceProjectPackageJSON": SourceProjectPackageJSON,
		"SourceBulkDeleteMessage": SourceBulkDeleteMessage, "SourceDerived": SourceDerived,
		"SourceAccessZip": SourceAccessZip, "SourceTypeProbe": SourceTypeProbe,
		"SourceFuzzy": SourceFuzzy, "SourceBootstrap": SourceBootstrap,
		"SourceCached": SourceCached, "SourceInferred": SourceInferred,
		"SourceUnknown": SourceUnknown, "SourceNA": SourceNA,
	}
	seen := map[string]string{}
	for name, value := range sources {
		if prev, dup := seen[value]; dup {
			t.Errorf("%s and %s both equal %q", name, prev, value)
		}
		seen[value] = name
	}
}

func TestWriteShapeEnumIsClosed(t *testing.T) {
	// §9.10's --set coercion switches on exactly these five values.
	want := map[string]bool{
		"id": true, "id_array": true, "{relationTo,value}": true,
		"[{relationTo,value}]": true, "json": true,
	}
	for _, v := range []string{WriteShapeID, WriteShapeIDArray, WriteShapeRelValue, WriteShapeRelList, WriteShapeJSON} {
		if !want[v] {
			t.Errorf("unexpected write shape %q", v)
		}
		delete(want, v)
	}
	if len(want) != 0 {
		t.Errorf("missing write shapes: %v", want)
	}
}

func TestIDTypeSentinelIsUnknownNotEmpty(t *testing.T) {
	// "" would be indistinguishable from a missing key; "unknown" is the
	// honest value every consumer must render as such.
	if IDTypeUnknown != "unknown" {
		t.Fatalf("IDTypeUnknown = %q", IDTypeUnknown)
	}
	if HookMutatedUnknown != "unknown" {
		t.Fatalf("HookMutatedUnknown = %q", HookMutatedUnknown)
	}
}
