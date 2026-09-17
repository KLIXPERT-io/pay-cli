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
)

// Exit 9 — config / profile.
const (
	CodeConfigMissing           Code = "config_missing"
	CodeConfigSecretInPlaintext Code = "config_secret_in_plaintext"
	CodeProfileUnknown          Code = "profile_unknown"
	CodeBaseURLInvalid          Code = "base_url_invalid"
	CodeEndpointNotPayload      Code = "endpoint_not_payload"
	CodeAuthCollectionUnknown   Code = "auth_collection_unknown"
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
)

// Exit 11 — confirmation required.
const (
	CodeConfirmationRequired Code = "confirmation_required"
)

func (c Code) String() string { return string(c) }
