package cli

import (
	"net/url"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
)

// §7.11's reactive operator memo.
//
// `unsupported_operators` is NOT pre-computed from a guessed adapter (§1
// conflict 36: `all` maps to $all on MongoDB and works, so the shipped list
// would make PayCLI refuse a query the server would have answered). It starts
// empty and is learned from what this project actually did: an operator that
// provably failed against collection C is appended to
// `learned_operator_failures[]` in the scope cache, and the next use of that
// operator on that collection emits `warning{code:"operator_previously_failed"}`
// BEFORE sending — a warning, never a block, because the memo is evidence and
// not a rule.
//
// Both halves lived as dead declarations until now:
// output.OperatorPreviouslyFailedWarning was called from nowhere, and
// discovery.LearnedOperatorFailures was initialised empty and never appended to.

// learnedOperatorWarnings returns the §7.11 warnings for a request that is
// about to be sent. It reads the CACHED manifest only — a memo lookup must
// never be able to trigger discovery, or a query would start paying for a
// network round trip to find out whether to warn about itself.
func (rt *Runtime) learnedOperatorWarnings(collection string, operators []string) []output.Warning {
	collection = strings.TrimSpace(collection)
	if rt == nil || collection == "" || len(operators) == 0 {
		return nil
	}
	// Cheap gate first: the overwhelming majority of queries use equals,
	// greater_than or contains, none of which the memo can ever hold.
	candidate := false
	for _, op := range operators {
		if discovery.OperatorFailureCandidate(op) {
			candidate = true
			break
		}
	}
	if !candidate {
		return nil
	}
	m, ok := rt.CachedManifest()
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	var out []output.Warning
	for _, op := range operators {
		if seen[op] {
			continue
		}
		seen[op] = true
		f, found := m.LearnedOperatorFailure(op, collection)
		if !found {
			continue
		}
		out = append(out, output.OperatorPreviouslyFailedWarning(f.Operator, f.Collection, f.Evidence))
	}
	return out
}

// rememberOperatorFailure is the write half. It appends the record to the
// scope cache's manifest.json and rewrites it atomically under the same lock
// discovery uses, leaving every field shard in place.
//
// Every failure here is silent by design: the memo is an optimisation for the
// NEXT invocation, and a read-only $CACHE must degrade this command to "no
// memory", never to "failed". The command's own result has already been
// decided by the time it is called.
func (rt *Runtime) rememberOperatorFailure(operator, collection, evidence string) bool {
	if rt == nil || rt.Cfg == nil {
		return false
	}
	operator = strings.TrimSpace(operator)
	collection = strings.TrimSpace(collection)
	if operator == "" || collection == "" {
		return false
	}
	sc, err := rt.Scope()
	if err != nil {
		return false
	}
	cm, ok, _ := rt.Cache().ReadManifest(sc)
	if !ok {
		// Nothing discovered yet: there is no manifest to append to, and
		// fabricating one here would publish an index that describes no shards.
		return false
	}
	m, decoded := discovery.DecodeManifest(cm.Raw)
	if !decoded {
		return false
	}
	if !m.RecordOperatorFailure(operator, collection, evidence, rt.Now()) {
		return false
	}
	// Keep this invocation's in-process copy in agreement, so a second failing
	// query in the same run does not re-learn what was just learned.
	if rt.disc != nil && rt.disc.manifest != nil {
		rt.disc.manifest.RecordOperatorFailure(operator, collection, evidence, rt.Now())
	}
	// --no-cache discovers in process and persists nothing (§8.5); the memo
	// obeys the same rule rather than inventing an exception to it.
	if rt.Cfg.NoCache || !rt.Cache().Enabled() {
		return false
	}
	written, _ := rt.Cache().WriteSet(sc, cache.Set{
		Generation: m.Generation,
		Manifest:   m,
	}, rt.Now())
	return written
}

// whereOperatorsInQuery pulls every Payload operator out of a raw query string.
//
// `pay raw --query` is a verbatim channel, so the only way to know which
// operators a request carries is to read them back off the wire form:
// `where[tags][all]=a,b` and `where[or][0][and][0][x][near]=…` both end in the
// operator, which is what makes the last bracket segment the one to test.
// Anything that is not a canonical Payload operator (a field name, an index) is
// ignored, so a plain `limit=1` contributes nothing.
func whereOperatorsInQuery(raw string) []string {
	if !strings.Contains(raw, "where") {
		return nil
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for key := range values {
		if !strings.HasPrefix(key, "where[") && key != "where" {
			continue
		}
		// Payload's grammar is where[path…][operator], so the operator is
		// never the FIRST bracket segment: `where[all]=1` names a field.
		if strings.Count(key, "[") < 2 || !strings.HasSuffix(key, "]") {
			continue
		}
		open := strings.LastIndex(key, "[")
		op := key[open+1 : len(key)-1]
		if !query.IsOperator(op) || seen[op] {
			continue
		}
		seen[op] = true
		out = append(out, op)
	}
	return out
}

// rawCollectionSlug names the collection a `pay raw` path addresses, or "" when
// the path is not a collection route at all (/api/access, /admin/…, a plugin
// route). The memo is keyed per collection, so guessing wrong here would file
// the evidence under a slug the next query never looks up.
func rawCollectionSlug(reqPath, apiPath string, absolute bool) string {
	p := strings.TrimSpace(reqPath)
	if absolute {
		prefix := strings.TrimSuffix(strings.TrimSpace(apiPath), "/")
		if prefix == "" || !strings.HasPrefix(p, prefix+"/") {
			return ""
		}
		p = strings.TrimPrefix(p, prefix)
	}
	p = strings.TrimPrefix(p, "/")
	slug, _, _ := strings.Cut(p, "/")
	slug, _, _ = strings.Cut(slug, "?")
	if slug == "" || strings.HasPrefix(slug, "_") {
		return ""
	}
	return slug
}

// ---------------------------------------------------------------------------
// §7.11 on the --where path
// ---------------------------------------------------------------------------

// operatorMemo is what one invocation's query carried, recorded by
// readFlags.build so runData can attribute a failure without the command
// bodies — find, count, update, delete, publish, unpublish — each repeating
// the same five lines at every error return they own.
type operatorMemo struct {
	collection string
	operators  []string
}

// rememberQueryOperators is §7.11's read half for `--where`. It records the
// query on the invocation and warns, BEFORE the request goes out, about every
// operator this project has already been seen to reject on this collection.
//
// It never blocks: the memo is evidence from one past response, not a rule,
// and `all` really does work on MongoDB (§1 conflict 36). Deciding for the
// agent on that evidence is exactly the overreach §7.11 forbids.
func (d *Deps) rememberQueryOperators(t *collTarget, operators []string) {
	// A nil RT is a unit test exercising the query builder in isolation: the
	// memo is a runtime service, so it degrades to "no memory", never to a panic.
	if d == nil || d.RT == nil || t == nil || t.Slug == "" || len(operators) == 0 {
		return
	}
	d.memo = operatorMemo{collection: t.Slug, operators: operators}
	d.RT.Warn(d.RT.learnedOperatorWarnings(t.Slug, operators)...)
}

// learnFromFailure is the write half. It runs on every data-command return,
// including the successful one, because "this query worked" is the only signal
// that can stop the memo from being learned — and a nil error is the cheapest
// possible early exit.
//
// The attribution bar is deliberately high (discovery.AttributeOperatorFailure):
// the server must have failed with a 500, and the request must have carried
// exactly ONE operator whose adapter support actually varies. Without that, a
// single flaky 502 on `slug eq home` would teach PayCLI that `equals` is broken
// on pages and warn about it forever.
func (d *Deps) learnFromFailure(err error) {
	if d == nil || d.RT == nil || err == nil || d.memo.collection == "" {
		return
	}
	serverFailed := apierr.CodeOf(err) == apierr.CodeServerError
	bad, ok := discovery.AttributeOperatorFailure(serverFailed, d.memo.operators)
	if !ok {
		return
	}
	d.RT.rememberOperatorFailure(bad, d.memo.collection, err.Error())
}
