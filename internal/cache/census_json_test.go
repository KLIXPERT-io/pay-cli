package cache

import (
	"encoding/json"
	"strings"
	"testing"
)

// The census keys maps by schema names; secret-LOOKING names keep their
// structure and counts, while scalar samples under them and JWT-shaped strings
// anywhere are withheld.
func TestCensusJSONRedactsValuesNotNames(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.abcdefghij"
	body := map[string]any{
		"census_version": 1, "scope": "0123456789abcdef",
		"collections": map[string]any{"pages": map[string]any{
			"types": map[string]any{"tokenList": map[string]any{
				"count": 3,
				"fields": map[string]any{
					"csrfToken": map[string]any{"kind": "scalar", "present": 3, "values": []any{map[string]any{"value": "abc", "count": 2}}},
					"title":     map[string]any{"kind": "scalar", "values": []any{map[string]any{"value": jwt, "count": 2}}},
				},
			}},
			"paths":  map[string]any{"layout": map[string]any{"types": map[string]any{"tokenList": 3}}},
			"apiKey": "plain-secret",
		}},
	}
	data, err := censusJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	pages := back["collections"].(map[string]any)["pages"].(map[string]any)
	tl := pages["types"].(map[string]any)["tokenList"].(map[string]any)
	fields := tl["fields"].(map[string]any)
	csrf, ok := fields["csrfToken"].(map[string]any)
	if !ok || csrf["kind"] != "scalar" || csrf["present"] != float64(3) {
		t.Fatalf("csrfToken lost its structure: %v", fields["csrfToken"])
	}
	if _, has := csrf["values"]; has {
		t.Error("values of a secret-named field were persisted")
	}
	if strings.Contains(string(data), jwt) {
		t.Error("a JWT-shaped value was persisted")
	}
	if pages["paths"].(map[string]any)["layout"].(map[string]any)["types"].(map[string]any)["tokenList"] != float64(3) {
		t.Error("a count under a secret-looking slug was not kept")
	}
	if pages["apiKey"] != "<redacted>" {
		t.Errorf("a string under a secret key = %v", pages["apiKey"])
	}
}

// WriteCensus persists through censusJSON: a census whose field is named like
// a secret reads back with that field's profile intact.
func TestWriteCensusKeepsSecretLookingFieldNames(t *testing.T) {
	sc, err := NewScope(base())
	if err != nil {
		t.Fatal(err)
	}
	s := New(t.TempDir())
	body := map[string]any{"census_version": 1, "scope": sc.Key, "collections": map[string]any{"pages": map[string]any{
		"types": map[string]any{"embed": map[string]any{"fields": map[string]any{
			"hash": map[string]any{"kind": "scalar", "present": 2, "values": []any{map[string]any{"value": "#top", "count": 2}}},
		}}},
	}}}
	if ok, w := s.WriteCensus(sc, body); !ok {
		t.Fatalf("write: %v", w)
	}
	raw, ok, _ := s.ReadCensus(sc)
	if !ok || strings.Contains(string(raw), `"hash": "<redacted>"`) || !strings.Contains(string(raw), `"present": 2`) {
		t.Errorf("census on disk:\n%s", raw)
	}
	if strings.Contains(string(raw), "#top") {
		t.Error("the value of a secret-named field reached the disk")
	}
}
