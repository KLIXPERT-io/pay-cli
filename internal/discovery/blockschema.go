package discovery

import (
	"fmt"
	"sort"
	"strings"
)

// A blocks field's slugs say WHICH blocks may go in it. They say nothing about
// what is INSIDE one, which is the fact an agent needs before it can construct
// a block rather than merely name it.
//
// The shape is readable, per block type, from the block's own GraphQL OBJECT
// type — the union member, not an input type. Verified live:
//
//	__type(name:"CallToActionBlock").fields -> richText: JSON, links: [CallToActionBlock_Links!],
//	                                          id: String, blockName: String, blockType: String
//	__type(name:"MediaBlock").fields        -> media: Media, id, blockName, blockType
//	__type(name:"Textarea").fields          -> name: String!, label, width: Float,
//	                                          defaultValue, required: Boolean, id, blockName, blockType
//
// **Required-ness is the one fact that type cannot fully answer.** Payload
// generates no INPUT_OBJECT for a block (the blocks mutation argument is the
// JSON scalar), so §7.4's `mutation{Singular}Input` NON_NULL trick has nothing
// to read: every INPUT_OBJECT in the live schema was enumerated and there is no
// CallToActionBlock / Textarea input. Two partial sources remain, and they are
// used in this order, each labelled:
//
//  1. NON_NULL on the block object type. Payload emits that only for
//     `required: true` (verified: Textarea.name is `String!`), so a NON_NULL is
//     PROOF of required. Its absence proves nothing, because a draft-enabled
//     collection force-nullables every field beneath it — verified:
//     MediaBlock.media is `required: true` in src/blocks/MediaBlock/config.ts
//     and still comes back nullable, because Pages has drafts.
//  2. The block's own `config.ts` on disk (§7.10's scan), which is where
//     `required: true` is actually written down. Available for the project's
//     own blocks and never for a plugin's — the form-builder's Textarea lives
//     in node_modules, which is deliberately never scanned.
//
// Anything neither source answers is `required: null` with
// `required_source: "unknown"` and a reason. It is never defaulted to false: a
// false here would make `pay create` look complete while Payload rejects it.

// BlockPlumbingNote is the one-line explanation of the three keys Payload puts
// on every block row. They are protocol, not content, and an agent that treats
// them as fields to fill in produces nonsense.
const BlockPlumbingNote = "id, blockName and blockType are Payload plumbing, not content fields: " +
	"blockType is MANDATORY on every block you write and must be the slug (Payload silently drops a " +
	"row whose blockType it does not recognise and still answers 201), blockName is an optional " +
	"admin-UI label, and id is server-generated — omit it when creating."

// SourcePayloadProtocol is the provenance of a fact that comes from Payload's
// block wire format itself rather than from this project: every block row
// carries id/blockName/blockType, and blockType must be sent. It is a distinct
// source because it is true of every Payload 3.x project and was not measured
// on this one.
const SourcePayloadProtocol = "payload-protocol"

// blockPlumbingFields are the keys Payload adds to every block object type.
var blockPlumbingFields = map[string]bool{"id": true, "blockName": true, "blockType": true}

// isBlockPlumbing reports the keys that are Payload's rather than the block
// author's. Nested `id` counts at every depth: an array row inside a block gets
// a server-generated id exactly like the block row itself does, and reporting
// it as content would put it in the list of things an agent has to supply.
func isBlockPlumbing(name string, depth int) bool {
	if depth == 0 {
		return blockPlumbingFields[name]
	}
	return name == "id"
}

// maxBlockNestDepth bounds the walk into a block's own nested groups and
// arrays. Three levels reaches `links.link.url` on the live CallToActionBlock
// (links -> link -> url) with room for one more; past that a self-referential
// block would cost an unbounded number of __type aliases.
const maxBlockNestDepth = 3

// BlockSourceField is one field declaration read out of a block's config.ts.
// Required is nil when the literal was absent or not a boolean.
type BlockSourceField struct {
	Name     string
	Type     string
	Required *bool
	// Description is the field's own `admin: { description: '…' }` — the
	// per-field instruction an agent needs while filling that field in
	// ("Pass a media document id"). "" is "the project's authors wrote none".
	Description string
	// DescriptionKey is the literal config path the text came from,
	// "admin.description" in the normal case.
	DescriptionKey string
}

// BlockSourceDecl is one block's declaration as it exists on disk.
//
// Complete is the load-bearing flag: CallToAction's fields array contains
// `linkGroup({…})`, a helper call the scanner cannot expand, so `links` is
// absent from Fields even though the block has it. Only a Complete declaration
// may answer "false" for a field it does not list; an incomplete one answers
// "unknown", which is the difference between a fact and a guess.
type BlockSourceDecl struct {
	File     string
	Fields   []BlockSourceField
	Complete bool
	// LabelSingular and LabelPlural are the block's `labels:` literals. They
	// are the human name of the block — "Call to Action" for slug cta — and
	// are "" when the project declares none, never a title-cased guess.
	LabelSingular string
	LabelPlural   string
	// Description is what this block is FOR, in the project's own words, and
	// DescriptionKey is the config key it was read from. Payload defines NO
	// standard description for a block, so the key is always reported with the
	// text: see config.DescriptionKeys.
	Description    string
	DescriptionKey string
}

// Field returns the declaration for a field name.
func (d BlockSourceDecl) Field(name string) (BlockSourceField, bool) {
	for _, f := range d.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return BlockSourceField{}, false
}

// BlockFieldSchema is ONE field inside a block type.
//
// It is deliberately not §7.8.2's Field: `queryable`, `operators`, `sortable`
// and `localized` are measured against a collection's `{Singular}_where` and a
// `locale=all` sample, neither of which exists for a block's interior, and
// emitting them here would mean publishing four confident-looking values that
// were never measured.
type BlockFieldSchema struct {
	Name string `json:"name"`
	// Path is dotted WITHIN the block: "links.link.url". It is not a
	// collection field path and cannot be used in --where.
	Path   string  `json:"path"`
	Parent *string `json:"parent"`

	PayloadType           string `json:"payload_type"`
	PayloadTypeConfidence string `json:"payload_type_confidence"`
	// PayloadTypeSource is "graphql" unless the block's config.ts resolved an
	// ambiguity GraphQL cannot: lexical richText and json are the same JSON
	// scalar, and select and radio compile to the same ENUM.
	PayloadTypeSource string `json:"payload_type_source"`

	GraphQLType *string `json:"graphql_type"`
	JSONType    string  `json:"json_type"`
	HasMany     bool    `json:"has_many"`

	// Required is tri-state and is null far more often here than on a
	// collection field: see this file's header for why.
	Required       *bool  `json:"required"`
	RequiredSource string `json:"required_source"`

	Options       []string `json:"options"`
	OptionsSource string   `json:"options_source"`

	RelationTo       []string `json:"relation_to"`
	RelationToSource string   `json:"relation_to_source"`
	Polymorphic      bool     `json:"polymorphic"`

	WriteShape *string `json:"write_shape"`

	// Plumbing marks id/blockName/blockType — Payload's own keys, not this
	// block's content.
	Plumbing bool `json:"plumbing"`

	Label string `json:"label"`

	// Description is the field's own human instruction in the project's own
	// words, read from `admin: { description: '…' }` in the block's config.ts
	// (§7.10). It is a POINTER: null is "nobody wrote one", which is a
	// different fact from an empty string and must never be filled in with a
	// sentence PayCLI made up.
	Description *string `json:"description"`
	// DescriptionSource is "project-source" when Description is non-null and
	// "unknown" when it is null. A plugin's block lives in node_modules, which
	// §7.10 never scans, so every one of its fields reads "unknown" — expected,
	// and stated rather than hidden.
	DescriptionSource string `json:"description_source"`
	// DescriptionKey is the literal config key the text came from
	// ("admin.description"), "" when there is none. Payload has no standard
	// description for a BLOCK, so naming the key is what keeps this output
	// honest about where the words originated.
	DescriptionKey string `json:"description_key"`
}

// BlockFieldSchemaKeys is the exact key set every block field entry carries,
// exported so a consumer can assert the contract instead of re-deriving it.
var BlockFieldSchemaKeys = []string{
	"name", "path", "parent",
	"payload_type", "payload_type_confidence", "payload_type_source",
	"graphql_type", "json_type", "has_many",
	"required", "required_source",
	"options", "options_source",
	"relation_to", "relation_to_source", "polymorphic",
	"write_shape",
	"plumbing",
	"label",
	"description", "description_source", "description_key",
}

// BlockTypeSchema is one block type's full answer: the slug to write, the
// GraphQL union member it came from, and every field inside it.
type BlockTypeSchema struct {
	Slug string `json:"slug"`
	// InterfaceName is the GraphQL union member this schema was read from. It
	// is NOT writable — sending it as a blockType is the §7.10 mistake.
	InterfaceName string `json:"interface_name"`
	// Fields are the block's own fields plus its three plumbing keys, in
	// GraphQL order, with nested group/array interiors flattened into dotted
	// paths.
	Fields []BlockFieldSchema `json:"fields"`
	// FieldsSource is "graphql" when the object type was readable and
	// "unknown" when it was not.
	FieldsSource string `json:"fields_source"`
	// RequiredSource is the summary across the block's content fields: the one
	// source when every field agreed, "mixed" when they did not (including
	// when SOME field is unknown), and "unknown" when nothing answered for any
	// of them. RequiredUnknown and Reason name the gaps precisely.
	RequiredSource string `json:"required_source"`
	// RequiredUnknown names every content field whose required is null, so an
	// agent can see the gap without walking the list.
	RequiredUnknown []string `json:"required_unknown"`
	// ConfigFile is the absolute path required-ness was read from, or "" when
	// no project source declared this slug.
	ConfigFile string `json:"config_file"`
	// PlumbingNote explains id/blockName/blockType in one line.
	PlumbingNote string `json:"plumbing_note"`
	// Reason is empty exactly when every content field's required-ness is
	// known; otherwise it says in plain words what is missing and why.
	Reason string `json:"reason"`

	// Label and LabelPlural are the block's human name, read from its
	// `labels: { singular, plural }` (§7.10). They are POINTERS because a
	// project that declares none has none: null is the honest answer, and
	// title-casing the slug would manufacture a label the admin UI never
	// shows.
	Label       *string `json:"label"`
	LabelPlural *string `json:"label_plural"`
	// LabelsSource is "project-source" when the labels were read off disk and
	// "unknown" when they were not.
	LabelsSource string `json:"labels_source"`

	// Description is the one-sentence statement of what this block is FOR —
	// the fact that lets an agent choose between cta, content and mediaBlock
	// without opening the project. null when the project's authors wrote none.
	Description *string `json:"description"`
	// DescriptionSource is "project-source" or "unknown".
	DescriptionSource string `json:"description_source"`
	// DescriptionKey is the config key the text was read from —
	// "custom.description", "custom.docs" or "custom.summary". It is reported
	// because Payload defines NO description for a block: a Block's `admin`
	// accepts only components/custom/disableBlockName/group/images/jsx and
	// `tsc --noEmit` rejects `admin: { description }` on one, so the text is
	// always project convention rather than a standard field, and saying which
	// key it came from is the difference between reporting and implying.
	DescriptionKey string `json:"description_key"`
	// DocsReason is empty exactly when both the label and the description were
	// found; otherwise it says in plain words which is missing and why.
	DocsReason string `json:"docs_reason"`
}

// BlockDoc is the CHOOSING view of one block type: the human name, what the
// block is for, and how many fields it has — enough to pick between cta,
// content and mediaBlock without running a second command, and small enough to
// print beside every slug list.
//
// It is a projection of BlockTypeSchema, never a second source of truth: every
// value here is copied from the schema that `--block <slug>` prints in full.
type BlockDoc struct {
	Slug        string  `json:"slug"`
	Label       *string `json:"label"`
	LabelPlural *string `json:"label_plural"`
	// LabelsSource / DescriptionSource are "project-source" or "unknown".
	// Nothing here is ever derived from the slug: a block whose author wrote
	// no labels has none, and title-casing "mediaBlock" would invent one.
	LabelsSource      string  `json:"labels_source"`
	Description       *string `json:"description"`
	DescriptionSource string  `json:"description_source"`
	// DescriptionKey names the config key the text came from, because Payload
	// defines none for a block: see BlockTypeSchema.DescriptionKey.
	DescriptionKey string `json:"description_key"`
	// FieldsCount is the block's content fields, excluding the three plumbing
	// keys every block row carries.
	FieldsCount int `json:"fields_count"`
	// DocsReason is empty exactly when both label and description were found,
	// and otherwise says why they were not — a plugin block has no config on
	// disk, and that is expected rather than a failure.
	DocsReason string `json:"docs_reason"`
}

// BlockDocsNote states, once per response, where a block's label and
// description come from — so that a null is read as "this project did not
// write one" and never as "PayCLI failed".
const BlockDocsNote = "label and description are the project's OWN words, read from the block's " +
	"config.ts on disk (§7.10): `labels: { singular, plural }` and a description under " +
	"custom.description / custom.docs / custom.summary. Payload publishes neither over REST or " +
	"GraphQL and defines NO description field for a block at all, so a project gets these only if " +
	"its authors wrote them; null means nobody did, and description_key says which key answered."

// Doc projects a block's schema into the compact form printed beside a slug
// list.
func (s BlockTypeSchema) Doc() BlockDoc {
	return BlockDoc{
		Slug:              s.Slug,
		Label:             s.Label,
		LabelPlural:       s.LabelPlural,
		LabelsSource:      orUnknownSource(s.LabelsSource),
		Description:       s.Description,
		DescriptionSource: orUnknownSource(s.DescriptionSource),
		DescriptionKey:    s.DescriptionKey,
		FieldsCount:       len(s.ContentFields()),
		DocsReason:        s.DocsReason,
	}
}

// orUnknownSource keeps a provenance key from ever being the empty string,
// which is not one of §7.8.3's documented values.
func orUnknownSource(source string) string {
	if source == "" {
		return SourceUnknown
	}
	return source
}

// DisplayName is the one-line human name for a block: its declared singular
// label, or the slug itself when the project declared none. It is for display
// only — the slug is what is written to the API — and it never fabricates a
// label, which is why the fallback is the slug verbatim rather than a
// title-cased guess.
func (s BlockTypeSchema) DisplayName() string {
	if s.Label != nil && *s.Label != "" {
		return *s.Label
	}
	return s.Slug
}

// normalizeDocs fills in the provenance keys a shard written by an OLDER
// PayCLI does not carry.
//
// A cache is not migrated on upgrade, so a shard decoded from one predating
// the label/description keys arrives with "" where §7.8.3 requires a
// documented value. "" is not one of them, and an agent that sees it cannot
// tell "unknown" from "the producer forgot": this maps it to the honest
// unknown before anything prints it. It only ever replaces an empty string, so
// it is idempotent and can never overwrite a measured fact.
func (s *BlockTypeSchema) normalizeDocs() {
	s.LabelsSource = orUnknownSource(s.LabelsSource)
	s.DescriptionSource = orUnknownSource(s.DescriptionSource)
	for i := range s.Fields {
		if s.Fields[i].DescriptionSource != "" {
			continue
		}
		if s.Fields[i].Plumbing {
			s.Fields[i].DescriptionSource = SourceNA
			continue
		}
		s.Fields[i].DescriptionSource = SourceUnknown
	}
}

// ContentFields returns the block's fields minus Payload's plumbing.
func (s BlockTypeSchema) ContentFields() []BlockFieldSchema {
	out := make([]BlockFieldSchema, 0, len(s.Fields))
	for _, f := range s.Fields {
		if !f.Plumbing {
			out = append(out, f)
		}
	}
	return out
}

// RequiredFields returns the names of the fields proved required.
func (s BlockTypeSchema) RequiredFields() []string {
	out := []string{}
	for _, f := range s.Fields {
		if f.Plumbing || f.Required == nil || !*f.Required {
			continue
		}
		out = append(out, f.Path)
	}
	return out
}

// BuildBlockSchema reads ONE block type's field schema from its GraphQL object
// type, overlaying what the project's own source says about required-ness.
//
// iface is the union member name (CallToActionBlock); slug is what the REST
// API accepts (cta). Both are needed: the schema is read under one name and
// written under the other.
func BuildBlockSchema(slug, iface string, schema *Schema, decl BlockSourceDecl, declared bool) BlockTypeSchema {
	out := BlockTypeSchema{
		Slug:              slug,
		InterfaceName:     iface,
		Fields:            []BlockFieldSchema{},
		FieldsSource:      SourceUnknown,
		RequiredSource:    SourceUnknown,
		RequiredUnknown:   []string{},
		PlumbingNote:      BlockPlumbingNote,
		LabelsSource:      SourceUnknown,
		DescriptionSource: SourceUnknown,
	}
	if declared {
		out.ConfigFile = decl.File
	}
	// The human documentation is read off disk and is independent of GraphQL:
	// it must survive an unreadable object type, because "what is this block
	// FOR" is answerable from the config alone.
	out.Label, out.LabelPlural, out.LabelsSource = blockLabels(decl, declared)
	out.Description, out.DescriptionSource, out.DescriptionKey = blockDescription(decl, declared)
	out.DocsReason = blockDocsReason(slug, declared, decl, out.Label != nil, out.Description != nil)

	obj := schema.Type(iface)
	if obj == nil || obj.Kind != KindObject {
		out.Reason = fmt.Sprintf("the GraphQL object type %q for blockType %q could not be read, "+
			"so this block's fields are unknown; `pay discover --refresh` re-reads it", iface, slug)
		return out
	}
	out.FieldsSource = SourceGraphQL

	b := &blockWalk{schema: schema, decl: decl, declared: declared, iface: iface}
	b.walk(obj, "", nil, 0)
	out.Fields = b.fields

	sources := map[string]bool{}
	for _, f := range out.Fields {
		if f.Plumbing {
			continue
		}
		if f.Required == nil {
			out.RequiredUnknown = append(out.RequiredUnknown, f.Path)
			// An unknown counts towards the summary, so a block with one
			// project-source answer and nine holes reports "mixed" rather than
			// "project-source" — the summary may never read stronger than the
			// weakest field under it.
			sources[SourceUnknown] = true
			continue
		}
		sources[f.RequiredSource] = true
	}
	switch {
	case len(sources) == 1:
		for s := range sources {
			out.RequiredSource = s
		}
	case len(sources) > 1:
		out.RequiredSource = SourceMixed
	}
	out.Reason = blockRequiredReason(slug, declared, decl, out.RequiredUnknown)
	return out
}

// blockLabels turns a block's on-disk `labels:` into the tri-state §7.8.3
// requires: a pointer that is null when nothing declared it, together with the
// source that answered. A block that declares only one of the two gets that
// one; the missing half stays null rather than being derived from its sibling.
func blockLabels(decl BlockSourceDecl, declared bool) (singular, plural *string, source string) {
	if !declared || (decl.LabelSingular == "" && decl.LabelPlural == "") {
		return nil, nil, SourceUnknown
	}
	if decl.LabelSingular != "" {
		singular = strPtr(decl.LabelSingular)
	}
	if decl.LabelPlural != "" {
		plural = strPtr(decl.LabelPlural)
	}
	return singular, plural, SourceProjectSource
}

// blockDescription returns the block's human description, its source and the
// config key it was read from. All three move together: a description with no
// key would imply Payload defines one, and Payload does not.
func blockDescription(decl BlockSourceDecl, declared bool) (*string, string, string) {
	if !declared || decl.Description == "" {
		return nil, SourceUnknown, ""
	}
	return strPtr(decl.Description), SourceProjectSource, decl.DescriptionKey
}

// blockDocsReason states why a block has no label or no description — never
// "no reason given", and never silence.
//
// The three cases are genuinely different and an agent acts differently on
// each: a plugin block can never be documented from disk, a project block with
// no `labels:` is a one-line edit away from having one, and a missing
// description is missing because Payload has no field for it.
func blockDocsReason(slug string, declared bool, decl BlockSourceDecl, haveLabel, haveDesc bool) string {
	if haveLabel && haveDesc {
		return ""
	}
	if !declared {
		return fmt.Sprintf("blockType %q has no config on disk to read them from — normal for a "+
			"plugin-provided block, which lives in node_modules — and the API publishes neither", slug)
	}
	missing := []string{}
	if !haveLabel {
		missing = append(missing, "no `labels:`")
	}
	if !haveDesc {
		missing = append(missing, "no "+strings.Join(descriptionKeyNames(), " / "))
	}
	return fmt.Sprintf("%s declares blockType %q with %s", decl.File, slug, strings.Join(missing, " and "))
}

// descriptionKeyNames is config.DescriptionKeys minus the field-only
// `admin.description`, which a Block cannot carry. internal/discovery must not
// import internal/config (arch-lint rule 5's sibling: the pipeline stays
// testable without a filesystem), so the block-level list is stated here and
// pinned to config's by TestBlockDescriptionKeysMatchScanner.
func descriptionKeyNames() []string {
	return []string{"custom.description", "custom.docs", "custom.summary"}
}

// blockRequiredReason states, in plain words, why some of a block's fields
// have no required-ness — never "no reason given".
func blockRequiredReason(slug string, declared bool, decl BlockSourceDecl, unknown []string) string {
	if len(unknown) == 0 {
		return ""
	}
	names := strings.Join(unknown, ", ")
	switch {
	case !declared:
		return fmt.Sprintf("required-ness for %s is unknown: Payload publishes no input type for a "+
			"block, and no project source declares blockType %q (a plugin-provided block lives in "+
			"node_modules, which is never scanned), so only a GraphQL NON_NULL could have proved it",
			names, slug)
	case !decl.Complete:
		return fmt.Sprintf("required-ness for %s is unknown: %s declares blockType %q but its "+
			"`fields:` array contains entries this scanner does not expand (a helper call such as "+
			"linkGroup(), a spread, or a field with no literal `name:`), and PayCLI never evaluates "+
			"project code", names, decl.File, slug)
	default:
		return fmt.Sprintf("required-ness for %s is unknown: they are nested inside a group or array, "+
			"and %s declares `required:` only on the block's top-level fields", names, decl.File)
	}
}

// blockWalk carries the state of one block type's field walk.
type blockWalk struct {
	schema   *Schema
	decl     BlockSourceDecl
	declared bool
	iface    string
	fields   []BlockFieldSchema
}

func (b *blockWalk) walk(obj *IntroType, prefix string, parent *string, depth int) {
	if obj == nil || depth >= maxBlockNestDepth {
		return
	}
	for i := range obj.Fields {
		f := &obj.Fields[i]
		path := f.Name
		if prefix != "" {
			path = prefix + "." + f.Name
		}
		name, _, nonNull, _ := f.Type.Named()
		kind := InferKind(f.Type, b.iface, f.Name, b.schema)

		entry := BlockFieldSchema{
			Name:                  f.Name,
			Path:                  path,
			Parent:                parent,
			PayloadType:           kind.PayloadType,
			PayloadTypeConfidence: kind.Confidence,
			PayloadTypeSource:     SourceGraphQL,
			JSONType:              kind.JSONType,
			HasMany:               kind.HasMany,
			RequiredSource:        SourceUnknown,
			Options:               kind.Options,
			OptionsSource:         kind.OptionsSource,
			RelationTo:            kind.RelationTo,
			RelationToSource:      kind.RelationToSource,
			Polymorphic:           kind.Polymorphic,
			Plumbing:              isBlockPlumbing(f.Name, depth),
			Label:                 HumanizeField(f.Name),
		}
		if name != "" {
			entry.GraphQLType = strPtr(name)
		}

		// The project's own `type:` literal is the only source that can settle
		// the two ambiguities GraphQL compiles away. It is applied ONLY when
		// GraphQL itself was unsure, so a confirmed GraphQL answer is never
		// overwritten by a byte scanner.
		if depth == 0 && kind.Confidence == ConfidenceInferred {
			if src, ok := b.sourceField(f.Name); ok && src.Type != "" {
				// A match is as load-bearing as a correction: it turns an
				// inferred label into a confirmed one, which is the difference
				// between "PayCLI guessed richText from the field name" and
				// "the config says richText".
				if t := payloadTypeFromSource(src.Type); t != "" {
					entry.PayloadType = t
					entry.PayloadTypeConfidence = ConfidenceGraphQL
					entry.PayloadTypeSource = SourceProjectSource
				}
			}
		}

		entry.Required, entry.RequiredSource = b.required(f.Name, nonNull, depth, entry.Plumbing)
		entry.Description, entry.DescriptionSource, entry.DescriptionKey =
			b.description(f.Name, depth, entry.Plumbing)
		entry.WriteShape = WriteShapeFor(entry.PayloadType, entry.Polymorphic, entry.HasMany)
		b.fields = append(b.fields, entry)

		if kind.Children != "" && (kind.PayloadType == TypeGroup || kind.PayloadType == TypeArray) {
			p := path
			b.walk(b.schema.Type(kind.Children), path, &p, depth+1)
		}
	}
}

// sourceField looks a field up in the block's on-disk declaration.
func (b *blockWalk) sourceField(name string) (BlockSourceField, bool) {
	if !b.declared {
		return BlockSourceField{}, false
	}
	return b.decl.Field(name)
}

// description returns the field's own human instruction, read from
// `admin: { description: '…' }` in the block's config.ts.
//
// It is null far more often than it is present, and every null is labelled:
//
//   - a plumbing key (id/blockName/blockType) is Payload's, not the author's,
//     so no project can document it — "n/a", not "unknown";
//   - a nested field (depth > 0) is out of reach, because §7.10's scanner
//     reads only a block's top-level `fields:` array;
//   - a plugin block has no config on disk at all.
//
// GraphQL is not a fallback here: introspection's own `description` is
// Payload's schema-generator output, never the project's admin.description
// (verified: the live CallToActionBlock's fields carry none), so reading it
// would publish a machine string as if it were the author's instruction.
func (b *blockWalk) description(name string, depth int, plumbing bool) (*string, string, string) {
	if plumbing {
		return nil, SourceNA, ""
	}
	if depth > 0 {
		return nil, SourceUnknown, ""
	}
	src, ok := b.sourceField(name)
	if !ok || src.Description == "" {
		return nil, SourceUnknown, ""
	}
	return strPtr(src.Description), SourceProjectSource, src.DescriptionKey
}

// required implements this file's two-source ladder. Every branch returns a
// source, and the unknown branch returns nil rather than false.
func (b *blockWalk) required(name string, nonNull bool, depth int, plumbing bool) (*bool, string) {
	if plumbing {
		// blockType is mandatory on the wire for every Payload block; the
		// other two are not. Neither fact was measured on this project.
		return boolPtr(name == "blockType"), SourcePayloadProtocol
	}
	if nonNull {
		// Payload emits NON_NULL only for required: true. Proof, not a hint.
		return boolPtr(true), SourceGraphQL
	}
	if depth > 0 {
		// The scanner reads a block's top-level `fields:` only, so it cannot
		// speak for an interior field.
		return nil, SourceUnknown
	}
	if src, ok := b.sourceField(name); ok {
		if src.Required != nil {
			return boolPtr(*src.Required), SourceProjectSource
		}
		// Declared with no `required:` key at all. Payload's default is not
		// required, and the declaration was read in full, so this is a fact.
		return boolPtr(false), SourceProjectSource
	}
	return nil, SourceUnknown
}

// payloadTypeFromSource maps a Payload `type:` literal to §7.4's payload_type.
// Only the literals PayCLI already models are mapped; anything else returns ""
// so that GraphQL's answer stands rather than being replaced by a string this
// CLI has no behaviour for.
func payloadTypeFromSource(t string) string {
	switch t {
	case "richText":
		return TypeRichText
	case "json":
		return TypeJSON
	case "select":
		return TypeSelect
	case "radio":
		return TypeRadio
	case "textarea":
		return TypeTextarea
	case "point":
		return TypePoint
	default:
		return ""
	}
}

// BlockSchemasForShard builds one schema per distinct blockType slug reachable
// from any of the shard's blocks fields.
//
// Keyed by SLUG because that is what an agent writes, and because a block type
// used by two fields is the same type both times: verified that pages.layout
// and posts.layout share cta, content and mediaBlock.
func BlockSchemasForShard(shard *Shard, schema *Schema, src BlockSources) map[string]BlockTypeSchema {
	if shard == nil || schema == nil || len(shard.BlockFields) == 0 {
		return nil
	}
	ifaceBySlug := map[string]string{}
	for _, bf := range shard.BlockFields {
		for slug, iface := range bf.SlugInterfaces {
			if _, seen := ifaceBySlug[slug]; !seen && iface != "" {
				ifaceBySlug[slug] = iface
			}
		}
	}
	if len(ifaceBySlug) == 0 {
		return nil
	}
	out := make(map[string]BlockTypeSchema, len(ifaceBySlug))
	for _, slug := range sortedSlugKeys(ifaceBySlug) {
		decl, declared := src.SourceDecls[slug]
		out[slug] = BuildBlockSchema(slug, ifaceBySlug[slug], schema, decl, declared)
	}
	return out
}

// sortedSlugKeys keeps the build order deterministic, which keeps the shard
// hash stable across runs.
func sortedSlugKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
