package apierr

import "sort"

// Process exit statuses (§11.4). 125–128 and 130 are shell/signal territory
// and are never used.
const (
	ExitOK                   = 0
	ExitInternal             = 1
	ExitAuth                 = 2
	ExitThrottled            = 3
	ExitNotFound             = 4
	ExitValidation           = 5
	ExitNetwork              = 6
	ExitPartial              = 7
	ExitAccessDenied         = 8
	ExitConfig               = 9
	ExitCapability           = 10
	ExitConfirmationRequired = 11
)

// MaxExit is the largest status PayCLI ever returns.
const MaxExit = ExitConfirmationRequired

// exitByCode is the single authority for §11.4's Code -> exit mapping.
// A unit test asserts it is TOTAL over every Code constant declared in
// code.go, because gsc-cli's `default: return 1` branch silently swallowed
// every code added after it was written.
var exitByCode = map[Code]int{
	// 1 — generic / internal
	CodeInternal:                 ExitInternal,
	CodeUnknown:                  ExitInternal,
	CodeCacheCorrupt:             ExitInternal,
	CodeUpdateVerificationFailed: ExitInternal,
	CodeAuditWriteFailed:         ExitInternal,

	// 2 — auth
	CodeAuthMissing:             ExitAuth,
	CodeAuthInvalid:             ExitAuth,
	CodeAuthRequired:            ExitAuth,
	CodeAuthLocked:              ExitAuth,
	CodeAuthUnverifiedEmail:     ExitAuth,
	CodeAuthInsecurePermissions: ExitAuth,
	CodeAuthHelperFailed:        ExitAuth,

	// 3 — throttled
	CodeRateLimited: ExitThrottled,
	CodeDocLocked:   ExitThrottled,
	CodeServerBusy:  ExitThrottled,

	// 4 — not found
	CodeDocNotFound:     ExitNotFound,
	CodeRouteNotFound:   ExitNotFound,
	CodeVersionNotFound: ExitNotFound,
	CodeSelectorNoMatch: ExitNotFound,

	// 5 — validation / bad input
	CodeValidationFailed:    ExitValidation,
	CodeQueryPathInvalid:    ExitValidation,
	CodeInvalidArgs:         ExitValidation,
	CodeInvalidWhereSyntax:  ExitValidation,
	CodeInvalidSortField:    ExitValidation,
	CodeUnknownField:        ExitValidation,
	CodeInvalidOption:       ExitValidation,
	CodeInvalidID:           ExitValidation,
	CodeBadRequestBody:      ExitValidation,
	CodeWhereRequired:       ExitValidation,
	CodeFileMissing:         ExitValidation,
	CodeNotUploadCollection: ExitValidation,
	CodeUnsupportedOperator: ExitValidation,
	CodeBulkLimitExceeded:   ExitValidation,
	CodeRequestTooLarge:     ExitValidation,
	CodeFormatUnsupported:   ExitValidation,
	CodeInvalidPathExpr:     ExitValidation,
	CodeSelectorAmbiguous:   ExitValidation,
	CodeFieldAmbiguous:      ExitValidation,
	CodeNoInput:             ExitValidation,
	CodeNoEdits:             ExitValidation,

	// 6 — network / server
	CodeNetworkUnreachable: ExitNetwork,
	CodeDNSFailure:         ExitNetwork,
	CodeTLSError:           ExitNetwork,
	CodeTimeout:            ExitNetwork,
	CodeServerError:        ExitNetwork,
	CodeServerUnavailable:  ExitNetwork,
	CodeNonJSONResponse:    ExitNetwork,

	// 7 — partial failure
	CodePartialFailure: ExitPartial,

	// 8 — access denied
	CodeAccessDenied:      ExitAccessDenied,
	CodeAdminAccessDenied: ExitAccessDenied,

	// 9 — config / profile
	CodeConfigMissing:           ExitConfig,
	CodeConfigSecretInPlaintext: ExitConfig,
	CodeProfileUnknown:          ExitConfig,
	CodeBaseURLInvalid:          ExitConfig,
	CodeEndpointNotPayload:      ExitConfig,
	CodeAuthCollectionUnknown:   ExitConfig,

	// 10 — capability / discovery
	CodeCollectionUnknown:    ExitCapability,
	CodeGlobalUnknown:        ExitCapability,
	CodeFeatureUnavailable:   ExitCapability,
	CodeOperationUnsupported: ExitCapability,
	CodeDiscoveryFailed:      ExitCapability,
	CodeGraphQLDisabled:      ExitCapability,
	CodeSchemaStale:          ExitCapability,
	CodeBlockTypeUnknown:     ExitCapability,

	// 11 — confirmation required
	CodeConfirmationRequired: ExitConfirmationRequired,
}

// retriable lists the codes PayCLI may auto-retry (§11.4). Exit 3 is retriable
// in full; exit 6 is retriable except server_error (a 500 may already have
// committed a write) and non_json_response (a misrouted endpoint will not fix
// itself). Exit 7 is never retriable, by construction and by doctrine.
var retriable = map[Code]bool{
	CodeRateLimited:        true,
	CodeDocLocked:          true,
	CodeServerBusy:         true,
	CodeNetworkUnreachable: true,
	CodeDNSFailure:         true,
	CodeTimeout:            true,
	CodeServerUnavailable:  true,
}

// Exit returns the process exit status for a code. An unregistered code is a
// programming error rather than a runtime condition; it maps to ExitInternal
// and the totality test is what prevents it from ever happening.
func (c Code) Exit() int {
	if exit, ok := exitByCode[c]; ok {
		return exit
	}
	return ExitInternal
}

// Known reports whether the code is part of the declared vocabulary.
func (c Code) Known() bool {
	_, ok := exitByCode[c]
	return ok
}

// Retriable reports whether PayCLI may transparently retry a request that
// failed with this code. Writes are never retried regardless (§6.1); this
// answers only "is the failure itself transient".
func (c Code) Retriable() bool { return retriable[c] }

// Codes returns every declared code, sorted, for `pay explain --section
// exit_codes` and for tests.
func Codes() []Code {
	out := make([]Code, 0, len(exitByCode))
	for c := range exitByCode {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// CodesByExit groups the vocabulary by exit status, for help and explain output.
func CodesByExit() map[int][]Code {
	out := make(map[int][]Code, MaxExit+1)
	for _, c := range Codes() {
		e := c.Exit()
		out[e] = append(out[e], c)
	}
	return out
}

// ExitCode extracts the process exit status from any error: 0 for nil, the
// mapped status for an *Error anywhere in the chain, and ExitInternal for an
// error that never passed through this package.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	if e, ok := As(err); ok {
		return e.Exit
	}
	return ExitInternal
}
