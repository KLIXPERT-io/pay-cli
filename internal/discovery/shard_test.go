package discovery

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

// TestFieldKeySetIsExactAndTotal is §7.8.2's central contract: an agent parses
// a field entry without presence checks, so every key must be present on every
// entry and no extra key may appear.
func TestFieldKeySetIsExactAndTotal(t *testing.T) {
	fields := []Field{
		NewField("status", "status"),
		{
			Name: "company", Path: "company", GraphQLPath: "company",
			PayloadType: TypeRelationship, PayloadTypeConfidence: ConfidenceGraphQL,
			GraphQLType: strPtr("CrmCompany"), JSONType: "number|object",
			Required: boolPtr(false), RequiredSource: SourceGraphQLInput,
			LocalizedSource: SourceUnknown, OptionsSource: SourceNA,
			RelationTo: []string{"crm-companies"}, RelationToSource: SourceGraphQL,
			WriteShape: strPtr(WriteShapeID), Queryable: true,
			Operators: []string{"equals"}, Sortable: true,
			SortableConfidence: ConfidenceHeuristic, HookMutated: HookMutatedUnknown,
			Label: "Company",
		},
	}
	want := append([]string(nil), FieldKeys...)
	sort.Strings(want)
	for i, f := range fields {
		b, err := json.Marshal(f)
		if err != nil {
			t.Fatalf("marshal field %d: %v", i, err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("unmarshal field %d: %v", i, err)
		}
		got := make([]string, 0, len(m))
		for k := range m {
			got = append(got, k)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("field %d key set:\n got %v\nwant %v", i, got, want)
		}
	}
}

func TestNewFieldStartsHonest(t *testing.T) {
	f := NewField("hero", "hero")
	if f.Required != nil {
		t.Error("required must start null, not false: a defaulted false is a claim PayCLI has not earned")
	}
	if f.Localized != nil {
		t.Error("localized must start null (§7.9d)")
	}
	if f.PayloadType != TypeUnknown || f.PayloadTypeConfidence != ConfidenceUnknown {
		t.Errorf("payload_type = %q/%q, want unknown/unknown", f.PayloadType, f.PayloadTypeConfidence)
	}
	if f.HookMutated != HookMutatedUnknown {
		t.Errorf("hook_mutated = %q", f.HookMutated)
	}
}

func TestGraphQLPathUsesDoubleUnderscore(t *testing.T) {
	f := NewField("url", "hero.links.link.url")
	if f.GraphQLPath != "hero__links__link__url" {
		t.Errorf("graphql_path = %q", f.GraphQLPath)
	}
}

func TestWriteShapeFor(t *testing.T) {
	tests := []struct {
		payloadType string
		poly        bool
		many        bool
		want        *string
	}{
		{TypeRelationship, false, false, strPtr(WriteShapeID)},
		{TypeRelationship, false, true, strPtr(WriteShapeIDArray)},
		{TypeRelationship, true, false, strPtr(WriteShapeRelValue)},
		{TypeRelationship, true, true, strPtr(WriteShapeRelList)},
		{TypeUpload, false, false, strPtr(WriteShapeID)},
		{TypeUpload, true, true, strPtr(WriteShapeRelList)},
		{TypeRichText, false, false, strPtr(WriteShapeJSON)},
		{TypeJSON, false, false, strPtr(WriteShapeJSON)},
		{TypeBlocks, false, true, strPtr(WriteShapeJSON)},
		{TypeText, false, false, nil},
		{TypeNumber, false, true, nil},
		{TypeGroup, false, false, nil},
	}
	for _, tt := range tests {
		got := WriteShapeFor(tt.payloadType, tt.poly, tt.many)
		switch {
		case got == nil && tt.want == nil:
		case got == nil || tt.want == nil:
			t.Errorf("WriteShapeFor(%s,%v,%v) = %v, want %v", tt.payloadType, tt.poly, tt.many, got, tt.want)
		case *got != *tt.want:
			t.Errorf("WriteShapeFor(%s,%v,%v) = %q, want %q", tt.payloadType, tt.poly, tt.many, *got, *tt.want)
		}
	}
}

func TestShardFinalizeComputesRequiredPathsAndHash(t *testing.T) {
	s := NewShard("gen", "pages")
	a := NewField("title", "title")
	a.Required = boolPtr(true)
	b := NewField("slug", "slug")
	b.Required = boolPtr(true)
	c := NewField("body", "body")
	c.Required = boolPtr(false)
	s.Fields = []Field{a, b, c}
	s.Finalize()

	if !reflect.DeepEqual(s.RequiredPaths, []string{"slug", "title"}) {
		t.Errorf("required_paths = %v", s.RequiredPaths)
	}
	if s.SHA256 == "" {
		t.Fatal("Finalize must stamp the content hash the index refers to")
	}

	// The hash must not depend on the generation, or a fresh run would always
	// look like a schema change to §8.4.
	s2 := NewShard("another-generation", "pages")
	s2.Fields = []Field{a, b, c}
	s2.Finalize()
	if s2.SHA256 != s.SHA256 {
		t.Error("fields_sha256 changed with the generation id")
	}

	// A real field change must change it.
	s3 := NewShard("gen", "pages")
	d := a
	d.Required = boolPtr(false)
	s3.Fields = []Field{d, b, c}
	s3.Finalize()
	if s3.SHA256 == s.SHA256 {
		t.Error("fields_sha256 did not change when a field's required-ness did")
	}
}

func TestQueryablePathsIsNilWhenNothingIsKnown(t *testing.T) {
	// query.Schema treats a nil slice as "never learned" and never rejects on
	// it. An empty non-nil slice would reject every path.
	empty := NewShard("g", "pages")
	if empty.QueryablePaths() != nil {
		t.Error("an empty shard must yield nil, not an empty slice")
	}
	if empty.SortablePaths() != nil {
		t.Error("an empty shard must yield nil for sortable paths too")
	}
	s := NewShard("g", "pages")
	f := NewField("title", "title")
	f.Queryable = true
	f.Sortable = true
	s.Fields = []Field{f}
	if got := s.QueryablePaths(); !reflect.DeepEqual(got, []string{"title"}) {
		t.Errorf("QueryablePaths() = %v", got)
	}
}

func TestHumanizeField(t *testing.T) {
	tests := map[string]string{
		"firstName":    "First Name",
		"first_name":   "First Name",
		"apiKey":       "Api Key",
		"APIKey":       "API Key",
		"slug":         "Slug",
		"crm-contacts": "Crm Contacts",
		"":             "",
	}
	for in, want := range tests {
		if got := HumanizeField(in); got != want {
			t.Errorf("HumanizeField(%q) = %q, want %q", in, got, want)
		}
	}
}
