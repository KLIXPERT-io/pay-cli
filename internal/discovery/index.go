package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/logging"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
)

// DefaultTTL is the discovery manifest's fresh window (§8.5).
const DefaultTTL = 10 * time.Minute

// sampleLimit is how many documents the REST-only field fallback and §7.9b's
// locale=all analysis read. Three is what §7.9b specifies; it is also enough
// for the key-set union a field list needs.
const sampleLimit = 3

// Options configures one discovery run.
//
// Everything here is a plain value rather than a config.* type on purpose:
// internal/discovery must not depend on the configuration layer's shape, and
// every project fact it needs (block slugs, the payload version, the db
// adapter) is produced by a local filesystem scan the caller already ran.
type Options struct {
	Client *payload.Client

	BaseURL string

	APIPath       string
	APIPathSource string
	// GraphQLRoute is routes.graphQL, defaulting to /graphql. graphql_path is
	// DERIVED from api_path + this (§1 conflict 34), never an independent
	// literal.
	GraphQLRoute      string
	GraphQLPath       string
	GraphQLPathSource string

	AuthMode       string
	AuthCollection string
	// AuthCollectionSource is "configured" when the slug was pinned.
	AuthCollectionSource string
	// CachedAuthCollection is a slug already resolved for this
	// (normURL, keyFP) pair by a previous run (§7.0e). When set, Stage -1 is
	// skipped entirely.
	CachedAuthCollection string
	// CredentialAbsent reports anonymous operation. Stage 0's /me request is
	// then not made at all and discovery continues.
	CredentialAbsent bool
	KeyFingerprint   string

	Concurrency int

	Profile     string
	Profiles    []string
	ScopeKey    string
	HeaderNames []string

	CLIVersion string
	Generation string
	Now        func() time.Time
	TTL        time.Duration

	// Deep widens Stage 3 to every collection and adds the document census.
	Deep bool
	// AllowWriteProbes enables §7.7's empty-POST harvest. It creates a real
	// document in any collection with no required fields, so it is off by
	// default and the flag name is deliberately unpleasant.
	AllowWriteProbes bool
	// NoLabels skips §7.5 probe 8, which is the only DELETE verb discovery
	// ever issues.
	NoLabels bool
	// NoProbes disables Stage 3 entirely.
	NoProbes bool

	ConfiguredIDType  string
	ConfiguredLocales []string
	ConfiguredBlocks  map[string][]string
	CustomEndpoints   []string

	PayloadVersion       string
	PayloadVersionSource string
	DBAdapter            string
	DBAdapterSource      string

	// ProjectBlockSlugs are §7.10's `slug:` literals found on the local
	// filesystem, and ProjectBlockSlugFiles the absolute files they came from.
	ProjectBlockSlugs     []string
	ProjectBlockSlugFiles []string
	// ProjectBlockInterfaces maps an `interfaceName:` literal found on the
	// local filesystem to the `slug:` declared beside it in the same object
	// (CallToActionBlock -> cta). It is the other half of §7.10: a blocks
	// field's GraphQL union publishes interfaceNames, and only this map turns
	// one into a blockType the REST API will accept. Empty is survivable —
	// every union member then resolves through the interfaceName heuristic and
	// is labelled as inferred — but it is never guessed at silently.
	ProjectBlockInterfaces map[string]string
	// ProjectAuthSlugs are auth-collection slug literals from the project's
	// own payload.config.ts, used only to widen Stage -1's candidate set.
	ProjectAuthSlugs []string
	// ProjectRoutes are `routes:` literals, named in the endpoint_not_payload
	// hint when nothing accepted.
	ProjectRoutes []string

	Logger *slog.Logger
}

// Result is one discovery run's output. Nothing here has been written to disk:
// persistence is internal/cache's job, and keeping it there means a failed
// write is a warning rather than a corrupt manifest.
type Result struct {
	Manifest *Manifest
	// Shards are keyed by the cache-relative shard path, e.g.
	// "fields/pages.json", so a caller can hand them straight to cache.Set.
	Shards map[string]*Shard

	AuthCollection       string
	AuthCollectionSource string
	// AuthResolved reports that Stage -1 ran and produced a slug worth
	// persisting to auth-resolution.json.
	AuthResolved bool

	APIPath           string
	APIPathSource     string
	GraphQLPath       string
	GraphQLPathSource string

	Warnings []output.Warning
}

// Discoverer runs §7's pipeline.
type Discoverer struct {
	opt    Options
	client *payload.Client
	log    *slog.Logger
	now    func() time.Time

	baseURL           string
	apiPath           string
	apiPathSource     string
	graphQLPath       string
	graphQLPathSource string
	graphQLRoute      string

	authMode         string
	authCollection   string
	credentialAbsent bool

	concurrency int

	projectAuthSlugs []string
	projectRoutes    []string

	// mu guards the counters and the warning list, which the Stage -1 /init
	// sweep, the Stage 3 worker pool and the project probes all touch from
	// worker goroutines.
	mu             sync.Mutex
	requests       int
	bytes          int64
	graphQLBatches int

	stage1Done bool
	stage1Res  *gqlResult
	stage1Err  error
	snapshot   *schemaSnapshot

	zipFallback bool
	// graphQLUsable gates §7.3's reachability split. When GraphQL is
	// unavailable for the whole project, "absent from GraphQL" says nothing
	// about an entity, so every entity /api/access saw is simply reachable.
	graphQLUsable bool

	// localeAnalysis and localeSampled carry §7.9d's per-field evidence, which
	// is the only thing that can turn fields[].localized from null into a
	// boolean.
	localeAnalysis map[string]LocaleAnalysis
	localeSampled  map[string]bool

	warnings []output.Warning
}

// New validates the options and returns a Discoverer.
func New(opt Options) (*Discoverer, error) {
	if opt.Client == nil {
		return nil, apierr.New(apierr.CodeInternal, "discovery requires a Payload client")
	}
	if opt.Now == nil {
		return nil, apierr.New(apierr.CodeInternal, "discovery requires an injected clock")
	}
	concurrency := opt.Concurrency
	if concurrency < 1 {
		concurrency = payload.DefaultConcurrency
	}
	if concurrency > payload.MaxConcurrency {
		concurrency = payload.MaxConcurrency
	}
	log := opt.Logger
	if log == nil {
		log = logging.Discard()
	}
	apiPath := normalizeAPIPath(opt.APIPath)
	apiSource := opt.APIPathSource
	if apiSource == "" {
		apiSource = SourceProbed
	}
	d := &Discoverer{
		opt:               opt,
		client:            opt.Client,
		log:               log,
		now:               opt.Now,
		baseURL:           opt.BaseURL,
		apiPath:           apiPath,
		apiPathSource:     apiSource,
		graphQLPath:       opt.GraphQLPath,
		graphQLPathSource: opt.GraphQLPathSource,
		graphQLRoute:      opt.GraphQLRoute,
		authMode:          opt.AuthMode,
		authCollection:    resolveConfiguredAuth(opt),
		credentialAbsent:  opt.CredentialAbsent,
		concurrency:       concurrency,
		projectAuthSlugs:  opt.ProjectAuthSlugs,
		projectRoutes:     opt.ProjectRoutes,
		localeAnalysis:    map[string]LocaleAnalysis{},
		localeSampled:     map[string]bool{},
	}
	if d.baseURL == "" {
		d.baseURL = opt.Client.BaseURL()
	}
	d.recomputeGraphQLPath()
	return d, nil
}

// resolveConfiguredAuth applies §7.0's skip conditions: an explicitly set slug
// or one already in auth-resolution.json means Stage -1 does not run.
func resolveConfiguredAuth(opt Options) string {
	if opt.AuthCollection != "" && opt.AuthCollection != "auto" {
		return opt.AuthCollection
	}
	if opt.CachedAuthCollection != "" {
		return opt.CachedAuthCollection
	}
	return ""
}

func (d *Discoverer) countRequest(resp *payload.Response) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requests++
	if resp != nil {
		d.bytes += resp.Bytes
	}
}

func (d *Discoverer) counters() (requests int, bytes int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.requests, d.bytes
}

func (d *Discoverer) warn(w output.Warning) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, existing := range d.warnings {
		if existing.Code == w.Code && existing.Message == w.Message {
			return
		}
	}
	d.warnings = append(d.warnings, w)
}

// Run executes §7's pipeline and returns the manifest plus one field shard per
// entity.
//
// The one hard failure is §7.6's: discovery_failed (exit 10) is reserved for
// the case where REST-only discovery also fails, i.e. GET {api_path}/access
// itself did not return parseable JSON. A working project must never be made
// unusable by a GraphQL-layer refusal.
func (d *Discoverer) Run(ctx context.Context) (*Result, error) {
	start := d.now()
	m := NewManifest()
	m.CLIVersion = d.opt.CLIVersion
	m.Generation = d.opt.Generation
	if m.Generation == "" {
		m.Generation = cache.NewGeneration(start)
	}

	authSource := d.opt.AuthCollectionSource
	authResolved := false
	var bootstrap *AuthBootstrap

	// Stage -1 (§7.0).
	if d.needsBootstrap() {
		bs, err := d.runAuthBootstrap(ctx)
		if err != nil {
			return nil, err
		}
		bootstrap = bs
		d.authCollection = bs.Collection
		authSource = bs.Source
		authResolved = bs.Collection != ""
		m.Diagnostics.AuthCandidates = bs.Candidates
		if bs.Truncated {
			m.AddLimitation(LimAuthCandidatesTruncated, "", "",
				"Stage -1 could only see the anonymous slug list")
		}
	} else if d.authCollection != "" && authSource == "" {
		authSource = SourceConfigured
		if d.opt.CachedAuthCollection != "" && d.opt.AuthCollection == "" {
			authSource = SourceCached
		}
	}

	// Stage 0 (§7.2).
	view, identity, err := d.stage0(ctx)
	if err != nil {
		return nil, err
	}

	// Stage 1 (§7.3) — also §7.6's mode classification and §8.4's Level-2
	// fingerprint.
	snap, entities, mode := d.stage1(ctx, append(view.Slugs(), view.GlobalSlugs()...))
	d.snapshot = snap
	m.Capabilities.GraphQL = GraphQLCapability{
		Mode: mode.Mode, Introspection: mode.Introspection,
		Path: d.graphQLPath, Detail: mode.Detail, Hint: mode.Hint,
	}
	if mode.Mode != GraphQLModeOK {
		m.Degrade(Stage1)
		entities = map[string]*entity{}
	}
	if d.zipFallback {
		m.Degrade(Stage1)
	}

	// Stage 2 (§7.4).
	var schema *Schema
	stage2Complete := false
	if mode.Mode == GraphQLModeOK && len(entities) > 0 {
		schema, stage2Complete = d.stage2(ctx, entities)
		if !stage2Complete {
			m.Degrade(Stage2)
		}
	}
	if schema == nil {
		schema = &Schema{Types: map[string]*IntroType{}, SlugBySingular: map[string]string{}}
	}

	rows := d.unite(view, entities, mode.Mode)
	d.graphQLUsable = mode.Mode == GraphQLModeOK

	// Stage 3 (§7.5) — only for what GraphQL did not already establish, plus
	// everything when --deep. That is what keeps §7.1's cold budget while
	// still measuring every capability on a project with GraphQL disabled.
	needs := map[string]probeNeed{}
	for slug, row := range rows {
		if row.kind != kindCollection {
			continue
		}
		needs[slug] = d.probeNeedFor(row, schema)
	}
	probes := map[string]probeResult{}
	if !d.opt.NoProbes {
		probes = d.runProbes(ctx, needs)
	}
	project := d.runProjectProbes(ctx)

	// §7.9 locale enumeration.
	localization := d.resolveLocalization(ctx, rows, schema, entities, m)

	// Assemble.
	shards := map[string]*Shard{}
	blockSources := BlockSources{
		Configured:         d.opt.ConfiguredBlocks,
		ProjectSource:      d.opt.ProjectBlockSlugs,
		ProjectSourceFiles: d.opt.ProjectBlockSlugFiles,
		SlugByInterface:    d.opt.ProjectBlockInterfaces,
	}
	idTypes := []string{}
	sampleIDs := []string{}

	for _, slug := range sortedRowKeys(rows) {
		row := rows[slug]
		probe := probes[slug]
		switch row.kind {
		case kindCollection:
			coll, shard := d.buildCollection(ctx, m, row, schema, probe, blockSources, localization)
			m.Collections = append(m.Collections, coll)
			shards[cache.ShardName(slug, cache.KindCollection)] = shard
			idTypes = append(idTypes, coll.IDType)
			if s := IDString(probe.SampleID); s != "" {
				sampleIDs = append(sampleIDs, s)
			}
		case kindGlobal:
			g, shard := d.buildGlobal(m, row, schema, blockSources, localization)
			m.Globals = append(m.Globals, g)
			shards[cache.ShardName(slug, cache.KindGlobal)] = shard
		}
	}

	d.fillManifest(m, view, identity, authSource, mode, project, localization, start)
	d.fillAdapter(m, idTypes, sampleIDs)
	if bootstrap != nil {
		m.Diagnostics.AuthCandidates = bootstrap.Candidates
	}
	m.Diagnostics.BlockSlugFiles = d.opt.ProjectBlockSlugFiles
	m.Sort()

	ttl := d.opt.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	m.GeneratedAt = start
	m.ExpiresAt = start.Add(ttl)
	m.Diagnostics.Requests, m.Diagnostics.Bytes = d.counters()
	m.Diagnostics.GraphQLBatches = d.graphQLBatches
	m.Diagnostics.ElapsedMS = d.now().Sub(start).Milliseconds()
	for _, w := range d.warnings {
		m.Diagnostics.Warnings = append(m.Diagnostics.Warnings, w.Code)
	}
	sort.Strings(m.Diagnostics.Warnings)

	return &Result{
		Manifest:             m,
		Shards:               shards,
		AuthCollection:       d.authCollection,
		AuthCollectionSource: authSource,
		AuthResolved:         authResolved,
		APIPath:              d.apiPath,
		APIPathSource:        d.apiPathSource,
		GraphQLPath:          d.graphQLPath,
		GraphQLPathSource:    d.graphQLPathSource,
		Warnings:             d.warnings,
	}, nil
}

// needsBootstrap applies §7.0's skip list.
func (d *Discoverer) needsBootstrap() bool {
	if d.authCollection != "" {
		return false
	}
	if d.authMode == payload.AuthModeAnonymous || d.credentialAbsent {
		return false
	}
	return true
}

// row is one entity of the united inventory.
type row struct {
	slug      string
	kind      string
	entity    *entity
	inAccess  bool
	inGraphQL bool
	perms     map[string]any
}

// unite implements §7.3's union. Neither source alone is complete, which is
// verified in both directions: payload-migrations is REST-only (endpoints
// disabled removes it from the GraphQL schema) and payload-kv is GraphQL-only
// (this identity has zero permissions on it, so /api/access omits it).
func (d *Discoverer) unite(view *accessView, entities map[string]*entity, mode string) map[string]*row {
	rows := map[string]*row{}

	for slug, v := range view.Collections {
		perms, _ := v.(map[string]any)
		rows[slug] = &row{slug: slug, kind: kindCollection, inAccess: true, perms: perms}
	}
	for slug, v := range view.Globals {
		perms, _ := v.(map[string]any)
		rows[slug] = &row{slug: slug, kind: kindGlobal, inAccess: true, perms: perms}
	}

	for slug, e := range entities {
		if e.Singular == "" {
			continue
		}
		r, ok := rows[slug]
		if !ok {
			kind := e.Kind
			if kind == "" {
				kind = kindCollection
			}
			r = &row{slug: slug, kind: kind}
			rows[slug] = r
		}
		r.entity = e
		r.inGraphQL = true
		// /api/access is authoritative about collection-vs-global for
		// anything it can see; GraphQL decides the rest.
		if !r.inAccess && e.Kind != "" {
			r.kind = e.Kind
		}
	}
	_ = mode
	return rows
}

func sortedRowKeys(rows map[string]*row) []string {
	out := make([]string, 0, len(rows))
	for k := range rows {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// probeNeedFor decides which of §7.5's probes still have something to teach.
func (d *Discoverer) probeNeedFor(r *row, schema *Schema) probeNeed {
	need := probeNeed{}
	obj := (*IntroType)(nil)
	if r.entity != nil && r.entity.Singular != "" {
		obj = schema.Type(r.entity.Singular)
	}
	known := obj != nil

	deep := d.opt.Deep
	need.Endpoints = deep || !known
	need.Upload = deep || !known
	need.Versions = deep || (r.entity == nil || r.entity.Versions == nil)
	need.Drafts = deep || !known
	need.Trash = deep || !known
	need.Folders = deep || !known
	need.Auth = deep || (r.entity == nil || r.entity.Auth == nil)
	need.Labels = !d.opt.NoLabels && (deep || !known)
	need.IDSample = deep || r.entity == nil || r.entity.IDTypeGraphQL == ""
	need.Count = deep
	if !known {
		// Nothing about this entity came from GraphQL, so every probe has
		// something to teach.
		need = probeNeed{
			Endpoints: true, Upload: true, Versions: true, Drafts: true,
			Trash: true, Folders: true, Auth: true, IDSample: true,
			Labels: !d.opt.NoLabels, Count: deep,
		}
	}
	return need
}

// buildCollection assembles one index entry and its field shard.
func (d *Discoverer) buildCollection(ctx context.Context, m *Manifest, r *row, schema *Schema,
	probe probeResult, blocks BlockSources, loc Localization) (*Collection, *Shard) {
	e := r.entity
	singular := ""
	if e != nil {
		singular = e.Singular
	}
	obj := schema.Type(singular)
	input := schema.Type("mutation" + singular + "Input")

	c := &Collection{
		Slug:         r.slug,
		Internal:     IsInternal(r.slug),
		Reachability: d.reachabilityFor(r),
		DateFields:   []string{},
		Warnings:     []string{},
		Publishable:  true,
		Stats:        Stats{TotalDocs: probe.TotalDocs, Sampled: probe.TotalDocs != nil},
	}
	c.Permissions = decodePermissions(r.perms)
	c.Labels = d.labelsFor(r.slug, singular, probe)
	if probe.LabelTried && !probe.LabelParsed {
		m.AddLimitation(LimLabelsUnavailable, r.slug, "", "the bulk-delete message did not match the English pattern")
	}
	if e != nil && e.Singular != "" {
		c.GraphQL = &GraphQLNames{Singular: e.Singular, Plural: e.Plural, Count: e.Count, Source: e.NameSrc}
	}

	// Flags: GraphQL first, probe second, unknown last. Never defaulted.
	drafts, trash, folders := FlagsFromObject(obj)
	c.Flags, c.FlagsSource = mergeFlags(e, probe, UploadFromObject(obj), drafts, trash, folders, UseAPIKey(input), c.Permissions)
	if anyFlagUnknown(c.Flags) {
		m.AddLimitation(LimCapabilityUnknown, r.slug, "", "one or more capability flags could not be established")
	}

	// Fields.
	var shard *Shard
	fieldsSource := SourceUnknown
	if obj != nil {
		shard = buildShard(e, schema, buildShardOptions{
			Generation:    m.Generation,
			Blocks:        blocks,
			LocaleAll:     d.localeAnalysis[r.slug],
			LocaleSampled: d.localeSampled[r.slug],
		})
		fieldsSource = SourceGraphQL
	} else {
		docs := d.sampleDocs(ctx, r.slug, sampleLimit, false)
		if len(docs) > 0 {
			// Without GraphQL there is no union to enumerate, so the sampled
			// documents are the only per-field evidence there is.
			blocks.Observed = ObservedBlockTypes(docs)
			shard = buildShardFromDocs(m.Generation, r.slug, docs, blocks)
			fieldsSource = SourceObserved
		} else {
			shard = NewShard(m.Generation, r.slug)
			shard.Finalize()
			if c.Flags.EndpointsDisabled == nil || !*c.Flags.EndpointsDisabled {
				m.AddLimitation(LimFieldsUnavailable, r.slug, "",
					"the collection has no documents to sample and no GraphQL schema")
			}
		}
	}
	// §7.10: every blocks field is resolved from ITS OWN GraphQL union, never
	// from a project-wide bag of slugs. This runs after the shard exists so
	// that the per-field answer, its provenance and the shard hash are written
	// in one place for both the GraphQL and the REST-only path.
	ResolveShardBlocks(shard, schema, blocks)
	shard.Finalize()

	c.FieldsCount = len(shard.Fields)
	c.FieldsSHA256 = shard.SHA256
	c.FieldsShard = cache.ShardName(r.slug, cache.KindCollection)
	c.FieldsSource = fieldsSource
	c.TitleField = TitleField(shard)
	c.DateFields = shard.DateFields()

	// id_type (§7.6's ladder).
	graphQLID := ""
	if e != nil {
		graphQLID = e.IDTypeGraphQL
	}
	c.IDType, c.IDTypeSource = ResolveIDType(IDLadderInput{
		GraphQL:              graphQLID,
		SampleID:             probe.SampleID,
		SampleIDPresent:      probe.SampleIDPresent,
		VersionParentID:      probe.VersionParentID,
		VersionParentPresent: probe.VersionParentPresent,
		Configured:           d.opt.ConfiguredIDType,
	})
	if c.IDType == IDTypeUnknown {
		m.AddLimitation(LimIDTypeUnknown, r.slug, "", "")
	}

	// §7.10: a required blocks field whose slugs are unresolvable makes the
	// collection unpublishable, and the reason says so in plain words. The
	// verdict is per field: one blocks field resolving says nothing about the
	// next one, which is exactly what a single entity-wide answer got wrong.
	for _, f := range shard.Fields {
		if f.PayloadType != TypeBlocks {
			continue
		}
		bf, _ := shard.BlockFieldFor(f.Path)
		switch {
		case len(bf.Slugs) == 0:
			code := LimBlockSlugsUnknownNoSource
			if len(blocks.ProjectSource) > 0 || len(blocks.Configured) > 0 || len(bf.InterfaceNames) > 0 {
				code = LimBlockSlugsUnknown
			}
			m.AddLimitation(code, r.slug, f.Path, bf.Reason)
			if f.Required != nil && *f.Required {
				c.Publishable = false
				c.PublishableReason = strPtr(UnresolvedBlocksReason(f.Path))
			}
		case len(bf.Unresolved) > 0 || bf.Source == SourceUnionInferred || bf.Source == SourceMixed:
			// Partly answered is not the same as answered. The field still has
			// a usable list, so the collection stays publishable, but the gap
			// is stated rather than papered over.
			m.AddLimitation(LimBlockSlugsUnknown, r.slug, f.Path, bf.Reason)
		}
	}
	for _, f := range shard.Fields {
		if f.PayloadType == TypeRichText {
			m.AddLimitation(LimRichTextShapeUnknown, r.slug, f.Path, "")
			break
		}
	}
	if !d.localeSampled[r.slug] && loc.Enabled != nil && *loc.Enabled {
		m.AddLimitation(LimLocalizationPerFieldUnknwn, r.slug, "", "")
	}

	if c.Reachability == ReachabilityAccessDenied {
		m.AddUnreachable(r.slug, kindCollection, ReasonAccessDenied,
			"present in the GraphQL schema, absent from /api/access")
	}
	if c.Flags.EndpointsDisabled != nil && *c.Flags.EndpointsDisabled {
		m.AddUnreachable(r.slug, kindCollection, ReasonEndpointsDisabled, "HTTP 501 Cannot GET")
	} else if c.Reachability == ReachabilityGraphQLDisabled {
		m.AddUnreachable(r.slug, kindCollection, ReasonGraphQLDisabled,
			"present in /api/access, absent from the GraphQL schema")
	}
	return c, shard
}

// buildGlobal assembles one global index entry and its shard.
func (d *Discoverer) buildGlobal(m *Manifest, r *row, schema *Schema, blocks BlockSources, loc Localization) (*Global, *Shard) {
	e := r.entity
	singular := ""
	if e != nil {
		singular = e.Singular
	}
	obj := schema.Type(singular)

	g := &Global{
		Slug:         r.slug,
		Internal:     IsInternal(r.slug),
		Reachability: d.reachabilityFor(r),
		Warnings:     []string{},
		Labels:       DeriveLabels(r.slug, singular),
	}
	if e != nil && e.Singular != "" {
		g.GraphQL = &GraphQLNames{Singular: e.Singular, Source: e.NameSrc}
	}
	g.Permissions = decodeGlobalPermissions(r.perms)

	drafts, _, _ := FlagsFromObject(obj)
	g.Flags = GlobalFlags{Drafts: drafts}
	g.FlagsSource = GlobalFlagsSource{Drafts: SourceUnknown, Versions: SourceUnknown}
	if drafts != nil {
		g.FlagsSource.Drafts = SourceGraphQL
	}
	if e != nil && e.Versions != nil {
		g.Flags.Versions = e.Versions
		g.FlagsSource.Versions = SourceGraphQL
	} else if g.Permissions.ReadVersions {
		// readVersions in /api/access is positive evidence on its own.
		g.Flags.Versions = boolPtr(true)
		g.FlagsSource.Versions = SourceAccess
	}

	var shard *Shard
	if obj != nil {
		shard = buildShard(e, schema, buildShardOptions{Generation: m.Generation, Blocks: blocks})
		ResolveShardBlocks(shard, schema, blocks)
		shard.Finalize()
		g.FieldsSource = SourceGraphQL
	} else {
		shard = NewShard(m.Generation, r.slug)
		shard.Finalize()
		g.FieldsSource = SourceUnknown
		m.AddLimitation(LimFieldsUnavailable, r.slug, "", "no GraphQL schema for this global")
	}
	g.FieldsCount = len(shard.Fields)
	g.FieldsSHA256 = shard.SHA256
	g.FieldsShard = cache.ShardName(r.slug, cache.KindGlobal)

	switch g.Reachability {
	case ReachabilityAccessDenied:
		m.AddUnreachable(r.slug, kindGlobal, ReasonAccessDenied,
			"present in the GraphQL schema, absent from /api/access")
	case ReachabilityGraphQLDisabled:
		m.AddUnreachable(r.slug, kindGlobal, ReasonGraphQLDisabled,
			"present in /api/access, absent from the GraphQL schema")
	}
	_ = loc
	return g, shard
}

// reachabilityFor implements §7.3's union bookkeeping.
//
// The graphql-disabled verdict is only meaningful when GraphQL answered for
// the rest of the project: if the endpoint is gone entirely, "absent from
// GraphQL" is true of every entity and says nothing about any of them, so
// everything /api/access saw is simply reachable over REST.
func (d *Discoverer) reachabilityFor(r *row) string {
	switch {
	case r.inAccess && r.inGraphQL:
		return ReachabilityOK
	case r.inAccess && !d.graphQLUsable:
		return ReachabilityOK
	case r.inAccess:
		return ReachabilityGraphQLDisabled
	case r.inGraphQL:
		return ReachabilityAccessDenied
	default:
		return ReachabilityOK
	}
}

func (d *Discoverer) labelsFor(slug, singular string, probe probeResult) Labels {
	if probe.LabelParsed {
		return LabelsFromMessage(slug, singular, probe.PluralLabel)
	}
	return DeriveLabels(slug, singular)
}

// mergeFlags applies §7.6's precedence: a GraphQL fact beats a probe, a probe
// beats /api/access, and nothing is ever defaulted.
func mergeFlags(e *entity, probe probeResult, upload, drafts, trash, folders, useAPIKey *bool, perms Permissions) (Flags, FlagsSource) {
	f := Flags{}
	s := FlagsSource{
		Upload: SourceUnknown, Auth: SourceUnknown, UseAPIKey: SourceUnknown,
		Versions: SourceUnknown, Drafts: SourceUnknown, Trash: SourceUnknown,
		Folders: SourceUnknown, Duplicate: SourceUnknown, Orderable: SourceUnknown,
		EndpointsDisabled: SourceUnknown,
	}
	set := func(dst **bool, src *string, graphql, probed *bool, probedSource string) {
		switch {
		case graphql != nil:
			*dst = graphql
			*src = SourceGraphQL
		case probed != nil:
			*dst = probed
			*src = probedSource
		}
	}

	set(&f.Upload, &s.Upload, upload, probe.Upload, SourceProbe)
	set(&f.Drafts, &s.Drafts, drafts, probe.Drafts, SourceProbe)
	set(&f.Trash, &s.Trash, trash, probe.Trash, SourceProbe)
	set(&f.Folders, &s.Folders, folders, probe.Folders, SourceProbe)
	set(&f.UseAPIKey, &s.UseAPIKey, useAPIKey, nil, SourceProbe)
	if f.EndpointsDisabled == nil && probe.EndpointsDisabled != nil {
		f.EndpointsDisabled = probe.EndpointsDisabled
		s.EndpointsDisabled = SourceProbe
	}

	var gqlVersions, gqlAuth, gqlDuplicate *bool
	if e != nil {
		gqlVersions, gqlAuth, gqlDuplicate = e.Versions, e.Auth, e.Duplicate
	}
	set(&f.Versions, &s.Versions, gqlVersions, probe.Versions, SourceProbe)
	set(&f.Auth, &s.Auth, gqlAuth, probe.Auth, SourceProbe)
	set(&f.Duplicate, &s.Duplicate, gqlDuplicate, nil, SourceProbe)

	// readVersions in /api/access is independent positive evidence: the key is
	// present only when the collection has versions enabled.
	if f.Versions == nil && perms.ReadVersions {
		f.Versions = boolPtr(true)
		s.Versions = SourceAccess
	}
	return f, s
}

// anyFlagUnknown reports a capability hole worth declaring.
//
// endpoints_disabled and orderable are deliberately excluded. Both are
// answered by rule (b) — attempt the operation and classify the server's 501 /
// 404 after the fact — so a null there costs nothing, while probing all 49
// collections to fill them in would cost 49 requests against §7.1's
// four-request cold budget. The flags themselves stay null in the manifest;
// only the limitations[] noise is suppressed.
func anyFlagUnknown(f Flags) bool {
	return f.Upload == nil || f.Auth == nil || f.UseAPIKey == nil || f.Versions == nil ||
		f.Drafts == nil || f.Trash == nil || f.Folders == nil || f.Duplicate == nil
}

// decodePermissions reads one /api/access collection entry.
//
// agentux claimed a permission may be {permission: true, where: {...}}; the
// live probe shows plain booleans. The parser accepts both and no UX is built
// on the conditional form (§1 conflict 22).
func decodePermissions(entry map[string]any) Permissions {
	p := Permissions{}
	if entry == nil {
		return p
	}
	p.Create = permBool(entry["create"])
	p.Read = permBool(entry["read"])
	p.Update = permBool(entry["update"])
	p.Delete = permBool(entry["delete"])
	_, hasReadVersions := entry["readVersions"]
	p.ReadVersions = hasReadVersions && permBool(entry["readVersions"])
	p.Unlock = permBool(entry["unlock"])
	// `fields` is the boolean true for a privileged key and an object for a
	// restricted one. It is recorded as a tri-state and NEVER used as a
	// field-name source (§1 conflict 12).
	switch v := entry["fields"].(type) {
	case bool:
		p.FieldLevel = boolPtr(!v)
	case map[string]any:
		p.FieldLevel = boolPtr(true)
	}
	return p
}

func decodeGlobalPermissions(entry map[string]any) GlobalPermissions {
	p := GlobalPermissions{}
	if entry == nil {
		return p
	}
	p.Read = permBool(entry["read"])
	p.Update = permBool(entry["update"])
	_, hasReadVersions := entry["readVersions"]
	p.ReadVersions = hasReadVersions && permBool(entry["readVersions"])
	switch v := entry["fields"].(type) {
	case bool:
		p.FieldLevel = boolPtr(!v)
	case map[string]any:
		p.FieldLevel = boolPtr(true)
	}
	return p
}

func permBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case map[string]any:
		b, _ := t["permission"].(bool)
		return b
	default:
		return false
	}
}

// fillManifest writes the project-level sections.
func (d *Discoverer) fillManifest(m *Manifest, view *accessView, identity *payload.Identity,
	authSource string, mode modeOutcome, project projectProbes, loc Localization, now time.Time) {
	m.Meta = ManifestMeta{
		Scope:          d.opt.ScopeKey,
		Profiles:       nonNilStrings(d.opt.Profiles),
		BaseURL:        d.baseURL,
		APIPath:        d.apiPath,
		GraphQLPath:    d.graphQLPath,
		HeaderNames:    nonNilStrings(d.opt.HeaderNames),
		KeyFingerprint: d.opt.KeyFingerprint,
		CreatedAt:      now,
		ConfirmedAt:    now,
	}
	if m.Meta.KeyFingerprint == "" {
		m.Meta.KeyFingerprint = cache.AnonKeyFingerprint
	}

	topology := ProjectTopology(view.Collections, view.Globals, view.CanAccessAdmin)
	m.Fingerprint = Fingerprint{
		TopologySHA256: TopologySHA256(topology),
		CheckedAt:      now,
		ServerIdentity: view.PoweredBy,
	}
	if d.snapshot != nil {
		m.Fingerprint.SchemaSHA256 = SchemaSHA256(d.snapshot.QueryNames)
	}
	if m.Fingerprint.SchemaSHA256 == "" {
		m.Degrade(StageFingerprint)
	}

	m.Source = Source{
		BaseURL:              d.baseURL,
		APIPath:              d.apiPath,
		APIPathSource:        d.apiPathSource,
		GraphQLPath:          d.graphQLPath,
		GraphQLPathSource:    d.graphQLPathSource,
		PoweredBy:            view.PoweredBy,
		PayloadVersionSource: orUnknown(d.opt.PayloadVersionSource),
		DBAdapter:            DBUnknown,
		DBAdapterSource:      SourceUnknown,
	}
	if d.opt.PayloadVersion != "" {
		m.Source.PayloadVersion = strPtr(d.opt.PayloadVersion)
	} else {
		m.Source.PayloadVersionSource = SourceUnknown
		m.AddLimitation(LimPayloadVersionUnknown, "", "", "")
	}

	authMode := d.authMode
	if authMode == "" {
		authMode = payload.AuthModeAnonymous
	}
	m.Identity = Identity{
		AuthMode:             authMode,
		AuthCollectionSource: orUnknown(authSource),
		KeyFingerprint:       m.Meta.KeyFingerprint,
	}
	if d.authCollection != "" {
		m.Identity.AuthCollection = strPtr(d.authCollection)
	}
	if identity != nil {
		m.Identity.UserID = identity.UserID
		m.Identity.Verified = identity.Verified
		m.Identity.CanAccessAdmin = identity.CanAccessAdmin
		m.Identity.Strategy = identity.Strategy
	} else {
		m.Identity.CanAccessAdmin = view.CanAccessAdmin
	}

	m.Capabilities.Localization = loc
	m.Capabilities.Reorder = project.Reorder
	m.Capabilities.OG = project.OG
	// §6.2's method-override promotion is a client capability that Payload
	// always honours for read-shaped requests; it is recorded as true rather
	// than probed, because probing it would mean sending a form-encoded body
	// whose misinterpretation is exactly conflict 14's hazard.
	m.Capabilities.MethodOverride = boolPtr(true)
	// Sortability is inferred from the field kind for every field on every
	// entity, never measured, so it is one project-level declaration rather
	// than one row per collection.
	m.AddLimitation(LimSortabilityHeuristic, "", "", "sortability is inferred from the field kind")
	m.Capabilities.CustomEndpoints = nonNilStrings(d.opt.CustomEndpoints)
	if len(m.Capabilities.CustomEndpoints) == 0 {
		m.AddLimitation(LimCustomEndpointsNotEnum, "", "", "")
	}

	for slug := range view.Collections {
		switch slug {
		case "payload-jobs":
			m.Capabilities.Jobs.Collection = strPtr(slug)
		case "payload-preferences":
			m.Capabilities.Preferences.Collection = strPtr(slug)
		}
	}
	for slug := range view.Globals {
		if slug == "payload-jobs-stats" {
			m.Capabilities.Jobs.StatsGlobal = strPtr(slug)
		}
	}
	if project.Preferences != nil && *project.Preferences && m.Capabilities.Preferences.Collection == nil {
		m.Capabilities.Preferences.Collection = strPtr("payload-preferences")
	}

	// Orderable is a project-level answer applied to every collection: the
	// reorder route either exists or it does not.
	for _, c := range m.Collections {
		if project.Reorder != nil {
			c.Flags.Orderable = project.Reorder
			c.FlagsSource.Orderable = SourceProbe
		}
	}

	if mode.Mode != GraphQLModeOK {
		detail := mode.Detail
		if mode.Hint != "" {
			detail = strings.TrimSpace(detail + " " + mode.Hint)
		}
		m.AddLimitation(LimHookMutationUnknown, "", "", "")
		m.Capabilities.GraphQL.Detail = mode.Detail
		m.Capabilities.GraphQL.Hint = mode.Hint
		d.warn(output.Warning{
			Code:    "graphql_degraded",
			Message: fmt.Sprintf("GraphQL is %s at %s; discovery fell back to REST-only", mode.Mode, d.graphQLPath),
			Hint:    detail,
		})
	} else {
		m.AddLimitation(LimHookMutationUnknown, "", "", "")
	}
}

// fillAdapter runs §7.11's source ladder for db_adapter.
func (d *Discoverer) fillAdapter(m *Manifest, idTypes, sampleIDs []string) {
	switch {
	case d.opt.DBAdapter != "" && d.opt.DBAdapterSource == SourceConfigured:
		m.Source.DBAdapter = d.opt.DBAdapter
		m.Source.DBAdapterSource = SourceConfigured
	case d.opt.DBAdapter != "" && d.opt.DBAdapter != DBUnknown:
		m.Source.DBAdapter = d.opt.DBAdapter
		m.Source.DBAdapterSource = orUnknown(d.opt.DBAdapterSource)
	default:
		m.Source.DBAdapter, m.Source.DBAdapterSource = InferDBAdapter(idTypes, sampleIDs)
	}
	if m.Source.DBAdapter == DBUnknown {
		m.AddLimitation(LimDBAdapterUnknown, "", "", "")
	}
	// unsupported_operators stays empty unless the adapter was pinned by the
	// profile: on MongoDB `all` maps to $all and works, so a guessed list
	// would make PayCLI refuse a query the server would have answered.
	if m.Source.DBAdapter == DBPostgres && m.Source.DBAdapterSource == SourceConfigured {
		for _, op := range []string{"all", "near", "within", "intersects"} {
			m.UnsupportedOperators = append(m.UnsupportedOperators, UnsupportedOperator{
				Operator: op,
				Reason:   "db_adapter is pinned to postgres in the profile, where this operator returns HTTP 500",
			})
		}
	}
}

func orUnknown(s string) string {
	if s == "" {
		return SourceUnknown
	}
	return s
}

func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// ResolveCollection looks a slug up and produces §11's collection_unknown with
// did-you-mean candidates when it is absent.
func (m *Manifest) ResolveCollection(slug string) (*Collection, error) {
	if c, ok := m.Collection(slug); ok {
		return c, nil
	}
	e := apierr.New(apierr.CodeCollectionUnknown,
		"this project has no collection %q", slug).
		WithDidYouMean(apierr.DidYouMean(slug, m.CollectionSlugs())...).
		WithHint("run `pay collections` for the full list, or `pay discover --refresh` if the project changed")
	return nil, e
}

// ResolveGlobal is ResolveCollection for globals.
func (m *Manifest) ResolveGlobal(slug string) (*Global, error) {
	if g, ok := m.Global(slug); ok {
		return g, nil
	}
	e := apierr.New(apierr.CodeGlobalUnknown,
		"this project has no global %q", slug).
		WithDidYouMean(apierr.DidYouMean(slug, m.GlobalSlugs())...).
		WithHint("run `pay collections --globals` for the full list")
	return nil, e
}
