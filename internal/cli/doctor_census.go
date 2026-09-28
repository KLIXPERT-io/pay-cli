package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
)

// introspectionEnableHint is how a project turns GraphQL introspection back on
// in production. Verified in Payload 3.87.1: `graphQL.disableIntrospectionInProduction`
// (payload/dist/config/types.d.ts, `@default true`; payload/dist/config/defaults.js
// sets it to true), enforced by @payloadcms/graphql's NoProductionIntrospection
// validation rule, which rejects __schema/__type only while
// process.env.NODE_ENV === 'production' — the exact error text discovery sees:
// "GraphQL introspection is not allowed, but the query contained __schema or __type".
const introspectionEnableHint = "to enable it, set `graphQL: { disableIntrospectionInProduction: false }` in " +
	"buildConfig() in payload.config.ts and redeploy (Payload 3.x; the default is true, and the rule only applies " +
	"when NODE_ENV=production). Until then the block census stands in: pay blocks types"

// introspectionDisabledCheck says exactly what a production build without
// introspection costs, what replaces it, and how to turn it back on (§18).
func introspectionDisabledCheck(m *discovery.Manifest) Check {
	detail := "GraphQL answers queries but introspection is disabled (NODE_ENV=production), so discovery is REST-observed: " +
		"field lists come from three sampled documents (fields that were null there have no type), " +
		"required flags are unknown, and block slugs and block schemas are not published at all. " +
		"The block census (pay blocks types | schema | new | validate) reads real documents instead and labels " +
		"every fact observed-census; writes check block rows against it once it is cached."
	if m != nil && m.Capabilities.GraphQL.Detail != "" {
		detail += " Server said: " + m.Capabilities.GraphQL.Detail
	}
	return Check{Name: "graphql", Status: CheckWarn, Detail: detail,
		Hint: introspectionEnableHint, Value: discovery.GraphQLModeIntrospectionDisabled}
}

// censusCheck reports the block census for this scope: whether one is cached,
// how old it is, and which entities it covers (§7.10c).
func (rt *Runtime) censusCheck(m *discovery.Manifest, add func(Check)) {
	c, ok := rt.cachedCensus()
	degraded := m != nil && m.Capabilities.GraphQL.Mode != discovery.GraphQLModeOK
	if !ok || len(c.Entities()) == 0 {
		status := CheckOK
		detail := "no block census is cached; it is built on first use of `pay blocks types|schema|new|validate`"
		if degraded {
			status = CheckWarn
			detail = "no block census is cached, and GraphQL introspection is unavailable, so writes cannot check " +
				"block rows (they warn blocks_unchecked) until one is built"
		}
		add(Check{Name: "block_census", Status: status, Detail: detail, Hint: "pay blocks learn"})
		return
	}
	now := rt.Now()
	var withBlocks []string
	oldest := time.Duration(0)
	failed := 0
	for _, e := range c.Entities() {
		if e.Error != "" {
			failed++
		}
		if e.BlockRows > 0 {
			withBlocks = append(withBlocks, fmt.Sprintf("%s (%d types)", e.Slug, len(e.Types)))
		}
		if age := now.Sub(e.BuiltAt); age > oldest {
			oldest = age
		}
	}
	sort.Strings(withBlocks)
	status, hint := CheckOK, ""
	if fresh, _ := cache.Evaluate(cache.ClassBlockCensus, now.Add(-oldest), now); fresh != cache.FreshnessFresh {
		status, hint = CheckWarn, "pay blocks learn"
	}
	if failed > 0 {
		status, hint = CheckWarn, "pay blocks learn"
	}
	detail := fmt.Sprintf("%d entities censused, oldest %s ago; blocks in: %s",
		len(c.Entities()), oldest.Truncate(time.Second), strings.Join(withBlocks, ", "))
	if failed > 0 {
		detail += fmt.Sprintf("; %d entity read(s) failed", failed)
	}
	sc, _ := rt.Scope()
	add(Check{Name: "block_census", Status: status, Detail: detail, Hint: hint,
		Value: rt.Cache().CensusPath(sc)})
}
