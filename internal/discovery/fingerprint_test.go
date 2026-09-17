package discovery

import "testing"

func accessBody(readVersions bool, extra string) (map[string]any, map[string]any) {
	pages := map[string]any{
		"fields": true, "create": true, "read": true, "update": true, "delete": true,
	}
	if readVersions {
		pages["readVersions"] = true
	}
	colls := map[string]any{"pages": pages}
	if extra != "" {
		colls[extra] = map[string]any{"read": true}
	}
	globals := map[string]any{"header": map[string]any{"read": true, "update": true}}
	return colls, globals
}

func TestTopologySHA256DetectsTheChangesItMust(t *testing.T) {
	base, globals := accessBody(false, "")
	baseFP := TopologySHA256(ProjectTopology(base, globals, true))
	if baseFP == "" {
		t.Fatal("empty fingerprint")
	}

	tests := []struct {
		name       string
		mutate     func() (map[string]any, map[string]any, bool)
		wantChange bool
	}{
		{"identical", func() (map[string]any, map[string]any, bool) {
			c, g := accessBody(false, "")
			return c, g, true
		}, false},
		{"values changed but key set did not", func() (map[string]any, map[string]any, bool) {
			c, g := accessBody(false, "")
			c["pages"].(map[string]any)["read"] = false
			return c, g, true
		}, false},
		{"readVersions appeared", func() (map[string]any, map[string]any, bool) {
			c, g := accessBody(true, "")
			return c, g, true
		}, true},
		{"collection added", func() (map[string]any, map[string]any, bool) {
			c, g := accessBody(false, "posts")
			return c, g, true
		}, true},
		{"canAccessAdmin flipped", func() (map[string]any, map[string]any, bool) {
			c, g := accessBody(false, "")
			return c, g, false
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, g, admin := tt.mutate()
			got := TopologySHA256(ProjectTopology(c, g, admin))
			changed := got != baseFP
			if changed != tt.wantChange {
				t.Fatalf("changed = %v, want %v", changed, tt.wantChange)
			}
		})
	}
}

func TestTopologyKeepsCollapsedEntries(t *testing.T) {
	// A restricted key collapses an entry; dropping it would hash the same as
	// the collection having been removed.
	full := map[string]any{"pages": map[string]any{"read": true}}
	collapsed := map[string]any{"pages": true}
	removed := map[string]any{}
	a := TopologySHA256(ProjectTopology(full, nil, true))
	b := TopologySHA256(ProjectTopology(collapsed, nil, true))
	c := TopologySHA256(ProjectTopology(removed, nil, true))
	if a == b || b == c || a == c {
		t.Fatalf("collapsed and removed entries must hash differently: %s %s %s", a[:8], b[:8], c[:8])
	}
}

func TestSchemaSHA256IsOrderIndependent(t *testing.T) {
	a := SchemaSHA256([]string{"Pages", "Page", "countPages"})
	b := SchemaSHA256([]string{"countPages", "Pages", "Page"})
	if a != b {
		t.Fatal("the fingerprint must not depend on schema field order")
	}
	if SchemaSHA256(nil) != "" {
		t.Fatal("an empty field list has no fingerprint; it must not hash to a constant")
	}
	if SchemaSHA256([]string{"Pages"}) == a {
		t.Fatal("a different field set must produce a different fingerprint")
	}
}
