// Package apierr is PayCLI's error vocabulary (§11): a closed set of stable
// machine-readable codes, a total code-to-exit-status map, and the normaliser
// that folds Payload's six different error body shapes into one.
//
// Nothing in this package reads a human-readable server message to decide a
// code. Payload runs followingFieldsInvalid, noFilesUploaded,
// notAllowedToPerformAction and deletedCountSuccessfully through req.t, so
// their text changes with the project's i18n config, a payload-lng cookie or a
// proxy-injected Accept-Language. Only errors[].name, the JSON structure and
// the HTTP status are trusted. The two strings Payload hardcodes in English —
// `Route not found "…"` and `Cannot <METHOD> …` — are the sole exceptions and
// are named explicitly at each use.
package apierr

// Code is a stable, machine-readable error identifier. Agents branch on it;
// it never changes for a given failure mode.
type Code string

// Exit 1 — generic / internal.
const (
	CodeInternal                 Code = "internal"
	CodeUnknown                  Code = "unknown"
	CodeCacheCorrupt             Code = "cache_corrupt"
	CodeUpdateVerificationFailed Code = "update_verification_failed"
	CodeAuditWriteFailed         Code = "audit_write_failed"

	// CodeBackupFailed is §12.8's pre-write backup failing. Like the pre-write
	// audit record it ABORTS the write: nothing was sent.
	CodeBackupFailed Code = "backup_failed"
)

// Exit 2 — auth: *your credentials* are the problem.
const (
	CodeAuthMissing             Code = "auth_missing"
	CodeAuthInvalid             Code = "auth_invalid"
	CodeAuthRequired            Code = "auth_required"
	CodeAuthLocked              Code = "auth_locked"
	CodeAuthUnverifiedEmail     Code = "auth_unverified_email"
	CodeAuthInsecurePermissions Code = "auth_insecure_permissions"
	CodeAuthHelperFailed        Code = "auth_helper_failed"
)

// Exit 3 — throttled / temporarily unavailable. Auto-retried.
const (
	CodeRateLimited Code = "rate_limited"
	CodeDocLocked   Code = "doc_locked"
	CodeServerBusy  Code = "server_busy"
)

// Exit 4 — not found.
const (
	CodeDocNotFound     Code = "doc_not_found"
	CodeRouteNotFound   Code = "route_not_found"
	CodeVersionNotFound Code = "version_not_found"
	// CodeSelectorNoMatch is a `pay blocks` selector that addressed no row. It
	// is a not-found rather than a validation failure because the selector is
	// well formed and the document simply does not contain that row — the same
	// distinction doc_not_found draws for a document.
	CodeSelectorNoMatch Code = "selector_no_match"
	// CodeNotPublished is `pay get --published-only` on a document that
	// exists but has no published state (never published, or unpublished).
	// It is a not-found: the caller asked for the published document and
	// there is none.
	CodeNotPublished Code = "not_published"
	// CodeRefUnresolved is a {"$ref": …} in a write body (create, update,
	// upsert, a sync manifest) that matched no document, or a plan-local
	// {"$ref":"@key"} naming no item of the plan.
	CodeRefUnresolved Code = "ref_unresolved"
)

// Exit 5 — validation / bad input.
const (
	CodeValidationFailed    Code = "validation_failed"
	CodeQueryPathInvalid    Code = "query_path_invalid"
	CodeInvalidArgs         Code = "invalid_args"
	CodeInvalidWhereSyntax  Code = "invalid_where_syntax"
	CodeInvalidSortField    Code = "invalid_sort_field"
	CodeUnknownField        Code = "unknown_field"
	CodeInvalidOption       Code = "invalid_option"
	CodeInvalidID           Code = "invalid_id"
	CodeBadRequestBody      Code = "bad_request_body"
	CodeWhereRequired       Code = "where_required"
	CodeFileMissing         Code = "file_missing"
	CodeNotUploadCollection Code = "not_upload_collection"
	CodeUnsupportedOperator Code = "unsupported_operator"
	CodeBulkLimitExceeded   Code = "bulk_limit_exceeded"
	CodeRequestTooLarge     Code = "request_too_large"
	CodeFormatUnsupported   Code = "format_unsupported"
	CodeInvalidPathExpr     Code = "invalid_path_expr"
	// CodeSelectorAmbiguous is a `pay blocks` selector that addressed several
	// rows where the operation acts on exactly one. It is deliberately not
	// invalid_args: the selector is well formed and the caller needs to know
	// WHICH rows it hit, not that they typed it wrongly.
	CodeSelectorAmbiguous Code = "selector_ambiguous"
	// CodeFieldAmbiguous is a collection with more than one blocks field and no
	// --field to say which one.
	CodeFieldAmbiguous Code = "field_ambiguous"
	// CodeNoInput is a pipeline stage with nothing on stdin.
	CodeNoInput Code = "no_input"
	// CodeNoEdits is `pay apply` on an envelope no transform ever touched, so
	// there is no field list to narrow the write to.
	CodeNoEdits Code = "no_edits"
	// CodeNotRichText is a `pay lexical` input that holds no Lexical editor
	// state where one was expected: a bare value that is not {"root":…}, a
	// document without a richText field, or a --field that names something
	// else.
	CodeNotRichText Code = "not_rich_text"
	// CodeRichTextRendered is a write (pay apply, --data/--data-file, a
	// `pay blocks` edit stage) handed a document whose rich text was rendered
	// to Markdown or plain text by `--richtext md|text`. Payload does not
	// reject a string in a richText field — it stores it, and the editor can
	// no longer open the field — so PayCLI refuses before anything is sent.
	CodeRichTextRendered Code = "richtext_rendered"
	// CodeBackupTargetMismatch is `pay backups restore` pointed at a backup
	// taken from a different profile or server than the one it would write
	// to — restoring a stage document into production by accident.
	CodeBackupTargetMismatch Code = "backup_target_mismatch"
	// CodeSlugAmbiguous is a --slug that matched more than one document where
	// the command acts on exactly one. error.did_you_mean lists the ids.
	CodeSlugAmbiguous Code = "slug_ambiguous"
	// CodeMatchAmbiguous is `pay upsert --match` (or a sync manifest item's
	// "match") finding more than one document: the command would not know
	// which one to update. error.did_you_mean lists the ids.
	CodeMatchAmbiguous Code = "match_ambiguous"
	// CodeRefAmbiguous is a {"$ref": …} matching more than one document.
	// error.did_you_mean lists the ids, exactly like slug_ambiguous.
	CodeRefAmbiguous Code = "ref_ambiguous"
	// CodeRefCycle is a plan whose items reference each other in a cycle
	// that contains a document still to be created: no order can give every
	// item the id it needs.
	CodeRefCycle Code = "ref_cycle"
	// CodeManifestInvalid is a `pay plan`/`pay sync` manifest that cannot be
	// planned: unreadable JSON, an unknown key, a missing collection, two
	// items writing the same document. error.failures names every item.
	CodeManifestInvalid Code = "manifest_invalid"
)

// Exit 6 — network / server.
const (
	CodeNetworkUnreachable Code = "network_unreachable"
	CodeDNSFailure         Code = "dns_failure"
	CodeTLSError           Code = "tls_error"
	CodeTimeout            Code = "timeout"
	CodeServerError        Code = "server_error"
	CodeServerUnavailable  Code = "server_unavailable"
	CodeNonJSONResponse    Code = "non_json_response"
)

// Exit 7 — partial failure. NEVER auto-retried: some documents committed.
const (
	CodePartialFailure Code = "partial_failure"
)

// Exit 8 — access denied: identity valid, permission not.
const (
	CodeAccessDenied      Code = "access_denied"
	CodeAdminAccessDenied Code = "admin_access_denied"
	// CodePreviewDenied is the site's preview route refusing the handshake
	// (HTTP 403 from the website template's /next/preview): a wrong preview
	// secret, or a credential payload.auth() did not accept.
	CodePreviewDenied Code = "preview_denied"
)

// Exit 9 — config / profile.
const (
	CodeConfigMissing           Code = "config_missing"
	CodeConfigSecretInPlaintext Code = "config_secret_in_plaintext"
	CodeProfileUnknown          Code = "profile_unknown"
	CodeBaseURLInvalid          Code = "base_url_invalid"
	CodeEndpointNotPayload      Code = "endpoint_not_payload"
	CodeAuthCollectionUnknown   Code = "auth_collection_unknown"
	// CodePreviewSecretMissing is `pay url --preview` with a preview_path that
	// needs {secret} and no preview secret configured for the profile.
	CodePreviewSecretMissing Code = "preview_secret_missing"
)

// Exit 10 — capability / discovery.
const (
	CodeCollectionUnknown    Code = "collection_unknown"
	CodeGlobalUnknown        Code = "global_unknown"
	CodeFeatureUnavailable   Code = "feature_unavailable"
	CodeOperationUnsupported Code = "operation_unsupported"
	CodeDiscoveryFailed      Code = "discovery_failed"
	CodeGraphQLDisabled      Code = "graphql_disabled"
	CodeSchemaStale          Code = "schema_stale"
	// CodeBlockTypeUnknown is a blockType this project's field does not accept.
	// It is exit 10 rather than 5 because the answer is a property of the
	// project, not of the command line: Payload silently drops a row whose
	// blockType it does not recognise and still answers 201.
	CodeBlockTypeUnknown Code = "block_type_unknown"
	// CodePreviewFailed is a preview handshake the site answered without
	// issuing a cookie (no preview route at preview_path, a 5xx, a 2xx with
	// no Set-Cookie). The site does not do draft preview the way the profile
	// describes it.
	CodePreviewFailed Code = "preview_failed"
)

// Exit 11 — confirmation required.
const (
	CodeConfirmationRequired Code = "confirmation_required"
)

func (c Code) String() string { return string(c) }
