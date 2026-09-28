package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/rows"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

// ---------------------------------------------------------------------------
// §7.10c the block catalog: pay blocks types | schema | example | new |
// validate | learn
// ---------------------------------------------------------------------------
//
// The stdin verbs (ls, mv, rm, add, cp, set) edit a document someone already
// has. These six answer the questions an agent has BEFORE it can write one —
// which blocks exist where, what is inside each, what a real one looks like —
// from the live site alone, without GraphQL introspection and without the
// project's source. None of them reads stdin except `validate`, and none of
// them writes anything to Payload.

// blocksCatalogCmds returns the catalog verbs for registration under
// `pay blocks`.
func blocksCatalogCmds(rt *Runtime) []*cobra.Command {
	cmds := []*cobra.Command{
		newBlocksTypesCmd(rt),
		newBlocksSchemaCmd(rt),
		newBlocksExampleCmd(rt),
		newBlocksNewCmd(rt),
		newBlocksValidateCmd(rt),
		newBlocksLearnCmd(rt),
	}
	registerCatalogCompletions(rt, cmds)
	return cmds
}

// withCatalogHelp extends the family's help with the catalog verbs.
func withCatalogHelp(h *Help) *Help {
	h.Synopsis = append(h.Synopsis,
		"pay blocks types    [COLLECTION] [--field PATH]                      # which blocks exist where",
		"pay blocks schema   <blockType> [--collection C] [--field PATH]      # what is inside one",
		"pay blocks example  <blockType> [--collection C] [--n N]             # real rows, ids stripped",
		"pay blocks new      <blockType> [--collection C] [--set k=v] [--from-example]",
		"pay blocks validate [--data-file F | stdin] [--collection C]",
		"pay blocks learn    [COLLECTION...]                                  # rebuild the census",
	)
	h.Examples = append(h.Examples,
		Example{Why: "which block types does this site use, and where?", Cmd: "pay blocks types pages"},
		Example{Why: "what goes inside a grid block", Cmd: "pay blocks schema grid --collection pages"},
		Example{Why: "a ready-to-edit hero row", Cmd: "pay blocks new hero --collection pages --path .row"},
	)
	h.Mistakes = append(h.Mistakes, Mistake{
		Wrong: "Reading the app's source code to learn what a block contains.",
		Right: "`pay blocks schema <type>` answers from real documents (the block census), with GraphQL facts merged in when introspection is on.",
	})
	h.SeeAlso = append(h.SeeAlso, "pay blocks learn                             # rebuild the block census")
	return h
}

// catalogFlags are shared by the catalog verbs.
type catalogFlags struct {
	collection string
	field      string
	maxDocs    int
}

func (f *catalogFlags) registerMaxDocs(cmd *cobra.Command) {
	cmd.Flags().IntVar(&f.maxDocs, "census-max-docs", 0,
		"documents per collection the census reads (default: census_max_docs, 500)")
}

func (f *catalogFlags) registerScope(cmd *cobra.Command, fieldHelp string) {
	cmd.Flags().StringVar(&f.collection, "collection", "", "the collection or global to answer for (default: every one using the block)")
	cmd.Flags().StringVar(&f.field, "field", "", fieldHelp)
}

func (f *catalogFlags) maxDocsFor(cmd *cobra.Command, d *Deps) (int, error) {
	return censusMaxDocs(d, cmd.Flags().Changed("census-max-docs"), f.maxDocs)
}

// typeScopeNote documents what a per-type verb reads without --collection.
const typeScopeNote = "Without --collection, the cached census answers first: when it saw the block type, only the entities holding it are consulted " +
	"(0 requests while fresh, a rebuild of just those when stale; data.census.scope says so). Only when no cached census knows the type is every " +
	"entity that can hold blocks read — the census_scope_wide warning states that cost (stage: about 35 requests) and names --collection to read one."

var catalogExitCodes = []int{
	apierr.ExitOK, apierr.ExitInternal, apierr.ExitAuth, apierr.ExitNotFound,
	apierr.ExitValidation, apierr.ExitNetwork, apierr.ExitConfig, apierr.ExitCapability,
}

var catalogFlagInfo = map[string]FlagInfo{
	"field":           {Grammar: "PATH", Note: "a blocks field path: layout, layout.2.blocks, layout[].blocks, or a parent-scoped group.blocks; indices and selectors are ignored"},
	"census-max-docs": {Grammar: "N", Note: "caps documents read per collection; config: [defaults] census_max_docs, env PAY_CENSUS_MAX_DOCS"},
}

// ---------------------------------------------------------------------------
// pay blocks types
// ---------------------------------------------------------------------------

// typeSummary is one blockType in `pay blocks types`.
type typeSummary struct {
	Slug        string               `json:"slug"`
	Count       int                  `json:"count"`
	Docs        int                  `json:"docs"`
	UsedIn      []discovery.BlockUse `json:"used_in"`
	Nested      []nestedField        `json:"nested,omitempty"`
	Label       *string              `json:"label"`
	Description *string              `json:"description"`
	Sources     []string             `json:"sources"`
}

// nestedField is a blocks field inside a block type (`group.blocks`).
type nestedField struct {
	Field   string                   `json:"field"`
	Pattern string                   `json:"pattern"`
	Accepts []discovery.AcceptedType `json:"accepts"`
}

type typesEntity struct {
	Kind   string                   `json:"kind"`
	Slug   string                   `json:"slug"`
	Fields []*discovery.AcceptedSet `json:"fields"`
	Types  []typeSummary            `json:"types"`
}

func newBlocksTypesCmd(rt *Runtime) *cobra.Command {
	f := &catalogFlags{}
	cmd := &cobra.Command{
		Use:   "types [collection|global]",
		Short: "List the block types this site uses, where, and how often (block census)",
		Long: `Answers "which blocks exist, and where may each one go?" from the live site.

Every blocks field is listed with the blockTypes it holds — top-level fields
(layout) and nested ones (layout[].blocks, and parent-scoped group.blocks) —
with row counts and the source of each fact. Declared sources (a profile pin,
the project source, GraphQL introspection) are merged with the block census,
which reads real documents; an observed-only list may be incomplete, and says so.`,
		Args: maxArgs(1, "pay blocks types [collection] [--field PATH]"),
	}
	cmd.Flags().StringVar(&f.field, "field", "", "narrow to one blocks field path")
	f.registerMaxDocs(cmd)
	cmd.RunE = runData(rt, safety.CmdBlocksTypes, func(ctx context.Context, c *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		ctx, cancel := rt.deadlineContext(ctx)
		defer cancel()
		maxDocs, err := f.maxDocsFor(c, d)
		if err != nil {
			return nil, err
		}
		opts := censusOpts{maxDocs: maxDocs}
		if len(args) == 1 {
			ref, err := resolveEntityArg(d, args[0])
			if err != nil {
				return nil, err
			}
			opts.only = []entityRef{ref}
		} else {
			opts.wide = true
		}
		census, report, err := ensureCensus(ctx, d, opts)
		if err != nil {
			return nil, err
		}
		if opts.wide {
			report.Scope = wideScopeNote
			wideCostWarning(d, report, "", "pay blocks types %s")
		}
		refs := opts.only
		if len(refs) == 0 {
			for _, e := range report.Entities {
				refs = append(refs, entityRef{Kind: e.Kind, Slug: e.Slug})
			}
		}
		pattern := ""
		if f.field != "" {
			pattern = discovery.NormalizeBlockPath(f.field)
		}
		var entities []typesEntity
		all := map[string]bool{}
		for _, ref := range sortedEntityRefs(refs) {
			k := knowledgeFor(ctx, d, census, ref)
			te := buildTypesEntity(k, pattern)
			if len(te.Fields) == 0 && len(te.Types) == 0 && len(refs) > 1 {
				continue
			}
			for _, t := range te.Types {
				all[t.Slug] = true
			}
			entities = append(entities, te)
		}
		if pattern != "" && len(args) == 1 && (len(entities) == 0 || len(entities[0].Fields) == 0) {
			k := knowledgeFor(ctx, d, census, refs[0])
			known := knownPatterns(k)
			return nil, apierr.New(apierr.CodeInvalidOption,
				"%s has no blocks field %q (as %s)", refs[0].Slug, f.field, pattern).
				WithDidYouMean(apierr.DidYouMean(pattern, known)...).
				WithHint("the blocks fields of %s are: %s", refs[0].Slug, orNoneList(known))
		}
		if entities == nil {
			entities = []typesEntity{}
		}
		data := map[string]any{
			"entities":    entities,
			"block_types": sortedKeys(all),
			"census":      report,
		}
		env := output.New(safety.CmdBlocksTypes, output.KindOpResult, data)
		if len(args) == 1 {
			env.WithTarget(&output.Target{Kind: opts.only[0].Kind, Slug: opts.only[0].Slug})
		}
		return env, nil
	})
	SetHelp(cmd, &Help{
		Synopsis: []string{"pay blocks types [collection|global] [--field PATH] [--census-max-docs N]"},
		Args:     []ArgSpec{{Name: "entity", Type: "enum", ValuesFrom: "discovery.collections", Example: "pages"}},
		FlagInfo: catalogFlagInfo,
		Output: OutputSpec{Kind: output.KindOpResult, Skeleton: `{"entities":[{"kind":"collection","slug":"pages",
  "fields":[{"path":"layout","types":[{"slug":"hero","count":41,"sources":["observed-census"]}],"authoritative":false,"source":"observed-census"},
            {"path":"layout[].blocks","types":[{"slug":"text","count":120,…}],…}],
  "types":[{"slug":"group","count":88,"used_in":[{"path":"layout","count":88}],
            "nested":[{"field":"blocks","pattern":"group.blocks","accepts":[{"slug":"text","count":90}]}]}]}],
 "block_types":["bento","cta","group","hero",…],
 "census":{"source":"built","requests":6,"entities":[{"slug":"pages","docs_scanned":41,…}]}}`},
		ExitCodes: catalogExitCodes,
		Notes: []string{
			"`authoritative: true` means a declared source enumerates the whole list; otherwise the list is what real documents contain.",
			"The first run reads documents (see data.census.requests); later runs answer from the cache for 24 h.",
			"Without an entity every collection and global that can hold blocks is consulted (stage: 28 entities, about 35 requests the first time); " +
				"the census_scope_wide warning states the cost, and entities whose census held no blocks are reused for up to 7 days. Name the entity to read one.",
		},
		Examples: []Example{
			{Why: "every block type on pages, and where each is used", Cmd: "pay blocks types pages"},
			{Why: "just the slugs", Cmd: "pay blocks types pages --path .block_types"},
			{Why: "what may go inside a group block", Cmd: "pay blocks types pages --field group.blocks"},
			{Why: "what sits in the second level of the layout", Cmd: "pay blocks types pages --field layout[].blocks"},
			{Why: "every collection and global that uses blocks", Cmd: "pay blocks types"},
		},
		Mistakes: []Mistake{
			{Wrong: "Treating an observed list as everything the field allows.",
				Right: "With `authoritative: false` the list is what documents contain today; a type the project declares but never used is absent. Payload itself is the final judge."},
			{Wrong: "Using GraphQL interface names (CallToActionBlock) as blockType.",
				Right: "blockType is the slug a document carries (cta). The census reports exactly those; `sources` shows when a GraphQL member was matched to one."},
			{Wrong: "Grepping the app's source to learn which blocks nest inside which.",
				Right: "`.types[].nested` and the `layout[].blocks` fields answer it from real documents."},
		},
		SeeAlso: []string{"pay blocks schema <blockType>", "pay blocks learn", "pay describe <collection> --field <path>"},
	})
	return cmd
}

func buildTypesEntity(k *discovery.BlockKnowledge, pattern string) typesEntity {
	te := typesEntity{Kind: k.Kind, Slug: k.Slug, Fields: []*discovery.AcceptedSet{}, Types: []typeSummary{}}
	var patterns []string
	patterns = append(patterns, k.TopLevelPaths()...)
	if k.Entity != nil {
		for _, p := range k.Entity.PathPatterns() {
			if strings.Contains(p, "[]") || k.Entity.Paths[p].Parent != "" {
				patterns = append(patterns, p)
			}
		}
	}
	patterns = dedupe(patterns)
	if pattern != "" {
		patterns = []string{pattern}
	}
	inField := map[string]bool{}
	for _, p := range patterns {
		a := k.Accepted(p)
		if len(a.Types) == 0 && a.Source == discovery.SourceUnknown && pattern == "" {
			continue
		}
		if len(a.Types) == 0 && pattern != "" && a.Source == discovery.SourceUnknown {
			continue
		}
		decorateAccepted(k, a)
		te.Fields = append(te.Fields, a)
		for _, t := range a.Types {
			inField[t.Slug] = true
		}
	}
	for _, slug := range k.KnownTypes() {
		if pattern != "" && !inField[slug] {
			continue
		}
		te.Types = append(te.Types, summarizeType(k, slug))
	}
	return te
}

// decorateAccepted adds labels/descriptions where a declared schema has them.
func decorateAccepted(k *discovery.BlockKnowledge, a *discovery.AcceptedSet) {
	for i := range a.Types {
		if s, ok := k.GraphQLSchema(a.Types[i].Slug); ok {
			a.Types[i].Label, a.Types[i].Description = s.Label, s.Description
		}
	}
}

func summarizeType(k *discovery.BlockKnowledge, slug string) typeSummary {
	ts := typeSummary{Slug: slug, UsedIn: []discovery.BlockUse{}, Sources: []string{}}
	if k.Entity != nil {
		if t := k.Entity.Types[slug]; t != nil {
			ts.Count, ts.Docs = t.Count, t.Docs
			for _, p := range sortedCountKeysOf(t.UsedIn) {
				ts.UsedIn = append(ts.UsedIn, discovery.BlockUse{Kind: k.Kind, Slug: k.Slug, Path: p, Count: t.UsedIn[p]})
			}
			for _, name := range sortedKeysOfFields(t.Fields) {
				fld := t.Fields[name]
				if fld.Kind != discovery.CensusKindBlocks {
					continue
				}
				a := k.Accepted(slug + "." + name)
				ts.Nested = append(ts.Nested, nestedField{Field: name, Pattern: slug + "." + name, Accepts: a.Types})
			}
			ts.Sources = append(ts.Sources, discovery.SourceObservedCensus)
		}
	}
	for _, p := range k.TopLevelPaths() {
		for _, at := range k.Accepted(p).Types {
			if at.Slug != slug {
				continue
			}
			for _, s := range at.Sources {
				ts.Sources = appendUniqueString(ts.Sources, s)
			}
		}
	}
	if s, ok := k.GraphQLSchema(slug); ok {
		ts.Label, ts.Description = s.Label, s.Description
	}
	return ts
}

func knownPatterns(k *discovery.BlockKnowledge) []string {
	out := append([]string{}, k.TopLevelPaths()...)
	if k.Entity != nil {
		out = append(out, k.Entity.PathPatterns()...)
		for _, slug := range k.Entity.TypeSlugs() {
			for name, f := range k.Entity.Types[slug].Fields {
				if f.Kind == discovery.CensusKindBlocks {
					out = append(out, slug+"."+name)
				}
			}
		}
	}
	out = dedupe(out)
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// pay blocks schema
// ---------------------------------------------------------------------------

// typeScope is the set of entities a per-type command answers for.
type typeScope struct {
	census    *discovery.Census
	report    *censusReport
	knowledge []*discovery.BlockKnowledge
	stats     *discovery.CensusType
	gql       *discovery.BlockTypeSchema
	uses      []discovery.BlockUse
}

// resolveTypeScope builds the census for the requested scope and gathers every
// fact about one blockType in it.
func resolveTypeScope(ctx context.Context, cmd *cobra.Command, d *Deps, f *catalogFlags, slug string) (*typeScope, error) {
	maxDocs, err := f.maxDocsFor(cmd, d)
	if err != nil {
		return nil, err
	}
	opts := censusOpts{maxDocs: maxDocs}
	narrowed := false
	if f.collection != "" {
		ref, err := resolveEntityArg(d, f.collection)
		if err != nil {
			return nil, err
		}
		opts.only = []entityRef{ref}
	} else if holders := censusHolders(d.RT, slug); len(holders) > 0 {
		// The cached census already knows where this type lives: consult
		// (and, when stale, rebuild) only those entities instead of every
		// entity that can hold blocks.
		opts.only, narrowed = holders, true
	} else {
		opts.wide = true
	}
	census, report, err := ensureCensus(ctx, d, opts)
	if err != nil {
		return nil, err
	}
	sc, known, err := gatherTypeScope(ctx, d, f, slug, census, report, opts.only)
	if err != nil {
		return nil, err
	}
	if narrowed && sc.stats == nil && sc.gql == nil {
		// The holders no longer carry the type (the rebuild saw it removed):
		// fall back to the whole project before answering "unknown".
		wide := censusOpts{maxDocs: maxDocs, wide: true}
		census2, report2, err := ensureCensus(ctx, d, wide)
		if err != nil {
			return nil, err
		}
		report = mergeCensusReports(report, report2)
		narrowed = false
		if sc, known, err = gatherTypeScope(ctx, d, f, slug, census2, report, nil); err != nil {
			return nil, err
		}
	}
	switch {
	case narrowed:
		report.Scope = fmt.Sprintf("narrowed: the cached block census saw %q only in %s, so only %s consulted; "+
			"`pay blocks learn` re-reads every entity", slug, refNames(opts.only), pluralWord(len(opts.only), "that entity was", "those entities were"))
	case f.collection == "":
		report.Scope = wideScopeNote
		wideCostWarning(d, report, slug, "pay blocks "+cmd.Name()+" "+slug+" --collection %s")
	}
	if sc.stats == nil && sc.gql == nil {
		where := "this project"
		if f.collection != "" {
			where = f.collection
		}
		if f.field != "" {
			where += " at " + discovery.NormalizeBlockPath(f.field)
		}
		hint := "pay blocks types"
		if f.collection != "" {
			hint += " " + f.collection
		}
		return nil, apierr.New(apierr.CodeBlockTypeUnknown,
			"blockType %q was never observed in %s and no declared schema describes it", slug, where).
			WithDidYouMean(apierr.DidYouMean(slug, dedupe(known))...).
			WithHint("%s lists the block types that exist", hint)
	}
	return sc, nil
}

// gatherTypeScope collects every fact about one blockType from the census
// entities in refs (every entity the report covers when refs is empty). It
// returns the slugs it saw, for did_you_mean.
func gatherTypeScope(ctx context.Context, d *Deps, f *catalogFlags, slug string, census *discovery.Census,
	report *censusReport, refs []entityRef) (*typeScope, []string, error) {
	sc := &typeScope{census: census, report: report}
	if len(refs) == 0 {
		for _, e := range report.Entities {
			refs = append(refs, entityRef{Kind: e.Kind, Slug: e.Slug})
		}
	}
	pattern := ""
	if f.field != "" {
		pattern = discovery.NormalizeBlockPath(f.field)
	}
	type part struct {
		ref  entityRef
		t    *discovery.CensusType
		uses []discovery.BlockUse
		k    *discovery.BlockKnowledge
	}
	var parts []part
	var known []string
	var gqlFrom []*discovery.BlockKnowledge
	for _, ref := range sortedEntityRefs(refs) {
		k := knowledgeFor(ctx, d, census, ref)
		known = append(known, k.KnownTypes()...)
		if _, ok := k.GraphQLSchema(slug); ok {
			gqlFrom = append(gqlFrom, k)
		}
		if k.Entity == nil {
			continue
		}
		t := k.Entity.Types[slug]
		if t == nil {
			continue
		}
		pt := part{ref: ref, t: t, k: k}
		for _, p := range sortedCountKeysOf(t.UsedIn) {
			if pattern != "" && p != pattern && !parentScopedMatch(pattern, k, slug) {
				continue
			}
			pt.uses = append(pt.uses, discovery.BlockUse{Kind: ref.Kind, Slug: ref.Slug, Path: p, Count: t.UsedIn[p]})
		}
		if pattern != "" && len(pt.uses) == 0 {
			continue
		}
		parts = append(parts, pt)
	}
	// A slug is not globally unique (the form-builder's `text` is not a page's
	// `text`), so only observations shaped like the most-used one are merged;
	// the others are named so the caller can ask for them with --collection.
	sort.SliceStable(parts, func(i, j int) bool { return parts[i].t.Count > parts[j].t.Count })
	var kept []*discovery.CensusType
	for i, pt := range parts {
		if i > 0 && !discovery.CompatibleTypes(parts[0].t, pt.t) {
			d.RT.Warn(output.Warning{Code: "block_type_shapes_differ",
				Message: fmt.Sprintf("%q in %s has different fields from %q in %s; this answer describes %s only",
					slug, pt.ref.Slug, slug, parts[0].ref.Slug, parts[0].ref.Slug),
				Hint: fmt.Sprintf("pay blocks schema %s --collection %s", slug, pt.ref.Slug)})
			continue
		}
		kept = append(kept, pt.t)
		sc.uses = append(sc.uses, pt.uses...)
		sc.knowledge = append(sc.knowledge, pt.k)
	}
	sc.stats = discovery.MergeCensusTypes(kept...)
	switch {
	case len(parts) > 0:
		if s, ok := parts[0].k.GraphQLSchema(slug); ok {
			sc.gql = &s
		}
	case len(gqlFrom) > 0:
		s, _ := gqlFrom[0].GraphQLSchema(slug)
		sc.gql = &s
		sc.knowledge = append(sc.knowledge, gqlFrom[0])
	}
	if sc.uses == nil {
		sc.uses = []discovery.BlockUse{}
	}
	return sc, known, nil
}

// parentScopedMatch lets --field group.blocks select the rows of a type that
// sit in any group's blocks field.
func parentScopedMatch(pattern string, k *discovery.BlockKnowledge, slug string) bool {
	if strings.Contains(pattern, "[]") || !strings.Contains(pattern, ".") {
		return false
	}
	return k.Accepted(pattern).Has(slug)
}

func newBlocksSchemaCmd(rt *Runtime) *cobra.Command {
	f := &catalogFlags{}
	cmd := &cobra.Command{
		Use:   "schema <blockType>",
		Short: "Print one block type's fields: kinds, presence, observed values, required-ness (block census)",
		Long: `Answers "what goes inside this block?" without the project's source.

Every field of the block — including a group's sub-fields (section.contentWidth)
and an array's row fields (buttons[].label) — with its kind, JSON types, how
often it is present and filled in, the values it takes (enum_like marks a
select-like vocabulary), nested blocks fields and what they hold, and whether it
is required: "true"/"false" only from a declared source (GraphQL NON_NULL,
project source), "likely" when every observed row fills it in, else "unknown".`,
		Args: exactArgs(1, "pay blocks schema <blockType> [--collection C]"),
	}
	f.registerScope(cmd, "only rows at this blocks field path (layout, layout[].blocks, group.blocks)")
	f.registerMaxDocs(cmd)
	cmd.RunE = runData(rt, safety.CmdBlocksSchema, func(ctx context.Context, c *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		ctx, cancel := rt.deadlineContext(ctx)
		defer cancel()
		slug := strings.TrimSpace(args[0])
		sc, err := resolveTypeScope(ctx, c, d, f, slug)
		if err != nil {
			return nil, err
		}
		view := discovery.BuildSchemaView(slug, sc.stats, sc.gql, sc.uses)
		data := map[string]any{"schema": view, "census": sc.report}
		return output.New(safety.CmdBlocksSchema, output.KindOpResult, data), nil
	})
	SetHelp(cmd, &Help{
		Synopsis: []string{"pay blocks schema <blockType> [--collection C] [--field PATH] [--census-max-docs N]"},
		Args:     []ArgSpec{{Name: "blockType", Required: true, Type: "string", ValuesFrom: "pay blocks types", Example: "grid"}},
		FlagInfo: catalogFlagInfo,
		Notes:    []string{typeScopeNote},
		Output: OutputSpec{Kind: output.KindOpResult, Skeleton: `{"schema":{"block_type":"grid","count":12,"docs":9,
  "used_in":[{"kind":"collection","slug":"pages","path":"layout[].blocks","count":12}],
  "fields":[{"path":"columnsDesktop","kind":"scalar","json_types":["string"],"presence_pct":100,"non_null_pct":100,
             "values":[{"value":"3","count":7},{"value":"2","count":5}],"enum_like":true,"required":"unknown",…},
            {"path":"blocks","kind":"blocks","accepts":["text","image"],…}],
  "required_fields":[],"likely_required":["title"],"best_instance":{"slug":"pages","id":47,"path":"layout[1].blocks[1]"}},
 "census":{…}}`},
		ExitCodes: catalogExitCodes,
		Examples: []Example{
			{Why: "what goes inside a grid", Cmd: "pay blocks schema grid"},
			{Why: "only as it is used on pages", Cmd: "pay blocks schema grid --collection pages"},
			{Why: "the select-like fields and their values (filtering needs jq; --path is not jq)",
				Cmd: "pay blocks schema hero | jq '.data.schema.fields[] | select(.enum_like) | {path, values}'"},
			{Why: "which fields every real row fills in", Cmd: "pay blocks schema hero --path .schema.likely_required"},
			{Why: "a text block as used inside groups", Cmd: "pay blocks schema text --collection pages --field group.blocks"},
		},
		Mistakes: []Mistake{
			{Wrong: "Reading `required: \"likely\"` as a server rule.",
				Right: "It means every observed row filled the field in. Only \"true\" (GraphQL NON_NULL or project source) is a declared requirement."},
			{Wrong: "Treating `values` as the full option list.",
				Right: "It lists values real rows use. `options` (with options_source) is the declared list when GraphQL introspection publishes one."},
			{Wrong: "Adding a key that is not in `fields`.",
				Right: "Payload silently drops unknown fields. `pay blocks validate` flags them before the write."},
		},
		SeeAlso: []string{"pay blocks example <blockType>", "pay blocks new <blockType>", "pay describe <collection> --block <blockType>"},
	})
	return cmd
}

// ---------------------------------------------------------------------------
// pay blocks example
// ---------------------------------------------------------------------------

// exampleRow is one real instance.
type exampleRow struct {
	Ref discovery.CensusRef `json:"ref"`
	Row map[string]any      `json:"row"`
}

func newBlocksExampleCmd(rt *Runtime) *cobra.Command {
	f := &catalogFlags{}
	var n int
	cmd := &cobra.Command{
		Use:   "example <blockType>",
		Short: "Fetch real rows of a block type, most complete first, ids stripped",
		Long: `Reads the most complete real rows of a block type straight from their documents
(the census keeps references, never copies). Every id is stripped, so a row can
be pasted into a write as a new block.`,
		Args: exactArgs(1, "pay blocks example <blockType> [--n N]"),
	}
	f.registerScope(cmd, "only rows at this blocks field path")
	f.registerMaxDocs(cmd)
	cmd.Flags().IntVar(&n, "n", 1, "how many examples (1-8)")
	cmd.RunE = runData(rt, safety.CmdBlocksExample, func(ctx context.Context, c *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		ctx, cancel := rt.deadlineContext(ctx)
		defer cancel()
		if n < 1 || n > 8 {
			return nil, apierr.New(apierr.CodeInvalidOption, "--n must be between 1 and 8, got %d", n)
		}
		slug := strings.TrimSpace(args[0])
		sc, err := resolveTypeScope(ctx, c, d, f, slug)
		if err != nil {
			return nil, err
		}
		examples, sc, err := fetchExamplesOrRelearn(ctx, c, d, f, sc, slug, n)
		if err != nil {
			return nil, err
		}
		data := map[string]any{"block_type": slug, "examples": examples, "census": sc.report}
		return output.New(safety.CmdBlocksExample, output.KindOpResult, data), nil
	})
	SetHelp(cmd, &Help{
		Synopsis: []string{"pay blocks example <blockType> [--collection C] [--field PATH] [--n N]"},
		Args:     []ArgSpec{{Name: "blockType", Required: true, Type: "string", ValuesFrom: "pay blocks types", Example: "hero"}},
		FlagInfo: mergeFlagInfo(catalogFlagInfo, map[string]FlagInfo{"n": {Grammar: "N", Note: "1 to 8; the census keeps 8 references per block type"}}),
		Notes:    []string{typeScopeNote},
		Output: OutputSpec{Kind: output.KindOpResult, Skeleton: `{"block_type":"hero","examples":[{"ref":{"kind":"collection","slug":"pages","id":47,"path":"layout[0]","score":9},
  "row":{"blockType":"hero","title":"…","variant":"default",…}}],"census":{…}}`},
		ExitCodes: catalogExitCodes,
		Examples: []Example{
			{Why: "the most complete real hero", Cmd: "pay blocks example hero"},
			{Why: "three grids as used on pages", Cmd: "pay blocks example grid --collection pages --n 3"},
			{Why: "just the row, to start a new block from", Cmd: "pay blocks example hero --path '.examples[0].row'"},
			{Why: "where the example lives", Cmd: "pay blocks example grid --path '.examples[0].ref'"},
		},
		Mistakes: []Mistake{
			{Wrong: "Copying a row out of `pay get` with its ids.",
				Right: "A kept id makes Payload rewrite the row that owns it. `example` strips every row id at every depth (upload/relationship ids and rich-text node ids are kept)."},
			{Wrong: "Assuming the example is the current content.",
				Right: "It is fetched live (draft version where drafts exist). A row that moved since the census is skipped with example_moved; `pay blocks learn` refreshes the references."},
			{Wrong: "Expecting the whole document.",
				Right: "Only the row is returned; `.ref` names the document and path if you need the context: `pay get <slug> <id> --draft --depth 0`."},
		},
		SeeAlso: []string{"pay blocks new <blockType> --from-example", "pay blocks schema <blockType>"},
	})
	return cmd
}

// fetchExamples reads up to n referenced rows live.
func fetchExamples(ctx context.Context, d *Deps, sc *typeScope, slug, field string, n int,
	warn func(output.Warning)) ([]exampleRow, error) {
	pattern := ""
	if field != "" {
		pattern = discovery.NormalizeBlockPath(field)
	}
	var refs []discovery.CensusRef
	if sc.stats != nil {
		for _, r := range sc.stats.Instances {
			if pattern != "" && discovery.NormalizeBlockPath(r.Path) != pattern+"[]" {
				continue
			}
			refs = append(refs, r)
		}
	}
	out := []exampleRow{}
	docs := map[string]map[string]any{}
	var lastErr error
	for _, ref := range refs {
		if len(out) >= n {
			break
		}
		key := ref.Kind + "/" + ref.Slug + "/" + fmt.Sprint(ref.ID)
		doc, ok := docs[key]
		if !ok {
			var err error
			doc, err = fetchCensusDoc(ctx, d, sc.census, ref)
			if err != nil {
				lastErr = err
				warn(output.Warning{Code: warnExampleMoved,
					Message: fmt.Sprintf("the example at %s %v %s could not be read: %s", ref.Slug, ref.ID, ref.Path, err),
					Hint:    "pay blocks learn " + ref.Slug})
				continue
			}
			docs[key] = doc
		}
		v, found := discovery.ValueAtRowPath(doc, ref.Path)
		row, isRow := v.(map[string]any)
		if !found || !isRow || row["blockType"] != slug {
			warn(output.Warning{Code: warnExampleMoved,
				Message: fmt.Sprintf("%s %v no longer has a %s row at %s; the document changed since the census", ref.Slug, ref.ID, slug, ref.Path),
				Hint:    "pay blocks learn " + ref.Slug})
			continue
		}
		rows.StripIDs(row)
		out = append(out, exampleRow{Ref: ref, Row: row})
	}
	if len(out) == 0 && lastErr != nil {
		return nil, lastErr
	}
	if len(out) == 0 {
		return nil, apierr.New(apierr.CodeFeatureUnavailable,
			"no real %s row could be read (the census holds no reference, or every referenced row moved)", slug).
			WithHint("pay blocks learn")
	}
	return out, nil
}

// fetchExamplesOrRelearn is fetchExamples with one recovery: when no
// referenced row could be read and the references came from a CACHED census,
// the documents changed since (a restructure moved every questions row into a
// group). The entities that hold the type are then read again, once, and the
// fresh references are tried (data.census.revalidated).
func fetchExamplesOrRelearn(ctx context.Context, c *cobra.Command, d *Deps, f *catalogFlags, sc *typeScope,
	slug string, n int) ([]exampleRow, *typeScope, error) {
	var moved []output.Warning
	examples, err := fetchExamples(ctx, d, sc, slug, f.field, n, func(w output.Warning) { moved = append(moved, w) })
	giveUp := func() ([]exampleRow, *typeScope, error) {
		d.RT.Warn(moved...)
		return examples, sc, err
	}
	if err == nil || sc.report == nil || sc.report.Source != "cache" {
		return giveUp()
	}
	switch apierr.CodeOf(err) {
	case apierr.CodeFeatureUnavailable, apierr.CodeDocNotFound:
	default:
		return giveUp()
	}
	var holders []entityRef
	for _, e := range sc.report.Entities {
		holders = append(holders, entityRef{Kind: e.Kind, Slug: e.Slug})
	}
	if h := censusHolders(d.RT, slug); len(h) > 0 {
		holders = h
	}
	if len(holders) == 0 {
		return giveUp()
	}
	maxDocs, merr := f.maxDocsFor(c, d)
	if merr != nil {
		return giveUp()
	}
	census, rep, cerr := ensureCensus(ctx, d, censusOpts{only: holders, maxDocs: maxDocs, force: true})
	if cerr != nil {
		return giveUp()
	}
	rep.Revalidated = true
	fresh, _, gerr := gatherTypeScope(ctx, d, f, slug, census, rep, holders)
	if gerr != nil || fresh == nil {
		return giveUp()
	}
	// The stale references' example_moved warnings are answered by the
	// rebuild (census.revalidated); only the fresh attempt's count.
	examples, err = fetchExamples(ctx, d, fresh, slug, f.field, n, func(w output.Warning) { d.RT.Warn(w) })
	return examples, fresh, err
}

// fetchCensusDoc reads one referenced document at depth 0, as a draft when the
// census read drafts.
func fetchCensusDoc(ctx context.Context, d *Deps, census *discovery.Census, ref discovery.CensusRef) (map[string]any, error) {
	p := query.Params{Depth: query.IntPtr(0)}
	if e, ok := census.Entity(ref.Kind, ref.Slug); !ok || e.Draft {
		p.Draft = query.BoolPtr(true)
	}
	var doc payload.Doc
	var err error
	if ref.Kind == discovery.EntityKindGlobal {
		doc, _, err = d.Client.GlobalGet(ctx, ref.Slug, p)
	} else {
		doc, _, err = d.Client.Get(ctx, ref.Slug, fmt.Sprint(ref.ID), p)
	}
	if err != nil {
		return nil, err
	}
	return doc, nil
}

// ---------------------------------------------------------------------------
// pay blocks new
// ---------------------------------------------------------------------------

func newBlocksNewCmd(rt *Runtime) *cobra.Command {
	f := &catalogFlags{}
	var set, setJSON, setMD []string
	var fromExample bool
	cmd := &cobra.Command{
		Use:   "new <blockType>",
		Short: "Print a ready-to-edit row for a block type, shaped like the real ones",
		Long: `Builds a new block row from the block census: every field real rows carry, the
most common value for select-like and boolean fields, Payload's own empty
rich-text value for rich text, [] for nested blocks and arrays, null for
everything else. --from-example starts from the most complete real row instead
(ids stripped). --set/--set-json then fill it in.

The row is data.row. Nothing is written: put it into a document with
` + "`pay blocks add <type> --data @row.json`" + ` (or ` + "`--template`" + `) and ` + "`pay apply`" + `.`,
		Args: exactArgs(1, "pay blocks new <blockType> [--set k=v]"),
	}
	f.registerScope(cmd, "only learn from rows found at this blocks field path")
	f.registerMaxDocs(cmd)
	cmd.Flags().StringArrayVar(&set, "set", nil, "k=v inside the row (repeatable, typed; dotted keys reach groups)")
	cmd.Flags().StringArrayVar(&setJSON, "set-json", nil, "k=JSON inside the row (repeatable, no coercion)")
	cmd.Flags().StringArrayVar(&setMD, "set-md", nil, "k=@file.md or k=markdown inside the row: a richText field from Markdown (repeatable; applied last)")
	cmd.Flags().BoolVar(&fromExample, "from-example", false, "start from the most complete real row instead of defaults")
	cmd.RunE = runData(rt, safety.CmdBlocksNew, func(ctx context.Context, c *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		ctx, cancel := rt.deadlineContext(ctx)
		defer cancel()
		slug := strings.TrimSpace(args[0])
		sc, err := resolveTypeScope(ctx, c, d, f, slug)
		if err != nil {
			return nil, err
		}
		var row map[string]any
		var chosen []string
		source := "census"
		var exampleRef *discovery.CensusRef
		if fromExample {
			var ex []exampleRow
			ex, sc, err = fetchExamplesOrRelearn(ctx, c, d, f, sc, slug, 1)
			if err != nil {
				return nil, err
			}
			row, source, exampleRef = ex[0].Row, "example", &ex[0].Ref
		} else {
			row, chosen = discovery.NewBlockRow(slug, sc.stats, sc.gql)
			if sc.stats == nil {
				source = "graphql"
			}
		}
		if err := applyRowFlags(row, set, setJSON); err != nil {
			return nil, err
		}
		mdWarns, err := applyRowMarkdown(d, row, setMD, sc.stats, slug)
		if err != nil {
			return nil, err
		}
		row["blockType"] = slug
		view := discovery.BuildSchemaView(slug, sc.stats, sc.gql, sc.uses)
		data := map[string]any{
			"block_type":           slug,
			"row":                  row,
			"source":               source,
			"defaults_from_values": orEmptyStrings(chosen),
			"fill_in":              fillIn(view, row),
			"used_in":              sc.uses,
			"census":               sc.report,
		}
		if exampleRef != nil {
			data["example_ref"] = exampleRef
		}
		env := output.New(safety.CmdBlocksNew, output.KindOpResult, data)
		if f.collection != "" {
			// The entity the row was shaped for, so `| pay blocks validate`
			// checks it against that census without --collection again.
			if ref, err := resolveEntityArg(d, f.collection); err == nil {
				env.WithTarget(&output.Target{Kind: ref.Kind, Slug: ref.Slug})
			}
		}
		for _, w := range mdWarns {
			env.AddWarning(w)
		}
		return env, nil
	})
	SetHelp(cmd, &Help{
		Synopsis: []string{
			"pay blocks new <blockType> [--collection C] [--field PATH] [--set k=v ...] [--set-json k=JSON ...] [--set-md k=@file.md ...] [--from-example]",
		},
		Args: []ArgSpec{{Name: "blockType", Required: true, Type: "string", ValuesFrom: "pay blocks types", Example: "hero"}},
		FlagInfo: mergeFlagInfo(catalogFlagInfo, map[string]FlagInfo{
			"set":      {Grammar: "KEY=VALUE", Repeatable: true, Note: "typed like --where values; dotted keys reach group fields (section.contentWidth=inhalt)"},
			"set-json": {Grammar: "KEY=JSON", Repeatable: true},
			"set-md":   {Grammar: setMDGrammar, Repeatable: true, Note: "a richText field of the row from Markdown; replaces the template's empty rich text"},
		}),
		Output: OutputSpec{Kind: output.KindOpResult, Skeleton: `{"block_type":"hero","source":"census",
  "row":{"blockType":"hero","blockName":null,"title":null,"variant":"default","content":{"root":{…}},"buttons":[],…},
  "defaults_from_values":["variant","textSize"],"fill_in":["title"],"used_in":[…],"census":{…}}`},
		ExitCodes: catalogExitCodes,
		Notes: []string{
			"`fill_in` lists the fields every real row fills in (or a declared schema requires) that the new row leaves empty.",
			"`defaults_from_values` lists the fields set to their most common observed value; change them if that is not what you want.",
			typeScopeNote,
		},
		Examples: []Example{
			{Why: "a hero row shaped like the real ones", Cmd: "pay blocks new hero --collection pages"},
			{Why: "with the title filled in", Cmd: "pay blocks new text --set heading=Kennzahlen --set section.contentWidth=inhalt"},
			{Why: "with its rich text written from Markdown", Cmd: "pay blocks new text --set heading=Ablauf --set-md content=@ablauf.md"},
			{Why: "write it to a file for `pay blocks add`", Cmd: "pay blocks new hero --path .row --output raw > hero.json"},
			{Why: "add it to a page in one pipe", Cmd: "pay get pages 12 --depth 0 --draft | pay blocks add hero --data @hero.json --first | pay apply --draft --yes"},
			{Why: "start from the richest real hero instead", Cmd: "pay blocks new hero --from-example"},
		},
		Mistakes: []Mistake{
			{Wrong: "Piping `pay blocks new` into `pay apply`.",
				Right: "`new` prints a row (op_result), not a document. Save .row and hand it to `pay blocks add --data @file`, or use `pay blocks add <type> --template`."},
			{Wrong: "Leaving the `fill_in` fields empty.",
				Right: "Every observed row fills them in; an empty one renders as a broken block. `pay blocks validate` warns with likely_required."},
			{Wrong: "Assuming the defaults are the block's configured defaultValue.",
				Right: "They are the most common value in real rows (defaults_from_values names them). Payload's configured defaults are not published over REST."},
		},
		SeeAlso: []string{"pay blocks add <blockType> --template", "pay blocks schema <blockType>", "pay blocks validate"},
	})
	return cmd
}

// applyRowFlags applies --set / --set-json to a row, later wins.
func applyRowFlags(row map[string]any, set, setJSON []string) error {
	for _, pair := range set {
		key, raw, ok := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return apierr.New(apierr.CodeInvalidArgs, "--set %q is not key=value", pair)
		}
		v, err := query.TypeValue(raw)
		if err != nil {
			return err
		}
		setPath(row, key, v)
	}
	for _, pair := range setJSON {
		key, raw, ok := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return apierr.New(apierr.CodeInvalidArgs, "--set-json %q is not key=JSON", pair)
		}
		var v any
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			return apierr.New(apierr.CodeInvalidArgs, "--set-json %s: the value is not valid JSON", key).
				WithHint(`quote it for the shell: --set-json 'buttons=[{"label":"Mehr"}]'`)
		}
		setPath(row, key, v)
	}
	return nil
}

// applyRowMarkdown applies --set-md to a new row: each value replaces the
// template's (an empty editor state for a rich-text field) outright. stdin is
// free here — `pay blocks new` reads no document — so KEY=@- works.
func applyRowMarkdown(d *Deps, row map[string]any, pairs []string, stats *discovery.CensusType, blockType string) ([]output.Warning, error) {
	values, warns, err := convertSetMD(d, pairs, mdSource{flag: "--set-md", stdinFree: true})
	if err != nil {
		return nil, err
	}
	for _, mv := range values {
		w := mdTargetWarning(mv.Key, mv.Key, rowValueAt(row, mv.Key))
		if w == nil {
			w = mdKindWarning(stats, blockType, mv.Key, mv.Key)
		}
		if w != nil {
			warns = append(warns, *w)
		}
		if err := rows.SetPath(row, mv.Key, mv.State); err != nil {
			return nil, apierr.New(apierr.CodeInvalidArgs, "--set-md %s: %s", mv.Key, err.Error()).
				WithHint("a key is a path inside the new row: %s", rows.PathGrammar)
		}
	}
	return warns, nil
}

// fillIn lists the top-level fields a row leaves empty that are declared
// required or filled in on every observed row.
//
// A link-like field most rows fill in (bentoItem.link: 17 of 17 rows, eight
// different targets) is listed too: `blocks new` never copies another row's
// link into the template, so leaving it empty is the one way the new row
// would silently point nowhere.
func fillIn(view discovery.BlockSchemaView, row map[string]any) []string {
	out := []string{}
	for _, p := range append(append([]string{}, view.Required...), view.Likely...) {
		if !discoveryNonEmpty(row[p]) {
			out = append(out, p)
		}
	}
	for _, f := range view.Fields {
		if f.Depth != 0 || f.Kind != discovery.CensusKindScalar || f.NonNullPct < 50 || discoveryNonEmpty(row[f.Path]) {
			continue
		}
		for _, v := range f.Values {
			if s, ok := v.Value.(string); ok && discovery.LinkLikeValue(s) {
				out = append(out, f.Path)
				break
			}
		}
	}
	sort.Strings(out)
	return dedupe(out)
}

func discoveryNonEmpty(v any) bool {
	return discovery.ContainsContent(v)
}

// ---------------------------------------------------------------------------
// pay blocks validate
// ---------------------------------------------------------------------------

func newBlocksValidateCmd(rt *Runtime) *cobra.Command {
	f := &catalogFlags{}
	var dataFile string
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Check every block row of a document against the block census and declared schema",
		Long: `Reads ONE document (or a PayCLI envelope, or ` + "`pay blocks new`" + ` output) from
--data-file or stdin and checks every block row at every depth:

  error    {} rows and rows without a blockType; a blockType outside a DECLARED
           list (profile pin, project source, fully resolved GraphQL union); a
           value outside a GraphQL enum; a declared-required field left empty
  warning  a blockType never observed in the project, or never at that position;
           a key no real row of that type carries (Payload silently drops it); a
           value outside an observed select-like vocabulary; a field every
           observed row fills in (likely_required)

An observation can only ever produce a warning. Exit 5 when any error exists.`,
		Args: cobra.NoArgs,
	}
	cmd.Flags().StringVar(&dataFile, "data-file", "", "read the document from a file instead of stdin")
	cmd.Flags().StringVar(&f.collection, "collection", "", "the collection or global the document belongs to (default: the envelope's target, "+
		"or where a `pay blocks new` row's type is used most)")
	cmd.Flags().StringVar(&f.field, "field", "", "for a bare row or a `pay blocks new` row: the blocks field it is meant for — "+
		"layout, layout[].blocks, layout.2.blocks, layout[type:group[1]].blocks or group.blocks (default: where `pay blocks new` output "+
		"says the type is used most, else layout, or the entity's only blocks field)")
	f.registerMaxDocs(cmd)
	cmd.RunE = runLocalData(rt, safety.CmdBlocksValidate, func(ctx context.Context, c *cobra.Command, d *Deps, _ []string) (*output.Envelope, error) {
		ctx, cancel := rt.deadlineContext(ctx)
		defer cancel()
		raw, err := readValidateInput(d, dataFile)
		if err != nil {
			return nil, err
		}
		in, err := decodeValidateInput(raw)
		if err != nil {
			return nil, err
		}
		target, upstream := in.target, in.upstream
		slug := f.collection
		if slug == "" && target != nil {
			slug = target.Slug
		}
		field := f.field
		if field == "" && in.rows != nil {
			// `pay blocks new` output names where the census saw its type:
			// check the row there, not at the default `layout` (a bentoItem
			// only ever sits in a bento's blocks).
			if use, ok := topUse(in.usedIn, slug); ok {
				field = use.Path
				if slug == "" {
					slug = use.Slug
				}
			}
		}
		validate := func(k *discovery.BlockKnowledge) discovery.BlockValidation {
			if in.rows != nil {
				return discovery.ValidateRowsAt(in.rows, validateRowField(field, k), k)
			}
			return discovery.ValidateBlocks(in.doc, k)
		}
		var k *discovery.BlockKnowledge
		var report *censusReport
		if slug != "" && d.Client != nil && d.Manifest != nil {
			ref, err := resolveEntityArg(d, slug)
			if err != nil {
				return nil, err
			}
			maxDocs, err := f.maxDocsFor(c, d)
			if err != nil {
				return nil, err
			}
			census, rep, err := ensureCensus(ctx, d, censusOpts{only: []entityRef{ref}, maxDocs: maxDocs})
			if err != nil {
				return nil, err
			}
			report = rep
			k = knowledgeFor(ctx, d, census, ref)
			// An observed-only anomaly against a census that was not built by
			// this invocation may just mean the site changed since (a deploy
			// added a block type). Rebuild this one entity once and look
			// again — the reactive counterpart of §8.4's Level 3, triggered
			// only by an anomaly, so a clean document costs nothing. A census
			// read minutes ago is not rebuilt again: in an edit-and-validate
			// loop the anomaly is almost always the edit itself (a typo'd
			// key), which no rebuild can clear.
			if report.Source == "cache" && staleSensitive(validate(k)) {
				if age, recent := recentlyBuilt(k, rt.Now()); recent {
					report.RevalidateSkipped = fmt.Sprintf("the census of %s was read %s ago (less than %s), so the "+
						"observed-only findings were not re-checked against a rebuild; `pay blocks learn %s` forces one",
						ref.Slug, age, revalidateCooldown, ref.Slug)
				} else {
					census, rep, err = ensureCensus(ctx, d, censusOpts{only: []entityRef{ref}, maxDocs: maxDocs, force: true})
					if err != nil {
						return nil, err
					}
					rep.Revalidated = true
					report = rep
					k = knowledgeFor(ctx, d, census, ref)
				}
			}
			if in.rows != nil && field != "" {
				if err := checkValidateField(k, ref, field); err != nil {
					return nil, err
				}
			}
		} else {
			reason := "no collection is known for this document (pass --collection, or pipe a `pay get` envelope)"
			if slug != "" {
				reason = "no Payload profile is configured, so the block census cannot be read"
			}
			rt.Warn(output.Warning{Code: warnCensusUnavailable,
				Message: "only structural checks ran ({} rows, rows without a blockType): " + reason,
				Hint:    "pay blocks validate --collection pages --data-file page.json"})
			k = discovery.NewBlockKnowledge(discovery.EntityKindCollection, slug, nil, nil)
		}
		res := validate(k)
		data := map[string]any{
			"collection":   slug,
			"checked_rows": res.CheckedRows,
			"errors":       res.Errors,
			"warnings":     res.Warnings,
			"issues":       res.Issues,
		}
		if report != nil {
			data["census"] = report
		}
		var env *output.Envelope
		if res.Errors > 0 {
			env = output.NewError(safety.CmdBlocksValidate, apierr.New(apierr.CodeValidationFailed,
				"%d block row issue(s) are errors (%d warning(s)); data.issues lists every one", res.Errors, res.Warnings).
				WithHint("fix the error-severity issues; each names its path and the fact that produced it"))
			env.Data = data
		} else {
			env = output.New(safety.CmdBlocksValidate, output.KindOpResult, data)
		}
		if target != nil {
			env.WithTarget(target)
		}
		for _, w := range upstream {
			env.AddWarning(w)
		}
		return env, nil
	})
	SetHelp(cmd, &Help{
		Synopsis: []string{
			"pay get <collection> <id> --draft --depth 0 | pay blocks validate",
			"pay blocks validate --data-file page.json --collection pages",
		},
		FlagInfo: mergeFlagInfo(catalogFlagInfo, map[string]FlagInfo{
			"data-file": {Grammar: "FILE", Note: "a document, a PayCLI envelope, or `pay blocks new` output"},
		}),
		Output: OutputSpec{Kind: output.KindOpResult, Skeleton: `{"collection":"pages","checked_rows":38,"errors":0,"warnings":1,
 "issues":[{"severity":"warning","code":"unknown_field","path":"layout[2].blocks[0].headline","block_type":"text",
            "message":"…Payload silently drops unknown fields","hint":"pay blocks schema text","source":"observed-census"}]}`},
		ExitCodes: catalogExitCodes,
		Notes: []string{
			"Exit 5 (validation_failed) when any issue has severity error; data carries the full report either way.",
			"Codes: empty_row, missing_block_type, invalid_row, unknown_block_type, block_type_not_observed_here, unknown_field, value_not_in_enum, likely_required, required_field_missing.",
		},
		Examples: []Example{
			{Why: "check a page before editing it", Cmd: "pay get pages 47 --draft --depth 0 | pay blocks validate"},
			{Why: "check an edited file before writing it", Cmd: "pay blocks validate --data-file page.json --collection pages"},
			{Why: "check the result of an edit pipeline before applying", Cmd: "pay get pages 47 --draft --depth 0 | pay blocks add hero --template | pay blocks validate"},
			{Why: "check a new row", Cmd: "pay blocks new hero --set title=Hallo | pay blocks validate --collection pages --field layout"},
			{Why: "just the issue list", Cmd: "pay blocks validate --data-file page.json --collection pages --path .issues"},
		},
		Mistakes: []Mistake{
			{Wrong: "Treating a warning as proof the write will fail.",
				Right: "Warnings come from observation (the census). Only errors rest on a declared fact or on Payload's wire format."},
			{Wrong: "Validating a document read without --draft and writing the draft.",
				Right: "Read, validate and write the same version: --draft on both ends."},
			{Wrong: "Ignoring empty_row on a read.",
				Right: "A `{}` row is an orphan left by a nested block restructure. Remove it (`pay blocks rm`) before writing the document back."},
		},
		SeeAlso: []string{"pay blocks schema <blockType>", "pay blocks learn", "pay apply --dry-run"},
	})
	return cmd
}

// staleSensitive reports findings a newer census could make disappear.
func staleSensitive(v discovery.BlockValidation) bool {
	for _, is := range v.Issues {
		if is.Source != discovery.SourceObservedCensus {
			continue
		}
		switch is.Code {
		case discovery.IssueUnknownBlockType, discovery.IssueBlockTypeNotObserved,
			discovery.IssueUnknownField, discovery.IssueValueNotInEnum:
			return true
		}
	}
	return false
}

// readValidateInput reads --data-file or stdin.
func readValidateInput(d *Deps, dataFile string) ([]byte, error) {
	if dataFile != "" {
		b, err := os.ReadFile(dataFile)
		if err != nil {
			return nil, apierr.Wrap(err, apierr.CodeFileMissing, "--data-file %s could not be read", dataFile)
		}
		return b, nil
	}
	b, err := io.ReadAll(d.inStream())
	if err != nil {
		return nil, apierr.Wrap(err, apierr.CodeNoInput, "stdin could not be read")
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil, apierr.New(apierr.CodeNoInput,
			"`pay blocks validate` checks a document from --data-file or stdin, and neither held one").
			WithHint("pay get pages 47 --draft --depth 0 | pay blocks validate")
	}
	return b, nil
}

// validateInput is what `pay blocks validate` was given: a whole document, or
// bare block rows (a row, a row array, `pay blocks new` output, a bare-rows
// envelope) to be checked at the position --field names.
type validateInput struct {
	doc      map[string]any
	rows     []any
	target   *output.Target
	upstream []output.Warning
	// usedIn is a `pay blocks new` result's used_in: where the census saw
	// the row's type.
	usedIn []discovery.BlockUse
}

// topUse is the most-used position of a `pay blocks new` row, in slug when
// one is known.
func topUse(uses []discovery.BlockUse, slug string) (discovery.BlockUse, bool) {
	var best discovery.BlockUse
	found := false
	for _, u := range uses {
		if u.Path == "" || (slug != "" && u.Slug != slug) {
			continue
		}
		if !found || u.Count > best.Count {
			best, found = u, true
		}
	}
	return best, found
}

// decodeUses reads a `pay blocks new` result's used_in back from JSON.
func decodeUses(v any) []discovery.BlockUse {
	list, _ := v.([]any)
	out := make([]discovery.BlockUse, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		u := discovery.BlockUse{}
		u.Kind, _ = m["kind"].(string)
		u.Slug, _ = m["slug"].(string)
		u.Path, _ = m["path"].(string)
		if n, ok := m["count"].(json.Number); ok {
			c, _ := n.Int64()
			u.Count = int(c)
		}
		out = append(out, u)
	}
	return out
}

// decodeValidateInput accepts a document, an envelope (doc or `blocks new`
// op_result), or a bare row array.
func decodeValidateInput(raw []byte) (*validateInput, error) {
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, apierr.New(apierr.CodeBadRequestBody, "the input is not valid JSON")
	}
	switch t := v.(type) {
	case []any:
		return &validateInput{rows: t}, nil
	case map[string]any:
		env, ok := asEnvelope(t)
		if !ok {
			if _, isRow := t["blockType"]; isRow {
				return &validateInput{rows: []any{t}}, nil
			}
			return &validateInput{doc: t}, nil
		}
		in, err := fromEnvelope(env)
		if err != nil {
			return nil, err
		}
		out := &validateInput{target: in.Target, upstream: in.Warnings}
		switch row, isRow := in.Doc["row"].(map[string]any); {
		case in.Bare:
			out.rows = in.BareRows
		case isRow && in.Kind == output.KindOpResult:
			out.rows = []any{row}
			out.usedIn = decodeUses(in.Doc["used_in"])
		default:
			out.doc = in.Doc
		}
		return out, nil
	}
	return nil, apierr.New(apierr.CodeBadRequestBody, "the input is %s; validate needs a document or rows", jsonKindOf(v))
}

// validateRowField is the position bare rows are checked at: --field, else `layout`,
// else the entity's only top-level blocks field (a form's `fields`).
func validateRowField(field string, k *discovery.BlockKnowledge) string {
	if field != "" {
		return field
	}
	if k != nil {
		top := k.TopLevelPaths()
		has := false
		for _, p := range top {
			has = has || p == "layout"
		}
		if !has && len(top) == 1 {
			return top[0]
		}
	}
	return "layout"
}

// checkValidateField refuses a --field the entity's census and schema do not
// know, as `pay blocks types --field` does: checking a row at a position that
// does not exist would report "never observed there" for every type.
func checkValidateField(k *discovery.BlockKnowledge, ref entityRef, field string) error {
	if k == nil {
		return nil
	}
	known := knownPatterns(k)
	if len(known) == 0 {
		// Nothing is known about this entity's blocks fields at all.
		return nil
	}
	pattern := discovery.NormalizeBlockPath(field)
	a := k.Accepted(pattern)
	if len(a.Types) > 0 || a.Source != discovery.SourceUnknown {
		return nil
	}
	for _, p := range known {
		if p == pattern {
			return nil
		}
	}
	return apierr.New(apierr.CodeInvalidOption,
		"%s has no blocks field %q (as %s)", ref.Slug, field, pattern).
		WithDidYouMean(apierr.DidYouMean(pattern, known)...).
		WithHint("the blocks fields of %s are: %s", ref.Slug, orNoneList(known))
}

// revalidateCooldown is how recently an entity's census must have been read
// for validate to skip its reactive rebuild.
const revalidateCooldown = 10 * time.Minute

// recentlyBuilt reports an entity census read within revalidateCooldown.
func recentlyBuilt(k *discovery.BlockKnowledge, now time.Time) (time.Duration, bool) {
	if k == nil || k.Entity == nil || k.Entity.BuiltAt.IsZero() {
		return 0, false
	}
	age := now.Sub(k.Entity.BuiltAt)
	return age.Truncate(time.Second), age >= 0 && age < revalidateCooldown
}

// ---------------------------------------------------------------------------
// pay blocks learn
// ---------------------------------------------------------------------------

func newBlocksLearnCmd(rt *Runtime) *cobra.Command {
	f := &catalogFlags{}
	cmd := &cobra.Command{
		Use:   "learn [collection|global...]",
		Short: "Rebuild the block census now and report what changed",
		Long: `Reads the named collections and globals (default: every one that can hold
blocks) and replaces their block census, then reports what changed since the
previous census: new or vanished block types, fields a block type gained or
lost ("grid gained splitDesktop"), and new positions blocks appear in.

The other catalog verbs build the census on first use and reuse it for 24 h;
run learn after a deploy that changed blocks, or after large content edits.`,
	}
	f.registerMaxDocs(cmd)
	cmd.RunE = runData(rt, safety.CmdBlocksLearn, func(ctx context.Context, c *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		ctx, cancel := rt.deadlineContext(ctx)
		defer cancel()
		maxDocs, err := f.maxDocsFor(c, d)
		if err != nil {
			return nil, err
		}
		opts := censusOpts{force: true, maxDocs: maxDocs}
		for _, a := range args {
			ref, err := resolveEntityArg(d, a)
			if err != nil {
				return nil, err
			}
			opts.only = append(opts.only, ref)
		}
		previous := map[entityRef]*discovery.CensusEntity{}
		if old, ok := rt.cachedCensus(); ok {
			for _, e := range old.Entities() {
				previous[entityRef{e.Kind, e.Slug}] = e
			}
		}
		census, report, err := ensureCensus(ctx, d, opts)
		if err != nil {
			return nil, err
		}
		changes := []discovery.CensusChange{}
		firstRun := []string{}
		for _, e := range report.Entities {
			ref := entityRef{e.Kind, e.Slug}
			next, _ := census.Entity(e.Kind, e.Slug)
			prev := previous[ref]
			if prev == nil {
				firstRun = append(firstRun, e.Slug)
				continue
			}
			changes = append(changes, discovery.DiffCensusEntity(prev, next)...)
		}
		data := map[string]any{
			"changes":   changes,
			"first_run": firstRun,
			"census":    report,
		}
		return output.New(safety.CmdBlocksLearn, output.KindOpResult, data), nil
	})
	SetHelp(cmd, &Help{
		Synopsis:  []string{"pay blocks learn [collection|global...] [--census-max-docs N]"},
		Args:      []ArgSpec{{Name: "entity", Type: "enum", ValuesFrom: "discovery.collections", Example: "pages"}},
		FlagInfo:  catalogFlagInfo,
		Output:    OutputSpec{Kind: output.KindOpResult, Skeleton: `{"changes":[{"change":"added","what":"field","entity":"pages","block_type":"grid","path":"splitDesktop","message":"grid gained splitDesktop"}],"first_run":[],"census":{"source":"built","requests":7,…}}`},
		ExitCodes: catalogExitCodes,
		Examples: []Example{
			{Why: "rebuild everything after a deploy", Cmd: "pay blocks learn"},
			{Why: "only pages, reading every document", Cmd: "pay blocks learn pages --census-max-docs 5000"},
			{Why: "what changed", Cmd: "pay blocks learn pages --path .changes[].message"},
			{Why: "what the rebuild cost", Cmd: "pay blocks learn pages --path .census.requests"},
		},
		Mistakes: []Mistake{
			{Wrong: "Running learn before every command.",
				Right: "The catalog verbs build the census on first use and reuse it for 24 h. Learn after a deploy or a big content change."},
			{Wrong: "Expecting learn to see block types no document uses.",
				Right: "The census observes documents. Enable GraphQL introspection (see `pay doctor`) or pin blocks in the profile to declare the rest."},
			{Wrong: "Running learn with --no-cache and expecting later commands to use it.",
				Right: "--no-cache keeps the census in memory for this command only."},
		},
		SeeAlso: []string{"pay blocks types", "pay doctor", "pay cache info"},
	})
	return cmd
}

// ---------------------------------------------------------------------------
// pay blocks add --template
// ---------------------------------------------------------------------------

// templateRow builds the `pay blocks add <type> --template` base row for the
// document's collection and the field being edited. It uses the cached census
// or builds the entity's census once, exactly like `pay blocks new`.
func templateRow(ctx context.Context, d *Deps, target *output.Target, field, slug string) (map[string]any, []output.Warning, error) {
	if target == nil || target.Slug == "" {
		return nil, nil, apierr.New(apierr.CodeInvalidArgs,
			"--template needs to know the document's collection, and the piped document carries no target").
			WithHint("pipe a `pay get <collection> <id>` envelope, or use `pay blocks new %s --collection <c>` and --data @row.json", slug)
	}
	if d.Client == nil {
		return nil, nil, apierr.New(apierr.CodeFeatureUnavailable,
			"--template reads the block census, which needs a configured Payload profile").
			WithHint("pay blocks new %s --collection %s", slug, target.Slug)
	}
	ref := entityRef{Kind: discovery.EntityKindCollection, Slug: target.Slug}
	if target.Kind == "global" {
		ref.Kind = discovery.EntityKindGlobal
	}
	census, _, err := ensureCensus(ctx, d, censusOpts{only: []entityRef{ref}, maxDocs: d.cfg().CensusMaxDocs})
	if err != nil {
		return nil, nil, err
	}
	k := knowledgeFor(ctx, d, census, ref)
	stats := k.TypeStats(slug)
	var gql *discovery.BlockTypeSchema
	if s, ok := k.GraphQLSchema(slug); ok {
		gql = &s
	}
	if stats == nil && gql == nil {
		return nil, nil, apierr.New(apierr.CodeBlockTypeUnknown,
			"--template: blockType %q was never observed in %s and no declared schema describes it", slug, target.Slug).
			WithDidYouMean(apierr.DidYouMean(slug, k.KnownTypes())...).
			WithHint("pay blocks types %s", target.Slug)
	}
	row, _ := discovery.NewBlockRow(slug, stats, gql)
	var warns []output.Warning
	if field != "" {
		pattern := discovery.NormalizeBlockPath(field)
		if a := k.Accepted(pattern); len(a.Types) > 0 && !a.Has(slug) {
			warns = append(warns, output.Warning{Code: discovery.IssueBlockTypeNotObserved,
				Message: fmt.Sprintf("%s was never observed at %s (observed there: %s)", slug, pattern, strings.Join(a.Slugs(), ", ")),
				Hint:    fmt.Sprintf("pay blocks types %s --field %s", target.Slug, pattern)})
		}
	}
	return row, warns, nil
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedCountKeysOf(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if m[out[i]] != m[out[j]] {
			return m[out[i]] > m[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

func sortedKeysOfFields(m map[string]*discovery.CensusField) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func appendUniqueString(ss []string, s string) []string {
	for _, have := range ss {
		if have == s {
			return ss
		}
	}
	return append(ss, s)
}

func orNoneList(ss []string) string {
	if len(ss) == 0 {
		return "none"
	}
	return strings.Join(ss, ", ")
}
