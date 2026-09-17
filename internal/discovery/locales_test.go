package discovery

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/payload"
)

func TestAnalyzeLocaleAll(t *testing.T) {
	tests := []struct {
		name          string
		docs          []map[string]any
		wantCodes     []string
		wantLocalized []string
		wantNot       []string
	}{
		{
			name: "two fields agree",
			docs: []map[string]any{{
				"id":          float64(1),
				"title":       map[string]any{"en": "Home", "de": "Start"},
				"description": map[string]any{"en": "x", "de": "y"},
				"slug":        "home",
			}},
			wantCodes:     []string{"de", "en"},
			wantLocalized: []string{"description", "title"},
			wantNot:       []string{"slug"},
		},
		{
			// One field alone could be an ordinary group whose keys happen to
			// look like codes, so it proves nothing.
			name: "single field is not enough",
			docs: []map[string]any{{
				"title": map[string]any{"en": "Home", "de": "Start"},
				"slug":  "home",
			}},
			wantCodes: nil,
		},
		{
			name: "a group with long keys is not a locale map",
			docs: []map[string]any{{
				"meta":  map[string]any{"metaTitleLong": "x", "description": "y"},
				"other": map[string]any{"metaTitleLong": "x", "description": "y"},
			}},
			wantCodes: nil,
		},
		{
			name:      "no documents",
			docs:      nil,
			wantCodes: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AnalyzeLocaleAll(tt.docs)
			if len(tt.wantCodes) == 0 {
				if len(got.Candidates) != 0 {
					t.Fatalf("candidates = %v, want none", got.Candidates)
				}
				return
			}
			if !reflect.DeepEqual(got.Candidates, tt.wantCodes) {
				t.Fatalf("candidates = %v, want %v", got.Candidates, tt.wantCodes)
			}
			if tt.wantLocalized != nil && !reflect.DeepEqual(got.Localized, tt.wantLocalized) {
				t.Errorf("localized = %v, want %v", got.Localized, tt.wantLocalized)
			}
			if tt.wantNot != nil && !reflect.DeepEqual(got.NotLocalized, tt.wantNot) {
				t.Errorf("not localized = %v, want %v", got.NotLocalized, tt.wantNot)
			}
		})
	}
}

func TestCrossValidateLocales(t *testing.T) {
	tests := []struct {
		name       string
		candidates []string
		enum       []string
		want       bool
	}{
		// LocaleInputType mangles the codes: en-US is exposed as en_US, and
		// introspection never exposes the underlying value.
		{"mangled match", []string{"en-US", "es.419"}, []string{"en_US", "es_419"}, true},
		{"plain match", []string{"de", "en"}, []string{"de", "en"}, true},
		{"count mismatch", []string{"de"}, []string{"de", "en"}, false},
		{"unknown candidate", []string{"de", "fr"}, []string{"de", "en"}, false},
		{"no enum", []string{"de"}, nil, false},
		{"no candidates", nil, []string{"de"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CrossValidateLocales(tt.candidates, tt.enum); got != tt.want {
				t.Fatalf("CrossValidateLocales = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolveLocalesLadder(t *testing.T) {
	tests := []struct {
		name       string
		in         LocaleInput
		wantCodes  []string
		wantSource string
	}{
		{"profile pin wins", LocaleInput{
			Configured: []string{"en", "de"},
			Analysis:   LocaleAnalysis{Candidates: []string{"fr"}},
		}, []string{"de", "en"}, SourceConfigured},
		{"cross-validated locale=all", LocaleInput{
			Analysis:         LocaleAnalysis{Candidates: []string{"de", "en"}},
			EnumNames:        []string{"de", "en"},
			GraphQLReachable: true,
		}, []string{"de", "en"}, SourceLocaleAll},
		{"unverified locale=all", LocaleInput{
			Analysis: LocaleAnalysis{Candidates: []string{"de", "en"}},
		}, []string{"de", "en"}, SourceLocaleAllUnverified},
		// Mangled enum names inform the count and nothing else: feeding them
		// back as codes would be rejected by PayCLI's own validation.
		{"mangled enum only", LocaleInput{
			EnumNames:        []string{"en_US", "es_419"},
			GraphQLReachable: true,
		}, []string{}, SourceGraphQLEnumMangled},
		{"nothing", LocaleInput{}, []string{}, SourceUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			codes, source, _ := ResolveLocales(tt.in)
			if !reflect.DeepEqual(codes, tt.wantCodes) || source != tt.wantSource {
				t.Fatalf("ResolveLocales = %v/%q want %v/%q", codes, source, tt.wantCodes, tt.wantSource)
			}
		})
	}
}

func TestLocalesValidatableOnlyForTrustedSources(t *testing.T) {
	validatable := map[string]bool{
		SourceConfigured:          true,
		SourceLocaleAll:           true,
		SourceLocaleAllUnverified: false,
		SourceGraphQLEnumMangled:  false,
		SourceUnknown:             false,
	}
	for source, want := range validatable {
		if got := LocalesValidatable(source); got != want {
			t.Errorf("LocalesValidatable(%q) = %v, want %v", source, got, want)
		}
	}
}

func TestLocaleUnverifiedMessageStatesTheSilentFailure(t *testing.T) {
	// The warning exists because Payload coerces an unknown code to the
	// default locale with HTTP 200 and no error, which is invisible.
	if LocaleUnverifiedMessage == "" {
		t.Fatal("empty message")
	}
	if LocaleUnverifiedHint("local") == "" {
		t.Fatal("empty hint")
	}
	if FallbackNone != "none" {
		t.Fatalf("FallbackNone = %q; sanitizeFallbackLocale accepts false|none|null", FallbackNone)
	}
}

// newLocaleDiscoverer builds a Discoverer over a fake project whose `pages`
// documents carry per-locale maps, with configurable GraphQL and profile pins.
func newLocaleDiscoverer(t *testing.T, f *fakeAPI, mutate func(*Options)) *Discoverer {
	t.Helper()
	cli, err := payload.New(payload.Config{
		BaseURL: "http://payload.test", APIPath: f.apiPath,
		AuthMode: payload.AuthModeAPIKey, AuthCollection: "users", Credential: testAPIKey,
		RoundTripper: f, Now: fixedClock(), Concurrency: 4,
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	opt := Options{
		Client: cli, BaseURL: "http://payload.test", APIPath: f.apiPath,
		APIPathSource: SourceConfigured, AuthMode: payload.AuthModeAPIKey,
		AuthCollection: "users", AuthCollectionSource: SourceConfigured,
		Concurrency: 4, Now: fixedClock(), Generation: "gen", CLIVersion: "0.1.0",
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

// localisedFakeAPI is a project with graphQL: { disable: true } whose pages
// documents answer ?locale=all with per-locale maps.
func localisedFakeAPI() *fakeAPI {
	f := newFakeAPI()
	f.graphQLMode = "404empty"
	f.docs["pages"] = []map[string]any{{
		"id":    json.Number("16"),
		"title": map[string]any{"en": "Home", "de": "Start"},
		"meta":  map[string]any{"en": "About", "de": "Über"},
		"body":  map[string]any{"en": "Text", "de": nil},
		"slug":  "home",
	}}
	return f
}

// A project with GraphQL disabled can never populate `enabled` from the plural
// Query field's locale argument, so it used to stay nil forever — and
// find.go's resolveLocale then skipped fallback-locale=none, which makes
// `--locale de` answer with DEFAULT-locale text that is indistinguishable from
// a real translation (§1 conflict 35, §7.9a). The ?locale=all sample is
// positive REST evidence and must settle it.
func TestLocalizationEnabledFromLocaleAllWhenGraphQLIsDisabled(t *testing.T) {
	d := newLocaleDiscoverer(t, localisedFakeAPI(), nil)
	res, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	loc := res.Manifest.Capabilities.Localization

	if loc.Enabled == nil || !*loc.Enabled {
		t.Fatalf("localization.enabled = %v, want true: ?locale=all returned per-locale maps", show(loc.Enabled))
	}
	if loc.EnabledSource != SourceLocaleAll {
		t.Errorf("enabled_source = %q, want %q", loc.EnabledSource, SourceLocaleAll)
	}
	if loc.FallbackDefault == nil || *loc.FallbackDefault != FallbackNone {
		t.Errorf("fallback_default = %v, want %q — without it every read masquerades the default locale as a translation",
			loc.FallbackDefault, FallbackNone)
	}
	if hasLimitation(res.Manifest, LimLocalizationUnknown) {
		t.Errorf("LOCALIZATION_UNKNOWN is still reported although the sample settled it")
	}
	if got := loc.Locales; len(got) != 2 || got[0] != "de" || got[1] != "en" {
		t.Errorf("locales = %v, want [de en]", got)
	}
}

// A profile that pins locale codes has told PayCLI the project is localised;
// nobody pins locale codes on a project without localisation.
func TestLocalizationEnabledFromConfiguredLocales(t *testing.T) {
	f := newFakeAPI()
	f.graphQLMode = "404empty"
	d := newLocaleDiscoverer(t, f, func(o *Options) { o.ConfiguredLocales = []string{"en", "de"} })
	res, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	loc := res.Manifest.Capabilities.Localization
	if loc.Enabled == nil || !*loc.Enabled {
		t.Fatalf("localization.enabled = %v, want true for a profile that pins locales", show(loc.Enabled))
	}
	if loc.EnabledSource != SourceConfigured {
		t.Errorf("enabled_source = %q, want %q", loc.EnabledSource, SourceConfigured)
	}
	if loc.FallbackDefault == nil || *loc.FallbackDefault != FallbackNone {
		t.Errorf("fallback_default = %v, want %q", loc.FallbackDefault, FallbackNone)
	}
}

// §21.3 rule 1: a defaulted value is not a discovered fact. Payload's
// localization.defaultLocale is exposed by no API surface, and `pay skills
// install --with-project-context` renders this field into PROJECT.md as a flat
// statement, so guessing locales[0] ships a wrong fact to the agent on every
// project whose default does not sort first (en/de, en/ar, en/cs …).
func TestLocalizationDefaultIsNeverGuessedFromTheLocaleList(t *testing.T) {
	d := newLocaleDiscoverer(t, localisedFakeAPI(), nil)
	res, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	loc := res.Manifest.Capabilities.Localization
	if len(loc.Locales) < 2 {
		t.Fatalf("precondition: want several locales, got %v", loc.Locales)
	}
	if loc.Default != nil {
		t.Errorf("localization.default = %q, want null: no API exposes defaultLocale, and %q is merely the first code alphabetically",
			*loc.Default, loc.Locales[0])
	}
	if loc.DefaultSource != SourceUnknown {
		t.Errorf("default_source = %q, want %q", loc.DefaultSource, SourceUnknown)
	}
}

// The asymmetry §7.9 rests on: a sample with no locale maps proves nothing, so
// `enabled` stays nil (never false) and LOCALIZATION_UNKNOWN is reported.
func TestLocalizationStaysUnknownWithoutEvidence(t *testing.T) {
	f := newFakeAPI()
	f.graphQLMode = "404empty"
	d := newLocaleDiscoverer(t, f, nil)
	res, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	loc := res.Manifest.Capabilities.Localization
	if loc.Enabled != nil {
		t.Fatalf("localization.enabled = %v, want null: an unlocalised sample is not proof of anything", show(loc.Enabled))
	}
	if loc.EnabledSource != SourceUnknown {
		t.Errorf("enabled_source = %q, want %q", loc.EnabledSource, SourceUnknown)
	}
	if !hasLimitation(res.Manifest, LimLocalizationUnknown) {
		t.Errorf("LOCALIZATION_UNKNOWN must be reported while enabled is null")
	}
}
