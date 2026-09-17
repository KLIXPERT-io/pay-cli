package discovery

import (
	"reflect"
	"sort"
	"testing"
)

func TestFormatName(t *testing.T) {
	// Payload's formatName() maps every character in -./+,()'[] and space to
	// an underscore. Only this direction is safe to compute; the inverse is
	// ambiguous, which is why §7.3 recovers the slug by matching against the
	// /api/access slug set.
	tests := map[string]string{
		"crm-contacts":          "crm_contacts",
		"form-submissions":      "form_submissions",
		"payload-jobs-stats":    "payload_jobs_stats",
		"a.b/c+d,e(f)g'h[i]j k": "a_b_c_d_e_f_g_h_i_j_k",
		"pages":                 "pages",
	}
	for in, want := range tests {
		if got := FormatName(in); got != want {
			t.Errorf("FormatName(%q) = %q, want %q", in, got, want)
		}
	}
}

// liveSnapshot reproduces the shape verified against the live instance,
// including the three names a pluraliser gets wrong: media -> Media/allMedia,
// search -> Search/Searches, and a hyphenated slug.
func liveSnapshot() *schemaSnapshot {
	q := []IntroField{
		{Name: "Page", Args: []IntroArg{{Name: "id", Type: nonNull(scalar("Int"))}, {Name: "draft", Type: scalar("Boolean")}}},
		{Name: "Pages", Args: []IntroArg{{Name: "where", Type: &TypeRef{Kind: KindInputObject, Name: "Page_where"}}, {Name: "limit", Type: scalar("Int")}}},
		{Name: "countPages"},
		{Name: "versionPage"},
		{Name: "versionsPages", Args: []IntroArg{{Name: "where", Type: &TypeRef{Kind: KindInputObject, Name: "versionsPage_where"}}, {Name: "limit", Type: scalar("Int")}}},
		{Name: "docAccessPage"},

		{Name: "Media", Args: []IntroArg{{Name: "id", Type: nonNull(scalar("Int"))}}},
		{Name: "allMedia", Args: []IntroArg{{Name: "where", Type: &TypeRef{Kind: KindInputObject, Name: "Media_where"}}, {Name: "limit", Type: scalar("Int")}}},
		{Name: "countallMedia"},
		{Name: "docAccessMedia"},

		{Name: "Search", Args: []IntroArg{{Name: "id", Type: nonNull(scalar("Int"))}}},
		{Name: "Searches", Args: []IntroArg{{Name: "where", Type: &TypeRef{Kind: KindInputObject, Name: "Search_where"}}, {Name: "limit", Type: scalar("Int")}}},
		{Name: "countSearches"},
		{Name: "docAccessSearch"},

		{Name: "User", Args: []IntroArg{{Name: "id", Type: nonNull(scalar("String"))}}},
		{Name: "Users", Args: []IntroArg{{Name: "where", Type: &TypeRef{Kind: KindInputObject, Name: "User_where"}}, {Name: "limit", Type: scalar("Int")}}},
		{Name: "countUsers"},
		{Name: "meUser"},
		{Name: "initializedUser"},
		{Name: "docAccessUser"},

		// A global: its singular Query field takes no id.
		{Name: "Header", Args: []IntroArg{{Name: "draft", Type: scalar("Boolean")}, {Name: "select", Type: scalar("Boolean")}}},
		{Name: "docAccessHeader"},
	}
	snap := &schemaSnapshot{
		QueryByName:    map[string]*IntroField{},
		MutationFields: map[string]bool{"duplicatePage": true, "duplicateMedia": true},
		// Access.fields minus canAccessAdmin lists every collection then every
		// global, in the same order as Query.docAccess*.
		AccessFields:       []string{"pages", "media", "search", "users", "header"},
		DocAccessSingulars: []string{"Page", "Media", "Search", "User", "Header"},
	}
	snap.QueryFields = q
	for i := range snap.QueryFields {
		f := &snap.QueryFields[i]
		snap.QueryByName[f.Name] = f
		snap.QueryNames = append(snap.QueryNames, f.Name)
	}
	return snap
}

func TestZipInventoryNeedsNoPluralisationGuessing(t *testing.T) {
	snap := liveSnapshot()
	entities, zipped := zipInventory(snap, []string{"pages", "media", "search", "users", "header"})
	if !zipped {
		t.Fatal("the zip must succeed when the two lists are the same length")
	}
	classifyEntities(snap, entities)

	want := map[string]struct {
		singular, plural, count, kind string
	}{
		"pages":  {"Page", "Pages", "countPages", kindCollection},
		"media":  {"Media", "allMedia", "countallMedia", kindCollection},
		"search": {"Search", "Searches", "countSearches", kindCollection},
		"users":  {"User", "Users", "countUsers", kindCollection},
		"header": {"Header", "", "", kindGlobal},
	}
	for slug, w := range want {
		e, ok := entities[slug]
		if !ok {
			t.Fatalf("missing %q", slug)
		}
		if e.Singular != w.singular || e.Plural != w.plural || e.Count != w.count || e.Kind != w.kind {
			t.Errorf("%s = %q/%q/%q/%q want %q/%q/%q/%q",
				slug, e.Singular, e.Plural, e.Count, e.Kind, w.singular, w.plural, w.count, w.kind)
		}
		if e.NameSrc != SourceAccessZip {
			t.Errorf("%s source = %q", slug, e.NameSrc)
		}
	}
}

func TestClassifyEntitiesDerivations(t *testing.T) {
	snap := liveSnapshot()
	entities, _ := zipInventory(snap, []string{"pages", "media", "search", "users", "header"})
	classifyEntities(snap, entities)

	tests := []struct {
		slug      string
		idType    string
		versions  *bool
		auth      *bool
		duplicate *bool
	}{
		// versions iff Query has version{S} AND versions{P}
		{"pages", IDTypeNumber, boolPtr(true), boolPtr(false), boolPtr(true)},
		{"media", IDTypeNumber, boolPtr(false), boolPtr(false), boolPtr(true)},
		// auth iff Query has me{S} AND initialized{S}
		{"users", IDTypeString, boolPtr(false), boolPtr(true), boolPtr(false)},
	}
	for _, tt := range tests {
		e := entities[tt.slug]
		if e.IDTypeGraphQL != tt.idType {
			t.Errorf("%s id_type = %q, want %q", tt.slug, e.IDTypeGraphQL, tt.idType)
		}
		if !eqBool(e.Versions, tt.versions) {
			t.Errorf("%s versions = %v, want %v", tt.slug, show(e.Versions), show(tt.versions))
		}
		if !eqBool(e.Auth, tt.auth) {
			t.Errorf("%s auth = %v, want %v", tt.slug, show(e.Auth), show(tt.auth))
		}
		if !eqBool(e.Duplicate, tt.duplicate) {
			t.Errorf("%s duplicate = %v, want %v", tt.slug, show(e.Duplicate), show(tt.duplicate))
		}
	}
	// A global's id_type is not a thing, and its singular Query field has no
	// id argument.
	if entities["header"].IDTypeGraphQL != "" {
		t.Errorf("a global must not acquire an id_type: %q", entities["header"].IDTypeGraphQL)
	}
	// localization: no plural Query field takes a locale arg here.
	if e := entities["pages"]; e.Localized == nil || *e.Localized {
		t.Errorf("localized = %v, want false", show(e.Localized))
	}
}

func TestZipGuardFallsBackWhenListsDesynchronise(t *testing.T) {
	// graphQL: { disableQueries: true } can make len(acc) != len(docAccess).
	// Zipping anyway would silently shift every slug by one.
	snap := liveSnapshot()
	snap.DocAccessSingulars = snap.DocAccessSingulars[:3]
	entities, zipped := zipInventory(snap, []string{"pages", "media", "search", "users", "header"})
	if zipped {
		t.Fatal("the guard must abandon the zip")
	}
	// The fuzzy fallback still recovers the obvious ones and leaves the rest
	// with no GraphQL name, which makes them REST-only rather than wrong.
	if e := entities["pages"]; e.Singular != "Page" || e.NameSrc != SourceFuzzy {
		t.Errorf("pages = %+v", e)
	}
	for slug, e := range entities {
		if e.Singular != "" && e.NameSrc != SourceFuzzy {
			t.Errorf("%s resolved with source %q in the fallback", slug, e.NameSrc)
		}
	}
}

func TestZipRecoversGraphQLOnlySlugs(t *testing.T) {
	// payload-kv is verified GraphQL-only: /api/access omits it because this
	// identity has zero permissions on it.
	snap := &schemaSnapshot{
		QueryByName:        map[string]*IntroField{},
		MutationFields:     map[string]bool{},
		AccessFields:       []string{"pages", "payload_kv"},
		DocAccessSingulars: []string{"Page", "PayloadKv"},
	}
	entities, zipped := zipInventory(snap, []string{"pages"})
	if !zipped {
		t.Fatal("the zip must still succeed")
	}
	if _, ok := entities["payload-kv"]; !ok {
		keys := make([]string, 0, len(entities))
		for k := range entities {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Fatalf("payload-kv was not recovered: %v", keys)
	}
}

func TestAuthSlugsFromSchema(t *testing.T) {
	snap := liveSnapshot()
	got := authSlugsFromSchema(snap)
	if !reflect.DeepEqual(got, []string{"users"}) {
		t.Fatalf("auth slugs = %v, want [users]", got)
	}
}

func eqBool(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func show(b *bool) any {
	if b == nil {
		return "null"
	}
	return *b
}
