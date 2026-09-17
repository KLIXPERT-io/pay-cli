package discovery

import (
	"encoding/json"
	"testing"
	"time"
)

func TestNewManifestHasNoNullSlices(t *testing.T) {
	m := NewManifest()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// An agent iterates these without a presence check, so a null here would
	// be a nil-pointer panic in every consumer language.
	for _, key := range []string{
		"collections", "globals", "unreachable", "limitations",
		"unsupported_operators", "learned_operator_failures",
	} {
		v, ok := decoded[key]
		if !ok {
			t.Errorf("manifest is missing %q", key)
			continue
		}
		if v == nil {
			t.Errorf("%q is null; it must be an empty array", key)
		}
	}
	if decoded["manifest_version"] != float64(ManifestVersion) {
		t.Errorf("manifest_version = %v, want %d", decoded["manifest_version"], ManifestVersion)
	}
}

func TestDecodeManifestVersionMismatchIsAMiss(t *testing.T) {
	tests := []struct {
		name string
		body string
		ok   bool
	}{
		{"current", `{"manifest_version":2}`, true},
		{"v1 on disk", `{"manifest_version":1}`, false},
		{"absent", `{}`, false},
		{"not json", `not json`, false},
		{"future", `{"manifest_version":99}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := DecodeManifest([]byte(tt.body))
			if ok != tt.ok {
				t.Fatalf("DecodeManifest(%s) ok = %v, want %v", tt.body, ok, tt.ok)
			}
		})
	}
}

func TestAddLimitationDeduplicatesAndCarriesMitigation(t *testing.T) {
	m := NewManifest()
	m.AddLimitation(LimIDTypeUnknown, "pages", "", "detail")
	m.AddLimitation(LimIDTypeUnknown, "pages", "", "other detail")
	m.AddLimitation(LimIDTypeUnknown, "posts", "", "")
	if len(m.Limitations) != 2 {
		t.Fatalf("limitations = %d, want 2", len(m.Limitations))
	}
	if m.Limitations[0].Mitigation == "" {
		t.Error("a limitation was written without its mitigation; §7.8.3 requires a value and its story together")
	}
}

func TestEveryLimitationCodeHasAMitigation(t *testing.T) {
	codes := []string{
		LimFieldsUnavailable, LimSelectOptionsUnavailable, LimBlockSlugsUnknown,
		LimCustomEndpointsNotEnum, LimSortabilityHeuristic, LimLocalizationUnknown,
		LimRichTextShapeUnknown, LimIDTypeUnknown, LimCapabilityUnknown,
		LimLabelsUnavailable, LimLocalesUnknown, LimLocalizationPerFieldUnknwn,
		LimBlockSlugsUnknownNoSource, LimAuthCandidatesTruncated, LimHookMutationUnknown,
		LimPayloadVersionUnknown, LimDBAdapterUnknown,
	}
	for _, code := range codes {
		if Mitigation(code) == "" {
			t.Errorf("limitation %s has no recorded mitigation", code)
		}
	}
}

func TestIsInternal(t *testing.T) {
	tests := map[string]bool{
		"payload-jobs":       true,
		"payload-migrations": true,
		"payloadthing":       false,
		"pages":              false,
		"crm-contacts":       false,
	}
	for slug, want := range tests {
		if got := IsInternal(slug); got != want {
			t.Errorf("IsInternal(%q) = %v, want %v", slug, got, want)
		}
	}
}

func TestSortIsDeterministic(t *testing.T) {
	m := NewManifest()
	m.Collections = []*Collection{{Slug: "posts"}, {Slug: "categories"}, {Slug: "pages"}}
	m.Globals = []*Global{{Slug: "header"}, {Slug: "footer"}}
	m.AddUnreachable("zeta", kindCollection, ReasonAccessDenied, "")
	m.AddUnreachable("alpha", kindCollection, ReasonAccessDenied, "")
	m.Sort()
	if m.Collections[0].Slug != "categories" || m.Collections[2].Slug != "posts" {
		t.Errorf("collections not sorted: %v", m.CollectionSlugs())
	}
	if m.Globals[0].Slug != "footer" {
		t.Errorf("globals not sorted: %v", m.GlobalSlugs())
	}
	if m.Unreachable[0].Slug != "alpha" {
		t.Errorf("unreachable not sorted: %v", m.Unreachable)
	}
}

func TestResolveCollectionSuggests(t *testing.T) {
	m := NewManifest()
	m.Collections = []*Collection{{Slug: "pages"}, {Slug: "posts"}}
	if _, err := m.ResolveCollection("pages"); err != nil {
		t.Fatalf("known slug: %v", err)
	}
	_, err := m.ResolveCollection("page")
	if err == nil {
		t.Fatal("an unknown slug must be an error")
	}
	if got := err.Error(); got == "" {
		t.Fatal("empty error message")
	}
}

func TestRevisionIsStable(t *testing.T) {
	m := NewManifest()
	m.Generation = "01K5Q7TZ4V3B8CJK2M9N0P1Q2R"
	if got := m.Revision(); got != m.Generation {
		t.Errorf("with no fingerprints Revision() = %q, want the generation", got)
	}
	m.Fingerprint.TopologySHA256 = "0df77681bf2d6bfc"
	m.Fingerprint.SchemaSHA256 = "c0fe892dd4be665a"
	if got, want := m.Revision(), "0df77681-c0fe892d"; got != want {
		t.Errorf("Revision() = %q, want %q", got, want)
	}
	m.GeneratedAt = time.Unix(0, 0)
	if got, want := m.Revision(), "0df77681-c0fe892d"; got != want {
		t.Errorf("Revision() changed with the clock: %q", got)
	}
}
