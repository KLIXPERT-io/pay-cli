package discovery

import (
	"encoding/json"
	"testing"
)

func TestIDTypeFromArg(t *testing.T) {
	tests := []struct {
		name   string
		field  *IntroField
		want   string
		wantOK bool
	}{
		{"int id", &IntroField{Name: "Page", Args: []IntroArg{
			{Name: "id", Type: nonNull(scalar("Int"))}}}, IDTypeNumber, true},
		{"string id", &IntroField{Name: "Page", Args: []IntroArg{
			{Name: "id", Type: nonNull(scalar("String"))}}}, IDTypeString, true},
		// A global's singular Query field takes no id at all
		// (Query.Header.args == [draft, select], verified live).
		{"global has no id arg", &IntroField{Name: "Header", Args: []IntroArg{
			{Name: "draft", Type: scalar("Boolean")}}}, IDTypeUnknown, false},
		{"nil", nil, IDTypeUnknown, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := IDTypeFromArg(tt.field)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("IDTypeFromArg = %q,%v want %q,%v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestResolveIDTypeLadder(t *testing.T) {
	tests := []struct {
		name       string
		in         IDLadderInput
		wantType   string
		wantSource string
	}{
		{"graphql wins", IDLadderInput{GraphQL: IDTypeNumber, SampleID: "abc", SampleIDPresent: true, Configured: IDTypeString},
			IDTypeNumber, SourceGraphQL},
		{"observed number", IDLadderInput{SampleID: json.Number("16"), SampleIDPresent: true}, IDTypeNumber, SourceObserved},
		{"observed string", IDLadderInput{SampleID: "66f1a2b3c4d5e6f708192a3b", SampleIDPresent: true}, IDTypeString, SourceObserved},
		{"versions parent", IDLadderInput{VersionParentID: json.Number("16"), VersionParentPresent: true},
			IDTypeNumber, SourceObserved},
		{"profile pin", IDLadderInput{Configured: IDTypeString}, IDTypeString, SourceConfigured},
		// The last rung is reachable on purpose: a defaulted "number" would
		// reject every valid 24-hex ObjectId with a message that is a lie.
		{"unknown", IDLadderInput{}, IDTypeUnknown, SourceUnknown},
		{"empty collection with no versions", IDLadderInput{SampleIDPresent: false}, IDTypeUnknown, SourceUnknown},
		{"null id is not a type", IDLadderInput{SampleID: nil, SampleIDPresent: true}, IDTypeUnknown, SourceUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotType, gotSource := ResolveIDType(tt.in)
			if gotType != tt.wantType || gotSource != tt.wantSource {
				t.Fatalf("ResolveIDType = %q/%q want %q/%q", gotType, gotSource, tt.wantType, tt.wantSource)
			}
		})
	}
}

func TestInferDBAdapter(t *testing.T) {
	tests := []struct {
		name       string
		idTypes    []string
		sampleIDs  []string
		wantAdptr  string
		wantSource string
	}{
		{"all numeric", []string{IDTypeNumber, IDTypeNumber}, nil, DBPostgres, SourceInferred},
		{"object ids", []string{IDTypeString}, []string{"66f1a2b3c4d5e6f708192a3b"}, DBMongoDB, SourceInferred},
		{"uuids", []string{IDTypeString}, []string{"3f2504e0-4f89-11d3-9a0c-0305e82c3301"}, DBPostgres, SourceInferred},
		{"mixed", []string{IDTypeNumber, IDTypeString}, nil, DBUnknown, SourceUnknown},
		{"string with no sample", []string{IDTypeString}, nil, DBUnknown, SourceUnknown},
		{"nothing", nil, nil, DBUnknown, SourceUnknown},
		{"only unknowns", []string{IDTypeUnknown}, nil, DBUnknown, SourceUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, s := InferDBAdapter(tt.idTypes, tt.sampleIDs)
			if a != tt.wantAdptr || s != tt.wantSource {
				t.Fatalf("InferDBAdapter = %q/%q want %q/%q", a, s, tt.wantAdptr, tt.wantSource)
			}
		})
	}
}

func TestIDTypeOfJSON(t *testing.T) {
	cases := []struct {
		v      any
		want   string
		wantOK bool
	}{
		{json.Number("1"), IDTypeNumber, true},
		{float64(1), IDTypeNumber, true},
		{"x", IDTypeString, true},
		{nil, IDTypeUnknown, false},
		{true, IDTypeUnknown, false},
		{map[string]any{}, IDTypeUnknown, false},
	}
	for _, c := range cases {
		got, ok := IDTypeOfJSON(c.v)
		if got != c.want || ok != c.wantOK {
			t.Errorf("IDTypeOfJSON(%#v) = %q,%v want %q,%v", c.v, got, ok, c.want, c.wantOK)
		}
	}
}

func TestLooksNumeric(t *testing.T) {
	for in, want := range map[string]bool{
		"66": true, "-1": true, "66f1a2b3c4d5e6f708192a3b": false, "": false, "-": false, "1.5": false,
	} {
		if got := LooksNumeric(in); got != want {
			t.Errorf("LooksNumeric(%q) = %v, want %v", in, got, want)
		}
	}
}
