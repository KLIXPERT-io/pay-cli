package discovery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// TopologyProjection is §8.4's Level-1 normalised projection of
// GET /api/access. Only key *sets* are hashed, never values, so a permission
// flip still changes the fingerprint (a collapsed entry has a different key
// set) while transient data does not.
type TopologyProjection struct {
	Collections map[string][]string `json:"c"`
	Globals     map[string][]string `json:"g"`
	Admin       bool                `json:"admin"`
}

// ProjectTopology builds the projection from a decoded /api/access body.
func ProjectTopology(collections, globals map[string]any, canAccessAdmin bool) TopologyProjection {
	return TopologyProjection{
		Collections: projectEntries(collections),
		Globals:     projectEntries(globals),
		Admin:       canAccessAdmin,
	}
}

func projectEntries(m map[string]any) map[string][]string {
	out := make(map[string][]string, len(m))
	for slug, v := range m {
		entry, ok := v.(map[string]any)
		if !ok {
			// A non-object entry still has a topology: the empty key set. It
			// must not be dropped, or removing a collection and collapsing one
			// would hash the same.
			out[slug] = []string{}
			continue
		}
		keys := make([]string, 0, len(entry))
		for k := range entry {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out[slug] = keys
	}
	return out
}

// TopologySHA256 is §8.4's Level-1 fingerprint. It detects a collection or
// global being added or removed, versions being enabled (readVersions appears)
// and access-control changes for this identity — for ~14 ms and 30 KB.
//
// It cannot detect a field being added to an existing collection, because
// `fields` collapses to the boolean true for a privileged key. That blind spot
// is stated honestly here and is what Level 3 exists for.
func TopologySHA256(p TopologyProjection) string {
	// encoding/json sorts map keys, so this is canonical by construction.
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// SchemaSHA256 is §8.4's Level-2 fingerprint: sha256 over the sorted field
// names of {__type(name:"Query"){fields{name}}}. It is byte-identical
// authenticated and unauthenticated, which makes it the only permission-free
// schema-version proxy the API offers.
func SchemaSHA256(queryFieldNames []string) string {
	if len(queryFieldNames) == 0 {
		return ""
	}
	names := append([]string(nil), queryFieldNames...)
	sort.Strings(names)
	b, err := json.Marshal(names)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
