package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

func init() { Register(newDescribeCmd) }

type describeFlags struct {
	field        string
	block        string
	blocksDetail bool
	requiredOnly bool
	queryable    bool
	examples     bool
	sample       bool
}

func newDescribeCmd(rt *Runtime) *cobra.Command {
	f := &describeFlags{}

	cmd := &cobra.Command{
		Use:               "describe <collection|global>",
		Short:             "Print one entity's full field schema: types, operators, required, relations.",
		GroupID:           GroupDiscovery,
		Args:              exactArgs(1, "pay describe <collection|global> [--field PATH]"),
		ValidArgsFunction: CompleteEntities(rt),
		RunE: Handle(rt, "describe", func(ctx context.Context, rt *Runtime, args []string) (*output.Envelope, error) {
			ctx, cancel := rt.deadlineContext(ctx)
			defer cancel()
			slug := args[0]
			m, err := rt.Discovery(ctx)
			if err != nil {
				return nil, err
			}

			kind := cache.KindCollection
			var header map[string]any
			var targetKind string
			if coll, ok := m.Collection(slug); ok {
				targetKind = "collection"
				header = collectionHeader(coll)
			} else if glob, ok := m.Global(slug); ok {
				targetKind, kind = "global", cache.KindGlobal
				header = globalHeader(glob)
			} else {
				// Resolve produces the collection_unknown/global_unknown error
				// with did_you_mean from the manifest.
				if _, err := m.ResolveCollection(slug); err != nil {
					return nil, err
				}
				return nil, apierr.New(apierr.CodeCollectionUnknown, "unknown entity %q", slug)
			}

			shard, haveShard := rt.Shard(ctx, slug, kind)
			data := map[string]any{
				"kind":   targetKind,
				"entity": header,
			}
			if !haveShard {
				rt.Warnf(discovery.LimFieldsUnavailable,
					"no field schema is cached for %q; run `pay discover --refresh`", slug)
				data["fields"] = []discovery.Field{}
				data["fields_source"] = discovery.SourceUnknown
				env := output.New("describe", output.KindSchema, data)
				env.WithTarget(&output.Target{Kind: targetKind, Slug: slug})
				return env, nil
			}

			if f.block != "" {
				return describeOneBlock(rt, slug, targetKind, shard, f.block, f.field, header)
			}
			if f.field != "" {
				return describeOneField(rt, slug, targetKind, shard, f.field, header, f.blocksDetail)
			}

			fields := filterFields(shard.Fields, f)
			data["fields"] = fields
			data["fields_count"] = len(fields)
			data["fields_total"] = len(shard.Fields)
			data["required_paths"] = orEmptyStrings(shard.RequiredPaths)
			data["join_fields"] = orEmptyStrings(shard.JoinFields)
			data["blocks"] = shard.Blocks
			// blocks_source is keyed by FIELD PATH: two blocks fields on one
			// entity are resolved independently and routinely disagree, so a
			// single string here would be a confident answer about a field it
			// was never measured on (§7.10).
			data["blocks_source"] = blocksSourceByField(shard)
			data["blocks_source_summary"] = shard.BlocksSource
			data["block_fields"] = shard.BlockFields
			addBlockSchemas(rt, data, shard, shard.BlockSlugsFor(), f.blocksDetail, slug)
			addFieldDocs(data, shard)
			data["queryable_paths"] = orEmptyStrings(shard.QueryablePaths())
			data["sortable_paths"] = orEmptyStrings(shard.SortablePaths())
			data["date_fields"] = orEmptyStrings(shard.DateFields())

			if f.examples {
				data["examples"] = describeExamples(slug, targetKind, shard, m)
			}
			if f.sample {
				sample, err := rt.sampleDocument(ctx, slug, targetKind)
				if err != nil {
					rt.Warnf("sample_unavailable", "could not fetch a sample document: %s", err)
				} else {
					data["sample"] = sample
				}
			}
			env := output.New("describe", output.KindSchema, data)
			env.WithTarget(&output.Target{Kind: targetKind, Slug: slug})
			return env, nil
		}),
	}
	cmd.Flags().StringVar(&f.field, "field", "", "describe one field path in full (dotted, e.g. hero.links.link.url)")
	cmd.Flags().StringVar(&f.block, "block", "",
		"describe ONE blockType's own field schema: what goes INSIDE a cta, not just that cta exists")
	cmd.Flags().BoolVar(&f.blocksDetail, "blocks-detail", false,
		"inline EVERY reachable blockType's field schema (off by default: see BLOCKS in `pay help describe`)")
	cmd.Flags().BoolVar(&f.requiredOnly, "required-only", false, "only fields whose required is known true")
	cmd.Flags().BoolVar(&f.queryable, "queryable", false, "only fields usable in --where")
	cmd.Flags().BoolVar(&f.examples, "examples", false, "add generated, copy-pasteable commands for this entity")
	cmd.Flags().BoolVar(&f.sample, "sample", false, "also fetch one real document (one extra request)")
	_ = cmd.RegisterFlagCompletionFunc("field", CompleteFieldPaths(rt, FieldSelectable))
	_ = cmd.RegisterFlagCompletionFunc("block", CompleteBlockSlugs(rt))

	SetHelp(cmd, &Help{
		Synopsis: []string{
			"pay describe <collection|global> [--field PATH] [--required-only] [--queryable] [--examples] [--sample]",
			"pay describe <collection|global> --block <blockType-slug> [--field PATH]",
			"pay describe <collection|global> [--field PATH] --blocks-detail",
		},
		Collections: true,
		Globals:     true,
		Long: "Every field entry carries the same 26 keys, using null or the sentinels \"n/a\" /\n" +
			"\"unknown\" where a fact could not be learned, so `jq '.data.fields[] |\n" +
			"select(.required)'` is total and never needs a presence check.\n" +
			"\n" +
			"BLOCKS. A blocks field answers two different questions. WHICH blocks may go in it\n" +
			"is .blocks[FIELD] (slugs) and is always printed. WHAT IS INSIDE one is\n" +
			"--block <slug>, which prints that block type's own fields, their types, their\n" +
			"relationship targets and their required-ness with provenance.\n" +
			"--blocks-detail inlines every one of them at once and is OFF by default on\n" +
			"purpose. Measured on the live project: `pay describe pages` is 34 KB, and 81 KB\n" +
			"with --blocks-detail, while `--field layout` goes from 7.2 KB to 53 KB — seven\n" +
			"times the output of the most-run discovery command, to answer a question that\n" +
			"is asked one block at a time. .block_schemas_available names every block whose\n" +
			"schema is cached; --block <slug> prints the one you are about to write.\n" +
			"\n" +
			"WHAT A BLOCK IS FOR. .block_docs[SLUG] is printed beside every slug list and\n" +
			"carries the block's human label and a one-sentence description, so an agent can\n" +
			"choose between cta, content and mediaBlock without a second command.\n" +
			"--block <slug> repeats it as .docs, and every field inside carries its own\n" +
			".description — the per-field instruction for filling that field in.\n" +
			"These are the PROJECT'S OWN WORDS, read from the block's config.ts on disk:\n" +
			"`labels: { singular, plural }` and a description under custom.description,\n" +
			"custom.docs or custom.summary. Payload publishes neither over REST or GraphQL,\n" +
			"and defines NO description field for a block at all (a Block's `admin` accepts\n" +
			"only components/custom/disableBlockName/group/images/jsx), which is why the text\n" +
			"lives in `custom` and why .description_key always names the key that answered.\n" +
			"A project gets any of this only if its authors wrote it: null means nobody did,\n" +
			"never that PayCLI failed, and .docs_reason says which of the two it is. A\n" +
			"plugin's blocks live in node_modules, which is never scanned, so theirs are\n" +
			"always null.\n" +
			"\n" +
			"PER-FIELD INSTRUCTIONS. For ordinary collection fields the same mechanism is\n" +
			"Payload's own `admin: { description }`, which IS supported on a field. They are\n" +
			"published as .field_docs[PATH] — a separate map, not a key on every field entry,\n" +
			"because three more keys on ~90 entries would add ~16 KB to this command to\n" +
			"publish null 90 times. .documented_paths lists them, .field_docs_source says\n" +
			"whether the entity's config was read at all, and --field PATH answers for one\n" +
			"field as .field_doc. Only an entity's TOP-LEVEL fields array is read; a field\n" +
			"nested in a group, an array or a tab is reported as undocumented rather than\n" +
			"given a path this scanner would have had to guess.\n" +
			"\n" +
			"Required-ness inside a block is tri-state and is null more often than on a\n" +
			"collection field. Payload publishes NO input type for a block type, so §7.4's\n" +
			"NON_NULL trick has nothing to read; what is left is a NON_NULL on the block's\n" +
			"object type (proof of required) and the block's own config.ts on disk. A\n" +
			"plugin's blocks live in node_modules, which is never scanned, so their optional\n" +
			"fields stay \"unknown\" — never \"not required\".",
		Args: []ArgSpec{
			{Name: "entity", Required: true, Type: "enum", ValuesFrom: "discovery.collections", Example: "pages"},
		},
		Output:    OutputSpec{Kind: output.KindSchema, Skeleton: `{"kind":"collection","entity":{…},"fields":[{"path":"title","payload_type":"text","required":true,"queryable":true,"operators":["equals","contains",…]}],"required_paths":["title"]}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitAuth, apierr.ExitNetwork, apierr.ExitConfig, apierr.ExitCapability},
		Examples: []Example{
			{Why: "everything about a collection", Cmd: "pay describe pages"},
			{Why: "what must I supply to create one?", Cmd: "pay describe pages --required-only --path .fields[].path"},
			{Why: "what can I filter on?", Cmd: "pay describe pages --queryable --path .queryable_paths[]"},
			{Why: "one field in full, including its operators", Cmd: "pay describe pages --field title"},
			{Why: "the blockTypes a blocks field accepts", Cmd: "pay describe pages --field layout"},
			{Why: "what goes INSIDE one block type", Cmd: "pay describe pages --block cta"},
			{Why: "which block do I want? label + description for every one",
				Cmd: "pay describe pages --path .block_docs"},
			{Why: "what is this one block FOR", Cmd: "pay describe pages --block cta --path .docs"},
			{Why: "the project's own instructions for filling fields in",
				Cmd: "pay describe pages --path .field_docs"},
			{Why: "just that block's required fields", Cmd: "pay describe pages --block mediaBlock --path .required_fields[]"},
			{Why: "every block type of one field, in full", Cmd: "pay describe pages --field layout --blocks-detail"},
			{Why: "schema plus a real document", Cmd: "pay describe pages --sample"},
			{Why: "generated commands for this collection", Cmd: "pay describe pages --examples --path .examples[]"},
		},
		Mistakes: []Mistake{
			{Wrong: "Reading `required: null` as \"not required\".",
				Right: "null means PayCLI never learned it. Attempt the write; a 400 names the missing fields precisely."},
			{Wrong: "Using a join field in --select.",
				Right: "Join fields are read-only and are requested with --joins 'field:limit=5,sort=-createdAt'; .join_fields lists them."},
			{Wrong: "Assuming `sortable: true` was measured.",
				Right: "Sortability is heuristic unless sortable_confidence says otherwise; Payload silently ignores an unsortable field."},
			{Wrong: "Expecting `blocks` to be filled on every project.",
				Right: "Block slugs are not in the API; a null means no source resolved them. .blocks_source[FIELD] says which source did."},
			{Wrong: "Reusing one collection's block types for another field.",
				Right: "Every blocks field has its own list: pages.layout and forms.fields share none. Read .blocks[FIELD]."},
			{Wrong: "Writing a block with only the fields --block listed as required.",
				Right: "Required-ness inside a block is often null (Payload publishes no input type for one). .block.required_unknown names every field nobody could answer for; .block.reason says why."},
			{Wrong: "Reading a null label or description as a PayCLI failure.",
				Right: "Payload publishes neither and defines no description for a block at all. PayCLI reports what the project's authors wrote in the block's config.ts (`labels:` and custom.description / custom.docs / custom.summary); null means nobody wrote one. .docs_reason says which case it is."},
			{Wrong: "Expecting a description on a plugin's blocks.",
				Right: "The form-builder's blocks live in node_modules, which §7.10 never scans, so their label and description are always null with labels_source \"unknown\". Read .block[].fields for what they contain."},
			{Wrong: "Treating blockName or id as content.",
				Right: "id, blockName and blockType are Payload plumbing. blockType is mandatory and must be the SLUG; id is server-generated; blockName is an optional admin label. .block.fields[].plumbing marks them."},
		},
		SeeAlso: []string{"pay collections", "pay explain --collection <slug>", "pay find <collection> --select …"},
	})
	return cmd
}

func collectionHeader(c *discovery.Collection) map[string]any {
	return map[string]any{
		"slug": c.Slug, "singular": c.Labels.Singular, "plural": c.Labels.Plural,
		"labels_source": c.Labels.Source,
		"kind":          collectionKind(c), "internal": c.Internal,
		"id_type": c.IDType, "id_type_source": c.IDTypeSource,
		"reachability": c.Reachability,
		"ops":          collectionOpsFor(c), "features": collectionFeaturesFor(c),
		"publishable": c.Publishable, "publishable_reason": c.PublishableReason,
		"title_field": c.TitleField, "date_fields": orEmptyStrings(c.DateFields),
		"fields_count": c.FieldsCount, "fields_source": c.FieldsSource,
		"permissions": c.Permissions, "flags": c.Flags, "flags_source": c.FlagsSource,
		"graphql": c.GraphQL,
	}
}

func globalHeader(g *discovery.Global) map[string]any {
	return map[string]any{
		"slug": g.Slug, "singular": g.Labels.Singular, "plural": g.Labels.Plural,
		"internal": g.Internal, "reachability": g.Reachability,
		"ops": newGlobalRow(g).Ops, "features": newGlobalRow(g).Features,
		"fields_count": g.FieldsCount, "fields_source": g.FieldsSource,
		"permissions": g.Permissions, "flags": g.Flags, "flags_source": g.FlagsSource,
		"graphql": g.GraphQL,
	}
}

func filterFields(fields []discovery.Field, f *describeFlags) []discovery.Field {
	out := make([]discovery.Field, 0, len(fields))
	for _, fl := range fields {
		if f.requiredOnly && (fl.Required == nil || !*fl.Required) {
			continue
		}
		if f.queryable && !fl.Queryable {
			continue
		}
		out = append(out, fl)
	}
	return out
}

func describeOneField(rt *Runtime, slug, targetKind string,
	shard *discovery.Shard, path string, header map[string]any, blocksDetail bool) (*output.Envelope, error) {
	field, ok := shard.Field(path)
	if !ok {
		return nil, apierr.New(apierr.CodeUnknownField,
			"%q has no field %q", slug, path).
			WithHint("pay describe %s --path .fields[].path lists every field", slug).
			WithDidYouMean(apierr.DidYouMean(path, shard.Paths())...)
	}
	data := map[string]any{
		"kind":   targetKind,
		"entity": header,
		"field":  field,
	}
	// This field's own human instruction, from the project's
	// `admin: { description }`. null is "the project's authors wrote none" —
	// Payload publishes admin.description nowhere in the API, so there is no
	// second source to fall back to and nothing is ever invented here.
	doc, documented := shard.FieldDocFor(path)
	if documented {
		data["field_doc"] = doc
	} else {
		data["field_doc"] = nil
	}
	data["field_doc_source"] = fieldDocSource(shard, documented)
	if field.PayloadType == discovery.TypeBlocks {
		// Everything below is THIS field's answer. shard.BlocksSource is an
		// entity-wide summary and is deliberately not used here: `forms.fields`
		// and `pages.layout` accept completely different block types, so an
		// answer that is not per-field is a wrong answer.
		bf, _ := shard.BlockFieldFor(path)
		data["block_types"] = bf.Slugs
		data["blocks_source"] = orUnknown(bf.Source)
		data["block_type_sources"] = bf.SlugSources
		data["block_interface_names"] = orEmptyStrings(bf.InterfaceNames)
		data["unresolved_interface_names"] = orEmptyStrings(bf.Unresolved)
		data["blocks_reason"] = bf.Reason
		addBlockSchemas(rt, data, shard, bf.Slugs, blocksDetail, slug)
		switch {
		case len(bf.Slugs) == 0:
			data["blocks_help"] = discovery.DescribeBlocksHelp(bf.InterfaceNames, rt.Cfg.Profile, path)
			rt.Warnf(discovery.LimBlockSlugsUnknown, "%s", discovery.UnresolvedBlocksReason(path))
		case bf.Reason != "":
			// A partly-inferred list is usable but not confirmed; saying so is
			// the difference between a hint and a silent wrong answer.
			rt.Warnf(discovery.LimBlockSlugsUnknown, "%s", bf.Reason)
		}
	}
	if len(field.Operators) > 0 {
		data["where_examples"] = fieldWhereExamples(slug, field)
	}
	env := output.New("describe", output.KindSchema, data)
	env.WithTarget(&output.Target{Kind: targetKind, Slug: slug})
	return env, nil
}

// addFieldDocs publishes an entity's per-field documentation as its own map,
// keyed by field path, plus the provenance of the map as a whole.
//
// It is a separate key rather than three more keys on each of ~90 field
// entries because the latter would add ~16 KB to `pay describe pages` to
// publish null 90 times. Here the cost is proportional to what the project
// actually wrote, and is zero on a project that wrote nothing.
func addFieldDocs(data map[string]any, shard *discovery.Shard) {
	docs := shard.FieldDocs
	if docs == nil {
		// An empty map, never null: "no field of this entity is documented" is
		// a complete answer, and field_docs_source says whether anything was
		// read at all.
		docs = map[string]discovery.FieldDoc{}
	}
	data["field_docs"] = docs
	data["field_docs_source"] = orUnknown(shard.FieldDocsSource)
	data["field_docs_file"] = shard.FieldDocsFile
	data["documented_paths"] = orEmptyStrings(shard.DocumentedPaths())
	data["field_docs_note"] = fieldDocsNote
}

// fieldDocsNote states where a per-field description comes from and why one
// may be missing — so an agent reads an absent entry as "this project did not
// write one" rather than "PayCLI failed to fetch it".
const fieldDocsNote = "field_docs are the project's OWN per-field instructions, read from " +
	"`admin: { description: '…' }` in the entity's config on disk (§7.10). Payload publishes them " +
	"nowhere in the API — not in a REST response and not in GraphQL introspection — so a field is " +
	"listed here only if its authors wrote one, and only when it is declared in the entity's " +
	"TOP-LEVEL fields array. An absent path means undocumented, never undiscovered."

// fieldDocSource labels a single field's documentation. A field with no entry
// still reports whether the entity's config was read at all, which is the
// difference between "nobody documented this field" and "PayCLI never saw the
// config".
func fieldDocSource(shard *discovery.Shard, documented bool) string {
	if documented {
		return discovery.SourceProjectSource
	}
	if shard.FieldDocsSource == discovery.SourceProjectSource {
		// The config WAS read; this field simply carries no description.
		return discovery.SourceNA
	}
	return discovery.SourceUnknown
}

// describeOneBlock answers `pay describe <entity> --block <slug>`: the field
// schema of ONE block type, which is what an agent needs to construct a block
// rather than merely name it.
//
// Scoped to the entity on the command line because a blockType is only
// writable where a blocks field accepts it: `cta` is real on pages.layout and
// meaningless on forms.fields, and answering for the wrong entity would
// produce a document Payload silently drops (§9.7).
func describeOneBlock(rt *Runtime, slug, targetKind string, shard *discovery.Shard,
	block, fieldPath string, header map[string]any) (*output.Envelope, error) {
	accepted := blockFieldsAccepting(shard, block, fieldPath)
	known := shard.BlockSlugsFor()
	if fieldPath != "" {
		if bf, ok := shard.BlockFieldFor(fieldPath); ok {
			known = bf.Slugs
		}
	}
	if len(accepted) == 0 {
		where := slug
		if fieldPath != "" {
			where = slug + "." + fieldPath
		}
		err := apierr.New(apierr.CodeInvalidOption,
			"no blocks field of %s accepts blockType %q", where, block).
			WithDidYouMean(apierr.DidYouMean(block, known)...)
		if len(known) == 0 {
			return nil, err.WithHint(
				"pay describe %s --path .blocks lists the blockTypes this entity accepts; it is empty here, "+
					"so either %s has no blocks field or its slugs could not be resolved (pay describe %s --field FIELD says which)",
				slug, slug, slug)
		}
		return nil, err.WithHint("pay describe %s --path .blocks lists every blockType this entity accepts", slug)
	}

	schema, ok := shard.BlockSchemaFor(block)
	if !ok {
		// The slug is real but its interior was never discovered: a shard
		// written before block schemas existed, or a REST-only shard with no
		// GraphQL union to read. Saying so beats answering "no fields".
		return nil, apierr.New(apierr.CodeFeatureUnavailable,
			"blockType %q is accepted by %s but PayCLI has no field schema for it", block, strings.Join(accepted, ", ")).
			WithHint("a block's fields are read from its GraphQL object type, so there is none when GraphQL " +
				"is unreachable, when the slug was pinned in the profile and no union member resolves to it, " +
				"or when the cache predates block_schemas; `pay discover --refresh` fixes the last of those")
	}

	data := map[string]any{
		"kind":   targetKind,
		"entity": header,
		"block":  schema,
		// docs is the answer to "what is this block FOR" on its own path, so
		// `--path .docs.description` works without walking the schema. Its
		// values are the same ones .block carries; it is a shortcut, not a
		// second source.
		"docs":            schema.Doc(),
		"docs_note":       discovery.BlockDocsNote,
		"block_fields":    accepted,
		"content_fields":  blockFieldPaths(schema.ContentFields()),
		"required_fields": schema.RequiredFields(),
		// documented_fields names every field of this block that carries a
		// per-field instruction from `admin: { description }`, so an agent can
		// see at a glance whether there is any guidance to read.
		"documented_fields": documentedBlockFields(schema),
		"plumbing_note":     discovery.BlockPlumbingNote,
		"examples":          blockExamples(slug, accepted[0], schema),
	}
	if len(schema.RequiredUnknown) > 0 {
		rt.Warnf(warnBlockRequiredUnknown, "%s", schema.Reason)
	}
	env := output.New("describe", output.KindSchema, data)
	env.WithTarget(&output.Target{Kind: targetKind, Slug: slug})
	return env, nil
}

// warnBlockRequiredUnknown is emitted when a block has at least one field
// whose required-ness neither GraphQL nor project source could establish. It
// is a warning rather than a silent gap because "not listed as required" and
// "not required" are the two readings an agent must not confuse.
const warnBlockRequiredUnknown = "block_required_unknown"

// blockFieldsAccepting lists the blocks field paths of this entity that accept
// a blockType, optionally narrowed to one field.
func blockFieldsAccepting(shard *discovery.Shard, block, fieldPath string) []string {
	out := []string{}
	for path, bf := range shard.BlockFields {
		if fieldPath != "" && path != fieldPath {
			continue
		}
		for _, s := range bf.Slugs {
			if s == block {
				out = append(out, path)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// documentedBlockFields lists the paths whose description is non-null. An
// empty list is a real answer — "this block's fields carry no per-field
// instructions" — and is why the key is always present.
func documentedBlockFields(schema discovery.BlockTypeSchema) []string {
	out := []string{}
	for _, f := range schema.Fields {
		if f.Description != nil && *f.Description != "" {
			out = append(out, f.Path)
		}
	}
	return out
}

func blockFieldPaths(fields []discovery.BlockFieldSchema) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, f.Path)
	}
	return out
}

// addBlockSchemas publishes the block interiors under a single pair of keys:
// block_schemas (the real thing, only with --blocks-detail) and
// block_schemas_hint (how to get it otherwise).
//
// Default-off is a size decision, argued in the flag's own help: pages.layout
// resolves 5 block types and forms.fields 9, each with ~13 field entries, so
// inlining them by default would multiply the output of the single most-used
// discovery command for information that is needed one block at a time.
func addBlockSchemas(rt *Runtime, data map[string]any, shard *discovery.Shard,
	slugs []string, detail bool, entity string) {
	if len(slugs) == 0 {
		return
	}
	available := []string{}
	missing := []string{}
	for _, s := range slugs {
		if _, ok := shard.BlockSchemaFor(s); ok {
			available = append(available, s)
		} else {
			missing = append(missing, s)
		}
	}
	data["block_schemas_available"] = available
	// The human half, printed BESIDE every slug list and never gated behind
	// --blocks-detail: a slug alone does not let an agent choose between cta,
	// content and mediaBlock, and needing a second command per candidate is
	// exactly the cost this key removes. It is one short object per block, not
	// a field schema, so the default response grows by ~1 KB rather than the
	// ~39 KB --blocks-detail costs.
	if docs := blockDocs(shard, available); len(docs) > 0 {
		data["block_docs"] = docs
		data["block_docs_note"] = discovery.BlockDocsNote
	}
	if len(missing) > 0 {
		// Never silently short: a slug with no schema is reported by name.
		data["block_schemas_missing"] = missing
		rt.Warnf(discovery.LimFieldsUnavailable,
			"no field schema is cached for blockType %s; run `pay discover --refresh`", strings.Join(missing, ", "))
	}
	if !detail {
		data["block_schemas"] = nil
		data["block_schemas_hint"] = fmt.Sprintf(
			"%d block type(s) have a cached field schema. They are omitted by default because they are large: "+
				"add --blocks-detail for all of them, or `pay describe %s --block %s` for one.",
			len(available), entity, firstOr(available, "<slug>"))
		return
	}
	out := make(map[string]discovery.BlockTypeSchema, len(available))
	for _, s := range available {
		bs, _ := shard.BlockSchemaFor(s)
		out[s] = bs
	}
	data["block_schemas"] = out
}

// blockDocs is the compact label/description view for a set of slugs, keyed by
// slug. Only blocks whose schema is cached appear: a slug with no schema has no
// documentation either, and block_schemas_missing already names it.
func blockDocs(shard *discovery.Shard, slugs []string) map[string]discovery.BlockDoc {
	out := make(map[string]discovery.BlockDoc, len(slugs))
	for _, s := range slugs {
		bs, ok := shard.BlockSchemaFor(s)
		if !ok {
			continue
		}
		out[s] = bs.Doc()
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func firstOr(ss []string, fallback string) string {
	if len(ss) == 0 {
		return fallback
	}
	return ss[0]
}

// blockExamples generates runnable commands for ONE block type, built from
// this block's real fields: its blockType slug, every field proved required,
// and a typed placeholder for each.
//
// Placeholders are angle-bracketed so that a value that must be replaced can
// never be mistaken for one that works, and every generated write carries
// --dry-run: §10.2's echo-diff is what tells an agent whether Payload kept the
// block, and a dry run is where to find that out first.
func blockExamples(entity, fieldPath string, schema discovery.BlockTypeSchema) []string {
	body := blockSkeleton(schema)
	return []string{
		fmt.Sprintf("pay describe %s --block %s --path .block.fields[]", entity, schema.Slug),
		fmt.Sprintf("pay create %s --set-json %s='[%s]' --dry-run", entity, fieldPath, body),
		fmt.Sprintf("pay update %s <id> --set-json %s='[%s]' --dry-run", entity, fieldPath, body),
	}
}

// blockSkeleton renders the minimum JSON object for one block: blockType plus
// every top-level field proved required. Fields whose required-ness is unknown
// are deliberately left out — including them would make a guess look like a
// requirement — and the block's reason says where the gap is.
func blockSkeleton(schema discovery.BlockTypeSchema) string {
	parts := []string{fmt.Sprintf("%q:%q", "blockType", schema.Slug)}
	for _, f := range schema.Fields {
		if f.Plumbing || f.Parent != nil || f.Required == nil || !*f.Required {
			continue
		}
		parts = append(parts, fmt.Sprintf("%q:%s", f.Name, blockPlaceholder(f)))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// blockPlaceholder is a JSON literal of the right SHAPE for a field, so that
// only the value has to be replaced and never the structure.
func blockPlaceholder(f discovery.BlockFieldSchema) string {
	if len(f.Options) > 0 {
		return fmt.Sprintf("%q", f.Options[0])
	}
	switch f.PayloadType {
	case discovery.TypeNumber:
		return "0"
	case discovery.TypeCheckbox:
		return "true"
	case discovery.TypeDate:
		return `"<ISO-8601>"`
	case discovery.TypeRichText, discovery.TypeJSON:
		return `{"<lexical root>":"pay describe ` + f.Name + ` --sample shows a real value"}`
	case discovery.TypeArray, discovery.TypeBlocks:
		return "[]"
	case discovery.TypeGroup:
		return "{}"
	case discovery.TypeRelationship, discovery.TypeUpload:
		target := "id"
		if len(f.RelationTo) > 0 {
			target = f.RelationTo[0] + " id"
		}
		if f.Polymorphic {
			return fmt.Sprintf(`{"relationTo":%q,"value":"<%s>"}`, firstOr(f.RelationTo, "<collection>"), target)
		}
		if f.HasMany {
			return fmt.Sprintf(`["<%s>"]`, target)
		}
		return fmt.Sprintf(`"<%s>"`, target)
	default:
		return `"<text>"`
	}
}

// fieldWhereExamples turns a field's operator list into runnable --where terms.
func fieldWhereExamples(slug string, f discovery.Field) []string {
	value := "VALUE"
	switch f.PayloadType {
	case discovery.TypeNumber:
		value = "10"
	case discovery.TypeCheckbox:
		value = "true"
	case discovery.TypeDate:
		value = "2026-01-01"
	case discovery.TypeSelect, discovery.TypeRadio:
		if len(f.Options) > 0 {
			value = f.Options[0]
		}
	}
	var out []string
	for _, op := range f.Operators {
		alias := op
		if canonical, ok := query.Canonical(op); ok {
			alias = canonical
		}
		out = append(out, fmt.Sprintf("pay find %s --where '%s %s %s'", slug, f.Path, alias, value))
		if len(out) >= 4 {
			break
		}
	}
	return out
}

// describeExamples generates commands that use this entity's real fields.
func describeExamples(slug, kind string, shard *discovery.Shard, m *discovery.Manifest) []string {
	if kind == "global" {
		return []string{
			"pay globals get " + slug,
			fmt.Sprintf("pay globals get %s --select %s", slug, strings.Join(firstPaths(shard, 3), ",")),
			fmt.Sprintf("pay globals update %s --set %s=VALUE --dry-run", slug, firstWritable(shard)),
		}
	}
	sel := strings.Join(firstPaths(shard, 4), ",")
	sortField := "updatedAt"
	if dates := shard.DateFields(); len(dates) > 0 {
		sortField = dates[0]
	}
	queryable := "id"
	if qs := shard.QueryablePaths(); len(qs) > 0 {
		queryable = qs[0]
	}
	out := []string{
		fmt.Sprintf("pay find %s --limit 5 --select %s", slug, sel),
		fmt.Sprintf("pay find %s --where '%s contains TEXT' --sort -%s", slug, queryable, sortField),
		fmt.Sprintf("pay count %s --where '%s exists true'", slug, queryable),
	}
	if req := shard.RequiredPaths; len(req) > 0 {
		sets := make([]string, 0, len(req))
		for _, p := range req {
			sets = append(sets, "--set "+p+"=VALUE")
		}
		out = append(out, fmt.Sprintf("pay create %s %s --dry-run", slug, strings.Join(sets, " ")))
	} else {
		out = append(out, fmt.Sprintf("pay create %s --set %s=VALUE --dry-run", slug, firstWritable(shard)))
	}
	if c, ok := m.Collection(slug); ok && c.Flags.Drafts != nil && *c.Flags.Drafts {
		out = append(out, fmt.Sprintf("pay find %s --published-only --limit 5", slug))
	}
	return out
}

func firstPaths(shard *discovery.Shard, n int) []string {
	paths := shard.Paths()
	out := []string{"id"}
	for _, p := range paths {
		if p == "id" {
			continue
		}
		out = append(out, p)
		if len(out) >= n {
			break
		}
	}
	return out
}

func firstWritable(shard *discovery.Shard) string {
	for _, f := range shard.Fields {
		if f.ReadOnly || f.Path == "id" {
			continue
		}
		if f.PayloadType == discovery.TypeText || f.PayloadType == discovery.TypeTextarea {
			return f.Path
		}
	}
	for _, f := range shard.Fields {
		if !f.ReadOnly && f.Path != "id" {
			return f.Path
		}
	}
	return "FIELD"
}

// sampleDocument fetches one real document so an agent can see the shape the
// server actually returns. It is redacted like everything else.
func (rt *Runtime) sampleDocument(ctx context.Context, slug, kind string) (any, error) {
	client, err := rt.Client(ctx)
	if err != nil {
		return nil, err
	}
	if kind == "global" {
		doc, _, err := client.GlobalGet(ctx, slug, query.Params{})
		if err != nil {
			return nil, err
		}
		masked, _ := redact.Value(map[string]any(doc))
		return masked, nil
	}
	limit := 1
	res, err := client.Find(ctx, slug, query.Params{Limit: &limit})
	if err != nil {
		return nil, err
	}
	if len(res.Docs) == 0 {
		return nil, apierr.New(apierr.CodeDocNotFound, "%s is empty", slug)
	}
	masked, _ := redact.Value(map[string]any(res.Docs[0]))
	return masked, nil
}

// blocksSourceByField projects the shard's per-field block provenance into the
// map `pay describe <entity>` publishes as blocks_source.
//
// It is nil when the entity has no blocks field, matching .blocks: nil means
// "no blocks field here", never "resolved to nothing".
func blocksSourceByField(shard *discovery.Shard) map[string]string {
	if shard == nil || len(shard.BlockFields) == 0 {
		return nil
	}
	out := make(map[string]string, len(shard.BlockFields))
	for path, bf := range shard.BlockFields {
		out[path] = orUnknown(bf.Source)
	}
	return out
}

// orUnknown keeps a provenance key from ever being the empty string, which is
// not one of §7.8.3's documented values.
func orUnknown(source string) string {
	if source == "" {
		return discovery.SourceUnknown
	}
	return source
}
