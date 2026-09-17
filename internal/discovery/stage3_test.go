package discovery

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/payload"
)

func newProbeDiscoverer(t *testing.T, f *fakeAPI) *Discoverer {
	t.Helper()
	cli, err := payload.New(payload.Config{
		BaseURL: "http://payload.test", APIPath: f.apiPath,
		AuthMode: payload.AuthModeAPIKey, AuthCollection: "users", Credential: testAPIKey,
		RoundTripper: f, Now: fixedClock(), Concurrency: 4,
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	d, err := New(Options{
		Client: cli, BaseURL: "http://payload.test", APIPath: f.apiPath,
		APIPathSource: SourceConfigured, AuthMode: payload.AuthModeAPIKey,
		AuthCollection: "users", AuthCollectionSource: SourceConfigured,
		Concurrency: 4, Now: fixedClock(), Generation: "gen",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

func allProbes() probeNeed {
	return probeNeed{
		Endpoints: true, Upload: true, Versions: true, Drafts: true,
		Trash: true, Folders: true, Auth: true, Labels: true, IDSample: true,
	}
}

func TestProbeCollectionTable(t *testing.T) {
	tests := []struct {
		slug      string
		upload    bool
		versions  bool
		drafts    bool
		trash     bool
		folders   bool
		auth      bool
		label     string
		labelOK   bool
		idPresent bool
	}{
		{slug: "pages", versions: true, drafts: true, label: "Pages", labelOK: true, idPresent: true},
		{slug: "media", upload: true, folders: true, label: "Media", labelOK: true},
		{slug: "posts", trash: true, label: "Posts", labelOK: true, idPresent: true},
		{slug: "users", auth: true, label: "Users", labelOK: true, idPresent: true},
	}
	for _, tt := range tests {
		t.Run(tt.slug, func(t *testing.T) {
			f := newFakeAPI()
			d := newProbeDiscoverer(t, f)
			got := d.probeCollection(context.Background(), tt.slug, allProbes())
			check := func(name string, got *bool, want bool) {
				t.Helper()
				if got == nil {
					t.Errorf("%s = null, want %v", name, want)
					return
				}
				if *got != want {
					t.Errorf("%s = %v, want %v", name, *got, want)
				}
			}
			check("upload", got.Upload, tt.upload)
			check("versions", got.Versions, tt.versions)
			check("drafts", got.Drafts, tt.drafts)
			check("trash", got.Trash, tt.trash)
			check("folders", got.Folders, tt.folders)
			check("auth", got.Auth, tt.auth)
			check("endpoints_disabled", got.EndpointsDisabled, false)
			if got.LabelParsed != tt.labelOK || got.PluralLabel != tt.label {
				t.Errorf("label = %q/%v want %q/%v", got.PluralLabel, got.LabelParsed, tt.label, tt.labelOK)
			}
			if got.SampleIDPresent != tt.idPresent {
				t.Errorf("sample id present = %v, want %v", got.SampleIDPresent, tt.idPresent)
			}
		})
	}
}

// TestProbe501GuardRunsFirst is the misclassification §7.5 exists to prevent:
// without the guard, payload-migrations reads as an upload positive, because
// its /file/ probe answers 501 rather than `Route not found`.
func TestProbe501GuardRunsFirstAndSkipsEverythingElse(t *testing.T) {
	f := newFakeAPI()
	d := newProbeDiscoverer(t, f)
	got := d.probeCollection(context.Background(), "payload-migrations", allProbes())
	if got.EndpointsDisabled == nil || !*got.EndpointsDisabled {
		t.Fatal("the 501 guard did not fire")
	}
	if got.Upload != nil {
		t.Errorf("upload = %v; every other probe must be skipped", *got.Upload)
	}
	if got.Versions != nil || got.Drafts != nil || got.Trash != nil || got.Auth != nil {
		t.Error("probes ran after the 501 guard")
	}
	// Exactly one request: the guard's own.
	if n := f.count("/api/payload-migrations"); n != 1 {
		t.Errorf("issued %d requests to a 501 collection, want 1", n)
	}
}

func TestVersionsProbeRequiresTheDocsArray(t *testing.T) {
	// payload-preferences registers a custom GET /:key that shadows /versions
	// and answers 200 {"message":"Not Found","value":null} (verified live), so
	// a status check alone reports versions on a collection that has none.
	f := newFakeAPI()
	f.accessCollections["payload-preferences"] = map[string]any{"read": true}
	f.labelFor["payload-preferences"] = "Payload Preferences"
	d := newProbeDiscoverer(t, f)
	got := d.probeCollection(context.Background(), "payload-preferences", allProbes())
	if got.Versions == nil || *got.Versions {
		t.Fatalf("versions = %v; a 200 with no docs array is not a versions endpoint", show(got.Versions))
	}
}

func TestVersionsProbeHarvestsTheParentID(t *testing.T) {
	// parent is the DOCUMENT id (16) while the version's own id is a different
	// row (20), which is what the id_type ladder's second rung needs.
	f := newFakeAPI()
	d := newProbeDiscoverer(t, f)
	got := d.probeCollection(context.Background(), "pages", allProbes())
	if !got.VersionParentPresent {
		t.Fatal("the version parent id was not harvested")
	}
	if idType, _ := IDTypeOfJSON(got.VersionParentID); idType != IDTypeNumber {
		t.Fatalf("parent id type = %q", idType)
	}
}

func TestLabelProbeFailsSoftOnATranslatedMessage(t *testing.T) {
	f := newFakeAPI()
	f.labelFor["pages"] = "" // the fake then answers a German message
	d := newProbeDiscoverer(t, f)
	got := d.probeCollection(context.Background(), "pages", allProbes())
	if got.LabelParsed {
		t.Fatal("a translated message must not parse")
	}
	if !got.LabelTried {
		t.Fatal("the attempt must be recorded so LABELS_UNAVAILABLE can be declared")
	}
	labels := DeriveLabels("pages", "Page")
	if labels.Plural == "" || labels.Source != SourceDerived {
		t.Fatalf("labels must fall back, never blank: %+v", labels)
	}
}

func TestLabelProbeIsSkippedWhenNotWanted(t *testing.T) {
	f := newFakeAPI()
	d := newProbeDiscoverer(t, f)
	need := allProbes()
	need.Labels = false
	d.probeCollection(context.Background(), "pages", need)
	for _, r := range f.requests() {
		if strings.HasPrefix(r, "DELETE ") {
			t.Fatalf("--no-labels must skip the DELETE verb entirely: %s", r)
		}
	}
}

func TestIsEndpointsDisabledNeedsStatusAndPrefix(t *testing.T) {
	tests := []struct {
		name string
		res  *restResult
		want bool
	}{
		{"real", &restResult{Status: 501, ContentType: "application/json",
			Body: []byte(`{"message":"Cannot GET http://x/api/payload-migrations"}`)}, true},
		{"501 from a proxy", &restResult{Status: 501, ContentType: "text/html", Body: []byte("<html>")}, false},
		{"501 with another message", &restResult{Status: 501, ContentType: "application/json",
			Body: []byte(`{"message":"Not implemented"}`)}, false},
		{"404", &restResult{Status: 404, ContentType: "application/json",
			Body: []byte(`{"message":"Cannot GET x"}`)}, false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isEndpointsDisabled(tt.res); got != tt.want {
				t.Fatalf("isEndpointsDisabled = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProjectProbes(t *testing.T) {
	f := newFakeAPI()
	d := newProbeDiscoverer(t, f)
	got := d.runProjectProbes(context.Background())
	if got.Reorder == nil || *got.Reorder {
		t.Errorf("reorder = %v; a 404 Route not found means no orderable fields", show(got.Reorder))
	}
	if got.OG == nil || !*got.OG {
		t.Errorf("og = %v", show(got.OG))
	}
	// The reorder probe must not carry a payload that could reorder anything.
	for _, r := range f.requests() {
		if strings.Contains(r, "/reorder") && !strings.HasPrefix(r, "POST ") {
			t.Errorf("unexpected reorder request: %s", r)
		}
	}
}

func TestSampleDocsUsesTheDocumentKeySet(t *testing.T) {
	f := newFakeAPI()
	d := newProbeDiscoverer(t, f)
	docs := d.sampleDocs(context.Background(), "pages", 3, false)
	if len(docs) != 1 {
		t.Fatalf("docs = %d", len(docs))
	}
	if _, ok := docs[0]["title"]; !ok {
		t.Error("the sample must carry the field names")
	}
	// /api/access `fields` is never consulted: it is the boolean true for a
	// privileged key and carries no names at all.
	for _, r := range f.requests() {
		if strings.Contains(r, "/access") {
			t.Errorf("the field sample must not read /access: %s", r)
		}
	}
}

func TestProbeNeedSkipsWhatGraphQLAlreadyEstablished(t *testing.T) {
	d := &Discoverer{}
	schema := &Schema{Types: map[string]*IntroType{"Page": {Kind: KindObject, Name: "Page"}}}
	known := &row{slug: "pages", kind: kindCollection, inAccess: true, inGraphQL: true,
		entity: &entity{Slug: "pages", Singular: "Page", Versions: boolPtr(true), Auth: boolPtr(false), IDTypeGraphQL: IDTypeNumber}}
	need := d.probeNeedFor(known, schema)
	if need.Upload || need.Versions || need.Drafts || need.Trash || need.Folders || need.Auth || need.Labels {
		t.Fatalf("a GraphQL-described entity must not be re-probed: %+v", need)
	}

	unknown := &row{slug: "payload-migrations", kind: kindCollection, inAccess: true}
	need = d.probeNeedFor(unknown, schema)
	if !need.Endpoints || !need.Upload || !need.Versions || !need.Auth || !need.Labels {
		t.Fatalf("an entity GraphQL could not describe needs every probe: %+v", need)
	}

	deep := &Discoverer{opt: Options{Deep: true}}
	need = deep.probeNeedFor(known, schema)
	if !need.Upload || !need.Labels || !need.Count {
		t.Fatalf("--deep must widen the probes: %+v", need)
	}
}

func TestItoa(t *testing.T) {
	for n, want := range map[int]string{0: "0", 3: "3", 100: "100", -12: "-12"} {
		if got := itoa(n); got != want {
			t.Errorf("itoa(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestRestResultMessageOnlyReadsTopLevel(t *testing.T) {
	r := &restResult{Status: http.StatusNotFound, ContentType: "application/json",
		Body: []byte(`{"message":"Route not found \"/api/pages/file/x\""}`)}
	if !strings.HasPrefix(r.message(), msgRouteNotFound) {
		t.Fatalf("message = %q", r.message())
	}
	// A 500 with an errors[] array has no top-level message, which is what
	// makes an upload collection's /file/ probe a positive.
	r = &restResult{Status: 500, ContentType: "application/json",
		Body: []byte(`{"errors":[{"message":"Something went wrong."}]}`)}
	if r.message() != "" {
		t.Fatalf("message = %q, want empty", r.message())
	}
}
