// Package discovery builds PayCLI's adaptive capability manifest (§7).
//
// The package exists because one binary has to drive *any* Payload 3.x
// project: slugs, GraphQL type names, id types, upload/versions/drafts/trash
// support and the queryable field set are all project-specific, and several of
// them are not obtainable at all from some deployments. Three rules therefore
// govern every line of it:
//
//   - **Shape, never status.** Every REST probe validates the shape of the
//     response body. A custom collection endpoint can shadow any built-in
//     route, an unauthenticated /api/access answers 200 with a truncated view,
//     and a wrong base URL answers 200 text/html.
//
//   - **Tri-state, never defaulted** (§7.6). Every fact only GraphQL can
//     establish is value | unknown, with a *_source sibling. An unknown value
//     must never cause a local rejection: a defaulted id_type would make
//     PayCLI reject valid Mongo ObjectIds with an error message that is a lie.
//
//   - **Read-only by default** (§7.7). Nothing here creates a document unless
//     the caller passed AllowWriteProbes, and §8.4's Level-3 reactive
//     invalidation is never armed from this package — discovery deliberately
//     generates every Level-3 trigger, so an ambient rule would recurse.
//
// The package never imports the CLI layer or cobra, never reads a clock of its
// own (Options.Now is injected), and never writes to disk: it returns a typed
// Manifest plus field Shards and lets internal/cache persist them.
package discovery

// Reachability records which of the two inventory sources saw an entity
// (§7.3). Neither source alone is complete: /api/access omits payload-kv (zero
// permission for this identity) and GraphQL omits payload-migrations
// (endpoints: false), both verified live.
const (
	// ReachabilityOK means both /api/access and the GraphQL Access type
	// listed the entity — or, when GraphQL is unavailable, that /api/access
	// listed it and nothing contradicted that.
	ReachabilityOK = "ok"
	// ReachabilityGraphQLDisabled means /api/access saw it and GraphQL did
	// not.
	ReachabilityGraphQLDisabled = "graphql-disabled"
	// ReachabilityAccessDenied means GraphQL saw it and /api/access did not,
	// i.e. this identity has zero permissions on it.
	ReachabilityAccessDenied = "access-denied"
)

// Reasons recorded in manifest.unreachable[].
const (
	ReasonAccessDenied      = "access-denied"
	ReasonEndpointsDisabled = "endpoints-disabled"
	ReasonGraphQLDisabled   = "graphql-disabled"
)

// GraphQL degradation modes (§7.6).
const (
	GraphQLModeOK                    = "ok"
	GraphQLModeIntrospectionDisabled = "introspection_disabled"
	GraphQLModeErrors                = "errors"
	GraphQLModeDisabled              = "disabled"
	GraphQLModeRouteMissing          = "route_missing"
	GraphQLModeUnreachable           = "unreachable"
)

// Provenance values. Every derived fact is written together with the story of
// where it came from, or neither is written (§7.8.3).
const (
	SourceConfigured   = "configured"
	SourceProbed       = "probed"
	SourceGraphQL      = "graphql"
	SourceGraphQLInput = "graphql-input"
	SourceGraphQLEnum  = "graphql-enum"
	SourceGraphQLArg   = "graphql-arg"
	// SourceGraphQLArgAbsent is §7.8.1's localization.locales_source for "the
	// plural Query field has no locale argument", i.e. the project is provably
	// not localised.
	SourceGraphQLArgAbsent    = "graphql-arg-absent"
	SourceGraphQLEnumMangled  = "graphql-enum-mangled"
	SourceProbe               = "probe"
	SourceAccess              = "access"
	SourceObserved            = "observed"
	SourceLocaleAll           = "locale-all"
	SourceLocaleAllUnverified = "locale-all-unverified"
	SourceProjectSource       = "project-source"
	SourceProjectPackageJSON  = "project-package-json"
	SourceBulkDeleteMessage   = "bulk-delete-message"
	SourceDerived             = "derived"
	SourceAccessZip           = "access-zip"
	SourceTypeProbe           = "type-probe"
	SourceFuzzy               = "fuzzy"
	SourceBootstrap           = "bootstrap"
	SourceCached              = "cached"
	SourceInferred            = "inferred"
	SourceWriteProbe          = "write-probe"
	SourceUnknown             = "unknown"
	// SourceNA is the documented sentinel for "this key does not apply to
	// this field", as opposed to "PayCLI could not find out".
	SourceNA = "n/a"
	// SourceDerivedGraphQLPath mirrors config.SourceDerivedGraphQL so the two
	// layers report the same string for the same fact.
	SourceDerivedGraphQLPath = "derived:api_path+graphql_route"
)

// Confidence values for payload_type and sortable.
const (
	ConfidenceGraphQL   = "graphql"
	ConfidenceInferred  = "inferred"
	ConfidenceObserved  = "observed"
	ConfidenceHeuristic = "heuristic"
	ConfidenceMeasured  = "measured"
	ConfidenceUnknown   = "unknown"
)

// IDType values (§7.6). Unknown is a first-class answer, not an error state.
const (
	IDTypeNumber  = "number"
	IDTypeString  = "string"
	IDTypeUnknown = "unknown"
)

// Payload field kinds inferred by §7.4's table.
const (
	TypeText         = "text"
	TypeTextarea     = "textarea"
	TypeNumber       = "number"
	TypeCheckbox     = "checkbox"
	TypeDate         = "date"
	TypeEmail        = "email"
	TypeSelect       = "select"
	TypeRadio        = "radio"
	TypeRelationship = "relationship"
	TypeUpload       = "upload"
	TypeJoin         = "join"
	TypeBlocks       = "blocks"
	TypeRichText     = "richText"
	TypeJSON         = "json"
	TypeGroup        = "group"
	TypeArray        = "array"
	TypePoint        = "point"
	TypeID           = "id"
	TypeUnknown      = "unknown"
)

// WriteShape is §7.8.2's closed enum. It is mandatory and non-null on every
// relationship, upload and polymorphic field and null on every other kind,
// because §9.10's --set coercion reads exactly this key.
const (
	WriteShapeID       = "id"
	WriteShapeIDArray  = "id_array"
	WriteShapeRelValue = "{relationTo,value}"
	WriteShapeRelList  = "[{relationTo,value}]"
	WriteShapeJSON     = "json"
)

// HookMutated is tri-state as a string because "unknown" is the normal answer:
// beforeChange / beforeValidate hooks are not introspectable (§7.6,
// HOOK_MUTATION_UNKNOWN).
const (
	HookMutatedUnknown = "unknown"
	HookMutatedTrue    = "true"
	HookMutatedFalse   = "false"
)

// Limitation codes (§7.6). They are stable identifiers: an agent branches on
// the code, never on the prose.
const (
	LimFieldsUnavailable          = "FIELDS_UNAVAILABLE"
	LimSelectOptionsUnavailable   = "SELECT_OPTIONS_UNAVAILABLE"
	LimBlockSlugsUnknown          = "BLOCK_SLUGS_UNKNOWN"
	LimCustomEndpointsNotEnum     = "CUSTOM_ENDPOINTS_NOT_ENUMERABLE"
	LimSortabilityHeuristic       = "SORTABILITY_HEURISTIC"
	LimLocalizationUnknown        = "LOCALIZATION_UNKNOWN"
	LimRichTextShapeUnknown       = "RICHTEXT_SHAPE_UNKNOWN"
	LimIDTypeUnknown              = "ID_TYPE_UNKNOWN"
	LimCapabilityUnknown          = "CAPABILITY_UNKNOWN"
	LimLabelsUnavailable          = "LABELS_UNAVAILABLE"
	LimLocalesUnknown             = "LOCALES_UNKNOWN"
	LimLocalizationPerFieldUnknwn = "LOCALIZATION_PER_FIELD_UNKNOWN"
	LimBlockSlugsUnknownNoSource  = "BLOCK_SLUGS_UNKNOWN_NO_SOURCE"
	LimAuthCandidatesTruncated    = "AUTH_CANDIDATES_TRUNCATED"
	LimHookMutationUnknown        = "HOOK_MUTATION_UNKNOWN"
	LimPayloadVersionUnknown      = "PAYLOAD_VERSION_UNKNOWN"
	LimDBAdapterUnknown           = "DB_ADAPTER_UNKNOWN"
)

// Stage names recorded in diagnostics.degraded[].
const (
	StageModeProbe   = "mode_probe"
	Stage1           = "stage1"
	Stage2           = "stage2"
	Stage3           = "stage3"
	StageFingerprint = "fingerprint"
)

// limitationMitigations is §7.6's table, keyed by code. Every mitigation
// string is written into the manifest next to the code, so an agent that hits
// a hole is told what to do about it without a second lookup.
//
// §7.10's hard rule applies: no hint here may name a `pay` command that cannot
// answer the situation that emitted it.
var limitationMitigations = map[string]string{
	LimFieldsUnavailable:          "the collection has no documents to sample and no GraphQL schema; try `pay find <slug> --limit 1 --trash` or re-run with `pay discover --deep --allow-write-probes`",
	LimSelectOptionsUnavailable:   "options were sampled from existing documents rather than enumerated; treat the list as incomplete",
	LimBlockSlugsUnknown:          "GraphQL exposes interfaceNames, not blockType slugs; resolve them from project source or pin them with `pay config set profiles.<p>.blocks.<field> a,b,c`",
	LimCustomEndpointsNotEnum:     "config.endpoints is not introspectable; list them in the profile's custom_endpoints or call them with `pay raw`",
	LimSortabilityHeuristic:       "sortability was inferred from the field kind, not measured",
	LimLocalizationUnknown:        "whether this project is localised is not detectable over REST; no locale parameters are sent until it is known",
	LimRichTextShapeUnknown:       "lexical richText maps to the JSON scalar; run `pay describe <slug> --field <field> --sample` to see a real value",
	LimIDTypeUnknown:              "pin it once with `pay config set profiles.<p>.id_type string`; until then ids are passed through unchecked",
	LimCapabilityUnknown:          "the operation is attempted and the server's answer is classified after the fact; run `pay discover --refresh` once the feature is known",
	LimLabelsUnavailable:          "the bulk-delete message did not match the English pattern, so labels were derived from the slug; nothing else depends on them",
	LimLocalesUnknown:             "pin them once: `pay config set profiles.<p>.locales en,de`",
	LimLocalizationPerFieldUnknwn: "per-field `localized` is not introspectable; the echo-diff skips every field whose localized is null",
	LimBlockSlugsUnknownNoSource:  "read them from payload.config.ts / src/blocks/*/config.ts (look for `slug:`), then pin them with `pay config set profiles.<p>.blocks.<field> a,b,c`",
	LimAuthCandidatesTruncated:    "only the anonymous slug list was visible; pass --auth-collection <slug>",
	LimHookMutationUnknown:        "beforeChange / beforeValidate hooks are not introspectable; list noisy fields in the profile's echo_check_ignore",
	LimPayloadVersionUnknown:      "no endpoint or header carries it and no package.json was found; version-gated hints use their unconditional wording",
	LimDBAdapterUnknown:           "not discoverable from the API; unsupported operators stay reactive and are never blocked pre-emptively",
}

// Mitigation returns §7.6's recorded mitigation for a limitation code.
func Mitigation(code string) string { return limitationMitigations[code] }

// boolPtr is used everywhere a tri-state flag is *known*.
func boolPtr(v bool) *bool { return &v }

// strPtr is used for nullable manifest strings.
func strPtr(v string) *string { return &v }

// intPtr is used for nullable manifest counts.
func intPtr(v int) *int { return &v }
