package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/config"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

func boolp(v bool) *bool    { return &v }
func strp(v string) *string { return &v }

// testField builds a shard field with the tri-state keys filled in.
func testField(name, payloadType string, opts ...func(*discovery.Field)) discovery.Field {
	f := discovery.NewField(name, name)
	f.PayloadType = payloadType
	f.Queryable = true
	f.Sortable = true
	f.Localized = boolp(false)
	for _, o := range opts {
		o(&f)
	}
	return f
}

func withOptions(values ...string) func(*discovery.Field) {
	return func(f *discovery.Field) {
		f.Options = values
		f.OptionsSource = discovery.SourceGraphQLEnum
	}
}

func withRelation(target, shape string) func(*discovery.Field) {
	return func(f *discovery.Field) {
		f.RelationTo = []string{target}
		f.RelationToSource = discovery.SourceGraphQL
		f.WriteShape = strp(shape)
	}
}

func withRequired() func(*discovery.Field) {
	return func(f *discovery.Field) { f.Required = boolp(true) }
}

// pagesShard is the shard used across these tests.
func pagesShard() *discovery.Shard {
	s := discovery.NewShard("gen", "pages")
	s.Fields = []discovery.Field{
		testField("id", discovery.TypeID),
		testField("title", discovery.TypeText, withRequired()),
		testField("slug", discovery.TypeText),
		testField("_status", discovery.TypeSelect, withOptions("draft", "published")),
		testField("publishedAt", discovery.TypeDate),
		testField("updatedAt", discovery.TypeDate),
		testField("createdAt", discovery.TypeDate),
		testField("views", discovery.TypeNumber),
		testField("featured", discovery.TypeCheckbox),
		testField("heroImage", discovery.TypeUpload, withRelation("media", discovery.WriteShapeID)),
		testField("tags", discovery.TypeRelationship, withRelation("categories", discovery.WriteShapeIDArray)),
		testField("layout", discovery.TypeBlocks, withRequired()),
		testField("content", discovery.TypeRichText),
	}
	for i := range s.Fields {
		if s.Fields[i].PayloadType == discovery.TypeBlocks || s.Fields[i].PayloadType == discovery.TypeRichText {
			s.Fields[i].WriteShape = strp(discovery.WriteShapeJSON)
		}
	}
	s.Blocks = map[string][]string{"layout": {"cta", "content", "mediaBlock"}}
	s.BlocksSource = discovery.SourceProjectSource
	s.Finalize()
	return s
}

func pagesManifest() *discovery.Manifest {
	m := discovery.NewManifest()
	m.Collections = []*discovery.Collection{
		{
			Slug:         "pages",
			Labels:       discovery.Labels{Singular: "Page", Plural: "Pages"},
			IDType:       discovery.IDTypeNumber,
			IDTypeSource: discovery.SourceGraphQL,
			Publishable:  true,
			TitleField:   strp("title"),
			DateFields:   []string{"publishedAt", "updatedAt", "createdAt"},
			Flags: discovery.Flags{
				Drafts:   boolp(true),
				Trash:    boolp(true),
				Versions: boolp(true),
				Upload:   boolp(false),
			},
		},
		{
			Slug:   "media",
			Labels: discovery.Labels{Singular: "Media"},
			IDType: discovery.IDTypeNumber,
			Flags:  discovery.Flags{Upload: boolp(true)},
		},
		{
			Slug:   "categories",
			Labels: discovery.Labels{Singular: "Category"},
			IDType: discovery.IDTypeString,
		},
	}
	m.Globals = []*discovery.Global{
		{Slug: "header", Labels: discovery.Labels{Singular: "Header"}},
	}
	return m
}

func testTarget() *collTarget {
	m := pagesManifest()
	c, _ := m.Collection("pages")
	return &collTarget{Slug: "pages", Coll: c, Shard: pagesShard()}
}

func testDeps() *Deps {
	return &Deps{Manifest: pagesManifest()}
}

// readCmd builds a command carrying the full read flag set.
func readCmd() (*cobra.Command, *readFlags) {
	f := &readFlags{}
	cmd := &cobra.Command{Use: "find"}
	f.registerFilterFlags(cmd)
	f.registerSelectionFlags(cmd, 0)
	f.registerPagingFlags(cmd, 20)
	return cmd, f
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

func TestCollectionUnknownSuggests(t *testing.T) {
	d := testDeps()
	_, err := d.collection(context.Background(), "page")
	if err == nil {
		t.Fatal("expected collection_unknown")
	}
	if got := apierr.CodeOf(err); got != apierr.CodeCollectionUnknown {
		t.Fatalf("code = %s, want collection_unknown", got)
	}
	if apierr.ExitCode(err) != apierr.ExitCapability {
		t.Fatalf("exit = %d, want %d", apierr.ExitCode(err), apierr.ExitCapability)
	}
	e, _ := apierr.As(err)
	if len(e.DidYouMean) == 0 || e.DidYouMean[0] != "pages" {
		t.Fatalf("did_you_mean = %v, want pages first", e.DidYouMean)
	}
}

// TestNilManifestNeverRejects is §9.3's tri-state rule at the top level: with
// no manifest there is nothing to validate against, so nothing is rejected.
func TestNilManifestNeverRejects(t *testing.T) {
	d := &Deps{}
	tgt, err := d.collection(context.Background(), "whatever")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tgt.Coll != nil || tgt.Shard != nil {
		t.Fatal("expected an empty target")
	}
	if tgt.idType() != "" {
		t.Fatalf("idType = %q, want empty (unknown)", tgt.idType())
	}
	// An id that is not a number must still be sent when id_type is unknown.
	if e := apierr.CheckID(tgt.Slug, "66f1a2b3c4d5e6f708192a3b", tgt.idType()); e != nil {
		t.Fatalf("unknown id_type must not reject: %v", e)
	}
}

func TestBuildValidation(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*readFlags)
		wantErr  apierr.Code
		wantExit int
	}{
		{
			name:     "limit zero is unlimited on the server",
			mutate:   func(f *readFlags) { f.limit = 0 },
			wantErr:  apierr.CodeInvalidArgs,
			wantExit: apierr.ExitValidation,
		},
		{
			name:     "negative limit",
			mutate:   func(f *readFlags) { f.limit = -3 },
			wantErr:  apierr.CodeInvalidArgs,
			wantExit: apierr.ExitValidation,
		},
		{
			name:     "unknown sort field",
			mutate:   func(f *readFlags) { f.sort = []string{"-bogusField"} },
			wantErr:  apierr.CodeInvalidSortField,
			wantExit: apierr.ExitValidation,
		},
		{
			name:     "unknown select key",
			mutate:   func(f *readFlags) { f.selectFields = []string{"nope"} },
			wantErr:  apierr.CodeUnknownField,
			wantExit: apierr.ExitValidation,
		},
		{
			name:     "unknown where path",
			mutate:   func(f *readFlags) { f.where = []string{"nosuch eq 1"} },
			wantErr:  apierr.CodeQueryPathInvalid,
			wantExit: apierr.ExitValidation,
		},
		{
			name:     "invalid enum value in where",
			mutate:   func(f *readFlags) { f.where = []string{"_status eq archived"} },
			wantErr:  apierr.CodeInvalidOption,
			wantExit: apierr.ExitValidation,
		},
		{
			name:     "unknown date field",
			mutate:   func(f *readFlags) { f.since = "7d"; f.dateField = "nope" },
			wantErr:  apierr.CodeUnknownField,
			wantExit: apierr.ExitValidation,
		},
		{
			name:     "date field without a bound",
			mutate:   func(f *readFlags) { f.dateField = "updatedAt" },
			wantErr:  apierr.CodeInvalidArgs,
			wantExit: apierr.ExitValidation,
		},
		{
			name:     "bad populate spec",
			mutate:   func(f *readFlags) { f.populate = []string{"mediafilename"} },
			wantErr:  apierr.CodeInvalidArgs,
			wantExit: apierr.ExitValidation,
		},
		{
			name:     "bad joins spec",
			mutate:   func(f *readFlags) { f.joins = []string{"related"} },
			wantErr:  apierr.CodeInvalidArgs,
			wantExit: apierr.ExitValidation,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd, f := readCmd()
			f.limit = 20
			tc.mutate(f)
			_, err := f.build(cmd, testDeps(), testTarget())
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := apierr.CodeOf(err); got != tc.wantErr {
				t.Fatalf("code = %s, want %s (%v)", got, tc.wantErr, err)
			}
			if got := apierr.ExitCode(err); got != tc.wantExit {
				t.Fatalf("exit = %d, want %d", got, tc.wantExit)
			}
		})
	}
}

// TestBuildEscapeHatches proves --no-validate-sort/--no-validate-where turn the
// local checks off rather than merely softening them.
func TestBuildEscapeHatches(t *testing.T) {
	cmd, f := readCmd()
	f.limit = 20
	f.sort = []string{"bogusField"}
	f.where = []string{"nosuch eq 1"}
	f.noValidateSort = true
	f.noValidateWhere = true
	if _, err := f.build(cmd, testDeps(), testTarget()); err != nil {
		t.Fatalf("escape hatches did not bypass validation: %v", err)
	}
}

// TestBuildUnknownFieldsAreSentWhenNothingWasLearned is the tri-state rule at
// the query layer: no shard means no rejection.
func TestBuildUnknownFieldsAreSentWhenNothingWasLearned(t *testing.T) {
	cmd, f := readCmd()
	f.limit = 20
	f.sort = []string{"bogusField"}
	f.selectFields = []string{"alsoBogus"}
	f.where = []string{"nosuch eq 1"}
	tgt := &collTarget{Slug: "pages"}
	if _, err := f.build(cmd, &Deps{}, tgt); err != nil {
		t.Fatalf("unknown facts must not reject: %v", err)
	}
}

func TestResolveDateField(t *testing.T) {
	withPublishedAt := testTarget()
	noPublishedAt := testTarget()
	noPublishedAt.Coll.DateFields = []string{"updatedAt", "createdAt"}
	shard := discovery.NewShard("gen", "pages")
	shard.Fields = []discovery.Field{testField("updatedAt", discovery.TypeDate)}
	shard.Finalize()
	noPublishedAt.Shard = shard

	tests := []struct {
		name          string
		flag          string
		published     bool
		target        *collTarget
		wantField     string
		wantDefaulted bool
	}{
		{"explicit flag wins", "createdAt", true, withPublishedAt, "createdAt", false},
		{"published-scoped with publishedAt", "", true, withPublishedAt, "publishedAt", true},
		{"published-scoped without publishedAt", "", true, noPublishedAt, "updatedAt", true},
		{"not published-scoped", "", false, withPublishedAt, "updatedAt", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, defaulted := resolveDateField(tc.flag, tc.published, tc.target)
			if got != tc.wantField || defaulted != tc.wantDefaulted {
				t.Fatalf("got (%q,%v), want (%q,%v)", got, defaulted, tc.wantField, tc.wantDefaulted)
			}
		})
	}
}

// TestSinceReportsResolution covers §9.4's mandatory reporting: the resolved
// bounds and field reach meta, and a defaulted field also emits the warning.
func TestSinceReportsResolution(t *testing.T) {
	cmd, f := readCmd()
	f.limit = 20
	f.since = "7d"
	f.publishedOnly = true
	d := testDeps()
	d.RT = nil

	built, err := f.build(cmd, d, testTarget())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if built.DateField == nil || *built.DateField != "publishedAt" {
		t.Fatalf("date field = %v, want publishedAt", built.DateField)
	}
	if built.Since == nil {
		t.Fatal("meta.since was not reported")
	}
	var found bool
	for _, w := range built.Warnings {
		if w.Code == warnDateFieldDefaulted {
			found = true
			if !strings.Contains(w.Message, "publishedAt") {
				t.Fatalf("warning does not name the field: %q", w.Message)
			}
			if !strings.HasPrefix(w.Hint, "override with --date-field ") {
				t.Fatalf("hint = %q", w.Hint)
			}
		}
	}
	if !found {
		t.Fatalf("no date_field_defaulted warning in %v", built.Warnings)
	}
	m := built.applyMeta(testEmptyMeta())
	if m.DateField == nil || *m.DateField != "publishedAt" {
		t.Fatal("applyMeta did not carry date_field")
	}
}

func TestSinceGrammarErrors(t *testing.T) {
	cmd, f := readCmd()
	f.limit = 20
	f.since = "last tuesday"
	d := testDeps()
	if _, err := f.build(cmd, d, testTarget()); err == nil {
		t.Fatal("expected invalid_args for an unparseable --since")
	} else if apierr.ExitCode(err) != apierr.ExitValidation {
		t.Fatalf("exit = %d, want 5", apierr.ExitCode(err))
	}
}

func TestFeatureGates(t *testing.T) {
	noDrafts := testTarget()
	noDrafts.Coll.Flags.Drafts = boolp(false)
	noTrash := testTarget()
	noTrash.Coll.Flags.Trash = boolp(false)
	unknown := testTarget()
	unknown.Coll.Flags.Drafts = nil
	unknown.Coll.Flags.Trash = nil

	tests := []struct {
		name    string
		target  *collTarget
		mutate  func(*readFlags)
		wantErr bool
	}{
		{"drafts false rejects --draft", noDrafts, func(f *readFlags) { f.draft = true }, true},
		{"drafts false rejects --published-only", noDrafts, func(f *readFlags) { f.publishedOnly = true }, true},
		{"trash false rejects --trash", noTrash, func(f *readFlags) { f.trash = true }, true},
		{"drafts unknown sends --draft", unknown, func(f *readFlags) { f.draft = true }, false},
		{"trash unknown sends --trash", unknown, func(f *readFlags) { f.trash = true }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd, f := readCmd()
			f.limit = 20
			tc.mutate(f)
			_, err := f.build(cmd, testDeps(), tc.target)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected feature_unavailable")
				}
				if apierr.CodeOf(err) != apierr.CodeFeatureUnavailable {
					t.Fatalf("code = %s", apierr.CodeOf(err))
				}
				if apierr.ExitCode(err) != apierr.ExitCapability {
					t.Fatalf("exit = %d, want 10", apierr.ExitCode(err))
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestLocaleValidation(t *testing.T) {
	known := testDeps()
	known.Manifest.Capabilities.Localization = discovery.Localization{
		Enabled:       boolp(true),
		Locales:       []string{"en", "de"},
		LocalesSource: discovery.SourceGraphQLEnum,
	}
	validatable := testDeps()
	validatable.Manifest.Capabilities.Localization = discovery.Localization{
		Enabled:       boolp(true),
		Locales:       []string{"en", "de"},
		LocalesSource: discovery.SourceConfigured,
	}

	t.Run("unverifiable locale passes through with a warning", func(t *testing.T) {
		loc, warn, err := resolveLocale(known, testTarget(), "fr", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if warn == nil || warn.Code != discovery.LocaleWarnUnverified {
			t.Fatalf("warning = %+v", warn)
		}
		if loc.Requested == nil || *loc.Requested != "fr" {
			t.Fatal("requested locale was dropped")
		}
		if loc.Fallback == nil || *loc.Fallback != discovery.FallbackNone {
			t.Fatalf("fallback = %v, want none on a localised project", loc.Fallback)
		}
	})

	t.Run("validatable locale rejects a typo", func(t *testing.T) {
		_, _, err := resolveLocale(validatable, testTarget(), "dee", "")
		if err == nil {
			t.Fatal("expected invalid_option")
		}
		if apierr.CodeOf(err) != apierr.CodeInvalidOption {
			t.Fatalf("code = %s", apierr.CodeOf(err))
		}
	})

	t.Run("non-localised project sends no fallback", func(t *testing.T) {
		loc, warn, err := resolveLocale(testDeps(), testTarget(), "", "")
		if err != nil || warn != nil {
			t.Fatalf("err=%v warn=%v", err, warn)
		}
		if loc.Fallback != nil {
			t.Fatalf("fallback = %v, want nil", *loc.Fallback)
		}
	})
}

func TestQFields(t *testing.T) {
	got := qFields(testTarget())
	want := []string{"title", "slug"}
	for i, w := range want {
		if i >= len(got) || got[i] != w {
			t.Fatalf("qFields = %v, want %v first", got, want)
		}
	}
	if len(got) == 0 {
		t.Fatal("no q fields")
	}
	if qFields(&collTarget{Slug: "x"}) != nil {
		t.Fatal("a shardless target must yield no q fields")
	}
}

func TestMixedStatusWarning(t *testing.T) {
	docs := []payload.Doc{
		{"id": 1, "_status": "draft"},
		{"id": 2, "_status": "published"},
		{"id": 3, "_status": "draft"},
	}
	tests := []struct {
		name       string
		docs       []payload.Doc
		drafts     *bool
		filtered   bool
		wantWarn   bool
		wantPrefix string
	}{
		{"mixed results warn", docs, boolp(true), false, true, "2 of 3"},
		{"status filtered does not warn", docs, boolp(true), true, false, ""},
		{"drafts disabled does not warn", docs, boolp(false), false, false, ""},
		{"drafts unknown does not warn", docs, nil, false, false, ""},
		{"all published does not warn", []payload.Doc{{"_status": "published"}}, boolp(true), false, false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := mixedStatusWarning("pages", tc.docs, tc.drafts, tc.filtered)
			if tc.wantWarn != (w != nil) {
				t.Fatalf("warning = %v, want %v", w, tc.wantWarn)
			}
			if w != nil {
				if !strings.HasPrefix(w.Message, tc.wantPrefix) {
					t.Fatalf("message = %q", w.Message)
				}
				if w.Hint != "pay find pages --published-only" {
					t.Fatalf("hint = %q", w.Hint)
				}
			}
		})
	}
}

func TestParsePopulateAndJoins(t *testing.T) {
	pop, err := parsePopulate([]string{"media:filename,url", "categories:title"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pop["media"]) != 2 || pop["media"][0] != "filename" {
		t.Fatalf("populate = %v", pop)
	}
	joins, err := parseJoins([]string{"relatedPosts:limit=5,sort=-createdAt"})
	if err != nil {
		t.Fatal(err)
	}
	if joins["relatedPosts"]["limit"] != "5" || joins["relatedPosts"]["sort"] != "-createdAt" {
		t.Fatalf("joins = %v", joins)
	}
}

func TestMergeIDs(t *testing.T) {
	got := mergeIDs([]string{"1", "2"}, "3, 4 ,,5")
	want := []string{"1", "2", "3", "4", "5"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("mergeIDs = %v, want %v", got, want)
	}
}

// TestDraftTrapWording pins §9.6.2's mandatory verbatim sentences.
func TestDraftTrapWording(t *testing.T) {
	for _, phrase := range []string{
		"does NOT filter out unpublished documents",
		`never published are returned with _status:"draft"`,
		"always pass",
		"--published-only (_status = published)",
		"swaps in the newest draft",
	} {
		if !strings.Contains(draftTrapHelp, phrase) {
			t.Fatalf("draft-trap help is missing %q:\n%s", phrase, draftTrapHelp)
		}
	}
	for _, cmd := range []*cobra.Command{newFindCmd(nil), newGetCmd(nil)} {
		if !strings.Contains(cmd.Long, "does NOT filter out unpublished documents") {
			t.Fatalf("%s help does not carry the draft trap", cmd.Name())
		}
	}
}

// TestCountHasNoDraftFlag encodes §9.6.3: the count route ignores draft, so
// offering the flag would be a promise PayCLI cannot keep.
func TestCountHasNoDraftFlag(t *testing.T) {
	cmd := newCountCmd(nil)
	if cmd.Flags().Lookup("draft") != nil {
		t.Fatal("pay count must not offer --draft")
	}
	if !strings.Contains(cmd.Long, "IGNORES the draft parameter") {
		t.Fatal("pay count help must say why --draft is absent")
	}
}

func TestSchemaKeepsNilContract(t *testing.T) {
	empty := &collTarget{Slug: "pages", Shard: discovery.NewShard("g", "pages")}
	s := empty.schema(&config.Resolved{})
	if s.Queryable != nil || s.Sortable != nil || s.Selectable != nil {
		t.Fatalf("an empty shard must yield nil sets, got %+v", s)
	}
}

func testEmptyMeta() output.Meta { return output.Meta{} }

// TestLocaleFallbackWhenLocalisationIsUnknown is finding 19's remaining half.
//
// Localization.Enabled is a tri-state fed (originally) only by the GraphQL
// schema, so on a project with graphQL:{disable:true} — a supported
// portability case — it stays nil no matter how localised the project really
// is. resolveLocale only sent fallback-locale=none when Enabled was
// explicitly true, so `--locale de` went out with Payload's default
// fallback:true and every untranslated field came back in the DEFAULT locale,
// indistinguishable from a real translation. Unknown must be treated as
// "maybe", not as "off".
func TestLocaleFallbackWhenLocalisationIsUnknown(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name         string
		enabled      *bool
		locale       string
		fallbackFlag string
		want         string // "" means meta.locale.fallback must be null
	}{
		{"localisation unknown, a locale is requested", nil, "de", "", discovery.FallbackNone},
		{"localisation known on", &yes, "de", "", discovery.FallbackNone},
		{"localisation known off sends nothing to fall back from", &no, "de", "", ""},
		{"unknown, but no locale requested", nil, "", "", ""},
		{"an explicit --fallback-locale always wins", nil, "de", "en", "en"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := &discovery.Manifest{}
			m.Capabilities.Localization = discovery.Localization{Enabled: tc.enabled}
			d := &Deps{RT: &Runtime{Cfg: &config.Resolved{}}, Manifest: m}

			got, _, err := resolveLocale(d, nil, tc.locale, tc.fallbackFlag)
			if err != nil {
				t.Fatalf("resolveLocale: %v", err)
			}
			switch {
			case tc.want == "":
				if got.Fallback != nil {
					t.Fatalf("fallback = %q, want null", *got.Fallback)
				}
			case got.Fallback == nil:
				t.Fatalf("fallback is null, want %q — --locale %s would return "+
					"default-locale text as if it were translated", tc.want, tc.locale)
			case *got.Fallback != tc.want:
				t.Fatalf("fallback = %q, want %q", *got.Fallback, tc.want)
			}
		})
	}
}
