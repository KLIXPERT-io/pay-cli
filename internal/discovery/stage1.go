package discovery

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
)

// stage1Query is §7.3's single request. It is issued once per run and its
// result is reused by Stage -1's candidate widening, by the mode
// classification and by the Level-2 schema fingerprint.
//
// The `acc` alias is literally named __type, which is what makes this query a
// valid §7.6 mode probe: Payload's introspection guard is a validation rule
// that fires only on a query AST containing __schema or __type.
const stage1Query = `{
  __typename
  q: __schema {
    queryType { fields { name args { name type { kind name ofType { kind name } } } } }
    mutationType { fields { name } }
  }
  acc: __type(name: "Access") { fields { name } }
}`

// schemaSnapshot is the decoded Stage-1 response.
type schemaSnapshot struct {
	QueryFields    []IntroField
	QueryByName    map[string]*IntroField
	QueryNames     []string
	MutationFields map[string]bool
	// AccessFields is Access.fields minus canAccessAdmin, in schema order.
	AccessFields []string
	// DocAccessSingulars are the Query.docAccess* fields with the prefix
	// stripped, in schema order.
	DocAccessSingulars []string
}

// decodeStage1 parses the Stage-1 response. A missing alias is not an error:
// §7.6's universal degrade trigger turns it into REST-only discovery.
func decodeStage1(res *gqlResult) (*schemaSnapshot, bool) {
	if res == nil || res.Data == nil {
		return nil, false
	}
	var q struct {
		QueryType struct {
			Fields []IntroField `json:"fields"`
		} `json:"queryType"`
		MutationType *struct {
			Fields []IntroField `json:"fields"`
		} `json:"mutationType"`
	}
	raw, ok := res.alias("q")
	if !ok || isJSONNull(raw) {
		return nil, false
	}
	if json.Unmarshal(raw, &q) != nil {
		return nil, false
	}

	snap := &schemaSnapshot{
		QueryFields:    q.QueryType.Fields,
		QueryByName:    make(map[string]*IntroField, len(q.QueryType.Fields)),
		QueryNames:     make([]string, 0, len(q.QueryType.Fields)),
		MutationFields: map[string]bool{},
	}
	for i := range snap.QueryFields {
		f := &snap.QueryFields[i]
		snap.QueryByName[f.Name] = f
		snap.QueryNames = append(snap.QueryNames, f.Name)
		if strings.HasPrefix(f.Name, "docAccess") {
			snap.DocAccessSingulars = append(snap.DocAccessSingulars, strings.TrimPrefix(f.Name, "docAccess"))
		}
	}
	if q.MutationType != nil {
		for _, f := range q.MutationType.Fields {
			snap.MutationFields[f.Name] = true
		}
	}

	if accRaw, ok := res.alias("acc"); ok && !isJSONNull(accRaw) {
		var acc IntroType
		if json.Unmarshal(accRaw, &acc) == nil {
			for _, f := range acc.Fields {
				if f.Name == "canAccessAdmin" {
					continue
				}
				snap.AccessFields = append(snap.AccessFields, f.Name)
			}
		}
	}
	return snap, len(snap.QueryFields) > 0
}

// FormatName reproduces Payload's formatName(): every character in
// -./+,()'[] and space becomes an underscore. It is what turns the slug
// `crm-contacts` into the Access field `crm_contacts`, and it is the only
// direction that is safe to compute — the inverse is ambiguous.
func FormatName(slug string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '-', '.', '/', '+', ',', '(', ')', '\'', '[', ']', ' ':
			return '_'
		}
		return r
	}, slug)
}

// entity is one row of the union inventory.
type entity struct {
	Slug string
	// Kind is "collection" or "global".
	Kind string

	Singular string
	Plural   string
	Count    string
	NameSrc  string

	IDTypeGraphQL string

	Versions  *bool
	Auth      *bool
	Duplicate *bool
	Localized *bool

	InAccess  bool
	InGraphQL bool
}

const (
	kindCollection = "collection"
	kindGlobal     = "global"
)

// zipInventory implements §7.3's index-for-index zip.
//
// acc.fields minus canAccessAdmin is formatName(slug) for every collection and
// then every global, in exactly the same order as Query.docAccess* (54 <-> 54
// verified live). Zipping them requires ZERO pluralisation guessing, which
// matters because media -> Media/allMedia, search -> Search/Searches, and a
// pluraliser mangles real slugs (cms -> Cm, physics -> Physic).
func zipInventory(snap *schemaSnapshot, knownSlugs []string) (map[string]*entity, bool) {
	if snap == nil {
		return nil, false
	}
	if len(snap.AccessFields) == 0 || len(snap.AccessFields) != len(snap.DocAccessSingulars) {
		// §7.3's guard: graphQL.disableQueries can desynchronise the two
		// lists. Abandon the zip rather than produce a silently shifted map.
		return fuzzyInventory(snap, knownSlugs), false
	}

	byMangled := make(map[string]string, len(knownSlugs))
	for _, s := range knownSlugs {
		byMangled[FormatName(s)] = s
	}

	out := make(map[string]*entity, len(snap.AccessFields))
	for i, mangled := range snap.AccessFields {
		slug, ok := byMangled[mangled]
		if !ok {
			// GraphQL sees entities /api/access does not (payload-kv is
			// verified GraphQL-only). The mangling is injective enough in
			// practice that turning underscores back into hyphens recovers
			// the slug, and the entity is flagged access-denied either way.
			slug = strings.ReplaceAll(mangled, "_", "-")
		}
		e := &entity{
			Slug:      slug,
			Singular:  snap.DocAccessSingulars[i],
			NameSrc:   SourceAccessZip,
			InGraphQL: true,
		}
		out[slug] = e
	}
	return out, true
}

// fuzzyInventory is §7.3's second and third fallbacks: per-slug verification
// against the schema, then normalised fuzzy matching. Anything that still does
// not resolve keeps graphql == nil and runs REST-only.
func fuzzyInventory(snap *schemaSnapshot, knownSlugs []string) map[string]*entity {
	out := make(map[string]*entity, len(knownSlugs))
	used := map[string]bool{}
	norm := func(s string) string {
		var b strings.Builder
		for _, r := range strings.ToLower(s) {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	singulars := append([]string(nil), snap.DocAccessSingulars...)
	sort.Slice(singulars, func(i, j int) bool { return len(singulars[i]) > len(singulars[j]) })

	for _, slug := range knownSlugs {
		ns := norm(slug)
		best := ""
		for _, sg := range singulars {
			if used[sg] {
				continue
			}
			n := norm(sg)
			if n == ns || strings.HasPrefix(ns, n) {
				best = sg
				break
			}
		}
		e := &entity{Slug: slug}
		if best != "" {
			used[best] = true
			e.Singular = best
			e.NameSrc = SourceFuzzy
			e.InGraphQL = true
		}
		out[slug] = e
	}
	return out
}

// classifyEntities fills in every §7.3 GraphQL-only fact for the zipped
// inventory. Nothing in here is ever defaulted: a signal that is not present
// leaves the corresponding tri-state nil.
func classifyEntities(snap *schemaSnapshot, entities map[string]*entity) {
	// plural lookup: the list Query field whose `where` argument is typed
	// {Singular}_where and which also takes a `limit`. This is how media
	// resolves to allMedia and search to Searches without guessing.
	pluralBySingular := map[string]string{}
	for i := range snap.QueryFields {
		f := &snap.QueryFields[i]
		whereArg, ok := f.Arg("where")
		if !ok || !f.HasArg("limit") {
			continue
		}
		name, _, _, _ := whereArg.Type.Named()
		if !strings.HasSuffix(name, "_where") {
			continue
		}
		singular := strings.TrimSuffix(name, "_where")
		if prev, exists := pluralBySingular[singular]; !exists || f.Name < prev {
			pluralBySingular[singular] = f.Name
		}
	}

	for _, e := range entities {
		if e.Singular == "" {
			continue
		}
		singularField := snap.QueryByName[e.Singular]
		plural := pluralBySingular[e.Singular]
		e.Plural = plural

		switch {
		case plural != "" && snap.QueryByName["count"+plural] != nil:
			e.Kind = kindCollection
			e.Count = "count" + plural
		case singularField != nil && !singularField.HasArg("id"):
			// A global's singular Query field takes no id
			// (Query.Header.args == [draft, select], verified).
			e.Kind = kindGlobal
		case plural != "":
			e.Kind = kindCollection
		default:
			e.Kind = kindGlobal
		}

		if singularField != nil {
			if t, ok := IDTypeFromArg(singularField); ok {
				e.IDTypeGraphQL = t
			}
		}

		// versions iff Query has version{S} AND versions{P}
		hasVersion := snap.QueryByName["version"+e.Singular] != nil
		hasVersions := plural != "" && snap.QueryByName["versions"+plural] != nil
		if e.Kind == kindGlobal {
			e.Versions = boolPtr(hasVersion)
		} else {
			e.Versions = boolPtr(hasVersion && hasVersions)
		}

		// auth iff Query has me{S} and initialized{S}
		e.Auth = boolPtr(snap.QueryByName["me"+e.Singular] != nil &&
			snap.QueryByName["initialized"+e.Singular] != nil)

		// duplicate iff Mutation has duplicate{S}. This is the ONLY duplicate
		// detection; probing /{coll}/{id}/duplicate created four real
		// documents on the live instance and is forbidden (§1 conflict 24).
		if e.Kind == kindCollection {
			e.Duplicate = boolPtr(snap.MutationFields["duplicate"+e.Singular])
		}

		// localization iff the plural Query field has a locale arg. The locale
		// CODES never come from here (§7.9): LocaleInputType exposes only
		// formatName()-mangled names.
		localeField := singularField
		if plural != "" {
			localeField = snap.QueryByName[plural]
		}
		if localeField != nil {
			e.Localized = boolPtr(localeField.HasArg("locale"))
		}
	}
}

// stage1 issues §7.3's request and returns the classified inventory.
func (d *Discoverer) stage1(ctx context.Context, knownSlugs []string) (*schemaSnapshot, map[string]*entity, modeOutcome) {
	res, err := d.gqlOnce(ctx)
	outcome := classifyGraphQL(res, err, []string{"q", "acc"})
	if outcome.Mode == GraphQLModeRouteMissing {
		outcome.Hint = RouteMissingHint(d.graphQLPath, d.apiPath, d.graphQLRoute, d.graphQLPathSource)
	}
	if outcome.Mode != GraphQLModeOK {
		return nil, nil, outcome
	}
	snap, ok := decodeStage1(res)
	if !ok {
		outcome.Mode = GraphQLModeErrors
		outcome.Introspection = boolPtr(false)
		outcome.Detail = "the Stage 1 introspection response could not be decoded"
		return nil, nil, outcome
	}
	entities, zipped := zipInventory(snap, knownSlugs)
	if !zipped {
		d.zipFallback = true
	}
	classifyEntities(snap, entities)
	return snap, entities, outcome
}

// gqlOnce issues the Stage-1 query at most once per process and memoises the
// raw result, so Stage -1's candidate widening and Stage 1 proper share it.
// The GraphQL schema is identity-independent (§8.4 Level 2 is byte-identical
// authenticated and unauthenticated), which is what makes the sharing sound
// even though Stage -1 runs before any credential is proven.
func (d *Discoverer) gqlOnce(ctx context.Context) (*gqlResult, error) {
	if d.stage1Done {
		return d.stage1Res, d.stage1Err
	}
	d.stage1Done = true
	// NoAuth: Stage -1 may not yet know which auth-collection slug to put in
	// the header, and a wrong slug is indistinguishable from no credential.
	d.stage1Res, d.stage1Err = d.postGraphQL(ctx, stage1Query, d.authCollection == "")
	d.graphQLBatches++
	return d.stage1Res, d.stage1Err
}
