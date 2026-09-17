package discovery

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// typeSelection is the universal __type selection set. Every introspection
// field it asks for is legal on any __Type; the ones that do not apply come
// back null, so one alias shape serves OBJECTs, INPUT_OBJECTs, ENUMs and
// UNIONs alike. That is what makes a single flat batch possible.
const typeSelection = `kind name ` +
	`fields { name type { kind name ofType { kind name ofType { kind name ofType { kind name } } } } } ` +
	`inputFields { name type { kind name ofType { kind name ofType { kind name ofType { kind name } } } } } ` +
	`enumValues { name } ` +
	`possibleTypes { name kind }`

// maxBatches is §7.4's adaptive-batching ceiling: start with one batch
// containing every alias and halve on a complexity error, an over-8 MB body or
// a null alias, up to four batches total. Past that, record
// diagnostics.degraded[] += "stage2" and fall through to the REST-only path —
// never a hard failure.
const maxBatches = 4

// maxLeafRounds bounds the leaf resolution. Round 1 is the entity types,
// round 2 the leaves they reference (enums, relationship wrappers, unions,
// nested groups and their mutation input types) and each further round one
// more level of nesting. It is bounded at 8 so a deeply nested
// or self-referential schema costs a fixed number of requests rather than an
// unbounded one; anything still unresolved is reported as unknown.
const maxLeafRounds = 8

// maxNestDepth caps the group/array walk so a self-referential type cannot
// produce an infinite field list.
const maxNestDepth = 4

// stage2 resolves field schemas for every entity that has a GraphQL name.
func (d *Discoverer) stage2(ctx context.Context, entities map[string]*entity) (*Schema, bool) {
	schema := &Schema{Types: map[string]*IntroType{}, SlugBySingular: map[string]string{}}
	for slug, e := range entities {
		if e.Singular != "" {
			schema.SlugBySingular[e.Singular] = slug
		}
	}

	want := []string{}
	for _, e := range entities {
		if e.Singular == "" {
			continue
		}
		want = append(want,
			e.Singular,
			"mutation"+e.Singular+"Input",
			e.Singular+"_where",
		)
	}
	// LocaleInputType is §7.9b's cross-validation source. It costs one alias
	// and its absence is itself the proof that this project is not localised.
	want = append(want, "LocaleInputType")
	sort.Strings(want)

	complete := true
	for round := 0; round < maxLeafRounds && len(want) > 0; round++ {
		pending := d.fetchTypes(ctx, schema, want)
		if pending {
			complete = false
		}
		want = missingTypes(entities, schema)
	}
	if len(want) > 0 {
		complete = false
	}
	return schema, complete
}

// fetchTypes issues §7.4's adaptive batches for a set of type names and stores
// whatever resolved. It returns true when at least one batch never resolved.
func (d *Discoverer) fetchTypes(ctx context.Context, schema *Schema, names []string) bool {
	names = pruneKnown(schema, names)
	if len(names) == 0 {
		return false
	}
	queue := [][]string{names}
	issued := 0
	failed := false
	for len(queue) > 0 {
		batch := queue[0]
		queue = queue[1:]
		if len(batch) == 0 {
			continue
		}
		if issued >= maxBatches {
			failed = true
			continue
		}
		issued++
		d.graphQLBatches++
		res, err := d.postGraphQL(ctx, buildTypeQuery(batch), false)
		if ok := d.absorbTypes(schema, batch, res); ok {
			continue
		}
		_ = err
		if len(batch) == 1 {
			// A single alias that will not resolve is a hole, not a reason to
			// keep splitting. It is recorded as unknown by the field walk.
			failed = true
			continue
		}
		mid := len(batch) / 2
		queue = append(queue, batch[:mid], batch[mid:])
	}
	return failed
}

func pruneKnown(schema *Schema, names []string) []string {
	out := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, n := range names {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		if _, ok := schema.Types[n]; ok {
			continue
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// buildTypeQuery builds one aliased batch. Alias names are positional (t0, t1,
// …) because a GraphQL alias must be a NAME and a Payload type name may
// contain characters that are not.
func buildTypeQuery(names []string) string {
	var b strings.Builder
	b.WriteString("{\n")
	for i, n := range names {
		b.WriteString("  t")
		b.WriteString(strconv.Itoa(i))
		b.WriteString(`: __type(name: `)
		b.WriteString(quoteGraphQLString(n))
		b.WriteString(") { ")
		b.WriteString(typeSelection)
		b.WriteString(" }\n")
	}
	b.WriteString("}")
	return b.String()
}

// quoteGraphQLString escapes a type name for a GraphQL string literal.
func quoteGraphQLString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// absorbTypes stores a batch's results. It reports false when the batch must
// be halved: a complexity error, an over-8 MB body, or any alias that came
// back null while its siblings did not.
func (d *Discoverer) absorbTypes(schema *Schema, names []string, res *gqlResult) bool {
	if res == nil || res.Data == nil {
		return false
	}
	if len(res.Body) > maxGraphQLBody {
		return false
	}
	if IsComplexityError(res.errorMessages()) {
		return false
	}

	resolved := 0
	for i, n := range names {
		raw, ok := res.alias("t" + strconv.Itoa(i))
		if !ok {
			continue
		}
		if isJSONNull(raw) {
			// A genuinely non-existent type (a global has no {S}_where) is a
			// legitimate null, so it is recorded as "asked and absent" rather
			// than treated as a batch failure.
			schema.Types[n] = nil
			resolved++
			continue
		}
		var t IntroType
		if json.Unmarshal(raw, &t) != nil {
			continue
		}
		schema.Types[n] = &t
		resolved++
	}
	if resolved == 0 {
		return false
	}
	// A batch that resolved some aliases and silently dropped others is
	// §7.4's "any data alias that came back null" trigger.
	return resolved == len(names)
}

// missingTypes walks every entity's fields with the schema resolved so far and
// reports the type names the walk still needs.
func missingTypes(entities map[string]*entity, schema *Schema) []string {
	need := map[string]bool{}
	for _, e := range entities {
		if e.Singular == "" {
			continue
		}
		obj := schema.Type(e.Singular)
		if obj == nil {
			continue
		}
		collectMissing(obj, e.Singular, schema, need, 0)
	}
	out := make([]string, 0, len(need))
	for n := range need {
		if _, known := schema.Types[n]; known {
			continue
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func collectMissing(obj *IntroType, ownerSingular string, schema *Schema, need map[string]bool, depth int) {
	if obj == nil || depth >= maxNestDepth {
		return
	}
	for i := range obj.Fields {
		f := &obj.Fields[i]
		k := InferKind(f.Type, ownerSingular, f.Name, schema)
		for _, leaf := range k.Leaves {
			if leaf != "" {
				need[leaf] = true
			}
		}
		// A blocks field's union members are OBJECT types carrying the block's
		// own fields, and they are the only place that schema exists: Payload
		// publishes no input type for a block, so nothing else in the schema
		// describes what goes inside one. They ride the SAME adaptive batch as
		// every other leaf — one more alias each, not one more request each.
		for _, iface := range k.BlockInterfaces {
			if iface == "" {
				continue
			}
			need[iface] = true
			collectBlockMissing(schema.Type(iface), iface, schema, need, 0)
		}
		if k.Children != "" {
			// The nested group's own mutation input type is what carries
			// required-ness one level down, exactly as mutation{Singular}Input
			// does at the top level.
			need[nestedInputName(k.Children)] = true
			child, known := schema.Types[k.Children]
			if !known {
				need[k.Children] = true
				continue
			}
			collectMissing(child, ownerSingular, schema, need, depth+1)
		}
	}
}

// collectBlockMissing walks a block type's own fields for the leaves its
// schema needs: the enums behind its selects, the relationship wrappers behind
// its polymorphic fields, and its nested group/array row types.
//
// It deliberately does NOT ask for a mutation input type at any level, unlike
// collectMissing. A block has none — mutationCallToActionBlock_LinksInput does
// not exist — so requesting one would spend an alias per nested type to learn
// nothing.
func collectBlockMissing(obj *IntroType, blockType string, schema *Schema, need map[string]bool, depth int) {
	if obj == nil || depth >= maxBlockNestDepth {
		return
	}
	for i := range obj.Fields {
		f := &obj.Fields[i]
		k := InferKind(f.Type, blockType, f.Name, schema)
		for _, leaf := range k.Leaves {
			if leaf != "" {
				need[leaf] = true
			}
		}
		if k.Children == "" {
			continue
		}
		child, known := schema.Types[k.Children]
		if !known {
			need[k.Children] = true
			continue
		}
		collectBlockMissing(child, blockType, schema, need, depth+1)
	}
}

// buildShardOptions carries the per-entity inputs the field walk needs beyond
// the schema itself.
type buildShardOptions struct {
	Generation string
	Blocks     BlockSources
	// LocaleAll is the per-field localisation evidence for this collection,
	// empty when no locale=all sample was taken.
	LocaleAll LocaleAnalysis
	// LocaleSampled reports that a locale=all sample ran at all; without it
	// fields[].localized stays null (§7.9d).
	LocaleSampled bool
}

// buildShard walks an entity's object type and emits §7.8.2's field entries.
//
// Required-ness comes from mutation{Singular}Input, NEVER from the object type
// — verified: mutationPageInput.title is NON_NULL String while Page.title is
// nullable, because drafts force-nullable the object type.
func buildShard(e *entity, schema *Schema, opts buildShardOptions) *Shard {
	shard := NewShard(opts.Generation, e.Slug)
	obj := schema.Type(e.Singular)
	if obj == nil {
		shard.Finalize()
		return shard
	}
	input := schema.Type("mutation" + e.Singular + "Input")
	where := schema.Type(e.Singular + "_where")

	requiredByName := map[string]bool{}
	writableByName := map[string]bool{}
	if input != nil {
		for _, f := range input.InputFields {
			writableByName[f.Name] = true
			_, _, nonNull, _ := f.Type.Named()
			requiredByName[f.Name] = nonNull
		}
	}
	queryable := map[string]string{}
	if where != nil {
		for _, f := range where.InputFields {
			if f.Name == "AND" || f.Name == "OR" {
				continue
			}
			name, _, _, _ := f.Type.Named()
			queryable[f.Name] = name
		}
	}

	ctx := &walkContext{
		schema:         schema,
		owner:          e.Singular,
		input:          input,
		requiredByName: requiredByName,
		writableByName: writableByName,
		queryable:      queryable,
		opts:           opts,
		shard:          shard,
	}
	ctx.walk(obj, "", nil, 0)

	// §7.10: resolve the real blockType slugs for every blocks field.
	for i := range shard.Fields {
		f := &shard.Fields[i]
		if f.PayloadType != TypeBlocks {
			continue
		}
		res := ResolveBlocks(f.Path, ctx.blockInterfaces[f.Path], opts.Blocks)
		if res.Slugs != nil {
			if shard.Blocks == nil {
				shard.Blocks = map[string][]string{}
			}
			shard.Blocks[f.Path] = res.Slugs
			shard.BlocksSource = res.Source
		} else if shard.BlocksSource == SourceUnknown {
			shard.BlocksSource = SourceUnknown
		}
	}
	shard.Finalize()
	return shard
}

// nestedInputName maps a nested object type to its mutation input twin:
// Page_Hero -> mutationPage_HeroInput, the same rule the top level uses.
func nestedInputName(objectType string) string { return "mutation" + objectType + "Input" }

type walkContext struct {
	schema         *Schema
	owner          string
	input          *IntroType
	requiredByName map[string]bool
	writableByName map[string]bool
	queryable      map[string]string
	opts           buildShardOptions
	shard          *Shard

	blockInterfaces map[string][]string
}

// descend walks a nested group or array row type with its own input type in
// scope, so required-ness one level down is read from the same source as at
// the top.
func (c *walkContext) descend(obj *IntroType, inputName, prefix string, parent *string, depth int) {
	saved, savedReq := c.input, c.requiredByName
	input := c.schema.Type(inputName)
	c.input = input
	c.requiredByName = map[string]bool{}
	if input != nil {
		for _, f := range input.InputFields {
			_, _, nonNull, _ := f.Type.Named()
			c.requiredByName[f.Name] = nonNull
		}
	}
	c.walk(obj, prefix, parent, depth)
	c.input, c.requiredByName = saved, savedReq
}

func (c *walkContext) walk(obj *IntroType, prefix string, parent *string, depth int) {
	if obj == nil || depth >= maxNestDepth {
		return
	}
	if c.blockInterfaces == nil {
		c.blockInterfaces = map[string][]string{}
	}
	for i := range obj.Fields {
		f := &obj.Fields[i]
		path := f.Name
		if prefix != "" {
			path = prefix + "." + f.Name
		}
		kind := InferKind(f.Type, c.owner, f.Name, c.schema)
		field := NewField(f.Name, path)
		field.Parent = parent
		field.PayloadType = kind.PayloadType
		field.PayloadTypeConfidence = kind.Confidence
		field.JSONType = kind.JSONType
		field.HasMany = kind.HasMany
		field.ReadOnly = kind.ReadOnly
		field.Polymorphic = kind.Polymorphic
		field.Options = kind.Options
		field.OptionsSource = kind.OptionsSource
		field.RelationTo = kind.RelationTo
		field.RelationToSource = kind.RelationToSource
		if name, _, _, _ := f.Type.Named(); name != "" {
			field.GraphQLType = strPtr(name)
		}

		// Required-ness and writability come from the mutation input type,
		// NEVER from the object type: drafts force-nullable every object
		// field, so mutationPageInput.title is NON_NULL while Page.title is
		// not.
		if c.input != nil {
			if req, ok := c.requiredByName[f.Name]; ok {
				field.Required = boolPtr(req)
				field.RequiredSource = SourceGraphQLInput
			} else {
				// Present on the object type and absent from the input type is
				// positive evidence of a read-only field (id, join fields,
				// upload-derived sizes).
				field.Required = boolPtr(false)
				field.RequiredSource = SourceGraphQLInput
				field.ReadOnly = true
			}
		}
		if serverManaged(f.Name) {
			field.ReadOnly = true
		}

		// Queryability comes from {Singular}_where, whose paths use __ as the
		// nesting separator.
		if opName, ok := c.queryable[field.GraphQLPath]; ok {
			field.Queryable = true
			field.Operators = OperatorsFor(field.PayloadType, opName)
		}
		field.Sortable = SortableKind(field.PayloadType)
		field.SortableConfidence = ConfidenceHeuristic

		// §7.9d: per-field localized is null unless a locale=all sample proved
		// it. The GraphQL object type is identical for a localized and a
		// non-localized field, so there is nothing else to read.
		if c.opts.LocaleSampled && depth == 0 {
			switch {
			case containsString(c.opts.LocaleAll.Localized, f.Name):
				field.Localized = boolPtr(true)
				field.LocalizedSource = SourceLocaleAll
			case containsString(c.opts.LocaleAll.NotLocalized, f.Name):
				field.Localized = boolPtr(false)
				field.LocalizedSource = SourceLocaleAll
			}
		}

		field.WriteShape = WriteShapeFor(field.PayloadType, field.Polymorphic, field.HasMany)

		if field.PayloadType == TypeJoin {
			c.shard.JoinFields = append(c.shard.JoinFields, path)
		}
		if field.PayloadType == TypeBlocks {
			c.blockInterfaces[path] = kind.BlockInterfaces
		}

		c.shard.Fields = append(c.shard.Fields, field)

		if kind.Children != "" && (kind.PayloadType == TypeGroup || kind.PayloadType == TypeArray) {
			p := path
			c.descend(c.schema.Type(kind.Children), nestedInputName(kind.Children), path, &p, depth+1)
		}
	}
}

// serverManagedNames are fields Payload writes itself. Marking them read_only
// keeps an agent from trying to set a password hash or a session list; the
// entries themselves are kept, because `pay describe users` hiding real fields
// would be a worse lie than showing them. No VALUE from any of them is ever
// written to a manifest artefact (§7.8.3) — discovery only ever sees names.
var serverManagedNames = map[string]bool{
	"id": true, "createdAt": true, "updatedAt": true,
	"apiKeyIndex": true, "hash": true, "salt": true, "sessions": true,
	"resetPasswordToken": true, "resetPasswordExpiration": true,
	"loginAttempts": true, "lockUntil": true,
	"sizes": true, "thumbnailURL": true,
}

func serverManaged(name string) bool { return serverManagedNames[name] }

func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// operatorsByKind is the per-field-kind operator list. It is derived from the
// field kind rather than from one __type call per field on purpose: a project
// with 49 collections and ~30 fields each has ~1,470 distinct *_operator input
// types, and fetching them would multiply §7.1's cold budget for information
// that Payload derives from the field type anyway.
//
// The lists are verified live against Page_title_operator,
// CrmContact_signalCount_operator, Page_publishedAt_operator,
// Page_generateSlug_operator, Page__status_operator and
// CrmContact_company_operator.
var operatorsByKind = map[string][]string{
	TypeText:         {"equals", "not_equals", "like", "contains", "in", "not_in", "all"},
	TypeTextarea:     {"equals", "not_equals", "like", "contains", "in", "not_in", "all"},
	TypeEmail:        {"equals", "not_equals", "like", "contains", "in", "not_in", "all"},
	TypeNumber:       {"equals", "not_equals", "greater_than_equal", "greater_than", "less_than_equal", "less_than", "exists"},
	TypeDate:         {"equals", "not_equals", "greater_than_equal", "greater_than", "less_than_equal", "less_than", "like", "exists"},
	TypeCheckbox:     {"equals", "not_equals", "exists"},
	TypeSelect:       {"equals", "not_equals", "in", "not_in", "all", "exists"},
	TypeRadio:        {"equals", "not_equals", "in", "not_in", "all", "exists"},
	TypeRelationship: {"equals", "not_equals", "in", "not_in", "all", "exists"},
	TypeUpload:       {"equals", "not_equals", "in", "not_in", "all", "exists"},
	TypeID:           {"equals", "not_equals", "greater_than_equal", "greater_than", "less_than_equal", "less_than", "exists"},
	TypePoint:        {"near", "within", "intersects"},
}

// OperatorsFor returns the operators Payload accepts for a field kind. A
// polymorphic relationship's where entry is a {T}_{f}_Relation input object
// taking relationTo and value rather than an operator set, which is why the
// operator type name is inspected as well as the kind.
func OperatorsFor(payloadType, operatorTypeName string) []string {
	if strings.HasSuffix(operatorTypeName, "_Relation") {
		return []string{"equals", "not_equals", "in", "not_in", "all", "exists"}
	}
	ops, ok := operatorsByKind[payloadType]
	if !ok {
		return []string{"equals", "not_equals", "exists"}
	}
	out := make([]string, len(ops))
	copy(out, ops)
	return out
}

// UseAPIKey reports §7.3's use_api_key signal: the mutation input type carries
// both apiKey and enableAPIKey (verified live on mutationUserInput). It drives
// §5.4's `pay doctor` reporting and nothing else.
func UseAPIKey(input *IntroType) *bool {
	if input == nil {
		return nil
	}
	names := map[string]bool{}
	for _, f := range input.InputFields {
		names[f.Name] = true
	}
	return boolPtr(names["apiKey"] && names["enableAPIKey"])
}

// FlagsFromObject reads the three capability flags that Payload encodes as
// fields of the object type. This is strictly better evidence than §7.5's REST
// probes — it is the schema rather than an inference from a 400 — and it is
// what keeps §7.1's four-request cold budget intact on a project whose GraphQL
// is reachable.
//
// _status  => drafts are enabled (verified: Page has it, Category does not)
// deletedAt => trash is enabled  (verified: CrmContact has it, Page does not)
// folder    => folders are enabled (verified: Media has it, Page does not)
func FlagsFromObject(obj *IntroType) (drafts, trash, folders *bool) {
	if obj == nil {
		return nil, nil, nil
	}
	names := map[string]bool{}
	for _, f := range obj.Fields {
		names[f.Name] = true
	}
	return boolPtr(names["_status"]), boolPtr(names["deletedAt"]), boolPtr(names["folder"])
}

// UploadFromObject reports §7.4's upload signal on the entity's own type.
func UploadFromObject(obj *IntroType) *bool {
	if obj == nil {
		return nil
	}
	return boolPtr(IsUploadType(obj))
}

// TitleField picks the field `pay find` shows as a document's human label. It
// is a display aid with no behavioural consequence, so a heuristic is honest
// here in a way it would not be for a capability.
func TitleField(shard *Shard) *string {
	if shard == nil {
		return nil
	}
	preferred := []string{"title", "name", "label", "slug", "email", "filename"}
	for _, want := range preferred {
		for _, f := range shard.Fields {
			if f.Path == want && (f.PayloadType == TypeText || f.PayloadType == TypeEmail) {
				return strPtr(f.Path)
			}
		}
	}
	for _, f := range shard.Fields {
		if f.Parent == nil && f.PayloadType == TypeText && !f.ReadOnly {
			return strPtr(f.Path)
		}
	}
	return nil
}

// LocaleEnumNames reads LocaleInputType's values. They are
// formatName()-mangled (en-US is exposed as en_US) and introspection never
// exposes the underlying value, so they are used for cross-validation and for
// locale_count only — never as codes (§7.9).
func LocaleEnumNames(schema *Schema) []string {
	t := schema.Type("LocaleInputType")
	if t == nil {
		return nil
	}
	return t.EnumNames()
}
