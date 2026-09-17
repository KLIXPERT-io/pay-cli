package discovery

import (
	"reflect"
	"testing"
)

func scalar(name string) *TypeRef  { return &TypeRef{Kind: KindScalar, Name: name} }
func object(name string) *TypeRef  { return &TypeRef{Kind: KindObject, Name: name} }
func enumRef(name string) *TypeRef { return &TypeRef{Kind: KindEnum, Name: name} }
func nonNull(t *TypeRef) *TypeRef  { return &TypeRef{Kind: KindNonNull, OfType: t} }
func listOf(t *TypeRef) *TypeRef   { return &TypeRef{Kind: KindList, OfType: t} }

func TestTypeRefNamed(t *testing.T) {
	tests := []struct {
		name     string
		ref      *TypeRef
		wantName string
		wantKind string
		nonNull  bool
		list     bool
	}{
		{"scalar", scalar("String"), "String", KindScalar, false, false},
		{"non-null scalar", nonNull(scalar("String")), "String", KindScalar, true, false},
		{"list of non-null union", nonNull(listOf(nonNull(&TypeRef{Kind: KindUnion, Name: "Page_Layout"}))),
			"Page_Layout", KindUnion, true, true},
		{"nil", nil, "", "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, kind, nn, list := tt.ref.Named()
			if name != tt.wantName || kind != tt.wantKind || nn != tt.nonNull || list != tt.list {
				t.Errorf("Named() = %q,%q,%v,%v want %q,%q,%v,%v",
					name, kind, nn, list, tt.wantName, tt.wantKind, tt.nonNull, tt.list)
			}
		})
	}
}

func testSchema() *Schema {
	return &Schema{
		SlugBySingular: map[string]string{
			"Page": "pages", "Post": "posts", "Media": "media", "CrmCompany": "crm-companies",
		},
		Types: map[string]*IntroType{
			"Page__status": {Kind: KindEnum, Name: "Page__status", EnumValues: []IntroEnumValue{{Name: "draft"}, {Name: "published"}}},
			"Page_Layout": {Kind: KindUnion, Name: "Page_Layout", PossibleTypes: []IntroTypeName{
				{Name: "CallToActionBlock"}, {Name: "ContentBlock"},
			}},
			"Media": {Kind: KindObject, Name: "Media", Fields: []IntroField{
				{Name: "filename", Type: scalar("String")},
				{Name: "mimeType", Type: scalar("String")},
				{Name: "filesize", Type: scalar("Float")},
				{Name: "url", Type: scalar("String")},
			}},
			"CrmCompany": {Kind: KindObject, Name: "CrmCompany", Fields: []IntroField{
				{Name: "id", Type: nonNull(scalar("Int"))},
				{Name: "name", Type: scalar("String")},
			}},
			"CrmContact_Activities": {Kind: KindObject, Name: "CrmContact_Activities", Fields: []IntroField{
				{Name: "docs", Type: listOf(object("CrmActivity"))},
				{Name: "hasNextPage", Type: scalar("Boolean")},
				{Name: "totalDocs", Type: scalar("Int")},
			}},
			"Page_Hero_Links_Link_Reference_Relationship": {Kind: KindObject, Fields: []IntroField{
				{Name: "relationTo", Type: enumRef("Page_Hero_Links_Link_Reference_RelationTo")},
				{Name: "value", Type: &TypeRef{Kind: KindUnion, Name: "Page_Hero_Links_Link_Reference"}},
			}},
			"Page_Hero_Links_Link_Reference_RelationTo": {Kind: KindEnum, EnumValues: []IntroEnumValue{{Name: "pages"}, {Name: "posts"}}},
			"Page_Hero_Links_Link_Reference": {Kind: KindUnion, PossibleTypes: []IntroTypeName{
				{Name: "Page"}, {Name: "Post"},
			}},
			"Page_Meta": {Kind: KindObject, Name: "Page_Meta", Fields: []IntroField{
				{Name: "title", Type: scalar("String")},
			}},
		},
	}
}

func TestInferKindTable(t *testing.T) {
	s := testSchema()
	tests := []struct {
		name        string
		ref         *TypeRef
		field       string
		wantType    string
		wantMany    bool
		wantPoly    bool
		wantRelTo   []string
		wantOptions []string
		wantJSON    string
		readOnly    bool
	}{
		{"enum select", enumRef("Page__status"), "_status", TypeSelect, false, false, nil,
			[]string{"draft", "published"}, "string", false},
		{"enum list select has_many", listOf(enumRef("Page__status")), "tags", TypeSelect, true, false, nil,
			[]string{"draft", "published"}, "string", false},
		{"join", object("CrmContact_Activities"), "activities", TypeJoin, true, false, nil, nil, "object", true},
		{"polymorphic relationship", object("Page_Hero_Links_Link_Reference_Relationship"), "reference",
			TypeRelationship, false, true, []string{"pages", "posts"}, nil, "object", false},
		{"monomorphic relationship", object("CrmCompany"), "company", TypeRelationship, false, false,
			[]string{"crm-companies"}, nil, numberOrObject, false},
		{"upload target", object("Media"), "image", TypeUpload, false, false, []string{"media"}, nil, numberOrObject, false},
		{"blocks union", nonNull(listOf(nonNull(&TypeRef{Kind: KindUnion, Name: "Page_Layout"}))), "layout",
			TypeBlocks, true, false, nil, nil, "array", false},
		{"json scalar as richText", scalar("JSON"), "richText", TypeRichText, false, false, nil, nil, "object", false},
		{"json scalar as json", scalar("JSON"), "enrichment", TypeJSON, false, false, nil, nil, "object", false},
		{"date", scalar("DateTime"), "createdAt", TypeDate, false, false, nil, nil, "string", false},
		{"email", scalar("EmailAddress"), "email", TypeEmail, false, false, nil, nil, "string", false},
		{"text", scalar("String"), "title", TypeText, false, false, nil, nil, "string", false},
		{"int", scalar("Int"), "count", TypeNumber, false, false, nil, nil, "number", false},
		{"float", scalar("Float"), "filesize", TypeNumber, false, false, nil, nil, "number", false},
		{"boolean", scalar("Boolean"), "enabled", TypeCheckbox, false, false, nil, nil, "boolean", false},
		{"group", object("Page_Meta"), "meta", TypeGroup, false, false, nil, nil, "object", false},
		{"int id is reported as id", nonNull(scalar("Int")), "id", TypeID, false, false, nil, nil, "number", true},
		{"string id is reported as id", nonNull(scalar("String")), "id", TypeID, false, false, nil, nil, "string", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := InferKind(tt.ref, "Page", tt.field, s)
			if k.PayloadType != tt.wantType {
				t.Errorf("payload_type = %q, want %q", k.PayloadType, tt.wantType)
			}
			if k.HasMany != tt.wantMany {
				t.Errorf("has_many = %v, want %v", k.HasMany, tt.wantMany)
			}
			if k.Polymorphic != tt.wantPoly {
				t.Errorf("polymorphic = %v, want %v", k.Polymorphic, tt.wantPoly)
			}
			if tt.wantRelTo != nil && !reflect.DeepEqual(k.RelationTo, tt.wantRelTo) {
				t.Errorf("relation_to = %v, want %v", k.RelationTo, tt.wantRelTo)
			}
			if tt.wantOptions != nil && !reflect.DeepEqual(k.Options, tt.wantOptions) {
				t.Errorf("options = %v, want %v", k.Options, tt.wantOptions)
			}
			if k.JSONType != tt.wantJSON {
				t.Errorf("json_type = %q, want %q", k.JSONType, tt.wantJSON)
			}
			if k.ReadOnly != tt.readOnly {
				t.Errorf("read_only = %v, want %v", k.ReadOnly, tt.readOnly)
			}
		})
	}
}

func TestInferKindReportsMissingLeaves(t *testing.T) {
	s := &Schema{Types: map[string]*IntroType{}, SlugBySingular: map[string]string{}}
	k := InferKind(enumRef("Page_status"), "Page", "status", s)
	if len(k.Leaves) != 1 || k.Leaves[0] != "Page_status" {
		t.Fatalf("leaves = %v, want [Page_status]", k.Leaves)
	}
	if k.OptionsSource != SourceUnknown {
		t.Errorf("options_source = %q, want unknown when the enum is unresolved", k.OptionsSource)
	}
}

func TestIsUploadTypeNeedsAllFourMarkers(t *testing.T) {
	full := testSchema().Type("Media")
	if !IsUploadType(full) {
		t.Fatal("Media must be recognised as an upload type")
	}
	partial := &IntroType{Fields: []IntroField{
		{Name: "filename"}, {Name: "mimeType"}, {Name: "url"},
	}}
	if IsUploadType(partial) {
		t.Error("three of four markers must not be enough")
	}
	if IsUploadType(nil) {
		t.Error("nil must not be an upload type")
	}
}

func TestIsJoinTypeMatchesOnKeySetNotName(t *testing.T) {
	join := testSchema().Type("CrmContact_Activities")
	if !IsJoinType(join) {
		t.Fatal("the {docs,hasNextPage,totalDocs} shape must be a join")
	}
	group := &IntroType{Name: "CrmContact_Activities", Fields: []IntroField{
		{Name: "docs"}, {Name: "hasNextPage"}, {Name: "totalDocs"}, {Name: "extra"},
	}}
	if IsJoinType(group) {
		t.Error("a fourth field means it is not Payload's join object")
	}
}

func TestSortableKind(t *testing.T) {
	notSortable := []string{TypeJoin, TypeBlocks, TypeRichText, TypeJSON, TypeGroup, TypeArray, TypePoint}
	for _, k := range notSortable {
		if SortableKind(k) {
			t.Errorf("%s must not be reported sortable", k)
		}
	}
	for _, k := range []string{TypeText, TypeNumber, TypeDate, TypeSelect, TypeRelationship} {
		if !SortableKind(k) {
			t.Errorf("%s must be reported sortable", k)
		}
	}
}
