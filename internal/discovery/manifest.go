package discovery

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// ManifestVersion is §7.8.1's layout version. It is 2 because the single
// consolidated manifest of the first draft was split into an index plus field
// shards; a manifest_version: 1 file on disk is a cache miss, not a parse
// error.
const ManifestVersion = 2

// Manifest is the discovery index — everything help text, `pay collections`,
// `pay explain --slim`, shell completion and target resolution need. It stays
// roughly 1 KB per collection regardless of field count, because the fields
// themselves live in per-entity Shards.
type Manifest struct {
	ManifestVersion int       `json:"manifest_version"`
	Generation      string    `json:"generation"`
	CLIVersion      string    `json:"cli_version"`
	GeneratedAt     time.Time `json:"generated_at"`
	ExpiresAt       time.Time `json:"expires_at"`

	Meta        ManifestMeta `json:"meta"`
	Fingerprint Fingerprint  `json:"fingerprint"`
	Source      Source       `json:"source"`
	Identity    Identity     `json:"identity"`

	Capabilities Capabilities `json:"capabilities"`

	Collections []*Collection `json:"collections"`
	Globals     []*Global     `json:"globals"`

	Unreachable             []Unreachable         `json:"unreachable"`
	Limitations             []Limitation          `json:"limitations"`
	UnsupportedOperators    []UnsupportedOperator `json:"unsupported_operators"`
	LearnedOperatorFailures []LearnedFailure      `json:"learned_operator_failures"`

	Diagnostics Diagnostics `json:"diagnostics"`
}

// ManifestMeta binds the manifest to the cache scope that produced it (§8.1).
// It carries the sorted header *names* only — a [profiles.X.headers] value is
// never written to any artefact (§7.8.3).
type ManifestMeta struct {
	Scope          string    `json:"scope"`
	Profiles       []string  `json:"profiles"`
	BaseURL        string    `json:"base_url"`
	APIPath        string    `json:"api_path"`
	GraphQLPath    string    `json:"graphql_path"`
	HeaderNames    []string  `json:"header_names"`
	KeyFingerprint string    `json:"key_fingerprint"`
	CreatedAt      time.Time `json:"created_at"`
	ConfirmedAt    time.Time `json:"confirmed_at"`
}

// Fingerprint carries §8.4's two invalidation levels. Level 1 is
// identity-scoped and cheap; Level 2 is identity-independent and is the only
// permission-free schema-version proxy the API offers.
type Fingerprint struct {
	TopologySHA256 string    `json:"topology_sha256"`
	SchemaSHA256   string    `json:"schema_sha256"`
	CheckedAt      time.Time `json:"checked_at"`
	ServerIdentity string    `json:"server_identity"`
}

// Source records how PayCLI reached the server and what it could learn about
// the project itself. payload_version and db_adapter are not obtainable from
// the API at all (§7.11), so both are nullable with provenance.
type Source struct {
	BaseURL              string  `json:"base_url"`
	APIPath              string  `json:"api_path"`
	APIPathSource        string  `json:"api_path_source"`
	GraphQLPath          string  `json:"graphql_path"`
	GraphQLPathSource    string  `json:"graphql_path_source"`
	PoweredBy            string  `json:"powered_by"`
	PayloadVersion       *string `json:"payload_version"`
	PayloadVersionSource string  `json:"payload_version_source"`
	DBAdapter            string  `json:"db_adapter"`
	DBAdapterSource      string  `json:"db_adapter_source"`
}

// Identity is Stage 0's verified answer to "who am I". The /me response body
// itself is never written to disk (§7.8.3) — it carries the API key in
// plaintext on a project with useAPIKey.
type Identity struct {
	AuthMode             string  `json:"auth_mode"`
	AuthCollection       *string `json:"auth_collection"`
	AuthCollectionSource string  `json:"auth_collection_source"`
	UserID               any     `json:"user_id"`
	Verified             bool    `json:"verified"`
	CanAccessAdmin       bool    `json:"can_access_admin"`
	Strategy             string  `json:"strategy"`
	KeyFingerprint       string  `json:"key_fingerprint"`
}

// Capabilities are the project-level facts.
type Capabilities struct {
	GraphQL         GraphQLCapability `json:"graphql"`
	Localization    Localization      `json:"localization"`
	Reorder         *bool             `json:"reorder"`
	OG              *bool             `json:"og"`
	MethodOverride  *bool             `json:"method_override"`
	Jobs            Jobs              `json:"jobs"`
	Preferences     Preferences       `json:"preferences"`
	CustomEndpoints []string          `json:"custom_endpoints"`
}

// GraphQLCapability is §7.6's ladder result. Introspection is tri-state: on a
// route_missing or unreachable endpoint PayCLI learned nothing about it.
type GraphQLCapability struct {
	Mode          string `json:"mode"`
	Introspection *bool  `json:"introspection"`
	Path          string `json:"path"`
	Detail        string `json:"detail,omitempty"`
	Hint          string `json:"hint,omitempty"`
}

// Localization models §7.9. `enabled` is tri-state because REST cannot tell a
// non-localised project from a localised one — `locale` is accepted and
// ignored when localisation is off.
type Localization struct {
	Enabled *bool `json:"enabled"`
	// EnabledSource is the story behind `enabled`, which §7.8.3 requires of
	// every derived fact: "graphql-arg" / "graphql-arg-absent" when the plural
	// Query field settled it, "locale-all" when a ?locale=all sample proved it
	// positively, "configured" when the profile pinned locale codes, and
	// "unknown" while it is still nil.
	EnabledSource string   `json:"enabled_source"`
	Locales       []string `json:"locales"`
	// Default is Payload's localization.defaultLocale. No API exposes it, so
	// it is null unless something actually established it — it is NEVER the
	// first entry of a sorted locale list (§21.3 rule 1: a defaulted value is
	// not a discovered fact).
	Default         *string `json:"default"`
	DefaultSource   string  `json:"default_source"`
	LocalesSource   string  `json:"locales_source"`
	FallbackDefault *string `json:"fallback_default"`
	LocaleCount     *int    `json:"locale_count"`
}

// Jobs and Preferences name Payload's internal collections when this project
// has them, so a command never has to hardcode the slug.
type Jobs struct {
	Collection  *string `json:"collection"`
	StatsGlobal *string `json:"stats_global"`
}

// Preferences names the preferences collection when present.
type Preferences struct {
	Collection *string `json:"collection"`
}

// Labels are §7.5 probe 8's harvest. They are never blank, never null and
// never half-parsed: a failed parse falls back to the title-cased slug with
// source "derived".
type Labels struct {
	Singular string `json:"singular"`
	Plural   string `json:"plural"`
	Source   string `json:"source"`
}

// GraphQLNames is the slug <-> GraphQL type mapping recovered by §7.3's
// index-for-index zip of the Access type against Query.docAccess*. It is nil
// when no GraphQL name could be established, and the entity then runs
// REST-only.
type GraphQLNames struct {
	Singular string `json:"singular"`
	Plural   string `json:"plural,omitempty"`
	Count    string `json:"count,omitempty"`
	Source   string `json:"source"`
}

// Flags are §7.6's tri-state capability flags. nil means "never learned" and
// must never block an operation locally — the operation is attempted and the
// server's answer is classified after the fact.
type Flags struct {
	Upload            *bool `json:"upload"`
	Auth              *bool `json:"auth"`
	UseAPIKey         *bool `json:"use_api_key"`
	Versions          *bool `json:"versions"`
	Drafts            *bool `json:"drafts"`
	Trash             *bool `json:"trash"`
	Folders           *bool `json:"folders"`
	Duplicate         *bool `json:"duplicate"`
	Orderable         *bool `json:"orderable"`
	EndpointsDisabled *bool `json:"endpoints_disabled"`
}

// FlagsSource carries one provenance string per flag.
type FlagsSource struct {
	Upload            string `json:"upload"`
	Auth              string `json:"auth"`
	UseAPIKey         string `json:"use_api_key"`
	Versions          string `json:"versions"`
	Drafts            string `json:"drafts"`
	Trash             string `json:"trash"`
	Folders           string `json:"folders"`
	Duplicate         string `json:"duplicate"`
	Orderable         string `json:"orderable"`
	EndpointsDisabled string `json:"endpoints_disabled"`
}

// Permissions is the /api/access entry for this identity. field_level is
// tri-state: `fields` is the boolean true for a privileged key and an object
// for a restricted one, and it is NEVER used as a field-name source
// (§1 conflict 12).
type Permissions struct {
	Create       bool  `json:"create"`
	Read         bool  `json:"read"`
	Update       bool  `json:"update"`
	Delete       bool  `json:"delete"`
	ReadVersions bool  `json:"read_versions"`
	Unlock       bool  `json:"unlock"`
	FieldLevel   *bool `json:"field_level"`
}

// Stats is the optional document census. total_docs is nil unless the caller
// asked for it (--deep): one count request per collection is 49 requests on
// the live project and §7.1's cold budget is four.
type Stats struct {
	TotalDocs *int `json:"total_docs"`
	Sampled   bool `json:"sampled"`
}

// Collection is one index entry.
type Collection struct {
	Slug         string        `json:"slug"`
	Labels       Labels        `json:"labels"`
	GraphQL      *GraphQLNames `json:"graphql"`
	IDType       string        `json:"id_type"`
	IDTypeSource string        `json:"id_type_source"`
	Reachability string        `json:"reachability"`
	Internal     bool          `json:"internal"`

	Publishable       bool    `json:"publishable"`
	PublishableReason *string `json:"publishable_reason"`

	Flags       Flags       `json:"flags"`
	FlagsSource FlagsSource `json:"flags_source"`
	Permissions Permissions `json:"permissions"`

	Stats Stats `json:"stats"`

	FieldsCount  int    `json:"fields_count"`
	FieldsSHA256 string `json:"fields_sha256"`
	FieldsShard  string `json:"fields_shard"`
	FieldsSource string `json:"fields_source"`

	TitleField *string  `json:"title_field"`
	DateFields []string `json:"date_fields"`
	Warnings   []string `json:"warnings"`
}

// Global is one global index entry. A global has no id, no bulk verbs and no
// trash, so the collection keys that cannot apply are absent rather than
// written as a misleading false.
type Global struct {
	Slug         string        `json:"slug"`
	Labels       Labels        `json:"labels"`
	GraphQL      *GraphQLNames `json:"graphql"`
	Reachability string        `json:"reachability"`
	Internal     bool          `json:"internal"`

	Flags       GlobalFlags       `json:"flags"`
	FlagsSource GlobalFlagsSource `json:"flags_source"`
	Permissions GlobalPermissions `json:"permissions"`

	FieldsCount  int    `json:"fields_count"`
	FieldsSHA256 string `json:"fields_sha256"`
	FieldsShard  string `json:"fields_shard"`
	FieldsSource string `json:"fields_source"`

	Warnings []string `json:"warnings"`
}

// GlobalFlags is the reduced flag set a global can have.
type GlobalFlags struct {
	Versions *bool `json:"versions"`
	Drafts   *bool `json:"drafts"`
}

// GlobalFlagsSource carries provenance for GlobalFlags.
type GlobalFlagsSource struct {
	Versions string `json:"versions"`
	Drafts   string `json:"drafts"`
}

// GlobalPermissions is the /api/access globals entry.
type GlobalPermissions struct {
	Read         bool  `json:"read"`
	Update       bool  `json:"update"`
	ReadVersions bool  `json:"read_versions"`
	FieldLevel   *bool `json:"field_level"`
}

// Unreachable records an entity PayCLI knows about but cannot fully use, with
// the evidence for why.
type Unreachable struct {
	Slug   string `json:"slug"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

// Limitation is a declared hole. Holes are declared, never guessed.
type Limitation struct {
	Code       string `json:"code"`
	Entity     string `json:"entity,omitempty"`
	Field      string `json:"field,omitempty"`
	Detail     string `json:"detail,omitempty"`
	Mitigation string `json:"mitigation"`
}

// UnsupportedOperator is a pre-emptive client-side block. It is only ever
// populated when db_adapter_source == "configured" (§7.11): on MongoDB `all`
// maps to $all and works, so a guessed list would make PayCLI refuse a query
// the server would have answered.
type UnsupportedOperator struct {
	Operator   string `json:"operator"`
	Collection string `json:"collection,omitempty"`
	Reason     string `json:"reason"`
}

// LearnedFailure is §7.11's reactive record: an operator that provably failed
// on a collection, so the next use warns before sending.
type LearnedFailure struct {
	Operator   string    `json:"operator"`
	Collection string    `json:"collection"`
	Evidence   string    `json:"evidence"`
	LearnedAt  time.Time `json:"learned_at"`
}

// MaxLearnedOperatorFailures bounds the memo. It is a cache file an agent may
// never garbage-collect by hand, and a project with more than this many
// distinct failing operator/collection pairs has a bigger problem than the
// memo can express; the oldest record is dropped first.
const MaxLearnedOperatorFailures = 64

// operatorFailureCandidates is §7.11's attribution filter.
//
// §7.11 records an operator failure only when the failure is "provably caused
// by operator O". Provability has to mean something narrower than "a 500 came
// back from a request that contained an operator", or the first flaky 502 on a
// `--where 'slug eq home'` would teach PayCLI that `equals` is broken and warn
// on every query for the rest of the cache's life. So two conditions must hold:
// the request used EXACTLY ONE distinct operator (with two, neither is proven),
// and that operator is one whose adapter support actually varies.
//
// This is the same four-operator set that query.CheckOperator blocks
// pre-emptively when db_adapter_source == "configured"; the two are asserted
// to agree by TestOperatorFailureCandidatesTrackTheAdapterGatedSet. The lists
// serve different rules — that one BLOCKS on a pinned adapter, this one LEARNS
// on an unpinned one — which is why §7.11 keeps both.
var operatorFailureCandidates = map[string]bool{
	"all":        true,
	"near":       true,
	"within":     true,
	"intersects": true,
}

// AttributeOperatorFailure implements the test described above. It returns the
// operator to blame and true only when the failure is genuinely attributable.
//
// serverFailed is the caller's verdict that the server, not PayCLI, refused the
// request: an HTTP 500 or a GraphQL QueryError. operators is every distinct
// operator the request carried.
func AttributeOperatorFailure(serverFailed bool, operators []string) (string, bool) {
	if !serverFailed {
		return "", false
	}
	seen := map[string]bool{}
	var distinct []string
	for _, op := range operators {
		op = strings.TrimSpace(op)
		if op == "" || seen[op] {
			continue
		}
		seen[op] = true
		distinct = append(distinct, op)
	}
	if len(distinct) != 1 || !operatorFailureCandidates[distinct[0]] {
		return "", false
	}
	return distinct[0], true
}

// OperatorFailureCandidate reports whether an operator is one §7.11 will ever
// learn about. Callers use it to skip the memo lookup for the ninety per cent
// of queries that only use equals/greater_than/contains.
func OperatorFailureCandidate(operator string) bool {
	return operatorFailureCandidates[strings.TrimSpace(operator)]
}

// RecordOperatorFailure appends §7.11's reactive memo, or refreshes the record
// that is already there. It reports whether the manifest changed, so a caller
// can skip an expensive cache rewrite when it did not.
//
// Records are keyed by (operator, collection): re-running the same failing
// query updates the evidence and the timestamp instead of growing the file
// without bound.
func (m *Manifest) RecordOperatorFailure(operator, collection, evidence string, at time.Time) bool {
	if m == nil {
		return false
	}
	operator = strings.TrimSpace(operator)
	collection = strings.TrimSpace(collection)
	if operator == "" || collection == "" {
		return false
	}
	at = at.UTC().Truncate(time.Second)
	for i := range m.LearnedOperatorFailures {
		f := &m.LearnedOperatorFailures[i]
		if f.Operator != operator || f.Collection != collection {
			continue
		}
		if f.Evidence == evidence && !f.LearnedAt.Before(at) {
			return false
		}
		f.Evidence = evidence
		f.LearnedAt = at
		return true
	}
	m.LearnedOperatorFailures = append(m.LearnedOperatorFailures, LearnedFailure{
		Operator:   operator,
		Collection: collection,
		Evidence:   evidence,
		LearnedAt:  at,
	})
	sort.SliceStable(m.LearnedOperatorFailures, func(i, j int) bool {
		a, b := m.LearnedOperatorFailures[i], m.LearnedOperatorFailures[j]
		if a.Collection != b.Collection {
			return a.Collection < b.Collection
		}
		return a.Operator < b.Operator
	})
	if n := len(m.LearnedOperatorFailures); n > MaxLearnedOperatorFailures {
		oldest := 0
		for i := 1; i < n; i++ {
			if m.LearnedOperatorFailures[i].LearnedAt.Before(m.LearnedOperatorFailures[oldest].LearnedAt) {
				oldest = i
			}
		}
		m.LearnedOperatorFailures = append(
			m.LearnedOperatorFailures[:oldest], m.LearnedOperatorFailures[oldest+1:]...)
	}
	return true
}

// LearnedOperatorFailure looks the memo up for one operator on one collection.
// It is deliberately NOT project-wide: `all` failing on crm-contacts says
// nothing about `all` on pages, and warning about the second because of the
// first would be the same "assume, do not learn" mistake §1 conflict 36 exists
// to correct.
func (m *Manifest) LearnedOperatorFailure(operator, collection string) (LearnedFailure, bool) {
	if m == nil {
		return LearnedFailure{}, false
	}
	operator = strings.TrimSpace(operator)
	collection = strings.TrimSpace(collection)
	for _, f := range m.LearnedOperatorFailures {
		if f.Operator == operator && f.Collection == collection {
			return f, true
		}
	}
	return LearnedFailure{}, false
}

// Diagnostics is the cost and degradation record for one discovery run.
type Diagnostics struct {
	Requests          int      `json:"requests"`
	ElapsedMS         int64    `json:"elapsed_ms"`
	Bytes             int64    `json:"bytes"`
	GraphQLBatches    int      `json:"graphql_batches"`
	Degraded          []string `json:"degraded"`
	Warnings          []string `json:"warnings"`
	CreatedAndDeleted []string `json:"created_and_deleted"`
	AuthCandidates    []string `json:"auth_candidates"`
	BlockSlugFiles    []string `json:"block_slug_files"`
}

// NewManifest returns a manifest with every slice non-nil, so the JSON form
// never has a null where an agent expects an array.
func NewManifest() *Manifest {
	return &Manifest{
		ManifestVersion:         ManifestVersion,
		Collections:             []*Collection{},
		Globals:                 []*Global{},
		Unreachable:             []Unreachable{},
		Limitations:             []Limitation{},
		UnsupportedOperators:    []UnsupportedOperator{},
		LearnedOperatorFailures: []LearnedFailure{},
		Capabilities: Capabilities{
			GraphQL: GraphQLCapability{Mode: GraphQLModeUnreachable},
			Localization: Localization{Locales: []string{}, LocalesSource: SourceUnknown,
				EnabledSource: SourceUnknown, DefaultSource: SourceUnknown},
			CustomEndpoints: []string{},
		},
		Diagnostics: Diagnostics{
			Degraded:          []string{},
			Warnings:          []string{},
			CreatedAndDeleted: []string{},
			AuthCandidates:    []string{},
			BlockSlugFiles:    []string{},
		},
	}
}

// Collection returns the index entry for a slug.
func (m *Manifest) Collection(slug string) (*Collection, bool) {
	if m == nil {
		return nil, false
	}
	for _, c := range m.Collections {
		if c.Slug == slug {
			return c, true
		}
	}
	return nil, false
}

// Global returns the index entry for a global slug.
func (m *Manifest) Global(slug string) (*Global, bool) {
	if m == nil {
		return nil, false
	}
	for _, g := range m.Globals {
		if g.Slug == slug {
			return g, true
		}
	}
	return nil, false
}

// CollectionSlugs returns every collection slug in index order (key-sorted).
func (m *Manifest) CollectionSlugs() []string {
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(m.Collections))
	for _, c := range m.Collections {
		out = append(out, c.Slug)
	}
	return out
}

// GlobalSlugs returns every global slug in index order.
func (m *Manifest) GlobalSlugs() []string {
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(m.Globals))
	for _, g := range m.Globals {
		out = append(out, g.Slug)
	}
	return out
}

// AddLimitation appends a limitation with its §7.6 mitigation, de-duplicating
// on (code, entity, field) so a per-collection hole is recorded once.
func (m *Manifest) AddLimitation(code, entity, field, detail string) {
	for _, l := range m.Limitations {
		if l.Code == code && l.Entity == entity && l.Field == field {
			return
		}
	}
	m.Limitations = append(m.Limitations, Limitation{
		Code: code, Entity: entity, Field: field, Detail: detail,
		Mitigation: Mitigation(code),
	})
}

// AddUnreachable appends an unreachable entry, de-duplicating on slug+kind.
func (m *Manifest) AddUnreachable(slug, kind, reason, detail string) {
	for _, u := range m.Unreachable {
		if u.Slug == slug && u.Kind == kind {
			return
		}
	}
	m.Unreachable = append(m.Unreachable, Unreachable{Slug: slug, Kind: kind, Reason: reason, Detail: detail})
}

// Degrade records that a stage fell back to REST-only discovery.
func (m *Manifest) Degrade(stage string) {
	for _, s := range m.Diagnostics.Degraded {
		if s == stage {
			return
		}
	}
	m.Diagnostics.Degraded = append(m.Diagnostics.Degraded, stage)
}

// Sort orders the index deterministically: collections and globals by slug,
// and every declared hole by (code, entity, field). Two discovery runs against
// an unchanged server must produce byte-identical manifests apart from the
// timestamps, or the §8.4 fingerprints would flap.
func (m *Manifest) Sort() {
	sort.Slice(m.Collections, func(i, j int) bool { return m.Collections[i].Slug < m.Collections[j].Slug })
	sort.Slice(m.Globals, func(i, j int) bool { return m.Globals[i].Slug < m.Globals[j].Slug })
	sort.Slice(m.Unreachable, func(i, j int) bool {
		if m.Unreachable[i].Slug != m.Unreachable[j].Slug {
			return m.Unreachable[i].Slug < m.Unreachable[j].Slug
		}
		return m.Unreachable[i].Kind < m.Unreachable[j].Kind
	})
	sort.SliceStable(m.Limitations, func(i, j int) bool {
		a, b := m.Limitations[i], m.Limitations[j]
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		if a.Entity != b.Entity {
			return a.Entity < b.Entity
		}
		return a.Field < b.Field
	})
	sort.Strings(m.Diagnostics.Degraded)
	sort.Strings(m.Diagnostics.Warnings)
	sort.Strings(m.Diagnostics.BlockSlugFiles)
}

// Revision is a short, stable identifier for the manifest's content, used as
// the discovery revision stamped into generated skill docs (§14) and into
// meta.discovery_revision.
func (m *Manifest) Revision() string {
	if m == nil {
		return ""
	}
	if m.Fingerprint.TopologySHA256 == "" && m.Fingerprint.SchemaSHA256 == "" {
		return m.Generation
	}
	t := m.Fingerprint.TopologySHA256
	s := m.Fingerprint.SchemaSHA256
	return firstN(t, 8) + "-" + firstN(s, 8)
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// IsInternal reports Payload's own collections, which §2's ground truth says
// to de-emphasise in default listings but keep reachable.
func IsInternal(slug string) bool { return strings.HasPrefix(slug, "payload-") }

// DecodeManifest parses a manifest index. A manifest_version other than the
// current one is reported as a miss by returning ok == false rather than an
// error, because §8.3 requires every cache read failure to be a miss.
func DecodeManifest(b []byte) (*Manifest, bool) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, false
	}
	if m.ManifestVersion != ManifestVersion {
		return nil, false
	}
	// A manifest written by a CLI that predates a provenance field decodes it
	// as "", and an empty provenance is itself a §7.8.3 violation: the fact
	// would be shipped to an agent without its story. Backfill the honest
	// answer rather than wait for the TTL.
	if m.Capabilities.Localization.EnabledSource == "" {
		m.Capabilities.Localization.EnabledSource = SourceUnknown
	}
	if m.Capabilities.Localization.DefaultSource == "" {
		m.Capabilities.Localization.DefaultSource = SourceUnknown
	}
	return &m, true
}
