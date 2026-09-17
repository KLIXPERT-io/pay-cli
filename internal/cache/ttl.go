package cache

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/output"
)

// Class is a §8.5 data class. Every cache entry carries its class, and the
// class alone decides both the TTL and whether the entry may ever reach disk.
type Class string

const (
	// ClassDiscoveryManifest is the §7.8.1 index: topology, capabilities, the
	// collection/global inventory.
	ClassDiscoveryManifest Class = "discovery_manifest"
	// ClassFieldShard is a §7.8.2 shard. §8.5 has no separate row for shards;
	// they are written and invalidated as part of the manifest set (§8.7) and
	// therefore share the manifest's TTL.
	ClassFieldShard Class = "field_shard"
	// ClassPermissions is the per-identity /api/access projection.
	ClassPermissions Class = "permissions"
	// ClassGraphQLType is one per-type introspection blob.
	ClassGraphQLType Class = "graphql_type"
	// ClassWhereInput is a _where input type.
	ClassWhereInput Class = "where_input"
	// ClassGraphQLMode is the GraphQL mode / capability probe result.
	ClassGraphQLMode Class = "graphql_mode"
	// ClassIdentity is /api/{auth}/me. Memory only: the body contains a
	// plaintext API key (§8.3 rule 5, verified).
	ClassIdentity Class = "identity"
	// ClassSkillsManifest covers the skills manifest and release metadata.
	ClassSkillsManifest Class = "skills_manifest"
	// ClassDocument is document data of any kind. Never cached to disk
	// (§8.3 rule 4): Payload content is mutable by definition and stale
	// content served to an agent causes wrong edits.
	ClassDocument Class = "document"
)

// TTL is one row of the §8.5 table. HardMax == 0 means there is no serve-stale
// window at all: past Fresh the entry is simply expired. (§8.5 writes "—" for
// the identity and skills rows.)
type TTL struct {
	Fresh   time.Duration
	HardMax time.Duration
	// Persist reports whether this class may ever be written to disk.
	Persist bool
}

var ttlTable = map[Class]TTL{
	ClassDiscoveryManifest: {Fresh: 10 * time.Minute, HardMax: 24 * time.Hour, Persist: true},
	ClassFieldShard:        {Fresh: 10 * time.Minute, HardMax: 24 * time.Hour, Persist: true},
	ClassPermissions:       {Fresh: 5 * time.Minute, HardMax: 24 * time.Hour, Persist: true},
	ClassGraphQLType:       {Fresh: 24 * time.Hour, HardMax: 7 * 24 * time.Hour, Persist: true},
	ClassWhereInput:        {Fresh: 24 * time.Hour, HardMax: 7 * 24 * time.Hour, Persist: true},
	ClassGraphQLMode:       {Fresh: 24 * time.Hour, HardMax: 7 * 24 * time.Hour, Persist: true},
	ClassSkillsManifest:    {Fresh: 6 * time.Hour, HardMax: 0, Persist: true},
	ClassIdentity:          {Fresh: 60 * time.Second, HardMax: 0, Persist: false},
	ClassDocument:          {Fresh: 0, HardMax: 0, Persist: false},
}

// Classes returns every known class, sorted, for `pay explain` and tests.
func Classes() []Class {
	out := make([]Class, 0, len(ttlTable))
	for c := range ttlTable {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// TTLFor returns the §8.5 row for a class. An unknown class is reported as
// not persistable with a zero freshness window, so a future caller that invents
// a class cannot accidentally get permanent caching.
func TTLFor(c Class) (TTL, bool) {
	t, ok := ttlTable[c]
	return t, ok
}

// Persistable reports whether a class may be written to disk (§8.3).
func (c Class) Persistable() bool {
	t, ok := ttlTable[c]
	return ok && t.Persist
}

// Freshness is the position of an entry on the §8.5 staleness ladder.
type Freshness string

const (
	// FreshnessFresh: use with zero network, meta.cache.discovery = "hit".
	FreshnessFresh Freshness = "fresh"
	// FreshnessStale: past fresh but within hard max — run the ladder.
	FreshnessStale Freshness = "stale"
	// FreshnessExpired: past hard max — a miss.
	FreshnessExpired Freshness = "expired"
)

// Evaluate places an entry on the ladder.
//
// A fetchedAt in the future (clock skew, or a file copied between machines) is
// treated as age zero rather than as a negative age, which would otherwise make
// an entry immortal on one side of the comparison and expired on the other.
func Evaluate(c Class, fetchedAt, now time.Time) (Freshness, time.Duration) {
	t, ok := TTLFor(c)
	if !ok || fetchedAt.IsZero() {
		return FreshnessExpired, 0
	}
	age := now.Sub(fetchedAt)
	if age < 0 {
		age = 0
	}
	switch {
	case age < t.Fresh:
		return FreshnessFresh, age
	case t.HardMax > 0 && age < t.HardMax:
		return FreshnessStale, age
	default:
		return FreshnessExpired, age
	}
}

// RevalidationBudget is §8.5's 250 ms: the longest a stale-but-valid entry may
// hold output unwritten while the Level-1 probe runs.
const RevalidationBudget = 250 * time.Millisecond

// Probe is the §8.4 Level-1 topology probe: it returns topology_sha256 for the
// current identity. It is supplied by internal/discovery so this package never
// makes a network call itself.
type Probe func(ctx context.Context) (fingerprint string, err error)

// Revalidation is the outcome of the §8.5 ladder.
type Revalidation struct {
	// Discovery is the meta.cache.discovery value: output.CacheStaleServed or
	// output.CacheRevalidated.
	Discovery string
	// Refresh is true when the caller must discard the held response,
	// re-discover and re-issue the operation (ladder step 4).
	Refresh bool
	// Confirm is true when the caller should bump meta.confirmed_at
	// (ladder step 3).
	Confirm bool
	// Warning is the revalidation_timed_out warning, set only on step 5.
	Warning *output.Warning
	// Err is the probe's error, if any. Informational: a probe error is a
	// stale-serve, never a command failure.
	Err error
	// Fingerprint is the topology hash the probe returned, when it completed.
	Fingerprint string
}

// Revalidate runs §8.5's five-step ladder for a stale entry.
//
//  1. The caller has already computed the response from the stale manifest and
//     is holding it unwritten — that is the caller's half of the contract, and
//     it is what makes the advertised warm latency honest.
//  2. Fire the Level-1 probe and wait up to budget (default 250 ms).
//  3. Hash matches   -> stale-served, bump confirmed_at.
//  4. Hash differs   -> discard, re-discover, re-issue, revalidated.
//  5. Timeout/error  -> stale-served plus the revalidation_timed_out warning.
//
// The probe goroutine writes to a buffered channel, so a timed-out probe
// finishes and exits on its own rather than leaking or blocking the command.
func Revalidate(ctx context.Context, cachedFingerprint string, probe Probe, budget time.Duration) Revalidation {
	if budget <= 0 {
		budget = RevalidationBudget
	}
	if probe == nil {
		return Revalidation{Discovery: output.CacheStaleServed, Warning: revalidationTimedOutWarning()}
	}

	pctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	type result struct {
		fp  string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		fp, err := probe(pctx)
		ch <- result{fp: fp, err: err}
	}()

	select {
	case r := <-ch:
		switch {
		case r.err != nil:
			// Step 5: an errored probe is indistinguishable from a slow one as
			// far as the caller's decision goes — serve stale, say so.
			return Revalidation{
				Discovery: output.CacheStaleServed,
				Warning:   revalidationTimedOutWarning(),
				Err:       r.err,
			}
		case r.fp == cachedFingerprint && r.fp != "":
			// Step 3.
			return Revalidation{Discovery: output.CacheStaleServed, Confirm: true, Fingerprint: r.fp}
		default:
			// Step 4. An empty fingerprint counts as "differs": a probe that
			// returned nothing has not confirmed anything.
			return Revalidation{Discovery: output.CacheRevalidated, Refresh: true, Fingerprint: r.fp}
		}
	case <-pctx.Done():
		// Step 5.
		return Revalidation{
			Discovery: output.CacheStaleServed,
			Warning:   revalidationTimedOutWarning(),
			Err:       pctx.Err(),
		}
	}
}

// Warning codes emitted by the cache layer. §10's output.Warn* constants cover
// the shared layers; these four are §8's own (§8.3, §8.5, §8.7).
const (
	// WarnCacheUnreadable accompanies every read-path degradation (§8.3).
	WarnCacheUnreadable = "cache_unreadable"
	// WarnCacheWriteFailed accompanies a skipped cache write (§8.7). A skipped
	// write is never silent: without this a read-only $CACHE makes every
	// invocation silently re-pay discovery forever.
	WarnCacheWriteFailed = "cache_write_failed"
	// WarnRevalidationTimedOut is §8.5 ladder step 5.
	WarnRevalidationTimedOut = "revalidation_timed_out"
	// WarnDiscoveryRan is §8.5's cold-cache warning.
	WarnDiscoveryRan = "discovery_ran"

	hintCachePath = "pay cache path"
)

func revalidationTimedOutWarning() *output.Warning {
	return &output.Warning{
		Code:    WarnRevalidationTimedOut,
		Message: "served from a cache entry older than its freshness window; the 250 ms revalidation probe did not complete",
		Hint:    "pay discover --refresh",
	}
}

// DiscoveryRanWarning is §8.5's cold-cache warning.
//
// §8.5 shows an "elapsed_ms" key on this warning; output.Warning (§10.1) has no
// such field and inventing one here would desynchronise the envelope schema, so
// the elapsed time is folded into the message instead.
func DiscoveryRanWarning(elapsed time.Duration) output.Warning {
	return output.Warning{
		Code:    WarnDiscoveryRan,
		Message: fmt.Sprintf("no cached schema for this profile; ran discovery first (%d ms)", elapsed.Milliseconds()),
		Hint:    "pay discover pre-warms this",
	}
}

func warning(code, message string) *output.Warning {
	return &output.Warning{Code: code, Message: message, Hint: hintCachePath}
}
