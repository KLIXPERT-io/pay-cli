package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
)

// Shell completion from the cached block census (§9.9, §7.10c).
//
// On a project with GraphQL introspection disabled the shard publishes no
// block slug at all, so completion that only read the shard offered nothing
// exactly where an agent or a person needed it most. The census is one
// cached file per scope; completion reads it (never builds it) under the same
// 100 ms budget as every other completion.

// completionCensus is the cached census for a completion call, nil when none
// is cached or reading it blew the budget.
func (rt *Runtime) completionCensus(started time.Time) *discovery.Census {
	c, ok := rt.cachedCensus()
	if !ok || c == nil || rt.overBudget(started) {
		return nil
	}
	return c
}

// completionEntity resolves a completion's collection or global slug against
// the cached manifest. ok is false for an unknown slug or a cold cache.
func (rt *Runtime) completionEntity(slug string) (entityRef, bool) {
	m, ok := rt.CachedManifest()
	if !ok || slug == "" {
		return entityRef{}, false
	}
	if _, isColl := m.Collection(slug); isColl {
		return entityRef{Kind: discovery.EntityKindCollection, Slug: slug}, true
	}
	if _, isGlobal := m.Global(slug); isGlobal {
		return entityRef{Kind: discovery.EntityKindGlobal, Slug: slug}, true
	}
	return entityRef{}, false
}

// censusSlugCandidates turns block slugs into completion candidates.
//
// For ONE entity (k set) the candidates are the merged block knowledge of
// §7.10c: each top-level field's reconciled accepted set — where a slug
// inferred from a GraphQL interfaceName (callToActionBlock) that the census
// matched to the slug documents really carry (cta) is replaced by the real
// one, because the inferred one is exactly what Payload silently drops — plus
// every type the census saw at a nested level. Without a census that is the
// declared list, as before. For every entity (k nil), the census's slugs.
//
// A declared slug keeps the project's own label as its description; a slug
// known only from documents says so ("observed: 41 rows in pages"), so an
// observation is never presented as a declaration.
func censusSlugCandidates(prefix string, k *discovery.BlockKnowledge, entities []*discovery.CensusEntity) []string {
	type cand struct {
		declared bool
	}
	slugs := map[string]*cand{}
	var shard *discovery.Shard
	if k != nil {
		shard = k.Shard
		for _, p := range k.TopLevelPaths() {
			for _, t := range k.Accepted(p).Types {
				c := slugs[t.Slug]
				if c == nil {
					c = &cand{}
					slugs[t.Slug] = c
				}
				for _, src := range t.Sources {
					if src != discovery.SourceObservedCensus {
						c.declared = true
					}
				}
			}
		}
		if k.Entity != nil {
			entities = append(entities, k.Entity)
		}
	}
	counts := map[string]int{}
	where := map[string][]string{}
	for _, e := range entities {
		if e == nil || e.Error != "" {
			continue
		}
		for slug, t := range e.Types {
			if t == nil {
				continue
			}
			if slugs[slug] == nil {
				slugs[slug] = &cand{}
			}
			counts[slug] += t.Count
			where[slug] = append(where[slug], e.Slug)
		}
	}
	out := []string{}
	for slug, c := range slugs {
		if !strings.HasPrefix(slug, prefix) {
			continue
		}
		desc := ""
		if c.declared && shard != nil {
			desc = completionDescription(shard, k.ShardSlug(slug))
		}
		if desc == "" && counts[slug] > 0 {
			sort.Strings(where[slug])
			desc = fmt.Sprintf("\tobserved: %d %s in %s", counts[slug], pluralWord(counts[slug], "row", "rows"),
				strings.Join(where[slug], ", "))
		}
		out = append(out, slug+desc)
	}
	sort.Strings(out)
	return out
}

// CompleteCensusBlockTypes completes the <blockType> argument of
// `pay blocks schema|example|new|add`: the entity named by --collection when
// it is given, otherwise every entity the cached census read.
func CompleteCensusBlockTypes(rt *Runtime) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		started := rt.Now()
		if len(args) > 0 || !rt.completionSetup(cmd) {
			return noSuggestions()
		}
		coll := ""
		if f := cmd.Flags().Lookup("collection"); f != nil {
			coll = f.Value.String()
		}
		var k *discovery.BlockKnowledge
		var entities []*discovery.CensusEntity
		c := rt.completionCensus(started)
		if coll != "" {
			ref, ok := rt.completionEntity(coll)
			if !ok {
				return noSuggestions()
			}
			shard, _ := rt.CachedShard(ref.Slug, ref.cacheKind())
			k = discovery.NewBlockKnowledge(ref.Kind, ref.Slug, c, shard)
		} else if c != nil {
			entities = c.Entities()
		}
		if rt.overBudget(started) {
			return noSuggestions()
		}
		return censusSlugCandidates(toComplete, k, entities), cobra.ShellCompDirectiveNoFileComp
	}
}

// CompleteEntityFlag completes a flag whose value is a collection or global
// slug (`--collection`), whatever positional arguments are already given.
func CompleteEntityFlag(rt *Runtime) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return rt.completeSlugs(cmd, toComplete, true, true)
	}
}

// CompleteCensusFieldPaths completes a blocks-field --field for the catalog
// verbs and `pay describe`: the entity's declared blocks fields plus every
// pattern the census saw (`layout`, `layout[].blocks`) and the parent-scoped
// form (`group.blocks`). The entity is --collection when the command has one
// and it is set, else the first positional argument when entityArg says that
// argument IS an entity (`pay blocks types pages`, `pay describe pages`);
// otherwise (`pay blocks schema text`) every entity the census read. extra,
// when set, supplies the command's ordinary field paths as well (describe's
// --field takes any field).
func CompleteCensusFieldPaths(rt *Runtime, entityArg bool, extra func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective)) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		started := rt.Now()
		var out []string
		if extra != nil {
			out, _ = extra(cmd, args, toComplete)
		}
		if !rt.completionSetup(cmd) {
			return out, cobra.ShellCompDirectiveNoFileComp
		}
		slug := ""
		if f := cmd.Flags().Lookup("collection"); f != nil {
			slug = f.Value.String()
		}
		if slug == "" && entityArg && len(args) > 0 {
			slug = args[0]
		}
		var refs []entityRef
		if slug != "" {
			ref, ok := rt.completionEntity(slug)
			if !ok {
				return out, cobra.ShellCompDirectiveNoFileComp
			}
			refs = append(refs, ref)
		}
		set := map[string]bool{}
		for _, p := range out {
			set[p] = true
		}
		add := func(p string) {
			if strings.HasPrefix(p, toComplete) && !set[p] {
				set[p] = true
				out = append(out, p)
			}
		}
		for _, ref := range refs {
			if shard, ok := rt.CachedShard(ref.Slug, ref.cacheKind()); ok && shard != nil {
				for p := range shard.BlockFields {
					add(p)
				}
			}
		}
		if c := rt.completionCensus(started); c != nil {
			var entities []*discovery.CensusEntity
			if len(refs) == 0 {
				entities = c.Entities()
			} else if e, ok := c.Entity(refs[0].Kind, refs[0].Slug); ok {
				entities = append(entities, e)
			}
			for _, e := range entities {
				for _, p := range e.PathPatterns() {
					add(p)
				}
				for bt, t := range e.Types {
					for name, f := range t.Fields {
						if f != nil && f.Kind == discovery.CensusKindBlocks {
							add(bt + "." + name)
						}
					}
				}
			}
		}
		sort.Strings(out)
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}

// CompleteEntitiesVariadic completes collection or global slugs for a command
// that takes several (`pay blocks learn pages ctas`), skipping those given.
func CompleteEntitiesVariadic(rt *Runtime) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		out, dir := rt.completeSlugs(cmd, toComplete, true, true)
		given := map[string]bool{}
		for _, a := range args {
			given[a] = true
		}
		kept := out[:0]
		for _, c := range out {
			slug, _, _ := strings.Cut(c, "\t")
			if !given[slug] {
				kept = append(kept, c)
			}
		}
		return kept, dir
	}
}

// registerCatalogCompletions wires the `pay blocks` catalog verbs to the
// cached manifest, shards and block census.
func registerCatalogCompletions(rt *Runtime, cmds []*cobra.Command) {
	for _, cmd := range cmds {
		switch cmd.Name() {
		case "types":
			cmd.ValidArgsFunction = CompleteEntities(rt)
		case "schema", "example", "new":
			cmd.ValidArgsFunction = CompleteCensusBlockTypes(rt)
		case "learn":
			cmd.ValidArgsFunction = CompleteEntitiesVariadic(rt)
		case "validate":
			cmd.ValidArgsFunction = func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
				return noSuggestions()
			}
		}
		if cmd.Flags().Lookup("collection") != nil {
			_ = cmd.RegisterFlagCompletionFunc("collection", CompleteEntityFlag(rt))
		}
		if cmd.Flags().Lookup("field") != nil && cmd.Name() != "validate" {
			_ = cmd.RegisterFlagCompletionFunc("field", CompleteCensusFieldPaths(rt, cmd.Name() == "types", nil))
		}
	}
}
