package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/buildinfo"
	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
)

// ---------------------------------------------------------------------------
// §7.10c the block census — runtime half
// ---------------------------------------------------------------------------
//
// internal/discovery builds and merges the census; this file decides WHEN it is
// read from the cache, when it is (re)built over the network, and where it is
// persisted. Two rules govern it:
//
//  1. The `pay blocks` catalog verbs (types, schema, example, new, validate,
//     learn) build it lazily, for exactly the entities they need, and the cost
//     is visible in meta.http_requests and in data.census.
//  2. Writes (create, update, apply, globals update) never build it. They use a
//     cached census when there is one — any age up to the 7-day hard max,
//     because it only ever produces warnings — and otherwise say, once, that
//     the blockTypes were not checked and which command fixes that. A write
//     that silently fanned out into dozens of extra GETs, dry-runs included,
//     would be the more surprising behaviour.

// Warning codes this file raises.
const (
	// warnBlocksUnchecked: a write carried block rows and nothing — no cached
	// census, no declared source — could check them.
	warnBlocksUnchecked = "blocks_unchecked"
	// warnCensusEntityFailed: one entity's census read failed; the others are
	// still reported.
	warnCensusEntityFailed = "census_entity_failed"
	// warnCensusTruncated: an entity has more documents than the census read.
	warnCensusTruncated = "census_truncated"
	// warnExampleMoved: a referenced example row is no longer where the census
	// saw it (the document changed since).
	warnExampleMoved = "example_moved"
	// warnCensusUnavailable: validate ran with structural checks only.
	warnCensusUnavailable = "census_unavailable"
	// warnCensusScopeWide: a catalog verb without --collection read the
	// census of more than one entity over the network; it states the cost
	// and the cheaper form.
	warnCensusScopeWide = "census_scope_wide"
)

// entityRef names one collection or global.
type entityRef struct {
	Kind string
	Slug string
}

func (e entityRef) cacheKind() cache.EntityKind {
	if e.Kind == discovery.EntityKindGlobal {
		return cache.KindGlobal
	}
	return cache.KindCollection
}

// cachedCensus loads the census from disk, never from the network. It is
// memoised on the invocation's discovery state.
func (rt *Runtime) cachedCensus() (*discovery.Census, bool) {
	if rt.disc != nil && rt.disc.census != nil {
		return rt.disc.census, true
	}
	if rt.Cfg == nil {
		return nil, false
	}
	sc, err := rt.Scope()
	if err != nil {
		return nil, false
	}
	raw, ok, warn := rt.Cache().ReadCensus(sc)
	if warn != nil {
		rt.Warn(*warn)
	}
	if !ok {
		return nil, false
	}
	c, err := discovery.DecodeCensusErr(raw)
	if err != nil {
		// The header matched, so this is a census file this build cannot
		// use; §8.3 makes that a miss PLUS a warning. The next build
		// overwrites it.
		rt.Warn(output.Warning{Code: cache.WarnCacheUnreadable,
			Message: rt.Cache().CensusPath(sc) + ": the block census could not be decoded (" + err.Error() +
				"); it is ignored and rebuilt by the next command that needs it",
			Hint: "pay blocks learn"})
		return nil, false
	}
	rt.rememberCensus(c)
	return c, true
}

func (rt *Runtime) rememberCensus(c *discovery.Census) {
	if rt.disc == nil {
		rt.disc = &discoveryState{shards: map[string]*discovery.Shard{}}
	}
	rt.disc.census = c
}

// censusOpts says which entities a command needs and how.
type censusOpts struct {
	// only names the entities; empty means every entity that can hold blocks.
	only []entityRef
	// force rebuilds even a fresh cached entity (`pay blocks learn`).
	force   bool
	maxDocs int
	// wide marks an every-entity scan by a catalog verb run without
	// --collection. An entity whose cached census found no block rows at all,
	// and whose schema declares no blocks field, is then reused for up to the
	// census's 7-day hard max instead of 24 h: re-reading two dozen
	// collections that held no blocks yesterday is the bulk of a wide
	// rebuild's cost and almost never its answer. `pay blocks learn` and an
	// explicit entity still re-read it.
	wide bool
}

// censusEntityReport is one entity's line in data.census.
type censusEntityReport struct {
	Kind          string    `json:"kind"`
	Slug          string    `json:"slug"`
	BuiltAt       time.Time `json:"built_at"`
	AgeS          int64     `json:"age_s"`
	FromCache     bool      `json:"from_cache"`
	Draft         bool      `json:"draft"`
	DocsScanned   int       `json:"docs_scanned"`
	TotalDocs     *int      `json:"total_docs"`
	Truncated     bool      `json:"truncated"`
	MaxDocs       int       `json:"max_docs"`
	Requests      int       `json:"requests"`
	RelationProbe string    `json:"relation_probe"`
	BlockRows     int       `json:"block_rows"`
	BlockTypes    int       `json:"block_types"`
	StoppedEarly  bool      `json:"stopped_early,omitempty"`
	Error         string    `json:"error,omitempty"`
}

// censusReport is data.census: where the facts came from and what they cost.
type censusReport struct {
	// Source is "cache" when nothing was read from the server, "built" when
	// everything was, "mixed" otherwise.
	Source    string               `json:"source"`
	Requests  int                  `json:"requests"`
	MaxDocs   int                  `json:"max_docs"`
	Persisted bool                 `json:"persisted"`
	TTLS      int64                `json:"ttl_s"`
	Entities  []censusEntityReport `json:"entities"`
	// Revalidated is true when an observed-only finding made `validate`
	// rebuild the census once before answering.
	Revalidated bool `json:"revalidated,omitempty"`
	// RevalidateSkipped says why an observed-only finding did NOT trigger
	// that rebuild (the census was read minutes ago).
	RevalidateSkipped string `json:"revalidate_skipped,omitempty"`
	// Scope says which entities a command without --collection consulted
	// and why (narrowed to the cached holders of a block type, or wide).
	Scope string `json:"scope,omitempty"`
	Note  string `json:"note"`
}

// censusNote is printed with every census report so an agent never reads an
// observation as a declaration.
const censusNote = "The census is OBSERVED from real documents (draft versions where drafts exist), " +
	"not declared by the project: a blockType or value that never occurs in a document is absent from it. " +
	"Facts from a profile pin, the project source or GraphQL introspection are merged in and labelled with " +
	"their own source. `pay blocks learn` rebuilds it."

func reportFor(e *discovery.CensusEntity, fromCache bool, now time.Time) censusEntityReport {
	age := int64(0)
	if !e.BuiltAt.IsZero() {
		age = int64(now.Sub(e.BuiltAt) / time.Second)
		if age < 0 {
			age = 0
		}
	}
	return censusEntityReport{
		Kind: e.Kind, Slug: e.Slug, BuiltAt: e.BuiltAt, AgeS: age, FromCache: fromCache, Draft: e.Draft,
		DocsScanned: e.DocsScanned, TotalDocs: e.TotalDocs, Truncated: e.Truncated, MaxDocs: e.MaxDocs,
		Requests: e.Requests, RelationProbe: e.RelationProbe, BlockRows: e.BlockRows,
		BlockTypes: len(e.Types), StoppedEarly: e.StoppedEarly, Error: e.Error,
	}
}

// ensureCensus returns a census covering opts.only (or every entity that can
// hold blocks), building whatever is missing, expired or explicitly forced.
func ensureCensus(ctx context.Context, d *Deps, opts censusOpts) (*discovery.Census, *censusReport, error) {
	rt := d.RT
	if d.Client == nil {
		client, err := d.requireClient()
		if err != nil {
			return nil, nil, err
		}
		d.Client = client
	}
	if d.Manifest == nil {
		return nil, nil, apierr.New(apierr.CodeFeatureUnavailable,
			"the block census needs this project's capability manifest, and discovery did not produce one").
			WithHint("pay discover --refresh")
	}
	if opts.maxDocs <= 0 {
		opts.maxDocs = discovery.DefaultCensusMaxDocs
	}
	now := rt.Now()
	sc, scopeErr := rt.Scope()

	census, have := rt.cachedCensus()
	if !have || rt.Cfg.Refresh {
		census = discovery.NewCensus(sc.Key)
	}

	lookup := func(kind, slug string) *discovery.Shard {
		ck := cache.KindCollection
		if kind == discovery.EntityKindGlobal {
			ck = cache.KindGlobal
		}
		return d.shardFor(ctx, slug, ck)
	}
	var names []string
	for _, e := range opts.only {
		names = append(names, e.Slug)
	}
	targets := discovery.CensusTargets(d.Manifest, lookup, names)
	if len(opts.only) > 0 {
		// A slug that is both a collection and a global yields two targets;
		// keep only the kinds that were asked for.
		want := map[entityRef]bool{}
		for _, e := range opts.only {
			want[e] = true
		}
		kept := targets[:0]
		for _, t := range targets {
			if want[entityRef{Kind: t.Kind, Slug: t.Slug}] {
				kept = append(kept, t)
			}
		}
		targets = kept
	}

	report := &censusReport{MaxDocs: opts.maxDocs, Entities: []censusEntityReport{}, Note: censusNote}
	if ttl, ok := cache.TTLFor(cache.ClassBlockCensus); ok {
		report.TTLS = int64(ttl.Fresh / time.Second)
	}
	var build []discovery.CensusTarget
	reused := map[entityRef]*discovery.CensusEntity{}
	for _, t := range targets {
		if e, ok := census.Entity(t.Kind, t.Slug); ok && !opts.force {
			if reusable(e, opts.maxDocs, now) ||
				(opts.wide && blocklessReusable(e, lookup(t.Kind, t.Slug), opts.maxDocs, now)) {
				reused[entityRef{t.Kind, t.Slug}] = e
				continue
			}
		}
		build = append(build, t)
	}

	built := discovery.RunCensus(ctx, discovery.CensusOptions{
		Client: d.Client, Now: rt.App.Now, MaxDocs: opts.maxDocs,
		Concurrency: rt.Cfg.Concurrency, Logger: rt.Log,
	}, build)
	if err := ctx.Err(); err != nil {
		return nil, nil, apierr.Wrap(err, apierr.CodeTimeout, "the block census was interrupted before it finished")
	}
	for _, e := range built {
		if e.Error != "" {
			rt.Warn(output.Warning{Code: warnCensusEntityFailed,
				Message: fmt.Sprintf("the block census could not read %s %q: %s", e.Kind, e.Slug, e.Error),
				Hint:    "pay blocks learn " + e.Slug})
			if prev, ok := census.Entity(e.Kind, e.Slug); ok && prev.Error == "" {
				// Keep the last good census rather than replace it with a
				// failure.
				reused[entityRef{e.Kind, e.Slug}] = prev
				continue
			}
		}
		census.Put(e)
		report.Requests += e.Requests
	}
	for _, t := range targets {
		ref := entityRef{t.Kind, t.Slug}
		if e, ok := reused[ref]; ok {
			report.Entities = append(report.Entities, reportFor(e, true, now))
			continue
		}
		if e, ok := census.Entity(t.Kind, t.Slug); ok {
			report.Entities = append(report.Entities, reportFor(e, false, now))
			// A read cut short by the budget is worth saying even when it
			// found no block row — then it is the only sign that "no blocks
			// here" may be wrong. One that stopped early after sampling both
			// ends of the collection is an answer, not a truncation.
			if e.Truncated && (e.BlockRows > 0 || !e.StoppedEarly) {
				msg := fmt.Sprintf("%s has %d documents and the census read %d; rarer block types may be missing",
					e.Slug, derefInt(e.TotalDocs), e.DocsScanned)
				if e.BlockRows == 0 {
					msg = fmt.Sprintf("%s has %d documents and the census read %d without finding a block row; "+
						"the unread ones may hold blocks", e.Slug, derefInt(e.TotalDocs), e.DocsScanned)
				}
				rt.Warn(output.Warning{Code: warnCensusTruncated, Message: msg,
					Hint: fmt.Sprintf("pay blocks learn %s --census-max-docs %d", e.Slug, derefInt(e.TotalDocs))})
			}
		}
	}
	switch {
	case len(built) == 0:
		report.Source = "cache"
	case len(reused) == 0:
		report.Source = "built"
	default:
		report.Source = "mixed"
	}

	if len(built) > 0 {
		census.Scope = sc.Key
		census.CLIVersion = buildinfo.Version()
		census.UpdatedAt = now.UTC().Truncate(time.Second)
		if scopeErr == nil && !rt.Cfg.NoCache {
			var fresh []*discovery.CensusEntity
			for _, e := range built {
				if cur, ok := census.Entity(e.Kind, e.Slug); ok && cur == e {
					fresh = append(fresh, e)
				}
			}
			replace := rt.Cfg.Refresh
			ok, warn := rt.Cache().UpdateCensus(sc, func(raw []byte) (any, error) {
				census = mergeBuiltCensus(raw, census, fresh, replace)
				return census, nil
			})
			if warn != nil {
				rt.Warn(*warn)
			}
			report.Persisted = ok
		}
	} else {
		report.Persisted = have
	}
	rt.rememberCensus(census)
	return census, report, nil
}

// mergeBuiltCensus is what ensureCensus writes: the entities this invocation
// read (fresh), merged into the census as it is on disk NOW (raw, read under
// the scope lock). Agents run tools in parallel, and another pay process may
// have written other entities since this one loaded the file; writing the
// stale copy back would drop them. Per entity the newer read wins. --refresh
// (replace) discards the rest of the census by design.
func mergeBuiltCensus(raw []byte, census *discovery.Census, fresh []*discovery.CensusEntity, replace bool) *discovery.Census {
	if replace || raw == nil {
		return census
	}
	disk, ok := discovery.DecodeCensus(raw)
	if !ok {
		return census
	}
	for _, e := range fresh {
		if prev, has := disk.Entity(e.Kind, e.Slug); has && prev.BuiltAt.After(e.BuiltAt) {
			continue
		}
		disk.Put(e)
	}
	disk.Scope, disk.CLIVersion, disk.UpdatedAt = census.Scope, census.CLIVersion, census.UpdatedAt
	return disk
}

// reusable decides whether a cached entity answers a catalog command: fresh,
// read without error, and read with at least the requested document budget
// (or complete anyway).
func reusable(e *discovery.CensusEntity, maxDocs int, now time.Time) bool {
	if e.Error != "" {
		return false
	}
	if fresh, _ := cache.Evaluate(cache.ClassBlockCensus, e.BuiltAt, now); fresh != cache.FreshnessFresh {
		return false
	}
	return !e.Truncated || e.MaxDocs >= maxDocs
}

// blocklessReusable is the wide-scan reuse rule (censusOpts.wide): a cached
// entity read without error that held no block row at all, within the hard
// max, read with enough documents (or stopped early after a full page with
// none), and whose schema declares no blocks field.
func blocklessReusable(e *discovery.CensusEntity, shard *discovery.Shard, maxDocs int, now time.Time) bool {
	if e.Error != "" || e.BlockRows > 0 || len(e.Types) > 0 || len(e.Paths) > 0 {
		return false
	}
	if fresh, _ := cache.Evaluate(cache.ClassBlockCensus, e.BuiltAt, now); fresh == cache.FreshnessExpired {
		return false
	}
	if e.Truncated && !e.StoppedEarly && e.MaxDocs < maxDocs {
		return false
	}
	return !shardDeclaresBlocks(shard)
}

// shardDeclaresBlocks reports a schema that names a blocks field.
func shardDeclaresBlocks(shard *discovery.Shard) bool {
	if shard == nil {
		return false
	}
	if len(shard.BlockFields) > 0 || len(shard.Blocks) > 0 {
		return true
	}
	for _, f := range shard.Fields {
		if f.PayloadType == discovery.TypeBlocks {
			return true
		}
	}
	return false
}

// censusHolders are the entities whose cached census (read without error,
// within the hard max) saw blockType slug, collections first. It never reads
// the network.
func censusHolders(rt *Runtime, slug string) []entityRef {
	if rt == nil {
		return nil
	}
	c, ok := rt.cachedCensus()
	if !ok || c == nil {
		return nil
	}
	now := rt.Now()
	var out []entityRef
	for _, e := range c.Entities() {
		if e.Error != "" || e.Types[slug] == nil {
			continue
		}
		if fresh, _ := cache.Evaluate(cache.ClassBlockCensus, e.BuiltAt, now); fresh == cache.FreshnessExpired {
			continue
		}
		out = append(out, entityRef{Kind: e.Kind, Slug: e.Slug})
	}
	return sortedEntityRefs(out)
}

// wideScopeNote is data.census.scope for a catalog verb run without
// --collection.
const wideScopeNote = "wide: --collection was not given, so every collection and global that can hold blocks was consulted; " +
	"entities whose cached census held no blocks were reused for up to 7 days instead of re-read"

// wideCostWarning states what a wide census read cost, when it cost
// anything, and names the cheaper form. hintFmt has one %s for the entity
// with the most rows of slug (or the most block rows when slug is "").
func wideCostWarning(d *Deps, report *censusReport, slug, hintFmt string) {
	if d == nil || d.RT == nil || report == nil || report.Requests == 0 {
		return
	}
	built, withBlocks := 0, 0
	for _, e := range report.Entities {
		if !e.FromCache {
			built++
		}
		if e.BlockRows > 0 {
			withBlocks++
		}
	}
	best, bestN := "", -1
	if c, ok := d.RT.cachedCensus(); ok {
		for _, e := range c.Entities() {
			n := e.BlockRows
			if slug != "" {
				n = 0
				if t := e.Types[slug]; t != nil {
					n = t.Count
				}
			}
			if n > bestN || (n == bestN && e.Slug < best) {
				best, bestN = e.Slug, n
			}
		}
	}
	w := output.Warning{Code: warnCensusScopeWide,
		Message: fmt.Sprintf("--collection was not given, so the block census read %d of %d entities that can hold blocks "+
			"(%d requests; %d of them hold blocks); later runs answer from the cache for 24 h",
			built, len(report.Entities), report.Requests, withBlocks)}
	if best != "" && bestN > 0 {
		w.Hint = fmt.Sprintf(hintFmt, best) + " reads one entity"
	}
	d.RT.Warn(w)
}

// mergeCensusReports folds a first census pass into a second one run in the
// same invocation (resolveTypeScope's fallback): requests add up, and an
// entity the first pass read is not reported as served from the cache.
func mergeCensusReports(first, second *censusReport) *censusReport {
	if first == nil {
		return second
	}
	builtFirst := map[entityRef]censusEntityReport{}
	for _, e := range first.Entities {
		if !e.FromCache {
			builtFirst[entityRef{e.Kind, e.Slug}] = e
		}
	}
	second.Requests += first.Requests
	fromCache := false
	for i, e := range second.Entities {
		if b, ok := builtFirst[entityRef{e.Kind, e.Slug}]; ok && e.FromCache {
			second.Entities[i].FromCache = false
			second.Entities[i].Requests = b.Requests
		}
		fromCache = fromCache || second.Entities[i].FromCache
	}
	switch {
	case second.Requests == 0:
		second.Source = "cache"
	case fromCache:
		second.Source = "mixed"
	default:
		second.Source = "built"
	}
	second.Persisted = second.Persisted || first.Persisted
	return second
}

// refNames renders "pages, ctas".
func refNames(refs []entityRef) string {
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		names = append(names, r.Slug)
	}
	return strings.Join(names, ", ")
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// resolveEntityArg turns a user-supplied slug into a collection or global.
func resolveEntityArg(d *Deps, slug string) (entityRef, error) {
	if d.Manifest == nil {
		return entityRef{Kind: discovery.EntityKindCollection, Slug: slug}, nil
	}
	if _, ok := d.Manifest.Collection(slug); ok {
		return entityRef{Kind: discovery.EntityKindCollection, Slug: slug}, nil
	}
	if _, ok := d.Manifest.Global(slug); ok {
		return entityRef{Kind: discovery.EntityKindGlobal, Slug: slug}, nil
	}
	if _, err := d.Manifest.ResolveCollection(slug); err != nil {
		return entityRef{}, err
	}
	return entityRef{}, apierr.New(apierr.CodeCollectionUnknown, "unknown collection or global %q", slug)
}

// knowledgeFor merges the census with an entity's shard.
func knowledgeFor(ctx context.Context, d *Deps, census *discovery.Census, ref entityRef) *discovery.BlockKnowledge {
	return discovery.NewBlockKnowledge(ref.Kind, ref.Slug, census, d.shardFor(ctx, ref.Slug, ref.cacheKind()))
}

// censusMaxDocs resolves the per-entity document cap: the command's flag, then
// census_max_docs / PAY_CENSUS_MAX_DOCS, then the default.
func censusMaxDocs(d *Deps, flagSet bool, flagVal int) (int, error) {
	if flagSet {
		if flagVal < 1 {
			return 0, apierr.New(apierr.CodeInvalidOption,
				"--census-max-docs must be at least 1, got %d", flagVal)
		}
		return flagVal, nil
	}
	if n := d.cfg().CensusMaxDocs; n > 0 {
		return n, nil
	}
	return discovery.DefaultCensusMaxDocs, nil
}

// ---------------------------------------------------------------------------
// the write path
// ---------------------------------------------------------------------------

// blockTypeWarnings implements §9.7 on top of §7.10c: every block row of a
// write body, at every depth, checked against the merged block knowledge.
//
// It replaced a check that warned about every blockType it could not confirm,
// which on a project with GraphQL introspection disabled meant every
// blockType of every write. Now:
//   - a DECLARED fact (profile pin, project source, a fully resolved GraphQL
//     union or enum) that the row contradicts is warned about as before;
//   - an OBSERVED fact (the cached census) produces a warning only for a
//     genuine anomaly: a blockType never seen in this project, one never seen
//     at this position, a key no real row of that type carries, a value
//     outside an observed vocabulary, an always-filled field left empty;
//   - no fact at all produces ONE blocks_unchecked warning naming the command
//     that makes the check possible.
//
// Every finding is a warning here: a write is never blocked on observation.
// `pay blocks validate` reports the same findings with severities.
func blockTypeWarnings(d *Deps, t *collTarget, shard *discovery.Shard, body map[string]any) []output.Warning {
	return blockTypeWarningsWith(d, t, shard, body, nil)
}

// blockTypeWarningsWith is blockTypeWarnings with the document's current
// server state, when the command already fetched it (a plan's or upsert's
// match, a dry-run's diff base). The census is a snapshot; during a migration
// the site changes under it — often through PayCLI itself — and a row the
// server already holds at exactly this position, with exactly these keys and
// values, is no anomaly however old the census is. The pre-image is folded in
// as additional observations (discovery.OverlayDocs) at no request cost.
//
// The check runs when the body holds a block row, or writes an array into a
// field the knowledge says is a blocks field (discovery.TouchesBlocks): rows
// that ALL lack a blockType (`layout=[{}]`) are the worst case — Payload
// drops every one with a 2xx and the layout is gone — and no row being typed
// is exactly why the old ContainsBlockRows gate let them through.
func blockTypeWarningsWith(d *Deps, t *collTarget, shard *discovery.Shard, body, current map[string]any) []output.Warning {
	if t == nil || !discovery.ContainsArray(body) {
		return nil
	}
	ref := entityRef{Kind: discovery.EntityKindCollection, Slug: t.Slug}
	if t.Coll == nil && d != nil && d.Manifest != nil {
		if _, isGlobal := d.Manifest.Global(t.Slug); isGlobal {
			ref.Kind = discovery.EntityKindGlobal
		}
	}
	var census *discovery.Census
	if d != nil && d.RT != nil {
		if c, ok := d.RT.cachedCensus(); ok && censusUsableForWrites(c, ref, d.RT.Now()) {
			census = c
			if current != nil {
				if e, ok := c.Entity(ref.Kind, ref.Slug); ok {
					census = c.WithEntity(discovery.OverlayDocs(e, false, current))
				}
			}
		}
	}
	k := discovery.NewBlockKnowledge(ref.Kind, ref.Slug, census, shard)
	if !discovery.TouchesBlocks(body, k) {
		return nil
	}
	res := discovery.ValidateBlocks(body, k)
	out := issuesToWarnings(res.Issues)
	if w := rowRichTextStrings(body, k); w != nil {
		out = append(out, *w)
	}
	if k.Entity != nil && d != nil && d.RT != nil {
		// Observed facts age: a block type deployed after the census reads as
		// unknown until the census is rebuilt, so every observation-based
		// warning says how old its evidence is and names the command that
		// renews it FIRST — the other command in the hint answers from the
		// same census and would repeat the stale answer.
		age := d.RT.Now().Sub(k.Entity.BuiltAt).Truncate(time.Minute)
		for i := range out {
			switch out[i].Code {
			case discovery.IssueUnknownBlockType, discovery.IssueBlockTypeNotObserved,
				discovery.IssueUnknownField, discovery.IssueValueNotInEnum, discovery.IssueLikelyRequired:
				out[i].Hint = fmt.Sprintf("`pay blocks learn %s` first if the site's blocks changed since the block census "+
					"was read (%s ago); then %s", ref.Slug, age, out[i].Hint)
			}
		}
	}
	if !k.HasObservedBlocks() && !declaredFor(k, body) {
		why := "no block census is cached for " + ref.Slug
		if k.Entity != nil {
			stopped := ""
			if k.Entity.StoppedEarly {
				stopped = " and stopped early"
			}
			why = fmt.Sprintf("the block census of %s holds no block row (it read %d document(s)%s)",
				ref.Slug, k.Entity.DocsScanned, stopped)
		}
		out = append(out, output.Warning{
			Code: warnBlocksUnchecked,
			Message: fmt.Sprintf("the blockTypes and block fields in this body were not checked: %s "+
				"and no declared source (profile pin, project source, GraphQL introspection) lists its blocks. "+
				"Payload silently drops a row whose blockType it does not recognise and still answers 2xx.", why),
			Hint: "pay blocks learn " + ref.Slug,
		})
	}
	return out
}

// blockCheckCodes are the warning codes blockTypeWarnings produces.
var blockCheckCodes = map[string]bool{
	discovery.IssueEmptyRow: true, discovery.IssueMissingBlockType: true, discovery.IssueInvalidRow: true,
	discovery.IssueUnknownBlockType: true, discovery.IssueBlockTypeNotObserved: true, discovery.IssueUnknownField: true,
	discovery.IssueValueNotInEnum: true, discovery.IssueLikelyRequired: true, discovery.IssueRequiredMissing: true,
	warnBlocksUnchecked: true, warnRichTextAsString: true,
}

// refineBlockWarnings recomputes the block-row warnings of a body once the
// document's current server state is known — the body was checked while it
// was being built, before any read — and replaces the earlier ones.
func refineBlockWarnings(d *Deps, t *collTarget, shard *discovery.Shard, body, current map[string]any,
	warnings []output.Warning) []output.Warning {
	if current == nil || !discovery.ContainsArray(body) {
		return warnings
	}
	kept := make([]output.Warning, 0, len(warnings))
	for _, w := range warnings {
		if !blockCheckCodes[w.Code] {
			kept = append(kept, w)
		}
	}
	return append(kept, blockTypeWarningsWith(d, t, shard, body, current)...)
}

// censusUsableForWrites accepts this entity's census within the 7-day hard
// max. Another entity's census is not enough: a slug is not globally unique
// (forms' `text` field block vs a page's `text` block), and "never observed"
// against an entity nobody read would be a guess.
func censusUsableForWrites(c *discovery.Census, ref entityRef, now time.Time) bool {
	e, ok := c.Entity(ref.Kind, ref.Slug)
	if !ok {
		return false
	}
	fresh, _ := cache.Evaluate(cache.ClassBlockCensus, e.BuiltAt, now)
	return fresh != cache.FreshnessExpired
}

// declaredFor reports whether every top-level blocks field the body writes has
// an authoritative accepted set. A declared blocks field counts even when no
// row of it carries a blockType (`layout=[{}]`).
func declaredFor(k *discovery.BlockKnowledge, body map[string]any) bool {
	found := false
	for key, v := range body {
		list, ok := v.([]any)
		declared := false
		if ok && k != nil {
			_, declared = discovery.DeclaredBlockField(k.Shard, key)
		}
		if !ok || (!declared && !discovery.ContainsBlockRows(list)) {
			if discovery.ContainsBlockRows(v) {
				return false
			}
			continue
		}
		found = true
		if !k.Accepted(key).Authoritative {
			return false
		}
	}
	return found
}

// issuesToWarnings folds validation issues into envelope warnings, one per
// code, so a page with forty rows does not print forty warnings.
func issuesToWarnings(issues []discovery.BlockIssue) []output.Warning {
	if len(issues) == 0 {
		return nil
	}
	byCode := map[string][]discovery.BlockIssue{}
	var order []string
	for _, is := range issues {
		if _, seen := byCode[is.Code]; !seen {
			order = append(order, is.Code)
		}
		byCode[is.Code] = append(byCode[is.Code], is)
	}
	out := make([]output.Warning, 0, len(order))
	for _, code := range order {
		list := byCode[code]
		w := output.Warning{Code: code, Hint: list[0].Hint}
		const shown = 3
		msgs := make([]string, 0, shown)
		for i, is := range list {
			w.Paths = append(w.Paths, is.Path)
			if i < shown {
				msgs = append(msgs, is.Message)
			}
		}
		w.Message = strings.Join(msgs, " | ")
		if len(list) > shown {
			w.Message += fmt.Sprintf(" | … and %d more (pay blocks validate lists every one)", len(list)-shown)
		}
		out = append(out, w)
	}
	return out
}

// declaredAccepts returns a top-level blocks field's slugs only when every one
// of them comes from a source that is not a guess — the one case in which the
// edit pipeline may refuse a blockType locally (block_type_unknown, exit 10).
// Slugs inferred from a GraphQL interfaceName (callToActionBlock for the real
// `cta`) or observed in three sampled documents are hints, never a gate.
func declaredAccepts(shard *discovery.Shard, path string) ([]string, bool) {
	if shard == nil {
		return nil, false
	}
	bf, ok := discovery.DeclaredBlockField(shard, path)
	if !ok || len(bf.Slugs) == 0 || len(bf.Unresolved) > 0 {
		return nil, false
	}
	for _, s := range bf.Slugs {
		src := bf.SlugSources[s]
		if src == "" {
			src = bf.Source
		}
		switch src {
		case discovery.SourceUnionInferred, discovery.SourceObserved, discovery.SourceObservedCensus,
			discovery.SourceMixed, discovery.SourceUnknown, "":
			return nil, false
		}
	}
	return bf.Slugs, true
}

// sortedEntityRefs orders refs collections first.
func sortedEntityRefs(in []entityRef) []entityRef {
	out := append([]entityRef{}, in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind == discovery.EntityKindCollection
		}
		return out[i].Slug < out[j].Slug
	})
	return out
}

// ---------------------------------------------------------------------------
// the describe fallback
// ---------------------------------------------------------------------------

// lazyCensus yields the block knowledge for `pay describe`'s entity, or nil.
// It runs at most once and only when a describe answer actually needs it.
type lazyCensus func() (*discovery.BlockKnowledge, *censusReport)

// describeCensus returns the census `pay describe --field/--block` falls back
// to when GraphQL and the project source cannot answer (§7.10c).
//
// A cached census is always used. A missing one is built — for this one
// entity, bounded by census_max_docs — only when GraphQL introspection is not
// available, because that is exactly when describe has nothing else: with
// introspection on, the answer describe already prints comes from the schema,
// and `pay blocks types` builds the census when it is wanted.
func (rt *Runtime) describeCensus(ctx context.Context, m *discovery.Manifest, slug, targetKind string, shard *discovery.Shard) lazyCensus {
	var once sync.Once
	var k *discovery.BlockKnowledge
	var rep *censusReport
	kind := discovery.EntityKindCollection
	if targetKind == "global" {
		kind = discovery.EntityKindGlobal
	}
	return func() (*discovery.BlockKnowledge, *censusReport) {
		once.Do(func() {
			c, cached := rt.cachedCensus()
			if cached {
				if _, has := c.Entity(kind, slug); has {
					k = discovery.NewBlockKnowledge(kind, slug, c, shard)
					return
				}
			}
			if m == nil || m.Capabilities.GraphQL.Mode == discovery.GraphQLModeOK || rt.Cfg == nil {
				if cached {
					k = discovery.NewBlockKnowledge(kind, slug, c, shard)
				}
				return
			}
			client, err := rt.Client(ctx)
			if err != nil {
				return
			}
			d := &Deps{RT: rt, Client: client, Manifest: m}
			built, report, err := ensureCensus(ctx, d, censusOpts{
				only: []entityRef{{Kind: kind, Slug: slug}}, maxDocs: rt.Cfg.CensusMaxDocs})
			if err != nil {
				rt.Warn(output.Warning{Code: warnCensusEntityFailed,
					Message: "the block census could not be built for " + slug + ": " + err.Error(),
					Hint:    "pay blocks learn " + slug})
				return
			}
			k, rep = discovery.NewBlockKnowledge(kind, slug, built, shard), report
		})
		return k, rep
	}
}

// describeFieldFromCensus answers `pay describe <e> --field PATH` from the
// census when the schema cannot: a nested blocks path (layout[].blocks,
// group.blocks), or a top-level blocks field whose slugs no declared source
// resolved (on a project with introspection disabled, `layout` is only an
// array to the three-document sample). It returns nil to let describe answer
// normally.
func describeFieldFromCensus(slug, targetKind string, shard *discovery.Shard, path string,
	header map[string]any, field discovery.Field, inShard bool, census lazyCensus) *output.Envelope {
	if inShard {
		if bf, ok := discovery.DeclaredBlockField(shard, path); ok && len(bf.Slugs) > 0 {
			return nil
		}
		if field.PayloadType != discovery.TypeBlocks && field.PayloadType != discovery.TypeUnknown &&
			field.PayloadType != discovery.TypeArray {
			return nil
		}
	}
	if census == nil {
		return nil
	}
	k, report := census()
	if k == nil {
		return nil
	}
	pattern := discovery.NormalizeBlockPath(path)
	a := k.Accepted(pattern)
	if len(a.Types) == 0 {
		return nil
	}
	decorateAccepted(k, a)
	var fieldOut any = field
	if !inShard {
		fieldOut = map[string]any{
			"path": pattern, "payload_type": discovery.TypeBlocks,
			"payload_type_source": discovery.SourceObservedCensus,
		}
	}
	sources := map[string][]string{}
	for _, t := range a.Types {
		sources[t.Slug] = t.Sources
	}
	data := map[string]any{
		"kind":               targetKind,
		"entity":             header,
		"field":              fieldOut,
		"block_types":        a.Slugs(),
		"blocks_source":      a.Source,
		"block_type_sources": sources,
		"blocks_reason":      a.Reason,
		"accepted":           a,
		"blocks_help": fmt.Sprintf("answered by the block census (observed in real documents). "+
			"pay blocks types %s --field %s lists counts; pay blocks schema <blockType> --collection %s shows what is inside one",
			slug, pattern, slug),
	}
	if report != nil {
		data["census"] = report
	}
	env := output.New("describe", output.KindSchema, data)
	env.WithTarget(&output.Target{Kind: targetKind, Slug: slug})
	return env
}

// describeBlockFromCensus answers `pay describe <e> --block TYPE` from the
// census when no declared schema exists for the type. The `block` key then
// holds the census view (fields with kind, presence, observed values and
// required-ness), marked by block_source.
func describeBlockFromCensus(slug, targetKind, block, fieldPath string, header map[string]any, census lazyCensus) *output.Envelope {
	if census == nil {
		return nil
	}
	k, report := census()
	if k == nil {
		return nil
	}
	var stats *discovery.CensusType
	if k.Entity != nil {
		stats = k.Entity.Types[block]
	}
	var gql *discovery.BlockTypeSchema
	if s, ok := k.GraphQLSchema(block); ok {
		gql = &s
	}
	if stats == nil && gql == nil {
		return nil
	}
	uses := []discovery.BlockUse{}
	paths := []string{}
	if stats != nil {
		pattern := ""
		if fieldPath != "" {
			pattern = discovery.NormalizeBlockPath(fieldPath)
		}
		for _, p := range sortedCountKeysOf(stats.UsedIn) {
			if pattern != "" && p != pattern {
				continue
			}
			uses = append(uses, discovery.BlockUse{Kind: k.Kind, Slug: k.Slug, Path: p, Count: stats.UsedIn[p]})
			paths = append(paths, p)
		}
		if pattern != "" && len(uses) == 0 {
			return nil
		}
	}
	view := discovery.BuildSchemaView(block, stats, gql, uses)
	source := discovery.SourceObservedCensus
	if stats == nil {
		source = discovery.SourceGraphQL
	}
	data := map[string]any{
		"kind":            targetKind,
		"entity":          header,
		"block":           view,
		"block_source":    source,
		"block_fields":    paths,
		"required_fields": view.Required,
		"likely_required": view.Likely,
		"examples": []string{
			fmt.Sprintf("pay blocks example %s --collection %s", block, slug),
			fmt.Sprintf("pay blocks new %s --collection %s", block, slug),
		},
	}
	if report != nil {
		data["census"] = report
	}
	env := output.New("describe", output.KindSchema, data)
	env.WithTarget(&output.Target{Kind: targetKind, Slug: slug})
	return env
}

// reconcileWithCensus replaces a describe answer whose slugs were inferred
// from GraphQL interfaceNames with the census's reconciled set — e.g.
// callToActionBlock → cta, matched to CallToActionBlock by name and confirmed
// by real documents. It reports whether it changed the answer.
func reconcileWithCensus(data map[string]any, bf discovery.BlockField, path string, census lazyCensus) bool {
	if census == nil || bf.Source == discovery.SourceConfigured || bf.Source == discovery.SourceProjectSource {
		return false
	}
	k, report := census()
	if k == nil || k.Entity == nil {
		return false
	}
	a := k.Accepted(path)
	if len(a.Types) == 0 {
		return false
	}
	sources := map[string][]string{}
	for _, t := range a.Types {
		sources[t.Slug] = t.Sources
	}
	data["declared_block_types"] = bf.Slugs
	data["block_types"] = a.Slugs()
	data["blocks_source"] = a.Source
	data["block_type_sources"] = sources
	data["blocks_reason"] = a.Reason
	data["accepted"] = a
	if report != nil {
		data["census"] = report
	}
	return true
}

func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ---------------------------------------------------------------------------
// folding PayCLI's own writes into the census
// ---------------------------------------------------------------------------

// censusFold is one document a successful write echoed.
type censusFold struct {
	kind, slug string
	doc        map[string]any
}

// foldIntoCensus remembers the echo of a successful write so the cached
// census learns the shapes PayCLI itself just created (a questions block
// moved into a group, a field left empty on purpose). Without it, every
// restructure made each later write of the same pages warn about content that
// is live on the server until someone ran `pay blocks learn`.
//
// Only an echo read like the census reads documents is folded: depth 0 (at
// depth ≥1 a relationship id comes back as a whole document and would read as
// a group) and one locale (locale=all returns every localized field as a
// locale map). The fold is applied when the invocation ends (flushCensusFolds).
func (d *Deps) foldIntoCensus(kind, slug string, p query.Params, doc map[string]any) {
	if d == nil || d.RT == nil || doc == nil || p.Locale == "all" || (p.Depth != nil && *p.Depth != 0) {
		return
	}
	if !discovery.ContainsBlockRows(doc) {
		return
	}
	rt := d.RT
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.censusFolds = append(rt.censusFolds, censusFold{kind: kind, slug: slug, doc: doc})
}

// flushCensusFolds applies the remembered echoes to the census on disk, under
// the scope lock, to whatever version of each entity is there NOW (another
// process may have rebuilt it meanwhile). An entity that is not cached, failed
// or expired is left alone: one document is not a census. It runs after the
// envelope was rendered, so a failure is only logged.
func (rt *Runtime) flushCensusFolds() {
	rt.mu.Lock()
	folds := rt.censusFolds
	rt.censusFolds = nil
	rt.mu.Unlock()
	if len(folds) == 0 || rt.Cfg == nil || rt.Cfg.NoCache {
		return
	}
	sc, err := rt.Scope()
	if err != nil {
		return
	}
	now := rt.Now()
	byEntity := map[entityRef][]map[string]any{}
	var order []entityRef
	for _, f := range folds {
		ref := entityRef{Kind: f.kind, Slug: f.slug}
		if _, seen := byEntity[ref]; !seen {
			order = append(order, ref)
		}
		byEntity[ref] = append(byEntity[ref], f.doc)
	}
	_, warn := rt.Cache().UpdateCensus(sc, func(raw []byte) (any, error) {
		if raw == nil {
			return nil, nil
		}
		c, ok := discovery.DecodeCensus(raw)
		if !ok {
			return nil, nil
		}
		changed := false
		for _, ref := range order {
			e, ok := c.Entity(ref.Kind, ref.Slug)
			if !ok || e.Error != "" {
				continue
			}
			if fresh, _ := cache.Evaluate(cache.ClassBlockCensus, e.BuiltAt, now); fresh == cache.FreshnessExpired {
				continue
			}
			c.Put(discovery.OverlayDocs(e, true, byEntity[ref]...))
			changed = true
		}
		if !changed {
			return nil, nil
		}
		c.UpdatedAt = now.UTC().Truncate(time.Second)
		return c, nil
	})
	if warn != nil {
		rt.Log.Debug("census fold not persisted", "code", warn.Code, "detail", warn.Message)
	}
}
