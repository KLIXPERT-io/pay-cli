package cli

import (
	"context"
	"encoding/json"
	"time"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/buildinfo"
	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/config"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
)

func init() { Register(newDiscoverCmd) }

// discoveryState memoises everything one invocation learned about the project.
// It is built at most once: a command that needs both the index and a shard
// pays for discovery exactly once, and a command that needs neither never
// touches the cache at all.
type discoveryState struct {
	manifest      *discovery.Manifest
	cacheManifest *cache.Manifest
	cacheMeta     *output.CacheMeta
	shards        map[string]*discovery.Shard
	err           error
	ran           bool
}

// DiscoverOptions are the knobs `pay discover` exposes. The zero value is what
// every other command uses.
type DiscoverOptions struct {
	Deep             bool
	AllowWriteProbes bool
	NoLabels         bool
	NoProbes         bool
}

// CachedManifest returns the discovery index from disk without any possibility
// of a network call. Help text (§10.5) and shell completion (§9.9) use this and
// nothing else: both must render instantly on a cold cache rather than block.
func (rt *Runtime) CachedManifest() (*discovery.Manifest, bool) {
	if rt.disc != nil && rt.disc.manifest != nil {
		return rt.disc.manifest, true
	}
	if rt.Cfg == nil {
		return nil, false
	}
	sc, err := rt.Scope()
	if err != nil {
		return nil, false
	}
	cm, ok, _ := rt.Cache().ReadManifest(sc)
	if !ok {
		return nil, false
	}
	m, ok := discovery.DecodeManifest(cm.Raw)
	if !ok {
		return nil, false
	}
	return m, true
}

// Discovery returns the discovery index, running §7's pipeline when the cache
// cannot answer. It is memoised per invocation.
func (rt *Runtime) Discovery(ctx context.Context) (*discovery.Manifest, error) {
	rt.discOnce.Do(func() {
		rt.disc = &discoveryState{shards: map[string]*discovery.Shard{}}
		rt.disc.manifest, rt.disc.err = rt.resolveDiscovery(ctx, DiscoverOptions{})
		rt.adoptManifest(rt.disc.manifest)
	})
	if rt.disc.err != nil {
		return nil, rt.disc.err
	}
	return rt.disc.manifest, nil
}

// resolveDiscovery implements §8.5's ladder for the discovery manifest.
func (rt *Runtime) resolveDiscovery(ctx context.Context, opts DiscoverOptions) (*discovery.Manifest, error) {
	if err := rt.requireServer(); err != nil {
		return nil, err
	}
	defer rt.enterDiscovery()()
	sc, scopeErr := rt.Scope()
	now := rt.Now()

	if scopeErr == nil && !rt.Cfg.Refresh && !rt.Cfg.NoCache {
		cm, ok, warn := rt.Cache().ReadManifest(sc)
		if warn != nil {
			rt.Warn(*warn)
		}
		if ok {
			if m, decoded := discovery.DecodeManifest(cm.Raw); decoded {
				fresh, age := cm.Freshness(now)
				switch fresh {
				case cache.FreshnessFresh:
					rt.disc.cacheManifest = cm
					rt.setCacheMeta(output.CacheHit, age, cm, false)
					return m, nil
				case cache.FreshnessStale:
					rev := cache.Revalidate(ctx, cm.Fingerprint.TopologySHA256,
						rt.topologyProbe(), cache.RevalidationBudget)
					if rev.Warning != nil {
						rt.Warn(*rev.Warning)
					}
					if !rev.Refresh {
						rt.disc.cacheManifest = cm
						rt.setCacheMeta(rev.Discovery, age, cm, rev.Discovery == output.CacheRevalidated)
						if rev.Confirm {
							if _, w := rt.Cache().Touch(sc, now); w != nil {
								rt.Warn(*w)
							}
						}
						return m, nil
					}
				}
			}
		}
	}
	return rt.RunDiscovery(ctx, opts)
}

// topologyProbe is §8.4's Level-1 check: one /api/access request reduced to a
// topology hash. It never mutates state.
func (rt *Runtime) topologyProbe() cache.Probe {
	return func(ctx context.Context) (string, error) {
		client, err := rt.Client(ctx)
		if err != nil {
			return "", err
		}
		acc, err := client.Access(ctx)
		if err != nil {
			return "", err
		}
		return discovery.TopologySHA256(
			discovery.ProjectTopology(acc.Collections, acc.Globals, acc.CanAccessAdmin)), nil
	}
}

// RunDiscovery executes §7's pipeline and persists the result (§8.7).
func (rt *Runtime) RunDiscovery(ctx context.Context, opts DiscoverOptions) (*discovery.Manifest, error) {
	if err := rt.requireServer(); err != nil {
		return nil, err
	}
	defer rt.enterDiscovery()()
	client, err := rt.Client(ctx)
	if err != nil {
		return nil, err
	}
	cred, err := rt.Credential(ctx)
	if err != nil {
		return nil, err
	}
	now := rt.Now()
	sc, scopeErr := rt.Scope()

	cachedAuth := ""
	if scopeErr == nil {
		if res, ok, warn := rt.Cache().LookupAuthResolution(sc); ok {
			cachedAuth = res.AuthCollection
		} else if warn != nil {
			rt.Warn(*warn)
		}
	}

	scan := config.Scan(rt.Project)
	generation := cache.NewGeneration(now)

	dopts := discovery.Options{
		Client:               client,
		BaseURL:              rt.Cfg.BaseURL,
		APIPath:              rt.Cfg.APIPath,
		APIPathSource:        rt.Cfg.Sources["api_path"],
		GraphQLRoute:         rt.Cfg.GraphQLRoute,
		GraphQLPath:          rt.Cfg.GraphQLPath,
		GraphQLPathSource:    rt.Cfg.Sources["graphql_path"],
		AuthMode:             string(cred.Mode),
		AuthCollection:       rt.Cfg.AuthCollection,
		AuthCollectionSource: rt.Cfg.Sources["auth_collection"],
		CachedAuthCollection: cachedAuth,
		CredentialAbsent:     cred.Anonymous(),
		KeyFingerprint:       cred.Fingerprint,
		Concurrency:          rt.Cfg.Concurrency,
		Profile:              rt.Cfg.Profile,
		Profiles:             []string{rt.Cfg.Profile},
		HeaderNames:          rt.Cfg.HeaderNames(),
		ScopeKey:             sc.Key,
		CLIVersion:           buildinfo.Version(),
		Generation:           generation,
		Now:                  rt.App.Now,
		TTL:                  rt.discoveryTTL(),
		Deep:                 opts.Deep,
		AllowWriteProbes:     opts.AllowWriteProbes,
		NoLabels:             opts.NoLabels,
		NoProbes:             opts.NoProbes,
		ConfiguredIDType:     rt.Cfg.IDType,
		ConfiguredLocales:    rt.Cfg.Locales,
		ConfiguredBlocks:     rt.Cfg.Blocks,
		CustomEndpoints:      rt.Cfg.CustomEndpoints,
		PayloadVersion:       firstNonEmpty(rt.Cfg.PayloadVersion, scan.PayloadVersion),
		PayloadVersionSource: payloadVersionSource(rt.Cfg, scan),
		DBAdapter:            firstNonEmpty(rt.Cfg.DBAdapter, scan.DBAdapter),
		DBAdapterSource:      dbAdapterSource(rt.Cfg, scan),
		ProjectBlockSlugs:    scan.BlockSlugs,
		// The (slug, interfaceName) pairs are what turn a blocks field's
		// GraphQL union members into blockTypes the REST API accepts (§7.10).
		ProjectBlockInterfaces: scan.SlugByInterface,
		// The blocks' own field declarations carry required-ness, which is
		// unreachable from GraphQL for a block: Payload publishes no input
		// type for one, so the NON_NULL trick §7.4 uses for a collection field
		// has nothing to read.
		ProjectBlockDecls: blockSourceDecls(scan),
		Logger:            rt.Log,
	}
	if scan.BlockSlugFiles != nil {
		dopts.ProjectBlockSlugFiles = scan.BlockSlugFiles
	}

	d, err := discovery.New(dopts)
	if err != nil {
		return nil, err
	}
	started := rt.Now()
	res, err := d.Run(ctx)
	if err != nil {
		return nil, err
	}
	rt.Warn(res.Warnings...)
	rt.Warn(cache.DiscoveryRanWarning(rt.Now().Sub(started)))

	if rt.disc == nil {
		rt.disc = &discoveryState{shards: map[string]*discovery.Shard{}}
	}
	rt.disc.manifest = res.Manifest
	rt.disc.ran = true
	// Stage -1 may have just resolved the slug the api-key header embeds; the
	// client this invocation already built still carries the placeholder
	// (§7.0).
	// Stage 0 may likewise have probed an api_path the configured value does
	// not match; without adopting it every later data request would go to the
	// configured prefix and 404 (§7.2, §21.1 H1).
	rt.adoptManifest(res.Manifest)
	rt.disc.shards = map[string]*discovery.Shard{}
	for name, shard := range res.Shards {
		rt.disc.shards[name] = shard
	}

	mode := output.CacheMiss
	if rt.Cfg.NoCache {
		mode = output.CacheDisabled
	} else if rt.Cfg.Refresh {
		mode = output.CacheBypassed
	}
	rt.disc.cacheMeta = &output.CacheMeta{
		Discovery:   mode,
		Fingerprint: res.Manifest.Fingerprint.TopologySHA256,
	}

	if scopeErr == nil && !rt.Cfg.NoCache {
		set := cache.Set{
			Generation: generation,
			Manifest:   res.Manifest,
			Shards:     map[string]any{},
		}
		for name, shard := range res.Shards {
			set.Shards[name] = shard
		}
		if _, warns := rt.Cache().WriteSet(sc, set, now); len(warns) > 0 {
			rt.Warn(warns...)
		}
		if res.AuthResolved && res.AuthCollection != "" {
			if _, w := rt.Cache().PutAuthResolution(sc, res.AuthCollection, now); w != nil {
				rt.Warn(*w)
			}
		}
	}
	return res.Manifest, nil
}

// adoptManifestAuthCollection feeds a manifest's resolved auth-collection slug
// back into the runtime's client, whether the manifest came from the cache or
// from a fresh Stage -1.
// adoptManifest feeds everything a manifest settled about *how to reach the
// server* back into the runtime. It is called at every site a manifest enters
// the runtime — a fresh Stage -1/Stage 0 run and a cache hit alike — because a
// warm cache is otherwise exactly as broken as a cold one.
func (rt *Runtime) adoptManifest(m *discovery.Manifest) {
	rt.adoptManifestAuthCollection(m)
	rt.adoptManifestAPIPath(m)
}

// adoptManifestAPIPath installs the api_path §7.2's ladder resolved onto the
// runtime and onto the already-built client. Without it the ladder is
// decorative: discovery finds the project at /cms-api, caches a manifest that
// says so, and every read and write still goes to the configured /api and
// comes back route_not_found.
//
// An explicitly configured api_path is never overridden. --api-path,
// PAY_API_PATH and a profile key all outrank a probe, and a stale cached
// manifest must not be able to redirect a user who said where the API is. The
// cache scope is deliberately pinned first: §8.1 keys it on the *configured*
// base_url + api_path at both ends, so re-deriving it here would write the
// manifest under a scope the next process never looks in.
func (rt *Runtime) adoptManifestAPIPath(m *discovery.Manifest) {
	if m == nil || rt.Cfg == nil || rt.apiPathPinned() {
		return
	}
	_, _ = rt.Scope()

	apiPath := payload.NormalizePath(m.Source.APIPath)
	if apiPath == payload.NormalizePath(rt.Cfg.APIPath) {
		return
	}
	graphQLPath := payload.NormalizePath(m.Source.GraphQLPath)
	if rt.Cfg.Sources["graphql_path"] != config.SourceDerivedGraphQL {
		// An explicit graphql_path outranks the derivation, exactly as §4.2
		// says; only the REST prefix moves.
		graphQLPath = payload.NormalizePath(rt.Cfg.GraphQLPath)
	} else if graphQLPath == "" {
		graphQLPath = apiPath + rt.Cfg.GraphQLRoute
	}

	rt.Cfg.APIPath = apiPath
	rt.Cfg.GraphQLPath = graphQLPath
	if rt.Cfg.Sources != nil {
		rt.Cfg.Sources["api_path"] = m.Source.APIPathSource
	}
	// AdoptAPIPath reaches every clone of the client, including the handle a
	// command took before discovery ran — which is the one that issues the
	// actual request.
	if rt.client != nil {
		rt.client.AdoptAPIPath(apiPath, graphQLPath)
	}
}

// apiPathPinned reports whether the operator said where the API lives. Only
// the built-in default and a value this runtime itself adopted from a probe
// may be replaced.
func (rt *Runtime) apiPathPinned() bool {
	switch rt.Cfg.Sources["api_path"] {
	case config.SourceDefault, discovery.SourceProbed, "":
		return false
	default:
		return true
	}
}

func (rt *Runtime) adoptManifestAuthCollection(m *discovery.Manifest) {
	if m == nil || m.Identity.AuthCollection == nil {
		return
	}
	rt.adoptAuthCollection(*m.Identity.AuthCollection, m.Identity.AuthCollectionSource)
}

func (rt *Runtime) discoveryTTL() time.Duration {
	if rt.Cfg != nil && rt.Cfg.DiscoveryTTL > 0 {
		return rt.Cfg.DiscoveryTTL
	}
	return discovery.DefaultTTL
}

func (rt *Runtime) setCacheMeta(mode string, age time.Duration, cm *cache.Manifest, revalidated bool) {
	ageS := int64(age / time.Second)
	ttl, _ := cache.TTLFor(cache.ClassDiscoveryManifest)
	ttlS := int64(ttl.Fresh / time.Second)
	if rt.disc == nil {
		rt.disc = &discoveryState{shards: map[string]*discovery.Shard{}}
	}
	meta := &output.CacheMeta{Discovery: mode, AgeS: &ageS, TTLS: &ttlS, Revalidated: revalidated}
	if cm != nil {
		meta.Fingerprint = cm.Fingerprint.TopologySHA256
	}
	rt.disc.cacheMeta = meta
}

// Shard returns one entity's field schema, decoding it from the cache when the
// current invocation did not produce it.
func (rt *Runtime) Shard(ctx context.Context, slug string, kind cache.EntityKind) (*discovery.Shard, bool) {
	if _, err := rt.Discovery(ctx); err != nil {
		return nil, false
	}
	return rt.shardFromState(slug, kind)
}

// CachedShard is the offline form used by completion and help, which may never
// trigger discovery.
func (rt *Runtime) CachedShard(slug string, kind cache.EntityKind) (*discovery.Shard, bool) {
	if rt.disc == nil {
		rt.disc = &discoveryState{shards: map[string]*discovery.Shard{}}
	}
	if rt.disc.manifest == nil {
		m, ok := rt.CachedManifest()
		if !ok {
			return nil, false
		}
		rt.disc.manifest = m
	}
	return rt.shardFromState(slug, kind)
}

func (rt *Runtime) shardFromState(slug string, kind cache.EntityKind) (*discovery.Shard, bool) {
	name := cache.ShardName(slug, kind)
	if s, ok := rt.disc.shards[name]; ok && s != nil {
		return s, true
	}
	sc, err := rt.Scope()
	if err != nil {
		return nil, false
	}
	cm := rt.disc.cacheManifest
	if cm == nil {
		var ok bool
		var warn *output.Warning
		cm, ok, warn = rt.Cache().ReadManifest(sc)
		if warn != nil {
			rt.Warn(*warn)
		}
		if !ok {
			return nil, false
		}
		rt.disc.cacheManifest = cm
	}
	raw, ok, warn := rt.Cache().ReadShard(sc, cm, slug, kind)
	if warn != nil {
		rt.Warn(*warn)
	}
	if !ok {
		return nil, false
	}
	var shard discovery.Shard
	if err := json.Unmarshal(raw.Raw, &shard); err != nil {
		return nil, false
	}
	rt.disc.shards[name] = &shard
	return &shard, true
}

// blockSourceDecls converts §7.10's on-disk block declarations into the shape
// internal/discovery consumes, keyed by slug.
//
// The conversion exists because internal/discovery must not depend on
// internal/config: every project fact it needs arrives as a plain value, so the
// pipeline stays testable without a filesystem.
func blockSourceDecls(scan *config.ScanResult) map[string]discovery.BlockSourceDecl {
	if scan == nil || len(scan.BlockDecls) == 0 {
		return nil
	}
	out := make(map[string]discovery.BlockSourceDecl, len(scan.BlockDecls))
	for _, d := range scan.BlockDecls {
		if d.Slug == "" {
			continue
		}
		fields := make([]discovery.BlockSourceField, 0, len(d.Fields))
		for _, f := range d.Fields {
			fields = append(fields, discovery.BlockSourceField{
				Name: f.Name, Type: f.Type, Required: f.Required,
			})
		}
		out[d.Slug] = discovery.BlockSourceDecl{
			File:     scan.SlugFile[d.Slug],
			Fields:   fields,
			Complete: d.FieldsComplete,
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func payloadVersionSource(cfg *config.Resolved, scan *config.ScanResult) string {
	if cfg != nil && cfg.PayloadVersion != "" {
		return config.SourceConfigured
	}
	if scan != nil {
		return scan.PayloadVersionSource
	}
	return config.SourceUnknown
}

func dbAdapterSource(cfg *config.Resolved, scan *config.ScanResult) string {
	if cfg != nil && cfg.DBAdapter != "" {
		return config.SourceConfigured
	}
	if scan != nil {
		return scan.DBAdapterSource
	}
	return config.SourceUnknown
}

// discoverPayload is the data block of `pay discover`.
type discoverPayload struct {
	DiscoveryRevision string         `json:"discovery_revision"`
	Generation        string         `json:"generation"`
	GeneratedAt       time.Time      `json:"generated_at"`
	ExpiresAt         time.Time      `json:"expires_at"`
	Scope             string         `json:"scope"`
	Collections       int            `json:"collections"`
	Globals           int            `json:"globals"`
	Fields            int            `json:"fields"`
	Requests          int            `json:"requests"`
	ElapsedMS         int64          `json:"elapsed_ms"`
	Bytes             int64          `json:"bytes"`
	Degraded          []string       `json:"degraded"`
	Limitations       int            `json:"limitations"`
	Unreachable       int            `json:"unreachable"`
	GraphQL           string         `json:"graphql_mode"`
	AuthCollection    *string        `json:"auth_collection"`
	Identity          identityDigest `json:"identity"`
}

type identityDigest struct {
	AuthMode       string `json:"auth_mode"`
	Verified       bool   `json:"verified"`
	CanAccessAdmin bool   `json:"can_access_admin"`
	KeyFingerprint string `json:"key_fingerprint"`
}

func newDiscoverCmd(rt *Runtime) *cobra.Command {
	var opts DiscoverOptions
	var verifySort bool

	cmd := &cobra.Command{
		Use:     "discover",
		Short:   "Probe this Payload project and cache its schema (§7 discovery pipeline).",
		GroupID: GroupDiscovery,
		Args:    maxArgs(0, "pay discover [--refresh] [--deep]"),
		RunE: Handle(rt, "discover", func(ctx context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			ctx, cancel := rt.deadlineContext(ctx)
			defer cancel()
			if verifySort {
				// The asc/desc differential probe §7.5 describes is not part of
				// this build's discovery pipeline; saying so beats silently
				// reporting measured sortability that was never measured.
				opts.Deep = true
				rt.Warnf("feature_unavailable",
					"--verify-sort implies --deep here; the asc/desc differential sort probe is not implemented, so fields keep sortable_confidence \"heuristic\"")
			}
			m, err := rt.RunDiscovery(ctx, opts)
			if err != nil {
				return nil, err
			}
			fields := 0
			for _, c := range m.Collections {
				fields += c.FieldsCount
			}
			for _, g := range m.Globals {
				fields += g.FieldsCount
			}
			sc, _ := rt.Scope()
			data := discoverPayload{
				DiscoveryRevision: m.Revision(),
				Generation:        m.Generation,
				GeneratedAt:       m.GeneratedAt,
				ExpiresAt:         m.ExpiresAt,
				Scope:             sc.Key,
				Collections:       len(m.Collections),
				Globals:           len(m.Globals),
				Fields:            fields,
				Requests:          m.Diagnostics.Requests,
				ElapsedMS:         m.Diagnostics.ElapsedMS,
				Bytes:             m.Diagnostics.Bytes,
				Degraded:          m.Diagnostics.Degraded,
				Limitations:       len(m.Limitations),
				Unreachable:       len(m.Unreachable),
				GraphQL:           m.Capabilities.GraphQL.Mode,
				AuthCollection:    m.Identity.AuthCollection,
				Identity: identityDigest{
					AuthMode:       m.Identity.AuthMode,
					Verified:       m.Identity.Verified,
					CanAccessAdmin: m.Identity.CanAccessAdmin,
					KeyFingerprint: m.Identity.KeyFingerprint,
				},
			}
			if data.Degraded == nil {
				data.Degraded = []string{}
			}
			env := output.New("discover", output.KindCapabilities, data)
			env.WithNext(&output.Next{
				Reason: output.ReasonDiscoverFirst,
				Cmd:    "pay explain",
				Args:   map[string]any{},
			})
			return env, nil
		}),
	}
	cmd.Flags().BoolVar(&opts.Deep, "deep", false, "probe every collection and count documents (slower, complete)")
	cmd.Flags().BoolVar(&verifySort, "verify-sort", false, "implies --deep; requests measured sortability where available")
	cmd.Flags().BoolVar(&opts.AllowWriteProbes, "allow-write-probes", false,
		"allow §7.7's empty-POST probe, which CREATES a document in collections with no required fields")
	cmd.Flags().BoolVar(&opts.NoLabels, "no-labels", false, "skip the label harvest (the only DELETE discovery issues)")
	cmd.Flags().BoolVar(&opts.NoProbes, "no-probes", false, "GraphQL only; no REST capability probes")

	SetHelp(cmd, &Help{
		Synopsis: []string{"pay discover [--refresh] [--deep] [--verify-sort] [--allow-write-probes] [--no-labels]"},
		Output:   OutputSpec{Kind: output.KindCapabilities, Skeleton: `{"discovery_revision":"…","collections":49,"globals":5,"fields":1820,"requests":14}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitAuth, apierr.ExitNetwork,
			apierr.ExitConfig, apierr.ExitCapability},
		Examples: []Example{
			{Why: "populate the cache for this project", Cmd: "pay discover"},
			{Why: "force a re-probe after changing payload.config.ts", Cmd: "pay discover --refresh"},
			{Why: "complete capability set plus document counts", Cmd: "pay discover --deep --refresh"},
			{Why: "GraphQL is disabled on this project; skip the REST probes too", Cmd: "pay discover --no-probes"},
		},
		Mistakes: []Mistake{
			{Wrong: "Running `pay discover` before every command.",
				Right: "Every command discovers on demand and caches for 10 minutes; run discover explicitly only after a schema change."},
			{Wrong: "Using `--allow-write-probes` on a production project.",
				Right: "It creates a real document in any collection with no required fields. Use it on a dev instance only."},
			{Wrong: "Expecting `--deep` to be the default.",
				Right: "A cold run is four requests; --deep is 49 counts plus per-collection probes. Ask for it when you need document counts."},
		},
		SeeAlso: []string{"pay explain", "pay collections", "pay describe <collection>", "pay cache info"},
	})
	return cmd
}
