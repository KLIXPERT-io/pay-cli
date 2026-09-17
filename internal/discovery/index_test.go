package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
)

// ---------------------------------------------------------------------------
// An in-memory Payload, wired in as an http.RoundTripper.
//
// No socket is opened: the tests must pass offline and must not depend on a
// loopback listener being available in a sandbox.
// ---------------------------------------------------------------------------

const testAPIKey = "test-key-0123456789"

type fakeAPI struct {
	mu  sync.Mutex
	log []string

	apiPath string
	// graphQLMode is "ok", "404empty", "html404", "introspection", "errors"
	// or "5xx".
	graphQLMode string

	authCollection string

	endpointsDisabled map[string]bool
	uploadCollections map[string]bool
	versioned         map[string]bool
	drafted           map[string]bool
	trashed           map[string]bool
	foldered          map[string]bool
	authCollections   map[string]bool
	docs              map[string][]map[string]any
	// labelFor is the plural the bulk-delete message reports; "" means the
	// project answered something the English pattern does not match.
	labelFor map[string]string

	accessCollections map[string]any
	accessGlobals     map[string]any
	// graphQLEntities are the slugs the GraphQL Access type lists, which is
	// deliberately allowed to differ from accessCollections.
	graphQLCollections []string
	graphQLGlobals     []string
	types              map[string]*IntroType
}

func newFakeAPI() *fakeAPI {
	f := &fakeAPI{
		apiPath:           "/api",
		graphQLMode:       "ok",
		authCollection:    "users",
		endpointsDisabled: map[string]bool{"payload-migrations": true},
		uploadCollections: map[string]bool{"media": true},
		versioned:         map[string]bool{"pages": true},
		drafted:           map[string]bool{"pages": true},
		trashed:           map[string]bool{"posts": true},
		foldered:          map[string]bool{"media": true},
		authCollections:   map[string]bool{"users": true},
		labelFor: map[string]string{
			"pages": "Pages", "posts": "Posts", "media": "Media",
			"users": "Users", "payload-migrations": "",
		},
		docs: map[string][]map[string]any{
			"pages": {{"id": json.Number("16"), "title": "Home", "slug": "home"}},
			"posts": {{"id": json.Number("3"), "title": "Hello"}},
			"users": {{"id": "66f1a2b3c4d5e6f708192a3b", "email": "a@b.c"}},
		},
	}
	entry := func(readVersions bool) map[string]any {
		m := map[string]any{"fields": true, "create": true, "read": true, "update": true, "delete": true}
		if readVersions {
			m["readVersions"] = true
		}
		return m
	}
	f.accessCollections = map[string]any{
		"pages":              entry(true),
		"posts":              entry(false),
		"media":              entry(false),
		"users":              entry(false),
		"payload-migrations": entry(false),
	}
	f.accessGlobals = map[string]any{"header": map[string]any{"read": true, "update": true}}
	// payload-kv is GraphQL-only and payload-migrations is REST-only, exactly
	// as verified live.
	f.graphQLCollections = []string{"pages", "posts", "media", "users", "payload-kv"}
	f.graphQLGlobals = []string{"header"}
	f.types = fakeTypes()
	return f
}

func fakeTypes() map[string]*IntroType {
	t := map[string]*IntroType{}
	t["Page"] = &IntroType{Kind: KindObject, Name: "Page", Fields: []IntroField{
		{Name: "id", Type: nonNull(scalar("Int"))},
		{Name: "title", Type: scalar("String")},
		{Name: "_status", Type: enumRef("Page__status")},
		{Name: "hero", Type: object("Page_Hero")},
	}}
	t["mutationPageInput"] = &IntroType{Kind: KindInputObject, InputFields: []IntroInputField{
		{Name: "title", Type: nonNull(scalar("String"))},
		{Name: "hero", Type: &TypeRef{Kind: KindInputObject, Name: "mutationPage_HeroInput"}},
	}}
	t["mutationPage_HeroInput"] = &IntroType{Kind: KindInputObject, InputFields: []IntroInputField{
		{Name: "media", Type: scalar("Int")},
	}}
	t["Page_Hero"] = &IntroType{Kind: KindObject, Name: "Page_Hero", Fields: []IntroField{
		{Name: "media", Type: object("Media")},
	}}
	t["Page__status"] = &IntroType{Kind: KindEnum, EnumValues: []IntroEnumValue{{Name: "draft"}, {Name: "published"}}}
	t["Page_where"] = &IntroType{Kind: KindInputObject, InputFields: []IntroInputField{
		{Name: "title", Type: &TypeRef{Kind: KindInputObject, Name: "Page_title_operator"}},
	}}
	t["Post"] = &IntroType{Kind: KindObject, Name: "Post", Fields: []IntroField{
		{Name: "id", Type: nonNull(scalar("Int"))},
		{Name: "title", Type: scalar("String")},
		{Name: "deletedAt", Type: scalar("DateTime")},
	}}
	t["mutationPostInput"] = &IntroType{Kind: KindInputObject, InputFields: []IntroInputField{
		{Name: "title", Type: nonNull(scalar("String"))},
	}}
	t["Media"] = &IntroType{Kind: KindObject, Name: "Media", Fields: []IntroField{
		{Name: "id", Type: nonNull(scalar("Int"))},
		{Name: "filename", Type: scalar("String")},
		{Name: "mimeType", Type: scalar("String")},
		{Name: "filesize", Type: scalar("Float")},
		{Name: "url", Type: scalar("String")},
		{Name: "folder", Type: scalar("Int")},
	}}
	t["mutationMediaInput"] = &IntroType{Kind: KindInputObject, InputFields: []IntroInputField{
		{Name: "alt", Type: scalar("String")},
	}}
	t["User"] = &IntroType{Kind: KindObject, Name: "User", Fields: []IntroField{
		{Name: "id", Type: nonNull(scalar("String"))},
		{Name: "email", Type: scalar("EmailAddress")},
	}}
	t["mutationUserInput"] = &IntroType{Kind: KindInputObject, InputFields: []IntroInputField{
		{Name: "email", Type: nonNull(scalar("EmailAddress"))},
		{Name: "apiKey", Type: scalar("String")},
		{Name: "enableAPIKey", Type: scalar("Boolean")},
	}}
	t["PayloadKv"] = &IntroType{Kind: KindObject, Name: "PayloadKv", Fields: []IntroField{
		{Name: "id", Type: nonNull(scalar("Int"))},
	}}
	t["Header"] = &IntroType{Kind: KindObject, Name: "Header", Fields: []IntroField{
		{Name: "navItems", Type: scalar("JSON")},
	}}
	t["mutationHeaderInput"] = &IntroType{Kind: KindInputObject, InputFields: []IntroInputField{
		{Name: "navItems", Type: scalar("JSON")},
	}}
	return t
}

var singularOf = map[string]string{
	"pages": "Page", "posts": "Post", "media": "Media", "users": "User",
	"payload-kv": "PayloadKv", "header": "Header",
}

var pluralOf = map[string]string{
	"pages": "Pages", "posts": "Posts", "media": "allMedia", "users": "Users",
	"payload-kv": "PayloadKvs",
}

func (f *fakeAPI) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry := r.Method + " " + r.URL.Path
	if r.URL.RawQuery != "" {
		entry += "?" + r.URL.RawQuery
	}
	f.log = append(f.log, entry)
}

func (f *fakeAPI) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func (f *fakeAPI) count(substr string) int {
	n := 0
	for _, r := range f.requests() {
		if strings.Contains(r, substr) {
			n++
		}
	}
	return n
}

func jsonResponse(status int, body any) *http.Response {
	b, _ := json.Marshal(body)
	return rawResponse(status, "application/json", b)
}

func rawResponse(status int, contentType string, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}, "X-Powered-By": []string{"Next.js, Payload"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

func routeNotFound(path string) *http.Response {
	return jsonResponse(http.StatusNotFound, map[string]any{"message": `Route not found "` + path + `"`})
}

func cannot(method, path string) *http.Response {
	return jsonResponse(http.StatusNotImplemented, map[string]any{"message": "Cannot " + method + " " + path})
}

func (f *fakeAPI) RoundTrip(r *http.Request) (*http.Response, error) {
	f.record(r)
	path := r.URL.Path
	if !strings.HasPrefix(path, f.apiPath+"/") {
		return routeNotFound(path), nil
	}
	rest := strings.TrimPrefix(path, f.apiPath+"/")
	seg := strings.Split(rest, "/")

	if rest == "graphql" {
		return f.graphQL(r)
	}
	if rest == "access" {
		return jsonResponse(http.StatusOK, map[string]any{
			"canAccessAdmin": f.authorized(r),
			"collections":    f.visibleCollections(r),
			"globals":        f.accessGlobals,
		}), nil
	}
	if rest == "og" {
		return rawResponse(http.StatusOK, "image/png", []byte("\x89PNG")), nil
	}
	if rest == "reorder" {
		return routeNotFound(path), nil
	}

	slug := seg[0]
	if f.endpointsDisabled[slug] {
		return cannot(r.Method, path), nil
	}
	if _, known := f.accessCollections[slug]; !known {
		if slug != "payload-kv" {
			return routeNotFound(path), nil
		}
		return jsonResponse(http.StatusForbidden, map[string]any{
			"errors": []any{map[string]any{"message": "You are not allowed to perform this action"}},
		}), nil
	}

	switch {
	case len(seg) == 2 && seg[1] == "me":
		if slug == f.authCollection && f.authorized(r) && f.slugInHeader(r, slug) {
			return jsonResponse(http.StatusOK, map[string]any{
				"user": map[string]any{"id": 66, "_strategy": "api-key", "canAccessAdmin": true},
			}), nil
		}
		// A wrong key AND a wrong auth-collection slug both answer 200 with
		// user:null, which is why the status is never the signal.
		return jsonResponse(http.StatusOK, map[string]any{"user": nil}), nil

	case len(seg) == 2 && seg[1] == "init":
		if f.authCollections[slug] {
			return jsonResponse(http.StatusOK, map[string]any{"initialized": true}), nil
		}
		return jsonResponse(http.StatusForbidden, map[string]any{
			"errors": []any{map[string]any{"message": "You are not allowed to perform this action"}},
		}), nil

	case len(seg) == 3 && seg[1] == "file":
		if f.uploadCollections[slug] {
			return jsonResponse(http.StatusInternalServerError, map[string]any{
				"errors": []any{map[string]any{"message": "Something went wrong."}},
			}), nil
		}
		return routeNotFound(path), nil

	case len(seg) == 2 && seg[1] == "versions":
		if f.versioned[slug] {
			return jsonResponse(http.StatusOK, map[string]any{
				"docs": []any{map[string]any{"id": 20, "parent": 16}}, "totalDocs": 1,
			}), nil
		}
		// payload-preferences registers a custom GET /:key that shadows
		// /versions and answers 200 with no docs array.
		return jsonResponse(http.StatusOK, map[string]any{"message": "Not Found", "value": nil}), nil

	case len(seg) == 2 && seg[1] == "count":
		q := r.URL.Query()
		switch {
		case q.Get("where[_status][equals]") != "":
			if !f.drafted[slug] {
				return queryError("_status"), nil
			}
		case q.Get("where[deletedAt][exists]") != "":
			if !f.trashed[slug] {
				return queryError("deletedAt"), nil
			}
		case q.Get("where[folder][exists]") != "":
			if !f.foldered[slug] {
				return queryError("folder"), nil
			}
		}
		return jsonResponse(http.StatusOK, map[string]any{"totalDocs": 0}), nil

	case len(seg) == 1 && r.Method == http.MethodDelete:
		plural, ok := f.labelFor[slug]
		if !ok || plural == "" {
			// A translated or custom message: the parse must fail soft.
			return jsonResponse(http.StatusOK, map[string]any{"docs": []any{}, "message": "0 Seiten gelöscht."}), nil
		}
		return jsonResponse(http.StatusOK, map[string]any{
			"docs": []any{}, "errors": []any{},
			"message": "Deleted 0 " + plural + " successfully.",
		}), nil

	case len(seg) == 1 && r.Method == http.MethodGet:
		docs := f.docs[slug]
		out := make([]any, 0, len(docs))
		for _, d := range docs {
			out = append(out, d)
		}
		return jsonResponse(http.StatusOK, map[string]any{
			"docs": out, "totalDocs": len(out), "limit": 1, "page": 1, "totalPages": 1,
			"hasNextPage": false, "hasPrevPage": false,
		}), nil
	}
	return routeNotFound(path), nil
}

func queryError(field string) *http.Response {
	return jsonResponse(http.StatusBadRequest, map[string]any{
		"errors": []any{map[string]any{
			"name":    "QueryError",
			"data":    []any{map[string]any{"path": field}},
			"message": "The following path cannot be queried: " + field,
		}},
	})
}

func (f *fakeAPI) authorized(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Authorization"), testAPIKey)
}

// slugInHeader reproduces Payload's api-key strategy: the header is
// `{collection} API-Key {key}` and a wrong slug silently yields the anonymous
// view rather than an error.
func (f *fakeAPI) slugInHeader(r *http.Request, slug string) bool {
	return strings.HasPrefix(r.Header.Get("Authorization"), slug+" API-Key ")
}

func (f *fakeAPI) visibleCollections(r *http.Request) map[string]any {
	if f.authorized(r) {
		return f.accessCollections
	}
	// Unauthenticated /api/access answers 200 with a reduced set (verified).
	return map[string]any{"pages": map[string]any{"read": true}, "users": map[string]any{"read": true}}
}

var aliasRe = regexp.MustCompile(`t(\d+): __type\(name: "((?:[^"\\]|\\.)*)"\)`)

func (f *fakeAPI) graphQL(r *http.Request) (*http.Response, error) {
	switch f.graphQLMode {
	case "404empty":
		return rawResponse(http.StatusNotFound, "application/json", nil), nil
	case "html404":
		return rawResponse(http.StatusNotFound, "text/html; charset=utf-8", []byte("<!DOCTYPE html>")), nil
	case "5xx":
		return jsonResponse(http.StatusBadGateway, map[string]any{}), nil
	}
	body, _ := io.ReadAll(r.Body)
	var req payload.GraphQLRequest
	_ = json.Unmarshal(body, &req)

	if f.graphQLMode == "introspection" {
		return jsonResponse(http.StatusOK, map[string]any{
			"errors": []any{map[string]any{"message": "GraphQL introspection is not allowed"}},
		}), nil
	}
	if f.graphQLMode == "errors" {
		return jsonResponse(http.StatusOK, map[string]any{
			"errors": []any{map[string]any{"message": "You are not allowed to perform this action"}},
		}), nil
	}

	if strings.Contains(req.Query, "__schema") {
		return jsonResponse(http.StatusOK, map[string]any{"data": f.stage1Data()}), nil
	}
	data := map[string]any{}
	for _, m := range aliasRe.FindAllStringSubmatch(req.Query, -1) {
		name := strings.ReplaceAll(m[2], `\"`, `"`)
		if t, ok := f.types[name]; ok {
			data["t"+m[1]] = t
		} else {
			data["t"+m[1]] = nil
		}
	}
	return jsonResponse(http.StatusOK, map[string]any{"data": data}), nil
}

func (f *fakeAPI) stage1Data() map[string]any {
	var queryFields []IntroField
	var mutationFields []IntroField
	var accessFields []map[string]string
	accessFields = append(accessFields, map[string]string{"name": "canAccessAdmin"})

	add := func(slug string, global bool) {
		singular := singularOf[slug]
		accessFields = append(accessFields, map[string]string{"name": FormatName(slug)})
		if global {
			queryFields = append(queryFields, IntroField{Name: singular, Args: []IntroArg{
				{Name: "draft", Type: scalar("Boolean")}, {Name: "select", Type: scalar("Boolean")},
			}})
			queryFields = append(queryFields, IntroField{Name: "docAccess" + singular})
			return
		}
		idScalar := "Int"
		if slug == "users" {
			idScalar = "String"
		}
		plural := pluralOf[slug]
		queryFields = append(queryFields,
			IntroField{Name: singular, Args: []IntroArg{
				{Name: "id", Type: nonNull(scalar(idScalar))},
				{Name: "draft", Type: scalar("Boolean")},
			}},
			IntroField{Name: plural, Args: []IntroArg{
				{Name: "where", Type: &TypeRef{Kind: KindInputObject, Name: singular + "_where"}},
				{Name: "limit", Type: scalar("Int")},
			}},
			IntroField{Name: "count" + plural},
			IntroField{Name: "docAccess" + singular},
		)
		if f.versioned[slug] {
			queryFields = append(queryFields,
				IntroField{Name: "version" + singular},
				IntroField{Name: "versions" + plural, Args: []IntroArg{
					{Name: "where", Type: &TypeRef{Kind: KindInputObject, Name: "versions" + singular + "_where"}},
					{Name: "limit", Type: scalar("Int")},
				}})
		}
		if f.authCollections[slug] {
			queryFields = append(queryFields,
				IntroField{Name: "me" + singular},
				IntroField{Name: "initialized" + singular})
		}
		mutationFields = append(mutationFields, IntroField{Name: "duplicate" + singular})
	}
	// Access lists every collection then every global, in the same order as
	// Query.docAccess*.
	for _, slug := range f.graphQLCollections {
		add(slug, false)
	}
	for _, slug := range f.graphQLGlobals {
		add(slug, true)
	}
	return map[string]any{
		"__typename": "Query",
		"q": map[string]any{
			"queryType":    map[string]any{"fields": queryFields},
			"mutationType": map[string]any{"fields": mutationFields},
		},
		"acc": map[string]any{"fields": accessFields},
	}
}

// ---------------------------------------------------------------------------

func fixedClock() func() time.Time {
	n := time.Date(2026, 9, 16, 17, 0, 0, 0, time.UTC)
	return func() time.Time { return n }
}

func newTestDiscoverer(t *testing.T, f *fakeAPI, mutate func(*Options)) *Discoverer {
	t.Helper()
	cli, err := payload.New(payload.Config{
		BaseURL:        "http://payload.test",
		APIPath:        f.apiPath,
		AuthMode:       payload.AuthModeAPIKey,
		AuthCollection: "auto",
		Credential:     testAPIKey,
		Concurrency:    4,
		RoundTripper:   f,
		Now:            fixedClock(),
		Sleep:          func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	opt := Options{
		Client:               cli,
		BaseURL:              "http://payload.test",
		APIPath:              f.apiPath,
		APIPathSource:        SourceConfigured,
		AuthMode:             payload.AuthModeAPIKey,
		AuthCollection:       "users",
		AuthCollectionSource: SourceConfigured,
		KeyFingerprint:       "a1b2c3d4e5f60718",
		Concurrency:          4,
		CLIVersion:           "0.1.0",
		Generation:           "01K5Q7TZ4V3B8CJK2M9N0P1Q2R",
		Now:                  fixedClock(),
		Profile:              "local",
		Profiles:             []string{"local"},
		ScopeKey:             "0df77681bf2d6bfc",
	}
	if mutate != nil {
		mutate(&opt)
	}
	d, err := New(opt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

func TestRunHappyPath(t *testing.T) {
	f := newFakeAPI()
	d := newTestDiscoverer(t, f, nil)
	res, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	m := res.Manifest

	// §7.3's union: /api/access sees payload-migrations and not payload-kv;
	// GraphQL sees payload-kv and not payload-migrations. Each source alone
	// loses a collection.
	wantSlugs := []string{"media", "pages", "payload-kv", "payload-migrations", "posts", "users"}
	if got := m.CollectionSlugs(); !equalStrings(got, wantSlugs) {
		t.Errorf("collections = %v, want %v", got, wantSlugs)
	}
	if got := m.GlobalSlugs(); !equalStrings(got, []string{"header"}) {
		t.Errorf("globals = %v", got)
	}
	reach := map[string]string{
		"pages": ReachabilityOK, "payload-kv": ReachabilityAccessDenied,
		"payload-migrations": ReachabilityGraphQLDisabled,
	}
	for slug, want := range reach {
		c, _ := m.Collection(slug)
		if c.Reachability != want {
			t.Errorf("%s reachability = %q, want %q", slug, c.Reachability, want)
		}
	}
	if m.Capabilities.GraphQL.Mode != GraphQLModeOK {
		t.Errorf("graphql mode = %q", m.Capabilities.GraphQL.Mode)
	}
	if !m.Identity.Verified || m.Identity.UserID == nil {
		t.Errorf("identity = %+v", m.Identity)
	}
	if m.Fingerprint.TopologySHA256 == "" || m.Fingerprint.SchemaSHA256 == "" {
		t.Error("both invalidation fingerprints must be recorded")
	}
	if m.Fingerprint.ServerIdentity != "Next.js, Payload" {
		t.Errorf("server identity = %q", m.Fingerprint.ServerIdentity)
	}

	checks := []struct {
		slug   string
		flag   func(Flags) *bool
		name   string
		want   bool
		source func(FlagsSource) string
	}{
		{"media", func(f Flags) *bool { return f.Upload }, "upload", true, func(s FlagsSource) string { return s.Upload }},
		{"pages", func(f Flags) *bool { return f.Upload }, "upload", false, func(s FlagsSource) string { return s.Upload }},
		{"pages", func(f Flags) *bool { return f.Versions }, "versions", true, func(s FlagsSource) string { return s.Versions }},
		{"pages", func(f Flags) *bool { return f.Drafts }, "drafts", true, func(s FlagsSource) string { return s.Drafts }},
		{"posts", func(f Flags) *bool { return f.Trash }, "trash", true, func(s FlagsSource) string { return s.Trash }},
		{"media", func(f Flags) *bool { return f.Folders }, "folders", true, func(s FlagsSource) string { return s.Folders }},
		{"users", func(f Flags) *bool { return f.Auth }, "auth", true, func(s FlagsSource) string { return s.Auth }},
		{"users", func(f Flags) *bool { return f.UseAPIKey }, "use_api_key", true, func(s FlagsSource) string { return s.UseAPIKey }},
		{"pages", func(f Flags) *bool { return f.Duplicate }, "duplicate", true, func(s FlagsSource) string { return s.Duplicate }},
	}
	for _, c := range checks {
		coll, ok := m.Collection(c.slug)
		if !ok {
			t.Fatalf("no collection %q", c.slug)
		}
		got := c.flag(coll.Flags)
		if got == nil || *got != c.want {
			t.Errorf("%s.%s = %v, want %v", c.slug, c.name, show(got), c.want)
		}
		if src := c.source(coll.FlagsSource); src == SourceUnknown || src == "" {
			t.Errorf("%s.%s has no provenance", c.slug, c.name)
		}
	}

	pages, _ := m.Collection("pages")
	if pages.IDType != IDTypeNumber || pages.IDTypeSource != SourceGraphQL {
		t.Errorf("pages id_type = %q/%q", pages.IDType, pages.IDTypeSource)
	}
	users, _ := m.Collection("users")
	if users.IDType != IDTypeString {
		t.Errorf("users id_type = %q", users.IDType)
	}
	if !users.Internal == false {
		t.Error("users must not be internal")
	}
	if mig, _ := m.Collection("payload-migrations"); !mig.Internal {
		t.Error("payload-* collections must be flagged internal")
	}
	if mig, _ := m.Collection("payload-migrations"); mig.Flags.EndpointsDisabled == nil || !*mig.Flags.EndpointsDisabled {
		t.Error("the 501 guard must flag payload-migrations as endpoints-disabled")
	}
	// Without the 501 guard running first, payload-migrations reads as an
	// upload positive: its /file/ probe never answers `Route not found`.
	if mig, _ := m.Collection("payload-migrations"); mig.Flags.Upload != nil && *mig.Flags.Upload {
		t.Error("payload-migrations was misclassified as an upload collection")
	}

	shard := res.Shards["fields/pages.json"]
	if shard == nil {
		t.Fatal("no shard for pages")
	}
	if shard.SHA256 != pages.FieldsSHA256 || pages.FieldsShard != "fields/pages.json" {
		t.Errorf("the index and the shard disagree: %q vs %q", pages.FieldsSHA256, shard.SHA256)
	}
	title, ok := shard.Field("title")
	if !ok {
		t.Fatal("no title field")
	}
	if title.Required == nil || !*title.Required || title.RequiredSource != SourceGraphQLInput {
		t.Errorf("title required = %v/%q", show(title.Required), title.RequiredSource)
	}
	if hero, ok := shard.Field("hero.media"); !ok || hero.PayloadType != TypeUpload {
		t.Errorf("hero.media = %+v", hero)
	}
	if _, ok := res.Shards["fields/_global_header.json"]; !ok {
		t.Error("globals need a shard too")
	}
}

func TestRunIsReadOnlyByDefault(t *testing.T) {
	f := newFakeAPI()
	d := newTestDiscoverer(t, f, nil)
	if _, err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, r := range f.requests() {
		switch {
		case strings.HasPrefix(r, "POST "):
			// The only POSTs discovery may issue are the GraphQL query and the
			// bodyless /reorder existence probe.
			if !strings.Contains(r, "/graphql") && !strings.Contains(r, "/reorder") {
				t.Errorf("unexpected POST: %s", r)
			}
		case strings.HasPrefix(r, "PUT "), strings.HasPrefix(r, "PATCH "):
			t.Errorf("discovery issued a write: %s", r)
		case strings.HasPrefix(r, "DELETE "):
			// §7.5 probe 8 matches nothing by construction.
			if !strings.Contains(r, "where%5Bid%5D%5Bequals%5D=-1") {
				t.Errorf("unscoped DELETE: %s", r)
			}
		}
	}
	// §7.7: nothing is created unless the caller opted in.
	if d.opt.AllowWriteProbes {
		t.Fatal("write probes must be off by default")
	}
	if len(f.requests()) == 0 {
		t.Fatal("no requests were issued at all")
	}
}

func TestRunRespectsTheColdBudgetWhenGraphQLWorks(t *testing.T) {
	f := newFakeAPI()
	d := newTestDiscoverer(t, f, nil)
	if _, err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// A fact GraphQL already established is never re-probed, so only the
	// entity GraphQL could not describe is probed at all.
	for _, slug := range []string{"pages", "posts", "media", "users"} {
		if n := f.count("/" + slug + "/count?"); n != 0 {
			t.Errorf("%s was probed %d times even though GraphQL described it", slug, n)
		}
	}
	if n := f.count("DELETE /api/pages"); n != 0 {
		t.Errorf("the label DELETE ran for a collection GraphQL described (%d times)", n)
	}
	if n := f.count("/api/payload-migrations"); n == 0 {
		t.Error("the entity GraphQL could not describe must still be probed")
	}
}

func TestRunDegradesToRESTOnly(t *testing.T) {
	modes := []struct {
		mode     string
		wantMode string
	}{
		{"404empty", GraphQLModeDisabled},
		{"html404", GraphQLModeRouteMissing},
		{"introspection", GraphQLModeIntrospectionDisabled},
		{"errors", GraphQLModeErrors},
		{"5xx", GraphQLModeUnreachable},
	}
	for _, tt := range modes {
		t.Run(tt.mode, func(t *testing.T) {
			f := newFakeAPI()
			f.graphQLMode = tt.mode
			d := newTestDiscoverer(t, f, nil)
			res, err := d.Run(context.Background())
			if err != nil {
				t.Fatalf("a GraphQL-layer refusal must never make a working project unusable: %v", err)
			}
			m := res.Manifest
			if m.Capabilities.GraphQL.Mode != tt.wantMode {
				t.Errorf("mode = %q, want %q", m.Capabilities.GraphQL.Mode, tt.wantMode)
			}
			// REST-only still recovers the full /api/access inventory and
			// every §7.5 capability.
			if got := m.CollectionSlugs(); !equalStrings(got,
				[]string{"media", "pages", "payload-migrations", "posts", "users"}) {
				t.Errorf("collections = %v", got)
			}
			media, _ := m.Collection("media")
			if media.Flags.Upload == nil || !*media.Flags.Upload {
				t.Errorf("upload not recovered by probe: %v", show(media.Flags.Upload))
			}
			if media.FlagsSource.Upload != SourceProbe {
				t.Errorf("upload source = %q", media.FlagsSource.Upload)
			}
			pages, _ := m.Collection("pages")
			if pages.Flags.Versions == nil || !*pages.Flags.Versions {
				t.Errorf("versions not recovered: %v", show(pages.Flags.Versions))
			}
			if pages.Flags.Drafts == nil || !*pages.Flags.Drafts {
				t.Errorf("drafts not recovered: %v", show(pages.Flags.Drafts))
			}
			posts, _ := m.Collection("posts")
			if posts.Flags.Trash == nil || !*posts.Flags.Trash {
				t.Errorf("trash not recovered: %v", show(posts.Flags.Trash))
			}
			users, _ := m.Collection("users")
			if users.Flags.Auth == nil || !*users.Flags.Auth {
				t.Errorf("auth not recovered: %v", show(users.Flags.Auth))
			}
			// Labels come from probe 8's message, with a fail-soft parse.
			if pages.Labels.Plural != "Pages" || pages.Labels.Source != SourceBulkDeleteMessage {
				t.Errorf("pages labels = %+v", pages.Labels)
			}
			// id_type falls back to the observed sample, never to a default.
			if pages.IDType != IDTypeNumber || pages.IDTypeSource != SourceObserved {
				t.Errorf("pages id_type = %q/%q", pages.IDType, pages.IDTypeSource)
			}
			if users.IDType != IDTypeString {
				t.Errorf("users id_type = %q", users.IDType)
			}
			// A GraphQL-only fact with no REST probe stays null: duplicate
			// must never become false, or `pay duplicate` would be blocked on
			// a project where it works.
			if pages.Flags.Duplicate != nil {
				t.Errorf("duplicate = %v; it must stay null without GraphQL", *pages.Flags.Duplicate)
			}
			if !hasLimitation(m, LimCapabilityUnknown) {
				t.Error("a null capability flag must be declared, not hidden")
			}
		})
	}
}

func TestRunReportsUnknownIDTypeRatherThanGuessing(t *testing.T) {
	f := newFakeAPI()
	f.graphQLMode = "404empty"
	// An empty collection with no versions row: exactly the 10-of-49 live case.
	f.docs = map[string][]map[string]any{}
	f.versioned = map[string]bool{}
	d := newTestDiscoverer(t, f, nil)
	res, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	pages, _ := res.Manifest.Collection("pages")
	if pages.IDType != IDTypeUnknown || pages.IDTypeSource != SourceUnknown {
		t.Fatalf("id_type = %q/%q; a defaulted number would reject every valid ObjectId with a false invalid_id",
			pages.IDType, pages.IDTypeSource)
	}
	if !hasLimitation(res.Manifest, LimIDTypeUnknown) {
		t.Error("ID_TYPE_UNKNOWN must be declared")
	}
	if !hasLimitation(res.Manifest, LimFieldsUnavailable) {
		t.Error("FIELDS_UNAVAILABLE must be declared for a collection with no documents and no GraphQL")
	}
}

func TestRunHonoursTheProfileIDTypePin(t *testing.T) {
	f := newFakeAPI()
	f.graphQLMode = "404empty"
	f.docs = map[string][]map[string]any{}
	f.versioned = map[string]bool{}
	d := newTestDiscoverer(t, f, func(o *Options) { o.ConfiguredIDType = IDTypeString })
	res, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	pages, _ := res.Manifest.Collection("pages")
	if pages.IDType != IDTypeString || pages.IDTypeSource != SourceConfigured {
		t.Fatalf("id_type = %q/%q", pages.IDType, pages.IDTypeSource)
	}
}

func TestRunFailsOnlyWhenRESTAlsoFails(t *testing.T) {
	f := newFakeAPI()
	f.apiPath = "/nope" // nothing answers /api/access
	d := newTestDiscoverer(t, f, func(o *Options) {
		o.APIPath = "/api"
		o.APIPathSource = SourceConfigured
	})
	_, err := d.Run(context.Background())
	if err == nil {
		t.Fatal("a project whose /access is unparseable must fail")
	}
	if !apierr.HasCode(err, apierr.CodeEndpointNotPayload) {
		t.Fatalf("err = %v, want endpoint_not_payload", err)
	}
}

func TestRunRejectsAWrongBaseURLAnsweringHTML(t *testing.T) {
	// A wrong base URL answers 200 text/html (verified), so the Content-Type
	// check is load-bearing: status alone is useless.
	f := newFakeAPI()
	html := &htmlEverything{}
	d := newTestDiscoverer(t, f, nil)
	d.client, _ = payload.New(payload.Config{
		BaseURL: "http://wrong.test", APIPath: "/api",
		AuthMode: payload.AuthModeAnonymous, RoundTripper: html, Now: fixedClock(),
	})
	_, err := d.Run(context.Background())
	if !apierr.HasCode(err, apierr.CodeEndpointNotPayload) {
		t.Fatalf("err = %v, want endpoint_not_payload", err)
	}
}

type htmlEverything struct{}

func (htmlEverything) RoundTrip(*http.Request) (*http.Response, error) {
	return rawResponse(http.StatusOK, "text/html; charset=utf-8", []byte("<!DOCTYPE html><html></html>")), nil
}

func TestRunWithNoCredentialSkipsMe(t *testing.T) {
	f := newFakeAPI()
	cli, err := payload.New(payload.Config{
		BaseURL: "http://payload.test", APIPath: "/api",
		AuthMode: payload.AuthModeAnonymous, RoundTripper: f, Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	d, err := New(Options{
		Client: cli, BaseURL: "http://payload.test", APIPath: "/api",
		APIPathSource: SourceConfigured, AuthMode: payload.AuthModeAnonymous,
		CredentialAbsent: true, Concurrency: 2, Now: fixedClock(),
		Generation: "gen", CLIVersion: "0.1.0",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("anonymous discovery must continue, not fail: %v", err)
	}
	if f.count("/me") != 0 {
		t.Error("anonymous mode must not issue the /me request at all")
	}
	if res.Manifest.Identity.Verified {
		t.Error("identity.verified must be false in anonymous mode")
	}
	if res.Manifest.Meta.KeyFingerprint != "anon" {
		t.Errorf("key_fingerprint = %q, want the anon sentinel", res.Manifest.Meta.KeyFingerprint)
	}
}

func TestRunRejectsAWrongCredential(t *testing.T) {
	f := newFakeAPI()
	cli, err := payload.New(payload.Config{
		BaseURL: "http://payload.test", APIPath: "/api",
		AuthMode: payload.AuthModeAPIKey, AuthCollection: "users",
		Credential: "wrong-key", RoundTripper: f, Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	d, _ := New(Options{
		Client: cli, BaseURL: "http://payload.test", APIPath: "/api",
		APIPathSource: SourceConfigured, AuthMode: payload.AuthModeAPIKey,
		AuthCollection: "users", AuthCollectionSource: SourceConfigured,
		Concurrency: 2, Now: fixedClock(), Generation: "gen",
	})
	_, err = d.Run(context.Background())
	if !apierr.HasCode(err, apierr.CodeAuthInvalid) {
		t.Fatalf("err = %v, want auth_invalid (the server answers 200 user:null)", err)
	}
}

func TestManifestNeverContainsACredential(t *testing.T) {
	f := newFakeAPI()
	d := newTestDiscoverer(t, f, nil)
	res, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	blob, err := json.Marshal(struct {
		M *Manifest         `json:"m"`
		S map[string]*Shard `json:"s"`
	}{res.Manifest, res.Shards})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(blob, []byte(testAPIKey)) {
		t.Fatal("the credential reached a manifest artefact")
	}
	// Nor may the /me response body, which carries the API key in plaintext on
	// a project with useAPIKey.
	if bytes.Contains(blob, []byte(`"_strategy"`)) {
		t.Fatal("the /me user object was written to the manifest")
	}
}

func TestDiscoveryNeverArmsLevel3(t *testing.T) {
	// §8.4(a): discovery deliberately generates every Level-3 trigger, so it
	// must never set the reactive-invalidation flag. A counting invalidator
	// proves no request opted in.
	f := newFakeAPI()
	inv := &countingInvalidator{}
	cli, err := payload.New(payload.Config{
		BaseURL: "http://payload.test", APIPath: "/api",
		AuthMode: payload.AuthModeAPIKey, AuthCollection: "users", Credential: testAPIKey,
		RoundTripper: f, Now: fixedClock(), Invalidator: inv, Concurrency: 4,
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	d, _ := New(Options{
		Client: cli, BaseURL: "http://payload.test", APIPath: "/api",
		APIPathSource: SourceConfigured, AuthMode: payload.AuthModeAPIKey,
		AuthCollection: "users", AuthCollectionSource: SourceConfigured,
		Concurrency: 4, Now: fixedClock(), Generation: "gen",
	})
	if _, err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := inv.calls.Load(); n != 0 {
		t.Fatalf("discovery triggered %d reactive invalidations; the ladder recurses if it ever does", n)
	}
}

type countingInvalidator struct{ calls atomicInt }

func (c *countingInvalidator) Invalidate(context.Context, payload.StaleSignal) (payload.Outcome, error) {
	c.calls.Add(1)
	return payload.Outcome{}, nil
}

type atomicInt struct {
	mu sync.Mutex
	n  int64
}

func (a *atomicInt) Add(n int64) { a.mu.Lock(); a.n += n; a.mu.Unlock() }
func (a *atomicInt) Load() int64 { a.mu.Lock(); defer a.mu.Unlock(); return a.n }

func TestRunIsDeterministic(t *testing.T) {
	var first string
	for i := 0; i < 3; i++ {
		f := newFakeAPI()
		d := newTestDiscoverer(t, f, nil)
		res, err := d.Run(context.Background())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		res.Manifest.Diagnostics.ElapsedMS = 0
		b, err := json.Marshal(res.Manifest)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if i == 0 {
			first = string(b)
			continue
		}
		if string(b) != first {
			t.Fatal("two runs against an unchanged server produced different manifests; the §8.4 fingerprints would flap")
		}
	}
}

func hasLimitation(m *Manifest, code string) bool {
	for _, l := range m.Limitations {
		if l.Code == code {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

var _ = strconv.Itoa
